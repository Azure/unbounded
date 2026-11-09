//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::admission::ResourceClass;
use crate::admission::reserve_connection;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::RequestId;
use racer_io::deadline::Deadline;
use racer_io::scoped::ReactorWake;
use racer_io::scoped::SUBMISSION_BYTES;
use racer_io::scoped::SubmissionCapacity;
use racer_io::scoped::fs::Buffer;
use racer_io::scoped::fs::prelude::*;
use std::ops::Deref;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
use std::time::Instant;

// Seeded hashing only in simulated worlds; production retains std hashing.
#[cfg(not(any(test, feature = "dst")))]
pub(crate) type HashMap<K, V> = std::collections::HashMap<K, V>;
#[cfg(not(any(test, feature = "dst")))]
pub(crate) type HashSet<K> = std::collections::HashSet<K>;
#[cfg(any(test, feature = "dst"))]
pub(crate) type HashMap<K, V> = std::collections::HashMap<K, V, HashState>;
#[cfg(any(test, feature = "dst"))]
pub(crate) type HashSet<K> = std::collections::HashSet<K, HashState>;
/// Test hasher with a fixed seed so simulated runs iterate maps identically.
#[cfg(any(test, feature = "dst"))]
#[derive(Clone, Debug, Default)]
pub(crate) struct HashState;
#[cfg(any(test, feature = "dst"))]
impl std::hash::BuildHasher for HashState {
    type Hasher = std::collections::hash_map::DefaultHasher;
    fn build_hasher(&self) -> Self::Hasher {
        Self::Hasher::new()
    }
}

/// Racer candidate policy shares the cancellation lifetime, but is not runtime policy.
#[derive(Clone)]
pub struct Cancellation {
    inner: racer_io::CancelToken,
    state: Arc<CancellationState>,
}
struct CancellationState {
    candidate_total: OnceLock<Instant>,
    candidate_idle: OnceLock<Mutex<(std::time::Duration, Instant)>>,
    candidate_body: OnceLock<Mutex<CandidateBody>>,
    /// The placeholder `deadline` of an untimed scope, shared by its clones.
    /// Any other `deadline` value, such as one narrowed by a header scope, is
    /// a real bound.
    untimed_at: OnceLock<Instant>,
}
type CandidateBody = flow_control::progress::ProgressBudget;
impl Cancellation {
    pub fn new() -> Result<Self> {
        Ok(Self {
            inner: racer_io::CancelToken::new(),
            state: Arc::new(CancellationState {
                candidate_total: OnceLock::new(),
                candidate_idle: OnceLock::new(),
                candidate_body: OnceLock::new(),
                untimed_at: OnceLock::new(),
            }),
        })
    }
    pub fn is_canceled(&self) -> bool {
        self.inner.is_canceled()
    }
    /// Subscribe for one operation's lifetime, even when several operations share
    /// an executor waker. Dropping the subscription removes its wake. Never fails.
    pub fn subscribe(&self) -> Result<racer_io::CancelWait> {
        Ok(self.inner.wait())
    }
    /// Live wake registrations (diagnostics and tests).
    pub fn waiters(&self) -> usize {
        self.inner.waiters()
    }
    pub fn cancel(&self) -> Result<()> {
        self.inner.cancel();
        Ok(())
    }
}

/// Placeholder distance of an untimed scope's `deadline` field; never a bound.
/// A century: far beyond any process lifetime, and still encodable on the wire.
const UNTIMED_HORIZON: std::time::Duration = std::time::Duration::from_secs(100 * 365 * 24 * 3600);

#[derive(Clone)]
pub struct RequestScope {
    pub request: RequestId,
    /// Absolute deadline. An untimed scope (see [`RequestScope::untimed`])
    /// holds [`UNTIMED_HORIZON`] past creation here only so `min`-narrowing
    /// arithmetic yields the narrower bound; checks, op contexts, and
    /// [`RequestScope::deadline_at`] treat it as no deadline.
    pub deadline: Deadline,
    pub cancellation: Cancellation,
    /// Local diagnostics only: original acquisition ceiling and candidate share.
    /// Never serialized or consulted by deadline/cancellation policy.
    pub(crate) body_deadlines: Option<(Instant, Instant)>,
    /// Time source for deadline and candidate timer checks.
    pub(crate) clock: racer_io::Clock,
    /// The work serves a read page past its window's head. Such work may take
    /// plaintext only while a page stays free for heads (see
    /// [`crate::read::Fill`]), and it never waits for plaintext, so ready pages
    /// held behind heads cannot use up a worker's share while the heads wait.
    /// Children inherit it.
    pub(crate) ahead: bool,
    /// NUMA node of the thread that will read the page's received bytes, when
    /// that is not the worker that fetches them: a host read decrypts its
    /// pages on the requesting worker's crypto helpers, while each page is
    /// fetched by its owning worker. The fetching worker lands RDMA pages on a
    /// rail whose NIC is on this node, so the registered receive buffer is
    /// local to the decrypt. None means the fetching worker's own node.
    /// Children inherit it.
    pub(crate) reader_numa: Option<u16>,
    /// The work fetches a page for a host read that decrypts it straight out
    /// of its RDMA receive buffer, so the page keeps that buffer until it is
    /// delivered. With [`Self::ahead`], its grant is deferred while the
    /// rail's pool is low (see [`crate::read::fill::ahead_grant_reserve`]).
    /// Children inherit it.
    pub(crate) holds_receive: bool,
}
impl RequestScope {
    /// The scope's clock.
    pub fn clock(&self) -> &racer_io::Clock {
        &self.clock
    }

    /// Local peer-exchange cap, including checkout and verification in the exchange.
    /// Set once; phase/body progress cannot renew it or change signed authority.
    pub(crate) fn set_candidate_total(&self, expires: Instant) -> Result<()> {
        self.check()?;
        // A cap may elapse while setting up the exchange. Install it as expired,
        // not invalid input; the next scope check rejects work without renewal.
        if self
            .deadline_at()
            .is_some_and(|deadline| expires > deadline)
        {
            return Err(Error::InvalidRequest);
        }
        self.cancellation
            .state
            .candidate_total
            .set(expires)
            .map_err(|_| Error::Internal)
    }

    /// Local alarm only. The immutable hard deadline is what credentials and
    /// routes sign; neither phase completion nor body progress can extend it.
    pub(crate) fn set_candidate_idle(&self, allowance: std::time::Duration) -> Result<()> {
        self.check()?;
        self.cancellation
            .state
            .candidate_idle
            .set(Mutex::new((allowance, self.clock.now() + allowance)))
            .map_err(|_| Error::Internal)?;
        Ok(())
    }

    pub(crate) fn candidate_progress(&self) -> Result<()> {
        self.candidate_progress_at(self.clock.now())
    }

    /// [`RequestScope::candidate_progress`] at an explicit instant.
    pub(crate) fn candidate_progress_at(&self, now: Instant) -> Result<()> {
        self.check_at(now)?;
        if let Some(idle) = self.cancellation.state.candidate_idle.get() {
            let mut idle = idle.lock().map_err(|_| Error::Unavailable)?;
            idle.1 = now + idle.0;
        }
        Ok(())
    }

    /// Local known-body completion budget, even without an affordable alternative.
    /// May reserve fallback time, but never changes signed authority.
    pub(crate) fn set_candidate_body_budget(
        &self,
        observation: std::time::Duration,
        complete_by: Instant,
    ) -> Result<()> {
        if observation.is_zero()
            || self
                .deadline_at()
                .is_some_and(|deadline| complete_by > deadline)
        {
            return Err(Error::InvalidRequest);
        }
        self.cancellation
            .state
            .candidate_body
            .set(Mutex::new(CandidateBody::new(observation, complete_by)))
            .map_err(|_| Error::Internal)
    }

    /// Only known-length network bodies participate. Start at first bytes, not
    /// checkout/admission/head wait. Integer cross products avoid rate thresholds,
    /// division by zero and overflowing Instant additions. Completion is retained.
    pub(crate) fn candidate_body_progress(&self, received: usize, total: usize) -> Result<()> {
        self.candidate_body_progress_at(received, total, self.clock.now())
    }

    /// [`RequestScope::candidate_body_progress`] at an explicit instant.
    pub(crate) fn candidate_body_progress_at(
        &self,
        received: usize,
        total: usize,
        now: Instant,
    ) -> Result<()> {
        self.candidate_progress_at(now)?;
        if received > total {
            return Err(Error::InvalidRequest);
        }
        if let Some(body) = self.cancellation.state.candidate_body.get() {
            let mut body = body.lock().map_err(|_| Error::Unavailable)?;
            if !body.advance(now, received, total) {
                return Err(Error::DeadlineExceeded);
            }
        }
        Ok(())
    }

    /// A scope whose deadline and timers are checked against `clock`.
    pub fn with_clock(
        request: RequestId,
        deadline: Instant,
        clock: racer_io::Clock,
    ) -> Result<Self> {
        Ok(Self {
            request,
            deadline: Deadline(deadline),
            cancellation: Cancellation::new()?,
            body_deadlines: None,
            clock,
            ahead: false,
            reader_numa: None,
            holds_receive: false,
        })
    }

    /// A lifetime scope with no deadline; it ends only when canceled.
    pub fn untimed(request: RequestId, clock: racer_io::Clock) -> Result<Self> {
        let placeholder = clock
            .now()
            .checked_add(UNTIMED_HORIZON)
            .ok_or(Error::InvalidConfiguration)?;
        let scope = Self::with_clock(request, placeholder, clock)?;
        scope
            .cancellation
            .state
            .untimed_at
            .set(placeholder)
            .map_err(|_| Error::Internal)?;
        Ok(scope)
    }

    /// The deadline, or `None` for an untimed scope.
    pub fn deadline_at(&self) -> Option<Instant> {
        (self.cancellation.state.untimed_at.get() != Some(&self.deadline.0))
            .then_some(self.deadline.0)
    }

    /// A new scope for `request` with its own cancellation, on this scope's
    /// clock. It keeps the read-ahead flag and the reader's NUMA node, so an
    /// acquisition run under an owned child scope still lands its RDMA page
    /// on a rail local to the helper that decrypts it.
    pub fn child(&self, request: RequestId, deadline: Instant) -> Result<Self> {
        let mut child = Self::with_clock(request, deadline, self.clock.clone())?;
        child.ahead = self.ahead;
        child.reader_numa = self.reader_numa;
        child.holds_receive = self.holds_receive;
        Ok(child)
    }

    /// Test support: a scope on the host clock.
    #[cfg(test)]
    pub fn host(request: RequestId, deadline: Instant) -> Result<Self> {
        Self::with_clock(request, deadline, racer_io::Clock::host())
    }

    /// Test support: a scope on `clock`, with the clock first so tests read
    /// left to right.
    #[cfg(any(test, feature = "dst"))]
    pub fn on(clock: &racer_io::Clock, request: RequestId, deadline: Instant) -> Result<Self> {
        Self::with_clock(request, deadline, clock.clone())
    }

    pub fn check(&self) -> Result<()> {
        self.check_at(self.clock.now())
    }
    /// [`RequestScope::check`] against an explicit instant instead of the
    /// environment clock.
    pub fn check_at(&self, now: Instant) -> Result<()> {
        if self.cancellation.is_canceled() {
            Err(Error::Canceled)
        } else if self.deadline_at().is_some_and(|deadline| now >= deadline) {
            Err(Error::DeadlineExceeded)
        } else {
            if let Some(expires) = self.cancellation.state.candidate_total.get()
                && now >= *expires
            {
                return Err(Error::DeadlineExceeded);
            }
            if let Some(body) = self.cancellation.state.candidate_body.get() {
                let body = body.lock().map_err(|_| Error::Unavailable)?;
                if body.expired(now) {
                    return Err(Error::DeadlineExceeded);
                }
            }
            if let Some(idle) = self.cancellation.state.candidate_idle.get()
                && now >= idle.lock().map_err(|_| Error::Unavailable)?.1
            {
                return Err(Error::DeadlineExceeded);
            }
            Ok(())
        }
    }
    pub fn cancel(&self) -> Result<()> {
        self.cancellation.cancel()
    }
}
impl racer_io::Scope for RequestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        RequestScope::check(self)
    }

    fn cancel_token(&self) -> Option<&racer_io::CancelToken> {
        Some(&self.cancellation.inner)
    }

    fn recheck_within(&self) -> Option<std::time::Duration> {
        self.deadline_at()
            .map(|deadline| deadline.saturating_duration_since(self.clock.now()))
    }

    fn polled_checks(&self) -> bool {
        let state = &self.cancellation.state;
        state.candidate_total.get().is_some()
            || state.candidate_body.get().is_some()
            || state.candidate_idle.get().is_some()
    }

    fn op_ctx(&self) -> racer_io::OpCtx {
        racer_io::OpCtx::with_cancel(&self.cancellation.inner).deadline(
            self.deadline_at()
                .map_or(racer_io::Deadline::NEVER, racer_io::Deadline::at),
        )
    }
}
impl crate::control::rest::Scope for RequestScope {
    fn deadline(&self) -> Option<Instant> {
        self.deadline_at()
    }

    fn narrowed(&self, until: Instant) -> Self {
        let mut scope = self.clone();
        scope.deadline.0 = scope.deadline.0.min(until);
        scope
    }
}

/// Racer admission and request policy around the worker-local runtime reactor.
///
/// Inline storage cannot meet the stable-buffer contract:
/// ```compile_fail
/// use racer::error::Result;
/// use racer_io::ring::IoBuffer;
/// struct Inline([u8; 16]);
/// impl IoBuffer for Inline {
///     type Error = racer::error::Error;
///     fn bytes(&self) -> Result<&[u8]> { Ok(&self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(&mut self.0) }
/// }
/// ```
/// Borrowed storage does not have an independent completion lifetime:
/// ```compile_fail
/// use racer::error::Result;
/// use racer_io::ring::IoBuffer;
/// struct Borrowed<'a>(&'a mut [u8]);
/// unsafe impl IoBuffer for Borrowed<'_> {
///     type Error = racer::error::Error;
///     fn bytes(&self) -> Result<&[u8]> { Ok(self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(self.0) }
/// }
/// ```
/// Audited production buffers own independent storage:
/// ```
/// use racer::{memory::PlaintextBuffer, admission::AdmissionPolicy};
/// use racer_io::ring::IoBuffer;
/// use flow_control::Charge;
/// use page_alloc::AlignedBuffer;
/// fn independent<T: 'static>() {}
/// fn completion_safe<B: IoBuffer>() { independent::<B>(); }
/// completion_safe::<PlaintextBuffer>();
/// completion_safe::<AlignedBuffer<Charge<AdmissionPolicy>>>();
/// ```
/// Immutable ciphertext cannot be used for receive:
/// ```compile_fail
/// use std::rc::Rc;
/// use racer::{memory::CiphertextPage, runtime::{Reactor, RequestScope}};
/// use racer_io::ring::Descriptor;
/// fn receive(r: &Reactor, fd: Rc<Descriptor>, page: CiphertextPage, scope: &RequestScope) {
///     let _ = r.recv(fd, page, (), scope);
/// }
/// ```
pub struct Reactor {
    core: racer_io::scoped::Reactor<RequestScope>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}
impl Deref for Reactor {
    type Target = racer_io::scoped::Reactor<RequestScope>;
    fn deref(&self) -> &Self::Target {
        &self.core
    }
}
impl Reactor {
    /// The worker's admission quotas.
    pub(crate) fn admission(&self) -> &Rc<flow_control::Quotas<AdmissionPolicy>> {
        &self.admission
    }
    /// Build a worker reactor on `env`'s backend and context.
    pub fn new(admission: Rc<flow_control::Quotas<AdmissionPolicy>>, env: &racer_io::Env) -> Self {
        let diagnostics = admission.clone();
        Self {
            core: racer_io::scoped::Reactor::new(
                admission.policy().limits().queue_entries.get(),
                env,
            )
            .with_rejection_observer(move |scope, facts| {
                diagnostics.policy().observer().gate_rejection(
                    scope,
                    None,
                    crate::telemetry::GateFacts::Reactor(facts),
                );
            }),
            admission,
        }
    }
    pub fn init(&self) -> Result<()> {
        self.core.init().map_err(Into::into)
    }
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        self.core.poll_budgeted(budget).map_err(Into::into)
    }
    pub fn wait(&self, duration: std::time::Duration) -> Result<()> {
        self.core.wait(duration).map_err(Into::into)
    }
    pub fn waker(&self) -> Result<ReactorWake> {
        self.core.waker().map_err(Into::into)
    }
    pub fn file_buffer(&self, length: usize) -> Result<Buffer> {
        if length == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.core.file_buffer(length).map_err(Into::into)
    }
    pub fn file_bytes(&self, bytes: &[u8]) -> Result<Buffer> {
        if bytes.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        self.core.file_bytes(bytes).map_err(Into::into)
    }
    pub fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation> {
        reserve_connection(&self.admission, role)
    }
    pub(crate) fn reserve_submissions(
        &self,
        slots: flow_control::Charge<AdmissionPolicy>,
        memory: flow_control::Charge<AdmissionPolicy>,
    ) -> Result<Rc<SubmissionCapacity>> {
        let capacity = slots.amount();
        slots.validate(ResourceClass::ControlProgress, capacity)?;
        memory.validate(
            ResourceClass::RequestContext,
            capacity
                .checked_mul(SUBMISSION_BYTES)
                .ok_or(Error::InvalidConfiguration)?,
        )?;
        if !self.admission.owns(&slots) || !self.admission.owns(&memory) {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .reserve_submissions(capacity, (slots, memory))
            .map_err(|error| match error {
                racer_io::Error::InvalidInput => Error::InvalidConfiguration,
                other => other.into(),
            })
    }
    pub fn file_fence(&self, request: RequestId) -> Operation<'_, ()> {
        self.core
            .fence_matching(move |scope| scope.request == request)
    }
}

#[cfg(test)]
mod tests {
    use super::HashMap;
    use crate::error::Error;
    use crate::model::RequestId;
    use crate::runtime::RequestScope;
    use std::time::Duration;
    mod deadline {
        //! Request scope and cancellation regression tests.
        use crate::runtime::*;

        mod tests {
            use super::*;
            use std::sync::atomic::AtomicUsize;
            use std::sync::atomic::Ordering;
            use std::task::Wake;
            use std::task::Waker;
            use std::time::Duration;
            #[test]
            fn child_scope_keeps_reader_numa_and_read_ahead() {
                let clock = racer_io::ctx::VirtualClock::new();
                let mut scope =
                    RequestScope::untimed(RequestId([3; 16]), clock.clone().into()).unwrap();
                let deadline = scope.clock().now() + Duration::from_secs(5);
                assert_eq!(
                    scope.child(scope.request, deadline).unwrap().reader_numa,
                    None
                );
                scope.reader_numa = Some(1);
                scope.ahead = true;
                scope.holds_receive = true;
                let child = scope.child(RequestId([4; 16]), deadline).unwrap();
                assert_eq!(child.reader_numa, Some(1));
                assert!(child.ahead);
                assert!(child.holds_receive);
                assert_eq!(child.request, RequestId([4; 16]));
                assert_eq!(child.deadline_at(), Some(deadline));
            }

            #[test]
            fn untimed_scope_ends_only_by_cancellation_and_narrows_to_bounded() {
                use crate::control::rest::Scope;
                use racer_io::Scope as _;
                let clock = racer_io::ctx::VirtualClock::new();
                let scope =
                    RequestScope::untimed(RequestId([2; 16]), clock.clone().into()).unwrap();
                assert_eq!(scope.deadline_at(), None);
                assert_eq!(Scope::deadline(&scope), None);
                assert_eq!(scope.recheck_within(), None);
                assert_eq!(scope.op_ctx().deadline, racer_io::Deadline::NEVER);
                clock.advance(Duration::from_secs(400 * 24 * 3600)).unwrap();
                assert_eq!(scope.check(), Ok(()));
                let until = scope.clock().now() + Duration::from_secs(5);
                let narrowed = scope.narrowed(until);
                assert_eq!(narrowed.deadline_at(), Some(until));
                clock.advance(Duration::from_secs(5)).unwrap();
                assert_eq!(narrowed.check(), Err(Error::DeadlineExceeded));
                assert_eq!(scope.check(), Ok(()));
                // A clone whose deadline is narrowed by assignment is bounded.
                let mut header = scope.clone();
                header.deadline.0 = header.deadline.0.min(until);
                assert_eq!(header.deadline_at(), Some(until));
                assert_eq!(header.check(), Err(Error::DeadlineExceeded));
                scope.cancel().unwrap();
                assert_eq!(scope.check(), Err(Error::Canceled));
            }

            #[test]
            fn rest_narrowing_preserves_policy_cancellation_and_parent_deadline() {
                use crate::control::rest::Scope;
                let clock = racer_io::SimClock::new(102);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let end = start + Duration::from_secs(30);
                let scope = RequestScope::on(&sim_ctx.clock, RequestId([102; 16]), end).unwrap();
                scope
                    .set_candidate_total(start + Duration::from_secs(10))
                    .unwrap();
                let narrowed = scope.narrowed(start + Duration::from_secs(20));
                assert_eq!(scope.deadline(), Some(end));
                assert_eq!(narrowed.deadline(), Some(start + Duration::from_secs(20)));
                assert_eq!(narrowed.narrowed(end).deadline(), narrowed.deadline());
                clock.advance(Duration::from_secs(10));
                assert_eq!(narrowed.check(), Err(Error::DeadlineExceeded));
                scope.cancel().unwrap();
                assert_eq!(narrowed.check(), Err(Error::Canceled));
            }

            #[test]
            fn candidate_total_survives_body_completion_and_never_changes_authority() {
                let clock = racer_io::SimClock::new(97);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let scope = RequestScope::on(
                    &sim_ctx.clock,
                    RequestId([97; 16]),
                    start + Duration::from_secs(60),
                )
                .unwrap();
                let signed =
                    crate::peer::protocol::encode_deadline(scope.clock(), scope.deadline).unwrap();
                scope
                    .set_candidate_total(start + Duration::from_secs(30))
                    .unwrap();
                scope.set_candidate_idle(Duration::from_secs(10)).unwrap();
                scope
                    .set_candidate_body_budget(
                        Duration::from_secs(10),
                        start + Duration::from_secs(20),
                    )
                    .unwrap();
                scope.candidate_body_progress(10, 80).unwrap();
                for received in (20..=80).step_by(10) {
                    clock.advance(Duration::from_secs(2));
                    scope.candidate_body_progress(received, 80).unwrap();
                }
                clock.advance(Duration::from_secs(7));
                scope.candidate_progress().unwrap();
                assert_eq!(
                    scope.set_candidate_total(start + Duration::from_secs(40)),
                    Err(Error::Internal)
                );
                clock.advance(Duration::from_secs(9));
                assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
                assert_eq!(
                    scope.candidate_body_progress(80, 80),
                    Err(Error::DeadlineExceeded)
                );
                assert_eq!(
                    crate::peer::protocol::encode_deadline(scope.clock(), scope.deadline).unwrap(),
                    signed
                );
            }

            #[test]
            fn candidate_body_budget_accepts_original_ceiling_and_rejects_invalid_bounds() {
                let clock = racer_io::SimClock::new(98);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let end = start + Duration::from_secs(30);
                let scope = RequestScope::on(&sim_ctx.clock, RequestId([98; 16]), end).unwrap();
                assert_eq!(
                    scope.set_candidate_total(end + Duration::from_secs(1)),
                    Err(Error::InvalidRequest)
                );
                assert_eq!(
                    scope.set_candidate_body_budget(Duration::ZERO, end),
                    Err(Error::InvalidRequest)
                );
                assert_eq!(
                    scope.set_candidate_body_budget(
                        Duration::from_secs(10),
                        end + Duration::from_secs(1)
                    ),
                    Err(Error::InvalidRequest)
                );
                scope
                    .set_candidate_body_budget(Duration::from_secs(10), end)
                    .unwrap();
                scope.candidate_body_progress(1, 1000).unwrap();
                scope.candidate_body_progress(2, 1000).unwrap();
                clock.advance(Duration::from_secs(10));
                assert_eq!(
                    scope.candidate_body_progress(3, 1000),
                    Err(Error::DeadlineExceeded)
                );
                assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
                assert_eq!(
                    scope.candidate_body_progress(1000, 1000),
                    Err(Error::DeadlineExceeded)
                );
                let elapsed = RequestScope::on(&sim_ctx.clock, RequestId([99; 16]), end).unwrap();
                elapsed.set_candidate_total(start).unwrap();
                assert_eq!(elapsed.check(), Err(Error::DeadlineExceeded));
            }

            #[test]
            fn candidate_body_reserve_keeps_healthy_progress_and_ignores_unknown_lengths() {
                let clock = racer_io::SimClock::new(91);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let healthy = RequestScope::on(
                    &sim_ctx.clock,
                    RequestId([91; 16]),
                    start + Duration::from_secs(30),
                )
                .unwrap();
                healthy.set_candidate_idle(Duration::from_secs(10)).unwrap();
                healthy
                    .set_candidate_body_budget(
                        Duration::from_secs(10),
                        start + Duration::from_secs(20),
                    )
                    .unwrap();
                healthy.candidate_body_progress(10, 80).unwrap();
                for received in (20..=80).step_by(10) {
                    clock.advance(Duration::from_secs(2));
                    healthy.candidate_body_progress(received, 80).unwrap();
                }
                assert!(sim_ctx.clock.now() > start + Duration::from_secs(10));
                assert_eq!(healthy.deadline.0, start + Duration::from_secs(30));
                // Completed network body does not subject later verification to the reserve.
                clock.advance(Duration::from_secs(7));
                healthy.check().unwrap();
                for reserve in [false, true] {
                    let now = sim_ctx.clock.now();
                    let scope = RequestScope::on(
                        &sim_ctx.clock,
                        RequestId([92; 16]),
                        now + Duration::from_secs(30),
                    )
                    .unwrap();
                    scope.set_candidate_idle(Duration::from_secs(10)).unwrap();
                    if reserve {
                        scope
                            .set_candidate_body_budget(
                                Duration::from_secs(10),
                                now + Duration::from_secs(20),
                            )
                            .unwrap();
                    }
                    for received in 1..=14 {
                        clock.advance(Duration::from_secs(2));
                        if reserve {
                            // Unknown-length/non-body progress has no rate prediction.
                            scope.candidate_progress().unwrap();
                        } else {
                            // Unconfigured low-level scope retains its original ceiling.
                            scope.candidate_body_progress(received, usize::MAX).unwrap();
                        }
                    }
                    clock.advance(Duration::from_secs(2));
                    assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
                }
            }

            #[test]
            fn candidate_body_rate_starts_at_first_bytes_and_expiry_is_sticky() {
                let clock = racer_io::SimClock::new(93);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let scope = RequestScope::on(
                    &sim_ctx.clock,
                    RequestId([93; 16]),
                    start + Duration::from_secs(30),
                )
                .unwrap();
                scope.set_candidate_idle(Duration::from_secs(10)).unwrap();
                scope
                    .set_candidate_body_budget(
                        Duration::from_secs(10),
                        start + Duration::from_secs(20),
                    )
                    .unwrap();
                clock.advance(Duration::from_secs(9));
                scope.candidate_body_progress(1, usize::MAX).unwrap();
                // Same-timestamp samples cannot divide by zero or invent a rate.
                scope.candidate_body_progress(2, usize::MAX).unwrap();
                for received in 3..12 {
                    clock.advance(Duration::from_secs(1));
                    scope.candidate_body_progress(received, usize::MAX).unwrap();
                }
                clock.advance(Duration::from_secs(1));
                assert_eq!(
                    scope.candidate_body_progress(12, usize::MAX),
                    Err(Error::DeadlineExceeded)
                );
                assert_eq!(
                    scope.candidate_body_progress(usize::MAX, usize::MAX),
                    Err(Error::DeadlineExceeded)
                );
                assert!(sim_ctx.clock.now() < scope.deadline.0);
            }

            #[test]
            fn candidate_idle_progress_never_renews_hard_deadline_or_revives_expiry() {
                let clock = racer_io::SimClock::new(39);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let scope = RequestScope::on(
                    &sim_ctx.clock,
                    RequestId([39; 16]),
                    start + Duration::from_secs(3),
                )
                .unwrap();
                scope.set_candidate_idle(Duration::from_secs(1)).unwrap();
                let signed =
                    crate::peer::protocol::encode_deadline(scope.clock(), scope.deadline).unwrap();
                for _ in 0..5 {
                    clock.advance(Duration::from_millis(500));
                    scope.candidate_progress().unwrap();
                    assert_eq!(
                        crate::peer::protocol::encode_deadline(scope.clock(), scope.deadline)
                            .unwrap(),
                        signed
                    );
                }
                clock.advance(Duration::from_millis(500));
                assert_eq!(scope.candidate_progress(), Err(Error::DeadlineExceeded));
                let stalled = RequestScope::on(
                    &sim_ctx.clock,
                    RequestId([40; 16]),
                    start + Duration::from_secs(10),
                )
                .unwrap();
                stalled.set_candidate_idle(Duration::from_secs(1)).unwrap();
                clock.advance(Duration::from_secs(1));
                assert_eq!(stalled.check(), Err(Error::DeadlineExceeded));
                assert_eq!(stalled.candidate_progress(), Err(Error::DeadlineExceeded));
            }
            struct Count(AtomicUsize);
            impl Wake for Count {
                fn wake(self: Arc<Self>) {
                    self.0.fetch_add(1, Ordering::Relaxed);
                }
            }
            #[test]
            fn runtime_scope_retains_candidate_policy_and_shared_cancellation() {
                let clock = racer_io::SimClock::new(101);
                let sim_env = clock.ctx(0);
                let sim_ctx = sim_env.clone();
                let start = sim_ctx.clock.now();
                let scope = RequestScope::on(
                    &sim_ctx.clock,
                    RequestId([101; 16]),
                    start + Duration::from_secs(30),
                )
                .unwrap();
                scope
                    .set_candidate_total(start + Duration::from_secs(10))
                    .unwrap();
                let clone = scope.clone();
                clock.advance(Duration::from_secs(10));
                assert_eq!(racer_io::Scope::check(&clone), Err(Error::DeadlineExceeded));
                let count = Arc::new(Count(AtomicUsize::new(0)));
                let registration = racer_io::Scope::cancel_wait(&clone).unwrap();
                registration.register(&Waker::from(count.clone()));
                scope.cancel().unwrap();
                assert_eq!(racer_io::Scope::check(&clone), Err(Error::Canceled));
                assert_eq!(count.0.load(Ordering::Relaxed), 1);
            }
            /// Explicit-time checks match the environment clock without reading it.
            #[test]
            fn check_at_uses_explicit_instant() {
                let start = Instant::now();
                let scope = RequestScope::host(RequestId([7; 16]), start + Duration::from_secs(30))
                    .unwrap();
                scope
                    .set_candidate_total(start + Duration::from_secs(10))
                    .unwrap();
                assert_eq!(scope.check_at(start), Ok(()));
                assert_eq!(scope.check_at(start + Duration::from_secs(9)), Ok(()));
                assert_eq!(
                    scope.check_at(start + Duration::from_secs(10)),
                    Err(Error::DeadlineExceeded)
                );
                let late = RequestScope::host(RequestId([8; 16]), start).unwrap();
                assert_eq!(late.check_at(start), Err(Error::DeadlineExceeded));
                assert_eq!(late.check_at(start - Duration::from_secs(1)), Ok(()));
                late.cancel().unwrap();
                assert_eq!(
                    late.check_at(start - Duration::from_secs(1)),
                    Err(Error::Canceled)
                );
            }
            #[test]
            fn clones_preserve_deadline_and_wake_independent_waiters() {
                let scope =
                    RequestScope::host(RequestId([0; 16]), Instant::now() + Duration::from_secs(2))
                        .unwrap();
                let a = Arc::new(Count(AtomicUsize::new(0)));
                let b = Arc::new(Count(AtomicUsize::new(0)));
                let first = scope.cancellation.subscribe().unwrap();
                let second = scope.cancellation.subscribe().unwrap();
                first.register(&Waker::from(a.clone()));
                second.register(&Waker::from(b.clone()));
                let clone = scope.clone();
                assert_eq!(clone.deadline.0, scope.deadline.0);
                clone.cancel().unwrap();
                assert_eq!(scope.check(), Err(Error::Canceled));
                assert_eq!(a.0.load(Ordering::Relaxed), 1);
                assert_eq!(b.0.load(Ordering::Relaxed), 1);
                assert_eq!(
                    RequestScope::host(RequestId([1; 16]), Instant::now())
                        .unwrap()
                        .check(),
                    Err(Error::DeadlineExceeded)
                );
            }
        }
    }
    mod reactor {
        //! Reactor ownership and simulation regression tests.
        use crate::runtime::*;

        mod tests {
            use super::*;
            use racer_io::ring::Descriptor;
            use racer_io::ring::IoBuffer;
            use racer_io::ring::SocketAddress;
            use racer_io::sim as simulation;
            use std::os::unix::net::UnixStream;
            use std::task::Context;
            use std::task::Poll;
            use std::time::Duration;
            use std::time::Instant;

            #[test]
            fn direct_filesystem_buffers_preserve_nonempty_boundary() {
                let env = racer_io::Env::host();
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(4))));
                let reactor = Reactor::new(admission.clone(), &env);
                assert!(matches!(
                    reactor.file_buffer(0),
                    Err(Error::InvalidConfiguration)
                ));
                assert!(matches!(
                    reactor.file_bytes(b""),
                    Err(Error::InvalidConfiguration)
                ));
                assert_eq!(admission.used(ResourceClass::RequestContext), 0);
                let mut buffer: racer_io::scoped::fs::Buffer = reactor.file_bytes(b"abc").unwrap();
                assert_eq!(buffer.prefix(3).unwrap(), b"abc");
                assert!(buffer.prefix(4).is_err());
                assert!(buffer.advance(4).is_err());
                buffer.advance(1).unwrap();
                assert_eq!(buffer.remaining(), 2);
                assert_eq!(buffer.bytes().unwrap(), b"bc");
                drop(buffer);
                assert_eq!(admission.used(ResourceClass::RequestContext), 0);
                let buffer = reactor.file_buffer(3).unwrap();
                assert_eq!(buffer.bytes().unwrap(), &[0; 3]);
                drop(buffer);
                assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            }

            #[test]
            fn movable_production_buffers_preserve_subrange_through_completion() {
                use crate::http::OwnedBuffer;
                use crate::memory::BufferPool;
                use crate::peer::transport::WireBuffer;
                use http1::connection::BufferRange;
                let env = racer_io::Env::host();
                fn check<B: IoBuffer>(buffer: B) {
                    let mut range = BufferRange::new::<Error>(buffer, 1..2).unwrap();
                    let ptr = range.bytes_mut().unwrap().as_mut_ptr();
                    let finish: Box<dyn FnOnce() -> B> = Box::new(move || range.into_inner());
                    // SAFETY: completion owns the fixed backing until after this write.
                    unsafe { ptr.write(7) };
                    let buffer = finish();
                    assert_eq!(buffer.bytes().unwrap(), &[0, 7, 0]);
                }
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(4))));
                check(OwnedBuffer::new(&crate::http::HttpContext(admission.clone()), 3).unwrap());
                let pool = BufferPool::new(admission.clone());
                check(WireBuffer::unkeyed(&pool, &admission, 3).unwrap());
                check(
                    pool.plaintext(
                        admission
                            .reserve(
                                Some(&racer_wire::control::CacheId("provenance".into())),
                                ResourceClass::Plaintext,
                                3,
                            )
                            .unwrap(),
                        3,
                    )
                    .unwrap(),
                );
                let reactor = Reactor::new(admission, &env);
                check(reactor.file_buffer(3).unwrap());
            }

            fn limits(capacity: usize) -> crate::config::Limits {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.queue_entries = std::num::NonZeroUsize::new(capacity).unwrap();
                limits
            }
            pub(super) fn scope() -> RequestScope {
                RequestScope::host(
                    crate::model::RequestId([0; 16]),
                    Instant::now() + Duration::from_secs(5),
                )
                .unwrap()
            }
            pub(super) fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
                future
                    .as_mut()
                    .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            }
            pub(super) fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
                let deadline = Instant::now() + Duration::from_secs(5);
                loop {
                    if let Poll::Ready(result) = poll(&mut future) {
                        return result;
                    }
                    assert!(Instant::now() < deadline, "reactor failed to make progress");
                    reactor.poll_budgeted(8).unwrap();
                    reactor.wait(Duration::from_millis(1)).unwrap();
                }
            }
            pub(super) fn kernel_reactor(env: &racer_io::Env, capacity: usize) -> Option<Reactor> {
                match io_uring::IoUring::new(2) {
                    Ok(ring) => drop(ring),
                    Err(error)
                        if matches!(
                            error.raw_os_error(),
                            Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
                        ) =>
                    {
                        eprintln!("io_uring kernel test unavailable: {error}");
                        return None;
                    }
                    Err(error) => panic!("unexpected io_uring setup failure: {error}"),
                }
                let reactor = Reactor::new(
                    Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(
                        capacity,
                    )))),
                    env,
                );
                reactor.init().unwrap();
                Some(reactor)
            }
            struct Buffer(Vec<u8>);
            // SAFETY: private fixed Vec owns stable exclusive backing.
            unsafe impl IoBuffer for Buffer {
                fn bytes(&self) -> racer_io::Result<&[u8]> {
                    Ok(&self.0)
                }
                fn bytes_mut(&mut self) -> racer_io::Result<&mut [u8]> {
                    Ok(&mut self.0)
                }
            }
            fn buffer(bytes: &[u8]) -> Buffer {
                Buffer(bytes.into())
            }
            #[test]
            fn file_fence_selects_only_the_matching_racer_request() {
                let sim = simulation::SimOs::new();
                let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                let reactor = Reactor::new(
                    Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(4)))),
                    &env,
                );
                sim.write_file(std::path::Path::new("/one"), b"one")
                    .unwrap();
                sim.write_file(std::path::Path::new("/two"), b"two")
                    .unwrap();
                let first = scope();
                let second =
                    RequestScope::host(crate::model::RequestId([1; 16]), first.deadline.0).unwrap();
                sim.inject("open", simulation::Fault::Delay(100)).unwrap();
                let mut a = reactor.file_open(
                    None,
                    std::ffi::CString::new("/one").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &first,
                );
                assert!(poll(&mut a).is_pending());
                sim.inject("open", simulation::Fault::Delay(100)).unwrap();
                let mut b = reactor.file_open(
                    None,
                    std::ffi::CString::new("/two").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &second,
                );
                assert!(poll(&mut b).is_pending());
                drive(&reactor, reactor.file_fence(first.request)).unwrap();
                assert_eq!(reactor.in_flight(), 1);
                assert!(matches!(poll(&mut a), Poll::Ready(Err(Error::Canceled))));
                assert!(poll(&mut b).is_pending());
                drop((a, b));
                drive(&reactor, reactor.file_fence(second.request)).unwrap();
                assert_eq!(reactor.in_flight(), 0);
            }
            #[test]
            fn listener_retry_preserves_racer_policy_after_capacity_recovery() {
                let sim = simulation::SimOs::new();
                let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                let reactor = Reactor::new(
                    Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(1)))),
                    &env,
                );
                let request = scope();
                let (fd, _peer) = sim.socket_pair();
                let mut busy = reactor.readiness(Rc::new(fd), libc::POLLIN as u32, &request);
                assert!(poll(&mut busy).is_pending());
                let address = SocketAddress::Unix("/retry-listener".into());
                let listener = Rc::new(sim.listen(address.clone()).unwrap());
                let mut accept = reactor
                    .retry_overloaded(&request, || reactor.accept(listener.clone(), &request));
                assert!(poll(&mut accept).is_pending());
                assert_eq!(reactor.in_flight(), 1);
                drop(busy);
                for _ in 0..4 {
                    reactor.poll_budgeted(8).unwrap();
                }
                let _client = sim.connect(address).unwrap();
                drop(drive(&reactor, accept).unwrap());
                assert_eq!(reactor.in_flight(), 0);
            }
            #[test]
            fn gate_events_use_late_observer_and_actual_readiness_rejection() {
                let sim = simulation::SimOs::new();
                let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(1))));
                let reactor = Reactor::new(admission.clone(), &env);
                let failures = crate::telemetry::Failures::default();
                admission
                    .policy()
                    .set_observer(failures.observer(crate::model::WorkerId(7)));
                let request = scope();
                let (fd, _peer) = sim.socket_pair();
                let fd = Rc::new(fd);
                let mut first = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
                assert!(poll(&mut first).is_pending());
                let mut second = reactor.readiness(fd, libc::POLLIN as u32, &request);
                assert!(matches!(
                    poll(&mut second),
                    Poll::Ready(Err(Error::Overloaded))
                ));
                assert_eq!(reactor.in_flight(), 1);
                let mut output = String::new();
                failures.write_gate_events(&mut output).unwrap();
                assert!(
                    output.contains("reason=OrdinaryFull kind=Readiness events=1"),
                    "{output}"
                );
                assert!(output.contains("worker=7"));
                assert!(output.contains("used: Some(1), limit: Some(1)"));
                drop((first, second));
                drive(&reactor, reactor.drain()).unwrap();
                assert_eq!(reactor.in_flight(), 0);
            }
            #[test]
            fn real_drain_io_preserves_control_capacity_after_admission_stop() {
                let env = racer_io::Env::host();
                let Some(reactor) = kernel_reactor(&env, 2) else {
                    return;
                };
                assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
                let control = reactor
                    .admission
                    .reserve(None, ResourceClass::ControlProgress, 2)
                    .unwrap();
                reactor.admission.stop();
                let scope = scope();
                let (left, right) = UnixStream::pair().unwrap();
                let left = Rc::new(Descriptor::from(left));
                let right = Rc::new(Descriptor::from(right));
                drive(&reactor, reactor.send(left, buffer(b"drain"), (), &scope)).unwrap();
                let read = drive(
                    &reactor,
                    reactor.recv(right.clone(), buffer(&[0; 8]), (), &scope),
                )
                .unwrap();
                assert_eq!(&read.buffer.bytes().unwrap()[..read.bytes], b"drain");
                let mut pending = reactor.readiness(right, libc::POLLOUT as u32, &scope);
                assert!(poll(&mut pending).is_pending());
                drive(&reactor, reactor.drain()).unwrap();
                assert_eq!(reactor.in_flight(), 0);
                assert!(matches!(
                    poll(&mut pending),
                    Poll::Ready(Err(Error::Canceled))
                ));
                assert_eq!(reactor.init(), Err(Error::Unavailable));
                assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 2);
                drop(control);
            }

            mod reserved_submission_tests {
                use super::*;
                fn reserve(
                    admission: &flow_control::Quotas<AdmissionPolicy>,
                ) -> (
                    flow_control::Charge<AdmissionPolicy>,
                    flow_control::Charge<AdmissionPolicy>,
                ) {
                    (
                        admission
                            .reserve(None, ResourceClass::ControlProgress, 1)
                            .unwrap(),
                        admission
                            .reserve(None, ResourceClass::RequestContext, SUBMISSION_BYTES)
                            .unwrap(),
                    )
                }
                #[test]
                fn provenance_capacity_and_completion_ownership() {
                    let env = racer_io::Env::host();
                    let reactor =
                        kernel_reactor(&env, 2).expect("real io_uring reserved submissions");
                    let failures = crate::telemetry::Failures::default();
                    reactor
                        .admission
                        .policy()
                        .set_observer(failures.observer(crate::model::WorkerId(8)));
                    let request = scope();
                    let foreign = flow_control::Quotas::new(AdmissionPolicy::new(limits(2)));
                    let (slots, memory) = reserve(&foreign);
                    assert!(matches!(
                        reactor.reserve_submissions(slots, memory),
                        Err(Error::InvalidConfiguration)
                    ));
                    assert_eq!(foreign.used(ResourceClass::ControlProgress), 0);
                    assert_eq!(foreign.used(ResourceClass::RequestContext), 0);
                    let (slots, memory) = reserve(&reactor.admission);
                    let capacity = reactor.reserve_submissions(slots, memory).unwrap();
                    let (slots, memory) = reserve(&reactor.admission);
                    assert!(matches!(
                        reactor.reserve_submissions(slots, memory),
                        Err(Error::InvalidConfiguration)
                    ));
                    let (socket, mut peer) = UnixStream::pair().unwrap();
                    let fd = Rc::new(Descriptor::from(socket));
                    let mut ordinary = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
                    assert!(poll(&mut ordinary).is_pending());
                    let mut overflow = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
                    assert!(matches!(
                        poll(&mut overflow),
                        Poll::Ready(Err(Error::Overloaded))
                    ));
                    let mut send =
                        reactor.send_reserved(fd.clone(), buffer(b"x"), capacity.clone(), &request);
                    assert!(poll(&mut send).is_pending());
                    let deadline = Instant::now() + Duration::from_secs(2);
                    while reactor.in_flight() != 1 {
                        assert!(Instant::now() < deadline);
                        reactor.poll_budgeted(8).unwrap();
                        reactor.wait(Duration::from_millis(1)).unwrap();
                    }
                    // A completed but unconsumed result still owns its reserved bookkeeping.
                    let mut excess =
                        reactor.send_reserved(fd.clone(), buffer(b"y"), capacity.clone(), &request);
                    assert!(matches!(
                        poll(&mut excess),
                        Poll::Ready(Err(Error::Overloaded))
                    ));
                    assert!(matches!(poll(&mut send), Poll::Ready(Ok(_))));
                    let mut events = String::new();
                    failures.write_gate_events(&mut events).unwrap();
                    assert!(
                        events.contains("reason=OrdinaryFull kind=Readiness events=1"),
                        "{events}"
                    );
                    assert!(
                        events.contains("reason=ReservedFull kind=OtherIo events=1"),
                        "{events}"
                    );
                    drop(send);
                    // Successful admission below asserts the previous reply released its slot.
                    let mut receive =
                        reactor.recv_reserved(fd, buffer(&[0]), capacity.clone(), &request);
                    assert!(poll(&mut receive).is_pending());
                    let weak = Rc::downgrade(&capacity);
                    drop((receive, capacity, ordinary));
                    assert!(
                        weak.upgrade().is_some(),
                        "abandoned I/O must retain the partition"
                    );
                    assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 1);
                    drive(&reactor, reactor.drain()).unwrap();
                    assert!(weak.upgrade().is_none());
                    assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
                    use std::io::Read;
                    let mut byte = [0];
                    peer.read_exact(&mut byte).unwrap();
                    assert_eq!(&byte, b"x");
                    let (slots, memory) = reserve(&reactor.admission);
                    assert!(matches!(
                        reactor.reserve_submissions(slots, memory),
                        Err(Error::Unavailable)
                    ));
                }
                #[test]
                fn attachment_cannot_overcommit_existing_ordinary_entries() {
                    let env = racer_io::Env::host();
                    let reactor =
                        kernel_reactor(&env, 2).expect("real io_uring reserved attachment");
                    let request = scope();
                    let (socket, _peer) = UnixStream::pair().unwrap();
                    let fd = Rc::new(Descriptor::from(socket));
                    let mut first = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
                    let mut second = reactor.readiness(fd, libc::POLLIN as u32, &request);
                    assert!(poll(&mut first).is_pending());
                    assert!(poll(&mut second).is_pending());
                    let (slots, memory) = reserve(&reactor.admission);
                    assert!(matches!(
                        reactor.reserve_submissions(slots, memory),
                        Err(Error::InvalidConfiguration)
                    ));
                    assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
                    drop((first, second));
                    drive(&reactor, reactor.drain()).unwrap();
                }
            }
        }

        #[cfg(test)]
        mod simulation_tests {
            use super::Reactor;
            use super::tests::drive;
            use super::tests::poll;
            use super::tests::scope;
            use crate::admission::AdmissionPolicy;
            use crate::admission::ResourceClass;
            use crate::error::Error;
            use crate::runtime::RequestScope;
            use racer_io::scoped::fs::prelude::*;
            use racer_io::sim::DiskState;
            use racer_io::sim::Fault;
            use racer_io::sim::SimOs;
            use std::cell::Cell;
            use std::ffi::CString;
            use std::path::Path;
            use std::rc::Rc;
            use std::time::Duration;
            fn reactor(env: &racer_io::Env) -> Reactor {
                Reactor::new(
                    Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                        crate::test_support::cluster::config(false).limits,
                    ))),
                    env,
                )
            }
            #[test]
            fn simulated_pipe_splice_preserves_suffix_under_backpressure() {
                let sim = SimOs::new();
                let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let pool = crate::http::new_pipe_pool(admission, env.sim_os().cloned());
                let mut pipe = pool.acquire().unwrap();
                let (a, b) = sim.socket_pair();
                sim.set_stream_capacity(2).unwrap();
                pipe.try_write(b"abc").unwrap();
                assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 2);
                assert_eq!(pipe.buffered(), 1);
                assert_eq!(
                    pipe.try_splice_descriptor(&a, 3).unwrap_err().kind(),
                    std::io::ErrorKind::WouldBlock
                );
                let b_owner = b;
                let b = b_owner.as_sim().unwrap();
                let mut bytes = [0; 2];
                b.recv(&mut bytes).unwrap();
                assert_eq!(&bytes, b"ab");
                assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 1);
                assert_eq!(b.recv(&mut bytes).unwrap(), 1);
                assert_eq!(bytes[0], b'c');
            }
            #[test]
            fn direct_io_faults_check_address_offset_and_length_independently() {
                use page_alloc::AlignedBuffer;
                use page_alloc::Alignment;
                use racer_io::ring::IoBuffer;
                struct View {
                    buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
                    start: usize,
                    length: usize,
                }
                // SAFETY: fixed range retains independently allocated aligned storage.
                unsafe impl IoBuffer for View {
                    fn bytes(&self) -> racer_io::Result<&[u8]> {
                        Ok(&self.buffer.as_slice()[self.start..self.start + self.length])
                    }
                    fn bytes_mut(&mut self) -> racer_io::Result<&mut [u8]> {
                        Ok(&mut self.buffer.as_mut_slice()[self.start..self.start + self.length])
                    }
                }
                let (sim, _env, r, scope) = setup();
                assert_eq!(
                    Alignment::new(0, 4096, 4096).map_err(Error::from),
                    Err(Error::DirectIoUnsupported)
                );
                let alignment = Alignment::new(4096, 4096, 4096).unwrap();
                let path = Path::new("/direct");
                let fd = Rc::new(
                    sim.open(None, path, libc::O_CREAT | libc::O_RDWR | libc::O_DIRECT)
                        .unwrap(),
                );
                for (offset, start, length) in
                    [(1, 0, 4096), (0, 1, 4096), (0, 0, 4095), (4096, 0, 4096)]
                {
                    let quota = r
                        .admission
                        .reserve(None, ResourceClass::Ciphertext, 8192)
                        .unwrap();
                    let mut buffer = alignment.allocate(8192, quota).unwrap();
                    buffer.bytes_mut().unwrap().fill(7);
                    let result = drive(
                        &r,
                        r.write_at(
                            fd.clone(),
                            offset,
                            View {
                                buffer,
                                start,
                                length,
                            },
                            (),
                            &scope,
                        ),
                    );
                    if offset == 4096 {
                        assert_eq!(result.unwrap().bytes, 4096);
                    } else {
                        assert!(matches!(result, Err(Error::Os(libc::EINVAL))));
                        assert_eq!(
                            drive(&r, r.file_stat(fd.clone(), &scope)).unwrap().stx_size,
                            0
                        );
                    }
                    assert_eq!(r.admission.used(ResourceClass::Ciphertext), 0);
                    assert_eq!(r.in_flight(), 0);
                }
                drive(&r, r.file_sync(fd.clone(), &scope)).unwrap();
                sim.disk().sync(Path::new("/")).unwrap();
                drop(fd);
                sim.disk().crash().unwrap();
                let result = drive(
                    &r,
                    r.read_at(
                        Rc::new(sim.open(None, path, libc::O_RDWR).unwrap()),
                        4096,
                        r.file_buffer(4096).unwrap(),
                        (),
                        &scope,
                    ),
                )
                .unwrap();
                assert_eq!(result.bytes, 4096);
                assert_eq!(result.buffer.prefix(4096).unwrap(), &[7; 4096]);
            }
            fn setup() -> (SimOs, racer_io::Env, Reactor, RequestScope) {
                let sim = SimOs::new();
                let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                let (reactor, scope) = (reactor(&env), scope());
                (sim, env, reactor, scope)
            }
            struct Probe(Rc<Cell<usize>>);
            impl Drop for Probe {
                fn drop(&mut self) {
                    self.0.set(self.0.get() + 1);
                }
            }
            #[test]
            fn candidate_total_cancels_pending_receive_without_bytes_and_retains_both_fences() {
                for cancel_first in [false, true] {
                    let clock = racer_io::SimClock::new(99);
                    let sim_env = clock.ctx(0);
                    let env = racer_io::Env::new(racer_io::Driver::Uring, sim_env.clone());
                    let sim_ctx = sim_env.clone();
                    let sim = SimOs::new();
                    sim.set_cancel_first(cancel_first);
                    let env = racer_io::Env::sim(&sim, env.ctx.clone());
                    let r = reactor(&env);
                    r.init().unwrap();
                    let baseline = r.admission.used(ResourceClass::RequestContext);
                    let start = sim_ctx.clock.now();
                    let request = RequestScope::on(
                        &sim_ctx.clock,
                        crate::model::RequestId([99; 16]),
                        start + Duration::from_secs(60),
                    )
                    .unwrap();
                    request
                        .set_candidate_total(start + Duration::from_secs(3))
                        .unwrap();
                    request.set_candidate_idle(Duration::from_secs(10)).unwrap();
                    let (fd, peer) = sim.socket_pair();
                    let fd = Rc::new(fd);
                    let weak = Rc::downgrade(&fd);
                    let drops = Rc::new(Cell::new(0));
                    let mut recv = r.recv(
                        fd,
                        r.file_buffer(8).unwrap(),
                        Probe(drops.clone()),
                        &request,
                    );
                    assert!(poll(&mut recv).is_pending());
                    clock.advance(Duration::from_secs(2));
                    request.candidate_progress().unwrap();
                    assert_eq!(r.poll_budgeted(1), Ok(0));
                    clock.advance(Duration::from_secs(1));
                    assert_eq!(r.poll_budgeted(1), Ok(0));
                    assert!(!request.cancellation.is_canceled());
                    assert_eq!(request.deadline.0, start + Duration::from_secs(60));
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(poll(&mut recv).is_pending());
                    assert_eq!(drops.get(), 0);
                    assert!(weak.upgrade().is_some());
                    assert_eq!(r.in_flight(), 1);
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(matches!(
                        poll(&mut recv),
                        std::task::Poll::Ready(Err(Error::DeadlineExceeded))
                    ));
                    drop(recv);
                    assert_eq!(drops.get(), 1);
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.in_flight(), 0);
                    assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
                    drop(peer);
                    assert_eq!(sim.live_handles(), 0);
                }
            }
            #[test]
            fn immutable_ciphertext_send_shares_backing_and_retains_it_through_cancel_fences() {
                use crate::model::CacheKey;
                use crate::model::ObjectId;
                use crate::model::ObjectVersion;
                use crate::model::StrongEtag;
                use crate::model::VersionMetadata;
                for cancel_first in [false, true] {
                    let sim = SimOs::new();
                    sim.set_cancel_first(cancel_first);
                    let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                    let r = reactor(&env);
                    let scope = scope();
                    let bundle = crate::memory::tests::bundle_for(
                        &r.admission,
                        VersionMetadata {
                            content_type: None,
                            version: ObjectVersion {
                                object: ObjectId {
                                    cache: racer_wire::control::CacheId("cache".into()),
                                    key: CacheKey([0; 32]),
                                },
                                etag: StrongEtag::test_value("v1"),
                            },
                            length: 3,
                        },
                    );
                    let page = bundle.ciphertext.clone();
                    drop(bundle);
                    let weak = std::sync::Arc::downgrade(&page.inner);
                    let pointer = page.bytes().as_ptr();
                    let (fd, peer) = sim.socket_pair();
                    let fd = Rc::new(fd);
                    sim.set_max_chunk(3).unwrap();
                    let completed =
                        drive(&r, r.send(fd.clone(), page.clone(), (), &scope)).unwrap();
                    assert_eq!(completed.bytes, 3);
                    assert_eq!(completed.buffer.bytes().as_ptr(), pointer);
                    assert_eq!(page.bytes(), &[2; 19]);
                    assert_eq!(
                        r.admission.used(ResourceClass::Ciphertext),
                        page_alloc::buf::GRANULE
                    );
                    let mut received = [0; 3];
                    assert_eq!(peer.try_recv(&mut received).unwrap(), 3);
                    assert_eq!(received, [2; 3]);
                    drop(completed);
                    sim.inject("send", Fault::Delay(20)).unwrap();
                    let mut send = r.send(fd, page, (), &scope);
                    assert!(poll(&mut send).is_pending());
                    // Abandonment requests the cancel at once; both fences follow.
                    drop(send);
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(weak.upgrade().is_some());
                    assert_eq!(
                        r.admission.used(ResourceClass::Ciphertext),
                        page_alloc::buf::GRANULE
                    );
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.admission.used(ResourceClass::Ciphertext), 0);
                }
            }
            #[test]
            fn real_slab_open_is_sparse_exclusive_and_checks_direct_geometry() {
                use page_alloc::Slab;
                let sim = SimOs::new();
                let env = racer_io::Env::sim(&sim, racer_io::WorkerCtx::host());
                let r = Rc::new(reactor(&env));
                let slabs = Slab::<flow_control::Charge<AdmissionPolicy>>::new(
                    "/slabs/worker-0-slab-0.dat".into(),
                    64 * 1024 * 1024,
                    32 * 1024 * 1024,
                    crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
                );
                slabs.simulate_in(sim.clone());
                assert!(slabs.open_now().is_ok());
                let other = Slab::<flow_control::Charge<AdmissionPolicy>>::new(
                    "/slabs/worker-0-slab-0.dat".into(),
                    64 * 1024 * 1024,
                    32 * 1024 * 1024,
                    crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
                );
                other.simulate_in(sim.clone());
                assert_eq!(
                    other.open_now().map_err(Error::from),
                    Err(Error::Unavailable)
                );
                assert_eq!(
                    other.open_now().map_err(Error::from),
                    Err(Error::Unavailable)
                );
                drop(slabs);
                assert!(other.open_now().is_ok());
                let path = Path::new("/slabs/worker-0-slab-0.dat");
                let file = Rc::new(sim.open(None, path, libc::O_RDWR | libc::O_DIRECT).unwrap());
                let scope = scope();
                assert!(matches!(
                    drive(
                        &r,
                        r.write_at(file, 1, r.file_bytes(b"bad").unwrap(), (), &scope)
                    ),
                    Err(Error::Os(libc::EINVAL))
                ));
                let file = sim.open(None, path, libc::O_RDONLY).unwrap();
                assert_eq!(
                    file.as_sim().unwrap().stat().unwrap().stx_size,
                    64 * 1024 * 1024
                );
                assert_eq!(
                    sim.disk().read(path, 0, 4096, DiskState::Volatile).unwrap(),
                    vec![0; 4096]
                );
            }
            #[test]
            fn projected_directory_rotation_and_private_atomic_writes_use_real_filesystem_calls() {
                let (sim, _env, r, scope) = setup();
                sim.write_file(Path::new("/projected/epoch-a/bundle"), b"first")
                    .unwrap();
                sim.symlink(Path::new("epoch-a"), Path::new("/projected/..data"))
                    .unwrap();
                let bytes = drive(
                    &r,
                    Box::pin(crate::test_support::projected_file(
                        &r,
                        Path::new("/projected"),
                        "bundle",
                        64,
                        &scope,
                    )),
                )
                .unwrap();
                assert_eq!(&*bytes, b"first");
                let private = drive(
                    &r,
                    Box::pin(racer_io::scoped::fs::secure::directory(
                        &r,
                        Path::new("/private"),
                        true,
                        true,
                        &scope,
                    )),
                )
                .unwrap();
                sim.inject("write", Fault::Short(2)).unwrap();
                drive(
                    &r,
                    Box::pin(async {
                        racer_io::scoped::fs::secure::atomic_write(
                            &r, &private, "identity", b"secret", &scope,
                        )
                        .await
                        .map_err(Error::from)
                    }),
                )
                .unwrap();
                assert_eq!(
                    sim.read_file(Path::new("/private/identity")).unwrap(),
                    b"secret"
                );
                sim.symlink(Path::new("/private"), Path::new("/projected/escape"))
                    .unwrap();
                assert!(
                    drive(
                        &r,
                        r.file_open(
                            None,
                            CString::new("/projected/escape").unwrap(),
                            libc::O_RDONLY,
                            4,
                            &scope
                        )
                    )
                    .is_err()
                );
            }
        }
    }

    #[test]
    fn test_collection_iteration_is_fixed_and_consumes_no_nonce_entropy() {
        let env = racer_io::Env::host();
        fn sample(seed: u64) -> (Vec<u64>, [u8; 32]) {
            let clock = racer_io::SimClock::new(seed);
            let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(9));
            let mut map = HashMap::default();
            for id in 0..100 {
                map.insert(id, id);
            }
            for id in (0..100).step_by(3) {
                map.remove(&id);
            }
            let mut bytes = [0; 32];
            env.ctx.rng.fill(&mut bytes).unwrap();
            (map.into_keys().collect(), bytes)
        }
        assert_eq!(sample(1), sample(1));
        assert_eq!(sample(1).0, sample(7).0);
        let clock = racer_io::SimClock::new(1);
        let env = racer_io::Env::new(env.driver.clone(), clock.ctx(9));
        let mut bytes = [0; 32];
        env.ctx.rng.fill(&mut bytes).unwrap();
        assert_eq!(sample(1).1, bytes);
    }

    #[test]
    fn replay_and_nested_worlds_preserve_time_entropy_and_deadlines() {
        let clock = racer_io::SimClock::new(7);
        let role = clock.ctx(11);
        let env = racer_io::Env::new(racer_io::Driver::Uring, role.clone());
        let start = env.ctx.now();
        let wall = env.ctx.clock.wall_now();
        let scope = RequestScope::on(
            &env.ctx.clock,
            RequestId([1; 16]),
            start + Duration::from_secs(2),
        )
        .unwrap();
        let wire = crate::peer::protocol::encode_deadline(scope.clock(), scope.deadline).unwrap();
        let mut first = [0; 64];
        env.ctx.rng.fill(&mut first[..3]).unwrap();
        env.ctx.rng.fill(&mut first[3..]).unwrap();
        {
            let replay = racer_io::SimClock::new(7);
            let env = racer_io::Env::new(env.driver.clone(), replay.ctx(11));
            let mut bytes = [0; 64];
            env.ctx.rng.fill(&mut bytes[..3]).unwrap();
            env.ctx.rng.fill(&mut bytes[3..]).unwrap();
            assert_eq!(bytes, first);
            assert_eq!(env.ctx.clock.wall_now(), wall);
            replay.advance(Duration::from_secs(500));
        }
        assert_eq!(env.ctx.now(), start);
        clock.advance(Duration::from_secs(2));
        assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
        clock.set_wall_time(wall - Duration::from_secs(60));
        assert_eq!(env.ctx.now(), start + Duration::from_secs(2));
        assert_eq!(
            crate::peer::protocol::encode_deadline(scope.clock(), scope.deadline).unwrap(),
            wire
        );
        assert_eq!(
            crate::peer::protocol::decode_deadline(&env.ctx.clock, wire)
                .unwrap()
                .0,
            scope.deadline.0
        );
    }
    mod listener_tests {
        use crate::error::Error;
        use crate::model::RequestId;
        use crate::runtime::RequestScope;
        use crate::test_support::WakeCounter;
        use std::cell::Cell;
        use std::sync::Arc;
        use std::task::Context;
        use std::task::Poll;
        use std::task::Waker;
        use std::time::Duration;

        /// A simulated reactor on `clock`'s worker context.
        fn sim_reactor(clock: &racer_io::SimClock) -> racer_io::scoped::Reactor<RequestScope> {
            racer_io::scoped::Reactor::new(
                8,
                &racer_io::Env::sim(&racer_io::sim::SimOs::new(), clock.ctx(0)),
            )
        }

        #[test]
        fn repeated_pressure_is_rate_limited_without_self_wakes() {
            let clock = racer_io::SimClock::new(1);
            let environment = clock.ctx(0);
            let env = racer_io::Env::new(racer_io::Driver::Uring, environment.clone());
            let r = sim_reactor(&clock);
            let scope = RequestScope::on(
                &env.ctx.clock,
                RequestId([0; 16]),
                env.ctx.now() + Duration::from_secs(1),
            )
            .unwrap();
            let calls = Cell::new(0);
            let wakes = Arc::new(WakeCounter::default());
            let waker = Waker::from(wakes.clone());
            let mut cx = Context::from_waker(&waker);
            let mut operation = r.retry_overloaded(&scope, || {
                calls.set(calls.get() + 1);
                Box::pin(std::future::ready(if calls.get() <= 4 {
                    Err(Error::Overloaded)
                } else {
                    Ok(7)
                }))
            });
            for attempt in 1..=4 {
                for _ in 0..100 {
                    assert!(operation.as_mut().poll(&mut cx).is_pending());
                }
                assert_eq!(calls.get(), attempt);
                clock.advance(Duration::from_millis(9));
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                assert_eq!(calls.get(), attempt);
                clock.advance(Duration::from_millis(1));
            }
            assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(7)));
            assert_eq!(wakes.count(), 0);
        }
        #[test]
        fn backoff_preserves_shutdown_deadlines_and_fatal_errors() {
            let clock = racer_io::SimClock::new(2);
            let environment = clock.ctx(0);
            let env = racer_io::Env::new(racer_io::Driver::Uring, environment.clone());
            let r = sim_reactor(&clock);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for expected in [Error::Canceled, Error::DeadlineExceeded] {
                let scope = RequestScope::on(
                    &env.ctx.clock,
                    RequestId([0; 16]),
                    env.ctx.now() + Duration::from_millis(1),
                )
                .unwrap();
                let mut operation = r.retry_overloaded::<()>(&scope, || {
                    Box::pin(std::future::ready(Err(Error::Overloaded)))
                });
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                if expected == Error::Canceled {
                    scope.cancel().unwrap();
                } else {
                    clock.advance(Duration::from_millis(1));
                }
                assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(expected)));
            }
            let scope = RequestScope::on(
                &env.ctx.clock,
                RequestId([0; 16]),
                env.ctx.now() + Duration::from_secs(1),
            )
            .unwrap();
            for error in [
                Error::Io,
                Error::Unavailable,
                Error::InvalidConfiguration,
                Error::Internal,
            ] {
                let mut operation =
                    r.retry_overloaded::<()>(&scope, || Box::pin(std::future::ready(Err(error))));
                assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
            }
        }
        #[test]
        fn submitted_listener_cancellation_waits_for_cqe_fence() {
            use crate::admission::AdmissionPolicy;
            use crate::runtime::Reactor;
            use racer_io::sim::SimOs;
            use std::rc::Rc;
            let sim = SimOs::new();
            let clock = racer_io::SimClock::new(3);
            let environment = clock.ctx(0);
            let env = racer_io::Env::sim(&sim, environment.clone());
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Reactor::new(admission, &env);
            let scope = RequestScope::on(
                &env.ctx.clock,
                RequestId([0; 16]),
                env.ctx.now() + Duration::from_secs(1),
            )
            .unwrap();
            let (reader, _writer) = sim.socket_pair();
            let reader = Rc::new(reader);
            let mut operation = reactor.retry_overloaded(&scope, || {
                reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope)
            });
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            scope.cancel().unwrap();
            // Cancellation is not a kernel ownership fence.
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            for _ in 0..4 {
                reactor.poll_budgeted(8).unwrap();
            }
            assert_eq!(
                operation.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Canceled))
            );
            assert_eq!(reactor.in_flight(), 0);
        }
    }
}
