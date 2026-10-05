//! Bounded second-chance sweeps over lease-fenced segments.
use crate::{Error, Result, SegmentId, SegmentState, Segments};
use std::{
    cell::{Cell, RefCell},
    collections::HashSet,
    rc::Rc,
};

/// Application-owned segment mappings. Removal must compare current mappings
/// before forgetting them and return the number removed, no greater than budget.
/// Metadata, version ownership, and availability side effects stay in the caller.
pub trait SegmentEntries {
    fn remove_bounded(&self, segment: SegmentId, budget: usize) -> usize;
    fn is_empty(&self, segment: SegmentId) -> bool;
}

/// One shared cursor and recent-read set for index and physical reclamation.
pub struct SegmentClock {
    segments: Rc<Segments>,
    hand: Cell<usize>,
    recent: RefCell<HashSet<SegmentId>>,
}
impl SegmentClock {
    pub fn new(segments: Rc<Segments>) -> Self {
        Self {
            segments,
            hand: Cell::new(0),
            recent: RefCell::new(HashSet::new()),
        }
    }
    pub fn mark_read(&self, segment: SegmentId) -> Result<()> {
        if !matches!(
            self.segments.state(segment)?,
            SegmentState::Open | SegmentState::Sealed
        ) {
            return Err(Error::Corrupt);
        }
        self.recent.borrow_mut().insert(segment);
        Ok(())
    }
    fn next(&self, count: usize) -> SegmentId {
        let hand = self.hand.get() % count;
        self.hand.set((hand + 1) % count);
        SegmentId(hand as u64)
    }
    /// Forget one mapping per candidate until admission succeeds, without changing
    /// bytes or generations. At most two rotations, further capped by max_visits.
    pub fn reclaim_index(
        &self,
        entries: &impl SegmentEntries,
        max_visits: usize,
        mut ready: impl FnMut() -> bool,
    ) -> Result<()> {
        if ready() {
            return Ok(());
        }
        let count = self.segments.count();
        for _ in 0..count.saturating_mul(2).min(max_visits) {
            let id = self.next(count);
            if self.recent.borrow_mut().remove(&id) {
                continue;
            }
            entries.remove_bounded(id, 1);
            if ready() {
                return Ok(());
            }
        }
        Err(Error::Busy)
    }
    /// Reclaim a caller-selected reserve with bounded visits and mapping removals.
    /// Busy leases keep segments Evicting until a later sweep; no compaction occurs.
    pub fn reclaim(
        &self,
        entries: &impl SegmentEntries,
        free_reserve: usize,
        max_visits: usize,
        max_entries: usize,
    ) -> Result<()> {
        let count = self.segments.count();
        if count == 0 {
            return Err(Error::Unavailable);
        }
        let target = free_reserve.max(1).min(count);
        let mut free = self.segments.free_count();
        let mut entries_left = max_entries;
        for _ in 0..count.saturating_mul(2).min(max_visits) {
            if free >= target {
                return Ok(());
            }
            let id = self.next(count);
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
            let removed = entries.remove_bounded(id, entries_left);
            assert!(removed <= entries_left, "segment removal exceeded budget");
            entries_left -= removed;
            if !entries.is_empty(id) {
                return Err(Error::Busy);
            }
            match self.segments.recycle(id) {
                Ok(()) => free += 1,
                Err(Error::Busy) => {}
                Err(e) => return Err(e),
            }
        }
        if free >= target {
            Ok(())
        } else {
            Err(Error::Busy)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{Alignment, Generation};

    struct Entries {
        counts: RefCell<Vec<usize>>,
        calls: RefCell<Vec<(SegmentId, usize)>>,
    }
    impl Entries {
        fn new(counts: Vec<usize>) -> Self {
            Self {
                counts: RefCell::new(counts),
                calls: RefCell::new(vec![]),
            }
        }
    }
    impl SegmentEntries for Entries {
        fn remove_bounded(&self, id: SegmentId, budget: usize) -> usize {
            self.calls.borrow_mut().push((id, budget));
            let mut counts = self.counts.borrow_mut();
            let count = &mut counts[id.0 as usize];
            let removed = (*count).min(budget);
            *count -= removed;
            removed
        }
        fn is_empty(&self, id: SegmentId) -> bool {
            self.counts.borrow()[id.0 as usize] == 0
        }
    }
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
        assert_eq!(segments.free_count(), 0);
        clock.reclaim(&entries, 2, 64, 256).unwrap();
        assert_eq!(segments.free_count(), 2);
        assert_eq!(*entries.counts.borrow(), [0, 0]);
    }

    #[test]
    fn busy_lease_and_frozen_table_preserve_reclaim_side_effect_order() {
        let segments = segments(2);
        let held = segments.append(1024).unwrap();
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1, 1]);
        segments.freeze().unwrap();
        assert_eq!(clock.reclaim(&entries, 2, 64, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
        segments.thaw();
        clock.mark_read(SegmentId(1)).unwrap();
        assert_eq!(clock.reclaim(&entries, 2, 64, 256), Err(Error::Busy));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 1);
        assert_eq!(clock.mark_read(SegmentId(0)), Err(Error::Corrupt));
        assert_eq!(clock.mark_read(SegmentId(1)), Err(Error::Corrupt));
        drop(held);
        clock.reclaim(&entries, 2, 64, 256).unwrap();
        assert_eq!(segments.free_count(), 2);
        assert!(segments.lease(SegmentId(0), Generation(1)).is_err());
    }

    #[test]
    fn physical_sweep_preserves_recent_bit_on_ineligible_open_segment() {
        let segments = segments(1);
        drop(segments.append(512).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1]);
        clock.mark_read(SegmentId(0)).unwrap();
        assert_eq!(clock.reclaim(&entries, 0, 64, 256), Err(Error::Busy));
        drop(segments.append(512).unwrap());
        assert_eq!(clock.reclaim(&entries, 0, 1, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
        clock.reclaim(&entries, 0, 1, 256).unwrap();
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 256)]);
    }
}
