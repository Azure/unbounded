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
}
impl Clone for AdmissionPolicy {
    fn clone(&self) -> Self {
        Self {
            limits: self.limits.clone(),
            observer: Mutex::new(self.observer()),
        }
    }
}
impl AdmissionPolicy {
    pub fn new(limits: Limits) -> Self {
        Self {
            limits,
            observer: Mutex::default(),
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
}

/// Translate exact synchronous operation facts into final Racer diagnostics.
/// Shared admission never observes this worker-local operation's sink.
pub(crate) fn capture_rejection<T>(
    admission: &Quotas<AdmissionPolicy>,
    operation: impl FnOnce() -> Result<T>,
) -> (Result<T>, Option<Detail>) {
    let (result, rejection) = admission.observe_rejections(operation);
    let detail = if matches!(result, Err(Error::Overloaded)) {
        rejection.map(rejection_detail)
    } else {
        None
    };
    (result, detail)
}

/// Keep resource names and telemetry mapping in application policy.
fn rejection_detail(rejection: Rejection<ResourceClass>) -> Detail {
    match rejection {
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
        let detail = rejection_detail(rejection);
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
        let reject = || {
            quotas
                .reserve(None, ResourceClass::Plaintext, usize::MAX)
                .map_err(Error::from)
        };
        let (_, detail) = capture_rejection(&quotas, reject);
        assert!(matches!(
            detail,
            Some(Detail::Resource {
                class: ResourceClass::Plaintext,
                requested: usize::MAX,
                ..
            })
        ));
        let (_, detail) = capture_rejection(&quotas, || {
            let (_, inner) = capture_rejection(&quotas, reject);
            assert!(inner.is_some());
            Err::<(), _>(Error::Overloaded)
        });
        assert!(
            detail.is_none(),
            "nested rejection cannot become outer facts"
        );
        let clone = Quotas::new(quotas.policy().clone());
        let (_, detail) = capture_rejection(&clone, || reject().map(|_| ()));
        assert!(detail.is_none());
        let panic = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _ = capture_rejection::<()>(&quotas, || {
                let _ = reject();
                panic!("fixture");
            });
        }));
        assert!(panic.is_err());
        let (_, detail) = capture_rejection(&quotas, || {
            let _ = reject();
            Ok(())
        });
        assert!(
            detail.is_none(),
            "recovered operation must not report final rejection"
        );
        let (_, detail) = capture_rejection(&quotas, || Err::<(), _>(Error::Cancelled));
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
        let (result, detail) = capture_rejection(&quotas, || {
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
        let (result, detail) = capture_rejection(&quotas, || {
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
    reserve_ingress_charges(admission).map_err(Into::into)
}
fn reserve_ingress_charges(
    admission: &SharedQuotas<AdmissionPolicy>,
) -> flow_control::Result<ConnectionReservation> {
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
    Retirement(crate::client::listener::RetirementAuthorization),
    Peer,
}
pub(crate) struct Accepted {
    pub fd: OwnedFd,
    pub reservation: ConnectionReservation,
    pub kind: Kind,
}
struct IngressAdmission(SharedQuotas<AdmissionPolicy>);
impl flow_control::HandoffAdmission for IngressAdmission {
    type Reservation = ConnectionReservation;
    fn register(&self, waker: &Waker) {
        self.0.register(waker);
    }
    fn reserve(&self) -> flow_control::Result<ConnectionReservation> {
        reserve_ingress_charges(&self.0)
    }
}
pub(crate) struct Ingress(Arc<flow_control::Handoff<WorkerId, IngressAdmission, (OwnedFd, Kind)>>);
pub(crate) struct Offer(flow_control::Offer<WorkerId, IngressAdmission, (OwnedFd, Kind)>);
impl Ingress {
    pub fn new(workers: &[WorkerId]) -> Self {
        Self(Arc::new(flow_control::Handoff::new(workers)))
    }
    pub fn install(&self, worker: WorkerId, admission: &Quotas<AdmissionPolicy>) -> Result<()> {
        self.0
            .install(&worker, IngressAdmission(admission.shared()))
            .map_err(Into::into)
    }
    pub fn reserve(self: &Arc<Self>, waker: &Waker) -> Result<Offer> {
        self.0.reserve(waker).map(Offer).map_err(Into::into)
    }
    pub fn pop_batch<const N: usize>(
        &self,
        worker: WorkerId,
        waker: &Waker,
        budget: usize,
    ) -> Result<[Option<Accepted>; N]> {
        self.0
            .pop_batch(&worker, waker, budget)
            .map(|items| {
                items.map(|item| {
                    item.map(|item| {
                        let ((fd, kind), reservation) = item.into_parts();
                        Accepted {
                            fd,
                            reservation,
                            kind,
                        }
                    })
                })
            })
            .map_err(Into::into)
    }
    pub fn close(&self, worker: WorkerId) {
        self.0.close(&worker);
    }
}
impl Offer {
    pub fn deliver(self, fd: OwnedFd, kind: Kind) -> Result<()> {
        self.0.deliver(|| (fd, kind)).map_err(Into::into)
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

    /// Keep Racer's payload backing class policy at the application boundary.
    #[test]
    fn backing_resource_class_mapping_is_ciphertext_only() {
        assert!(AdmissionPolicy::covers(ResourceClass::Ciphertext));
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::DirtyCiphertext,
            ResourceClass::RequestContext,
            ResourceClass::Connection,
            ResourceClass::ControlProgress,
        ] {
            assert!(!AdmissionPolicy::covers(class));
        }
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
