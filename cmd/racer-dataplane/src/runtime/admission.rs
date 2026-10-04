//! Racer resource policy and compound admission operations.
use crate::error::Error;
use crate::error::Result;
use crate::model::CacheId;
use crate::model::Limits;
use crate::model::PAGE_BYTES;
use crate::model::ResourceClass;
use crate::telemetry::Detail;
use crate::telemetry::Failure;
use crate::telemetry::Observer;
use crate::telemetry::Stage;
use flow_control::Charge;
use flow_control::Policy;
use flow_control::Quotas;
use flow_control::Rejection;
use flow_control::SharedQuotas;
use std::sync::Mutex;

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
    fn observer(&self) -> Observer {
        self.observer
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
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
        self.observer()
            .record(Failure::new(Stage::Admission, Error::Overloaded).detail(detail));
    }
}

pub struct FillReservation {
    pub plaintext: Charge<AdmissionPolicy>,
    pub ciphertext: Charge<AdmissionPolicy>,
    pub dirty: Option<Charge<AdmissionPolicy>>,
}
/// Compound socket role admission, retained through kernel completion.
pub struct ConnectionReservation {
    _total: Charge<AdmissionPolicy>,
    _role: Charge<AdmissionPolicy>,
}

/// Racer operations on the generic local authority. No quota facade or alias.
pub trait AdmissionExt {
    fn limits(&self) -> &Limits;
    fn observer(&self) -> Observer;
    fn set_observer(&self, observer: Observer);
    fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation>;
    fn reserve_fill(&self, cache: &CacheId, persist: bool) -> Result<FillReservation>;
}
impl AdmissionExt for Quotas<AdmissionPolicy> {
    fn limits(&self) -> &Limits {
        &self.policy().limits
    }
    fn observer(&self) -> Observer {
        self.policy().observer()
    }
    fn set_observer(&self, observer: Observer) {
        *self
            .policy()
            .observer
            .lock()
            .unwrap_or_else(|e| e.into_inner()) = observer;
    }
    fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation> {
        if matches!(role, ResourceClass::IngressConnection) {
            return self.shared().reserve_ingress();
        }
        if !matches!(
            role,
            ResourceClass::OutboundConnection | ResourceClass::ControlConnection
        ) {
            return Err(Error::InvalidConfiguration);
        }
        let role_charge = self.reserve(None, role, 1)?;
        let total = self.reserve(None, ResourceClass::Connection, 1)?;
        Ok(ConnectionReservation {
            _total: total,
            _role: role_charge,
        })
    }
    fn reserve_fill(&self, cache: &CacheId, persist: bool) -> Result<FillReservation> {
        let plaintext = self.reserve(Some(cache), ResourceClass::Plaintext, PAGE_BYTES as usize)?;
        let ciphertext = self.reserve(
            Some(cache),
            ResourceClass::Ciphertext,
            PAGE_BYTES as usize + 16,
        )?;
        let dirty = if persist {
            Some(self.reserve(
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
}

/// Compound ingress admission and Racer-specific usage views on the direct
/// generic shared handle. No cache authority or payload pool crosses workers.
pub trait SharedAdmissionExt {
    fn reserve_ingress(&self) -> Result<ConnectionReservation>;
    fn relay(&self) -> (usize, usize);
    fn ciphertext(&self) -> (usize, usize);
}
impl SharedAdmissionExt for SharedQuotas<AdmissionPolicy> {
    fn reserve_ingress(&self) -> Result<ConnectionReservation> {
        let role = self.reserve(ResourceClass::IngressConnection, 1)?;
        let total = self.reserve(ResourceClass::Connection, 1)?;
        Ok(ConnectionReservation {
            _total: total,
            _role: role,
        })
    }
    fn relay(&self) -> (usize, usize) {
        (
            self.used(ResourceClass::Relay),
            self.limit(ResourceClass::Relay),
        )
    }
    fn ciphertext(&self) -> (usize, usize) {
        (
            self.used(ResourceClass::Ciphertext),
            self.limit(ResourceClass::Ciphertext),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
        let connection = std::thread::spawn(move || ingress.reserve_ingress().unwrap())
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
        assert_eq!(usage.ciphertext().0, 1 << 20);
        drop(admission);
        assert_eq!(usage.ciphertext().0, 0);
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
            .map(|_| {
                admission
                    .reserve_connection(ResourceClass::IngressConnection)
                    .unwrap()
            })
            .collect();
        assert!(
            admission
                .reserve_connection(ResourceClass::IngressConnection)
                .is_err()
        );
        let outbound: Vec<_> = (0..admission.limit(ResourceClass::OutboundConnection))
            .map(|_| {
                admission
                    .reserve_connection(ResourceClass::OutboundConnection)
                    .unwrap()
            })
            .collect();
        assert!(
            admission
                .reserve_connection(ResourceClass::OutboundConnection)
                .is_err()
        );
        let control: Vec<_> = (0..admission.limit(ResourceClass::ControlConnection))
            .map(|_| {
                admission
                    .reserve_connection(ResourceClass::ControlConnection)
                    .unwrap()
            })
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
            admission.reserve_fill(&CacheId("c".into()), false),
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
