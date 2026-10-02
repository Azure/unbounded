//! Exclusive connections and bounded pools. No executor or application policy.
use crate::{Codec, Error, MessageHead, Opaque, StartLine};
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    future::poll_fn,
    ops::{Deref, Range},
    rc::{Rc, Weak},
    task::{Poll, Waker},
    time::{Duration, Instant},
};
use uring_runtime::{
    Budget, Scope,
    reactor::{Completion, Descriptor, IoBuffer, Reactor, SendBuffer, SocketAddress},
};
use zeroize::{Zeroize, Zeroizing};
#[cfg(test)]
mod tests;

pub type Result<C, T> = std::result::Result<T, <C as Context>::Error>;
pub type Operation<'a, C, T> = uring_runtime::Operation<'a, T, <C as Context>::Error>;

pub trait Context: 'static {
    type Error: Copy + Send + PartialEq + From<Error> + From<uring_runtime::Error> + 'static;
    type Scope: Scope<Error = Self::Error>;
    type Budget: Budget;
    type Reactor: Deref<Target = Reactor<Self::Scope, Self::Budget>>;
    type Charge: 'static;
    type Slot: 'static;
    type Opaque: Opaque;
    type State: State<Self::Error>;
    type Endpoint: Endpoint<Self::Error>;
    /// Ordinary admission, not the runtime's completion/drain reserve.
    fn charge(&self, bytes: usize) -> Result<Self, Self::Charge>
    where
        Self: Sized;
    fn outbound_slot(&self) -> Result<Self, Self::Slot>
    where
        Self: Sized;
    fn stopped(&self) -> bool;
}

/// Per-connection policy travels through every completion and cancellation fence.
pub trait State<E>: Default + 'static {
    /// May transform application fields, but must preserve wire content length,
    /// message kind, response status, HEAD semantics, and connection-close policy.
    /// I/O rejects any such framing change after the hook returns.
    fn admit(&mut self, head: MessageHead) -> std::result::Result<MessageHead, E> {
        Ok(head)
    }
    /// The returned head is validated again; its framing controls body sending.
    fn sign(&mut self, head: MessageHead) -> std::result::Result<MessageHead, E> {
        Ok(head)
    }
    fn finished(&mut self) {}
    /// Invoked on the retained observation state after a failed connect completes.
    fn connect_failed(&self, _errno: Option<i32>) {}
    /// Install checkout-local policy before connect submission. The default replaces
    /// all state; session-preserving policies must override this method.
    fn attach(&mut self, checkout: Self) {
        *self = checkout;
    }
    /// Clear transient attachments, retaining any connection session.
    fn idle(self) -> Self {
        self
    }
}
impl<E> State<E> for () {}

pub trait Endpoint<E>: Clone + Ord + 'static {
    fn address(&self) -> std::result::Result<SocketAddress, E>;
    fn capacity(&self, config: &PoolConfig) -> usize {
        config.per_endpoint
    }
    /// Only endpoints explicitly opting in reserve priority headroom.
    fn priority_headroom(&self) -> usize {
        0
    }
    fn allocation(&self) -> usize {
        std::mem::size_of_val(self)
    }
}

#[derive(Clone)]
pub struct PoolConfig {
    pub per_endpoint: usize,
    pub secondary_cap: usize,
    pub max_endpoints: usize,
    pub waiter_cap: usize,
    pub idle_timeout: Duration,
    pub tcp_nodelay: bool,
}
impl Default for PoolConfig {
    fn default() -> Self {
        Self {
            per_endpoint: 1,
            secondary_cap: 1,
            max_endpoints: 256,
            waiter_cap: 256,
            idle_timeout: Duration::from_secs(30),
            tcp_nodelay: false,
        }
    }
}

pub struct OwnedBuffer<C: Context> {
    bytes: Vec<u8>,
    reservation: Option<C::Charge>,
    pool: Weak<RefCell<Option<Self>>>,
}
impl<C: Context> Drop for OwnedBuffer<C> {
    fn drop(&mut self) {
        self.bytes.as_mut_slice().zeroize();
        if let Some(pool) = self.pool.upgrade() {
            if let Ok(mut idle) = pool.try_borrow_mut() {
                if idle.is_none() {
                    *idle = Some(Self {
                        bytes: std::mem::take(&mut self.bytes),
                        reservation: self.reservation.take(),
                        pool: Weak::new(),
                    });
                }
            }
        }
    }
}
impl<C: Context> OwnedBuffer<C> {
    pub fn new(context: &C, length: usize) -> Result<C, Self> {
        let reservation = context.charge(length.max(1))?;
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(length)
            .map_err(|_| C::Error::from(uring_runtime::Error::Overloaded))?;
        bytes.resize(length, 0);
        Ok(Self {
            bytes: bytes.into_boxed_slice().into_vec(),
            reservation: Some(reservation),
            pool: Weak::new(),
        })
    }
    pub fn copy_from(context: &C, bytes: &[u8]) -> Result<C, Self> {
        let mut buffer = Self::new(context, bytes.len())?;
        buffer.bytes.copy_from_slice(bytes);
        Ok(buffer)
    }
}
// SAFETY: fixed, private backing allocation and charge remain owned through completion.
unsafe impl<C: Context> IoBuffer for OwnedBuffer<C> {
    type Error = C::Error;
    fn bytes(&self) -> Result<C, &[u8]> {
        Ok(&self.bytes)
    }
    fn bytes_mut(&mut self) -> Result<C, &mut [u8]> {
        Ok(&mut self.bytes)
    }
}
pub struct BufferRange<B: IoBuffer> {
    buffer: B,
    range: Range<usize>,
}
impl<B: IoBuffer> BufferRange<B> {
    pub fn new<E: From<B::Error> + From<Error>>(
        buffer: B,
        range: Range<usize>,
    ) -> std::result::Result<Self, E> {
        if range.start > range.end || range.end > buffer.bytes()?.len() {
            return Err(Error::Malformed.into());
        }
        Ok(Self { buffer, range })
    }
    pub fn into_inner(self) -> B {
        self.buffer
    }
}
// SAFETY: the fixed view retains the complete stable backing owner.
unsafe impl<B: IoBuffer> IoBuffer for BufferRange<B> {
    type Error = B::Error;
    fn bytes(&self) -> std::result::Result<&[u8], B::Error> {
        Ok(&self.buffer.bytes()?[self.range.clone()])
    }
    fn bytes_mut(&mut self) -> std::result::Result<&mut [u8], B::Error> {
        Ok(&mut self.buffer.bytes_mut()?[self.range.clone()])
    }
}
struct SendRange<B: SendBuffer> {
    buffer: B,
    range: Range<usize>,
}
// SAFETY: immutable fixed view retains its complete send owner.
unsafe impl<B: SendBuffer> SendBuffer for SendRange<B> {
    type Error = B::Error;
    fn send_bytes(&self) -> std::result::Result<&[u8], B::Error> {
        Ok(&self.buffer.send_bytes()?[self.range.clone()])
    }
}

pub struct HttpIo<C: Context> {
    reactor: Rc<C::Reactor>,
    codec: Codec<C::Opaque>,
    receive_limit: u64,
    send_limit: u64,
    context: Rc<C>,
    idle_buffer: Rc<RefCell<Option<OwnedBuffer<C>>>>,
}
pub struct HeadCompletion<C: Context, T> {
    pub connection: ConnectionLease<C>,
    pub value: T,
    pub _decoded: Option<C::Charge>,
}
impl<C: Context> HttpIo<C> {
    #[cfg(feature = "test-util")]
    pub fn framing(&self, head: &MessageHead, request_is_head: bool) -> Result<C, u64> {
        framing(head, request_is_head, self.receive_limit).map_err(Into::into)
    }
    pub fn new(
        reactor: Rc<C::Reactor>,
        codec: Codec<C::Opaque>,
        context: Rc<C>,
        receive_limit: u64,
        send_limit: u64,
    ) -> Self {
        Self {
            reactor,
            codec,
            context,
            receive_limit,
            send_limit,
            idle_buffer: Rc::default(),
        }
    }
    pub fn reactor(&self) -> &Rc<C::Reactor> {
        &self.reactor
    }
    pub fn disconnected<'a>(
        &'a self,
        connection: &ConnectionLease<C>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, u32> {
        self.reactor.readiness_with_lease(
            connection.socket(),
            libc::POLLHUP as u32,
            connection.slot().cloned(),
            scope,
        )
    }
    pub fn buffer(&self, length: usize) -> Result<C, OwnedBuffer<C>> {
        if self.context.stopped() {
            return Err(uring_runtime::Error::Unavailable.into());
        }
        let idle = self.idle_buffer.borrow_mut().take();
        let mut buffer = match idle {
            Some(buffer) if buffer.bytes.len() == length => buffer,
            other => {
                drop(other);
                OwnedBuffer::new(self.context.as_ref(), length)?
            }
        };
        if length <= 65536 {
            buffer.pool = Rc::downgrade(&self.idle_buffer);
        }
        Ok(buffer)
    }
    pub fn reclaim_buffer(&self) {
        self.idle_buffer.borrow_mut().take();
    }
    #[cfg(feature = "test-util")]
    pub fn retained_buffer_bytes(&self) -> usize {
        self.idle_buffer
            .borrow()
            .as_ref()
            .map_or(0, |b| b.bytes.len().max(1))
    }
    pub fn capped(&self, limit: usize) -> Self {
        Self {
            reactor: self.reactor.clone(),
            codec: self.codec.limited(limit),
            receive_limit: self.receive_limit,
            send_limit: self.send_limit,
            context: self.context.clone(),
            idle_buffer: self.idle_buffer.clone(),
        }
    }
    pub fn receive_head<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, HeadCompletion<C, MessageHead>> {
        self.receive_head_limited(connection, scope, self.codec.header_limit())
    }
    pub fn receive_head_limited<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        scope: &'a C::Scope,
        header_limit: usize,
    ) -> Operation<'a, C, HeadCompletion<C, MessageHead>> {
        Box::pin(async move {
            let completed = self
                .receive_head_outcome(connection, scope, header_limit, false)
                .await?;
            Ok(HeadCompletion {
                connection: completed.connection,
                value: completed.value?,
                _decoded: completed._decoded,
            })
        })
    }
    pub fn receive_request_head_limited<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        scope: &'a C::Scope,
        header_limit: usize,
    ) -> Operation<'a, C, HeadCompletion<C, Result<C, MessageHead>>> {
        self.receive_head_outcome(connection, scope, header_limit, true)
    }
    fn receive_head_outcome<'a>(
        &'a self,
        mut connection: ConnectionLease<C>,
        scope: &'a C::Scope,
        header_limit: usize,
        request_only: bool,
    ) -> Operation<'a, C, HeadCompletion<C, Result<C, MessageHead>>> {
        Box::pin(async move {
            scope.check()?;
            if connection.rx_remaining.is_some_and(|n| n != 0) {
                return Err(Error::Malformed.into());
            }
            connection.begin_io();
            let codec = self.codec.limited(header_limit);
            let ahead_length = connection.read_ahead.as_ref().map_or(0, |(_, r)| r.len());
            let mut buffer = self.buffer(codec.header_limit().min(4096.max(ahead_length)))?;
            let mut used = 0;
            let mut scanned: usize = 0;
            if let Some((ahead, range)) = connection.read_ahead.take() {
                if range.len() > buffer.bytes.len() {
                    return Ok(rejected_head(connection, Error::HeadTooLarge));
                }
                used = range.len();
                buffer.bytes[..used].copy_from_slice(&ahead.bytes[range]);
            }
            loop {
                let complete = buffer.bytes[scanned.saturating_sub(3)..used]
                    .windows(4)
                    .any(|w| w == b"\r\n\r\n");
                let malformed = !complete
                    && (scanned.saturating_sub(1)..used).any(|i| {
                        (buffer.bytes[i] == b'\n' && (i == 0 || buffer.bytes[i - 1] != b'\r'))
                            || (buffer.bytes[i] == b'\r'
                                && i + 1 < used
                                && buffer.bytes[i + 1] != b'\n')
                    });
                scanned = used;
                let decoded_charge = if complete {
                    Some(
                        self.context
                            .charge(codec.decoded_allocation(&buffer.bytes[..used])?)?,
                    )
                } else {
                    None
                };
                let decoded = match if complete || malformed || used == codec.header_limit() {
                    codec.decode_head(&buffer.bytes[..used])
                } else {
                    Ok(None)
                } {
                    Ok(decoded) => decoded,
                    Err(error) => {
                        drop(buffer);
                        return Ok(rejected_head(connection, error));
                    }
                };
                if let Some((head, end)) = decoded {
                    if request_only && !matches!(head.start, StartLine::Request { .. }) {
                        return Ok(rejected_head(connection, Error::Malformed));
                    }
                    let length =
                        match framing(&head, connection.request_is_head, self.receive_limit) {
                            Ok(length) => length,
                            Err(error) => {
                                drop(buffer);
                                return Ok(rejected_head(connection, error));
                            }
                        };
                    let wire = WireFraming::new(&head)?;
                    connection.close |= wire.close;
                    if let StartLine::Request { method, .. } = &head.start {
                        connection.request_is_head = method == "HEAD";
                    }
                    connection.rx_remaining = Some(length);
                    buffer.bytes[..end].zeroize();
                    if used > end {
                        connection.read_ahead = Some((buffer, end..used));
                    }
                    let head = connection.state_mut().admit(head)?;
                    // Admission may remove authentication fields, but it must not
                    // change the framing already received from the socket.
                    if WireFraming::new(&head)? != wire {
                        return Err(Error::Malformed.into());
                    }
                    return Ok(HeadCompletion {
                        connection,
                        value: Ok(head),
                        _decoded: decoded_charge,
                    });
                }
                if used == buffer.bytes.len() {
                    let size = used.saturating_mul(2).min(codec.header_limit());
                    if size <= used {
                        return Ok(rejected_head(connection, Error::HeadTooLarge));
                    }
                    let mut larger = self.buffer(size)?;
                    larger.bytes[..used].copy_from_slice(&buffer.bytes[..used]);
                    buffer = larger;
                }
                let end = buffer.bytes.len();
                let completed = self
                    .reactor
                    .recv(
                        connection.socket(),
                        BufferRange::new::<C::Error>(buffer, used..end)?,
                        connection,
                        scope,
                    )
                    .await?;
                if completed.bytes == 0 || completed.bytes > end - used {
                    return Err(uring_runtime::Error::Io.into());
                }
                used = used
                    .checked_add(completed.bytes)
                    .ok_or(uring_runtime::Error::Io)?;
                buffer = completed.buffer.into_inner();
                connection = completed.lease;
            }
        })
    }
    pub fn send_head<'a>(
        &'a self,
        mut connection: ConnectionLease<C>,
        head: MessageHead,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, HeadCompletion<C, ()>> {
        Box::pin(async move {
            scope.check()?;
            if connection.tx_remaining.is_some_and(|n| n != 0) {
                return Err(Error::Malformed.into());
            }
            connection.begin_io();
            // Preserve pre-hook validation precedence, then validate the actual
            // signed head and use its framing for the bytes that will be sent.
            framing(&head, connection.request_is_head, self.send_limit)?;
            let scratch = self.context.charge(self.codec.header_limit().max(1))?;
            let head = connection.state_mut().sign(head)?;
            let length = framing(&head, connection.request_is_head, self.send_limit)?;
            connection.close |= head.closes_connection()?;
            if let StartLine::Request { method, .. } = &head.start {
                connection.request_is_head = method == "HEAD";
            }
            let encoded = Zeroizing::new(self.codec.encode_head(&head)?);
            let mut buffer = self.buffer(encoded.len())?;
            buffer.bytes.copy_from_slice(&encoded);
            let length_encoded = encoded.len();
            drop(encoded);
            drop(scratch);
            let mut offset = 0;
            while offset < length_encoded {
                let completed = self
                    .reactor
                    .send(
                        connection.socket(),
                        BufferRange::new::<C::Error>(buffer, offset..length_encoded)?,
                        connection,
                        scope,
                    )
                    .await?;
                if completed.bytes == 0 || completed.bytes > length_encoded - offset {
                    return Err(uring_runtime::Error::Io.into());
                }
                offset += completed.bytes;
                buffer = completed.buffer.into_inner();
                connection = completed.lease;
            }
            drop(head);
            connection.tx_remaining = Some(length);
            Ok(HeadCompletion {
                connection,
                value: (),
                _decoded: None,
            })
        })
    }
    pub fn read_body<'a, B: IoBuffer>(
        &'a self,
        connection: ConnectionLease<C>,
        buffer: B,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(async move {
            let length = buffer.bytes()?.len();
            self.read_body_range(connection, buffer, 0..length, scope)
                .await
        })
    }
    pub fn write_body<'a, B: SendBuffer>(
        &'a self,
        connection: ConnectionLease<C>,
        buffer: B,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(async move {
            let length = buffer.send_bytes()?.len();
            self.write_body_range(connection, buffer, 0..length, scope)
                .await
        })
    }
    pub fn read_body_range<'a, B: IoBuffer>(
        &'a self,
        mut connection: ConnectionLease<C>,
        mut buffer: B,
        range: Range<usize>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(async move {
            scope.check()?;
            connection.begin_io();
            if range.start > range.end || range.end > buffer.bytes()?.len() {
                return Err(Error::Malformed.into());
            }
            let remaining = connection.rx_remaining.ok_or(Error::Malformed)?;
            let length = range
                .len()
                .min(usize::try_from(remaining).unwrap_or(usize::MAX));
            if length == 0 && remaining != 0 {
                return Err(Error::Malformed.into());
            }
            if length == 0 {
                return Ok(Completion {
                    buffer,
                    bytes: 0,
                    lease: connection,
                });
            }
            if let Some((ahead, mut available)) = connection.read_ahead.take() {
                let count = length.min(available.len());
                buffer.bytes_mut()?[range.start..range.start + count]
                    .copy_from_slice(&ahead.bytes[available.start..available.start + count]);
                available.start += count;
                if !available.is_empty() {
                    connection.read_ahead = Some((ahead, available));
                }
                connection.consume_received(count)?;
                return Ok(Completion {
                    buffer,
                    bytes: count,
                    lease: connection,
                });
            }
            let completed = self
                .reactor
                .recv(
                    connection.socket(),
                    BufferRange::new::<C::Error>(buffer, range.start..range.start + length)?,
                    connection,
                    scope,
                )
                .await?;
            if completed.bytes == 0 || completed.bytes > length {
                return Err(uring_runtime::Error::Io.into());
            }
            connection = completed.lease;
            connection.consume_received(completed.bytes)?;
            Ok(Completion {
                buffer: completed.buffer.into_inner(),
                bytes: completed.bytes,
                lease: connection,
            })
        })
    }
    pub fn write_body_range<'a, B: SendBuffer>(
        &'a self,
        mut connection: ConnectionLease<C>,
        mut buffer: B,
        range: Range<usize>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(async move {
            scope.check()?;
            connection.begin_io();
            if range.start > range.end || range.end > buffer.send_bytes()?.len() {
                return Err(Error::Malformed.into());
            }
            let remaining = connection.tx_remaining.ok_or(Error::Malformed)?;
            if range.len() as u64 > remaining {
                return Err(Error::Malformed.into());
            }
            let mut offset = range.start;
            while offset < range.end {
                let completed = self
                    .reactor
                    .send(
                        connection.socket(),
                        SendRange {
                            buffer,
                            range: offset..range.end,
                        },
                        connection,
                        scope,
                    )
                    .await?;
                if completed.bytes == 0 || completed.bytes > range.end - offset {
                    return Err(uring_runtime::Error::Io.into());
                }
                offset += completed.bytes;
                buffer = completed.buffer.buffer;
                connection = completed.lease;
            }
            connection.consume_sent(range.len())?;
            Ok(Completion {
                buffer,
                bytes: range.len(),
                lease: connection,
            })
        })
    }
    pub fn exchange_head<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        request: MessageHead,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, HeadCompletion<C, MessageHead>> {
        Box::pin(async move {
            if !matches!(request.start, StartLine::Request { .. })
                || request.content_length()?.unwrap_or(0) != 0
            {
                return Err(Error::Malformed.into());
            }
            let sent = self.send_head(connection, request, scope).await?;
            let received = self.receive_head(sent.connection, scope).await?;
            if !matches!(received.value.start, StartLine::Response { .. }) {
                return Err(Error::Malformed.into());
            }
            Ok(received)
        })
    }
    pub fn collect_body<'a>(
        &'a self,
        mut connection: ConnectionLease<C>,
        maximum: usize,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<OwnedBuffer<C>, ConnectionLease<C>>> {
        Box::pin(async move {
            scope.check()?;
            let length = usize::try_from(connection.rx_remaining.ok_or(Error::Malformed)?)
                .map_err(|_| Error::Malformed)?;
            if length > maximum {
                return Err(Error::Malformed.into());
            }
            let mut buffer = self.buffer(length)?;
            let mut offset = 0;
            while offset < length {
                let completed = self
                    .read_body_range(connection, buffer, offset..length, scope)
                    .await?;
                if completed.bytes == 0 {
                    return Err(uring_runtime::Error::Io.into());
                }
                offset += completed.bytes;
                buffer = completed.buffer;
                connection = completed.lease;
            }
            Ok(Completion {
                buffer,
                bytes: length,
                lease: connection,
            })
        })
    }
}
/// Allocation-free snapshot of wire framing, including representation length
/// and method/status semantics even when the effective body length is zero.
#[derive(PartialEq, Eq)]
struct WireFraming {
    request_is_head: Option<bool>,
    status: Option<u16>,
    length: Option<u64>,
    close: bool,
}
impl WireFraming {
    fn new(head: &MessageHead) -> std::result::Result<Self, Error> {
        let (request_is_head, status) = match &head.start {
            StartLine::Request { method, .. } => (Some(method == "HEAD"), None),
            StartLine::Response { status } => (None, Some(*status)),
        };
        Ok(Self {
            request_is_head,
            status,
            length: head.content_length()?,
            close: head.closes_connection()?,
        })
    }
}
fn framing(
    head: &MessageHead,
    request_is_head: bool,
    limit: u64,
) -> std::result::Result<u64, Error> {
    let length = head.content_length()?;
    let body = match head.start {
        StartLine::Request { .. } => length.unwrap_or(0),
        StartLine::Response { status: 100..=199 } => return Err(Error::Malformed),
        StartLine::Response { status: 204 } => {
            if length.is_some_and(|n| n != 0) {
                return Err(Error::Malformed);
            }
            0
        }
        StartLine::Response { status: 304 } => 0,
        StartLine::Response { .. } if request_is_head => 0,
        StartLine::Response { .. } => length.ok_or(Error::Malformed)?,
    };
    if body > limit {
        return Err(Error::Malformed);
    }
    Ok(body)
}
fn rejected_head<C: Context>(
    mut connection: ConnectionLease<C>,
    error: Error,
) -> HeadCompletion<C, Result<C, MessageHead>> {
    connection.poison();
    connection.read_ahead = None;
    connection.rx_remaining = None;
    connection.request_is_head = false;
    HeadCompletion {
        connection,
        value: Err(error.into()),
        _decoded: None,
    }
}

struct Idle<C: Context> {
    state: C::State,
    fd: Rc<Descriptor>,
    reservation: C::Slot,
    since: Instant,
}
struct Entry<C: Context> {
    active: usize,
    idle: Vec<Idle<C>>,
    generation: u64,
}
impl<C: Context> Default for Entry<C> {
    fn default() -> Self {
        Self {
            active: 0,
            idle: Vec::new(),
            generation: 0,
        }
    }
}
struct PoolState<C: Context> {
    entries: BTreeMap<C::Endpoint, Entry<C>>,
    expiry_cursor: Option<C::Endpoint>,
    next_expiry: Instant,
    next_generation: u64,
    closed: bool,
    waiting: VecDeque<Rc<WaitingEntry<C>>>,
    poll_cursor: usize,
    next_waiter_poll: Instant,
}
struct WaitingEntry<C: Context> {
    endpoint: C::Endpoint,
    priority: bool,
    waker: RefCell<Option<Waker>>,
}
struct Waiting<C: Context> {
    state: Rc<RefCell<PoolState<C>>>,
    entry: Rc<WaitingEntry<C>>,
    _reservation: C::Charge,
}
impl<C: Context> Drop for Waiting<C> {
    fn drop(&mut self) {
        let mut state = self.state.borrow_mut();
        state.waiting.retain(|e| !Rc::ptr_eq(e, &self.entry));
        if state.waiting.is_empty() {
            state.waiting = VecDeque::new();
        }
        state.wake_endpoint(&self.entry.endpoint);
    }
}
impl<C: Context> PoolState<C> {
    fn wake_endpoint(&self, endpoint: &C::Endpoint) {
        if let Some(entry) = self.waiting.iter().find(|e| &e.endpoint == endpoint) {
            if let Some(waker) = entry.waker.borrow().as_ref() {
                waker.wake_by_ref();
            }
        }
    }
}
struct ReturnToPool<C: Context> {
    state: Weak<RefCell<PoolState<C>>>,
    endpoint: C::Endpoint,
    generation: u64,
}

pub struct ConnectionLease<C: Context> {
    state: Option<C::State>,
    fd: Rc<Descriptor>,
    reservation: Option<Rc<C::Slot>>,
    pool: Option<ReturnToPool<C>>,
    reusable: bool,
    read_ahead: Option<(OwnedBuffer<C>, Range<usize>)>,
    rx_remaining: Option<u64>,
    tx_remaining: Option<u64>,
    request_is_head: bool,
    close: bool,
}
impl<C: Context> ConnectionLease<C> {
    pub fn from_reserved(fd: Descriptor, slot: C::Slot, state: C::State) -> Result<C, Self> {
        fd.set_nonblocking()?;
        Ok(Self::new(Rc::new(fd), slot, state, None))
    }
    fn new(
        fd: Rc<Descriptor>,
        slot: C::Slot,
        state: C::State,
        pool: Option<ReturnToPool<C>>,
    ) -> Self {
        Self {
            state: Some(state),
            fd,
            reservation: Some(Rc::new(slot)),
            pool,
            reusable: false,
            read_ahead: None,
            rx_remaining: None,
            tx_remaining: None,
            request_is_head: false,
            close: false,
        }
    }
    pub fn state(&self) -> &C::State {
        self.state.as_ref().expect("live state")
    }
    pub fn state_mut(&mut self) -> &mut C::State {
        self.state.as_mut().expect("live state")
    }
    pub fn slot(&self) -> Option<&Rc<C::Slot>> {
        self.reservation.as_ref()
    }
    /// Obtain the descriptor for an owned runtime operation. Pass this entire
    /// connection as its lease: the descriptor alone does not retain admission
    /// or policy attachments through completion and cancellation fences.
    pub fn socket(&self) -> Rc<Descriptor> {
        self.fd.clone()
    }
    pub fn begin_io(&mut self) {
        self.reusable = false;
    }
    pub fn finish_exchange(&mut self) -> Result<C, ()> {
        self.next_round()?;
        self.state_mut().finished();
        self.reusable = !self.close;
        Ok(())
    }
    pub fn next_round(&mut self) -> Result<C, ()> {
        if self.rx_remaining != Some(0) || self.tx_remaining != Some(0) || self.read_ahead.is_some()
        {
            return Err(Error::Malformed.into());
        }
        self.reusable = false;
        self.rx_remaining = None;
        self.tx_remaining = None;
        self.request_is_head = false;
        Ok(())
    }
    pub fn is_reusable(&self) -> bool {
        self.reusable
    }
    pub fn poison(&mut self) {
        self.close = true;
        self.reusable = false;
    }
    pub fn closing(&self) -> bool {
        self.close
    }
    pub fn receive_remaining(&self) -> Option<u64> {
        self.rx_remaining
    }
    pub fn send_remaining(&self) -> Option<u64> {
        self.tx_remaining
    }
    pub fn consume_received(&mut self, bytes: usize) -> Result<C, ()> {
        self.rx_remaining = Some(
            self.rx_remaining
                .ok_or(Error::Malformed)?
                .checked_sub(bytes as u64)
                .ok_or(Error::Malformed)?,
        );
        Ok(())
    }
    pub fn consume_sent(&mut self, bytes: usize) -> Result<C, ()> {
        self.tx_remaining = Some(
            self.tx_remaining
                .ok_or(Error::Malformed)?
                .checked_sub(bytes as u64)
                .ok_or(Error::Malformed)?,
        );
        Ok(())
    }
    /// Excess beyond the framed body poisons reuse, even if the caller takes it away.
    pub fn take_read_ahead(&mut self) -> Option<(OwnedBuffer<C>, Range<usize>)> {
        if self
            .read_ahead
            .as_ref()
            .is_some_and(|(_, r)| self.rx_remaining.is_none_or(|n| r.len() as u64 > n))
        {
            self.poison();
        }
        self.read_ahead.take()
    }
    /// Return a partially consumed tail; validate the fixed allocation bounds.
    pub fn restore_read_ahead(
        &mut self,
        buffer: OwnedBuffer<C>,
        range: Range<usize>,
    ) -> Result<C, ()> {
        if self.read_ahead.is_some() || range.start > range.end || range.end > buffer.bytes.len() {
            return Err(Error::Malformed.into());
        }
        if !range.is_empty() {
            self.begin_io();
            self.read_ahead = Some((buffer, range));
        }
        Ok(())
    }
    #[cfg(feature = "test-util")]
    pub fn set_framing(&mut self, receive: Option<u64>, send: Option<u64>, request_is_head: bool) {
        self.begin_io();
        self.rx_remaining = receive;
        self.tx_remaining = send;
        self.request_is_head = request_is_head;
    }
    #[cfg(feature = "test-util")]
    pub fn remaining_body(&self) -> Option<u64> {
        self.receive_remaining()
    }
}
impl<C: Context> Drop for ConnectionLease<C> {
    fn drop(&mut self) {
        // Transient attachments may themselves return a lease to this same pool.
        // Transform and drop policy strictly outside its RefCell borrow.
        let policy = self.state.take().map(State::idle);
        let Some(target) = &self.pool else {
            return;
        };
        let Some(state) = target.state.upgrade() else {
            return;
        };
        let candidate = if self.reusable && Rc::strong_count(&self.fd) == 1 {
            self.reservation
                .take()
                .and_then(|r| Rc::try_unwrap(r).ok())
                .map(|reservation| Idle {
                    state: policy.expect("live state"),
                    fd: self.fd.clone(),
                    reservation,
                    since: uring_runtime::environment::now(),
                })
        } else {
            None
        };
        let mut candidate = candidate;
        {
            let mut state = state.borrow_mut();
            let closed = state.closed;
            if let Some(entry) = state.entries.get_mut(&target.endpoint) {
                entry.active = entry.active.saturating_sub(1);
                if !closed && entry.generation == target.generation {
                    if let Some(idle) = candidate.take() {
                        entry.idle.push(idle);
                    }
                }
                if entry.active == 0 && entry.idle.is_empty() {
                    state.entries.remove(&target.endpoint);
                }
            }
            state.wake_endpoint(&target.endpoint);
        }
        drop(candidate);
    }
}

// Both heap allocations used by connect observation are charged before allocation.
// The owner follows the runtime lease through completion or abandonment.
struct ConnectOwner<C: Context> {
    connection: RefCell<Option<ConnectionLease<C>>>,
    _charge: C::Charge,
}
impl<C: Context> ConnectOwner<C> {
    fn allocation() -> usize {
        std::mem::size_of::<(usize, usize, Self)>()
            + std::mem::size_of::<(usize, usize, Cell<Option<i32>>)>()
    }
}
pub struct HttpPool<C: Context> {
    reactor: Rc<C::Reactor>,
    context: Rc<C>,
    config: PoolConfig,
    state: Rc<RefCell<PoolState<C>>>,
}
impl<C: Context> HttpPool<C> {
    pub fn new(reactor: Rc<C::Reactor>, context: Rc<C>, config: PoolConfig) -> Self {
        Self {
            reactor,
            context,
            config,
            state: Rc::new(RefCell::new(PoolState {
                entries: BTreeMap::new(),
                expiry_cursor: None,
                next_expiry: uring_runtime::environment::now(),
                next_generation: 0,
                closed: false,
                waiting: VecDeque::new(),
                poll_cursor: 0,
                next_waiter_poll: uring_runtime::environment::now(),
            })),
        }
    }
    pub fn config_mut(&mut self) -> &mut PoolConfig {
        &mut self.config
    }
    pub fn checkout<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        self.checkout_with_state(endpoint, C::State::default(), scope)
    }
    pub fn checkout_with_state<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        checkout: C::State,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        Box::pin(async move {
            scope.check()?;
            let (mut connection, address) = self.prepare_connection(endpoint)?;
            connection.state_mut().attach(checkout);
            self.connect(connection, address, scope).await
        })
    }
    pub fn checkout_wait<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        self.checkout_wait_class(endpoint, scope, false)
    }
    pub fn checkout_metadata<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        self.checkout_wait_class(endpoint, scope, true)
    }
    fn checkout_wait_class<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
        priority: bool,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        Box::pin(async move {
            scope.check()?;
            let mut waiting: Option<Waiting<C>> = None;
            let cancellation = scope.cancellation().map(|c| c.subscribe()).transpose()?;
            let (connection, address) = poll_fn(|cx| {
                if let Some(c) = &cancellation {
                    c.register(cx.waker());
                }
                scope.check()?;
                if self.state.borrow().closed || self.context.stopped() {
                    return Poll::Ready(Err(uring_runtime::Error::Unavailable.into()));
                }
                let first = self
                    .state
                    .borrow()
                    .waiting
                    .iter()
                    .find(|e| &e.endpoint == endpoint && e.priority == priority)
                    .cloned();
                let turn = first.as_ref().is_none_or(|first| {
                    waiting
                        .as_ref()
                        .is_some_and(|w| Rc::ptr_eq(first, &w.entry))
                });
                if turn && self.class_available(endpoint, priority) {
                    match self.prepare_connection(endpoint) {
                        Err(e) if e == C::Error::from(uring_runtime::Error::Overloaded) => (),
                        result => return Poll::Ready(result),
                    }
                }
                if waiting.is_none() {
                    if self.state.borrow().waiting.len() >= self.config.waiter_cap {
                        return Poll::Ready(Err(uring_runtime::Error::Overloaded.into()));
                    }
                    let reservation = self.context.charge(
                        std::mem::size_of::<Waiting<C>>()
                            + std::mem::size_of::<WaitingEntry<C>>()
                            + endpoint.allocation()
                            + 128,
                    )?;
                    let entry = Rc::new(WaitingEntry {
                        endpoint: endpoint.clone(),
                        priority,
                        waker: RefCell::new(None),
                    });
                    self.state.borrow_mut().waiting.push_back(entry.clone());
                    waiting = Some(Waiting {
                        state: self.state.clone(),
                        entry,
                        _reservation: reservation,
                    });
                }
                *waiting.as_ref().unwrap().entry.waker.borrow_mut() = Some(cx.waker().clone());
                Poll::Pending
            })
            .await?;
            drop(waiting);
            self.connect(connection, address, scope).await
        })
    }
    fn class_available(&self, endpoint: &C::Endpoint, priority: bool) -> bool {
        let cap = endpoint.capacity(&self.config);
        if priority || cap <= 1 || endpoint.priority_headroom() == 0 {
            return true;
        }
        self.state
            .borrow()
            .entries
            .get(endpoint)
            .is_none_or(|e| e.active < cap.saturating_sub(endpoint.priority_headroom()))
    }
    pub fn poll_waiters(&self, budget: usize) {
        self.expire_idle_budgeted(budget);
        let mut state = self.state.borrow_mut();
        let now = uring_runtime::environment::now();
        if budget == 0 || state.waiting.is_empty() || now < state.next_waiter_poll {
            return;
        }
        state.next_waiter_poll = now + Duration::from_millis(1);
        for _ in 0..budget.min(state.waiting.len()) {
            state.poll_cursor %= state.waiting.len();
            if let Some(waker) = state.waiting[state.poll_cursor].waker.borrow().as_ref() {
                waker.wake_by_ref();
            }
            state.poll_cursor += 1;
        }
    }
    pub fn prepare_connection(
        &self,
        endpoint: &C::Endpoint,
    ) -> Result<C, (ConnectionLease<C>, Option<SocketAddress>)> {
        if self.context.stopped() {
            return Err(uring_runtime::Error::Unavailable.into());
        }
        // All evicted policy and resource owners are dropped after releasing the borrow.
        let mut garbage = Vec::new();
        let prepared: Result<C, _> = (|| {
            let mut state = self.state.borrow_mut();
            if state.closed {
                return Err(uring_runtime::Error::Unavailable.into());
            }
            if !state.entries.contains_key(endpoint) {
                if state.entries.len() >= self.config.max_endpoints {
                    let keys: Vec<_> = state
                        .entries
                        .iter()
                        .filter(|(_, e)| e.active == 0)
                        .map(|(k, _)| k.clone())
                        .collect();
                    for key in keys {
                        if let Some(entry) = state.entries.remove(&key) {
                            garbage.extend(entry.idle);
                        }
                    }
                }
                if state.entries.len() >= self.config.max_endpoints {
                    return Err(uring_runtime::Error::Overloaded.into());
                }
                state.next_generation = state
                    .next_generation
                    .checked_add(1)
                    .ok_or(uring_runtime::Error::Unavailable)?;
                let generation = state.next_generation;
                state.entries.insert(
                    endpoint.clone(),
                    Entry {
                        generation,
                        ..Entry::default()
                    },
                );
            }
            let entry = state
                .entries
                .get_mut(endpoint)
                .ok_or(uring_runtime::Error::Unavailable)?;
            let now = uring_runtime::environment::now();
            let mut i = 0;
            while i < entry.idle.len() {
                if now.saturating_duration_since(entry.idle[i].since) >= self.config.idle_timeout {
                    garbage.push(entry.idle.swap_remove(i));
                } else {
                    i += 1;
                }
            }
            if entry.active >= endpoint.capacity(&self.config) {
                return Err(uring_runtime::Error::Overloaded.into());
            }
            entry.active += 1;
            Ok((entry.idle.pop(), entry.generation))
        })();
        drop(garbage);
        let (idle, generation) = prepared?;
        let target = ReturnToPool {
            state: Rc::downgrade(&self.state),
            endpoint: endpoint.clone(),
            generation,
        };
        let mut slot = ConnectingSlot(Some(target));
        if let Some(idle) = idle {
            if idle.fd.idle_healthy() {
                return Ok((
                    ConnectionLease::new(idle.fd, idle.reservation, idle.state, slot.0.take()),
                    None,
                ));
            }
        }
        let reservation = match self.context.outbound_slot() {
            Err(e) if e == C::Error::from(uring_runtime::Error::Overloaded) => {
                self.clear_idle();
                self.context.outbound_slot()?
            }
            result => result?,
        };
        let address = endpoint.address()?;
        let domain = match &address {
            SocketAddress::Inet(a) if a.is_ipv4() => libc::AF_INET,
            SocketAddress::Inet(_) => libc::AF_INET6,
            SocketAddress::Unix(_) => libc::AF_UNIX,
        };
        let fd = Descriptor::socket(domain)?;
        if matches!(address, SocketAddress::Inet(_)) {
            fd.enable_tcp_nodelay(self.config.tcp_nodelay)?;
        }
        Ok((
            ConnectionLease::new(Rc::new(fd), reservation, C::State::default(), slot.0.take()),
            Some(address),
        ))
    }
    async fn connect(
        &self,
        connection: ConnectionLease<C>,
        address: Option<SocketAddress>,
        scope: &C::Scope,
    ) -> Result<C, ConnectionLease<C>> {
        let Some(address) = address else {
            return Ok(connection);
        };
        // Observation is shared separately from the completion-owned connection.
        // A guard references policy without moving it out of the operation's fence.
        let charge = self.context.charge(ConnectOwner::<C>::allocation())?;
        let fd = connection.socket();
        let observation = Rc::new(ConnectOwner {
            connection: RefCell::new(Some(connection)),
            _charge: charge,
        });
        let errno = Rc::new(Cell::new(None));
        let result = self
            .reactor
            .connect_with_observation(fd, address, observation.clone(), Some(errno.clone()), scope)
            .await;
        if result.is_err() && scope.check().is_ok() {
            observation
                .connection
                .borrow()
                .as_ref()
                .unwrap()
                .state()
                .connect_failed(errno.get());
        }
        drop(result?);
        let connection = observation
            .connection
            .borrow_mut()
            .take()
            .expect("connect owner");
        Ok(connection)
    }
    fn clear_idle(&self) {
        let garbage: Vec<_> = self
            .state
            .borrow_mut()
            .entries
            .values_mut()
            .flat_map(|e| std::mem::take(&mut e.idle))
            .collect();
        drop(garbage);
    }
    pub fn expire_idle_budgeted(&self, budget: usize) {
        use std::ops::Bound::{Excluded, Unbounded};
        let now = uring_runtime::environment::now();
        let mut garbage = Vec::new();
        {
            let mut state = self.state.borrow_mut();
            if budget == 0 || now < state.next_expiry {
                return;
            }
            state.next_expiry = now + Duration::from_millis(100);
            for _ in 0..budget.min(state.entries.len()) {
                let next = state
                    .expiry_cursor
                    .as_ref()
                    .and_then(|cursor| state.entries.range((Excluded(cursor), Unbounded)).next())
                    .or_else(|| state.entries.iter().next())
                    .map(|(k, _)| k.clone());
                let Some(key) = next else {
                    break;
                };
                let entry = state.entries.get_mut(&key).expect("selected endpoint");
                let mut i = 0;
                while i < entry.idle.len() {
                    if now.saturating_duration_since(entry.idle[i].since)
                        >= self.config.idle_timeout
                    {
                        garbage.push(entry.idle.swap_remove(i));
                    } else {
                        i += 1;
                    }
                }
                if entry.active == 0 && entry.idle.is_empty() {
                    state.entries.remove(&key);
                }
                state.expiry_cursor = Some(key);
            }
        }
        drop(garbage);
    }
    pub fn invalidate(&self, endpoint: &C::Endpoint) {
        let garbage = {
            let mut state = self.state.borrow_mut();
            let Some(generation) = state.next_generation.checked_add(1) else {
                state.closed = true;
                return;
            };
            state.next_generation = generation;
            state.entries.get_mut(endpoint).map(|e| {
                e.generation = generation;
                std::mem::take(&mut e.idle)
            })
        };
        drop(garbage);
    }
    pub fn close(&self) {
        {
            let mut state = self.state.borrow_mut();
            state.closed = true;
            for entry in &state.waiting {
                if let Some(waker) = entry.waker.borrow().as_ref() {
                    waker.wake_by_ref();
                }
            }
        }
        self.clear_idle();
    }
    #[cfg(feature = "test-util")]
    pub fn expire_idle(&self) {
        self.state.borrow_mut().next_expiry = uring_runtime::environment::now();
        self.expire_idle_budgeted(usize::MAX);
    }
    #[cfg(feature = "test-util")]
    pub fn snapshot(&self) -> PoolSnapshot<C::Endpoint> {
        let s = self.state.borrow();
        PoolSnapshot {
            waiting: s.waiting.len(),
            entries: s
                .entries
                .iter()
                .map(|(k, e)| (k.clone(), (e.active, e.idle.len())))
                .collect(),
        }
    }
}
#[cfg(feature = "test-util")]
pub struct PoolSnapshot<E> {
    pub waiting: usize,
    pub entries: BTreeMap<E, (usize, usize)>,
}
impl<C: Context> Drop for HttpPool<C> {
    fn drop(&mut self) {
        self.close();
    }
}
struct ConnectingSlot<C: Context>(Option<ReturnToPool<C>>);
impl<C: Context> Drop for ConnectingSlot<C> {
    fn drop(&mut self) {
        if let Some(target) = &self.0 {
            if let Some(state) = target.state.upgrade() {
                let mut state = state.borrow_mut();
                if let Some(entry) = state.entries.get_mut(&target.endpoint) {
                    entry.active = entry.active.saturating_sub(1);
                    if entry.active == 0 && entry.idle.is_empty() {
                        state.entries.remove(&target.endpoint);
                    }
                }
            }
        }
    }
}
