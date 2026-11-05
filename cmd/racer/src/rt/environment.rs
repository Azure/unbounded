//! Worker time, entropy, deadlines, and cancellation observations.
//!
//! Production uses the host clock and getrandom. Deterministic environments
//! require the simulation feature. Deadline registries are explicitly driven by
//! their owner; neither registering a deadline nor reading a clock starts a timer.
use crate::rt::Result;
use std::{
    future::Future,
    marker::PhantomData,
    pin::Pin,
    rc::Rc,
    sync::OnceLock,
    task::{Context, Poll},
    time::{Instant, SystemTime},
};

/// Time and entropy selection retained by a worker role across individual polls.
#[derive(Clone, Debug, Default)]
pub struct Environment {
    #[cfg(test)]
    simulated: Option<Simulated>,
}

pub use racer_io::cancel::{Cancellation, CancellationRegistration};

#[cfg(test)]
thread_local! {
    static REQUIRE_SIMULATED: std::cell::Cell<bool> = const { std::cell::Cell::new(false) };
    static CURRENT: std::cell::RefCell<Environment> = const {
        std::cell::RefCell::new(Environment { simulated: None })
    };
}

/// Fail closed if a simulated role accidentally selects real clock/entropy.
#[cfg(test)]
pub fn require_simulated() -> SimulationRequired {
    SimulationRequired(
        REQUIRE_SIMULATED.with(|required| required.replace(true)),
        PhantomData,
    )
}

#[cfg(test)]
/// Restores the thread's previous host-access requirement when dropped.
pub struct SimulationRequired(bool, PhantomData<Rc<()>>);

#[cfg(test)]
impl Drop for SimulationRequired {
    /// Restore the strictness selected before this thread entered the guard.
    fn drop(&mut self) {
        REQUIRE_SIMULATED.with(|required| required.set(self.0));
    }
}

/// A guard belongs to the thread that entered it. Never hold it across an await;
/// use `Environment::scope` to enter only while polling or dropping a future.
pub struct Guard {
    #[cfg(test)]
    previous: Environment,

    _local: PhantomData<Rc<()>>,
}

impl Environment {
    /// The explicit racer-io worker context equivalent to this environment.
    // MIGRATION: bridges thread-local environments to WorkerCtx until every
    // role receives its context explicitly.
    pub fn worker_ctx(&self) -> racer_io::WorkerCtx {
        #[cfg(test)]
        if let Some(sim) = &self.simulated {
            return sim.ctx.clone();
        }
        racer_io::WorkerCtx::host()
    }

    /// Capture this thread's selected environment, or the host environment.
    pub fn current() -> Self {
        #[cfg(test)]
        return CURRENT.with(|current| current.borrow().clone());
        #[cfg(not(test))]
        Self::default()
    }

    /// Select this environment until the returned thread-local guard is dropped.
    pub fn enter(&self) -> Guard {
        Guard {
            #[cfg(test)]
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
        #[cfg(test)]
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

#[cfg(test)]
/// Return the active simulated world's seed, or None for a host environment.
pub fn simulation_seed() -> Option<u64> {
    Environment::current().simulated.map(|sim| sim.clock.seed)
}

/// Read monotonic time from the selected environment.
pub fn now() -> Instant {
    #[cfg(test)]
    if let Some(sim) = selected_simulation() {
        return sim.clock.clock.now();
    }
    Instant::now()
}

/// Read wall time, including simulated wall-clock corrections.
pub fn wall_now() -> SystemTime {
    #[cfg(test)]
    if let Some(sim) = selected_simulation() {
        return sim.clock.clock.wall_now();
    }
    SystemTime::now()
}

/// Stable mapping anchor, unaffected by later wall-clock corrections. All roles
/// of a simulated world use the same anchor, including after nested world scopes.
pub fn clock_anchor() -> (Instant, SystemTime) {
    #[cfg(test)]
    if let Some(sim) = selected_simulation() {
        return racer_io::Clock::from(sim.clock.clock.clone()).anchor();
    }

    static ANCHOR: OnceLock<(Instant, SystemTime)> = OnceLock::new();
    *ANCHOR.get_or_init(|| (Instant::now(), SystemTime::now()))
}

/// Fill bytes from the host entropy source or the selected replayable role stream.
pub fn fill_random(bytes: &mut [u8]) -> Result<(), getrandom::Error> {
    #[cfg(test)]
    if let Some(sim) = selected_simulation() {
        return sim.entropy.lock().unwrap().fill(bytes);
    }
    getrandom::getrandom(bytes)
}

#[cfg(test)]
#[derive(Clone, Debug)]
/// A world's clock and a role's shared entropy cursor.
struct Simulated {
    clock: SimulationClock,

    entropy: std::sync::Arc<std::sync::Mutex<Entropy>>,

    /// The same world clock with a racer-io entropy stream for migrated callers.
    ctx: racer_io::WorkerCtx,
}

#[cfg(test)]
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

#[cfg(test)]
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
#[cfg(test)]
#[derive(Clone, Debug)]
pub struct SimulationClock {
    seed: u64,

    // MIGRATION: the world clock is racer-io's so migrated roles that hold a
    // WorkerCtx observe the same virtual time as thread-local environment users.
    clock: racer_io::ctx::VirtualClock,
}

#[cfg(test)]
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
        Self {
            seed,
            clock: racer_io::ctx::VirtualClock::new_at(monotonic, wall),
        }
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
                    seed: self.seed,
                    stream,
                    counter: 0,
                    block: [0; 32],
                    offset: 32,
                })),
                ctx: racer_io::WorkerCtx::simulated(&self.clock, self.seed, stream),
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
    pub fn checked_advance(&self, duration: std::time::Duration) -> crate::rt::Result<()> {
        // Keep the old contract: the monotonic instant itself must not overflow.
        self.clock
            .now()
            .checked_add(duration)
            .ok_or(crate::rt::Error::InvalidInput)?;
        self.clock.advance(duration)
    }

    /// Return the total monotonic time advanced since world creation.
    pub fn elapsed(&self) -> std::time::Duration {
        self.clock.elapsed()
    }

    /// Correct wall time without changing elapsed time or the stable anchor.
    pub fn set_wall_time(&self, wall: SystemTime) {
        self.clock
            .set_wall_time(wall)
            .expect("simulated wall time is after the Unix epoch");
    }

    /// The racer-io virtual clock backing this world.
    // MIGRATION: lets migrated roles build a WorkerCtx on the same world clock.
    pub fn virtual_clock(&self) -> &racer_io::ctx::VirtualClock {
        &self.clock
    }
}

/// Select the simulated role, rejecting host fallback inside a strict scope.
#[cfg(test)]
fn selected_simulation() -> Option<Simulated> {
    let simulated = Environment::current().simulated;
    if simulated.is_none() {
        REQUIRE_SIMULATED
            .with(|required| assert!(!required.get(), "DST escaped into host time/entropy"));
    }
    simulated
}

/// Deterministic clock, entropy, and nested environment ownership regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    /// Overflow leaves both clocks unchanged and does not poison later advances.
    #[test]
    fn checked_clock_overflow_is_atomic_and_unchecked_panic_does_not_poison() {
        let clock = SimulationClock::new(9);
        let _role = clock.environment(0).enter();
        let before = (now(), wall_now(), clock.elapsed());
        assert_eq!(
            clock.checked_advance(Duration::MAX),
            Err(crate::rt::Error::InvalidInput)
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

    /// Counter exhaustion reports failure while keeping the cursor usable for checks.
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

    /// Empty reads do not advance entropy, and partial exhaustion retains copied bytes.
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

    /// Cloned roles consume one shared, domain-separated entropy stream.
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
}

/// Host environment behavior independent of simulation support.
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
