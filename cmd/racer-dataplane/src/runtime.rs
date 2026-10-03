//! Explicit execution ownership. No library may introduce an unbudgeted thread pool.

pub mod admission;
pub mod affinity;
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
            match uring_runtime::environment::simulation_seed() {
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
        use uring_runtime::environment::{SimulationClock, fill_random};
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
        assert!(matches!(State::default(), State::Real(_)));
    }
}
pub mod crypto;
pub mod deadline;
/// Racer TLS time adapter over the runtime's scoped wall clock.
pub fn unix_time() -> rustls::pki_types::UnixTime {
    rustls::pki_types::UnixTime::since_unix_epoch(
        uring_runtime::environment::wall_now()
            .duration_since(std::time::SystemTime::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

#[cfg(test)]
mod environment_tests {
    use crate::{error::Error, model::RequestId, runtime::deadline::RequestScope};
    use std::time::Duration;
    use uring_runtime::environment::*;

    #[test]
    fn replay_and_nested_worlds_preserve_time_entropy_and_deadlines() {
        let clock = SimulationClock::new(7);
        let role = clock.environment(11);
        let _guard = role.enter();
        let start = now();
        let wall = wall_now();
        let scope = RequestScope::new(RequestId([1; 16]), start + Duration::from_secs(2)).unwrap();
        let wire = crate::security::protocol::encode_deadline(scope.deadline).unwrap();
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
            crate::security::protocol::encode_deadline(scope.deadline).unwrap(),
            wire
        );
        assert_eq!(
            crate::security::protocol::decode_deadline(wire).unwrap().0,
            scope.deadline.0
        );
    }
}
pub(crate) mod ingress {
    //! Bounded pre-session socket handoff. No submitted operation crosses reactors.
    use super::admission::{AdmissionPolicy, ConnectionReservation, SharedAdmissionExt};
    use crate::{
        error::{Error, Result},
        model::{CacheId, WorkerId},
    };
    use std::{
        collections::VecDeque,
        os::fd::OwnedFd,
        sync::{Arc, Mutex, atomic::AtomicBool},
        task::Waker,
    };

    pub(crate) enum Kind {
        Client(CacheId, Arc<AtomicBool>),
        Peer,
    }
    pub(crate) struct Accepted {
        pub fd: OwnedFd,
        pub reservation: ConnectionReservation,
        pub kind: Kind,
    }
    struct Target {
        admission: Option<flow_control::SharedQuotas<AdmissionPolicy>>,
        queue: VecDeque<Accepted>,
        waker: Option<Waker>,
        closed: bool,
    }
    struct State {
        targets: Vec<(WorkerId, Target)>,
        cursor: usize,
    }
    pub(crate) struct Ingress(Mutex<State>);
    pub(crate) struct Offer {
        ingress: Arc<Ingress>,
        target: usize,
        reservation: ConnectionReservation,
    }
    impl Ingress {
        pub fn new(workers: &[WorkerId]) -> Self {
            Self(Mutex::new(State {
                targets: workers
                    .iter()
                    .map(|id| {
                        (
                            *id,
                            Target {
                                admission: None,
                                queue: VecDeque::new(),
                                waker: None,
                                closed: false,
                            },
                        )
                    })
                    .collect(),
                cursor: 0,
            }))
        }
        pub fn install(
            &self,
            worker: WorkerId,
            admission: &flow_control::Quotas<AdmissionPolicy>,
        ) -> Result<()> {
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            let target = &mut state
                .targets
                .iter_mut()
                .find(|(id, _)| *id == worker)
                .ok_or(Error::InvalidConfiguration)?
                .1;
            if target.admission.is_some() || target.closed {
                return Err(Error::InvalidConfiguration);
            }
            target.admission = Some(admission.shared());
            Ok(())
        }
        pub fn reserve(self: &Arc<Self>, waker: &Waker) -> Result<Offer> {
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            for offset in 0..state.targets.len() {
                let index = (state.cursor + offset) % state.targets.len();
                let (_, target) = &state.targets[index];
                if target.closed {
                    continue;
                }
                let Some(admission) = &target.admission else {
                    continue;
                };
                admission.register(waker);
                if let Ok(reservation) = admission.reserve_ingress() {
                    state.cursor = (index + 1) % state.targets.len();
                    return Ok(Offer {
                        ingress: self.clone(),
                        target: index,
                        reservation,
                    });
                }
            }
            Err(Error::Overloaded)
        }
        pub fn pop_batch<const N: usize>(
            &self,
            worker: WorkerId,
            waker: &Waker,
            budget: usize,
        ) -> Result<[Option<Accepted>; N]> {
            let mut state = self.0.lock().map_err(|_| Error::Unavailable)?;
            let target = &mut state
                .targets
                .iter_mut()
                .find(|(id, _)| *id == worker)
                .ok_or(Error::InvalidConfiguration)?
                .1;
            if let Some(old) = &mut target.waker {
                old.clone_from(waker);
            } else {
                target.waker = Some(waker.clone());
            }
            Ok(std::array::from_fn(|index| {
                if index < budget {
                    target.queue.pop_front()
                } else {
                    None
                }
            }))
        }
        pub fn close(&self, worker: WorkerId) {
            let queued = {
                let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
                let Some((_, target)) = state.targets.iter_mut().find(|(id, _)| *id == worker)
                else {
                    return;
                };
                target.closed = true;
                std::mem::take(&mut target.queue)
            };
            drop(queued);
        }
    }
    impl Offer {
        pub fn deliver(self, fd: OwnedFd, kind: Kind) -> Result<()> {
            let mut state = self.ingress.0.lock().map_err(|_| Error::Unavailable)?;
            // The target vector is immutable after construction.
            let target = &mut state.targets[self.target].1;
            if target.closed {
                return Err(Error::Unavailable);
            }
            // Every queue element holds one target reservation; queue length therefore
            // cannot exceed its ingress ceiling, even while the worker is stalled.
            target.queue.push_back(Accepted {
                fd,
                reservation: self.reservation,
                kind,
            });
            let waker = target.waker.clone();
            drop(state);
            if let Some(waker) = waker {
                waker.wake();
            }
            Ok(())
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::model::ResourceClass;
        use crate::runtime::admission::AdmissionExt;
        #[test]
        fn offer_delivered_after_target_close_releases_socket_and_charges() {
            use std::io::Read;
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let ingress = Arc::new(Ingress::new(&[WorkerId(7)]));
            ingress.install(WorkerId(7), &admission).unwrap();
            let waker = futures::task::noop_waker();
            let offer = ingress.reserve(&waker).unwrap();
            ingress.close(WorkerId(7));
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            let (fd, mut peer) = std::os::unix::net::UnixStream::pair().unwrap();
            peer.set_nonblocking(true).unwrap();
            assert!(matches!(
                offer.deliver(fd.into(), Kind::Peer),
                Err(Error::Unavailable)
            ));
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert_eq!(admission.used(ResourceClass::IngressConnection), 0);
            assert_eq!(peer.read(&mut [0]).unwrap(), 0);
            assert!(matches!(ingress.reserve(&waker), Err(Error::Overloaded)));
        }
        #[test]
        fn batch_pop_respects_budget_and_releases_unconsumed_entries() {
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let ingress = Arc::new(Ingress::new(&[WorkerId(7)]));
            ingress.install(WorkerId(7), &admission).unwrap();
            let waker = futures::task::noop_waker();
            let mut peers = Vec::new();
            for _ in 0..3 {
                let (fd, peer) = std::os::unix::net::UnixStream::pair().unwrap();
                peers.push(peer);
                ingress
                    .reserve(&waker)
                    .unwrap()
                    .deliver(fd.into(), Kind::Peer)
                    .unwrap();
            }
            assert!(
                ingress
                    .pop_batch::<4>(WorkerId(7), &waker, 0)
                    .unwrap()
                    .iter()
                    .all(Option::is_none)
            );
            let batch = ingress.pop_batch::<4>(WorkerId(7), &waker, 2).unwrap();
            assert_eq!(batch.iter().filter(|item| item.is_some()).count(), 2);
            assert_eq!(admission.used(ResourceClass::Connection), 3);
            drop(batch);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            ingress.close(WorkerId(7));
            assert_eq!(admission.used(ResourceClass::Connection), 0);
            assert!(matches!(
                ingress.pop_batch::<4>(WorkerId(8), &waker, 4),
                Err(Error::InvalidConfiguration)
            ));
        }
        #[test]
        fn target_charge_follows_queued_socket_and_closed_offer_rolls_back() {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.client_connections = std::num::NonZeroUsize::new(32).unwrap();
            let admissions: Vec<_> = (0..4)
                .map(|_| flow_control::Quotas::new(AdmissionPolicy::new(limits.clone())))
                .collect();
            let ingress = Arc::new(Ingress::new(&[
                WorkerId(0),
                WorkerId(1),
                WorkerId(2),
                WorkerId(3),
            ]));
            for (index, admission) in admissions.iter().enumerate() {
                ingress.install(WorkerId(index as u16), admission).unwrap();
            }
            let waker = futures::task::noop_waker();
            let per_worker = admissions[0].limit(ResourceClass::IngressConnection);
            let offers: Vec<_> = (0..per_worker * admissions.len())
                .map(|_| ingress.reserve(&waker).unwrap())
                .collect();
            assert!(matches!(ingress.reserve(&waker), Err(Error::Overloaded)));
            for admission in &admissions {
                assert_eq!(admission.used(ResourceClass::IngressConnection), per_worker);
                assert!(
                    admission
                        .reserve_connection(ResourceClass::OutboundConnection)
                        .is_ok()
                );
                assert!(
                    admission
                        .reserve_connection(ResourceClass::ControlConnection)
                        .is_ok()
                );
            }
            let mut peers = Vec::new();
            for offer in offers {
                let (fd, peer) = std::os::unix::net::UnixStream::pair().unwrap();
                peers.push(peer);
                offer.deliver(fd.into(), Kind::Peer).unwrap();
            }
            let [accepted] = ingress.pop_batch::<1>(WorkerId(0), &waker, 1).unwrap();
            let accepted = accepted.unwrap();
            ingress.close(WorkerId(0));
            assert_eq!(admissions[0].used(ResourceClass::Connection), 1);
            drop(accepted);
            assert_eq!(admissions[0].used(ResourceClass::Connection), 0);
            for worker in 1..4 {
                ingress.close(WorkerId(worker));
            }
            assert!(
                admissions
                    .iter()
                    .all(|a| a.used(ResourceClass::Connection) == 0)
            );
            drop(peers);
        }
    }
}
#[cfg(test)]
mod listener_tests {
    use super::{deadline::RequestScope, retry_listener as retry};
    use crate::{error::Error, model::RequestId, test_support::WakeCounter};
    use std::{
        cell::Cell,
        sync::Arc,
        task::{Context, Poll, Waker},
        time::Duration,
    };
    use uring_runtime::environment::{self, SimulationClock};

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
        use crate::runtime::{admission::AdmissionPolicy, reactor::Reactor};
        use std::rc::Rc;
        use uring_runtime::reactor::simulation::Simulation;
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
