// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Lifecycle of the process's shard layer and the binary's
//! [`ConfigApplyTarget`] implementation.
//!
//! A *shard layer* is the full set of per-shard threads spawned from a
//! single [`Config`], plus the handles needed to drive and tear them
//! down: the [`FabricGroup`] that owns every shared fabric endpoint (for
//! live peer/backend reconcile), the [`ShardControlGroup`] used to fan
//! config applies out to every shard, and a shared `layer_stop` flag
//! that retires just this layer's shards without touching the
//! process-wide shutdown signal.
//!
//! [`prepare_shard_layer`] brings a layer through phase B without
//! serving, [`activate_shard_layer`] starts RPC and releases the shards;
//! [`teardown_shard_layer`] drains and joins it (used at process
//! shutdown). [`ProcessApplyTarget`] realizes a live config apply
//! entirely in place via [`ProcessApplyTarget::apply_in_place`]:
//! routing is republished to every shard (blocking until each acks via
//! the control group), each shard reconciles its own backend/frontend
//! registries from the broadcast config, and projected cache disks are
//! reconciled in place against the shared channel directory. No shard
//! restart is ever required for a config change.
//!
//! [`ConfigController`]: crate::config::ConfigController
//! [`ConfigApplyTarget`]: crate::config::ConfigApplyTarget

use std::collections::HashSet;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use unbounded_storage::config::{
    self, ApplyError, ConfigApplyTarget, ConfigDiff, LoadedConfig, ShardControlGroup,
    ShardDecision, ShardTransactionId,
};
use unbounded_storage::fabric::PeerId;
use unbounded_storage::memory::HUGEPAGE_2MB;
use unbounded_storage::p2p::RouteTableHandle;
use unbounded_storage::runtime::{JoinHandle, Threading, WorkerIdx};
use unbounded_storage::storage::disks::{CacheDirectorySet, DiskRegistry, UringDiskTarget};
use unbounded_storage::topology::ServingShard;

use crate::StartupSettings;
use crate::fabric_group::{FabricGroup, FabricPlan, FabricUnitAddress, RpcShardPublish};

const SHARD_CONTROL_TIMEOUT: Duration = Duration::from_secs(60);
const SHARD_PHASE_TIMEOUT: Duration = Duration::from_secs(60);
const CLEANUP_TIMEOUT: Duration = Duration::from_secs(60);

struct ShardJoin {
    worker_idx: WorkerIdx,
    handle: JoinHandle,
}

struct CleanupWatchdog {
    disarm_tx: mpsc::Sender<()>,
    join: thread::JoinHandle<()>,
}

struct CleanupProgress {
    stage: &'static str,
    outstanding_workers: Vec<u16>,
}

struct CollectedReports<R> {
    reports: Vec<R>,
    errors: Vec<String>,
}

fn collect_worker_reports<R, F>(
    phase: &str,
    rx: &mpsc::Receiver<R>,
    expected: &[WorkerIdx],
    timeout: Duration,
    worker_of: F,
) -> CollectedReports<R>
where
    F: Fn(&R) -> WorkerIdx,
{
    let deadline = Instant::now() + timeout;
    let expected: HashSet<WorkerIdx> = expected.iter().copied().collect();
    let mut outstanding = expected.clone();
    let mut reports = Vec::with_capacity(expected.len());
    let mut errors = Vec::new();

    while !outstanding.is_empty() {
        let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
            errors.push(format_outstanding(phase, "timed out", &outstanding));
            break;
        };
        match rx.recv_timeout(remaining) {
            Ok(report) => {
                let worker_idx = worker_of(&report);
                if !expected.contains(&worker_idx) {
                    errors.push(format!(
                        "{phase} received unexpected report from worker={}",
                        worker_idx.0
                    ));
                } else if !outstanding.remove(&worker_idx) {
                    errors.push(format!(
                        "{phase} received duplicate report from worker={}",
                        worker_idx.0
                    ));
                } else {
                    reports.push(report);
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {
                errors.push(format_outstanding(phase, "timed out", &outstanding));
                break;
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                errors.push(format_outstanding(
                    phase,
                    "report channel disconnected",
                    &outstanding,
                ));
                break;
            }
        }
    }

    CollectedReports { reports, errors }
}

fn format_outstanding(phase: &str, reason: &str, outstanding: &HashSet<WorkerIdx>) -> String {
    let mut workers: Vec<u16> = outstanding.iter().map(|worker| worker.0).collect();
    workers.sort_unstable();
    format!("{phase} {reason}; outstanding workers={workers:?}")
}

impl CleanupWatchdog {
    fn spawn<F, T>(timeout: Duration, on_timeout: F, terminate: T) -> Self
    where
        F: FnOnce() + Send + 'static,
        T: FnOnce() + Send + 'static,
    {
        let (disarm_tx, disarm_rx) = mpsc::channel();
        let join = thread::spawn(move || match disarm_rx.recv_timeout(timeout) {
            Ok(()) => {}
            Err(mpsc::RecvTimeoutError::Timeout | mpsc::RecvTimeoutError::Disconnected) => {
                on_timeout();
                terminate();
            }
        });
        Self { disarm_tx, join }
    }

    fn production(progress: Arc<Mutex<CleanupProgress>>) -> Self {
        Self::spawn(
            CLEANUP_TIMEOUT,
            move || {
                let progress = progress.lock().unwrap_or_else(|error| error.into_inner());
                eprintln!(
                    "fatal: shard cleanup stalled for 60 seconds during {}; outstanding workers={:?}; forcing process exit because registered memory cannot be released safely",
                    progress.stage, progress.outstanding_workers
                );
            },
            || std::process::exit(1),
        )
    }

    fn disarm(self) {
        let _ = self.disarm_tx.send(());
        let _ = self.join.join();
    }
}

/// Inputs that are constant across the life of the process and used to
/// spawn the shard layer. Cloned cheaply into every shard thread.
pub struct ShardSpawnDeps {
    /// Pinned runtime the shards are spawned on.
    pub runtime: Arc<dyn Threading>,
    /// One entry per serving shard: the [`ServingShard`] core placement
    /// and the HCA device name bound to it (`None` for the TCP-fallback
    /// path).
    pub workers: Vec<(ServingShard, Option<String>)>,
    /// Startup-fixed settings (fabric endpoint/thread knobs, backing
    /// allocator kind, total memory pool) sourced from CLI flags / env
    /// vars. Shared and not reloadable.
    pub settings: Arc<StartupSettings>,
    /// Live per-cache disk-channel directories every shard reads through.
    /// Shared and reconciled in place, never rebuilt per layer.
    pub cache_directories: Arc<CacheDirectorySet>,
    /// How serving shards map onto fabric endpoints (one endpoint per
    /// shard on the TCP path, one per HCA device on verbs). Built once at
    /// startup and realized into the layer's [`FabricGroup`].
    pub fabric_plan: FabricPlan,
}

/// A prepared set of shard threads parked after phase B, before RPC
/// startup and serving activation.
pub struct PreparedShardLayer {
    joins: Vec<ShardJoin>,
    fabric_group: FabricGroup,
    control: ShardControlGroup,
    layer_stop: Arc<AtomicBool>,
    backing_keepalives: Vec<Arc<dyn Send + Sync>>,
    rpc_shards: Vec<RpcShardPublish>,
    serve_start_txs: Vec<mpsc::Sender<()>>,
    terminal_rx: mpsc::Receiver<crate::ShardTerminalReport>,
    routes: RouteTableHandle,
}

/// A spawned set of shard threads plus the handles to drive and retire
/// them. Produced by [`activate_shard_layer`], consumed by
/// [`teardown_shard_layer`].
pub struct ShardLayer {
    /// Join handles for every shard thread, in spawn order.
    joins: Vec<ShardJoin>,
    /// Owns every shared fabric endpoint (and its RPC server) the shards
    /// run on. Drives in-place peer and RPC-side backend reconcile, and
    /// is dropped during teardown after all shard threads have joined but
    /// before their backings are freed.
    fabric_group: FabricGroup,
    /// Blocking fan-out/fan-in over every shard's control channel.
    control: ShardControlGroup,
    /// Retires just this layer's shards (layer teardown at shutdown)
    /// without tripping the process-wide [`crate::SHUTDOWN`]. Each shard
    /// ORs it into its run-loop predicate.
    layer_stop: Arc<AtomicBool>,
    /// Shared Drop carriers for every shard's backing allocation. Held
    /// here so each mapping outlives all shard threads: a coordinator
    /// shard's io_uring ring registers peer backings as `SEND_ZC`
    /// sources, and those rings are only provably gone once every shard
    /// thread has joined. Dropped last in [`teardown_shard_layer`],
    /// strictly after all joins, so no ring ever references unmapped
    /// memory.
    _backing_keepalives: Vec<Arc<dyn Send + Sync>>,
    terminal_rx: mpsc::Receiver<crate::ShardTerminalReport>,
    /// The sole writer for the process-wide routing publication. Every
    /// shard transport and fabric RPC handler holds a clone.
    routes: RouteTableHandle,
}

impl ShardLayer {
    pub fn fabric_unit_addresses(&self) -> Vec<FabricUnitAddress> {
        self.fabric_group.unit_addresses()
    }
}

/// Prepare a shard layer from `loaded` on the runtime in `deps`,
/// blocking until every shard has completed phase B. The returned
/// shards remain parked and no RPC server has started.
///
/// Spawns one thread per worker in `deps.workers`, each with its own
/// control channel (collected into the returned [`ShardControlGroup`])
/// and a shared fresh `layer_stop`. Collects readiness over a bounded
/// receive (the up shards park holding their sender and never
/// disconnect, so an unbounded drain would hang), then reconciles the
/// startup peer set into every shard's fabric.
///
/// Returns `Err` with the collected per-shard error messages if any
/// shard failed to come up; any shards that *did* come up are torn down
/// first so no threads leak.
pub fn prepare_shard_layer(
    loaded: Arc<LoadedConfig>,
    deps: &ShardSpawnDeps,
) -> Result<PreparedShardLayer, Vec<String>> {
    let layer_stop = Arc::new(AtomicBool::new(false));
    let worker_count = deps.workers.len();
    let settings = &deps.settings;
    let shard_backing_sizes = shard_backing_sizes(settings.memory_total_bytes, worker_count)
        .map_err(|e| vec![format!("invalid shard memory budget: {e}")])?;
    let config = loaded.config();
    let runtime = loaded.runtime();
    let routes = RouteTableHandle::from_snapshot(loaded.routes().clone());
    let runtime_peers = config::runtime_peers(runtime);
    let self_peer = local_self_peer(runtime)
        .map_err(|e| vec![format!("unsupported fabric identity config: {e}")])?;

    // Bring up the shared fabric endpoints before spawning any shards:
    // each shard registers its data backing against the endpoint it maps
    // onto. RPC servers start after shards publish their pool MRs/fetch
    // channels because owner responses source those bufferpool pages.
    let fabric_group = FabricGroup::new(
        &deps.runtime,
        &deps.fabric_plan,
        settings.backing_kind,
        &settings.fabric,
        &config.backends,
        deps.cache_directories.clone(),
        &routes,
        &runtime_peers,
        self_peer,
    )?;

    let (ready_tx, ready_rx) = mpsc::channel::<crate::ShardReady>();
    // Phase-B rendezvous: each shard reports here once it has registered
    // every peer's backing and built its fan-out surface. Kept separate
    // from `ready_tx` so the layer can wait for the second rendezvous
    // (peer registration) after broadcasting the full peer set.
    let (phaseb_tx, phaseb_rx) = mpsc::channel::<crate::PhaseBReport>();
    let (terminal_tx, terminal_rx) = mpsc::channel::<crate::ShardTerminalReport>();
    let mut joins = Vec::with_capacity(worker_count);
    let mut control_senders = Vec::with_capacity(worker_count);
    // Per-shard senders for broadcasting the assembled peer set in phase
    // B. Dropping these unblocks any shard parked on `peer_rx.recv()`
    // when bring-up fails before the broadcast.
    let mut peer_txs: Vec<mpsc::Sender<Arc<Vec<crate::PeerPublish>>>> =
        Vec::with_capacity(worker_count);
    let mut serve_start_txs = Vec::with_capacity(worker_count);

    for (i, (shard, _)) in deps.workers.iter().enumerate() {
        let widx = WorkerIdx(u16::try_from(i).expect("worker index fits in u16"));
        let backing_size = shard_backing_sizes[i];
        let (ctrl_tx, ctrl_rx) = mpsc::channel::<config::ShardCommand>();
        control_senders.push((widx, ctrl_tx));
        let (peer_tx, peer_rx) = mpsc::channel::<Arc<Vec<crate::PeerPublish>>>();
        peer_txs.push(peer_tx);
        let (serve_start_tx, serve_start_rx) = mpsc::channel::<()>();
        serve_start_txs.push(serve_start_tx);
        let phaseb_tx = phaseb_tx.clone();

        let shard = *shard;
        let fabric = fabric_group.fabric_for_shard(i);
        let tx = ready_tx.clone();
        let backing_kind = settings.backing_kind;
        let cache_directories = deps.cache_directories.clone();
        let route_handle = routes.clone();
        let loaded = loaded.clone();
        let layer_stop = layer_stop.clone();
        let terminal_tx = terminal_tx.clone();
        let rt = deps.runtime.clone();
        let handle = rt.spawn_pinned(
            widx,
            &format!("ub-storage-shard-{i}"),
            Box::new(move || {
                crate::report_on_panic(widx, move || {
                    crate::run_shard(
                        widx,
                        shard,
                        fabric,
                        tx,
                        backing_kind,
                        backing_size,
                        cache_directories,
                        route_handle,
                        loaded,
                        ctrl_rx,
                        peer_rx,
                        phaseb_tx,
                        serve_start_rx,
                        terminal_tx,
                        layer_stop,
                    );
                });
            }),
        );
        joins.push(ShardJoin {
            worker_idx: widx,
            handle,
        });
    }
    drop(ready_tx);
    // The layer keeps no phase-B sender of its own: collection below is
    // bounded by the number of shards that came up, and each live shard
    // holds its sender, so this never closes the channel prematurely.
    drop(phaseb_tx);
    drop(terminal_tx);

    let expected_workers: Vec<WorkerIdx> = joins.iter().map(|join| join.worker_idx).collect();
    let mut publishes = Vec::new();
    let collected = collect_worker_reports(
        "shard phase A",
        &ready_rx,
        &expected_workers,
        SHARD_PHASE_TIMEOUT,
        |report| match report {
            crate::ShardReady::Up { worker_idx, .. }
            | crate::ShardReady::Failed { worker_idx, .. } => *worker_idx,
        },
    );
    let mut errors = collected.errors;
    for report in collected.reports {
        match report {
            crate::ShardReady::Up {
                worker_idx,
                publish,
            } => publishes.push((worker_idx, publish)),
            crate::ShardReady::Failed {
                worker_idx: _,
                message,
            } => {
                eprintln!("shard failed: {message}");
                errors.push(message);
            }
        }
    }

    if !errors.is_empty() {
        // Retire any shards that did come up so a partially-built layer
        // never leaks threads, then surface the failures to the caller.
        // Dropping the peer senders unblocks any up-shard parked on
        // `peer_rx.recv()` so it can exit and be joined.
        drop(peer_txs);
        let backing_keepalives = publishes
            .into_iter()
            .map(|(_, publish)| publish.backing_keepalive)
            .collect();
        cleanup_shards(
            joins,
            fabric_group,
            ShardControlGroup::new(control_senders, SHARD_CONTROL_TIMEOUT),
            layer_stop,
            backing_keepalives,
            serve_start_txs,
            terminal_rx,
            routes,
        );
        return Err(errors);
    }

    // Assemble the broadcast peer set in worker-index order so
    // `shard_index` is stable across every shard. Each shard locates its
    // own entry, registers the others' backings, and reports phase-B
    // readiness below.
    publishes.sort_by_key(|(widx, _)| widx.0);
    // Retain every shard's backing Drop carrier for the layer's whole
    // life. Split out here as the `ShardPublish` values are consumed
    // into the broadcast `PeerPublish` set (which deliberately carries
    // only base/len, not ownership).
    let mut backing_keepalives: Vec<Arc<dyn Send + Sync>> = Vec::with_capacity(publishes.len());
    let mut rpc_shards = Vec::with_capacity(publishes.len());
    let peer_list: Arc<Vec<crate::PeerPublish>> = Arc::new(
        publishes
            .into_iter()
            .enumerate()
            .map(|(shard_index, (worker_idx, publish))| {
                let crate::ShardPublish {
                    backing_base,
                    backing_len,
                    fabric_mr,
                    numa,
                    fetch_channel,
                    backing_keepalive,
                } = publish;
                backing_keepalives.push(backing_keepalive);
                rpc_shards.push(RpcShardPublish {
                    shard_index,
                    fetch_channel: fetch_channel.clone(),
                    mr: fabric_mr,
                    numa,
                });
                crate::PeerPublish {
                    shard_index,
                    worker_idx,
                    backing_base,
                    backing_len,
                    channel: fetch_channel,
                    numa,
                }
            })
            .collect(),
    );
    for tx in &peer_txs {
        // A send error means that shard died after phase A; it will be
        // surfaced as a missing/closed phase-B report below.
        let _ = tx.send(peer_list.clone());
    }

    // Phase-B collection: every up-shard reports exactly once (the
    // `PhaseBGuard` guarantees this even on early return or panic).
    let collected = collect_worker_reports(
        "shard phase B",
        &phaseb_rx,
        &expected_workers,
        SHARD_PHASE_TIMEOUT,
        |report| match report {
            crate::PhaseBReport::Ready(worker_idx)
            | crate::PhaseBReport::Failed { worker_idx, .. } => *worker_idx,
        },
    );
    let mut phaseb_errors = collected.errors;
    for report in collected.reports {
        if let crate::PhaseBReport::Failed {
            worker_idx: _,
            message,
        } = report
        {
            eprintln!("shard phase-B failed: {message}");
            phaseb_errors.push(message);
        }
    }
    if !phaseb_errors.is_empty() {
        cleanup_shards(
            joins,
            fabric_group,
            ShardControlGroup::new(control_senders, SHARD_CONTROL_TIMEOUT),
            layer_stop,
            backing_keepalives,
            serve_start_txs,
            terminal_rx,
            routes,
        );
        return Err(phaseb_errors);
    }

    Ok(PreparedShardLayer {
        joins,
        fabric_group,
        control: ShardControlGroup::new(control_senders, SHARD_CONTROL_TIMEOUT),
        layer_stop,
        backing_keepalives,
        rpc_shards,
        serve_start_txs,
        terminal_rx,
        routes,
    })
}

/// Start recursive RPC servers and release every prepared shard into
/// its serve loop. Any failure retires and joins the entire prepared
/// layer before returning.
pub fn activate_shard_layer(prepared: PreparedShardLayer) -> Result<ShardLayer, Vec<String>> {
    let PreparedShardLayer {
        joins,
        mut fabric_group,
        control,
        layer_stop,
        backing_keepalives,
        rpc_shards,
        serve_start_txs,
        terminal_rx,
        routes,
    } = prepared;

    if let Err(errors) = fabric_group.start_rpc_servers(&rpc_shards) {
        cleanup_shards(
            joins,
            fabric_group,
            control,
            layer_stop,
            backing_keepalives,
            serve_start_txs,
            terminal_rx,
            routes,
        );
        return Err(errors);
    }
    for tx in &serve_start_txs {
        if tx.send(()).is_err() {
            cleanup_shards(
                joins,
                fabric_group,
                control,
                layer_stop,
                backing_keepalives,
                serve_start_txs,
                terminal_rx,
                routes,
            );
            return Err(vec![
                "shard exited before recursive RPC servers were ready".to_string(),
            ]);
        }
    }
    drop(serve_start_txs);

    Ok(ShardLayer {
        joins,
        fabric_group,
        control,
        layer_stop,
        _backing_keepalives: backing_keepalives,
        terminal_rx,
        routes,
    })
}

/// Retire a prepared layer that must not be activated.
pub fn retire_prepared_shard_layer(prepared: PreparedShardLayer) {
    let PreparedShardLayer {
        joins,
        fabric_group,
        control,
        layer_stop,
        backing_keepalives,
        rpc_shards: _,
        serve_start_txs,
        terminal_rx,
        routes,
    } = prepared;
    cleanup_shards(
        joins,
        fabric_group,
        control,
        layer_stop,
        backing_keepalives,
        serve_start_txs,
        terminal_rx,
        routes,
    );
}

fn cleanup_shards(
    joins: Vec<ShardJoin>,
    fabric_group: FabricGroup,
    control: ShardControlGroup,
    layer_stop: Arc<AtomicBool>,
    backing_keepalives: Vec<Arc<dyn Send + Sync>>,
    serve_start_txs: Vec<mpsc::Sender<()>>,
    terminal_rx: mpsc::Receiver<crate::ShardTerminalReport>,
    routes: RouteTableHandle,
) -> bool {
    let mut outstanding_workers: Vec<u16> = joins.iter().map(|join| join.worker_idx.0).collect();
    outstanding_workers.sort_unstable();
    let cleanup_progress = Arc::new(Mutex::new(CleanupProgress {
        stage: "shard joins",
        outstanding_workers,
    }));
    let watchdog = CleanupWatchdog::production(cleanup_progress.clone());
    layer_stop.store(true, Ordering::Relaxed);
    drop(serve_start_txs);
    // Reverse joins mirror spawn order. Only after every shard is gone may
    // control, fabric, routes, and finally registered backing ownership drop.
    let mut failed = false;
    for join in joins.into_iter().rev() {
        let worker_idx = join.worker_idx;
        if let Err(error) = join.handle.join() {
            eprintln!(
                "shard worker={} panicked during cleanup: {error:?}",
                worker_idx.0
            );
            failed = true;
        }
        cleanup_progress
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .outstanding_workers
            .retain(|worker| *worker != worker_idx.0);
    }
    for report in terminal_rx.try_iter() {
        eprintln!(
            "shard worker={} terminal failure: {}",
            report.worker_idx.0, report.message
        );
        failed = true;
    }
    drop(control);
    cleanup_progress
        .lock()
        .unwrap_or_else(|error| error.into_inner())
        .stage = "fabric teardown";
    drop(fabric_group);
    cleanup_progress
        .lock()
        .unwrap_or_else(|error| error.into_inner())
        .stage = "registered backing teardown";
    drop(routes);
    drop(backing_keepalives);
    drop(terminal_rx);
    watchdog.disarm();
    failed
}

fn shard_backing_sizes(
    memory_total_bytes: usize,
    shard_count: usize,
) -> Result<Vec<usize>, String> {
    if shard_count == 0 {
        return Ok(Vec::new());
    }

    let total_pages = memory_total_bytes / HUGEPAGE_2MB;
    if total_pages < shard_count {
        return Err(format!(
            "{memory_total_bytes} bytes provides {total_pages} whole 2 MiB pages for {shard_count} serving shards"
        ));
    }

    let pages_per_shard = total_pages / shard_count;
    let remainder = total_pages % shard_count;
    Ok((0..shard_count)
        .map(|i| (pages_per_shard + usize::from(i < remainder)) * HUGEPAGE_2MB)
        .collect())
}

fn local_self_peer(projection: &config::RuntimeGraph) -> Result<PeerId, String> {
    Ok(projection.mesh.self_peer_id)
}

/// Retire a shard layer: signal its shards to exit, then join every
/// thread in reverse spawn order so teardown mirrors bring-up.
pub fn teardown_shard_layer(layer: ShardLayer) -> bool {
    let ShardLayer {
        joins,
        fabric_group,
        control,
        layer_stop,
        _backing_keepalives,
        terminal_rx,
        routes,
    } = layer;
    cleanup_shards(
        joins,
        fabric_group,
        control,
        layer_stop,
        _backing_keepalives,
        Vec::new(),
        terminal_rx,
        routes,
    )
}

/// The binary's [`ConfigApplyTarget`]: owns the live shard layer and
/// the disk registry, and realizes config applies in place against
/// them.
pub struct ProcessApplyTarget {
    /// `Option` so the layer can be moved out at shutdown via
    /// [`Self::into_parts`]. Always `Some` while the process is serving.
    layer: Option<ShardLayer>,
    disk_registry: DiskRegistry<UringDiskTarget>,
    cache_directories: Arc<CacheDirectorySet>,
    next_transaction_id: u64,
}

impl ProcessApplyTarget {
    pub fn new(
        layer: ShardLayer,
        disk_registry: DiskRegistry<UringDiskTarget>,
        cache_directories: Arc<CacheDirectorySet>,
    ) -> Self {
        Self {
            layer: Some(layer),
            disk_registry,
            cache_directories,
            next_transaction_id: 1,
        }
    }

    /// Reconcile projected cache disks in place and republish the
    /// resulting channel set to the live directory (idempotent when the
    /// disk set is unchanged).
    fn reconcile_disks(&mut self, projection: &config::RuntimeGraph) -> Result<(), ApplyError> {
        let report = crate::reconcile_cache_disks(
            &mut self.disk_registry,
            &self.cache_directories,
            projection,
        );
        if report.failures.is_empty() {
            return Ok(());
        }

        let failure_count = report.failures.len();
        let mut failures = report.failures;
        failures.sort_by(|a, b| a.0.cmp(&b.0));
        let details = failures
            .into_iter()
            .map(|(path, error)| format!("{}: {error}", path.display()))
            .collect::<Vec<_>>()
            .join("; ");
        Err(ApplyError::Target(format!(
            "{} disk(s) failed to open: {details}",
            failure_count
        )))
    }

    /// Consume the target at shutdown, returning the live layer (if any)
    /// and the disk registry so the caller can tear them down in the
    /// correct order (shards first, then disks).
    pub fn into_parts(self) -> (Option<ShardLayer>, DiskRegistry<UringDiskTarget>) {
        (self.layer, self.disk_registry)
    }

    fn allocate_transaction_id(&mut self) -> Result<ShardTransactionId, ApplyError> {
        let id = ShardTransactionId(self.next_transaction_id);
        self.next_transaction_id = self.next_transaction_id.checked_add(1).ok_or_else(|| {
            ApplyError::Target("shard transaction id space exhausted".to_string())
        })?;
        Ok(id)
    }
}

fn fail_stop_disk_reconcile<T>(
    result: Result<T, ApplyError>,
    shutdown: &AtomicBool,
) -> Result<T, ApplyError> {
    if result.is_err() {
        shutdown.store(true, Ordering::Release);
    }
    result
}

impl ConfigApplyTarget for ProcessApplyTarget {
    fn apply_in_place(
        &mut self,
        new: &Arc<LoadedConfig>,
        diff: &ConfigDiff,
    ) -> Result<(), ApplyError> {
        if diff.requires_restart() {
            let reason = if diff.identity_changed {
                "self peer identity changed"
            } else {
                "backend stripe geometry changed"
            };
            return Err(ApplyError::Target(format!("{reason}; restart required")));
        }

        // The shards must see a new config whenever their routing surface,
        // graph projection, per-disk page-cache policy, or per-shard
        // backend/frontend registries need to change. Cache graph changes can
        // also alter frontend backend/bypass resolution, so they are broadcast.
        let needs_broadcast = diff.requires_routing_reload()
            || diff.caches_changed
            || diff.disks_changed
            || diff.backends_changed
            || diff.frontends_changed;

        if needs_broadcast {
            let id = self.allocate_transaction_id()?;

            let prepare = self
                .layer
                .as_ref()
                .expect("shard layer present between applies")
                .control
                .broadcast_prepare(id, new.clone(), *diff);
            if let Err(error) = prepare {
                if error.apply_state_is_indeterminate() {
                    crate::SHUTDOWN.store(true, Ordering::Release);
                    return Err(error);
                }
                if let Err(abort_error) = self
                    .layer
                    .as_ref()
                    .expect("shard layer present between applies")
                    .control
                    .broadcast_finish(id, ShardDecision::Abort)
                {
                    crate::SHUTDOWN.store(true, Ordering::Release);
                    return Err(abort_error);
                }
                return Err(error);
            }

            if diff.disks_changed {
                let drain = self
                    .layer
                    .as_ref()
                    .expect("shard layer present between applies")
                    .control
                    .broadcast_drain_page_cache(id);
                if let Err(error) = drain {
                    if error.apply_state_is_indeterminate() {
                        crate::SHUTDOWN.store(true, Ordering::Release);
                        return Err(error);
                    }
                    if let Err(abort_error) = self
                        .layer
                        .as_ref()
                        .expect("shard layer present between applies")
                        .control
                        .broadcast_finish(id, ShardDecision::Abort)
                    {
                        crate::SHUTDOWN.store(true, Ordering::Release);
                        return Err(abort_error);
                    }
                    return Err(error);
                }
            }

            // Opening a disk may provision a file or format metadata. Cross
            // the fail-stop boundary before reconciliation because closing a
            // staged handle cannot undo either operation.
            if diff.caches_changed || diff.disks_changed {
                if let Err(error) =
                    fail_stop_disk_reconcile(self.reconcile_disks(new.runtime()), &crate::SHUTDOWN)
                {
                    return Err(error);
                }
            }

            // Shared fabric state cannot be rolled back either. Reconcile it
            // only after shard preparation and disk reconciliation succeed.
            {
                let layer = self
                    .layer
                    .as_mut()
                    .expect("shard layer present between applies");
                if diff.requires_peer_reconcile() {
                    let runtime_peers = config::runtime_peers(new.runtime());
                    if let Err(failures) = layer.fabric_group.reconcile_peers(&runtime_peers) {
                        crate::SHUTDOWN.store(true, Ordering::Release);
                        return Err(ApplyError::Target(format!(
                            "hard peer reconciliation failure(s): {}",
                            failures.join("; ")
                        )));
                    }
                }
                if diff.backends_changed {
                    layer
                        .fabric_group
                        .reconcile_backends(&new.config().backends);
                }
            }

            // Route publication is irrevocable. A subsequent commit failure
            // therefore makes the process unsafe to continue.
            if diff.requires_routing_reload() {
                self.layer
                    .as_ref()
                    .expect("shard layer present between applies")
                    .routes
                    .store_snapshot(new.routes().clone());
            }

            let layer = self
                .layer
                .as_ref()
                .expect("shard layer present between applies");
            if let Err(error) = layer.control.broadcast_finish(id, ShardDecision::Commit) {
                crate::SHUTDOWN.store(true, Ordering::Release);
                return Err(error);
            }
        } else {
            // Keep non-shard process surfaces correct if a future diff flag
            // stops requiring a shard transaction.
            if diff.caches_changed || diff.disks_changed {
                if let Err(error) =
                    fail_stop_disk_reconcile(self.reconcile_disks(new.runtime()), &crate::SHUTDOWN)
                {
                    return Err(error);
                }
            }
            if diff.requires_routing_reload() {
                self.layer
                    .as_ref()
                    .expect("shard layer present between applies")
                    .routes
                    .store_snapshot(new.routes().clone());
            }
        }

        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::sync::atomic::AtomicUsize;

    use super::*;
    use unbounded_storage::p2p::NodeId;

    fn graph_with_self_peer(self_peer_id: PeerId) -> config::RuntimeGraph {
        config::RuntimeGraph {
            disks: Vec::new(),
            caches: HashMap::new(),
            mesh: config::RuntimeMesh {
                fingers_per_node: 100,
                self_name: None,
                self_node_id: NodeId(self_peer_id.0),
                self_peer_id,
                self_tags: Vec::new(),
                routing_plan: None,
                topology_weighting: None,
                peers: Vec::new(),
            },
            frontends: HashMap::new(),
        }
    }

    #[test]
    fn local_self_peer_uses_projection_identity() {
        let graph = graph_with_self_peer(PeerId(7));

        assert_eq!(local_self_peer(&graph).unwrap(), PeerId(7));
    }

    #[test]
    fn collector_timeout_reports_sorted_outstanding_workers() {
        let (tx, rx) = mpsc::channel();
        tx.send(WorkerIdx(1)).unwrap();

        let collected = collect_worker_reports(
            "test phase",
            &rx,
            &[WorkerIdx(2), WorkerIdx(0), WorkerIdx(1)],
            Duration::from_millis(1),
            |worker| *worker,
        );

        assert_eq!(collected.reports, vec![WorkerIdx(1)]);
        assert_eq!(
            collected.errors,
            vec!["test phase timed out; outstanding workers=[0, 2]"]
        );
    }

    #[test]
    fn collector_rejects_duplicate_worker_report() {
        let (tx, rx) = mpsc::channel();
        tx.send(WorkerIdx(0)).unwrap();
        tx.send(WorkerIdx(0)).unwrap();
        tx.send(WorkerIdx(1)).unwrap();

        let collected = collect_worker_reports(
            "test phase",
            &rx,
            &[WorkerIdx(0), WorkerIdx(1)],
            Duration::from_secs(1),
            |worker| *worker,
        );

        assert_eq!(collected.reports, vec![WorkerIdx(0), WorkerIdx(1)]);
        assert_eq!(
            collected.errors,
            vec!["test phase received duplicate report from worker=0"]
        );
    }

    #[test]
    fn cleanup_watchdog_disarms_without_firing() {
        let fired = Arc::new(AtomicUsize::new(0));
        let terminated = Arc::new(AtomicUsize::new(0));
        let fired_for_watchdog = fired.clone();
        let terminated_for_watchdog = terminated.clone();
        let watchdog = CleanupWatchdog::spawn(
            Duration::from_secs(1),
            move || {
                fired_for_watchdog.fetch_add(1, Ordering::Relaxed);
            },
            move || {
                terminated_for_watchdog.fetch_add(1, Ordering::Relaxed);
            },
        );

        watchdog.disarm();

        assert_eq!(fired.load(Ordering::Relaxed), 0);
        assert_eq!(terminated.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn cleanup_watchdog_fires_and_terminates() {
        let (fired_tx, fired_rx) = mpsc::channel();
        let terminated = Arc::new(AtomicUsize::new(0));
        let terminated_for_watchdog = terminated.clone();
        let watchdog = CleanupWatchdog::spawn(
            Duration::from_millis(1),
            move || {
                fired_tx.send(()).unwrap();
            },
            move || {
                terminated_for_watchdog.fetch_add(1, Ordering::Relaxed);
            },
        );

        fired_rx.recv_timeout(Duration::from_secs(1)).unwrap();
        watchdog.disarm();

        assert_eq!(terminated.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn disk_reconcile_failure_is_fail_stop() {
        let shutdown = AtomicBool::new(false);
        let result: Result<(), ApplyError> = fail_stop_disk_reconcile(
            Err(ApplyError::Target(
                "later disk failed after destructive open".into(),
            )),
            &shutdown,
        );

        assert!(result.is_err());
        assert!(shutdown.load(Ordering::Acquire));
    }

    #[test]
    fn shard_backing_sizes_split_evenly() {
        assert_eq!(
            shard_backing_sizes(4 * HUGEPAGE_2MB, 2).unwrap(),
            vec![2 * HUGEPAGE_2MB, 2 * HUGEPAGE_2MB]
        );
    }

    #[test]
    fn shard_backing_sizes_assign_remainder_in_worker_order() {
        assert_eq!(
            shard_backing_sizes(5 * HUGEPAGE_2MB, 2).unwrap(),
            vec![3 * HUGEPAGE_2MB, 2 * HUGEPAGE_2MB]
        );
    }

    #[test]
    fn shard_backing_sizes_ignore_partial_trailing_page() {
        let total = 5 * HUGEPAGE_2MB + HUGEPAGE_2MB - 1;
        let sizes = shard_backing_sizes(total, 2).unwrap();

        assert_eq!(sizes, vec![3 * HUGEPAGE_2MB, 2 * HUGEPAGE_2MB]);
        assert!(sizes.iter().sum::<usize>() <= total);
    }

    #[test]
    fn shard_backing_sizes_accept_exact_minimum() {
        assert_eq!(
            shard_backing_sizes(3 * HUGEPAGE_2MB, 3).unwrap(),
            vec![HUGEPAGE_2MB; 3]
        );
    }

    #[test]
    fn shard_backing_sizes_reject_insufficient_or_zero_budget() {
        assert!(shard_backing_sizes(2 * HUGEPAGE_2MB, 3).is_err());
        assert!(shard_backing_sizes(0, 1).is_err());
    }

    #[test]
    fn shard_backing_sizes_allow_zero_shards() {
        assert_eq!(shard_backing_sizes(0, 0).unwrap(), Vec::<usize>::new());
    }
}
