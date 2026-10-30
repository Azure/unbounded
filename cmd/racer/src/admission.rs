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
use std::sync::OnceLock;
use std::sync::atomic::AtomicBool;
use std::task::Waker;

pub use racer_core::admission::ResourceClass;
use racer_core::admission::Rules;

pub struct AdmissionPolicy {
    rules: Rules,

    observer: OnceLock<Observer>,

    /// Published cache priorities. Clones share one table.
    weights: crate::priority::Weights,

    /// Set while ublk devices may still send reads. Worker shutdown then leaves
    /// the quotas open and the owning application stops them later. Clones
    /// share one flag.
    stop_deferred: Arc<AtomicBool>,
}
impl Clone for AdmissionPolicy {
    fn clone(&self) -> Self {
        Self {
            rules: self.rules.clone(),
            observer: self.observer.clone(),
            weights: self.weights.clone(),
            stop_deferred: self.stop_deferred.clone(),
        }
    }
}
impl AdmissionPolicy {
    pub fn new(limits: Limits) -> Self {
        Self {
            rules: Rules::new(
                limits,
                racer_core::admission::Floors::for_page(
                    PAGE_BYTES as usize,
                    crate::store::MAX_HEADER_BYTES,
                ),
            ),
            observer: OnceLock::new(),
            weights: crate::priority::Weights::default(),
            stop_deferred: Arc::default(),
        }
    }
    /// Leave the quotas open when worker shutdown begins. The caller that sets
    /// this must call `Quotas::stop` itself once the deferral ends.
    pub fn defer_stop(&self, deferred: bool) {
        self.stop_deferred
            .store(deferred, std::sync::atomic::Ordering::Release);
    }
    /// Whether worker shutdown must leave the quotas open.
    pub fn stop_deferred(&self) -> bool {
        self.stop_deferred
            .load(std::sync::atomic::Ordering::Acquire)
    }
    /// Shared priority table. Call `Quotas::reweigh` after updating it.
    pub fn weights(&self) -> &crate::priority::Weights {
        &self.weights
    }
    pub fn limits(&self) -> &Limits {
        &self.rules.limits
    }
    pub fn set_observer(&self, observer: Observer) {
        assert!(
            self.observer.set(observer).is_ok(),
            "observer already installed"
        );
    }
    pub fn observer(&self) -> &Observer {
        static ABSENT: OnceLock<Observer> = OnceLock::new();
        self.observer
            .get()
            .unwrap_or_else(|| ABSENT.get_or_init(Observer::default))
    }
}

#[cfg(test)]
mod observer_tests {
    use super::*;

    #[test]
    fn observer_borrows_stable_storage_and_absent_read_does_not_install() {
        let policy = AdmissionPolicy::new(crate::test_support::cluster::config(false).limits);
        assert!(std::ptr::eq(policy.observer(), policy.observer()));
        assert!(policy.observer.get().is_none());
        policy.set_observer(Observer::default());
        assert!(std::ptr::eq(
            policy.observer(),
            policy.observer.get().unwrap()
        ));
        assert!(policy.clone().observer.get().is_some());
    }

    #[test]
    #[should_panic(expected = "observer already installed")]
    fn observer_rejects_reinstallation() {
        let policy = AdmissionPolicy::new(crate::test_support::cluster::config(false).limits);
        policy.set_observer(Observer::default());
        policy.set_observer(Observer::default());
    }

    #[test]
    fn memory_fair_limits_follow_live_priority_with_floor() {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.range_window_pages = std::num::NonZeroUsize::new(2).unwrap();
        let quotas = Quotas::new(AdmissionPolicy::new(limits));
        let class = ResourceClass::Plaintext;
        let limit = quotas.limit(class);
        let floor = quotas.policy().floor(class);
        let a = CacheId("a".into());
        let b = CacheId("b".into());
        let definition = |id: &CacheId, priority| racer_control_wire::CacheDefinition {
            id: id.clone(),
            name: id.0.clone(),
            client_socket: "/c".into(),
            origin_socket: "/o".into(),
            priority,
        };
        // Unknown caches use the default priority, so shares start equal.
        assert_eq!(quotas.policy().weight(&a), 500);
        let _ca = quotas.reserve(Some(&a), class, 1).unwrap();
        let _cb = quotas.reserve(Some(&b), class, 1).unwrap();
        assert_eq!(quotas.fair_limit(&a, class), quotas.fair_limit(&b, class));
        assert!(
            quotas
                .policy()
                .weights()
                .update(&[definition(&a, 900), definition(&b, 100)])
        );
        quotas.reweigh();
        assert_eq!(quotas.active_weight(), 1000);
        // Weights never cut below the request-progress allowance or the
        // equal split, whichever is smaller.
        let progress = quotas.policy().progress(class).min(limit / 2);
        assert!(progress > floor);
        assert_eq!(quotas.fair_limit(&a, class), (limit * 9 / 10).max(progress));
        assert_eq!(quotas.fair_limit(&b, class), (limit / 10).max(progress));
        // Priority 0 keeps only the request-progress allowance.
        quotas
            .policy()
            .weights()
            .update(&[definition(&a, 1000), definition(&b, 0)]);
        quotas.reweigh();
        assert_eq!(quotas.fair_limit(&b, class), progress);
        assert_eq!(quotas.fair_limit(&a, class), limit);
    }

    #[test]
    fn zero_priority_cache_can_reserve_request_context_beside_active_cache() {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.range_window_pages = std::num::NonZeroUsize::new(2).unwrap();
        let quotas = Quotas::new(AdmissionPolicy::new(limits));
        let a = CacheId("a".into());
        let z = CacheId("z".into());
        let definition = |id: &CacheId, priority| racer_control_wire::CacheDefinition {
            id: id.clone(),
            name: id.0.clone(),
            client_socket: "/c".into(),
            origin_socket: "/o".into(),
            priority,
        };
        quotas
            .policy()
            .weights()
            .update(&[definition(&a, 1000), definition(&z, 0)]);
        quotas.reweigh();
        let context = racer_object_wire::client::MAX_HEAD_BYTES;
        let _active = quotas
            .reserve(Some(&a), ResourceClass::RequestContext, context)
            .unwrap();
        let _page = quotas
            .reserve(Some(&a), ResourceClass::Plaintext, PAGE_BYTES as usize)
            .unwrap();
        // The listener reserves a full head of context before reading a request.
        let _zero = quotas
            .reserve(Some(&z), ResourceClass::RequestContext, context)
            .unwrap();
        let limit = quotas.limit(ResourceClass::RequestContext);
        assert_eq!(
            quotas.fair_limit(&z, ResourceClass::RequestContext),
            quotas.fair_limit(&a, ResourceClass::RequestContext)
        );
        assert_eq!(
            quotas.fair_limit(&z, ResourceClass::RequestContext),
            (limit / 2).max(1)
        );
        // Priority-0 page memory keeps its request-progress allowance.
        let _zero_page = quotas
            .reserve(Some(&z), ResourceClass::Plaintext, PAGE_BYTES as usize)
            .unwrap();
        let window = quotas.policy().limits().range_window_pages.get();
        assert_eq!(
            quotas.fair_limit(&z, ResourceClass::Plaintext),
            (window + 1) * PAGE_BYTES as usize
        );
        // The allowance covers a full range window beyond the held page, and no more.
        let _ahead = quotas
            .reserve(
                Some(&z),
                ResourceClass::Plaintext,
                window * PAGE_BYTES as usize,
            )
            .unwrap();
        assert!(
            quotas
                .reserve(Some(&z), ResourceClass::Plaintext, 1)
                .is_err()
        );
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
        self.rules.limit(class)
    }
    /// A cache's keyed fair share follows its published priority.
    fn weight(&self, key: &CacheId) -> u64 {
        self.weights.weight(key)
    }
    fn weighted(class: ResourceClass) -> bool {
        Rules::weighted(class)
    }
    fn floor(&self, class: ResourceClass) -> usize {
        self.rules.floor(class)
    }
    fn progress(&self, class: ResourceClass) -> usize {
        self.rules.progress(class)
    }
    fn max_keys(&self) -> usize {
        self.rules.max_keys()
    }
    fn wakes(class: ResourceClass) -> bool {
        Rules::wakes(class)
    }
    fn allows_stopped(class: ResourceClass) -> bool {
        Rules::allows_stopped(class)
    }
    fn covers(class: ResourceClass) -> bool {
        Rules::covers(class)
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
    ordinary: Option<Charge<AdmissionPolicy>>,
}
impl ConnectionReservation {
    /// Move an outbound reservation between metadata and ordinary use.
    /// Ordinary use must fit below the metadata reserve; metadata use releases it.
    pub fn reclassify(
        &mut self,
        admission: &Quotas<AdmissionPolicy>,
        metadata: bool,
    ) -> Result<()> {
        if metadata {
            self.ordinary = None;
        } else if self.ordinary.is_none() {
            self.ordinary =
                Some(admission.reserve(None, ResourceClass::OrdinaryOutboundConnection, 1)?);
        }
        Ok(())
    }
}

/// Reserve an outbound socket for origin metadata, which may use the reserve
/// that ordinary outbound reservations leave free.
pub fn reserve_metadata_connection(
    admission: &Quotas<AdmissionPolicy>,
) -> Result<ConnectionReservation> {
    let role_charge = admission.reserve(None, ResourceClass::OutboundConnection, 1)?;
    let total = admission.reserve(None, ResourceClass::Connection, 1)?;
    Ok(ConnectionReservation {
        _total: total,
        _role: role_charge,
        ordinary: None,
    })
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
    let ordinary = match role {
        ResourceClass::OutboundConnection => {
            Some(admission.reserve(None, ResourceClass::OrdinaryOutboundConnection, 1)?)
        }
        _ => None,
    };
    let role_charge = admission.reserve(None, role, 1)?;
    let total = admission.reserve(None, ResourceClass::Connection, 1)?;
    Ok(ConnectionReservation {
        _total: total,
        _role: role_charge,
        ordinary,
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
        ordinary: None,
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
    fn ciphertext_floor_covers_one_rounded_maximum_disk_extent() {
        use flow_control::Policy;
        let policy = AdmissionPolicy::new(crate::test_support::cluster::config(false).limits);
        assert_eq!(policy.floor(ResourceClass::Ciphertext), 16_842_752);
        for alignment in [512usize, 4096] {
            let logical = PAGE_BYTES as usize + 16 + crate::store::MAX_HEADER_BYTES;
            let extent = logical.div_ceil(alignment) * alignment;
            assert!(
                page_alloc::buffer_capacity(extent).unwrap()
                    <= policy.floor(ResourceClass::Ciphertext)
            );
        }
        assert_eq!(policy.floor(ResourceClass::Plaintext), PAGE_BYTES as usize);
    }

    #[test]
    fn request_progress_follows_range_window_and_never_exceeds_equal_split() {
        use flow_control::Policy;
        let mut limits = crate::test_support::cluster::config(false).limits;
        let record = PAGE_BYTES as usize + 16 + crate::store::MAX_HEADER_BYTES;
        for window in [1usize, 2, 4] {
            limits.range_window_pages = std::num::NonZeroUsize::new(window).unwrap();
            let policy = AdmissionPolicy::new(limits.clone());
            assert_eq!(
                policy.progress(ResourceClass::Plaintext),
                (window + 1) * PAGE_BYTES as usize
            );
            assert_eq!(
                policy.progress(ResourceClass::Ciphertext),
                window * (PAGE_BYTES as usize + 16) + record
            );
            // Floors and unweighted classes are unchanged.
            assert_eq!(policy.floor(ResourceClass::Plaintext), PAGE_BYTES as usize);
            assert_eq!(policy.progress(ResourceClass::DirtyCiphertext), 0);
            assert_eq!(policy.progress(ResourceClass::RequestContext), 0);
        }
        // With a small budget the allowance is clipped to the equal split, so
        // a priority-0 cache cannot take more than an equal-priority peer.
        let definition = |id: &CacheId, priority| racer_control_wire::CacheDefinition {
            id: id.clone(),
            name: id.0.clone(),
            client_socket: "/c".into(),
            origin_socket: "/o".into(),
            priority,
        };
        limits.range_window_pages = std::num::NonZeroUsize::new(2).unwrap();
        limits.plaintext_bytes = std::num::NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
        let quotas = Quotas::new(AdmissionPolicy::new(limits));
        let a = CacheId("a".into());
        let z = CacheId("z".into());
        quotas
            .policy()
            .weights()
            .update(&[definition(&a, 1000), definition(&z, 0)]);
        quotas.reweigh();
        let _active = quotas
            .reserve(Some(&a), ResourceClass::Plaintext, 1)
            .unwrap();
        let _held = quotas
            .reserve(Some(&z), ResourceClass::Plaintext, 2 * PAGE_BYTES as usize)
            .unwrap();
        assert_eq!(
            quotas.fair_limit(&z, ResourceClass::Plaintext),
            2 * PAGE_BYTES as usize
        );
        assert!(
            quotas
                .reserve(Some(&z), ResourceClass::Plaintext, 1)
                .is_err()
        );
    }

    #[test]
    fn only_ciphertext_opts_out_of_secure_wiping() {
        use flow_control::Class;
        assert!(!ResourceClass::Ciphertext.requires_wipe());
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::DirtyCiphertext,
            ResourceClass::Registered,
            ResourceClass::RequestContext,
            ResourceClass::Flight,
            ResourceClass::Waiter,
            ResourceClass::Connection,
            ResourceClass::Pipe,
            ResourceClass::ControlProgress,
            ResourceClass::Relay,
            ResourceClass::IngressConnection,
            ResourceClass::OutboundConnection,
            ResourceClass::ControlConnection,
        ] {
            assert!(class.requires_wipe());
        }
    }

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
    fn ordinary_outbound_leaves_metadata_reserve_and_reclassification_enforces_it() {
        for (connections, ordinary, total) in [(64, 15, 16), (8, 1, 2), (4, 1, 1)] {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.client_connections = std::num::NonZeroUsize::new(connections).unwrap();
            let admission = Quotas::new(AdmissionPolicy::new(limits));
            assert_eq!(admission.limit(ResourceClass::OutboundConnection), total);
            assert_eq!(
                admission.limit(ResourceClass::OrdinaryOutboundConnection),
                ordinary
            );
            let pages: Vec<_> = (0..ordinary)
                .map(|_| reserve_connection(&admission, ResourceClass::OutboundConnection).unwrap())
                .collect();
            assert!(reserve_connection(&admission, ResourceClass::OutboundConnection).is_err());
            let metadata: Vec<_> = (ordinary..total)
                .map(|_| reserve_metadata_connection(&admission).unwrap())
                .collect();
            assert!(reserve_metadata_connection(&admission).is_err());
            assert_eq!(admission.used(ResourceClass::OutboundConnection), total);
            drop(metadata);
            // Reused metadata sockets cannot carry ordinary work past the reserve.
            if ordinary < total {
                let mut slot = reserve_metadata_connection(&admission).unwrap();
                assert!(slot.reclassify(&admission, false).is_err());
                drop(pages);
                slot.reclassify(&admission, false).unwrap();
                assert_eq!(admission.used(ResourceClass::OrdinaryOutboundConnection), 1);
                slot.reclassify(&admission, true).unwrap();
                assert_eq!(admission.used(ResourceClass::OrdinaryOutboundConnection), 0);
            } else {
                drop(pages);
            }
            assert_eq!(admission.used(ResourceClass::Connection), 0);
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
        let mut outbound: Vec<_> = (0..admission.limit(ResourceClass::OrdinaryOutboundConnection))
            .map(|_| reserve_connection(&admission, ResourceClass::OutboundConnection).unwrap())
            .collect();
        assert!(reserve_connection(&admission, ResourceClass::OutboundConnection).is_err());
        outbound.push(reserve_metadata_connection(&admission).unwrap());
        assert!(reserve_metadata_connection(&admission).is_err());
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

    /// Owner contracts for node-wide and cache-keyed admission classes.
    mod capacity {
        use super::*;
        use racer_control_wire::CacheId;
        use std::num::NonZeroUsize;

        const PLAIN: usize = PAGE_BYTES as usize;
        const CIPHER: usize = PLAIN + 16;

        /// 16 pages, 16 dirty pages, 6 pipes, 64 queue entries.
        fn global_capacity() -> flow_control::Quotas<AdmissionPolicy> {
            const CONTEXT: usize = 256;
            let nz = |n: usize| NonZeroUsize::new(n).unwrap();
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.plaintext_bytes = nz(16 * PLAIN);
            limits.ciphertext_bytes = nz(16 * CIPHER);
            limits.dirty_bytes = nz(16 * CIPHER);
            limits.pipes = nz(6);
            limits.queue_entries = nz(64);
            limits.flights = nz(16);
            limits.waiters_per_flight = nz(64);
            limits.client_connections = nz(64 + 6);
            limits.request_context_bytes = nz((64 + 6) * CONTEXT);
            limits.metadata_entries = nz(64);
            flow_control::Quotas::new(AdmissionPolicy::new(limits))
        }

        /// Pipe and connection leases are node-wide (no cache key, as `PipePool`
        /// and `HttpPool` charge them): one live cache may use more than half
        /// while another cache holds request contexts, until the global limit.
        #[test]
        fn transport_capacity_is_global_across_live_caches() {
            for class in [ResourceClass::Pipe, ResourceClass::Connection] {
                let admission = global_capacity();
                let caches = [CacheId("0".into()), CacheId("1".into())];
                let contexts: Vec<_> = caches
                    .iter()
                    .map(|cache| {
                        admission
                            .reserve(Some(cache), ResourceClass::RequestContext, 256)
                            .unwrap()
                    })
                    .collect();
                let limit = admission.limit(class);
                let leases: Vec<_> = (0..limit)
                    .map(|slot| {
                        admission
                            .reserve(None, class, 1)
                            .unwrap_or_else(|_| panic!("{class:?} rejected at {slot}/{limit}"))
                    })
                    .collect();
                assert!(leases.iter().all(|lease| lease.key().is_none()));
                assert_eq!(admission.used(class), limit);
                assert!(admission.reserve(None, class, 1).is_err());
                let mut leases = leases;
                drop(leases.pop());
                let reused = admission.reserve(None, class, 1).unwrap();
                assert_eq!(admission.used(class), limit);
                drop((leases, reused, contexts));
                assert!(
                    ResourceClass::ALL
                        .iter()
                        .all(|class| admission.used(*class) == 0)
                );
            }
        }

        /// Cache-keyed classes keep per-cache fair share even with global room: a
        /// cache holding half the limit is rejected once another cache is live.
        #[test]
        fn cache_scoped_resources_enforce_fairness_with_global_room() {
            for (class, amount) in [
                (ResourceClass::Plaintext, PLAIN),
                (ResourceClass::Ciphertext, CIPHER),
                (ResourceClass::DirtyCiphertext, CIPHER),
                (ResourceClass::Registered, CIPHER),
                (ResourceClass::RequestContext, 256),
                (ResourceClass::Flight, 1),
                (ResourceClass::Waiter, 1),
            ] {
                let admission = global_capacity();
                let (a, b) = (CacheId("0".into()), CacheId("1".into()));
                let limit = admission.limit(class);
                let first = admission.reserve(Some(&a), class, limit / 2).unwrap();
                let second = admission.reserve(Some(&b), class, amount).unwrap();
                assert_eq!(first.key(), Some(&a));
                assert_eq!(second.key(), Some(&b));
                assert!(admission.used(class) + amount <= limit);
                assert!(
                    admission.reserve(Some(&a), class, amount).is_err(),
                    "{class:?}"
                );
                drop((first, second));
                assert_eq!(admission.used(class), 0);
            }
        }
    }
}

#[cfg(test)]
mod rules_equivalence_tests {
    use super::*;
    use std::num::NonZeroUsize;

    /// The policy as it read before the rules moved to racer-core, kept
    /// verbatim so the delegation is checked against it.
    fn legacy_limit(limits: &Limits, class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => limits.plaintext_bytes.get(),
            ResourceClass::Ciphertext => limits.ciphertext_bytes.get(),
            ResourceClass::DirtyCiphertext => limits.dirty_bytes.get(),
            ResourceClass::Registered => limits.registered_bytes.get(),
            ResourceClass::RequestContext => limits.request_context_bytes.get(),
            ResourceClass::Flight => limits.flights.get(),
            ResourceClass::Waiter => limits
                .flights
                .get()
                .saturating_mul(limits.waiters_per_flight.get()),
            ResourceClass::Connection => limits.client_connections.get(),
            ResourceClass::ControlConnection => (limits.client_connections.get() / 4).min(3),
            ResourceClass::OutboundConnection => limits.client_connections.get() / 4,
            ResourceClass::OrdinaryOutboundConnection => {
                let outbound = legacy_limit(limits, ResourceClass::OutboundConnection);
                outbound
                    - usize::from(outbound >= 2) * racer_core::admission::METADATA_OUTBOUND_RESERVE
            }
            ResourceClass::IngressConnection => limits.client_connections.get().saturating_sub(
                legacy_limit(limits, ResourceClass::ControlConnection)
                    + legacy_limit(limits, ResourceClass::OutboundConnection),
            ),
            ResourceClass::Pipe => limits.pipes.get(),
            ResourceClass::ControlProgress => limits.queue_entries.get(),
            ResourceClass::Relay => limits.relay_transfers.get(),
        }
    }

    fn legacy_floor(class: ResourceClass) -> usize {
        match class {
            ResourceClass::Plaintext => PAGE_BYTES as usize,
            ResourceClass::Ciphertext => page_alloc::buffer_capacity(
                PAGE_BYTES as usize + 16 + crate::store::MAX_HEADER_BYTES + 4096,
            )
            .expect("bounded page capacity"),
            ResourceClass::DirtyCiphertext | ResourceClass::Registered => PAGE_BYTES as usize + 16,
            _ => 1,
        }
    }

    /// Cache priority's per-cache allowance as it read before it moved to
    /// racer-core.
    fn legacy_progress(limits: &Limits, class: ResourceClass) -> usize {
        let window = limits.range_window_pages.get();
        let record = PAGE_BYTES as usize + 16 + crate::store::MAX_HEADER_BYTES;
        match class {
            ResourceClass::Plaintext => {
                window.saturating_add(1).saturating_mul(PAGE_BYTES as usize)
            }
            ResourceClass::Ciphertext => window
                .saturating_mul(PAGE_BYTES as usize + 16)
                .saturating_add(record),
            _ => 0,
        }
    }

    fn check(limits: Limits) {
        let policy = AdmissionPolicy::new(limits.clone());
        for class in ResourceClass::ALL {
            assert_eq!(
                policy.progress(class),
                legacy_progress(&limits, class),
                "{class:?}"
            );
            assert_eq!(
                AdmissionPolicy::weighted(class),
                matches!(
                    class,
                    ResourceClass::Plaintext
                        | ResourceClass::Ciphertext
                        | ResourceClass::DirtyCiphertext
                )
            );
            assert_eq!(
                policy.limit(class),
                legacy_limit(&limits, class),
                "{class:?}"
            );
            assert_eq!(policy.floor(class), legacy_floor(class), "{class:?}");
            assert_eq!(
                AdmissionPolicy::wakes(class),
                matches!(
                    class,
                    ResourceClass::Connection | ResourceClass::IngressConnection
                )
            );
            assert_eq!(
                AdmissionPolicy::allows_stopped(class),
                matches!(class, ResourceClass::ControlProgress)
            );
            assert_eq!(
                AdmissionPolicy::covers(class),
                matches!(class, ResourceClass::Ciphertext)
            );
        }
        assert_eq!(policy.max_keys(), limits.metadata_entries.get());
    }

    #[test]
    fn moved_rules_match_legacy_policy() {
        let config = crate::test_support::cluster::config(false);
        check(config.limits.clone());
        // Unset environment yields exactly the racer-core per-worker defaults.
        let parsed = crate::config::Config::from_lookup(|_| Ok(None)).unwrap();
        assert_eq!(parsed.limits, Limits::default());
        check(parsed.limits.clone());
        let mut scaled = parsed;
        scaled.scale_limits(7).unwrap();
        check(scaled.limits);
        let one = NonZeroUsize::MIN;
        for connections in [1, 2, 3, 4, 7, 8, 11, 12, 13, 16, 65_536] {
            check(Limits {
                client_connections: NonZeroUsize::new(connections).unwrap(),
                ..Limits::default()
            });
        }
        check(Limits {
            flights: NonZeroUsize::MAX,
            waiters_per_flight: NonZeroUsize::MAX,
            metadata_entries: one,
            relay_transfers: one,
            queue_entries: one,
            ..Limits::default()
        });
    }
}
