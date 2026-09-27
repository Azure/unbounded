//! I/O-local admission authority with completion-safe cross-thread quota release.
use crate::{
    error::{Error, Result},
    model::{
        identity::CacheId,
        limits::{Limits, ResourceClass},
        range::PAGE_BYTES,
    },
};
use std::{
    cell::{Cell, RefCell},
    collections::HashMap,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};

const CLASSES: usize = 11;
fn index(class: ResourceClass) -> usize {
    class as usize
}
struct Counters {
    used: [AtomicUsize; CLASSES],
    transit: AtomicUsize,
    ordinary_ciphertext: AtomicUsize,
}
impl Counters {
    fn new() -> Self {
        Self {
            used: std::array::from_fn(|_| AtomicUsize::new(0)),
            transit: AtomicUsize::new(0),
            ordinary_ciphertext: AtomicUsize::new(0),
        }
    }
}

pub struct Admission {
    limits: Limits,
    totals: Arc<Counters>,
    caches: RefCell<HashMap<CacheId, Arc<Counters>>>,
    stopped: Cell<bool>,
    transit_bytes: Cell<usize>,
}

/// Ownership of a charge, released only when its last containing allocation dies.
pub struct Reservation {
    class: ResourceClass,
    amount: usize,
    cache: Option<CacheId>,
    totals: Arc<Counters>,
    local: Option<Arc<Counters>>,
    transit: bool,
}
impl Reservation {
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
        if self.transit {
            self.totals.transit.fetch_sub(self.amount, Ordering::AcqRel);
        } else if matches!(self.class, ResourceClass::Ciphertext) {
            self.totals
                .ordinary_ciphertext
                .fetch_sub(self.amount, Ordering::AcqRel);
        }
        self.totals.used[index(self.class)].fetch_sub(self.amount, Ordering::AcqRel);
        if let Some(local) = &self.local {
            local.used[index(self.class)].fetch_sub(self.amount, Ordering::AcqRel);
        }
    }
}
pub struct FillReservation {
    pub plaintext: Reservation,
    pub ciphertext: Reservation,
    pub dirty: Option<Reservation>,
}
impl Admission {
    pub fn new(limits: Limits) -> Self {
        Self {
            limits,
            totals: Arc::new(Counters::new()),
            caches: RefCell::new(HashMap::new()),
            stopped: Cell::new(false),
            transit_bytes: Cell::new(0),
        }
    }
    pub fn limits(&self) -> &Limits {
        &self.limits
    }
    /// Reserve small transit chunks inside the aggregate ciphertext ceiling.
    /// Called during graph assembly, before admitting local work.
    pub fn enable_transit(&self) -> Result<()> {
        let bytes = Self::transit_capacity(&self.limits)?;
        self.transit_bytes.set(bytes);
        Ok(())
    }
    pub fn transit_capacity(limits: &Limits) -> Result<usize> {
        let available = limits
            .ciphertext_bytes
            .get()
            .checked_sub(PAGE_BYTES as usize + 16)
            .ok_or(Error::InvalidConfiguration)?;
        let slots = limits
            .relay_transfers
            .get()
            .min(limits.queue_entries.get())
            .min(limits.client_connections.get() / 2)
            .min(available / TRANSIT_CHUNK)
            .min(PAGE_BYTES as usize / TRANSIT_CHUNK);
        if slots == 0 {
            return Err(Error::InvalidConfiguration);
        }
        Ok(slots * TRANSIT_CHUNK)
    }
    pub fn reserve_transit(&self) -> Result<Reservation> {
        if self.transit_bytes.get() == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.reserve_inner(None, ResourceClass::Ciphertext, TRANSIT_CHUNK, false, true)
    }
    pub fn stop(&self) {
        self.stopped.set(true);
    }
    pub fn is_stopped(&self) -> bool {
        self.stopped.get()
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
            ResourceClass::Pipe => self.limits.pipes.get(),
            ResourceClass::ControlProgress => self.limits.queue_entries.get(),
            ResourceClass::Relay => self.limits.relay_transfers.get(),
        }
    }
    pub fn reserve(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
    ) -> Result<Reservation> {
        self.reserve_inner(cache, class, amount, false, false)
    }
    /// Only for already-admitted work during drain. Does not bypass byte/count
    /// bounds; callers must not use this entry point to accept new requests.
    pub fn reserve_completion(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
    ) -> Result<Reservation> {
        self.reserve_inner(cache, class, amount, true, false)
    }
    fn reserve_inner(
        &self,
        cache: Option<&CacheId>,
        class: ResourceClass,
        amount: usize,
        completing: bool,
        transit: bool,
    ) -> Result<Reservation> {
        if self.stopped.get() && !completing && !matches!(class, ResourceClass::ControlProgress) {
            return Err(Error::Unavailable);
        }
        if amount == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let limit = self.limit(class);
        if matches!(class, ResourceClass::Ciphertext) {
            let used = if transit {
                self.totals.transit.load(Ordering::Acquire)
            } else {
                self.totals.ordinary_ciphertext.load(Ordering::Acquire)
            };
            let ceiling = if transit {
                self.transit_bytes.get()
            } else {
                limit.saturating_sub(self.transit_bytes.get())
            };
            if used.checked_add(amount).is_none_or(|next| next > ceiling) {
                return Err(Error::Overloaded);
            }
        }
        let local = if let Some(cache) = cache {
            let mut caches = self.caches.borrow_mut();
            caches.retain(|_, counts| counts.used.iter().any(|n| n.load(Ordering::Acquire) != 0));
            if !caches.contains_key(cache) && caches.len() >= self.limits.metadata_entries.get() {
                return Err(Error::Overloaded);
            }
            Some(
                caches
                    .entry(cache.clone())
                    .or_insert_with(|| Arc::new(Counters::new()))
                    .clone(),
            )
        } else {
            None
        };
        // Active cache accounting is bounded above. Fair-share admission prevents
        // a busy cache from continuing to grow while other caches have live work.
        // Existing leases are never revoked, so a newly active cache may have to
        // wait for their natural release/idle eviction before its first admission.
        if let Some(local) = local.as_ref().filter(|_| !completing) {
            let active = self.caches.borrow().len().max(1);
            let progress = match class {
                ResourceClass::Plaintext => PAGE_BYTES as usize,
                ResourceClass::Ciphertext
                | ResourceClass::DirtyCiphertext
                | ResourceClass::Registered => PAGE_BYTES as usize + 16,
                _ => 1,
            };
            let fair_limit = (limit / active).max(progress).min(limit);
            if local.used[index(class)]
                .load(Ordering::Acquire)
                .checked_add(amount)
                .is_none_or(|used| used > fair_limit)
            {
                return Err(Error::Overloaded);
            }
        }
        self.totals.used[index(class)]
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |used| {
                used.checked_add(amount).filter(|next| *next <= limit)
            })
            .map_err(|_| Error::Overloaded)?;
        if let Some(local) = &local {
            local.used[index(class)].fetch_add(amount, Ordering::AcqRel);
        }
        if transit {
            self.totals.transit.fetch_add(amount, Ordering::AcqRel);
        } else if matches!(class, ResourceClass::Ciphertext) {
            self.totals
                .ordinary_ciphertext
                .fetch_add(amount, Ordering::AcqRel);
        }
        Ok(Reservation {
            class,
            amount,
            cache: cache.cloned(),
            totals: self.totals.clone(),
            local,
            transit,
        })
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
pub const TRANSIT_CHUNK: usize = 16 * 1024;
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn transit_floor_partition_and_completion_release_preserve_total_ceiling() {
        use std::num::NonZeroUsize as N;
        let mut limits = crate::test_support::cluster::config(false).limits;
        let page = PAGE_BYTES as usize + 16;
        for bytes in [1, page, page + TRANSIT_CHUNK - 1] {
            limits.ciphertext_bytes = N::new(bytes).unwrap();
            assert_eq!(
                Admission::transit_capacity(&limits),
                Err(Error::InvalidConfiguration)
            );
        }
        limits.ciphertext_bytes = N::new(page + TRANSIT_CHUNK).unwrap();
        limits.client_connections = N::new(1).unwrap();
        assert_eq!(
            Admission::transit_capacity(&limits),
            Err(Error::InvalidConfiguration)
        );
        limits.client_connections = N::new(2).unwrap();
        assert_eq!(Admission::transit_capacity(&limits), Ok(TRANSIT_CHUNK));
        let admission = Admission::new(limits);
        admission.enable_transit().unwrap();
        let ordinary = admission
            .reserve(None, ResourceClass::Ciphertext, page)
            .unwrap();
        assert!(matches!(
            admission.reserve_completion(None, ResourceClass::Ciphertext, 1),
            Err(Error::Overloaded)
        ));
        let chunk = admission.reserve_transit().unwrap();
        assert_eq!(
            admission.used(ResourceClass::Ciphertext),
            page + TRANSIT_CHUNK
        );
        assert!(matches!(
            admission.reserve_transit(),
            Err(Error::Overloaded)
        ));
        std::thread::spawn(move || drop(chunk)).join().unwrap();
        assert_eq!(admission.used(ResourceClass::Ciphertext), page);
        let chunk = admission.reserve_transit().unwrap();
        admission.stop();
        assert!(matches!(
            admission.reserve_transit(),
            Err(Error::Unavailable)
        ));
        drop((ordinary, chunk));
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(
            admission.totals.ordinary_ciphertext.load(Ordering::Acquire),
            0
        );
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
        assert_eq!(admission.caches.borrow().len(), 1);
        assert_eq!(admission.used(ResourceClass::RequestContext), 100);
        drop((first, third));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }
}
