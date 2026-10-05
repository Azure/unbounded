//! Explicit-time second-sight history and exact resident heat for arbitrary keys.

use std::collections::{HashMap, hash_map::RandomState};
use std::hash::{BuildHasher, Hash};
use std::time::{Duration, Instant};

const GENERATIONS: usize = 4;

/// Bounded rotating Bloom history. A hit is a hint, never identity or ownership.
pub struct SecondSight {
    hash: RandomState,

    pages: [Vec<u64>; GENERATIONS],

    occupancy: [usize; GENERATIONS],

    current: usize,

    rotated: Instant,

    period: Duration,
}

impl SecondSight {
    /// Construct four generations within the byte budget, starting at `now`.
    pub fn new(bytes: usize, period: Duration, now: Instant) -> Option<Self> {
        let words = bytes / (GENERATIONS * 8);
        if words == 0 || period.is_zero() || period.as_nanos() > u64::MAX as u128 {
            return None;
        }
        Some(Self {
            hash: RandomState::new(),
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

/// Exact, capacity-bounded resident heat. Only explicitly tracked keys can heat up.
pub struct Heat<K> {
    entries: HashMap<K, Temperature>,

    capacity: usize,
}

/// Saturating heat and its last whole-minute decay boundary.
struct Temperature {
    value: u8,

    decayed: Instant,
}

impl Temperature {
    /// Decay at most three heat units without losing a partial minute.
    fn decay(&mut self, now: Instant) {
        let periods = now.saturating_duration_since(self.decayed).as_secs() / 60;
        self.value = self.value.saturating_sub(periods.min(3) as u8);
        self.decayed += Duration::from_secs(periods * 60);
    }
}

impl<K: Eq + Hash + Clone> Heat<K> {
    /// Create an empty exact table; zero capacity is invalid.
    pub fn new(capacity: usize) -> Option<Self> {
        (capacity != 0).then(|| Self {
            entries: HashMap::new(),
            capacity,
        })
    }

    /// Install a cold key without replacing another resident's heat.
    pub fn track(&mut self, key: &K, now: Instant) -> bool {
        if self.entries.contains_key(key) {
            return true;
        }
        if self.entries.len() >= self.capacity {
            return false;
        }
        self.entries.insert(
            key.clone(),
            Temperature {
                value: 0,
                decayed: now,
            },
        );
        true
    }

    /// Release heat when the caller's final residency owner disappears.
    pub fn forget(&mut self, key: &K) {
        self.entries.remove(key);
    }

    /// Count one access to an already resident key, saturating at three.
    pub fn touch(&mut self, key: &K, now: Instant) {
        if let Some(heat) = self.entries.get_mut(key) {
            heat.decay(now);
            heat.value = (heat.value + 1).min(3);
        }
    }

    /// Return current heat, with no implicit admission or ownership bias.
    pub fn score(&mut self, key: &K, now: Instant) -> u8 {
        self.entries.get_mut(key).map_or(0, |heat| {
            heat.decay(now);
            heat.value
        })
    }

    /// Count exactly tracked resident keys.
    pub fn len(&self) -> usize {
        self.entries.len()
    }

    /// Whether any resident heat remains.
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
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

    #[test]
    fn exact_heat_is_bounded_saturates_decays_and_forgets() {
        let now = Instant::now();
        let mut heat = Heat::new(2).unwrap();
        assert!(heat.track(&1, now));
        assert!(heat.track(&2, now));
        assert!(!heat.track(&3, now));
        for _ in 0..8 {
            heat.touch(&1, now);
        }
        assert_eq!(heat.score(&1, now), 3);
        assert_eq!(heat.score(&2, now), 0);
        assert_eq!(heat.score(&1, now + Duration::from_secs(119)), 2);
        assert_eq!(heat.score(&1, now + Duration::from_secs(120)), 1);
        assert_eq!(heat.score(&1, now + Duration::from_secs(180)), 0);
        heat.forget(&1);
        assert!(heat.track(&3, now));
        assert_eq!(heat.len(), 2);
        assert!(Heat::<u64>::new(0).is_none());
    }
}
