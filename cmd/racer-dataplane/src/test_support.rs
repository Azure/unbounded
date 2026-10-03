//! Deterministic test-only seams; no fake implementation is linked into production.
pub mod clock {
    use crate::{
        error::{Error, Result},
        runtime::deadline::Deadline,
    };
    use std::{
        cell::Cell,
        time::{Duration, Instant, SystemTime, UNIX_EPOCH},
    };

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
        use crate::model::{
            CacheId, CacheKey, CurrentVersion, ExpiresAt, ObjectId, ObjectVersion, StrongEtag,
            VersionMetadata,
        };
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
pub mod origin;

/// Minimal real control-plane state for storage and flight fixtures. Callers with
/// rotating keys or publications should share their own Availability instead.
pub fn availability() -> std::rc::Rc<crate::control::state::Availability> {
    availability_for(vec![crate::model::CacheId(
        crate::security::test_support::CACHE.into(),
    )])
}
pub fn availability_for(
    caches: Vec<crate::model::CacheId>,
) -> std::rc::Rc<crate::control::state::Availability> {
    crate::control::state::for_caches(
        std::rc::Rc::new(crate::security::test_support::keys_for(&caches)),
        caches,
    )
}

pub struct NoPeers;

impl crate::peer::PeerClient for NoPeers {
    fn direct_hedge_available(
        &self,
        _: &crate::topology::membership::MembershipLease,
        _: &crate::model::NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _: crate::peer::protocol::PeerRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a crate::runtime::deadline::RequestScope,
    ) -> crate::error::Operation<'a, crate::peer::protocol::VerifiedResponse> {
        panic!("local origin scenario must not hedge to a peer")
    }
    fn request<'a>(
        &'a self,
        _: crate::peer::protocol::PeerRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a crate::runtime::deadline::RequestScope,
    ) -> crate::error::Operation<'a, crate::peer::protocol::VerifiedResponse> {
        Box::pin(async { panic!("local origin scenario must not contact peers") })
    }
}

/// Side-effect-free configuration for assembled worker scenarios.
pub mod cluster {
    use crate::{
        config::Config,
        model::{ClusterId, Limits, NodeId},
    };
    use std::{
        num::{NonZeroU32, NonZeroUsize},
        time::Duration,
    };

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
