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

    generation: u64,

    retry: Option<Instant>,

    probe: bool,

    updated: Instant,
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
            s.peers
                .get(key)
                .is_none_or(|p| !p.probe && p.retry.is_none_or(|at| now >= at))
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
                        && p.retry.is_none_or(|retry| now >= retry)
                        && now.saturating_duration_since(p.updated) >= self.config.retire_after
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
            generation: 0,
            retry: None,
            probe: false,
            updated: now,
        });
        if peer.probe || peer.retry.is_some_and(|at| now < at) {
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
        let generation = peer.generation;
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
        if peer.generation != self.generation {
            return;
        }
        match outcome {
            Outcome::PeerFailure => {
                peer.limit = (peer.limit / 2).max(1);
                peer.generation = peer.generation.saturating_add(1);
                peer.retry = Some(now + config.backoff);
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
        let now = self.probe.then(|| (self.owner.now)());
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let peer = state
            .peers
            .get_mut(&self.key)
            .expect("live permit retains key");
        peer.active -= 1;
        if self.probe {
            peer.probe = false;
            if peer.retry.is_some() {
                let now = now.expect("probe clock sampled before locking");
                peer.retry = Some(now + self.owner.config.backoff);
                peer.updated = now;
            }
        }
        state.active -= 1;
        self.owner.observer.active(state.active);
    }
}

/// Bounded worker-local endpoint failure tracking, independent of adaptive limits.
mod circuit {
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

        retry_at: Instant,

        probe_until: Option<Instant>,
    }

    impl Circuit {
        /// Require both retry backoff and any abandoned probe timeout to expire.
        fn available(&self, now: Instant) -> bool {
            now >= self.retry_at && self.probe_until.is_none_or(|until| now >= until)
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
                retry_at: now,
                probe_until: None,
            });
            state.failures = state.failures.saturating_add(1);
            state.retry_at = now + backoff(key, state.failures);
            state.probe_until = None;
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
            state.probe_until = Some(now + self.probe_timeout);
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

        waker: Option<Waker>,

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
        pub fn new(keys: &[K]) -> Self {
            Self(Mutex::new(State {
                targets: keys
                    .iter()
                    .map(|key| {
                        (
                            key.clone(),
                            Target {
                                admission: None,
                                queue: VecDeque::new(),
                                waker: None,
                                closed: false,
                            },
                        )
                    })
                    .collect(),
                cursor: 0,
            }))
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
                if let Ok(reservation) = admission.reserve() {
                    state.cursor = (index + 1) % state.targets.len();
                    return Ok(Offer {
                        handoff: self.clone(),
                        target: index,
                        reservation,
                    });
                }
            }
            Err(Error::Overloaded)
        }

        /// Pop at most the caller's budget while retaining each item's admission.
        pub fn pop_batch<const N: usize>(
            &self,
            key: &K,
            waker: &Waker,
            budget: usize,
        ) -> Result<Batch<T, A::Reservation, N>> {
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            let target = state.target(key).ok_or(Error::InvalidInput)?;
            if let Some(old) = &mut target.waker {
                old.clone_from(waker);
            } else {
                target.waker = Some(waker.clone());
            }
            Ok(std::array::from_fn(|index| {
                if index < budget {
                    target.queue.pop_front()
                } else {
                    None
                }
            }))
        }

        /// Close a target and drop queued ownership outside the shared lock.
        pub fn close(&self, key: &K) {
            let queued = {
                let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
                let Some(target) = state.target(key) else {
                    return;
                };
                target.closed = true;
                std::mem::take(&mut target.queue)
            };
            drop(queued);
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
                waker.wake();
            }
            Ok(())
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
            let mut state = self.owner.state.lock().expect("hedge alarm lock");
            let alarm = state.alarms.get_mut(&self.id).expect("live hedge alarm");
            if now >= alarm.due {
                Poll::Ready(())
            } else {
                alarm.wake = Some(cx.waker().clone());
                Poll::Pending
            }
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
            sync::atomic::{AtomicUsize, Ordering},
            task::Wake,
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
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry = Some(Instant::now());
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
            .updated = Instant::now() - Duration::from_secs(61);
        assert!(matches!(owner.acquire(&3), Err(Error::Overloaded)));
        assert!(owner.state.lock().unwrap().peers.contains_key(&1));
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry = Some(Instant::now());
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
            .updated = Instant::now() - Duration::from_secs(61);
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
        owner.state.lock().unwrap().peers.get_mut(&1).unwrap().retry = Some(Instant::now());
        let probe = owner.acquire(&1).unwrap();
        drop(probe);
    }
}
