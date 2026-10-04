//! Identity mappings, segment lifecycle, and lease-fenced second-chance eviction.
use crate::error::Error;
use crate::error::Result;
use crate::model::CurrentVersion;
use crate::model::KeyId;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::runtime::collections::HashMap;
use crate::runtime::collections::HashSet;
use page_alloc::Extent;
use page_alloc::Generation;
use page_alloc::SegmentId;
use page_alloc::SegmentState;
use page_alloc::Segments;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::BTreeMap;
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
}
#[derive(Default)]
struct State {
    pages: HashMap<PageId, IndexedPage>,
    reverse: HashMap<SegmentId, HashSet<PageId>>,
    versions: HashMap<ObjectVersion, (VersionMetadata, usize)>,
    metadata: HashMap<ObjectVersion, VersionMetadata>,
    order: VecDeque<ObjectVersion>,
    current: HashMap<ObjectId, CurrentVersion>,
    page_order: BTreeMap<u64, PageId>,
    page_age: HashMap<PageId, u64>,
    next_age: u64,
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
            if let Some(old) = lengths.get(&metadata.version) {
                if !old.compatible(metadata) {
                    return Err(Error::CorruptRecord);
                }
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
        }
    }
    fn available(&self, cache: &crate::model::CacheId) -> bool {
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
    /// Reserve one potential new mapping, optionally evicting one oldest mapping.
    /// Replacement writes also reserve: concurrent invalidation may remove the old
    /// mapping while the SQE is in flight. No segment bytes are recycled here.
    pub fn reserve_page(self: &Rc<Self>, evict: bool) -> Result<PageTicket> {
        let mut state = self.state.borrow_mut();
        if state.pages.len() + self.reserved.get() >= self.page_capacity.get() {
            if !evict {
                return Err(Error::Overloaded);
            }
            let victim = state
                .page_order
                .first_key_value()
                .map(|(_, p)| p.clone())
                .ok_or(Error::Overloaded)?;
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
        entry.metadata.page_length(&page)?;
        let mut state = self.state.borrow_mut();
        Self::check_length(&state, &entry.metadata)?;
        if !state.pages.contains_key(&page)
            && state.pages.len() + self.reserved.get() >= self.page_capacity.get()
        {
            return Err(Error::Overloaded);
        }
        Self::remove_page(&mut state, &page);
        let version = state
            .versions
            .entry(page.version.clone())
            .or_insert((entry.metadata.clone(), 0));
        version.1 += 1;
        state
            .reverse
            .entry(entry.location.segment)
            .or_default()
            .insert(page.clone());
        let age = state.next_age.checked_add(1).ok_or(Error::Unavailable)?;
        state.next_age = age;
        state.page_order.insert(age, page.clone());
        state.page_age.insert(page.clone(), age);
        state.pages.insert(page, entry);
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
        use std::ops::Bound::Excluded;
        use std::ops::Bound::Unbounded;
        let state = self.state.borrow();
        let mut cursor = after;
        let entries = state
            .page_order
            .range((Excluded(after), Unbounded))
            .take(limit)
            .map(|(age, page)| {
                cursor = *age;
                (page.clone(), state.pages[page].clone())
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
        let replacement = Index::new(
            self.worker,
            self.metadata_capacity,
            self.availability.clone(),
        );
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
        s.reverse
            .get(&segment)
            .into_iter()
            .flatten()
            .take(budget)
            .filter_map(|p| s.pages.get(p).map(|e| (p.clone(), e.location.clone())))
            .collect()
    }
    pub fn segment_empty(&self, segment: SegmentId) -> bool {
        !self.state.borrow().reverse.contains_key(&segment)
    }
    pub fn remove_cache(&self, cache: &crate::model::CacheId) {
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
        if let Some(old) = s.pages.remove(page) {
            if let Some(age) = s.page_age.remove(page) {
                s.page_order.remove(&age);
            }
            if let Some(set) = s.reverse.get_mut(&old.location.segment) {
                set.remove(page);
                if set.is_empty() {
                    s.reverse.remove(&old.location.segment);
                }
            }
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
/// Per-worker bounded second-chance clock, with no payload compaction.
pub struct SegmentClock {
    index: Rc<Index>,
    segments: Rc<Segments>,
    free_reserve: usize,
    hand: Cell<usize>,
    recent: RefCell<HashSet<SegmentId>>,
}
impl SegmentClock {
    pub fn reserve(&self) -> usize {
        self.free_reserve
    }
    pub fn new(index: Rc<Index>, segments: Rc<Segments>, free_reserve: usize) -> Self {
        Self {
            index,
            segments,
            free_reserve,
            hand: Cell::new(0),
            recent: RefCell::new(HashSet::default()),
        }
    }
    pub fn mark_read(&self, segment: SegmentId) -> Result<()> {
        if !matches!(
            self.segments.state(segment)?,
            SegmentState::Open | SegmentState::Sealed
        ) {
            return Err(Error::CorruptRecord);
        }
        self.recent.borrow_mut().insert(segment);
        Ok(())
    }
    /// Make index room independently of slab space, with at most two rotations.
    /// Use the same segment-level second chance as payload reclamation, but only
    /// forget mappings: even an open segment can lose its index entries safely.
    /// Its bytes and generation stay intact until normal lease-fenced recycling.
    pub fn reclaim_index_for(&self, page: &PageId) -> Result<()> {
        if self.index.preflight_capacity(page).is_ok() {
            return Ok(());
        }
        let count = self.segments.count();
        for _ in 0..count.saturating_mul(2).min(64) {
            let hand = self.hand.get() % count;
            self.hand.set((hand + 1) % count);
            let id = SegmentId(hand as u64);
            if self.recent.borrow_mut().remove(&id) {
                continue;
            }
            for (victim, location) in self.index.segment_entries_bounded(id, 1) {
                self.index.remove_if_matches(&victim, &location);
            }
            if self.index.preflight_capacity(page).is_ok() {
                return Ok(());
            }
        }
        Err(Error::Overloaded)
    }
    /// At most two rotations. Busy segments remain Evicting until a later poll.
    pub fn reclaim_now(&self) -> Result<()> {
        let count = self.segments.count();
        if count == 0 {
            return Err(Error::Unavailable);
        }
        let target = self.free_reserve.max(1).min(count);
        let mut free = self.segments.free_count();
        let mut entries_left = 256;
        for _ in 0..count.saturating_mul(2).min(64) {
            if free >= target {
                return Ok(());
            }
            let hand = self.hand.get() % count;
            self.hand.set((hand + 1) % count);
            let id = SegmentId(hand as u64);
            if !matches!(
                self.segments.state(id)?,
                SegmentState::Sealed | SegmentState::Evicting
            ) {
                continue;
            }
            if self.recent.borrow_mut().remove(&id) {
                continue;
            }
            self.segments.begin_evict(id)?;
            for (page, location) in self.index.segment_entries_bounded(id, entries_left) {
                self.index.remove_if_matches(&page, &location);
                entries_left -= 1;
            }
            if !self.index.segment_empty(id) {
                return Err(Error::Overloaded);
            }
            match self.segments.recycle(id).map_err(Error::from) {
                Ok(()) => free += 1,
                Err(Error::Overloaded) => {}
                Err(e) => return Err(e),
            }
        }
        if free >= target {
            Ok(())
        } else {
            Err(Error::Overloaded)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheId;
    use crate::model::CacheKey;
    use crate::model::StrongEtag;

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
    fn index(capacity: usize) -> Index {
        Index::new(WorkerId(0), capacity, crate::test_support::availability())
    }
    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::security::test_support::CACHE.into()),
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
