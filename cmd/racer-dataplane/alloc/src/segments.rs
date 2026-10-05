use crate::{Alignment, Error, Extent, MAX_SEGMENTS, Result, SegmentGeometry};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeSet,
    rc::Rc,
};

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
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SegmentSnapshot {
    pub id: SegmentId,
    pub generation: Generation,
    pub state: SegmentState,
    pub used_bytes: u64,
}

pub struct SegmentLease {
    id: SegmentId,
    generation: Generation,
    count: Rc<Cell<usize>>,
    table: Rc<()>,
    geometry: SegmentGeometry,
    start: u64,
    end: u64,
}
impl SegmentLease {
    pub fn id(&self) -> SegmentId {
        self.id
    }
    pub fn generation(&self) -> Generation {
        self.generation
    }
    pub(crate) fn table_identity(&self) -> Rc<()> {
        self.table.clone()
    }
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
    fn drop(&mut self) {
        self.count.set(self.count.get() - 1);
    }
}
struct Slot {
    image: SegmentSnapshot,
    leases: Rc<Cell<usize>>,
}
/// Holds a freeze independently of the table's lifetime. Dropping it thaws the table.
#[must_use = "keep the guard alive while the table must remain frozen"]
#[derive(Debug)]
pub struct FreezeGuard(Rc<Cell<bool>>);
impl Drop for FreezeGuard {
    fn drop(&mut self) {
        self.0.set(false);
    }
}
pub struct Segments {
    segment_bytes: u64,
    geometry: Cell<Option<SegmentGeometry>>,
    slots: RefCell<Vec<Slot>>,
    frozen: Rc<Cell<bool>>,
    table: Rc<()>,
    restore_epoch: Cell<u64>,
    open: Cell<Option<usize>>,
    free: RefCell<BTreeSet<usize>>,
}
impl Segments {
    /// Create an unconfigured table. Use configure or from_geometry before append.
    pub fn new(segment_bytes: u64) -> Self {
        Self {
            segment_bytes,
            geometry: Cell::new(None),
            slots: RefCell::new(Vec::new()),
            frozen: Rc::new(Cell::new(false)),
            table: Rc::new(()),
            restore_epoch: Cell::new(0),
            open: Cell::new(None),
            free: RefCell::new(BTreeSet::new()),
        }
    }
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }
    pub fn capacity_bytes(&self) -> u64 {
        self.geometry.get().map_or(0, SegmentGeometry::slab_bytes)
    }
    /// Build an entirely free table, rejecting geometry above the retained slot limit.
    pub fn from_geometry(geometry: SegmentGeometry) -> Result<Self> {
        let segments = Self::new(geometry.segment_bytes());
        segments.configure(
            geometry.slab_bytes(),
            usize::try_from(geometry.segment_count()).map_err(|_| Error::InvalidConfiguration)?,
            geometry.alignment(),
        )?;
        Ok(segments)
    }
    pub fn is_configured(&self) -> bool {
        self.geometry.get().is_some()
    }
    pub fn geometry(&self) -> Option<SegmentGeometry> {
        self.geometry.get()
    }
    pub(crate) fn table_identity(&self) -> Rc<()> {
        self.table.clone()
    }
    pub(crate) fn restore_epoch(&self) -> u64 {
        self.restore_epoch.get()
    }
    pub fn configure(&self, capacity: u64, count: usize, alignment: Alignment) -> Result<()> {
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
    fn position(id: SegmentId) -> Result<usize> {
        usize::try_from(id.0).map_err(|_| Error::Corrupt)
    }
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
    pub fn begin_evict(&self, id: SegmentId) -> Result<()> {
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
        slot.image.state = SegmentState::Evicting;
        Ok(())
    }
    pub fn recycle(&self, id: SegmentId) -> Result<()> {
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
        slot.image.used_bytes = 0;
        self.free.borrow_mut().insert(Self::position(id)?);
        Ok(())
    }
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
    pub fn validate_restore(&self, images: &[SegmentSnapshot]) -> Result<()> {
        self.validate_restore_epoch(images).map(|_| ())
    }
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
        self.restore_epoch
            .get()
            .checked_add(1)
            .ok_or(Error::Unavailable)
    }
    /// Validate the complete image before publishing it, sealing its open tail.
    /// Frozen tables and outstanding leases return Busy without changing state.
    pub fn restore(&self, images: Vec<SegmentSnapshot>) -> Result<()> {
        let epoch = self.validate_restore_epoch(&images)?;
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
        self.restore_epoch.set(epoch);
        Ok(())
    }
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
        if !Rc::ptr_eq(&self.table, &lease.table) {
            return Err(Error::Stale);
        }
        lease.validate_extent(extent)
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
            .get(Self::position(id)?)
            .map(|s| s.image.state)
            .ok_or(Error::Corrupt)
    }
}

#[cfg(test)]
mod tests;
