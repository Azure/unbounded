// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::RefCell;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::task::Waker;
use std::thread;
use std::time::Duration;

use clap::Parser;

use unbounded_storage::backend::BackendRegistry;
use unbounded_storage::bufferpool::{Pool, PoolConfig};
use unbounded_storage::config::{
    self, BackendSpec, Config, FrontendSpec, ResolvedFrontendBinding, frontend_spec,
};
use unbounded_storage::fabric::{self, Fabric, MrHandle, Provider};
use unbounded_storage::fanout::{
    FanoutPeer, FanoutTable, FetchChannel, FetchService, NumaShardTable,
};
use unbounded_storage::frontend::{
    HttpDriver, HttpFrontend, LoadgenDriver, LoadgenFrontend, S3Driver, S3Frontend,
};
use unbounded_storage::p2p::{
    FingerTable, FingerTableConfig, PeerEntry, RouteTableHandle, RouteTableSnapshot,
    RoutedTransport, TopologyPrefixWeight, TopologySelection, TopologyTags, TopologyWeighting,
    node_to_ring,
};
use unbounded_storage::ring::{NetHandle, NetworkRing};
use unbounded_storage::runtime::{PinnedRuntime, ShardLoop, WorkerIdx, WorkerSpec};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::storage::disks::{
    CacheDirectorySet, ChainLocalStore, DiskRegistry, UringDiskTarget,
};
use unbounded_storage::topology::{CorePlan, CorePlanConfig, DiskCpuSlot, Host, ServingShard};

use unbounded_storage::memory::{BackingKind, BackingRequest, allocate};
use unbounded_storage::metrics;

mod device_inventory;
mod fabric_group;
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

type ShardPool = Pool<RoutedTransport<StripeReq, BackendRegistry>, Arc<ChainLocalStore>, StripeReq>;

/// Startup-fixed settings sourced from the config file's `[startup]`
/// section, distinct from the dynamically reloadable parts of the
/// [`Config`]. These take effect only at process start: they size the
/// pinned runtime, the per-shard fabric endpoint, and the backing
/// allocation, so changing any of them requires a restart. The config
/// version whose startup settings are realized is tracked separately as
/// the startup config version.
pub struct StartupSettings {
    pub fabric: FabricStartup,
    /// Total backing pool across all serving shards. The shard layer
    /// divides this by the serving-shard count so each shard gets a
    /// NUMA-local slice and the host footprint stays fixed regardless of
    /// the auto-scaled core count.
    pub memory_total_bytes: usize,
    pub backing_kind: BackingKind,
    pub core_plan_config: CorePlanConfig,
}

/// Startup-fixed fabric endpoint and thread-pool knobs, including the
/// per-shard `max_inflight` back-pressure limit. These are sized once at
/// process start from the config file's `[startup.fabric]` section and
/// are not part of the dynamic reload path.
#[derive(Clone, Debug)]
pub struct FabricStartup {
    pub listen_addr: String,
    pub rdma_listen_addrs: Vec<String>,
    pub progress_threads: u32,
    pub progress_poll_us: u32,
    pub rpc_worker_threads: u32,
    pub max_inflight: u32,
}

impl FabricStartup {
    pub fn listen_addr_for_unit(&self, unit_idx: usize) -> &str {
        self.rdma_listen_addrs
            .get(unit_idx)
            .map_or(self.listen_addr.as_str(), String::as_str)
    }
}

/// Build the [`CorePlanConfig`] from the config file's
/// `[startup.topology]` knobs. Fields with no corresponding knob retain
/// their `CorePlanConfig` defaults.
/// Decide whether to force the tcp provider fallback because the
/// discovered RDMA hardware cannot back a working libfabric `verbs`
/// provider. Returns true only when RDMA is not already disabled, at
/// least one HCA was discovered, and the verbs provider is unavailable.
/// Pure so it can be unit-tested without touching libfabric or sysfs.
fn should_force_tcp_fallback(disable_rdma: bool, hca_count: usize, verbs_available: bool) -> bool {
    !disable_rdma && hca_count > 0 && !verbs_available
}

fn startup_to_core_plan_config(
    topology: &config::TopologyCfg,
    fabric: &config::FabricCfg,
) -> CorePlanConfig {
    let defaults = CorePlanConfig::default();
    CorePlanConfig {
        nic_workers: topology
            .nic_workers
            .map(|n| n as usize)
            .unwrap_or(defaults.nic_workers),
        serving_cores: topology.serving_cores.map(|n| n as usize),
        hcas_per_numa: fabric
            .auto_hcas_per_numa_node()
            .map(|n| n as usize)
            .unwrap_or(defaults.hcas_per_numa),
        use_smt_siblings: topology.use_smt_siblings,
        respect_isolated: !topology.ignore_isolated,
        exclude_node_cpu0: !topology.include_node_cpu0,
        require_node_type_ca: defaults.require_node_type_ca,
        require_active_port: !topology.allow_inactive_port,
        disable_rdma: topology.disable_rdma,
    }
}

/// Resolve the HCA device name each serving shard should bind for its
/// fabric endpoint. Every serving shard is assigned one active-HCA
/// device, preferring an HCA on the shard's own NUMA node and
/// round-robining within that node for spread. Shards on a node with no
/// local HCA fall back to a flat round-robin over all active HCAs. When
/// the plan kept no HCAs (RDMA disabled or none usable) every shard gets
/// `None`, i.e. the tcp loopback fallback. This decides only per-shard
/// device affinity; `plan_fabric_units` then groups shards that share a
/// device onto a single endpoint (one per HCA), and `FabricGroup` owns
/// the resulting fabrics. `hca_dev_names` is indexed by HCA index,
/// matching `NicWorkerGroup::hca`.
fn assign_shard_devices(plan: &CorePlan, hca_dev_names: &[String]) -> Vec<Option<String>> {
    let active: Vec<(Option<u16>, String)> = plan
        .nic_workers
        .iter()
        .map(|group| (group.numa, hca_dev_names[group.hca].clone()))
        .collect();

    let shard_count = plan.serving_shards.len();
    if active.is_empty() {
        return vec![None; shard_count];
    }

    let mut by_numa: BTreeMap<u16, Vec<String>> = BTreeMap::new();
    for (numa, dev) in &active {
        if let Some(node) = numa {
            by_numa.entry(*node).or_default().push(dev.clone());
        }
    }
    let flat: Vec<String> = active.iter().map(|(_, dev)| dev.clone()).collect();

    let mut numa_cursor: HashMap<u16, usize> = HashMap::new();
    let mut flat_cursor = 0usize;
    let mut out = Vec::with_capacity(shard_count);
    for shard in &plan.serving_shards {
        let local = shard
            .numa
            .and_then(|node| by_numa.get(&node).map(|devs| (node, devs)))
            .filter(|(_, devs)| !devs.is_empty());
        let dev = match local {
            Some((node, devs)) => {
                let cursor = numa_cursor.entry(node).or_insert(0);
                let dev = devs[*cursor % devs.len()].clone();
                *cursor += 1;
                dev
            }
            None => {
                let dev = flat[flat_cursor % flat.len()].clone();
                flat_cursor += 1;
                dev
            }
        };
        out.push(Some(dev));
    }
    out
}

fn main() -> ExitCode {
    let cli = Cli::parse();

    unbounded_storage::obs::init_from_env();

    metrics::init();

    #[cfg(feature = "profiling")]
    {
        use unbounded_storage::profiling;

        match profiling::ProfilingConfig::from_env() {
            Ok(cfg) => {
                profiling::install_signal_handler();
                profiling::spawn(cfg, || SHUTDOWN.load(Ordering::Acquire));
            }
            Err(e) => eprintln!("profiling: disabled: {e}"),
        }
    }

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
    // The metrics exporter addr is a startup-fixed knob; capture
    // it now while `startup` borrows `config`, before `config` is moved
    // into the controller below. An empty string disables the exporter.
    let metrics_bind = startup.metrics().addr.clone();
    let backing_kind = if memory.no_hugepages {
        BackingKind::Heap
    } else {
        BackingKind::Hugepage2Mb
    };
    let memory_total_bytes = memory
        .memory_total_bytes
        .expect("memory_total_bytes defaulted") as usize;

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
    let mut core_plan_config = startup_to_core_plan_config(startup.topology(), fabric_cfg);
    if should_force_tcp_fallback(
        core_plan_config.disable_rdma,
        host.hcas.len(),
        fabric::provider_available(Provider::Verbs),
    ) {
        eprintln!(
            "fabric: {} RDMA HCA(s) discovered but the libfabric verbs provider is \
             unavailable; forcing the tcp provider fallback",
            host.hcas.len(),
        );
        core_plan_config.disable_rdma = true;
    }

    let settings = Arc::new(StartupSettings {
        fabric: FabricStartup {
            listen_addr: fabric_cfg
                .default_listen_addr()
                .expect("fabric listen address defaulted")
                .to_string(),
            rdma_listen_addrs: fabric_cfg.rdma_listen_addrs().map(str::to_string).collect(),
            progress_threads: fabric_cfg
                .progress_threads
                .expect("progress_threads defaulted"),
            progress_poll_us: fabric_cfg
                .progress_poll_us
                .expect("progress_poll_us defaulted"),
            rpc_worker_threads: fabric_cfg
                .rpc_worker_threads
                .expect("rpc_worker_threads defaulted"),
            max_inflight: fabric_cfg.max_inflight.expect("max_inflight defaulted"),
        },
        memory_total_bytes,
        backing_kind,
        core_plan_config,
    });

    let core_plan = CorePlan::for_host(&host, &settings.core_plan_config);

    let nic_worker_cpus: usize = core_plan
        .nic_workers
        .iter()
        .map(|group| group.workers.len())
        .sum();
    eprintln!(
        "topology plan: serving_shards={} nic_workers={} storage_cores={} numa_pools={:?}",
        core_plan.serving_shards.len(),
        nic_worker_cpus,
        core_plan.storage_cores.len(),
        core_plan.numa_pools,
    );

    // Serving shards are the per-shard threads (HTTP frontend, checksum,
    // `Rc<Pool>` + io_uring socket ring, fanout). Their count is now
    // decoupled from the HCA count: it auto-fills the usable cores left
    // after the storage and NIC-worker reservations. The per-disk storage
    // cores are wired separately by the disk supervisor below. The fabric
    // endpoints the shards register against are built once by the shard
    // layer from the plan computed below, not per serving shard.
    if core_plan.serving_shards.is_empty() {
        eprintln!("topology plan produced no serving shards; exiting");
        return ExitCode::FAILURE;
    }

    // `WorkerIdx(i)` addresses the i-th pinned runtime worker. The first
    // `serving_shards.len()` workers are the serving shards (index-aligned
    // with `core_plan.serving_shards`); the NIC-worker groups follow,
    // flattened in group order, so a verbs fabric unit can pin its
    // progress and RPC threads to a dedicated NIC-worker core. The planner
    // in `fabric_group` relies on exactly this layout to resolve a
    // device's worker index (`serving_count` + the group's flattened
    // base).
    let mut worker_specs: Vec<WorkerSpec> = core_plan
        .serving_shards
        .iter()
        .map(|shard| WorkerSpec::new(shard.cpu, shard.numa))
        .collect();
    for group in &core_plan.nic_workers {
        for worker in &group.workers {
            worker_specs.push(WorkerSpec::new(worker.cpu, worker.numa));
        }
    }
    let runtime = PinnedRuntime::new(worker_specs);
    install_signal_handler();

    // Hot-swap publication surface for cache disks. The disk supervisor
    // publishes one channel directory per configured cache; shard stores
    // select the directory from the request's cache id.
    let cache_directories = CacheDirectorySet::new();

    // Precompute the per-shard worker list: each serving shard plus the
    // HCA device name it should bind, resolved here while `host` is in
    // scope. The shard layer owns this list for the lifetime of the
    // process and re-uses it verbatim on a coordinated rebuild.
    let hca_dev_names: Vec<String> = host.hcas.iter().map(|h| h.dev_name.clone()).collect();
    let shard_devices = assign_shard_devices(&core_plan, &hca_dev_names);

    // Plan how serving shards map onto fabric endpoints. This is the
    // single tcp/verbs seam: tcp gets one loopback fabric per shard,
    // verbs gets one shared fabric per HCA (see `fabric_group`). Built
    // here, before `shard_devices` is consumed by the worker zip below,
    // and owned by the shard layer for the lifetime of the process so a
    // coordinated rebuild reuses the same plan.
    let fabric_plan = fabric_group::plan_fabric_units(
        &core_plan.serving_shards,
        &shard_devices,
        &core_plan.nic_workers,
        &hca_dev_names,
    );

    let workers: Vec<(ServingShard, Option<String>)> = core_plan
        .serving_shards
        .iter()
        .copied()
        .zip(shard_devices)
        .collect();

    let deps = shard_layer::ShardSpawnDeps {
        runtime: runtime.clone(),
        workers,
        settings: settings.clone(),
        cache_directories: cache_directories.clone(),
        fabric_plan,
    };

    // Disk supervisor: reconcile projected cache disks onto pinned
    // storage cores. Each disk runs on its own storage core hosting the
    // engine and ring, and publishes a `PageChannel` that carries the
    // page data path cross-core from the shards. CPU pin hints come from
    // the topology plan's per-drive storage cores: each one already
    // inherits the drive's NUMA node and is disjoint from the serving
    // and NIC-worker cores by construction. If the host discovered no
    // NVMe devices the slot list is empty and disks run unpinned. The
    // registry is owned by the apply target so live cache disk changes
    // reconcile through the same funnel as the rest of the config.
    let disk_slots: Vec<DiskCpuSlot> = core_plan
        .storage_cores
        .iter()
        .map(|core| DiskCpuSlot {
            cpu: core.cpu,
            numa: core.numa,
        })
        .collect();
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
    let device_inventory = metrics::DeviceInventoryStatus::new();
    device_inventory.set_rdma(device_inventory::rdma_annotation(
        &host,
        &layer.fabric_unit_addresses(),
    ));
    device_inventory.set_block(device_inventory::block_annotation(&host));

    // Reconcile the startup disk set now that the shards are up, then
    // publish the channel set so shards can reach their disks.
    let projection =
        config::runtime_projection(&config).expect("loaded config projects to runtime");
    reconcile_cache_disks(&mut disk_registry, &cache_directories, &projection);

    // The config controller is the single funnel for live changes. It
    // owns the running shard layer and disk supervisor (via the apply
    // target) and blocks each apply until the process has converged onto
    // the new config.
    let target = shard_layer::ProcessApplyTarget::new(layer, disk_registry, cache_directories);
    // Seeds the latest-known, latest-applied, and startup config
    // versions from the startup config's top-level `version`.
    // `controller.config_versions()` hands out a cloneable handle to all
    // three values, ready to be published as gauge metrics once a metrics
    // exporter exists. The known/applied versions advance as configs are
    // loaded and applied; the startup version stays pinned to the config
    // realized here at process start, since the `[startup]` knobs it
    // tracks only take effect on restart.
    let mut controller = config::ConfigController::new(target, Arc::new(config));

    // Start the Prometheus exporter once the controller exists, so it can
    // publish the live config-version gauges. A bind failure is logged
    // but non-fatal: the daemon serves data without metrics rather than
    // refusing to start. The thread parks off the pinned shard cores and
    // exits when `SHUTDOWN` is set.
    let metrics_exporter = if metrics_bind.is_empty() {
        None
    } else {
        match metrics::spawn(
            &metrics_bind,
            controller.config_versions(),
            device_inventory,
            &SHUTDOWN,
        ) {
            Ok(handle) => {
                eprintln!("metrics: exporter listening on {metrics_bind}");
                Some(handle)
            }
            Err(e) => {
                eprintln!("metrics: exporter disabled: {e}");
                None
            }
        }
    };

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
    disk_registry.drain();

    // The exporter polls `SHUTDOWN` on its accept loop; join it last so a
    // late scrape can still observe the final config versions during
    // teardown. Bounded by the accept poll interval.
    if let Some(handle) = metrics_exporter {
        let _ = handle.join();
    }

    exit_code
}

/// Body of one shard thread. Runs on the pinned executor: brings up
/// the `Fabric`, registers a NUMA-local `Backing`, wires up the
/// `Pool`, reports readiness, then idles until shutdown so the
/// `Fabric` (and its progress threads) plus the `Pool` are dropped
/// together.
fn run_shard(
    widx: WorkerIdx,
    shard: ServingShard,
    fabric: Arc<Fabric>,
    tx: mpsc::Sender<ShardReady>,
    backing_kind: BackingKind,
    bytes_per_shard: usize,
    cache_directories: Arc<CacheDirectorySet>,
    routes: RouteTableHandle,
    frontend_specs: Arc<Vec<FrontendSpec>>,
    frontend_bindings: Arc<HashMap<String, ResolvedFrontendBinding>>,
    backend_specs: Arc<Vec<BackendSpec>>,
    ctrl_rx: mpsc::Receiver<config::ShardCommand>,
    peer_rx: mpsc::Receiver<Arc<Vec<PeerPublish>>>,
    phaseb_tx: mpsc::Sender<PhaseBReport>,
    serve_start_rx: mpsc::Receiver<()>,
    layer_stop: Arc<AtomicBool>,
) {
    // The fabric endpoint is built and owned by the `FabricGroup` in the
    // shard layer and shared by every shard mapped onto it (one per shard
    // for the tcp fallback, one per HCA for verbs). This shard registers
    // its data backing and client transport against it; it neither
    // creates nor tears it down.

    // NUMA-local backing. Allocated on the pinned shard thread so the
    // `PinnedRuntime`'s `set_mempolicy` keeps the pages on the intended
    // node; the hugepage variant additionally pins via `mbind` when
    // `shard.numa` is known. Register it with the fabric before
    // building the transport.
    let backing = match allocate(BackingRequest {
        kind: backing_kind,
        bytes: bytes_per_shard,
        numa: shard.numa,
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
    let mr = match fabric.register_backing(&backing, shard.numa) {
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
        Ok(s) => Rc::new(s),
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: NetworkRing::new: {e}",
                widx.0,
            )));
            return;
        }
    };
    if let Err(e) = socket.register_backing(&backing) {
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

    // Total registered length of the backing, captured before
    // `Pool::new` moves it. Peers register `(backing_base, backing_len)`
    // on their own socket rings so they can `SEND_ZC` from this shard's
    // pinned pages.
    let backing_len = backing.page_size * backing.page_count;

    // Shared Drop carrier for this shard's backing allocation, captured
    // before `Pool::new` moves the `Backing`. Published to the layer so
    // it can keep the mapping alive until every shard thread has joined:
    // a peer coordinator's socket ring registers this region as a
    // `SEND_ZC` source, and that ring is only guaranteed torn down once
    // the peer's thread joins. Freeing the backing when this shard's
    // `Pool` drops (which happens inside this thread, before the layer
    // joins anyone) would leave a peer ring pointing at unmapped memory.
    let backing_keepalive = backing.keepalive();

    // Stripe geometry per backend name, shared (behind an `Rc`) with the
    // frontend registry and the control-drain hook so a live backend
    // add/replace keeps every frontend's stripe size in sync. A frontend
    // references a backend by name; load-time validation guarantees the
    // referent exists, but the registry falls back to the default stripe
    // size if a lookup races an as-yet-unapplied backend add.
    let geometry: Rc<RefCell<HashMap<String, u64>>> = Rc::new(RefCell::new(
        backend_specs
            .iter()
            .map(|b| (b.name.clone(), b.stripe_size_bytes()))
            .collect(),
    ));

    // Build the shard's origin-backend registry: every configured
    // backend, keyed by name, behind an `ArcSwap` so a live config apply
    // can add/remove/replace a tier without tearing down the shard.
    // Each request names its backend through the `OriginRef` the
    // frontend stamps, so the registry resolves the tier per fetch.
    //
    // The registry runs on the shard thread, reuses the shard ring, and
    // writes origin bytes into the pool backing. Recursive RPC owner
    // serves go through this same pool/fetch-service path rather than a
    // separate RPC-worker origin registry.
    log_backend_registry(widx, &backend_specs);

    // Route the pool's transport through the p2p finger table: a stripe
    // owned by a peer goes over the fabric RDMA path; a stripe this node
    // owns (Chord `next_hop` returns None) goes to the registry, which
    // fetches the missing byte range from the named backend's origin.
    // The pool transport runs on the shard thread, so it reuses the
    // shard socket ring and writes origin bytes into the pool backing.
    let transport_registry = match BackendRegistry::new(
        &backend_specs,
        NetHandle::new(socket.clone()),
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
    let transport = match RoutedTransport::with_routes(
        fabric.clone(),
        mr,
        page_size,
        routes.clone(),
        transport_registry.clone(),
    ) {
        Ok(t) => t,
        Err(e) => {
            let _ = tx.send(ShardReady::Failed(format!(
                "worker={}: RoutedTransport::with_routes: {e}",
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
    let blockstore = ChainLocalStore::new(cache_directories.clone());
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
    unbounded_storage::backend::install_recv_quarantine(&socket, pool.recv_quarantine_handle());

    // The RPC server that serves peer cache-hits and forwards Chord hops
    // is brought up and owned by the `FabricGroup` (shared across every
    // shard mapped onto this fabric), so it is no longer created here.

    // Build the shard's cooperative loop and register its progress
    // sources. The socket ring's `progress()` is a tick hook; the
    // control-drain and the frontend registry are registered as further
    // tick hooks below.
    let mut shard_loop = ShardLoop::new();
    {
        let socket = socket.clone();
        shard_loop.add_tick_hook(move || socket.progress());
    }

    // Cross-shard fan-out channel. This shard publishes the sender half
    // (so peer coordinators can request stripes it owns) and keeps the
    // receiver half to drive its own fetch service below.
    let (fetch_channel, fetch_rx) = FetchChannel::new();

    // PHASE A: announce readiness, publishing our backing region and
    // fetch channel, then block until the layer broadcasts the full peer
    // set. Bring-up is two-phase because registering a peer's backing
    // re-registers the socket ring's whole fixed-buffer table, which is
    // only safe while the ring is quiescent (no I/O in flight) - i.e.
    // before the serve loop starts. A broadcast `Err` means the layer
    // hit a bring-up failure on another shard and tore down before
    // publishing; exit cleanly by returning (the shard locals drop in
    // reverse declaration order; the shared `fabric` is released by the
    // `FabricGroup`, not here).
    let _ = tx.send(ShardReady::Up {
        worker_idx: widx,
        publish: ShardPublish {
            backing_base: backing_base as usize,
            backing_len,
            fabric_mr: mr,
            numa: shard.numa,
            fetch_channel,
            backing_keepalive,
        },
    });
    let peers = match peer_rx.recv() {
        Ok(peers) => peers,
        Err(_) => return,
    };

    // From here on this shard owes the layer exactly one PhaseB report.
    // The guard sends `Failed` on any early return or panic so the
    // layer's bounded Phase-B collection never hangs waiting on a shard
    // that aborted (mirroring `report_on_panic` for Phase A).
    let mut phaseb_guard = PhaseBGuard::new(widx, phaseb_tx);

    // PHASE B: register every peer shard's backing on our socket ring
    // (recording the fixed-buffer index each lands at) and assemble the
    // routing table. The ring is still quiescent here.
    let own_shard_index = peers
        .iter()
        .position(|p| p.worker_idx == widx)
        .expect("own shard present in broadcast peer set");
    let shard_count = peers.len();
    let mut routed: Vec<Option<FanoutPeer>> = (0..shard_count).map(|_| None).collect();
    for peer in peers.iter() {
        if peer.shard_index == own_shard_index {
            continue;
        }
        let buf_index =
            match socket.register_region_indexed(peer.backing_base as *mut u8, peer.backing_len) {
                Ok(idx) => idx,
                Err(e) => {
                    phaseb_guard.report_failed(format!(
                        "worker={}: register peer shard {} backing: {e}",
                        widx.0, peer.shard_index,
                    ));
                    return;
                }
            };
        routed[peer.shard_index] = Some(FanoutPeer {
            channel: peer.channel.clone(),
            buf_index,
        });
    }
    // NUMA -> serving-shard table for NUMA-local fetch routing: a stripe
    // is routed to a shard co-located with the drive that backs it,
    // spreading across that node's shards. Built from the full peer set
    // (each peer carries its shard index and NUMA node).
    let numa_shards = NumaShardTable::from_shards(peers.iter().map(|p| (p.shard_index, p.numa)));
    let fanout = Rc::new(FanoutTable::new(
        own_shard_index,
        routed,
        numa_shards,
        cache_directories.clone(),
    ));

    // Owner-side fetch service: serves stripe requests this shard owns by
    // pinning pages in its backing and replying with their byte offsets.
    // Registered as a tick hook so it shares the shard's cooperative loop.
    {
        let mut fetch_service =
            FetchService::new(pool.clone(), fetch_rx, page_size, shard_loop.waker());
        shard_loop.add_tick_hook(move || fetch_service.progress());
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
        fanout: fanout.clone(),
        geometry: geometry.clone(),
        routes: routes.clone(),
        bindings: Rc::new(RefCell::new((*frontend_bindings).clone())),
        page_size,
        worker_idx: widx.0,
        waker: shard_loop.waker(),
    };
    let frontend_registry = match FrontendRegistry::new(&frontend_specs, frontend_ctx) {
        Ok(r) => r,
        Err(e) => {
            phaseb_guard.report_failed(format!("worker={}: {e}", widx.0));
            return;
        }
    };

    // Control-drain tick hook: applies live config changes on this
    // shard's own thread so all `!Send` per-shard state stays
    // thread-local. Each `ShardCommand::ApplyConfig` republishes the
    // routing surface through this shard's `RouteTableHandle` (observed
    // atomically by its transport; the fabric RPC handlers are reloaded
    // separately by the `FabricGroup`), refreshes the stripe geometry,
    // reconciles the transport origin-backend registry and the frontend
    // registry toward the new config, applies disk-policy side effects, and then acknowledges
    // so the coordinator's blocking apply can complete. Everything is
    // driven from this one thread so the `ArcSwap` publishes are ordered
    // and the build-from-spec (DNS resolve, listener bind) stays off the
    // fast path.
    {
        let routes = routes.clone();
        let transport_registry = transport_registry.clone();
        let frontend_registry = frontend_registry.clone();
        let pool = pool.clone();
        let geometry = geometry.clone();
        let bindings = frontend_registry.ctx.bindings.clone();
        let mut last_backends: HashMap<String, BackendSpec> = backend_specs
            .iter()
            .map(|b| (b.name.clone(), b.clone()))
            .collect();
        let mut last_frontends: HashMap<String, FrontendSpec> = frontend_specs
            .iter()
            .map(|f| (f.name.clone(), f.clone()))
            .collect();
        let mut last_bindings: HashMap<String, ResolvedFrontendBinding> =
            (*frontend_bindings).clone();
        shard_loop.add_tick_hook(move || {
            let mut did_work = false;
            while let Ok(cmd) = ctrl_rx.try_recv() {
                match cmd {
                    config::ShardCommand::ApplyConfig(apply) => {
                        routes.store_snapshot(apply.routes.clone());

                        let projection = match config::runtime_projection(&apply.config) {
                            Ok(projection) => projection,
                            Err(e) => {
                                let _ = apply.ack.send(config::ShardAck {
                                    worker: widx,
                                    result: Err(format!("config projection failed: {e}")),
                                });
                                did_work = true;
                                continue;
                            }
                        };

                        let desired_backends = apply.config.backends.as_slice();
                        let desired_frontends = apply.config.frontends.as_slice();
                        let desired_frontend_backends =
                            config::frontend_backend_map(&projection.frontends);
                        // Refresh stripe geometry before building any
                        // frontend so a co-applied backend stripe change
                        // is visible to a frontend add in the same pass.
                        {
                            let mut g = geometry.borrow_mut();
                            g.clear();
                            for b in desired_backends {
                                g.insert(b.name.clone(), b.stripe_size_bytes());
                            }
                        }

                        let frontends_to_rebuild = frontend_rebuild_ids(
                            &last_bindings,
                            &projection.frontends,
                            &last_backends,
                            desired_backends,
                        );
                        if !frontends_to_rebuild.is_empty() {
                            for id in frontends_to_rebuild {
                                let _ = config::reconcile::FrontendReconcileTarget::remove(
                                    &frontend_registry,
                                    &id,
                                );
                                last_frontends.remove(&id);
                            }
                        }
                        *bindings.borrow_mut() = projection.frontends.clone();

                        // Drive the transport backend registry and the
                        // frontend registry together so frontend adds are
                        // gated on their referenced backend being present.
                        let combined = config::reconcile::reconcile_backends_and_frontends(
                            &transport_registry,
                            &frontend_registry,
                            desired_backends,
                            desired_frontends,
                            &desired_frontend_backends,
                            Some(&last_backends),
                            Some(&last_frontends),
                        );
                        last_backends = combined.backends.applied;
                        last_frontends = combined.frontends.applied;
                        last_bindings = projection.frontends;

                        let mut failures = combined.backends.failures;
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
                    config::ShardCommand::DrainPageCache(drain) => {
                        pool.drain_page_cache();
                        let _ = drain.ack.send(config::ShardAck {
                            worker: widx,
                            result: Ok(()),
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

    // PHASE B complete: peers registered, fetch service installed, and
    // frontend drivers built. Report readiness so the layer can start the
    // shared recursive RPC servers, then wait until those handlers are
    // installed before entering the shard loop. Without this barrier a
    // loadgen/frontend tick can issue a remote read during the small
    // window after phase-B reporting but before peer RPC handlers exist.
    phaseb_guard.report_ready();
    match serve_start_rx.recv() {
        Ok(()) => {}
        Err(_) => return,
    }
    metrics::shards_delta(1);

    // Drive the shard's cooperative future set until shutdown. The loop
    // busy-polls socket I/O and frontend work while active and idles
    // cheaply (100us, matching the disk thread cadence) when quiet.
    // Fabric and Pool self-drive progress on their own threads.
    shard_loop.run_until_with(
        || SHUTDOWN.load(Ordering::Acquire) || layer_stop.load(Ordering::Acquire),
        Duration::from_micros(100),
    );

    metrics::shards_delta(-1);

    // Drop order matters:
    //   1. `shard_loop` first - clears tick hooks and futures, releasing
    //      their `Rc<socket>` clones and (if registered) the `HttpDriver`
    //      that holds the pool and a `NetHandle`.
    //   2. `pool` - tears down `Pool` and its `FabricTransport`, which
    //      holds an `Arc<Fabric>` clone; releasing it here lets the
    //      `FabricGroup` close the shared endpoint during layer teardown
    //      once this thread has joined.
    //   3. `socket` - the last shard-local `Rc<socket>` clone.
    // The `fabric` parameter is a clone of a group-owned endpoint; it is
    // released when this function returns, before the layer drops the
    // `FabricGroup`.
    drop(shard_loop);
    drop(pool);
    drop(socket);
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

fn build_routes(config: &Config) -> RouteTableSnapshot {
    let projection = config::runtime_projection(config).expect("loaded config projects to runtime");
    if projection.caches.is_empty() {
        return RouteTableSnapshot {
            cache_ids: HashSet::new(),
            fingers: None,
        };
    }

    let mesh = &projection.mesh;
    let local = PeerEntry {
        node: mesh.self_node_id,
        ring: node_to_ring(mesh.self_node_id),
        tags: TopologyTags(mesh.self_tags.clone()),
    };
    let fingers = if let Some(plan) = &mesh.routing_plan {
        let peer_by_name: HashMap<&str, &config::RuntimePeer> = mesh
            .peers
            .iter()
            .map(|peer| (peer.name.as_str(), peer))
            .collect();
        let entry_of = |name: &str| {
            let peer = peer_by_name
                .get(name)
                .expect("validated routing_plan names reference peers");
            PeerEntry {
                node: peer.node_id,
                ring: node_to_ring(peer.node_id),
                tags: TopologyTags(peer.spec.tags.clone()),
            }
        };
        Arc::new(FingerTable::from_explicit(
            local,
            plan.fingers.iter().map(|name| entry_of(name)).collect(),
            plan.successor.as_deref().map(entry_of),
            plan.predecessor.as_deref().map(entry_of),
        ))
    } else {
        let peers: Vec<PeerEntry> = mesh
            .peers
            .iter()
            .map(|peer| PeerEntry {
                node: peer.node_id,
                ring: node_to_ring(peer.node_id),
                tags: TopologyTags(peer.spec.tags.clone()),
            })
            .collect();
        Arc::new(FingerTable::build(
            local,
            &peers,
            FingerTableConfig {
                k: mesh.fingers_per_node.max(1),
                topology: mesh
                    .topology_weighting
                    .as_ref()
                    .map(p2p_topology_weighting)
                    .map(TopologySelection::Weighted)
                    .unwrap_or_default(),
            },
        ))
    };
    RouteTableSnapshot {
        cache_ids: projection.caches.keys().cloned().collect(),
        fingers: Some(fingers),
    }
}

fn p2p_topology_weighting(weighting: &config::TopologyWeighting) -> TopologyWeighting {
    TopologyWeighting {
        prefix_weights: weighting
            .prefix_weights
            .iter()
            .map(|weight| TopologyPrefixWeight {
                tag_index: weight.tag_index,
                weight: weight.weight,
            })
            .collect(),
    }
}

fn reconcile_cache_disks(
    disk_registry: &mut DiskRegistry<UringDiskTarget>,
    cache_directories: &CacheDirectorySet,
    projection: &config::RuntimeGraph,
) {
    let mut cache_ids: Vec<String> = projection.caches.keys().cloned().collect();
    cache_ids.sort();
    cache_directories.reconcile(cache_ids.iter().cloned());

    let disks = config::runtime_disks(projection);
    let report = disk_registry.reconcile(&disks);
    eprintln!(
        "config: shared disks: added={} removed={} failures={}",
        report.added,
        report.removed,
        report.failures.len(),
    );
    for (path, msg) in &report.failures {
        eprintln!("disk {}: open failed: {msg}", path.display());
    }

    let channels = disk_registry.channels_snapshot();
    for cache_id in cache_ids {
        cache_directories.apply_channels(&cache_id, channels.clone());
    }
}

/// Validate and log the configured backends.
///
/// [`OriginBackend`] is now the active origin tier: it is built per
/// shard from the matching [`BackendSpec`] (an [`HttpBackend`] or an
/// [`S3Backend`], selected by `spec.config`) and plugged into the
/// `RoutedTransport`'s backend slot (see `run_shard`). This function
/// surfaces the configured backend set for observability and returns
/// the number of backends seen so the caller (and tests) can assert the
/// registry was walked.
fn log_backend_registry(widx: WorkerIdx, specs: &[BackendSpec]) -> usize {
    for spec in specs {
        eprintln!(
            "shard {}: backend {} kind={} url={} stripe_size={} http_concurrency={} (OriginBackend active)",
            widx.0,
            spec.name,
            spec.kind_name(),
            spec.url().unwrap_or(""),
            spec.stripe_size_bytes(),
            spec.http_concurrency().unwrap_or(0),
        );
    }
    specs.len()
}

/// Per-shard frontend driver, selected by [`FrontendSpec::config`]. All
/// variants are registered as a shard-loop tick hook through
/// [`Self::progress`]; the enum lets a single boxed closure drive
/// whichever concrete driver the frontend kind produced.
enum ShardFrontendDriver {
    Http(HttpDriver<ShardPool>),
    Loadgen(LoadgenDriver<ShardPool>),
    S3(S3Driver<ShardPool>),
}

impl ShardFrontendDriver {
    fn progress(&mut self) -> bool {
        match self {
            ShardFrontendDriver::Http(d) => d.progress(),
            ShardFrontendDriver::Loadgen(d) => d.progress(),
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
    /// Per-shard cross-shard routing surface. The HTTP serve path
    /// consults it to fan each stripe out to its owner shard (or serve
    /// locally). Shared behind an `Rc` so cloning the context stays
    /// cheap.
    fanout: Rc<FanoutTable>,
    /// Backend name -> stripe size, shared (and live-updated) with the
    /// control-drain hook so a frontend brought up after a backend
    /// stripe change sees the new geometry. A frontend's stripe size is
    /// that of the backend it serves.
    geometry: Rc<RefCell<HashMap<String, u64>>>,
    /// Per-cache peer routing table. Loadgen can optionally use it for
    /// remote-only key selection while issuing normal cache reads.
    routes: RouteTableHandle,
    bindings: Rc<RefCell<HashMap<String, ResolvedFrontendBinding>>>,
    page_size: usize,
    worker_idx: u16,
    waker: Waker,
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

fn frontend_rebuild_ids(
    last_bindings: &HashMap<String, ResolvedFrontendBinding>,
    next_bindings: &HashMap<String, ResolvedFrontendBinding>,
    last_backends: &HashMap<String, BackendSpec>,
    desired_backends: &[BackendSpec],
) -> Vec<String> {
    let changed_backend_geometry: HashSet<String> = desired_backends
        .iter()
        .filter_map(|backend| {
            let old = last_backends.get(&backend.name)?;
            (old.stripe_size_bytes() != backend.stripe_size_bytes()).then(|| backend.name.clone())
        })
        .collect();

    let mut rebuild: Vec<String> = next_bindings
        .iter()
        .filter_map(|(id, binding)| {
            (last_bindings.get(id) != Some(binding)
                || changed_backend_geometry.contains(&binding.backend_id))
            .then(|| id.clone())
        })
        .collect();
    rebuild.sort();
    rebuild
}

impl FrontendBuildCtx {
    /// Turn one [`FrontendSpec`] into a bound, ready-to-drive
    /// [`ShardFrontendDriver`]. Validates the spec, binds the shard's
    /// `SO_REUSEPORT` listener, and selects the stripe size from the
    /// referenced backend's geometry (falling back to the default if the
    /// backend is not yet known). Returns a human-readable error string
    /// so it slots into the reconcile traits' `Result<_, String>`.
    fn build(&self, spec: &FrontendSpec) -> Result<ShardFrontendDriver, String> {
        let binding = self
            .bindings
            .borrow()
            .get(&spec.name)
            .cloned()
            .ok_or_else(|| format!("frontend {} has no resolved binding", spec.name))?;
        let stripe_size = frontend_stripe_size(&self.geometry.borrow(), &binding.backend_id);
        match spec.config.as_ref() {
            Some(frontend_spec::Config::Http(_)) => {
                let frontend = HttpFrontend::from_spec(spec)
                    .map_err(|e| format!("frontend {} from_spec: {e}", spec.name))?;
                let listen_fd = frontend
                    .bind_listener()
                    .map_err(|e| format!("frontend {} bind_listener: {e}", spec.name))?;
                Ok(ShardFrontendDriver::Http(HttpDriver::new(
                    self.pool.clone(),
                    self.handle.clone(),
                    listen_fd,
                    Rc::from(spec.name.as_str()),
                    binding.backend_id.clone(),
                    binding.cache_id.clone(),
                    stripe_size,
                    self.page_size,
                    self.fanout.clone(),
                    binding.bypass_cache,
                    frontend.max_requests_per_connection(),
                )))
            }
            Some(frontend_spec::Config::S3(_)) => {
                let frontend = S3Frontend::from_spec(spec)
                    .map_err(|e| format!("frontend {} from_spec: {e}", spec.name))?;
                let listen_fd = frontend
                    .bind_listener()
                    .map_err(|e| format!("frontend {} bind_listener: {e}", spec.name))?;
                Ok(ShardFrontendDriver::S3(S3Driver::new(
                    self.pool.clone(),
                    self.handle.clone(),
                    listen_fd,
                    Rc::from(spec.name.as_str()),
                    binding.backend_id.clone(),
                    binding.cache_id.clone(),
                    stripe_size,
                    self.page_size,
                    binding.bypass_cache,
                )))
            }
            Some(frontend_spec::Config::Loadgen(_)) => {
                let frontend = LoadgenFrontend::from_spec(spec)
                    .map_err(|e| format!("frontend {} from_spec: {e}", spec.name))?;
                Ok(ShardFrontendDriver::Loadgen(LoadgenDriver::new(
                    frontend,
                    self.pool.clone(),
                    binding.backend_id.clone(),
                    binding.cache_id.clone(),
                    stripe_size,
                    self.page_size,
                    self.routes.clone(),
                    binding.bypass_cache,
                    self.worker_idx,
                    self.waker.clone(),
                )))
            }
            None => Err(format!("frontend {} missing config", spec.name)),
        }
    }
}

/// Shard-local registry of running frontend drivers, keyed by frontend
/// name. A single permanent tick hook drives whichever drivers are live,
/// and the control-drain hook adds/removes drivers on a config apply;
/// the [`ShardLoop`] has no hook-removal API, so the registry (not the
/// hook set) is the unit of liveness. Each frontend stamps its backend
/// backend name into every request, so per-frontend origin routing falls
/// out of the backend registry resolving that name.
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
                .insert(spec.name.clone(), driver);
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
        self.drivers.borrow_mut().insert(spec.name.clone(), driver);
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
        worker_idx: WorkerIdx,
        /// Cross-shard fan-out endpoints this shard exposes: its
        /// registered backing region and the channel to its fetch
        /// service. The layer collects these from every shard and
        /// broadcasts the full set back so each shard can register its
        /// peers' backings and build its [`FanoutTable`].
        publish: ShardPublish,
    },
    Failed(String),
}

/// Phase-A publication from one shard: the backing region other shards
/// must register to `SEND_ZC` from this shard's pinned pages, plus the
/// channel to this shard's [`FetchService`]. The base is shipped as a
/// `usize` because the raw `*mut u8` is `!Send`; it is re-cast to a
/// pointer by the peer that registers it.
struct ShardPublish {
    backing_base: usize,
    backing_len: usize,
    /// Fabric memory region for this shard's pool backing. Shared RPC
    /// workers use it to source owner responses directly from pinned
    /// bufferpool pages.
    fabric_mr: MrHandle,
    /// NUMA node this shard's cores and backing are pinned to (`None`
    /// when unpinned). Forwarded into [`PeerPublish`] so every shard can
    /// build its NUMA -> serving-shard table for NUMA-local fetch
    /// routing.
    numa: Option<u16>,
    fetch_channel: FetchChannel,
    /// Shared Drop carrier for this shard's backing allocation. The
    /// layer retains it (in [`shard_layer::ShardLayer`]) and frees it
    /// only after every shard thread has joined, so a peer
    /// coordinator's still-live socket ring never references unmapped
    /// pages during teardown. Not forwarded into [`PeerPublish`]: peers
    /// only need the region's base/len to register it, not ownership.
    backing_keepalive: Arc<dyn Send + Sync>,
}

/// One entry in the broadcast peer list every shard receives in phase B.
/// `shard_index` is the position in the worker-index-sorted shard order,
/// which keeps ownership indices consistent across the fan-out data path.
struct PeerPublish {
    shard_index: usize,
    worker_idx: WorkerIdx,
    backing_base: usize,
    backing_len: usize,
    channel: FetchChannel,
    /// NUMA node this peer shard is pinned to (`None` when unpinned).
    /// Feeds each shard's NUMA -> serving-shard table so fetches land on
    /// a shard co-located with the drive that backs the stripe.
    numa: Option<u16>,
}

/// Phase-B readiness a shard reports after registering its peers'
/// backings and building its fan-out surface. Separate from
/// [`ShardReady`] so the layer can wait for the second rendezvous (peer
/// registration) independently of the first (fabric/pool bring-up).
enum PhaseBReport {
    Ready(WorkerIdx),
    Failed(String),
}

/// RAII guard ensuring a shard that has entered phase B reports exactly
/// once. `report_ready`/`report_failed` send the terminal report and
/// disarm; if the shard returns early or panics before reporting, `Drop`
/// sends `Failed` so the layer's bounded phase-B collection never hangs.
struct PhaseBGuard {
    widx: WorkerIdx,
    tx: mpsc::Sender<PhaseBReport>,
    reported: bool,
}

impl PhaseBGuard {
    fn new(widx: WorkerIdx, tx: mpsc::Sender<PhaseBReport>) -> Self {
        Self {
            widx,
            tx,
            reported: false,
        }
    }

    fn report_ready(&mut self) {
        self.reported = true;
        let _ = self.tx.send(PhaseBReport::Ready(self.widx));
    }

    fn report_failed(&mut self, msg: String) {
        self.reported = true;
        let _ = self.tx.send(PhaseBReport::Failed(msg));
    }
}

impl Drop for PhaseBGuard {
    fn drop(&mut self) {
        if !self.reported {
            let _ = self.tx.send(PhaseBReport::Failed(format!(
                "worker={}: aborted during phase B bring-up",
                self.widx.0
            )));
        }
    }
}

/// Parsed command-line options for one run of the daemon.
///
/// The daemon takes all of its configuration - both the dynamically
/// reloadable cluster state (mesh, caches, backends, frontends)
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
        Ok(c) => {
            if path.extension().and_then(|e| e.to_str()) == Some("binpb") {
                eprintln!("config: loaded {} (binpb):\n{c:#?}", path.display());
            }
            Ok(c)
        }
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
    use unbounded_storage::topology::{NicWorker, NicWorkerGroup};

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

    fn default_fabric() -> config::FabricCfg {
        let mut cfg = Config::default();
        cfg.apply_defaults();
        cfg.startup().fabric().clone()
    }

    fn auto_rdma_fabric(hcas_per_numa_node: Option<u64>) -> config::FabricCfg {
        config::FabricCfg {
            binds: Some(config::fabric_cfg::Binds::AutoRdma(
                config::AutoRdmaFabricBinds { hcas_per_numa_node },
            )),
            ..Default::default()
        }
    }

    #[test]
    fn startup_to_core_plan_config_default_mapping() {
        // The defaults-populated `[startup.topology]` must map to the
        // `CorePlanConfig` defaults: nic_workers=4, serving_cores=auto,
        // and the historical safe placement flags.
        let cc = startup_to_core_plan_config(&default_topology(), &default_fabric());
        let d = CorePlanConfig::default();
        assert_eq!(cc.nic_workers, 4);
        assert_eq!(cc.serving_cores, None);
        assert_eq!(cc.hcas_per_numa, 1);
        // Default-off topology flags map to the historical defaults.
        assert!(!cc.use_smt_siblings);
        assert!(cc.respect_isolated);
        assert!(cc.exclude_node_cpu0);
        assert!(cc.require_active_port);
        assert!(!cc.disable_rdma);
        // Fields with no knob retain CorePlanConfig defaults.
        assert_eq!(cc.require_node_type_ca, d.require_node_type_ca);
    }

    #[test]
    fn startup_to_core_plan_config_inverts_negative_flags() {
        // The inverted-sense topology knobs (ignore_isolated,
        // include_node_cpu0, allow_inactive_port) must flip the
        // corresponding CorePlanConfig fields, and nic_workers /
        // serving_cores must pass through.
        let topology = config::TopologyCfg {
            use_smt_siblings: true,
            ignore_isolated: true,
            include_node_cpu0: true,
            allow_inactive_port: true,
            disable_rdma: true,
            serving_cores: Some(12),
            nic_workers: Some(6),
        };
        let cc = startup_to_core_plan_config(&topology, &auto_rdma_fabric(Some(2)));
        assert_eq!(cc.nic_workers, 6);
        assert_eq!(cc.serving_cores, Some(12));
        assert_eq!(cc.hcas_per_numa, 2);
        assert!(cc.use_smt_siblings);
        assert!(!cc.respect_isolated);
        assert!(!cc.exclude_node_cpu0);
        assert!(!cc.require_active_port);
        assert!(cc.disable_rdma);
    }

    #[test]
    fn startup_to_core_plan_config_absent_fields_default() {
        // Absent optional topology knobs map to CorePlanConfig defaults,
        // and absent serving_cores means auto.
        let topology = config::TopologyCfg {
            nic_workers: None,
            serving_cores: None,
            ..default_topology()
        };
        let cc = startup_to_core_plan_config(&topology, &auto_rdma_fabric(None));
        assert_eq!(cc.nic_workers, 4);
        assert_eq!(cc.serving_cores, None);
        assert_eq!(cc.hcas_per_numa, 1);
    }

    #[test]
    fn assign_shard_devices_decouples_serving_from_hca_count() {
        // One active HCA but three serving shards: every shard still
        // gets a device, proving serving capacity is no longer pinned
        // to the HCA count (the Phase 2.5 decoupling invariant).
        let plan = CorePlan {
            storage_cores: vec![],
            nic_workers: vec![NicWorkerGroup {
                hca: 0,
                numa: Some(0),
                workers: vec![NicWorker {
                    cpu: 1,
                    numa: Some(0),
                }],
            }],
            serving_shards: vec![
                ServingShard {
                    cpu: 2,
                    numa: Some(0),
                },
                ServingShard {
                    cpu: 3,
                    numa: Some(0),
                },
                ServingShard {
                    cpu: 4,
                    numa: Some(0),
                },
            ],
            numa_pools: vec![],
        };
        let devs = assign_shard_devices(&plan, &["mlx5_0".to_string()]);
        assert_eq!(
            devs,
            vec![
                Some("mlx5_0".to_string()),
                Some("mlx5_0".to_string()),
                Some("mlx5_0".to_string()),
            ],
        );
    }

    #[test]
    fn assign_shard_devices_prefers_numa_local_hca() {
        // Two HCAs on different nodes; each shard binds the HCA on its
        // own NUMA node.
        let plan = CorePlan {
            storage_cores: vec![],
            nic_workers: vec![
                NicWorkerGroup {
                    hca: 0,
                    numa: Some(0),
                    workers: vec![],
                },
                NicWorkerGroup {
                    hca: 1,
                    numa: Some(1),
                    workers: vec![],
                },
            ],
            serving_shards: vec![
                ServingShard {
                    cpu: 10,
                    numa: Some(1),
                },
                ServingShard {
                    cpu: 11,
                    numa: Some(0),
                },
            ],
            numa_pools: vec![],
        };
        let devs = assign_shard_devices(&plan, &["mlx5_0".to_string(), "mlx5_1".to_string()]);
        assert_eq!(
            devs,
            vec![Some("mlx5_1".to_string()), Some("mlx5_0".to_string())],
        );
    }

    #[test]
    fn assign_shard_devices_tcp_fallback_when_no_active_hca() {
        // No NIC-worker groups (RDMA disabled or none usable): every
        // shard takes the tcp loopback fallback (`None`).
        let plan = CorePlan {
            storage_cores: vec![],
            nic_workers: vec![],
            serving_shards: vec![
                ServingShard {
                    cpu: 2,
                    numa: Some(0),
                },
                ServingShard {
                    cpu: 3,
                    numa: Some(0),
                },
            ],
            numa_pools: vec![],
        };
        let devs = assign_shard_devices(&plan, &[]);
        assert_eq!(devs, vec![None, None]);
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
        // leaves `startup` unset and panics the daemon at
        // startup when it reads `cfg.startup()`.
        let path = Path::new("/definitely/not/a/real/path/unbounded-storage.toml");
        let cfg = load_config(path, false).expect("missing default path falls back to defaults");
        // These accessors panic if defaults were not applied.
        assert_eq!(
            cfg.startup().fabric().default_listen_addr(),
            Some("0.0.0.0:0")
        );
        assert_eq!(
            cfg.startup().memory().memory_total_bytes,
            Some(128 * 1024 * 1024)
        );
    }

    #[test]
    fn load_config_missing_explicit_path_is_fatal() {
        // An explicit path that is missing stays fatal: only the default
        // path is allowed to silently fall back to built-in defaults.
        let path = Path::new("/definitely/not/a/real/path/unbounded-storage.toml");
        assert!(load_config(path, true).is_err());
    }

    fn backend_spec(id: &str) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(config::backend_spec::Config::Http(
                config::HttpBackendConfig {
                    url: "https://example.com".to_string(),
                    stripe_size_bytes: Some(4 * 1024 * 1024),
                    http_concurrency: Some(64),
                    ca_cert_path: None,
                    insecure_skip_verify: false,
                    client_cert_path: None,
                    client_key_path: None,
                },
            )),
        }
    }

    fn binding(frontend_id: &str, backend_id: &str) -> ResolvedFrontendBinding {
        ResolvedFrontendBinding {
            frontend_id: frontend_id.to_string(),
            backend_id: backend_id.to_string(),
            cache_id: None,
            bypass_cache: true,
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
    fn backend_stripe_size_change_rebuilds_frontend() {
        let mut last_bindings = HashMap::new();
        last_bindings.insert("f".to_string(), binding("f", "b"));

        let mut next_bindings = HashMap::new();
        next_bindings.insert("f".to_string(), binding("f", "b"));

        let old_backend = backend_spec("b");
        let mut new_backend = old_backend.clone();
        let Some(config::backend_spec::Config::Http(cfg)) = new_backend.config.as_mut() else {
            panic!("expected http backend config");
        };
        *cfg.stripe_size_bytes.as_mut().expect("stripe size set") *= 2;

        let mut last_backends = HashMap::new();
        last_backends.insert(old_backend.name.clone(), old_backend);

        assert_eq!(
            frontend_rebuild_ids(
                &last_bindings,
                &next_bindings,
                &last_backends,
                &[new_backend]
            ),
            vec!["f".to_string()],
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
