//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

pub mod affinity;
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
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::RequestId;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
use std::time::Instant;
use uring_runtime::deadline::CancellationRegistration;
use uring_runtime::deadline::Deadline;

/// Racer candidate policy shares the cancellation lifetime, but is not runtime policy.
#[derive(Clone)]
pub struct Cancellation {
    inner: uring_runtime::deadline::Cancellation,
    state: Arc<CancellationState>,
}
struct CancellationState {
    candidate_total: OnceLock<Instant>,
    candidate_idle: OnceLock<Mutex<(std::time::Duration, Instant)>>,
    candidate_body: OnceLock<Mutex<CandidateBody>>,
}
struct CandidateBody {
    observation: std::time::Duration,
    complete_by: Instant,
    first: Option<(Instant, usize)>,
    expired: bool,
}
impl Cancellation {
    pub fn new() -> Result<Self> {
        Ok(Self {
            inner: uring_runtime::deadline::Cancellation::new()?,
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
            .set(Mutex::new(CandidateBody {
                observation,
                complete_by,
                first: None,
                expired: false,
            }))
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
            if received == total {
                body.first = None;
                return Ok(());
            }
            let (first, initial) = *body.first.get_or_insert((now, received));
            let elapsed = now.saturating_duration_since(first).as_nanos();
            let delivered = received.saturating_sub(initial) as u128;
            let remaining = (total - received) as u128;
            let available = body.complete_by.saturating_duration_since(now).as_nanos();
            if now >= body.complete_by
                || (elapsed >= body.observation.as_nanos()
                    && remaining.saturating_mul(elapsed) > delivered.saturating_mul(available))
            {
                body.expired = true;
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
                if body.expired
                    || (body.first.is_some()
                        && uring_runtime::environment::now() >= body.complete_by)
                {
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

    fn cancellation(&self) -> Option<&uring_runtime::deadline::Cancellation> {
        Some(&self.cancellation.inner)
    }
}
impl rest_client::Scope for RequestScope {
    fn deadline(&self) -> Instant {
        self.deadline.0
    }

    fn narrowed(&self, until: Instant) -> Self {
        let mut scope = self.clone();
        scope.deadline.0 = scope.deadline.0.min(until);
        scope
    }
}
use crate::admission::AdmissionPolicy;
use crate::admission::ConnectionReservation;
use crate::admission::reserve_connection;
use crate::model::ResourceClass;
use std::ops::Deref;
use std::rc::Rc;
use uring_runtime::reactor::ReactorWake;
use uring_runtime::reactor::SUBMISSION_BYTES;
use uring_runtime::reactor::SubmissionCapacity;
use uring_runtime::reactor::filesystem::Buffer;

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
/// use uring_runtime::reactor::Descriptor;
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
pub mod worker;

/// Yield one cooperative turn without retaining an executor or I/O owner.
pub(crate) async fn cooperative_turn() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            std::task::Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    })
    .await
}
#[cfg(test)]
mod tests {
    mod deadline;
    mod reactor;
    use super::HashMap;
    use super::HashState;
    use crate::error::Error;
    use crate::model::RequestId;
    use crate::runtime::RequestScope;
    use std::time::Duration;
    use uring_runtime::environment::*;

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
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;
        use uring_runtime::retry_listener as retry;

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
