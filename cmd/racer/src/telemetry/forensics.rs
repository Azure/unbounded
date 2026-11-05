use super::*;

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
    /// The stored AEAD tag (last 16 ciphertext bytes); fingerprints the copy.
    pub tag: [u8; 16],
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
        write!(out, " tag=")?;
        hex(out, &self.tag)
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
    CorruptAead,
    CorruptUnclassified,
    Other,
}
impl DecryptReason {
    #[cfg(test)]
    pub(crate) const ALL: [Self; 9] = [
        Self::Cancelled,
        Self::DeadlineExceeded,
        Self::MissingKey,
        Self::InvalidConfiguration,
        Self::InvalidRequest,
        Self::Overloaded,
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
            Error::Overloaded | Error::FlightFull => Self::Overloaded,
            Error::CorruptRecord => match rejection {
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
    // Kept so the forensic counter layout (`counter_rows=58`) stays stable; the shared flight
    // table allocates ids and inserts infallibly, so these are no longer produced.
    #[allow(dead_code)]
    WaiterId,
    #[allow(dead_code)]
    Identity,
    #[allow(dead_code)]
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
    Reactor(racer_io::scoped::SubmissionRejection),
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

pub(crate) fn timestamp(clock: &racer_io::Clock, at: std::time::Instant) -> u64 {
    crate::peer::protocol::encode_deadline(clock, racer_io::deadline::Deadline(at))
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
            // Stamped by `Observer::record` from the node clock.
            unix_millis: 0,
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
    pub(crate) fn at(mut self, unix_millis: u64) -> Self {
        self.unix_millis = unix_millis;
        self
    }
}

#[derive(Clone, Default)]
pub struct Failures {
    /// Terminal client-request failures, one per failed request.
    terminal: SharedRing<(WorkerId, Failure), FAILURE_CAPACITY>,
    /// AEAD open failures with their peer provenance.
    aead: AeadRing,
    /// Protected-path failures outside admission.
    protected: SharedRing<ProtectedFailure, 64>,
    /// Protected-path failures at fill admission.
    admission: SharedRing<ProtectedFailure, 64>,
    /// Final candidate trails for requests that exhausted every source.
    candidates: SharedRing<(WorkerId, CandidateFinal), 16>,
    /// Join and wait gate events with per-kind counts.
    gates: std::sync::Arc<GateEvents>,
    /// Single-flight expiries.
    expiries: SharedRing<(WorkerId, RequestId, u64, FlightExpiry), 16>,
    /// Decrypt scope failures.
    decrypts: SharedRing<DecryptFailure, DECRYPT_FAILURE_CAPACITY>,
    clock: FailureClock,
}

/// Wall clock that stamps failures when they are recorded.
#[derive(Clone)]
pub(crate) struct FailureClock(std::sync::Arc<racer_io::Clock>);
impl Default for FailureClock {
    fn default() -> Self {
        Self(std::sync::Arc::new(racer_io::Clock::host()))
    }
}

pub(crate) fn unix_millis(at: std::time::SystemTime) -> u64 {
    at.duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .min(u64::MAX as u128) as u64
}

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

    pub queue: racer_io::xthread::offload::AdmissionSnapshot,

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
    /// Failures stamped from `clock` instead of the host wall clock.
    pub fn with_clock(clock: racer_io::Clock) -> Self {
        Self {
            clock: FailureClock(std::sync::Arc::new(clock)),
            ..Self::default()
        }
    }
    pub(crate) fn unix_millis(&self) -> u64 {
        unix_millis(self.clock.0.wall_now())
    }
    pub(crate) fn now(&self) -> Instant {
        self.clock.0.now()
    }
    /// Write the independent bounded expiry ring without changing gate counters.
    pub(crate) fn write_flight_expiry(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let snapshot = self.expiries.snapshot();
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
        let snapshot = self.gates.ring.snapshot();
        writeln!(
            out,
            "schema_version=1 counter_rows=58 coverage=flight_join_and_reactor_submit_events terminal_cause=false operation_identity=not_recorded page_identity=request_relative_number existing_failed=propagated_not_new_rejection unclassified=join_overload_origin_unknown wait_events=terminal_once wait_cause=prior_observation_not_terminal_capacity counters_and_ring=non_atomic_snapshot total={} retained={} overwritten={} capacity=64",
            snapshot.total(),
            snapshot.len(),
            snapshot.total().saturating_sub(snapshot.len() as u64)
        )?;
        for (index, count) in self.gates.counts.iter().enumerate() {
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
        let snapshot = self.candidates.snapshot();
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
        let ring = if admission {
            &self.admission
        } else {
            &self.protected
        };
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
        let ring = self.aead.snapshot();
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
        let ring = self.decrypts.snapshot();
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
        let ring = self.terminal.snapshot();
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
    /// The recording clock's wall time, or `None` when detached.
    pub(crate) fn unix_millis(&self) -> Option<u64> {
        self.0.as_ref().map(|(failures, _)| failures.unix_millis())
    }
    /// Publish already frozen facts after all worker-local borrows have ended.
    pub(crate) fn flight_expiry(&self, scope: &RequestScope, page: u64, facts: FlightExpiry) {
        if let Some((failures, worker)) = &self.0 {
            failures
                .expiries
                .push((*worker, scope.request, page, facts));
        }
    }

    pub(crate) fn gate_rejection(&self, scope: &RequestScope, page: Option<u64>, facts: GateFacts) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        failures.gates.counts[facts.index()].fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        failures.gates.ring.push(GateEvent {
            worker: *worker,
            failure: Failure::new(
                Stage::Admission,
                match facts {
                    GateFacts::Wait { error, .. } => error,
                    _ => Error::Overloaded,
                },
            )
            .request(scope)
            .at(failures.unix_millis()),
            page,
            facts,
        });
    }
    pub(crate) fn candidate_final(&self, mut record: CandidateFinal) {
        if let Some((failures, worker)) = &self.0 {
            if record.failure.unix_millis == 0 {
                record.failure.unix_millis = failures.unix_millis();
            }
            failures.candidates.push((*worker, record));
        }
    }
    pub(crate) fn record_aead(&self, id: crate::security::CryptoId, failure: AeadFailure) {
        let Some((failures, _)) = &self.0 else {
            return;
        };
        failures.aead.push((id, failure));
    }
    pub(crate) fn record_decrypt(&self, failure: DecryptFailure) {
        if let Some((failures, _)) = &self.0 {
            failures.decrypts.push(failure);
        }
    }
    pub fn record(&self, mut failure: Failure) {
        let Some((failures, worker)) = &self.0 else {
            return;
        };
        if failure.unix_millis == 0 {
            failure.unix_millis = failures.unix_millis();
        }
        failures.terminal.push((*worker, failure));
        if matches!(
            failure.stage,
            Stage::ClientRead | Stage::FirstSlice | Stage::NextSlice | Stage::ClientWrite
        ) {
            failures.protected.push(ProtectedFailure {
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
        failures.admission.push(ProtectedFailure {
            worker: *worker,
            failure: Failure::new(Stage::Admission, Error::Overloaded)
                .request(scope)
                .detail(detail.unwrap_or_default())
                .at(failures.unix_millis()),
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
        let scope = RequestScope::host(
            RequestId([255; 16]),
            racer_io::Clock::host().now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let facts = FlightExpiry {
            observed_unix_ms: u64::MAX,
            overshoot_us: u64::MAX,
            queue: racer_io::xthread::offload::AdmissionSnapshot {
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
        let scope = RequestScope::host(
            RequestId([255; 16]),
            racer_io::Clock::host().now() + std::time::Duration::from_secs(5),
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
                GateFacts::Reactor(racer_io::scoped::SubmissionRejection {
                    reason: racer_io::scoped::RejectionReason::SubmissionQueueFull,
                    kind: racer_io::scoped::SubmissionKind::Readiness,
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
                assert!(self.failures.candidates.try_snapshot().is_some());
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
                assert!(self.failures.terminal.try_snapshot().is_some());
                assert!(self.failures.aead.try_snapshot().is_some());
                assert!(self.failures.protected.try_snapshot().is_some());
                assert!(self.failures.admission.try_snapshot().is_some());
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
        let scope = RequestScope::host(
            RequestId([255; 16]),
            racer_io::Clock::host().now() + std::time::Duration::from_secs(10),
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
        tag: [255; 16],
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) mod failures_tests {
        use super::*;

        #[test]
        fn decrypt_failure_classification_is_fixed_and_preserves_unknown_causes() {
            use crate::security::IntegrityRejection::Aead;
            // Classification only: this does not exercise the allocator failure path.
            let cases = [
                (Error::Cancelled, None, "cancelled"),
                (Error::DeadlineExceeded, None, "deadline_exceeded"),
                (Error::MissingKey, None, "missing_key"),
                (Error::InvalidConfiguration, None, "invalid_configuration"),
                (Error::InvalidRequest, None, "invalid_request"),
                (Error::Overloaded, None, "overloaded"),
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
                    assert!(self.failures.decrypts.try_snapshot().is_some());
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
            // The full ring stays small regardless of the response buffer size.
            assert!(out.text.len() + 512 < 64 * 1024);
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
            observer.record_aead(id, aead);
            struct Unlocked<'a> {
                failures: &'a Failures,
                text: String,
            }
            impl std::fmt::Write for Unlocked<'_> {
                fn write_str(&mut self, value: &str) -> std::fmt::Result {
                    assert!(self.failures.terminal.try_snapshot().is_some());
                    assert!(self.failures.aead.try_snapshot().is_some());
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
                    "total=1 retained=1 overwritten=0 capacity=64\nseq=1 w=3 crypto=4:5 ms=18446744073709551615 request={} acquisition=none attempt=none supplier=none remote=none page={} number=18446744073709551615 key={} nonce={} lengths=4294967295/4294967295 aad={} tag={}\n",
                    "ff".repeat(16),
                    "ff".repeat(32),
                    "ff".repeat(16),
                    "ff".repeat(24),
                    "ff".repeat(32),
                    "ff".repeat(16),
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
            let scope = RequestScope::host(
                RequestId([0xab; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap();
            observer.result(Stage::ClientRead, &scope, Ok(())).unwrap();
            assert_eq!(failures.terminal.snapshot().total(), 0);
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
}
