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
//! back already-created resources. No operational lifecycle is implemented here.

use crate::{
    client::{listener::ClientListeners, request::RequestParser, response::Responses},
    config::Config,
    control::{
        caches::CacheRegistry,
        client::ControlClient,
        enrollment::Enrollment,
        secrets::SecretWatcher,
        snapshot::{PublishedState, SnapshotStore},
    },
    error::{Operation, Result, deferred, pending},
    http::{io::HttpIo, pool::HttpPool},
    memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool},
    model::identity::WorkerId,
    origin::client::{Origin, OriginClient},
    peer::{
        handshake::Handshake,
        relay::Relay,
        requester::{PeerClient, Requester},
        server::PeerServer,
        transfer::Transfers,
    },
    rdma::{
        device::Devices, permission::Permissions, registered::RegisteredPool, session::Sessions,
        transfer::RdmaTransfer, verbs::Verbs,
    },
    read::{
        candidates::CandidatePolicy,
        dispatch::{Dispatcher, WorkerDirectory},
        fill::{Fill, FillDependencies},
        flight::Flights,
        metadata::MetadataService,
        range_stream::RangeStreams,
        serve::Coordinator,
    },
    runtime::{
        affinity::AffinityPlan,
        deadline::RequestScope,
        worker::{WorkerFactory, WorkerGroup, WorkerRuntime, WorkerService},
    },
    security::{
        aead::PageCrypto,
        certificates::Certificates,
        credentials::CredentialCrypto,
        forwarding::Forwarding,
        keyring::{KeyEpochs, Keyring},
        replay::{ReplayState, ReplayWindow},
        signing::Signatures,
    },
    store::{
        Store, checkpoint::Checkpointer, eviction::SegmentClock, index::Index, reader::StoreReader,
        recovery::Recovery, segment::Segments, slab::Slabs, writer::StoreWriter,
    },
    telemetry::Telemetry,
    topology::{health::LinkHealth, paths::Paths, placement::Placement, rails::Rails},
};
use std::{rc::Rc, sync::Arc};

pub struct Application {
    config: Arc<Config>,
    node: Arc<NodeState>,
}

/// Shared immutable-publication and partitioned-admission roots. No Rc worker
/// graph crosses a thread. Worker zero alone drives enrollment/control reloads.
pub struct NodeState {
    publications: Arc<PublishedState>,
    keys: Arc<KeyEpochs>,
    replay: Arc<ReplayState>,
    control_worker: WorkerId,
    workers: Arc<WorkerDirectory>,
}
impl Default for NodeState {
    fn default() -> Self {
        Self {
            publications: Arc::new(PublishedState),
            keys: Arc::new(KeyEpochs),
            replay: Arc::new(ReplayState),
            control_worker: WorkerId(0),
            workers: Arc::new(WorkerDirectory),
        }
    }
}

impl Application {
    /// Composition only. Does not open files, spawn threads, or accept requests.
    pub fn assemble(config: Config) -> Result<Self> {
        Ok(Self {
            config: Arc::new(config),
            node: Arc::new(NodeState::default()),
        })
    }

    pub fn run(self) -> Result<()> {
        self.config.validate()?;
        let plan = AffinityPlan::discover(&self.config)?;
        let mut workers = WorkerGroup::new(plan);
        workers.run(&self)
    }
}

/// This graph is constructed on its owning worker, never sent across threads.
/// Node-level control events enter through bounded worker commands. Mutable state
/// is not implicitly made global by Arc/Mutex or by a background async runtime.
pub struct WorkerApplication {
    pub worker: WorkerId,
    runtime: WorkerRuntime,
    control: Option<ControlClient>,
    snapshots: Rc<SnapshotStore>,
    keys: Rc<Keyring>,
    store: Store,
    clients: ClientListeners,
    peers: PeerServer,
    coordinator: Rc<Coordinator>,
    dispatcher: Rc<Dispatcher>,
    rdma: Option<Rc<RdmaTransfer>>,
    telemetry: Telemetry,
}

impl WorkerApplication {
    /// All constructors below only connect dependencies. Runtime commands route
    /// listener work to page/metadata owners before any acquisition is dispatched.
    pub fn assemble(
        config: &Config,
        node: &NodeState,
        worker: WorkerId,
        runtime: WorkerRuntime,
    ) -> Self {
        let admission = runtime.admission.clone();
        let reactor = runtime.reactor.clone();
        let snapshots = Rc::new(SnapshotStore::new(
            node.publications.clone(),
            config.limits.retained_snapshots.get(),
        ));
        let caches = Rc::new(CacheRegistry);
        let keys = Rc::new(Keyring::new(node.keys.clone()));
        let certificates = Rc::new(Certificates::new(config.trust_bundle.clone()));
        let replay = Rc::new(ReplayWindow::new(
            node.replay.clone(),
            config.limits.replay_entries.get(),
        ));
        let signatures = Rc::new(Signatures::new(keys.clone(), certificates, replay));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone()));
        let crypto = Rc::new(PageCrypto::new(keys.clone()));
        let control = if worker == node.control_worker {
            let enrollment =
                Enrollment::new(config.node.clone(), config.service_account_token.clone());
            let secrets = SecretWatcher::new(config.secret_directory.clone(), keys.clone());
            Some(ControlClient::new(
                config.control_endpoint.clone(),
                enrollment,
                secrets,
                snapshots.clone(),
                caches,
            ))
        } else {
            None
        };

        let http = Rc::new(HttpPool::new(
            reactor.clone(),
            admission.clone(),
            config.limits.connections_per_neighbor.get(),
        ));
        let io = Rc::new(HttpIo::new(
            reactor.clone(),
            crate::http::codec::Codec::new(
                config.limits.header_bytes.get(),
                crate::model::range::PAGE_BYTES + 16,
            ),
        ));
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let pipes = Rc::new(PipePool::new(admission.clone(), reactor.clone()));
        let delivery = Rc::new(Delivery::new(pipes, config.reader_stall_timeout));

        let index = Rc::new(Index);
        let segments = Rc::new(Segments::new(worker, config.segment_bytes));
        let eviction = Rc::new(SegmentClock::new(
            index.clone(),
            segments.clone(),
            config.free_segment_reserve,
        ));
        let slabs = Rc::new(Slabs::new(
            worker,
            config.slab_directory.clone(),
            reactor,
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
        let store = Store {
            reader: disk.clone(),
            writer: writer.clone(),
            checkpoint: Checkpointer::new(config.slab_directory.clone(), index, segments),
            recovery: Recovery::new(config.slab_directory.clone()),
            eviction,
        };

        let (sessions, rdma) = if config.enable_rdma {
            let devices = Rc::new(Devices::new(Rc::new(Verbs)));
            let sessions = Rc::new(Sessions::new(
                devices.clone(),
                config.limits.connections_per_neighbor.get(),
            ));
            let registered = Rc::new(RegisteredPool::new(devices, admission.clone()));
            let transfer = Rc::new(RdmaTransfer::new(
                sessions.clone(),
                registered,
                Rc::new(Permissions),
            ));
            (Some(sessions), Some(transfer))
        } else {
            (None, None)
        };

        let paths = Rc::new(Paths::new(
            Rc::new(LinkHealth),
            config.limits.cached_paths.get(),
            config.limits.route_search_work.get(),
        ));
        let rails = Rc::new(Rails);
        let placement = Rc::new(Placement::new(config.limits.cached_rankings.get()));
        let handshake = Rc::new(Handshake::new(signatures.clone(), sessions));
        let transfers = Rc::new(Transfers::new(http.clone(), io.clone(), rdma.clone()));
        let requester: Rc<dyn PeerClient> = Rc::new(Requester::new(
            paths.clone(),
            rails,
            signatures.clone(),
            handshake,
            transfers.clone(),
        ));
        let candidates = Rc::new(CandidatePolicy::new(
            config.node.clone(),
            placement,
            requester.clone(),
        ));
        let origin: Rc<dyn Origin> =
            Rc::new(OriginClient::new(snapshots.clone(), http, io.clone()));
        let flights = Rc::new(Flights::new(admission.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory,
            buffers,
            disk,
            writer,
            peers: requester.clone(),
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights,
            crypto,
            credentials: credentials.clone(),
            admission: admission.clone(),
        }));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            origin,
            requester,
            credentials.clone(),
            config.limits.metadata_entries.get(),
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
        let relay = Rc::new(Relay::new(
            paths,
            Rc::new(Forwarding::new(signatures.clone())),
            transfers,
            admission.clone(),
        ));
        let dispatcher = Rc::new(Dispatcher::new(
            worker,
            node.workers.clone(),
            coordinator.clone(),
        ));
        let peers = PeerServer::new(
            io.clone(),
            signatures,
            admission.clone(),
            dispatcher.clone(),
            relay,
        );
        let responses = Rc::new(Responses::new(io.clone(), delivery));
        let clients = ClientListeners::new(
            dispatcher.clone(),
            RequestParser::new(config.limits.header_bytes.get()),
            responses,
            io,
            admission,
        );

        Self {
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
            rdma,
            telemetry: Telemetry::default(),
        }
    }

    pub fn start<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("app.start")
    }
    pub fn shutdown<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("app.shutdown")
    }
}

impl WorkerFactory for Application {
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>> {
        Ok(Box::new(WorkerApplication::assemble(
            &self.config,
            &self.node,
            worker,
            runtime,
        )))
    }
}
impl WorkerService for WorkerApplication {
    fn poll_budgeted(&mut self, _work_budget: usize) -> Result<()> {
        pending("app.poll_budgeted")
    }
    fn stop_admission(&mut self) -> Result<()> {
        pending("app.stop_admission")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::{admission::Admission, reactor::Reactor};

    #[test]
    fn composes_http_and_optional_rdma_without_operational_side_effects() {
        for enable_rdma in [false, true] {
            let config = crate::test_support::cluster::config(enable_rdma);
            let admission = Rc::new(Admission::new(config.limits.clone()));
            let runtime = WorkerRuntime {
                reactor: Rc::new(Reactor::new(admission.clone())),
                admission,
            };
            let node = NodeState::default();
            let mut worker = WorkerApplication::assemble(&config, &node, WorkerId(0), runtime);
            assert!(worker.control.is_some());
            assert_eq!(worker.rdma.is_some(), enable_rdma);
            let second_admission = Rc::new(Admission::new(config.limits.clone()));
            let second_runtime = WorkerRuntime {
                reactor: Rc::new(Reactor::new(second_admission.clone())),
                admission: second_admission,
            };
            let second = WorkerApplication::assemble(&config, &node, WorkerId(1), second_runtime);
            assert!(
                second.control.is_none(),
                "only one worker enrolls and publishes"
            );
            assert!(
                Rc::strong_count(&worker.dispatcher) == 3
                    && Rc::strong_count(&worker.coordinator) == 2,
                "client and peer share one dispatcher and local coordinator"
            );
            assert_eq!(
                worker.poll_budgeted(1),
                Err(crate::error::Error::Unimplemented("app.poll_budgeted"))
            );
        }
    }

    // Implement startup rollback, control owner fanout, all-shard checkpoints,
    // multi-worker dispatch, and shutdown with active I/O alongside lifecycle code.
}
