//! Exclusive connections and bounded pools. No executor or application policy.
//!
//! Charges cover payload buffers and explicit admissions, not every metadata
//! allocation. The caller drives the reactor and ticks pool maintenance.
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

/// A result using the caller's connection error type.
pub type Result<C, T> = std::result::Result<T, <C as Context>::Error>;
/// An owned, lazy operation retaining resources through runtime completion.
pub type Operation<'a, C, T> = uring_runtime::Operation<'a, T, <C as Context>::Error>;

/// Caller-owned runtime, admission, and connection policy.
pub trait Context: 'static {
    /// Common HTTP, runtime, and application error.
    type Error: Copy + Send + PartialEq + From<Error> + From<uring_runtime::Error> + 'static;
    /// Cancellation and deadline checks for each operation.
    type Scope: Scope<Error = Self::Error>;
    /// Runtime completion budget, separate from ordinary admission.
    type Budget: Budget;
    /// Caller-owned reactor handle.
    type Reactor: Deref<Target = Reactor<Self::Scope, Self::Budget>>;
    /// Reservation released when admitted storage is no longer retained.
    type Charge: 'static;
    /// Connection admission retained through the runtime fence.
    type Slot: 'static;
    /// Header fields requiring opaque whitespace handling.
    type Opaque: Opaque;
    /// Policy carried by each connection.
    type State: State<Self::Error>;
    /// Ordered pool key and connection destination.
    type Endpoint: Endpoint<Self::Error>;
    /// Ordinary admission, not the runtime's completion/drain reserve.
    fn charge(&self, bytes: usize) -> Result<Self, Self::Charge>
    where
        Self: Sized;
    /// Reserve admission for a new outbound connection.
    fn outbound_slot(&self) -> Result<Self, Self::Slot>
    where
        Self: Sized;
    /// Whether new work must be rejected.
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
    /// Observe a successfully finished exchange.
    fn finished(&mut self) {}
    /// Invoked on the retained observation state after a failed connect completes.
    fn connect_failed(&self, _errno: Option<i32>) {}
    /// Install checkout-local policy before connect submission. The default replaces
    /// all state; session-preserving policies must override this method.
    fn attach(&mut self, checkout: Self) {
        *self = checkout;
    }
    /// Transform policy before returning to idle; the default returns self unchanged.
    /// Implementors must override this to clear transient attachments.
    fn idle(self) -> Self {
        self
    }
}
impl<E> State<E> for () {}

/// Pool identity, destination address, and endpoint-specific capacity policy.
pub trait Endpoint<E>: Clone + Ord + 'static {
    /// Resolve the already-selected destination without DNS or background work.
    fn address(&self) -> std::result::Result<SocketAddress, E>;
    /// Maximum active connections for this endpoint.
    fn capacity(&self, config: &PoolConfig) -> usize {
        config.per_endpoint
    }
    /// Only endpoints explicitly opting in reserve priority headroom.
    fn priority_headroom(&self) -> usize {
        0
    }
    /// Estimate key storage included in waiter admission.
    fn allocation(&self) -> usize {
        std::mem::size_of_val(self)
    }
}

/// Connection, waiter, and idle-retention limits supplied by the caller.
#[derive(Clone)]
pub struct PoolConfig {
    /// Default active-connection cap for each endpoint.
    pub per_endpoint: usize,
    /// Secondary endpoint cap available to caller-defined endpoint policy.
    pub secondary_cap: usize,
    /// Maximum number of endpoint entries retained by the pool.
    pub max_endpoints: usize,
    /// Maximum admitted checkout registrations waiting for capacity.
    pub waiter_cap: usize,
    /// Idle age at which a connection may be evicted.
    pub idle_timeout: Duration,
    /// Whether newly created TCP sockets disable Nagle's algorithm.
    pub tcp_nodelay: bool,
}

/// Fixed-length HTTP I/O that transfers owned leases through runtime fences.
pub struct HttpIo<C: Context> {
    reactor: Rc<C::Reactor>,
    codec: Codec<C::Opaque>,
    receive_limit: u64,
    send_limit: u64,
    context: Rc<C>,
    idle_buffer: Rc<RefCell<Option<OwnedBuffer<C>>>>,
}

/// Bounded exclusive connections with caller-driven waiting and idle maintenance.
pub struct HttpPool<C: Context> {
    reactor: Rc<C::Reactor>,
    context: Rc<C>,
    config: PoolConfig,
    state: Rc<RefCell<PoolState<C>>>,
}

/// Exclusive socket ownership with policy, admission, framing, and reuse state.
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

/// A head result and the connection and decoded-storage charge it retains.
pub struct HeadCompletion<C: Context, T> {
    /// Connection ownership recovered after head processing.
    pub connection: ConnectionLease<C>,
    /// Decoded head or operation-specific head outcome.
    pub value: T,
    /// Admission retained while the decoded head remains owned by this completion.
    pub _decoded: Option<C::Charge>,
}

/// Fixed admitted storage, zeroized before release or single-buffer reuse.
pub struct OwnedBuffer<C: Context> {
    bytes: Vec<u8>,
    reservation: Option<C::Charge>,
    pool: Weak<RefCell<Option<Self>>>,
}

/// A checked fixed view retaining ownership of its complete backing buffer.
pub struct BufferRange<B: IoBuffer> {
    buffer: B,
    range: Range<usize>,
}

/// Test-only pool counts, with each endpoint mapped to its active and idle counts.
#[cfg(feature = "test-util")]
pub struct PoolSnapshot<E> {
    /// Number of registered checkout waiters.
    pub waiting: usize,
    /// Active and idle connection counts keyed by endpoint.
    pub entries: BTreeMap<E, (usize, usize)>,
}

impl Default for PoolConfig {
    /// Use one active connection per endpoint and a thirty-second idle timeout.
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

impl<C: Context> Drop for OwnedBuffer<C> {
    /// Clear bytes and return storage only if the idle cache can accept it.
    fn drop(&mut self) {
        self.bytes.as_mut_slice().zeroize();
        if let Some(pool) = self.pool.upgrade()
            && let Ok(mut idle) = pool.try_borrow_mut()
            && idle.is_none()
        {
            *idle = Some(Self {
                bytes: std::mem::take(&mut self.bytes),
                reservation: self.reservation.take(),
                pool: Weak::new(),
            });
        }
    }
}
impl<C: Context> OwnedBuffer<C> {
    /// Allocate zeroed fixed-length storage after obtaining its charge.
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
    /// Copy borrowed bytes into independently admitted storage.
    pub fn copy_from(context: &C, bytes: &[u8]) -> Result<C, Self> {
        let mut buffer = Self::new(context, bytes.len())?;
        buffer.bytes.copy_from_slice(bytes);
        Ok(buffer)
    }
}
// SAFETY: fixed, private backing allocation and charge remain owned through completion.
unsafe impl<C: Context> IoBuffer for OwnedBuffer<C> {
    type Error = C::Error;
    /// Borrow the stable backing bytes.
    fn bytes(&self) -> Result<C, &[u8]> {
        Ok(&self.bytes)
    }
    /// Mutably borrow the stable backing bytes.
    fn bytes_mut(&mut self) -> Result<C, &mut [u8]> {
        Ok(&mut self.bytes)
    }
}
impl<B: IoBuffer> BufferRange<B> {
    /// Validate a range against the buffer before constructing its view.
    pub fn new<E: From<B::Error> + From<Error>>(
        buffer: B,
        range: Range<usize>,
    ) -> std::result::Result<Self, E> {
        if range.start > range.end || range.end > buffer.bytes()?.len() {
            return Err(Error::Malformed.into());
        }
        Ok(Self { buffer, range })
    }
    /// Recover the complete backing buffer.
    pub fn into_inner(self) -> B {
        self.buffer
    }
}
// SAFETY: the fixed view retains the complete stable backing owner.
unsafe impl<B: IoBuffer> IoBuffer for BufferRange<B> {
    type Error = B::Error;
    /// Borrow only the checked view.
    fn bytes(&self) -> std::result::Result<&[u8], B::Error> {
        Ok(&self.buffer.bytes()?[self.range.clone()])
    }
    /// Mutably borrow only the checked view.
    fn bytes_mut(&mut self) -> std::result::Result<&mut [u8], B::Error> {
        Ok(&mut self.buffer.bytes_mut()?[self.range.clone()])
    }
}
impl<C: Context> HttpIo<C> {
    /// Inspect receive framing without submitting I/O.
    #[cfg(feature = "test-util")]
    pub fn framing(&self, head: &MessageHead, request_is_head: bool) -> Result<C, u64> {
        framing(head, request_is_head, self.receive_limit).map_err(Into::into)
    }
    /// Bind a reactor, codec, caller policy, and directional body limits.
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
    /// Borrow the runtime handle used by this I/O facade.
    pub fn reactor(&self) -> &Rc<C::Reactor> {
        &self.reactor
    }
    /// Wait for hangup while retaining the connection's admission slot.
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
    /// Obtain admitted zeroed storage, reusing an exact-size idle buffer if present.
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
    /// Release the cached buffer and its charge.
    pub fn reclaim_buffer(&self) {
        self.idle_buffer.borrow_mut().take();
    }
    /// Report the admission retained by the idle buffer cache.
    #[cfg(feature = "test-util")]
    pub fn retained_buffer_bytes(&self) -> usize {
        self.idle_buffer
            .borrow()
            .as_ref()
            .map_or(0, |b| b.bytes.len().max(1))
    }
    /// Share runtime and buffer storage with a more restrictive header limit.
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
    /// Receive and admit a head using the configured header limit.
    pub fn receive_head<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, HeadCompletion<C, MessageHead>> {
        self.receive_head_limited(connection, scope, self.codec.header_limit())
    }
    /// Receive and admit a head under an additional wire-size cap.
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
    /// Receive a request, retaining a poisoned connection on wire rejection.
    pub fn receive_request_head_limited<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        scope: &'a C::Scope,
        header_limit: usize,
    ) -> Operation<'a, C, HeadCompletion<C, Result<C, MessageHead>>> {
        self.receive_head_outcome(connection, scope, header_limit, true)
    }
    /// Decode and admit a head without allowing hooks to change wire framing.
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
    /// Validate, sign, and send a head using the signed head's final framing.
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
    /// Read up to the buffer length, accessing the buffer only when polled.
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
            self.read_body_range_impl(connection, buffer, 0..length, scope)
                .await
        })
    }
    /// Send the complete buffer, accessing its bytes only when polled.
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
            self.write_body_range_impl(connection, buffer, 0..length, scope)
                .await
        })
    }
    /// Copy borrowed bytes into admitted storage before submitting a body write.
    /// The owned copy and connection remain retained through the completion fence.
    pub fn write_body_bytes<'a>(
        &'a self,
        connection: ConnectionLease<C>,
        bytes: &'a [u8],
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<OwnedBuffer<C>, ConnectionLease<C>>> {
        Box::pin(async move {
            let mut buffer = self.buffer(bytes.len())?;
            buffer.bytes.copy_from_slice(bytes);
            let length = buffer.send_bytes()?.len();
            self.write_body_range_impl(connection, buffer, 0..length, scope)
                .await
        })
    }
    /// Fill the caller's entire fixed buffer, possibly across read-ahead and
    /// multiple receives. A framed end or socket EOF before the buffer is full
    /// is an I/O error. An empty buffer completes without submitting I/O.
    pub fn read_body_exact<'a, B: IoBuffer>(
        &'a self,
        connection: ConnectionLease<C>,
        buffer: B,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(self.read_body_exact_impl(connection, buffer, scope))
    }
    /// Fill a fixed buffer without allocating nested operation futures.
    async fn read_body_exact_impl<B: IoBuffer>(
        &self,
        mut connection: ConnectionLease<C>,
        mut buffer: B,
        scope: &C::Scope,
    ) -> Result<C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        let length = buffer.bytes()?.len();
        let mut offset = 0;
        while offset < length {
            let completed = self
                .read_body_range_impl(connection, buffer, offset..length, scope)
                .await?;
            if completed.bytes == 0 || completed.bytes > length - offset {
                return Err(uring_runtime::Error::Io.into());
            }
            offset += completed.bytes;
            buffer = completed.buffer;
            connection = completed.lease;
        }
        Ok(Completion {
            buffer,
            bytes: offset,
            lease: connection,
        })
    }
    /// Read a checked range, returning a partial count without exceeding framing.
    pub fn read_body_range<'a, B: IoBuffer>(
        &'a self,
        connection: ConnectionLease<C>,
        buffer: B,
        range: Range<usize>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(self.read_body_range_impl(connection, buffer, range, scope))
    }
    /// Validate lazily and receive from read-ahead or a completion-owned buffer.
    async fn read_body_range_impl<B: IoBuffer>(
        &self,
        mut connection: ConnectionLease<C>,
        mut buffer: B,
        range: Range<usize>,
        scope: &C::Scope,
    ) -> Result<C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
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
    }
    /// Send a complete checked range without exceeding the framed body length.
    pub fn write_body_range<'a, B: SendBuffer>(
        &'a self,
        connection: ConnectionLease<C>,
        buffer: B,
        range: Range<usize>,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
        Box::pin(self.write_body_range_impl(connection, buffer, range, scope))
    }
    /// Validate lazily and retain each send suffix through its completion fence.
    async fn write_body_range_impl<B: SendBuffer>(
        &self,
        mut connection: ConnectionLease<C>,
        mut buffer: B,
        range: Range<usize>,
        scope: &C::Scope,
    ) -> Result<C, Completion<B, ConnectionLease<C>>>
    where
        C::Error: From<B::Error>,
    {
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
    }
    /// Send a bodyless request and receive one response head.
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
    /// Collect the remaining framed body into admitted storage under a size cap.
    pub fn collect_body<'a>(
        &'a self,
        connection: ConnectionLease<C>,
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
            let buffer = self.buffer(length)?;
            self.read_body_exact_impl(connection, buffer, scope).await
        })
    }
}
impl<C: Context> ConnectionLease<C> {
    /// Adopt an already admitted descriptor and set it nonblocking.
    pub fn from_reserved(fd: Descriptor, slot: C::Slot, state: C::State) -> Result<C, Self> {
        fd.set_nonblocking()?;
        Ok(Self::new(Rc::new(fd), slot, state, None))
    }
    /// Assemble an unfinished lease with an optional return-to-pool address.
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
    /// Borrow the connection's live policy.
    pub fn state(&self) -> &C::State {
        self.state.as_ref().expect("live state")
    }
    /// Mutably borrow the connection's live policy.
    pub fn state_mut(&mut self) -> &mut C::State {
        self.state.as_mut().expect("live state")
    }
    /// Borrow admission for operations that must retain it beyond a socket clone.
    pub fn slot(&self) -> Option<&Rc<C::Slot>> {
        self.reservation.as_ref()
    }
    /// Obtain the descriptor for an owned runtime operation. Pass this entire
    /// connection as its lease: the descriptor alone does not retain admission
    /// or policy attachments through completion and cancellation fences.
    pub fn socket(&self) -> Rc<Descriptor> {
        self.fd.clone()
    }
    /// Disable reuse before starting any further I/O.
    pub fn begin_io(&mut self) {
        self.reusable = false;
    }
    /// Validate complete framing, notify policy, and permit reuse unless closing.
    pub fn finish_exchange(&mut self) -> Result<C, ()> {
        self.next_round()?;
        self.state_mut().finished();
        self.reusable = !self.close;
        Ok(())
    }
    /// Reset completed framing only when no body bytes or read-ahead remain.
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
    /// Whether a finished exchange currently permits returning this socket idle.
    pub fn is_reusable(&self) -> bool {
        self.reusable
    }
    /// Permanently forbid reuse of this connection.
    pub fn poison(&mut self) {
        self.close = true;
        self.reusable = false;
    }
    /// Whether wire policy or an error requires closing after this exchange.
    pub fn closing(&self) -> bool {
        self.close
    }
    /// Return unread framed bytes, or none before receive framing is established.
    pub fn receive_remaining(&self) -> Option<u64> {
        self.rx_remaining
    }
    /// Return unsent framed bytes, or none before send framing is established.
    pub fn send_remaining(&self) -> Option<u64> {
        self.tx_remaining
    }
    /// Account for received bytes without allowing framed-length underflow.
    pub fn consume_received(&mut self, bytes: usize) -> Result<C, ()> {
        self.rx_remaining = Some(
            self.rx_remaining
                .ok_or(Error::Malformed)?
                .checked_sub(bytes as u64)
                .ok_or(Error::Malformed)?,
        );
        Ok(())
    }
    /// Account for sent bytes without allowing framed-length underflow.
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
    /// Install synthetic framing for caller tests without submitting I/O.
    #[cfg(any(test, feature = "test-util"))]
    pub fn set_framing(&mut self, receive: Option<u64>, send: Option<u64>, request_is_head: bool) {
        self.begin_io();
        self.rx_remaining = receive;
        self.tx_remaining = send;
        self.request_is_head = request_is_head;
    }
    /// Inspect the remaining receive body in caller tests.
    #[cfg(feature = "test-util")]
    pub fn remaining_body(&self) -> Option<u64> {
        self.receive_remaining()
    }
}
impl<C: Context> Drop for ConnectionLease<C> {
    /// Transform policy outside pool borrows and return only reusable idle owners.
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
                if !closed
                    && entry.generation == target.generation
                    && let Some(idle) = candidate.take()
                {
                    entry.idle.push(idle);
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

impl<C: Context> HttpPool<C> {
    /// Create an empty pool without spawning maintenance tasks.
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
    /// Adjust policy applied by subsequent checkouts and maintenance.
    pub fn config_mut(&mut self) -> &mut PoolConfig {
        &mut self.config
    }
    /// Check out a connection with default policy, failing rather than queueing.
    pub fn checkout<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        self.checkout_with_state(endpoint, C::State::default(), scope)
    }
    /// Attach checkout policy before any connect submission, including on reuse.
    pub fn checkout_with_state<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        checkout: C::State,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        Box::pin(async move {
            scope.check()?;
            let (mut connection, address) = self.prepare_connection_inner(endpoint)?;
            connection.state_mut().attach(checkout);
            self.connect(connection, address, scope).await
        })
    }
    /// Wait for ordinary endpoint capacity with bounded queue admission.
    pub fn checkout_wait<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        self.checkout_wait_class(endpoint, scope, false)
    }
    /// Wait with access to endpoint priority headroom when explicitly configured.
    pub fn checkout_metadata<'a>(
        &'a self,
        endpoint: &'a C::Endpoint,
        scope: &'a C::Scope,
    ) -> Operation<'a, C, ConnectionLease<C>> {
        self.checkout_wait_class(endpoint, scope, true)
    }
    /// Queue by endpoint and priority class while retaining cancellation registration.
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
                    match self.prepare_connection_inner(endpoint) {
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
    /// Check ordinary headroom without changing the endpoint's total capacity.
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
    /// Maintain at most `budget` endpoints and wake at most `budget` waiters.
    /// Expiry scans every idle socket in each selected endpoint, not `budget` sockets.
    /// Expiry is throttled to 100 ms and waiter polling to 1 ms; zero does neither.
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
    /// Prepare an admitted test connection without submitting its connect operation.
    #[cfg(any(test, feature = "test-util"))]
    pub fn prepare_connection(
        &self,
        endpoint: &C::Endpoint,
    ) -> Result<C, (ConnectionLease<C>, Option<SocketAddress>)> {
        self.prepare_connection_inner(endpoint)
    }
    /// Reserve capacity and reuse healthy idle storage or create an admitted socket.
    fn prepare_connection_inner(
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
            entry.expire_idle(now, self.config.idle_timeout, &mut garbage);
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
        if let Some(idle) = idle
            && idle.fd.idle_healthy()
        {
            return Ok((
                ConnectionLease::new(idle.fd, idle.reservation, idle.state, slot.0.take()),
                None,
            ));
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
    /// Connect while keeping observation policy and admission inside runtime fencing.
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
    /// Evict idle owners and drop their policies after releasing the pool borrow.
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
    /// Scan at most `budget` endpoints, dropping expired owners outside pool borrows.
    fn expire_idle_budgeted(&self, budget: usize) {
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
                entry.expire_idle(now, self.config.idle_timeout, &mut garbage);
                if entry.active == 0 && entry.idle.is_empty() {
                    state.entries.remove(&key);
                }
                state.expiry_cursor = Some(key);
            }
        }
        drop(garbage);
    }
    /// Invalidate idle and checked-out generations for caller tests.
    #[cfg(feature = "test-util")]
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
    /// Reject new work, wake queued checkouts, and release idle connections.
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
    /// Force an unthrottled full endpoint expiry pass for caller tests.
    #[cfg(feature = "test-util")]
    pub fn expire_idle(&self) {
        self.state.borrow_mut().next_expiry = uring_runtime::environment::now();
        self.expire_idle_budgeted(usize::MAX);
    }
    /// Snapshot waiter counts and per-endpoint active and idle counts for tests.
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
impl<C: Context> Drop for HttpPool<C> {
    /// Close the pool while outstanding leases retain their own runtime resources.
    fn drop(&mut self) {
        self.close();
    }
}
/// A healthy reusable connection and its retained policy and admission.
struct Idle<C: Context> {
    state: C::State,
    fd: Rc<Descriptor>,
    reservation: C::Slot,
    since: Instant,
}
/// Active and idle connections belonging to one endpoint generation.
struct Entry<C: Context> {
    active: usize,
    idle: Vec<Idle<C>>,
    generation: u64,
}
impl<C: Context> Default for Entry<C> {
    /// Create an empty entry before assigning its pool generation.
    fn default() -> Self {
        Self {
            active: 0,
            idle: Vec::new(),
            generation: 0,
        }
    }
}
impl<C: Context> Entry<C> {
    /// Move expired owners into deferred garbage without dropping policy in a borrow.
    fn expire_idle(&mut self, now: Instant, timeout: Duration, garbage: &mut Vec<Idle<C>>) {
        let mut i = 0;
        while i < self.idle.len() {
            if now.saturating_duration_since(self.idle[i].since) >= timeout {
                garbage.push(self.idle.swap_remove(i));
            } else {
                i += 1;
            }
        }
    }
}
/// Shared endpoint table, maintenance cursors, and bounded waiting queue.
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
/// One queued checkout and the task to wake when its endpoint may be available.
struct WaitingEntry<C: Context> {
    endpoint: C::Endpoint,
    priority: bool,
    waker: RefCell<Option<Waker>>,
}
/// Admission-backed queue registration removed when checkout finishes or cancels.
struct Waiting<C: Context> {
    state: Rc<RefCell<PoolState<C>>>,
    entry: Rc<WaitingEntry<C>>,
    _reservation: C::Charge,
}
impl<C: Context> Drop for Waiting<C> {
    /// Remove this registration and wake the next waiter for its endpoint.
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
    /// Notify the first queued waiter for an endpoint without moving its owner.
    fn wake_endpoint(&self, endpoint: &C::Endpoint) {
        if let Some(entry) = self.waiting.iter().find(|e| &e.endpoint == endpoint)
            && let Some(waker) = entry.waker.borrow().as_ref()
        {
            waker.wake_by_ref();
        }
    }
}
/// Weak return address preventing stale generations from reentering the idle pool.
struct ReturnToPool<C: Context> {
    state: Weak<RefCell<PoolState<C>>>,
    endpoint: C::Endpoint,
    generation: u64,
}

/// Charged connect observation retained through completion or abandonment.
/// Both observation allocations are admitted before allocation.
struct ConnectOwner<C: Context> {
    connection: RefCell<Option<ConnectionLease<C>>>,
    _charge: C::Charge,
}
impl<C: Context> ConnectOwner<C> {
    /// Size both reference-counted observation allocations for admission.
    fn allocation() -> usize {
        std::mem::size_of::<(usize, usize, Self)>()
            + std::mem::size_of::<(usize, usize, Cell<Option<i32>>)>()
    }
}

/// Roll back active admission if preparing a connection fails before lease creation.
struct ConnectingSlot<C: Context>(Option<ReturnToPool<C>>);
impl<C: Context> Drop for ConnectingSlot<C> {
    /// Release an untransferred active slot without adding a waiter wakeup.
    fn drop(&mut self) {
        if let Some(target) = &self.0
            && let Some(state) = target.state.upgrade()
        {
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

/// Immutable send view retaining its entire backing owner.
struct SendRange<B: SendBuffer> {
    buffer: B,
    range: Range<usize>,
}
// SAFETY: immutable fixed view retains its complete send owner.
unsafe impl<B: SendBuffer> SendBuffer for SendRange<B> {
    type Error = B::Error;
    /// Borrow the selected send suffix.
    fn send_bytes(&self) -> std::result::Result<&[u8], B::Error> {
        Ok(&self.buffer.send_bytes()?[self.range.clone()])
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
    /// Snapshot framing-sensitive fields before running an admission hook.
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
/// Validate fixed-length message semantics and return the effective body length.
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
/// Preserve a rejected connection for a response, but forbid reuse and body reads.
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

/// Connection framing, ownership, policy, and cancellation regression tests.
#[cfg(test)]
mod tests {
    use super::*;

    /// Test error preserving the HTTP or runtime failure source.
    #[derive(Clone, Copy, Debug, PartialEq)]
    enum Failure {
        Http(Error),
        Runtime(uring_runtime::Error),
    }
    impl From<Error> for Failure {
        /// Preserve an HTTP failure for assertions.
        fn from(e: Error) -> Self {
            Self::Http(e)
        }
    }
    impl From<uring_runtime::Error> for Failure {
        /// Preserve a runtime failure for assertions.
        fn from(e: uring_runtime::Error) -> Self {
            Self::Runtime(e)
        }
    }
    /// Always-live scope for explicit reactor-driving tests.
    #[derive(Clone)]
    struct TestScope;
    impl Scope for TestScope {
        type Error = Failure;
        /// Keep the test operation live.
        fn check(&self) -> std::result::Result<(), Failure> {
            Ok(())
        }
    }
    /// Counted admission released when its owner drops.
    struct Charge(Rc<Cell<usize>>, usize);
    impl Drop for Charge {
        /// Return the reserved count to the test ledger.
        fn drop(&mut self) {
            self.0.set(self.0.get() - self.1);
        }
    }
    /// Observable connection policy with configurable head rewrites.
    #[derive(Default)]
    struct Policy {
        session: usize,
        finished: usize,
        transient: Option<Charge>,
        on_idle: Option<Box<dyn FnOnce()>>,
        rewrite: Option<Rewrite>,
        hook_calls: Option<Rc<Cell<usize>>>,
    }
    /// Framing-sensitive and ordinary mutations exercised by policy tests.
    #[derive(Clone, Copy)]
    enum Rewrite {
        Length(u64),
        Status(u16),
        Head,
        Kind,
        Close,
        Transfer,
        Ordinary,
    }
    impl Policy {
        /// Apply the selected mutation and count the hook invocation.
        fn rewrite(&self, mut head: MessageHead) -> MessageHead {
            if let Some(calls) = &self.hook_calls {
                calls.set(calls.get() + 1);
            }
            match self.rewrite {
                Some(Rewrite::Length(n)) => head.headers[0].value = n.to_string().into_bytes(),
                Some(Rewrite::Status(status)) => head.start = StartLine::Response { status },
                Some(Rewrite::Head) => {
                    head.start = StartLine::Request {
                        method: "HEAD".into(),
                        target: "/".into(),
                    }
                }
                Some(Rewrite::Kind) => head.start = StartLine::Response { status: 200 },
                Some(Rewrite::Close) => head.headers.push(crate::Header {
                    name: "Connection".into(),
                    value: b"close".to_vec(),
                }),
                Some(Rewrite::Transfer) => head.headers.push(crate::Header {
                    name: "Transfer-Encoding".into(),
                    value: b"chunked".to_vec(),
                }),
                Some(Rewrite::Ordinary) => head.headers.push(crate::Header {
                    name: "X-Checked".into(),
                    value: b"yes".to_vec(),
                }),
                None => (),
            }
            head
        }
    }
    impl State<Failure> for Policy {
        /// Count admission and apply its test mutation.
        fn admit(&mut self, head: MessageHead) -> std::result::Result<MessageHead, Failure> {
            self.session += 1;
            Ok(self.rewrite(head))
        }
        /// Count signing and apply its test mutation.
        fn sign(&mut self, head: MessageHead) -> std::result::Result<MessageHead, Failure> {
            self.session += 1;
            Ok(self.rewrite(head))
        }
        /// Record a completed exchange.
        fn finished(&mut self) {
            self.finished += 1;
        }
        /// Run the reentrancy probe and release transient admission.
        fn idle(mut self) -> Self {
            if let Some(callback) = self.on_idle.take() {
                callback();
            }
            self.transient.take();
            self
        }
    }
    /// Single ordered endpoint for pool tests.
    #[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
    struct Key;
    impl Endpoint<Failure> for Key {
        /// Supply a loopback address without resolving names.
        fn address(&self) -> std::result::Result<SocketAddress, Failure> {
            Ok(SocketAddress::Inet("127.0.0.1:9".parse().unwrap()))
        }
    }
    /// Direct reactor owner satisfying the context's Deref contract without another Rc.
    struct TestReactor(Reactor<TestScope, ()>);
    impl Deref for TestReactor {
        type Target = Reactor<TestScope, ()>;

        /// Borrow the fixture's directly owned reactor.
        fn deref(&self) -> &Self::Target {
            &self.0
        }
    }

    /// Caller hooks exposing retained bytes, connection slots, and rejection switches.
    struct Hooks {
        used: Rc<Cell<usize>>,
        slots: Rc<Cell<usize>>,
        stopped: Cell<bool>,
        reject_charge: Cell<bool>,
    }
    impl Context for Hooks {
        type Error = Failure;
        type Scope = TestScope;
        type Budget = ();
        type Reactor = TestReactor;
        type Charge = Charge;
        type Slot = Charge;
        type Opaque = ();
        type State = Policy;
        type Endpoint = Key;
        /// Reserve counted bytes unless shutdown or overload is injected.
        fn charge(&self, n: usize) -> Result<Self, Charge> {
            if self.stopped.get() {
                return Err(uring_runtime::Error::Unavailable.into());
            }
            if self.reject_charge.get() {
                return Err(uring_runtime::Error::Overloaded.into());
            }
            self.used.set(self.used.get() + n);
            Ok(Charge(self.used.clone(), n))
        }
        /// Reserve one counted connection slot.
        fn outbound_slot(&self) -> Result<Self, Charge> {
            self.slots.set(self.slots.get() + 1);
            Ok(Charge(self.slots.clone(), 1))
        }
        /// Report the injected shutdown state.
        fn stopped(&self) -> bool {
            self.stopped.get()
        }
    }
    /// Create empty admission ledgers with all operations enabled.
    fn hooks() -> Rc<Hooks> {
        Rc::new(Hooks {
            used: Rc::default(),
            slots: Rc::default(),
            stopped: Cell::new(false),
            reject_charge: Cell::new(false),
        })
    }
    /// Create the caller-owned reactor wrapper used by test contexts.
    fn reactor() -> Rc<TestReactor> {
        Rc::new(TestReactor(Reactor::new(16, ())))
    }
    /// Build a response with one explicit content length.
    fn head(status: u16, length: u64) -> MessageHead {
        MessageHead {
            start: StartLine::Response { status },
            headers: vec![crate::Header {
                name: "Content-Length".into(),
                value: length.to_string().into_bytes(),
            }],
        }
    }
    /// Stable storage that records buffer access and can inject an access failure.
    struct ObservedBuffer {
        bytes: OwnedBuffer<Hooks>,
        accesses: Rc<Cell<usize>>,
        reject: bool,
    }
    // SAFETY: the admitted backing owner keeps its fixed allocation and charge alive.
    unsafe impl IoBuffer for ObservedBuffer {
        type Error = Failure;

        /// Record immutable access before returning bytes or the injected error.
        fn bytes(&self) -> std::result::Result<&[u8], Failure> {
            self.accesses.set(self.accesses.get() + 1);
            if self.reject {
                return Err(uring_runtime::Error::Overloaded.into());
            }
            self.bytes.bytes()
        }

        /// Expose the fixed mutable storage if a receive reaches it.
        fn bytes_mut(&mut self) -> std::result::Result<&mut [u8], Failure> {
            self.bytes.bytes_mut()
        }
    }

    /// Body wrappers defer buffer access and preserve access-error precedence.
    #[test]
    fn body_wrappers_keep_buffer_access_lazy_and_preserve_validation_order() {
        for mode in 0..5 {
            for reject in [false, true] {
                let hooks = hooks();
                let reactor = reactor();
                let io =
                    HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
                let accesses = Rc::new(Cell::new(0));
                let (connection, _peer) = lease(&hooks, Policy::default());
                let buffer = ObservedBuffer {
                    bytes: OwnedBuffer::new(hooks.as_ref(), 1).unwrap(),
                    accesses: accesses.clone(),
                    reject,
                };
                let mut operation = match mode {
                    0 => io.read_body(connection, buffer, &TestScope),
                    1 => io.write_body(connection, buffer, &TestScope),
                    2 => io.read_body_exact(connection, buffer, &TestScope),
                    3 => io.read_body_range(connection, buffer, 0..1, &TestScope),
                    _ => io.write_body_range(connection, buffer, 0..1, &TestScope),
                };
                assert_eq!(accesses.get(), 0, "construction must not access storage");
                assert_eq!(reactor.in_flight(), 0);
                let expected = if reject {
                    Failure::Runtime(uring_runtime::Error::Overloaded)
                } else {
                    Failure::Http(Error::Malformed)
                };
                assert!(matches!(
                    operation.as_mut().poll(&mut std::task::Context::from_waker(Waker::noop())),
                    Poll::Ready(Err(error)) if error == expected
                ));
                assert_eq!(accesses.get(), if reject || mode >= 3 { 1 } else { 2 });
                assert_eq!(reactor.in_flight(), 0);
                drop(operation);
                assert_eq!(hooks.slots.get(), 0);
            }
        }
    }

    /// Borrowed writes do not reserve or access copied storage until first poll.
    #[test]
    fn borrowed_body_write_admission_remains_lazy() {
        let hooks = hooks();
        let reactor = reactor();
        let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
        let (connection, _peer) = lease(&hooks, Policy::default());
        let operation = io.write_body_bytes(connection, b"abc", &TestScope);
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(reactor.in_flight(), 0);
        drop(operation);
        assert_eq!(hooks.slots.get(), 0);

        let (connection, _peer) = lease(&hooks, Policy::default());
        hooks.reject_charge.set(true);
        let mut operation = io.write_body_bytes(connection, b"abc", &TestScope);
        assert!(matches!(
            operation
                .as_mut()
                .poll(&mut std::task::Context::from_waker(Waker::noop())),
            Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Overloaded)))
        ));
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(reactor.in_flight(), 0);
        drop(operation);
        assert_eq!(hooks.slots.get(), 0);
    }

    /// Effective body bounds do not constrain bodyless representation lengths.
    #[test]
    fn framing_bounds_and_head_representation_are_independent() {
        assert_eq!(framing(&head(200, 9), false, 8), Err(Error::Malformed));
        assert_eq!(framing(&head(200, u64::MAX), true, 8), Ok(0));
        assert_eq!(framing(&head(204, 1), false, 8), Err(Error::Malformed));
        assert_eq!(framing(&head(304, u64::MAX), false, 8), Ok(0));
        assert_eq!(framing(&head(101, 0), false, 8), Err(Error::Malformed));
        assert_eq!(framing(&head(200, 8), false, 8), Ok(8));
    }
    /// Cached storage is cleared, charged, and unavailable after caller shutdown.
    #[test]
    fn buffer_charge_zeroization_and_stop_use_caller_hooks() {
        let hooks = hooks();
        let io = HttpIo::<Hooks>::new(reactor(), Codec::new(128), hooks.clone(), 8, 16);
        let mut buffer = io.buffer(32).unwrap();
        buffer.bytes_mut().unwrap().fill(99);
        drop(buffer);
        assert_eq!(hooks.used.get(), 32);
        let buffer = io.buffer(32).unwrap();
        assert_eq!(buffer.bytes().unwrap(), &[0; 32]);
        drop(buffer);
        hooks.stopped.set(true);
        assert!(matches!(
            io.buffer(32),
            Err(Failure::Runtime(uring_runtime::Error::Unavailable))
        ));
        io.reclaim_buffer();
        assert_eq!(hooks.used.get(), 0);
    }
    /// Underflow and removed excess bytes cannot make a connection reusable.
    #[test]
    fn checked_consumption_and_taken_excess_cannot_enable_reuse() {
        let hooks = hooks();
        let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let mut lease = ConnectionLease::<Hooks>::from_reserved(
            fd.into(),
            hooks.outbound_slot().unwrap(),
            Policy::default(),
        )
        .unwrap();
        lease.rx_remaining = Some(1);
        lease.tx_remaining = Some(0);
        assert_eq!(
            lease.consume_received(2),
            Err(Failure::Http(Error::Malformed))
        );
        assert_eq!(lease.receive_remaining(), Some(1));
        assert_eq!(lease.consume_sent(1), Err(Failure::Http(Error::Malformed)));
        let buffer = OwnedBuffer::copy_from(hooks.as_ref(), b"ab").unwrap();
        lease.restore_read_ahead(buffer, 0..2).unwrap();
        let ahead = lease.take_read_ahead();
        assert!(lease.closing());
        lease.consume_received(1).unwrap();
        lease.finish_exchange().unwrap();
        assert!(!lease.is_reusable());
        assert_eq!(lease.state().finished, 1);
        drop(ahead);
        drop(lease);
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(hooks.slots.get(), 0);
    }

    /// Restored read-ahead revokes reuse even after an exchange was finished.
    #[test]
    fn restoring_tail_invalidates_an_already_finished_lease() {
        let hooks = hooks();
        let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let mut lease = ConnectionLease::<Hooks>::from_reserved(
            fd.into(),
            hooks.outbound_slot().unwrap(),
            Policy::default(),
        )
        .unwrap();
        lease.rx_remaining = Some(0);
        lease.tx_remaining = Some(0);
        lease.finish_exchange().unwrap();
        assert!(lease.is_reusable());
        let buffer = OwnedBuffer::copy_from(hooks.as_ref(), b"next").unwrap();
        lease.restore_read_ahead(buffer, 0..4).unwrap();
        assert!(!lease.is_reusable());
        assert_eq!(lease.next_round(), Err(Failure::Http(Error::Malformed)));
    }
    /// Pool return permits policy reentrancy and retains only reusable resources.
    #[test]
    fn pool_return_transforms_policy_outside_borrow_and_releases_resources() {
        let hooks = hooks();
        let pool = HttpPool::<Hooks>::new(reactor(), hooks.clone(), PoolConfig::default());
        let (mut lease, _) = pool.prepare_connection(&Key).unwrap();
        assert_eq!(hooks.slots.get(), 1);
        assert!(matches!(
            pool.prepare_connection(&Key),
            Err(Failure::Runtime(uring_runtime::Error::Overloaded))
        ));
        // Replace an unconnected test descriptor with a healthy accepted pair.
        let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
        fd.set_nonblocking(true).unwrap();
        lease.fd = Rc::new(fd.into());
        lease.state_mut().admit(head(200, 0)).unwrap();
        lease.state_mut().sign(head(200, 0)).unwrap();
        lease.state_mut().transient = Some(hooks.charge(7).unwrap());
        let state = pool.state.clone();
        lease.state_mut().on_idle = Some(Box::new(move || {
            assert!(state.try_borrow_mut().is_ok());
        }));
        lease.rx_remaining = Some(0);
        lease.tx_remaining = Some(0);
        lease.finish_exchange().unwrap();
        drop(lease);
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(hooks.slots.get(), 1);
        let (lease, address) = pool.prepare_connection(&Key).unwrap();
        assert!(address.is_none());
        assert_eq!(lease.state().session, 2);
        assert_eq!(lease.state().finished, 1);
        drop(lease);
        assert_eq!(hooks.slots.get(), 0);
        pool.close();
        assert!(matches!(
            pool.prepare_connection(&Key),
            Err(Failure::Runtime(uring_runtime::Error::Unavailable))
        ));
    }

    /// Drive a future and reactor with a bounded deadline.
    fn drive<T>(
        reactor: &Reactor<TestScope, ()>,
        future: impl std::future::Future<Output = T>,
    ) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = std::task::Context::from_waker(Waker::noop());
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
            assert!(Instant::now() < deadline, "bounded test driver");
            reactor.poll_budgeted(32).unwrap();
            std::thread::yield_now();
        }
    }
    /// Pair an admitted nonblocking connection with its test peer.
    fn lease(
        hooks: &Hooks,
        policy: Policy,
    ) -> (ConnectionLease<Hooks>, std::os::unix::net::UnixStream) {
        let (fd, peer) = std::os::unix::net::UnixStream::pair().unwrap();
        (
            ConnectionLease::from_reserved(fd.into(), hooks.outbound_slot().unwrap(), policy)
                .unwrap(),
            peer,
        )
    }

    /// Signing controls final framing but cannot bypass prevalidation or limits.
    #[test]
    fn sign_uses_final_framing_and_rejects_invalid_changes_before_send() {
        use std::io::Read;
        for (rewrite, expected) in [
            (Rewrite::Length(3), Some(3)),
            (Rewrite::Status(304), Some(0)),
            (Rewrite::Length(9), None),
            (Rewrite::Status(204), None),
            (Rewrite::Status(101), None),
            (Rewrite::Transfer, None),
        ] {
            let hooks = hooks();
            let reactor = reactor();
            let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(256), hooks.clone(), 8, 8);
            let (connection, mut peer) = lease(
                &hooks,
                Policy {
                    rewrite: Some(rewrite),
                    ..Policy::default()
                },
            );
            let result = drive(&reactor, io.send_head(connection, head(200, 1), &TestScope));
            if let Some(expected) = expected {
                let done = result.unwrap();
                assert_eq!(done.connection.send_remaining(), Some(expected));
                drop(done);
                let mut wire = Vec::new();
                peer.read_to_end(&mut wire).unwrap();
                let decoded = Codec::<()>::new(256).decode_head(&wire).unwrap().unwrap().0;
                assert_eq!(framing(&decoded, false, 8).unwrap(), expected);
            } else {
                assert!(matches!(result, Err(Failure::Http(Error::Malformed))));
                let mut wire = Vec::new();
                peer.read_to_end(&mut wire).unwrap();
                assert!(wire.is_empty());
            }
        }
        let hooks = hooks();
        let io = HttpIo::<Hooks>::new(reactor(), Codec::new(256), hooks.clone(), 8, 8);
        let calls = Rc::new(Cell::new(0));
        let (connection, _peer) = lease(
            &hooks,
            Policy {
                rewrite: Some(Rewrite::Length(0)),
                hook_calls: Some(calls.clone()),
                ..Policy::default()
            },
        );
        hooks.reject_charge.set(true);
        let mut future = io.send_head(connection, head(200, 9), &TestScope);
        assert!(matches!(
            future
                .as_mut()
                .poll(&mut std::task::Context::from_waker(Waker::noop())),
            Poll::Ready(Err(Failure::Http(Error::Malformed)))
        ));
        assert_eq!(
            calls.get(),
            0,
            "prevalidation precedes charging and signing"
        );
    }

    /// Admission may change ordinary fields but never the wire framing snapshot.
    #[test]
    fn admission_cannot_rewrite_wire_framing_even_for_bodyless_heads() {
        use std::io::Write;
        for rewrite in [
            Rewrite::Length(3),
            Rewrite::Status(304),
            Rewrite::Head,
            Rewrite::Kind,
            Rewrite::Close,
            Rewrite::Transfer,
            Rewrite::Ordinary,
        ] {
            let hooks = hooks();
            let reactor = reactor();
            let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(256), hooks.clone(), 8, 8);
            let (mut connection, mut peer) = lease(
                &hooks,
                Policy {
                    rewrite: Some(rewrite),
                    ..Policy::default()
                },
            );
            connection.request_is_head = true;
            let wire: &[u8] = if matches!(rewrite, Rewrite::Head | Rewrite::Kind) {
                b"GET / HTTP/1.1\r\nContent-Length: 0\r\n\r\n"
            } else {
                b"HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\n"
            };
            peer.write_all(wire).unwrap();
            let result = drive(&reactor, io.receive_head(connection, &TestScope));
            if matches!(rewrite, Rewrite::Ordinary) {
                let done = result.unwrap();
                assert_eq!(
                    done.value.unique("x-checked").unwrap(),
                    Some(b"yes".as_slice())
                );
                assert_eq!(done.connection.receive_remaining(), Some(0));
                drop(done);
            } else {
                assert!(matches!(result, Err(Failure::Http(Error::Malformed))));
            }
            io.reclaim_buffer();
            assert_eq!(hooks.slots.get(), 0);
            assert_eq!(hooks.used.get(), 0);
        }
    }

    /// Default attachment replaces both fresh and reused connection policy.
    #[test]
    fn default_attach_installs_checkout_state_on_fresh_and_reused_connections() {
        let hooks = hooks();
        let pool = HttpPool::<Hooks>::new(reactor(), hooks.clone(), PoolConfig::default());
        let (mut connection, _) = pool.prepare_connection(&Key).unwrap();
        connection.state_mut().attach(Policy {
            session: 42,
            transient: Some(hooks.charge(7).unwrap()),
            ..Policy::default()
        });
        assert_eq!(connection.state().session, 42);
        assert_eq!(hooks.used.get(), 7);
        let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
        fd.set_nonblocking(true).unwrap();
        connection.fd = Rc::new(fd.into());
        connection.rx_remaining = Some(0);
        connection.tx_remaining = Some(0);
        connection.finish_exchange().unwrap();
        drop(connection);
        assert_eq!(hooks.used.get(), 0);
        let mut checkout = pool.checkout_with_state(
            &Key,
            Policy {
                session: 99,
                transient: Some(hooks.charge(11).unwrap()),
                ..Policy::default()
            },
            &TestScope,
        );
        let Poll::Ready(Ok(connection)) = checkout
            .as_mut()
            .poll(&mut std::task::Context::from_waker(Waker::noop()))
        else {
            panic!("healthy idle checkout must complete immediately");
        };
        assert_eq!(connection.state().session, 99);
        assert_eq!(hooks.used.get(), 11);
        drop(connection);
        drop(checkout);
        assert_eq!(hooks.used.get(), 0);
    }

    /// Connect observation is admitted before submission and retained until drain.
    #[test]
    fn connect_observation_charge_rejects_before_submission_and_survives_abandonment() {
        let hooks = hooks();
        let reactor = reactor();
        reactor.init().unwrap();
        let pool = HttpPool::<Hooks>::new(reactor.clone(), hooks.clone(), PoolConfig::default());
        hooks.reject_charge.set(true);
        let mut checkout = pool.checkout(&Key, &TestScope);
        let mut cx = std::task::Context::from_waker(Waker::noop());
        assert!(matches!(
            checkout.as_mut().poll(&mut cx),
            Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Overloaded)))
        ));
        drop(checkout);
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(hooks.slots.get(), 0);
        assert_eq!(hooks.used.get(), 0);
        hooks.reject_charge.set(false);
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = SocketAddress::Inet(listener.local_addr().unwrap());
        let (connection, _) = pool.prepare_connection(&Key).unwrap();
        let mut connect = Box::pin(pool.connect(connection, Some(address), &TestScope));
        assert!(connect.as_mut().poll(&mut cx).is_pending());
        assert_eq!(hooks.used.get(), ConnectOwner::<Hooks>::allocation());
        assert_eq!(reactor.in_flight(), 1);
        drop(connect);
        drop(pool);
        assert_eq!(hooks.used.get(), ConnectOwner::<Hooks>::allocation());
        assert_eq!(hooks.slots.get(), 1);
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(hooks.slots.get(), 0);
    }

    /// A successful connect releases only observation admission, not its live slot.
    #[test]
    fn completed_connect_releases_observation_charge_but_retains_connection_slot() {
        let hooks = hooks();
        let reactor = reactor();
        let pool = HttpPool::<Hooks>::new(reactor.clone(), hooks.clone(), PoolConfig::default());
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let (connection, _) = pool.prepare_connection(&Key).unwrap();
        let connection = drive(
            &reactor,
            pool.connect(
                connection,
                Some(SocketAddress::Inet(listener.local_addr().unwrap())),
                &TestScope,
            ),
        )
        .unwrap();
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(hooks.slots.get(), 1);
        drop(connection);
        assert_eq!(hooks.slots.get(), 0);
    }

    /// Exact reads combine read-ahead and socket bytes without overconsuming framing.
    #[test]
    fn exact_body_read_combines_ahead_and_socket_without_consuming_next_body_bytes() {
        use std::io::Write;
        let hooks = hooks();
        let reactor = reactor();
        let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
        let (mut connection, mut peer) = lease(&hooks, Policy::default());
        connection.rx_remaining = Some(6);
        connection
            .restore_read_ahead(OwnedBuffer::copy_from(hooks.as_ref(), b"ab").unwrap(), 0..2)
            .unwrap();
        peer.write_all(b"cdef").unwrap();
        let first = drive(
            &reactor,
            io.read_body_exact(connection, io.buffer(4).unwrap(), &TestScope),
        )
        .unwrap();
        assert_eq!(first.bytes, 4);
        assert_eq!(first.buffer.bytes().unwrap(), b"abcd");
        assert_eq!(first.lease.receive_remaining(), Some(2));
        let last = drive(&reactor, io.collect_body(first.lease, 2, &TestScope)).unwrap();
        assert_eq!(last.buffer.bytes().unwrap(), b"ef");
        assert_eq!(last.lease.receive_remaining(), Some(0));
    }

    /// Exact reads distinguish short bodies and EOF from missing framing.
    #[test]
    fn exact_body_read_rejects_short_framing_eof_and_missing_framing() {
        use std::io::Write;
        for (remaining, expected) in [
            (Some(1), Failure::Runtime(uring_runtime::Error::Io)),
            (Some(2), Failure::Runtime(uring_runtime::Error::Io)),
            (None, Failure::Http(Error::Malformed)),
        ] {
            let hooks = hooks();
            let reactor = reactor();
            let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
            let (mut connection, mut peer) = lease(&hooks, Policy::default());
            connection.rx_remaining = remaining;
            peer.write_all(b"a").unwrap();
            drop(peer);
            let result = drive(
                &reactor,
                io.read_body_exact(connection, io.buffer(2).unwrap(), &TestScope),
            );
            assert!(matches!(result, Err(error) if error == expected));
            io.reclaim_buffer();
            assert_eq!(hooks.used.get(), 0);
            assert_eq!(hooks.slots.get(), 0);
        }
    }

    /// Empty exact reads complete without framing checks or runtime submissions.
    #[test]
    fn empty_exact_read_preserves_no_io_behavior() {
        let hooks = hooks();
        let reactor = reactor();
        let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
        let (connection, _peer) = lease(&hooks, Policy::default());
        let done = drive(
            &reactor,
            io.read_body_exact(connection, io.buffer(0).unwrap(), &TestScope),
        )
        .unwrap();
        assert_eq!(done.bytes, 0);
        assert_eq!(done.lease.receive_remaining(), None);
        assert_eq!(reactor.in_flight(), 0);
    }

    /// Abandoned exact reads keep bytes and connection admission fenced until drain.
    #[test]
    fn exact_read_abandonment_retains_buffer_and_slot_until_drain() {
        let hooks = hooks();
        let reactor = reactor();
        reactor.init().unwrap();
        let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
        let (mut connection, _peer) = lease(&hooks, Policy::default());
        connection.rx_remaining = Some(4);
        let mut operation = io.read_body_exact(connection, io.buffer(4).unwrap(), &TestScope);
        assert!(
            operation
                .as_mut()
                .poll(&mut std::task::Context::from_waker(Waker::noop()))
                .is_pending()
        );
        drop(operation);
        assert_eq!(hooks.used.get(), 4);
        assert_eq!(hooks.slots.get(), 1);
        drive(&reactor, reactor.drain()).unwrap();
        io.reclaim_buffer();
        assert_eq!(hooks.used.get(), 0);
        assert_eq!(hooks.slots.get(), 0);
    }

    /// Borrowed body writes copy exactly and reject oversize framing before sending.
    #[test]
    fn body_bytes_write_is_exact_and_enforces_framing() {
        use std::io::Read;
        for remaining in [0, 2, 3] {
            let hooks = hooks();
            let reactor = reactor();
            let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(128), hooks.clone(), 8, 8);
            let (mut connection, mut peer) = lease(&hooks, Policy::default());
            connection.tx_remaining = Some(remaining);
            let result = drive(
                &reactor,
                io.write_body_bytes(connection, b"abc", &TestScope),
            );
            if remaining == 3 {
                let done = result.unwrap();
                assert_eq!(done.bytes, 3);
                assert_eq!(done.buffer.bytes().unwrap(), b"abc");
                assert_eq!(done.lease.send_remaining(), Some(0));
                drop(done);
            } else {
                assert!(matches!(result, Err(Failure::Http(Error::Malformed))));
            }
            let mut wire = Vec::new();
            peer.read_to_end(&mut wire).unwrap();
            assert_eq!(
                wire.as_slice(),
                if remaining == 3 {
                    b"abc".as_slice()
                } else {
                    b""
                }
            );
        }
    }
}
