// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategy, and the `run_workload` driver
//! that ties the executor, sim block device, and `StorageEngine`
//! together.
//!
//! Shape mirrors the bufferpool DST harness: a small `Workload`
//! struct describes the universe of operations, `client_strategy`
//! builds individual op sequences, and `run_workload` spawns one
//! task per client against a single shared engine, recording an
//! `Outcome` per op for invariant checking.

use std::cell::RefCell;
use std::rc::Rc;

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{BlockStore, PageRef, StripeKey};
use unbounded_storage::storage::blockdev::MockDeviceConfig;
use unbounded_storage::storage::{EngineConfig, StorageEngine};

use crate::framework::executor::{Executor, RunError, yield_once};
use crate::storage::mocks::{MockSimConfig, SimBlockDevice};
use crate::storage::oracle::Oracle;

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

/// Sized so a single run stays well under a second and shrinks
/// quickly. Bounds are tuned against the engine defaults overridden
/// inside `run_workload`.
#[derive(Clone, Debug)]
pub struct Workload {
    /// Cache + device page size in bytes. Constrained to a single
    /// value (4 KiB) because the engine equates cache page and
    /// btree page in this regime.
    pub page_size: usize,
    /// Total device capacity in pages. Must leave room for two
    /// btree meta pages plus all admitted writes.
    pub device_pages: u64,
    pub max_io_delay: u32,
    pub io_fault_rate: u32,
    /// Distinct stripe keys the workload may reference.
    pub key_count: u8,
    /// Distinct page offsets within each stripe.
    pub offset_count: u8,
    pub clients: Vec<ClientSpec>,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    pub ops: Vec<Op>,
}

#[derive(Clone, Debug)]
pub enum Op {
    Write {
        key_idx: u8,
        off_idx: u8,
        payload_seed: u8,
    },
    Read {
        key_idx: u8,
        off_idx: u8,
    },
}

impl Workload {
    pub fn key(&self, idx: u8) -> StripeKey {
        let b = idx % self.key_count.max(1);
        StripeKey([b; 32])
    }

    pub fn offset(&self, idx: u8) -> u64 {
        let n = self.offset_count.max(1) as u64;
        (idx as u64 % n) * self.page_size as u64
    }

    /// Deterministic byte pattern for `(key_idx, off_idx, seed)`.
    pub fn payload(&self, key_idx: u8, off_idx: u8, seed: u8) -> Vec<u8> {
        let mut out = vec![0u8; self.page_size];
        let mix = key_idx.wrapping_mul(31) ^ off_idx.wrapping_mul(17) ^ seed;
        for (i, b) in out.iter_mut().enumerate() {
            *b = (i as u8).wrapping_add(mix);
        }
        out
    }
}

// ---------------------------------------------------------------------------
// Proptest strategy.
// ---------------------------------------------------------------------------

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    let max_io_delay = 0u32..=3;
    let io_fault_rate = prop_oneof![
        9 => Just(0u32),
        1 => 1u32..=20,
    ];
    let key_count = 1u8..=3;
    let offset_count = 1u8..=3;
    let clients = vec(client_strategy(), 1..=4);
    (
        max_io_delay,
        io_fault_rate,
        key_count,
        offset_count,
        clients,
    )
        .prop_map(
            |(max_io_delay, io_fault_rate, key_count, offset_count, clients)| Workload {
                page_size: 4096,
                // 2 meta pages + budget for ~32 admitted writes plus
                // slack for eviction churn / fault-induced re-allocs.
                device_pages: 128,
                max_io_delay,
                io_fault_rate,
                key_count,
                offset_count,
                clients,
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    vec(op_strategy(), 1..=8).prop_map(|ops| ClientSpec { ops })
}

fn op_strategy() -> impl Strategy<Value = Op> {
    // 60% writes, 40% reads. Writes dominate because admission
    // requires a second touch before anything lands; a read-heavy
    // mix would barely exercise the data path.
    prop_oneof![
        6 => (any::<u8>(), any::<u8>(), any::<u8>())
            .prop_map(|(k, o, s)| Op::Write { key_idx: k, off_idx: o, payload_seed: s }),
        4 => (any::<u8>(), any::<u8>())
            .prop_map(|(k, o)| Op::Read { key_idx: k, off_idx: o }),
    ]
}

// ---------------------------------------------------------------------------
// Outcomes and report.
// ---------------------------------------------------------------------------

#[derive(Debug, Clone)]
#[allow(dead_code)]
pub enum Outcome {
    /// `write_page` returned `Ok(())`. The engine may have admitted,
    /// rejected, or coalesced this call; the workload only sees that
    /// the call did not blow up.
    WriteOk,
    /// `read_page` returned `Ok(true)` and `bytes` is what landed in
    /// the destination slot.
    ReadHit {
        key: StripeKey,
        offset: u64,
        bytes: Vec<u8>,
    },
    /// `read_page` returned `Ok(false)`.
    ReadMiss { key: StripeKey, offset: u64 },
    /// Some `Err` came back. Not allowed under happy-path settings;
    /// tolerated under fault injection.
    Err(String),
}

#[derive(Debug)]
#[allow(dead_code)]
pub struct RunReport {
    pub outcomes: Vec<Outcome>,
    pub steps: u64,
    pub hits: u64,
    pub misses: u64,
    pub admitted: u64,
    pub rejected_by_filter: u64,
    pub evictions: u64,
    pub write_io_errors: u64,
    pub read_io_errors: u64,
    pub checksum_misses: u64,
    pub resident_pages: usize,
    pub btree_entries: usize,
    pub device_reads: u64,
    pub device_writes: u64,
    pub device_io_errors: u64,
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

/// Drive `w` under `seed`. Returns the report so callers can assert
/// invariants. Panics only on framework setup errors that are not
/// "test failures" the caller should shrink against.
pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    // Pre-flatten ops so we know exactly how many pool slots we need
    // (one per op, so reads and writes never alias the same byte
    // range across an `await`).
    let mut flat: Vec<(usize, Op)> = Vec::new();
    for (cid, c) in w.clients.iter().enumerate() {
        for op in &c.ops {
            flat.push((cid, op.clone()));
        }
    }
    let pool_pages = flat.len().max(1) + 1;

    // Heap-allocated pool backing. The Box keeps it alive past
    // `exec.run` so engine reads/writes remain in bounds.
    let mut pool_buf: Box<[u8]> = vec![0u8; pool_pages * w.page_size].into_boxed_slice();
    let pool_base: *mut u8 = pool_buf.as_mut_ptr();

    // Device + sim config.
    let sim_cfg = MockSimConfig::new();
    sim_cfg.max_io_delay.set(w.max_io_delay);
    sim_cfg.io_fault_rate.set(w.io_fault_rate);
    let device = std::sync::Arc::new(SimBlockDevice::new(
        MockDeviceConfig {
            page_size: w.page_size,
            capacity_pages: w.device_pages,
            ..Default::default()
        },
        sim_cfg.clone(),
    ));

    let engine_cfg = EngineConfig {
        page_size_bytes: w.page_size,
        btree_page_bytes: w.page_size,
        ..EngineConfig::default()
    };

    // Shared slot: clients spin on it until the bootstrap task
    // either installs an engine (`Some(Some(_))`) or marks itself
    // failed (`Some(None)`). The two-level Option distinguishes
    // "not ready yet" from "open failed; abort".
    type EngineRc = Rc<StorageEngine<SimBlockDevice>>;
    let slot: Rc<RefCell<Option<Option<EngineRc>>>> = Rc::new(RefCell::new(None));

    let oracle = Rc::new(Oracle::new());
    let outcomes: Rc<RefCell<Vec<Outcome>>> = Rc::new(RefCell::new(Vec::new()));

    let mut exec = Executor::new(seed);

    // Bootstrap: open engine with faults temporarily disabled
    // (the engine has no recovery contract under torn opens yet),
    // register pages, publish in slot, then restore the configured
    // fault rate so client-time I/Os exercise the fault path.
    {
        let slot = slot.clone();
        let device = device.clone();
        let outcomes = outcomes.clone();
        let sim_cfg = sim_cfg.clone();
        let configured_delay = w.max_io_delay;
        let configured_faults = w.io_fault_rate;
        exec.spawn(async move {
            sim_cfg.max_io_delay.set(0);
            sim_cfg.io_fault_rate.set(0);
            let open_res = StorageEngine::open(device, engine_cfg).await;
            let eng = match open_res {
                Ok(e) => e,
                Err(e) => {
                    outcomes
                        .borrow_mut()
                        .push(Outcome::Err(format!("open: {e}")));
                    *slot.borrow_mut() = Some(None);
                    return;
                }
            };
            // SAFETY: `pool_buf` outlives `exec.run` because it is
            // dropped only after the executor returns. The engine
            // only reads/writes the slice during this `run`.
            if let Err(e) = eng.register_pages(pool_base, w.page_size, pool_pages) {
                outcomes
                    .borrow_mut()
                    .push(Outcome::Err(format!("register: {e}")));
                *slot.borrow_mut() = Some(None);
                return;
            }
            sim_cfg.max_io_delay.set(configured_delay);
            sim_cfg.io_fault_rate.set(configured_faults);
            *slot.borrow_mut() = Some(Some(Rc::new(eng)));
        });
    }

    // Client tasks: one per ClientSpec; each runs its op sequence
    // serially. Concurrency across clients is what the executor
    // randomizes.
    for (cid, c) in w.clients.iter().cloned().enumerate() {
        // Pre-compute the slot index for each op so the closure
        // doesn't need to know about `flat`.
        let mut op_slots: Vec<usize> = Vec::with_capacity(c.ops.len());
        for (i, (this_cid, _)) in flat.iter().enumerate() {
            if *this_cid == cid {
                op_slots.push(i);
            }
        }
        let slot = slot.clone();
        let outcomes = outcomes.clone();
        let oracle = oracle.clone();
        let w = w.clone();
        let page_size = w.page_size;
        let pool_base_v = pool_base as usize; // make Send across the async move
        exec.spawn(async move {
            // Wait for bootstrap to publish the engine or abort.
            let eng = loop {
                if let Some(opt) = slot.borrow().clone() {
                    match opt {
                        Some(e) => break e,
                        None => return, // bootstrap failed; nothing to do.
                    }
                }
                yield_once().await;
            };
            for (i, op) in c.ops.iter().enumerate() {
                let pool_slot = op_slots[i];
                match op {
                    Op::Write {
                        key_idx,
                        off_idx,
                        payload_seed,
                    } => {
                        let key = w.key(*key_idx);
                        let offset = w.offset(*off_idx);
                        let bytes = w.payload(*key_idx, *off_idx, *payload_seed);
                        // SAFETY: each op owns a unique pool slot,
                        // so no other task writes here concurrently.
                        unsafe {
                            let p = (pool_base_v as *mut u8).add(pool_slot * page_size);
                            std::ptr::copy_nonoverlapping(bytes.as_ptr(), p, page_size);
                        }
                        let page = PageRef {
                            page_idx: pool_slot as u32,
                            offset: 0,
                            len: page_size as u32,
                        };
                        oracle.record_write(key, offset, bytes);
                        match eng.write_page(key, offset, page).await {
                            Ok(()) => outcomes.borrow_mut().push(Outcome::WriteOk),
                            Err(e) => outcomes
                                .borrow_mut()
                                .push(Outcome::Err(format!("write: {e}"))),
                        }
                    }
                    Op::Read { key_idx, off_idx } => {
                        let key = w.key(*key_idx);
                        let offset = w.offset(*off_idx);
                        // Zero the destination slot so a partial /
                        // skipped fill is visible to the oracle
                        // check.
                        unsafe {
                            let p = (pool_base_v as *mut u8).add(pool_slot * page_size);
                            std::ptr::write_bytes(p, 0, page_size);
                        }
                        let page = PageRef {
                            page_idx: pool_slot as u32,
                            offset: 0,
                            len: page_size as u32,
                        };
                        match eng.read_page(key, offset, page).await {
                            Ok(true) => {
                                let bytes = unsafe {
                                    let p = (pool_base_v as *const u8).add(pool_slot * page_size);
                                    std::slice::from_raw_parts(p, page_size).to_vec()
                                };
                                outcomes
                                    .borrow_mut()
                                    .push(Outcome::ReadHit { key, offset, bytes });
                            }
                            Ok(false) => outcomes
                                .borrow_mut()
                                .push(Outcome::ReadMiss { key, offset }),
                            Err(e) => outcomes
                                .borrow_mut()
                                .push(Outcome::Err(format!("read: {e}"))),
                        }
                    }
                }
            }
        });
    }

    // Generous step budget: per op we expect O(delay) yields,
    // multiplied by the typical handful of awaits the engine does
    // per call (admission, singleflight, device.write, btree
    // apply_batch which itself reads + writes the device).
    let total_ops = flat.len() as u64;
    let step_budget =
        4096 + total_ops * (w.max_io_delay as u64 + 4) * 64 * (1 + w.io_fault_rate as u64 / 4);

    exec.run(step_budget)?;

    // Snapshot the engine for invariant assertions. If bootstrap
    // aborted (open failed under fault injection) the engine never
    // existed, so report zero counters; that path still satisfies
    // every invariant by construction.
    let (
        hits,
        misses,
        admitted,
        rejected_by_filter,
        evictions,
        write_io_errors,
        read_io_errors,
        checksum_misses,
        resident_pages,
        btree_entries,
    ) = match slot.borrow().clone() {
        Some(Some(eng)) => {
            let s = eng.snapshot();
            (
                s.hits,
                s.misses,
                s.admitted,
                s.rejected_by_filter,
                s.evictions,
                s.write_io_errors,
                s.read_io_errors,
                s.checksum_misses,
                s.resident_pages,
                s.btree_entries,
            )
        }
        _ => (0, 0, 0, 0, 0, 0, 0, 0, 0, 0),
    };

    let outcomes = Rc::try_unwrap(outcomes)
        .map_err(|_| RunError::Deadlock)
        .expect("all tasks completed; outcomes Rc must be unique")
        .into_inner();

    // Hold `pool_buf` alive until here (it would have been dropped
    // already if we'd let the compiler reorder it).
    drop(pool_buf);
    // `oracle` and `slot` are dropped naturally at end of scope.
    let _ = oracle;

    Ok(RunReport {
        outcomes,
        steps: exec.last_steps(),
        hits,
        misses,
        admitted,
        rejected_by_filter,
        evictions,
        write_io_errors,
        read_io_errors,
        checksum_misses,
        resident_pages,
        btree_entries,
        device_reads: device.reads(),
        device_writes: device.writes(),
        device_io_errors: device.io_errors(),
    })
}
