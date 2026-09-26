//! Single-node production graph validation. Only control publication, origin data,
//! and the worker polling loop are fixtures. No read/storage/crypto success doubles.
use base64::Engine;
use racer_dataplane::{
    client::{request::RequestParser, response::Responses},
    control::{
        caches::{CacheDefinition, canonical_socket_paths},
        snapshot::{PublishedState, SnapshotStore},
        wire::{self, Publication, PublicationSequence},
    },
    error::{Error, Operation, Result},
    http::{
        codec::Codec,
        io::HttpIo,
        pool::{ConnectionLease, HttpPool},
    },
    memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool},
    model::{
        identity::*,
        limits::{Limits, ResourceClass},
        range::PAGE_BYTES,
    },
    origin::client::OriginClient,
    peer::{PeerNetwork, handshake::Handshake, requester::Requester, transfer::Transfers},
    read::{
        candidates::CandidatePolicy,
        dispatch::{Dispatcher, WorkerDirectory, WorkerEndpoint},
        fill::{Fill, FillDependencies},
        flight::{AcquisitionBudget, Flights},
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
        serve::{Coordinator, ReadService},
    },
    runtime::{
        admission::Admission,
        crypto::{self, CryptoClient},
        deadline::RequestScope,
        reactor::Reactor,
        worker::{CryptoRuntime, CryptoService, WorkerMap},
    },
    security::{
        aead::{PageCrypto, PageCryptoEngine},
        certificates::Certificates,
        credentials::CredentialCrypto,
        forwarding::Forwarding,
        keyring::{KeyEpochs, Keyring},
        replay::{ReplayState, ReplayWindow},
        signing::Signatures,
    },
    store::{
        eviction::SegmentClock, index::Index, reader::StoreReader, segment::Segments, slab::Slabs,
        writer::StoreWriter,
    },
    topology::{
        health::LinkHealth, membership::Member, paths::Paths, placement::Placement, rails::Rails,
    },
};
use std::{
    cell::RefCell,
    collections::BTreeMap,
    fs,
    future::Future,
    io::{Read, Write},
    num::{NonZeroU32, NonZeroUsize},
    os::{
        fd::AsRawFd,
        unix::net::{UnixListener, UnixStream},
    },
    path::PathBuf,
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    thread,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

const P: u64 = PAGE_BYTES;
const CACHE: &str = "44444444-4444-4444-8444-444444444444";
const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
const NODE: &str = "22222222-2222-4222-8222-222222222222";
const TIMEOUT: Duration = Duration::from_secs(40);

fn scope() -> RequestScope {
    static NEXT: AtomicUsize = AtomicUsize::new(1);
    let mut id = [0; 16];
    id[..8].copy_from_slice(&(NEXT.fetch_add(1, Ordering::Relaxed) as u64).to_le_bytes());
    RequestScope::new(RequestId(id), Instant::now() + TIMEOUT).unwrap()
}
fn nz(value: usize) -> NonZeroUsize {
    NonZeroUsize::new(value).unwrap()
}
fn limits(pages: usize) -> Limits {
    let n = nz(64);
    Limits {
        plaintext_bytes: nz(pages * P as usize),
        ciphertext_bytes: nz((pages + 4) * (P as usize + 4096)),
        dirty_bytes: nz(pages * (P as usize + 16)),
        registered_bytes: nz(P as usize + 16),
        request_context_bytes: nz(4 * 1024 * 1024),
        flights: n,
        waiters_per_flight: n,
        queue_entries: n,
        connections_per_neighbor: nz(8),
        client_connections: n,
        pipes: nz(8),
        range_window_pages: nz(2),
        replay_entries: n,
        header_bytes: nz(32768),
        route_search_work: n,
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    }
}

struct Scratch {
    path: PathBuf,
    directory: fs::File,
}
impl Scratch {
    fn new() -> Self {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target")
            .join(format!(
                "production-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        fs::create_dir(&path).unwrap();
        let directory = fs::File::open(&path).unwrap();
        Self { path, directory }
    }
    fn socket(&self, name: &str) -> PathBuf {
        // Short alias, with every created inode still inside the worktree.
        self.socket_root().join(name)
    }
    fn socket_root(&self) -> PathBuf {
        format!("/proc/self/fd/{}", self.directory.as_raw_fd()).into()
    }
}
impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.path);
    }
}

#[derive(Clone, Debug)]
struct OriginRequest {
    method: String,
    pin: Option<String>,
    range: Option<String>,
}
struct OriginState {
    version: u8,
    length: u64,
    zero_ttl: bool,
    expires_at: Option<u128>,
    online: bool,
    paused: bool,
    calls: Vec<OriginRequest>,
}
struct Adapter {
    state: Arc<Mutex<OriginState>>,
    stop: Arc<AtomicBool>,
    thread: Option<thread::JoinHandle<()>>,
}
impl Adapter {
    fn start(path: &PathBuf, length: u64, zero_ttl: bool) -> Self {
        let listener = UnixListener::bind(path).unwrap();
        listener.set_nonblocking(true).unwrap();
        let state = Arc::new(Mutex::new(OriginState {
            version: 1,
            length,
            zero_ttl,
            expires_at: None,
            online: true,
            paused: false,
            calls: Vec::new(),
        }));
        let stop = Arc::new(AtomicBool::new(false));
        let shared = state.clone();
        let stopping = stop.clone();
        let thread = thread::spawn(move || {
            let mut connections = Vec::new();
            while !stopping.load(Ordering::Relaxed) {
                match listener.accept() {
                    Ok((stream, _)) => {
                        let shared = shared.clone();
                        let stopping = stopping.clone();
                        connections.push(thread::spawn(move || {
                            adapter_connection(stream, shared, stopping)
                        }));
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(1))
                    }
                    Err(e) => panic!("adapter accept: {e}"),
                }
            }
            for connection in connections {
                connection.join().unwrap();
            }
        });
        Self {
            state,
            stop,
            thread: Some(thread),
        }
    }
    fn calls(&self) -> Vec<OriginRequest> {
        self.state.lock().unwrap().calls.clone()
    }
    fn offline(&self) {
        self.state.lock().unwrap().online = false;
    }
}
impl Drop for Adapter {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(t) = self.thread.take() {
            t.join().unwrap();
        }
    }
}
fn read_head(stream: &mut UnixStream) -> std::io::Result<String> {
    let mut raw = Vec::new();
    while !raw.ends_with(b"\r\n\r\n") {
        let mut b = [0];
        stream.read_exact(&mut b)?;
        raw.push(b[0]);
        assert!(raw.len() <= 32768, "oversized fixture head");
    }
    Ok(String::from_utf8(raw).unwrap())
}
fn fields(head: &str) -> BTreeMap<String, String> {
    head.split("\r\n")
        .skip(1)
        .filter_map(|line| line.split_once(':'))
        .map(|(k, v)| (k.to_ascii_lowercase(), v.trim().to_string()))
        .collect()
}
fn byte(version: u8, offset: u64) -> u8 {
    version.wrapping_mul(37).wrapping_add((offset % 251) as u8)
}
fn adapter_connection(
    mut stream: UnixStream,
    state: Arc<Mutex<OriginState>>,
    stop: Arc<AtomicBool>,
) {
    stream
        .set_read_timeout(Some(Duration::from_millis(100)))
        .unwrap();
    stream.set_write_timeout(Some(TIMEOUT)).unwrap();
    while !stop.load(Ordering::Relaxed) {
        let head = match read_head(&mut stream) {
            Ok(head) => head,
            Err(e)
                if matches!(
                    e.kind(),
                    std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
                ) =>
            {
                continue;
            }
            Err(_) => return,
        };
        let f = fields(&head);
        let method = head.split_whitespace().next().unwrap();
        assert!(head.contains(&format!("/v1/objects/{} HTTP/1.1", "ab".repeat(32))));
        assert_eq!(
            f.get("authorization").map(String::as_str),
            Some("fixture-credential")
        );
        assert_eq!(
            f.get("racer-metadata").map(String::as_str),
            Some("fixture-metadata")
        );
        let (version, length, zero_ttl, expires_at, online) = {
            let mut state = state.lock().unwrap();
            state.calls.push(OriginRequest {
                method: method.into(),
                pin: f.get("if-match").cloned(),
                range: f.get("range").cloned(),
            });
            (
                state.version,
                state.length,
                state.zero_ttl,
                state.expires_at,
                state.online,
            )
        };
        while state.lock().unwrap().paused {
            if stop.load(Ordering::Relaxed) {
                return;
            }
            thread::sleep(Duration::from_millis(1));
        }
        let tag = format!("\"v{version}\"");
        if !online || f.get("if-match").is_some_and(|pin| pin != &tag) {
            let status = if online { 412 } else { 503 };
            if write!(
                stream,
                "HTTP/1.1 {status} Result\r\nContent-Length: 0\r\n\r\n"
            )
            .is_err()
            {
                return;
            }
            continue;
        }
        let expiry = expires_at.unwrap_or_else(|| {
            if zero_ttl {
                0
            } else {
                (SystemTime::now().duration_since(UNIX_EPOCH).unwrap() + Duration::from_secs(120))
                    .as_millis()
            }
        });
        if method == "HEAD" || length == 0 {
            if write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: {length}\r\nContent-Type: application/octet-stream\r\nETag: {tag}\r\nRacer-Expires-At: {expiry}\r\n\r\n").is_err() { return; }
            continue;
        }
        let range = f.get("range").unwrap().strip_prefix("bytes=").unwrap();
        let (first, last) = range.split_once('-').unwrap();
        let first: u64 = first.parse().unwrap();
        let last: u64 = last.parse::<u64>().unwrap().min(length - 1);
        assert_eq!(first % P, 0);
        assert_eq!(last, (first + P - 1).min(length - 1));
        if write!(stream, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Type: application/octet-stream\r\nContent-Range: bytes {first}-{last}/{length}\r\nETag: {tag}\r\nRacer-Expires-At: {expiry}\r\n\r\n", last - first + 1).is_err() { return; }
        let mut chunk = [0; 65536];
        let mut offset = first;
        while offset <= last {
            let n = chunk.len().min((last - offset + 1) as usize);
            for (i, b) in chunk[..n].iter_mut().enumerate() {
                *b = byte(version, offset + i as u64);
            }
            if stream.write_all(&chunk[..n]).is_err() {
                return;
            }
            offset += n as u64;
        }
    }
}

struct Rig {
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    crypto: Rc<CryptoClient>,
    engine: RefCell<PageCryptoEngine>,
    endpoint: RefCell<WorkerEndpoint>,
    flights: Rc<Flights>,
    fill: Rc<Fill>,
    page_crypto: Rc<PageCrypto>,
    membership: racer_dataplane::topology::membership::MembershipLease,
    writer: Rc<StoreWriter>,
    memory: Rc<MemoryCache>,
    dispatcher: Rc<Dispatcher>,
    io: Rc<HttpIo>,
    responses: Rc<Responses>,
    writer_task: RefCell<Option<Operation<'static, ()>>>,
    adapter: Adapter,
    _scratch: Scratch,
}
impl Rig {
    fn new(length: u64, zero_ttl: bool, pages: usize) -> Self {
        Self::with_dirty_pages(length, zero_ttl, pages, pages)
    }
    fn with_dirty_pages(length: u64, zero_ttl: bool, pages: usize, dirty_pages: usize) -> Self {
        Self::with_limits(length, zero_ttl, pages, dirty_pages, None)
    }
    fn with_limits(
        length: u64,
        zero_ttl: bool,
        pages: usize,
        dirty_pages: usize,
        configure: Option<fn(&mut Limits)>,
    ) -> Self {
        Self::with_worker(
            length,
            zero_ttl,
            pages,
            dirty_pages,
            configure,
            WorkerId(0),
            None,
        )
    }
    fn with_worker(
        length: u64,
        zero_ttl: bool,
        pages: usize,
        dirty_pages: usize,
        configure: Option<fn(&mut Limits)>,
        worker: WorkerId,
        shared: Option<Arc<WorkerDirectory>>,
    ) -> Self {
        let scratch = Scratch::new();
        fs::create_dir_all(scratch.path.join("production-fixture/origin")).unwrap();
        let origin_path = scratch.socket("production-fixture/origin/socket");
        assert!(origin_path.as_os_str().len() <= 107);
        let adapter = Adapter::start(&origin_path, length, zero_ttl);
        let mut budget = limits(pages);
        budget.dirty_bytes = nz(dirty_pages * (P as usize + 16));
        if let Some(configure) = configure {
            configure(&mut budget);
        }
        let admission = Rc::new(Admission::new(budget));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(32768, P + 16),
            admission.clone(),
        ));
        let http = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 8));
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let index = Rc::new(Index::new(worker, 64));
        let segments = Rc::new(Segments::new(worker, 64 * 1024 * 1024));
        let slabs = Rc::new(Slabs::new(
            worker,
            scratch.path.join("slabs"),
            reactor.clone(),
            512 * 1024 * 1024,
            64 * 1024 * 1024,
        ));
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
            .configure(admission.clone(), eviction, 64, 64)
            .unwrap();
        futures::executor::block_on(writer.open()).expect("real O_DIRECT slab must open");
        let keys = Rc::new(Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        ));
        // The wire fixture reuses material across purposes; the real keyring
        // requires distinct material. These are deterministic test-only keys.
        let mut bundle: serde_json::Value =
            serde_json::from_slice(include_bytes!("../src/control/testdata/bundle.json")).unwrap();
        for (i, key) in bundle["cache_keys"]
            .as_array_mut()
            .unwrap()
            .iter_mut()
            .enumerate()
        {
            key["material"] = base64::engine::general_purpose::STANDARD
                .encode([i as u8 + 7; 32])
                .into();
        }
        keys.install(wire::decode_bundle(&serde_json::to_vec(&bundle).unwrap()).unwrap())
            .unwrap();
        let snapshots = Rc::new(SnapshotStore::new(
            ClusterId(CLUSTER.into()),
            Arc::new(PublishedState::default()),
            4,
        ));
        let (client_socket, origin_socket) = canonical_socket_paths("production-fixture").unwrap();
        let snapshot = snapshots
            .publish(Publication {
                schema_version: 1,
                cluster: ClusterId(CLUSTER.into()),
                sequence: PublicationSequence(1),
                membership_version: MembershipVersion(1),
                members: vec![Member {
                    node: NodeId(NODE.into()),
                    shares: NonZeroU32::new(4).unwrap(),
                    peer_endpoint: "127.0.0.1:8000".into(),
                    rails: vec![],
                    alignment_enabled: false,
                }],
                caches: vec![CacheDefinition {
                    id: CacheId(CACHE.into()),
                    name: "production-fixture".into(),
                    client_socket,
                    origin_socket,
                    socket_mode: 0o600,
                }],
            })
            .unwrap();
        let network = Rc::new(PeerNetwork::new(NodeId(NODE.into()), 4).unwrap());
        network.install(snapshot.membership.clone()).unwrap();
        let certificates = Rc::new(Certificates::new(ClusterId(CLUSTER.into()), keys.clone()));
        let signatures = Rc::new(Signatures::new(
            keys.clone(),
            certificates,
            Rc::new(ReplayWindow::new(Arc::new(ReplayState::default()), 64)),
        ));
        let forwarding = Rc::new(Forwarding::new(signatures.clone()));
        let transfers = Rc::new(Transfers::new(http.clone(), io.clone(), None));
        let handshake =
            Rc::new(Handshake::new(signatures, None).with_http(network.clone(), transfers.clone()));
        let peers = Rc::new(
            Requester::new(
                Rc::new(Paths::new(Rc::new(LinkHealth), 64, 64)),
                Rc::new(Rails),
                forwarding,
                handshake,
                transfers,
            )
            .with_network(network),
        );
        let candidates = Rc::new(CandidatePolicy::new(
            NodeId(NODE.into()),
            Rc::new(Placement::new(64)),
            peers.clone(),
        ));
        let origin = Rc::new(
            OriginClient::new(snapshots.clone(), http, io.clone())
                .with_buffers(admission.clone(), buffers.clone())
                .with_socket_root(scratch.socket_root())
                .unwrap(),
        );
        assert_eq!(
            snapshot.caches[0].origin_socket,
            PathBuf::from("/run/racer/production-fixture/origin/socket")
        );
        let (port, engine) = crypto::pair(worker, 0, nz(64));
        let crypto = Rc::new(CryptoClient::new(port));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let directory = shared.unwrap_or_else(|| {
            Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    admission.limits().queue_entries.get(),
                )
                .unwrap(),
            )
        });
        let flights = Rc::new(Flights::new(admission.clone()));
        let page_crypto = Rc::new(PageCrypto::new(keys, crypto.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers,
            disk,
            writer: writer.clone(),
            peers: peers.clone(),
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights: flights.clone(),
            crypto: page_crypto.clone(),
            credentials: credentials.clone(),
            admission: admission.clone(),
            metadata_owner: directory.clone(),
        }));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            origin,
            peers,
            credentials.clone(),
            64,
            MetadataDependencies {
                index,
                fill: fill.clone(),
                owners: directory.clone(),
            },
        ));
        let delivery = Rc::new(Delivery::new(
            Rc::new(PipePool::new(admission.clone(), reactor.clone())),
            // This fixture polls real crypto inline rather than on its paired
            // production thread. Debug-build page crypto must not count as a
            // two-second client stall while the fixture executor is occupied.
            TIMEOUT,
        ));
        let streams = Rc::new(RangeStreams::new(
            fill.clone(),
            directory.clone(),
            delivery.clone(),
            2,
        ));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill.clone(),
            streams,
            credentials,
        ));
        let endpoint = RefCell::new(directory.install(worker, coordinator.clone()).unwrap());
        let dispatcher = Rc::new(Dispatcher::new(worker, directory, coordinator));
        let client_io = Rc::new(HttpIo::for_clients(reactor.clone(), admission.clone()));
        let responses = Rc::new(Responses::new(client_io.clone(), delivery));
        Self {
            admission,
            reactor,
            crypto,
            engine: RefCell::new(PageCryptoEngine::new(CryptoRuntime { port: engine })),
            endpoint,
            flights,
            fill,
            page_crypto,
            membership: snapshot.membership.clone(),
            writer,
            memory,
            dispatcher,
            io: client_io,
            responses,
            writer_task: RefCell::new(None),
            adapter,
            _scratch: scratch,
        }
    }
    fn tick(&self) {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        self.reactor.poll_budgeted(128).unwrap();
        self.engine.borrow_mut().poll_budgeted(64).unwrap();
        self.crypto.poll_budgeted(64).unwrap();
        self.endpoint.borrow_mut().poll(&mut cx, 64).unwrap();
        self.flights.poll_with_context(&mut cx, 64).unwrap();
        let mut task = self.writer_task.borrow_mut();
        if let Some(write) = task.as_mut() {
            if let Poll::Ready(result) = write.as_mut().poll(&mut cx) {
                result.expect("dirty persistence");
                *task = None;
            }
        }
        if task.is_none() && self.writer.pending_count() > 0 {
            let writer = self.writer.clone();
            let scope = scope();
            *task = Some(Box::pin(async move {
                writer.progress(1, &scope).await.map(|_| ())
            }));
        }
    }
    fn drive<T>(&self, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let deadline = Instant::now() + TIMEOUT;
        loop {
            self.tick();
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(Instant::now() < deadline, "production graph stalled");
            self.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    async fn serve(&self, stream: UnixStream, scope: &RequestScope) -> Result<()> {
        let lease = ConnectionLease::from_accepted(stream.into(), &self.admission)?;
        let received = self.io.receive_head(lease, scope).await?;
        let request = RequestParser::new(32768).parse(&CacheId(CACHE.into()), received.value)?;
        let kind = request.kind.clone();
        match self.dispatcher.read(request, scope).await {
            Ok(response) => {
                self.responses.validate(&kind, &response)?;
                drop(
                    self.responses
                        .send(received.connection, response, scope)
                        .await?,
                );
            }
            Err(error) => {
                drop(
                    self.responses
                        .send_error(received.connection, error, scope)
                        .await?,
                );
            }
        }
        Ok(())
    }
    fn request(&self, method: &str, fields: &str) -> Reply {
        let (local, mut remote) = UnixStream::pair().unwrap();
        let request = request(method, fields);
        let head_only = method == "HEAD";
        let reader = thread::spawn(move || {
            remote.write_all(request.as_bytes()).unwrap();
            receive(remote, head_only)
        });
        let result = self.drive(self.serve(local, &scope()));
        let reply = reader.join();
        assert!(
            result.is_ok(),
            "production response delivery for {method} {fields:?}: {result:?}; origin calls: {:?}",
            self.adapter.calls()
        );
        reply.unwrap()
    }
    fn flush(&self) {
        self.drive(std::future::poll_fn(|_| {
            if self.writer.is_idle() && self.writer_task.borrow().is_none() {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        }));
        assert_eq!(self.admission.used(ResourceClass::DirtyCiphertext), 0);
        assert_eq!(self.writer.writes_in_flight(), 0);
    }
}
fn request(method: &str, fields: &str) -> String {
    format!(
        "{method} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\n{fields}\r\n",
        "ab".repeat(32)
    )
}
struct Reply {
    status: u16,
    fields: BTreeMap<String, String>,
    body: Vec<u8>,
}
fn receive(mut stream: UnixStream, head_only: bool) -> Reply {
    stream.set_read_timeout(Some(TIMEOUT)).unwrap();
    let head = read_head(&mut stream).expect("client response head");
    let fields = fields(&head);
    let status = head.split_whitespace().nth(1).unwrap().parse().unwrap();
    let length: usize = if head_only {
        0
    } else {
        fields["content-length"].parse().unwrap()
    };
    let mut body = vec![0; length];
    stream.read_exact(&mut body).expect("complete client body");
    Reply {
        status,
        fields,
        body,
    }
}
fn check(reply: &Reply, version: u8, start: u64, end: u64, total: u64) {
    assert_eq!(reply.status, 206);
    assert_eq!(reply.fields["etag"], format!("\"v{version}\""));
    assert_eq!(
        reply.fields["content-range"],
        format!("bytes {start}-{}/{total}", end - 1)
    );
    assert_eq!(reply.body.len() as u64, end - start);
    for (offset, b) in reply.body.iter().enumerate() {
        assert_eq!(
            *b,
            byte(version, start + offset as u64),
            "body offset {offset}"
        );
    }
}

#[test]
fn stream_window_under_production_ciphertext_limit() {
    let length = 4 * P + 113;
    let rig = Rig::with_limits(
        length,
        false,
        4,
        2,
        Some(|limits| {
            limits.ciphertext_bytes = nz(64 * 1024 * 1024);
        }),
    );
    for _ in 0..3 {
        check(
            &rig.request("GET", "Range: bytes=0-16777215\r\n"),
            1,
            0,
            P,
            length,
        );
        check(
            &rig.request("GET", &format!("If-Match: \"v1\"\r\nRange: bytes={P}-\r\n")),
            1,
            P,
            length,
            length,
        );
        rig.flush();
        rig.memory.evict_idle(usize::MAX).unwrap();
    }
}

#[test]
fn disk_stream_reuses_fill_ciphertext_reservation_without_origin() {
    let length = 4 * P + 113;
    let rig = Rig::with_limits(
        length,
        false,
        4,
        2,
        Some(|limits| {
            limits.ciphertext_bytes = nz(64 * 1024 * 1024);
        }),
    );
    assert_eq!(rig.request("HEAD", "").status, 200);
    for number in 0..5 {
        let start = number * P;
        let end = (start + P).min(length);
        check(
            &rig.request(
                "GET",
                &format!("If-Match: \"v1\"\r\nRange: bytes={start}-{}\r\n", end - 1),
            ),
            1,
            start,
            end,
            length,
        );
        rig.flush();
        rig.memory.evict_idle(usize::MAX).unwrap();
    }
    assert_eq!(rig.writer.index().snapshot().unwrap().entries.len(), 5);
    let calls = rig.adapter.calls().len();
    rig.adapter.offline();
    check(
        &rig.request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
        1,
        0,
        length,
        length,
    );
    assert_eq!(rig.adapter.calls().len(), calls);
}

#[test]
fn concurrent_full_layer_streams_verify_every_byte() {
    let length = 4 * P + 113;
    let rig = Rig::with_limits(
        length,
        false,
        4,
        4,
        Some(|limits| {
            // Isolate delivery pressure: 64 accepted streams also need origin
            // connection headroom and two page-window commands per reader.
            limits.client_connections = nz(128);
            limits.queue_entries = nz(256);
            limits.request_context_bytes = nz(16 * 1024 * 1024);
            limits.pipes = nz(8);
        }),
    );
    assert_eq!(rig.request("HEAD", "").status, 200);
    let mut sockets = Vec::new();
    let mut readers = Vec::new();
    for _ in 0..64 {
        let (local, mut remote) = UnixStream::pair().unwrap();
        sockets.push(local);
        readers.push(thread::spawn(move || {
            remote
                .write_all(request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n").as_bytes())
                .unwrap();
            remote.set_read_timeout(Some(TIMEOUT)).unwrap();
            let head = read_head(&mut remote).unwrap();
            assert!(head.starts_with("HTTP/1.1 206"), "{head}");
            assert_eq!(fields(&head)["content-length"], length.to_string());
            let mut scratch = [0; 65536];
            let mut offset = 0;
            while offset < length {
                let count = scratch.len().min((length - offset) as usize);
                remote.read_exact(&mut scratch[..count]).unwrap();
                for (i, actual) in scratch[..count].iter().enumerate() {
                    assert_eq!(*actual, byte(1, offset + i as u64));
                }
                offset += count as u64;
            }
            assert_eq!(remote.read(&mut scratch[..1]).unwrap(), 0);
        }));
    }
    let results = rig.drive(futures::future::join_all(
        sockets
            .into_iter()
            .map(|socket| async { rig.serve(socket, &scope()).await }),
    ));
    let joined: Vec<_> = readers.into_iter().map(|reader| reader.join()).collect();
    assert!(
        results.iter().all(Result::is_ok),
        "stream results: {results:?}"
    );
    assert!(joined.iter().all(std::result::Result::is_ok));
}

#[test]
fn two_worker_bootstrap_and_concurrent_continuations_verify_bytes() {
    let length = 4 * P + 113;
    let workers = vec![WorkerId(0), WorkerId(1)];
    let directory = Arc::new(
        WorkerDirectory::new(
            Arc::new(WorkerMap::new(workers.clone()).unwrap()),
            workers,
            64,
        )
        .unwrap(),
    );
    let owners: std::collections::HashSet<_> = (0..5)
        .map(|number| {
            directory
                .page_owner(&PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: CacheId(CACHE.into()),
                            key: CacheKey([0xab; 32]),
                        },
                        etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                    },
                    number: PageNumber(number),
                })
                .unwrap()
        })
        .collect();
    assert_eq!(owners.len(), 2, "continuation must cross worker ownership");
    let stop = Arc::new(AtomicBool::new(false));
    let ready = Arc::new(std::sync::Barrier::new(2));
    thread::scope(|threads| {
        let secondary_directory = directory.clone();
        let secondary_stop = stop.clone();
        let secondary_ready = ready.clone();
        threads.spawn(move || {
            let rig = Rig::with_worker(
                length,
                false,
                8,
                4,
                None,
                WorkerId(1),
                Some(secondary_directory),
            );
            secondary_ready.wait();
            let deadline = Instant::now() + Duration::from_secs(15);
            while !secondary_stop.load(Ordering::Acquire) && Instant::now() < deadline {
                rig.tick();
                rig.reactor.wait(Duration::from_millis(1)).unwrap();
            }
            rig.flush();
        });
        let rig = Rig::with_worker(length, false, 8, 4, None, WorkerId(0), Some(directory));
        ready.wait();
        check(
            &rig.request("GET", "Range: bytes=0-16777215\r\n"),
            1,
            0,
            P,
            length,
        );
        let mut sockets = Vec::new();
        let mut readers = Vec::new();
        for _ in 0..4 {
            let (local, mut remote) = UnixStream::pair().unwrap();
            sockets.push(local);
            readers.push(thread::spawn(move || {
                remote
                    .write_all(
                        request("GET", "If-Match: \"v1\"\r\nRange: bytes=16777216-\r\n").as_bytes(),
                    )
                    .unwrap();
                let reply = receive(remote, false);
                check(&reply, 1, P, length, length);
            }));
        }
        let results = rig.drive(futures::future::join_all(
            sockets
                .into_iter()
                .map(|socket| async { rig.serve(socket, &scope()).await }),
        ));
        stop.store(true, Ordering::Release);
        assert!(
            results.iter().all(Result::is_ok),
            "continuations: {results:?}"
        );
        for reader in readers {
            reader.join().unwrap();
        }
        rig.flush();
    });
}

#[test]
fn bootstrap_then_pinned_remainder_over_three_pages_and_disk_hits_without_origin() {
    let length = 5 * P + 113;
    let rig = Rig::new(length, false, 10);
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        1,
        0,
        P,
        length,
    );
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].method, "GET");
    assert_eq!(calls[0].pin, None);
    assert_eq!(calls[0].range.as_deref(), Some("bytes=0-16777215"));
    check(
        &rig.request("GET", &format!("If-Match: \"v1\"\r\nRange: bytes={P}-\r\n")),
        1,
        P,
        length,
        length,
    );
    rig.flush();
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 6);
    assert_eq!(rig.writer.index().snapshot().unwrap().entries.len(), 6);
    assert!(
        calls[1..]
            .iter()
            .all(|call| call.method == "GET" && call.pin.as_deref() == Some("\"v1\""))
    );
    rig.adapter.offline();
    check(
        &rig.request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
        1,
        0,
        length,
        length,
    );
    assert!(
        rig.memory.evict_idle(usize::MAX).unwrap() > 0,
        "force disk-only reads"
    );
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    check(
        &rig.request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
        1,
        0,
        length,
        length,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        calls.len(),
        "cache hit contacted disabled origin"
    );
}

#[test]
fn zero_attempt_acquire_serves_validated_disk_copy_without_origin() {
    let rig = Rig::with_limits(
        P,
        false,
        8,
        4,
        Some(|limits| {
            limits.plaintext_bytes = nz(128 * 1024 * 1024);
            limits.ciphertext_bytes = nz(128 * 1024 * 1024);
            limits.dirty_bytes = nz(64 * 1024 * 1024);
            limits.connections_per_neighbor = nz(2);
            limits.relay_transfers = nz(8);
        }),
    );
    let seeded = rig.request("GET", "Range: bytes=0-16777215\r\n");
    check(&seeded, 1, 0, P, P);
    rig.flush();
    rig.adapter.offline();
    let origin_calls = rig.adapter.calls().len();
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([0xab; 32]),
            },
            etag: StrongEtag::parse(b"\"v1\"").unwrap(),
        },
        number: PageNumber(0),
    };
    assert_eq!(rig.writer.pending_count(), 0);
    assert!(rig.memory.evict_idle(usize::MAX).unwrap() > 0);
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(rig.admission.used(ResourceClass::Flight), 0);
    let scope = scope();
    let (metadata, ciphertext) = rig
        .drive(rig.fill.copy_only(&page, &scope))
        .unwrap()
        .expect("actual disk CopyOnly must return the retained page");
    assert_eq!(metadata.version, page.version);
    let plaintext = rig
        .drive(
            rig.page_crypto.decrypt(
                ciphertext,
                rig.admission
                    .reserve(
                        Some(&page.version.object.cache),
                        ResourceClass::Plaintext,
                        P as usize,
                    )
                    .unwrap(),
                &scope,
            ),
        )
        .expect("disk CopyOnly ciphertext must pass actual AEAD validation");
    assert_eq!(plaintext.bytes(), seeded.body);
    drop(plaintext);
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(rig.admission.used(ResourceClass::Flight), 0);
    let context = racer_dataplane::model::context::OriginContext {
        object: page.version.object.clone(),
        metadata: None,
        authorization: None,
    };
    let mut budget = AcquisitionBudget::new(scope.deadline.0, 0, 0);
    let result =
        rig.drive(
            rig.fill
                .acquire(page, rig.membership.clone(), &context, &scope, &mut budget),
        );
    assert_eq!(rig.adapter.calls().len(), origin_calls);
    assert_eq!(budget.remaining_attempts(), 0);
    assert_eq!(budget.remaining_links(), 0);
    let result = result.expect("zero attempts must not reject an available validated disk copy");
    assert_eq!(result.plaintext.bytes(), seeded.body);
}

#[test]
fn zero_ttl_revalidates_fresh_reads_but_retained_pins_survive_metadata_change() {
    let rig = Rig::new(113, true, 6);
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        1,
        0,
        113,
        113,
    );
    assert_eq!(rig.adapter.calls().len(), 1);
    assert_eq!(rig.request("HEAD", "").fields["racer-expires-at"], "0");
    assert_eq!(
        rig.adapter.calls().len(),
        2,
        "zero TTL must revalidate unpinned HEAD"
    );
    rig.adapter.state.lock().unwrap().version = 2;
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        2,
        0,
        113,
        113,
    );
    assert_eq!(rig.adapter.calls().len(), 3);
    rig.flush();
    rig.adapter.offline();
    check(
        &rig.request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
        1,
        0,
        113,
        113,
    );
    assert_eq!(
        rig.request("HEAD", "If-Match: \"v1\"\r\n").fields["etag"],
        "\"v1\""
    );
    assert_eq!(
        rig.adapter.calls().len(),
        3,
        "old pin must not consult changed origin"
    );
}

#[test]
fn expired_nonzero_metadata_keeps_old_version_length_and_disk_bytes() {
    let rig = Rig::new(113, false, 6);
    rig.adapter.state.lock().unwrap().expires_at = Some(1);
    let old = rig.request("GET", "Range: bytes=0-16777215\r\n");
    check(&old, 1, 0, 113, 113);
    assert_eq!(old.fields["racer-expires-at"], "1");
    rig.flush();
    {
        let mut origin = rig.adapter.state.lock().unwrap();
        origin.version = 2;
        origin.length = 227;
        origin.expires_at = None;
    }
    let fresh = rig.request("HEAD", "");
    assert_eq!(fresh.status, 200);
    assert_eq!(fresh.fields["etag"], "\"v2\"");
    assert_eq!(fresh.fields["content-length"], "227");
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 2);
    assert_eq!(calls[1].method, "HEAD");
    assert_eq!(calls[1].pin, None);
    // An unknown pin must fail instead of substituting the current version.
    let missing = rig.request("HEAD", "If-Match: \"missing\"\r\n");
    assert_eq!(missing.status, 412);
    assert!(missing.body.is_empty());
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 3);
    assert_eq!(calls[2].pin.as_deref(), Some("\"missing\""));
    rig.adapter.offline();
    assert!(rig.memory.evict_idle(usize::MAX).unwrap() > 0);
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    let pinned = rig.request("HEAD", "If-Match: \"v1\"\r\n");
    assert_eq!(pinned.status, 200);
    assert_eq!(pinned.fields["etag"], "\"v1\"");
    assert_eq!(pinned.fields["content-length"], "113");
    check(
        &rig.request("GET", "If-Match: \"v1\"\r\nRange: bytes=-17\r\n"),
        1,
        96,
        113,
        113,
    );
    assert_eq!(rig.adapter.calls().len(), calls.len());
}

#[test]
fn dirty_drain_and_byte_pressure_keep_long_range_progressing() {
    let length = 7 * P + 113;
    let rig = Rig::with_dirty_pages(length, false, 4, 2);
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        1,
        0,
        P,
        length,
    );
    check(
        &rig.request("GET", &format!("If-Match: \"v1\"\r\nRange: bytes={P}-\r\n")),
        1,
        P,
        length,
        length,
    );
    rig.flush();
    assert!(rig.admission.used(ResourceClass::Plaintext) <= 4 * P as usize);
    assert_eq!(rig.adapter.calls().len(), 8);
    // Under pressure, Fill may discard unsubmitted writes or skip persistence.
    // A drained queue is not evidence that every fetched page reached disk.
    let persisted = rig.writer.index().snapshot().unwrap().entries;
    assert!(!persisted.is_empty(), "dirty drain persisted no pages");
    rig.adapter.offline();
    rig.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    for (page, _) in persisted {
        let start = page.number.0 * P;
        let end = (start + P).min(length);
        check(
            &rig.request(
                "GET",
                &format!("If-Match: \"v1\"\r\nRange: bytes={start}-{}\r\n", end - 1),
            ),
            1,
            start,
            end,
            length,
        );
    }
    assert_eq!(rig.adapter.calls().len(), 8);
}

#[test]
fn concurrent_zero_ttl_bootstraps_share_only_the_inflight_cohort() {
    let rig = Rig::new(113, true, 6);
    rig.adapter.state.lock().unwrap().paused = true;
    let mut servers = Vec::new();
    let mut readers = Vec::new();
    for _ in 0..2 {
        let (local, mut remote) = UnixStream::pair().unwrap();
        remote
            .write_all(request("GET", "Range: bytes=0-16777215\r\n").as_bytes())
            .unwrap();
        servers.push(local);
        readers.push(thread::spawn(move || receive(remote, false)));
    }
    let first = scope();
    let second = scope();
    let mut turns = 0;
    let release = std::future::poll_fn(|_| {
        if !rig.adapter.calls().is_empty() {
            // Both prewritten requests are polled on every turn while origin
            // completion is gated, making cohort overlap independent of timing.
            turns += 1;
            if turns == 64 {
                assert_eq!(rig.adapter.calls().len(), 1);
                rig.adapter.state.lock().unwrap().paused = false;
                return Poll::Ready(());
            }
        }
        Poll::Pending
    });
    let (a, b, ()) = rig.drive(async {
        futures::join!(
            rig.serve(servers.remove(0), &first),
            rig.serve(servers.remove(0), &second),
            release
        )
    });
    a.unwrap();
    b.unwrap();
    for reader in readers {
        check(&reader.join().unwrap(), 1, 0, 113, 113);
    }
    assert_eq!(
        rig.adapter.calls().len(),
        1,
        "concurrent bootstrap duplicated origin GET"
    );
    rig.adapter.state.lock().unwrap().version = 2;
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        2,
        0,
        113,
        113,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        2,
        "new zero-TTL caller reused completed cohort"
    );
    rig.flush();
}

#[test]
fn cancel_backpressured_reader_preserves_fast_reader_and_releases_leases() {
    let rig = Rig::new(P, false, 4);
    check(
        &rig.request("GET", "Range: bytes=0-16777215\r\n"),
        1,
        0,
        P,
        P,
    );
    rig.flush();
    rig.adapter.offline();
    let baseline_connections = rig.admission.used(ResourceClass::Connection);
    let (slow_local, mut slow_remote) = UnixStream::pair().unwrap();
    let (fast_local, mut fast_remote) = UnixStream::pair().unwrap();
    let wire = request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n");
    slow_remote.write_all(wire.as_bytes()).unwrap();
    fast_remote.write_all(wire.as_bytes()).unwrap();
    let slow_head = Arc::new(AtomicBool::new(false));
    let saw_head = slow_head.clone();
    let slow_reader = thread::spawn(move || {
        slow_remote.set_read_timeout(Some(TIMEOUT)).unwrap();
        let head = read_head(&mut slow_remote).unwrap();
        assert_eq!(head.split_whitespace().nth(1), Some("206"));
        saw_head.store(true, Ordering::Release);
        // Retain the socket without draining its body until cancellation finishes.
        slow_remote
    });
    let fast_done = Arc::new(AtomicBool::new(false));
    let done = fast_done.clone();
    let fast_reader = thread::spawn(move || {
        let reply = receive(fast_remote, false);
        done.store(true, Ordering::Release);
        reply
    });
    let slow_scope = scope();
    let fast_scope = scope();
    let cancel = std::future::poll_fn(|_| {
        if slow_head.load(Ordering::Acquire) && fast_done.load(Ordering::Acquire) {
            assert!(
                rig.admission.used(ResourceClass::Pipe) > 0,
                "slow reader must retain a delivery lease"
            );
            slow_scope.cancel().unwrap();
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    });
    let (slow, fast, ()) = rig.drive(async {
        futures::join!(
            rig.serve(slow_local, &slow_scope),
            rig.serve(fast_local, &fast_scope),
            cancel
        )
    });
    assert_eq!(slow, Err(Error::Cancelled));
    fast.unwrap();
    drop(slow_reader.join().unwrap());
    check(&fast_reader.join().unwrap(), 1, 0, P, P);
    rig.drive(std::future::poll_fn(|_| {
        if rig.reactor.in_flight() == 0 && rig.admission.used(ResourceClass::Pipe) == 0 {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }));
    rig.flush();
    assert_eq!(
        rig.admission.used(ResourceClass::Connection),
        baseline_connections
    );
    assert_eq!(rig.admission.used(ResourceClass::Flight), 0);
    assert_eq!(rig.admission.used(ResourceClass::Waiter), 0);
    rig.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(
        rig.admission.used(ResourceClass::Plaintext),
        0,
        "canceled reader retained a page"
    );
    check(
        &rig.request("GET", "If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
        1,
        0,
        P,
        P,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        1,
        "reader cancellation invalidated shared cached page"
    );
}
