//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

pub mod admission;
pub mod affinity;
pub mod channel;
pub(crate) mod collections {
    //! Seeded hashing only in simulated worlds; production retains std hashing.
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
pub mod crypto;
pub mod deadline;
pub mod environment;
pub(crate) mod ingress;
#[cfg(test)]
mod listener_tests;
pub mod reactor;
pub mod worker;

use crate::error::{Error, Operation};
use deadline::RequestScope;
use std::{task::Poll, time::Duration};

/// Retry listener submission without terminating its service on queue pressure.
/// Workers poll services every turn, with a reactor::wait fallback of at most
/// 10ms. Do not self-wake or submit a timer to the saturated ring. One attempt per
/// interval bounds work even when unrelated completions keep the worker busy.
/// Only Overloaded is retried; shutdown, deadlines and fatal I/O still propagate.
pub(crate) fn retry_listener<'a, T: 'a>(
    scope: &'a RequestScope,
    mut submit: impl FnMut() -> Operation<'a, T> + 'a,
) -> Operation<'a, T> {
    Box::pin(async move {
        let mut operation = None;
        let mut retry_at = environment::now();
        std::future::poll_fn(|cx| {
            if operation.is_none() {
                scope.check()?;
                if environment::now() < retry_at {
                    return Poll::Pending;
                }
                operation = Some(submit());
            }
            match operation.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Ready(Err(Error::Overloaded)) => {
                    // A failed submission published no SQE. For submitted work,
                    // the underlying future retains the existing CQE fence.
                    operation = None;
                    retry_at = environment::now() + Duration::from_millis(10);
                    Poll::Pending
                }
                result => result,
            }
        })
        .await
    })
}
