//! Ambient worker time and entropy, wire deadlines, and deadline registries.
//!
//! Production reads the host clock and the kernel entropy source directly:
//! without the `simulation` feature [`now`] is exactly [`Instant::now`] and
//! touches no thread-local state. With the feature, a thread may select a
//! simulated [`Environment`] (usually from [`SimulationClock::environment`])
//! whose clock is a world's [`SimClock`] and whose entropy is a role's
//! replayable [`SimEntropy`] stream. Deadline registries are explicitly driven
//! by their owner; neither registering a deadline nor reading a clock starts a
//! timer.
use crate::{Error, Result};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    future::Future,
    marker::PhantomData,
    pin::Pin,
    rc::Rc,
    sync::OnceLock,
    task::{Context, Poll, Waker},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
#[cfg(feature = "simulation")]
use uring::Clock as _;
use uring::{Entropy as _, SystemEntropy};

#[cfg(feature = "simulation")]
pub use uring_runtime_sim::{DEFAULT_ENTROPY_DOMAIN, SimClock, SimEntropy};

/// Time and entropy selection retained by a worker role across individual polls.
///
/// Without the `simulation` feature this is a zero-sized handle for the host.
/// A simulated environment is `Send` (a role can move to its group thread)
/// and its clones share one entropy cursor.
#[derive(Clone, Debug, Default)]
pub struct Environment {
    #[cfg(feature = "simulation")]
    simulated: Option<Simulated>,
}

/// A world's clock and a role's shared entropy cursor.
#[cfg(feature = "simulation")]
#[derive(Clone, Debug)]
struct Simulated {
    clock: SimClock,

    entropy: SimEntropy,
}

/// One clock per simulated world. Give each node/worker/role a stable, unique
/// stream ID and retain its [`Environment`]; cloning a role shares its entropy
/// cursor.
///
/// A thin wrapper over the library's [`SimClock`] that keeps the old Racer
/// constructor names and [`Error`] results.
#[cfg(feature = "simulation")]
#[derive(Clone, Debug)]
pub struct SimulationClock(SimClock);

/// An absolute monotonic deadline, mapped to wire time through a stable anchor.
#[derive(Clone, Copy, Debug)]
pub struct Deadline(pub Instant);

/// Worker-local deadlines ordered by time and then registration order.
#[derive(Default)]
pub struct Registry {
    next: Cell<u64>,

    pending: RefCell<BTreeMap<(Instant, u64), Option<Waker>>>,
}

/// Removes its deadline on drop; expiration is observed only after registry polling.
pub struct Registration {
    table: Rc<Registry>,

    key: (Instant, u64),
}

/// Observes wall-clock drift without choosing how applications invalidate data.
#[derive(Default)]
pub struct Observer {
    sample: Option<(SystemTime, Instant)>,

    epoch: u64,
}

#[cfg(feature = "simulation")]
thread_local! {
    static REQUIRE_SIMULATED: Cell<bool> = const { Cell::new(false) };
    static CURRENT: RefCell<Environment> = const {
        RefCell::new(Environment { simulated: None })
    };
}

/// Fail closed if a simulated role accidentally selects real clock/entropy.
#[cfg(feature = "simulation")]
pub fn require_simulated() -> SimulationRequired {
    SimulationRequired(
        REQUIRE_SIMULATED.with(|required| required.replace(true)),
        PhantomData,
    )
}

#[cfg(feature = "simulation")]
/// Restores the thread's previous host-access requirement when dropped.
pub struct SimulationRequired(bool, PhantomData<Rc<()>>);

#[cfg(feature = "simulation")]
impl Drop for SimulationRequired {
    /// Restore the strictness selected before this thread entered the guard.
    fn drop(&mut self) {
        REQUIRE_SIMULATED.with(|required| required.set(self.0));
    }
}

/// A guard belongs to the thread that entered it. Never hold it across an await;
/// use `Environment::scope` to enter only while polling or dropping a future.
pub struct Guard {
    #[cfg(feature = "simulation")]
    previous: Environment,

    _local: PhantomData<Rc<()>>,
}

impl Environment {
    /// The host environment: real clock and kernel entropy.
    pub fn host() -> Self {
        Self::default()
    }

    /// An environment that reads time from `clock` and entropy from
    /// `entropy`. Usually created with [`SimulationClock::environment`].
    #[cfg(feature = "simulation")]
    pub fn simulated(clock: SimClock, entropy: SimEntropy) -> Self {
        Self {
            simulated: Some(Simulated { clock, entropy }),
        }
    }

    /// The simulated world clock, or `None` for the host environment.
    #[cfg(feature = "simulation")]
    pub fn sim_clock(&self) -> Option<&SimClock> {
        self.simulated.as_ref().map(|simulated| &simulated.clock)
    }

    /// The role's simulated entropy stream (shared with clones), or `None`
    /// for the host environment.
    #[cfg(feature = "simulation")]
    pub fn sim_entropy(&self) -> Option<&SimEntropy> {
        self.simulated.as_ref().map(|simulated| &simulated.entropy)
    }

    /// Whether this environment uses a simulated source.
    pub fn is_simulated(&self) -> bool {
        #[cfg(feature = "simulation")]
        return self.simulated.is_some();
        #[cfg(not(feature = "simulation"))]
        false
    }

    /// Capture this thread's selected environment, or the host environment.
    pub fn current() -> Self {
        #[cfg(feature = "simulation")]
        return CURRENT.with(|current| current.borrow().clone());
        #[cfg(not(feature = "simulation"))]
        Self::default()
    }

    /// Select this environment until the returned thread-local guard is dropped.
    pub fn enter(&self) -> Guard {
        Guard {
            #[cfg(feature = "simulation")]
            previous: CURRENT.with(|current| current.replace(self.clone())),
            _local: PhantomData,
        }
    }

    /// Select this environment only while polling or dropping the given future.
    pub fn scope<F: Future>(&self, future: F) -> Scoped<F> {
        Scoped {
            environment: self.clone(),
            future: Some(Box::pin(future)),
        }
    }
}

impl Drop for Guard {
    /// Restore this thread's previous environment, including during unwinding.
    fn drop(&mut self) {
        #[cfg(feature = "simulation")]
        CURRENT.with(|current| current.replace(std::mem::take(&mut self.previous)));
    }
}

/// A future that restores its captured environment for polling and destruction.
pub struct Scoped<F> {
    environment: Environment,

    future: Option<Pin<Box<F>>>,
}

impl<F: Future> Future for Scoped<F> {
    /// Preserve the wrapped future's output without imposing application policy.
    type Output = F::Output;

    /// Enter the captured environment for one poll, restoring it on return.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let _environment = this.environment.enter();
        this.future
            .as_mut()
            .expect("scoped future")
            .as_mut()
            .poll(cx)
    }
}

impl<F> Drop for Scoped<F> {
    /// Destroy the inner future before leaving its captured environment.
    fn drop(&mut self) {
        let _environment = self.environment.enter();
        self.future.take();
    }
}

#[cfg(feature = "simulation")]
/// Return the active simulated world's seed, or None for a host environment.
pub fn simulation_seed() -> Option<u64> {
    CURRENT.with(|current| {
        current
            .borrow()
            .simulated
            .as_ref()
            .map(|simulated| simulated.clock.seed())
    })
}

/// Read monotonic time from the selected environment.
#[inline]
pub fn now() -> Instant {
    #[cfg(feature = "simulation")]
    if let Some(now) = with_simulation(|simulated| simulated.clock.now()) {
        return now;
    }
    Instant::now()
}

/// Read wall time, including simulated wall-clock corrections.
#[inline]
pub fn wall_now() -> SystemTime {
    #[cfg(feature = "simulation")]
    if let Some(wall) = with_simulation(|simulated| simulated.clock.wall_now()) {
        return wall;
    }
    SystemTime::now()
}

/// Stable mapping anchor, unaffected by later wall-clock corrections. All roles
/// of a simulated world use the same anchor, including after nested world scopes.
pub fn clock_anchor() -> (Instant, SystemTime) {
    #[cfg(feature = "simulation")]
    if let Some(anchor) =
        with_simulation(|simulated| (simulated.clock.origin(), simulated.clock.wall_origin()))
    {
        return anchor;
    }

    static ANCHOR: OnceLock<(Instant, SystemTime)> = OnceLock::new();
    *ANCHOR.get_or_init(|| (Instant::now(), SystemTime::now()))
}

/// Fill bytes from the host entropy source or the selected replayable role stream.
pub fn fill_random(bytes: &mut [u8]) -> Result<()> {
    #[cfg(feature = "simulation")]
    if let Some(result) = with_simulation(|simulated| simulated.entropy.fill(bytes)) {
        return result.map_err(Error::from);
    }
    SystemEntropy.fill(bytes).map_err(Error::from)
}

/// Run `read` against the selected simulated role without cloning it, or
/// return `None` for the host, rejecting host fallback inside a strict scope.
#[cfg(feature = "simulation")]
fn with_simulation<T>(read: impl FnOnce(&Simulated) -> T) -> Option<T> {
    let result = CURRENT.with(|current| current.borrow().simulated.as_ref().map(read));
    if result.is_none() {
        REQUIRE_SIMULATED
            .with(|required| assert!(!required.get(), "DST escaped into host time/entropy"));
    }
    result
}

#[cfg(feature = "simulation")]
impl SimulationClock {
    /// Create a world with the process monotonic origin and a fixed wall epoch
    /// (`1_800_000_000` seconds after the Unix epoch).
    pub fn new(seed: u64) -> Self {
        Self(SimClock::new(seed))
    }

    /// Create a world with explicit monotonic and wall-clock anchors.
    pub fn new_at(seed: u64, monotonic: Instant, wall: SystemTime) -> Self {
        Self(SimClock::new_at(seed, monotonic, wall))
    }

    /// Wrap an existing library clock (clones share its time).
    pub fn from_sim_clock(clock: SimClock) -> Self {
        Self(clock)
    }

    /// The library clock, for building simulated backends.
    pub fn sim_clock(&self) -> &SimClock {
        &self.0
    }

    /// The world's seed.
    pub fn seed(&self) -> u64 {
        self.0.seed()
    }

    /// Creates a fresh entropy stream. Call once per role incarnation and retain
    /// the result; calling again with the same ID intentionally replays its bytes.
    /// The default domain is [`DEFAULT_ENTROPY_DOMAIN`]
    /// (`b"uring-runtime.entropy.v1\0"`). Applications with persisted replay
    /// corpora should own an explicit, versioned domain instead.
    pub fn environment(&self, stream: u64) -> Environment {
        self.environment_with_entropy_domain(stream, DEFAULT_ENTROPY_DOMAIN)
    }

    /// Creates a stream in a caller-selected domain. Bytes are SHA-256 blocks of
    /// domain || seed_le64 || stream_le64 || counter_le64, starting at counter 0.
    /// Chunking reads and cloning environments do not change the byte stream.
    pub fn environment_with_entropy_domain(
        &self,
        stream: u64,
        domain: impl AsRef<[u8]>,
    ) -> Environment {
        Environment::simulated(
            self.0.clone(),
            SimEntropy::new(domain, self.0.seed(), stream),
        )
    }

    /// Advance both clocks, panicking on overflow without poisoning the clock.
    pub fn advance(&self, duration: Duration) {
        self.0.advance(duration);
    }

    /// Advance atomically or leave both clocks unchanged on overflow
    /// ([`Error::InvalidInput`]).
    pub fn checked_advance(&self, duration: Duration) -> Result<()> {
        self.0.checked_advance(duration).map_err(Error::from)
    }

    /// Return the total monotonic time advanced since world creation.
    pub fn elapsed(&self) -> Duration {
        self.0.elapsed()
    }

    /// Correct wall time without changing elapsed time or the stable anchor.
    pub fn set_wall_time(&self, wall: SystemTime) {
        self.0.set_wall_time(wall);
    }
}

impl Deadline {
    /// Map through the stable environment clock anchor, not a renewed timeout.
    /// Submillisecond precision is truncated, never rounded up.
    pub fn to_unix_millis(self) -> Result<u64> {
        self.to_unix_millis_at(clock_anchor())
    }

    /// Decode using the same stable anchor, accounting for its fractional millis.
    pub fn from_unix_millis(value: u64) -> Result<Self> {
        Self::from_unix_millis_at(value, clock_anchor())
    }

    /// Encode relative to an explicit anchor, rejecting unrepresentable wall time.
    fn to_unix_millis_at(self, (mono, wall): (Instant, SystemTime)) -> Result<u64> {
        let time = if self.0 >= mono {
            wall.checked_add(self.0.duration_since(mono))
        } else {
            wall.checked_sub(mono.duration_since(self.0))
        }
        .ok_or(Error::InvalidInput)?;
        unix_millis(time)
    }

    /// Decode relative to an explicit anchor without extending the wire deadline.
    fn from_unix_millis_at(value: u64, (mono, wall): (Instant, SystemTime)) -> Result<Self> {
        let base = unix_millis(wall)?;
        let fraction = wall
            .duration_since(UNIX_EPOCH)
            .map_err(|_| Error::InvalidInput)?
            .subsec_nanos()
            % 1_000_000;
        let instant = if value >= base {
            mono.checked_add(Duration::from_millis(value - base))
        } else {
            mono.checked_sub(Duration::from_millis(base - value))
        }
        .and_then(|i| i.checked_sub(Duration::from_nanos(u64::from(fraction))))
        .ok_or(Error::InvalidInput)?;
        Ok(Self(instant))
    }
}

impl Registry {
    /// Start at a chosen identifier to exercise exhaustion without many entries.
    #[cfg(any(test, feature = "test-util"))]
    pub fn with_next_id(next: u64) -> Self {
        Self {
            next: Cell::new(next),
            ..Self::default()
        }
    }

    /// Register a deadline after caller-owned admission has bounded the allocation.
    pub fn register(self: &Rc<Self>, deadline: Instant) -> Result<Registration> {
        let id = self.next.get();
        self.next.set(id.checked_add(1).ok_or(Error::Overloaded)?);
        let key = (deadline, id);
        self.pending.borrow_mut().insert(key, None);
        Ok(Registration {
            table: self.clone(),
            key,
        })
    }

    /// Count entries that have not expired or been dropped.
    pub fn len(&self) -> usize {
        self.pending.borrow().len()
    }

    /// Whether there are no deadlines waiting for an owner polling turn.
    pub fn is_empty(&self) -> bool {
        self.pending.borrow().is_empty()
    }

    /// Earliest pending deadline, including due entries left by a polling budget.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.pending
            .borrow()
            .first_key_value()
            .map(|(key, _)| key.0)
    }

    /// Expire at most budget entries, waking their tasks outside table borrows.
    pub fn poll(&self, now: Instant, budget: usize) -> usize {
        let mut wakers = Vec::new();
        let mut expired = 0;
        {
            let mut pending = self.pending.borrow_mut();
            while expired < budget
                && pending
                    .first_key_value()
                    .is_some_and(|(key, _)| key.0 <= now)
            {
                let (_, waker) = pending.pop_first().expect("due deadline");
                if let Some(waker) = waker {
                    wakers.push(waker);
                }
                expired += 1;
            }
        }
        for waker in wakers {
            waker.wake();
        }
        expired
    }
}

impl Registration {
    /// Observe driver-processed expiration and register the latest waiting task.
    /// This does not read the clock or apply scope policy. The owner must drive
    /// Registry::poll; callers choose cancellation and deadline error precedence.
    pub fn poll_expired(&self, cx: &mut Context<'_>) -> Poll<()> {
        // RawWaker clone/drop callbacks may reenter this registration or registry.
        let new = cx.waker().clone();
        let mut pending = self.table.pending.borrow_mut();
        let Some(waker) = pending.get_mut(&self.key) else {
            // Keys are never reused. Only expiry can remove a live handle's key.
            drop(pending);
            return Poll::Ready(());
        };
        let old = waker.replace(new);
        drop(pending);
        drop(old);
        Poll::Pending
    }

    /// Translate observed expiration after the caller checks its scope policy.
    #[cfg(test)]
    fn check(&self, waker: &Waker) -> Result<()> {
        match self.poll_expired(&mut Context::from_waker(waker)) {
            Poll::Ready(()) => Err(Error::DeadlineExceeded),
            Poll::Pending => Ok(()),
        }
    }
}

impl Drop for Registration {
    /// Remove the deadline and release its waiter outside the registry borrow.
    fn drop(&mut self) {
        let removed = self.table.pending.borrow_mut().remove(&self.key);
        drop(removed);
    }
}

impl Observer {
    /// Read the saturating count of samples whose clock relationship was uncertain.
    pub fn epoch(&self) -> u64 {
        self.epoch
    }

    /// Record a sample, returning true for backward time or drift above threshold.
    pub fn observe(&mut self, wall: SystemTime, monotonic: Instant, threshold: Duration) -> bool {
        let uncertain = self.sample.is_some_and(|(previous_wall, previous_mono)| {
            match (
                wall.duration_since(previous_wall),
                monotonic.checked_duration_since(previous_mono),
            ) {
                (Ok(wall_elapsed), Some(elapsed)) => wall_elapsed.abs_diff(elapsed) > threshold,
                _ => true,
            }
        });
        self.sample = Some((wall, monotonic));
        if uncertain {
            self.epoch = self.epoch.saturating_add(1);
        }
        uncertain
    }
}

/// Milliseconds since the Unix epoch, rejecting pre-epoch and overflowing times.
pub fn unix_millis(time: SystemTime) -> Result<u64> {
    u64::try_from(
        time.duration_since(UNIX_EPOCH)
            .map_err(|_| Error::InvalidInput)?
            .as_millis(),
    )
    .map_err(|_| Error::InvalidInput)
}

#[cfg(test)]
mod host_tests {
    use super::*;

    /// Host clock mapping retains one stable process anchor.
    #[test]
    fn host_environment_uses_a_stable_anchor() {
        let _host = Environment::default().enter();
        let first = clock_anchor();
        assert_eq!(clock_anchor(), first);
        assert!(now() >= first.0);
        fill_random(&mut [0; 32]).unwrap();
    }
}

/// Deadline conversion regressions.
#[cfg(test)]
mod deadline_tests {
    use super::*;

    /// Millisecond deadlines round-trip on either side of a fractional anchor.
    #[test]
    fn millisecond_mapping_preserves_both_sides_of_fractional_anchor() {
        let mono = Instant::now();
        let wall = UNIX_EPOCH + Duration::from_secs(100) + Duration::from_nanos(123_456);
        for value in [0, 99_999, 100_000, 100_001, 200_000, u64::MAX] {
            let decoded = Deadline::from_unix_millis_at(value, (mono, wall)).unwrap();
            assert_eq!(decoded.to_unix_millis_at((mono, wall)), Ok(value));
        }
        assert_eq!(Deadline(mono).to_unix_millis_at((mono, wall)), Ok(100_000));
        let decoded = Deadline::from_unix_millis_at(100_000, (mono, wall)).unwrap();
        assert_eq!(
            mono.duration_since(decoded.0),
            Duration::from_nanos(123_456)
        );
    }

    /// Invalid wall times are rejected rather than wrapped or rounded into range.
    #[test]
    fn millisecond_mapping_rejects_pre_epoch_and_overflow() {
        let mono = Instant::now();
        let before = UNIX_EPOCH - Duration::from_nanos(1);
        assert_eq!(unix_millis(before), Err(Error::InvalidInput));
        assert!(matches!(
            Deadline::from_unix_millis_at(0, (mono, before)),
            Err(Error::InvalidInput)
        ));
        assert_eq!(
            Deadline(mono - Duration::from_secs(1)).to_unix_millis_at((mono, UNIX_EPOCH)),
            Err(Error::InvalidInput)
        );
        let overflow = UNIX_EPOCH
            .checked_add(Duration::from_millis(u64::MAX))
            .unwrap()
            + Duration::from_millis(1);
        assert_eq!(unix_millis(overflow), Err(Error::InvalidInput));
    }
}

/// Pure registry ordering, expiration budgets, and reentrant waker disposal.
#[cfg(test)]
mod registry_tests {
    use super::*;

    /// Equal deadlines retain registration order and replace wakers outside borrows.
    #[test]
    fn deadlines_are_ordered_by_time_then_registration_and_drop_is_reentrant() {
        thread_local! {
            static TABLE: RefCell<Option<Rc<Registry>>> = const { RefCell::new(None) };
        }

        /// Reenters the registry when a retired waker releases its last owner.
        struct OnDrop;

        impl std::task::Wake for OnDrop {
            /// Reject waking a registration owner that should only be discarded.
            fn wake(self: std::sync::Arc<Self>) {
                panic!("replaced registration must not wake its retired owner");
            }
        }

        impl Drop for OnDrop {
            /// Assert registry mutation is available during retired waker disposal.
            fn drop(&mut self) {
                TABLE.with(|slot| {
                    let table = slot.borrow().as_ref().unwrap().clone();
                    assert!(table.pending.try_borrow_mut().is_ok());
                });
            }
        }
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let late = table
            .register(now + std::time::Duration::from_secs(1))
            .unwrap();
        let first = table.register(now).unwrap();
        let second = table.register(now).unwrap();
        TABLE.with(|slot| slot.replace(Some(table.clone())));
        first
            .check(&Waker::from(std::sync::Arc::new(OnDrop)))
            .unwrap();
        first.check(Waker::noop()).unwrap();
        assert_eq!(table.poll(now, 1), 1);
        assert!(first.check(Waker::noop()).is_err());
        assert!(second.check(Waker::noop()).is_ok());
        assert!(late.check(Waker::noop()).is_ok());
        assert_eq!(table.poll(now, 8), 1);
        assert!(late.check(Waker::noop()).is_ok());
        TABLE.with(|slot| slot.take());
    }

    /// Zero budgets, dropped entries, and exhausted identifiers preserve accounting.
    #[test]
    fn bounded_poll_drop_and_overflow() {
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let first = table.register(now).unwrap();
        let second = table.register(now).unwrap();
        assert_eq!(first.check(Waker::noop()), Ok(()));
        assert_eq!(table.poll(now, 0), 0);
        assert_eq!(table.poll(now, 1), 1);
        assert_eq!(
            first.check(Waker::noop()),
            Err(crate::Error::DeadlineExceeded)
        );
        assert_eq!(second.check(Waker::noop()), Ok(()));
        drop(second);
        assert!(table.is_empty());
        let table = Rc::new(Registry::with_next_id(u64::MAX));
        assert!(matches!(table.register(now), Err(crate::Error::Overloaded)));
        assert!(table.is_empty());
    }

    /// The next deadline follows removals and partially processed expiration batches.
    #[test]
    fn next_deadline_tracks_order_drop_and_budgeted_expiration() {
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let later = now + std::time::Duration::from_secs(1);
        assert_eq!(table.next_deadline(), None);
        let late = table.register(later).unwrap();
        assert_eq!(table.next_deadline(), Some(later));
        let first = table.register(now).unwrap();
        let second = table.register(now).unwrap();
        assert_eq!(table.next_deadline(), Some(now));
        assert_eq!(table.poll(now - std::time::Duration::from_nanos(1), 8), 0);
        assert_eq!(table.poll(now, 0), 0);
        assert_eq!(table.next_deadline(), Some(now));
        assert_eq!(table.poll(now, 1), 1);
        assert_eq!(table.next_deadline(), Some(now));
        let mut cx = Context::from_waker(Waker::noop());
        // Expiration does not require the handle to have been polled first.
        assert_eq!(first.poll_expired(&mut cx), Poll::Ready(()));
        assert_eq!(first.poll_expired(&mut cx), Poll::Ready(()));
        assert_eq!(second.poll_expired(&mut cx), Poll::Pending);
        drop(second);
        assert_eq!(table.next_deadline(), Some(later));
        drop(first);
        assert_eq!(table.next_deadline(), Some(later));
        assert_eq!(table.poll(later, 1), 1);
        assert_eq!(late.poll_expired(&mut cx), Poll::Ready(()));
        assert_eq!(table.next_deadline(), None);
        let removed = table.register(now).unwrap();
        drop(removed);
        assert_eq!(table.next_deadline(), None);
    }

    /// Only the latest waiter wakes, and removing its registration releases ownership.
    #[test]
    fn latest_waker_is_notified_once_and_drop_releases_it() {
        use crate::test_util::WakeCounter;
        use std::sync::Arc;
        let table = Rc::new(Registry::default());
        let now = Instant::now();
        let registration = table.register(now).unwrap();
        let old = Arc::new(WakeCounter::default());
        let latest = Arc::new(WakeCounter::default());
        for count in [&old, &latest] {
            assert!(
                registration
                    .poll_expired(&mut Context::from_waker(&Waker::from(count.clone())))
                    .is_pending()
            );
        }
        assert_eq!(table.poll(now, 1), 1);
        assert_eq!(old.count(), 0);
        assert_eq!(latest.count(), 1);
        assert_eq!(table.poll(now, 1), 0);
        assert_eq!(latest.count(), 1);
        let registration = table.register(now).unwrap();
        let weak = Arc::downgrade(&latest);
        registration.check(&Waker::from(latest)).unwrap();
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
    }

    /// Wake and drop callbacks can register nested deadlines without borrow conflicts.
    #[test]
    fn waking_and_removing_entries_allow_registry_reentry() {
        thread_local! {
            static TABLE: RefCell<Option<Rc<Registry>>> = const { RefCell::new(None) };
        }

        /// Mutates the selected registry from both wake and destructor callbacks.
        struct Reentrant;

        impl Reentrant {
            /// Add and remove a nested deadline while the outer table is empty.
            fn access() {
                TABLE.with(|slot| {
                    let table = slot.borrow().as_ref().unwrap().clone();
                    assert!(table.is_empty());
                    let nested = table.register(Instant::now()).unwrap();
                    assert_eq!(table.len(), 1);
                    drop(nested);
                });
            }
        }

        impl std::task::Wake for Reentrant {
            /// Exercise registry reentry from notification.
            fn wake(self: std::sync::Arc<Self>) {
                Self::access();
            }
        }

        impl Drop for Reentrant {
            /// Exercise registry reentry from disposal.
            fn drop(&mut self) {
                Self::access();
            }
        }
        let table = Rc::new(Registry::default());
        TABLE.with(|slot| slot.replace(Some(table.clone())));
        let now = Instant::now();
        let registration = table.register(now).unwrap();
        registration
            .check(&Waker::from(std::sync::Arc::new(Reentrant)))
            .unwrap();
        assert_eq!(table.poll(now, 1), 1);
        drop(registration);
        let registration = table.register(now).unwrap();
        registration
            .check(&Waker::from(std::sync::Arc::new(Reentrant)))
            .unwrap();
        drop(registration);
        assert!(table.is_empty());
        TABLE.with(|slot| slot.take());
    }
}

/// Wall-clock uncertainty detection without application invalidation policy.
#[cfg(test)]
mod clock_observer_tests {
    use super::*;

    /// Drift uses a strict threshold and backward samples saturate the epoch counter.
    #[test]
    fn strict_threshold_backwards_samples_and_epoch_saturation() {
        let mut observer = Observer::default();
        let mono = Instant::now();
        let wall = SystemTime::UNIX_EPOCH + Duration::from_secs(100);
        let second = Duration::from_secs(1);
        assert!(!observer.observe(wall, mono, second));
        assert!(!observer.observe(wall + second, mono, second));
        assert!(observer.observe(wall, mono, second));
        assert!(observer.observe(wall, mono - second, second));
        assert_eq!(observer.epoch(), 2);
        observer.epoch = u64::MAX;
        assert!(observer.observe(wall + second * 3, mono, second));
        assert_eq!(observer.epoch(), u64::MAX);
    }
}

/// Deterministic clock, entropy, and nested environment ownership regressions.
#[cfg(all(test, feature = "simulation"))]
mod simulation_tests {
    use super::*;

    /// Entering selects the world clock, role entropy and seed, and nests.
    #[test]
    fn selected_source_drives_ambient_reads_and_nests() {
        let clock = SimulationClock::new(7);
        assert!(!Environment::current().is_simulated());
        assert_eq!(simulation_seed(), None);
        let environment = clock.environment(0);
        {
            let _role = environment.enter();
            assert!(Environment::current().is_simulated());
            let before = now();
            clock.advance(Duration::from_secs(5));
            assert_eq!(now() - before, Duration::from_secs(5));
            assert_eq!(simulation_seed(), Some(7));
            {
                let _host = Environment::host().enter();
                assert_eq!(simulation_seed(), None);
            }
            assert_eq!(simulation_seed(), Some(7));
        }
        assert_eq!(simulation_seed(), None);
    }

    /// Environments are `Send` and moving one to another thread keeps sharing
    /// the entropy cursor with the clones left behind.
    #[test]
    fn environment_is_send_and_moved_clones_share_the_cursor() {
        fn assert_send<T: Send>() {}
        assert_send::<Environment>();
        let clock = SimulationClock::new(5);
        let mut expected = [0; 16];
        {
            let _role = clock.environment(3).enter();
            fill_random(&mut expected).unwrap();
        }
        let role = clock.environment(3);
        let moved = role.clone();
        let first = std::thread::spawn(move || {
            let _role = moved.enter();
            let mut bytes = [0; 8];
            fill_random(&mut bytes).unwrap();
            bytes
        })
        .join()
        .unwrap();
        let _role = role.enter();
        let mut second = [0; 8];
        fill_random(&mut second).unwrap();
        assert_eq!([first, second].concat(), expected);
    }

    /// Overflow leaves both clocks unchanged and does not poison later advances.
    #[test]
    fn checked_clock_overflow_is_atomic_and_unchecked_panic_does_not_poison() {
        let clock = SimulationClock::new(9);
        let _role = clock.environment(0).enter();
        let before = (now(), wall_now(), clock.elapsed());
        assert_eq!(
            clock.checked_advance(Duration::MAX),
            Err(Error::InvalidInput)
        );
        assert_eq!((now(), wall_now(), clock.elapsed()), before);
        assert!(std::panic::catch_unwind(|| clock.advance(Duration::MAX)).is_err());
        assert_eq!((now(), wall_now(), clock.elapsed()), before);
        clock.checked_advance(Duration::from_secs(1)).unwrap();
        assert_eq!(clock.elapsed(), Duration::from_secs(1));
    }

    /// Explicit entropy domains preserve replay independently of read chunk sizes.
    #[test]
    fn domains_and_multiblock_chunking_are_explicit_and_replayable() {
        let clock = SimulationClock::new(7);
        let mut default = [0; 99];
        {
            let _role = clock.environment(11).enter();
            fill_random(&mut default).unwrap();
        }
        let role = clock.environment_with_entropy_domain(11, &b"uring-runtime.entropy.v1\0"[..]);
        let mut replay = [0; 99];
        for chunk in replay.chunks_mut(7) {
            let _role = role.clone().enter();
            fill_random(chunk).unwrap();
        }
        assert_eq!(default, replay);
        let _role = clock
            .environment_with_entropy_domain(11, &b"test.entropy.v1\0"[..])
            .enter();
        fill_random(&mut replay).unwrap();
        assert_ne!(default, replay);
    }

    /// Empty reads do not advance the stream.
    #[test]
    fn empty_entropy_reads_do_not_advance_the_stream() {
        let clock = SimulationClock::new(1);
        let mut expected = [0; 4];
        {
            let _role = clock.environment(0).enter();
            fill_random(&mut expected).unwrap();
        }
        let _role = clock.environment(0).enter();
        fill_random(&mut []).unwrap();
        let mut bytes = [0; 4];
        fill_random(&mut bytes).unwrap();
        assert_eq!(bytes, expected);
    }

    /// Cloned roles consume one shared, domain-separated entropy stream with
    /// the old runtime's exact bytes.
    #[test]
    fn entropy_domain_and_cloned_cursor_remain_stable() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let clone = role.clone();
        let mut bytes = [0; 40];
        {
            let _role = role.enter();
            assert_eq!(simulation_seed(), Some(7));
            fill_random(&mut bytes[..3]).unwrap();
        }
        {
            let _clone = clone.enter();
            fill_random(&mut bytes[3..]).unwrap();
        }
        let hex: String = bytes.iter().map(|b| format!("{b:02x}")).collect();
        // SHA-256(b"uring-runtime.entropy.v1\0" || 7 || 11 || counter), as the
        // legacy runtime computed it.
        assert_eq!(
            hex,
            "5eb0f6598c0f2d9c3ea6f15f9019d7a1defa575d5d1fe54b8cb95f87e7bc5b82627c66322c311609"
        );
    }

    /// Nested strict guards restore the surrounding host-access requirement.
    #[test]
    fn nested_strict_guards_restore_previous_requirement() {
        let _host = Environment::default().enter();
        assert_eq!(simulation_seed(), None);
        {
            let _strict = require_simulated();
            {
                let _nested = require_simulated();
            }
            assert!(std::panic::catch_unwind(now).is_err());
            let clock = SimulationClock::new(19);
            let _role = clock.environment(0).enter();
            assert_eq!(simulation_seed(), Some(19));
            assert_eq!(now(), clock_anchor().0);
        }
        assert!(std::panic::catch_unwind(now).is_ok());
    }

    /// Every host time and entropy entry point rejects strict simulation fallback.
    #[test]
    fn strict_simulation_rejects_real_role_fallback() {
        let _strict = require_simulated();
        let _real = Environment::default().enter();
        let reads: [fn(); 4] = [
            || {
                let _ = now();
            },
            || {
                let _ = wall_now();
            },
            || {
                let _ = clock_anchor();
            },
            || {
                let _ = fill_random(&mut [0; 1]);
            },
        ];
        for read in reads {
            assert!(std::panic::catch_unwind(read).is_err());
        }
    }

    /// Pending and dropped scoped futures restore the caller's independent stream.
    #[test]
    fn streams_are_independent_and_scoped_futures_restore_on_pending_and_drop() {
        let clock = SimulationClock::new(42);
        let a = clock.environment(1);
        let b = clock.environment(2);
        let mut expected = [0; 8];
        {
            let _guard = clock.environment(1).enter();
            fill_random(&mut expected).unwrap();
        }
        let _outer = b.enter();
        let mut future = Box::pin(a.scope(async {
            let mut bytes = [0; 8];
            fill_random(&mut bytes).unwrap();
            assert_eq!(bytes, expected);
            std::future::pending::<()>().await;
        }));
        assert!(
            future
                .as_mut()
                .poll(&mut Context::from_waker(Waker::noop()))
                .is_pending()
        );
        drop(future);
        let mut actual = [0; 8];
        fill_random(&mut actual).unwrap();
        assert_ne!(actual, expected);
        let _guard = clock.environment(2).enter();
        fill_random(&mut expected).unwrap();
        assert_eq!(actual, expected);
    }

    /// Destructors observe their captured environment, even when scopes unwind.
    #[test]
    fn scoped_drop_and_unwind_restore_the_callers_environment() {
        /// Checks that destruction occurs under the expected wall clock.
        struct OnDrop(SystemTime);

        impl Drop for OnDrop {
            /// Compare the selected wall clock with the captured expectation.
            fn drop(&mut self) {
                assert_eq!(wall_now(), self.0);
            }
        }
        let outer = SimulationClock::new(1);
        let inner = SimulationClock::new(2);
        inner.advance(Duration::from_secs(5));
        let _outer = outer.environment(0).enter();
        let wall = wall_now();
        let role = inner.environment(0);
        let dropped = OnDrop(wall + Duration::from_secs(5));
        let future = role.scope(async move {
            let _dropped = dropped;
            std::future::pending::<()>().await;
        });
        drop(future);
        assert_eq!(wall_now(), wall);
        let result = std::panic::catch_unwind(|| {
            let _inner = role.enter();
            panic!("injected worker panic");
        });
        assert!(result.is_err());
        assert_eq!(wall_now(), wall);
    }

    /// Wall corrections never renew an existing monotonic deadline.
    #[test]
    fn deadline_mapping_is_stable_across_simulated_time_and_wall_jumps() {
        let clock = SimulationClock::new(17);
        let _environment = clock.environment(0).enter();
        let deadline = Deadline(now() + Duration::from_secs(30));
        let encoded = deadline.to_unix_millis().unwrap();
        clock.advance(Duration::from_secs(60));
        clock.set_wall_time(wall_now() + Duration::from_secs(100));
        assert_eq!(deadline.to_unix_millis(), Ok(encoded));
        assert_eq!(
            Deadline::from_unix_millis(encoded)
                .unwrap()
                .to_unix_millis(),
            Ok(encoded)
        );
    }

    /// Scoped futures see their environment only while polled or dropped.
    #[test]
    fn scoped_future_enters_only_while_polled() {
        let clock = SimulationClock::new(3);
        let mut future = std::pin::pin!(
            clock
                .environment(0)
                .scope(std::future::poll_fn(|_| Poll::Ready(simulation_seed())))
        );
        let mut cx = Context::from_waker(Waker::noop());
        assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Some(3)));
        assert_eq!(simulation_seed(), None);
    }
}
