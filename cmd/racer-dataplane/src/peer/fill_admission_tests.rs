//! Real Fill/election and signed TCP relay sharing the same worker admission.
use super::*;
use crate::{
    control::{
        caches::{CacheDefinition, canonical_socket_paths},
        snapshot::{PublishedState, SnapshotStore},
        wire::{Publication, PublicationSequence},
    },
    error::Operation as FutureResult,
    http::{
        codec::Codec,
        io::HttpIo,
        pool::{ConnectionLease, Endpoint, HttpPool},
    },
    memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool},
    model::{
        context::OriginContext,
        metadata::{ExpiresAt, ObjectMetadata},
        range::PAGE_BYTES,
    },
    origin::{client::Origin, metadata::MetadataReply, page::OriginPage},
    read::{
        candidates::{CandidatePolicy, OriginAuthority},
        dispatch::WorkerDirectory,
        fill::{Fill, FillDependencies},
        flight::{AcquisitionBudget, Flights},
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
        serve::Coordinator,
    },
    runtime::{
        admission::Reservation,
        crypto::{self, CryptoClient},
        reactor::{IoBuffer, Reactor},
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::{
        aead::{PageCrypto, PageCryptoEngine},
        credentials::CredentialCrypto,
    },
    store::{
        eviction::SegmentClock, index::Index, reader::StoreReader, segment::Segments, slab::Slabs,
        writer::StoreWriter,
    },
    topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        paths::Paths,
        placement::Placement,
        rails::Rails,
    },
};
use futures::{StreamExt, stream::FuturesUnordered};
use sha2::{Digest, Sha256};
use std::{
    cell::{Cell, RefCell},
    collections::HashMap,
    future::Future,
    net::TcpListener,
    num::{NonZeroU32, NonZeroUsize},
    os::fd::OwnedFd,
    task::Context,
    time::SystemTime,
};

const P: usize = PAGE_BYTES as usize;
struct Data {
    metadata_enabled: bool,
    bytes: HashMap<CacheKey, Vec<u8>>,
    calls: RefCell<HashMap<PageId, usize>>,
}
struct Adapter {
    data: Rc<Data>,
    buffers: Rc<BufferPool>,
}
impl Origin for Adapter {
    fn metadata<'a>(
        &'a self,
        _: &'a OriginAuthority,
        context: &'a OriginContext,
        _: MetadataSelector,
        _: &'a RequestScope,
    ) -> FutureResult<'a, MetadataReply> {
        Box::pin(async move {
            assert!(
                self.data.metadata_enabled,
                "pinned fixture must not refresh"
            );
            Ok(MetadataReply {
                metadata: ObjectMetadata {
                    version: page(context.object.key, 0).version,
                    length: self.data.bytes[&context.object.key].len() as u64,
                    expires_at: ExpiresAt::from_unix_millis(
                        (SystemTime::now()
                            .duration_since(SystemTime::UNIX_EPOCH)
                            .unwrap()
                            .as_millis()
                            + 60_000) as u64,
                    )
                    .unwrap(),
                },
                page_zero: None,
            })
        })
    }
    fn page<'a>(
        &'a self,
        _: &'a OriginAuthority,
        _: &'a OriginContext,
        _: &'a PageId,
        _: &'a RequestScope,
    ) -> FutureResult<'a, OriginPage> {
        Box::pin(async { panic!("Fill must supply admission") })
    }
    fn page_reserved<'a>(
        &'a self,
        authority: &'a OriginAuthority,
        context: &'a OriginContext,
        page: &'a PageId,
        reservation: Reservation,
        scope: &'a RequestScope,
    ) -> FutureResult<'a, OriginPage> {
        Box::pin(async move {
            scope.check()?;
            authority.validate(&context.object, page.number)?;
            *self
                .data
                .calls
                .borrow_mut()
                .entry(page.clone())
                .or_default() += 1;
            let data = &self.data.bytes[&page.version.object.key];
            let start = page.number.0 as usize * P;
            let bytes = &data[start..data.len().min(start + P)];
            let mut plaintext = self.buffers.plaintext(reservation, bytes.len())?;
            plaintext.bytes_mut()?.copy_from_slice(bytes);
            Ok(OriginPage {
                plaintext,
                metadata: ObjectMetadata {
                    version: page.version.clone(),
                    length: data.len() as u64,
                    expires_at: ExpiresAt::from_unix_millis(
                        (SystemTime::now()
                            .duration_since(SystemTime::UNIX_EPOCH)
                            .unwrap()
                            .as_millis()
                            + 60_000) as u64,
                    )
                    .unwrap(),
                },
            })
        })
    }
}
struct Node {
    coordinator: Rc<Coordinator>,
    owners: Arc<WorkerDirectory>,
    client_io: Rc<HttpIo>,
    responses: Rc<crate::client::response::Responses>,
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    crypto: Rc<CryptoClient>,
    engine: RefCell<PageCryptoEngine>,
    fill: Rc<Fill>,
    memory: Rc<MemoryCache>,
    writer: Rc<StoreWriter>,
    flights: Rc<Flights>,
    server: server::PeerServer,
    transfers: Rc<transfer::Transfers>,
    pool: Rc<HttpPool>,
    credentials: Rc<CredentialCrypto>,
    directory: std::path::PathBuf,
}
impl Drop for Node {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.directory);
    }
}
fn object(key: CacheKey) -> ObjectId {
    ObjectId {
        cache: CacheId(CACHE.into()),
        key,
    }
}
fn page(key: CacheKey, number: u64) -> PageId {
    PageId {
        version: ObjectVersion {
            object: object(key),
            etag: StrongEtag::parse(b"\"fixture\"").unwrap(),
        },
        number: PageNumber(number),
    }
}
fn context(key: CacheKey) -> OriginContext {
    OriginContext {
        object: object(key),
        metadata: None,
        authorization: None,
    }
}
fn build_node(
    i: usize,
    membership: Arc<Membership>,
    signer: Rc<Signatures>,
    discovery: &Discovery,
    data: Rc<Data>,
    concurrency: usize,
) -> Node {
    build_node_with_peer_limit(
        i,
        membership,
        signer,
        discovery,
        data,
        concurrency,
        concurrency + 1,
    )
}
fn build_node_with_peer_limit(
    i: usize,
    membership: Arc<Membership>,
    signer: Rc<Signatures>,
    discovery: &Discovery,
    data: Rc<Data>,
    concurrency: usize,
    peer_limit: usize,
) -> Node {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.ciphertext_bytes = NonZeroUsize::new((concurrency + 1) * (P + 16)).unwrap();
    limits.plaintext_bytes = NonZeroUsize::new((concurrency + 3) * P).unwrap();
    limits.dirty_bytes = NonZeroUsize::new((concurrency + 1) * (P + 16)).unwrap();
    limits.client_connections = NonZeroUsize::new(64).unwrap();
    limits.request_context_bytes = NonZeroUsize::new(8 * 1024 * 1024).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let buffers = Rc::new(BufferPool::new(admission.clone()));
    let memory = Rc::new(MemoryCache::new(buffers.clone()));
    let index = Rc::new(Index::new(WorkerId(0), 128));
    let segments = Rc::new(Segments::new(WorkerId(0), 64 * 1024 * 1024));
    let directory = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("target")
        .join(format!(
            "fill-fleet-{}-{:?}-{concurrency}-{peer_limit}-{i}",
            std::process::id(),
            std::thread::current().id()
        ));
    let slabs = Rc::new(Slabs::new(
        WorkerId(0),
        directory.clone(),
        reactor.clone(),
        512 * 1024 * 1024,
        64 * 1024 * 1024,
    ));
    slabs.set_admission(admission.clone());
    slabs.open_now().unwrap();
    let eviction = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
    let disk = Rc::new(StoreReader::new(
        eviction.clone(),
        index.clone(),
        segments.clone(),
        slabs.clone(),
        buffers.clone(),
    ));
    let writer = Rc::new(StoreWriter::new(index.clone(), segments, slabs));
    writer
        .configure(admission.clone(), eviction, 128, 128)
        .unwrap();
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(wire::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
        admission.clone(),
    ));
    let pool = Rc::new(HttpPool::new(
        reactor.clone(),
        admission.clone(),
        peer_limit,
    ));
    let codec = Rc::new(codec(&admission));
    let transfers = Rc::new(
        transfer::Transfers::new(pool.clone(), io.clone(), None)
            .with_wire(admission.clone(), codec.clone())
            .with_receive_reclamation(memory.clone(), writer.clone()),
    );
    let network = Rc::new(PeerNetwork::new(signer.node().clone(), 2).unwrap());
    network.install(membership.clone()).unwrap();
    let auth = Rc::new(Forwarding::new(signer.clone()));
    let handshake = Rc::new(
        handshake::Handshake::new(signer.clone(), None)
            .with_http(network.clone(), transfers.clone())
            .with_discovery(
                discovery.0.clone(),
                discovery.1.clone(),
                discovery.2.clone(),
            ),
    );
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 128, 4096));
    let peers = Rc::new(
        requester::Requester::new(
            paths.clone(),
            Rc::new(Rails),
            auth.clone(),
            handshake.clone(),
            transfers.clone(),
        )
        .with_network(network.clone()),
    );
    let candidates = Rc::new(CandidatePolicy::new(
        signer.node().clone(),
        Rc::new(Placement::new(128)),
        peers.clone(),
    ));
    let credentials = Rc::new(CredentialCrypto::new(
        discovery.0.clone(),
        admission.clone(),
    ));
    let (port, engine) = crypto::pair(WorkerId(0), 0, NonZeroUsize::new(64).unwrap());
    let crypto = Rc::new(CryptoClient::new(port));
    let owners = Arc::new(
        WorkerDirectory::new(
            Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
            vec![WorkerId(0)],
            128,
        )
        .unwrap(),
    );
    let origin = Rc::new(Adapter {
        data,
        buffers: buffers.clone(),
    });
    let flights = Rc::new(Flights::new(admission.clone()));
    let fill = Rc::new(Fill::new(FillDependencies {
        memory: memory.clone(),
        buffers,
        disk,
        writer: writer.clone(),
        peers: peers.clone(),
        origin: origin.clone(),
        candidates: candidates.clone(),
        flights: flights.clone(),
        crypto: Rc::new(PageCrypto::new(discovery.0.clone(), crypto.clone())),
        credentials: credentials.clone(),
        admission: admission.clone(),
        metadata_owner: owners.clone(),
    }));
    let metadata = Rc::new(MetadataService::new(
        candidates,
        origin,
        peers.clone(),
        credentials.clone(),
        128,
        MetadataDependencies {
            index,
            fill: fill.clone(),
            owners: owners.clone(),
        },
    ));
    let snapshots = Rc::new(SnapshotStore::new(
        ClusterId(CLUSTER.into()),
        Arc::new(PublishedState::default()),
        2,
    ));
    let (client_socket, origin_socket) = canonical_socket_paths("fixture").unwrap();
    snapshots
        .publish(Publication {
            schema_version: 1,
            cluster: ClusterId(CLUSTER.into()),
            sequence: PublicationSequence(1),
            membership_version: membership.version,
            members: membership.members().to_vec(),
            caches: vec![CacheDefinition {
                id: CacheId(CACHE.into()),
                name: "fixture".into(),
                client_socket,
                origin_socket,
                socket_mode: 0o600,
            }],
        })
        .unwrap();
    let delivery = Rc::new(Delivery::new(
        Rc::new(PipePool::new(admission.clone(), reactor.clone())),
        Duration::from_secs(10),
    ));
    let streams = Rc::new(RangeStreams::new(
        fill.clone(),
        owners.clone(),
        delivery.clone(),
        2,
    ));
    let coordinator = Rc::new(Coordinator::new(
        snapshots,
        metadata,
        fill.clone(),
        streams,
        credentials.clone(),
    ));
    let relay = Rc::new(
        relay::Relay::new(paths, auth.clone(), peers, admission.clone())
            .with_network(network.clone())
            .with_handshake(handshake.clone()),
    );
    let server = server::PeerServer::new(io, auth, admission.clone(), coordinator.clone(), relay)
        .with_network(network)
        .with_wire(codec)
        .with_handshake(handshake);
    let client_io = Rc::new(HttpIo::for_clients(reactor.clone(), admission.clone()));
    let responses = Rc::new(crate::client::response::Responses::new(
        client_io.clone(),
        delivery,
    ));
    Node {
        coordinator,
        owners,
        client_io,
        responses,
        admission,
        reactor,
        crypto,
        engine: RefCell::new(PageCryptoEngine::new(CryptoRuntime { port: engine })),
        fill,
        memory,
        writer,
        flights,
        server,
        transfers,
        pool,
        credentials,
        directory,
    }
}

#[path = "production_stream_tests.rs"]
mod production_stream_tests;

#[test]
#[ignore = "full production Fill/election eight-layer TCP regression: run with --release"]
fn full_image_real_fill_reclaims_relay_cache() {
    for concurrency in [1, 4] {
        run(concurrency);
    }
}
fn run(concurrency: usize) {
    let (signers, discovery) = identities_with_replay_capacity(8192);
    for signer in &signers {
        for peer in &signers {
            signer
                .configure_authenticated_peer_challenge(
                    peer.node().clone(),
                    peer.challenge().unwrap(),
                )
                .unwrap();
        }
    }
    // Install distinct test-only page and credential keys into real keyrings.
    for (keys, _, _) in &discovery {
        let mut bundle: serde_json::Value =
            serde_json::from_slice(include_bytes!("../control/testdata/bundle.json")).unwrap();
        bundle["cluster"] = CLUSTER.into();
        bundle["generation"] = "2".into();
        for (i, key) in bundle["cache_keys"]
            .as_array_mut()
            .unwrap()
            .iter_mut()
            .enumerate()
        {
            key["cache"] = CACHE.into();
            key["material"] = base64::Engine::encode(
                &base64::engine::general_purpose::STANDARD,
                [i as u8 + 7; 32],
            )
            .into();
        }
        // Retain the generated trust roots for signed peer verification.
        bundle["peer_trust_roots"] = serde_json::to_value(
            keys.peer_trust_roots()
                .unwrap()
                .iter()
                .map(|root| {
                    base64::Engine::encode(&base64::engine::general_purpose::STANDARD, root)
                })
                .collect::<Vec<_>>(),
        )
        .unwrap();
        keys.install(
            crate::control::wire::decode_bundle(&serde_json::to_vec(&bundle).unwrap()).unwrap(),
        )
        .unwrap();
    }
    let listeners: Vec<_> = (0..3)
        .map(|_| {
            let l = TcpListener::bind("127.0.0.1:0").unwrap();
            l.set_nonblocking(true).unwrap();
            l
        })
        .collect();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            signers
                .iter()
                .zip(&listeners)
                .map(|(s, l)| Member {
                    node: s.node().clone(),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: l.local_addr().unwrap().to_string(),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let placement = Placement::new(256);
    let mut bytes = HashMap::new();
    let mut keys = Vec::new();
    for layer in 0..8u8 {
        let body = vec![layer; 4 * P + 17];
        let key = CacheKey(Sha256::digest(&body).into());
        bytes.insert(key, body);
        keys.push(key);
    }
    let config = br#"{"architecture":"amd64","os":"linux"}"#.to_vec();
    let config_key = CacheKey(Sha256::digest(&config).into());
    let manifest = serde_json::to_vec(&serde_json::json!({"schemaVersion":2,"config":{"digest":format!("sha256:{:x}",Sha256::digest(&config)),"size":config.len()},"layers":keys.iter().map(|key| serde_json::json!({"digest":format!("sha256:{:x}",Sha256::digest(&bytes[key])),"size":bytes[key].len()})).collect::<Vec<_>>()})).unwrap();
    let manifest_key = CacheKey(Sha256::digest(&manifest).into());
    bytes.insert(config_key, config);
    bytes.insert(manifest_key, manifest);
    // Real local Fill warms a page whose elected origin supplier is each node.
    let warm: Vec<_> = signers
        .iter()
        .enumerate()
        .map(|(i, s)| {
            (0..10000u32)
                .filter_map(|n| {
                    let mut key = [0; 32];
                    key[..4].copy_from_slice(&n.to_le_bytes());
                    key[4] = i as u8;
                    let key = CacheKey(key);
                    (placement
                        .rank(membership.clone(), &object(key), PageNumber(0))
                        .unwrap()
                        .ordered[0]
                        == *s.node())
                    .then_some(key)
                })
                .take(concurrency + 1)
                .collect::<Vec<_>>()
        })
        .collect();
    for key in warm.iter().flatten() {
        bytes.insert(*key, vec![42; P]);
    }
    let data = Rc::new(Data {
        metadata_enabled: false,
        bytes,
        calls: RefCell::new(HashMap::new()),
    });
    let nodes: Vec<_> = (0..3)
        .map(|i| {
            build_node(
                i,
                membership.clone(),
                signers[i].clone(),
                &discovery[i],
                data.clone(),
                concurrency,
            )
        })
        .collect();
    let scope = RequestScope::new(
        RequestId([19; 16]),
        Instant::now() + Duration::from_secs(100),
    )
    .unwrap();
    let drive = |mut work: std::pin::Pin<Box<dyn Future<Output = ()> + '_>>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if work.as_mut().poll(&mut cx).is_ready() {
                break;
            }
            scope.check().unwrap();
            for node in &nodes {
                node.reactor.poll_budgeted(256).unwrap();
                node.engine.borrow_mut().poll_budgeted(64).unwrap();
                node.crypto.poll_budgeted(64).unwrap();
                node.flights.poll_with_context(&mut cx, 64).unwrap();
            }
            crate::read::drivers::poll(&mut cx, 64);
            nodes[0].reactor.wait(Duration::from_micros(100)).unwrap();
        }
    };
    let servers = async {
        let mut all = FuturesUnordered::new();
        for (node, listener) in nodes.iter().zip(&listeners) {
            let fd = Rc::new(OwnedFd::from(listener.try_clone().unwrap()));
            let scope = &scope;
            all.push(async move {
                let mut active = FuturesUnordered::new();
                loop {
                    let accept = node.reactor.accept(fd.clone(),scope);
                    futures::pin_mut!(accept);
                    let accepted = loop {
                        if active.is_empty() { break accept.await; }
                        use futures::FutureExt;
                        futures::select_biased! { _ = active.next().fuse() => {}, result = accept.as_mut().fuse() => break result }
                    }?;
                    let mut connection = ConnectionLease::from_accepted(accepted,&node.admission)?;
                    active.push(async move { loop { connection = node.server.serve_connection(connection,scope).await?; if !connection.is_reusable() { return Ok::<(),Error>(()); } } });
                }
                #[allow(unreachable_code)] Ok::<(),Error>(())
            });
        }
        all.next().await.unwrap()
    };
    let completed = Cell::new(0usize);
    let clients = async {
        for batch in [vec![manifest_key], vec![config_key]]
            .into_iter()
            .chain(keys.chunks(concurrency).map(|v| v.to_vec()))
        {
            // Populate every relay's actual idle local cache to its byte limit.
            for (i, node) in nodes.iter().enumerate() {
                node.writer.discard_unsubmitted();
                node.memory.evict_idle(usize::MAX).unwrap();
                for key in &warm[i] {
                    let ctx = context(*key);
                    let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
                    let mut follower_budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
                    let before = data
                        .calls
                        .borrow()
                        .get(&page(*key, 0))
                        .copied()
                        .unwrap_or(0);
                    let (leader, follower) = futures::future::join(
                        node.fill.acquire(
                            page(*key, 0),
                            membership.clone(),
                            &ctx,
                            &scope,
                            &mut budget,
                        ),
                        node.fill.acquire(
                            page(*key, 0),
                            membership.clone(),
                            &ctx,
                            &scope,
                            &mut follower_budget,
                        ),
                    )
                    .await;
                    let result = leader.unwrap();
                    let follower = follower.unwrap();
                    assert!(Arc::ptr_eq(
                        &result.plaintext.inner,
                        &follower.plaintext.inner
                    ));
                    assert!(Arc::ptr_eq(
                        &result.ciphertext.inner,
                        &follower.ciphertext.inner
                    ));
                    assert_eq!(data.calls.borrow().get(&page(*key, 0)), Some(&(before + 1)));
                    assert_eq!(result.plaintext.bytes(), &data.bytes[key]);
                    drop((result, follower));
                    node.writer.discard_unsubmitted();
                }
                assert_eq!(
                    node.admission.used(ResourceClass::Ciphertext),
                    node.admission.limit(ResourceClass::Ciphertext)
                );
                // Keep one actual independently held page protected while a
                // receive evicts exactly one other idle full-page allocation.
                let protected = node.memory.get(&page(warm[i][0], 0)).unwrap().unwrap();
                assert!(matches!(
                    node.transfers
                        .receive_buffer(&node.admission, &CacheId(CACHE.into()), P + 17),
                    Err(Error::InvalidRequest)
                ));
                assert!(
                    node.memory
                        .get(&page(*warm[i].last().unwrap(), 0))
                        .unwrap()
                        .is_some()
                );
                let incoming = node
                    .transfers
                    .receive_buffer(&node.admission, &CacheId(CACHE.into()), P + 16)
                    .unwrap();
                assert!(node.memory.get(&page(warm[i][0], 0)).unwrap().is_some());
                assert_eq!(protected.plaintext.bytes(), &data.bytes[&warm[i][0]]);
                assert_eq!(
                    node.admission.used(ResourceClass::Ciphertext),
                    node.admission.limit(ResourceClass::Ciphertext)
                );
                drop((incoming, protected));
                // Refill the evicted page through production Fill, retaining its
                // queued persistence too when staging fits the quota.
                for key in &warm[i] {
                    if node.memory.get(&page(*key, 0)).unwrap().is_none() {
                        let ctx = context(*key);
                        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
                        drop(
                            node.fill
                                .acquire(
                                    page(*key, 0),
                                    membership.clone(),
                                    &ctx,
                                    &scope,
                                    &mut budget,
                                )
                                .await
                                .unwrap(),
                        );
                    }
                }
            }
            let mut jobs = FuturesUnordered::new();
            for key in batch {
                let nodes = &nodes;
                let signers = &signers;
                let membership = &membership;
                let placement = &placement;
                let scope = &scope;
                let data = &data;
                let completed = &completed;
                let discovery = &discovery;
                jobs.push(async move {
                    let mut hash = Sha256::new();
                    let mut size = 0;
                    let mut small_body = Vec::new();
                    for number in 0..data.bytes[&key].len().div_ceil(P) {
                        let page = page(key, number as u64);
                        let ranked = placement
                            .rank(membership.clone(), &page.version.object, page.number)
                            .unwrap();
                        let dest = signers
                            .iter()
                            // The tail uses the second candidate, exercising a
                            // real copy-only predecessor probe before origin.
                            .position(|s| *s.node() == ranked.ordered[usize::from(number == 4)])
                            .unwrap();
                        let source = (dest + 1) % 3;
                        let relay = (dest + 2) % 3;
                        let mut id = [0; 16];
                        id[..8].copy_from_slice(
                            &(completed.get() as u64 + size as u64 + 1).to_le_bytes(),
                        );
                        getrandom::getrandom(&mut id[8..]).unwrap();
                        let request_scope =
                            RequestScope::new(RequestId(id), scope.deadline.0).unwrap();
                        let attempt = AttemptId(id);
                        let request = PeerRequest {
                            operation: Operation::Page {
                                page: page.clone(),
                                mode: FetchMode::Acquire,
                            },
                            origin: nodes[source]
                                .credentials
                                .seal(&context(key), attempt, &request_scope)
                                .unwrap(),
                            route: RouteBudget {
                                membership: membership.version,
                                request: request_scope.request,
                                attempt,
                                destination: signers[dest].node().clone(),
                                visited: vec![signers[source].node().clone()],
                                remaining_links: 4,
                                remaining_attempts: 8,
                                deadline: scope.deadline,
                            },
                        };
                        let auth = Forwarding::new(signers[source].clone());
                        let (signed, binding) = auth
                            .sign_request_to(request, signers[relay].node())
                            .unwrap();
                        let response = nodes[source]
                            .transfers
                            .exchange(
                                Endpoint::Peer(membership.members()[relay].peer_endpoint.clone()),
                                signed,
                                &request_scope,
                            )
                            .await
                            .unwrap();
                        let response = auth.verify_response(response, &binding).unwrap();
                        let PeerResponse::Page {
                            ciphertext,
                            metadata,
                        } = response.response()
                        else {
                            panic!(
                                "real Fill layer page {number}, received {size}: non-page response"
                            );
                        };
                        assert_eq!(metadata.length, data.bytes[&key].len() as u64);
                        assert_eq!(ciphertext.envelope().page, page);
                        let charge = nodes[source]
                            .admission
                            .reserve(Some(&CacheId(CACHE.into())), ResourceClass::Plaintext, P)
                            .unwrap();
                        let crypto = PageCrypto::new(
                            discovery[source].0.clone(),
                            nodes[source].crypto.clone(),
                        );
                        let plaintext = crypto
                            .decrypt(ciphertext.clone(), charge, &request_scope)
                            .await
                            .unwrap();
                        hash.update(plaintext.bytes());
                        if key == manifest_key || key == config_key {
                            small_body.extend_from_slice(plaintext.bytes());
                        }
                        size += plaintext.bytes().len();
                        assert_eq!(
                            data.calls.borrow().get(&page),
                            Some(&1),
                            "one elected origin supplier per cold page"
                        );
                    }
                    assert_eq!(size, data.bytes[&key].len());
                    assert_eq!(
                        hash.finalize().as_slice(),
                        Sha256::digest(&data.bytes[&key]).as_slice()
                    );
                    if key == manifest_key {
                        let manifest: serde_json::Value =
                            serde_json::from_slice(&small_body).unwrap();
                        assert_eq!(manifest["schemaVersion"], 2);
                        let layers = manifest["layers"].as_array().unwrap();
                        assert_eq!(layers.len(), 8);
                        for (i, descriptor) in layers.iter().enumerate() {
                            let expected = vec![i as u8; 4 * P + 17];
                            assert_eq!(descriptor["size"], expected.len());
                            assert_eq!(
                                descriptor["digest"],
                                format!("sha256:{:x}", Sha256::digest(&expected))
                            );
                        }
                        assert_eq!(manifest["config"]["size"], data.bytes[&config_key].len());
                        assert_eq!(
                            manifest["config"]["digest"],
                            format!("sha256:{:x}", Sha256::digest(&data.bytes[&config_key]))
                        );
                    } else if key == config_key {
                        let config: serde_json::Value =
                            serde_json::from_slice(&small_body).unwrap();
                        assert_eq!(config["architecture"], "amd64");
                    }
                    completed.set(completed.get() + 1);
                });
            }
            while jobs.next().await.is_some() {}
        }
    };
    drive(Box::pin(async {
        futures::pin_mut!(servers, clients);
        match futures::future::select(clients, servers).await {
            futures::future::Either::Left(_) => {}
            futures::future::Either::Right((result, _)) => panic!("peer server ended: {result:?}"),
        }
    }));
    assert_eq!(completed.get(), 10);
    // Fully busy capacity remains a real overload. No hidden queue or raised cap.
    // Releasing an independent owner permits progress, and malformed lengths do
    // not evict a retained page before protocol validation.
    for node in &nodes {
        node.writer.discard_unsubmitted();
        node.memory.evict_idle(usize::MAX).unwrap();
        let held = node
            .admission
            .reserve(
                Some(&CacheId(CACHE.into())),
                ResourceClass::Ciphertext,
                node.admission.limit(ResourceClass::Ciphertext),
            )
            .unwrap();
        assert!(matches!(
            node.transfers
                .receive_buffer(&node.admission, &CacheId(CACHE.into()), P + 16),
            Err(Error::Overloaded)
        ));
        assert_eq!(
            node.admission.used(ResourceClass::Ciphertext),
            held.amount()
        );
        assert!(matches!(
            node.transfers
                .receive_buffer(&node.admission, &CacheId(CACHE.into()), P + 17),
            Err(Error::InvalidRequest)
        ));
        drop(held);
        drop(
            node.transfers
                .receive_buffer(&node.admission, &CacheId(CACHE.into()), P + 16)
                .unwrap(),
        );
    }
    for node in &nodes {
        node.pool.close();
        node.writer.discard_unsubmitted();
        node.memory.evict_idle(usize::MAX).unwrap();
    }
    drive(Box::pin(async {
        for node in &nodes {
            node.reactor.drain().await.unwrap();
        }
    }));
    for node in &nodes {
        for class in [
            ResourceClass::Plaintext,
            ResourceClass::Ciphertext,
            ResourceClass::DirtyCiphertext,
            ResourceClass::Relay,
            ResourceClass::Connection,
        ] {
            assert_eq!(node.admission.used(class), 0, "{class:?}");
        }
    }
    eprintln!("real Fill: manifest, config, eight layers verified; concurrency={concurrency}");
}
