use std::collections::{HashMap, HashSet};

use super::*;
use crate::volume::{RETAIN_FOREVER, SLOT_BYTES, class_for};

const PAGE: u64 = UNIT_BYTES as u64;

fn key(n: u32) -> Key {
    Key::new([1; 8], [2; 12], n)
}

fn table(slots: u64) -> Slots {
    Slots::new(slots, Limits::UNLIMITED).unwrap()
}

fn value(slot: u32, unit: u16, class: u8, tier: Tier, retain_until: u64) -> Value {
    Value {
        slot,
        unit,
        class,
        tier,
        length: (u64::from(class_units(class).unwrap()) * PAGE).min(u64::from(u32::MAX)) as u32,
        flags: 0,
        generation: 1,
        retain_until,
    }
}

fn kind(slots: &Slots, slot: u32) -> SlotKind {
    slots.slot_state(slot).unwrap().kind
}

#[test]
fn new_checks_slot_count() {
    assert_eq!(
        Slots::new(MIN_SLOTS - 1, Limits::UNLIMITED).err(),
        Some(Error::InvalidConfiguration)
    );
    assert_eq!(
        Slots::new(MAX_SLOTS + 1, Limits::UNLIMITED).err(),
        Some(Error::InvalidConfiguration)
    );
    let slots = table(MIN_SLOTS);
    let usage = slots.usage();
    assert_eq!(usage.slot_count, 4);
    assert_eq!(usage.free_slots, 3);
    assert_eq!(usage.node_units, 2);
    assert_eq!(kind(&slots, META_SLOT), SlotKind::Node);
}

#[test]
fn per_slot_memory_is_small() {
    assert_eq!(std::mem::size_of::<Slot>(), 24);
    assert_eq!(std::mem::size_of::<(u16, Key)>(), 26);
}

#[test]
fn nodes_fill_meta_slot_then_node_slots() {
    let mut slots = table(4);
    let first = slots.alloc_node().unwrap();
    assert_eq!(first, NodeAddr::new(META_SLOT, META_FIRST_NODE_UNIT));
    for _ in 1..SLOT_UNITS - 2 {
        assert_eq!(slots.alloc_node().unwrap().slot(), META_SLOT);
    }
    let next = slots.alloc_node().unwrap();
    assert_eq!(next, NodeAddr::new(1, 0));
    assert_eq!(slots.usage().node_slots, 1);
    assert!(slots.slot_state(1).unwrap().open);
    // Freeing the only node of slot 1 returns it to free after two commits.
    slots.free_node(next, slots.epoch()).unwrap();
    let e = slots.begin_commit();
    slots.commit_succeeded(e);
    assert_eq!(kind(&slots, 1), SlotKind::Node);
    let e = slots.begin_commit();
    slots.commit_succeeded(e);
    assert_eq!(kind(&slots, 1), SlotKind::Free);
    assert_eq!(slots.usage().node_slots, 0);
    // A free unit in the meta slot is preferred again.
    slots.free_node(first, slots.epoch()).unwrap();
    for _ in 0..2 {
        let e = slots.begin_commit();
        slots.commit_succeeded(e);
    }
    assert_eq!(slots.alloc_node().unwrap(), first);
}

#[test]
fn free_node_checks_address() {
    let mut slots = table(4);
    assert_eq!(
        slots.free_node(NodeAddr::new(0, 0), 1),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(slots.free_node(NodeAddr::new(0, 5), 1), Err(Error::Stale));
    assert_eq!(slots.free_node(NodeAddr::new(2, 0), 1), Err(Error::Stale));
    assert_eq!(
        slots.free_node(NodeAddr::new(4, 0), 1),
        Err(Error::InvalidConfiguration)
    );
}

#[test]
fn reuse_rule_waits_for_two_successes_across_failures() {
    let mut slots = table(4);
    let node = slots.alloc_node().unwrap();
    // Commit 1 makes the node durable.
    let e1 = slots.begin_commit();
    slots.commit_succeeded(e1);
    // Freed after commit 1: superblock 1 still references it.
    slots.free_node(node, slots.epoch()).unwrap();
    assert_eq!(slots.usage().pending_units, 1);
    let e2 = slots.begin_commit();
    slots.commit_failed(e2);
    assert_eq!(slots.usage().pending_units, 1);
    let e3 = slots.begin_commit();
    slots.commit_succeeded(e3);
    // Superblock 1 is still on disk.
    assert_eq!(slots.usage().pending_units, 1);
    let e4 = slots.begin_commit();
    slots.commit_failed(e4);
    assert_eq!(slots.usage().pending_units, 1);
    let e5 = slots.begin_commit();
    slots.commit_succeeded(e5);
    assert_eq!(slots.usage().pending_units, 0);
    assert_eq!(slots.alloc_node().unwrap(), node);
}

#[test]
fn free_during_commit_waits_for_a_later_commit() {
    let mut slots = table(4);
    let node = slots.alloc_node().unwrap();
    let e1 = slots.begin_commit();
    // Freed while attempt 1 runs: tagged with the next attempt.
    slots.free_node(node, slots.epoch()).unwrap();
    slots.commit_succeeded(e1);
    let e2 = slots.begin_commit();
    slots.commit_succeeded(e2);
    assert_eq!(slots.usage().pending_units, 1);
    let e3 = slots.begin_commit();
    slots.commit_succeeded(e3);
    assert_eq!(slots.usage().pending_units, 0);
}

#[test]
fn small_records_share_the_open_slot() {
    let mut slots = table(8);
    let a = slots.allocate(Tier::Cache, 100).unwrap();
    let b = slots.allocate(Tier::Cache, 4096).unwrap();
    let c = slots.allocate(Tier::Cache, 5000).unwrap();
    assert_eq!((a.slot, a.unit, a.class), (1, 0, 0));
    assert_eq!((b.slot, b.unit), (1, 1));
    assert_eq!((c.slot, c.unit, c.class), (2, 0, 1));
    let s = slots.allocate(Tier::Storage, 100).unwrap();
    assert_eq!(s.slot, 3);
    let state = slots.slot_state(1).unwrap();
    assert!(state.open);
    assert_eq!(state.used, 2);
    assert_eq!(state.leases, 2);
    assert_eq!(slots.usage().small_bytes, 3 * PAGE + 2 * PAGE);
    drop((a, b, c, s));
    assert_eq!(slots.slot_state(1).unwrap().leases, 0);
}

#[test]
fn full_slots_seal_and_holes_are_reused() {
    let mut slots = table(8);
    let class = class_for(2048 * PAGE).unwrap();
    let a = slots.allocate(Tier::Storage, 2048 * PAGE).unwrap();
    let b = slots.allocate(Tier::Storage, 2048 * PAGE).unwrap();
    assert_eq!((a.slot, a.unit, b.slot, b.unit), (1, 0, 1, 2048));
    assert!(!slots.slot_state(1).unwrap().open);
    slots.commit_record(1, 0, key(1), 10).unwrap();
    slots.commit_record(1, 2048, key(2), 10).unwrap();
    let c = slots.allocate(Tier::Storage, 2048 * PAGE).unwrap();
    assert_eq!((c.slot, c.class), (2, class));
    assert!(slots.slot_state(2).unwrap().open);
    // Release a storage record in the sealed slot; the hole appears after
    // the reuse rule and is used once the open slot is full.
    slots.release_record(1, 2048, slots.epoch()).unwrap();
    for _ in 0..2 {
        let e = slots.begin_commit();
        slots.commit_succeeded(e);
    }
    let d = slots.allocate(Tier::Storage, 2048 * PAGE).unwrap();
    assert_eq!((d.slot, d.unit), (2, 2048));
    let e = slots.allocate(Tier::Storage, 2048 * PAGE).unwrap();
    assert_eq!((e.slot, e.unit), (1, 2048));
}

#[test]
fn whole_slot_records() {
    let mut slots = table(4);
    let a = slots.allocate(Tier::Cache, SLOT_BYTES).unwrap();
    assert_eq!((a.slot, a.unit, a.class), (1, 0, WHOLE_SLOT_CLASS));
    assert!(!slots.slot_state(1).unwrap().open);
    assert_eq!(slots.usage().small_bytes, 0);
    let b = slots.allocate(Tier::Cache, SLOT_BYTES).unwrap();
    assert_eq!(b.slot, 2);
    slots.commit_record(1, 0, key(7), 0).unwrap();
    assert_eq!(slots.commit_record(1, 0, key(7), 0), Err(Error::Stale));
    assert_eq!(slots.keys(1), vec![key(7)]);
    slots.abort(b).unwrap();
    assert_eq!(kind(&slots, 2), SlotKind::Free);
    slots.release_record(1, 0, slots.epoch()).unwrap();
    assert_eq!(kind(&slots, 1), SlotKind::Free);
    assert_eq!(slots.release_record(1, 0, 1), Err(Error::Stale));
    drop(a);
    assert_eq!(slots.usage().free_slots, 3);
}

#[test]
fn allocate_limits_and_errors() {
    let mut slots = Slots::new(
        5,
        Limits {
            storage_slots: 1,
            small_bytes: 3 * PAGE,
        },
    )
    .unwrap();
    assert_eq!(
        slots.allocate(Tier::Cache, 0).err(),
        Some(Error::InvalidConfiguration)
    );
    assert_eq!(
        slots.allocate(Tier::Cache, SLOT_BYTES + 1).err(),
        Some(Error::InvalidConfiguration)
    );
    let _s = slots.allocate(Tier::Storage, SLOT_BYTES).unwrap();
    assert_eq!(
        slots.allocate(Tier::Storage, SLOT_BYTES).err(),
        Some(Error::Busy)
    );
    let small = slots.allocate(Tier::Cache, 3 * PAGE).unwrap();
    assert_eq!(slots.allocate(Tier::Cache, 1).err(), Some(Error::Busy));
    slots.abort(small).unwrap();
    assert_eq!(slots.usage().small_bytes, 0);
    slots.set_limits(Limits::UNLIMITED);
    let _a = slots.allocate(Tier::Cache, SLOT_BYTES).unwrap();
    let _b = slots.allocate(Tier::Storage, SLOT_BYTES).unwrap();
    let _c = slots.allocate(Tier::Cache, SLOT_BYTES).unwrap();
    assert_eq!(slots.usage().free_slots, 0);
    assert_eq!(
        slots.allocate(Tier::Cache, SLOT_BYTES).err(),
        Some(Error::Busy)
    );
    assert_eq!(slots.alloc_node().unwrap().slot(), META_SLOT);
}

#[test]
fn record_calls_check_arguments() {
    let mut slots = table(4);
    let a = slots.allocate(Tier::Cache, 2 * PAGE).unwrap();
    assert_eq!(
        slots.commit_record(1, 1, key(1), 0),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(slots.commit_record(2, 0, key(1), 0), Err(Error::Stale));
    assert_eq!(
        slots.commit_record(9, 0, key(1), 0),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(slots.commit_record(1, 2, key(1), 0), Err(Error::Stale));
    assert_eq!(slots.release_record(1, 0, 1), Err(Error::Stale));
    slots.commit_record(1, 0, key(1), 0).unwrap();
    assert_eq!(
        slots.abort(Allocation {
            slot: 1,
            unit: 0,
            class: a.class,
            lease: slots.lease(1),
        }),
        Err(Error::Stale)
    );
    assert_eq!(slots.finish_eviction(1), Err(Error::Stale));
    assert_eq!(slots.cancel_eviction(1), Err(Error::Stale));
}

#[test]
fn cache_release_is_immediate_and_storage_is_deferred() {
    let mut slots = table(4);
    let c = slots.allocate(Tier::Cache, PAGE).unwrap();
    let s = slots.allocate(Tier::Storage, PAGE).unwrap();
    slots.commit_record(c.slot, c.unit, key(1), 0).unwrap();
    slots.commit_record(s.slot, s.unit, key(2), 0).unwrap();
    slots.release_record(c.slot, c.unit, slots.epoch()).unwrap();
    slots.release_record(s.slot, s.unit, slots.epoch()).unwrap();
    // The open cache slot became empty and is free.
    assert_eq!(kind(&slots, c.slot), SlotKind::Free);
    assert_eq!(slots.slot_state(s.slot).unwrap().used, 1);
    assert_eq!(slots.usage().pending_units, 1);
    for _ in 0..2 {
        let e = slots.begin_commit();
        slots.commit_succeeded(e);
    }
    assert_eq!(kind(&slots, s.slot), SlotKind::Free);
    assert_eq!(slots.usage().small_bytes, 0);
}

#[test]
fn clock_eviction() {
    let mut slots = table(6);
    let mut keep = Vec::new();
    for n in 0..3 {
        let a = slots.allocate(Tier::Cache, SLOT_BYTES).unwrap();
        slots.commit_record(a.slot, a.unit, key(n), 0).unwrap();
        keep.push(a);
    }
    // Leased slots are skipped.
    assert_eq!(slots.evict_candidate(0), None);
    keep.clear();
    for slot in 1..4 {
        slots.touch(slot);
    }
    // Every slot is referenced: the hand clears them all, then picks one.
    let e = slots.evict_candidate(0).unwrap();
    assert_eq!(e.keys, vec![key(e.slot - 1)]);
    for slot in 1..4 {
        assert!(!slots.slot_state(slot).unwrap().referenced);
    }
    assert!(slots.slot_state(e.slot).unwrap().evicting);
    assert_eq!(
        slots
            .evict_candidate(0)
            .map(|x| x.slot)
            .filter(|s| *s == e.slot),
        None
    );
    slots.cancel_eviction(e.slot).unwrap();
    // An open slot is skipped.
    let open = slots.allocate(Tier::Cache, PAGE).unwrap();
    slots
        .commit_record(open.slot, open.unit, key(9), 0)
        .unwrap();
    drop(open);
    let mut seen = HashSet::new();
    for slot in 1..4 {
        let _ = slots.cancel_eviction(slot);
    }
    while let Some(e) = slots.evict_candidate(0) {
        seen.insert(e.slot);
        slots.finish_eviction(e.slot).unwrap();
        assert_eq!(slots.slot_state(e.slot).unwrap().kind, SlotKind::Free);
    }
    assert_eq!(seen, HashSet::from([1, 2, 3]));
    assert_eq!(slots.usage().cache_slots, 1);
}

#[test]
fn storage_eviction_waits_for_retention() {
    let mut slots = table(4);
    let a = slots.allocate(Tier::Storage, SLOT_BYTES).unwrap();
    slots.commit_record(a.slot, a.unit, key(1), 100).unwrap();
    drop(a);
    assert_eq!(slots.evict_candidate(100), None);
    let e = slots.evict_candidate(101).unwrap();
    slots.cancel_eviction(e.slot).unwrap();
    slots.retain(1, 200);
    assert_eq!(slots.evict_candidate(150), None);
    slots.set_retain(1, 50);
    assert_eq!(slots.slot_state(1).unwrap().retain_until, 50);
    let e = slots.evict_candidate(150).unwrap();
    slots.finish_eviction(e.slot).unwrap();
    assert_eq!(slots.usage().storage_slots, 0);
}

#[test]
fn eviction_drops_pending_units() {
    let mut slots = table(4);
    let a = slots.allocate(Tier::Storage, PAGE).unwrap();
    let b = slots.allocate(Tier::Storage, PAGE).unwrap();
    slots.commit_record(1, a.unit, key(1), 0).unwrap();
    slots.commit_record(1, b.unit, key(2), 0).unwrap();
    drop((a, b));
    slots.release_record(1, 0, slots.epoch()).unwrap();
    // Fill the open slot so it is sealed: allocate the rest of the slot.
    let mut held = Vec::new();
    while slots.slot_state(1).unwrap().open {
        held.push(slots.allocate(Tier::Storage, PAGE).unwrap());
    }
    for allocation in held {
        slots.abort(allocation).unwrap();
    }
    let e = slots.evict_candidate(1).unwrap();
    assert_eq!(e.keys, vec![key(2)]);
    slots.finish_eviction(1).unwrap();
    let usage = slots.usage();
    assert_eq!((usage.pending_units, usage.small_bytes), (0, 0));
    for _ in 0..3 {
        let e = slots.begin_commit();
        slots.commit_succeeded(e);
    }
    assert_eq!(kind(&slots, 1), SlotKind::Free);
}

#[test]
fn evict_returns_none_when_nothing_qualifies() {
    let mut slots = table(5);
    assert_eq!(slots.evict_candidate(0), None);
    let a = slots.allocate(Tier::Storage, SLOT_BYTES).unwrap();
    slots.commit_record(1, 0, key(1), RETAIN_FOREVER).unwrap();
    drop(a);
    assert_eq!(slots.evict_candidate(u64::MAX), None);
}

#[test]
fn rebuild_resolves_conflicts() {
    let nodes = [
        NodeAddr::new(0, 2),
        NodeAddr::new(2, 5),
        NodeAddr::new(2, 5),
        NodeAddr::new(0, 0),
        NodeAddr::new(9, 0),
    ];
    let class4 = class_for(4 * PAGE).unwrap();
    let entries = vec![
        // Cache in slot 1 loses to a later storage entry in slot 1.
        (key(1), value(1, 0, 0, Tier::Cache, 0)),
        (key(2), value(1, 4, class4, Tier::Storage, 30)),
        (key(3), value(1, 8, class4, Tier::Storage, 20)),
        // Overlaps key 2.
        (key(4), value(1, 4, class4, Tier::Storage, 20)),
        // Node slot.
        (key(5), value(2, 0, 0, Tier::Storage, 0)),
        // Out of range and malformed.
        (key(6), value(9, 0, 0, Tier::Cache, 0)),
        (key(7), value(3, 1, class4, Tier::Cache, 0)),
        // Two cache entries with different classes: first wins.
        (key(8), value(3, 0, 0, Tier::Cache, 0)),
        (key(9), value(3, 0, 1, Tier::Cache, 0)),
        (key(10), value(4, 0, WHOLE_SLOT_CLASS, Tier::Cache, 0)),
    ];
    let (slots, dropped) = Slots::rebuild(6, Limits::UNLIMITED, nodes, entries).unwrap();
    let mut dropped: Vec<u32> = dropped.iter().map(|(k, _)| k.page()).collect();
    dropped.sort();
    assert_eq!(dropped, vec![1, 4, 5, 6, 7, 9]);
    assert_eq!(kind(&slots, 2), SlotKind::Node);
    let s1 = slots.slot_state(1).unwrap();
    assert_eq!(
        s1.kind,
        SlotKind::Data {
            tier: Tier::Storage,
            class: class4
        }
    );
    assert_eq!(
        (s1.used, s1.indexed, s1.retain_until, s1.open),
        (2, 2, 30, false)
    );
    assert_eq!(slots.slot_state(3).unwrap().indexed, 1);
    assert_eq!(slots.keys(4), vec![key(10)]);
    let usage = slots.usage();
    assert_eq!(
        (
            usage.free_slots,
            usage.node_slots,
            usage.cache_slots,
            usage.storage_slots
        ),
        (1, 1, 2, 1)
    );
    assert_eq!(usage.node_units, 4);
    assert_eq!(usage.small_bytes, 2 * 4 * PAGE + PAGE);
    assert_eq!(slots.slot_count(), 6);
    assert!(Slots::rebuild(3, Limits::UNLIMITED, [], []).is_err());
}

#[test]
fn rebuilt_partial_slots_are_reused() {
    let entries = vec![(key(1), value(1, 1, 0, Tier::Cache, 0))];
    let (mut slots, dropped) = Slots::rebuild(4, Limits::UNLIMITED, [], entries).unwrap();
    assert!(dropped.is_empty());
    let a = slots.allocate(Tier::Cache, PAGE).unwrap();
    assert_eq!((a.slot, a.unit), (1, 0));
    let b = slots.allocate(Tier::Cache, PAGE).unwrap();
    assert_eq!((b.slot, b.unit), (1, 2));
    let n = slots.alloc_node().unwrap();
    assert_eq!(n, NodeAddr::new(0, 2));
}

#[test]
fn pending_nodes_from_older_tree() {
    let (mut slots, _) = Slots::rebuild(
        5,
        Limits::UNLIMITED,
        [NodeAddr::new(0, 2)],
        [(key(1), value(1, 0, WHOLE_SLOT_CLASS, Tier::Cache, 0))],
    )
    .unwrap();
    let marked = slots.mark_pending_nodes(
        [
            NodeAddr::new(0, 2),
            NodeAddr::new(0, 3),
            NodeAddr::new(1, 0),
            NodeAddr::new(2, 7),
            NodeAddr::new(0, 1),
        ],
        slots.epoch(),
    );
    assert_eq!(marked, 2);
    assert_eq!(kind(&slots, 2), SlotKind::Node);
    assert_eq!(slots.usage().pending_units, 2);
    assert_eq!(slots.alloc_node().unwrap(), NodeAddr::new(0, 4));
    let e = slots.begin_commit();
    slots.commit_succeeded(e);
    assert_eq!(slots.usage().pending_units, 2);
    let e = slots.begin_commit();
    slots.commit_succeeded(e);
    assert_eq!(slots.usage().pending_units, 0);
    assert_eq!(kind(&slots, 2), SlotKind::Free);
    assert_eq!(slots.alloc_node().unwrap(), NodeAddr::new(0, 3));
}

#[test]
fn lease_is_static_and_counts() {
    fn is_static<T: 'static>(_: &T) {}
    let slots = table(4);
    let a = slots.lease(2);
    is_static(&a);
    let b = a.clone();
    assert_eq!(b.slot(), 2);
    assert_eq!(slots.slot_state(2).unwrap().leases, 2);
    drop(a);
    drop(b);
    assert_eq!(slots.slot_state(2).unwrap().leases, 0);
}

// Randomized model test.

struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }

    fn below(&mut self, n: u64) -> u64 {
        self.next() % n
    }
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum State {
    Reserved,
    Indexed(Key),
    Pending(u64),
}

#[derive(Clone, Copy, Debug)]
struct Item {
    tier: Tier,
    class: u8,
    length: u64,
    retain_until: u64,
    state: State,
}

#[derive(Default)]
struct Model {
    items: HashMap<(u32, u16), Item>,
    reservations: Vec<Allocation>,
    nodes: HashSet<NodeAddr>,
    pending_nodes: Vec<(u64, NodeAddr)>,
    last_success: u64,
}

const LENGTHS: [u64; 6] = [1, 4096, 3 * 4096 + 1, 64 * 4096, 2048 * 4096, SLOT_BYTES];

fn check(slots: &Slots, model: &Model) {
    let usage = slots.usage();
    assert_eq!(
        usage.free_slots + usage.node_slots + usage.cache_slots + usage.storage_slots + 1,
        usage.slot_count
    );
    let small: u64 = model.items.values().map(|i| record_bytes(i.class)).sum();
    assert_eq!(usage.small_bytes, small);
    let pending = model
        .items
        .values()
        .filter(|i| matches!(i.state, State::Pending(_)))
        .count()
        + model.pending_nodes.len();
    assert_eq!(usage.pending_units as usize, pending);
    assert_eq!(
        usage.node_units as usize,
        2 + model.nodes.len() + model.pending_nodes.len()
    );
    let mut used = vec![0u32; slots.slot_count() as usize];
    for &(slot, _) in model.items.keys() {
        used[slot as usize] += 1;
    }
    for addr in model
        .nodes
        .iter()
        .chain(model.pending_nodes.iter().map(|(_, a)| a))
    {
        used[addr.slot() as usize] += 1;
    }
    used[0] += 2;
    let mut free = 0;
    for slot in 0..slots.slot_count() as u32 {
        let state = slots.slot_state(slot).unwrap();
        assert_eq!(state.used, used[slot as usize], "slot {slot}");
        if state.kind == SlotKind::Free {
            free += 1;
        }
    }
    assert_eq!(free, usage.free_slots);
}

fn release_pending(model: &mut Model, epoch: u64) {
    let limit = model.last_success;
    model
        .items
        .retain(|_, i| !matches!(i.state, State::Pending(tag) if tag <= limit));
    model.pending_nodes.retain(|(tag, _)| *tag > limit);
    model.last_success = model.last_success.max(epoch);
}

fn step(slots: &mut Slots, model: &mut Model, rng: &mut Rng, next_key: &mut u32, now: u64) {
    match rng.below(12) {
        0..=2 => {
            let tier = if rng.below(2) == 0 {
                Tier::Cache
            } else {
                Tier::Storage
            };
            let length = LENGTHS[rng.below(LENGTHS.len() as u64) as usize];
            match slots.allocate(tier, length) {
                Ok(a) => {
                    let units = class_units(a.class).unwrap();
                    for (&(slot, unit), item) in &model.items {
                        if slot == a.slot {
                            let other = class_units(item.class).unwrap();
                            let disjoint = u32::from(a.unit) + units <= u32::from(unit)
                                || u32::from(unit) + other <= u32::from(a.unit);
                            assert!(disjoint, "overlap at {slot}:{unit}");
                        }
                    }
                    assert!(!model.nodes.iter().any(|n| n.slot() == a.slot));
                    model.items.insert(
                        (a.slot, a.unit),
                        Item {
                            tier,
                            class: a.class,
                            length,
                            retain_until: now + rng.below(50),
                            state: State::Reserved,
                        },
                    );
                    model.reservations.push(a);
                }
                Err(error) => assert_eq!(error, Error::Busy),
            }
        }
        3 | 4 => {
            if model.reservations.is_empty() {
                return;
            }
            let index = rng.below(model.reservations.len() as u64) as usize;
            let a = model.reservations.swap_remove(index);
            if rng.below(4) == 0 {
                model.items.remove(&(a.slot, a.unit));
                slots.abort(a).unwrap();
            } else {
                *next_key += 1;
                let key = key(*next_key);
                let item = model.items.get_mut(&(a.slot, a.unit)).unwrap();
                slots
                    .commit_record(a.slot, a.unit, key, item.retain_until)
                    .unwrap();
                item.state = State::Indexed(key);
            }
        }
        5 => {
            let indexed: Vec<(u32, u16)> = model
                .items
                .iter()
                .filter(|(_, i)| matches!(i.state, State::Indexed(_)))
                .map(|(k, _)| *k)
                .collect();
            if indexed.is_empty() {
                return;
            }
            let (slot, unit) = indexed[rng.below(indexed.len() as u64) as usize];
            let epoch = slots.epoch();
            slots.release_record(slot, unit, epoch).unwrap();
            let item = model.items.get_mut(&(slot, unit)).unwrap();
            if item.tier == Tier::Cache {
                model.items.remove(&(slot, unit));
            } else {
                item.state = State::Pending(epoch);
            }
        }
        6 => match slots.alloc_node() {
            Ok(addr) => {
                assert!(!model.items.keys().any(|(s, _)| *s == addr.slot()));
                assert!(model.nodes.insert(addr));
                assert!(!model.pending_nodes.iter().any(|(_, a)| *a == addr));
            }
            Err(error) => assert_eq!(error, Error::Busy),
        },
        7 => {
            let Some(&addr) = model.nodes.iter().min_by_key(|_| rng.next()) else {
                return;
            };
            model.nodes.remove(&addr);
            let epoch = slots.epoch();
            slots.free_node(addr, epoch).unwrap();
            model.pending_nodes.push((epoch, addr));
        }
        8 => {
            let epoch = slots.begin_commit();
            if rng.below(3) == 0 {
                slots.commit_failed(epoch);
            } else {
                slots.commit_succeeded(epoch);
                release_pending(model, epoch);
            }
        }
        9 | 10 => {
            if let Some(e) = slots.evict_candidate(now) {
                let mut expected: Vec<Key> = model
                    .items
                    .iter()
                    .filter(|((s, _), _)| *s == e.slot)
                    .filter_map(|(_, i)| match i.state {
                        State::Indexed(k) => Some(k),
                        _ => None,
                    })
                    .collect();
                let mut keys = e.keys.clone();
                keys.sort();
                expected.sort();
                assert_eq!(keys, expected);
                assert!(!model.reservations.iter().any(|a| a.slot == e.slot));
                if rng.below(4) == 0 {
                    slots.cancel_eviction(e.slot).unwrap();
                } else {
                    slots.finish_eviction(e.slot).unwrap();
                    model.items.retain(|(s, _), _| *s != e.slot);
                }
            }
        }
        _ => {
            let slot = rng.below(slots.slot_count()) as u32;
            slots.touch(slot);
        }
    }
}

#[test]
fn randomized_model() {
    for seed in 1..=8u64 {
        let mut rng = Rng(seed.wrapping_mul(0x9e37_79b9_7f4a_7c15));
        let mut slots = Slots::new(
            12,
            Limits {
                storage_slots: 6,
                small_bytes: 2048 * PAGE * 6,
            },
        )
        .unwrap();
        let mut model = Model::default();
        let mut next_key = 0;
        for round in 0..3000u64 {
            step(&mut slots, &mut model, &mut rng, &mut next_key, round / 10);
            check(&slots, &model);
        }
        // Drain reservations and pending space, then rebuild from live state.
        for a in std::mem::take(&mut model.reservations) {
            model.items.remove(&(a.slot, a.unit));
            slots.abort(a).unwrap();
        }
        for _ in 0..2 {
            let e = slots.begin_commit();
            slots.commit_succeeded(e);
            release_pending(&mut model, e);
        }
        check(&slots, &model);
        let entries: Vec<(Key, Value)> = model
            .items
            .iter()
            .map(|(&(slot, unit), i)| {
                let State::Indexed(key) = i.state else {
                    panic!("only indexed items remain");
                };
                let mut v = value(slot, unit, i.class, i.tier, i.retain_until);
                v.length = i.length as u32;
                (key, v)
            })
            .collect();
        let (rebuilt, dropped) =
            Slots::rebuild(12, slots.limits(), model.nodes.iter().copied(), entries).unwrap();
        assert!(dropped.is_empty());
        assert_eq!(rebuilt.usage(), slots.usage());
        for slot in 0..12 {
            let (a, b) = (
                slots.slot_state(slot).unwrap(),
                rebuilt.slot_state(slot).unwrap(),
            );
            assert_eq!((a.kind, a.used, a.indexed), (b.kind, b.used, b.indexed));
            let (mut ka, mut kb) = (slots.keys(slot), rebuilt.keys(slot));
            ka.sort();
            kb.sort();
            assert_eq!(ka, kb);
        }
    }
}
