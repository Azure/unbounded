//! A manually advanced, seeded clock.
//!
//! [`SimClock`] never reads host time after construction. Time moves only when
//! a test calls [`advance`](SimClock::advance) (or a [`SimBackend`](crate::SimBackend) configured
//! with [`set_auto_advance`](crate::SimBackend::set_auto_advance) jumps to
//! the next timer deadline), so a program driven by the same seed observes the
//! same instants on every run.
//!
//! The clock is `Send + Sync` so one world clock can be shared by simulated
//! workers running on several threads. Reads never block: monotonic time is
//! a single atomic load and wall time a short sequence-checked read; only
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
use std::sync::atomic::{AtomicI64, AtomicU32, AtomicU64, Ordering, fence};
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
    /// Whole wall seconds relative to `wall_origin` (rounded toward negative
    /// infinity), so any representable wall time can be set.
    wall_secs: AtomicI64,
    /// Nanoseconds added to `wall_secs`, below one second.
    wall_nanos: AtomicU32,
    /// Odd while a writer updates `wall_secs` and `wall_nanos`.
    wall_sequence: AtomicU64,
    /// Serializes writers so `elapsed` and the wall offset move together.
    writer: Mutex<()>,
}

/// A wall offset from `wall_origin`: whole seconds plus nanoseconds below one
/// second.
type WallOffset = (i64, u32);

const NANOS_PER_SEC: u32 = 1_000_000_000;

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
            wall_secs: AtomicI64::new(0),
            wall_nanos: AtomicU32::new(0),
            wall_sequence: AtomicU64::new(0),
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
        let (secs, nanos) = self.wall_offset();
        let nanos = nanos + duration.subsec_nanos();
        let wall = i64::try_from(duration.as_secs())
            .ok()
            .and_then(|step| secs.checked_add(step))
            .and_then(|secs| secs.checked_add(i64::from(nanos / NANOS_PER_SEC)))
            .map(|secs| (secs, nanos % NANOS_PER_SEC))
            .ok_or_else(overflow)?;
        self.wall_at(wall).ok_or_else(overflow)?;
        self.0.elapsed.store(elapsed, Ordering::Release);
        self.store_wall(wall);
        Ok(())
    }

    /// Correct the wall clock without changing monotonic time.
    ///
    /// # Panics
    ///
    /// Panics if `wall` is more than `i64::MAX` seconds away from
    /// [`wall_origin`](Self::wall_origin), which no `SystemTime` near the
    /// origin can be.
    pub fn set_wall_time(&self, wall: SystemTime) {
        let offset = match wall.duration_since(self.0.wall_origin) {
            Ok(after) => i64::try_from(after.as_secs())
                .ok()
                .map(|secs| (secs, after.subsec_nanos())),
            Err(before) => {
                let before = before.duration();
                i64::try_from(before.as_secs())
                    .ok()
                    .map(|secs| match before.subsec_nanos() {
                        0 => (-secs, 0),
                        nanos => (-secs - 1, NANOS_PER_SEC - nanos),
                    })
            }
        }
        .expect("SimClock::set_wall_time: too far from the wall origin");
        let _writer = self.0.writer.lock().unwrap_or_else(|e| e.into_inner());
        self.store_wall(offset);
    }

    /// Read a consistent wall offset without blocking writers.
    fn wall_offset(&self) -> WallOffset {
        loop {
            let before = self.0.wall_sequence.load(Ordering::Acquire);
            if before % 2 == 1 {
                std::hint::spin_loop();
                continue;
            }
            let secs = self.0.wall_secs.load(Ordering::Relaxed);
            let nanos = self.0.wall_nanos.load(Ordering::Relaxed);
            fence(Ordering::Acquire);
            if self.0.wall_sequence.load(Ordering::Relaxed) == before {
                return (secs, nanos);
            }
        }
    }

    /// Publish a wall offset. Callers hold the writer lock.
    fn store_wall(&self, (secs, nanos): WallOffset) {
        let sequence = self.0.wall_sequence.load(Ordering::Relaxed);
        self.0.wall_sequence.store(sequence + 1, Ordering::Relaxed);
        fence(Ordering::Release);
        self.0.wall_secs.store(secs, Ordering::Relaxed);
        self.0.wall_nanos.store(nanos, Ordering::Relaxed);
        self.0.wall_sequence.store(sequence + 2, Ordering::Release);
    }

    fn wall_at(&self, (secs, nanos): WallOffset) -> Option<SystemTime> {
        let whole = Duration::from_secs(secs.unsigned_abs());
        let base = if secs >= 0 {
            self.0.wall_origin.checked_add(whole)
        } else {
            self.0.wall_origin.checked_sub(whole)
        };
        base?.checked_add(Duration::from_nanos(u64::from(nanos)))
    }
}

impl Clock for SimClock {
    fn now(&self) -> Instant {
        // `checked_advance` guarantees this addition cannot overflow.
        self.0.origin + self.elapsed()
    }

    fn wall_now(&self) -> SystemTime {
        self.wall_at(self.wall_offset())
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
    }

    #[test]
    fn wall_time_is_not_limited_to_a_nanosecond_range() {
        let clock = SimClock::new(1);
        // Far beyond the roughly 292 years an i64 of nanoseconds can span,
        // like the year-4096 expiry rcgen certificates default to.
        let far = SystemTime::UNIX_EPOCH + Duration::from_secs(66_000_000_000);
        clock.set_wall_time(far);
        assert_eq!(clock.wall_now(), far);
        clock.advance(Duration::from_millis(1500));
        assert_eq!(clock.wall_now(), far + Duration::from_millis(1500));
        let before = SystemTime::UNIX_EPOCH - Duration::new(20_000_000_000, 250);
        clock.set_wall_time(before);
        assert_eq!(clock.wall_now(), before);
        clock.advance(Duration::from_nanos(250));
        assert_eq!(
            clock.wall_now(),
            SystemTime::UNIX_EPOCH - Duration::from_secs(20_000_000_000)
        );
        clock.set_wall_time(clock.wall_origin());
        assert_eq!(clock.wall_now(), clock.wall_origin());
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
