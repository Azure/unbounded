//! Bounded append/seal/evict/reuse state with completion-owned generation leases.
use super::{
    direct::DirectAlignment,
    index::RecordLocation,
    slab::{SlabId, SlabLocation},
};
use crate::{
    error::{Error, Result},
    model::identity::WorkerId,
};
use std::{
    cell::{Cell, RefCell},
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
        for slot in slots.iter_mut() {
            if slot.image.state == SegmentState::Open
                && self.segment_bytes - slot.image.used_bytes < disk_bytes as u64
            {
                slot.image.state = SegmentState::Sealed;
            }
        }
        let position = slots
            .iter()
            .position(|s| s.image.state == SegmentState::Open)
            .or_else(|| {
                slots
                    .iter()
                    .position(|s| s.image.state == SegmentState::Free)
            })
            .ok_or(Error::Overloaded)?;
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
        for (slot, mut image) in slots.iter_mut().zip(images) {
            if image.state == SegmentState::Open {
                image.state = SegmentState::Sealed;
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
        self.slots
            .borrow()
            .iter()
            .filter(|s| s.image.state == SegmentState::Free)
            .count()
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
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn no_reuse_before_lease_drop_and_no_aba() {
        let s = Segments::new(WorkerId(0), 1024);
        s.configure(2048, 2, DirectAlignment::validate(512, 512, 512).unwrap())
            .unwrap();
        let a = s.append(1024).unwrap();
        s.begin_evict(a.segment.id).unwrap();
        assert!(s.recycle(a.segment.id).is_err());
        drop(a);
        s.recycle(SegmentId(0)).unwrap();
        assert!(s.lease(SegmentId(0), Generation(1)).is_err());
        assert_eq!(s.append(512).unwrap().segment.generation, Generation(2));
        s.freeze().unwrap();
        assert!(s.append(512).is_err());
        s.thaw();
    }
    #[test]
    fn restore_seals_open_and_rejects_invalid_geometry() {
        let s = Segments::new(WorkerId(0), 1024);
        s.configure(1024, 1, DirectAlignment::validate(512, 512, 512).unwrap())
            .unwrap();
        drop(s.append(512).unwrap());
        let mut snap = s.snapshot().unwrap();
        s.restore(snap.clone()).unwrap();
        assert_eq!(s.snapshot().unwrap()[0].state, SegmentState::Sealed);
        snap[0].used_bytes = 513;
        assert!(s.restore(snap).is_err());
    }
    #[test]
    fn generation_exhaustion_never_wraps_and_small_tails_are_sealed() {
        let s = Segments::new(WorkerId(0), 1024);
        s.configure(2048, 2, DirectAlignment::validate(512, 512, 512).unwrap())
            .unwrap();
        drop(s.append(512).unwrap());
        let next = s.append(1024).unwrap();
        assert_eq!(next.segment.id(), SegmentId(1));
        drop(next);
        assert_eq!(s.state(SegmentId(0)).unwrap(), SegmentState::Sealed);
        let mut snapshot = s.snapshot().unwrap();
        snapshot[0].generation = Generation(u64::MAX);
        s.restore(snapshot).unwrap();
        s.begin_evict(SegmentId(0)).unwrap();
        assert_eq!(s.recycle(SegmentId(0)), Err(Error::Unavailable));
        assert_eq!(s.snapshot().unwrap()[0].generation, Generation(u64::MAX));
    }
}
