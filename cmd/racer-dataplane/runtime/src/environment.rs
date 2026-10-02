//! Scoped time and entropy for worker roles. Production always uses the host clock
//! and getrandom; deterministic environments require the simulation feature.
use std::{
    future::Future,
    marker::PhantomData,
    pin::Pin,
    rc::Rc,
    sync::OnceLock,
    task::{Context, Poll},
    time::{Instant, SystemTime},
};

#[derive(Clone, Debug, Default)]
pub struct Environment {
    #[cfg(feature = "simulation")]
    simulated: Option<Simulated>,
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
pub struct SimulationRequired(bool, PhantomData<Rc<()>>);
#[cfg(feature = "simulation")]
impl Drop for SimulationRequired {
    fn drop(&mut self) {
        REQUIRE_SIMULATED.with(|required| required.set(self.0));
    }
}
#[cfg(feature = "simulation")]
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
    pub fn current() -> Self {
        #[cfg(feature = "simulation")]
        return CURRENT.with(|current| current.borrow().clone());
        #[cfg(not(feature = "simulation"))]
        Self::default()
    }

    pub fn enter(&self) -> Guard {
        Guard {
            #[cfg(feature = "simulation")]
            previous: CURRENT.with(|current| current.replace(self.clone())),
            _local: PhantomData,
        }
    }

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
pub fn simulation_seed() -> Option<u64> {
    Environment::current()
        .simulated
        .map(|sim| sim.clock.0.lock().unwrap().seed)
}

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

pub fn fill_random(bytes: &mut [u8]) -> Result<(), getrandom::Error> {
    #[cfg(feature = "simulation")]
    if let Some(sim) = Environment::current().simulated {
        use sha2::{Digest, Sha256};
        let mut entropy = sim.entropy.lock().unwrap();
        for byte in bytes {
            if entropy.offset == 32 {
                let mut hash = Sha256::new();
                hash.update(b"racer.dst.entropy.v1\0");
                hash.update(entropy.seed.to_le_bytes());
                hash.update(entropy.stream.to_le_bytes());
                hash.update(entropy.counter.to_le_bytes());
                entropy.block = hash.finalize().into();
                entropy.counter = entropy.counter.checked_add(1).expect("entropy exhausted");
                entropy.offset = 0;
            }
            *byte = entropy.block[entropy.offset];
            entropy.offset += 1;
        }
        return Ok(());
    }
    #[cfg(feature = "simulation")]
    check_host_access();
    getrandom::getrandom(bytes)
}

#[cfg(feature = "simulation")]
#[derive(Clone, Debug)]
struct Simulated {
    clock: SimulationClock,
    entropy: std::sync::Arc<std::sync::Mutex<Entropy>>,
}

#[cfg(feature = "simulation")]
#[derive(Debug)]
struct Entropy {
    seed: u64,
    stream: u64,
    counter: u64,
    block: [u8; 32],
    offset: usize,
}

/// One clock per simulated world. Give each node/worker/role a stable, unique
/// stream ID and retain its Environment; cloning a role shares its entropy cursor.
#[cfg(feature = "simulation")]
#[derive(Clone, Debug)]
pub struct SimulationClock(std::sync::Arc<std::sync::Mutex<Clock>>);

#[cfg(feature = "simulation")]
#[derive(Debug)]
struct Clock {
    seed: u64,
    monotonic: Instant,
    elapsed: std::time::Duration,
    wall_origin: SystemTime,
    wall: SystemTime,
}

#[cfg(feature = "simulation")]
impl SimulationClock {
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
    pub fn environment(&self, stream: u64) -> Environment {
        Environment {
            simulated: Some(Simulated {
                clock: self.clone(),
                entropy: std::sync::Arc::new(std::sync::Mutex::new(Entropy {
                    seed: self.0.lock().unwrap().seed,
                    stream,
                    counter: 0,
                    block: [0; 32],
                    offset: 32,
                })),
            }),
        }
    }

    pub fn advance(&self, duration: std::time::Duration) {
        let mut clock = self.0.lock().unwrap();
        let elapsed = clock.elapsed.checked_add(duration).expect("clock overflow");
        clock
            .monotonic
            .checked_add(elapsed)
            .expect("monotonic overflow");
        let wall = clock
            .wall
            .checked_add(duration)
            .expect("wall clock overflow");
        clock.elapsed = elapsed;
        clock.wall = wall;
    }

    pub fn elapsed(&self) -> std::time::Duration {
        self.0.lock().unwrap().elapsed
    }

    pub fn set_wall_time(&self, wall: SystemTime) {
        self.0.lock().unwrap().wall = wall;
    }
}

#[cfg(all(test, feature = "simulation"))]
mod tests {
    use super::*;
    use std::time::Duration;

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
        assert_eq!(
            bytes,
            [
                3, 158, 83, 113, 252, 214, 143, 64, 34, 247, 154, 192, 42, 95, 161, 48, 152, 147,
                76, 64, 247, 88, 244, 158, 105, 39, 35, 80, 39, 109, 231, 156,
            ]
        );
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
