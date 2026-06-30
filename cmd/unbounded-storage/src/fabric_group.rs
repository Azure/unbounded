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

use unbounded_storage::backend::{BackendRegistry, FixedRegion, OriginRing};
use unbounded_storage::bufferpool::BlockStore;
use unbounded_storage::config::{self, BackendSpec, RuntimePeer};
use unbounded_storage::fabric::{self, ConnectionSpec, Fabric, PeerId, Provider, RpcServerHandle};
use unbounded_storage::memory::{BackingKind, BackingRequest, HUGEPAGE_2MB, allocate};
use unbounded_storage::p2p::{RecursiveHandler, RouteTableHandle, RouteTableSnapshot};
use unbounded_storage::runtime::{Threading, WorkerIdx};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::storage::disks::{CacheDirectorySet, ChainLocalStore};
use unbounded_storage::topology::{NicWorkerGroup, ServingShard};

use crate::FabricStartup;

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
    /// Worker the fabric's progress and RPC threads pin to.
    pub worker_idx: WorkerIdx,
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
/// - verbs: one endpoint per distinct HCA device, pinned to the first
///   worker of the matching nic-worker group and shared by every shard
///   assigned that device.
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
        worker_idx: WorkerIdx(0),
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
            let (worker_idx, numa) =
                verbs_worker_for_device(&device, serving_count, nic_workers, hca_dev_names);
            units.push(FabricUnitSpec {
                unit_idx: idx,
                device_name: device.clone(),
                provider: Provider::from_device_name(&device),
                worker_idx,
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

/// Worker index (and NUMA) a verbs endpoint on `device` pins to: the
/// first worker of the nic-worker group whose HCA matches `device`. The
/// runtime lays serving shards out at `0..serving_count` and the
/// nic-worker groups after them, flattened in order, so a group's base
/// is `serving_count` plus the worker counts of all earlier groups.
fn verbs_worker_for_device(
    device: &str,
    serving_count: usize,
    nic_workers: &[NicWorkerGroup],
    hca_dev_names: &[String],
) -> (WorkerIdx, Option<u16>) {
    let mut flat_base = 0usize;

    for group in nic_workers {
        let matches = hca_dev_names
            .get(group.hca)
            .is_some_and(|name| name == device);
        if matches && !group.workers.is_empty() {
            return (WorkerIdx((serving_count + flat_base) as u16), group.numa);
        }
        flat_base += group.workers.len();
    }

    // No matching nic-worker group. Unreachable when `shard_devices`
    // came from `assign_shard_devices` over these same `nic_workers`;
    // fall back to worker 0 so the unit still pins somewhere valid.
    (WorkerIdx(0), None)
}

/// A live fabric endpoint plus its shared RPC server and the
/// reconcile-state for the shards mapped onto it.
///
/// Field order is drop order and mirrors the pre-lift per-shard teardown
/// exactly: `rpc_server` drops first (it signals shutdown and joins its
/// worker pool, releasing the `RecursiveHandler` it owns, which frees
/// the scratch backing), then the registry and routing handle, then
/// `fabric` last (closing every memory region registered against the
/// domain: the shared scratch plus every assigned shard's data backing).
/// Closing the domain while a worker still touches it would be unsound,
/// hence the order.
///
/// The four teardown-ordered resources are type parameters defaulted to
/// the production types, so the rest of the module refers to this as the
/// plain `FabricUnit`. Bringing a real unit up needs a live libfabric
/// domain, pinned backings, and RPC worker threads (what the smoke test
/// covers), so the parameters exist solely so a unit test can substitute
/// drop-logging stand-ins and lock this ordering hardware-free; see
/// `fabric_unit_drops_resources_in_teardown_order`.
struct FabricUnit<S = RpcServerHandle, Rg = BackendRegistry, Rt = RouteTableHandle, F = Arc<Fabric>>
{
    // Held only for its Drop: see the struct doc for the teardown order.
    #[allow(dead_code)]
    rpc_server: S,
    handler_registry: Rg,
    routes: Rt,
    fabric: F,
    device_name: String,
    rdma: bool,
    self_addr: String,
    hca_numa: Option<u16>,
    applied_peers: HashMap<PeerId, ConnectionSpec>,
    last_backends: HashMap<String, BackendSpec>,
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
        backend_specs: &[BackendSpec],
        cache_directories: Arc<CacheDirectorySet>,
        routes: &RouteTableSnapshot,
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
                backend_specs,
                &cache_directories,
                routes,
                peers,
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

        Ok(Self {
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

    /// Reload every endpoint's RPC-handler routing from a new snapshot.
    /// Driven in lockstep with the per-shard transport reload so the
    /// classify and forward paths move together.
    pub fn reload_routes(&self, snapshot: &RouteTableSnapshot) {
        for unit in &self.units {
            unit.routes.store_snapshot(snapshot.clone());
        }
    }

    /// Re-drive every endpoint's fabric connection table toward `peers`.
    /// Connections live at the fabric/address-vector level, so this runs
    /// once per endpoint rather than once per shard.
    pub fn reconcile_peers(&mut self, peers: &[RuntimePeer]) {
        for (unit_idx, unit) in self.units.iter_mut().enumerate() {
            let desired = runtime_peer_connections_for_unit(peers, unit_idx);
            let report = config::reconcile::reconcile_connections(
                &unit.fabric,
                &desired,
                Some(&unit.applied_peers),
            );
            unit.applied_peers = report.applied;
            for (peer, err) in &report.failures {
                eprintln!("fabric peer reconcile failed: peer={} err={err}", peer.0);
            }
            // Publish the full desired set so the background reconnect
            // thread keeps retrying peers whose dial lost the startup
            // race, and so peers dropped from config (that never
            // connected, hence were never removed by reconcile) are
            // pruned from the desired set.
            unit.fabric.set_desired_peers(desired);
        }
    }

    /// Re-drive every endpoint's RPC-side backend registry toward
    /// `backends`. The shard-side transport registries are reconciled
    /// separately in each shard's control drain.
    pub fn reconcile_backends(&mut self, backends: &[BackendSpec]) {
        for unit in &mut self.units {
            let report = config::reconcile_backends(
                &unit.handler_registry,
                backends,
                Some(&unit.last_backends),
            );
            unit.last_backends = report.applied;
            for (id, err) in &report.failures {
                eprintln!("fabric backend reconcile failed: id={id} err={err}");
            }
        }
    }
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
    backend_specs: &[BackendSpec],
    cache_directories: &Arc<CacheDirectorySet>,
    routes: &RouteTableSnapshot,
    peers: &[RuntimePeer],
    self_peer: PeerId,
) -> Result<FabricUnit, String> {
    let worker = spec.worker_idx.0;

    let mut cfg = fabric::defaults_for(spec.device_name.clone(), runtime.clone(), spec.worker_idx);
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
    let scratch_base = scratch.base;
    let scratch_mr = fabric
        .register_backing(&scratch, spec.numa)
        .map_err(|e| format!("worker={worker}: register rpc scratch: {e}"))?;

    let routes = RouteTableHandle::from_snapshot(routes.clone());

    // The handler resolves disk reads into scratch pages, so it needs a
    // `BlockStore` whose single registered backing IS the scratch
    // region; it shares the same disk-channel directory as the shards'
    // data-path stores but keeps page resolution unambiguous.
    let rpc_store = ChainLocalStore::new(cache_directories.clone());
    rpc_store
        .register_pages(&scratch)
        .map_err(|e| format!("worker={worker}: rpc scratch blockstore register: {e}"))?;

    // The handler runs on a persistent RPC worker thread, so its origin
    // backend must use a worker-local ring (not a shard ring another
    // thread progresses) and memcpy origin bytes into the scratch
    // region.
    let handler_registry = BackendRegistry::new(
        backend_specs,
        OriginRing::WorkerLocal {
            queue_depth: 256,
            region: Some(FixedRegion {
                base: scratch_base,
                len: page_size * RPC_SCRATCH_PAGES as usize,
            }),
        },
        page_size,
        scratch_base,
    )
    .map_err(|e| format!("worker={worker}: build rpc backend registry: {e}"))?;

    // `scratch` moves into the handler here; `scratch_mr` is a `Copy`
    // value handle shared by copy between the handler's forwarding
    // transport and the RPC server's `local_mr`.
    let rpc_handler = Arc::new(
        RecursiveHandler::with_routes(
            rpc_store,
            scratch,
            RPC_SCRATCH_PAGES,
            routes.clone(),
            fabric.clone(),
            scratch_mr,
            page_size,
            handler_registry.clone(),
        )
        .map_err(|e| format!("worker={worker}: RecursiveHandler::with_routes: {e}"))?,
    );
    let rpc_server = fabric
        .start_rpc_server::<StripeReq, _>(rpc_handler, Some(scratch_mr), page_size)
        .map_err(|e| format!("worker={worker}: start_rpc_server: {e}"))?;

    fabric.check_shared_domain_capacity(spec.expected_mr);

    let desired_peers = runtime_peer_connections_for_unit(peers, spec.unit_idx);
    let applied_peers =
        config::reconcile::reconcile_connections(&fabric, &desired_peers, None).applied;
    // Seed the desired-peer set so the background reconnect thread can
    // retry any peer whose initial dial lost the startup race.
    fabric.set_desired_peers(desired_peers.clone());
    let last_backends = backend_specs
        .iter()
        .map(|b| (b.name.clone(), b.clone()))
        .collect();

    Ok(FabricUnit {
        rpc_server,
        handler_registry,
        routes,
        fabric,
        device_name: spec.device_name.clone(),
        rdma: spec.provider == Provider::Verbs,
        self_addr,
        hca_numa: spec.numa,
        applied_peers,
        last_backends,
    })
}

fn runtime_peer_connections(peers: &[RuntimePeer]) -> Vec<ConnectionSpec> {
    runtime_peer_connections_for_unit(peers, 0)
}

fn runtime_peer_connections_for_unit(
    peers: &[RuntimePeer],
    unit_idx: usize,
) -> Vec<ConnectionSpec> {
    peers
        .iter()
        .map(|peer| ConnectionSpec {
            peer: peer.fabric_peer_id,
            address: runtime_peer_address_for_unit(&peer.spec, unit_idx),
            hca_numa: None,
            tags: peer.spec.tags.clone(),
        })
        .collect()
}

fn runtime_peer_address_for_unit(
    peer: &config::PeerSpec,
    unit_idx: usize,
) -> fabric::FabricAddress {
    match peer.config.as_ref() {
        Some(config::peer_spec::Config::Tcp(cfg)) => {
            fabric::FabricAddress::socket(cfg.addr.clone())
        }
        Some(config::peer_spec::Config::Rdma(cfg)) => {
            rdma_fabric_address(cfg.addrs.get(unit_idx).unwrap_or(&cfg.addr))
        }
        None => fabric::FabricAddress::native(""),
    }
}

fn rdma_fabric_address(addr: &str) -> fabric::FabricAddress {
    if addr.parse::<SocketAddr>().is_ok() {
        fabric::FabricAddress::socket(addr.to_string())
    } else {
        fabric::FabricAddress::native(addr.to_string())
    }
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::rc::Rc;

    use super::*;

    use unbounded_storage::config::{PeerSpec, RdmaPeerConfig, peer_spec};
    use unbounded_storage::p2p::node_id_from_name;

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

    fn runtime_peer(id: u64, addr: &str) -> RuntimePeer {
        let name = format!("node-{id}");
        let node_id = node_id_from_name(&name);
        RuntimePeer {
            name: name.clone(),
            node_id,
            fabric_peer_id: PeerId(node_id.0),
            spec: PeerSpec {
                name,
                tags: Vec::new(),
                config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                    addr: addr.to_string(),
                    addrs: Vec::new(),
                })),
            },
        }
    }

    fn runtime_peer_with_addrs(id: u64, addr: &str, addrs: Vec<&str>) -> RuntimePeer {
        let name = format!("node-{id}");
        let node_id = node_id_from_name(&name);
        RuntimePeer {
            name: name.clone(),
            node_id,
            fabric_peer_id: PeerId(node_id.0),
            spec: PeerSpec {
                name,
                tags: Vec::new(),
                config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                    addr: addr.to_string(),
                    addrs: addrs.into_iter().map(str::to_string).collect(),
                })),
            },
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
        assert_eq!(unit.worker_idx, WorkerIdx(0));
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
        assert_eq!(u0.worker_idx, WorkerIdx(4)); // serving_count(4) + base(0)
        assert_eq!(u0.numa, Some(0));
        assert_eq!(u0.shards_assigned, vec![0, 2]);
        assert_eq!(u0.expected_mr, 3); // 2 shards + 1 scratch

        let u1 = &plan.units[1];
        assert_eq!(u1.device_name, "mlx5_1");
        assert_eq!(u1.provider, Provider::Verbs);
        assert_eq!(u1.worker_idx, WorkerIdx(6)); // serving_count(4) + base(2)
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
        assert_eq!(plan.units[0].worker_idx, WorkerIdx(2)); // serving_count(2) + base(0)
        assert_eq!(plan.units[0].shards_assigned, vec![0, 1]);
        assert_eq!(plan.units[0].expected_mr, 3); // 2 shards + 1 scratch
    }

    #[test]
    fn rdma_peer_connections_are_unscoped_by_numa() {
        let peers = [runtime_peer(1, "hex:00"), runtime_peer(2, "hex:ff")];

        let connections = runtime_peer_connections(&peers);

        assert_eq!(connections.len(), 2);
        assert_eq!(
            connections[0].address,
            fabric::FabricAddress::native("hex:00")
        );
        assert_eq!(connections[0].hca_numa, None);
        assert_eq!(
            connections[1].address,
            fabric::FabricAddress::native("hex:ff")
        );
        assert_eq!(connections[1].hca_numa, None);
    }

    #[test]
    fn rdma_socket_peer_connections_use_socket_addresses() {
        let peers = [runtime_peer(1, "10.0.0.1:9000")];

        let connections = runtime_peer_connections(&peers);

        assert_eq!(connections.len(), 1);
        assert_eq!(
            connections[0].address,
            fabric::FabricAddress::socket("10.0.0.1:9000")
        );
        assert_eq!(connections[0].hca_numa, None);
    }

    #[test]
    fn rdma_peer_connections_select_unit_index_address() {
        let peers = [runtime_peer_with_addrs(
            1,
            "hex:fallback",
            vec!["hex:unit0", "hex:unit1"],
        )];

        let unit0 = runtime_peer_connections_for_unit(&peers, 0);
        let unit1 = runtime_peer_connections_for_unit(&peers, 1);
        let unit2 = runtime_peer_connections_for_unit(&peers, 2);

        assert_eq!(unit0[0].address, fabric::FabricAddress::native("hex:unit0"));
        assert_eq!(unit1[0].address, fabric::FabricAddress::native("hex:unit1"));
        assert_eq!(
            unit2[0].address,
            fabric::FabricAddress::native("hex:fallback")
        );
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
        // fabric closes it, with the registry and route handle in
        // between. Bringing a real unit up needs a live libfabric domain
        // (covered by the smoke test), so here we substitute drop-logging
        // tokens for the four hardware-bound resources. This binds to the
        // production struct definition (same `FabricUnit`, fake generic
        // arguments), so reordering any field fails this test.
        let log: Rc<RefCell<Vec<&'static str>>> = Rc::new(RefCell::new(Vec::new()));

        let unit: FabricUnit<DropToken, DropToken, DropToken, DropToken> = FabricUnit {
            rpc_server: DropToken::new("rpc_server", &log),
            handler_registry: DropToken::new("handler_registry", &log),
            routes: DropToken::new("routes", &log),
            fabric: DropToken::new("fabric", &log),
            device_name: "mlx5_0".to_string(),
            rdma: true,
            self_addr: "hex:01".to_string(),
            hca_numa: None,
            applied_peers: HashMap::new(),
            last_backends: HashMap::new(),
        };

        drop(unit);

        assert_eq!(
            *log.borrow(),
            vec!["rpc_server", "handler_registry", "routes", "fabric"],
        );
    }
}
