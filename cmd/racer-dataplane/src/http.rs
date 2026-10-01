//! Strict HTTP/1.1 framing and reactor-driven TCP/Unix socket operations.
//!
//! Construct `HttpIo::with_admission` for operational use. The worker must poll
//! `Reactor::poll_budgeted` and arrange reactor wakeups; these futures do not own
//! an executor. `read_body`/`read_body_range` return partial reads. `write_body`
//! and `write_body_range` send their complete initialized range. Call
//! `ConnectionLease::finish_exchange` only after both directions are consumed;
//! dropping an unfinished lease closes it instead of recycling it.
//!
//! Heads retain duplicate fields for endpoint validation, while rejecting
//! ambiguous framing. The protocol supports fixed-length bodies, no transfer
//! coding, upgrades, informational responses, or pipelined pool reuse. Local
//! HEAD representation lengths are not subject to the body-allocation cap.
//!
//! Header order, spelling, duplicates, and opaque value bytes are preserved. The
//! one optional SP immediately following a colon is syntax, not value data; any
//! additional whitespace is retained except on Authorization/Racer-Metadata,
//! which require exactly one separator and reject edge whitespace. Encoding
//! always emits that separator SP.
//! Raw heads deliberately do not implement Debug (they can contain credentials).
pub mod connection {
    //! Exclusive HTTP connections and bounded TCP/Unix reuse. Unfinished exchanges close.
    use super::{MessageHead, StartLine};
    use crate::runtime::reactor::Descriptor;
    use crate::{
        error::{Error, Operation, Result},
        memory::pipe::{MAX_PIPE_BYTES, PipeLease},
        model::ResourceClass,
        runtime::{
            admission::{Admission, ConnectionReservation, Reservation},
            deadline::RequestScope,
            reactor::{Completion, IoBuffer, Reactor, SendBuffer},
        },
    };
    use std::{
        cell::RefCell,
        collections::{BTreeMap, VecDeque},
        future::poll_fn,
        net::SocketAddr,
        ops::Range,
        path::PathBuf,
        rc::{Rc, Weak},
        task::{Poll, Waker},
        time::{Duration, Instant},
    };
    use zeroize::{Zeroize, Zeroizing};

    #[cfg(test)]
    pub(crate) mod io_tests;

    /// Bounded, address-stable staging storage. Constructor reserves quota before
    /// allocation; ownership includes that quota through reactor completion.
    /// Vec avoids Box move retagging; its private backing is never resized after construction.
    pub struct OwnedBuffer {
        bytes: Vec<u8>,
        reservation: Option<Reservation>,
        pool: Weak<RefCell<Option<OwnedBuffer>>>,
    }
    impl Drop for OwnedBuffer {
        fn drop(&mut self) {
            // Wipe without clearing the length needed by the idle-buffer pool.
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
    impl OwnedBuffer {
        pub fn new(admission: &Admission, length: usize) -> Result<Self> {
            let reservation =
                admission.reserve(None, ResourceClass::RequestContext, length.max(1))?;
            let mut bytes = Vec::new();
            bytes
                .try_reserve_exact(length)
                .map_err(|_| Error::Overloaded)?;
            bytes.resize(length, 0);
            // Normalize capacity to the charged length before deriving any I/O pointer.
            Ok(Self {
                bytes: bytes.into_boxed_slice().into_vec(),
                reservation: Some(reservation),
                pool: Weak::new(),
            })
        }
        pub fn copy_from(admission: &Admission, bytes: &[u8]) -> Result<Self> {
            let mut buffer = Self::new(admission, bytes.len())?;
            buffer.bytes.copy_from_slice(bytes);
            Ok(buffer)
        }
    }
    impl crate::runtime::reactor::sealed::Sealed for OwnedBuffer {}
    impl IoBuffer for OwnedBuffer {
        fn bytes(&self) -> Result<&[u8]> {
            Ok(&self.bytes)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8]> {
            Ok(&mut self.bytes)
        }
    }
    /// Owns the full backing allocation while exposing a fixed subrange to one SQE.
    /// Construct a new view after completion instead of mutating an in-flight view.
    pub struct BufferRange<B: IoBuffer> {
        buffer: B,
        range: Range<usize>,
    }
    impl<B: IoBuffer> BufferRange<B> {
        pub fn new(buffer: B, range: Range<usize>) -> Result<Self> {
            if range.start > range.end || range.end > buffer.bytes()?.len() {
                return Err(Error::InvalidRequest);
            }
            Ok(Self { buffer, range })
        }
        pub fn into_inner(self) -> B {
            self.buffer
        }
    }
    impl<B: IoBuffer> crate::runtime::reactor::sealed::Sealed for BufferRange<B> {}
    impl<B: IoBuffer> IoBuffer for BufferRange<B> {
        fn bytes(&self) -> Result<&[u8]> {
            Ok(&self.buffer.bytes()?[self.range.clone()])
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8]> {
            Ok(&mut self.buffer.bytes_mut()?[self.range.clone()])
        }
    }
    /// Socket operations retain buffers through partial I/O and cancellation completion.
    /// Connections transfer by value, so abandonment cannot recycle a kernel-owned socket.
    pub struct HttpIo {
        reactor: Rc<Reactor>,
        codec: super::Codec,
        send_body_limit: u64,
        admission: Rc<Admission>,
        idle_buffer: Rc<RefCell<Option<OwnedBuffer>>>,
    }
    /// Immutable subrange owner; cannot be submitted to a receive operation.
    struct SendRange<B: SendBuffer> {
        buffer: B,
        range: Range<usize>,
    }
    impl<B: SendBuffer> crate::runtime::reactor::sealed::Sealed for SendRange<B> {}
    impl<B: SendBuffer> SendBuffer for SendRange<B> {
        fn send_bytes(&self) -> Result<&[u8]> {
            Ok(&self.buffer.send_bytes()?[self.range.clone()])
        }
    }
    /// A completed head operation, including the still-exclusively-owned connection.
    ///
    /// Head and body operations transfer ownership in both directions:
    /// ```no_run
    /// use racer_dataplane::{error::Result,
    ///     http::connection::{HttpIo, ConnectionLease}, memory::pool::PlaintextBuffer,
    ///     runtime::{deadline::RequestScope, reactor::Completion}};
    /// async fn exchange(io: &HttpIo, connection: ConnectionLease,
    ///     buffer: PlaintextBuffer, scope: &RequestScope)
    ///     -> Result<Completion<PlaintextBuffer, ConnectionLease>> {
    ///     let head = io.receive_head(connection, scope).await?;
    ///     let sent = io.send_head(head.connection, head.value, scope).await?;
    ///     let body = io.read_body(sent.connection, buffer, scope).await?;
    ///     io.write_body(body.lease, body.buffer, scope).await
    /// }
    /// ```
    pub struct HeadCompletion<T> {
        pub connection: ConnectionLease,
        pub value: T,
        // Retained alongside the decoded value, including its field descriptors.
        _decoded: Option<Reservation>,
    }
    impl HttpIo {
        /// Watch hangup without consuming request bytes. The pending poll retains both
        /// descriptor and connection admission through its cancellation CQE.
        pub(crate) fn disconnected<'a>(
            &'a self,
            connection: &ConnectionLease,
            scope: &'a RequestScope,
        ) -> Operation<'a, u32> {
            self.reactor.readiness_with_lease(
                connection.socket(),
                libc::POLLHUP as u32,
                connection.reservation.clone(),
                scope,
            )
        }
        pub(crate) fn reactor(&self) -> &Rc<Reactor> {
            &self.reactor
        }
        /// Every head and body staging allocation uses this worker's admission owner.
        pub fn with_admission(
            reactor: Rc<Reactor>,
            codec: super::Codec,
            admission: Rc<Admission>,
        ) -> Self {
            Self {
                reactor,
                send_body_limit: codec.body_limit(),
                codec,
                admission,
                idle_buffer: Rc::default(),
            }
        }
        /// Local client responses stream an entire object range rather than one page.
        /// Only sending permits SDK-sized ranges; receiving retains the page cap.
        /// These are framing limits; body storage remains page-window admitted.
        pub fn for_clients(reactor: Rc<Reactor>, admission: Rc<Admission>) -> Self {
            let mut io = Self::with_admission(
                reactor,
                super::Codec::new(
                    admission
                        .limits()
                        .header_bytes
                        .get()
                        .min(super::MAX_HEAD_BYTES),
                    crate::model::PAGE_BYTES + 16,
                ),
                admission,
            );
            io.send_body_limit = i64::MAX as u64;
            io
        }
        pub fn buffer(&self, length: usize) -> Result<OwnedBuffer> {
            let admission = &self.admission;
            if admission.is_stopped() {
                return Err(Error::Unavailable);
            }
            let idle = self.idle_buffer.borrow_mut().take();
            let mut buffer = match idle {
                Some(buffer) if buffer.bytes.len() == length => buffer,
                other => {
                    drop(other);
                    OwnedBuffer::new(&self.admission, length)?
                }
            };
            // Retain at most one ordinary head-sized allocation, never a maximum
            // multi-hop envelope that could monopolize shared context admission.
            if length <= 65536 {
                buffer.pool = Rc::downgrade(&self.idle_buffer);
            }
            Ok(buffer)
        }
        #[cfg(test)]
        pub fn retained_buffer_bytes(&self) -> usize {
            self.idle_buffer
                .borrow()
                .as_ref()
                .and_then(|b| b.reservation.as_ref())
                .map_or(0, Reservation::amount)
        }
        pub fn reclaim_buffer(&self) {
            self.idle_buffer.borrow_mut().take();
        }
        /// Share reactor/admission while applying a smaller endpoint-specific head cap.
        pub fn capped(&self, header_limit: usize) -> Self {
            Self {
                reactor: self.reactor.clone(),
                codec: self.codec.limited(header_limit),
                send_body_limit: self.send_body_limit,
                admission: self.admission.clone(),
                idle_buffer: self.idle_buffer.clone(),
            }
        }
        pub fn receive_head<'a>(
            &'a self,
            connection: ConnectionLease,
            scope: &'a RequestScope,
        ) -> Operation<'a, HeadCompletion<MessageHead>> {
            self.receive_head_limited(connection, scope, self.codec.header_limit())
        }
        /// Applies a caller's smaller head cap before receiving/allocating a head.
        /// Client/origin adapters use 32 KiB even if another protocol raises its cap.
        pub fn receive_head_limited<'a>(
            &'a self,
            connection: ConnectionLease,
            scope: &'a RequestScope,
            header_limit: usize,
        ) -> Operation<'a, HeadCompletion<MessageHead>> {
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
        /// Request ingress with recoverable parsing failures. The outer error means
        /// I/O, cancellation, or resource failure and closes the connection. An inner
        /// InvalidRequest/HeaderTooLarge returns a fenced, poisoned connection on
        /// which the server may send an empty 400/431 response, then close it.
        /// All rejected bytes are zeroized and no unread data is retained for reuse.
        pub fn receive_request_head_limited<'a>(
            &'a self,
            connection: ConnectionLease,
            scope: &'a RequestScope,
            header_limit: usize,
        ) -> Operation<'a, HeadCompletion<Result<MessageHead>>> {
            self.receive_head_outcome(connection, scope, header_limit, true)
        }
        fn receive_head_outcome<'a>(
            &'a self,
            mut connection: ConnectionLease,
            scope: &'a RequestScope,
            header_limit: usize,
            request_only: bool,
        ) -> Operation<'a, HeadCompletion<Result<MessageHead>>> {
            Box::pin(async move {
                scope.check()?;
                if connection.rx_remaining.is_some_and(|n| n != 0) {
                    return Err(Error::InvalidRequest);
                }
                connection.begin_io();
                let codec = self.codec.limited(header_limit);
                let ahead_length = connection
                    .read_ahead
                    .as_ref()
                    .map_or(0, |(_, range)| range.len());
                let mut buffer = self.buffer(codec.header_limit().min(4096.max(ahead_length)))?;
                let mut used = 0;
                let mut scanned: usize = 0;
                if let Some((ahead, range)) = connection.read_ahead.take() {
                    if range.len() > buffer.bytes.len() {
                        drop(ahead);
                        drop(buffer);
                        return Ok(rejected_head(connection, Error::HeaderTooLarge));
                    }
                    used = range.len();
                    buffer.bytes[..used].copy_from_slice(&ahead.bytes[range]);
                }
                loop {
                    // Scan each received byte once, including the delimiter overlap.
                    // Large peer heads must not cause quadratic rescans on trickled I/O.
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
                        let size = codec.decoded_allocation(&buffer.bytes[..used])?;
                        Some(
                            self.admission
                                .reserve(None, ResourceClass::RequestContext, size)?,
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
                        Err(error @ (Error::InvalidRequest | Error::HeaderTooLarge)) => {
                            drop(buffer);
                            return Ok(rejected_head(connection, error));
                        }
                        Err(error) => return Err(error),
                    };
                    if let Some((head, end)) = decoded {
                        if request_only && !matches!(head.start, StartLine::Request { .. }) {
                            drop(buffer);
                            return Ok(rejected_head(connection, Error::InvalidRequest));
                        }
                        let length = match self.framing(&head, connection.request_is_head) {
                            Ok(length) => length,
                            Err(error @ (Error::InvalidRequest | Error::HeaderTooLarge)) => {
                                drop(buffer);
                                return Ok(rejected_head(connection, error));
                            }
                            Err(error) => return Err(error),
                        };
                        connection.close |= head.closes_connection()?;
                        if let StartLine::Request { method, .. } = &head.start {
                            connection.request_is_head = method == "HEAD";
                        }
                        connection.rx_remaining = Some(length);
                        // The rest of this allocation may survive as read-ahead.
                        // Credentials in its consumed head must not survive with it.
                        buffer.bytes[..end].zeroize();
                        if used > end {
                            connection.read_ahead = Some((buffer, end..used));
                        }
                        let head = if let Some(session) = connection.session.as_mut() {
                            session.admit(head)?
                        } else {
                            head
                        };
                        return Ok(HeadCompletion {
                            connection,
                            value: Ok(head),
                            _decoded: decoded_charge,
                        });
                    }
                    if used == buffer.bytes.len() {
                        let size = used.saturating_mul(2).min(codec.header_limit());
                        if size <= used {
                            return Ok(rejected_head(connection, Error::HeaderTooLarge));
                        }
                        // The previous receive completed before growth. Both old and
                        // new allocations remain admitted during the copy; drop wipes
                        // the old head before the new allocation is submitted.
                        let mut larger = self.buffer(size)?;
                        larger.bytes[..used].copy_from_slice(&buffer.bytes[..used]);
                        buffer = larger;
                    }
                    let end = buffer.bytes.len();
                    let completion = self
                        .reactor
                        .recv(
                            connection.fd.clone(),
                            BufferRange::new(buffer, used..end)?,
                            connection,
                            scope,
                        )
                        .await?;
                    if completion.bytes == 0 || completion.bytes > end - used {
                        return Err(Error::Io);
                    }
                    used = used.checked_add(completion.bytes).ok_or(Error::Io)?;
                    buffer = completion.buffer.into_inner();
                    connection = completion.lease;
                }
            })
        }
        pub fn send_head<'a>(
            &'a self,
            mut connection: ConnectionLease,
            head: MessageHead,
            scope: &'a RequestScope,
        ) -> Operation<'a, HeadCompletion<()>> {
            Box::pin(async move {
                scope.check()?;
                if connection.tx_remaining.is_some_and(|n| n != 0) {
                    return Err(Error::InvalidRequest);
                }
                connection.begin_io();
                let length = Self::framing_with_limit(
                    &head,
                    connection.request_is_head,
                    self.send_body_limit,
                )?;
                // Admit signing and encoding scratch before either can allocate.
                // Only the encoded head needs to remain staged across socket I/O,
                // not the endpoint's maximum legal envelope size.
                let scratch = self.admission.reserve(
                    None,
                    ResourceClass::RequestContext,
                    self.codec.header_limit().max(1),
                )?;
                let head = if let Some(session) = connection.session.as_mut() {
                    session.sign(head)?
                } else {
                    head
                };
                connection.close |= head.closes_connection()?;
                if let StartLine::Request { method, .. } = &head.start {
                    connection.request_is_head = method == "HEAD";
                }
                let encoded = Zeroizing::new(self.codec.encode_head(&head)?);
                let mut buffer = self.buffer(encoded.len())?;
                buffer.bytes[..encoded.len()].copy_from_slice(&encoded);
                let encoded_length = encoded.len();
                drop(encoded);
                drop(scratch);
                let mut offset = 0;
                while offset < encoded_length {
                    let completion = self
                        .reactor
                        .send(
                            connection.fd.clone(),
                            BufferRange::new(buffer, offset..encoded_length)?,
                            connection,
                            scope,
                        )
                        .await?;
                    if completion.bytes == 0 || completion.bytes > encoded_length - offset {
                        return Err(Error::Io);
                    }
                    offset += completion.bytes;
                    buffer = completion.buffer.into_inner();
                    connection = completion.lease;
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
        /// Stream bounded chunks; endpoint controls expected body length and validation.
        /// Borrowed destinations cannot survive abandonment of a submitted operation:
        /// ```compile_fail
        /// use racer_dataplane::{http::connection::{HttpIo, ConnectionLease},
        ///     runtime::deadline::RequestScope};
        /// fn borrowed(io: &HttpIo, connection: ConnectionLease, scope: &RequestScope) {
        ///     let mut bytes = [0; 16];
        ///     let _future = io.read_body(connection, &mut bytes[..], scope);
        /// }
        /// ```
        pub fn read_body<'a, B: IoBuffer>(
            &'a self,
            connection: ConnectionLease,
            buffer: B,
            scope: &'a RequestScope,
        ) -> Operation<'a, Completion<B, ConnectionLease>> {
            Box::pin(async move {
                let length = buffer.bytes()?.len();
                self.read_body_range(connection, buffer, 0..length, scope)
                    .await
            })
        }
        /// Transfer an admitted mutable or immutable owner, never a borrowed slice.
        /// ```compile_fail
        /// use racer_dataplane::{http::connection::{HttpIo, ConnectionLease},
        ///     runtime::deadline::RequestScope};
        /// fn borrowed(io: &HttpIo, connection: ConnectionLease, scope: &RequestScope) {
        ///     let bytes = [0; 16];
        ///     let _future = io.write_body(connection, &bytes[..], scope);
        /// }
        /// ```
        pub fn write_body<'a, B: SendBuffer>(
            &'a self,
            connection: ConnectionLease,
            buffer: B,
            scope: &'a RequestScope,
        ) -> Operation<'a, Completion<B, ConnectionLease>> {
            Box::pin(async move {
                let length = buffer.send_bytes()?.len();
                self.write_body_range(connection, buffer, 0..length, scope)
                    .await
            })
        }
        /// Reads at most the specified range and remaining fixed-length body. A short
        /// read is returned explicitly; zero means the complete body was consumed.
        pub fn read_body_range<'a, B: IoBuffer>(
            &'a self,
            mut connection: ConnectionLease,
            mut buffer: B,
            range: Range<usize>,
            scope: &'a RequestScope,
        ) -> Operation<'a, Completion<B, ConnectionLease>> {
            Box::pin(async move {
                scope.check()?;
                connection.begin_io();
                if range.start > range.end || range.end > buffer.bytes()?.len() {
                    return Err(Error::InvalidRequest);
                }
                let remaining = connection.rx_remaining.ok_or(Error::InvalidRequest)?;
                let length = range
                    .len()
                    .min(usize::try_from(remaining).unwrap_or(usize::MAX));
                if length == 0 && remaining != 0 {
                    return Err(Error::InvalidRequest);
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
                    connection.rx_remaining = Some(remaining - count as u64);
                    return Ok(Completion {
                        buffer,
                        bytes: count,
                        lease: connection,
                    });
                }
                let completion = self
                    .reactor
                    .recv(
                        connection.fd.clone(),
                        BufferRange::new(buffer, range.start..range.start + length)?,
                        connection,
                        scope,
                    )
                    .await?;
                if completion.bytes == 0 || completion.bytes > length {
                    return Err(Error::Io);
                }
                connection = completion.lease;
                connection.rx_remaining = Some(remaining - completion.bytes as u64);
                Ok(Completion {
                    buffer: completion.buffer.into_inner(),
                    bytes: completion.bytes,
                    lease: connection,
                })
            })
        }
        /// Writes exactly this initialized subrange through partial sends. The full
        /// buffer allocation and connection remain completion-owned at every send.
        pub fn write_body_range<'a, B: SendBuffer>(
            &'a self,
            mut connection: ConnectionLease,
            mut buffer: B,
            range: Range<usize>,
            scope: &'a RequestScope,
        ) -> Operation<'a, Completion<B, ConnectionLease>> {
            Box::pin(async move {
                scope.check()?;
                connection.begin_io();
                if range.start > range.end || range.end > buffer.send_bytes()?.len() {
                    return Err(Error::InvalidRequest);
                }
                let remaining = connection.tx_remaining.ok_or(Error::InvalidRequest)?;
                if range.len() as u64 > remaining {
                    return Err(Error::InvalidRequest);
                }
                let mut offset = range.start;
                while offset < range.end {
                    let completion = self
                        .reactor
                        .send(
                            connection.fd.clone(),
                            SendRange {
                                buffer,
                                range: offset..range.end,
                            },
                            connection,
                            scope,
                        )
                        .await?;
                    if completion.bytes == 0 || completion.bytes > range.end - offset {
                        return Err(Error::Io);
                    }
                    offset += completion.bytes;
                    buffer = completion.buffer.buffer;
                    connection = completion.lease;
                }
                connection.tx_remaining = Some(remaining - range.len() as u64);
                Ok(Completion {
                    buffer,
                    bytes: range.len(),
                    lease: connection,
                })
            })
        }
        /// Send a bodyless request and receive its response head. The caller owns
        /// consumption of the response body and finish_exchange/pool return.
        pub fn exchange_head<'a>(
            &'a self,
            connection: ConnectionLease,
            request: MessageHead,
            scope: &'a RequestScope,
        ) -> Operation<'a, HeadCompletion<MessageHead>> {
            Box::pin(async move {
                if !matches!(request.start, StartLine::Request { .. })
                    || request.content_length()?.unwrap_or(0) != 0
                {
                    return Err(Error::InvalidRequest);
                }
                let sent = self.send_head(connection, request, scope).await?;
                let received = self.receive_head(sent.connection, scope).await?;
                if !matches!(received.value.start, StartLine::Response { .. }) {
                    return Err(Error::InvalidRequest);
                }
                Ok(received)
            })
        }
        /// Collect a bounded response body. Intended for control messages; pages can
        /// stream via read_body_range without an additional full-size allocation.
        pub fn collect_body<'a>(
            &'a self,
            mut connection: ConnectionLease,
            maximum: usize,
            scope: &'a RequestScope,
        ) -> Operation<'a, Completion<OwnedBuffer, ConnectionLease>> {
            Box::pin(async move {
                scope.check()?;
                let length = usize::try_from(connection.rx_remaining.ok_or(Error::InvalidRequest)?)
                    .map_err(|_| Error::InvalidRequest)?;
                if length > maximum {
                    return Err(Error::InvalidRequest);
                }
                let mut buffer = self.buffer(length)?;
                let mut offset = 0;
                while offset < length {
                    let completion = self
                        .read_body_range(connection, buffer, offset..length, scope)
                        .await?;
                    if completion.bytes == 0 {
                        return Err(Error::Io);
                    }
                    offset += completion.bytes;
                    buffer = completion.buffer;
                    connection = completion.lease;
                }
                Ok(Completion {
                    buffer,
                    bytes: length,
                    lease: connection,
                })
            })
        }
        fn framing(&self, head: &MessageHead, request_is_head: bool) -> Result<u64> {
            Self::framing_with_limit(head, request_is_head, self.codec.body_limit())
        }
        fn framing_with_limit(
            head: &MessageHead,
            request_is_head: bool,
            body_limit: u64,
        ) -> Result<u64> {
            let length = head.content_length()?;
            let body = match head.start {
                StartLine::Request { .. } => length.unwrap_or(0),
                StartLine::Response { status: 100..=199 } => return Err(Error::InvalidRequest),
                StartLine::Response { status: 204 } => {
                    if length.is_some_and(|n| n != 0) {
                        return Err(Error::InvalidRequest);
                    }
                    0
                }
                StartLine::Response { status: 304 } => 0,
                StartLine::Response { .. } if request_is_head => 0,
                StartLine::Response { .. } => length.ok_or(Error::InvalidRequest)?,
            };
            if body > body_limit {
                return Err(Error::InvalidRequest);
            }
            Ok(body)
        }
    }
    fn rejected_head(
        mut connection: ConnectionLease,
        error: Error,
    ) -> HeadCompletion<Result<MessageHead>> {
        connection.poison();
        connection.read_ahead = None;
        // Unknown framing must not be declared consumed. This prevents successful
        // finish_exchange even if the caller sends its error response successfully.
        connection.rx_remaining = None;
        connection.request_is_head = false;
        HeadCompletion {
            connection,
            value: Err(error),
            _decoded: None,
        }
    }

    #[cfg(test)]
    mod relay_tests;

    /// A transit owns both connections, pipe, fallback storage, and relay admission.
    struct Transit {
        source: ConnectionLease,
        destination: ConnectionLease,
        pipe: PipeLease,
        fallback: Option<OwnedBuffer>,
        pending: std::ops::Range<usize>,
        copied: bool,
        #[cfg(test)]
        fallback_at: Option<usize>,
    }
    fn unsupported(error: &std::io::Error) -> bool {
        matches!(
            error.raw_os_error(),
            Some(libc::EINVAL | libc::ENOSYS | libc::EOPNOTSUPP)
        )
    }
    impl HttpIo {
        pub(crate) async fn relay_body(
            &self,
            source: ConnectionLease,
            destination: ConnectionLease,
            pipe: Option<PipeLease>,
            scope: &RequestScope,
        ) -> Result<ConnectionLease> {
            if source.rx_remaining != destination.tx_remaining || source.rx_remaining.is_none() {
                return Err(Error::InvalidRequest);
            }
            if source.rx_remaining == Some(0) {
                let mut source = source;
                let mut destination = destination;
                finish(&mut source, &mut destination)?;
                destination.relay_reservation = None;
                return Ok(destination);
            }
            #[cfg(test)]
            let copied = destination.relay_fallback;
            #[cfg(test)]
            let fallback_at = destination.relay_fallback_at;
            #[cfg(not(test))]
            let copied = false;
            let state = Rc::new(RefCell::new(Transit {
                source,
                destination,
                pipe: pipe.ok_or(Error::InvalidRequest)?,
                fallback: None,
                pending: 0..0,
                copied,
                #[cfg(test)]
                fallback_at,
            }));
            loop {
                scope.check()?;
                let wait = self.relay_chunk(&state)?;
                {
                    let s = state.borrow();
                    if s.source.rx_remaining == Some(0) && s.destination.tx_remaining == Some(0) {
                        break;
                    }
                }
                self.wait_relay_progress(state.clone(), wait, scope).await?;
            }
            scope.check()?;
            let mut state = Rc::try_unwrap(state)
                .map_err(|_| Error::Internal)?
                .into_inner();
            // Both finish checks must pass before either connection can be pooled.
            finish(&mut state.source, &mut state.destination)?;
            state.destination.relay_reservation = None;
            Ok(state.destination)
        }

        /// Make bounded synchronous progress, draining each chunk before receiving more.
        fn relay_chunk(&self, state: &RefCell<Transit>) -> Result<Option<(Rc<Descriptor>, i16)>> {
            let mut state = state.borrow_mut();
            let s = &mut *state;
            if s.destination.fd.peer_read_closed() {
                return Err(Error::Io);
            }
            let mut wait = None;
            // Bound synchronous work per poll so a hot link cannot monopolize
            // the worker. Drain each chunk before receiving the next one.
            for _ in 0..32 {
                let remaining = s.source.rx_remaining.ok_or(Error::InvalidRequest)? as usize;
                if s.pending.is_empty() && s.pipe.buffered() == 0 && remaining == 0 {
                    break;
                }
                let writing = !s.pending.is_empty() || s.pipe.buffered() != 0;
                let result = if !s.pending.is_empty() {
                    s.destination
                        .fd
                        .try_send(&s.fallback.as_ref().unwrap().bytes()?[s.pending.clone()])
                } else if s.pipe.buffered() != 0 {
                    #[cfg(test)]
                    if s.fallback_at
                        .is_some_and(|threshold| remaining <= threshold)
                        && !s.copied
                    {
                        s.copied =
                            unsupported(&std::io::Error::from_raw_os_error(libc::EOPNOTSUPP));
                    }
                    if s.copied {
                        if s.fallback.is_none() {
                            s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                        }
                        let n = s
                            .pipe
                            .try_read(s.fallback.as_mut().unwrap().bytes_mut()?)
                            .map_err(|_| Error::Io)?;
                        s.pending = 0..n;
                        continue;
                    }
                    s.pipe.try_splice_connection(&s.destination)
                } else if let Some((ahead, range)) = s.source.read_ahead.take() {
                    let count = remaining.min(range.len());
                    if s.fallback.is_none() {
                        s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                    }
                    // Read-ahead may have a large head allocation but the tail
                    // is bounded by the receive growth step. Consume in chunks.
                    let count = count.min(MAX_PIPE_BYTES);
                    s.fallback.as_mut().unwrap().bytes_mut()?[..count]
                        .copy_from_slice(&ahead.bytes()?[range.start..range.start + count]);
                    if range.len() > count {
                        s.source.read_ahead = Some((ahead, range.start + count..range.end));
                    }
                    s.pending = 0..count;
                    s.source.rx_remaining = Some((remaining - count) as u64);
                    continue;
                } else if s.copied {
                    if s.fallback.is_none() {
                        s.fallback = Some(self.buffer(MAX_PIPE_BYTES)?);
                    }
                    s.source.fd.try_recv(
                        &mut s.fallback.as_mut().unwrap().bytes_mut()?
                            [..remaining.min(MAX_PIPE_BYTES)],
                    )
                } else {
                    s.pipe.try_splice_from(&s.source.fd, remaining)
                };
                match result {
                    Ok(0) => return Err(Error::Io),
                    Ok(n) => {
                        if writing {
                            if !s.pending.is_empty() {
                                s.pending.start += n;
                            }
                            s.destination.tx_remaining = Some(
                                s.destination.tx_remaining.ok_or(Error::InvalidRequest)? - n as u64,
                            );
                        } else {
                            s.source.rx_remaining = Some((remaining - n) as u64);
                            if s.copied {
                                s.pending = 0..n;
                            }
                        }
                    }
                    Err(error) if unsupported(&error) => {
                        s.copied = true;
                    }
                    Err(error) if error.kind() == std::io::ErrorKind::Interrupted => (),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                        wait = Some((
                            if writing {
                                s.destination.socket()
                            } else {
                                s.source.socket()
                            },
                            if writing { libc::POLLOUT } else { libc::POLLIN },
                        ));
                        break;
                    }
                    Err(_) => return Err(Error::Io),
                }
            }
            Ok(wait)
        }

        /// Readiness retains both connections and the pipe until the reactor fences it.
        async fn wait_relay_progress(
            &self,
            state: Rc<RefCell<Transit>>,
            wait: Option<(Rc<Descriptor>, i16)>,
            scope: &RequestScope,
        ) -> Result<()> {
            if let Some((fd, interest)) = wait {
                // A bounded poll interval also notices reverse disconnect while
                // the downstream source is silent. It never extends the deadline.
                let mut tick = scope.clone();
                tick.deadline.0 = tick
                    .deadline
                    .0
                    .min(crate::runtime::environment::now() + Duration::from_millis(10));
                match self
                    .reactor()
                    .readiness_with_lease(fd, interest as u32, state.clone(), &tick)
                    .await
                {
                    Ok(_) | Err(Error::DeadlineExceeded) => (),
                    Err(error) => return Err(error),
                }
            } else {
                let mut yielded = false;
                std::future::poll_fn(|cx| {
                    if yielded {
                        Poll::Ready(())
                    } else {
                        yielded = true;
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                })
                .await;
            }
            Ok(())
        }
    }
    fn finish(source: &mut ConnectionLease, destination: &mut ConnectionLease) -> Result<()> {
        if let Err(error) = source
            .finish_exchange()
            .and_then(|()| destination.finish_exchange())
        {
            source.poison();
            destination.poison();
            return Err(error);
        }
        Ok(())
    }

    #[derive(Clone, Debug, Eq, Hash, PartialEq, Ord, PartialOrd)]
    pub enum Endpoint {
        Unix(PathBuf),
        /// Cache incarnation is pool identity; the name-derived path is only a dial address.
        Origin {
            cache: crate::model::CacheId,
            path: PathBuf,
        },
        /// Numeric IP:port only. DNS resolution is deliberately not performed on an
        /// I/O worker. Control-plane DNS must supply an already-resolved endpoint.
        Peer(String),
    }
    struct Idle {
        session: Option<crate::security::connection::Session>,
        fd: Rc<Descriptor>,
        reservation: ConnectionReservation,
        since: Instant,
    }
    #[derive(Default)]
    struct Entry {
        active: usize,
        idle: Vec<Idle>,
        generation: u64,
    }
    struct PoolState {
        entries: BTreeMap<Endpoint, Entry>,
        expiry_cursor: Option<Endpoint>,
        next_expiry: Instant,
        next_generation: u64,
        closed: bool,
        waiting: VecDeque<Rc<WaitingEntry>>,
        poll_cursor: usize,
        next_waiter_poll: Instant,
    }
    struct WaitingEntry {
        endpoint: Endpoint,
        metadata: bool,
        waker: RefCell<Option<Waker>>,
    }
    struct Waiting {
        state: Rc<RefCell<PoolState>>,
        entry: Rc<WaitingEntry>,
        _reservation: Reservation,
    }
    impl Drop for Waiting {
        fn drop(&mut self) {
            let mut state = self.state.borrow_mut();
            state
                .waiting
                .retain(|entry| !Rc::ptr_eq(entry, &self.entry));
            if state.waiting.is_empty() {
                state.waiting = VecDeque::new();
            }
            state.wake_endpoint(&self.entry.endpoint);
        }
    }
    impl PoolState {
        fn wake_endpoint(&self, endpoint: &Endpoint) {
            if let Some(entry) = self
                .waiting
                .iter()
                .find(|entry| &entry.endpoint == endpoint)
            {
                if let Some(waker) = entry.waker.borrow().as_ref() {
                    waker.wake_by_ref();
                }
            }
        }
    }
    struct ReturnToPool {
        state: Weak<RefCell<PoolState>>,
        endpoint: Endpoint,
        generation: u64,
    }

    /// Exclusive connection ownership follows every submitted operation into the
    /// reactor. Drop is a pool return only after explicit successful finish_exchange.
    pub struct ConnectionLease {
        // Follows submitted I/O and opaque bodies through the actual completion fence.
        pub(crate) peer_admission: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
        pub(crate) peer_response_verified: bool,
        #[cfg(test)]
        pub(crate) relay_fallback: bool,
        #[cfg(test)]
        pub(crate) relay_fallback_at: Option<usize>,
        // While a relay sends its verified head, retain the unfinished downstream
        // connection through that send's completion and cancellation fences too.
        pub(crate) relay_peer: Option<Box<ConnectionLease>>,
        pub(crate) relay_pipe: Option<crate::memory::pipe::PipeLease>,
        pub(crate) relay_context: Option<Reservation>,
        pub(crate) relay_reservation: Option<Rc<Reservation>>,
        pub(crate) session: Option<crate::security::connection::Session>,
        // Ingress handshake admission follows socket I/O through cancellation fences.
        pub(crate) control_reservation: Option<Reservation>,
        pub(crate) fd: Rc<Descriptor>,
        reusable: bool,
        pub(crate) reservation: Option<Rc<ConnectionReservation>>,
        pool: Option<ReturnToPool>,
        pub(crate) read_ahead: Option<(OwnedBuffer, std::ops::Range<usize>)>,
        pub(crate) rx_remaining: Option<u64>,
        pub(crate) tx_remaining: Option<u64>,
        pub(crate) request_is_head: bool,
        pub(crate) close: bool,
    }
    impl ConnectionLease {
        /// Takes ownership of an accepted socket, configures NONBLOCK/CLOEXEC, and
        /// reserves connection admission. No pool return is associated with this FD.
        pub fn from_accepted(fd: Descriptor, admission: &Admission) -> Result<Self> {
            let reservation = admission.reserve_connection(ResourceClass::IngressConnection)?;
            Self::from_reserved(fd, reservation)
        }
        pub(crate) fn from_reserved(
            fd: Descriptor,
            reservation: ConnectionReservation,
        ) -> Result<Self> {
            set_nonblocking(&fd)?;
            Ok(Self::new(Rc::new(fd), reservation, None))
        }
        fn new(
            fd: Rc<Descriptor>,
            reservation: ConnectionReservation,
            pool: Option<ReturnToPool>,
        ) -> Self {
            Self {
                fd,
                peer_admission: None,
                peer_response_verified: false,
                #[cfg(test)]
                relay_fallback: false,
                #[cfg(test)]
                relay_fallback_at: None,
                relay_peer: None,
                relay_pipe: None,
                relay_context: None,
                relay_reservation: None,
                session: None,
                control_reservation: None,
                reusable: false,
                reservation: Some(Rc::new(reservation)),
                pool,
                read_ahead: None,
                rx_remaining: None,
                tx_remaining: None,
                request_is_head: false,
                close: false,
            }
        }
        pub(crate) fn begin_io(&mut self) {
            self.reusable = false;
        }
        pub(crate) fn install_session(
            &mut self,
            session: crate::security::connection::Session,
        ) -> Result<()> {
            if self.session.is_some() || self.close {
                return Err(Error::Unauthorized);
            }
            self.session = Some(session);
            self.reusable = false;
            Ok(())
        }
        /// Explicitly mark the complete request/response exchange reusable. This is
        /// valid for client or server use and resets framing for the next exchange.
        /// Unexpected pipelined/read-ahead data prevents pool return.
        pub fn finish_exchange(&mut self) -> Result<()> {
            self.next_round()?;
            if self.peer_response_verified {
                if let Some(permit) = &self.peer_admission {
                    permit.observe(crate::peer::adaptive::Outcome::Verified);
                }
                self.peer_response_verified = false;
            }
            self.reusable = !self.close;
            Ok(())
        }
        /// Reset framing inside an unfinished handshake/native transaction. Errors
        /// after this boundary still close the socket rather than returning it idle.
        pub(crate) fn next_round(&mut self) -> Result<()> {
            if self.rx_remaining != Some(0)
                || self.tx_remaining != Some(0)
                || self.read_ahead.is_some()
            {
                return Err(Error::InvalidRequest);
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
        #[cfg(test)]
        pub fn remaining_body(&self) -> Option<u64> {
            self.rx_remaining
        }
        /// Obtain the FD for an owned runtime operation. Pass this entire connection
        /// as the operation's lease; retaining just this FD does not retain admission.
        pub fn socket(&self) -> Rc<Descriptor> {
            self.fd.clone()
        }
        pub fn poison(&mut self) {
            self.close = true;
            self.reusable = false;
        }
    }
    impl Drop for ConnectionLease {
        fn drop(&mut self) {
            let Some(target) = &self.pool else {
                return;
            };
            let Some(state) = target.state.upgrade() else {
                return;
            };
            let mut state = state.borrow_mut();
            let closed = state.closed;
            if let Some(entry) = state.entries.get_mut(&target.endpoint) {
                entry.active = entry.active.saturating_sub(1);
                if !closed
                    && entry.generation == target.generation
                    && self.reusable
                    && Rc::strong_count(&self.fd) == 1
                {
                    if let Some(reservation) =
                        self.reservation.take().and_then(|r| Rc::try_unwrap(r).ok())
                    {
                        entry.idle.push(Idle {
                            session: self.session.take(),
                            fd: self.fd.clone(),
                            reservation,
                            since: crate::runtime::environment::now(),
                        });
                    }
                }
                if entry.active == 0 && entry.idle.is_empty() {
                    state.entries.remove(&target.endpoint);
                }
            }
            state.wake_endpoint(&target.endpoint);
        }
    }

    pub struct HttpPool {
        peer_tcp_nodelay: bool,
        reactor: Rc<Reactor>,
        admission: Rc<Admission>,
        per_endpoint: usize,
        per_origin: usize,
        max_endpoints: usize,
        idle_timeout: Duration,
        state: Rc<RefCell<PoolState>>,
    }
    impl HttpPool {
        pub fn new(reactor: Rc<Reactor>, admission: Rc<Admission>, per_endpoint: usize) -> Self {
            Self::with_limits(
                reactor,
                admission,
                per_endpoint,
                256,
                Duration::from_secs(30),
            )
        }
        pub fn with_limits(
            reactor: Rc<Reactor>,
            admission: Rc<Admission>,
            per_endpoint: usize,
            max_endpoints: usize,
            idle_timeout: Duration,
        ) -> Self {
            Self {
                reactor,
                admission,
                per_endpoint,
                peer_tcp_nodelay: false,
                per_origin: per_endpoint,
                max_endpoints,
                idle_timeout,
                state: Rc::new(RefCell::new(PoolState {
                    entries: BTreeMap::new(),
                    expiry_cursor: None,
                    next_expiry: crate::runtime::environment::now(),
                    next_generation: 0,
                    closed: false,
                    waiting: VecDeque::new(),
                    poll_cursor: 0,
                    next_waiter_poll: crate::runtime::environment::now(),
                })),
            }
        }
        /// Unix adapter endpoints have a separate cap from TCP peer endpoints. Both
        /// share the same accounted connection ceiling and bounded endpoint table.
        pub fn with_origin_limit(mut self, per_origin: usize) -> Self {
            self.per_origin = per_origin;
            self
        }
        /// Opt into NODELAY only for outbound peer TCP sockets, never Unix origins.
        pub fn with_peer_tcp_nodelay(mut self, enabled: bool) -> Self {
            self.peer_tcp_nodelay = enabled;
            self
        }
        /// Capacity exhaustion fails immediately with Overloaded: there is no hidden
        /// unbounded waiter queue. Callers schedule any retry under their own budget.
        pub fn checkout<'a>(
            &'a self,
            endpoint: &'a Endpoint,
            scope: &'a RequestScope,
        ) -> Operation<'a, ConnectionLease> {
            self.checkout_peer(endpoint, None, None, None, scope)
        }
        pub(crate) fn checkout_peer<'a>(
            &'a self,
            endpoint: &'a Endpoint,
            relay: Option<Rc<Reservation>>,
            peer: Option<std::sync::Arc<crate::peer::adaptive::Permit>>,
            failure: Option<Rc<std::cell::Cell<bool>>>,
            scope: &'a RequestScope,
        ) -> Operation<'a, ConnectionLease> {
            Box::pin(async move {
                scope.check()?;
                let (mut connection, address) = self.prepare_connection(endpoint)?;
                connection.relay_reservation = relay;
                connection.peer_admission = peer.clone();
                let errno = Rc::new(std::cell::Cell::new(None));
                let result = if let Some(address) = address {
                    self.reactor
                        .connect_with_observation(
                            connection.socket(),
                            address,
                            connection,
                            Some(errno.clone()),
                            scope,
                        )
                        .await
                } else {
                    Ok(connection)
                };
                if peer_connect_failure(errno.get()) && scope.check().is_ok() {
                    if let Some(failure) = failure {
                        failure.set(true);
                    }
                    if let Some(peer) = peer {
                        peer.observe(crate::peer::adaptive::Outcome::PeerFailure);
                    }
                }
                result
            })
        }

        /// Bounded FIFO per endpoint, used by origin requests. Waiters hold context
        /// quota but no connection or reactor operation. The worker must call
        /// poll_waiters on its bounded tick, including while draining, for deadline
        /// checks and connection quota released by other pools/accepted sockets.
        pub fn checkout_wait<'a>(
            &'a self,
            endpoint: &'a Endpoint,
            scope: &'a RequestScope,
        ) -> Operation<'a, ConnectionLease> {
            self.checkout_wait_class(endpoint, scope, false)
        }

        /// Metadata does not queue behind origin page transfers. Where the endpoint
        /// cap permits it, GETs leave one slot free for HEAD. Global outbound and
        /// context quotas remain authoritative; this is not an unaccounted socket.
        pub fn checkout_metadata<'a>(
            &'a self,
            endpoint: &'a Endpoint,
            scope: &'a RequestScope,
        ) -> Operation<'a, ConnectionLease> {
            self.checkout_wait_class(endpoint, scope, true)
        }

        fn checkout_wait_class<'a>(
            &'a self,
            endpoint: &'a Endpoint,
            scope: &'a RequestScope,
            metadata: bool,
        ) -> Operation<'a, ConnectionLease> {
            Box::pin(async move {
                scope.check()?;
                let mut waiting = None;
                let cancellation = scope.cancellation.subscribe()?;
                let (connection, address) = poll_fn(|cx| {
                    cancellation.register(cx.waker());
                    scope.check()?;
                    if self.state.borrow().closed || self.admission.is_stopped() {
                        return Poll::Ready(Err(Error::Unavailable));
                    }
                    let first = self
                        .state
                        .borrow()
                        .waiting
                        .iter()
                        .find(|entry| &entry.endpoint == endpoint && entry.metadata == metadata)
                        .cloned();
                    let turn = first.as_ref().is_none_or(|first| {
                        waiting
                            .as_ref()
                            .is_some_and(|waiting: &Waiting| Rc::ptr_eq(first, &waiting.entry))
                    });
                    if turn && self.class_available(endpoint, metadata) {
                        match self.prepare_connection(endpoint) {
                            Err(Error::Overloaded) => (),
                            result => return Poll::Ready(result),
                        }
                    }
                    if waiting.is_none() {
                        if self.state.borrow().waiting.len()
                            >= self.admission.limits().queue_entries.get()
                        {
                            return Poll::Ready(Err(Error::Overloaded));
                        }
                        let reservation = self.admission.reserve(
                            None,
                            ResourceClass::RequestContext,
                            std::mem::size_of::<Waiting>()
                                + std::mem::size_of::<WaitingEntry>()
                                + match endpoint {
                                    Endpoint::Unix(path) => path.as_os_str().len(),
                                    Endpoint::Origin { cache, path } => {
                                        cache.0.len() + path.as_os_str().len()
                                    }
                                    Endpoint::Peer(address) => address.len(),
                                }
                                + 128,
                        )?;
                        let entry = Rc::new(WaitingEntry {
                            endpoint: endpoint.clone(),
                            metadata,
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

        fn class_available(&self, endpoint: &Endpoint, metadata: bool) -> bool {
            if metadata || !matches!(endpoint, Endpoint::Origin { .. }) || self.per_origin <= 1 {
                return true;
            }
            self.state
                .borrow()
                .entries
                .get(endpoint)
                .is_none_or(|entry| entry.active < self.per_origin - 1)
        }

        /// Wake a bounded round-robin batch, including child futures whose executor
        /// does not repoll them merely because the outer worker received a timer tick.
        pub fn poll_waiters(&self, budget: usize) {
            self.expire_idle_budgeted(budget);
            let mut state = self.state.borrow_mut();
            let now = crate::runtime::environment::now();
            if budget == 0 || state.waiting.is_empty() || now < state.next_waiter_poll {
                return;
            }
            // Worker wakeups may be much more frequent than its fallback timer.
            // Do not let waking a queued child create an immediate busy-wake loop.
            state.next_waiter_poll = now + Duration::from_millis(1);
            for _ in 0..budget.min(state.waiting.len()) {
                state.poll_cursor %= state.waiting.len();
                if let Some(waker) = state.waiting[state.poll_cursor].waker.borrow().as_ref() {
                    waker.wake_by_ref();
                }
                state.poll_cursor += 1;
            }
        }

        fn prepare_connection(
            &self,
            endpoint: &Endpoint,
        ) -> Result<(
            ConnectionLease,
            Option<crate::runtime::reactor::SocketAddress>,
        )> {
            if self.admission.is_stopped() {
                return Err(Error::Unavailable);
            }
            let (idle, generation) = {
                let mut state = self.state.borrow_mut();
                if state.closed {
                    return Err(Error::Unavailable);
                }
                if !state.entries.contains_key(endpoint) {
                    if state.entries.len() >= self.max_endpoints {
                        state.entries.retain(|_, entry| entry.active != 0);
                    }
                    if state.entries.len() >= self.max_endpoints {
                        return Err(Error::Overloaded);
                    }
                    state.next_generation = state
                        .next_generation
                        .checked_add(1)
                        .ok_or(Error::Unavailable)?;
                    let generation = state.next_generation;
                    state.entries.insert(
                        endpoint.clone(),
                        Entry {
                            generation,
                            ..Entry::default()
                        },
                    );
                }
                let entry = state.entries.get_mut(endpoint).ok_or(Error::Unavailable)?;
                let now = crate::runtime::environment::now();
                entry
                    .idle
                    .retain(|idle| now.saturating_duration_since(idle.since) < self.idle_timeout);
                let limit = match endpoint {
                    Endpoint::Unix(_) | Endpoint::Origin { .. } => self.per_origin,
                    Endpoint::Peer(_) => self.per_endpoint,
                };
                if entry.active >= limit {
                    return Err(Error::Overloaded);
                }
                entry.active += 1;
                (entry.idle.pop(), entry.generation)
            };
            // The slot guard closes a connecting slot if this future is dropped,
            // including before the connection lease has been constructed.
            let target = ReturnToPool {
                state: Rc::downgrade(&self.state),
                endpoint: endpoint.clone(),
                generation,
            };
            let mut slot = ConnectingSlot(Some(target));
            if let Some(idle) = idle {
                if idle_healthy(&idle.fd) {
                    let mut connection =
                        ConnectionLease::new(idle.fd, idle.reservation, slot.0.take());
                    connection.session = idle.session;
                    return Ok((connection, None));
                }
            }
            let reservation = match self
                .admission
                .reserve_connection(ResourceClass::OutboundConnection)
            {
                Err(Error::Overloaded) => {
                    // Idle sockets must not strand this pool's connection quota.
                    for entry in self.state.borrow_mut().entries.values_mut() {
                        entry.idle.clear();
                    }
                    self.admission
                        .reserve_connection(ResourceClass::OutboundConnection)?
                }
                result => result?,
            };
            let (fd, address) = create_socket(endpoint)?;
            if matches!(endpoint, Endpoint::Peer(_)) {
                fd.enable_tcp_nodelay(self.peer_tcp_nodelay)?;
            }
            let connection = ConnectionLease::new(Rc::new(fd), reservation, slot.0.take());
            Ok((connection, Some(address)))
        }

        async fn connect(
            &self,
            connection: ConnectionLease,
            address: Option<crate::runtime::reactor::SocketAddress>,
            scope: &RequestScope,
        ) -> Result<ConnectionLease> {
            let Some(address) = address else {
                return Ok(connection);
            };
            // Retain socket, admission and pool slot in the reactor through the
            // original/cancellation fences, even after both future and pool drop.
            self.reactor
                .connect_with_lease(connection.socket(), address, connection, scope)
                .await
        }
        #[cfg(test)]
        pub fn expire_idle(&self) {
            self.state.borrow_mut().next_expiry = crate::runtime::environment::now();
            self.expire_idle_budgeted(usize::MAX);
        }
        /// Autonomous worker tick: visit at most `budget` endpoint buckets, resuming
        /// from an ordered cursor. No checkout or waiter is required for expiration.
        pub fn expire_idle_budgeted(&self, budget: usize) {
            use std::ops::Bound::{Excluded, Unbounded};
            let now = crate::runtime::environment::now();
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
                    .map(|(key, _)| key.clone());
                let Some(key) = next else {
                    break;
                };
                let entry = state.entries.get_mut(&key).expect("selected endpoint");
                entry
                    .idle
                    .retain(|idle| now.saturating_duration_since(idle.since) < self.idle_timeout);
                if entry.active == 0 && entry.idle.is_empty() {
                    state.entries.remove(&key);
                }
                state.expiry_cursor = Some(key);
            }
        }
        /// Invalidate an endpoint generation without closing active operations. Old
        /// leases finish normally but cannot enter the new generation's idle pool.
        pub fn invalidate(&self, endpoint: &Endpoint) {
            let mut state = self.state.borrow_mut();
            let Some(generation) = state.next_generation.checked_add(1) else {
                state.closed = true;
                return;
            };
            state.next_generation = generation;
            if let Some(entry) = state.entries.get_mut(endpoint) {
                entry.idle.clear();
                entry.generation = generation;
            }
        }
        pub fn close(&self) {
            let mut state = self.state.borrow_mut();
            state.closed = true;
            for entry in state.entries.values_mut() {
                entry.idle.clear();
            }
            for entry in &state.waiting {
                if let Some(waker) = entry.waker.borrow().as_ref() {
                    waker.wake_by_ref();
                }
            }
        }
    }

    /// Resource exhaustion/address selection failures are local, not broken peers.
    fn peer_connect_failure(errno: Option<i32>) -> bool {
        matches!(
            errno,
            Some(libc::ECONNREFUSED | libc::ECONNRESET | libc::EPIPE)
        )
    }

    #[cfg(test)]
    #[test]
    fn adaptive_connect_errno_preserves_local_exhaustion_as_neutral() {
        for errno in [
            None,
            Some(libc::ENOBUFS),
            Some(libc::ENOMEM),
            Some(libc::EADDRNOTAVAIL),
            Some(libc::ECANCELED),
            Some(libc::EIO),
        ] {
            assert!(!peer_connect_failure(errno));
        }
        for errno in [libc::ECONNREFUSED, libc::ECONNRESET, libc::EPIPE] {
            assert!(peer_connect_failure(Some(errno)));
        }
    }

    #[cfg(test)]
    #[test]
    fn adaptive_checkout_attributes_actual_connect_completion_errno() {
        use crate::{
            runtime::reactor::simulation::{Fault, Simulation},
            telemetry::metrics::{Event, Gauge, Metrics},
        };
        for (errno, blame) in [
            (libc::ENOBUFS, false),
            (libc::ENOMEM, false),
            (libc::EADDRNOTAVAIL, false),
            (libc::ECONNREFUSED, true),
        ] {
            let simulation = Simulation::new();
            let _env = simulation.enter();
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let pool = HttpPool::new(reactor.clone(), admission, 1);
            let metrics = Metrics::default();
            let peers = crate::peer::adaptive::AdaptivePeers::new(
                crate::peer::adaptive::Config {
                    total: 1,
                    per_peer: 1,
                },
                metrics.clone(),
            )
            .unwrap();
            let node = crate::model::NodeId("peer".into());
            let permit = peers.acquire(&node).unwrap();
            let failure = Rc::new(std::cell::Cell::new(false));
            let scope = RequestScope::new(
                crate::model::RequestId([88; 16]),
                crate::runtime::environment::now() + Duration::from_secs(5),
            )
            .unwrap();
            let endpoint = Endpoint::Peer("127.0.0.1:9999".into());
            simulation.inject("connect", Fault::Errno(errno));
            let mut checkout = pool.checkout_peer(
                &endpoint,
                None,
                Some(permit.clone()),
                Some(failure.clone()),
                &scope,
            );
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let mut result = None;
            for _ in 0..32 {
                if let std::task::Poll::Ready(done) = checkout.as_mut().poll(&mut cx) {
                    result = Some(done);
                    break;
                }
                reactor.poll_budgeted(32).unwrap();
            }
            assert!(matches!(result, Some(Err(Error::Io))));
            drop(checkout);
            drop(permit);
            assert_eq!(failure.get(), blame);
            assert_eq!(peers.available(&node), !blame);
            assert_eq!(metrics.count(Event::PeerLinkFailure), u64::from(blame));
            assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
        }
    }
    impl Drop for HttpPool {
        fn drop(&mut self) {
            self.close();
        }
    }

    fn idle_healthy(fd: &Descriptor) -> bool {
        fd.idle_healthy()
    }
    struct ConnectingSlot(Option<ReturnToPool>);
    impl Drop for ConnectingSlot {
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

    fn set_nonblocking(fd: &Descriptor) -> Result<()> {
        fd.set_nonblocking()
    }

    // Runtime's owned address type is used so connect never borrows sockaddr bytes
    // from a future that may disappear while its SQE is in flight.
    fn create_socket(
        endpoint: &Endpoint,
    ) -> Result<(Descriptor, crate::runtime::reactor::SocketAddress)> {
        use crate::runtime::reactor::SocketAddress;
        let (domain, address) = match endpoint {
            Endpoint::Peer(value) => {
                let address: SocketAddr = value.parse().map_err(|_| Error::InvalidConfiguration)?;
                (
                    if address.is_ipv4() {
                        libc::AF_INET
                    } else {
                        libc::AF_INET6
                    },
                    SocketAddress::Inet(address),
                )
            }
            Endpoint::Unix(path) | Endpoint::Origin { path, .. } => {
                (libc::AF_UNIX, SocketAddress::Unix(path.clone()))
            }
        };
        Ok((Descriptor::socket(domain)?, address))
    }

    #[cfg(test)]
    mod pool_tests;
}

use crate::error::{Error, Result};
use zeroize::Zeroize;
/// Client/origin profile cap. Other endpoints must supply their own bounded cap.
pub const MAX_HEAD_BYTES: usize = 32 * 1024;

pub enum StartLine {
    Request { method: String, target: String },
    Response { status: u16 },
}
pub struct Header {
    pub name: String,
    pub value: Vec<u8>,
}
impl Drop for Header {
    fn drop(&mut self) {
        self.value.zeroize();
    }
}
pub struct MessageHead {
    pub start: StartLine,
    pub headers: Vec<Header>,
}

impl MessageHead {
    /// All occurrences, in wire order. Semantic consumers must reject ambiguous
    /// singleton fields rather than silently taking the first value.
    pub fn values<'a>(&'a self, name: &'a str) -> impl Iterator<Item = &'a [u8]> + 'a {
        self.headers.iter().filter_map(move |header| {
            header
                .name
                .eq_ignore_ascii_case(name)
                .then_some(header.value.as_slice())
        })
    }
    pub fn unique(&self, name: &str) -> Result<Option<&[u8]>> {
        let mut found = None;
        for header in &self.headers {
            if header.name.eq_ignore_ascii_case(name) {
                if found.is_some() {
                    return Err(Error::InvalidRequest);
                }
                found = Some(header.value.as_slice());
            }
        }
        Ok(found)
    }
    /// This fixed-length protocol rejects transfer coding, including identity.
    /// Content-Length duplicates are rejected even if their values are identical.
    pub fn content_length(&self) -> Result<Option<u64>> {
        if self
            .headers
            .iter()
            .any(|h| h.name.eq_ignore_ascii_case("transfer-encoding"))
        {
            return Err(Error::InvalidRequest);
        }
        self.unique("content-length")?.map(decimal).transpose()
    }
    pub fn closes_connection(&self) -> Result<bool> {
        let mut close = false;
        for value in self.values("connection") {
            for token in value.split(|b| *b == b',') {
                let token = trim_ows(token);
                if token.is_empty() || !token.iter().copied().all(is_token) {
                    return Err(Error::InvalidRequest);
                }
                // Headers defining framing/authentication must not be nominated
                // as hop-by-hop fields and then stripped by an intermediary.
                if !token.eq_ignore_ascii_case(b"close")
                    && !token.eq_ignore_ascii_case(b"keep-alive")
                {
                    return Err(Error::InvalidRequest);
                }
                close |= token.eq_ignore_ascii_case(b"close");
            }
        }
        Ok(close)
    }
}

pub struct Codec {
    header_limit: usize,
    body_limit: u64,
}
impl Codec {
    pub fn new(header_limit: usize, body_limit: u64) -> Self {
        Self {
            header_limit,
            body_limit,
        }
    }
    pub fn header_limit(&self) -> usize {
        self.header_limit
    }
    pub fn body_limit(&self) -> u64 {
        self.body_limit
    }
    pub fn limited(&self, header_limit: usize) -> Self {
        Self::new(self.header_limit.min(header_limit), self.body_limit)
    }
    /// Exact representation upper bound, checked before decoded allocations.
    /// Four wire bytes is the minimum valid field ("X:\r\n"); retain every
    /// previously legal field count rather than imposing an arbitrary small cap.
    pub(crate) fn decoded_allocation(&self, bytes: &[u8]) -> Result<usize> {
        let end = bytes
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|n| n + 4)
            .ok_or(Error::InvalidRequest)?;
        if end > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        let fields = bytes[..end]
            .windows(2)
            .filter(|w| *w == b"\r\n")
            .count()
            .saturating_sub(2);
        if fields > self.header_limit / 4 {
            return Err(Error::HeaderTooLarge);
        }
        fields
            .checked_mul(std::mem::size_of::<Header>())
            .and_then(|n| n.checked_add(end))
            .and_then(|n| n.checked_add(std::mem::size_of::<MessageHead>()))
            .ok_or(Error::HeaderTooLarge)
    }

    /// Returns bytes consumed from a possibly larger read, never body bytes.
    /// Content-Length is representation metadata on HEAD responses, so the body
    /// cap is enforced by HttpIo after method/status semantics are known.
    pub fn decode_head(&self, bytes: &[u8]) -> Result<Option<(MessageHead, usize)>> {
        let bounded = &bytes[..bytes.len().min(self.header_limit)];
        let Some(end) = bounded
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .map(|n| n + 4)
        else {
            if bytes.len() >= self.header_limit {
                return Err(Error::HeaderTooLarge);
            }
            if invalid_line_endings(bytes) {
                return Err(Error::InvalidRequest);
            }
            return Ok(None);
        };
        if end > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        if invalid_line_endings(&bytes[..end]) {
            return Err(Error::InvalidRequest);
        }
        self.decoded_allocation(&bytes[..end])?;
        // Validate the start line with constant parser scratch. Header syntax is
        // validated below without allocating one httparse slot per field.
        let count = bytes[..end].windows(2).filter(|w| *w == b"\r\n").count();
        let first = bytes[..end]
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or(Error::InvalidRequest)?;
        let mut slots = [];
        let start = if bytes.starts_with(b"HTTP/") {
            let mut response = httparse::Response::new(&mut slots);
            response
                .parse(&bytes[..first + 2])
                .map_err(|_| Error::InvalidRequest)?;
            if response.version != Some(1) {
                return Err(Error::InvalidRequest);
            }
            StartLine::Response {
                status: response.code.ok_or(Error::InvalidRequest)?,
            }
        } else {
            let mut request = httparse::Request::new(&mut slots);
            request
                .parse(&bytes[..first + 2])
                .map_err(|_| Error::InvalidRequest)?;
            if request.version != Some(1) {
                return Err(Error::InvalidRequest);
            }
            StartLine::Request {
                method: request.method.ok_or(Error::InvalidRequest)?.into(),
                target: request.path.ok_or(Error::InvalidRequest)?.into(),
            }
        };
        // Do not take httparse's trimmed values: signed opaque fields must retain
        // the exact application bytes, including trailing SP/HTAB and obs-text.
        let first = bytes[..end]
            .windows(2)
            .position(|w| w == b"\r\n")
            .ok_or(Error::InvalidRequest)?;
        let mut headers = Vec::with_capacity(count.saturating_sub(2));
        for line in bytes[first + 2..end - 2].split(|b| *b == b'\n') {
            if line.is_empty() {
                continue;
            }
            let line = line.strip_suffix(b"\r").ok_or(Error::InvalidRequest)?;
            if line.is_empty() {
                continue;
            }
            let colon = line
                .iter()
                .position(|b| *b == b':')
                .ok_or(Error::InvalidRequest)?;
            let name = std::str::from_utf8(&line[..colon])
                .map_err(|_| Error::InvalidRequest)?
                .to_owned();
            let value = &line[colon + 1..];
            if opaque_field(&name) {
                let Some(value) = value.strip_prefix(b" ") else {
                    return Err(Error::InvalidRequest);
                };
                validate_opaque_edges(value)?;
            }
            headers.push(Header {
                name,
                value: value.strip_prefix(b" ").unwrap_or(value).to_vec(),
            });
        }
        let head = MessageHead { start, headers };
        self.validate(&head)?;
        Ok(Some((head, end)))
    }
    pub fn encode_head(&self, head: &MessageHead) -> Result<Vec<u8>> {
        let start_length = match &head.start {
            StartLine::Request { method, target } => method
                .len()
                .checked_add(target.len())
                .and_then(|n| n.checked_add(12))
                .ok_or(Error::HeaderTooLarge)?,
            StartLine::Response { .. } => 15,
        };
        let length = head.headers.iter().try_fold(
            start_length.checked_add(2).ok_or(Error::HeaderTooLarge)?,
            |total, h| {
                total
                    .checked_add(h.name.len())
                    .and_then(|n| n.checked_add(h.value.len()))
                    .and_then(|n| n.checked_add(4))
                    .ok_or(Error::HeaderTooLarge)
            },
        )?;
        if length > self.header_limit {
            return Err(Error::HeaderTooLarge);
        }
        self.validate(head)?;
        let start = match &head.start {
            StartLine::Request { method, target } => format!("{method} {target} HTTP/1.1\r\n"),
            StartLine::Response { status } => format!("HTTP/1.1 {status:03} \r\n"),
        };
        let mut bytes = Vec::with_capacity(length);
        bytes.extend_from_slice(start.as_bytes());
        for header in &head.headers {
            bytes.extend_from_slice(header.name.as_bytes());
            bytes.extend_from_slice(b": ");
            bytes.extend_from_slice(&header.value);
            bytes.extend_from_slice(b"\r\n");
        }
        bytes.extend_from_slice(b"\r\n");
        Ok(bytes)
    }
    fn validate(&self, head: &MessageHead) -> Result<()> {
        match &head.start {
            StartLine::Request { method, target } => {
                if method.is_empty()
                    || !method.bytes().all(is_token)
                    || target.is_empty()
                    || !target.bytes().all(|b| b > 32 && b < 127)
                {
                    return Err(Error::InvalidRequest);
                }
            }
            StartLine::Response { status } if !(100..=599).contains(status) => {
                return Err(Error::InvalidRequest);
            }
            _ => {}
        }
        for header in &head.headers {
            if header.name.is_empty()
                || !header.name.bytes().all(is_token)
                || !header
                    .value
                    .iter()
                    .all(|b| *b == b'\t' || (*b >= 32 && *b != 127))
            {
                return Err(Error::InvalidRequest);
            }
            if opaque_field(&header.name) {
                validate_opaque_edges(&header.value)?;
            }
        }
        head.content_length()?;
        head.closes_connection()?;
        // Host remains optional for the local UDS profile, but never ambiguous.
        head.unique("host")?;
        Ok(())
    }
}

fn opaque_field(name: &str) -> bool {
    name.eq_ignore_ascii_case("authorization") || name.eq_ignore_ascii_case("racer-metadata")
}
fn validate_opaque_edges(value: &[u8]) -> Result<()> {
    if value.first().is_some_and(|b| *b == b' ' || *b == b'\t')
        || value.last().is_some_and(|b| *b == b' ' || *b == b'\t')
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

pub(crate) fn is_token(b: u8) -> bool {
    b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b)
}
fn invalid_line_endings(bytes: &[u8]) -> bool {
    bytes.iter().enumerate().any(|(i, b)| {
        (*b == b'\n' && (i == 0 || bytes[i - 1] != b'\r'))
            || (*b == b'\r' && i + 1 < bytes.len() && bytes[i + 1] != b'\n')
    })
}
pub(crate) fn trim_ows(mut value: &[u8]) -> &[u8] {
    while value.first().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[1..];
    }
    while value.last().is_some_and(|b| *b == b' ' || *b == b'\t') {
        value = &value[..value.len() - 1];
    }
    value
}
fn decimal(value: &[u8]) -> Result<u64> {
    if value.is_empty() {
        return Err(Error::InvalidRequest);
    }
    value.iter().try_fold(0u64, |n, b| {
        if !b.is_ascii_digit() {
            return Err(Error::InvalidRequest);
        }
        n.checked_mul(10)
            .and_then(|n| n.checked_add(u64::from(*b - b'0')))
            .ok_or(Error::InvalidRequest)
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn maximum_wire_heads_and_maximum_field_count_have_checked_decoded_bounds() {
        for limit in [MAX_HEAD_BYTES, crate::peer::protocol::MAX_ENVELOPE_HEAD] {
            let codec = Codec::new(limit, 16);
            let mut bytes = b"GET / HTTP/1.1\r\nX: ".to_vec();
            bytes.resize(limit - 4, b'a');
            bytes.extend_from_slice(b"\r\n\r\n");
            let (head, used) = codec.decode_head(&bytes).unwrap().unwrap();
            assert_eq!(used, limit);
            assert_eq!(codec.encode_head(&head).unwrap(), bytes);
            let allocation = codec.decoded_allocation(&bytes).unwrap();
            assert!(allocation >= head.headers[0].value.len() + std::mem::size_of::<Header>());
            let mut fields = b"GET / HTTP/1.1\r\n".to_vec();
            while fields.len() + 6 <= limit {
                fields.extend_from_slice(b"X:\r\n");
            }
            fields.extend_from_slice(b"\r\n");
            let count = (fields.len() - 18) / 4;
            let allocation = codec.decoded_allocation(&fields).unwrap();
            assert!(allocation >= count * std::mem::size_of::<Header>());
            assert_eq!(
                codec.decode_head(&fields).unwrap().unwrap().0.headers.len(),
                count
            );
            assert!(matches!(
                Codec::new(limit - 1, 16).decoded_allocation(&bytes),
                Err(Error::HeaderTooLarge)
            ));
        }
    }
    #[test]
    fn fragmented_head_preserves_opaque_values_and_duplicates() {
        let codec = Codec::new(1024, 16);
        let bytes = b"GET /a HTTP/1.1\r\nX-Opaque:  \xff\t \r\nx-opaque: second\r\nContent-Length: 3\r\n\r\nabc";
        let end = bytes.len() - 3;
        for length in 0..end {
            assert!(codec.decode_head(&bytes[..length]).unwrap().is_none());
        }
        let (head, used) = codec.decode_head(bytes).unwrap().unwrap();
        assert_eq!(used, end);
        assert_eq!(head.headers[0].value, b" \xff\t ");
        assert_eq!(head.values("x-OPAQUE").count(), 2);
        assert_eq!(head.unique("x-opaque"), Err(Error::InvalidRequest));
        assert_eq!(codec.encode_head(&head).unwrap(), bytes[..end]);
    }
    #[test]
    fn rejects_smuggling_and_malformed_syntax() {
        let codec = Codec::new(1024, 16);
        for bytes in [
            &b"GET / HTTP/1.1\n\n"[..],
            &b"GET / HTTP/1.0\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nContent-Length: 1\r\ncontent-length: 1\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nContent-Length: +1\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nContent-Length: 18446744073709551616\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nX: first\r\n second\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nConnection: content-length\r\n\r\n"[..],
            &b"GET / HTTP/1.1\r\nBad : x\r\n\r\n"[..],
        ] {
            assert!(codec.decode_head(bytes).is_err(), "accepted malformed head");
        }
    }
    #[test]
    fn bounds_heads_without_counting_read_ahead_or_head_representation_length() {
        let bytes = b"HTTP/1.1 200 OK\r\nContent-Length: 99999999\r\n\r\n";
        let codec = Codec::new(bytes.len(), 16);
        assert_eq!(codec.decode_head(bytes).unwrap().unwrap().1, bytes.len());
        assert!(Codec::new(bytes.len() - 1, 16).decode_head(bytes).is_err());
        assert!(Codec::new(8, 16).decode_head(b"GET /thi").is_err());
        let head = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header {
                name: "x".into(),
                value: b"a\r\ninjected: true".to_vec(),
            }],
        };
        assert!(codec.encode_head(&head).is_err());
    }
    #[test]
    fn opaque_field_raw_separators_are_validated_before_decoding() {
        let codec = Codec::new(MAX_HEAD_BYTES, 16);
        for field in ["Authorization", "Racer-Metadata"] {
            for suffix in [
                &b"x"[..],
                &b"  x"[..],
                &b"\tx"[..],
                &b" \tx"[..],
                &b" x "[..],
                &b" x\t"[..],
            ] {
                let mut bytes = format!("GET / HTTP/1.1\r\n{field}:").into_bytes();
                bytes.extend_from_slice(suffix);
                bytes.extend_from_slice(b"\r\n\r\n");
                assert!(codec.decode_head(&bytes).is_err());
            }
            let mut bytes = format!("GET / HTTP/1.1\r\n{field}: ").into_bytes();
            bytes.extend_from_slice(b"\xffopaque\x80\r\n\r\n");
            let (head, _) = codec.decode_head(&bytes).unwrap().unwrap();
            assert_eq!(head.unique(field).unwrap().unwrap(), b"\xffopaque\x80");
            assert_eq!(codec.encode_head(&head).unwrap(), bytes);
        }
        assert!(matches!(
            codec.decode_head(&vec![b'x'; MAX_HEAD_BYTES]),
            Err(Error::HeaderTooLarge)
        ));
    }
}
