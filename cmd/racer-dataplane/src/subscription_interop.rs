//! Opt-in Go SDK fixture using the production library, not a second crate root.
use crate::{
    client, control, error, http, memory, model, origin, peer, read, runtime, security, store,
    topology,
};

use error::Operation;
use model::{OriginContext, PAGE_BYTES, ResourceClass, *};
use runtime::{
    admission::{Admission, Reservation},
    deadline::RequestScope,
    reactor::{IoBuffer, Reactor},
    worker::{CryptoRuntime, CryptoService, WorkerMap},
};
use std::{
    num::NonZeroUsize,
    path::PathBuf,
    rc::Rc,
    sync::Arc,
    task::Context,
    time::{Duration, Instant, UNIX_EPOCH},
};

// Only origin content is generated. No client wire framing, scheduling, credit
// accounting, page validation, encryption, or delivery is implemented here.
struct GeneratedOrigin(Rc<memory::pool::BufferPool>);
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
        expires_at: ExpiresAt(UNIX_EPOCH),
    }
}
impl origin::Origin for GeneratedOrigin {
    fn bootstrap_reserved<'a>(
        &'a self,
        authority: &'a read::candidates::OriginAuthority,
        context: &'a OriginContext,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, origin::metadata::MetadataReply> {
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
            Ok(origin::metadata::MetadataReply {
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
    ) -> Operation<'a, origin::metadata::MetadataReply> {
        Box::pin(async move {
            scope.check()?;
            Ok(origin::metadata::MetadataReply {
                metadata: metadata(context),
                page_zero: None,
            })
        })
    }
    fn page<'a>(
        &'a self,
        _: &'a read::candidates::OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: &'a RequestScope,
    ) -> Operation<'a, origin::page::OriginPage> {
        Box::pin(async { panic!("Fill must supply bounded plaintext reservation") })
    }
    fn page_reserved<'a>(
        &'a self,
        authority: &'a read::candidates::OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> Operation<'a, origin::page::OriginPage> {
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
            Ok(origin::page::OriginPage {
                metadata,
                plaintext,
            })
        })
    }
}
struct NoPeer;
impl peer::PeerClient for NoPeer {
    fn request<'a>(
        &'a self,
        _: peer::protocol::PeerRequest,
        _: topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> Operation<'a, peer::protocol::VerifiedResponse> {
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
    drivers: Rc<read::drivers::DriverQueue>,
    engine: security::aead::PageCryptoEngine,
    crypto: Rc<runtime::crypto::CryptoClient>,
    reactor: Rc<Reactor>,
    writer: Rc<store::writer::StoreWriter>,
    memory: Rc<memory::cache::MemoryCache>,
    admission: Rc<Admission>,
    cache: control::caches::CacheDefinition,
    scope: RequestScope,
}

impl SubscriptionFixture {
    fn construct(root: PathBuf) -> Self {
        use client::{RequestParser, listener::ClientListeners, response::Responses};
        use control::{
            caches::CacheDefinition,
            snapshot::{PublishedState, SnapshotStore},
            wire::*,
        };
        use memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool};
        use read::{
            Coordinator,
            candidates::CandidatePolicy,
            dispatch::WorkerDirectory,
            fill::{Fill, FillDependencies},
            flight::Flights,
            metadata::{MetadataDependencies, MetadataService},
            range_stream::RangeStreams,
        };
        use security::{
            aead::{PageCrypto, PageCryptoEngine},
            credentials::CredentialCrypto,
        };
        use store::{
            StoreReader, eviction::SegmentClock, index::Index, segment::Segments, slab::Slabs,
            writer::StoreWriter,
        };

        let mut limits = interop_limits();
        // Independent of the 512 MiB logical object: at most four plaintext pages.
        limits.plaintext_bytes = NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
        limits.ciphertext_bytes = NonZeroUsize::new(4 * (PAGE_BYTES as usize + 16)).unwrap();
        limits.range_window_pages = NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(Admission::new(limits.clone()));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let keys = Rc::new(interop_keys());
        let snapshots = Rc::new(SnapshotStore::new(
            keys.cluster().clone(),
            Arc::new(PublishedState::default()),
            2,
        ));
        let (client_socket, origin_socket) =
            control::caches::canonical_socket_paths("interop").unwrap();
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
                members: vec![topology::membership::Member {
                    node: keys.node().clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:1".into(),
                    rails: vec![],
                    alignment_enabled: false,
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
        let index = Rc::new(Index::new(WorkerId(0), 16));
        let segments = Rc::new(Segments::new(WorkerId(0), 64 * 1024 * 1024));
        let slabs = Rc::new(Slabs::new(
            WorkerId(0),
            root.join("slabs"),
            reactor.clone(),
            admission.clone(),
            256 * 1024 * 1024,
            64 * 1024 * 1024,
        ));
        slabs.open_now().unwrap();
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
        let (port, engine) = runtime::crypto::pair(WorkerId(0), 0, limits.queue_entries);
        let crypto = Rc::new(runtime::crypto::CryptoClient::new(port));
        let engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let peers = Rc::new(NoPeer);
        let candidates = Rc::new(CandidatePolicy::new(
            keys.node().clone(),
            Rc::new(topology::placement::Placement::new(16)),
            peers.clone(),
            credentials.clone(),
            Arc::new(Default::default()),
        ));
        let origin = Rc::new(GeneratedOrigin(buffers.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers,
            disk,
            writer: writer.clone(),
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights: Rc::new(Flights::new(admission.clone())),
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
            Rc::new(PipePool::new(admission.clone(), reactor.clone())),
            Duration::from_secs(10),
        ));
        let streams = Rc::new(RangeStreams::new(directory.clone(), delivery.clone(), 2));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
        ));
        let endpoint = directory.install(WorkerId(0), coordinator.clone()).unwrap();
        let io = Rc::new(http::connection::HttpIo::with_admission(
            reactor.clone(),
            http::Codec::new(32768, i64::MAX as u64),
            admission.clone(),
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
        let drivers = Rc::new(read::drivers::DriverQueue::default());
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
        self.engine.poll_budgeted(64).unwrap();
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

fn interop_keys() -> security::identity::Keyring {
    use control::wire::*;
    use security::identity::{KeyEpochs, Keyring};

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
            .map(|(i, purpose)| CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
                    id: KeyId([i as u8 + 1; 16]),
                    purpose,
                },
                state: CacheKeyState::Active,
                material: [i as u8 + 7; 32],
            })
            .collect(),
    })
    .unwrap();
    keys
}
