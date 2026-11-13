//! Worker-local allocation authority, recovery images, and bounded reclamation.
//!
//! Slot reuse is metadata-only: it never erases, truncates, or compacts disk bytes.
//! Existing leases protect their captured used prefix even after eviction starts.
//! Freeze protects table mutation, not caller indexes or data-I/O completion.
use crate::{Alignment, Error, Extent, MAX_SEGMENTS, Result, SegmentGeometry};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeSet,
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
/// Any lease permits both reads and writes in that prefix; it is not exclusive
/// record ownership. The trusted caller must follow [`crate::Slab::write`]'s
/// ownership and publication rules.
/// A lease does not prove that bytes were initialized in this generation. Append
/// only reserves space; recycled bytes may belong to an earlier generation or a
/// different cache. The caller must check record integrity, authentication, and
/// cache identity before exposing bytes as a valid record.
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

    usage_group: usize,

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
pub struct FreezeGuard {
    frozen: Rc<Cell<bool>>,
    appends: Rc<Cell<bool>>,
}

impl FreezeGuard {
    /// Allow appends again while eviction, recycling, and restore stay frozen.
    /// Appends only reserve free slots or the unused tail of the open
    /// segment, so a snapshot taken before this call still describes every
    /// byte it maps: no mapped extent can be recycled and overwritten.
    pub fn thaw_appends(&self) {
        self.appends.set(false);
    }
}

impl Drop for FreezeGuard {
    /// Release mutation exclusion even if the table has already been dropped.
    fn drop(&mut self) {
        self.frozen.set(false);
        self.appends.set(false);
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

    /// Appends are blocked by a fresh freeze until the guard thaws them.
    appends_frozen: Rc<Cell<bool>>,

    table: TableIdentity,

    restore_epoch: Cell<u64>,

    open: Cell<Option<usize>>,

    /// Dedicated tail for written (write-back) records, with the generation it
    /// was taken at. Kept Sealed so checkpoint images stay valid and only
    /// [`Self::append_written`] grows it.
    written: Cell<Option<(usize, Generation)>>,

    free: RefCell<BTreeSet<usize>>,

    evicting: Cell<usize>,

    usage: RefCell<Vec<(u64, u64)>>,

    /// Bumped by every mutable borrow of `slots`; see [`Segments::generation`].
    generation: Cell<u64>,
}

impl Segments {
    /// Create an unconfigured table for Slab::open_configured to bind at startup.
    pub fn new(segment_bytes: u64) -> Self {
        Self {
            segment_bytes,
            geometry: Cell::new(None),
            slots: RefCell::new(Vec::new()),
            frozen: Rc::new(Cell::new(false)),
            appends_frozen: Rc::new(Cell::new(false)),
            table: TableIdentity::new(),
            restore_epoch: Cell::new(0),
            open: Cell::new(None),
            written: Cell::new(None),
            free: RefCell::new(BTreeSet::new()),
            evicting: Cell::new(0),
            usage: RefCell::new(Vec::new()),
            generation: Cell::new(0),
        }
    }

    /// Mutable slot table. Every caller counts as a change to
    /// [`Self::snapshot`], so checkpoint skipping cannot miss one.
    fn slots_mut(&self) -> std::cell::RefMut<'_, Vec<Slot>> {
        self.generation.set(self.generation.get().wrapping_add(1));
        self.slots.borrow_mut()
    }

    /// Mutation generation of the slot table: equal values mean
    /// [`Self::snapshot`] is unchanged.
    pub fn generation(&self) -> u64 {
        self.generation.get()
    }

    /// Fixed size of a physical segment.
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }

    /// Configured physical capacity, or zero before configuration.
    pub fn capacity_bytes(&self) -> u64 {
        self.geometry.get().map_or(0, SegmentGeometry::slab_bytes)
    }

    /// Group consecutive slots for storage metrics. Counts must cover every slot.
    /// Call at startup; regrouping scans the table once.
    pub fn configure_usage_groups(&self, counts: &[usize]) -> Result<()> {
        let mut slots = self.slots_mut();
        if !self.is_configured()
            || counts.is_empty()
            || counts.contains(&0)
            || counts.iter().try_fold(0usize, |n, c| n.checked_add(*c)) != Some(slots.len())
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut usage = Vec::with_capacity(counts.len());
        let mut start = 0;
        for (group, &count) in counts.iter().enumerate() {
            let mut used = 0;
            for slot in &mut slots[start..start + count] {
                slot.usage_group = group;
                used += slot.image.used_bytes;
            }
            usage.push((count as u64 * self.segment_bytes, used));
            start += count;
        }
        *self.usage.borrow_mut() = usage;
        Ok(())
    }

    /// Assigned capacity and reserved bytes per group, without scanning slots.
    pub fn usage(&self) -> Vec<(u64, u64)> {
        self.usage.borrow().clone()
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

    /// Recovery revision used to invalidate an eviction cursor.
    #[doc(hidden)]
    pub fn restore_epoch(&self) -> u64 {
        self.restore_epoch.get()
    }

    /// Install an entirely free bounded table exactly once, unless frozen.
    #[doc(hidden)]
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
        *self.slots_mut() = (0..count)
            .map(|id| Slot {
                usage_group: 0,
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
        *self.usage.borrow_mut() = vec![(count as u64 * self.segment_bytes, 0)];
        Ok(())
    }

    /// Reserve an aligned used range. Malformed requests and lease overflow leave
    /// state unchanged. A valid rollover without a free slot seals the open tail
    /// before returning Busy, allowing reclamation to make a retry possible.
    /// Reservation does not write or initialize disk bytes; see [`SegmentLease`].
    pub fn append(&self, length: usize) -> Result<(SegmentLease, Extent)> {
        if self.appends_frozen.get() {
            return Err(Error::Busy);
        }
        let alignment = self.geometry().ok_or(Error::Unavailable)?.alignment();
        if length == 0
            || length as u64 > self.segment_bytes
            || alignment.extent(0, length)?.length() != length
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut slots = self.slots_mut();
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
        self.usage.borrow_mut()[slot.usage_group].1 += length as u64;
        if slot.image.used_bytes == self.segment_bytes {
            slot.image.state = SegmentState::Sealed;
            self.open.set(None);
        }
        Ok((lease, extent))
    }

    /// Reserve an aligned range in the dedicated written-record tail. Written
    /// records never share a segment with ordinary cache records, so a segment
    /// pinned by live written data does not keep cache pages from eviction.
    /// The tail stays [`SegmentState::Sealed`]; the caller pins it against
    /// eviction (see `DiskIndex::pin_segment`). A tail that was evicted or
    /// filled is replaced by a free slot. `Busy` when no slot is free or
    /// appends are frozen.
    pub fn append_written(&self, length: usize) -> Result<(SegmentLease, Extent)> {
        if self.appends_frozen.get() {
            return Err(Error::Busy);
        }
        let alignment = self.geometry().ok_or(Error::Unavailable)?.alignment();
        if length == 0
            || length as u64 > self.segment_bytes
            || alignment.extent(0, length)?.length() != length
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut slots = self.slots_mut();
        let current = self.written.get().filter(|&(position, generation)| {
            let image = &slots[position].image;
            image.state == SegmentState::Sealed
                && image.generation == generation
                && self.segment_bytes - image.used_bytes >= length as u64
        });
        let (position, fresh) = match current {
            Some((position, _)) => (position, false),
            None => match self.free.borrow().first() {
                Some(&position) => (position, true),
                None => {
                    self.written.set(None);
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
        let lease = self.take_lease(slot, used_bytes)?;
        if fresh {
            self.free.borrow_mut().remove(&position);
        }
        let slot = &mut slots[position];
        slot.image.state = SegmentState::Sealed;
        slot.image.used_bytes = used_bytes;
        self.usage.borrow_mut()[slot.usage_group].1 += length as u64;
        self.written.set(Some((position, slot.image.generation)));
        Ok((lease, extent))
    }

    /// The current written segment, when the next [`Self::append_written`]
    /// of `length` bytes would land in it rather than open a fresh segment.
    pub fn written_fits(&self, length: usize) -> Option<SegmentId> {
        let (position, generation) = self.written.get()?;
        let slots = self.slots.borrow();
        let image = &slots.get(position)?.image;
        (image.state == SegmentState::Sealed
            && image.generation == generation
            && self.segment_bytes - image.used_bytes >= length as u64)
            .then_some(image.id)
    }

    /// Re-adopt a segment that holds durable written records after a restart,
    /// before any append can reuse it. A free slot at `generation` (or any
    /// older generation, as after a cold start) becomes Sealed at
    /// `generation`; an Open or Sealed slot already at `generation` keeps its
    /// state. Either way its used prefix grows to cover `used_bytes`.
    ///
    /// A non-free, unleased slot at an older generation was restored from a
    /// checkpoint taken before the segment was recycled and written again.
    /// The fsynced journal record proves the reuse, so the slot is superseded:
    /// it becomes Sealed at `generation` with only `used_bytes` in use, and
    /// `Ok(true)` tells the caller to drop index entries of the older
    /// generation. Any newer generation, or a leased older one, is `Stale`.
    pub fn adopt_written(
        &self,
        id: SegmentId,
        generation: Generation,
        used_bytes: u64,
    ) -> Result<bool> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let alignment = self.geometry().ok_or(Error::Unavailable)?.alignment();
        if generation.0 == 0
            || used_bytes == 0
            || used_bytes > self.segment_bytes
            || !used_bytes.is_multiple_of(alignment.offset())
            || !used_bytes.is_multiple_of(alignment.length() as u64)
        {
            return Err(Error::Corrupt);
        }
        let position = Self::position(id)?;
        let mut slots = self.slots_mut();
        let slot = slots.get_mut(position).ok_or(Error::Corrupt)?;
        let mut superseded = false;
        match slot.image.state {
            SegmentState::Free if slot.image.generation.0 <= generation.0 => {
                self.free.borrow_mut().remove(&position);
                slot.image.generation = generation;
                slot.image.state = SegmentState::Sealed;
            }
            SegmentState::Open | SegmentState::Sealed if slot.image.generation == generation => {}
            SegmentState::Open | SegmentState::Sealed | SegmentState::Evicting
                if slot.image.generation.0 < generation.0 && slot.leases.get() == 0 =>
            {
                if slot.image.state == SegmentState::Evicting {
                    self.evicting.set(self.evicting.get() - 1);
                }
                if self.open.get() == Some(position) {
                    self.open.set(None);
                }
                if self.written.get().is_some_and(|(open, _)| open == position) {
                    self.written.set(None);
                }
                self.usage.borrow_mut()[slot.usage_group].1 -= slot.image.used_bytes;
                slot.image.used_bytes = 0;
                slot.image.generation = generation;
                slot.image.state = SegmentState::Sealed;
                superseded = true;
            }
            _ => return Err(Error::Stale),
        }
        if slot.image.used_bytes < used_bytes {
            self.usage.borrow_mut()[slot.usage_group].1 += used_bytes - slot.image.used_bytes;
            slot.image.used_bytes = used_bytes;
        }
        Ok(superseded)
    }

    /// Seal the open segment, if any, so reclaim may evict it. Used when
    /// every reclaimable record lives in the open tail. Returns whether a
    /// segment was sealed.
    pub fn seal_open(&self) -> bool {
        let Some(position) = self.open.take() else {
            return false;
        };
        self.slots_mut()[position].image.state = SegmentState::Sealed;
        true
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

    /// Acquire the current used prefix only while the slot is Open or Sealed and
    /// the generation matches. This checks allocation state, not whether bytes
    /// were initialized in this generation or form valid records; see [`SegmentLease`].
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
    #[doc(hidden)]
    pub fn begin_evict(&self, id: SegmentId) -> Result<()> {
        self.begin_eviction(id)
    }

    /// Enter eviction before the clock invokes any caller index removal.
    fn begin_eviction(&self, id: SegmentId) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let mut slots = self.slots_mut();
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
    #[doc(hidden)]
    pub fn recycle(&self, id: SegmentId) -> Result<()> {
        self.recycle_evicted(id)
    }

    /// Complete clock-authorized eviction only after outstanding leases drain.
    fn recycle_evicted(&self, id: SegmentId) -> Result<()> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let mut slots = self.slots_mut();
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
        self.usage.borrow_mut()[slot.usage_group].1 -= slot.image.used_bytes;
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

    /// Block allocation, eviction, recycling, and restore until the guard drops
    /// (allocation until [`FreezeGuard::thaw_appends`]). Reads and snapshots
    /// remain available; only one guard may exist at a time.
    pub fn freeze(&self) -> Result<FreezeGuard> {
        if self.frozen.replace(true) {
            return Err(Error::Busy);
        }
        self.appends_frozen.set(true);
        Ok(FreezeGuard {
            frozen: self.frozen.clone(),
            appends: self.appends_frozen.clone(),
        })
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
        // Generations are monotonic: an image older than the live table could
        // revalidate mappings to bytes that were since reissued.
        if images
            .iter()
            .zip(slots.iter())
            .any(|(s, l)| s.generation.0 < l.image.generation.0)
        {
            return Err(Error::Stale);
        }
        for (s, l) in images.iter().zip(slots.iter()) {
            Self::restored_generation(s, &l.image)?;
        }
        self.restore_epoch
            .get()
            .checked_add(1)
            .ok_or(Error::Unavailable)
    }

    /// Generation published for one restored slot. Turning an occupied slot Free
    /// at its live generation advances it, so bytes are never reissued under a
    /// generation that earlier mappings carry; Unavailable if that would wrap.
    fn restored_generation(image: &SegmentSnapshot, live: &SegmentSnapshot) -> Result<Generation> {
        if image.state == SegmentState::Free
            && image.generation == live.generation
            && live.state != SegmentState::Free
        {
            return Ok(Generation(
                live.generation.0.checked_add(1).ok_or(Error::Unavailable)?,
            ));
        }
        Ok(image.generation)
    }

    /// Validate the complete image before publishing it, sealing its open tail.
    /// Images older than the live table are Stale.
    /// Frozen tables and outstanding leases return Busy without changing state.
    pub fn restore(&self, images: Vec<SegmentSnapshot>) -> Result<()> {
        let epoch = self.validate_restore_epoch(&images)?;
        let mut slots = self.slots_mut();
        self.open.set(None);
        self.written.set(None);
        self.free.borrow_mut().clear();
        self.evicting.set(0);
        let mut usage = self.usage.borrow_mut();
        for (_, used) in usage.iter_mut() {
            *used = 0;
        }
        for (slot, mut image) in slots.iter_mut().zip(images) {
            image.generation = Self::restored_generation(&image, &slot.image)?;
            if image.state == SegmentState::Open {
                image.state = SegmentState::Sealed;
            }
            if image.state == SegmentState::Free {
                self.free.borrow_mut().insert(image.id.0 as usize);
            }
            if image.state == SegmentState::Evicting {
                self.evicting.set(self.evicting.get() + 1);
            }
            usage[slot.usage_group].1 += image.used_bytes;
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

    /// Number of slots in eviction, waiting for leases to drain.
    pub fn evicting_count(&self) -> usize {
        self.evicting.get()
    }

    /// Whether eviction and recycling are frozen by a live [`FreezeGuard`],
    /// so no reclaim can add a free slot until the guard drops.
    pub fn reuse_frozen(&self) -> bool {
        self.frozen.get()
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

    #[test]
    fn written_tail_is_separate_sealed_and_replaced_when_full_or_evicted() {
        let s = segments(1024, 4);
        let (cache_lease, cache) = s.append(512).unwrap();
        let (w1, first) = s.append_written(512).unwrap();
        assert_ne!(cache_lease.id(), w1.id());
        assert_eq!(s.state(w1.id()).unwrap(), SegmentState::Sealed);
        let (w2, second) = s.append_written(512).unwrap();
        assert_eq!(w1.id(), w2.id());
        assert_eq!(second.offset(), first.offset() + 512);
        // The cache tail keeps appending to its own open segment.
        let (cache_lease2, cache2) = s.append(512).unwrap();
        assert_eq!(cache_lease2.id(), cache_lease.id());
        assert_eq!(cache2.offset(), cache.offset() + 512);
        // A full tail rolls to a fresh free slot.
        let (w3, _) = s.append_written(512).unwrap();
        assert_ne!(w3.id(), w1.id());
        let full = w1.id();
        let tail = w3.id();
        drop((w1, w2, w3, cache_lease, cache_lease2));
        // Checkpoint images with a written tail stay restorable.
        s.validate_restore(&s.snapshot()).unwrap();
        // An evicted tail is not appended to again.
        s.begin_evict(tail).unwrap();
        s.recycle(tail).unwrap();
        let (w4, _) = s.append_written(512).unwrap();
        assert_eq!(s.state(w4.id()).unwrap(), SegmentState::Sealed);
        assert_eq!(s.snapshot()[w4.id().0 as usize].used_bytes, 512);
        assert_eq!(s.state(full).unwrap(), SegmentState::Sealed);
        assert_eq!(
            s.append_written(0).err().unwrap(),
            Error::InvalidConfiguration
        );
        assert_eq!(
            s.append_written(2048).err().unwrap(),
            Error::InvalidConfiguration
        );
        let frozen = s.freeze().unwrap();
        assert_eq!(s.append_written(512).err().unwrap(), Error::Busy);
        drop(frozen);
    }

    #[test]
    fn written_tail_reports_busy_without_free_slots() {
        let s = segments(512, 2);
        let _a = s.append(512).unwrap();
        let _b = s.append_written(512).unwrap();
        assert_eq!(s.append_written(512).err().unwrap(), Error::Busy);
    }

    #[test]
    fn adopt_written_restores_free_slots_and_rejects_reused_generations() {
        let s = segments(1024, 3);
        // Cold start: every slot is free at generation 1.
        s.adopt_written(SegmentId(1), Generation(7), 1024).unwrap();
        assert_eq!(s.state(SegmentId(1)).unwrap(), SegmentState::Sealed);
        assert_eq!(s.free_count(), 2);
        s.validate(
            SegmentId(1),
            Generation(7),
            &Extent::new(1024 + 512, 512).unwrap(),
        )
        .unwrap();
        // Adopting again at the same generation only grows the used prefix.
        s.adopt_written(SegmentId(1), Generation(7), 512).unwrap();
        assert_eq!(s.snapshot()[1].used_bytes, 1024);
        // An older generation on a live slot was reused.
        assert_eq!(
            s.adopt_written(SegmentId(1), Generation(6), 512),
            Err(Error::Stale)
        );
        // A newer generation supersedes a checkpoint-restored live slot.
        assert!(s.adopt_written(SegmentId(1), Generation(8), 512).unwrap());
        assert_eq!(s.snapshot()[1].used_bytes, 512);
        assert_eq!(s.snapshot()[1].generation, Generation(8));
        assert!(s.lease(SegmentId(1), Generation(7)).is_err());
        assert!(!s.adopt_written(SegmentId(1), Generation(8), 1024).unwrap());
        // A leased slot is never superseded.
        let lease = s.lease(SegmentId(1), Generation(8)).unwrap();
        assert_eq!(
            s.adopt_written(SegmentId(1), Generation(9), 512),
            Err(Error::Stale)
        );
        drop(lease);
        let (lease, _) = s.append(512).unwrap();
        let open = lease.id();
        drop(lease);
        s.adopt_written(open, Generation(1), 1024).unwrap();
        assert_eq!(s.snapshot()[open.0 as usize].used_bytes, 1024);
        // A free slot newer than the record was recycled after it.
        s.seal_open();
        s.begin_evict(open).unwrap();
        s.recycle(open).unwrap();
        assert_eq!(s.adopt_written(open, Generation(1), 512), Err(Error::Stale));
        for bad in [0, 100, 2048] {
            assert_eq!(
                s.adopt_written(SegmentId(2), Generation(1), bad),
                Err(Error::Corrupt)
            );
        }
        assert_eq!(
            s.adopt_written(SegmentId(2), Generation(0), 512),
            Err(Error::Corrupt)
        );
        assert_eq!(
            s.adopt_written(SegmentId(9), Generation(1), 512),
            Err(Error::Corrupt)
        );
        let frozen = s.freeze().unwrap();
        assert_eq!(
            s.adopt_written(SegmentId(2), Generation(1), 512),
            Err(Error::Busy)
        );
        drop(frozen);
    }

    /// Lease ownership prevents reuse and old-generation acquisition.
    #[test]
    fn generation_moves_with_every_snapshot_change() {
        let s = segments(1024, 2);
        let start = s.generation();
        let snapshot = s.snapshot();
        let frozen = s.freeze().unwrap();
        drop(frozen);
        assert_eq!(s.generation(), start, "reads and freezes are not changes");
        let mut last = start;
        let mut changed = |s: &Segments, before: &[SegmentSnapshot]| {
            assert_ne!(s.snapshot(), before);
            assert_ne!(s.generation(), last);
            last = s.generation();
        };
        let lease = s.append(1024).unwrap();
        changed(&s, &snapshot);
        let before = s.snapshot();
        drop(lease);
        s.begin_evict(SegmentId(0)).unwrap();
        changed(&s, &before);
        let before = s.snapshot();
        s.recycle(SegmentId(0)).unwrap();
        changed(&s, &before);
    }

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

    #[test]
    fn disk_usage_tracks_reservations_recycle_and_restore() {
        let s = segments(1024, 3);
        assert_eq!(s.usage(), [(3072, 0)]);
        s.configure_usage_groups(&[1, 2]).unwrap();
        assert_eq!(s.usage(), [(1024, 0), (2048, 0)]);
        let (held, _) = s.append(1024).unwrap();
        drop(s.append(512).unwrap());
        assert_eq!(s.usage(), [(1024, 1024), (2048, 512)]);
        let image = s.snapshot();
        assert!(s.append(513).is_err());
        assert!(s.restore(image.clone()).is_err());
        for counts in [vec![], vec![0, 3], vec![2], vec![usize::MAX, 1]] {
            assert!(s.configure_usage_groups(&counts).is_err());
            assert_eq!(s.usage(), [(1024, 1024), (2048, 512)]);
        }
        s.begin_evict(SegmentId(0)).unwrap();
        assert!(s.recycle(SegmentId(0)).is_err());
        assert_eq!(s.usage(), [(1024, 1024), (2048, 512)]);
        drop(held);
        s.recycle(SegmentId(0)).unwrap();
        assert_eq!(s.usage(), [(1024, 0), (2048, 512)]);
        assert_eq!(s.restore(image.clone()), Err(Error::Stale));
        let mut image = image;
        // Restore never rolls a generation back, so carry the recycled one.
        image[0].generation = s.snapshot()[0].generation;
        s.restore(image.clone()).unwrap();
        assert_eq!(s.usage(), [(1024, 1024), (2048, 512)]);
        let mut invalid = image;
        invalid[0].used_bytes = 1025;
        assert!(s.restore(invalid).is_err());
        assert_eq!(s.usage(), [(1024, 1024), (2048, 512)]);
        s.configure_usage_groups(&[3]).unwrap();
        assert_eq!(s.usage(), [(3072, 1536)]);
        let mut fresh = segments(1024, 3).snapshot();
        for (f, live) in fresh.iter_mut().zip(s.snapshot()) {
            f.generation = live.generation;
        }
        s.restore(fresh).unwrap();
        assert_eq!(s.usage(), [(3072, 0)]);
    }

    #[test]
    fn disk_usage_counts_assigned_slots_not_unassigned_capacity() {
        let s = Segments::new(1024);
        assert!(s.usage().is_empty());
        assert!(s.configure_usage_groups(&[1]).is_err());
        s.configure(8192, 2, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        assert_eq!(s.capacity_bytes(), 8192);
        assert_eq!(s.usage(), [(2048, 0)]);
        s.configure_usage_groups(&[1, 1]).unwrap();
        drop(s.append(1024).unwrap());
        drop(s.append(1024).unwrap());
        assert!(s.append(512).is_err());
        assert_eq!(s.usage(), [(1024, 1024), (1024, 1024)]);
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

    /// Thawed appends reserve space while reuse stays frozen until the drop.
    #[test]
    fn freeze_guard_thaws_appends_but_not_reuse() {
        let s = segments(1024, 2);
        let frozen = s.freeze().unwrap();
        assert!(matches!(s.append(512), Err(Error::Busy)));
        frozen.thaw_appends();
        let (lease, _) = s.append(512).unwrap();
        drop(lease);
        assert!(matches!(s.freeze(), Err(Error::Busy)));
        assert_eq!(s.restore(s.snapshot()), Err(Error::Busy));
        assert_eq!(s.begin_evict(SegmentId(0)), Err(Error::Busy));
        assert_eq!(s.recycle(SegmentId(0)), Err(Error::Busy));
        drop(frozen);
        drop(s.append(512).unwrap());
        // A new freeze blocks appends again.
        let frozen = s.freeze().unwrap();
        assert!(matches!(s.append(512), Err(Error::Busy)));
        drop(frozen);
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
        let mut images = before.clone();
        images[1].used_bytes = 513;
        cases.push(images);
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
