//! Monotonic deadlines and bounded cancellation notification registrations.
use crate::{
    error::{Error, Result},
    model::RequestId,
};
use std::{
    sync::{
        Arc, Mutex, OnceLock, Weak,
        atomic::{AtomicBool, Ordering},
    },
    task::Waker,
    time::Instant,
};
#[derive(Clone, Copy, Debug)]
pub struct Deadline(pub Instant);
#[derive(Clone)]
pub struct Cancellation {
    state: Arc<State>,
}
struct State {
    canceled: AtomicBool,
    candidate_total: OnceLock<Instant>,
    candidate_idle: OnceLock<Mutex<(std::time::Duration, Instant)>>,
    candidate_body: OnceLock<Mutex<CandidateBody>>,
    registrations: Mutex<Vec<Weak<futures::task::AtomicWaker>>>,
}
struct CandidateBody {
    observation: std::time::Duration,
    complete_by: Instant,
    first: Option<(Instant, usize)>,
    expired: bool,
}
/// Operation-owned cancellation wake registration. Drop removes its live entry;
/// independent operations may safely register the same executor waker.
pub struct CancellationRegistration {
    cancellation: Cancellation,
    wake: Arc<futures::task::AtomicWaker>,
}
impl CancellationRegistration {
    pub fn register(&self, waker: &Waker) {
        self.wake.register(waker);
        if self.cancellation.is_cancelled() {
            self.wake.wake();
        }
    }
}
impl Drop for CancellationRegistration {
    fn drop(&mut self) {
        if let Ok(mut entries) = self.cancellation.state.registrations.lock() {
            entries.retain(|entry| {
                !std::ptr::eq(entry.as_ptr(), Arc::as_ptr(&self.wake)) && entry.strong_count() != 0
            });
        }
    }
}
impl Cancellation {
    pub fn new() -> Result<Self> {
        Ok(Self {
            state: Arc::new(State {
                canceled: AtomicBool::new(false),
                candidate_total: OnceLock::new(),
                candidate_idle: OnceLock::new(),
                candidate_body: OnceLock::new(),
                registrations: Mutex::new(Vec::new()),
            }),
        })
    }
    pub fn is_cancelled(&self) -> bool {
        self.state.canceled.load(Ordering::Acquire)
    }
    /// Subscribe for one operation's lifetime, even when several operations share
    /// an executor waker. Dropping the subscription reclaims its capacity and wake.
    pub fn subscribe(&self) -> Result<CancellationRegistration> {
        let wake = Arc::new(futures::task::AtomicWaker::new());
        let mut entries = self
            .state
            .registrations
            .lock()
            .map_err(|_| Error::Unavailable)?;
        entries.retain(|entry| entry.strong_count() != 0);
        if entries.len() >= 1024 {
            return Err(Error::Overloaded);
        }
        entries.push(Arc::downgrade(&wake));
        Ok(CancellationRegistration {
            cancellation: self.clone(),
            wake,
        })
    }
    pub fn cancel(&self) -> Result<()> {
        self.state.canceled.store(true, Ordering::Release);
        let registrations: Vec<_> = self
            .state
            .registrations
            .lock()
            .map_err(|_| Error::Unavailable)?
            .iter()
            .filter_map(Weak::upgrade)
            .collect();
        for registration in registrations {
            registration.wake();
        }
        Ok(())
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
                crate::runtime::environment::now() + allowance,
            )))
            .map_err(|_| Error::Internal)?;
        Ok(())
    }

    pub(crate) fn candidate_progress(&self) -> Result<()> {
        self.check()?;
        if let Some(idle) = self.cancellation.state.candidate_idle.get() {
            let mut idle = idle.lock().map_err(|_| Error::Unavailable)?;
            idle.1 = crate::runtime::environment::now() + idle.0;
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
            let now = crate::runtime::environment::now();
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
        } else if crate::runtime::environment::now() >= self.deadline.0 {
            Err(Error::DeadlineExceeded)
        } else {
            if let Some(expires) = self.cancellation.state.candidate_total.get()
                && crate::runtime::environment::now() >= *expires
            {
                return Err(Error::DeadlineExceeded);
            }
            if let Some(body) = self.cancellation.state.candidate_body.get() {
                let body = body.lock().map_err(|_| Error::Unavailable)?;
                if body.expired
                    || (body.first.is_some()
                        && crate::runtime::environment::now() >= body.complete_by)
                {
                    return Err(Error::DeadlineExceeded);
                }
            }
            if let Some(idle) = self.cancellation.state.candidate_idle.get()
                && crate::runtime::environment::now()
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
#[cfg(test)]
mod tests {
    use super::*;
    use std::{sync::atomic::AtomicUsize, task::Wake, time::Duration};
    #[test]
    fn candidate_total_survives_body_completion_and_never_changes_authority() {
        let clock = crate::runtime::environment::SimulationClock::new(97);
        let _env = clock.environment(0).enter();
        let start = crate::runtime::environment::now();
        let scope =
            RequestScope::new(RequestId([97; 16]), start + Duration::from_secs(60)).unwrap();
        let signed = crate::security::protocol::encode_deadline(scope.deadline).unwrap();
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
            crate::security::protocol::encode_deadline(scope.deadline).unwrap(),
            signed
        );
    }

    #[test]
    fn candidate_body_budget_accepts_original_ceiling_and_rejects_invalid_bounds() {
        let clock = crate::runtime::environment::SimulationClock::new(98);
        let _env = clock.environment(0).enter();
        let start = crate::runtime::environment::now();
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
        let clock = crate::runtime::environment::SimulationClock::new(91);
        let _env = clock.environment(0).enter();
        let start = crate::runtime::environment::now();
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
        assert!(crate::runtime::environment::now() > start + Duration::from_secs(10));
        assert_eq!(healthy.deadline.0, start + Duration::from_secs(30));
        // Completed network body does not subject later verification to the reserve.
        clock.advance(Duration::from_secs(7));
        healthy.check().unwrap();
        for reserve in [false, true] {
            let now = crate::runtime::environment::now();
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
        let clock = crate::runtime::environment::SimulationClock::new(93);
        let _env = clock.environment(0).enter();
        let start = crate::runtime::environment::now();
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
        assert!(crate::runtime::environment::now() < scope.deadline.0);
    }

    #[test]
    fn candidate_idle_progress_never_renews_hard_deadline_or_revives_expiry() {
        let clock = crate::runtime::environment::SimulationClock::new(39);
        let _env = clock.environment(0).enter();
        let start = crate::runtime::environment::now();
        let scope = RequestScope::new(RequestId([39; 16]), start + Duration::from_secs(3)).unwrap();
        scope.set_candidate_idle(Duration::from_secs(1)).unwrap();
        let signed = crate::security::protocol::encode_deadline(scope.deadline).unwrap();
        for _ in 0..5 {
            clock.advance(Duration::from_millis(500));
            scope.candidate_progress().unwrap();
            assert_eq!(
                crate::security::protocol::encode_deadline(scope.deadline).unwrap(),
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
    fn operation_registrations_reclaim_capacity_and_do_not_remove_shared_wakes() {
        let cancellation = Cancellation::new().unwrap();
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        for _ in 0..2048 {
            let registration = cancellation.subscribe().unwrap();
            registration.register(&waker);
        }
        assert!(cancellation.state.registrations.lock().unwrap().is_empty());
        let first = cancellation.subscribe().unwrap();
        let second = cancellation.subscribe().unwrap();
        first.register(&waker);
        second.register(&waker);
        drop(first);
        cancellation.cancel().unwrap();
        assert_eq!(count.0.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn dropping_subscription_releases_executor_resources_before_scope_ends() {
        let cancellation = Cancellation::new().unwrap();
        let executor = Arc::new(Count(AtomicUsize::new(0)));
        let weak = Arc::downgrade(&executor);
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor));
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
        assert!(!cancellation.is_cancelled());

        // A worker that subscribes after cancellation must still be notified.
        cancellation.cancel().unwrap();
        let executor = Arc::new(Count(AtomicUsize::new(0)));
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor.clone()));
        assert_eq!(executor.0.load(Ordering::Relaxed), 1);
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
