//! Worker-local allocation authority, recovery images, and bounded reclamation.
//!
//! Slot reuse is metadata-only: it never erases, truncates, or compacts disk bytes.
//! Existing leases protect their captured used prefix even after eviction starts.
//! Freeze protects table mutation, not caller indexes or data-I/O completion.
use crate::{Alignment, Error, Extent, MAX_SEGMENTS, Result, SegmentGeometry};
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeSet, HashSet},
    rc::Rc,
};

/// Stable slot number within one allocation table, not authority to access it.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct SegmentId(pub u64);

/// Monotonic reuse counter; zero is invalid in a restored image.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct Generation(pub u64);

/// Persisted segment lifecycle, separate from live lease and freeze ownership.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SegmentState {
    /// Available for a new append.
    Free,

    /// The sole appendable tail.
    Open,

    /// Full or rotated away from; eligible for eviction.
    Sealed,

    /// Rejects new leases while existing completion owners drain.
    Evicting,
}

/// Caller-owned recovery image; restore validates it before publishing any slot.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SegmentSnapshot {
    /// Ordered slot identity.
    pub id: SegmentId,

    /// Current nonzero reuse generation.
    pub generation: Generation,

    /// Persisted lifecycle state.
    pub state: SegmentState,

    /// Aligned occupied prefix, including record padding.
    pub used_bytes: u64,
}

/// Unique worker-local lease that prevents reuse until its completion owner drops.
/// It authorizes the used prefix at acquisition, not just the latest append.
///
/// Lease counts cannot be duplicated by cloning a completion capability:
///
/// ```compile_fail
/// fn duplicate(lease: page_alloc::SegmentLease) { let _ = lease.clone(); }
/// ```
#[must_use = "keep the lease alive until the operation completes"]
pub struct SegmentLease {
    id: SegmentId,

    generation: Generation,

    count: Rc<Cell<usize>>,

    table: TableIdentity,

    geometry: SegmentGeometry,

    start: u64,

    end: u64,
}

impl SegmentLease {
    /// Slot protected against recycling by this lease.
    pub fn id(&self) -> SegmentId {
        self.id
    }

    /// Reuse generation captured when the lease was acquired.
    pub fn generation(&self) -> Generation {
        self.generation
    }

    /// Borrow the nominal identity without granting table mutation authority.
    pub(crate) fn table_identity(&self) -> TableIdentity {
        self.table.clone()
    }

    /// Validated dimensions captured independently of the table's lifetime.
    pub(crate) fn geometry(&self) -> SegmentGeometry {
        self.geometry
    }

    /// An existing lease remains valid during eviction, but cannot authorize
    /// bytes appended after it was acquired.
    pub(crate) fn validate_extent(&self, extent: &Extent) -> Result<()> {
        let end = extent
            .offset()
            .checked_add(extent.length() as u64)
            .ok_or(Error::Corrupt)?;
        if extent.offset() < self.start
            || end > self.end
            || self
                .geometry()
                .alignment()
                .extent(extent.offset(), extent.length())
                .map_err(|_| Error::Corrupt)?
                != *extent
        {
            return Err(Error::Corrupt);
        }
        Ok(())
    }
}

impl Drop for SegmentLease {
    /// Release exactly the one lease count acquired during construction.
    fn drop(&mut self) {
        self.count.set(self.count.get() - 1);
    }
}

/// Mutable recovery image paired with its independently retained lease counter.
struct Slot {
    image: SegmentSnapshot,

    leases: Rc<Cell<usize>>,
}

/// Holds a freeze independently of the table's lifetime. Dropping it thaws the table.
///
/// A second guard cannot be created by cloning the first:
///
/// ```compile_fail
/// fn duplicate(guard: page_alloc::FreezeGuard) { let _ = guard.clone(); }
/// ```
#[must_use = "keep the guard alive while the table must remain frozen"]
#[derive(Debug)]
pub struct FreezeGuard(Rc<Cell<bool>>);

impl Drop for FreezeGuard {
    /// Release mutation exclusion even if the table has already been dropped.
    fn drop(&mut self) {
        self.0.set(false);
    }
}

/// Non-cloneable worker-local table whose leases and guards can outlive it.
/// Rc sharing is intentional within a worker; authority cannot cross threads.
///
/// ```compile_fail
/// fn require_send<T: Send>() {}
/// require_send::<page_alloc::Segments>();
/// ```
#[repr(align(64))]
pub struct Segments {
    segment_bytes: u64,

    geometry: Cell<Option<SegmentGeometry>>,

    slots: RefCell<Vec<Slot>>,

    frozen: Rc<Cell<bool>>,

    table: TableIdentity,

    restore_epoch: Cell<u64>,

    open: Cell<Option<usize>>,

    free: RefCell<BTreeSet<usize>>,

    evicting: Cell<usize>,
}

impl Segments {
    /// Create an unconfigured table for Slab::open_configured to bind at startup.
    pub fn new(segment_bytes: u64) -> Self {
        Self {
            segment_bytes,
            geometry: Cell::new(None),
            slots: RefCell::new(Vec::new()),
            frozen: Rc::new(Cell::new(false)),
            table: TableIdentity::new(),
            restore_epoch: Cell::new(0),
            open: Cell::new(None),
            free: RefCell::new(BTreeSet::new()),
            evicting: Cell::new(0),
        }
    }

    /// Fixed size of a physical segment.
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }

    /// Configured physical capacity, or zero before configuration.
    pub fn capacity_bytes(&self) -> u64 {
        self.geometry.get().map_or(0, SegmentGeometry::slab_bytes)
    }

    /// Build an entirely free table, rejecting geometry above the retained slot limit.
    pub fn from_geometry(geometry: SegmentGeometry) -> Result<Self> {
        let segments = Self::new(geometry.segment_bytes());
        segments.configure_table(
            geometry.slab_bytes(),
            usize::try_from(geometry.segment_count()).map_err(|_| Error::InvalidConfiguration)?,
            geometry.alignment(),
        )?;
        Ok(segments)
    }

    /// Whether validated geometry and the slot table have been installed.
    pub fn is_configured(&self) -> bool {
        self.geometry.get().is_some()
    }

    /// Configured geometry, which may expose fewer slots than physical capacity.
    pub fn geometry(&self) -> Option<SegmentGeometry> {
        self.geometry.get()
    }

    /// Clone identity only, without granting allocation authority.
    pub(crate) fn table_identity(&self) -> TableIdentity {
        self.table.clone()
    }

    /// Recovery revision used to invalidate eviction cursor and recent-read state.
    pub(crate) fn restore_epoch(&self) -> u64 {
        self.restore_epoch.get()
    }

    /// Install an entirely free bounded table exactly once, unless frozen.
    #[cfg(any(test, feature = "simulation"))]
    pub fn configure(&self, capacity: u64, count: usize, alignment: Alignment) -> Result<()> {
        self.configure_table(capacity, count, alignment)
    }

    /// Install validated geometry for startup or an explicitly constructed table.
    pub(crate) fn configure_table(
        &self,
        capacity: u64,
        count: usize,
        alignment: Alignment,
    ) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        if self.is_configured() || count as u64 > MAX_SEGMENTS {
            return Err(Error::InvalidConfiguration);
        }
        let geometry = SegmentGeometry::new(capacity, self.segment_bytes, count as u64, alignment)
            .map_err(|_| Error::InvalidConfiguration)?;
        self.geometry.set(Some(geometry));
        *self.slots.borrow_mut() = (0..count)
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
        *self.free.borrow_mut() = (0..count).collect();
        Ok(())
    }

    /// Reserve an aligned used range. Malformed requests and lease overflow leave
    /// state unchanged. A valid rollover without a free slot seals the open tail
    /// before returning Busy, allowing reclamation to make a retry possible.
    pub fn append(&self, length: usize) -> Result<(SegmentLease, Extent)> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let alignment = self.geometry().ok_or(Error::Unavailable)?.alignment();
        if length == 0
            || length as u64 > self.segment_bytes
            || alignment.extent(0, length)?.length() != length
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut slots = self.slots.borrow_mut();
        let previous_open = self.open.get();
        let usable_open = previous_open.filter(|&position| {
            self.segment_bytes - slots[position].image.used_bytes >= length as u64
        });
        let position = match usable_open {
            Some(position) => position,
            None => match self.free.borrow().first() {
                Some(&position) => position,
                None => {
                    if let Some(previous) = previous_open {
                        slots[previous].image.state = SegmentState::Sealed;
                        self.open.set(None);
                    }
                    return Err(Error::Busy);
                }
            },
        };
        let slot = &slots[position];
        let offset = slot
            .image
            .id
            .0
            .checked_mul(self.segment_bytes)
            .and_then(|v| v.checked_add(slot.image.used_bytes))
            .ok_or(Error::InvalidConfiguration)?;
        let extent = alignment.extent(offset, length)?;
        let used_bytes = slot
            .image
            .used_bytes
            .checked_add(length as u64)
            .ok_or(Error::InvalidConfiguration)?;
        // All fallible checks, including lease-counter overflow, precede mutation.
        let lease = self.take_lease(slot, used_bytes)?;
        if usable_open.is_none() {
            if let Some(previous) = previous_open {
                slots[previous].image.state = SegmentState::Sealed;
            }
            self.free.borrow_mut().remove(&position);
        }
        self.open.set(Some(position));
        let slot = &mut slots[position];
        slot.image.state = SegmentState::Open;
        slot.image.used_bytes = used_bytes;
        if slot.image.used_bytes == self.segment_bytes {
            slot.image.state = SegmentState::Sealed;
            self.open.set(None);
        }
        Ok((lease, extent))
    }

    /// Capture a checked prefix and increment its counter before publishing a lease.
    fn take_lease(&self, slot: &Slot, used_bytes: u64) -> Result<SegmentLease> {
        let start = slot
            .image
            .id
            .0
            .checked_mul(self.segment_bytes)
            .ok_or(Error::Corrupt)?;
        let end = start.checked_add(used_bytes).ok_or(Error::Corrupt)?;
        let geometry = self.geometry().ok_or(Error::Unavailable)?;
        slot.leases
            .set(slot.leases.get().checked_add(1).ok_or(Error::Busy)?);
        Ok(SegmentLease {
            id: slot.image.id,
            generation: slot.image.generation,
            count: slot.leases.clone(),
            table: self.table.clone(),
            geometry,
            start,
            end,
        })
    }

    /// Convert an external slot number without truncation.
    fn position(id: SegmentId) -> Result<usize> {
        usize::try_from(id.0).map_err(|_| Error::Corrupt)
    }

    /// Acquire the current used prefix only while the generation is readable.
    pub fn lease(&self, id: SegmentId, generation: Generation) -> Result<SegmentLease> {
        let slots = self.slots.borrow();
        let slot = slots.get(Self::position(id)?).ok_or(Error::Corrupt)?;
        if slot.image.generation != generation
            || !matches!(slot.image.state, SegmentState::Open | SegmentState::Sealed)
        {
            return Err(Error::Stale);
        }
        self.take_lease(slot, slot.image.used_bytes)
    }

    /// Stop new leases for a sealed slot; repeating eviction is harmless.
    #[cfg(any(test, feature = "simulation"))]
    pub fn begin_evict(&self, id: SegmentId) -> Result<()> {
        self.begin_eviction(id)
    }

    /// Enter eviction before the clock invokes any caller index removal.
    fn begin_eviction(&self, id: SegmentId) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let mut slots = self.slots.borrow_mut();
        let slot = slots.get_mut(Self::position(id)?).ok_or(Error::Corrupt)?;
        if !matches!(
            slot.image.state,
            SegmentState::Sealed | SegmentState::Evicting
        ) {
            return Err(Error::Busy);
        }
        if slot.image.state != SegmentState::Evicting {
            self.evicting.set(self.evicting.get() + 1);
            slot.image.state = SegmentState::Evicting;
        }
        Ok(())
    }

    /// Reuse only an evicting, unleased slot, incrementing generation without wrap.
    #[cfg(any(test, feature = "simulation"))]
    pub fn recycle(&self, id: SegmentId) -> Result<()> {
        self.recycle_evicted(id)
    }

    /// Complete clock-authorized eviction only after outstanding leases drain.
    fn recycle_evicted(&self, id: SegmentId) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let mut slots = self.slots.borrow_mut();
        let slot = slots.get_mut(Self::position(id)?).ok_or(Error::Corrupt)?;
        if slot.image.state != SegmentState::Evicting || slot.leases.get() != 0 {
            return Err(Error::Busy);
        }
        slot.image.generation = Generation(
            slot.image
                .generation
                .0
                .checked_add(1)
                .ok_or(Error::Unavailable)?,
        );
        slot.image.state = SegmentState::Free;
        self.evicting.set(self.evicting.get() - 1);
        slot.image.used_bytes = 0;
        self.free.borrow_mut().insert(Self::position(id)?);
        Ok(())
    }

    /// Copy the complete ordered image, including while the table is frozen.
    pub fn snapshot(&self) -> Vec<SegmentSnapshot> {
        self.slots
            .borrow()
            .iter()
            .map(|s| s.image.clone())
            .collect()
    }

    /// Block allocation, eviction, recycling, and restore until the guard drops.
    /// Reads and snapshots remain available; only one guard may exist at a time.
    pub fn freeze(&self) -> Result<FreezeGuard> {
        if self.frozen.replace(true) {
            return Err(Error::Busy);
        }
        Ok(FreezeGuard(self.frozen.clone()))
    }

    /// Check the entire image and current restore eligibility without mutation.
    pub fn validate_restore(&self, images: &[SegmentSnapshot]) -> Result<()> {
        self.validate_restore_epoch(images).map(|_| ())
    }

    /// Validate recovery invariants and reserve the next nonwrapping epoch value.
    fn validate_restore_epoch(&self, images: &[SegmentSnapshot]) -> Result<u64> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let slots = self.slots.borrow();
        if slots.iter().any(|s| s.leases.get() != 0) {
            return Err(Error::Busy);
        }
        if images.len() != slots.len() {
            return Err(Error::Corrupt);
        }
        let alignment = self.geometry().ok_or(Error::Unavailable)?.alignment();
        let mut open = false;
        for (i, s) in images.iter().enumerate() {
            if s.id.0 != i as u64
                || s.generation.0 == 0
                || s.generation.0 < slots[i].image.generation.0
                || s.used_bytes > self.segment_bytes
                || !s.used_bytes.is_multiple_of(alignment.offset())
                || !s.used_bytes.is_multiple_of(alignment.length() as u64)
                || (s.state == SegmentState::Free && s.used_bytes != 0)
                || (s.state != SegmentState::Free && s.used_bytes == 0)
            {
                return Err(Error::Corrupt);
            }
            if s.state == SegmentState::Open {
                if open || s.used_bytes == self.segment_bytes {
                    return Err(Error::Corrupt);
                }
                open = true;
            }
        }
        self.restore_epoch
            .get()
            .checked_add(1)
            .ok_or(Error::Unavailable)
    }

    /// Validate the complete image before publishing it, sealing its open tail.
    /// Frozen tables and outstanding leases return Busy without changing state.
    /// A generation below the current slot generation returns Corrupt.
    pub fn restore(&self, images: Vec<SegmentSnapshot>) -> Result<()> {
        let epoch = self.validate_restore_epoch(&images)?;
        let mut slots = self.slots.borrow_mut();
        self.open.set(None);
        self.free.borrow_mut().clear();
        self.evicting.set(0);
        for (slot, mut image) in slots.iter_mut().zip(images) {
            if image.state == SegmentState::Open {
                image.state = SegmentState::Sealed;
            }
            if image.state == SegmentState::Free {
                self.free.borrow_mut().insert(image.id.0 as usize);
            }
            if image.state == SegmentState::Evicting {
                self.evicting.set(self.evicting.get() + 1);
            }
            slot.image = image;
        }
        self.restore_epoch.set(epoch);
        Ok(())
    }

    /// Validate a stored mapping against current readable state and used bytes.
    pub fn validate(&self, id: SegmentId, generation: Generation, extent: &Extent) -> Result<()> {
        let slots = self.slots.borrow();
        let slot = slots.get(Self::position(id)?).ok_or(Error::Corrupt)?;
        let start = id.0.checked_mul(self.segment_bytes).ok_or(Error::Corrupt)?;
        let end = extent
            .offset()
            .checked_add(extent.length() as u64)
            .ok_or(Error::Corrupt)?;
        if slot.image.generation != generation
            || !matches!(slot.image.state, SegmentState::Open | SegmentState::Sealed)
        {
            return Err(Error::Stale);
        }
        if extent.offset() < start
            || end
                > start
                    .checked_add(slot.image.used_bytes)
                    .ok_or(Error::Corrupt)?
        {
            return Err(Error::Corrupt);
        }
        let alignment = self.geometry().ok_or(Error::Unavailable)?.alignment();
        if alignment
            .extent(extent.offset(), extent.length())
            .map_err(|_| Error::Corrupt)?
            != *extent
        {
            return Err(Error::Corrupt);
        }
        Ok(())
    }

    /// Validate a live lease, including table identity and its captured used range.
    /// Existing leases remain usable while their segment is Evicting.
    pub fn validate_lease(&self, lease: &SegmentLease, extent: &Extent) -> Result<()> {
        if !self.table.matches(&lease.table) {
            return Err(Error::Stale);
        }
        lease.validate_extent(extent)
    }

    /// Number of immediately appendable slots, excluding pending eviction.
    pub fn free_count(&self) -> usize {
        self.free.borrow().len()
    }

    /// Number of retained slots, which may be less than physical capacity.
    pub fn count(&self) -> usize {
        self.slots.borrow().len()
    }

    /// Inspect a valid slot without granting mutation or lease authority.
    pub fn state(&self, id: SegmentId) -> Result<SegmentState> {
        self.slots
            .borrow()
            .get(Self::position(id)?)
            .map(|s| s.image.state)
            .ok_or(Error::Corrupt)
    }
}

/// Nominal, unforgeable identity shared by a table, bound file, and its leases.
/// Cloning identity does not clone allocation authority or keep the table alive.
#[derive(Clone)]
pub(crate) struct TableIdentity(Rc<()>);

impl TableIdentity {
    /// Mint one distinct identity for a newly constructed table.
    fn new() -> Self {
        Self(Rc::new(()))
    }

    /// Compare allocation identity, never persisted slot numbers or generations.
    pub(crate) fn matches(&self, other: &Self) -> bool {
        Rc::ptr_eq(&self.0, &other.0)
    }
}

/// Application-owned mappings; removal must compare current entries first.
/// Callbacks own metadata and version side effects and must never exceed budget.
pub trait SegmentEntries {
    /// Active unpublished writes must not lose publication authority. Read
    /// leases still permit eviction and independently fence physical reuse.
    fn can_evict(&self, _segment: SegmentId) -> bool {
        true
    }

    /// Remove at most budget current mappings, returning the number removed.
    fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize;

    /// Report whether any current mapping still refers to this segment.
    fn is_empty(&self, segment: SegmentId) -> bool;
}

/// One worker-local cursor and recent-read set for index and physical reclamation.
#[repr(align(64))]
pub struct SegmentClock {
    segments: Rc<Segments>,

    hand: Cell<usize>,

    recent: RefCell<HashSet<SegmentId>>,

    restore_epoch: Cell<u64>,
}

impl SegmentClock {
    /// Attach bounded reclamation to one table without cloning its authority.
    pub fn new(segments: Rc<Segments>) -> Self {
        let epoch = segments.restore_epoch();
        Self {
            segments,
            hand: Cell::new(0),
            recent: RefCell::new(HashSet::new()),
            restore_epoch: Cell::new(epoch),
        }
    }

    /// Give live segments a second chance; ignore completions arriving after eviction.
    pub fn mark_read(&self, segment: SegmentId) -> Result<()> {
        self.sync_restore();
        if !matches!(
            self.segments.state(segment)?,
            SegmentState::Open | SegmentState::Sealed
        ) {
            return Ok(());
        }
        self.recent.borrow_mut().insert(segment);
        Ok(())
    }

    /// Clear transient eviction history after successful table recovery.
    fn sync_restore(&self) {
        let epoch = self.segments.restore_epoch();
        if self.restore_epoch.replace(epoch) != epoch {
            self.hand.set(0);
            self.recent.borrow_mut().clear();
        }
    }

    /// Advance one slot; callers only invoke this inside a nonempty bounded sweep.
    fn next(&self, count: usize) -> SegmentId {
        let hand = self.hand.get() % count;
        self.hand.set((hand + 1) % count);
        SegmentId(hand as u64)
    }

    /// Forget one mapping per candidate until admission succeeds, without changing
    /// bytes or generations. At most two rotations, further capped by max_visits.
    /// Freeze does not block caller-owned index mutation.
    pub fn reclaim_index(
        &self,
        entries: &impl SegmentEntries,
        max_visits: usize,
        mut ready: impl FnMut() -> bool,
    ) -> Result<()> {
        self.sync_restore();
        if ready() {
            return Ok(());
        }
        let count = self.segments.count();
        for _ in 0..count.saturating_mul(2).min(max_visits) {
            let id = self.next(count);
            if self.recent.borrow_mut().remove(&id) {
                continue;
            }
            if entries.remove_bounded(id, 1) > 1 {
                return Err(Error::InvalidConfiguration);
            }
            if ready() {
                return Ok(());
            }
        }
        Err(Error::Busy)
    }

    /// Rank at most 64 eligible slots before entering eviction. Scores are soft
    /// preferences, not pins. The callback must itself bound mapping inspection.
    /// Existing eviction work sorts first so partial removals always make progress.
    /// Unlike the legacy clock, logical heat is supplied by the caller, so recent
    /// disk completions do not override value ranking.
    /// For a nonzero reserve, success requires both the reserve (capped at the slot
    /// count) and no pending evictions. Busy is intentional even if the reserve is
    /// met while evictions remain. Use [`Segments::free_count`] to check capacity;
    /// keep retrying bounded reclamation with a nonzero reserve to drain evictions.
    /// A zero reserve is a no-op, not a drain request.
    pub fn reclaim_scored(
        &self,
        entries: &impl SegmentEntries,
        free_reserve: usize,
        max_visits: usize,
        max_entries: usize,
        mut score: impl FnMut(SegmentId) -> u64,
    ) -> Result<()> {
        self.sync_restore();
        if free_reserve == 0 {
            return Ok(());
        }
        let count = self.segments.count();
        if count == 0 {
            return Err(Error::Unavailable);
        }
        let target = free_reserve.min(count);
        if self.segments.free_count() >= target && self.segments.evicting.get() == 0 {
            return Ok(());
        }
        let mut candidates = Vec::new();
        for visit in 0..count.min(max_visits).min(64) {
            let id = self.next(count);
            let state = self.segments.state(id)?;
            if !matches!(state, SegmentState::Sealed | SegmentState::Evicting) {
                continue;
            }
            if state == SegmentState::Sealed && !entries.can_evict(id) {
                continue;
            }
            // Rank before any begin_eviction or index side effects.
            let value = if state == SegmentState::Evicting {
                0
            } else {
                score(id)
            };
            candidates.push((state != SegmentState::Evicting, value, visit, id));
        }
        candidates.sort_by_key(|(sealed, value, visit, _)| (*sealed, *value, *visit));
        let mut entries_left = max_entries;
        for (_, _, _, id) in candidates {
            let state = self.segments.state(id)?;
            if state == SegmentState::Sealed
                && (self
                    .segments
                    .free_count()
                    .saturating_add(self.segments.evicting.get())
                    >= target
                    || !entries.can_evict(id))
            {
                continue;
            }
            if entries_left == 0 && !entries.is_empty(id) {
                continue;
            }
            self.segments.begin_eviction(id)?;
            self.recent.borrow_mut().remove(&id);
            if entries_left != 0 && !entries.is_empty(id) {
                let removed = entries.remove_bounded(id, entries_left);
                if removed > entries_left {
                    return Err(Error::InvalidConfiguration);
                }
                entries_left -= removed;
            }
            if !entries.is_empty(id) {
                continue;
            }
            match self.segments.recycle_evicted(id) {
                Ok(()) | Err(Error::Busy) => {}
                Err(error) => return Err(error),
            }
        }
        if self.segments.free_count() >= target && self.segments.evicting.get() == 0 {
            Ok(())
        } else {
            Err(Error::Busy)
        }
    }

    /// Reclaim a reserve with bounded visits and mapping removals, without compaction.
    /// Busy leases keep segments Evicting until a later sweep. A zero reserve is a
    /// no-op; a zero mapping budget can recycle empty but not populated segments.
    /// For a nonzero reserve, success requires both the reserve (capped at the slot
    /// count) and no pending evictions. Busy is intentional even if the reserve is
    /// met while evictions remain. Use [`Segments::free_count`] to check capacity;
    /// keep retrying bounded reclamation with a nonzero reserve to drain evictions.
    pub fn reclaim(
        &self,
        entries: &impl SegmentEntries,
        free_reserve: usize,
        max_visits: usize,
        max_entries: usize,
    ) -> Result<()> {
        self.sync_restore();
        if free_reserve == 0 {
            return Ok(());
        }
        let count = self.segments.count();
        if count == 0 {
            return Err(Error::Unavailable);
        }
        let target = free_reserve.min(count);
        let mut free = self.segments.free_count();
        let mut entries_left = max_entries;
        for _ in 0..count.saturating_mul(2).min(max_visits) {
            if free >= target && self.segments.evicting.get() == 0 {
                return Ok(());
            }
            let id = self.next(count);
            let state = self.segments.state(id)?;
            if !matches!(state, SegmentState::Sealed | SegmentState::Evicting) {
                continue;
            }
            if state == SegmentState::Sealed
                && (free.saturating_add(self.segments.evicting.get()) >= target
                    || !entries.can_evict(id))
            {
                continue;
            }
            if self.recent.borrow_mut().remove(&id) {
                continue;
            }
            if entries_left == 0 && !entries.is_empty(id) {
                continue;
            }
            self.segments.begin_eviction(id)?;
            if entries_left != 0 && !entries.is_empty(id) {
                let removed = entries.remove_bounded(id, entries_left);
                if removed > entries_left {
                    return Err(Error::InvalidConfiguration);
                }
                entries_left -= removed;
            }
            if !entries.is_empty(id) {
                continue;
            }
            match self.segments.recycle_evicted(id) {
                Ok(()) => free += 1,
                Err(Error::Busy) => {}
                Err(e) => return Err(e),
            }
        }
        if free >= target && self.segments.evicting.get() == 0 {
            Ok(())
        } else {
            Err(Error::Busy)
        }
    }
}

/// Pure state-machine tests, including synthetic overflow and invalid recovery images.
#[cfg(test)]
mod tests {
    use super::*;

    /// Build a bounded free table with deterministic alignment.
    fn segments(bytes: u64, count: usize) -> Segments {
        let s = Segments::new(bytes);
        s.configure(
            bytes * count as u64,
            count,
            Alignment::new(512, 512, 512).unwrap(),
        )
        .unwrap();
        s
    }

    /// Lease ownership prevents reuse and old-generation acquisition.
    #[test]
    fn no_reuse_before_lease_drop_and_no_aba() {
        let s = segments(1024, 2);
        let a = s.append(1024).unwrap();
        s.begin_evict(a.0.id()).unwrap();
        assert_eq!(s.recycle(a.0.id()), Err(Error::Busy));
        drop(a);
        s.recycle(SegmentId(0)).unwrap();
        assert!(s.lease(SegmentId(0), Generation(1)).is_err());
        assert_eq!(s.append(512).unwrap().0.generation(), Generation(2));
        let frozen = s.freeze().unwrap();
        assert!(s.append(512).is_err());
        drop(frozen);
    }

    /// Recovery seals the append tail and rejects unaligned occupancy.
    #[test]
    fn restore_seals_open_and_rejects_invalid_geometry() {
        let s = segments(1024, 1);
        drop(s.append(512).unwrap());
        let mut snap = s.snapshot();
        s.restore(snap.clone()).unwrap();
        assert_eq!(s.snapshot()[0].state, SegmentState::Sealed);
        snap[0].used_bytes = 513;
        assert!(s.restore(snap).is_err());
    }

    /// Reject rollback atomically while allowing equal or newer generations.
    #[test]
    fn restore_rejects_generation_rollback_without_reviving_stale_mappings() {
        let s = segments(1024, 2);
        drop(s.append(1024).unwrap());
        let (lease, extent) = s.append(1024).unwrap();
        let id = lease.id();
        let generation = lease.generation();
        drop(lease);
        let old_sealed = s.snapshot();
        s.begin_evict(id).unwrap();
        s.recycle(id).unwrap();
        let before = s.snapshot();
        let epoch = s.restore_epoch();

        for state in [
            SegmentState::Free,
            SegmentState::Open,
            SegmentState::Sealed,
            SegmentState::Evicting,
        ] {
            let mut images = old_sealed.clone();
            images[0].generation = Generation(3);
            images[1].state = state;
            images[1].used_bytes = match state {
                SegmentState::Free => 0,
                SegmentState::Open => 512,
                _ => 1024,
            };
            assert_eq!(s.validate_restore(&images), Err(Error::Corrupt));
            assert_eq!(s.restore(images), Err(Error::Corrupt));
            assert_eq!(s.snapshot(), before);
            assert_eq!(s.restore_epoch(), epoch);
            assert_eq!(*s.free.borrow(), BTreeSet::from([1]));
            assert_eq!(s.open.get(), None);
            assert_eq!(s.evicting.get(), 0);
        }

        let (lease, new_extent) = s.append(1024).unwrap();
        assert_eq!(lease.id(), id);
        assert_eq!(lease.generation(), Generation(2));
        assert_eq!(new_extent, extent);
        assert_eq!(s.validate(id, generation, &extent), Err(Error::Stale));
        assert!(matches!(s.lease(id, generation), Err(Error::Stale)));
        drop(lease);

        let mut images = s.snapshot();
        assert_eq!(s.validate_restore(&images), Ok(()));
        s.restore(images.clone()).unwrap();
        images[1].generation = Generation(3);
        assert_eq!(s.validate_restore(&images), Ok(()));
        s.restore(images).unwrap();
        assert_eq!(s.snapshot()[1].generation, Generation(3));
        assert_eq!(s.validate(id, generation, &extent), Err(Error::Stale));
    }

    /// Tail rotation and generation exhaustion never wrap into stale authority.
    #[test]
    fn generation_exhaustion_never_wraps_and_small_tails_are_sealed() {
        let s = segments(1024, 2);
        drop(s.append(512).unwrap());
        let next = s.append(1024).unwrap();
        assert_eq!(next.0.id(), SegmentId(1));
        drop(next);
        assert_eq!(s.state(SegmentId(0)).unwrap(), SegmentState::Sealed);
        let mut snapshot = s.snapshot();
        snapshot[0].generation = Generation(u64::MAX);
        s.restore(snapshot).unwrap();
        s.begin_evict(SegmentId(0)).unwrap();
        assert_eq!(s.recycle(SegmentId(0)), Err(Error::Unavailable));
        assert_eq!(s.snapshot()[0].generation, Generation(u64::MAX));
    }

    /// Validation distinguishes stale identity, corrupt ranges, and busy ownership.
    #[test]
    fn validation_rejects_unwritten_ranges_generations_and_busy_restore() {
        let s = segments(1024, 1);
        assert!(s.append(0).is_err());
        assert!(s.append(513).is_err());
        let (lease, extent) = s.append(512).unwrap();
        assert_eq!(s.validate(lease.id(), lease.generation(), &extent), Ok(()));
        assert_eq!(
            s.validate(lease.id(), Generation(2), &extent),
            Err(Error::Stale)
        );
        assert_eq!(
            s.validate(
                lease.id(),
                lease.generation(),
                &Extent::new(512, 512).unwrap()
            ),
            Err(Error::Corrupt)
        );
        let mut images = s.snapshot();
        assert_eq!(s.restore(images.clone()), Err(Error::Busy));
        drop(lease);
        images[0].generation = Generation(0);
        assert_eq!(s.restore(images), Err(Error::Corrupt));
        assert_eq!(s.state(SegmentId(0)).unwrap(), SegmentState::Open);
        assert_eq!(s.count(), 1);
        assert_eq!(s.free_count(), 0);
        assert_eq!(s.capacity_bytes(), 1024);
        let frozen = s.freeze().unwrap();
        assert!(matches!(s.freeze(), Err(Error::Busy)));
        assert_eq!(s.begin_evict(SegmentId(0)), Err(Error::Busy));
        assert_eq!(s.recycle(SegmentId(0)), Err(Error::Busy));
        drop(frozen);
        drop(s.append(512).unwrap());
        assert!(matches!(s.append(512), Err(Error::Busy)));
    }

    /// Invalid configuration does not partially install geometry or slots.
    #[test]
    fn configuration_rejects_zero_and_misaligned_capacity() {
        let a = Alignment::new(512, 512, 512).unwrap();
        for (bytes, capacity, count) in [
            (0, 1024, 1),
            (1024, 0, 1),
            (513, 1026, 2),
            (1024, 1024, 0),
            (1024, 1024, 2),
            (1024, 1025, 1),
            (1024, 1024 * 1_000_001, 1_000_001),
        ] {
            assert_eq!(
                Segments::new(bytes).configure(capacity, count, a),
                Err(Error::InvalidConfiguration)
            );
        }
        let s = segments(1024, 1);
        assert_eq!(s.configure(1024, 1, a), Err(Error::InvalidConfiguration));
        assert!(s.state(SegmentId(u64::MAX)).is_err());
        let s = Segments::new(1024);
        assert_eq!(s.configure(1025, 1, a), Err(Error::InvalidConfiguration));
        assert!(!s.is_configured());
        assert_eq!(s.geometry(), None);
        assert_eq!(s.capacity_bytes(), 0);
        assert_eq!(s.free_count(), 0);
        assert!(s.snapshot().is_empty());
        s.configure(1024, 1, a).unwrap();
    }

    /// Malformed append and synthetic saturation cannot consume or seal slots.
    #[test]
    fn append_failures_preserve_open_tail_free_list_and_lease_counts() {
        let s = segments(1024, 1);
        drop(s.append(512).unwrap());
        let before = s.snapshot();
        for length in [0, 1, 513, 1536] {
            assert!(s.append(length).is_err());
            assert_eq!(s.snapshot(), before);
            assert_eq!(s.open.get(), Some(0));
            assert_eq!(s.free_count(), 0);
            assert_eq!(s.slots.borrow()[0].leases.get(), 0);
        }
        drop(s.append(512).unwrap());
        assert_eq!(s.state(SegmentId(0)), Ok(SegmentState::Sealed));
        let s = segments(1024, 2);
        drop(s.append(512).unwrap());
        for (position, length) in [(0, 512), (1, 1024)] {
            s.slots.borrow()[position].leases.set(usize::MAX);
            let before = s.snapshot();
            assert!(matches!(s.append(length), Err(Error::Busy)));
            assert_eq!(s.snapshot(), before);
            assert_eq!(s.open.get(), Some(0));
            assert_eq!(*s.free.borrow(), BTreeSet::from([1]));
            assert_eq!(s.slots.borrow()[position].leases.get(), usize::MAX);
            s.slots.borrow()[position].leases.set(0);
        }
        drop(s.append(1024).unwrap());
        assert_eq!(s.state(SegmentId(0)), Ok(SegmentState::Sealed));
        let s = segments(1024, 2);
        drop(s.append(512).unwrap());
        s.slots.borrow_mut()[1].image.id = SegmentId(u64::MAX);
        let before = s.snapshot();
        assert!(matches!(s.append(1024), Err(Error::InvalidConfiguration)));
        assert_eq!(s.snapshot(), before);
        assert_eq!(s.open.get(), Some(0));
        assert_eq!(*s.free.borrow(), BTreeSet::from([1]));
        assert_eq!(s.slots.borrow()[1].leases.get(), 0);
    }

    /// Freeze ownership survives table destruction and releases during unwinding.
    #[test]
    fn freeze_guard_drop_thaws_and_can_outlive_table() {
        let s = segments(1024, 1);
        let frozen = s.freeze().unwrap();
        let before = s.snapshot();
        assert_eq!(s.restore(before.clone()), Err(Error::Busy));
        assert_eq!(s.validate_restore(&before), Err(Error::Busy));
        assert!(matches!(s.append(512), Err(Error::Busy)));
        assert!(matches!(s.freeze(), Err(Error::Busy)));
        assert_eq!(s.snapshot(), before);
        drop(frozen);
        drop(s.append(512).unwrap());
        let frozen = s.freeze().unwrap();
        drop(s);
        drop(frozen);
        let s = Segments::new(1024);
        let frozen = s.freeze().unwrap();
        assert_eq!(
            s.configure(1024, 1, Alignment::new(512, 512, 512).unwrap()),
            Err(Error::Busy)
        );
        drop(frozen);
        assert!(!s.is_configured());
        assert_eq!(s.geometry(), None);
        let s = segments(1024, 1);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                let _frozen = s.freeze().unwrap();
                panic!("simulated owner failure");
            }))
            .is_err()
        );
        drop(s.append(512).unwrap());
    }

    /// A lease retains both validated dimensions and the nominal table identity.
    #[test]
    fn geometry_is_shared_by_configuration_and_leases() {
        let geometry =
            SegmentGeometry::new(4096, 1024, 2, Alignment::new(512, 512, 512).unwrap()).unwrap();
        let s = Segments::from_geometry(geometry).unwrap();
        assert!(s.is_configured());
        assert_eq!(s.geometry(), Some(geometry));
        assert_eq!(s.count(), 2);
        assert_eq!(s.free_count(), 2);
        let (lease, extent) = s.append(512).unwrap();
        assert_eq!(lease.geometry(), geometry);
        assert!(lease.table_identity().matches(&s.table_identity()));
        assert_eq!(s.validate_lease(&lease, &extent), Ok(()));
    }

    /// Large sparse capacity need not allocate an equally large slot table.
    #[test]
    fn large_physical_geometry_supports_bounded_partial_tables() {
        let capacity = 1024 * (MAX_SEGMENTS + 1);
        let alignment = Alignment::new(512, 512, 512).unwrap();
        let full = SegmentGeometry::new(capacity, 1024, MAX_SEGMENTS + 1, alignment).unwrap();
        assert!(matches!(
            Segments::from_geometry(full),
            Err(Error::InvalidConfiguration)
        ));
        let s = Segments::new(1024);
        assert_eq!(
            s.configure(capacity, (MAX_SEGMENTS + 1) as usize, alignment),
            Err(Error::InvalidConfiguration)
        );
        assert!(!s.is_configured());
        s.configure(capacity, 2, alignment).unwrap();
        assert_eq!(s.capacity_bytes(), capacity);
        assert_eq!(s.count(), 2);
        assert_eq!(s.geometry().unwrap().segment_count(), 2);
        drop(s.append(1024).unwrap());
        assert_eq!(s.free_count(), 1);
    }

    /// Existing leases authorize only their captured prefix, including during eviction.
    #[test]
    fn leases_validate_table_and_captured_used_range_through_eviction() {
        let s = segments(1024, 1);
        let other = segments(1024, 1);
        let (lease, extent) = s.append(512).unwrap();
        let (_, later) = s.append(512).unwrap();
        assert_eq!(s.validate_lease(&lease, &later), Err(Error::Corrupt));
        assert_eq!(other.validate_lease(&lease, &extent), Err(Error::Stale));
        assert_eq!(
            lease.validate_extent(&Extent::new(1, 511).unwrap()),
            Err(Error::Corrupt)
        );
        assert_eq!(
            lease.validate_extent(&Extent::new(0, 1).unwrap()),
            Err(Error::Corrupt)
        );
        s.begin_evict(SegmentId(0)).unwrap();
        assert_eq!(s.validate_lease(&lease, &extent), Ok(()));
        assert_eq!(
            s.validate(lease.id(), lease.generation(), &extent),
            Err(Error::Stale)
        );
        assert!(matches!(
            s.lease(lease.id(), lease.generation()),
            Err(Error::Stale)
        ));
        assert!(matches!(
            s.lease(SegmentId(1), Generation(1)),
            Err(Error::Corrupt)
        ));
        drop(s);
        assert_eq!(lease.validate_extent(&extent), Ok(()));
        drop(lease);
    }

    /// Stored malformed ranges are corruption rather than configuration errors.
    #[test]
    fn malformed_extents_are_corrupt_not_configuration_errors() {
        let s = segments(1024, 1);
        let (lease, _) = s.append(1024).unwrap();
        for extent in [
            Extent::new(1, 512).unwrap(),
            Extent::new(0, 513).unwrap(),
            Extent::new(1024, 512).unwrap(),
        ] {
            assert_eq!(
                s.validate(lease.id(), lease.generation(), &extent),
                Err(Error::Corrupt)
            );
        }
    }

    /// Recovery validates all states atomically and fails before epoch wraparound.
    #[test]
    fn invalid_restore_states_are_atomic_and_valid_states_round_trip() {
        let s = segments(1024, 3);
        drop(s.append(1024).unwrap());
        drop(s.append(512).unwrap());
        let before = s.snapshot();
        let mut cases = Vec::new();
        for state in [
            SegmentState::Open,
            SegmentState::Sealed,
            SegmentState::Evicting,
        ] {
            let mut images = before.clone();
            images[2].state = state;
            cases.push(images);
        }
        let mut images = before.clone();
        images[0].state = SegmentState::Open;
        cases.push(images);
        let mut images = before.clone();
        images[0].state = SegmentState::Open;
        images[0].used_bytes = 512;
        cases.push(images);
        for images in cases {
            assert_eq!(s.restore(images), Err(Error::Corrupt));
            assert_eq!(s.snapshot(), before);
            assert_eq!(s.free_count(), 1);
            assert_eq!(s.open.get(), Some(1));
            assert_eq!(s.restore_epoch(), 0);
        }
        let mut images = before;
        images[0].state = SegmentState::Evicting;
        s.restore(images).unwrap();
        assert_eq!(s.restore_epoch(), 1);
        assert_eq!(s.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(s.state(SegmentId(1)), Ok(SegmentState::Sealed));
        s.recycle(SegmentId(0)).unwrap();
        assert_eq!(s.free_count(), 2);
        let before = s.snapshot();
        s.restore_epoch.set(u64::MAX - 1);
        assert_eq!(s.validate_restore(&before), Ok(()));
        s.restore(before.clone()).unwrap();
        assert_eq!(s.restore_epoch(), u64::MAX);
        assert_eq!(s.snapshot(), before);
        s.restore_epoch.set(u64::MAX);
        assert_eq!(s.validate_restore(&before), Err(Error::Unavailable));
        assert_eq!(s.restore(before.clone()), Err(Error::Unavailable));
        assert_eq!(s.snapshot(), before);
    }
}

/// Bounded sweep state-space coverage, including invalid callbacks and recovery.
#[cfg(test)]
mod clock_tests {
    use super::*;

    /// Deterministic index occupancy and removal-call observations.
    struct Entries {
        counts: RefCell<Vec<usize>>,

        calls: RefCell<Vec<(SegmentId, usize)>>,

        evictable: Cell<bool>,
    }

    impl Entries {
        /// Populate a synthetic index without changing allocator state.
        fn new(counts: Vec<usize>) -> Self {
            Self {
                counts: RefCell::new(counts),
                calls: RefCell::new(vec![]),
                evictable: Cell::new(true),
            }
        }
    }

    impl SegmentEntries for Entries {
        /// Allow tests to protect unpublished mappings before eviction begins.
        fn can_evict(&self, _: SegmentId) -> bool {
            self.evictable.get()
        }

        /// Record and honor each bounded removal request.
        fn remove_bounded(&self, id: SegmentId, budget: usize) -> usize {
            self.calls.borrow_mut().push((id, budget));
            let mut counts = self.counts.borrow_mut();
            let count = &mut counts[id.0 as usize];
            let removed = (*count).min(budget);
            *count -= removed;
            removed
        }

        /// Read current occupancy after bounded removal.
        fn is_empty(&self, id: SegmentId) -> bool {
            self.counts.borrow()[id.0 as usize] == 0
        }
    }

    /// Create a shared table with enough slots for a bounded clock scenario.
    fn segments(count: usize) -> Rc<Segments> {
        let segments = Rc::new(Segments::new(1024));
        segments
            .configure(
                1024 * count as u64,
                count,
                Alignment::new(512, 512, 512).unwrap(),
            )
            .unwrap();
        segments
    }

    /// Empty tables and zero visits never invoke mapping removal.
    #[test]
    fn empty_and_zero_budget_sweeps_do_not_call_entries() {
        let clock = SegmentClock::new(Rc::new(Segments::new(1024)));
        let entries = Entries::new(vec![]);
        assert_eq!(clock.reclaim_index(&entries, 64, || true), Ok(()));
        assert_eq!(
            clock.reclaim_index(&entries, 64, || false),
            Err(Error::Busy)
        );
        assert_eq!(clock.reclaim(&entries, 1, 64, 256), Err(Error::Unavailable));
        assert_eq!(clock.mark_read(SegmentId(0)), Err(Error::Corrupt));
        assert!(entries.calls.borrow().is_empty());
        let clock = SegmentClock::new(segments(1));
        let entries = Entries::new(vec![1]);
        assert_eq!(clock.reclaim_index(&entries, 0, || false), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
    }

    /// Index admission can forget open mappings without reclaiming their storage.
    #[test]
    fn index_second_chance_removes_open_mappings_without_recycling() {
        let segments = segments(2);
        drop(segments.append(512).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1, 1]);
        clock.mark_read(SegmentId(0)).unwrap();
        clock
            .reclaim_index(&entries, 64, || entries.counts.borrow()[1] == 0)
            .unwrap();
        assert_eq!(*entries.counts.borrow(), [1, 0]);
        clock.mark_read(SegmentId(0)).unwrap();
        clock
            .reclaim_index(&entries, 64, || entries.counts.borrow()[0] == 0)
            .unwrap();
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Open));
        assert!(segments.lease(SegmentId(0), Generation(1)).is_ok());
        assert_eq!(segments.free_count(), 1);
    }

    /// Calls resume the prior cursor but never exceed visits or two rotations.
    #[test]
    fn sweep_budget_persists_cursor_and_limits_to_two_rotations() {
        let clock = SegmentClock::new(segments(40));
        let entries = Entries::new(vec![10; 40]);
        assert_eq!(
            clock.reclaim_index(&entries, 64, || false),
            Err(Error::Busy)
        );
        assert_eq!(entries.calls.borrow().len(), 64);
        assert_eq!(entries.calls.borrow()[63], (SegmentId(23), 1));
        entries.calls.borrow_mut().clear();
        assert_eq!(clock.reclaim_index(&entries, 1, || false), Err(Error::Busy));
        assert_eq!(*entries.calls.borrow(), [(SegmentId(24), 1)]);
        let clock = SegmentClock::new(segments(2));
        let entries = Entries::new(vec![10; 2]);
        assert_eq!(
            clock.reclaim_index(&entries, 64, || false),
            Err(Error::Busy)
        );
        assert_eq!(entries.calls.borrow().len(), 4);
    }

    /// Mapping budget exhaustion leaves partial eviction for the next bounded call.
    #[test]
    fn mapping_budget_keeps_partial_eviction_until_next_sweep() {
        let segments = segments(2);
        drop(segments.append(1024).unwrap());
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![257, 0]);
        assert_eq!(clock.reclaim(&entries, 2, 64, 256), Err(Error::Busy));
        assert_eq!(*entries.counts.borrow(), [1, 0]);
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 256)]);
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 1);
        clock.reclaim(&entries, 2, 64, 256).unwrap();
        assert_eq!(segments.free_count(), 2);
        assert_eq!(*entries.counts.borrow(), [0, 0]);
    }

    /// Sealed vetoes preserve mappings, but cannot strand eviction already in progress.
    #[test]
    fn reclaim_honors_sealed_veto_but_drains_existing_eviction() {
        for mapping_count in [0usize, 2] {
            let segments = segments(1);
            let held = segments.append(1024).unwrap().0;
            let clock = SegmentClock::new(segments.clone());
            let entries = Entries::new(vec![mapping_count]);
            entries.evictable.set(false);
            let before = segments.snapshot();
            assert_eq!(before[0].state, SegmentState::Sealed);

            assert_eq!(clock.reclaim(&entries, 1, 2, 1), Err(Error::Busy));
            assert_eq!(segments.snapshot(), before);
            assert_eq!(*entries.counts.borrow(), [mapping_count]);
            assert!(entries.calls.borrow().is_empty());
            assert_eq!(segments.free_count(), 0);

            entries.evictable.set(true);
            assert_eq!(clock.reclaim(&entries, 1, 1, 1), Err(Error::Busy));
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
            assert_eq!(*entries.counts.borrow(), [mapping_count.saturating_sub(1)]);

            // Once eviction starts, a later veto must not block remaining mappings.
            entries.evictable.set(false);
            assert_eq!(clock.reclaim(&entries, 1, 1, 1), Err(Error::Busy));
            assert_eq!(*entries.counts.borrow(), [0]);
            assert_eq!(entries.calls.borrow().len(), mapping_count);
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
            assert_eq!(segments.free_count(), 0);
            assert_eq!(segments.snapshot()[0].generation, Generation(1));

            // The live lease, not the veto, remains the physical reuse fence.
            drop(held);
            clock.reclaim(&entries, 1, 1, 0).unwrap();
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Free));
            assert_eq!(segments.free_count(), 1);
            assert_eq!(segments.snapshot()[0].generation, Generation(2));
        }
    }

    /// Pending victims satisfy demand without evicting more leased segments.
    #[test]
    fn reclaim_pending_victims_count_toward_reserve() {
        let segments = segments(3);
        let mut leases: Vec<_> = (0..3)
            .map(|_| Some(segments.append(1024).unwrap().0))
            .collect();
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1; 3]);
        for _ in 0..6 {
            assert_eq!(clock.reclaim(&entries, 1, 1, 256), Err(Error::Busy));
            assert_eq!(*entries.counts.borrow(), [0, 1, 1]);
            assert_eq!(segments.evicting.get(), 1);
            assert_eq!(segments.free_count(), 0);
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
            assert!(segments.lease(SegmentId(1), Generation(1)).is_ok());
            assert!(segments.lease(SegmentId(2), Generation(1)).is_ok());
        }
        drop(leases[0].take());
        clock.reclaim(&entries, 1, 6, 0).unwrap();
        assert_eq!(segments.free_count(), 1);
        assert_eq!(segments.evicting.get(), 0);
        assert_eq!(*entries.counts.borrow(), [0, 1, 1]);
        assert_eq!(segments.snapshot()[0].generation, Generation(2));
    }

    /// Lowering the target still drains existing victims within each visit budget.
    #[test]
    fn reclaim_pending_victims_drain_after_reserve_is_met() {
        for max_visits in [1, 8] {
            let segments = segments(4);
            let mut leases: Vec<_> = (0..3)
                .map(|_| Some(segments.append(1024).unwrap().0))
                .collect();
            drop(segments.append(1024).unwrap());
            let clock = SegmentClock::new(segments.clone());
            let entries = Entries::new(vec![1; 4]);
            assert_eq!(clock.reclaim(&entries, 3, 3, 3), Err(Error::Busy));
            assert_eq!(segments.evicting.get(), 3);
            assert_eq!(*entries.counts.borrow(), [0, 0, 0, 1]);

            // Wrap the cursor without starting a fourth victim.
            entries.evictable.set(false);
            assert_eq!(clock.reclaim(&entries, 1, 1, 1), Err(Error::Busy));
            drop(leases[0].take());
            assert_eq!(clock.reclaim(&entries, 1, max_visits, 0), Err(Error::Busy));
            assert_eq!(segments.free_count(), 1);
            assert_eq!(segments.evicting.get(), 2);
            assert!(matches!(
                segments.lease(SegmentId(1), Generation(1)),
                Err(Error::Stale)
            ));

            drop(leases);
            let before = segments.snapshot();
            assert_eq!(clock.reclaim(&entries, 0, 8, 8), Ok(()));
            assert_eq!(clock.reclaim(&entries, 1, 0, 0), Err(Error::Busy));
            assert_eq!(segments.snapshot(), before);
            for _ in 0..4 {
                let _ = clock.reclaim(&entries, 1, max_visits, 0);
            }
            assert_eq!(clock.reclaim(&entries, 1, 0, 0), Ok(()));
            assert_eq!(segments.free_count(), 3);
            assert_eq!(segments.evicting.get(), 0);
            assert_eq!(segments.state(SegmentId(3)), Ok(SegmentState::Sealed));
            assert_eq!(*entries.counts.borrow(), [0, 0, 0, 1]);
            for image in &segments.snapshot()[..3] {
                assert_eq!(image.state, SegmentState::Free);
                assert_eq!(image.generation, Generation(2));
            }
        }
    }

    /// Freeze checks precede index side effects, and leases precede physical reuse.
    #[test]
    fn busy_lease_and_frozen_table_preserve_reclaim_side_effect_order() {
        let segments = segments(2);
        let held = segments.append(1024).unwrap();
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1, 1]);
        let frozen = segments.freeze().unwrap();
        assert_eq!(clock.reclaim(&entries, 2, 64, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
        drop(frozen);
        clock.mark_read(SegmentId(1)).unwrap();
        assert_eq!(clock.reclaim(&entries, 2, 64, 256), Err(Error::Busy));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 1);
        assert_eq!(clock.mark_read(SegmentId(0)), Ok(()));
        assert_eq!(clock.mark_read(SegmentId(1)), Ok(()));
        assert!(clock.recent.borrow().is_empty());
        drop(held);
        clock.reclaim(&entries, 2, 64, 256).unwrap();
        assert_eq!(segments.free_count(), 2);
        assert!(segments.lease(SegmentId(0), Generation(1)).is_err());
    }

    /// Skipping an open segment does not consume its future second chance.
    #[test]
    fn physical_sweep_preserves_recent_bit_on_ineligible_open_segment() {
        let segments = segments(1);
        drop(segments.append(512).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1]);
        clock.mark_read(SegmentId(0)).unwrap();
        assert_eq!(clock.reclaim(&entries, 1, 64, 256), Err(Error::Busy));
        drop(segments.append(512).unwrap());
        assert_eq!(clock.reclaim(&entries, 1, 1, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
        clock.reclaim(&entries, 1, 1, 256).unwrap();
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 256)]);
    }

    /// A no-space rollover creates a reclaimable tail without reserving bytes.
    #[test]
    fn no_space_rollover_seals_tail_for_reclaim_and_retry() {
        for retain_lease in [false, true] {
            let segments = segments(1);
            let mut held = Some(segments.append(512).unwrap().0);
            if !retain_lease {
                drop(held.take());
            }
            assert!(matches!(segments.append(1024), Err(Error::Busy)));
            let image = &segments.snapshot()[0];
            assert_eq!(image.state, SegmentState::Sealed);
            assert_eq!(image.used_bytes, 512);
            assert_eq!(image.generation, Generation(1));
            let clock = SegmentClock::new(segments.clone());
            let entries = Entries::new(vec![0]);
            if retain_lease {
                assert_eq!(clock.reclaim(&entries, 1, 2, 0), Err(Error::Busy));
                assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
                drop(held.take());
            }
            clock.reclaim(&entries, 1, 2, 0).unwrap();
            assert!(entries.calls.borrow().is_empty());
            let (lease, extent) = segments.append(1024).unwrap();
            assert_eq!(lease.id(), SegmentId(0));
            assert_eq!(lease.generation(), Generation(2));
            assert_eq!(extent.offset(), 0);
            assert_eq!(extent.length(), 1024);
        }
    }

    /// Zero mapping work cannot start populated eviction but may recycle empty slots.
    #[test]
    fn zero_budget_does_not_start_populated_eviction_but_recycles_empty_candidates() {
        let segments = segments(2);
        drop(segments.append(1024).unwrap());
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1, 0]);
        clock.reclaim(&entries, 1, 2, 0).unwrap();
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
        assert_eq!(segments.state(SegmentId(1)), Ok(SegmentState::Free));
        assert_eq!(*entries.counts.borrow(), [1, 0]);
        assert!(entries.calls.borrow().is_empty());
        assert_eq!(clock.reclaim(&entries, 2, 64, 0), Err(Error::Busy));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
    }

    /// A zero reserve does not inspect or change allocation and mapping state.
    #[test]
    fn zero_reserve_has_no_side_effects_even_when_unconfigured() {
        let unconfigured = SegmentClock::new(Rc::new(Segments::new(1024)));
        assert_eq!(
            unconfigured.reclaim(&Entries::new(vec![]), 0, 64, 256),
            Ok(())
        );
        let segments = segments(1);
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1]);
        clock.mark_read(SegmentId(0)).unwrap();
        assert_eq!(clock.reclaim(&entries, 0, 64, 256), Ok(()));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
        assert!(entries.calls.borrow().is_empty());
        assert!(clock.recent.borrow().contains(&SegmentId(0)));
    }

    /// Scoring completes before mutation; mapping budgets, freeze, leases and
    /// generation authority remain enforced even when the cheapest slot is busy.
    #[test]
    fn scored_eviction_preserves_fences_and_partial_progress() {
        let segments = segments(3);
        drop(segments.append(1024).unwrap());
        let held = segments.append(1024).unwrap().0;
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1, 2, 1]);
        let scores = [30, 0, 10];
        let frozen = segments.freeze().unwrap();
        assert_eq!(
            clock.reclaim_scored(&entries, 1, 64, 1, |id| scores[id.0 as usize]),
            Err(Error::Busy)
        );
        assert!(entries.calls.borrow().is_empty());
        drop(frozen);
        assert_eq!(
            clock.reclaim_scored(&entries, 1, 64, 1, |id| {
                assert_eq!(segments.state(id), Ok(SegmentState::Sealed));
                scores[id.0 as usize]
            }),
            Err(Error::Busy)
        );
        assert_eq!(*entries.counts.borrow(), [1, 1, 1]);
        assert_eq!(segments.state(SegmentId(1)), Ok(SegmentState::Evicting));
        assert!(segments.lease(SegmentId(1), Generation(1)).is_err());
        assert_eq!(
            clock.reclaim_scored(&entries, 1, 64, 2, |id| scores[id.0 as usize]),
            Err(Error::Busy)
        );
        assert_eq!(*entries.counts.borrow(), [1, 0, 1]);
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
        assert_eq!(segments.state(SegmentId(1)), Ok(SegmentState::Evicting));
        drop(held);
        clock
            .reclaim_scored(&entries, 1, 64, 0, |_| u64::MAX)
            .unwrap();
        assert_eq!(segments.free_count(), 1);
        assert_eq!(segments.snapshot()[1].generation, Generation(2));
    }

    /// A score callback never turns a soft preference into immunity or a full scan.
    #[test]
    fn scored_eviction_caps_candidates_and_evicts_maximum_scores() {
        let segments = segments(65);
        for _ in 0..65 {
            drop(segments.append(1024).unwrap());
        }
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1; 65]);
        let mut visits = 0;
        clock
            .reclaim_scored(&entries, 1, usize::MAX, 1, |_| {
                visits += 1;
                u64::MAX
            })
            .unwrap();
        assert_eq!(visits, 64);
        assert_eq!(entries.calls.borrow().len(), 1);
        assert_eq!(segments.free_count(), 1);
        assert_eq!(clock.hand.get(), 64);
    }

    /// Pending leased victims count toward reserve even outside the next sample.
    #[test]
    fn scored_eviction_never_overshoots_reserve_with_multiple_leased_victims() {
        let segments = segments(3);
        let mut leases: Vec<_> = (0..3)
            .map(|_| Some(segments.append(1024).unwrap().0))
            .collect();
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1; 3]);
        for _ in 0..6 {
            assert_eq!(
                clock.reclaim_scored(&entries, 1, 1, 256, |_| 0),
                Err(Error::Busy)
            );
            assert_eq!(*entries.counts.borrow(), [0, 1, 1]);
            assert_eq!(segments.evicting.get(), 1);
        }
        drop(leases[0].take());
        clock.reclaim_scored(&entries, 1, 64, 256, |_| 0).unwrap();
        assert_eq!(segments.free_count(), 1);
        assert_eq!(segments.evicting.get(), 0);
        assert_eq!(*entries.counts.borrow(), [0, 1, 1]);
    }

    /// Lowering scored demand still waits for pending leases and bounded visits.
    #[test]
    fn scored_pending_victims_drain_after_reserve_is_met() {
        for max_visits in [1, 8] {
            let segments = segments(4);
            let mut leases: Vec<_> = (0..3)
                .map(|_| Some(segments.append(1024).unwrap().0))
                .collect();
            drop(segments.append(1024).unwrap());
            let clock = SegmentClock::new(segments.clone());
            let entries = Entries::new(vec![1; 4]);
            assert_eq!(
                clock.reclaim_scored(&entries, 3, 3, 3, |_| 0),
                Err(Error::Busy)
            );
            assert_eq!(segments.evicting.get(), 3);
            assert_eq!(*entries.counts.borrow(), [0, 0, 0, 1]);

            assert_eq!(
                clock.reclaim_scored(&entries, 1, 1, 1, |_| 0),
                Err(Error::Busy)
            );
            drop(leases[0].take());
            assert_eq!(
                clock.reclaim_scored(&entries, 1, max_visits, 0, |_| 0),
                Err(Error::Busy)
            );
            assert_eq!(segments.free_count(), 1);
            assert_eq!(segments.evicting.get(), 2);
            assert!(matches!(
                segments.lease(SegmentId(1), Generation(1)),
                Err(Error::Stale)
            ));

            drop(leases);
            let before = segments.snapshot();
            assert_eq!(clock.reclaim_scored(&entries, 0, 8, 8, |_| 0), Ok(()));
            assert_eq!(
                clock.reclaim_scored(&entries, 1, 0, 0, |_| 0),
                Err(Error::Busy)
            );
            assert_eq!(segments.snapshot(), before);
            for _ in 0..4 {
                let _ = clock.reclaim_scored(&entries, 1, max_visits, 0, |_| 0);
            }
            assert_eq!(clock.reclaim_scored(&entries, 1, 0, 0, |_| 0), Ok(()));
            assert_eq!(segments.free_count(), 3);
            assert_eq!(segments.evicting.get(), 0);
            assert_eq!(segments.state(SegmentId(3)), Ok(SegmentState::Sealed));
            assert_eq!(*entries.counts.borrow(), [0, 0, 0, 1]);
            assert_eq!(entries.calls.borrow().len(), 3);
            for image in &segments.snapshot()[..3] {
                assert_eq!(image.state, SegmentState::Free);
                assert_eq!(image.generation, Generation(2));
            }
        }
    }

    /// Meeting a lower reserve does not hide unfinished index removal.
    #[test]
    fn scored_pending_mappings_drain_after_reserve_is_met() {
        let segments = segments(3);
        for _ in 0..3 {
            drop(segments.append(1024).unwrap());
        }
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![0, 3, 1]);
        assert_eq!(
            clock.reclaim_scored(&entries, 2, 3, 1, |_| 0),
            Err(Error::Busy)
        );
        assert_eq!(segments.free_count(), 1);
        assert_eq!(segments.evicting.get(), 1);
        assert_eq!(*entries.counts.borrow(), [0, 2, 1]);

        let before = segments.snapshot();
        assert_eq!(
            clock.reclaim_scored(&entries, 1, 3, 0, |_| 0),
            Err(Error::Busy)
        );
        assert_eq!(segments.snapshot(), before);
        assert_eq!(*entries.counts.borrow(), [0, 2, 1]);
        assert_eq!(entries.calls.borrow().len(), 1);
        assert_eq!(
            clock.reclaim_scored(&entries, 1, 3, 1, |_| 0),
            Err(Error::Busy)
        );
        assert_eq!(*entries.counts.borrow(), [0, 1, 1]);
        assert_eq!(segments.state(SegmentId(1)), Ok(SegmentState::Evicting));
        assert_eq!(clock.reclaim_scored(&entries, 1, 3, 1, |_| 0), Ok(()));
        assert_eq!(segments.free_count(), 2);
        assert_eq!(segments.evicting.get(), 0);
        assert_eq!(segments.snapshot()[1].generation, Generation(2));
        assert_eq!(segments.state(SegmentId(2)), Ok(SegmentState::Sealed));
        assert_eq!(*entries.counts.borrow(), [0, 0, 1]);
        assert_eq!(*entries.calls.borrow(), [(SegmentId(1), 1); 3]);
    }

    /// Over-reporting callbacks return configuration errors without unsafe reuse.
    #[test]
    fn removal_contract_violations_return_errors_without_panicking() {
        /// An intentionally broken callback that over-reports every removal.
        struct InvalidEntries;

        impl SegmentEntries for InvalidEntries {
            /// Violate the caller contract to test error handling.
            fn remove_bounded(&self, _: SegmentId, budget: usize) -> usize {
                budget + 1
            }

            /// Keep the segment populated despite the invalid removal report.
            fn is_empty(&self, _: SegmentId) -> bool {
                false
            }
        }
        let segments = segments(1);
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        assert_eq!(
            clock.reclaim_index(&InvalidEntries, 1, || false),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(
            clock.reclaim(&InvalidEntries, 1, 1, 1),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 0);
    }

    /// Every clock operation observes the restore epoch before using old history.
    #[test]
    fn restore_resets_cursor_and_recent_reads_before_any_clock_operation() {
        let segments = segments(2);
        drop(segments.append(1024).unwrap());
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![10, 10]);
        assert_eq!(clock.reclaim_index(&entries, 1, || false), Err(Error::Busy));
        clock.mark_read(SegmentId(0)).unwrap();
        assert_eq!(clock.hand.get(), 1);
        segments.restore(segments.snapshot()).unwrap();
        entries.calls.borrow_mut().clear();
        assert_eq!(clock.reclaim_index(&entries, 1, || false), Err(Error::Busy));
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 1)]);
        clock.mark_read(SegmentId(1)).unwrap();
        segments.restore(segments.snapshot()).unwrap();
        clock.mark_read(SegmentId(0)).unwrap();
        assert_eq!(clock.hand.get(), 0);
        assert_eq!(*clock.recent.borrow(), HashSet::from([SegmentId(0)]));
        segments.restore(segments.snapshot()).unwrap();
        clock.reclaim(&entries, 0, 0, 0).unwrap();
        assert!(clock.recent.borrow().is_empty());
    }
}
