//! Bounded second-sight page history. Reader accounting is exact, operation-local
//! state; Bloom approximation applies ONLY to whether a page was seen previously.
use crate::error::{Error, Result};
use crate::model::PageId;
use crate::priority::{ShareBasis, Weights};
use crate::runtime::HashMap;
use page_alloc::retention::{Heat, SecondSight};
use racer_control_wire::CacheId;
use std::cell::{Cell, OnceCell, RefCell};
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
    /// Optional pages skipped because their cache was over share under pressure.
    pub share_rejected: u64,
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
    /// Indexed payload is also charged to its cache for fair-share accounting.
    cache: Option<CacheId>,
}
impl Drop for Payload {
    fn drop(&mut self) {
        let gauge = self.retention.payload_gauge(self.indexed);
        gauge.set(gauge.get() - self.bytes);
        if let Some(cache) = &self.cache {
            let mut caches = self.retention.cache_bytes.borrow_mut();
            let used = caches.get_mut(cache).expect("charged cache payload");
            *used -= self.bytes;
            if *used == 0 {
                caches.remove(cache);
            }
        }
    }
}
pub(crate) type Ownership = Rc<dyn Fn(&PageId) -> bool>;
pub(crate) type OwnershipPass = Rc<dyn Fn() -> Ownership>;

/// Upper bound of `Scoring::score`: heat 0 to 3 plus 1 for ownership.
pub(crate) const MAX_SCORE: u8 = 4;

/// One eviction or ordering pass. Ownership and time are pinned when the pass
/// starts. Over-share classification is computed lazily per candidate cache
/// and memoized for the rest of the pass, so a pass never walks the published
/// catalog: the share basis scans only caches holding resident payload.
pub(crate) struct Scoring<'a> {
    retention: &'a Retention,
    ownership: Ownership,
    now: Instant,
    capacity: u64,
    basis: OnceCell<ShareBasis>,
    classified: RefCell<HashMap<CacheId, bool>>,
    any_over_share: OnceCell<bool>,
}
impl Scoring<'_> {
    pub(crate) fn score(&self, page: &PageId) -> u8 {
        self.retention
            .score_owned_at(page, self.now, (self.ownership)(page))
    }
    /// Whether the cache's indexed payload exceeded its fair share of this
    /// worker's disk capacity when the pass started.
    pub(crate) fn over_share(&self, cache: &CacheId) -> bool {
        if let Some(over) = self.classified.borrow().get(cache) {
            return *over;
        }
        let resident = self.retention.cache_payload_bytes(cache);
        // A cache without resident payload is never over share; skip the basis.
        let over = resident != 0 && self.classify(cache, resident);
        self.classified.borrow_mut().insert(cache.clone(), over);
        over
    }
    fn classify(&self, cache: &CacheId, resident: u64) -> bool {
        let basis = *self.basis.get_or_init(|| self.retention.share_basis());
        let share = self
            .retention
            .weights
            .borrow()
            .share_in(basis, self.capacity, cache);
        over_share(resident, share, self.capacity)
    }
    /// Whether any cache with resident payload is over share. Scans only the
    /// resident caches, once per pass.
    pub(crate) fn any_over_share(&self) -> bool {
        *self.any_over_share.get_or_init(|| {
            let used = self.retention.cache_bytes.borrow();
            used.keys().any(|cache| self.over_share(cache))
        })
    }
    /// Combined victim rank: under-share pages outrank every over-share page;
    /// the retention score orders pages within one class. Higher is kept longer.
    pub(crate) fn rank(&self, page: &PageId) -> u64 {
        let under = !self.over_share(&page.version.object.cache);
        (u64::from(under) << 8) | u64::from(self.score(page))
    }
}
/// One cache's indexed payload and fair share on this worker.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheDisk {
    pub cache: CacheId,
    pub resident_bytes: u64,
    pub share_bytes: u64,
}
impl CacheDisk {
    /// Above its share. A cache entitled to the whole capacity (the sole
    /// cache, or no published catalog) is never over share, even when the
    /// capacity estimate is below its actual residency.
    pub fn over_share(&self, capacity: u64) -> bool {
        over_share(self.resident_bytes, self.share_bytes, capacity)
    }
}
fn over_share(resident: u64, share: u64, capacity: u64) -> bool {
    share < capacity && resident > share
}
#[derive(Default)]
struct Counters {
    observations: AtomicU64,
    qualified: AtomicU64,
    attempts: AtomicU64,
    accepted: AtomicU64,
    share_rejected: AtomicU64,
}
pub struct Retention {
    state: RefCell<State>,

    ownership: RefCell<Ownership>,
    ownership_pass: RefCell<Option<OwnershipPass>>,

    counters: Counters,

    enabled: bool,

    disk: Cell<[DiskClassSnapshot; 2]>,

    pending_payload_bytes: Cell<u64>,

    indexed_payload_bytes: Cell<u64>,

    /// Indexed payload bytes per cache. Maintained by `Payload` owners, so
    /// every index mapping removal path (eviction, replacement, cache removal,
    /// restore) releases its charge exactly once.
    cache_bytes: RefCell<HashMap<CacheId, u64>>,

    weights: RefCell<Weights>,
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
            ownership_pass: RefCell::new(None),
            counters: Counters::default(),
            enabled,
            disk: Cell::new([DiskClassSnapshot::default(); 2]),
            pending_payload_bytes: Cell::new(0),
            indexed_payload_bytes: Cell::new(0),
            cache_bytes: RefCell::default(),
            weights: RefCell::default(),
        })
    }
    pub fn set_ownership(&self, classify: Ownership) {
        *self.ownership.borrow_mut() = classify;
        *self.ownership_pass.borrow_mut() = None;
    }
    pub(crate) fn set_ownership_pass(&self, prepare: OwnershipPass) {
        *self.ownership_pass.borrow_mut() = Some(prepare);
    }
    /// Share caches' disk capacity by the published priority weights.
    pub fn set_weights(&self, weights: Weights) {
        *self.weights.borrow_mut() = weights;
    }
    pub fn weights(&self) -> Weights {
        self.weights.borrow().clone()
    }
    /// `capacity` is this worker's usable payload capacity in bytes.
    pub(crate) fn scoring_at(&self, now: Instant, capacity: u64) -> Scoring<'_> {
        let ownership = match self.ownership_pass.borrow().as_ref() {
            Some(prepare) => prepare(),
            None => self.ownership.borrow().clone(),
        };
        Scoring {
            retention: self,
            ownership,
            now,
            capacity,
            basis: OnceCell::new(),
            classified: RefCell::default(),
            any_over_share: OnceCell::new(),
        }
    }
    /// Share basis over the published catalog plus every cache that still
    /// holds indexed payload, matching the member set of `cache_disk`.
    fn share_basis(&self) -> ShareBasis {
        self.weights
            .borrow()
            .basis(self.cache_bytes.borrow().keys())
    }
    /// Indexed payload bytes currently charged to one cache.
    pub fn cache_payload_bytes(&self, cache: &CacheId) -> u64 {
        self.cache_bytes.borrow().get(cache).copied().unwrap_or(0)
    }
    /// Whether one cache's indexed payload exceeds its share of `capacity`.
    pub fn over_share(&self, cache: &CacheId, capacity: u64) -> bool {
        let resident = self.cache_payload_bytes(cache);
        resident != 0
            && over_share(
                resident,
                self.weights
                    .borrow()
                    .share_in(self.share_basis(), capacity, cache),
                capacity,
            )
    }
    /// Resident bytes and shares for every published cache and every cache
    /// that still holds indexed payload, sorted by cache id. Telemetry only:
    /// this walks and sorts the whole catalog.
    pub fn cache_disk(&self, capacity: u64) -> Vec<CacheDisk> {
        let used = self.cache_bytes.borrow();
        let weights = self.weights.borrow();
        let mut caches: Vec<CacheId> = used.keys().cloned().collect();
        caches.extend(
            weights
                .caches()
                .into_iter()
                .filter(|c| !used.contains_key(c)),
        );
        caches.sort_by(|a, b| a.0.cmp(&b.0));
        let shares = weights.shares(capacity, &caches);
        caches
            .into_iter()
            .zip(shares)
            .map(|(cache, share_bytes)| CacheDisk {
                resident_bytes: used.get(&cache).copied().unwrap_or(0),
                cache,
                share_bytes,
            })
            .collect()
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
        // Disabled second-sight admits only owned pages.
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
        self.score_owned_at(page, now, owned)
    }
    fn score_owned_at(&self, page: &PageId, now: Instant, owned: bool) -> u8 {
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
    pub fn share_rejected(&self) {
        self.counters.share_rejected.fetch_add(1, Ordering::Relaxed);
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
            cache: None,
        }
    }
    /// Charge one index mapping's payload to the aggregate and its cache.
    pub(crate) fn indexed_payload(self: &Rc<Self>, cache: &CacheId, bytes: u64) -> Payload {
        let mut payload = self.payload(bytes, true);
        *self
            .cache_bytes
            .borrow_mut()
            .entry(cache.clone())
            .or_default() += bytes;
        payload.cache = Some(cache.clone());
        payload
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
            share_rejected: self.counters.share_rejected.load(Ordering::Relaxed),
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
    #[test]
    fn lazy_scoring_classification_matches_telemetry_cache_disk() {
        use racer_control_wire::CacheDefinition;
        let clock = uring_runtime::environment::SimulationClock::new(52);
        let _environment = clock.environment(0).enter();
        let policy = Rc::new(Retention::new(4096, 1).unwrap());
        let id = |n: &str| CacheId(n.into());
        let def = |n: &str, priority: u16| CacheDefinition {
            id: id(n),
            name: n.into(),
            client_socket: "/client".into(),
            origin_socket: "/origin".into(),
            priority,
        };
        // "u" holds payload but is unpublished; "e" is published but empty.
        let _held = [
            policy.indexed_payload(&id("a"), 600),
            policy.indexed_payload(&id("b"), 300),
            policy.indexed_payload(&id("u"), 400),
        ];
        let all = ["a", "b", "u", "e", "z"].map(id);
        let tables: [Vec<CacheDefinition>; 5] = [
            vec![],
            vec![def("a", 900), def("b", 100), def("e", 500)],
            vec![def("a", 0), def("b", 0), def("e", 0)],
            vec![def("a", 1)],
            vec![def("a", 1000), def("b", 1000), def("e", 0)],
        ];
        let now = uring_runtime::environment::now();
        for table in &tables {
            policy.weights().update(table);
            for capacity in [0, 500, 1000, 1300, u64::MAX] {
                let disk = policy.cache_disk(capacity);
                let scoring = policy.scoring_at(now, capacity);
                for cache in &all {
                    let expected = disk
                        .iter()
                        .find(|d| &d.cache == cache)
                        .is_some_and(|d| d.over_share(capacity));
                    assert_eq!(scoring.over_share(cache), expected, "{cache:?} {capacity}");
                    assert_eq!(policy.over_share(cache, capacity), expected);
                }
                let any = disk.iter().any(|d| d.over_share(capacity));
                assert_eq!(scoring.any_over_share(), any);
                assert_eq!(policy.scoring_at(now, capacity).any_over_share(), any);
            }
        }
    }
    #[test]
    fn scoring_pass_pins_ownership_and_time_and_replacement_clears_factory() {
        let clock = uring_runtime::environment::SimulationClock::new(51);
        let _environment = clock.environment(0).enter();
        let policy = Retention::new(4096, 1).unwrap();
        let owned = Rc::new(Cell::new(true));
        let calls = Rc::new(Cell::new(0));
        policy.set_ownership_pass(Rc::new({
            let owned = owned.clone();
            let calls = calls.clone();
            move || {
                calls.set(calls.get() + 1);
                let owned = owned.get();
                Rc::new(move |_| owned)
            }
        }));
        policy.track(&page(0));
        policy.touch(&page(0));
        let now = uring_runtime::environment::now();
        let scoring = policy.scoring_at(now, u64::MAX);
        owned.set(false);
        for _ in 0..256 {
            assert_eq!(scoring.score(&page(0)), 2);
            assert_eq!(scoring.score(&page(1)), 1);
        }
        assert_eq!(calls.get(), 1);
        assert_eq!(
            policy
                .scoring_at(now + Duration::from_secs(60), u64::MAX)
                .score(&page(0)),
            0
        );
        assert_eq!(calls.get(), 2);
        policy.set_ownership(Rc::new(|_| true));
        assert_eq!(policy.scoring_at(now, u64::MAX).score(&page(1)), 1);
        assert_eq!(calls.get(), 2);
        let disabled = Retention::configured(4096, 1, PERIOD, false).unwrap();
        disabled.set_ownership(Rc::new(|_| true));
        assert_eq!(disabled.scoring_at(now, u64::MAX).score(&page(0)), 0);
    }
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
