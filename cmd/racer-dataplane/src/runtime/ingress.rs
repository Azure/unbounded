//! Bounded pre-session socket handoff. No submitted operation crosses reactors.
use super::admission::{Admission, ConnectionAdmission, ConnectionReservation};
use crate::{
    error::{Error, Result},
    model::identity::{CacheId, WorkerId},
};
use std::{
    collections::VecDeque,
    os::fd::OwnedFd,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    task::Waker,
};

#[derive(Default)]
pub(crate) struct Retired(AtomicBool);
impl Retired {
    pub(crate) fn get(&self) -> bool {
        self.0.load(Ordering::Acquire)
    }
    pub(crate) fn set(&self, value: bool) {
        self.0.store(value, Ordering::Release);
    }
}
pub(crate) enum Kind {
    Client(CacheId, Arc<Retired>),
    Peer,
}
pub(crate) struct Accepted {
    pub fd: OwnedFd,
    pub reservation: ConnectionReservation,
    pub kind: Kind,
}
struct Target {
    admission: Option<ConnectionAdmission>,
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
    pub fn install(&self, worker: WorkerId, admission: &Admission) -> Result<()> {
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
        target.admission = Some(admission.connection_admission());
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
            if let Ok(reservation) = admission.reserve() {
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
            let Some((_, target)) = state.targets.iter_mut().find(|(id, _)| *id == worker) else {
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
    use crate::model::limits::ResourceClass;
    #[test]
    fn batch_pop_respects_budget_and_releases_unconsumed_entries() {
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
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
        let admissions: Vec<_> = (0..4).map(|_| Admission::new(limits.clone())).collect();
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
