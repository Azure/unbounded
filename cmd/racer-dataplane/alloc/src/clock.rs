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
    restore_epoch: Cell<u64>,
}
impl SegmentClock {
    pub fn new(segments: Rc<Segments>) -> Self {
        let epoch = segments.restore_epoch();
        Self {
            segments,
            hand: Cell::new(0),
            recent: RefCell::new(HashSet::new()),
            restore_epoch: Cell::new(epoch),
        }
    }
    pub fn mark_read(&self, segment: SegmentId) -> Result<()> {
        self.sync_restore();
        if !matches!(
            self.segments.state(segment)?,
            SegmentState::Open | SegmentState::Sealed
        ) {
            // Reads accepted before eviction may complete after the state changed.
            return Ok(());
        }
        self.recent.borrow_mut().insert(segment);
        Ok(())
    }
    fn sync_restore(&self) {
        let epoch = self.segments.restore_epoch();
        if self.restore_epoch.replace(epoch) != epoch {
            self.hand.set(0);
            self.recent.borrow_mut().clear();
        }
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
    /// Reclaim a caller-selected reserve with bounded visits and mapping removals.
    /// Busy leases keep segments Evicting until a later sweep; no compaction occurs.
    /// A zero reserve is a no-op. A zero mapping budget can recycle empty segments
    /// but does not begin eviction of segments that still have mappings.
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
            if entries_left == 0 && !entries.is_empty(id) {
                continue;
            }
            self.segments.begin_evict(id)?;
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
        assert_eq!(segments.free_count(), 1);
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

    #[test]
    fn removal_contract_violations_return_errors_without_panicking() {
        struct InvalidEntries;
        impl SegmentEntries for InvalidEntries {
            fn remove_bounded(&self, _: SegmentId, budget: usize) -> usize {
                budget + 1
            }
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
