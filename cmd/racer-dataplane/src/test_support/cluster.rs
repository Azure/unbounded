//! Virtual topology/control fixtures and side-effect-free composition configuration.
use crate::{
    config::Config,
    error::{Result, pending},
    model::{
        identity::{ClusterId, NodeId},
        limits::Limits,
    },
};
use std::{num::NonZeroUsize, time::Duration};
pub struct Cluster;
impl Cluster {
    pub fn partition(&self, _left: &NodeId, _right: &NodeId) -> Result<()> {
        pending("test.cluster.partition")
    }
}
pub fn config(enable_rdma: bool) -> Config {
    let count = NonZeroUsize::new(16).unwrap();
    let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
    Config {
        cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
        node: NodeId("00000000-0000-4000-8000-000000000002".into()),
        max_threads: 2,
        enable_rdma,
        control_endpoint: "https://control.invalid".into(),
        peer_listen: "127.0.0.1:0".parse().unwrap(),
        diagnostics_listen: "127.0.0.1:0".parse().unwrap(),
        trust_bundle: "unused/ca".into(),
        service_account_token: "unused/token".into(),
        secret_directory: "unused/secrets".into(),
        identity_directory: "unused/identity".into(),
        slab_directory: "unused/slabs".into(),
        slab_bytes: 1024 * 1024 * 1024,
        segment_bytes: 64 * 1024 * 1024,
        free_segment_reserve: 2,
        request_timeout: Duration::from_secs(30),
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
            replay_entries: count,
            header_bytes: NonZeroUsize::new(16 * 1024).unwrap(),
            route_search_work: count,
            cached_rankings: count,
            cached_paths: count,
            retained_snapshots: count,
            metadata_entries: count,
            relay_transfers: count,
        },
    }
}
