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
//! [`spawn_shard_layer`] brings a layer up from a config;
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

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;

use unbounded_storage::bufferpool::{PoolGroup, Req};
use unbounded_storage::config::{
    self, ApplyError, Config, ConfigApplyTarget, ConfigDiff, ShardControlGroup,
};
use unbounded_storage::fabric::PeerId;
use unbounded_storage::p2p::RouteTableHandle;
use unbounded_storage::runtime::{JoinHandle, Threading, WorkerIdx};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::storage::disks::{CacheDirectorySet, DiskRegistrySet, UringDiskTarget};
use unbounded_storage::topology::ServingShard;

use crate::StartupSettings;
use crate::fabric_group::{FabricGroup, FabricPlan};

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

/// A spawned set of shard threads plus the handles to drive and retire
/// them. Produced by [`spawn_shard_layer`], consumed by
/// [`teardown_shard_layer`].
pub struct ShardLayer {
    /// Join handles for every shard thread, in spawn order.
    joins: Vec<JoinHandle>,
    /// Owns every shared fabric endpoint (and its RPC server) the shards
    /// run on. Drives in-place peer and RPC-side backend reconcile, and
    /// is dropped during teardown after all shard threads have joined but
    /// before their backings are freed.
    fabric_group: FabricGroup,
    /// Blocking fan-out/fan-in over every shard's control channel.
    control: ShardControlGroup,
    /// Process-wide-routing dispatcher kept alive for the layer's
    /// lifetime (observability today; the seam cross-shard routing will
    /// use). `None` only for the degenerate zero-shard layer.
    _pool_group: Option<PoolGroup<StripeReq>>,
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
}

/// Bring up a shard layer from `config` on the runtime in `deps`,
/// blocking until every shard has reported readiness.
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
pub fn spawn_shard_layer(
    config: &Config,
    deps: &ShardSpawnDeps,
) -> Result<ShardLayer, Vec<String>> {
    let layer_stop = Arc::new(AtomicBool::new(false));
    let worker_count = deps.workers.len();
    let settings = &deps.settings;
    // `memory_total_bytes` is the whole host backing budget; split it
    // evenly across the serving shards so each gets a NUMA-local slice
    // and the host footprint stays fixed regardless of the auto-scaled
    // serving-shard count.
    let bytes_per_shard = if worker_count == 0 {
        0
    } else {
        settings.memory_total_bytes / worker_count
    };
    let projection = config::runtime_projection(config)
        .map_err(|e| vec![format!("config projection failed: {e}")])?;
    let frontend_specs = Arc::new(config.frontends.clone());
    let frontend_bindings = Arc::new(projection.frontends.clone());
    let backend_specs = Arc::new(config.backends.clone());
    let routes = crate::build_routes(config);
    let runtime_peers = config::runtime_peers(&projection);
    let self_peer = local_self_peer(&projection);

    // Bring up the shared fabric endpoints before spawning any shards:
    // each shard registers its data backing against the endpoint it maps
    // onto, so the fabrics (and their RPC servers) must exist first. On
    // failure nothing has spawned yet, so there is nothing to retire.
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
    let mut joins = Vec::with_capacity(worker_count);
    let mut control_senders = Vec::with_capacity(worker_count);
    // Per-shard senders for broadcasting the assembled peer set in phase
    // B. Dropping these unblocks any shard parked on `peer_rx.recv()`
    // when bring-up fails before the broadcast.
    let mut peer_txs: Vec<mpsc::Sender<Arc<Vec<crate::PeerPublish>>>> =
        Vec::with_capacity(worker_count);

    for (i, (shard, _)) in deps.workers.iter().enumerate() {
        let widx = WorkerIdx(u16::try_from(i).expect("worker index fits in u16"));
        let (ctrl_tx, ctrl_rx) = mpsc::channel::<config::ShardCommand>();
        control_senders.push((widx, ctrl_tx));
        let (peer_tx, peer_rx) = mpsc::channel::<Arc<Vec<crate::PeerPublish>>>();
        peer_txs.push(peer_tx);
        let phaseb_tx = phaseb_tx.clone();

        let shard = *shard;
        let fabric = fabric_group.fabric_for_shard(i);
        let tx = ready_tx.clone();
        let backing_kind = settings.backing_kind;
        let cache_directories = deps.cache_directories.clone();
        let route_handle = RouteTableHandle::from_snapshot(routes.clone());
        let frontend_specs = frontend_specs.clone();
        let frontend_bindings = frontend_bindings.clone();
        let backend_specs = backend_specs.clone();
        let layer_stop = layer_stop.clone();
        let rt = deps.runtime.clone();
        let panic_tx = tx.clone();
        let handle = rt.spawn_pinned(
            widx,
            &format!("ub-storage-shard-{i}"),
            Box::new(move || {
                crate::report_on_panic(panic_tx, widx, move || {
                    crate::run_shard(
                        widx,
                        shard,
                        fabric,
                        tx,
                        backing_kind,
                        bytes_per_shard,
                        cache_directories,
                        route_handle,
                        frontend_specs,
                        frontend_bindings,
                        backend_specs,
                        ctrl_rx,
                        peer_rx,
                        phaseb_tx,
                        layer_stop,
                    );
                });
            }),
        );
        joins.push(handle);
    }
    drop(ready_tx);
    // The layer keeps no phase-B sender of its own: collection below is
    // bounded by the number of shards that came up, and each live shard
    // holds its sender, so this never closes the channel prematurely.
    drop(phaseb_tx);

    // Bounded readiness collection: read exactly one message per spawned
    // thread. Shards that come up park holding their sender, so they
    // never close the channel; only a panic-before-report or a clean
    // failure surfaces here.
    let mut descriptors = Vec::new();
    let mut publishes = Vec::new();
    let mut errors = Vec::new();
    for _ in 0..joins.len() {
        match ready_rx.recv() {
            Ok(crate::ShardReady::Up {
                descriptor,
                publish,
            }) => {
                publishes.push((descriptor.worker_idx, publish));
                descriptors.push(descriptor);
            }
            Ok(crate::ShardReady::Failed(err)) => {
                eprintln!("shard failed: {err}");
                errors.push(err);
            }
            Err(_) => {
                errors.push("shard thread exited without reporting readiness".to_string());
            }
        }
    }

    if !errors.is_empty() {
        // Retire any shards that did come up so a partially-built layer
        // never leaks threads, then surface the failures to the caller.
        // Dropping the peer senders unblocks any up-shard parked on
        // `peer_rx.recv()` so it can exit and be joined.
        drop(peer_txs);
        layer_stop.store(true, Ordering::Relaxed);
        for h in joins.into_iter().rev() {
            let _ = h.join();
        }
        return Err(errors);
    }

    // Assemble the broadcast peer set: sort the up-shards by worker index
    // so `shard_index` (the position here) matches the `PoolGroup`
    // ordering and `stripe_key_to_shard`, then hand every shard the full
    // list. Each shard locates its own entry by worker index, registers
    // the others' backings, and reports phase-B readiness below.
    publishes.sort_by_key(|(widx, _)| widx.0);
    // Retain every shard's backing Drop carrier for the layer's whole
    // life. Split out here as the `ShardPublish` values are consumed
    // into the broadcast `PeerPublish` set (which deliberately carries
    // only base/len, not ownership).
    let mut backing_keepalives: Vec<Arc<dyn Send + Sync>> = Vec::with_capacity(publishes.len());
    let peer_list: Arc<Vec<crate::PeerPublish>> = Arc::new(
        publishes
            .into_iter()
            .enumerate()
            .map(|(shard_index, (worker_idx, publish))| {
                backing_keepalives.push(publish.backing_keepalive);
                crate::PeerPublish {
                    shard_index,
                    worker_idx,
                    backing_base: publish.backing_base,
                    backing_len: publish.backing_len,
                    channel: publish.fetch_channel,
                    numa: publish.numa,
                }
            })
            .collect(),
    );
    let up_count = peer_list.len();
    for tx in &peer_txs {
        // A send error means that shard died after phase A; it will be
        // surfaced as a missing/closed phase-B report below.
        let _ = tx.send(peer_list.clone());
    }

    // Phase-B collection: every up-shard reports exactly once (the
    // `PhaseBGuard` guarantees this even on early return or panic), so
    // this is bounded by `up_count`.
    let mut phaseb_errors = Vec::new();
    for _ in 0..up_count {
        match phaseb_rx.recv() {
            Ok(crate::PhaseBReport::Ready(_)) => {}
            Ok(crate::PhaseBReport::Failed(err)) => {
                eprintln!("shard phase-B failed: {err}");
                phaseb_errors.push(err);
            }
            Err(_) => {
                phaseb_errors
                    .push("shard thread exited without reporting phase-B readiness".to_string());
                break;
            }
        }
    }
    if !phaseb_errors.is_empty() {
        layer_stop.store(true, Ordering::Relaxed);
        for h in joins.into_iter().rev() {
            let _ = h.join();
        }
        return Err(phaseb_errors);
    }

    descriptors.sort_by_key(|d| d.worker_idx.0);
    let shard_count = descriptors.len();
    let pool_group = if descriptors.is_empty() {
        None
    } else {
        let group: PoolGroup<StripeReq> = PoolGroup::new(descriptors, move |req: &StripeReq| {
            crate::stripe_key_to_shard(&req.key(), shard_count)
        });
        eprintln!("pool group up: shards={shard_count}");
        Some(group)
    };

    Ok(ShardLayer {
        joins,
        fabric_group,
        control: ShardControlGroup::new(control_senders),
        _pool_group: pool_group,
        layer_stop,
        _backing_keepalives: backing_keepalives,
    })
}

fn local_self_peer(projection: &config::RuntimeGraph) -> PeerId {
    let mut local_ids: Vec<(&str, u64)> = projection
        .neighborhoods
        .values()
        .filter_map(|n| n.p2p.local_node_id.map(|id| (n.id.as_str(), id)))
        .collect();
    local_ids.sort_by_key(|(neighborhood_id, _)| *neighborhood_id);
    local_ids
        .first()
        .map(|(neighborhood_id, node_id)| config::scoped_peer_id(neighborhood_id, *node_id))
        .unwrap_or(PeerId(0))
}

/// Retire a shard layer: signal its shards to exit, then join every
/// thread in reverse spawn order so teardown mirrors bring-up.
pub fn teardown_shard_layer(layer: ShardLayer) {
    let ShardLayer {
        joins,
        fabric_group,
        control,
        _pool_group,
        layer_stop,
        _backing_keepalives,
    } = layer;
    layer_stop.store(true, Ordering::Relaxed);
    for h in joins.into_iter().rev() {
        if let Err(e) = h.join() {
            eprintln!("shard thread panicked during teardown: {e:?}");
        }
    }
    // Drop the control senders only after every shard thread has
    // exited, so nothing observes a half-torn layer.
    drop(control);
    drop(_pool_group);
    // Now retire the shared fabric endpoints. Each unit drops its RPC
    // server first (signalling shutdown and joining the RPC worker
    // pool), then its `Fabric`, which closes every memory region
    // registered against the domain: the shared RPC scratch and every
    // assigned shard's data backing. The shard threads have already
    // joined, so nothing still touches those regions.
    drop(fabric_group);
    // Free every shard's backing only now: the fabrics above have closed
    // their MRs, and all shard threads (and thus every io_uring ring that
    // may still reference a peer's pages as a `SEND_ZC` source) have
    // joined, so no mapping is unmapped out from under a live ring or an
    // open MR.
    drop(_backing_keepalives);
}

/// The binary's [`ConfigApplyTarget`]: owns the live shard layer and
/// the disk registry, and realizes config applies in place against
/// them.
pub struct ProcessApplyTarget {
    /// `Option` so the layer can be moved out at shutdown via
    /// [`Self::into_parts`]. Always `Some` while the process is serving.
    layer: Option<ShardLayer>,
    disk_registry: DiskRegistrySet<UringDiskTarget>,
    cache_directories: Arc<CacheDirectorySet>,
}

impl ProcessApplyTarget {
    pub fn new(
        layer: ShardLayer,
        disk_registry: DiskRegistrySet<UringDiskTarget>,
        cache_directories: Arc<CacheDirectorySet>,
    ) -> Self {
        Self {
            layer: Some(layer),
            disk_registry,
            cache_directories,
        }
    }

    /// Reconcile projected cache disks in place and republish the
    /// resulting channel set to the live directory (idempotent when the
    /// disk set is unchanged).
    fn reconcile_disks(&mut self, projection: &config::RuntimeGraph) {
        crate::reconcile_cache_disks(&mut self.disk_registry, &self.cache_directories, projection);
    }

    /// Consume the target at shutdown, returning the live layer (if any)
    /// and the disk registry so the caller can tear them down in the
    /// correct order (shards first, then disks).
    pub fn into_parts(self) -> (Option<ShardLayer>, DiskRegistrySet<UringDiskTarget>) {
        (self.layer, self.disk_registry)
    }
}

impl ConfigApplyTarget for ProcessApplyTarget {
    fn apply_in_place(&mut self, new: &Arc<Config>, diff: &ConfigDiff) -> Result<(), ApplyError> {
        let projection = config::runtime_projection(new)
            .map_err(|e| ApplyError::Target(format!("config projection failed: {e}")))?;

        // The shards must see a new config whenever their routing surface,
        // graph projection, or per-shard backend/frontend registries need
        // to change. Pure projected-disk changes are absorbed by
        // `reconcile_disks` below, but cache graph changes can also alter
        // frontend backend/bypass resolution, so they are broadcast.
        let needs_broadcast = diff.requires_routing_reload()
            || diff.caches_changed
            || diff.backends_changed
            || diff.frontends_changed;

        if needs_broadcast {
            let routes = crate::build_routes(new);
            let layer = self
                .layer
                .as_mut()
                .expect("shard layer present between applies");

            // Peer connections and the RPC handlers' routing live on the
            // shared fabric endpoints, not on individual shards, so they
            // are reconciled once per endpoint here. Both only need
            // touching when the routing surface (p2p/peers) changed; a
            // backend/frontend-only change leaves them untouched.
            if diff.requires_routing_reload() {
                layer.fabric_group.reload_routes(&routes);
                let runtime_peers = config::runtime_peers(&projection);
                layer.fabric_group.reconcile_peers(&runtime_peers);
            }

            // The RPC-side backend registries also live on the shared
            // endpoints; reconcile them before broadcasting so the shards
            // rebuild their own transport registries against an already
            // up-to-date origin surface.
            if diff.backends_changed {
                layer.fabric_group.reconcile_backends(&new.backends);
            }

            // Republish config + routing to every shard and block until
            // each has acked, so the apply (routing surface and the
            // per-shard backend/frontend reconcile each shard performs on
            // receipt) has provably landed everywhere before we return.
            layer.control.broadcast_apply(new.clone(), routes)?;
        }

        if diff.caches_changed {
            self.reconcile_disks(&projection);
        }

        Ok(())
    }
}
