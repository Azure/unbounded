//! Deterministic test-only seams; no fake implementation is linked into production.
pub mod clock;
pub mod origin;

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
            page_hedge: Default::default(),
            peer_admission: Default::default(),
            routing_algorithm: crate::topology::RoutingAlgorithm::default(),
            shares: NonZeroU32::new(4).unwrap(),
            disk_page_entries: NonZeroUsize::new(65536).unwrap(),
            checkpoint_bytes: NonZeroUsize::new(64 * 1024 * 1024).unwrap(),
            cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
            node: NodeId("00000000-0000-4000-8000-000000000002".into()),
            max_threads: 2,
            allow_smt: false,
            opaque_relay: false,
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
