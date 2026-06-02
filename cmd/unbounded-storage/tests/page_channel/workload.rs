// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategy, and the `run_workload` driver
//! for the cross-core [`PageChannel`] DST.
//!
//! The driver spawns, upfront on the seeded executor:
//!   - one bootstrap task that opens one [`StorageEngine`] per
//!     simulated disk (with faults disabled), registers the page
//!     backing DIRECTLY on each engine via
//!     [`StorageEngine::register_extra_buffer`], builds one
//!     [`PageChannel`] per disk, and publishes the channels to the
//!     clients;
//!   - one cooperative "storage-core" task per disk that drives the
//!     engine mutator and the [`PageService`] off the same poll
//!     `Context`;
//!   - one supervisor task that flips a shared stop flag once every
//!     client has finished; and
//!   - one client task per [`ClientSpec`] that issues its op sequence
//!     over the per-disk [`PageChannel`]s, routing each op with
//!     [`disk_for`] exactly as the production shard view does.
//!
//! Buffer registration MUST NOT go through
//! [`PageChannel::register_buffer`]: that call is synchronous and
//! busy-spins on its reply slot, which deadlocks on the single
//! threaded executor because the [`PageService`] that would set the
//! slot runs on the same thread. Registering on the engine directly
//! sidesteps that; it is the same method `PageService` calls
//! internally for a `RegisterBuffer` command.
//!
//! [`PageChannel`]: unbounded_storage::storage::PageChannel
//! [`PageChannel::register_buffer`]: unbounded_storage::storage::PageChannel::register_buffer
//! [`PageService`]: unbounded_storage::storage::PageService
//! [`StorageEngine`]: unbounded_storage::storage::StorageEngine
//! [`StorageEngine::register_extra_buffer`]: unbounded_storage::storage::StorageEngine::register_extra_buffer
//! [`disk_for`]: unbounded_storage::storage::disk_for

use std::cell::{Cell, RefCell};
use std::future::{Future, poll_fn};
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{Error, StripeKey};
use unbounded_storage::storage::blockdev::MockDeviceConfig;
use unbounded_storage::storage::{
    EngineConfig, PageChannel, PageChannelReceiver, PageService, StorageEngine, disk_for,
};

use crate::framework::executor::{Executor, RunError, yield_once};
use crate::storage::mocks::{MockSimConfig, SimBlockDevice};
use crate::storage::oracle::Oracle;

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

type EngineRc = Arc<StorageEngine<SimBlockDevice>>;
/// Per-disk handoff from the bootstrap task to the storage-core task:
/// the opened engine plus the receiver half of that disk's channel.
type CoreSlot = Rc<RefCell<Option<(EngineRc, PageChannelReceiver)>>>;

/// Handoff state from the bootstrap task to the client tasks.
/// `Pending` means bootstrap has not yet published a result. `Failed`
/// means an open or registration aborted; clients exit without running
/// their ops. `Ready` carries the per-disk [`PageChannel`]s clients
/// route over.
enum BootstrapStatus {
    Pending,
    Failed,
    Ready(Rc<Vec<PageChannel>>),
}

/// Sized so a single run stays well under a second and shrinks
/// quickly.
#[derive(Clone, Debug)]
pub struct Workload {
    /// Cache + device page size in bytes. Fixed at 4 KiB because the
    /// engine equates cache page and btree page in this regime.
    pub page_size: usize,
    /// Per-disk device capacity in pages. Kept comfortably above the
    /// admitted-write count so the page-channel path, not eviction,
    /// is what these tests exercise.
    pub device_pages: u64,
    pub max_io_delay: u32,
    pub io_fault_rate: u32,
    /// Probability `[0, 100]` that a successful device read silently
    /// corrupts its first byte. The engine's xxh3 page checksum must
    /// convert this into a miss; a `ReadHit` must never return
    /// corrupted bytes.
    pub read_corrupt_rate: u32,
    /// Distinct stripe keys the workload may reference.
    pub key_count: u8,
    /// Distinct page offsets within each stripe.
    pub offset_count: u8,
    pub clients: Vec<ClientSpec>,
    /// Number of simulated disks. `1` exercises the single-core path;
    /// `>= 2` exercises cross-disk routing via [`disk_for`], with one
    /// [`PageChannel`] and storage-core per disk.
    pub num_disks: u32,
    /// If true, after the executor run completes (every storage-core
    /// task has exited and dropped its [`PageChannelReceiver`]), issue
    /// one more `write_page` on a surviving channel clone and record
    /// whether it failed. Exercises the service-shutdown path: a send
    /// after the service is gone must resolve with an error rather
    /// than parking forever.
    pub probe_shutdown: bool,
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
    /// Length is always `page_size`: the engine requires every write
    /// to be a positive multiple of `btree_page_bytes`.
    pub fn payload(&self, key_idx: u8, off_idx: u8, seed: u8) -> Vec<u8> {
        let len = self.page_size;
        let mut out = vec![0u8; len];
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
    // Skew toward non-zero delay so the executor can actually
    // interleave concurrent page ops; the `0` arm keeps the
    // no-latency regime covered.
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
    // concurrency on a shared storage core.
    let clients = vec(client_strategy(), 2..=4);
    // `1` keeps the single-core path covered; `2..=4` exercises
    // cross-disk routing without blowing up the per-run open cost.
    let num_disks = 1u32..=4;
    let probe_shutdown = any::<bool>();
    // Roomy enough that eviction is rare: these tests target the
    // page-channel path, not the LRU watermark (the storage DST
    // covers eviction).
    let device_pages = 32u64..=128;
    (
        max_io_delay,
        io_fault_rate,
        read_corrupt_rate,
        key_count,
        offset_count,
        clients,
        num_disks,
        probe_shutdown,
        device_pages,
    )
        .prop_map(
            |(
                max_io_delay,
                io_fault_rate,
                read_corrupt_rate,
                key_count,
                offset_count,
                clients,
                num_disks,
                probe_shutdown,
                device_pages,
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
                    num_disks,
                    probe_shutdown,
                }
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    vec(op_strategy(), 1..=8).prop_map(|ops| ClientSpec { ops })
}

fn op_strategy() -> impl Strategy<Value = Op> {
    // 60% writes, 40% reads. Admission requires a second touch before
    // anything lands, so a read-heavy mix would barely exercise the
    // data path.
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
    /// `write_page` returned `Ok(())`.
    WriteOk,
    /// `read_page` returned `Ok(true)`; `bytes` is what landed in the
    /// destination slot.
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
    /// Echo of `Workload::num_disks` after clamping (>= 1).
    pub num_disks_used: u32,
    pub hits: u64,
    pub misses: u64,
    pub errors: u64,
    pub device_reads: u64,
    pub device_writes: u64,
    pub device_io_errors: u64,
    pub device_corruptions_injected: u64,
    /// Per-disk `device_writes` counters in disk-index order. Used by
    /// the routing-diversity invariant: a healthy `disk_for` hash
    /// spreads writes across more than one disk under a non-trivial
    /// multi-disk workload.
    pub device_writes_per_disk: Vec<u64>,
    /// `Some(true)` if the post-shutdown probe write failed (the
    /// expected outcome once the service is gone), `Some(false)` if it
    /// unexpectedly succeeded, `None` if no probe ran (either the flag
    /// was off or bootstrap aborted so no channel existed).
    pub post_shutdown_send_errored: Option<bool>,
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

/// Build `num_disks` sim devices that share one `MockSimConfig` (so
/// fault / corruption knobs apply uniformly) but each have their own
/// `MockDevice` backing.
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

/// Drive `w` under `seed`. Returns the report so callers can assert
/// invariants. Panics only on framework setup errors that are not
/// "test failures" the caller should shrink against.
pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let num_disks = w.num_disks.max(1);

    // Pre-flatten ops so we know exactly how many pool slots we need
    // (one per op, so reads and writes never alias the same byte range
    // across an `await`). The trailing `+ 1` slot backs the
    // post-shutdown probe.
    let mut flat: Vec<(usize, Op)> = Vec::new();
    for (cid, c) in w.clients.iter().enumerate() {
        for op in &c.ops {
            flat.push((cid, op.clone()));
        }
    }
    let pool_pages = flat.len() + 1;

    // Heap-allocated pool backing. The Box keeps it alive past
    // `exec.run` so engine reads/writes (and the post-shutdown probe)
    // stay in bounds.
    let mut pool_buf: Box<[u8]> = vec![0u8; pool_pages * w.page_size].into_boxed_slice();
    let pool_base: *mut u8 = pool_buf.as_mut_ptr();
    let pool_len = pool_pages * w.page_size;

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

    // Shared bootstrap handoff + per-disk engine/receiver slots.
    let slot: Rc<RefCell<BootstrapStatus>> = Rc::new(RefCell::new(BootstrapStatus::Pending));
    let core_slots: Vec<CoreSlot> = (0..num_disks as usize)
        .map(|_| Rc::new(RefCell::new(None)))
        .collect();

    let oracle = Rc::new(Oracle::new());
    let outcomes: Rc<RefCell<Vec<Outcome>>> = Rc::new(RefCell::new(Vec::new()));
    let pending_clients: Rc<Cell<usize>> = Rc::new(Cell::new(w.clients.len()));
    // Flipped by the supervisor once every client has finished; the
    // storage-core tasks observe it and begin their shutdown.
    let stop: Rc<Cell<bool>> = Rc::new(Cell::new(false));

    let mut exec = Executor::new(seed);

    // Bootstrap: open engines with faults disabled, register the page
    // backing directly on each engine (NOT via the channel, which
    // would deadlock), build one channel per disk, hand each engine +
    // receiver to its storage-core slot, then publish the channels.
    {
        let slot = slot.clone();
        let devices_task = devices.clone();
        let outcomes = outcomes.clone();
        let sim_cfg = sim_cfg.clone();
        let core_slots = core_slots.clone();
        let configured = (w.max_io_delay, w.io_fault_rate, w.read_corrupt_rate);
        let pool_base_v = pool_base as usize;
        exec.spawn(async move {
            sim_cfg.max_io_delay.set(0);
            sim_cfg.io_fault_rate.set(0);
            sim_cfg.read_corrupt_rate.set(0);

            let mut channels = Vec::with_capacity(devices_task.len());
            let mut ok = true;
            for (i, dev) in devices_task.iter().enumerate() {
                let eng = match StorageEngine::open(dev.clone(), engine_cfg).await {
                    Ok(e) => Arc::new(e),
                    Err(e) => {
                        outcomes
                            .borrow_mut()
                            .push(Outcome::Err(format!("open: {e}")));
                        ok = false;
                        break;
                    }
                };
                // Register on the engine directly; the synchronous
                // `PageChannel::register_buffer` busy-spins and would
                // deadlock the single-threaded executor.
                if let Err(e) = eng.register_extra_buffer(pool_base_v as *mut u8, pool_len) {
                    outcomes
                        .borrow_mut()
                        .push(Outcome::Err(format!("register: {e}")));
                    ok = false;
                    break;
                }
                let (channel, rx) = PageChannel::new();
                channels.push(channel);
                *core_slots[i].borrow_mut() = Some((eng.clone(), rx));
            }

            if !ok {
                *slot.borrow_mut() = BootstrapStatus::Failed;
                return;
            }

            sim_cfg.max_io_delay.set(configured.0);
            sim_cfg.io_fault_rate.set(configured.1);
            sim_cfg.read_corrupt_rate.set(configured.2);
            *slot.borrow_mut() = BootstrapStatus::Ready(Rc::new(channels));
        });
    }

    // Storage-core tasks: one per disk. Each parks until bootstrap has
    // filled its slot, then cooperatively drives the engine mutator
    // and the page service until the stop flag is set and all work has
    // drained. The loop body is a `poll_fn` so it can hand its own
    // poll `Context` to `PageService::poll_once` and the mutator
    // future; the device's `yield_n` re-wakes this task through that
    // same context. `yield_once().await` each iteration keeps the task
    // cooperatively scheduled in place of the production `thread::sleep`.
    for disk_idx in 0..(num_disks as usize) {
        let slot = slot.clone();
        let core_slot = core_slots[disk_idx].clone();
        let stop = stop.clone();
        exec.spawn(async move {
            let (engine_arc, rx) = loop {
                match &*slot.borrow() {
                    BootstrapStatus::Pending => {}
                    BootstrapStatus::Failed => return,
                    BootstrapStatus::Ready(_) => {
                        if let Some(pair) = core_slot.borrow_mut().take() {
                            break pair;
                        }
                    }
                }
                yield_once().await;
            };

            let mut service = PageService::new(engine_arc.clone(), rx);
            let mut mutator: Pin<Box<dyn Future<Output = ()>>> =
                Box::pin(engine_arc.clone().run_mutator());
            let mut close_signaled = false;
            let mut mutator_done = false;

            loop {
                // One cooperative iteration: advance the mutator and
                // the page service against this task's poll Context.
                poll_fn(|cx| {
                    if !mutator_done {
                        if let Poll::Ready(()) = mutator.as_mut().poll(cx) {
                            mutator_done = true;
                        }
                    }
                    service.poll_once(cx);
                    Poll::Ready(())
                })
                .await;

                let shutdown = stop.get() || service.channel_disconnected();
                if shutdown && !close_signaled {
                    engine_arc.close_mutator();
                    service.fail_all(Error::Io(libc::EIO));
                    service.mark_dead();
                    close_signaled = true;
                }
                if close_signaled {
                    service.drain_pending(Error::Io(libc::EIO));
                }
                if mutator_done && close_signaled && !service.has_inflight() {
                    return;
                }
                yield_once().await;
            }
        });
    }

    // Supervisor: once every client has finished, settle the bootstrap
    // (covers the zero-client edge) and flip the stop flag so the
    // storage-core tasks can drain and exit.
    {
        let slot = slot.clone();
        let pending_clients = pending_clients.clone();
        let stop = stop.clone();
        exec.spawn(async move {
            while pending_clients.get() > 0 {
                yield_once().await;
            }
            loop {
                match &*slot.borrow() {
                    BootstrapStatus::Pending => {}
                    _ => break,
                }
                yield_once().await;
            }
            stop.set(true);
        });
    }

    // Client tasks: one per ClientSpec; each runs its op sequence
    // serially over the per-disk channels, routing with `disk_for`
    // exactly as the production shard view does.
    for (cid, c) in w.clients.iter().cloned().enumerate() {
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
        let num_disks = num_disks as usize;
        let pool_base_v = pool_base as usize; // raw ptr carried as usize across the move
        exec.spawn(async move {
            let channels = loop {
                match &*slot.borrow() {
                    BootstrapStatus::Pending => {}
                    BootstrapStatus::Failed => {
                        pending_clients.set(pending_clients.get() - 1);
                        return;
                    }
                    BootstrapStatus::Ready(ch) => break ch.clone(),
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
                        // SAFETY: each op owns a unique pool slot, so
                        // no other task touches this range concurrently.
                        let base = unsafe { (pool_base_v as *mut u8).add(pool_slot * page_size) };
                        unsafe {
                            std::ptr::write_bytes(base, 0, page_size);
                            std::ptr::copy_nonoverlapping(bytes.as_ptr(), base, byte_len);
                        }
                        oracle.record_write(key, offset, bytes);
                        let src = std::ptr::slice_from_raw_parts(base as *const u8, byte_len);
                        let idx = disk_for(&key, offset, num_disks);
                        match channels[idx].write_page(key, offset, src).await {
                            Ok(()) => outcomes.borrow_mut().push(Outcome::WriteOk),
                            Err(e) => outcomes
                                .borrow_mut()
                                .push(Outcome::Err(format!("write: {e}"))),
                        }
                    }
                    Op::Read { key_idx, off_idx } => {
                        let key = w.key(*key_idx);
                        let offset = w.offset(*off_idx);
                        let byte_len = page_size;
                        // SAFETY: unique pool slot; zero it first so a
                        // partial / skipped fill is visible.
                        let base = unsafe { (pool_base_v as *mut u8).add(pool_slot * page_size) };
                        unsafe {
                            std::ptr::write_bytes(base, 0, page_size);
                        }
                        let dst = std::ptr::slice_from_raw_parts_mut(base, byte_len);
                        let idx = disk_for(&key, offset, num_disks);
                        match channels[idx].read_page(key, offset, dst).await {
                            Ok(true) => {
                                let bytes = unsafe {
                                    std::slice::from_raw_parts(base as *const u8, byte_len).to_vec()
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

    // Step-budget derivation mirrors the storage workload's formula:
    //   4096                       constant bootstrap slack (per-disk
    //                              engine open, page registration).
    //   total_ops                  per-op cost.
    //   (max_io_delay + 4)         awaits per op (admission,
    //                              singleflight, device I/O, btree
    //                              apply_batch), each device I/O
    //                              stalling up to `max_io_delay`.
    //   64                         yields per await: headroom for the
    //                              executor's random interleaving.
    //   (1 + io_fault_rate / 4)    retry inflation on fault paths.
    //   num_disks                  per-disk fan-out.
    let total_ops = flat.len() as u64;
    let step_budget = 4096
        + total_ops
            * (w.max_io_delay as u64 + 4)
            * 64
            * (1 + w.io_fault_rate as u64 / 4)
            * (num_disks as u64);

    exec.run(step_budget)?;

    // Service-shutdown probe. Every storage-core task has now returned,
    // dropping its `PageService` and the receiver half of its channel,
    // so a send on a surviving channel clone must fail rather than park.
    let post_shutdown_send_errored = if w.probe_shutdown {
        let chan = match &*slot.borrow() {
            BootstrapStatus::Ready(channels) => Some(channels[0].clone()),
            _ => None,
        };
        chan.map(|ch| {
            // SAFETY: `pool_buf` is still alive; slot 0 is in bounds.
            // The send fails at the channel before the buffer is ever
            // touched, but keep the pointer valid regardless.
            let src = std::ptr::slice_from_raw_parts(pool_base as *const u8, w.page_size);
            let res = block_on_local(ch.write_page(StripeKey([0u8; 32]), 0, src));
            res.is_err()
        })
    } else {
        None
    };

    // Aggregate device counters.
    let mut device_reads = 0u64;
    let mut device_writes = 0u64;
    let mut device_io_errors = 0u64;
    let mut device_corruptions_injected = 0u64;
    let mut device_writes_per_disk = Vec::with_capacity(devices.len());
    for d in &devices {
        device_reads += d.reads();
        device_writes += d.writes();
        device_io_errors += d.io_errors();
        device_corruptions_injected += d.corruptions_injected();
        device_writes_per_disk.push(d.writes());
    }

    let outcomes = Rc::try_unwrap(outcomes)
        .map_err(|_| RunError::Deadlock)
        .expect("all tasks completed; outcomes Rc must be unique")
        .into_inner();

    let mut hits = 0u64;
    let mut misses = 0u64;
    let mut errors = 0u64;
    for o in &outcomes {
        match o {
            Outcome::ReadHit { .. } => hits += 1,
            Outcome::ReadMiss { .. } => misses += 1,
            Outcome::Err(_) => errors += 1,
            Outcome::WriteOk => {}
        }
    }

    // Hold `pool_buf` alive until here (engine DMA and the probe both
    // borrow it).
    drop(pool_buf);
    let _ = oracle;

    Ok(RunReport {
        outcomes,
        steps: exec.last_steps(),
        num_disks_used: num_disks,
        hits,
        misses,
        errors,
        device_reads,
        device_writes,
        device_io_errors,
        device_corruptions_injected,
        device_writes_per_disk,
        post_shutdown_send_errored,
    })
}

/// Minimal noop-waker `block_on` for the post-shutdown probe, which
/// runs after the executor returns and so cannot use the framework's
/// scheduler. The probed future resolves in a single poll (the channel
/// send fails immediately once the receiver is gone), so the spin
/// budget is generous insurance, not a hot loop.
fn block_on_local<F: Future>(fut: F) -> F::Output {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: the vtable functions are no-ops or return the same
    // vtable, so the waker can be cloned and dropped freely.
    let waker = unsafe { Waker::from_raw(raw()) };
    let mut cx = Context::from_waker(&waker);
    let mut fut = fut;
    // SAFETY: the future is owned here and never moved after pinning.
    let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
    for _ in 0..1_000_000 {
        if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
            return v;
        }
        std::thread::yield_now();
    }
    panic!("block_on_local: probe future did not complete within spin budget");
}
