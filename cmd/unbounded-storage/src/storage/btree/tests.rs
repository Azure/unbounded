// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Integration tests for the CoW B+tree against [`MockDevice`].

use std::future::Future;
use std::pin::pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use crate::runtime::noop_waker;
use crate::storage::alloc::Allocator;
use crate::storage::blockdev::{BlockDevice, MockDevice, MockDeviceConfig, ScratchPool};
use crate::storage::btree::page::{self, Decoded, META_SLOT_A, META_SLOT_B};
use crate::storage::btree::{BTreeIndex, LeafEntry, Mutation};
use crate::storage::types::{Checksum, Lba, PageKey};

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
    BTreeIndex::open(dev, alloc, scratch, 4096, false)
}

#[test]
fn open_empty_creates_meta_and_root() {
    let (dev, alloc) = fresh(64);
    let idx: BTreeIndex<MockDevice> = block_on(open_btree(dev.clone(), alloc.clone())).unwrap();
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
    let _idx: BTreeIndex<MockDevice> = block_on(open_btree(dev.clone(), alloc.clone())).unwrap();
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
    let (_txn, _root_meta, hwm) = read_live_meta(&dev).expect("post-commit meta must decode");
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
    let (_t, _r, persisted_hwm) = read_live_meta(&dev).expect("post-commit meta must decode");

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

#[test]
fn reopen_marks_full_data_run_in_use() {
    // The write path allocates a contiguous LBA run of
    // `byte_len / btree_page_bytes` pages per entry. On reopen
    // the allocator bitmap must reflect every LBA in the run,
    // not just the start: a later `alloc` / `alloc_contig` that
    // landed inside the run would silently overwrite live data.
    // `entry()` here uses byte_len = 2 MiB with btree_page_bytes
    // = 4 KiB, so each entry covers 512 LBAs.
    const CAP: u64 = 8 * 1024;
    const BTREE_PAGE_BYTES: usize = 4096;

    let (dev, alloc) = fresh(CAP);
    // Allocate a real contiguous run for the entry so its LBAs
    // are actually in-use during phase 1 (mirrors the engine's
    // write path).
    let n_pages = (entry(0).byte_len as usize / BTREE_PAGE_BYTES) as u64;
    let run_start = alloc.alloc_contig(n_pages).unwrap();
    let leaf_entry = LeafEntry {
        lba: run_start,
        data_checksum: Checksum(0xCAFEBABE),
        byte_len: 2 * 1024 * 1024,
    };
    {
        let idx = block_on(open_btree(dev.clone(), alloc.clone())).unwrap();
        block_on(idx.apply_batch(vec![Mutation::Insert {
            key: key(1),
            value: leaf_entry,
        }]))
        .unwrap();
    }

    // Reopen with a fresh allocator: this is the regression
    // surface. Before the fix, only `run_start` would be marked;
    // `run_start + 1 .. run_start + n_pages` would be cleared.
    let alloc2 = Arc::new(Allocator::new(CAP));
    let _idx2 = block_on(open_btree(dev.clone(), alloc2.clone())).unwrap();

    for off in 0..n_pages {
        let lba = Lba(run_start.0 + off);
        assert!(
            alloc2.is_in_use(lba).unwrap(),
            "lba {} inside entry run [{}..{}) was not marked in use on reopen",
            lba.0,
            run_start.0,
            run_start.0 + n_pages,
        );
    }

    // And `alloc_contig` of the same size must not land inside
    // the existing run: that would be the silent-overwrite bug.
    let next = alloc2.alloc_contig(n_pages).unwrap();
    let next_end = next.0 + n_pages;
    let run_end = run_start.0 + n_pages;
    let overlaps = next.0 < run_end && run_start.0 < next_end;
    assert!(
        !overlaps,
        "alloc_contig({n_pages}) returned {} which overlaps the live run [{}..{})",
        next.0, run_start.0, run_end,
    );
}

#[test]
fn apply_batch_empty_is_noop() {
    // Empty mutation lists must not bump the txn counter, change
    // the published root, or allocate new pages: a no-op commit
    // would otherwise burn LBAs and meta-slot writes on every
    // empty flush the engine schedules.
    let (dev, alloc) = fresh(64);
    let idx = block_on(open_btree(dev, alloc.clone())).unwrap();
    let txn_before = idx.current_txn();
    let root_before = idx.current_root();
    let used_before = alloc.used_pages();
    block_on(idx.apply_batch(Vec::new())).unwrap();
    assert_eq!(idx.current_txn(), txn_before, "empty batch bumped txn");
    assert_eq!(idx.current_root(), root_before, "empty batch rotated root");
    assert_eq!(
        alloc.used_pages(),
        used_before,
        "empty batch allocated pages",
    );
}

#[test]
fn many_commits_to_small_keyset_bounds_allocator_growth() {
    // Path-copy CoW must reuse untouched subtrees: rewriting the
    // same handful of keys over and over should retire each
    // commit's spine and free it on the next snapshot drop. A
    // regression to bulk-rewrite (or a broken pending-free queue)
    // would let `used_pages` grow with commit count.
    let (dev, alloc) = fresh(256);
    let idx = block_on(open_btree(dev, alloc.clone())).unwrap();
    // Seed a small tree.
    let seed: Vec<_> = (0..8u32)
        .map(|i| Mutation::Insert {
            key: key(i),
            value: entry(i as u64),
        })
        .collect();
    block_on(idx.apply_batch(seed)).unwrap();
    let used_after_seed = alloc.used_pages();
    // Many commits, each touching one key. Each commit retires
    // the previous spine; on the next commit the published
    // snapshot drops and its retired pages free.
    for round in 0..50u32 {
        block_on(idx.apply_batch(vec![Mutation::Insert {
            key: key(round % 8),
            value: entry((round as u64) + 1000),
        }]))
        .unwrap();
    }
    let used_after_many = alloc.used_pages();
    // Allow modest slack (one in-flight spine plus pending
    // bundles for the currently-published snapshot) but not
    // linear growth with the 50 commits above.
    assert!(
        used_after_many <= used_after_seed + 8,
        "allocator grew without bound across commits: \
         after_seed={used_after_seed}, after_many={used_after_many}",
    );
    // And the data must still be correct.
    for i in 0..8u32 {
        assert!(
            block_on(idx.lookup(&key(i))).unwrap().is_some(),
            "key {i} missing after path-copy rewrites",
        );
    }
}

#[test]
fn outstanding_snapshot_pins_retired_pages_until_drop() {
    // Generation-tracked deferred free: a `Guard` taken on
    // snapshot N must keep N's pages reachable. Commits made
    // while the guard is held must not free anything N can still
    // see; dropping the guard must release them.
    let (dev, alloc) = fresh(256);
    let idx = block_on(open_btree(dev, alloc.clone())).unwrap();
    block_on(
        idx.apply_batch(
            (0..16u32)
                .map(|i| Mutation::Insert {
                    key: key(i),
                    value: entry(i as u64),
                })
                .collect(),
        ),
    )
    .unwrap();
    // Pin the current snapshot. We intentionally hold the `Arc`
    // (which is what `arc_swap::Guard::into_inner` would also
    // yield) so the pre-commit pages cannot be freed until we
    // drop it.
    let pinned = idx.root.load_full();
    let pinned_txn = pinned.txn_id;
    let used_pinned = alloc.used_pages();
    // Several commits while the snapshot is pinned. Pages they
    // retire belong to `pinned_txn` and must stay live.
    for round in 0..10u32 {
        block_on(idx.apply_batch(vec![Mutation::Insert {
            key: key(round % 16),
            value: entry((round as u64) + 500),
        }]))
        .unwrap();
    }
    // Pinned snapshot must still be in the alive set: a commit
    // path that forgot to insert before publishing would let an
    // intermediate Drop compute a min_alive past `pinned_txn`.
    assert!(
        idx.alive.borrow().alive.contains(&pinned_txn),
        "pinned txn {pinned_txn} dropped out of alive set",
    );
    let used_while_pinned = alloc.used_pages();
    assert!(
        used_while_pinned >= used_pinned,
        "allocator shrank while a snapshot was pinned",
    );
    // Drop the pin: every retired bundle whose retire_t fell
    // before `min_alive` should now flush.
    drop(pinned);
    let used_after_drop = alloc.used_pages();
    assert!(
        used_after_drop < used_while_pinned,
        "dropping pinned snapshot freed nothing: while_pinned={used_while_pinned}, \
         after_drop={used_after_drop}",
    );
    // And the pending queue must drain past the dropped txn.
    let pending_min = idx.pending.borrow().by_retire_t.keys().next().copied();
    assert!(
        pending_min.map(|t| t > pinned_txn).unwrap_or(true),
        "pending queue still holds bundles retired at/before pinned txn {pinned_txn}: \
         next pending retire_t = {pending_min:?}",
    );
}

#[test]
fn overwrites_and_deletes_path_copy_consistent() {
    // Mixed overwrite / delete batches drive every arm of
    // `merge_leaf` and the internal-node recursion in
    // `apply_path_copy`. Walk through several commits and assert
    // both the in-memory mirror and a fresh lookup agree.
    let (dev, alloc) = fresh(256);
    let idx = block_on(open_btree(dev, alloc)).unwrap();
    // Seed 32 keys spanning multiple leaves.
    block_on(
        idx.apply_batch(
            (0..32u32)
                .map(|i| Mutation::Insert {
                    key: key(i),
                    value: entry(i as u64),
                })
                .collect(),
        ),
    )
    .unwrap();
    // Overwrite every even key, delete every key divisible by 5.
    let mut muts: Vec<Mutation> = Vec::new();
    for i in 0..32u32 {
        if i % 5 == 0 {
            muts.push(Mutation::Delete { key: key(i) });
        } else if i % 2 == 0 {
            muts.push(Mutation::Insert {
                key: key(i),
                value: entry((i as u64) + 10_000),
            });
        }
    }
    block_on(idx.apply_batch(muts)).unwrap();
    for i in 0..32u32 {
        let got = block_on(idx.lookup(&key(i))).unwrap();
        if i % 5 == 0 {
            assert!(got.is_none(), "key {i} should be deleted, got {got:?}");
        } else if i % 2 == 0 {
            assert_eq!(
                got,
                Some(entry((i as u64) + 10_000)),
                "even key {i} should be overwritten",
            );
        } else {
            assert_eq!(
                got,
                Some(entry(i as u64)),
                "odd key {i} should be untouched",
            );
        }
    }
    // One more pass: reinsert the deleted keys with fresh values
    // so the path-copy code rebuilds previously-collapsed
    // subtrees.
    let muts: Vec<Mutation> = (0..32u32)
        .filter(|i| i % 5 == 0)
        .map(|i| Mutation::Insert {
            key: key(i),
            value: entry((i as u64) + 99_000),
        })
        .collect();
    block_on(idx.apply_batch(muts)).unwrap();
    for i in (0..32u32).step_by(5) {
        assert_eq!(
            block_on(idx.lookup(&key(i))).unwrap(),
            Some(entry((i as u64) + 99_000)),
            "key {i} reinsert lost",
        );
    }
}

// ---------------------------------------------------------------------------
// Concurrency + unwind coverage for the parallel commit write path.
//
// The two tests below pin down the contract of the within-commit
// write parallelism: (1) independent spine pages of a single commit
// are actually in flight simultaneously, and (2) a write failure
// anywhere in the group unwinds the *whole* commit, freeing every
// LBA it allocated and leaving the previously committed tree intact.
//
// `MockDevice` completes writes synchronously, so it can neither show
// overlap nor fail one chosen write mid-group. `GateDevice` wraps it
// to add exactly those two capabilities while delegating all storage
// to the inner mock.
// ---------------------------------------------------------------------------

/// A future that returns `Pending` exactly once before completing.
/// Under the busy-spin `block_on` above, this hands control back to
/// the surrounding `join_all` poll sweep, so every write submitted in
/// the same sweep is parked together and observed as concurrently in
/// flight.
struct YieldOnce(bool);

impl Future for YieldOnce {
    type Output = ();

    fn poll(mut self: std::pin::Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<()> {
        if self.0 {
            Poll::Ready(())
        } else {
            self.0 = true;
            Poll::Pending
        }
    }
}

/// `MockDevice` wrapper that (a) optionally holds each write in
/// flight across one re-poll so concurrent writes overlap and
/// `max_inflight` becomes observable, and (b) can fail a single write
/// chosen by sequence number to drive mid-commit unwind.
struct GateDevice {
    inner: MockDevice,
    inflight: std::cell::Cell<u32>,
    max_inflight: std::cell::Cell<u32>,
    writes: std::cell::Cell<u64>,
    pend: std::cell::Cell<bool>,
    fail_at: std::cell::Cell<Option<u64>>,
}

impl GateDevice {
    fn new(cfg: MockDeviceConfig) -> Self {
        Self {
            inner: MockDevice::new(cfg),
            inflight: std::cell::Cell::new(0),
            max_inflight: std::cell::Cell::new(0),
            writes: std::cell::Cell::new(0),
            pend: std::cell::Cell::new(false),
            fail_at: std::cell::Cell::new(None),
        }
    }

    /// Enable one-shot pending so concurrently submitted writes
    /// overlap instead of completing on first poll.
    fn set_pend(&self, on: bool) {
        self.pend.set(on);
    }

    /// Zero the in-flight high-water mark so a later commit can be
    /// measured without `open()`'s serial writes counting.
    fn reset_max_inflight(&self) {
        self.max_inflight.set(0);
    }

    fn max_inflight(&self) -> u32 {
        self.max_inflight.get()
    }

    /// Arrange for the write occurring `k` writes from now to return
    /// `Err`, simulating a device write failure partway through a
    /// commit.
    fn fail_after(&self, k: u64) {
        self.fail_at.set(Some(self.writes.get() + k));
    }
}

impl BlockDevice for GateDevice {
    fn page_size(&self) -> usize {
        self.inner.page_size()
    }

    fn capacity_pages(&self) -> u64 {
        self.inner.capacity_pages()
    }

    fn register_buffers(
        &self,
        base: *mut u8,
        len: usize,
    ) -> Result<(), crate::storage::types::Error> {
        self.inner.register_buffers(base, len)
    }

    fn write_queue_depth(&self) -> u32 {
        self.inner.write_queue_depth()
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), crate::storage::types::Error> {
        self.inner.read(lba, dst).await
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), crate::storage::types::Error> {
        let seq = self.writes.get() + 1;
        self.writes.set(seq);
        let n = self.inflight.get() + 1;
        self.inflight.set(n);
        if n > self.max_inflight.get() {
            self.max_inflight.set(n);
        }
        if self.pend.get() {
            YieldOnce(false).await;
        }
        let res = if self.fail_at.get() == Some(seq) {
            Err(crate::storage::types::Error::Io(5))
        } else {
            self.inner.write(lba, src).await
        };
        self.inflight.set(self.inflight.get() - 1);
        res
    }
}

fn open_gate(
    dev: Arc<GateDevice>,
    alloc: Arc<Allocator>,
) -> impl Future<Output = Result<BTreeIndex<GateDevice>, crate::storage::types::Error>> {
    open_gate_pool(dev, alloc, 64)
}

/// Like [`open_gate`] but with a caller-chosen scratch-pool size so a
/// test can deliberately undersize the pool relative to the number of
/// pages a single commit writes concurrently.
fn open_gate_pool(
    dev: Arc<GateDevice>,
    alloc: Arc<Allocator>,
    pool_pages: usize,
) -> impl Future<Output = Result<BTreeIndex<GateDevice>, crate::storage::types::Error>> {
    let scratch = ScratchPool::new(&*dev, 4096, pool_pages).expect("scratch pool");
    BTreeIndex::open(dev, alloc, scratch, 4096, false)
}

fn gate_dev(capacity_pages: u64) -> (Arc<GateDevice>, Arc<Allocator>) {
    let cfg = MockDeviceConfig {
        page_size: 4096,
        capacity_pages,
        ..Default::default()
    };
    (
        Arc::new(GateDevice::new(cfg)),
        Arc::new(Allocator::new(capacity_pages)),
    )
}

#[test]
fn commit_writes_independent_pages_concurrently() {
    let (dev, alloc) = gate_dev(512);
    let idx = block_on(open_gate(dev.clone(), alloc)).unwrap();
    // Park writes mid-flight and forget `open()`'s serial writes so
    // the high-water mark reflects only the batch below.
    dev.set_pend(true);
    dev.reset_max_inflight();
    // 200 entries exceed one leaf (cap 72) -> 3 leaves in a single
    // commit, which the parallel path must submit concurrently.
    let muts: Vec<_> = (0..200u32)
        .map(|i| Mutation::Insert {
            key: key(i),
            value: entry(1000 + i as u64),
        })
        .collect();
    block_on(idx.apply_batch(muts)).unwrap();
    assert!(
        dev.max_inflight() >= 2,
        "expected >1 btree page write in flight within one commit, got {}",
        dev.max_inflight(),
    );
    // Concurrency must not corrupt the result.
    for i in 0..200u32 {
        assert_eq!(
            block_on(idx.lookup(&key(i))).unwrap(),
            Some(entry(1000 + i as u64)),
            "key {i}",
        );
    }
}

#[test]
fn failed_write_mid_commit_unwinds_and_preserves_tree() {
    let (dev, alloc) = gate_dev(512);
    let idx = block_on(open_gate(dev.clone(), alloc.clone())).unwrap();
    // Baseline commit: a real tree the failed commit must not disturb.
    block_on(
        idx.apply_batch(
            (0..100u32)
                .map(|i| Mutation::Insert {
                    key: key(i),
                    value: entry(i as u64),
                })
                .collect(),
        ),
    )
    .unwrap();
    let good_txn = idx.current_txn();
    let good_root = idx.current_root();
    let used_before = alloc.used_pages();

    // Fail the second write of the next commit, after at least one
    // sibling page has already been written to disk.
    dev.fail_after(2);
    let muts: Vec<_> = (100..300u32)
        .map(|i| Mutation::Insert {
            key: key(i),
            value: entry(i as u64),
        })
        .collect();
    let res = block_on(idx.apply_batch(muts));
    assert!(res.is_err(), "commit must surface the device write error");

    // The committed tree is untouched: same txn, same root, no
    // entries from the failed batch, and every LBA the failed commit
    // allocated has been freed (no leak).
    assert_eq!(
        idx.current_txn(),
        good_txn,
        "txn advanced past a failed commit"
    );
    assert_eq!(
        idx.current_root(),
        good_root,
        "root advanced past a failed commit"
    );
    assert_eq!(
        alloc.used_pages(),
        used_before,
        "failed commit leaked allocator pages",
    );
    for i in 0..100u32 {
        assert_eq!(
            block_on(idx.lookup(&key(i))).unwrap(),
            Some(entry(i as u64)),
            "pre-commit key {i} lost",
        );
    }
    assert!(
        block_on(idx.lookup(&key(250))).unwrap().is_none(),
        "key from the failed commit must not be visible",
    );

    // The tree must still accept new commits after the failure.
    dev.fail_at.set(None);
    block_on(idx.apply_batch(vec![Mutation::Insert {
        key: key(500),
        value: entry(500),
    }]))
    .unwrap();
    assert_eq!(block_on(idx.lookup(&key(500))).unwrap(), Some(entry(500)));
}

// An overwrite that lands in several leaves drives the *incremental*
// path-copy (`recurse_internal`), which launches one `apply_node`
// future per touched child concurrently and stitches the results back
// by position. `commit_writes_independent_pages_concurrently` only
// covers the bulk-load (`build_tree`) path; this points real overlap
// at the path-copy path and checks the stitched result is correct.
#[test]
fn path_copy_writes_sibling_subtrees_concurrently() {
    let (dev, alloc) = gate_dev(512);
    let idx = block_on(open_gate(dev.clone(), alloc)).unwrap();
    // 200 entries exceed one leaf (cap 72) -> 3 leaves under a single
    // internal root. Sorted by key, the leaves cover roughly
    // [0..71], [72..143], [144..199].
    block_on(
        idx.apply_batch(
            (0..200u32)
                .map(|i| Mutation::Insert {
                    key: key(i),
                    value: entry(i as u64),
                })
                .collect(),
        ),
    )
    .unwrap();

    // Park writes mid-flight and forget the bulk load's writes so the
    // high-water mark reflects only the overwrite commit below.
    dev.set_pend(true);
    dev.reset_max_inflight();

    // One overwrite per leaf forces `recurse_internal` to copy all
    // three sibling subtrees in the same commit.
    block_on(idx.apply_batch(vec![
        Mutation::Insert {
            key: key(0),
            value: entry(9000),
        },
        Mutation::Insert {
            key: key(80),
            value: entry(9080),
        },
        Mutation::Insert {
            key: key(160),
            value: entry(9160),
        },
    ]))
    .unwrap();

    assert!(
        dev.max_inflight() >= 2,
        "expected concurrent sibling writes on the path-copy path, got {}",
        dev.max_inflight(),
    );

    // Overwritten keys reflect the new values...
    assert_eq!(block_on(idx.lookup(&key(0))).unwrap(), Some(entry(9000)));
    assert_eq!(block_on(idx.lookup(&key(80))).unwrap(), Some(entry(9080)));
    assert_eq!(block_on(idx.lookup(&key(160))).unwrap(), Some(entry(9160)));
    // ...and untouched keys in every leaf are preserved, so the
    // stitching did not drop or reorder a sibling subtree.
    for i in [1u32, 40, 71, 72, 100, 143, 144, 180, 199] {
        assert_eq!(
            block_on(idx.lookup(&key(i))).unwrap(),
            Some(entry(i as u64)),
            "untouched key {i} corrupted by concurrent path copy",
        );
    }
}

// The deadlock-free claim under an undersized scratch pool rests on
// `write_page` never holding its buffer across another `acquire`: a
// commit that writes more pages than the pool has buffers must simply
// drain in waves, never wedge. Open with a pool smaller than the
// number of concurrent writes a single commit issues and confirm the
// commit still completes (the busy-spin `block_on` panics on
// no-progress, so completion itself is the deadlock guard) with a
// correct result and an in-flight count bounded by the pool.
#[test]
fn commit_drains_in_waves_under_undersized_scratch_pool() {
    let (dev, alloc) = gate_dev(512);
    // Pool of 2 vs a 3-leaf commit: at least one write must wait for a
    // buffer that an in-flight, parked write still holds.
    let idx = block_on(open_gate_pool(dev.clone(), alloc, 2)).unwrap();
    dev.set_pend(true);
    dev.reset_max_inflight();

    block_on(
        idx.apply_batch(
            (0..200u32)
                .map(|i| Mutation::Insert {
                    key: key(i),
                    value: entry(i as u64),
                })
                .collect(),
        ),
    )
    .unwrap();

    // The leaf level wants 3 concurrent writes but the pool caps it at
    // 2, so overlap is reached (>= 2) yet never exceeds the pool size
    // (<= 2): the third write drained in a later wave.
    assert!(
        dev.max_inflight() >= 2,
        "expected overlap up to the pool size, got {}",
        dev.max_inflight(),
    );
    assert!(
        dev.max_inflight() <= 2,
        "in-flight writes exceeded the scratch-pool size, got {}",
        dev.max_inflight(),
    );

    // The wave-drained commit is correct.
    for i in 0..200u32 {
        assert_eq!(
            block_on(idx.lookup(&key(i))).unwrap(),
            Some(entry(i as u64)),
            "key {i}",
        );
    }
}
