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

use crate::admission::AdmissionPolicy;
use crate::client::RequestParser;
use crate::client::Responses;
use crate::client::listener::ClientListeners;
use crate::client::listener::PreparedListeners;
use crate::config::Config;
use crate::config::Limits;
use crate::control::CacheTransition;
use crate::control::ControlEndpoint;
use crate::control::ReactorControlIo;
use crate::control::Snapshot;
use crate::control::publication::PublicationTarget;
use crate::control::rails::RailJournal;
use crate::control::session::Session;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::Delivery;
use crate::http::HttpPool;
use crate::http::new_pipe_pool;
use crate::memory::BufferPool;
use crate::memory::MemoryCache;
use crate::model::RequestId;
use crate::model::WorkerId;
use crate::origin::Origin;
use crate::origin::OriginClient;
use crate::peer::Relay;
use crate::peer::Requester;
use crate::peer::forwarding::Forwarding;
use crate::peer::protocol::Signatures;
use crate::peer::server::PeerServer;
use crate::peer::transport::Transfers;
use crate::rdma::Devices;
use crate::rdma::Sessions;
use crate::read::Coordinator;
use crate::read::candidates::CandidatePolicy;
use crate::read::dispatch::WorkerDirectory;
use crate::read::dispatch::WorkerEndpoint;
use crate::read::dispatch::WorkerMap;
use crate::read::fill::Fill;
use crate::read::fill::FillDependencies;
use crate::read::flight::Flights;
use crate::read::metadata::MetadataDependencies;
use crate::read::metadata::MetadataService;
use crate::read::range_stream::RangeStreams;
use crate::runtime::HashMap;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use crate::security::CredentialCrypto;
use crate::security::PageCrypto;
use crate::security::PageCryptoEngine;
use crate::store::Store;
use crate::store::StoreReader;
use crate::store::StoreWriter;
use crate::store::catalog::Index;
use crate::store::catalog::SegmentClock;
use crate::store::checkpoint::CheckpointGeometry;
use crate::store::checkpoint::Checkpointer;
use crate::store::checkpoint::Recovery;
use crate::store::checkpoint::ShardImage;
use crate::telemetry::Gauge;
use crate::telemetry::Health;
use crate::telemetry::Resources;
use crate::telemetry::State;
use crate::telemetry::Telemetry;
use crate::topology::LinkHealth;
use crate::topology::Paths;
use crate::topology::Placement;
use crate::worker::AffinityPlan;
use crate::worker::CryptoRuntime;
use crate::worker::WorkerGroup;
use crate::worker::WorkerRuntime;
use controlplane::Published;
use racer_control_wire::CacheDefinition;
use racer_control_wire::NodeId;
use racer_crypto::enrollment::Enrollment;
use racer_crypto::identity::BundleInstaller;
use racer_crypto::identity::Certificates;
use racer_crypto::identity::KeyEpochs;
use racer_crypto::identity::KeyPurpose;
use racer_crypto::identity::Keyring;
use std::cell::RefCell;
use std::num::NonZeroUsize;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::UNIX_EPOCH;

use uring_runtime::drivers::poll_task;

mod native;
mod recovery;

/// Node-level composition and startup owner for the worker service group.
pub struct Application {
    resources: std::sync::OnceLock<crate::worker::Resources>,

    config: Arc<Config>,

    node: Arc<NodeState>,

    limits: Limits,

    discovered_nics: Vec<racer_control_wire::RailMapping>,
}

/// Shared immutable-publication and partitioned-admission roots. No Rc worker
/// graph crosses a thread. Worker zero alone drives enrollment/control reloads.
pub struct NodeState {
    inventory: Arc<crate::rdma::Inventory>,

    send_crc: crate::telemetry::Samples,

    hedges: std::sync::OnceLock<Arc<crate::read::candidates::Hedges>>,

    peer_admission: Arc<crate::peer::AdaptivePeers>,

    peer_receive: std::sync::OnceLock<Arc<crate::peer::receive::Gate>>,

    subscriptions: Arc<crate::peer::subscriptions::Subscriptions>,

    ingress: Arc<crate::admission::Ingress>,

    metrics: Vec<(WorkerId, crate::telemetry::Metrics)>,

    failures: crate::telemetry::Failures,

    publications: Arc<Published<Snapshot>>,

    keys: Arc<KeyEpochs>,

    control_worker: WorkerId,

    workers: Arc<WorkerDirectory>,

    count: usize,

    prepared: AtomicUsize,

    checkpoint: Mutex<CheckpointCut>,

    periodic_checkpoint: Mutex<CheckpointCut>,

    recovery: Mutex<recovery::RecoveryCut>,

    observations: Observations,

    native: native::NativePairs,

    cache_rollout: controlplane::Rollout<WorkerId, Vec<CacheDefinition>>,
}

/// Shared progress for collecting and durably publishing a worker checkpoint cut.
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
    /// Allocate shared state with default peer admission for the registered workers.
    fn new(workers: Vec<WorkerId>, capacity: usize) -> Result<Self> {
        Self::with_peer_admission(workers, capacity, Default::default())
    }

    /// Allocate node-wide authorities and partitioned metrics for the worker set.
    fn with_peer_admission(
        workers: Vec<WorkerId>,
        capacity: usize,
        peer_config: crate::peer::Config,
    ) -> Result<Self> {
        let count = workers.len();
        let map = Arc::new(WorkerMap::new(workers.clone())?);
        let metrics = crate::telemetry::Metrics::for_workers(count)?;
        let peer_admission = crate::peer::AdaptivePeers::new(peer_config, metrics[0].clone())?;
        Ok(Self {
            peer_admission,
            peer_receive: Default::default(),
            inventory: Arc::new(crate::rdma::Inventory::default()),
            send_crc: Default::default(),
            hedges: std::sync::OnceLock::new(),
            ingress: Arc::new(crate::admission::Ingress::new(&workers)),
            subscriptions: Arc::new(crate::peer::subscriptions::Subscriptions::new(
                Default::default(),
            )?),
            publications: Arc::new(Published::new(Snapshot::retention(2))),
            metrics: workers.iter().copied().zip(metrics).collect(),
            failures: crate::telemetry::Failures::default(),
            keys: Arc::new(KeyEpochs::default()),
            control_worker: WorkerId(0),
            cache_rollout: controlplane::Rollout::new(workers.iter().copied()),
            workers: Arc::new(WorkerDirectory::new(map, workers, capacity)?),
            count,
            prepared: AtomicUsize::new(0),
            checkpoint: Mutex::new(CheckpointCut::default()),
            periodic_checkpoint: Mutex::new(CheckpointCut::default()),
            recovery: Mutex::new(recovery::RecoveryCut::default()),
            observations: Observations::default(),
            native: native::NativePairs::default(),
        })
    }
}

impl Application {
    /// Composition only. Does not open files, spawn threads, or accept requests.
    pub fn assemble(config: Config) -> Result<Self> {
        Ok(Self {
            resources: std::sync::OnceLock::new(),
            limits: config.limits.clone(),
            config: Arc::new(config),
            node: Arc::new(NodeState::default()),
            discovered_nics: Vec::new(),
        })
    }

    /// Discover resources, enroll the node, and run its worker group until shutdown.
    pub fn run(mut self) -> Result<()> {
        self.config.validate()?;
        self.discovered_nics = crate::rdma::inventory();
        let mut plan = AffinityPlan::discover(&self.config)?;
        log_worker_plan("planned", &plan);
        self.limits = size_workers(&self.config.limits, &mut plan, self.config.enable_rdma)?;
        self.config.admission_history_per_worker(plan.pairs.len())?;
        log_worker_plan("final", &plan);
        self.node = Arc::new(NodeState::with_peer_admission(
            plan.pairs.iter().map(|p| p.worker).collect(),
            self.limits.queue_entries.get(),
            self.config.peer_admission,
        )?);
        Arc::get_mut(&mut self.node)
            .ok_or(Error::Internal)?
            .publications = Arc::new(Published::new(Snapshot::retention(
            self.config.limits.retained_snapshots.get(),
        )));
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
        self.prepare_workers()?;
        let mut workers = WorkerGroup::new(plan);
        let result = workers.run(&self);
        if signals.requested() && result == Err(Error::Cancelled) {
            Ok(())
        } else {
            result
        }
    }
}

/// Create a bounded scope for application-owned lifecycle operations.
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
        Rc::new(PublicationTarget::new(
            config.cluster.clone(),
            node.publications.clone(),
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
    // Fence CPU preparation before draining the reactor, on success and failure.
    let shutdown_scope = scope(config.shutdown_timeout)?;
    let mut shutdown = control.shutdown(&shutdown_scope);
    let shutdown_result = loop {
        if let Poll::Ready(result) = shutdown.as_mut().poll(&mut cx) {
            break result;
        }
        reactor.poll_budgeted(64)?;
        reactor.wait(Duration::from_millis(1))?;
    };
    drop(shutdown);
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
    result.and_then(|identity| shutdown_result.map(|()| identity))
}

static STOP_REQUESTED: AtomicBool = AtomicBool::new(false);

/// Record a process stop request using only a signal-safe atomic store.
extern "C" fn request_stop(_: libc::c_int) {
    STOP_REQUESTED.store(true, Ordering::Relaxed);
}

/// Restore the original process signal handlers when application ownership ends.
struct SignalGuard {
    previous: Vec<(libc::c_int, libc::sigaction)>,
}

impl SignalGuard {
    /// Install stop handlers while retaining each previous process handler.
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

    /// Check whether a handled process signal requested shutdown.
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

    /// Worker identity used for routing and rollout acknowledgments.
    pub worker: WorkerId,

    runtime: WorkerRuntime,

    control: Option<Rc<Session>>,

    snapshots: Rc<PublicationTarget>,

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

    discovered_nics: Vec<racer_control_wire::RailMapping>,

    actual_rails: Vec<racer_control_wire::RailMapping>,

    inventory_generation: u64,

    native_numa: Option<native::NativePlacement>,

    native_task: Option<Operation<'static, Vec<racer_control_wire::RailMapping>>>,

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

    candidates: Rc<CandidatePolicy>,

    ownership_maintenance: OwnershipMaintenance,

    placement_retry: std::time::Instant,

    placement_warning: Option<std::time::Instant>,

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

    caches: Vec<racer_control_wire::CacheDefinition>,

    slab_directory: std::path::PathBuf,

    prepared_listeners: Rc<std::cell::RefCell<Option<crate::client::listener::PreparedListeners>>>,

    cache_prepare_task: Option<Operation<'static, ()>>,

    cache_preparing_generation: u64,

    control_scope: Option<RequestScope>,
}

/// Ownership is optional eviction guidance. Even Progress may mean the ranking
/// cache is pinned, so do not self-wake continuously or retry within one turn.
struct OwnershipMaintenance {
    next: std::time::Instant,
}

impl OwnershipMaintenance {
    /// Advance eviction guidance at most once per scheduled maintenance turn.
    fn poll(
        &mut self,
        now: std::time::Instant,
        stopping: bool,
        refresh: impl FnOnce() -> Result<crate::topology::Maintenance>,
    ) {
        if stopping || now < self.next {
            return;
        }
        let delay = match refresh() {
            Ok(crate::topology::Maintenance::Progress) => Duration::from_millis(1),
            // Missing publications and unavailable capacity must not fail reads
            // or permanently prevent a later pass over idle/recovered residents.
            Ok(crate::topology::Maintenance::Idle | crate::topology::Maintenance::Blocked)
            | Err(_) => Duration::from_millis(100),
        };
        self.next = now + delay;
    }

    /// Limit idle waiting by the next maintenance turn unless shutdown has begun.
    fn wait_timeout(&self, now: std::time::Instant, stopping: bool, maximum: Duration) -> Duration {
        if stopping {
            maximum
        } else {
            maximum.min(self.next.saturating_duration_since(now))
        }
    }
}

#[cfg(test)]
mod ownership_maintenance_tests {
    //! Maintenance scheduling keeps progress bounded and shutdown idle.

    use super::*;
    use crate::topology::Maintenance;

    #[test]
    fn ownership_maintenance_bounds_progress_and_retries_idle_and_errors() {
        let start = std::time::Instant::now();
        let mut maintenance = OwnershipMaintenance { next: start };
        let calls = std::cell::Cell::new(0);
        for status in [
            Ok(Maintenance::Progress),
            Ok(Maintenance::Idle),
            Ok(Maintenance::Blocked),
            Err(Error::Unavailable),
            Err(Error::Overloaded),
        ] {
            let now = maintenance.next;
            maintenance.poll(now, false, || {
                calls.set(calls.get() + 1);
                status
            });
            let expected = if status == Ok(Maintenance::Progress) {
                Duration::from_millis(1)
            } else {
                Duration::from_millis(100)
            };
            assert_eq!(maintenance.next, now + expected);
            assert_eq!(
                maintenance.wait_timeout(now, false, Duration::from_secs(1)),
                expected
            );
            maintenance.poll(now, false, || panic!("same-turn refresh"));
            maintenance.poll(maintenance.next - Duration::from_nanos(1), false, || {
                panic!("early refresh")
            });
        }
        assert_eq!(calls.get(), 5);
        maintenance.poll(maintenance.next, true, || panic!("refresh during shutdown"));
        assert_eq!(
            maintenance.wait_timeout(maintenance.next, true, Duration::from_secs(1)),
            Duration::from_secs(1)
        );
        assert_eq!(
            maintenance.wait_timeout(maintenance.next, false, Duration::from_secs(1)),
            Duration::ZERO
        );
    }
}

impl WorkerApplication {
    /// All constructors below only connect dependencies. Runtime commands route
    /// listener work to page/metadata owners before any acquisition is dispatched.
    pub fn assemble(
        config: &Config,
        node: Arc<NodeState>,
        worker: WorkerId,
        runtime: WorkerRuntime,
        discovered_nics: Vec<racer_control_wire::RailMapping>,
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
                crate::read::candidates::Hedges::new(config.page_hedge, node.metrics[0].1.clone())
                    .expect("validated hedge config")
            })
            .clone();
        admission
            .policy()
            .set_observer(node.failures.observer(worker));
        metrics.observe_admission(worker, admission.shared())?;
        node.ingress.install(worker, &admission)?;
        let reactor = runtime.reactor.clone();
        let limits = admission.policy().limits();
        let snapshots = Rc::new(PublicationTarget::new(
            config.cluster.clone(),
            node.publications.clone(),
        ));
        let keys = Rc::new(Keyring::new(
            config.cluster.clone(),
            config.node.clone(),
            node.keys.clone(),
        ));
        let certificates = Rc::new(Certificates::new(config.cluster.clone(), keys.clone()));
        let availability = Rc::new(crate::control::Availability::new(
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

        let mut http = crate::http::new_pool(
            reactor.clone(),
            admission.clone(),
            limits.connections_per_neighbor.get(),
        );
        http.config_mut().tcp_nodelay = config.peer_tcp_nodelay;
        http.config_mut().secondary_cap = config
            .origin_connections_per_cache
            .get()
            .min(limits.client_connections.get());
        let http = Rc::new(http);
        let io = Rc::new(crate::http::new_io(
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
            Paths::with_limits(
                Rc::new(LinkHealth),
                limits.cached_paths.get(),
                limits.path_cache_bytes.get(),
                limits.active_path_searches.get(),
            )
            .with_peer_admission(node.peer_admission.clone()),
        );
        let placement = Rc::new(Placement::with_memory_budget(
            limits.placement_cache_bytes.get(),
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
        .with_receive_gate({
            let gate = crate::peer::receive::Gate::with_metrics(
                config.peer_receive,
                node.metrics[0].1.clone(),
            )?;
            node.peer_receive.get_or_init(|| gate).clone()
        })
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
            .with_observer(admission.policy().observer()),
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
            .with_observer(admission.policy().observer()),
        );
        let origin: Rc<dyn Origin> = Rc::new(OriginClient::new(
            node.publications.clone(),
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
            candidates,
            ownership_maintenance: OwnershipMaintenance {
                next: uring_runtime::environment::now(),
            },
            placement_retry: uring_runtime::environment::now(),
            placement_warning: None,
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

    /// Connect shared publication state to worker-local read and metadata services.
    fn assemble_reads(
        config: &Config,
        node: &NodeState,
        snapshots: Rc<PublicationTarget>,
        availability: Rc<crate::control::Availability>,
        delivery: Rc<Delivery>,
        metrics: &crate::telemetry::Metrics,
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
            admission.policy().limits().metadata_entries.get(),
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
            snapshots.published.clone(),
            metadata.clone(),
            fill,
            streams,
            credentials,
            availability,
        ));
        (coordinator, metadata)
    }

    /// Compose client framing and optional distributed ingress without binding sockets.
    fn assemble_clients(
        config: &Config,
        node: &NodeState,
        runtime: &WorkerRuntime,
        coordinator: Rc<Coordinator>,
        delivery: Rc<Delivery>,
        metrics: &crate::telemetry::Metrics,
        distributed: bool,
    ) -> Rc<ClientListeners> {
        let admission = runtime.admission.clone();
        let io = Rc::new(crate::http::client_io(
            runtime.reactor.clone(),
            admission.clone(),
        ));
        let responses = Rc::new(
            Responses::new(io.clone(), delivery).with_observer(admission.policy().observer()),
        );
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

    /// Bind independent control feeds and durable enrollment to the serving reactor.
    fn assemble_control(
        config: &Config,
        reactor: Rc<Reactor>,
        keys: Rc<Keyring>,
        snapshots: Rc<PublicationTarget>,
        inventory: Arc<crate::rdma::Inventory>,
    ) -> Rc<Session> {
        let io = Rc::new(ReactorControlIo::new(reactor));
        let rails = RailJournal::new(inventory, config.identity_directory.clone(), io.clone());
        let enrollment = Rc::new(Enrollment::new(
            config.cluster.clone(),
            config.service_account_token.clone(),
            config.identity_directory.clone(),
            io.clone(),
        ));
        let secrets = BundleInstaller::new(keys.clone());
        let control = Rc::new(Session::new(
            ControlEndpoint {
                url: config.control_endpoint.clone(),
                trust_bundle: config.trust_bundle.clone(),
            },
            enrollment,
            rails,
            config.shares,
            keys,
            secrets,
            snapshots,
        ));
        control
            .attach_io(io)
            .expect("valid static feed configuration");
        control
    }

    /// Compose worker storage, checkpointing, and retention without opening files.
    fn assemble_storage(
        config: &Config,
        node: &NodeState,
        worker: WorkerId,
        runtime: &WorkerRuntime,
        buffers: BufferPool,
        availability: Rc<crate::control::Availability>,
        metrics: &crate::telemetry::Metrics,
    ) -> Result<Store> {
        let limits = runtime.admission.policy().limits();
        let index = Rc::new(Index::new(
            worker,
            limits.metadata_entries.get(),
            availability.clone(),
        ));
        let segments = Rc::new(page_alloc::Segments::new(config.segment_bytes));
        let eviction = Rc::new(SegmentClock::with_budget(
            index.clone(),
            segments.clone(),
            config.free_segment_reserve,
            crate::store::catalog::FOREGROUND_RECLAIM_BUDGET,
        ));
        let slabs = Rc::new(page_alloc::Slab::new(
            config
                .slab_directory
                .join(format!("worker-{}-slab-0.dat", worker.0)),
            config.slab_bytes,
            config.segment_bytes,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
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
        let disk_page_entries = (config.disk_page_entries.get() / node.count).max(1);
        writer.configure(
            runtime.admission.clone(),
            eviction.clone(),
            limits.queue_entries.get(),
            disk_page_entries,
        )?;
        // Install once before Fill attaches current-membership ownership. The
        // writer, index, and segment clock all consult this same worker policy.
        writer.set_retention(Rc::new(crate::retention::Retention::configured(
            config.admission_history_per_worker(node.count)?,
            disk_page_entries
                .checked_add(limits.queue_entries.get())
                .ok_or(Error::InvalidConfiguration)?,
            config.admission_period,
            config.admission_mode == crate::config::AdmissionMode::SecondSight,
        )?));
        metrics.observe_retention(writer.retention().snapshot());
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

    /// Reclaim idle ciphertext owners before returning unused pool buffers.
    fn ciphertext_reclaimer(
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        memory: Rc<MemoryCache>,
        writer: Rc<StoreWriter>,
    ) -> impl Fn(&racer_control_wire::CacheId, usize) {
        move |cache, amount| {
            let class = crate::admission::ResourceClass::Ciphertext;
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

    /// Apply accepted cache removals and retire incompatible local native resources.
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

    /// Advance bounded service work under this worker's environment and driver queue.
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
        let now = uring_runtime::environment::now();
        if !self.stopping
            && now >= self.placement_retry
            && let Ok(snapshot) = self.snapshots.current()
        {
            let status = self.placement.maintain(&snapshot.membership);
            self.observe_placement_maintenance(status, now, cx)?;
        }
        // Independent of placement.maintain being Idle: recovered residents and
        // changed membership need periodic refresh even without foreground reads.
        self.ownership_maintenance.poll(now, self.stopping, || {
            self.candidates.refresh_ownership(self.store.writer.index())
        });
        self.poll_checkpoint(cx)?;
        self.poll_ingress(cx, budget)?;
        self.http.poll_waiters(budget);
        if let Some(gate) = self.node.peer_receive.get() {
            gate.poll_deadlines(budget);
        }
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

    /// Schedule placement progress or back off pinned capacity with a bounded warning rate.
    fn observe_placement_maintenance(
        &mut self,
        status: Result<crate::topology::Maintenance>,
        now: std::time::Instant,
        cx: &mut Context<'_>,
    ) -> Result<()> {
        match status {
            Ok(crate::topology::Maintenance::Progress) => cx.waker().wake_by_ref(),
            Ok(crate::topology::Maintenance::Idle) => (),
            Ok(crate::topology::Maintenance::Blocked) | Err(Error::Overloaded) => {
                // Foreground requests release pinned rankings. Back off background
                // work and rate-limit the capacity diagnostic, not request service.
                self.placement_retry = now + Duration::from_millis(100);
                if self
                    .placement_warning
                    .is_none_or(|last| now.duration_since(last) >= Duration::from_secs(60))
                {
                    eprintln!(
                        "racer-dataplane: worker={} stage=placement-maintenance status=blocked retry_ms=100 action=check-inflight-requests-or-increase-RACER_PLACEMENT_CACHE_BYTES",
                        self.worker.0
                    );
                    self.placement_warning = Some(now);
                }
            }
            Err(error) => return Err(error),
        }
        Ok(())
    }

    /// Admit a bounded batch of handed-off connections and poll active peer work.
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
                    crate::http::from_reserved(accepted.fd.into(), accepted.reservation);
                // Isolated retirement failures must not terminate listener service.
                if let crate::admission::Kind::Retirement(authorization) = accepted.kind {
                    match connection {
                        Ok(connection) => {
                            self.clients.install_retirement(connection, authorization)?;
                        }
                        Err(error) => eprintln!(
                            "racer-dataplane: stage=client-retirement operation=from-reserved error={error}"
                        ),
                    }
                    continue;
                }
                let connection = connection?;
                match accepted.kind {
                    crate::admission::Kind::Client(cache, retired) => {
                        self.clients
                            .install_connection(connection, cache, retired)?;
                    }
                    crate::admission::Kind::Retirement(_) => unreachable!("handled above"),
                    crate::admission::Kind::Peer => {
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

    /// Advance listeners, read flights, and native transfers while observing failures.
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

    /// Drive queued writes unless a periodic checkpoint is waiting for its cut.
    fn poll_writer(&mut self, cx: &mut Context<'_>) -> Result<()> {
        if let Some(result) = poll_task(&mut self.writer_task, cx)
            && !matches!(
                result,
                Err(Error::Io
                    | Error::Os(_)
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

    /// Advance publication and renewal work without blocking the independent key feed.
    fn poll_control(&mut self, cx: &mut Context<'_>) -> Result<()> {
        // Raw filesystem errno and uncertain publication outcomes are not blanket
        // retries: the owner must reconcile namespace state before another mutation.
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

    /// Keep one scoped keyring turn active and preserve nontransient failures.
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

    /// Fence control preparation and release this worker's remaining service owners.
    pub fn shutdown<'a>(&'a mut self, shutdown_scope: &'a RequestScope) -> Operation<'a, ()> {
        let environment = self.environment.clone();
        let drivers = self.drivers.clone();
        Box::pin(environment.scope(drivers.scope(async move {
            self.control_task.take();
            self.keyring_task.take();
            if let Some(control) = &self.control {
                control.shutdown(shutdown_scope).await?;
            }
            if let Some(endpoint) = &mut self.endpoint {
                endpoint.uninstall()?;
            }
            self.endpoint.take();
            self.store.checkpoint.finish_snapshot();
            self.started = false;
            self.telemetry
                .health
                .transition(crate::telemetry::State::Stopped)?;
            Ok(())
        })))
    }
}

impl Application {
    /// Allocate paired worker resources once before building service lanes.
    fn prepare_workers(&self) -> Result<()> {
        let resources = crate::worker::Resources::new(
            self.limits.clone(),
            self.node
                .metrics
                .iter()
                .map(|(worker, _)| *worker)
                .collect(),
            1,
        )?;
        self.resources
            .set(resources)
            .map_err(|_| Error::InvalidConfiguration)
    }

    /// Build one worker-local service graph from the node's shared authorities.
    fn build(
        &self,
        worker: WorkerId,
        runtime: WorkerRuntime,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        let application = WorkerApplication::assemble(
            &self.config,
            self.node.clone(),
            worker,
            runtime,
            self.discovered_nics.clone(),
        )?;
        Ok(Box::new(application))
    }

    /// Build the crypto service and attach any configured native resources.
    fn build_crypto(
        &self,
        worker: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        self.node
            .native
            .crypto(worker, PageCryptoEngine::new(runtime))
    }
}

impl uring_runtime::group::Factory<RequestScope> for Application {
    fn build_lane(
        &self,
        lane: usize,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        self.resources
            .get()
            .ok_or(Error::InvalidConfiguration)?
            .build_lane(lane, |worker, runtime| self.build(worker, runtime))
    }

    fn build_helper(
        &self,
        lane: usize,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        self.resources
            .get()
            .ok_or(Error::InvalidConfiguration)?
            .build_helper(lane, |worker, runtime| self.build_crypto(worker, runtime))
    }

    fn abandon_lane(&self, lane: usize) -> Result<()> {
        self.resources
            .get()
            .ok_or(Error::InvalidConfiguration)?
            .abandon_lane(lane)
    }

    fn teardown_scope(&self, startup: &RequestScope) -> RequestScope {
        scope(Duration::from_secs(30)).unwrap_or_else(|_| startup.clone())
    }
}

impl uring_runtime::group::Service<RequestScope> for WorkerApplication {
    fn wait_timeout(&self, maximum: Duration) -> Duration {
        let _environment = self.environment.enter();
        let maximum = self.ownership_maintenance.wait_timeout(
            uring_runtime::environment::now(),
            self.stopping,
            maximum,
        );
        self.metadata.next_deadline().map_or(maximum, |deadline| {
            maximum.min(deadline.saturating_duration_since(uring_runtime::environment::now()))
        })
    }

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
        if self.telemetry.health.state()? != crate::telemetry::State::Stopped {
            self.telemetry
                .health
                .transition(crate::telemetry::State::Draining)?;
        }
        self.clients.stop_admission();
        if let Some(endpoint) = &self.endpoint {
            endpoint.stop_admission();
        }
        if let Some(scope) = &self.listener_scope {
            scope.cancel()?;
        }
        if let Some(control) = &self.control {
            control.stop();
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
            if let Some(control) = &self.control {
                control.shutdown(&deadline).await?;
            }
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

/// Control-worker projection and local resources for a registered-worker rollout.
pub(crate) struct CachePublication {
    node: Arc<NodeState>,

    listeners: Rc<RefCell<Option<PreparedListeners>>>,

    capacity: usize,

    proposal: RefCell<Option<(u64, Arc<Vec<CacheDefinition>>)>>,
}

/// Reserved worker barrier and the control worker's prepared listeners.
struct Transition<'a> {
    guard: Option<controlplane::rollout::Guard<'a, WorkerId, Vec<CacheDefinition>>>,

    listeners: Option<PreparedListeners>,
}

impl CacheTransition for Transition<'_> {
    fn commit(mut self: Box<Self>) {
        if let Some(listeners) = self.listeners.take() {
            listeners.commit();
        }
        if let Some(guard) = self.guard.take() {
            guard.commit();
        }
    }
}

impl CachePublication {
    /// Propose changed definitions or reserve the acknowledged cut for atomic commit.
    pub(crate) fn stage(
        &self,
        definitions: &[CacheDefinition],
    ) -> Result<Box<dyn CacheTransition + '_>> {
        racer_control_wire::validate_definitions(definitions)?;
        let mut proposal = self.proposal.borrow_mut();
        if proposal
            .as_ref()
            .is_none_or(|(_, old)| old.as_slice() != definitions)
        {
            if definitions.len() > self.capacity {
                return Err(Error::Overloaded);
            }
            let value = Arc::new(definitions.to_vec());
            let generation = self.node.cache_rollout.propose(value.clone())?;
            self.listeners.borrow_mut().take();
            *proposal = Some((generation, value));
            return Err(Error::Unavailable);
        }
        let generation = proposal.as_ref().ok_or(Error::Internal)?.0;
        if self.node.cache_rollout.committed(generation)? {
            return Ok(Box::new(Transition {
                guard: None,
                listeners: None,
            }));
        }
        let guard = self.node.cache_rollout.stage(generation)?;
        let listeners = self
            .listeners
            .borrow_mut()
            .take()
            .ok_or(Error::Unavailable)?;
        if listeners.definitions() != definitions {
            return Err(Error::Unavailable);
        }
        Ok(Box::new(Transition {
            guard: Some(guard),
            listeners: Some(listeners),
        }))
    }
}

impl WorkerApplication {
    /// Attach cache rollout policy only on the worker that owns control feeds.
    fn attach_cache_adapter(&self) {
        if let Some(control) = &self.control {
            control.attach_cache_publication(Rc::new(CachePublication {
                node: self.node.clone(),
                listeners: self.prepared_listeners.clone(),
                proposal: RefCell::new(None),
                capacity: self
                    .runtime
                    .admission
                    .policy()
                    .limits()
                    .metadata_entries
                    .get(),
            }));
        }
    }

    /// Prepare local resources and acknowledge only the current rollout generation.
    fn poll_cache_preparation(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let node = self.node.clone();
        let Some(proposal) = node.cache_rollout.pending(&self.worker)? else {
            return Ok(());
        };
        let generation = proposal.generation;
        let definitions = proposal.value;
        if self.cache_preparing_generation != generation {
            self.cache_prepare_task.take();
            self.prepared_listeners.borrow_mut().take();
            self.cache_preparing_generation = generation;
        }
        // Reject publications exceeding this worker's metadata catalog.
        if definitions.len()
            > self
                .runtime
                .admission
                .policy()
                .limits()
                .metadata_entries
                .get()
        {
            return Ok(());
        }
        if self.control.is_some() && self.prepared_listeners.borrow().is_none() {
            if self.cache_prepare_task.is_none() {
                let clients = self.clients.clone();
                let prepared = self.prepared_listeners.clone();
                let startup = scope(self.timeout)?;
                self.cache_prepare_task = Some(Box::pin(async move {
                    *prepared.borrow_mut() = Some(clients.prepare(&definitions, &startup).await?);
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
        match node.cache_rollout.ack(self.worker, generation) {
            Ok(()) | Err(controlplane::Error::Stale) => (),
            Err(error) => return Err(error.into()),
        }
        Ok(())
    }
}

/// Readiness expires unless every required worker keeps progressing.
#[derive(Default)]
struct Observations {
    pub health: Health,

    workers: Mutex<HashMap<WorkerId, (Resources, Option<racer_control_wire::PublicationSequence>)>>,
}

impl Observations {
    /// Record fixture readiness without an accepted publication cursor.
    #[cfg(test)]
    fn record(&self, worker: WorkerId, resources: Resources, count: usize) -> Result<()> {
        self.record_snapshot(worker, resources, count, None)
    }

    /// Combine fresh worker observations into node-wide readiness.
    fn record_snapshot(
        &self,
        worker: WorkerId,
        resources: Resources,
        count: usize,
        sequence: Option<racer_control_wire::PublicationSequence>,
    ) -> Result<()> {
        let mut workers = self.workers.lock().map_err(|_| Error::Unavailable)?;
        workers.insert(worker, (resources, sequence));
        let now = uring_runtime::environment::now();
        let complete = workers.len() == count;
        let all = |test: fn(&Resources) -> bool| {
            complete
                && workers
                    .values()
                    .all(|(r, _)| test(r) && r.observed_until.is_some_and(|until| now < until))
        };
        let resources = Resources {
            workers_usable: all(|r| r.workers_usable),
            storage_usable: all(|r| r.storage_usable),
            listeners_usable: all(|r| r.listeners_usable),
            membership_usable: all(|r| r.membership_usable),
            admission_usable: all(|r| r.admission_usable),
            credentials_valid_until: workers
                .values()
                .map(|(r, _)| r.credentials_valid_until)
                .min()
                .flatten(),
            observed_until: workers
                .values()
                .map(|(r, _)| r.observed_until)
                .min()
                .flatten(),
        };
        self.health.observe(resources)?;
        if matches!(
            self.health.state()?,
            State::Starting | State::Ready | State::Degraded
        ) {
            self.health.transition(if resources.usable_at(now) {
                State::Ready
            } else {
                State::Degraded
            })?;
        }
        Ok(())
    }

    /// Count fresh worker observations matching the accepted publication.
    fn membership_workers(
        &self,
        diagnostic: &mut crate::telemetry::MembershipDiagnostic,
        count: usize,
    ) -> Result<()> {
        let workers = self.workers.lock().map_err(|_| Error::Unavailable)?;
        let now = uring_runtime::environment::now();
        diagnostic.expected_workers = count;
        diagnostic.matching_workers = workers
            .values()
            .filter(|(r, sequence)| {
                sequence.is_some_and(|s| s.0 == diagnostic.accepted_sequence)
                    && r.workers_usable
                    && r.observed_until.is_some_and(|until| now < until)
            })
            .count();
        Ok(())
    }
}

impl WorkerApplication {
    /// Start diagnostics on the control worker using its live publication observer.
    fn start_diagnostics(&mut self, cx: &mut Context<'_>) -> Result<()> {
        if self.control.is_none() {
            return Ok(());
        }
        let control = self.control.as_ref().unwrap().clone();
        let node = self.node.clone();
        self.telemetry
            .membership
            .set(Rc::new(move || {
                let mut diagnostic = control.membership_diagnostic()?;
                node.observations
                    .membership_workers(&mut diagnostic, node.count)?;
                Ok(diagnostic)
            }))
            .map_err(|_| Error::InvalidConfiguration)?;
        self.telemetry
            .attach_io(self.runtime.reactor.clone(), self.runtime.admission.clone())?;
        let diagnostic_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
        let task_scope = diagnostic_scope.clone();
        let telemetry = self.telemetry.clone();
        let address = self.diagnostics_address;
        self.diagnostic_scope = Some(diagnostic_scope);
        self.diagnostic_task = Some(Box::pin(async move {
            telemetry.serve(address, &task_scope).await
        }));
        if let Some(result) = poll_task(&mut self.diagnostic_task, cx) {
            result?;
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    /// Report this worker's current resources and credential lifetime.
    fn observe_health(&self) -> Result<()> {
        let node = &self.node;
        self.telemetry
            .metrics
            .observe_retention(self.store.writer.retention().snapshot());
        let now = uring_runtime::environment::now();
        if self.control.is_some() {
            self.telemetry.metrics.set_gauge(
                crate::telemetry::Gauge::KeyringGeneration,
                self.keys.generation()?.unwrap_or(0),
            );
        }
        let credentials = self.keys.signing_identity().ok().and_then(|identity| {
            let expiry = identity.expires_at_seconds();
            if self.control.is_some() {
                self.telemetry
                    .metrics
                    .set_gauge(crate::telemetry::Gauge::IdentityExpiresAtSeconds, expiry);
            }
            let remaining = (UNIX_EPOCH + Duration::from_secs(expiry))
                .duration_since(uring_runtime::environment::wall_now())
                .ok()?;
            now.checked_add(remaining)
        });
        let snapshot = self.snapshots.current().ok();
        node.observations.record_snapshot(
            self.worker,
            Resources {
                workers_usable: self.started && !self.stopping,
                storage_usable: self.store.writer.slabs().alignment().is_ok(),
                listeners_usable: self.started
                    && (self.control.is_none()
                        || self.peer_task.is_some() && self.diagnostic_task.is_some()),
                membership_usable: snapshot
                    .as_ref()
                    .is_some_and(|s| self.snapshot_sequence == Some(s.sequence)),
                admission_usable: !self.runtime.admission.is_stopped() && !self.stopping,
                credentials_valid_until: credentials,
                observed_until: Some(now + Duration::from_secs(2)),
            },
            node.count,
            self.snapshot_sequence,
        )
    }

    /// Recover storage and accept initial control state before opening listeners.
    pub fn start<'a>(&'a mut self, startup: &'a RequestScope) -> Operation<'a, ()> {
        let environment = self.environment.clone();
        let drivers = self.drivers.clone();
        Box::pin(environment.scope(drivers.scope(async move {
            startup.check()?;
            if self.started || self.stopping {
                return Err(Error::InvalidConfiguration);
            }
            // The authenticated node identity was installed before this graph existed.
            self.keys.signing_identity()?;
            let geometry = self.prepare_storage().await?;
            self.endpoint = Some(
                self.directory
                    .install(self.worker, self.coordinator.clone())?,
            );
            self.node.prepared.fetch_add(1, Ordering::Release);
            self.attach_cache_adapter();
            self.accept_initial_publication(startup).await?;
            self.wait_for_prepared_workers(startup).await?;
            // Recovery must consult the accepted cache UID set, not only secrets.
            self.recover_node(geometry, startup).await?;
            self.refresh_snapshot(startup)?;
            self.activate_native(startup).await?;
            self.start_listeners().await?;
            self.started = true;
            self.observe_health()?;
            Ok(())
        })))
    }

    /// Open storage and publish the effective disk capacity metrics.
    async fn prepare_storage(&self) -> Result<CheckpointGeometry> {
        let _ = self.store.open().await?;
        let geometry = CheckpointGeometry::from(self.store.writer.slabs().geometry()?);
        let (payload, tail) = geometry.payload_capacity(
            self.store.eviction.reserve(),
            self.store.writer.index().page_capacity(),
        )?;
        self.telemetry
            .metrics
            .add_gauge(Gauge::EffectivePayloadBytes, payload);
        self.telemetry
            .metrics
            .add_gauge(Gauge::SegmentTailBytes, tail);
        self.telemetry.metrics.add_gauge(
            Gauge::DiskPageEntries,
            self.store.writer.index().page_capacity() as u64,
        );
        Ok(geometry)
    }

    /// Drive enrollment and local installation until the first publication is accepted.
    async fn accept_initial_publication(&mut self, startup: &RequestScope) -> Result<()> {
        if let Some(control) = self.control.clone() {
            let identity = loop {
                match control.start(startup).await {
                    Ok(identity) => break identity,
                    Err(
                        Error::Io
                        | Error::Unavailable
                        | Error::Overloaded
                        | Error::DeadlineExceeded,
                    ) => {
                        startup.check()?;
                        let io = ReactorControlIo::new(self.runtime.reactor.clone());
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
            if identity.node() != self.keys.node() {
                return Err(Error::NodeIdentityChanged);
            }
            control.activate_identity()?;
            // Accept a complete compatible first snapshot before any listener.
            while self.snapshots.current().is_err() {
                startup.check()?;
                let mut progress = control.progress(startup);
                let result = std::future::poll_fn(|cx| {
                    self.poll_keyring(cx)?;
                    self.poll_cache_preparation(cx)?;
                    let result = progress.as_mut().poll(cx).map(|r| r.map(|_| ()));
                    if matches!(result, Poll::Ready(Err(_))) {
                        return result;
                    }
                    // Progress can install the prepared first snapshot while
                    // awaiting the next long poll. Startup depends on that
                    // local commit, not on another controller publication.
                    startup.check()?;
                    if self.snapshots.current().is_ok() {
                        Poll::Ready(Ok(()))
                    } else {
                        result
                    }
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
                        ReactorControlIo::sleep(
                            &io,
                            uring_runtime::environment::now() + Duration::from_millis(100),
                            startup,
                        )
                        .await?;
                    }
                    Err(error) => return Err(error),
                }
            }
        }
        Ok(())
    }

    /// Keep rollout preparation moving until every registered worker is ready.
    async fn wait_for_prepared_workers(&mut self, startup: &RequestScope) -> Result<()> {
        let node = self.node.clone();
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
        .await
    }

    /// Start diagnostics and bind the control worker's peer listener before readiness.
    async fn start_listeners(&mut self) -> Result<()> {
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
        Ok(())
    }
}

/// Divide node quotas and report the first resource that cannot fund worker progress.
fn partition_limits_with_cause(
    node: &Limits,
    workers: usize,
    rdma: bool,
) -> std::result::Result<Limits, (&'static str, Error)> {
    let invalid = |dimension| (dimension, Error::InvalidConfiguration);
    if workers == 0 {
        return Err(invalid("io_workers"));
    }
    let mut limits = node.clone();
    for (dimension, value) in [
        ("plaintext_bytes", &mut limits.plaintext_bytes),
        ("ciphertext_bytes", &mut limits.ciphertext_bytes),
        ("dirty_bytes", &mut limits.dirty_bytes),
        ("request_context_bytes", &mut limits.request_context_bytes),
        ("flights", &mut limits.flights),
        ("queue_entries", &mut limits.queue_entries),
        ("client_connections", &mut limits.client_connections),
        ("pipes", &mut limits.pipes),
        ("placement_cache_bytes", &mut limits.placement_cache_bytes),
        ("cached_paths", &mut limits.cached_paths),
        ("path_cache_bytes", &mut limits.path_cache_bytes),
        ("metadata_entries", &mut limits.metadata_entries),
        ("relay_transfers", &mut limits.relay_transfers),
    ] {
        *value = NonZeroUsize::new(value.get() / workers).ok_or_else(|| invalid(dimension))?;
    }
    if rdma {
        limits.registered_bytes = NonZeroUsize::new(node.registered_bytes.get() / workers)
            .ok_or_else(|| invalid("registered_bytes"))?;
    }
    let page = crate::model::PAGE_BYTES as usize;
    let window = limits.range_window_pages.get();
    for (dimension, insufficient) in [
        (
            "plaintext_bytes",
            limits.plaintext_bytes.get() < (window + 1) * page,
        ),
        (
            "ciphertext_bytes",
            limits.ciphertext_bytes.get()
                < (window + 1) * (page + 16) + crate::store::MAX_HEADER_BYTES,
        ),
        ("dirty_bytes", limits.dirty_bytes.get() < page + 16),
        (
            "placement_cache_bytes",
            limits.placement_cache_bytes.get() < crate::topology::RANKING_BYTES,
        ),
        (
            "registered_bytes",
            rdma && native::slot_count(&limits).map_err(|error| ("registered_bytes", error))? == 0,
        ),
        (
            "request_context_bytes",
            limits.request_context_bytes.get()
                < crate::telemetry::RESERVED_BYTES
                    + crate::peer::protocol::MIN_REQUEST_CONTEXT_BYTES
                    + 4 * limits.header_bytes.get().max(crate::model::MAX_FIELD_BYTES),
        ),
        // Diagnostics permanently partition the reactor and ControlProgress
        // quota. Preserve the ordinary two-slot progress floor alongside them.
        (
            "queue_entries",
            limits.queue_entries.get() < crate::telemetry::CONTROL_SLOTS + 2,
        ),
        (
            "client_connections",
            limits.client_connections < limits.connections_per_neighbor,
        ),
    ] {
        if insufficient {
            return Err(invalid(dimension));
        }
    }
    // Snapshot polling, renewal, and key delivery need independent control slots.
    let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits.clone()));
    if admission.limit(crate::admission::ResourceClass::ControlConnection) < 3 {
        return Err(invalid("control_connections"));
    }
    Ok(limits)
}

/// Report worker counts and CPU placement for one startup planning stage.
fn log_worker_plan(stage: &str, plan: &AffinityPlan) {
    let groups = plan.crypto_groups();
    eprintln!(
        "racer-dataplane: stage=worker-plan state={stage} io_workers={} crypto_workers={}",
        plan.pairs.len(),
        groups.len()
    );
    for indices in groups {
        let crypto = &plan.pairs[indices[0]].crypto;
        let io = indices
            .iter()
            .map(|&index| {
                let pair = &plan.pairs[index];
                (pair.worker.0, pair.io.cpu, pair.io.numa_node)
            })
            .collect::<Vec<_>>();
        eprintln!(
            "racer-dataplane: stage=worker-placement state={stage} crypto_cpu={} crypto_numa={:?} io_worker_cpu_numa={io:?}",
            crypto.cpu, crypto.numa_node
        );
    }
}

/// Reduce worker count until each shard can fund its required progress resources.
fn size_workers(node: &Limits, plan: &mut AffinityPlan, rdma: bool) -> Result<Limits> {
    // Reduce shards to fund progress, then rebalance shared crypto execution.
    let mut count = plan.pairs.len();
    let mut limiting_resource = None;
    loop {
        match partition_limits_with_cause(node, count, rdma) {
            Ok(limits) => {
                if count != plan.pairs.len() {
                    eprintln!(
                        "racer-dataplane: stage=worker-sizing planned_io={} final_io={count} limiting_resource={}",
                        plan.pairs.len(),
                        limiting_resource.unwrap_or("unknown")
                    );
                    plan.reduce_workers(count);
                }
                return Ok(limits);
            }
            Err((dimension, _)) if count > 1 => {
                limiting_resource = Some(dimension);
                count -= 1;
            }
            Err((dimension, error)) => {
                eprintln!(
                    "racer-dataplane: stage=worker-sizing io_workers={count} limiting_resource={dimension} error={error:?}"
                );
                return Err(error);
            }
        }
    }
}

#[cfg(test)]
pub(crate) mod tests;
