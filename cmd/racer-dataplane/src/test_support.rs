//! Deterministic test-only seams; no fake implementation is linked into production.

use crate::admission::AdmissionPolicy;
use crate::control::Snapshot;
use crate::control::publication::PublicationTarget;
use crate::http::Delivery;
use crate::memory::BufferPool;
use crate::memory::MemoryCache;
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
use crate::security::CredentialCrypto;
use crate::security::CryptoClient;
use crate::security::PageCrypto;
use crate::security::PageCryptoEngine;
use crate::store::StoreReader;
use crate::store::StoreWriter;
use crate::store::catalog::Index;
use crate::store::catalog::SegmentClock;
use crate::test_support::origin::AdapterOrigin;
use crate::topology::Placement;
use crate::worker::CryptoRuntime;
use controlplane::Published;
use racer_control_wire::CacheDefinition;
use racer_control_wire::MembershipVersion;
use racer_control_wire::Publication;
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
    use uring_runtime::environment::Deadline;

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
        use crate::model::CacheKey;
        use crate::model::CurrentVersion;
        use crate::model::ExpiresAt;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::StrongEtag;
        use crate::model::VersionMetadata;
        use racer_control_wire::CacheId;
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
    use crate::control::publication::PublicationTarget;
    use crate::http::Codec;
    use crate::memory::BufferPool;
    use crate::model::ObjectMetadata;
    use crate::model::PAGE_BYTES;
    use crate::origin::OriginClient;
    use crate::runtime::Reactor;
    pub use racer_object_wire::test_util::RequestKind;
    use std::rc::Rc;

    /// Application client factory around the portable protocol fixture.
    pub struct AdapterOrigin(racer_object_wire::test_util::AdapterOrigin);

    impl std::ops::Deref for AdapterOrigin {
        type Target = racer_object_wire::test_util::AdapterOrigin;

        fn deref(&self) -> &Self::Target {
            &self.0
        }
    }

    impl AdapterOrigin {
        /// With no explicit body, three-byte objects contain `abc`; other pages
        /// contain their page number repeated. Large fixtures need no object allocation.
        pub fn new(cache_name: &str, metadata: ObjectMetadata) -> Self {
            Self(racer_object_wire::test_util::AdapterOrigin::new(
                cache_name, metadata,
            ))
        }

        pub fn client(
            &self,
            snapshots: Rc<PublicationTarget>,
            admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
            reactor: Rc<Reactor>,
            buffers: BufferPool,
        ) -> Rc<OriginClient> {
            Rc::new(
                OriginClient::new(
                    snapshots.published.clone(),
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
    }
}

/// Minimal real control-plane state for storage and flight fixtures. Callers with
/// rotating keys or publications should share their own Availability instead.
pub fn availability() -> std::rc::Rc<crate::control::Availability> {
    availability_for(vec![racer_control_wire::CacheId(
        crate::test_support::security::CACHE.into(),
    )])
}
pub fn availability_for(
    caches: Vec<racer_control_wire::CacheId>,
) -> std::rc::Rc<crate::control::Availability> {
    crate::control::for_caches(
        std::rc::Rc::new(crate::test_support::security::keys_for(&caches)),
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
        _: &racer_control_wire::NodeId,
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
    use racer_control_wire::ClusterId;
    use racer_control_wire::NodeId;
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
            peer_receive: Default::default(),
            shares: NonZeroU32::new(4).unwrap(),
            disk_page_entries: NonZeroUsize::new(65536).unwrap(),
            admission_mode: crate::config::AdmissionMode::SecondSight,
            admission_history_bytes: NonZeroUsize::new(4 * 1024 * 1024).unwrap(),
            admission_period: Duration::from_secs(60),
            checkpoint_bytes: NonZeroUsize::new(64 * 1024 * 1024).unwrap(),
            device_directory: "/host/dev".into(),
            cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
            node: NodeId("00000000-0000-4000-8000-000000000002".into()),
            max_threads: 2,
            allow_smt: false,
            opaque_relay: false,
            peer_tcp_nodelay: false,
            pprof_enabled: false,
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
                placement_cache_bytes: NonZeroUsize::new(16 * crate::topology::RANKING_BYTES)
                    .unwrap(),
                path_cache_bytes: NonZeroUsize::new(8 * 1024 * 1024).unwrap(),
                active_path_searches: NonZeroUsize::new(8).unwrap(),
                cached_paths: count,
                retained_snapshots: count,
                metadata_entries: count,
                relay_transfers: count,
            },
        }
    }
}

pub use uring_runtime::test_util::WakeCounter;

/// Assemble the real worker without activating I/O for wake and quota scenarios.
pub(crate) fn wake_test_worker() -> crate::app::WorkerApplication {
    let config = cluster::config(false);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, _engine) = crate::security::pair(WorkerId(0), 0, config.limits.queue_entries);
    crate::app::WorkerApplication::assemble(
        &config,
        Arc::new(crate::app::NodeState::default()),
        WorkerId(0),
        crate::worker::WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
            admission,
            crypto: Rc::new(CryptoClient::new(io)),
        },
        Vec::new(),
    )
    .unwrap()
}

pub(crate) fn wake_test_coordinator() -> Rc<Coordinator> {
    wake_test_worker().into_test_coordinator()
}

/// Projected-volume fixture composed from the production secure file operations.
pub(crate) async fn projected_file(
    r: &Reactor,
    path: &std::path::Path,
    file: &str,
    limit: usize,
    scope: &crate::runtime::RequestScope,
) -> crate::error::Result<uring_runtime::reactor::filesystem::ReadBuffer> {
    use uring_runtime::reactor::filesystem::secure::{BENEATH, NO_MAGICLINKS};

    let dir =
        uring_runtime::reactor::filesystem::secure::directory(r, path, false, false, scope).await?;
    // One openat2 resolves ..data and pins the target directory across rotation.
    // BENEATH rejects absolute/escaping links; NO_MAGICLINKS rejects proc escapes.
    let generation = r
        .file_open(
            Some(dir),
            std::ffi::CString::new("..data").unwrap(),
            libc::O_RDONLY | libc::O_DIRECTORY,
            BENEATH | NO_MAGICLINKS,
            scope,
        )
        .await?;
    uring_runtime::reactor::filesystem::secure::read_at(r, &generation, file, limit, false, scope)
        .await
}

/// Enrollment I/O and certificate fixtures shared by control and application tests.
pub(crate) mod enrollment {
    use crate as dataplane;
    use std::path::PathBuf;
    use std::sync::atomic::AtomicU64;
    use std::sync::atomic::Ordering;
    #[allow(dead_code)]
    mod io {
        include!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/support/enrollment.rs"
        ));
    }
    pub(crate) use io::ca;
    pub(crate) use io::drive;
    pub(crate) use io::issue;
    pub(crate) use io::issue_at;
    pub(crate) use io::signing_identity;
    pub(crate) struct Directory(pub PathBuf);
    impl Directory {
        pub fn new() -> Self {
            static NEXT: AtomicU64 = AtomicU64::new(0);
            let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("target/control-tests")
                .join(format!(
                    "{}-{}",
                    std::process::id(),
                    NEXT.fetch_add(1, Ordering::Relaxed)
                ));
            std::fs::create_dir_all(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Directory {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }
    pub(crate) fn scope() -> crate::runtime::RequestScope {
        crate::runtime::RequestScope::new(
            crate::model::RequestId([7; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap()
    }
    pub(crate) fn reactor() -> Option<std::rc::Rc<crate::runtime::Reactor>> {
        match io_uring::IoUring::new(2) {
            Ok(ring) => drop(ring),
            Err(e)
                if matches!(
                    e.raw_os_error(),
                    Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
                ) =>
            {
                assert_ne!(
                    std::env::var("RUNTIME_REQUIRE_IO_URING").as_deref(),
                    Ok("1"),
                    "RUNTIME_REQUIRE_IO_URING=1 but control io_uring unavailable: {e}"
                );
                eprintln!("control io_uring unavailable: {e}");
                return None;
            }
            Err(e) => panic!("io_uring setup: {e}"),
        }
        Some(io::reactor())
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
    cache: racer_control_wire::CacheId,
}

/// Publish a topology-only fixture through the real generic publication authority.
pub(crate) fn published_membership(
    membership: Arc<crate::topology::Membership>,
) -> Arc<Published<Snapshot>> {
    let published = Arc::new(Published::new(Snapshot::retention(2)));
    published
        .publish(
            Arc::new(Snapshot::membership_fixture(membership)),
            uring_runtime::environment::now(),
            |_, _| Ok::<_, crate::error::Error>(()),
            || (),
        )
        .unwrap();
    published
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
        let keys = Rc::new(crate::test_support::security::keys());
        let publications = Arc::new(Published::new(Snapshot::retention(2)));
        let availability = Rc::new(crate::control::Availability::new(
            publications.clone(),
            keys.clone(),
        ));
        let snapshots = Rc::new(PublicationTarget::new(keys.cluster().clone(), publications));
        snapshots
            .apply(Publication {
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
        let (port, engine) =
            crate::security::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(16).unwrap());
        let crypto = Rc::new(CryptoClient::new(port));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let candidates = Rc::new(CandidatePolicy::new(
            keys.node().clone(),
            Rc::new(Placement::new(16)),
            crate::test_support::NoPeers::requester(),
            credentials.clone(),
            Arc::new(Published::new(Snapshot::retention(2))),
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
            snapshots.published.clone(),
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
pub(crate) mod security {
    //! Shared security fixtures. Never linked into production.
    use crate::http::Codec;
    use crate::peer::protocol;
    use crate::peer::protocol::Signatures;
    use crate::peer::protocol::SignedHead;
    use racer_control_wire::BundleGeneration;
    use racer_control_wire::CacheEncryptionKey;
    use racer_control_wire::CacheId;
    use racer_control_wire::CacheKeyPurpose;
    use racer_control_wire::CacheKeyRef;
    use racer_control_wire::CacheKeyState;
    use racer_control_wire::ClusterId;
    use racer_control_wire::KeyringBundle;
    use racer_control_wire::NodeId;
    use racer_control_wire::SCHEMA_VERSION;
    use racer_crypto::identity::Certificates;
    use racer_crypto::identity::KeyEpochs;
    use racer_crypto::identity::KeyLease;
    use racer_crypto::identity::Keyring;
    use racer_crypto::identity::PendingIdentity;
    use std::rc::Rc;
    use std::sync::Arc;

    /// Shared Ed25519 CA and customizable node-certificate fixtures.
    pub(crate) use racer_crypto::identity::test_util::{ca, issue};

    pub struct Identity {
        pub keys: Rc<Keyring>,
        pub certificates: Rc<Certificates>,
        pub signatures: Rc<Signatures>,
    }
    pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
    pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";
    pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";
    pub(crate) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        let (ca, ca_key) = ca();
        let (pending, chain) = issue(
            &ca,
            &ca_key,
            &ClusterId(CLUSTER.into()),
            &NodeId(NODE.into()),
            |_| {},
        );
        (pending, chain, vec![ca.der().to_vec()])
    }
    pub(crate) fn keys() -> Keyring {
        keys_for(&[CacheId(CACHE.into())])
    }
    pub(crate) fn assert_page_key(lease: &KeyLease, expected: &[u8; 32]) {
        let mut sealed = [0; 19];
        lease
            .seal_page(lease.cache(), &[1; 24], b"retained", b"abc", &mut sealed)
            .unwrap();
        let mut opened = [0; 3];
        racer_crypto::open(expected, &[1; 24], b"retained", &sealed, &mut opened).unwrap();
        assert_eq!(&opened, b"abc");
    }
    pub(crate) fn keys_for(caches: &[CacheId]) -> Keyring {
        let (_, _, roots) = issued();
        let keys = Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        );
        let mut initial = rotation_bundle(1, roots);
        // keys() historically used the unrotated seed bundle; rotation_bundle(1)
        // intentionally has different suffixes and material just like later epochs.
        for (i, record) in initial.cache_keys.iter_mut().enumerate() {
            record.key.id = crate::model::key_id_from_generation(1, i as u32 + 1).unwrap();
            *record = CacheEncryptionKey::new(
                record.key.clone(),
                record.state,
                zeroize::Zeroizing::new([7 + i as u8; 32]),
            );
        }
        let templates = std::mem::take(&mut initial.cache_keys);
        for (i, cache) in caches.iter().enumerate() {
            for template in &templates {
                let (mut key, state, mut material) = template.clone().into_installation();
                key.cache = cache.clone();
                if i != 0 {
                    material[..8].copy_from_slice(&(i as u64).to_be_bytes());
                }
                initial
                    .cache_keys
                    .push(CacheEncryptionKey::new(key, state, material));
            }
        }
        keys.install(initial).unwrap();
        keys
    }
    pub(crate) fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
        KeyringBundle {
            schema_version: SCHEMA_VERSION,
            cluster: ClusterId(CLUSTER.into()),
            generation: BundleGeneration(generation),
            peer_trust_roots: roots,
            cache_keys: [CacheKeyPurpose::Page, CacheKeyPurpose::OriginCredentials]
                .into_iter()
                .enumerate()
                .map(|(i, purpose)| {
                    let mut material = zeroize::Zeroizing::new([7 + i as u8; 32]);
                    material[..8].copy_from_slice(&generation.to_be_bytes());
                    CacheEncryptionKey::new(
                        CacheKeyRef {
                            cache: CacheId(CACHE.into()),
                            id: crate::model::key_id_from_generation(generation, i as u32).unwrap(),
                            purpose,
                        },
                        CacheKeyState::Active,
                        material,
                    )
                })
                .collect(),
        }
    }
    pub(crate) fn node(n: usize) -> NodeId {
        NodeId(format!("{n:08x}-1111-4111-8111-111111111111"))
    }
    pub(crate) fn network(count: usize) -> Vec<Rc<Signatures>> {
        identities(
            ClusterId(node(99).0),
            &(0..count).map(node).collect::<Vec<_>>(),
            mac_test_keys,
        )
        .into_iter()
        .map(|identity| identity.signatures)
        .collect()
    }
    pub(crate) fn mac_test_keys() -> Vec<CacheEncryptionKey> {
        [node(88).0, CACHE.into()]
            .into_iter()
            .enumerate()
            .map(|(i, cache)| {
                CacheEncryptionKey::new(
                    CacheKeyRef {
                        cache: CacheId(cache),
                        id: crate::model::key_id_from_generation(1, 100 + i as u32).unwrap(),
                        purpose: CacheKeyPurpose::OriginCredentials,
                    },
                    CacheKeyState::Active,
                    zeroize::Zeroizing::new([100 + i as u8; 32]),
                )
            })
            .collect()
    }
    pub(crate) fn mac_test_key(cache: &str) -> Vec<CacheEncryptionKey> {
        let mut keys = mac_test_keys();
        keys.truncate(1);
        keys[0].key.cache = CacheId(cache.into());
        keys
    }
    pub(crate) fn clone_head(head: &SignedHead) -> SignedHead {
        let codec = Codec::new(protocol::MAX_HEAD);
        let encoded = codec.encode_head(&head.head).unwrap();
        SignedHead {
            head: codec.decode_head(&encoded).unwrap().unwrap().0,
            signature: head.signature.clone(),
        }
    }
    pub fn identities(
        cluster: ClusterId,
        nodes: &[NodeId],
        cache_keys: impl Fn() -> Vec<CacheEncryptionKey>,
    ) -> Vec<Identity> {
        let (ca, ca_key) = ca();
        let roots = vec![ca.der().to_vec()];
        nodes
            .iter()
            .map(|node| {
                let (pending, chain) = issue(&ca, &ca_key, &cluster, node, |_| {});
                let identity = pending
                    .accept(cluster.clone(), node.clone(), chain, &roots)
                    .unwrap();
                let keys = Rc::new(Keyring::new(
                    cluster.clone(),
                    node.clone(),
                    Arc::new(KeyEpochs::default()),
                ));
                keys.install(KeyringBundle {
                    schema_version: SCHEMA_VERSION,
                    cluster: cluster.clone(),
                    generation: BundleGeneration(1),
                    peer_trust_roots: roots.clone(),
                    cache_keys: cache_keys(),
                })
                .unwrap();
                keys.install_signing_identity(Arc::new(identity)).unwrap();
                let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
                let signatures = Rc::new(Signatures::new(keys.clone(), certificates.clone()));
                Identity {
                    keys,
                    certificates,
                    signatures,
                }
            })
            .collect()
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        use racer_crypto::identity::KeyPurpose;
        #[test]
        fn complete_component_error_mapping_preserves_application_meanings() {
            use crate::error::Error as App;
            use racer_crypto::identity::Error as Identity;
            for (component, application) in [
                (Identity::InvalidRequest, App::InvalidRequest),
                (Identity::InvalidConfiguration, App::InvalidConfiguration),
                (Identity::Unauthorized, App::Unauthorized),
                (Identity::Unavailable, App::Unavailable),
                (Identity::MissingKey, App::MissingKey),
                (Identity::CorruptRecord, App::CorruptRecord),
            ] {
                assert_eq!(App::from(component), application);
            }
        }
        #[test]
        fn request_key_purpose_separation() {
            let keys = keys();
            let cache = CacheId(CACHE.into());
            let mut tag = [0; 32];
            assert!(
                keys.active(&cache, KeyPurpose::Page)
                    .unwrap()
                    .request_mac(&cache, b"request", &mut tag)
                    .is_err()
            );
            let credential = keys.active(&cache, KeyPurpose::OriginCredentials).unwrap();
            credential
                .request_mac(&cache, b"request", &mut tag)
                .unwrap();
            credential
                .verify_request_mac(&cache, credential.id(), b"request", &tag)
                .unwrap();
            assert!(
                credential
                    .verify_request_mac(&cache, credential.id(), b"changed", &tag)
                    .is_err()
            );
        }
    }
}
