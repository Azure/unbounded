//! Dependency composition and ordered node/worker lifecycle.
//!
//! Startup validates budgets/cpuset/geometry, enrolls credentials, opens O_DIRECT
//! slabs, recovers checkpoints, accepts a compatible snapshot, then starts workers
//! and listeners. Readiness follows usable resources. Control publication is a sole
//! node-level owner distributing immutable snapshots/key epochs to worker handles.
//!
//! Shutdown stops admission, drains reads/relays and dirty writes to the deadline,
//! optionally checkpoints a consistent all-worker cut, revokes RDMA permissions,
//! fences kernel/NIC references, then destroys buffers/devices and owned sockets.
//! Cache removal closes new admission; accepted resource owners drain independently.
//! Startup failures roll back created resources. Constructors perform no operational I/O.

use crate::telemetry::metrics::Gauge;
use crate::{
    client::{RequestParser, listener::ClientListeners, response::Responses},
    config::Config,
    control::{
        ControlClient, ControlEndpoint,
        caches::CacheRegistry,
        enrollment::Enrollment,
        secrets::BundleInstaller,
        snapshot::{PublishedState, SnapshotStore},
        transport::ReactorControlIo,
    },
    error::{Error, Operation, Result},
    http::connection::{HttpIo, HttpPool},
    memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool},
    model::{Limits, NodeId, RequestId, WorkerId},
    origin::{Origin, OriginClient},
    peer::{Relay, Requester, server::PeerServer, transport::Transfers},
    rdma::{Devices, RdmaTransfer, Sessions},
    read::{
        Coordinator,
        candidates::CandidatePolicy,
        dispatch::{WorkerDirectory, WorkerEndpoint},
        fill::{Fill, FillDependencies},
        flight::Flights,
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
    },
    runtime::{
        admission::Admission,
        affinity::AffinityPlan,
        deadline::RequestScope,
        reactor::Reactor,
        worker::{
            CryptoRuntime, CryptoService, WorkerFactory, WorkerGroup, WorkerMap, WorkerRuntime,
            WorkerService,
        },
    },
    security::{
        aead::{PageCrypto, PageCryptoEngine},
        credentials::CredentialCrypto,
        forwarding::Forwarding,
        identity::Certificates,
        identity::{KeyEpochs, KeyPurpose, Keyring},
        signing::Signatures,
    },
    store::{
        Store, StoreReader,
        checkpoint::Checkpointer,
        checkpoint_format::{CheckpointGeometry, ShardImage},
        eviction::SegmentClock,
        index::Index,
        recovery::Recovery,
        segment::Segments,
        slab::Slabs,
        writer::StoreWriter,
    },
    telemetry::Telemetry,
    topology::{health::LinkHealth, paths::Paths, placement::Placement},
};
#[cfg(test)]
use std::{collections::VecDeque, num::NonZeroUsize, time::Instant};
use std::{
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::Duration,
};

pub(crate) mod caches;
mod lifecycle;
mod native;
#[cfg(test)]
mod peer_tests;
mod recovery;
mod sizing;
#[cfg(test)]
use sizing::partition_limits_with_cause;
use sizing::{log_worker_plan, size_workers};

pub struct Application {
    config: Arc<Config>,
    node: Arc<NodeState>,
    limits: Limits,
    fabric_ports: Vec<crate::rdma::FabricPort>,
}

/// Shared immutable-publication and partitioned-admission roots. No Rc worker
/// graph crosses a thread. Worker zero alone drives enrollment/control reloads.
pub struct NodeState {
    hedges: std::sync::OnceLock<Arc<crate::read::hedge::Hedges>>,
    peer_admission: Arc<crate::peer::adaptive::AdaptivePeers>,
    subscriptions: Arc<crate::peer::subscriptions::Subscriptions>,
    ingress: Arc<crate::runtime::ingress::Ingress>,
    metrics: Vec<(WorkerId, crate::telemetry::metrics::Metrics)>,
    failures: crate::telemetry::failures::Failures,
    publications: Arc<PublishedState>,
    keys: Arc<KeyEpochs>,
    control_worker: WorkerId,
    workers: Arc<WorkerDirectory>,
    count: usize,
    prepared: AtomicUsize,
    checkpoint: Mutex<CheckpointCut>,
    periodic_checkpoint: Mutex<CheckpointCut>,
    recovery: Mutex<recovery::RecoveryCut>,
    observations: lifecycle::Observations,
    native: native::NativePairs,
    cache_cut: Mutex<caches::CacheCut>,
}
#[derive(Default)]
struct CheckpointCut {
    shards: Vec<ShardImage>,
    result: Option<Result<()>>,
    publishing: bool,
    periodic_generation: u64,
    periodic_started: Option<std::time::Instant>,
    periodic_finished: usize,
    last_sequence: u64,
    last_slot: usize,
}
impl Default for NodeState {
    fn default() -> Self {
        Self::new(vec![WorkerId(0), WorkerId(1)], 16).expect("valid default worker map")
    }
}
impl NodeState {
    fn new(workers: Vec<WorkerId>, capacity: usize) -> Result<Self> {
        Self::with_peer_admission(workers, capacity, Default::default())
    }
    fn with_peer_admission(
        workers: Vec<WorkerId>,
        capacity: usize,
        peer_config: crate::peer::adaptive::Config,
    ) -> Result<Self> {
        let count = workers.len();
        let map = Arc::new(WorkerMap::new(workers.clone())?);
        let metrics = crate::telemetry::metrics::Metrics::for_workers(count)?;
        let peer_admission =
            crate::peer::adaptive::AdaptivePeers::new(peer_config, metrics[0].clone())?;
        Ok(Self {
            peer_admission,
            hedges: std::sync::OnceLock::new(),
            ingress: Arc::new(crate::runtime::ingress::Ingress::new(&workers)),
            subscriptions: Arc::new(crate::peer::subscriptions::Subscriptions::new(
                Default::default(),
            )?),
            publications: Arc::new(PublishedState::default()),
            metrics: workers.iter().copied().zip(metrics).collect(),
            failures: crate::telemetry::failures::Failures::default(),
            keys: Arc::new(KeyEpochs::default()),
            control_worker: WorkerId(0),
            workers: Arc::new(WorkerDirectory::new(map, workers, capacity)?),
            count,
            prepared: AtomicUsize::new(0),
            checkpoint: Mutex::new(CheckpointCut::default()),
            periodic_checkpoint: Mutex::new(CheckpointCut::default()),
            recovery: Mutex::new(recovery::RecoveryCut::default()),
            observations: lifecycle::Observations::default(),
            native: native::NativePairs::default(),
            cache_cut: Mutex::new(caches::CacheCut::default()),
        })
    }
}

impl Application {
    /// Composition only. Does not open files, spawn threads, or accept requests.
    pub fn assemble(config: Config) -> Result<Self> {
        Ok(Self {
            limits: config.limits.clone(),
            config: Arc::new(config),
            node: Arc::new(NodeState::default()),
            fabric_ports: Vec::new(),
        })
    }

    pub fn run(mut self) -> Result<()> {
        self.config.validate()?;
        let mut plan = AffinityPlan::discover(&self.config)?;
        log_worker_plan("planned", &plan);
        self.limits = size_workers(&self.config.limits, &mut plan, self.config.enable_rdma)?;
        log_worker_plan("final", &plan);
        self.node = Arc::new(NodeState::with_peer_admission(
            plan.pairs.iter().map(|p| p.worker).collect(),
            self.limits.queue_entries.get(),
            self.config.peer_admission,
        )?);
        if self.config.enable_rdma {
            self.node.native.place(&plan)?;
            self.node
                .native
                .prepare(plan.pairs.iter().map(|p| p.worker), &self.limits)?;
        }
        let signals = SignalGuard::install()?;
        let scope = scope(self.config.request_timeout)?;
        let identity = bootstrap(&self.config, &self.node, &self.limits, &scope)?;
        Arc::get_mut(&mut self.config)
            .ok_or(Error::InvalidConfiguration)?
            .node = identity;
        let mut workers = WorkerGroup::new(plan);
        let result = workers.run(&self);
        if signals.requested() && result == Err(Error::Cancelled) {
            Ok(())
        } else {
            result
        }
    }
}

fn scope(timeout: Duration) -> Result<RequestScope> {
    RequestScope::new(
        RequestId([0; 16]),
        crate::runtime::environment::now() + timeout,
    )
}

/// Divide aggregate resource dimensions, preserving per-operation protocol caps.
/// Replay is a single node-wide table, so every handle uses the same node cap.
#[cfg(test)]
fn partition_limits(node: &Limits, workers: usize, rdma: bool) -> Result<Limits> {
    partition_limits_with_cause(node, workers, rdma).map_err(|(_, error)| error)
}

/// Enrollment is the only authority for the Node UID. No node-bound service graph
/// exists during this single-threaded, reactor-driven bootstrap.
fn bootstrap(
    config: &Config,
    node: &NodeState,
    limits: &Limits,
    startup: &RequestScope,
) -> Result<NodeId> {
    let admission = Rc::new(Admission::new(limits.clone()));
    let reactor = Rc::new(Reactor::new(admission));
    let io = Rc::new(ReactorControlIo::new(reactor.clone()));
    let unresolved = Rc::new(Keyring::new(
        config.cluster.clone(),
        NodeId(String::new()),
        node.keys.clone(),
    ));
    let projection = BundleInstaller::new(unresolved.clone());
    let enrollment = Rc::new(Enrollment::new(
        config.cluster.clone(),
        config.service_account_token.clone(),
        config.identity_directory.clone(),
    ));
    enrollment.set_shares(config.shares);
    let control = ControlClient::new(
        ControlEndpoint {
            url: config.control_endpoint.clone(),
            trust_bundle: config.trust_bundle.clone(),
        },
        enrollment,
        unresolved,
        projection,
        Rc::new(SnapshotStore::new(
            config.cluster.clone(),
            node.publications.clone(),
            config.limits.retained_snapshots.get(),
        )),
        Rc::new(CacheRegistry::default()),
    );
    control.attach_io(io);
    let mut operation = Box::pin(async {
        let identity = loop {
            match control.start(startup).await {
                Ok(identity) => break identity,
                Err(
                    Error::Io | Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded,
                ) => {
                    startup.check()?;
                    let io = ReactorControlIo::new(reactor.clone());
                    crate::control::transport::ControlIo::sleep(
                        &io,
                        control
                            .next_attempt()
                            .unwrap_or_else(crate::runtime::environment::now),
                        startup,
                    )
                    .await?;
                }
                Err(error) => return Err(error),
            }
        };
        let keys = Rc::new(Keyring::new(
            config.cluster.clone(),
            identity.node().clone(),
            node.keys.clone(),
        ));
        control.bind_keyring(keys)?;
        Ok(identity.node().clone())
    });
    let waker = futures::task::noop_waker();
    let mut cx = Context::from_waker(&waker);
    let result = loop {
        if STOP_REQUESTED.load(Ordering::Relaxed) {
            break Err(Error::Cancelled);
        }
        if let Err(error) = startup
            .check()
            .and_then(|()| reactor.poll_budgeted(64).map(|_| ()))
        {
            break Err(error);
        }
        if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
            break result;
        }
        if let Err(error) = reactor.wait(Duration::from_millis(1)) {
            break Err(error);
        }
    };
    drop(operation);
    // Dropped TLS waits can still own CQEs. Cancellation is not their fence.
    let _ = startup.cancel();
    let mut drain = reactor.drain();
    loop {
        reactor.poll_budgeted(64)?;
        if let Poll::Ready(fenced) = drain.as_mut().poll(&mut cx) {
            fenced?;
            break;
        }
        reactor.wait(Duration::from_millis(1))?;
    }
    result
}

static STOP_REQUESTED: AtomicBool = AtomicBool::new(false);
extern "C" fn request_stop(_: libc::c_int) {
    STOP_REQUESTED.store(true, Ordering::Relaxed);
}
struct SignalGuard {
    previous: Vec<(libc::c_int, libc::sigaction)>,
}
impl SignalGuard {
    fn install() -> Result<Self> {
        STOP_REQUESTED.store(false, Ordering::Relaxed);
        let mut guard = Self {
            previous: Vec::new(),
        };
        for signal in [libc::SIGINT, libc::SIGTERM] {
            // SAFETY: initialized sigaction records and a signal-safe atomic handler.
            unsafe {
                let mut action: libc::sigaction = std::mem::zeroed();
                action.sa_sigaction = request_stop as *const () as usize;
                libc::sigemptyset(&mut action.sa_mask);
                let mut previous = std::mem::zeroed();
                if libc::sigaction(signal, &action, &mut previous) != 0 {
                    return Err(Error::Io);
                }
                guard.previous.push((signal, previous));
            }
        }
        Ok(guard)
    }
    fn requested(&self) -> bool {
        STOP_REQUESTED.load(Ordering::Relaxed)
    }
}
impl Drop for SignalGuard {
    fn drop(&mut self) {
        for (signal, previous) in self.previous.iter().rev() {
            // SAFETY: restore the exact process handlers saved during installation.
            unsafe {
                libc::sigaction(*signal, previous, std::ptr::null_mut());
            }
        }
    }
}

/// This graph is constructed on its owning worker, never sent across threads.
/// Node-level control events enter through bounded worker commands. Mutable state
/// is not implicitly made global by Arc/Mutex or by a background async runtime.
pub struct WorkerApplication {
    #[cfg(test)]
    peer_requester: Rc<Requester>,
    ingress_peers: futures::stream::FuturesUnordered<Operation<'static, ()>>,
    next_health: std::time::Instant,
    environment: crate::runtime::environment::Environment,
    drivers: Rc<crate::read::drivers::DriverQueue>,
    http: Rc<HttpPool>,
    pub worker: WorkerId,
    runtime: WorkerRuntime,
    control: Option<Rc<ControlClient>>,
    snapshots: Rc<SnapshotStore>,
    keys: Rc<Keyring>,
    store: Store,
    clients: Rc<ClientListeners>,
    peers: Rc<PeerServer>,
    coordinator: Rc<Coordinator>,
    metadata: Rc<MetadataService>,
    /// Same table as Fill; the worker drives abandoned work without user futures.
    flights: Rc<Flights>,
    rdma: Option<Rc<RdmaTransfer>>,
    devices: Option<Rc<Devices>>,
    fabric_ports: Vec<crate::rdma::FabricPort>,
    actual_rails: Vec<crate::topology::rails::RailMapping>,
    native_numa: Option<native::NativePlacement>,
    native_task: Option<Operation<'static, Vec<crate::topology::rails::RailMapping>>>,
    native_retry: std::time::Instant,
    telemetry: Rc<Telemetry>,
    directory: Arc<WorkerDirectory>,
    node: Arc<NodeState>,
    endpoint: Option<WorkerEndpoint>,
    control_task: Option<Operation<'static, ()>>,
    keyring_task: Option<Operation<'static, ()>>,
    keyring_scope: Option<RequestScope>,
    peer_task: Option<Operation<'static, ()>>,
    writer_task: Option<Operation<'static, ()>>,
    checkpoint_task: Option<Operation<'static, ()>>,
    checkpoint_snapshot: Option<Operation<'static, ShardImage>>,
    checkpoint_generation: u64,
    checkpoint_completed: u64,
    checkpoint_budget: usize,
    placement: Rc<Placement>,
    diagnostic_task: Option<Operation<'static, ()>>,
    diagnostic_scope: Option<RequestScope>,
    diagnostics_address: std::net::SocketAddr,
    listener_scope: Option<RequestScope>,
    task_scope: Option<RequestScope>,
    peer_address: std::net::SocketAddr,
    timeout: Duration,
    shutdown_timeout: Duration,
    started: bool,
    stopping: bool,
    snapshot_sequence: Option<crate::control::wire::PublicationSequence>,
    memory: Rc<MemoryCache>,
    caches: Vec<crate::control::caches::CacheDefinition>,
    slab_directory: std::path::PathBuf,
    prepared_listeners: Rc<std::cell::RefCell<Option<crate::client::listener::PreparedListeners>>>,
    cache_prepare_task: Option<Operation<'static, ()>>,
    cache_preparing_generation: u64,
    control_scope: Option<RequestScope>,
}

impl WorkerApplication {
    /// All constructors below only connect dependencies. Runtime commands route
    /// listener work to page/metadata owners before any acquisition is dispatched.
    pub fn assemble(
        config: &Config,
        node: Arc<NodeState>,
        worker: WorkerId,
        runtime: WorkerRuntime,
        fabric_ports: Vec<crate::rdma::FabricPort>,
    ) -> Result<Self> {
        let environment = crate::runtime::environment::Environment::current();
        let metrics = node
            .metrics
            .iter()
            .find(|(id, _)| *id == worker)
            .map(|(_, metrics)| metrics.clone())
            .ok_or(Error::InvalidConfiguration)?;
        runtime.crypto.set_metrics(metrics.clone());
        let drivers = Rc::new(crate::read::drivers::DriverQueue::default());
        let _queue = drivers.enter();
        let admission = runtime.admission.clone();
        config.page_hedge.validate()?;
        let hedges = node
            .hedges
            .get_or_init(|| {
                crate::read::hedge::Hedges::new(config.page_hedge, node.metrics[0].1.clone())
                    .expect("validated hedge config")
            })
            .clone();
        admission.set_observer(node.failures.observer(worker));
        metrics.observe_admission(worker, admission.usage())?;
        node.ingress.install(worker, &admission)?;
        let reactor = runtime.reactor.clone();
        let limits = admission.limits();
        let snapshots = Rc::new(SnapshotStore::new(
            config.cluster.clone(),
            node.publications.clone(),
            config.limits.retained_snapshots.get(),
        ));
        let caches = Rc::new(CacheRegistry::default());
        let keys = Rc::new(Keyring::new(
            config.cluster.clone(),
            config.node.clone(),
            node.keys.clone(),
        ));
        let certificates = Rc::new(Certificates::new(config.cluster.clone(), keys.clone()));
        let availability = Rc::new(crate::control::availability::Availability::new(
            node.publications.clone(),
            keys.clone(),
        ));
        let signatures = Rc::new(Signatures::new(keys.clone(), certificates.clone()));
        let forwarding = Rc::new(Forwarding::new(signatures.clone()));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let crypto = Rc::new(PageCrypto::new(keys.clone(), runtime.crypto.clone()));
        let control = if worker == node.control_worker {
            Some(Self::assemble_control(
                config,
                &runtime,
                keys.clone(),
                snapshots.clone(),
                caches,
            ))
        } else {
            None
        };

        let http = Rc::new(
            HttpPool::new(
                reactor.clone(),
                admission.clone(),
                limits.connections_per_neighbor.get(),
            )
            .with_origin_limit(
                config
                    .origin_connections_per_cache
                    .get()
                    .min(limits.client_connections.get()),
            ),
        );
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            crate::http::Codec::new(
                crate::peer::protocol::MAX_ENVELOPE_HEAD,
                crate::model::PAGE_BYTES + 16,
            ),
            admission.clone(),
        ));
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let memory =
            Rc::new(MemoryCache::new(buffers.clone()).with_availability(availability.clone()));
        let pipes = Rc::new(PipePool::new(admission.clone(), reactor.clone()));
        let delivery = Rc::new(
            Delivery::new(pipes.clone(), config.reader_stall_timeout).with_metrics(metrics.clone()),
        );

        let store = Self::assemble_storage(
            config,
            &node,
            worker,
            &runtime,
            buffers.clone(),
            availability.clone(),
            &metrics,
        )?;
        let disk = store.reader.clone();
        let writer = store.writer.clone();

        let (sessions, rdma, devices) = if config.enable_rdma {
            let devices = Rc::new(Devices::new());
            if let Some(port) = node.native.io(worker)? {
                devices.attach(port)?;
            }
            let sessions = Rc::new(Sessions::new(
                devices.clone(),
                config.limits.connections_per_neighbor.get(),
            ));
            let transfer = Rc::new(RdmaTransfer::new(sessions.clone()));
            (Some(sessions), Some(transfer), Some(devices))
        } else {
            (None, None, None)
        };

        let paths = Rc::new(
            Paths::with_algorithm(
                Rc::new(LinkHealth),
                limits.cached_paths.get(),
                config.routing_algorithm,
            )
            .with_peer_admission(node.peer_admission.clone()),
        );
        let placement = Rc::new(Placement::with_memory_budget(
            limits.cached_rankings.get() * crate::topology::placement::RANKING_BYTES,
        ));
        let network = Rc::new(crate::peer::PeerNetwork::with_algorithm(
            config.node.clone(),
            node.publications.clone(),
            config.routing_algorithm,
        )?);
        let wire = Rc::new(crate::peer::protocol::SecurityCodec::new(
            admission.clone(),
            buffers.clone(),
        ));
        let transfers = Transfers::new(
            http.clone(),
            io.clone(),
            rdma.clone(),
            admission.clone(),
            wire.clone(),
            signatures.clone(),
        )
        .with_reclamation(Self::ciphertext_reclaimer(
            admission.clone(),
            memory.clone(),
            writer.clone(),
        ));
        let transfers = Rc::new(match &sessions {
            Some(sessions) => transfers.with_native(sessions.clone()),
            None => transfers,
        });
        let requester = Rc::new(
            Requester::new(
                paths.clone(),
                forwarding.clone(),
                transfers.clone(),
                network.clone(),
            )
            .with_observer(admission.observer()),
        );
        let candidates = Rc::new(
            CandidatePolicy::new(
                config.node.clone(),
                placement.clone(),
                requester.clone(),
                credentials.clone(),
                node.publications.clone(),
            )
            .with_hedges(hedges)
            .with_attempt_timeout(config.peer_attempt_timeout)
            .with_observer(admission.observer()),
        );
        let origin: Rc<dyn Origin> = Rc::new(OriginClient::new(
            snapshots.clone(),
            http.clone(),
            Rc::new(
                io.capped(
                    config
                        .limits
                        .header_bytes
                        .get()
                        .min(crate::http::MAX_HEAD_BYTES),
                ),
            ),
            admission.clone(),
            buffers.clone(),
            "/run/racer",
        )?);
        let flights =
            Rc::new(Flights::new(admission.clone()).with_availability(availability.clone()));
        let (coordinator, metadata) = Self::assemble_reads(
            config,
            &node,
            snapshots.clone(),
            availability,
            delivery.clone(),
            &metrics,
            FillDependencies {
                memory: memory.clone(),
                buffers,
                disk,
                writer,
                origin: origin.clone(),
                candidates: candidates.clone(),
                flights: flights.clone(),
                crypto,
                credentials: credentials.clone(),
                admission: admission.clone(),
                metadata_owner: node.workers.clone(),
            },
        );
        let relay = Rc::new(Relay::new(
            paths,
            forwarding.clone(),
            requester.clone(),
            admission.clone(),
            network.clone(),
        ));
        #[cfg(not(test))]
        let distributed = true;
        #[cfg(test)]
        let distributed = crate::runtime::reactor::simulation::Simulation::current().is_none();
        let peers = PeerServer::new(
            io.clone(),
            forwarding,
            admission.clone(),
            Rc::new(node.workers.clone()),
            relay,
            wire,
            signatures,
            node.subscriptions.clone(),
            pipes.clone(),
            transfers,
            crate::peer::server::Settings {
                accept: if distributed {
                    crate::peer::server::AcceptMode::Distributed(node.ingress.clone())
                } else {
                    crate::peer::server::AcceptMode::Local
                },
                request_timeout: config.request_timeout,
                opaque_relay: config.opaque_relay,
            },
        );
        let peers = Rc::new(peers);
        let clients = Self::assemble_clients(
            config,
            &node,
            &runtime,
            coordinator.clone(),
            delivery,
            &metrics,
            distributed,
        );

        let mut telemetry = Telemetry::default();
        telemetry.failures = node.failures.clone();
        telemetry.metrics = metrics;
        telemetry.health = node.observations.health.clone();
        Ok(Self {
            ingress_peers: futures::stream::FuturesUnordered::new(),
            #[cfg(test)]
            peer_requester: requester,
            http,
            environment,
            drivers,
            worker,
            runtime,
            control,
            snapshots,
            keys,
            store,
            clients,
            peers,
            coordinator,
            metadata,
            flights,
            rdma,
            devices,
            fabric_ports,
            actual_rails: Vec::new(),
            native_numa: node.native.numa(worker)?,
            native_task: None,
            native_retry: crate::runtime::environment::now(),
            telemetry: Rc::new(telemetry),
            directory: node.workers.clone(),
            node,
            endpoint: None,
            control_task: None,
            keyring_task: None,
            keyring_scope: None,
            peer_task: None,
            writer_task: None,
            checkpoint_task: None,
            checkpoint_snapshot: None,
            checkpoint_generation: 0,
            checkpoint_completed: 0,
            checkpoint_budget: config.checkpoint_bytes.get(),
            placement,
            diagnostic_task: None,
            diagnostic_scope: None,
            diagnostics_address: config.diagnostics_listen,
            listener_scope: None,
            task_scope: None,
            peer_address: config.peer_listen,
            timeout: config.request_timeout,
            shutdown_timeout: config.shutdown_timeout,
            started: false,
            stopping: false,
            snapshot_sequence: None,
            memory,
            caches: Vec::new(),
            slab_directory: config.slab_directory.clone(),
            prepared_listeners: Rc::new(std::cell::RefCell::new(None)),
            cache_prepare_task: None,
            cache_preparing_generation: 0,
            control_scope: None,
            next_health: crate::runtime::environment::now(),
        })
    }

    fn assemble_reads(
        config: &Config,
        node: &NodeState,
        snapshots: Rc<SnapshotStore>,
        availability: Rc<crate::control::availability::Availability>,
        delivery: Rc<Delivery>,
        metrics: &crate::telemetry::metrics::Metrics,
        dependencies: FillDependencies,
    ) -> (Rc<Coordinator>, Rc<MetadataService>) {
        let candidates = dependencies.candidates.clone();
        let origin = dependencies.origin.clone();
        let credentials = dependencies.credentials.clone();
        let index = dependencies.writer.index().clone();
        let admission = dependencies.admission.clone();
        let fill = Rc::new(Fill::new(dependencies).with_metrics(metrics.clone()));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            origin,
            credentials.clone(),
            admission.limits().metadata_entries.get(),
            MetadataDependencies {
                index,
                fill: fill.clone(),
                owners: node.workers.clone(),
            },
        ));
        let streams = Rc::new(
            RangeStreams::new(
                node.workers.clone(),
                delivery,
                config.limits.range_window_pages.get(),
            )
            .with_observer(admission.observer()),
        );
        let coordinator = Rc::new(
            Coordinator::new(snapshots, metadata.clone(), fill, streams, credentials)
                .with_availability(availability),
        );
        (coordinator, metadata)
    }

    fn assemble_clients(
        config: &Config,
        node: &NodeState,
        runtime: &WorkerRuntime,
        coordinator: Rc<Coordinator>,
        delivery: Rc<Delivery>,
        metrics: &crate::telemetry::metrics::Metrics,
        distributed: bool,
    ) -> Rc<ClientListeners> {
        let admission = runtime.admission.clone();
        let io = Rc::new(HttpIo::for_clients(
            runtime.reactor.clone(),
            admission.clone(),
        ));
        let responses =
            Rc::new(Responses::new(io.clone(), delivery).with_observer(admission.observer()));
        let clients = ClientListeners::new(
            coordinator,
            RequestParser::new(config.limits.header_bytes.get()),
            responses,
            io,
            admission,
        )
        .with_request_timeout(config.request_timeout)
        .with_metrics(metrics.clone());
        Rc::new(if distributed {
            clients.with_ingress(node.ingress.clone())
        } else {
            clients
        })
    }

    fn assemble_control(
        config: &Config,
        runtime: &WorkerRuntime,
        keys: Rc<Keyring>,
        snapshots: Rc<SnapshotStore>,
        caches: Rc<CacheRegistry>,
    ) -> Rc<ControlClient> {
        let enrollment = Rc::new(Enrollment::new(
            config.cluster.clone(),
            config.service_account_token.clone(),
            config.identity_directory.clone(),
        ));
        enrollment.set_shares(config.shares);
        let secrets = BundleInstaller::new(keys.clone());
        let control = Rc::new(ControlClient::new(
            ControlEndpoint {
                url: config.control_endpoint.clone(),
                trust_bundle: config.trust_bundle.clone(),
            },
            enrollment,
            keys,
            secrets,
            snapshots,
            caches,
        ));
        control.attach_io(Rc::new(ReactorControlIo::new(runtime.reactor.clone())));
        control
    }

    fn assemble_storage(
        config: &Config,
        node: &NodeState,
        worker: WorkerId,
        runtime: &WorkerRuntime,
        buffers: Rc<BufferPool>,
        availability: Rc<crate::control::availability::Availability>,
        metrics: &crate::telemetry::metrics::Metrics,
    ) -> Result<Store> {
        let limits = runtime.admission.limits();
        let index = Rc::new(
            Index::new(worker, limits.metadata_entries.get())
                .with_availability(availability.clone()),
        );
        let segments = Rc::new(Segments::new(worker, config.segment_bytes));
        let eviction = Rc::new(SegmentClock::new(
            index.clone(),
            segments.clone(),
            config.free_segment_reserve,
        ));
        let slabs = Rc::new(Slabs::new(
            worker,
            config.slab_directory.clone(),
            runtime.reactor.clone(),
            runtime.admission.clone(),
            config.slab_bytes,
            config.segment_bytes,
        ));
        let reader = Rc::new(
            StoreReader::new(
                eviction.clone(),
                index.clone(),
                segments.clone(),
                slabs.clone(),
                buffers,
            )
            .with_metrics(metrics.clone()),
        );
        let writer = Rc::new(
            StoreWriter::new(index.clone(), segments.clone(), slabs)
                .with_availability(availability)
                .with_metrics(metrics.clone()),
        );
        writer.configure(
            runtime.admission.clone(),
            eviction.clone(),
            limits.queue_entries.get(),
            (config.disk_page_entries.get() / node.count).max(1),
        )?;
        Ok(Store {
            reader,
            writer,
            checkpoint: Rc::new(Checkpointer::new(
                config.slab_directory.clone(),
                index.clone(),
                segments.clone(),
            )),
            recovery: Recovery::new(config.slab_directory.clone(), index, segments),
            eviction,
        })
    }

    fn ciphertext_reclaimer(
        admission: Rc<Admission>,
        memory: Rc<MemoryCache>,
        writer: Rc<StoreWriter>,
    ) -> impl Fn(&crate::model::CacheId, usize) {
        move |cache, amount| {
            let class = crate::model::ResourceClass::Ciphertext;
            for _ in 0..2 {
                let Some((owner, bytes)) = admission.reclamation(cache, class, amount) else {
                    break;
                };
                let released = memory.reclaim_idle(class, owner.as_ref(), bytes, |page| {
                    writer.discard_idle_copy(page)
                });
                if released < bytes {
                    writer.reclaim_ciphertext(owner.as_ref(), bytes - released);
                }
                admission.reclaim_buffers();
            }
        }
    }

    fn refresh_snapshot(&mut self, current_scope: &RequestScope) -> Result<()> {
        let snapshot = self.snapshots.current()?;
        if self
            .snapshot_sequence
            .is_some_and(|sequence| sequence != snapshot.sequence)
            && self.native_task.is_some()
        {
            self.native_task.take();
            if let Some(devices) = &self.devices {
                devices.close();
            }
        }
        if !self.actual_rails.is_empty() {
            let compatible = self.native_publication().is_ok_and(|published_rails| {
                published_rails.len() == self.actual_rails.len()
                    && self.actual_rails.iter().all(|actual| {
                        published_rails.iter().any(|published| {
                            published.rail == actual.rail
                                && published.fabric == actual.fabric
                                && published
                                    .numa_node
                                    .is_none_or(|numa| actual.numa_node == Some(numa))
                        })
                    })
            });
            if !compatible {
                if let Some(devices) = &self.devices {
                    devices.close();
                }
                self.actual_rails.clear();
            }
        }
        if self.snapshot_sequence == Some(snapshot.sequence) {
            return Ok(());
        }
        for old in &self.caches {
            if !snapshot.caches.iter().any(|new| new.id == old.id) {
                self.memory.remove_cache(&old.id)?;
                self.store.writer.remove_cache(&old.id)?;
            }
        }
        current_scope.check()?;
        self.caches = snapshot.caches.clone();
        self.snapshot_sequence = Some(snapshot.sequence);
        Ok(())
    }

    fn poll_services(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        let _environment = self.environment.enter();
        let _queue = self.drivers.enter();
        if work_budget == 0 {
            return Ok(());
        }
        if let Some(hedges) = self.node.hedges.get() {
            hedges.poll();
        }
        let budget = work_budget.min(64);
        self.poll_native(cx)?;
        if !self.stopping
            && let Ok(snapshot) = self.snapshots.current()
        {
            match self.placement.maintain(&snapshot.membership) {
                Ok(()) | Err(Error::Overloaded) => (),
                Err(error) => return Err(error),
            }
        }
        self.poll_checkpoint(cx)?;
        self.poll_ingress(cx, budget)?;
        self.http.poll_waiters(budget);
        // Expire metadata before advancing peer/listener tasks.
        self.metadata
            .poll_deadlines(crate::runtime::environment::now(), budget);
        if !self.stopping {
            self.poll_cache_preparation(cx)?;
        }
        self.poll_listeners(cx, budget)?;
        self.poll_writer(cx)?;
        if !self.stopping {
            self.poll_control(cx)?;
            self.refresh_snapshot(&scope(self.timeout)?)?;
        }
        let now = crate::runtime::environment::now();
        if now >= self.next_health {
            self.observe_health()?;
            self.next_health = now + Duration::from_millis(100);
        }
        Ok(())
    }

    fn poll_ingress(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
        {
            for accepted in self
                .node
                .ingress
                .pop_batch::<64>(self.worker, cx.waker(), budget)?
                .into_iter()
                .flatten()
            {
                let connection = crate::http::connection::ConnectionLease::from_reserved(
                    accepted.fd.into(),
                    accepted.reservation,
                )?;
                match accepted.kind {
                    crate::runtime::ingress::Kind::Client(cache, retired) => {
                        self.clients
                            .install_connection(connection, cache, retired)?;
                    }
                    crate::runtime::ingress::Kind::Peer => {
                        let peers = self.peers.clone();
                        let scope = self.task_scope.clone().ok_or(Error::Unavailable)?;
                        self.ingress_peers.push(Box::pin(async move {
                            let mut connection = connection;
                            loop {
                                connection = peers.serve_connection(connection, &scope).await?;
                                if !connection.is_reusable() {
                                    return Ok(());
                                }
                            }
                        }));
                    }
                }
            }
        }
        for _ in 0..budget {
            use futures::Stream;
            if !matches!(
                std::pin::Pin::new(&mut self.ingress_peers).poll_next(cx),
                Poll::Ready(Some(_))
            ) {
                break;
            }
        }
        Ok(())
    }

    fn poll_listeners(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
        if let Some(result) = poll_task(&mut self.diagnostic_task, cx) {
            if let Err(error) = &result
                && !self.stopping
            {
                eprintln!(
                    "racer-dataplane: worker={} stage=diagnostic-service error={error:?}",
                    self.worker.0
                );
            }
            if !self.stopping {
                result?;
                return Err(Error::Unavailable);
            }
            if !matches!(
                result,
                Ok(()) | Err(Error::Cancelled | Error::DeadlineExceeded)
            ) {
                result?;
            }
        }
        if let Some(endpoint) = &mut self.endpoint {
            endpoint.poll(cx, budget).inspect_err(|error| {
                eprintln!(
                    "racer-dataplane: worker={} stage=worker-endpoint error={error:?}",
                    self.worker.0
                );
            })?;
        }
        self.flights.poll_with_context(cx, budget)?;
        self.clients
            .poll_budgeted(cx, budget)
            .inspect_err(|error| {
                eprintln!(
                    "racer-dataplane: worker={} stage=client-listeners error={error:?}",
                    self.worker.0
                );
            })?;
        if let Some(rdma) = &self.rdma {
            rdma.progress()?;
        }
        if let Some(result) = poll_task(&mut self.peer_task, cx) {
            if let Err(error) = &result
                && !self.stopping
            {
                eprintln!(
                    "racer-dataplane: worker={} stage=peer-listener error={error:?}",
                    self.worker.0
                );
            }
            if !self.stopping {
                result?;
                return Err(Error::Unavailable);
            }
            if !matches!(
                result,
                Ok(()) | Err(Error::Cancelled | Error::DeadlineExceeded)
            ) {
                result?;
            }
        }
        Ok(())
    }

    fn poll_writer(&mut self, cx: &mut Context<'_>) -> Result<()> {
        if let Some(result) = poll_task(&mut self.writer_task, cx)
            && !matches!(
                result,
                Err(Error::Io
                    | Error::Unavailable
                    | Error::Overloaded
                    | Error::MissingKey
                    | Error::Cancelled
                    | Error::DeadlineExceeded)
            )
        {
            result?;
        }
        let checkpoint_waiting = self
            .node
            .periodic_checkpoint
            .lock()
            .is_ok_and(|cut| cut.periodic_generation != 0 && cut.result.is_none());
        if !checkpoint_waiting
            && self.writer_task.is_none()
            && self.store.writer.pending_count() != 0
        {
            let writer = self.store.writer.clone();
            let write_scope = scope(self.timeout)?;
            self.writer_task = Some(Box::pin(async move {
                writer.progress(8, &write_scope).await.map(|_| ())
            }));
        }
        Ok(())
    }

    fn poll_control(&mut self, cx: &mut Context<'_>) -> Result<()> {
        self.poll_keyring(cx)?;
        if let Some(result) = poll_task(&mut self.control_task, cx)
            && !matches!(
                result,
                Err(Error::Io | Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded)
            )
        {
            result?;
        }
        if self.control_task.is_none()
            && let Some(control) = &self.control
        {
            let control = control.clone();
            let turn = scope(crate::control::wire::POLL_WAIT + Duration::from_secs(10))?;
            self.control_scope = Some(turn.clone());
            self.control_task = Some(Box::pin(async move {
                control.progress(&turn).await.map(|_| ())
            }));
        }
        Ok(())
    }
    fn poll_keyring(&mut self, cx: &mut Context<'_>) -> Result<()> {
        if let Some(result) = poll_task(&mut self.keyring_task, cx)
            && !matches!(
                result,
                Err(Error::Io | Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded)
            )
        {
            result?;
        }
        if self.keyring_task.is_none()
            && let Some(control) = &self.control
        {
            let control = control.clone();
            let turn = scope(Duration::from_secs(80))?;
            self.keyring_scope = Some(turn.clone());
            self.keyring_task = Some(Box::pin(
                async move { control.keyring_progress(&turn).await },
            ));
        }
        Ok(())
    }

    pub fn shutdown<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        let environment = self.environment.clone();
        let drivers = self.drivers.clone();
        Box::pin(environment.scope(drivers.scope(async move {
            if let Some(endpoint) = &mut self.endpoint {
                endpoint.uninstall()?;
            }
            self.endpoint.take();
            self.store.checkpoint.finish_snapshot();
            self.started = false;
            self.telemetry
                .health
                .transition(crate::telemetry::health::State::Stopped)?;
            Ok(())
        })))
    }
}

fn poll_task(
    task: &mut Option<Operation<'static, ()>>,
    cx: &mut Context<'_>,
) -> Option<Result<()>> {
    match task.as_mut()?.as_mut().poll(cx) {
        Poll::Pending => None,
        Poll::Ready(result) => {
            task.take();
            Some(result)
        }
    }
}

impl WorkerFactory for Application {
    fn limits(&self) -> Limits {
        self.limits.clone()
    }
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>> {
        let application = WorkerApplication::assemble(
            &self.config,
            self.node.clone(),
            worker,
            runtime,
            self.fabric_ports.clone(),
        )?;
        Ok(Box::new(application))
    }
    fn build_crypto(
        &self,
        worker: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn CryptoService>> {
        self.node
            .native
            .crypto(worker, PageCryptoEngine::new(runtime))
    }
}
impl WorkerService for WorkerApplication {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        WorkerApplication::start(self, scope)
    }
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        if !self.started {
            return Err(Error::Unavailable);
        }
        if STOP_REQUESTED.load(Ordering::Relaxed) {
            return Err(Error::Cancelled);
        }
        self.poll_services(cx, work_budget)
    }
    fn stop_admission(&mut self) -> Result<()> {
        let _environment = self.environment.enter();
        let _queue = self.drivers.enter();
        self.stopping = true;
        self.node.ingress.close(self.worker);
        if let Some(scope) = &self.task_scope {
            scope.cancel()?;
        }
        self.cache_prepare_task.take();
        self.prepared_listeners.borrow_mut().take();
        if self.telemetry.health.state()? != crate::telemetry::health::State::Stopped {
            self.telemetry
                .health
                .transition(crate::telemetry::health::State::Draining)?;
        }
        self.clients.stop_admission();
        if let Some(endpoint) = &self.endpoint {
            endpoint.stop_admission();
        }
        if let Some(scope) = &self.listener_scope {
            scope.cancel()?;
        }
        if let Some(control) = &self.control {
            control.shutdown()?;
        }
        self.control_task.take();
        if let Some(scope) = self.keyring_scope.take() {
            scope.cancel()?;
        }
        self.keyring_task.take();
        Ok(())
    }
    fn drain<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        let environment = self.environment.clone();
        let drivers = self.drivers.clone();
        Box::pin(environment.scope(drivers.scope(async move {
            self.stop_admission()?;
            let deadline = scope(self.shutdown_timeout)?;
            let clients = self.clients.clone();
            let flights = self.flights.clone();
            let fence_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
            let mut client_drain = clients.drain(&deadline);
            let mut flight_drain = flights.drain(&fence_scope);
            let mut clients_done = false;
            let mut flights_done = false;
            let mut first_error = None;
            std::future::poll_fn(|cx| {
                if deadline.check().is_err()
                    && let Err(error) = self.store.writer.cancel_pending_writes()
                {
                    first_error.get_or_insert(error);
                }
                if let Err(error) = self.poll_services(cx, 64) {
                    first_error.get_or_insert(error);
                }
                if !clients_done && let Poll::Ready(result) = client_drain.as_mut().poll(cx) {
                    if let Err(error) = result {
                        first_error.get_or_insert(error);
                    }
                    clients_done = true;
                }
                if !flights_done && let Poll::Ready(result) = flight_drain.as_mut().poll(cx) {
                    if let Err(error) = result {
                        first_error.get_or_insert(error);
                    }
                    flights_done = true;
                }
                if let Some(endpoint) = &mut self.endpoint {
                    let mut drain = endpoint.drain(&deadline);
                    if let Poll::Ready(Err(error)) = drain.as_mut().poll(cx) {
                        first_error.get_or_insert(error);
                    }
                }
                if clients_done
                    && flights_done
                    && self.clients.active_connections() == 0
                    && self
                        .endpoint
                        .as_ref()
                        .is_none_or(WorkerEndpoint::is_drained)
                    && self.peer_task.is_none()
                    && self.ingress_peers.is_empty()
                    && self.writer_task.is_none()
                    && self.checkpoint_task.is_none()
                    && self.checkpoint_snapshot.is_none()
                    && self.checkpoint_generation == 0
                    && self.store.writer.is_idle()
                {
                    Poll::Ready(())
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await;
            if let Some(rdma) = &self.rdma {
                rdma.drain().await?;
            }
            if let Some(devices) = &self.devices {
                devices.close();
            }
            if let Some(scope) = &self.diagnostic_scope {
                scope.cancel()?;
            }
            std::future::poll_fn(|cx| match poll_task(&mut self.diagnostic_task, cx) {
                Some(Ok(()) | Err(Error::Cancelled | Error::DeadlineExceeded)) | None
                    if self.diagnostic_task.is_none() =>
                {
                    Poll::Ready(Ok(()))
                }
                Some(Err(error)) => Poll::Ready(Err(error)),
                _ => Poll::Pending,
            })
            .await?;
            if let Some(error) = first_error {
                self.node
                    .checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .result = Some(Err(error));
                return Err(error);
            }
            if self.started {
                self.checkpoint(&deadline).await?;
            }
            Ok(())
        })))
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        WorkerApplication::shutdown(self, scope)
    }
}

#[cfg(test)]
mod dst;

#[cfg(test)]
mod integration_tests;
#[cfg(test)]
mod test_support;

#[cfg(test)]
mod lifetime_tests;

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::runtime::{
        admission::Admission,
        crypto::{self, CryptoClient},
        reactor::Reactor,
    };

    #[test]
    fn worker_sizing_reports_specific_resource_floor() {
        let base = Config::from_lookup_with_fabric_ports(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                "RACER_ENABLE_RDMA" => Some("false".into()),
                _ => None,
            })
        })
        .unwrap()
        .0
        .limits;
        assert!(partition_limits_with_cause(&base, 1, false).is_ok());
        assert_eq!(
            partition_limits_with_cause(&base, 0, false).err(),
            Some(("io_workers", Error::InvalidConfiguration))
        );
        for dimension in [
            "plaintext_bytes",
            "ciphertext_bytes",
            "dirty_bytes",
            "request_context_bytes",
            "queue_entries",
            "client_connections",
            "registered_bytes",
            "pipes",
        ] {
            let mut limits = base.clone();
            let value = match dimension {
                "plaintext_bytes" => &mut limits.plaintext_bytes,
                "ciphertext_bytes" => &mut limits.ciphertext_bytes,
                "dirty_bytes" => &mut limits.dirty_bytes,
                "request_context_bytes" => &mut limits.request_context_bytes,
                "queue_entries" => &mut limits.queue_entries,
                "client_connections" => &mut limits.client_connections,
                "registered_bytes" => &mut limits.registered_bytes,
                _ => &mut limits.pipes,
            };
            *value = NonZeroUsize::new(1).unwrap();
            let workers = if dimension == "pipes" { 2 } else { 1 };
            let rdma = dimension == "registered_bytes";
            assert_eq!(
                partition_limits_with_cause(&limits, workers, rdma).err(),
                Some((dimension, Error::InvalidConfiguration)),
                "{dimension}"
            );
            if rdma {
                assert!(partition_limits_with_cause(&limits, workers, false).is_ok());
            }
        }
    }

    #[test]
    fn worker_sizing_funds_derived_connection_pools() {
        use crate::{
            model::ResourceClass,
            runtime::affinity::{CpuLocation, EffectiveTopology},
        };
        let config = Config::from_lookup_with_fabric_ports(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                "RACER_ENABLE_RDMA" => Some("false".into()),
                "RACER_CLIENT_CONNECTIONS" => Some("16".into()),
                _ => None,
            })
        })
        .unwrap()
        .0;
        let make_plan = || {
            AffinityPlan::from_topology(
                &config,
                EffectiveTopology {
                    cpus: (0..8)
                        .map(|cpu| CpuLocation {
                            cpu,
                            package: 0,
                            core: cpu,
                            numa_node: Some(0),
                        })
                        .collect(),
                    quota: None,
                    nics: vec![],
                },
                &[],
            )
            .unwrap()
        };
        let mut plan = make_plan();
        assert_eq!(plan.pairs.len(), 5);
        for workers in 2..=5 {
            // This cause feeds the production worker-sizing diagnostic, including
            // the last rejected count before reduction succeeds.
            assert_eq!(
                partition_limits_with_cause(&config.limits, workers, false).err(),
                Some(("control_connections", Error::InvalidConfiguration))
            );
        }
        let limits = size_workers(&config.limits, &mut plan, false).unwrap();
        assert_eq!(plan.pairs.len(), 1);
        assert_eq!(plan.crypto_groups(), vec![vec![0]]);
        assert_eq!(limits.client_connections.get(), 16);

        for (connections, neighbor, cause) in [
            (3, 2, Some("control_connections")),
            (11, 2, Some("control_connections")),
            (12, 2, None),
            (15, 4, None),
            (16, 4, None),
        ] {
            let mut node = config.limits.clone();
            node.client_connections = NonZeroUsize::new(connections).unwrap();
            node.connections_per_neighbor = NonZeroUsize::new(neighbor).unwrap();
            let result = partition_limits_with_cause(&node, 1, false);
            if let Some(cause) = cause {
                assert_eq!(result.err(), Some((cause, Error::InvalidConfiguration)));
                assert!(matches!(
                    size_workers(&node, &mut make_plan(), false),
                    Err(Error::InvalidConfiguration)
                ));
                continue;
            }
            let admission = Admission::new(result.unwrap());
            let control = (0..3)
                .map(|_| {
                    admission
                        .reserve_connection(ResourceClass::ControlConnection)
                        .unwrap()
                })
                .collect::<Vec<_>>();
            let outbound_count = neighbor.min(admission.limit(ResourceClass::OutboundConnection));
            let outbound = (0..outbound_count)
                .map(|_| {
                    admission
                        .reserve_connection(ResourceClass::OutboundConnection)
                        .unwrap()
                })
                .collect::<Vec<_>>();
            let ingress = admission
                .reserve_connection(ResourceClass::IngressConnection)
                .unwrap();
            assert_eq!(
                admission.used(ResourceClass::Connection),
                3 + outbound_count + 1
            );
            assert!(admission.used(ResourceClass::Connection) <= connections);
            drop((control, outbound, ingress));
            assert_eq!(admission.used(ResourceClass::Connection), 0);
        }
    }

    pub(crate) fn wake_test_worker() -> WorkerApplication {
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(Admission::new(config.limits.clone()));
        let (io, _engine) = crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
        WorkerApplication::assemble(
            &config,
            Arc::new(NodeState::default()),
            WorkerId(0),
            WorkerRuntime {
                reactor: Rc::new(Reactor::new(admission.clone())),
                admission,
                crypto: Rc::new(CryptoClient::new(io)),
            },
            Vec::new(),
        )
        .unwrap()
    }

    pub(crate) fn wake_test_coordinator() -> Rc<Coordinator> {
        wake_test_worker().coordinator
    }

    #[test]
    fn assembled_worker_exports_live_quota_gauges() {
        use crate::model::ResourceClass;
        let worker = wake_test_worker();
        let relay = worker
            .runtime
            .admission
            .reserve(None, ResourceClass::Relay, 1)
            .unwrap();
        let ciphertext = worker
            .runtime
            .admission
            .reserve(None, ResourceClass::Ciphertext, 17)
            .unwrap();
        let mut output = String::new();
        worker
            .telemetry
            .metrics
            .write_prometheus(&mut output)
            .unwrap();
        assert!(output.contains("racer_worker_relay_used{worker=\"0\"} 1\n"));
        assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 17\n"));
        for (name, class) in [
            ("relay_limit", ResourceClass::Relay),
            ("ciphertext_limit_bytes", ResourceClass::Ciphertext),
        ] {
            assert!(output.contains(&format!(
                "racer_worker_{name}{{worker=\"0\"}} {}\n",
                worker.runtime.admission.limit(class)
            )));
        }
        drop((relay, ciphertext));
    }

    #[test]
    fn application_budget_poll_preserves_cooperative_and_completion_wakes() {
        let mut worker = wake_test_worker();
        // Exercise the production WorkerService entry point with side-effect-free
        // tasks. Stopping bypasses control publication and snapshot requirements.
        worker.started = true;
        worker.stopping = true;
        let count = Arc::new(crate::test_support::WakeCounter::default());
        let waker = std::task::Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let (send, receive) = futures::channel::oneshot::channel::<()>();
        worker.peer_task = Some(Box::pin(async move {
            receive.await.map_err(|_| Error::Unavailable)
        }));
        let mut yielded = false;
        worker.diagnostic_task = Some(Box::pin(std::future::poll_fn(move |cx| {
            if yielded {
                Poll::Ready(Ok(()))
            } else {
                yielded = true;
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })));
        worker.poll_budgeted(&mut cx, 0).unwrap();
        assert_eq!(count.count(), 0);
        worker.poll_budgeted(&mut cx, 1).unwrap();
        assert_eq!(count.count(), 1, "cooperative continuation reaches driver");
        worker.poll_budgeted(&mut cx, 1).unwrap();
        assert_eq!(count.count(), 1, "blocked task does not spin");
        std::thread::spawn(move || send.send(()).unwrap())
            .join()
            .unwrap();
        assert_eq!(
            count.count(),
            2,
            "registered completion wakes driver across threads"
        );
        worker.poll_budgeted(&mut cx, 1).unwrap();
        assert!(worker.peer_task.is_none());
        assert!(worker.diagnostic_task.is_none());
    }

    #[test]
    fn application_metadata_deadline_hook_is_budgeted_and_precedes_peer_polling() {
        use futures::{Stream, stream::FuturesUnordered};
        let mut worker = wake_test_worker();
        worker.started = true;
        worker.stopping = true;
        assert_eq!(
            Rc::strong_count(&worker.metadata),
            2,
            "worker and coordinator retain the same metadata service"
        );
        let count = Arc::new(crate::test_support::WakeCounter::default());
        let waker = std::task::Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut children = FuturesUnordered::new();
        for _ in 0..65 {
            children.push(crate::read::metadata::tests::deadline_probe(
                &worker.metadata,
                Instant::now(),
            ));
        }
        assert!(
            std::pin::Pin::new(&mut children)
                .poll_next(&mut cx)
                .is_pending()
        );
        let completed = Rc::new(std::cell::Cell::new(0));
        let observed = completed.clone();
        worker.peer_task = Some(Box::pin(std::future::poll_fn(move |cx| {
            while let Poll::Ready(Some(result)) = std::pin::Pin::new(&mut children).poll_next(cx) {
                assert_eq!(result, Err(Error::DeadlineExceeded));
                observed.set(observed.get() + 1);
            }
            Poll::Pending
        })));
        worker.poll_budgeted(&mut cx, 0).unwrap();
        assert_eq!(completed.get(), 0);
        worker.poll_budgeted(&mut cx, usize::MAX).unwrap();
        assert_eq!(
            completed.get(),
            64,
            "deadline hook clamps before polling peers"
        );
        worker.poll_budgeted(&mut cx, 1).unwrap();
        assert_eq!(completed.get(), 65);
        let settled = count.count();
        worker.poll_budgeted(&mut cx, 64).unwrap();
        assert_eq!(count.count(), settled);
    }

    #[test]
    fn composes_http_and_optional_rdma_without_operational_side_effects() {
        for enable_rdma in [false, true] {
            let config = crate::test_support::cluster::config(enable_rdma);
            let admission = Rc::new(Admission::new(config.limits.clone()));
            let (io, engine) = crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
            let crypto = Rc::new(CryptoClient::new(io));
            let runtime = WorkerRuntime {
                reactor: Rc::new(Reactor::new(admission.clone())),
                admission,
                crypto: crypto.clone(),
            };
            let application =
                Application::assemble(crate::test_support::cluster::config(enable_rdma)).unwrap();
            let _engine = application
                .build_crypto(WorkerId(0), CryptoRuntime { port: engine })
                .unwrap();
            let node = Arc::new(NodeState::default());
            let mut worker = WorkerApplication::assemble(
                &config,
                node.clone(),
                WorkerId(0),
                runtime,
                Vec::new(),
            )
            .expect("valid side-effect-free worker composition");
            assert!(worker.control.is_some());
            assert_eq!(worker.rdma.is_some(), enable_rdma);
            assert_eq!(
                Rc::strong_count(&worker.flights),
                2,
                "worker lifecycle and fill share one flight table"
            );
            assert_eq!(
                Rc::strong_count(&crypto),
                3,
                "runtime and page facade share one local crypto client"
            );
            let second_admission = Rc::new(Admission::new(config.limits.clone()));
            let (second_io, second_engine) =
                crypto::pair(WorkerId(1), 0, config.limits.queue_entries);
            let second_runtime = WorkerRuntime {
                reactor: Rc::new(Reactor::new(second_admission.clone())),
                admission: second_admission,
                crypto: Rc::new(CryptoClient::new(second_io)),
            };
            let _second_engine = application
                .build_crypto(
                    WorkerId(1),
                    CryptoRuntime {
                        port: second_engine,
                    },
                )
                .unwrap();
            let second = WorkerApplication::assemble(
                &config,
                node.clone(),
                WorkerId(1),
                second_runtime,
                Vec::new(),
            )
            .expect("valid second worker composition");
            assert!(
                second.control.is_none(),
                "only one worker enrolls and publishes"
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert_eq!(worker.poll_budgeted(&mut cx, 0), Err(Error::Unavailable));
            assert_eq!(worker.poll_budgeted(&mut cx, 1), Err(Error::Unavailable));
            // Admission bounds retained generations once across the node.
            let mut publication = crate::control::wire::decode_publication(include_bytes!(
                "control/testdata/publication.json"
            ))
            .unwrap();
            publication.cluster = config.cluster.clone();
            let mut requests = Vec::new();
            for version in 1..=config.limits.retained_snapshots.get() + 1 {
                publication.sequence.0 = version as u64;
                publication.membership_version.0 = version as u64;
                let snapshot = worker.snapshots.publish(publication.clone()).unwrap();
                requests.push(snapshot.membership.clone());
            }
            publication.sequence.0 += 1;
            publication.membership_version.0 += 1;
            assert!(matches!(
                worker.snapshots.publish(publication),
                Err(Error::Overloaded)
            ));
        }
    }

    #[test]
    fn shared_factory_is_send_and_sync_without_moving_worker_graphs() {
        fn shared<T: Send + Sync>() {}
        shared::<Application>();
        shared::<NodeState>();
    }

    #[test]
    fn multiworker_memberships_retire_after_request_leases_and_reuse_capacity() {
        use crate::{model::MembershipVersion, peer::PeerNetwork};
        let mut publication = crate::control::wire::decode_publication(include_bytes!(
            "control/testdata/publication.json"
        ))
        .unwrap();
        let local = publication.members[0].node.clone();
        let neighbor = publication.members[1].node.clone();
        let published = Arc::new(PublishedState::default());
        let store = SnapshotStore::new(publication.cluster.clone(), published.clone(), 1);
        let networks = [
            PeerNetwork::new(local.clone(), published.clone()).unwrap(),
            PeerNetwork::new(local, published).unwrap(),
        ];
        let mut publish = |sequence, version| {
            publication.sequence.0 = sequence;
            publication.membership_version.0 = version;
            store.publish(publication.clone())
        };
        let first = publish(1, 1).unwrap();
        // A delayed worker holds a publication, while a read starts from the
        // cache-only replacement. Both must own the same canonical generation.
        let cache_only = publish(2, 1).unwrap();
        assert!(Arc::ptr_eq(&first.membership, &cache_only.membership));
        let request = cache_only.membership.clone();
        let weak = Arc::downgrade(&request);
        drop(cache_only);
        let current = publish(3, 2).unwrap();
        for network in &networks {
            assert!(Arc::ptr_eq(
                &request,
                &network.membership(MembershipVersion(1)).unwrap()
            ));
            assert!(network.endpoint(&request, &neighbor).is_ok());
        }
        assert!(matches!(publish(4, 3), Err(Error::Overloaded)));
        // Even at capacity, arbitrarily many cache-only publications fit.
        for sequence in 4..30 {
            publish(sequence, 2).unwrap();
        }
        drop(current);
        drop(request);
        assert!(
            weak.upgrade().is_some(),
            "delayed worker still holds the generation"
        );
        drop(first);
        assert!(weak.upgrade().is_none());
        for network in &networks {
            assert!(network.membership(MembershipVersion(1)).is_err());
        }
        for version in 3..20 {
            let current = publish(version + 30, version).unwrap();
            for network in &networks {
                assert!(Arc::ptr_eq(
                    &current.membership,
                    &network.membership(MembershipVersion(version)).unwrap()
                ));
            }
        }
        // Only one worker observes an intermediate publication. Neither requires
        // an installation/retirement poll to resolve the next accepted version.
        publish(50, 20).unwrap();
        let delayed = networks[0].membership(MembershipVersion(20)).unwrap();
        publish(51, 21).unwrap();
        for worker in [1, 0] {
            assert!(networks[worker].membership(MembershipVersion(21)).is_ok());
        }
        drop(delayed);
        publish(52, 22).unwrap();
        assert!(networks[0].membership(MembershipVersion(20)).is_err());
    }
}
