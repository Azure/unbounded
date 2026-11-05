//! Scored segment clock over caller-owned page mappings: a test driver that
//! reclaims through the public `Segments` API. Production evicts through
//! `page_alloc::disk_index::DiskIndex`; `alloc_dst.rs` keeps this clock as an
//! independent reclaim policy for its allocator lifecycle traces.
use page_alloc::{Error, Result, SegmentId, SegmentState, Segments};
use std::cell::{Cell, RefCell};
use std::rc::Rc;

/// Application-owned mappings; removal must compare current entries first.
/// Callbacks own metadata and version side effects and must never exceed budget.
pub trait SegmentEntries {
    /// Hard retention veto, including index-only removal and eviction in progress.
    fn can_reclaim(&self, _segment: SegmentId) -> bool {
        true
    }

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

/// One worker-local cursor for scored physical reclamation.
#[repr(align(64))]
pub struct SegmentClock {
    segments: Rc<Segments>,

    hand: Cell<usize>,

    restore_epoch: Cell<u64>,
}

impl SegmentClock {
    /// Attach bounded reclamation to one table without cloning its authority.
    pub fn new(segments: Rc<Segments>) -> Self {
        let epoch = segments.restore_epoch();
        Self {
            segments,
            hand: Cell::new(0),
            restore_epoch: Cell::new(epoch),
        }
    }

    /// Clear transient eviction history after successful table recovery.
    fn sync_restore(&self) {
        let epoch = self.segments.restore_epoch();
        if self.restore_epoch.replace(epoch) != epoch {
            self.hand.set(0);
        }
    }

    /// Advance one slot; callers only invoke this inside a nonempty bounded sweep.
    fn next(&self, count: usize) -> SegmentId {
        let hand = self.hand.get() % count;
        self.hand.set((hand + 1) % count);
        SegmentId(hand as u64)
    }

    /// Visit at most min(slot count, max_visits, 64) slots, then rank eligible
    /// candidates before entering eviction. Skipped slots count toward this limit.
    /// Scores are soft preferences, not pins. The callback must itself bound
    /// mapping inspection.
    /// Existing eviction work sorts first so partial removals always make progress.
    /// Logical heat is supplied by the caller, so recent disk completions do not
    /// override value ranking.
    /// For a nonzero reserve, success requires the reserve (capped at the slot
    /// count), but does not guarantee that pending evictions have drained. Busy
    /// means the bounded sweep could not meet the reserve or mutation was blocked.
    /// Use [`Segments::free_count`] to check capacity and retry when more is needed.
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
        if self.segments.free_count() >= target && self.segments.evicting_count() == 0 {
            return Ok(());
        }
        let mut candidates = Vec::new();
        for visit in 0..count.min(max_visits).min(64) {
            let id = self.next(count);
            let state = self.segments.state(id)?;
            if !matches!(state, SegmentState::Sealed | SegmentState::Evicting) {
                continue;
            }
            if !entries.can_reclaim(id) || (state == SegmentState::Sealed && !entries.can_evict(id))
            {
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
            if !entries.can_reclaim(id) {
                continue;
            }
            if state == SegmentState::Sealed
                && (self
                    .segments
                    .free_count()
                    .saturating_add(self.segments.evicting_count())
                    >= target
                    || !entries.can_evict(id))
            {
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
                Ok(()) | Err(Error::Busy) => {}
                Err(error) => return Err(error),
            }
        }
        if self.segments.free_count() >= target {
            Ok(())
        } else {
            Err(Error::Busy)
        }
    }
}

/// Bounded sweep state-space coverage, including invalid callbacks and recovery.
#[cfg(test)]
mod clock_tests {
    use super::*;
    use page_alloc::{Alignment, Generation};

    /// Deterministic index occupancy and removal-call observations.
    struct Entries {
        counts: RefCell<Vec<usize>>,

        calls: RefCell<Vec<(SegmentId, usize)>>,

        evictable: Cell<bool>,
        hard_veto: Cell<bool>,
    }

    impl Entries {
        /// Populate a synthetic index without changing allocator state.
        fn new(counts: Vec<usize>) -> Self {
            Self {
                counts: RefCell::new(counts),
                calls: RefCell::new(vec![]),
                evictable: Cell::new(true),
                hard_veto: Cell::new(false),
            }
        }
    }

    impl SegmentEntries for Entries {
        fn can_reclaim(&self, _: SegmentId) -> bool {
            !self.hard_veto.get()
        }
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

    /// Unscored reclamation through the scored clock.
    fn reclaim(
        clock: &SegmentClock,
        entries: &impl SegmentEntries,
        reserve: usize,
        visits: usize,
        budget: usize,
    ) -> Result<()> {
        clock.reclaim_scored(entries, reserve, visits, budget, |_| 0)
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
        assert_eq!(
            reclaim(&clock, &entries, 1, 64, 256),
            Err(Error::Unavailable)
        );
        assert!(entries.calls.borrow().is_empty());
        let segments = segments(1);
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments);
        let entries = Entries::new(vec![1]);
        assert_eq!(reclaim(&clock, &entries, 1, 0, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
    }

    #[test]
    fn hard_veto_applies_to_already_evicting_segments() {
        let segments = segments(2);
        drop(segments.append(1024).unwrap());
        segments.begin_evict(SegmentId(0)).unwrap();
        let entries = Entries::new(vec![1, 0]);
        entries.evictable.set(false);
        entries.hard_veto.set(true);
        let clock = SegmentClock::new(segments.clone());
        assert_eq!(
            clock.reclaim_scored(&entries, 2, 64, 256, |_| 0),
            Err(Error::Busy)
        );
        assert_eq!(reclaim(&clock, &entries, 2, 64, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        entries.evictable.set(true);
        entries.hard_veto.set(false);
        clock.reclaim_scored(&entries, 2, 64, 256, |_| 0).unwrap();
        assert_eq!(segments.free_count(), 2);
    }

    /// Mapping budget exhaustion leaves partial eviction for the next bounded call.
    #[test]
    fn mapping_budget_keeps_partial_eviction_until_next_sweep() {
        let segments = segments(2);
        drop(segments.append(1024).unwrap());
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![257, 0]);
        assert_eq!(reclaim(&clock, &entries, 2, 64, 256), Err(Error::Busy));
        assert_eq!(*entries.counts.borrow(), [1, 0]);
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 256)]);
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 1);
        reclaim(&clock, &entries, 2, 64, 256).unwrap();
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

            assert_eq!(reclaim(&clock, &entries, 1, 2, 1), Err(Error::Busy));
            assert_eq!(segments.snapshot(), before);
            assert_eq!(*entries.counts.borrow(), [mapping_count]);
            assert!(entries.calls.borrow().is_empty());
            assert_eq!(segments.free_count(), 0);

            entries.evictable.set(true);
            assert_eq!(reclaim(&clock, &entries, 1, 1, 1), Err(Error::Busy));
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
            assert_eq!(*entries.counts.borrow(), [mapping_count.saturating_sub(1)]);

            // Once eviction starts, a later veto must not block remaining mappings.
            entries.evictable.set(false);
            assert_eq!(reclaim(&clock, &entries, 1, 1, 1), Err(Error::Busy));
            assert_eq!(*entries.counts.borrow(), [0]);
            assert_eq!(entries.calls.borrow().len(), mapping_count);
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
            assert_eq!(segments.free_count(), 0);
            assert_eq!(segments.snapshot()[0].generation, Generation(1));

            // The live lease, not the veto, remains the physical reuse fence.
            drop(held);
            reclaim(&clock, &entries, 1, 1, 0).unwrap();
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Free));
            assert_eq!(segments.free_count(), 1);
            assert_eq!(segments.snapshot()[0].generation, Generation(2));
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
        assert_eq!(reclaim(&clock, &entries, 2, 64, 256), Err(Error::Busy));
        assert!(entries.calls.borrow().is_empty());
        drop(frozen);
        assert_eq!(reclaim(&clock, &entries, 2, 64, 256), Err(Error::Busy));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 1);
        drop(held);
        reclaim(&clock, &entries, 2, 64, 256).unwrap();
        assert_eq!(segments.free_count(), 2);
        assert!(segments.lease(SegmentId(0), Generation(1)).is_err());
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
                assert_eq!(reclaim(&clock, &entries, 1, 2, 0), Err(Error::Busy));
                assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
                drop(held.take());
            }
            reclaim(&clock, &entries, 1, 2, 0).unwrap();
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
        reclaim(&clock, &entries, 1, 2, 0).unwrap();
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
        assert_eq!(segments.state(SegmentId(1)), Ok(SegmentState::Free));
        assert_eq!(*entries.counts.borrow(), [1, 0]);
        assert!(entries.calls.borrow().is_empty());
        assert_eq!(reclaim(&clock, &entries, 2, 64, 0), Err(Error::Busy));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
    }

    /// A zero reserve does not inspect or change allocation and mapping state.
    #[test]
    fn zero_reserve_has_no_side_effects_even_when_unconfigured() {
        let unconfigured = SegmentClock::new(Rc::new(Segments::new(1024)));
        assert_eq!(
            reclaim(&unconfigured, &Entries::new(vec![]), 0, 64, 256),
            Ok(())
        );
        let segments = segments(1);
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![1]);
        assert_eq!(reclaim(&clock, &entries, 0, 64, 256), Ok(()));
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Sealed));
        assert!(entries.calls.borrow().is_empty());
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
            assert_eq!(segments.evicting_count(), 1);
        }
        drop(leases[0].take());
        clock.reclaim_scored(&entries, 1, 64, 256, |_| 0).unwrap();
        assert_eq!(segments.free_count(), 1);
        assert_eq!(segments.evicting_count(), 0);
        assert_eq!(*entries.counts.borrow(), [0, 1, 1]);
    }

    /// Meeting a smaller reserve does not promise to drain a leased pending victim.
    #[test]
    fn reserve_success_does_not_require_pending_evictions_to_drain() {
        {
            let segments = segments(3);
            let held = segments.append(1024).unwrap().0;
            drop(segments.append(1024).unwrap());
            let clock = SegmentClock::new(segments.clone());
            let entries = Entries::new(vec![1, 1, 0]);
            let reclaim =
                |reserve, visits| clock.reclaim_scored(&entries, reserve, visits, 3, |_| 0);
            assert_eq!(reclaim(3, 1), Err(Error::Busy));
            assert_eq!(segments.free_count(), 1);
            assert_eq!(segments.evicting_count(), 1);
            assert_eq!(reclaim(1, 0), Ok(()));
            assert_eq!(segments.evicting_count(), 1);
            assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
            assert_eq!(reclaim(3, 6), Err(Error::Busy));
            drop(held);
            assert_eq!(reclaim(3, 6), Ok(()));
            assert_eq!(segments.free_count(), 3);
            assert_eq!(segments.evicting_count(), 0);
        }
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
            reclaim(&clock, &InvalidEntries, 1, 1, 1),
            Err(Error::InvalidConfiguration)
        );
        assert_eq!(segments.state(SegmentId(0)), Ok(SegmentState::Evicting));
        assert_eq!(segments.free_count(), 0);
    }

    /// Every clock operation observes the restore epoch before using the old cursor.
    #[test]
    fn restore_resets_cursor_before_any_clock_operation() {
        let segments = segments(2);
        drop(segments.append(1024).unwrap());
        drop(segments.append(1024).unwrap());
        let clock = SegmentClock::new(segments.clone());
        let entries = Entries::new(vec![10, 10]);
        assert_eq!(reclaim(&clock, &entries, 2, 1, 1), Err(Error::Busy));
        assert_eq!(clock.hand.get(), 1);
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 1)]);
        segments.restore(segments.snapshot()).unwrap();
        entries.calls.borrow_mut().clear();
        assert_eq!(reclaim(&clock, &entries, 2, 1, 1), Err(Error::Busy));
        assert_eq!(*entries.calls.borrow(), [(SegmentId(0), 1)]);
    }
}

/// Payload-free entries used by the simulator.
#[cfg(test)]
mod phantom_tests {
    use super::*;
    use page_alloc::Alignment;

    /// Build a table where every slot is sealed.
    #[test]
    fn seal_open_seals_only_the_open_tail() {
        let segments = Segments::new(1024);
        segments
            .configure(2048, 2, Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        assert!(!segments.seal_open(), "nothing open yet");
        let (lease, _) = segments.append(512).unwrap();
        let id = lease.id();
        drop(lease);
        assert_eq!(segments.state(id).unwrap(), SegmentState::Open);
        assert!(segments.seal_open());
        assert_eq!(segments.state(id).unwrap(), SegmentState::Sealed);
        assert!(!segments.seal_open(), "already sealed");
        // The next append opens a fresh slot instead of the sealed tail.
        let (lease, _) = segments.append(512).unwrap();
        assert_ne!(lease.id(), id);
        assert_eq!(segments.free_count(), 0);
    }
}
