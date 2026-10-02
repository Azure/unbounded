use crate::{Alignment, Error, Extent, Result};
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
#[derive(Clone, Debug)]
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
}
impl SegmentLease {
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
struct Slot {
    image: SegmentSnapshot,
    leases: Rc<Cell<usize>>,
}
pub struct Segments {
    segment_bytes: u64,
    capacity_bytes: Cell<u64>,
    alignment: Cell<Option<Alignment>>,
    slots: RefCell<Vec<Slot>>,
    frozen: Cell<bool>,
    open: Cell<Option<usize>>,
    free: RefCell<BTreeSet<usize>>,
}
impl Segments {
    pub fn new(segment_bytes: u64) -> Self {
        Self {
            segment_bytes,
            capacity_bytes: Cell::new(0),
            alignment: Cell::new(None),
            slots: RefCell::new(Vec::new()),
            frozen: Cell::new(false),
            open: Cell::new(None),
            free: RefCell::new(BTreeSet::new()),
        }
    }
    pub fn segment_bytes(&self) -> u64 {
        self.segment_bytes
    }
    pub fn capacity_bytes(&self) -> u64 {
        self.capacity_bytes.get()
    }
    pub fn configure(&self, capacity: u64, count: usize, alignment: Alignment) -> Result<()> {
        if !self.slots.borrow().is_empty()
            || self.segment_bytes == 0
            || capacity == 0
            || !capacity.is_multiple_of(self.segment_bytes)
            || !self.segment_bytes.is_multiple_of(alignment.offset())
            || !self.segment_bytes.is_multiple_of(alignment.length() as u64)
            || count == 0
            || count > 1_000_000
            || count as u64 > capacity / self.segment_bytes
        {
            return Err(Error::InvalidConfiguration);
        }
        self.capacity_bytes.set(capacity);
        self.alignment.set(Some(alignment));
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
    pub fn append(&self, length: usize) -> Result<(SegmentLease, Extent)> {
        if self.frozen.get() {
            return Err(Error::Busy);
        }
        let alignment = self.alignment.get().ok_or(Error::Unavailable)?;
        if length == 0
            || length as u64 > self.segment_bytes
            || alignment.extent(0, length)?.length() != length
        {
            return Err(Error::InvalidConfiguration);
        }
        let mut slots = self.slots.borrow_mut();
        if let Some(position) = self.open.get() {
            let slot = &mut slots[position];
            if self.segment_bytes - slot.image.used_bytes < length as u64 {
                slot.image.state = SegmentState::Sealed;
                self.open.set(None);
            }
        }
        let position = match self.open.get() {
            Some(position) => position,
            None => self.free.borrow_mut().pop_first().ok_or(Error::Busy)?,
        };
        self.open.set(Some(position));
        let slot = &mut slots[position];
        let offset = slot
            .image
            .id
            .0
            .checked_mul(self.segment_bytes)
            .and_then(|v| v.checked_add(slot.image.used_bytes))
            .ok_or(Error::InvalidConfiguration)?;
        let extent = alignment.extent(offset, length)?;
        let lease = Self::take_lease(slot)?;
        slot.image.state = SegmentState::Open;
        slot.image.used_bytes += length as u64;
        if slot.image.used_bytes == self.segment_bytes {
            slot.image.state = SegmentState::Sealed;
            self.open.set(None);
        }
        Ok((lease, extent))
    }
    fn take_lease(slot: &Slot) -> Result<SegmentLease> {
        slot.leases
            .set(slot.leases.get().checked_add(1).ok_or(Error::Busy)?);
        Ok(SegmentLease {
            id: slot.image.id,
            generation: slot.image.generation,
            count: slot.leases.clone(),
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
            return Err(Error::Corrupt);
        }
        Self::take_lease(slot)
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
            return Err(Error::Busy);
        }
        Ok(())
    }
    pub fn thaw(&self) {
        self.frozen.set(false);
    }
    pub fn validate_restore(&self, images: &[SegmentSnapshot]) -> Result<()> {
        let slots = self.slots.borrow();
        if images.len() != slots.len() || slots.iter().any(|s| s.leases.get() != 0) {
            return Err(Error::Corrupt);
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
                return Err(Error::Corrupt);
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
            || extent.offset() < start
            || end
                > start
                    .checked_add(slot.image.used_bytes)
                    .ok_or(Error::Corrupt)?
        {
            return Err(Error::Corrupt);
        }
        let alignment = self.alignment.get().ok_or(Error::Unavailable)?;
        if alignment.extent(extent.offset(), extent.length())? != *extent {
            return Err(Error::Corrupt);
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
            .get(Self::position(id)?)
            .map(|s| s.image.state)
            .ok_or(Error::Corrupt)
    }
}

#[cfg(test)]
mod tests;
