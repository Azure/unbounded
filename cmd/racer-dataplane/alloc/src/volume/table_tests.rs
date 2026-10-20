use super::*;
use crate::volume::format::SLOT_BYTES;

const SLOTS: u64 = 16;

fn table() -> Slots {
    Slots::new(SLOTS, Limits::UNLIMITED).unwrap()
}

#[test]
fn slot_entries_are_small() {
    assert_eq!(std::mem::size_of::<Slot>(), 24);
}

#[test]
fn new_table_has_meta_node_slot() {
    assert!(Slots::new(3, Limits::UNLIMITED).is_err());
    let mut slots = table();
    let meta = slots.slot_state(META_SLOT).unwrap();
    assert_eq!(
        (meta.kind, meta.used, meta.incarnation),
        (SlotKind::Node, 4, 1)
    );
    assert_eq!(slots.usage().free_slots, SLOTS - 1);
    let ops = slots.take_ops();
    assert_eq!(ops[&META_SLOT].ident, Some(Ident::Node(1)));
    assert!(slots.take_dirty_nodes().contains(&META_SLOT));
    assert_eq!(slots.next_incarnation(), 2);
}

#[test]
fn pages_are_contiguous_and_spill_into_new_node_slots() {
    let mut slots = table();
    slots.take_ops();
    slots.take_dirty_nodes();
    let pages = slots.alloc_pages(64);
    assert_eq!(pages[0], NodeAddr::new(META_SLOT, 4));
    assert!(pages.windows(2).all(|w| w[1].0 == w[0].0 + 1));
    // Fill the meta slot and spill.
    let more = slots.alloc_pages(SLOT_UNITS as usize);
    assert_eq!(more.len(), SLOT_UNITS as usize);
    let spilled: Vec<_> = more.iter().filter(|a| a.slot() != META_SLOT).collect();
    assert!(!spilled.is_empty());
    let node = spilled[0].slot();
    assert_eq!(spilled[0].unit(), 2);
    assert_eq!(slots.slot_state(node).unwrap().kind, SlotKind::Node);
    let ops = slots.take_ops();
    assert!(matches!(ops[&node].ident, Some(Ident::Node(_))));
    let dirty = slots.take_dirty_nodes();
    assert!(dirty.contains(&META_SLOT) && dirty.contains(&node));
    assert!(slots.page_used(*spilled[0]));
    // Freeing every page of the node slot frees the slot.
    for addr in spilled {
        assert!(slots.free_page(*addr));
    }
    assert_eq!(slots.slot_state(node).unwrap().kind, SlotKind::Free);
    assert_eq!(slots.take_ops()[&node].ident, Some(Ident::Free));
    assert!(!slots.free_page(NodeAddr::new(META_SLOT, 2)));
    assert!(slots.free_page(pages[0]));
    assert!(!slots.free_page(pages[0]));
    assert_eq!(slots.alloc_pages(1), vec![pages[0]]);
    assert_eq!(slots.used_bytes(0..1), u64::from(SLOT_UNITS) * 4096);
}

#[test]
fn records_fill_open_slot_then_reopen_with_a_loaded_bitmap() {
    let mut slots = table();
    let first = slots.allocate(Tier::Cache, 4096).unwrap();
    let slot = first.slot;
    assert!(slots.slot_state(slot).unwrap().open);
    let incarnation = first.incarnation;
    slots.published(first);
    let mut held = Vec::new();
    for _ in 1..class_records(0).unwrap() {
        let allocation = slots.allocate(Tier::Cache, 100).unwrap();
        assert_eq!(allocation.slot, slot);
        held.push(allocation);
    }
    let state = slots.slot_state(slot).unwrap();
    assert!(!state.open);
    assert_eq!(state.reserved, SLOT_UNITS - 1);
    // The next record opens another slot.
    let other = slots.allocate(Tier::Cache, 100).unwrap();
    assert_ne!(other.slot, slot);
    for allocation in held.drain(..) {
        slots.published(allocation);
    }
    // Free two records: the slot is partial but has no bitmap.
    assert!(slots.release(slot, 0, incarnation));
    assert!(slots.release(slot, 7, incarnation));
    assert!(!slots.release(slot, 8, incarnation + 50));
    assert_eq!(
        slots.partial_candidate(Tier::Cache, 100),
        None,
        "an open slot exists"
    );
    slots.abort(other);
    assert_eq!(
        slots.partial_candidate(Tier::Cache, 100),
        Some((slot, incarnation))
    );
    let used: Vec<u32> = (0..SLOT_UNITS).filter(|r| *r != 0 && *r != 7).collect();
    assert!(!slots.install_bitmap(slot, incarnation, &used[1..]));
    assert!(slots.install_bitmap(slot, incarnation, &used));
    let a = slots.allocate(Tier::Cache, 100).unwrap();
    let b = slots.allocate(Tier::Cache, 100).unwrap();
    assert_eq!((a.slot, a.unit, b.slot, b.unit), (slot, 0, slot, 7));
}

#[test]
fn limits_return_busy() {
    let mut slots = Slots::new(
        SLOTS,
        Limits {
            storage_slots: 1,
            small_bytes: 3 * 4096,
        },
    )
    .unwrap();
    let whole = slots.allocate(Tier::Storage, SLOT_BYTES).unwrap();
    assert_eq!(whole.class, WHOLE_SLOT_CLASS);
    assert!(slots.storage_quota_blocks(Tier::Storage, SLOT_BYTES));
    assert_eq!(
        slots.allocate(Tier::Storage, SLOT_BYTES).err(),
        Some(Error::Busy)
    );
    let a = slots.allocate(Tier::Cache, 4096).unwrap();
    let _b = slots.allocate(Tier::Cache, 8192).unwrap();
    assert_eq!(slots.allocate(Tier::Cache, 1).err(), Some(Error::Busy));
    slots.abort(a);
    assert!(slots.allocate(Tier::Cache, 1).is_ok());
    assert_eq!(
        slots.allocate(Tier::Cache, 0).err(),
        Some(Error::InvalidConfiguration)
    );
}

#[test]
fn eviction_follows_clock_leases_and_retention() {
    let mut slots = table();
    let mut taken = Vec::new();
    for (tier, retain) in [(Tier::Cache, 0), (Tier::Storage, 100), (Tier::Cache, 0)] {
        let allocation = slots.allocate(tier, SLOT_BYTES).unwrap();
        let slot = allocation.slot;
        slots.published(allocation);
        slots.retain(slot, retain);
        taken.push(slot);
    }
    slots.touch(taken[0]);
    let lease = slots.lease(taken[2]);
    // Slot 0 is referenced, slot 1 retained, slot 2 leased: the sweep
    // clears slot 0's bit and picks it on the second pass.
    let (slot, _) = slots.evict_candidate(50, None).unwrap();
    assert_eq!(slot, taken[0]);
    assert!(slots.slot_state(slot).unwrap().evicting);
    assert_eq!(slots.evict_candidate(50, None), None);
    slots.cancel_eviction(slot).unwrap();
    drop(lease);
    assert_eq!(slots.evict_candidate(50, Some(Tier::Storage)), None);
    let (slot, _) = slots.evict_candidate(101, Some(Tier::Storage)).unwrap();
    assert_eq!(slot, taken[1]);
    slots.take_ops();
    slots.finish_eviction(slot).unwrap();
    assert_eq!(slots.slot_state(slot).unwrap().kind, SlotKind::Free);
    assert_eq!(slots.take_ops()[&slot].ident, Some(Ident::Free));
    assert!(slots.finish_eviction(slot).is_err());
}

#[test]
fn idle_open_cache_slot_is_evicted_under_pressure() {
    let mut slots = table();
    let allocation = slots.allocate(Tier::Cache, 4096).unwrap();
    let slot = allocation.slot;
    assert_eq!(slots.evict_candidate(0, None), None, "reserved");
    slots.published(allocation);
    let (chosen, _) = slots.evict_candidate(0, None).unwrap();
    assert_eq!(chosen, slot);
    assert!(!slots.slot_state(slot).unwrap().open);
}

#[test]
fn retain_bounds_and_recompute() {
    let mut slots = table();
    let allocation = slots.allocate(Tier::Storage, 4096).unwrap();
    let (slot, incarnation) = (allocation.slot, allocation.incarnation);
    slots.published(allocation);
    slots.take_ops();
    slots.retain(slot, 50);
    slots.retain(slot, 10);
    assert_eq!(slots.slot_state(slot).unwrap().retain_until, 50);
    assert_eq!(slots.take_ops()[&slot].retain_max, Some(50));
    slots.retain_lowered(slot);
    assert_eq!(slots.stale_retain(8), vec![(slot, incarnation)]);
    // A raise during the recompute wins.
    slots.retain(slot, 60);
    slots.set_retain(slot, incarnation, 50, 5);
    assert_eq!(slots.slot_state(slot).unwrap().retain_until, 60);
    slots.set_retain(slot, incarnation, 60, 5);
    assert_eq!(slots.slot_state(slot).unwrap().retain_until, 5);
    assert!(slots.stale_retain(8).is_empty());
}

#[test]
fn slot_ops_merge_and_apply() {
    let data = Ident::Data {
        tier: Tier::Storage,
        class: 3,
        incarnation: 9,
    };
    let mut op = SlotOp {
        ident: Some(data),
        retain_max: Some(5),
        retain_set: None,
    };
    op.merge(&SlotOp {
        retain_max: Some(3),
        ..SlotOp::default()
    });
    assert_eq!(op.retain_max, Some(5));
    let summary = op.apply(None).unwrap();
    assert_eq!(
        (
            summary.kind,
            summary.class,
            summary.incarnation,
            summary.retain_until
        ),
        (SLOT_DATA, 3, 9, 5)
    );
    let raise = SlotOp {
        retain_max: Some(40),
        ..SlotOp::default()
    };
    assert_eq!(raise.apply(Some(summary)).unwrap().retain_until, 40);
    assert_eq!(raise.apply(None), None);
    let set = SlotOp {
        retain_set: Some(1),
        ..SlotOp::default()
    };
    assert_eq!(set.apply(Some(summary)).unwrap().retain_until, 1);
    op.merge(&SlotOp {
        ident: Some(Ident::Free),
        ..SlotOp::default()
    });
    assert_eq!(op.apply(Some(summary)), None);
}

#[test]
fn mount_restores_summaries_and_bitmaps() {
    let summaries = vec![
        (
            0,
            SlotSummary {
                kind: SLOT_NODE,
                incarnation: 1,
                bitmap_seq: 4,
                copy: 1,
                ..SlotSummary::default()
            },
        ),
        (
            3,
            SlotSummary {
                kind: SLOT_DATA,
                tier: 1,
                class: 0,
                incarnation: 7,
                used: 10,
                retain_until: 99,
                ..SlotSummary::default()
            },
        ),
        (
            5,
            SlotSummary {
                kind: SLOT_QUARANTINED,
                incarnation: 8,
                ..SlotSummary::default()
            },
        ),
    ];
    let mut slots = Slots::mount(SLOTS, Limits::UNLIMITED, summaries, 2).unwrap();
    assert_eq!(slots.next_incarnation(), 9);
    assert_eq!(slots.node_copy(0), Some((1, 4, 1)));
    // Unknown bitmap: the meta slot is full until supplied.
    assert_eq!(slots.slot_state(0).unwrap().used, SLOT_UNITS);
    let mut words = [0u64; BITMAP_WORDS];
    words[0] = 0b1_0000_0000;
    slots.set_node_bitmap(0, &words);
    assert_eq!(slots.slot_state(0).unwrap().used, 5);
    let state = slots.slot_state(3).unwrap();
    assert_eq!(
        (state.kind, state.used, state.retain_until),
        (
            SlotKind::Data {
                tier: Tier::Storage,
                class: 0
            },
            10,
            99
        )
    );
    assert_eq!(slots.slot_state(5).unwrap().kind, SlotKind::Quarantined);
    let usage = slots.usage();
    assert_eq!(
        (usage.free_slots, usage.quarantined_slots, usage.records),
        (SLOTS - 3, 1, 10)
    );
    // A closed partial slot is a reopen candidate.
    assert_eq!(slots.partial_candidate(Tier::Storage, 1), Some((3, 7)));
    slots.quarantine(3);
    assert_eq!(slots.slot_state(3).unwrap().kind, SlotKind::Quarantined);
    assert_eq!(slots.usage().free_slots, SLOTS - 3);
    assert!(matches!(
        slots.take_ops()[&3].ident,
        Some(Ident::Quarantined(_))
    ));
}
