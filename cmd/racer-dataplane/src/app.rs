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
        transport::{ControlTransport, ReactorControlIo},
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
        handshake::Handshake, relay::Relay, requester::Requester, server::PeerServer,
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

#[path = "app_health.rs"]
mod health;
#[path = "app_native.rs"]
mod native;
#[path = "app_recovery.rs"]
mod recovery;

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
}
#[derive(Default)]
struct CheckpointCut {
    shards: Vec<ShardImage>,
    result: Option<Result<()>>,
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
    let transport = ControlTransport::new(ControlEndpoint {
        url: config.control_endpoint.clone(),
        trust_bundle: config.trust_bundle.clone(),
    });
    transport.attach_io(io);
    // read_bundle only parses the projection. It does not install node-bound keys.
    let unresolved = Rc::new(Keyring::new(
        config.cluster.clone(),
        NodeId(String::new()),
        node.keys.clone(),
    ));
    let projection = SecretWatcher::new(config.secret_directory.clone(), unresolved);
    projection.attach_reactor(reactor.clone());
    let enrollment = Enrollment::new(
        config.cluster.clone(),
        config.service_account_token.clone(),
        config.identity_directory.clone(),
    );
    enrollment.attach_reactor(reactor.clone());
    let mut operation = Box::pin(async {
        let bundle = projection.read_bundle_async(startup).await?;
        enrollment.set_peer_trust_roots(bundle.peer_trust_roots.clone())?;
        let identity = match enrollment.load_identity_async(startup).await? {
            Some(identity) => identity,
            None => {
                let request = enrollment.prepare(startup).await?;
                let bytes = crate::control::wire::encode_enrollment_request(&request)?;
                let connection = transport.bootstrap(startup).await?;
                let token = enrollment.read_token_async(startup).await?;
                let response = connection
                    .request(
                        "POST",
                        crate::control::wire::BOOTSTRAP_PATH,
                        Some(&token),
                        &bytes,
                        crate::control::wire::MAX_ENROLLMENT_BYTES,
                        startup,
                    )
                    .await?;
                if response.status != 200 {
                    return Err(Error::Unauthorized);
                }
                enrollment
                    .accept_response_async(
                        crate::control::wire::decode_enrollment_response(&response.body)?,
                        startup,
                    )
                    .await?
            }
        };
        let keys = Keyring::new(
            config.cluster.clone(),
            identity.node().clone(),
            node.keys.clone(),
        );
        keys.install(bundle)?;
        keys.install_signing_identity(identity.signing_identity(&keys.peer_trust_roots()?)?)?;
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
            config.limits.retained_snapshots.get(),
        )?);
        let wire = Rc::new(crate::peer::wire::SecurityCodec::new(
            admission.clone(),
            buffers.clone(),
        ));
        let transfers = Transfers::new(http.clone(), io.clone(), rdma.clone())
            .with_wire(admission.clone(), wire.clone());
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
            OriginClient::new(snapshots.clone(), http, io.clone())
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
            metadata,
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
            .with_network(network.clone())
            .with_wire(wire)
            .with_handshake(handshake)
            .with_transfers(transfers)
            .with_reactor(reactor.clone()),
        );
        let io = Rc::new(HttpIo::for_clients(reactor, admission.clone()));
        let responses = Rc::new(Responses::new(io.clone(), delivery));
        let clients = Rc::new(ClientListeners::new(
            dispatcher.clone(),
            RequestParser::new(config.limits.header_bytes.get()),
            responses,
            io,
            admission,
        ));

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
            if let Some(control) = &self.control {
                let identity = control.start(startup).await?;
                if identity.node() != self.keys.node() {
                    return Err(Error::Unauthorized);
                }
                control.activate_identity()?;
                // Accept a complete compatible first snapshot before any listener.
                while self.snapshots.current().is_err() {
                    startup.check()?;
                    match control.progress(startup).await {
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
            self.start_diagnostics()?;
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
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                if let Some(result) = poll_task(&mut self.peer_task, &mut cx) {
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
        if self.snapshot_sequence == Some(snapshot.sequence) {
            return Ok(());
        }
        // Retire only network references with no outstanding request lease.
        self.memberships.retain(|membership| {
            if membership.version != snapshot.membership.version
                && Arc::strong_count(membership) == 2
            {
                self.network.retire(membership.version);
                false
            } else {
                true
            }
        });
        if !self
            .memberships
            .iter()
            .any(|m| m.version == snapshot.membership.version)
        {
            self.network.install(snapshot.membership.clone())?;
            self.memberships.push_back(snapshot.membership.clone());
        }
        for old in &self.caches {
            if !snapshot.caches.iter().any(|new| new.id == old.id) {
                self.memory.remove_cache(&old.id)?;
                self.store.writer.remove_cache(&old.id)?;
            }
        }
        if self.control.is_some() {
            self.clients
                .reconcile(&snapshot.caches, current_scope)
                .await?;
        }
        self.caches = snapshot.caches.clone();
        self.snapshot_sequence = Some(snapshot.sequence);
        Ok(())
    }

    async fn checkpoint(&self, deadline: &RequestScope) -> Result<()> {
        let node = self.node.as_ref().ok_or(Error::InvalidConfiguration)?;
        let image = match self.store.checkpoint.snapshot_shard().await {
            Ok(image) => image,
            Err(error) => {
                node.checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .result = Some(Err(error));
                return Err(error);
            }
        };
        node.checkpoint
            .lock()
            .map_err(|_| Error::Unavailable)?
            .shards
            .push(image);
        let result = std::future::poll_fn(|cx| {
            let mut cut = node.checkpoint.lock().map_err(|_| Error::Unavailable)?;
            if let Some(result) = cut.result {
                return Poll::Ready(result);
            }
            if let Err(error) = deadline.check() {
                cut.result = Some(Err(error));
                return Poll::Ready(Err(error));
            }
            if self.worker == node.control_worker && cut.shards.len() == node.count {
                let shards = std::mem::take(&mut cut.shards);
                drop(cut);
                let mut publication = self.store.checkpoint.publish(shards);
                let result = match publication.as_mut().poll(cx) {
                    Poll::Ready(result) => result,
                    Poll::Pending => Err(Error::InvalidConfiguration),
                };
                node.checkpoint
                    .lock()
                    .map_err(|_| Error::Unavailable)?
                    .result = Some(result);
                return Poll::Ready(result);
            }
            cx.waker().wake_by_ref();
            Poll::Pending
        })
        .await;
        self.store.checkpoint.finish_snapshot();
        result
    }

    fn poll_services(&mut self, work_budget: usize) -> Result<()> {
        if work_budget == 0 {
            return Ok(());
        }
        let budget = work_budget.min(64);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        if let Some(result) = poll_task(&mut self.diagnostic_task, &mut cx) {
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
            endpoint.poll(&mut cx, budget)?;
        }
        self.flights.poll_with_context(&mut cx, budget)?;
        self.clients.poll_budgeted(budget)?;
        if let Some(rdma) = &self.rdma {
            rdma.progress()?;
        }
        if let Some(result) = poll_task(&mut self.peer_task, &mut cx) {
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
        if let Some(result) = poll_task(&mut self.writer_task, &mut cx) {
            if !matches!(
                result,
                Err(Error::Io
                    | Error::Unavailable
                    | Error::Overloaded
                    | Error::MissingKey
                    | Error::Cancelled
                    | Error::DeadlineExceeded)
            ) {
                result?;
            }
        }
        if self.writer_task.is_none() && self.store.writer.pending_count() != 0 {
            let writer = self.store.writer.clone();
            let write_scope = scope(self.timeout)?;
            self.writer_task = Some(Box::pin(async move {
                writer.progress(1, &write_scope).await.map(|_| ())
            }));
        }
        if !self.stopping {
            if let Some(result) = poll_task(&mut self.control_task, &mut cx) {
                if !matches!(
                    result,
                    Err(Error::Io
                        | Error::Unavailable
                        | Error::Overloaded
                        | Error::DeadlineExceeded)
                ) {
                    result?;
                }
            }
            if self.control_task.is_none() {
                if let Some(control) = &self.control {
                    let control = control.clone();
                    let turn = scope(crate::control::wire::POLL_WAIT + Duration::from_secs(10))?;
                    self.control_task = Some(Box::pin(async move {
                        control.progress(&turn).await.map(|_| ())
                    }));
                }
            }
            let current_scope = scope(self.timeout)?;
            // Reconciliation currently performs only bounded synchronous staging;
            // retain a future if that contract becomes asynchronous.
            let mut refresh = Box::pin(self.refresh_snapshot(&current_scope));
            match refresh.as_mut().poll(&mut cx) {
                Poll::Ready(result) => result?,
                Poll::Pending => return Err(Error::InvalidConfiguration),
            }
        }
        self.observe_health()?;
        Ok(())
    }

    pub fn shutdown<'a>(&'a mut self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            if let Some(endpoint) = &mut self.endpoint {
                endpoint.uninstall()?;
            }
            self.endpoint.take();
            self.store.checkpoint.finish_snapshot();
            self.memberships.clear();
            self.started = false;
            self.telemetry
                .health
                .transition(crate::telemetry::health::State::Stopped)?;
            Ok(())
        })
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
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()> {
        if !self.started {
            return Err(Error::Unavailable);
        }
        if STOP_REQUESTED.load(Ordering::Relaxed) {
            return Err(Error::Cancelled);
        }
        self.poll_services(work_budget)
    }
    fn stop_admission(&mut self) -> Result<()> {
        self.stopping = true;
        self.telemetry
            .health
            .transition(crate::telemetry::health::State::Draining)?;
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
                if deadline.check().is_err() {
                    if let Err(error) = self.store.writer.cancel_pending_writes() {
                        first_error.get_or_insert(error);
                    }
                }
                if let Err(error) = self.poll_services(64) {
                    first_error.get_or_insert(error);
                }
                if !clients_done {
                    if let Poll::Ready(result) = client_drain.as_mut().poll(cx) {
                        if let Err(error) = result {
                            first_error.get_or_insert(error);
                        }
                        clients_done = true;
                    }
                }
                if !flights_done {
                    if let Poll::Ready(result) = flight_drain.as_mut().poll(cx) {
                        if let Err(error) = result {
                            first_error.get_or_insert(error);
                        }
                        flights_done = true;
                    }
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
mod tests {
    use super::*;
    use crate::runtime::{
        admission::Admission,
        crypto::{self, CryptoClient},
        reactor::Reactor,
    };

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
            assert_eq!(worker.poll_budgeted(0), Err(Error::Unavailable));
            assert_eq!(worker.poll_budgeted(1), Err(Error::Unavailable));
        }
    }

    #[test]
    fn shared_factory_is_send_and_sync_without_moving_worker_graphs() {
        fn shared<T: Send + Sync>() {}
        shared::<Application>();
        shared::<NodeState>();
    }

    // Implement startup rollback, control owner fanout, all-shard checkpoints,
    // multi-worker dispatch, and shutdown with active I/O alongside lifecycle code.
}
