use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::AttemptId;
use crate::model::NodeId;
use crate::model::RequestId;
use crate::model::ResourceClass;
use crate::model::WorkerId;

use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use ::telemetry::Ring;
use ::telemetry::metrics;
use ::telemetry::server;
use ::telemetry::server::Handler;
use ::telemetry::server::Response;
use ::telemetry::server::Server;
pub use server::CONNECTION_TIMEOUT;
pub use server::CONTROL_SLOTS;
pub use server::MAX_CONNECTIONS;
pub use server::MAX_REQUEST_BYTES;
pub use server::MAX_RESPONSE_BYTES;
pub use server::RESERVED_BYTES;
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
use uring_runtime::deadline::Deadline;
use uring_runtime::environment;
use uring_runtime::reactor::Descriptor;

// Bounded diagnostics and HTTP endpoints, polled by an existing worker.

#[derive(Default)]
pub struct Telemetry {
    pub(crate) membership: OnceCell<Rc<dyn Fn() -> Result<MembershipDiagnostic>>>,
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
    server: Server,
}
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Result<Self> {
        let control = admission.reserve(None, ResourceClass::ControlProgress, CONTROL_SLOTS)?;
        let mut memory = admission.reserve(None, ResourceClass::RequestContext, RESERVED_BYTES)?;
        let bookkeeping = memory.split(CONTROL_SLOTS * uring_runtime::reactor::SUBMISSION_BYTES)?;
        let submissions = reactor.reserve_submissions(control, bookkeeping)?;
        Ok(Self {
            reactor,
            admission,
            server: Server::new(submissions, memory),
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
        };
        io.server
            .serve(&io.reactor, listener, &handler, scope)
            .await
    })
}

struct DiagnosticHandler<'a> {
    telemetry: &'a Telemetry,
    admission: &'a flow_control::Quotas<AdmissionPolicy>,
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
}
fn get(
    telemetry: &Telemetry,
    path: &str,
    admission_usable: bool,
    mut output: &mut dyn Write,
) -> std::result::Result<Response, std::fmt::Error> {
    let ready = admission_usable && telemetry.health.ready();
    let (response, body, event) = match path {
        "/healthz" if telemetry.health.live() => (Response::Text, "ok\n", Event::DiagnosticHealth),
        "/healthz" => (Response::Unavailable, "not live\n", Event::DiagnosticHealth),
        "/readyz" if ready => (Response::Text, "ready\n", Event::DiagnosticReady),
        "/readyz" => (Response::Unavailable, "not ready\n", Event::DiagnosticReady),
        "/metrics" => (Response::Metrics, "", Event::DiagnosticMetrics),
        "/debug/membership" => (Response::Text, "", Event::DiagnosticMetrics),
        "/debug/failures" | "/debug/aead" | "/debug/send-crc" => {
            (Response::Text, "", Event::DiagnosticFailures)
        }
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
    } else if path == "/debug/failures" {
        telemetry.failures.write(&mut output)?;
    } else {
        output.write_str(body)?;
    }
    Ok(response)
}

#[derive(Clone, Default)]
pub struct Health(Arc<Mutex<Status>>);
#[derive(Default)]
struct Status {
    lifecycle: State,
    resources: Resources,
}
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum State {
    #[default]
    Starting,
    Ready,
    Degraded,
    Draining,
    Stopped,
}
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
        let status = self.0.lock().map_err(|_| Error::Unavailable)?;
        Ok(match status.lifecycle {
            State::Ready if !status.resources.usable_at(now) => State::Degraded,
            state => state,
        })
    }
    pub fn observe(&self, resources: Resources) -> Result<()> {
        self.0.lock().map_err(|_| Error::Unavailable)?.resources = resources;
        Ok(())
    }
    pub fn ready(&self) -> bool {
        self.state().is_ok_and(|state| state == State::Ready)
    }
    /// Starting, degraded, and draining remain live; a response establishes progress.
    pub fn live(&self) -> bool {
        self.state().is_ok_and(|state| state != State::Stopped)
    }
    pub fn transition(&self, state: State) -> Result<()> {
        let mut status = self.0.lock().map_err(|_| Error::Unavailable)?;
        if status.lifecycle == State::Stopped && state != State::Stopped
            || status.lifecycle == State::Draining
                && !matches!(state, State::Draining | State::Stopped)
            || state == State::Ready
                && !status
                    .resources
                    .usable_at(uring_runtime::environment::now())
        {
            return Err(Error::Unavailable);
        }
        status.lifecycle = state;
        Ok(())
    }
}

// Opt-in HTTP send fingerprints, never authentication or payload scans on I/O.
// Shared limits: one active owner, two samples/second, 240 total, 120 seconds.
// Cached CRCs are reused. Pending/unavailable samples do not prove agreement.
pub const SEND_CRC_CAPACITY: usize = 64;
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
            || !racer_identity::canonical_uuid(&self.sender.0)
            || !racer_identity::canonical_uuid(&self.receiver.0)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
#[derive(Clone, Default)]
pub struct Samples(Arc<Mutex<SendCrcState>>);
struct SendCrcState {
    first: Option<Instant>,
    last: Option<Instant>,
    busy: bool,
    eligible: u64,
    sampled: u64,
    skipped: u64,
    entries: Ring<Arc<Sample>, SEND_CRC_CAPACITY>,
}
impl Default for SendCrcState {
    fn default() -> Self {
        Self {
            first: None,
            last: None,
            busy: false,
            eligible: 0,
            sampled: 0,
            skipped: 0,
            entries: Ring::default(),
        }
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
    owner: Samples,
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
        for _ in 0..SEND_CRC_CAPACITY {
            self.0.lock().unwrap().last = None;
            let (ticket, mut work) = self.begin(&pair, &pair.sender, &pair.receiver).unwrap();
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
        if sender != &pair.sender || receiver != &pair.receiver {
            return None;
        }
        let now = environment::now();
        let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
        state.eligible = state.eligible.saturating_add(1);
        let first = *state.first.get_or_insert(now);
        if state.busy
            || state.sampled >= 240
            || now.saturating_duration_since(first) >= Duration::from_secs(120)
            || state.last.is_some_and(|last| {
                now.saturating_duration_since(last) < Duration::from_millis(500)
            })
        {
            state.skipped = state.skipped.saturating_add(1);
            return None;
        }
        state.busy = true;
        state.last = Some(now);
        state.sampled += 1;
        let sample = Arc::new(Sample {
            sequence: state.sampled,
            sender: sender.clone(),
            receiver: receiver.clone(),
            data: Mutex::new(Data {
                crypto: None,
                send: "pending",
                status: "pending",
                error: None,
                facts: None,
            }),
        });
        state.entries.push(sample.clone());
        Some((
            Ticket(sample.clone()),
            Work {
                sample,
                owner: self.clone(),
                facts: None,
                cached: false,
            },
        ))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let (entries, eligible, sampled, skipped, busy) = {
            let state = self.0.lock().unwrap_or_else(|e| e.into_inner());
            (
                state.entries.clone(),
                state.eligible,
                state.sampled,
                state.skipped,
                state.busy,
            )
        };
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
        self.owner.0.lock().unwrap_or_else(|e| e.into_inner()).busy = false;
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

type AeadRing = Ring<(crate::security::CryptoId, AeadFailure), AEAD_CAPACITY>;

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
    crate::peer::protocol::encode_deadline(uring_runtime::deadline::Deadline(at))
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
    Arc<Mutex<Ring<(WorkerId, Failure), FAILURE_CAPACITY>>>,
    Arc<Mutex<AeadRing>>,
);

/// Absent in standalone components until the production composition attaches it.
#[derive(Clone, Default)]
pub struct Observer(Option<(Failures, WorkerId)>);
impl Failures {
    pub fn write_aead(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let ring = self.1.lock().unwrap_or_else(|e| e.into_inner()).clone();
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
    pub fn observer(&self, worker: WorkerId) -> Observer {
        Observer(Some((self.clone(), worker)))
    }
    pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        // Copy bounded records before formatting; never hold the lock across I/O.
        let ring = self.0.lock().unwrap_or_else(|e| e.into_inner()).clone();
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
    pub(crate) fn record_aead(&self, id: crate::security::CryptoId, failure: AeadFailure) {
        let Some((failures, _)) = &self.0 else {
            return;
        };
        let mut ring = failures.1.lock().unwrap_or_else(|e| e.into_inner());
        ring.push((id, failure));
    }
    pub fn record(&self, failure: Failure) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        let mut ring = failures.0.lock().unwrap_or_else(|e| e.into_inner());
        ring.push((*worker, failure));
    }
    pub fn result<T>(&self, stage: Stage, scope: &RequestScope, result: Result<T>) -> Result<T> {
        if let Err(error) = &result {
            self.record(Failure::new(stage, *error).request(scope));
        }
        result
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
/// Clones retain their writer shard; reads aggregate the fixed node registry.
#[derive(Clone)]
pub struct Metrics {
    core: ::telemetry::Metrics<Event, Gauge>,
    admission: Arc<[OnceLock<(WorkerId, flow_control::SharedQuotas<AdmissionPolicy>)>]>,
    shard: usize,
}

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
}
metrics! { Gauge, GAUGES, GAUGE_COUNT;
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
    pub(crate) fn opaque_relay_body(&self, bytes: usize) -> Option<OpaqueRelayBody<'_>> {
        (bytes != 0).then(|| OpaqueRelayBody {
            metrics: self,
            bytes,
            completed: false,
        })
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
        Ok(::telemetry::Metrics::shards(count)
            .into_iter()
            .enumerate()
            .map(|(shard, core)| Self {
                core,
                admission: admission.clone(),
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
        // Only runtime worker IDs are labels. Read the authority's actual charge,
        // including pooled ciphertext capacity, without sampling on worker polls.
        // A stalled worker therefore remains observable from another worker.
        const QUOTAS: [&str; 4] = [
            "racer_worker_relay_used",
            "racer_worker_relay_limit",
            "racer_worker_ciphertext_used_bytes",
            "racer_worker_ciphertext_limit_bytes",
        ];
        if self.admission.iter().any(|s| s.get().is_some()) {
            for name in QUOTAS {
                writeln!(out, "# TYPE {name} gauge")?;
            }
            for shard in self.admission.iter() {
                if let Some((worker, usage)) = shard.get() {
                    let (relay_used, relay_limit) = (
                        usage.used(crate::model::ResourceClass::Relay),
                        usage.limit(crate::model::ResourceClass::Relay),
                    );
                    let (ciphertext_used, ciphertext_limit) = (
                        usage.used(crate::model::ResourceClass::Ciphertext),
                        usage.limit(crate::model::ResourceClass::Ciphertext),
                    );
                    for (name, value) in QUOTAS.into_iter().zip([
                        relay_used,
                        relay_limit,
                        ciphertext_used,
                        ciphertext_limit,
                    ]) {
                        writeln!(out, "{name}{{worker=\"{}\"}} {value}", worker.0)?;
                    }
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
        fn fixed_parser_and_worst_case_response_bounds() {
            let telemetry = Telemetry::default();
            for event in crate::telemetry::EVENTS {
                telemetry.metrics.record(event, u64::MAX);
            }
            let text = diagnostic_text(&telemetry, "/metrics");
            assert!(text.len() < MAX_RESPONSE_BYTES);
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
            let mut telemetry = Telemetry::default();
            telemetry.metrics = workers[0].clone();
            for event in crate::telemetry::EVENTS {
                telemetry.metrics.record(event, u64::MAX);
            }
            let text = diagnostic_text(&telemetry, "/metrics");
            assert!(text.len() < MAX_RESPONSE_BYTES);
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
                4 * 64
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
    }

    pub(crate) mod health_tests {
        use super::*;
        use std::time::Duration;
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
            assert!(samples.0.lock().unwrap().busy);
            drop(work);
            assert!(!samples.0.lock().unwrap().busy);
            assert!(
                samples.begin(&p, &p.sender, &p.receiver).is_none(),
                "burst is one"
            );
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("send=abandoned status=unavailable"));
            samples.0.lock().unwrap().last = None;
            samples.0.lock().unwrap().first = Some(environment::now() - Duration::from_secs(120));
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
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
            let state = samples.0.lock().unwrap();
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
            let snapshot = samples.0.lock().unwrap().entries.clone();
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
            let p = pair();
            let samples = Samples::default();
            for i in 0..240 {
                samples.0.lock().unwrap().last = None;
                let (ticket, mut work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
                ticket.finish(i % 2 == 0);
                work.facts = Some(crate::telemetry::test_aead_failure());
                work.finish(None);
                drop(work);
                drop(ticket);
            }
            samples.0.lock().unwrap().last = None;
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
                    assert!(self.failures.0.try_lock().is_ok());
                    assert!(self.failures.1.try_lock().is_ok());
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
            assert_eq!(failures.0.lock().unwrap().total(), 0);
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
        use crate::model::ResourceClass;

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
                assert_eq!(workers[1].count(event), u64::MAX);
            }
            let mut output = String::new();
            workers[1].write_prometheus(&mut output).unwrap();
            for event in events {
                assert!(output.contains(&format!(
                    "# TYPE {} counter\n{} {}\n",
                    event.name(),
                    event.name(),
                    u64::MAX
                )));
            }
            assert!(!output.contains('{'), "no identity or content labels");
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
            assert_eq!(
                scrape(),
                idle,
                "final release remains visible after owner exit"
            );
            assert_eq!(
                idle.lines()
                    .filter(|line| line.contains("{worker="))
                    .count(),
                8
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
                assert_eq!(metrics.count(event), u64::MAX);
            }
            let lease = metrics.lease(Gauge::ActiveRequests).unwrap();
            assert_eq!(metrics.clone().gauge(Gauge::ActiveRequests), 1);
            drop(lease);
            assert_eq!(metrics.gauge(Gauge::ActiveRequests), 0);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert_eq!(
                output.lines().filter(|line| !line.starts_with('#')).count(),
                EVENT_COUNT + GAUGE_COUNT
            );
            assert!(!output.contains('{'));
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
                        EVENT_COUNT + GAUGE_COUNT
                    );
                    assert!(!output.contains('{'));
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
