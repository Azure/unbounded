//! Socket operations retain buffers through partial I/O and cancellation completion.
//!
//! Connections transfer by value so an abandoned future cannot close/recycle the
//! socket while the kernel uses it. Before submission, move the connection and
//! owned buffers into reactor-owned state; return them only after the final fence.
//! Head operations must stage encoded/received bytes in owned IoBuffer storage.
use super::{
    codec::{MessageHead, StartLine},
    pool::ConnectionLease,
};
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
        reactor::{Completion, IoBuffer, Reactor},
    },
};
use std::{ops::Range, rc::Rc};
use zeroize::{Zeroize, Zeroizing};
/// Bounded, address-stable staging storage. Constructor reserves quota before
/// allocation; ownership includes that quota through reactor completion.
pub struct OwnedBuffer {
    bytes: Box<[u8]>,
    reservation: Reservation,
}
impl Drop for OwnedBuffer {
    fn drop(&mut self) {
        self.bytes.zeroize();
    }
}
impl OwnedBuffer {
    pub fn new(admission: &Admission, length: usize) -> Result<Self> {
        let reservation = admission.reserve(None, ResourceClass::RequestContext, length.max(1))?;
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(length)
            .map_err(|_| Error::Overloaded)?;
        bytes.resize(length, 0);
        Ok(Self {
            bytes: bytes.into_boxed_slice(),
            reservation,
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
pub struct HttpIo {
    reactor: Rc<Reactor>,
    codec: super::codec::Codec,
    admission: Option<Rc<Admission>>,
}
/// A completed head operation, including the still-exclusively-owned connection.
///
/// Head and body operations transfer ownership in both directions:
/// ```no_run
/// use racer_dataplane::{error::Result,
///     http::{io::HttpIo, pool::ConnectionLease}, memory::pool::PlaintextBuffer,
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
}
impl HttpIo {
    pub fn new(reactor: Rc<Reactor>, codec: super::codec::Codec) -> Self {
        Self {
            reactor,
            codec,
            admission: None,
        }
    }
    /// Production constructor. The legacy constructor is retained for composition
    /// compatibility, but head staging requires explicitly supplied admission.
    pub fn with_admission(
        reactor: Rc<Reactor>,
        codec: super::codec::Codec,
        admission: Rc<Admission>,
    ) -> Self {
        Self {
            reactor,
            codec,
            admission: Some(admission),
        }
    }
    pub fn buffer(&self, length: usize) -> Result<OwnedBuffer> {
        OwnedBuffer::new(
            self.admission
                .as_deref()
                .ok_or(Error::InvalidConfiguration)?,
            length,
        )
    }
    /// Share reactor/admission while applying a smaller endpoint-specific head cap.
    pub fn capped(&self, header_limit: usize) -> Self {
        Self {
            reactor: self.reactor.clone(),
            codec: self.codec.limited(header_limit),
            admission: self.admission.clone(),
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
            let mut buffer = self.buffer(codec.header_limit())?;
            let mut used = 0;
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
                let decoded = match codec.decode_head(&buffer.bytes[..used]) {
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
                    return Ok(HeadCompletion {
                        connection,
                        value: Ok(head),
                    });
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
            let length = self.framing(&head, connection.request_is_head)?;
            connection.close |= head.closes_connection()?;
            if let StartLine::Request { method, .. } = &head.start {
                connection.request_is_head = method == "HEAD";
            }
            // Reserve worst-case staging before the codec allocates encoded bytes.
            let mut buffer = self.buffer(self.codec.header_limit())?;
            let scratch = self
                .admission
                .as_deref()
                .ok_or(Error::InvalidConfiguration)?
                .reserve(
                    None,
                    ResourceClass::RequestContext,
                    self.codec.header_limit().max(1),
                )?;
            let encoded = Zeroizing::new(self.codec.encode_head(&head)?);
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
            })
        })
    }
    /// Stream bounded chunks; endpoint controls expected body length and validation.
    /// Borrowed destinations cannot survive abandonment of a submitted operation:
    /// ```compile_fail
    /// use racer_dataplane::{http::{io::HttpIo, pool::ConnectionLease},
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
    /// Stage borrowed/shared page slices in an owned buffer before calling.
    /// ```compile_fail
    /// use racer_dataplane::{http::{io::HttpIo, pool::ConnectionLease},
    ///     runtime::deadline::RequestScope};
    /// fn borrowed(io: &HttpIo, connection: ConnectionLease, scope: &RequestScope) {
    ///     let bytes = [0; 16];
    ///     let _future = io.write_body(connection, &bytes[..], scope);
    /// }
    /// ```
    pub fn write_body<'a, B: IoBuffer>(
        &'a self,
        connection: ConnectionLease,
        buffer: B,
        scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, ConnectionLease>> {
        Box::pin(async move {
            let length = buffer.bytes()?.len();
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
    pub fn write_body_range<'a, B: IoBuffer>(
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
                        BufferRange::new(buffer, offset..range.end)?,
                        connection,
                        scope,
                    )
                    .await?;
                if completion.bytes == 0 || completion.bytes > range.end - offset {
                    return Err(Error::Io);
                }
                offset += completion.bytes;
                buffer = completion.buffer.into_inner();
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
        if body > self.codec.body_limit() {
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
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        http::{
            codec::{Codec, Header},
            pool::{Endpoint, HttpPool},
        },
        model::identity::RequestId,
    };
    use std::{
        future::Future,
        io::{Read, Write},
        net::TcpListener,
        os::unix::net::UnixStream,
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    fn setup() -> (Rc<Admission>, Rc<Reactor>, HttpIo, RequestScope) {
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = HttpIo::with_admission(
            reactor.clone(),
            Codec::new(4096, 8 * 1024 * 1024),
            admission.clone(),
        );
        let scope = RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10))
            .unwrap();
        (admission, reactor, io, scope)
    }
    fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let deadline = Instant::now() + Duration::from_secs(12);
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(
                Instant::now() < deadline,
                "HTTP operation failed to make progress"
            );
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    fn request(method: &str) -> MessageHead {
        MessageHead {
            start: StartLine::Request {
                method: method.into(),
                target: "/test".into(),
            },
            headers: vec![Header {
                name: "Host".into(),
                value: b"localhost".to_vec(),
            }],
        }
    }
    fn response(length: usize) -> MessageHead {
        MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header {
                name: "Content-Length".into(),
                value: length.to_string().into_bytes(),
            }],
        }
    }
    fn drain(reactor: &Reactor) {
        drive(reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.in_flight(), 0);
    }
    #[test]
    fn real_socket_fragmentation_read_ahead_and_owned_ranges() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        let thread = std::thread::spawn(move || {
            peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Len").unwrap();
            std::thread::sleep(Duration::from_millis(5));
            peer.write_all(b"gth: 5\r\n\r\nhello").unwrap();
        });
        let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        let mut bytes = io.buffer(9).unwrap();
        bytes.bytes_mut().unwrap().fill(b'_');
        let mut connection = received.connection;
        let mut offset = 2;
        while offset < 7 {
            let completed = drive(
                &reactor,
                io.read_body_range(connection, bytes, offset..7, &scope),
            )
            .unwrap();
            offset += completed.bytes;
            bytes = completed.buffer;
            connection = completed.lease;
        }
        assert_eq!(bytes.bytes().unwrap(), b"__hello__");
        assert_eq!(connection.remaining_body(), Some(0));
        drop(connection);
        drop(bytes);
        thread.join().unwrap();
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn real_partial_sends_preserve_owned_subrange() {
        let (admission, reactor, io, scope) = setup();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        use std::os::fd::AsRawFd;
        let size: libc::c_int = 4096;
        // SAFETY: setsockopt synchronously reads this correctly sized integer.
        assert_eq!(
            unsafe {
                libc::setsockopt(
                    socket.as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_SNDBUF,
                    (&size as *const libc::c_int).cast(),
                    std::mem::size_of_val(&size) as _,
                )
            },
            0
        );
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        let length = 1024 * 1024;
        let thread = std::thread::spawn(move || {
            std::thread::sleep(Duration::from_millis(10));
            let mut received = Vec::new();
            peer.read_to_end(&mut received).unwrap();
            let start = received.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
            assert_eq!(received.len() - start, length);
            assert!(received[start..].iter().all(|b| *b == 91));
        });
        let sent = drive(&reactor, io.send_head(connection, response(length), &scope)).unwrap();
        let mut bytes = io.buffer(length + 4).unwrap();
        bytes.bytes_mut().unwrap()[2..length + 2].fill(91);
        let completed = drive(
            &reactor,
            io.write_body_range(sent.connection, bytes, 2..length + 2, &scope),
        )
        .unwrap();
        assert_eq!(completed.bytes, length);
        drop(completed);
        drain(&reactor);
        thread.join().unwrap();
    }
    #[test]
    fn tcp_pool_reuses_only_finished_exchanges_and_enforces_capacity() {
        let (admission, reactor, io, scope) = setup();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let thread = std::thread::spawn(move || {
            let (mut socket, _) = listener.accept().unwrap();
            for _ in 0..2 {
                let mut bytes = Vec::new();
                while !bytes.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    socket.read_exact(&mut byte).unwrap();
                    bytes.push(byte[0]);
                }
                socket
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                    .unwrap();
            }
        });
        for _ in 0..2 {
            let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
            assert!(matches!(
                drive(&reactor, pool.checkout(&endpoint, &scope)),
                Err(Error::Overloaded)
            ));
            let received = drive(
                &reactor,
                io.exchange_head(connection, request("GET"), &scope),
            )
            .unwrap();
            let mut completed =
                drive(&reactor, io.collect_body(received.connection, 2, &scope)).unwrap();
            assert_eq!(completed.buffer.bytes().unwrap(), b"ok");
            completed.lease.finish_exchange().unwrap();
            assert!(completed.lease.is_reusable());
            drop(completed);
        }
        thread.join().unwrap();
        pool.close();
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn head_has_no_body_even_with_large_representation_length() {
        let (admission, reactor, io, scope) = setup();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        let thread = std::thread::spawn(move || {
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                peer.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 999999999\r\n\r\n")
                .unwrap();
        });
        let mut received = drive(
            &reactor,
            io.exchange_head(connection, request("HEAD"), &scope),
        )
        .unwrap();
        assert_eq!(received.connection.remaining_body(), Some(0));
        received.connection.finish_exchange().unwrap();
        thread.join().unwrap();
    }
    #[test]
    fn truncation_and_future_drop_do_not_recycle_live_buffers() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort")
            .unwrap();
        drop(peer);
        let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        assert!(matches!(
            drive(&reactor, io.collect_body(received.connection, 9, &scope)),
            Err(Error::Io)
        ));
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        let mut future = io.receive_head(connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        drop(future);
        scope.cancel().unwrap();
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn deadline_expires_without_peer_traffic() {
        let (admission, reactor, io, _) = setup();
        let scope = RequestScope::new(
            RequestId([2; 16]),
            Instant::now() + Duration::from_millis(30),
        )
        .unwrap();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        assert!(matches!(
            drive(&reactor, io.receive_head(connection, &scope)),
            Err(Error::DeadlineExceeded)
        ));
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn unix_pool_reconnects_after_unread_response_and_bounds_endpoints() {
        use std::os::unix::net::UnixListener;
        let (admission, reactor, io, scope) = setup();
        let mut nonce = [0; 8];
        getrandom::getrandom(&mut nonce).unwrap();
        let path = std::path::PathBuf::from(format!(
            "/tmp/opencode/racer-http-{}-{}.sock",
            std::process::id(),
            u64::from_ne_bytes(nonce)
        ));
        let listener = UnixListener::bind(&path).unwrap();
        let endpoint = Endpoint::Unix(path.clone());
        let pool = HttpPool::with_limits(reactor.clone(), admission.clone(), 1, 1, Duration::ZERO);
        let thread = std::thread::spawn(move || {
            for _ in 0..2 {
                let (mut socket, _) = listener.accept().unwrap();
                let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata");
                let mut sink = Vec::new();
                let _ = socket.read_to_end(&mut sink);
            }
        });
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        assert_eq!(
            received.connection.finish_exchange(),
            Err(Error::InvalidRequest)
        );
        drop(received);
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        let other = Endpoint::Peer("127.0.0.1:1".into());
        assert!(matches!(
            drive(&reactor, pool.checkout(&other, &scope)),
            Err(Error::Overloaded)
        ));
        pool.invalidate(&endpoint);
        drop(connection);
        pool.expire_idle();
        pool.close();
        thread.join().unwrap();
        std::fs::remove_file(path).unwrap();
        assert!(matches!(
            drive(&reactor, pool.checkout(&endpoint, &scope)),
            Err(Error::Unavailable)
        ));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn read_ahead_erases_credentials_and_endpoint_limit_is_enforced() {
        let (admission, reactor, io, scope) = setup();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(
            b"POST / HTTP/1.1\r\nAuthorization: secret\r\nContent-Length: 4\r\n\r\nbody",
        )
        .unwrap();
        let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
        let (ahead, range) = received
            .connection
            .read_ahead
            .as_ref()
            .expect("socket supplied head and body together");
        assert!(ahead.bytes[..range.start].iter().all(|b| *b == 0));
        assert_eq!(&ahead.bytes[range.clone()], b"body");
        drop(received);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        peer.write_all(b"GET / HTTP/1.1\r\nAuthorization: too-large\r\n\r\n")
            .unwrap();
        assert!(matches!(
            drive(&reactor, io.receive_head_limited(connection, &scope, 20)),
            Err(Error::HeaderTooLarge)
        ));
    }
    #[test]
    fn bodyless_and_malformed_response_framing_is_explicit() {
        let (_, _, io, _) = setup();
        assert_eq!(io.framing(&response(usize::MAX), true), Ok(0));
        assert!(io.framing(&response(usize::MAX), false).is_err());
        assert_eq!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status: 204 },
                    headers: vec![]
                },
                false
            ),
            Ok(0)
        );
        assert!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status: 200 },
                    headers: vec![]
                },
                false
            )
            .is_err()
        );
        assert!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status: 101 },
                    headers: vec![]
                },
                false
            )
            .is_err()
        );
    }
    #[test]
    fn dropped_connect_retains_slot_until_fd_fence_then_releases_quota() {
        let (admission, reactor, _, scope) = setup();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let mut future = pool.checkout(&endpoint, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        drop(future);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert!(matches!(
            drive(&reactor, pool.checkout(&endpoint, &scope)),
            Err(Error::Overloaded)
        ));
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn dropped_checkout_and_destroyed_pool_release_quota_only_after_connect_fence() {
        for (cancel_before_drop, submit_before_drop) in
            [(false, false), (true, false), (true, true)]
        {
            let (admission, reactor, _, scope) = setup();
            reactor.init().unwrap();
            let baseline = admission.used(ResourceClass::RequestContext);
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
            let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
            let mut future = pool.checkout(&endpoint, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            if submit_before_drop {
                reactor.poll_budgeted(32).unwrap();
            }
            if cancel_before_drop {
                scope.cancel().unwrap();
            }
            drop(future);
            drop(pool);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert_eq!(reactor.in_flight(), 1);
            drain(&reactor);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
            // No pool or pool sweep exists to release this charge. Prove it
            // returned to admission rather than being intentionally leaked.
            let charge = admission
                .reserve(
                    None,
                    ResourceClass::Connection,
                    admission.limits().client_connections.get(),
                )
                .unwrap();
            drop(charge);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
    #[test]
    fn cancelled_receive_retains_resources_until_completion_and_reports_cancelled() {
        let (admission, reactor, io, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        let mut future = io.receive_head(connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert!(matches!(drive(&reactor, future), Err(Error::Cancelled)));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }
    #[test]
    fn raw_request_head_errors_return_fenced_socket_for_empty_400_and_431() {
        for oversized in [false, true] {
            let (admission, reactor, _, scope) = setup();
            let io = HttpIo::with_admission(
                reactor.clone(),
                Codec::new(32 * 1024, 1024),
                admission.clone(),
            );
            reactor.init().unwrap();
            let baseline = admission.used(ResourceClass::RequestContext);
            let (socket, mut peer) = UnixStream::pair().unwrap();
            peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
            let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
            let raw = if oversized {
                let mut raw = b"GET / HTTP/1.1\r\nAuthorization: ".to_vec();
                raw.resize(super::super::codec::MAX_HEAD_BYTES, b'x');
                raw
            } else {
                b"GET / HTTP/1.1\r\nAuthorization:secret\r\nContent-Length: 4\r\n\r\nbody".to_vec()
            };
            peer.write_all(&raw).unwrap();
            let expected = if oversized {
                Error::HeaderTooLarge
            } else {
                Error::InvalidRequest
            };
            let mut outcome = drive(
                &reactor,
                io.receive_request_head_limited(connection, &scope, 32 * 1024),
            )
            .unwrap();
            assert!(matches!(outcome.value, Err(error) if error == expected));
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
            assert!(outcome.connection.read_ahead.is_none());
            assert!(!outcome.connection.is_reusable());
            assert_eq!(
                outcome.connection.finish_exchange(),
                Err(Error::InvalidRequest)
            );
            let status = if oversized { 431 } else { 400 };
            let mut head = response(0);
            head.start = StartLine::Response { status };
            head.headers.push(Header {
                name: "Connection".into(),
                value: b"close".to_vec(),
            });
            let mut sent = drive(&reactor, io.send_head(outcome.connection, head, &scope)).unwrap();
            assert!(!sent.connection.is_reusable());
            assert_eq!(
                sent.connection.finish_exchange(),
                Err(Error::InvalidRequest)
            );
            let expected =
                format!("HTTP/1.1 {status} \r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
            let mut wire = vec![0; expected.len()];
            peer.read_exact(&mut wire).unwrap();
            assert_eq!(wire, expected.as_bytes());
            drop(sent);
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }
}
