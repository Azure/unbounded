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

/// Published pages under live hard holds on one storage worker.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct HeldSnapshot {
    pub pages: u64,
    pub payload_bytes: u64,
    pub segments: u64,
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

    holds: RefCell<super::holds::Holds>,
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
    hold: Option<Rc<Cell<usize>>>,
}
impl Drop for PageTicket {
    fn drop(&mut self) {
        self.index.reserved.set(self.index.reserved.get() - 1);
        if let Some(inflight) = &self.hold {
            inflight.set(inflight.get() - 1);
        }
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
    pub(super) fn bind_hold_segments(&self, segments: Rc<Segments>, free_reserve: usize) {
        let mut holds = self.holds.borrow_mut();
        holds.segments = Some(segments);
        holds.free_reserve = free_reserve;
    }

    /// Startup only. Cap hard holds as a percentage of both slots and index entries.
    /// The default is 50. One reserved page charges one whole segment.
    pub fn set_hold_capacity_percent(&self, percent: usize) -> Result<()> {
        let mut holds = self.holds.borrow_mut();
        holds.expire();
        if percent > 100 || !holds.entries.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        holds.percent = percent;
        Ok(())
    }

    /// Hold pages `0..pages` of `version` for a warm job. See [`Self::reserve_hold_pages`].
    pub fn reserve_hold(
        &self,
        version: &ObjectVersion,
        pages: usize,
        until: std::time::SystemTime,
    ) -> Result<()> {
        if pages > self.page_capacity.get() {
            return Err(Error::Overloaded);
        }
        self.reserve_warm_hold_pages(
            version,
            &super::holds::HoldPages::prefix(pages as u64),
            until,
        )
        .map(drop)
    }

    /// Reserve exactly `pages` of `version` at HEAD, before fetching any payload.
    /// The set merges with any existing hold for the version, so earlier promises
    /// stay held and extension never shortens a deadline. Capacity is charged once
    /// per member page; cached members count toward that charge and become
    /// protected atomically, or this call fails unchanged. Cached pages outside
    /// the set are not pinned.
    ///
    /// Each call adds one reader holder to every page in `pages`, to be given
    /// back with [`Self::release_hold_pages`] and the returned epoch, which
    /// identifies this reservation. Warm jobs use
    /// [`Self::reserve_warm_hold_pages`] instead.
    pub fn reserve_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        until: std::time::SystemTime,
    ) -> Result<super::holds::HoldEpoch> {
        self.reserve_hold_inner(
            version,
            pages,
            until,
            Some(super::holds::HolderKind::Reader),
        )
    }

    /// Like [`Self::reserve_hold_pages`], but the holders belong to a warm job:
    /// [`Self::release_hold`] drops them from unpublished pages, and expiry
    /// drops the rest. Releasing them with [`Self::release_hold_pages`] also works.
    pub fn reserve_warm_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        until: std::time::SystemTime,
    ) -> Result<super::holds::HoldEpoch> {
        self.reserve_hold_inner(version, pages, until, Some(super::holds::HolderKind::Warm))
    }

    /// Reserve with holders of `kind`; see [`Self::reserve_hold_pages`].
    pub fn reserve_hold_kind(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        until: std::time::SystemTime,
        kind: super::holds::HolderKind,
    ) -> Result<super::holds::HoldEpoch> {
        self.reserve_hold_inner(version, pages, until, Some(kind))
    }

    fn reserve_hold_inner(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        until: std::time::SystemTime,
        kind: Option<super::holds::HolderKind>,
    ) -> Result<super::holds::HoldEpoch> {
        let monotonic = super::holds::Holds::deadline(until)?;
        if !self.available(&version.object.cache) {
            return Err(Error::MissingKey);
        }
        let state = self.state.borrow();
        let mut holds = self.holds.borrow_mut();
        holds.expire();
        // Every holder-adding reservation gets a fresh epoch. Pages that join
        // here record it, so releases issued before they joined (including
        // for an earlier, trimmed membership of the same page) cannot reach them.
        let fresh = kind.map(|_| holds.next_epoch());
        let segments = holds.segments.as_ref().ok_or(Error::InvalidConfiguration)?;
        let old = holds.entries.get(version);
        let epoch = fresh.or(old.map(|hold| hold.epoch)).unwrap_or_default();
        let members = old.map_or_else(|| pages.clone(), |hold| hold.pages.union(pages));
        // Empty sets need no disk guarantee or retained reservation entry.
        if members.is_empty() {
            return Ok(epoch);
        }
        let mut locations = HashMap::default();
        let mut page = PageId {
            version: version.clone(),
            number: crate::model::PageNumber(0),
        };
        for number in members.iter() {
            page.number = number;
            let Some(entry) = state.pages.get(&page) else {
                continue;
            };
            segments
                .validate(
                    entry.location.segment,
                    entry.location.generation,
                    &entry.location.extent,
                )
                .map_err(|_| Error::Overloaded)?;
            locations.insert(number, entry.location.clone());
        }
        let count = members.len();
        let total = holds
            .entries
            .values()
            .try_fold(0usize, |sum, hold| sum.checked_add(hold.pages.len()))
            .ok_or(Error::Overloaded)?;
        let total = total
            .checked_sub(old.map_or(0, |hold| hold.pages.len()))
            .and_then(|n| n.checked_add(count))
            .ok_or(Error::Overloaded)?;
        let limit = (segments.count() * holds.percent / 100)
            .min(segments.count().saturating_sub(holds.free_reserve.max(1)))
            .min(self.page_capacity.get() * holds.percent / 100);
        if total > limit {
            return Err(Error::Overloaded);
        }
        let inflight = old.map_or_else(|| Rc::new(Cell::new(0)), |hold| hold.inflight.clone());
        let active = old.map_or_else(|| Rc::new(Cell::new(0)), |hold| hold.active.clone());
        let unused = holds.unused() - old.map_or(0, super::holds::Hold::unused)
            + count.saturating_sub(locations.len() + inflight.get());
        if state.pages.len() + self.reserved.get() + unused > self.page_capacity.get() {
            return Err(Error::Overloaded);
        }
        let mut holders = old.map_or_else(HashMap::default, |hold| hold.holders.clone());
        if let Some(kind) = kind {
            for number in pages.iter() {
                let holder = holders.entry(number).or_insert(super::holds::PageHolders {
                    count: 0,
                    warm: 0,
                    since: epoch,
                });
                holder.count = holder.count.checked_add(1).ok_or(Error::Overloaded)?;
                if kind == super::holds::HolderKind::Warm {
                    holder.warm += 1;
                }
            }
        }
        let until = old.map_or(until, |hold| hold.until.max(until));
        let monotonic = old.map_or(monotonic, |hold| hold.monotonic.max(monotonic));
        holds.entries.insert(
            version.clone(),
            super::holds::Hold {
                until,
                monotonic,
                pages: members,
                holders,
                epoch,
                inflight,
                active,
                locations,
            },
        );
        Ok(epoch)
    }

    /// Active reservation deadline, not proof that every requested page is on disk.
    pub fn hold_expiry(&self, version: &ObjectVersion) -> Option<std::time::SystemTime> {
        self.holds
            .borrow()
            .entries
            .get(version)
            .filter(|hold| hold.live())
            .map(|hold| hold.until)
    }

    /// Scan only bounded hold locations, not the full page index or payloads.
    /// Scratch space contains distinct segment IDs, bounded by held publications.
    pub fn held_snapshot(&self) -> HeldSnapshot {
        let holds = self.holds.borrow();
        let state = self.state.borrow();
        let mut snapshot = HeldSnapshot::default();
        let mut segments = HashSet::default();
        for (version, hold) in holds.entries.iter().filter(|(_, hold)| hold.live()) {
            let mut page = PageId {
                version: version.clone(),
                number: crate::model::PageNumber(0),
            };
            for (number, location) in &hold.locations {
                page.number = *number;
                let Some(entry) = state.pages.get(&page) else {
                    continue;
                };
                if entry.location != *location {
                    continue;
                }
                let Ok(bytes) = entry.metadata.page_length(&page) else {
                    continue;
                };
                snapshot.pages = snapshot.pages.saturating_add(1);
                snapshot.payload_bytes = snapshot.payload_bytes.saturating_add(u64::from(bytes));
                segments.insert(location.segment);
            }
        }
        snapshot.segments = segments.len() as u64;
        snapshot
    }

    /// Extend an existing reservation without adding page capacity.
    pub fn extend_hold(&self, version: &ObjectVersion, until: std::time::SystemTime) -> Result<()> {
        let pages = self
            .holds
            .borrow()
            .entries
            .get(version)
            .filter(|hold| hold.live())
            .map(|hold| hold.pages.clone())
            .ok_or(Error::Unavailable)?;
        // Extension renews the deadline only; it adds no holders.
        self.reserve_hold_inner(version, &pages, until, None)
            .map(drop)
    }

    /// Release only unused capacity after a partial failure. Completed pages stay held.
    /// Call after this version's persistence futures and pending writes have finished.
    /// Unpublished members lose their warm holders and leave the hold unless a
    /// reader still holds them; published members keep all their holders.
    pub fn release_hold(&self, version: &ObjectVersion) -> Result<()> {
        let mut holds = self.holds.borrow_mut();
        if let Some(hold) = holds.entries.get_mut(version) {
            if hold.inflight.get() != 0 || hold.active.get() != 0 {
                return Err(Error::Overloaded);
            }
            let locations = &hold.locations;
            hold.holders.retain(|page, holder| {
                if !locations.contains_key(page) {
                    holder.count -= holder.warm;
                    holder.warm = 0;
                }
                holder.count != 0
            });
            let holders = &hold.holders;
            hold.pages = hold
                .pages
                .iter()
                .filter(|page| holders.contains_key(page) || locations.contains_key(page))
                .collect();
            if hold.pages.is_empty() {
                holds.entries.remove(version);
            }
        }
        Ok(())
    }

    /// Drop one holder from each of `pages` taken under `epoch`. A page whose count reaches zero
    /// leaves the hold, returns its charged capacity and becomes evictable like
    /// any other cached page. Pages that are not members are ignored, so a
    /// release after expiry or [`Self::release_hold`] is a no-op. A page that
    /// joined the hold after `epoch` was issued (it left and was reserved again,
    /// or the entry expired, was released or restored) is skipped, so a stale
    /// release cannot drop a later reservation's holder. Releasing a
    /// page whose held write is still in flight is allowed; that write then
    /// publishes as an ordinary cached page.
    pub fn release_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        epoch: super::holds::HoldEpoch,
    ) {
        let mut holds = self.holds.borrow_mut();
        let Some(hold) = holds.entries.get_mut(version) else {
            return;
        };
        let mut released = Vec::new();
        for number in pages.iter() {
            if let Some(holder) = hold
                .holders
                .get_mut(&number)
                .filter(|holder| holder.since <= epoch)
            {
                holder.count -= 1;
                holder.warm = holder.warm.min(holder.count);
                if holder.count == 0 {
                    hold.holders.remove(&number);
                    hold.locations.remove(&number);
                    released.push(number);
                }
            }
        }
        if released.is_empty() {
            return;
        }
        let released: super::holds::HoldPages = released.into_iter().collect();
        hold.pages = hold
            .pages
            .iter()
            .filter(|page| !released.contains(*page))
            .collect();
        if hold.pages.is_empty() && hold.inflight.get() == 0 && hold.active.get() == 0 {
            holds.entries.remove(version);
        }
    }

    /// Epoch of the latest reservation into `version`'s hold, live or not.
    #[cfg(test)]
    pub fn hold_epoch(&self, version: &ObjectVersion) -> Option<super::holds::HoldEpoch> {
        self.holds
            .borrow()
            .entries
            .get(version)
            .map(|hold| hold.epoch)
    }

    /// Holder count of `page` in a live hold, or zero when it is not held.
    pub fn hold_holders(&self, page: &PageId) -> u32 {
        self.holds
            .borrow()
            .entries
            .get(&page.version)
            .filter(|hold| hold.live())
            .and_then(|hold| hold.holders.get(&page.number).map(|holder| holder.count))
            .unwrap_or(0)
    }

    /// Whether `page` is a member of a live hold, published or not.
    pub fn is_held(&self, page: &PageId) -> bool {
        self.holds
            .borrow()
            .entries
            .get(&page.version)
            .is_some_and(|hold| hold.live() && hold.pages.contains(page.number))
    }

    pub(super) fn protect_hold(&self, version: &ObjectVersion) -> Result<super::holds::HoldGuard> {
        let holds = self.holds.borrow();
        let hold = holds
            .entries
            .get(version)
            .filter(|hold| hold.live())
            .ok_or(Error::Unavailable)?;
        hold.active
            .set(hold.active.get().checked_add(1).ok_or(Error::Overloaded)?);
        Ok(super::holds::HoldGuard(hold.active.clone()))
    }

    /// Deadline of a published held page. A HEAD reservation alone returns None.
    pub fn held_page_expiry(&self, page: &PageId) -> Option<std::time::SystemTime> {
        self.holds
            .borrow()
            .entries
            .get(&page.version)
            .filter(|hold| hold.live() && hold.locations.contains_key(&page.number))
            .map(|hold| hold.until)
    }

    fn hold_unused(&self) -> usize {
        let mut holds = self.holds.borrow_mut();
        holds.expire();
        holds.unused()
    }

    fn hold_available(&self, page: &PageId) -> bool {
        self.holds
            .borrow()
            .entries
            .get(&page.version)
            .is_some_and(|hold| {
                hold.live()
                    && hold.unused() != 0
                    && hold.pages.contains(page.number)
                    && !hold.locations.contains_key(&page.number)
            })
    }

    fn hold_published(&self, page: &PageId, location: &RecordLocation) -> Result<()> {
        let mut holds = self.holds.borrow_mut();
        let segments = holds.segments.clone();
        if let Some(hold) = holds
            .entries
            .get_mut(&page.version)
            .filter(|hold| hold.live() && hold.pages.contains(page.number))
        {
            if !hold.locations.contains_key(&page.number) && hold.unused() == 0 {
                return Err(Error::Overloaded);
            }
            segments.ok_or(Error::InvalidConfiguration)?.validate(
                location.segment,
                location.generation,
                &location.extent,
            )?;
            hold.locations.insert(page.number, location.clone());
        }
        Ok(())
    }

    pub(super) fn snapshot_holds(
        &self,
        segments: &mut [page_alloc::SegmentSnapshot],
        remaining: &mut usize,
    ) -> Result<()> {
        let holds = self.holds.borrow();
        for hold in holds.entries.values().filter(|hold| hold.live()) {
            for location in hold.locations.values() {
                *remaining = remaining
                    .checked_sub(super::checkpoint::RETAINED_RECORD_BYTES)
                    .ok_or(Error::Overloaded)?;
                segments
                    .get_mut(location.segment.0 as usize)
                    .ok_or(Error::CorruptRecord)?
                    .retained_records
                    .push((location.extent.offset(), hold.until));
            }
        }
        Ok(())
    }

    pub(super) fn restore_holds(&self, segments: &[page_alloc::SegmentSnapshot]) -> Result<()> {
        let now = uring_runtime::environment::wall_now();
        let mut deadlines = HashMap::default();
        for segment in segments {
            for (offset, until) in &segment.retained_records {
                if *until > now {
                    deadlines.insert(*offset, *until);
                }
            }
        }
        let mut versions: HashMap<ObjectVersion, super::holds::Hold> = HashMap::default();
        // Pages are visited in hash order; collect them and sort each set once
        // rather than paying a vector shift per out-of-order insert.
        let mut members: HashMap<ObjectVersion, Vec<crate::model::PageNumber>> = HashMap::default();
        for (page, entry) in self.state.borrow().pages.iter() {
            if let Some(until) = deadlines.get(&entry.location.extent.offset()) {
                let Some(monotonic) = until
                    .duration_since(now)
                    .ok()
                    .and_then(|ttl| uring_runtime::environment::now().checked_add(ttl))
                else {
                    continue;
                };
                let hold =
                    versions
                        .entry(page.version.clone())
                        .or_insert_with(|| super::holds::Hold {
                            until: *until,
                            monotonic,
                            pages: super::holds::HoldPages::default(),
                            holders: HashMap::default(),
                            epoch: super::holds::HoldEpoch::default(),
                            inflight: Rc::new(Cell::new(0)),
                            active: Rc::new(Cell::new(0)),
                            locations: HashMap::default(),
                        });
                hold.until = hold.until.max(*until);
                hold.monotonic = hold.monotonic.max(monotonic);
                hold.locations.insert(page.number, entry.location.clone());
                members
                    .entry(page.version.clone())
                    .or_default()
                    .push(page.number);
            }
        }
        for (version, pages) in members {
            if let Some(hold) = versions.get_mut(&version) {
                hold.pages = pages.into_iter().collect();
            }
        }
        let mut holds = self.holds.borrow_mut();
        // Restored entries are new lifetimes; releases from before cannot match.
        // Holder identities do not survive restart; each restored page keeps
        // one reader holder until expiry or `release_hold_pages`.
        for hold in versions.values_mut() {
            hold.epoch = holds.next_epoch();
            hold.holders = super::holds::Hold::single_holders(&hold.pages, hold.epoch);
        }
        holds.entries = versions;
        Ok(())
    }

    pub(super) fn validate_restored_holds(
        &self,
        snapshot: &IndexSnapshot,
        segments: &[page_alloc::SegmentSnapshot],
    ) -> Result<()> {
        let now = uring_runtime::environment::wall_now();
        let mut offsets = HashSet::default();
        for segment in segments {
            for (offset, until) in &segment.retained_records {
                if *until > now {
                    super::holds::Holds::deadline(*until)?;
                    offsets.insert(*offset);
                }
            }
        }
        // Count exactly what `restore_holds` restores as held: entries whose
        // offsets carry a live deadline. Cached nonmember pages of a held
        // version are not pinned by sparse holds and must not be charged.
        let pages = snapshot
            .entries
            .iter()
            .filter(|(_, entry)| offsets.contains(&entry.location.extent.offset()))
            .count();
        let holds = self.holds.borrow();
        let limit = (segments.len() * holds.percent / 100)
            .min(segments.len().saturating_sub(holds.free_reserve.max(1)))
            .min(self.page_capacity.get() * holds.percent / 100);
        if pages > limit || (pages != 0 && holds.segments.is_none()) {
            return Err(Error::Overloaded);
        }
        Ok(())
    }

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
            holds: RefCell::new(super::holds::Holds::default()),
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
        if capacity == 0
            || capacity < self.state.borrow().pages.len() + self.reserved.get() + self.hold_unused()
        {
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
            || state.pages.len() + self.reserved.get() + self.hold_unused()
                < self.page_capacity.get()
            || self.hold_available(page)
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
        let scoring = retention.scoring_at(uring_runtime::environment::now());
        state.pages.victim_if(64, |page, _| {
            (!self.is_held(page)).then(|| u64::from(scoring.score(page)))
        })
    }
    /// Reserve one potential new mapping, optionally evicting a low-value mapping.
    /// Replacement writes also reserve: concurrent invalidation may remove the old
    /// mapping while the SQE is in flight. No segment bytes are recycled here.
    pub fn reserve_page(self: &Rc<Self>, evict: bool) -> Result<PageTicket> {
        self.reserve_page_inner(None, evict)
    }

    pub(super) fn reserve_page_for(
        self: &Rc<Self>,
        page: &PageId,
        evict: bool,
    ) -> Result<PageTicket> {
        self.reserve_page_inner(Some(page), evict)
    }

    fn reserve_page_inner(
        self: &Rc<Self>,
        page: Option<&PageId>,
        evict: bool,
    ) -> Result<PageTicket> {
        let held = page.filter(|page| self.hold_available(page));
        let mut state = self.state.borrow_mut();
        if state.pages.len() + self.reserved.get() + self.hold_unused()
            - usize::from(held.is_some())
            >= self.page_capacity.get()
        {
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
        let hold = held.map(|page| {
            let inflight = self.holds.borrow().entries[&page.version].inflight.clone();
            inflight.set(inflight.get() + 1);
            inflight
        });
        Ok(PageTicket {
            index: self.clone(),
            hold,
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
            && state.pages.len() + self.reserved.get() + self.hold_unused()
                - usize::from(self.hold_available(&page))
                >= self.page_capacity.get()
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
        self.hold_published(&page, &entry.location)?;
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
            if let Some(hold) = self.holds.borrow_mut().entries.get_mut(&page.version) {
                hold.locations.remove(&page.number);
            }
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

    /// Reject an already oversized cut without pausing writes or cloning state.
    pub(super) fn preflight_snapshot(&self, remaining: &mut usize) -> Result<()> {
        let state = self.state.borrow();
        for bytes in state
            .pages
            .iter()
            .map(|(_, entry)| super::checkpoint::snapshot_entry_bytes(&entry.metadata))
            .chain(
                state
                    .metadata
                    .values()
                    .map(super::checkpoint::snapshot_metadata_bytes),
            )
        {
            *remaining = remaining.checked_sub(bytes).ok_or(Error::Overloaded)?;
        }
        let holds = self.holds.borrow();
        for hold in holds.entries.values().filter(|hold| hold.live()) {
            let bytes = hold
                .locations
                .len()
                .checked_mul(super::checkpoint::RETAINED_RECORD_BYTES)
                .ok_or(Error::Overloaded)?;
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

    /// Check the whole bounded batch before cloning its first mapping.
    pub(super) fn snapshot_pages_budgeted(
        &self,
        after: u64,
        limit: usize,
        remaining: &mut usize,
    ) -> Result<(u64, Vec<(PageId, IndexedPage)>)> {
        let state = self.state.borrow();
        for (_, _, entry) in state.pages.after(after, limit) {
            *remaining = remaining
                .checked_sub(super::checkpoint::snapshot_entry_bytes(&entry.metadata))
                .ok_or(Error::Overloaded)?;
        }
        let mut cursor = after;
        let entries = state
            .pages
            .after(after, limit)
            .map(|(age, page, entry)| {
                cursor = age;
                (page.clone(), entry.clone())
            })
            .collect();
        Ok((cursor, entries))
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
    #[cfg(test)]
    fn segment_score(&self, segment: SegmentId) -> u64 {
        let retention = self.retention();
        self.segment_score_with(
            segment,
            &retention.scoring_at(uring_runtime::environment::now()),
        )
    }
    fn segment_score_with(
        &self,
        segment: SegmentId,
        scoring: &crate::retention::Scoring<'_>,
    ) -> u64 {
        let state = self.state.borrow();
        state
            .pages
            .segment_score(segment, 256, crate::model::PAGE_BYTES * 4, |page, entry| {
                let bytes = entry
                    .metadata
                    .page_length(page)
                    .unwrap_or(crate::model::PAGE_BYTES as u32);
                u64::from(bytes) * u64::from(scoring.score(page))
            })
    }
    pub fn remove_cache(&self, cache: &racer_control_wire::CacheId) {
        self.holds
            .borrow_mut()
            .entries
            .retain(|version, _| &version.object.cache != cache);
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
    fn can_reclaim(&self, segment: SegmentId) -> bool {
        !self.holds.borrow().segment_held(segment)
    }
    fn can_evict(&self, segment: SegmentId) -> bool {
        !self.active_writes.borrow().contains_key(&segment)
            && !self.holds.borrow().segment_held(segment)
    }
    fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize {
        if !self.can_evict(segment) {
            return 0;
        }
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
        index.bind_hold_segments(segments.clone(), free_reserve);
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
        let now = uring_runtime::environment::now();
        let retention = self.index.retention();
        let scoring = retention.scoring_at(now);
        self.clock
            .reclaim_scored(
                &*self.index,
                self.free_reserve,
                self.budget.segment_visits,
                self.budget.mapping_removals,
                |segment| self.index.segment_score_with(segment, &scoring),
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
    #[test]
    fn reclaim_and_index_victim_prepare_one_scoring_pass() {
        let index = Rc::new(index(8));
        index.set_page_capacity(4).unwrap();
        let segments = Rc::new(segments(1024, 2));
        for i in 0..2 {
            drop(segments.append(1024).unwrap());
            for j in 0..2 {
                let (page, entry) = indexed(descriptor(&format!("{i}-{j}"), 17), i);
                index.publish(page, entry).unwrap();
            }
        }
        let calls = Rc::new(Cell::new(0));
        index.retention().set_ownership_pass(Rc::new({
            let calls = calls.clone();
            move || {
                calls.set(calls.get() + 1);
                Rc::new(|_| true)
            }
        }));
        assert!(index.victim(&index.state.borrow()).is_some());
        assert_eq!(calls.get(), 1);
        let clock = SegmentClock::new(index.clone(), segments, 1);
        clock.reclaim_now().unwrap();
        assert_eq!(calls.get(), 2);
        assert_eq!(index.snapshot().unwrap().entries.len(), 2);
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
    fn hard_hold_capacity_dedup_extension_release_and_eviction() {
        use page_alloc::SegmentEntries;
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index.reserve_hold(&page.version, 2, until).unwrap();
        index.reserve_hold(&page.version, 2, until).unwrap();
        index
            .extend_hold(&page.version, until - std::time::Duration::from_secs(1))
            .unwrap();
        assert_eq!(index.hold_expiry(&page.version), Some(until));
        assert!(!index.can_evict(SegmentId(0)));
        assert_eq!(index.remove_bounded(SegmentId(0), 1), 0);
        assert!(index.victim(&index.state.borrow()).is_none());
        let other = descriptor("other", 17).version;
        assert_eq!(index.reserve_hold(&other, 2, until), Err(Error::Overloaded));
        index.release_hold(&page.version).unwrap();
        index.reserve_hold(&other, 2, until).unwrap();
        assert!(index.is_held(&page));
        for _ in 0..5 {
            drop(segments.append(1024).unwrap());
        }
        assert_eq!(segments.free_count(), 0);
        clock.reclaim_now().unwrap();
        assert!(index.lookup(&page).unwrap().is_some());
        assert_eq!(
            segments.state(SegmentId(0)),
            Ok(page_alloc::SegmentState::Sealed)
        );
        index.release_hold(&other).unwrap();
        assert_eq!(index.hold_expiry(&other), None);
        assert_eq!(
            index.reserve_hold(&other, usize::MAX, until),
            Err(Error::Overloaded)
        );
    }

    fn page_set(numbers: &[u64]) -> super::super::holds::HoldPages {
        numbers
            .iter()
            .copied()
            .map(crate::model::PageNumber)
            .collect()
    }

    #[test]
    fn hold_page_holders_share_and_release_individual_pages() {
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let version = page.version.clone();
        let at = |number| PageId {
            version: version.clone(),
            number: crate::model::PageNumber(number),
        };
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        // A warm holds 0..3 and a read independently holds 0 and 2.
        let epoch = index
            .reserve_hold_pages(&version, &page_set(&[0, 1, 2]), until)
            .unwrap();
        let read = index
            .reserve_hold_pages(&version, &page_set(&[0, 2]), until)
            .unwrap();
        assert!(read > epoch, "every reservation takes a newer epoch");
        assert_eq!(index.hold_holders(&at(0)), 2);
        assert_eq!(index.hold_holders(&at(1)), 1);
        assert_eq!(index.hold_holders(&at(2)), 2);
        // Extension renews only; it adds no holders.
        index.extend_hold(&version, until).unwrap();
        assert_eq!(index.hold_holders(&at(0)), 2);
        let other = descriptor("other", 17).version;
        assert_eq!(index.reserve_hold(&other, 2, until), Err(Error::Overloaded));
        // The read's release leaves the shared pages held by the warm.
        index.release_hold_pages(&version, &page_set(&[0, 2]), epoch);
        assert!(index.is_held(&at(0)));
        assert!(index.is_held(&at(2)));
        assert_eq!(index.hold_holders(&at(0)), 1);
        assert!(index.victim(&index.state.borrow()).is_none());
        // Releasing the last holder makes the published page evictable and
        // returns its capacity; unknown pages are ignored.
        index.release_hold_pages(&version, &page_set(&[0, 1, 9]), epoch);
        assert!(!index.is_held(&at(0)));
        assert!(!index.is_held(&at(1)));
        assert_eq!(index.held_page_expiry(&at(0)), None);
        assert_eq!(index.hold_holders(&at(0)), 0);
        assert_eq!(index.victim(&index.state.borrow()), Some(page.clone()));
        assert!(index.is_held(&at(2)));
        index.reserve_hold(&other, 2, until).unwrap();
        // The last page leaving removes the whole hold.
        index.release_hold_pages(&version, &page_set(&[2]), epoch);
        assert_eq!(index.hold_expiry(&version), None);
        index.release_hold_pages(&version, &page_set(&[2]), epoch);
        assert_eq!(index.hold_expiry(&version), None);
    }

    #[test]
    fn stale_hold_page_release_cannot_drop_a_replacement_holder() {
        let clock = uring_runtime::environment::SimulationClock::new(29);
        let _environment = clock.environment(0).enter();
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let version = page.version.clone();
        let ttl = std::time::Duration::from_secs(10);
        // A read holds page 0, then its hold expires before it delivers.
        let original = index
            .reserve_hold_pages(
                &version,
                &page_set(&[0]),
                uring_runtime::environment::wall_now() + ttl,
            )
            .unwrap();
        clock.advance(ttl + std::time::Duration::from_secs(1));
        assert_eq!(index.hold_holders(&page), 0);
        // Another reader or warm job re-reserves the page; the expired entry is
        // pruned and the replacement is a new epoch.
        let until = uring_runtime::environment::wall_now() + ttl;
        let replacement = index
            .reserve_hold_pages(&version, &page_set(&[0]), until)
            .unwrap();
        assert_ne!(replacement, original);
        assert_eq!(index.hold_holders(&page), 1);
        // The original read's delayed release must not consume that holder.
        index.release_hold_pages(&version, &page_set(&[0]), original);
        assert_eq!(index.hold_holders(&page), 1);
        assert!(index.is_held(&page));
        assert_eq!(index.held_page_expiry(&page), Some(until));
        assert!(index.victim(&index.state.borrow()).is_none());
        // The replacement's own release still works.
        index.release_hold_pages(&version, &page_set(&[0]), replacement);
        assert!(!index.is_held(&page));
        assert_eq!(index.hold_expiry(&version), None);
        assert_eq!(index.victim(&index.state.borrow()), Some(page.clone()));
    }

    #[test]
    fn hold_epoch_changes_with_every_reservation_but_not_extension() {
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let version = page.version.clone();
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        let first = index
            .reserve_hold_pages(&version, &page_set(&[0]), until)
            .unwrap();
        index.extend_hold(&version, until).unwrap();
        assert_eq!(index.hold_epoch(&version), Some(first));
        // Releasing the last page removes the entry; the next reservation is
        // a new lifetime and a repeated release of the first is ignored.
        index.release_hold_pages(&version, &page_set(&[0]), first);
        assert_eq!(index.hold_epoch(&version), None);
        let second = index
            .reserve_hold_pages(&version, &page_set(&[0]), until)
            .unwrap();
        assert_ne!(second, first);
        index.release_hold_pages(&version, &page_set(&[0]), first);
        assert_eq!(index.hold_holders(&page), 1);
        // A reservation into a live entry also takes a newer epoch, which
        // still releases pages that joined earlier.
        let mut unpublished = page.clone();
        unpublished.number = crate::model::PageNumber(1);
        let joined = index
            .reserve_warm_hold_pages(&version, &page_set(&[1]), until)
            .unwrap();
        assert!(joined > second);
        assert_eq!(index.hold_epoch(&version), Some(joined));
        index.release_hold_pages(&version, &page_set(&[0]), joined);
        assert_eq!(index.hold_holders(&page), 0);
        // Whole release followed by re-reservation starts a new membership.
        index.release_hold(&version).unwrap();
        assert_eq!(index.hold_epoch(&version), None);
        let third = index
            .reserve_hold_pages(&version, &page_set(&[1]), until)
            .unwrap();
        assert!(third > joined);
        index.release_hold_pages(&version, &page_set(&[1]), second);
        index.release_hold_pages(&version, &page_set(&[1]), joined);
        assert!(index.is_held(&unpublished));
        index.release_hold_pages(&version, &page_set(&[1]), third);
        assert!(!index.is_held(&unpublished));
    }

    /// Warm cleanup drops only warm holders from unpublished pages: a reader's
    /// page hold survives it and is still released by that reader's epoch.
    #[test]
    fn warm_cleanup_keeps_reader_holds_on_unpublished_pages() {
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 3 * crate::model::PAGE_BYTES), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let version = page.version.clone();
        let at = |number| PageId {
            version: version.clone(),
            number: crate::model::PageNumber(number),
        };
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_warm_hold_pages(&version, &page_set(&[0, 1, 2]), until)
            .unwrap();
        let reader = index
            .reserve_hold_pages(&version, &page_set(&[1]), until)
            .unwrap();
        index.release_hold(&version).unwrap();
        assert!(
            index.is_held(&at(1)),
            "the reader's holder survives warm cleanup"
        );
        assert_eq!(index.hold_holders(&at(1)), 1);
        assert!(!index.is_held(&at(2)), "warm-only unpublished pages leave");
        assert_eq!(
            index.hold_holders(&at(0)),
            1,
            "published pages keep holders"
        );
        assert_eq!(index.hold_unused(), 1, "the reader's page keeps its slot");
        index.release_hold_pages(&version, &page_set(&[1]), reader);
        assert!(!index.is_held(&at(1)));
        assert_eq!(index.hold_unused(), 0);
        assert!(index.is_held(&page));
    }

    /// A page that leaves a hold and is reserved again belongs to the new
    /// reservation: a delayed release taken before the trim cannot drop it,
    /// even while a published page keeps the version's entry alive.
    #[test]
    fn delayed_release_cannot_drop_a_trimmed_and_re_reserved_page() {
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 2 * crate::model::PAGE_BYTES), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let version = page.version.clone();
        let mut unpublished = page.clone();
        unpublished.number = crate::model::PageNumber(1);
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        let warm = index
            .reserve_warm_hold_pages(&version, &page_set(&[0, 1]), until)
            .unwrap();
        let reader = index
            .reserve_hold_pages(&version, &page_set(&[1]), until)
            .unwrap();
        // Trim the unpublished page: warm cleanup, then the reader's release.
        index.release_hold(&version).unwrap();
        index.release_hold_pages(&version, &page_set(&[1]), reader);
        assert!(!index.is_held(&unpublished));
        assert!(
            index.is_held(&page),
            "the published page keeps the entry alive"
        );
        // A new warm job reserves the trimmed page again.
        index
            .reserve_warm_hold_pages(&version, &page_set(&[1]), until)
            .unwrap();
        assert_eq!(index.hold_holders(&unpublished), 1);
        // Delayed releases from before the trim do not reach the new holder.
        index.release_hold_pages(&version, &page_set(&[1]), reader);
        index.release_hold_pages(&version, &page_set(&[1]), warm);
        assert!(index.is_held(&unpublished));
        assert_eq!(index.hold_holders(&unpublished), 1);
        assert_eq!(index.hold_unused(), 1);
        // The published page joined before both epochs, so they still apply.
        index.release_hold_pages(&version, &page_set(&[0]), reader);
        assert!(!index.is_held(&page));
        assert!(index.is_held(&unpublished));
    }

    #[test]
    fn whole_hold_release_drops_unpublished_pages_and_keeps_published_counts() {
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let mut unpublished = page.clone();
        unpublished.number = crate::model::PageNumber(1);
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        let epoch = index
            .reserve_warm_hold_pages(&page.version, &page_set(&[0, 1]), until)
            .unwrap();
        index
            .reserve_warm_hold_pages(&page.version, &page_set(&[0, 1]), until)
            .unwrap();
        index.release_hold(&page.version).unwrap();
        assert!(!index.is_held(&unpublished));
        assert_eq!(index.hold_holders(&unpublished), 0);
        assert_eq!(index.hold_holders(&page), 2);
        index.release_hold_pages(&page.version, &page_set(&[0, 1]), epoch);
        assert!(index.is_held(&page));
        index.release_hold_pages(&page.version, &page_set(&[0]), epoch);
        assert_eq!(index.hold_expiry(&page.version), None);
    }

    #[test]
    fn hold_pages_sort_dedup_insert_and_union() {
        let pages = page_set(&[5, 1, 3, 1]);
        assert_eq!(pages, page_set(&[1, 3, 5]));
        assert_eq!(pages.len(), 3);
        assert!(pages.contains(crate::model::PageNumber(3)));
        assert!(!pages.contains(crate::model::PageNumber(2)));
        let mut grown = pages.clone();
        grown.insert(crate::model::PageNumber(7));
        grown.insert(crate::model::PageNumber(2));
        grown.insert(crate::model::PageNumber(3));
        assert_eq!(grown, page_set(&[1, 2, 3, 5, 7]));
        assert_eq!(
            pages.union(&page_set(&[0, 3, 9])),
            page_set(&[0, 1, 3, 5, 9])
        );
        assert_eq!(pages.union(&page_set(&[])), pages);
        assert!(super::super::holds::HoldPages::prefix(0).is_empty());
        assert_eq!(super::super::holds::HoldPages::prefix(2), page_set(&[0, 1]));
    }

    #[test]
    fn page_set_hold_ignores_cached_nonmember_and_charges_only_members() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        index.set_page_capacity(2).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (cached, entry) = indexed(descriptor("primary", 2 * crate::model::PAGE_BYTES), 0);
        drop(segments.append(1024).unwrap());
        index.publish(cached.clone(), entry).unwrap();
        let mut primary = cached.clone();
        primary.number = crate::model::PageNumber(1);
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_hold_pages(&cached.version, &page_set(&[]), until)
            .unwrap();
        assert_eq!(
            index.hold_expiry(&cached.version),
            None,
            "empty set pins nothing"
        );
        index
            .reserve_hold_pages(&cached.version, &page_set(&[1]), until)
            .unwrap();
        assert!(!index.is_held(&cached), "cached nonmember stays evictable");
        assert!(index.is_held(&primary));
        assert_eq!(index.hold_unused(), 1, "the missing member keeps its slot");
        assert!(page_alloc::SegmentEntries::can_evict(&*index, SegmentId(0)));
        // Capacity 2 holds the cached page plus the reserved member slot.
        assert!(matches!(index.reserve_page(false), Err(Error::Overloaded)));
        let (_, mut entry) = indexed(descriptor("primary", 2 * crate::model::PAGE_BYTES), 1);
        drop(segments.append(1024).unwrap());
        entry.location.extent = Extent::new(1024, 512).unwrap();
        let ticket = index.reserve_page_for(&primary, false).unwrap();
        ticket.publish(primary.clone(), entry).unwrap();
        assert_eq!(index.held_page_expiry(&primary), Some(until));
        assert_eq!(index.held_page_expiry(&cached), None);
        assert_eq!(index.hold_unused(), 0);
        assert_eq!(index.held_snapshot().pages, 1);
        index.release_hold(&cached.version).unwrap();
        assert_eq!(index.held_page_expiry(&primary), Some(until));
        assert!(!index.is_held(&cached));
    }

    #[test]
    fn page_set_hold_counts_cached_members_and_unions_earlier_promises() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (cached, entry) = indexed(descriptor("union", 3 * crate::model::PAGE_BYTES), 0);
        drop(segments.append(1024).unwrap());
        index.publish(cached.clone(), entry).unwrap();
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_warm_hold_pages(&cached.version, &page_set(&[0, 2]), until)
            .unwrap();
        assert_eq!(index.held_page_expiry(&cached), Some(until));
        assert_eq!(
            index.hold_unused(),
            1,
            "the cached member covers its own charge"
        );
        let later = until + std::time::Duration::from_secs(30);
        index
            .reserve_warm_hold_pages(&cached.version, &page_set(&[1]), later)
            .unwrap();
        assert_eq!(index.hold_unused(), 2, "union keeps page 2 and adds page 1");
        for number in 0..3 {
            let mut page = cached.clone();
            page.number = crate::model::PageNumber(number);
            assert!(index.is_held(&page));
        }
        assert_eq!(index.hold_expiry(&cached.version), Some(later));
        index.extend_hold(&cached.version, until).unwrap();
        assert_eq!(
            index.hold_unused(),
            2,
            "extension keeps every promised page"
        );
        assert_eq!(index.hold_expiry(&cached.version), Some(later));
        let mut other = descriptor("other", crate::model::PAGE_BYTES).version;
        other.etag = crate::model::StrongEtag::test_value("other-union");
        assert_eq!(
            index.reserve_hold_pages(&other, &page_set(&[0]), until),
            Err(Error::Overloaded),
            "three members already reach the hold limit"
        );
        index.release_hold(&cached.version).unwrap();
        assert_eq!(index.hold_unused(), 0);
        assert_eq!(index.held_page_expiry(&cached), Some(later));
        index
            .reserve_hold_pages(&other, &page_set(&[0]), until)
            .unwrap();
    }

    #[test]
    fn restored_hold_rebuilds_sorted_page_set_from_unordered_visits() {
        let segments = Rc::new(segments(1024, 64));
        let index = Rc::new(index(64));
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let count = 40u64;
        let bytes = count * crate::model::PAGE_BYTES;
        let (first, _) = indexed(descriptor("many", bytes), 0);
        for number in 0..count {
            let (mut page, entry) = indexed(descriptor("many", bytes), number);
            page.number = crate::model::PageNumber(number);
            drop(segments.append(1024).unwrap());
            index.publish(page, entry).unwrap();
        }
        let held: Vec<u64> = (0..count).rev().step_by(3).collect();
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_hold_pages(&first.version, &page_set(&held), until)
            .unwrap();
        let mut snapshots = segments.snapshot();
        let mut remaining = usize::MAX;
        index
            .snapshot_holds(&mut snapshots, &mut remaining)
            .unwrap();
        index.holds.borrow_mut().entries.clear();
        index.restore_holds(&snapshots).unwrap();
        let holds = index.holds.borrow();
        let hold = &holds.entries[&first.version];
        assert_eq!(hold.pages, page_set(&held));
        let restored: Vec<u64> = hold.pages.iter().map(|p| p.0).collect();
        let mut expected = held.clone();
        expected.sort_unstable();
        assert_eq!(restored, expected);
        assert_eq!(hold.locations.len(), held.len());
    }

    #[test]
    fn restored_sparse_hold_counts_only_held_pages_at_capacity_edge() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        index.set_page_capacity(4).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (first, _) = indexed(descriptor("sparse", 3 * crate::model::PAGE_BYTES), 0);
        let mut pages = Vec::new();
        for number in 0..3u64 {
            let (mut page, entry) =
                indexed(descriptor("sparse", 3 * crate::model::PAGE_BYTES), number);
            page.number = crate::model::PageNumber(number);
            drop(segments.append(1024).unwrap());
            index.publish(page.clone(), entry).unwrap();
            pages.push(page);
        }
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_hold_pages(&first.version, &page_set(&[1]), until)
            .unwrap();
        assert_eq!(index.held_snapshot().pages, 1);
        let mut snapshots = segments.snapshot();
        let mut remaining = usize::MAX;
        index
            .snapshot_holds(&mut snapshots, &mut remaining)
            .unwrap();
        let image = index.snapshot().unwrap();
        index.holds.borrow_mut().entries.clear();
        // Three cached pages exceed the limit of two; one held page does not.
        index.validate_restored_holds(&image, &snapshots).unwrap();
        index.restore_holds(&snapshots).unwrap();
        assert!(!index.is_held(&pages[0]), "nonmember stays evictable");
        assert_eq!(index.held_page_expiry(&pages[1]), Some(until));
        assert!(!index.is_held(&pages[2]), "nonmember stays evictable");
        assert_eq!(index.held_snapshot().pages, 1);
    }

    #[test]
    fn held_snapshot_counts_only_live_publications_and_distinct_segments() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        let (first, entry) = indexed(descriptor("first", 17), 0);
        index.reserve_hold(&first.version, 2, until).unwrap();
        let pending = index.reserve_page_for(&first, false).unwrap();
        assert_eq!(index.held_snapshot(), HeldSnapshot::default());
        drop(segments.append(1024).unwrap());
        pending.publish(first.clone(), entry).unwrap();
        let (second, mut entry) = indexed(descriptor("second", 3), 0);
        entry.location.extent = Extent::new(512, 512).unwrap();
        index.publish(second.clone(), entry.clone()).unwrap();
        index.reserve_hold(&second.version, 1, until).unwrap();
        let expected = HeldSnapshot {
            pages: 2,
            payload_bytes: 20,
            segments: 1,
        };
        assert_eq!(index.held_snapshot(), expected);
        assert_eq!(index.held_snapshot(), expected);
        index.release_hold(&first.version).unwrap();
        assert_eq!(index.held_snapshot(), expected);
        index.remove_if_matches(&second, &entry.location);
        assert_eq!(
            index.held_snapshot(),
            HeldSnapshot {
                pages: 1,
                payload_bytes: 17,
                segments: 1
            }
        );
        let mut snapshots = segments.snapshot();
        let mut remaining = usize::MAX;
        index
            .snapshot_holds(&mut snapshots, &mut remaining)
            .unwrap();
        index.holds.borrow_mut().entries.clear();
        index.restore_holds(&snapshots).unwrap();
        assert_eq!(
            index.held_snapshot().payload_bytes,
            17,
            "recovered holds have no job"
        );
        {
            let mut holds = index.holds.borrow_mut();
            let hold = holds.entries.get_mut(&first.version).unwrap();
            hold.until = std::time::UNIX_EPOCH;
            assert!(hold.live(), "monotonic deadline still protects pages");
        }
        assert_eq!(index.held_snapshot().pages, 1);
        index
            .holds
            .borrow_mut()
            .entries
            .get_mut(&first.version)
            .unwrap()
            .monotonic = uring_runtime::environment::now() - std::time::Duration::from_secs(1);
        assert_eq!(index.held_snapshot(), HeldSnapshot::default());
        assert!(
            index.lookup(&first).unwrap().is_some(),
            "expiry need not evict the page"
        );
    }

    #[test]
    fn hard_hold_rejects_already_evicting_mapping_atomically() {
        let segments = Rc::new(segments(1024, 4));
        let index = Rc::new(index(8));
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("evicting", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        segments.begin_evict(SegmentId(0)).unwrap();
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        assert_eq!(
            index.reserve_hold(&page.version, 1, until),
            Err(Error::Overloaded)
        );
        assert_eq!(index.hold_expiry(&page.version), None);
        assert_eq!(index.hold_unused(), 0);
    }

    #[test]
    fn hard_hold_partial_failure_preserves_completed_page_and_deadline() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("partial", 17), 0);
        let until = uring_runtime::environment::wall_now() + std::time::Duration::from_secs(60);
        index.reserve_hold(&page.version, 2, until).unwrap();
        assert_eq!(index.held_page_expiry(&page), None);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        assert_eq!(
            index.reserve_hold(&page.version, 5, until + std::time::Duration::from_secs(60)),
            Err(Error::Overloaded)
        );
        index.release_hold(&page.version).unwrap();
        assert_eq!(index.hold_unused(), 0);
        assert_eq!(index.held_page_expiry(&page), Some(until));
        assert!(index.lookup(&page).unwrap().is_some());
    }

    #[test]
    fn hard_hold_reserves_index_slots_before_payload_and_bounds_ttl() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        index.set_page_capacity(4).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments, 1);
        let version = descriptor("held", 17).version;
        let now = uring_runtime::environment::wall_now();
        assert_eq!(
            index.reserve_hold(&version, 1, now),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(
            index.reserve_hold(
                &version,
                1,
                now + super::super::holds::MAX_HOLD_TTL + std::time::Duration::from_secs(1)
            ),
            Err(Error::InvalidConfiguration)
        );
        index
            .reserve_hold(&version, 2, now + std::time::Duration::from_secs(60))
            .unwrap();
        let a = index.reserve_page(false).unwrap();
        let b = index.reserve_page(false).unwrap();
        assert!(matches!(index.reserve_page(true), Err(Error::Overloaded)));
        let page = PageId {
            version: version.clone(),
            number: crate::model::PageNumber(0),
        };
        let held = index.reserve_page_for(&page, false).unwrap();
        assert_eq!(index.release_hold(&version), Err(Error::Overloaded));
        drop(held);
        drop((a, b));
        index.release_hold(&version).unwrap();
        assert_eq!(index.hold_unused(), 0);
        index
            .reserve_hold(&version, 1, now + std::time::Duration::from_secs(60))
            .unwrap();
        let old_ticket = index.reserve_page_for(&page, false).unwrap();
        index.remove_cache(&version.object.cache);
        index
            .reserve_hold(&version, 1, now + std::time::Duration::from_secs(60))
            .unwrap();
        drop(old_ticket);
        index.release_hold(&version).unwrap();
        assert_eq!(index.hold_expiry(&version), None);
    }

    #[test]
    fn hard_hold_expiry_requires_both_clocks() {
        let segments = Rc::new(segments(1024, 4));
        let index = Rc::new(index(8));
        let _clock = SegmentClock::new(index.clone(), segments, 1);
        let version = descriptor("ttl", 17).version;
        let now = uring_runtime::environment::wall_now();
        index
            .reserve_hold(&version, 1, now + std::time::Duration::from_secs(60))
            .unwrap();
        index
            .holds
            .borrow_mut()
            .entries
            .get_mut(&version)
            .unwrap()
            .until = now - std::time::Duration::from_secs(1);
        assert!(index.hold_expiry(&version).is_some());
        index
            .holds
            .borrow_mut()
            .entries
            .get_mut(&version)
            .unwrap()
            .monotonic = uring_runtime::environment::now() - std::time::Duration::from_secs(1);
        assert_eq!(index.hold_expiry(&version), None);
        assert_eq!(index.hold_unused(), 0);
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
