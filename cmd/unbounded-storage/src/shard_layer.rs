// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Lifecycle of the process's shard layer and the binary's
//! [`ConfigApplyTarget`] implementation.
//!
//! A *shard layer* is the full set of per-shard threads spawned from a
//! single [`Config`], plus the handles needed to drive and tear them
//! down: the readiness-derived fabrics (for live peer reconcile), the
//! [`ShardControlGroup`] used to fan config applies out to every shard,
//! and a shared `layer_stop` flag that retires just this layer's shards
//! without touching the process-wide shutdown signal.
//!
//! [`spawn_shard_layer`] brings a layer up from a config;
//! [`teardown_shard_layer`] drains and joins it (used at process
//! shutdown). [`ProcessApplyTarget`] realizes a live config apply
//! entirely in place via [`ProcessApplyTarget::apply_in_place`]:
//! routing is republished to every shard (blocking until each acks via
//! the control group), each shard reconciles its own backend/frontend
//! registries from the broadcast config, and disks are reconciled in
//! place against the shared channel directory. No shard restart is ever
//! required for a config change.
//!
//! [`ConfigController`]: crate::config::ConfigController
//! [`ConfigApplyTarget`]: crate::config::ConfigApplyTarget

use std::collections::HashMap;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;

use unbounded_storage::bufferpool::{PoolGroup, Req};
use unbounded_storage::config::{
    self, ApplyError, Config, ConfigApplyTarget, ConfigDiff, ShardControlGroup,
};
use unbounded_storage::fabric::{ConnectionSpec, Fabric, PeerId};
use unbounded_storage::p2p::RoutingSnapshot;
use unbounded_storage::runtime::{JoinHandle, Threading, WorkerIdx};
use unbounded_storage::storage::StripeReq;
use unbounded_storage::storage::disks::{DiskChannelDirectory, DiskRegistry, UringDiskTarget};
use unbounded_storage::topology::Worker;

use crate::StartupSettings;

/// Inputs that are constant across the life of the process and used to
/// spawn the shard layer. Cloned cheaply into every shard thread.
pub struct ShardSpawnDeps {
    /// Pinned runtime the shards are spawned on.
    pub runtime: Arc<dyn Threading>,
    /// One entry per shard worker: the `Worker` plan slot and the HCA
    /// device name bound to it (`None` for the TCP-fallback path).
    pub workers: Vec<(Worker, Option<String>)>,
    /// Startup-fixed settings (fabric endpoint/thread knobs, backing
    /// allocator kind, total pool size) sourced from CLI flags / env
    /// vars. Shared and not reloadable.
    pub settings: Arc<StartupSettings>,
    /// Live disk-channel directory every shard reads through. Shared and
    /// reconciled in place, never rebuilt per layer.
    pub disk_channels: Arc<DiskChannelDirectory>,
}

/// A spawned set of shard threads plus the handles to drive and retire
/// them. Produced by [`spawn_shard_layer`], consumed by
/// [`teardown_shard_layer`].
pub struct ShardLayer {
    /// Join handles for every shard thread, in spawn order.
    joins: Vec<JoinHandle>,
    /// Per-shard fabric plus the last-applied peer connection set, used
    /// to drive in-place peer reconcile on the Tier 1 path.
    shard_state: Vec<(WorkerIdx, Arc<Fabric>, HashMap<PeerId, ConnectionSpec>)>,
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
    let bytes_per_shard = if worker_count == 0 {
        0
    } else {
        settings.bytes_per_shard / worker_count
    };
    let fabric_startup = Arc::new(settings.fabric.clone());
    let max_inflight = u64::from(settings.fabric.max_inflight);
    let frontend_specs = Arc::new(config.frontends.clone());
    let backend_specs = Arc::new(config.backends.clone());
    let (fingers, node_to_peer) = crate::build_routing(config);

    let (ready_tx, ready_rx) = mpsc::channel::<crate::ShardReady>();
    let mut joins = Vec::with_capacity(worker_count);
    let mut control_senders = Vec::with_capacity(worker_count);

    for (i, (worker, dev_name)) in deps.workers.iter().enumerate() {
        let widx = WorkerIdx(u16::try_from(i).expect("worker index fits in u16"));
        let (ctrl_tx, ctrl_rx) = mpsc::channel::<config::ShardCommand>();
        control_senders.push((widx, ctrl_tx));

        let worker = worker.clone();
        let dev_name = dev_name.clone();
        let runtime = deps.runtime.clone();
        let tx = ready_tx.clone();
        let backing_kind = settings.backing_kind;
        let fabric_startup = fabric_startup.clone();
        let disk_channels = deps.disk_channels.clone();
        let fingers = fingers.clone();
        let node_to_peer = node_to_peer.clone();
        let frontend_specs = frontend_specs.clone();
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
                        worker,
                        dev_name,
                        runtime,
                        tx,
                        backing_kind,
                        bytes_per_shard,
                        fabric_startup,
                        max_inflight,
                        disk_channels,
                        fingers,
                        node_to_peer,
                        frontend_specs,
                        backend_specs,
                        ctrl_rx,
                        layer_stop,
                    );
                });
            }),
        );
        joins.push(handle);
    }
    drop(ready_tx);

    // Bounded readiness collection: read exactly one message per spawned
    // thread. Shards that come up park holding their sender, so they
    // never close the channel; only a panic-before-report or a clean
    // failure surfaces here.
    let mut descriptors = Vec::new();
    let mut shard_fabrics = Vec::new();
    let mut errors = Vec::new();
    for _ in 0..joins.len() {
        match ready_rx.recv() {
            Ok(crate::ShardReady::Up { descriptor, fabric }) => {
                shard_fabrics.push((descriptor.worker_idx, fabric));
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
        layer_stop.store(true, Ordering::Relaxed);
        for h in joins.into_iter().rev() {
            let _ = h.join();
        }
        return Err(errors);
    }

    let mut shard_state = Vec::with_capacity(shard_fabrics.len());
    let mut total_added = 0;
    let mut total_failures = 0;
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
        shard_state,
        control: ShardControlGroup::new(control_senders),
        _pool_group: pool_group,
        layer_stop,
    })
}

/// Retire a shard layer: signal its shards to exit, then join every
/// thread in reverse spawn order so teardown mirrors bring-up.
pub fn teardown_shard_layer(layer: ShardLayer) {
    let ShardLayer {
        joins,
        shard_state,
        control,
        _pool_group,
        layer_stop,
    } = layer;
    layer_stop.store(true, Ordering::Relaxed);
    for h in joins.into_iter().rev() {
        if let Err(e) = h.join() {
            eprintln!("shard thread panicked during teardown: {e:?}");
        }
    }
    // Drop the control senders and per-shard fabrics only after every
    // shard thread has exited, so nothing observes a half-torn layer.
    drop(control);
    drop(shard_state);
    drop(_pool_group);
}

/// The binary's [`ConfigApplyTarget`]: owns the live shard layer and
/// the disk registry, and realizes config applies in place against
/// them.
pub struct ProcessApplyTarget {
    /// `Option` so the layer can be moved out at shutdown via
    /// [`Self::into_parts`]. Always `Some` while the process is serving.
    layer: Option<ShardLayer>,
    disk_registry: DiskRegistry<UringDiskTarget>,
    disk_channels: Arc<DiskChannelDirectory>,
}

impl ProcessApplyTarget {
    pub fn new(
        layer: ShardLayer,
        disk_registry: DiskRegistry<UringDiskTarget>,
        disk_channels: Arc<DiskChannelDirectory>,
    ) -> Self {
        Self {
            layer: Some(layer),
            disk_registry,
            disk_channels,
        }
    }

    /// Reconcile disks in place against `config` and republish the
    /// resulting channel set to the live directory (idempotent when the
    /// disk set is unchanged).
    fn reconcile_disks(&mut self, config: &Config) {
        let report = self.disk_registry.reconcile(&config.disks);
        eprintln!(
            "config: disks: added={} removed={} failures={}",
            report.added,
            report.removed,
            report.failures.len(),
        );
        for (path, msg) in &report.failures {
            eprintln!("disk {}: open failed: {msg}", path.display());
        }
        self.disk_channels
            .apply_channels(self.disk_registry.channels_snapshot());
    }

    /// Consume the target at shutdown, returning the live layer (if any)
    /// and the disk registry so the caller can tear them down in the
    /// correct order (shards first, then disks).
    pub fn into_parts(self) -> (Option<ShardLayer>, DiskRegistry<UringDiskTarget>) {
        (self.layer, self.disk_registry)
    }
}

impl ConfigApplyTarget for ProcessApplyTarget {
    fn apply_in_place(&mut self, new: &Arc<Config>, diff: &ConfigDiff) -> Result<(), ApplyError> {
        // The shards must see a new config whenever their routing surface
        // or their per-shard backend/frontend registries need to change.
        // A disks-only change is absorbed entirely in `reconcile_disks`
        // below (shared channel directory), so it needs no broadcast.
        let needs_broadcast =
            diff.requires_routing_reload() || diff.backends_changed || diff.frontends_changed;

        if needs_broadcast {
            let (fingers, node_to_peer) = crate::build_routing(new);
            let layer = self
                .layer
                .as_mut()
                .expect("shard layer present between applies");

            // Fabric connections only need reconciling when the routing
            // surface (p2p/peers) changed; a backend/frontend-only change
            // leaves the peer set untouched.
            if diff.requires_routing_reload() {
                let mut total_added = 0;
                let mut total_removed = 0;
                let mut total_updated = 0;
                let mut total_failures = 0;
                for (widx, fabric, last) in layer.shard_state.iter_mut() {
                    let report = config::reconcile_peers(fabric, &new.peers, Some(&*last));
                    total_added += report.added;
                    total_removed += report.removed;
                    total_updated += report.updated;
                    total_failures += report.failures.len();
                    for (peer_id, msg) in &report.failures {
                        eprintln!(
                            "shard {}: peer {} failed to apply: {msg}",
                            widx.0, peer_id.0
                        );
                    }
                    *last = report.applied;
                }
                eprintln!(
                    "config: peers reconciled: added={total_added} removed={total_removed} updated={total_updated} failures={total_failures}",
                );
            }

            // Republish config + routing to every shard and block until
            // each has acked, so the apply (routing surface and the
            // per-shard backend/frontend reconcile each shard performs on
            // receipt) has provably landed everywhere before we return.
            let snapshot = RoutingSnapshot {
                fingers,
                node_to_peer,
            };
            layer.control.broadcast_apply(new.clone(), snapshot)?;
        }

        if diff.disks_changed {
            self.reconcile_disks(new);
        }

        Ok(())
    }
}
