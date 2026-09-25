//! Per-worker bounded second-chance clock, with no payload compaction.
use super::{
    index::Index,
    segment::{SegmentId, SegmentState, Segments},
};
use crate::error::{Error, Operation, Result};
use std::{
    cell::{Cell, RefCell},
    collections::HashSet,
    rc::Rc,
};
pub struct SegmentClock {
    index: Rc<Index>,
    segments: Rc<Segments>,
    free_reserve: usize,
    hand: Cell<usize>,
    recent: RefCell<HashSet<SegmentId>>,
}
impl SegmentClock {
    pub fn new(index: Rc<Index>, segments: Rc<Segments>, free_reserve: usize) -> Self {
        Self {
            index,
            segments,
            free_reserve,
            hand: Cell::new(0),
            recent: RefCell::new(HashSet::new()),
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
    /// At most two rotations. Busy segments remain Evicting until a later poll.
    pub fn reclaim_now(&self) -> Result<()> {
        let count = self.segments.count();
        if count == 0 {
            return Err(Error::Unavailable);
        }
        let target = self.free_reserve.max(1).min(count);
        let mut free = self.segments.free_count();
        for _ in 0..count.saturating_mul(2) {
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
            for (page, location) in self.index.segment_entries(id) {
                self.index.remove_if_matches(&page, &location)?;
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
mod tests {
    use super::*;
    use crate::{model::identity::WorkerId, store::direct::DirectAlignment};
    #[test]
    fn busy_victim_waits_and_clock_makes_progress() {
        let segments = Rc::new(Segments::new(WorkerId(0), 512));
        segments
            .configure(1024, 2, DirectAlignment::validate(512, 512, 512).unwrap())
            .unwrap();
        let held = segments.append(512).unwrap();
        drop(segments.append(512).unwrap());
        let clock = SegmentClock::new(Rc::new(Index::new(WorkerId(0), 1)), segments.clone(), 2);
        clock.mark_read(SegmentId(1)).unwrap();
        assert_eq!(clock.reclaim_now(), Err(Error::Overloaded));
        assert_eq!(segments.free_count(), 1);
        drop(held);
        clock.reclaim_now().unwrap();
        assert_eq!(segments.free_count(), 2);
    }
}
