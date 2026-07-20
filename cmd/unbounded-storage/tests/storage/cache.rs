// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Scripted DST coverage for persistent B+tree internal caches.

use std::collections::BTreeMap;
use std::rc::Rc;
use std::sync::Arc;

use unbounded_storage::storage::blockdev::{BlockDevice, MockDeviceConfig, ScratchPool};
use unbounded_storage::storage::types::{Checksum, Lba, PageKey};
use unbounded_storage::storage::{Allocator, BTreeIndex, LeafEntry, Mutation};

use crate::framework::executor::Executor;
use crate::storage::mocks::{MockSimConfig, SimBlockDevice};

const PAGE_SIZE: usize = 512;
const ENTRIES: u32 = 120;
const HEADER_LEN: usize = 32;
const INTERNAL_ENTRY_LEN: usize = 44;
const INTERNAL_CHILD_OFFSET: usize = 36;

fn key(i: u32) -> PageKey {
    let mut hash = [0u8; 32];
    hash[..4].copy_from_slice(&i.to_be_bytes());
    PageKey::new(hash, 0)
}

fn entry(i: u64) -> LeafEntry {
    LeafEntry {
        lba: Lba(400 + i),
        data_checksum: Checksum(i.wrapping_mul(0x9e37_79b9)),
        byte_len: PAGE_SIZE as u32,
    }
}

async fn open(
    device: Arc<SimBlockDevice>,
    allocator: Arc<Allocator>,
    force_format: bool,
) -> BTreeIndex<SimBlockDevice> {
    let scratch = ScratchPool::new(&*device, PAGE_SIZE, 16).expect("scratch pool");
    BTreeIndex::open(device, allocator, scratch, PAGE_SIZE, false, force_format)
        .await
        .expect("open btree")
}

async fn assert_exact(index: &BTreeIndex<SimBlockDevice>, expected: &BTreeMap<PageKey, LeafEntry>) {
    for i in 0..ENTRIES {
        let k = key(i);
        assert_eq!(index.lookup(&k).await.unwrap(), expected.get(&k).copied());
        assert_eq!(index.lookup_committed_mirror(&k), expected.get(&k).copied());
    }
}

fn internal_children(device: &SimBlockDevice, lba: Lba) -> Vec<Lba> {
    let mut page = vec![0u8; PAGE_SIZE];
    device.peek(lba, &mut page);
    assert_eq!(page[0], 2, "LBA {} must hold an internal page", lba.0);
    let count = u16::from_le_bytes([page[2], page[3]]) as usize;
    (0..count)
        .map(|i| {
            let start = HEADER_LEN + i * INTERNAL_ENTRY_LEN + INTERNAL_CHILD_OFFSET;
            Lba(u64::from_le_bytes(
                page[start..start + 8].try_into().unwrap(),
            ))
        })
        .collect()
}

#[test]
fn deep_cache_failure_is_atomic_and_reopens_exactly() {
    let sim_cfg = MockSimConfig::new();
    sim_cfg.max_io_delay.set(3);
    let device = Arc::new(SimBlockDevice::new(
        MockDeviceConfig {
            page_size: PAGE_SIZE,
            capacity_pages: 1024,
            ..Default::default()
        },
        sim_cfg,
    ));
    let result = Rc::new(std::cell::RefCell::new(None));
    let mut exec = Executor::new(0xCA_C4E5_5A11);

    {
        let device = device.clone();
        let result = result.clone();
        exec.spawn(Box::pin(async move {
            let allocator = Arc::new(Allocator::new(device.capacity_pages()));
            let index = open(device.clone(), allocator.clone(), true).await;
            let mut expected = BTreeMap::new();
            let initial: Vec<_> = (0..ENTRIES)
                .map(|i| {
                    let value = entry(i as u64);
                    expected.insert(key(i), value);
                    Mutation::Insert { key: key(i), value }
                })
                .collect();
            index.apply_batch(initial).await.unwrap();

            // At 512 bytes a leaf holds 8 entries and an internal page
            // holds 10 children. 120 entries therefore force two internal
            // levels. A one-key path copy writes leaf, branch, root, meta.
            let reads_before = device.reads();
            let txn_before = index.current_txn();
            let root_before = index.current_root();
            let used_before = allocator.used_pages();
            let replacement = entry(10_000);
            device.fail_write_after(4);
            let failed = index
                .apply_batch(vec![Mutation::Insert {
                    key: key(37),
                    value: replacement,
                }])
                .await;
            assert!(failed.is_err(), "targeted meta write must fail");
            assert_eq!(
                device.reads() - reads_before,
                4,
                "path-copy reads one leaf, then validates all candidate internals"
            );
            assert_eq!(index.current_txn(), txn_before);
            assert_eq!(index.current_root(), root_before);
            assert_eq!(allocator.used_pages(), used_before);
            assert_exact(&index, &expected).await;

            let reads_before_retry = device.reads();
            index
                .apply_batch(vec![Mutation::Insert {
                    key: key(37),
                    value: replacement,
                }])
                .await
                .unwrap();
            assert_eq!(
                device.reads() - reads_before_retry,
                4,
                "retry must route through the cache and validate before publication",
            );
            expected.insert(key(37), replacement);
            assert_exact(&index, &expected).await;
            drop(index);

            let reopened = open(
                device.clone(),
                Arc::new(Allocator::new(device.capacity_pages())),
                false,
            )
            .await;
            assert_exact(&reopened, &expected).await;
            reopened
                .apply_batch(vec![Mutation::Delete { key: key(82) }])
                .await
                .unwrap();
            expected.remove(&key(82));
            assert_exact(&reopened, &expected).await;
            *result.borrow_mut() = Some(());
        }));
    }

    exec.run(2_000_000)
        .expect("executor finished without deadlock or budget exhaustion");
    assert_eq!(*result.borrow(), Some(()));
}

#[test]
fn reopen_rejects_an_incomplete_internal_cache() {
    let sim_cfg = MockSimConfig::new();
    let device = Arc::new(SimBlockDevice::new(
        MockDeviceConfig {
            page_size: PAGE_SIZE,
            capacity_pages: 1024,
            ..Default::default()
        },
        sim_cfg,
    ));
    let result = Rc::new(std::cell::RefCell::new(None));
    let mut exec = Executor::new(0x1C_C4_C4E);

    {
        let device = device.clone();
        let result = result.clone();
        exec.spawn(Box::pin(async move {
            let index = open(
                device.clone(),
                Arc::new(Allocator::new(device.capacity_pages())),
                true,
            )
            .await;
            let expected: BTreeMap<_, _> =
                (0..ENTRIES).map(|i| (key(i), entry(i as u64))).collect();
            index
                .apply_batch(
                    expected
                        .iter()
                        .map(|(&key, &value)| Mutation::Insert { key, value })
                        .collect(),
                )
                .await
                .unwrap();

            let branches = internal_children(&device, index.current_root());
            assert!(branches.len() >= 2, "test requires a two-level tree");
            let omitted_branch = *branches.last().unwrap();
            assert!(!internal_children(&device, omitted_branch).is_empty());
            drop(index);

            // Cache construction is the first tree walk after the two meta
            // reads. A one-shot branch fault must reject that meta generation
            // and use the leaf-scan rebuild, not publish a partial cache.
            device.fail_next_read(omitted_branch);
            let reopened = open(
                device.clone(),
                Arc::new(Allocator::new(device.capacity_pages())),
                false,
            )
            .await;
            assert_eq!(device.io_errors(), 1);
            assert_exact(&reopened, &expected).await;

            // The rebuilt snapshot has a complete cache, so corruption in an
            // untouched branch is still detected before either meta changes.
            let rebuilt_branches = internal_children(&device, reopened.current_root());
            let corrupt_branch = *rebuilt_branches.last().unwrap();
            device.poke(corrupt_branch, &vec![0xff; PAGE_SIZE]);
            let txn_before = reopened.current_txn();
            let commit = reopened
                .apply_batch(vec![Mutation::Insert {
                    key: key(0),
                    value: entry(10_000),
                }])
                .await;
            assert!(commit.is_err());
            assert_eq!(reopened.current_txn(), txn_before);
            *result.borrow_mut() = Some(());
        }));
    }

    exec.run(2_000_000)
        .expect("executor finished without deadlock or budget exhaustion");
    assert_eq!(*result.borrow(), Some(()));
}
