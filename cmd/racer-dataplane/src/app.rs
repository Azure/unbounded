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
//! Cache removal applies the same ordering within that cache. Startup failures roll
//! back already-created resources. Constructors perform no operational I/O.

use crate::{
    client::{listener::ClientListeners, request::RequestParser, response::Responses},
    config::Config,
    control::{
        caches::CacheRegistry,
        client::{ControlClient, ControlEndpoint},
        enrollment::Enrollment,
        secrets::SecretWatcher,
        snapshot::{PublishedState, SnapshotStore},
        transport::ReactorControlIo,
    },
    error::{Error, Operation, Result},
    http::{io::HttpIo, pool::HttpPool},
    memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool},
    model::{
        identity::{NodeId, RequestId, WorkerId},
        limits::Limits,
    },
    origin::client::{Origin, OriginClient},
    peer::{
        PeerNetwork, handshake::Handshake, relay::Relay, requester::Requester, server::PeerServer,
        transfer::Transfers,
    },
    rdma::{
        device::Devices, permission::Permissions, registered::RegisteredPool, session::Sessions,
        transfer::RdmaTransfer, verbs::Verbs,
    },
    read::{
        candidates::CandidatePolicy,
        dispatch::{Dispatcher, WorkerDirectory, WorkerEndpoint},
        fill::{Fill, FillDependencies},
        flight::Flights,
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
        serve::Coordinator,
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
        certificates::Certificates,
        credentials::CredentialCrypto,
        forwarding::Forwarding,
        keyring::{KeyEpochs, KeyPurpose, Keyring},
        replay::{ReplayState, ReplayWindow},
        signing::Signatures,
    },
    store::{
        Store,
        checkpoint::Checkpointer,
        checkpoint_format::{CheckpointGeometry, ShardImage},
        eviction::SegmentClock,
        index::Index,
        reader::StoreReader,
        recovery::Recovery,
        segment::Segments,
        slab::Slabs,
        writer::StoreWriter,
    },
    telemetry::Telemetry,
    topology::{health::LinkHealth, paths::Paths, placement::Placement, rails::Rails},
};
use std::{
    collections::VecDeque,
    num::NonZeroUsize,
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::{Duration, Instant},
};

#[path = "app_caches.rs"]
mod caches;
#[path = "app_health.rs"]
mod health;
#[path = "app_native.rs"]
mod native;
#[path = "app_recovery.rs"]
mod recovery;
#[path = "app_retirement.rs"]
mod retirement;

pub struct Application {
    config: Arc<Config>,
    node: Arc<NodeState>,
    limits: Limits,
    fabric_ports: Vec<crate::rdma::device::FabricPort>,
}

/// Shared immutable-publication and partitioned-admission roots. No Rc worker
/// graph crosses a thread. Worker zero alone drives enrollment/control reloads.
pub struct NodeState {
    publications: Arc<PublishedState>,
    keys: Arc<KeyEpochs>,
    replay: Arc<ReplayState>,
    control_worker: WorkerId,
    workers: Arc<WorkerDirectory>,
    count: usize,
    prepared: AtomicUsize,
    checkpoint: Mutex<CheckpointCut>,
    recovery: Mutex<recovery::RecoveryCut>,
    observations: health::Observations,
    native: native::NativePairs,
    cache_cut: Mutex<caches::CacheCut>,
    retirement: Arc<retirement::Retirement>,
    membership_owners: Mutex<std::collections::HashMap<usize, usize>>,
}
#[derive(Default)]
struct CheckpointCut {
    shards: Vec<ShardImage>,
    result: Option<Result<()>>,
    publishing: bool,
}
impl Default for NodeState {
    fn default() -> Self {
        Self::new(vec![WorkerId(0), WorkerId(1)], 16).expect("valid default worker map")
    }
}
impl NodeState {
    fn new(workers: Vec<WorkerId>, capacity: usize) -> Result<Self> {
        let count = workers.len();
        let map = Arc::new(WorkerMap::new(workers.clone())?);
        Ok(Self {
            publications: Arc::new(PublishedState::default()),
            keys: Arc::new(KeyEpochs::default()),
            replay: Arc::new(ReplayState::default()),
            control_worker: WorkerId(0),
            workers: Arc::new(WorkerDirectory::new(map, workers, capacity)?),
            count,
            prepared: AtomicUsize::new(0),
            checkpoint: Mutex::new(CheckpointCut::default()),
            recovery: Mutex::new(recovery::RecoveryCut::default()),
            observations: health::Observations::default(),
            native: native::NativePairs::default(),
            cache_cut: Mutex::new(caches::CacheCut::default()),
            retirement: Arc::new(retirement::Retirement::new(count)),
            membership_owners: Mutex::new(std::collections::HashMap::new()),
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
        // A pair must retain complete page progress reserves. Use fewer pairs if
        // the configured node budget cannot support every discovered CPU pair.
        loop {
            match partition_limits(
                &self.config.limits,
                plan.pairs.len(),
                self.config.enable_rdma,
            ) {
                Ok(limits) => {
                    self.limits = limits;
                    break;
                }
                Err(_) if plan.pairs.len() > 1 => {
                    plan.pairs.pop();
                }
                Err(error) => return Err(error),
            }
        }
        self.node = Arc::new(NodeState::new(
            plan.pairs.iter().map(|p| p.worker).collect(),
            self.limits.queue_entries.get(),
        )?);
        if self.config.enable_rdma {
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
    RequestScope::new(RequestId([0; 16]), Instant::now() + timeout)
}

/// Divide aggregate resource dimensions, preserving per-operation protocol caps.
/// Replay is a single node-wide table, so every handle uses the same node cap.
fn partition_limits(node: &Limits, workers: usize, rdma: bool) -> Result<Limits> {
    if workers == 0 {
        return Err(Error::InvalidConfiguration);
    }
    let mut limits = node.clone();
    for value in [
        &mut limits.plaintext_bytes,
        &mut limits.ciphertext_bytes,
        &mut limits.dirty_bytes,
        &mut limits.request_context_bytes,
        &mut limits.flights,
        &mut limits.queue_entries,
        &mut limits.client_connections,
        &mut limits.pipes,
        &mut limits.cached_rankings,
        &mut limits.cached_paths,
        &mut limits.metadata_entries,
        &mut limits.relay_transfers,
    ] {
        *value = NonZeroUsize::new(value.get() / workers).ok_or(Error::InvalidConfiguration)?;
    }
    if rdma {
        limits.registered_bytes = NonZeroUsize::new(node.registered_bytes.get() / workers)
            .ok_or(Error::InvalidConfiguration)?;
    }
    let page = crate::model::range::PAGE_BYTES as usize;
    let window = limits.range_window_pages.get();
    if limits.plaintext_bytes.get() < (window + 1) * page
        || limits.ciphertext_bytes.get()
            < (window + 1) * (page + 16) + crate::store::format::MAX_HEADER_BYTES
        || limits.dirty_bytes.get() < page + 16
        || rdma && limits.registered_bytes.get() < page + 16
        || limits.request_context_bytes.get()
            < 4 * limits.header_bytes.get().max(crate::model::MAX_FIELD_BYTES)
        || limits.queue_entries.get() < 2
        || limits.client_connections < limits.connections_per_neighbor
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(limits)
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
    let projection = SecretWatcher::new(config.secret_directory.clone(), unresolved.clone());
    let enrollment = Rc::new(Enrollment::new(
        config.cluster.clone(),
        config.service_account_token.clone(),
        config.identity_directory.clone(),
    ));
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
        Rc::new(CacheRegistry),
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
                        control.next_attempt().unwrap_or_else(Instant::now),
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
        keys.register_retirement_barriers(node.retirement.clone())?;
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
    dispatcher: Rc<Dispatcher>,
    /// Same table as Fill; the worker drives abandoned work without user futures.
    flights: Rc<Flights>,
    rdma: Option<Rc<RdmaTransfer>>,
    devices: Option<Rc<Devices>>,
    fabric_ports: Vec<crate::rdma::device::FabricPort>,
    actual_rails: Vec<crate::topology::rails::RailMapping>,
    telemetry: Rc<Telemetry>,
    network: Rc<crate::peer::PeerNetwork>,
    directory: Arc<WorkerDirectory>,
    node: Option<Arc<NodeState>>,
    endpoint: Option<WorkerEndpoint>,
    control_task: Option<Operation<'static, ()>>,
    peer_task: Option<Operation<'static, ()>>,
    writer_task: Option<Operation<'static, ()>>,
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
    memberships: VecDeque<crate::topology::membership::MembershipLease>,
    memory: Rc<MemoryCache>,
    caches: Vec<crate::control::caches::CacheDefinition>,
    slab_directory: std::path::PathBuf,
    prepared_listeners: Rc<std::cell::RefCell<Option<crate::client::listener::PreparedListeners>>>,
    cache_prepare_task: Option<Operation<'static, ()>>,
    cache_preparing_generation: u64,
    control_scope: Option<RequestScope>,
    retiring: bool,
    retirement_registered: bool,
    retirement_native_started: bool,
    retirement_scope: Option<RequestScope>,
    retirement_native: Option<Operation<'static, ()>>,
    retirement_resume: Option<Operation<'static, ()>>,
    retirement_checkpoint: Option<Operation<'static, ()>>,
}

impl WorkerApplication {
    /// All constructors below only connect dependencies. Runtime commands route
    /// listener work to page/metadata owners before any acquisition is dispatched.
    pub fn assemble(
        config: &Config,
        node: &NodeState,
        worker: WorkerId,
        runtime: WorkerRuntime,
    ) -> Result<Self> {
        let admission = runtime.admission.clone();
        let reactor = runtime.reactor.clone();
        let limits = admission.limits();
        let snapshots = Rc::new(SnapshotStore::new(
            config.cluster.clone(),
            node.publications.clone(),
            config.limits.retained_snapshots.get(),
        ));
        let caches = Rc::new(CacheRegistry);
        let keys = Rc::new(Keyring::new(
            config.cluster.clone(),
            config.node.clone(),
            node.keys.clone(),
        ));
        let certificates = Rc::new(Certificates::new(config.cluster.clone(), keys.clone()));
        let replay = Rc::new(ReplayWindow::new(
            node.replay.clone(),
            config.limits.replay_entries.get(),
        ));
        let signatures = Rc::new(Signatures::new(
            keys.clone(),
            certificates.clone(),
            replay.clone(),
        ));
        let forwarding = Rc::new(Forwarding::new(signatures.clone()));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let crypto = Rc::new(PageCrypto::new(keys.clone(), runtime.crypto.clone()));
        let control = if worker == node.control_worker {
            let enrollment = Rc::new(Enrollment::new(
                config.cluster.clone(),
                config.service_account_token.clone(),
                config.identity_directory.clone(),
            ));
            let secrets = SecretWatcher::new(config.secret_directory.clone(), keys.clone());
            let control = Rc::new(ControlClient::new(
                ControlEndpoint {
                    url: config.control_endpoint.clone(),
                    trust_bundle: config.trust_bundle.clone(),
                },
                enrollment,
                keys.clone(),
                secrets,
                snapshots.clone(),
                caches,
            ));
            control.attach_io(Rc::new(ReactorControlIo::new(reactor.clone())));
            Some(control)
        } else {
            None
        };

        let http = Rc::new(HttpPool::new(
            reactor.clone(),
            admission.clone(),
            limits.connections_per_neighbor.get(),
        ));
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            crate::http::codec::Codec::new(
                config.limits.header_bytes.get(),
                crate::model::range::PAGE_BYTES + 16,
            ),
            admission.clone(),
        ));
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let pipes = Rc::new(PipePool::new(admission.clone(), reactor.clone()));
        let delivery = Rc::new(Delivery::new(pipes, config.reader_stall_timeout));

        let index = Rc::new(Index::new(worker, limits.metadata_entries.get()));
        let segments = Rc::new(Segments::new(worker, config.segment_bytes));
        let eviction = Rc::new(SegmentClock::new(
            index.clone(),
            segments.clone(),
            config.free_segment_reserve,
        ));
        let slabs = Rc::new(Slabs::new(
            worker,
            config.slab_directory.clone(),
            reactor.clone(),
            config.slab_bytes,
            config.segment_bytes,
        ));
        let disk = Rc::new(StoreReader::new(
            eviction.clone(),
            index.clone(),
            segments.clone(),
            slabs.clone(),
            buffers.clone(),
        ));
        let writer = Rc::new(StoreWriter::new(index.clone(), segments.clone(), slabs));
        writer.configure(
            admission.clone(),
            eviction.clone(),
            limits.queue_entries.get(),
            limits.metadata_entries.get(),
        )?;
        let store = Store {
            reader: disk.clone(),
            writer: writer.clone(),
            checkpoint: Checkpointer::new(
                config.slab_directory.clone(),
                index.clone(),
                segments.clone(),
            ),
            recovery: Recovery::new(config.slab_directory.clone(), index.clone(), segments),
            eviction,
        };

        let (sessions, rdma, devices) = if config.enable_rdma {
            let devices = Rc::new(Devices::new(Rc::new(Verbs)));
            if let Some(port) = node.native.io(worker)? {
                devices.attach(port)?;
            }
            let sessions = Rc::new(Sessions::new(
                devices.clone(),
                config.limits.connections_per_neighbor.get(),
            ));
            let registered = Rc::new(RegisteredPool::new(devices.clone(), admission.clone()));
            let transfer = Rc::new(RdmaTransfer::new(
                sessions.clone(),
                registered,
                Rc::new(Permissions),
            ));
            (Some(sessions), Some(transfer), Some(devices))
        } else {
            (None, None, None)
        };

        let paths = Rc::new(Paths::new(
            Rc::new(LinkHealth),
            limits.cached_paths.get(),
            config.limits.route_search_work.get(),
        ));
        let rails = Rc::new(Rails);
        let placement = Rc::new(Placement::new(limits.cached_rankings.get()));
        let network = Rc::new(crate::peer::PeerNetwork::new(
            config.node.clone(),
            // SnapshotStore bounds old generations separately from the current
            // publication. Every accepted generation must fit the routing table.
            config
                .limits
                .retained_snapshots
                .get()
                .checked_add(1)
                .ok_or(Error::InvalidConfiguration)?,
        )?);
        let wire = Rc::new(crate::peer::wire::SecurityCodec::new(
            admission.clone(),
            buffers.clone(),
        ));
        let transfers = Transfers::new(http.clone(), io.clone(), rdma.clone())
            .with_wire(admission.clone(), wire.clone())
            .with_receive_reclamation(memory.clone(), writer.clone());
        let transfers = Rc::new(match &sessions {
            Some(sessions) => transfers.with_native(signatures.clone(), sessions.clone()),
            None => transfers,
        });
        let handshake = Rc::new(
            Handshake::new(signatures.clone(), sessions)
                .with_http(network.clone(), transfers.clone())
                .with_discovery(keys.clone(), certificates, replay),
        );
        let requester = Rc::new(
            Requester::new(
                paths.clone(),
                rails,
                forwarding.clone(),
                handshake.clone(),
                transfers.clone(),
            )
            .with_network(network.clone()),
        );
        let candidates = Rc::new(CandidatePolicy::new(
            config.node.clone(),
            placement,
            requester.clone(),
        ));
        let origin: Rc<dyn Origin> = Rc::new(
            OriginClient::new(snapshots.clone(), http.clone(), io.clone())
                .with_buffers(admission.clone(), buffers.clone()),
        );
        let flights = Rc::new(Flights::new(admission.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers,
            disk,
            writer,
            peers: requester.clone(),
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights: flights.clone(),
            crypto,
            credentials: credentials.clone(),
            admission: admission.clone(),
            metadata_owner: node.workers.clone(),
        }));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            origin,
            requester.clone(),
            credentials.clone(),
            limits.metadata_entries.get(),
            MetadataDependencies {
                index,
                fill: fill.clone(),
                owners: node.workers.clone(),
            },
        ));
        let streams = Rc::new(RangeStreams::new(
            fill.clone(),
            node.workers.clone(),
            delivery.clone(),
            config.limits.range_window_pages.get(),
        ));
        let coordinator = Rc::new(Coordinator::new(
            snapshots.clone(),
            metadata.clone(),
            fill,
            streams,
            credentials,
        ));
        let relay = Rc::new(
            Relay::new(paths, forwarding.clone(), requester, admission.clone())
                .with_network(network.clone())
                .with_handshake(handshake.clone()),
        );
        let dispatcher = Rc::new(Dispatcher::new(
            worker,
            node.workers.clone(),
            coordinator.clone(),
        ));
        let peers = Rc::new(
            PeerServer::new(
                io.clone(),
                forwarding,
                admission.clone(),
                dispatcher.clone(),
                relay,
            )
            .with_request_timeout(config.request_timeout)
            .with_network(network.clone())
            .with_wire(wire)
            .with_handshake(handshake)
            .with_transfers(transfers)
            .with_reactor(reactor.clone()),
        );
        let io = Rc::new(HttpIo::for_clients(reactor, admission.clone()));
        let responses = Rc::new(Responses::new(io.clone(), delivery));
        let clients = Rc::new(
            ClientListeners::new(
                dispatcher.clone(),
                RequestParser::new(config.limits.header_bytes.get()),
                responses,
                io,
                admission,
            )
            .with_pool(http),
        );

        let mut telemetry = Telemetry::default();
        telemetry.health = node.observations.health.clone();
        Ok(Self {
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
            dispatcher,
            flights,
            rdma,
            devices,
            fabric_ports: Vec::new(),
            actual_rails: Vec::new(),
            telemetry: Rc::new(telemetry),
            network,
            directory: node.workers.clone(),
            node: None,
            endpoint: None,
            control_task: None,
            peer_task: None,
            writer_task: None,
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
            memberships: VecDeque::new(),
            memory,
            caches: Vec::new(),
            slab_directory: config.slab_directory.clone(),
            prepared_listeners: Rc::new(std::cell::RefCell::new(None)),
            cache_prepare_task: None,
            cache_preparing_generation: 0,
            control_scope: None,
            retiring: false,
            retirement_registered: false,
            retirement_native_started: false,
            retirement_scope: None,
            retirement_native: None,
            retirement_resume: None,
            retirement_checkpoint: None,
        })
    }

    pub fn start<'a>(&'a mut self, startup: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            startup.check()?;
            if self.started || self.stopping {
                return Err(Error::InvalidConfiguration);
            }
            // The authenticated node identity was installed before this graph existed.
            self.keys.signing_identity()?;
            let alignment = self.store.writer.open().await?;
            let slabs = self.store.writer.slabs();
            let geometry = CheckpointGeometry::new(
                slabs.slab_bytes(),
                slabs.segment_bytes(),
                slabs.slab_bytes() / slabs.segment_bytes(),
                alignment,
            )?;
            self.store.recovery.configure_geometry(geometry)?;
            self.store.checkpoint.configure_geometry(geometry)?;
            self.recover_node(geometry, startup).await?;
            self.endpoint = Some(
                self.directory
                    .install(self.worker, self.coordinator.clone())?,
            );
            let node = self
                .node
                .as_ref()
                .ok_or(Error::InvalidConfiguration)?
                .clone();
            node.prepared.fetch_add(1, Ordering::Release);
            self.attach_cache_adapter();
            if let Some(control) = self.control.clone() {
                let identity = control.start(startup).await?;
                if identity.node() != self.keys.node() {
                    return Err(Error::Unauthorized);
                }
                control.activate_identity()?;
                // Accept a complete compatible first snapshot before any listener.
                while self.snapshots.current().is_err() {
                    startup.check()?;
                    let mut progress = control.progress(startup);
                    let result = std::future::poll_fn(|cx| {
                        self.poll_cache_preparation(cx)?;
                        progress.as_mut().poll(cx)
                    })
                    .await;
                    match result {
                        Ok(_) => (),
                        Err(
                            Error::Io
                            | Error::Unavailable
                            | Error::Overloaded
                            | Error::DeadlineExceeded,
                        ) => {
                            let io = ReactorControlIo::new(self.runtime.reactor.clone());
                            crate::control::transport::ControlIo::sleep(
                                &io,
                                Instant::now() + Duration::from_millis(100),
                                startup,
                            )
                            .await?;
                        }
                        Err(error) => return Err(error),
                    }
                }
            }
            std::future::poll_fn(|cx| {
                startup.check()?;
                self.poll_cache_preparation(cx)?;
                if STOP_REQUESTED.load(Ordering::Relaxed) {
                    return Poll::Ready(Err(Error::Cancelled));
                }
                if node.prepared.load(Ordering::Acquire) == node.count
                    && self.snapshots.current().is_ok()
                {
                    Poll::Ready(Ok(()))
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await?;
            self.refresh_snapshot(startup).await?;
            self.activate_native(startup).await?;
            std::future::poll_fn(|cx| Poll::Ready(self.start_diagnostics(cx))).await?;
            self.task_scope = Some(scope(Duration::from_secs(365 * 24 * 3600))?);
            if self.control.is_some() {
                let listener_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
                let peers = self.peers.clone();
                let peer_scope = listener_scope.clone();
                let address = self.peer_address;
                self.peer_task = Some(Box::pin(
                    async move { peers.listen(address, &peer_scope).await },
                ));
                self.listener_scope = Some(listener_scope);
                // The first poll actually binds the socket. A failed bind is a
                // startup error, never a successful readiness transition.
                if let Some(result) =
                    std::future::poll_fn(|cx| Poll::Ready(poll_task(&mut self.peer_task, cx))).await
                {
                    result?;
                    return Err(Error::Unavailable);
                }
            }
            self.started = true;
            self.observe_health()?;
            Ok(())
        })
    }

    async fn refresh_snapshot(&mut self, current_scope: &RequestScope) -> Result<()> {
        let snapshot = self.snapshots.current()?;
        if !self.actual_rails.is_empty() {
            let compatible = snapshot
                .membership
                .member(self.keys.node())
                .is_ok_and(|member| {
                    member.alignment_enabled
                        && self.actual_rails.iter().all(|actual| {
                            member.rails.iter().any(|published| {
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
        let node = self.node.as_ref().ok_or(Error::InvalidConfiguration)?;
        update_memberships(
            &self.network,
            &mut self.memberships,
            &node.membership_owners,
            &snapshot.membership,
        )?;
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

    async fn checkpoint(&self, deadline: &RequestScope) -> Result<()> {
        let node = self.node.as_ref().ok_or(Error::InvalidConfiguration)?;
        let mut image = match self.store.checkpoint.snapshot_shard().await {
            Ok(image) => image,
            Err(error) => {
                node.checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .result = Some(Err(error));
                return Err(error);
            }
        };
        image.index.entries.retain(|(page, entry)| {
            self.keys
                .lease(
                    Some(&page.version.object.cache),
                    entry.key_id,
                    KeyPurpose::Page,
                )
                .is_ok()
        });
        node.checkpoint
            .lock()
            .map_err(|_| Error::Unavailable)?
            .shards
            .push(image);
        let mut publication: Option<Operation<'_, ()>> = None;
        let result = std::future::poll_fn(|cx| {
            if let Some(publish) = publication.as_mut() {
                if let Poll::Ready(result) = std::pin::Pin::as_mut(publish).poll(cx) {
                    node.checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .result = Some(result);
                    return Poll::Ready(result);
                }
                return Poll::Pending;
            }
            let mut cut = node.checkpoint.lock().map_err(|_| Error::Unavailable)?;
            if let Some(result) = cut.result {
                return Poll::Ready(result);
            }
            if cut.publishing {
                cx.waker().wake_by_ref();
                return Poll::Pending;
            }
            if let Err(error) = deadline.check() {
                cut.result = Some(Err(error));
                return Poll::Ready(Err(error));
            }
            if self.worker == node.control_worker && cut.shards.len() == node.count {
                cut.publishing = true;
                let shards = std::mem::take(&mut cut.shards);
                drop(cut);
                publication = Some(self.store.checkpoint.publish(shards));
            }
            cx.waker().wake_by_ref();
            Poll::Pending
        })
        .await;
        self.store.checkpoint.finish_snapshot();
        result
    }

    fn poll_services(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        if work_budget == 0 {
            return Ok(());
        }
        let budget = work_budget.min(64);
        self.peers.poll_admission_deadlines();
        self.metadata.poll_deadlines(Instant::now(), budget);
        if self.stopping {
            for task in [&mut self.retirement_native, &mut self.retirement_checkpoint] {
                if let Some(result) = poll_task(task, cx) {
                    result?;
                }
            }
        }
        if !self.stopping && self.poll_retirement(cx)? {
            self.observe_health()?;
            return Ok(());
        }
        if !self.stopping {
            self.poll_cache_preparation(cx)?;
        }
        if let Some(result) = poll_task(&mut self.diagnostic_task, cx) {
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
            endpoint.poll(cx, budget)?;
        }
        self.flights.poll_with_context(cx, budget)?;
        self.clients.poll_budgeted(cx, budget)?;
        if let Some(rdma) = &self.rdma {
            rdma.progress()?;
        }
        if let Some(result) = poll_task(&mut self.peer_task, cx) {
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
        if self.writer_task.is_none() && self.store.writer.pending_count() != 0 {
            let writer = self.store.writer.clone();
            let write_scope = scope(self.timeout)?;
            self.writer_task = Some(Box::pin(async move {
                writer.progress(1, &write_scope).await.map(|_| ())
            }));
        }
        if !self.stopping {
            self.poll_control(cx)?;
            let current_scope = scope(self.timeout)?;
            let mut refresh = Box::pin(self.refresh_snapshot(&current_scope));
            match refresh.as_mut().poll(cx) {
                Poll::Ready(result) => result?,
                Poll::Pending => return Err(Error::InvalidConfiguration),
            }
        }
        self.observe_health()?;
        Ok(())
    }

    fn poll_control(&mut self, cx: &mut Context<'_>) -> Result<()> {
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

    pub fn shutdown<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            if let Some(endpoint) = &mut self.endpoint {
                endpoint.uninstall()?;
            }
            self.endpoint.take();
            self.store.checkpoint.finish_snapshot();
            if let Some(node) = &self.node {
                let mut owners = node
                    .membership_owners
                    .lock()
                    .map_err(|_| Error::Unavailable)?;
                for membership in self.memberships.drain(..) {
                    self.network.retire(membership.version);
                    if let Some(count) = owners.get_mut(&(Arc::as_ptr(&membership) as usize)) {
                        *count -= 1;
                    }
                }
                owners.retain(|_, count| *count != 0);
            }
            self.started = false;
            self.telemetry
                .health
                .transition(crate::telemetry::health::State::Stopped)?;
            Ok(())
        })
    }
}

fn update_memberships(
    network: &PeerNetwork,
    memberships: &mut VecDeque<crate::topology::membership::MembershipLease>,
    owners: &Mutex<std::collections::HashMap<usize, usize>>,
    current: &crate::topology::membership::MembershipLease,
) -> Result<()> {
    // Each installed worker owns exactly two structural references: its queue and
    // its network. Serialize their accounting across workers; all other references
    // are publication or request leases and prevent retirement.
    let mut owners = owners.lock().map_err(|_| Error::Unavailable)?;
    memberships.retain(|membership| {
        let count = owners
            .get_mut(&(Arc::as_ptr(membership) as usize))
            .expect("installed membership owner");
        if membership.version != current.version && Arc::strong_count(membership) == 2 * *count {
            network.retire(membership.version);
            *count -= 1;
            false
        } else {
            true
        }
    });
    owners.retain(|_, count| *count != 0);
    if !memberships
        .iter()
        .any(|membership| membership.version == current.version)
    {
        network.install(current.clone())?;
        memberships.push_back(current.clone());
        *owners.entry(Arc::as_ptr(current) as usize).or_default() += 1;
    }
    Ok(())
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
        let mut application =
            WorkerApplication::assemble(&self.config, &self.node, worker, runtime)?;
        application.node = Some(self.node.clone());
        application.fabric_ports = self.fabric_ports.clone();
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
        self.stopping = true;
        self.cache_prepare_task.take();
        self.prepared_listeners.borrow_mut().take();
        self.retirement_resume.take();
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
        Ok(())
    }
    fn drain<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
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
                    && self.writer_task.is_none()
                    && self.store.writer.is_idle()
                    && self.retirement_native.is_none()
                    && self.retirement_checkpoint.is_none()
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
                if let Some(node) = &self.node {
                    node.checkpoint
                        .lock()
                        .map_err(|_| Error::Unavailable)?
                        .result = Some(Err(error));
                }
                return Err(error);
            }
            if self.started {
                self.checkpoint(&deadline).await?;
            }
            Ok(())
        })
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        WorkerApplication::shutdown(self, scope)
    }
}

#[cfg(test)]
#[path = "app_integration_tests.rs"]
mod integration_tests;

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::runtime::{
        admission::Admission,
        crypto::{self, CryptoClient},
        reactor::Reactor,
    };

    pub(crate) fn wake_test_worker() -> WorkerApplication {
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(Admission::new(config.limits.clone()));
        let (io, _engine) = crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
        WorkerApplication::assemble(
            &config,
            &NodeState::default(),
            WorkerId(0),
            WorkerRuntime {
                reactor: Rc::new(Reactor::new(admission.clone())),
                admission,
                crypto: Rc::new(CryptoClient::new(io)),
            },
        )
        .unwrap()
    }

    pub(crate) fn wake_test_coordinator() -> Rc<Coordinator> {
        wake_test_worker().coordinator
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
            let node = NodeState::default();
            let mut worker = WorkerApplication::assemble(&config, &node, WorkerId(0), runtime)
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
            let second = WorkerApplication::assemble(&config, &node, WorkerId(1), second_runtime)
                .expect("valid second worker composition");
            assert!(
                second.control.is_none(),
                "only one worker enrolls and publishes"
            );
            assert!(
                Rc::strong_count(&worker.dispatcher) == 3
                    && Rc::strong_count(&worker.coordinator) == 2,
                "client and peer share one dispatcher and local coordinator"
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert_eq!(worker.poll_budgeted(&mut cx, 0), Err(Error::Unavailable));
            assert_eq!(worker.poll_budgeted(&mut cx, 1), Err(Error::Unavailable));
            // Hold every old generation at the configured limit while installing
            // the current generation on both actual application networks.
            for network in [&worker.network, &second.network] {
                let mut requests = Vec::new();
                for version in 0..=config.limits.retained_snapshots.get() {
                    let membership = Arc::new(
                        crate::topology::membership::Membership::validate(
                            crate::model::identity::MembershipVersion(version as u64 + 1),
                            vec![],
                        )
                        .unwrap(),
                    );
                    network.install(membership.clone()).unwrap();
                    requests.push(membership);
                }
                assert_eq!(requests.len(), config.limits.retained_snapshots.get() + 1);
                let extra = Arc::new(
                    crate::topology::membership::Membership::validate(
                        crate::model::identity::MembershipVersion(requests.len() as u64 + 1),
                        vec![],
                    )
                    .unwrap(),
                );
                assert_eq!(network.install(extra), Err(Error::Overloaded));
            }
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
        use crate::{model::identity::MembershipVersion, topology::membership::Membership};
        let owners = Mutex::new(std::collections::HashMap::new());
        let publication = crate::control::wire::decode_publication(include_bytes!(
            "control/testdata/publication.json"
        ))
        .unwrap();
        let local = publication.members[0].node.clone();
        let networks = [
            PeerNetwork::new(local.clone(), 2).unwrap(),
            PeerNetwork::new(local, 2).unwrap(),
        ];
        let mut queues = [VecDeque::new(), VecDeque::new()];
        let membership = |version| {
            Arc::new(
                Membership::validate(MembershipVersion(version), publication.members.clone())
                    .unwrap(),
            )
        };
        let first = membership(1);
        for worker in 0..2 {
            update_memberships(&networks[worker], &mut queues[worker], &owners, &first).unwrap();
        }
        let request = first.clone();
        drop(first);
        let mut current = membership(2);
        for worker in 0..2 {
            update_memberships(&networks[worker], &mut queues[worker], &owners, &current).unwrap();
            assert!(networks[worker].membership(MembershipVersion(1)).is_ok());
        }
        drop(request);
        for worker in 0..2 {
            update_memberships(&networks[worker], &mut queues[worker], &owners, &current).unwrap();
            assert!(networks[worker].membership(MembershipVersion(1)).is_err());
        }
        for version in 3..20 {
            current = membership(version);
            for worker in 0..2 {
                update_memberships(&networks[worker], &mut queues[worker], &owners, &current)
                    .unwrap();
                assert_eq!(queues[worker].len(), 1);
            }
            assert_eq!(owners.lock().unwrap().len(), 1);
        }
        // Only one worker installs an intermediate publication. Ownership is
        // counted per installed object, never inferred from the worker total.
        current = membership(20);
        update_memberships(&networks[0], &mut queues[0], &owners, &current).unwrap();
        current = membership(21);
        for worker in [1, 0] {
            update_memberships(&networks[worker], &mut queues[worker], &owners, &current).unwrap();
            assert_eq!(queues[worker].len(), 1);
        }
        assert_eq!(owners.lock().unwrap().len(), 1);
    }
}
