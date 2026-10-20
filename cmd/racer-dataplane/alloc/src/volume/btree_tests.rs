use super::*;
use crate::volume::format::{Tier, UNIT_BYTES, Value};
use crate::volume::page::RAW_BYTES;
use std::cell::RefCell;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::task::{Context, Poll, Waker};

const VOLUME: [u8; 16] = [3; 16];
const SLOTS: u64 = 64;

fn block_on<T>(future: impl Future<Output = T>) -> T {
    let mut future = Box::pin(future);
    match future
        .as_mut()
        .poll(&mut Context::from_waker(Waker::noop()))
    {
        Poll::Ready(value) => value,
        Poll::Pending => panic!("in-memory future pending"),
    }
}

#[derive(Default)]
struct Disk {
    units: RefCell<HashMap<NodeAddr, Box<[u8; UNIT_BYTES]>>>,
    next: RefCell<u64>,
    loads: RefCell<u64>,
}

impl Disk {
    fn alloc(&self) -> NodeAddr {
        let mut next = self.next.borrow_mut();
        *next += 1;
        NodeAddr::new(1 + (*next / 4000) as u32, 2 + (*next % 4000) as u16)
    }
}

impl Source for Disk {
    type Error = ();

    fn cached(&self, req: &Req) -> Option<Rc<TreePage>> {
        block_on(self.load(*req)).unwrap()
    }

    fn load(&self, req: Req) -> LocalFuture<'_, Result<Option<Rc<TreePage>>, ()>> {
        Box::pin(async move {
            *self.loads.borrow_mut() += 1;
            for addr in req.addrs.iter().filter(|a| !a.is_none()) {
                let Some(bytes) = self.units.borrow().get(addr).cloned() else {
                    continue;
                };
                if let Some(page) = TreePage::decode(bytes, &VOLUME, *addr, SLOTS)
                    && page.fits(
                        req.kind,
                        req.level,
                        req.max_seq,
                        &req.low,
                        req.high.as_ref(),
                    )
                {
                    return Ok(Some(Rc::new(page)));
                }
            }
            Ok(None)
        })
    }

    fn prefetch(&self, _reqs: Vec<Req>) -> LocalFuture<'_, ()> {
        Box::pin(async {})
    }
}

struct Eager<'d> {
    disk: &'d Disk,
    seq: u64,
}

impl Sink for Eager<'_> {
    type Error = ();

    fn emit<'a>(
        &'a mut self,
        entries: &'a [(Key, Raw)],
    ) -> LocalFuture<'a, Result<Option<NodeAddr>, ()>> {
        Box::pin(async move {
            let addr = self.disk.alloc();
            let mut buf = Box::new([0u8; UNIT_BYTES]);
            super::super::page::encode_leaf(
                &mut buf[..],
                TreeKind::Index,
                &VOLUME,
                self.seq,
                addr,
                entries.iter().map(|(k, v)| (k, v)),
            );
            self.disk.units.borrow_mut().insert(addr, buf);
            Ok(Some(addr))
        })
    }
}

fn key(n: u32) -> Key {
    Key::new([(n % 251) as u8; 8], [1; 12], n)
}

/// Sorted puts of `keys`.
fn puts(keys: std::ops::Range<u32>, value: impl Fn(u32) -> Raw) -> Vec<(Key, Option<Raw>)> {
    let map: BTreeMap<Key, Option<Raw>> = keys.map(|n| (key(n), Some(value(n)))).collect();
    map.into_iter().collect()
}

fn raw(n: u64) -> Raw {
    let value = Value {
        slot: 2,
        unit: 0,
        class: 0,
        tier: Tier::Cache,
        length: 1,
        flags: 0,
        generation: n + 1,
        retain_until: 0,
    };
    let mut raw = [0; RAW_BYTES];
    raw[..32].copy_from_slice(&value.encode());
    raw
}

/// Apply ops (Some = put) and place every new page.
fn commit(
    disk: &Disk,
    root: &TreeRoot,
    seq: u64,
    ops: &[(Key, Option<Raw>)],
    eager: bool,
) -> (TreeRoot, Applied) {
    let mut delta: i64 = 0;
    let mut resolve = |_: &Key, old: Option<&Raw>, op: &Option<Raw>| {
        delta += i64::from(op.is_some()) - i64::from(old.is_some());
        *op
    };
    let applied = if eager {
        let mut sink = Eager { disk, seq };
        block_on(apply(
            disk,
            &mut sink,
            root,
            TreeKind::Index,
            seq - 1,
            ops,
            &mut resolve,
        ))
    } else {
        struct Keep;
        impl Sink for Keep {
            type Error = ();
            fn emit<'a>(
                &'a mut self,
                _: &'a [(Key, Raw)],
            ) -> LocalFuture<'a, Result<Option<NodeAddr>, ()>> {
                Box::pin(async { Ok(None) })
            }
        }
        block_on(apply(
            disk,
            &mut Keep,
            root,
            TreeKind::Index,
            seq - 1,
            ops,
            &mut resolve,
        ))
    }
    .unwrap();
    let order = placement_order(&applied);
    let mut placed = vec![[NodeAddr::NONE; 2]; applied.pages.len()];
    for &index in &order {
        let page = &applied.pages[index];
        let copies = if page.level == 0 { 1 } else { 2 };
        let mut addrs = [NodeAddr::NONE; 2];
        for slot in addrs.iter_mut().take(copies) {
            let addr = disk.alloc();
            let mut buf = Box::new([0u8; UNIT_BYTES]);
            encode_new(
                &mut buf[..],
                page,
                &placed,
                TreeKind::Index,
                &VOLUME,
                seq,
                addr,
            );
            disk.units.borrow_mut().insert(addr, buf);
            *slot = addr;
        }
        placed[index] = addrs;
    }
    let root_addrs = applied
        .root
        .map_or([NodeAddr::NONE; 2], |child| resolve_child(&child, &placed));
    let count = (root.count as i64 + delta) as u64;
    (
        TreeRoot {
            root: root_addrs,
            level: if root_addrs[0].is_none() {
                0
            } else {
                applied.level
            },
            count,
        },
        applied,
    )
}

fn scan(disk: &Disk, root: &TreeRoot, seq: u64) -> (Vec<(Key, Raw)>, Vec<(Key, Option<Key>)>) {
    let mut out = Vec::new();
    let mut lost = Vec::new();
    let mut at = Key::MIN;
    loop {
        let high =
            match block_on(seek(disk, root, TreeKind::Index, seq, &at, Priority::Scan)).unwrap() {
                Seek::Empty => return (out, lost),
                Seek::Lost(low, high) => {
                    lost.push((low, high));
                    high
                }
                Seek::Leaf(page, high) => {
                    for i in 0..page.count() {
                        if page.key(i) >= at {
                            out.push((page.key(i), page.raw(i)));
                        }
                    }
                    high
                }
            };
        match high {
            Some(high) => at = high,
            None => return (out, lost),
        }
    }
}

fn addresses(disk: &Disk, root: &TreeRoot, seq: u64) -> HashSet<NodeAddr> {
    let mut set = HashSet::new();
    let lost = block_on(walk_inner(
        disk,
        root,
        TreeKind::Index,
        seq,
        &mut |addr, _| {
            set.insert(addr);
        },
    ))
    .unwrap();
    assert!(lost.is_empty());
    set
}

struct Rng(u64);

impl Rng {
    fn below(&mut self, n: u64) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0 % n
    }
}

#[test]
fn random_batches_match_a_model() {
    for (seed, eager) in [(1u64, true), (2, false), (3, true)] {
        let disk = Disk::default();
        let mut rng = Rng(seed * 7919 + 1);
        let mut model = BTreeMap::new();
        let mut root = TreeRoot::EMPTY;
        for round in 0..60u64 {
            let seq = round + 2;
            let mut ops = BTreeMap::new();
            let size = 1 + rng.below(if round % 10 == 0 { 3000 } else { 200 });
            let deleting = round % 7 >= 4;
            for _ in 0..size {
                let k = key(rng.below(5000) as u32);
                let op = if deleting || rng.below(4) == 0 {
                    None
                } else {
                    Some(raw(rng.below(1000)))
                };
                ops.insert(k, op);
            }
            let ops: Vec<_> = ops.into_iter().collect();
            for (k, op) in &ops {
                match op {
                    Some(v) => model.insert(*k, *v),
                    None => model.remove(k),
                };
            }
            let before = addresses(&disk, &root, seq - 1);
            let (next, applied) = commit(&disk, &root, seq, &ops, eager);
            let after = addresses(&disk, &next, seq);
            let freed: HashSet<NodeAddr> = applied.freed.iter().copied().collect();
            assert_eq!(freed.len(), applied.freed.len(), "freed twice");
            assert!(freed.is_subset(&before), "freed a page the old tree lacks");
            assert!(freed.is_disjoint(&after), "freed a page the new tree uses");
            assert!(
                before.difference(&freed).all(|a| after.contains(a)),
                "leaked a page"
            );
            root = next;
            assert_eq!(root.count, model.len() as u64);
            let (entries, lost) = scan(&disk, &root, seq);
            assert!(lost.is_empty());
            assert_eq!(
                entries,
                model.iter().map(|(k, v)| (*k, *v)).collect::<Vec<_>>()
            );
            for _ in 0..50 {
                let k = key(rng.below(5000) as u32);
                let want = model.get(&k).map_or(Lookup::Absent, |v| Lookup::Found(*v));
                assert_eq!(
                    block_on(get(&disk, &root, TreeKind::Index, seq, &k)).unwrap(),
                    want
                );
                assert_eq!(try_get(&disk, &root, TreeKind::Index, seq, &k), Some(want));
            }
        }
        assert!(root.level >= 1 || model.len() <= 72);
    }
}

#[test]
fn lost_leaf_is_reported_and_rebuilt() {
    let disk = Disk::default();
    let ops = puts(0..1000, |n| raw(u64::from(n)));
    let (root, _) = commit(&disk, &TreeRoot::EMPTY, 2, &ops, true);
    assert!(root.level >= 1);
    // Corrupt the leaf holding key(500).
    let found = block_on(seek(
        &disk,
        &root,
        TreeKind::Index,
        2,
        &key(500),
        Priority::Leaf,
    ))
    .unwrap();
    let Seek::Leaf(page, _) = found else {
        panic!("leaf");
    };
    let n = page.count();
    let addr = *disk
        .units
        .borrow()
        .iter()
        .find(|(_, bytes)| bytes[..] == page.bytes()[..])
        .unwrap()
        .0;
    disk.units.borrow_mut().get_mut(&addr).unwrap()[200] ^= 1;
    let (low, high) = match block_on(get(&disk, &root, TreeKind::Index, 2, &key(500))).unwrap() {
        Lookup::Lost(l, h) => (l, h),
        other => panic!("{other:?}"),
    };
    assert!(low <= key(500) && high.is_none_or(|h| key(500) < h));
    let (entries, lost) = scan(&disk, &root, 2);
    assert_eq!(lost, vec![(low, high)]);
    assert_eq!(entries.len(), 1000 - n);
    // A batch touching the range rebuilds it from the batch alone.
    let (root, applied) = commit(&disk, &root, 3, &[(key(500), Some(raw(7)))], true);
    assert_eq!(applied.lost, vec![(low, high)]);
    let (entries, lost) = scan(&disk, &root, 3);
    assert!(lost.is_empty());
    assert_eq!(entries.len(), 1000 - n + 1);
    assert_eq!(
        block_on(get(&disk, &root, TreeKind::Index, 3, &key(500))).unwrap(),
        Lookup::Found(raw(7))
    );
}

#[test]
fn bad_inner_copy_falls_back_to_the_other() {
    let disk = Disk::default();
    let ops = puts(0..10_000, |n| raw(u64::from(n)));
    let (root, _) = commit(&disk, &TreeRoot::EMPTY, 2, &ops, true);
    assert!(root.level >= 2);
    disk.units.borrow_mut().get_mut(&root.root[0]).unwrap()[300] ^= 1;
    let (entries, lost) = scan(&disk, &root, 2);
    assert!(lost.is_empty());
    assert_eq!(entries.len(), 10_000);
}

#[test]
fn deleting_everything_empties_the_tree() {
    let disk = Disk::default();
    let ops = puts(0..5000, |_| raw(1));
    let (root, _) = commit(&disk, &TreeRoot::EMPTY, 2, &ops, false);
    let deletes: Vec<_> = ops.iter().map(|(k, _)| (*k, None)).collect();
    let (root, applied) = commit(&disk, &root, 3, &deletes, false);
    assert!(root.is_empty() && root.count == 0);
    assert!(applied.root.is_none());
    // A point update rewrites one path only.
    let (root, _) = commit(&disk, &TreeRoot::EMPTY, 4, &ops, true);
    let loads = *disk.loads.borrow();
    let (_, applied) = commit(&disk, &root, 5, &[(key(77), Some(raw(9)))], true);
    assert_eq!(applied.freed.len(), 1 + 2 * usize::from(root.level));
    assert!(*disk.loads.borrow() - loads <= u64::from(root.level) + 1);
}
