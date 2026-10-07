//! Distinct admission mechanisms for peer recovery, handoff, and speculation.
//!
//! Adaptive permits retain shared capacity until their last owner leaves. Endpoint
//! circuits instead own local half-open probes, whose exclusivity survives timeout
//! and successful observation. Handoff envelopes retain target reservations while
//! queued and after dequeue. Hedge alarms never release their capacity: owners
//! retain permits through both contenders' completion fences.
use crate::{Error, Result};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

pub use circuit::{Circuits, Probe};
pub use handoff::{Admission as HandoffAdmission, Admitted, Handoff, Offer};
pub use hedge::{Hedges, Permit as HedgePermit};

/// A retry or timeout beyond the clock's range never expires.
#[derive(Clone, Copy)]
enum Deadline {
    At(Instant),

    Never,
}

impl Deadline {
    /// Keep an unrepresentable deadline distinct from an absent delay.
    fn after(now: Instant, delay: Duration) -> Self {
        now.checked_add(delay).map_or(Self::Never, Self::At)
    }

    /// Only finite deadlines can become eligible.
    fn elapsed(self, now: Instant) -> bool {
        matches!(self, Self::At(at) if now >= at)
    }
}

/// Limits and time intervals for shared adaptive admission.
#[derive(Clone, Copy)]
pub struct Config {
    /// Maximum aggregate active operations before adaptive reductions.
    pub total: usize,

    /// Maximum active operations for one key before failure reductions.
    pub per_key: usize,

    /// Maximum retained peer records, including idle backoff history.
    pub capacity: usize,

    /// Peer retry delay and minimum interval between aggregate pressure reductions.
    /// A retry beyond the clock's range keeps the peer circuit open indefinitely.
    pub backoff: Duration,

    /// Minimum interval between one-slot verified-success recoveries.
    pub recovery: Duration,

    /// Minimum idle age before an eligible peer record can be replaced.
    pub retire_after: Duration,
}

/// Caller-classified evidence, independent of application errors.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    /// Authenticated successful work provides recovery evidence.
    Verified,

    /// A failure attributable to the immediate peer opens its circuit.
    PeerFailure,

    /// Local overload reduces aggregate admission without blaming the peer.
    LocalPressure,

    /// No evidence should affect admission or peer recovery.
    Neutral,
}

/// Admission observations emitted synchronously in state-transition order.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Event {
    /// Aggregate, per-key, or record capacity rejected the attempt.
    Rejected,

    /// Peer backoff or an owned probe rejected the attempt.
    CircuitRejected,

    /// An operation now owns active admission.
    Accepted,

    /// An accepted operation owns the exclusive recovery probe.
    Probe,

    /// Caller-reported local pressure was observed.
    LocalPressure,

    /// Caller-reported verified success was observed.
    Verified,

    /// Caller-reported immediate-link failure was observed.
    LinkFailure,
}

/// Callbacks run synchronously under the admission lock and must not reenter it.
pub trait Observer {
    /// Record an admission or outcome event without reentering the owner.
    fn event(&self, event: Event);

    /// Publish the current active count without reentering the owner.
    fn active(&self, active: usize);

    /// Publish the current adaptive limit without reentering the owner.
    fn limit(&self, limit: usize);
}

/// Shared total and per-key admission with generation-fenced peer recovery.
pub struct Adaptive<K, O> {
    config: Config,

    state: Mutex<State<K>>,

    observer: O,

    now: fn() -> Instant,
}

/// Mutex-protected aggregate admission and bounded peer records.
struct State<K> {
    active: usize,

    limit: usize,

    updated: Instant,

    peers: BTreeMap<K, Peer>,
}

/// Per-key recovery state retained while any permit remains active.
struct Peer {
    active: usize,

    limit: usize,

    /// Exhaustion blocks peer evidence and admission until all permits drain.
    generation: Option<u64>,

    retry: Option<Deadline>,

    probe: bool,

    updated: Instant,

    /// Start of the current idle interval, independent of recovery throttling.
    idle_since: Option<Instant>,
}

/// An admitted operation; the final shared owner releases capacity.
pub struct Permit<K: Ord + Clone, O: Observer> {
    owner: Arc<Adaptive<K, O>>,

    key: K,

    generation: u64,

    probe: bool,
}

impl<K: Ord + Clone, O: Observer> Adaptive<K, O> {
    /// Validate positive limits and publish the initial aggregate ceiling.
    pub fn new(config: Config, observer: O, now: fn() -> Instant) -> Result<Arc<Self>> {
        if config.total == 0
            || config.per_key == 0
            || config.per_key > config.total
            || config.capacity == 0
        {
            return Err(Error::InvalidInput);
        }
        observer.limit(config.total);
        Ok(Arc::new(Self {
            config,
            observer,
            now,
            state: Mutex::new(State {
                active: 0,
                limit: config.total,
                updated: now(),
                peers: BTreeMap::new(),
            }),
        }))
    }

    /// Hint whether fully recovered limits leave spare speculative capacity.
    pub fn hedge_available(&self, key: &K) -> bool {
        self.state.lock().is_ok_and(|s| {
            s.limit == self.config.total
                && s.active.saturating_add(1) < s.limit
                && s.peers.get(key).is_none_or(|p| {
                    p.retry.is_none()
                        && !p.probe
                        && p.limit == self.config.per_key
                        && p.active < p.limit
                })
        })
    }

    /// Hint whether peer backoff permits an attempt; this does not reserve capacity.
    pub fn available(&self, key: &K) -> bool {
        let now = (self.now)();
        self.state.lock().is_ok_and(|s| {
            s.peers.get(key).is_none_or(|p| {
                p.generation.is_some() && !p.probe && p.retry.is_none_or(|at| at.elapsed(now))
            })
        })
    }

    /// Admit work or one exclusive recovery probe without waiting.
    pub fn acquire(self: &Arc<Self>, key: &K) -> Result<Arc<Permit<K, O>>> {
        let indexed = key.clone();
        let owned = key.clone();
        let now = (self.now)();
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.active >= state.limit {
            self.observer.event(Event::Rejected);
            return Err(Error::Overloaded);
        }
        if !state.peers.contains_key(key) && state.peers.len() == self.config.capacity {
            let retired = state
                .peers
                .extract_if(.., |_, p| {
                    p.active == 0
                        && !p.probe
                        && p.retry.is_none_or(|retry| retry.elapsed(now))
                        && p.idle_since.is_some_and(|since| {
                            now.saturating_duration_since(since) >= self.config.retire_after
                        })
                })
                .next();
            if retired.is_none() {
                self.observer.event(Event::Rejected);
                return Err(Error::Overloaded);
            }
        }
        let peer = state.peers.entry(indexed).or_insert(Peer {
            active: 0,
            limit: self.config.per_key,
            generation: Some(0),
            retry: None,
            probe: false,
            updated: now,
            idle_since: None,
        });
        if peer.generation.is_none() || peer.probe || peer.retry.is_some_and(|at| !at.elapsed(now))
        {
            self.observer.event(Event::CircuitRejected);
            return Err(Error::Unavailable);
        }
        if peer.active >= peer.limit {
            self.observer.event(Event::Rejected);
            return Err(Error::Overloaded);
        }
        let probe = peer.retry.is_some();
        peer.probe = probe;
        peer.active += 1;
        peer.idle_since = None;
        let generation = peer
            .generation
            .expect("admission rejects exhausted generations");
        state.active += 1;
        self.observer.active(state.active);
        self.observer.event(Event::Accepted);
        if probe {
            self.observer.event(Event::Probe);
        }
        Ok(Arc::new(Permit {
            owner: self.clone(),
            key: owned,
            generation,
            probe,
        }))
    }
}

impl<K: Ord + Clone, O: Observer> Permit<K, O> {
    /// Apply caller evidence while retaining admission through completion.
    pub fn observe(&self, outcome: Outcome) {
        let now = (self.owner.now)();
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let config = self.owner.config;
        if outcome == Outcome::LocalPressure {
            self.owner.observer.event(Event::LocalPressure);
            if now.saturating_duration_since(state.updated) >= config.backoff {
                state.limit = (state.limit / 2).max(1);
                state.updated = now;
                self.owner.observer.limit(state.limit);
            }
            return;
        }
        if outcome == Outcome::Verified
            && state.limit < config.total
            && now.saturating_duration_since(state.updated) >= config.recovery
        {
            state.limit = state.limit.saturating_add(1).min(config.total);
            state.updated = now;
            self.owner.observer.limit(state.limit);
        }
        let peer = state
            .peers
            .get_mut(&self.key)
            .expect("live permit retains key");
        let event = match outcome {
            Outcome::Verified => Event::Verified,
            Outcome::PeerFailure => Event::LinkFailure,
            Outcome::LocalPressure => Event::LocalPressure,
            Outcome::Neutral => return,
        };
        self.owner.observer.event(event);
        if peer.generation != Some(self.generation) {
            return;
        }
        match outcome {
            Outcome::PeerFailure => {
                peer.limit = (peer.limit / 2).max(1);
                peer.generation = self.generation.checked_add(1);
                peer.retry = Some(Deadline::after(now, config.backoff));
                peer.updated = now;
            }
            Outcome::Verified => {
                if peer.retry.is_some() && !self.probe {
                    return;
                }
                peer.retry = None;
                if now.saturating_duration_since(peer.updated) >= config.recovery {
                    peer.limit = peer.limit.saturating_add(1).min(config.per_key);
                    peer.updated = now;
                }
            }
            Outcome::LocalPressure | Outcome::Neutral => {}
        }
    }
}

impl<K: Ord + Clone, O: Observer> Drop for Permit<K, O> {
    /// Release final ownership and renew backoff for an unsuccessful probe.
    fn drop(&mut self) {
        let now = (self.owner.now)();
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let peer = state
            .peers
            .get_mut(&self.key)
            .expect("live permit retains key");
        peer.active -= 1;
        if peer.active == 0 {
            // No live permit can carry a reused generation after this point.
            peer.generation.get_or_insert(0);
            peer.idle_since = Some(now);
        }
        if self.probe {
            peer.probe = false;
            if peer.retry.is_some() {
                peer.retry = Some(Deadline::after(now, self.owner.config.backoff));
                peer.updated = now;
            }
        }
        state.active -= 1;
        self.owner.observer.active(state.active);
    }
}

/// Bounded worker-local endpoint failure tracking, independent of adaptive limits.
mod circuit {
    use super::Deadline;
    use crate::{Error, Result};
    use std::{
        cell::RefCell,
        collections::{BTreeMap, BTreeSet},
        marker::PhantomData,
        rc::Rc,
        time::{Duration, Instant},
    };

    /// Local endpoint health with bounded records and completion-owned probes.
    ///
    /// The authority belongs to one worker even when keys themselves are shared:
    ///
    /// ```compile_fail
    /// use flow_control::Circuits;
    /// let circuits = Circuits::<u8>::new(1, std::time::Duration::ZERO);
    /// std::thread::spawn(move || drop(circuits));
    /// ```
    ///
    /// ```compile_fail
    /// use flow_control::Circuits;
    /// fn require_sync<T: Sync>() {}
    /// require_sync::<Circuits<u8>>();
    /// ```
    pub struct Circuits<K> {
        capacity: usize,

        probe_timeout: Duration,

        states: RefCell<BTreeMap<K, Circuit>>,

        probes: RefCell<BTreeSet<K>>,

        local: PhantomData<Rc<()>>,
    }

    /// Failure history and eligibility for one endpoint.
    struct Circuit {
        failures: u32,

        retry_at: Deadline,

        probe_until: Option<Deadline>,

        pending_backoff: Option<Rc<()>>,
    }

    impl Circuit {
        /// Require both retry backoff and any abandoned probe timeout to expire.
        fn available(&self, now: Instant) -> bool {
            self.retry_at.elapsed(now) && self.probe_until.is_none_or(|until| until.elapsed(now))
        }
    }

    /// Local exclusive half-open ownership; healthy acquisitions need no record.
    pub struct Probe<'a, K: Ord> {
        health: &'a Circuits<K>,

        key: Option<K>,
    }

    impl<K: Ord> Drop for Probe<'_, K> {
        /// Release exclusivity without erasing the probe's timeout.
        fn drop(&mut self) {
            if let Some(key) = &self.key {
                self.health.probes.borrow_mut().remove(key);
            }
        }
    }

    impl<K: Ord + Clone> Circuits<K> {
        /// Bound failure records and set eligibility delay for abandoned probes.
        /// A timeout beyond the clock's range never expires.
        pub const fn new(capacity: usize, probe_timeout: Duration) -> Self {
            Self {
                capacity,
                probe_timeout,
                states: RefCell::new(BTreeMap::new()),
                probes: RefCell::new(BTreeSet::new()),
                local: PhantomData,
            }
        }

        /// Acquire an exclusive half-open guard, or an untracked healthy guard.
        pub fn acquire(&self, key: &K, now: Instant) -> Result<Probe<'_, K>> {
            if !self.try_acquire(key, now) {
                return Err(Error::Unavailable);
            }
            let probe = self.states.borrow().contains_key(key);
            let key = if probe {
                if self.probes.borrow().len() >= self.capacity {
                    return Err(Error::Overloaded);
                }
                // Complete application cloning before publishing exclusive ownership.
                let indexed = key.clone();
                let owned = key.clone();
                self.probes.borrow_mut().insert(indexed);
                Some(owned)
            } else {
                None
            };
            Ok(Probe { health: self, key })
        }

        /// Remove failure state without releasing any owned probe.
        pub fn success(&self, key: &K) {
            self.states.borrow_mut().remove(key);
        }

        /// Record a caller-classified failure using caller-selected retry jitter.
        /// Backoff may reenter: the count is visible before the callback, and any
        /// newer transition for this key takes precedence over its returned delay.
        /// A retry beyond the clock's range never expires on its own.
        pub fn failure(
            &self,
            key: &K,
            now: Instant,
            backoff: impl FnOnce(&K, u32) -> Duration,
        ) -> Result<()> {
            let mut states = self.states.borrow_mut();
            if !states.contains_key(key) && states.len() >= self.capacity {
                return Err(Error::Overloaded);
            }
            let state = states.entry(key.clone()).or_insert(Circuit {
                failures: 0,
                retry_at: Deadline::At(now),
                probe_until: None,
                pending_backoff: None,
            });
            state.failures = state.failures.saturating_add(1);
            let failures = state.failures;
            let pending = Rc::new(());
            state.pending_backoff = Some(Rc::clone(&pending));
            drop(states);

            let retry_at = Deadline::after(now, backoff(key, failures));
            let mut states = self.states.borrow_mut();
            // Never reinsert a removed record or overwrite a newer transition.
            if let Some(state) = states.get_mut(key)
                && state
                    .pending_backoff
                    .as_ref()
                    .is_some_and(|current| Rc::ptr_eq(current, &pending))
            {
                state.retry_at = retry_at;
                state.probe_until = None;
                state.pending_backoff = None;
            }
            Ok(())
        }

        /// Routing hint only; actual work must acquire an exclusive probe.
        pub fn available(&self, key: &K, now: Instant) -> bool {
            !self.probes.borrow().contains(key)
                && self
                    .states
                    .borrow()
                    .get(key)
                    .is_none_or(|s| s.available(now))
        }

        /// Admit an unowned probe that becomes eligible again after its timeout.
        pub fn try_acquire(&self, key: &K, now: Instant) -> bool {
            if self.probes.borrow().contains(key) {
                return false;
            }
            let mut states = self.states.borrow_mut();
            let Some(state) = states.get_mut(key) else {
                return true;
            };
            if !state.available(now) {
                return false;
            }
            state.probe_until = Some(Deadline::after(now, self.probe_timeout));
            state.pending_backoff = None;
            true
        }

        /// Forget excluded failure records without releasing owned probes.
        pub fn retain(&self, keys: &[K]) {
            self.states.borrow_mut().retain(|key, _| keys.contains(key));
        }

        /// Count retained failure records, not active probes.
        pub fn len(&self) -> usize {
            self.states.borrow().len()
        }

        /// Whether no failure records remain, independently of owned probes.
        pub fn is_empty(&self) -> bool {
            self.states.borrow().is_empty()
        }
    }

    /// Endpoint state transition and ownership contracts.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Oversized retry delays stay closed to probes until explicit success.
        #[test]
        fn duration_overflow_endpoint_backoff() {
            let now = Instant::now();
            assert!(now.checked_add(Duration::MAX).is_none());
            for delay in [
                Duration::ZERO,
                Duration::from_secs(1),
                Duration::from_secs(u64::from(u32::MAX)),
                Duration::MAX,
            ] {
                let health = Circuits::new(1, Duration::ZERO);
                health.failure(&7, now, |_, _| delay).unwrap();
                assert_eq!(health.available(&7, now), delay.is_zero());
                if let Some(due) = now.checked_add(delay) {
                    assert!(health.acquire(&7, due).is_ok());
                } else {
                    let later = now + Duration::from_secs(60);
                    assert!(!health.available(&7, later));
                    assert!(matches!(health.acquire(&7, later), Err(Error::Unavailable)));
                    assert_eq!(
                        health.failure(&8, later, |_, _| Duration::ZERO),
                        Err(Error::Overloaded)
                    );
                }
                health.success(&7);
                assert!(health.acquire(&7, now).is_ok());
            }
        }

        /// Oversized abandoned-probe timeouts never expire or lose ownership.
        #[test]
        fn duration_overflow_endpoint_probe_timeout() {
            let now = Instant::now();
            for delay in [
                Duration::ZERO,
                Duration::from_secs(1),
                Duration::from_secs(u64::from(u32::MAX)),
                Duration::MAX,
            ] {
                let health = Circuits::new(1, delay);
                health.failure(&7, now, |_, _| Duration::ZERO).unwrap();
                let probe = health.acquire(&7, now).unwrap();
                assert!(!health.available(&7, now));
                drop(probe);
                assert_eq!(health.available(&7, now), delay.is_zero());
                if let Some(due) = now.checked_add(delay) {
                    assert!(health.try_acquire(&7, due));
                } else {
                    assert!(!health.try_acquire(&7, now + Duration::from_secs(60)));
                }
                health.success(&7);
                assert!(health.acquire(&7, now).is_ok());
            }
        }

        /// Backoff can inspect the circuit without borrowing conflicts.
        #[test]
        fn backoff_reentry_observes_failure_record() {
            let now = Instant::now();
            let health = Circuits::new(1, Duration::from_secs(1));
            health
                .failure(&7, now, |key, failures| {
                    assert_eq!((*key, failures), (7, 1));
                    assert_eq!(health.len(), 1);
                    assert!(!health.is_empty());
                    assert!(health.available(key, now));
                    Duration::from_secs(2)
                })
                .unwrap();
            assert!(!health.available(&7, now));
            assert!(health.available(&7, now + Duration::from_secs(2)));
        }

        /// A nested success or retention change must not be undone by backoff.
        #[test]
        fn backoff_reentry_preserves_removal_and_capacity() {
            let now = Instant::now();
            for retain in [false, true] {
                let health = Circuits::new(1, Duration::ZERO);
                health
                    .failure(&7, now, |_, _| {
                        if retain {
                            health.retain(&[]);
                        } else {
                            health.success(&7);
                        }
                        health.failure(&8, now, |_, _| Duration::ZERO).unwrap();
                        Duration::from_secs(10)
                    })
                    .unwrap();
                assert_eq!(health.len(), 1);
                assert!(health.available(&7, now));
                assert_eq!(
                    health.failure(&9, now, |_, _| panic!("capacity is full")),
                    Err(Error::Overloaded)
                );
                health.success(&8);
                assert!(health.is_empty());
            }
        }

        /// Newer failures win even when counts saturate or the key is replaced.
        #[test]
        fn backoff_reentry_preserves_newer_failure() {
            let now = Instant::now();
            for (initial, replace) in [(0, false), (u32::MAX, false), (0, true)] {
                let health = Circuits::new(1, Duration::ZERO);
                if initial != 0 {
                    health.failure(&7, now, |_, _| Duration::ZERO).unwrap();
                    health.states.borrow_mut().get_mut(&7).unwrap().failures = initial;
                }
                health
                    .failure(&7, now, |_, count| {
                        assert_eq!(count, initial.saturating_add(1));
                        if replace {
                            health.success(&7);
                        }
                        health
                            .failure(&7, now, |_, nested_count| {
                                assert_eq!(
                                    nested_count,
                                    if replace { 1 } else { count.saturating_add(1) }
                                );
                                Duration::from_secs(2)
                            })
                            .unwrap();
                        Duration::from_secs(10)
                    })
                    .unwrap();
                assert!(!health.available(&7, now));
                assert!(health.available(&7, now + Duration::from_secs(2)));
                assert_eq!(health.len(), 1);
            }
        }

        /// A probe admitted by backoff keeps its timeout after the callback.
        #[test]
        fn backoff_reentry_preserves_probe_timeout() {
            let now = Instant::now();
            let timeout = Duration::from_secs(2);
            let health = Circuits::new(1, timeout);
            health.failure(&7, now, |_, _| Duration::ZERO).unwrap();
            health
                .failure(&7, now, |_, _| {
                    drop(health.acquire(&7, now).unwrap());
                    Duration::from_secs(10)
                })
                .unwrap();
            assert!(!health.available(&7, now));
            assert!(health.available(&7, now + timeout));
        }

        /// Failed key cloning must not leave an exclusive probe without an owner.
        #[test]
        fn review_regression_probe_clone_panic_allows_acquisition_after_timeout() {
            use std::{
                panic::{AssertUnwindSafe, catch_unwind},
                sync::atomic::{AtomicUsize, Ordering},
            };

            static CLONES: AtomicUsize = AtomicUsize::new(0);
            static PANIC_AT: AtomicUsize = AtomicUsize::new(usize::MAX);

            /// A single endpoint with controllable clone failure.
            #[derive(Eq, Ord, PartialEq, PartialOrd)]
            struct Key;

            impl Clone for Key {
                /// Fail at the selected clone during probe acquisition.
                fn clone(&self) -> Self {
                    let clone = CLONES.fetch_add(1, Ordering::SeqCst) + 1;
                    assert_ne!(clone, PANIC_AT.load(Ordering::SeqCst), "key clone failed");
                    Self
                }
            }

            for panic_at in [1, 2] {
                let now = Instant::now();
                let timeout = Duration::from_secs(1);
                let health = Circuits::new(1, timeout);
                health.failure(&Key, now, |_, _| Duration::ZERO).unwrap();
                CLONES.store(0, Ordering::SeqCst);
                PANIC_AT.store(panic_at, Ordering::SeqCst);
                let result = catch_unwind(AssertUnwindSafe(|| health.acquire(&Key, now)));
                PANIC_AT.store(usize::MAX, Ordering::SeqCst);
                assert!(result.is_err());
                assert_eq!(CLONES.load(Ordering::SeqCst), panic_at);
                assert!(!health.available(&Key, now));
                let retry = now + timeout;
                let probe = health
                    .acquire(&Key, retry)
                    .expect("probe must not be stranded");
                assert!(!health.available(&Key, retry + timeout));
                drop(probe);
                assert!(health.acquire(&Key, retry + timeout).is_ok());
            }
        }

        /// Keep an owned probe exclusive through success and retention changes.
        #[test]
        fn bounded_backoff_and_owned_probe_survive_retention_and_success() {
            let now = Instant::now();
            let health = Circuits::new(1, Duration::from_secs(1));
            assert!(health.try_acquire(&7, now));
            health
                .failure(&7, now, |key, failures| {
                    assert_eq!((*key, failures), (7, 1));
                    Duration::from_secs(2)
                })
                .unwrap();
            assert_eq!(
                health.failure(&8, now, |_, _| Duration::ZERO),
                Err(Error::Overloaded)
            );
            assert!(!health.available(&7, now));
            let retry = now + Duration::from_secs(2);
            let probe = health.acquire(&7, retry).unwrap();
            assert!(!health.try_acquire(&7, retry + Duration::from_secs(5)));
            health.success(&7);
            health.retain(&[]);
            assert!(!health.available(&7, retry));
            drop(probe);
            assert!(health.available(&7, retry));
            assert!(health.is_empty());
        }

        /// Abandonment preserves timeout and failure history never overflows.
        #[test]
        fn dropped_probe_preserves_timeout_and_failures_saturate() {
            let now = Instant::now();
            let health = Circuits::new(1, Duration::from_secs(1));
            health.failure(&(), now, |_, _| Duration::ZERO).unwrap();
            drop(health.acquire(&(), now).unwrap());
            assert!(!health.try_acquire(&(), now));
            assert!(health.try_acquire(&(), now + Duration::from_secs(1)));
            health.states.borrow_mut().get_mut(&()).unwrap().failures = u32::MAX;
            health
                .failure(&(), now, |_, n| {
                    assert_eq!(n, u32::MAX);
                    Duration::ZERO
                })
                .unwrap();
            health.retain(&[]);
            assert_eq!(health.len(), 0);
            assert_eq!(
                Circuits::new(0, Duration::ZERO).failure(&(), now, |_, _| Duration::ZERO),
                Err(Error::Overloaded)
            );
        }

        /// Probe capacity and failure records remain independent after success.
        #[test]
        fn full_probe_set_preserves_rejected_attempt_timeout() {
            let now = Instant::now();
            let timeout = Duration::from_secs(1);
            let health = Circuits::new(1, timeout);
            health.failure(&7, now, |_, _| Duration::ZERO).unwrap();
            let held = health.acquire(&7, now).unwrap();
            health.success(&7);
            assert!(health.is_empty());
            assert!(
                health.acquire(&9, now).is_ok(),
                "healthy work needs no slot"
            );

            health.failure(&8, now, |_, _| Duration::ZERO).unwrap();
            assert!(health.available(&8, now));
            assert!(matches!(health.acquire(&8, now), Err(Error::Overloaded)));
            drop(held);
            assert!(!health.available(&8, now));
            assert!(matches!(health.acquire(&8, now), Err(Error::Unavailable)));
            assert!(health.acquire(&8, now + timeout).is_ok());
        }
    }
}

/// Round-robin delivery with structurally retained target admission.
mod handoff {
    use crate::{Error, Result};
    use std::{
        collections::VecDeque,
        sync::{Arc, Mutex},
        task::Waker,
    };

    /// Bounds queued, delivered, and outstanding offered items with reservations.
    /// Callbacks execute under the handoff lock and must not reenter it.
    pub trait Admission {
        /// Ownership that retains capacity until final admitted work completes.
        type Reservation;

        /// Register for released capacity before attempting reservation.
        fn register(&self, waker: &Waker);

        /// Reserve capacity for one item without waiting.
        fn reserve(&self) -> Result<Self::Reservation>;
    }

    /// Payload and reservation remain inseparable while queued or freshly popped.
    pub struct Admitted<T, R> {
        item: T,

        reservation: R,
    }

    /// Fixed batch whose occupied entries each retain their target reservation.
    type Batch<T, R, const N: usize> = [Option<Admitted<T, R>>; N];

    impl<T, R> Admitted<T, R> {
        /// Transfer both owners together to the application's completion owner.
        pub fn into_parts(self) -> (T, R) {
            (self.item, self.reservation)
        }
    }

    /// One target's installed admission, inbox, and wake registration.
    struct Target<A: Admission, T> {
        admission: Option<A>,

        queue: VecDeque<Admitted<T, A::Reservation>>,

        // Sharing the registration avoids raw waker callbacks under the lock.
        waker: Option<Arc<Waker>>,

        closed: bool,
    }

    /// Stable target slots and the next round-robin scan position.
    struct State<K, A: Admission, T> {
        targets: Vec<(K, Target<A, T>)>,

        cursor: usize,
    }

    impl<K: Eq, A: Admission, T> State<K, A, T> {
        /// Find a stable target without changing its admission or closed state.
        fn target(&mut self, key: &K) -> Option<&mut Target<A, T>> {
            self.targets
                .iter_mut()
                .find(|(candidate, _)| candidate == key)
                .map(|(_, target)| target)
        }
    }

    /// Shared handoff bounded by admission, not a separate queue-slot limit.
    pub struct Handoff<K, A: Admission, T>(Mutex<State<K, A, T>>);

    /// Capacity reserved on a selected target before constructing its payload.
    pub struct Offer<K, A: Admission, T> {
        handoff: Arc<Handoff<K, A, T>>,

        target: usize,

        reservation: A::Reservation,
    }

    impl<K: Eq + Clone, A: Admission, T> Handoff<K, A, T> {
        /// Create stable target slots; empty target lists simply reject offers.
        /// Repeated keys share one slot, in first-occurrence order.
        pub fn new(keys: &[K]) -> Self {
            let mut targets = Vec::new();
            for key in keys {
                if targets.iter().any(|(candidate, _)| candidate == key) {
                    continue;
                }
                targets.push((
                    key.clone(),
                    Target {
                        admission: None,
                        queue: VecDeque::new(),
                        waker: None,
                        closed: false,
                    },
                ));
            }
            Self(Mutex::new(State { targets, cursor: 0 }))
        }

        /// Install admission once on a known target that has not closed.
        pub fn install(&self, key: &K, admission: A) -> Result<()> {
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            let target = state.target(key).ok_or(Error::InvalidInput)?;
            if target.admission.is_some() || target.closed {
                return Err(Error::InvalidInput);
            }
            target.admission = Some(admission);
            Ok(())
        }

        /// Scan open targets fairly and reserve before returning an offer.
        pub fn reserve(self: &Arc<Self>, waker: &Waker) -> Result<Offer<K, A, T>> {
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            let mut rejection = None;
            for offset in 0..state.targets.len() {
                let index = (state.cursor + offset) % state.targets.len();
                let (_, target) = &state.targets[index];
                if target.closed {
                    continue;
                }
                let Some(admission) = &target.admission else {
                    continue;
                };
                admission.register(waker);
                match admission.reserve() {
                    Ok(reservation) => {
                        state.cursor = (index + 1) % state.targets.len();
                        return Ok(Offer {
                            handoff: self.clone(),
                            target: index,
                            reservation,
                        });
                    }
                    Err(Error::Overloaded) => rejection = Some(Error::Overloaded),
                    Err(Error::Unavailable) => {
                        rejection.get_or_insert(Error::Unavailable);
                    }
                    Err(error) => return Err(error),
                }
            }
            Err(rejection.unwrap_or(Error::Overloaded))
        }

        /// Pop at most the caller's budget while retaining each item's admission.
        /// Closed targets reject pops without retaining the caller's waker.
        pub fn pop_batch<const N: usize>(
            &self,
            key: &K,
            waker: &Waker,
            budget: usize,
        ) -> Result<Batch<T, A::Reservation, N>> {
            let waker = Arc::new(waker.clone());
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            let target = state.target(key).ok_or(Error::InvalidInput)?;
            if target.closed {
                return Err(Error::Unavailable);
            }
            let old = target.waker.replace(waker);
            let batch = std::array::from_fn(|index| {
                if index < budget {
                    target.queue.pop_front()
                } else {
                    None
                }
            });
            drop(state);
            drop(old);
            Ok(batch)
        }

        /// Close permanently, then drop ownership and wake outside the shared lock.
        pub fn close(&self, key: &K) {
            let (queued, admission, waker) = {
                let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
                let Some(target) = state.target(key) else {
                    return;
                };
                target.closed = true;
                (
                    std::mem::take(&mut target.queue),
                    target.admission.take(),
                    target.waker.take(),
                )
            };
            drop(queued);
            drop(admission);
            if let Some(waker) = waker {
                waker.wake_by_ref();
            }
        }
    }

    impl<K, A: Admission, T> Offer<K, A, T> {
        /// Build only on an open target; the envelope retains admission itself.
        /// The closure executes under the handoff lock and must not reenter it.
        pub fn deliver(self, item: impl FnOnce() -> T) -> Result<()> {
            let mut state = self.handoff.0.lock().map_err(|_| Error::Unavailable)?;
            let target = &mut state.targets[self.target].1;
            if target.closed {
                return Err(Error::Unavailable);
            }
            target.queue.push_back(Admitted {
                item: item(),
                reservation: self.reservation,
            });
            let waker = target.waker.clone();
            drop(state);
            if let Some(waker) = waker {
                waker.wake_by_ref();
            }
            Ok(())
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use std::{
            sync::{
                Weak,
                atomic::{AtomicUsize, Ordering},
            },
            task::{RawWaker, RawWakerVTable, Wake},
        };

        struct TargetWake {
            handoff: Weak<Handoff<u8, Ready, u8>>,
            blocked: AtomicUsize,
            clones: AtomicUsize,
            drops: AtomicUsize,
            wakes: AtomicUsize,
        }

        impl TargetWake {
            fn reenter(&self) {
                let Some(handoff) = self.handoff.upgrade() else {
                    return;
                };
                if let Ok(state) = handoff.0.try_lock() {
                    drop(state);
                    handoff.close(&2);
                } else {
                    self.blocked.fetch_add(1, Ordering::SeqCst);
                }
            }

            fn new(handoff: &Arc<Handoff<u8, Ready, u8>>) -> (Arc<Self>, Waker) {
                let state = Arc::new(Self {
                    handoff: Arc::downgrade(handoff),
                    blocked: AtomicUsize::new(0),
                    clones: AtomicUsize::new(0),
                    drops: AtomicUsize::new(0),
                    wakes: AtomicUsize::new(0),
                });
                // SAFETY: Each raw waker owns one Arc, managed by this vtable.
                let waker = unsafe { Waker::from_raw(Self::raw(state.clone())) };
                (state, waker)
            }

            fn raw(state: Arc<Self>) -> RawWaker {
                RawWaker::new(Arc::into_raw(state).cast(), &Self::VTABLE)
            }

            const VTABLE: RawWakerVTable = RawWakerVTable::new(
                |data| {
                    // SAFETY: Borrow the live Arc without consuming the source waker.
                    let state =
                        std::mem::ManuallyDrop::new(unsafe { Arc::<Self>::from_raw(data.cast()) });
                    state.reenter();
                    state.clones.fetch_add(1, Ordering::SeqCst);
                    Self::raw(Arc::clone(&state))
                },
                |data| {
                    // SAFETY: Wake consumes this raw waker's Arc exactly once.
                    let state = unsafe { Arc::<Self>::from_raw(data.cast()) };
                    state.reenter();
                    state.wakes.fetch_add(1, Ordering::SeqCst);
                },
                |data| {
                    // SAFETY: Borrow without consuming the source waker's Arc.
                    let state =
                        std::mem::ManuallyDrop::new(unsafe { Arc::<Self>::from_raw(data.cast()) });
                    state.reenter();
                    state.wakes.fetch_add(1, Ordering::SeqCst);
                },
                |data| {
                    // SAFETY: Drop consumes this raw waker's Arc exactly once.
                    let state = unsafe { Arc::<Self>::from_raw(data.cast()) };
                    state.reenter();
                    state.drops.fetch_add(1, Ordering::SeqCst);
                },
            );
        }

        #[test]
        fn target_waker_clone_can_reenter_pop() {
            let handoff = Arc::new(Handoff::new(&[1]));
            let (state, waker) = TargetWake::new(&handoff);
            for _ in 0..2 {
                assert!(handoff.pop_batch::<1>(&1, &waker, 0).unwrap()[0].is_none());
            }
            assert!(state.clones.load(Ordering::SeqCst) > 0);
            assert_eq!(state.blocked.load(Ordering::SeqCst), 0);
        }

        #[test]
        fn target_waker_drop_can_reenter_replacement_and_rejection() {
            let handoff = Arc::new(Handoff::new(&[1]));
            let (state, waker) = TargetWake::new(&handoff);
            handoff.pop_batch::<0>(&1, &waker, 0).unwrap();
            state.blocked.store(0, Ordering::SeqCst);
            handoff.pop_batch::<0>(&1, Waker::noop(), 0).unwrap();
            assert_eq!(state.drops.load(Ordering::SeqCst), 1);
            assert_eq!(state.blocked.load(Ordering::SeqCst), 0);
            handoff.close(&1);
            assert!(matches!(
                handoff.pop_batch::<1>(&1, &waker, 1),
                Err(Error::Unavailable)
            ));
            assert!(matches!(
                handoff.pop_batch::<1>(&2, &waker, 1),
                Err(Error::InvalidInput)
            ));
            assert_eq!(state.blocked.load(Ordering::SeqCst), 0);
            assert!(handoff.0.lock().unwrap().targets[0].1.waker.is_none());
        }

        #[test]
        fn target_waker_callbacks_can_reenter_repeated_delivery() {
            let handoff = Arc::new(Handoff::new(&[1]));
            handoff.install(&1, Ready).unwrap();
            let (state, waker) = TargetWake::new(&handoff);
            handoff.pop_batch::<0>(&1, &waker, 0).unwrap();
            state.blocked.store(0, Ordering::SeqCst);
            for item in [7, 8] {
                handoff
                    .reserve(Waker::noop())
                    .unwrap()
                    .deliver(|| item)
                    .unwrap();
            }
            assert_eq!(state.wakes.load(Ordering::SeqCst), 2);
            assert_eq!(state.blocked.load(Ordering::SeqCst), 0);
            let items = handoff.pop_batch::<2>(&1, Waker::noop(), 2).unwrap();
            assert_eq!(items.map(|item| item.unwrap().into_parts().0), [7, 8]);
        }

        type ClosingHandoff = Handoff<u8, CloseAdmission, CloseDrop>;

        #[derive(Clone)]
        struct CloseDrop {
            handoff: Weak<ClosingHandoff>,
            drops: Arc<AtomicUsize>,
        }

        impl CloseDrop {
            fn reenter(&self) {
                if let Some(handoff) = self.handoff.upgrade() {
                    let state = handoff
                        .0
                        .try_lock()
                        .expect("close must unlock before callbacks");
                    assert!(state.targets[0].1.closed);
                    assert!(state.targets[0].1.queue.is_empty());
                    assert!(state.targets[0].1.admission.is_none());
                    assert!(state.targets[0].1.waker.is_none());
                    drop(state);
                    handoff.close(&1);
                    assert!(matches!(
                        handoff.pop_batch::<1>(&1, Waker::noop(), 1),
                        Err(Error::Unavailable)
                    ));
                }
            }
        }

        impl Drop for CloseDrop {
            fn drop(&mut self) {
                self.reenter();
                self.drops.fetch_add(1, Ordering::SeqCst);
            }
        }

        struct CloseAdmission(CloseDrop);

        impl Admission for CloseAdmission {
            type Reservation = CloseDrop;

            fn register(&self, _: &Waker) {}

            fn reserve(&self) -> Result<CloseDrop> {
                Ok(self.0.clone())
            }
        }

        struct CloseWake {
            on_drop: CloseDrop,
            wakes: Arc<AtomicUsize>,
        }

        impl Wake for CloseWake {
            fn wake(self: Arc<Self>) {
                self.on_drop.reenter();
                self.wakes.fetch_add(1, Ordering::SeqCst);
            }
        }

        #[test]
        fn close_releases_admission_and_queue_outside_lock() {
            for queued in [0, 2] {
                let handoff = Arc::new(ClosingHandoff::new(&[1]));
                let drops = Arc::new(AtomicUsize::new(0));
                handoff
                    .install(
                        &1,
                        CloseAdmission(CloseDrop {
                            handoff: Arc::downgrade(&handoff),
                            drops: drops.clone(),
                        }),
                    )
                    .unwrap();
                for _ in 0..queued {
                    handoff
                        .reserve(Waker::noop())
                        .unwrap()
                        .deliver(|| CloseDrop {
                            handoff: Arc::downgrade(&handoff),
                            drops: drops.clone(),
                        })
                        .unwrap();
                }
                handoff.close(&1);
                assert_eq!(drops.load(Ordering::SeqCst), 1 + 2 * queued);
                handoff.close(&1);
                handoff.close(&2);
                assert_eq!(drops.load(Ordering::SeqCst), 1 + 2 * queued);
                assert!(handoff.0.lock().unwrap().targets[0].1.admission.is_none());
            }
        }

        #[test]
        fn close_wakes_and_releases_waiter_outside_lock() {
            let handoff = Arc::new(ClosingHandoff::new(&[1]));
            let drops = Arc::new(AtomicUsize::new(0));
            let wakes = Arc::new(AtomicUsize::new(0));
            let waker = Waker::from(Arc::new(CloseWake {
                on_drop: CloseDrop {
                    handoff: Arc::downgrade(&handoff),
                    drops: drops.clone(),
                },
                wakes: wakes.clone(),
            }));
            assert!(handoff.pop_batch::<1>(&1, &waker, 1).unwrap()[0].is_none());
            drop(waker);
            handoff.close(&2);
            assert_eq!(wakes.load(Ordering::SeqCst), 0);
            handoff.close(&1);
            assert_eq!(wakes.load(Ordering::SeqCst), 1);
            assert_eq!(drops.load(Ordering::SeqCst), 1);
            handoff.close(&1);
            assert_eq!(wakes.load(Ordering::SeqCst), 1);
            assert!(handoff.0.lock().unwrap().targets[0].1.waker.is_none());
        }

        #[test]
        fn close_rejects_pop_without_retaining_new_waiter() {
            let handoff = Handoff::<_, Ready, ()>::new(&[1, 2]);
            handoff.install(&1, Ready).unwrap();
            for key in [1, 2] {
                handoff.close(&key);
                for budget in [0, 1, usize::MAX] {
                    assert!(matches!(
                        handoff.pop_batch::<1>(&key, Waker::noop(), budget),
                        Err(Error::Unavailable)
                    ));
                    assert!(matches!(
                        handoff.pop_batch::<0>(&key, Waker::noop(), budget),
                        Err(Error::Unavailable)
                    ));
                    assert!(
                        handoff
                            .0
                            .lock()
                            .unwrap()
                            .target(&key)
                            .unwrap()
                            .waker
                            .is_none()
                    );
                }
                assert_eq!(handoff.install(&key, Ready), Err(Error::InvalidInput));
            }
            assert!(matches!(
                handoff.pop_batch::<1>(&3, Waker::noop(), 1),
                Err(Error::InvalidInput)
            ));
        }

        struct Ready;

        impl Admission for Ready {
            type Reservation = ();

            fn register(&self, _: &Waker) {}

            fn reserve(&self) -> Result<()> {
                Ok(())
            }
        }

        #[test]
        fn handoff_constructor_keeps_first_unique_targets() {
            let cases: &[(&[u8], &[u8])] = &[
                (&[], &[]),
                (&[1], &[1]),
                (&[1, 1, 1], &[1]),
                (&[3, 1, 3, 2, 1, 3], &[3, 1, 2]),
            ];
            for &(keys, expected) in cases {
                let handoff = Arc::new(Handoff::<_, Ready, u8>::new(keys));
                assert_eq!(
                    handoff
                        .0
                        .lock()
                        .unwrap()
                        .targets
                        .iter()
                        .map(|(key, _)| *key)
                        .collect::<Vec<_>>(),
                    expected
                );
                for key in expected {
                    handoff.install(key, Ready).unwrap();
                    assert_eq!(handoff.install(key, Ready), Err(Error::InvalidInput));
                }
                for _ in 0..2 {
                    for key in expected {
                        handoff
                            .reserve(Waker::noop())
                            .unwrap()
                            .deliver(|| *key)
                            .unwrap();
                        let [item] = handoff.pop_batch::<1>(key, Waker::noop(), 1).unwrap();
                        assert_eq!(item.unwrap().into_parts(), (*key, ()));
                    }
                }
                for key in expected {
                    handoff.close(key);
                    assert_eq!(handoff.install(key, Ready), Err(Error::InvalidInput));
                }
                assert!(matches!(
                    handoff.reserve(Waker::noop()),
                    Err(Error::Overloaded)
                ));
            }
        }
    }
}

/// Shared speculative capacity and explicitly polled due-time registrations.
mod hedge {
    use crate::{Error, Result};
    use std::{
        collections::BTreeMap,
        sync::{Arc, Mutex},
        task::{Context, Poll, Waker},
        time::Instant,
    };

    /// One admitted speculation and its latest wake registration.
    struct Alarm {
        due: Instant,

        cost: usize,

        wake: Option<Waker>,
    }

    /// Mutex-protected capacity and monotonically assigned alarm identities.
    #[derive(Default)]
    struct State {
        next: u64,

        used: usize,

        alarms: BTreeMap<u64, Alarm>,
    }

    /// Process-shared slot and cost limits, independent of worker registries.
    pub struct Hedges {
        slots: usize,

        capacity: usize,

        state: Mutex<State>,
    }

    /// Owns speculative capacity until both contenders reach their fences.
    #[must_use = "retain the permit until both contenders are fenced"]
    pub struct Permit {
        owner: Arc<Hedges>,

        id: u64,
    }

    impl Hedges {
        /// Create shared ceilings; zero slots disable speculative admission.
        pub fn new(slots: usize, capacity: usize) -> Arc<Self> {
            Arc::new(Self {
                slots,
                capacity,
                state: Mutex::new(State::default()),
            })
        }

        /// Reserve one slot and caller-selected cost until permit drop.
        pub fn acquire(self: &Arc<Self>, cost: usize, due: Instant) -> Result<Permit> {
            let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
            if state.alarms.len() >= self.slots || cost > self.capacity - state.used {
                return Err(Error::Overloaded);
            }
            let id = state.next.checked_add(1).ok_or(Error::Unavailable)?;
            state.next = id;
            state.used += cost;
            state.alarms.insert(
                id,
                Alarm {
                    due,
                    cost,
                    wake: None,
                },
            );
            Ok(Permit {
                owner: self.clone(),
                id,
            })
        }

        /// Wake due registrations outside the shared lock, once per registration.
        pub fn poll(&self, now: Instant) {
            let wakes: Vec<_> = self
                .state
                .lock()
                .map(|mut state| {
                    state
                        .alarms
                        .values_mut()
                        .filter(|a| now >= a.due)
                        .filter_map(|a| a.wake.take())
                        .collect()
                })
                .unwrap_or_default();
            for wake in wakes {
                wake.wake();
            }
        }
    }

    impl Permit {
        /// Register the latest waiter until due, without releasing capacity.
        pub fn delay(&self, now: Instant, cx: &mut Context<'_>) -> Poll<()> {
            // Waker clone and drop callbacks may reenter the hedge registry.
            let wake = cx.waker().clone();
            let mut state = self.owner.state.lock().expect("hedge alarm lock");
            let alarm = state.alarms.get_mut(&self.id).expect("live hedge alarm");
            let (result, previous) = if now >= alarm.due {
                (Poll::Ready(()), alarm.wake.take())
            } else {
                (Poll::Pending, alarm.wake.replace(wake))
            };
            drop(state);
            drop(previous);
            result
        }
    }

    impl Drop for Permit {
        /// Remove the alarm and return its cost only at final ownership release.
        fn drop(&mut self) {
            if let Ok(mut state) = self.owner.state.lock()
                && let Some(alarm) = state.alarms.remove(&self.id)
            {
                state.used -= alarm.cost;
            }
        }
    }

    /// Shared capacity and alarm lifecycle contracts.
    #[cfg(test)]
    mod tests {
        use super::*;
        use std::{
            mem::ManuallyDrop,
            sync::atomic::{AtomicUsize, Ordering},
            task::{RawWaker, RawWakerVTable, Wake},
            time::Duration,
        };

        /// Count alarm notifications across threads.
        #[derive(Default)]
        struct Counter(AtomicUsize);

        impl Wake for Counter {
            /// Record one notification.
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }

        struct DelayCallbacks {
            owner: Arc<Hedges>,
            now: Instant,
            clones: AtomicUsize,
            drops: AtomicUsize,
            locked_clones: AtomicUsize,
            locked_drops: AtomicUsize,
        }

        impl DelayCallbacks {
            fn new(owner: &Arc<Hedges>, now: Instant) -> Arc<Self> {
                Arc::new(Self {
                    owner: owner.clone(),
                    now,
                    clones: AtomicUsize::new(0),
                    drops: AtomicUsize::new(0),
                    locked_clones: AtomicUsize::new(0),
                    locked_drops: AtomicUsize::new(0),
                })
            }

            fn reenter(&self, calls: &AtomicUsize, locked: &AtomicUsize) {
                calls.fetch_add(1, Ordering::SeqCst);
                // Record a deadlock risk without hanging or poisoning the mutex.
                if self.owner.state.try_lock().is_err() {
                    locked.fetch_add(1, Ordering::SeqCst);
                    return;
                }
                self.owner.poll(self.now);
            }

            fn raw(this: Arc<Self>) -> RawWaker {
                RawWaker::new(Arc::into_raw(this).cast(), &Self::VTABLE)
            }

            fn waker(this: &Arc<Self>) -> Waker {
                // SAFETY: The vtable owns one Arc per raw waker and uses atomic state.
                unsafe { Waker::from_raw(Self::raw(this.clone())) }
            }

            const VTABLE: RawWakerVTable =
                RawWakerVTable::new(Self::clone_raw, Self::drop_raw, |_| {}, Self::drop_raw);

            unsafe fn clone_raw(data: *const ()) -> RawWaker {
                // SAFETY: Borrow the raw waker's Arc without consuming its reference.
                let this = ManuallyDrop::new(unsafe { Arc::from_raw(data.cast::<Self>()) });
                this.reenter(&this.clones, &this.locked_clones);
                Self::raw(Arc::clone(&this))
            }

            unsafe fn drop_raw(data: *const ()) {
                // SAFETY: Drop or consuming wake releases exactly one owned reference.
                let this = unsafe { Arc::from_raw(data.cast::<Self>()) };
                this.reenter(&this.drops, &this.locked_drops);
            }
        }

        #[test]
        fn hedge_delay_clones_waiter_outside_lock() {
            let now = Instant::now();
            let due = now + Duration::from_secs(1);
            let owner = Hedges::new(1, 1);
            let permit = owner.acquire(1, due).unwrap();
            let callbacks = DelayCallbacks::new(&owner, now);
            let waker = DelayCallbacks::waker(&callbacks);
            assert!(
                permit
                    .delay(now, &mut Context::from_waker(&waker))
                    .is_pending()
            );
            // Clear the registration so cleanup does not exercise Permit::drop.
            assert!(
                permit
                    .delay(due, &mut Context::from_waker(Waker::noop()))
                    .is_ready()
            );
            assert_eq!(callbacks.clones.load(Ordering::SeqCst), 1);
            assert_eq!(callbacks.locked_clones.load(Ordering::SeqCst), 0);
        }

        #[test]
        fn hedge_delay_replaces_waiter_outside_lock() {
            let now = Instant::now();
            let due = now + Duration::from_secs(1);
            let owner = Hedges::new(1, 1);
            let permit = owner.acquire(1, due).unwrap();
            let callbacks = DelayCallbacks::new(&owner, now);
            let waker = DelayCallbacks::waker(&callbacks);
            assert!(
                permit
                    .delay(now, &mut Context::from_waker(&waker))
                    .is_pending()
            );
            let latest = Arc::new(Counter::default());
            assert!(
                permit
                    .delay(now, &mut Context::from_waker(&Waker::from(latest.clone())))
                    .is_pending()
            );
            assert_eq!(callbacks.drops.load(Ordering::SeqCst), 1);
            assert_eq!(callbacks.locked_drops.load(Ordering::SeqCst), 0);
            owner.poll(due);
            owner.poll(due);
            assert_eq!(latest.0.load(Ordering::SeqCst), 1);
            assert!(matches!(owner.acquire(1, due), Err(Error::Overloaded)));
        }

        #[test]
        fn hedge_delay_clears_waiter_outside_lock() {
            for elapsed in [Duration::ZERO, Duration::from_secs(1)] {
                let now = Instant::now();
                let due = now + Duration::from_secs(1);
                let owner = Hedges::new(1, 1);
                let permit = owner.acquire(1, due).unwrap();
                let callbacks = DelayCallbacks::new(&owner, now);
                let waker = DelayCallbacks::waker(&callbacks);
                assert!(
                    permit
                        .delay(now, &mut Context::from_waker(&waker))
                        .is_pending()
                );
                assert!(
                    permit
                        .delay(due + elapsed, &mut Context::from_waker(Waker::noop()))
                        .is_ready()
                );
                assert_eq!(callbacks.drops.load(Ordering::SeqCst), 1);
                assert_eq!(callbacks.locked_drops.load(Ordering::SeqCst), 0);
                owner.poll(due + elapsed);
                assert_eq!(callbacks.drops.load(Ordering::SeqCst), 1);
                assert!(matches!(owner.acquire(1, due), Err(Error::Overloaded)));
            }
        }

        /// Slots and bytes stay charged after an alarm fires and across threads.
        #[test]
        fn hedge_shared_slots_costs_and_release_are_independent_of_alarm() {
            /// Require shared ownership at compile time.
            fn shared<T: Send + Sync>() {}
            shared::<Hedges>();
            shared::<Permit>();
            let now = Instant::now();
            let owner = Hedges::new(2, 10);
            let a = owner.acquire(7, now).unwrap();
            assert!(matches!(owner.acquire(4, now), Err(Error::Overloaded)));
            let b =
                std::thread::scope(|s| s.spawn(|| owner.acquire(3, now)).join().unwrap().unwrap());
            assert!(matches!(owner.acquire(0, now), Err(Error::Overloaded)));
            owner.poll(now);
            assert!(matches!(owner.acquire(1, now), Err(Error::Overloaded)));
            drop(a);
            let c = owner.acquire(7, now).unwrap();
            drop((b, c));
            assert!(owner.acquire(10, now).is_ok());
            assert!(matches!(
                Hedges::new(0, 10).acquire(0, now),
                Err(Error::Overloaded)
            ));
            let max = Hedges::new(2, usize::MAX);
            let _all = max.acquire(usize::MAX, now).unwrap();
            assert!(matches!(max.acquire(1, now), Err(Error::Overloaded)));
        }

        /// Direct completion clears the waiter but retains speculative capacity.
        #[test]
        fn hedge_ready_delay_clears_registration_without_releasing_capacity() {
            for elapsed in [Duration::ZERO, Duration::from_secs(1)] {
                let now = Instant::now();
                let due = now + Duration::from_secs(1);
                let owner = Hedges::new(1, 1);
                let permit = owner.acquire(1, due).unwrap();
                let waiter = Arc::new(Counter::default());
                assert!(
                    permit
                        .delay(now, &mut Context::from_waker(&Waker::from(waiter.clone())))
                        .is_pending()
                );
                assert!(
                    permit
                        .delay(due + elapsed, &mut Context::from_waker(Waker::noop()))
                        .is_ready()
                );
                assert_eq!(Arc::strong_count(&waiter), 1);
                owner.poll(due + elapsed);
                owner.poll(due + elapsed);
                assert_eq!(waiter.0.load(Ordering::SeqCst), 0);
                assert_eq!(Arc::strong_count(&waiter), 1);
                assert!(matches!(owner.acquire(1, due), Err(Error::Overloaded)));
                drop(permit);
                assert!(owner.acquire(1, due).is_ok());
            }
        }

        /// Only the latest registered waker fires and drop cancels notification.
        #[test]
        fn hedge_alarm_replaces_waker_and_drop_removes_registration() {
            let now = Instant::now();
            let due = now + Duration::from_secs(1);
            let owner = Hedges::new(1, 1);
            let permit = owner.acquire(1, due).unwrap();
            let old = Arc::new(Counter::default());
            let current = Arc::new(Counter::default());
            assert!(
                permit
                    .delay(now, &mut Context::from_waker(&Waker::from(old.clone())))
                    .is_pending()
            );
            assert!(
                permit
                    .delay(now, &mut Context::from_waker(&Waker::from(current.clone())))
                    .is_pending()
            );
            owner.poll(now);
            assert_eq!(current.0.load(Ordering::SeqCst), 0);
            owner.poll(due);
            owner.poll(due);
            assert_eq!(old.0.load(Ordering::SeqCst), 0);
            assert_eq!(current.0.load(Ordering::SeqCst), 1);
            assert!(
                permit
                    .delay(due, &mut Context::from_waker(Waker::noop()))
                    .is_ready()
            );
            drop(permit);
            let permit = owner.acquire(1, due).unwrap();
            assert!(
                permit
                    .delay(now, &mut Context::from_waker(&Waker::from(current.clone())))
                    .is_pending()
            );
            drop(permit);
            owner.poll(due);
            assert_eq!(current.0.load(Ordering::SeqCst), 1);
        }
    }
}

/// Adaptive state transitions, generation fencing, and shared ownership contracts.
#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::OnceLock;

    /// Synchronous observer recording emitted events and gauges.
    #[derive(Default)]
    struct Counts {
        events: Mutex<Vec<Event>>,

        active: Mutex<usize>,

        limit: Mutex<usize>,
    }

    static CLOCK_OWNER: OnceLock<std::sync::Weak<Adaptive<u8, Counts>>> = OnceLock::new();

    /// Verify clock callbacks run without the adaptive-state mutex held.
    fn reentrant_clock() -> Instant {
        if let Some(owner) = CLOCK_OWNER.get().and_then(std::sync::Weak::upgrade) {
            let _state = owner
                .state
                .try_lock()
                .expect("clock called while adaptive state is locked");
        }
        Instant::now()
    }

    impl Observer for Counts {
        /// Append an event in emission order.
        fn event(&self, event: Event) {
            self.events.lock().unwrap().push(event);
        }

        /// Save the latest active-work gauge.
        fn active(&self, active: usize) {
            *self.active.lock().unwrap() = active;
        }

        /// Save the latest admission-limit gauge.
        fn limit(&self, limit: usize) {
            *self.limit.lock().unwrap() = limit;
        }
    }

    /// Return small limits with independent backoff and recovery intervals.
    fn config() -> Config {
        Config {
            total: 4,
            per_key: 4,
            capacity: 2,
            backoff: Duration::from_millis(250),
            recovery: Duration::from_secs(1),
            retire_after: Duration::from_secs(60),
        }
    }

    /// Oversized peer backoff must not panic, poison state, or retire early.
    #[test]
    fn duration_overflow_adaptive_failure() {
        let owner = Adaptive::new(
            Config {
                capacity: 1,
                backoff: Duration::MAX,
                retire_after: Duration::ZERO,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let stale = owner.acquire(&1).unwrap();
        let failed = owner.acquire(&1).unwrap();
        failed.observe(Outcome::PeerFailure);
        stale.observe(Outcome::Verified);
        assert!(!owner.available(&1));
        assert!(!owner.hedge_available(&1));
        drop((stale, failed));
        assert_eq!(owner.state.lock().unwrap().active, 0);
        assert_eq!(*owner.observer.active.lock().unwrap(), 0);
        assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
        assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
    }

    /// Renewing a probe can overflow even when its previous retry was finite.
    #[test]
    fn duration_overflow_adaptive_probe_drop() {
        let owner = Adaptive::new(
            Config {
                backoff: Duration::MAX,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        drop(owner.acquire(&1).unwrap());
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry =
            Some(Deadline::At(Instant::now()));
        let probe = owner.acquire(&1).unwrap();
        drop(probe);
        assert_eq!(owner.state.lock().unwrap().active, 0);
        assert_eq!(*owner.observer.active.lock().unwrap(), 0);
        assert!(!owner.available(&1));
        assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
        assert!(owner.acquire(&2).is_ok());
    }

    /// Elapsed-time limits accept huge durations without forming deadlines.
    #[test]
    fn duration_overflow_elapsed_limits_remain_valid() {
        let owner = Adaptive::new(
            Config {
                capacity: 1,
                backoff: Duration::ZERO,
                recovery: Duration::MAX,
                retire_after: Duration::MAX,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let work = owner.acquire(&1).unwrap();
        work.observe(Outcome::LocalPressure);
        work.observe(Outcome::PeerFailure);
        drop(work);
        let probe = owner.acquire(&1).unwrap();
        probe.observe(Outcome::Verified);
        drop(probe);
        assert!(owner.available(&1));
        assert_eq!(owner.state.lock().unwrap().limit, config().total / 2);
        assert_eq!(
            owner.state.lock().unwrap().peers[&1].limit,
            config().per_key / 2
        );
        assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
        assert!(owner.acquire(&1).is_ok());
    }

    /// Clone panics leave the mutex usable and all admission capacity recoverable.
    #[test]
    fn adaptive_clone_panic_preserves_capacity_and_mutex() {
        use std::{
            panic::{AssertUnwindSafe, catch_unwind},
            sync::atomic::{AtomicUsize, Ordering},
        };

        static CLONES: AtomicUsize = AtomicUsize::new(0);
        static PANIC_AT: AtomicUsize = AtomicUsize::new(usize::MAX);
        static PANIC_KEY: AtomicUsize = AtomicUsize::new(usize::MAX);

        /// Key with selectable clone failures, including during retirement.
        #[derive(Eq, Ord, PartialEq, PartialOrd)]
        struct Key(usize);

        impl Clone for Key {
            fn clone(&self) -> Self {
                let clone = CLONES.fetch_add(1, Ordering::SeqCst) + 1;
                assert_ne!(clone, PANIC_AT.load(Ordering::SeqCst), "key clone failed");
                assert_ne!(
                    self.0,
                    PANIC_KEY.load(Ordering::SeqCst),
                    "retired key cloned"
                );
                Self(self.0)
            }
        }

        for existing in [false, true] {
            for panic_at in [1, 2] {
                let owner = Adaptive::new(
                    Config {
                        total: 2,
                        per_key: 2,
                        capacity: 1,
                        retire_after: Duration::ZERO,
                        ..config()
                    },
                    Counts::default(),
                    Instant::now,
                )
                .unwrap();
                let held = existing.then(|| owner.acquire(&Key(1)).unwrap());
                CLONES.store(0, Ordering::SeqCst);
                PANIC_AT.store(panic_at, Ordering::SeqCst);
                let result = catch_unwind(AssertUnwindSafe(|| owner.acquire(&Key(1))));
                PANIC_AT.store(usize::MAX, Ordering::SeqCst);
                assert!(result.is_err());
                assert_eq!(CLONES.load(Ordering::SeqCst), panic_at);
                {
                    let state = owner.state.lock().expect("clone panic poisoned state");
                    assert_eq!(state.active, usize::from(existing));
                    assert_eq!(state.peers.len(), usize::from(existing));
                    if existing {
                        assert_eq!(state.peers[&Key(1)].active, 1);
                    }
                }
                drop(held);
                let one = owner.acquire(&Key(1)).unwrap();
                let two = owner.acquire(&Key(1)).unwrap();
                assert!(matches!(owner.acquire(&Key(1)), Err(Error::Overloaded)));
                drop((one, two));
                assert_eq!(owner.state.lock().unwrap().active, 0);
                assert_eq!(*owner.observer.active.lock().unwrap(), 0);

                PANIC_KEY.store(1, Ordering::SeqCst);
                let replacement = owner.acquire(&Key(2)).expect("retirement must not clone");
                PANIC_KEY.store(usize::MAX, Ordering::SeqCst);
                {
                    let state = owner.state.lock().unwrap();
                    assert_eq!(state.peers.len(), 1);
                    assert!(!state.peers.contains_key(&Key(1)));
                    assert_eq!(state.peers[&Key(2)].active, 1);
                }
                drop(replacement);
                assert_eq!(owner.state.lock().unwrap().active, 0);
            }
        }
    }

    /// Stale success cannot undo failure and shared fences retain active work.
    #[test]
    fn fences_generation_exclusivity_and_local_pressure() {
        let owner = Adaptive::new(config(), Counts::default(), Instant::now).unwrap();
        assert!(owner.hedge_available(&1));
        let old = owner.acquire(&1).unwrap();
        let failed = owner.acquire(&1).unwrap();
        failed.observe(Outcome::PeerFailure);
        old.observe(Outcome::Verified);
        assert!(!owner.available(&1));
        let fence = failed.clone();
        drop((old, failed));
        assert_eq!(*owner.observer.active.lock().unwrap(), 1);
        drop(fence);
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry =
            Some(Deadline::At(Instant::now()));
        let probe = owner.acquire(&1).unwrap();
        assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
        probe.observe(Outcome::Verified);
        assert!(!owner.available(&1));
        drop(probe);
        assert!(owner.available(&1));
        let work = owner.acquire(&2).unwrap();
        owner.state.lock().unwrap().updated = Instant::now() - config().backoff;
        work.observe(Outcome::LocalPressure);
        assert_eq!(*owner.observer.limit.lock().unwrap(), 2);
        assert!(owner.available(&2));
        owner.state.lock().unwrap().updated = Instant::now() - config().recovery;
        work.observe(Outcome::Verified);
        assert_eq!(*owner.observer.limit.lock().unwrap(), 3);
    }

    /// Failed probes cannot clear retry, even at the last generation.
    #[test]
    fn generation_exhaustion_fences_failed_probe_success() {
        for generation in [0, u64::MAX - 1, u64::MAX] {
            let owner = Adaptive::new(config(), Counts::default(), Instant::now).unwrap();
            drop(owner.acquire(&1).unwrap());
            {
                let mut state = owner.state.lock().unwrap();
                let peer = state.peers.get_mut(&1).unwrap();
                peer.generation = Some(generation);
                peer.retry = Some(Deadline::At(Instant::now()));
            }
            let probe = owner.acquire(&1).unwrap();
            let fence = probe.clone();
            probe.observe(Outcome::PeerFailure);
            probe.observe(Outcome::Verified);
            assert!(owner.state.lock().unwrap().peers[&1].retry.is_some());
            assert!(!owner.available(&1));
            drop(probe);
            assert_eq!(*owner.observer.active.lock().unwrap(), 1);
            assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
            drop(fence);
            assert_eq!(*owner.observer.active.lock().unwrap(), 0);
            assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
            owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry =
                Some(Deadline::At(Instant::now()));
            let recovered = owner.acquire(&1).unwrap();
            recovered.observe(Outcome::Verified);
            assert!(!owner.available(&1));
            drop(recovered);
            assert!(owner.available(&1));
        }
    }

    /// Exhaustion fences every old permit and resets only after final release.
    #[test]
    fn generation_exhaustion_waits_for_live_permits_before_reuse() {
        let owner = Adaptive::new(
            Config {
                backoff: Duration::ZERO,
                recovery: Duration::ZERO,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let old = owner.acquire(&1).unwrap();
        owner
            .state
            .lock()
            .unwrap()
            .peers
            .get_mut(&1)
            .unwrap()
            .generation = Some(u64::MAX);
        let failed = owner.acquire(&1).unwrap();
        let fence = failed.clone();
        failed.observe(Outcome::PeerFailure);
        failed.observe(Outcome::PeerFailure);
        assert_eq!(owner.state.lock().unwrap().peers[&1].limit, 2);
        assert!(!owner.available(&1));
        assert!(!owner.hedge_available(&1));
        drop(failed);
        drop(fence);
        assert_eq!(*owner.observer.active.lock().unwrap(), 1);
        assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
        assert!(owner.acquire(&2).is_ok());
        drop(old);
        assert_eq!(*owner.observer.active.lock().unwrap(), 0);
        assert!(owner.available(&1));
        let recovered = owner.acquire(&1).unwrap();
        recovered.observe(Outcome::Verified);
        drop(recovered);
        assert!(owner.available(&1));
        assert_eq!(owner.state.lock().unwrap().peers[&1].limit, 3);
    }

    /// Success at the ceiling must not delay an eligible pressure reduction.
    #[test]
    fn recovery_at_ceiling_preserves_pressure_eligibility() {
        fn now() -> Instant {
            static NOW: OnceLock<Instant> = OnceLock::new();
            *NOW.get_or_init(Instant::now)
        }

        for recovery in [Duration::ZERO, config().recovery] {
            let config = Config {
                recovery,
                ..config()
            };
            let owner = Adaptive::new(config, Counts::default(), now).unwrap();
            let work = owner.acquire(&1).unwrap();
            let updated = now() - config.backoff.max(config.recovery);
            owner.state.lock().unwrap().updated = updated;

            for _ in 0..2 {
                work.observe(Outcome::Verified);
                let state = owner.state.lock().unwrap();
                assert_eq!(state.limit, config.total);
                assert_eq!(state.updated, updated);
            }
            work.observe(Outcome::LocalPressure);
            assert_eq!(*owner.observer.limit.lock().unwrap(), config.total / 2);
            assert_eq!(owner.state.lock().unwrap().updated, now());
            assert_eq!(owner.state.lock().unwrap().peers[&1].limit, config.per_key);
            assert!(owner.available(&1));
            assert_eq!(
                *owner.observer.events.lock().unwrap(),
                [
                    Event::Accepted,
                    Event::Verified,
                    Event::Verified,
                    Event::LocalPressure,
                ]
            );
        }
    }

    /// Restoring the final slot still starts backoff and rate-limits pressure.
    #[test]
    fn recovery_restoring_slot_advances_pressure_backoff() {
        fn now() -> Instant {
            static NOW: OnceLock<Instant> = OnceLock::new();
            *NOW.get_or_init(Instant::now)
        }

        let config = config();
        let owner = Adaptive::new(config, Counts::default(), now).unwrap();
        let work = owner.acquire(&1).unwrap();
        {
            let mut state = owner.state.lock().unwrap();
            state.limit = config.total - 1;
            state.updated = now() - config.recovery;
        }
        work.observe(Outcome::Verified);
        assert_eq!(*owner.observer.limit.lock().unwrap(), config.total);
        assert_eq!(owner.state.lock().unwrap().updated, now());
        work.observe(Outcome::LocalPressure);
        assert_eq!(*owner.observer.limit.lock().unwrap(), config.total);

        owner.state.lock().unwrap().updated = now() - config.backoff;
        work.observe(Outcome::LocalPressure);
        assert_eq!(*owner.observer.limit.lock().unwrap(), config.total / 2);
        work.observe(Outcome::LocalPressure);
        assert_eq!(*owner.observer.limit.lock().unwrap(), config.total / 2);
    }

    /// Idle age and recovery use independent clocks without wall-clock sleeps.
    mod idle_retirement {
        use super::*;
        use std::cell::Cell;

        thread_local! {
            static CLOCK: Cell<Instant> = Cell::new(Instant::now());
        }

        fn now() -> Instant {
            CLOCK.get()
        }

        fn advance(duration: Duration) {
            CLOCK.set(now() + duration);
        }

        #[test]
        fn waits_from_last_owner_and_restarts_after_reuse() {
            for outcome in [Outcome::Neutral, Outcome::Verified] {
                let config = Config {
                    capacity: 1,
                    recovery: Duration::MAX,
                    ..config()
                };
                let owner = Adaptive::new(config, Counts::default(), now).unwrap();
                let first = owner.acquire(&1).unwrap();
                let last = owner.acquire(&1).unwrap();
                let fence = last.clone();
                advance(config.retire_after);
                first.observe(outcome);
                drop(first);
                advance(config.retire_after);
                drop(last);
                assert_eq!(owner.state.lock().unwrap().peers[&1].active, 1);
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                drop(fence);
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));

                advance(config.retire_after - Duration::from_nanos(1));
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                let reused = owner.acquire(&1).unwrap();
                advance(config.retire_after);
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                drop(reused);
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                advance(config.retire_after - Duration::from_nanos(1));
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                advance(Duration::from_nanos(1));
                let replacement = owner.acquire(&2).unwrap();
                assert!(!owner.state.lock().unwrap().peers.contains_key(&1));
                drop(replacement);
                assert_eq!(*owner.observer.active.lock().unwrap(), 0);
            }
        }

        #[test]
        fn probe_release_requires_idle_age_and_expired_backoff() {
            for outcome in [Outcome::Neutral, Outcome::Verified, Outcome::PeerFailure] {
                let config = Config {
                    capacity: 1,
                    backoff: Duration::from_secs(3),
                    retire_after: Duration::from_secs(2),
                    ..config()
                };
                let owner = Adaptive::new(config, Counts::default(), now).unwrap();
                let failed = owner.acquire(&1).unwrap();
                failed.observe(Outcome::PeerFailure);
                drop(failed);
                advance(config.backoff);
                let probe = owner.acquire(&1).unwrap();
                probe.observe(outcome);
                advance(config.backoff + config.retire_after);
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                drop(probe);
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                advance(config.retire_after - Duration::from_nanos(1));
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                advance(Duration::from_nanos(1));
                if outcome != Outcome::Verified {
                    assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                    assert!(matches!(owner.acquire(&1), Err(Error::Unavailable)));
                    advance(config.backoff - config.retire_after);
                }
                assert!(owner.acquire(&2).is_ok());
            }
        }

        #[test]
        fn acquisition_and_drop_preserve_recovery_throttle() {
            let config = Config {
                backoff: Duration::ZERO,
                recovery: Duration::from_secs(10),
                ..config()
            };
            let owner = Adaptive::new(config, Counts::default(), now).unwrap();
            let failed = owner.acquire(&1).unwrap();
            failed.observe(Outcome::PeerFailure);
            drop(failed);
            let updated = now();
            advance(config.recovery / 2);
            let probe = owner.acquire(&1).unwrap();
            probe.observe(Outcome::Verified);
            drop(probe);
            assert_eq!(owner.state.lock().unwrap().peers[&1].updated, updated);
            assert_eq!(owner.state.lock().unwrap().peers[&1].limit, 2);
            advance(config.recovery / 2);
            let work = owner.acquire(&1).unwrap();
            work.observe(Outcome::Verified);
            drop(work);
            assert_eq!(owner.state.lock().unwrap().peers[&1].limit, 3);
            let updated = now();
            advance(config.recovery - Duration::from_nanos(1));
            let early = owner.acquire(&1).unwrap();
            early.observe(Outcome::Verified);
            drop(early);
            assert_eq!(owner.state.lock().unwrap().peers[&1].updated, updated);
            assert_eq!(owner.state.lock().unwrap().peers[&1].limit, 3);
            advance(Duration::from_nanos(1));
            let ready = owner.acquire(&1).unwrap();
            ready.observe(Outcome::Verified);
            assert_eq!(owner.state.lock().unwrap().peers[&1].limit, 4);
        }

        #[test]
        fn zero_and_maximum_idle_age_preserve_live_ownership() {
            for retire_after in [Duration::ZERO, Duration::MAX] {
                let owner = Adaptive::new(
                    Config {
                        capacity: 1,
                        retire_after,
                        ..config()
                    },
                    Counts::default(),
                    now,
                )
                .unwrap();
                let work = owner.acquire(&1).unwrap();
                advance(Duration::from_secs(120));
                assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                drop(work);
                if retire_after.is_zero() {
                    assert!(owner.acquire(&2).is_ok());
                } else {
                    assert!(matches!(owner.acquire(&2), Err(Error::Overloaded)));
                    assert!(owner.acquire(&1).is_ok());
                }
            }
        }
    }

    /// Only sufficiently old, idle, eligible peer records may be retired.
    #[test]
    fn capacity_preserves_live_work_and_stale_backoff_then_retires_idle() {
        let owner = Adaptive::new(config(), Counts::default(), Instant::now).unwrap();
        let one = owner.acquire(&1).unwrap();
        let two = owner.acquire(&2).unwrap();
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        one.observe(Outcome::PeerFailure);
        drop(one);
        owner
            .state
            .lock()
            .unwrap()
            .peers
            .get_mut(&1)
            .unwrap()
            .idle_since = Some(Instant::now() - Duration::from_secs(61));
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        assert!(owner.state.lock().unwrap().peers.contains_key(&1));
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry =
            Some(Deadline::At(Instant::now()));
        let probe = owner.acquire(&1).unwrap();
        drop(probe);
        assert!(!owner.available(&1));
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        drop(two);
        owner
            .state
            .lock()
            .unwrap()
            .peers
            .get_mut(&2)
            .unwrap()
            .idle_since = Some(Instant::now() - Duration::from_secs(61));
        assert!(owner.acquire(&3).is_ok());
        assert_eq!(owner.state.lock().unwrap().peers.len(), 2);
    }

    /// Reject invalid geometry and enforce aggregate and per-key caps.
    #[test]
    fn validation_and_caps() {
        for (total, per_key, capacity) in [(0, 1, 1), (1, 0, 1), (1, 2, 1), (1, 1, 0)] {
            assert!(matches!(
                Adaptive::<u8, _>::new(
                    Config {
                        total,
                        per_key,
                        capacity,
                        ..config()
                    },
                    Counts::default(),
                    Instant::now
                ),
                Err(Error::InvalidInput)
            ));
        }
        let owner = Adaptive::new(
            Config {
                total: 2,
                per_key: 1,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let one = owner.acquire(&1).unwrap();
        assert!(matches!(owner.acquire(&1), Err(Error::Overloaded)));
        let two = owner.acquire(&2).unwrap();
        assert!(!owner.hedge_available(&3));
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        one.observe(Outcome::Neutral);
        drop((one, two));
        assert_eq!(*owner.observer.active.lock().unwrap(), 0);
    }

    /// Recovery saturates even when limits span the machine word.
    #[test]
    fn full_width_limits_recover_without_overflow() {
        let owner = Adaptive::new(
            Config {
                total: usize::MAX,
                per_key: usize::MAX,
                recovery: Duration::ZERO,
                ..config()
            },
            Counts::default(),
            Instant::now,
        )
        .unwrap();
        let permit = owner.acquire(&()).unwrap();
        permit.observe(Outcome::Verified);
        assert_eq!(*owner.observer.limit.lock().unwrap(), usize::MAX);
        assert_eq!(owner.state.lock().unwrap().peers[&()].limit, usize::MAX);
    }

    /// Probe release invokes the injected clock only after releasing adaptive state.
    #[test]
    fn probe_drop_calls_clock_outside_state_lock() {
        let owner = Adaptive::new(config(), Counts::default(), reentrant_clock).unwrap();
        CLOCK_OWNER.set(Arc::downgrade(&owner)).unwrap();
        let failed = owner.acquire(&1).unwrap();
        failed.observe(Outcome::PeerFailure);
        drop(failed);
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry =
            Some(Deadline::At(Instant::now()));
        let probe = owner.acquire(&1).unwrap();
        drop(probe);
    }
}
