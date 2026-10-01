// Opt-in Go SDK interoperability using the production client and read graph.
// Compile source modules in this test crate to use crate-private fixture hooks
// without exporting test APIs or modifying production modules.
mod app;
mod client;
mod config;
mod control;
mod error;
mod http;
mod memory;
mod model;
mod origin;
mod peer;
mod rdma;
mod read;
mod runtime;
mod security;
mod store;
mod telemetry;
mod test_support;
mod topology;

use error::Operation;
use model::{
    OriginContext, *, ResourceClass, *, PAGE_BYTES,
};
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
        _: peer::wire::PeerRequest,
        _: topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> Operation<'a, peer::wire::VerifiedResponse> {
        Box::pin(async { panic!("single-node fixture contacted peer") })
    }
}

#[test]
#[ignore = "run RACER_SUBSCRIPTION_INTEROP=1 go test ./pkg/racersdk -run '^TestRustSubscriptionInterop$' -timeout=5m under external timeout"]
fn go_sdk_subscription_server() {
    use client::{listener::ClientListeners, request::RequestParser, response::Responses};
    use control::{
        caches::CacheDefinition,
        snapshot::{PublishedState, SnapshotStore},
        wire::*,
    };
    use memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool};
    use read::{
        candidates::CandidatePolicy,
        dispatch::WorkerDirectory,
        fill::{Fill, FillDependencies},
        flight::Flights,
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
        Coordinator,
    };
    use security::{
        aead::{PageCrypto, PageCryptoEngine},
        credentials::CredentialCrypto,
    };
    use store::{
        eviction::SegmentClock, index::Index, StoreReader, segment::Segments, slab::Slabs,
        writer::StoreWriter,
    };

    let root = PathBuf::from(
        std::env::var_os("RACER_SUBSCRIPTION_INTEROP_DIR").expect("Go fixture directory"),
    );
    let mut limits = test_support::cluster::config(false).limits;
    // Independent of the 512 MiB logical object: at most four plaintext pages.
    limits.plaintext_bytes = NonZeroUsize::new(4 * PAGE_BYTES as usize).unwrap();
    limits.ciphertext_bytes = NonZeroUsize::new(4 * (PAGE_BYTES as usize + 16)).unwrap();
    limits.range_window_pages = NonZeroUsize::new(2).unwrap();
    let admission = Rc::new(Admission::new(limits.clone()));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    reactor.init().unwrap();
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    let keys = Rc::new(security::keyring::tests::keys());
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
        256 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
    slabs.set_admission(admission.clone());
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
    let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
    let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
    let peers = Rc::new(NoPeer);
    let candidates = Rc::new(CandidatePolicy::new(
        keys.node().clone(),
        Rc::new(topology::placement::Placement::new(16)),
        peers.clone(),
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
    let streams = Rc::new(RangeStreams::new(
        directory.clone(),
        delivery.clone(),
        2,
    ));
    let coordinator = Rc::new(Coordinator::new(
        snapshots,
        metadata,
        fill,
        streams,
        credentials,
    ));
    let mut endpoint = directory.install(WorkerId(0), coordinator.clone()).unwrap();
    let io = Rc::new(http::io::HttpIo::with_admission(
        reactor.clone(),
        http::codec::Codec::new(32768, i64::MAX as u64),
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
    let _guard = drivers.enter();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut peak = 0;
    while !root.join("stop").exists() {
        scope.check().unwrap();
        clients.poll_budgeted(&mut cx, 64).unwrap();
        endpoint.poll_budgeted(64).unwrap();
        drivers.poll(&mut cx, 64);
        engine.poll_budgeted(64).unwrap();
        crypto.poll_budgeted(64).unwrap();
        reactor.poll_budgeted(64).unwrap();
        // This fixture tests reads, not persistence. Drop only unsubmitted dirty
        // work; memory hits and acquired page authentication remain production.
        writer.discard_unsubmitted();
        peak = peak.max(admission.used(ResourceClass::Plaintext));
        assert!(peak <= 4 * PAGE_BYTES as usize);
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    clients.cancel_cache(&cache.id).unwrap();
    clients.stop_admission();
    let until = Instant::now() + Duration::from_secs(10);
    loop {
        clients.poll_budgeted(&mut cx, 64).unwrap();
        endpoint.poll_budgeted(64).unwrap();
        drivers.poll(&mut cx, 64);
        engine.poll_budgeted(64).unwrap();
        crypto.poll_budgeted(64).unwrap();
        reactor.poll_budgeted(64).unwrap();
        writer.discard_unsubmitted();
        if clients.active_connections() == 0
            && drivers.pending() == 0
            && reactor.in_flight() == 0
            && endpoint.is_drained()
        {
            break;
        }
        assert!(Instant::now() < until, "cancel did not drain fixture");
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    memory.remove_cache(&cache.id).unwrap();
    // Returned allocation buffers remain charged until their reusable pool is
    // reclaimed. They are not live subscription leases.
    admission.reclaim_buffers();
    assert_eq!(
        admission.used(ResourceClass::Plaintext),
        0,
        "retained page lease after drain"
    );
    assert_eq!(admission.used(ResourceClass::Flight), 0);
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    println!(
        "Rust subscription interop: peak plaintext={peak}, limit={}, final plaintext/flight/waiter=0",
        4 * PAGE_BYTES
    );
}
