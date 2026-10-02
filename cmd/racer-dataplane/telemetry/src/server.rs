//! Bounded, explicitly polled diagnostic HTTP transport. Application policy is
//! supplied by a handler; no executor, listener binding, or admission is implicit.
use std::{
    cell::Cell,
    fmt::{self, Write},
    rc::Rc,
    task::Poll,
    time::{Duration, Instant},
};
use uring_runtime::{
    Budget, Error, Operation, Result, environment,
    reactor::{Descriptor, IoBuffer, Reactor, SubmissionCapacity},
};
use zeroize::{Zeroize, Zeroizing};

pub const MAX_CONNECTIONS: usize = 4;
pub const MAX_REQUEST_BYTES: usize = 1024;
pub const MAX_RESPONSE_BYTES: usize = 64 * 1024;
pub const CONNECTION_TIMEOUT: Duration = Duration::from_secs(2);
/// Buffers plus bounded service/future bookkeeping, acquired before serving.
pub const RESERVED_BYTES: usize = (MAX_CONNECTIONS + 1) * (MAX_RESPONSE_BYTES + 4096);
pub const CONTROL_SLOTS: usize = MAX_CONNECTIONS + 1;

pub trait Scope: uring_runtime::Scope {
    /// Narrow the deadline, preserving cancellation and caller metadata.
    fn with_deadline(&self, deadline: Instant) -> Self;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Response {
    Text,
    Metrics,
    NotFound,
    Unavailable,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Event {
    Accepted,
    Rejected,
    IoError,
    Timeout,
}

pub trait Handler {
    /// Retained in reactor-owned buffers until the completion fence.
    type Connection: 'static;
    /// Returning None rejects only this connection, not the listener.
    fn connect(&self) -> Option<Self::Connection>;
    /// Write only trusted output. NotFound discards output and uses a fixed body.
    fn get(&self, path: &str, out: &mut dyn Write) -> Result<Response, fmt::Error>;
    fn observe(&self, event: Event);
}

pub struct Server {
    resources: Rc<Resources>,
    serving: Cell<bool>,
}
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
        }
    }
    fn buffer<H: Handler>(&self, handler: &H) -> Option<Buffer<H::Connection>> {
        if self.resources.active.get() >= MAX_CONNECTIONS {
            return None;
        }
        let connection = handler.connect()?;
        let bytes = vec![0; MAX_RESPONSE_BYTES];
        self.resources.active.set(self.resources.active.get() + 1);
        Some(Buffer {
            bytes,
            start: 0,
            end: MAX_REQUEST_BYTES,
            resources: self.resources.clone(),
            _connection: connection,
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
                    accepting = Some(uring_runtime::retry_listener(scope, || {
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
    fn exchange<'a, S: Scope, B: Budget, H: Handler>(
        &'a self,
        reactor: &'a Reactor<S, B>,
        handler: &'a H,
        fd: Rc<Descriptor>,
        mut buffer: Buffer<H::Connection>,
        parent: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let scope = parent.with_deadline(environment::now() + CONNECTION_TIMEOUT);
            let mut used = 0;
            let route = loop {
                buffer.start = used;
                buffer.end = MAX_REQUEST_BYTES;
                let completed = reactor
                    .recv_reserved(
                        fd.clone(),
                        buffer,
                        self.resources.submissions.clone(),
                        &scope,
                    )
                    .await?;
                if completed.bytes == 0 || completed.bytes > MAX_REQUEST_BYTES - used {
                    return Err(Error::Io.into());
                }
                used += completed.bytes;
                buffer = completed.buffer;
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
            let length = respond(handler, route, &mut buffer.bytes).map_err(|_| Error::Io)?;
            path.zeroize();
            let mut sent = 0;
            while sent < length {
                buffer.start = sent;
                buffer.end = length;
                let completed = reactor
                    .send_reserved(
                        fd.clone(),
                        buffer,
                        self.resources.submissions.clone(),
                        &scope,
                    )
                    .await?;
                if completed.bytes == 0 || completed.bytes > length - sent {
                    return Err(Error::Io.into());
                }
                sent += completed.bytes;
                buffer = completed.buffer;
            }
            Ok(())
        })
    }
}
struct Serving<'a>(&'a Cell<bool>);
impl Drop for Serving<'_> {
    fn drop(&mut self) {
        self.0.set(false);
    }
}
struct Buffer<C: 'static> {
    bytes: Vec<u8>,
    start: usize,
    end: usize,
    resources: Rc<Resources>,
    _connection: C,
}
impl<C> Drop for Buffer<C> {
    fn drop(&mut self) {
        self.bytes.as_mut_slice().zeroize();
        self.resources.active.set(self.resources.active.get() - 1);
    }
}
// SAFETY: private fixed Vec retains its reservation and is not aliased.
unsafe impl<C: 'static> IoBuffer for Buffer<C> {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes[self.start..self.end])
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes[self.start..self.end])
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Route<'a> {
    Get(&'a str),
    Method,
    BadRequest,
    TooLarge,
}
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
struct Output<'a> {
    bytes: &'a mut [u8],
    used: usize,
}
impl Write for Output<'_> {
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
fn respond(
    handler: &impl Handler,
    route: Route<'_>,
    bytes: &mut [u8],
) -> Result<usize, fmt::Error> {
    const HEAD: usize = 256;
    if bytes.len() < HEAD {
        return Err(fmt::Error);
    }
    let (header, body) = bytes.split_at_mut(HEAD);
    let mut output = Output {
        bytes: body,
        used: 0,
    };
    let mut metrics = false;
    let (status, fixed) = match route {
        Route::Get(path) => match handler.get(path, &mut output)? {
            Response::Text => ("200 OK", None),
            Response::Metrics => {
                metrics = true;
                ("200 OK", None)
            }
            Response::Unavailable => ("503 Service Unavailable", None),
            Response::NotFound => ("404 Not Found", Some("not found\n")),
        },
        Route::Method => ("405 Method Not Allowed", Some("method not allowed\n")),
        Route::BadRequest => ("400 Bad Request", Some("bad request\n")),
        Route::TooLarge => (
            "431 Request Header Fields Too Large",
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
    let content_type = if metrics {
        "text/plain; version=0.0.4; charset=utf-8"
    } else {
        "text/plain; charset=utf-8"
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

#[cfg(test)]
mod tests;
