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
                1,
                "cache must route both internal levels"
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
                1,
                "retry must still route through the unchanged parent cache",
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
