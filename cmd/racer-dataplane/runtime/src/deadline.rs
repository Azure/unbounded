//! Monotonic deadlines and bounded cancellation notification registrations.
use crate::{Error, Result};
use std::{
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, Ordering},
    },
    task::Waker,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

#[derive(Clone, Copy, Debug)]
pub struct Deadline(pub Instant);

/// Milliseconds since the Unix epoch, rejecting pre-epoch and overflowing times.
pub fn unix_millis(time: SystemTime) -> Result<u64> {
    u64::try_from(
        time.duration_since(UNIX_EPOCH)
            .map_err(|_| Error::InvalidInput)?
            .as_millis(),
    )
    .map_err(|_| Error::InvalidInput)
}

impl Deadline {
    /// Map through the stable environment clock anchor, not a renewed timeout.
    /// Submillisecond precision is truncated, never rounded up.
    pub fn to_unix_millis(self) -> Result<u64> {
        self.to_unix_millis_at(crate::environment::clock_anchor())
    }

    /// Decode using the same stable anchor, accounting for its fractional millis.
    pub fn from_unix_millis(value: u64) -> Result<Self> {
        Self::from_unix_millis_at(value, crate::environment::clock_anchor())
    }

    fn to_unix_millis_at(self, (mono, wall): (Instant, SystemTime)) -> Result<u64> {
        let time = if self.0 >= mono {
            wall.checked_add(self.0.duration_since(mono))
        } else {
            wall.checked_sub(mono.duration_since(self.0))
        }
        .ok_or(Error::InvalidInput)?;
        unix_millis(time)
    }

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

#[derive(Clone)]
pub struct Cancellation {
    state: Arc<State>,
}

struct State {
    canceled: AtomicBool,
    registrations: Mutex<Vec<Weak<futures::task::AtomicWaker>>>,
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_util::WakeCounter;

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
