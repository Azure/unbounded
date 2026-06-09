// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::RefCell;
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use clap::Parser;

use unbounded_storage::backend::{BackendRegistry, FixedRegion, OriginRing};
use unbounded_storage::bufferpool::{Pool, PoolConfig, ShardDescriptor, StripeKey};
use unbounded_storage::config::{self, BackendSpec, Config, FrontendKind, FrontendSpec};
use unbounded_storage::fabric::PeerId;
use unbounded_storage::fabric::{self, Fabric, Provider};
use unbounded_storage::frontend::{HttpDriver, HttpFrontend, S3Driver, S3Frontend};
use unbounded_storage::p2p::{
    FingerTable, FingerTableConfig, NodeId, PeerEntry, RecursiveHandler, RoutedTransport,
    TopologyLabels, node_to_ring,
};
use unbounded_storage::ring::{NetHandle, NetworkRing};
use unbounded_storage::runtime::{PinnedRuntime, ShardLoop, Threading, WorkerIdx};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::storage::disks::{
    DiskChannelDirectory, DiskRegistry, LiveShardLocalStore, UringDiskTarget,
};
use unbounded_storage::topology::{Host, Plan, PlanConfig, Role, Worker};

use unbounded_storage::memory::{BackingKind, BackingRequest, allocate};

mod shard_layer;

const DEFAULT_CONFIG_PATH: &str = "/etc/unbounded-storage/config.toml";
const SHUTDOWN_POLL: Duration = Duration::from_millis(100);

/// Stripe granularity used to build an inert origin backend on shards
/// that have no configured backend. Such a backend is never exercised
/// (no frontend drives reads against the pool), so the value is
/// immaterial; it only has to make the `ShardPool` type check.
const DEFAULT_STRIPE_SIZE_BYTES: u64 = 4 * 1024 * 1024;

/// Process-wide shutdown flag. Set by the signal handler (which
/// is restricted to async-signal-safe operations) and polled by
/// the main thread plus every shard thread.
static SHUTDOWN: AtomicBool = AtomicBool::new(false);

type ShardPool =
    Pool<RoutedTransport<StripeReq, BackendRegistry>, Arc<LiveShardLocalStore>, StripeReq>;

/// Startup-fixed settings sourced from the config file's `[startup]`
/// section, distinct from the dynamically reloadable parts of the
/// [`Config`]. These take effect only at process start: they size the
/// pinned runtime, the per-shard fabric endpoint, and the backing
/// allocation, so changing any of them requires a restart. The config
/// version whose startup settings are realized is tracked separately as
/// the startup config version.
pub struct StartupSettings {
    pub fabric: FabricStartup,
    pub bytes_per_shard: usize,
    pub backing_kind: BackingKind,
    pub plan_config: PlanConfig,
}

/// Startup-fixed fabric endpoint and thread-pool knobs, including the
/// per-shard `max_inflight` back-pressure limit. These are sized once at
/// process start from the config file's `[startup.fabric]` section and
/// are not part of the dynamic reload path.
#[derive(Clone, Debug)]
pub struct FabricStartup {
    pub listen_addr: String,
    pub progress_threads: u32,
    pub progress_poll_us: u32,
    pub rpc_worker_threads: u32,
    pub max_inflight: u32,
}

/// Build the host-`Plan` configuration from the config file's
/// `[startup.topology]` knobs. Fields with no corresponding knob retain
/// their `PlanConfig` defaults.
/// Decide whether to force the tcp provider fallback because the
/// discovered RDMA hardware cannot back a working libfabric `verbs`
/// provider. Returns true only when RDMA is not already disabled, at
/// least one HCA was discovered, and the verbs provider is unavailable.
/// Pure so it can be unit-tested without touching libfabric or sysfs.
fn should_force_tcp_fallback(disable_rdma: bool, hca_count: usize, verbs_available: bool) -> bool {
    !disable_rdma && hca_count > 0 && !verbs_available
}

fn startup_to_plan_config(topology: &config::TopologyCfg) -> PlanConfig {
    let defaults = PlanConfig::default();
    PlanConfig {
        rdma_progress_per_hca: topology.rdma_progress_per_hca as usize,
        rdma_handlers_per_hca: topology.rdma_handlers_per_hca as usize,
        nvme_threads_per_drive: defaults.nvme_threads_per_drive,
        network_shards_per_nic: defaults.network_shards_per_nic,
        use_smt_siblings: topology.use_smt_siblings,
        respect_isolated: !topology.ignore_isolated,
        exclude_node_cpu0: !topology.include_node_cpu0,
        require_node_type_ca: defaults.require_node_type_ca,
        require_active_port: !topology.allow_inactive_port,
        tcp_fallback_threads: topology.tcp_fallback_threads as usize,
        disable_rdma: topology.disable_rdma,
    }
}

fn main() -> ExitCode {
    let cli = Cli::parse();

    fabric::apply_tcp_env_defaults();

    let (config_path, config_explicit) = match cli.config.as_ref() {
        Some(p) => (p.clone(), true),
        None => (PathBuf::from(DEFAULT_CONFIG_PATH), false),
    };

    let config = match load_config(&config_path, config_explicit) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("config error: {e}");
            return ExitCode::FAILURE;
        }
    };

    // Startup-fixed settings come from the config file's `[startup]`
    // section, not the dynamically reloaded sections. They size the
    // pinned runtime, the per-shard fabric endpoint, and the backing
    // allocation, and only take effect at process start. `load_config`
    // has already run `apply_defaults`, so every startup field is
    // populated with its documented default when omitted.
    let startup = config.startup();
    let memory = startup.memory();
    let fabric_cfg = startup.fabric();
    let backing_kind = if memory.no_hugepages {
        BackingKind::Heap
    } else {
        BackingKind::Hugepage2Mb
    };
    let bytes_per_shard = memory.bytes_per_shard as usize;

    let host = Host::discover();

    // RDMA HCAs are discovered from sysfs, but a discovered HCA does not
    // guarantee a usable libfabric `verbs` provider: some cloud VMs
    // expose an `mlx5` device that backs an accelerated-networking
    // datapath with no working user-space verbs stack. Binding a shard
    // to `verbs` there fails the very first `fi_getinfo` with
    // `-FI_ENODATA` and crash-loops the daemon. When that shape is
    // detected, force the tcp fallback (the same path as the
    // `disable_rdma` escape hatch) so the daemon comes up over the tcp
    // provider instead of failing every shard at bring-up.
    let mut plan_config = startup_to_plan_config(startup.topology());
    if should_force_tcp_fallback(
        plan_config.disable_rdma,
        host.hcas.len(),
        fabric::provider_available(Provider::Verbs),
    ) {
        eprintln!(
            "fabric: {} RDMA HCA(s) discovered but the libfabric verbs provider is \
             unavailable; forcing the tcp provider fallback",
            host.hcas.len(),
        );
        plan_config.disable_rdma = true;
    }

    let settings = Arc::new(StartupSettings {
        fabric: FabricStartup {
            listen_addr: fabric_cfg.listen_addr.clone(),
            progress_threads: fabric_cfg.progress_threads,
            progress_poll_us: fabric_cfg.progress_poll_us,
            rpc_worker_threads: fabric_cfg.rpc_worker_threads,
            max_inflight: fabric_cfg.max_inflight,
        },
        bytes_per_shard,
        backing_kind,
        plan_config,
    });

    let plan = Plan::for_host(&host, &settings.plan_config);

    let counts = RoleCounts::from_plan(&plan);
    eprintln!(
        "topology plan: workers={} progress={} handlers={} numa_pools={:?}",
        plan.workers.len(),
        counts.progress,
        counts.handlers,
        plan.numa_pools,
    );

    // RDMA progress workers are spawned here as shard threads. The
    // per-disk storage cores are now wired separately by the disk
    // supervisor below: each disk runs on its own pinned storage core
    // hosting the engine and ring, exposed to shards as a
    // `PageChannel`. RDMA handler placements are still only computed
    // and logged, not spawned as their own threads. The `NvmeIoUring`
    // and `NetworkShard` plan roles are vestigial and intentionally
    // left uncounted.
    // Per-shard `Worker` metadata, in plan order. `rdma_progress()`
    // filters `plan.workers` by `Role::RdmaProgress`, the SAME
    // predicate the `from_plan` closure below uses, so `WorkerIdx(i)`
    // (which addresses the i-th retained worker in the runtime) lines
    // up with `progress[i]`. Keep these two filters in sync.
    let progress: Vec<Worker> = plan.rdma_progress().cloned().collect();
    if progress.is_empty() {
        eprintln!("topology plan produced no RDMA progress workers; exiting");
        return ExitCode::FAILURE;
    }

    let runtime = PinnedRuntime::from_plan(&plan, |w| matches!(w.role, Role::RdmaProgress { .. }));
    install_signal_handler();

    // Hot-swap publication surface for shards. The disk supervisor
    // runs each disk on its own pinned storage core hosting the engine
    // and ring, and hands a `PageChannel` to that core back here for
    // publication. Shards observe the channel set through
    // `LiveShardLocalStore` and ship page ops over it. Created before
    // the shard loop so each shard receives a clone.
    let disk_channels: Arc<DiskChannelDirectory> = DiskChannelDirectory::new();

    // Precompute the per-shard worker list: each progress worker plus
    // the HCA device name it should bind, resolved here while `host` is
    // in scope. The shard layer owns this list for the lifetime of the
    // process and re-uses it verbatim on a coordinated rebuild.
    let workers: Vec<(Worker, Option<String>)> = progress
        .iter()
        .map(|worker| {
            let dev_name = match worker.role {
                Role::RdmaProgress { hca } if hca != usize::MAX => {
                    Some(host.hcas[hca].dev_name.clone())
                }
                _ => None,
            };
            (worker.clone(), dev_name)
        })
        .collect();

    let deps = shard_layer::ShardSpawnDeps {
        runtime: runtime.clone(),
        workers,
        settings: settings.clone(),
        disk_channels: disk_channels.clone(),
    };

    // Disk supervisor: reconcile `[[disks]]` entries onto pinned
    // storage cores. Each disk runs on its own storage core hosting the
    // engine and ring, and publishes a `PageChannel` that carries the
    // page data path cross-core from the shards. CPU pin hints come from
    // the topology plan's disjoint NVMe (`Role::NvmeIoUring`) slots: the
    // registry assigns each disk a NUMA-local slot that is disjoint from
    // the shard cores by construction. If the plan discovered no NVMe
    // devices the slot list is empty and disks run unpinned. The
    // registry is owned by the apply target so live `[[disks]]` changes
    // reconcile through the same funnel as the rest of the config.
    let disk_slots = plan.disk_cpu_slots();
    let mut disk_registry = DiskRegistry::new(UringDiskTarget::new(runtime.clone()), disk_slots);

    // Bring up the initial shard layer. A bring-up failure is fatal:
    // there is no running process to reconcile into.
    let layer = match shard_layer::spawn_shard_layer(&config, &deps) {
        Ok(layer) => layer,
        Err(errs) => {
            for e in &errs {
                eprintln!("shard bring-up failed: {e}");
            }
            return ExitCode::FAILURE;
        }
    };

    // Reconcile the startup disk set now that the shards are up, then
    // publish the channel set so shards can reach their disks.
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
    for (path, hint) in disk_registry.placement_snapshot() {
        match hint {
            Some(slot) => eprintln!("disk {}: pinned to cpu {}", path.display(), slot.cpu),
            None => eprintln!("disk {}: unpinned (no plan slot available)", path.display()),
        }
    }
    disk_channels.apply_channels(disk_registry.channels_snapshot());

    // The config controller is the single funnel for live changes. It
    // owns the running shard layer and disk supervisor (via the apply
    // target) and blocks each apply until the process has converged onto
    // the new config.
    let target = shard_layer::ProcessApplyTarget::new(layer, disk_registry, disk_channels.clone());
    // Seeds the latest-known, latest-applied, and startup config
    // versions from the startup config's top-level `version`.
    // `controller.config_versions()` hands out a cloneable handle to all
    // three values, ready to be published as gauge metrics once a metrics
    // exporter exists. The known/applied versions advance as configs are
    // loaded and applied; the startup version stays pinned to the config
    // realized here at process start, since the `[startup]` knobs it
    // tracks only take effect on restart.
    let mut controller = config::ConfigController::new(target, Arc::new(config));

    // Watch the config file and drive each update through the
    // controller until shutdown. Every apply is in place; an apply error
    // is logged and the process keeps serving on the last-good config.
    let exit_code = ExitCode::SUCCESS;
    match config::ConfigWatcher::new(config_path.clone()) {
        Ok((_watcher, update_rx)) => {
            while !SHUTDOWN.load(Ordering::Acquire) {
                match update_rx.recv_timeout(SHUTDOWN_POLL) {
                    Ok(update) => {
                        let version = update.config.version;
                        match controller.apply(update.config.clone()) {
                            Ok(outcome) => eprintln!(
                                "config: applied gen={} version={} tier={:?}",
                                update.generation, version, outcome.tier
                            ),
                            Err(e) => {
                                eprintln!(
                                    "config: apply gen={} version={} failed: {e}",
                                    update.generation, version
                                );
                            }
                        }
                    }
                    Err(mpsc::RecvTimeoutError::Timeout) => continue,
                    Err(mpsc::RecvTimeoutError::Disconnected) => {
                        if shutdown_on_watcher_error(mpsc::RecvTimeoutError::Disconnected) {
                            SHUTDOWN.store(true, Ordering::Relaxed);
                        }
                        break;
                    }
                }
            }
        }
        Err(e) => {
            eprintln!("config watch: not installed: {e}");
            wait_for_shutdown();
        }
    }
    eprintln!("shutdown signaled; tearing down shards");

    // Teardown order: shard threads exit first so they release any
    // `Arc<StorageEngine>` refs published via the channel directory,
    // then drop the published snapshot, then drain the disk supervisor
    // so each per-disk thread sees its engine refcount fall before its
    // stop flag.
    let (layer, disk_registry) = controller.into_target().into_parts();
    if let Some(layer) = layer {
        shard_layer::teardown_shard_layer(layer);
    }
    drop(disk_channels);
    disk_registry.drain();

    exit_code
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
    fabric_startup: Arc<FabricStartup>,
    max_inflight: u64,
    disk_channels: Arc<DiskChannelDirectory>,
    fingers: Arc<FingerTable>,
    node_to_peer: Arc<HashMap<NodeId, PeerId>>,
    frontend_specs: Arc<Vec<FrontendSpec>>,
    backend_specs: Arc<Vec<BackendSpec>>,
    ctrl_rx: mpsc::Receiver<config::ShardCommand>,
    layer_stop: Arc<AtomicBool>,
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
    cfg.listen_addr = Some(fabric_startup.listen_addr.clone());
    cfg.max_inflight = max_inflight as usize;
    cfg.rpc_worker_threads = fabric_startup.rpc_worker_threads as usize;
    cfg.progress_threads = fabric_startup.progress_threads as u8;
    cfg.progress_poll_us = fabric_startup.progress_poll_us;
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

    // NUMA-local backing. Allocated on the pinned shard thread so the
    // `PinnedRuntime`'s `set_mempolicy` keeps the pages on the intended
    // node; the hugepage variant additionally pins via `mbind` when
    // `worker.numa` is known. Register it with the fabric before
    // building the transport.
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

    // Per-shard io_uring socket ring. Created on the pinned shard
    // thread (the ring is !Send) and given the same backing as a single
    // fixed buffer so SEND_ZC / RECV can target bufferpool pages. Must
    // register while `backing` is still owned, before `Pool::new` moves
    // it.
    let socket = match NetworkRing::new(256) {
        Ok(s) => Rc::new(RefCell::new(s)),
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: NetworkRing::new: {e}",
                widx.0,
            )));
            return;
        }
    };
    if let Err(e) = socket.borrow().register_backing(&backing) {
        let _ = tx.send(ShardReady::Failed(format!(
            "worker={}: socket register_backing: {e}",
            widx.0,
        )));
        return;
    }

    // Capture the backing's base pointer before `Pool::new` moves the
    // `Backing` value. The pointer addresses the same registered region
    // the socket ring and the pool share; the `HttpBackend` memcpys
    // origin bytes directly into pages carved from this base.
    let backing_base = backing.base;

    // Stripe geometry per backend id, shared (behind an `Rc`) with the
    // frontend registry and the control-drain hook so a live backend
    // add/replace keeps every frontend's stripe size in sync. A frontend
    // references a backend by id; load-time validation guarantees the
    // referent exists, but the registry falls back to the default stripe
    // size if a lookup races an as-yet-unapplied backend add.
    let geometry: Rc<RefCell<HashMap<String, u64>>> = Rc::new(RefCell::new(
        backend_specs
            .iter()
            .map(|b| (b.id.clone(), b.stripe_size_bytes))
            .collect(),
    ));

    // Build the shard's origin-backend registry: every configured
    // backend, keyed by id, behind an `ArcSwap` so a live config apply
    // can add/remove/replace a tier without tearing down the shard.
    // Each request names its backend through the `OriginRef` the
    // frontend stamps, so the registry resolves the tier per fetch.
    //
    // Two independent registries are needed (the pool transport and the
    // RPC handler each own one) because they differ in which ring a
    // cache-miss fetch drives (`OriginRing`) and which registered region
    // origin bytes are copied into (`backing_base`). The pool transport
    // runs on the shard thread and reuses the shard ring over the pool
    // backing; the RPC handler serves on an ephemeral worker thread and
    // must use a worker-local ring writing into the scratch backing.
    log_backend_registry(widx, &backend_specs);

    // Route the pool's transport through the p2p finger table: a stripe
    // owned by a peer goes over the fabric RDMA path; a stripe this node
    // owns (Chord `next_hop` returns None) goes to the registry, which
    // fetches the missing byte range from the named backend's origin.
    // The pool transport runs on the shard thread, so it reuses the
    // shard socket ring and writes origin bytes into the pool backing.
    let transport_registry = match BackendRegistry::new(
        &backend_specs,
        OriginRing::Shard(socket.clone()),
        page_size,
        backing_base,
    ) {
        Ok(r) => r,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: build backend registry: {e}",
                widx.0,
            )));
            return;
        }
    };
    // Shared, live-reloadable routing surface. Built once from the
    // startup config and handed (by cheap clone) to every p2p consumer
    // on this shard: the pool transport, the recursive RPC handler, and
    // the control-drain tick hook below. A `ShardCommand::ApplyConfig`
    // republishes through this single handle so all consumers observe
    // the new finger table atomically without a restart.
    let routing = unbounded_storage::p2p::RoutingHandle::new(fingers, node_to_peer);
    let transport = match RoutedTransport::with_routing(
        fabric.clone(),
        mr,
        page_size,
        routing.clone(),
        transport_registry.clone(),
    ) {
        Ok(t) => t,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: RoutedTransport::new: {e}",
                widx.0,
            )));
            return;
        }
    };
    // Per-shard view over the live disk channel directory. When the
    // directory is empty (no engines yet), `register_pages` records the
    // backing silently and reads/writes return `Error::Transport("no
    // disks open")`. Once the disk supervisor publishes engines, the
    // view's `current_or_replay` catches the swap and replays buffer
    // registration before delegating.
    let blockstore = Arc::new(LiveShardLocalStore::new(disk_channels.clone()));
    let pool: ShardPool = match Pool::new(
        PoolConfig::default(),
        backing,
        transport,
        blockstore.clone(),
    ) {
        Ok(p) => p,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: Pool::new: {e}",
                widx.0,
            )));
            return;
        }
    };
    // Share the pool via `Rc` so the frontend driver can clone it onto
    // its per-connection serve futures; it also fixes the teardown
    // order (see the drop sequence below).
    let pool = Rc::new(pool);

    // Make cancelled fixed-buffer RECVs into pool pages sound without
    // blocking the dropping task: a cancelled RECV's destination page is
    // withheld from the free list until the kernel finishes with it (its
    // RECV CQE is reaped). Only the shard socket ring receives into pool
    // pages, so the quarantine is installed on it alone; the RPC worker
    // rings receive into their own scratch backing and keep the blocking
    // drain fallback. Installed before serving begins so every cancelled
    // RECV is covered.
    unbounded_storage::backend::install_recv_quarantine(
        &socket.borrow(),
        pool.recv_quarantine_handle(),
    );

    // Bring up the per-shard RPC server so remote peers can fetch
    // stripes this node has resident locally (a peer "cache hit") and
    // so this node can act as an intermediate Chord hop, forwarding to
    // the next hop and relaying the downstream page back upstream. The
    // production `RecursiveHandler` serves and forwards out of a
    // dedicated scratch backing that only the RPC worker threads ever
    // touch, sidestepping the `Send + Sync` requirement on `Handler`
    // without sharing the `!Send` `Rc<Pool>` or its free list off the
    // shard thread. The scratch backing doubles as a fabric MR (the
    // `fi_write` source for serving and the `fi_write` destination for
    // forwarded pages) and a `BlockStore` extra buffer. Sized at a
    // small, fixed page count: one in-flight request consumes one
    // scratch page for the duration of its serve/forward. See
    // `p2p/handler.rs` for the soundness argument.
    const RPC_SCRATCH_PAGES: u32 = 8;
    let scratch = match allocate(BackingRequest {
        kind: backing_kind,
        bytes: page_size * RPC_SCRATCH_PAGES as usize,
        numa: worker.numa,
    }) {
        Ok(b) => b,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: rpc scratch backing allocation failed: {e}",
                widx.0,
            )));
            return;
        }
    };
    let scratch_mr = match fabric.register_backing(&scratch, worker.numa) {
        Ok(mr) => mr,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: rpc scratch register_backing: {e}",
                widx.0,
            )));
            return;
        }
    };
    // The RPC handler resolves disk reads into scratch pages, so it
    // needs a `BlockStore` whose single registered backing IS the
    // scratch region: `LiveShardLocalStore::resolve` maps a `PageRef`
    // through exactly one backing's geometry and refuses to guess among
    // several. The data-path `blockstore` already owns the (much
    // larger) pool backing, so it cannot also host scratch. We give the
    // handler its own store over the *same* disk channel directory:
    // scratch is still registered as an io_uring fixed buffer against
    // every disk channel (via the shared directory's replay), but page
    // resolution stays unambiguous.
    let rpc_store = Arc::new(LiveShardLocalStore::new(disk_channels));
    if let Err(e) = rpc_store.register_backing(&scratch) {
        let _ = tx.send(ShardReady::Failed(format!(
            "worker={}: rpc scratch blockstore register: {e}",
            widx.0,
        )));
        return;
    }
    // `MrHandle` is a `Copy` value handle (the underlying `fid_mr` is
    // owned by the `Fabric`), so a single scratch registration is
    // shared by copy between the handler's forwarding `FabricTransport`
    // (whose downstream `fi_write`s land in scratch) and the RPC
    // server's `local_mr` (the `fi_write` source that ships served
    // pages back to the requester). One backing, one MR, no double
    // allocation.
    // Capture the scratch backing's base before `scratch` is moved into
    // the handler below. The RPC handler serves peer cache-misses into
    // scratch pages (see `serve_owned` in `p2p/handler.rs`), so its
    // origin backend must memcpy origin bytes into the scratch region,
    // not the pool backing the data-path transport uses.
    let scratch_base = scratch.base;
    // The handler runs on one of the persistent `fabric-rpc-worker`
    // pool threads, so it must NOT touch the shard ring (the shard
    // thread progresses it concurrently; a cross-thread `RefCell`
    // borrow panics). A worker-local ring keeps every origin op on the
    // serving thread; it is lazily initialized once per long-lived pool
    // thread and reused across the many RPCs that thread serves.
    let handler_registry = match BackendRegistry::new(
        &backend_specs,
        OriginRing::WorkerLocal {
            queue_depth: 256,
            region: Some(FixedRegion {
                base: scratch_base,
                len: page_size * RPC_SCRATCH_PAGES as usize,
            }),
        },
        page_size,
        scratch_base,
    ) {
        Ok(r) => r,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: build rpc backend registry: {e}",
                widx.0,
            )));
            return;
        }
    };
    let rpc_handler = Arc::new(
        match RecursiveHandler::with_routing(
            rpc_store.clone(),
            scratch,
            RPC_SCRATCH_PAGES,
            routing.clone(),
            fabric.clone(),
            scratch_mr,
            page_size,
            handler_registry.clone(),
        ) {
            Ok(h) => h,
            Err(e) => {
                let _ = tx.send(ShardReady::Failed(format!(
                    "worker={}: RecursiveHandler::new: {e}",
                    widx.0,
                )));
                return;
            }
        },
    );
    let rpc_server =
        match fabric.start_rpc_server::<StripeReq, _>(rpc_handler, Some(scratch_mr), page_size) {
            Ok(h) => h,
            Err(e) => {
                let _ = tx.send(ShardReady::Failed(format!(
                    "worker={}: start_rpc_server: {e}",
                    widx.0,
                )));
                return;
            }
        };

    // Build the shard's cooperative loop and register its progress
    // sources. The socket ring's `progress()` is a tick hook; the
    // control-drain and the frontend registry are registered as further
    // tick hooks below.
    let mut shard_loop = ShardLoop::new();
    {
        let socket = socket.clone();
        // `progress()` takes `&self`, so this must be a *shared* borrow:
        // the origin backend holds `socket.borrow()` across its recv/send
        // awaits (see `backend::http`), so a `borrow_mut()` here would hit
        // a `BorrowMutError` panic whenever a cache-miss fetch is in
        // flight across a shard tick. Shared borrows coexist.
        shard_loop.add_tick_hook(move || socket.borrow().progress());
    }

    // Shard-local registry of running frontend drivers, seeded with every
    // configured frontend. A single permanent tick hook (registered
    // below) drives whichever drivers are live, and the control-drain
    // hook adds/removes drivers on a config apply; the `ShardLoop` has no
    // hook-removal API, so the registry (not the hook set) is the unit of
    // liveness. Each frontend stamps its backend id into every request,
    // so per-frontend origin routing falls out of the backend registry.
    let frontend_ctx = FrontendBuildCtx {
        pool: pool.clone(),
        handle: NetHandle::new(socket.clone()),
        geometry: geometry.clone(),
        page_size,
    };
    let frontend_registry = match FrontendRegistry::new(&frontend_specs, frontend_ctx) {
        Ok(r) => r,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!("worker={}: {e}", widx.0)));
            return;
        }
    };

    // Control-drain tick hook: applies live config changes on this
    // shard's own thread so all `!Send` per-shard state stays
    // thread-local. Each `ShardCommand::ApplyConfig` republishes the
    // routing surface through the shared `RoutingHandle` (observed
    // atomically by the transport and the recursive handler), refreshes
    // the stripe geometry, reconciles the origin-backend registries and
    // the frontend registry toward the new config, and then acknowledges
    // so the coordinator's blocking apply can complete. Everything is
    // driven from this one thread so the `ArcSwap` publishes are ordered
    // and the build-from-spec (DNS resolve, listener bind) stays off the
    // fast path.
    {
        let routing = routing.clone();
        let transport_registry = transport_registry.clone();
        let handler_registry = handler_registry.clone();
        let frontend_registry = frontend_registry.clone();
        let geometry = geometry.clone();
        let mut last_backends: HashMap<String, BackendSpec> = backend_specs
            .iter()
            .map(|b| (b.id.clone(), b.clone()))
            .collect();
        let mut last_frontends: HashMap<String, FrontendSpec> = frontend_specs
            .iter()
            .map(|f| (f.id.clone(), f.clone()))
            .collect();
        shard_loop.add_tick_hook(move || {
            let mut did_work = false;
            while let Ok(cmd) = ctrl_rx.try_recv() {
                match cmd {
                    config::ShardCommand::ApplyConfig(apply) => {
                        routing.store(apply.routing.fingers, apply.routing.node_to_peer);

                        let desired_backends = apply.config.backends.as_slice();
                        let desired_frontends = apply.config.frontends.as_slice();

                        // Refresh stripe geometry before building any
                        // frontend so a co-applied backend stripe change
                        // is visible to a frontend add in the same pass.
                        {
                            let mut g = geometry.borrow_mut();
                            g.clear();
                            for b in desired_backends {
                                g.insert(b.id.clone(), b.stripe_size_bytes);
                            }
                        }

                        // The RPC handler's backend registry is a second
                        // copy of the same desired set; reconcile it in
                        // lockstep with identical inputs so it converges to
                        // the same applied map.
                        let handler_report = config::reconcile::reconcile_backends(
                            &handler_registry,
                            desired_backends,
                            Some(&last_backends),
                        );

                        // Drive the transport backend registry and the
                        // frontend registry together so frontend adds are
                        // gated on their referenced backend being present.
                        let combined = config::reconcile::reconcile_backends_and_frontends(
                            &transport_registry,
                            &frontend_registry,
                            desired_backends,
                            desired_frontends,
                            Some(&last_backends),
                            Some(&last_frontends),
                        );
                        last_backends = combined.backends.applied;
                        last_frontends = combined.frontends.applied;

                        let mut failures = handler_report.failures;
                        failures.extend(combined.backends.failures);
                        failures.extend(combined.frontends.failures);
                        let result = if failures.is_empty() {
                            Ok(())
                        } else {
                            Err(format!("config reconcile failed: {failures:?}"))
                        };

                        let _ = apply.ack.send(config::ShardAck {
                            worker: widx,
                            result,
                        });
                        did_work = true;
                    }
                }
            }
            did_work
        });
    }

    // Frontend progress tick hook: drives every live frontend driver once
    // per shard tick. Runs on the shard thread, exclusive with the
    // control-drain hook above, so its `borrow_mut` of the driver map
    // never overlaps a reconcile that mutates the same map.
    {
        let frontend_registry = frontend_registry.clone();
        shard_loop.add_tick_hook(move || frontend_registry.progress());
    }

    let _ = tx.send(ShardReady::Up {
        descriptor: ShardDescriptor {
            worker_idx: widx,
            numa: worker.numa,
        },
        fabric: fabric.clone(),
    });

    // Drive the shard's cooperative future set until shutdown. The loop
    // busy-polls socket I/O and frontend work while active and idles
    // cheaply (100us, matching the disk thread cadence) when quiet.
    // Fabric and Pool self-drive progress on their own threads.
    shard_loop.run_until_with(
        || SHUTDOWN.load(Ordering::Acquire) || layer_stop.load(Ordering::Acquire),
        Duration::from_micros(100),
    );

    // Drop order matters:
    //   1. `shard_loop` first - clears tick hooks and futures, releasing
    //      their `Rc<socket>` clones and (if registered) the `HttpDriver`
    //      that holds the pool and a `NetHandle`.
    //   2. `pool` - tears down `Pool` and its `FabricTransport`, which
    //      holds an `Arc<Fabric>` clone that must go before `fabric`.
    //      The pool's `HttpBackend` also holds an `Rc<socket>` clone.
    //   3. `socket` - the last shard-local `Rc<socket>` clone.
    //   4. `rpc_server` - signals shutdown to the persistent RPC worker
    //      pool, closes its job queue, and joins every worker thread;
    //      this must complete while `fabric` (which they use) is still
    //      alive.
    //   5. `fabric` last - joins its progress threads, closes the
    //      scratch MR the RPC server used, and tears libfabric down.
    drop(shard_loop);
    drop(pool);
    drop(socket);
    drop(rpc_server);
    drop(fabric);
}

/// Run a shard thread body `f`, guaranteeing exactly one
/// [`ShardReady`] is observed on `tx` even when `f` panics during
/// bring-up.
///
/// `f` (i.e. [`run_shard`]) reports its own readiness on the normal
/// success and error paths through a sender it owns. The `tx` handed
/// here is a *separate* clone reserved solely for the panic path: if
/// `f` unwinds before reporting, its own sender is dropped by the
/// unwind, but the surviving shards stay parked holding their sender
/// clones, so `main`'s bounded `recv()` would block forever. Emitting a
/// `Failed` here keeps the readiness count whole. A panic *after* `f`
/// already reported `Up` produces a harmless extra `Failed`: `main`
/// reads exactly N messages and ignores any surplus.
fn report_on_panic<F>(tx: mpsc::Sender<ShardReady>, widx: WorkerIdx, f: F)
where
    F: FnOnce(),
{
    // `AssertUnwindSafe` is sound here: on the unwind path we touch only
    // `tx` and `widx` (both owned, no shared mutable state), and the
    // process is headed for the coherent failure path regardless.
    if let Err(payload) = std::panic::catch_unwind(std::panic::AssertUnwindSafe(f)) {
        eprintln!(
            "shard worker={} panicked during bring-up: {}",
            widx.0,
            panic_payload_str(&payload),
        );
        let _ = tx.send(ShardReady::Failed(format!(
            "worker={}: panicked during bring-up",
            widx.0,
        )));
    }
}

/// Best-effort extraction of a human-readable message from a panic
/// payload returned by [`std::panic::catch_unwind`]. Panics raised by
/// `panic!`/`.expect(...)`/`unwrap()` carry either a `&str` or a
/// `String`; anything else is reported generically.
fn panic_payload_str(payload: &(dyn std::any::Any + Send)) -> &str {
    if let Some(s) = payload.downcast_ref::<&str>() {
        s
    } else if let Some(s) = payload.downcast_ref::<String>() {
        s.as_str()
    } else {
        "non-string panic payload"
    }
}

/// Build the shared p2p routing surface from the loaded config: the
/// local node's finger table over the configured peers, plus the
/// `node -> peer` map the `FingerRouter` uses to resolve a finger's
/// `NodeId` to the fabric `PeerId`.
///
/// When `[p2p.routing_plan]` is present the table is built from that
/// precomputed neighbor set verbatim (disjoint discovery); otherwise
/// it is derived from the full `peers` list via `FingerTable::build`.
/// Either way `node_to_peer` is keyed off `peers`, which carry the
/// fabric connection info, so a node only connects to the peers it is
/// configured with.
///
/// `local_node_id` falls back to `0` when unset. Load-time
/// validation rejects an unset id when peers are configured, so the
/// fallback only fires for a single-node (no-peer) deployment, where
/// the table routes every stripe to self/backend regardless of the
/// id. `PeerSpec.id` doubles as both the `NodeId` and the `PeerId`,
/// matching `config::reconcile_peers`, which adds connections keyed
/// by `PeerId(spec.id)`.
fn build_routing(config: &Config) -> (Arc<FingerTable>, Arc<HashMap<NodeId, PeerId>>) {
    let local_id = config.p2p().local_node_id.unwrap_or(0);
    let local = PeerEntry {
        node: NodeId(local_id),
        ring: node_to_ring(NodeId(local_id)),
        labels: TopologyLabels(config.p2p().local_labels.clone()),
    };

    let node_to_peer: HashMap<NodeId, PeerId> = config
        .peers
        .iter()
        .map(|spec| (NodeId(spec.id), PeerId(spec.id)))
        .collect();

    if let Some(plan) = &config.p2p().routing_plan {
        // Disjoint discovery: a global-view planner already selected
        // this node's neighbors, so build the table from them verbatim
        // instead of deriving it from the full peer set. Ring positions
        // are derived from ids; labels (which do not affect runtime
        // routing) are carried over from the matching peer when present.
        let labels_of = |id: u64| -> TopologyLabels {
            config
                .peers
                .iter()
                .find(|p| p.id == id)
                .map(|p| TopologyLabels(p.labels.clone()))
                .unwrap_or_default()
        };
        let entry_of = |id: u64| PeerEntry {
            node: NodeId(id),
            ring: node_to_ring(NodeId(id)),
            labels: labels_of(id),
        };
        let fingers = plan.fingers.iter().map(|id| entry_of(*id)).collect();
        let successor = plan.successor.map(entry_of);
        let predecessor = plan.predecessor.map(entry_of);
        let table = FingerTable::from_explicit(local, fingers, successor, predecessor);
        return (Arc::new(table), Arc::new(node_to_peer));
    }

    let peers: Vec<PeerEntry> = config
        .peers
        .iter()
        .map(|spec| PeerEntry {
            node: NodeId(spec.id),
            ring: node_to_ring(NodeId(spec.id)),
            labels: TopologyLabels(spec.labels.clone()),
        })
        .collect();

    let cfg = FingerTableConfig {
        k: config.p2p().fingers_per_node.max(1),
    };
    let fingers = FingerTable::build(local, &peers, cfg);
    (Arc::new(fingers), Arc::new(node_to_peer))
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

/// Validate and log the configured backends.
///
/// [`OriginBackend`] is now the active origin tier: it is built per
/// shard from the matching [`BackendSpec`] (an [`HttpBackend`] or an
/// [`S3Backend`], selected by `spec.kind`) and plugged into the
/// `RoutedTransport`'s backend slot (see `run_shard`). This function
/// surfaces the configured backend set for observability and returns
/// the number of backends seen so the caller (and tests) can assert the
/// registry was walked.
fn log_backend_registry(widx: WorkerIdx, specs: &[BackendSpec]) -> usize {
    for spec in specs {
        eprintln!(
            "shard {}: backend {} kind={:?} endpoint={} stripe_size={} http_concurrency={} (OriginBackend active)",
            widx.0,
            spec.id,
            spec.kind,
            spec.endpoint,
            spec.stripe_size_bytes,
            spec.http_concurrency,
        );
    }
    specs.len()
}

/// Per-shard frontend driver, selected by [`FrontendKind`]. Both
/// variants are registered as a shard-loop tick hook through
/// [`Self::progress`]; the enum lets a single boxed closure drive
/// whichever concrete driver the frontend kind produced.
enum ShardFrontendDriver {
    Http(HttpDriver<ShardPool>),
    S3(S3Driver<ShardPool>),
}

impl ShardFrontendDriver {
    fn progress(&mut self) -> bool {
        match self {
            ShardFrontendDriver::Http(d) => d.progress(),
            ShardFrontendDriver::S3(d) => d.progress(),
        }
    }
}

/// Shard-local build context for frontend drivers: everything a
/// [`FrontendSpec`] needs to become a running [`ShardFrontendDriver`]
/// except the spec itself. Cheap to clone (all fields are handles), so
/// the [`FrontendRegistry`] holds one and rebuilds drivers on demand
/// when a live config apply adds a frontend.
#[derive(Clone)]
struct FrontendBuildCtx {
    pool: Rc<ShardPool>,
    handle: NetHandle,
    /// Backend id -> stripe size, shared (and live-updated) with the
    /// control-drain hook so a frontend brought up after a backend
    /// stripe change sees the new geometry. A frontend's stripe size is
    /// that of the backend it serves.
    geometry: Rc<RefCell<HashMap<String, u64>>>,
    page_size: usize,
}

/// Resolve the stripe size a frontend should serve from the shard's
/// live backend geometry. A frontend inherits the stripe size of the
/// backend it references; if that backend is not yet present in the
/// geometry (for example a frontend whose backend add is still
/// deferred), fall back to [`DEFAULT_STRIPE_SIZE_BYTES`] so the driver
/// can still be built and will pick up the real value when the geometry
/// is refreshed on a later apply.
fn frontend_stripe_size(geometry: &HashMap<String, u64>, backend_id: &str) -> u64 {
    geometry
        .get(backend_id)
        .copied()
        .unwrap_or(DEFAULT_STRIPE_SIZE_BYTES)
}

impl FrontendBuildCtx {
    /// Turn one [`FrontendSpec`] into a bound, ready-to-drive
    /// [`ShardFrontendDriver`]. Validates the spec, binds the shard's
    /// `SO_REUSEPORT` listener, and selects the stripe size from the
    /// referenced backend's geometry (falling back to the default if the
    /// backend is not yet known). Returns a human-readable error string
    /// so it slots into the reconcile traits' `Result<_, String>`.
    fn build(&self, spec: &FrontendSpec) -> Result<ShardFrontendDriver, String> {
        let stripe_size = frontend_stripe_size(&self.geometry.borrow(), &spec.backend);
        match spec.kind() {
            FrontendKind::Http => {
                let frontend = HttpFrontend::from_spec(spec)
                    .map_err(|e| format!("frontend {} from_spec: {e}", spec.id))?;
                let listen_fd = frontend
                    .bind_listener()
                    .map_err(|e| format!("frontend {} bind_listener: {e}", spec.id))?;
                Ok(ShardFrontendDriver::Http(HttpDriver::new(
                    self.pool.clone(),
                    self.handle.clone(),
                    listen_fd,
                    spec.backend.clone(),
                    stripe_size,
                    self.page_size,
                )))
            }
            FrontendKind::S3 => {
                let frontend = S3Frontend::from_spec(spec)
                    .map_err(|e| format!("frontend {} from_spec: {e}", spec.id))?;
                let listen_fd = frontend
                    .bind_listener()
                    .map_err(|e| format!("frontend {} bind_listener: {e}", spec.id))?;
                Ok(ShardFrontendDriver::S3(S3Driver::new(
                    self.pool.clone(),
                    self.handle.clone(),
                    listen_fd,
                    spec.backend.clone(),
                    stripe_size,
                    self.page_size,
                )))
            }
        }
    }
}

/// Shard-local registry of running frontend drivers, keyed by frontend
/// id. A single permanent tick hook drives whichever drivers are live,
/// and the control-drain hook adds/removes drivers on a config apply;
/// the [`ShardLoop`] has no hook-removal API, so the registry (not the
/// hook set) is the unit of liveness. Each frontend stamps its backend
/// id into every request, so per-frontend origin routing falls out of
/// the backend registry resolving that id.
#[derive(Clone)]
struct FrontendRegistry {
    drivers: Rc<RefCell<HashMap<String, ShardFrontendDriver>>>,
    ctx: FrontendBuildCtx,
}

impl FrontendRegistry {
    /// Seed a registry with every configured frontend. Fails the shard
    /// (returns `Err`) on the first spec that will not build, preserving
    /// the startup contract that a bad frontend aborts bring-up rather
    /// than silently serving a subset.
    fn new(specs: &[FrontendSpec], ctx: FrontendBuildCtx) -> Result<Self, String> {
        let registry = Self {
            drivers: Rc::new(RefCell::new(HashMap::with_capacity(specs.len()))),
            ctx,
        };
        for spec in specs {
            let driver = registry.ctx.build(spec)?;
            registry
                .drivers
                .borrow_mut()
                .insert(spec.id.clone(), driver);
        }
        Ok(registry)
    }

    /// Drive every registered frontend once, returning whether any
    /// driver did work. Runs on the shard thread, exclusive with the
    /// control-drain hook, so this `borrow_mut` never overlaps a
    /// reconcile that mutates the same map.
    fn progress(&self) -> bool {
        let mut busy = false;
        for driver in self.drivers.borrow_mut().values_mut() {
            busy |= driver.progress();
        }
        busy
    }
}

impl config::reconcile::FrontendReconcileTarget for FrontendRegistry {
    fn list(&self) -> Vec<String> {
        self.drivers.borrow().keys().cloned().collect()
    }

    fn add(&self, spec: &FrontendSpec) -> Result<(), String> {
        let driver = self.ctx.build(spec)?;
        self.drivers.borrow_mut().insert(spec.id.clone(), driver);
        Ok(())
    }

    fn remove(&self, id: &str) -> Result<(), String> {
        // Dropping the driver runs its `Drop`, which closes the listen
        // fd, so a removed frontend stops accepting immediately.
        self.drivers.borrow_mut().remove(id);
        Ok(())
    }
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
                Role::NetworkShard { .. } => {}
            }
        }
        c
    }
}

/// Parsed command-line options for one run of the daemon.
///
/// The daemon takes all of its configuration - both the dynamically
/// reloadable cluster state (`[p2p]`, peers, disks, backends, frontends)
/// and the startup-fixed knobs (`[startup]`: memory sizing, fabric
/// endpoint/thread pools, CPU-topology selection) - from the config
/// file. The only command-line option is the path to that file, since
/// the daemon cannot read the file until it knows where it is.
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
    /// Path to the config file.
    ///
    /// A `.binpb` extension is decoded as a raw binary protobuf wire
    /// message; any other extension is parsed as TOML.
    ///
    /// If left at the default and the file is missing, the daemon
    /// continues with built-in defaults. An explicit path that is
    /// missing or invalid is fatal.
    #[arg(long, value_name = "PATH")]
    config: Option<PathBuf>,
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
            // Mirror the `load` finalization: a raw `Config::default()`
            // leaves every section `None`, so the section accessors
            // (`startup()`, `p2p()`, ...) would panic. Promote the proto3
            // zero values to documented defaults exactly as the on-disk
            // path does.
            let mut c = Config::default();
            c.apply_defaults();
            Ok(c)
        }
        Err(e) => Err(format!("loading {}: {e}", path.display())),
    }
}

/// Block the calling thread until the process-wide [`SHUTDOWN`] latch is
/// set, polling on [`SHUTDOWN_POLL`]. Used to park the main thread while
/// the shard threads run.
fn wait_for_shutdown() {
    while !SHUTDOWN.load(Ordering::Acquire) {
        thread::sleep(SHUTDOWN_POLL);
    }
}

/// Decide whether a `recv_timeout` error on the config-update channel
/// should drive shutdown. A `Disconnected` watcher is treated as a
/// shutdown request so the shard joins in `main` can complete; a
/// `Timeout` is an ordinary idle tick. Pure so the disconnect
/// semantics can be unit-tested without touching the process-global
/// `SHUTDOWN` latch.
fn shutdown_on_watcher_error(err: mpsc::RecvTimeoutError) -> bool {
    matches!(err, mpsc::RecvTimeoutError::Disconnected)
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

    fn default_topology() -> config::TopologyCfg {
        let mut cfg = Config::default();
        cfg.apply_defaults();
        cfg.startup().topology().clone()
    }

    #[test]
    fn startup_to_plan_config_default_mapping() {
        // The defaults-populated `[startup.topology]` must map to the
        // historical PlanConfig defaults the old CLI flag defaults
        // produced.
        let pc = startup_to_plan_config(&default_topology());
        let d = PlanConfig::default();
        assert_eq!(pc.rdma_progress_per_hca, 1);
        assert_eq!(pc.rdma_handlers_per_hca, 4);
        assert_eq!(pc.tcp_fallback_threads, 1);
        // Default-off topology flags map to the historical defaults.
        assert!(!pc.use_smt_siblings);
        assert!(pc.respect_isolated);
        assert!(pc.exclude_node_cpu0);
        assert!(pc.require_active_port);
        assert!(!pc.disable_rdma);
        // Fields with no knob retain PlanConfig defaults.
        assert_eq!(pc.nvme_threads_per_drive, d.nvme_threads_per_drive);
        assert_eq!(pc.network_shards_per_nic, d.network_shards_per_nic);
        assert_eq!(pc.require_node_type_ca, d.require_node_type_ca);
    }

    #[test]
    fn startup_to_plan_config_inverts_negative_flags() {
        // The inverted-sense topology knobs (ignore_isolated,
        // include_node_cpu0, allow_inactive_port) must flip the
        // corresponding PlanConfig fields, and the numeric counts must
        // pass through.
        let topology = config::TopologyCfg {
            rdma_progress_per_hca: 3,
            rdma_handlers_per_hca: 7,
            tcp_fallback_threads: 2,
            use_smt_siblings: true,
            ignore_isolated: true,
            include_node_cpu0: true,
            allow_inactive_port: true,
            disable_rdma: true,
        };
        let pc = startup_to_plan_config(&topology);
        assert_eq!(pc.rdma_progress_per_hca, 3);
        assert_eq!(pc.rdma_handlers_per_hca, 7);
        assert_eq!(pc.tcp_fallback_threads, 2);
        assert!(pc.use_smt_siblings);
        assert!(!pc.respect_isolated);
        assert!(!pc.exclude_node_cpu0);
        assert!(!pc.require_active_port);
        assert!(pc.disable_rdma);
    }

    #[test]
    fn force_tcp_fallback_when_hca_present_but_verbs_unavailable() {
        // The crash-loop case: sysfs surfaced an HCA but libfabric has
        // no usable verbs provider. Force the fallback.
        assert!(should_force_tcp_fallback(false, 1, false));
        assert!(should_force_tcp_fallback(false, 4, false));
    }

    #[test]
    fn no_force_tcp_fallback_when_verbs_available() {
        // Real RDMA hardware with a working verbs provider: keep RDMA.
        assert!(!should_force_tcp_fallback(false, 2, true));
    }

    #[test]
    fn no_force_tcp_fallback_without_hcas() {
        // No HCA discovered: planning already takes the tcp_fallback
        // path, so there is nothing to override regardless of the probe.
        assert!(!should_force_tcp_fallback(false, 0, false));
        assert!(!should_force_tcp_fallback(false, 0, true));
    }

    #[test]
    fn no_force_tcp_fallback_when_already_disabled() {
        // Operator already set disable_rdma: do not log a redundant
        // override.
        assert!(!should_force_tcp_fallback(true, 1, false));
        assert!(!should_force_tcp_fallback(true, 3, true));
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
    fn removed_startup_flags_are_rejected() {
        // The startup knobs moved into the config file; their old CLI
        // flags must no longer be accepted so stale invocations fail
        // loudly instead of silently ignoring the value.
        for flag in [
            "--no-hugepages",
            "--bytes-per-shard=64M",
            "--fabric-listen-addr=10.0.0.1:7777",
            "--disable-rdma",
            "--use-smt-siblings",
        ] {
            let err = parse(&[flag]).unwrap_err();
            assert_eq!(
                err.kind(),
                ErrorKind::UnknownArgument,
                "flag {flag} should be rejected",
            );
        }
    }

    #[test]
    fn load_config_missing_default_path_applies_defaults() {
        // A missing default (non-explicit) config path falls back to the
        // built-in defaults. That fallback must run `apply_defaults` so
        // the section accessors are populated; a raw `Config::default()`
        // leaves `startup`/`p2p` as `None` and panics the daemon at
        // startup when it reads `cfg.startup()`.
        let path = Path::new("/definitely/not/a/real/path/unbounded-storage.toml");
        let cfg = load_config(path, false).expect("missing default path falls back to defaults");
        // These accessors panic if defaults were not applied.
        assert_eq!(cfg.p2p().fingers_per_node, 100);
        assert_eq!(cfg.startup().fabric().listen_addr, "0.0.0.0:0");
        assert_eq!(cfg.startup().memory().bytes_per_shard, 128 * 1024 * 1024);
    }

    #[test]
    fn load_config_missing_explicit_path_is_fatal() {
        // An explicit path that is missing stays fatal: only the default
        // path is allowed to silently fall back to built-in defaults.
        let path = Path::new("/definitely/not/a/real/path/unbounded-storage.toml");
        assert!(load_config(path, true).is_err());
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

    fn backend_spec(id: &str) -> BackendSpec {
        BackendSpec {
            id: id.to_string(),
            kind: config::BackendKind::Http as i32,
            endpoint: "https://example.com".to_string(),
            stripe_size_bytes: 4 * 1024 * 1024,
            http_concurrency: 64,
            bucket: None,
        }
    }

    #[test]
    fn frontend_stripe_size_uses_backend_geometry() {
        // A frontend inherits the stripe size of the backend it
        // references when that backend is present in the geometry.
        let mut geometry = HashMap::new();
        geometry.insert("b".to_string(), 8 * 1024 * 1024);
        assert_eq!(frontend_stripe_size(&geometry, "b"), 8 * 1024 * 1024);
    }

    #[test]
    fn frontend_stripe_size_falls_back_when_backend_absent() {
        // A frontend whose backend is not yet known (e.g. a deferred
        // backend add) builds against the default stripe size and picks
        // up the real value when the geometry is refreshed.
        let geometry = HashMap::new();
        assert_eq!(
            frontend_stripe_size(&geometry, "missing"),
            DEFAULT_STRIPE_SIZE_BYTES,
        );
    }

    #[test]
    fn log_backend_registry_counts_specs() {
        assert_eq!(log_backend_registry(WorkerIdx(0), &[]), 0);
        let specs = vec![backend_spec("primary"), backend_spec("secondary")];
        assert_eq!(log_backend_registry(WorkerIdx(1), &specs), 2);
    }

    #[test]
    fn report_on_panic_emits_failed_when_body_panics() {
        // A shard body that panics before reporting must still leave
        // exactly one `Failed` on the channel so `main`'s bounded recv
        // count stays whole and startup cannot deadlock.
        let (tx, rx) = mpsc::channel::<ShardReady>();
        report_on_panic(tx, WorkerIdx(7), || {
            panic!("boom during bring-up");
        });
        match rx.recv() {
            Ok(ShardReady::Failed(msg)) => {
                assert!(msg.contains("worker=7"), "got: {msg}");
                assert!(msg.contains("panicked during bring-up"), "got: {msg}");
            }
            other => panic!("expected Failed, got {:?}", other.is_ok()),
        }
        // No second message: the panic path reports exactly once.
        assert!(rx.try_recv().is_err());
    }

    #[test]
    fn report_on_panic_is_silent_when_body_succeeds() {
        // On the normal path `run_shard` owns the reporting; the
        // panic-path clone must stay quiet so no spurious `Failed`
        // is appended.
        let (tx, rx) = mpsc::channel::<ShardReady>();
        report_on_panic(tx, WorkerIdx(0), || {});
        assert!(rx.try_recv().is_err());
    }

    #[test]
    fn panic_payload_str_handles_str_and_string() {
        let s: Box<dyn std::any::Any + Send> = Box::new("static str");
        assert_eq!(panic_payload_str(&*s), "static str");
        let s: Box<dyn std::any::Any + Send> = Box::new(String::from("owned"));
        assert_eq!(panic_payload_str(&*s), "owned");
        let s: Box<dyn std::any::Any + Send> = Box::new(42u32);
        assert_eq!(panic_payload_str(&*s), "non-string panic payload");
    }

    #[test]
    fn watcher_disconnect_signals_shutdown_but_timeout_does_not() {
        // A disconnected config watcher must behave like a shutdown
        // request so the shard joins complete; a timeout is an idle
        // tick and must not.
        assert!(shutdown_on_watcher_error(
            mpsc::RecvTimeoutError::Disconnected
        ));
        assert!(!shutdown_on_watcher_error(mpsc::RecvTimeoutError::Timeout));
    }
}
