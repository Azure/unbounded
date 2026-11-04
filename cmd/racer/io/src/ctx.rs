//! Explicit worker time and entropy.
//!
//! Production builds a [`WorkerCtx`] with [`WorkerCtx::host`]: the host
//! monotonic and wall clocks plus operating-system entropy. Deterministic runs
//! build one with [`WorkerCtx::simulated`], which pairs a [`VirtualClock`] with a
//! seeded [`Rng`]. No clock or entropy source is reachable through thread-local
//! or process-global state, so many simulated nodes can share one thread.
//!
//! Guard: a reactor using the kernel driver rejects a simulated context
//! ([`crate::ring::Reactor::new`]), so a production reactor can never be paired
//! with seeded nonces.
use crate::{Error, Result};
use std::{
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicU64, Ordering},
    },
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// Monotonic and wall time for one worker role.
#[derive(Clone, Debug)]
pub struct Clock(ClockRepr);

#[derive(Clone, Debug)]
enum ClockRepr {
    Host,
    Virtual(VirtualClock),
}

/// A manually advanced clock shared by every role of one simulated world.
#[derive(Clone, Debug)]
pub struct VirtualClock(Arc<VirtualState>);

#[derive(Debug)]
struct VirtualState {
    origin: Instant,

    wall_origin: SystemTime,

    elapsed: AtomicU64,

    /// Wall time as nanoseconds since the Unix epoch.
    wall: AtomicU64,
}

impl Clock {
    /// The host monotonic and wall clocks.
    pub fn host() -> Self {
        Self(ClockRepr::Host)
    }

    /// Whether this clock is virtual.
    pub fn is_virtual(&self) -> bool {
        matches!(self.0, ClockRepr::Virtual(_))
    }

    /// The virtual clock, if this is one.
    pub fn as_virtual(&self) -> Option<&VirtualClock> {
        match &self.0 {
            ClockRepr::Virtual(clock) => Some(clock),
            ClockRepr::Host => None,
        }
    }

    /// Current monotonic time.
    #[inline]
    pub fn now(&self) -> Instant {
        match &self.0 {
            ClockRepr::Host => Instant::now(),
            ClockRepr::Virtual(clock) => clock.now(),
        }
    }

    /// Current wall time.
    pub fn wall_now(&self) -> SystemTime {
        match &self.0 {
            ClockRepr::Host => SystemTime::now(),
            ClockRepr::Virtual(clock) => clock.wall_now(),
        }
    }

    /// A stable `(monotonic, wall)` pair used to map deadlines to wire time.
    /// It never changes for the lifetime of the clock domain.
    pub fn anchor(&self) -> (Instant, SystemTime) {
        match &self.0 {
            ClockRepr::Host => {
                static ANCHOR: OnceLock<(Instant, SystemTime)> = OnceLock::new();
                *ANCHOR.get_or_init(|| (Instant::now(), SystemTime::now()))
            }
            ClockRepr::Virtual(clock) => (clock.0.origin, clock.0.wall_origin),
        }
    }
}

impl From<VirtualClock> for Clock {
    /// Read time from a virtual clock.
    fn from(clock: VirtualClock) -> Self {
        Self(ClockRepr::Virtual(clock))
    }
}

impl VirtualClock {
    /// Test support: a virtual clock starting at a fixed process-local instant
    /// and a fixed wall epoch (2027-01-15).
    pub fn new() -> Self {
        static ORIGIN: OnceLock<Instant> = OnceLock::new();
        Self::new_at(
            *ORIGIN.get_or_init(Instant::now),
            UNIX_EPOCH + Duration::from_secs(1_800_000_000),
        )
    }

    /// Test support: a virtual clock with explicit anchors.
    pub fn new_at(origin: Instant, wall: SystemTime) -> Self {
        let wall_nanos = wall
            .duration_since(UNIX_EPOCH)
            .map_or(0, |since| since.as_nanos().min(u64::MAX as u128) as u64);
        Self(Arc::new(VirtualState {
            origin,
            wall_origin: wall,
            elapsed: AtomicU64::new(0),
            wall: AtomicU64::new(wall_nanos),
        }))
    }

    /// Current virtual monotonic time.
    pub fn now(&self) -> Instant {
        self.0.origin + Duration::from_nanos(self.0.elapsed.load(Ordering::Acquire))
    }

    /// Current virtual wall time.
    pub fn wall_now(&self) -> SystemTime {
        UNIX_EPOCH + Duration::from_nanos(self.0.wall.load(Ordering::Acquire))
    }

    /// Total monotonic time advanced since creation.
    pub fn elapsed(&self) -> Duration {
        Duration::from_nanos(self.0.elapsed.load(Ordering::Acquire))
    }

    /// Advance monotonic and wall time together.
    pub fn advance(&self, duration: Duration) -> Result<()> {
        let nanos = u64::try_from(duration.as_nanos()).map_err(|_| Error::InvalidInput)?;
        let elapsed = self.0.elapsed.load(Ordering::Acquire);
        let wall = self.0.wall.load(Ordering::Acquire);
        let (Some(next), Some(next_wall)) = (elapsed.checked_add(nanos), wall.checked_add(nanos))
        else {
            return Err(Error::InvalidInput);
        };
        self.0.elapsed.store(next, Ordering::Release);
        self.0.wall.store(next_wall, Ordering::Release);
        Ok(())
    }

    /// Advance monotonic time to `until` if it is later than now.
    pub fn advance_to(&self, until: Instant) -> Result<()> {
        let now = self.now();
        if until > now {
            self.advance(until - now)?;
        }
        Ok(())
    }

    /// Correct wall time without changing monotonic time or the anchor.
    pub fn set_wall_time(&self, wall: SystemTime) -> Result<()> {
        let nanos = wall
            .duration_since(UNIX_EPOCH)
            .ok()
            .and_then(|since| u64::try_from(since.as_nanos()).ok())
            .ok_or(Error::InvalidInput)?;
        self.0.wall.store(nanos, Ordering::Release);
        Ok(())
    }
}

impl Default for VirtualClock {
    /// Same as [`VirtualClock::new`].
    fn default() -> Self {
        Self::new()
    }
}

/// Entropy for one worker role: operating-system entropy or a seeded stream.
#[derive(Clone, Debug)]
pub struct Rng(RngRepr);

#[derive(Clone, Debug)]
enum RngRepr {
    Os,
    Seeded(Arc<Mutex<Xoshiro>>),
}

impl Rng {
    /// Operating-system entropy (`getrandom`).
    pub fn os() -> Self {
        Self(RngRepr::Os)
    }

    /// Test support: a replayable stream. The same `(seed, stream)` always
    /// yields the same bytes; clones share one cursor.
    pub fn seeded(seed: u64, stream: u64) -> Self {
        Self(RngRepr::Seeded(Arc::new(Mutex::new(Xoshiro::new(
            seed, stream,
        )))))
    }

    /// Whether this stream is seeded (never true in production).
    pub fn is_seeded(&self) -> bool {
        matches!(self.0, RngRepr::Seeded(_))
    }

    /// Fill `bytes` with entropy.
    pub fn fill(&self, bytes: &mut [u8]) -> Result<()> {
        match &self.0 {
            RngRepr::Os => getrandom::getrandom(bytes).map_err(|_| Error::Unavailable),
            RngRepr::Seeded(state) => {
                let mut state = state.lock().map_err(|_| Error::Unavailable)?;
                for chunk in bytes.chunks_mut(8) {
                    let word = state.next().to_le_bytes();
                    chunk.copy_from_slice(&word[..chunk.len()]);
                }
                Ok(())
            }
        }
    }

    /// A uniformly distributed `u64`.
    pub fn next_u64(&self) -> Result<u64> {
        let mut bytes = [0; 8];
        self.fill(&mut bytes)?;
        Ok(u64::from_le_bytes(bytes))
    }
}

/// xoshiro256** seeded through SplitMix64.
#[derive(Clone, Debug)]
pub(crate) struct Xoshiro([u64; 4]);

impl Xoshiro {
    /// Derive an independent stream from a seed and a stream identifier.
    pub(crate) fn new(seed: u64, stream: u64) -> Self {
        let mut mix = seed ^ stream.wrapping_mul(0x9E37_79B9_7F4A_7C15).rotate_left(17);
        let mut next = || {
            mix = mix.wrapping_add(0x9E37_79B9_7F4A_7C15);
            let mut z = mix;
            z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
            z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
            z ^ (z >> 31)
        };
        Self([next(), next(), next(), next()])
    }

    /// Next 64 output bits.
    pub(crate) fn next(&mut self) -> u64 {
        let s = &mut self.0;
        let result = s[1].wrapping_mul(5).rotate_left(7).wrapping_mul(9);
        let t = s[1] << 17;
        s[2] ^= s[0];
        s[3] ^= s[1];
        s[1] ^= s[2];
        s[0] ^= s[3];
        s[2] ^= t;
        s[3] = s[3].rotate_left(45);
        result
    }

    /// A value in `0..bound` (`bound > 0`), without modulo bias.
    #[allow(dead_code)]
    pub(crate) fn below(&mut self, bound: u64) -> u64 {
        debug_assert!(bound > 0);
        let zone = u64::MAX - u64::MAX % bound;
        loop {
            let value = self.next();
            if value < zone {
                return value % bound;
            }
        }
    }
}

/// Time and entropy for one worker role, passed explicitly.
#[derive(Clone, Debug)]
pub struct WorkerCtx {
    /// Monotonic and wall time.
    pub clock: Clock,

    /// Nonces, jitter, and identifiers.
    pub rng: Rng,
}

impl WorkerCtx {
    /// Production context: host clocks and operating-system entropy.
    pub fn host() -> Self {
        Self {
            clock: Clock::host(),
            rng: Rng::os(),
        }
    }

    /// Test support: a deterministic context sharing `clock` with its world.
    pub fn simulated(clock: &VirtualClock, seed: u64, stream: u64) -> Self {
        Self {
            clock: clock.clone().into(),
            rng: Rng::seeded(seed, stream),
        }
    }

    /// Whether either time or entropy is simulated.
    pub fn is_simulated(&self) -> bool {
        self.clock.is_virtual() || self.rng.is_seeded()
    }

    /// Current monotonic time.
    #[inline]
    pub fn now(&self) -> Instant {
        self.clock.now()
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

/// Map a monotonic instant to Unix milliseconds through a stable anchor.
/// Submillisecond precision is truncated, never rounded up.
pub fn instant_to_unix_millis(
    instant: Instant,
    (mono, wall): (Instant, SystemTime),
) -> Result<u64> {
    let time = if instant >= mono {
        wall.checked_add(instant.duration_since(mono))
    } else {
        wall.checked_sub(mono.duration_since(instant))
    }
    .ok_or(Error::InvalidInput)?;
    unix_millis(time)
}

/// Map Unix milliseconds back to a monotonic instant through a stable anchor.
pub fn unix_millis_to_instant(value: u64, (mono, wall): (Instant, SystemTime)) -> Result<Instant> {
    let base = unix_millis(wall)?;
    let fraction = wall
        .duration_since(UNIX_EPOCH)
        .map_err(|_| Error::InvalidInput)?
        .subsec_nanos()
        % 1_000_000;
    if value >= base {
        mono.checked_add(Duration::from_millis(value - base))
    } else {
        mono.checked_sub(Duration::from_millis(base - value))
    }
    .and_then(|instant| instant.checked_sub(Duration::from_nanos(u64::from(fraction))))
    .ok_or(Error::InvalidInput)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn virtual_clock_advances_both_clocks_and_keeps_anchor() {
        let clock = VirtualClock::new();
        let start = clock.now();
        let wall = clock.wall_now();
        clock.advance(Duration::from_millis(5)).unwrap();
        assert_eq!(clock.now() - start, Duration::from_millis(5));
        assert_eq!(
            clock.wall_now().duration_since(wall).unwrap(),
            Duration::from_millis(5)
        );
        clock
            .set_wall_time(UNIX_EPOCH + Duration::from_secs(10))
            .unwrap();
        assert_eq!(clock.now() - start, Duration::from_millis(5));
        let shared: Clock = clock.clone().into();
        assert_eq!(shared.anchor(), (start, wall));
        assert!(clock.advance(Duration::MAX).is_err());
        clock.advance_to(start).unwrap();
        assert_eq!(clock.elapsed(), Duration::from_millis(5));
    }

    #[test]
    fn seeded_rng_replays_and_streams_are_independent() {
        let mut a = [0; 37];
        let mut b = [0; 37];
        Rng::seeded(7, 1).fill(&mut a).unwrap();
        Rng::seeded(7, 1).fill(&mut b).unwrap();
        assert_eq!(a, b);
        Rng::seeded(7, 2).fill(&mut b).unwrap();
        assert_ne!(a, b);
        Rng::seeded(8, 1).fill(&mut b).unwrap();
        assert_ne!(a, b);
        let shared = Rng::seeded(1, 1);
        let clone = shared.clone();
        assert_ne!(shared.next_u64().unwrap(), clone.next_u64().unwrap());
        let mut x = Xoshiro::new(3, 3);
        for bound in [1, 2, 3, 1000] {
            assert!(x.below(bound) < bound);
        }
    }

    #[test]
    fn host_context_is_not_simulated() {
        let host = WorkerCtx::host();
        assert!(!host.is_simulated());
        let mut bytes = [0; 16];
        host.rng.fill(&mut bytes).unwrap();
        let sim = WorkerCtx::simulated(&VirtualClock::new(), 1, 0);
        assert!(sim.is_simulated());
    }

    #[test]
    fn unix_millis_round_trip_through_anchor() {
        let clock = VirtualClock::new();
        let anchor: (Instant, SystemTime) = Clock::from(clock.clone()).anchor();
        let at = clock.now() + Duration::from_millis(1500);
        let millis = instant_to_unix_millis(at, anchor).unwrap();
        let back = unix_millis_to_instant(millis, anchor).unwrap();
        assert!(back <= at && at - back < Duration::from_millis(1));
        assert!(unix_millis(UNIX_EPOCH - Duration::from_secs(1)).is_err());
    }
}
