// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategy, and the `run_workload` driver
//! that ties the executor, sim block devices, and per-disk
//! `StorageEngine` instances together behind a `LocalStorage` router.
//!
//! Shape mirrors the bufferpool DST harness: a small `Workload`
//! struct describes the universe of operations, `client_strategy`
//! builds individual op sequences, and `run_workload` spawns one
//! task per client against a shared `LocalStorage` over `num_disks`
//! engines, recording an `Outcome` per op for invariant checking.
//!
//! Routing across disks is invisible to the workload semantics: the
//! `Outcome` enum carries only the logical `(key, offset)` and the
//! bytes observed, so the oracle stays disk-agnostic. The per-disk
//! `device_writes_per_disk` aggregate in `RunReport` is the only
//! externally observable signal that routing actually fanned out.

use std::cell::{Cell, RefCell};
use std::rc::Rc;
use std::sync::Arc;

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{BlockStore, PageRef, StripeKey};
use unbounded_storage::storage::blockdev::MockDeviceConfig;
use unbounded_storage::storage::{EngineConfig, LocalStorage, StorageEngine};

use crate::framework::executor::{Executor, RunError, yield_once};
use crate::storage::mocks::{MockSimConfig, SimBlockDevice};
use crate::storage::oracle::Oracle;

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

type LocalRc = Rc<LocalStorage<SimBlockDevice>>;

/// Handoff state from the bootstrap task to the client tasks.
/// `Pending` means bootstrap has not yet published a result; clients
/// yield. `Failed` means the initial open aborted; clients exit
/// without running their op sequences. `Ready` carries the shared
/// `LocalStorage` router clients drive.
enum BootstrapStatus {
    Pending,
    Failed,
    Ready(LocalRc),
}

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
    /// btree meta pages plus all admitted writes. The strategy
    /// deliberately samples small values so the LRU watermark
    /// fires and the eviction path is exercised; invariants in
    /// `tests.rs` rely on this to make eviction reachable.
    pub device_pages: u64,
    pub max_io_delay: u32,
    pub io_fault_rate: u32,
    /// Probability `[0, 100]` that a successful device read silently
    /// corrupts its first byte. The engine's xxh3 page checksum
    /// must convert this into a miss; we never want a `ReadHit`
    /// that returns corrupted bytes.
    pub read_corrupt_rate: u32,
    /// Distinct stripe keys the workload may reference.
    pub key_count: u8,
    /// Distinct page offsets within each stripe.
    pub offset_count: u8,
    pub clients: Vec<ClientSpec>,
    /// Optional shorter byte length for selected grid cells. When
    /// `Some(n)`, a deterministic subset of `(key, offset)` slots
    /// stores `n` bytes instead of a full `page_size` payload, so
    /// the engine writes a short trailing-page value through
    /// `PageRef.len < page_size`. Exercises `LeafEntry.byte_len`
    /// round-trip and the page checksum's coverage of the variable
    /// tail. `None` keeps every slot at full `page_size`, which is
    /// the legacy behavior.
    pub short_page_byte_len: Option<u32>,
    /// If true, after the main workload completes, drop the engines
    /// and reopen one engine per surviving sim device, then
    /// read-replay every `(key, offset)` in the grid. Verifies the
    /// restart contract: previously-admitted reads either still hit
    /// with correct bytes, OR miss; they must never return wrong
    /// bytes.
    pub restart_after: bool,
    /// Number of simulated disks that back the `LocalStorage`
    /// router. `1` matches the original single-engine harness
    /// exactly; `>= 2` exercises the cross-disk routing path
    /// (`disk_for(value_hash, page_index) mod num_disks`).
    pub num_disks: u32,
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
    /// Length matches `self.byte_len(key_idx, off_idx)` so the
    /// payload and the engine's recorded `LeafEntry.byte_len` agree
    /// on a per-slot basis.
    pub fn payload(&self, key_idx: u8, off_idx: u8, seed: u8) -> Vec<u8> {
        let len = self.byte_len(key_idx, off_idx);
        let mut out = vec![0u8; len];
        let mix = key_idx.wrapping_mul(31) ^ off_idx.wrapping_mul(17) ^ seed;
        for (i, b) in out.iter_mut().enumerate() {
            *b = (i as u8).wrapping_add(mix);
        }
        out
    }

    /// Logical byte length stored at `(key_idx, off_idx)`. Returns
    /// `page_size` for every slot when `short_page_byte_len` is
    /// `None`. Otherwise a deterministic subset of slots returns
    /// the configured short length; the subset is a pure function
    /// of the normalized `(key_idx, off_idx)` pair so writes and
    /// reads against the same slot always agree on length.
    pub fn byte_len(&self, key_idx: u8, off_idx: u8) -> usize {
        match self.short_page_byte_len {
            Some(n) if self.slot_is_short(key_idx, off_idx) => n as usize,
            _ => self.page_size,
        }
    }

    fn slot_is_short(&self, key_idx: u8, off_idx: u8) -> bool {
        let k = key_idx % self.key_count.max(1);
        let o = off_idx % self.offset_count.max(1);
        // Roughly one in three slots is short. The mix uses two
        // small primes so adjacent (key, offset) pairs do not all
        // share the same fate, but the function stays trivially
        // reproducible.
        (k as u16 * 3 + o as u16 * 5) % 3 == 0
    }
}

// ---------------------------------------------------------------------------
// Proptest strategy.
// ---------------------------------------------------------------------------

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    // Skew toward non-zero delay so the executor actually has
    // permission to interleave I/Os from different clients on the
    // same disk. A delay of zero collapses every device call to a
    // single yield-free await, which hides the concurrency the
    // mutator + singleflight machinery exists to handle. The
    // `0` arm stays in the mix so the no-latency regime is still
    // covered.
    let max_io_delay = prop_oneof![
        1 => Just(0u32),
        9 => 1u32..=8,
    ];
    let io_fault_rate = prop_oneof![
        9 => Just(0u32),
        1 => 1u32..=20,
    ];
    let read_corrupt_rate = prop_oneof![
        9 => Just(0u32),
        1 => 1u32..=20,
    ];
    let key_count = 1u8..=3;
    let offset_count = 1u8..=3;
    // Two clients is the minimum that can produce in-flight
    // concurrency on a shared engine; bump the floor so the
    // concurrency invariants below have something to assert
    // against on most runs.
    let clients = vec(client_strategy(), 2..=4);
    let restart_after = prop_oneof![Just(false), Just(true)];
    // `1` keeps coverage of the single-engine codepath; `2..=4`
    // exercises cross-disk routing without blowing up the per-run
    // open cost.
    let num_disks = 1u32..=4;
    // Mix small and roomy capacities. The small end (down to 16
    // pages) deliberately drives utilization above the LRU
    // watermark so eviction fires; the large end keeps the
    // original head-room regime where eviction is rare.
    let device_pages = prop_oneof![
        2 => 16u64..=32,
        2 => 33u64..=64,
        1 => Just(128u64),
    ];
    // The engine now requires every write length to be a positive
    // multiple of `btree_page_bytes` (see `write_page_from`), so
    // the DST sweep only generates full-page writes. The field is
    // retained on `Workload` so hand-rolled regression cases still
    // compile, but the strategy always picks `None`.
    let short_page_byte_len = Just(None);
    (
        max_io_delay,
        io_fault_rate,
        read_corrupt_rate,
        key_count,
        offset_count,
        clients,
        restart_after,
        num_disks,
        device_pages,
        short_page_byte_len,
    )
        .prop_map(
            |(
                max_io_delay,
                io_fault_rate,
                read_corrupt_rate,
                key_count,
                offset_count,
                clients,
                restart_after,
                num_disks,
                device_pages,
                short_page_byte_len,
            )| {
                Workload {
                    page_size: 4096,
                    device_pages,
                    max_io_delay,
                    io_fault_rate,
                    read_corrupt_rate,
                    key_count,
                    offset_count,
                    clients,
                    restart_after,
                    num_disks,
                    short_page_byte_len,
                }
            },
        )
}

/// Same shape as [`workload_strategy`] but pins
/// `io_fault_rate = read_corrupt_rate = 0` and forces at least two
/// clients. Used by invariants that are only meaningful in the
/// fault-free, multi-writer regime so they don't lean on
/// `prop_assume!` at run time. Filtering at the strategy level keeps
/// soak runs (large `PROPTEST_CASES`) from tripping
/// `max_global_rejects` once the surrounding strategy's fault
/// distribution shifts.
pub fn workload_strategy_no_faults_multi_client() -> impl Strategy<Value = Workload> {
    workload_strategy().prop_map(|mut w| {
        w.io_fault_rate = 0;
        w.read_corrupt_rate = 0;
        // The base strategy already samples `2..=4` clients, but
        // pin the floor explicitly so a future relaxation there
        // can't silently weaken this strategy.
        if w.clients.len() < 2 {
            // Duplicate the first client spec to reach the floor.
            // Cheap and deterministic; avoids reaching back into
            // `client_strategy` from inside a `prop_map`.
            let dup = w.clients.first().cloned().unwrap_or(ClientSpec { ops: Vec::new() });
            while w.clients.len() < 2 {
                w.clients.push(dup.clone());
            }
        }
        w
    })
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
    /// Aggregate length of the engines' deferred-reclaim queues.
    /// At end-of-run quiescence this should be zero (no pinned
    /// readers remain), so any non-zero value is a leak signal.
    pub pending_free_len: usize,
    /// Per-disk `resident_pages` counters in disk-index order.
    /// Used by eviction invariants to assert each disk stays
    /// below its `device_pages` budget; the aggregate above can
    /// hide a single overgrown disk behind quiet siblings.
    pub resident_pages_per_disk: Vec<usize>,
    /// Echo of `Workload::device_pages` so eviction invariants
    /// can compare per-disk residency against the per-disk
    /// capacity without threading the workload alongside.
    pub device_pages: u64,
    pub device_reads: u64,
    pub device_writes: u64,
    pub device_io_errors: u64,
    /// Count of times the sim device flipped a byte after a
    /// successful inner read. Visible in reports so invariants can
    /// observe whether the corruption path actually fired in this
    /// case.
    pub device_corruptions_injected: u64,
    /// Per-disk `device_writes` counters in disk-index order. Useful
    /// for routing-diversity invariants: when `num_disks_used >= 2`
    /// a healthy hash should spread writes across more than one
    /// disk under any non-trivial workload.
    pub device_writes_per_disk: Vec<u64>,
    /// Echo of `Workload::num_disks` after clamping (>= 1). Lets
    /// invariants scope assertions to the multi-disk regime without
    /// having to thread the workload alongside the report.
    pub num_disks_used: u32,
    /// True if the workload's `restart_after` flag was honored and
    /// the engine was successfully reopened on the same backing
    /// devices. False either because `restart_after` was false or
    /// the pre-restart open path aborted before reaching the
    /// restart phase.
    pub restart_performed: bool,
    /// Outcomes of the post-restart read-replay phase. The replay
    /// scans the full `(key, offset)` grid once. Appended to
    /// `outcomes` is intentional for hit-bytes invariants; these
    /// counters are aggregates so a single invariant can compare
    /// totals without re-scanning `outcomes`.
    pub post_restart_hits: u64,
    pub post_restart_misses: u64,
    pub post_restart_errors: u64,
    /// Per-disk peak `inflight` counters from `SimBlockDevice`,
    /// in disk-index order. A value `>= 2` proves the executor
    /// actually interleaved two device ops on that disk; we use
    /// this to assert that runs configured for concurrency
    /// (multi-client + non-zero `max_io_delay`) observe it at
    /// least once across the sweep.
    pub max_inflight_per_disk: Vec<u32>,
    /// Per-engine `mutator_pending_len` measured AFTER every
    /// client task has finished, every engine's mutator queue
    /// has been closed, and `run_mutator` has joined. At that
    /// point every queue must be drained (zero); a non-zero
    /// value indicates the mutator exited with backlog.
    pub mutator_pending_per_disk: Vec<usize>,
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

/// Build `num_disks` sim devices that share the same `MockSimConfig`
/// so fault / corruption knobs apply uniformly, but each has its own
/// `MockDevice` backing of `device_pages * page_size` bytes.
fn build_devices(
    num_disks: u32,
    page_size: usize,
    device_pages: u64,
    sim_cfg: &Rc<MockSimConfig>,
) -> Vec<Arc<SimBlockDevice>> {
    (0..num_disks.max(1))
        .map(|_| {
            Arc::new(SimBlockDevice::new(
                MockDeviceConfig {
                    page_size,
                    capacity_pages: device_pages,
                    ..Default::default()
                },
                sim_cfg.clone(),
            ))
        })
        .collect()
}

/// Open one [`StorageEngine`] per device and wrap them in a
/// [`LocalStorage`] router. Returns `Ok(None)` if any per-disk open
/// or page registration fails; in that case the caller records the
/// error outcome and skips the rest of the run. Faults / corruption
/// must be disabled by the caller before invoking this.
async fn open_local(
    devices: &[Arc<SimBlockDevice>],
    cfg: EngineConfig,
    pool_base: *mut u8,
    page_size: usize,
    pool_pages: usize,
) -> Result<LocalRc, String> {
    let mut engines = Vec::with_capacity(devices.len());
    for dev in devices {
        let eng = StorageEngine::open(dev.clone(), cfg)
            .await
            .map_err(|e| format!("open: {e}"))?;
        engines.push(Arc::new(eng));
    }
    let local = LocalStorage::new(engines);
    // `LocalStorage::register_pages` fans out to every engine with
    // the same backing. Multi-disk routing only needs each engine
    // to be able to resolve `PageRef`s against the shared pool;
    // production deployments wrap `LocalStorage` in
    // `ShardLocalStore` and use per-shard backings, which is out of
    // scope for this harness.
    local
        .register_pages(pool_base, page_size, pool_pages)
        .map_err(|e| format!("register: {e}"))?;
    Ok(Rc::new(local))
}

/// Sum per-engine snapshot counters across every disk behind
/// `local`. The cache and index are partitioned by disk, so all of
/// these are pointwise summable; in particular
/// `resident_pages == btree_entries` continues to hold globally
/// when it holds on every disk.
fn aggregate_snapshot(local: &LocalStorage<SimBlockDevice>) -> EngineAggregate {
    let mut agg = EngineAggregate {
        resident_pages_per_disk: Vec::with_capacity(local.num_disks()),
        ..EngineAggregate::default()
    };
    for i in 0..local.num_disks() {
        let s = local.engine(i).snapshot();
        agg.hits += s.hits;
        agg.misses += s.misses;
        agg.admitted += s.admitted;
        agg.rejected_by_filter += s.rejected_by_filter;
        agg.evictions += s.evictions;
        agg.write_io_errors += s.write_io_errors;
        agg.read_io_errors += s.read_io_errors;
        agg.checksum_misses += s.checksum_misses;
        agg.resident_pages += s.resident_pages;
        agg.btree_entries += s.btree_entries;
        agg.pending_free_len += s.pending_free_len;
        agg.resident_pages_per_disk.push(s.resident_pages);
    }
    agg
}

#[derive(Default)]
struct EngineAggregate {
    hits: u64,
    misses: u64,
    admitted: u64,
    rejected_by_filter: u64,
    evictions: u64,
    write_io_errors: u64,
    read_io_errors: u64,
    checksum_misses: u64,
    resident_pages: usize,
    btree_entries: usize,
    pending_free_len: usize,
    resident_pages_per_disk: Vec<usize>,
}

#[derive(Default)]
struct DeviceAggregate {
    reads: u64,
    writes: u64,
    io_errors: u64,
    corruptions_injected: u64,
    writes_per_disk: Vec<u64>,
}

fn aggregate_devices(devices: &[Arc<SimBlockDevice>]) -> DeviceAggregate {
    let mut agg = DeviceAggregate {
        writes_per_disk: Vec::with_capacity(devices.len()),
        ..DeviceAggregate::default()
    };
    for d in devices {
        agg.reads += d.reads();
        agg.writes += d.writes();
        agg.io_errors += d.io_errors();
        agg.corruptions_injected += d.corruptions_injected();
        agg.writes_per_disk.push(d.writes());
    }
    agg
}

/// Drive `w` under `seed`. Returns the report so callers can assert
/// invariants. Panics only on framework setup errors that are not
/// "test failures" the caller should shrink against.
pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let num_disks = w.num_disks.max(1);

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

    // Shared sim config (fault / corruption knobs apply uniformly
    // across every disk on this node), distinct per-disk backings.
    let sim_cfg = MockSimConfig::new();
    sim_cfg.max_io_delay.set(w.max_io_delay);
    sim_cfg.io_fault_rate.set(w.io_fault_rate);
    sim_cfg.read_corrupt_rate.set(w.read_corrupt_rate);
    let devices = build_devices(num_disks, w.page_size, w.device_pages, &sim_cfg);

    let engine_cfg = EngineConfig {
        page_size_bytes: w.page_size,
        btree_page_bytes: w.page_size,
        ..EngineConfig::default()
    };

    // Shared slot: clients spin on it until the bootstrap task
    // either installs a `LocalStorage` (`Ready`) or marks itself
    // `Failed`. `Pending` distinguishes "not ready yet" from
    // "open failed; abort".
    let slot: Rc<RefCell<BootstrapStatus>> = Rc::new(RefCell::new(BootstrapStatus::Pending));

    let oracle = Rc::new(Oracle::new());
    let outcomes: Rc<RefCell<Vec<Outcome>>> = Rc::new(RefCell::new(Vec::new()));
    // Tracks the number of client tasks that have not yet
    // completed. The supervisor task closes every engine's
    // mutator queue once this reaches zero so the per-disk
    // `run_mutator` tasks can drain and exit.
    let pending_clients: Rc<Cell<usize>> = Rc::new(Cell::new(w.clients.len()));

    let mut exec = Executor::new(seed);

    // Bootstrap: open engines with faults temporarily disabled
    // (the engine has no recovery contract under torn opens yet),
    // register pages, publish in slot, then restore the configured
    // fault rate so client-time I/Os exercise the fault path.
    {
        let slot = slot.clone();
        let devices_task = devices.clone();
        let outcomes = outcomes.clone();
        let sim_cfg = sim_cfg.clone();
        let configured_delay = w.max_io_delay;
        let configured_faults = w.io_fault_rate;
        let configured_corrupt = w.read_corrupt_rate;
        let page_size = w.page_size;
        exec.spawn(async move {
            sim_cfg.max_io_delay.set(0);
            sim_cfg.io_fault_rate.set(0);
            sim_cfg.read_corrupt_rate.set(0);
            let local =
                match open_local(&devices_task, engine_cfg, pool_base, page_size, pool_pages).await
                {
                    Ok(l) => l,
                    Err(e) => {
                        outcomes.borrow_mut().push(Outcome::Err(e));
                        *slot.borrow_mut() = BootstrapStatus::Failed;
                        return;
                    }
                };
            sim_cfg.max_io_delay.set(configured_delay);
            sim_cfg.io_fault_rate.set(configured_faults);
            sim_cfg.read_corrupt_rate.set(configured_corrupt);
            *slot.borrow_mut() = BootstrapStatus::Ready(local);
        });
    }

    // Per-disk mutator tasks. Each waits for the bootstrap to
    // publish the router, then drives its engine's mutator loop
    // until the supervisor below closes the queue. Spawned
    // upfront because the executor does not let tasks spawn
    // tasks; the wait-loop keeps them parked cheaply until the
    // engines exist.
    for disk_idx in 0..(num_disks as usize) {
        let slot = slot.clone();
        exec.spawn(async move {
            let engine_arc = loop {
                match &*slot.borrow() {
                    BootstrapStatus::Pending => {}
                    BootstrapStatus::Failed => return,
                    BootstrapStatus::Ready(l) => break l.engine_arc(disk_idx),
                }
                yield_once().await;
            };
            engine_arc.run_mutator().await;
        });
    }

    // Supervisor: waits for every client task to finish, then
    // closes each engine's mutator queue. Without this the
    // mutator tasks would park forever and the executor would
    // report a deadlock. The supervisor also waits for the
    // bootstrap to settle before consulting `slot` so the
    // zero-clients edge case (close before Ready was published)
    // still wakes the mutator tasks.
    {
        let slot = slot.clone();
        let pending_clients = pending_clients.clone();
        exec.spawn(async move {
            while pending_clients.get() > 0 {
                yield_once().await;
            }
            // Bootstrap may still be Pending if there were zero
            // clients; wait until it settles.
            loop {
                match &*slot.borrow() {
                    BootstrapStatus::Pending => {}
                    _ => break,
                }
                yield_once().await;
            }
            if let BootstrapStatus::Ready(l) = &*slot.borrow() {
                for i in 0..l.num_disks() {
                    l.engine(i).close_mutator();
                }
            }
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
        let pending_clients = pending_clients.clone();
        let w = w.clone();
        let page_size = w.page_size;
        let pool_base_v = pool_base as usize; // make Send across the async move
        exec.spawn(async move {
            // Wait for bootstrap to publish the router or abort.
            let local = loop {
                match &*slot.borrow() {
                    BootstrapStatus::Pending => {}
                    BootstrapStatus::Failed => {
                        pending_clients.set(pending_clients.get() - 1);
                        return;
                    }
                    BootstrapStatus::Ready(l) => break l.clone(),
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
                        let byte_len = bytes.len();
                        // SAFETY: each op owns a unique pool slot,
                        // so no other task writes here concurrently.
                        // We always wipe the full slot first so any
                        // trailing bytes outside `byte_len` are
                        // zeroed and cannot leak into the device
                        // write (the engine only DMAs `byte_len`
                        // bytes, but a stale slot would still be
                        // observable to a future op that reads at
                        // full page length, which would mask bugs).
                        unsafe {
                            let p = (pool_base_v as *mut u8).add(pool_slot * page_size);
                            std::ptr::write_bytes(p, 0, page_size);
                            std::ptr::copy_nonoverlapping(bytes.as_ptr(), p, byte_len);
                        }
                        let page = PageRef {
                            page_idx: pool_slot as u32,
                            offset: 0,
                            len: byte_len as u32,
                        };
                        oracle.record_write(key, offset, bytes);
                        match local.write_page(key, offset, page).await {
                            Ok(()) => outcomes.borrow_mut().push(Outcome::WriteOk),
                            Err(e) => outcomes
                                .borrow_mut()
                                .push(Outcome::Err(format!("write: {e}"))),
                        }
                    }
                    Op::Read { key_idx, off_idx } => {
                        let key = w.key(*key_idx);
                        let offset = w.offset(*off_idx);
                        let byte_len = w.byte_len(*key_idx, *off_idx);
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
                            len: byte_len as u32,
                        };
                        match local.read_page(key, offset, page).await {
                            Ok(true) => {
                                let bytes = unsafe {
                                    let p = (pool_base_v as *const u8).add(pool_slot * page_size);
                                    std::slice::from_raw_parts(p, byte_len).to_vec()
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
            pending_clients.set(pending_clients.get() - 1);
        });
    }

    // Generous step budget: per op we expect O(delay) yields,
    // multiplied by the typical handful of awaits the engine does
    // per call (admission, singleflight, device.write, btree
    // apply_batch which itself reads + writes the device). Scale
    // mildly with `num_disks` so multi-disk runs that fan I/O out
    // still terminate within budget.
    let total_ops = flat.len() as u64;
    // Step-budget derivation, factor by factor:
    //   4096                       constant bootstrap slack: open
    //                              fans out per-disk engine setup
    //                              (page registration, meta reads,
    //                              btree open) that happens before
    //                              any workload op runs.
    //   total_ops                  per-op cost: every workload op
    //                              contributes its own chain of
    //                              executor steps below.
    //   (max_io_delay + 4)         awaits per op: each engine call
    //                              hits roughly four await points
    //                              (admission, singleflight,
    //                              device I/O, btree apply_batch)
    //                              and each device I/O may stall
    //                              up to `max_io_delay` extra
    //                              yields injected by the sim.
    //   64                         yields per await: generous
    //                              headroom for the executor's
    //                              random interleaving so a single
    //                              await chained with other tasks'
    //                              progress fits comfortably.
    //   (1 + io_fault_rate / 4)    retry inflation for fault
    //                              paths: when faults are enabled
    //                              the engine retries / re-issues
    //                              I/O, so multiply the per-op
    //                              cost by a factor that grows
    //                              with the fault rate.
    //   num_disks                  per-disk fan-out: multi-disk
    //                              runs spread I/O across engines,
    //                              each of which adds its own
    //                              await chains to the total.
    let step_budget = 4096
        + total_ops
            * (w.max_io_delay as u64 + 4)
            * 64
            * (1 + w.io_fault_rate as u64 / 4)
            * (num_disks as u64);

    exec.run(step_budget)?;

    // Snapshot the engines for invariant assertions. If bootstrap
    // aborted (open failed under fault injection) the router never
    // existed, so report zero counters; that path still satisfies
    // every invariant by construction.
    let agg = match &*slot.borrow() {
        BootstrapStatus::Ready(local) => aggregate_snapshot(local),
        _ => EngineAggregate::default(),
    };

    // Per-engine mutator queue lengths AFTER the supervisor closed
    // every queue and `run_mutator` joined. Read while the engines
    // are still alive (via `slot`); after the restart phase drops
    // the pre-restart router these handles disappear.
    let mutator_pending_per_disk: Vec<usize> = match &*slot.borrow() {
        BootstrapStatus::Ready(local) => (0..local.num_disks())
            .map(|i| local.engine(i).mutator_pending_len())
            .collect(),
        _ => Vec::new(),
    };

    // Restart phase. Only runs when the workload requested it AND
    // the original open succeeded. The original engines must be
    // dropped BEFORE the second open: they own buffer-pool
    // registration against `pool_buf` and hold mutable state that
    // would otherwise race with a second open on the same devices.
    // Faults and corruption are disabled for both the second open
    // and the replay: the goal here is to verify the restart
    // contract (no wrong bytes), not to retest fault tolerance of
    // the rebuild path - which `recovery.rs` covers scripted-ly.
    // The disable mirrors the bootstrap-time disable above exactly.
    let (restart_performed, post_restart_hits, post_restart_misses, post_restart_errors) = if w
        .restart_after
        && matches!(*slot.borrow(), BootstrapStatus::Ready(_))
    {
        // Drop the pre-restart router (and with it every
        // engine). Holding any Rc clone would leak the
        // devices' buffer-pool registrations past the second
        // open. Replacing the slot with `Failed` is just for
        // tidiness; nothing reads it after this point.
        let pre = std::mem::replace(&mut *slot.borrow_mut(), BootstrapStatus::Failed);
        drop(pre);

        sim_cfg.max_io_delay.set(0);
        sim_cfg.io_fault_rate.set(0);
        sim_cfg.read_corrupt_rate.set(0);

        let mut exec2 = Executor::new(seed ^ 0xA5A5_A5A5_A5A5_A5A5);
        let outcomes2 = outcomes.clone();
        let devices2 = devices.clone();
        let w2 = w.clone();
        let pool_base_v = pool_base as usize;
        let stats: Rc<RefCell<(u64, u64, u64, bool)>> = Rc::new(RefCell::new((0, 0, 0, false)));
        let stats_task = stats.clone();
        exec2.spawn(async move {
            let local = match open_local(
                &devices2,
                engine_cfg,
                pool_base_v as *mut u8,
                w2.page_size,
                pool_pages,
            )
            .await
            {
                Ok(l) => l,
                Err(e) => {
                    outcomes2
                        .borrow_mut()
                        .push(Outcome::Err(format!("restart {e}")));
                    stats_task.borrow_mut().3 = false;
                    return;
                }
            };
            stats_task.borrow_mut().3 = true;
            let mut slot_idx = 0usize;
            for ki in 0..w2.key_count.max(1) {
                for oi in 0..w2.offset_count.max(1) {
                    let key = w2.key(ki);
                    let offset = w2.offset(oi);
                    let byte_len = w2.byte_len(ki, oi);
                    // SAFETY: every read uses a unique pool
                    // slot; the modulus keeps us inside
                    // `pool_pages` even when grid >= pool size.
                    let pool_slot = slot_idx % pool_pages;
                    slot_idx += 1;
                    unsafe {
                        let p = (pool_base_v as *mut u8).add(pool_slot * w2.page_size);
                        std::ptr::write_bytes(p, 0, w2.page_size);
                    }
                    let page = PageRef {
                        page_idx: pool_slot as u32,
                        offset: 0,
                        len: byte_len as u32,
                    };
                    match local.read_page(key, offset, page).await {
                        Ok(true) => {
                            let bytes = unsafe {
                                let p = (pool_base_v as *const u8).add(pool_slot * w2.page_size);
                                std::slice::from_raw_parts(p, byte_len).to_vec()
                            };
                            outcomes2
                                .borrow_mut()
                                .push(Outcome::ReadHit { key, offset, bytes });
                            stats_task.borrow_mut().0 += 1;
                        }
                        Ok(false) => {
                            outcomes2
                                .borrow_mut()
                                .push(Outcome::ReadMiss { key, offset });
                            stats_task.borrow_mut().1 += 1;
                        }
                        Err(e) => {
                            outcomes2
                                .borrow_mut()
                                .push(Outcome::Err(format!("restart read: {e}")));
                            stats_task.borrow_mut().2 += 1;
                        }
                    }
                }
            }
        });

        // Replay touches one read per grid cell. Each read
        // typically costs O(delay + btree-internal awaits);
        // delays are zero here, but the engine still yields
        // through admission/singleflight/device, so allow
        // generous headroom. Scale mildly with `num_disks`
        // because the per-disk btree opens dominate the
        // replay setup cost.
        let grid = (w.key_count.max(1) as u64) * (w.offset_count.max(1) as u64);
        let replay_budget = 4096 + grid * 256 + (num_disks as u64) * 1024;
        exec2.run(replay_budget)?;

        let (h, m, e, performed) = *stats.borrow();
        (performed, h, m, e)
    } else {
        (false, 0, 0, 0)
    };

    let dev_agg = aggregate_devices(&devices);
    let max_inflight_per_disk: Vec<u32> = devices.iter().map(|d| d.max_inflight()).collect();

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
        hits: agg.hits,
        misses: agg.misses,
        admitted: agg.admitted,
        rejected_by_filter: agg.rejected_by_filter,
        evictions: agg.evictions,
        write_io_errors: agg.write_io_errors,
        read_io_errors: agg.read_io_errors,
        checksum_misses: agg.checksum_misses,
        resident_pages: agg.resident_pages,
        btree_entries: agg.btree_entries,
        pending_free_len: agg.pending_free_len,
        resident_pages_per_disk: agg.resident_pages_per_disk,
        device_pages: w.device_pages,
        device_reads: dev_agg.reads,
        device_writes: dev_agg.writes,
        device_io_errors: dev_agg.io_errors,
        device_corruptions_injected: dev_agg.corruptions_injected,
        device_writes_per_disk: dev_agg.writes_per_disk,
        num_disks_used: num_disks,
        restart_performed,
        post_restart_hits,
        post_restart_misses,
        post_restart_errors,
        max_inflight_per_disk,
        mutator_pending_per_disk,
    })
}
