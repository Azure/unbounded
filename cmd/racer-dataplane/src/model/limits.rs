//! Independent bounded-resource dimensions. Validation precedes resource creation.

use std::num::NonZeroUsize;

#[derive(Clone, Debug)]
pub struct Limits {
    pub plaintext_bytes: NonZeroUsize,
    pub ciphertext_bytes: NonZeroUsize,
    pub dirty_bytes: NonZeroUsize,
    pub registered_bytes: NonZeroUsize,
    pub request_context_bytes: NonZeroUsize,
    pub flights: NonZeroUsize,
    pub waiters_per_flight: NonZeroUsize,
    pub queue_entries: NonZeroUsize,
    pub connections_per_neighbor: NonZeroUsize,
    pub client_connections: NonZeroUsize,
    pub pipes: NonZeroUsize,
    pub range_window_pages: NonZeroUsize,
    pub replay_entries: NonZeroUsize,
    pub header_bytes: NonZeroUsize,
    pub route_search_work: NonZeroUsize,
    pub cached_rankings: NonZeroUsize,
    pub cached_paths: NonZeroUsize,
    pub retained_snapshots: NonZeroUsize,
    pub metadata_entries: NonZeroUsize,
    pub relay_transfers: NonZeroUsize,
}

#[derive(Clone, Copy, Debug)]
pub enum ResourceClass {
    Plaintext,
    Ciphertext,
    DirtyCiphertext,
    Registered,
    RequestContext,
    Flight,
    Waiter,
    Connection,
    Pipe,
    ControlProgress,
    Relay,
}
