// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::RefCell;
use std::collections::HashMap;
use std::num::NonZeroUsize;
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::thread;
use std::time::Duration;

use clap::Parser;

use unbounded_storage::backend::HttpBackend;
use unbounded_storage::bufferpool::{Pool, PoolConfig, PoolGroup, Req, ShardDescriptor, StripeKey};
use unbounded_storage::config::{self, BackendSpec, Config, FabricCfg, FrontendSpec};
use unbounded_storage::fabric::PeerId;
use unbounded_storage::fabric::{self, ConnectionSpec, Fabric, Provider};
use unbounded_storage::frontend::{HttpDriver, HttpFrontend};
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
use unbounded_storage::topology::{Host, Plan, Role, Worker};

use unbounded_storage::memory::{BackingKind, BackingRequest, allocate};

const DEFAULT_CONFIG_PATH: &str = "/etc/unbounded-storage/config.toml";
const SHUTDOWN_POLL: Duration = Duration::from_millis(100);

/// Object-length cache TTL for the per-shard HTTP frontend driver,
/// in milliseconds. Matches the default the frontend's `from_spec`
/// path uses.
const DEFAULT_META_TTL_MS: u64 = 30_000;

/// Stripe granularity used to build an inert origin backend on shards
/// that have no configured backend. Such a backend is never exercised
/// (no frontend drives reads against the pool), so the value is
/// immaterial; it only has to make the `ShardPool` type check.
const DEFAULT_STRIPE_SIZE_BYTES: u64 = 4 * 1024 * 1024;

/// Process-wide shutdown flag. Set by the signal handler (which
/// is restricted to async-signal-safe operations) and polled by
/// the main thread plus every shard thread.
static SHUTDOWN: AtomicBool = AtomicBool::new(false);

type ShardPool = Pool<RoutedTransport<StripeReq, HttpBackend>, Arc<LiveShardLocalStore>, StripeReq>;

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
        config.storage.bytes_per_shard = unbounded_storage::config::schema::ByteSize(b.get());
    }
    if cli.no_hugepages {
        config.storage.backing_kind = config::BackingKindCfg::Heap;
    }
    let backing_kind = config::backing_kind_from_cfg(config.storage.backing_kind);
    let bytes_per_shard_total = config.storage.bytes_per_shard.bytes();

    let host = Host::discover();
    let plan = Plan::for_host(
        &host,
        &config::topology_cfg_to_plan_config(&config.topology),
    );

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

    // Each shard thread reports either an error or a populated
    // `ShardDescriptor` on this channel. The main thread aggregates
    // descriptors into a `PoolGroup` once every shard has reported.
    let (ready_tx, ready_rx) = mpsc::channel::<ShardReady>();
    let bytes_per_shard = bytes_per_shard_total / progress.len();
    let fabric_cfg = Arc::new(config.fabric.clone());
    // Frontend/backend specs are shared read-only across shards.
    // Each shard reads them to build (frontend) or validate-and-log
    // (backend) its per-shard tier wiring.
    let frontend_specs = Arc::new(config.frontends.clone());
    let backend_specs = Arc::new(config.backends.clone());

    // Build the p2p routing surface once and share it across shards.
    // Every shard's `RoutedTransport` routes through the same finger
    // table and node->peer map.
    let (fingers, node_to_peer) = build_routing(&config);

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
        let disk_channels = disk_channels.clone();
        let fingers = fingers.clone();
        let node_to_peer = node_to_peer.clone();
        let frontend_specs = frontend_specs.clone();
        let backend_specs = backend_specs.clone();
        // `spawn_pinned` spawns the thread and pins it (affinity +
        // NUMA mempolicy) before running `f`. The `!Send` shard types
        // are constructed inside `run_shard`, which runs AFTER pinning
        // on the spawned thread, so only the `FnOnce` crosses the
        // thread boundary.
        let rt = runtime.clone();
        // `run_shard` reports exactly one `ShardReady` on every normal
        // path (its own `tx`), but it has `.expect(...)`/index calls
        // during bring-up that can panic before any report. The
        // collection loop in `main` does a bounded `recv()` that only
        // unblocks once all senders drop, so a silent panic would hang
        // startup forever (the other shards park holding their senders).
        // `report_on_panic` owns a dedicated `tx` clone reserved for the
        // panic path so a panicking shard still emits one `Failed`.
        let panic_tx = tx.clone();
        let handle = rt.spawn_pinned(
            widx,
            &format!("ub-storage-shard-{i}"),
            Box::new(move || {
                report_on_panic(panic_tx, widx, move || {
                    run_shard(
                        widx,
                        worker,
                        dev_name,
                        runtime,
                        tx,
                        backing_kind,
                        bytes_per_shard,
                        fabric_cfg,
                        disk_channels,
                        fingers,
                        node_to_peer,
                        frontend_specs,
                        backend_specs,
                    );
                });
            }),
        );
        joins.push(handle);
    }
    drop(ready_tx);

    // Collect shard readiness messages, separating successes from
    // failures. We wait for every shard so a partial bring-up produces
    // a coherent error path rather than a half-built `PoolGroup`.
    let mut descriptors: Vec<ShardDescriptor> = Vec::with_capacity(joins.len());
    let mut shard_fabrics: Vec<(WorkerIdx, Arc<Fabric>)> = Vec::with_capacity(joins.len());
    let mut errors: Vec<String> = Vec::new();
    // Receive exactly one readiness message per shard. We must NOT
    // iterate the receiver to channel-disconnect here: a successful
    // shard reports `Up` and then parks in `wait_for_shutdown`, holding
    // its `Sender` alive for the lifetime of the process. Draining to
    // disconnect would therefore block forever on the happy path. Each
    // shard sends exactly one message before either parking (`Up`) or
    // returning (`Failed`), so a bounded recv is both sufficient and
    // correct.
    for _ in 0..joins.len() {
        match ready_rx.recv() {
            Ok(ShardReady::Up { descriptor, fabric }) => {
                shard_fabrics.push((descriptor.worker_idx, fabric));
                descriptors.push(descriptor);
            }
            Ok(ShardReady::Failed(err)) => {
                eprintln!("shard failed: {err}");
                errors.push(err);
                SHUTDOWN.store(true, Ordering::Relaxed);
            }
            Err(_) => {
                // A shard thread panicked or dropped its sender without
                // reporting. Treat the missing report as a bring-up
                // failure so we take the coherent error path rather than
                // proceeding with a half-built PoolGroup.
                errors.push("shard thread exited without reporting readiness".into());
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

    // Disk supervisor: reconcile `[[disks]]` entries onto pinned
    // storage cores. Each disk runs on its own storage core hosting the
    // engine and ring, and publishes a `PageChannel` that carries the
    // page data path cross-core from the shards. CPU pin hints now come
    // from the topology plan's disjoint NVMe (`Role::NvmeIoUring`)
    // slots: the registry assigns each disk a NUMA-local slot that is
    // disjoint from the shard cores by construction. If the plan
    // discovered no NVMe devices the slot list is empty and disks run
    // unpinned.
    let disk_slots = plan.disk_cpu_slots();
    let mut disk_registry = DiskRegistry::new(UringDiskTarget::new(runtime.clone()), disk_slots);
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
        for (path, hint) in disk_registry.placement_snapshot() {
            match hint {
                Some(slot) => eprintln!("disk {}: pinned to cpu {}", path.display(), slot.cpu),
                None => eprintln!("disk {}: unpinned (no plan slot available)", path.display()),
            }
        }
        disk_channels.apply_channels(disk_registry.channels_snapshot());
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
        let _group: PoolGroup<StripeReq> = PoolGroup::new(descriptors, move |req: &StripeReq| {
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
                disk_channels.clone(),
            );
        }
        Err(e) => {
            eprintln!("config watch: not installed: {e}");
            wait_for_shutdown();
        }
    }
    eprintln!("shutdown signaled; tearing down shards");

    // Shard threads must exit first so they release any
    // `Arc<StorageEngine>` refs published via the channel directory.
    // Then drop the published snapshot, then drain the disk supervisor
    // so each per-disk thread sees its engine refcount fall before
    // its stop flag.
    // (reverse order so the last-built shard tears down first)
    for h in joins.into_iter().rev() {
        if let Err(e) = h.join() {
            eprintln!("shard thread panicked: {e:?}");
            errors.push(format!("panic: {e:?}"));
        }
    }
    drop(disk_channels);
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
    disk_channels: Arc<DiskChannelDirectory>,
    fingers: Arc<FingerTable>,
    node_to_peer: Arc<HashMap<NodeId, PeerId>>,
    frontend_specs: Arc<Vec<FrontendSpec>>,
    backend_specs: Arc<Vec<BackendSpec>>,
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

    // Select the single frontend this shard serves (v1: one per shard;
    // `select_frontend_spec` logs-and-skips extras). The selection also
    // decides which backend the origin tier fetches from: a selected
    // frontend names its backend and load-time validation guarantees
    // that backend exists. With no frontend we still must construct an
    // `HttpBackend` so the `ShardPool` type checks, so fall back to the
    // first configured backend, or an inert loopback origin when none
    // are configured (no reads are issued against the pool without a
    // frontend, so the backend stays dormant).
    let selected = select_frontend_spec(widx, &frontend_specs);
    let backend_spec = match selected {
        Some(fe) => backend_specs.iter().find(|b| b.id == fe.backend),
        None => backend_specs.first(),
    };
    let (origin, backend_id, stripe_size) = match backend_spec {
        Some(spec) => match HttpBackend::resolve_origin(&spec.endpoint) {
            Ok(o) => (o, spec.id.clone(), spec.stripe_size_bytes),
            Err(e) => {
                let _ = tx.send(ShardReady::Failed(format!(
                    "worker={}: backend {} resolve_origin({}): {e}",
                    widx.0, spec.id, spec.endpoint,
                )));
                return;
            }
        },
        None => {
            let o = HttpBackend::resolve_origin("127.0.0.1:0")
                .expect("loopback:0 resolves to an IPv4 address");
            (o, String::new(), DEFAULT_STRIPE_SIZE_BYTES)
        }
    };

    log_backend_registry(widx, &backend_specs);

    // Route the pool's transport through the p2p finger table: a stripe
    // owned by a peer goes over the fabric RDMA path; a stripe this node
    // owns (Chord `next_hop` returns None) goes to the `HttpBackend`,
    // which fetches the missing byte range from the origin endpoint.
    let backend = HttpBackend::new(
        socket.clone(),
        origin.duplicate(),
        backend_id.clone(),
        stripe_size,
        page_size,
        backing_base,
    );
    let transport = match RoutedTransport::new(
        fabric.clone(),
        mr,
        page_size,
        fingers.clone(),
        node_to_peer.clone(),
        backend,
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
    let rpc_handler = Arc::new(
        match RecursiveHandler::new(
            rpc_store.clone(),
            scratch,
            RPC_SCRATCH_PAGES,
            fingers.clone(),
            node_to_peer.clone(),
            fabric.clone(),
            scratch_mr,
            page_size,
            HttpBackend::new(
                socket.clone(),
                origin.duplicate(),
                backend_id.clone(),
                stripe_size,
                page_size,
                backing_base,
            ),
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
    // configured frontend driver, if any, is registered as a second
    // tick hook.
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

    // Bring up the frontend selected above (if any). The `HttpFrontend`
    // factory validates the spec and binds the shard's `SO_REUSEPORT`
    // listener; the per-shard `HttpDriver` then serves connections out
    // of this shard's pool, fetching origin misses through the same
    // backend wired into the transport above. v1 drives one frontend
    // per shard (see `select_frontend_spec`).
    match selected {
        Some(spec) => {
            let frontend = match HttpFrontend::from_spec(spec) {
                Ok(f) => f,
                Err(e) => {
                    let _ = tx.send(ShardReady::Failed(format!(
                        "worker={}: frontend {} from_spec: {e}",
                        widx.0, spec.id,
                    )));
                    return;
                }
            };
            let listen_fd = match frontend.bind_listener() {
                Ok(fd) => fd,
                Err(e) => {
                    let _ = tx.send(ShardReady::Failed(format!(
                        "worker={}: frontend {} bind_listener: {e}",
                        widx.0, spec.id,
                    )));
                    return;
                }
            };
            let handle = NetHandle::new(socket.clone());
            let mut driver: HttpDriver<ShardPool> = HttpDriver::new(
                pool.clone(),
                handle,
                listen_fd,
                backend_id.clone(),
                stripe_size,
                page_size,
                origin,
                DEFAULT_META_TTL_MS,
            );
            shard_loop.add_tick_hook(move || driver.progress());
            eprintln!("shard {}: frontend {} driver registered", widx.0, spec.id);
        }
        None => {
            // No frontend on this shard; the listener and serve engine
            // are not built. The pool's origin backend stays dormant.
        }
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
        || SHUTDOWN.load(Ordering::Acquire),
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
    //   4. `rpc_server` - signals shutdown to outstanding RPC workers
    //      and waits briefly for them; this must complete while `fabric`
    //      (which they use) is still alive.
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
/// `local_node_id` falls back to `0` when unset. Load-time
/// validation rejects an unset id when peers are configured, so the
/// fallback only fires for a single-node (no-peer) deployment, where
/// the table routes every stripe to self/backend regardless of the
/// id. `PeerSpec.id` doubles as both the `NodeId` and the `PeerId`,
/// matching `config::reconcile_peers`, which adds connections keyed
/// by `PeerId(spec.id)`.
fn build_routing(config: &Config) -> (Arc<FingerTable>, Arc<HashMap<NodeId, PeerId>>) {
    let local_id = config.p2p.local_node_id.unwrap_or(0);
    let local = PeerEntry {
        node: NodeId(local_id),
        ring: node_to_ring(NodeId(local_id)),
        labels: TopologyLabels(config.p2p.local_labels.clone()),
    };

    let peers: Vec<PeerEntry> = config
        .peers
        .iter()
        .map(|spec| PeerEntry {
            node: NodeId(spec.id),
            ring: node_to_ring(NodeId(spec.id)),
            labels: TopologyLabels(spec.labels.clone()),
        })
        .collect();

    let node_to_peer: HashMap<NodeId, PeerId> = config
        .peers
        .iter()
        .map(|spec| (NodeId(spec.id), PeerId(spec.id)))
        .collect();

    let cfg = FingerTableConfig {
        k: config.p2p.fingers_per_node.max(1),
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

/// Select the single [`FrontendSpec`] a shard should serve.
///
/// `start_on_shard` consumes the one-per-shard [`ShardContext`] by
/// value, so a shard can drive at most one frontend through the
/// tick-hook seam. v1 therefore supports exactly one frontend per
/// shard: the first configured spec is selected and any extras are
/// logged and skipped. Returns `None` when no frontends are configured.
fn select_frontend_spec<'a>(
    widx: WorkerIdx,
    specs: &'a [FrontendSpec],
) -> Option<&'a FrontendSpec> {
    let first = specs.first()?;
    if specs.len() > 1 {
        let extra: Vec<&str> = specs[1..].iter().map(|s| s.id.as_str()).collect();
        eprintln!(
            "shard {}: multiple frontends configured; serving {:?}, skipping {:?} \
             (v1 supports one frontend per shard)",
            widx.0, first.id, extra,
        );
    }
    Some(first)
}

/// Validate and log the configured backends.
///
/// [`HttpBackend`] is now the active origin tier: it is built per shard
/// from the matching [`BackendSpec`] and plugged into the
/// `RoutedTransport`'s backend slot (see `run_shard`). This function
/// surfaces the configured backend set for observability and returns
/// the number of backends seen so the caller (and tests) can assert the
/// registry was walked.
fn log_backend_registry(widx: WorkerIdx, specs: &[BackendSpec]) -> usize {
    for spec in specs {
        eprintln!(
            "shard {}: backend {} kind={:?} endpoint={} stripe_size={} http_concurrency={} (HttpBackend active)",
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
    disk_channels: Arc<DiskChannelDirectory>,
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
                    let report =
                        config::reconcile_peers(fabric, &update.config.peers, Some(last_applied));
                    added += report.added;
                    removed += report.removed;
                    updated += report.updated;
                    failures += report.failures.len();
                    for (peer_id, msg) in &report.failures {
                        if first_failure.is_none() {
                            first_failure =
                                Some(format!("shard {} peer {} {}", widx.0, peer_id.0, msg));
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
                for (path, hint) in disk_registry.placement_snapshot() {
                    match hint {
                        Some(slot) => eprintln!(
                            "config gen={} disk {}: pinned to cpu {}",
                            update.generation,
                            path.display(),
                            slot.cpu,
                        ),
                        None => eprintln!(
                            "config gen={} disk {}: unpinned (no plan slot available)",
                            update.generation,
                            path.display(),
                        ),
                    }
                }
                // Republish a fresh channel snapshot on every config
                // update so any shard view caches its new generation
                // and reseats its buffer registrations against the
                // current per-disk storage cores.
                disk_channels.apply_channels(disk_registry.channels_snapshot());
            }
            Err(e) => {
                // A timeout is just a quiet poll interval; keep waiting.
                // A disconnect means the watcher's notify thread is gone
                // (panic, inotify error, ...). `main` joins the shard
                // threads after this returns, but those threads only
                // exit once `SHUTDOWN` is set, so we must latch it here
                // or the joins hang forever.
                if shutdown_on_watcher_error(e) {
                    SHUTDOWN.store(true, Ordering::Relaxed);
                    break;
                }
                continue;
            }
        }
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

    fn frontend_spec(id: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: config::FrontendKind::Http,
            bind: "0.0.0.0:9000".to_string(),
            backend: "b".to_string(),
            tls: None,
        }
    }

    fn backend_spec(id: &str) -> BackendSpec {
        BackendSpec {
            id: id.to_string(),
            kind: config::BackendKind::Http,
            endpoint: "https://example.com".to_string(),
            stripe_size_bytes: 4 * 1024 * 1024,
            http_concurrency: 64,
        }
    }

    #[test]
    fn select_frontend_none_when_empty() {
        let specs: Vec<FrontendSpec> = Vec::new();
        assert!(select_frontend_spec(WorkerIdx(0), &specs).is_none());
    }

    #[test]
    fn select_frontend_returns_single() {
        let specs = vec![frontend_spec("only")];
        let sel = select_frontend_spec(WorkerIdx(0), &specs).expect("one selected");
        assert_eq!(sel.id, "only");
    }

    #[test]
    fn select_frontend_picks_first_and_skips_extras() {
        // v1: a single frontend per shard. The first configured spec is
        // selected; extras are logged-and-skipped, not an error.
        let specs = vec![frontend_spec("a"), frontend_spec("b"), frontend_spec("c")];
        let sel = select_frontend_spec(WorkerIdx(3), &specs).expect("first selected");
        assert_eq!(sel.id, "a");
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
