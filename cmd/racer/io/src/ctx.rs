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
    sync::{Arc, Mutex, OnceLock},
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
///
/// Besides local monotonic and wall time it tracks a reference time (the
/// simulation's true time) so a harness can skew one node's clock: a rate
/// ([`VirtualClock::set_rate_ppm`]) makes local time run fast or slow against
/// the reference, and a forward jump ([`VirtualClock::jump`]) moves local
/// monotonic and wall time at once. With the default rate local and
/// reference time are identical.
#[derive(Clone, Debug)]
pub struct VirtualClock(Arc<VirtualState>);

#[derive(Debug)]
struct VirtualState {
    origin: Instant,

    wall_origin: SystemTime,

    times: Mutex<VirtualTimes>,
}

/// Mutable clock state, updated together under one lock so readers never see a
/// torn advance. `Duration` and `SystemTime` keep the full range of the host
/// types.
#[derive(Clone, Copy, Debug)]
struct VirtualTimes {
    /// Reference (true) time since creation.
    reference: Duration,

    /// Local monotonic time since creation.
    local: Duration,

    /// Reference and local time when the current rate took effect.
    segment: (Duration, Duration),

    /// Local rate deviation in parts per million.
    rate_ppm: i64,

    /// Wall time at local time `wall_base.1`.
    wall_base: (SystemTime, Duration),
}

/// Rates are bounded so local time always moves forward and the scaled
/// arithmetic cannot overflow `u128`.
const MAX_RATE_PPM: i64 = 1_000_000;

/// `Duration` from nanoseconds, or `None` past `Duration::MAX`.
fn duration_from_nanos(nanos: u128) -> Option<Duration> {
    let secs = u64::try_from(nanos / 1_000_000_000).ok()?;
    // The remainder is below 10^9, so it fits in `u32`.
    Some(Duration::new(secs, (nanos % 1_000_000_000) as u32))
}

impl VirtualTimes {
    fn scale(&self) -> u128 {
        // `rate_ppm` lies in `-999_999..=1_000_000`, so the scale is positive.
        (1_000_000 + self.rate_ppm) as u128
    }

    /// Local time at `reference` under the current segment.
    fn local_at(&self, reference: Duration) -> Option<Duration> {
        // At most ~1.8e28 ns times 2e6, well inside `u128`.
        let delta = (reference - self.segment.0).as_nanos() * self.scale() / 1_000_000;
        self.segment.1.checked_add(duration_from_nanos(delta)?)
    }

    /// Smallest reference time at which local time reaches `local`.
    fn reference_for(&self, local: Duration) -> Option<Duration> {
        if local <= self.local {
            return Some(self.reference);
        }
        let delta = ((local - self.segment.1).as_nanos() * 1_000_000).div_ceil(self.scale());
        self.segment.0.checked_add(duration_from_nanos(delta)?)
    }

    /// Move reference time forward to `reference`, or change nothing if local
    /// monotonic or wall time would overflow.
    fn set_reference(&mut self, origin: Instant, reference: Duration) -> Option<()> {
        if reference > self.reference {
            let local = self.local_at(reference)?;
            self.check_local(origin, local)?;
            self.reference = reference;
            self.local = local;
        }
        Some(())
    }

    /// Check that `local` is representable as both an `Instant` and a wall time.
    fn check_local(&self, origin: Instant, local: Duration) -> Option<()> {
        origin.checked_add(local)?;
        self.wall_at(local)?;
        Some(())
    }

    /// Wall time at local time `local` (`local >= wall_base.1`).
    fn wall_at(&self, local: Duration) -> Option<SystemTime> {
        self.wall_base.0.checked_add(local - self.wall_base.1)
    }
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
        Self(Arc::new(VirtualState {
            origin,
            wall_origin: wall,
            times: Mutex::new(VirtualTimes {
                reference: Duration::ZERO,
                local: Duration::ZERO,
                segment: (Duration::ZERO, Duration::ZERO),
                rate_ppm: 0,
                wall_base: (wall, Duration::ZERO),
            }),
        }))
    }

    /// Lock the mutable times; no code panics while holding the lock.
    fn times(&self) -> std::sync::MutexGuard<'_, VirtualTimes> {
        self.0
            .times
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Current virtual monotonic time.
    pub fn now(&self) -> Instant {
        self.0.origin + self.times().local
    }

    /// Current virtual wall time.
    pub fn wall_now(&self) -> SystemTime {
        let times = self.times();
        // Every stored local time was checked against `wall_at`.
        times.wall_at(times.local).unwrap_or(times.wall_base.0)
    }

    /// Total monotonic time advanced since creation.
    pub fn elapsed(&self) -> Duration {
        self.times().local
    }

    /// Advance local monotonic and wall time together by at least `duration`
    /// (exactly `duration` at the default rate), or change neither on
    /// overflow.
    pub fn advance(&self, duration: Duration) -> Result<()> {
        let mut times = self.times();
        let target = times
            .local
            .checked_add(duration)
            .ok_or(Error::InvalidInput)?;
        let reference = times.reference_for(target).ok_or(Error::InvalidInput)?;
        times
            .set_reference(self.0.origin, reference)
            .ok_or(Error::InvalidInput)
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
        let mut times = self.times();
        times.wall_base = (wall, times.local);
        Ok(())
    }

    /// Reference (true simulation) time since creation.
    pub fn reference_elapsed(&self) -> Duration {
        self.times().reference
    }

    /// Advance reference time to `elapsed` since creation, moving local
    /// monotonic and wall time at the clock's rate. Earlier values are a
    /// no-op.
    pub fn advance_reference_to(&self, elapsed: Duration) -> Result<()> {
        self.times()
            .set_reference(self.0.origin, elapsed)
            .ok_or(Error::InvalidInput)
    }

    /// The reference time since creation at which local monotonic time
    /// reaches `local` under the current rate (now, if it already has).
    pub fn reference_at(&self, local: Instant) -> Option<Duration> {
        let local = local.checked_duration_since(self.0.origin)?;
        self.times().reference_for(local)
    }

    /// Run local time `ppm` parts per million fast (positive) or slow
    /// (negative) against reference time from now on. `ppm` must lie in
    /// `-999_999..=1_000_000`.
    pub fn set_rate_ppm(&self, ppm: i64) -> Result<()> {
        if ppm <= -MAX_RATE_PPM || ppm > MAX_RATE_PPM {
            return Err(Error::InvalidInput);
        }
        let mut times = self.times();
        times.segment = (times.reference, times.local);
        times.rate_ppm = ppm;
        Ok(())
    }

    /// The current rate deviation in parts per million.
    pub fn rate_ppm(&self) -> i64 {
        self.times().rate_ppm
    }

    /// Jump local monotonic and wall time forward by `offset` without
    /// moving reference time (a suspended VM or a stepped clock).
    pub fn jump(&self, offset: Duration) -> Result<()> {
        let mut times = self.times();
        let local = times.local.checked_add(offset).ok_or(Error::InvalidInput)?;
        times
            .check_local(self.0.origin, local)
            .ok_or(Error::InvalidInput)?;
        // The segment start is at most `local`, so this cannot overflow.
        times.segment.1 += offset;
        times.local = local;
        Ok(())
    }
}

/// Test support: one simulated world's virtual clock and the seed its role
/// contexts draw entropy from. Give each role a stable, unique stream.
#[derive(Clone, Debug)]
pub struct SimClock {
    seed: u64,

    clock: VirtualClock,
}

impl SimClock {
    /// A world on [`VirtualClock::new`]'s fixed anchors.
    pub fn new(seed: u64) -> Self {
        Self {
            seed,
            clock: VirtualClock::new(),
        }
    }

    /// A world with explicit monotonic and wall-clock anchors.
    pub fn new_at(seed: u64, monotonic: Instant, wall: SystemTime) -> Self {
        Self {
            seed,
            clock: VirtualClock::new_at(monotonic, wall),
        }
    }

    /// The world's seed.
    pub fn seed(&self) -> u64 {
        self.seed
    }

    /// A role context on this world's clock with entropy stream `stream`.
    /// Calling again with the same stream replays its bytes.
    pub fn ctx(&self, stream: u64) -> WorkerCtx {
        WorkerCtx::simulated(&self.clock, self.seed, stream)
    }

    /// The world's virtual clock.
    pub fn virtual_clock(&self) -> &VirtualClock {
        &self.clock
    }

    /// Advance time, panicking if the monotonic instant would overflow.
    pub fn advance(&self, duration: Duration) {
        self.checked_advance(duration).expect("clock overflow");
    }

    /// Advance time, or leave it unchanged when the monotonic instant would overflow.
    pub fn checked_advance(&self, duration: Duration) -> Result<()> {
        self.clock
            .now()
            .checked_add(duration)
            .ok_or(Error::InvalidInput)?;
        self.clock.advance(duration)
    }

    /// Correct wall time without changing elapsed time or the stable anchor.
    pub fn set_wall_time(&self, wall: SystemTime) {
        self.clock
            .set_wall_time(wall)
            .expect("simulated wall time is after the Unix epoch");
    }

    /// Total monotonic time advanced since the world was created.
    pub fn elapsed(&self) -> Duration {
        self.clock.elapsed()
    }

    /// Current monotonic time.
    pub fn now(&self) -> Instant {
        self.clock.now()
    }

    /// Current wall time.
    pub fn wall_now(&self) -> SystemTime {
        self.clock.wall_now()
    }
}

impl Default for VirtualClock {
    /// Same as [`VirtualClock::new`].
    fn default() -> Self {
        Self::new()
    }
}

/// An external stream of raw choices for simulation.
///
/// A deterministic simulation harness implements this over its single choice
/// stream so that seeded entropy ([`Rng::from_source`]) and simulated OS
/// scheduling ([`crate::sim::SimOs::with_choices`]) are drawn from it
/// directly. Derived choices map the raw draw `0` to the simplest outcome
/// (no delay, identity order, cancel CQE first), so shrinking a stream toward
/// zeros simplifies the run.
pub trait ChoiceSource {
    /// The next raw 64-bit draw.
    fn next_u64(&mut self) -> u64;
}

impl std::fmt::Debug for dyn ChoiceSource {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ChoiceSource")
    }
}

impl ChoiceSource for Xoshiro {
    fn next_u64(&mut self) -> u64 {
        self.next()
    }
}

/// A value in `0..bound` (`bound > 0`) from `source`; raw `0` yields `0`.
pub(crate) fn below(source: &mut dyn ChoiceSource, bound: u64) -> u64 {
    debug_assert!(bound > 0);
    source.next_u64() % bound
}

/// Entropy for one worker role: operating-system entropy or a simulated stream.
#[derive(Clone)]
pub struct Rng(RngRepr);

#[derive(Clone)]
enum RngRepr {
    Os,
    Simulated(Arc<Mutex<dyn ChoiceSource + Send>>),
}

impl std::fmt::Debug for Rng {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self.0 {
            RngRepr::Os => "Rng::Os",
            RngRepr::Simulated(_) => "Rng::Simulated",
        })
    }
}

impl Rng {
    /// Operating-system entropy (`getrandom`).
    pub fn os() -> Self {
        Self(RngRepr::Os)
    }

    /// Test support: a replayable stream. The same `(seed, stream)` always
    /// yields the same bytes; clones share one cursor.
    pub fn seeded(seed: u64, stream: u64) -> Self {
        Self::from_source(Xoshiro::new(seed, stream))
    }

    /// Test support: entropy drawn from an external choice stream, one raw
    /// draw per eight bytes. Clones share the source.
    pub fn from_source(source: impl ChoiceSource + Send + 'static) -> Self {
        Self(RngRepr::Simulated(Arc::new(Mutex::new(source))))
    }

    /// Whether this stream is simulated (never true in production).
    pub fn is_seeded(&self) -> bool {
        matches!(self.0, RngRepr::Simulated(_))
    }

    /// Fill `bytes` with entropy.
    pub fn fill(&self, bytes: &mut [u8]) -> Result<()> {
        match &self.0 {
            RngRepr::Os => getrandom::getrandom(bytes).map_err(|_| Error::Unavailable),
            RngRepr::Simulated(state) => {
                let mut state = state.lock().map_err(|_| Error::Unavailable)?;
                for chunk in bytes.chunks_mut(8) {
                    let word = state.next_u64().to_le_bytes();
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
    fn virtual_clock_keeps_full_duration_range_and_rejects_overflow_atomically() {
        let clock = VirtualClock::new_at(Instant::now(), UNIX_EPOCH - Duration::from_secs(1));
        assert_eq!(clock.wall_now(), UNIX_EPOCH - Duration::from_secs(1));
        let beyond_nanos = Duration::from_secs(u64::MAX / 1_000_000_000 + 1);
        clock.advance(beyond_nanos).unwrap();
        assert_eq!(clock.elapsed(), beyond_nanos);
        assert_eq!(
            clock.wall_now(),
            UNIX_EPOCH - Duration::from_secs(1) + beyond_nanos
        );
        let (now, wall) = (clock.now(), clock.wall_now());
        assert!(clock.advance(Duration::MAX).is_err());
        assert_eq!((clock.now(), clock.wall_now()), (now, wall));
    }

    #[test]
    fn virtual_clock_rate_and_jump_skew_monotonic_time() {
        let clock = VirtualClock::new();
        let start = clock.now();
        let wall = clock.wall_now();
        assert!(clock.set_rate_ppm(-1_000_000).is_err());
        assert!(clock.set_rate_ppm(1_000_001).is_err());
        clock.set_rate_ppm(500_000).unwrap();
        assert_eq!(clock.rate_ppm(), 500_000);
        clock
            .advance_reference_to(Duration::from_millis(10))
            .unwrap();
        assert_eq!(clock.now() - start, Duration::from_millis(15));
        assert_eq!(
            clock.wall_now().duration_since(wall).unwrap(),
            Duration::from_millis(15)
        );
        // A local deadline maps back to reference time at the current rate.
        let at = start + Duration::from_millis(30);
        assert_eq!(clock.reference_at(at), Some(Duration::from_millis(20)));
        assert_eq!(clock.reference_at(start), Some(Duration::from_millis(10)));
        // Local advance moves reference time by the scaled amount.
        clock.advance_to(at).unwrap();
        assert_eq!(clock.now(), at);
        assert_eq!(clock.reference_elapsed(), Duration::from_millis(20));
        // A rate change only affects later time; earlier references are a no-op.
        clock.set_rate_ppm(-500_000).unwrap();
        clock
            .advance_reference_to(Duration::from_millis(5))
            .unwrap();
        clock
            .advance_reference_to(Duration::from_millis(30))
            .unwrap();
        assert_eq!(clock.now() - start, Duration::from_millis(35));
        clock.jump(Duration::from_millis(1)).unwrap();
        assert_eq!(clock.now() - start, Duration::from_millis(36));
        assert_eq!(clock.reference_elapsed(), Duration::from_millis(30));
        assert_eq!(
            clock.wall_now().duration_since(wall).unwrap(),
            Duration::from_millis(36)
        );
        clock
            .advance_reference_to(Duration::from_millis(32))
            .unwrap();
        assert_eq!(clock.now() - start, Duration::from_millis(37));
        assert!(clock.jump(Duration::MAX).is_err());
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

    /// Counts up from a start value.
    struct Counter(u64);

    impl ChoiceSource for Counter {
        fn next_u64(&mut self) -> u64 {
            self.0 += 1;
            self.0
        }
    }

    #[test]
    fn external_source_drives_entropy() {
        let rng = Rng::from_source(Counter(0));
        assert!(rng.is_seeded());
        assert_eq!(rng.next_u64().unwrap(), 1);
        let clone = rng.clone();
        assert_eq!(clone.next_u64().unwrap(), 2, "clones share the source");
        let mut bytes = [0; 10];
        rng.fill(&mut bytes).unwrap();
        assert_eq!(bytes, [3, 0, 0, 0, 0, 0, 0, 0, 4, 0]);
        assert_eq!(below(&mut Counter(6), 7), 0);
        assert!(WorkerCtx::simulated(&VirtualClock::new(), 1, 0).is_simulated());
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
