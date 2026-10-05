//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::admission::ResourceClass;
use crate::admission::reserve_connection;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::RequestId;
use std::ops::Deref;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
use std::time::Instant;
use uring_runtime::environment::CancellationRegistration;
use uring_runtime::environment::Deadline;
use uring_runtime::reactor::ReactorWake;
use uring_runtime::reactor::SUBMISSION_BYTES;
use uring_runtime::reactor::SubmissionCapacity;
use uring_runtime::reactor::filesystem::Buffer;

// Seeded hashing only in simulated worlds; production retains std hashing.
#[cfg(not(test))]
pub(crate) type HashMap<K, V> = std::collections::HashMap<K, V>;
#[cfg(not(test))]
pub(crate) type HashSet<K> = std::collections::HashSet<K>;
#[cfg(test)]
pub(crate) type HashMap<K, V> = std::collections::HashMap<K, V, HashState>;
#[cfg(test)]
pub(crate) type HashSet<K> = std::collections::HashSet<K, HashState>;
#[cfg(test)]
#[derive(Clone, Debug)]
pub(crate) enum HashState {
    Real(std::collections::hash_map::RandomState),
    Simulated(u64),
}
#[cfg(test)]
impl Default for HashState {
    fn default() -> Self {
        match uring_runtime::environment::simulation_seed() {
            Some(seed) => Self::Simulated(seed),
            None => Self::Real(std::collections::hash_map::RandomState::new()),
        }
    }
}
#[cfg(test)]
impl std::hash::BuildHasher for HashState {
    type Hasher = std::collections::hash_map::DefaultHasher;
    fn build_hasher(&self) -> Self::Hasher {
        use std::hash::Hasher;
        match self {
            Self::Real(state) => state.build_hasher(),
            Self::Simulated(seed) => {
                let mut hasher = Self::Hasher::new();
                hasher.write_u64(*seed);
                hasher
            }
        }
    }
}

/// Racer candidate policy shares the cancellation lifetime, but is not runtime policy.
#[derive(Clone)]
pub struct Cancellation {
    inner: uring_runtime::environment::Cancellation,
    state: Arc<CancellationState>,
}
struct CancellationState {
    candidate_total: OnceLock<Instant>,
    candidate_idle: OnceLock<Mutex<(std::time::Duration, Instant)>>,
    candidate_body: OnceLock<Mutex<CandidateBody>>,
}
type CandidateBody = flow_control::progress::ProgressBudget;
impl Cancellation {
    pub fn new() -> Result<Self> {
        Ok(Self {
            inner: uring_runtime::environment::Cancellation::new()?,
            state: Arc::new(CancellationState {
                candidate_total: OnceLock::new(),
                candidate_idle: OnceLock::new(),
                candidate_body: OnceLock::new(),
            }),
        })
    }
    pub fn is_cancelled(&self) -> bool {
        self.inner.is_cancelled()
    }
    /// Subscribe for one operation's lifetime, even when several operations share
    /// an executor waker. Dropping the subscription reclaims its capacity and wake.
    pub fn subscribe(&self) -> Result<CancellationRegistration> {
        self.inner.subscribe().map_err(Into::into)
    }
    pub fn cancel(&self) -> Result<()> {
        self.inner.cancel().map_err(Into::into)
    }
}
#[derive(Clone)]
pub struct RequestScope {
    pub request: RequestId,
    pub deadline: Deadline,
    pub cancellation: Cancellation,
    /// Local diagnostics only: original acquisition ceiling and candidate share.
    /// Never serialized or consulted by deadline/cancellation policy.
    pub(crate) body_deadlines: Option<(Instant, Instant)>,
}
impl RequestScope {
    /// Local peer-exchange cap, including checkout and verification in the exchange.
    /// Set once; phase/body progress cannot renew it or change signed authority.
    pub(crate) fn set_candidate_total(&self, expires: Instant) -> Result<()> {
        self.check()?;
        // A cap may elapse while setting up the exchange. Install it as expired,
        // not invalid input; the next scope check rejects work without renewal.
        if expires > self.deadline.0 {
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
            .set(Mutex::new((
                allowance,
                uring_runtime::environment::now() + allowance,
            )))
            .map_err(|_| Error::Internal)?;
        Ok(())
    }

    pub(crate) fn candidate_progress(&self) -> Result<()> {
        self.check()?;
        if let Some(idle) = self.cancellation.state.candidate_idle.get() {
            let mut idle = idle.lock().map_err(|_| Error::Unavailable)?;
            idle.1 = uring_runtime::environment::now() + idle.0;
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
        if observation.is_zero() || complete_by > self.deadline.0 {
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
        self.candidate_progress()?;
        if received > total {
            return Err(Error::InvalidRequest);
        }
        if let Some(body) = self.cancellation.state.candidate_body.get() {
            let now = uring_runtime::environment::now();
            let mut body = body.lock().map_err(|_| Error::Unavailable)?;
            if !body.advance(now, received, total) {
                return Err(Error::DeadlineExceeded);
            }
        }
        Ok(())
    }

    pub fn new(request: RequestId, deadline: Instant) -> Result<Self> {
        Ok(Self {
            request,
            deadline: Deadline(deadline),
            cancellation: Cancellation::new()?,
            body_deadlines: None,
        })
    }
    pub fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            Err(Error::Cancelled)
        } else if uring_runtime::environment::now() >= self.deadline.0 {
            Err(Error::DeadlineExceeded)
        } else {
            if let Some(expires) = self.cancellation.state.candidate_total.get()
                && uring_runtime::environment::now() >= *expires
            {
                return Err(Error::DeadlineExceeded);
            }
            if let Some(body) = self.cancellation.state.candidate_body.get() {
                let body = body.lock().map_err(|_| Error::Unavailable)?;
                if body.expired(uring_runtime::environment::now()) {
                    return Err(Error::DeadlineExceeded);
                }
            }
            if let Some(idle) = self.cancellation.state.candidate_idle.get()
                && uring_runtime::environment::now()
                    >= idle.lock().map_err(|_| Error::Unavailable)?.1
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
impl uring_runtime::Scope for RequestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        RequestScope::check(self)
    }

    fn cancellation(&self) -> Option<&uring_runtime::environment::Cancellation> {
        Some(&self.cancellation.inner)
    }
}
impl wire_codec::rest::Scope for RequestScope {
    fn deadline(&self) -> Instant {
        self.deadline.0
    }

    fn narrowed(&self, until: Instant) -> Self {
        let mut scope = self.clone();
        scope.deadline.0 = scope.deadline.0.min(until);
        scope
    }
}

pub struct AdmissionBudget(Rc<flow_control::Quotas<AdmissionPolicy>>);
impl uring_runtime::Budget for AdmissionBudget {
    type Charge = flow_control::Charge<AdmissionPolicy>;
    fn charge(&self, bytes: usize) -> uring_runtime::Result<flow_control::Charge<AdmissionPolicy>> {
        self.0
            .reserve_completion(None, ResourceClass::RequestContext, bytes)
            .map_err(|error| match error {
                flow_control::Error::InvalidInput => uring_runtime::Error::InvalidConfiguration,
                flow_control::Error::Unavailable => uring_runtime::Error::Unavailable,
                _ => uring_runtime::Error::Overloaded,
            })
    }
}

/// Racer admission and request policy around the worker-local runtime reactor.
///
/// Inline storage cannot meet the stable-buffer contract:
/// ```compile_fail
/// use racer_dataplane::error::Result;
/// use uring_runtime::reactor::IoBuffer;
/// struct Inline([u8; 16]);
/// impl IoBuffer for Inline {
///     type Error = racer_dataplane::error::Error;
///     fn bytes(&self) -> Result<&[u8]> { Ok(&self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(&mut self.0) }
/// }
/// ```
/// Borrowed storage does not have an independent completion lifetime:
/// ```compile_fail
/// use racer_dataplane::error::Result;
/// use uring_runtime::reactor::IoBuffer;
/// struct Borrowed<'a>(&'a mut [u8]);
/// unsafe impl IoBuffer for Borrowed<'_> {
///     type Error = racer_dataplane::error::Error;
///     fn bytes(&self) -> Result<&[u8]> { Ok(self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(self.0) }
/// }
/// ```
/// Audited production buffers own independent storage:
/// ```
/// use racer_dataplane::{memory::PlaintextBuffer, admission::AdmissionPolicy};
/// use uring_runtime::reactor::IoBuffer;
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
/// use racer_dataplane::{memory::CiphertextPage, runtime::{Reactor, RequestScope}};
/// use uring_runtime::reactor::descriptor::Descriptor;
/// fn receive(r: &Reactor, fd: Rc<Descriptor>, page: CiphertextPage, scope: &RequestScope) {
///     let _ = r.recv(fd, page, (), scope);
/// }
/// ```
pub struct Reactor {
    core: uring_runtime::reactor::Reactor<RequestScope, AdmissionBudget>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}
impl Deref for Reactor {
    type Target = uring_runtime::reactor::Reactor<RequestScope, AdmissionBudget>;
    fn deref(&self) -> &Self::Target {
        &self.core
    }
}
impl Reactor {
    pub fn new(admission: Rc<flow_control::Quotas<AdmissionPolicy>>) -> Self {
        Self {
            core: uring_runtime::reactor::Reactor::new(
                admission.policy().limits().queue_entries.get(),
                AdmissionBudget(admission.clone()),
            ),
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
                uring_runtime::Error::InvalidInput => Error::InvalidConfiguration,
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
    use super::HashState;
    use crate::error::Error;
    use crate::model::RequestId;
    use crate::runtime::RequestScope;
    use std::time::Duration;
    use uring_runtime::environment::*;
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
            fn rest_narrowing_preserves_policy_cancellation_and_parent_deadline() {
                use wire_codec::rest::Scope;
                let clock = uring_runtime::environment::SimulationClock::new(102);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let end = start + Duration::from_secs(30);
                let scope = RequestScope::new(RequestId([102; 16]), end).unwrap();
                scope
                    .set_candidate_total(start + Duration::from_secs(10))
                    .unwrap();
                let narrowed = scope.narrowed(start + Duration::from_secs(20));
                assert_eq!(scope.deadline(), end);
                assert_eq!(narrowed.deadline(), start + Duration::from_secs(20));
                assert_eq!(narrowed.narrowed(end).deadline(), narrowed.deadline());
                clock.advance(Duration::from_secs(10));
                assert_eq!(narrowed.check(), Err(Error::DeadlineExceeded));
                scope.cancel().unwrap();
                assert_eq!(narrowed.check(), Err(Error::Cancelled));
            }

            #[test]
            fn candidate_total_survives_body_completion_and_never_changes_authority() {
                let clock = uring_runtime::environment::SimulationClock::new(97);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let scope = RequestScope::new(RequestId([97; 16]), start + Duration::from_secs(60))
                    .unwrap();
                let signed = crate::peer::protocol::encode_deadline(scope.deadline).unwrap();
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
                    crate::peer::protocol::encode_deadline(scope.deadline).unwrap(),
                    signed
                );
            }

            #[test]
            fn candidate_body_budget_accepts_original_ceiling_and_rejects_invalid_bounds() {
                let clock = uring_runtime::environment::SimulationClock::new(98);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let end = start + Duration::from_secs(30);
                let scope = RequestScope::new(RequestId([98; 16]), end).unwrap();
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
                let elapsed = RequestScope::new(RequestId([99; 16]), end).unwrap();
                elapsed.set_candidate_total(start).unwrap();
                assert_eq!(elapsed.check(), Err(Error::DeadlineExceeded));
            }

            #[test]
            fn candidate_body_reserve_keeps_healthy_progress_and_ignores_unknown_lengths() {
                let clock = uring_runtime::environment::SimulationClock::new(91);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let healthy =
                    RequestScope::new(RequestId([91; 16]), start + Duration::from_secs(30))
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
                assert!(uring_runtime::environment::now() > start + Duration::from_secs(10));
                assert_eq!(healthy.deadline.0, start + Duration::from_secs(30));
                // Completed network body does not subject later verification to the reserve.
                clock.advance(Duration::from_secs(7));
                healthy.check().unwrap();
                for reserve in [false, true] {
                    let now = uring_runtime::environment::now();
                    let scope =
                        RequestScope::new(RequestId([92; 16]), now + Duration::from_secs(30))
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
                let clock = uring_runtime::environment::SimulationClock::new(93);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let scope = RequestScope::new(RequestId([93; 16]), start + Duration::from_secs(30))
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
                assert!(uring_runtime::environment::now() < scope.deadline.0);
            }

            #[test]
            fn candidate_idle_progress_never_renews_hard_deadline_or_revives_expiry() {
                let clock = uring_runtime::environment::SimulationClock::new(39);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let scope =
                    RequestScope::new(RequestId([39; 16]), start + Duration::from_secs(3)).unwrap();
                scope.set_candidate_idle(Duration::from_secs(1)).unwrap();
                let signed = crate::peer::protocol::encode_deadline(scope.deadline).unwrap();
                for _ in 0..5 {
                    clock.advance(Duration::from_millis(500));
                    scope.candidate_progress().unwrap();
                    assert_eq!(
                        crate::peer::protocol::encode_deadline(scope.deadline).unwrap(),
                        signed
                    );
                }
                clock.advance(Duration::from_millis(500));
                assert_eq!(scope.candidate_progress(), Err(Error::DeadlineExceeded));
                let stalled =
                    RequestScope::new(RequestId([40; 16]), start + Duration::from_secs(10))
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
                let clock = uring_runtime::environment::SimulationClock::new(101);
                let _env = clock.environment(0).enter();
                let start = uring_runtime::environment::now();
                let scope =
                    RequestScope::new(RequestId([101; 16]), start + Duration::from_secs(30))
                        .unwrap();
                scope
                    .set_candidate_total(start + Duration::from_secs(10))
                    .unwrap();
                let clone = scope.clone();
                clock.advance(Duration::from_secs(10));
                assert_eq!(
                    uring_runtime::Scope::check(&clone),
                    Err(Error::DeadlineExceeded)
                );
                let count = Arc::new(Count(AtomicUsize::new(0)));
                let registration = uring_runtime::Scope::cancellation(&clone)
                    .unwrap()
                    .subscribe()
                    .unwrap();
                registration.register(&Waker::from(count.clone()));
                scope.cancel().unwrap();
                assert_eq!(uring_runtime::Scope::check(&clone), Err(Error::Cancelled));
                assert_eq!(count.0.load(Ordering::Relaxed), 1);
            }
            #[test]
            fn clones_preserve_deadline_and_wake_independent_waiters() {
                let scope =
                    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(2))
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
                assert_eq!(scope.check(), Err(Error::Cancelled));
                assert_eq!(a.0.load(Ordering::Relaxed), 1);
                assert_eq!(b.0.load(Ordering::Relaxed), 1);
                assert_eq!(
                    RequestScope::new(RequestId([1; 16]), Instant::now())
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
            use std::os::unix::net::UnixStream;
            use std::task::Context;
            use std::task::Poll;
            use std::time::Duration;
            use std::time::Instant;
            use uring_runtime::reactor::IoBuffer;
            use uring_runtime::reactor::SocketAddress;
            use uring_runtime::reactor::descriptor::Descriptor;
            use uring_runtime::reactor::simulation;

            #[test]
            fn direct_filesystem_buffers_preserve_nonempty_boundary_and_charge() {
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(4))));
                let reactor = Reactor::new(admission.clone());
                assert!(matches!(
                    reactor.file_buffer(0),
                    Err(Error::InvalidConfiguration)
                ));
                assert!(matches!(
                    reactor.file_bytes(b""),
                    Err(Error::InvalidConfiguration)
                ));
                assert_eq!(admission.used(ResourceClass::RequestContext), 0);
                let mut buffer: uring_runtime::reactor::filesystem::Buffer =
                    reactor.file_bytes(b"abc").unwrap();
                assert_eq!(buffer.prefix(3).unwrap(), b"abc");
                assert!(buffer.prefix(4).is_err());
                assert!(buffer.advance(4).is_err());
                buffer.advance(1).unwrap();
                assert_eq!(buffer.remaining(), 2);
                assert_eq!(buffer.bytes().unwrap(), b"bc");
                assert!(admission.used(ResourceClass::RequestContext) >= 3);
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
                fn check<B: IoBuffer>(buffer: B)
                where
                    Error: From<B::Error>,
                    B::Error: std::fmt::Debug,
                {
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
                check(WireBuffer::new(&admission, 3).unwrap());
                let pool = BufferPool::new(admission.clone());
                check(
                    pool.plaintext(
                        admission
                            .reserve(
                                Some(&racer_control_wire::CacheId("provenance".into())),
                                ResourceClass::Plaintext,
                                3,
                            )
                            .unwrap(),
                        3,
                    )
                    .unwrap(),
                );
                let reactor = Reactor::new(admission);
                check(reactor.file_buffer(3).unwrap());
            }

            fn limits(capacity: usize) -> crate::config::Limits {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.queue_entries = std::num::NonZeroUsize::new(capacity).unwrap();
                limits
            }
            pub(super) fn scope() -> RequestScope {
                RequestScope::new(
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
            pub(super) fn kernel_reactor(capacity: usize) -> Option<Reactor> {
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
                let reactor = Reactor::new(Rc::new(flow_control::Quotas::new(
                    AdmissionPolicy::new(limits(capacity)),
                )));
                reactor.init().unwrap();
                Some(reactor)
            }
            struct Buffer(Vec<u8>);
            // SAFETY: private fixed Vec owns stable exclusive backing.
            unsafe impl IoBuffer for Buffer {
                type Error = Error;
                fn bytes(&self) -> Result<&[u8]> {
                    Ok(&self.0)
                }
                fn bytes_mut(&mut self) -> Result<&mut [u8]> {
                    Ok(&mut self.0)
                }
            }
            fn buffer(bytes: &[u8]) -> Buffer {
                Buffer(bytes.into())
            }
            #[test]
            fn file_fence_selects_only_the_matching_racer_request() {
                let sim = simulation::Simulation::new();
                let _environment = sim.enter();
                let reactor = Reactor::new(Rc::new(flow_control::Quotas::new(
                    AdmissionPolicy::new(limits(4)),
                )));
                sim.write_file(std::path::Path::new("/one"), b"one")
                    .unwrap();
                sim.write_file(std::path::Path::new("/two"), b"two")
                    .unwrap();
                let first = scope();
                let second =
                    RequestScope::new(crate::model::RequestId([1; 16]), first.deadline.0).unwrap();
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
                assert!(matches!(poll(&mut a), Poll::Ready(Err(Error::Cancelled))));
                assert!(poll(&mut b).is_pending());
                drop((a, b));
                drive(&reactor, reactor.file_fence(second.request)).unwrap();
                assert_eq!(reactor.in_flight(), 0);
            }
            #[test]
            fn listener_retry_preserves_racer_policy_after_capacity_recovery() {
                let sim = simulation::Simulation::new();
                let _environment = sim.enter();
                let reactor = Reactor::new(Rc::new(flow_control::Quotas::new(
                    AdmissionPolicy::new(limits(1)),
                )));
                let request = scope();
                let (fd, _peer) = sim.socket_pair();
                let mut busy = reactor.readiness(Rc::new(fd), libc::POLLIN as u32, &request);
                assert!(poll(&mut busy).is_pending());
                let address = SocketAddress::Unix("/retry-listener".into());
                let listener = Rc::new(sim.listen(address.clone()).unwrap());
                let mut accept = uring_runtime::drivers::retry_listener(&request, || {
                    reactor.accept(listener.clone(), &request)
                });
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
            fn real_drain_io_preserves_control_capacity_after_admission_stop() {
                let Some(reactor) = kernel_reactor(2) else {
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
                    Poll::Ready(Err(Error::Cancelled))
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
                    let reactor = kernel_reactor(2).expect("real io_uring reserved submissions");
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
                    let reactor = kernel_reactor(2).expect("real io_uring reserved attachment");
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
            use crate::error::Result;
            use crate::runtime::RequestScope;
            use std::cell::Cell;
            use std::ffi::CString;
            use std::path::Path;
            use std::rc::Rc;
            use std::time::Duration;
            use uring_runtime::reactor::simulation::Environment;
            use uring_runtime::reactor::simulation::Fault;
            use uring_runtime::reactor::simulation::Simulation;
            use uring_runtime::reactor::simulation::disk::DiskState;
            fn reactor() -> Reactor {
                Reactor::new(Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ))))
            }
            #[test]
            fn simulated_pipe_splice_preserves_suffix_under_backpressure() {
                let sim = Simulation::new();
                let _environment = sim.enter();
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let pool = crate::http::new_pipe_pool(admission);
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
                let b = b.into_sim().unwrap();
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
                use uring_runtime::reactor::IoBuffer;
                struct View {
                    buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
                    start: usize,
                    length: usize,
                }
                // SAFETY: fixed range retains independently allocated aligned storage.
                unsafe impl IoBuffer for View {
                    type Error = Error;
                    fn bytes(&self) -> Result<&[u8]> {
                        Ok(&self.buffer.bytes()?[self.start..self.start + self.length])
                    }
                    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
                        Ok(&mut self.buffer.bytes_mut()?[self.start..self.start + self.length])
                    }
                }
                let (sim, _environment, r, scope) = setup();
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
            fn setup() -> (Simulation, Environment, Reactor, RequestScope) {
                let sim = Simulation::new();
                let environment = sim.enter();
                (sim, environment, reactor(), scope())
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
                    let clock = uring_runtime::environment::SimulationClock::new(99);
                    let _clock = clock.environment(0).enter();
                    let sim = Simulation::new();
                    sim.set_cancel_first(cancel_first);
                    let _environment = sim.enter();
                    let r = reactor();
                    r.init().unwrap();
                    let baseline = r.admission.used(ResourceClass::RequestContext);
                    let start = uring_runtime::environment::now();
                    let request = RequestScope::new(
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
                    assert!(!request.cancellation.is_cancelled());
                    assert_eq!(request.deadline.0, start + Duration::from_secs(60));
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(poll(&mut recv).is_pending());
                    assert_eq!(drops.get(), 0);
                    assert!(weak.upgrade().is_some());
                    assert_eq!(r.in_flight(), 1);
                    assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
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
                    let sim = Simulation::new();
                    sim.set_cancel_first(cancel_first);
                    let _environment = sim.enter();
                    let r = reactor();
                    let scope = scope();
                    let bundle = crate::memory::tests::bundle_for(
                        &r.admission,
                        VersionMetadata {
                            content_type: None,
                            version: ObjectVersion {
                                object: ObjectId {
                                    cache: racer_control_wire::CacheId("cache".into()),
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
                    assert_eq!(r.admission.used(ResourceClass::Ciphertext), 19);
                    let mut received = [0; 3];
                    assert_eq!(peer.try_recv(&mut received).unwrap(), 3);
                    assert_eq!(received, [2; 3]);
                    drop(completed);
                    sim.inject("send", Fault::Delay(20)).unwrap();
                    let mut send = r.send(fd, page, (), &scope);
                    assert!(poll(&mut send).is_pending());
                    drop(send);
                    assert_eq!(r.poll_budgeted(1), Ok(0));
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(weak.upgrade().is_some());
                    assert_eq!(r.admission.used(ResourceClass::Ciphertext), 19);
                    assert_eq!(r.poll_budgeted(1), Ok(1));
                    assert!(weak.upgrade().is_none());
                    assert_eq!(r.admission.used(ResourceClass::Ciphertext), 0);
                }
            }
            #[test]
            fn real_slab_open_is_sparse_exclusive_and_checks_direct_geometry() {
                use page_alloc::Slab;
                let sim = Simulation::new();
                let _environment = sim.enter();
                let r = Rc::new(reactor());
                let slabs = Slab::<flow_control::Charge<AdmissionPolicy>>::new(
                    "/slabs/worker-0-slab-0.dat".into(),
                    64 * 1024 * 1024,
                    32 * 1024 * 1024,
                    crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
                );
                assert!(slabs.open_now().is_ok());
                let other = Slab::<flow_control::Charge<AdmissionPolicy>>::new(
                    "/slabs/worker-0-slab-0.dat".into(),
                    64 * 1024 * 1024,
                    32 * 1024 * 1024,
                    crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
                );
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
                let (sim, _environment, r, scope) = setup();
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
                    Box::pin(uring_runtime::reactor::filesystem::secure::directory(
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
                        uring_runtime::reactor::filesystem::secure::atomic_write(
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
    fn simulation_replays_collection_iteration_without_consuming_nonce_entropy() {
        fn sample(seed: u64) -> (Vec<u64>, [u8; 32]) {
            let clock = SimulationClock::new(seed);
            let _role = clock.environment(9).enter();
            let mut map = HashMap::default();
            for id in 0..100 {
                map.insert(id, id);
            }
            for id in (0..100).step_by(3) {
                map.remove(&id);
            }
            let mut bytes = [0; 32];
            fill_random(&mut bytes).unwrap();
            (map.into_keys().collect(), bytes)
        }
        assert_eq!(sample(1), sample(1));
        assert_ne!(sample(1).0, sample(7).0);
        let clock = SimulationClock::new(1);
        let _role = clock.environment(9).enter();
        let mut bytes = [0; 32];
        fill_random(&mut bytes).unwrap();
        assert_eq!(sample(1).1, bytes);
        let _real = uring_runtime::environment::Environment::default().enter();
        assert!(matches!(HashState::default(), HashState::Real(_)));
    }

    #[test]
    fn replay_and_nested_worlds_preserve_time_entropy_and_deadlines() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let _guard = role.enter();
        let start = now();
        let wall = wall_now();
        let scope = RequestScope::new(RequestId([1; 16]), start + Duration::from_secs(2)).unwrap();
        let wire = crate::peer::protocol::encode_deadline(scope.deadline).unwrap();
        let mut first = [0; 64];
        fill_random(&mut first[..3]).unwrap();
        fill_random(&mut first[3..]).unwrap();
        {
            let replay = SimulationClock::new(7);
            let _nested = replay.environment(11).enter();
            let mut bytes = [0; 64];
            fill_random(&mut bytes).unwrap();
            assert_eq!(bytes, first);
            assert_eq!(wall_now(), wall);
            replay.advance(Duration::from_secs(500));
        }
        assert_eq!(now(), start);
        clock.advance(Duration::from_secs(2));
        assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
        clock.set_wall_time(wall - Duration::from_secs(60));
        assert_eq!(now(), start + Duration::from_secs(2));
        assert_eq!(
            crate::peer::protocol::encode_deadline(scope.deadline).unwrap(),
            wire
        );
        assert_eq!(
            crate::peer::protocol::decode_deadline(wire).unwrap().0,
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
        use uring_runtime::drivers::retry_listener as retry;
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;

        #[test]
        fn repeated_pressure_is_rate_limited_without_self_wakes() {
            let clock = SimulationClock::new(1);
            let environment = clock.environment(0);
            let _time = environment.enter();
            let scope = RequestScope::new(
                RequestId([0; 16]),
                environment::now() + Duration::from_secs(1),
            )
            .unwrap();
            let calls = Cell::new(0);
            let wakes = Arc::new(WakeCounter::default());
            let waker = Waker::from(wakes.clone());
            let mut cx = Context::from_waker(&waker);
            let mut operation = retry(&scope, || {
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
            let clock = SimulationClock::new(2);
            let environment = clock.environment(0);
            let _time = environment.enter();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for expected in [Error::Cancelled, Error::DeadlineExceeded] {
                let scope = RequestScope::new(
                    RequestId([0; 16]),
                    environment::now() + Duration::from_millis(1),
                )
                .unwrap();
                let mut operation = retry::<_, ()>(&scope, || {
                    Box::pin(std::future::ready(Err(Error::Overloaded)))
                });
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                if expected == Error::Cancelled {
                    scope.cancel().unwrap();
                } else {
                    clock.advance(Duration::from_millis(1));
                }
                assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(expected)));
            }
            let scope = RequestScope::new(
                RequestId([0; 16]),
                environment::now() + Duration::from_secs(1),
            )
            .unwrap();
            for error in [
                Error::Io,
                Error::Unavailable,
                Error::InvalidConfiguration,
                Error::Internal,
            ] {
                let mut operation =
                    retry::<_, ()>(&scope, || Box::pin(std::future::ready(Err(error))));
                assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
            }
        }
        #[test]
        fn submitted_listener_cancellation_waits_for_cqe_fence() {
            use crate::admission::AdmissionPolicy;
            use crate::runtime::Reactor;
            use std::rc::Rc;
            use uring_runtime::reactor::simulation::Simulation;
            let sim = Simulation::new();
            let _os = sim.enter();
            let clock = SimulationClock::new(3);
            let environment = clock.environment(0);
            let _time = environment.enter();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Reactor::new(admission);
            let scope = RequestScope::new(
                RequestId([0; 16]),
                environment::now() + Duration::from_secs(1),
            )
            .unwrap();
            let (reader, _writer) = sim.socket_pair();
            let reader = Rc::new(reader);
            let mut operation = retry(&scope, || {
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
                Poll::Ready(Err(Error::Cancelled))
            );
            assert_eq!(reactor.in_flight(), 0);
        }
    }
}
