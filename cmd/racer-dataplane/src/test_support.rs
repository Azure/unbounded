//! Deterministic test-only seams; no fake implementation is linked into production.
use crate::admission::AdmissionPolicy;
use crate::control::PublishedState;
use crate::control::SnapshotStore;
use crate::http::Delivery;
use crate::memory::BufferPool;
use crate::memory::cache::MemoryCache;
use crate::model::MembershipVersion;
use crate::model::ObjectMetadata;
use crate::model::WorkerId;
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
use crate::runtime::Reactor;
use crate::runtime::crypto;
use crate::runtime::crypto::CryptoClient;
use crate::runtime::worker::CryptoRuntime;
use racer_control_wire::CacheDefinition;
use racer_control_wire::Publication;

use crate::security::aead::PageCrypto;
use crate::security::aead::PageCryptoEngine;
use crate::security::credentials::CredentialCrypto;
use crate::store::StoreReader;
use crate::store::StoreWriter;
use crate::store::catalog::Index;
use crate::store::catalog::SegmentClock;
use crate::test_support::origin::AdapterOrigin;
use crate::topology::Placement;
use racer_control_wire::PublicationSequence;
use std::cell::RefCell;
use std::num::NonZeroU32;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use uring_runtime::drivers::DriverQueue;
pub mod clock {
    use crate::error::Error;
    use crate::error::Result;
    use std::cell::Cell;
    use std::time::Duration;
    use std::time::Instant;
    use std::time::SystemTime;
    use std::time::UNIX_EPOCH;
    use uring_runtime::deadline::Deadline;

    /// Pure deadline/freshness fixture. Reactor timers use SimulationClock.
    pub struct Clock {
        now: Cell<Instant>,
        wall: Cell<SystemTime>,
    }
    impl Default for Clock {
        fn default() -> Self {
            Self {
                now: Cell::new(Instant::now()),
                wall: Cell::new(UNIX_EPOCH),
            }
        }
    }
    impl Clock {
        pub fn now(&self) -> Instant {
            self.now.get()
        }
        pub fn wall(&self) -> SystemTime {
            self.wall.get()
        }
        pub fn advance(&self, duration: Duration) -> Result<()> {
            let now = self
                .now()
                .checked_add(duration)
                .ok_or(Error::InvalidRange)?;
            let wall = self
                .wall()
                .checked_add(duration)
                .ok_or(Error::InvalidRange)?;
            self.now.set(now);
            self.wall.set(wall);
            Ok(())
        }
        pub fn jump_wall(&self, time: SystemTime) {
            self.wall.set(time);
        }
        pub fn check_deadline(&self, deadline: Deadline) -> Result<()> {
            if self.now() >= deadline.0 {
                Err(Error::DeadlineExceeded)
            } else {
                Ok(())
            }
        }
    }
    #[test]
    fn wall_corrections_do_not_extend_original_deadlines() {
        let clock = Clock::default();
        let original = clock.now();
        let deadline = Deadline(original + Duration::from_secs(2));
        clock.advance(Duration::from_secs(1)).unwrap();
        clock.jump_wall(UNIX_EPOCH - Duration::from_secs(100));
        assert_eq!(clock.now(), original + Duration::from_secs(1));
        assert_eq!(clock.check_deadline(deadline), Ok(()));
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(clock.check_deadline(deadline), Err(Error::DeadlineExceeded));
        let before = (clock.now(), clock.wall());
        assert_eq!(clock.advance(Duration::MAX), Err(Error::InvalidRange));
        assert_eq!((clock.now(), clock.wall()), before);
    }
    #[test]
    fn wall_time_drives_freshness_but_expired_versions_still_answer_pins() {
        use crate::model::CacheId;
        use crate::model::CacheKey;
        use crate::model::CurrentVersion;
        use crate::model::ExpiresAt;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::StrongEtag;
        use crate::model::VersionMetadata;
        let clock = Clock::default();
        let descriptor = VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: 42,
        };
        let current = CurrentVersion {
            version: descriptor.version.clone(),
            expires_at: ExpiresAt::test_time(clock.wall() + Duration::from_secs(2)),
        };
        assert_eq!(
            current
                .resolve(&descriptor, clock.wall())
                .unwrap()
                .unwrap()
                .length,
            42
        );
        clock.advance(Duration::from_secs(2)).unwrap();
        assert_eq!(current.resolve(&descriptor, clock.wall()), Ok(None));
        assert_eq!(descriptor.for_pin().length, 42);
        assert_eq!(
            descriptor.for_pin().expires_at,
            ExpiresAt::from_system_time(UNIX_EPOCH).unwrap()
        );
        let monotonic = clock.now();
        clock.jump_wall(UNIX_EPOCH);
        assert_eq!(clock.now(), monotonic);
    }
}
pub mod origin {
    //! Controllable adapter boundary shared by read and client scenarios.
    //! The client, HTTP parser, reactor, and plaintext admission remain production code.
    use crate::admission::AdmissionPolicy;
    use crate::control::SnapshotStore;
    use crate::http::Codec;
    use crate::memory::BufferPool;
    use crate::model::ObjectMetadata;
    use crate::model::PAGE_BYTES;
    use crate::origin::OriginClient;
    use crate::runtime::Reactor;
    use std::collections::BTreeMap;
    use std::collections::BTreeSet;
    use std::collections::VecDeque;
    use std::fs::File;
    use std::io::Read;
    use std::io::Write;
    use std::os::fd::AsRawFd;
    use std::os::unix::net::UnixListener;
    use std::os::unix::net::UnixStream;
    use std::path::PathBuf;
    use std::rc::Rc;
    use std::sync::Arc;
    use std::sync::Mutex;
    use std::sync::atomic::AtomicBool;
    use std::sync::atomic::AtomicU64;
    use std::sync::atomic::Ordering;
    use std::thread;
    use std::thread::JoinHandle;
    use std::time::Duration;
    use std::time::Instant;
    use std::time::UNIX_EPOCH;

    #[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
    pub enum RequestKind {
        Head,
        InitialGet,
        PinnedGet,
    }

    #[derive(Clone, Debug)]
    pub struct Call {
        pub kind: RequestKind,
        pub page: u64,
        pub if_match: Option<Vec<u8>>,
    }

    struct State {
        metadata: ObjectMetadata,
        missing: bool,
        body: Option<Vec<u8>>,
        calls: Vec<Call>,
        completed: Vec<Call>,
        rejections: BTreeMap<RequestKind, VecDeque<u16>>,
        rejected_pages: BTreeMap<u64, u16>,
        blocked: BTreeSet<RequestKind>,
        delays: BTreeMap<RequestKind, Duration>,
    }

    pub struct AdapterOrigin {
        state: Arc<Mutex<State>>,
        stop: Arc<AtomicBool>,
        server: Option<JoinHandle<()>>,
        directory: PathBuf,
        // Keep the short /proc path valid even for long worktree names.
        _directory_fd: File,
        pub root: PathBuf,
    }

    impl AdapterOrigin {
        /// With no explicit body, three-byte objects contain `abc`; other pages
        /// contain their page number repeated. Large fixtures need no object allocation.
        pub fn new(cache_name: &str, metadata: ObjectMetadata) -> Self {
            static NEXT: AtomicU64 = AtomicU64::new(0);
            assert!(!cache_name.contains('/') && !cache_name.is_empty());
            let directory = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!(
                    "adapter-origin-{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                ));
            std::fs::create_dir_all(directory.join(cache_name).join("origin")).unwrap();
            let directory_fd = File::open(&directory).unwrap();
            let root = PathBuf::from(format!(
                "/proc/{}/fd/{}",
                std::process::id(),
                directory_fd.as_raw_fd()
            ));
            let listener = UnixListener::bind(root.join(cache_name).join("origin/socket")).unwrap();
            listener.set_nonblocking(true).unwrap();
            let state = Arc::new(Mutex::new(State {
                metadata,
                missing: false,
                body: None,
                calls: vec![],
                completed: vec![],
                rejections: BTreeMap::new(),
                rejected_pages: BTreeMap::new(),
                blocked: BTreeSet::new(),
                delays: BTreeMap::new(),
            }));
            let stop = Arc::new(AtomicBool::new(false));
            let server = {
                let state = state.clone();
                let stop = stop.clone();
                thread::spawn(move || {
                    let mut connections = vec![];
                    while !stop.load(Ordering::Acquire) {
                        match listener.accept() {
                            Ok((stream, _)) => {
                                let state = state.clone();
                                let stop = stop.clone();
                                connections
                                    .push(thread::spawn(move || serve(stream, &state, &stop)));
                            }
                            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                                thread::sleep(Duration::from_millis(1))
                            }
                            Err(error) => panic!("adapter accept: {error}"),
                        }
                    }
                    for connection in connections {
                        connection.join().unwrap();
                    }
                })
            };
            Self {
                state,
                stop,
                server: Some(server),
                directory,
                _directory_fd: directory_fd,
                root,
            }
        }

        pub fn client(
            &self,
            snapshots: Rc<SnapshotStore>,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            reactor: Rc<Reactor>,
            buffers: BufferPool,
        ) -> Rc<OriginClient> {
            Rc::new(
                OriginClient::new(
                    snapshots,
                    Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 8)),
                    Rc::new(crate::http::new_io(
                        reactor,
                        Codec::new(32768),
                        admission.clone(),
                        PAGE_BYTES,
                    )),
                    admission,
                    buffers,
                    self.root.clone(),
                )
                .unwrap(),
            )
        }

        pub fn set_version(&self, metadata: ObjectMetadata) {
            self.state.lock().unwrap().metadata = metadata;
        }
        /// Missing current objects return 404; unsatisfied explicit pins return 412.
        pub fn set_missing(&self, missing: bool) {
            self.state.lock().unwrap().missing = missing;
        }
        pub fn set_body(&self, body: Vec<u8>) {
            let mut state = self.state.lock().unwrap();
            assert_eq!(body.len() as u64, state.metadata.length);
            state.body = Some(body);
        }
        pub fn reject_next(&self, kind: RequestKind, status: u16) {
            assert!((400..600).contains(&status));
            self.state
                .lock()
                .unwrap()
                .rejections
                .entry(kind)
                .or_default()
                .push_back(status);
        }
        /// Reject every GET for this page, including retries, without failing HEAD.
        pub fn reject_page(&self, page: u64, status: u16) {
            assert!((400..600).contains(&status));
            self.state
                .lock()
                .unwrap()
                .rejected_pages
                .insert(page, status);
        }
        pub fn block(&self, kind: RequestKind) {
            self.state.lock().unwrap().blocked.insert(kind);
        }
        pub fn release(&self, kind: RequestKind) {
            self.state.lock().unwrap().blocked.remove(&kind);
        }
        pub fn delay(&self, kind: RequestKind, delay: Duration) {
            self.state.lock().unwrap().delays.insert(kind, delay);
        }
        pub fn calls(&self) -> Vec<Call> {
            self.state.lock().unwrap().calls.clone()
        }
        pub fn count(&self, kind: RequestKind) -> usize {
            self.calls().iter().filter(|call| call.kind == kind).count()
        }
        pub fn completed(&self, kind: RequestKind) -> usize {
            self.state
                .lock()
                .unwrap()
                .completed
                .iter()
                .filter(|call| call.kind == kind)
                .count()
        }
    }

    impl Drop for AdapterOrigin {
        fn drop(&mut self) {
            self.stop.store(true, Ordering::Release);
            self.server.take().unwrap().join().unwrap();
            std::fs::remove_dir_all(&self.directory).unwrap();
        }
    }

    fn serve(mut stream: UnixStream, state: &Mutex<State>, stop: &AtomicBool) {
        stream
            .set_read_timeout(Some(Duration::from_millis(20)))
            .unwrap();
        stream
            .set_write_timeout(Some(Duration::from_millis(100)))
            .unwrap();
        let deadline = Instant::now() + Duration::from_secs(10);
        let mut request = Vec::new();
        while !request.ends_with(b"\r\n\r\n") {
            if stop.load(Ordering::Acquire) || Instant::now() >= deadline {
                return;
            }
            let mut byte = [0];
            match stream.read(&mut byte) {
                Ok(0) => return,
                Ok(_) => request.push(byte[0]),
                Err(error)
                    if matches!(
                        error.kind(),
                        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
                    ) =>
                {
                    continue;
                }
                Err(_) => return,
            }
            assert!(request.len() <= 32768, "oversized adapter request");
        }
        let request = Codec::new(32768).decode_head(&request).unwrap().unwrap().0;
        let head =
            matches!(&request.start, http1::StartLine::Request { method, .. } if method == "HEAD");
        let if_match = request.unique("If-Match").unwrap().map(<[u8]>::to_vec);
        let first = request
            .unique("Range")
            .unwrap()
            .map(|range| {
                std::str::from_utf8(range)
                    .unwrap()
                    .strip_prefix("bytes=")
                    .unwrap()
                    .split('-')
                    .next()
                    .unwrap()
                    .parse::<u64>()
                    .unwrap()
            })
            .unwrap_or(0);
        let kind = if head {
            RequestKind::Head
        } else if if_match.is_some() {
            RequestKind::PinnedGet
        } else {
            RequestKind::InitialGet
        };
        let call = Call {
            kind,
            page: first / PAGE_BYTES,
            if_match,
        };
        let started = Instant::now();
        state.lock().unwrap().calls.push(call.clone());
        loop {
            if stop.load(Ordering::Acquire) || Instant::now() >= deadline {
                return;
            }
            let state = state.lock().unwrap();
            let blocked = state.blocked.contains(&kind)
                || started.elapsed() < state.delays.get(&kind).copied().unwrap_or_default();
            drop(state);
            if !blocked {
                break;
            }
            thread::sleep(Duration::from_millis(1));
        }
        let mut state = state.lock().unwrap();
        state.completed.push(call.clone());
        let rejected = state
            .rejections
            .entry(kind)
            .or_default()
            .pop_front()
            .or_else(|| {
                (!head)
                    .then(|| state.rejected_pages.get(&call.page).copied())
                    .flatten()
            });
        let metadata = &state.metadata;
        let status = rejected.unwrap_or_else(|| {
            if state.missing {
                if call.if_match.is_some() { 412 } else { 404 }
            } else if call
                .if_match
                .as_deref()
                .is_some_and(|etag| etag != metadata.version.etag.as_bytes())
            {
                412
            } else if !head && first >= metadata.length && metadata.length != 0 {
                416
            } else if head || metadata.length == 0 {
                200
            } else {
                206
            }
        });
        let length = if head {
            metadata.length
        } else {
            metadata.length.saturating_sub(first).min(PAGE_BYTES)
        };
        let mut response = format!("HTTP/1.1 {status} Fixture\r\nConnection: close\r\n");
        let body = if status >= 400 {
            response.push_str("Content-Length: 0\r\n\r\n");
            vec![]
        } else {
            response.push_str(&format!(
                "Content-Length: {length}\r\nETag: {}\r\nRacer-Expires-At: {}\r\n",
                std::str::from_utf8(metadata.version.etag.as_bytes()).unwrap(),
                metadata
                    .expires_at
                    .as_system_time()
                    .duration_since(UNIX_EPOCH)
                    .unwrap()
                    .as_millis()
            ));
            if let Some(content_type) = &metadata.content_type {
                response.push_str(&format!(
                    "Racer-Content-Type: {}\r\n",
                    content_type.as_str()
                ));
            }
            if !head {
                response.push_str("Content-Type: application/octet-stream\r\n");
            }
            if !head && length != 0 {
                response.push_str(&format!(
                    "Content-Range: bytes {first}-{}/{}\r\n",
                    first + length - 1,
                    metadata.length
                ));
            }
            response.push_str("\r\n");
            if head {
                vec![]
            } else if let Some(body) = &state.body {
                body[first as usize..(first + length) as usize].to_vec()
            } else if metadata.length == 3 {
                b"abc".to_vec()
            } else {
                vec![call.page as u8; length as usize]
            }
        };
        drop(state);
        if stream.write_all(response.as_bytes()).is_ok() {
            let _ = stream.write_all(&body);
        }
    }
}

/// Minimal real control-plane state for storage and flight fixtures. Callers with
/// rotating keys or publications should share their own Availability instead.
pub fn availability() -> std::rc::Rc<crate::control::Availability> {
    availability_for(vec![crate::model::CacheId(
        crate::security::test_support::CACHE.into(),
    )])
}
pub fn availability_for(
    caches: Vec<crate::model::CacheId>,
) -> std::rc::Rc<crate::control::Availability> {
    crate::control::for_caches(
        std::rc::Rc::new(crate::security::test_support::keys_for(&caches)),
        caches,
    )
}

pub struct NoPeers;

impl NoPeers {
    pub fn requester() -> std::rc::Rc<crate::peer::Requester> {
        crate::peer::Requester::scripted(
            std::rc::Rc::new(Self),
            Self::direct_hedge_available,
            Self::request,
            Self::request_direct,
        )
    }
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &crate::model::NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _: crate::peer::protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a crate::runtime::RequestScope,
    ) -> crate::error::Operation<'a, crate::peer::forwarding::VerifiedResponse> {
        panic!("local origin scenario must not hedge to a peer")
    }
    fn request<'a>(
        &'a self,
        _: crate::peer::protocol::PeerRequest,
        _: std::sync::Arc<crate::topology::Membership>,
        _: &'a crate::runtime::RequestScope,
    ) -> crate::error::Operation<'a, crate::peer::forwarding::VerifiedResponse> {
        Box::pin(async { panic!("local origin scenario must not contact peers") })
    }
}

/// Side-effect-free configuration for assembled worker scenarios.
pub mod cluster {
    use crate::config::Config;
    use crate::config::Limits;
    use crate::model::ClusterId;
    use crate::model::NodeId;
    use std::num::NonZeroU32;
    use std::num::NonZeroUsize;
    use std::time::Duration;

    pub fn config(enable_rdma: bool) -> Config {
        let count = NonZeroUsize::new(16).unwrap();
        let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
        Config {
            send_crc_pair: None,
            page_hedge: Default::default(),
            peer_admission: Default::default(),
            shares: NonZeroU32::new(4).unwrap(),
            disk_page_entries: NonZeroUsize::new(65536).unwrap(),
            checkpoint_bytes: NonZeroUsize::new(64 * 1024 * 1024).unwrap(),
            cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
            node: NodeId("00000000-0000-4000-8000-000000000002".into()),
            max_threads: 2,
            allow_smt: false,
            opaque_relay: false,
            peer_tcp_nodelay: false,
            enable_rdma,
            control_endpoint: "https://control.invalid".into(),
            peer_listen: "127.0.0.1:0".parse().unwrap(),
            diagnostics_listen: "127.0.0.1:0".parse().unwrap(),
            trust_bundle: "unused/ca".into(),
            service_account_token: "unused/token".into(),
            identity_directory: "unused/identity".into(),
            slab_directory: "unused/slabs".into(),
            slab_bytes: 1024 * 1024 * 1024,
            segment_bytes: 64 * 1024 * 1024,
            free_segment_reserve: 2,
            origin_connections_per_cache: NonZeroUsize::new(8).unwrap(),
            request_timeout: Duration::from_secs(30),
            peer_attempt_timeout: Duration::from_secs(30),
            reader_stall_timeout: Duration::from_secs(10),
            shutdown_timeout: Duration::from_secs(30),
            limits: Limits {
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
            },
        }
    }
}

#[derive(Default)]
pub struct WakeCounter(std::sync::atomic::AtomicUsize);

impl WakeCounter {
    pub fn count(&self) -> usize {
        self.0.load(std::sync::atomic::Ordering::SeqCst)
    }
}

impl std::task::Wake for WakeCounter {
    fn wake(self: std::sync::Arc<Self>) {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
}

// Real read ownership shared by read and client scenarios. Only the UDS adapter is scripted.

pub(crate) struct ReadWorker {
    pub coordinator: Rc<Coordinator>,
    pub streams: Rc<RangeStreams>,
    pub membership: std::sync::Arc<crate::topology::Membership>,
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
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        delivery: Rc<Delivery>,
        window: usize,
    ) -> Self {
        let origin = AdapterOrigin::new(&cache.name, metadata);
        let keys = Rc::new(crate::security::test_support::keys());
        let publications = Arc::new(PublishedState::default());
        let availability = Rc::new(crate::control::Availability::new(
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
                members: vec![racer_control_wire::Member {
                    node: keys.node().clone(),
                    shares: NonZeroU32::new(1).unwrap(),
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
        let buffers = BufferPool::new(admission.clone());
        let index = Rc::new(Index::new(WorkerId(0), 16, availability.clone()));
        let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
        // Reads use an empty disk index. No slabs need to be opened or written.
        let slabs = Rc::new(page_alloc::Slab::new(
            origin.root.join("slabs/worker-0-slab-0.dat"),
            256 * 1024 * 1024,
            64 * 1024 * 1024,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
        ));
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
        let (port, engine) = crypto::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(16).unwrap());
        let crypto = Rc::new(CryptoClient::new(port));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let candidates = Rc::new(CandidatePolicy::new(
            keys.node().clone(),
            Rc::new(Placement::new(16)),
            crate::test_support::NoPeers::requester(),
            credentials.clone(),
            Arc::new(Default::default()),
        ));
        let client = origin.client(
            snapshots.clone(),
            admission.clone(),
            reactor,
            buffers.clone(),
        );
        let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
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
            drivers: Rc::new(DriverQueue::new(1024)),
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
        uring_runtime::group::Service::poll_budgeted(
            &mut *self.engine.borrow_mut(),
            &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )
        .unwrap();
        self.crypto.poll_budgeted(64).unwrap();
        // Exercise acquisition/authentication, not persistence or cache retention.
        // Delivered leases remain charged even after the cache drops its copy.
        self.writer.discard_unsubmitted();
        self.memory.remove_cache(&self.cache).unwrap();
    }
}
