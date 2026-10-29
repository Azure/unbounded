use std::collections::{BTreeMap, HashMap, HashSet};
use std::ops::Bound;

use super::*;
use crate::volume::format::Tier;

const VOLUME: [u8; 16] = [9; 16];
const SLOTS: u64 = 1 << 20;

struct Rng(u64);

impl Rng {
    fn new(seed: u64) -> Self {
        Self(seed.wrapping_mul(0x9e37_79b9_7f4a_7c15) | 1)
    }

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

fn seeds() -> Vec<u64> {
    match std::env::var("VOLUME_TREE_SEED") {
        Ok(seed) => vec![seed.parse().expect("VOLUME_TREE_SEED must be a u64")],
        Err(_) => vec![1, 2, 3],
    }
}

fn key(n: u32) -> Key {
    Key::new([(n >> 10) as u8; 8], [(n % 3) as u8; 12], n)
}

fn val(g: u64) -> Value {
    Value {
        slot: 1 + (g % 100) as u32,
        unit: 0,
        class: 0,
        tier: if g.is_multiple_of(2) {
            Tier::Cache
        } else {
            Tier::Storage
        },
        length: 1 + (g % 4096) as u32,
        flags: 0,
        generation: g + 1,
        retain_until: g,
    }
}

type Model = BTreeMap<Key, Value>;

fn same(tree: &Tree, model: &Model) {
    let got: Vec<_> = tree.range(Bound::Unbounded, Bound::Unbounded).collect();
    let want: Vec<_> = model.iter().map(|(k, v)| (*k, *v)).collect();
    assert_eq!(got.len(), want.len());
    assert!(got == want, "tree differs from model");
    assert_eq!(tree.len(), model.len() as u64);
}

fn bound(rng: &mut Rng, space: u32) -> Bound<Key> {
    let k = key(rng.below(u64::from(space)) as u32);
    match rng.below(3) {
        0 => Bound::Unbounded,
        1 => Bound::Included(k),
        _ => Bound::Excluded(k),
    }
}

/// One random mutation or query, checked against the model.
fn step(tree: &mut Tree, model: &mut Model, rng: &mut Rng, space: u32) {
    let k = key(rng.below(u64::from(space)) as u32);
    match rng.below(20) {
        0..=8 => {
            let v = val(rng.next() >> 1);
            assert_eq!(tree.insert(k, v), model.insert(k, v));
        }
        9..=14 => assert_eq!(tree.remove(&k), model.remove(&k)),
        15..=16 => {
            let g = rng.next() >> 1;
            let found = tree.update(&k, |v| v.generation = g + 1);
            assert_eq!(
                found,
                model.get_mut(&k).map(|v| v.generation = g + 1).is_some()
            );
            assert_eq!(tree.get(&k), model.get(&k).copied());
        }
        17..=18 => {
            let (low, high) = (bound(rng, space), bound(rng, space));
            let valid = match (low, high) {
                (
                    Bound::Included(a) | Bound::Excluded(a),
                    Bound::Included(b) | Bound::Excluded(b),
                ) => {
                    a < b
                        || (a == b
                            && matches!((low, high), (Bound::Included(_), Bound::Included(_))))
                }
                _ => true,
            };
            let got: Vec<_> = tree.range(low, high).collect();
            if valid {
                let want: Vec<_> = model.range((low, high)).map(|(k, v)| (*k, *v)).collect();
                assert_eq!(got, want);
            } else {
                assert!(got.is_empty());
            }
        }
        _ => {
            let after = (rng.below(4) != 0).then_some(k);
            let limit = rng.below(200) as usize;
            let got = tree.next_after(after.as_ref(), limit);
            let low = after.map_or(Bound::Unbounded, Bound::Excluded);
            let want: Vec<_> = model
                .range((low, Bound::Unbounded))
                .take(limit)
                .map(|(k, v)| (*k, *v))
                .collect();
            assert_eq!(got, want);
        }
    }
}

#[test]
fn matches_model() {
    for seed in seeds() {
        let mut rng = Rng::new(seed);
        let mut tree = Tree::new(VOLUME);
        let mut model = Model::new();
        for space in [300, 5_000, 40_000] {
            for i in 0..40_000 {
                step(&mut tree, &mut model, &mut rng, space);
                if i % 500 == 0 {
                    tree.check(true);
                }
            }
            tree.check(true);
            same(&tree, &model);
        }
        // Drain to empty.
        let keys: Vec<_> = model.keys().copied().collect();
        for k in keys {
            assert_eq!(tree.remove(&k), model.remove(&k));
        }
        tree.check(true);
        assert!(tree.is_empty());
        assert_eq!(tree.height(), 0);
        assert_eq!(tree.node_count(), 0);
    }
}

#[test]
fn sequential_inserts_grow_height() {
    let mut tree = Tree::new(VOLUME);
    for n in 0..200_000 {
        tree.insert(key(n), val(u64::from(n)));
    }
    tree.check(true);
    assert!(tree.height() >= 3);
    for n in (0..200_000).step_by(2) {
        assert!(tree.remove(&key(n)).is_some());
    }
    tree.check(true);
    assert_eq!(tree.len(), 100_000);
}

#[test]
fn descending_inserts_keep_first_keys_lower_bounds() {
    let mut tree = Tree::new(VOLUME);
    let mut model = Model::new();
    for n in (0..50_000).rev() {
        tree.insert(key(n), val(u64::from(n)));
        model.insert(key(n), val(u64::from(n)));
    }
    tree.check(true);
    same(&tree, &model);
}

#[derive(Default)]
struct Disk {
    blocks: HashMap<NodeAddr, Box<[u8; UNIT_BYTES]>>,
    next: u64,
    allocated: HashSet<NodeAddr>,
    freed: HashSet<NodeAddr>,
}

impl Disk {
    fn alloc(&mut self) -> NodeAddr {
        let n = self.next;
        self.next += 1;
        let addr = NodeAddr::new(1 + (n / 4000) as u32, (n % 4000) as u16);
        self.allocated.insert(addr);
        addr
    }

    fn take_freed(&mut self, tree: &mut Tree) {
        for addr in tree.take_freed() {
            assert!(self.allocated.contains(&addr), "freed unknown {addr:?}");
            assert!(self.freed.insert(addr), "double free {addr:?}");
        }
    }

    /// Live and freed addresses partition everything allocated.
    fn check_accounting(&self, tree: &Tree) {
        let live: HashSet<_> = tree.node_addrs().collect();
        assert_eq!(
            live.len(),
            tree.node_addrs().count(),
            "duplicate live address"
        );
        assert!(live.is_disjoint(&self.freed), "live address freed");
        let union: HashSet<_> = live.union(&self.freed).copied().collect();
        assert_eq!(union, self.allocated, "leaked addresses");
    }

    fn prepare(&mut self, tree: &mut Tree, seq: u64) -> CommitPlan {
        let mut alloc = || Ok(self.alloc());
        tree.prepare_commit(seq, &mut alloc).unwrap()
    }

    fn write(&mut self, plan: &CommitPlan) {
        for (addr, bytes) in &plan.writes {
            assert!(
                self.blocks.insert(*addr, bytes.clone()).is_none(),
                "overwrite"
            );
        }
    }

    fn commit(&mut self, tree: &mut Tree, seq: u64) -> Super {
        let plan = self.prepare(tree, seq);
        self.write(&plan);
        let sb = Super::of(&plan);
        tree.commit_succeeded(plan);
        self.take_freed(tree);
        assert!(!tree.is_dirty());
        self.check_accounting(tree);
        sb
    }

    fn load(&self, sb: &Super) -> (Tree, LoadReport) {
        let mut loader = Loader::new(VOLUME, SLOTS, sb.seq, sb.root, sb.level, sb.entries);
        loop {
            let reads = loader.next_reads();
            if reads.is_empty() {
                break;
            }
            for addr in reads {
                loader.supply(addr, self.blocks.get(&addr).map(|b| &**b));
            }
        }
        assert!(loader.is_done());
        let (tree, report) = loader.finish();
        tree.check(false);
        assert_eq!(report.entries, tree.len());
        (tree, report)
    }

    fn corrupt(&mut self, addr: NodeAddr) {
        self.blocks.get_mut(&addr).unwrap()[100] ^= 0x40;
    }

    fn node(&self, addr: NodeAddr) -> DecodedNode {
        let expect = Expect {
            volume_id: VOLUME,
            slot_count: SLOTS,
            addr,
            max_seq: u64::MAX,
            level: None,
            low: Key::MIN,
            high: None,
        };
        node::decode(&self.blocks[&addr], &expect).unwrap()
    }
}

#[derive(Clone, Copy, Debug)]
struct Super {
    seq: u64,
    root: [NodeAddr; 2],
    level: u8,
    entries: u64,
}

impl Super {
    fn of(plan: &CommitPlan) -> Self {
        Self {
            seq: plan.seq,
            root: plan.root,
            level: plan.root_level,
            entries: plan.entries,
        }
    }
}

fn expected_writes(tree: &Tree) -> usize {
    tree.nodes
        .iter()
        .filter(|n| n.live && n.dirty)
        .map(|n| if n.level == 0 { 1 } else { 2 })
        .sum()
}

#[test]
fn empty_tree_commits_and_loads() {
    let mut disk = Disk::default();
    let mut tree = Tree::new(VOLUME);
    assert!(!tree.is_dirty());
    let sb = disk.commit(&mut tree, 1);
    assert_eq!(sb.root, NO_ADDRS);
    let (loaded, report) = disk.load(&sb);
    assert!(loaded.is_empty() && !report.root_lost && !loaded.is_dirty());
    // Insert then remove everything: the root changes back to empty.
    tree.insert(key(1), val(1));
    disk.commit(&mut tree, 2);
    tree.remove(&key(1));
    assert!(tree.is_dirty());
    let sb = disk.commit(&mut tree, 3);
    assert_eq!(sb.root, NO_ADDRS);
}

#[test]
fn commit_load_round_trips() {
    for seed in seeds() {
        let mut rng = Rng::new(seed ^ 0x55);
        let mut tree = Tree::new(VOLUME);
        let mut model = Model::new();
        let mut disk = Disk::default();
        let mut history = Vec::new();
        for round in 1..=40u64 {
            let ops = if round == 1 { 20_000 } else { rng.below(800) };
            for _ in 0..ops {
                step(&mut tree, &mut model, &mut rng, 30_000);
            }
            tree.check(true);
            let dirty_writes = expected_writes(&tree);
            let snapshot = model.clone();
            match round % 5 {
                0 => {
                    // Failed attempt, then a successful one.
                    let plan = disk.prepare(&mut tree, round);
                    let addrs: HashSet<_> = plan.writes.iter().map(|w| w.0).collect();
                    tree.commit_failed(plan);
                    let freed: HashSet<_> = tree.freed.iter().copied().collect();
                    assert!(addrs.is_subset(&freed));
                    disk.take_freed(&mut tree);
                    disk.check_accounting(&tree);
                    assert!(tree.is_dirty() || dirty_writes == 0);
                    let sb = disk.commit(&mut tree, round);
                    history.push((sb, snapshot));
                }
                3 => {
                    // Mutation between prepare and success.
                    let plan = disk.prepare(&mut tree, round);
                    assert_eq!(plan.writes.len(), dirty_writes);
                    for _ in 0..rng.below(50) + 1 {
                        let k = key(rng.below(30_000) as u32);
                        let v = val(rng.next() >> 1);
                        assert_eq!(tree.insert(k, v), model.insert(k, v));
                    }
                    disk.write(&plan);
                    let sb = Super::of(&plan);
                    tree.commit_succeeded(plan);
                    assert!(tree.is_dirty());
                    disk.take_freed(&mut tree);
                    disk.check_accounting(&tree);
                    tree.check(true);
                    history.push((sb, snapshot));
                }
                _ => {
                    let plan = disk.prepare(&mut tree, round);
                    assert_eq!(plan.writes.len(), dirty_writes);
                    disk.write(&plan);
                    let sb = Super::of(&plan);
                    tree.commit_succeeded(plan);
                    disk.take_freed(&mut tree);
                    assert!(!tree.is_dirty());
                    disk.check_accounting(&tree);
                    history.push((sb, snapshot));
                }
            }
        }
        let sb = disk.commit(&mut tree, 41);
        // A clean tree still yields a plan with the current root.
        let plan = disk.prepare(&mut tree, 42);
        assert!(plan.writes.is_empty());
        assert_eq!(plan.root, sb.root);
        tree.commit_succeeded(plan);
        history.push((sb, model.clone()));

        let (loaded, report) = disk.load(&sb);
        assert_eq!(report.lost, vec![]);
        assert_eq!(report.invalid_copies, 0);
        assert!(!loaded.is_dirty());
        same(&loaded, &model);
        let live: HashSet<_> = tree.node_addrs().collect();
        assert_eq!(loaded.node_addrs().collect::<HashSet<_>>(), live);
        for (sb, snapshot) in &history {
            let (old, report) = disk.load(sb);
            assert!(report.lost.is_empty());
            same(&old, snapshot);
        }
    }
}

#[test]
fn alloc_failure_rolls_back() {
    let mut tree = Tree::new(VOLUME);
    let mut model = Model::new();
    let mut rng = Rng::new(7);
    for _ in 0..5_000 {
        step(&mut tree, &mut model, &mut rng, 10_000);
    }
    let mut disk = Disk::default();
    for limit in [0, 1, 5, 30] {
        let mut given = 0;
        let mut alloc = || {
            if given == limit {
                return Err(Error::Busy);
            }
            given += 1;
            Ok(disk.alloc())
        };
        assert_eq!(tree.prepare_commit(1, &mut alloc).unwrap_err(), Error::Busy);
        assert_eq!(tree.freed.len(), limit);
        disk.take_freed(&mut tree);
        assert!(tree.is_dirty());
        tree.check(true);
    }
    let sb = disk.commit(&mut tree, 2);
    let (loaded, _) = disk.load(&sb);
    same(&loaded, &model);
}

/// Bounds of the children of an inner node.
fn child_ranges(
    node: &DecodedNode,
    low: Key,
    high: Option<Key>,
) -> Vec<([NodeAddr; 2], Key, Option<Key>)> {
    let NodeBody::Inner(entries) = &node.body else {
        panic!("not inner");
    };
    entries
        .iter()
        .enumerate()
        .map(|(i, (k, addrs))| {
            let lo = if i == 0 { low } else { *k };
            let hi = entries.get(i + 1).map(|e| e.0).or(high);
            (*addrs, lo, hi)
        })
        .collect()
}

fn big() -> (Disk, Tree, Model, Super) {
    let mut tree = Tree::new(VOLUME);
    let mut model = Model::new();
    for n in 0..20_000 {
        tree.insert(key(n * 2), val(u64::from(n)));
        model.insert(key(n * 2), val(u64::from(n)));
    }
    let mut disk = Disk::default();
    let sb = disk.commit(&mut tree, 1);
    assert_eq!(sb.level, 2);
    (disk, tree, model, sb)
}

fn without(model: &Model, low: Key, high: Option<Key>) -> Model {
    model
        .iter()
        .filter(|(k, _)| **k < low || high.is_some_and(|h| **k >= h))
        .map(|(k, v)| (*k, *v))
        .collect()
}

/// The loaded tree commits and reloads to the same contents.
fn recommit(disk: &mut Disk, mut tree: Tree, model: &Model) {
    let plan = disk.prepare(&mut tree, 2);
    disk.write(&plan);
    let sb = Super::of(&plan);
    tree.commit_succeeded(plan);
    assert!(!tree.is_dirty());
    let (again, report) = disk.load(&sb);
    assert!(report.lost.is_empty() && report.invalid_copies == 0);
    same(&again, model);
}

#[test]
fn one_inner_copy_corrupt_recovers() {
    let (mut disk, _, model, sb) = big();
    let root = disk.node(sb.root[0]);
    let children = child_ranges(&root, Key::MIN, None);
    let (addrs, _, _) = children[children.len() / 2];
    disk.corrupt(addrs[0]);
    let (tree, report) = disk.load(&sb);
    assert!(report.lost.is_empty());
    assert_eq!(report.invalid_copies, 1);
    same(&tree, &model);
    assert!(tree.is_dirty());
    recommit(&mut disk, tree, &model);

    // A corrupt root copy also recovers.
    let (mut disk, _, model, sb) = big();
    disk.corrupt(sb.root[0]);
    let (tree, report) = disk.load(&sb);
    assert!(report.lost.is_empty() && !report.root_lost);
    same(&tree, &model);
    recommit(&mut disk, tree, &model);
}

#[test]
fn both_inner_copies_corrupt_lose_range() {
    for pick in [0, 3, usize::MAX] {
        let (mut disk, _, model, sb) = big();
        let root = disk.node(sb.root[0]);
        let children = child_ranges(&root, Key::MIN, None);
        let (addrs, low, high) = children[pick.min(children.len() - 1)];
        disk.corrupt(addrs[0]);
        disk.corrupt(addrs[1]);
        let (tree, report) = disk.load(&sb);
        assert_eq!(report.lost, vec![(low, high)]);
        assert_eq!(report.invalid_copies, 2);
        assert_eq!(report.lost_addrs.len(), 2);
        let expected = without(&model, low, high);
        assert!(expected.len() < model.len());
        same(&tree, &expected);
        recommit(&mut disk, tree, &expected);
    }
}

#[test]
fn corrupt_leaf_loses_its_range() {
    let (mut disk, _, model, sb) = big();
    let root = disk.node(sb.root[0]);
    let (inner, low, high) = child_ranges(&root, Key::MIN, None)[1];
    let node = disk.node(inner[0]);
    let leaves = child_ranges(&node, low, high);
    let (leaf, leaf_low, leaf_high) = leaves[0];
    assert!(leaf[1].is_none());
    disk.corrupt(leaf[0]);
    let (tree, report) = disk.load(&sb);
    assert_eq!(report.lost, vec![(leaf_low, leaf_high)]);
    let expected = without(&model, leaf_low, leaf_high);
    assert_eq!(model.len() - expected.len(), 36);
    same(&tree, &expected);
    // Inserting into the lost range works and commits.
    let mut tree = tree;
    let mut expected = expected;
    let k = *model.range(leaf_low..).next().unwrap().0;
    tree.insert(k, val(5));
    expected.insert(k, val(5));
    tree.check(false);
    recommit(&mut disk, tree, &expected);
}

#[test]
fn corrupt_root_is_lost() {
    let (mut disk, _, _, sb) = big();
    disk.corrupt(sb.root[0]);
    disk.corrupt(sb.root[1]);
    let (mut tree, report) = disk.load(&sb);
    assert!(report.root_lost);
    assert_eq!(report.lost, vec![(Key::MIN, None)]);
    assert!(tree.is_empty() && tree.is_dirty());
    let plan = disk.prepare(&mut tree, 2);
    assert_eq!(plan.root, NO_ADDRS);
    assert!(plan.writes.is_empty());
    tree.commit_succeeded(plan);
    assert!(!tree.is_dirty());

    // Missing reads count as invalid too.
    let (mut disk, _, _, sb) = big();
    disk.blocks.remove(&sb.root[0]);
    disk.blocks.remove(&sb.root[1]);
    let (_, report) = disk.load(&sb);
    assert!(report.root_lost);
    assert_eq!(report.invalid_copies, 2);
}

#[test]
fn losing_all_but_one_child_collapses_root() {
    let (mut disk, _, model, sb) = big();
    let root = disk.node(sb.root[0]);
    let children = child_ranges(&root, Key::MIN, None);
    let mut expected = model.clone();
    for (addrs, low, high) in &children[1..] {
        disk.corrupt(addrs[0]);
        disk.corrupt(addrs[1]);
        expected = without(&expected, *low, *high);
    }
    let (tree, report) = disk.load(&sb);
    assert_eq!(report.lost.len(), children.len() - 1);
    assert_eq!(tree.height(), 2);
    assert!(tree.is_dirty());
    same(&tree, &expected);
    recommit(&mut disk, tree, &expected);
}

#[test]
fn wrong_seq_or_level_is_rejected() {
    let (disk, _, _, sb) = big();
    let older = Super { seq: 0, ..sb };
    let (_, report) = disk.load(&older);
    assert!(report.root_lost);
    let wrong_level = Super { level: 3, ..sb };
    let (_, report) = disk.load(&wrong_level);
    assert!(report.root_lost);
}

#[test]
fn duplicate_reference_is_invalid() {
    // A root whose two copies have the same address: the second is a duplicate.
    let mut tree = Tree::new(VOLUME);
    for n in 0..1_000 {
        tree.insert(key(n), val(u64::from(n)));
    }
    let mut disk = Disk::default();
    let sb = disk.commit(&mut tree, 1);
    let dup = Super {
        root: [sb.root[0], sb.root[0]],
        ..sb
    };
    disk.corrupt(sb.root[0]);
    let (_, report) = disk.load(&dup);
    assert!(report.root_lost);
    assert_eq!(report.invalid_copies, 2);
    assert_eq!(report.lost_addrs, vec![sb.root[0]]);
}
