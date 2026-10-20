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
    pub slots: u64,
}

/// Disk page bound until `set_page_capacity` configures the worker's value.
const DEFAULT_PAGE_CAPACITY: usize = 65536;

/// Placeholder retention policy bounds until `set_retention` installs the
/// worker's policy: second-sight history bytes, and heat entries (the default
/// page capacity plus slack for pending writes).
const DEFAULT_RETENTION_HISTORY_BYTES: usize = 256 * 1024;
const DEFAULT_RETENTION_HEAT_ENTRIES: usize = DEFAULT_PAGE_CAPACITY + 64;

/// One storage shard. The metadata catalog budget is independent of disk
/// pages; evicting a catalog item cannot remove a page's version descriptor.
pub struct Index {
    worker: WorkerId,

    metadata_capacity: usize,

    page_capacity: Cell<usize>,

    state: RefCell<State>,

    availability: Rc<crate::control::Availability>,

    pub(super) residency: Rc<Residency>,

    /// The mounted extent volume; `None` until `Store::open`.
    pub(super) volume: RefCell<Option<Rc<super::disk::DiskVolume>>>,

    /// The worker reactor the volume's I/O runs on; set with the volume.
    pub(super) reactor: RefCell<Option<Rc<crate::runtime::Reactor>>>,

    /// Work queued by synchronous callers for `Index::maintain`.
    pub(super) maintenance: RefCell<super::disk::Maintenance>,

    pub(super) holds: RefCell<super::holds::Holds>,

    pub(super) versions: RefCell<super::versions::VersionTable>,

    /// Retention owners of published pages, by volume key, with payload bytes.
    pub(super) residents: RefCell<HashMap<page_alloc::volume::Key, (Resident, u64)>>,
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
    /// Drop disposable state whose keys are gone. Standalone metadata has no
    /// key ID, so `metadata_available` decides it per cache. A version
    /// descriptor keeps only its available page keys and is dropped when none
    /// remain.
    pub fn retain_available(
        &mut self,
        mut metadata_available: impl FnMut(&racer_control_wire::CacheId) -> bool,
        mut available: impl FnMut(&racer_control_wire::CacheId, KeyId) -> bool,
    ) {
        self.versions.retain_mut(|entry| {
            let cache = &entry.version.object.cache;
            entry.key_ids.retain(|key_id| available(cache, *key_id));
            !entry.key_ids.is_empty()
        });
        self.metadata
            .retain(|m| metadata_available(&m.version.object.cache));
    }
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
        let page_capacity = DEFAULT_PAGE_CAPACITY;
        Self {
            worker,
            metadata_capacity,
            page_capacity: Cell::new(page_capacity),
            state: RefCell::new(State::default()),
            availability,
            residency: Rc::new(Residency {
                retention: RefCell::new(Rc::new(
                    Retention::new(
                        DEFAULT_RETENTION_HISTORY_BYTES,
                        DEFAULT_RETENTION_HEAT_ENTRIES,
                    )
                    .expect("valid bounded retention"),
                )),
                counts: RefCell::new(HashMap::default()),
            }),
            volume: RefCell::new(None),
            reactor: RefCell::new(None),
            maintenance: RefCell::new(super::disk::Maintenance::default()),
            holds: RefCell::new(super::holds::Holds::default()),
            versions: RefCell::new(
                super::versions::VersionTable::new(page_capacity)
                    .expect("nonzero version capacity"),
            ),
            residents: RefCell::new(HashMap::default()),
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
        self.versions.borrow_mut().set_capacity(capacity)?;
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
                self.versions
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
        self.insert_metadata(&mut self.state.borrow_mut(), metadata)
    }
    /// Insert a validated descriptor, evicting the oldest past the catalog bound.
    fn insert_metadata(&self, s: &mut State, metadata: VersionMetadata) -> Result<()> {
        if self.metadata_capacity == 0 {
            return Err(Error::Overloaded);
        }
        if !s.metadata.contains_key(&metadata.version) {
            while s.metadata.len() >= self.metadata_capacity {
                Self::evict_one(s);
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
        let versions = self.versions.borrow();
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
        let versions = self.versions.borrow();
        let mut cursor = after;
        for (age, entry) in versions.iter_after(after, limit) {
            *remaining = remaining
                .checked_sub(super::checkpoint::snapshot_version_bytes(entry))
                .ok_or(Error::Overloaded)?;
            cursor = Some(age);
        }
        let batch = versions
            .iter_after(after, limit)
            .map(|(_, entry)| {
                // Defensive: never let one oversized descriptor fail the image.
                let mut entry = entry.clone();
                entry.truncate_key_ids();
                entry
            })
            .collect();
        Ok((cursor, batch))
    }

    /// Install recovered metadata before admission. Validate identity/length
    /// agreement and catalog bounds; clear all freshness. Version descriptors
    /// are kept only for versions that still have pages on the mounted volume.
    pub fn restore(&self, snapshot: IndexSnapshot) -> Result<()> {
        self.validate_snapshot(&snapshot)?;
        // Rebuild the catalog aside so a rejected snapshot leaves it untouched.
        let mut rebuilt = State::default();
        for m in snapshot.metadata {
            if !self.available(&m.version.object.cache) {
                continue;
            }
            Self::validate_descriptor(&m)?;
            if rebuilt
                .metadata
                .get(&m.version)
                .is_some_and(|old| !old.compatible(&m))
            {
                return Err(Error::CorruptRecord);
            }
            self.insert_metadata(&mut rebuilt, m)?;
        }
        self.restore_versions(snapshot.versions);
        let mut state = self.state.borrow_mut();
        state.metadata = rebuilt.metadata;
        state.order = rebuilt.order;
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
        index.reserve_hold_now(&held.version, 2, until).unwrap();
        index.reserve_hold_now(&held.version, 2, until).unwrap();
        index
            .extend_hold_now(&held.version, until - Duration::from_secs(1))
            .unwrap();
        assert_eq!(index.hold_expiry(&held.version), Some(until));
        // A cache-tier page is protected only once rewritten to storage.
        assert_eq!(index.held_page_expiry(&held), None);
        assert!(!index.already_on_disk(&held).unwrap());
        let other = descriptor("other", 17).version;
        assert_eq!(
            index.reserve_hold_now(&other, 3, until),
            Err(Error::Overloaded)
        );
        index.release_hold(&held.version).unwrap();
        assert_eq!(index.hold_expiry(&held.version), None);
        index.reserve_hold_now(&other, 3, until).unwrap();
        assert!(!index.is_held(&held));
        assert!(index.already_on_disk(&held).unwrap());
        index.release_hold(&other).unwrap();
        assert_eq!(index.hold_expiry(&other), None);
        assert_eq!(
            index.reserve_hold_now(&other, usize::MAX, until),
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
        index.reserve_hold_now(&held.version, 1, until).unwrap();
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
            match index.allocate_now(Tier::Cache, SLOT_BYTES) {
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
            reservations.push(index.allocate_now(Tier::Cache, SLOT_BYTES).unwrap());
        }
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
        assert_eq!(retention.snapshot().disk[0].slot_evicted_pages, 1);
        assert_eq!(retention.snapshot().disk[0].slot_evicted_payload_bytes, 17);
        assert!(index.disk_pages().is_empty());
    }

    #[test]
    fn held_snapshot_counts_only_live_storage_publications_and_distinct_slots() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let until = until();
        let first = descriptor("first", 17);
        index.reserve_hold_now(&first.version, 2, until).unwrap();
        assert_eq!(index.held_snapshot(), HeldSnapshot::default());
        put_held(&disk, &index, &first, 0);
        let second = descriptor("second", 3);
        index.reserve_hold_now(&second.version, 1, until).unwrap();
        put_held(&disk, &index, &second, 0);
        // An unheld cache page of the same size class is not counted.
        let other = descriptor("other", 5);
        disk.put(&index, &page(&other, 0), &other, key_id(), Tier::Cache);
        let expected = HeldSnapshot {
            pages: 2,
            payload_bytes: 20,
            slots: 1,
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
                slots: 1
            }
        );
        let key = version_key(&first.version);
        {
            let mut holds = index.holds.borrow_mut();
            let hold = holds.entries.get_mut(&key).unwrap();
            hold.until = std::time::UNIX_EPOCH;
            assert!(hold.live(), "monotonic deadline still protects pages");
        }
        assert_eq!(index.held_snapshot().pages, 1);
        index
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
        index.reserve_hold_now(&held.version, 2, until).unwrap();
        assert_eq!(index.held_page_expiry(&held), None);
        put_held(&disk, &index, &metadata, 0);
        assert_eq!(
            index.reserve_hold_now(&held.version, 5, until + Duration::from_secs(60)),
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
            unmounted.reserve_hold_now(&version, 1, now + Duration::from_secs(60)),
            Err(Error::Overloaded)
        );
        let index = index(8);
        let _disk = TestDisk::mount(&index, 8);
        assert_eq!(
            index.reserve_hold_now(&version, 1, now),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(
            index.reserve_hold_now(
                &version,
                1,
                now + super::super::holds::MAX_HOLD_TTL + Duration::from_secs(1)
            ),
            Err(Error::InvalidConfiguration)
        );
        index
            .reserve_hold_now(&version, 2, now + Duration::from_secs(60))
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
            .reserve_hold_now(&version, 1, now + Duration::from_secs(60))
            .unwrap();
        index.remove_cache(&version.object.cache);
        index.maintain_now();
        assert_eq!(index.hold_expiry(&version), None);
    }

    #[test]
    fn hard_hold_expiry_requires_both_clocks() {
        let index = index(8);
        let _disk = TestDisk::mount(&index, 8);
        let version = descriptor("ttl", 17).version;
        let now = uring_runtime::environment::wall_now();
        index
            .reserve_hold_now(&version, 1, now + Duration::from_secs(60))
            .unwrap();
        let key = version_key(&version);
        index
            .holds
            .borrow_mut()
            .entries
            .get_mut(&key)
            .unwrap()
            .until = now - Duration::from_secs(1);
        assert!(index.hold_expiry(&version).is_some());
        index
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
        index.reserve_hold_now(&held.version, 2, until).unwrap();
        put_held(&disk, &index, &held, 0);
        put_held(&disk, &index, &held, 1);
        let long = descriptor("long", 17);
        index.reserve_hold_now(&long.version, 1, until).unwrap();
        put_held(&disk, &index, &long, 0);
        let volume = index.volume().unwrap();
        let long_key = crate::store::key::volume_key(&page(&long, 0)).unwrap();
        let now = uring_runtime::environment::wall_now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs();
        // A deadline beyond the maximum TTL is clamped at mount.
        disk.drive(volume.set_retain(
            &*disk.reactor,
            &long_key,
            now + 40 * 24 * 60 * 60,
            &crate::store::test_disk::scope(),
        ))
        .unwrap();
        let expired = descriptor("expired", 17);
        index.reserve_hold_now(&expired.version, 1, until).unwrap();
        put_held(&disk, &index, &expired, 0);
        let expired_key = crate::store::key::volume_key(&page(&expired, 0)).unwrap();
        disk.drive(volume.set_retain(
            &*disk.reactor,
            &expired_key,
            now - 1,
            &crate::store::test_disk::scope(),
        ))
        .unwrap();
        drop(volume);
        disk.commit(&index);
        // Published after the commit, so the crash loses it.
        let lost = descriptor("lost", 17);
        index.reserve_hold_now(&lost.version, 1, until).unwrap();
        put_held(&disk, &index, &lost, 0);
        let fresh = Index::new(WorkerId(0), 8, crate::test_support::availability());
        let _disk = disk.crash(&fresh);
        assert!(fresh.is_held(&page(&held, 0)));
        let deadline = fresh.held_page_expiry(&page(&held, 1)).unwrap();
        assert!(deadline >= until && deadline < until + Duration::from_secs(1));
        // Both pages of one version merge into a single hold.
        {
            let holds = fresh.holds.borrow();
            let hold = holds.entries.get(&version_key(&held.version)).unwrap();
            assert_eq!(hold.pages, 2);
            assert_eq!(hold.published.len(), 2);
            assert_eq!(holds.entries.len(), 2, "held and long only");
        }
        // The deadline is clamped to the maximum TTL from the mount time.
        let clamped = fresh.hold_expiry(&long.version).unwrap();
        let ceiling = uring_runtime::environment::wall_now() + super::super::holds::MAX_HOLD_TTL;
        assert!(clamped <= ceiling + Duration::from_secs(1));
        assert!(clamped + Duration::from_secs(1) >= ceiling);
        // An expired deadline is skipped: the page stays as a plain entry.
        assert_eq!(fresh.hold_expiry(&expired.version), None);
        assert!(fresh.lookup(&page(&expired, 0)).unwrap().is_some());
        assert_eq!(fresh.hold_expiry(&lost.version), None);
        assert!(fresh.disk_entry_now(&page(&lost, 0)).is_none());
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
    fn restored_pages_count_indexed_payload_bytes() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("kept", 17);
        disk.put(
            &index,
            &page(&metadata, 0),
            &metadata,
            key_id(),
            Tier::Cache,
        );
        disk.commit(&index);
        let snapshot = index.snapshot().unwrap();
        let fresh = Index::new(WorkerId(0), 8, crate::test_support::availability());
        let _disk = disk.crash(&fresh);
        assert_eq!(fresh.retention().snapshot().indexed_payload_bytes, 0);
        fresh.restore(snapshot).unwrap();
        fresh.maintain_now();
        assert_eq!(fresh.retention().snapshot().indexed_payload_bytes, 17);
        fresh.remove_cache(&metadata.version.object.cache);
        fresh.maintain_now();
        assert_eq!(fresh.retention().snapshot().indexed_payload_bytes, 0);
    }

    #[test]
    fn hold_extension_is_durable_only_after_commit() {
        let index = index(8);
        let disk = TestDisk::mount(&index, 16);
        let held = descriptor("held", 17);
        let first = until();
        assert!(!index.reserve_hold_now(&held.version, 1, first).unwrap());
        put_held(&disk, &index, &held, 0);
        disk.commit(&index);
        let second = first + Duration::from_secs(600);
        assert!(index.extend_hold_now(&held.version, second).unwrap());
        assert!(index.is_dirty());
        // Repeating the same deadline changes nothing on disk.
        assert!(!index.extend_hold_now(&held.version, second).unwrap());

        // A crash before the commit rebuilds the hold from the old deadline.
        let lost = Index::new(WorkerId(0), 8, crate::test_support::availability());
        let disk = disk.crash(&lost);
        let deadline = lost.hold_expiry(&held.version).unwrap();
        assert!(deadline >= first && deadline < first + Duration::from_secs(1));

        // After a commit the extension survives a restart.
        assert!(lost.extend_hold_now(&held.version, second).unwrap());
        disk.commit(&lost);
        let fresh = Index::new(WorkerId(0), 8, crate::test_support::availability());
        let _disk = disk.crash(&fresh);
        let deadline = fresh.hold_expiry(&held.version).unwrap();
        assert!(deadline >= second && deadline < second + Duration::from_secs(1));
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
        index.maintain_now();
        assert_eq!(
            index.version(&resident.version).unwrap(),
            Some(metadata.clone())
        );
        let value = index.lookup(&resident).unwrap().unwrap();
        index.remove_if_matches(&resident, &value);
        index.restore(snapshot).unwrap();
        index.maintain_now();
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
        assert!(index.disk_entry_now(&resident).is_none());
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
            index.reserve_hold_now(&metadata.version, 1, until).unwrap();
            let held = page(&metadata, 0);
            let ticket = index.hold_ticket(&held).unwrap();
            disk.put(&index, &held, &metadata, key_id, Tier::Storage);
            drop(ticket);
            let cached = page(&metadata, 1);
            disk.put(&index, &cached, &metadata, key_id, Tier::Cache);
            pages.push((held, cached));
        }
        assert_eq!(index.disk_len(), 4);
        assert_eq!(index.retain_caches_now(std::slice::from_ref(&kept)), 2);
        assert_eq!(index.disk_len(), 2);
        let (kept_held, kept_cached) = &pages[0];
        assert!(index.is_held(kept_held));
        assert!(index.lookup(kept_cached).unwrap().is_some());
        assert!(index.version(&kept_held.version).unwrap().is_some());
        let (removed_held, removed_cached) = &pages[1];
        assert!(!index.is_held(removed_held));
        assert!(index.disk_entry_now(removed_held).is_none());
        assert!(index.disk_entry_now(removed_cached).is_none());
        assert!(
            index
                .snapshot()
                .unwrap()
                .versions
                .iter()
                .all(|entry| entry.version.object.cache == kept)
        );
        // Purging is idempotent, and no accepted cache removes everything.
        assert_eq!(index.retain_caches_now(std::slice::from_ref(&kept)), 0);
        assert_eq!(index.retain_caches_now(&[]), 2);
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
        index.maintain_now();
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
        index.maintain_now();
        assert!(index.current(&m.version.object).unwrap().is_none());
        assert_eq!(index.version(&m.version).unwrap(), Some(m));
    }

    #[test]
    fn memory_only_callers_queue_prefetch_on_a_cold_index() {
        let index = index(64);
        let disk = TestDisk::mount(&index, 8);
        let metadata = descriptor("cold", crate::model::PAGE_BYTES * 40);
        for number in 0..40 {
            disk.put(
                &index,
                &page(&metadata, number),
                &metadata,
                key_id(),
                Tier::Cache,
            );
        }
        disk.commit(&index);
        // A remount starts with an empty page cache.
        let disk = disk.crash(&index);
        let first = page(&metadata, 0);
        assert!(!index.maintenance_due());
        assert!(!index.on_disk_cached(&first));
        let (cursor, pages) = index.resident_pages_after(None, 8);
        assert_eq!(
            (cursor, pages.len()),
            (Some(page_alloc::volume::Key::MIN), 0)
        );
        assert!(index.maintenance_due() && index.is_dirty());
        index.maintain_now();
        assert!(index.on_disk_cached(&first));
        let (_, pages) = index.resident_pages_after(None, 8);
        assert_eq!(pages.len(), 8);
        assert_eq!(index.disk_pages().len(), 40);
        drop(disk);
    }

    #[test]
    fn cache_removal_hides_pages_at_once_and_deletes_them_in_maintenance() {
        let index = index(64);
        let disk = TestDisk::mount(&index, 8);
        let old = descriptor("old", crate::model::PAGE_BYTES * 3);
        for number in 0..3 {
            disk.put(&index, &page(&old, number), &old, key_id(), Tier::Cache);
        }
        disk.commit(&index);
        index.remove_cache(&old.version.object.cache);
        assert!(index.lookup(&page(&old, 0)).unwrap().is_none());
        assert!(index.disk_pages().is_empty());
        // Entries stay in the volume until maintenance deletes them.
        assert_eq!(index.disk_len(), 3);
        assert!(index.maintenance_due());
        // A page written after the removal is kept.
        let new = descriptor("new", 4096);
        disk.put(&index, &page(&new, 0), &new, key_id(), Tier::Cache);
        disk.commit(&index);
        assert!(!index.maintenance_due());
        assert_eq!(index.disk_len(), 1);
        assert!(index.lookup(&page(&new, 0)).unwrap().is_some());
    }
}
