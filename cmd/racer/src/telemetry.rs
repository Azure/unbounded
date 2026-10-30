use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::AttemptId;
use crate::model::RequestId;
use crate::model::WorkerId;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use ::telemetry::Ring;
use ::telemetry::SampleLimits;
use ::telemetry::SharedRing;
use ::telemetry::metrics;
use ::telemetry::server;
use ::telemetry::server::Handler;
use ::telemetry::server::Response;
use ::telemetry::server::Server;
use racer_control_wire::NodeId;
use std::cell::OnceCell;
use std::fmt::Write;
use std::net::SocketAddr;
#[cfg(test)]
use std::net::TcpListener;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
#[cfg(test)]
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::environment;
use uring_runtime::environment::Deadline;
use uring_runtime::reactor::descriptor::Descriptor;

pub use server::CONNECTION_TIMEOUT;
pub use server::MAX_CONNECTIONS;
pub use server::MAX_REQUEST_BYTES;
pub use server::MAX_RESPONSE_BYTES;

/// Prepaid diagnostic submissions: the transport's connection slots plus the
/// waits for one in-flight `/debug/hardware` producer, which is always attached.
pub const CONTROL_SLOTS: usize = server::CONTROL_SLOTS + server::JSON_CONTROL_SLOTS;

/// Prepaid diagnostic memory: transport buffers, the hardware producer's wait
/// bookkeeping, and the `/debug/hardware` helper's peak footprint.
///
/// At most one helper exists at a time. Its peak is during encoding, when the
/// discovered report (charged against `hardware::WORKING_BYTES` by
/// `hardware::Walk`, which aborts with `response_too_large` when exhausted) is
/// alive alongside the JSON buffer. The JSON buffer is allocated once with
/// capacity `MAX_BODY_BYTES` (see `Bounded`), never grows, and becomes the
/// result held for the response, so one `MAX_RESPONSE_BYTES` term covers both
/// the encoding buffer and the held result. The report is dropped before the
/// result is published. The detached discovery helper's thread stack
/// (`HARDWARE_STACK_BYTES`) is charged too, since a blocked sysfs read can keep
/// the helper alive after diagnostics teardown.
pub const RESERVED_BYTES: usize = server::RESERVED_BYTES
    + server::JSON_CONTROL_SLOTS * uring_runtime::reactor::SUBMISSION_BYTES
    + server::MAX_RESPONSE_BYTES
    + crate::hardware::WORKING_BYTES
    + HARDWARE_STACK_BYTES;

/// Stack size of the `/debug/hardware` discovery helper thread.
const HARDWARE_STACK_BYTES: usize = 2 * 1024 * 1024;

const _: () = assert!(server::MAX_BODY_BYTES <= server::MAX_RESPONSE_BYTES);
const _: () = assert!(RESERVED_BYTES >= server::RESERVED_BYTES + HARDWARE_BYTES);

/// Deadline for one `/debug/hardware` request, covering discovery and the response.
pub(crate) const HARDWARE_DEADLINE: Duration = Duration::from_secs(5);

/// Owned, `Send` hardware discovery prepared on the control worker and run on a
/// helper thread, so blocking sysfs reads never stall the worker. The job charges
/// the walk's working budget and stops early once the request is cancelled.
pub(crate) type HardwareJob = Box<
    dyn FnOnce(
            &crate::hardware::Walk,
        ) -> std::result::Result<crate::hardware::Report, crate::hardware::Abort>
        + Send,
>;

// Bounded diagnostics and HTTP endpoints, polled by an existing worker.

#[derive(Default)]
pub struct Telemetry {
    pub(crate) profiling: Option<Arc<crate::profiling::ProfilingController>>,
    pub(crate) membership: OnceCell<Rc<dyn Fn() -> Result<MembershipDiagnostic>>>,
    /// Prepares live hardware discovery for `/debug/hardware`; installed on the
    /// control worker only. The returned job runs on a helper thread.
    pub(crate) hardware: OnceCell<Rc<dyn Fn() -> HardwareJob>>,
    /// The current hardware helper, alive until both its thread and response finish.
    hardware_active: std::cell::RefCell<std::sync::Weak<HardwareState>>,
    pub send_crc: crate::telemetry::Samples,
    pub failures: crate::telemetry::Failures,
    pub metrics: crate::telemetry::Metrics,
    pub health: crate::telemetry::Health,
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
    pub fn attach_io(
        &self,
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Result<()> {
        if self.io.get().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        let io = Rc::new(DiagnosticIo::attach_configured(
            reactor,
            admission,
            self.profiling.is_some(),
        )?);
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
    reactor: Rc<Reactor>,
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
const HARDWARE_BYTES: usize =
    server::MAX_RESPONSE_BYTES + crate::hardware::WORKING_BYTES + HARDWARE_STACK_BYTES;
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Result<Self> {
        Self::attach_configured(reactor, admission, false)
    }

    fn attach_configured(
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
            RESERVED_BYTES + (slots - CONTROL_SLOTS) * uring_runtime::reactor::SUBMISSION_BYTES,
        )?;
        let bookkeeping = memory.split(slots * uring_runtime::reactor::SUBMISSION_BYTES)?;
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

fn serve<'a>(
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
    let session = HardwareSession::start(prepare(), server::MAX_BODY_BYTES, charge.clone())?;
    *active = Arc::downgrade(&session.state);
    Ok(server::BinaryResponse {
        kind: server::BinaryKind::Json,
        duration: HARDWARE_DEADLINE,
        session: Box::new(session),
    })
}

/// State shared by one hardware helper thread and its response session.
struct HardwareState {
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
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
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
        }))
        .unwrap_or(Err(server::BinaryError::Unavailable("unavailable")));
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
fn get(
    telemetry: &Telemetry,
    path: &str,
    admission_usable: bool,
    mut output: &mut dyn Write,
) -> std::result::Result<Response, std::fmt::Error> {
    let ready = admission_usable && telemetry.health.ready();
    const LIVE_PROBE: ::telemetry::health::Probe =
        ::telemetry::health::Probe::new("ok\n", "not live\n");
    const READY_PROBE: ::telemetry::health::Probe =
        ::telemetry::health::Probe::new("ready\n", "not ready\n");
    let (response, body, event) = match path {
        "/healthz" => {
            let (response, body) = LIVE_PROBE.response(telemetry.health.live());
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
        | "/debug/send-crc"
        | "/debug/terminal"
        | "/debug/gate-events"
        | "/debug/flight-expiry"
        | "/debug/admission-final"
        | "/debug/candidate-final" => (Response::Text, "", Event::DiagnosticFailures),
        _ => return Ok(Response::NotFound),
    };
    telemetry.metrics.record(event, 1);
    if path == "/metrics" {
        telemetry.metrics.write_prometheus(&mut output)?;
        writeln!(
            output,
            "# TYPE racer_ready gauge\nracer_ready {}\n# TYPE racer_live gauge\nracer_live {}",
            u8::from(ready),
            u8::from(telemetry.health.live())
        )?;
    } else if path == "/debug/membership" {
        match telemetry.membership.get().and_then(|read| read().ok()) {
            Some(state) => state.write(&mut output)?,
            None => output.write_str("unavailable fully_applied=0\n")?,
        }
    } else if path == "/debug/send-crc" {
        telemetry.send_crc.write(&mut output)?;
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
                + server::JSON_CONTROL_SLOTS * uring_runtime::reactor::SUBMISSION_BYTES
                + server::MAX_RESPONSE_BYTES
                + crate::hardware::WORKING_BYTES
                + HARDWARE_STACK_BYTES
        );
        assert_eq!(HARDWARE_STACK_BYTES, 2 * 1024 * 1024);
        assert_eq!(
            HARDWARE_BYTES,
            server::MAX_RESPONSE_BYTES + crate::hardware::WORKING_BYTES + HARDWARE_STACK_BYTES
        );
        let report = crate::hardware::Report::default();
        let body = encode_report(&report, server::MAX_BODY_BYTES).unwrap();
        assert!(body.capacity() <= server::MAX_RESPONSE_BYTES);
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
mod warm_metric_tests {
    use super::*;

    #[test]
    fn held_worker_snapshots_replace_aggregate_saturate_and_survive_failed_output() {
        use crate::store::catalog::HeldSnapshot;
        let workers = Metrics::for_workers(2).unwrap();
        let snapshot = HeldSnapshot {
            pages: 2,
            payload_bytes: 17,
            segments: 1,
        };
        workers[0].observe_held(snapshot);
        workers[0].observe_held(snapshot);
        workers[1].observe_held(snapshot);
        let mut output = String::new();
        workers[1].write_prometheus(&mut output).unwrap();
        for (name, value) in HELD_METRICS.into_iter().zip([4, 34, 2]) {
            assert!(output.contains(&format!("# TYPE {} gauge\n{} {value}\n", name.0, name.0)));
        }
        workers[0].observe_held(HeldSnapshot::default());
        output.clear();
        workers[0].write_held(&mut output).unwrap();
        assert!(output.contains("racer_warm_held_payload_bytes 17\n"));
        for worker in &workers {
            worker.observe_held(HeldSnapshot {
                pages: u64::MAX,
                payload_bytes: u64::MAX,
                segments: u64::MAX,
            });
        }
        struct Full;
        impl Write for Full {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        assert!(workers[1].write_held(&mut Full).is_err());
        output.clear();
        workers[0].write_held(&mut output).unwrap();
        for (name, _) in HELD_METRICS {
            assert!(output.contains(&format!("{name} {}\n", u64::MAX)));
        }
    }

    #[test]
    fn node_registry_gauges_replace_without_multiplying_by_workers() {
        let workers = Metrics::for_workers(3).unwrap();
        let mut output = String::new();
        workers[0].write_prometheus(&mut output).unwrap();
        assert!(output.contains("racer_warm_tracked_jobs 0\n"));
        assert!(output.contains("racer_warm_waiters 0\n"));
        for worker in &workers {
            worker.observe_warm((7, 11));
        }
        output.clear();
        workers[2].write_prometheus(&mut output).unwrap();
        assert!(
            output.contains("# TYPE racer_warm_tracked_jobs gauge\nracer_warm_tracked_jobs 7\n")
        );
        assert!(output.contains("# TYPE racer_warm_waiters gauge\nracer_warm_waiters 11\n"));
        workers[1].clone().observe_warm((2, 0));
        assert_eq!(workers[0].gauge(Gauge::WarmTrackedJobs), 2);
        assert_eq!(workers[0].gauge(Gauge::WarmWaiters), 0);
        workers[0].observe_warm((0, 0));
        assert_eq!(workers[2].gauge(Gauge::WarmTrackedJobs), 0);
    }

    #[test]
    fn warm_event_hooks_export_fixed_saturating_node_counters() {
        let workers = Metrics::for_workers(2).unwrap();
        for (event, name) in [
            (WarmEvent::Accepted, "racer_warm_accepted_total"),
            (WarmEvent::Refused, "racer_warm_refused_total"),
            (WarmEvent::Completed, "racer_warm_completed_total"),
            (WarmEvent::Failed, "racer_warm_failed_total"),
            (WarmEvent::WaitStarted, "racer_warm_wait_started_total"),
            (WarmEvent::WaitCompleted, "racer_warm_wait_completed_total"),
            (WarmEvent::WaitFallback, "racer_warm_wait_fallback_total"),
            (WarmEvent::WaitFailed, "racer_warm_wait_failed_total"),
            (
                WarmEvent::RemoteHoldAccepted,
                "racer_warm_remote_hold_accepted_total",
            ),
            (
                WarmEvent::RemoteHoldRefused,
                "racer_warm_remote_hold_refused_total",
            ),
            (
                WarmEvent::RemoteFillCompleted,
                "racer_warm_remote_fill_completed_total",
            ),
            (
                WarmEvent::RemoteFillRetried,
                "racer_warm_remote_fill_retried_total",
            ),
            (
                WarmEvent::RemoteFillFailed,
                "racer_warm_remote_fill_failed_total",
            ),
            (
                WarmEvent::RemoteReleaseSent,
                "racer_warm_remote_release_sent_total",
            ),
            (
                WarmEvent::RemoteReleaseFailed,
                "racer_warm_remote_release_failed_total",
            ),
            (
                WarmEvent::OwnerHoldAccepted,
                "racer_warm_owner_hold_accepted_total",
            ),
            (
                WarmEvent::OwnerHoldRefused,
                "racer_warm_owner_hold_refused_total",
            ),
            (
                WarmEvent::OwnerFillCompleted,
                "racer_warm_owner_fill_completed_total",
            ),
            (
                WarmEvent::OwnerFillFailed,
                "racer_warm_owner_fill_failed_total",
            ),
            (WarmEvent::OwnerReleased, "racer_warm_owner_released_total"),
        ] {
            workers[0].warm_event(event);
            workers[1].warm_event(event);
            workers[1].clone().warm_event(event);
            let mut output = String::new();
            workers[0].write_prometheus(&mut output).unwrap();
            assert!(output.contains(&format!("# TYPE {name} counter\n{name} 3\n")));
            workers[0].record(event.event(), u64::MAX);
            workers[0].warm_event(event);
            output.clear();
            workers[1].write_prometheus(&mut output).unwrap();
            assert!(output.contains(&format!("{name} {}\n", 3)));
            assert!(
                !output
                    .lines()
                    .any(|line| line.starts_with("racer_warm_") && line.contains('{'))
            );
        }
    }

    #[test]
    fn warm_endpoint_preserves_samples_across_scrapes_and_output_failure() {
        struct Full;
        impl Write for Full {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        let telemetry = Telemetry::default();
        telemetry.metrics.observe_warm((3, 5));
        telemetry.metrics.warm_event(WarmEvent::Refused);
        assert!(get(&telemetry, "/metrics", true, &mut Full).is_err());
        for usable in [true, false, true] {
            let mut output = String::new();
            assert!(matches!(
                get(&telemetry, "/metrics", usable, &mut output),
                Ok(Response::Metrics)
            ));
            assert!(output.contains("racer_warm_tracked_jobs 3\n"));
            assert!(output.contains("racer_warm_waiters 5\n"));
            assert!(output.contains("racer_warm_refused_total 1\n"));
            assert!(output.len() < MAX_RESPONSE_BYTES);
        }
    }
}

#[cfg(test)]
mod retention_metric_tests {
    use super::*;

    #[test]
    fn disk_usage_aggregates_shared_disks_replaces_and_escapes_labels() {
        let workers = Metrics::for_workers(2).unwrap();
        let mut output = String::new();
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.is_empty());
        let sample = |disk: &str, size, used| DiskUsage {
            disk: disk.into(),
            size,
            used,
        };
        workers[0].observe_disks(vec![
            sample("device:a", 1024, 512),
            sample("file:worker-0-slab-0.dat", 4096, 0),
        ]);
        workers[1].observe_disks(vec![
            sample("device:a", 2048, 1024),
            sample("device:b\\\"\n", 512, 512),
        ]);
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.contains("# TYPE racer_disk_size_bytes gauge\n"));
        assert!(output.contains("# TYPE racer_disk_used_bytes gauge\n"));
        assert!(output.contains("racer_disk_size_bytes{disk=\"device:a\"} 3072\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:a\"} 1536\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:b\\\\\\\"\\n\"} 512\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"file:worker-0-slab-0.dat\"} 0\n"));
        workers[1].observe_disks(vec![sample("device:a", 2048, 0)]);
        workers[1].observe_disks(vec![sample("device:a", 2048, 0)]);
        output.clear();
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.contains("racer_disk_size_bytes{disk=\"device:a\"} 3072\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:a\"} 512\n"));
        assert!(!output.contains("device:b"));
        workers[0].observe_disks(vec![]);
        workers[1].observe_disks(vec![]);
        output.clear();
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.is_empty());
    }

    #[test]
    fn disk_usage_export_saturates_and_keeps_samples_on_output_failure() {
        struct Full;
        impl Write for Full {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        let workers = Metrics::for_workers(2).unwrap();
        for worker in &workers {
            worker.observe_disks(vec![DiskUsage {
                disk: "device:a".into(),
                size: u64::MAX,
                used: u64::MAX,
            }]);
        }
        assert!(workers[0].write_disks(&mut Full).is_err());
        let mut output = String::new();
        workers[1].write_disks(&mut output).unwrap();
        for name in ["size", "used"] {
            assert!(output.contains(&format!(
                "racer_disk_{name}_bytes{{disk=\"device:a\"}} {}\n",
                u64::MAX
            )));
        }
    }

    #[test]
    fn cache_disk_usage_sums_workers_by_name_and_drops_removed_caches() {
        let workers = Metrics::for_workers(2).unwrap();
        let mut output = String::new();
        workers[0].write_cache_disks(&mut output).unwrap();
        assert!(output.is_empty());
        let sample = |cache: &str, resident, share| CacheDiskUsage {
            cache: cache.into(),
            resident,
            share,
        };
        workers[0].observe_cache_disks(vec![sample("models", 900, 800), sample("logs", 50, 200)]);
        workers[1].observe_cache_disks(vec![sample("models", 100, 800), sample("x\"\n", 1, 2)]);
        workers[1].write_prometheus(&mut output).unwrap();
        assert!(output.contains("# TYPE racer_cache_disk_resident_bytes gauge\n"));
        assert!(output.contains("# TYPE racer_cache_disk_share_bytes gauge\n"));
        assert!(output.contains("racer_cache_disk_resident_bytes{cache=\"models\"} 1000\n"));
        assert!(output.contains("racer_cache_disk_share_bytes{cache=\"models\"} 1600\n"));
        assert!(output.contains("racer_cache_disk_resident_bytes{cache=\"logs\"} 50\n"));
        assert!(output.contains("racer_cache_disk_share_bytes{cache=\"x\\\"\\n\"} 2\n"));
        workers[0].observe_cache_disks(vec![sample("models", 0, 1000)]);
        workers[1].observe_cache_disks(vec![]);
        output.clear();
        workers[0].write_cache_disks(&mut output).unwrap();
        assert!(!output.contains("logs"));
        assert!(output.contains("racer_cache_disk_share_bytes{cache=\"models\"} 1000\n"));
    }

    #[test]
    fn disk_observability_aggregates_fixed_classes_and_replaces_gauges() {
        let workers = Metrics::for_workers(2).unwrap();
        for (i, worker) in workers.iter().enumerate() {
            let mut snapshot = crate::retention::Snapshot {
                pending_payload_bytes: 3,
                indexed_payload_bytes: 17,
                ..Default::default()
            };
            snapshot.disk[i] = crate::retention::DiskClassSnapshot {
                published_pages: 1,
                published_payload_bytes: 17,
                index_evicted_pages: 2,
                index_evicted_payload_bytes: 34,
                segment_evicted_pages: 3,
                segment_evicted_payload_bytes: 51,
                read_payload_bytes: u64::MAX,
            };
            worker.observe_retention(snapshot);
            worker.observe_retention(snapshot);
        }
        let mut output = String::new();
        workers[0].write_retention(&mut output).unwrap();
        for class in ["nonowned", "owned"] {
            for (name, value) in DISK_CLASS_METRICS
                .into_iter()
                .zip([1, 17, 2, 34, 3, 51, u64::MAX])
            {
                assert!(output.contains(&format!("# TYPE {name} counter\n")));
                assert!(
                    output.contains(&format!("{name}{{classification=\"{class}\"}} {value}\n"))
                );
            }
        }
        assert_eq!(
            output
                .lines()
                .filter(|line| line.contains("{classification="))
                .count(),
            14
        );
        assert!(output.contains("racer_disk_pending_payload_bytes 6\n"));
        assert!(output.contains("racer_disk_indexed_payload_bytes 34\n"));
        workers[0].observe_retention(crate::retention::Snapshot::default());
        output.clear();
        workers[1].write_retention(&mut output).unwrap();
        assert!(output.contains("racer_disk_pending_payload_bytes 3\n"));
        assert!(output.contains("racer_disk_indexed_payload_bytes 17\n"));
    }
    #[test]
    fn retention_snapshots_replace_shards_and_aggregate_without_labels() {
        let workers = Metrics::for_workers(2).unwrap();
        workers[0].observe_retention(crate::retention::Snapshot {
            observations: 10,
            qualified: 4,
            persistence_attempts: 3,
            persistence_accepted: 2,
            filter_set_bits: 7,
            filter_bits: 256,
            heat_entries: 1,
            ..Default::default()
        });
        workers[1].observe_retention(crate::retention::Snapshot {
            observations: 5,
            filter_set_bits: 2,
            filter_bits: 256,
            ..Default::default()
        });
        // Repeated health observations must not add cumulative counters again.
        let snapshot = workers[0].retention.snapshots().next().unwrap();
        workers[0].observe_retention(snapshot);
        let mut output = String::new();
        workers[1].write_prometheus(&mut output).unwrap();
        for sample in [
            "racer_retention_observations_total 15\n",
            "racer_retention_qualified_total 4\n",
            "racer_retention_persistence_attempts_total 3\n",
            "racer_retention_persistence_accepted_total 2\n",
            "racer_retention_filter_set_bits 9\n",
            "racer_retention_filter_bits 512\n",
            "racer_retention_heat_entries 1\n",
        ] {
            assert!(output.contains(sample), "{sample}");
        }
        assert!(
            !output
                .lines()
                .any(|line| line.starts_with("racer_retention_") && line.contains('{'))
        );
        assert!(output.contains("# TYPE racer_retention_observations_total counter\n"));
        assert!(output.contains("# TYPE racer_retention_heat_entries gauge\n"));
    }

    #[test]
    fn retention_export_saturates_and_propagates_output_failure() {
        let workers = Metrics::for_workers(2).unwrap();
        for worker in &workers {
            worker.observe_retention(crate::retention::Snapshot {
                observations: u64::MAX,
                disk: [crate::retention::DiskClassSnapshot {
                    read_payload_bytes: u64::MAX,
                    ..Default::default()
                }; 2],
                ..Default::default()
            });
        }
        let mut output = String::new();
        workers[0].write_retention(&mut output).unwrap();
        assert!(output.contains(&format!(
            "racer_retention_observations_total {}\n",
            u64::MAX
        )));
        for class in ["owned", "nonowned"] {
            assert!(output.contains(&format!(
                "racer_disk_class_read_payload_bytes_total{{classification=\"{class}\"}} {}\n",
                u64::MAX
            )));
        }
        struct Failed;
        impl Write for Failed {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        assert!(workers[0].write_retention(&mut Failed).is_err());
    }

    #[test]
    fn retention_metrics_endpoint_is_nonconsuming_when_admission_stops() {
        let telemetry = Telemetry::default();
        telemetry
            .metrics
            .observe_retention(crate::retention::Snapshot {
                observations: 17,
                qualified: 5,
                ..Default::default()
            });
        for usable in [true, false, true] {
            let mut output = String::new();
            assert!(matches!(
                get(&telemetry, "/metrics", usable, &mut output),
                Ok(Response::Metrics)
            ));
            assert!(output.contains("racer_retention_observations_total 17\n"));
            assert!(output.contains("racer_retention_qualified_total 5\n"));
            assert!(output.len() < MAX_RESPONSE_BYTES);
        }
    }
}

#[derive(Clone, Default)]
pub struct Health(::telemetry::health::Health<Resources>);
pub use ::telemetry::health::State;
/// Complete worker observation; no identities or credentials enter health state.
#[derive(Clone, Copy, Debug, Default)]
pub struct Resources {
    pub workers_usable: bool,
    pub storage_usable: bool,
    pub listeners_usable: bool,
    pub membership_usable: bool,
    pub admission_usable: bool,
    pub credentials_valid_until: Option<Instant>,
    pub observed_until: Option<Instant>,
}
impl Resources {
    pub fn usable_at(&self, now: Instant) -> bool {
        self.workers_usable
            && self.storage_usable
            && self.listeners_usable
            && self.membership_usable
            && self.admission_usable
            && self
                .credentials_valid_until
                .is_some_and(|expiry| now < expiry)
            && self.observed_until.is_some_and(|expiry| now < expiry)
    }
}
impl Health {
    pub fn state(&self) -> Result<State> {
        self.state_at(uring_runtime::environment::now())
    }
    pub fn state_at(&self, now: Instant) -> Result<State> {
        self.0
            .state_at(now, Resources::usable_at)
            .map_err(|_| Error::Unavailable)
    }
    pub fn observe(&self, resources: Resources) -> Result<()> {
        self.0.observe(resources).map_err(|_| Error::Unavailable)
    }
    pub fn ready(&self) -> bool {
        self.state().is_ok_and(|state| state == State::Ready)
    }
    /// Starting, degraded, and draining remain live; a response establishes progress.
    pub fn live(&self) -> bool {
        self.state().is_ok_and(|state| state != State::Stopped)
    }
    pub fn transition(&self, state: State) -> Result<()> {
        self.0
            .transition(state, |resources| {
                resources.usable_at(uring_runtime::environment::now())
            })
            .map_err(|_| Error::Unavailable)
    }
    /// Store an observation and settle Ready or Degraded atomically. Usability
    /// is judged at `now`, the instant the observation was aggregated.
    pub fn settle(&self, resources: Resources, now: Instant) -> Result<State> {
        self.0
            .settle(resources, |resources| resources.usable_at(now))
            .map_err(|_| Error::Unavailable)
    }
}

// Opt-in HTTP send fingerprints, never authentication or payload scans on I/O.
// Shared limits: one active owner, two samples/second, 240 total, 120 seconds.
// Cached CRCs are reused. Pending/unavailable samples do not prove agreement.
pub const SEND_CRC_CAPACITY: usize = 64;
const SEND_CRC_LIMITS: SampleLimits = SampleLimits {
    total: 240,
    duration: Duration::from_secs(120),
    interval: Duration::from_millis(500),
};
#[derive(Clone, Debug)]
pub struct Pair {
    pub sender: NodeId,
    pub receiver: NodeId,
}
impl Pair {
    pub fn parse(value: &str) -> Result<Self> {
        let (sender, receiver) = value.split_once(',').ok_or(Error::InvalidConfiguration)?;
        let pair = Self {
            sender: NodeId(sender.into()),
            receiver: NodeId(receiver.into()),
        };
        pair.validate()?;
        Ok(pair)
    }
    pub fn validate(&self) -> Result<()> {
        if self.sender == self.receiver
            || !racer_crypto::identity::canonical_uuid(&self.sender.0)
            || !racer_crypto::identity::canonical_uuid(&self.receiver.0)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
/// Racer's selected sender/receiver sampling policy and diagnostic records.
#[derive(Clone)]
pub struct Samples(::telemetry::Sampler<Sample, SEND_CRC_CAPACITY>);

impl Default for Samples {
    /// Apply Racer's fixed send-checksum sample limits.
    fn default() -> Self {
        Self(::telemetry::Sampler::new(SEND_CRC_LIMITS))
    }
}
struct Sample {
    sequence: u64,
    sender: NodeId,
    receiver: NodeId,
    data: Mutex<Data>,
}
struct Data {
    crypto: Option<crate::security::CryptoId>,
    send: &'static str,
    status: &'static str,
    error: Option<Error>,
    facts: Option<AeadFailure>,
}
pub(crate) struct Ticket(Arc<Sample>);
pub(crate) struct Work {
    sample: Arc<Sample>,

    _lease: ::telemetry::SampleLease<Sample, SEND_CRC_CAPACITY>,

    pub facts: Option<AeadFailure>,

    pub cached: bool,
}
impl Samples {
    #[cfg(test)]
    pub(crate) fn fill_test_ring(&self) {
        let pair = Pair::parse(
            "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
        )
        .unwrap();
        let now = environment::now();
        for i in 0..SEND_CRC_CAPACITY {
            let (ticket, mut work) = self
                .begin_at(&pair, &pair.sender, &pair.receiver, || {
                    now + SEND_CRC_LIMITS.interval * i as u32
                })
                .unwrap();
            work.facts = Some(crate::telemetry::test_aead_failure());
            work.identify(crate::security::CryptoId {
                worker: crate::model::WorkerId(u16::MAX),
                generation: u64::MAX,
                sequence: u64::MAX,
            });
            work.finish(None);
            ticket.finish(true);
        }
    }
    pub(crate) fn begin(
        &self,
        pair: &Pair,
        sender: &NodeId,
        receiver: &NodeId,
    ) -> Option<(Ticket, Work)> {
        self.begin_at(pair, sender, receiver, environment::now)
    }
    fn begin_at(
        &self,
        pair: &Pair,
        sender: &NodeId,
        receiver: &NodeId,
        now: impl FnOnce() -> Instant,
    ) -> Option<(Ticket, Work)> {
        if sender != &pair.sender || receiver != &pair.receiver {
            return None;
        }
        let now = now();
        let (sample, lease) = self.0.acquire(now, |sequence| Sample {
            sequence,
            sender: sender.clone(),
            receiver: receiver.clone(),
            data: Mutex::new(Data {
                crypto: None,
                send: "pending",
                status: "pending",
                error: None,
                facts: None,
            }),
        })?;
        Some((
            Ticket(sample.clone()),
            Work {
                sample,
                _lease: lease,
                facts: None,
                cached: false,
            },
        ))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let (entries, counts) = self.0.snapshot();
        let ::telemetry::SampleCounts {
            eligible,
            sampled,
            skipped,
            busy,
        } = counts;
        writeln!(
            out,
            "eligible={eligible} sampled={sampled} skipped={skipped} busy={} retained={} overwritten={} capacity={SEND_CRC_CAPACITY}",
            u8::from(busy),
            sampled.min(SEND_CRC_CAPACITY as u64),
            sampled.saturating_sub(SEND_CRC_CAPACITY as u64)
        )?;
        for (_, sample) in entries.iter_refs() {
            let data = sample.data.lock().unwrap_or_else(|e| e.into_inner());
            write!(
                out,
                "seq={} sender={} receiver={} send={} status={} error={:?}",
                sample.sequence,
                sample.sender.0,
                sample.receiver.0,
                data.send,
                data.status,
                data.error
            )?;
            if let Some(id) = data.crypto {
                write!(
                    out,
                    " w={} crypto={}:{}",
                    id.worker.0, id.generation, id.sequence
                )?;
            }
            if let Some(f) = data.facts {
                f.write_fields(out)?;
            }
            writeln!(out)?;
        }
        Ok(())
    }
}
impl Ticket {
    pub(crate) fn finish(&self, success: bool) {
        self.0.data.lock().unwrap_or_else(|e| e.into_inner()).send =
            if success { "completed" } else { "failed" };
    }
}
impl Drop for Ticket {
    fn drop(&mut self) {
        let mut data = self.0.data.lock().unwrap_or_else(|e| e.into_inner());
        if data.send == "pending" {
            data.send = "abandoned";
        }
    }
}
impl Work {
    pub(crate) fn identify(&self, id: crate::security::CryptoId) {
        self.sample
            .data
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .crypto = Some(id);
    }
    pub(crate) fn finish(&self, error: Option<Error>) {
        let mut data = self.sample.data.lock().unwrap_or_else(|e| e.into_inner());
        data.facts = self.facts;
        data.error = error;
        data.status = if error.is_some() {
            "unavailable"
        } else if self.cached {
            "cached"
        } else {
            "computed"
        };
    }
}
impl Drop for Work {
    fn drop(&mut self) {
        {
            let mut data = self.sample.data.lock().unwrap_or_else(|e| e.into_inner());
            if data.status == "pending" {
                data.status = "unavailable";
            }
        }
    }
}

// Bounded internal failures, exported separately from low-cardinality metrics.
// Only typed errors, correlation IDs, and numeric progress/resource facts enter
// this ring. Object keys, ETags, headers, credentials, and payloads never enter it.
// A separate 64-entry AEAD ring survives transient admission floods. Its CRC is
// a receiver-side fingerprint, not proof of equality with sender bytes. Page/AAD
// hashes are pseudonyms (known inputs can be guessed), never authentication.
// Supplier is the original response signer; remote is the last reverse signer,
// not a TCP address. Acquisition IDs can differ from the decrypt request after
// retention or cross-worker handoff. Disk reconstruction has no peer provenance.

pub const FAILURE_CAPACITY: usize = 128;
pub const AEAD_CAPACITY: usize = 64;

fn hex(out: &mut impl std::fmt::Write, bytes: &[u8]) -> std::fmt::Result {
    for byte in bytes {
        write!(out, "{byte:02x}")?;
    }
    Ok(())
}

/// Authenticated acquisition identity, not a TCP tuple or proof of body integrity.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct PeerProvenance {
    pub request: RequestId,
    pub attempt: AttemptId,
    pub supplier: [u8; 36],
    pub remote: [u8; 36],
}

/// Fixed-size rejection facts. Page hashes are pseudonyms, not secret-key hashes.
#[derive(Clone, Copy)]
pub(crate) struct AeadFailure {
    pub unix_millis: u64,
    pub request: RequestId,
    pub peer: Option<PeerProvenance>,
    pub page: [u8; 32],
    pub number: u64,
    pub key: [u8; 16],
    pub nonce: [u8; 24],
    pub plaintext: u32,
    pub ciphertext: u32,
    pub aad: [u8; 32],
    pub crc: Option<u64>,
}
impl AeadFailure {
    pub(crate) fn write_fields(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        write!(out, " ms={} request=", self.unix_millis)?;
        hex(out, &self.request.0)?;
        if let Some(peer) = self.peer {
            write!(out, " acquisition=")?;
            hex(out, &peer.request.0)?;
            write!(out, " attempt=")?;
            hex(out, &peer.attempt.0)?;
            write!(
                out,
                " supplier={}",
                std::str::from_utf8(&peer.supplier).unwrap_or("unknown")
            )?;
        }
        self.write_fingerprint(out)
    }
    fn write_fingerprint(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        write!(out, " page=")?;
        hex(out, &self.page)?;
        write!(out, " number={} key=", self.number)?;
        hex(out, &self.key)?;
        write!(out, " nonce=")?;
        hex(out, &self.nonce)?;
        write!(out, " lengths={}/{} aad=", self.plaintext, self.ciphertext)?;
        hex(out, &self.aad)?;
        match self.crc {
            Some(crc) => write!(out, " crc={crc:016x}"),
            None => write!(out, " crc=none"),
        }
    }
}

type AeadRing = SharedRing<(crate::security::CryptoId, AeadFailure), AEAD_CAPACITY>;

const DECRYPT_FAILURE_CAPACITY: usize = 64;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(crate) enum DecryptScopeFailure {
    #[default]
    Unknown,
    Entry(Error),
    Final(Error),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DecryptReason {
    Cancelled,
    DeadlineExceeded,
    MissingKey,
    InvalidConfiguration,
    InvalidRequest,
    Overloaded,
    CorruptCrc,
    CorruptAead,
    CorruptUnclassified,
    Other,
}
impl DecryptReason {
    #[cfg(test)]
    pub(crate) const ALL: [Self; 10] = [
        Self::Cancelled,
        Self::DeadlineExceeded,
        Self::MissingKey,
        Self::InvalidConfiguration,
        Self::InvalidRequest,
        Self::Overloaded,
        Self::CorruptCrc,
        Self::CorruptAead,
        Self::CorruptUnclassified,
        Self::Other,
    ];
    pub(crate) fn new(
        error: Error,
        rejection: Option<crate::security::IntegrityRejection>,
    ) -> Self {
        use crate::security::IntegrityRejection;
        match error {
            Error::Cancelled => Self::Cancelled,
            Error::DeadlineExceeded => Self::DeadlineExceeded,
            Error::MissingKey => Self::MissingKey,
            Error::InvalidConfiguration => Self::InvalidConfiguration,
            Error::InvalidRequest => Self::InvalidRequest,
            Error::Overloaded => Self::Overloaded,
            Error::CorruptRecord => match rejection {
                Some(IntegrityRejection::Crc) => Self::CorruptCrc,
                Some(IntegrityRejection::Aead) => Self::CorruptAead,
                None => Self::CorruptUnclassified,
            },
            _ => Self::Other,
        }
    }
    pub(crate) fn event(self) -> Event {
        match self {
            Self::Cancelled => Event::CryptoDecryptFailureCancelled,
            Self::DeadlineExceeded => Event::CryptoDecryptFailureDeadlineExceeded,
            Self::MissingKey => Event::CryptoDecryptFailureMissingKey,
            Self::InvalidConfiguration => Event::CryptoDecryptFailureInvalidConfiguration,
            Self::InvalidRequest => Event::CryptoDecryptFailureInvalidRequest,
            Self::Overloaded => Event::CryptoDecryptFailureOverloaded,
            Self::CorruptCrc => Event::CryptoDecryptFailureCorruptCrc,
            Self::CorruptAead => Event::CryptoDecryptFailureCorruptAead,
            Self::CorruptUnclassified => Event::CryptoDecryptFailureCorruptUnclassified,
            Self::Other => Event::CryptoDecryptFailureOther,
        }
    }
    fn name(self) -> &'static str {
        match self {
            Self::Cancelled => "cancelled",
            Self::DeadlineExceeded => "deadline_exceeded",
            Self::MissingKey => "missing_key",
            Self::InvalidConfiguration => "invalid_configuration",
            Self::InvalidRequest => "invalid_request",
            Self::Overloaded => "overloaded",
            Self::CorruptCrc => "corrupt_crc",
            Self::CorruptAead => "corrupt_aead",
            Self::CorruptUnclassified => "corrupt_unclassified",
            Self::Other => "other",
        }
    }
}

/// Failure-only facts. Page is a numeric envelope page, not an object identity.
#[derive(Clone, Copy)]
pub(crate) struct DecryptFailure {
    pub id: crate::security::CryptoId,
    pub error: Error,
    pub reason: DecryptReason,
    pub request: Option<RequestId>,
    pub page: Option<u64>,
    pub engine_ms: Option<u64>,
    pub reap_ms: u64,
    pub scope_failure: DecryptScopeFailure,
    pub queue_ns: Option<u64>,
    pub execution_ns: Option<u64>,
}

#[derive(Clone, Copy, Debug)]
pub enum Stage {
    Admission,
    ClientRead,
    FirstSlice,
    NextSlice,
    ClientWrite,
    RangeScope,
    RangePipe,
    RangeBudget,
    PageDispatch,
    PageAcquire,
    PageAttach,
    CandidateExchange,
    CandidateResponse,
    CandidateExhausted,
    PeerRoute,
    PeerVerify,
    PeerCheckout,
    PeerHandshake,
    PeerHead,
    PeerReceiveAdmission,
    PeerReceiveBody,
    PeerDecode,
    PeerLocal,
    PeerRelay,
}

#[derive(Clone, Copy, Debug, Default)]
pub enum Detail {
    #[default]
    None,
    Body(BodyProgress),
    Page(u64),
    Delivery {
        sent: u64,
        expected: u64,
    },
    ClientDelivery {
        boundary: ClientBoundary,
        sent: u64,
        expected: u64,
    },
    Budget {
        attempts: u32,
        links: u8,
    },
    Resource {
        class: ResourceClass,
        used: usize,
        limit: usize,
        requested: usize,
        cache_used: Option<usize>,
        cache_limit: Option<usize>,
    },
    CacheEntries {
        used: usize,
        limit: usize,
    },
}

#[derive(Clone, Copy, Debug)]
pub enum ClientBoundary {
    Stream,
    ReleaseReadiness,
    ReleaseProcessing,
    Scope,
}

#[derive(Clone, Copy, Debug)]
pub(crate) enum JoinGate {
    EntryCap,
    WaiterCap,
    WaiterId,
    Identity,
    Insert,
    ExistingFailed,
    Unclassified,
    WaiterQuota,
    FlightQuota,
}

#[derive(Clone, Copy, Debug)]
pub(crate) enum WaitGate {
    QueueFull,
    AllowanceExpired,
    Queued,
    ScopeDeadline,
    BudgetDeadline,
    Cancelled,
    QueueAdmissionRejected,
    AdmissionFailed,
}

#[derive(Clone, Copy, Debug)]
pub(crate) enum GateFacts {
    Wait {
        reason: WaitGate,

        copy: bool,

        cause: Option<JoinGate>,

        error: Error,
    },
    Reactor(uring_runtime::reactor::SubmissionRejection),
    Join {
        gate: JoinGate,
        copy: bool,
        used: Option<usize>,
        limit: Option<usize>,
        detail: Option<Detail>,
    },
}

impl GateFacts {
    fn write(self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        match self {
            Self::Wait {
                reason,
                copy,
                cause,
                error,
            } => write!(
                out,
                "Wait reason={reason:?} copy={copy} error={error:?} last_observed_cause={cause:?} cause_time=prior_observation_not_terminal_capacity"
            ),
            Self::Reactor(facts) => write!(out, "{facts:?}"),
            Self::Join {
                gate,
                copy,
                used,
                limit,
                detail,
            } => write!(
                out,
                "Join gate={gate:?} copy={copy} used={used:?} limit={limit:?} requested=1 quota={detail:?}"
            ),
        }
    }
    fn index(self) -> usize {
        match self {
            Self::Wait { reason, copy, .. } => 42 + reason as usize * 2 + usize::from(copy),
            Self::Reactor(f) => f.reason as usize * 3 + f.kind as usize,
            Self::Join { gate, copy, .. } => 24 + gate as usize * 2 + usize::from(copy),
        }
    }
}

#[derive(Clone, Copy)]
struct GateEvent {
    worker: WorkerId,

    failure: Failure,

    page: Option<u64>,

    facts: GateFacts,
}

struct GateEvents {
    ring: SharedRing<GateEvent, 64>,

    counts: [std::sync::atomic::AtomicU64; 58],
}
impl Default for GateEvents {
    fn default() -> Self {
        Self {
            ring: SharedRing::default(),
            counts: std::array::from_fn(|_| std::sync::atomic::AtomicU64::new(0)),
        }
    }
}

/// Fixed-size facts only. Times are absolute Unix milliseconds through the same
/// stable monotonic mapping as signed deadlines, not fresh relative allowances.
/// Missing original/share means this process did not create the candidate.
#[derive(Clone, Copy, Debug)]
pub struct BodyProgress {
    pub received: u32,
    pub expected: u32,
    pub reads: u32,
    pub first: u64,
    pub last: u64,
    pub now: u64,
    pub original: u64,
    pub share: u64,
    pub signed: u64,
    pub remote: [u8; 36],
    pub tuple: Option<(std::net::SocketAddr, std::net::SocketAddr)>,
}

pub(crate) fn timestamp(at: std::time::Instant) -> u64 {
    crate::peer::protocol::encode_deadline(uring_runtime::environment::Deadline(at))
        .unwrap_or_default()
}

#[derive(Clone, Copy, Debug)]
pub struct Failure {
    pub unix_millis: u64,
    pub stage: Stage,
    pub error: Error,
    pub request: Option<RequestId>,
    pub attempt: Option<AttemptId>,
    pub detail: Detail,
}
impl Failure {
    pub fn new(stage: Stage, error: Error) -> Self {
        Self {
            unix_millis: uring_runtime::environment::wall_now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis()
                .min(u64::MAX as u128) as u64,
            stage,
            error,
            request: None,
            attempt: None,
            detail: Detail::None,
        }
    }
    pub fn request(mut self, scope: &RequestScope) -> Self {
        self.request = Some(scope.request);
        self
    }
    pub fn attempt(mut self, attempt: AttemptId) -> Self {
        self.attempt = Some(attempt);
        self
    }
    pub fn detail(mut self, detail: Detail) -> Self {
        self.detail = detail;
        self
    }
}

#[derive(Clone, Default)]
pub struct Failures(
    SharedRing<(WorkerId, Failure), FAILURE_CAPACITY>,
    AeadRing,
    SharedRing<ProtectedFailure, 64>,
    SharedRing<ProtectedFailure, 64>,
    SharedRing<(WorkerId, CandidateFinal), 16>,
    std::sync::Arc<GateEvents>,
    SharedRing<(WorkerId, RequestId, u64, FlightExpiry), 16>,
    SharedRing<DecryptFailure, DECRYPT_FAILURE_CAPACITY>,
);

/// One sampled entry, with no object identity or owned resources.
#[derive(Clone, Copy, Debug)]
pub(crate) struct FlightExpiryEntry {
    pub age_us: u64,

    pub lifecycle: crate::read::flight::FlightState,

    pub stage: Option<crate::read::flight::DriverStage>,

    pub stage_age_us: Option<u64>,

    pub cancel_request_seen_age_us: Option<u64>,

    pub driver_abandoned: bool,

    pub waiters: usize,

    pub operations: usize,

    pub completing: usize,
}

/// Fixed expiry facts; local state and shared quota counters are not an atomic cut.
#[derive(Clone, Copy, Debug)]
pub(crate) struct FlightExpiry {
    pub observed_unix_ms: u64,

    pub overshoot_us: u64,

    pub queue: uring_runtime::offload::AdmissionSnapshot,

    pub caller_head: Option<bool>,

    pub caller_age_us: Option<u64>,

    pub total: usize,

    pub entry_limit: usize,

    pub flight_used: usize,

    pub flight_limit: usize,

    pub waiter_used: usize,

    pub waiter_limit: usize,

    pub entries: [Option<FlightExpiryEntry>; 6],
}

/// Operation-local recent facts, not a client outcome or a transport diagnosis.
/// Resolve and remaining-copy failures are recorded only after fallback ends.
/// Origin-miss records retain predecessor events, not later copy-miss events.
/// Standalone subscription attempts and errors returned directly by hedging are
/// outside coverage. A resolution after hedging marks its missing prior history.
/// Four recent events travel with each final record; validation can add a second
/// event for one attempt. No global ring lookup is needed to reconstruct this tail.
#[derive(Clone, Copy, Debug)]
pub(crate) enum CandidateSite {
    Resolve,
    Prepare,
    Exchange,
    Funding,
    Exhausted,
    Validation,
    RemainingCopy,
    OriginMiss,
}
#[derive(Clone, Copy, Debug)]
pub(crate) enum CandidateOperation {
    Page,
    Bootstrap,
    Metadata,
    Subscribe,
}
#[derive(Clone, Copy)]
pub(crate) enum CandidateOutcome {
    Success,
    Miss,
    Error(Error),
    ResponseError(Error),
    UnusableCopy,
}
impl std::fmt::Debug for CandidateOutcome {
    fn fmt(&self, out: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Success => out.write_str("Success"),
            Self::Miss => out.write_str("Miss"),
            Self::Error(error) => out.debug_tuple("Error").field(error).finish(),
            Self::ResponseError(error) => out.debug_tuple("ResponseError").field(error).finish(),
            Self::UnusableCopy => out.write_str("UnusableCopy"),
        }
    }
}
#[derive(Clone, Copy, Debug)]
pub(crate) struct CandidateAttempt {
    // Ranked destination identity, not necessarily the immediate TCP peer.
    pub destination: [u8; 36],
    pub membership: u64,
    pub rank: u8,
    pub acquire: bool,
    pub attempt: Option<AttemptId>,
    pub site: CandidateSite,
    pub raw: Option<CandidateOutcome>,
    pub effective: Option<Error>,
    // Observed local stop, not a classification of idle versus ETA versus total cap.
    pub stop: Option<Error>,
    pub attempts: u32,
    pub links: u8,
    pub overall: u64,
    pub share: u64,
    pub cap: u64,
}
#[derive(Clone)]
pub(crate) struct CandidateTrail {
    pub site: CandidateSite,
    pub hedge_history_unavailable: bool,
    pub recent: Ring<CandidateAttempt, 4>,
}
impl Default for CandidateTrail {
    fn default() -> Self {
        Self {
            site: CandidateSite::Resolve,
            hedge_history_unavailable: false,
            recent: Ring::default(),
        }
    }
}
#[derive(Clone)]
pub(crate) struct CandidateFinal {
    pub failure: Failure,
    pub operation: CandidateOperation,
    pub page: u64,
    pub membership: u64,
    // Membership above is the initial operation version; events carry their own.
    pub trail: CandidateTrail,
}

/// Fixed call sites, never user input or identifier labels. Admission records
/// describe a failed Fill operation, not necessarily the final client outcome.
#[derive(Clone, Copy, Debug)]
pub(crate) enum FillAdmissionSite {
    RetainedPlaintext,
    SelectedCiphertext,
    SelectedPlaintext,
    LocalPlaintext,
    DiskCiphertext,
    NetworkPlaintext,
    OriginCiphertext,
    OriginPlaintext,
    ResponsePlaintext,
}
#[derive(Clone, Copy)]
struct ProtectedFailure {
    worker: WorkerId,
    failure: Failure,
    site: Option<FillAdmissionSite>,
    page: Option<u64>,
}

/// Absent in standalone components until the production composition attaches it.
#[derive(Clone, Default)]
pub struct Observer(Option<(Failures, WorkerId)>);
impl Failures {
    /// Write the independent bounded expiry ring without changing gate counters.
    pub(crate) fn write_flight_expiry(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let snapshot = self.6.snapshot();
        writeln!(
            out,
            "schema_version=1 capacity=16 sample_limit=6 selection=index_order_not_oldest capture=expiry_poll_before_unwind quota_counters=separate_non_atomic_samples stage=driver_call_not_backend_execution candidate_stage=composite_including_nested_validation absent_stage=unknown completing=flight_resource_destruction_fence backend_phase=unknown total={} retained={} overwritten={}",
            snapshot.total(),
            snapshot.len(),
            snapshot.total().saturating_sub(snapshot.len() as u64)
        )?;
        for (sequence, (worker, request, page, facts)) in snapshot.iter() {
            let sampled = facts.entries.iter().flatten().count();
            write!(out, "sequence={sequence} worker={} request=", worker.0)?;
            hex(out, &request.0)?;
            write!(
                out,
                " page={page} total_entries={} sampled={sampled} omitted={} truncated={} observed_unix_ms={} overshoot_us={} queue_len={} queue_limit={} head_age_us={:?} tail_age_us={:?} caller_head={:?} caller_age_us={:?} entry_limit={} flight_used={} flight_limit={} waiter_used={} waiter_limit={}",
                facts.total,
                facts.total.saturating_sub(sampled),
                facts.total > sampled,
                facts.observed_unix_ms,
                facts.overshoot_us,
                facts.queue.queued,
                facts.queue.capacity,
                facts.queue.head_age_us,
                facts.queue.tail_age_us,
                facts.caller_head,
                facts.caller_age_us,
                facts.entry_limit,
                facts.flight_used,
                facts.flight_limit,
                facts.waiter_used,
                facts.waiter_limit
            )?;
            for (index, entry) in facts.entries.iter().flatten().enumerate() {
                write!(
                    out,
                    " entry{index}=[age_us={},lifecycle={:?},stage={:?},stage_age_us={:?},cancel_request_seen={},cancel_request_seen_age_us={:?},driver_abandoned={},waiters={},operations={},retained={},completing={}]",
                    entry.age_us,
                    entry.lifecycle,
                    entry.stage,
                    entry.stage_age_us,
                    entry.cancel_request_seen_age_us.is_some(),
                    entry.cancel_request_seen_age_us,
                    entry.driver_abandoned,
                    entry.waiters,
                    entry.operations,
                    entry.operations.saturating_sub(entry.completing),
                    entry.completing
                )?;
            }
            writeln!(out)?;
        }
        Ok(())
    }

    pub(crate) fn write_gate_events(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let snapshot = self.5.ring.snapshot();
        writeln!(
            out,
            "schema_version=1 counter_rows=58 coverage=flight_join_and_reactor_submit_events terminal_cause=false operation_identity=not_recorded page_identity=request_relative_number existing_failed=propagated_not_new_rejection unclassified=join_overload_origin_unknown wait_events=terminal_once wait_cause=prior_observation_not_terminal_capacity counters_and_ring=non_atomic_snapshot total={} retained={} overwritten={} capacity=64",
            snapshot.total(),
            snapshot.len(),
            snapshot.total().saturating_sub(snapshot.len() as u64)
        )?;
        for (index, count) in self.5.counts.iter().enumerate() {
            let reason = if index < 24 {
                [
                    "Initialization",
                    "ReservedFull",
                    "OrdinaryFull",
                    "Bookkeeping",
                    "EntriesFull",
                    "PartitionFull",
                    "IdExhausted",
                    "SubmissionQueueFull",
                ][index / 3]
            } else if index >= 42 {
                [
                    "QueueFull",
                    "AllowanceExpired",
                    "Queued",
                    "ScopeDeadline",
                    "BudgetDeadline",
                    "Cancelled",
                    "QueueAdmissionRejected",
                    "AdmissionFailed",
                ][(index - 42) / 2]
            } else {
                [
                    "EntryCap",
                    "WaiterCap",
                    "WaiterId",
                    "Identity",
                    "Insert",
                    "ExistingFailed",
                    "Unclassified",
                    "WaiterQuota",
                    "FlightQuota",
                ][(index - 24) / 2]
            };
            let kind = if index < 24 {
                ["OtherIo", "Readiness", "Timeout"][index % 3]
            } else {
                ["Join", "JoinCopy"][(index - 24) % 2]
            };
            writeln!(
                out,
                "counter={index} reason={reason} kind={kind} events={}",
                count.load(std::sync::atomic::Ordering::Relaxed)
            )?;
        }
        for (sequence, event) in snapshot.iter() {
            write!(
                out,
                "sequence={sequence} worker={} unix_millis={} request=",
                event.worker.0, event.failure.unix_millis
            )?;
            if let Some(request) = event.failure.request {
                hex(out, &request.0)?;
            } else {
                write!(out, "none")?;
            }
            write!(
                out,
                " page={:?} counter={} facts=",
                event.page,
                event.facts.index(),
            )?;
            event.facts.write(out)?;
            writeln!(out)?;
        }
        Ok(())
    }
    pub(crate) fn write_candidate_final(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let snapshot = self.4.snapshot();
        writeln!(
            out,
            "schema_version=1 total={} retained={} overwritten={} capacity=16 coverage=candidate_operation_final operations=resolve,remaining_copy,origin_miss excluded=standalone_subscribe,hedge_errors origin_miss_trail=predecessors_only trail_capacity=4 trail_unit=events",
            snapshot.total(),
            snapshot.len(),
            snapshot.total().saturating_sub(snapshot.len() as u64)
        )?;
        for (sequence, (worker, record)) in snapshot.iter_refs() {
            let f = record.failure;
            write!(
                out,
                "sequence={sequence} worker={} unix_millis={} request=",
                worker.0, f.unix_millis
            )?;
            if let Some(request) = f.request {
                hex(out, &request.0)?;
            } else {
                write!(out, "none")?;
            }
            writeln!(
                out,
                " operation={:?} page={} membership={} site={:?} error={:?} budget={:?} hedge_history_unavailable={} omitted={}",
                record.operation,
                record.page,
                record.membership,
                record.trail.site,
                f.error,
                f.detail,
                record.trail.hedge_history_unavailable,
                record
                    .trail
                    .recent
                    .total()
                    .saturating_sub(record.trail.recent.len() as u64)
            )?;
            for (index, a) in record.trail.recent.iter() {
                write!(
                    out,
                    " candidate={index} destination={} membership={} rank={} acquire={} attempt=",
                    std::str::from_utf8(&a.destination).unwrap_or("unknown"),
                    a.membership,
                    a.rank,
                    a.acquire
                )?;
                if let Some(attempt) = a.attempt {
                    hex(out, &attempt.0)?;
                } else {
                    write!(out, "none")?;
                }
                writeln!(
                    out,
                    " site={:?} raw={:?} effective={:?} stop={:?} attempts={} links={} overall={} share={} cap={}",
                    a.site,
                    a.raw,
                    a.effective,
                    a.stop,
                    a.attempts,
                    a.links,
                    a.overall,
                    a.share,
                    a.cap
                )?;
            }
        }
        Ok(())
    }
    pub(crate) fn write_protected(
        &self,
        admission: bool,
        out: &mut impl std::fmt::Write,
    ) -> std::fmt::Result {
        let ring = if admission { &self.3 } else { &self.2 };
        let snapshot = ring.snapshot();
        writeln!(
            out,
            "schema_version=1 total={} retained={} overwritten={} capacity=64 coverage={}",
            snapshot.total(),
            snapshot.len(),
            snapshot.total().saturating_sub(snapshot.len() as u64),
            if admission {
                "fill_only"
            } else {
                "client_boundaries_resource_unknown next_slice_unexpected_end=untagged first_slice_unexpected_end=stream"
            }
        )?;
        for (sequence, record) in snapshot.iter() {
            let failure = record.failure;
            write!(
                out,
                "sequence={sequence} worker={} stage={:?} error={:?} unix_millis={} request=",
                record.worker.0, failure.stage, failure.error, failure.unix_millis
            )?;
            if let Some(request) = failure.request {
                hex(out, &request.0)?;
            } else {
                write!(out, "none")?;
            }
            write!(out, " attempt=")?;
            if let Some(attempt) = failure.attempt {
                hex(out, &attempt.0)?;
            } else {
                write!(out, "none")?;
            }
            writeln!(
                out,
                " site={:?} page={:?} facts={} detail={:?}",
                record.site,
                record.page,
                if admission
                    && matches!(
                        failure.detail,
                        Detail::Resource { .. } | Detail::CacheEntries { .. }
                    )
                {
                    "same_call_rejection"
                } else {
                    "unknown"
                },
                failure.detail
            )?;
        }
        Ok(())
    }
    pub fn write_aead(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let ring = self.1.snapshot();
        let (total, len) = (ring.total(), ring.len());
        writeln!(
            out,
            "total={total} retained={len} overwritten={} capacity={AEAD_CAPACITY}",
            total.saturating_sub(len as u64)
        )?;
        for (sequence, (id, f)) in ring.iter() {
            write!(
                out,
                "seq={sequence} w={} crypto={}:{} ms={} request=",
                id.worker.0, id.generation, id.sequence, f.unix_millis
            )?;
            hex(out, &f.request.0)?;
            if let Some(p) = f.peer {
                write!(out, " acquisition=")?;
                hex(out, &p.request.0)?;
                write!(out, " attempt=")?;
                hex(out, &p.attempt.0)?;
                write!(
                    out,
                    " supplier={} remote={}",
                    std::str::from_utf8(&p.supplier).unwrap_or("unknown"),
                    std::str::from_utf8(&p.remote).unwrap_or("unknown")
                )?;
            } else {
                write!(
                    out,
                    " acquisition=none attempt=none supplier=none remote=none"
                )?;
            }
            f.write_fingerprint(out)?;
            writeln!(out)?;
        }
        Ok(())
    }
    pub fn write_decrypt(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let ring = self.7.snapshot();
        let (total, len) = (ring.total(), ring.len());
        writeln!(
            out,
            "schema_version=1 coverage=executed_decrypt_failure_at_reap total={total} retained={len} overwritten={} capacity={DECRYPT_FAILURE_CAPACITY} page_kind=envelope_number_not_object_identity scope_at_reap=unknown",
            total.saturating_sub(len as u64)
        )?;
        fn optional(out: &mut impl std::fmt::Write, value: Option<u64>) -> std::fmt::Result {
            match value {
                Some(value) => write!(out, "{value}"),
                None => write!(out, "unknown"),
            }
        }
        for (sequence, f) in ring.iter() {
            write!(
                out,
                "seq={sequence} worker={} crypto={}:{} error={:?} reason={} request=",
                f.id.worker.0,
                f.id.generation,
                f.id.sequence,
                f.error,
                f.reason.name()
            )?;
            match f.request {
                Some(request) => hex(out, &request.0)?,
                None => write!(out, "unknown")?,
            }
            write!(out, " page=")?;
            optional(out, f.page)?;
            write!(out, " engine_failure_observed_ms=")?;
            optional(out, f.engine_ms)?;
            write!(out, " reap_ms={} scope_at_failure=", f.reap_ms)?;
            match f.scope_failure {
                DecryptScopeFailure::Unknown => write!(out, "unknown")?,
                DecryptScopeFailure::Entry(error) => write!(out, "entry_check:{error:?}")?,
                DecryptScopeFailure::Final(error) => write!(out, "final_check:{error:?}")?,
            }
            write!(out, " scope_at_reap=unknown queue_ns=")?;
            optional(out, f.queue_ns)?;
            write!(out, " execution_ns=")?;
            optional(out, f.execution_ns)?;
            writeln!(out)?;
        }
        Ok(())
    }
    pub fn observer(&self, worker: WorkerId) -> Observer {
        Observer(Some((self.clone(), worker)))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        // Copy bounded records before formatting; never hold the lock across I/O.
        let ring = self.0.snapshot();
        let (total, len) = (ring.total(), ring.len());
        writeln!(
            out,
            "total={total} retained={len} capacity={FAILURE_CAPACITY}"
        )?;
        for (sequence, (worker, failure)) in ring.iter() {
            let body = matches!(failure.detail, Detail::Body(_));
            if body {
                write!(
                    out,
                    "seq={sequence:x} w={} stage={:?} error={:?} request=",
                    worker.0, failure.stage, failure.error
                )?;
            } else {
                write!(
                    out,
                    "sequence={sequence} worker={} stage={:?} error={:?} request=",
                    worker.0, failure.stage, failure.error
                )?;
            }
            if let Some(request) = failure.request {
                hex(out, &request.0)?;
            } else {
                write!(out, "none")?;
            }
            write!(out, " attempt=")?;
            if let Some(attempt) = failure.attempt {
                hex(out, &attempt.0)?;
            } else {
                write!(out, "none")?;
            }
            if let Detail::Body(b) = failure.detail {
                // Compact formatting keeps all 128 worst-case records within
                // the existing 64-KiB diagnostic response budget.
                write!(
                    out,
                    " detail=Body rx={}/{} n={} ms=hex f={:x} l={:x} now={:x} orig={:x} share={:x} sig={:x} remote={}",
                    b.received,
                    b.expected,
                    b.reads,
                    b.first,
                    b.last,
                    b.now,
                    b.original,
                    b.share,
                    b.signed,
                    std::str::from_utf8(&b.remote).unwrap_or("unknown")
                )?;
                if let Some((local, remote)) = b.tuple {
                    write!(out, " tcp={local}>{remote}")?;
                } else {
                    write!(out, " tcp=none")?;
                }
                writeln!(out)?;
            } else {
                writeln!(
                    out,
                    " unix_millis={} detail={:?}",
                    failure.unix_millis, failure.detail
                )?;
            }
        }
        Ok(())
    }
}
impl Observer {
    /// Publish already frozen facts after all worker-local borrows have ended.
    pub(crate) fn flight_expiry(&self, scope: &RequestScope, page: u64, facts: FlightExpiry) {
        if let Some((failures, worker)) = &self.0 {
            failures.6.push((*worker, scope.request, page, facts));
        }
    }

    pub(crate) fn gate_rejection(&self, scope: &RequestScope, page: Option<u64>, facts: GateFacts) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        failures.5.counts[facts.index()].fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        failures.5.ring.push(GateEvent {
            worker: *worker,
            failure: Failure::new(
                Stage::Admission,
                match facts {
                    GateFacts::Wait { error, .. } => error,
                    _ => Error::Overloaded,
                },
            )
            .request(scope),
            page,
            facts,
        });
    }
    pub(crate) fn candidate_final(&self, record: CandidateFinal) {
        if let Some((failures, worker)) = &self.0 {
            failures.4.push((*worker, record));
        }
    }
    pub(crate) fn record_aead(&self, id: crate::security::CryptoId, failure: AeadFailure) {
        let Some((failures, _)) = &self.0 else {
            return;
        };
        failures.1.push((id, failure));
    }
    pub(crate) fn record_decrypt(&self, failure: DecryptFailure) {
        if let Some((failures, _)) = &self.0 {
            failures.7.push(failure);
        }
    }
    pub fn record(&self, failure: Failure) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        failures.0.push((*worker, failure));
        if matches!(
            failure.stage,
            Stage::ClientRead | Stage::FirstSlice | Stage::NextSlice | Stage::ClientWrite
        ) {
            failures.2.push(ProtectedFailure {
                worker: *worker,
                failure,
                site: None,
                page: None,
            });
        }
    }
    pub(crate) fn final_fill_admission(
        &self,
        scope: &RequestScope,
        page: Option<u64>,
        site: FillAdmissionSite,
        detail: Option<Detail>,
    ) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        failures.3.push(ProtectedFailure {
            worker: *worker,
            failure: Failure::new(Stage::Admission, Error::Overloaded)
                .request(scope)
                .detail(detail.unwrap_or_default()),
            site: Some(site),
            page,
        });
    }
    pub fn result<T>(&self, stage: Stage, scope: &RequestScope, result: Result<T>) -> Result<T> {
        if let Err(error) = &result {
            self.record(Failure::new(stage, *error).request(scope));
        }
        result
    }
}

#[cfg(test)]
mod gate_event_tests {
    use super::*;

    /// Maximum scalar widths and full samples fit the endpoint without gate noise.
    #[test]
    fn expiry_snapshot_ring_is_bounded_private_and_independent() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(u16::MAX));
        let scope = RequestScope::new(
            RequestId([255; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let facts = FlightExpiry {
            observed_unix_ms: u64::MAX,
            overshoot_us: u64::MAX,
            queue: uring_runtime::offload::AdmissionSnapshot {
                queued: usize::MAX,
                capacity: usize::MAX,
                head: Some(u64::MAX),
                head_age_us: Some(u64::MAX),
                tail_age_us: Some(u64::MAX),
            },
            caller_head: Some(false),
            caller_age_us: Some(u64::MAX),
            total: usize::MAX,
            entry_limit: usize::MAX,
            flight_used: usize::MAX,
            flight_limit: usize::MAX,
            waiter_used: usize::MAX,
            waiter_limit: usize::MAX,
            entries: [Some(FlightExpiryEntry {
                age_us: u64::MAX,
                lifecycle: crate::read::flight::FlightState::RetryPending,
                stage: Some(crate::read::flight::DriverStage::CandidateCompositeCall),
                stage_age_us: Some(u64::MAX),
                cancel_request_seen_age_us: Some(u64::MAX),
                driver_abandoned: true,
                waiters: usize::MAX,
                operations: usize::MAX,
                completing: usize::MAX,
            }); 6],
        };
        for _ in 0..17 {
            observer.flight_expiry(&scope, u64::MAX, facts);
        }
        let mut text = String::new();
        failures.write_flight_expiry(&mut text).unwrap();
        assert!(text.contains("total=17 retained=16 overwritten=1"));
        assert_eq!(text.lines().count(), 17);
        assert!(text.len() < MAX_RESPONSE_BYTES - 256, "{}", text.len());
        assert!(text.contains("selection=index_order_not_oldest"));
        assert!(text.contains("truncated=true"));
        assert!(!text.contains("object="));
        assert!(!text.contains("key="));
        text.clear();
        failures.write_gate_events(&mut text).unwrap();
        assert!(text.contains("schema_version=1 counter_rows=58"));
        assert!(text.contains("total=0 retained=0 overwritten=0"));
    }

    #[test]
    fn gate_events_wrap_without_losing_counts_or_accepting_noise() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(u16::MAX));
        let scope = RequestScope::new(
            RequestId([255; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        for _ in 0..65 {
            observer.gate_rejection(
                &scope,
                Some(u64::MAX),
                GateFacts::Join {
                    gate: JoinGate::EntryCap,
                    copy: false,
                    used: Some(usize::MAX),
                    limit: Some(usize::MAX),
                    detail: None,
                },
            );
        }
        for _ in 0..4096 {
            observer.record(Failure::new(Stage::Admission, Error::Overloaded));
        }
        let mut output = String::new();
        failures.write_gate_events(&mut output).unwrap();
        assert!(output.contains("total=65 retained=64 overwritten=1 capacity=64"));
        assert!(output.contains("schema_version=1 counter_rows=58"));
        assert_eq!(
            output
                .lines()
                .filter(|line| line.starts_with("counter="))
                .count(),
            58
        );
        assert!(output.contains("counter=42 reason=QueueFull kind=Join events=0"));
        assert!(output.contains("counter=53 reason=Cancelled kind=JoinCopy events=0"));
        assert!(output.contains("reason=EntryCap kind=Join events=65"));
        assert!(output.contains("terminal_cause=false operation_identity=not_recorded"));
        assert!(output.contains("request=ffffffffffffffffffffffffffffffff"));
        assert!(output.len() < MAX_RESPONSE_BYTES - 256);
        for _ in 0..64 {
            observer.gate_rejection(
                &scope,
                Some(u64::MAX),
                GateFacts::Reactor(uring_runtime::reactor::SubmissionRejection {
                    reason: uring_runtime::reactor::RejectionReason::SubmissionQueueFull,
                    kind: uring_runtime::reactor::SubmissionKind::Readiness,
                    reserved: true,
                    used: Some(usize::MAX),
                    limit: Some(usize::MAX),
                    requested: usize::MAX,
                }),
            );
        }
        output.clear();
        failures.write_gate_events(&mut output).unwrap();
        assert!(output.contains("total=129 retained=64 overwritten=65"));
        assert!(output.len() + 64 * 20 < MAX_RESPONSE_BYTES - 256);
    }
}

#[cfg(test)]
mod protected_failure_tests {
    use super::*;
    #[test]
    fn candidate_final_retention_is_independent_bounded_and_nonconsuming() {
        let telemetry = Telemetry::default();
        let observer = telemetry.failures.observer(WorkerId(u16::MAX));
        let mut trail = CandidateTrail::default();
        for _ in 0..6 {
            trail.recent.push(CandidateAttempt {
                destination: [b'f'; 36],
                membership: u64::MAX,
                rank: u8::MAX,
                acquire: true,
                attempt: Some(AttemptId([255; 16])),
                site: CandidateSite::Exchange,
                raw: Some(CandidateOutcome::Error(
                    Error::UnsatisfiableRangeWithLength(u64::MAX),
                )),
                effective: Some(Error::UnsatisfiableRangeWithLength(u64::MAX)),
                stop: Some(Error::UnsatisfiableRangeWithLength(u64::MAX)),
                attempts: u32::MAX,
                links: u8::MAX,
                overall: u64::MAX,
                share: u64::MAX,
                cap: u64::MAX,
            });
        }
        let mut failure =
            Failure::new(Stage::CandidateExhausted, Error::Unavailable).detail(Detail::Budget {
                attempts: u32::MAX,
                links: u8::MAX,
            });
        failure.request = Some(RequestId([255; 16]));
        failure.unix_millis = u64::MAX;
        for _ in 0..17 {
            observer.candidate_final(CandidateFinal {
                failure,
                operation: CandidateOperation::Bootstrap,
                page: u64::MAX,
                membership: u64::MAX,
                trail: trail.clone(),
            });
        }
        observer.record(Failure::new(Stage::NextSlice, Error::Unavailable));
        for _ in 0..4096 {
            observer.record(Failure::new(Stage::Admission, Error::Overloaded));
            observer.record(Failure::new(Stage::CandidateExchange, Error::Io));
        }
        struct Output<'a> {
            failures: &'a Failures,
            text: String,
            fail: bool,
        }
        impl std::fmt::Write for Output<'_> {
            fn write_str(&mut self, text: &str) -> std::fmt::Result {
                assert!(self.failures.4.try_snapshot().is_some());
                if self.fail {
                    return Err(std::fmt::Error);
                }
                self.text.push_str(text);
                Ok(())
            }
        }
        let mut out = Output {
            failures: &telemetry.failures,
            text: String::new(),
            fail: true,
        };
        assert!(get(&telemetry, "/debug/candidate-final", true, &mut out).is_err());
        out.fail = false;
        get(&telemetry, "/debug/candidate-final", true, &mut out).unwrap();
        assert!(
            out.text
                .contains("total=17 retained=16 overwritten=1 capacity=16")
        );
        assert!(out.text.contains("coverage=candidate_operation_final"));
        assert_eq!(out.text.matches(" omitted=2").count(), 16);
        assert_eq!(out.text.matches(" candidate=").count(), 64);
        assert!(out.text.len() + 512 < MAX_RESPONSE_BYTES);
        assert!(!out.text.contains("Admission"));
        let mut terminal = String::new();
        telemetry
            .failures
            .write_protected(false, &mut terminal)
            .unwrap();
        assert!(terminal.contains("total=1 retained=1 overwritten=0"));
    }
    #[test]
    fn protected_endpoints_preserve_records_and_format_outside_locks() {
        let telemetry = Telemetry::default();
        let observer = telemetry.failures.observer(WorkerId(3));
        observer.record(Failure::new(Stage::ClientWrite, Error::Io));
        struct Output<'a> {
            failures: &'a Failures,
            text: String,
            fail: bool,
        }
        impl std::fmt::Write for Output<'_> {
            fn write_str(&mut self, s: &str) -> std::fmt::Result {
                assert!(self.failures.0.try_snapshot().is_some());
                assert!(self.failures.1.try_snapshot().is_some());
                assert!(self.failures.2.try_snapshot().is_some());
                assert!(self.failures.3.try_snapshot().is_some());
                if self.fail {
                    return Err(std::fmt::Error);
                }
                self.text.push_str(s);
                Ok(())
            }
        }
        let mut out = Output {
            failures: &telemetry.failures,
            text: String::new(),
            fail: true,
        };
        assert!(get(&telemetry, "/debug/terminal", true, &mut out).is_err());
        out.fail = false;
        get(&telemetry, "/debug/terminal", true, &mut out).unwrap();
        assert!(out.text.contains("schema_version=1 total=1 retained=1"));
        assert!(out.text.contains("stage=ClientWrite error=Io"));
        assert!(out.text.contains("facts=unknown"));
        out.text.clear();
        get(&telemetry, "/debug/admission-final", true, &mut out).unwrap();
        assert!(out.text.contains("total=0 retained=0"));
        for _ in 0..64 {
            observer.record(Failure::new(Stage::ClientRead, Error::Unavailable));
        }
        out.text.clear();
        get(&telemetry, "/debug/terminal", true, &mut out).unwrap();
        assert!(out.text.contains("total=65 retained=64 overwritten=1"));
        assert!(!out.text.contains("stage=ClientWrite"));
    }
    #[test]
    fn terminal_and_final_fill_survive_noise_and_wrap_independently() {
        let failures = Failures::default();
        let observer = failures.observer(WorkerId(u16::MAX));
        let scope = RequestScope::new(
            RequestId([255; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        observer.record(
            Failure::new(Stage::NextSlice, Error::Overloaded)
                .request(&scope)
                .detail(Detail::Delivery {
                    sent: u64::MAX,
                    expected: u64::MAX,
                }),
        );
        let detail = Detail::Resource {
            class: ResourceClass::Plaintext,
            used: usize::MAX,
            limit: usize::MAX,
            requested: usize::MAX,
            cache_used: Some(usize::MAX),
            cache_limit: Some(usize::MAX),
        };
        observer.final_fill_admission(
            &scope,
            Some(u64::MAX),
            FillAdmissionSite::NetworkPlaintext,
            Some(detail),
        );
        for _ in 0..4096 {
            observer.record(Failure::new(Stage::Admission, Error::Overloaded));
        }
        for admission in [false, true] {
            let mut output = String::new();
            failures.write_protected(admission, &mut output).unwrap();
            assert!(output.contains("total=1 retained=1 overwritten=0 capacity=64"));
            assert!(output.contains(if admission {
                "class: Plaintext"
            } else {
                "stage=NextSlice"
            }));
        }
        for _ in 0..64 {
            observer.final_fill_admission(
                &scope,
                Some(u64::MAX),
                FillAdmissionSite::DiskCiphertext,
                Some(detail),
            );
        }
        let mut output = String::new();
        failures.write_protected(true, &mut output).unwrap();
        assert!(output.contains("total=65 retained=64 overwritten=1 capacity=64"));
        assert_eq!(output.lines().count(), 65);
        assert!(output.len() + 64 * 20 < MAX_RESPONSE_BYTES - 256);
        output.clear();
        failures.write_protected(false, &mut output).unwrap();
        assert!(output.contains("total=1 retained=1 overwritten=0"));
    }
}

#[cfg(test)]
pub(crate) fn test_aead_failure() -> AeadFailure {
    AeadFailure {
        unix_millis: u64::MAX,
        request: RequestId([255; 16]),
        peer: Some(PeerProvenance {
            request: RequestId([255; 16]),
            attempt: AttemptId([255; 16]),
            supplier: [b'f'; 36],
            remote: [b'f'; 36],
        }),
        page: [255; 32],
        number: u64::MAX,
        key: [255; 16],
        nonce: [255; 24],
        plaintext: u32::MAX,
        ciphertext: u32::MAX,
        aad: [255; 32],
        crc: Some(u64::MAX),
    }
}

// Fixed metric names; only installed runtime worker IDs are exposed as labels.
//
// Integrity diagnostics are two independent axes, not a reason/source matrix:
// - `racer_crypto_decrypt_{crc,aead}_rejected_total` counts the exact failed
//   check once at completion reap, including abandoned jobs. Structural errors,
//   missing keys, and cancellation are not classified as CRC or AEAD failures.
// - `racer_fill_decrypt_{disk,retained,peer}_corrupt_total` counts CorruptRecord
//   results observed by the fill decrypt helper (including its structural checks).
//   Retained includes memory, flights, and pending writes, even for copies first
//   read from disk or peers. Earlier read/response parsing failures are excluded.
// These counters count attempts, not unique records or corrupt client deliveries,
// and identify rejection/check location, not where corruption originated. Source
// totals need not equal crypto totals, especially with abandoned fill waiters.

#[derive(Clone, Copy)]
pub(crate) enum LookupTier {
    Plaintext,
    Ciphertext,
    Pending,
    DiskIndex,
}
/// Fixed warm lifecycle events; callers record each transition once, not each poll.
#[derive(Clone, Copy, Debug)]
pub enum WarmEvent {
    Accepted,
    Refused,
    Completed,
    Failed,
    WaitStarted,
    WaitCompleted,
    WaitFallback,
    WaitFailed,
    RemoteHoldAccepted,
    RemoteHoldRefused,
    RemoteFillCompleted,
    RemoteFillRetried,
    RemoteFillFailed,
    RemoteReleaseSent,
    RemoteReleaseFailed,
    OwnerHoldAccepted,
    OwnerHoldRefused,
    OwnerFillCompleted,
    OwnerFillFailed,
    OwnerReleased,
}
impl WarmEvent {
    fn event(self) -> Event {
        match self {
            Self::Accepted => Event::WarmAccepted,
            Self::Refused => Event::WarmRefused,
            Self::Completed => Event::WarmCompleted,
            Self::Failed => Event::WarmFailed,
            Self::WaitStarted => Event::WarmWaitStarted,
            Self::WaitCompleted => Event::WarmWaitCompleted,
            Self::WaitFallback => Event::WarmWaitFallback,
            Self::WaitFailed => Event::WarmWaitFailed,
            Self::RemoteHoldAccepted => Event::WarmRemoteHoldAccepted,
            Self::RemoteHoldRefused => Event::WarmRemoteHoldRefused,
            Self::RemoteFillCompleted => Event::WarmRemoteFillCompleted,
            Self::RemoteFillRetried => Event::WarmRemoteFillRetried,
            Self::RemoteFillFailed => Event::WarmRemoteFillFailed,
            Self::RemoteReleaseSent => Event::WarmRemoteReleaseSent,
            Self::RemoteReleaseFailed => Event::WarmRemoteReleaseFailed,
            Self::OwnerHoldAccepted => Event::WarmOwnerHoldAccepted,
            Self::OwnerHoldRefused => Event::WarmOwnerHoldRefused,
            Self::OwnerFillCompleted => Event::WarmOwnerFillCompleted,
            Self::OwnerFillFailed => Event::WarmOwnerFillFailed,
            Self::OwnerReleased => Event::WarmOwnerReleased,
        }
    }
}
/// Clones retain their writer shard; reads aggregate the fixed node registry.
#[derive(Clone)]
pub struct Metrics {
    core: ::telemetry::Metrics<Event, Gauge>,

    // Each registration pairs its worker identity with the shared quota view.
    #[allow(clippy::type_complexity)]
    admission: Arc<[OnceLock<(WorkerId, flow_control::SharedQuotas<AdmissionPolicy>)>]>,

    retention: ::telemetry::SnapshotShards<crate::retention::Snapshot>,

    held: ::telemetry::SnapshotShards<crate::store::catalog::HeldSnapshot>,

    disks: ::telemetry::SnapshotShards<Vec<DiskUsage>>,

    memory: Arc<MemoryShards>,
    cache_disks: ::telemetry::SnapshotShards<Vec<CacheDiskUsage>>,

    /// GPU, cuFile, and GPU landing sources, shared by every worker.
    gpu: Arc<crate::gpu_telemetry::GpuTelemetry>,

    shard: usize,
}

/// Memory telemetry shared by every worker's handle, behind one pointer.
struct MemoryShards {
    staging: ::telemetry::SnapshotShards<Option<(WorkerId, flow_control::PoolStats)>>,

    // One node-wide slot written by the sampling worker.
    process: ::telemetry::SnapshotShards<ProcessMemory>,
}

/// Process and cgroup memory signals from the most recent sample.
/// Absent fields are omitted from the exposition.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct ProcessMemory {
    pub resident: Option<u64>,
    pub cgroup_current: Option<u64>,
    pub cgroup_limit: Option<u64>,
    pub pressure_some_avg10: Option<f64>,
}

impl From<&crate::pressure::MemorySample> for ProcessMemory {
    fn from(sample: &crate::pressure::MemorySample) -> Self {
        Self {
            resident: sample.resident,
            cgroup_current: sample.current,
            cgroup_limit: sample.limit,
            pressure_some_avg10: sample.some_avg10,
        }
    }
}

/// Snake-case `class` label values for every admission class, in index order.
const ADMISSION_CLASSES: [(crate::admission::ResourceClass, &str); 15] = {
    use crate::admission::ResourceClass as C;
    [
        (C::Plaintext, "plaintext"),
        (C::Ciphertext, "ciphertext"),
        (C::DirtyCiphertext, "dirty_ciphertext"),
        (C::Registered, "registered"),
        (C::RequestContext, "request_context"),
        (C::Flight, "flight"),
        (C::Waiter, "waiter"),
        (C::Connection, "connection"),
        (C::Pipe, "pipe"),
        (C::ControlProgress, "control_progress"),
        (C::Relay, "relay"),
        (C::IngressConnection, "ingress_connection"),
        (C::OutboundConnection, "outbound_connection"),
        (C::ControlConnection, "control_connection"),
        (
            C::OrdinaryOutboundConnection,
            "ordinary_outbound_connection",
        ),
    ]
};

/// Node-wide idle pool series: name suffix, Prometheus kind, and value.
fn pool_series(stats: &flow_control::PoolStats) -> [(&'static str, &'static str, u64); 7] {
    [
        ("entries", "gauge", stats.entries as u64),
        ("bytes", "gauge", stats.bytes as u64),
        ("target", "gauge", stats.target as u64),
        ("hits_total", "counter", stats.hits),
        ("misses_total", "counter", stats.misses),
        ("rejects_total", "counter", stats.rejects),
        ("trimmed_total", "counter", stats.trimmed),
    ]
}

/// Write node-wide sums as `racer_{family}_*` and only retained bytes per
/// worker as `racer_worker_{family}_bytes{worker="N"}`. Per-worker breakdowns of
/// every series would not fit the bounded diagnostic response on large nodes.
fn write_pools(
    out: &mut impl Write,
    family: &str,
    samples: &[(WorkerId, flow_control::PoolStats)],
) -> std::fmt::Result {
    if samples.is_empty() {
        return Ok(());
    }
    let mut totals = [0u64; 7];
    for (_, stats) in samples {
        for (total, (_, _, value)) in totals.iter_mut().zip(pool_series(stats)) {
            *total = total.saturating_add(value);
        }
    }
    for ((suffix, kind, _), total) in pool_series(&Default::default()).into_iter().zip(totals) {
        writeln!(
            out,
            "# TYPE racer_{family}_{suffix} {kind}\nracer_{family}_{suffix} {total}"
        )?;
    }
    writeln!(out, "# TYPE racer_worker_{family}_bytes gauge")?;
    for (worker, stats) in samples {
        writeln!(
            out,
            "racer_worker_{family}_bytes{{worker=\"{}\"}} {}",
            worker.0, stats.bytes
        )?;
    }
    Ok(())
}

#[derive(Clone)]
/// One worker's indexed payload and fair disk share for a published cache.
pub(crate) struct CacheDiskUsage {
    pub cache: Arc<str>,
    pub resident: u64,
    pub share: u64,
}

#[derive(Clone)]
/// One worker's assigned capacity and reserved record bytes on a backing disk.
pub(crate) struct DiskUsage {
    pub disk: Arc<str>,
    pub size: u64,
    pub used: u64,
}

fn write_disk_label(out: &mut impl Write, disk: &str) -> std::fmt::Result {
    for ch in disk.chars() {
        match ch {
            '\\' => out.write_str("\\\\")?,
            '"' => out.write_str("\\\"")?,
            '\n' => out.write_str("\\n")?,
            _ => out.write_char(ch)?,
        }
    }
    Ok(())
}
const RETENTION_METRICS: [(&str, &str); 10] = [
    ("racer_retention_observations_total", "counter"),
    ("racer_retention_qualified_total", "counter"),
    ("racer_retention_persistence_attempts_total", "counter"),
    ("racer_retention_persistence_accepted_total", "counter"),
    ("racer_retention_filter_set_bits", "gauge"),
    ("racer_retention_filter_bits", "gauge"),
    ("racer_retention_heat_entries", "gauge"),
    ("racer_disk_pending_payload_bytes", "gauge"),
    ("racer_disk_indexed_payload_bytes", "gauge"),
    ("racer_retention_share_rejected_total", "counter"),
];
const CACHE_DISK_METRICS: [&str; 2] = [
    "racer_cache_disk_resident_bytes",
    "racer_cache_disk_share_bytes",
];
const HELD_METRICS: [(&str, &str); 3] = [
    ("racer_warm_held_pages", "gauge"),
    ("racer_warm_held_payload_bytes", "gauge"),
    ("racer_warm_held_segments", "gauge"),
];
const DISK_CLASS_METRICS: [&str; 7] = [
    "racer_disk_class_publications_total",
    "racer_disk_class_published_payload_bytes_total",
    "racer_disk_class_index_evicted_pages_total",
    "racer_disk_class_index_evicted_payload_bytes_total",
    "racer_disk_class_segment_evicted_pages_total",
    "racer_disk_class_segment_evicted_payload_bytes_total",
    "racer_disk_class_read_payload_bytes_total",
];

impl Default for Metrics {
    fn default() -> Self {
        Self::for_workers(1)
            .expect("one metrics worker")
            .pop()
            .unwrap()
    }
}
// Declaration order is the counter index; names are the exported wire contract.
metrics! { Event, EVENTS, EVENT_COUNT;
            Self::WarmAccepted => "racer_warm_accepted_total",
            Self::WarmRefused => "racer_warm_refused_total",
            Self::WarmCompleted => "racer_warm_completed_total",
            Self::WarmFailed => "racer_warm_failed_total",
            Self::WarmWaitStarted => "racer_warm_wait_started_total",
            Self::WarmWaitCompleted => "racer_warm_wait_completed_total",
            Self::WarmWaitFallback => "racer_warm_wait_fallback_total",
            Self::WarmWaitFailed => "racer_warm_wait_failed_total",
            Self::WarmRemoteHoldAccepted => "racer_warm_remote_hold_accepted_total",
            Self::WarmRemoteHoldRefused => "racer_warm_remote_hold_refused_total",
            Self::WarmRemoteFillCompleted => "racer_warm_remote_fill_completed_total",
            Self::WarmRemoteFillRetried => "racer_warm_remote_fill_retried_total",
            Self::WarmRemoteFillFailed => "racer_warm_remote_fill_failed_total",
            Self::WarmRemoteReleaseSent => "racer_warm_remote_release_sent_total",
            Self::WarmRemoteReleaseFailed => "racer_warm_remote_release_failed_total",
            Self::WarmOwnerHoldAccepted => "racer_warm_owner_hold_accepted_total",
            Self::WarmOwnerHoldRefused => "racer_warm_owner_hold_refused_total",
            Self::WarmOwnerFillCompleted => "racer_warm_owner_fill_completed_total",
            Self::WarmOwnerFillFailed => "racer_warm_owner_fill_failed_total",
            Self::WarmOwnerReleased => "racer_warm_owner_released_total",
            Self::PeerReceiveAdmitted => "racer_peer_receive_admitted_total",
            Self::PeerReceiveFull => "racer_peer_receive_full_total",
            Self::PeerReceiveTimeout => "racer_peer_receive_timeout_total",
            Self::PeerReceiveCancelled => "racer_peer_receive_canceled_total",
            Self::PeerReceiveWaitNs => "racer_peer_receive_wait_ns_total",
            Self::PageHedgeStarted => "racer_page_hedges_started_total",
            Self::PageHedgeWon => "racer_page_hedges_won_total",
            Self::PageHedgeSuppressed => "racer_page_hedges_suppressed_total",
            Self::PageHedgeDuplicateBytes => "racer_page_hedge_duplicate_reserved_bytes_total",
            Self::PeerAdmissionAccepted => "racer_peer_admission_accepted_total",
            Self::PeerAdmissionRejected => "racer_peer_admission_rejected_total",
            Self::PeerCircuitRejected => "racer_peer_circuit_rejected_total",
            Self::PeerProbe => "racer_peer_probes_total",
            Self::PeerVerified => "racer_peer_verified_responses_total",
            Self::PeerLinkFailure => "racer_peer_link_failures_total",
            Self::PeerLocalPressure => "racer_peer_local_pressure_total",
            Self::Request => "racer_requests_total",
            Self::RequestError => "racer_request_errors_total",
            Self::MemoryHit => "racer_memory_hits_total",
            Self::DiskHit => "racer_disk_hits_total",
            Self::PeerHit => "racer_peer_hits_total",
            Self::OriginFill => "racer_origin_fills_total",
            Self::DirtyDiscard => "racer_dirty_discards_total",
            Self::DiskPublication => "racer_disk_publications_total",
            Self::PersistZeroCopy => "racer_persist_zero_copy_total",
            Self::PersistCopyFallback => "racer_persist_copy_fallback_total",
            Self::Overload => "racer_overloads_total",
            Self::CorruptMiss => "racer_corrupt_misses_total",
            Self::DiagnosticAccepted => "racer_diagnostic_accepted_total",
            Self::DiagnosticHealth => "racer_diagnostic_health_total",
            Self::DiagnosticReady => "racer_diagnostic_ready_total",
            Self::DiagnosticMetrics => "racer_diagnostic_metrics_total",
            Self::DiagnosticRejected => "racer_diagnostic_rejected_total",
            Self::DiagnosticIoError => "racer_diagnostic_io_errors_total",
            Self::DiagnosticTimeout => "racer_diagnostic_timeouts_total",
            Self::PageDecrypt => "racer_page_decrypts_total",
            Self::PeerBootstrap => "racer_peer_bootstraps_total",
            Self::DiagnosticFailures => "racer_diagnostic_failures_total",
            Self::DeliveryPipeDrain => "racer_delivery_pipe_drains_total",
            Self::DeliveryDirectBytes => "racer_delivery_direct_bytes_total",
            Self::CryptoEncryptStarted => "racer_crypto_encrypt_started_total",
            Self::CryptoEncryptSuccess => "racer_crypto_encrypt_success_total",
            Self::CryptoEncryptFailure => "racer_crypto_encrypt_failure_total",
            Self::CryptoEncryptBytes => "racer_crypto_encrypt_success_bytes_total",
            Self::CryptoEncryptExecutionCount => "racer_crypto_encrypt_execution_nanoseconds_count",
            Self::CryptoEncryptExecutionNs => "racer_crypto_encrypt_execution_nanoseconds_sum",
            Self::CryptoEncryptQueueCount => "racer_crypto_encrypt_queue_nanoseconds_count",
            Self::CryptoEncryptQueueNs => "racer_crypto_encrypt_queue_nanoseconds_sum",
            Self::CryptoDecryptStarted => "racer_crypto_decrypt_started_total",
            Self::CryptoDecryptSuccess => "racer_crypto_decrypt_success_total",
            Self::CryptoDecryptFailure => "racer_crypto_decrypt_failure_total",
            Self::CryptoDecryptBytes => "racer_crypto_decrypt_success_bytes_total",
            Self::CryptoDecryptExecutionCount => "racer_crypto_decrypt_execution_nanoseconds_count",
            Self::CryptoDecryptExecutionNs => "racer_crypto_decrypt_execution_nanoseconds_sum",
            Self::CryptoDecryptQueueCount => "racer_crypto_decrypt_queue_nanoseconds_count",
            Self::CryptoDecryptQueueNs => "racer_crypto_decrypt_queue_nanoseconds_sum",
            Self::PlaintextLookupHit => "racer_plaintext_lookup_hits_total",
            Self::PlaintextLookupMiss => "racer_plaintext_lookup_misses_total",
            Self::PlaintextLookupError => "racer_plaintext_lookup_errors_total",
            Self::CiphertextLookupHit => "racer_ciphertext_lookup_hits_total",
            Self::CiphertextLookupMiss => "racer_ciphertext_lookup_misses_total",
            Self::CiphertextLookupError => "racer_ciphertext_lookup_errors_total",
            Self::PendingLookupHit => "racer_pending_lookup_hits_total",
            Self::PendingLookupMiss => "racer_pending_lookup_misses_total",
            Self::PendingLookupError => "racer_pending_lookup_errors_total",
            Self::DiskIndexLookupHit => "racer_disk_index_lookup_hits_total",
            Self::DiskIndexLookupMiss => "racer_disk_index_lookup_misses_total",
            Self::DiskIndexLookupError => "racer_disk_index_lookup_errors_total",
            Self::PeerPageCheckoutCount => "racer_peer_page_checkout_nanoseconds_count",
            Self::PeerPageCheckoutNs => "racer_peer_page_checkout_nanoseconds_sum",
            Self::PeerPageAuthCount => "racer_peer_page_auth_nanoseconds_count",
            Self::PeerPageAuthNs => "racer_peer_page_auth_nanoseconds_sum",
            Self::PeerPageHeadCount => "racer_peer_page_head_nanoseconds_count",
            Self::PeerPageHeadNs => "racer_peer_page_head_nanoseconds_sum",
            Self::PeerPageBodyCount => "racer_peer_page_body_nanoseconds_count",
            Self::PeerPageBodyNs => "racer_peer_page_body_nanoseconds_sum",
            Self::PeerPageCensored => "racer_peer_page_censored_total",
            Self::CryptoDecryptCrcRejected => "racer_crypto_decrypt_crc_rejected_total",
            Self::CryptoDecryptAeadRejected => "racer_crypto_decrypt_aead_rejected_total",
            Self::FillDecryptDiskCorrupt => "racer_fill_decrypt_disk_corrupt_total",
            Self::FillDecryptRetainedCorrupt => "racer_fill_decrypt_retained_corrupt_total",
            Self::FillDecryptPeerCorrupt => "racer_fill_decrypt_peer_corrupt_total",
            Self::OpaqueRelayBodyCompleted => "racer_opaque_relay_body_completed_total",
            Self::OpaqueRelayBodyBytes => "racer_opaque_relay_body_completed_bytes_total",
            Self::OpaqueRelayBodyFailed => "racer_opaque_relay_body_failed_total",
            Self::CryptoDecryptFailureCancelled => "racer_crypto_decrypt_failure_cancelled_total",
            Self::CryptoDecryptFailureDeadlineExceeded => "racer_crypto_decrypt_failure_deadline_exceeded_total",
            Self::CryptoDecryptFailureMissingKey => "racer_crypto_decrypt_failure_missing_key_total",
            Self::CryptoDecryptFailureInvalidConfiguration => "racer_crypto_decrypt_failure_invalid_configuration_total",
            Self::CryptoDecryptFailureInvalidRequest => "racer_crypto_decrypt_failure_invalid_request_total",
            Self::CryptoDecryptFailureOverloaded => "racer_crypto_decrypt_failure_overloaded_total",
            Self::CryptoDecryptFailureCorruptCrc => "racer_crypto_decrypt_failure_corrupt_crc_total",
            Self::CryptoDecryptFailureCorruptAead => "racer_crypto_decrypt_failure_corrupt_aead_total",
            Self::CryptoDecryptFailureCorruptUnclassified => "racer_crypto_decrypt_failure_corrupt_unclassified_total",
            Self::CryptoDecryptFailureOther => "racer_crypto_decrypt_failure_other_total",
            Self::MemoryPressureTransitions => "racer_memory_pressure_transitions_total",
            Self::AllocatorTrims => "racer_allocator_trims_total",
            Self::RdmaPagesSent => "racer_rdma_pages_sent_total",
            Self::RdmaSentBytes => "racer_rdma_sent_bytes_total",
            Self::RdmaPagesReceived => "racer_rdma_pages_received_total",
            Self::RdmaReceivedBytes => "racer_rdma_received_bytes_total",
            Self::RdmaPagesLanded => "racer_rdma_pages_landed_total",
            Self::RdmaHttpFallback => "racer_rdma_http_fallback_total",
            Self::RdmaGrantCount => "racer_rdma_grant_nanoseconds_count",
            Self::RdmaGrantNs => "racer_rdma_grant_nanoseconds_sum",
            Self::RdmaHeadCount => "racer_rdma_head_nanoseconds_count",
            Self::RdmaHeadNs => "racer_rdma_head_nanoseconds_sum",
            Self::RdmaArrivalCount => "racer_rdma_arrival_nanoseconds_count",
            Self::RdmaArrivalNs => "racer_rdma_arrival_nanoseconds_sum",
            Self::RdmaSendCount => "racer_rdma_send_nanoseconds_count",
            Self::RdmaSendNs => "racer_rdma_send_nanoseconds_sum",
            Self::RdmaServeCount => "racer_rdma_serve_nanoseconds_count",
            Self::RdmaServeNs => "racer_rdma_serve_nanoseconds_sum",
            Self::FillDriverStartCount => "racer_fill_driver_start_nanoseconds_count",
            Self::FillDriverStartNs => "racer_fill_driver_start_nanoseconds_sum",
            Self::FillDriverCount => "racer_fill_driver_nanoseconds_count",
            Self::FillDriverNs => "racer_fill_driver_nanoseconds_sum",
            Self::FillCandidatesCount => "racer_fill_candidates_nanoseconds_count",
            Self::FillCandidatesNs => "racer_fill_candidates_nanoseconds_sum",
            Self::FillPlaintextReclaimCount => "racer_fill_plaintext_reclaim_nanoseconds_count",
            Self::FillPlaintextReclaimNs => "racer_fill_plaintext_reclaim_nanoseconds_sum",
            Self::FillPeerResponseCount => "racer_fill_peer_response_nanoseconds_count",
            Self::FillPeerResponseNs => "racer_fill_peer_response_nanoseconds_sum",
            Self::FillPeerDecryptCount => "racer_fill_peer_decrypt_nanoseconds_count",
            Self::FillPeerDecryptNs => "racer_fill_peer_decrypt_nanoseconds_sum",
            Self::FillHedgeCount => "racer_fill_hedge_nanoseconds_count",
            Self::FillHedgeNs => "racer_fill_hedge_nanoseconds_sum",
            Self::FillPublishCount => "racer_fill_publish_nanoseconds_count",
            Self::FillPublishNs => "racer_fill_publish_nanoseconds_sum",
            Self::FillDecryptCount => "racer_fill_decrypt_nanoseconds_count",
            Self::FillDecryptNs => "racer_fill_decrypt_nanoseconds_sum",
            Self::FillDiskReadCount => "racer_fill_disk_read_nanoseconds_count",
            Self::FillDiskReadNs => "racer_fill_disk_read_nanoseconds_sum",
            Self::FillOriginCount => "racer_fill_origin_nanoseconds_count",
            Self::FillOriginNs => "racer_fill_origin_nanoseconds_sum",
            Self::FillEncryptCount => "racer_fill_encrypt_nanoseconds_count",
            Self::FillEncryptNs => "racer_fill_encrypt_nanoseconds_sum",
            Self::FillChecksumCount => "racer_fill_checksum_nanoseconds_count",
            Self::FillChecksumNs => "racer_fill_checksum_nanoseconds_sum",
            Self::DiagnosticHardware => "racer_diagnostic_hardware_total",
            Self::GpuDirectDiskRead => "racer_gpu_direct_disk_reads_total",
            Self::GpuDirectDiskReadBytes => "racer_gpu_direct_disk_read_bytes_total",
            Self::SuccessorHint => "racer_successor_hints_total",
            Self::SuccessorDeferral => "racer_successor_deferrals_total",
            Self::CopyJoinDeclined => "racer_copy_join_declined_total",
            Self::OrphanNotice => "racer_orphan_notices_total",
            Self::OrphanOfferSent => "racer_orphan_offers_sent_total",
            Self::OrphanOfferDelivered => "racer_orphan_offers_delivered_total",
            Self::OrphanOfferFailed => "racer_orphan_offers_failed_total",
            Self::OrphanOfferNoted => "racer_orphan_offers_noted_total",
            Self::SilencePingSent => "racer_silence_pings_sent_total",
            Self::SilencePingAnswered => "racer_silence_pings_answered_total",
            Self::SilencePeerSilent => "racer_silence_peers_silent_total",
            Self::PassthroughLocalOrigin => "racer_passthrough_local_origin_fills_total",
}
metrics! { Gauge, GAUGES, GAUGE_COUNT;
            Self::WarmTrackedJobs => "racer_warm_tracked_jobs",
            Self::WarmWaiters => "racer_warm_waiters",
            Self::PeerReceiveActive => "racer_peer_receive_active",
            Self::PeerReceiveQueued => "racer_peer_receive_queued",
            Self::PeerAdmissionLimit => "racer_peer_admission_limit",
            Self::PeerExchanges => "racer_peer_exchanges_active",
            Self::DiagnosticConnections => "racer_diagnostic_connections",
            Self::ActiveRequests => "racer_active_requests",
            Self::ActiveFills => "racer_active_fills",
            Self::KeyringGeneration => "racer_keyring_generation",
            Self::IdentityExpiresAtSeconds => "racer_identity_expires_at_seconds",
            Self::PendingDiskWrites => "racer_pending_disk_writes",
            Self::ActiveDeliveries => "racer_active_deliveries",
            Self::EffectivePayloadBytes => "racer_effective_payload_bytes",
            Self::SegmentTailBytes => "racer_segment_tail_bytes",
            Self::DiskPageEntries => "racer_disk_page_index_capacity",
            Self::CheckpointSequence => "racer_checkpoint_sequence",
            Self::MemoryPressureLevel => "racer_memory_pressure_level",
}
/// One nonempty intermediate HTTP relay_body call, after sending its response head.
/// Success credits the entire ciphertext body only after both HTTP finish checks.
/// Error or abandonment credits one failure and no bytes, even after partial writes.
/// Head failures, empty bodies, materialized/native paths, and pre-body retries are
/// excluded. Splice-to-copy fallback is still one attempt. These are per-hop transfer
/// counts, not unique pages, endpoint AEAD acceptance, or confirmed client delivery.
pub(crate) struct OpaqueRelayBody<'a> {
    metrics: &'a Metrics,
    bytes: usize,
    completed: bool,
}
impl OpaqueRelayBody<'_> {
    pub(crate) fn complete(&mut self) {
        self.completed = true;
    }
}
impl Drop for OpaqueRelayBody<'_> {
    fn drop(&mut self) {
        if self.completed {
            self.metrics
                .record(Event::OpaqueRelayBodyBytes, self.bytes as u64);
            self.metrics.record(Event::OpaqueRelayBodyCompleted, 1);
        } else {
            self.metrics.record(Event::OpaqueRelayBodyFailed, 1);
        }
    }
}
/// One complete client head through final delivery, including cancellation/drop.
pub(crate) struct RequestMetrics {
    _active: ::telemetry::Lease,
    metrics: Metrics,
    succeeded: bool,
    overloaded: bool,
}
impl RequestMetrics {
    pub(crate) fn success(&mut self) {
        self.succeeded = true;
    }
    pub(crate) fn fail(&mut self, error: crate::error::Error) {
        if error == crate::error::Error::Overloaded && !self.overloaded {
            self.metrics.record(Event::Overload, 1);
            self.overloaded = true;
        }
    }
}
impl Drop for RequestMetrics {
    fn drop(&mut self) {
        if !self.succeeded {
            self.metrics.record(Event::RequestError, 1);
        }
    }
}
impl Metrics {
    pub(crate) fn observe_held(&self, snapshot: crate::store::catalog::HeldSnapshot) {
        self.held.replace(self.shard, snapshot);
    }

    fn write_held(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = [0u64; 3];
        for snapshot in self.held.snapshots() {
            for (total, value) in
                totals
                    .iter_mut()
                    .zip([snapshot.pages, snapshot.payload_bytes, snapshot.segments])
            {
                *total = total.saturating_add(value);
            }
        }
        ::telemetry::metrics::Exposition::new(HELD_METRICS).write(out, totals)
    }

    /// Record a HEAD admission, job outcome, or GET wait transition once.
    /// Accepted includes coalesced admissions; completed/failed count job outcomes.
    /// WaitStarted counts registered waits, not registry misses or repeated polls.
    /// WaitCompleted means publication; fallback means normal Fill may proceed.
    /// WaitFailed includes cancellation, deadline, and abandoned registered waits.
    pub fn warm_event(&self, event: WarmEvent) {
        self.record(event.event(), 1);
    }

    /// Replace node-wide registry counts, never sum the same registry per worker.
    /// Tracked jobs include completed versions retained until expiry or removal.
    pub(crate) fn observe_warm(&self, (jobs, waiters): (usize, usize)) {
        self.set_gauge(Gauge::WarmTrackedJobs, jobs as u64);
        self.set_gauge(Gauge::WarmWaiters, waiters as u64);
    }

    pub(crate) fn observe_disks(&self, samples: Vec<DiskUsage>) {
        self.disks.replace(self.shard, samples);
    }

    fn write_disks(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = std::collections::BTreeMap::<Arc<str>, (u64, u64)>::new();
        for shard in self.disks.snapshots() {
            for sample in shard {
                let total = totals.entry(sample.disk).or_default();
                total.0 = total.0.saturating_add(sample.size);
                total.1 = total.1.saturating_add(sample.used);
            }
        }
        if !totals.is_empty() {
            for metric in ["racer_disk_size_bytes", "racer_disk_used_bytes"] {
                writeln!(out, "# TYPE {metric} gauge")?;
            }
        }
        for (disk, (size, used)) in totals {
            for (metric, value) in [
                ("racer_disk_size_bytes", size),
                ("racer_disk_used_bytes", used),
            ] {
                write!(out, "{metric}{{disk=\"")?;
                write_disk_label(out, &disk)?;
                writeln!(out, "\"}} {value}")?;
            }
        }
        Ok(())
    }

    /// Replace this worker's per-cache disk samples. Only published caches are
    /// sampled, so a removed cache's series disappear on the next tick.
    pub(crate) fn observe_cache_disks(&self, samples: Vec<CacheDiskUsage>) {
        self.cache_disks.replace(self.shard, samples);
    }

    fn write_cache_disks(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = std::collections::BTreeMap::<Arc<str>, (u64, u64)>::new();
        for shard in self.cache_disks.snapshots() {
            for sample in shard {
                let total = totals.entry(sample.cache).or_default();
                total.0 = total.0.saturating_add(sample.resident);
                total.1 = total.1.saturating_add(sample.share);
            }
        }
        if !totals.is_empty() {
            for metric in CACHE_DISK_METRICS {
                writeln!(out, "# TYPE {metric} gauge")?;
            }
        }
        for (cache, (resident, share)) in totals {
            for (metric, value) in CACHE_DISK_METRICS.into_iter().zip([resident, share]) {
                write!(out, "{metric}{{cache=\"")?;
                write_disk_label(out, &cache)?;
                writeln!(out, "\"}} {value}")?;
            }
        }
        Ok(())
    }

    /// Sample on the owning worker's health tick, never on a request hot path.
    /// Diagnostics aggregate fixed-size copies without accessing worker-local Rc.
    pub(crate) fn observe_retention(&self, snapshot: crate::retention::Snapshot) {
        self.retention.replace(self.shard, snapshot);
    }

    fn write_retention(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = [0u64; RETENTION_METRICS.len()];
        let mut classes = [[0u64; DISK_CLASS_METRICS.len()]; 2];
        for snapshot in self.retention.snapshots() {
            for (total, value) in totals.iter_mut().zip([
                snapshot.observations,
                snapshot.qualified,
                snapshot.persistence_attempts,
                snapshot.persistence_accepted,
                snapshot.filter_set_bits as u64,
                snapshot.filter_bits as u64,
                snapshot.heat_entries as u64,
                snapshot.pending_payload_bytes,
                snapshot.indexed_payload_bytes,
                snapshot.share_rejected,
            ]) {
                *total = total.saturating_add(value);
            }
            for (totals, class) in classes.iter_mut().zip(snapshot.disk) {
                for (total, value) in totals.iter_mut().zip([
                    class.published_pages,
                    class.published_payload_bytes,
                    class.index_evicted_pages,
                    class.index_evicted_payload_bytes,
                    class.segment_evicted_pages,
                    class.segment_evicted_payload_bytes,
                    class.read_payload_bytes,
                ]) {
                    *total = total.saturating_add(value);
                }
            }
        }
        ::telemetry::metrics::Exposition::new(RETENTION_METRICS).write(out, totals)?;
        ::telemetry::metrics::Exposition::new(DISK_CLASS_METRICS.map(|name| (name, "counter")))
            .write_labeled(out, "classification", ["nonowned", "owned"], classes)
    }

    pub(crate) fn opaque_relay_body(&self, bytes: usize) -> Option<OpaqueRelayBody<'_>> {
        (bytes != 0).then(|| OpaqueRelayBody {
            metrics: self,
            bytes,
            completed: false,
        })
    }
    /// Replace this worker's slab staging pool statistics from its health tick.
    pub(crate) fn observe_staging_pool(&self, worker: WorkerId, stats: flow_control::PoolStats) {
        self.memory
            .staging
            .replace(self.shard, Some((worker, stats)));
    }

    /// Replace the node-wide process and cgroup memory sample.
    pub(crate) fn observe_memory(&self, memory: ProcessMemory) {
        self.memory.process.replace(0, memory);
    }

    fn write_memory(&self, out: &mut impl Write) -> std::fmt::Result {
        let Some(memory) = self.memory.process.snapshots().next() else {
            return Ok(());
        };
        for (name, value) in [
            ("racer_process_resident_bytes", memory.resident),
            ("racer_cgroup_memory_current_bytes", memory.cgroup_current),
            ("racer_cgroup_memory_limit_bytes", memory.cgroup_limit),
        ] {
            if let Some(value) = value {
                writeln!(out, "# TYPE {name} gauge\n{name} {value}")?;
            }
        }
        if let Some(avg10) = memory.pressure_some_avg10.filter(|v| v.is_finite()) {
            let name = "racer_cgroup_memory_pressure_some_avg10";
            writeln!(out, "# TYPE {name} gauge\n{name} {avg10}")?;
        }
        Ok(())
    }

    /// Write node-wide admission for every class, plus recycler and staging pools.
    fn write_worker_pools(&self, out: &mut impl Write) -> std::fmt::Result {
        let installed: Vec<_> = self
            .admission
            .iter()
            .filter_map(|shard| shard.get())
            .collect();
        if !installed.is_empty() {
            for (name, used) in [
                ("racer_admission_used", true),
                ("racer_admission_limit", false),
            ] {
                writeln!(out, "# TYPE {name} gauge")?;
                for (class, label) in ADMISSION_CLASSES {
                    let total = installed.iter().fold(0u64, |total, (_, usage)| {
                        let value = if used {
                            usage.used(class)
                        } else {
                            usage.limit(class)
                        };
                        total.saturating_add(value as u64)
                    });
                    writeln!(out, "{name}{{class=\"{label}\"}} {total}")?;
                }
            }
        }
        let recycler: Vec<_> = installed
            .iter()
            .map(|(worker, usage)| (*worker, usage.buffer_pool_stats()))
            .collect();
        write_pools(out, "recycler", &recycler)?;
        let staging: Vec<_> = self.memory.staging.snapshots().flatten().collect();
        write_pools(out, "staging_pool", &staging)
    }

    /// Install once during worker assembly, not on the admission hot path.
    pub(crate) fn observe_admission(
        &self,
        worker: WorkerId,
        usage: flow_control::SharedQuotas<AdmissionPolicy>,
    ) -> Result<()> {
        self.admission[self.shard]
            .set((worker, usage))
            .map_err(|_| crate::error::Error::InvalidConfiguration)
    }
    /// Observe only an executed synchronous presence probe, preserving its result.
    pub(crate) fn lookup<T>(
        &self,
        tier: LookupTier,
        result: Result<Option<T>>,
    ) -> Result<Option<T>> {
        let events = match tier {
            LookupTier::Plaintext => [
                Event::PlaintextLookupHit,
                Event::PlaintextLookupMiss,
                Event::PlaintextLookupError,
            ],
            LookupTier::Ciphertext => [
                Event::CiphertextLookupHit,
                Event::CiphertextLookupMiss,
                Event::CiphertextLookupError,
            ],
            LookupTier::Pending => [
                Event::PendingLookupHit,
                Event::PendingLookupMiss,
                Event::PendingLookupError,
            ],
            LookupTier::DiskIndex => [
                Event::DiskIndexLookupHit,
                Event::DiskIndexLookupMiss,
                Event::DiskIndexLookupError,
            ],
        };
        let outcome = match &result {
            Ok(Some(_)) => 0,
            Ok(None) => 1,
            Err(_) => 2,
        };
        self.record(events[outcome], 1);
        result
    }
    /// Allocate a fixed registry at startup, with one event writer per worker.
    /// No registration, locking, or registry reference-count changes on record.
    pub(crate) fn for_workers(count: usize) -> Result<Vec<Self>> {
        if count == 0 {
            return Err(crate::error::Error::InvalidConfiguration);
        }
        let admission: Arc<[_]> = (0..count).map(|_| OnceLock::new()).collect();
        let retention = ::telemetry::SnapshotShards::new(count);
        let held = ::telemetry::SnapshotShards::new(count);
        let disks = ::telemetry::SnapshotShards::new(count);
        let memory = Arc::new(MemoryShards {
            staging: ::telemetry::SnapshotShards::new(count),
            process: ::telemetry::SnapshotShards::new(1),
        });
        let cache_disks = ::telemetry::SnapshotShards::new(count);
        let gpu = Arc::new(crate::gpu_telemetry::GpuTelemetry::default());
        Ok(::telemetry::Metrics::shards(count)
            .into_iter()
            .enumerate()
            .map(|(shard, core)| Self {
                core,
                admission: admission.clone(),
                retention: retention.clone(),
                held: held.clone(),
                disks: disks.clone(),
                memory: memory.clone(),
                cache_disks: cache_disks.clone(),
                gpu: gpu.clone(),
                shard,
            })
            .collect())
    }

    pub(crate) fn request(&self) -> Result<RequestMetrics> {
        let active = self.lease(Gauge::ActiveRequests)?;
        self.record(Event::Request, 1);
        Ok(RequestMetrics {
            _active: active,
            metrics: self.clone(),
            succeeded: false,
            overloaded: false,
        })
    }
    /// Saturate instead of wrapping a long-lived Prometheus counter.
    pub fn record(&self, event: Event, amount: u64) {
        self.core.add(event, amount);
    }
    /// Add one observation of the time since `start` to a count and nanosecond sum pair.
    pub fn record_duration(&self, count: Event, sum: Event, start: std::time::Instant) {
        let elapsed = uring_runtime::environment::now().saturating_duration_since(start);
        self.record(sum, u64::try_from(elapsed.as_nanos()).unwrap_or(u64::MAX));
        self.record(count, 1);
    }
    /// GPU metric sources shared by every worker.
    pub fn gpu(&self) -> &crate::gpu_telemetry::GpuTelemetry {
        &self.gpu
    }
    /// A shared handle to the GPU metric sources, for the lazy GPU start.
    pub fn gpu_shared(&self) -> Arc<crate::gpu_telemetry::GpuTelemetry> {
        self.gpu.clone()
    }
    pub(crate) fn writer_shard(&self) -> usize {
        self.shard
    }
    #[cfg(test)]
    pub(crate) fn writer_count(&self, event: Event) -> u64 {
        self.core.count_on(self.shard, event)
    }
    pub(crate) fn record_on(&self, shard: usize, event: Event, amount: u64) {
        self.core.add_on(shard, event, amount);
    }
    pub fn count(&self, event: Event) -> u64 {
        self.core.count(event)
    }
    pub fn gauge(&self, gauge: Gauge) -> u64 {
        self.core.gauge(gauge)
    }
    pub(crate) fn set_gauge(&self, gauge: Gauge, value: u64) {
        self.core.set(gauge, value);
    }
    pub(crate) fn add_gauge(&self, gauge: Gauge, value: u64) {
        self.core.increase(gauge, value);
    }
    /// Keep with the actual resource, including through a submitted I/O fence.
    pub fn lease(&self, gauge: Gauge) -> Result<::telemetry::Lease> {
        self.core
            .lease(gauge)
            .ok_or(crate::error::Error::Overloaded)
    }
    /// The destination is bounded by the diagnostic server; no intermediate String.
    /// Relaxed per-series observations are not a coherent snapshot of all workers.
    pub fn write_prometheus(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        self.core.write_prometheus(out)?;
        self.write_retention(out)?;
        self.write_held(out)?;
        self.write_disks(out)?;
        self.write_memory(out)?;
        self.write_worker_pools(out)?;
        self.write_cache_disks(out)?;
        self.gpu.write(out)?;
        // Quota labels use runtime worker IDs. Read the authority's actual charge,
        // including pooled ciphertext capacity, without sampling on worker polls.
        // A stalled worker therefore remains observable from another worker.
        const QUOTAS: ::telemetry::metrics::LabeledGauges<4> =
            ::telemetry::metrics::LabeledGauges::new(
                [
                    "racer_worker_relay_used",
                    "racer_worker_relay_limit",
                    "racer_worker_ciphertext_used_bytes",
                    "racer_worker_ciphertext_limit_bytes",
                ],
                "worker",
            );
        if self.admission.iter().any(|s| s.get().is_some()) {
            QUOTAS.write_types(out)?;
            for shard in self.admission.iter() {
                if let Some((worker, usage)) = shard.get() {
                    let (relay_used, relay_limit) = (
                        usage.used(crate::admission::ResourceClass::Relay),
                        usage.limit(crate::admission::ResourceClass::Relay),
                    );
                    let (ciphertext_used, ciphertext_limit) = (
                        usage.used(crate::admission::ResourceClass::Ciphertext),
                        usage.limit(crate::admission::ResourceClass::Ciphertext),
                    );
                    QUOTAS.write_sample(
                        out,
                        u64::from(worker.0),
                        [relay_used, relay_limit, ciphertext_used, ciphertext_limit]
                            .map(|value| value as u64),
                    )?;
                }
            }
        }
        Ok(())
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    pub(crate) mod http {
        use super::*;
        use crate::model::RequestId;
        use std::io::Read;
        use std::io::Write as IoWrite;
        use std::net::TcpStream;
        use std::task::Context;

        fn scope() -> RequestScope {
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(15)).unwrap()
        }
        fn setup() -> (
            Rc<flow_control::Quotas<AdmissionPolicy>>,
            Rc<Reactor>,
            Rc<DiagnosticIo>,
        ) {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
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
                let mut bytes = [0; 512];
                match socket.read(&mut bytes) {
                    Ok(0) => break,
                    Ok(count) => response.extend_from_slice(&bytes[..count]),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                    Err(error) => panic!("{error}"),
                }
                assert!(response.len() <= MAX_RESPONSE_BYTES);
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
            let length: usize = head
                .lines()
                .find_map(|line| line.strip_prefix("Content-Length: "))
                .unwrap()
                .parse()
                .unwrap();
            assert_eq!(actual_body.len(), length);
            assert!(head.contains("Connection: close"));
            if let Some(body) = body {
                assert_eq!(actual_body, body);
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
            assert!(bytes.len() < MAX_RESPONSE_BYTES);
            let text = std::str::from_utf8(&bytes).unwrap().to_owned();
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
                errors: vec!["x".repeat(::telemetry::server::MAX_BODY_BYTES)],
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
                HardwareSession::start(job, ::telemetry::server::MAX_BODY_BYTES, test_charge())
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
        fn hardware_helper_panic_is_unavailable_and_releases_slot() {
            let telemetry = Telemetry::default();
            set_hardware(&telemetry, || panic!("synthetic discovery failure"));
            for _ in 0..2 {
                let text = diagnostic_response(&telemetry, "/debug/hardware");
                assert_response(
                    text.as_bytes(),
                    "503 Service Unavailable",
                    Some("unavailable\n"),
                );
            }
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
                HardwareSession::start(job, ::telemetry::server::MAX_BODY_BYTES, test_charge())
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
                HardwareSession::start(job, ::telemetry::server::MAX_BODY_BYTES, test_charge())
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
            let reactor = Rc::new(Reactor::new(admission.clone()));
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
            let reactor = Rc::new(Reactor::new(admission.clone()));
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
            let scope = RequestScope::new(
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
        fn send_crc_endpoint_is_empty_when_disabled_and_does_not_echo_headers() {
            let telemetry = Telemetry::default();
            let text = diagnostic_text(&telemetry, "/debug/send-crc");
            assert!(text.contains("sampled=0"));
            assert!(!text.contains("synthetic"));
            telemetry.send_crc.fill_test_ring();
            let text = diagnostic_text(&telemetry, "/debug/send-crc");
            assert_eq!(text.matches("status=computed").count(), 64);
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
                        heat_entries: usize::MAX,
                        pending_payload_bytes: u64::MAX,
                        indexed_payload_bytes: u64::MAX,
                        disk: [crate::retention::DiskClassSnapshot {
                            published_pages: u64::MAX,
                            published_payload_bytes: u64::MAX,
                            index_evicted_pages: u64::MAX,
                            index_evicted_payload_bytes: u64::MAX,
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
            let text = diagnostic_text(&telemetry, "/metrics");
            for name in [
                "racer_admission_used{class=\"ordinary_outbound_connection\"}",
                "racer_staging_pool_trimmed_total ",
                "racer_worker_staging_pool_bytes{worker=\"65535\"}",
                "racer_cgroup_memory_pressure_some_avg10 100\n",
            ] {
                assert!(text.contains(name), "{name}");
            }
            assert!(text.len() < MAX_RESPONSE_BYTES);
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
                6 * 64
            );
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
            let reactor = Rc::new(Reactor::new(admission.clone()));
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
            let reactor = Rc::new(Reactor::new(admission.clone()));
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
                let reactor = Rc::new(Reactor::new(admission.clone()));
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
                            server::BINARY_CONTROL_SLOTS * uring_runtime::reactor::SUBMISSION_BYTES
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

    pub(crate) mod health_tests {
        use super::*;
        use std::time::Duration;
        #[test]
        fn lifecycle_probes_keep_resource_policy_bodies_and_irreversible_drain() {
            let telemetry = Telemetry::default();
            let now = environment::now();
            let usable = Resources {
                workers_usable: true,
                storage_usable: true,
                listeners_usable: true,
                membership_usable: true,
                admission_usable: true,
                credentials_valid_until: Some(now + Duration::from_secs(60)),
                observed_until: Some(now + Duration::from_secs(60)),
            };
            let probe = |path, admission, expected, available: bool| {
                let mut body = String::new();
                let response = get(&telemetry, path, admission, &mut body).unwrap();
                assert_eq!(body, expected);
                assert_eq!(matches!(response, Response::Text), available);
                assert_eq!(matches!(response, Response::Unavailable), !available);
            };
            probe("/healthz", true, "ok\n", true);
            probe("/readyz", true, "not ready\n", false);
            assert_eq!(
                telemetry.health.transition(State::Ready),
                Err(Error::Unavailable)
            );
            telemetry.health.observe(usable).unwrap();
            telemetry.health.transition(State::Ready).unwrap();
            probe("/readyz", true, "ready\n", true);
            probe("/readyz", false, "not ready\n", false);
            telemetry
                .health
                .observe(Resources {
                    storage_usable: false,
                    ..usable
                })
                .unwrap();
            assert_eq!(telemetry.health.state(), Ok(State::Degraded));
            probe("/healthz", true, "ok\n", true);
            probe("/readyz", true, "not ready\n", false);
            telemetry.health.observe(usable).unwrap();
            assert_eq!(telemetry.health.state(), Ok(State::Ready));
            telemetry.health.transition(State::Draining).unwrap();
            assert_eq!(
                telemetry.health.transition(State::Ready),
                Err(Error::Unavailable)
            );
            probe("/healthz", true, "ok\n", true);
            probe("/readyz", true, "not ready\n", false);
            telemetry.health.transition(State::Stopped).unwrap();
            assert_eq!(
                telemetry.health.transition(State::Starting),
                Err(Error::Unavailable)
            );
            probe("/healthz", true, "not live\n", false);
            assert_eq!(telemetry.metrics.count(Event::DiagnosticHealth), 4);
            assert_eq!(telemetry.metrics.count(Event::DiagnosticReady), 5);
        }
        #[test]
        fn observation_and_credential_expiry_are_inclusive() {
            let now = uring_runtime::environment::now();
            for (observed, boundary) in [(5, 5), (20, 10)] {
                let health = Health::default();
                health
                    .observe(Resources {
                        workers_usable: true,
                        storage_usable: true,
                        listeners_usable: true,
                        membership_usable: true,
                        admission_usable: true,
                        credentials_valid_until: Some(now + Duration::from_secs(10)),
                        observed_until: Some(now + Duration::from_secs(observed)),
                    })
                    .unwrap();
                health.transition(State::Ready).unwrap();
                assert_eq!(
                    health
                        .state_at(now + Duration::from_secs(boundary) - Duration::from_nanos(1))
                        .unwrap(),
                    State::Ready
                );
                assert_eq!(
                    health
                        .state_at(now + Duration::from_secs(boundary))
                        .unwrap(),
                    State::Degraded
                );
            }
        }
    }

    pub(crate) mod send_crc_tests {
        use super::*;
        fn pair() -> Pair {
            Pair::parse("8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111")
                .unwrap()
        }
        #[test]
        fn send_crc_pair_and_shared_owner_bounds() {
            let clock = environment::SimulationClock::new(72);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            for value in [
                "",
                "x,y",
                "11111111-1111-4111-8111-111111111111,11111111-1111-4111-8111-111111111111",
                "x,y,z",
            ] {
                assert!(Pair::parse(value).is_err());
            }
            let p = pair();
            let samples = Samples::default();
            assert!(samples.begin(&p, &p.receiver, &p.sender).is_none());
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            drop(ticket);
            let other = samples.clone();
            let p2 = p.clone();
            assert!(
                std::thread::spawn(move || other.begin(&p2, &p2.sender, &p2.receiver).is_none())
                    .join()
                    .unwrap()
            );
            assert!(samples.0.counts().busy);
            drop(work);
            assert!(!samples.0.counts().busy);
            assert!(
                samples.begin(&p, &p.sender, &p.receiver).is_none(),
                "burst is one"
            );
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("send=abandoned status=unavailable"));
            clock.advance(Duration::from_secs(120));
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
        }
        #[test]
        fn send_crc_ineligible_pairs_do_not_start_the_sampling_window() {
            let clock = environment::SimulationClock::new(76);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            let p = pair();
            let samples = Samples::default();
            assert!(samples.begin(&p, &p.receiver, &p.sender).is_none());
            clock.advance(Duration::from_secs(120));
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            assert_eq!(ticket.0.sequence, 1);
            assert_eq!(samples.0.counts().eligible, 1);
            work.finish(Some(Error::Io));
            drop(work);
            assert!(!samples.0.counts().busy);
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("status=unavailable error=Some(Io)"));
        }
        #[test]
        fn send_crc_interval_accepts_exactly_500ms() {
            let clock = environment::SimulationClock::new(73);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            let p = pair();
            let samples = Samples::default();
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            drop(work);
            drop(ticket);
            clock.advance(Duration::from_millis(500) - Duration::from_nanos(1));
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
            clock.advance(Duration::from_nanos(1));
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            assert_eq!(ticket.0.sequence, 2);
            drop(work);
            drop(ticket);
            let state = samples.0.counts();
            assert_eq!((state.eligible, state.sampled, state.skipped), (3, 2, 1));
            assert!(!state.busy);
        }

        #[test]
        fn send_crc_ticket_and_snapshot_survive_retention_overwrite() {
            let clock = environment::SimulationClock::new(74);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            let p = pair();
            let samples = Samples::default();
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            let weak = Arc::downgrade(&ticket.0);
            work.finish(None);
            drop(work);
            let snapshot = samples.0.snapshot().0;
            for _ in 0..SEND_CRC_CAPACITY {
                clock.advance(Duration::from_millis(500));
                let (next, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
                next.finish(true);
                work.finish(None);
            }
            let mut before = String::new();
            samples.write(&mut before).unwrap();
            assert!(before.starts_with(
                "eligible=65 sampled=65 skipped=0 busy=0 retained=64 overwritten=1 capacity=64\nseq=2 "
            ));
            assert_eq!(before.lines().count(), SEND_CRC_CAPACITY + 1);
            ticket.finish(false);
            let mut after = String::new();
            samples.write(&mut after).unwrap();
            assert_eq!(
                before, after,
                "evicted Ticket cannot change retained records"
            );
            let (_, retained) = snapshot.iter_refs().next().unwrap();
            assert!(Arc::ptr_eq(retained, &ticket.0));
            assert_eq!(retained.data.lock().unwrap().send, "failed");
            drop(ticket);
            assert!(
                weak.upgrade().is_some(),
                "snapshot retains the evicted sample"
            );
            drop(snapshot);
            assert!(weak.upgrade().is_none(), "last handle releases the sample");
        }

        #[test]
        fn send_crc_ring_is_bounded_and_preserves_send_outcomes() {
            let clock = environment::SimulationClock::new(75);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            let p = pair();
            let samples = Samples::default();
            for i in 0..240 {
                let (ticket, mut work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
                ticket.finish(i % 2 == 0);
                work.facts = Some(crate::telemetry::test_aead_failure());
                work.finish(None);
                drop(work);
                drop(ticket);
                clock.advance(Duration::from_millis(500));
            }
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert_eq!(text.lines().count(), SEND_CRC_CAPACITY + 1);
            assert!(text.contains("overwritten=176 capacity=64\nseq=177 "));
            assert!(text.contains("send=completed"));
            assert!(text.contains("send=failed"));
            assert!(text.len() < crate::telemetry::MAX_RESPONSE_BYTES - 256);
        }
    }

    pub(crate) mod failures_tests {
        use super::*;

        #[test]
        fn decrypt_failure_classification_is_fixed_and_preserves_unknown_causes() {
            use crate::security::IntegrityRejection::{Aead, Crc};
            // Classification only: this does not exercise the allocator failure path.
            let cases = [
                (Error::Cancelled, None, "cancelled"),
                (Error::DeadlineExceeded, None, "deadline_exceeded"),
                (Error::MissingKey, None, "missing_key"),
                (Error::InvalidConfiguration, None, "invalid_configuration"),
                (Error::InvalidRequest, None, "invalid_request"),
                (Error::Overloaded, None, "overloaded"),
                (Error::CorruptRecord, Some(Crc), "corrupt_crc"),
                (Error::CorruptRecord, Some(Aead), "corrupt_aead"),
                (Error::CorruptRecord, None, "corrupt_unclassified"),
                (Error::Os(-123), None, "other"),
            ];
            for ((error, rejection, name), reason) in cases.into_iter().zip(DecryptReason::ALL) {
                assert_eq!(DecryptReason::new(error, rejection), reason);
                assert_eq!(reason.name(), name);
            }
            assert_eq!(
                DecryptReason::new(Error::Overloaded, Some(Aead)),
                DecryptReason::Overloaded
            );
        }

        #[test]
        fn decrypt_failure_ring_is_bounded_independent_redacted_and_nonconsuming() {
            let telemetry = Telemetry::default();
            let failures = &telemetry.failures;
            let observer = failures.observer(WorkerId(7));
            let mut record = DecryptFailure {
                id: crate::security::CryptoId {
                    worker: WorkerId(u16::MAX),
                    generation: u64::MAX,
                    sequence: u64::MAX,
                },
                error: Error::Os(i32::MIN),
                reason: DecryptReason::Other,
                request: Some(RequestId([255; 16])),
                page: Some(u64::MAX),
                engine_ms: Some(u64::MAX),
                reap_ms: u64::MAX,
                scope_failure: DecryptScopeFailure::Final(Error::DeadlineExceeded),
                queue_ns: Some(u64::MAX),
                execution_ns: Some(u64::MAX),
            };
            observer.record_decrypt(record);
            for _ in 0..4096 {
                observer.record(Failure::new(Stage::Admission, Error::Overloaded));
                observer.record_aead(record.id, test_aead_failure());
            }
            struct Output<'a> {
                failures: &'a Failures,
                text: String,
                fail: bool,
            }
            impl std::fmt::Write for Output<'_> {
                fn write_str(&mut self, text: &str) -> std::fmt::Result {
                    assert!(self.failures.7.try_snapshot().is_some());
                    if self.fail {
                        return Err(std::fmt::Error);
                    }
                    self.text.push_str(text);
                    Ok(())
                }
            }
            let mut out = Output {
                failures,
                text: String::new(),
                fail: true,
            };
            assert!(get(&telemetry, "/debug/decrypt-failures", true, &mut out).is_err());
            out.fail = false;
            get(&telemetry, "/debug/decrypt-failures", true, &mut out).unwrap();
            assert!(out.text.contains("coverage=executed_decrypt_failure_at_reap total=1 retained=1 overwritten=0 capacity=64"));
            assert!(out.text.contains("error=Os(-2147483648) reason=other"));
            assert!(
                out.text
                    .contains("scope_at_failure=final_check:DeadlineExceeded")
            );
            assert!(
                out.text
                    .contains("worker=65535 crypto=18446744073709551615:18446744073709551615")
            );
            for _ in 0..64 {
                record.error = Error::PublishedNotDurable(
                    crate::error::PublicationCause::UnsatisfiableRangeWithLength(u64::MAX),
                );
                record.reason = DecryptReason::CorruptUnclassified;
                record.scope_failure = DecryptScopeFailure::Final(record.error);
                observer.record_decrypt(record);
            }
            out.text.clear();
            get(&telemetry, "/debug/decrypt-failures", true, &mut out).unwrap();
            assert!(
                out.text
                    .contains("total=65 retained=64 overwritten=1 capacity=64")
            );
            assert_eq!(out.text.lines().count(), 65);
            assert!(out.text.contains("\nseq=2 "));
            assert!(out.text.len() + 512 < MAX_RESPONSE_BYTES);
            const { assert!(MAX_RESPONSE_BYTES <= 64 * 1024) };
            for forbidden in [
                " key=",
                " nonce=",
                " cache=",
                " etag=",
                " object=",
                "payload",
                "supplier=",
            ] {
                assert!(!out.text.contains(forbidden));
            }
            record.request = None;
            record.page = None;
            record.engine_ms = None;
            record.scope_failure = DecryptScopeFailure::Unknown;
            record.queue_ns = None;
            record.execution_ns = None;
            observer.record_decrypt(record);
            out.text.clear();
            failures.write_decrypt(&mut out).unwrap();
            let last = out.text.lines().last().unwrap();
            for unknown in [
                "request=unknown",
                "page=unknown",
                "engine_failure_observed_ms=unknown",
                "scope_at_failure=unknown",
                "scope_at_reap=unknown",
                "queue_ns=unknown",
                "execution_ns=unknown",
            ] {
                assert!(last.contains(unknown), "{last}");
            }
        }

        #[test]
        fn exact_output_and_formatting_outside_both_locks() {
            let failures = Failures::default();
            let observer = failures.observer(WorkerId(3));
            observer.record(Failure {
                unix_millis: 42,
                stage: Stage::Admission,
                error: Error::Overloaded,
                request: None,
                attempt: None,
                detail: Detail::None,
            });
            let id = crate::security::CryptoId {
                worker: WorkerId(3),
                generation: 4,
                sequence: 5,
            };
            let mut aead = test_aead_failure();
            aead.peer = None;
            aead.crc = None;
            observer.record_aead(id, aead);
            struct Unlocked<'a> {
                failures: &'a Failures,
                text: String,
            }
            impl std::fmt::Write for Unlocked<'_> {
                fn write_str(&mut self, value: &str) -> std::fmt::Result {
                    assert!(self.failures.0.try_snapshot().is_some());
                    assert!(self.failures.1.try_snapshot().is_some());
                    self.text.push_str(value);
                    Ok(())
                }
            }
            let mut out = Unlocked {
                failures: &failures,
                text: String::new(),
            };
            failures.write(&mut out).unwrap();
            assert_eq!(
                out.text,
                "total=1 retained=1 capacity=128\nsequence=1 worker=3 stage=Admission error=Overloaded request=none attempt=none unix_millis=42 detail=None\n"
            );
            out.text.clear();
            failures.write_aead(&mut out).unwrap();
            assert_eq!(
                out.text,
                format!(
                    "total=1 retained=1 overwritten=0 capacity=64\nseq=1 w=3 crypto=4:5 ms=18446744073709551615 request={} acquisition=none attempt=none supplier=none remote=none page={} number=18446744073709551615 key={} nonce={} lengths=4294967295/4294967295 aad={} crc=none\n",
                    "ff".repeat(16),
                    "ff".repeat(32),
                    "ff".repeat(16),
                    "ff".repeat(24),
                    "ff".repeat(32),
                )
            );
        }

        #[test]
        fn failed_formatting_does_not_consume_records() {
            struct Full;
            impl std::fmt::Write for Full {
                fn write_str(&mut self, _: &str) -> std::fmt::Result {
                    Err(std::fmt::Error)
                }
            }
            let failures = Failures::default();
            let observer = failures.observer(WorkerId(7));
            observer.record(Failure::new(Stage::ClientRead, Error::Io));
            observer.record_aead(
                crate::security::CryptoId {
                    worker: WorkerId(7),
                    generation: 1,
                    sequence: 2,
                },
                test_aead_failure(),
            );
            assert!(failures.write(&mut Full).is_err());
            assert!(failures.write_aead(&mut Full).is_err());
            let mut text = String::new();
            failures.write(&mut text).unwrap();
            assert!(text.starts_with("total=1 retained=1 capacity=128\n"));
            text.clear();
            failures.write_aead(&mut text).unwrap();
            assert!(text.starts_with("total=1 retained=1 overwritten=0 capacity=64\n"));
        }

        #[test]
        fn aead_ring_survives_admission_flood_and_wraps_independently() {
            let failures = Failures::default();
            let observer = failures.observer(WorkerId(3));
            let id = crate::security::CryptoId {
                worker: WorkerId(3),
                generation: 1,
                sequence: 1,
            };
            let record = test_aead_failure();
            observer.record_aead(id, record);
            for _ in 0..4096 {
                observer.record(Failure::new(Stage::Admission, Error::Overloaded));
            }
            let mut text = String::new();
            failures.write_aead(&mut text).unwrap();
            assert!(text.starts_with("total=1 retained=1 overwritten=0 capacity=64\nseq=1 "));
            for _ in 0..AEAD_CAPACITY {
                observer.record_aead(id, record);
            }
            text.clear();
            failures.write_aead(&mut text).unwrap();
            assert!(text.starts_with("total=65 retained=64 overwritten=1 capacity=64\nseq=2 "));
            assert_eq!(text.lines().count(), AEAD_CAPACITY + 1);
        }
        #[test]
        fn body_ring_worst_case_fits_existing_response_budget() {
            let failures = Failures::default();
            let observer = failures.observer(WorkerId(u16::MAX));
            let address = "[ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff%4294967295]:65535"
                .parse()
                .unwrap();
            let body = BodyProgress {
                received: u32::MAX,
                expected: u32::MAX,
                reads: u32::MAX,
                first: u64::MAX,
                last: u64::MAX,
                now: u64::MAX,
                original: u64::MAX,
                share: u64::MAX,
                signed: u64::MAX,
                remote: [b'f'; 36],
                tuple: Some((address, address)),
            };
            for _ in 0..FAILURE_CAPACITY {
                observer.record(Failure {
                    unix_millis: u64::MAX,
                    stage: Stage::PeerReceiveBody,
                    error: Error::InvalidConfiguration,
                    request: Some(RequestId([255; 16])),
                    attempt: Some(AttemptId([255; 16])),
                    detail: Detail::Body(body),
                });
            }
            let mut text = String::new();
            failures.write(&mut text).unwrap();
            assert_eq!(text.lines().count(), FAILURE_CAPACITY + 1);
            // The generic ring's saturation is tested in the core. Account for the
            // widest sequence and total here without exposing a mutable sequence API.
            let sequence_growth: usize = (1..=FAILURE_CAPACITY)
                .map(|sequence| 16 - format!("{sequence:x}").len())
                .sum();
            let worst_case_len =
                text.len() + sequence_growth + 20 - FAILURE_CAPACITY.to_string().len();
            assert!(
                worst_case_len <= crate::telemetry::MAX_RESPONSE_BYTES - 256,
                "{}",
                worst_case_len
            );
        }
        #[test]
        fn shared_workers_bounded_oldest_first_and_success_is_silent() {
            let failures = Failures::default();
            let observer = failures.observer(WorkerId(3));
            let scope = RequestScope::new(
                RequestId([0xab; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap();
            observer.result(Stage::ClientRead, &scope, Ok(())).unwrap();
            assert_eq!(failures.0.snapshot().total(), 0);
            std::thread::spawn(move || {
                for _ in 0..FAILURE_CAPACITY + 2 {
                    observer.record(
                        Failure::new(Stage::NextSlice, Error::Io)
                            .request(&scope)
                            .detail(Detail::Delivery {
                                sent: 16777216,
                                expected: 52157952,
                            }),
                    );
                }
            })
            .join()
            .unwrap();
            let mut text = String::new();
            failures.write(&mut text).unwrap();
            assert_eq!(text.lines().count(), FAILURE_CAPACITY + 1);
            assert!(
                text.lines()
                    .nth(1)
                    .unwrap()
                    .starts_with("sequence=3 worker=3 stage=NextSlice error=Io request=abab")
            );
            assert!(text.contains("sent: 16777216, expected: 52157952"));
        }
    }

    pub(crate) mod metrics_tests {
        use super::*;
        use crate::admission::ResourceClass;

        #[test]
        fn request_lease_overflow_preserves_counters_and_error() {
            let metrics = Metrics::default();
            metrics.set_gauge(Gauge::ActiveRequests, u64::MAX);
            assert!(matches!(
                metrics.request(),
                Err(crate::error::Error::Overloaded)
            ));
            assert_eq!(metrics.count(Event::Request), 0);
            assert_eq!(metrics.count(Event::RequestError), 0);
            assert_eq!(metrics.gauge(Gauge::ActiveRequests), u64::MAX);
        }

        #[test]
        fn opaque_body_attempts_exclude_empty_and_count_abandonment_once() {
            let metrics = Metrics::default();
            assert!(metrics.opaque_relay_body(0).is_none());
            assert_eq!(metrics.count(Event::OpaqueRelayBodyFailed), 0);
            let mut completed = metrics.opaque_relay_body(17).unwrap();
            assert_eq!(metrics.count(Event::OpaqueRelayBodyBytes), 0);
            completed.complete();
            completed.complete();
            drop(completed);
            drop(metrics.opaque_relay_body(19));
            assert_eq!(metrics.count(Event::OpaqueRelayBodyCompleted), 1);
            assert_eq!(metrics.count(Event::OpaqueRelayBodyBytes), 17);
            assert_eq!(metrics.count(Event::OpaqueRelayBodyFailed), 1);
        }

        #[test]
        fn integrity_diagnostics_have_fixed_names_and_saturating_sharded_counts() {
            let workers = Metrics::for_workers(2).unwrap();
            let events = [
                Event::CryptoDecryptCrcRejected,
                Event::CryptoDecryptAeadRejected,
                Event::FillDecryptDiskCorrupt,
                Event::FillDecryptRetainedCorrupt,
                Event::FillDecryptPeerCorrupt,
            ];
            for event in events {
                workers[0].record(event, u64::MAX);
                workers[0].record(event, 1);
                workers[1].record(event, 1);
                assert_eq!(workers[1].count(event), 1);
            }
            let mut output = String::new();
            workers[1].write_prometheus(&mut output).unwrap();
            for event in events {
                assert!(output.contains(&format!(
                    "# TYPE {} counter\n{} {}\n",
                    event.name(),
                    event.name(),
                    1
                )));
            }
            assert_only_disk_class_labels(&output);
            assert_eq!(
                EVENTS
                    .iter()
                    .map(|e| e.name())
                    .collect::<std::collections::BTreeSet<_>>()
                    .len(),
                EVENT_COUNT
            );
        }

        #[test]
        fn worker_quota_gauges_follow_authoritative_reservations() {
            let workers = Metrics::for_workers(2).unwrap();
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let other = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            workers[0]
                .observe_admission(WorkerId(7), admission.shared())
                .unwrap();
            workers[1]
                .observe_admission(WorkerId(19), other.shared())
                .unwrap();
            assert!(matches!(
                workers[0].observe_admission(WorkerId(20), other.shared()),
                Err(crate::error::Error::InvalidConfiguration)
            ));
            let scrape = || {
                let mut output = String::new();
                workers[1].write_prometheus(&mut output).unwrap();
                output
            };
            let relay_limit = admission.limit(ResourceClass::Relay);
            let ciphertext_limit = admission.limit(ResourceClass::Ciphertext);
            let idle = scrape();
            for worker in [7, 19] {
                for (name, value) in [
                    ("relay_used", 0),
                    ("relay_limit", relay_limit),
                    ("ciphertext_used_bytes", 0),
                    ("ciphertext_limit_bytes", ciphertext_limit),
                ] {
                    assert!(idle.contains(&format!(
                        "racer_worker_{name}{{worker=\"{worker}\"}} {value}\n"
                    )));
                }
            }
            let relay = admission
                .reserve(None, ResourceClass::Relay, relay_limit)
                .unwrap();
            let mut ciphertext = admission
                .reserve(None, ResourceClass::Ciphertext, ciphertext_limit)
                .unwrap();
            let full = scrape();
            for (name, value) in [
                ("relay_used", relay_limit),
                ("ciphertext_used_bytes", ciphertext_limit),
            ] {
                assert!(full.contains(&format!("racer_worker_{name}{{worker=\"7\"}} {value}\n")));
                assert!(full.contains(&format!("racer_worker_{name}{{worker=\"19\"}} 0\n")));
            }
            for class in [ResourceClass::Relay, ResourceClass::Ciphertext] {
                assert!(matches!(
                    admission.reserve(None, class, 1),
                    Err(flow_control::Error::Overloaded)
                ));
            }
            assert_eq!(scrape(), full, "rejection must not change quota gauges");
            let split = ciphertext.split(8).unwrap();
            assert_eq!(scrape(), full, "splitting retains the total charge");
            drop(split);
            ciphertext.shrink(16).unwrap();
            assert!(scrape().contains("racer_worker_ciphertext_used_bytes{worker=\"7\"} 16\n"));
            admission.stop();
            drop(admission);
            assert!(scrape().contains("racer_worker_ciphertext_used_bytes{worker=\"7\"} 16\n"));
            std::thread::spawn(move || drop((relay, ciphertext)))
                .join()
                .unwrap();
            // Recycler stats come from the live authority and reset once it exits;
            // quota charges must still match the idle scrape.
            let quotas = |text: &str| {
                text.lines()
                    .filter(|line| !line.contains("recycler_"))
                    .collect::<Vec<_>>()
                    .join("\n")
            };
            assert_eq!(
                quotas(&scrape()),
                quotas(&idle),
                "final release remains visible after owner exit"
            );
            assert_eq!(
                idle.lines()
                    .filter(|line| line.starts_with("racer_worker_relay_")
                        || line.starts_with("racer_worker_ciphertext_"))
                    .count(),
                8
            );
            // Four compatibility series and retained recycler bytes per worker.
            assert_eq!(
                idle.lines()
                    .filter(|line| line.contains("{worker="))
                    .count(),
                2 * (4 + 1)
            );
            assert!(!idle.contains("request="));
        }

        #[test]
        fn worker_quota_gauges_include_recycled_ciphertext_capacity() {
            let metrics = Metrics::default();
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            metrics
                .observe_admission(WorkerId(0), admission.shared())
                .unwrap();
            let mut reservation = admission
                .reserve(None, ResourceClass::Ciphertext, 1 << 20)
                .unwrap();
            let bytes = reservation.buffer(1 << 20).unwrap();
            reservation.recycle(bytes);
            drop(reservation);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 1048576\n"));
            admission.stop();
            output.clear();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 0\n"));
        }

        #[test]
        fn admission_classes_cover_every_class_in_index_order() {
            use flow_control::Class;
            assert_eq!(ADMISSION_CLASSES.len(), ResourceClass::COUNT);
            for (index, (class, label)) in ADMISSION_CLASSES.iter().enumerate() {
                assert_eq!(class.index(), index);
                assert!(label.chars().all(|c| c.is_ascii_lowercase() || c == '_'));
            }
        }

        #[test]
        fn admission_and_pool_series_sum_workers_and_label_retained_bytes() {
            let metrics = Metrics::for_workers(2).unwrap();
            let mut output = String::new();
            metrics[0].write_prometheus(&mut output).unwrap();
            // Nothing installed: no admission, pool, or process memory series.
            for absent in [
                "racer_admission_used",
                "racer_recycler_entries",
                "racer_staging_pool_entries",
                "racer_process_resident_bytes",
            ] {
                assert!(!output.contains(absent), "{absent}");
            }
            let quotas: Vec<_> = (0..2)
                .map(|_| {
                    flow_control::Quotas::new(AdmissionPolicy::new(
                        crate::test_support::cluster::config(false).limits,
                    ))
                })
                .collect();
            for (index, (metrics, admission)) in metrics.iter().zip(&quotas).enumerate() {
                metrics
                    .observe_admission(WorkerId(3 + index as u16), admission.shared())
                    .unwrap();
            }
            let mut reservation = quotas[0]
                .reserve(None, ResourceClass::Ciphertext, 1 << 20)
                .unwrap();
            let bytes = reservation.buffer(1 << 20).unwrap();
            reservation.recycle(bytes);
            drop(reservation);
            let _relay = quotas[1].reserve(None, ResourceClass::Relay, 1).unwrap();
            let recycler = quotas[0].buffer_pool_stats();
            assert!(recycler.bytes >= 1 << 20, "recycled buffer is retained");
            let staging = |entries, bytes, counter| flow_control::PoolStats {
                entries,
                bytes,
                target: 3,
                max_depth: 8,
                hits: counter,
                misses: counter + 1,
                rejects: counter + 2,
                trimmed: counter + 3,
                constrained: false,
            };
            metrics[0].observe_staging_pool(WorkerId(3), staging(2, 4096, 5));
            metrics[1].observe_staging_pool(WorkerId(4), staging(1, 1024, 10));
            output.clear();
            metrics[1].write_prometheus(&mut output).unwrap();
            let sum = |class| quotas.iter().map(|q| q.limit(class)).sum::<usize>();
            let ciphertext_used = quotas[0].used(ResourceClass::Ciphertext);
            for expected in [
                "# TYPE racer_admission_used gauge\n".to_string(),
                "# TYPE racer_admission_limit gauge\n".to_string(),
                format!("racer_admission_used{{class=\"ciphertext\"}} {ciphertext_used}\n"),
                "racer_admission_used{class=\"relay\"} 1\n".to_string(),
                "racer_admission_used{class=\"plaintext\"} 0\n".to_string(),
                format!(
                    "racer_admission_limit{{class=\"relay\"}} {}\n",
                    sum(ResourceClass::Relay)
                ),
                format!(
                    "racer_admission_limit{{class=\"plaintext\"}} {}\n",
                    sum(ResourceClass::Plaintext)
                ),
                format!("racer_recycler_entries {}\n", recycler.entries),
                format!("racer_recycler_bytes {}\n", recycler.bytes),
                "# TYPE racer_recycler_hits_total counter\n".to_string(),
                format!(
                    "racer_worker_recycler_bytes{{worker=\"3\"}} {}\n",
                    recycler.bytes
                ),
                "racer_worker_recycler_bytes{worker=\"4\"} 0\n".to_string(),
                "# TYPE racer_staging_pool_entries gauge\nracer_staging_pool_entries 3\n"
                    .to_string(),
                "racer_staging_pool_bytes 5120\n".to_string(),
                "racer_staging_pool_target 6\n".to_string(),
                "# TYPE racer_staging_pool_hits_total counter\nracer_staging_pool_hits_total 15\n"
                    .to_string(),
                "racer_staging_pool_misses_total 17\n".to_string(),
                "racer_staging_pool_rejects_total 19\n".to_string(),
                "racer_staging_pool_trimmed_total 21\n".to_string(),
                "# TYPE racer_worker_staging_pool_bytes gauge\n".to_string(),
                "racer_worker_staging_pool_bytes{worker=\"3\"} 4096\n".to_string(),
                "racer_worker_staging_pool_bytes{worker=\"4\"} 1024\n".to_string(),
                // Compatibility series remain per worker.
                "racer_worker_relay_used{worker=\"4\"} 1\n".to_string(),
            ] {
                assert!(
                    output.contains(&expected),
                    "missing {expected:?} in {output}"
                );
            }
            assert_eq!(
                output
                    .lines()
                    .filter(|l| l.starts_with("racer_admission_used{"))
                    .count(),
                <ResourceClass as flow_control::Class>::COUNT
            );
            // Each TYPE line appears exactly once.
            let types: Vec<_> = output
                .lines()
                .filter(|l| l.starts_with("# TYPE "))
                .collect();
            let unique: std::collections::BTreeSet<_> = types.iter().collect();
            assert_eq!(types.len(), unique.len());
        }

        #[test]
        fn process_memory_series_omit_absent_signals() {
            let metrics = Metrics::default();
            metrics.observe_memory(ProcessMemory {
                resident: Some(4096),
                ..Default::default()
            });
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains(
                "# TYPE racer_process_resident_bytes gauge\nracer_process_resident_bytes 4096\n"
            ));
            assert!(!output.contains("racer_cgroup_memory"));
            metrics.observe_memory(ProcessMemory {
                resident: Some(1),
                cgroup_current: Some(800),
                cgroup_limit: Some(1000),
                pressure_some_avg10: Some(12.5),
            });
            metrics.set_gauge(Gauge::MemoryPressureLevel, 2);
            metrics.record(Event::MemoryPressureTransitions, 3);
            metrics.record(Event::AllocatorTrims, 1);
            output.clear();
            metrics.write_prometheus(&mut output).unwrap();
            for expected in [
                "racer_cgroup_memory_current_bytes 800\n",
                "racer_cgroup_memory_limit_bytes 1000\n",
                "racer_cgroup_memory_pressure_some_avg10 12.5\n",
                "# TYPE racer_memory_pressure_level gauge\nracer_memory_pressure_level 2\n",
                "# TYPE racer_memory_pressure_transitions_total counter\nracer_memory_pressure_transitions_total 3\n",
                "# TYPE racer_allocator_trims_total counter\nracer_allocator_trims_total 1\n",
            ] {
                assert!(output.contains(expected), "missing {expected:?}");
            }
            // Non-finite PSI is not exported.
            metrics.observe_memory(ProcessMemory {
                pressure_some_avg10: Some(f64::NAN),
                ..Default::default()
            });
            output.clear();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(!output.contains("racer_cgroup_memory_pressure_some_avg10"));
            assert!(!output.contains("racer_process_resident_bytes"));
        }

        #[test]
        fn lookup_outcomes_preserve_results_and_export_fixed_series() {
            let metrics = Metrics::default();
            for (tier, hit, miss, error) in [
                (
                    LookupTier::Plaintext,
                    Event::PlaintextLookupHit,
                    Event::PlaintextLookupMiss,
                    Event::PlaintextLookupError,
                ),
                (
                    LookupTier::Ciphertext,
                    Event::CiphertextLookupHit,
                    Event::CiphertextLookupMiss,
                    Event::CiphertextLookupError,
                ),
                (
                    LookupTier::Pending,
                    Event::PendingLookupHit,
                    Event::PendingLookupMiss,
                    Event::PendingLookupError,
                ),
                (
                    LookupTier::DiskIndex,
                    Event::DiskIndexLookupHit,
                    Event::DiskIndexLookupMiss,
                    Event::DiskIndexLookupError,
                ),
            ] {
                assert_eq!(metrics.lookup(tier, Ok(Some(7))), Ok(Some(7)));
                assert_eq!(metrics.lookup::<u8>(tier, Ok(None)), Ok(None));
                for failure in [
                    crate::error::Error::CorruptRecord,
                    crate::error::Error::Io,
                    crate::error::Error::MissingKey,
                ] {
                    assert_eq!(metrics.lookup::<u8>(tier, Err(failure)), Err(failure));
                }
                assert_eq!(metrics.count(hit), 1);
                assert_eq!(metrics.count(miss), 1);
                assert_eq!(metrics.count(error), 3);
                let mut output = String::new();
                metrics.write_prometheus(&mut output).unwrap();
                assert!(output.contains(&format!(
                    "# TYPE {} counter\n{} 1\n",
                    hit.name(),
                    hit.name()
                )));
                assert!(output.contains(&format!("{} 1\n", miss.name())));
                assert!(output.contains(&format!("{} 3\n", error.name())));
            }
        }
        #[test]
        fn request_drop_counts_failure_once_and_workers_share_counters() {
            let mut workers = Metrics::for_workers(2).unwrap();
            let metrics = workers.pop().unwrap();
            let worker = workers.pop().unwrap();
            std::thread::spawn(move || {
                let mut success = worker.request().unwrap();
                success.success();
                drop(success);
                let mut overloaded = worker.request().unwrap();
                overloaded.fail(crate::error::Error::Overloaded);
                overloaded.fail(crate::error::Error::Overloaded);
                drop(overloaded);
                let abandoned = worker.request().unwrap();
                assert_eq!(worker.gauge(Gauge::ActiveRequests), 1);
                drop(abandoned);
            })
            .join()
            .unwrap();
            assert_eq!(metrics.count(Event::Request), 3);
            assert_eq!(metrics.count(Event::RequestError), 2);
            assert_eq!(metrics.count(Event::Overload), 1);
            assert_eq!(metrics.gauge(Gauge::ActiveRequests), 0);
        }
        #[test]
        fn fixed_series_saturate_and_leases_return_to_baseline() {
            let metrics = Metrics::default();
            for event in EVENTS {
                metrics.record(event, u64::MAX);
                metrics.record(event, 9);
                assert_eq!(metrics.count(event), 8);
            }
            let lease = metrics.lease(Gauge::ActiveRequests).unwrap();
            assert_eq!(metrics.clone().gauge(Gauge::ActiveRequests), 1);
            drop(lease);
            assert_eq!(metrics.gauge(Gauge::ActiveRequests), 0);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert_eq!(
                output.lines().filter(|line| !line.starts_with('#')).count(),
                EVENT_COUNT
                    + GAUGE_COUNT
                    + RETENTION_METRICS.len()
                    + HELD_METRICS.len()
                    + DISK_CLASS_METRICS.len() * 2
                    + crate::gpu_telemetry::BASE_SERIES
            );
            assert_only_disk_class_labels(&output);
            assert!(output.len() < 64 * 1024);
        }

        #[test]
        fn installed_credential_gauges_replace_values_across_shared_handles() {
            let mut workers = Metrics::for_workers(2).unwrap();
            let metrics = workers.pop().unwrap();
            let worker = workers.pop().unwrap();
            worker.set_gauge(Gauge::KeyringGeneration, 3);
            worker.set_gauge(Gauge::IdentityExpiresAtSeconds, 120);
            worker.set_gauge(Gauge::IdentityExpiresAtSeconds, 200);
            assert_eq!(metrics.gauge(Gauge::KeyringGeneration), 3);
            assert_eq!(metrics.gauge(Gauge::IdentityExpiresAtSeconds), 200);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains("racer_keyring_generation 3\n"));
            assert!(output.contains("racer_identity_expires_at_seconds 200\n"));
        }

        #[test]
        fn fixed_registry_is_aligned_and_clones_keep_their_writer() {
            assert!(matches!(
                Metrics::for_workers(0),
                Err(crate::error::Error::InvalidConfiguration)
            ));
            let workers = Metrics::for_workers(3).unwrap();
            // Cache-line alignment and per-shard storage assertions live in the core
            // test of the same name; this adapter retains its worker mapping checks.
            for (index, worker) in workers.iter().enumerate() {
                assert_eq!(worker.shard, index);
                let clone = worker.clone();
                assert_eq!(clone.shard, index);
                assert!(Arc::ptr_eq(&clone.admission, &workers[0].admission));
                clone.record(Event::MemoryHit, (index + 1) as u64);
            }
            assert_eq!(workers[0].count(Event::MemoryHit), 6);
        }

        #[test]
        fn concurrent_writers_and_scrapes_retain_totals_after_worker_exit() {
            let workers = Metrics::for_workers(4).unwrap();
            let reader = workers[0].clone();
            let barrier = Arc::new(std::sync::Barrier::new(workers.len() + 1));
            std::thread::scope(|scope| {
                for worker in workers {
                    let barrier = barrier.clone();
                    scope.spawn(move || {
                        barrier.wait();
                        for _ in 0..1000 {
                            let mut request = worker.request().unwrap();
                            worker.record(Event::MemoryHit, 1);
                            request.success();
                        }
                    });
                }
                barrier.wait();
                let mut last = 0;
                for _ in 0..100 {
                    let count = reader.count(Event::Request);
                    assert!((last..=4000).contains(&count));
                    last = count;
                    let mut output = String::new();
                    reader.write_prometheus(&mut output).unwrap();
                    assert_eq!(
                        output.lines().filter(|line| !line.starts_with('#')).count(),
                        EVENT_COUNT
                            + GAUGE_COUNT
                            + RETENTION_METRICS.len()
                            + HELD_METRICS.len()
                            + DISK_CLASS_METRICS.len() * 2
                            + crate::gpu_telemetry::BASE_SERIES
                    );
                    assert_only_disk_class_labels(&output);
                }
            });
            assert_eq!(reader.count(Event::Request), 4000);
            assert_eq!(reader.count(Event::MemoryHit), 4000);
            assert_eq!(reader.count(Event::RequestError), 0);
            assert_eq!(reader.gauge(Gauge::ActiveRequests), 0);
        }

        #[test]
        fn aggregate_events_saturate_without_wrapping() {
            let workers = Metrics::for_workers(2).unwrap();
            for event in EVENTS {
                workers[0].record(event, u64::MAX - 1);
                workers[1].record(event, 2);
                assert_eq!(workers[0].count(event), u64::MAX);
                workers[1].record(event, u64::MAX);
                assert_eq!(workers[1].count(event), u64::MAX);
            }
        }

        fn assert_only_disk_class_labels(output: &str) {
            let labeled: Vec<_> = output
                .lines()
                .filter(|line| {
                    line.contains('{') && !line.starts_with("racer_gpu_read_requests_total{")
                })
                .collect();
            assert_eq!(labeled.len(), DISK_CLASS_METRICS.len() * 2);
            for line in labeled {
                let (name, rest) = line.split_once('{').unwrap();
                assert!(DISK_CLASS_METRICS.contains(&name));
                let (labels, _) = rest.split_once('}').unwrap();
                assert!(
                    matches!(
                        labels,
                        "classification=\"owned\"" | "classification=\"nonowned\""
                    ),
                    "no identity or content labels"
                );
            }
        }

        #[test]
        fn gauges_preserve_node_wide_overflow_replacement_and_cross_thread_release() {
            let workers = Metrics::for_workers(2).unwrap();
            for gauge in GAUGES {
                workers[0].set_gauge(gauge, u64::MAX - 1);
                let lease = workers[1].lease(gauge).unwrap();
                assert_eq!(workers[0].gauge(gauge), u64::MAX);
                assert!(matches!(
                    workers[0].lease(gauge),
                    Err(crate::error::Error::Overloaded)
                ));
                assert!(matches!(
                    workers[1].lease(gauge),
                    Err(crate::error::Error::Overloaded)
                ));
                std::thread::spawn(move || drop(lease)).join().unwrap();
                assert_eq!(workers[0].gauge(gauge), u64::MAX - 1);
                workers[1].set_gauge(gauge, 0);
                workers[0].add_gauge(gauge, 2);
                workers[1].add_gauge(gauge, 3);
                assert_eq!(workers[0].gauge(gauge), 5);
            }
            let lease = workers[1].lease(Gauge::ActiveRequests).unwrap();
            let reader = workers[0].clone();
            drop(workers);
            std::thread::spawn(move || drop(lease)).join().unwrap();
            assert_eq!(reader.gauge(Gauge::ActiveRequests), 5);
        }
    }
}
