// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Integration tests for the CoW B+tree against [`MockDevice`].

use std::future::Future;
use std::pin::pin;
use std::sync::Arc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use crate::storage::alloc::Allocator;
use crate::storage::blockdev::{BlockDevice, MockDevice, MockDeviceConfig, ScratchPool};
use crate::storage::btree::page::{self, Decoded, META_SLOT_A, META_SLOT_B};
use crate::storage::btree::{BTreeIndex, LeafEntry, Mutation};
use crate::storage::types::{Checksum, Lba, PageKey};

fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    unsafe { Waker::from_raw(raw()) }
}

fn block_on<F: Future>(f: F) -> F::Output {
    let w = noop_waker();
    let mut cx = Context::from_waker(&w);
    let mut f = pin!(f);
    let mut spins = 0u64;
    loop {
        match f.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < 1_000_000, "block_on stuck");
            }
        }
    }
}

fn key(i: u32) -> PageKey {
    let mut h = [0u8; 32];
    h[..4].copy_from_slice(&i.to_be_bytes());
    PageKey::new(h, 0)
}

fn entry(lba: u64) -> LeafEntry {
    LeafEntry {
        lba: Lba(lba),
        data_checksum: Checksum(lba.wrapping_mul(0xDEADBEEF)),
        byte_len: 2 * 1024 * 1024,
    }
}

fn fresh(capacity_pages: u64) -> (Arc<MockDevice>, Arc<Allocator>) {
    let cfg = MockDeviceConfig {
        page_size: 4096,
        capacity_pages,
        ..Default::default()
    };
    let device = Arc::new(MockDevice::new(cfg));
    let alloc = Arc::new(Allocator::new(capacity_pages));
    (device, alloc)
}

/// Construct a [`BTreeIndex`] against `dev`/`alloc`, with a
/// fresh [`ScratchPool`] registered on the device. Used by every
/// test below so the registered-buffer contract holds end-to-end.
fn open_btree(
    dev: Arc<MockDevice>,
    alloc: Arc<Allocator>,
) -> impl Future<Output = Result<BTreeIndex<MockDevice>, crate::storage::types::Error>> {
    let scratch = ScratchPool::new(&*dev, 4096, 8).expect("scratch pool");
    BTreeIndex::open(dev, alloc, scratch, false)
}

#[test]
fn open_empty_creates_meta_and_root() {
    let (dev, alloc) = fresh(64);
    let idx: BTreeIndex<MockDevice> =
        block_on(open_btree(dev.clone(), alloc.clone())).unwrap();
    assert_eq!(idx.current_txn(), 1);
    // Meta slots are reserved + at least one root page.
    assert!(alloc.is_in_use(META_SLOT_A).unwrap());
    assert!(alloc.is_in_use(META_SLOT_B).unwrap());
    assert!(alloc.is_in_use(idx.current_root()).unwrap());
    // No entries.
    assert_eq!(idx.live_entries(), 0);
    assert!(block_on(idx.lookup(&key(1))).unwrap().is_none());
}

#[test]
fn insert_then_lookup() {
    let (dev, alloc) = fresh(64);
    let idx = block_on(open_btree(dev, alloc)).unwrap();
    let muts = vec![
        Mutation::Insert {
            key: key(1),
            value: entry(100),
        },
        Mutation::Insert {
            key: key(2),
            value: entry(200),
        },
    ];
    block_on(idx.apply_batch(muts)).unwrap();
    assert_eq!(block_on(idx.lookup(&key(1))).unwrap(), Some(entry(100)));
    assert_eq!(block_on(idx.lookup(&key(2))).unwrap(), Some(entry(200)));
    assert_eq!(block_on(idx.lookup(&key(3))).unwrap(), None);
}

#[test]
fn delete_removes_entry() {
    let (dev, alloc) = fresh(64);
    let idx = block_on(open_btree(dev, alloc)).unwrap();
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(1),
        value: entry(100),
    }]))
    .unwrap();
    block_on(idx.apply_batch(vec![Mutation::Delete { key: key(1) }])).unwrap();
    assert_eq!(block_on(idx.lookup(&key(1))).unwrap(), None);
}

#[test]
fn large_batch_spans_multiple_leaves() {
    // 200 entries > one 4 KiB leaf (cap = 72) so this forces at
    // least one internal page above the leaves.
    let (dev, alloc) = fresh(512);
    let idx = block_on(open_btree(dev, alloc)).unwrap();
    let muts: Vec<_> = (0..200u32)
        .map(|i| Mutation::Insert {
            key: key(i),
            value: entry(1000 + i as u64),
        })
        .collect();
    block_on(idx.apply_batch(muts)).unwrap();
    for i in 0..200u32 {
        assert_eq!(
            block_on(idx.lookup(&key(i))).unwrap(),
            Some(entry(1000 + i as u64)),
            "key {i}",
        );
    }
    assert!(block_on(idx.lookup(&key(300))).unwrap().is_none());
}

#[test]
fn restart_from_meta_restores_entries() {
    let (dev, alloc) = fresh(128);
    {
        let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
        for i in 0..50u32 {
            block_on(idx.apply_batch(vec![Mutation::Insert {
                key: key(i),
                value: entry(i as u64 * 10),
            }]))
            .unwrap();
        }
    }
    // New allocator (simulates restart): only the persistent
    // device survives.
    let alloc2 = Arc::new(Allocator::new(128));
    let idx2 = block_on(open_btree(dev, alloc2)).unwrap();
    for i in 0..50u32 {
        assert_eq!(
            block_on(idx2.lookup(&key(i))).unwrap(),
            Some(entry(i as u64 * 10)),
            "key {i} survived restart",
        );
    }
}

#[test]
fn restart_picks_highest_txn_meta() {
    // Walk the index a few times so both meta slots get written
    // at least once; then make sure the higher-txn one wins.
    let (dev, alloc) = fresh(128);
    let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(1),
        value: entry(11),
    }]))
    .unwrap();
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(2),
        value: entry(22),
    }]))
    .unwrap();
    let txn = idx.current_txn();
    drop(idx);

    let alloc2 = Arc::new(Allocator::new(128));
    let idx2 = block_on(open_btree(dev, alloc2)).unwrap();
    assert_eq!(idx2.current_txn(), txn);
    assert_eq!(block_on(idx2.lookup(&key(1))).unwrap(), Some(entry(11)));
    assert_eq!(block_on(idx2.lookup(&key(2))).unwrap(), Some(entry(22)));
}

#[test]
fn corrupted_active_meta_falls_back_to_other_slot() {
    let (dev, alloc) = fresh(128);
    let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(1),
        value: entry(11),
    }]))
    .unwrap();
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(2),
        value: entry(22),
    }]))
    .unwrap();
    let active_slot = idx.active_meta_slot();
    drop(idx);

    // Smash the *active* meta slot. The other slot should still
    // have a valid (older) tree to recover from.
    let bad = vec![0xFFu8; 4096];
    let slot = if active_slot == 0 {
        META_SLOT_A
    } else {
        META_SLOT_B
    };
    dev.poke(slot, &bad);

    let alloc2 = Arc::new(Allocator::new(128));
    let idx2 = block_on(open_btree(dev, alloc2)).unwrap();
    // The older meta only saw the first insert.
    assert_eq!(block_on(idx2.lookup(&key(1))).unwrap(), Some(entry(11)));
}

#[test]
fn double_corrupted_meta_triggers_lba_scan_rebuild() {
    let (dev, alloc) = fresh(128);
    let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
    block_on(idx.apply_batch(vec![
        Mutation::Insert {
            key: key(1),
            value: entry(11),
        },
        Mutation::Insert {
            key: key(2),
            value: entry(22),
        },
        Mutation::Insert {
            key: key(3),
            value: entry(33),
        },
    ]))
    .unwrap();
    drop(idx);

    let garbage = vec![0u8; 4096];
    dev.poke(META_SLOT_A, &garbage);
    dev.poke(META_SLOT_B, &garbage);

    let alloc2 = Arc::new(Allocator::new(128));
    let idx2 = block_on(open_btree(dev, alloc2)).unwrap();
    assert_eq!(block_on(idx2.lookup(&key(1))).unwrap(), Some(entry(11)));
    assert_eq!(block_on(idx2.lookup(&key(2))).unwrap(), Some(entry(22)));
    assert_eq!(block_on(idx2.lookup(&key(3))).unwrap(), Some(entry(33)));
}

#[test]
fn snapshot_drop_frees_old_pages() {
    // Catches a regression where dropping the prior `RootSnapshot`
    // fails to hand its pages back to the allocator: without the
    // baseline + monotonicity bounds, an empty Drop impl (or a
    // committed write that never allocated) would still satisfy
    // a bare `used_after_first == used_after_second` check.
    let (dev, alloc) = fresh(128);
    let used_before_any = alloc.used_pages();
    let idx = block_on(open_btree(dev, alloc.clone())).unwrap();
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(1),
        value: entry(11),
    }]))
    .unwrap();
    let used_after_first = alloc.used_pages();
    assert!(
        used_after_first > used_before_any,
        "first commit must allocate pages (before={used_before_any}, after={used_after_first})",
    );
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(2),
        value: entry(22),
    }]))
    .unwrap();
    // After the second commit the new snapshot is the only
    // strong ref (no `Guard`s outstanding) so the old snapshot's
    // pages have been freed. With one entry the tree is a single
    // leaf, so total usage must not grow past the first commit.
    let used_after_second = alloc.used_pages();
    assert!(
        used_after_second <= used_after_first,
        "second commit must not grow allocator (after_first={used_after_first}, after_second={used_after_second})",
    );
}

// Decode one meta slot via direct device peek. Returns `None` if
// the slot does not decode (corrupt / empty).
fn read_meta_slot(dev: &MockDevice, lba: Lba) -> Option<(u64, Lba, u64)> {
    let mut buf = vec![0u8; dev.page_size()];
    dev.peek(lba, &mut buf);
    match page::decode(&buf) {
        Decoded::Meta {
            txn_id,
            root_lba,
            hwm,
        } => Some((txn_id, root_lba, hwm)),
        _ => None,
    }
}

// Highest-txn-id meta slot currently on disk, or `None` if neither
// slot decodes. Mirrors what `meta::load_meta` would select.
fn read_live_meta(dev: &MockDevice) -> Option<(u64, Lba, u64)> {
    let a = read_meta_slot(dev, META_SLOT_A);
    let b = read_meta_slot(dev, META_SLOT_B);
    match (a, b) {
        (None, None) => None,
        (Some(m), None) | (None, Some(m)) => Some(m),
        (Some(ma), Some(mb)) => Some(if ma.0 >= mb.0 { ma } else { mb }),
    }
}

#[test]
fn hwm_persisted_in_meta_after_bootstrap() {
    // Bootstrap must persist a non-zero hwm because the allocator
    // reserves META_SLOT_B (LBA 1) before the first commit.
    let (dev, alloc) = fresh(64);
    let _idx: BTreeIndex<MockDevice> =
        block_on(open_btree(dev.clone(), alloc.clone())).unwrap();
    let (_txn, _root, hwm) = read_live_meta(&dev).expect("bootstrap writes a valid meta slot");
    assert!(
        hwm >= 1,
        "bootstrap should observe at least the reserved meta slot LBAs in hwm; got {hwm}",
    );
}

#[test]
fn hwm_grows_with_commits() {
    // The recorded hwm must cover at least the new root LBA after
    // a commit; otherwise a bounded rebuild would miss the root.
    let (dev, alloc) = fresh(128);
    let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
    let muts: Vec<_> = (0..20u32)
        .map(|i| Mutation::Insert {
            key: key(i),
            value: entry(1000 + i as u64),
        })
        .collect();
    block_on(idx.apply_batch(muts)).unwrap();
    let root = idx.current_root();
    let (_txn, _root_meta, hwm) =
        read_live_meta(&dev).expect("post-commit meta must decode");
    assert!(
        hwm >= root.0,
        "hwm ({hwm}) must cover the new root LBA ({})",
        root.0,
    );
}

#[test]
fn hwm_monotonic_across_reopen() {
    // Reopen + commit must not let the persisted hwm regress: a
    // regression would let bounded recovery skip a real LBA.
    let (dev, alloc) = fresh(128);
    {
        let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
        block_on(idx.apply_batch(vec![
            Mutation::Insert {
                key: key(1),
                value: entry(11),
            },
            Mutation::Insert {
                key: key(2),
                value: entry(22),
            },
        ]))
        .unwrap();
    }
    let (_t1, _r1, hwm_before) = read_live_meta(&dev).expect("post-first-commit meta");

    let alloc2 = Arc::new(Allocator::new(128));
    let idx2 = block_on(open_btree(dev.clone(), alloc2)).unwrap();
    block_on(idx2.apply_batch(vec![Mutation::Insert {
        key: key(3),
        value: entry(33),
    }]))
    .unwrap();
    let (_t2, _r2, hwm_after) = read_live_meta(&dev).expect("post-second-commit meta");
    assert!(
        hwm_after >= hwm_before,
        "hwm regressed across reopen: before={hwm_before}, after={hwm_after}",
    );
}

#[test]
fn reopen_seeds_allocator_hwm() {
    // After reopen the allocator's HWM must be at least the
    // persisted hwm so future commits keep the recorded hwm
    // monotonic across restarts (bounded recovery relies on it).
    let (dev, alloc) = fresh(128);
    {
        let idx = block_on(open_btree(dev.clone(), alloc)).unwrap();
        let muts: Vec<_> = (0..30u32)
            .map(|i| Mutation::Insert {
                key: key(i),
                value: entry(2000 + i as u64),
            })
            .collect();
        block_on(idx.apply_batch(muts)).unwrap();
    }
    let (_t, _r, persisted_hwm) =
        read_live_meta(&dev).expect("post-commit meta must decode");

    let alloc2 = Arc::new(Allocator::new(128));
    let idx2 = block_on(open_btree(dev.clone(), alloc2)).unwrap();
    assert!(
        idx2.allocator_high_water() >= persisted_hwm,
        "allocator hwm ({}) after reopen must cover persisted hwm ({})",
        idx2.allocator_high_water(),
        persisted_hwm,
    );

    // A follow-up commit must not let the persisted hwm regress.
    block_on(idx2.apply_batch(vec![Mutation::Insert {
        key: key(999),
        value: entry(9999),
    }]))
    .unwrap();
    let (_t2, _r2, hwm_after) = read_live_meta(&dev).expect("post-reopen-commit meta");
    assert!(
        hwm_after >= persisted_hwm,
        "persisted hwm regressed after reopen+commit: before={persisted_hwm}, after={hwm_after}",
    );
}
