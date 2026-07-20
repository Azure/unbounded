// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Fabric grouping: lifts the libfabric endpoint and its shared RPC
//! server out of the per-shard bring-up.
//!
//! Before this module each serving shard created and owned its own
//! `Fabric`. That is correct for the tcp fallback (one shard per CPU,
//! one loopback endpoint each) but wrong for verbs, where several
//! serving shards share a single HCA and must therefore share one
//! libfabric domain rather than open one per shard.
//!
//! [`plan_fabric_units`] is the single seam between the two data paths:
//! it decides how serving shards map onto fabric endpoints. Everything
//! downstream - [`FabricGroup::new`] and the per-shard bring-up in
//! `run_shard` - is provider-agnostic and byte-for-byte identical for
//! tcp and verbs, so the tcp smoke test exercises the same lift-out code
//! the (hardware-only) verbs path uses.

use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use unbounded_storage::config::{self, BackendSpec, RuntimePeer};
use unbounded_storage::fabric::{
    self, ConnectionSpec, Fabric, MrHandle, PeerId, Provider, RpcServerHandle,
};
use unbounded_storage::fanout::FetchChannel;
use unbounded_storage::memory::{Backing, BackingKind, BackingRequest, HUGEPAGE_2MB, allocate};
use unbounded_storage::p2p::{
    OwnerShardSource, OwnerShardTable, RecursiveHandler, RouteTableHandle,
};
use unbounded_storage::runtime::{Threading, WorkerIdx};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::storage::disks::CacheDirectorySet;
use unbounded_storage::topology::{NicWorkerGroup, ServingShard};

use crate::FabricStartup;

const DISCOVERY_RETRY: Duration = Duration::from_secs(1);
const DISCOVERY_REFRESH: Duration = Duration::from_secs(30);

/// Number of 2 MiB scratch pages each fabric unit reserves for its RPC
/// server. The RPC handler resolves cross-node cache misses into these
/// pages, so the bound is set by RPC concurrency, not shard count: it is
/// the same for a 1:1 tcp unit and an N:1 verbs unit.
const RPC_SCRATCH_PAGES: u32 = 8;

/// One libfabric endpoint to bring up, plus the serving shards that will
/// register their data backings against it. Produced by
/// [`plan_fabric_units`] and realized by [`FabricGroup::new`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FabricUnitSpec {
    pub unit_idx: usize,
    pub device_name: String,
    pub provider: Provider,
    /// Workers over which the fabric's progress and RPC threads are spread.
    pub worker_indices: Vec<WorkerIdx>,
    pub numa: Option<u16>,
    /// Serving-shard indices that share this endpoint.
    pub shards_assigned: Vec<usize>,
    /// Forward-looking memory-region count used only for the domain
    /// capacity check (one data backing per assigned shard plus the one
    /// shared scratch region).
    pub expected_mr: usize,
}

/// The full plan: the set of fabric units to construct and, for each
/// serving-shard index, the unit it maps onto.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct FabricPlan {
    pub units: Vec<FabricUnitSpec>,
    pub shard_to_unit: Vec<usize>,
}

/// Fabric address published by a live unit after libfabric bind/listen
/// completed. These are the addresses peers must dial; configured bind
/// strings are not sufficient for verbs because libfabric owns the native
/// address encoding.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct FabricUnitAddress {
    pub device_name: String,
    pub rdma: bool,
    pub addr: String,
}

/// Decide how serving shards map onto fabric endpoints. This function is
/// THE tcp/verbs seam (see the module docs).
///
/// `shard_devices` is all-`None` (tcp fallback) or all-`Some` (verbs),
/// per `assign_shard_devices`:
/// - tcp: a single `lo` endpoint shared by every serving shard, pinned to
///   worker 0. Peers reach a node through one static process peer addr,
///   so a node must expose exactly one inbound
///   fabric endpoint on that fixed port; binding one endpoint per shard
///   would make every shard past the first collide on the port
///   (`fi_endpoint` -> EADDRINUSE) once `serving_cores > 1`.
/// - verbs: one endpoint per distinct HCA device, using all workers in
///   the matching nic-worker group and shared by every assigned shard.
pub fn plan_fabric_units(
    serving_shards: &[ServingShard],
    shard_devices: &[Option<String>],
    nic_workers: &[NicWorkerGroup],
    hca_dev_names: &[String],
) -> FabricPlan {
    if shard_devices.iter().all(|d| d.is_none()) {
        plan_tcp_units(serving_shards)
    } else {
        plan_verbs_units(serving_shards, shard_devices, nic_workers, hca_dev_names)
    }
}

/// tcp fallback: a single `lo` endpoint shared by every serving shard,
/// pinned to worker 0. A node advertises one static fabric address to its
/// peers, so it must bind exactly one inbound
/// endpoint on that fixed port; one endpoint per shard would collide
/// (`fi_endpoint` -> EADDRINUSE) as soon as more than one shard serves.
/// The shared unit carries no NUMA affinity (`numa = None`) so shards
/// from any node can register their data backing against it without
/// tripping the `register_backing` NUMA-mismatch check.
fn plan_tcp_units(serving_shards: &[ServingShard]) -> FabricPlan {
    if serving_shards.is_empty() {
        return FabricPlan::default();
    }

    let shards_assigned: Vec<usize> = (0..serving_shards.len()).collect();
    let units = vec![FabricUnitSpec {
        unit_idx: 0,
        device_name: "lo".to_string(),
        provider: Provider::Tcp,
        worker_indices: vec![WorkerIdx(0)],
        numa: None,
        shards_assigned,
        // One data backing per assigned shard plus the one shared scratch.
        expected_mr: serving_shards.len() + 1,
    }];
    let shard_to_unit = vec![0usize; serving_shards.len()];

    FabricPlan {
        units,
        shard_to_unit,
    }
}

/// verbs: group serving shards by their assigned HCA device (in
/// first-appearance order) so each distinct device gets exactly one
/// shared endpoint.
fn plan_verbs_units(
    serving_shards: &[ServingShard],
    shard_devices: &[Option<String>],
    nic_workers: &[NicWorkerGroup],
    hca_dev_names: &[String],
) -> FabricPlan {
    let serving_count = serving_shards.len();
    let mut units: Vec<FabricUnitSpec> = Vec::new();
    let mut shard_to_unit = vec![0usize; shard_devices.len()];
    let mut device_to_unit: HashMap<String, usize> = HashMap::new();

    for (i, dev) in shard_devices.iter().enumerate() {
        // Defensive: a stray `None` in an otherwise-verbs plan degrades
        // that shard to loopback rather than panicking.
        let device = dev.clone().unwrap_or_else(|| "lo".to_string());
        let unit_idx = *device_to_unit.entry(device.clone()).or_insert_with(|| {
            let idx = units.len();
            let (worker_indices, numa) =
                verbs_workers_for_device(&device, serving_count, nic_workers, hca_dev_names);
            units.push(FabricUnitSpec {
                unit_idx: idx,
                device_name: device.clone(),
                provider: Provider::from_device_name(&device),
                worker_indices,
                numa,
                shards_assigned: Vec::new(),
                expected_mr: 0,
            });
            idx
        });
        units[unit_idx].shards_assigned.push(i);
        shard_to_unit[i] = unit_idx;
    }

    for unit in &mut units {
        unit.expected_mr = unit.shards_assigned.len() + 1;
    }

    FabricPlan {
        units,
        shard_to_unit,
    }
}

/// Worker indices (and NUMA) available to a verbs endpoint on `device`.
/// The
/// runtime lays serving shards out at `0..serving_count` and the
/// nic-worker groups after them, flattened in order, so a group's base
/// is `serving_count` plus the worker counts of all earlier groups.
fn verbs_workers_for_device(
    device: &str,
    serving_count: usize,
    nic_workers: &[NicWorkerGroup],
    hca_dev_names: &[String],
) -> (Vec<WorkerIdx>, Option<u16>) {
    let mut flat_base = 0usize;

    for group in nic_workers {
        let matches = hca_dev_names
            .get(group.hca)
            .is_some_and(|name| name == device);
        if matches && !group.workers.is_empty() {
            let workers = (0..group.workers.len())
                .map(|offset| WorkerIdx((serving_count + flat_base + offset) as u16))
                .collect();
            return (workers, group.numa);
        }
        flat_base += group.workers.len();
    }

    // No matching nic-worker group. Unreachable when `shard_devices`
    // came from `assign_shard_devices` over these same `nic_workers`;
    // fall back to worker 0 so the unit still pins somewhere valid.
    (vec![WorkerIdx(0)], None)
}

/// A shard phase-A publication needed by shared RPC workers.
#[derive(Clone)]
pub struct RpcShardPublish {
    pub shard_index: usize,
    pub fetch_channel: FetchChannel,
    pub mr: MrHandle,
    pub numa: Option<u16>,
}

/// A live fabric endpoint plus delayed RPC server state.
///
/// Field order is drop order and mirrors the pre-lift per-shard teardown
/// exactly: `rpc_server` drops first (it signals shutdown and joins its
/// worker pool), then scratch/routing, then `fabric` last (closing every
/// memory region registered against the domain: the shared scratch plus
/// every assigned shard's data backing).
/// Closing the domain while a worker still touches it would be unsound,
/// hence the order.
///
/// The four teardown-ordered resources are type parameters defaulted to
/// the production types, so the rest of the module refers to this as the
/// plain `FabricUnit`. Bringing a real unit up needs a live libfabric
/// domain and pinned backings (what the smoke test covers), so the
/// parameters exist solely so a unit test can substitute drop-logging
/// stand-ins and lock this ordering hardware-free; see
/// `fabric_unit_drops_resources_in_teardown_order`.
struct FabricUnit<
    S = Option<RpcServerHandle>,
    Sc = Option<Backing>,
    Rt = RouteTableHandle,
    F = Arc<Fabric>,
> {
    // Held only for its Drop: see the struct doc for the teardown order.
    #[allow(dead_code)]
    rpc_server: S,
    scratch: Sc,
    routes: Rt,
    fabric: F,
    scratch_mr: MrHandle,
    page_size: usize,
    shards_assigned: Vec<usize>,
    cache_directories: Arc<CacheDirectorySet>,
    worker_idx: WorkerIdx,
    device_name: String,
    rdma: bool,
    self_addr: String,
}

/// The set of fabric endpoints serving a process, owning every shared
/// `Fabric` and RPC server. Constructed once at shard-layer bring-up and
/// dropped during teardown after the shard threads have joined.
///
/// This is the per-HCA runtime fabric owner. The CPU-side worker
/// grouping it pins to lives separately in `topology::NicWorkerGroup`:
/// topology decides which CPUs are NIC workers, this owns the `Fabric`
/// those workers progress.
pub struct FabricGroup {
    discovery: DiscoveryCoordinator,
    units: Vec<FabricUnit>,
    shard_to_unit: Vec<usize>,
}

impl FabricGroup {
    /// Bring up every endpoint in `plan`. On any failure the units
    /// already built are dropped (closing their fabrics and servers) and
    /// all collected errors are returned.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        runtime: &Arc<dyn Threading>,
        plan: &FabricPlan,
        backing_kind: BackingKind,
        fabric_startup: &FabricStartup,
        _backend_specs: &[BackendSpec],
        cache_directories: Arc<CacheDirectorySet>,
        routes: &RouteTableHandle,
        peers: &[RuntimePeer],
        self_peer: PeerId,
    ) -> Result<Self, Vec<String>> {
        let mut units = Vec::with_capacity(plan.units.len());
        let mut errors = Vec::new();

        for spec in &plan.units {
            match build_unit(
                runtime,
                spec,
                backing_kind,
                fabric_startup,
                &cache_directories,
                routes,
                self_peer,
            ) {
                Ok(unit) => units.push(unit),
                Err(e) => errors.push(e),
            }
        }

        if !errors.is_empty() {
            // Tear down any endpoints already up before returning so a
            // partial bring-up does not leak fabrics or worker threads.
            drop(units);
            return Err(errors);
        }

        let discovery = DiscoveryCoordinator::spawn(
            units.iter().map(|unit| unit.fabric.clone()).collect(),
            peers.to_vec(),
        );

        Ok(Self {
            discovery,
            units,
            shard_to_unit: plan.shard_to_unit.clone(),
        })
    }

    /// The fabric a serving shard registers its data backing against and
    /// builds its client transport on. The unit retains ownership; the
    /// returned clone keeps the domain alive only until teardown drops
    /// the unit.
    pub fn fabric_for_shard(&self, shard: usize) -> Arc<Fabric> {
        let unit = self.shard_to_unit[shard];
        self.units[unit].fabric.clone()
    }

    /// Live local addresses for every fabric unit in deterministic unit order.
    pub fn unit_addresses(&self) -> Vec<FabricUnitAddress> {
        self.units
            .iter()
            .map(|unit| FabricUnitAddress {
                device_name: unit.device_name.clone(),
                rdma: unit.rdma,
                addr: unit.self_addr.clone(),
            })
            .collect()
    }

    /// Start every shared RPC server after shards have published the
    /// bufferpool MRs/fetch channels the owner path sources from.
    pub fn start_rpc_servers(&mut self, shards: &[RpcShardPublish]) -> Result<(), Vec<String>> {
        let mut errors = Vec::new();
        for unit in &mut self.units {
            if unit.rpc_server.is_some() {
                continue;
            }

            let worker = unit.worker_idx.0;
            let owner_entries: Vec<OwnerShardSource> = shards
                .iter()
                .filter(|shard| unit.shards_assigned.contains(&shard.shard_index))
                .map(|shard| OwnerShardSource {
                    shard_index: shard.shard_index,
                    channel: shard.fetch_channel.clone(),
                    mr: shard.mr,
                    numa: shard.numa,
                })
                .collect();
            let owners = OwnerShardTable::new(
                owner_entries,
                unit.cache_directories.clone(),
                unit.page_size,
            );
            let Some(scratch) = unit.scratch.take() else {
                errors.push(format!("worker={worker}: rpc scratch already consumed"));
                continue;
            };
            let handler = match RecursiveHandler::with_routes(
                scratch,
                RPC_SCRATCH_PAGES,
                unit.routes.clone(),
                unit.fabric.clone(),
                unit.scratch_mr,
                unit.page_size,
                owners,
            ) {
                Ok(handler) => Arc::new(handler),
                Err(e) => {
                    errors.push(format!(
                        "worker={worker}: RecursiveHandler::with_routes: {e}"
                    ));
                    continue;
                }
            };
            match unit.fabric.start_rpc_server::<StripeReq, _>(
                handler,
                Some(unit.scratch_mr),
                unit.page_size,
            ) {
                Ok(server) => unit.rpc_server = Some(server),
                Err(e) => errors.push(format!("worker={worker}: start_rpc_server: {e}")),
            }
        }

        if errors.is_empty() {
            Ok(())
        } else {
            Err(errors)
        }
    }

    /// Re-drive every endpoint's fabric connection table toward `peers`.
    /// Connections live at the fabric/address-vector level, so this runs
    /// once per endpoint rather than once per shard.
    pub fn reconcile_peers(&mut self, peers: &[RuntimePeer]) {
        self.discovery.update(peers.to_vec());
    }

    /// RPC owner reads are served through shard bufferpools, whose
    /// backend registries are reconciled by shard control broadcasts.
    pub fn reconcile_backends(&mut self, _backends: &[BackendSpec]) {}
}

/// Bring up one fabric endpoint from its spec. Mirrors the pre-lift
/// per-shard fabric + RPC bring-up; the only behavioral change is that
/// the endpoint is shared by `spec.shards_assigned` rather than owned by
/// a single shard.
#[allow(clippy::too_many_arguments)]
fn build_unit(
    runtime: &Arc<dyn Threading>,
    spec: &FabricUnitSpec,
    backing_kind: BackingKind,
    fabric_startup: &FabricStartup,
    cache_directories: &Arc<CacheDirectorySet>,
    routes: &RouteTableHandle,
    self_peer: PeerId,
) -> Result<FabricUnit, String> {
    let worker_idx = spec
        .worker_indices
        .first()
        .copied()
        .ok_or_else(|| format!("fabric unit {} has no workers", spec.unit_idx))?;
    let worker = worker_idx.0;

    let mut cfg = fabric::defaults_for(spec.device_name.clone(), runtime.clone(), worker_idx);
    cfg.worker_indices = spec.worker_indices.clone();
    cfg.provider = spec.provider;
    cfg.listen = true;
    cfg.listen_addr = Some(
        fabric_startup
            .listen_addr_for_unit(spec.unit_idx)
            .to_string(),
    );
    cfg.max_inflight = fabric_startup.max_inflight as usize;
    cfg.rpc_worker_threads = fabric_startup.rpc_worker_threads as usize;
    cfg.progress_threads = fabric_startup.progress_threads as u8;
    cfg.progress_poll_us = fabric_startup.progress_poll_us;
    cfg.numa = spec.numa;
    // The local peer identity is the connection-manager private data this
    // node sends on every outbound dial; the accepting side keys the
    // inbound connection by it. `defaults_for` leaves it PeerId(0), so it
    // must be set here from the node's configured local id or accepted
    // connections are mis-keyed and `resolve_peer` cannot find the peer.
    cfg.self_peer = self_peer;

    let fabric = Arc::new(Fabric::new(cfg).map_err(|e| format!("worker={worker}: fabric: {e}"))?);
    let self_addr = fabric
        .self_address()
        .map_err(|e| format!("worker={worker}: self_address: {e}"))?;
    println!(
        "fabric unit up: worker={worker} dev={} provider={:?} numa={} shards={:?} \
         self_addr={} self_addr_bytes={}",
        spec.device_name,
        spec.provider,
        spec.numa
            .map(|n| n.to_string())
            .unwrap_or_else(|| "none".into()),
        spec.shards_assigned,
        self_addr,
        self_addr.len(),
    );

    // Shared RPC scratch: `RPC_SCRATCH_PAGES` 2 MiB pages. `allocate`
    // always uses 2 MiB pages, so request that many pages' worth of
    // bytes and read the realized `page_size` back.
    let scratch = allocate(BackingRequest {
        kind: backing_kind,
        bytes: HUGEPAGE_2MB * RPC_SCRATCH_PAGES as usize,
        numa: spec.numa,
    })
    .map_err(|e| format!("worker={worker}: rpc scratch alloc: {e}"))?;
    let page_size = scratch.page_size;
    let scratch_mr = fabric
        .register_backing(&scratch, spec.numa)
        .map_err(|e| format!("worker={worker}: register rpc scratch: {e}"))?;

    fabric.check_shared_domain_capacity(spec.expected_mr);

    Ok(FabricUnit {
        rpc_server: None,
        scratch: Some(scratch),
        routes: routes.clone(),
        fabric,
        scratch_mr,
        page_size,
        shards_assigned: spec.shards_assigned.clone(),
        cache_directories: cache_directories.clone(),
        worker_idx,
        device_name: spec.device_name.clone(),
        rdma: spec.provider == Provider::Verbs,
        self_addr,
    })
}

struct DiscoveryCoordinator {
    updates: mpsc::Sender<Vec<RuntimePeer>>,
    shutdown: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
}

impl DiscoveryCoordinator {
    fn spawn(fabrics: Vec<Arc<Fabric>>, peers: Vec<RuntimePeer>) -> Self {
        let (updates, receiver) = mpsc::channel();
        let shutdown = Arc::new(AtomicBool::new(false));
        let thread_shutdown = shutdown.clone();
        let thread = std::thread::Builder::new()
            .name("fabric-discovery".to_string())
            .spawn(move || run_discovery(fabrics, peers, receiver, thread_shutdown))
            .expect("spawn fabric discovery thread");
        Self {
            updates,
            shutdown,
            thread: Some(thread),
        }
    }

    fn update(&self, peers: Vec<RuntimePeer>) {
        let _ = self.updates.send(peers);
    }
}

impl Drop for DiscoveryCoordinator {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

struct CachedPeer {
    endpoint: String,
    refreshed: Instant,
    addresses: Vec<fabric::FabricAddress>,
}

fn run_discovery(
    fabrics: Vec<Arc<Fabric>>,
    mut peers: Vec<RuntimePeer>,
    updates: mpsc::Receiver<Vec<RuntimePeer>>,
    shutdown: Arc<AtomicBool>,
) {
    let mut cache: HashMap<PeerId, CachedPeer> = HashMap::new();
    let mut applied = vec![HashMap::new(); fabrics.len()];

    while !shutdown.load(Ordering::Acquire) {
        while let Ok(update) = updates.try_recv() {
            peers = update;
        }
        let live: std::collections::HashSet<PeerId> =
            peers.iter().map(|peer| peer.fabric_peer_id).collect();
        cache.retain(|peer, _| live.contains(peer));

        for peer in &peers {
            if shutdown.load(Ordering::Acquire) {
                break;
            }

            let Some(config::peer_spec::Config::Rdma(rdma)) = peer.spec.config.as_ref() else {
                continue;
            };
            if rdma.discovery_addr.is_empty() {
                continue;
            }
            if cache.get(&peer.fabric_peer_id).is_some_and(|cached| {
                cached.endpoint == rdma.discovery_addr
                    && cached.refreshed.elapsed() < DISCOVERY_REFRESH
            }) {
                continue;
            }
            match resolve_discovered_peer(&fabrics, &rdma.discovery_addr) {
                Ok(addresses) => {
                    cache.insert(
                        peer.fabric_peer_id,
                        CachedPeer {
                            endpoint: rdma.discovery_addr.clone(),
                            refreshed: Instant::now(),
                            addresses,
                        },
                    );
                }
                Err(error) => eprintln!(
                    "fabric discovery failed: peer={} endpoint={} err={error}",
                    peer.name, rdma.discovery_addr
                ),
            }
        }

        for (unit_idx, fabric) in fabrics.iter().enumerate() {
            let mut desired = Vec::new();
            for peer in &peers {
                match peer.spec.config.as_ref() {
                    Some(config::peer_spec::Config::Tcp(tcp)) => desired.push(ConnectionSpec {
                        peer: peer.fabric_peer_id,
                        address: fabric::FabricAddress::socket(tcp.addr.clone()),
                        hca_numa: None,
                        tags: peer.spec.tags.clone(),
                    }),
                    Some(config::peer_spec::Config::Rdma(_)) => {
                        let discovered = cache
                            .get(&peer.fabric_peer_id)
                            .map(|cached| cached.addresses.as_slice());
                        if let Some(address) =
                            discovered.and_then(|addresses| addresses.get(unit_idx))
                        {
                            desired.push(ConnectionSpec {
                                peer: peer.fabric_peer_id,
                                address: address.clone(),
                                hca_numa: None,
                                tags: peer.spec.tags.clone(),
                            });
                        }
                    }
                    None => {}
                }
            }
            let report = config::reconcile::reconcile_connections(
                fabric,
                &desired,
                Some(&applied[unit_idx]),
            );
            applied[unit_idx] = report.applied;
            for (peer, error) in report.failures {
                eprintln!("fabric peer reconcile failed: peer={} err={error}", peer.0);
            }
            fabric.set_desired_peers(desired);
        }

        match updates.recv_timeout(DISCOVERY_RETRY) {
            Ok(update) => peers = update,
            Err(RecvTimeoutError::Timeout) => {}
            Err(RecvTimeoutError::Disconnected) => break,
        }
    }
}

fn resolve_discovered_peer(
    fabrics: &[Arc<Fabric>],
    discovery_addr: &str,
) -> Result<Vec<fabric::FabricAddress>, String> {
    let endpoint: SocketAddr = discovery_addr
        .parse()
        .map_err(|_| "invalid discovery socket address".to_string())?;
    let candidates =
        unbounded_storage::fabric_discovery::fetch(endpoint).map_err(|error| error.to_string())?;
    let candidates: Vec<fabric::FabricAddress> = candidates
        .iter()
        .map(|candidate| candidate_address(candidate, endpoint))
        .collect::<Result<_, _>>()?;
    complete_matching(fabrics, &candidates)
}

fn candidate_address(addr: &str, endpoint: SocketAddr) -> Result<fabric::FabricAddress, String> {
    if let Ok(mut socket) = addr.parse::<SocketAddr>() {
        if socket.port() == 0 {
            return Err(format!("invalid fabric candidate {addr:?}"));
        }
        if socket.ip().is_unspecified() {
            socket.set_ip(endpoint.ip());
        }
        Ok(fabric::FabricAddress::socket(socket.to_string()))
    } else if valid_native_address(addr) {
        Ok(fabric::FabricAddress::native(addr.to_string()))
    } else {
        Err(format!("invalid fabric candidate {addr:?}"))
    }
}

fn valid_native_address(addr: &str) -> bool {
    let Some(hex) = addr.strip_prefix("hex:") else {
        return false;
    };
    !hex.is_empty() && hex.len() % 2 == 0 && hex.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn complete_matching(
    fabrics: &[Arc<Fabric>],
    candidates: &[fabric::FabricAddress],
) -> Result<Vec<fabric::FabricAddress>, String> {
    let compatible: Vec<Vec<bool>> = fabrics
        .iter()
        .map(|fabric| {
            candidates
                .iter()
                .map(|candidate| fabric.destination_resolves(candidate).is_ok())
                .collect()
        })
        .collect();
    let indices = maximum_matching(&compatible)
        .ok_or_else(|| "no complete local-fabric to remote-listener matching".to_string())?;
    Ok(indices
        .into_iter()
        .map(|candidate| candidates[candidate].clone())
        .collect())
}

fn maximum_matching(compatible: &[Vec<bool>]) -> Option<Vec<usize>> {
    if compatible.is_empty() {
        return Some(Vec::new());
    }
    let candidates = compatible.first()?.len();
    if candidates < compatible.len() || compatible.iter().any(|row| row.len() != candidates) {
        return None;
    }

    let mut candidate_owner = vec![None; candidates];
    for local in 0..compatible.len() {
        let mut seen = vec![false; candidates];
        if !augment(local, compatible, &mut seen, &mut candidate_owner) {
            return None;
        }
    }
    let mut result = vec![usize::MAX; compatible.len()];
    for (candidate, local) in candidate_owner.into_iter().enumerate() {
        if let Some(local) = local {
            result[local] = candidate;
        }
    }
    result
        .iter()
        .all(|candidate| *candidate != usize::MAX)
        .then_some(result)
}

fn augment(
    local: usize,
    compatible: &[Vec<bool>],
    seen: &mut [bool],
    candidate_owner: &mut [Option<usize>],
) -> bool {
    for candidate in 0..candidate_owner.len() {
        if !compatible[local][candidate] || seen[candidate] {
            continue;
        }
        seen[candidate] = true;
        if candidate_owner[candidate].is_none()
            || augment(
                candidate_owner[candidate].expect("owner checked"),
                compatible,
                seen,
                candidate_owner,
            )
        {
            candidate_owner[candidate] = Some(local);
            return true;
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::rc::Rc;

    use super::*;

    fn shard(cpu: u32, numa: Option<u16>) -> ServingShard {
        ServingShard { cpu, numa }
    }

    fn nic_group(hca: usize, numa: Option<u16>, worker_count: usize) -> NicWorkerGroup {
        NicWorkerGroup {
            hca,
            numa,
            workers: (0..worker_count)
                .map(|i| unbounded_storage::topology::NicWorker {
                    cpu: 100 + i as u32,
                    numa,
                })
                .collect(),
        }
    }

    #[test]
    fn tcp_fallback_is_one_shared_unit() {
        let shards = [shard(0, Some(0)), shard(1, Some(0)), shard(2, Some(1))];
        let devices = [None, None, None];

        let plan = plan_fabric_units(&shards, &devices, &[], &[]);

        // All serving shards share one inbound endpoint on the fixed
        // listen port; one unit per shard would collide (EADDRINUSE).
        assert_eq!(plan.units.len(), 1);
        assert_eq!(plan.shard_to_unit, vec![0, 0, 0]);

        let unit = &plan.units[0];
        assert_eq!(unit.device_name, "lo");
        assert_eq!(unit.provider, Provider::Tcp);
        assert_eq!(unit.worker_indices, vec![WorkerIdx(0)]);
        // No NUMA affinity, so shards across nodes can register against it.
        assert_eq!(unit.numa, None);
        assert_eq!(unit.shards_assigned, vec![0, 1, 2]);
        assert_eq!(unit.expected_mr, 4); // 3 shards + 1 scratch
    }

    #[test]
    fn tcp_fallback_no_shards_is_empty_plan() {
        let plan = plan_fabric_units(&[], &[], &[], &[]);

        assert!(plan.units.is_empty());
        assert!(plan.shard_to_unit.is_empty());
    }

    #[test]
    fn verbs_groups_shards_by_device() {
        // Two HCAs; group 0 has two nic workers, group 1 has one. The
        // runtime lays the four serving shards out at workers 0..4, then
        // group 0's workers at 4,5 and group 1's worker at 6.
        let shards = [
            shard(0, Some(0)),
            shard(1, Some(1)),
            shard(2, Some(0)),
            shard(3, Some(1)),
        ];
        let devices = [
            Some("mlx5_0".to_string()),
            Some("mlx5_1".to_string()),
            Some("mlx5_0".to_string()),
            Some("mlx5_1".to_string()),
        ];
        let nic_workers = [nic_group(0, Some(0), 2), nic_group(1, Some(1), 1)];
        let hca_dev_names = ["mlx5_0".to_string(), "mlx5_1".to_string()];

        let plan = plan_fabric_units(&shards, &devices, &nic_workers, &hca_dev_names);

        assert_eq!(plan.units.len(), 2);
        assert_eq!(plan.shard_to_unit, vec![0, 1, 0, 1]);

        let u0 = &plan.units[0];
        assert_eq!(u0.device_name, "mlx5_0");
        assert_eq!(u0.provider, Provider::Verbs);
        assert_eq!(u0.worker_indices, vec![WorkerIdx(4), WorkerIdx(5)]);
        assert_eq!(u0.numa, Some(0));
        assert_eq!(u0.shards_assigned, vec![0, 2]);
        assert_eq!(u0.expected_mr, 3); // 2 shards + 1 scratch

        let u1 = &plan.units[1];
        assert_eq!(u1.device_name, "mlx5_1");
        assert_eq!(u1.provider, Provider::Verbs);
        assert_eq!(u1.worker_indices, vec![WorkerIdx(6)]);
        assert_eq!(u1.numa, Some(1));
        assert_eq!(u1.shards_assigned, vec![1, 3]);
        assert_eq!(u1.expected_mr, 3);
    }

    #[test]
    fn verbs_single_hca_shares_one_unit() {
        let shards = [shard(0, Some(0)), shard(1, Some(0))];
        let devices = [Some("mlx5_0".to_string()), Some("mlx5_0".to_string())];
        let nic_workers = [nic_group(0, Some(0), 4)];
        let hca_dev_names = ["mlx5_0".to_string()];

        let plan = plan_fabric_units(&shards, &devices, &nic_workers, &hca_dev_names);

        assert_eq!(plan.units.len(), 1);
        assert_eq!(plan.shard_to_unit, vec![0, 0]);
        assert_eq!(
            plan.units[0].worker_indices,
            vec![WorkerIdx(2), WorkerIdx(3), WorkerIdx(4), WorkerIdx(5)]
        );
        assert_eq!(plan.units[0].shards_assigned, vec![0, 1]);
        assert_eq!(plan.units[0].expected_mr, 3); // 2 shards + 1 scratch
    }

    #[test]
    fn matching_finds_complete_assignment_that_greedy_misses() {
        let compatible = vec![vec![true, true], vec![true, false]];
        assert_eq!(maximum_matching(&compatible), Some(vec![1, 0]));
    }

    #[test]
    fn matching_requires_every_local_fabric() {
        assert_eq!(maximum_matching(&[vec![true], vec![true]]), None);
        assert_eq!(
            maximum_matching(&[vec![false, true], vec![false, false]]),
            None
        );
    }

    #[test]
    fn candidate_rewrites_wildcard_to_discovery_ip() {
        assert_eq!(
            candidate_address("0.0.0.0:9000", "192.0.2.4:9101".parse().unwrap()).unwrap(),
            fabric::FabricAddress::socket("192.0.2.4:9000")
        );
        assert!(candidate_address("hex:0102", "192.0.2.4:9101".parse().unwrap()).is_ok());
        assert!(candidate_address("hex:no", "192.0.2.4:9101".parse().unwrap()).is_err());
        assert!(candidate_address("192.0.2.4:0", "192.0.2.4:9101".parse().unwrap()).is_err());
    }

    /// Drop-logging stand-in for a `FabricUnit` resource: records its
    /// label when dropped so a test can observe field teardown order.
    struct DropToken {
        label: &'static str,
        log: Rc<RefCell<Vec<&'static str>>>,
    }

    impl DropToken {
        fn new(label: &'static str, log: &Rc<RefCell<Vec<&'static str>>>) -> Self {
            Self {
                label,
                log: log.clone(),
            }
        }
    }

    impl Drop for DropToken {
        fn drop(&mut self) {
            self.log.borrow_mut().push(self.label);
        }
    }

    #[test]
    fn fabric_unit_drops_resources_in_teardown_order() {
        // A `FabricUnit`'s field order is its drop order, and that order
        // is the teardown contract: the RPC server (and the worker
        // threads it joins) must stop touching the domain before the
        // fabric closes it, with scratch and the route handle in
        // between. Bringing a real unit up needs a live libfabric domain
        // (covered by the smoke test), so here we substitute drop-logging
        // tokens for the four hardware-bound resources. This binds to the
        // production struct definition (same `FabricUnit`, fake generic
        // arguments), so reordering any field fails this test.
        let log: Rc<RefCell<Vec<&'static str>>> = Rc::new(RefCell::new(Vec::new()));

        let unit: FabricUnit<DropToken, DropToken, DropToken, DropToken> = FabricUnit {
            rpc_server: DropToken::new("rpc_server", &log),
            scratch: DropToken::new("scratch", &log),
            routes: DropToken::new("routes", &log),
            fabric: DropToken::new("fabric", &log),
            scratch_mr: MrHandle {
                mr: std::ptr::null_mut(),
                remote_key: 0,
                base: 0,
                remote_base: 0,
                len: 0,
            },
            page_size: 4096,
            shards_assigned: Vec::new(),
            cache_directories: CacheDirectorySet::new(),
            worker_idx: WorkerIdx(0),
            device_name: "mlx5_0".to_string(),
            rdma: true,
            self_addr: "hex:01".to_string(),
        };

        drop(unit);

        assert_eq!(
            *log.borrow(),
            vec!["rpc_server", "scratch", "routes", "fabric"],
        );
    }
}
