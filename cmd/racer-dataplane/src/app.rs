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
        BundleInstaller, ControlClient, ControlEndpoint,
        enrollment::Enrollment,
        state::{PublishedState, SnapshotStore},
        transport::ReactorControlIo,
    },
    error::{Error, Operation, Result},
    http::{HttpIo, HttpPool},
    memory::{BufferPool, cache::MemoryCache, delivery::Delivery, new_pipe_pool},
    model::{Limits, NodeId, RequestId, WorkerId},
    origin::{Origin, OriginClient},
    peer::{Relay, Requester, server::PeerServer, transport::Transfers},
    rdma::{Devices, Sessions},
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
        admission::{AdmissionExt, AdmissionPolicy},
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
        connection::Signatures,
        credentials::CredentialCrypto,
        forwarding::Forwarding,
    },
    store::{
        Store, StoreReader, StoreWriter,
        catalog::{Index, SegmentClock},
        checkpoint::{CheckpointGeometry, Checkpointer, Recovery, ShardImage},
    },
    telemetry::Telemetry,
    topology::{
        health::LinkHealth,
        routing::{Paths, Placement},
    },
};
use racer_identity::{Certificates, KeyEpochs, KeyPurpose, Keyring};
use std::{
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::Duration,
};

pub(crate) mod caches {
    use super::*;
    use crate::{
        client::listener::PreparedListeners,
        control::state::{CacheDefinition, CacheTransition},
        runtime::collections::HashSet,
    };
    use std::cell::RefCell;

    #[derive(Default)]
    pub(super) struct CacheCut {
        generation: u64,
        pub(super) definitions: Vec<CacheDefinition>,
        prepared: HashSet<WorkerId>,
        pub(super) committed: bool,
    }
    pub(crate) struct CachePublication {
        pub(super) node: Arc<NodeState>,
        pub(super) listeners: Rc<RefCell<Option<PreparedListeners>>>,
        pub(super) capacity: usize,
    }
    struct Transition {
        node: Arc<NodeState>,
        generation: u64,
        listeners: Option<PreparedListeners>,
        committed: bool,
    }
    impl CacheTransition for Transition {
        fn commit(mut self: Box<Self>) {
            if let Some(listeners) = self.listeners.take() {
                listeners.commit();
            }
            let mut cut = self
                .node
                .cache_cut
                .lock()
                .unwrap_or_else(|e| e.into_inner());
            assert_eq!(cut.generation, self.generation);
            cut.committed = true;
            self.committed = true;
        }
    }
    impl Drop for Transition {
        fn drop(&mut self) {
            if !self.committed {
                let mut cut = self
                    .node
                    .cache_cut
                    .lock()
                    .unwrap_or_else(|e| e.into_inner());
                if cut.generation == self.generation && !cut.committed {
                    cut.prepared.remove(&self.node.control_worker);
                }
            }
        }
    }
    impl CachePublication {
        pub(crate) fn stage(
            &self,
            definitions: &[CacheDefinition],
        ) -> Result<Box<dyn CacheTransition>> {
            crate::control::state::validate_definitions(definitions)?;
            let mut cut = self.node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
            if cut.generation == 0 || cut.definitions != definitions {
                if definitions.len() > self.capacity {
                    return Err(Error::Overloaded);
                }
                self.listeners.borrow_mut().take();
                cut.generation = cut.generation.checked_add(1).ok_or(Error::Unavailable)?;
                cut.definitions = definitions.to_vec();
                cut.prepared.clear();
                cut.committed = false;
                return Err(Error::Unavailable);
            }
            if cut.prepared.len() != self.node.count {
                return Err(Error::Unavailable);
            }
            let listeners = if cut.committed {
                None
            } else {
                let listeners = self
                    .listeners
                    .borrow_mut()
                    .take()
                    .ok_or(Error::Unavailable)?;
                if listeners.definitions() != definitions {
                    return Err(Error::Unavailable);
                }
                Some(listeners)
            };
            Ok(Box::new(Transition {
                node: self.node.clone(),
                generation: cut.generation,
                listeners,
                committed: false,
            }))
        }
    }
    impl WorkerApplication {
        pub(super) fn attach_cache_adapter(&self) {
            if let Some(control) = &self.control {
                control.attach_cache_publication(Rc::new(CachePublication {
                    node: self.node.clone(),
                    listeners: self.prepared_listeners.clone(),
                    capacity: self.runtime.admission.limits().metadata_entries.get(),
                }));
            }
        }
        pub(super) fn poll_cache_preparation(&mut self, cx: &mut Context<'_>) -> Result<()> {
            let node = self.node.clone();
            let (generation, definitions) = {
                let cut = node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
                if cut.generation == 0 || cut.committed || cut.prepared.contains(&self.worker) {
                    return Ok(());
                }
                (
                    cut.generation,
                    if self.cache_prepare_task.is_none()
                        || self.cache_preparing_generation != cut.generation
                    {
                        cut.definitions.clone()
                    } else {
                        Vec::new()
                    },
                )
            };
            if self.cache_preparing_generation != generation {
                self.cache_prepare_task.take();
                self.prepared_listeners.borrow_mut().take();
                self.cache_preparing_generation = generation;
            }
            if node
                .cache_cut
                .lock()
                .map_err(|_| Error::Unavailable)?
                .prepared
                .contains(&self.worker)
            {
                return Ok(());
            }
            // Reject publications exceeding this worker's metadata catalog.
            if definitions.len() > self.runtime.admission.limits().metadata_entries.get() {
                return Ok(());
            }
            if self.control.is_some() && self.prepared_listeners.borrow().is_none() {
                if self.cache_prepare_task.is_none() {
                    let clients = self.clients.clone();
                    let prepared = self.prepared_listeners.clone();
                    let startup = scope(self.timeout)?;
                    self.cache_prepare_task = Some(Box::pin(async move {
                        *prepared.borrow_mut() =
                            Some(clients.prepare(&definitions, &startup).await?);
                        Ok(())
                    }));
                }
                if let Some(result) = poll_task(&mut self.cache_prepare_task, cx) {
                    match result {
                        Ok(())
                        | Err(
                            Error::Io
                            | Error::Overloaded
                            | Error::Unavailable
                            | Error::DeadlineExceeded,
                        ) => (),
                        Err(error) => return Err(error),
                    }
                }
                if self.prepared_listeners.borrow().is_none() {
                    return Ok(());
                }
            }
            let mut cut = node.cache_cut.lock().map_err(|_| Error::Unavailable)?;
            if cut.generation == generation {
                cut.prepared.insert(self.worker);
            }
            Ok(())
        }
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        #[test]
        fn publication_waits_for_every_worker_and_failed_acceptance_rolls_back_preparation() {
            let config = crate::test_support::cluster::config(false);
            let node = Arc::new(NodeState::default());
            let (mut worker, _, _) = crate::app::tests::local_worker(&config, &node, 0);
            let (mut second, _, _) = crate::app::tests::local_worker(&config, &node, 1);
            let adapter = CachePublication {
                node: node.clone(),
                listeners: worker.prepared_listeners.clone(),
                capacity: config.limits.metadata_entries.get(),
            };
            assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            worker.poll_cache_preparation(&mut cx).unwrap();
            assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
            assert!(worker.prepared_listeners.borrow().is_some());
            second.poll_cache_preparation(&mut cx).unwrap();
            drop(adapter.stage(&[]).unwrap());
            assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
            worker.poll_cache_preparation(&mut cx).unwrap();
            for _ in 0..2 {
                adapter.stage(&[]).unwrap().commit();
                assert!(node.cache_cut.lock().unwrap().committed);
            }
        }
        #[test]
        fn capacity_failure_keeps_last_good_generation_and_uid_reuse_needs_no_tombstones() {
            let config = crate::test_support::cluster::config(false);
            let node = Arc::new(NodeState::default());
            let definition = crate::app::tests::definition();
            let store = SnapshotStore::new(config.cluster.clone(), node.publications.clone(), 16);
            store
                .publish(crate::app::tests::publication(
                    &config,
                    1,
                    vec![definition.clone()],
                ))
                .unwrap();
            let mut adapter = CachePublication {
                node: node.clone(),
                listeners: Rc::new(RefCell::new(None)),
                capacity: 0,
            };
            assert!(matches!(
                adapter.stage(&[definition.clone()]),
                Err(Error::Overloaded)
            ));
            assert_eq!(node.cache_cut.lock().unwrap().generation, 0);
            adapter.capacity = 16;
            assert!(matches!(adapter.stage(&[]), Err(Error::Unavailable)));
            let previous = node.cache_cut.lock().unwrap().generation;
            assert!(matches!(
                adapter.stage(&[definition]),
                Err(Error::Unavailable)
            ));
            assert_eq!(node.cache_cut.lock().unwrap().generation, previous + 1);
            assert_eq!(
                store.cursor().unwrap(),
                Some(racer_control_wire::PublicationSequence(1))
            );
        }
    }
}
mod lifecycle;
mod native;
mod recovery;
#[cfg(test)]
use lifecycle::partition_limits_with_cause;
use lifecycle::{log_worker_plan, size_workers};

pub struct Application {
    config: Arc<Config>,
    node: Arc<NodeState>,
    limits: Limits,
    discovered_nics: Vec<crate::topology::rails::RailMapping>,
}

/// Shared immutable-publication and partitioned-admission roots. No Rc worker
/// graph crosses a thread. Worker zero alone drives enrollment/control reloads.
pub struct NodeState {
    inventory: Arc<crate::rdma::discovery::Inventory>,
    send_crc: crate::telemetry::send_crc::Samples,
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
            inventory: crate::rdma::discovery::Inventory::shared(),
            send_crc: Default::default(),
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
            discovered_nics: Vec::new(),
        })
    }

    pub fn run(mut self) -> Result<()> {
        self.config.validate()?;
        self.discovered_nics = crate::rdma::discovery::inventory();
        let mut plan = AffinityPlan::discover(&self.config)?;
        log_worker_plan("planned", &plan);
        self.limits = size_workers(&self.config.limits, &mut plan, self.config.enable_rdma)?;
        log_worker_plan("final", &plan);
        self.node = Arc::new(NodeState::with_peer_admission(
            plan.pairs.iter().map(|p| p.worker).collect(),
            self.limits.queue_entries.get(),
            self.config.peer_admission,
        )?);
        self.node.inventory.update(self.discovered_nics.clone())?;
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
        uring_runtime::environment::now() + timeout,
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
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        limits.clone(),
    )));
    let reactor = Rc::new(Reactor::new(admission));
    let unresolved = Rc::new(Keyring::new(
        config.cluster.clone(),
        NodeId(String::new()),
        node.keys.clone(),
    ));
    let control = WorkerApplication::assemble_control(
        config,
        reactor.clone(),
        unresolved,
        Rc::new(SnapshotStore::new(
            config.cluster.clone(),
            node.publications.clone(),
            config.limits.retained_snapshots.get(),
        )),
        node.inventory.clone(),
    );
    let mut operation = Box::pin(async {
        let identity = loop {
            match control.start(startup).await {
                Ok(identity) => break identity,
                Err(
                    Error::Io | Error::Unavailable | Error::Overloaded | Error::DeadlineExceeded,
                ) => {
                    startup.check()?;
                    let io = ReactorControlIo::new(reactor.clone());
                    ReactorControlIo::sleep(
                        &io,
                        control
                            .next_attempt()
                            .unwrap_or_else(uring_runtime::environment::now),
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
    environment: uring_runtime::environment::Environment,
    drivers: Rc<uring_runtime::drivers::DriverQueue>,
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
    rdma: Option<Rc<Sessions>>,
    devices: Option<Rc<Devices>>,
    discovered_nics: Vec<crate::topology::rails::RailMapping>,
    actual_rails: Vec<crate::topology::rails::RailMapping>,
    inventory_generation: u64,
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
    snapshot_sequence: Option<racer_control_wire::PublicationSequence>,
    memory: Rc<MemoryCache>,
    caches: Vec<crate::control::state::CacheDefinition>,
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
        discovered_nics: Vec<crate::topology::rails::RailMapping>,
    ) -> Result<Self> {
        let environment = uring_runtime::environment::Environment::current();
        let metrics = node
            .metrics
            .iter()
            .find(|(id, _)| *id == worker)
            .map(|(_, metrics)| metrics.clone())
            .ok_or(Error::InvalidConfiguration)?;
        runtime.crypto.set_metrics(metrics.clone());
        runtime
            .crypto
            .set_failure_observer(node.failures.observer(worker));
        let drivers = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
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
        metrics.observe_admission(worker, admission.shared())?;
        node.ingress.install(worker, &admission)?;
        let reactor = runtime.reactor.clone();
        let limits = admission.limits();
        let snapshots = Rc::new(SnapshotStore::new(
            config.cluster.clone(),
            node.publications.clone(),
            config.limits.retained_snapshots.get(),
        ));
        let keys = Rc::new(Keyring::new(
            config.cluster.clone(),
            config.node.clone(),
            node.keys.clone(),
        ));
        let certificates = Rc::new(Certificates::new(config.cluster.clone(), keys.clone()));
        let availability = Rc::new(crate::control::state::Availability::new(
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
                runtime.reactor.clone(),
                keys.clone(),
                snapshots.clone(),
                node.inventory.clone(),
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
            .with_peer_tcp_nodelay(config.peer_tcp_nodelay)
            .with_origin_limit(
                config
                    .origin_connections_per_cache
                    .get()
                    .min(limits.client_connections.get()),
            ),
        );
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            crate::http::Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            crate::model::PAGE_BYTES + 16,
        ));
        let buffers = BufferPool::new(admission.clone());
        let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
        let pipes = Rc::new(new_pipe_pool(admission.clone()));
        let delivery = Rc::new(
            Delivery::new(pipes.clone(), reactor.clone(), config.reader_stall_timeout)
                .with_metrics(metrics.clone()),
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
            (Some(sessions.clone()), Some(sessions), Some(devices))
        } else {
            (None, None, None)
        };

        let paths = Rc::new(
            Paths::new(Rc::new(LinkHealth), limits.cached_paths.get())
                .with_peer_admission(node.peer_admission.clone()),
        );
        let placement = Rc::new(Placement::with_memory_budget(
            limits.cached_rankings.get() * crate::topology::routing::RANKING_BYTES,
        ));
        let network = Rc::new(crate::peer::PeerNetwork::new(
            config.node.clone(),
            node.publications.clone(),
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
            .with_metrics(metrics.clone())
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
        let flights = Rc::new(Flights::new(admission.clone(), availability.clone()));
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
                crypto: crypto.clone(),
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
        let distributed = uring_runtime::reactor::simulation::Simulation::current().is_none();
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
                tcp_nodelay: config.peer_tcp_nodelay,
                accept: if distributed {
                    crate::peer::server::AcceptMode::Distributed(node.ingress.clone())
                } else {
                    crate::peer::server::AcceptMode::Local
                },
                request_timeout: config.request_timeout,
                opaque_relay: config.opaque_relay,
            },
        )
        .with_metrics(metrics.clone());
        let peers = Rc::new(peers.with_send_crc(
            config.send_crc_pair.clone(),
            node.send_crc.clone(),
            crypto,
        ));
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
        telemetry.send_crc = node.send_crc.clone();
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
            discovered_nics,
            actual_rails: Vec::new(),
            inventory_generation: 0,
            native_numa: node.native.numa(worker)?,
            native_task: None,
            native_retry: uring_runtime::environment::now(),
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
            next_health: uring_runtime::environment::now(),
        })
    }

    fn assemble_reads(
        config: &Config,
        node: &NodeState,
        snapshots: Rc<SnapshotStore>,
        availability: Rc<crate::control::state::Availability>,
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
        let streams = Rc::new(RangeStreams::new(
            node.workers.clone(),
            delivery,
            config.limits.range_window_pages.get(),
        ));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata.clone(),
            fill,
            streams,
            credentials,
            availability,
        ));
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
        reactor: Rc<Reactor>,
        keys: Rc<Keyring>,
        snapshots: Rc<SnapshotStore>,
        inventory: Arc<crate::rdma::discovery::Inventory>,
    ) -> Rc<ControlClient> {
        let enrollment = Rc::new(
            Enrollment::new(
                config.cluster.clone(),
                config.service_account_token.clone(),
                config.identity_directory.clone(),
            )
            .with_inventory(inventory),
        );
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
        ));
        control.attach_io(Rc::new(ReactorControlIo::new(reactor)));
        control
    }

    fn assemble_storage(
        config: &Config,
        node: &NodeState,
        worker: WorkerId,
        runtime: &WorkerRuntime,
        buffers: BufferPool,
        availability: Rc<crate::control::state::Availability>,
        metrics: &crate::telemetry::metrics::Metrics,
    ) -> Result<Store> {
        let limits = runtime.admission.limits();
        let index = Rc::new(Index::new(
            worker,
            limits.metadata_entries.get(),
            availability.clone(),
        ));
        let segments = Rc::new(page_alloc::Segments::new(config.segment_bytes));
        let eviction = Rc::new(SegmentClock::new(
            index.clone(),
            segments.clone(),
            config.free_segment_reserve,
        ));
        let slabs = Rc::new(page_alloc::Slab::new(
            config
                .slab_directory
                .join(format!("worker-{}-slab-0.dat", worker.0)),
            config.slab_bytes,
            config.segment_bytes,
            crate::model::PAGE_BYTES as usize + crate::store::format::MAX_HEADER_BYTES + 16,
        ));
        let reader = Rc::new(
            StoreReader::new(
                eviction.clone(),
                index.clone(),
                segments.clone(),
                slabs.clone(),
                runtime.admission.clone(),
                runtime.reactor.clone(),
                buffers,
            )
            .with_metrics(metrics.clone()),
        );
        let writer = Rc::new(
            StoreWriter::new(
                index.clone(),
                segments.clone(),
                slabs,
                runtime.admission.clone(),
                runtime.reactor.clone(),
                availability,
            )
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
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
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
        self.refresh_inventory()?;
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
                                && published.device == actual.device
                                && published.port == actual.port
                                && published.gid.is_none_or(|gid| actual.gid == Some(gid))
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
            .poll_deadlines(uring_runtime::environment::now(), budget);
        if !self.stopping {
            self.poll_cache_preparation(cx)?;
        }
        self.poll_listeners(cx, budget)?;
        self.poll_writer(cx)?;
        if !self.stopping {
            self.poll_control(cx)?;
            self.refresh_snapshot(&scope(self.timeout)?)?;
        }
        let now = uring_runtime::environment::now();
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
                let connection =
                    crate::http::from_reserved(accepted.fd.into(), accepted.reservation)?;
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
            let turn = scope(racer_control_wire::POLL_WAIT + Duration::from_secs(10))?;
            self.control_scope = Some(turn.clone());
            self.control_task = Some(Box::pin(async move { control.progress(&turn).await }));
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
            self.discovered_nics.clone(),
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
pub(crate) mod tests;
