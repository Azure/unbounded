//! Bounded, explicitly polled diagnostic HTTP transport. Application policy is
//! supplied by a handler; no executor, listener binding, or admission is implicit.
//!
//! [`Server`] serves an already-bound descriptor on a borrowed [`Reactor`]. The
//! owner polls both service and reactor, at least every 10ms under queue pressure;
//! no background thread is created. The caller's budget authority supplies startup
//! submission capacity and an opaque memory charge. Buffers retain that charge and
//! the handler's connection guard until the completion fence, even if the service
//! future is abandoned. Drain the reactor before releasing worker resources.
//!
//! Each server permits one listener, four concurrent connections, 1024 request
//! bytes, 64KiB text buffers, and two-second text/header deadlines. An optional
//! binary producer has separate prepaid capacity and a bounded extended deadline. [`Scope`]
//! must narrow the caller's deadline while preserving cancellation and metadata.
//! Only HTTP/1.1 GET with one nonempty Host and no body is accepted; each connection
//! closes after one response. Headers are scrubbed before dispatch, never echoed.
//! The handler receives only the path and a bounded formatter, selects the response
//! kind, and observes fixed transport events. Application routes, trusted response
//! bodies, health, metric names, and connection admission remain with the caller.
use std::{
    cell::Cell,
    fmt::{self, Write},
    ops::Range,
    rc::Rc,
    task::Poll,
    time::{Duration, Instant},
};
use uring_runtime::{
    Budget, Error, Operation, Result, environment,
    reactor::{IoBuffer, Reactor, SUBMISSION_BYTES, SubmissionCapacity, descriptor::Descriptor},
};
use zeroize::{Zeroize, Zeroizing};

/// Maximum live connections, including abandoned buffers awaiting their fence.
pub const MAX_CONNECTIONS: usize = 4;

/// Maximum bytes read while looking for the end of the request headers.
pub const MAX_REQUEST_BYTES: usize = 1024;

/// Fixed buffer capacity shared by request parsing and response formatting.
pub const MAX_RESPONSE_BYTES: usize = 64 * 1024;

/// Maximum lifetime of an exchange, further limited by the caller's deadline.
pub const CONNECTION_TIMEOUT: Duration = Duration::from_secs(2);

/// Buffers plus bounded service/future bookkeeping, acquired before serving.
pub const RESERVED_BYTES: usize = (MAX_CONNECTIONS + 1) * (MAX_RESPONSE_BYTES + SUBMISSION_BYTES);

/// Prepaid submissions for one accept and one operation per connection.
pub const CONTROL_SLOTS: usize = MAX_CONNECTIONS + 1;

/// Extra prepaid waits for one binary producer and its socket disconnect watch.
pub const BINARY_CONTROL_SLOTS: usize = 2;
/// One binary result, allocated only by the admitted producer.
pub const MAX_BINARY_BYTES: usize = 16 * 1024 * 1024;

/// Fixed errors selected by the application, never raw operating-system messages.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BinaryError {
    NotFound,
    BadRequest,
    Busy,
    Unavailable(&'static str),
}

/// Nonblocking producer. Drop must cancel; the owner survives through send fences.
pub trait BinarySession {
    fn completion_fd(&self) -> Result<Descriptor, BinaryError>;
    fn result(&self) -> Option<Result<Vec<u8>, BinaryError>>;
    fn cancel(&self);
}

/// A bounded capture duration and its admitted, type-erased producer.
pub struct BinaryResponse {
    pub duration: Duration,
    pub session: Box<dyn BinarySession>,
}

/// A runtime scope that can narrow the deadline for a single exchange.
pub trait Scope: uring_runtime::Scope {
    /// Narrow the deadline, preserving cancellation and caller metadata.
    fn with_deadline(&self, deadline: Instant) -> Self;
}

/// Status and body format selected by an application handler.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Response {
    /// Send the handler's text with a successful status.
    Text,

    /// Send the handler's text with the Prometheus exposition content type.
    Metrics,

    /// Discard handler output and send the fixed not-found body.
    NotFound,

    /// Send the handler's text with a service-unavailable status.
    Unavailable,
}

/// Fixed transport observations, without request headers or application data.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Event {
    /// A socket was accepted, before application admission is attempted.
    Accepted,

    /// Admission was denied or a fixed rejection response was selected.
    Rejected,

    /// An exchange failed for a reason other than deadline expiry.
    IoError,

    /// An exchange exceeded its narrowed deadline.
    Timeout,
}

/// Application admission, trusted response formatting, and event reporting.
pub trait Handler {
    /// Retained in reactor-owned buffers until the completion fence.
    type Connection: 'static;

    /// Returning None rejects only this connection, not the listener.
    fn connect(&self) -> Option<Self::Connection>;

    /// Write only trusted output. NotFound discards output and uses a fixed body.
    fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error>;

    /// None preserves the synchronous text route without allocating a session.
    fn binary(&self, _path: &str) -> Option<Result<BinaryResponse, BinaryError>> {
        None
    }

    /// Record a fixed event without retaining untrusted request content.
    fn observe(&self, event: Event);
}

/// Single-listener owner of prepaid transport resources.
pub struct Server {
    resources: Rc<Resources>,

    binary: Option<Rc<BinaryResources>>,

    serving: Cell<bool>,
}

struct BinaryResources {
    submissions: Rc<SubmissionCapacity>,
    _memory: Box<dyn std::any::Any>,
    active: Cell<bool>,
}

struct BinaryGuard {
    session: Box<dyn BinarySession>,
    resources: Rc<BinaryResources>,
}

impl Drop for BinaryGuard {
    fn drop(&mut self) {
        self.session.cancel();
        self.resources.active.set(false);
    }
}

struct CancelBinary(Rc<BinaryGuard>);

impl Drop for CancelBinary {
    fn drop(&mut self) {
        // Request stop immediately; fenced owners keep the busy lease afterward.
        self.0.session.cancel();
    }
}

/// Reservations retained by every connection buffer through its final fence.
struct Resources {
    _memory: Box<dyn std::any::Any>,

    submissions: Rc<SubmissionCapacity>,

    active: Cell<usize>,
}

impl Server {
    /// The caller reserves CONTROL_SLOTS submissions and RESERVED_BYTES memory
    /// (including submission bookkeeping) through its existing budget authority.
    pub fn new<C: 'static>(submissions: Rc<SubmissionCapacity>, memory: C) -> Self {
        Self {
            resources: Rc::new(Resources {
                _memory: Box::new(memory),
                submissions,
                active: Cell::new(0),
            }),
            serving: Cell::new(false),
            binary: None,
        }
    }

    /// Reserve binary capacity separately; ordinary text-only owners stay unchanged.
    pub fn with_binary<C: 'static>(
        mut self,
        submissions: Rc<SubmissionCapacity>,
        memory: C,
    ) -> Self {
        self.binary = Some(Rc::new(BinaryResources {
            submissions,
            _memory: Box::new(memory),
            active: Cell::new(false),
        }));
        self
    }

    /// Admit one connection and allocate its fixed, completion-owned buffer.
    fn buffer<H: Handler>(&self, handler: &H) -> Option<Buffer<H::Connection>> {
        if self.resources.active.get() >= MAX_CONNECTIONS {
            return None;
        }
        let connection = handler.connect()?;
        let bytes = vec![0; MAX_RESPONSE_BYTES];
        self.resources.active.set(self.resources.active.get() + 1);
        Some(Buffer {
            bytes,
            pending: 0..MAX_REQUEST_BYTES,
            resources: self.resources.clone(),
            _connection: connection,
            binary: None,
        })
    }

    /// Poll alongside the reactor, at least every 10ms during queue pressure.
    /// Dropping this future abandons work, not ownership: drain the reactor.
    pub fn serve<'a, S: Scope, B: Budget, H: Handler>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        listener: Descriptor,
        handler: &'a H,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error>
    where
        S::Error: PartialEq,
    {
        Box::pin(async move {
            scope.check()?;
            if self.serving.replace(true) {
                return Err(Error::InvalidConfiguration.into());
            }
            let _serving = Serving(&self.serving);
            listener.set_nonblocking()?;
            let listener = Rc::new(listener);
            let mut accepting = None;
            let mut connections: Vec<Operation<'_, (), S::Error>> =
                Vec::with_capacity(MAX_CONNECTIONS);
            let cancellation = scope.cancellation().map(|c| c.subscribe()).transpose()?;
            std::future::poll_fn(|cx| {
                if let Some(cancellation) = &cancellation {
                    cancellation.register(cx.waker());
                }
                scope.check()?;
                let mut index = 0;
                while index < connections.len() {
                    match connections[index].as_mut().poll(cx) {
                        Poll::Pending => index += 1,
                        Poll::Ready(result) => {
                            drop(connections.swap_remove(index));
                            if let Err(error) = result {
                                handler.observe(if error == Error::DeadlineExceeded.into() {
                                    Event::Timeout
                                } else {
                                    Event::IoError
                                });
                            }
                        }
                    }
                }
                if accepting.is_none() && self.resources.active.get() < MAX_CONNECTIONS {
                    accepting = Some(uring_runtime::drivers::retry_listener(scope, || {
                        reactor.accept_reserved(
                            listener.clone(),
                            Some(self.resources.submissions.clone()),
                            scope,
                        )
                    }));
                }
                if let Some(accept) = &mut accepting {
                    match accept.as_mut().poll(cx) {
                        Poll::Pending => (),
                        Poll::Ready(result) => {
                            accepting = None;
                            let fd = result?;
                            handler.observe(Event::Accepted);
                            if let Some(buffer) = self.buffer(handler) {
                                connections.push(self.exchange(
                                    reactor,
                                    handler,
                                    Rc::new(fd),
                                    buffer,
                                    scope,
                                ));
                            } else {
                                drop(fd);
                                handler.observe(Event::Rejected);
                            }
                            cx.waker().wake_by_ref();
                        }
                    }
                }
                Poll::Pending
            })
            .await
        })
    }

    /// Read one request, scrub it, and send one response under a shared deadline.
    fn exchange<'a, S: Scope, B: Budget, H: Handler>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        handler: &'a H,
        fd: Rc<Descriptor>,
        mut buffer: Buffer<H::Connection>,
        parent: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let deadline = environment::now()
                .checked_add(CONNECTION_TIMEOUT)
                .ok_or(Error::InvalidInput)?;
            let scope = parent.with_deadline(deadline);
            let route = loop {
                let completed = reactor
                    .recv_reserved(
                        fd.clone(),
                        buffer,
                        self.resources.submissions.clone(),
                        &scope,
                    )
                    .await?;
                buffer = completed.buffer;
                buffer.advance(completed.bytes)?;
                let used = buffer.pending.start;
                if let Some(end) = buffer.bytes[..used]
                    .windows(4)
                    .position(|part| part == b"\r\n\r\n")
                {
                    break parse(&buffer.bytes[..end + 4]);
                }
                if used == MAX_REQUEST_BYTES {
                    break Route::TooLarge;
                }
            };
            // Keep only the bounded path until dispatch; scrub all request headers
            // before formatting, including on handler or output failure.
            let mut path = Zeroizing::new([0u8; MAX_REQUEST_BYTES]);
            let route = match route {
                Route::Get(value) => {
                    path[..value.len()].copy_from_slice(value.as_bytes());
                    Route::Get(
                        std::str::from_utf8(&path[..value.len()]).expect("parsed UTF-8 path"),
                    )
                }
                Route::Method => Route::Method,
                Route::BadRequest => Route::BadRequest,
                Route::TooLarge => Route::TooLarge,
            };
            buffer.bytes.as_mut_slice().zeroize();
            if let Route::Get(value) = route
                && let Some(response) = handler.binary(value)
            {
                path.zeroize();
                return self
                    .binary_exchange(reactor, fd, buffer, parent, &scope, response)
                    .await;
            }
            let length = respond(handler, route, &mut buffer.bytes).map_err(|_| Error::Io)?;
            path.zeroize();
            buffer.pending = 0..length;
            while !buffer.pending.is_empty() {
                let completed = reactor
                    .send_reserved(
                        fd.clone(),
                        buffer,
                        self.resources.submissions.clone(),
                        &scope,
                    )
                    .await?;
                buffer = completed.buffer;
                buffer.advance(completed.bytes)?;
            }
            Ok(())
        })
    }

    async fn binary_exchange<S: Scope, B: Budget, C: 'static>(
        &self,
        reactor: &Reactor<S, B>,
        fd: Rc<Descriptor>,
        mut buffer: Buffer<C>,
        parent: &S,
        header_scope: &S,
        response: Result<BinaryResponse, BinaryError>,
    ) -> Result<(), S::Error> {
        let mut scope = header_scope.clone();
        let response = match response {
            Ok(response) => {
                if response.duration < Duration::from_secs(1)
                    || response.duration > Duration::from_secs(60)
                {
                    Err(BinaryError::BadRequest)
                } else if let Some(resources) = &self.binary {
                    if resources.active.replace(true) {
                        Err(BinaryError::Busy)
                    } else {
                        let guard = Rc::new(BinaryGuard {
                            session: response.session,
                            resources: resources.clone(),
                        });
                        let _cancel = CancelBinary(guard.clone());
                        buffer.binary = Some(guard.clone());
                        scope = parent.with_deadline(
                            environment::now()
                                .checked_add(response.duration + Duration::from_secs(15))
                                .ok_or(Error::InvalidInput)?,
                        );
                        match guard.session.completion_fd() {
                            Err(error) => Err(error),
                            Ok(completion) => {
                                let mut ready = reactor.readiness_reserved(
                                    Rc::new(completion),
                                    1,
                                    guard.clone(),
                                    resources.submissions.clone(),
                                    &scope,
                                );
                                // POLLHUP | POLLERR, deliberately not POLLRDHUP: shutdown(Write) is legal.
                                let mut disconnected = reactor.readiness_reserved(
                                    fd.clone(),
                                    0x18,
                                    guard.clone(),
                                    resources.submissions.clone(),
                                    &scope,
                                );
                                let mut half_closed = false;
                                std::future::poll_fn(|cx| {
                                    // Some kernels report RDHUP even when it was not requested.
                                    // Do not rearm that level-triggered event or reject a legal FIN.
                                    if !half_closed {
                                        if let Poll::Ready(result) = disconnected.as_mut().poll(cx)
                                        {
                                            match result {
                                                Err(error) => return Poll::Ready(Err(error)),
                                                Ok(flags) if flags & 0x18 != 0 => {
                                                    return Poll::Ready(Err(Error::Io.into()));
                                                }
                                                Ok(_) => half_closed = true,
                                            }
                                        }
                                    } else if fd.peer_disconnected() {
                                        return Poll::Ready(Err(Error::Io.into()));
                                    }
                                    ready.as_mut().poll(cx)
                                })
                                .await?;
                                // Notification must follow result publication; never spin on a broken producer.
                                guard
                                    .session
                                    .result()
                                    .unwrap_or(Err(BinaryError::Unavailable("backend_protocol")))
                            }
                        }
                    }
                } else {
                    Err(BinaryError::Unavailable("resource_limit"))
                }
            }
            Err(error) => Err(error),
        };
        let response = response.and_then(|bytes| {
            if bytes.len() > MAX_BINARY_BYTES {
                Err(BinaryError::Unavailable("resource_limit"))
            } else {
                Ok(bytes)
            }
        });
        let (status, body, binary) = match response {
            Ok(bytes) => ("200 OK", bytes, true),
            Err(BinaryError::NotFound) => ("404 Not Found", b"not found\n".to_vec(), false),
            Err(BinaryError::BadRequest) => ("400 Bad Request", b"invalid query\n".to_vec(), false),
            Err(BinaryError::Busy) => ("409 Conflict", b"busy\n".to_vec(), false),
            Err(BinaryError::Unavailable(code)) => (
                "503 Service Unavailable",
                format!("{code}\n").into_bytes(),
                false,
            ),
        };
        let content_type = if binary {
            "application/octet-stream"
        } else {
            "text/plain; charset=utf-8"
        };
        let mut head = Output {
            bytes: &mut buffer.bytes,
            used: 0,
        };
        write!(head, "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\nX-Content-Type-Options: nosniff\r\n", body.len()).map_err(|_| Error::Io)?;
        if binary {
            head.write_str("Content-Disposition: attachment; filename=\"profile.pb\"\r\n")
                .map_err(|_| Error::Io)?;
        }
        head.write_str("\r\n").map_err(|_| Error::Io)?;
        buffer.pending = 0..head.used;
        // Send the small head separately to avoid copying the 16MiB result on I/O.
        for body in [None, Some(body)] {
            if let Some(body) = body {
                buffer.bytes = body;
                buffer.pending = 0..buffer.bytes.len();
            }
            while !buffer.pending.is_empty() {
                let completed = reactor
                    .send_reserved(
                        fd.clone(),
                        buffer,
                        self.resources.submissions.clone(),
                        &scope,
                    )
                    .await?;
                buffer = completed.buffer;
                buffer.advance(completed.bytes)?;
            }
        }
        Ok(())
    }
}

/// Release the listener guard when serving returns or its future is dropped.
struct Serving<'a>(&'a Cell<bool>);

impl Drop for Serving<'_> {
    /// Permit a subsequent service future after this one releases its listener.
    fn drop(&mut self) {
        self.0.set(false);
    }
}

/// Stable allocation and guards moved into the reactor for each operation.
struct Buffer<C: 'static> {
    bytes: Vec<u8>,

    /// Unfilled request capacity while receiving, then unsent response bytes.
    pending: Range<usize>,

    resources: Rc<Resources>,

    _connection: C,

    binary: Option<Rc<BinaryGuard>>,
}

impl<C> Buffer<C> {
    /// Consume positive progress within the submitted range, for reads or writes.
    fn advance(&mut self, completed: usize) -> Result<()> {
        if completed == 0 || completed > self.pending.len() {
            return Err(Error::Io);
        }
        self.pending.start += completed;
        Ok(())
    }
}

impl<C> Drop for Buffer<C> {
    /// Scrub the allocation and release the connection slot after its fence.
    fn drop(&mut self) {
        self.bytes.as_mut_slice().zeroize();
        self.resources.active.set(self.resources.active.get() - 1);
    }
}

// SAFETY: private fixed Vec retains its reservation and is not aliased.
unsafe impl<C: 'static> IoBuffer for Buffer<C> {
    type Error = Error;

    /// Expose only the remaining range of the current operation.
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes[self.pending.clone()])
    }

    /// Receive only into the remaining range without resizing the allocation.
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes[self.pending.clone()])
    }
}

/// Parsed path or fixed rejection selected before request bytes are scrubbed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Route<'a> {
    Get(&'a str),

    Method,

    BadRequest,

    TooLarge,
}

/// Accept exactly the supported HTTP version, headers, and bodyless GET method.
fn parse(bytes: &[u8]) -> Route<'_> {
    let mut headers = [httparse::EMPTY_HEADER; 16];
    let mut request = httparse::Request::new(&mut headers);
    if !matches!(request.parse(bytes), Ok(httparse::Status::Complete(_)))
        || request.version != Some(1)
    {
        return Route::BadRequest;
    }
    let mut host = false;
    let mut length = false;
    for header in request.headers.iter() {
        if header.name.eq_ignore_ascii_case("host") {
            if host || header.value.is_empty() {
                return Route::BadRequest;
            }
            host = true;
        }
        if header.name.eq_ignore_ascii_case("transfer-encoding") {
            return Route::BadRequest;
        }
        if header.name.eq_ignore_ascii_case("content-length") {
            if length || header.value != b"0" {
                return Route::BadRequest;
            }
            length = true;
        }
    }
    if !host {
        return Route::BadRequest;
    }
    if request.method != Some("GET") {
        return Route::Method;
    }
    request.path.map(Route::Get).unwrap_or(Route::BadRequest)
}

/// A formatter that fails rather than growing beyond the caller's slice.
struct Output<'a> {
    bytes: &'a mut [u8],

    used: usize,
}

impl Write for Output<'_> {
    /// Append all bytes or fail without modifying the written length.
    fn write_str(&mut self, text: &str) -> fmt::Result {
        let end = self.used.checked_add(text.len()).ok_or(fmt::Error)?;
        self.bytes
            .get_mut(self.used..end)
            .ok_or(fmt::Error)?
            .copy_from_slice(text.as_bytes());
        self.used = end;
        Ok(())
    }
}

/// Format a complete response, replacing rejected handler output with fixed text.
fn respond(
    handler: &impl Handler,
    route: Route<'_>,
    bytes: &mut [u8],
) -> Result<usize, fmt::Error> {
    const HEAD: usize = 256;
    const TEXT: &str = "text/plain; charset=utf-8";
    const METRICS: &str = "text/plain; version=0.0.4; charset=utf-8";
    if bytes.len() < HEAD {
        return Err(fmt::Error);
    }
    let (header, body) = bytes.split_at_mut(HEAD);
    let mut output = Output {
        bytes: body,
        used: 0,
    };
    let (status, content_type, fixed) = match route {
        Route::Get(path) => match handler.get(path, &mut output)? {
            Response::Text => ("200 OK", TEXT, None),
            Response::Metrics => ("200 OK", METRICS, None),
            Response::Unavailable => ("503 Service Unavailable", TEXT, None),
            Response::NotFound => ("404 Not Found", TEXT, Some("not found\n")),
        },
        Route::Method => ("405 Method Not Allowed", TEXT, Some("method not allowed\n")),
        Route::BadRequest => ("400 Bad Request", TEXT, Some("bad request\n")),
        Route::TooLarge => (
            "431 Request Header Fields Too Large",
            TEXT,
            Some("headers too large\n"),
        ),
    };
    if let Some(body) = fixed {
        handler.observe(Event::Rejected);
        output.bytes.zeroize();
        output.used = 0;
        output.write_str(body)?;
    }
    let body_len = output.used;
    let mut output = Output {
        bytes: header,
        used: 0,
    };
    write!(
        output,
        "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {body_len}\r\nConnection: close\r\nCache-Control: no-store\r\n"
    )?;
    if route == Route::Method {
        output.write_str("Allow: GET\r\n")?;
    }
    output.write_str("\r\n")?;
    let header_len = output.used;
    bytes.copy_within(HEAD..HEAD + body_len, header_len);
    Ok(header_len + body_len)
}

/// Parser, wire-format, completion-range, and resource-lifetime regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::os::fd::OwnedFd;

    struct Producer {
        fd: OwnedFd,
        body_bytes: usize,
        ready: Rc<Cell<bool>>,
        live: Rc<Cell<usize>>,
        cancelled: Rc<Cell<bool>>,
    }

    impl BinarySession for Producer {
        fn completion_fd(&self) -> Result<Descriptor, BinaryError> {
            Ok(self.fd.try_clone().unwrap().into())
        }
        fn result(&self) -> Option<Result<Vec<u8>, BinaryError>> {
            self.ready.replace(false).then(|| {
                Ok(if self.body_bytes == 4 {
                    vec![0, 255, 10, 13]
                } else {
                    vec![7; self.body_bytes]
                })
            })
        }
        fn cancel(&self) {
            self.cancelled.set(true);
        }
    }

    impl Drop for Producer {
        fn drop(&mut self) {
            self.cancel();
            self.live.set(self.live.get() - 1);
        }
    }

    struct BinaryRoutes {
        routes: Routes,
        producer: RefCell<Option<Producer>>,
    }

    struct SimProducer {
        fd: RefCell<Option<Descriptor>>,
        live: Rc<Cell<usize>>,
        cancelled: Rc<Cell<bool>>,
    }
    impl BinarySession for SimProducer {
        fn completion_fd(&self) -> Result<Descriptor, BinaryError> {
            Ok(self.fd.borrow_mut().take().unwrap())
        }
        fn result(&self) -> Option<Result<Vec<u8>, BinaryError>> {
            Some(Ok(vec![7; MAX_BINARY_BYTES]))
        }
        fn cancel(&self) {
            self.cancelled.set(true);
        }
    }
    impl Drop for SimProducer {
        fn drop(&mut self) {
            self.live.set(self.live.get() - 1);
        }
    }

    #[test]
    fn binary_small_window_abandonment_retains_output_and_session_until_fence() {
        use std::task::{Context, Waker};
        use uring_runtime::reactor::simulation::Simulation;
        let sim = Simulation::new();
        sim.set_stream_capacity(32).unwrap();
        let _sim = sim.enter();
        let reactor = Reactor::<TestScope, ()>::new(12, ());
        let submissions = reactor
            .reserve_submissions(CONTROL_SLOTS + BINARY_CONTROL_SLOTS, ())
            .unwrap();
        let owner = Server::new(submissions.clone(), ()).with_binary(submissions, ());
        let scope = TestScope {
            deadline: environment::now() + Duration::from_secs(100),
            cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
        };
        let (fd, peer) = sim.socket_pair();
        let (completion, signal) = sim.socket_pair();
        signal.try_send(&[1]).unwrap();
        let live = Rc::new(Cell::new(1));
        let cancelled = Rc::new(Cell::new(false));
        let response = BinaryResponse {
            duration: Duration::from_secs(1),
            session: Box::new(SimProducer {
                fd: RefCell::new(Some(completion)),
                live: live.clone(),
                cancelled: cancelled.clone(),
            }),
        };
        let mut task = Box::pin(owner.binary_exchange(
            &reactor,
            Rc::new(fd),
            owner.buffer(&Routes::default()).unwrap(),
            &scope,
            &scope,
            Ok(response),
        ));
        let mut cx = Context::from_waker(Waker::noop());
        let mut header = Vec::new();
        for _ in 0..256 {
            assert!(task.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(32).unwrap();
            let mut bytes = [0; 32];
            if let Ok(n) = peer.try_recv(&mut bytes) {
                header.extend_from_slice(&bytes[..n]);
            }
            if header.windows(4).any(|w| w == b"\r\n\r\n") {
                break;
            }
        }
        assert!(header.windows(4).any(|w| w == b"\r\n\r\n"));
        for _ in 0..16 {
            assert!(task.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(32).unwrap();
        }
        assert_eq!(live.get(), 1);
        drop(task);
        assert_eq!(live.get(), 1);
        assert!(owner.binary.as_ref().unwrap().active.get());
        let mut drain = reactor.drain();
        let mut done = false;
        for _ in 0..32 {
            reactor.poll_budgeted(32).unwrap();
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                done = true;
                break;
            }
        }
        assert!(done);
        assert_eq!(live.get(), 0);
        assert!(cancelled.get());
        assert_eq!(owner.resources.active.get(), 0);
    }

    impl Handler for BinaryRoutes {
        type Connection = ();
        fn connect(&self) -> Option<()> {
            Some(())
        }
        fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error> {
            self.routes.get(path, out)
        }
        fn observe(&self, event: Event) {
            self.routes.observe(event);
        }
        fn binary(&self, path: &str) -> Option<Result<BinaryResponse, BinaryError>> {
            if path != "/profile" {
                return None;
            }
            Some(
                self.producer
                    .borrow_mut()
                    .take()
                    .map(|session| BinaryResponse {
                        duration: Duration::from_secs(30),
                        session: Box::new(session),
                    })
                    .ok_or(BinaryError::Busy),
            )
        }
    }

    #[test]
    fn binary_wait_preserves_text_progress_half_close_and_fenced_ownership() {
        use std::{
            io::{Read, Write as _},
            net::{Shutdown, TcpListener, TcpStream},
            os::unix::net::UnixStream,
            task::{Context, Waker},
        };
        use uring_runtime::environment::SimulationClock;
        for outcome in [
            "success",
            "timeout",
            "disconnect",
            "abandon",
            "parent",
            "fin",
            "protocol",
            "backpressure",
        ] {
            let now = Instant::now();
            let clock = SimulationClock::new_at(1, now, std::time::SystemTime::UNIX_EPOCH);
            let _clock = clock.environment(0).enter();
            let reactor = Reactor::<TestScope, ()>::new(12, ());
            let submissions = reactor
                .reserve_submissions(CONTROL_SLOTS + BINARY_CONTROL_SLOTS, ())
                .unwrap();
            let binary = submissions.clone();
            let owner = Server::new(submissions, ()).with_binary(binary, ());
            let scope = TestScope {
                deadline: now + Duration::from_secs(if outcome == "parent" { 4 } else { 100 }),
                cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
            };
            let (completion, mut signal) = UnixStream::pair().unwrap();
            completion.set_nonblocking(true).unwrap();
            let ready = Rc::new(Cell::new(false));
            let live = Rc::new(Cell::new(1));
            let cancelled = Rc::new(Cell::new(false));
            let handler = BinaryRoutes {
                routes: Routes::default(),
                producer: RefCell::new(Some(Producer {
                    fd: completion.into(),
                    body_bytes: if outcome == "backpressure" {
                        MAX_BINARY_BYTES
                    } else {
                        4
                    },
                    ready: ready.clone(),
                    live: live.clone(),
                    cancelled: cancelled.clone(),
                })),
            };
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let mut client = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (mut socket, _) = listener.accept().unwrap();
            if outcome == "disconnect" {
                // Closing a TCP client with unread server data sends RST.
                socket.write_all(b"unread").unwrap();
            }
            socket.set_nonblocking(true).unwrap();
            client
                .write_all(b"GET /profile HTTP/1.1\r\nHost: local\r\n\r\n")
                .unwrap();
            client.set_nonblocking(true).unwrap();
            let mut exchange = owner.exchange(
                &reactor,
                &handler,
                Rc::new(OwnedFd::from(socket).into()),
                owner.buffer(&handler).unwrap(),
                &scope,
            );
            let mut cx = Context::from_waker(Waker::noop());
            for _ in 0..16 {
                assert!(exchange.as_mut().poll(&mut cx).is_pending());
                reactor.poll_budgeted(32).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            assert!(handler.producer.borrow().is_none());
            clock.advance(Duration::from_secs(3));
            assert!(exchange.as_mut().poll(&mut cx).is_pending());
            assert_eq!(live.get(), 1);
            let (socket, mut busy) = UnixStream::pair().unwrap();
            socket.set_nonblocking(true).unwrap();
            busy.write_all(b"GET /profile HTTP/1.1\r\nHost: local\r\n\r\n")
                .unwrap();
            busy.set_nonblocking(true).unwrap();
            let mut rejected = owner.exchange(
                &reactor,
                &handler,
                Rc::new(OwnedFd::from(socket).into()),
                owner.buffer(&handler).unwrap(),
                &scope,
            );
            let until = Instant::now() + Duration::from_secs(2);
            loop {
                if let Poll::Ready(result) = rejected.as_mut().poll(&mut cx) {
                    result.unwrap();
                    break;
                }
                assert!(Instant::now() < until);
                reactor.poll_budgeted(32).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            let mut bytes = [0; 512];
            let n = busy.read(&mut bytes).unwrap();
            assert!(bytes[..n].starts_with(b"HTTP/1.1 409 Conflict\r\n"));
            assert!(!cancelled.get());
            drop(rejected);
            // Saturate ordinary capacity: both capture waits must use prepaid slots.
            let (idle, _idle_peer) = UnixStream::pair().unwrap();
            idle.set_nonblocking(true).unwrap();
            let idle = Rc::new(Descriptor::from(OwnedFd::from(idle)));
            let mut pressure: Vec<_> = (0..5)
                .map(|_| reactor.readiness(idle.clone(), 1, &scope))
                .collect();
            for wait in &mut pressure {
                assert!(wait.as_mut().poll(&mut cx).is_pending());
            }
            // Three ordinary HTTP exchanges still fit while the profile owns slot four.
            for _ in 0..3 {
                let (socket, mut text) = UnixStream::pair().unwrap();
                socket.set_nonblocking(true).unwrap();
                text.write_all(b"GET /live HTTP/1.1\r\nHost: local\r\n\r\n")
                    .unwrap();
                text.set_nonblocking(true).unwrap();
                let mut request = owner.exchange(
                    &reactor,
                    &handler,
                    Rc::new(OwnedFd::from(socket).into()),
                    owner.buffer(&handler).unwrap(),
                    &scope,
                );
                let until = Instant::now() + Duration::from_secs(2);
                loop {
                    if let Poll::Ready(result) = request.as_mut().poll(&mut cx) {
                        result.unwrap();
                        break;
                    }
                    assert!(Instant::now() < until);
                    reactor.poll_budgeted(32).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                }
                let mut bytes = [0; 256];
                let n = text.read(&mut bytes).unwrap();
                assert!(bytes[..n].ends_with(b"\r\n\r\nok\n"));
            }
            match outcome {
                "success" | "backpressure" => {
                    client.shutdown(Shutdown::Write).unwrap();
                    for _ in 0..4 {
                        reactor.poll_budgeted(32).unwrap();
                        let result = exchange.as_mut().poll(&mut cx);
                        assert!(result.is_pending(), "half-close: {result:?}");
                    }
                    assert!(!cancelled.get());
                    ready.set(true);
                    signal.write_all(&[1]).unwrap();
                }
                "timeout" => clock.advance(Duration::from_secs(43)),
                "parent" => clock.advance(Duration::from_secs(1)),
                "disconnect" => {
                    drop(client);
                    client = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
                }
                "fin" => {
                    drop(client);
                    client = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
                    clock.advance(Duration::from_secs(43));
                }
                "protocol" => {
                    signal.write_all(&[1]).unwrap();
                }
                "abandon" => (),
                _ => unreachable!(),
            }
            if outcome == "backpressure" {
                for _ in 0..32 {
                    assert!(exchange.as_mut().poll(&mut cx).is_pending());
                    reactor.poll_budgeted(32).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                }
                assert_eq!(live.get(), 1);
                assert!(owner.binary.as_ref().unwrap().active.get());
            } else if outcome != "abandon" {
                let until = Instant::now() + Duration::from_secs(2);
                loop {
                    if let Poll::Ready(result) = exchange.as_mut().poll(&mut cx) {
                        match outcome {
                            "success" | "protocol" => assert_eq!(result, Ok(())),
                            "timeout" | "parent" | "fin" => {
                                assert_eq!(result, Err(Error::DeadlineExceeded))
                            }
                            _ => assert_eq!(result, Err(Error::Io)),
                        }
                        break;
                    }
                    assert!(Instant::now() < until, "{outcome}");
                    reactor.poll_budgeted(32).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                }
                if outcome == "success" {
                    let mut bytes = [0; 512];
                    let n = client.read(&mut bytes).unwrap();
                    assert!(bytes[..n].ends_with(&[0, 255, 10, 13]));
                    let text = String::from_utf8_lossy(&bytes[..n - 4]);
                    assert!(text.contains("Content-Length: 4\r\n"));
                    assert!(text.contains("X-Content-Type-Options: nosniff\r\n"));
                    assert!(text.contains("Cache-Control: no-store\r\n"));
                }
                if outcome == "protocol" {
                    let mut bytes = [0; 512];
                    let n = client.read(&mut bytes).unwrap();
                    assert!(bytes[..n].ends_with(b"backend_protocol\n"));
                }
            }
            drop(exchange);
            if outcome == "abandon" || outcome == "backpressure" {
                assert!(cancelled.get());
                assert_eq!(live.get(), 1, "readiness leases must survive until fenced");
                assert!(owner.binary.as_ref().unwrap().active.get());
            }
            drop(pressure);
            let mut drain = reactor.drain();
            let until = Instant::now() + Duration::from_secs(2);
            loop {
                if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                    result.unwrap();
                    break;
                }
                assert!(Instant::now() < until);
                reactor.poll_budgeted(32).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            assert_eq!(live.get(), 0);
            assert!(cancelled.get());
            assert!(!owner.binary.as_ref().unwrap().active.get());
            assert_eq!(owner.resources.active.get(), 0);
        }
    }

    /// Reserve runtime bookkeeping for every prepaid slot in addition to buffers.
    #[test]
    fn reservation_covers_buffers_and_runtime_submission_bookkeeping() {
        let buffer_bytes = (MAX_CONNECTIONS + 1) * MAX_RESPONSE_BYTES;
        assert_eq!(
            RESERVED_BYTES - buffer_bytes,
            CONTROL_SLOTS * uring_runtime::reactor::SUBMISSION_BYTES
        );
    }

    /// Trusted routes with recorded transport observations.
    #[derive(Default)]
    struct Routes(RefCell<Vec<Event>>);

    impl Handler for Routes {
        type Connection = ();

        /// Admit every test connection without an extra guard.
        fn connect(&self) -> Option<()> {
            Some(())
        }

        /// Supply each response kind, oversized output, or discarded private text.
        fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error> {
            match path {
                "/live" => {
                    out.write_str("ok\n")?;
                    Ok(Response::Text)
                }
                "/stats" => {
                    out.write_str("example_total 1\n")?;
                    Ok(Response::Metrics)
                }
                "/ready" => {
                    out.write_str("not ready\n")?;
                    Ok(Response::Unavailable)
                }
                "/overflow" => {
                    for _ in 0..MAX_RESPONSE_BYTES {
                        out.write_char('x')?;
                    }
                    Ok(Response::Text)
                }
                _ => {
                    out.write_str("discarded secret")?;
                    Ok(Response::NotFound)
                }
            }
        }

        /// Retain events in order for exact assertions.
        fn observe(&self, event: Event) {
            self.0.borrow_mut().push(event);
        }
    }

    /// Reject unsupported versions, headers, bodies, and methods without relaxing parsing.
    #[test]
    fn fixed_parser_restrictions_and_header_limit() {
        for request in [
            "GET /live HTTP/1.0\r\n\r\n",
            "GET /live HTTP/1.1\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost:\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost: a\r\nhOsT: b\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost: a\r\nTransfer-Encoding: chunked\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 1\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\ncontent-length: 0\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 00\r\n\r\n",
            "GET /live HTTP/1.1\r\nHost: a\r\n",
        ] {
            assert_eq!(parse(request.as_bytes()), Route::BadRequest, "{request:?}");
        }
        let many = format!(
            "GET /live HTTP/1.1\r\nHost: a\r\n{}\r\n",
            "X: a\r\n".repeat(16)
        );
        assert_eq!(parse(many.as_bytes()), Route::BadRequest);
        assert_eq!(
            parse(b"POST /debug/pprof/profile?seconds=1 HTTP/1.1\r\nHost: a\r\n\r\n"),
            Route::Method
        );
        assert_eq!(
            parse(b"POST /live HTTP/1.1\r\nHost: a\r\n\r\n"),
            Route::Method
        );
        assert_eq!(
            parse(
                b"GET /live HTTP/1.1\r\nHost: a\r\nContent-Length: 0\r\nAuthorization: secret\r\n\r\n"
            ),
            Route::Get("/live")
        );
    }

    /// Preserve exact response bytes, fixed rejection bodies, and formatting bounds.
    #[test]
    fn precise_wire_formats_redaction_and_output_bounds() {
        let handler = Routes::default();
        for (route, status, body, metrics, allow) in [
            (Route::Get("/live"), "200 OK", "ok\n", false, false),
            (
                Route::Get("/stats"),
                "200 OK",
                "example_total 1\n",
                true,
                false,
            ),
            (
                Route::Get("/ready"),
                "503 Service Unavailable",
                "not ready\n",
                false,
                false,
            ),
            (
                Route::Get("/secret"),
                "404 Not Found",
                "not found\n",
                false,
                false,
            ),
            (
                Route::Method,
                "405 Method Not Allowed",
                "method not allowed\n",
                false,
                true,
            ),
            (
                Route::BadRequest,
                "400 Bad Request",
                "bad request\n",
                false,
                false,
            ),
            (
                Route::TooLarge,
                "431 Request Header Fields Too Large",
                "headers too large\n",
                false,
                false,
            ),
        ] {
            let mut bytes = vec![0; MAX_RESPONSE_BYTES];
            let used = respond(&handler, route, &mut bytes).unwrap();
            let content_type = if metrics {
                "text/plain; version=0.0.4; charset=utf-8"
            } else {
                "text/plain; charset=utf-8"
            };
            let allow = if allow { "Allow: GET\r\n" } else { "" };
            assert_eq!(
                std::str::from_utf8(&bytes[..used]).unwrap(),
                format!(
                    "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\nCache-Control: no-store\r\n{allow}\r\n{body}",
                    body.len()
                )
            );
            assert!(!bytes.windows(6).any(|w| w == b"secret"));
        }
        assert_eq!(*handler.0.borrow(), vec![Event::Rejected; 4]);
        for size in [0, 255, 256, 257] {
            assert!(respond(&handler, Route::Get("/live"), &mut vec![0; size]).is_err());
        }
        assert!(
            respond(
                &handler,
                Route::Get("/overflow"),
                &mut vec![0; MAX_RESPONSE_BYTES]
            )
            .is_err()
        );
    }

    /// Exercise partial progress and invalid completion sizes against the same range.
    #[test]
    fn completion_progress_preserves_bounds_on_failure() {
        let reactor = Reactor::<TestScope, ()>::new(8, ());
        let submissions = reactor.reserve_submissions(CONTROL_SLOTS, ()).unwrap();
        let owner = Server::new(submissions, ());
        let mut buffer = owner.buffer(&Routes::default()).unwrap();
        assert_eq!(buffer.bytes().unwrap().len(), MAX_REQUEST_BYTES);
        assert_eq!(buffer.advance(0), Err(Error::Io));
        assert_eq!(buffer.advance(MAX_REQUEST_BYTES + 1), Err(Error::Io));
        assert_eq!(buffer.pending, 0..MAX_REQUEST_BYTES);
        buffer.advance(MAX_REQUEST_BYTES - 1).unwrap();
        assert_eq!(buffer.bytes_mut().unwrap().len(), 1);
        assert_eq!(buffer.advance(2), Err(Error::Io));
        assert_eq!(buffer.pending, MAX_REQUEST_BYTES - 1..MAX_REQUEST_BYTES);
        buffer.advance(1).unwrap();
        assert!(buffer.pending.is_empty());
        assert_eq!(buffer.advance(1), Err(Error::Io));
        assert_eq!(buffer.advance(0), Err(Error::Io));

        // Sending starts a new range in the same allocation after request scrubbing.
        buffer.pending = 0..MAX_RESPONSE_BYTES;
        buffer.advance(17).unwrap();
        assert_eq!(buffer.bytes().unwrap().len(), MAX_RESPONSE_BYTES - 17);
        assert_eq!(buffer.advance(usize::MAX), Err(Error::Io));
        assert_eq!(buffer.pending, 17..MAX_RESPONSE_BYTES);
        buffer.advance(MAX_RESPONSE_BYTES - 17).unwrap();
        assert!(buffer.pending.is_empty());
        drop(buffer);
        assert_eq!(owner.resources.active.get(), 0);
    }

    /// Cancellation-aware scope with a deadline that can only become earlier.
    #[derive(Clone)]
    struct TestScope {
        deadline: Instant,

        cancellation: uring_runtime::environment::Cancellation,
    }

    impl uring_runtime::Scope for TestScope {
        type Error = Error;

        /// Prioritize cancellation over deadline expiry.
        fn check(&self) -> Result<()> {
            if self.cancellation.is_cancelled() {
                Err(Error::Cancelled)
            } else if environment::now() >= self.deadline {
                Err(Error::DeadlineExceeded)
            } else {
                Ok(())
            }
        }

        /// Share the caller's cancellation source with reactor operations.
        fn cancellation(&self) -> Option<&uring_runtime::environment::Cancellation> {
            Some(&self.cancellation)
        }
    }

    impl Scope for TestScope {
        /// Retain cancellation while choosing the earlier deadline.
        fn with_deadline(&self, deadline: Instant) -> Self {
            Self {
                deadline: self.deadline.min(deadline),
                ..self.clone()
            }
        }
    }

    /// Count a retained memory, submission, or connection reservation.
    struct Charge(Rc<Cell<usize>>);

    impl Charge {
        /// Acquire a counted reservation.
        fn new(count: &Rc<Cell<usize>>) -> Self {
            count.set(count.get() + 1);
            Self(count.clone())
        }
    }

    impl Drop for Charge {
        /// Release the reservation when its final owner is dropped.
        fn drop(&mut self) {
            self.0.set(self.0.get() - 1);
        }
    }

    /// Routes with counted connections and a one-shot admission refusal.
    struct Tracked {
        routes: Routes,

        connections: Rc<Cell<usize>>,

        deny_next: Cell<bool>,
    }

    impl Handler for Tracked {
        type Connection = Charge;

        /// Reject the next connection when requested, otherwise retain a charge.
        fn connect(&self) -> Option<Charge> {
            if self.deny_next.replace(false) {
                return None;
            }
            Some(Charge::new(&self.connections))
        }

        /// Forward trusted response generation to the shared test routes.
        fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error> {
            self.routes.get(path, out)
        }

        /// Forward observations to the ordered event log.
        fn observe(&self, event: Event) {
            self.routes.observe(event);
        }
    }

    /// Find a valid instant less than one second below the platform's upper limit.
    fn near_instant_limit() -> Instant {
        let mut instant = Instant::now();
        for bit in (0..64).rev() {
            if let Some(next) = instant.checked_add(Duration::from_secs(1u64 << bit)) {
                instant = next;
            }
        }
        assert!(instant.checked_add(Duration::from_secs(1)).is_none());
        instant
    }

    /// Reject deadline overflow before submitting I/O and release connection ownership.
    #[test]
    fn exchange_deadline_overflow_returns_invalid_input_without_io() {
        use std::task::{Context, Waker};
        use uring_runtime::{environment::SimulationClock, reactor::simulation::Simulation};

        let limit = near_instant_limit();
        let now = limit - Duration::from_secs(1);
        assert!(now.checked_add(CONNECTION_TIMEOUT).is_none());
        let clock = SimulationClock::new_at(1, now, std::time::SystemTime::UNIX_EPOCH);
        let _clock = clock.environment(0).enter();
        let simulation = Simulation::new();
        let _simulation = simulation.enter();
        let reactor = Reactor::<TestScope, ()>::new(8, ());
        let submissions = reactor.reserve_submissions(CONTROL_SLOTS, ()).unwrap();
        let owner = Server::new(submissions, ());
        let connections = Rc::new(Cell::new(0));
        let handler = Tracked {
            routes: Routes::default(),
            connections: connections.clone(),
            deny_next: Cell::new(false),
        };
        let scope = TestScope {
            deadline: limit,
            cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
        };
        assert_eq!(uring_runtime::Scope::check(&scope), Ok(()));
        let (fd, peer) = simulation.socket_pair();
        let buffer = owner.buffer(&handler).unwrap();
        assert_eq!(connections.get(), 1);
        let trace = simulation.trace();
        let mut exchange = owner.exchange(&reactor, &handler, Rc::new(fd), buffer, &scope);
        let mut cx = Context::from_waker(Waker::noop());
        assert_eq!(
            exchange.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::InvalidInput))
        );
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(owner.resources.active.get(), 0);
        assert_eq!(connections.get(), 0);
        let after = simulation.trace();
        assert_eq!(&after[..trace.len()], trace);
        assert_eq!(after.len(), trace.len() + 1);
        assert_eq!(after.last().unwrap().operation, "close");
        assert_eq!(peer.try_recv(&mut [0; 1]).unwrap(), 0);
    }

    /// Keep the two-second limit and earlier parent deadline, even near Instant's limit.
    #[test]
    fn exchange_deadline_preserves_timeout_and_parent_limit() {
        use std::task::{Context, Waker};
        use uring_runtime::{environment::SimulationClock, reactor::simulation::Simulation};

        for (now, parent_timeout, timeout) in [
            (Instant::now(), Duration::from_secs(10), CONNECTION_TIMEOUT),
            (
                Instant::now(),
                Duration::from_secs(1),
                Duration::from_secs(1),
            ),
            (
                near_instant_limit() - CONNECTION_TIMEOUT,
                CONNECTION_TIMEOUT,
                CONNECTION_TIMEOUT,
            ),
        ] {
            let clock = SimulationClock::new_at(1, now, std::time::SystemTime::UNIX_EPOCH);
            let _clock = clock.environment(0).enter();
            let simulation = Simulation::new();
            let _simulation = simulation.enter();
            let reactor = Reactor::<TestScope, ()>::new(8, ());
            let submissions = reactor.reserve_submissions(CONTROL_SLOTS, ()).unwrap();
            let owner = Server::new(submissions, ());
            let handler = Routes::default();
            let scope = TestScope {
                deadline: now.checked_add(parent_timeout).unwrap(),
                cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
            };
            let (fd, _peer) = simulation.socket_pair();
            let buffer = owner.buffer(&handler).unwrap();
            let mut exchange = owner.exchange(&reactor, &handler, Rc::new(fd), buffer, &scope);
            let mut cx = Context::from_waker(Waker::noop());
            assert!(exchange.as_mut().poll(&mut cx).is_pending());
            clock.advance(timeout - Duration::from_nanos(1));
            for _ in 0..4 {
                reactor.poll_budgeted(32).unwrap();
                assert!(exchange.as_mut().poll(&mut cx).is_pending());
            }
            assert_eq!(owner.resources.active.get(), 1);
            clock.advance(Duration::from_nanos(1));
            let mut result = Poll::Pending;
            for _ in 0..32 {
                reactor.poll_budgeted(32).unwrap();
                result = exchange.as_mut().poll(&mut cx);
                if result.is_ready() {
                    break;
                }
            }
            assert_eq!(result, Poll::Ready(Err(Error::DeadlineExceeded)));
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(owner.resources.active.get(), 0);
        }
    }

    /// Report exchange expiry once, keep serving, and release all resources on drain.
    #[test]
    fn serve_classifies_exchange_deadline_without_stopping_listener() {
        use std::task::{Context, Waker};
        use uring_runtime::{
            environment::SimulationClock,
            reactor::{SocketAddress, simulation::Simulation},
        };

        let now = Instant::now();
        let clock = SimulationClock::new_at(1, now, std::time::SystemTime::UNIX_EPOCH);
        let _clock = clock.environment(0).enter();
        let simulation = Simulation::new();
        let _simulation = simulation.enter();
        let reactor = Reactor::<TestScope, ()>::new(8, ());
        let memory = Rc::new(Cell::new(0));
        let control = Rc::new(Cell::new(0));
        let connections = Rc::new(Cell::new(0));
        let submissions = reactor
            .reserve_submissions(CONTROL_SLOTS, Charge::new(&control))
            .unwrap();
        let owner = Server::new(submissions, Charge::new(&memory));
        let handler = Tracked {
            routes: Routes::default(),
            connections: connections.clone(),
            deny_next: Cell::new(false),
        };
        let scope = TestScope {
            deadline: now + Duration::from_secs(10),
            cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
        };
        let address = SocketAddress::Inet("127.0.0.1:18080".parse().unwrap());
        let listener = simulation.listen(address.clone()).unwrap();
        let slow = simulation.connect(address.clone()).unwrap();
        let mut server = owner.serve(&reactor, listener, &handler, &scope);
        let mut cx = Context::from_waker(Waker::noop());
        for _ in 0..4 {
            assert!(server.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(32).unwrap();
        }
        assert_eq!(*handler.routes.0.borrow(), vec![Event::Accepted]);
        assert_eq!(connections.get(), 1);

        clock.advance(CONNECTION_TIMEOUT - Duration::from_nanos(1));
        for _ in 0..4 {
            reactor.poll_budgeted(32).unwrap();
            assert!(server.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(*handler.routes.0.borrow(), vec![Event::Accepted]);
        assert_eq!(connections.get(), 1);

        clock.advance(Duration::from_nanos(1));
        for _ in 0..32 {
            reactor.poll_budgeted(32).unwrap();
            assert!(server.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(uring_runtime::Scope::check(&scope), Ok(()));
        assert!(owner.serving.get());
        assert_eq!(
            *handler.routes.0.borrow(),
            vec![Event::Accepted, Event::Timeout]
        );
        assert_eq!(owner.resources.active.get(), 0);
        assert_eq!(connections.get(), 0);
        assert_eq!(slow.try_recv(&mut [0; 1]).unwrap(), 0);

        let healthy = simulation.connect(address).unwrap();
        let request = b"GET /live HTTP/1.1\r\nHost: local\r\n\r\n";
        assert_eq!(healthy.try_send(request).unwrap(), request.len());
        for _ in 0..32 {
            reactor.poll_budgeted(32).unwrap();
            assert!(server.as_mut().poll(&mut cx).is_pending());
        }
        let mut response = [0; 256];
        let used = healthy.try_recv(&mut response).unwrap();
        assert_eq!(
            std::str::from_utf8(&response[..used]).unwrap(),
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 3\r\nConnection: close\r\nCache-Control: no-store\r\n\r\nok\n"
        );
        assert_eq!(healthy.try_recv(&mut response).unwrap(), 0);
        assert_eq!(uring_runtime::Scope::check(&scope), Ok(()));
        assert_eq!(connections.get(), 0);
        assert_eq!(
            *handler.routes.0.borrow(),
            vec![Event::Accepted, Event::Timeout, Event::Accepted]
        );

        drop(server);
        assert!(!owner.serving.get());
        drop(owner);
        let mut drain = reactor.drain();
        let mut result = Poll::Pending;
        for _ in 0..32 {
            reactor.poll_budgeted(32).unwrap();
            result = drain.as_mut().poll(&mut cx);
            if result.is_ready() {
                break;
            }
        }
        assert_eq!(result, Poll::Ready(Ok(())));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(memory.get(), 0);
        assert_eq!(control.get(), 0);
        assert_eq!(connections.get(), 0);
        drop(drain);
        drop((reactor, scope, slow, healthy));
        assert_eq!(simulation.live_handles(), 0);
    }

    /// Keep reservations through abandonment and reject a second concurrent listener.
    #[test]
    fn independent_server_dispatch_guard_and_abandoned_buffer_fence() {
        use std::{
            io::{Read, Write as _},
            net::{TcpListener, TcpStream},
            task::{Context, Waker},
        };
        let reactor = Reactor::<TestScope, ()>::new(8, ());
        let memory = Rc::new(Cell::new(0));
        let control = Rc::new(Cell::new(0));
        let connections = Rc::new(Cell::new(0));
        let submissions = reactor
            .reserve_submissions(CONTROL_SLOTS, Charge::new(&control))
            .unwrap();
        let owner = Server::new(submissions, Charge::new(&memory));
        let handler = Tracked {
            routes: Routes::default(),
            connections: connections.clone(),
            deny_next: Cell::new(false),
        };
        let scope = TestScope {
            deadline: environment::now() + Duration::from_secs(10),
            cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
        };
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let mut server = owner.serve(&reactor, listener.into(), &handler, &scope);
        let mut cx = Context::from_waker(Waker::noop());
        assert!(server.as_mut().poll(&mut cx).is_pending());
        let second = TcpListener::bind("127.0.0.1:0").unwrap();
        let mut duplicate = owner.serve(&reactor, second.into(), &handler, &scope);
        assert_eq!(
            duplicate.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::InvalidConfiguration))
        );
        drop(duplicate);
        let mut socket = TcpStream::connect(address).unwrap();
        socket
            .write_all(b"GET /live HTTP/1.1\r\nHost: local\r\nAuthorization: secret\r\n\r\n")
            .unwrap();
        socket.set_nonblocking(true).unwrap();
        let until = Instant::now() + Duration::from_secs(3);
        let mut response = Vec::new();
        loop {
            assert!(Instant::now() < until);
            assert!(server.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
            let mut bytes = [0; 1024];
            match socket.read(&mut bytes) {
                Ok(0) => break,
                Ok(n) => response.extend_from_slice(&bytes[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(e) => panic!("{e}"),
            }
        }
        assert_eq!(
            std::str::from_utf8(&response).unwrap(),
            "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 3\r\nConnection: close\r\nCache-Control: no-store\r\n\r\nok\n"
        );
        assert_eq!(connections.get(), 0);
        let _slow = TcpStream::connect(address).unwrap();
        let until = Instant::now() + Duration::from_secs(1);
        while connections.get() == 0 {
            assert!(Instant::now() < until);
            assert!(server.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        // The accept creates a buffer; the next poll submits its receive.
        assert!(server.as_mut().poll(&mut cx).is_pending());
        drop(server);
        assert!(!owner.serving.get());
        drop(owner);
        assert_eq!(memory.get(), 1);
        assert_eq!(control.get(), 1);
        assert_eq!(connections.get(), 1);
        let mut drain = reactor.drain();
        let until = Instant::now() + Duration::from_secs(3);
        loop {
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            assert!(Instant::now() < until);
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(memory.get(), 0);
        assert_eq!(control.get(), 0);
        assert_eq!(connections.get(), 0);
        assert_eq!(*handler.routes.0.borrow(), vec![Event::Accepted; 2]);
    }

    /// Refusal must not consume capacity or interrupt an already admitted exchange.
    #[test]
    fn refused_connection_preserves_listener_existing_exchange_and_capacity() {
        use std::{
            io::{Read, Write as _},
            net::{TcpListener, TcpStream},
            task::{Context, Waker},
        };
        let reactor = Reactor::<TestScope, ()>::new(8, ());
        let memory = Rc::new(Cell::new(0));
        let control = Rc::new(Cell::new(0));
        let connections = Rc::new(Cell::new(0));
        let submissions = reactor
            .reserve_submissions(CONTROL_SLOTS, Charge::new(&control))
            .unwrap();
        let owner = Server::new(submissions, Charge::new(&memory));
        let handler = Tracked {
            routes: Routes::default(),
            connections: connections.clone(),
            deny_next: Cell::new(false),
        };
        let scope = TestScope {
            deadline: environment::now() + Duration::from_secs(10),
            cancellation: uring_runtime::environment::Cancellation::new().unwrap(),
        };
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let mut server = owner.serve(&reactor, listener.into(), &handler, &scope);
        let mut cx = Context::from_waker(Waker::noop());
        let until = Instant::now() + Duration::from_secs(1);
        let mut tick = || {
            assert!(Instant::now() < until);
            assert!(server.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };

        let existing = TcpStream::connect(address).unwrap();
        while connections.get() != 1 {
            tick();
        }
        // Submit the existing exchange's receive before refusing another socket.
        tick();
        handler.deny_next.set(true);
        let mut refused = TcpStream::connect(address).unwrap();
        refused.set_nonblocking(true).unwrap();
        while !handler.routes.0.borrow().contains(&Event::Rejected) {
            tick();
        }
        assert_eq!(refused.read(&mut [0; 1]).unwrap(), 0);
        assert!(!handler.deny_next.get());
        assert!(owner.serving.get());
        assert_eq!(owner.resources.active.get(), 1);
        assert_eq!(connections.get(), 1);

        // Fill every connection slot after the refusal to detect leaked capacity.
        let mut sockets = vec![existing];
        for _ in 1..MAX_CONNECTIONS {
            sockets.push(TcpStream::connect(address).unwrap());
        }
        while connections.get() != MAX_CONNECTIONS {
            tick();
        }
        assert_eq!(owner.resources.active.get(), MAX_CONNECTIONS);
        for socket in &mut sockets {
            socket
                .write_all(b"GET /live HTTP/1.1\r\nHost: local\r\n\r\n")
                .unwrap();
            socket.set_nonblocking(true).unwrap();
        }
        for mut socket in sockets {
            let mut response = Vec::new();
            loop {
                tick();
                let mut bytes = [0; 1024];
                match socket.read(&mut bytes) {
                    Ok(0) => break,
                    Ok(n) => response.extend_from_slice(&bytes[..n]),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(e) => panic!("{e}"),
                }
            }
            assert_eq!(
                std::str::from_utf8(&response).unwrap(),
                "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 3\r\nConnection: close\r\nCache-Control: no-store\r\n\r\nok\n"
            );
        }
        assert_eq!(connections.get(), 0);
        assert_eq!(owner.resources.active.get(), 0);
        let mut expected = vec![Event::Accepted, Event::Accepted, Event::Rejected];
        expected.extend([Event::Accepted; MAX_CONNECTIONS - 1]);
        assert_eq!(*handler.routes.0.borrow(), expected);
        drop(server);
        assert!(!owner.serving.get());
        drop(owner);
        let mut drain = reactor.drain();
        let until = Instant::now() + Duration::from_secs(3);
        loop {
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            assert!(Instant::now() < until);
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(memory.get(), 0);
        assert_eq!(control.get(), 0);
        assert_eq!(connections.get(), 0);
    }
}
