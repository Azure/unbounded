// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Scripted snapshot-lifetime scenarios for the storage engine.

use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Arc;

use unbounded_storage::bufferpool::{BlockStore, PageRef, StripeKey};
use unbounded_storage::memory::Backing;
use unbounded_storage::storage::blockdev::{BlockDevice, MockDeviceConfig};
use unbounded_storage::storage::types::{Lba, PageKey};
use unbounded_storage::storage::{EngineConfig, StorageEngine};

use crate::framework::executor::{Executor, yield_once};
use crate::storage::mocks::{MockSimConfig, SimBlockDevice};

const PAGE_SIZE: usize = 4096;
const PAGE_TYPE_OFFSET: usize = 0;
const PAGE_TYPE_LEAF: u8 = 1;
const NENTRIES_OFFSET: usize = 2;
const TXN_ID_RANGE: std::ops::Range<usize> = 8..16;
const HEADER_LEN: usize = 32;
const KEY_LEN: usize = 36;
const LEAF_ENTRY_LEN: usize = 56;

fn backing(base: *mut u8, page_size: usize, page_count: usize) -> Backing {
    Backing {
        base,
        page_size,
        page_count,
        keepalive: std::sync::Arc::new(()),
    }
}

fn engine_cfg() -> EngineConfig {
    EngineConfig {
        page_size_bytes: PAGE_SIZE,
        btree_page_bytes: PAGE_SIZE,
        ..EngineConfig::default()
    }
}

fn payload(key_idx: u8, off_idx: u8, seed: u8) -> Vec<u8> {
    let mut out = vec![0u8; PAGE_SIZE];
    let mix = key_idx.wrapping_mul(31) ^ off_idx.wrapping_mul(17) ^ seed;
    for (i, b) in out.iter_mut().enumerate() {
        *b = (i as u8).wrapping_add(mix);
    }
    out
}

fn stripe(i: u8) -> StripeKey {
    let mut s = [0u8; 32];
    s[0] = i;
    StripeKey(s)
}

fn page_key(key: StripeKey, offset: u64) -> PageKey {
    PageKey::new(key.0, (offset / PAGE_SIZE as u64) as u32)
}

struct Pool {
    buf: Box<[u8]>,
}

impl Pool {
    fn new(page_count: usize) -> Self {
        Self {
            buf: vec![0u8; page_count * PAGE_SIZE].into_boxed_slice(),
        }
    }

    fn base(&mut self) -> *mut u8 {
        self.buf.as_mut_ptr()
    }
}

async fn admit_one<B>(
    eng: &StorageEngine<B>,
    pool_base: *mut u8,
    pool_slot: usize,
    key: StripeKey,
    offset: u64,
    bytes: &[u8],
) where
    B: BlockDevice,
{
    unsafe {
        let p = pool_base.add(pool_slot * PAGE_SIZE);
        std::ptr::copy_nonoverlapping(bytes.as_ptr(), p, PAGE_SIZE);
    }
    let page = PageRef {
        page_idx: pool_slot as u32,
        offset: 0,
        len: PAGE_SIZE as u32,
    };
    eng.write_page(&key, offset, page).await.unwrap();
    eng.write_page(&key, offset, page).await.unwrap();
}

fn find_leaf_lba_for_key(device: &SimBlockDevice, key: PageKey) -> Option<u64> {
    let mut buf = vec![0u8; PAGE_SIZE];
    let mut best: Option<(u64, u64)> = None;
    let max_entries = (PAGE_SIZE - HEADER_LEN) / LEAF_ENTRY_LEN;

    for lba in 2..device.capacity_pages() {
        device.peek(Lba(lba), &mut buf);
        if buf[PAGE_TYPE_OFFSET] != PAGE_TYPE_LEAF {
            continue;
        }
        let nentries =
            u16::from_le_bytes([buf[NENTRIES_OFFSET], buf[NENTRIES_OFFSET + 1]]) as usize;
        if nentries > max_entries {
            continue;
        }
        let contains_key = (0..nentries).any(|i| {
            let base = HEADER_LEN + i * LEAF_ENTRY_LEN;
            PageKey::decode(&buf[base..base + KEY_LEN]) == Some(key)
        });
        if !contains_key {
            continue;
        }
        let txn = u64::from_le_bytes(buf[TXN_ID_RANGE].try_into().unwrap());
        if best.map(|(_, best_txn)| txn > best_txn).unwrap_or(true) {
            best = Some((lba, txn));
        }
    }

    best.map(|(lba, _)| lba)
}

#[test]
fn lookup_pins_snapshot_until_leaf_read_finishes() {
    const ENTRIES: usize = 80;
    const TARGET: u8 = 0;
    const REWRITE: u8 = 1;
    const OTHER_LEAF: u8 = 79;
    const READ_SLOT: usize = ENTRIES;
    const WRITE_SLOT: usize = ENTRIES + 1;

    let sim_cfg = MockSimConfig::new();
    sim_cfg.max_io_delay.set(0);
    sim_cfg.io_fault_rate.set(0);
    sim_cfg.read_corrupt_rate.set(0);
    let device = Arc::new(SimBlockDevice::new(
        MockDeviceConfig {
            page_size: PAGE_SIZE,
            capacity_pages: 512,
            ..Default::default()
        },
        sim_cfg,
    ));

    let mut pool = Pool::new(ENTRIES + 2);
    let pool_base = pool.base() as usize;
    let expected = payload(TARGET, 0, 10);

    type Engine = StorageEngine<SimBlockDevice>;
    enum Stage {
        Pending,
        Failed,
        Ready(Arc<Engine>),
    }

    let stage: Rc<RefCell<Stage>> = Rc::new(RefCell::new(Stage::Pending));
    let phase = Rc::new(Cell::new(0u8));
    let reader_done = Rc::new(Cell::new(false));
    let writer_done = Rc::new(Cell::new(false));
    let result: Rc<RefCell<Option<(bool, Vec<u8>)>>> = Rc::new(RefCell::new(None));
    let mut exec = Executor::new(0x5107_BA11);

    {
        let stage = stage.clone();
        let device = device.clone();
        exec.spawn(Box::pin(async move {
            let eng = match StorageEngine::open(device, engine_cfg()).await {
                Ok(e) => Arc::new(e),
                Err(_) => {
                    *stage.borrow_mut() = Stage::Failed;
                    return;
                }
            };
            if eng
                .register_pages(&backing(pool_base as *mut u8, PAGE_SIZE, ENTRIES + 2))
                .is_err()
            {
                *stage.borrow_mut() = Stage::Failed;
                return;
            }
            *stage.borrow_mut() = Stage::Ready(eng);
        }));
    }

    {
        let stage = stage.clone();
        exec.spawn(Box::pin(async move {
            let eng = loop {
                match &*stage.borrow() {
                    Stage::Pending => {}
                    Stage::Failed => return,
                    Stage::Ready(e) => break e.clone(),
                }
                yield_once().await;
            };
            eng.run_mutator().await;
        }));
    }

    {
        let stage = stage.clone();
        let phase = phase.clone();
        let reader_done = reader_done.clone();
        let result = result.clone();
        exec.spawn(Box::pin(async move {
            let eng = loop {
                match &*stage.borrow() {
                    Stage::Pending => {}
                    Stage::Failed => {
                        reader_done.set(true);
                        return;
                    }
                    Stage::Ready(e) => break e.clone(),
                }
                yield_once().await;
            };
            while phase.get() < 1 {
                yield_once().await;
            }
            unsafe {
                let p = (pool_base as *mut u8).add(READ_SLOT * PAGE_SIZE);
                std::ptr::write_bytes(p, 0, PAGE_SIZE);
            }
            let dst = PageRef {
                page_idx: READ_SLOT as u32,
                offset: 0,
                len: PAGE_SIZE as u32,
            };
            let hit = eng.read_page(&stripe(TARGET), 0, dst).await.unwrap();
            let bytes = if hit {
                let p = unsafe { (pool_base as *const u8).add(READ_SLOT * PAGE_SIZE) };
                unsafe { std::slice::from_raw_parts(p, PAGE_SIZE) }.to_vec()
            } else {
                Vec::new()
            };
            *result.borrow_mut() = Some((hit, bytes));
            reader_done.set(true);
        }));
    }

    {
        let stage = stage.clone();
        let phase = phase.clone();
        let writer_done = writer_done.clone();
        exec.spawn(Box::pin(async move {
            let eng = loop {
                match &*stage.borrow() {
                    Stage::Pending => {}
                    Stage::Failed => {
                        writer_done.set(true);
                        return;
                    }
                    Stage::Ready(e) => break e.clone(),
                }
                yield_once().await;
            };
            while phase.get() < 2 {
                yield_once().await;
            }
            admit_one(
                &eng,
                pool_base as *mut u8,
                WRITE_SLOT,
                stripe(REWRITE),
                0,
                &payload(REWRITE, 0, 20),
            )
            .await;
            admit_one(
                &eng,
                pool_base as *mut u8,
                WRITE_SLOT,
                stripe(OTHER_LEAF),
                0,
                &payload(OTHER_LEAF, 0, 30),
            )
            .await;
            writer_done.set(true);
        }));
    }

    {
        let stage = stage.clone();
        let phase = phase.clone();
        let reader_done = reader_done.clone();
        let writer_done = writer_done.clone();
        let device = device.clone();
        exec.spawn(Box::pin(async move {
            let eng = loop {
                match &*stage.borrow() {
                    Stage::Pending => {}
                    Stage::Failed => return,
                    Stage::Ready(e) => break e.clone(),
                }
                yield_once().await;
            };

            for i in 0..ENTRIES {
                let bytes = payload(i as u8, 0, 10);
                admit_one(&eng, pool_base as *mut u8, i, stripe(i as u8), 0, &bytes).await;
            }
            assert!(
                eng.snapshot().btree_lookup_cache_bytes > 0,
                "test requires an internal-node cache",
            );

            let target_leaf = find_leaf_lba_for_key(&device, page_key(stripe(TARGET), 0))
                .expect("target key lives in a leaf");
            let rewrite_leaf = find_leaf_lba_for_key(&device, page_key(stripe(REWRITE), 0))
                .expect("rewrite key lives in a leaf");
            let other_leaf = find_leaf_lba_for_key(&device, page_key(stripe(OTHER_LEAF), 0))
                .expect("other key lives in a leaf");
            assert_eq!(
                target_leaf, rewrite_leaf,
                "rewrite must retire the leaf containing the target key",
            );
            assert_ne!(
                target_leaf, other_leaf,
                "second metadata commit must rewrite a different leaf",
            );

            let pause = device.pause_next_read(Lba(target_leaf));
            phase.set(1);
            while !pause.paused() {
                yield_once().await;
            }
            phase.set(2);
            while !writer_done.get() {
                yield_once().await;
            }
            pause.release();
            while !reader_done.get() {
                yield_once().await;
            }
            eng.close_mutator();
        }));
    }

    exec.run(1_000_000)
        .expect("executor finished without deadlock or budget exhaustion");

    let (hit, bytes) = result.borrow().clone().expect("reader populated result");
    assert!(
        hit,
        "lookup lost the target key while its snapshot leaf was reclaimed and reused",
    );
    assert_eq!(
        bytes, expected,
        "lookup returned bytes from a different snapshot generation",
    );
}
