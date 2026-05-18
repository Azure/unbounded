// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Construction-time configuration for a single Mercury class +
//! progress thread (one per NUMA shard in the bufferpool model).
//! Holds only the inputs needed to bring the transport up; runtime
//! tunables (page registration, in-flight bounds) live alongside
//! the components that use them.

use std::sync::Arc;

use crate::bufferpool::PeerId;
use crate::runtime::{Threading, WorkerIdx};

/// Configuration for one Mercury class. Construct one per NUMA
/// shard; the embedder is responsible for ensuring `peers` is the
/// bounded neighbor set selected by its topology layer, not a full
/// mesh (see `designs/storage-high-level.md`).
pub struct TransportConfig {
    /// Mercury NA info string. Examples:
    ///   - `"na+sm://"`               in-process shared-memory (tests)
    ///   - `"ofi+tcp://0.0.0.0:0"`    TCP via libfabric
    ///   - `"ofi+verbs;ofi_rxm://"`   IB / RoCE via libfabric
    /// The string is consumed verbatim by `HG_Init_opt2`.
    pub na_info: String,

    /// Listen for incoming RPCs. `false` means client-only (no
    /// server can be attached). With `true`, the local address can
    /// be obtained via `Class::self_address` after init for sharing
    /// with peers out-of-band.
    pub listen: bool,

    /// Bounded neighbor set. Each entry is eagerly looked up at
    /// class construction; failures abort the construction.
    pub peers: Vec<PeerEntry>,

    /// Maximum number of concurrent in-flight `bulk_get` RPCs the
    /// transport will accept. Bounds the completion slab; new
    /// requests fail fast when full rather than queueing
    /// unboundedly.
    pub max_inflight: usize,

    /// `HG_Progress` blocking timeout in milliseconds. Larger
    /// values reduce wake overhead on idle classes; smaller values
    /// shorten shutdown latency. 100 ms is a reasonable default.
    pub progress_poll_ms: u32,

    /// Worker-placement runtime. Used to spawn the progress thread
    /// pinned to the same NUMA node as the worker that drives this
    /// class. The same `Arc` is shared with the bufferpool and the
    /// blockstore.
    pub runtime: Arc<dyn Threading>,

    /// Worker slot this class is bound to. Identifies which NUMA
    /// node the progress thread is pinned against and matches the
    /// `WorkerIdx` the executor running this transport runs on.
    pub worker_idx: WorkerIdx,
}

impl TransportConfig {
    /// Minimal constructor for a client-only configuration. Tests
    /// and embedders that want listen + custom peers build on top
    /// via field assignments.
    pub fn new(
        na_info: impl Into<String>,
        runtime: Arc<dyn Threading>,
        worker_idx: WorkerIdx,
    ) -> Self {
        Self {
            na_info: na_info.into(),
            listen: false,
            peers: Vec::new(),
            max_inflight: 1024,
            progress_poll_ms: 100,
            runtime,
            worker_idx,
        }
    }
}

/// Peer address. `peer_id` is the embedder's opaque handle; `addr`
/// is the Mercury-format address string returned by the peer's
/// `Class::self_address`.
pub struct PeerEntry {
    pub peer_id: PeerId,
    pub addr: String,
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::DefaultRuntime;

    #[test]
    fn new_sets_minimal_client_defaults() {
        let rt = DefaultRuntime::new(1);
        let cfg = TransportConfig::new("na+sm://", rt, WorkerIdx(0));
        assert_eq!(cfg.na_info, "na+sm://");
        assert!(!cfg.listen);
        assert!(cfg.peers.is_empty());
        assert_eq!(cfg.max_inflight, 1024);
        assert_eq!(cfg.progress_poll_ms, 100);
        assert_eq!(cfg.worker_idx, WorkerIdx(0));
    }

    #[test]
    fn fields_are_writable_after_new() {
        let rt = DefaultRuntime::new(1);
        let mut cfg = TransportConfig::new("na+sm://", rt, WorkerIdx(0));
        cfg.listen = true;
        cfg.max_inflight = 4;
        cfg.progress_poll_ms = 5;
        cfg.peers.push(PeerEntry {
            peer_id: PeerId(1),
            addr: "na+sm://1/2".into(),
        });
        assert!(cfg.listen);
        assert_eq!(cfg.max_inflight, 4);
        assert_eq!(cfg.progress_poll_ms, 5);
        assert_eq!(cfg.peers.len(), 1);
        assert_eq!(cfg.peers[0].peer_id, PeerId(1));
    }
}
