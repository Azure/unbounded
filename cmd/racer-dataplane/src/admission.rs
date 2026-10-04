//! Racer resource policy and compound admission operations.

use crate::config::Limits;
use crate::error::Error;
use crate::error::Result;
use crate::model::PAGE_BYTES;
use crate::model::WorkerId;
use crate::telemetry::Detail;
use crate::telemetry::Failure;
use crate::telemetry::Observer;
use crate::telemetry::Stage;
use flow_control::Charge;
use flow_control::Policy;
use flow_control::Quotas;
use flow_control::Rejection;
use flow_control::SharedQuotas;
use racer_control_wire::CacheId;
use std::collections::VecDeque;
use std::os::fd::OwnedFd;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::AtomicBool;
use std::task::Waker;

#[derive(Clone, Copy, Debug)]
pub enum ResourceClass {
    Plaintext,
    Ciphertext,
    DirtyCiphertext,
    Registered,
    RequestContext,
    Flight,
    Waiter,
    Connection,
    Pipe,
    ControlProgress,
    Relay,
    IngressConnection,
    OutboundConnection,
    ControlConnection,
}
impl flow_control::Class for ResourceClass {
    const COUNT: usize = 14;
    fn index(self) -> usize {
        self as usize
    }
}

pub struct AdmissionPolicy {
    limits: Limits,
    observer: Mutex<Observer>,
    capture: Mutex<Option<RejectionCapture>>,
}
struct RejectionCapture {
    thread: std::thread::ThreadId,
    detail: Option<Detail>,
}
struct CaptureGuard<'a> {
    policy: &'a AdmissionPolicy,
    previous: Option<RejectionCapture>,
    _local: std::marker::PhantomData<std::rc::Rc<()>>,
}
impl Drop for CaptureGuard<'_> {
    fn drop(&mut self) {
        *self
            .policy
            .capture
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = self.previous.take();
    }
}
impl Clone for AdmissionPolicy {
    fn clone(&self) -> Self {
        Self {
            limits: self.limits.clone(),
            observer: Mutex::new(self.observer()),
            capture: Mutex::default(),
        }
    }
}
impl AdmissionPolicy {
    pub fn new(limits: Limits) -> Self {
        Self {
            limits,
            observer: Mutex::default(),
            capture: Mutex::default(),
        }
    }
    pub fn limits(&self) -> &Limits {
        &self.limits
    }
    pub fn set_observer(&self, observer: Observer) {
        *self.observer.lock().unwrap_or_else(|e| e.into_inner()) = observer;
    }
    pub fn observer(&self) -> Observer {
        self.observer
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Capture this synchronous call only. No guard or lock escapes to an async
    /// caller; nested calls restore the previous sink, including during unwind.
    pub(crate) fn capture_rejection<T>(
        &self,
        operation: impl FnOnce() -> Result<T>,
    ) -> (Result<T>, Option<Detail>) {
        let thread = std::thread::current().id();
        let mut capture = self.capture.lock().unwrap_or_else(|e| e.into_inner());
        assert!(
            capture.as_ref().is_none_or(|c| c.thread == thread),
            "capture is worker-local"
        );
        let previous = capture.replace(RejectionCapture {
            thread,
            detail: None,
        });
        drop(capture);
        let guard = CaptureGuard {
            policy: self,
            previous,
            _local: std::marker::PhantomData,
        };
        let result = operation();
        let detail = if matches!(result, Err(Error::Overloaded)) {
            self.capture
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .as_ref()
                .and_then(|c| c.detail)
        } else {
            None
        };
        drop(guard);
        (result, detail)
    }
}
impl Policy for AdmissionPolicy {
    type Class = ResourceClass;
    type Key = CacheId;
    fn limit(&self, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => self.limits.plaintext_bytes.get(),
            ResourceClass::Ciphertext => self.limits.ciphertext_bytes.get(),
            ResourceClass::DirtyCiphertext => self.limits.dirty_bytes.get(),
            ResourceClass::Registered => self.limits.registered_bytes.get(),
            ResourceClass::RequestContext => self.limits.request_context_bytes.get(),
            ResourceClass::Flight => self.limits.flights.get(),
            ResourceClass::Waiter => self
                .limits
                .flights
                .get()
                .saturating_mul(self.limits.waiters_per_flight.get()),
            ResourceClass::Connection => self.limits.client_connections.get(),
            // Snapshot, renewal, and keyring delivery make independent progress.
            ResourceClass::ControlConnection => (self.limits.client_connections.get() / 4).min(3),
            ResourceClass::OutboundConnection => self.limits.client_connections.get() / 4,
            ResourceClass::IngressConnection => {
                self.limits.client_connections.get().saturating_sub(
                    self.limit(ResourceClass::ControlConnection)
                        + self.limit(ResourceClass::OutboundConnection),
                )
            }
            ResourceClass::Pipe => self.limits.pipes.get(),
            ResourceClass::ControlProgress => self.limits.queue_entries.get(),
            ResourceClass::Relay => self.limits.relay_transfers.get(),
        }
    }
    fn floor(&self, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => PAGE_BYTES as usize,
            // Disk reads own padded staging and decoded ciphertext together.
            ResourceClass::Ciphertext => {
                2 * (PAGE_BYTES as usize + 16) + crate::store::MAX_HEADER_BYTES + 4096
            }
            ResourceClass::DirtyCiphertext | ResourceClass::Registered => PAGE_BYTES as usize + 16,
            _ => 1,
        }
    }
    fn max_keys(&self) -> usize {
        self.limits.metadata_entries.get()
    }
    fn wakes(class: ResourceClass) -> bool {
        matches!(
            class,
            ResourceClass::Connection | ResourceClass::IngressConnection
        )
    }
    fn allows_stopped(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::ControlProgress)
    }
    fn covers(class: ResourceClass) -> bool {
        matches!(class, ResourceClass::Ciphertext)
    }
    fn rejected(&self, rejection: Rejection<ResourceClass>) {
        let detail = match rejection {
            Rejection::Keys { used, limit } => Detail::CacheEntries { used, limit },
            Rejection::Resource {
                class,
                used,
                limit,
                requested,
                key_used,
                key_limit,
            } => Detail::Resource {
                class,
                used,
                limit,
                requested,
                cache_used: key_used,
                cache_limit: key_limit,
            },
        };
        {
            let mut capture = self.capture.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(capture) = capture.as_mut()
                && capture.thread == std::thread::current().id()
            {
                capture.detail = Some(detail);
            }
        }
        self.observer()
            .record(Failure::new(Stage::Admission, Error::Overloaded).detail(detail));
    }
}

#[cfg(test)]
pub struct FillReservation {
    pub plaintext: Charge<AdmissionPolicy>,
    pub ciphertext: Charge<AdmissionPolicy>,
    pub dirty: Option<Charge<AdmissionPolicy>>,
}

#[cfg(test)]
mod capture_tests {
    use super::*;
    #[test]
    fn same_call_capture_restores_nested_panic_and_clone_state() {
        let quotas = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let policy = quotas.policy();
        let reject = || {
            quotas
                .reserve(None, ResourceClass::Plaintext, usize::MAX)
                .map_err(Error::from)
        };
        let (_, detail) = policy.capture_rejection(reject);
        assert!(matches!(
            detail,
            Some(Detail::Resource {
                class: ResourceClass::Plaintext,
                requested: usize::MAX,
                ..
            })
        ));
        let (_, detail) = policy.capture_rejection(|| {
            let (_, inner) = policy.capture_rejection(reject);
            assert!(inner.is_some());
            Err::<(), _>(Error::Overloaded)
        });
        assert!(
            detail.is_none(),
            "nested rejection cannot become outer facts"
        );
        let clone = policy.clone();
        let (_, detail) = clone.capture_rejection(|| reject().map(|_| ()));
        assert!(detail.is_none());
        let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _ = policy.capture_rejection::<()>(|| {
                let _ = reject();
                panic!("fixture");
            });
        }));
        assert!(panic.is_err());
        assert!(policy.capture.lock().unwrap().is_none());
        let (_, detail) = policy.capture_rejection(|| {
            let _ = reject();
            Ok(())
        });
        assert!(
            detail.is_none(),
            "recovered operation must not report final rejection"
        );
        let (_, detail) = policy.capture_rejection(|| Err::<(), _>(Error::Cancelled));
        assert!(detail.is_none());
    }

    #[test]
    fn capture_keeps_fair_share_and_key_table_facts() {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.plaintext_bytes = std::num::NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
        let quotas = Quotas::new(AdmissionPolicy::new(limits.clone()));
        let a = CacheId("a".into());
        let b = CacheId("b".into());
        let _a = quotas
            .reserve(Some(&a), ResourceClass::Plaintext, 2 * PAGE_BYTES as usize)
            .unwrap();
        let _b = quotas
            .reserve(Some(&b), ResourceClass::RequestContext, 1)
            .unwrap();
        let (result, detail) = quotas.policy().capture_rejection(|| {
            quotas
                .reserve(Some(&a), ResourceClass::Plaintext, 1)
                .map_err(Error::from)
        });
        assert!(matches!(result, Err(Error::Overloaded)));
        assert!(
            matches!(detail, Some(Detail::Resource { requested: 1, cache_used: Some(used), cache_limit: Some(limit), .. }) if used == 2 * PAGE_BYTES as usize && limit == used)
        );
        limits.metadata_entries = std::num::NonZeroUsize::new(1).unwrap();
        let quotas = Quotas::new(AdmissionPolicy::new(limits));
        let _a = quotas
            .reserve(Some(&a), ResourceClass::RequestContext, 1)
            .unwrap();
        let (result, detail) = quotas.policy().capture_rejection(|| {
            quotas
                .reserve(Some(&b), ResourceClass::Plaintext, 1)
                .map_err(Error::from)
        });
        assert!(matches!(result, Err(Error::Overloaded)));
        assert!(matches!(
            detail,
            Some(Detail::CacheEntries { used: 1, limit: 1 })
        ));
    }
}
/// Compound socket role admission, retained through kernel completion.
pub struct ConnectionReservation {
    _total: Charge<AdmissionPolicy>,
    _role: Charge<AdmissionPolicy>,
}

/// Reserve the socket role and total together, rolling back on either rejection.
pub fn reserve_connection(
    admission: &Quotas<AdmissionPolicy>,
    role: ResourceClass,
) -> Result<ConnectionReservation> {
    if matches!(role, ResourceClass::IngressConnection) {
        return reserve_ingress(&admission.shared());
    }
    if !matches!(
        role,
        ResourceClass::OutboundConnection | ResourceClass::ControlConnection
    ) {
        return Err(Error::InvalidConfiguration);
    }
    let role_charge = admission.reserve(None, role, 1)?;
    let total = admission.reserve(None, ResourceClass::Connection, 1)?;
    Ok(ConnectionReservation {
        _total: total,
        _role: role_charge,
    })
}

#[cfg(test)]
pub fn reserve_fill(
    admission: &Quotas<AdmissionPolicy>,
    cache: &CacheId,
    persist: bool,
) -> Result<FillReservation> {
    let plaintext =
        admission.reserve(Some(cache), ResourceClass::Plaintext, PAGE_BYTES as usize)?;
    let ciphertext = admission.reserve(
        Some(cache),
        ResourceClass::Ciphertext,
        PAGE_BYTES as usize + 16,
    )?;
    let dirty = if persist {
        Some(admission.reserve(
            Some(cache),
            ResourceClass::DirtyCiphertext,
            PAGE_BYTES as usize + 16,
        )?)
    } else {
        None
    };
    Ok(FillReservation {
        plaintext,
        ciphertext,
        dirty,
    })
}

/// Shared ingress admission never transfers cache authority or payload pools.
pub fn reserve_ingress(admission: &SharedQuotas<AdmissionPolicy>) -> Result<ConnectionReservation> {
    let role = admission.reserve(ResourceClass::IngressConnection, 1)?;
    let total = admission.reserve(ResourceClass::Connection, 1)?;
    Ok(ConnectionReservation {
        _total: total,
        _role: role,
    })
}

/// Bounded pre-session socket handoff. No submitted operation crosses reactors.
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
    admission: Option<SharedQuotas<AdmissionPolicy>>,
    queue: VecDeque<Accepted>,
    waker: Option<Waker>,
    closed: bool,
}
struct IngressState {
    targets: Vec<(WorkerId, Target)>,
    cursor: usize,
}
pub(crate) struct Ingress(Mutex<IngressState>);
pub(crate) struct Offer {
    ingress: Arc<Ingress>,
    target: usize,
    reservation: ConnectionReservation,
}
impl Ingress {
    pub fn new(workers: &[WorkerId]) -> Self {
        Self(Mutex::new(IngressState {
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
    pub fn install(&self, worker: WorkerId, admission: &Quotas<AdmissionPolicy>) -> Result<()> {
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
            if let Ok(reservation) = reserve_ingress(admission) {
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

    #[test]
    fn offer_delivered_after_target_close_releases_socket_and_charges() {
        use std::io::Read;
        let admission = Quotas::new(AdmissionPolicy::new(
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
        let admission = Quotas::new(AdmissionPolicy::new(
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
            .map(|_| Quotas::new(AdmissionPolicy::new(limits.clone())))
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
            assert!(reserve_connection(admission, ResourceClass::OutboundConnection).is_ok());
            assert!(reserve_connection(admission, ResourceClass::ControlConnection).is_ok());
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

    #[test]
    fn shared_policy_handles_are_send_safe_and_do_not_retain_payloads() {
        fn send_sync<T: Send + Sync>() {}
        send_sync::<AdmissionPolicy>();
        send_sync::<Charge<AdmissionPolicy>>();
        send_sync::<SharedQuotas<AdmissionPolicy>>();
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let usage = admission.shared();
        let ingress = admission.shared();
        let connection = std::thread::spawn(move || reserve_ingress(&ingress).unwrap())
            .join()
            .unwrap();
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(admission.used(ResourceClass::IngressConnection), 1);
        drop(connection);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        let mut charge = admission
            .reserve(None, ResourceClass::Ciphertext, 1 << 20)
            .unwrap();
        charge.recycle(vec![0xa7; 1 << 20]);
        drop(charge);
        assert_eq!(usage.used(ResourceClass::Ciphertext), 1 << 20);
        drop(admission);
        assert_eq!(usage.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn recycled_truncated_capacity_is_zero_before_cross_cache_and_class_reuse() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let first = CacheId("first".into());
        let second = CacheId("second".into());
        let capacity = 1024 * 1024;
        for length in [0, 1, capacity - 16, capacity] {
            let mut reservation = admission
                .reserve(Some(&first), ResourceClass::Ciphertext, capacity)
                .unwrap();
            let mut bytes = reservation.buffer(capacity).unwrap();
            bytes.fill(0xa7);
            bytes.truncate(length);
            let pointer = bytes.as_ptr();
            reservation.recycle(bytes);
            drop(reservation);
            assert_eq!(admission.used(ResourceClass::Ciphertext), capacity);
            let mut reservation = admission
                .reserve(Some(&second), ResourceClass::Plaintext, capacity)
                .unwrap();
            let bytes = reservation.buffer(capacity).unwrap();
            assert_eq!(bytes.len(), capacity);
            assert_eq!(bytes.as_ptr(), pointer);
            assert!(bytes.iter().all(|byte| *byte == 0));
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            assert_eq!(admission.used(ResourceClass::Plaintext), capacity);
            reservation.recycle(bytes);
            drop(reservation);
            admission.reclaim_buffers();
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        }
    }

    #[test]
    fn recycled_spare_capacity_is_initialized_and_checkout_is_exact_and_admitted() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let mut bytes = Vec::with_capacity(1 << 20);
        bytes.push(0xa7);
        let capacity = bytes.capacity();
        let pointer = bytes.as_ptr();
        let mut reservation = admission
            .reserve(None, ResourceClass::Ciphertext, capacity)
            .unwrap();
        reservation.recycle(bytes);
        drop(reservation);
        let reservation = admission
            .reserve(None, ResourceClass::Plaintext, capacity)
            .unwrap();
        assert!(matches!(
            reservation.buffer(capacity + 1),
            Err(flow_control::Error::InvalidInput)
        ));
        let different = reservation.buffer(capacity - 1).unwrap();
        assert_ne!(different.as_ptr(), pointer);
        assert_eq!(different.len(), capacity - 1);
        assert!(different.iter().all(|byte| *byte == 0));
        drop(different);
        assert_eq!(admission.retained_buffer_bytes(), capacity);
        let bytes = reservation.buffer(capacity).unwrap();
        assert_eq!(bytes.as_ptr(), pointer);
        assert_eq!(bytes.len(), capacity);
        assert!(bytes.iter().all(|byte| *byte == 0));
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), capacity);
        drop((bytes, reservation));
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn recycled_pool_keeps_two_slots_and_pressure_releases_idle_charges() {
        let capacity = 1 << 20;
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.ciphertext_bytes = std::num::NonZeroUsize::new(3 * capacity).unwrap();
        let admission = Quotas::new(AdmissionPolicy::new(limits));
        let buffers: Vec<_> = (0..3)
            .map(|_| {
                let reservation = admission
                    .reserve(None, ResourceClass::Ciphertext, capacity)
                    .unwrap();
                let mut bytes = reservation.buffer(capacity).unwrap();
                bytes.fill(0xa7);
                (bytes, reservation)
            })
            .collect();
        for (bytes, mut reservation) in buffers {
            reservation.recycle(bytes);
        }
        assert_eq!(admission.retained_buffer_bytes(), 2 * capacity);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 2 * capacity);
        let reservation = admission
            .reserve(None, ResourceClass::Ciphertext, 3 * capacity)
            .unwrap();
        assert_eq!(admission.retained_buffer_bytes(), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 3 * capacity);
        drop(reservation);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn unrelated_quota_failure_preserves_recycled_payload() {
        for completing in [false, true] {
            for class in [ResourceClass::Plaintext, ResourceClass::Ciphertext] {
                let admission = Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ));
                let length = PAGE_BYTES as usize
                    + usize::from(matches!(class, ResourceClass::Ciphertext)) * 16;
                let mut old = admission.reserve(None, class, length).unwrap();
                let mut bytes = old.buffer(length).unwrap();
                bytes.fill(0xa7);
                let pointer = bytes.as_ptr();
                old.recycle(bytes);
                drop(old);
                let limit = admission.limit(ResourceClass::RequestContext);
                let held = admission
                    .reserve(None, ResourceClass::RequestContext, limit)
                    .unwrap();
                let result = if completing {
                    admission.reserve_completion(None, ResourceClass::RequestContext, 1)
                } else {
                    admission.reserve(None, ResourceClass::RequestContext, 1)
                };
                assert!(matches!(result, Err(flow_control::Error::Overloaded)));
                assert_eq!(admission.used(ResourceClass::RequestContext), limit);
                assert_eq!(admission.retained_buffer_bytes(), length);
                assert_eq!(admission.used(class), length);
                drop(held);
                let next = admission.reserve(None, class, length).unwrap();
                let bytes = next.buffer(length).unwrap();
                assert_eq!(bytes.as_ptr(), pointer);
                assert!(bytes.iter().all(|byte| *byte == 0));
                assert_eq!(admission.retained_buffer_bytes(), 0);
                assert_eq!(admission.used(class), length);
                drop((bytes, next));
                assert_eq!(admission.used(class), 0);
            }
        }
    }

    #[test]
    fn relevant_quota_failure_still_reclaims_recycled_payload() {
        for completing in [false, true] {
            for cache_records in [false, true] {
                let length = 1 << 20;
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.ciphertext_bytes = std::num::NonZeroUsize::new(length).unwrap();
                limits.metadata_entries = std::num::NonZeroUsize::new(1).unwrap();
                let admission = Quotas::new(AdmissionPolicy::new(limits));
                let first = CacheId("first".into());
                let second = CacheId("second".into());
                let mut old = admission
                    .reserve(Some(&first), ResourceClass::Ciphertext, length)
                    .unwrap();
                let bytes = old.buffer(length).unwrap();
                old.recycle(bytes);
                drop(old);
                assert_eq!(admission.retained_buffer_bytes(), length);
                let (cache, class, amount) = if cache_records {
                    (Some(&second), ResourceClass::RequestContext, 1)
                } else {
                    (None, ResourceClass::Ciphertext, length)
                };
                let reservation = if completing {
                    admission.reserve_completion(cache, class, amount)
                } else {
                    admission.reserve(cache, class, amount)
                }
                .unwrap();
                assert_eq!(admission.retained_buffer_bytes(), 0);
                assert_eq!(admission.used(class), amount);
                if cache_records {
                    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                }
                drop(reservation);
                assert_eq!(admission.used(class), 0);
            }
        }
    }

    #[test]
    #[ignore = "opt-in same-workload payload recycle benchmark"]
    fn payload_recycle_benchmark() {
        use std::hint::black_box;
        use std::time::Instant;
        const ITERATIONS: usize = 128;
        for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
            for retain in [true, false] {
                let admission = Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ));
                // Keep geometry, allocation, writes, admission, and destructor work
                // identical. Stop forces the non-pooling path.
                if !retain {
                    admission.stop();
                }
                for sample in 0..6 {
                    let start = Instant::now();
                    for _ in 0..ITERATIONS {
                        let mut reservation = admission
                            .reserve_completion(None, ResourceClass::Ciphertext, length)
                            .unwrap();
                        let mut bytes = reservation.buffer(length).unwrap();
                        bytes.fill(black_box(0xa7));
                        black_box(&bytes);
                        reservation.recycle(bytes);
                        drop(reservation);
                    }
                    let elapsed = start.elapsed();
                    if sample != 0 {
                        println!(
                            "payload_recycle length={length} retain={retain} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                            elapsed.as_nanos() as f64 / ITERATIONS as f64
                        );
                    }
                }
                admission.reclaim_buffers();
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            }
        }
    }

    #[test]
    fn recycled_payload_capacity_stays_admitted_zeroed_and_reclaimable() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let cache = CacheId("pool".into());
        let mut reservation = admission
            .reserve(Some(&cache), ResourceClass::Plaintext, 1024 * 1024)
            .unwrap();
        let mut bytes = reservation.buffer(1024 * 1024).unwrap();
        let pointer = bytes.as_ptr();
        bytes.fill(87);
        reservation.recycle(bytes);
        drop(reservation);
        assert_eq!(admission.used(ResourceClass::Plaintext), 1024 * 1024);
        let reservation = admission
            .reserve(Some(&cache), ResourceClass::Plaintext, 1024 * 1024)
            .unwrap();
        let bytes = reservation.buffer(1024 * 1024).unwrap();
        assert_eq!(bytes.as_ptr(), pointer);
        assert!(bytes.iter().all(|b| *b == 0));
        assert_eq!(admission.used(ResourceClass::Plaintext), 1024 * 1024);
        drop((bytes, reservation));
        assert_eq!(admission.retained_buffer_bytes(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn ingress_saturation_preserves_outbound_and_control_without_raising_total() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let ingress: Vec<_> = (0..admission.limit(ResourceClass::IngressConnection))
            .map(|_| reserve_connection(&admission, ResourceClass::IngressConnection).unwrap())
            .collect();
        assert!(reserve_connection(&admission, ResourceClass::IngressConnection).is_err());
        let outbound: Vec<_> = (0..admission.limit(ResourceClass::OutboundConnection))
            .map(|_| reserve_connection(&admission, ResourceClass::OutboundConnection).unwrap())
            .collect();
        assert!(reserve_connection(&admission, ResourceClass::OutboundConnection).is_err());
        let control: Vec<_> = (0..admission.limit(ResourceClass::ControlConnection))
            .map(|_| reserve_connection(&admission, ResourceClass::ControlConnection).unwrap())
            .collect();
        assert_eq!(
            control.len(),
            3,
            "snapshot, enrollment, and key delivery slots"
        );
        assert_eq!(
            admission.used(ResourceClass::Connection),
            admission.limit(ResourceClass::Connection)
        );
        drop((ingress, outbound, control));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }

    #[test]
    fn split_and_shrink_preserve_live_ownership_and_reject_growth() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let cache = CacheId("cache".into());
        let mut bundle = admission
            .reserve(Some(&cache), ResourceClass::Ciphertext, 100)
            .unwrap();
        assert!(bundle.split(100).is_err());
        assert!(bundle.shrink(101).is_err());
        assert!(bundle.shrink(0).is_err());
        let staging = bundle.split(60).unwrap();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 100);
        bundle.shrink(19).unwrap();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 79);
        std::thread::spawn(move || drop(staging)).join().unwrap();
        assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
        drop(bundle);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn rollback_and_cross_thread_release() {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.ciphertext_bytes = std::num::NonZeroUsize::new(1).unwrap();
        let admission = Quotas::new(AdmissionPolicy::new(limits));
        assert!(matches!(
            reserve_fill(&admission, &CacheId("c".into()), false),
            Err(Error::Overloaded)
        ));
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        let reservation = admission
            .reserve(None, ResourceClass::Plaintext, 7)
            .unwrap();
        assert!(admission.owns(&reservation));
        assert!(reservation.validate(ResourceClass::Ciphertext, 7).is_err());
        std::thread::spawn(move || drop(reservation))
            .join()
            .unwrap();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn stop_preserves_completion_progress_and_rejects_overflow() {
        let admission = Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        assert!(matches!(
            admission.reserve(None, ResourceClass::Plaintext, usize::MAX),
            Err(flow_control::Error::Overloaded)
        ));
        admission.stop();
        assert!(matches!(
            admission.reserve(None, ResourceClass::Plaintext, 1),
            Err(flow_control::Error::Unavailable)
        ));
        assert!(
            admission
                .reserve(None, ResourceClass::ControlProgress, 1)
                .is_ok()
        );
        let completion = admission
            .reserve_completion(None, ResourceClass::RequestContext, 1)
            .unwrap();
        assert_eq!(completion.amount(), 1);
        assert!(
            admission
                .reserve_completion(None, ResourceClass::RequestContext, usize::MAX)
                .is_err()
        );
    }

    #[test]
    fn active_caches_share_admission_and_released_counters_are_reclaimed() {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.request_context_bytes = std::num::NonZeroUsize::new(100).unwrap();
        let admission = Quotas::new(AdmissionPolicy::new(limits));
        let a = CacheId("a".into());
        let b = CacheId("b".into());
        let first = admission
            .reserve(Some(&a), ResourceClass::RequestContext, 40)
            .unwrap();
        let second = admission
            .reserve(Some(&b), ResourceClass::RequestContext, 40)
            .unwrap();
        assert!(matches!(
            admission.reserve(Some(&a), ResourceClass::RequestContext, 11),
            Err(flow_control::Error::Overloaded)
        ));
        drop(second);
        let third = admission
            .reserve(Some(&a), ResourceClass::RequestContext, 60)
            .unwrap();
        assert_eq!(admission.used(ResourceClass::RequestContext), 100);
        drop((first, third));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }
}
