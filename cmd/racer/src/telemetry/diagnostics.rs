use super::*;

impl server::Scope for RequestScope {
    fn with_deadline(&self, deadline: std::time::Instant) -> Self {
        Self {
            deadline: Deadline(self.deadline.0.min(deadline)),
            ..self.clone()
        }
    }
}

/// One attachment serves at most one listener. Resource reservations outlive
/// abandoned connection futures because the reactor retains their buffers.
pub struct DiagnosticIo {
    pub(crate) reactor: Rc<Reactor>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    /// `/debug/hardware` working set, helper stack, and result (`HARDWARE_BYTES` of
    /// `RESERVED_BYTES`). Each helper and its session share this charge, so it is
    /// returned only after this owner, every helper, and every response drop it.
    hardware: HardwareCharge,
    server: Server,
}

/// Thread-safe hardware memory charge shared with detached discovery helpers.
pub(crate) type HardwareCharge = Arc<flow_control::Charge<AdmissionPolicy>>;

/// Portion of `RESERVED_BYTES` funding one hardware discovery, its helper
/// stack, and its result.
pub(super) const HARDWARE_BYTES: usize =
    HARDWARE_BODY_BYTES + crate::hardware::WORKING_BYTES + HARDWARE_STACK_BYTES;
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Result<Self> {
        Self::attach_configured(reactor, admission, false)
    }

    pub(super) fn attach_configured(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        binary: bool,
    ) -> Result<Self> {
        let slots = CONTROL_SLOTS
            + if binary {
                server::BINARY_CONTROL_SLOTS
            } else {
                0
            };
        let control = admission.reserve(None, ResourceClass::ControlProgress, slots)?;
        let mut memory = admission.reserve(
            None,
            ResourceClass::RequestContext,
            RESERVED_BYTES + (slots - CONTROL_SLOTS) * racer_io::scoped::SUBMISSION_BYTES,
        )?;
        let bookkeeping = memory.split(slots * racer_io::scoped::SUBMISSION_BYTES)?;
        let submissions = reactor.reserve_submissions(control, bookkeeping)?;
        // The hardware report is always available, independent of pprof, with its
        // own single in-flight slot so a running profile never makes it busy. Its
        // working set and bounded result are split into a shared charge that the
        // detached helper retains, so dropping this owner while a sysfs read is
        // blocked cannot return memory the helper still uses.
        let hardware = Arc::new(memory.split(HARDWARE_BYTES)?);
        // Connection buffers also retain the shared charge through their fences,
        // as they retain the rest of `memory`.
        let mut server = Server::new(submissions.clone(), (memory, hardware.clone()))
            .with_json(submissions.clone(), ());
        if binary {
            // The process-wide profiler budget includes the result allocation.
            server = server.with_binary(submissions, ());
        }
        Ok(Self {
            reactor,
            admission,
            hardware,
            server,
        })
    }
}

pub(super) fn serve<'a>(
    telemetry: &'a Telemetry,
    listener: Descriptor,
    io: Rc<DiagnosticIo>,
    scope: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        let handler = DiagnosticHandler {
            telemetry,
            admission: &io.admission,
            hardware: &io.hardware,
        };
        io.server
            .serve(&io.reactor, listener, &handler, scope)
            .await
    })
}

struct DiagnosticHandler<'a> {
    telemetry: &'a Telemetry,
    admission: &'a flow_control::Quotas<AdmissionPolicy>,
    hardware: &'a HardwareCharge,
}
impl Handler for DiagnosticHandler<'_> {
    type Connection = ::telemetry::Lease;
    fn connect(&self) -> Option<::telemetry::Lease> {
        self.telemetry
            .metrics
            .lease(Gauge::DiagnosticConnections)
            .ok()
    }
    fn get(
        &self,
        path: &str,
        out: &mut dyn Write,
    ) -> std::result::Result<Response, std::fmt::Error> {
        get(self.telemetry, path, !self.admission.is_stopped(), out)
    }
    fn observe(&self, event: server::Event) {
        let event = match event {
            server::Event::Accepted => Event::DiagnosticAccepted,
            server::Event::Rejected => Event::DiagnosticRejected,
            server::Event::IoError => Event::DiagnosticIoError,
            server::Event::Timeout => Event::DiagnosticTimeout,
        };
        self.telemetry.metrics.record(event, 1);
    }

    fn stream<'a>(&'a self, path: &str) -> Option<(Response, Box<dyn server::TextStream + 'a>)> {
        if path != "/metrics" {
            return None;
        }
        let telemetry = self.telemetry;
        let now = telemetry.failures.now();
        let ready = !self.admission.is_stopped() && telemetry.health.ready_at(now);
        telemetry.metrics.record(Event::DiagnosticMetrics, 1);
        Some((
            Response::Metrics,
            Box::new(MetricsStream::new(telemetry, ready, now)),
        ))
    }

    fn binary(
        &self,
        path: &str,
    ) -> Option<std::result::Result<server::BinaryResponse, server::BinaryError>> {
        if self.telemetry.profiling.is_some()
            && self.admission.is_stopped()
            && profile_duration(path).is_some()
        {
            return Some(Err(server::BinaryError::Unavailable("stopping")));
        }
        if path == "/debug/hardware" {
            return Some(hardware_request(self.telemetry, self.hardware));
        }
        profile_request(self.telemetry.profiling.as_ref(), path)
    }
}

/// `/metrics` as resumable pieces: the registry exposition, then health.
struct MetricsStream<'a> {
    telemetry: &'a Telemetry,
    exposition: Option<super::metrics::Exposition<'a>>,
    ready: bool,
    now: Instant,
}

impl<'a> MetricsStream<'a> {
    fn new(telemetry: &'a Telemetry, ready: bool, now: Instant) -> Self {
        Self {
            telemetry,
            exposition: Some(telemetry.metrics.exposition()),
            ready,
            now,
        }
    }
}

impl server::TextStream for MetricsStream<'_> {
    fn next(&mut self, mut out: &mut dyn Write) -> std::result::Result<bool, std::fmt::Error> {
        if let Some(exposition) = &mut self.exposition {
            if !exposition.next(&mut out)? {
                self.exposition = None;
            }
            return Ok(true);
        }
        writeln!(
            out,
            "# TYPE racer_ready gauge\nracer_ready {}\n# TYPE racer_live gauge\nracer_live {}",
            u8::from(self.ready),
            u8::from(self.telemetry.health.live_at(self.now))
        )?;
        Ok(false)
    }
}

/// Start one `/debug/hardware` discovery on a helper thread.
///
/// The control worker only captures the tiny Racer view (selector, claimed disks,
/// rails snapshot); the sysfs walk and JSON encoding run on a named, detached
/// thread. At most one helper exists at a time: a request while the previous
/// helper or its response is still alive gets 409 busy. The transport's own JSON
/// slot is a second, worker-local bound. The worker never joins the helper, so a
/// slow sysfs read cannot block it; on cancellation or deadline the helper stops
/// at its next read, skips encoding, and drops its result. Discovery that exceeds
/// `hardware::WORKING_BYTES` stops early with 503 `response_too_large`.
fn hardware_request(
    telemetry: &Telemetry,
    charge: &HardwareCharge,
) -> std::result::Result<server::BinaryResponse, server::BinaryError> {
    telemetry.metrics.record(Event::DiagnosticHardware, 1);
    let unavailable = server::BinaryError::Unavailable("unavailable");
    let prepare = telemetry.hardware.get().ok_or(unavailable)?;
    let mut active = telemetry.hardware_active.borrow_mut();
    if active.strong_count() != 0 {
        return Err(server::BinaryError::Busy);
    }
    let session = HardwareSession::start(prepare(), HARDWARE_BODY_BYTES, charge.clone())?;
    *active = Arc::downgrade(&session.state);
    Ok(server::BinaryResponse {
        kind: server::BinaryKind::Json,
        duration: HARDWARE_DEADLINE,
        session: Box::new(session),
    })
}

/// State shared by one hardware helper thread and its response session.
pub(super) struct HardwareState {
    completion: std::os::fd::OwnedFd,
    cancelled: Arc<std::sync::atomic::AtomicBool>,
    result: Mutex<Option<std::result::Result<Vec<u8>, server::BinaryError>>>,
    /// Memory funding discovery and the result, held until the helper and the
    /// response both finish, even if the diagnostics owner drops first.
    _charge: HardwareCharge,
}

/// One live hardware discovery, polled through its eventfd. Drop cancels.
struct HardwareSession {
    state: Arc<HardwareState>,
}

impl HardwareSession {
    /// Spawn the helper. `limit` bounds the encoded body.
    fn start(
        job: HardwareJob,
        limit: usize,
        charge: HardwareCharge,
    ) -> std::result::Result<Self, server::BinaryError> {
        use std::os::fd::FromRawFd;
        let unavailable = server::BinaryError::Unavailable("unavailable");
        // SAFETY: eventfd has no pointer arguments; ownership transfers only on success.
        let raw = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        if raw < 0 {
            return Err(unavailable);
        }
        let state = Arc::new(HardwareState {
            // SAFETY: raw is a new, successful eventfd descriptor.
            completion: unsafe { std::os::fd::OwnedFd::from_raw_fd(raw) },
            cancelled: Arc::default(),
            result: Mutex::new(None),
            _charge: charge,
        });
        let helper = state.clone();
        std::thread::Builder::new()
            .name("racer-hardware".into())
            .stack_size(HARDWARE_STACK_BYTES)
            .spawn(move || helper.run(job, limit))
            .map_err(|_| unavailable)?;
        Ok(Self { state })
    }
}

impl HardwareState {
    /// Discover, encode unless cancelled, publish, then notify the worker.
    fn run(&self, job: HardwareJob, limit: usize) {
        use std::os::fd::AsRawFd;
        use std::sync::atomic::Ordering;
        let result = (|| {
            let walk =
                crate::hardware::Walk::new(self.cancelled.clone(), crate::hardware::WORKING_BYTES);
            let report = match job(&walk) {
                Ok(report) => report,
                Err(crate::hardware::Abort::TooLarge) => {
                    return Err(server::BinaryError::Unavailable("response_too_large"));
                }
                Err(crate::hardware::Abort::Cancelled) => {
                    return Err(server::BinaryError::Unavailable("unavailable"));
                }
            };
            if self.cancelled.load(Ordering::Acquire) {
                return Err(server::BinaryError::Unavailable("unavailable"));
            }
            match encode_report(&report, limit) {
                Ok(body) => Ok(body.into_bytes()),
                Err(Encode::TooLarge) => {
                    Err(server::BinaryError::Unavailable("response_too_large"))
                }
                Err(Encode::Failed) => Err(server::BinaryError::Unavailable("unavailable")),
            }
        })();
        if self.cancelled.load(Ordering::Acquire) {
            return;
        }
        *self.result.lock().unwrap_or_else(|e| e.into_inner()) = Some(result);
        let one = 1u64;
        loop {
            // SAFETY: the descriptor and eight-byte value remain live during write.
            let n =
                unsafe { libc::write(self.completion.as_raw_fd(), (&one as *const u64).cast(), 8) };
            if n >= 0 || std::io::Error::last_os_error().kind() != std::io::ErrorKind::Interrupted {
                break;
            }
        }
    }
}

impl server::BinarySession for HardwareSession {
    fn completion_fd(&self) -> std::result::Result<Descriptor, server::BinaryError> {
        self.state
            .completion
            .try_clone()
            .map(Into::into)
            .map_err(|_| server::BinaryError::Unavailable("unavailable"))
    }
    /// Take the result once, without waiting for the helper or its mutex.
    fn result(&self) -> Option<std::result::Result<Vec<u8>, server::BinaryError>> {
        self.state.result.try_lock().ok()?.take()
    }
    fn cancel(&self) {
        self.state
            .cancelled
            .store(true, std::sync::atomic::Ordering::Release);
    }
}

impl Drop for HardwareSession {
    fn drop(&mut self) {
        server::BinarySession::cancel(self);
    }
}

fn profile_duration(path: &str) -> Option<std::result::Result<Duration, server::BinaryError>> {
    let (route, query) = path
        .split_once('?')
        .map_or((path, None), |(p, q)| (p, Some(q)));
    if route != "/debug/pprof/profile" {
        return None;
    }
    Some((|| {
        let seconds = match query {
            None => 30,
            Some(query) => {
                let value = query
                    .strip_prefix("seconds=")
                    .ok_or(server::BinaryError::BadRequest)?;
                if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
                    return Err(server::BinaryError::BadRequest);
                }
                value
                    .parse::<u64>()
                    .map_err(|_| server::BinaryError::BadRequest)?
            }
        };
        if !(1..=60).contains(&seconds) {
            return Err(server::BinaryError::BadRequest);
        }
        Ok(Duration::from_secs(seconds))
    })())
}

fn profile_request(
    controller: Option<&Arc<crate::profiling::ProfilingController>>,
    path: &str,
) -> Option<std::result::Result<server::BinaryResponse, server::BinaryError>> {
    let duration = profile_duration(path)?;
    Some((|| {
        let controller = controller.ok_or(server::BinaryError::NotFound)?;
        let duration = duration?;
        let session = controller.start(duration).map_err(profile_error)?;
        Ok(server::BinaryResponse {
            kind: server::BinaryKind::Profile,
            duration,
            session: Box::new(session),
        })
    })())
}

fn profile_error(error: crate::profiling::ProfileError) -> server::BinaryError {
    use crate::profiling::ProfileError;
    match error {
        ProfileError::Disabled => server::BinaryError::NotFound,
        ProfileError::InvalidDuration => server::BinaryError::BadRequest,
        ProfileError::Busy => server::BinaryError::Busy,
        error => server::BinaryError::Unavailable(error.code()),
    }
}

impl server::BinarySession for crate::profiling::ProfileSession {
    fn completion_fd(&self) -> std::result::Result<Descriptor, server::BinaryError> {
        self.completion_fd().map(Into::into).map_err(profile_error)
    }
    fn result(&self) -> Option<std::result::Result<Vec<u8>, server::BinaryError>> {
        self.result().map(|result| result.map_err(profile_error))
    }
    fn cancel(&self) {
        self.cancel();
    }
}

#[cfg(test)]
mod profile_http_tests {
    use super::*;
    #[test]
    fn profile_query_is_strict_and_disabled_is_not_found() {
        assert!(profile_duration("/metrics").is_none());
        for (query, seconds) in [("", 30), ("?seconds=1", 1), ("?seconds=60", 60)] {
            let path = format!("/debug/pprof/profile{query}");
            assert_eq!(
                profile_duration(&path),
                Some(Ok(Duration::from_secs(seconds)))
            );
            assert!(matches!(
                profile_request(None, &path),
                Some(Err(server::BinaryError::NotFound))
            ));
        }
        for query in [
            "?",
            "?seconds=",
            "?seconds=0",
            "?seconds=61",
            "?seconds=-1",
            "?seconds=+1",
            "?seconds=1.5",
            "?seconds=18446744073709551616",
            "?seconds=1&seconds=2",
            "?seconds=1&other=2",
            "?other=1",
            "?seconds=%31",
            "?seconds=1#x",
        ] {
            assert_eq!(
                profile_duration(&format!("/debug/pprof/profile{query}")),
                Some(Err(server::BinaryError::BadRequest)),
                "{query}"
            );
        }
        assert_eq!(
            profile_error(crate::profiling::ProfileError::Busy),
            server::BinaryError::Busy
        );
        assert_eq!(
            profile_error(crate::profiling::ProfileError::PermissionDenied),
            server::BinaryError::Unavailable("permission_denied")
        );
    }
}
pub(super) fn get(
    telemetry: &Telemetry,
    path: &str,
    admission_usable: bool,
    mut output: &mut dyn Write,
) -> std::result::Result<Response, std::fmt::Error> {
    let now = telemetry.failures.now();
    let ready = admission_usable && telemetry.health.ready_at(now);
    const LIVE_PROBE: ::telemetry::health::Probe =
        ::telemetry::health::Probe::new("ok\n", "not live\n");
    const READY_PROBE: ::telemetry::health::Probe =
        ::telemetry::health::Probe::new("ready\n", "not ready\n");
    let (response, body, event) = match path {
        "/healthz" => {
            let (response, body) = LIVE_PROBE.response(telemetry.health.live_at(now));
            (response, body, Event::DiagnosticHealth)
        }
        "/readyz" => {
            let (response, body) = READY_PROBE.response(ready);
            (response, body, Event::DiagnosticReady)
        }
        "/metrics" => (Response::Metrics, "", Event::DiagnosticMetrics),
        "/debug/membership" => (Response::Text, "", Event::DiagnosticMetrics),
        "/debug/failures"
        | "/debug/aead"
        | "/debug/decrypt-failures"
        | "/debug/send-tag"
        | "/debug/terminal"
        | "/debug/gate-events"
        | "/debug/flight-expiry"
        | "/debug/admission-final"
        | "/debug/candidate-final" => (Response::Text, "", Event::DiagnosticFailures),
        _ => return Ok(Response::NotFound),
    };
    telemetry.metrics.record(event, 1);
    if path == "/metrics" {
        let mut stream = MetricsStream::new(telemetry, ready, now);
        while server::TextStream::next(&mut stream, output)? {}
    } else if path == "/debug/membership" {
        match telemetry.membership.get().and_then(|read| read().ok()) {
            Some(state) => state.write(&mut output)?,
            None => output.write_str("unavailable fully_applied=0\n")?,
        }
    } else if path == "/debug/send-tag" {
        telemetry.send_tag.write(&mut output)?;
    } else if path == "/debug/aead" {
        telemetry.failures.write_aead(&mut output)?;
    } else if path == "/debug/decrypt-failures" {
        telemetry.failures.write_decrypt(&mut output)?;
    } else if path == "/debug/candidate-final" {
        telemetry.failures.write_candidate_final(&mut output)?;
    } else if path == "/debug/gate-events" {
        telemetry.failures.write_gate_events(&mut output)?;
    } else if path == "/debug/flight-expiry" {
        telemetry.failures.write_flight_expiry(&mut output)?;
    } else if path == "/debug/terminal" {
        telemetry.failures.write_protected(false, &mut output)?;
    } else if path == "/debug/admission-final" {
        telemetry.failures.write_protected(true, &mut output)?;
    } else if path == "/debug/failures" {
        telemetry.failures.write(&mut output)?;
    } else {
        output.write_str(body)?;
    }
    Ok(response)
}

/// Why a hardware report could not be encoded.
#[derive(Debug, PartialEq, Eq)]
enum Encode {
    /// The pretty JSON plus trailing newline would exceed the byte limit.
    TooLarge,
    /// Serialization failed for another reason.
    Failed,
}

/// Encode `report` as pretty JSON with a trailing newline, failing as soon as the output
/// would exceed `limit` bytes so oversized reports never allocate the full body.
fn encode_report(
    report: &crate::hardware::Report,
    limit: usize,
) -> std::result::Result<String, Encode> {
    let mut writer = Bounded::new(limit);
    let result = serde_json::to_writer_pretty(&mut writer, report)
        .map_err(|_| ())
        .and_then(|()| std::io::Write::write_all(&mut writer, b"\n").map_err(|_| ()));
    match result {
        Ok(()) => String::from_utf8(writer.buffer).map_err(|_| Encode::Failed),
        Err(()) if writer.exceeded => Err(Encode::TooLarge),
        Err(()) => Err(Encode::Failed),
    }
}

/// `io::Write` sink that refuses to grow past a fixed byte limit.
///
/// The buffer is allocated once with capacity `limit` so amortized growth can
/// never exceed the limit charged in `RESERVED_BYTES`.
struct Bounded {
    buffer: Vec<u8>,
    limit: usize,
    exceeded: bool,
}

impl Bounded {
    fn new(limit: usize) -> Self {
        Self {
            buffer: Vec::with_capacity(limit),
            limit,
            exceeded: false,
        }
    }
}

impl std::io::Write for Bounded {
    fn write(&mut self, data: &[u8]) -> std::io::Result<usize> {
        if data.len() > self.limit - self.buffer.len() {
            self.exceeded = true;
            return Err(std::io::ErrorKind::FileTooLarge.into());
        }
        self.buffer.extend_from_slice(data);
        Ok(data.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

#[cfg(test)]
mod encode_report_tests {
    use super::*;
    use std::io::Write as _;

    #[test]
    fn bounded_writer_accepts_exact_limit_and_rejects_one_more() {
        let mut writer = Bounded::new(4);
        assert!(writer.write_all(b"abcd").is_ok());
        assert_eq!(writer.buffer, b"abcd");
        assert!(!writer.exceeded);
        assert!(writer.write_all(b"e").is_err());
        assert!(writer.exceeded);
        assert_eq!(writer.buffer, b"abcd");
        let mut writer = Bounded::new(4);
        assert!(writer.write_all(b"abcde").is_err());
        assert!(writer.exceeded);
        assert!(writer.buffer.is_empty());
        assert_eq!(writer.buffer.capacity(), 4);
    }

    #[test]
    fn reserved_bytes_charges_hardware_working_budget_and_result() {
        assert_eq!(
            RESERVED_BYTES,
            server::RESERVED_BYTES
                + server::JSON_CONTROL_SLOTS * racer_io::scoped::SUBMISSION_BYTES
                + HARDWARE_BODY_BYTES
                + crate::hardware::WORKING_BYTES
                + HARDWARE_STACK_BYTES
        );
        assert_eq!(HARDWARE_STACK_BYTES, 2 * 1024 * 1024);
        assert_eq!(
            HARDWARE_BYTES,
            HARDWARE_BODY_BYTES + crate::hardware::WORKING_BYTES + HARDWARE_STACK_BYTES
        );
        let report = crate::hardware::Report::default();
        let body = encode_report(&report, HARDWARE_BODY_BYTES).unwrap();
        assert!(body.capacity() <= HARDWARE_BODY_BYTES);
    }

    #[test]
    fn encode_report_matches_pretty_json_and_enforces_limit() {
        let report = crate::hardware::Report {
            errors: vec!["x".into()],
            ..Default::default()
        };
        let expected = serde_json::to_string_pretty(&report).unwrap() + "\n";
        assert_eq!(
            encode_report(&report, expected.len()).as_deref(),
            Ok(expected.as_str())
        );
        assert_eq!(
            encode_report(&report, expected.len() - 1),
            Err(Encode::TooLarge)
        );
        assert_eq!(encode_report(&report, 0), Err(Encode::TooLarge));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) mod http {
        use super::*;
        use crate::model::RequestId;
        use std::io::Read;
        use std::io::Write as IoWrite;
        use std::net::TcpStream;
        use std::task::Context;

        fn scope() -> RequestScope {
            RequestScope::host(RequestId([0; 16]), Instant::now() + Duration::from_secs(15))
                .unwrap()
        }
        fn setup() -> (
            Rc<flow_control::Quotas<AdmissionPolicy>>,
            Rc<Reactor>,
            Rc<DiagnosticIo>,
        ) {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &crate::test_support::env()));
            let io = Rc::new(
                DiagnosticIo::attach(reactor.clone(), admission.clone())
                    .expect("real io_uring diagnostics attachment"),
            );
            (admission, reactor, io)
        }
        fn poll_server(server: &mut Operation<'_, ()>, reactor: &Reactor) {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(server.as_mut().poll(&mut cx).is_pending());
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        fn exchange_raw(
            address: std::net::SocketAddr,
            request: &[u8],
            server: &mut Operation<'_, ()>,
            reactor: &Reactor,
        ) -> Vec<u8> {
            let mut socket = TcpStream::connect(address).unwrap();
            socket.set_nonblocking(true).unwrap();
            let mut sent = 0;
            let mut response = Vec::new();
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                assert!(Instant::now() < deadline, "raw diagnostic exchange stalled");
                poll_server(server, reactor);
                if sent < request.len() {
                    // Force fragmentation across worker polls, including CRLF boundaries.
                    match socket.write(&request[sent..(sent + 7).min(request.len())]) {
                        Ok(count) => sent += count,
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(error) => panic!("{error}"),
                    }
                }
                let mut bytes = vec![0; 64 * 1024];
                match socket.read(&mut bytes) {
                    Ok(0) => break,
                    Ok(count) => response.extend_from_slice(&bytes[..count]),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(error) => panic!("{error}"),
                }
                // Streamed `/metrics` may exceed one buffer; still bound runaway output.
                assert!(response.len() <= 16 * MAX_RESPONSE_BYTES);
            }
            response
        }
        fn finish(mut server: Operation<'_, ()>, scope: &RequestScope, reactor: &Reactor) {
            scope.cancel().unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(matches!(
                server.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            drop(server);
            let mut drain = reactor.drain();
            let deadline = Instant::now() + Duration::from_secs(3);
            loop {
                if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                    result.unwrap();
                    break;
                }
                assert!(Instant::now() < deadline);
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        }
        fn assert_response(response: &[u8], status: &str, body: Option<&str>) {
            let text = std::str::from_utf8(response).unwrap();
            assert!(
                text.starts_with(&format!("HTTP/1.1 {status}\r\n")),
                "{text}"
            );
            let (head, actual_body) = text.split_once("\r\n\r\n").unwrap();
            let actual_body = if head.contains("Transfer-Encoding: chunked") {
                assert!(!head.contains("Content-Length"));
                dechunk(actual_body)
            } else {
                let length: usize = head
                    .lines()
                    .find_map(|line| line.strip_prefix("Content-Length: "))
                    .unwrap()
                    .parse()
                    .unwrap();
                assert_eq!(actual_body.len(), length);
                actual_body.to_owned()
            };
            assert!(head.contains("Connection: close"));
            if let Some(body) = body {
                assert_eq!(actual_body, body);
            }
        }
        /// Decode a complete chunked body, requiring its terminating chunk.
        fn dechunk(mut chunked: &str) -> String {
            let mut body = String::new();
            loop {
                let (size, rest) = chunked.split_once("\r\n").unwrap();
                let size = usize::from_str_radix(size, 16).unwrap();
                if size == 0 {
                    assert_eq!(rest, "\r\n");
                    return body;
                }
                body.push_str(&rest[..size]);
                chunked = rest[size..].strip_prefix("\r\n").unwrap();
            }
        }
        fn good_resources() -> crate::telemetry::Resources {
            crate::telemetry::Resources {
                workers_usable: true,
                storage_usable: true,
                listeners_usable: true,
                membership_usable: true,
                admission_usable: true,
                credentials_valid_until: Some(Instant::now() + Duration::from_secs(30)),
                observed_until: Some(Instant::now() + Duration::from_secs(30)),
            }
        }

        fn diagnostic_text(telemetry: &Telemetry, path: &str) -> String {
            let (_, reactor, io) = setup();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let request = format!(
                "GET {path} HTTP/1.1\r\nHost: local\r\nAuthorization: synthetic-secret\r\n\r\n"
            );
            let bytes = exchange_raw(address, request.as_bytes(), &mut server, &reactor);
            assert_response(&bytes, "200 OK", None);
            // Keep the head, but decode a streamed body.
            let raw = std::str::from_utf8(&bytes).unwrap();
            let (head, body) = raw.split_once("\r\n\r\n").unwrap();
            let text = if head.contains("Transfer-Encoding: chunked") {
                format!("{head}\r\n\r\n{}", dechunk(body))
            } else {
                assert!(bytes.len() < MAX_RESPONSE_BYTES);
                raw.to_owned()
            };
            assert!(!text.contains("synthetic"));
            finish(server, &scope, &reactor);
            text
        }

        /// A standalone hardware charge for sessions started without a server.
        fn test_charge() -> HardwareCharge {
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            Arc::new(
                admission
                    .reserve(None, ResourceClass::RequestContext, HARDWARE_BYTES)
                    .unwrap(),
            )
        }

        /// Exchange one GET without asserting the status.
        fn diagnostic_response(telemetry: &Telemetry, path: &str) -> String {
            let (_, reactor, io) = setup();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let request = format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n");
            let bytes = exchange_raw(address, request.as_bytes(), &mut server, &reactor);
            finish(server, &scope, &reactor);
            String::from_utf8(bytes).unwrap()
        }

        fn hardware_counter(telemetry: &Telemetry) -> bool {
            let mut text = String::new();
            telemetry.metrics.write_prometheus(&mut text).unwrap();
            text.lines()
                .any(|line| line == "racer_diagnostic_hardware_total 1")
        }

        /// Install a hardware job builder returning `report` from the helper thread.
        fn set_hardware(telemetry: &Telemetry, report: fn() -> crate::hardware::Report) {
            assert!(
                telemetry
                    .hardware
                    .set(Rc::new(move || -> HardwareJob {
                        Box::new(move |_| Ok(report()))
                    }))
                    .is_ok()
            );
        }

        #[test]
        fn hardware_endpoint_serves_json_and_counts_requests() {
            let telemetry = Telemetry::default();
            set_hardware(&telemetry, || crate::hardware::Report {
                errors: vec!["rdma_devices: example".into()],
                ..Default::default()
            });
            // `Telemetry::default()` has pprof disabled; hardware still works.
            assert!(telemetry.profiling.is_none());
            let text = diagnostic_text(&telemetry, "/debug/hardware");
            let (head, body) = text.split_once("\r\n\r\n").unwrap();
            assert!(head.contains("\r\nContent-Type: application/json\r\n"));
            assert!(!head.contains("Content-Disposition"));
            assert!(body.ends_with("}\n"));
            let value: serde_json::Value = serde_json::from_str(body).unwrap();
            for key in ["block_devices", "rdma_devices", "racer", "errors"] {
                assert!(value.get(key).is_some(), "{key}");
            }
            assert_eq!(value["errors"][0], "rdma_devices: example");
            assert!(hardware_counter(&telemetry));
            // The finished helper released the single-flight slot.
            assert_eq!(telemetry.hardware_active.borrow().strong_count(), 0);
        }

        #[test]
        fn hardware_endpoint_unavailable_oversize_and_exact_path() {
            let telemetry = Telemetry::default();
            let text = diagnostic_response(&telemetry, "/debug/hardware");
            assert_response(
                text.as_bytes(),
                "503 Service Unavailable",
                Some("unavailable\n"),
            );
            assert!(hardware_counter(&telemetry));
            let text = diagnostic_response(&telemetry, "/debug/hardware?x");
            assert_response(text.as_bytes(), "404 Not Found", Some("not found\n"));
            let text = diagnostic_response(&telemetry, "/debug/hardware/");
            assert_response(text.as_bytes(), "404 Not Found", Some("not found\n"));
            set_hardware(&telemetry, || crate::hardware::Report {
                errors: vec!["x".repeat(crate::telemetry::HARDWARE_BODY_BYTES)],
                ..Default::default()
            });
            let text = diagnostic_response(&telemetry, "/debug/hardware");
            assert_response(
                text.as_bytes(),
                "503 Service Unavailable",
                Some("response_too_large\n"),
            );
        }

        #[test]
        fn hardware_budget_exhaustion_is_response_too_large() {
            let telemetry = Telemetry::default();
            assert!(
                telemetry
                    .hardware
                    .set(Rc::new(|| -> HardwareJob {
                        Box::new(|walk| {
                            // The helper's walk carries exactly the fixed budget.
                            assert_eq!(walk.remaining(), crate::hardware::WORKING_BYTES);
                            walk.charge(crate::hardware::WORKING_BYTES + 1)?;
                            Ok(crate::hardware::Report::default())
                        })
                    }))
                    .is_ok()
            );
            let text = diagnostic_response(&telemetry, "/debug/hardware");
            assert_response(
                text.as_bytes(),
                "503 Service Unavailable",
                Some("response_too_large\n"),
            );
        }

        #[test]
        fn hardware_cancel_reaches_the_running_walk() {
            use ::telemetry::server::BinarySession;
            use std::sync::mpsc;
            let (started, running) = mpsc::channel::<()>();
            let (observed, seen) = mpsc::channel::<crate::hardware::Abort>();
            let job: HardwareJob = Box::new(move |walk| {
                let _ = started.send(());
                let until = Instant::now() + Duration::from_secs(5);
                let abort = loop {
                    if let Err(abort) = walk.check() {
                        break abort;
                    }
                    assert!(Instant::now() < until, "cancel never reached the walk");
                    std::thread::sleep(Duration::from_millis(1));
                };
                let _ = observed.send(abort);
                Err(abort)
            });
            let session =
                HardwareSession::start(job, crate::telemetry::HARDWARE_BODY_BYTES, test_charge())
                    .unwrap();
            running.recv_timeout(Duration::from_secs(5)).unwrap();
            session.cancel();
            assert_eq!(
                seen.recv_timeout(Duration::from_secs(5)).unwrap(),
                crate::hardware::Abort::Cancelled
            );
            assert!(session.result().is_none(), "cancelled result is discarded");
        }

        #[test]
        fn hardware_discovery_runs_off_worker_and_is_single_flight() {
            use std::sync::mpsc;
            let telemetry = Telemetry::default();
            let (release, gate) = mpsc::channel::<()>();
            let gate = Arc::new(Mutex::new(gate));
            let job_gate = gate.clone();
            assert!(
                telemetry
                    .hardware
                    .set(Rc::new(move || -> HardwareJob {
                        let gate = job_gate.clone();
                        Box::new(move |_| {
                            // Simulate a slow sysfs read until the test releases it.
                            let _ = gate.lock().unwrap().recv();
                            Ok(crate::hardware::Report::default())
                        })
                    }))
                    .is_ok()
            );
            let (_, reactor, io) = setup();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let mut slow = TcpStream::connect(address).unwrap();
            slow.write_all(b"GET /debug/hardware HTTP/1.1\r\nHost: local\r\n\r\n")
                .unwrap();
            slow.set_nonblocking(true).unwrap();
            let until = Instant::now() + Duration::from_secs(5);
            while telemetry.hardware_active.borrow().strong_count() == 0 {
                assert!(Instant::now() < until);
                poll_server(&mut server, &reactor);
            }
            // The worker keeps serving health checks while discovery is blocked.
            let bytes = exchange_raw(
                address,
                b"GET /healthz HTTP/1.1\r\nHost: local\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&bytes, "200 OK", None);
            // A concurrent discovery is refused rather than starting a second helper.
            let bytes = exchange_raw(
                address,
                b"GET /debug/hardware HTTP/1.1\r\nHost: local\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&bytes, "409 Conflict", Some("busy\n"));
            release.send(()).unwrap();
            let mut response = Vec::new();
            loop {
                assert!(Instant::now() < until, "hardware response stalled");
                poll_server(&mut server, &reactor);
                let mut bytes = [0; 512];
                match slow.read(&mut bytes) {
                    Ok(0) => break,
                    Ok(count) => response.extend_from_slice(&bytes[..count]),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(error) => panic!("{error}"),
                }
            }
            assert_response(&response, "200 OK", None);
            assert!(
                std::str::from_utf8(&response)
                    .unwrap()
                    .contains("\r\nContent-Type: application/json\r\n")
            );
            finish(server, &scope, &reactor);
            assert_eq!(telemetry.hardware_active.borrow().strong_count(), 0);
        }

        #[test]
        fn hardware_cancelled_helper_finishes_detached_and_stays_busy_until_done() {
            use std::sync::mpsc;
            let (release, gate) = mpsc::channel::<()>();
            let (done, finished) = mpsc::channel::<()>();
            let job: HardwareJob = Box::new(move |_| {
                let _ = gate.recv();
                let _ = done.send(());
                Ok(crate::hardware::Report::default())
            });
            let session =
                HardwareSession::start(job, crate::telemetry::HARDWARE_BODY_BYTES, test_charge())
                    .unwrap();
            let weak = Arc::downgrade(&session.state);
            // Dropping the session cancels without joining the blocked helper.
            drop(session);
            assert!(weak.strong_count() > 0, "helper still owns its state");
            release.send(()).unwrap();
            finished.recv_timeout(Duration::from_secs(5)).unwrap();
            let until = Instant::now() + Duration::from_secs(5);
            while weak.strong_count() > 0 {
                assert!(Instant::now() < until);
                std::thread::sleep(Duration::from_millis(1));
            }
        }

        #[test]
        fn hardware_charge_outlives_teardown_until_blocked_helper_finishes() {
            use std::sync::mpsc;
            let telemetry = Telemetry::default();
            let (release, gate) = mpsc::channel::<()>();
            let gate = Arc::new(Mutex::new(gate));
            let job_gate = gate.clone();
            assert!(
                telemetry
                    .hardware
                    .set(Rc::new(move || -> HardwareJob {
                        let gate = job_gate.clone();
                        Box::new(move |_| {
                            // A sysfs read that stays blocked across teardown.
                            let _ = gate.lock().unwrap().recv();
                            Ok(crate::hardware::Report::default())
                        })
                    }))
                    .is_ok()
            );
            let (admission, reactor, io) = setup();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let mut slow = TcpStream::connect(address).unwrap();
            slow.write_all(b"GET /debug/hardware HTTP/1.1\r\nHost: local\r\n\r\n")
                .unwrap();
            let until = Instant::now() + Duration::from_secs(5);
            while telemetry.hardware_active.borrow().strong_count() == 0 {
                assert!(Instant::now() < until);
                poll_server(&mut server, &reactor);
            }
            // Tear down the session, the server, and the diagnostics owner while
            // the helper is still blocked.
            finish(server, &scope, &reactor);
            drop(slow);
            drop(reactor);
            let class = ResourceClass::RequestContext;
            assert!(
                admission.used(class) >= HARDWARE_BYTES,
                "blocked helper keeps its hardware charge after teardown"
            );
            assert!(
                telemetry.hardware_active.borrow().strong_count() > 0,
                "a later request stays busy while the charge is held"
            );
            release.send(()).unwrap();
            while telemetry.hardware_active.borrow().strong_count() > 0 {
                assert!(Instant::now() < until, "helper never finished");
                std::thread::sleep(Duration::from_millis(1));
            }
            assert_eq!(admission.used(class), 0, "helper returns the charge");
        }

        #[test]
        fn hardware_session_publishes_then_signals() {
            use ::telemetry::server::BinarySession;
            let job: HardwareJob = Box::new(|_| Ok(crate::hardware::Report::default()));
            let session =
                HardwareSession::start(job, crate::telemetry::HARDWARE_BODY_BYTES, test_charge())
                    .unwrap();
            let fd = session.state.completion.try_clone().unwrap();
            let mut poll = libc::pollfd {
                fd: std::os::fd::AsRawFd::as_raw_fd(&fd),
                events: libc::POLLIN,
                revents: 0,
            };
            // SAFETY: one valid pollfd for the duration of the call.
            assert_eq!(unsafe { libc::poll(&mut poll, 1, 5000) }, 1);
            let body = session.result().unwrap().unwrap();
            let expected =
                serde_json::to_string_pretty(&crate::hardware::Report::default()).unwrap() + "\n";
            assert_eq!(body, expected.as_bytes());
            assert!(session.result().is_none(), "result is taken once");
            let tiny: HardwareJob = Box::new(|_| Ok(crate::hardware::Report::default()));
            let session = HardwareSession::start(tiny, 1, test_charge()).unwrap();
            // SAFETY: one valid pollfd for the duration of the call.
            poll.fd = std::os::fd::AsRawFd::as_raw_fd(&session.state.completion);
            assert_eq!(unsafe { libc::poll(&mut poll, 1, 5000) }, 1);
            assert_eq!(
                session.result(),
                Some(Err(::telemetry::server::BinaryError::Unavailable(
                    "response_too_large"
                )))
            );
        }

        #[test]
        fn membership_endpoint_is_bounded_and_not_a_readiness_alias() {
            let telemetry = Telemetry::default();
            let text = diagnostic_text(&telemetry, "/debug/membership");
            assert_response(
                text.as_bytes(),
                "200 OK",
                Some("unavailable fully_applied=0\n"),
            );
            assert!(
                telemetry
                    .membership
                    .set(Rc::new(|| Ok(MembershipDiagnostic {
                        accepted_sequence: u64::MAX,
                        accepted_membership: 7,
                        accepted_hash: [0xab; 32],
                        expected_workers: 2,
                        matching_workers: 2,
                        ..Default::default()
                    })))
                    .is_ok()
            );
            let text = diagnostic_text(&telemetry, "/debug/membership");
            assert!(text.contains("accepted_sequence=18446744073709551615 accepted_membership=7"));
            assert!(text.contains(&format!("accepted_membership_hash={}", "ab".repeat(32))));
            assert!(text.ends_with("matching_workers=2 fully_applied=1\n"));
            assert!(!telemetry.health.ready());
            assert!(text.len() < 1024);
        }

        #[test]
        fn resource_and_lifecycle_changes_are_observable_at_both_health_endpoints() {
            let (_, reactor, io) = setup();
            let telemetry = Telemetry::default();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let mut observe = |ready: bool, live: bool| {
                for (path, healthy, body) in [
                    (
                        "/readyz",
                        ready,
                        if ready { "ready\n" } else { "not ready\n" },
                    ),
                    ("/healthz", live, if live { "ok\n" } else { "not live\n" }),
                ] {
                    let response = exchange_raw(
                        address,
                        format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes(),
                        &mut server,
                        &reactor,
                    );
                    assert_response(
                        &response,
                        if healthy {
                            "200 OK"
                        } else {
                            "503 Service Unavailable"
                        },
                        Some(body),
                    );
                }
            };
            observe(false, true);
            assert_eq!(
                telemetry.health.transition(crate::telemetry::State::Ready),
                Err(Error::Unavailable)
            );
            let good = good_resources();
            telemetry.health.observe(good).unwrap();
            telemetry
                .health
                .transition(crate::telemetry::State::Ready)
                .unwrap();
            observe(true, true);
            for bad in [
                crate::telemetry::Resources {
                    workers_usable: false,
                    ..good
                },
                crate::telemetry::Resources {
                    storage_usable: false,
                    ..good
                },
                crate::telemetry::Resources {
                    listeners_usable: false,
                    ..good
                },
                crate::telemetry::Resources {
                    membership_usable: false,
                    ..good
                },
                crate::telemetry::Resources {
                    admission_usable: false,
                    ..good
                },
                crate::telemetry::Resources {
                    credentials_valid_until: Some(Instant::now()),
                    ..good
                },
                crate::telemetry::Resources {
                    credentials_valid_until: None,
                    ..good
                },
                crate::telemetry::Resources {
                    observed_until: Some(Instant::now()),
                    ..good
                },
                crate::telemetry::Resources {
                    observed_until: None,
                    ..good
                },
            ] {
                telemetry.health.observe(bad).unwrap();
                observe(false, true);
                telemetry.health.observe(good).unwrap();
                observe(true, true);
            }
            telemetry
                .health
                .transition(crate::telemetry::State::Draining)
                .unwrap();
            observe(false, true);
            assert_eq!(
                telemetry.health.transition(crate::telemetry::State::Ready),
                Err(Error::Unavailable)
            );
            telemetry
                .health
                .transition(crate::telemetry::State::Stopped)
                .unwrap();
            observe(false, false);
            assert_eq!(
                telemetry
                    .health
                    .transition(crate::telemetry::State::Starting),
                Err(Error::Unavailable)
            );
            finish(server, &scope, &reactor);
        }

        #[test]
        fn diagnostic_probe_and_monitors_progress_under_sustained_queue_pressure() {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.queue_entries = std::num::NonZeroUsize::new(8).unwrap();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone(), &crate::test_support::env()));
            let io = Rc::new(DiagnosticIo::attach(reactor.clone(), admission.clone()).unwrap());
            let telemetry = Telemetry::default();
            telemetry.health.observe(good_resources()).unwrap();
            telemetry
                .health
                .transition(crate::telemetry::State::Ready)
                .unwrap();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let mut sockets: Vec<_> = ["/readyz", "/metrics", "/debug/failures", "/healthz"]
                .into_iter()
                .map(|path| {
                    let mut socket = TcpStream::connect(address).unwrap();
                    socket
                        .write_all(format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes())
                        .unwrap();
                    socket.set_nonblocking(true).unwrap();
                    (socket, Vec::new(), false)
                })
                .collect();
            // Accept one connection first, then let ordinary work occupy every available
            // slot before its receive is submitted. Before isolation this resets the probe.
            let deadline = Instant::now() + Duration::from_secs(1);
            while telemetry.metrics.count(Event::DiagnosticAccepted) == 0 {
                assert!(Instant::now() < deadline);
                poll_server(&mut server, &reactor);
            }
            let (reader, _writer) = std::os::unix::net::UnixStream::pair().unwrap();
            let reader = Rc::new(Descriptor::from(reader));
            let mut pressure = Vec::new();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..=8 {
                let mut wait = reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope);
                match wait.as_mut().poll(&mut cx) {
                    Poll::Pending => pressure.push(wait),
                    Poll::Ready(Err(Error::Overloaded)) => break,
                    _ => panic!("unexpected pressure result"),
                }
            }
            assert!(!pressure.is_empty());
            assert_eq!(pressure.len(), 8 - CONTROL_SLOTS);
            // Payload work also consumes all remaining bookkeeping memory. Diagnostics
            // must use their existing startup charge, not acquire shared bytes per SQE.
            let _memory_pressure = admission
                .reserve(
                    None,
                    ResourceClass::RequestContext,
                    admission.limit(ResourceClass::RequestContext)
                        - admission.used(ResourceClass::RequestContext),
                )
                .unwrap();
            let deadline = Instant::now() + Duration::from_secs(1);
            while sockets.iter().any(|(_, _, done)| !done) {
                assert!(
                    Instant::now() < deadline,
                    "diagnostics starved by ordinary queue entries"
                );
                poll_server(&mut server, &reactor);
                assert!(reactor.in_flight() <= 8);
                assert!(
                    telemetry.metrics.gauge(Gauge::DiagnosticConnections) <= MAX_CONNECTIONS as u64
                );
                for (socket, response, done) in &mut sockets {
                    if *done {
                        continue;
                    }
                    let mut bytes = [0; 4096];
                    match socket.read(&mut bytes) {
                        Ok(0) => {
                            assert_response(response, "200 OK", None);
                            *done = true;
                        }
                        Ok(count) => response.extend_from_slice(&bytes[..count]),
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(error) => {
                            panic!("diagnostic reset under ordinary queue pressure: {error}")
                        }
                    }
                }
            }
            assert_eq!(telemetry.metrics.count(Event::DiagnosticIoError), 0);
            assert_eq!(telemetry.metrics.count(Event::DiagnosticTimeout), 0);
            // Isolation must not turn genuinely stale/unusable health into a ready result.
            telemetry
                .health
                .observe(crate::telemetry::Resources {
                    observed_until: Some(Instant::now()),
                    ..good_resources()
                })
                .unwrap();
            let response = exchange_raw(
                address,
                b"GET /readyz HTTP/1.1\r\nHost: local\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
            admission.stop();
            let response = exchange_raw(
                address,
                b"GET /readyz HTTP/1.1\r\nHost: local\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
            drop(_memory_pressure);
            drop(pressure);
            finish(server, &scope, &reactor);
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
        }

        #[test]
        fn diagnostic_accept_recovers_after_full_entry_table() {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.queue_entries = std::num::NonZeroUsize::new(8).unwrap();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone(), &crate::test_support::env()));
            let io = Rc::new(DiagnosticIo::attach(reactor.clone(), admission.clone()).unwrap());
            let telemetry = Telemetry::default();
            telemetry.health.observe(good_resources()).unwrap();
            telemetry
                .health
                .transition(crate::telemetry::State::Ready)
                .unwrap();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let (reader, _writer) = std::os::unix::net::UnixStream::pair().unwrap();
            let reader = Rc::new(Descriptor::from(reader));
            let mut pressure = Vec::new();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            // The ordinary partition can fill, but cannot steal the listener's slots.
            for _ in 0..8 - CONTROL_SLOTS {
                let mut wait = reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope);
                assert!(wait.as_mut().poll(&mut cx).is_pending());
                pressure.push(wait);
            }
            assert_eq!(reactor.in_flight(), 8 - CONTROL_SLOTS);
            for _ in 0..32 {
                assert!(server.as_mut().poll(&mut cx).is_pending());
                assert_eq!(reactor.in_flight(), 8 - CONTROL_SLOTS + 1);
            }
            drop(pressure);
            for path in ["/readyz", "/metrics"] {
                let response = exchange_raw(
                    address,
                    format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes(),
                    &mut server,
                    &reactor,
                );
                assert_response(&response, "200 OK", None);
            }
            finish(server, &scope, &reactor);
            assert_eq!(reactor.in_flight(), 0);
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
        }

        #[test]
        fn failure_endpoint_exports_full_ring_with_maximum_numeric_fields() {
            use crate::model::AttemptId;
            use crate::model::WorkerId;
            use crate::telemetry::Detail;
            use crate::telemetry::FAILURE_CAPACITY as CAPACITY;
            use crate::telemetry::Failure;
            use crate::telemetry::Stage;
            let telemetry = Telemetry::default();
            let observer = telemetry.failures.observer(WorkerId(u16::MAX));
            let scope = RequestScope::host(
                RequestId([255; 16]),
                Instant::now() + Duration::from_secs(10),
            )
            .unwrap();
            for _ in 0..CAPACITY + 1 {
                observer.record(
                    Failure::new(
                        Stage::PeerReceiveAdmission,
                        Error::UnsatisfiableRangeWithLength(u64::MAX),
                    )
                    .request(&scope)
                    .attempt(AttemptId([255; 16]))
                    .detail(Detail::Resource {
                        class: ResourceClass::OutboundConnection,
                        used: usize::MAX,
                        limit: usize::MAX,
                        requested: usize::MAX,
                        cache_used: Some(usize::MAX),
                        cache_limit: Some(usize::MAX),
                    }),
                );
            }
            let text = diagnostic_text(&telemetry, "/debug/failures");
            assert_eq!(text.matches("stage=PeerReceiveAdmission").count(), CAPACITY);
            assert!(text.contains("sequence=2 worker=65535"));
            assert!(!text.contains("synthetic"));
        }

        #[test]
        fn aead_endpoint_exports_full_ring_with_maximum_fields() {
            use crate::model::WorkerId;
            use crate::security::CryptoId;
            use crate::telemetry::AEAD_CAPACITY;
            use crate::telemetry::test_aead_failure;
            let telemetry = Telemetry::default();
            let observer = telemetry.failures.observer(WorkerId(u16::MAX));
            for _ in 0..AEAD_CAPACITY + 1 {
                observer.record_aead(
                    CryptoId {
                        worker: WorkerId(u16::MAX),
                        generation: u64::MAX,
                        sequence: u64::MAX,
                    },
                    test_aead_failure(),
                );
            }
            let text = diagnostic_text(&telemetry, "/debug/aead");
            assert_eq!(text.matches(" supplier=").count(), AEAD_CAPACITY);
            assert!(text.contains("total=65 retained=64 overwritten=1 capacity=64"));
            assert!(!text.contains("synthetic"));
        }

        #[test]
        fn candidate_final_raw_endpoint_does_not_echo_request_secrets() {
            let telemetry = Telemetry::default();
            let observer = telemetry.failures.observer(crate::model::WorkerId(2));
            observer.candidate_final(CandidateFinal {
                failure: Failure::new(Stage::CandidateExhausted, Error::Unavailable),
                operation: CandidateOperation::Page,
                page: 1,
                membership: 7,
                trail: CandidateTrail::default(),
            });
            let text = diagnostic_text(&telemetry, "/debug/candidate-final");
            assert!(
                text.contains("coverage=candidate_operation_final"),
                "{text}"
            );
            assert!(
                text.contains("operation=Page page=1 membership=7"),
                "{text}"
            );
            for secret in ["synthetic", "Authorization", "X-Key"] {
                assert!(!text.contains(secret), "{text}");
            }
        }

        #[test]
        fn send_tag_endpoint_is_empty_when_disabled_and_does_not_echo_headers() {
            let telemetry = Telemetry::default();
            let text = diagnostic_text(&telemetry, "/debug/send-tag");
            assert!(text.contains("sampled=0"));
            assert!(!text.contains("synthetic"));
            telemetry.send_tag.fill_test_ring();
            let text = diagnostic_text(&telemetry, "/debug/send-tag");
            assert_eq!(text.matches("status=verified").count(), 64);
        }

        #[test]
        fn raw_endpoints_fragmentation_readiness_redaction_and_data_admission_stop() {
            let (admission, reactor, io) = setup();
            let telemetry = Telemetry::default();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io.clone(), &scope);
            // Diagnostics consume neither ordinary connection nor payload quota.
            let _connections = admission
                .reserve(
                    None,
                    ResourceClass::Connection,
                    admission.limit(ResourceClass::Connection),
                )
                .unwrap();
            let _payload = admission
                .reserve(
                    None,
                    ResourceClass::Plaintext,
                    admission.limit(ResourceClass::Plaintext),
                )
                .unwrap();
            let get = |path: &str| {
                format!(
                    "GET {path} HTTP/1.1\r\nHost: local\r\nAuthorization: synthetic-secret\r\nX-Key: synthetic-key\r\n\r\n"
                )
            };
            let response = exchange_raw(address, get("/healthz").as_bytes(), &mut server, &reactor);
            assert_response(&response, "200 OK", Some("ok\n"));
            let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
            assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
            telemetry.health.observe(good_resources()).unwrap();
            telemetry
                .health
                .transition(crate::telemetry::State::Ready)
                .unwrap();
            let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
            assert_response(&response, "200 OK", Some("ready\n"));
            // Stopping local admission overrides even a still-valid ready observation.
            admission.stop();
            let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
            assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
            telemetry
                .health
                .observe(crate::telemetry::Resources {
                    credentials_valid_until: Some(Instant::now()),
                    ..good_resources()
                })
                .unwrap();
            let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
            assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
            let response = exchange_raw(address, get("/metrics").as_bytes(), &mut server, &reactor);
            assert_response(&response, "200 OK", None);
            let text = std::str::from_utf8(&response).unwrap();
            assert!(text.contains("racer_diagnostic_ready_total 4\n"));
            assert!(text.contains("racer_requests_total 0\n"));
            assert!(text.contains("racer_request_errors_total 0\n"));
            assert!(text.contains("racer_active_requests 0\n"));
            assert!(text.contains("racer_ready 0\n"));
            assert!(!text.contains("synthetic"));
            assert!(!text.contains("Authorization"));
            telemetry
                .failures
                .observer(crate::model::WorkerId(1))
                .record(
                    crate::telemetry::Failure::new(
                        crate::telemetry::Stage::NextSlice,
                        Error::Unavailable,
                    )
                    .request(&scope),
                );
            let response = exchange_raw(
                address,
                get("/debug/failures").as_bytes(),
                &mut server,
                &reactor,
            );
            assert_response(&response, "200 OK", None);
            let text = std::str::from_utf8(&response).unwrap();
            assert!(text.contains("worker=1 stage=NextSlice error=Unavailable"));
            assert!(!text.contains("synthetic"));
            finish(server, &scope, &reactor);
            assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 0);
            drop(io);
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
        }

        #[test]
        fn raw_rejections_are_fixed_and_never_echo_untrusted_input() {
            let (_, reactor, io) = setup();
            let telemetry = Telemetry::default();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            for (request, status) in [
                (
                    "GET /secret-key HTTP/1.1\r\nHost: local\r\n\r\n",
                    "404 Not Found",
                ),
                (
                    "POST /metrics HTTP/1.1\r\nHost: local\r\n\r\n",
                    "405 Method Not Allowed",
                ),
                (
                    "GET /metrics HTTP/1.1\r\nHost: local\r\nContent-Length: 9\r\n\r\n",
                    "400 Bad Request",
                ),
                (
                    "GET /metrics HTTP/1.1\r\nHost: local\r\nTransfer-Encoding: chunked\r\n\r\n",
                    "400 Bad Request",
                ),
                (
                    "GET /metrics HTTP/1.1\r\nHost: local\r\nHost: other\r\n\r\n",
                    "400 Bad Request",
                ),
            ] {
                let response = exchange_raw(address, request.as_bytes(), &mut server, &reactor);
                assert_response(&response, status, None);
                assert!(
                    !std::str::from_utf8(&response)
                        .unwrap()
                        .contains("secret-key")
                );
            }
            let mut huge = b"GET /metrics HTTP/1.1\r\nHost: local\r\nX: ".to_vec();
            huge.resize(MAX_REQUEST_BYTES, b'x');
            let response = exchange_raw(address, &huge, &mut server, &reactor);
            assert_response(
                &response,
                "431 Request Header Fields Too Large",
                Some("headers too large\n"),
            );
            assert_eq!(telemetry.metrics.count(Event::DiagnosticRejected), 6);
            finish(server, &scope, &reactor);
        }

        #[test]
        fn slow_socket_does_not_block_probes_and_abandonment_keeps_quota_until_fenced() {
            let (admission, reactor, io) = setup();
            let reserved_memory = admission.used(ResourceClass::RequestContext);
            let telemetry = Telemetry::default();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io.clone(), &scope);
            let mut slow = TcpStream::connect(address).unwrap();
            slow.write_all(b"GET /healthz HTTP/1.1\r\n").unwrap();
            for _ in 0..10 {
                poll_server(&mut server, &reactor);
            }
            assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 1);
            let response = exchange_raw(
                address,
                b"GET /healthz HTTP/1.1\r\nHost: local\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&response, "200 OK", Some("ok\n"));
            drop(server);
            drop(io);
            assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 1);
            assert_eq!(
                admission.used(ResourceClass::ControlProgress),
                CONTROL_SLOTS
            );
            assert!(admission.used(ResourceClass::RequestContext) >= reserved_memory);
            let mut drain = reactor.drain();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let deadline = Instant::now() + Duration::from_secs(3);
            loop {
                if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                    result.unwrap();
                    break;
                }
                assert!(Instant::now() < deadline);
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 0);
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
            assert!(admission.used(ResourceClass::RequestContext) < reserved_memory);
        }

        #[test]
        fn server_scope_preserves_metadata_cancellation_and_narrows_deadline() {
            use server::Scope;
            let mut parent = scope();
            parent.body_deadlines = Some((Instant::now(), parent.deadline.0));
            let shorter = parent.with_deadline(parent.deadline.0 - Duration::from_secs(1));
            let longer = parent.with_deadline(parent.deadline.0 + Duration::from_secs(1));
            assert_eq!(shorter.request, parent.request);
            assert_eq!(shorter.body_deadlines, parent.body_deadlines);
            assert_eq!(
                shorter.deadline.0,
                parent.deadline.0 - Duration::from_secs(1)
            );
            assert_eq!(longer.deadline.0, parent.deadline.0);
            parent.cancel().unwrap();
            assert_eq!(shorter.check(), Err(Error::Cancelled));
            assert_eq!(longer.check(), Err(Error::Cancelled));
        }

        #[test]
        fn fixed_parser_rejects_invalid_versions_framing_and_query_paths() {
            let telemetry = Telemetry::default();
            let (_, reactor, io) = setup();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            for request in [
                b"GET /metrics HTTP/1.0\r\n\r\n".as_slice(),
                b"GET /metrics HTTP/1.1\r\n\r\n",
                b"GET /metrics HTTP/1.1\r\nHost: l\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n",
            ] {
                let response = exchange_raw(address, request, &mut server, &reactor);
                assert_response(&response, "400 Bad Request", Some("bad request\n"));
            }
            let response = exchange_raw(
                address,
                b"GET /metrics?secret HTTP/1.1\r\nHost: l\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&response, "404 Not Found", Some("not found\n"));
            finish(server, &scope, &reactor);
        }

        /// Largest catalog the control plane can key: one retained page key
        /// per cache gives the most caches that fit a key bundle.
        fn max_metric_caches() -> usize {
            racer_cluster_control::rules::keyring::catalog_capacity(1)
        }

        #[test]
        fn metrics_http_response_exports_worker_quotas_with_bounded_output() {
            use crate::model::WorkerId;
            use crate::telemetry::Metrics;
            let workers = Metrics::for_workers(64).unwrap();
            let admissions: Vec<_> = workers
                .iter()
                .enumerate()
                .map(|(index, metrics)| {
                    let mut limits = crate::test_support::cluster::config(false).limits;
                    limits.relay_transfers = std::num::NonZeroUsize::new(usize::MAX).unwrap();
                    limits.ciphertext_bytes = std::num::NonZeroUsize::new(usize::MAX).unwrap();
                    let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits));
                    metrics
                        .observe_admission(WorkerId(u16::MAX - index as u16), admission.shared())
                        .unwrap();
                    metrics.observe_staging_pool(
                        WorkerId(u16::MAX - index as u16),
                        flow_control::PoolStats {
                            entries: usize::MAX,
                            bytes: usize::MAX,
                            target: usize::MAX,
                            max_depth: usize::MAX,
                            hits: u64::MAX,
                            misses: u64::MAX,
                            rejects: u64::MAX,
                            trimmed: u64::MAX,
                            constrained: true,
                        },
                    );
                    metrics.observe_retention(crate::retention::Snapshot {
                        observations: u64::MAX,
                        qualified: u64::MAX,
                        persistence_attempts: u64::MAX,
                        persistence_accepted: u64::MAX,
                        share_rejected: u64::MAX,
                        filter_set_bits: usize::MAX,
                        filter_bits: usize::MAX,
                        pending_payload_bytes: u64::MAX,
                        indexed_payload_bytes: u64::MAX,
                        disk: [crate::retention::DiskClassSnapshot {
                            published_pages: u64::MAX,
                            published_payload_bytes: u64::MAX,
                            segment_evicted_pages: u64::MAX,
                            segment_evicted_payload_bytes: u64::MAX,
                            read_payload_bytes: u64::MAX,
                        }; 2],
                    });
                    metrics.observe_held(crate::store::catalog::HeldSnapshot {
                        pages: u64::MAX,
                        payload_bytes: u64::MAX,
                        segments: u64::MAX,
                    });
                    admission
                })
                .collect();
            let charges: Vec<_> = admissions
                .iter()
                .map(|admission| {
                    (
                        admission
                            .reserve(None, ResourceClass::Relay, usize::MAX)
                            .unwrap(),
                        admission
                            .reserve(None, ResourceClass::Ciphertext, usize::MAX)
                            .unwrap(),
                    )
                })
                .collect();
            let telemetry = Telemetry {
                metrics: workers[0].clone(),
                ..Telemetry::default()
            };
            for event in crate::telemetry::EVENTS {
                telemetry.metrics.record(event, u64::MAX);
            }
            telemetry.metrics.observe_memory(ProcessMemory {
                resident: Some(u64::MAX),
                cgroup_current: Some(u64::MAX),
                cgroup_limit: Some(u64::MAX),
                pressure_some_avg10: Some(100.0),
            });
            // Every cache-labeled series at the maximum catalog, with
            // maximum-length names and values, spread across all workers.
            let max_caches = max_metric_caches();
            assert!(max_caches >= 1000, "{max_caches}");
            // GPU helper and prefetch families at their full fixed cardinality.
            telemetry
                .metrics
                .gpu()
                .attach_helper(crate::gpu::testing::helper(&[None, None]));
            for priority in crate::prefetch::Priority::ALL {
                telemetry
                    .metrics
                    .prefetch()
                    .demand_wait(priority, Duration::MAX);
            }
            let caches: Vec<_> = (0..max_caches)
                .map(|index| crate::telemetry::CacheDiskUsage {
                    cache: format!("{index:04}{}", "a".repeat(249)).into(),
                    resident: u64::MAX,
                    share: u64::MAX,
                })
                .collect();
            for metrics in &workers {
                metrics.observe_cache_disks(caches.clone());
            }
            let text = diagnostic_text(&telemetry, "/metrics");
            assert!(text.contains(&format!(
                "racer_cache_disk_share_bytes{{cache=\"{:04}{}\"}}",
                max_caches - 1,
                "a".repeat(249)
            )));
            assert!(text.contains("racer_gpu_cufile_enabled "));
            assert!(text.contains("racer_gpu_staging_slots_free{ordinal=\"1\"}"));
            assert!(
                text.contains("racer_prefetch_demand_wait_seconds_count{priority=\"high\"} 1\n")
            );
            assert!(text.ends_with("racer_live 1\n") || text.ends_with("racer_live 0\n"));
            for name in [
                "racer_admission_used{class=\"ordinary_outbound_connection\"}",
                "racer_staging_pool_trimmed_total ",
                "racer_worker_staging_pool_bytes{worker=\"65535\"}",
                "racer_cgroup_memory_pressure_some_avg10 100\n",
            ] {
                assert!(text.contains(name), "{name}");
            }
            for (name, kind) in crate::telemetry::RETENTION_METRICS
                .into_iter()
                .chain(crate::telemetry::HELD_METRICS)
            {
                assert!(text.contains(&format!("# TYPE {name} {kind}\n{name} {}\n", u64::MAX)));
                assert_eq!(
                    text.lines().filter(|line| line.starts_with(name)).count(),
                    1
                );
                assert!(!text.contains(&format!("{name}{{")));
            }
            for name in [
                "racer_opaque_relay_body_completed_total",
                "racer_opaque_relay_body_completed_bytes_total",
                "racer_opaque_relay_body_failed_total",
            ] {
                assert!(text.contains(&format!("# TYPE {name} counter\n{name} {}\n", u64::MAX)));
                assert_eq!(
                    text.lines().filter(|line| line.starts_with(name)).count(),
                    1
                );
                assert!(!text.contains(&format!("{name}{{")));
            }
            for name in [
                "relay_used",
                "relay_limit",
                "ciphertext_used_bytes",
                "ciphertext_limit_bytes",
            ] {
                assert!(text.contains(&format!("# TYPE racer_worker_{name} gauge\n")));
                assert!(text.contains(&format!(
                    "racer_worker_{name}{{worker=\"65535\"}} {}\n",
                    usize::MAX
                )));
            }
            assert_eq!(
                text.lines()
                    .filter(|line| line.contains("{worker="))
                    .count(),
                5 * 64
            );
            // Streaming has no fixed body cap: several times the largest
            // catalog still scrapes completely over HTTP.
            let caches: Vec<_> = (0..4 * max_caches)
                .map(|index| crate::telemetry::CacheDiskUsage {
                    cache: format!("{index:05}{}", "b".repeat(248)).into(),
                    resident: u64::MAX,
                    share: u64::MAX,
                })
                .collect();
            workers[0].observe_cache_disks(caches);
            let text = diagnostic_text(&telemetry, "/metrics");
            assert!(text.len() > MAX_RESPONSE_BYTES, "{} bytes", text.len());
            assert!(text.contains(&format!(
                "racer_cache_disk_resident_bytes{{cache=\"{:05}{}\"}}",
                4 * max_caches - 1,
                "b".repeat(248)
            )));
            assert!(text.ends_with("racer_live 1\n") || text.ends_with("racer_live 0\n"));
            struct Small(usize);
            impl std::fmt::Write for Small {
                fn write_str(&mut self, text: &str) -> std::fmt::Result {
                    self.0 = self.0.checked_sub(text.len()).ok_or(std::fmt::Error)?;
                    Ok(())
                }
            }
            assert!(get(&telemetry, "/metrics", true, &mut Small(256)).is_err());
            drop(charges);
        }

        #[test]
        fn unattached_serving_fails_closed_and_attachment_capacity_rolls_back() {
            let telemetry = Telemetry::default();
            let scope = scope();
            let mut serving = telemetry.serve("127.0.0.1:0".parse().unwrap(), &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(matches!(
                serving.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::InvalidConfiguration))
            ));
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.request_context_bytes = std::num::NonZeroUsize::new(1).unwrap();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone(), &crate::test_support::env()));
            assert!(matches!(
                telemetry.attach_io(reactor, admission.clone()),
                Err(Error::Overloaded)
            ));
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
        }

        #[test]
        fn raw_slow_clients_are_bounded_and_timeout_releases_capacity() {
            let (_, reactor, io) = setup();
            let telemetry = Telemetry::default();
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let scope = scope();
            let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
            let sockets: Vec<_> = (0..MAX_CONNECTIONS + 1)
                .map(|_| TcpStream::connect(address).unwrap())
                .collect();
            for _ in 0..40 {
                poll_server(&mut server, &reactor);
            }
            assert_eq!(
                telemetry.metrics.gauge(Gauge::DiagnosticConnections),
                MAX_CONNECTIONS as u64
            );
            assert_eq!(
                telemetry.metrics.count(Event::DiagnosticAccepted),
                MAX_CONNECTIONS as u64
            );
            let deadline = Instant::now() + CONNECTION_TIMEOUT + Duration::from_secs(2);
            while telemetry.metrics.count(Event::DiagnosticTimeout) < MAX_CONNECTIONS as u64 {
                assert!(Instant::now() < deadline);
                poll_server(&mut server, &reactor);
                assert!(
                    telemetry.metrics.gauge(Gauge::DiagnosticConnections) <= MAX_CONNECTIONS as u64
                );
            }
            drop(sockets);
            let response = exchange_raw(
                address,
                b"GET /healthz HTTP/1.1\r\nHost: local\r\n\r\n",
                &mut server,
                &reactor,
            );
            assert_response(&response, "200 OK", Some("ok\n"));
            finish(server, &scope, &reactor);
            assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 0);
        }

        #[test]
        fn attach_is_explicit_and_bind_failure_is_reported_on_first_poll() {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &crate::test_support::env()));
            let telemetry = Telemetry::default();
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
            telemetry
                .attach_io(reactor.clone(), admission.clone())
                .unwrap();
            assert_eq!(
                admission.used(ResourceClass::ControlProgress),
                CONTROL_SLOTS
            );
            assert_eq!(
                telemetry.attach_io(reactor, admission.clone()),
                Err(Error::InvalidConfiguration)
            );
            let occupied = TcpListener::bind("127.0.0.1:0").unwrap();
            let scope = scope();
            let mut serving = telemetry.serve(occupied.local_addr().unwrap(), &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(matches!(
                serving.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Io))
            ));
            drop(serving);
            drop(telemetry);
            assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
        }

        #[test]
        fn profile_http_disabled_query_method_and_reserved_attachment() {
            for enabled in [false, true] {
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let reactor = Rc::new(Reactor::new(admission.clone(), &crate::test_support::env()));
                let telemetry = Telemetry {
                    profiling: enabled.then(|| crate::profiling::ProfilingController::new(true)),
                    ..Telemetry::default()
                };
                set_hardware(&telemetry, crate::hardware::Report::default);
                reactor.init().unwrap();
                let baseline = admission.used(ResourceClass::RequestContext);
                telemetry
                    .attach_io(reactor.clone(), admission.clone())
                    .unwrap();
                assert_eq!(
                    admission.used(ResourceClass::RequestContext) - baseline,
                    RESERVED_BYTES
                        + if enabled {
                            server::BINARY_CONTROL_SLOTS * racer_io::scoped::SUBMISSION_BYTES
                        } else {
                            0
                        }
                );
                assert_eq!(
                    admission.used(ResourceClass::ControlProgress),
                    CONTROL_SLOTS
                        + if enabled {
                            server::BINARY_CONTROL_SLOTS
                        } else {
                            0
                        }
                );
                let io = telemetry.io.get().unwrap().clone();
                let listener = TcpListener::bind("127.0.0.1:0").unwrap();
                let address = listener.local_addr().unwrap();
                let scope = scope();
                let mut serving = telemetry.serve_listener_with_io(listener, io, &scope);
                for (method, query, status) in [
                    ("POST", "?seconds=1", "405 Method Not Allowed"),
                    (
                        "GET",
                        "?seconds=1&seconds=2",
                        if enabled {
                            "400 Bad Request"
                        } else {
                            "404 Not Found"
                        },
                    ),
                    (
                        "GET",
                        "?seconds=18446744073709551616",
                        if enabled {
                            "400 Bad Request"
                        } else {
                            "404 Not Found"
                        },
                    ),
                    (
                        "GET",
                        "?unsupported=1",
                        if enabled {
                            "400 Bad Request"
                        } else {
                            "404 Not Found"
                        },
                    ),
                ] {
                    let response = exchange_raw(
                        address,
                        format!(
                            "{method} /debug/pprof/profile{query} HTTP/1.1\r\nHost: local\r\n\r\n"
                        )
                        .as_bytes(),
                        &mut serving,
                        &reactor,
                    );
                    assert_response(&response, status, None);
                }
                // The hardware producer is attached whether or not pprof is.
                let response = exchange_raw(
                    address,
                    b"GET /debug/hardware HTTP/1.1\r\nHost: local\r\n\r\n",
                    &mut serving,
                    &reactor,
                );
                assert_response(&response, "200 OK", None);
                assert!(
                    std::str::from_utf8(&response)
                        .unwrap()
                        .contains("\r\nContent-Type: application/json\r\n")
                );
                admission.stop();
                let response = exchange_raw(
                    address,
                    b"GET /debug/pprof/profile?seconds=1 HTTP/1.1\r\nHost: local\r\n\r\n",
                    &mut serving,
                    &reactor,
                );
                assert_response(
                    &response,
                    if enabled {
                        "503 Service Unavailable"
                    } else {
                        "404 Not Found"
                    },
                    Some(if enabled { "stopping\n" } else { "not found\n" }),
                );
                finish(serving, &scope, &reactor);
                drop(telemetry);
                assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
            }
        }
    }
}
