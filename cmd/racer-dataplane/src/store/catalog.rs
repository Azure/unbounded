//! Identity mappings, segment lifecycle, and lease-fenced bounded value eviction.
use crate::error::Error;
use crate::error::Result;
use crate::model::CurrentVersion;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::retention::Retention;
use crate::runtime::HashMap;
use crate::runtime::HashSet;
use page_alloc::Extent;
use page_alloc::Generation;
use page_alloc::SegmentId;
use page_alloc::Segments;
use racer_control_wire::KeyId;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::rc::Rc;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RecordLocation {
    pub segment: SegmentId,
    pub generation: Generation,
    pub extent: Extent,
}
/// One storage shard. The catalog budget is independent of retained page entries;
/// evicting a catalog item cannot remove a page's attached immutable descriptor.
pub struct Index {
    worker: WorkerId,

    metadata_capacity: usize,

    page_capacity: Cell<usize>,

    state: RefCell<State>,

    availability: Rc<crate::control::Availability>,

    reserved: Cell<usize>,

    residency: Rc<Residency>,

    active_writes: Rc<RefCell<HashMap<SegmentId, usize>>>,
}
/// Production mappings use randomized hashing, as did the original catalog.
#[cfg(not(test))]
type Pages = page_alloc::index::PageIndex<PageId, IndexedPage>;
/// Simulation keeps its deterministic hash state across the extraction boundary.
#[cfg(test)]
type Pages = page_alloc::index::PageIndex<PageId, IndexedPage, crate::runtime::HashState>;

#[derive(Default)]
struct State {
    pages: Pages,

    versions: HashMap<ObjectVersion, (VersionMetadata, usize)>,

    metadata: HashMap<ObjectVersion, VersionMetadata>,

    order: VecDeque<ObjectVersion>,

    current: HashMap<ObjectId, CurrentVersion>,

    residents: HashMap<PageId, Resident>,
}
/// One worker's exact union of pending and indexed pages. Guards release heat
/// only when the final storage owner disappears, including cancellation/drop.
struct Residency {
    retention: RefCell<Rc<Retention>>,
    counts: RefCell<HashMap<PageId, usize>>,
}
pub(super) struct Resident {
    residency: Rc<Residency>,
    page: PageId,
    payload: Option<crate::retention::Payload>,
}
pub(super) struct PublicationGuard {
    active: Rc<RefCell<HashMap<SegmentId, usize>>>,
    segment: SegmentId,
}
impl Drop for PublicationGuard {
    fn drop(&mut self) {
        let mut active = self.active.borrow_mut();
        let count = active
            .get_mut(&self.segment)
            .expect("active publication owner");
        *count -= 1;
        if *count == 0 {
            active.remove(&self.segment);
        }
    }
}
impl Drop for Resident {
    fn drop(&mut self) {
        let mut counts = self.residency.counts.borrow_mut();
        let count = counts.get_mut(&self.page).expect("tracked storage owner");
        *count -= 1;
        if *count == 0 {
            counts.remove(&self.page);
            self.residency.retention.borrow().forget(&self.page);
        }
    }
}
/// Capacity ownership acquired before a disk SQE. Drop releases unused capacity.
pub struct PageTicket {
    index: Rc<Index>,
}
impl Drop for PageTicket {
    fn drop(&mut self) {
        self.index.reserved.set(self.index.reserved.get() - 1);
    }
}
impl PageTicket {
    pub fn publish(self, page: PageId, entry: IndexedPage) -> Result<()> {
        let index = self.index.clone();
        drop(self);
        index.publish(page, entry)
    }
}
#[derive(Clone, Debug)]
pub struct IndexedPage {
    pub location: RecordLocation,
    pub metadata: VersionMetadata,
    pub key_id: KeyId,
}
#[derive(Clone)]
pub struct IndexSnapshot {
    /// Every entry owns its descriptor: no cross-shard catalog reference can dangle.
    pub entries: Vec<(PageId, IndexedPage)>,
    /// Bounded standalone descriptors, including HEAD-only and zero-length objects,
    /// on their page-zero owner. Current-version freshness is never recovered.
    pub metadata: Vec<VersionMetadata>,
}
impl IndexSnapshot {
    /// Structural checkpoint validation, in addition to checksum, ownership,
    /// capacity, geometry, and generation checks performed during recovery.
    pub fn validate_metadata(&self) -> Result<()> {
        let mut lengths: HashMap<&ObjectVersion, &VersionMetadata> = HashMap::default();
        for metadata in self
            .metadata
            .iter()
            .chain(self.entries.iter().map(|(_, entry)| &entry.metadata))
        {
            if let Some(old) = lengths.get(&metadata.version)
                && !old.compatible(metadata)
            {
                return Err(Error::CorruptRecord);
            }
            lengths.insert(&metadata.version, metadata);
        }
        for (page, entry) in &self.entries {
            entry.metadata.page_length(page)?;
        }
        Ok(())
    }
}
impl Index {
    pub fn new(
        worker: WorkerId,
        metadata_capacity: usize,
        availability: Rc<crate::control::Availability>,
    ) -> Self {
        Self {
            worker,
            metadata_capacity,
            page_capacity: Cell::new(65536),
            state: RefCell::new(State::default()),
            availability,
            reserved: Cell::new(0),
            residency: Rc::new(Residency {
                retention: RefCell::new(Rc::new(
                    Retention::new(256 * 1024, 65536 + 64).expect("valid bounded retention"),
                )),
                counts: RefCell::new(HashMap::default()),
            }),
            active_writes: Rc::new(RefCell::new(HashMap::default())),
        }
    }
    fn available(&self, cache: &racer_control_wire::CacheId) -> bool {
        self.availability.metadata(cache)
    }
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
    pub fn metadata_capacity(&self) -> usize {
        self.metadata_capacity
    }
    pub fn page_capacity(&self) -> usize {
        self.page_capacity.get()
    }
    pub fn set_page_capacity(&self, capacity: usize) -> Result<()> {
        if capacity == 0 || capacity < self.state.borrow().pages.len() + self.reserved.get() {
            return Err(Error::InvalidConfiguration);
        }
        self.page_capacity.set(capacity);
        Ok(())
    }
    pub fn lookup(&self, page: &PageId) -> Result<Option<IndexedPage>> {
        Ok(self
            .state
            .borrow()
            .pages
            .get(page)
            .filter(|e| self.availability.page(&page.version.object.cache, e.key_id))
            .cloned())
    }
    /// Allocation-free capacity preflight, not a slot reservation. Replacements
    /// remain admissible at capacity; new pages require an existing free slot.
    pub fn preflight_capacity(&self, page: &PageId) -> Result<()> {
        let state = self.state.borrow();
        if state.pages.contains_key(page)
            || state.pages.len() + self.reserved.get() < self.page_capacity.get()
        {
            Ok(())
        } else {
            Err(Error::Overloaded)
        }
    }
    pub fn retention(&self) -> Rc<Retention> {
        self.residency.retention.borrow().clone()
    }
    pub(super) fn publishing(&self, segment: SegmentId) -> PublicationGuard {
        *self.active_writes.borrow_mut().entry(segment).or_default() += 1;
        PublicationGuard {
            active: self.active_writes.clone(),
            segment,
        }
    }
    pub fn set_retention(&self, retention: Rc<Retention>) {
        // Startup wiring only: replacing a live policy would invalidate read-side
        // clones and require a capacity-sized heat migration in a request turn.
        assert!(
            self.residency.counts.borrow().is_empty(),
            "install retention before residency"
        );
        *self.residency.retention.borrow_mut() = retention;
    }
    pub(super) fn track(&self, page: &PageId) -> Result<Resident> {
        let mut counts = self.residency.counts.borrow_mut();
        if !self.retention().track(page) {
            return Err(Error::Overloaded);
        }
        *counts.entry(page.clone()).or_default() += 1;
        Ok(Resident {
            residency: self.residency.clone(),
            page: page.clone(),
            payload: None,
        })
    }
    /// Scan at most 64 candidates with a separate cursor. Publication ordering is
    /// never changed by retention, so checkpoint traversal remains monotonic.
    fn victim(&self, state: &State) -> Option<PageId> {
        let retention = self.retention();
        state
            .pages
            .victim(64, |page, _| u64::from(retention.score(page)))
    }
    /// Reserve one potential new mapping, optionally evicting a low-value mapping.
    /// Replacement writes also reserve: concurrent invalidation may remove the old
    /// mapping while the SQE is in flight. No segment bytes are recycled here.
    pub fn reserve_page(self: &Rc<Self>, evict: bool) -> Result<PageTicket> {
        let mut state = self.state.borrow_mut();
        if state.pages.len() + self.reserved.get() >= self.page_capacity.get() {
            if !evict {
                return Err(Error::Overloaded);
            }
            let victim = self.victim(&state).ok_or(Error::Overloaded)?;
            self.retention().evicted(
                &victim,
                u64::from(
                    state
                        .pages
                        .get(&victim)
                        .expect("victim mapping")
                        .metadata
                        .page_length(&victim)?,
                ),
                false,
            );
            Self::remove_page(&mut state, &victim);
        }
        self.reserved.set(self.reserved.get() + 1);
        Ok(PageTicket {
            index: self.clone(),
        })
    }
    /// Atomically publish a completed record with its immutable descriptor. Reject
    /// conflicting lengths for one version; never update current-version freshness.
    pub fn publish(&self, page: PageId, entry: IndexedPage) -> Result<()> {
        if !self
            .availability
            .page(&page.version.object.cache, entry.key_id)
        {
            return Ok(());
        }
        Self::validate_descriptor(&entry.metadata)?;
        let payload_bytes = u64::from(entry.metadata.page_length(&page)?);
        let mut state = self.state.borrow_mut();
        Self::check_length(&state, &entry.metadata)?;
        if !state.pages.contains_key(&page)
            && state.pages.len() + self.reserved.get() >= self.page_capacity.get()
        {
            return Err(Error::Overloaded);
        }
        // Fail before removing a mapping or acquiring a residency owner.
        if !state.pages.can_publish() {
            return Err(Error::Unavailable);
        }
        // Acquire first: replacement retains its existing heat without a gap.
        let mut resident = self.track(&page)?;
        resident.payload = Some(self.retention().payload(payload_bytes, true));
        Self::remove_page(&mut state, &page);
        let version = state
            .versions
            .entry(page.version.clone())
            .or_insert((entry.metadata.clone(), 0));
        version.1 += 1;
        let segment = entry.location.segment;
        state
            .pages
            .insert(page, entry, segment)
            .expect("preflighted publication age");
        state.residents.insert(resident.page.clone(), resident);
        Ok(())
    }
    /// Look in both the standalone catalog and retained page entries for this exact
    /// version. This operation does not consult TTL or substitute another ETag.
    pub fn version(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        if !self.available(&version.object.cache) {
            return Ok(None);
        }
        let s = self.state.borrow();
        let descriptor = s.metadata.get(version).cloned().or_else(|| {
            s.versions
                .get(version)
                .map(|(metadata, _)| metadata.clone())
        });
        Ok(descriptor)
    }
    /// Page-zero owner only. Supports metadata-only objects without a dirty page,
    /// slab allocation, encryption record, or ciphertext reservation.
    pub fn publish_version(&self, metadata: VersionMetadata) -> Result<()> {
        if !self.available(&metadata.version.object.cache) {
            return Ok(());
        }
        Self::validate_descriptor(&metadata)?;
        let mut s = self.state.borrow_mut();
        Self::check_length(&s, &metadata)?;
        if self.metadata_capacity == 0 {
            return Err(Error::Overloaded);
        }
        if !s.metadata.contains_key(&metadata.version) {
            while s.metadata.len() >= self.metadata_capacity {
                Self::evict_one(&mut s);
            }
            s.order.push_back(metadata.version.clone());
        }
        s.metadata.insert(metadata.version.clone(), metadata);
        Ok(())
    }
    pub fn current(&self, object: &ObjectId) -> Result<Option<CurrentVersion>> {
        if !self.available(&object.cache) {
            return Ok(None);
        }
        Ok(self
            .state
            .borrow()
            .current
            .get(object)
            .filter(|v| uring_runtime::environment::wall_now() < v.expires_at.as_system_time())
            .cloned())
    }
    /// Page-zero owner only, after fresh revalidation. Atomically retain the
    /// immutable descriptor and advance the volatile pointer; reject length conflicts.
    /// A zero-TTL observation is returned to its waiters without a reusable hit.
    pub fn publish_current(&self, metadata: ObjectMetadata) -> Result<()> {
        if !self.available(&metadata.version.object.cache) {
            return Ok(());
        }
        self.publish_version(metadata.immutable())?;
        let mut s = self.state.borrow_mut();
        s.current.remove(&metadata.version.object);
        if uring_runtime::environment::wall_now() < metadata.expires_at.as_system_time() {
            s.current.insert(
                metadata.version.object.clone(),
                CurrentVersion {
                    version: metadata.version,
                    expires_at: metadata.expires_at,
                },
            );
        }
        Ok(())
    }
    /// Drop volatile freshness on clock uncertainty, preserving version descriptors.
    pub fn invalidate_freshness(&self) -> Result<()> {
        self.state.borrow_mut().current.clear();
        Ok(())
    }
    /// Reclaim standalone catalog entries and any pointers depending on them.
    /// Page-attached descriptors follow page eviction instead; no page is removed.
    pub fn evict_metadata(&self, entries: usize) -> Result<usize> {
        let mut s = self.state.borrow_mut();
        let count = entries.min(s.metadata.len());
        for _ in 0..count {
            Self::evict_one(&mut s);
        }
        Ok(count)
    }
    /// Compare the complete mapping before removing, preserving replacement writes.
    pub fn remove_if_matches(&self, page: &PageId, location: &RecordLocation) {
        let mut s = self.state.borrow_mut();
        if s.pages.get(page).is_some_and(|p| &p.location == location) {
            Self::remove_page(&mut s, page);
        }
    }
    pub fn snapshot(&self) -> Result<IndexSnapshot> {
        let s = self.state.borrow();
        Ok(IndexSnapshot {
            entries: s
                .pages
                .iter()
                .map(|(p, e)| (p.clone(), e.clone()))
                .collect(),
            metadata: s.metadata.values().cloned().collect(),
        })
    }
    /// Owner-local bounded cut. Appends are frozen by Checkpointer; removals are
    /// safe omissions. The age tree avoids rescanning a growing hash table.
    pub fn snapshot_pages(&self, after: u64, limit: usize) -> (u64, Vec<(PageId, IndexedPage)>) {
        let state = self.state.borrow();
        let mut cursor = after;
        let entries = state
            .pages
            .after(after, limit)
            .map(|(age, page, entry)| {
                cursor = age;
                (page.clone(), entry.clone())
            })
            .collect();
        (cursor, entries)
    }
    pub fn snapshot_metadata(&self) -> Vec<VersionMetadata> {
        self.state.borrow().metadata.values().cloned().collect()
    }
    /// Install only with the matching recovered segment state before admission.
    /// Validate identity/length agreement and catalog bounds; clear all freshness.
    pub fn restore(&self, snapshot: IndexSnapshot) -> Result<()> {
        self.validate_snapshot(&snapshot)?;
        let mut replacement = Index::new(
            self.worker,
            self.metadata_capacity,
            self.availability.clone(),
        );
        replacement.residency = self.residency.clone();
        replacement.set_page_capacity(self.page_capacity.get())?;
        for (p, e) in snapshot.entries {
            replacement.publish(p, e)?;
        }
        for m in snapshot.metadata {
            replacement.publish_version(m)?;
        }
        *self.state.borrow_mut() = replacement.state.into_inner();
        Ok(())
    }
    pub fn validate_snapshot(&self, snapshot: &IndexSnapshot) -> Result<()> {
        snapshot.validate_capacity(self.page_capacity.get(), self.metadata_capacity)
    }
}
impl IndexSnapshot {
    pub fn validate_capacity(&self, page_capacity: usize, metadata_capacity: usize) -> Result<()> {
        let snapshot = self;
        // Reject downsized cuts before allocating descriptor/duplicate tables.
        if snapshot.entries.len() > page_capacity || snapshot.metadata.len() > metadata_capacity {
            return Err(Error::CorruptRecord);
        }
        snapshot.validate_metadata()?;
        for m in snapshot
            .metadata
            .iter()
            .chain(snapshot.entries.iter().map(|(_, e)| &e.metadata))
        {
            Index::validate_descriptor(m)?;
        }
        let mut pages = HashSet::default();
        let mut versions = HashSet::default();
        if snapshot.entries.iter().any(|(p, _)| !pages.insert(p))
            || snapshot
                .metadata
                .iter()
                .any(|m| !versions.insert(&m.version))
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
}
impl Index {
    pub fn segment_entries_bounded(
        &self,
        segment: SegmentId,
        budget: usize,
    ) -> Vec<(PageId, RecordLocation)> {
        let s = self.state.borrow();
        s.pages
            .segment(segment, budget)
            .map(|(p, e)| (p.clone(), e.location.clone()))
            .collect()
    }
    pub fn segment_empty(&self, segment: SegmentId) -> bool {
        self.state.borrow().pages.segment_empty(segment)
    }
    /// Sum live payload value, not padded disk size. At most 256 mappings are
    /// inspected per segment. Larger segments use a conservative upper bound for
    /// the unseen suffix; this is a preference, never an eviction exemption.
    fn segment_score(&self, segment: SegmentId) -> u64 {
        let state = self.state.borrow();
        let retention = self.retention();
        state
            .pages
            .segment_score(segment, 256, crate::model::PAGE_BYTES * 4, |page, entry| {
                let bytes = entry
                    .metadata
                    .page_length(page)
                    .unwrap_or(crate::model::PAGE_BYTES as u32);
                u64::from(bytes) * u64::from(retention.score(page))
            })
    }
    pub fn remove_cache(&self, cache: &racer_control_wire::CacheId) {
        let mut s = self.state.borrow_mut();
        let pages: Vec<_> = s
            .pages
            .keys()
            .filter(|p| &p.version.object.cache == cache)
            .cloned()
            .collect();
        for p in pages {
            Self::remove_page(&mut s, &p);
        }
        s.metadata.retain(|v, _| &v.object.cache != cache);
        s.order.retain(|v| &v.object.cache != cache);
        s.current.retain(|o, _| &o.cache != cache);
    }
    fn check_length(s: &State, m: &VersionMetadata) -> Result<()> {
        if s.versions
            .get(&m.version)
            .is_some_and(|(v, _)| !v.compatible(m))
            || s.metadata.get(&m.version).is_some_and(|v| !v.compatible(m))
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
    fn validate_descriptor(m: &VersionMetadata) -> Result<()> {
        if m.version.object.cache.0.is_empty()
            || m.version.object.cache.0.len() > super::MAX_ID_BYTES
            || m.version.etag.as_bytes().is_empty()
            || m.version.etag.as_bytes().len() > super::MAX_ETAG_BYTES
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
    fn remove_page(s: &mut State, page: &PageId) {
        if s.pages.remove(page).is_some() {
            s.residents.remove(page);
            if let Some((_, count)) = s.versions.get_mut(&page.version) {
                *count -= 1;
                if *count == 0 {
                    s.versions.remove(&page.version);
                }
            }
        }
    }
    fn evict_one(s: &mut State) {
        if let Some(version) = s.order.pop_front() {
            s.metadata.remove(&version);
            if s.current
                .get(&version.object)
                .is_some_and(|c| c.version == version)
            {
                s.current.remove(&version.object);
            }
        }
    }
}
/// Per-worker bounded value selection, with no payload compaction.
pub struct SegmentClock {
    index: Rc<Index>,
    free_reserve: usize,
    budget: ReclaimBudget,
    clock: page_alloc::SegmentClock,
}

/// Per-call reactor work limits, selected by the application, not the allocator.
#[derive(Clone, Copy, Debug)]
pub struct ReclaimBudget {
    pub segment_visits: usize,
    pub mapping_removals: usize,
}

/// Bound foreground reclamation so a large disk index cannot monopolize a turn.
pub const FOREGROUND_RECLAIM_BUDGET: ReclaimBudget = ReclaimBudget {
    segment_visits: 64,
    mapping_removals: 256,
};
impl page_alloc::SegmentEntries for Index {
    fn can_evict(&self, segment: SegmentId) -> bool {
        !self.active_writes.borrow().contains_key(&segment)
    }
    fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize {
        let entries = self.segment_entries_bounded(segment, budget);
        let removed = entries.len();
        for (page, location) in entries {
            // This synchronous bounded traversal cannot interleave with replacement.
            let bytes = self
                .state
                .borrow()
                .pages
                .get(&page)
                .expect("indexed page")
                .metadata
                .page_length(&page)
                .expect("validated indexed page");
            self.retention().evicted(&page, u64::from(bytes), true);
            self.remove_if_matches(&page, &location);
        }
        removed
    }
    fn is_empty(&self, segment: SegmentId) -> bool {
        self.segment_empty(segment)
    }
}
impl SegmentClock {
    pub fn reserve(&self) -> usize {
        self.free_reserve
    }
    pub fn new(index: Rc<Index>, segments: Rc<Segments>, free_reserve: usize) -> Self {
        Self::with_budget(index, segments, free_reserve, FOREGROUND_RECLAIM_BUDGET)
    }
    pub fn with_budget(
        index: Rc<Index>,
        segments: Rc<Segments>,
        free_reserve: usize,
        budget: ReclaimBudget,
    ) -> Self {
        Self {
            index,
            free_reserve,
            budget,
            clock: page_alloc::SegmentClock::new(segments),
        }
    }
    pub fn mark_read(&self, segment: SegmentId) -> Result<()> {
        self.clock.mark_read(segment).map_err(Into::into)
    }
    /// Make index room independently of slab space, with at most two rotations.
    /// Use the same segment-level second chance as payload reclamation, but only
    /// forget mappings: even an open segment can lose its index entries safely.
    /// Its bytes and generation stay intact until normal lease-fenced recycling.
    pub fn reclaim_index_for(&self, page: &PageId) -> Result<()> {
        self.clock
            .reclaim_index(
                &*self.index,
                self.budget.segment_visits.min(self.budget.mapping_removals),
                || self.index.preflight_capacity(page).is_ok(),
            )
            .map_err(Into::into)
    }
    /// Rank at most 64 slots before eviction. Busy segments remain Evicting until
    /// a later poll; ownership and heat cannot prevent eventual reclamation.
    pub fn reclaim_now(&self) -> Result<()> {
        self.clock
            .reclaim_scored(
                &*self.index,
                self.free_reserve,
                self.budget.segment_visits,
                self.budget.mapping_removals,
                |segment| self.index.segment_score(segment),
            )
            .map_err(Into::into)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::StrongEtag;
    use racer_control_wire::CacheId;

    fn segments(bytes: u64, count: usize) -> Segments {
        let s = Segments::new(bytes);
        s.configure(
            bytes * count as u64,
            count,
            page_alloc::Alignment::new(512, 512, 512).unwrap(),
        )
        .unwrap();
        s
    }
    #[test]
    fn busy_victim_waits_and_clock_makes_progress() {
        let segments = Rc::new(segments(512, 2));
        let held = segments.append(512).unwrap();
        drop(segments.append(512).unwrap());
        let clock = SegmentClock::new(
            Rc::new(Index::new(
                WorkerId(0),
                1,
                crate::test_support::availability(),
            )),
            segments.clone(),
            2,
        );
        clock.mark_read(SegmentId(1)).unwrap();
        assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
        assert_eq!(segments.free_count(), 1);
        drop(held);
        clock.reclaim_now().unwrap();
        assert_eq!(segments.free_count(), 2);
    }

    #[test]
    fn caller_selected_zero_budget_preserves_allocations_and_index() {
        let segments = Rc::new(segments(512, 2));
        drop(segments.append(512).unwrap());
        let index = Rc::new(index(1));
        let clock = SegmentClock::with_budget(
            index,
            segments.clone(),
            2,
            ReclaimBudget {
                segment_visits: 0,
                mapping_removals: 0,
            },
        );
        assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
        assert_eq!(segments.free_count(), 1);
        assert_eq!(
            segments.state(SegmentId(0)).unwrap(),
            page_alloc::SegmentState::Sealed
        );
    }
    fn index(capacity: usize) -> Index {
        Index::new(WorkerId(0), capacity, crate::test_support::availability())
    }
    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::test_support::security::CACHE.into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
        }
    }
    fn indexed(metadata: VersionMetadata, segment: u64) -> (PageId, IndexedPage) {
        (
            PageId {
                version: metadata.version.clone(),
                number: crate::model::PageNumber(0),
            },
            IndexedPage {
                metadata,
                key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                location: RecordLocation {
                    segment: SegmentId(segment),
                    generation: Generation(1),
                    extent: Extent::new(segment * 1024, 512).unwrap(),
                },
            },
        )
    }
    #[test]
    fn disk_observability_index_lifecycle_and_victims() {
        use page_alloc::SegmentEntries;
        let index = Rc::new(index(4));
        index.set_page_capacity(1).unwrap();
        let retention = index.retention();
        let (page, entry) = indexed(descriptor("metrics", 17), 0);
        index.publish(page.clone(), entry.clone()).unwrap();
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
        index.publish(page.clone(), entry.clone()).unwrap();
        index.restore(index.snapshot().unwrap()).unwrap();
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
        assert_eq!(
            retention.snapshot().disk[0].published_pages,
            0,
            "restore is not writer publication"
        );
        let (other, other_entry) = indexed(descriptor("other", 3), 1);
        assert_eq!(
            index.publish(other.clone(), other_entry.clone()),
            Err(Error::Overloaded)
        );
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
        retention.set_ownership(Rc::new(|_| true));
        drop(index.reserve_page(true).unwrap());
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
        assert_eq!(retention.snapshot().disk[1].index_evicted_payload_bytes, 17);
        index.publish(other.clone(), other_entry.clone()).unwrap();
        retention.set_ownership(Rc::new(|_| false));
        assert_eq!(index.remove_bounded(SegmentId(1), 1), 1);
        assert_eq!(index.remove_bounded(SegmentId(1), 1), 0);
        assert_eq!(retention.snapshot().disk[0].segment_evicted_pages, 1);
        assert_eq!(
            retention.snapshot().disk[0].segment_evicted_payload_bytes,
            3
        );
        index.publish(other.clone(), other_entry.clone()).unwrap();
        index.remove_if_matches(&other, &entry.location);
        assert_eq!(retention.snapshot().indexed_payload_bytes, 3);
        index.remove_cache(&other.version.object.cache);
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
        assert_eq!(retention.snapshot().disk[0].segment_evicted_pages, 1);
        index.publish(page, entry).unwrap();
        drop(index);
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
    }
    #[test]
    fn content_type_is_immutable_including_absence() {
        let legacy = descriptor("v1", 3);
        let mut typed = legacy.clone();
        typed.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
        for (first, second) in [(legacy.clone(), typed.clone()), (typed.clone(), legacy)] {
            let index = index(8);
            index.publish_version(first.clone()).unwrap();
            assert_eq!(index.publish_version(second), Err(Error::CorruptRecord));
            index.publish_version(first.clone()).unwrap();
            assert_eq!(index.version(&first.version).unwrap(), Some(first.clone()));
            assert_eq!(first.for_pin().content_type, first.content_type);
            let mut conflict = typed.clone();
            conflict.content_type = Some(crate::model::ContentType::parse(b"text/html").unwrap());
            assert_eq!(index.publish_version(conflict), Err(Error::CorruptRecord));
        }
    }
    #[test]
    fn second_sight_index_owned_bias_is_soft_and_snapshot_order_is_unchanged() {
        for hot_nonowned in [false, true] {
            let index = Rc::new(index(4));
            index.set_page_capacity(2).unwrap();
            let (owned, first) = indexed(descriptor("owned", 17), 0);
            let (other, second) = indexed(descriptor("other", 17), 1);
            let owner = owned.clone();
            let retention = index.retention();
            retention.set_ownership(Rc::new(move |page| page == &owner));
            index.publish(owned.clone(), first).unwrap();
            index.publish(other.clone(), second).unwrap();
            if hot_nonowned {
                for _ in 0..3 {
                    retention.touch(&other);
                }
            }
            let before = index.snapshot_pages(0, 1);
            let ticket = index.reserve_page(true).unwrap();
            assert_eq!(index.lookup(&owned).unwrap().is_some(), !hot_nonowned);
            assert_eq!(index.lookup(&other).unwrap().is_some(), hot_nonowned);
            if hot_nonowned {
                let after = index.snapshot_pages(before.0, 2);
                assert_eq!(after.1.len(), 1);
                assert_eq!(after.1[0].0, other);
            }
            drop(ticket);
        }
    }
    #[test]
    fn second_sight_index_candidate_cursor_is_bounded_and_ownership_is_current() {
        let index = Rc::new(index(1));
        index.set_page_capacity(65).unwrap();
        let retention = index.retention();
        let calls = Rc::new(Cell::new(0));
        let counted = calls.clone();
        let owned = Rc::new(Cell::new(true));
        let current = owned.clone();
        retention.set_ownership(Rc::new(move |_| {
            counted.set(counted.get() + 1);
            current.get()
        }));
        for i in 0..65 {
            let (page, entry) = indexed(descriptor(&format!("v{i}"), 17), 0);
            index.publish(page, entry).unwrap();
        }
        let ticket = index.reserve_page(true).unwrap();
        assert_eq!(
            calls.get(),
            65,
            "64 candidates plus one victim classification"
        );
        drop(ticket);
        let (page, entry) = indexed(descriptor("new", 17), 0);
        index.publish(page.clone(), entry).unwrap();
        owned.set(false);
        let ticket = index.reserve_page(true).unwrap();
        assert_eq!(calls.get(), 130);
        // Cursor progression and wrap are asserted in page_alloc::index tests.
        assert_eq!(retention.score(&page), 0);
        drop(ticket);
    }
    #[test]
    fn second_sight_mixed_segments_use_live_payload_value_not_padding_or_last_read() {
        let segments = Rc::new(segments(1024, 3));
        for _ in 0..3 {
            drop(segments.append(1024).unwrap());
        }
        let index = Rc::new(index(8));
        let retention = index.retention();
        let (owned, owned_entry) = indexed(descriptor("owned", 100), 0);
        let (cold, cold_entry) = indexed(descriptor("cold", 800), 0);
        let (hot, hot_entry) = indexed(descriptor("hot", 100), 1);
        let (small, small_entry) = indexed(descriptor("small-hot", 10), 2);
        let owner = owned.clone();
        let small_owner = small.clone();
        retention.set_ownership(Rc::new(move |page| page == &owner || page == &small_owner));
        for (page, entry) in [
            (owned.clone(), owned_entry),
            (cold, cold_entry),
            (hot.clone(), hot_entry),
            (small.clone(), small_entry),
        ] {
            index.publish(page, entry).unwrap();
        }
        for _ in 0..3 {
            retention.touch(&hot);
        }
        assert_eq!(index.segment_score(SegmentId(0)), 100);
        assert_eq!(index.segment_score(SegmentId(1)), 300);
        assert_eq!(index.segment_score(SegmentId(2)), 10);
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        clock.mark_read(SegmentId(2)).unwrap();
        clock.reclaim_now().unwrap();
        assert!(index.lookup(&small).unwrap().is_none());
        assert!(index.lookup(&owned).unwrap().is_some());
        let clock = SegmentClock::new(index.clone(), segments.clone(), 2);
        clock.reclaim_now().unwrap();
        assert!(index.lookup(&owned).unwrap().is_none());
        assert!(index.lookup(&hot).unwrap().is_some());
    }
    #[test]
    fn second_sight_segment_equal_heat_owned_bias_and_bounded_large_segments() {
        let segments = Rc::new(segments(1024, 2));
        for _ in 0..2 {
            drop(segments.append(1024).unwrap());
        }
        let index = Rc::new(index(1));
        let (owned, first) = indexed(descriptor("owned", 17), 0);
        let (other, second) = indexed(descriptor("other", 17), 1);
        let owner = owned.clone();
        let retention = index.retention();
        retention.set_ownership(Rc::new(move |page| page == &owner));
        index.publish(owned.clone(), first).unwrap();
        index.publish(other.clone(), second).unwrap();
        SegmentClock::new(index.clone(), segments.clone(), 1)
            .reclaim_now()
            .unwrap();
        assert!(index.lookup(&owned).unwrap().is_some());
        assert!(index.lookup(&other).unwrap().is_none());
        for i in 0..256 {
            let (page, entry) = indexed(descriptor(&format!("extra{i}"), 17), 0);
            index.publish(page, entry).unwrap();
        }
        let calls = Rc::new(Cell::new(0));
        let counted = calls.clone();
        retention.set_ownership(Rc::new(move |_| {
            counted.set(counted.get() + 1);
            true
        }));
        assert_eq!(
            index.segment_score(SegmentId(0)),
            256 * 17 + crate::model::PAGE_BYTES * 4
        );
        assert_eq!(calls.get(), 256);
        // A conservative suffix is still evictable, and removal respects 256 per turn.
        let clock = SegmentClock::new(index.clone(), segments.clone(), 2);
        assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
        assert_eq!(index.snapshot().unwrap().entries.len(), 1);
        clock.reclaim_now().unwrap();
        assert!(index.snapshot().unwrap().entries.is_empty());
    }
    #[test]
    fn second_sight_residency_replacement_restore_purge_and_drop_are_balanced() {
        let index = index(4);
        let retention = index.retention();
        let (page, entry) = indexed(descriptor("resident", 17), 0);
        let pending = index.track(&page).unwrap();
        retention.touch(&page);
        index.publish(page.clone(), entry.clone()).unwrap();
        assert_eq!(retention.snapshot().heat_entries, 1);
        assert_eq!(retention.score(&page), 1);
        let (_, replacement) = indexed(entry.metadata.clone(), 1);
        index.publish(page.clone(), replacement.clone()).unwrap();
        assert_eq!(retention.score(&page), 1);
        index.remove_if_matches(&page, &entry.location);
        assert_eq!(retention.snapshot().heat_entries, 1);
        let snapshot = index.snapshot().unwrap();
        index.restore(snapshot).unwrap();
        assert_eq!(retention.score(&page), 1);
        index.remove_cache(&page.version.object.cache);
        assert_eq!(
            retention.snapshot().heat_entries,
            1,
            "pending still owns heat"
        );
        drop(pending);
        assert_eq!(retention.snapshot().heat_entries, 0);
        index.publish(page.clone(), entry).unwrap();
        assert_eq!(retention.score(&page), 0);
        drop(index);
        assert_eq!(retention.snapshot().heat_entries, 0);
    }
    #[test]
    fn second_sight_heat_capacity_and_failed_restore_leave_existing_residency_intact() {
        let index = index(4);
        let retention = Rc::new(Retention::new(4096, 1).unwrap());
        index.set_retention(retention.clone());
        let (page, entry) = indexed(descriptor("resident", 17), 0);
        index.publish(page.clone(), entry.clone()).unwrap();
        retention.touch(&page);
        let (other, other_entry) = indexed(descriptor("other", 17), 1);
        assert!(matches!(index.track(&other), Err(Error::Overloaded)));
        assert_eq!(
            index.publish(other.clone(), other_entry.clone()),
            Err(Error::Overloaded)
        );
        assert_eq!(
            index.restore(IndexSnapshot {
                entries: vec![(other, other_entry)],
                metadata: vec![]
            }),
            Err(Error::Overloaded)
        );
        assert!(index.lookup(&page).unwrap().is_some());
        assert_eq!(retention.snapshot().heat_entries, 1);
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
        assert_eq!(retention.score(&page), 1);
        // Exhausted publication ages reject replacement atomically in page_alloc::index.
        drop(index);
        assert_eq!(retention.snapshot().heat_entries, 0);
    }
    #[test]
    fn second_sight_sealed_unpublished_write_is_not_a_victim() {
        let segments = Rc::new(segments(1024, 3));
        let index = Rc::new(index(4));
        let (published, entry) = indexed(descriptor("published", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(published.clone(), entry).unwrap();
        index.retention().touch(&published);
        let (write, extent) = segments.append(1024).unwrap();
        let publication = index.publishing(write.id());
        let (other_write, _) = segments.append(1024).unwrap();
        let other_publication = index.publishing(other_write.id());
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        clock.reclaim_now().unwrap();
        assert!(index.lookup(&published).unwrap().is_none());
        assert_eq!(
            segments.state(write.id()),
            Ok(page_alloc::SegmentState::Sealed)
        );
        segments
            .validate(write.id(), write.generation(), &extent)
            .unwrap();
        let (page, mut entry) = indexed(descriptor("completed", 17), 1);
        entry.location.extent = extent;
        index.publish(page.clone(), entry).unwrap();
        drop(publication);
        drop(write);
        assert!(index.lookup(&page).unwrap().is_some());
        drop(other_publication);
        drop(other_write);
    }
    #[test]
    fn metadata_only_checkpoint_preserves_empty_and_old_versions_without_freshness() {
        let snapshot = IndexSnapshot {
            entries: vec![],
            metadata: vec![descriptor("old", 17), descriptor("new", 0)],
        };
        assert_eq!(snapshot.validate_metadata(), Ok(()));
        assert_eq!(snapshot.metadata[0].for_pin().length, 17);
        assert_eq!(snapshot.metadata[1].for_pin().length, 0);
        assert_eq!(
            snapshot.metadata[0].for_pin().expires_at.as_system_time(),
            std::time::UNIX_EPOCH
        );
    }
    #[test]
    fn checkpoint_rejects_two_lengths_for_one_version() {
        let first = descriptor("v1", 17);
        let conflicting = VersionMetadata {
            length: 18,
            ..first.clone()
        };
        let snapshot = IndexSnapshot {
            entries: vec![],
            metadata: vec![first, conflicting],
        };
        assert_eq!(snapshot.validate_metadata(), Err(Error::CorruptRecord));
    }
    #[test]
    fn catalog_eviction_preserves_pages_and_conditional_removal_preserves_replacement() {
        let index = index(1);
        index.set_page_capacity(1).unwrap();
        let (page, old) = indexed(descriptor("old", 17), 0);
        index.publish_version(old.metadata.clone()).unwrap();
        index.publish(page.clone(), old.clone()).unwrap();
        index.publish_version(descriptor("new", 0)).unwrap();
        assert_eq!(index.version(&page.version).unwrap().unwrap().length, 17);
        let (_, replacement) = indexed(old.metadata.clone(), 1);
        index.publish(page.clone(), replacement.clone()).unwrap();
        assert!(index.segment_empty(SegmentId(0)));
        index.remove_if_matches(&page, &old.location);
        assert_eq!(
            index.lookup(&page).unwrap().unwrap().location,
            replacement.location
        );
        let (other, entry) = indexed(descriptor("other", 1), 2);
        assert_eq!(index.publish(other, entry), Err(Error::Overloaded));
        index.remove_if_matches(&page, &replacement.location);
        assert!(index.version(&page.version).unwrap().is_none());
    }
    #[test]
    fn capacity_preflight_allows_replacement_and_reopens_only_after_removal() {
        let index = index(1);
        index.set_page_capacity(1).unwrap();
        let (page, entry) = indexed(descriptor("first", 17), 0);
        let (other, other_entry) = indexed(descriptor("other", 17), 1);
        assert_eq!(index.preflight_capacity(&page), Ok(()));
        assert_eq!(index.preflight_capacity(&other), Ok(()));
        assert!(index.snapshot().unwrap().entries.is_empty());
        index.publish(page.clone(), entry.clone()).unwrap();
        assert_eq!(index.preflight_capacity(&page), Ok(()));
        assert_eq!(index.preflight_capacity(&other), Err(Error::Overloaded));
        assert_eq!(
            index.publish(other.clone(), other_entry.clone()),
            Err(Error::Overloaded)
        );
        let (_, replacement) = indexed(entry.metadata, 2);
        index.publish(page.clone(), replacement.clone()).unwrap();
        assert_eq!(
            index.lookup(&page).unwrap().unwrap().location,
            replacement.location
        );
        assert_eq!(index.preflight_capacity(&other), Err(Error::Overloaded));
        index.remove_if_matches(&page, &replacement.location);
        assert_eq!(index.preflight_capacity(&other), Ok(()));
        index.publish(other, other_entry).unwrap();
    }
    #[test]
    fn restore_is_atomic_and_drops_freshness() {
        let index = index(2);
        let m = descriptor("v1", 17);
        let mut current = m.for_pin();
        current.expires_at = crate::model::ExpiresAt::from_unix_millis(
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_millis() as u64
                + 60_000,
        )
        .unwrap();
        index.publish_current(current).unwrap();
        assert!(index.current(&m.version.object).unwrap().is_some());
        let bad = IndexSnapshot {
            entries: vec![],
            metadata: vec![
                m.clone(),
                VersionMetadata {
                    content_type: None,
                    length: 99,
                    ..m.clone()
                },
            ],
        };
        assert_eq!(index.restore(bad), Err(Error::CorruptRecord));
        assert!(index.current(&m.version.object).unwrap().is_some());
        let snapshot = index.snapshot().unwrap();
        index.restore(snapshot).unwrap();
        assert!(index.current(&m.version.object).unwrap().is_none());
        assert_eq!(index.version(&m.version).unwrap(), Some(m));
    }
}
