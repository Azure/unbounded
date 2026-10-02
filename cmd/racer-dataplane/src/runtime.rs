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
mod listener_tests {
    use super::{deadline::RequestScope, environment, retry_listener as retry};
    use crate::{
        error::Error, model::RequestId, runtime::environment::SimulationClock,
        test_support::WakeCounter,
    };
    use std::{
        cell::Cell,
        sync::Arc,
        task::{Context, Poll, Waker},
        time::Duration,
    };

    #[test]
    fn repeated_pressure_is_rate_limited_without_self_wakes() {
        let clock = SimulationClock::new(1);
        let environment = clock.environment(0);
        let _time = environment.enter();
        let scope = RequestScope::new(
            RequestId([0; 16]),
            environment::now() + Duration::from_secs(1),
        )
        .unwrap();
        let calls = Cell::new(0);
        let wakes = Arc::new(WakeCounter::default());
        let waker = Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        let mut operation = retry(&scope, || {
            calls.set(calls.get() + 1);
            Box::pin(std::future::ready(if calls.get() <= 4 {
                Err(Error::Overloaded)
            } else {
                Ok(7)
            }))
        });
        for attempt in 1..=4 {
            for _ in 0..100 {
                assert!(operation.as_mut().poll(&mut cx).is_pending());
            }
            assert_eq!(calls.get(), attempt);
            clock.advance(Duration::from_millis(9));
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(calls.get(), attempt);
            clock.advance(Duration::from_millis(1));
        }
        assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(7)));
        assert_eq!(wakes.count(), 0);
    }
    #[test]
    fn backoff_preserves_shutdown_deadlines_and_fatal_errors() {
        let clock = SimulationClock::new(2);
        let environment = clock.environment(0);
        let _time = environment.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for expected in [Error::Cancelled, Error::DeadlineExceeded] {
            let scope = RequestScope::new(
                RequestId([0; 16]),
                environment::now() + Duration::from_millis(1),
            )
            .unwrap();
            let mut operation = retry::<()>(&scope, || {
                Box::pin(std::future::ready(Err(Error::Overloaded)))
            });
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            if expected == Error::Cancelled {
                scope.cancel().unwrap();
            } else {
                clock.advance(Duration::from_millis(1));
            }
            assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(expected)));
        }
        let scope = RequestScope::new(
            RequestId([0; 16]),
            environment::now() + Duration::from_secs(1),
        )
        .unwrap();
        for error in [
            Error::Io,
            Error::Unavailable,
            Error::InvalidConfiguration,
            Error::Internal,
        ] {
            let mut operation = retry::<()>(&scope, || Box::pin(std::future::ready(Err(error))));
            assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
        }
    }
    #[test]
    fn submitted_listener_cancellation_waits_for_cqe_fence() {
        use crate::runtime::{
            admission::Admission,
            reactor::{Reactor, simulation::Simulation},
        };
        use std::rc::Rc;
        let sim = Simulation::new();
        let _os = sim.enter();
        let clock = SimulationClock::new(3);
        let environment = clock.environment(0);
        let _time = environment.enter();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Reactor::new(admission);
        let scope = RequestScope::new(
            RequestId([0; 16]),
            environment::now() + Duration::from_secs(1),
        )
        .unwrap();
        let (reader, _writer) = sim.socket_pair();
        let reader = Rc::new(reader);
        let mut operation = retry(&scope, || {
            reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope)
        });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        scope.cancel().unwrap();
        // Cancellation is not a kernel ownership fence.
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        for _ in 0..4 {
            reactor.poll_budgeted(8).unwrap();
        }
        assert_eq!(
            operation.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        assert_eq!(reactor.in_flight(), 0);
    }
}
pub mod reactor;
pub mod worker;

use crate::error::Operation;
use deadline::RequestScope;

/// Retry listener submission without terminating its service on queue pressure.
/// Workers poll services every turn, with a reactor::wait fallback of at most
/// 10ms. Do not self-wake or submit a timer to the saturated ring. One attempt per
/// interval bounds work even when unrelated completions keep the worker busy.
/// Only Overloaded is retried; shutdown, deadlines and fatal I/O still propagate.
pub(crate) fn retry_listener<'a, T: 'a>(
    scope: &'a RequestScope,
    submit: impl FnMut() -> Operation<'a, T> + 'a,
) -> Operation<'a, T> {
    uring_runtime::retry_listener(scope, submit)
}
