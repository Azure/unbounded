//! Scoped time and entropy for worker roles. Production always uses the host clock
//! and getrandom; deterministic environments can only be constructed in tests.
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
    #[cfg(test)]
    simulated: Option<Simulated>,
}

#[cfg(test)]
thread_local! {
    static CURRENT: std::cell::RefCell<Environment> = const {
        std::cell::RefCell::new(Environment { simulated: None })
    };
}

/// A guard belongs to the thread that entered it. Never hold it across an await;
/// use `Environment::scope` to enter only while polling or dropping a future.
pub struct Guard {
    #[cfg(test)]
    previous: Environment,
    _local: PhantomData<Rc<()>>,
}

impl Environment {
    pub fn current() -> Self {
        #[cfg(test)]
        return CURRENT.with(|current| current.borrow().clone());
        #[cfg(not(test))]
        Self::default()
    }

    pub fn enter(&self) -> Guard {
        Guard {
            #[cfg(test)]
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
        #[cfg(test)]
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

pub fn now() -> Instant {
    #[cfg(test)]
    if let Some(sim) = Environment::current().simulated {
        let clock = sim.clock.0.lock().unwrap();
        return clock.monotonic + clock.elapsed;
    }
    Instant::now()
}

pub fn wall_now() -> SystemTime {
    #[cfg(test)]
    if let Some(sim) = Environment::current().simulated {
        let clock = sim.clock.0.lock().unwrap();
        return clock.wall;
    }
    SystemTime::now()
}

/// Stable mapping anchor, unaffected by later wall-clock corrections. All roles
/// of a simulated world use the same anchor, including after nested world scopes.
pub fn clock_anchor() -> (Instant, SystemTime) {
    #[cfg(test)]
    if let Some(sim) = Environment::current().simulated {
        let clock = sim.clock.0.lock().unwrap();
        return (clock.monotonic, clock.wall_origin);
    }
    static ANCHOR: OnceLock<(Instant, SystemTime)> = OnceLock::new();
    *ANCHOR.get_or_init(|| (Instant::now(), SystemTime::now()))
}

pub fn fill_random(bytes: &mut [u8]) -> Result<(), getrandom::Error> {
    #[cfg(test)]
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
    getrandom::getrandom(bytes)
}

pub fn unix_time() -> rustls::pki_types::UnixTime {
    rustls::pki_types::UnixTime::since_unix_epoch(
        wall_now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

#[cfg(test)]
#[derive(Clone, Debug)]
struct Simulated {
    clock: SimulationClock,
    entropy: std::sync::Arc<std::sync::Mutex<Entropy>>,
}

#[cfg(test)]
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
#[cfg(test)]
#[derive(Clone, Debug)]
pub struct SimulationClock(std::sync::Arc<std::sync::Mutex<Clock>>);

#[cfg(test)]
#[derive(Debug)]
struct Clock {
    seed: u64,
    monotonic: Instant,
    elapsed: std::time::Duration,
    wall_origin: SystemTime,
    wall: SystemTime,
}

#[cfg(test)]
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{error::Error, model::identity::RequestId, runtime::deadline::RequestScope};
    use std::time::Duration;

    #[test]
    fn replay_and_nested_worlds_preserve_time_entropy_and_deadlines() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let _guard = role.enter();
        let start = now();
        let wall = wall_now();
        let scope = RequestScope::new(RequestId([1; 16]), start + Duration::from_secs(2)).unwrap();
        let wire = crate::security::protocol::encode_deadline(scope.deadline).unwrap();
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
            crate::security::protocol::encode_deadline(scope.deadline).unwrap(),
            wire
        );
        assert_eq!(
            crate::security::protocol::decode_deadline(wire).unwrap().0,
            scope.deadline.0
        );
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
