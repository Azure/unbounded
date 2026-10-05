use super::*;
use std::{num::NonZeroU32, task::Context};

#[derive(Clone, Debug)]
struct TestMember(String, NonZeroU32);
impl Member for TestMember {
    const DOMAIN: &'static str = "placement-tests";
    fn id(&self) -> &[u8] {
        self.0.as_bytes()
    }
    fn weight(&self) -> NonZeroU32 {
        self.1
    }
}
fn member(index: usize, weight: u32) -> TestMember {
    TestMember(format!("node-{index:06}"), NonZeroU32::new(weight).unwrap())
}
fn membership(count: usize) -> Membership<TestMember> {
    Membership::new((0..count).map(|i| member(i, 4)).collect()).unwrap()
}
fn key(value: u64) -> [u8; 8] {
    value.to_be_bytes()
}

#[test]
fn golden_slot_and_weighted_ranking_vectors() {
    let members = Membership::new(
        [1, 3, 6, 4]
            .into_iter()
            .enumerate()
            .map(|(i, w)| member(i, w))
            .collect(),
    )
    .unwrap();
    let placement = Placement::new(3);
    for (page, expected_slot, expected_order) in [
        (0, 325_512, [0, 2, 1]),
        (1, 733_552, [2, 1, 3]),
        (u64::MAX, 922_998, [0, 3, 2]),
    ] {
        assert_eq!(slot::<TestMember>(&key(page)), expected_slot);
        assert_eq!(
            placement.rank(&members, &key(page)).unwrap(),
            expected_order
        );
    }
    for (sample, cost) in [
        (0xe410_9581_2e88_5f6f, 715_971_622),
        (0xc425_1196_ce41_9070, 1_650_232_626),
        (0x9322_018f_0806_e768, 3_431_784_333),
        (0xb379_deab_a20d_903a, 2_200_536_977),
    ] {
        assert_eq!(exponential_cost(sample), cost);
    }
}

#[test]
fn integer_log_edges_and_ties() {
    assert_eq!(exponential_cost(0), 64 << 32);
    assert_eq!(exponential_cost(u64::MAX), 1);
    for exponent in 0..64 {
        assert_eq!(
            exponential_cost((1u64 << exponent) - 1),
            (64 - exponent) << 32
        );
    }
    let a = Score {
        node: 0,
        cost: 12,
        shares: 4,
    };
    let b = Score {
        node: 1,
        cost: 3,
        shares: 1,
    };
    assert!(a.compare(&b).is_lt());
    assert!(
        Score {
            cost: u64::MAX,
            shares: u32::MAX,
            ..a
        }
        .compare(&Score {
            cost: u64::MAX,
            shares: 1,
            ..b
        })
        .is_lt()
    );
}

#[test]
fn weighted_distribution_without_share_expansion() {
    let members = Membership::new(vec![member(0, 1), member(1, 3), member(2, 6)]).unwrap();
    let mut counts = [0usize; 3];
    for slot in 0..20_000 {
        let mut ranking = Ranking {
            cursor: 0,
            best: vec![],
            ..Ranking::default()
        };
        ranking.advance(&members, slot, usize::MAX);
        counts[ranking.best[0].node] += 1;
        assert_eq!(ranking.best.len(), 3);
    }
    for (actual, expected) in counts.into_iter().zip([2000, 6000, 12000]) {
        assert!(actual.abs_diff(expected) < 400, "{counts:?}");
    }
    let huge = Membership::new(vec![member(0, u32::MAX), member(1, 1)]).unwrap();
    assert_eq!(Placement::new(1).rank(&huge, &key(0)).unwrap()[0], 0);
}

#[test]
fn cooperative_coalescing_bounded_cache_and_cancellation() {
    let placement = Placement::new(1);
    let members = membership(1000);
    let mut first = Box::pin(placement.rank_async(&members, &key(0)));
    let mut second = Box::pin(placement.rank_async(&members, &key(0)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(first.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        placement
            .cache
            .borrow()
            .entries
            .values()
            .next()
            .unwrap()
            .borrow()
            .cursor,
        256
    );
    assert!(second.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        placement
            .cache
            .borrow()
            .entries
            .values()
            .next()
            .unwrap()
            .borrow()
            .cursor,
        512
    );
    assert_eq!(
        placement.rank(&members, &key(1)),
        Placement::new(0).rank(&members, &key(1))
    );
    assert_eq!(
        futures::executor::block_on(placement.rank_async(&members, &key(1))),
        Err(Error::Overloaded)
    );
    drop(first);
    let result = futures::executor::block_on(second).unwrap();
    assert_eq!(result, placement.rank(&members, &key(0)).unwrap());
    placement.rank(&members, &key(1)).unwrap();
    assert_eq!(placement.cache.borrow().entries.len(), 1);
    let uncached = Placement::new(0);
    uncached.rank(&members, &key(0)).unwrap();
    assert!(uncached.cache.borrow().entries.is_empty());
}

#[test]
fn generic_domain_isolates_identity_slot_and_cached_scores() {
    struct Other(TestMember);
    impl Member for Other {
        const DOMAIN: &'static str = "object-store";
        fn id(&self) -> &[u8] {
            self.0.id()
        }
        fn weight(&self) -> NonZeroU32 {
            self.0.weight()
        }
    }
    let a = membership(30);
    let b = Membership::new(a.members().iter().cloned().map(Other).collect()).unwrap();
    assert_ne!(a.identity(), b.identity());
    assert_ne!(
        Membership::<TestMember>::new(vec![]).unwrap().identity(),
        Membership::<Other>::new(vec![]).unwrap().identity()
    );
    assert_ne!(
        slot::<TestMember>(b"opaque\0key"),
        slot::<Other>(b"opaque\0key")
    );
    let placement = Placement::new(4);
    placement.rank(&a, b"opaque\0key").unwrap();
    assert_eq!(
        placement.rank(&b, b"opaque\0key").unwrap(),
        Placement::new(0).rank(&b, b"opaque\0key").unwrap()
    );
    assert_eq!(placement.cache.borrow().entries.len(), 2);
    for count in 0..=4 {
        assert_eq!(
            Placement::new(0)
                .rank(&membership(count), b"")
                .unwrap()
                .len(),
            count.min(3)
        );
    }
}

#[test]
fn predecessor_maintenance_and_churn_match_cold_oracle() {
    let placement = Placement::new(512);
    let oracle = Placement::new(0);
    let mut old = membership(80);
    for generation in 2..42 {
        for page in 0..40 {
            placement.rank(&old, &key(page)).unwrap();
        }
        let mut members = old.members().to_vec();
        members.remove(generation % members.len());
        members.push(member(100 + generation, 4));
        members[generation % 10].1 =
            NonZeroU32::new(u32::try_from(generation % 7 + 1).unwrap()).unwrap();
        let next = Membership::new(members).unwrap().with_predecessor(&old);
        for _ in 0..100 {
            placement.maintain(&next).unwrap();
        }
        for page in 0..40 {
            assert_eq!(
                placement.rank(&next, &key(page)).unwrap(),
                oracle.rank(&next, &key(page)).unwrap(),
                "generation {generation} page {page}"
            );
        }
        old = next;
    }
}

fn warm_to_idle(placement: &Placement, members: &Membership<TestMember>, limit: usize) {
    for _ in 0..limit {
        match placement.maintain(members).unwrap() {
            Maintenance::Idle => return,
            Maintenance::Progress => {}
            Maintenance::Blocked => panic!("unexpected pinned predecessor"),
        }
    }
    panic!("maintenance did not finish within bounded turns");
}

#[test]
fn full_cache_warm_migration_retains_every_populated_slot() {
    for cold in [false, true] {
        let placement = Placement::new(32);
        let old = membership(600);
        for page in 0..32 {
            placement.rank(&old, &key(page)).unwrap();
        }
        let slots: Vec<_> = placement
            .cache
            .borrow()
            .entries
            .keys()
            .map(|key| key.1)
            .collect();
        assert_eq!(slots.len(), 32);
        let mut values = old.members().to_vec();
        if cold {
            // Remove a retained winner: at least one slot must take cold fallback.
            let winner = placement.rank(&old, &key(0)).unwrap()[0];
            values.remove(winner);
        } else {
            values.push(member(9999, 4));
        }
        let next = Membership::new(values).unwrap().with_predecessor(&old);
        warm_to_idle(&placement, &next, 200);
        let cache = placement.cache.borrow();
        assert_eq!(cache.entries.len(), slots.len());
        let mut cold_slots = 0;
        for slot in slots {
            let ranking = cache
                .entries
                .get(&(next.identity(), slot))
                .expect("lost populated slot")
                .borrow();
            assert_eq!(ranking.cursor, next.members().len());
            let mut oracle = Ranking::default();
            oracle.advance(&next, slot, usize::MAX);
            assert_eq!(ranking.candidates(), oracle.candidates());
            if ranking.incremental {
                assert!(ranking.scored <= MAX_INCREMENTAL_CHANGES + REPLICAS);
            } else {
                cold_slots += 1;
                assert_eq!(ranking.scored, next.members().len());
            }
        }
        assert_eq!(cold_slots > 0, cold);
        assert!(!cache.entries.keys().any(|key| key.0 == old.identity()));
    }
}

#[test]
fn late_predecessor_admission_reopens_completed_scan_only_when_relevant() {
    let placement = Placement::new(8);
    let old = membership(20);
    let next = membership(21).with_predecessor(&old);
    placement.rank(&old, &key(0)).unwrap();
    warm_to_idle(&placement, &next, 10);
    let completed_cursor = placement.maintenance.borrow().cursor;
    assert!(completed_cursor.is_some());

    // Current-generation hits and new admissions must not invalidate the scan.
    for value in 0..3 {
        placement.rank(&next, &key(value)).unwrap();
        assert!(!placement.cache.borrow().predecessor_dirty);
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Idle));
        assert_eq!(placement.maintenance.borrow().cursor, completed_cursor);
    }
    // A valid old-generation request introduces a slot never warmed before.
    let late_slot = slot::<TestMember>(&key(3));
    placement.rank(&old, &key(3)).unwrap();
    assert!(placement.cache.borrow().predecessor_dirty);
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    warm_to_idle(&placement, &next, 10);
    let cache = placement.cache.borrow();
    assert!(!cache.entries.contains_key(&(old.identity(), late_slot)));
    let warmed = cache.entries[&(next.identity(), late_slot)].borrow();
    assert!(warmed.incremental);
    assert_eq!(
        warmed.candidates(),
        Placement::new(0).rank(&next, &key(3)).unwrap()
    );
    drop(warmed);
    drop(cache);
    let completed_cursor = placement.maintenance.borrow().cursor;
    for _ in 0..3 {
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Idle));
        assert_eq!(placement.maintenance.borrow().cursor, completed_cursor);
    }
}

#[test]
fn late_predecessor_behind_cursor_preserves_active_work_and_grace_pins() {
    let placement = Placement::new(4);
    let old = membership(1000);
    let mut keys = [key(0), key(1)];
    keys.sort_by_key(|key| slot::<TestMember>(key));
    let [late_key, active_key] = keys;
    let late_slot = slot::<TestMember>(&late_key);
    let active_slot = slot::<TestMember>(&active_key);
    assert!(late_slot < active_slot);
    let winner = placement.rank(&old, &active_key).unwrap()[0];
    let mut values = old.members().to_vec();
    values.remove(winner);
    let next = Membership::new(values).unwrap().with_predecessor(&old);
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    assert_eq!(
        placement.maintenance.borrow().cursor,
        Some((old.identity(), active_slot))
    );
    let active = placement.cache.borrow().entries[&(next.identity(), active_slot)].clone();
    assert_eq!(active.borrow().cursor, WORK_QUANTUM);

    let mut grace = Box::pin(placement.rank_async(&old, &late_key));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(grace.as_mut().poll(&mut cx).is_pending());
    assert!(placement.cache.borrow().predecessor_dirty);
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    assert!(placement.maintenance.borrow().cursor.is_none());
    assert_eq!(active.borrow().cursor, 2 * WORK_QUANTUM);
    assert!(Rc::ptr_eq(
        &active,
        &placement.maintenance.borrow().active.as_ref().unwrap().1
    ));
    for _ in 0..2 {
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    }
    assert_eq!(active.borrow().scored, next.members().len());
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
    assert_eq!(
        futures::executor::block_on(grace),
        Placement::new(0).rank(&old, &late_key)
    );
    warm_to_idle(&placement, &next, 10);
    let cache = placement.cache.borrow();
    assert!(!cache.entries.contains_key(&(old.identity(), late_slot)));
    assert_eq!(
        cache.entries[&(next.identity(), late_slot)]
            .borrow()
            .candidates(),
        Placement::new(0).rank(&next, &late_key).unwrap()
    );
}

#[test]
fn pinned_predecessor_blocks_without_losing_cursor_or_old_request() {
    let placement = Placement::new(1);
    let old = membership(1000);
    let mut pending = Box::pin(placement.rank_async(&old, &key(0)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(pending.as_mut().poll(&mut cx).is_pending());
    let mut values = old.members().to_vec();
    values.push(member(9999, 4));
    let next = Membership::new(values).unwrap().with_predecessor(&old);
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
    assert!(placement.maintenance.borrow().cursor.is_none());
    assert_eq!(
        placement.rank(&next, &key(0)),
        Placement::new(0).rank(&next, &key(0))
    );
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
    assert_eq!(
        futures::executor::block_on(pending),
        Placement::new(0).rank(&old, &key(0))
    );
    warm_to_idle(&placement, &next, 10);
    assert!(
        placement
            .cache
            .borrow()
            .entries
            .contains_key(&(next.identity(), slot::<TestMember>(&key(0))))
    );
}

#[test]
fn cold_maintenance_progress_is_pinned_and_coalesces_with_requests() {
    let placement = Placement::new(1);
    let old = membership(1000);
    let winner = placement.rank(&old, &key(0)).unwrap()[0];
    let mut values = old.members().to_vec();
    values.remove(winner);
    let next = Membership::new(values).unwrap().with_predecessor(&old);
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
    let cache_key = (next.identity(), slot::<TestMember>(&key(0)));
    assert_eq!(
        placement.cache.borrow().entries[&cache_key].borrow().cursor,
        WORK_QUANTUM
    );
    let mut pending = Box::pin(placement.rank_async(&next, &key(0)));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(pending.as_mut().poll(&mut cx).is_pending());
    assert_eq!(
        placement.cache.borrow().entries[&cache_key].borrow().cursor,
        2 * WORK_QUANTUM
    );
    assert_eq!(
        futures::executor::block_on(placement.rank_async(&next, &key(1))),
        Err(Error::Overloaded)
    );
    assert_eq!(
        placement.rank(&next, &key(1)),
        Placement::new(0).rank(&next, &key(1))
    );
    drop(pending);
    warm_to_idle(&placement, &next, 10);
    assert_eq!(
        placement.cache.borrow().entries[&cache_key].borrow().scored,
        next.members().len()
    );
}

#[test]
fn clock_rotation_inspects_more_than_64_busy_entries_and_refreshes_hits() {
    let placement = Placement::new(80);
    let members = membership(1);
    let mut pinned = Vec::new();
    for slot in 0..79 {
        pinned.push(placement.ranking(&members, slot).unwrap());
    }
    drop(placement.ranking(&members, 79).unwrap());
    drop(placement.ranking(&members, 80).unwrap());
    assert!(
        !placement
            .cache
            .borrow()
            .entries
            .contains_key(&(members.identity(), 79))
    );
    assert!(
        placement
            .cache
            .borrow()
            .entries
            .contains_key(&(members.identity(), 80))
    );
    assert_eq!(placement.cache.borrow().entries.len(), 80);
    drop(pinned);

    let placement = Placement::new(3);
    for slot in 0..3 {
        drop(placement.ranking(&members, slot).unwrap());
    }
    drop(placement.ranking(&members, 3).unwrap()); // clears references, evicts 0
    drop(placement.ranking(&members, 1).unwrap()); // refreshes reference on hit
    drop(placement.ranking(&members, 4).unwrap()); // second chance saves 1
    let cache = placement.cache.borrow();
    assert!(cache.entries.contains_key(&(members.identity(), 1)));
    assert!(!cache.entries.contains_key(&(members.identity(), 2)));
}

#[test]
fn scoring_uses_frozen_snapshots_not_live_member_accessors() {
    use std::cell::Cell;
    struct Mutable {
        id: Cell<&'static [u8]>,
        weight: Cell<NonZeroU32>,
    }
    impl Member for Mutable {
        const DOMAIN: &'static str = "mutable-placement";
        fn id(&self) -> &[u8] {
            self.id.get()
        }
        fn weight(&self) -> NonZeroU32 {
            self.weight.get()
        }
    }
    let members = Membership::new(
        (0..4)
            .map(|index| Mutable {
                id: Cell::new([b"a", b"b", b"c", b"d"][index].as_slice()),
                weight: Cell::new(NonZeroU32::new(u32::try_from(index).unwrap() + 1).unwrap()),
            })
            .collect(),
    )
    .unwrap();
    let expected: Vec<_> = (0..20)
        .map(|page| Placement::new(0).rank(&members, &key(page)).unwrap())
        .collect();
    for value in members.members() {
        value.id.set(b"changed");
        value.weight.set(NonZeroU32::new(u32::MAX).unwrap());
    }
    for (page, expected) in expected.into_iter().enumerate() {
        assert_eq!(
            Placement::new(0)
                .rank(&members, &key(u64::try_from(page).unwrap()))
                .unwrap(),
            expected
        );
    }
}

#[test]
fn randomized_incremental_diff_matches_full_sort_oracle() {
    let mut state = 0x1365_a739_2135_bcedu64;
    let mut draw = || {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        state
    };
    let mut fast = 0;
    let mut cold = 0;
    for _ in 0..120 {
        let mut before = Vec::new();
        let mut after = Vec::new();
        for index in 0..40 {
            let sample = draw();
            if sample % 3 != 0 {
                before.push(member(
                    index,
                    u32::try_from((sample >> 32) % 20).unwrap() + 1,
                ));
            }
            if sample % 5 != 0 {
                after.push(member(
                    index,
                    u32::try_from(((sample >> 16) & u64::from(u32::MAX)) % 20).unwrap() + 1,
                ));
            }
        }
        let old = Membership::new(before).unwrap();
        let next = Membership::new(after).unwrap().with_predecessor(&old);
        for slot in 0..16 {
            let mut previous = Ranking::default();
            previous.advance(&old, slot, usize::MAX);
            let mut updated = previous.updated(&next, slot).unwrap_or_default();
            if updated.incremental {
                fast += 1;
            } else {
                cold += 1;
            }
            updated.advance(&next, slot, usize::MAX);
            let mut scoring = Ranking::default();
            let mut all: Vec<_> = (0..next.members().len())
                .map(|index| scoring.score(&next, slot, index))
                .collect();
            all.sort_by(Score::compare);
            let expected: Vec<_> = all.iter().take(REPLICAS).map(|score| score.node).collect();
            assert_eq!(updated.candidates(), expected);
        }
    }
    assert!(fast > 0 && cold > 0, "fast={fast}, cold={cold}");
}

#[test]
fn exponential_is_monotonic_including_extrema_and_power_boundaries() {
    let mut samples = vec![0, 1, u64::MAX - 1, u64::MAX];
    for bit in 0..64 {
        let pivot = 1u64 << bit;
        samples.extend([pivot - 1, pivot, pivot.saturating_add(1)]);
    }
    for index in 0..20_000 {
        samples.push((u64::MAX / 20_000) * index);
    }
    samples.sort_unstable();
    for pair in samples.windows(2) {
        assert!(
            exponential_cost(pair[0]) >= exponential_cost(pair[1]),
            "{pair:?}"
        );
    }
    assert_eq!(exponential_cost(0), 64u64 << 32);
    assert_eq!(exponential_cost(u64::MAX), 1);
    assert_eq!(SLOT_COUNT, 1_048_576);
}

#[test]
fn entry_budget_covers_structures_and_dynamic_score_capacity() {
    use std::mem::size_of;
    let mut ranking = Ranking::default();
    ranking.advance(&membership(100), 0, usize::MAX);
    assert_eq!(ranking.best.capacity(), REPLICAS + 1);
    // Model a sparsely occupied BTreeMap leaf/internal node charged to one entry:
    // 11 key/value slots, 12 child pointers and generously padded node metadata.
    // These are explicit standard-library implementation assumptions, not an
    // allocator contract. CLOCK needs only one inline cursor, counted below as
    // a whole cache header per entry to cover the smallest cache as well.
    let map_node = 11 * size_of::<(CacheKey, Rc<RefCell<Ranking>>)>()
        + 12 * size_of::<usize>()
        + 4 * size_of::<usize>();
    let rc = 2 * size_of::<usize>() + size_of::<RefCell<Ranking>>();
    let dynamic_scores = ranking.best.capacity() * size_of::<Score>();
    let clock = size_of::<RankingCache>();
    assert!(map_node + rc + dynamic_scores + clock <= Placement::ENTRY_BYTES);
    let placement = Placement::new(1);
    let empty_bytes = placement.retained_bytes();
    placement.rank(&membership(4), b"key").unwrap();
    assert!(placement.retained_bytes() >= empty_bytes + Placement::ENTRY_BYTES);
}
