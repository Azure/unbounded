// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::collections::HashMap;
use std::num::NonZeroUsize;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use clap::Parser;
use serde::{Deserialize, Serialize};

use unbounded_storage::bufferpool::{
    PeerId, Pool, PoolConfig, PoolGroup, Req, ShardDescriptor, StripeKey,
};
use unbounded_storage::config::{self, Config, FabricCfg};
use unbounded_storage::disk_supervisor::{
    DiskRegistry, LiveDiskTopology, LiveShardLocalStore, UringDiskTarget,
};
use unbounded_storage::storage::blockdev::BlockDeviceProxy;
use unbounded_storage::fabric::{self, ConnectionSpec, Fabric, FabricTransport, Provider, StaticPeer};
use unbounded_storage::runtime::{PinnedRuntime, Threading, WorkerIdx, WorkerSpec};
use unbounded_storage::topology::{Host, Plan, Role, Worker};

use unbounded_storage::backing::{BackingKind, BackingRequest, allocate};

const DEFAULT_CONFIG_PATH: &str = "/etc/unbounded-storage/config.toml";
const SHUTDOWN_POLL: Duration = Duration::from_millis(100);

/// Process-wide shutdown flag. Set by the signal handler (which
/// is restricted to async-signal-safe operations) and polled by
/// the main thread plus every shard thread.
static SHUTDOWN: AtomicBool = AtomicBool::new(false);

/// Placeholder request type for the per-shard `Pool`. Carries a
/// `StripeKey` and satisfies the trait bounds required by
/// `FabricTransport` (`Serialize` + `DeserializeOwned`). No reads
/// are issued against the pool today; this exists so `Pool::new`
/// type-checks.
#[derive(Clone, Serialize, Deserialize)]
struct PlaceholderReq {
    key: [u8; 32],
}

impl Req for PlaceholderReq {
    fn key(&self) -> StripeKey {
        StripeKey(self.key)
    }
}

type ShardPool = Pool<
    FabricTransport<PlaceholderReq, StaticPeer>,
    LiveShardLocalStore<BlockDeviceProxy>,
    PlaceholderReq,
>;

fn main() -> ExitCode {
    let cli = Cli::parse();
    let (config_path, config_explicit) = match cli.config.as_ref() {
        Some(p) => (p.clone(), true),
        None => (PathBuf::from(DEFAULT_CONFIG_PATH), false),
    };

    let mut config = match load_config(&config_path, config_explicit) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("config error: {e}");
            return ExitCode::FAILURE;
        }
    };
    // CLI overrides config for back-compat.
    if let Some(b) = cli.bytes_per_shard {
        config.storage.bytes_per_shard =
            unbounded_storage::config::schema::ByteSize(b.get());
    }
    if cli.no_hugepages {
        config.storage.backing_kind = config::BackingKindCfg::Heap;
    }
    let backing_kind = config::backing_kind_from_cfg(config.storage.backing_kind);
    let bytes_per_shard_total = config.storage.bytes_per_shard.bytes();

    let host = Host::discover();
    let plan = Plan::for_host(&host, &config::topology_cfg_to_plan_config(&config.topology));

    let counts = RoleCounts::from_plan(&plan);
    eprintln!(
        "topology plan: workers={} progress={} handlers={} numa_pools={:?}",
        plan.workers.len(),
        counts.progress,
        counts.handlers,
        plan.numa_pools,
    );

    // Only RDMA progress workers are spawned in this phase; handler
    // and NVMe placements are computed and logged for observability
    // but not yet wired up.
    let progress: Vec<Worker> = plan.rdma_progress().cloned().collect();
    if progress.is_empty() {
        eprintln!("topology plan produced no RDMA progress workers; exiting");
        return ExitCode::FAILURE;
    }

    let specs: Vec<WorkerSpec> = progress
        .iter()
        .map(|w| WorkerSpec::new(w.cpu, w.numa))
        .collect();
    let runtime = PinnedRuntime::new(specs);
    install_signal_handler();

    // Hot-swap publication surface for shards. The disk supervisor
    // opens each `UringBlockDevice` on its own progress thread,
    // wraps it in a `BlockDeviceProxy` so the resulting
    // `StorageEngine<BlockDeviceProxy>` is `Send + Sync`, and hands
    // the engine `Arc`s back here for publication. Shards observe
    // the snapshot through `LiveShardLocalStore`. Created before
    // the shard loop so each shard receives a clone.
    let topology: Arc<LiveDiskTopology<BlockDeviceProxy>> = LiveDiskTopology::new();

    // Each shard thread reports either an error or a populated
    // `ShardDescriptor` on this channel. The main thread aggregates
    // descriptors into a `PoolGroup` once every shard has reported.
    let (ready_tx, ready_rx) = mpsc::channel::<ShardReady>();
    let bytes_per_shard = bytes_per_shard_total / progress.len();
    let fabric_cfg = Arc::new(config.fabric.clone());
    let mut joins = Vec::with_capacity(progress.len());
    for (i, worker) in progress.iter().enumerate() {
        let widx = WorkerIdx(u16::try_from(i).expect("worker index fits in u16"));
        let dev_name = match worker.role {
            Role::RdmaProgress { hca } if hca != usize::MAX => {
                Some(host.hcas[hca].dev_name.clone())
            }
            _ => None,
        };
        let worker = worker.clone();
        let runtime = runtime.clone();
        let tx = ready_tx.clone();
        let fabric_cfg = fabric_cfg.clone();
        let topology = topology.clone();
        joins.push(
            thread::Builder::new()
                .name(format!("ub-storage-shard-{i}"))
                .spawn(move || {
                    let rt = runtime.clone();
                    rt.run_worker(
                        widx,
                        Box::new(move || {
                            run_shard(
                                widx,
                                worker,
                                dev_name,
                                runtime,
                                tx,
                                backing_kind,
                                bytes_per_shard,
                                fabric_cfg,
                                topology,
                            );
                        }),
                    );
                })
                .expect("spawn shard thread"),
        );
    }
    drop(ready_tx);

    // Drain shard readiness messages, separating successes from
    // failures. We wait for every shard so a partial bring-up
    // produces a coherent error path rather than a half-built
    // `PoolGroup`.
    let mut descriptors: Vec<ShardDescriptor> = Vec::with_capacity(joins.len());
    let mut shard_fabrics: Vec<(WorkerIdx, Arc<Fabric>)> = Vec::with_capacity(joins.len());
    let mut errors: Vec<String> = Vec::new();
    for msg in ready_rx {
        match msg {
            ShardReady::Up { descriptor, fabric } => {
                shard_fabrics.push((descriptor.worker_idx, fabric));
                descriptors.push(descriptor);
            }
            ShardReady::Failed(err) => {
                eprintln!("shard failed: {err}");
                errors.push(err);
                SHUTDOWN.store(true, Ordering::Relaxed);
            }
        }
    }

    let mut shard_state: Vec<(WorkerIdx, Arc<Fabric>, HashMap<PeerId, ConnectionSpec>)> =
        Vec::with_capacity(shard_fabrics.len());
    if errors.is_empty() {
        let mut total_added = 0usize;
        let mut total_failures = 0usize;
        for (widx, fabric) in &shard_fabrics {
            let report = config::reconcile_peers(fabric, &config.peers, None);
            total_added += report.added;
            total_failures += report.failures.len();
            for (peer_id, msg) in &report.failures {
                eprintln!(
                    "shard {}: peer {} failed to apply: {msg}",
                    widx.0, peer_id.0
                );
            }
            shard_state.push((*widx, fabric.clone(), report.applied));
        }
        if !config.peers.is_empty() {
            eprintln!(
                "config: peers applied across shards: applied={total_added} failures={total_failures}"
            );
        }
    }

    // Disk supervisor: open `[[disks]]` entries (progress threads
    // only, no data-path wiring yet). Open hints are derived from
    // the per-disk `numa` field where present; absent that, opens
    // are unpinned and `DiskRegistry` falls back to round-robin
    // across the empty hint list.
    let disk_cpu_hints: Vec<usize> = config
        .disks
        .iter()
        .filter_map(|d| d.numa)
        .filter_map(|n| host.cpus_on(Some(n)).first().copied().map(|c| c as usize))
        .collect();
    let mut disk_registry = DiskRegistry::new(UringDiskTarget, disk_cpu_hints);
    if errors.is_empty() {
        let report = disk_registry.reconcile(&config.disks);
        eprintln!(
            "config: disks: added={} removed={} failures={}",
            report.added,
            report.removed,
            report.failures.len(),
        );
        for (path, msg) in &report.failures {
            eprintln!("disk {}: open failed: {msg}", path.display());
        }
        topology.apply_engines(disk_registry.engines_snapshot());
    }

    // Build the process-wide `PoolGroup` over the successful
    // shards. Routing is content-addressed: hash the request's
    // `StripeKey` into the shard index. This is the simplest
    // routing that distributes load uniformly across shards and
    // is replaceable per design once topology-aware peer routing
    // lands.
    if errors.is_empty() && !descriptors.is_empty() {
        descriptors.sort_by_key(|d| d.worker_idx.0);
        let shard_count = descriptors.len();
        let _group: PoolGroup<PlaceholderReq> =
            PoolGroup::new(descriptors, move |req: &PlaceholderReq| {
                stripe_key_to_shard(&req.key(), shard_count)
            });
        // No consumer yet; the group is built to validate the
        // bring-up surface. A future change will hand it to
        // whichever subsystem first needs cross-shard fan-out.
        eprintln!("pool group up: shards={}", shard_count);
    }

    // Wait for shutdown, listening for config updates if the
    // watcher installs cleanly. Reconciling the updates back into
    // running subsystems is intentionally deferred to a later phase;
    // for now main only logs receipt.
    match config::ConfigWatcher::new(config_path.clone()) {
        Ok((_watcher, update_rx)) => {
            wait_for_shutdown_with_updates(
                update_rx,
                &mut shard_state,
                &mut disk_registry,
                topology.clone(),
            );
        }
        Err(e) => {
            eprintln!("config watch: not installed: {e}");
            wait_for_shutdown();
        }
    }
    eprintln!("shutdown signaled; tearing down shards");

    // Shard threads must exit first so they release any
    // `Arc<StorageEngine>` refs published via the topology. Then
    // drop the topology snapshot, then drain the disk supervisor
    // so each per-disk thread sees its engine refcount fall before
    // its stop flag.
    // (reverse order so the last-built shard tears down first)
    for h in joins.into_iter().rev() {
        if let Err(e) = h.join() {
            eprintln!("shard thread panicked: {e:?}");
            errors.push(format!("panic: {e:?}"));
        }
    }
    drop(topology);
    disk_registry.drain();

    if errors.is_empty() {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}

/// Body of one shard thread. Runs on the pinned executor: brings up
/// the `Fabric`, registers a NUMA-local `Backing`, wires up the
/// `Pool`, reports readiness, then idles until shutdown so the
/// `Fabric` (and its progress threads) plus the `Pool` are dropped
/// together.
fn run_shard(
    widx: WorkerIdx,
    worker: Worker,
    dev_name: Option<String>,
    runtime: Arc<dyn Threading>,
    tx: mpsc::Sender<ShardReady>,
    backing_kind: BackingKind,
    bytes_per_shard: usize,
    fabric_cfg: Arc<FabricCfg>,
    topology: Arc<LiveDiskTopology<BlockDeviceProxy>>,
) {
    // Default to the loopback device when no HCA is bound to this
    // shard; the `tcp` provider is the fallback path.
    let device_name = dev_name.clone().unwrap_or_else(|| "lo".to_string());
    let provider = match &dev_name {
        Some(name) => Provider::from_device_name(name),
        None => Provider::Tcp,
    };
    let mut cfg = fabric::defaults_for(device_name, runtime, widx);
    cfg.provider = provider;
    cfg.listen = true;
    cfg.listen_addr = Some(fabric_cfg.listen_addr.clone());
    cfg.max_inflight = fabric_cfg.max_inflight;
    cfg.progress_threads = fabric_cfg.progress_threads;
    cfg.progress_poll_us = fabric_cfg.progress_poll_us;
    cfg.numa = worker.numa;

    let fabric = match Fabric::new(cfg).and_then(|f| f.self_address().map(|a| (f, a))) {
        Ok((fabric, self_addr)) => {
            println!(
                "shard up: worker={} dev={} numa={} cpu={} self_addr_bytes={}",
                widx.0,
                dev_name.as_deref().unwrap_or("tcp-fallback"),
                worker
                    .numa
                    .map(|n| n.to_string())
                    .unwrap_or_else(|| "none".into()),
                worker.cpu,
                self_addr.len(),
            );
            Arc::new(fabric)
        }
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!("worker={}: {e}", widx.0)));
            return;
        }
    };

    // NUMA-local backing. Allocated on the pinned shard thread so
    // the `PinnedRuntime`'s `set_mempolicy` keeps the pages on the
    // intended node; the hugepage variant additionally pins via
    // `mbind` when `worker.numa` is known. Register it with the
    // fabric before building the transport (the embedder
    // pre-registers model replaces the old `Transport::register_pages`
    // handshake).
    let backing = match allocate(BackingRequest {
        kind: backing_kind,
        bytes: bytes_per_shard,
        numa: worker.numa,
    }) {
        Ok(b) => b,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: backing allocation failed: {e}",
                widx.0,
            )));
            return;
        }
    };
    let page_size = backing.page_size;
    let mr = match fabric.register_backing(&backing, worker.numa) {
        Ok(mr) => mr,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: register_backing: {e}",
                widx.0,
            )));
            return;
        }
    };

    // `StaticPeer { peer: PeerId(0) }` is a placeholder: with no
    // connections added to the fabric, no `bulk_get` will resolve,
    // but constructing the transport is harmless. A real router
    // will be installed when peer discovery lands.
    let transport = match FabricTransport::<PlaceholderReq, _>::new(
        fabric.clone(),
        mr,
        StaticPeer { peer: PeerId(0) },
        page_size,
    ) {
        Ok(t) => t,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: FabricTransport::new: {e}",
                widx.0,
            )));
            return;
        }
    };
    // Wire the per-shard view over the live disk topology. When
    // the topology is empty (no engines yet), `register_pages`
    // records the backing silently and reads/writes return
    // `Error::Transport("no disks open")`. Once the disk supervisor
    // publishes engines, the view's `current_or_replay` catches the
    // swap and replays buffer registration before delegating.
    let blockstore = LiveShardLocalStore::new(topology);
    let pool: ShardPool = match Pool::new(PoolConfig::default(), backing, transport, blockstore) {
        Ok(p) => p,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: Pool::new: {e}",
                widx.0,
            )));
            return;
        }
    };

    let _ = tx.send(ShardReady::Up {
        descriptor: ShardDescriptor {
            worker_idx: widx,
            numa: worker.numa,
        },
        fabric: fabric.clone(),
    });

    wait_for_shutdown();

    // Drop order matters: drop the `Pool` first so the
    // `FabricTransport` (which holds an `Arc<Fabric>`) goes away
    // before the owning `Fabric` runs its own drop. `Fabric::drop`
    // joins its progress threads and tears the libfabric stack
    // down in the documented order.
    drop(pool);
    drop(fabric);
}

/// Hash a `StripeKey` into a shard index. The first eight bytes of
/// the 32-byte key are interpreted as a little-endian `u64`; that
/// distributes uniformly under a content-addressed key (which is
/// already a hash) and avoids pulling in a hash crate. The modulus
/// is the shard count; `shard_count` is asserted non-zero by
/// `PoolGroup::new`.
fn stripe_key_to_shard(key: &StripeKey, shard_count: usize) -> usize {
    let bytes = &key.0[..8];
    let h = u64::from_le_bytes(bytes.try_into().expect("8 bytes"));
    (h as usize) % shard_count
}

/// Status a shard thread reports once it has either come up or
/// failed during bring-up.
enum ShardReady {
    Up {
        descriptor: ShardDescriptor,
        fabric: Arc<Fabric>,
    },
    Failed(String),
}

/// Per-role worker counts derived from a [`Plan`]; used for the
/// startup observability line.
#[derive(Copy, Clone, Debug, Default, PartialEq, Eq)]
struct RoleCounts {
    progress: usize,
    handlers: usize,
}

impl RoleCounts {
    fn from_plan(plan: &Plan) -> Self {
        let mut c = Self::default();
        for w in &plan.workers {
            match w.role {
                Role::RdmaProgress { .. } => c.progress += 1,
                Role::RdmaHandler { .. } => c.handlers += 1,
                Role::NvmeIoUring { .. } => {}
            }
        }
        c
    }
}

/// Parsed command-line options for one run of the daemon. All
/// flags are either absent (let the TOML config drive the field)
/// or override the matching `[storage]` knob for this run.
#[derive(Clone, Debug, Parser)]
#[command(
    name = "unbounded-storage",
    version,
    about = "Unbounded storage daemon",
    long_about = "Daemon process for the unbounded-storage subsystem. Reads its \
                  configuration from a TOML file (default: \
                  /etc/unbounded-storage/config.toml) and reloads peer and disk \
                  state in place when the file changes."
)]
struct Cli {
    /// Path to the TOML config file.
    ///
    /// If left at the default and the file is missing, the daemon
    /// continues with built-in defaults. An explicit path that is
    /// missing or invalid is fatal.
    #[arg(long, value_name = "PATH")]
    config: Option<PathBuf>,

    /// Override `[storage] backing_kind` to `heap`.
    ///
    /// Without this flag (and without an override in the config),
    /// the per-shard backing is allocated from 2 MiB hugepages.
    #[arg(long)]
    no_hugepages: bool,

    /// Override `[storage] bytes_per_shard`.
    ///
    /// Accepts a bare integer (bytes) or a string with a `K`, `M`,
    /// or `G` suffix interpreted as powers of 1024. Zero is
    /// rejected.
    #[arg(long, value_name = "BYTES", value_parser = parse_bytes)]
    bytes_per_shard: Option<NonZeroUsize>,
}

/// Load the daemon configuration. When the default path is used and
/// the file is absent, fall back to [`Config::default`] with a
/// warning. Any other failure - including explicit missing paths and
/// parse errors - is fatal.
fn load_config(path: &Path, explicit: bool) -> Result<Config, String> {
    match Config::load(path) {
        Ok(c) => Ok(c),
        Err(config::ConfigError::Io(e))
            if !explicit && e.kind() == std::io::ErrorKind::NotFound =>
        {
            eprintln!(
                "config: {} not found; continuing with built-in defaults",
                path.display()
            );
            Ok(Config::default())
        }
        Err(e) => Err(format!("loading {}: {e}", path.display())),
    }
}

/// Parse a byte count with an optional `K`/`M`/`G` suffix (powers
/// of 1024). Bare integers are bytes. Used as the `clap`
/// `value_parser` for `--bytes-per-shard`, so its `Err` strings are
/// surfaced directly to the user by clap.
fn parse_bytes(s: &str) -> Result<NonZeroUsize, String> {
    let s = s.trim();
    if s.is_empty() {
        return Err("empty byte count".into());
    }
    let (num, mult) = match s.as_bytes().last().copied() {
        Some(b'K') | Some(b'k') => (&s[..s.len() - 1], 1024usize),
        Some(b'M') | Some(b'm') => (&s[..s.len() - 1], 1024 * 1024),
        Some(b'G') | Some(b'g') => (&s[..s.len() - 1], 1024 * 1024 * 1024),
        _ => (s, 1usize),
    };
    let n: usize = num
        .parse()
        .map_err(|e| format!("invalid byte count {s:?}: {e}"))?;
    let total = n
        .checked_mul(mult)
        .ok_or_else(|| format!("byte count {s:?} overflows usize"))?;
    NonZeroUsize::new(total).ok_or_else(|| "byte count must be > 0".to_string())
}

fn wait_for_shutdown() {
    while !SHUTDOWN.load(Ordering::Acquire) {
        thread::sleep(SHUTDOWN_POLL);
    }
}

/// Same as [`wait_for_shutdown`] but also drains `ConfigUpdate`
/// events. Each update is reconciled against every shard's fabric;
/// the per-shard `last_applied` cache in `shard_state` lets us detect
/// address/numa drift for an existing peer id as a remove+add.
fn wait_for_shutdown_with_updates(
    update_rx: mpsc::Receiver<config::ConfigUpdate>,
    shard_state: &mut [(WorkerIdx, Arc<Fabric>, HashMap<PeerId, ConnectionSpec>)],
    disk_registry: &mut DiskRegistry<UringDiskTarget>,
    topology: Arc<LiveDiskTopology<BlockDeviceProxy>>,
) {
    while !SHUTDOWN.load(Ordering::Acquire) {
        match update_rx.recv_timeout(SHUTDOWN_POLL) {
            Ok(update) => {
                let mut added = 0usize;
                let mut removed = 0usize;
                let mut updated = 0usize;
                let mut failures = 0usize;
                let mut first_failure: Option<String> = None;
                for (widx, fabric, last_applied) in shard_state.iter_mut() {
                    let report = config::reconcile_peers(
                        fabric,
                        &update.config.peers,
                        Some(last_applied),
                    );
                    added += report.added;
                    removed += report.removed;
                    updated += report.updated;
                    failures += report.failures.len();
                    for (peer_id, msg) in &report.failures {
                        if first_failure.is_none() {
                            first_failure = Some(format!(
                                "shard {} peer {} {}",
                                widx.0, peer_id.0, msg
                            ));
                        }
                    }
                    *last_applied = report.applied;
                }
                let shards = shard_state.len();
                match first_failure {
                    Some(msg) => eprintln!(
                        "config gen={} peers: shards={shards} added={added} removed={removed} updated={updated} failures={failures} first_failure={msg}",
                        update.generation,
                    ),
                    None => eprintln!(
                        "config gen={} peers: shards={shards} added={added} removed={removed} updated={updated} failures={failures}",
                        update.generation,
                    ),
                }
                let disk_report = disk_registry.reconcile(&update.config.disks);
                eprintln!(
                    "config gen={} disks: added={} removed={} failures={}",
                    update.generation,
                    disk_report.added,
                    disk_report.removed,
                    disk_report.failures.len(),
                );
                for (path, msg) in &disk_report.failures {
                    eprintln!(
                        "config gen={} disk {}: open failed: {msg}",
                        update.generation,
                        path.display(),
                    );
                }
                // Republish a fresh `LocalStorage` snapshot on
                // every config update so any shard view caches its
                // new generation. Engines remain unpublished until
                // the per-thread open path lands.
        topology.apply_engines(disk_registry.engines_snapshot());
            }
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        }
    }
}

/// Install a SIGINT/SIGTERM handler that flips [`SHUTDOWN`]. The
/// handler is async-signal-safe (only a relaxed atomic store).
/// All threads observe the flag via their poll loops.
fn install_signal_handler() {
    unsafe extern "C" fn handler(_sig: libc::c_int) {
        // SAFETY: AtomicBool::store with Relaxed compiles to a
        // single machine store on every supported arch; it is
        // async-signal-safe. Readers synchronize via their own
        // Acquire loads.
        SHUTDOWN.store(true, Ordering::Relaxed);
    }
    // SAFETY: sigaction is invoked once at startup with a
    // zero-initialized sigaction whose handler does not touch
    // any non-async-signal-safe state.
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

#[cfg(test)]
mod tests {
    use super::*;
    use unbounded_storage::topology::NumaPool;

    use clap::error::ErrorKind;

    fn parse(args: &[&str]) -> Result<Cli, clap::Error> {
        let mut argv = vec!["unbounded-storage".to_string()];
        argv.extend(args.iter().map(|s| s.to_string()));
        Cli::try_parse_from(argv)
    }

    fn nz(n: usize) -> NonZeroUsize {
        NonZeroUsize::new(n).expect("non-zero")
    }

    #[test]
    fn defaults_to_hugepages() {
        let c = parse(&[]).unwrap();
        assert!(!c.no_hugepages);
        assert_eq!(c.bytes_per_shard, None);
        assert_eq!(c.config, None);
    }

    #[test]
    fn no_hugepages_selects_heap() {
        let c = parse(&["--no-hugepages"]).unwrap();
        assert!(c.no_hugepages);
    }

    #[test]
    fn bytes_per_shard_equals_form() {
        let c = parse(&["--bytes-per-shard=64M"]).unwrap();
        assert_eq!(c.bytes_per_shard, Some(nz(64 * 1024 * 1024)));
    }

    #[test]
    fn bytes_per_shard_space_form() {
        let c = parse(&["--bytes-per-shard", "2G"]).unwrap();
        assert_eq!(c.bytes_per_shard, Some(nz(2 * 1024 * 1024 * 1024)));
    }

    #[test]
    fn bytes_plain_integer_is_bytes() {
        let c = parse(&["--bytes-per-shard=4194304"]).unwrap();
        assert_eq!(c.bytes_per_shard, Some(nz(4 * 1024 * 1024)));
    }

    #[test]
    fn config_path_default_when_absent() {
        let c = parse(&[]).unwrap();
        assert_eq!(c.config, None);
    }

    #[test]
    fn config_path_explicit_equals_form() {
        let c = parse(&["--config=/tmp/foo.toml"]).unwrap();
        assert_eq!(c.config, Some(PathBuf::from("/tmp/foo.toml")));
    }

    #[test]
    fn config_path_explicit_space_form() {
        let c = parse(&["--config", "/tmp/foo.toml"]).unwrap();
        assert_eq!(c.config, Some(PathBuf::from("/tmp/foo.toml")));
    }

    #[test]
    fn help_flag_returns_help_action() {
        // clap signals `--help` by returning an `Err` whose
        // `ErrorKind` is `DisplayHelp`; calling `.exit()` on it
        // would print help and exit with success.
        let err = parse(&["--help"]).unwrap_err();
        assert_eq!(err.kind(), ErrorKind::DisplayHelp);
    }

    #[test]
    fn unknown_arg_is_rejected() {
        let err = parse(&["--nope"]).unwrap_err();
        assert_eq!(err.kind(), ErrorKind::UnknownArgument);
    }

    #[test]
    fn zero_bytes_rejected() {
        let err = parse(&["--bytes-per-shard=0"]).unwrap_err();
        // clap wraps value-parser errors in `ValueValidation`; the
        // underlying message from `parse_bytes` is in the rendered
        // error body.
        assert_eq!(err.kind(), ErrorKind::ValueValidation);
        let rendered = err.to_string();
        assert!(rendered.contains("must be > 0"), "got: {rendered}");
    }

    #[test]
    fn role_counts_aggregate_per_role() {
        // Synthetic plan: 2 progress, 3 handlers, 1 nvme. We do not
        // route this through `Plan::for_host`; we just want to
        // confirm `RoleCounts::from_plan` walks the worker list
        // correctly because main.rs feeds that into the startup
        // observability line. NvmeIoUring workers are intentionally
        // not counted: the production daemon no longer pins per-disk
        // progress threads from the topology plan, so any such
        // worker that survives in a synthetic fixture is ignored.
        let plan = Plan {
            workers: vec![
                Worker {
                    cpu: 1,
                    numa: Some(0),
                    role: Role::NvmeIoUring { nvme: 0 },
                },
                Worker {
                    cpu: 2,
                    numa: Some(0),
                    role: Role::RdmaProgress { hca: 0 },
                },
                Worker {
                    cpu: 3,
                    numa: Some(0),
                    role: Role::RdmaProgress { hca: 1 },
                },
                Worker {
                    cpu: 4,
                    numa: Some(0),
                    role: Role::RdmaHandler { hca: 0 },
                },
                Worker {
                    cpu: 5,
                    numa: Some(0),
                    role: Role::RdmaHandler { hca: 0 },
                },
                Worker {
                    cpu: 6,
                    numa: Some(0),
                    role: Role::RdmaHandler { hca: 1 },
                },
            ],
            numa_pools: vec![NumaPool {
                numa: 0,
                workers: 6,
            }],
        };
        let c = RoleCounts::from_plan(&plan);
        assert_eq!(
            c,
            RoleCounts {
                progress: 2,
                handlers: 3,
            }
        );
    }
}
