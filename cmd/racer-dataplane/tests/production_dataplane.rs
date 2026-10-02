//! Single-node production graph validation. Only control publication, origin data,
//! and the worker polling loop are fixtures. No read/storage/crypto success doubles.
use base64::Engine;
use racer_dataplane::{
    client::{RequestParser, response::Responses},
    control::{
        state::{CacheDefinition, PublishedState, SnapshotStore, canonical_socket_paths},
        wire::{self, Publication, PublicationSequence},
    },
    error::{Error, Operation, Result},
    http::{Codec, connection::HttpIo, connection::HttpPool},
    memory::{cache::MemoryCache, delivery::Delivery, pipe::PipePool, pool::BufferPool},
    model::{Limits, PAGE_BYTES, ResourceClass, *},
    origin::OriginClient,
    peer::{
        PeerNetwork, Requester,
        protocol::{FetchMode, Operation as PeerOperation, PeerRequest, PeerResponse},
        server::LocalPageService,
        transport::Transfers,
    },
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
        connection::Signatures,
        credentials::CredentialCrypto,
        forwarding::Forwarding,
        identity::Certificates,
        identity::PendingIdentity,
        identity::{KeyEpochs, Keyring},
    },
    store::{
        StoreReader,
        catalog::{Index, SegmentClock},
        writer::StoreWriter,
    },
    topology::{
        health::LinkHealth,
        membership::{Member, MembershipLease},
        placement::Placement,
        routing::{Paths, RouteBudget},
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

#[path = "production_dataplane/hotpath.rs"]
mod hotpath;
use racer_dataplane as dataplane;
#[path = "support/enrollment.rs"]
#[allow(dead_code)]
mod fixture_io;
use fixture_io::{fields, read_head};

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
    bootstrap: Bootstrap,
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
    // One explicit graph serves both pressure scenarios and the worker hotpath.
    // WorkerApplication owns private key/publication/storage state, so using its
    // lifecycle here would remove the admission and held-reader controls these
    // scenarios need. Real executable lifecycle coverage lives in process_restart.
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
            Codec::new(32768),
            admission.clone(),
            P + 16,
        ));
        let http = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 8));
        let buffers = BufferPool::new(admission.clone());
        let (keys, sender_keys) = fixture_keys();
        let published = Arc::new(PublishedState::default());
        let availability = Rc::new(racer_dataplane::control::state::Availability::new(
            published.clone(),
            keys.clone(),
        ));
        let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
        let (index, disk, writer) = open_fixture_storage(
            worker,
            scratch.path.join("slabs"),
            slab_bytes,
            entries,
            &reactor,
            &admission,
            buffers.clone(),
            availability,
        );
        let snapshots = Rc::new(SnapshotStore::new(
            ClusterId(CLUSTER.into()),
            published.clone(),
            4,
        ));
        let snapshot = snapshots.publish(fixture_publication()).unwrap();
        let network = Rc::new(PeerNetwork::new(NodeId(NODE.into()), published.clone()).unwrap());
        let certificates = Rc::new(Certificates::new(ClusterId(CLUSTER.into()), keys.clone()));
        let signatures = Rc::new(Signatures::new(keys.clone(), certificates));
        let forwarding = Rc::new(Forwarding::new(signatures.clone()));
        let transfers = Rc::new(Transfers::new(
            http.clone(),
            io.clone(),
            None,
            admission.clone(),
            Rc::new(racer_dataplane::peer::protocol::SecurityCodec::new(
                admission.clone(),
                buffers.clone(),
            )),
            signatures,
        ));
        let peers = Rc::new(Requester::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 64)),
            forwarding.clone(),
            transfers,
            network,
        ));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let candidates = Rc::new(CandidatePolicy::new(
            NodeId(NODE.into()),
            Rc::new(Placement::new(64)),
            peers.clone(),
            credentials.clone(),
            Arc::new(Default::default()),
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
        let availability = Rc::new(racer_dataplane::control::state::Availability::new(
            published.clone(),
            keys.clone(),
        ));
        let flights = Rc::new(Flights::new(admission.clone(), availability.clone()));
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
            availability,
        ));
        let endpoint = RefCell::new(directory.install(worker, coordinator.clone()).unwrap());
        let bootstrap = Bootstrap::new(
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
        let lease =
            racer_dataplane::http::connection::from_accepted(stream.into(), &self.admission)?;
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

    fn bootstrap(&self, key: u8) -> Reply {
        self.drive(self.bootstrap.acquire(key, &scope())).unwrap()
    }
}

// Atomic bootstrap enters the verified local peer service boundary. Signatures,
// credential opening, origin HTTP, encryption, and persistence are real; the
// socket/session transport is covered separately from this pressure fixture.
const REQUESTER: &str = "33333333-3333-4333-8333-333333333333";

struct Bootstrap {
    sender: Forwarding,
    receiver: Rc<Forwarding>,
    credentials: CredentialCrypto,
    coordinator: Rc<Coordinator>,
    membership: MembershipLease,
}

fn open_fixture_storage(
    worker: WorkerId,
    path: PathBuf,
    slab_bytes: u64,
    entries: usize,
    reactor: &Rc<Reactor>,
    admission: &Rc<Admission>,
    buffers: BufferPool,
    availability: Rc<racer_dataplane::control::state::Availability>,
) -> (Rc<Index>, Rc<StoreReader>, Rc<StoreWriter>) {
    let index = Rc::new(Index::new(worker, entries, availability.clone()));
    let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
    let slabs = Rc::new(page_alloc::Slab::new(
        path.join(format!("worker-{}-slab-0.dat", worker.0)),
        slab_bytes,
        64 * 1024 * 1024,
        racer_dataplane::model::PAGE_BYTES as usize
            + racer_dataplane::store::format::MAX_HEADER_BYTES
            + 16,
    ));
    let eviction = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
    let disk = Rc::new(StoreReader::new(
        eviction.clone(),
        index.clone(),
        segments.clone(),
        slabs.clone(),
        admission.clone(),
        reactor.clone(),
        buffers,
    ));
    let writer = Rc::new(StoreWriter::new(
        index.clone(),
        segments,
        slabs,
        admission.clone(),
        reactor.clone(),
        availability,
    ));
    writer
        .configure(admission.clone(), eviction, 64, entries)
        .unwrap();
    futures::executor::block_on(writer.open()).expect("real O_DIRECT slab must open");
    (index, disk, writer)
}

fn fixture_publication() -> Publication {
    let (client_socket, origin_socket) = canonical_socket_paths("production-fixture").unwrap();
    Publication {
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
            site: String::new(),
        }],
        caches: vec![CacheDefinition {
            id: CacheId(CACHE.into()),
            name: "production-fixture".into(),
            client_socket,
            origin_socket,
        }],
    }
}

fn fixture_keys() -> (Rc<Keyring>, Rc<Keyring>) {
    let keys = Rc::new(Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(NODE.into()),
        Arc::new(KeyEpochs::default()),
    ));
    // The wire fixture reuses material across purposes; the real keyring
    // requires distinct material. These are deterministic test-only keys.
    let mut bundle: serde_json::Value = serde_json::from_slice(include_bytes!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../internal/racer/wire/testdata/bundle.json"
    )))
    .unwrap();
    for (i, key) in bundle["cache_keys"]
        .as_array_mut()
        .unwrap()
        .iter_mut()
        .enumerate()
    {
        key["id"] = base64::engine::general_purpose::STANDARD
            .encode(
                racer_dataplane::model::KeyId::from_generation(1, i as u32 + 1)
                    .unwrap()
                    .0,
            )
            .into();
        key["material"] = base64::engine::general_purpose::STANDARD
            .encode([i as u8 + 7; 32])
            .into();
    }
    let sender = identities(&mut bundle, &keys);
    (keys, sender)
}

fn identities(bundle: &mut serde_json::Value, keys: &Keyring) -> Rc<Keyring> {
    let (ca, ca_key) = fixture_io::ca();
    let roots = vec![ca.der().to_vec()];
    bundle["peer_trust_roots"] =
        serde_json::json!([base64::engine::general_purpose::STANDARD.encode(&roots[0])]);
    let sender = Rc::new(Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(REQUESTER.into()),
        Arc::new(KeyEpochs::default()),
    ));
    for keyring in [keys, sender.as_ref()] {
        keyring
            .install(wire::decode_bundle(&serde_json::to_vec(bundle).unwrap()).unwrap())
            .unwrap();
        keyring
            .install_signing_identity(fixture_io::signing_identity(
                PendingIdentity::generate().unwrap(),
                &ca,
                &ca_key,
                ClusterId(CLUSTER.into()),
                keyring.node().clone(),
            ))
            .unwrap();
    }
    sender
}

impl Bootstrap {
    fn new(
        sender: Rc<Keyring>,
        receiver: Rc<Forwarding>,
        admission: Rc<Admission>,
        coordinator: Rc<Coordinator>,
        membership: MembershipLease,
    ) -> Self {
        let certificates = Rc::new(Certificates::new(ClusterId(CLUSTER.into()), sender.clone()));
        Self {
            sender: Forwarding::new(Rc::new(Signatures::new(sender.clone(), certificates))),
            receiver,
            credentials: CredentialCrypto::new(sender, admission),
            coordinator,
            membership,
        }
    }

    async fn acquire(&self, key: u8, scope: &RequestScope) -> Result<Reply> {
        let context = OriginContext {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([key; 32]),
            },
            metadata: Some(OpaqueMetadata::from_header(b"fixture-metadata")?),
            authorization: Some(Authorization::from_header(b"fixture-credential")?),
        };
        let attempt = AttemptId(scope.request.0);
        let request = PeerRequest {
            operation: PeerOperation::Bootstrap {
                object: context.object.clone(),
                mode: FetchMode::Acquire,
            },
            origin: self.credentials.seal(&context, attempt, scope)?,
            route: RouteBudget {
                membership: self.membership.version,
                request: scope.request,
                attempt,
                destination: NodeId(NODE.into()),
                visited: vec![NodeId(REQUESTER.into())],
                remaining_links: 4,
                remaining_attempts: 8,
                deadline: scope.deadline,
            },
        };
        let (signed, binding) = self.sender.sign_request(request)?;
        let verified = self.receiver.verify_request(signed)?;
        let response_binding = verified.binding().clone();
        let response = self
            .coordinator
            .serve_peer(verified, self.membership.clone(), scope)
            .await?;
        let verified = self.sender.verify_response(
            self.receiver.sign_response(&response_binding, response)?,
            &binding,
        )?;
        match verified.response() {
            PeerResponse::Bootstrap {
                metadata,
                page_zero,
            } => {
                let mut body = Vec::new();
                if let Some(page) = page_zero {
                    metadata.immutable().validate_page(page.envelope())?;
                    assert_eq!(page.envelope().page.number, PageNumber(0));
                    body = vec![0; page.envelope().plaintext_length as usize];
                    racer_crypto::aead::open(
                        &[7; 32],
                        &page.envelope().nonce.0,
                        &racer_dataplane::security::aead::page_aad(page.envelope())?,
                        page.bytes(),
                        &mut body,
                    )
                    .expect("authenticate actual bootstrap ciphertext");
                } else {
                    assert_eq!(metadata.length, 0);
                }
                assert_eq!(body.len() as u64, metadata.length.min(P));
                // Normalize only the assertion view shared with client scenarios.
                // These fields are not a fabricated HTTP response or read result.
                Ok(Reply {
                    status: 200,
                    fields: BTreeMap::from([
                        ("etag".into(), metadata.version.etag.as_str().into()),
                        ("racer-object-length".into(), metadata.length.to_string()),
                        ("racer-range-start".into(), "0".into()),
                        ("racer-range-end".into(), body.len().to_string()),
                        (
                            "racer-expires-at".into(),
                            metadata
                                .expires_at
                                .as_system_time()
                                .duration_since(UNIX_EPOCH)
                                .unwrap()
                                .as_millis()
                                .to_string(),
                        ),
                    ]),
                    body,
                })
            }
            PeerResponse::Unavailable | PeerResponse::Overloaded => Ok(Reply {
                status: 503,
                fields: BTreeMap::new(),
                body: vec![],
            }),
            _ => panic!("unexpected bootstrap response"),
        }
    }
}

fn page(key: u8, version: u8) -> PageId {
    PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(CACHE.into()),
                key: CacheKey([key; 32]),
            },
            etag: StrongEtag::parse(format!("\"v{version}\"").as_bytes()).unwrap(),
        },
        number: PageNumber(0),
    }
}

#[test]
fn zero_ttl_bootstrap_reclaims_idle_versions_and_new_objects_beyond_byte_budget() {
    for new_objects in [false, true] {
        let rig = Rig::new(P, true, 4);
        for version in 1..=10 {
            rig.adapter.state.lock().unwrap().version = version;
            let key = if new_objects { version } else { 0xab };
            let reply = rig.bootstrap(key);
            check(&reply, version, 0, P, P);
            assert_eq!(reply.fields["racer-expires-at"], "0");
            rig.flush();
            assert_eq!(rig.adapter.calls().len(), version as usize);
            assert!(rig.admission.used(ResourceClass::Plaintext) <= 4 * P as usize);
            if version == 4 {
                assert_eq!(rig.admission.used(ResourceClass::Plaintext), 4 * P as usize);
            }
        }
        assert!(
            rig.memory
                .get(&page(if new_objects { 1 } else { 0xab }, 1))
                .unwrap()
                .is_none()
        );
        for call in rig.adapter.calls() {
            assert_eq!(call.method, "GET");
            assert_eq!(call.pin, None);
            assert_eq!(call.range.as_deref(), Some("bytes=0-16777215"));
        }
        rig.memory.evict_idle(usize::MAX).unwrap();
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(
            rig.admission.used(ResourceClass::Ciphertext),
            rig.writer.retained_staging_bytes()
        );
    }
}

#[test]
fn bootstrap_preserves_active_reader_and_inflight_admission_until_cancellation() {
    let rig = Rig::new(113, true, 2);
    check(&rig.bootstrap(0xab), 1, 0, 113, 113);
    rig.flush();
    let held = rig.memory.get(&page(0xab, 1)).unwrap().unwrap().plaintext;
    {
        let mut state = rig.adapter.state.lock().unwrap();
        state.version = 2;
        state.paused = true;
    }
    let pending_scope = scope();
    let blocked_scope = scope();
    let observer = async {
        std::future::poll_fn(|_| {
            if rig.adapter.calls().len() == 2 {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        assert_eq!(
            rig.admission.used(ResourceClass::Plaintext),
            P as usize + 113
        );
        assert_eq!(
            rig.bootstrap
                .acquire(3, &blocked_scope)
                .await
                .unwrap()
                .status,
            503
        );
        assert_eq!(
            rig.adapter.calls().len(),
            2,
            "overload must precede origin I/O"
        );
        assert_eq!(
            rig.admission.used(ResourceClass::Plaintext),
            P as usize + 113
        );
        assert!(rig.memory.get(&page(0xab, 1)).unwrap().is_some());
        assert_eq!(held.bytes()[0], byte(1, 0));
        pending_scope.cancel().unwrap();
    };
    let (pending_result, ()) =
        rig.drive(async { futures::join!(rig.bootstrap.acquire(2, &pending_scope), observer) });
    assert!(matches!(pending_result, Err(Error::Cancelled)));
    rig.drive(std::future::poll_fn(|_| {
        if rig.admission.used(ResourceClass::Plaintext) == 113 {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    }));
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 113);
    rig.adapter.state.lock().unwrap().paused = false;
    check(&rig.bootstrap(3), 2, 0, 113, 113);
    rig.flush();
    // Reclamation must leave the independently held page readable.
    rig.adapter.state.lock().unwrap().version = 3;
    check(&rig.bootstrap(4), 3, 0, 113, 113);
    rig.flush();
    assert_eq!(held.bytes()[0], byte(1, 0));
    assert!(rig.memory.get(&page(0xab, 1)).unwrap().is_some());
    drop(held);
    rig.memory.evict_idle(usize::MAX).unwrap();
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(
        rig.admission.used(ResourceClass::Ciphertext),
        rig.writer.retained_staging_bytes()
    );
}

#[test]
fn failed_and_empty_bootstraps_release_reclaimed_plaintext_reservations() {
    let rig = Rig::new(113, true, 1);
    check(&rig.bootstrap(0xab), 1, 0, 113, 113);
    rig.flush();
    rig.adapter.offline();
    for attempt in 0..3 {
        // These are separate half-open probes, rather than retries inside the
        // circuit's exponential backoff window.
        thread::sleep(Duration::from_secs(1));
        assert_eq!(rig.bootstrap(0xab).status, 503);
        assert_eq!(
            rig.adapter.calls().len(),
            attempt + 2,
            "failure must reach origin"
        );
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(
            rig.admission.used(ResourceClass::Ciphertext),
            rig.writer.retained_staging_bytes()
        );
        assert_eq!(rig.admission.used(ResourceClass::DirtyCiphertext), 0);
    }
    thread::sleep(Duration::from_secs(1));
    {
        let mut state = rig.adapter.state.lock().unwrap();
        state.online = true;
        state.version = 2;
        state.length = 0;
    }
    let empty = rig.bootstrap(0xab);
    assert_eq!(empty.status, 200);
    assert!(empty.body.is_empty());
    assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(
        rig.admission.used(ResourceClass::Ciphertext),
        rig.writer.retained_staging_bytes()
    );
    rig.adapter.state.lock().unwrap().length = 113;
    // Use another version because an immutable version's length cannot change.
    rig.adapter.state.lock().unwrap().version = 3;
    check(&rig.bootstrap(0xab), 3, 0, 113, 113);
    rig.flush();
    assert_eq!(rig.adapter.calls().len(), 6);
}

#[test]
fn full_page_writeback_reclaims_idle_ciphertext_at_default_worker_budget() {
    let rig = Rig::new(P, true, 4);
    // Leave exactly the default four-worker shard's 64 MiB ciphertext budget.
    // The fixture otherwise provides additional staging headroom that hides this
    // failure. This charge is unavailable to reclamation, like another live user.
    let _outside_worker_budget = rig
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            rig.admission.limit(ResourceClass::Ciphertext) - 64 * 1024 * 1024,
        )
        .unwrap();
    for version in 1..=6 {
        rig.adapter.state.lock().unwrap().version = version;
        let read_scope = scope();
        let reply = rig.drive(rig.bootstrap.acquire(0xab, &read_scope)).unwrap();
        check(&reply, version, 0, P, P);
        read_scope.cancel().unwrap();
        rig.flush();
        assert_eq!(rig.writer.pending_count(), 0);
        assert_eq!(rig.writer.discarded_count(), 0);
        let entries = rig.writer.index().snapshot().unwrap().entries;
        assert!(
            entries.iter().any(|(page, _)| page.version.etag
                == StrongEtag::parse(format!("\"v{version}\"").as_bytes()).unwrap()),
            "successful full-page fill v{version} lost disk publication with an idle writer and reclaimable cached bytes"
        );
        assert!(
            rig.admission.used(ResourceClass::Ciphertext)
                <= rig.admission.limit(ResourceClass::Ciphertext)
        );
    }
    assert_eq!(rig.adapter.calls().len(), 6);
    rig.memory.evict_idle(usize::MAX).unwrap();
    rig.adapter.offline();
    check(
        &rig.subscribe("If-Match: \"v6\"\r\nRange: bytes=0-16777215\r\n"),
        6,
        0,
        P,
        P,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        6,
        "persisted target must be reusable offline"
    );
}

#[test]
fn writeback_staging_preserves_live_readers_and_recovers_after_release() {
    let rig = Rig::new(P, true, 4);
    let _outside_worker_budget = rig
        .admission
        .reserve(
            None,
            ResourceClass::Ciphertext,
            rig.admission.limit(ResourceClass::Ciphertext) - 64 * 1024 * 1024,
        )
        .unwrap();
    let mut held = Vec::new();
    for version in 1..=2 {
        rig.adapter.state.lock().unwrap().version = version;
        check(&rig.bootstrap(0xab), version, 0, P, P);
        rig.flush();
        held.push(rig.memory.get(&page(0xab, version)).unwrap().unwrap());
    }
    rig.adapter.state.lock().unwrap().version = 3;
    check(&rig.bootstrap(0xab), 3, 0, P, P);
    rig.flush();
    assert_eq!(
        rig.writer.index().snapshot().unwrap().entries.len(),
        2,
        "live leases cannot be reclaimed to force persistence"
    );
    for (index, page) in held.iter().enumerate() {
        assert_eq!(page.plaintext.bytes()[0], byte(index as u8 + 1, 0));
        assert!(rig.memory.get(page.plaintext.page()).unwrap().is_some());
    }
    drop(held);
    rig.adapter.state.lock().unwrap().version = 4;
    check(&rig.bootstrap(0xab), 4, 0, P, P);
    rig.flush();
    assert_eq!(rig.writer.discarded_count(), 0);
    assert!(
        rig.admission.used(ResourceClass::Ciphertext)
            <= rig.admission.limit(ResourceClass::Ciphertext)
    );
    rig.memory.evict_idle(usize::MAX).unwrap();
    rig.adapter.offline();
    let calls = rig.adapter.calls().len();
    check(
        &rig.subscribe("If-Match: \"v4\"\r\nRange: bytes=0-\r\n"),
        4,
        0,
        P,
        P,
    );
    assert_eq!(
        rig.adapter.calls().len(),
        calls,
        "released staging must persist v4 for offline reads"
    );
}

#[test]
fn small_versions_keep_persisting_at_index_capacity_and_serve_from_disk_offline() {
    let rig = Rig::new(113, true, 4);
    rig.writer.index().set_page_capacity(2).unwrap();
    for version in 1..=4 {
        rig.adapter.state.lock().unwrap().version = version;
        check(&rig.bootstrap(0xab), version, 0, 113, 113);
        rig.flush();
        assert!(rig.writer.index().snapshot().unwrap().entries.len() <= 2);
        rig.memory.evict_idle(usize::MAX).unwrap();
        assert_eq!(rig.admission.used(ResourceClass::Plaintext), 0);
        rig.adapter.offline();
        let calls = rig.adapter.calls().len();
        check(
            &rig.subscribe(&format!("If-Match: \"v{version}\"\r\nRange: bytes=0-\r\n")),
            version,
            0,
            113,
            113,
        );
        assert_eq!(
            rig.adapter.calls().len(),
            calls,
            "each newly served version must persist for offline reads"
        );
        rig.memory.evict_idle(usize::MAX).unwrap();
        rig.adapter.state.lock().unwrap().online = true;
    }
    assert_eq!(rig.writer.discarded_count(), 0);
    assert_eq!(rig.adapter.calls().len(), 4);
    rig.adapter.offline();
    for version in [3, 4] {
        check(
            &rig.subscribe(&format!("If-Match: \"v{version}\"\r\nRange: bytes=0-\r\n")),
            version,
            0,
            113,
            113,
        );
        rig.memory.evict_idle(usize::MAX).unwrap();
    }
    assert_eq!(
        rig.adapter.calls().len(),
        4,
        "disk hits must not reach origin"
    );
}
fn request(method: &str, fields: &str) -> String {
    // GET is retained only to verify rejection of the retired client protocol.
    let version = if method == "GET" { "v1" } else { "v2" };
    format!(
        "{method} /{version}/objects/{} HTTP/1.1\r\nHost: racer\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\n{fields}\r\n",
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
