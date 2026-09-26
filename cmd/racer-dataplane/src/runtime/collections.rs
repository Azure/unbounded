//! Production keeps std's randomized hashing. Simulated worlds use a seeded
//! hasher so iteration, wake order, eviction, and destruction replay exactly.
#[cfg(not(test))]
pub type HashMap<K, V> = std::collections::HashMap<K, V>;
#[cfg(not(test))]
pub type HashSet<K> = std::collections::HashSet<K>;

#[cfg(test)]
pub type HashMap<K, V> = std::collections::HashMap<K, V, State>;
#[cfg(test)]
pub type HashSet<K> = std::collections::HashSet<K, State>;

#[cfg(test)]
#[derive(Clone, Debug)]
pub enum State {
    Real(std::collections::hash_map::RandomState),
    Simulated(u64),
}
#[cfg(test)]
impl Default for State {
    fn default() -> Self {
        match super::environment::simulation_seed() {
            Some(seed) => Self::Simulated(seed),
            None => Self::Real(std::collections::hash_map::RandomState::new()),
        }
    }
}
#[cfg(test)]
impl std::hash::BuildHasher for State {
    type Hasher = std::collections::hash_map::DefaultHasher;
    fn build_hasher(&self) -> Self::Hasher {
        use std::hash::Hasher;
        match self {
            Self::Real(state) => state.build_hasher(),
            Self::Simulated(seed) => {
                let mut hasher = Self::Hasher::new();
                hasher.write_u64(*seed);
                hasher
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn simulation_replays_collection_iteration_without_consuming_nonce_entropy() {
        use crate::runtime::environment::{SimulationClock, fill_random};
        fn sample(seed: u64) -> (Vec<u64>, [u8; 32]) {
            let clock = SimulationClock::new(seed);
            let _role = clock.environment(9).enter();
            let mut map = HashMap::default();
            for id in 0..100 {
                map.insert(id, id);
            }
            for id in (0..100).step_by(3) {
                map.remove(&id);
            }
            let mut bytes = [0; 32];
            fill_random(&mut bytes).unwrap();
            (map.into_keys().collect(), bytes)
        }
        assert_eq!(sample(1), sample(1));
        assert_ne!(sample(1).0, sample(7).0);
        let clock = SimulationClock::new(1);
        let _role = clock.environment(9).enter();
        let mut bytes = [0; 32];
        fill_random(&mut bytes).unwrap();
        assert_eq!(sample(1).1, bytes);
        let _real = crate::runtime::environment::Environment::default().enter();
        assert!(matches!(State::default(), State::Real(_)));
    }
}
