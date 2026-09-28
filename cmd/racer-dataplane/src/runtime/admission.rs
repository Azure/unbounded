//! I/O-local admission authority with completion-safe cross-thread quota release.
use crate::runtime::collections::HashMap;
use crate::{
    error::{Error, Result},
    model::{
        identity::CacheId,
        limits::{Limits, ResourceClass},
        range::PAGE_BYTES,
    },
};
use std::{
    cell::RefCell,
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

const CLASSES: usize = 14;
#[repr(align(64))]
struct CacheLineCounter(AtomicUsize);
impl std::ops::Deref for CacheLineCounter {
    type Target = AtomicUsize;
    fn deref(&self) -> &AtomicUsize {
        &self.0
    }
}
fn index(class: ResourceClass) -> usize {
    class as usize
}
struct Counters {
    active: Option<Arc<AtomicUsize>>,
    retired: Option<(CacheId, Arc<Mutex<std::collections::VecDeque<CacheId>>>)>,
    used: [CacheLineCounter; CLASSES],
    wake: futures::task::AtomicWaker,
}
impl Counters {
    fn new() -> Self {
        Self {
            active: None,
            retired: None,
            used: std::array::from_fn(|_| CacheLineCounter(AtomicUsize::new(0))),
            wake: futures::task::AtomicWaker::new(),
        }
    }
}
impl Drop for Counters {
    fn drop(&mut self) {
        if let Some(active) = &self.active {
            active.fetch_sub(1, Ordering::AcqRel);
        }
        if let Some((cache, queue)) = &self.retired {
            queue
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .push_back(cache.clone());
        }
    }
}

pub struct Admission {
    observer: RefCell<crate::telemetry::failures::Observer>,
    limits: Limits,
    totals: Arc<Counters>,
    caches: RefCell<HashMap<CacheId, Weak<Counters>>>,
    active_caches: Arc<AtomicUsize>,
    retired_caches: Arc<Mutex<std::collections::VecDeque<CacheId>>>,
    stopped: Arc<AtomicBool>,
    buffers: Arc<Mutex<Vec<(Vec<u8>, Reservation)>>>,
}

/// Ownership of a charge, released only when its last containing allocation dies.
pub struct Reservation {
    class: ResourceClass,
    amount: usize,
    cache: Option<CacheId>,
    totals: Arc<Counters>,
    local: Option<Arc<Counters>>,
    buffers: Weak<Mutex<Vec<(Vec<u8>, Reservation)>>>,
    stopped: Arc<AtomicBool>,
}
impl Reservation {
    /// Return only the final exclusive payload owner. The retained reservation
    /// accounts for idle capacity; at most two buffers survive per worker.
    pub(crate) fn recycle(&mut self, mut bytes: Vec<u8>) {
        use zeroize::Zeroize;
        // Vec::zeroize wipes initialized elements, then the entire capacity.
        // u8 has no destructor: clear the length first to avoid wiping the live
        // prefix twice. The full capacity (including truncated tails) is still
        // securely zeroized before either pooling or deallocation.
        bytes.clear();
        bytes.zeroize();
        if self.stopped.load(Ordering::Acquire) {
            return;
        }
        if bytes.capacity() < 1024 * 1024 || bytes.capacity() > self.amount {
            return;
        }
        let Some(pool) = self.buffers.upgrade() else {
            return;
        };
        let Ok(mut pool) = pool.try_lock() else {
            return;
        };
        if self.stopped.load(Ordering::Acquire) {
            return;
        }
        if pool.len() >= 2 {
            return;
        }
        let reservation = Reservation {
            class: self.class,
            amount: std::mem::take(&mut self.amount),
            cache: self.cache.clone(),
            totals: self.totals.clone(),
            local: self.local.clone(),
            buffers: Weak::new(),
            stopped: self.stopped.clone(),
        };
        pool.push((bytes, reservation));
    }
    pub(crate) fn buffer(&self, length: usize) -> Result<Vec<u8>> {
        if length > self.amount {
            return Err(Error::InvalidConfiguration);
        }
        if let Some(pool) = self.buffers.upgrade() {
            let mut pool = pool.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(index) = pool
                .iter()
                .position(|(bytes, _)| bytes.capacity() == length)
            {
                let (mut bytes, old) = pool.swap_remove(index);
                drop(old);
                bytes.resize(length, 0);
                return Ok(bytes);
            }
        }
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(length)
            .map_err(|_| Error::Overloaded)?;
        bytes.resize(length, 0);
        Ok(bytes)
    }
    /// Release unused capacity only while the allocation owner is exclusive.
    /// Callers must retain at least the capacity of every live backing allocation.
    pub fn shrink(&mut self, amount: usize) -> Result<()> {
        if amount == 0 || amount > self.amount {
            return Err(Error::InvalidConfiguration);
        }
        let released = self.amount - amount;
        self.amount = amount;
        self.totals.used[index(self.class)].fetch_sub(released, Ordering::AcqRel);
        if let Some(local) = &self.local {
            local.used[index(self.class)].fetch_sub(released, Ordering::AcqRel);
        }
        Ok(())
    }
    /// Divide an already admitted working set without changing its total charge.
    pub fn split(&mut self, amount: usize) -> Result<Self> {
        if amount == 0 || amount >= self.amount {
            return Err(Error::InvalidConfiguration);
        }
        self.amount -= amount;
        Ok(Self {
            class: self.class,
            amount,
            cache: self.cache.clone(),
            totals: self.totals.clone(),
            local: self.local.clone(),
            buffers: self.buffers.clone(),
            stopped: self.stopped.clone(),
        })
    }
    pub fn amount(&self) -> usize {
        self.amount
    }
    pub fn class(&self) -> ResourceClass {
        self.class
    }
    pub fn cache(&self) -> Option<&CacheId> {
        self.cache.as_ref()
    }
    pub fn validate(&self, class: ResourceClass, amount: usize) -> Result<()> {
        if index(self.class) != index(class) || amount > self.amount {
            Err(Error::InvalidConfiguration)
        } else {
            Ok(())
        }
    }
}
impl Drop for Reservation {
    fn drop(&mut self) {
        self.totals.used[index(self.class)].fetch_sub(self.amount, Ordering::AcqRel);
        if let Some(local) = &self.local {
            local.used[index(self.class)].fetch_sub(self.amount, Ordering::AcqRel);
        }
        if matches!(
            self.class,
            ResourceClass::Connection | ResourceClass::IngressConnection
        ) {
            self.totals.wake.wake();
        }
    }
}
pub struct FillReservation {
    pub plaintext: Reservation,
    pub ciphertext: Reservation,
    pub dirty: Option<Reservation>,
}
/// Socket role admission, retained by the connection through kernel completion.
pub struct ConnectionReservation {
    _total: Reservation,
    _role: Reservation,
}
/// Only socket admission crosses workers; cache admission remains I/O-local.
#[derive(Clone)]
pub(crate) struct ConnectionAdmission {
    observer: crate::telemetry::failures::Observer,
    totals: Arc<Counters>,
    stopped: Arc<AtomicBool>,
    total: usize,
    ingress: usize,
}
impl ConnectionAdmission {
    pub(crate) fn register(&self, waker: &std::task::Waker) {
        self.totals.wake.register(waker);
    }
    pub(crate) fn reserve(&self) -> Result<ConnectionReservation> {
        if self.stopped.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        let charge = |class, limit| {
            self.totals.used[index(class)]
                .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                    used.checked_add(1).filter(|next| *next <= limit)
                })
                .map_err(|_| {
                    use crate::telemetry::failures::{Detail, Failure, Stage};
                    self.observer
                        .record(Failure::new(Stage::Admission, Error::Overloaded).detail(
                            Detail::Resource {
                                class,
                                used: self.totals.used[index(class)].load(Ordering::Acquire),
                                limit,
                                requested: 1,
                                cache_used: None,
                                cache_limit: None,
                            },
                        ));
                    Error::Overloaded
                })?;
            Ok(Reservation {
                buffers: Weak::new(),
                stopped: self.stopped.clone(),
                class,
                amount: 1,
                cache: None,
                totals: self.totals.clone(),
                local: None,
            })
        };
        let role = charge(ResourceClass::IngressConnection, self.ingress)?;
        let total = charge(ResourceClass::Connection, self.total)?;
        Ok(ConnectionReservation {
            _total: total,
            _role: role,
        })
    }
}
impl Admission {
    pub(crate) fn set_observer(&self, observer: crate::telemetry::failures::Observer) {
        *self.observer.borrow_mut() = observer;
    }
    pub(crate) fn observer(&self) -> crate::telemetry::failures::Observer {
        self.observer.borrow().clone()
    }
    pub(crate) fn connection_admission(&self) -> ConnectionAdmission {
        ConnectionAdmission {
            observer: self.observer(),
            totals: self.totals.clone(),
            stopped: self.stopped.clone(),
            total: self.limit(ResourceClass::Connection),
            ingress: self.limit(ResourceClass::IngressConnection),
        }
    }
    /// Partition the existing socket ceiling. Ingress cannot consume outbound or
    /// control progress slots; outbound traffic cannot consume control slots.
    pub fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation> {
        if matches!(role, ResourceClass::IngressConnection) {
            return self.connection_admission().reserve();
        }
        if !matches!(
            role,
            ResourceClass::IngressConnection
                | ResourceClass::OutboundConnection
                | ResourceClass::ControlConnection
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
    pub fn new(limits: Limits) -> Self {
        Self {
            observer: RefCell::default(),
            limits,
            totals: Arc::new(Counters::new()),
            caches: RefCell::new(HashMap::default()),
            active_caches: Arc::new(AtomicUsize::new(0)),
            retired_caches: Arc::new(Mutex::new(std::collections::VecDeque::new())),
            stopped: Arc::new(AtomicBool::new(false)),
            buffers: Arc::new(Mutex::new(Vec::new())),
        }
    }
    pub fn limits(&self) -> &Limits {
        &self.limits
    }
    pub fn stop(&self) {
        self.stopped.store(true, Ordering::Release);
        self.reclaim_buffers();
        self.totals.wake.wake();
    }
    pub fn is_stopped(&self) -> bool {
        self.stopped.load(Ordering::Acquire)
    }
    pub fn used(&self, class: ResourceClass) -> usize {
        self.totals.used[index(class)].load(Ordering::Acquire)
    }
    pub fn owns(&self, reservation: &Reservation) -> bool {
        Arc::ptr_eq(&self.totals, &reservation.totals)
    }
    pub fn limit(&self, class: ResourceClass) -> usize {
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
            // Snapshot, renewal, and keyring delivery must make independent progress.
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
    fn fair_limit(&self, class: ResourceClass, active: usize) -> usize {
        let limit = self.limit(class);
        let progress = match class {
            ResourceClass::Plaintext => PAGE_BYTES as usize,
            // A disk read owns padded staging and decoded ciphertext together.
            // Global admission still bounds aggregate concurrent working sets.
            ResourceClass::Ciphertext => {
                2 * (PAGE_BYTES as usize + 16) + crate::store::format::MAX_HEADER_BYTES + 4096
            }
            ResourceClass::DirtyCiphertext | ResourceClass::Registered => PAGE_BYTES as usize + 16,
            _ => 1,
        };
        (limit / active.max(1)).max(progress).min(limit)
    }
    /// Diagnose byte pressure after a failed reserve. A fair-share deficit must
    /// be reclaimed from this cache; global pressure may use any idle cache.
    /// Impossible allocations and cache-accounting saturation have no byte remedy.
    pub(crate) fn reclamation(
        &self,
        cache: &CacheId,
        class: ResourceClass,
        amount: usize,
    ) -> Option<(Option<CacheId>, usize)> {
        let caches = self.caches.borrow();
        let local = caches.get(cache).and_then(Weak::upgrade);
        let fair = self.fair_limit(
            class,
            self.active_caches.load(Ordering::Acquire) + usize::from(local.is_none()),
        );
        if amount > fair {
            return None;
        }
        let local_deficit = local
            .as_ref()
            .map_or(0, |local| local.used[index(class)].load(Ordering::Acquire))
            .saturating_sub(fair - amount);
        if local_deficit != 0 {
            return Some((Some(cache.clone()), local_deficit));
        }
        let deficit = self.used(class).saturating_sub(self.limit(class) - amount);
        (deficit != 0).then_some((None, deficit))
    }
    pub fn reserve(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
    ) -> Result<Reservation> {
        let result = self.reserve_inner(cache, class, amount, false);
        if matches!(result, Err(Error::Overloaded)) {
            self.reclaim_buffers();
            return self.reserve_inner(cache, class, amount, false);
        }
        result
    }
    /// Only for already-admitted work during drain. Does not bypass byte/count
    /// bounds; callers must not use this entry point to accept new requests.
    pub fn reserve_completion(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
    ) -> Result<Reservation> {
        let result = self.reserve_inner(cache, class, amount, true);
        if matches!(result, Err(Error::Overloaded)) {
            self.reclaim_buffers();
            return self.reserve_inner(cache, class, amount, true);
        }
        result
    }
    pub fn retained_buffer_bytes(&self) -> usize {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .iter()
            .map(|(_, reservation)| reservation.amount())
            .sum()
    }
    pub fn reclaim_buffers(&self) {
        self.buffers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clear();
    }
    fn reserve_inner(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
        completing: bool,
    ) -> Result<Reservation> {
        if self.is_stopped() && !completing && !matches!(class, ResourceClass::ControlProgress) {
            return Err(Error::Unavailable);
        }
        if amount == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let limit = self.limit(class);
        let local = if let Some(cache) = cache {
            let mut caches = self.caches.borrow_mut();
            // Completion-thread destructors enqueue only released cache records.
            // Cleanup examines notifications, never every class of every cache.
            let mut retired = self
                .retired_caches
                .lock()
                .unwrap_or_else(|e| e.into_inner());
            for _ in 0..256 {
                let Some(id) = retired.pop_front() else {
                    break;
                };
                if caches
                    .get(&id)
                    .is_some_and(|counts| counts.strong_count() == 0)
                {
                    caches.remove(&id);
                }
            }
            drop(retired);
            if !caches.contains_key(cache) && caches.len() >= self.limits.metadata_entries.get() {
                use crate::telemetry::failures::{Detail, Failure, Stage};
                self.observer.borrow().record(
                    Failure::new(Stage::Admission, Error::Overloaded).detail(
                        Detail::CacheEntries {
                            used: caches.len(),
                            limit: self.limits.metadata_entries.get(),
                        },
                    ),
                );
                return Err(Error::Overloaded);
            }
            Some(
                if let Some(counts) = caches.get(cache).and_then(Weak::upgrade) {
                    counts
                } else {
                    let mut counts = Counters::new();
                    counts.active = Some(self.active_caches.clone());
                    counts.retired = Some((cache.clone(), self.retired_caches.clone()));
                    self.active_caches.fetch_add(1, Ordering::AcqRel);
                    let counts = Arc::new(counts);
                    caches.insert(cache.clone(), Arc::downgrade(&counts));
                    counts
                },
            )
        } else {
            None
        };
        // Active cache accounting is bounded above. Fair-share admission prevents
        // a busy cache from continuing to grow while other caches have live work.
        // Existing leases are never revoked, so a newly active cache may have to
        // wait for their natural release/idle eviction before its first admission.
        if let Some(local) = local.as_ref().filter(|_| !completing) {
            let active = self.active_caches.load(Ordering::Acquire).max(1);
            let fair_limit = self.fair_limit(class, active);
            if local.used[index(class)]
                .load(Ordering::Acquire)
                .checked_add(amount)
                .is_none_or(|used| used > fair_limit)
            {
                self.rejected(
                    class,
                    amount,
                    Some(local.used[index(class)].load(Ordering::Acquire)),
                    Some(fair_limit),
                );
                return Err(Error::Overloaded);
            }
        }
        self.totals.used[index(class)]
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(amount).filter(|next| *next <= limit)
            })
            .map_err(|_| {
                self.rejected(class, amount, None, None);
                Error::Overloaded
            })?;
        if let Some(local) = &local {
            local.used[index(class)].fetch_add(amount, Ordering::AcqRel);
        }
        Ok(Reservation {
            class,
            amount,
            cache: cache.cloned(),
            totals: self.totals.clone(),
            local,
            buffers: Arc::downgrade(&self.buffers),
            stopped: self.stopped.clone(),
        })
    }
    fn rejected(
        &self,
        class: ResourceClass,
        amount: usize,
        cache_used: Option<usize>,
        cache_limit: Option<usize>,
    ) {
        use crate::telemetry::failures::{Detail, Failure, Stage};
        self.observer
            .borrow()
            .record(
                Failure::new(Stage::Admission, Error::Overloaded).detail(Detail::Resource {
                    class,
                    used: self.used(class),
                    limit: self.limit(class),
                    requested: amount,
                    cache_used,
                    cache_limit,
                }),
            );
    }
    /// All allocation dimensions are acquired together; failure rolls back every charge.
    pub fn reserve_fill(&self, cache: &CacheId, persist: bool) -> Result<FillReservation> {
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
#[cfg(test)]
mod tests {
    #[test]
    fn recycled_truncated_capacity_is_zero_before_cross_cache_and_class_reuse() {
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
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
            {
                let pool = admission.buffers.lock().unwrap();
                let bytes = &pool[0].0;
                // SAFETY: buffer() initialized the entire allocation; truncation
                // and recycling cannot deallocate it while the pool lock is held.
                let idle = unsafe { std::slice::from_raw_parts(bytes.as_ptr(), capacity) };
                assert!(idle.iter().all(|byte| *byte == 0));
            }
            let mut reservation = admission
                .reserve(Some(&second), ResourceClass::Plaintext, capacity)
                .unwrap();
            let bytes = reservation.buffer(capacity).unwrap();
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
    #[ignore = "opt-in same-workload payload recycle benchmark"]
    fn payload_recycle_benchmark() {
        use std::{hint::black_box, time::Instant};
        const ITERATIONS: usize = 128;
        for length in [1 << 20, 16 << 20, (16 << 20) + 16] {
            for retain in [true, false] {
                let admission = Admission::new(crate::test_support::cluster::config(false).limits);
                // Keep geometry, allocation, writes, admission, and destructor work
                // identical between revisions. Stop forces the non-pooling path.
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
                    // First sample warms allocator and retained buffers.
                    if sample != 0 {
                        println!(
                            "payload_recycle length={length} retain={retain} sample={sample} iterations={ITERATIONS} ns_per_op={:.0}",
                            elapsed.as_nanos() as f64 / ITERATIONS as f64,
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
        use super::*;
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
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
    use super::*;
    #[test]
    fn ingress_saturation_preserves_outbound_and_control_without_raising_total() {
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
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
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
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
        let admission = Admission::new(limits);
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
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
        assert!(matches!(
            admission.reserve(None, ResourceClass::Plaintext, usize::MAX),
            Err(Error::Overloaded)
        ));
        admission.stop();
        assert!(matches!(
            admission.reserve(None, ResourceClass::Plaintext, 1),
            Err(Error::Unavailable)
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
        let admission = Admission::new(limits);
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
            Err(Error::Overloaded)
        ));
        drop(second);
        let third = admission
            .reserve(Some(&a), ResourceClass::RequestContext, 60)
            .unwrap();
        assert_eq!(admission.active_caches.load(Ordering::Acquire), 1);
        assert!(!admission.caches.borrow().contains_key(&b));
        assert_eq!(admission.used(ResourceClass::RequestContext), 100);
        drop((first, third));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }
}
