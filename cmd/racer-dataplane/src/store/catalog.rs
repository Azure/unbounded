//! Identity mappings, segment lifecycle, and lease-fenced second-chance eviction.
use super::disk::{DirectAlignment, SlabId, SlabLocation};
use crate::model::KeyId;
use crate::runtime::collections::{HashMap, HashSet};
use crate::{
    error::{Error, Operation, Result},
    model::{
        CurrentVersion, ObjectId, ObjectMetadata, ObjectVersion, PageId, VersionMetadata, WorkerId,
    },
};
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, BTreeSet, VecDeque},
    rc::Rc,
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
    availability: Option<std::rc::Rc<crate::control::state::Availability>>,
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
                if old.content_type.is_some() {
                    continue;
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
    pub fn new(worker: WorkerId, metadata_capacity: usize) -> Self {
        Self {
            worker,
            metadata_capacity,
            page_capacity: Cell::new(65536),
            state: RefCell::new(State::default()),
            availability: None,
            reserved: Cell::new(0),
        }
    }
    pub fn with_availability(
        mut self,
        availability: std::rc::Rc<crate::control::state::Availability>,
    ) -> Self {
        self.availability = Some(availability);
        self
    }
    fn available(&self, cache: &crate::model::CacheId) -> bool {
        self.availability.as_ref().is_none_or(|a| a.metadata(cache))
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
            .filter(|e| {
                self.availability
                    .as_ref()
                    .is_none_or(|a| a.page(&page.version.object.cache, e.key_id))
            })
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
        if self
            .availability
            .as_ref()
            .is_some_and(|a| !a.page(&page.version.object.cache, entry.key_id))
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
        if version.0.content_type.is_none() {
            version.0.content_type = entry.metadata.content_type.clone();
        }
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
        let mut descriptor = s.metadata.get(version).cloned().or_else(|| {
            s.versions
                .get(version)
                .map(|(metadata, _)| metadata.clone())
        });
        if let Some(m) = descriptor.as_mut() {
            if m.content_type.is_none() {
                m.content_type = s
                    .versions
                    .get(version)
                    .and_then(|(v, _)| v.content_type.clone());
            }
        }
        Ok(descriptor)
    }
    /// Page-zero owner only. Supports metadata-only objects without a dirty page,
    /// slab allocation, encryption record, or ciphertext reservation.
    pub fn publish_version(&self, mut metadata: VersionMetadata) -> Result<()> {
        if !self.available(&metadata.version.object.cache) {
            return Ok(());
        }
        Self::validate_descriptor(&metadata)?;
        let mut s = self.state.borrow_mut();
        Self::check_length(&s, &metadata)?;
        if metadata.content_type.is_none() {
            metadata.content_type = s
                .metadata
                .get(&metadata.version)
                .or_else(|| s.versions.get(&metadata.version).map(|(m, _)| m))
                .and_then(|m| m.content_type.clone());
        }
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
            .filter(|v| crate::runtime::environment::wall_now() < v.expires_at.0)
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
        if crate::runtime::environment::wall_now() < metadata.expires_at.0 {
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
    /// Owner-local bounded cut. Appends are frozen by Checkpointer; removals are
    /// safe omissions. The age tree avoids rescanning a growing hash table.
    pub fn snapshot_pages(&self, after: u64, limit: usize) -> (u64, Vec<(PageId, IndexedPage)>) {
        use std::ops::Bound::{Excluded, Unbounded};
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
        // Reject downsized cuts before allocating descriptor/duplicate tables.
        if snapshot.entries.len() > self.page_capacity.get()
            || snapshot.metadata.len() > self.metadata_capacity
        {
            return Err(Error::CorruptRecord);
        }
        snapshot.validate_metadata()?;
        for m in snapshot
            .metadata
            .iter()
            .chain(snapshot.entries.iter().map(|(_, e)| &e.metadata))
        {
            Self::validate_descriptor(m)?;
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
    pub fn segment_entries(&self, segment: SegmentId) -> Vec<(PageId, RecordLocation)> {
        self.segment_entries_bounded(segment, usize::MAX)
    }
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
    pub fn retire_key(&self, cache: &crate::model::CacheId, key: KeyId) -> usize {
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
            || m.version.object.cache.0.len() > super::format::MAX_ID_BYTES
            || m.version.etag.as_bytes().is_empty()
            || m.version.etag.as_bytes().len() > super::format::MAX_ETAG_BYTES
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
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct SegmentId(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct Generation(pub u64);
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SegmentState {
    Free,
    Open,
    Sealed,
    Evicting,
}
#[derive(Clone, Debug)]
pub struct SegmentSnapshot {
    pub id: SegmentId,
    pub generation: Generation,
    pub state: SegmentState,
    pub used_bytes: u64,
}
pub struct SegmentLease {
    worker: WorkerId,
    pub(crate) id: SegmentId,
    pub(crate) generation: Generation,
    count: Rc<Cell<usize>>,
}
impl SegmentLease {
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
    pub fn id(&self) -> SegmentId {
        self.id
    }
    pub fn generation(&self) -> Generation {
        self.generation
    }
}
impl Drop for SegmentLease {
    fn drop(&mut self) {
        self.count.set(self.count.get() - 1);
    }
}
pub struct AppendLease {
    pub segment: SegmentLease,
    pub location: SlabLocation,
}
struct Slot {
    image: SegmentSnapshot,
    leases: Rc<Cell<usize>>,
}
pub struct Segments {
    worker: WorkerId,
    segment_bytes: u64,
    slab_bytes: Cell<u64>,
    alignment: Cell<Option<DirectAlignment>>,
    slots: RefCell<Vec<Slot>>,
    frozen: Cell<bool>,
    open: Cell<Option<usize>>,
    free: RefCell<BTreeSet<usize>>,
}
impl Segments {
    pub fn new(worker: WorkerId, segment_bytes: u64) -> Self {
        Self {
            worker,
            segment_bytes,
            slab_bytes: Cell::new(0),
            alignment: Cell::new(None),
            slots: RefCell::new(Vec::new()),
            frozen: Cell::new(false),
            open: Cell::new(None),
            free: RefCell::new(BTreeSet::new()),
        }
    }
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }
    pub fn slab_bytes(&self) -> u64 {
        self.slab_bytes.get()
    }
    pub fn configure(
        &self,
        slab_bytes: u64,
        segment_count: usize,
        alignment: DirectAlignment,
    ) -> Result<()> {
        if !self.slots.borrow().is_empty()
            || self.segment_bytes == 0
            || slab_bytes == 0
            || !slab_bytes.is_multiple_of(self.segment_bytes)
            || !self.segment_bytes.is_multiple_of(alignment.offset())
            || !self.segment_bytes.is_multiple_of(alignment.length() as u64)
            || segment_count == 0
            || segment_count > 1_000_000
            || segment_count as u64 > slab_bytes / self.segment_bytes
        {
            return Err(Error::InvalidConfiguration);
        }
        self.slab_bytes.set(slab_bytes);
        self.alignment.set(Some(alignment));
        *self.slots.borrow_mut() = (0..segment_count)
            .map(|id| Slot {
                image: SegmentSnapshot {
                    id: SegmentId(id as u64),
                    generation: Generation(1),
                    state: SegmentState::Free,
                    used_bytes: 0,
                },
                leases: Rc::new(Cell::new(0)),
            })
            .collect();
        *self.free.borrow_mut() = (0..segment_count).collect();
        Ok(())
    }
    pub fn append(&self, disk_bytes: usize) -> Result<AppendLease> {
        if self.frozen.get() {
            return Err(Error::Overloaded);
        }
        let alignment = self.alignment.get().ok_or(Error::Unavailable)?;
        if disk_bytes == 0
            || disk_bytes as u64 > self.segment_bytes
            || alignment.extent(0, disk_bytes)?.length() != disk_bytes
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut slots = self.slots.borrow_mut();
        if let Some(position) = self.open.get() {
            let slot = &mut slots[position];
            if self.segment_bytes - slot.image.used_bytes < disk_bytes as u64 {
                slot.image.state = SegmentState::Sealed;
                self.open.set(None);
            }
        }
        let position = match self.open.get() {
            Some(position) => position,
            None => self
                .free
                .borrow_mut()
                .pop_first()
                .ok_or(Error::Overloaded)?,
        };
        self.open.set(Some(position));
        let slot = &mut slots[position];
        slot.image.state = SegmentState::Open;
        let offset = slot
            .image
            .id
            .0
            .checked_mul(self.segment_bytes)
            .and_then(|v| v.checked_add(slot.image.used_bytes))
            .ok_or(Error::InvalidConfiguration)?;
        slot.image.used_bytes += disk_bytes as u64;
        if slot.image.used_bytes == self.segment_bytes {
            slot.image.state = SegmentState::Sealed;
            self.open.set(None);
        }
        let lease = self.take_lease(slot)?;
        Ok(AppendLease {
            segment: lease,
            location: SlabLocation {
                slab: SlabId(0),
                extent: alignment.extent(offset, disk_bytes)?,
            },
        })
    }
    fn take_lease(&self, slot: &Slot) -> Result<SegmentLease> {
        slot.leases
            .set(slot.leases.get().checked_add(1).ok_or(Error::Overloaded)?);
        Ok(SegmentLease {
            worker: self.worker,
            id: slot.image.id,
            generation: slot.image.generation,
            count: slot.leases.clone(),
        })
    }
    pub fn lease(&self, id: SegmentId, generation: Generation) -> Result<SegmentLease> {
        let slots = self.slots.borrow();
        let slot = slots.get(id.0 as usize).ok_or(Error::CorruptRecord)?;
        if slot.image.generation != generation
            || !matches!(slot.image.state, SegmentState::Open | SegmentState::Sealed)
        {
            return Err(Error::CorruptRecord);
        }
        self.take_lease(slot)
    }
    pub fn begin_evict(&self, id: SegmentId) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Overloaded);
        }
        let mut slots = self.slots.borrow_mut();
        let slot = slots.get_mut(id.0 as usize).ok_or(Error::CorruptRecord)?;
        if !matches!(
            slot.image.state,
            SegmentState::Sealed | SegmentState::Evicting
        ) {
            return Err(Error::Overloaded);
        }
        slot.image.state = SegmentState::Evicting;
        Ok(())
    }
    pub fn recycle(&self, id: SegmentId) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Overloaded);
        }
        let mut slots = self.slots.borrow_mut();
        let slot = slots.get_mut(id.0 as usize).ok_or(Error::CorruptRecord)?;
        if slot.image.state != SegmentState::Evicting || slot.leases.get() != 0 {
            return Err(Error::Overloaded);
        }
        slot.image.generation = Generation(
            slot.image
                .generation
                .0
                .checked_add(1)
                .ok_or(Error::Unavailable)?,
        );
        slot.image.state = SegmentState::Free;
        slot.image.used_bytes = 0;
        self.free.borrow_mut().insert(id.0 as usize);
        Ok(())
    }
    pub fn snapshot(&self) -> Result<Vec<SegmentSnapshot>> {
        Ok(self
            .slots
            .borrow()
            .iter()
            .map(|s| s.image.clone())
            .collect())
    }
    pub fn freeze(&self) -> Result<()> {
        if self.frozen.replace(true) {
            return Err(Error::Overloaded);
        }
        Ok(())
    }
    pub fn thaw(&self) {
        self.frozen.set(false);
    }
    pub fn validate_restore(&self, images: &[SegmentSnapshot]) -> Result<()> {
        let slots = self.slots.borrow();
        if images.len() != slots.len() || slots.iter().any(|s| s.leases.get() != 0) {
            return Err(Error::CorruptRecord);
        }
        let alignment = self.alignment.get().ok_or(Error::Unavailable)?;
        for (i, s) in images.iter().enumerate() {
            if s.id.0 != i as u64
                || s.generation.0 == 0
                || s.used_bytes > self.segment_bytes
                || !s.used_bytes.is_multiple_of(alignment.offset())
                || !s.used_bytes.is_multiple_of(alignment.length() as u64)
                || (s.state == SegmentState::Free && s.used_bytes != 0)
            {
                return Err(Error::CorruptRecord);
            }
        }
        Ok(())
    }
    pub fn restore(&self, images: Vec<SegmentSnapshot>) -> Result<()> {
        self.validate_restore(&images)?;
        let mut slots = self.slots.borrow_mut();
        self.open.set(None);
        self.free.borrow_mut().clear();
        for (slot, mut image) in slots.iter_mut().zip(images) {
            if image.state == SegmentState::Open {
                image.state = SegmentState::Sealed;
            }
            if image.state == SegmentState::Free {
                self.free.borrow_mut().insert(image.id.0 as usize);
            }
            slot.image = image;
        }
        Ok(())
    }
    pub fn validate_location(&self, location: &RecordLocation) -> Result<()> {
        let slots = self.slots.borrow();
        let s = slots
            .get(location.segment.0 as usize)
            .ok_or(Error::CorruptRecord)?;
        let start = location
            .segment
            .0
            .checked_mul(self.segment_bytes)
            .ok_or(Error::CorruptRecord)?;
        let end = location
            .location
            .extent
            .offset()
            .checked_add(location.location.extent.length() as u64)
            .ok_or(Error::CorruptRecord)?;
        if location.location.slab != SlabId(0)
            || s.image.generation != location.generation
            || !matches!(s.image.state, SegmentState::Open | SegmentState::Sealed)
            || location.location.extent.offset() < start
            || end > start + s.image.used_bytes
        {
            return Err(Error::CorruptRecord);
        }
        let a = self.alignment.get().ok_or(Error::Unavailable)?;
        if a.extent(
            location.location.extent.offset(),
            location.location.extent.length(),
        )? != location.location.extent
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
    pub fn free_count(&self) -> usize {
        self.free.borrow().len()
    }
    pub fn count(&self) -> usize {
        self.slots.borrow().len()
    }
    pub fn state(&self, id: SegmentId) -> Result<SegmentState> {
        self.slots
            .borrow()
            .get(id.0 as usize)
            .map(|s| s.image.state)
            .ok_or(Error::CorruptRecord)
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
                self.index.remove_if_matches(&victim, &location)?;
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
                self.index.remove_if_matches(&page, &location)?;
                entries_left -= 1;
            }
            if !self.index.segment_empty(id) {
                return Err(Error::Overloaded);
            }
            match self.segments.recycle(id) {
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
    pub fn reclaim(&self) -> Operation<'_, ()> {
        Box::pin(async move { self.reclaim_now() })
    }
}

#[cfg(test)]
mod lifecycle_tests;
#[cfg(test)]
mod tests;
