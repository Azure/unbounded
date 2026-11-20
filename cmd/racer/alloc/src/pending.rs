//! Cancellation-safe publication over a shared [`DiskIndex`].
//!
//! [`PendingWrite`] owns a [`PageTicket`] and then the [`Writing`] token for
//! one record. The order reserve -> append -> write -> publish is enforced by
//! the tokens; dropping a `PendingWrite` (for example when a persist future
//! is canceled) cancels the reservation or reports the write failed, so no
//! in-flight or write count leaks.

use crate::disk_index::{
    CacheKey, DiskIndex, PageSlot, PageTicket, RecordLocation, Removed, Writing,
};
use crate::{Result, SegmentLease, Segments};
use std::cell::RefCell;
use std::rc::Rc;

#[derive(Debug)]
enum Stage {
    Reserved(PageTicket),
    Writing(Writing),
    Done,
}

/// One publication in progress. Callers must not hold a borrow of the index
/// across an await point, so the drop cleanup can always borrow it.
#[derive(Debug)]
pub struct PendingWrite {
    index: Rc<RefCell<DiskIndex>>,

    stage: Stage,
}

impl PendingWrite {
    /// Reserve `slot` for `cache`.
    pub fn reserve(
        index: &Rc<RefCell<DiskIndex>>,
        slot: PageSlot,
        cache: CacheKey,
    ) -> Result<Self> {
        let ticket = index.borrow_mut().reserve(slot, cache)?;
        Ok(Self::from_ticket(index.clone(), ticket))
    }

    /// Guard an already reserved ticket (for example from `reserve_replacing`).
    pub fn from_ticket(index: Rc<RefCell<DiskIndex>>, ticket: PageTicket) -> Self {
        Self {
            index,
            stage: Stage::Reserved(ticket),
        }
    }

    /// The reserved slot.
    pub fn slot(&self) -> Option<PageSlot> {
        match &self.stage {
            Stage::Reserved(ticket) => Some(ticket.slot()),
            Stage::Writing(writing) => Some(writing.slot()),
            Stage::Done => None,
        }
    }

    /// Reserve `length` bytes on disk. Keep the lease until the write
    /// completes. On error the reservation is kept and may be retried.
    pub fn append(
        &mut self,
        segments: &Segments,
        length: usize,
    ) -> Result<(SegmentLease, RecordLocation)> {
        self.append_in(segments, length, false)
    }

    /// [`Self::append`] into the dedicated written-record tail.
    pub fn append_written(
        &mut self,
        segments: &Segments,
        length: usize,
    ) -> Result<(SegmentLease, RecordLocation)> {
        self.append_in(segments, length, true)
    }

    fn append_in(
        &mut self,
        segments: &Segments,
        length: usize,
        written: bool,
    ) -> Result<(SegmentLease, RecordLocation)> {
        let Stage::Reserved(ticket) = std::mem::replace(&mut self.stage, Stage::Done) else {
            return Err(crate::Error::InvalidConfiguration);
        };
        let appended = if written {
            self.index
                .borrow_mut()
                .append_written(segments, ticket, length)
        } else {
            self.index.borrow_mut().append(segments, ticket, length)
        };
        match appended {
            Ok((lease, writing)) => {
                let location = writing.location();
                self.stage = Stage::Writing(writing);
                Ok((lease, location))
            }
            Err((ticket, error)) => {
                self.stage = Stage::Reserved(ticket);
                Err(error)
            }
        }
    }

    /// The write landed: publish it, returning the replaced record.
    pub fn publish(mut self) -> Result<Option<Removed>> {
        let Stage::Writing(writing) = std::mem::replace(&mut self.stage, Stage::Done) else {
            return Err(crate::Error::InvalidConfiguration);
        };
        let mut index = self.index.borrow_mut();
        let written = index.write_complete(writing);
        index.publish(written)
    }
}

impl Drop for PendingWrite {
    fn drop(&mut self) {
        match std::mem::replace(&mut self.stage, Stage::Done) {
            Stage::Reserved(ticket) => self.index.borrow_mut().cancel(ticket),
            Stage::Writing(writing) => self.index.borrow_mut().write_failed(writing),
            Stage::Done => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Alignment;
    use crate::disk_index::CacheIdx;
    use std::future::Future;
    use std::task::{Context, Waker};

    fn fixture() -> (Rc<RefCell<DiskIndex>>, Segments, CacheKey) {
        let segments = Segments::new(4096);
        segments
            .configure(4096 * 2, 2, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        let mut index = DiskIndex::new(4, 1, 2).unwrap();
        let cache = index.add_cache(CacheIdx(0)).unwrap();
        (Rc::new(RefCell::new(index)), segments, cache)
    }

    #[test]
    fn publish_follows_reserve_append_write() {
        let (index, segments, cache) = fixture();
        let mut pending = PendingWrite::reserve(&index, PageSlot(0), cache).unwrap();
        let (lease, location) = pending.append(&segments, 512).unwrap();
        assert!(!index.borrow().can_reclaim(location.segment()));
        drop(lease);
        assert_eq!(pending.publish().unwrap(), None);
        assert_eq!(index.borrow().len(), 1);
        assert!(!index.borrow().references(PageSlot(1)));
        index.borrow().check_invariants();
    }

    #[test]
    fn dropped_reservation_and_write_release_counts() {
        let (index, segments, cache) = fixture();
        drop(PendingWrite::reserve(&index, PageSlot(0), cache).unwrap());
        assert!(!index.borrow().references(PageSlot(0)));
        let mut pending = PendingWrite::reserve(&index, PageSlot(0), cache).unwrap();
        let (lease, location) = pending.append(&segments, 512).unwrap();
        drop(pending);
        drop(lease);
        assert!(!index.borrow().references(PageSlot(0)));
        assert!(index.borrow().can_reclaim(location.segment()));
        // Capacity 1 is free again.
        PendingWrite::reserve(&index, PageSlot(1), cache).unwrap();
        index.borrow().check_invariants();
    }

    #[test]
    fn canceled_persist_future_mid_write_leaks_nothing() {
        let (index, segments, cache) = fixture();
        let persist = {
            let index = index.clone();
            async move {
                let mut pending = PendingWrite::reserve(&index, PageSlot(0), cache)?;
                let (_lease, _location) = pending.append(&segments, 512)?;
                std::future::pending::<()>().await; // the disk write never completes
                pending.publish()
            }
        };
        let mut persist = Box::pin(persist);
        let mut context = Context::from_waker(Waker::noop());
        assert!(persist.as_mut().poll(&mut context).is_pending());
        assert!(index.borrow().is_inflight(PageSlot(0)));
        drop(persist);
        let index = index.borrow();
        assert!(!index.references(PageSlot(0)));
        assert_eq!(index.len(), 0);
        index.check_invariants();
    }
}
