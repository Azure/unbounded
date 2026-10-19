//! Object metadata catalog and the per-worker storage shard. Page data and its
//! index live in the extent volume; see `disk.rs` for the volume-backed page,
//! hold, and version operations.
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
use racer_control_wire::KeyId;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::rc::Rc;

/// One page on a worker's disk.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DiskPage {
    pub page: PageId,
    /// A page key recorded for the page's version. The volume does not store
    /// per-page key IDs; the record header is authoritative.
    pub key_id: KeyId,
    /// Byte offset of the record in the worker's slab address space.
    pub offset: u64,
    /// Bytes of the record on disk.
    pub length: u64,
}

/// Published pages under live hard holds on one storage worker.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct HeldSnapshot {
    pub pages: u64,
    pub payload_bytes: u64,
    /// Distinct data slots holding those pages.
    pub segments: u64,
}

/// One storage shard. The metadata catalog budget is independent of disk
/// pages; evicting a catalog item cannot remove a page's version descriptor.
pub struct Index {
    worker: WorkerId,

    metadata_capacity: usize,

    page_capacity: Cell<usize>,

    state: RefCell<State>,

    availability: Rc<crate::control::Availability>,

    pub(super) residency: Rc<Residency>,

    pub(super) disk: super::disk::Disk,
}

#[derive(Default)]
struct State {
    metadata: HashMap<ObjectVersion, VersionMetadata>,

    order: VecDeque<ObjectVersion>,

    current: HashMap<ObjectId, CurrentVersion>,
}
/// One worker's exact union of pending and indexed pages. Guards release heat
/// only when the final storage owner disappears, including cancellation/drop.
pub(super) struct Residency {
    retention: RefCell<Rc<Retention>>,
    counts: RefCell<HashMap<PageId, usize>>,
}
pub(super) struct Resident {
    residency: Rc<Residency>,
    pub(super) page: PageId,
    pub(super) payload: Option<crate::retention::Payload>,
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
#[derive(Clone, Default)]
pub struct IndexSnapshot {
    /// Bounded standalone descriptors, including HEAD-only and zero-length objects,
    /// on their page-zero owner. Current-version freshness is never recovered.
    pub metadata: Vec<VersionMetadata>,
    /// Descriptors of versions with pages on this worker's disk, with the page
    /// keys seen for them.
    pub versions: Vec<super::versions::VersionEntry>,
}
impl IndexSnapshot {
    /// Structural checkpoint validation, in addition to checksum, ownership,
    /// capacity, and layout checks performed during recovery.
    pub fn validate_metadata(&self) -> Result<()> {
        let mut lengths: HashMap<&ObjectVersion, &VersionMetadata> = HashMap::default();
        for metadata in self
            .metadata
            .iter()
            .chain(self.versions.iter().map(|entry| &entry.metadata))
        {
            if let Some(old) = lengths.get(&metadata.version)
                && !old.compatible(metadata)
            {
                return Err(Error::CorruptRecord);
            }
            lengths.insert(&metadata.version, metadata);
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
        let page_capacity = 65536;
        Self {
            worker,
            metadata_capacity,
            page_capacity: Cell::new(page_capacity),
            state: RefCell::new(State::default()),
            availability,
            residency: Rc::new(Residency {
                retention: RefCell::new(Rc::new(
                    Retention::new(256 * 1024, 65536 + 64).expect("valid bounded retention"),
                )),
                counts: RefCell::new(HashMap::default()),
            }),
            disk: super::disk::Disk::new(page_capacity),
        }
    }
    pub(super) fn available(&self, cache: &racer_control_wire::CacheId) -> bool {
        self.availability.metadata(cache)
    }
    pub(super) fn page_available(&self, cache: &racer_control_wire::CacheId, key: KeyId) -> bool {
        self.availability.page(cache, key)
    }
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
    pub fn metadata_capacity(&self) -> usize {
        self.metadata_capacity
    }
    /// Bound on disk pages, also the bound on the version table.
    pub fn page_capacity(&self) -> usize {
        self.page_capacity.get()
    }
    pub fn set_page_capacity(&self, capacity: usize) -> Result<()> {
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.disk.versions.borrow_mut().set_capacity(capacity)?;
        self.page_capacity.set(capacity);
        self.update_volume_limits();
        Ok(())
    }
    pub fn retention(&self) -> Rc<Retention> {
        self.residency.retention.borrow().clone()
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
    /// Look in both the standalone catalog and the disk version table for this
    /// exact version. This operation does not consult TTL or substitute another ETag.
    pub fn version(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        if !self.available(&version.object.cache) {
            return Ok(None);
        }
        let descriptor = self
            .state
            .borrow()
            .metadata
            .get(version)
            .cloned()
            .or_else(|| {
                self.disk
                    .versions
                    .borrow()
                    .get_version(version)
                    .map(|entry| entry.metadata.clone())
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
        self.check_length(&metadata)?;
        let mut s = self.state.borrow_mut();
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
    /// Disk version descriptors follow their pages instead; no page is removed.
    pub fn evict_metadata(&self, entries: usize) -> Result<usize> {
        let mut s = self.state.borrow_mut();
        let count = entries.min(s.metadata.len());
        for _ in 0..count {
            Self::evict_one(&mut s);
        }
        Ok(count)
    }
    pub fn snapshot(&self) -> Result<IndexSnapshot> {
        let s = self.state.borrow();
        Ok(IndexSnapshot {
            metadata: s.metadata.values().cloned().collect(),
            versions: self
                .disk
                .versions
                .borrow()
                .iter()
                .map(|(_, entry)| entry.clone())
                .collect(),
        })
    }
    pub fn snapshot_metadata(&self) -> Vec<VersionMetadata> {
        self.state.borrow().metadata.values().cloned().collect()
    }

    /// Reject an already oversized cut without pausing writes or cloning state.
    pub(super) fn preflight_snapshot(&self, remaining: &mut usize) -> Result<()> {
        let state = self.state.borrow();
        let versions = self.disk.versions.borrow();
        for bytes in state
            .metadata
            .values()
            .map(super::checkpoint::snapshot_metadata_bytes)
            .chain(
                versions
                    .iter()
                    .map(|(_, entry)| super::checkpoint::snapshot_version_bytes(entry)),
            )
        {
            *remaining = remaining.checked_sub(bytes).ok_or(Error::Overloaded)?;
        }
        Ok(())
    }

    /// Charge borrowed descriptors before cloning any snapshot strings or vectors.
    pub(super) fn snapshot_metadata_budgeted(
        &self,
        remaining: &mut usize,
    ) -> Result<Vec<VersionMetadata>> {
        let state = self.state.borrow();
        for metadata in state.metadata.values() {
            *remaining = remaining
                .checked_sub(super::checkpoint::snapshot_metadata_bytes(metadata))
                .ok_or(Error::Overloaded)?;
        }
        Ok(state.metadata.values().cloned().collect())
    }

    /// Up to `limit` disk version descriptors after the table cursor, oldest
    /// first. Checks the whole batch against the budget before cloning it.
    pub(super) fn snapshot_versions_budgeted(
        &self,
        after: Option<u64>,
        limit: usize,
        remaining: &mut usize,
    ) -> Result<(Option<u64>, Vec<super::versions::VersionEntry>)> {
        let versions = self.disk.versions.borrow();
        let mut cursor = after;
        for (age, entry) in versions.iter_after(after, limit) {
            *remaining = remaining
                .checked_sub(super::checkpoint::snapshot_version_bytes(entry))
                .ok_or(Error::Overloaded)?;
            cursor = Some(age);
        }
        let batch = versions
            .iter_after(after, limit)
            .map(|(_, entry)| entry.clone())
            .collect();
        Ok((cursor, batch))
    }

    /// Install recovered metadata before admission. Validate identity/length
    /// agreement and catalog bounds; clear all freshness. Version descriptors
    /// are kept only for versions that still have pages on the mounted volume.
    pub fn restore(&self, snapshot: IndexSnapshot) -> Result<()> {
        self.validate_snapshot(&snapshot)?;
        let mut replacement = Index::new(
            self.worker,
            self.metadata_capacity,
            self.availability.clone(),
        );
        for m in snapshot.metadata {
            replacement.publish_version(m)?;
        }
        let replacement = std::mem::take(&mut *replacement.state.get_mut());
        self.disk.restore_versions(snapshot.versions)?;
        let mut state = self.state.borrow_mut();
        state.metadata = replacement.metadata;
        state.order = replacement.order;
        state.current.clear();
        Ok(())
    }
    pub fn validate_snapshot(&self, snapshot: &IndexSnapshot) -> Result<()> {
        snapshot.validate_capacity(self.page_capacity.get(), self.metadata_capacity)
    }
    /// Drop metadata, freshness, holds, versions, and disk pages of one cache.
    pub fn remove_cache(&self, cache: &racer_control_wire::CacheId) {
        self.remove_disk_cache(cache);
        let mut s = self.state.borrow_mut();
        s.metadata.retain(|v, _| &v.object.cache != cache);
        s.order.retain(|v| &v.object.cache != cache);
        s.current.retain(|o, _| &o.cache != cache);
    }
    /// Reject a descriptor whose length conflicts with a known one.
    pub(super) fn check_length(&self, m: &VersionMetadata) -> Result<()> {
        if self
            .state
            .borrow()
            .metadata
            .get(&m.version)
            .is_some_and(|v| !v.compatible(m))
            || self
                .disk
                .versions
                .borrow()
                .get_version(&m.version)
                .is_some_and(|entry| !entry.metadata.compatible(m))
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
    pub(super) fn validate_descriptor(m: &VersionMetadata) -> Result<()> {
        if m.version.object.cache.0.is_empty()
            || m.version.object.cache.0.len() > super::MAX_ID_BYTES
            || m.version.etag.as_bytes().is_empty()
            || m.version.etag.as_bytes().len() > super::MAX_ETAG_BYTES
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
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
impl IndexSnapshot {
    pub fn validate_capacity(&self, page_capacity: usize, metadata_capacity: usize) -> Result<()> {
        let snapshot = self;
        // Reject downsized cuts before allocating descriptor/duplicate tables.
        if snapshot.versions.len() > page_capacity || snapshot.metadata.len() > metadata_capacity {
            return Err(Error::CorruptRecord);
        }
        snapshot.validate_metadata()?;
        for m in snapshot
            .metadata
            .iter()
            .chain(snapshot.versions.iter().map(|entry| &entry.metadata))
        {
            Index::validate_descriptor(m)?;
        }
        let mut metadata = HashSet::default();
        let mut versions = HashSet::default();
        if snapshot
            .versions
            .iter()
            .any(|entry| !versions.insert(&entry.version))
            || snapshot
                .metadata
                .iter()
                .any(|m| !metadata.insert(&m.version))
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::PageNumber;
    use crate::model::StrongEtag;
    use crate::store::key::version_key;
    use crate::store::test_disk::TestDisk;
    use page_alloc::volume::SLOT_BYTES;
    use page_alloc::volume::Tier;
    use racer_control_wire::CacheId;
    use std::time::Duration;

    fn index(capacity: usize) -> Index {
        Index::new(WorkerId(0), capacity, crate::test_support::availability())
    }
    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::test_support::security::CACHE.into()),
                    key: CacheKey([7; 32]),
                },
                etag: StrongEtag::parse(format!("\"{etag}\"").as_bytes()).unwrap(),
            },
            length,
        }
    }
    fn page(metadata: &VersionMetadata, number: u64) -> PageId {
        PageId {
            version: metadata.version.clone(),
            number: PageNumber(number),
        }
    }
    fn key_id() -> KeyId {
        crate::model::key_id_from_generation(1, 1).unwrap()
    }
    fn until() -> std::time::SystemTime {
        uring_runtime::environment::wall_now() + Duration::from_secs(60)
    }
    /// Publish a held storage-tier page through a hold ticket, as persistence does.
    fn put_held(disk: &TestDisk, index: &Index, metadata: &VersionMetadata, number: u64) {
        let page = page(metadata, number);
        let ticket = index.hold_ticket(&page).expect("unused hold capacity");
        disk.put(index, &page, metadata, key_id(), Tier::Storage);
        drop(ticket);
    }

    #[test]
    fn hard_hold_capacity_dedup_extension_and_release() {
        let index = index(8);
        // Eight slots at 50 percent allow four held pages.
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("held", 17);
        let held = page(&metadata, 0);
        disk.put(&index, &held, &metadata, key_id(), Tier::Cache);
        let until = until();
        index.reserve_hold(&held.version, 2, until).unwrap();
        index.reserve_hold(&held.version, 2, until).unwrap();
        index
            .extend_hold(&held.version, until - Duration::from_secs(1))
            .unwrap();
        assert_eq!(index.hold_expiry(&held.version), Some(until));
        // A cache-tier page is protected only once rewritten to storage.
        assert_eq!(index.held_page_expiry(&held), None);
        assert!(!index.already_on_disk(&held).unwrap());
        let other = descriptor("other", 17).version;
        assert_eq!(index.reserve_hold(&other, 3, until), Err(Error::Overloaded));
        index.release_hold(&held.version).unwrap();
        assert_eq!(index.hold_expiry(&held.version), None);
        index.reserve_hold(&other, 3, until).unwrap();
        assert!(!index.is_held(&held));
        assert!(index.already_on_disk(&held).unwrap());
        index.release_hold(&other).unwrap();
        assert_eq!(index.hold_expiry(&other), None);
        assert_eq!(
            index.reserve_hold(&other, usize::MAX, until),
            Err(Error::Overloaded)
        );
    }

    #[test]
    fn held_storage_pages_survive_eviction_pressure_until_expiry() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("protected", 17);
        let held = page(&metadata, 0);
        let until = until();
        index.reserve_hold(&held.version, 1, until).unwrap();
        put_held(&disk, &index, &metadata, 0);
        let value = index.lookup(&held).unwrap().unwrap();
        assert_eq!(value.tier, Tier::Storage);
        assert_eq!(
            value.retain_until,
            until
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs()
                + 1
        );
        assert_eq!(index.held_page_expiry(&held), Some(until));
        assert!(index.already_on_disk(&held).unwrap());
        // Fill every other slot with cache reservations, then ask for one more.
        let mut reservations = Vec::new();
        loop {
            match index.allocate(Tier::Cache, SLOT_BYTES) {
                Ok(reservation) => reservations.push(reservation),
                Err(error) => {
                    assert_eq!(error, Error::Overloaded);
                    break;
                }
            }
        }
        assert!(!reservations.is_empty());
        assert_eq!(index.lookup(&held).unwrap(), Some(value));
    }

    #[test]
    fn cache_pages_are_evicted_with_retention_accounting() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let retention = index.retention();
        let metadata = descriptor("evicted", 17);
        let cached = page(&metadata, 0);
        disk.put_whole(&index, &cached, &metadata, key_id(), Tier::Cache);
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
        let mut reservations = Vec::new();
        while index.lookup(&cached).unwrap().is_some() {
            reservations.push(index.allocate(Tier::Cache, SLOT_BYTES).unwrap());
        }
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
        assert_eq!(retention.snapshot().disk[0].segment_evicted_pages, 1);
        assert_eq!(
            retention.snapshot().disk[0].segment_evicted_payload_bytes,
            17
        );
        assert!(index.disk_pages().is_empty());
    }

    #[test]
    fn held_snapshot_counts_only_live_storage_publications_and_distinct_slots() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let until = until();
        let first = descriptor("first", 17);
        index.reserve_hold(&first.version, 2, until).unwrap();
        assert_eq!(index.held_snapshot(), HeldSnapshot::default());
        put_held(&disk, &index, &first, 0);
        let second = descriptor("second", 3);
        index.reserve_hold(&second.version, 1, until).unwrap();
        put_held(&disk, &index, &second, 0);
        // An unheld cache page of the same size class is not counted.
        let other = descriptor("other", 5);
        disk.put(&index, &page(&other, 0), &other, key_id(), Tier::Cache);
        let expected = HeldSnapshot {
            pages: 2,
            payload_bytes: 20,
            segments: 1,
        };
        assert_eq!(index.held_snapshot(), expected);
        index.release_hold(&first.version).unwrap();
        assert_eq!(index.held_snapshot(), expected);
        let value = index.lookup(&page(&second, 0)).unwrap().unwrap();
        index.remove_if_matches(&page(&second, 0), &value);
        assert_eq!(
            index.held_snapshot(),
            HeldSnapshot {
                pages: 1,
                payload_bytes: 17,
                segments: 1
            }
        );
        let key = version_key(&first.version);
        {
            let mut holds = index.disk.holds.borrow_mut();
            let hold = holds.entries.get_mut(&key).unwrap();
            hold.until = std::time::UNIX_EPOCH;
            assert!(hold.live(), "monotonic deadline still protects pages");
        }
        assert_eq!(index.held_snapshot().pages, 1);
        index
            .disk
            .holds
            .borrow_mut()
            .entries
            .get_mut(&key)
            .unwrap()
            .monotonic = uring_runtime::environment::now() - Duration::from_secs(1);
        assert_eq!(index.held_snapshot(), HeldSnapshot::default());
        assert!(
            index.lookup(&page(&first, 0)).unwrap().is_some(),
            "expiry need not evict the page"
        );
    }

    #[test]
    fn hard_hold_partial_failure_preserves_completed_page_and_deadline() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("partial", 17);
        let held = page(&metadata, 0);
        let until = until();
        index.reserve_hold(&held.version, 2, until).unwrap();
        assert_eq!(index.held_page_expiry(&held), None);
        put_held(&disk, &index, &metadata, 0);
        assert_eq!(
            index.reserve_hold(&held.version, 5, until + Duration::from_secs(60)),
            Err(Error::Overloaded)
        );
        index.release_hold(&held.version).unwrap();
        assert!(index.hold_ticket(&page(&metadata, 1)).is_none());
        assert_eq!(index.held_page_expiry(&held), Some(until));
        assert!(index.lookup(&held).unwrap().is_some());
    }

    #[test]
    fn hard_hold_bounds_ttl_and_keeps_inflight_capacity() {
        let unmounted = index(8);
        let version = descriptor("held", 17).version;
        let now = uring_runtime::environment::wall_now();
        // Before mount there is no slot to protect.
        assert_eq!(
            unmounted.reserve_hold(&version, 1, now + Duration::from_secs(60)),
            Err(Error::Overloaded)
        );
        let index = index(8);
        let _disk = TestDisk::mount(&index, 8);
        assert_eq!(
            index.reserve_hold(&version, 1, now),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(
            index.reserve_hold(
                &version,
                1,
                now + super::super::holds::MAX_HOLD_TTL + Duration::from_secs(1)
            ),
            Err(Error::InvalidConfiguration)
        );
        index
            .reserve_hold(&version, 2, now + Duration::from_secs(60))
            .unwrap();
        let page = PageId {
            version: version.clone(),
            number: PageNumber(0),
        };
        let ticket = index.hold_ticket(&page).unwrap();
        assert_eq!(index.release_hold(&version), Err(Error::Overloaded));
        drop(ticket);
        index.release_hold(&version).unwrap();
        assert_eq!(index.hold_expiry(&version), None);
        index
            .reserve_hold(&version, 1, now + Duration::from_secs(60))
            .unwrap();
        index.remove_cache(&version.object.cache);
        assert_eq!(index.hold_expiry(&version), None);
    }

    #[test]
    fn hard_hold_expiry_requires_both_clocks() {
        let index = index(8);
        let _disk = TestDisk::mount(&index, 8);
        let version = descriptor("ttl", 17).version;
        let now = uring_runtime::environment::wall_now();
        index
            .reserve_hold(&version, 1, now + Duration::from_secs(60))
            .unwrap();
        let key = version_key(&version);
        index
            .disk
            .holds
            .borrow_mut()
            .entries
            .get_mut(&key)
            .unwrap()
            .until = now - Duration::from_secs(1);
        assert!(index.hold_expiry(&version).is_some());
        index
            .disk
            .holds
            .borrow_mut()
            .entries
            .get_mut(&key)
            .unwrap()
            .monotonic = uring_runtime::environment::now() - Duration::from_secs(1);
        assert_eq!(index.hold_expiry(&version), None);
    }

    #[test]
    fn holds_are_rebuilt_from_committed_storage_entries_at_mount() {
        let index = index(8);
        // Sixteen slots at 50 percent allow eight held pages.
        let disk = TestDisk::mount(&index, 16);
        let until = until();
        let held = descriptor("held", 2 * crate::model::PAGE_BYTES);
        index.reserve_hold(&held.version, 2, until).unwrap();
        put_held(&disk, &index, &held, 0);
        put_held(&disk, &index, &held, 1);
        let long = descriptor("long", 17);
        index.reserve_hold(&long.version, 1, until).unwrap();
        put_held(&disk, &index, &long, 0);
        let volume = index.volume().unwrap();
        let long_key = crate::store::key::volume_key(&page(&long, 0)).unwrap();
        let now = uring_runtime::environment::wall_now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
        // A deadline beyond the maximum TTL is clamped at mount.
        volume.set_retain(&long_key, now + 40 * 24 * 60 * 60);
        let expired = descriptor("expired", 17);
        index.reserve_hold(&expired.version, 1, until).unwrap();
        put_held(&disk, &index, &expired, 0);
        let expired_key = crate::store::key::volume_key(&page(&expired, 0)).unwrap();
        volume.set_retain(&expired_key, now - 1);
        drop(volume);
        disk.commit(&index);
        // Published after the commit, so the crash loses it.
        let lost = descriptor("lost", 17);
        index.reserve_hold(&lost.version, 1, until).unwrap();
        put_held(&disk, &index, &lost, 0);
        let fresh = Index::new(WorkerId(0), 8, crate::test_support::availability());
        let _disk = disk.crash(&fresh);
        assert!(fresh.is_held(&page(&held, 0)));
        let deadline = fresh.held_page_expiry(&page(&held, 1)).unwrap();
        assert!(deadline >= until && deadline < until + Duration::from_secs(1));
        let clamped = fresh.hold_expiry(&long.version).unwrap();
        assert!(
            clamped
                <= uring_runtime::environment::wall_now()
                    + super::super::holds::MAX_HOLD_TTL
                    + Duration::from_secs(1)
        );
        assert_eq!(fresh.hold_expiry(&expired.version), None);
        assert!(fresh.lookup(&page(&expired, 0)).unwrap().is_some());
        assert_eq!(fresh.hold_expiry(&lost.version), None);
        assert!(fresh.disk_entry(&page(&lost, 0)).is_none());
        // Recovered holds do not block startup configuration.
        fresh.set_hold_capacity_percent(10).unwrap();
        assert_eq!(
            fresh.set_hold_capacity_percent(101),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(fresh.release_hold(&held.version), Ok(()));
        assert!(fresh.is_held(&page(&held, 0)));
    }

    #[test]
    fn version_descriptors_follow_disk_pages_and_restore_only_present_versions() {
        let index = index(1);
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("old", 17);
        let resident = page(&metadata, 0);
        index.publish_version(metadata.clone()).unwrap();
        disk.put(&index, &resident, &metadata, key_id(), Tier::Cache);
        // Catalog eviction keeps the disk page's descriptor.
        index.publish_version(descriptor("new", 0)).unwrap();
        assert_eq!(
            index.version(&resident.version).unwrap(),
            Some(metadata.clone())
        );
        let mut conflict = metadata.clone();
        conflict.length = 18;
        assert_eq!(index.publish_version(conflict), Err(Error::CorruptRecord));
        let snapshot = index.snapshot().unwrap();
        assert_eq!(snapshot.versions.len(), 1);
        assert_eq!(snapshot.versions[0].key_ids, vec![key_id()]);
        index.restore(snapshot.clone()).unwrap();
        assert_eq!(
            index.version(&resident.version).unwrap(),
            Some(metadata.clone())
        );
        let value = index.lookup(&resident).unwrap().unwrap();
        index.remove_if_matches(&resident, &value);
        index.restore(snapshot).unwrap();
        assert!(index.version(&resident.version).unwrap().is_none());
    }

    #[test]
    fn revoked_page_keys_hide_disk_pages() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("revoked", 17);
        let resident = page(&metadata, 0);
        assert!(!disk.try_put(
            &index,
            &resident,
            &metadata,
            racer_control_wire::KeyId([99; 16]),
            Tier::Cache,
        ));
        assert!(index.disk_entry(&resident).is_none());
        assert!(index.lookup(&resident).unwrap().is_none());
    }

    #[test]
    fn retain_caches_purges_pages_holds_and_versions_of_removed_caches() {
        use racer_crypto::identity::KeyPurpose;
        let kept = CacheId(crate::test_support::security::CACHE.into());
        let removed = CacheId("44444444-4444-4444-8444-444444444444".into());
        let keys = Rc::new(crate::test_support::security::keys_for(&[
            kept.clone(),
            removed.clone(),
        ]));
        let availability =
            crate::control::for_caches(keys.clone(), vec![kept.clone(), removed.clone()]);
        let index = Index::new(WorkerId(0), 8, availability);
        let disk = TestDisk::mount(&index, 16);
        let until = until();
        let mut pages = Vec::new();
        for cache in [&kept, &removed] {
            let key_id = keys.active(cache, KeyPurpose::Page).unwrap().id();
            let mut metadata = descriptor("purge", 2 * crate::model::PAGE_BYTES);
            metadata.version.object.cache = cache.clone();
            index.reserve_hold(&metadata.version, 1, until).unwrap();
            let held = page(&metadata, 0);
            let ticket = index.hold_ticket(&held).unwrap();
            disk.put(&index, &held, &metadata, key_id, Tier::Storage);
            drop(ticket);
            let cached = page(&metadata, 1);
            disk.put(&index, &cached, &metadata, key_id, Tier::Cache);
            pages.push((held, cached));
        }
        assert_eq!(index.disk_len(), 4);
        assert_eq!(index.retain_caches(std::slice::from_ref(&kept)), 2);
        assert_eq!(index.disk_len(), 2);
        let (kept_held, kept_cached) = &pages[0];
        assert!(index.is_held(kept_held));
        assert!(index.lookup(kept_cached).unwrap().is_some());
        assert!(index.version(&kept_held.version).unwrap().is_some());
        let (removed_held, removed_cached) = &pages[1];
        assert!(!index.is_held(removed_held));
        assert!(index.disk_entry(removed_held).is_none());
        assert!(index.disk_entry(removed_cached).is_none());
        assert!(
            index
                .snapshot()
                .unwrap()
                .versions
                .iter()
                .all(|entry| entry.version.object.cache == kept)
        );
        // Purging is idempotent, and no accepted cache removes everything.
        assert_eq!(index.retain_caches(std::slice::from_ref(&kept)), 0);
        assert_eq!(index.retain_caches(&[]), 2);
        assert_eq!(index.disk_len(), 0);
        assert!(!index.is_held(kept_held));
    }

    #[test]
    fn content_type_is_immutable_including_absence() {
        let untyped = descriptor("v1", 3);
        let mut typed = untyped.clone();
        typed.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
        for (first, second) in [(untyped.clone(), typed.clone()), (typed.clone(), untyped)] {
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
    fn second_sight_residency_replacement_purge_and_drop_are_balanced() {
        let index = index(4);
        let disk = TestDisk::mount(&index, 8);
        let retention = index.retention();
        let metadata = descriptor("resident", 17);
        let resident = page(&metadata, 0);
        let pending = index.track(&resident).unwrap();
        retention.touch(&resident);
        disk.put(&index, &resident, &metadata, key_id(), Tier::Cache);
        assert_eq!(retention.snapshot().heat_entries, 1);
        assert_eq!(retention.score(&resident), 1);
        let first = index.lookup(&resident).unwrap().unwrap();
        disk.put(&index, &resident, &metadata, key_id(), Tier::Cache);
        assert_eq!(retention.score(&resident), 1);
        // A stale token cannot remove the replacement.
        index.remove_if_matches(&resident, &first);
        assert!(index.lookup(&resident).unwrap().is_some());
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
        index.remove_cache(&resident.version.object.cache);
        assert!(index.lookup(&resident).unwrap().is_none());
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
        assert_eq!(
            retention.snapshot().heat_entries,
            1,
            "pending still owns heat"
        );
        drop(pending);
        assert_eq!(retention.snapshot().heat_entries, 0);
        disk.put(&index, &resident, &metadata, key_id(), Tier::Cache);
        assert_eq!(retention.score(&resident), 0);
        drop(index);
        assert_eq!(retention.snapshot().heat_entries, 0);
    }

    #[test]
    fn heat_capacity_does_not_block_publication() {
        let index = index(4);
        let disk = TestDisk::mount(&index, 8);
        let retention = Rc::new(Retention::new(4096, 1).unwrap());
        index.set_retention(retention.clone());
        let metadata = descriptor("resident", 17);
        disk.put(
            &index,
            &page(&metadata, 0),
            &metadata,
            key_id(),
            Tier::Cache,
        );
        let other = descriptor("other", 17);
        assert!(matches!(
            index.track(&page(&other, 0)),
            Err(Error::Overloaded)
        ));
        // Residency is best effort: the page is still published, untracked.
        disk.put(&index, &page(&other, 0), &other, key_id(), Tier::Cache);
        assert!(index.lookup(&page(&other, 0)).unwrap().is_some());
        assert_eq!(retention.snapshot().indexed_payload_bytes, 17);
    }

    #[test]
    fn metadata_only_checkpoint_preserves_empty_and_old_versions_without_freshness() {
        let snapshot = IndexSnapshot {
            versions: vec![],
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
            versions: vec![],
            metadata: vec![first, conflicting],
        };
        assert_eq!(snapshot.validate_metadata(), Err(Error::CorruptRecord));
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
            versions: vec![],
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
