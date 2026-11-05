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
use racer_wire::control::KeyId;
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

    /// Worker retention policy; startup wiring replaces it once.
    retention: RefCell<Rc<Retention>>,

    holds: RefCell<super::holds::Holds>,

    /// Keyring generation and age cursor of the unavailable-key sweep.
    key_sweep: Cell<(Option<u64>, Option<u64>)>,

    /// Worker time for freshness, retention scoring, and hold expiry.
    clock: racer_io::Clock,
}
use super::pages::Pages;
use super::pages::Walk;

/// Expired holds released before one capacity decision.
const HOLD_EXPIRY_BATCH: usize = 16;

/// Default page capacity before startup configuration.
const DEFAULT_PAGE_CAPACITY: usize = 65536;

/// Interned page slots: the page capacity plus headroom for held pages that
/// are not stored yet, at the hold percentage of the capacity.
fn page_slots(capacity: usize, hold_percent: usize) -> Result<u32> {
    capacity
        .checked_mul(hold_percent)
        .map(|held| held / 100)
        .and_then(|held| held.checked_add(capacity))
        .and_then(|slots| u32::try_from(slots).ok())
        .filter(|slots| *slots < u32::MAX)
        .ok_or(Error::InvalidConfiguration)
}

struct State {
    pages: Pages,

    metadata: HashMap<ObjectVersion, VersionMetadata>,

    order: VecDeque<ObjectVersion>,

    current: HashMap<ObjectId, CurrentVersion>,
}
impl State {
    fn new(slots: u32) -> Self {
        Self {
            pages: Pages::new(slots),
            metadata: HashMap::default(),
            order: VecDeque::new(),
            current: HashMap::default(),
        }
    }
}
/// Capacity ownership acquired before a disk SQE. [`Self::append`] then
/// reserves the page's index slot and its record, whose write in flight keeps
/// the segment from eviction. Drop, including cancellation of the persisting
/// future, releases all of it.
pub struct PageTicket {
    index: Rc<Index>,
    counted: bool,
    reservation: Option<super::pages::Reservation>,
}
impl Drop for PageTicket {
    fn drop(&mut self) {
        if std::mem::take(&mut self.counted) {
            self.index.reserved.set(self.index.reserved.get() - 1);
        }
        if let Some(reservation) = self.reservation.take() {
            self.index.state.borrow_mut().pages.release(reservation);
        }
    }
}
impl PageTicket {
    /// Reserve `page`'s index slot and append `length` bytes for it. Keep the
    /// lease until the write completes. On error the ticket is unchanged and
    /// may be retried. `Overloaded` while another write of `page` is in flight.
    pub fn append(
        &mut self,
        page: &PageId,
        segments: &Segments,
        length: usize,
    ) -> Result<(page_alloc::SegmentLease, RecordLocation)> {
        if self.reservation.is_none() {
            self.reservation = Some(self.index.state.borrow_mut().pages.reserve(page)?);
        }
        self.reservation
            .as_mut()
            .expect("reserved page")
            .append(segments, length)
    }

    /// Publish the completed write at the location [`Self::append`] returned.
    pub fn publish(mut self, page: PageId, entry: IndexedPage) -> Result<()> {
        if std::mem::take(&mut self.counted) {
            self.index.reserved.set(self.index.reserved.get() - 1);
        }
        let reservation = self.reservation.take();
        // Only tests publish a caller-chosen location through a ticket.
        #[cfg(not(test))]
        if reservation.is_none() {
            return Err(Error::InvalidConfiguration);
        }
        let index = self.index.clone();
        drop(self);
        index.publish_with(page, entry, reservation)
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
/// Per-byte segment value of one page; see
/// [`racer_core::cache::class_score`].
fn page_value(scoring: &crate::retention::Scoring<'_>, page: &PageId, heat: u32) -> u8 {
    let any = scoring.any_over_share();
    racer_core::cache::class_score(
        scoring.score(page, heat),
        any,
        any && scoring.over_share(&page.version.object.cache),
    )
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
        self.expire_holds(usize::MAX);
        let mut holds = self.holds.borrow_mut();
        if percent > 100 || !holds.entries.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let slots = page_slots(self.page_capacity.get(), percent)?;
        self.state.borrow_mut().pages.resize(slots)?;
        holds.percent = percent;
        Ok(())
    }

    /// Hold pages `0..pages` of `version` with one reader holder each.
    #[cfg(test)]
    pub fn reserve_hold(
        &self,
        version: &ObjectVersion,
        pages: usize,
        until: std::time::SystemTime,
    ) -> Result<()> {
        if pages > self.page_capacity.get() {
            return Err(Error::Overloaded);
        }
        self.reserve_hold_pages(
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
    /// identifies this reservation.
    pub fn reserve_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        until: std::time::SystemTime,
    ) -> Result<super::holds::HoldEpoch> {
        self.reserve_hold_inner(version, pages, until, true)
    }

    fn reserve_hold_inner(
        &self,
        version: &ObjectVersion,
        pages: &super::holds::HoldPages,
        until: std::time::SystemTime,
        add_holders: bool,
    ) -> Result<super::holds::HoldEpoch> {
        let monotonic = super::holds::Holds::deadline(until, &self.clock)?;
        if !self.available(&version.object.cache) {
            return Err(Error::MissingKey);
        }
        self.expire_holds(HOLD_EXPIRY_BATCH);
        let mut state = self.state.borrow_mut();
        let mut holds = self.holds.borrow_mut();
        // An expired entry that is not swept yet no longer holds anything.
        if holds
            .entries
            .get(version)
            .is_some_and(|hold| !hold.live(&self.clock))
        {
            holds.drop_version(&mut state.pages, version);
        }
        let segments = holds.segments.clone().ok_or(Error::InvalidConfiguration)?;
        let old = holds.entries.get(version);
        let members = old.map_or_else(|| pages.clone(), |hold| hold.pages.union(pages));
        // Empty sets need no disk guarantee or retained reservation entry.
        if members.is_empty() {
            return Ok(super::holds::HoldEpoch(state.pages.last_hold_id()));
        }
        let mut page = PageId {
            version: version.clone(),
            number: crate::model::PageNumber(0),
        };
        // Pages that join the hold, and how many of them charge capacity.
        let mut joining = Vec::new();
        let mut joining_unused = 0;
        for number in members.iter() {
            page.number = number;
            if let Some(location) = state.pages.location(&page) {
                segments
                    .validate(location.segment, location.generation, &location.extent)
                    .map_err(|_| Error::Overloaded)?;
            }
            if old.is_none_or(|hold| !hold.holders.contains_key(&number)) {
                joining_unused += usize::from(!state.pages.occupied(&page));
                joining.push(page.clone());
            }
        }
        let total = holds
            .members
            .checked_add(joining.len())
            .ok_or(Error::Overloaded)?;
        let limit = (segments.count() * holds.percent / 100)
            .min(segments.count().saturating_sub(holds.free_reserve.max(1)))
            .min(self.page_capacity.get() * holds.percent / 100);
        if total > limit {
            return Err(Error::Overloaded);
        }
        if state.pages.len() + self.reserved.get() + state.pages.holds_unused() + joining_unused
            > self.page_capacity.get()
        {
            return Err(Error::Overloaded);
        }
        if add_holders
            && let Some(old) = old
            && pages.iter().any(|number| {
                old.holders
                    .get(&number)
                    .is_some_and(|holder| holder.count == u32::MAX)
            })
        {
            return Err(Error::Overloaded);
        }
        let until = old.map_or(until, |hold| hold.until.max(until));
        let monotonic = old.map_or(monotonic, |hold| hold.monotonic.max(monotonic));
        let now = holds.at(self.clock.now());
        let deadline = holds.at(monotonic);
        if let Some(old) = old
            && monotonic > old.monotonic
        {
            for id in &old.ids {
                state.pages.extend_hold(*id, deadline, now)?;
            }
        }
        let joined = if joining.is_empty() {
            None
        } else {
            Some(state.pages.hold(&joining, deadline, now)?)
        };
        let epoch = super::holds::HoldEpoch(state.pages.last_hold_id());
        holds.members += joining.len();
        let hold = holds
            .entries
            .entry(version.clone())
            .or_insert_with(|| super::holds::Hold {
                until,
                monotonic,
                pages: super::holds::HoldPages::default(),
                holders: HashMap::default(),
                ids: Vec::new(),
                active: Rc::new(Cell::new(0)),
            });
        hold.until = until;
        hold.monotonic = monotonic;
        hold.pages = members;
        if let Some((id, slots)) = joined {
            hold.ids.push(id);
            for (page, slot) in joining.iter().zip(slots) {
                hold.holders.insert(
                    page.number,
                    super::holds::PageHolders {
                        count: 0,
                        since: epoch,
                        slot,
                    },
                );
            }
            holds.owners.insert(id, version.clone());
        }
        if add_holders {
            let hold = holds.entries.get_mut(version).expect("inserted hold");
            for number in pages.iter() {
                hold.holders.get_mut(&number).expect("member").count += 1;
            }
        }
        Ok(epoch)
    }

    /// Release up to `budget` expired holds. Runs every maintenance turn and,
    /// with a small budget, before capacity decisions.
    pub fn expire_holds(&self, budget: usize) {
        let mut state = self.state.borrow_mut();
        let mut holds = self.holds.borrow_mut();
        if holds.entries.is_empty() {
            return;
        }
        let now = holds.at(self.clock.now());
        let mut expired = Vec::new();
        state.pages.expire_holds(now, budget, |id| expired.push(id));
        for id in expired {
            holds.forget(id);
        }
    }

    /// Active reservation deadline, not proof that every requested page is on disk.
    pub fn hold_expiry(&self, version: &ObjectVersion) -> Option<std::time::SystemTime> {
        self.holds
            .borrow()
            .entries
            .get(version)
            .filter(|hold| hold.live(&self.clock))
            .map(|hold| hold.until)
    }

    /// Scan only bounded hold locations, not the full page index or payloads.
    /// Scratch space contains distinct segment IDs, bounded by held publications.
    pub fn held_snapshot(&self) -> HeldSnapshot {
        let holds = self.holds.borrow();
        let state = self.state.borrow();
        let mut snapshot = HeldSnapshot::default();
        let mut segments = HashSet::default();
        for (version, hold) in holds
            .entries
            .iter()
            .filter(|(_, hold)| hold.live(&self.clock))
        {
            let mut page = PageId {
                version: version.clone(),
                number: crate::model::PageNumber(0),
            };
            for number in hold.pages.iter() {
                page.number = number;
                let Some(entry) = state.pages.get(&page) else {
                    continue;
                };
                let Ok(bytes) = entry.metadata.page_length(&page) else {
                    continue;
                };
                snapshot.pages = snapshot.pages.saturating_add(1);
                snapshot.payload_bytes = snapshot.payload_bytes.saturating_add(u64::from(bytes));
                segments.insert(entry.location.segment);
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
            .filter(|hold| hold.live(&self.clock))
            .map(|hold| hold.pages.clone())
            .ok_or(Error::Unavailable)?;
        // Extension renews the deadline only; it adds no holders.
        self.reserve_hold_inner(version, &pages, until, false)
            .map(drop)
    }

    /// Test helper: drop every unpublished member of `version`'s hold, with
    /// all its holders. Published members keep theirs.
    #[cfg(test)]
    pub fn release_hold(&self, version: &ObjectVersion) -> Result<()> {
        let mut state = self.state.borrow_mut();
        let mut holds = self.holds.borrow_mut();
        let Some(hold) = holds.entries.get(version) else {
            return Ok(());
        };
        if hold.active.get() != 0 {
            return Err(Error::Overloaded);
        }
        let mut page = PageId {
            version: version.clone(),
            number: crate::model::PageNumber(0),
        };
        let unpublished: Vec<_> = hold
            .pages
            .iter()
            .filter(|number| {
                page.number = *number;
                !state.pages.contains_key(&page)
            })
            .collect();
        Self::drop_members(&mut state, &mut holds, version, &unpublished);
        Ok(())
    }

    /// Remove `numbers` from `version`'s hold with all their holders.
    fn drop_members(
        state: &mut State,
        holds: &mut super::holds::Holds,
        version: &ObjectVersion,
        numbers: &[crate::model::PageNumber],
    ) {
        let Some(hold) = holds.entries.get_mut(version) else {
            return;
        };
        let mut gone = Vec::new();
        for number in numbers {
            let Some(holder) = hold.holders.remove(number) else {
                continue;
            };
            let id = holder.since.0;
            if !state.pages.release_hold_slot(id, holder.slot) {
                hold.ids.retain(|held| *held != id);
                gone.push(id);
            }
        }
        let holders = &hold.holders;
        let before = hold.pages.len();
        hold.pages = hold
            .pages
            .iter()
            .filter(|page| holders.contains_key(page))
            .collect();
        let removed = before - hold.pages.len();
        let empty = hold.pages.is_empty();
        holds.members -= removed;
        for id in gone {
            holds.owners.remove(&id);
        }
        if empty {
            holds.drop_version(&mut state.pages, version);
        }
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
        let mut state = self.state.borrow_mut();
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
                if holder.count == 0 {
                    released.push(number);
                }
            }
        }
        if !released.is_empty() {
            Self::drop_members(&mut state, &mut holds, version, &released);
        }
    }

    /// Epoch of the latest reservation that added members to `version`'s hold.
    #[cfg(test)]
    pub fn hold_epoch(&self, version: &ObjectVersion) -> Option<super::holds::HoldEpoch> {
        self.holds
            .borrow()
            .entries
            .get(version)
            .and_then(|hold| hold.ids.iter().max().copied())
            .map(super::holds::HoldEpoch)
    }

    /// Holder count of `page` in a live hold, or zero when it is not held.
    pub fn hold_holders(&self, page: &PageId) -> u32 {
        self.holds
            .borrow()
            .entries
            .get(&page.version)
            .filter(|hold| hold.live(&self.clock))
            .and_then(|hold| hold.holders.get(&page.number).map(|holder| holder.count))
            .unwrap_or(0)
    }

    /// Whether `page` is a member of a live hold, published or not.
    pub fn is_held(&self, page: &PageId) -> bool {
        self.holds
            .borrow()
            .entries
            .get(&page.version)
            .is_some_and(|hold| hold.live(&self.clock) && hold.pages.contains(page.number))
    }

    pub(super) fn protect_hold(&self, version: &ObjectVersion) -> Result<super::holds::HoldGuard> {
        let holds = self.holds.borrow();
        let hold = holds
            .entries
            .get(version)
            .filter(|hold| hold.live(&self.clock))
            .ok_or(Error::Unavailable)?;
        hold.active
            .set(hold.active.get().checked_add(1).ok_or(Error::Overloaded)?);
        Ok(super::holds::HoldGuard(hold.active.clone()))
    }

    /// Deadline of a published held page. A HEAD reservation alone returns None.
    pub fn held_page_expiry(&self, page: &PageId) -> Option<std::time::SystemTime> {
        let holds = self.holds.borrow();
        let hold = holds
            .entries
            .get(&page.version)
            .filter(|hold| hold.live(&self.clock) && hold.pages.contains(page.number))?;
        self.state
            .borrow()
            .pages
            .contains_key(page)
            .then_some(hold.until)
    }

    /// Held pages that are neither stored nor being written.
    fn held_unused(state: &State) -> usize {
        state.pages.holds_unused()
    }

    #[cfg(test)]
    fn hold_unused(&self) -> usize {
        self.expire_holds(usize::MAX);
        Self::held_unused(&self.state.borrow())
    }

    /// Whether `page` is held and charged to [`Self::hold_unused`], so
    /// writing it consumes held capacity instead of a free slot.
    fn hold_available(state: &State, page: &PageId) -> bool {
        state.pages.is_unused_held(page)
    }

    /// Append every live published hold as (page, remaining duration). The
    /// remaining time comes from the monotonic deadline only, so a wall-clock
    /// step can neither extend nor shorten a hold across a restart.
    pub(super) fn snapshot_holds(
        &self,
        out: &mut Vec<(PageId, std::time::Duration)>,
        remaining: &mut usize,
    ) -> Result<()> {
        let holds = self.holds.borrow();
        let state = self.state.borrow();
        let now = self.clock.now();
        for (version, hold) in holds
            .entries
            .iter()
            .filter(|(_, hold)| hold.live(&self.clock))
        {
            let ttl = hold.monotonic.saturating_duration_since(now);
            for number in hold.pages.iter() {
                let page = PageId {
                    version: version.clone(),
                    number,
                };
                if !state.pages.contains_key(&page) {
                    continue;
                }
                *remaining = remaining
                    .checked_sub(super::checkpoint::HELD_RECORD_BYTES)
                    .ok_or(Error::Overloaded)?;
                out.push((page, ttl));
            }
        }
        Ok(())
    }

    /// Drop every hold.
    fn clear_holds(&self) {
        let mut state = self.state.borrow_mut();
        let mut holds = self.holds.borrow_mut();
        let versions: Vec<_> = holds.entries.keys().cloned().collect();
        for version in versions {
            holds.drop_version(&mut state.pages, &version);
        }
    }

    /// Rebuild holds from checkpointed remaining durations, measured from now.
    /// Holder identities do not survive restart; each restored page keeps
    /// one reader holder until expiry or `release_hold_pages`.
    pub(super) fn restore_holds(&self, held: &[(PageId, std::time::Duration)]) -> Result<()> {
        self.clear_holds();
        let wall = self.clock.wall_now();
        let now = self.clock.now();
        // Collect each version's stored pages and latest deadline.
        let mut versions: HashMap<
            ObjectVersion,
            (std::time::SystemTime, std::time::Instant, Vec<PageId>),
        > = HashMap::default();
        let mut state = self.state.borrow_mut();
        for (page, ttl) in held {
            if !state.pages.contains_key(page) {
                continue;
            }
            let (Some(until), Some(monotonic)) = (wall.checked_add(*ttl), now.checked_add(*ttl))
            else {
                continue;
            };
            let entry = versions
                .entry(page.version.clone())
                .or_insert_with(|| (until, monotonic, Vec::new()));
            entry.0 = entry.0.max(until);
            entry.1 = entry.1.max(monotonic);
            entry.2.push(page.clone());
        }
        let mut holds = self.holds.borrow_mut();
        let at = holds.at(now);
        for (version, (until, monotonic, pages)) in versions {
            let (id, slots) = state.pages.hold(&pages, holds.at(monotonic), at)?;
            let since = super::holds::HoldEpoch(id);
            let holders: HashMap<_, _> = pages
                .iter()
                .zip(slots)
                .map(|(page, slot)| {
                    (
                        page.number,
                        super::holds::PageHolders {
                            count: 1,
                            since,
                            slot,
                        },
                    )
                })
                .collect();
            let members: super::holds::HoldPages = pages.iter().map(|page| page.number).collect();
            holds.members += members.len();
            holds.owners.insert(id, version.clone());
            holds.entries.insert(
                version,
                super::holds::Hold {
                    until,
                    monotonic,
                    pages: members,
                    holders,
                    ids: vec![id],
                    active: Rc::new(Cell::new(0)),
                },
            );
        }
        Ok(())
    }

    pub(super) fn validate_restored_holds(
        &self,
        snapshot: &IndexSnapshot,
        held: &[(PageId, std::time::Duration)],
        segment_count: usize,
    ) -> Result<()> {
        let pages: HashSet<&PageId> = snapshot.entries.iter().map(|(page, _)| page).collect();
        let mut count = 0;
        for (page, ttl) in held {
            if ttl.is_zero() || *ttl > super::holds::MAX_HOLD_TTL {
                return Err(Error::InvalidConfiguration);
            }
            count += usize::from(pages.contains(page));
        }
        let holds = self.holds.borrow();
        let limit = (segment_count * holds.percent / 100)
            .min(segment_count.saturating_sub(holds.free_reserve.max(1)))
            .min(self.page_capacity.get() * holds.percent / 100);
        if count > limit || (count != 0 && holds.segments.is_none()) {
            return Err(Error::Overloaded);
        }
        Ok(())
    }

    pub fn new(
        worker: WorkerId,
        metadata_capacity: usize,
        availability: Rc<crate::control::Availability>,
        clock: racer_io::Clock,
    ) -> Self {
        Self {
            clock: clock.clone(),
            worker,
            metadata_capacity,
            page_capacity: Cell::new(DEFAULT_PAGE_CAPACITY),
            state: RefCell::new(State::new(
                page_slots(DEFAULT_PAGE_CAPACITY, super::holds::DEFAULT_HOLD_PERCENT)
                    .expect("default page slots"),
            )),
            availability,
            reserved: Cell::new(0),
            retention: RefCell::new(Rc::new(
                Retention::new(256 * 1024, clock.clone()).expect("valid bounded retention"),
            )),
            holds: RefCell::new(super::holds::Holds::new(&clock)),
            key_sweep: Cell::default(),
        }
    }
    /// Worker time for this index's freshness and expiry decisions.
    pub fn clock(&self) -> &racer_io::Clock {
        &self.clock
    }
    fn available(&self, cache: &racer_wire::control::CacheId) -> bool {
        self.availability.metadata(cache)
    }
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
    pub fn metadata_capacity(&self) -> usize {
        self.metadata_capacity
    }
    /// This worker's payload capacity for fair shares: the tighter of index
    /// entries times a full page and slab segments times segment bytes. Either
    /// one bounds how much payload the worker can keep resident. Segment space
    /// is ignored until segments are bound.
    pub fn disk_capacity(&self) -> u64 {
        let pages = (self.page_capacity.get() as u64).saturating_mul(crate::model::PAGE_BYTES);
        self.holds
            .borrow()
            .segments
            .as_ref()
            .map_or(pages, |segments| {
                pages.min((segments.count() as u64).saturating_mul(segments.segment_bytes()))
            })
    }
    /// Startup only: key page interning from the worker's random source,
    /// so production hashing is unpredictable and simulation replays.
    pub fn seed_page_hash(&self, rng: &racer_io::Rng) -> Result<()> {
        let mut key = [0; 16];
        rng.fill(&mut key).map_err(|_| Error::Unavailable)?;
        self.state
            .borrow_mut()
            .pages
            .set_hash_key(page_alloc::intern::KeyedState::from_bytes(key))
    }
    pub fn page_capacity(&self) -> usize {
        self.page_capacity.get()
    }
    pub fn set_page_capacity(&self, capacity: usize) -> Result<()> {
        self.expire_holds(usize::MAX);
        let state = self.state.borrow();
        let used = state.pages.len() + self.reserved.get() + Self::held_unused(&state);
        drop(state);
        if capacity == 0 || capacity < used {
            return Err(Error::InvalidConfiguration);
        }
        let slots = page_slots(capacity, self.holds.borrow().percent)?;
        self.state.borrow_mut().pages.resize(slots)?;
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
            .map(super::pages::PageView::into_entry))
    }
    /// Allocation-free capacity preflight, not a slot reservation. Replacements
    /// remain admissible at capacity; new pages require an existing free slot.
    pub fn preflight_capacity(&self, page: &PageId) -> Result<()> {
        self.expire_holds(HOLD_EXPIRY_BATCH);
        let state = self.state.borrow();
        if state.pages.contains_key(page)
            || state.pages.len() + self.reserved.get() + Self::held_unused(&state)
                < self.page_capacity.get()
            || Self::hold_available(&state, page)
        {
            Ok(())
        } else {
            Err(Error::Overloaded)
        }
    }
    pub fn retention(&self) -> Rc<Retention> {
        self.retention.borrow().clone()
    }
    pub fn set_retention(&self, retention: Rc<Retention>) {
        // Startup wiring only: replacing a live policy would invalidate read-side
        // clones and strand the payload charges of stored pages.
        assert!(
            self.state.borrow().pages.len() == 0,
            "install retention before publishing pages"
        );
        *self.retention.borrow_mut() = retention;
    }
    /// Record a disk read of `page`, raising its segment's retention heat.
    pub fn touch(&self, page: &PageId) {
        self.state.borrow_mut().pages.touch(page);
    }
    /// Reserve one potential new mapping for `page`. A full index returns
    /// `Overloaded`; the caller frees entries with
    /// [`SegmentClock::reclaim_entries`], which evicts whole segments by score,
    /// so over-share caches lose space first. Replacement writes also reserve:
    /// concurrent invalidation may remove the old mapping while the SQE is in
    /// flight.
    pub(super) fn reserve_page_for(self: &Rc<Self>, page: &PageId) -> Result<PageTicket> {
        self.expire_holds(HOLD_EXPIRY_BATCH);
        let state = self.state.borrow();
        // A held page charged to the hold moves that charge to the ticket
        // here, and to its in-flight disk entry once appended.
        let held = Self::hold_available(&state, page);
        if state.pages.len() + self.reserved.get() + Self::held_unused(&state) - usize::from(held)
            >= self.page_capacity.get()
        {
            return Err(Error::Overloaded);
        }
        drop(state);
        self.reserved.set(self.reserved.get() + 1);
        Ok(PageTicket {
            index: self.clone(),
            counted: true,
            reservation: None,
        })
    }
    /// Atomically publish a completed record with its immutable descriptor. Reject
    /// conflicting lengths for one version; never update current-version freshness.
    pub fn publish(&self, page: PageId, entry: IndexedPage) -> Result<()> {
        self.publish_with(page, entry, None)
    }
    /// [`Self::publish`] of a reserved write, or of a validated location
    /// when `reservation` is `None`. A reservation is always consumed.
    fn publish_with(
        &self,
        page: PageId,
        entry: IndexedPage,
        mut reservation: Option<super::pages::Reservation>,
    ) -> Result<()> {
        let result = self.try_publish(page, entry, &mut reservation);
        if let Some(reservation) = reservation {
            self.state.borrow_mut().pages.release(reservation);
        }
        result
    }
    fn try_publish(
        &self,
        page: PageId,
        entry: IndexedPage,
        reservation: &mut Option<super::pages::Reservation>,
    ) -> Result<()> {
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
            && state.pages.len() + self.reserved.get() + Self::held_unused(&state)
                - usize::from(Self::hold_available(&state, &page))
                >= self.page_capacity.get()
        {
            return Err(Error::Overloaded);
        }
        let payload = self
            .retention()
            .indexed_payload(&page.version.object.cache, payload_bytes);
        let inserted = match reservation.take() {
            Some(reservation) => state.pages.publish(reservation, &page, entry),
            None => state.pages.insert(page.clone(), entry).map(|()| true),
        };
        if inserted != Ok(true) {
            return inserted.map(drop);
        }
        state.pages.charge(&page, payload);
        Ok(())
    }
    /// Look in both the standalone catalog and retained page entries for this exact
    /// version. This operation does not consult TTL or substitute another ETag.
    pub fn version(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        if !self.available(&version.object.cache) {
            return Ok(None);
        }
        let s = self.state.borrow();
        let descriptor = s
            .metadata
            .get(version)
            .or_else(|| s.pages.version(version))
            .cloned();
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
            .filter(|v| self.clock.wall_now() < v.expires_at.as_system_time())
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
        if self.clock.wall_now() < metadata.expires_at.as_system_time() {
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
    /// Drop published pages whose cache or page key is no longer admitted,
    /// examining at most `budget` entries in age order. Idle until the keyring
    /// generation changes, then resumes each call until one pass completes.
    /// Held pages are kept; their hold decides their lifetime. Returns the
    /// number of pages removed.
    pub fn sweep_unavailable(&self, budget: usize) -> usize {
        let generation = self.availability.key_generation();
        let (known, mut cursor) = self.key_sweep.get();
        if generation != known {
            cursor = Some(0);
        }
        let Some(after) = cursor else {
            return 0;
        };
        let (next, visited) =
            self.state
                .borrow_mut()
                .pages
                .walk(Walk::KeySweep, after, budget, |view| {
                    Some(
                        (!self
                            .availability
                            .page(&view.page.version.object.cache, view.key_id)
                            && !self.is_held(view.page))
                        .then(|| (view.page.clone(), view.location.clone())),
                    )
                });
        let examined = visited.len();
        self.key_sweep
            .set((generation, (examined == budget).then_some(next)));
        let stale: Vec<_> = visited.into_iter().flatten().collect();
        let removed = stale.len();
        for (page, location) in stale {
            self.remove_if_matches(&page, &location);
        }
        removed
    }
    pub fn remove_if_matches(&self, page: &PageId, location: &RecordLocation) {
        let mut s = self.state.borrow_mut();
        s.pages.remove(page, Some(location));
    }
    pub fn snapshot(&self) -> Result<IndexSnapshot> {
        let s = self.state.borrow();
        Ok(IndexSnapshot {
            entries: s.pages.iter().map(|view| view.into_owned()).collect(),
            metadata: s.metadata.values().cloned().collect(),
        })
    }
    /// Resumable publish-order walk for ownership refresh. Token 0 restarts at
    /// the oldest page; pass back the returned token to continue. Removals are
    /// safe omissions; a replaced page is visited again at the tail.
    pub fn snapshot_pages(&self, after: u64, limit: usize) -> (u64, Vec<(PageId, IndexedPage)>) {
        self.state
            .borrow_mut()
            .pages
            .walk(Walk::Ownership, after, limit, |view| {
                Some(view.into_owned())
            })
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
            .map(|view| super::checkpoint::snapshot_entry_bytes(view.metadata))
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
        for (version, hold) in holds
            .entries
            .iter()
            .filter(|(_, hold)| hold.live(&self.clock))
        {
            let mut page = PageId {
                version: version.clone(),
                number: crate::model::PageNumber(0),
            };
            let stored = hold
                .pages
                .iter()
                .filter(|number| {
                    page.number = *number;
                    state.pages.contains_key(&page)
                })
                .count();
            let bytes = stored
                .checked_mul(super::checkpoint::HELD_RECORD_BYTES)
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

    /// Checkpoint walk (token 0 restarts). Each mapping is charged before it
    /// is cloned; an exhausted budget fails the whole checkpoint.
    pub(super) fn snapshot_pages_budgeted(
        &self,
        after: u64,
        limit: usize,
        remaining: &mut usize,
    ) -> Result<(u64, Vec<(PageId, IndexedPage)>)> {
        let mut over = false;
        let result = self
            .state
            .borrow_mut()
            .pages
            .walk(Walk::Checkpoint, after, limit, |view| {
                match remaining.checked_sub(super::checkpoint::snapshot_entry_bytes(view.metadata))
                {
                    Some(left) => {
                        *remaining = left;
                        Some(view.into_owned())
                    }
                    None => {
                        over = true;
                        None
                    }
                }
            });
        if over {
            return Err(Error::Overloaded);
        }
        Ok(result)
    }
    /// Install only with the matching recovered segment state before admission.
    /// Validate identity/length agreement and catalog bounds; clear all freshness.
    pub fn restore(&self, snapshot: IndexSnapshot) -> Result<()> {
        self.validate_snapshot(&snapshot)?;
        let replacement = Index::new(
            self.worker,
            self.metadata_capacity,
            self.availability.clone(),
            self.clock.clone(),
        );
        *replacement.retention.borrow_mut() = self.retention();
        let hash_key = self.state.borrow().pages.hash_key();
        replacement
            .state
            .borrow_mut()
            .pages
            .set_hash_key(hash_key)?;
        replacement.set_page_capacity(self.page_capacity.get())?;
        for (p, e) in snapshot.entries {
            replacement.publish(p, e)?;
        }
        for m in snapshot.metadata {
            replacement.publish_version(m)?;
        }
        // Hold identities keep increasing across the replaced table, so a
        // release issued before the restore cannot match a later hold.
        self.clear_holds();
        let last_hold = self.state.borrow().pages.last_hold_id();
        replacement
            .state
            .borrow_mut()
            .pages
            .continue_hold_ids(last_hold);
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
        self.state.borrow().pages.segment(segment, budget)
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
            &retention.scoring_at(self.clock.now(), self.disk_capacity()),
        )
    }
    #[cfg(test)]
    fn segment_score_with(
        &self,
        segment: SegmentId,
        scoring: &crate::retention::Scoring<'_>,
    ) -> u64 {
        let state = self.state.borrow();
        state.pages.segment_score(
            segment,
            racer_core::cache::SCAN_BUDGET,
            racer_core::cache::unseen_value_for(crate::model::PAGE_BYTES, scoring.any_over_share()),
            |page, metadata, heat| {
                racer_core::cache::page_value(
                    metadata.page_length(page).ok(),
                    crate::model::PAGE_BYTES as u32,
                    page_value(scoring, page, heat),
                )
            },
        )
    }
    #[cfg(test)]
    pub fn remove_cache(&self, cache: &racer_wire::control::CacheId) {
        self.remove_caches(&std::collections::HashSet::from([cache.clone()]));
    }
    /// Remove holds, pages, and metadata for every cache in `caches` in one
    /// pass over each table.
    pub fn remove_caches(&self, caches: &std::collections::HashSet<racer_wire::control::CacheId>) {
        if caches.is_empty() {
            return;
        }
        let mut s = self.state.borrow_mut();
        {
            let mut holds = self.holds.borrow_mut();
            let removed: Vec<_> = holds
                .entries
                .keys()
                .filter(|version| caches.contains(&version.object.cache))
                .cloned()
                .collect();
            for version in removed {
                holds.drop_version(&mut s.pages, &version);
            }
        }
        s.pages.remove_caches(caches);
        s.metadata.retain(|v, _| !caches.contains(&v.object.cache));
        s.order.retain(|v| !caches.contains(&v.object.cache));
        s.current.retain(|o, _| !caches.contains(&o.cache));
    }
    fn check_length(s: &State, m: &VersionMetadata) -> Result<()> {
        if s.pages
            .version(&m.version)
            .is_some_and(|v| !v.compatible(m))
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
    segments: Rc<Segments>,
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
impl Index {
    /// Whether no held record keeps `segment`.
    #[cfg(test)]
    pub(crate) fn can_reclaim(&self, segment: SegmentId) -> bool {
        !self.state.borrow().pages.segment_held(segment)
    }
    /// Whether `segment` may be evicted: no held record and no write in flight.
    #[cfg(test)]
    pub(crate) fn can_evict(&self, segment: SegmentId) -> bool {
        let state = self.state.borrow();
        state.pages.can_reclaim(segment) && !state.pages.segment_held(segment)
    }
    /// Test helper: remove up to `budget` pages of an evictable `segment`.
    #[cfg(test)]
    pub(crate) fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize {
        if !self.can_evict(segment) {
            return 0;
        }
        let entries = self.segment_entries_bounded(segment, budget);
        let removed = entries.len();
        for (page, location) in entries {
            let bytes = self
                .state
                .borrow()
                .pages
                .get(&page)
                .expect("indexed page")
                .metadata
                .page_length(&page)
                .expect("validated indexed page");
            self.retention().evicted(&page, u64::from(bytes));
            self.remove_if_matches(&page, &location);
        }
        removed
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
            segments,
        }
    }
    /// Rank at most 64 slots before eviction. Busy segments remain Evicting until
    /// a later poll; ownership and heat cannot prevent eventual reclamation.
    pub fn reclaim_now(&self) -> Result<()> {
        self.reclaim_at(self.index.clock.now())
    }
    /// [`Self::reclaim_now`] with retention heat evaluated at an explicit time.
    pub fn reclaim_at(&self, now: std::time::Instant) -> Result<()> {
        self.reclaim_to(now, self.free_reserve)
    }
    /// Free index entries when the index is full: evict at least one more
    /// segment than is free or evicting now, chosen by the same score as
    /// space reclaim (lowest value first, over-share caches first).
    /// When nothing sealed can be evicted, the open tail is sealed and
    /// reclaimed instead (its unused bytes are recycled with it).
    pub fn reclaim_entries(&self) -> Result<()> {
        let before = self.index.state.borrow().pages.len();
        let result = self.reclaim_one();
        if self.index.state.borrow().pages.len() < before || !self.segments.seal_open() {
            return result;
        }
        self.reclaim_one()
    }
    fn reclaim_one(&self) -> Result<()> {
        let segments = &self.segments;
        let target = segments.free_count() + segments.evicting_count() + 1;
        self.reclaim_to(self.index.clock.now(), target.max(self.free_reserve))
    }
    fn reclaim_to(&self, now: std::time::Instant, free_reserve: usize) -> Result<()> {
        let retention = self.index.retention();
        let scoring = retention.scoring_at(now, self.index.disk_capacity());
        let unseen =
            racer_core::cache::unseen_value_for(crate::model::PAGE_BYTES, scoring.any_over_share());
        let mut evicted = Vec::new();
        let result = self.index.state.borrow_mut().pages.reclaim(
            &self.segments,
            page_alloc::disk_index::ReclaimBudget {
                free_reserve,
                max_visits: self.budget.segment_visits,
                max_entries: self.budget.mapping_removals,
            },
            racer_core::cache::SCAN_BUDGET,
            unseen,
            |page, metadata, heat| {
                racer_core::cache::page_value(
                    metadata.page_length(page).ok(),
                    crate::model::PAGE_BYTES as u32,
                    page_value(&scoring, page, heat),
                )
            },
            |page, metadata| {
                evicted.push((page.clone(), metadata.page_length(page).unwrap_or(0)));
            },
        );
        for (page, bytes) in evicted {
            retention.evicted(&page, u64::from(bytes));
        }
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::StrongEtag;
    use racer_wire::control::CacheId;

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
                racer_io::Clock::host(),
            )),
            segments.clone(),
            2,
        );
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
        Index::new(
            WorkerId(0),
            capacity,
            crate::test_support::availability(),
            crate::test_support::clock(),
        )
    }
    #[test]
    fn reclaim_prepares_one_scoring_pass() {
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
        let clock = SegmentClock::new(index.clone(), segments, 1);
        clock.reclaim_now().unwrap();
        assert_eq!(calls.get(), 1);
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
    /// An unheld page for capacity probes.
    fn probe() -> PageId {
        indexed(descriptor("probe", 17), 0).0
    }
    #[test]
    fn page_ticket_publishes_appended_write_and_cancellation_releases_everything() {
        use std::future::Future;
        let segments = Rc::new(segments(1024, 2));
        let index = Rc::new(index(8));
        index.set_page_capacity(1).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("a", 17), 0);
        let persist = {
            let (index, segments, page) = (index.clone(), segments.clone(), page.clone());
            async move {
                let mut ticket = index.reserve_page_for(&page)?;
                let (_lease, location) = ticket.append(&page, &segments, 512)?;
                std::future::pending::<()>().await; // the disk write never completes
                ticket.publish(page, IndexedPage { location, ..entry })
            }
        };
        let mut persist = Box::pin(persist);
        let mut context = std::task::Context::from_waker(std::task::Waker::noop());
        assert!(persist.as_mut().poll(&mut context).is_pending());
        let written = SegmentId(0);
        assert!(!index.can_evict(written));
        assert_eq!(index.preflight_capacity(&probe()), Err(Error::Overloaded));
        drop(persist);
        assert!(index.can_evict(written));
        assert_eq!(index.reserved.get(), 0);
        index.preflight_capacity(&probe()).unwrap();
        assert!(index.lookup(&page).unwrap().is_none());
        index.state.borrow().pages.check();
        // A location other than the appended one is refused and releases the ticket.
        let mut ticket = index.reserve_page_for(&page).unwrap();
        let (lease, location) = ticket.append(&page, &segments, 512).unwrap();
        drop(lease);
        let mut wrong = location.clone();
        wrong.generation = Generation(location.generation.0 + 1);
        let (_, entry) = indexed(descriptor("a", 17), 0);
        assert_eq!(
            ticket.publish(
                page.clone(),
                IndexedPage {
                    location: wrong,
                    ..entry.clone()
                }
            ),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(index.reserved.get(), 0);
        assert!(index.can_evict(location.segment));
        // The same steps publish the appended location.
        let mut ticket = index.reserve_page_for(&page).unwrap();
        let (lease, location) = ticket.append(&page, &segments, 512).unwrap();
        drop(lease);
        assert_eq!(
            index.reserve_page_for(&probe()).err(),
            Some(Error::Overloaded)
        );
        ticket
            .publish(
                page.clone(),
                IndexedPage {
                    location: location.clone(),
                    ..entry
                },
            )
            .unwrap();
        assert_eq!(index.lookup(&page).unwrap().unwrap().location, location);
        assert!(index.can_evict(location.segment));
        index.state.borrow().pages.check();
    }
    #[test]
    fn hard_hold_capacity_dedup_extension_release_and_eviction() {
        let segments = Rc::new(segments(1024, 6));
        let index = Rc::new(index(8));
        index.set_page_capacity(6).unwrap();
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (page, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(page.clone(), entry).unwrap();
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        index.reserve_hold(&page.version, 2, until).unwrap();
        index.reserve_hold(&page.version, 2, until).unwrap();
        index
            .extend_hold(&page.version, until - std::time::Duration::from_secs(1))
            .unwrap();
        assert_eq!(index.hold_expiry(&page.version), Some(until));
        assert!(!index.can_evict(SegmentId(0)));
        assert_eq!(index.remove_bounded(SegmentId(0), 1), 0);
        assert!(!index.can_evict(SegmentId(0)));
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        // One reader holds 0..3 and another independently holds 0 and 2.
        let epoch = index
            .reserve_hold_pages(&version, &page_set(&[0, 1, 2]), until)
            .unwrap();
        let read = index
            .reserve_hold_pages(&version, &page_set(&[0, 2]), until)
            .unwrap();
        assert!(read >= epoch, "a reservation never takes an older epoch");
        assert_eq!(index.hold_holders(&at(0)), 2);
        assert_eq!(index.hold_holders(&at(1)), 1);
        assert_eq!(index.hold_holders(&at(2)), 2);
        // Extension renews only; it adds no holders.
        index.extend_hold(&version, until).unwrap();
        assert_eq!(index.hold_holders(&at(0)), 2);
        let other = descriptor("other", 17).version;
        assert_eq!(index.reserve_hold(&other, 2, until), Err(Error::Overloaded));
        // The second reader's release leaves the shared pages held by the first.
        index.release_hold_pages(&version, &page_set(&[0, 2]), epoch);
        assert!(index.is_held(&at(0)));
        assert!(index.is_held(&at(2)));
        assert_eq!(index.hold_holders(&at(0)), 1);
        assert!(!index.can_evict(SegmentId(0)));
        // Releasing the last holder makes the published page evictable and
        // returns its capacity; unknown pages are ignored.
        index.release_hold_pages(&version, &page_set(&[0, 1, 9]), epoch);
        assert!(!index.is_held(&at(0)));
        assert!(!index.is_held(&at(1)));
        assert_eq!(index.held_page_expiry(&at(0)), None);
        assert_eq!(index.hold_holders(&at(0)), 0);
        assert!(index.can_evict(SegmentId(0)));
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
        let clock = crate::rt::environment::SimulationClock::new(29);
        let sim_env = clock.environment(0);
        let _environment = sim_env.enter();
        let sim_ctx = sim_env.worker_ctx();
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
            .reserve_hold_pages(&version, &page_set(&[0]), sim_ctx.clock.wall_now() + ttl)
            .unwrap();
        clock.advance(ttl + std::time::Duration::from_secs(1));
        assert_eq!(index.hold_holders(&page), 0);
        // Another reader re-reserves the page; the expired entry is
        // pruned and the replacement is a new epoch.
        let until = sim_ctx.clock.wall_now() + ttl;
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
        assert!(!index.can_evict(SegmentId(0)));
        // The replacement's own release still works.
        index.release_hold_pages(&version, &page_set(&[0]), replacement);
        assert!(!index.is_held(&page));
        assert_eq!(index.hold_expiry(&version), None);
        assert!(index.can_evict(SegmentId(0)));
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
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
            .reserve_hold_pages(&version, &page_set(&[1]), until)
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        let epoch = index
            .reserve_hold_pages(&page.version, &page_set(&[0, 1]), until)
            .unwrap();
        index
            .reserve_hold_pages(&page.version, &page_set(&[0, 1]), until)
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
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
        assert!(index.can_evict(SegmentId(0)));
        // Capacity 2 holds the cached page plus the reserved member slot.
        assert!(matches!(
            index.reserve_page_for(&probe()),
            Err(Error::Overloaded)
        ));
        let (_, mut entry) = indexed(descriptor("primary", 2 * crate::model::PAGE_BYTES), 1);
        drop(segments.append(1024).unwrap());
        entry.location.extent = Extent::new(1024, 512).unwrap();
        let ticket = index.reserve_page_for(&primary).unwrap();
        ticket.publish(primary.clone(), entry).unwrap();
        assert_eq!(index.held_page_expiry(&primary), Some(until));
        assert_eq!(index.held_page_expiry(&cached), None);
        assert_eq!(index.hold_unused(), 0);
        assert_eq!(index.held_snapshot().pages, 1);
        index.release_hold(&cached.version).unwrap();
        assert_eq!(index.held_page_expiry(&primary), Some(until));
        assert!(!index.is_held(&cached));
    }

    /// Releasing a page while its held write is in flight returns only that
    /// page's capacity: the write keeps the entry alive, but no longer offsets
    /// the unused reservation of the pages still held.
    #[test]
    fn released_page_with_held_write_in_flight_keeps_other_members_reserved() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        index.set_page_capacity(4).unwrap();
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let version = descriptor("inflight", 2 * crate::model::PAGE_BYTES).version;
        let at = |number| PageId {
            version: version.clone(),
            number: crate::model::PageNumber(number),
        };
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        let epoch = index
            .reserve_hold_pages(&version, &page_set(&[0, 1]), until)
            .unwrap();
        assert_eq!(index.hold_unused(), 2);
        let mut writing = index.reserve_page_for(&at(0)).unwrap();
        assert_eq!(
            index.hold_unused(),
            2,
            "a ticket charges the hold until it appends"
        );
        let _lease = writing.append(&at(0), &segments, 512).unwrap();
        assert_eq!(index.hold_unused(), 1);
        index.release_hold_pages(&version, &page_set(&[0]), epoch);
        assert!(!index.is_held(&at(0)));
        assert_eq!(index.hold_unused(), 1, "page 1 keeps its reservation");
        assert_eq!(index.hold_epoch(&version), Some(epoch));
        // Ordinary writes cannot take page 1's slot.
        let ordinary = [
            index.reserve_page_for(&probe()).unwrap(),
            index.reserve_page_for(&probe()).unwrap(),
        ];
        assert!(matches!(
            index.reserve_page_for(&probe()),
            Err(Error::Overloaded)
        ));
        let mut held = index.reserve_page_for(&at(1)).unwrap();
        let _held_lease = held.append(&at(1), &segments, 512).unwrap();
        assert_eq!(index.hold_unused(), 0);
        drop((held, ordinary));
        assert_eq!(index.hold_unused(), 1);
        // Releasing the last member ends the hold; page 0's write still owns
        // its entry until it completes.
        index.release_hold_pages(&version, &page_set(&[1]), epoch);
        assert_eq!(index.hold_epoch(&version), None);
        assert_eq!(index.hold_unused(), 0);
        drop(writing);
        index.release_hold(&version).unwrap();
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_hold_pages(&cached.version, &page_set(&[0, 2]), until)
            .unwrap();
        assert_eq!(index.held_page_expiry(&cached), Some(until));
        assert_eq!(
            index.hold_unused(),
            1,
            "the cached member covers its own charge"
        );
        let later = until + std::time::Duration::from_secs(30);
        index
            .reserve_hold_pages(&cached.version, &page_set(&[1]), later)
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_hold_pages(&first.version, &page_set(&held), until)
            .unwrap();
        let mut snapshots = Vec::new();
        let mut remaining = usize::MAX;
        index
            .snapshot_holds(&mut snapshots, &mut remaining)
            .unwrap();
        index.clear_holds();
        index.restore_holds(&snapshots).unwrap();
        let holds = index.holds.borrow();
        let hold = &holds.entries[&first.version];
        assert_eq!(hold.pages, page_set(&held));
        let restored: Vec<u64> = hold.pages.iter().map(|p| p.0).collect();
        let mut expected = held.clone();
        expected.sort_unstable();
        assert_eq!(restored, expected);
        assert_eq!(hold.holders.len(), held.len());
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        index
            .reserve_hold_pages(&first.version, &page_set(&[1]), until)
            .unwrap();
        assert_eq!(index.held_snapshot().pages, 1);
        let mut snapshots = Vec::new();
        let mut remaining = usize::MAX;
        index
            .snapshot_holds(&mut snapshots, &mut remaining)
            .unwrap();
        let image = index.snapshot().unwrap();
        index.clear_holds();
        // Three cached pages exceed the limit of two; one held page does not.
        index
            .validate_restored_holds(&image, &snapshots, segments.count())
            .unwrap();
        index.restore_holds(&snapshots).unwrap();
        assert!(!index.is_held(&pages[0]), "nonmember stays evictable");
        // Only the remaining duration is restored; the wall deadline is
        // recomputed from it and so cannot be later than the original.
        let restored = index.held_page_expiry(&pages[1]).unwrap();
        assert!(restored <= until + std::time::Duration::from_secs(1));
        assert!(restored + std::time::Duration::from_secs(5) > until);
        assert!(!index.is_held(&pages[2]), "nonmember stays evictable");
        assert_eq!(index.held_snapshot().pages, 1);
    }

    #[test]
    fn held_snapshot_counts_only_live_publications_and_distinct_segments() {
        let segments = Rc::new(segments(1024, 8));
        let index = Rc::new(index(8));
        let _clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        let (first, entry) = indexed(descriptor("first", 17), 0);
        index.reserve_hold(&first.version, 2, until).unwrap();
        let pending = index.reserve_page_for(&first).unwrap();
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
        let mut snapshots = Vec::new();
        let mut remaining = usize::MAX;
        index
            .snapshot_holds(&mut snapshots, &mut remaining)
            .unwrap();
        index.clear_holds();
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
            assert!(
                hold.live(&index.clock),
                "monotonic deadline still protects pages"
            );
        }
        assert_eq!(index.held_snapshot().pages, 1);
        index
            .holds
            .borrow_mut()
            .entries
            .get_mut(&first.version)
            .unwrap()
            .monotonic = racer_io::Clock::host().now() - std::time::Duration::from_secs(1);
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
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
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
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
        let now = racer_io::Clock::host().wall_now();
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
        let a = index.reserve_page_for(&probe()).unwrap();
        let b = index.reserve_page_for(&probe()).unwrap();
        assert!(matches!(
            index.reserve_page_for(&probe()),
            Err(Error::Overloaded)
        ));
        let page = PageId {
            version: version.clone(),
            number: crate::model::PageNumber(0),
        };
        let held = index.reserve_page_for(&page).unwrap();
        drop(held);
        drop((a, b));
        index.release_hold(&version).unwrap();
        assert_eq!(index.hold_unused(), 0);
        index
            .reserve_hold(&version, 1, now + std::time::Duration::from_secs(60))
            .unwrap();
        let old_ticket = index.reserve_page_for(&page).unwrap();
        index.remove_cache(&version.object.cache);
        index
            .reserve_hold(&version, 1, now + std::time::Duration::from_secs(60))
            .unwrap();
        drop(old_ticket);
        index.release_hold(&version).unwrap();
        assert_eq!(index.hold_expiry(&version), None);
    }

    #[test]
    fn held_page_segment_is_never_reclaimed() {
        let segments = Rc::new(segments(1024, 4));
        let index = Rc::new(index(8));
        let _bind = SegmentClock::new(index.clone(), segments.clone(), 1);
        let (held, entry) = indexed(descriptor("held", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(held.clone(), entry).unwrap();
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        index.reserve_hold(&held.version, 1, until).unwrap();
        let mut others = Vec::new();
        for segment in 1..4 {
            let (other, entry) = indexed(descriptor(&format!("other{segment}"), 17), segment);
            drop(segments.append(1024).unwrap());
            index.publish(other.clone(), entry).unwrap();
            others.push(other);
        }
        // The reserve asks for every segment; only the unheld ones may go.
        let clock = SegmentClock::new(index.clone(), segments.clone(), 4);
        for _ in 0..4 {
            assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
        }
        assert!(index.lookup(&held).unwrap().is_some());
        for other in &others {
            assert!(index.lookup(other).unwrap().is_none());
        }
        assert_eq!(
            segments.state(SegmentId(0)).unwrap(),
            page_alloc::SegmentState::Sealed
        );
        assert_eq!(segments.free_count(), 3);
    }

    #[test]
    fn hard_hold_expiry_follows_monotonic_time_only() {
        let segments = Rc::new(segments(1024, 4));
        let virtual_clock = racer_io::ctx::VirtualClock::new();
        let clock = racer_io::Clock::from(virtual_clock.clone());
        let index = Rc::new(Index::new(
            WorkerId(0),
            8,
            crate::test_support::availability(),
            clock.clone(),
        ));
        let _clock = SegmentClock::new(index.clone(), segments, 1);
        let version = descriptor("ttl", 17).version;
        index
            .reserve_hold(
                &version,
                1,
                clock.wall_now() + std::time::Duration::from_secs(60),
            )
            .unwrap();
        // A wall step past the requested deadline does not end the hold.
        virtual_clock
            .set_wall_time(clock.wall_now() + std::time::Duration::from_secs(3600))
            .unwrap();
        assert!(index.hold_expiry(&version).is_some());
        assert_eq!(index.hold_unused(), 1);
        virtual_clock
            .advance(std::time::Duration::from_secs(60))
            .unwrap();
        assert_eq!(index.hold_expiry(&version), None);
        assert_eq!(index.hold_unused(), 0);
        assert!(index.holds.borrow().entries.is_empty());
    }
    #[test]
    fn disk_observability_index_lifecycle_and_victims() {
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
        // A full index never evicts a single page; segment reclaim frees space.
        assert!(matches!(
            index.reserve_page_for(&other),
            Err(Error::Overloaded)
        ));
        index.remove_if_matches(&page, &entry.location);
        assert_eq!(retention.snapshot().indexed_payload_bytes, 0);
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
            index.touch(&hot);
        }
        assert_eq!(index.segment_score(SegmentId(0)), 100);
        assert_eq!(index.segment_score(SegmentId(1)), 300);
        assert_eq!(index.segment_score(SegmentId(2)), 10);
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
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
    fn second_sight_sealed_unpublished_write_is_not_a_victim() {
        let segments = Rc::new(segments(1024, 3));
        let index = Rc::new(index(4));
        let (published, entry) = indexed(descriptor("published", 17), 0);
        drop(segments.append(1024).unwrap());
        index.publish(published.clone(), entry).unwrap();
        index.touch(&published);
        let (page, entry) = indexed(descriptor("completed", 17), 1);
        let (other, _) = indexed(descriptor("other", 17), 2);
        let mut ticket = index.reserve_page_for(&page).unwrap();
        let (write, location) = ticket.append(&page, &segments, 1024).unwrap();
        let mut other_ticket = index.reserve_page_for(&other).unwrap();
        let (other_write, _) = other_ticket.append(&other, &segments, 1024).unwrap();
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        clock.reclaim_now().unwrap();
        assert!(index.lookup(&published).unwrap().is_none());
        assert_eq!(
            segments.state(write.id()),
            Ok(page_alloc::SegmentState::Sealed)
        );
        segments
            .validate(write.id(), write.generation(), &location.extent)
            .unwrap();
        ticket
            .publish(page.clone(), IndexedPage { location, ..entry })
            .unwrap();
        drop(write);
        assert!(index.lookup(&page).unwrap().is_some());
        drop(other_ticket);
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

#[cfg(test)]
mod priority_tests {
    use super::*;
    use crate::model::{CacheKey, StrongEtag};
    use racer_wire::control::{CacheDefinition, CacheId};

    const PAGE: u64 = crate::model::PAGE_BYTES;

    /// Cache ids are UUIDs; derive a stable one from a short test name.
    fn cache(name: &str) -> CacheId {
        let digit = (b'a' + name.as_bytes()[0] % 6) as char;
        let d = digit.to_string().repeat(12);
        CacheId(format!(
            "{}-{}-4{}-8{}-{d}",
            &d[..8],
            &d[..4],
            &d[..3],
            &d[..3]
        ))
    }

    fn definition(name: &str, priority: u16) -> CacheDefinition {
        CacheDefinition {
            id: cache(name),
            name: name.into(),
            client_socket: "/client".into(),
            origin_socket: "/origin".into(),
            priority,
            mode: racer_wire::control::CacheMode::Cached,
            origin: None,
        }
    }

    fn index(pages: usize, weights: &[(&str, u16)]) -> Rc<Index> {
        let index = Rc::new(Index::new(
            WorkerId(0),
            64,
            crate::test_support::availability_for(
                weights.iter().map(|(name, _)| cache(name)).collect(),
            ),
            racer_io::Clock::host(),
        ));
        index.set_page_capacity(pages).unwrap();
        let table = crate::priority::Weights::default();
        table.update(
            &weights
                .iter()
                .map(|(name, priority)| definition(name, *priority))
                .collect::<Vec<_>>(),
        );
        index.retention().set_weights(table);
        index
    }

    /// One full-size page of `cache` recorded in `segment`.
    fn page(cache_name: &str, etag: &str, segment: u64) -> (PageId, IndexedPage) {
        let metadata = VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache(cache_name),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length: PAGE,
        };
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
                    extent: Extent::new(segment * 4 * PAGE, 512).unwrap(),
                },
            },
        )
    }

    fn publish(index: &Index, cache_name: &str, etag: &str, segment: u64) -> PageId {
        let (page, entry) = page(cache_name, etag, segment);
        index.publish(page.clone(), entry).unwrap();
        page
    }

    /// Per-cache counters must equal a recomputation from live mappings.
    fn assert_accounting(index: &Index) {
        let mut expected = crate::runtime::HashMap::<CacheId, u64>::default();
        index.state.borrow().pages.check();
        for view in index.state.borrow().pages.iter() {
            *expected
                .entry(view.page.version.object.cache.clone())
                .or_default() += PAGE;
        }
        let retention = index.retention();
        for (cache, bytes) in &expected {
            assert_eq!(retention.cache_payload_bytes(cache), *bytes, "{cache:?}");
        }
        let charged: u64 = retention
            .cache_disk(u64::MAX)
            .iter()
            .map(|c| c.resident_bytes)
            .sum();
        assert_eq!(charged, expected.values().sum::<u64>());
        assert_eq!(retention.snapshot().indexed_payload_bytes, charged);
    }

    fn segments(count: usize) -> Rc<Segments> {
        let s = Segments::new(4 * PAGE);
        s.configure(
            4 * PAGE * count as u64,
            count,
            page_alloc::Alignment::new(512, 512, 512).unwrap(),
        )
        .unwrap();
        Rc::new(s)
    }

    #[test]
    fn accounting_tracks_publish_replace_evict_remove_and_restore() {
        let index = index(4, &[("a", 500), ("b", 500)]);
        let a0 = publish(&index, "a", "0", 0);
        publish(&index, "b", "0", 0);
        publish(&index, "b", "1", 0);
        assert_accounting(&index);
        assert_eq!(index.retention().cache_payload_bytes(&cache("a")), PAGE);
        assert_eq!(index.retention().cache_payload_bytes(&cache("b")), 2 * PAGE);
        // Replacement charges once.
        let (_, entry) = page("a", "0", 1);
        index.publish(a0, entry).unwrap();
        assert_accounting(&index);
        assert_eq!(index.retention().cache_payload_bytes(&cache("a")), PAGE);
        publish(&index, "a", "1", 0);
        assert_accounting(&index);
        assert_eq!(index.state.borrow().pages.len(), 4);
        // Segment removal releases charges too.
        index.remove_bounded(SegmentId(1), 8);
        assert_accounting(&index);
        let snapshot = index.snapshot().unwrap();
        let before = index.retention().cache_disk(u64::MAX);
        // Restore rebuilds accounting from recovered mappings.
        index.restore(snapshot).unwrap();
        assert_accounting(&index);
        assert_eq!(index.retention().cache_disk(u64::MAX), before);
        index.remove_cache(&cache("b"));
        assert_accounting(&index);
        assert_eq!(index.retention().cache_payload_bytes(&cache("b")), 0);
    }

    #[test]
    fn remove_caches_removes_only_listed_caches_in_one_call() {
        let index = index(8, &[("a", 1), ("b", 1), ("c", 1)]);
        publish(&index, "a", "0", 0);
        publish(&index, "b", "0", 0);
        publish(&index, "c", "0", 0);
        index.remove_caches(&std::collections::HashSet::new());
        assert_eq!(index.state.borrow().pages.len(), 3);
        index.remove_caches(&std::collections::HashSet::from([cache("a"), cache("c")]));
        assert_accounting(&index);
        assert_eq!(index.retention().cache_payload_bytes(&cache("a")), 0);
        assert_eq!(index.retention().cache_payload_bytes(&cache("c")), 0);
        assert_eq!(index.retention().cache_payload_bytes(&cache("b")), PAGE);
        let s = index.state.borrow();
        assert_eq!(s.pages.len(), 1);
        assert!(
            s.pages
                .iter()
                .all(|view| view.page.version.object.cache == cache("b"))
        );
        assert!(s.metadata.keys().all(|v| v.object.cache == cache("b")));
    }

    #[test]
    fn segment_reclaim_prefers_segment_dominated_by_over_share_pages() {
        let segments = segments(2);
        let index = index(8, &[("a", 900), ("b", 100)]);
        let clock = SegmentClock::new(index.clone(), segments.clone(), 1);
        drop(segments.append(4 * PAGE as usize).unwrap());
        drop(segments.append(4 * PAGE as usize).unwrap());
        let a: Vec<_> = (0..2)
            .map(|n| publish(&index, "a", &n.to_string(), 0))
            .collect();
        let b: Vec<_> = (0..3)
            .map(|n| publish(&index, "b", &n.to_string(), 1))
            .collect();
        for page in &b {
            for _ in 0..8 {
                index.touch(page);
            }
        }
        let retention = index.retention();
        let scoring = retention.scoring_at(racer_io::Clock::host().now(), index.disk_capacity());
        assert!(scoring.over_share(&cache("b")));
        assert!(!scoring.over_share(&cache("a")));
        assert!(
            index.segment_score_with(SegmentId(1), &scoring)
                < index.segment_score_with(SegmentId(0), &scoring)
        );
        drop(scoring);
        clock.reclaim_now().unwrap();
        for page in &a {
            assert!(index.lookup(page).unwrap().is_some());
        }
        for page in &b {
            assert!(index.lookup(page).unwrap().is_none());
        }
        assert_accounting(&index);
    }

    #[test]
    fn live_priority_change_flips_over_share() {
        let index = index(4, &[("a", 900), ("b", 100)]);
        publish(&index, "a", "0", 0);
        publish(&index, "b", "0", 0);
        let retention = index.retention();
        let capacity = index.disk_capacity();
        assert!(retention.over_share(&cache("b"), capacity));
        assert!(!retention.over_share(&cache("a"), capacity));
        assert!(
            retention
                .weights()
                .update(&[definition("a", 100), definition("b", 900)])
        );
        assert!(retention.over_share(&cache("a"), capacity));
        assert!(!retention.over_share(&cache("b"), capacity));
    }

    /// Publish four `name` pages into one fresh full segment, freeing index
    /// entries first by segment reclaim, as the store does when the index is
    /// full. Returns the pages.
    fn fill_segment(
        index: &Rc<Index>,
        clock: &SegmentClock,
        segments: &Segments,
        name: &str,
        round: usize,
    ) -> Vec<PageId> {
        for _ in 0..8 {
            if index.page_capacity() - index.state.borrow().pages.len() >= 4 {
                break;
            }
            let _ = clock.reclaim_entries();
        }
        let (id, generation) = (0..8)
            .find_map(|_| match segments.append(4 * PAGE as usize) {
                Ok((lease, _)) => Some((lease.id(), lease.generation())),
                Err(_) => {
                    let _ = clock.reclaim_now();
                    None
                }
            })
            .expect("segment space");
        (0..4)
            .map(|n| {
                let (page, mut entry) = page(name, &format!("{round}-{n}"), id.0);
                entry.location.generation = generation;
                index
                    .reserve_page_for(&page)
                    .unwrap()
                    .publish(page.clone(), entry)
                    .unwrap();
                page
            })
            .collect()
    }

    /// Deterministic churn: both caches insert one full segment per round
    /// into a full index. Returns resident pages of ("a", "b") after `rounds`.
    fn churn(index: &Rc<Index>, clock: &SegmentClock, rounds: usize, start: usize) -> (u64, u64) {
        let segments = clock.segments.clone();
        for round in start..start + rounds {
            for name in ["a", "b"] {
                fill_segment(index, clock, &segments, name, round);
            }
            assert_accounting(index);
        }
        let retention = index.retention();
        (
            retention.cache_payload_bytes(&cache("a")) / PAGE,
            retention.cache_payload_bytes(&cache("b")) / PAGE,
        )
    }

    #[test]
    fn index_full_reclaims_over_share_segment_first_and_keeps_holds() {
        // 8 index pages, 4 per segment: "a" (900) is under share, "b" over.
        let index = index(8, &[("a", 900), ("b", 100)]);
        let clock = SegmentClock::new(index.clone(), segments(8), 1);
        let segments = clock.segments.clone();
        let b = fill_segment(&index, &clock, &segments, "b", 0);
        let a = fill_segment(&index, &clock, &segments, "a", 0);
        // Make the over-share cache hotter: heat alone would evict "a".
        for page in &b {
            for _ in 0..8 {
                index.touch(page);
            }
        }
        assert!(matches!(
            index.reserve_page_for(&page("a", "probe", 0).0),
            Err(Error::Overloaded)
        ));
        clock.reclaim_entries().unwrap();
        assert!(b.iter().all(|page| index.lookup(page).unwrap().is_none()));
        assert!(a.iter().all(|page| index.lookup(page).unwrap().is_some()));
        assert_accounting(&index);
        // A held over-share segment is kept; the under-share one goes instead.
        let b = fill_segment(&index, &clock, &segments, "b", 1);
        let until = racer_io::Clock::host().wall_now() + std::time::Duration::from_secs(60);
        index.reserve_hold(&b[0].version, 1, until).unwrap();
        clock.reclaim_entries().unwrap();
        assert!(index.lookup(&b[0]).unwrap().is_some());
        assert!(a.iter().all(|page| index.lookup(page).unwrap().is_none()));
        assert_accounting(&index);
    }

    #[test]
    fn index_full_reclaim_work_is_bounded_with_many_resident_caches() {
        const PHANTOMS: usize = 3000;
        let index = index(32, &[("a", 1), ("z", 1)]);
        let clock = SegmentClock::new(index.clone(), segments(16), 1);
        let segments = clock.segments.clone();
        // Many resident caches without index pages, each with weight 1, so
        // "a" stays under share. Every tenth one is over share.
        let mut definitions = vec![definition("a", 60000), definition("z", 0)];
        let phantom = |n: usize| CacheId(format!("phantom-{n}"));
        definitions.extend((0..PHANTOMS).map(|n| CacheDefinition {
            id: phantom(n),
            ..definition("a", 1)
        }));
        let table = crate::priority::Weights::default();
        table.update(&definitions);
        let retention = index.retention();
        retention.set_weights(table);
        let _phantoms: Vec<_> = (0..PHANTOMS)
            .map(|n| retention.indexed_payload(&phantom(n), if n % 10 == 0 { PAGE } else { 1 }))
            .collect();
        let z = fill_segment(&index, &clock, &segments, "z", 0);
        let a: Vec<_> = (0..7)
            .flat_map(|round| fill_segment(&index, &clock, &segments, "a", round))
            .collect();
        let (rebuilds, visits) = retention.pressure_counters();
        clock.reclaim_entries().unwrap();
        let (after_rebuilds, after_visits) = retention.pressure_counters();
        // One scoring pass: at most one pressure-order rebuild and a constant
        // number of candidates, independent of the number of caches.
        assert!(
            after_rebuilds - rebuilds <= 1,
            "{rebuilds} -> {after_rebuilds}"
        );
        assert!(after_visits - visits <= 2, "{visits} -> {after_visits}");
        assert!(z.iter().all(|page| index.lookup(page).unwrap().is_none()));
        assert!(a.iter().all(|page| index.lookup(page).unwrap().is_some()));
    }

    #[test]
    fn index_full_reclaim_follows_live_reweight() {
        let index = index(8, &[("a", 900), ("b", 100)]);
        let clock = SegmentClock::new(index.clone(), segments(4), 1);
        let segments = clock.segments.clone();
        let a = fill_segment(&index, &clock, &segments, "a", 0);
        let b = fill_segment(&index, &clock, &segments, "b", 0);
        index
            .retention()
            .weights()
            .update(&[definition("a", 100), definition("b", 900)]);
        clock.reclaim_entries().unwrap();
        assert!(a.iter().all(|page| index.lookup(page).unwrap().is_none()));
        assert!(b.iter().all(|page| index.lookup(page).unwrap().is_some()));
    }

    #[test]
    fn priority_zero_keeps_idle_space_but_goes_first_under_pressure() {
        let index = index(16, &[("a", 1000), ("z", 0)]);
        let clock = SegmentClock::new(index.clone(), segments(8), 1);
        let segments = clock.segments.clone();
        for round in 0..3 {
            fill_segment(&index, &clock, &segments, "z", round);
        }
        // Idle capacity: priority 0 data is still cached.
        assert_eq!(
            index.retention().cache_payload_bytes(&cache("z")),
            12 * PAGE
        );
        for page in fill_segment(&index, &clock, &segments, "a", 0) {
            for _ in 0..8 {
                index.touch(&page);
            }
        }
        for round in 1..4 {
            fill_segment(&index, &clock, &segments, "a", round);
            assert_accounting(&index);
        }
        assert_eq!(index.retention().cache_payload_bytes(&cache("z")), 0);
        assert_eq!(
            index.retention().cache_payload_bytes(&cache("a")),
            16 * PAGE
        );
    }

    #[test]
    fn simulated_churn_converges_to_priority_ratio_and_follows_live_change() {
        let clock = |index: &Rc<Index>| SegmentClock::new(index.clone(), segments(32), 1);
        let index = index(100, &[("a", 900), ("b", 100)]);
        let eviction = clock(&index);
        let (a, b) = churn(&index, &eviction, 100, 0);
        assert_eq!(a + b, 100);
        assert!((6 * b..=14 * b).contains(&a), "{a}:{b}");
        // Live change: the ratio moves toward the new weights without restart.
        index
            .retention()
            .weights()
            .update(&[definition("a", 100), definition("b", 900)]);
        let (a, b) = churn(&index, &eviction, 100, 100);
        assert!((6 * a..=14 * a).contains(&b), "{a}:{b}");
        let even = self::index(100, &[("a", 500), ("b", 500)]);
        let eviction = clock(&even);
        let (a, b) = churn(&even, &eviction, 100, 0);
        assert!(a.abs_diff(b) <= 8, "{a}:{b}");
    }
}
