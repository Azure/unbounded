//! Monotonic deadlines and bounded cancellation notification registrations.
use crate::error::Error;
use crate::error::Result;
use crate::model::RequestId;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
use std::time::Instant;
pub use uring_runtime::deadline::CancellationRegistration;
pub use uring_runtime::deadline::Deadline;

/// Racer candidate policy shares the cancellation lifetime, but is not runtime policy.
#[derive(Clone)]
pub struct Cancellation {
    inner: uring_runtime::deadline::Cancellation,
    state: Arc<State>,
}
struct State {
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
            state: Arc::new(State {
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
#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;
    use std::sync::atomic::Ordering;
    use std::task::Wake;
    use std::task::Waker;
    use std::time::Duration;
    #[test]
    fn rest_narrowing_preserves_policy_cancellation_and_parent_deadline() {
        use rest_client::Scope;
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
        let scope =
            RequestScope::new(RequestId([97; 16]), start + Duration::from_secs(60)).unwrap();
        let signed = crate::peer::protocol::encode_deadline(scope.deadline).unwrap();
        scope
            .set_candidate_total(start + Duration::from_secs(30))
            .unwrap();
        scope.set_candidate_idle(Duration::from_secs(10)).unwrap();
        scope
            .set_candidate_body_budget(Duration::from_secs(10), start + Duration::from_secs(20))
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
            scope.set_candidate_body_budget(Duration::from_secs(10), end + Duration::from_secs(1)),
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
            RequestScope::new(RequestId([91; 16]), start + Duration::from_secs(30)).unwrap();
        healthy.set_candidate_idle(Duration::from_secs(10)).unwrap();
        healthy
            .set_candidate_body_budget(Duration::from_secs(10), start + Duration::from_secs(20))
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
                RequestScope::new(RequestId([92; 16]), now + Duration::from_secs(30)).unwrap();
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
        let scope =
            RequestScope::new(RequestId([93; 16]), start + Duration::from_secs(30)).unwrap();
        scope.set_candidate_idle(Duration::from_secs(10)).unwrap();
        scope
            .set_candidate_body_budget(Duration::from_secs(10), start + Duration::from_secs(20))
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
        let scope = RequestScope::new(RequestId([39; 16]), start + Duration::from_secs(3)).unwrap();
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
            RequestScope::new(RequestId([40; 16]), start + Duration::from_secs(10)).unwrap();
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
            RequestScope::new(RequestId([101; 16]), start + Duration::from_secs(30)).unwrap();
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
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(2)).unwrap();
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
