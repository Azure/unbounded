// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `bench` is a separate CLI for benchmarking subsystems of the
//! `unbounded-storage` crate. New benchmark families are added as
//! top-level subcommands; today the only family is `storage`, whose
//! `block` subcommand drives the io_uring + `StorageEngine` +
//! `LocalStorage` stack, running a write phase followed by a read
//! phase against a deterministic `xxh3`-derived key stream.
//!
//! ## Trust invariants
//!
//! The bench is only useful if its numbers reflect real on-disk
//! work. To keep that contract honest:
//!
//! - Every `write_page` is awaited end-to-end: the writer counts an
//!   op as `Ok` only after the engine's mutator commits the btree
//!   insert that publishes the key. A device-level write error
//!   surfaces as `Err` from `write_page` (see
//!   `storage::engine::write_page_from`) and is *not* counted as a
//!   successful op.
//! - The read phase must observe near-zero misses against the keys
//!   the write phase recorded. The driver prints both phases'
//!   numbers plus per-engine `EngineSnapshot` so any silent drop
//!   (admission rejection, write_io_error, etc.) is visible.
//! - Each op stamps a per-op tag (`worker_id`, `seq`, mixed seed)
//!   into the page's first 64 bytes so the engine's content
//!   checksum changes per op. This prevents any future
//!   content-addressed dedup from collapsing distinct ops into one
//!   physical write.
//! - `--verify` switches to a small, byte-equality round trip:
//!   write N distinct payloads, read each back, and assert the
//!   bytes match exactly. Use this whenever the bench is modified
//!   to reconfirm closed-loop correctness before trusting any
//!   throughput numbers.
//!
//! Threading model: the `StorageRing` built by `UringDevice::open` is
//! `!Send + !Sync` because it is pinned to the thread that opened it.
//! The design (`docs design "io_uring"`) calls for N pinned threads per
//! NVMe drive, where each thread owns its own ring, on a CPU in that
//! drive's NUMA domain. We satisfy that by spawning one OS shard
//! thread per (device, thread-slot) pair via the crate's
//! `runtime::PinnedRuntime`, with placement derived from
//! `topology::CorePlan`. Each storage-core thread installs its own
//! `StorageRing` into the thread-local registry, then builds a
//! `CoreLocalDevice` (which resolves that ring), a `StorageEngine`,
//! hugepage `Backing`, and a single-engine `LocalStorage` so the
//! registration and routing paths are exercised; cross-shard routing is
//! partitioned at the workload generator (each shard only writes /
//! reads keys hashed to its own global shard idx).
//!
//! When the host's sysfs topology cannot be discovered or a `--device`
//! path cannot be mapped onto a topology NVMe controller, the bench
//! falls back to an unpinned `DefaultRuntime` with a warning so it
//! remains usable in containers and dev hosts.

use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::process::ExitCode;
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

use clap::{Parser, Subcommand};
use rand::{RngCore, SeedableRng};
use rand_chacha::ChaCha20Rng;

use unbounded_storage::bufferpool::{BlockStore, PageRef, StripeKey};
use unbounded_storage::memory::{BackingKind, BackingRequest, HUGEPAGE_2MB, allocate};
use unbounded_storage::ring::{
    StorageRing, StorageRingConfig, clear_current_storage_ring, set_current_storage_ring,
};
use unbounded_storage::runtime::{
    DefaultRuntime, PinnedRuntime, Threading, WorkerIdx, WorkerSpec, noop_waker,
};
use unbounded_storage::storage::blockdev::{BlockDevice, CoreLocalDevice, OpenDisk, UringDevice};
use unbounded_storage::storage::{EngineConfig, LocalStorage, StorageEngine};
use unbounded_storage::topology::{CorePlan, CorePlanConfig, Host, Nvme};

#[path = "bench/transport.rs"]
mod transport;

const _: () = assert!(HUGEPAGE_2MB == 2 * 1024 * 1024);

/// User-data page size used by the bench, in bytes.
///
/// Cache pages are 2 MiB so each user write_page is one large
/// device I/O. 4 KiB writes are IOPS-bound on commodity disks
/// (~100 MiB/s at 4 KiB qd=128 on /dev/sdc); 2 MiB writes reach
/// the device's bandwidth ceiling (~1.9 GiB/s on the same disk
/// under fio). The engine still uses 4 KiB btree pages internally
/// (see `btree_page_bytes` below), so the device's atomic-write
/// unit remains 4 KiB and torn-write safety of the index is
/// preserved.
const PAGE_BYTES: usize = 2 * 1024 * 1024;

/// Default PRNG seed for benchmark workloads.
const DEFAULT_SEED: u64 = 0xBE_DE_CA_FE_u64;

/// Default IO threads per NVMe drive. Matches the design's "fixed
/// small N (e.g. 2-4) per drive" guidance; the bench uses it to size
/// its own per-device workload sharding.
const DEFAULT_THREADS_PER_DEVICE: usize = 2;

/// Latency-sample ring per worker. Picked to bound memory while
/// being large enough that a 2-second run still produces a useful
/// distribution.
const LATENCY_RING: usize = 4096;

/// Hard ceiling on consecutive write errors before a shard sets
/// `SHUTDOWN`. Avoids runaway noise if the device is wedged.
const MAX_CONSECUTIVE_ERRORS: u64 = 64;

/// Process-wide shutdown flag set by SIGINT/SIGTERM and by the
/// runaway-error guard.
static SHUTDOWN: AtomicBool = AtomicBool::new(false);

#[derive(Parser)]
#[command(name = "bench", about = "Benchmark harness for unbounded subsystems.")]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Storage subsystem benchmarks.
    Storage {
        #[command(subcommand)]
        cmd: StorageCmd,
    },
}

#[derive(Subcommand)]
enum StorageCmd {
    /// Block-layer (io_uring + buffer pool) benchmark.
    Block(BlockArgs),
    /// Transport (libfabric RPC + RMA + buffer pool) benchmark.
    Transport(transport::TransportArgs),
    /// Topology probe / report (not yet implemented).
    Topology,
}

#[derive(clap::Args)]
struct BlockArgs {
    /// Block device(s) to target. Repeat the flag for each device.
    #[arg(short = 'd', long = "device", required = true, num_args = 1..)]
    device: Vec<PathBuf>,

    /// Number of concurrent worker tasks per phase. Distributed
    /// across all shards (devices x threads-per-device).
    #[arg(short = 'w', long = "workers", default_value_t = 128)]
    workers: usize,

    /// Duration of each phase, in seconds.
    #[arg(short = 't', long = "duration", default_value_t = 30)]
    duration: u64,

    /// Optional cap on total operations per phase.
    #[arg(long = "ops")]
    ops: Option<u64>,

    /// PRNG seed for workload generation.
    #[arg(long = "seed", default_value_t = DEFAULT_SEED)]
    seed: u64,

    /// Override the io_uring submission queue depth.
    #[arg(long = "queue-depth")]
    queue_depth: Option<u32>,

    /// Number of pinned IO threads per NVMe drive. Each thread
    /// owns its own io_uring ring and `StorageEngine` for that
    /// drive (design: "n threads per nvme drive", `io_uring`
    /// section).
    #[arg(long = "threads-per-device", default_value_t = DEFAULT_THREADS_PER_DEVICE)]
    threads_per_device: usize,

    /// Disable `IORING_SETUP_IOPOLL`. Required for devices whose
    /// request queue does not support polled I/O (e.g. SATA, SCSI,
    /// virtio-blk). The production default targets NVMe and assumes
    /// IOPOLL is supported.
    #[arg(long = "no-iopoll")]
    no_iopoll: bool,

    /// Open the underlying file without `O_DIRECT`. Required for
    /// regular files on filesystems that reject direct I/O, and
    /// usable for non-NVMe block devices that mis-handle 2 MiB
    /// direct I/O. Trades realistic numbers for the ability to run
    /// at all.
    #[arg(long = "no-o-direct")]
    no_o_direct: bool,

    /// Disable `IORING_SETUP_SINGLE_ISSUER` and
    /// `IORING_SETUP_DEFER_TASKRUN`. These two flags pair with
    /// IOPOLL on NVMe; turn them off together when running against
    /// non-NVMe targets. Implied by `--no-iopoll`.
    #[arg(long = "no-single-issuer")]
    no_single_issuer: bool,

    /// Skip the engine's admission filter on writes. The filter
    /// rejects every first-touch key by design (it is a Bloom-style
    /// doorkeeper for one-hit-wonders), so leaving it on causes
    /// every workload to admit ~half its ops on the cold cache
    /// and the read phase to miss the rejected keys. On by default
    /// so the read phase observes the keys the write phase
    /// recorded; set to false to measure the production path
    /// including the doorkeeper.
    #[arg(long = "bypass-admission", default_value_t = true)]
    bypass_admission: bool,

    /// Run a small closed-loop verification instead of the timed
    /// throughput phases: write `--verify-ops` distinct payloads,
    /// read each back, and assert that the returned bytes match
    /// what was written. Exits non-zero on any mismatch or miss.
    /// Use this whenever the bench is modified to reconfirm trust.
    #[arg(long = "verify")]
    verify: bool,

    /// Number of ops to issue in `--verify` mode. Kept small by
    /// default so a verify run completes in seconds.
    #[arg(long = "verify-ops", default_value_t = 256)]
    verify_ops: u64,

    /// Run the full LBA-order leaf scan at open time when no meta
    /// page can be loaded. Off by default: a wiped device has no
    /// leaves to recover and a full-capacity scan (~20M reads on a
    /// 80 GiB SSD) makes the bench unusable. Set this flag to
    /// exercise the production "meta lost, recover from leaves"
    /// path.
    #[arg(long = "full-recovery-scan")]
    full_recovery_scan: bool,
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match cli.cmd {
        Cmd::Storage { cmd } => match cmd {
            StorageCmd::Block(args) => run_block(args),
            StorageCmd::Transport(args) => transport::run(args),
            StorageCmd::Topology => {
                eprintln!("bench storage: topology subcommand is not yet implemented");
                ExitCode::from(2)
            }
        },
    }
}

fn run_block(args: BlockArgs) -> ExitCode {
    if args.workers == 0 {
        eprintln!("bench: --workers must be >= 1");
        return ExitCode::FAILURE;
    }
    if args.threads_per_device == 0 {
        eprintln!("bench: --threads-per-device must be >= 1");
        return ExitCode::FAILURE;
    }
    let num_devices = args.device.len();
    if num_devices == 0 {
        eprintln!("bench: at least one --device required");
        return ExitCode::FAILURE;
    }

    install_signal_handler();

    let threads_per_device = args.threads_per_device;
    let total_shards = num_devices * threads_per_device;

    // Build per-device placement specs from the topology module.
    // `build_pinned_specs` returns `Some` only when every --device
    // path was matched to a topology NVMe controller; otherwise we
    // fall back to an unpinned runtime so the bench still runs in
    // containers / dev hosts. We only spawn one OS thread per
    // device because all shards on a device share a single
    // `StorageEngine` (see `run_device`).
    let pinned_specs = build_pinned_specs(&args.device);
    let runtime: Arc<dyn Threading> = match pinned_specs {
        Some(specs) => {
            debug_assert_eq!(specs.len(), num_devices);
            PinnedRuntime::new(specs)
        }
        None => DefaultRuntime::new(num_devices),
    };

    // Distribute workers across the global shard count so the
    // workload still routes to `total_shards` partitions; the
    // OS-thread count is `num_devices`, but each thread runs
    // `threads_per_device` logical shards' worth of workers
    // sharing one engine.
    let base = args.workers / total_shards;
    let extra = args.workers % total_shards;
    let workers_per_shard: Vec<usize> = (0..total_shards)
        .map(|i| if i < extra { base + 1 } else { base })
        .collect();

    // Per-phase per-shard op cap. Spread the global cap evenly.
    let ops_cap_per_shard: Option<u64> = args.ops.map(|n| n.div_ceil(total_shards as u64).max(1));

    // Cumulative worker_id offset so every worker has a globally
    // unique id (used as both a routing tiebreaker and a ChaCha
    // sub-seed).
    let mut worker_id_offset = 0usize;

    let (tx, rx) = mpsc::channel::<DeviceOutcome>();
    let phase_duration = Duration::from_secs(args.duration);
    // `--no-iopoll` forces the single-issuer / defer-taskrun pair off
    // too: those flags only make sense on the IOPOLL fast path.
    let iopoll = !args.no_iopoll;
    let single_issuer = iopoll && !args.no_single_issuer;
    let defer_taskrun = single_issuer;
    let o_direct = !args.no_o_direct;
    // Verify mode bypasses the timed throughput phases entirely;
    // each shard runs a small closed-loop write+read+compare.
    let verify_ops_per_shard = if args.verify {
        args.verify_ops.div_ceil(total_shards as u64).max(1)
    } else {
        0
    };
    let mut joins = Vec::with_capacity(num_devices);
    for device_idx in 0..num_devices {
        let pinned_numa = runtime.numa_of(WorkerIdx(device_idx as u16));
        let mut shard_cfgs: Vec<ShardConfig> = Vec::with_capacity(threads_per_device);
        for thread_idx in 0..threads_per_device {
            let shard_idx = device_idx * threads_per_device + thread_idx;
            let workers_for_shard = workers_per_shard[shard_idx];
            shard_cfgs.push(ShardConfig {
                shard_idx,
                total_shards,
                thread_idx,
                device_path: args.device[device_idx].clone(),
                pinned_numa,
                queue_depth: args.queue_depth,
                workers: workers_for_shard,
                worker_id_base: worker_id_offset,
                seed: args.seed,
                duration: phase_duration,
                ops_cap_per_shard,
                iopoll,
                o_direct,
                single_issuer,
                defer_taskrun,
                // --verify implies --bypass-admission: the doorkeeper
                // deliberately drops every first-touch write, which
                // makes byte-equality verification impossible. Verify
                // is about closed-loop correctness of the I/O path, so
                // disabling the filter is the right call here.
                bypass_admission: args.bypass_admission || args.verify,
                verify: args.verify,
                verify_ops_per_shard,
                skip_recovery_scan_if_no_meta: !args.full_recovery_scan,
            });
            worker_id_offset += workers_for_shard;
        }
        let tx = tx.clone();
        let name = format!("bench-dev-{device_idx}");
        let handle = runtime.spawn_pinned(
            WorkerIdx(device_idx as u16),
            &name,
            Box::new(move || {
                let outcome = run_device(device_idx, shard_cfgs);
                let _ = tx.send(outcome);
            }),
        );
        joins.push(handle);
    }
    drop(tx);

    let mut write_reports: Vec<ShardReport> = Vec::with_capacity(total_shards);
    let mut read_reports: Vec<ShardReport> = Vec::with_capacity(total_shards);
    let mut errors: Vec<String> = Vec::new();
    while let Ok(outcome) = rx.recv() {
        match outcome {
            DeviceOutcome::Ok { writes, reads } => {
                write_reports.extend(writes);
                read_reports.extend(reads);
            }
            DeviceOutcome::Failed { device_idx, err } => {
                eprintln!("device {device_idx} failed: {err}");
                errors.push(err);
                SHUTDOWN.store(true, Ordering::Relaxed);
            }
        }
    }
    for h in joins {
        let _ = h.join();
    }

    if !write_reports.is_empty() {
        write_reports.sort_by_key(|r| r.shard_idx);
        print_phase("write", &args, total_shards, &write_reports);
    }
    if !read_reports.is_empty() {
        read_reports.sort_by_key(|r| r.shard_idx);
        println!();
        print_phase("read", &args, total_shards, &read_reports);
    }

    if errors.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

/// Build per-device `WorkerSpec`s from the host topology.
///
/// Returns `Some(specs)` of length `devices.len()` when every device
/// path could be mapped onto a topology NVMe controller; otherwise
/// prints a warning and returns `None` to signal "no pinning
/// available, use a `DefaultRuntime` fallback".
///
/// The three-class [`CorePlan`] reserves exactly one storage core per
/// NVMe drive, NUMA-local where possible and disjoint from every other
/// pinned core, so the bench pins each device's owning OS thread to its
/// drive's storage core.
fn build_pinned_specs(devices: &[PathBuf]) -> Option<Vec<WorkerSpec>> {
    let host = Host::discover();
    if host.cpus.is_empty() {
        eprintln!(
            "bench: topology empty (no CPUs visible in sysfs); pinning disabled, \
             running unpinned",
        );
        return None;
    }

    // Match every --device to a sysfs NVMe controller. If any path
    // fails, fall back wholesale: a partially-pinned run silently
    // mixes pinned and unpinned shards which is worse than
    // uniformly unpinned.
    let mut matched_nvmes: Vec<Nvme> = Vec::with_capacity(devices.len());
    for path in devices {
        let Some(ctrl) = controller_name(path) else {
            eprintln!(
                "bench: cannot derive NVMe controller name from `{}`; pinning disabled",
                path.display(),
            );
            return None;
        };
        let Some(nvme) = host.nvmes.iter().find(|n| n.dev_name == ctrl).cloned() else {
            eprintln!(
                "bench: no topology entry for controller `{ctrl}` (from `{}`); \
                 pinning disabled",
                path.display(),
            );
            return None;
        };
        matched_nvmes.push(nvme);
    }

    // Build a synthetic Host with just the matched NVMes (in --device
    // order) and no HCAs. CorePlan::for_host then reserves one
    // NUMA-local storage core per drive, disjoint from every other
    // pinned core.
    let synthetic = Host {
        cpus: host.cpus.clone(),
        numa_nodes: host.numa_nodes.clone(),
        hcas: Vec::new(),
        nvmes: matched_nvmes,
        nics: Vec::new(),
        isolated: host.isolated.clone(),
    };
    let plan = CorePlan::for_host(&synthetic, &CorePlanConfig::default());

    // Exactly one storage core per drive, in --device order.
    if plan.storage_cores.len() != devices.len() {
        eprintln!(
            "bench: topology plan produced {} storage cores, expected {}; pinning disabled",
            plan.storage_cores.len(),
            devices.len(),
        );
        return None;
    }
    let specs: Vec<WorkerSpec> = plan
        .storage_cores
        .iter()
        .map(|sc| WorkerSpec::new(sc.cpu, sc.numa))
        .collect();
    Some(specs)
}

/// Map a block-device path like `/dev/nvme0n1` or `/dev/nvme0` to
/// the sysfs controller name (`nvme0`). Returns `None` if the path
/// is not an NVMe-style device node.
fn controller_name(path: &Path) -> Option<String> {
    let name = path.file_name()?.to_str()?;
    let rest = name.strip_prefix("nvme")?;
    let digits: String = rest.chars().take_while(|c| c.is_ascii_digit()).collect();
    if digits.is_empty() {
        return None;
    }
    Some(format!("nvme{digits}"))
}

/// One pinned-OS-thread configuration. Owned by the shard thread;
/// the main thread never touches the engine, device, or backing.
struct ShardConfig {
    shard_idx: usize,
    total_shards: usize,
    thread_idx: usize,
    device_path: PathBuf,
    pinned_numa: Option<u16>,
    queue_depth: Option<u32>,
    workers: usize,
    worker_id_base: usize,
    seed: u64,
    duration: Duration,
    ops_cap_per_shard: Option<u64>,
    iopoll: bool,
    o_direct: bool,
    single_issuer: bool,
    defer_taskrun: bool,
    bypass_admission: bool,
    verify: bool,
    verify_ops_per_shard: u64,
    skip_recovery_scan_if_no_meta: bool,
}

/// Per-phase per-shard tally returned to the main thread.
struct ShardReport {
    shard_idx: usize,
    device_label: String,
    ops: u64,
    bytes: u64,
    errors: u64,
    read_misses: u64,
    elapsed: Duration,
    latency_samples: Vec<Duration>,
}

enum DeviceOutcome {
    Ok {
        writes: Vec<ShardReport>,
        reads: Vec<ShardReport>,
    },
    Failed {
        device_idx: usize,
        err: String,
    },
}

fn run_device(device_idx: usize, cfgs: Vec<ShardConfig>) -> DeviceOutcome {
    assert!(!cfgs.is_empty(), "run_device called with no shards");
    let label = device_label(&cfgs[0]);
    match run_device_inner(&cfgs) {
        Ok((writes, reads)) => DeviceOutcome::Ok { writes, reads },
        Err(err) => DeviceOutcome::Failed {
            device_idx,
            err: format!("{label}: {err}"),
        },
    }
}

fn shard_label(cfg: &ShardConfig) -> String {
    let numa = match cfg.pinned_numa {
        Some(n) => format!("numa={n}"),
        None => "numa=?".to_string(),
    };
    format!(
        "{}#{} ({})",
        cfg.device_path.display(),
        cfg.thread_idx,
        numa,
    )
}

/// Label for the device thread as a whole; the per-shard label is
/// derived per `ShardConfig` once the shards run inside that thread.
fn device_label(cfg: &ShardConfig) -> String {
    let numa = match cfg.pinned_numa {
        Some(n) => format!("numa={n}"),
        None => "numa=?".to_string(),
    };
    format!("{} ({})", cfg.device_path.display(), numa)
}

fn run_device_inner(cfgs: &[ShardConfig]) -> Result<(Vec<ShardReport>, Vec<ShardReport>), String> {
    // Every shard in `cfgs` targets the same device, was given the
    // same device-level knobs by `run_block`, and shares a single
    // OS thread. Sanity-check the invariants the rest of the
    // function relies on so a regression in `run_block` shows up
    // here rather than as a subtle correctness bug.
    let head = &cfgs[0];
    for c in &cfgs[1..] {
        debug_assert_eq!(c.device_path, head.device_path);
        debug_assert_eq!(c.iopoll, head.iopoll);
        debug_assert_eq!(c.single_issuer, head.single_issuer);
        debug_assert_eq!(c.defer_taskrun, head.defer_taskrun);
        debug_assert_eq!(c.o_direct, head.o_direct);
        debug_assert_eq!(c.queue_depth, head.queue_depth);
        debug_assert_eq!(c.bypass_admission, head.bypass_admission);
        debug_assert_eq!(c.verify, head.verify);
        debug_assert_eq!(
            c.skip_recovery_scan_if_no_meta,
            head.skip_recovery_scan_if_no_meta
        );
    }

    let mut ring_cfg = StorageRingConfig {
        iopoll: head.iopoll,
        single_issuer: head.single_issuer,
        defer_taskrun: head.defer_taskrun,
        ..StorageRingConfig::default()
    };
    if let Some(qd) = head.queue_depth {
        if qd == 0 {
            return Err("--queue-depth must be >= 1".into());
        }
        ring_cfg.queue_depth = qd;
    }
    // Device page size is the atomic write unit (4 KiB on commodity
    // SCSI/NVMe). Cache pages are PAGE_BYTES and each cache-page write
    // spans `PAGE_BYTES / 4096` consecutive LBAs. `o_direct` is passed
    // independently of `iopoll` (production NVMe sets both).
    let OpenDisk {
        device,
        ring,
        // Held for this storage core's lifetime: the ring addresses
        // this fd by its registered Fixed index, so it must outlive the
        // ring. Dropped last (after the ring) at end of scope.
        file: _disk_file,
    } = UringDevice::open(&head.device_path, ring_cfg, head.o_direct, 4096)
        .map_err(|e| format!("UringDevice::open: {e}"))?;
    let ring = Rc::new(ring);
    let device = Arc::new(device);

    // Install the ring so this thread's `CoreLocalDevice`, engine, and
    // engine-open I/O resolve it from the thread-local registry. MUST
    // run before `register_buffers` and `StorageEngine::open`, both of
    // which issue ring ops on this thread.
    set_current_storage_ring(ring.clone());

    // One PAGE_BYTES slot per worker across every shard on this
    // device, packed into a single page-aligned region. All shards
    // on this device share the same registered buffer; each worker
    // gets a unique `page_idx` so the slots are disjoint.
    // BackingKind::Heap avoids any host hugepage setup; flip to
    // Hugepage2Mb for a TLB benefit when hugepages are reserved.
    let total_workers: usize = cfgs.iter().map(|c| c.workers).sum();
    let page_count = total_workers.max(1);
    let backing_bytes = (page_count * PAGE_BYTES).next_multiple_of(HUGEPAGE_2MB);
    let backing = allocate(BackingRequest {
        kind: BackingKind::Heap,
        bytes: backing_bytes,
        numa: cfgs.first().and_then(|c| c.pinned_numa),
    })
    .map_err(|e| format!("backing allocate: {e}"))?;
    let backing_base = backing.base;

    fill_backing_random(
        backing_base,
        backing_bytes,
        head.seed ^ shard_seed(head.shard_idx),
    );

    // Register the backing region with the io_uring ring before
    // opening the engine. `BTreeIndex::open` issues meta writes through
    // the engine's own scratch pool (registered during open), but
    // registering the backing up front means the bufferpool's pages are
    // ready for `READ_FIXED` / `WRITE_FIXED` the moment the phases run.
    // `local.register_pages` later re-registers this same region; the
    // ring's buffer table accumulates entries and `resolve_buf_index`
    // matches the first containing region, so the duplicate is harmless.
    device
        .register_buffers(backing_base, backing_bytes)
        .map_err(|e| format!("device.register_buffers: {e:?}"))?;

    let engine_cfg = EngineConfig {
        // PAGE_BYTES (2 MiB) is the cache page size; the btree
        // and the device addressing operate in 4 KiB units (one
        // cache page = 512 contiguous LBAs). Keeping the btree
        // small confines per-commit metadata I/O to a few 4 KiB
        // writes regardless of user-write size.
        page_size_bytes: PAGE_BYTES,
        btree_page_bytes: 4096,
        bypass_admission: head.bypass_admission,
        skip_recovery_scan_if_no_meta: head.skip_recovery_scan_if_no_meta,
        ..Default::default()
    };
    let engine = Arc::new(open_engine_on_ring(
        &ring,
        StorageEngine::open(device.clone(), engine_cfg),
    )?);

    // Build a single-engine LocalStorage that every shard on this
    // device shares. The `CoreLocalDevice` is `Send`, but the ring it
    // resolves is pinned to this thread, which forces one OS thread per
    // device; sharing the engine across the `threads_per_device`
    // logical shards on that thread is what makes the multi-shard
    // configuration correct (single allocator + btree per LBA space).
    let local = Arc::new(LocalStorage::new(vec![engine.clone()]));
    local
        .register_pages(&backing)
        .map_err(|e| format!("register_pages: {e:?}"))?;

    // Assign each shard a disjoint `page_idx` range in the shared
    // backing so its workers never collide on a slot.
    let mut page_offsets: Vec<usize> = Vec::with_capacity(cfgs.len());
    {
        let mut cursor = 0usize;
        for c in cfgs {
            page_offsets.push(cursor);
            cursor += c.workers;
        }
    }

    if head.verify {
        let report =
            run_verify_device(&engine, &local, &device, backing_base, cfgs, &page_offsets)?;
        engine.close_mutator();
        drop(local);
        drop(engine);
        drop(device);
        drop(backing);
        // Release the thread-local ring before `ring` (and then
        // `_disk_file`) drop at scope end: the ring unregisters the file
        // fd, so it must drop before the `File` closes it.
        clear_current_storage_ring();
        return Ok((report, Vec::new()));
    }

    let phase1 = run_phase_device(
        Phase::Write,
        cfgs,
        &page_offsets,
        engine.clone(),
        local.clone(),
        device.clone(),
        PAGE_BYTES,
        None,
        backing_base,
    )?;

    let read_reports = if SHUTDOWN.load(Ordering::Relaxed) {
        Vec::new()
    } else {
        let read = run_phase_device(
            Phase::Read,
            cfgs,
            &page_offsets,
            engine.clone(),
            local.clone(),
            device.clone(),
            PAGE_BYTES,
            Some(phase1.iter().map(|p| p.keys_per_worker.clone()).collect()),
            backing_base,
        )?;
        read.into_iter()
            .zip(cfgs.iter())
            .map(|(r, c)| ShardReport {
                shard_idx: c.shard_idx,
                device_label: shard_label(c),
                ops: r.ops,
                bytes: r.bytes,
                errors: r.errors,
                read_misses: r.read_misses,
                elapsed: r.elapsed,
                latency_samples: r.latency_samples,
            })
            .collect()
    };

    let snapshot = engine.snapshot();
    engine.close_mutator();

    let write_reports: Vec<ShardReport> = phase1
        .into_iter()
        .zip(cfgs.iter())
        .map(|(p, c)| ShardReport {
            shard_idx: c.shard_idx,
            device_label: shard_label(c),
            ops: p.ops,
            bytes: p.bytes,
            errors: p.errors,
            read_misses: p.read_misses,
            elapsed: p.elapsed,
            latency_samples: p.latency_samples,
        })
        .collect();

    // Trust check: if the engine reports any write_io_errors,
    // the bench's "ops succeeded" count is no longer a reliable
    // measure of bytes-on-disk. Print so the operator sees it.
    if snapshot.write_io_errors > 0 || snapshot.read_io_errors > 0 || snapshot.checksum_misses > 0 {
        eprintln!(
            "device {} engine snapshot: write_io_errors={} read_io_errors={} \
             checksum_misses={} rejected_by_filter={} admitted={} hits={} \
             misses={} pending_free_len={}",
            head.device_path.display(),
            snapshot.write_io_errors,
            snapshot.read_io_errors,
            snapshot.checksum_misses,
            snapshot.rejected_by_filter,
            snapshot.admitted,
            snapshot.hits,
            snapshot.misses,
            snapshot.pending_free_len,
        );
    }

    drop(local);
    drop(engine);
    drop(device);
    drop(backing);
    // Release the thread-local ring before `ring` (and then
    // `_disk_file`) drop at scope end: the ring unregisters the file fd,
    // so it must drop before the `File` closes it.
    clear_current_storage_ring();

    Ok((write_reports, read_reports))
}

/// Closed-loop byte-equality round trip used by `--verify`. Writes
/// `cfg.verify_ops_per_shard` distinct payloads (one per key,
/// stamped with worker/seq) then reads each back and asserts byte
/// equality. Returns a single ShardReport summarizing the verify
/// pass under the "write" slot and `None` for the read slot so the
/// existing report-printing code does not have to special-case
/// verify.
fn run_verify_device(
    engine: &Arc<StorageEngine<CoreLocalDevice>>,
    local: &Arc<LocalStorage<CoreLocalDevice>>,
    device: &Arc<CoreLocalDevice>,
    backing_base: *mut u8,
    cfgs: &[ShardConfig],
    page_offsets: &[usize],
) -> Result<Vec<ShardReport>, String> {
    let phase_done = Arc::new(AtomicBool::new(false));

    let mutator_eng = engine.clone();
    let mutator_fut: Pin<Box<dyn std::future::Future<Output = ()>>> =
        Box::pin(mutator_eng.run_mutator());

    let progress_dev = device.clone();
    let phase_done_for_progress = phase_done.clone();
    let progress_fut: Pin<Box<dyn std::future::Future<Output = ()>>> = Box::pin(async move {
        loop {
            if let Err(e) = progress_dev.progress() {
                eprintln!("progress error: {e:?}");
                SHUTDOWN.store(true, Ordering::Relaxed);
                return;
            }
            yield_once().await;
            if SHUTDOWN.load(Ordering::Relaxed) || phase_done_for_progress.load(Ordering::Relaxed) {
                let _ = progress_dev.progress();
                return;
            }
        }
    });

    let mut all_futs: Vec<Pin<Box<dyn std::future::Future<Output = ()>>>> =
        Vec::with_capacity(2 + cfgs.len());
    all_futs.push(mutator_fut);
    all_futs.push(progress_fut);

    let body_results: Vec<Arc<Mutex<Option<Result<(u64, Duration), String>>>>> = (0..cfgs.len())
        .map(|_| Arc::new(Mutex::new(None)))
        .collect();
    let start = Instant::now();
    for (i, cfg) in cfgs.iter().enumerate() {
        // Each verify shard needs two disjoint pages: a write
        // source and a read destination. Reserve the first two
        // slots of this shard's `page_offsets` window for those;
        // the bench requires `--workers >= 2 * threads_per_device`
        // in verify mode so each shard has its pair.
        if cfg.workers < 2 {
            return Err(format!(
                "verify: shard {} needs workers >= 2 (got {}); rerun with \
                 --workers >= 2*--threads-per-device",
                cfg.shard_idx, cfg.workers,
            ));
        }
        let base = page_offsets[i] as u32;
        let src_idx = base;
        let dst_idx = base + 1;
        let local_for_body = local.clone();
        let engine_for_body = engine.clone();
        let result_slot = body_results[i].clone();
        let label = shard_label(cfg);
        let cfg_seed = cfg.seed;
        let total_shards = cfg.total_shards;
        let my_shard = cfg.shard_idx;
        let total_ops = cfg.verify_ops_per_shard;
        let body_fut: Pin<Box<dyn std::future::Future<Output = ()>>> = Box::pin(async move {
            let res = verify_round_trip(
                local_for_body,
                engine_for_body,
                backing_base,
                src_idx,
                dst_idx,
                cfg_seed,
                my_shard,
                total_shards,
                total_ops,
                start,
                &label,
            )
            .await;
            *result_slot.lock().unwrap() = Some(res);
        });
        all_futs.push(body_fut);
    }

    let shard_count = cfgs.len();
    block_on_many(all_futs, |statuses| {
        let workers_done = statuses[2..2 + shard_count].iter().all(|s| *s);
        if workers_done {
            phase_done.store(true, Ordering::Relaxed);
            statuses[0] = true;
        }
    });

    let snapshot = engine.snapshot();
    if snapshot.write_io_errors > 0 || snapshot.read_io_errors > 0 || snapshot.checksum_misses > 0 {
        return Err(format!(
            "verify: engine reported errors: write_io_errors={} read_io_errors={} \
             checksum_misses={}",
            snapshot.write_io_errors, snapshot.read_io_errors, snapshot.checksum_misses,
        ));
    }

    let mut reports = Vec::with_capacity(cfgs.len());
    for (i, cfg) in cfgs.iter().enumerate() {
        let (ops, elapsed) = body_results[i]
            .lock()
            .unwrap()
            .take()
            .unwrap_or_else(|| Err("verify: body future did not produce a result".into()))?;
        reports.push(ShardReport {
            shard_idx: cfg.shard_idx,
            device_label: shard_label(cfg),
            ops,
            bytes: ops * PAGE_BYTES as u64,
            errors: 0,
            read_misses: 0,
            elapsed,
            latency_samples: Vec::new(),
        });
    }
    Ok(reports)
}

#[allow(clippy::too_many_arguments)]
async fn verify_round_trip(
    local: Arc<LocalStorage<CoreLocalDevice>>,
    engine: Arc<StorageEngine<CoreLocalDevice>>,
    backing_base: *mut u8,
    src_idx: u32,
    dst_idx: u32,
    seed: u64,
    my_shard: usize,
    total_shards: usize,
    n: u64,
    start: Instant,
    label: &str,
) -> Result<(u64, Duration), String> {
    // Use two distinct PageRefs out of the registered region: one
    // for the write source, one for the read destination, so a
    // read that erroneously hits the source slot would still be
    // caught by the byte-equality check. The caller assigns
    // `src_idx`/`dst_idx` from this shard's disjoint window into
    // the device-wide backing.
    let src = PageRef {
        page_idx: src_idx,
        offset: 0,
        len: PAGE_BYTES as u32,
    };
    let dst = PageRef {
        page_idx: dst_idx,
        offset: 0,
        len: PAGE_BYTES as u32,
    };
    // SAFETY: `backing_base` is a freshly allocated region owned
    // by the device thread; the slots at `src_idx` and `dst_idx`
    // are disjoint per shard (the caller reserves a 2-page window
    // for each shard).
    let src_ptr = unsafe { backing_base.add(src_idx as usize * PAGE_BYTES) };
    let dst_ptr = unsafe { backing_base.add(dst_idx as usize * PAGE_BYTES) };

    // Generate keys upfront so each op writes a distinct key
    // routable to this shard.
    let mut keys: Vec<StripeKey> = Vec::with_capacity(n as usize);
    let mut seq: u64 = 0;
    while (keys.len() as u64) < n {
        let candidate = make_key(seed, my_shard as u64, seq);
        seq = seq.wrapping_add(1);
        if route_shard(&candidate, total_shards) == my_shard {
            keys.push(candidate);
        }
    }

    // Snapshot of "expected bytes" for each key. We stamp the
    // first 64 bytes of the source page with the op tag and copy
    // the entire page contents so the reader has the exact bytes
    // it should see.
    let mut expected: Vec<Vec<u8>> = Vec::with_capacity(n as usize);
    for (i, key) in keys.iter().enumerate() {
        // SAFETY: src_ptr is the start of a 4 KiB owned slot.
        let src_slice = unsafe { std::slice::from_raw_parts_mut(src_ptr, PAGE_BYTES) };
        stamp_page(src_slice, seed, my_shard as u64, i as u64, key);
        let snapshot = src_slice.to_vec();
        expected.push(snapshot);
        if let Err(e) = local.write_page(*key, 0, src).await {
            return Err(format!("verify[{label}] write op {i} failed: {e:?}"));
        }
    }

    // Trust gate: any device-level write error before this point
    // means the byte-equality compare below cannot be trusted.
    // Surface the engine's view of the world so the operator can
    // see at a glance whether the writes actually committed.
    let post_write = engine.snapshot();
    if post_write.write_io_errors > 0 {
        return Err(format!(
            "verify[{label}]: {} write_io_errors during write phase; aborting",
            post_write.write_io_errors,
        ));
    }

    for (i, key) in keys.iter().enumerate() {
        // Clear the destination slot so a partial / skipped read
        // is visible in the byte compare.
        // SAFETY: same disjoint-slot reasoning as above.
        unsafe {
            std::ptr::write_bytes(dst_ptr, 0, PAGE_BYTES);
        }
        let hit = match local.read_page(*key, 0, dst).await {
            Ok(b) => b,
            Err(e) => return Err(format!("verify[{label}] read op {i} failed: {e:?}")),
        };
        if !hit {
            return Err(format!(
                "verify[{label}] read op {i} missed key it just wrote",
            ));
        }
        // SAFETY: dst_ptr is the start of a 4 KiB owned slot.
        let dst_slice = unsafe { std::slice::from_raw_parts(dst_ptr, PAGE_BYTES) };
        if dst_slice != expected[i].as_slice() {
            // Find the first mismatch for a useful message.
            let mismatch = dst_slice
                .iter()
                .zip(expected[i].iter())
                .position(|(a, b)| a != b)
                .unwrap_or(0);
            return Err(format!(
                "verify[{label}] op {i} byte mismatch at offset {mismatch}: \
                 got 0x{:02x}, expected 0x{:02x}",
                dst_slice[mismatch], expected[i][mismatch],
            ));
        }
    }
    Ok((n, start.elapsed()))
}

/// Stamp a per-op tag into the first 64 bytes of `page`. The tag
/// mixes `(seed, worker_id, seq, key)` so every op produces a
/// content-distinct page even when the bench reuses the same
/// backing slot across many ops. The tail of the page keeps
/// whatever the caller put there (typically random bytes from
/// startup).
fn stamp_page(page: &mut [u8], seed: u64, worker_id: u64, seq: u64, key: &StripeKey) {
    debug_assert!(page.len() >= 64);
    page[0..8].copy_from_slice(&seed.to_le_bytes());
    page[8..16].copy_from_slice(&worker_id.to_le_bytes());
    page[16..24].copy_from_slice(&seq.to_le_bytes());
    page[24..32].copy_from_slice(b"UNB-OPv1");
    page[32..64].copy_from_slice(&key.0);
}

#[derive(Copy, Clone)]
enum Phase {
    Write,
    Read,
}

struct PhaseRun {
    ops: u64,
    bytes: u64,
    errors: u64,
    read_misses: u64,
    elapsed: Duration,
    latency_samples: Vec<Duration>,
    keys_per_worker: Vec<Vec<StripeKey>>,
}

#[allow(clippy::too_many_arguments)]
fn run_phase_device(
    phase: Phase,
    cfgs: &[ShardConfig],
    page_offsets: &[usize],
    engine: Arc<StorageEngine<CoreLocalDevice>>,
    local: Arc<LocalStorage<CoreLocalDevice>>,
    device: Arc<CoreLocalDevice>,
    page_size: usize,
    seed_keys: Option<Vec<Vec<Vec<StripeKey>>>>,
    backing_base: *mut u8,
) -> Result<Vec<PhaseRun>, String> {
    let phase_done = Arc::new(AtomicBool::new(false));
    let total_ops = Arc::new(AtomicU64::new(0));

    // Build per-shard worker states. Workers from different shards
    // get disjoint `page_idx` ranges in the shared device backing
    // so their source/destination slots never collide.
    let workers_per_shard: Vec<Vec<Arc<Mutex<WorkerState>>>> = cfgs
        .iter()
        .enumerate()
        .map(|(s_idx, cfg)| {
            (0..cfg.workers)
                .map(|i| {
                    let worker_id = cfg.worker_id_base + i;
                    let mut keys: Vec<StripeKey> = match phase {
                        Phase::Write => Vec::new(),
                        Phase::Read => seed_keys
                            .as_ref()
                            .and_then(|all| all.get(s_idx))
                            .and_then(|per_shard| per_shard.get(i).cloned())
                            .unwrap_or_default(),
                    };
                    if matches!(phase, Phase::Read) && !keys.is_empty() {
                        shuffle(&mut keys, cfg.seed ^ (worker_id as u64) ^ 0xDEAD_u64);
                    }
                    Arc::new(Mutex::new(WorkerState {
                        worker_id,
                        page_idx: (page_offsets[s_idx] + i) as u32,
                        seq: 0,
                        keys,
                        ops: 0,
                        bytes: 0,
                        errors: 0,
                        read_misses: 0,
                        latency: Vec::with_capacity(LATENCY_RING),
                        latency_cursor: 0,
                    }))
                })
                .collect()
        })
        .collect();

    let phase_start = Instant::now();
    let phase_duration = cfgs[0].duration;
    let ops_cap = cfgs[0].ops_cap_per_shard;

    let mutator_eng = engine.clone();
    let mutator_fut: Pin<Box<dyn std::future::Future<Output = ()>>> =
        Box::pin(mutator_eng.run_mutator());

    let progress_fut: Pin<Box<dyn std::future::Future<Output = ()>>> = {
        let device = device.clone();
        let phase_done = phase_done.clone();
        Box::pin(async move {
            loop {
                if let Err(e) = device.progress() {
                    eprintln!("progress error: {e:?}");
                    SHUTDOWN.store(true, Ordering::Relaxed);
                    return;
                }
                yield_once().await;
                if SHUTDOWN.load(Ordering::Relaxed) || phase_done.load(Ordering::Relaxed) {
                    let _ = device.progress();
                    return;
                }
            }
        })
    };

    let total_worker_count: usize = workers_per_shard.iter().map(|v| v.len()).sum();
    let mut all_futs: Vec<Pin<Box<dyn std::future::Future<Output = ()>>>> =
        Vec::with_capacity(2 + total_worker_count);
    all_futs.push(mutator_fut);
    all_futs.push(progress_fut);

    for (s_idx, shard_workers) in workers_per_shard.iter().enumerate() {
        let cfg = &cfgs[s_idx];
        let total_shards = cfg.total_shards;
        let my_shard = cfg.shard_idx;
        let seed = cfg.seed;
        for w in shard_workers {
            let worker = w.clone();
            let local = local.clone();
            let phase_done = phase_done.clone();
            let total_ops = total_ops.clone();
            let fut: Pin<Box<dyn std::future::Future<Output = ()>>> = match phase {
                Phase::Write => Box::pin(write_worker(
                    worker,
                    local,
                    phase_done,
                    total_ops,
                    phase_start,
                    phase_duration,
                    ops_cap,
                    seed,
                    my_shard,
                    total_shards,
                    page_size,
                    backing_base,
                )),
                Phase::Read => Box::pin(read_worker(
                    worker,
                    local,
                    phase_done,
                    total_ops,
                    phase_start,
                    phase_duration,
                    ops_cap,
                    page_size,
                )),
            };
            all_futs.push(fut);
        }
    }

    block_on_many(all_futs, |statuses| {
        // statuses[0] = mutator, statuses[1] = progress, [2..] = workers.
        let workers_done = statuses[2..2 + total_worker_count].iter().all(|s| *s);
        if workers_done {
            phase_done.store(true, Ordering::Relaxed);
            // The mutator only exits when `close_mutator` is called,
            // which `run_device_inner` does after both phases. Mark
            // it as done here so this `block_on_many` returns; the
            // outer loop will start the next phase or finalize.
            statuses[0] = true;
        }
    });

    let elapsed = phase_start.elapsed();

    let mut runs: Vec<PhaseRun> = Vec::with_capacity(cfgs.len());
    for shard_workers in &workers_per_shard {
        let mut ops = 0u64;
        let mut bytes = 0u64;
        let mut errors = 0u64;
        let mut read_misses = 0u64;
        let mut latency_samples: Vec<Duration> = Vec::new();
        let mut keys_per_worker: Vec<Vec<StripeKey>> = Vec::with_capacity(shard_workers.len());
        for w in shard_workers {
            let s = w.lock().unwrap();
            ops += s.ops;
            bytes += s.bytes;
            errors += s.errors;
            read_misses += s.read_misses;
            latency_samples.extend_from_slice(&s.latency);
            keys_per_worker.push(s.keys.clone());
        }
        runs.push(PhaseRun {
            ops,
            bytes,
            errors,
            read_misses,
            elapsed,
            latency_samples,
            keys_per_worker,
        });
    }

    Ok(runs)
}

struct WorkerState {
    worker_id: usize,
    page_idx: u32,
    seq: u64,
    keys: Vec<StripeKey>,
    ops: u64,
    bytes: u64,
    errors: u64,
    read_misses: u64,
    latency: Vec<Duration>,
    latency_cursor: usize,
}

impl WorkerState {
    fn record_latency(&mut self, dt: Duration) {
        if self.latency.len() < LATENCY_RING {
            self.latency.push(dt);
        } else {
            self.latency[self.latency_cursor] = dt;
            self.latency_cursor = (self.latency_cursor + 1) % LATENCY_RING;
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn write_worker(
    state: Arc<Mutex<WorkerState>>,
    local: Arc<LocalStorage<CoreLocalDevice>>,
    phase_done: Arc<AtomicBool>,
    total_ops: Arc<AtomicU64>,
    phase_start: Instant,
    phase_duration: Duration,
    ops_cap: Option<u64>,
    seed: u64,
    my_shard: usize,
    total_shards: usize,
    page_size: usize,
    backing_base: *mut u8,
) {
    let mut consecutive_errors: u64 = 0;
    loop {
        if SHUTDOWN.load(Ordering::Relaxed) || phase_done.load(Ordering::Relaxed) {
            return;
        }
        if phase_start.elapsed() >= phase_duration {
            return;
        }
        if let Some(cap) = ops_cap
            && total_ops.load(Ordering::Relaxed) >= cap
        {
            return;
        }
        let (worker_id, page_ref, page_idx) = {
            let s = state.lock().unwrap();
            (
                s.worker_id,
                PageRef {
                    page_idx: s.page_idx,
                    offset: 0,
                    len: page_size as u32,
                },
                s.page_idx,
            )
        };
        // Skip keys that route to a different shard so each shard's
        // workers only touch pages this shard owns. `route_shard`
        // hashes across the *global* shard count (devices x
        // threads-per-device), matching the layout `LocalStorage`
        // would use if every shard's engine lived under one router.
        let key;
        let op_seq;
        loop {
            let seq = {
                let mut s = state.lock().unwrap();
                let v = s.seq;
                s.seq += 1;
                v
            };
            let candidate = make_key(seed, worker_id as u64, seq);
            if route_shard(&candidate, total_shards) == my_shard {
                key = candidate;
                op_seq = seq;
                break;
            }
        }

        // Stamp the per-op tag into the page so the engine sees
        // content-distinct bytes for every op. Without this, a
        // future content-addressed dedup layer (or even just
        // identical-checksum coalescing) could collapse many
        // logically distinct ops to one physical write and the
        // throughput number would no longer reflect the work the
        // bench thinks it did.
        // SAFETY: `backing_base + page_idx * page_size` is the
        // start of this worker's PAGE_BYTES-sized slot, exclusive
        // to this worker for the lifetime of the run. The slice
        // is not aliased because each worker owns a distinct
        // `page_idx` (assigned at construction in run_phase).
        unsafe {
            let p = backing_base.add(page_idx as usize * page_size);
            let s = std::slice::from_raw_parts_mut(p, page_size);
            stamp_page(s, seed, worker_id as u64, op_seq, &key);
        }

        let t0 = Instant::now();
        let res = local.write_page(key, 0, page_ref).await;
        let dt = t0.elapsed();
        match res {
            Ok(()) => {
                consecutive_errors = 0;
                let mut s = state.lock().unwrap();
                s.ops += 1;
                s.bytes += page_size as u64;
                s.record_latency(dt);
                s.keys.push(key);
                total_ops.fetch_add(1, Ordering::Relaxed);
            }
            Err(_) => {
                consecutive_errors += 1;
                let mut s = state.lock().unwrap();
                s.errors += 1;
                if consecutive_errors >= MAX_CONSECUTIVE_ERRORS {
                    eprintln!(
                        "bench: worker {} hit {} consecutive write errors; setting SHUTDOWN",
                        worker_id, consecutive_errors,
                    );
                    SHUTDOWN.store(true, Ordering::Relaxed);
                    return;
                }
            }
        }
    }
}

async fn read_worker(
    state: Arc<Mutex<WorkerState>>,
    local: Arc<LocalStorage<CoreLocalDevice>>,
    phase_done: Arc<AtomicBool>,
    total_ops: Arc<AtomicU64>,
    phase_start: Instant,
    phase_duration: Duration,
    ops_cap: Option<u64>,
    page_size: usize,
) {
    let key_count;
    let page_ref;
    {
        let s = state.lock().unwrap();
        key_count = s.keys.len();
        page_ref = PageRef {
            page_idx: s.page_idx,
            offset: 0,
            len: page_size as u32,
        };
    }
    if key_count == 0 {
        return;
    }
    let mut idx: usize = 0;
    loop {
        if SHUTDOWN.load(Ordering::Relaxed) || phase_done.load(Ordering::Relaxed) {
            return;
        }
        if phase_start.elapsed() >= phase_duration {
            return;
        }
        if let Some(cap) = ops_cap
            && total_ops.load(Ordering::Relaxed) >= cap
        {
            return;
        }
        let key = {
            let s = state.lock().unwrap();
            s.keys[idx % key_count]
        };
        idx = idx.wrapping_add(1);

        let t0 = Instant::now();
        let res = local.read_page(key, 0, page_ref).await;
        let dt = t0.elapsed();
        match res {
            Ok(true) => {
                let mut s = state.lock().unwrap();
                s.ops += 1;
                s.bytes += page_size as u64;
                s.record_latency(dt);
                total_ops.fetch_add(1, Ordering::Relaxed);
            }
            Ok(false) => {
                let mut s = state.lock().unwrap();
                s.read_misses += 1;
            }
            Err(_) => {
                let mut s = state.lock().unwrap();
                s.errors += 1;
            }
        }
    }
}

/// Build a 32-byte content-addressed key from `(seed, worker_id, seq)`.
fn make_key(seed: u64, worker_id: u64, seq: u64) -> StripeKey {
    let mut buf = [0u8; 24];
    buf[0..8].copy_from_slice(&seed.to_le_bytes());
    buf[8..16].copy_from_slice(&worker_id.to_le_bytes());
    buf[16..24].copy_from_slice(&seq.to_le_bytes());
    let lo = twox_hash::xxh3::hash128_with_seed(&buf, 0);
    let hi = twox_hash::xxh3::hash128_with_seed(&buf, 0xA5A5_A5A5_A5A5_A5A5);
    let mut out = [0u8; 32];
    out[0..16].copy_from_slice(&lo.to_le_bytes());
    out[16..32].copy_from_slice(&hi.to_le_bytes());
    StripeKey(out)
}

/// Stable mapping from key to global shard index across all
/// (device, thread-slot) pairs. Mirrors the engine's internal
/// routing function so cross-shard workload partitioning lines up
/// with what a single unified `LocalStorage` would compute.
fn route_shard(key: &StripeKey, num_shards: usize) -> usize {
    debug_assert!(num_shards > 0);
    if num_shards == 1 {
        return 0;
    }
    let mut buf = [0u8; 40];
    buf[..32].copy_from_slice(&key.0);
    // stripe_off = 0 for the bench.
    let h = twox_hash::xxh3::hash64(&buf);
    (h % num_shards as u64) as usize
}

fn shard_seed(shard_idx: usize) -> u64 {
    // Mixed with the user-provided seed; any non-trivial mixer
    // works. xxh3-64 of the shard index.
    twox_hash::xxh3::hash64(&(shard_idx as u64).to_le_bytes())
}

fn fill_backing_random(base: *mut u8, len: usize, seed: u64) {
    let mut rng = ChaCha20Rng::seed_from_u64(seed);
    // SAFETY: `base` is the start of a freshly-allocated, exclusively
    // owned region of `len` bytes; no other references alias it during
    // bench startup.
    let slice = unsafe { std::slice::from_raw_parts_mut(base, len) };
    rng.fill_bytes(slice);
}

fn shuffle<T>(v: &mut [T], seed: u64) {
    if v.len() < 2 {
        return;
    }
    let mut rng = ChaCha20Rng::seed_from_u64(seed);
    for i in (1..v.len()).rev() {
        let j = (rng.next_u64() % (i as u64 + 1)) as usize;
        v.swap(i, j);
    }
}

// --- Output formatting -------------------------------------------------

fn print_phase(name: &str, args: &BlockArgs, total_shards: usize, reports: &[ShardReport]) {
    let total_ops: u64 = reports.iter().map(|r| r.ops).sum();
    let total_bytes: u64 = reports.iter().map(|r| r.bytes).sum();
    let total_errors: u64 = reports.iter().map(|r| r.errors).sum();
    let total_misses: u64 = reports.iter().map(|r| r.read_misses).sum();
    let elapsed = reports.iter().map(|r| r.elapsed).max().unwrap_or_default();
    let elapsed_secs = elapsed.as_secs_f64();
    let throughput_bps = if elapsed_secs > 0.0 {
        total_bytes as f64 / elapsed_secs
    } else {
        0.0
    };
    let ops_per_sec = if elapsed_secs > 0.0 {
        total_ops as f64 / elapsed_secs
    } else {
        0.0
    };

    let mut all_lat: Vec<Duration> = reports
        .iter()
        .flat_map(|r| r.latency_samples.iter().copied())
        .collect();
    all_lat.sort();

    let device_list = args
        .device
        .iter()
        .map(|d| d.display().to_string())
        .collect::<Vec<_>>()
        .join(", ");

    println!("bench storage block - {name} phase");
    println!("  devices:        {} ({})", args.device.len(), device_list,);
    println!(
        "  threads/device: {} (total shards: {})",
        args.threads_per_device, total_shards,
    );
    println!("  workers:        {}", args.workers);
    println!("  page size:      {}", human_bytes(PAGE_BYTES as u64));
    println!("  duration:       {:.2}s", elapsed_secs);
    println!(
        "  admission:      {}",
        if args.bypass_admission {
            "bypassed"
        } else {
            "enabled"
        },
    );
    println!();
    println!("  ops:            {}", total_ops);
    println!("  bytes:          {}", human_bytes(total_bytes));
    println!(
        "  throughput:     {}/s  ({:.0} ops/s)",
        human_bytes(throughput_bps as u64),
        ops_per_sec,
    );
    println!(
        "  latency p50:    {}",
        format_duration(percentile(&all_lat, 0.50))
    );
    println!(
        "  latency p99:    {}",
        format_duration(percentile(&all_lat, 0.99))
    );
    println!(
        "  latency max:    {}",
        format_duration(all_lat.last().copied().unwrap_or_default())
    );
    if total_errors > 0 {
        println!("  errors:         {}", total_errors);
    }
    if matches!(name, "read") && total_misses > 0 {
        println!("  read misses:    {}", total_misses);
    }
    println!();
    println!("  per-shard:");
    for r in reports {
        let secs = r.elapsed.as_secs_f64().max(f64::MIN_POSITIVE);
        let rate = r.bytes as f64 / secs;
        println!(
            "    {}:  {} ops   {}/s",
            r.device_label,
            r.ops,
            human_bytes(rate as u64),
        );
    }
}

fn percentile(sorted: &[Duration], p: f64) -> Duration {
    if sorted.is_empty() {
        return Duration::ZERO;
    }
    let idx = ((sorted.len() as f64) * p) as usize;
    sorted[idx.min(sorted.len() - 1)]
}

fn human_bytes(n: u64) -> String {
    const KIB: f64 = 1024.0;
    let n = n as f64;
    if n >= KIB.powi(3) {
        format!("{:.2} GiB", n / KIB.powi(3))
    } else if n >= KIB.powi(2) {
        format!("{:.2} MiB", n / KIB.powi(2))
    } else if n >= KIB {
        format!("{:.2} KiB", n / KIB)
    } else {
        format!("{} B", n as u64)
    }
}

fn format_duration(d: Duration) -> String {
    let us = d.as_micros();
    if us >= 1_000_000 {
        format!("{:.2}s", d.as_secs_f64())
    } else if us >= 1_000 {
        format!("{:.2}ms", us as f64 / 1_000.0)
    } else {
        format!("{}us", us)
    }
}

// --- Executor and signal handling -------------------------------------

fn install_signal_handler() {
    unsafe extern "C" fn handler(_sig: libc::c_int) {
        SHUTDOWN.store(true, Ordering::Relaxed);
    }
    // SAFETY: handler only does an atomic store and is therefore
    // async-signal-safe. The sigaction struct is zeroed before use.
    unsafe {
        let mut sa: libc::sigaction = std::mem::zeroed();
        sa.sa_sigaction = handler as *const () as usize;
        libc::sigemptyset(&mut sa.sa_mask);
        sa.sa_flags = 0;
        for sig in [libc::SIGINT, libc::SIGTERM] {
            if libc::sigaction(sig, &sa, std::ptr::null_mut()) != 0 {
                let e = std::io::Error::last_os_error();
                eprintln!("failed to install signal handler for sig={sig}: {e}");
            }
        }
    }
}

/// Drive a one-shot `StorageEngine::open` future to completion on this
/// storage core, pumping `ring.progress()` between polls so the btree's
/// open-time meta I/O (issued through the installed thread-local ring)
/// actually reaps. Mirrors the production bring-up loop in
/// `disks::uring::run_storage_core`. The ring must already be installed
/// via `set_current_storage_ring` so the engine's `CoreLocalDevice`
/// resolves it.
fn open_engine_on_ring<E: std::fmt::Debug>(
    ring: &StorageRing,
    fut: impl std::future::Future<Output = Result<StorageEngine<CoreLocalDevice>, E>>,
) -> Result<StorageEngine<CoreLocalDevice>, String> {
    use std::pin::pin;
    use std::task::{Context, Poll};
    let w = noop_waker();
    let mut cx = Context::from_waker(&w);
    let mut fut = pin!(fut);
    let mut spins: u64 = 0;
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(r) => return r.map_err(|e| format!("StorageEngine::open: {e:?}")),
            Poll::Pending => {
                ring.progress()
                    .map_err(|e| format!("ring progress during open: {e:?}"))?;
                spins += 1;
                if spins > 100_000_000 {
                    panic!("open_engine_on_ring stuck");
                }
            }
        }
    }
}

/// Round-robin executor: poll every future once per spin, allow the
/// caller to inspect / mutate the per-future "done" flags after each
/// spin, and exit when every status entry is `true`. The bench uses
/// this to drive the mutator + device-progress loop concurrently
/// with N workers without pulling in tokio.
fn block_on_many<F>(
    mut futs: Vec<Pin<Box<dyn std::future::Future<Output = ()>>>>,
    mut after_spin: F,
) where
    F: FnMut(&mut [bool]),
{
    use std::task::{Context, Poll};
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut done: Vec<bool> = vec![false; futs.len()];
    let mut spins_without_progress: u64 = 0;
    loop {
        let mut made_progress = false;
        for (i, f) in futs.iter_mut().enumerate() {
            if done[i] {
                continue;
            }
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(()) => {
                    done[i] = true;
                    made_progress = true;
                }
                Poll::Pending => {}
            }
        }
        after_spin(&mut done);
        if done.iter().all(|d| *d) {
            return;
        }
        if made_progress {
            spins_without_progress = 0;
        } else {
            spins_without_progress += 1;
            // Bench can legitimately spin a long time waiting on
            // io_uring CQEs; treat extreme spins as a stuck executor.
            if spins_without_progress > 1_000_000_000 {
                panic!("block_on_many: stuck without progress");
            }
        }
    }
}

/// Yield back to the executor exactly once.
fn yield_once() -> YieldOnce {
    YieldOnce { yielded: false }
}

struct YieldOnce {
    yielded: bool,
}

impl std::future::Future for YieldOnce {
    type Output = ();
    fn poll(mut self: Pin<&mut Self>, cx: &mut std::task::Context<'_>) -> std::task::Poll<()> {
        if self.yielded {
            std::task::Poll::Ready(())
        } else {
            self.yielded = true;
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    }
}
