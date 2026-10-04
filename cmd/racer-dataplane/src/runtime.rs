//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

pub mod affinity;
// Seeded hashing only in simulated worlds; production retains std hashing.
#[cfg(not(test))]
pub(crate) type HashMap<K, V> = std::collections::HashMap<K, V>;
#[cfg(not(test))]
pub(crate) type HashSet<K> = std::collections::HashSet<K>;
#[cfg(test)]
pub(crate) type HashMap<K, V> = std::collections::HashMap<K, V, HashState>;
#[cfg(test)]
pub(crate) type HashSet<K> = std::collections::HashSet<K, HashState>;
#[cfg(test)]
#[derive(Clone, Debug)]
pub(crate) enum HashState {
    Real(std::collections::hash_map::RandomState),
    Simulated(u64),
}
#[cfg(test)]
impl Default for HashState {
    fn default() -> Self {
        match uring_runtime::environment::simulation_seed() {
            Some(seed) => Self::Simulated(seed),
            None => Self::Real(std::collections::hash_map::RandomState::new()),
        }
    }
}
#[cfg(test)]
impl std::hash::BuildHasher for HashState {
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
pub mod crypto;
pub mod deadline;
pub mod reactor;
pub mod worker;

/// Yield one cooperative turn without retaining an executor or I/O owner.
pub(crate) async fn cooperative_turn() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            std::task::Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    })
    .await
}
/// Racer TLS time adapter over the runtime's scoped wall clock.
pub fn unix_time() -> rustls::pki_types::UnixTime {
    rustls::pki_types::UnixTime::since_unix_epoch(
        uring_runtime::environment::wall_now()
            .duration_since(std::time::SystemTime::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

#[cfg(test)]
mod tests {
    use super::HashMap;
    use super::HashState;
    use crate::error::Error;
    use crate::model::RequestId;
    use crate::runtime::RequestScope;
    use std::time::Duration;
    use uring_runtime::environment::*;

    #[test]
    fn simulation_replays_collection_iteration_without_consuming_nonce_entropy() {
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
        let _real = uring_runtime::environment::Environment::default().enter();
        assert!(matches!(HashState::default(), HashState::Real(_)));
    }

    #[test]
    fn replay_and_nested_worlds_preserve_time_entropy_and_deadlines() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let _guard = role.enter();
        let start = now();
        let wall = wall_now();
        let scope = RequestScope::new(RequestId([1; 16]), start + Duration::from_secs(2)).unwrap();
        let wire = crate::peer::protocol::encode_deadline(scope.deadline).unwrap();
        let mut first = [0; 64];
        fill_random(&mut first[..3]).unwrap();
        fill_random(&mut first[3..]).unwrap();
        {
            let replay = SimulationClock::new(7);
            let _nested = replay.environment(11).enter();
            let mut bytes = [0; 64];
            fill_random(&mut bytes).unwrap();
            assert_eq!(bytes, first);
            assert_eq!(wall_now(), wall);
            replay.advance(Duration::from_secs(500));
        }
        assert_eq!(now(), start);
        clock.advance(Duration::from_secs(2));
        assert_eq!(scope.check(), Err(Error::DeadlineExceeded));
        clock.set_wall_time(wall - Duration::from_secs(60));
        assert_eq!(now(), start + Duration::from_secs(2));
        assert_eq!(
            crate::peer::protocol::encode_deadline(scope.deadline).unwrap(),
            wire
        );
        assert_eq!(
            crate::peer::protocol::decode_deadline(wire).unwrap().0,
            scope.deadline.0
        );
    }
    mod listener_tests {
        use crate::runtime::RequestScope;
        use crate::error::Error;
        use crate::model::RequestId;
        use crate::test_support::WakeCounter;
        use std::cell::Cell;
        use std::sync::Arc;
        use std::task::Context;
        use std::task::Poll;
        use std::task::Waker;
        use std::time::Duration;
        use uring_runtime::environment;
        use uring_runtime::environment::SimulationClock;
        use uring_runtime::retry_listener as retry;

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
                let mut operation = retry::<_, ()>(&scope, || {
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
                let mut operation =
                    retry::<_, ()>(&scope, || Box::pin(std::future::ready(Err(error))));
                assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
            }
        }
        #[test]
        fn submitted_listener_cancellation_waits_for_cqe_fence() {
            use crate::admission::AdmissionPolicy;
            use crate::runtime::Reactor;
            use std::rc::Rc;
            use uring_runtime::simulation::Simulation;
            let sim = Simulation::new();
            let _os = sim.enter();
            let clock = SimulationClock::new(3);
            let environment = clock.environment(0);
            let _time = environment.enter();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
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
}
