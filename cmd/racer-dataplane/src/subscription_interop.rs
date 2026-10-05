//! Test-only Go SDK fixture, compiled only with the `subscription-interop` feature.
//!
//! This private module stays in the library because fixture assembly needs
//! `Requester::scripted`, `ClientListeners::set_root`, and `MemoryCache::remove_cache`,
//! which are crate-private. Moving it to the integration-test crate would expose
//! those internals or compile a second crate root. Only the launcher is reexported.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::client;
use crate::config::Limits;
use crate::control;
use crate::error;
use crate::http;
use crate::memory;
use crate::model;
use crate::origin;
use crate::peer;
use crate::read;
use crate::read::dispatch::WorkerMap;
use crate::runtime;
use crate::security;
use crate::store;
use crate::topology;
use crate::worker::CryptoRuntime;
use error::Operation;
use model::PAGE_BYTES;
use model::*;
use racer_control_wire::BundleGeneration;
use racer_control_wire::CacheEncryptionKey;
use racer_control_wire::CacheKeyPurpose;
use racer_control_wire::CacheKeyRef;
use racer_control_wire::CacheKeyState;
use racer_control_wire::KeyringBundle;
use racer_control_wire::SCHEMA_VERSION;
use racer_control_wire::{CacheId, ClusterId, NodeId};
use racer_identity::KeyEpochs;
use racer_identity::Keyring;
use runtime::Reactor;
use runtime::RequestScope;
use security::OriginContext;
use std::num::NonZeroUsize;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::time::Duration;
use std::time::Instant;
use std::time::UNIX_EPOCH;
use uring_runtime::reactor::IoBuffer;

// Only origin content is generated. No client wire framing, scheduling, credit
// accounting, page validation, encryption, or delivery is implemented here.
struct GeneratedOrigin(memory::BufferPool);
fn metadata(context: &OriginContext) -> ObjectMetadata {
    let length = match context.object.key.0[0] {
        0 => 0,
        1 => 4096,
        2 => PAGE_BYTES,
        3 => 3 * PAGE_BYTES + 13,
        4 => 32 * PAGE_BYTES + 13,
        _ => 8 * PAGE_BYTES + 13,
    };
    ObjectMetadata {
        version: ObjectVersion {
            object: context.object.clone(),
            etag: StrongEtag::parse(b"\"interop\"").unwrap(),
        },
        length,
        content_type: None,
        expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
    }
}
impl origin::Origin for GeneratedOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        authority: &'a read::candidates::OriginAuthority,
        context: &'a OriginContext,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, origin::MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, PageNumber(0))?;
            let metadata = metadata(context);
            let page = PageId {
                version: metadata.version.clone(),
                number: PageNumber(0),
            };
            let page_zero = if metadata.length == 0 {
                None
            } else {
                Some(
                    self.page_reserved(authority, context, &page, reservation, scope)
                        .await?,
                )
            };
            Ok(origin::MetadataReply {
                metadata,
                page_zero,
            })
        })
    }
    fn metadata<'a>(
        &'a self,
        _: &'a read::candidates::OriginAuthority,
        context: &'a OriginContext,
        _: MetadataSelector,
        scope: &'a RequestScope,
    ) -> Operation<'a, origin::MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            Ok(origin::MetadataReply {
                metadata: metadata(context),
                page_zero: None,
            })
        })
    }
    fn page_reserved<'a>(
        &'a self,
        authority: &'a read::candidates::OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, origin::OriginPage> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, page.number)?;
            let metadata = metadata(context);
            let length = metadata.immutable().page_length(page)? as usize;
            let mut plaintext = self.0.plaintext(reservation, length)?;
            let offset = page.number.0 * PAGE_BYTES;
            for (i, byte) in plaintext.bytes_mut()?.iter_mut().enumerate() {
                *byte = ((offset + i as u64) % 251) as u8;
            }
            Ok(origin::OriginPage {
                metadata,
                plaintext,
            })
        })
    }
}
struct NoPeer;
impl NoPeer {
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &racer_control_wire::NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _: peer::protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, peer::forwarding::VerifiedResponse> {
        panic!("single-node fixture must not hedge to a peer")
    }
    fn request<'a>(
        &'a self,
        _: peer::protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a RequestScope,
    ) -> Operation<'a, peer::forwarding::VerifiedResponse> {
        Box::pin(async { panic!("single-node fixture contacted peer") })
    }
}

pub fn go_sdk_subscription_server() {
    let root = PathBuf::from(
        std::env::var_os("RACER_SUBSCRIPTION_INTEROP_DIR").expect("Go fixture directory"),
    );
    let mut fixture = SubscriptionFixture::construct(root);
    let peak = fixture.serve();
    fixture.teardown();
    println!(
        "Rust subscription interop: peak plaintext={peak}, limit={}, final plaintext/flight/waiter=0",
        4 * PAGE_BYTES
    );
}

struct SubscriptionFixture {
    root: PathBuf,
    clients: client::listener::ClientListeners,
    endpoint: read::dispatch::WorkerEndpoint,
    drivers: Rc<uring_runtime::drivers::DriverQueue>,
    engine: security::PageCryptoEngine,
    crypto: Rc<security::CryptoClient>,
    reactor: Rc<Reactor>,
    writer: Rc<store::StoreWriter>,
    memory: Rc<memory::MemoryCache>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    cache: racer_control_wire::CacheDefinition,
    scope: RequestScope,
}

impl SubscriptionFixture {
    fn construct(root: PathBuf) -> Self {
        use client::RequestParser;
        use client::Responses;
        use client::listener::ClientListeners;
        use control::PublishedState;
        use control::SnapshotStore;
        use http::Delivery;
        use http::new_pipe_pool;
        use memory::BufferPool;
        use memory::MemoryCache;
        use racer_control_wire::CacheDefinition;
        use racer_control_wire::Publication;
        use racer_control_wire::*;
        use read::Coordinator;
        use read::candidates::CandidatePolicy;
        use read::dispatch::WorkerDirectory;
        use read::fill::Fill;
        use read::fill::FillDependencies;
        use read::flight::Flights;
        use read::metadata::MetadataDependencies;
        use read::metadata::MetadataService;
        use read::range_stream::RangeStreams;
        use security::CredentialCrypto;
        use security::PageCrypto;
        use security::PageCryptoEngine;
        use store::StoreReader;
        use store::StoreWriter;
        use store::catalog::Index;
        use store::catalog::SegmentClock;

        let mut limits = interop_limits();
        // Independent of the 512 MiB logical object: at most four plaintext pages.
        limits.plaintext_bytes = NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
        limits.ciphertext_bytes = NonZeroUsize::new(4 * (PAGE_BYTES as usize + 16)).unwrap();
        limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            limits.clone(),
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let buffers = BufferPool::new(admission.clone());
        let keys = Rc::new(interop_keys());
        let published = Arc::new(PublishedState::default());
        let availability = Rc::new(control::Availability::new(published.clone(), keys.clone()));
        let snapshots = Rc::new(SnapshotStore::new(keys.cluster().clone(), published, 2));
        let (client_socket, origin_socket) =
            racer_control_wire::canonical_socket_paths("interop").unwrap();
        let cache = CacheDefinition {
            id: CacheId("33333333-3333-4333-8333-333333333333".into()),
            name: "interop".into(),
            client_socket,
            origin_socket,
        };
        snapshots
            .publish(Publication {
                schema_version: 1,
                cluster: keys.cluster().clone(),
                sequence: PublicationSequence(1),
                membership_version: MembershipVersion(1),
                members: vec![racer_control_wire::Member {
                    node: keys.node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:1".into(),
                    rails: vec![],
                    site: String::new(),
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
        let index = Rc::new(Index::new(WorkerId(0), 16, availability.clone()));
        let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
        let slabs = Rc::new(page_alloc::Slab::new(
            root.join("slabs/worker-0-slab-0.dat"),
            256 * 1024 * 1024,
            64 * 1024 * 1024,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
        ));
        slabs.open_now().unwrap();
        let writer = Rc::new(StoreWriter::new(
            index.clone(),
            segments.clone(),
            slabs.clone(),
            admission.clone(),
            reactor.clone(),
            availability.clone(),
        ));
        let disk = Rc::new(StoreReader::new(
            Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1)),
            index.clone(),
            segments,
            slabs,
            admission.clone(),
            reactor.clone(),
            buffers.clone(),
        ));
        let (port, engine) = security::pair(WorkerId(0), 0, limits.queue_entries);
        let crypto = Rc::new(security::CryptoClient::new(port));
        let engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let peers = peer::Requester::scripted(
            Rc::new(NoPeer),
            NoPeer::direct_hedge_available,
            NoPeer::request,
            NoPeer::request_direct,
        );
        let candidates = Rc::new(CandidatePolicy::new(
            keys.node().clone(),
            Rc::new(topology::Placement::new(16)),
            peers.clone(),
            credentials.clone(),
            Arc::new(Default::default()),
        ));
        let origin = Rc::new(GeneratedOrigin(buffers.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers,
            disk,
            writer: writer.clone(),
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights: Rc::new(Flights::new(admission.clone(), availability.clone())),
            crypto: Rc::new(PageCrypto::new(keys, crypto.clone())),
            credentials: credentials.clone(),
            admission: admission.clone(),
            metadata_owner: directory.clone(),
        }));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            origin,
            credentials.clone(),
            16,
            MetadataDependencies {
                index,
                owners: directory.clone(),
                fill: fill.clone(),
            },
        ));
        let delivery = Rc::new(Delivery::new(
            Rc::new(new_pipe_pool(admission.clone())),
            reactor.clone(),
            Duration::from_secs(10),
        ));
        let streams = Rc::new(RangeStreams::new(directory.clone(), delivery.clone(), 2));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
            availability,
        ));
        let endpoint = directory.install(WorkerId(0), coordinator.clone()).unwrap();
        let io = Rc::new(http::new_io(
            reactor.clone(),
            http::Codec::new(32768),
            admission.clone(),
            i64::MAX as u64,
        ));
        let responses = Rc::new(Responses::new(io.clone(), delivery));
        let mut clients = ClientListeners::new(
            coordinator,
            RequestParser::new(32768),
            responses,
            io,
            admission.clone(),
        );
        clients.set_root(root.clone());
        let scope = RequestScope::new(
            RequestId([1; 16]),
            Instant::now() + Duration::from_secs(180),
        )
        .unwrap();
        futures::executor::block_on(clients.reconcile(&[cache.clone()], &scope)).unwrap();
        std::fs::write(root.join("ready"), b"ready").unwrap();
        let drivers = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        Self {
            root,
            clients,
            endpoint,
            drivers,
            engine,
            crypto,
            reactor,
            writer,
            memory,
            admission,
            cache,
            scope,
        }
    }

    fn serve(&mut self) -> usize {
        let _guard = self.drivers.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut peak = 0;
        while !self.root.join("stop").exists() {
            self.scope.check().unwrap();
            self.progress(&mut cx);
            // This fixture tests reads, not persistence. Drop only unsubmitted dirty
            // work; memory hits and acquired page authentication remain production.
            peak = peak.max(self.admission.used(ResourceClass::Plaintext));
            assert!(peak <= 4 * PAGE_BYTES as usize);
            self.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        peak
    }

    fn progress(&mut self, cx: &mut Context<'_>) {
        self.clients.poll_budgeted(cx, 64).unwrap();
        self.endpoint.poll_budgeted(64).unwrap();
        self.drivers.poll(cx, 64);
        uring_runtime::group::Service::poll_budgeted(
            &mut self.engine,
            &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )
        .unwrap();
        self.crypto.poll_budgeted(64).unwrap();
        self.reactor.poll_budgeted(64).unwrap();
        self.writer.discard_unsubmitted();
    }

    fn teardown(&mut self) {
        let _guard = self.drivers.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        self.clients.cancel_cache(&self.cache.id).unwrap();
        self.clients.stop_admission();
        let until = Instant::now() + Duration::from_secs(10);
        loop {
            self.progress(&mut cx);
            if self.clients.active_connections() == 0
                && self.drivers.pending() == 0
                && self.reactor.in_flight() == 0
                && self.endpoint.is_drained()
            {
                break;
            }
            assert!(Instant::now() < until, "cancel did not drain fixture");
            self.reactor.wait(Duration::from_millis(1)).unwrap();
        }
        self.memory.remove_cache(&self.cache.id).unwrap();
        // Returned allocation buffers remain charged until their reusable pool is
        // reclaimed. They are not live subscription leases.
        self.admission.reclaim_buffers();
        assert_eq!(
            self.admission.used(ResourceClass::Plaintext),
            0,
            "retained page lease after drain"
        );
        assert_eq!(self.admission.used(ResourceClass::Flight), 0);
        assert_eq!(self.admission.used(ResourceClass::Waiter), 0);
    }
}

fn interop_limits() -> Limits {
    let count = NonZeroUsize::new(16).unwrap();
    let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
    Limits {
        plaintext_bytes: bytes,
        ciphertext_bytes: bytes,
        dirty_bytes: bytes,
        registered_bytes: bytes,
        request_context_bytes: bytes,
        flights: count,
        waiters_per_flight: count,
        queue_entries: count,
        connections_per_neighbor: count,
        client_connections: count,
        pipes: count,
        range_window_pages: count,
        header_bytes: NonZeroUsize::new(16 * 1024).unwrap(),
        cached_rankings: count,
        cached_paths: count,
        retained_snapshots: count,
        metadata_entries: count,
        relay_transfers: count,
    }
}

fn interop_keys() -> racer_identity::Keyring {
    let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
    let keys = Keyring::new(
        cluster.clone(),
        NodeId("22222222-2222-4222-8222-222222222222".into()),
        Arc::new(KeyEpochs::default()),
    );
    let mut ca = rcgen::CertificateParams::new(vec![]).unwrap();
    ca.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    let ca = ca
        .self_signed(&rcgen::KeyPair::generate().unwrap())
        .unwrap();
    keys.install(KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster,
        generation: BundleGeneration(1),
        peer_trust_roots: vec![ca.der().to_vec()],
        cache_keys: [CacheKeyPurpose::Page, CacheKeyPurpose::OriginCredentials]
            .into_iter()
            .enumerate()
            .map(|(i, purpose)| {
                CacheEncryptionKey::new(
                    CacheKeyRef {
                        cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
                        id: crate::model::key_id_from_generation(1, i as u32 + 1).unwrap(),
                        purpose,
                    },
                    CacheKeyState::Active,
                    zeroize::Zeroizing::new([i as u8 + 7; 32]),
                )
            })
            .collect(),
    })
    .unwrap();
    keys
}
