//! Worker time, entropy, deadlines, and cancellation observations.
//!
//! Production uses the host clock and getrandom. Deterministic environments
//! require the simulation feature. Deadline registries are explicitly driven by
//! their owner; neither registering a deadline nor reading a clock starts a timer.
use crate::{Error, Result};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    future::Future,
    marker::PhantomData,
    pin::Pin,
    rc::Rc,
    sync::{
        Arc, Mutex, OnceLock, Weak,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// Time and entropy selection retained by a worker role across individual polls.
#[derive(Clone, Debug, Default)]
pub struct Environment {
    #[cfg(feature = "simulation")]
    simulated: Option<Simulated>,
}

/// An absolute monotonic deadline, mapped to wire time through a stable anchor.
#[derive(Clone, Copy, Debug)]
pub struct Deadline(pub Instant);

/// Shared cancellation state with a caller-bounded set of operation wakeups.
#[derive(Clone)]
pub struct Cancellation {
    state: Arc<CancellationState>,
}

/// Operation-owned cancellation wake registration. Drop removes its live entry;
/// independent operations may safely register the same executor waker.
pub struct CancellationRegistration {
    cancellation: Cancellation,
    wake: Arc<futures::task::AtomicWaker>,
}

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
    static REQUIRE_SIMULATED: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
    static CURRENT: std::cell::RefCell<Environment> = const {
        std::cell::RefCell::new(Environment { simulated: None })
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
    fn drop(&mut self) {
        REQUIRE_SIMULATED.with(|required| required.set(self.0));
    }
}
#[cfg(feature = "simulation")]
/// Reject host time or entropy when a strict simulation scope is active.
fn check_host_access() {
    REQUIRE_SIMULATED
        .with(|required| assert!(!required.get(), "DST escaped into host time/entropy"));
}

/// A guard belongs to the thread that entered it. Never hold it across an await;
/// use `Environment::scope` to enter only while polling or dropping a future.
pub struct Guard {
    #[cfg(feature = "simulation")]
    previous: Environment,
    _local: PhantomData<Rc<()>>,
}

impl Environment {
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
    type Output = F::Output;
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
    fn drop(&mut self) {
        let _environment = self.environment.enter();
        self.future.take();
    }
}

#[cfg(feature = "simulation")]
/// Return the active simulated world's seed, or None for a host environment.
pub fn simulation_seed() -> Option<u64> {
    Environment::current()
        .simulated
        .map(|sim| sim.clock.0.lock().unwrap().seed)
}

/// Read monotonic time from the selected environment.
pub fn now() -> Instant {
    #[cfg(feature = "simulation")]
    if let Some(sim) = Environment::current().simulated {
        let clock = sim.clock.0.lock().unwrap();
        return clock.monotonic + clock.elapsed;
    }
    #[cfg(feature = "simulation")]
    check_host_access();
    Instant::now()
}

/// Read wall time, including simulated wall-clock corrections.
pub fn wall_now() -> SystemTime {
    #[cfg(feature = "simulation")]
    if let Some(sim) = Environment::current().simulated {
        let clock = sim.clock.0.lock().unwrap();
        return clock.wall;
    }
    #[cfg(feature = "simulation")]
    check_host_access();
    SystemTime::now()
}

/// Stable mapping anchor, unaffected by later wall-clock corrections. All roles
/// of a simulated world use the same anchor, including after nested world scopes.
pub fn clock_anchor() -> (Instant, SystemTime) {
    #[cfg(feature = "simulation")]
    if let Some(sim) = Environment::current().simulated {
        let clock = sim.clock.0.lock().unwrap();
        return (clock.monotonic, clock.wall_origin);
    }
    #[cfg(feature = "simulation")]
    check_host_access();
    static ANCHOR: OnceLock<(Instant, SystemTime)> = OnceLock::new();
    *ANCHOR.get_or_init(|| (Instant::now(), SystemTime::now()))
}

/// Fill bytes from the host entropy source or the selected replayable role stream.
pub fn fill_random(bytes: &mut [u8]) -> Result<(), getrandom::Error> {
    #[cfg(feature = "simulation")]
    if let Some(sim) = Environment::current().simulated {
        return sim.entropy.lock().unwrap().fill(bytes);
    }
    #[cfg(feature = "simulation")]
    check_host_access();
    getrandom::getrandom(bytes)
}

#[cfg(feature = "simulation")]
#[derive(Clone, Debug)]
/// A world's clock and a role's shared entropy cursor.
struct Simulated {
    clock: SimulationClock,
    entropy: std::sync::Arc<std::sync::Mutex<Entropy>>,
}

#[cfg(feature = "simulation")]
#[derive(Debug)]
/// A domain-separated, counter-based entropy stream with a partial-block cursor.
struct Entropy {
    domain: std::sync::Arc<[u8]>,
    seed: u64,
    stream: u64,
    counter: u64,
    block: [u8; 32],
    offset: usize,
}

#[cfg(feature = "simulation")]
impl Entropy {
    /// Copy whole available chunks while retaining the cursor across role clones.
    fn fill(&mut self, mut bytes: &mut [u8]) -> Result<(), getrandom::Error> {
        use sha2::{Digest, Sha256};

        while !bytes.is_empty() {
            if self.offset == self.block.len() {
                let mut hash = Sha256::new();
                hash.update(&self.domain);
                hash.update(self.seed.to_le_bytes());
                hash.update(self.stream.to_le_bytes());
                hash.update(self.counter.to_le_bytes());
                self.block = hash.finalize().into();
                self.counter = self
                    .counter
                    .checked_add(1)
                    .ok_or(getrandom::Error::UNEXPECTED)?;
                self.offset = 0;
            }
            let count = bytes.len().min(self.block.len() - self.offset);
            let (chunk, remaining) = bytes.split_at_mut(count);
            chunk.copy_from_slice(&self.block[self.offset..self.offset + count]);
            self.offset += count;
            bytes = remaining;
        }
        Ok(())
    }
}

/// One clock per simulated world. Give each node/worker/role a stable, unique
/// stream ID and retain its Environment; cloning a role shares its entropy cursor.
#[cfg(feature = "simulation")]
#[derive(Clone, Debug)]
pub struct SimulationClock(std::sync::Arc<std::sync::Mutex<Clock>>);

#[cfg(feature = "simulation")]
#[derive(Debug)]
/// Clock anchors and mutable elapsed time shared by one simulated world.
struct Clock {
    seed: u64,
    monotonic: Instant,
    elapsed: std::time::Duration,
    wall_origin: SystemTime,
    wall: SystemTime,
}

#[cfg(feature = "simulation")]
impl SimulationClock {
    /// Create a world with the process monotonic origin and a fixed wall epoch.
    pub fn new(seed: u64) -> Self {
        // std::Instant has no epoch constructor. Sample once outside the exercised
        // graph; only relative virtual durations and fixed wall time are observable.
        static ORIGIN: OnceLock<Instant> = OnceLock::new();
        Self::new_at(
            seed,
            *ORIGIN.get_or_init(Instant::now),
            SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(1_800_000_000),
        )
    }

    /// Create a world with explicit monotonic and wall-clock anchors.
    pub fn new_at(seed: u64, monotonic: Instant, wall: SystemTime) -> Self {
        Self(std::sync::Arc::new(std::sync::Mutex::new(Clock {
            seed,
            monotonic,
            elapsed: std::time::Duration::ZERO,
            wall_origin: wall,
            wall,
        })))
    }

    /// Creates a fresh entropy stream. Call once per role incarnation and retain
    /// the result; calling again with the same ID intentionally replays its bytes.
    /// The default domain is `b"uring-runtime.entropy.v1\0"`. Applications with
    /// persisted replay corpora should own an explicit, versioned domain instead.
    pub fn environment(&self, stream: u64) -> Environment {
        self.environment_with_entropy_domain(stream, &b"uring-runtime.entropy.v1\0"[..])
    }

    /// Creates a stream in a caller-selected domain. Bytes are SHA-256 blocks of
    /// domain || seed_le64 || stream_le64 || counter_le64, starting at counter 0.
    /// Chunking reads and cloning environments do not change the byte stream.
    pub fn environment_with_entropy_domain(
        &self,
        stream: u64,
        domain: impl Into<std::sync::Arc<[u8]>>,
    ) -> Environment {
        Environment {
            simulated: Some(Simulated {
                clock: self.clone(),
                entropy: std::sync::Arc::new(std::sync::Mutex::new(Entropy {
                    domain: domain.into(),
                    seed: self.0.lock().unwrap().seed,
                    stream,
                    counter: 0,
                    block: [0; 32],
                    offset: 32,
                })),
            }),
        }
    }

    /// Advance both clocks, panicking on overflow without poisoning the clock.
    pub fn advance(&self, duration: std::time::Duration) {
        // Panic only after checked_advance releases the lock, so compatibility
        // callers can catch overflow without poisoning their simulated world.
        self.checked_advance(duration).expect("clock overflow");
    }

    /// Advance atomically or leave both clocks unchanged on overflow.
    pub fn checked_advance(&self, duration: std::time::Duration) -> crate::Result<()> {
        let mut clock = self.0.lock().map_err(|_| crate::Error::Unavailable)?;
        let elapsed = clock
            .elapsed
            .checked_add(duration)
            .ok_or(crate::Error::InvalidInput)?;
        clock
            .monotonic
            .checked_add(elapsed)
            .ok_or(crate::Error::InvalidInput)?;
        let wall = clock
            .wall
            .checked_add(duration)
            .ok_or(crate::Error::InvalidInput)?;
        clock.elapsed = elapsed;
        clock.wall = wall;
        Ok(())
    }

    /// Return the total monotonic time advanced since world creation.
    pub fn elapsed(&self) -> std::time::Duration {
        self.0.lock().unwrap().elapsed
    }

    /// Correct wall time without changing elapsed time or the stable anchor.
    pub fn set_wall_time(&self, wall: SystemTime) {
        self.0.lock().unwrap().wall = wall;
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

impl Cancellation {
    /// Construct with a 1024-live-registration limit. Construction cannot fail;
    /// the Result contract is retained for existing callers.
    pub fn new() -> Result<Self> {
        Ok(Self::with_registration_limit(1024))
    }

    /// Choose a subscription bound; zero still permits canceling and querying.
    fn with_registration_limit(registration_limit: usize) -> Self {
        Self {
            state: Arc::new(CancellationState {
                canceled: AtomicBool::new(false),
                registration_limit,
                registrations: Mutex::new(Vec::new()),
            }),
        }
    }

    /// Whether any owner has requested cancellation.
    pub fn is_cancelled(&self) -> bool {
        self.state.canceled.load(Ordering::Acquire)
    }

    /// Subscribe for one operation's lifetime, even when operations share a waker.
    /// Dropping the subscription reclaims its capacity and wake.
    pub fn subscribe(&self) -> Result<CancellationRegistration> {
        let wake = Arc::new(futures::task::AtomicWaker::new());
        let mut entries = self
            .state
            .registrations
            .lock()
            .map_err(|_| Error::Unavailable)?;
        entries.retain(|entry| entry.strong_count() != 0);
        if entries.len() >= self.state.registration_limit {
            return Err(Error::Overloaded);
        }
        entries.push(Arc::downgrade(&wake));
        Ok(CancellationRegistration {
            cancellation: self.clone(),
            wake,
        })
    }

    /// Mark cancellation and wake live subscribers after releasing the state lock.
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

impl CancellationRegistration {
    /// Replace the waiting task and close the race with concurrent cancellation.
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

/// Cancellation state shared by handles and their operation registrations.
struct CancellationState {
    canceled: AtomicBool,
    registration_limit: usize,
    registrations: Mutex<Vec<Weak<futures::task::AtomicWaker>>>,
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

#[cfg(all(test, feature = "simulation"))]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn checked_clock_overflow_is_atomic_and_legacy_panic_does_not_poison() {
        let clock = SimulationClock::new(9);
        let _role = clock.environment(0).enter();
        let before = (now(), wall_now(), clock.elapsed());
        assert_eq!(
            clock.checked_advance(Duration::MAX),
            Err(crate::Error::InvalidInput)
        );
        assert_eq!((now(), wall_now(), clock.elapsed()), before);
        assert!(std::panic::catch_unwind(|| clock.advance(Duration::MAX)).is_err());
        assert_eq!((now(), wall_now(), clock.elapsed()), before);
        clock.checked_advance(Duration::from_secs(1)).unwrap();
        assert_eq!(clock.elapsed(), Duration::from_secs(1));
    }

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

    #[test]
    fn exhausted_entropy_returns_error_without_poisoning_cursor() {
        let clock = SimulationClock::new(1);
        let role = clock.environment(0);
        let entropy = &role.simulated.as_ref().unwrap().entropy;
        entropy.lock().unwrap().counter = u64::MAX;
        let _role = role.enter();
        for _ in 0..2 {
            assert!(fill_random(&mut [0; 1]).is_err());
            assert_eq!(entropy.lock().unwrap().offset, 32);
        }
    }

    #[test]
    fn entropy_empty_reads_and_exhausted_partial_blocks_preserve_the_cursor() {
        let clock = SimulationClock::new(1);
        let role = clock.environment(0);
        let _role = role.enter();
        let entropy = &role.simulated.as_ref().unwrap().entropy;
        {
            let mut cursor = entropy.lock().unwrap();
            cursor.counter = u64::MAX;
            cursor.offset = 30;
            cursor.block = [7; 32];
        }
        fill_random(&mut []).unwrap();
        assert_eq!(entropy.lock().unwrap().offset, 30);
        let mut bytes = [9; 4];
        assert!(fill_random(&mut bytes).is_err());
        assert_eq!(bytes, [7, 7, 9, 9]);
        assert_eq!(entropy.lock().unwrap().offset, 32);
        fill_random(&mut []).unwrap();
        assert_eq!(entropy.lock().unwrap().counter, u64::MAX);
    }

    #[test]
    fn entropy_domain_and_cloned_cursor_remain_stable() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let clone = role.clone();
        let mut bytes = [0; 32];
        {
            let _role = role.enter();
            assert_eq!(simulation_seed(), Some(7));
            fill_random(&mut bytes[..3]).unwrap();
        }
        {
            let _clone = clone.enter();
            fill_random(&mut bytes[3..]).unwrap();
        }
        assert_eq!(bytes, {
            use sha2::{Digest, Sha256};
            let mut hash = Sha256::new();
            hash.update(b"uring-runtime.entropy.v1\0");
            hash.update(7u64.to_le_bytes());
            hash.update(11u64.to_le_bytes());
            hash.update(0u64.to_le_bytes());
            <[u8; 32]>::from(hash.finalize())
        });
    }

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
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
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

    #[test]
    fn scoped_drop_and_unwind_restore_the_callers_environment() {
        struct OnDrop(SystemTime);
        impl Drop for OnDrop {
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
}

#[cfg(test)]
mod host_tests {
    use super::*;

    #[test]
    fn host_environment_uses_a_stable_anchor() {
        let _host = Environment::default().enter();
        let first = clock_anchor();
        assert_eq!(clock_anchor(), first);
        assert!(now() >= first.0);
        fill_random(&mut [0; 32]).unwrap();
    }
}

#[cfg(test)]
mod deadline_tests {
    use super::*;
    use crate::test_util::WakeCounter;

    #[test]
    fn caller_selected_registration_bound_and_concurrent_cancel() {
        let disabled = Cancellation::with_registration_limit(0);
        assert!(matches!(disabled.subscribe(), Err(Error::Overloaded)));
        disabled.cancel().unwrap();
        assert!(disabled.is_cancelled());
        for _ in 0..64 {
            let cancellation = Cancellation::with_registration_limit(1);
            let registration = cancellation.subscribe().unwrap();
            assert!(matches!(cancellation.subscribe(), Err(Error::Overloaded)));
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            std::thread::scope(|threads| {
                threads.spawn(|| cancellation.cancel().unwrap());
                threads.spawn(|| registration.register(&waker));
            });
            assert!(count.count() >= 1);
            drop(registration);
            assert!(cancellation.subscribe().is_ok());
        }
    }

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

    #[cfg(feature = "simulation")]
    #[test]
    fn deadline_mapping_is_stable_across_simulated_time_and_wall_jumps() {
        let clock = crate::environment::SimulationClock::new(17);
        let _environment = clock.environment(0).enter();
        let deadline = Deadline(crate::environment::now() + Duration::from_secs(30));
        let encoded = deadline.to_unix_millis().unwrap();
        clock.advance(Duration::from_secs(60));
        clock.set_wall_time(crate::environment::wall_now() + Duration::from_secs(100));
        assert_eq!(deadline.to_unix_millis(), Ok(encoded));
        assert_eq!(
            Deadline::from_unix_millis(encoded)
                .unwrap()
                .to_unix_millis(),
            Ok(encoded)
        );
    }

    #[test]
    fn operation_registrations_reclaim_capacity_and_do_not_remove_shared_wakes() {
        let cancellation = Cancellation::new().unwrap();
        let count = Arc::new(WakeCounter::default());
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
        assert_eq!(count.count(), 1);
    }

    #[test]
    fn dropping_subscription_releases_executor_resources_before_scope_ends() {
        let cancellation = Cancellation::new().unwrap();
        let executor = Arc::new(WakeCounter::default());
        let weak = Arc::downgrade(&executor);
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor));
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
        assert!(!cancellation.is_cancelled());

        // A worker that subscribes after cancellation must still be notified.
        cancellation.cancel().unwrap();
        let executor = Arc::new(WakeCounter::default());
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor.clone()));
        assert_eq!(executor.count(), 1);
    }

    #[test]
    fn live_registration_limit_is_reclaimed_after_drop() {
        let cancellation = Cancellation::new().unwrap();
        let mut registrations: Vec<_> = (0..1024)
            .map(|_| cancellation.subscribe().unwrap())
            .collect();
        assert!(matches!(cancellation.subscribe(), Err(Error::Overloaded)));
        registrations.pop();
        assert!(cancellation.subscribe().is_ok());
    }
}

#[cfg(test)]
mod registry_tests {
    use super::*;

    #[test]
    fn deadlines_are_ordered_by_time_then_registration_and_drop_is_reentrant() {
        thread_local! {
            static TABLE: RefCell<Option<Rc<Registry>>> = const { RefCell::new(None) };
        }
        struct OnDrop;
        impl std::task::Wake for OnDrop {
            fn wake(self: std::sync::Arc<Self>) {
                panic!("replaced registration must not wake its retired owner");
            }
        }
        impl Drop for OnDrop {
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

    #[test]
    fn waking_and_removing_entries_allow_registry_reentry() {
        thread_local! {
            static TABLE: RefCell<Option<Rc<Registry>>> = const { RefCell::new(None) };
        }
        struct Reentrant;
        impl Reentrant {
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
            fn wake(self: std::sync::Arc<Self>) {
                Self::access();
            }
        }
        impl Drop for Reentrant {
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

#[cfg(test)]
mod clock_observer_tests {
    use super::*;

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
