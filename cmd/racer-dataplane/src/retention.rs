//! Bounded second-sight page history. Reader accounting is exact, operation-local
//! state; Bloom approximation applies ONLY to whether a page was seen previously.
use crate::error::{Error, Result};
use crate::model::PageId;
use page_alloc::retention::{Heat, SecondSight};
use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

const PERIOD: Duration = Duration::from_secs(60);

#[derive(Clone, Copy, Debug)]
pub struct Observation {
    eligible: bool,
}
impl Observation {
    pub fn eligible(self) -> bool {
        self.eligible
    }
}

/// One token per logical page interest, retained across retries and handoffs.
/// No global reader map, expiration, collision, or capacity-based suppression.
#[derive(Default)]
pub struct Interest(Mutex<Option<(PageId, Observation)>>);

#[derive(Clone, Copy, Default, Debug)]
pub struct Snapshot {
    pub observations: u64,
    pub qualified: u64,
    pub persistence_attempts: u64,
    pub persistence_accepted: u64,
    pub filter_set_bits: usize,
    pub filter_bits: usize,
    pub heat_entries: usize,
    pub pending_payload_bytes: u64,
    pub indexed_payload_bytes: u64,
    /// Event-time cache-only ownership, never insertion-class resident gauges.
    /// Index 0 is nonowned; index 1 is owned.
    pub disk: [DiskClassSnapshot; 2],
}
#[derive(Clone, Copy, Default, Debug)]
pub struct DiskClassSnapshot {
    pub published_pages: u64,
    pub published_payload_bytes: u64,
    pub index_evicted_pages: u64,
    pub index_evicted_payload_bytes: u64,
    pub segment_evicted_pages: u64,
    pub segment_evicted_payload_bytes: u64,
    pub read_payload_bytes: u64,
}
/// Counts one storage owner's logical page payload, not ciphertext or padding.
pub(crate) struct Payload {
    retention: Rc<Retention>,
    bytes: u64,
    indexed: bool,
}
impl Drop for Payload {
    fn drop(&mut self) {
        let gauge = self.retention.payload_gauge(self.indexed);
        gauge.set(gauge.get() - self.bytes);
    }
}
type Ownership = Rc<dyn Fn(&PageId) -> bool>;
#[derive(Default)]
struct Counters {
    observations: AtomicU64,
    qualified: AtomicU64,
    attempts: AtomicU64,
    accepted: AtomicU64,
}
pub struct Retention {
    state: RefCell<State>,

    ownership: RefCell<Ownership>,

    counters: Counters,

    enabled: bool,

    disk: Cell<[DiskClassSnapshot; 2]>,

    pending_payload_bytes: Cell<u64>,

    indexed_payload_bytes: Cell<u64>,
}
struct State {
    history: SecondSight,

    heat: Heat<PageId>,
}
impl Retention {
    pub fn new(history_bytes: usize, heat_entries: usize) -> Result<Self> {
        Self::configured(history_bytes, heat_entries, PERIOD, true)
    }
    pub fn configured(
        history_bytes: usize,
        heat_entries: usize,
        period: Duration,
        enabled: bool,
    ) -> Result<Self> {
        let history = SecondSight::new(history_bytes, period, uring_runtime::environment::now())
            .ok_or(Error::InvalidConfiguration)?;
        let heat = Heat::new(heat_entries).ok_or(Error::InvalidConfiguration)?;
        Ok(Self {
            state: RefCell::new(State { history, heat }),
            ownership: RefCell::new(Rc::new(|_| false)),
            counters: Counters::default(),
            enabled,
            disk: Cell::new([DiskClassSnapshot::default(); 2]),
            pending_payload_bytes: Cell::new(0),
            indexed_payload_bytes: Cell::new(0),
        })
    }
    pub fn set_ownership(&self, classify: Ownership) {
        *self.ownership.borrow_mut() = classify;
    }
    /// Current cache-only classification; missing/stale ownership hints are false.
    /// Does not insert history, touch heat, or initiate a placement ranking.
    pub fn is_owned(&self, page: &PageId) -> bool {
        let classify = self.ownership.borrow().clone();
        classify(page)
    }
    pub fn owned_only(&self, page: &PageId) -> Observation {
        Observation {
            eligible: self.is_owned(page),
        }
    }
    pub fn observe(&self, page: &PageId, interest: &Interest) -> Observation {
        self.observe_at(page, interest, uring_runtime::environment::now())
    }
    fn observe_at(&self, page: &PageId, interest: &Interest, now: Instant) -> Observation {
        let mut latched = interest.0.lock().expect("interest mutex poisoned");
        if let Some((identity, observation)) = latched.as_ref() {
            assert_eq!(identity, page, "interest belongs to one logical page");
            return *observation;
        }
        let owned = self.is_owned(page);
        let mut state = self.state.borrow_mut();
        let seen = self.enabled && state.history.observe(page, now);
        if self.enabled {
            state.heat.touch(page, now);
        }
        // Disabled second-sight keeps legacy owned-only admission.
        let observation = Observation {
            eligible: owned || (self.enabled && seen),
        };
        *latched = Some((page.clone(), observation));
        self.counters.observations.fetch_add(1, Ordering::Relaxed);
        self.counters
            .qualified
            .fetch_add(u64::from(observation.eligible), Ordering::Relaxed);
        observation
    }
    pub fn eligible(&self, observation: Observation) -> bool {
        observation.eligible()
    }
    /// Storage installs/removes heat with resident index mappings. Admission never
    /// replaces another resident's exact heat just because hashes collide.
    pub fn track(&self, page: &PageId) -> bool {
        self.state
            .borrow_mut()
            .heat
            .track(page, uring_runtime::environment::now())
    }
    pub fn forget(&self, page: &PageId) {
        self.state.borrow_mut().heat.forget(page);
    }
    pub fn touch(&self, page: &PageId) {
        self.state
            .borrow_mut()
            .heat
            .touch(page, uring_runtime::environment::now());
    }
    pub fn score(&self, page: &PageId) -> u8 {
        self.score_at(page, uring_runtime::environment::now())
    }
    fn score_at(&self, page: &PageId, now: Instant) -> u8 {
        let owned = self.is_owned(page);
        let mut state = self.state.borrow_mut();
        let heat = state.heat.score(page, now);
        if self.enabled {
            heat + u8::from(owned)
        } else {
            0
        }
    }
    pub fn persistence_attempt(&self, accepted: bool) {
        self.counters.attempts.fetch_add(1, Ordering::Relaxed);
        self.counters
            .accepted
            .fetch_add(u64::from(accepted), Ordering::Relaxed);
    }
    fn payload_gauge(&self, indexed: bool) -> &Cell<u64> {
        if indexed {
            &self.indexed_payload_bytes
        } else {
            &self.pending_payload_bytes
        }
    }
    pub(crate) fn payload(self: &Rc<Self>, bytes: u64, indexed: bool) -> Payload {
        let gauge = self.payload_gauge(indexed);
        gauge.set(
            gauge
                .get()
                .checked_add(bytes)
                .expect("bounded storage payload"),
        );
        Payload {
            retention: self.clone(),
            bytes,
            indexed,
        }
    }
    fn disk_event(&self, page: &PageId, update: impl FnOnce(&mut DiskClassSnapshot)) {
        let class = usize::from(self.is_owned(page));
        let mut disk = self.disk.get();
        update(&mut disk[class]);
        self.disk.set(disk);
    }
    pub(crate) fn published(&self, page: &PageId, bytes: u64) {
        self.disk_event(page, |c| {
            c.published_pages = c.published_pages.saturating_add(1);
            c.published_payload_bytes = c.published_payload_bytes.saturating_add(bytes);
        });
    }
    pub(crate) fn evicted(&self, page: &PageId, bytes: u64, segment: bool) {
        self.disk_event(page, |c| {
            let (pages, payload) = if segment {
                (
                    &mut c.segment_evicted_pages,
                    &mut c.segment_evicted_payload_bytes,
                )
            } else {
                (
                    &mut c.index_evicted_pages,
                    &mut c.index_evicted_payload_bytes,
                )
            };
            *pages = pages.saturating_add(1);
            *payload = payload.saturating_add(bytes);
        });
    }
    pub(crate) fn disk_read(&self, page: &PageId, bytes: u64) {
        self.disk_event(page, |c| {
            c.read_payload_bytes = c.read_payload_bytes.saturating_add(bytes)
        });
    }
    pub fn snapshot(&self) -> Snapshot {
        let mut state = self.state.borrow_mut();
        let (set_bits, bits) = state.history.occupancy(uring_runtime::environment::now());
        Snapshot {
            observations: self.counters.observations.load(Ordering::Relaxed),
            qualified: self.counters.qualified.load(Ordering::Relaxed),
            persistence_attempts: self.counters.attempts.load(Ordering::Relaxed),
            persistence_accepted: self.counters.accepted.load(Ordering::Relaxed),
            pending_payload_bytes: self.pending_payload_bytes.get(),
            indexed_payload_bytes: self.indexed_payload_bytes.get(),
            disk: self.disk.get(),
            filter_set_bits: set_bits,
            filter_bits: bits,
            heat_entries: state.heat.len(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::Cell;
    fn page(number: u64) -> PageId {
        PageId {
            version: crate::model::ObjectVersion {
                object: crate::model::ObjectId {
                    cache: racer_control_wire::CacheId("cache".into()),
                    key: crate::model::CacheKey([7; 32]),
                },
                etag: crate::model::StrongEtag::test_value("v1"),
            },
            number: crate::model::PageNumber(number),
        }
    }
    #[test]
    fn disk_observability_samples_current_class_without_scrape_classification() {
        let policy = Rc::new(Retention::new(4096, 2).unwrap());
        let owned = Rc::new(Cell::new(false));
        let current = owned.clone();
        let calls = Rc::new(Cell::new(0));
        let counted = calls.clone();
        policy.set_ownership(Rc::new(move |_| {
            counted.set(counted.get() + 1);
            current.get()
        }));
        let pending = policy.payload(17, false);
        let indexed = policy.payload(17, true);
        policy.published(&page(0), 17);
        owned.set(true);
        policy.disk_read(&page(0), 17);
        policy.evicted(&page(0), 17, false);
        owned.set(false);
        policy.evicted(&page(1), 3, true);
        let before = calls.get();
        for _ in 0..10 {
            let snapshot = policy.snapshot();
            assert_eq!(snapshot.pending_payload_bytes, 17);
            assert_eq!(snapshot.indexed_payload_bytes, 17);
            assert_eq!(snapshot.disk[0].published_pages, 1);
            assert_eq!(snapshot.disk[0].published_payload_bytes, 17);
            assert_eq!(snapshot.disk[1].published_pages, 0);
            assert_eq!(snapshot.disk[1].read_payload_bytes, 17);
            assert_eq!(snapshot.disk[1].index_evicted_pages, 1);
            assert_eq!(snapshot.disk[1].index_evicted_payload_bytes, 17);
            assert_eq!(snapshot.disk[0].segment_evicted_pages, 1);
            assert_eq!(snapshot.disk[0].segment_evicted_payload_bytes, 3);
            assert_eq!(snapshot.observations, 0);
            assert_eq!(snapshot.heat_entries, 0);
        }
        assert_eq!(calls.get(), before);
        drop(pending);
        drop(indexed);
        assert_eq!(policy.snapshot().pending_payload_bytes, 0);
        assert_eq!(policy.snapshot().indexed_payload_bytes, 0);
    }
    #[test]
    fn readers_latch_before_insert_and_retries_do_not_promote() {
        let policy = Retention::new(32, 1).unwrap();
        let first = Interest::default();
        assert!(!policy.observe(&page(0), &first).eligible());
        for n in 1..1000 {
            policy.observe(&page(n), &Interest::default());
        }
        assert!(!policy.observe(&page(0), &first).eligible());
        for _ in 0..1000 {
            assert!(policy.observe(&page(0), &Interest::default()).eligible());
        }
        assert_eq!(policy.snapshot().observations, 2000);
        assert!(policy.snapshot().filter_set_bits <= policy.snapshot().filter_bits);
    }
    #[test]
    fn rotation_retains_three_to_four_minutes_and_clears_long_idle() {
        for period in [Duration::from_secs(60), Duration::from_millis(10)] {
            let policy = Retention::configured(4096, 1, period, true).unwrap();
            let start = uring_runtime::environment::now();
            let first = Interest::default();
            assert!(!policy.observe_at(&page(0), &first, start).eligible());
            assert!(
                !policy
                    .observe_at(&page(0), &first, start + period * 3)
                    .eligible()
            );
            let retained = Retention::configured(4096, 1, period, true).unwrap();
            retained.observe_at(&page(0), &Interest::default(), start);
            assert!(
                retained
                    .observe_at(&page(0), &Interest::default(), start + period * 3)
                    .eligible()
            );
            assert!(
                !policy
                    .observe_at(&page(0), &Interest::default(), start + period * 4)
                    .eligible()
            );
        }
    }
    #[test]
    fn ownership_is_current_and_heat_saturates_then_decays() {
        let policy = Retention::new(4096, 2).unwrap();
        assert!(policy.track(&page(0)));
        assert!(policy.track(&page(1)));
        assert!(!policy.track(&page(2)));
        for _ in 0..8 {
            policy.touch(&page(0));
        }
        assert_eq!(policy.score(&page(0)), 3);
        assert_eq!(policy.score(&page(1)), 0);
        let now = uring_runtime::environment::now();
        assert_eq!(policy.score_at(&page(0), now + Duration::from_secs(60)), 2);
        let owned = Rc::new(Cell::new(true));
        let value = owned.clone();
        policy.set_ownership(Rc::new(move |_| value.get()));
        assert_eq!(policy.score_at(&page(0), now + Duration::from_secs(180)), 1);
        owned.set(false);
        assert_eq!(policy.score_at(&page(0), now + Duration::from_secs(180)), 0);
        policy.forget(&page(0));
        assert!(policy.track(&page(2)));
        assert_eq!(policy.snapshot().heat_entries, 2);
    }
    #[test]
    fn disabled_is_owned_only_and_metrics_count_once() {
        let policy = Retention::configured(4096, 1, PERIOD, false).unwrap();
        for _ in 0..10 {
            assert!(!policy.observe(&page(0), &Interest::default()).eligible());
        }
        policy.set_ownership(Rc::new(|_| true));
        let interest = Interest::default();
        for _ in 0..10 {
            assert!(policy.observe(&page(0), &interest).eligible());
        }
        policy.persistence_attempt(false);
        policy.persistence_attempt(true);
        let snapshot = policy.snapshot();
        assert_eq!(snapshot.observations, 11);
        assert_eq!(snapshot.qualified, 1);
        assert_eq!(snapshot.persistence_attempts, 2);
        assert_eq!(snapshot.persistence_accepted, 1);
        assert_eq!(snapshot.filter_set_bits, 0);
        assert!(Retention::configured(4096, 1, Duration::ZERO, true).is_err());
    }
    #[test]
    fn full_identity_and_memory_bounds() {
        let policy = Retention::new(4096, 2).unwrap();
        let a = page(0);
        let mut b = a.clone();
        b.version.etag = crate::model::StrongEtag::test_value("v2");
        assert!(!policy.observe(&a, &Interest::default()).eligible());
        assert!(!policy.observe(&b, &Interest::default()).eligible());
        assert!(policy.track(&a));
        assert!(policy.track(&b));
        policy.touch(&a);
        assert_eq!(policy.score(&a), 1);
        assert_eq!(policy.score(&b), 0);
        for n in 0..10000 {
            policy.observe(&page(n), &Interest::default());
        }
        assert_eq!(policy.snapshot().heat_entries, 2);
        assert_eq!(policy.snapshot().filter_bits, 4096 * 8);
        assert!(Retention::new(31, 1).is_err());
        assert!(Retention::new(4096, 0).is_err());
    }
}
