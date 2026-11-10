//! Explicit-time second-sight admission history for arbitrary keys.

use std::collections::hash_map::RandomState;
use std::hash::{BuildHasher, Hash};
use std::time::{Duration, Instant};

const GENERATIONS: usize = 4;

/// Bounded rotating Bloom history. A hit is a hint, never identity or ownership.
///
/// Keyed by a per-process random [`RandomState`], so crafted keys cannot
/// target filter bits.
pub struct SecondSight<S = RandomState> {
    hash: S,

    pages: [Vec<u64>; GENERATIONS],

    occupancy: [usize; GENERATIONS],

    current: usize,

    rotated: Instant,

    period: Duration,
}

impl SecondSight {
    /// Construct four generations within the byte budget, starting at `now`.
    pub fn new(bytes: usize, period: Duration, now: Instant) -> Option<Self> {
        Self::with_hasher(bytes, period, now, RandomState::new())
    }
}

impl<S: BuildHasher> SecondSight<S> {
    fn with_hasher(bytes: usize, period: Duration, now: Instant, hash: S) -> Option<Self> {
        let words = bytes / (GENERATIONS * 8);
        if words == 0 || period.is_zero() || period.as_nanos() > u64::MAX as u128 {
            return None;
        }
        Some(Self {
            hash,
            pages: std::array::from_fn(|_| vec![0; words]),
            occupancy: [0; GENERATIONS],
            current: 0,
            rotated: now,
            period,
        })
    }

    /// Check prior history before inserting this observation.
    pub fn observe(&mut self, key: &impl Hash, now: Instant) -> bool {
        self.rotate(now);
        let hash = self.hash.hash_one(key);
        let step = hash.rotate_left(29) | 1;
        let bits: [usize; 4] = std::array::from_fn(|i| {
            hash.wrapping_add(step.wrapping_mul(i as u64)) as usize % (self.pages[0].len() * 64)
        });
        let seen = self.pages.iter().any(|generation| {
            bits.iter()
                .all(|bit| generation[bit / 64] & (1 << (bit % 64)) != 0)
        });
        for bit in bits {
            let mask = 1 << (bit % 64);
            if self.pages[self.current][bit / 64] & mask == 0 {
                self.pages[self.current][bit / 64] |= mask;
                self.occupancy[self.current] += 1;
            }
        }
        seen
    }

    /// Return occupied and total bits after applying elapsed rotations.
    pub fn occupancy(&mut self, now: Instant) -> (usize, usize) {
        self.rotate(now);
        (
            self.occupancy.iter().sum(),
            self.pages[0].len() * 64 * GENERATIONS,
        )
    }

    /// Expire whole generations while preserving the fractional period.
    fn rotate(&mut self, now: Instant) {
        let elapsed = now.saturating_duration_since(self.rotated);
        let periods = elapsed.as_nanos() / self.period.as_nanos();
        if periods == 0 {
            return;
        }
        for _ in 0..periods.min(GENERATIONS as u128) {
            self.current = (self.current + 1) % GENERATIONS;
            self.pages[self.current].fill(0);
            self.occupancy[self.current] = 0;
        }
        self.rotated =
            now - Duration::from_nanos((elapsed.as_nanos() % self.period.as_nanos()) as u64);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rotation_retains_three_to_four_periods_and_clears_long_idle() {
        let start = Instant::now();
        for period in [Duration::from_secs(60), Duration::from_millis(10)] {
            let mut history = SecondSight::new(4096, period, start).unwrap();
            assert!(!history.observe(&0, start));
            assert!(history.observe(&0, start + period * 3));
            assert_eq!(history.occupancy(start + period * 8), (0, 4096 * 8));
            assert!(!history.observe(&0, start + period * 8));
            let mut history = SecondSight::new(4096, period, start).unwrap();
            history.observe(&0, start);
            assert!(!history.observe(&0, start + period * 4));
        }
        assert!(SecondSight::new(31, Duration::from_secs(1), start).is_none());
        assert!(SecondSight::new(32, Duration::ZERO, start).is_none());
    }
}
