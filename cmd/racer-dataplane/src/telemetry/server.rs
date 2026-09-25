//! Small HTTP diagnostics, polled by an existing worker without an executor.
use super::{
    Telemetry,
    metrics::{Event, Gauge, GaugeLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::{Deadline, RequestScope},
        reactor::{IoBuffer, Reactor},
    },
};
use std::{
    cell::Cell,
    fmt::Write,
    net::TcpListener,
    os::fd::OwnedFd,
    rc::Rc,
    task::Poll,
    time::{Duration, Instant},
};
use zeroize::Zeroize;

pub const MAX_CONNECTIONS: usize = 4;
pub const MAX_REQUEST_BYTES: usize = 1024;
pub const MAX_RESPONSE_BYTES: usize = 4096;
pub const CONNECTION_TIMEOUT: Duration = Duration::from_secs(2);
/// Fixed startup charge covers buffers and bounded service/future bookkeeping.
pub const RESERVED_BYTES: usize = (MAX_CONNECTIONS + 1) * 8192;
pub const CONTROL_SLOTS: usize = MAX_CONNECTIONS + 1;

/// One attachment serves at most one listener. Resource reservations outlive
/// abandoned connection futures because the reactor retains their buffers.
pub struct DiagnosticIo {
    reactor: Rc<Reactor>,
    admission: Rc<Admission>,
    resources: Rc<Resources>,
    serving: Cell<bool>,
}
struct Resources {
    _memory: Reservation,
    _control: Reservation,
    active: Cell<usize>,
}
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(reactor: Rc<Reactor>, admission: Rc<Admission>) -> Result<Self> {
        let control = admission.reserve(None, ResourceClass::ControlProgress, CONTROL_SLOTS)?;
        let memory = admission.reserve(None, ResourceClass::RequestContext, RESERVED_BYTES)?;
        reactor.init()?;
        Ok(Self {
            reactor,
            admission,
            resources: Rc::new(Resources {
                _memory: memory,
                _control: control,
                active: Cell::new(0),
            }),
            serving: Cell::new(false),
        })
    }
    fn buffer(&self, metrics: &super::metrics::Metrics) -> Result<Buffer> {
        if self.resources.active.get() >= MAX_CONNECTIONS {
            return Err(Error::Overloaded);
        }
        let gauge = metrics.lease(Gauge::DiagnosticConnections)?;
        let bytes = Box::new([0; MAX_RESPONSE_BYTES]);
        self.resources.active.set(self.resources.active.get() + 1);
        Ok(Buffer {
            bytes,
            start: 0,
            end: MAX_REQUEST_BYTES,
            resources: self.resources.clone(),
            _gauge: gauge,
        })
    }
}
struct Serving(Rc<DiagnosticIo>);
impl Drop for Serving {
    fn drop(&mut self) {
        self.0.serving.set(false);
    }
}
struct Buffer {
    bytes: Box<[u8; MAX_RESPONSE_BYTES]>,
    start: usize,
    end: usize,
    resources: Rc<Resources>,
    _gauge: GaugeLease,
}
impl Drop for Buffer {
    fn drop(&mut self) {
        self.bytes.zeroize();
        self.resources.active.set(self.resources.active.get() - 1);
    }
}
impl crate::runtime::reactor::sealed::Sealed for Buffer {}
impl IoBuffer for Buffer {
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes[self.start..self.end])
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes[self.start..self.end])
    }
}

pub(super) fn serve<'a>(
    telemetry: &'a Telemetry,
    listener: TcpListener,
    io: Rc<DiagnosticIo>,
    scope: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        scope.check()?;
        if io.serving.replace(true) {
            return Err(Error::InvalidConfiguration);
        }
        let _serving = Serving(io.clone());
        listener.set_nonblocking(true).map_err(|_| Error::Io)?;
        let listener = Rc::new(OwnedFd::from(listener));
        let mut accepting = None;
        let mut connections: Vec<Operation<'_, ()>> = Vec::with_capacity(MAX_CONNECTIONS);
        std::future::poll_fn(|cx| {
            scope.cancellation.register(cx.waker())?;
            scope.check()?;
            // Every poll performs bounded work. Slow headers cannot monopolize
            // the listener; up to four independent two-second exchanges progress.
            let mut index = 0;
            while index < connections.len() {
                match connections[index].as_mut().poll(cx) {
                    Poll::Pending => index += 1,
                    Poll::Ready(result) => {
                        drop(connections.swap_remove(index));
                        if let Err(error) = result {
                            telemetry.metrics.record(
                                match error {
                                    Error::DeadlineExceeded => Event::DiagnosticTimeout,
                                    _ => Event::DiagnosticIoError,
                                },
                                1,
                            )?;
                        }
                    }
                }
            }
            if accepting.is_none() && io.resources.active.get() < MAX_CONNECTIONS {
                accepting = Some(io.reactor.accept(listener.clone(), scope));
            }
            if let Some(accept) = &mut accepting {
                match accept.as_mut().poll(cx) {
                    Poll::Pending => (),
                    Poll::Ready(result) => {
                        accepting = None;
                        let fd = result?;
                        telemetry.metrics.record(Event::DiagnosticAccepted, 1)?;
                        let buffer = io.buffer(&telemetry.metrics)?;
                        connections.push(exchange(
                            telemetry,
                            io.clone(),
                            Rc::new(fd),
                            buffer,
                            scope,
                        ));
                        cx.waker().wake_by_ref();
                    }
                }
            }
            Poll::Pending
        })
        .await
    })
}

fn exchange<'a>(
    telemetry: &'a Telemetry,
    io: Rc<DiagnosticIo>,
    fd: Rc<OwnedFd>,
    mut buffer: Buffer,
    parent: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        let scope = RequestScope {
            request: parent.request,
            deadline: Deadline(parent.deadline.0.min(Instant::now() + CONNECTION_TIMEOUT)),
            cancellation: parent.cancellation.clone(),
        };
        let mut used = 0;
        let route = loop {
            buffer.start = used;
            buffer.end = MAX_REQUEST_BYTES;
            let completed = io.reactor.recv(fd.clone(), buffer, (), &scope).await?;
            if completed.bytes == 0 || completed.bytes > MAX_REQUEST_BYTES - used {
                return Err(Error::Io);
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
        // Headers are never retained in decoded objects, traces, or responses.
        buffer.bytes.zeroize();
        let length = respond(
            telemetry,
            route,
            !io.admission.is_stopped(),
            &mut buffer.bytes[..],
        )?;
        let mut sent = 0;
        while sent < length {
            buffer.start = sent;
            buffer.end = length;
            let completed = io.reactor.send(fd.clone(), buffer, (), &scope).await?;
            if completed.bytes == 0 || completed.bytes > length - sent {
                return Err(Error::Io);
            }
            sent += completed.bytes;
            buffer = completed.buffer;
        }
        // Always close. There is no pipelining, keepalive, body buffering, or echo.
        Ok(())
    })
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Route {
    Health,
    Ready,
    Metrics,
    NotFound,
    Method,
    BadRequest,
    TooLarge,
}
fn parse(bytes: &[u8]) -> Route {
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
    match request.path {
        Some("/healthz") => Route::Health,
        Some("/readyz") => Route::Ready,
        Some("/metrics") => Route::Metrics,
        _ => Route::NotFound,
    }
}

/// A fallible fixed-slice writer avoids allocating intermediate response strings.
struct Output<'a> {
    bytes: &'a mut [u8],
    used: usize,
}
impl Write for Output<'_> {
    fn write_str(&mut self, text: &str) -> std::fmt::Result {
        let end = self.used.checked_add(text.len()).ok_or(std::fmt::Error)?;
        self.bytes
            .get_mut(self.used..end)
            .ok_or(std::fmt::Error)?
            .copy_from_slice(text.as_bytes());
        self.used = end;
        Ok(())
    }
}
fn respond(
    telemetry: &Telemetry,
    route: Route,
    admission_usable: bool,
    bytes: &mut [u8],
) -> Result<usize> {
    let ready = admission_usable && telemetry.health.ready();
    let (status, body, event) = match route {
        Route::Health if telemetry.health.live() => ("200 OK", "ok\n", Event::DiagnosticHealth),
        Route::Health => (
            "503 Service Unavailable",
            "not live\n",
            Event::DiagnosticHealth,
        ),
        Route::Ready if ready => ("200 OK", "ready\n", Event::DiagnosticReady),
        Route::Ready => (
            "503 Service Unavailable",
            "not ready\n",
            Event::DiagnosticReady,
        ),
        Route::Metrics => ("200 OK", "", Event::DiagnosticMetrics),
        Route::Method => (
            "405 Method Not Allowed",
            "method not allowed\n",
            Event::DiagnosticRejected,
        ),
        Route::NotFound => ("404 Not Found", "not found\n", Event::DiagnosticRejected),
        Route::BadRequest => (
            "400 Bad Request",
            "bad request\n",
            Event::DiagnosticRejected,
        ),
        Route::TooLarge => (
            "431 Request Header Fields Too Large",
            "headers too large\n",
            Event::DiagnosticRejected,
        ),
    };
    telemetry.metrics.record(event, 1)?;
    // Body starts after fixed header headroom, then moves next to the actual head.
    const HEAD: usize = 256;
    let (header, body_bytes) = bytes.split_at_mut(HEAD);
    let mut output = Output {
        bytes: body_bytes,
        used: 0,
    };
    if route == Route::Metrics {
        telemetry
            .metrics
            .write_prometheus(&mut output)
            .map_err(|_| Error::Internal)?;
        writeln!(
            output,
            "# TYPE racer_ready gauge\nracer_ready {}\n# TYPE racer_live gauge\nracer_live {}",
            u8::from(ready),
            u8::from(telemetry.health.live())
        )
        .map_err(|_| Error::Internal)?;
    } else {
        output.write_str(body).map_err(|_| Error::Internal)?;
    }
    let body_len = output.used;
    let mut output = Output {
        bytes: header,
        used: 0,
    };
    let content_type = if route == Route::Metrics {
        "text/plain; version=0.0.4; charset=utf-8"
    } else {
        "text/plain; charset=utf-8"
    };
    write!(output, "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {body_len}\r\nConnection: close\r\nCache-Control: no-store\r\n").map_err(|_| Error::Internal)?;
    if route == Route::Method {
        output
            .write_str("Allow: GET\r\n")
            .map_err(|_| Error::Internal)?;
    }
    output.write_str("\r\n").map_err(|_| Error::Internal)?;
    let header_len = output.used;
    bytes.copy_within(HEAD..HEAD + body_len, header_len);
    Ok(header_len + body_len)
}

#[cfg(test)]
#[path = "server/tests.rs"]
mod tests;
