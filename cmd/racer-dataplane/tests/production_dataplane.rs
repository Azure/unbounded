//! Single-node production graph validation. Only control publication, origin data,
//! and the worker polling loop are fixtures. No read/storage/crypto success doubles.
#[path = "production/bootstrap.rs"]
mod bootstrap;
#[path = "production/bootstrap_pressure.rs"]
mod bootstrap_pressure;
#[path = "production/index_pressure.rs"]
mod index_pressure;

use base64::Engine;
use racer_dataplane::{
    client::{RequestParser, response::Responses},
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
    model::{Limits, PAGE_BYTES, ResourceClass, *},
    origin::OriginClient,
    peer::{PeerNetwork, Requester, transfer::Transfers},
    read::{
        Coordinator, ReadService,
        candidates::CandidatePolicy,
        dispatch::{WorkerDirectory, WorkerEndpoint},
        fill::{Fill, FillDependencies},
        flight::Flights,
        metadata::{MetadataDependencies, MetadataService},
        range_stream::RangeStreams,
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
        signing::Signatures,
    },
    store::{
        StoreReader, eviction::SegmentClock, index::Index, segment::Segments, slab::Slabs,
        writer::StoreWriter,
    },
    topology::{health::LinkHealth, membership::Member, paths::Paths, placement::Placement},
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

#[path = "hotpath/bench.rs"]
mod hotpath;

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
        header_bytes: nz(32768),
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
        Self::under(&PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target"))
    }
    fn under(root: &std::path::Path) -> Self {
        static NEXT: AtomicUsize = AtomicUsize::new(0);
        let path = root.join(format!(
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
    benchmark_keys: bool,
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
            benchmark_keys: false,
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
        if state.lock().unwrap().benchmark_keys {
            let target = head.split_whitespace().nth(1).unwrap();
            let key = target.strip_prefix("/v1/objects/").unwrap();
            assert_eq!(key.len(), 64);
            assert!(key.bytes().all(|c| c.is_ascii_hexdigit()));
        } else {
            let target = head.split_whitespace().nth(1).unwrap();
            CacheKey::parse_hex(target.strip_prefix("/v1/objects/").unwrap().as_bytes()).unwrap();
        }
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
    bootstrap: bootstrap::Bootstrap,
    drivers: Rc<racer_dataplane::read::drivers::DriverQueue>,
    admission: Rc<Admission>,
    reactor: Rc<Reactor>,
    crypto: Rc<CryptoClient>,
    engine: Option<RefCell<PageCryptoEngine>>,
    endpoint: RefCell<WorkerEndpoint>,
    flights: Rc<Flights>,
    writer: Rc<StoreWriter>,
    memory: Rc<MemoryCache>,
    coordinator: Rc<Coordinator>,
    io: Rc<HttpIo>,
    responses: Rc<Responses>,
    pipes: Rc<PipePool>,
    writer_task: RefCell<Option<Operation<'static, ()>>>,
    adapter: Adapter,
    _scratch: Scratch,
}
struct RigWorker {
    scratch: Scratch,
    runtime: racer_dataplane::runtime::worker::WorkerRuntime,
    worker: WorkerId,
    directory: Arc<WorkerDirectory>,
    slab_bytes: u64,
}
impl Rig {
    fn new(length: u64, zero_ttl: bool, pages: usize) -> Self {
        Self::with_dirty_pages(length, zero_ttl, pages, pages)
    }
    fn with_dirty_pages(length: u64, zero_ttl: bool, pages: usize, dirty_pages: usize) -> Self {
        Self::assemble(length, zero_ttl, pages, dirty_pages, None)
    }
    fn assemble(
        length: u64,
        zero_ttl: bool,
        pages: usize,
        dirty_pages: usize,
        benchmark: Option<RigWorker>,
    ) -> Self {
        let benchmarking = benchmark.is_some();
        let (scratch, runtime, worker, directory, slab_bytes) = match benchmark {
            Some(worker) => (
                worker.scratch,
                Some(worker.runtime),
                worker.worker,
                worker.directory,
                worker.slab_bytes,
            ),
            None => (
                Scratch::new(),
                None,
                WorkerId(0),
                Arc::new(
                    WorkerDirectory::new(
                        Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                        vec![WorkerId(0)],
                        64,
                    )
                    .unwrap(),
                ),
                512 * 1024 * 1024,
            ),
        };
        fs::create_dir_all(scratch.path.join("production-fixture/origin")).unwrap();
        let origin_path = scratch.socket("production-fixture/origin/socket");
        assert!(origin_path.as_os_str().len() <= 107);
        let adapter = Adapter::start(&origin_path, length, zero_ttl);
        adapter.state.lock().unwrap().benchmark_keys = benchmarking;
        let mut budget = limits(pages);
        budget.dirty_bytes = nz(dirty_pages * (P as usize + 16));
        let (admission, reactor) = match &runtime {
            Some(runtime) => (runtime.admission.clone(), runtime.reactor.clone()),
            None => {
                let admission = Rc::new(Admission::new(budget));
                let reactor = Rc::new(Reactor::new(admission.clone()));
                (admission, reactor)
            }
        };
        let entries = if benchmarking { 512 } else { 64 };
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(32768, P + 16),
            admission.clone(),
        ));
        let http = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 8));
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let index = Rc::new(Index::new(worker, entries));
        let segments = Rc::new(Segments::new(worker, 64 * 1024 * 1024));
        let slabs = Rc::new(Slabs::new(
            worker,
            scratch.path.join("slabs"),
            reactor.clone(),
            slab_bytes,
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
            .configure(admission.clone(), eviction, 64, entries)
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
        let sender_keys = bootstrap::identities(&mut bundle, &keys);
        let published = Arc::new(PublishedState::default());
        let snapshots = Rc::new(SnapshotStore::new(
            ClusterId(CLUSTER.into()),
            published.clone(),
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
                }],
            })
            .unwrap();
        let network = Rc::new(PeerNetwork::new(NodeId(NODE.into()), published).unwrap());
        let certificates = Rc::new(Certificates::new(ClusterId(CLUSTER.into()), keys.clone()));
        let signatures = Rc::new(Signatures::new(keys.clone(), certificates));
        let forwarding = Rc::new(Forwarding::new(signatures.clone()));
        let transfers =
            Rc::new(Transfers::new(http.clone(), io.clone(), None).with_signatures(signatures));
        let peers = Rc::new(Requester::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 64)),
            forwarding.clone(),
            transfers,
            network,
        ));
        let candidates = Rc::new(CandidatePolicy::new(
            NodeId(NODE.into()),
            Rc::new(Placement::new(64)),
            peers.clone(),
        ));
        let origin = Rc::new(
            OriginClient::new(
                snapshots.clone(),
                http,
                io.clone(),
                admission.clone(),
                buffers.clone(),
                scratch.socket_root(),
            )
            .unwrap(),
        );
        assert_eq!(
            snapshot.caches[0].origin_socket,
            PathBuf::from("/run/racer/production-fixture/origin/socket")
        );
        let (crypto, engine) = match runtime {
            Some(runtime) => (runtime.crypto, None),
            None => {
                let (port, engine) = crypto::pair(WorkerId(0), 0, nz(64));
                (
                    Rc::new(CryptoClient::new(port)),
                    Some(RefCell::new(PageCryptoEngine::new(CryptoRuntime {
                        port: engine,
                    }))),
                )
            }
        };
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let flights = Rc::new(Flights::new(admission.clone()));
        let fill = Rc::new(Fill::new(FillDependencies {
            memory: memory.clone(),
            buffers,
            disk,
            writer: writer.clone(),
            origin: origin.clone(),
            candidates: candidates.clone(),
            flights: flights.clone(),
            crypto: Rc::new(PageCrypto::new(keys, crypto.clone())),
            credentials: credentials.clone(),
            admission: admission.clone(),
            metadata_owner: directory.clone(),
        }));
        let metadata = Rc::new(MetadataService::new(
            candidates,
            origin,
            credentials.clone(),
            entries,
            MetadataDependencies {
                index,
                fill: fill.clone(),
                owners: directory.clone(),
            },
        ));
        let pipes = Rc::new(PipePool::new(admission.clone(), reactor.clone()));
        let delivery = Rc::new(Delivery::new(
            pipes.clone(),
            // This fixture polls real crypto inline rather than on its paired
            // production thread. Debug-build page crypto must not count as a
            // two-second client stall while the fixture executor is occupied.
            TIMEOUT,
        ));
        let streams = Rc::new(RangeStreams::new(directory.clone(), delivery.clone(), 2));
        let coordinator = Rc::new(Coordinator::new(
            snapshots,
            metadata,
            fill,
            streams,
            credentials,
        ));
        let endpoint = RefCell::new(directory.install(worker, coordinator.clone()).unwrap());
        let bootstrap = bootstrap::Bootstrap::new(
            sender_keys,
            forwarding,
            admission.clone(),
            coordinator.clone(),
            snapshot.membership.clone(),
        );
        let client_io = Rc::new(HttpIo::for_clients(reactor.clone(), admission.clone()));
        let responses = Rc::new(Responses::new(client_io.clone(), delivery));
        Self {
            bootstrap,
            admission,
            drivers: Rc::new(racer_dataplane::read::drivers::DriverQueue::default()),
            reactor,
            crypto,
            engine,
            endpoint,
            flights,
            writer,
            memory,
            coordinator,
            io: client_io,
            responses,
            pipes,
            writer_task: RefCell::new(None),
            adapter,
            _scratch: scratch,
        }
    }
    fn tick(&self) {
        let _queue = self.drivers.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        self.reactor.poll_budgeted(128).unwrap();
        if let Some(engine) = &self.engine {
            engine.borrow_mut().poll_budgeted(64).unwrap();
        }
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
        let _queue = self.drivers.enter();
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
        match self.coordinator.read(request, scope).await {
            Ok(response) => {
                self.responses.validate(&kind, &response)?;
                if kind.is_head() {
                    drop(
                        self.responses
                            .send(received.connection, response, scope)
                            .await?,
                    );
                } else {
                    drop(
                        self.responses
                            .send_subscription_unobserved(
                                received.connection,
                                response,
                                scope,
                                TIMEOUT,
                            )
                            .await?,
                    );
                }
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
    fn subscribe(&self, fields: &str) -> Reply {
        let (local, mut remote) = UnixStream::pair().unwrap();
        let request = subscription_request(fields);
        let reader = thread::spawn(move || {
            remote.write_all(request.as_bytes()).unwrap();
            receive_subscription(remote)
        });
        let result = self.drive(self.serve(local, &scope()));
        let reply = reader.join();
        assert_eq!(result, Ok(()), "origin calls: {:?}", self.adapter.calls());
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

fn subscription_request(fields: &str) -> String {
    format!(
        "POST /v2/objects/{} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\nRacer-Ordered: 1\r\nRacer-Page-Credits: 1\r\nRacer-Byte-Credits: {P}\r\n{fields}\r\n",
        "ab".repeat(32)
    )
}

// Exercise real duplex credits with one outstanding page, not a legacy HTTP body.
fn receive_subscription(mut stream: UnixStream) -> Reply {
    stream.set_read_timeout(Some(TIMEOUT)).unwrap();
    stream.set_write_timeout(Some(TIMEOUT)).unwrap();
    let head = read_head(&mut stream).expect("subscription response head");
    let fields = fields(&head);
    let status = head.split_whitespace().nth(1).unwrap().parse().unwrap();
    if status != 200 {
        assert_eq!(fields["content-length"], "0");
        return Reply {
            status,
            fields,
            body: vec![],
        };
    }
    assert_eq!(fields["connection"], "close");
    let start: u64 = fields["racer-range-start"].parse().unwrap();
    let end: u64 = fields["racer-range-end"].parse().unwrap();
    let total: u64 = fields["racer-object-length"].parse().unwrap();
    assert!(start <= end && end <= total);
    let mut body = Vec::new();
    let mut pages = 0;
    loop {
        let mut frame = [0; 21];
        stream
            .read_exact(&mut frame)
            .expect("complete subscription frame");
        let number = u64::from_be_bytes(frame[1..9].try_into().unwrap());
        let offset = u64::from_be_bytes(frame[9..17].try_into().unwrap());
        let length = u32::from_be_bytes(frame[17..].try_into().unwrap());
        match frame[0] {
            1 => {
                assert_eq!(offset, start + body.len() as u64);
                assert!(offset < end);
                assert_eq!(number, offset / P);
                assert_eq!(u64::from(length), (end - offset).min(P - offset % P));
                assert!(length > 0);
                let previous = body.len();
                body.resize(previous + length as usize, 0);
                stream
                    .read_exact(&mut body[previous..])
                    .expect("complete page payload");
                pages += 1;
                // The final completion retires remaining leases; only nonfinal
                // pages need releases to admit the next page under one credit.
                if offset + u64::from(length) < end {
                    stream.write_all(&frame[1..9]).unwrap();
                    stream.write_all(&frame[17..]).unwrap();
                }
            }
            2 => {
                assert_eq!((number, offset, length), (pages, end - start, 0));
                assert_eq!(body.len() as u64, end - start);
                assert_eq!(
                    fields["content-length"].parse::<u64>().unwrap(),
                    (pages + 1) * 21 + end - start
                );
                break;
            }
            kind => panic!("unexpected subscription frame {kind}"),
        }
    }
    Reply {
        status,
        fields,
        body,
    }
}

fn check_subscription(reply: &Reply, version: u8, start: u64, end: u64, total: u64) {
    assert_eq!(reply.status, 200);
    assert_eq!(reply.fields["etag"], format!("\"v{version}\""));
    assert_eq!(reply.fields["racer-range-start"], start.to_string());
    assert_eq!(reply.fields["racer-range-end"], end.to_string());
    assert_eq!(reply.fields["racer-object-length"], total.to_string());
    assert_eq!(reply.body.len() as u64, end - start);
    for (offset, value) in reply.body.iter().enumerate() {
        assert_eq!(
            *value,
            byte(version, start + offset as u64),
            "body offset {offset}"
        );
    }
}

#[test]
fn v2_subscription_credits_persist_three_pages_and_serve_partial_disk_range_offline() {
    let length = 2 * P + 113;
    // Fund every page independently of asynchronous write completion. Dirty-only
    // pressure intentionally skips persistence and is covered by separate tests.
    let rig = Rig::with_dirty_pages(length, false, 4, 3);
    check_subscription(&rig.subscribe(""), 1, 0, length, length);
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 4);
    assert_eq!(calls[0].method, "HEAD");
    assert_eq!(calls[0].pin, None);
    for (number, call) in calls[1..].iter().enumerate() {
        assert_eq!(call.method, "GET");
        assert_eq!(call.pin.as_deref(), Some("\"v1\""));
        let expected = format!(
            "bytes={}-{}",
            number as u64 * P,
            (number as u64 + 1) * P - 1
        );
        assert_eq!(call.range.as_deref(), Some(expected.as_str()));
    }
    rig.flush();
    assert_eq!(rig.writer.index().snapshot().unwrap().entries.len(), 3);
    rig.adapter.offline();
    assert!(rig.memory.evict_idle(usize::MAX).unwrap() > 0);
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    let start = P - 7;
    let end = 2 * P + 13;
    check_subscription(
        &rig.subscribe(&format!(
            "If-Match: \"v1\"\r\nRange: bytes={start}-{}\r\n",
            end - 1
        )),
        1,
        start,
        end,
        length,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        4,
        "disk hit contacted disabled origin"
    );
    rig.drive(rig.reactor.drain()).unwrap();
    assert_eq!(rig.admission.used(ResourceClass::Flight), 0);
    assert_eq!(rig.admission.used(ResourceClass::Waiter), 0);
    rig.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
}

#[test]
fn v2_subscription_zero_ttl_revalidates_metadata_without_losing_retained_pin() {
    let rig = Rig::new(113, true, 4);
    check_subscription(&rig.subscribe(""), 1, 0, 113, 113);
    rig.flush();
    rig.adapter.state.lock().unwrap().version = 2;
    check_subscription(&rig.subscribe(""), 2, 0, 113, 113);
    rig.flush();
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 4);
    assert_eq!(
        calls.iter().map(|c| c.method.as_str()).collect::<Vec<_>>(),
        ["HEAD", "GET", "HEAD", "GET"]
    );
    assert_eq!(rig.subscribe("If-Match: \"missing\"\r\n").status, 412);
    assert_eq!(rig.adapter.calls().len(), 5);
    rig.adapter.offline();
    rig.memory.evict_idle(usize::MAX).unwrap();
    check_subscription(&rig.subscribe("If-Match: \"v1\"\r\n"), 1, 0, 113, 113);
    assert_eq!(rig.adapter.calls().len(), 5);
}

#[test]
fn v2_empty_subscription_completes_without_pages_and_v1_get_is_rejected() {
    let rig = Rig::new(0, true, 1);
    check_subscription(&rig.subscribe(""), 1, 0, 0, 0);
    assert_eq!(rig.adapter.calls().len(), 1);
    assert_eq!(rig.adapter.calls()[0].method, "HEAD");
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(rig.admission.used(ResourceClass::DirtyCiphertext), 0);
    let (local, mut remote) = UnixStream::pair().unwrap();
    remote
        .write_all(request("GET", "Range: bytes=0-16777215\r\n").as_bytes())
        .unwrap();
    assert_eq!(
        rig.drive(rig.serve(local, &scope())),
        Err(Error::InvalidRequest)
    );
    assert_eq!(
        rig.adapter.calls().len(),
        1,
        "rejected v1 GET reached origin"
    );
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
    check_subscription(reply, version, start, end, total);
}

#[test]
fn bootstrap_then_pinned_remainder_over_three_pages_and_disk_hits_without_origin() {
    let length = 5 * P + 113;
    let rig = Rig::new(length, false, 10);
    check(&rig.bootstrap(0xab), 1, 0, P, length);
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].method, "GET");
    assert_eq!(calls[0].pin, None);
    assert_eq!(calls[0].range.as_deref(), Some("bytes=0-16777215"));
    check(
        &rig.subscribe(&format!("If-Match: \"v1\"\r\nRange: bytes={P}-\r\n")),
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
        &rig.subscribe("If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
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
        &rig.subscribe("If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
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
fn zero_ttl_revalidates_fresh_reads_but_retained_pins_survive_metadata_change() {
    let rig = Rig::new(113, true, 6);
    check(&rig.bootstrap(0xab), 1, 0, 113, 113);
    assert_eq!(rig.adapter.calls().len(), 1);
    assert_eq!(rig.request("HEAD", "").fields["racer-expires-at"], "0");
    assert_eq!(
        rig.adapter.calls().len(),
        2,
        "zero TTL must revalidate unpinned HEAD"
    );
    rig.adapter.state.lock().unwrap().version = 2;
    check(&rig.bootstrap(0xab), 2, 0, 113, 113);
    assert_eq!(rig.adapter.calls().len(), 3);
    rig.flush();
    rig.adapter.offline();
    check(
        &rig.subscribe("If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
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
    let old = rig.bootstrap(0xab);
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
        &rig.subscribe("If-Match: \"v1\"\r\nRange: bytes=-17\r\n"),
        1,
        96,
        113,
        113,
    );
    assert_eq!(rig.adapter.calls().len(), calls.len());
}

#[test]
#[cfg_attr(debug_assertions, ignore = "35 real crypto pages; run with --release")]
fn cold_pinned_range_exceeds_32_pages_under_bounded_memory() {
    let length = 34 * P + 113;
    let rig = Rig::with_dirty_pages(length, false, 4, 2);
    let head = rig.request("HEAD", "");
    assert_eq!(head.status, 200);
    assert_eq!(head.fields["etag"], "\"v1\"");
    assert_eq!(head.fields["content-length"], length.to_string());
    check(
        &rig.subscribe("If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
        1,
        0,
        length,
        length,
    );
    let calls = rig.adapter.calls();
    assert_eq!(calls.len(), 36);
    assert_eq!(calls[0].method, "HEAD");
    for call in &calls[1..] {
        assert_eq!(call.method, "GET");
        assert_eq!(call.pin.as_deref(), Some("\"v1\""));
    }
    // Window acquisitions may reach the origin out of order. Compare the exact
    // multiset to catch missing pages or retries without imposing arrival order.
    let mut actual: Vec<_> = calls[1..]
        .iter()
        .map(|call| call.range.clone().unwrap())
        .collect();
    let mut expected: Vec<_> = (0..35)
        .map(|number| format!("bytes={}-{}", number * P, (number + 1) * P - 1))
        .collect();
    actual.sort();
    expected.sort();
    assert_eq!(actual, expected);
    rig.flush();
    assert!(rig.admission.used(ResourceClass::Plaintext) <= 4 * P as usize);
}

#[test]
fn dirty_drain_and_byte_pressure_keep_long_range_progressing() {
    let length = 7 * P + 113;
    let rig = Rig::with_dirty_pages(length, false, 4, 2);
    check(&rig.bootstrap(0xab), 1, 0, P, length);
    check(
        &rig.subscribe(&format!("If-Match: \"v1\"\r\nRange: bytes={P}-\r\n")),
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
            &rig.subscribe(&format!(
                "If-Match: \"v1\"\r\nRange: bytes={start}-{}\r\n",
                end - 1
            )),
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
    let first = scope();
    let second = scope();
    let mut turns = 0;
    let release = std::future::poll_fn(|_| {
        if !rig.adapter.calls().is_empty() {
            // Both signed peer requests are polled on every turn while origin
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
            rig.bootstrap.acquire(0xab, &first),
            rig.bootstrap.acquire(0xab, &second),
            release
        )
    });
    check(&a.unwrap(), 1, 0, 113, 113);
    check(&b.unwrap(), 1, 0, 113, 113);
    assert_eq!(
        rig.adapter.calls().len(),
        1,
        "concurrent bootstrap duplicated origin GET"
    );
    rig.adapter.state.lock().unwrap().version = 2;
    check(&rig.bootstrap(0xab), 2, 0, 113, 113);
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
    check(&rig.bootstrap(0xab), 1, 0, P, P);
    rig.flush();
    rig.adapter.offline();
    let baseline_connections = rig.admission.used(ResourceClass::Connection);
    let (slow_local, mut slow_remote) = UnixStream::pair().unwrap();
    let (fast_local, mut fast_remote) = UnixStream::pair().unwrap();
    let wire = subscription_request("If-Match: \"v1\"\r\nRange: bytes=0-\r\n");
    slow_remote.write_all(wire.as_bytes()).unwrap();
    fast_remote.write_all(wire.as_bytes()).unwrap();
    let slow_head = Arc::new(AtomicBool::new(false));
    let saw_head = slow_head.clone();
    let slow_reader = thread::spawn(move || {
        slow_remote.set_read_timeout(Some(TIMEOUT)).unwrap();
        let head = read_head(&mut slow_remote).unwrap();
        assert_eq!(head.split_whitespace().nth(1), Some("200"));
        saw_head.store(true, Ordering::Release);
        // Retain the socket without draining its body until cancellation finishes.
        slow_remote
    });
    let fast_done = Arc::new(AtomicBool::new(false));
    let done = fast_done.clone();
    let fast_reader = thread::spawn(move || {
        let reply = receive_subscription(fast_remote);
        done.store(true, Ordering::Release);
        reply
    });
    let slow_scope = scope();
    let fast_scope = scope();
    let cancel = std::future::poll_fn(|_| {
        if slow_head.load(Ordering::Acquire) && fast_done.load(Ordering::Acquire) {
            assert!(
                rig.admission.used(ResourceClass::Pipe) > rig.pipes.idle_count(),
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
        if rig.reactor.in_flight() == 0
            && rig.admission.used(ResourceClass::Pipe) == rig.pipes.idle_count()
        {
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
        &rig.subscribe("If-Match: \"v1\"\r\nRange: bytes=0-\r\n"),
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
