//! Real read ownership behind client wire scenarios. Only the UDS adapter is scripted.
use crate::{
    control::{
        state::{CacheDefinition, PublishedState, SnapshotStore},
        wire::{Publication, PublicationSequence},
    },
    memory::{cache::MemoryCache, delivery::Delivery, pool::BufferPool},
    model::{MembershipVersion, ObjectMetadata, WorkerId},
    read::{
        Coordinator,
        candidates::CandidatePolicy,
        dispatch::{WorkerDirectory, WorkerEndpoint},
        drivers::DriverQueue,
        fill::{Fill, FillDependencies},
        flight::Flights,
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
    },
    runtime::{
        admission::Admission,
        crypto::{self, CryptoClient},
        reactor::Reactor,
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::{
        aead::{PageCrypto, PageCryptoEngine},
        credentials::CredentialCrypto,
    },
    store::{
        StoreReader,
        catalog::{Index, SegmentClock, Segments},
        disk::Slabs,
        writer::StoreWriter,
    },
    test_support::origin::AdapterOrigin,
    topology::{membership::Member, placement::Placement},
};
use std::{cell::RefCell, num::NonZeroU32, rc::Rc, sync::Arc, task::Context};

pub(crate) struct ReadWorker {
    pub coordinator: Rc<Coordinator>,
    pub streams: Rc<RangeStreams>,
    pub membership: crate::topology::membership::MembershipLease,
    pub origin: AdapterOrigin,
    pub drivers: Rc<DriverQueue>,
    endpoint: RefCell<WorkerEndpoint>,
    engine: RefCell<PageCryptoEngine>,
    crypto: Rc<CryptoClient>,
    writer: Rc<StoreWriter>,
    memory: Rc<MemoryCache>,
    cache: crate::model::CacheId,
}

impl ReadWorker {
    pub fn new(
        cache: CacheDefinition,
        metadata: ObjectMetadata,
        admission: Rc<Admission>,
        reactor: Rc<Reactor>,
        delivery: Rc<Delivery>,
        window: usize,
    ) -> Self {
        let origin = AdapterOrigin::new(&cache.name, metadata);
        let keys = Rc::new(crate::security::identity::keyring_tests::keys());
        let publications = Arc::new(PublishedState::default());
        let availability = Rc::new(crate::control::state::Availability::new(
            publications.clone(),
            keys.clone(),
        ));
        let snapshots = Rc::new(SnapshotStore::new(keys.cluster().clone(), publications, 2));
        snapshots
            .publish(Publication {
                schema_version: 1,
                cluster: keys.cluster().clone(),
                sequence: PublicationSequence(1),
                membership_version: MembershipVersion(1),
                members: vec![Member {
                    node: keys.node().clone(),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:1".into(),
                    rails: vec![],
                    alignment_enabled: false,
                }],
                caches: vec![cache.clone()],
            })
            .unwrap();
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                vec![WorkerId(0)],
                16,
            )
            .unwrap(),
        );
        let buffers = BufferPool::new(admission.clone());
        let index = Rc::new(Index::new(WorkerId(0), 16));
        let segments = Rc::new(Segments::new(WorkerId(0), 64 * 1024 * 1024));
        // Reads use an empty disk index. No slabs need to be opened or written.
        let slabs = Rc::new(Slabs::new(
            WorkerId(0),
            origin.root.join("slabs"),
            reactor.clone(),
            admission.clone(),
            256 * 1024 * 1024,
            64 * 1024 * 1024,
        ));
        let writer = Rc::new(StoreWriter::new(
            index.clone(),
            segments.clone(),
            slabs.clone(),
        ));
        let disk = Rc::new(StoreReader::new(
            Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
            index.clone(),
            segments,
            slabs,
            buffers.clone(),
        ));
        let (port, engine) = crypto::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(16).unwrap());
        let crypto = Rc::new(CryptoClient::new(port));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let candidates = Rc::new(CandidatePolicy::new(
            keys.node().clone(),
            Rc::new(Placement::new(16)),
            Rc::new(crate::test_support::NoPeers),
            credentials.clone(),
            Arc::new(Default::default()),
        ));
        let client = origin.client(
            snapshots.clone(),
            admission.clone(),
            reactor,
            buffers.clone(),
        );
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers,
            disk,
            writer: writer.clone(),
            origin: client.clone(),
            candidates: candidates.clone(),
            flights: Rc::new(Flights::new(admission.clone(), availability.clone())),
            crypto: Rc::new(PageCrypto::new(keys, crypto.clone())),
            credentials: credentials.clone(),
            admission,
            metadata_owner: directory.clone(),
        }));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            client,
            credentials.clone(),
            16,
            MetadataDependencies {
                index,
                owners: directory.clone(),
                fill: fill.clone(),
            },
        ));
        let streams = Rc::new(RangeStreams::new(directory.clone(), delivery, window));
        let membership = snapshots.current().unwrap().membership.clone();
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill,
            streams.clone(),
            credentials,
            availability,
        ));
        let endpoint = directory.install(WorkerId(0), coordinator.clone()).unwrap();
        Self {
            coordinator,
            streams,
            membership,
            origin,
            drivers: Rc::new(DriverQueue::default()),
            endpoint: RefCell::new(endpoint),
            engine: RefCell::new(PageCryptoEngine::new(CryptoRuntime { port: engine })),
            crypto,
            writer,
            memory,
            cache: cache.id,
        }
    }

    pub fn poll(&self, cx: &mut Context<'_>) {
        let _queue = self.drivers.enter();
        self.endpoint.borrow_mut().poll_budgeted(64).unwrap();
        self.drivers.poll(cx, 64);
        self.engine.borrow_mut().poll_budgeted(64).unwrap();
        self.crypto.poll_budgeted(64).unwrap();
        // Exercise acquisition/authentication, not persistence or cache retention.
        // Delivered leases remain charged even after the cache drops its copy.
        self.writer.discard_unsubmitted();
        self.memory.remove_cache(&self.cache).unwrap();
    }
}
