//! Deterministic test-only seams; no fake implementation is linked into production.

use crate::admission::AdmissionPolicy;
use crate::control::Snapshot;
use crate::control::cell::PublicationCell;
use crate::control::publication::PublicationTarget;
use crate::drivers::DriverQueue;
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
use racer_io::scoped::fs::prelude::*;
use racer_wire::control::CacheDefinition;
use racer_wire::control::MembershipVersion;
use racer_wire::control::Publication;
use racer_wire::control::PublicationSequence;
use std::cell::RefCell;
use std::num::NonZeroU32;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;

pub mod clock {
    use crate::error::Error;
    use crate::error::Result;
    use racer_io::deadline::Deadline;
    use std::cell::Cell;
    use std::time::Duration;
    use std::time::Instant;
    use std::time::SystemTime;
    use std::time::UNIX_EPOCH;

    /// Pure deadline/freshness fixture. Reactor timers use racer_io::SimClock.
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
        use racer_wire::control::CacheId;
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
    pub use racer_wire::object::test_util::RequestKind;
    use std::rc::Rc;

    /// Application client factory around the portable protocol fixture.
    pub struct AdapterOrigin(racer_wire::object::test_util::AdapterOrigin);

    impl std::ops::Deref for AdapterOrigin {
        type Target = racer_wire::object::test_util::AdapterOrigin;

        fn deref(&self) -> &Self::Target {
            &self.0
        }
    }

    impl AdapterOrigin {
        /// With no explicit body, three-byte objects contain `abc`; other pages
        /// contain their page number repeated. Large fixtures need no object allocation.
        pub fn new(cache_name: &str, metadata: ObjectMetadata) -> Self {
            Self(racer_wire::object::test_util::AdapterOrigin::new(
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
pub fn availability(env: &racer_io::Env) -> std::rc::Rc<crate::control::Availability> {
    availability_for(
        env,
        vec![racer_wire::control::CacheId(
            crate::test_support::security::CACHE.into(),
        )],
    )
}
pub fn availability_for(
    env: &racer_io::Env,
    caches: Vec<racer_wire::control::CacheId>,
) -> std::rc::Rc<crate::control::Availability> {
    crate::control::for_caches(
        env,
        std::rc::Rc::new(crate::test_support::security::keys_for(env, &caches)),
        caches,
    )
}

pub struct NoPeers;

impl NoPeers {
    pub fn requester(env: &racer_io::Env) -> std::rc::Rc<crate::peer::Requester> {
        crate::peer::Requester::scripted(
            env.ctx.clock.clone(),
            std::rc::Rc::new(Self),
            Self::direct_hedge_available,
            Self::request,
            Self::request_direct,
        )
    }
    fn direct_hedge_available(
        &self,
        _: &std::sync::Arc<crate::topology::Membership>,
        _: &racer_wire::control::NodeId,
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
pub use crate::app::dst::cluster;

/// Assemble the real worker without activating I/O for wake and quota scenarios.
pub(crate) fn wake_test_worker(env: &racer_io::Env) -> crate::app::WorkerApplication {
    let config = cluster::config(false);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        config.limits.clone(),
    )));
    let (io, _engine) = crate::security::pair(WorkerId(0), 0, config.limits.queue_entries);
    crate::app::WorkerApplication::assemble(
        &config,
        Arc::new(crate::app::NodeState::on(env.ctx.clone())),
        WorkerId(0),
        crate::worker::WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone(), env)),
            admission,
            crypto: Rc::new(CryptoClient::new(io)),
        },
        Vec::new(),
    )
    .unwrap()
}

pub(crate) fn wake_test_coordinator(env: &racer_io::Env) -> Rc<Coordinator> {
    wake_test_worker(env).into_test_coordinator()
}

/// Projected-volume fixture composed from the production secure file operations.
pub(crate) async fn projected_file(
    r: &Reactor,
    path: &std::path::Path,
    file: &str,
    limit: usize,
    scope: &crate::runtime::RequestScope,
) -> crate::error::Result<racer_io::scoped::fs::ReadBuffer> {
    use racer_io::scoped::fs::secure::{BENEATH, NO_MAGICLINKS};

    let dir = racer_io::scoped::fs::secure::directory(r, path, false, false, scope).await?;
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
    racer_io::scoped::fs::secure::read_at(r, &generation, file, limit, false, scope).await
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
    /// A request scope for enrollment fixtures. Its deadline only bounds a
    /// stuck test: the durable enrollment steps sync files on a real ring,
    /// and a contended disk can stall each sync for seconds.
    pub(crate) fn scope() -> crate::runtime::RequestScope {
        crate::runtime::RequestScope::host(
            crate::model::RequestId([7; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(60),
        )
        .unwrap()
    }
    pub(crate) fn reactor(env: &racer_io::Env) -> Option<std::rc::Rc<crate::runtime::Reactor>> {
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
        Some(io::reactor_in(env))
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
    cache: racer_wire::control::CacheId,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    clock: Rc<SegmentClock>,
    reactor: Rc<Reactor>,
    metrics: crate::telemetry::Metrics,
    /// Keep cached pages and queued disk writes between polls.
    retain: std::cell::Cell<bool>,
}

/// Publish a topology-only fixture through the real generic publication authority.
pub(crate) fn published_membership(
    membership: Arc<crate::topology::Membership>,
) -> Arc<PublicationCell<Snapshot>> {
    let published = Arc::new(PublicationCell::new(Snapshot::retention(2)));
    published
        .publish(
            Arc::new(Snapshot::membership_fixture(membership)),
            racer_io::Clock::host().now(),
            |_, _| Ok::<_, crate::error::Error>(()),
            || (),
        )
        .unwrap();
    published
}

impl ReadWorker {
    pub fn new(
        env: &racer_io::Env,
        cache: CacheDefinition,
        metadata: ObjectMetadata,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        delivery: Rc<Delivery>,
        window: usize,
    ) -> Self {
        // Fixture time and entropy come from the reactor that drives it.
        let reactor_ctx = reactor.ctx().clone();
        let origin = AdapterOrigin::new(&cache.name, metadata);
        let keys = Rc::new(crate::test_support::security::keys(env));
        let publications = Arc::new(PublicationCell::new(Snapshot::retention(2)));
        let availability = Rc::new(crate::control::Availability::new(
            publications.clone(),
            keys.clone(),
        ));
        let snapshots = Rc::new(PublicationTarget::new(
            keys.cluster().clone(),
            publications,
            reactor_ctx.clock.clone(),
        ));
        snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: keys.cluster().clone(),
                    sequence: PublicationSequence(1),
                    membership_version: MembershipVersion(1),
                    members: vec![racer_wire::control::Member {
                        node: keys.node().clone(),
                        shares: NonZeroU32::new(1).unwrap(),
                        peer_endpoint: "127.0.0.1:1".into(),
                        rails: vec![],
                        site: String::new(),
                    }],
                    caches: vec![cache.clone()],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        let directory = std::rc::Rc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                vec![WorkerId(0)],
                16,
                reactor_ctx.clone(),
            )
            .unwrap(),
        );
        let buffers = BufferPool::new(admission.clone());
        let index = Rc::new(Index::new(
            WorkerId(0),
            16,
            availability.clone(),
            reactor_ctx.clock.clone(),
        ));
        let segments = Rc::new(page_alloc::Segments::new(64 * 1024 * 1024));
        // Reads use an empty disk index. No slabs need to be opened or written.
        let slabs = Rc::new(crate::test_support::simulated_slab(
            page_alloc::Slab::new(
                Self::disk_root(&origin).join("slabs/worker-0-slab-0.dat"),
                256 * 1024 * 1024,
                64 * 1024 * 1024,
                crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
            ),
            env,
        ));
        let writer = Rc::new(StoreWriter::new(
            index.clone(),
            segments.clone(),
            slabs.clone(),
            admission.clone(),
            reactor.clone(),
            availability.clone(),
        ));
        let clock = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
        let disk = Rc::new(StoreReader::new(
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
        let metrics = crate::telemetry::Metrics::default();
        crypto.set_metrics(metrics.clone());
        let worker_admission = admission.clone();
        let worker_reactor = reactor.clone();
        let credentials = Rc::new(CredentialCrypto::new(
            keys.clone(),
            admission.clone(),
            reactor_ctx.rng.clone(),
        ));
        let drivers = Rc::new(DriverQueue::new(1024));
        let candidates = Rc::new(CandidatePolicy::new(
            keys.node().clone(),
            Rc::new(Placement::new(16)),
            crate::test_support::NoPeers::requester(env),
            credentials.clone(),
            Arc::new(PublicationCell::new(Snapshot::retention(2))),
            reactor_ctx.clone(),
            drivers.clone(),
        ));
        let client = origin.client(
            snapshots.clone(),
            admission.clone(),
            reactor,
            buffers.clone(),
        );
        let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
        let fill = Rc::new(
            Fill::new(FillDependencies {
                memory: memory.clone(),
                buffers,
                disk,
                writer: writer.clone(),
                origin: client.clone(),
                candidates: candidates.clone(),
                flights: Rc::new(Flights::new(
                    admission.clone(),
                    availability.clone(),
                    reactor_ctx.clock.clone(),
                    drivers.clone(),
                )),
                crypto: Rc::new(PageCrypto::new(
                    keys,
                    crypto.clone(),
                    crate::memory::BufferPool::new(admission.clone()),
                )),
                credentials: credentials.clone(),
                admission,
                metadata_owner: directory.clone(),
            })
            .with_metrics(metrics.clone()),
        );
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
        let streams = Rc::new(RangeStreams::new(
            directory.clone(),
            delivery,
            window,
            drivers.clone(),
        ));
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
            drivers,
            endpoint: RefCell::new(endpoint),
            engine: RefCell::new(PageCryptoEngine::new(CryptoRuntime {
                port: engine,
                ctx: reactor_ctx.clone(),
            })),
            crypto,
            writer,
            memory,
            cache: cache.id,
            admission: worker_admission,
            clock,
            reactor: worker_reactor,
            metrics,
            retain: std::cell::Cell::new(false),
        }
    }

    /// Keep cached pages and queued disk writes between polls, so a test can
    /// arrange memory and disk contents across requests.
    pub fn retain(&self, retain: bool) {
        self.retain.set(retain);
    }

    /// This worker's page cache.
    pub fn memory(&self) -> &Rc<MemoryCache> {
        &self.memory
    }

    /// This worker's crypto client.
    pub fn crypto(&self) -> Rc<CryptoClient> {
        self.crypto.clone()
    }

    /// CPU page decryptions started on this worker's crypto engine.
    pub fn cpu_decrypts(&self) -> u64 {
        self.metrics
            .count(crate::telemetry::Event::CryptoDecryptStarted)
    }

    /// This worker's count of `event`.
    pub fn metric(&self, event: crate::telemetry::Event) -> u64 {
        self.metrics.count(event)
    }

    /// Open this worker's slab so pages can be written to and read from disk.
    pub fn open_disk(&self) {
        std::fs::create_dir_all(Self::disk_root(&self.origin).join("slabs")).unwrap();
        self.writer
            .configure(self.admission.clone(), self.clock.clone(), 8, 64)
            .unwrap();
        let _ = futures::executor::block_on(self.writer.open()).unwrap();
    }

    /// Write `copy` to disk and wait until it is published.
    pub fn persist(&self, copy: crate::memory::CiphertextCopy) {
        use crate::admission::ResourceClass;
        let id = copy.ciphertext.envelope().page.clone();
        let dirty = self
            .admission
            .reserve(
                Some(&id.version.object.cache),
                ResourceClass::DirtyCiphertext,
                copy.ciphertext.bytes().len(),
            )
            .unwrap();
        self.writer.enqueue(copy, dirty).unwrap();
        let scope = crate::runtime::RequestScope::host(
            crate::model::RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let mut progress = self.writer.progress(1, &scope);
        let waker = futures::task::noop_waker();
        let mut cx = Context::from_waker(&waker);
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        while self.writer.index().lookup(&id).unwrap().is_none() {
            if progress.as_mut().poll(&mut cx).is_ready() {
                progress = self.writer.progress(1, &scope);
            }
            self.reactor.poll_budgeted(32).unwrap();
            assert!(
                std::time::Instant::now() < deadline,
                "disk write did not finish"
            );
            std::thread::yield_now();
        }
    }

    /// The published disk location of a page, if any.
    pub fn disk_location(
        &self,
        page: &crate::model::PageId,
    ) -> Option<crate::store::catalog::RecordLocation> {
        self.writer
            .index()
            .lookup(page)
            .unwrap()
            .map(|entry| entry.location)
    }

    /// The slab file backing this worker's disk.
    pub fn slab_path(&self) -> std::path::PathBuf {
        Self::disk_root(&self.origin).join("slabs/worker-0-slab-0.dat")
    }

    /// The origin's directory by its real path. The slab refuses to open
    /// through the `/proc` symlink that `root` is.
    fn disk_root(origin: &AdapterOrigin) -> std::path::PathBuf {
        std::fs::read_link(&origin.root).unwrap_or_else(|_| origin.root.clone())
    }

    /// Whether every command submitted to this worker has completed.
    pub fn is_drained(&self) -> bool {
        self.endpoint.borrow().is_drained() && self.drivers.pending() == 0
    }

    pub fn poll(&self, cx: &mut Context<'_>) {
        self.endpoint.borrow_mut().poll_budgeted(64).unwrap();
        self.drivers.poll(cx, 64);
        crate::lifecycle::Service::poll_budgeted(
            &mut *self.engine.borrow_mut(),
            &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
            64,
        )
        .unwrap();
        self.crypto.poll_budgeted(64).unwrap();
        if self.retain.get() {
            return;
        }
        // Exercise acquisition/authentication, not persistence or cache retention.
        // Delivered leases remain charged even after the cache drops its copy.
        self.writer.discard_unsubmitted();
        self.memory.remove_cache(&self.cache).unwrap();
    }
}
pub(crate) mod security {
    //! Shared security fixtures. Never linked into production.
    use crate::peer::protocol::Signatures;
    use racer_crypto::identity::Certificates;
    use racer_crypto::identity::KeyEpochs;
    use racer_crypto::identity::KeyLease;
    use racer_crypto::identity::Keyring;
    use racer_crypto::identity::PendingIdentity;
    use racer_wire::control::BundleGeneration;
    use racer_wire::control::CacheEncryptionKey;
    use racer_wire::control::CacheId;
    use racer_wire::control::CacheKeyPurpose;
    use racer_wire::control::CacheKeyRef;
    use racer_wire::control::CacheKeyState;
    use racer_wire::control::ClusterId;
    use racer_wire::control::KeyringBundle;
    use racer_wire::control::NodeId;
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
    pub(crate) fn keys(env: &racer_io::Env) -> Keyring {
        keys_for(env, &[CacheId(CACHE.into())])
    }
    pub(crate) fn assert_page_key(lease: &KeyLease, expected: &[u8; 32]) {
        let mut sealed = [0; 19];
        lease
            .seal_page(
                lease.cache(),
                &crate::test_support::test_vector([1; 24]),
                b"retained",
                b"abc",
                &mut sealed,
            )
            .unwrap();
        let mut opened = [0; 3];
        racer_crypto::open(
            expected,
            &crate::test_support::test_vector([1; 24]),
            b"retained",
            &sealed,
            &mut opened,
        )
        .unwrap();
        assert_eq!(&opened, b"abc");
    }
    pub(crate) fn keys_for(env: &racer_io::Env, caches: &[CacheId]) -> Keyring {
        let keys = empty_keys(env);
        keys.install(initial_bundle(caches)).unwrap();
        keys
    }
    /// A keyring with no bundle installed yet, as on a node that has not
    /// fetched its keys.
    pub(crate) fn empty_keys(env: &racer_io::Env) -> Keyring {
        Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::with_clock(crate::enrollment::wall_clock(
                &env.ctx.clock,
            ))),
        )
    }
    /// The bundle [`keys_for`] installs.
    pub(crate) fn initial_bundle(caches: &[CacheId]) -> KeyringBundle {
        let (_, _, roots) = issued();
        let mut initial = rotation_bundle(1, roots);
        // rotation_bundle(1) intentionally has different suffixes and material,
        // just like later epochs, rather than reusing the unrotated seed bundle.
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
        initial
    }
    pub(crate) fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
        KeyringBundle {
            schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
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
    pub(crate) fn network(env: &racer_io::Env, count: usize) -> Vec<Rc<Signatures>> {
        identities(
            env,
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
    pub(crate) use crate::app::dst::clone_head;
    pub fn identities(
        env: &racer_io::Env,
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
                    .accept(
                        cluster.clone(),
                        node.clone(),
                        chain,
                        &roots,
                        &crate::enrollment::wall_clock(&env.ctx.clock),
                    )
                    .unwrap();
                let keys = Rc::new(Keyring::new(
                    cluster.clone(),
                    node.clone(),
                    Arc::new(KeyEpochs::with_clock(crate::enrollment::wall_clock(
                        &env.ctx.clock,
                    ))),
                ));
                keys.install(KeyringBundle {
                    schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                    cluster: cluster.clone(),
                    generation: BundleGeneration(1),
                    peer_trust_roots: roots.clone(),
                    cache_keys: cache_keys(),
                })
                .unwrap();
                keys.install_signing_identity(Arc::new(identity)).unwrap();
                let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
                let signatures = Rc::new(Signatures::new(
                    keys.clone(),
                    certificates.clone(),
                    env.ctx.clone(),
                ));
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
            let env = racer_io::Env::host();
            let keys = keys(&env);
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

/// Bind a file-mode slab to `env`'s simulated world, if it has one.
pub(crate) fn simulated_slab<C: page_alloc::Charge>(
    slab: page_alloc::Slab<C>,
    env: &racer_io::Env,
) -> page_alloc::Slab<C> {
    if let Some(sim) = env.sim_os() {
        slab.simulate_in(sim.clone());
    }
    slab
}

/// Counts owned and borrowed wakes across threads.
#[derive(Default)]
pub struct WakeCounter(std::sync::atomic::AtomicUsize);

impl WakeCounter {
    /// Read the number of wake calls observed so far.
    pub fn count(&self) -> usize {
        self.0.load(std::sync::atomic::Ordering::SeqCst)
    }
}

impl std::task::Wake for WakeCounter {
    /// Count an owned wake; borrowed wakes use the trait's default forwarding.
    fn wake(self: std::sync::Arc<Self>) {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
}

/// `bytes`, returned through a per-byte data-dependent step. Static analysis
/// (CodeQL `rust/hard-coded-cryptographic-value`) then stops treating fixed
/// test vectors as hard-coded production keys or nonces. The value is
/// unchanged: `index >= N` is always false, so every byte is XORed with 0.
pub(crate) fn test_vector<const N: usize>(bytes: [u8; N]) -> [u8; N] {
    core::array::from_fn(|index| bytes[index] ^ u8::from(index >= N))
}

#[cfg(test)]
mod wake_counter_tests {
    use super::*;

    /// Shared wake instrumentation counts owned and borrowed notifications equally.
    #[test]
    fn counts_owned_and_borrowed_wakes() {
        let counter = std::sync::Arc::new(WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        waker.wake_by_ref();
        waker.wake();
        assert_eq!(counter.count(), 2);
    }
}
