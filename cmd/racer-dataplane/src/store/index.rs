//! Identity mappings and reverse segment membership, published after complete writes.
use super::{
    segment::{Generation, SegmentId},
    slab::SlabLocation,
};
use crate::model::envelope::KeyId;
use crate::{
    error::{Error, Result},
    model::{
        identity::{ObjectId, ObjectVersion, PageId, WorkerId},
        metadata::{CurrentVersion, ObjectMetadata, VersionMetadata},
    },
};
use std::{
    cell::{Cell, RefCell},
    collections::{HashMap, HashSet, VecDeque},
};
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RecordLocation {
    pub segment: SegmentId,
    pub generation: Generation,
    pub location: SlabLocation,
}
/// One storage shard. The catalog budget is independent of retained page entries;
/// evicting a catalog item cannot remove a page's attached immutable descriptor.
pub struct Index {
    worker: WorkerId,
    metadata_capacity: usize,
    page_capacity: Cell<usize>,
    state: RefCell<State>,
}
#[derive(Default)]
struct State {
    pages: HashMap<PageId, IndexedPage>,
    reverse: HashMap<SegmentId, HashSet<PageId>>,
    versions: HashMap<ObjectVersion, (u64, usize)>,
    metadata: HashMap<ObjectVersion, VersionMetadata>,
    order: VecDeque<ObjectVersion>,
    current: HashMap<ObjectId, CurrentVersion>,
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
        let mut lengths = std::collections::HashMap::new();
        for metadata in self
            .metadata
            .iter()
            .chain(self.entries.iter().map(|(_, entry)| &entry.metadata))
        {
            if lengths
                .insert(&metadata.version, metadata.length)
                .is_some_and(|length| length != metadata.length)
            {
                return Err(crate::error::Error::CorruptRecord);
            }
        }
        for (page, entry) in &self.entries {
            entry.metadata.page_length(page)?;
        }
        Ok(())
    }
}
impl Index {
    pub fn new(worker: WorkerId, metadata_capacity: usize) -> Self {
        Self {
            worker,
            metadata_capacity,
            page_capacity: Cell::new(65536),
            state: RefCell::new(State::default()),
        }
    }
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
    pub fn metadata_capacity(&self) -> usize {
        self.metadata_capacity
    }
    pub fn set_page_capacity(&self, capacity: usize) -> Result<()> {
        if capacity == 0 || capacity < self.state.borrow().pages.len() {
            return Err(Error::InvalidConfiguration);
        }
        self.page_capacity.set(capacity);
        Ok(())
    }
    pub fn lookup(&self, page: &PageId) -> Result<Option<IndexedPage>> {
        Ok(self.state.borrow().pages.get(page).cloned())
    }
    /// Atomically publish a completed record with its immutable descriptor. Reject
    /// conflicting lengths for one version; never update current-version freshness.
    pub fn publish(&self, page: PageId, entry: IndexedPage) -> Result<()> {
        entry.metadata.page_length(&page)?;
        let mut state = self.state.borrow_mut();
        Self::check_length(&state, &entry.metadata)?;
        if !state.pages.contains_key(&page) && state.pages.len() >= self.page_capacity.get() {
            return Err(Error::Overloaded);
        }
        Self::remove_page(&mut state, &page);
        let version = state
            .versions
            .entry(page.version.clone())
            .or_insert((entry.metadata.length, 0));
        version.1 += 1;
        state
            .reverse
            .entry(entry.location.segment)
            .or_default()
            .insert(page.clone());
        state.pages.insert(page, entry);
        Ok(())
    }
    /// Look in both the standalone catalog and retained page entries for this exact
    /// version. This operation does not consult TTL or substitute another ETag.
    pub fn version(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        let s = self.state.borrow();
        Ok(s.metadata.get(version).cloned().or_else(|| {
            s.versions.get(version).map(|(length, _)| VersionMetadata {
                version: version.clone(),
                length: *length,
            })
        }))
    }
    /// Page-zero owner only. Supports metadata-only objects without a dirty page,
    /// slab allocation, encryption record, or ciphertext reservation.
    pub fn publish_version(&self, metadata: VersionMetadata) -> Result<()> {
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
        Ok(self
            .state
            .borrow()
            .current
            .get(object)
            .filter(|v| std::time::SystemTime::now() < v.expires_at.0)
            .cloned())
    }
    /// Page-zero owner only, after fresh revalidation. Atomically retain the
    /// immutable descriptor and advance the volatile pointer; reject length conflicts.
    /// A zero-TTL observation is returned to its waiters without a reusable hit.
    pub fn publish_current(&self, metadata: ObjectMetadata) -> Result<()> {
        self.publish_version(metadata.immutable())?;
        let mut s = self.state.borrow_mut();
        s.current.remove(&metadata.version.object);
        if std::time::SystemTime::now() < metadata.expires_at.0 {
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
    pub fn remove_if_matches(&self, page: &PageId, location: &RecordLocation) -> Result<()> {
        let mut s = self.state.borrow_mut();
        if s.pages.get(page).is_some_and(|p| &p.location == location) {
            Self::remove_page(&mut s, page);
        }
        Ok(())
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
    /// Install only with the matching recovered segment state before admission.
    /// Validate identity/length agreement and catalog bounds; clear all freshness.
    pub fn restore(&self, snapshot: IndexSnapshot) -> Result<()> {
        self.validate_snapshot(&snapshot)?;
        let replacement = Index::new(self.worker, self.metadata_capacity);
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
        snapshot.validate_metadata()?;
        if snapshot.entries.len() > self.page_capacity.get()
            || snapshot.metadata.len() > self.metadata_capacity
        {
            return Err(Error::CorruptRecord);
        }
        let mut pages = HashSet::new();
        let mut versions = HashSet::new();
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
    pub fn segment_entries(&self, segment: SegmentId) -> Vec<(PageId, RecordLocation)> {
        let s = self.state.borrow();
        s.reverse
            .get(&segment)
            .into_iter()
            .flatten()
            .filter_map(|p| s.pages.get(p).map(|e| (p.clone(), e.location.clone())))
            .collect()
    }
    pub fn retire_key(&self, cache: &crate::model::identity::CacheId, key: KeyId) -> usize {
        let mut s = self.state.borrow_mut();
        let pages: Vec<_> = s
            .pages
            .iter()
            .filter(|(p, e)| &p.version.object.cache == cache && e.key_id == key)
            .map(|(p, _)| p.clone())
            .collect();
        for page in &pages {
            Self::remove_page(&mut s, page);
        }
        pages.len()
    }
    pub fn remove_cache(&self, cache: &crate::model::identity::CacheId) {
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
            .is_some_and(|(l, _)| *l != m.length)
            || s.metadata
                .get(&m.version)
                .is_some_and(|v| v.length != m.length)
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
    fn remove_page(s: &mut State, page: &PageId) {
        if let Some(old) = s.pages.remove(page) {
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
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        error::Error,
        model::identity::{CacheId, CacheKey, StrongEtag},
    };

    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
        }
    }

    #[test]
    fn metadata_only_checkpoint_preserves_empty_and_old_versions_without_freshness() {
        let old = descriptor("old", 17);
        let empty = descriptor("new", 0);
        let snapshot = IndexSnapshot {
            entries: vec![],
            metadata: vec![old.clone(), empty.clone()],
        };
        assert_eq!(snapshot.validate_metadata(), Ok(()));
        assert_eq!(snapshot.metadata[0].for_pin().length, 17);
        assert_eq!(snapshot.metadata[1].for_pin().length, 0);
        assert_eq!(
            snapshot.metadata[0].for_pin().expires_at.0,
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

    fn indexed(metadata: VersionMetadata, segment: u64) -> (PageId, IndexedPage) {
        let page = PageId {
            version: metadata.version.clone(),
            number: crate::model::identity::PageNumber(0),
        };
        (
            page,
            IndexedPage {
                metadata,
                key_id: KeyId([1; 16]),
                location: RecordLocation {
                    segment: SegmentId(segment),
                    generation: Generation(1),
                    location: SlabLocation {
                        slab: super::super::slab::SlabId(0),
                        extent: super::super::direct::DirectExtent::checked(segment * 1024, 512)
                            .unwrap(),
                    },
                },
            },
        )
    }
    #[test]
    fn catalog_eviction_preserves_pages_and_conditional_removal_preserves_replacement() {
        let index = Index::new(WorkerId(0), 1);
        index.set_page_capacity(1).unwrap();
        let (page, old) = indexed(descriptor("old", 17), 0);
        index.publish_version(old.metadata.clone()).unwrap();
        index.publish(page.clone(), old.clone()).unwrap();
        index.publish_version(descriptor("new", 0)).unwrap();
        assert_eq!(index.version(&page.version).unwrap().unwrap().length, 17);
        let (_, replacement) = indexed(old.metadata.clone(), 1);
        index.publish(page.clone(), replacement.clone()).unwrap();
        assert!(index.segment_entries(SegmentId(0)).is_empty());
        index.remove_if_matches(&page, &old.location).unwrap();
        assert_eq!(
            index.lookup(&page).unwrap().unwrap().location,
            replacement.location
        );
        let (other, entry) = indexed(descriptor("other", 1), 2);
        assert_eq!(index.publish(other, entry), Err(Error::Overloaded));
        index
            .remove_if_matches(&page, &replacement.location)
            .unwrap();
        assert!(index.version(&page.version).unwrap().is_none());
    }
    #[test]
    fn restore_is_atomic_and_drops_freshness() {
        let index = Index::new(WorkerId(0), 2);
        let m = descriptor("v1", 17);
        let mut current = m.for_pin();
        current.expires_at.0 = std::time::SystemTime::now() + std::time::Duration::from_secs(60);
        index.publish_current(current).unwrap();
        assert!(index.current(&m.version.object).unwrap().is_some());
        let bad = IndexSnapshot {
            entries: vec![],
            metadata: vec![
                m.clone(),
                VersionMetadata {
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

    // Compile the complete recovery -> disk copy -> memory -> pending write path.
    // No fake buffers, alignment proof, or successful operational stub is needed.
    fn page_metadata_api_contract(
        snapshot: IndexSnapshot,
        index: &Index,
        page: PageId,
        entry: IndexedPage,
        result: crate::read::fill::PageResult,
        memory: &crate::memory::cache::MemoryCache,
        writer: &crate::store::writer::StoreWriter,
        dirty: crate::runtime::admission::Reservation,
    ) -> Result<()> {
        snapshot.validate_metadata()?;
        index.restore(snapshot)?;
        index.publish(page.clone(), entry)?;
        let _: Option<IndexedPage> = index.lookup(&page)?;
        let _: Option<VersionMetadata> = index.version(&page.version)?;
        let copy = result.copy();
        memory.publish(result.clone())?;
        let _: Option<crate::read::fill::PageResult> = memory.get(&page)?;
        writer.enqueue(copy, dirty)?;
        let _: Option<crate::memory::page::CiphertextCopy> = writer.copy_only(&page)?;
        Ok(())
    }

    // Duplicate writes, reverse membership, and actual restore/eviction behavior
    // remain tests for the storage implementation, not simulated by this scaffold.
}
