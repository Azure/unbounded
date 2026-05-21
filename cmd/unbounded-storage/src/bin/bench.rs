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
//! Threading model: `UringBlockDevice` is `!Send + !Sync` because the
//! ring is pinned to the thread that opened it. The design
//! (`docs design "io_uring"`) calls for N pinned threads per NVMe
//! drive, where each thread owns its own ring, on a CPU in that
//! drive's NUMA domain. We satisfy that by spawning one OS shard
//! thread per (device, thread-slot) pair via the crate's
//! `runtime::PinnedRuntime`, with placement derived from
//! `topology::Plan`. Each shard owns its own `UringBlockDevice`,
//! `StorageEngine`, hugepage `Backing`, and a single-engine
//! `LocalStorage` so the registration and routing paths are exercised;
//! cross-shard routing is partitioned at the workload generator (each
//! shard only writes / reads keys hashed to its own global shard idx).
//!
//! When the host's sysfs topology cannot be discovered or a `--device`
//! path cannot be mapped onto a topology NVMe controller, the bench
//! falls back to an unpinned `DefaultRuntime` with a warning so it
//! remains usable in containers and dev hosts.

use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::process::ExitCode;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, mpsc};
use std::time::{Duration, Instant};

use clap::{Parser, Subcommand};
use rand::{RngCore, SeedableRng};
use rand_chacha::ChaCha20Rng;

use unbounded_storage::backing::{
    BackingError, BackingKind, BackingRequest, HUGEPAGE_2MB, allocate,
};
use unbounded_storage::bufferpool::{BlockStore, PageRef, StripeKey};
use unbounded_storage::runtime::{
    DefaultRuntime, PinnedRuntime, Threading, WorkerIdx, WorkerSpec,
};
use unbounded_storage::storage::blockdev::{BlockDevice, UringBlockDevice, UringConfig};
use unbounded_storage::storage::{EngineConfig, LocalStorage, StorageEngine};
use unbounded_storage::topology::{Host, Nvme, Plan, PlanConfig, Role};

const _: () = assert!(HUGEPAGE_2MB == 2 * 1024 * 1024);

/// User-data page size used by the bench, in bytes.
///
/// Today the storage stack assumes the block device's atomic write
/// unit equals the engine's cache page size (the btree, allocator,
/// and engine all use the same LBA -> byte offset = LBA * page_size
/// formula). Until that mismatch is properly disentangled, the
/// bench operates at the device's atomic-write granularity (4 KiB
/// on commodity SCSI/NVMe). The hugepage backing is still 2 MiB so
/// it can be DMA'd directly, but each worker only addresses a
/// 4 KiB sub-slice of it.
const PAGE_BYTES: usize = 4096;
const _: () = assert!(HUGEPAGE_2MB.is_multiple_of(PAGE_BYTES));

/// Default PRNG seed for benchmark workloads.
const DEFAULT_SEED: u64 = 0xBE_DE_CA_FE_u64;

/// Default IO threads per NVMe drive. Matches the design's "fixed
/// small N (e.g. 2-4) per drive" guidance and mirrors
/// `PlanConfig::nvme_threads_per_drive`'s default.
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
#[command(
    name = "bench",
    about = "Benchmark harness for unbounded subsystems."
)]
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
    /// RDMA transport benchmark (not yet implemented).
    Rdma,
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
            StorageCmd::Rdma => {
                eprintln!("bench storage: rdma subcommand is not yet implemented");
                ExitCode::from(2)
            }
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

    // Fail fast: confirm hugepages are reservable on this host
    // before spawning shard threads. The probe is deliberately a
    // single page so it returns to the pool immediately.
    match allocate(BackingRequest {
        kind: BackingKind::Hugepage2Mb,
        bytes: HUGEPAGE_2MB,
        numa: None,
    }) {
        Ok(b) => drop(b),
        Err(e) => {
            eprintln!("bench: hugepage probe failed: {e}");
            if let BackingError::HugepageMmap { free_hugepages, .. } = &e
                && free_hugepages.unwrap_or(0) == 0
            {
                eprintln!(
                    "bench: reserve 2 MiB hugepages before running, e.g. \
                     `echo N > /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages`",
                );
            }
            return ExitCode::FAILURE;
        }
    }

    install_signal_handler();

    let threads_per_device = if args.threads_per_device > 1 {
        // Each shard opens its own StorageEngine, Allocator, and
        // BTreeIndex against the same on-disk LBA space. With more
        // than one engine per device, the per-engine allocators
        // race for freshly-allocated LBAs (every fresh allocator
        // starts at LBA 2) and the per-engine btrees both try to
        // commit to meta slots A/B, silently corrupting each
        // other's data. Until the engine supports a shared
        // allocator + btree across shards on one device, force a
        // single engine per device so the numbers are
        // trustworthy and reads can hit the keys writes published.
        eprintln!(
            "bench: --threads-per-device > 1 ({}) currently corrupts cross-shard \
             writes (per-engine Allocator races). Forcing threads-per-device=1.",
            args.threads_per_device,
        );
        1
    } else {
        args.threads_per_device
    };
    let _ = args.verify; // verify path now folds into the same single-engine constraint above.
    let total_shards = num_devices * threads_per_device;

    // Build per-shard placement specs from the topology module.
    // `build_pinned_specs` returns `Some` only when every --device
    // path was matched to a topology NVMe controller; otherwise we
    // fall back to an unpinned runtime so the bench still runs in
    // containers / dev hosts.
    let pinned_specs = build_pinned_specs(&args.device, threads_per_device);
    let runtime: Arc<dyn Threading> = match pinned_specs {
        Some(specs) => {
            debug_assert_eq!(specs.len(), total_shards);
            PinnedRuntime::new(specs)
        }
        None => DefaultRuntime::new(total_shards),
    };

    // Distribute workers across shards: first `workers % total_shards`
    // shards get one extra so the sum is exactly `--workers`.
    let base = args.workers / total_shards;
    let extra = args.workers % total_shards;
    let workers_per_shard: Vec<usize> = (0..total_shards)
        .map(|i| if i < extra { base + 1 } else { base })
        .collect();

    // Per-phase per-shard op cap. Spread the global cap evenly.
    let ops_cap_per_shard: Option<u64> =
        args.ops.map(|n| n.div_ceil(total_shards as u64).max(1));

    // Cumulative worker_id offset so every worker has a globally
    // unique id (used as both a routing tiebreaker and a ChaCha
    // sub-seed).
    let mut worker_id_offset = 0usize;

    let (tx, rx) = mpsc::channel::<ShardOutcome>();
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
    let mut joins = Vec::with_capacity(total_shards);
    for shard_idx in 0..total_shards {
        let device_idx = shard_idx / threads_per_device;
        let thread_idx = shard_idx % threads_per_device;
        let workers_for_shard = workers_per_shard[shard_idx];
        let pinned_numa = runtime.numa_of(WorkerIdx(shard_idx as u16));
        let cfg = ShardConfig {
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
        };
        worker_id_offset += workers_for_shard;
        let tx = tx.clone();
        let name = format!("bench-shard-{shard_idx}");
        let handle = runtime.spawn_aux(
            WorkerIdx(shard_idx as u16),
            &name,
            Box::new(move || {
                let outcome = run_shard(cfg);
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
            ShardOutcome::Ok { write, read } => {
                write_reports.push(write);
                if let Some(r) = read {
                    read_reports.push(r);
                }
            }
            ShardOutcome::Failed { shard_idx, err } => {
                eprintln!("shard {shard_idx} failed: {err}");
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

/// Build per-shard `WorkerSpec`s from the host topology.
///
/// Returns `Some(specs)` of length `devices.len() * threads_per_device`
/// when every device path could be mapped onto a topology NVMe
/// controller; otherwise prints a warning and returns `None` to signal
/// "no pinning available, use a `DefaultRuntime` fallback".
///
/// The returned specs are ordered so `specs[device_idx * N +
/// thread_idx]` is the slot for thread `thread_idx` on device
/// `device_idx`, matching the `(device_idx, thread_idx) -> shard_idx`
/// layout the caller uses elsewhere.
fn build_pinned_specs(
    devices: &[PathBuf],
    threads_per_device: usize,
) -> Option<Vec<WorkerSpec>> {
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

    // Build a synthetic Host with just the matched NVMes (in
    // --device order) and no HCAs / TCP fallback. Plan::for_host
    // then schedules CPUs disjointly across `threads_per_device`
    // workers per drive, NUMA-local where possible. We pass an
    // explicit PlanConfig overriding only the knobs we care about
    // here.
    let synthetic = Host {
        cpus: host.cpus.clone(),
        numa_nodes: host.numa_nodes.clone(),
        hcas: Vec::new(),
        nvmes: matched_nvmes,
        isolated: host.isolated.clone(),
    };
    let cfg = PlanConfig {
        nvme_threads_per_drive: threads_per_device,
        rdma_progress_per_hca: 0,
        rdma_handlers_per_hca: 0,
        tcp_fallback_threads: 0,
        ..Default::default()
    };
    let plan = Plan::for_host(&synthetic, &cfg);

    let specs: Vec<WorkerSpec> = plan
        .workers
        .iter()
        .filter(|w| matches!(w.role, Role::NvmeIoUring { .. }))
        .map(|w| WorkerSpec::new(w.cpu, w.numa))
        .collect();
    let expected = devices.len() * threads_per_device;
    if specs.len() != expected {
        // Defensive: Plan::for_host should always emit exactly
        // nvme_threads_per_drive workers per NVMe in the synthetic
        // host. If it doesn't, fall back rather than misalign the
        // shard <-> spec mapping.
        eprintln!(
            "bench: topology plan produced {} nvme workers, expected {}; pinning disabled",
            specs.len(),
            expected,
        );
        return None;
    }
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

enum ShardOutcome {
    Ok {
        write: ShardReport,
        read: Option<ShardReport>,
    },
    Failed {
        shard_idx: usize,
        err: String,
    },
}

fn run_shard(cfg: ShardConfig) -> ShardOutcome {
    let device_label = shard_label(&cfg);
    let shard_idx = cfg.shard_idx;
    match run_shard_inner(cfg, device_label.clone()) {
        Ok((write, read)) => ShardOutcome::Ok { write, read },
        Err(err) => ShardOutcome::Failed {
            shard_idx,
            err: format!("{device_label}: {err}"),
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

fn run_shard_inner(
    cfg: ShardConfig,
    device_label: String,
) -> Result<(ShardReport, Option<ShardReport>), String> {
    let mut uring_cfg = UringConfig {
        // The device must be told the same page size the engine
        // will write: the device enforces `src.len() ==
        // page_size` on every read/write, so a mismatch produces
        // -EINVAL on every IO and (until the engine started
        // propagating those errors) silently passed the bench
        // straight through to a no-op loop.
        page_size: PAGE_BYTES,
        ..UringConfig::default()
    };
    uring_cfg.iopoll = cfg.iopoll;
    uring_cfg.single_issuer = cfg.single_issuer;
    uring_cfg.defer_taskrun = cfg.defer_taskrun;
    uring_cfg.o_direct = cfg.o_direct;
    if let Some(qd) = cfg.queue_depth {
        if qd == 0 {
            return Err("--queue-depth must be >= 1".into());
        }
        uring_cfg.queue_depth = qd;
    }
    let device = Arc::new(
        UringBlockDevice::open(&cfg.device_path, uring_cfg)
            .map_err(|e| format!("UringBlockDevice::open: {e:?}"))?,
    );

    // One PAGE_BYTES slot per worker, packed into a single
    // hugepage-backed region. PAGE_BYTES is small (4 KiB today),
    // so we round up to whole 2 MiB hugepages.
    let page_count = cfg.workers.max(1);
    let backing_bytes = (page_count * PAGE_BYTES).next_multiple_of(HUGEPAGE_2MB);
    let backing = allocate(BackingRequest {
        kind: BackingKind::Hugepage2Mb,
        bytes: backing_bytes,
        numa: cfg.pinned_numa,
    })
    .map_err(|e| format!("backing allocate: {e}"))?;
    let backing_base = backing.base;

    // Fill the backing with deterministic pseudo-random bytes so
    // every worker's source page starts from distinct content.
    // The write workers then overwrite the first WRITE_TAG_BYTES
    // of their slot per op with a (seq, worker, seed) tag so the
    // engine sees distinct content per op (and so reads can be
    // byte-compared against the expected value).
    fill_backing_random(
        backing_base,
        backing_bytes,
        cfg.seed ^ shard_seed(cfg.shard_idx),
    );

    // Register the backing region with the io_uring ring before
    // opening the engine: `BTreeIndex::open` issues meta writes
    // through the device, and `READ_FIXED` / `WRITE_FIXED` need a
    // registered buffer. The later `local.register_pages` call is
    // tolerant of a duplicate `register_buffers` (it ignores the
    // -EBUSY) and still installs the bufferpool binding.
    device
        .register_buffers(backing_base, backing_bytes)
        .map_err(|e| format!("device.register_buffers: {e:?}"))?;

    let engine_cfg = EngineConfig {
        // Both the cache page size and the btree page size must
        // equal the device's atomic write unit until the engine
        // separates user-data page size from device LBA size.
        page_size_bytes: PAGE_BYTES,
        btree_page_bytes: PAGE_BYTES,
        bypass_admission: cfg.bypass_admission,
        skip_recovery_scan_if_no_meta: cfg.skip_recovery_scan_if_no_meta,
        ..Default::default()
    };
    let engine = Arc::new(
        block_on(StorageEngine::open(device.clone(), engine_cfg))
            .map_err(|e| format!("StorageEngine::open: {e:?}"))?,
    );

    // Build a single-engine LocalStorage on this shard. Engines
    // owning a UringBlockDevice are !Send so this construct cannot
    // be hoisted to the main thread; the design's "compose via
    // LocalStorage" intent is satisfied by exercising registration
    // and routing on each shard against its own one-engine router.
    let local = Arc::new(LocalStorage::new(vec![engine.clone()]));
    local
        .register_pages(backing_base, PAGE_BYTES, page_count)
        .map_err(|e| format!("register_pages: {e:?}"))?;

    if cfg.verify {
        return run_verify_phase(&cfg, engine, local, device, backing_base, device_label);
    }

    let phase1 = run_phase(
        Phase::Write,
        &cfg,
        engine.clone(),
        local.clone(),
        device.clone(),
        PAGE_BYTES,
        None,
        backing_base,
    )?;

    let read_report = if SHUTDOWN.load(Ordering::Relaxed) {
        None
    } else {
        let read = run_phase(
            Phase::Read,
            &cfg,
            engine.clone(),
            local.clone(),
            device.clone(),
            PAGE_BYTES,
            Some(phase1.keys_per_worker.clone()),
            backing_base,
        )?;
        Some(ShardReport {
            shard_idx: cfg.shard_idx,
            device_label: device_label.clone(),
            ops: read.ops,
            bytes: read.bytes,
            errors: read.errors,
            read_misses: read.read_misses,
            elapsed: read.elapsed,
            latency_samples: read.latency_samples,
        })
    };

    let snapshot = engine.snapshot();
    engine.close_mutator();

    let write_report = ShardReport {
        shard_idx: cfg.shard_idx,
        device_label,
        ops: phase1.ops,
        bytes: phase1.bytes,
        errors: phase1.errors,
        read_misses: phase1.read_misses,
        elapsed: phase1.elapsed,
        latency_samples: phase1.latency_samples,
    };

    // Trust check: if the engine reports any write_io_errors,
    // the bench's "ops succeeded" count is no longer a reliable
    // measure of bytes-on-disk. Print so the operator sees it.
    if snapshot.write_io_errors > 0 || snapshot.read_io_errors > 0 || snapshot.checksum_misses > 0
    {
        eprintln!(
            "shard {} engine snapshot: write_io_errors={} read_io_errors={} \
             checksum_misses={} rejected_by_filter={} admitted={} hits={} \
             misses={} pending_free_len={}",
            cfg.shard_idx,
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

    Ok((write_report, read_report))
}

/// Closed-loop byte-equality round trip used by `--verify`. Writes
/// `cfg.verify_ops_per_shard` distinct payloads (one per key,
/// stamped with worker/seq) then reads each back and asserts byte
/// equality. Returns a single ShardReport summarizing the verify
/// pass under the "write" slot and `None` for the read slot so the
/// existing report-printing code does not have to special-case
/// verify.
fn run_verify_phase(
    cfg: &ShardConfig,
    engine: Arc<StorageEngine<UringBlockDevice>>,
    local: Arc<LocalStorage<UringBlockDevice>>,
    device: Arc<UringBlockDevice>,
    backing_base: *mut u8,
    device_label: String,
) -> Result<(ShardReport, Option<ShardReport>), String> {
    let phase_done = Arc::new(AtomicBool::new(false));
    let phase_done_for_progress = phase_done.clone();
    let total_ops = cfg.verify_ops_per_shard;

    let mutator_eng = engine.clone();
    let mutator_fut: Pin<Box<dyn std::future::Future<Output = ()>>> =
        Box::pin(mutator_eng.run_mutator());

    let progress_dev = device.clone();
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

    let phase_done_for_body = phase_done.clone();
    let cfg_seed = cfg.seed;
    let total_shards = cfg.total_shards;
    let my_shard = cfg.shard_idx;
    let local_for_body = local.clone();
    let engine_for_body = engine.clone();
    let verify_label = device_label.clone();
    let start = Instant::now();
    let body_result: Arc<Mutex<Option<Result<(u64, Duration), String>>>> =
        Arc::new(Mutex::new(None));
    let body_result_for_fut = body_result.clone();
    let body_fut: Pin<Box<dyn std::future::Future<Output = ()>>> = Box::pin(async move {
        let res = verify_round_trip(
            local_for_body,
            engine_for_body,
            backing_base,
            cfg_seed,
            my_shard,
            total_shards,
            total_ops,
            start,
            &verify_label,
        )
        .await;
        *body_result_for_fut.lock().unwrap() = Some(res);
        phase_done_for_body.store(true, Ordering::Relaxed);
    });

    let workers_count = 1usize;
    let all_futs: Vec<Pin<Box<dyn std::future::Future<Output = ()>>>> =
        vec![mutator_fut, progress_fut, body_fut];
    block_on_many(all_futs, |statuses| {
        let workers_done = statuses[2..2 + workers_count].iter().all(|s| *s);
        if workers_done {
            phase_done.store(true, Ordering::Relaxed);
            statuses[0] = true;
        }
    });

    let (ops, elapsed) = body_result
        .lock()
        .unwrap()
        .take()
        .unwrap_or_else(|| Err("verify: body future did not produce a result".into()))?;
    let snapshot = engine.snapshot();
    engine.close_mutator();
    if snapshot.write_io_errors > 0 || snapshot.read_io_errors > 0 || snapshot.checksum_misses > 0
    {
        return Err(format!(
            "verify: engine reported errors: write_io_errors={} read_io_errors={} \
             checksum_misses={}",
            snapshot.write_io_errors, snapshot.read_io_errors, snapshot.checksum_misses,
        ));
    }

    let report = ShardReport {
        shard_idx: cfg.shard_idx,
        device_label,
        ops,
        bytes: ops * PAGE_BYTES as u64,
        errors: 0,
        read_misses: 0,
        elapsed,
        latency_samples: Vec::new(),
    };
    drop(local);
    drop(engine);
    drop(device);
    Ok((report, None))
}

async fn verify_round_trip(
    local: Arc<LocalStorage<UringBlockDevice>>,
    engine: Arc<StorageEngine<UringBlockDevice>>,
    backing_base: *mut u8,
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
    // caught by the byte-equality check.
    let src_idx = 0u32;
    let dst_idx = 1u32;
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
    // SAFETY: `backing_base` is a freshly allocated hugepage
    // region owned by the shard; the slots at indices 0 and 1 are
    // disjoint and not aliased by any other task during verify
    // (single-worker mode).
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
    if post_write.admitted < n {
        return Err(format!(
            "verify[{label}]: only {} of {n} writes were admitted; aborting",
            post_write.admitted,
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

fn run_phase(
    phase: Phase,
    cfg: &ShardConfig,
    engine: Arc<StorageEngine<UringBlockDevice>>,
    local: Arc<LocalStorage<UringBlockDevice>>,
    device: Arc<UringBlockDevice>,
    page_size: usize,
    seed_keys: Option<Vec<Vec<StripeKey>>>,
    backing_base: *mut u8,
) -> Result<PhaseRun, String> {
    let phase_done = Arc::new(AtomicBool::new(false));
    let total_ops = Arc::new(AtomicU64::new(0));

    let workers: Vec<Arc<Mutex<WorkerState>>> = (0..cfg.workers)
        .map(|i| {
            let worker_id = cfg.worker_id_base + i;
            let mut keys: Vec<StripeKey> = match phase {
                Phase::Write => Vec::new(),
                Phase::Read => seed_keys
                    .as_ref()
                    .map(|all| all.get(i).cloned().unwrap_or_default())
                    .unwrap_or_default(),
            };
            // Shuffle the read key list per worker so the read
            // phase doesn't replay phase 1's exact submission order.
            if matches!(phase, Phase::Read) && !keys.is_empty() {
                shuffle(
                    &mut keys,
                    cfg.seed ^ (worker_id as u64) ^ 0xDEAD_u64,
                );
            }
            Arc::new(Mutex::new(WorkerState {
                worker_id,
                page_idx: i as u32,
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
        .collect();

    let phase_start = Instant::now();
    let phase_duration = cfg.duration;
    let ops_cap = cfg.ops_cap_per_shard;

    // Build the futures: one mutator, one progress, plus N workers.
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
                    // One last drain so any in-flight CQEs wake their
                    // tasks before we exit.
                    let _ = device.progress();
                    return;
                }
            }
        })
    };

    let mut all_futs: Vec<Pin<Box<dyn std::future::Future<Output = ()>>>> =
        Vec::with_capacity(2 + cfg.workers);
    all_futs.push(mutator_fut);
    all_futs.push(progress_fut);

    let total_shards = cfg.total_shards;
    let my_shard = cfg.shard_idx;
    for w in &workers {
        let worker = w.clone();
        let local = local.clone();
        let phase_done = phase_done.clone();
        let total_ops = total_ops.clone();
        let seed = cfg.seed;
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

    // Single-threaded executor: round-robin poll every future on
    // each spin until they're all done. We mark the mutator and
    // progress futures as the "ambient" pair: they only finish
    // after the workers do (and after `close_mutator` runs).
    let workers_count = cfg.workers;
    block_on_many(all_futs, |statuses| {
        // statuses[0] = mutator, statuses[1] = progress, [2..] = workers.
        let workers_done = statuses[2..2 + workers_count].iter().all(|s| *s);
        if workers_done {
            phase_done.store(true, Ordering::Relaxed);
            // Mutator only exits when close_mutator is called; we
            // want it to keep running until reads finish on this
            // phase, but for the write phase we still need it. So
            // we let it finish by calling close_mutator from
            // run_shard_inner *after* both phases. Here, once the
            // workers and the progress task are done we treat the
            // phase as complete and break out.
            statuses[0] = true;
        }
    });

    let elapsed = phase_start.elapsed();

    // Aggregate.
    let mut ops = 0u64;
    let mut bytes = 0u64;
    let mut errors = 0u64;
    let mut read_misses = 0u64;
    let mut latency_samples: Vec<Duration> = Vec::new();
    let mut keys_per_worker: Vec<Vec<StripeKey>> = Vec::with_capacity(workers.len());
    for w in &workers {
        let s = w.lock().unwrap();
        ops += s.ops;
        bytes += s.bytes;
        errors += s.errors;
        read_misses += s.read_misses;
        latency_samples.extend_from_slice(&s.latency);
        keys_per_worker.push(s.keys.clone());
    }

    Ok(PhaseRun {
        ops,
        bytes,
        errors,
        read_misses,
        elapsed,
        latency_samples,
        keys_per_worker,
    })
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
    local: Arc<LocalStorage<UringBlockDevice>>,
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
    local: Arc<LocalStorage<UringBlockDevice>>,
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
    let elapsed = reports
        .iter()
        .map(|r| r.elapsed)
        .max()
        .unwrap_or_default();
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
    println!(
        "  devices:        {} ({})",
        args.device.len(),
        device_list,
    );
    println!(
        "  threads/device: {} (total shards: {})",
        args.threads_per_device, total_shards,
    );
    println!("  workers:        {}", args.workers);
    println!("  page size:      {}", human_bytes(PAGE_BYTES as u64));
    println!("  duration:       {:.2}s", elapsed_secs);
    println!(
        "  admission:      {}",
        if args.bypass_admission { "bypassed" } else { "enabled" },
    );
    println!();
    println!("  ops:            {}", total_ops);
    println!("  bytes:          {}", human_bytes(total_bytes));
    println!(
        "  throughput:     {}/s  ({:.0} ops/s)",
        human_bytes(throughput_bps as u64),
        ops_per_sec,
    );
    println!("  latency p50:    {}", format_duration(percentile(&all_lat, 0.50)));
    println!("  latency p99:    {}", format_duration(percentile(&all_lat, 0.99)));
    println!("  latency max:    {}", format_duration(all_lat.last().copied().unwrap_or_default()));
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

fn noop_waker() -> std::task::Waker {
    use std::task::{RawWaker, RawWakerVTable, Waker};
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: VTABLE clone/wake/drop are all no-ops referencing static data.
    unsafe { Waker::from_raw(raw()) }
}

/// Drive a future to completion on a single thread using a noop
/// waker. Used for one-shot `StorageEngine::open` futures during
/// shard bring-up.
fn block_on<F: std::future::Future>(fut: F) -> F::Output {
    use std::pin::pin;
    use std::task::{Context, Poll};
    let w = noop_waker();
    let mut cx = Context::from_waker(&w);
    let mut fut = pin!(fut);
    let mut spins: u64 = 0;
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                if spins > 100_000_000 {
                    panic!("block_on stuck");
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
    fn poll(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<()> {
        if self.yielded {
            std::task::Poll::Ready(())
        } else {
            self.yielded = true;
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    }
}
