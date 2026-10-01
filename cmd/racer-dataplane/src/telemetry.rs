//! Bounded diagnostics and HTTP endpoints, polled by an existing worker.
pub mod failures;
pub mod health;
pub mod metrics;
pub mod send_crc;
pub mod tracing;

use crate::runtime::reactor::Descriptor;
use crate::{
    error::{Error, Operation, Result},
    model::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::{Deadline, RequestScope},
        reactor::{IoBuffer, Reactor},
    },
};
use metrics::{Event, Gauge, GaugeLease};
#[cfg(test)]
use std::net::TcpListener;
#[cfg(test)]
use std::time::Instant;
use std::{
    cell::{Cell, OnceCell},
    fmt::Write,
    net::SocketAddr,
    rc::Rc,
    task::Poll,
    time::Duration,
};
use zeroize::Zeroize;

#[derive(Default)]
pub struct Telemetry {
    pub(crate) membership: OnceCell<Rc<dyn Fn() -> Result<MembershipDiagnostic>>>,
    pub send_crc: send_crc::Samples,
    pub failures: failures::Failures,
    pub metrics: metrics::Metrics,
    pub health: health::Health,
    pub tracing: tracing::Tracing,
    io: OnceCell<Rc<DiagnosticIo>>,
}

/// Fixed-size diagnostic, with exact decimal counters and no metric labels.
#[derive(Default)]
pub(crate) struct MembershipDiagnostic {
    pub accepted_sequence: u64,
    pub accepted_membership: u64,
    pub accepted_hash: [u8; 32],
    pub pending_sequence: u64,
    pub pending_membership: u64,
    pub expected_workers: usize,
    pub matching_workers: usize,
}
impl MembershipDiagnostic {
    pub(crate) fn fully_applied(&self) -> bool {
        self.accepted_sequence != 0
            && self.pending_sequence == 0
            && self.expected_workers != 0
            && self.matching_workers == self.expected_workers
    }
    fn write(&self, out: &mut impl Write) -> std::fmt::Result {
        write!(
            out,
            "accepted_sequence={} accepted_membership={} accepted_membership_hash=",
            self.accepted_sequence, self.accepted_membership
        )?;
        for byte in self.accepted_hash {
            write!(out, "{byte:02x}")?;
        }
        writeln!(
            out,
            " pending_sequence={} pending_membership={} expected_workers={} matching_workers={} fully_applied={}",
            self.pending_sequence,
            self.pending_membership,
            self.expected_workers,
            self.matching_workers,
            u8::from(self.fully_applied())
        )
    }
}

impl Telemetry {
    /// Reserve diagnostic memory/control slots before data admission. Does not
    /// bind a listener or spawn work. Duplicate attachment is rejected.
    pub fn attach_io(&self, reactor: Rc<Reactor>, admission: Rc<Admission>) -> Result<()> {
        if self.io.get().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        let io = Rc::new(DiagnosticIo::attach(reactor, admission)?);
        self.io.set(io).map_err(|_| Error::InvalidConfiguration)
    }

    /// Poll alongside the worker reactor. Bind errors occur on first poll.
    /// After cancellation, drain the reactor before dropping attached resources.
    pub fn serve<'a>(&'a self, address: SocketAddr, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let io = self.io.get().ok_or(Error::InvalidConfiguration)?.clone();
            self.serve_with_io(address, io, scope).await
        })
    }

    /// Alternative for integrators that retain attachment separately.
    pub fn serve_with_io<'a>(
        &'a self,
        address: SocketAddr,
        io: Rc<DiagnosticIo>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let listener = Descriptor::tcp_listener(address)?;
            serve(self, listener, io, scope).await
        })
    }

    /// Transfer a listener, including port-zero bindings, to the worker reactor.
    pub fn serve_listener_with_io<'a>(
        &'a self,
        listener: std::net::TcpListener,
        io: Rc<DiagnosticIo>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        serve(self, listener.into(), io, scope)
    }
}

pub const MAX_CONNECTIONS: usize = 4;
pub const MAX_REQUEST_BYTES: usize = 1024;
pub const MAX_RESPONSE_BYTES: usize = 64 * 1024;
pub const CONNECTION_TIMEOUT: Duration = Duration::from_secs(2);
/// Fixed startup charge covers buffers and bounded service/future bookkeeping.
pub const RESERVED_BYTES: usize = (MAX_CONNECTIONS + 1) * (MAX_RESPONSE_BYTES + 4096);
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
    submissions: Rc<crate::runtime::reactor::SubmissionCapacity>,
    active: Cell<usize>,
}
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(reactor: Rc<Reactor>, admission: Rc<Admission>) -> Result<Self> {
        let control = admission.reserve(None, ResourceClass::ControlProgress, CONTROL_SLOTS)?;
        let mut memory = admission.reserve(None, ResourceClass::RequestContext, RESERVED_BYTES)?;
        let bookkeeping =
            memory.split(CONTROL_SLOTS * crate::runtime::reactor::SUBMISSION_BYTES)?;
        let submissions = reactor.reserve_submissions(control, bookkeeping)?;
        Ok(Self {
            reactor,
            admission,
            resources: Rc::new(Resources {
                _memory: memory,
                submissions,
                active: Cell::new(0),
            }),
            serving: Cell::new(false),
        })
    }
    fn buffer(&self, metrics: &metrics::Metrics) -> Result<Buffer> {
        if self.resources.active.get() >= MAX_CONNECTIONS {
            return Err(Error::Overloaded);
        }
        let gauge = metrics.lease(Gauge::DiagnosticConnections)?;
        let bytes = vec![0; MAX_RESPONSE_BYTES];
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
    bytes: Vec<u8>,
    start: usize,
    end: usize,
    resources: Rc<Resources>,
    _gauge: GaugeLease,
}
impl Drop for Buffer {
    fn drop(&mut self) {
        self.bytes.as_mut_slice().zeroize();
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

fn serve<'a>(
    telemetry: &'a Telemetry,
    listener: Descriptor,
    io: Rc<DiagnosticIo>,
    scope: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        scope.check()?;
        if io.serving.replace(true) {
            return Err(Error::InvalidConfiguration);
        }
        let _serving = Serving(io.clone());
        listener.set_nonblocking()?;
        let listener = Rc::new(listener);
        let mut accepting = None;
        let mut connections: Vec<Operation<'_, ()>> = Vec::with_capacity(MAX_CONNECTIONS);
        let cancellation = scope.cancellation.subscribe()?;
        std::future::poll_fn(|cx| {
            cancellation.register(cx.waker());
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
                accepting = Some(crate::runtime::retry_listener(scope, || {
                    io.reactor.accept_reserved(
                        listener.clone(),
                        Some(io.resources.submissions.clone()),
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
    fd: Rc<Descriptor>,
    mut buffer: Buffer,
    parent: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        let scope = RequestScope {
            body_deadlines: parent.body_deadlines,
            request: parent.request,
            deadline: Deadline(
                parent
                    .deadline
                    .0
                    .min(crate::runtime::environment::now() + CONNECTION_TIMEOUT),
            ),
            cancellation: parent.cancellation.clone(),
        };
        let mut used = 0;
        let route = loop {
            buffer.start = used;
            buffer.end = MAX_REQUEST_BYTES;
            let completed = io
                .reactor
                .recv_reserved(fd.clone(), buffer, io.resources.submissions.clone(), &scope)
                .await?;
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
        buffer.bytes.as_mut_slice().zeroize();
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
            let completed = io
                .reactor
                .send_reserved(fd.clone(), buffer, io.resources.submissions.clone(), &scope)
                .await?;
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
    Membership,
    Health,
    Ready,
    Metrics,
    Failures,
    Aead,
    SendCrc,
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
        Some("/debug/membership") => Route::Membership,
        Some("/healthz") => Route::Health,
        Some("/readyz") => Route::Ready,
        Some("/metrics") => Route::Metrics,
        Some("/debug/failures") => Route::Failures,
        Some("/debug/aead") => Route::Aead,
        Some("/debug/send-crc") => Route::SendCrc,
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
        Route::Membership => ("200 OK", "", Event::DiagnosticMetrics),
        Route::Failures => ("200 OK", "", Event::DiagnosticFailures),
        Route::Aead => ("200 OK", "", Event::DiagnosticFailures),
        Route::SendCrc => ("200 OK", "", Event::DiagnosticFailures),
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
    } else if route == Route::Membership {
        match telemetry.membership.get().and_then(|read| read().ok()) {
            Some(state) => state.write(&mut output).map_err(|_| Error::Internal)?,
            None => output
                .write_str("unavailable fully_applied=0\n")
                .map_err(|_| Error::Internal)?,
        }
    } else if route == Route::SendCrc {
        telemetry
            .send_crc
            .write(&mut output)
            .map_err(|_| Error::Internal)?;
    } else if route == Route::Aead {
        telemetry
            .failures
            .write_aead(&mut output)
            .map_err(|_| Error::Internal)?;
    } else if route == Route::Failures {
        telemetry
            .failures
            .write(&mut output)
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
mod tests;
