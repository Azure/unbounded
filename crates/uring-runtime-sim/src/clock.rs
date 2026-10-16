//! A manually advanced, seeded clock.
//!
//! [`SimClock`] never reads host time after construction. Time moves only when
//! a test calls [`advance`](SimClock::advance) (or a [`SimBackend`](crate::SimBackend) configured
//! with [`set_auto_advance`](crate::SimBackend::set_auto_advance) jumps to
//! the next timer deadline), so a program driven by the same seed observes the
//! same instants on every run.
//!
//! The clock is `Send + Sync` so one world clock can be shared by simulated
//! workers running on several threads. Reads are a single atomic load; only
//! writers take a lock.
//!
//! ```
//! use std::time::Duration;
//! use uring_runtime::Clock;
//! use uring_runtime_sim::SimClock;
//!
//! let clock = SimClock::new(42);
//! let start = clock.now();
//! clock.advance(Duration::from_secs(5));
//! assert_eq!(clock.now() - start, Duration::from_secs(5));
//! assert_eq!(clock.elapsed(), Duration::from_secs(5));
//! assert_eq!(clock.seed(), 42);
//! ```

use std::fmt;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant, SystemTime};

use uring_runtime::{Clock, Error, ErrorKind, Result};

/// The wall-clock time a [`SimClock::new`] clock starts at: `1_800_000_000`
/// seconds after the Unix epoch (January 2027).
pub const DEFAULT_WALL_EPOCH: Duration = Duration::from_secs(1_800_000_000);

/// A deterministic, manually advanced monotonic and wall clock.
///
/// Clones share the same time. The monotonic clock starts at an origin
/// [`Instant`] and only moves forward by [`advance`](Self::advance); the wall
/// clock moves with it and can additionally be corrected with
/// [`set_wall_time`](Self::set_wall_time) without affecting monotonic time.
#[derive(Clone)]
pub struct SimClock(Arc<Inner>);

struct Inner {
    seed: u64,
    origin: Instant,
    wall_origin: SystemTime,
    /// Monotonic nanoseconds since `origin`.
    elapsed: AtomicU64,
    /// Wall nanoseconds relative to `wall_origin`.
    wall_offset: AtomicI64,
    /// Serializes writers so `elapsed` and `wall_offset` move together.
    writer: Mutex<()>,
}

impl SimClock {
    /// Create a clock for `seed`, starting at a process-global monotonic origin
    /// and at [`DEFAULT_WALL_EPOCH`].
    ///
    /// [`Instant`] has no constructor from a fixed value, so the origin is
    /// sampled from the host once per process and shared by every clock made
    /// with `new`. Programs only observe differences between instants, which
    /// are fully deterministic.
    pub fn new(seed: u64) -> Self {
        static ORIGIN: OnceLock<Instant> = OnceLock::new();
        Self::new_at(
            seed,
            *ORIGIN.get_or_init(Instant::now),
            SystemTime::UNIX_EPOCH + DEFAULT_WALL_EPOCH,
        )
    }

    /// Create a clock with explicit monotonic and wall-clock origins.
    pub fn new_at(seed: u64, monotonic: Instant, wall: SystemTime) -> Self {
        Self(Arc::new(Inner {
            seed,
            origin: monotonic,
            wall_origin: wall,
            elapsed: AtomicU64::new(0),
            wall_offset: AtomicI64::new(0),
            writer: Mutex::new(()),
        }))
    }

    /// The seed this clock was created with.
    pub fn seed(&self) -> u64 {
        self.0.seed
    }

    /// The monotonic origin: [`now`](Clock::now) before any advance.
    pub fn origin(&self) -> Instant {
        self.0.origin
    }

    /// The wall-clock origin: [`wall_now`](Clock::wall_now) at construction.
    /// Unaffected by [`set_wall_time`](Self::set_wall_time).
    pub fn wall_origin(&self) -> SystemTime {
        self.0.wall_origin
    }

    /// Total monotonic time advanced since construction.
    pub fn elapsed(&self) -> Duration {
        Duration::from_nanos(self.0.elapsed.load(Ordering::Acquire))
    }

    /// Advance monotonic and wall time by `duration`.
    ///
    /// # Panics
    ///
    /// Panics if the clock would overflow; see
    /// [`checked_advance`](Self::checked_advance). The clock is unchanged and
    /// remains usable after the panic.
    pub fn advance(&self, duration: Duration) {
        if let Err(error) = self.checked_advance(duration) {
            panic!("SimClock::advance({duration:?}): {error}");
        }
    }

    /// Advance monotonic and wall time by `duration`, or leave both unchanged
    /// and fail with [`ErrorKind::InvalidInput`] if either would overflow
    /// (more than about 584 years of total elapsed time).
    pub fn checked_advance(&self, duration: Duration) -> Result<()> {
        let overflow = || Error::new(ErrorKind::InvalidInput).with_context("SimClock overflow");
        let _writer = self.0.writer.lock().unwrap_or_else(|e| e.into_inner());
        let step = u64::try_from(duration.as_nanos()).map_err(|_| overflow())?;
        let elapsed = self
            .0
            .elapsed
            .load(Ordering::Relaxed)
            .checked_add(step)
            .ok_or_else(overflow)?;
        self.0
            .origin
            .checked_add(Duration::from_nanos(elapsed))
            .ok_or_else(overflow)?;
        let wall = i64::try_from(step)
            .ok()
            .and_then(|step| self.0.wall_offset.load(Ordering::Relaxed).checked_add(step))
            .ok_or_else(overflow)?;
        self.wall_at(wall).ok_or_else(overflow)?;
        self.0.elapsed.store(elapsed, Ordering::Release);
        self.0.wall_offset.store(wall, Ordering::Release);
        Ok(())
    }

    /// Correct the wall clock without changing monotonic time.
    ///
    /// # Panics
    ///
    /// Panics if `wall` is more than about 292 years away from
    /// [`wall_origin`](Self::wall_origin).
    pub fn set_wall_time(&self, wall: SystemTime) {
        let offset = match wall.duration_since(self.0.wall_origin) {
            Ok(after) => i64::try_from(after.as_nanos()).ok(),
            Err(before) => i64::try_from(before.duration().as_nanos()).ok().map(|n| -n),
        }
        .expect("SimClock::set_wall_time: too far from the wall origin");
        let _writer = self.0.writer.lock().unwrap_or_else(|e| e.into_inner());
        self.0.wall_offset.store(offset, Ordering::Release);
    }

    fn wall_at(&self, offset: i64) -> Option<SystemTime> {
        let magnitude = Duration::from_nanos(offset.unsigned_abs());
        if offset >= 0 {
            self.0.wall_origin.checked_add(magnitude)
        } else {
            self.0.wall_origin.checked_sub(magnitude)
        }
    }
}

impl Clock for SimClock {
    fn now(&self) -> Instant {
        // `checked_advance` guarantees this addition cannot overflow.
        self.0.origin + self.elapsed()
    }

    fn wall_now(&self) -> SystemTime {
        self.wall_at(self.0.wall_offset.load(Ordering::Acquire))
            .expect("wall offset validated on store")
    }
}

impl fmt::Debug for SimClock {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("SimClock")
            .field("seed", &self.0.seed)
            .field("elapsed", &self.elapsed())
            .field("wall_now", &self.wall_now())
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clones_share_time_and_defaults_match_old_runtime() {
        let clock = SimClock::new(9);
        let other = SimClock::new(10);
        assert_eq!(clock.origin(), other.origin());
        assert_eq!(
            clock.wall_now(),
            SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000)
        );
        let clone = clock.clone();
        clone.advance(Duration::from_millis(1500));
        assert_eq!(clock.elapsed(), Duration::from_millis(1500));
        assert_eq!(clock.now(), clock.origin() + Duration::from_millis(1500));
        assert_eq!(
            clock.wall_now(),
            clock.wall_origin() + Duration::from_millis(1500)
        );
        assert!(format!("{clock:?}").contains("seed: 9"));
    }

    #[test]
    fn overflow_is_atomic_and_panic_does_not_poison() {
        let clock = SimClock::new(9);
        let before = (clock.now(), clock.wall_now(), clock.elapsed());
        assert_eq!(
            clock.checked_advance(Duration::MAX).unwrap_err().kind(),
            ErrorKind::InvalidInput
        );
        assert_eq!((clock.now(), clock.wall_now(), clock.elapsed()), before);
        let panicking = clock.clone();
        assert!(std::panic::catch_unwind(move || panicking.advance(Duration::MAX)).is_err());
        assert_eq!((clock.now(), clock.wall_now(), clock.elapsed()), before);
        clock.checked_advance(Duration::from_secs(1)).unwrap();
        assert_eq!(clock.elapsed(), Duration::from_secs(1));
    }

    #[test]
    fn wall_corrections_do_not_move_monotonic_time() {
        let wall = SystemTime::UNIX_EPOCH + Duration::from_secs(100);
        let origin = Instant::now();
        let clock = SimClock::new_at(1, origin, wall);
        clock.set_wall_time(SystemTime::UNIX_EPOCH + Duration::from_secs(40));
        assert_eq!(
            clock.wall_now(),
            SystemTime::UNIX_EPOCH + Duration::from_secs(40)
        );
        assert_eq!(clock.now(), origin);
        clock.advance(Duration::from_secs(2));
        assert_eq!(
            clock.wall_now(),
            SystemTime::UNIX_EPOCH + Duration::from_secs(42)
        );
        assert_eq!(clock.wall_origin(), wall);
        assert!(
            std::panic::catch_unwind(
                || clock.set_wall_time(SystemTime::UNIX_EPOCH + Duration::from_secs(u64::MAX / 2))
            )
            .is_err()
        );
    }

    #[test]
    fn clock_is_send_and_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<SimClock>();
        let clock = SimClock::new(0);
        let remote = clock.clone();
        std::thread::spawn(move || remote.advance(Duration::from_secs(3)))
            .join()
            .unwrap();
        assert_eq!(clock.elapsed(), Duration::from_secs(3));
    }
}
