// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared value types for the stripe DHT: ring positions, topology
//! labels, peer entries, and the recursive-routing request.

use std::time::Instant;

use crate::bufferpool::{Req, StripeKey, TraceCtx};
use crate::p2p::ring::stripe_to_ring;

/// Opaque node identifier minted by the p2p layer.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash)]
pub struct NodeId(pub u64);

/// 64-bit position on the DHT ring. The protocol truncates
/// `SHA-256(file_id || stripe_index)` to 64 bits; we accept any
/// pre-hashed `u64` and provide a deterministic mapping from
/// [`StripeKey`] via [`ring::stripe_to_ring`](crate::p2p::stripe_to_ring).
///
/// We deliberately stop at 64 bits even though the underlying key
/// is 256: at the operating point (up to ~200k nodes, billions of
/// stripes) the birthday bound on collisions is well above what
/// we will see; ring math (`wrapping_sub`, division by `arc_span`,
/// `splitmix64`, rendezvous hash) is a single-register operation
/// at 64 bits and would become multiword at 128 or 256; and
/// `RingId` being `Copy` is relied on by routing hot paths. The
/// 256-bit content address is still carried alongside as
/// [`StripeKey`]; only the *routing position* is truncated.
#[derive(Copy, Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct RingId(pub u64);

/// Topology label vector in coarsest-to-finest order
/// (e.g. region, zone, row, rack). Distance between two label
/// vectors is the number of trailing slots that differ: same rack
/// is 0, same row but different rack is 1, and entirely disjoint
/// vectors are at distance `len`. Shorter vectors are right-padded
/// with a wildcard sentinel that never matches.
#[derive(Clone, Debug, Default, PartialEq, Eq, Hash)]
pub struct TopologyLabels(pub Vec<String>);

/// A peer participating in the DHT. `ring` is the peer's hashed
/// identity on the 64-bit ring; `node` is the local fabric
/// identifier used by the bufferpool.
#[derive(Clone, Debug)]
pub struct PeerEntry {
    pub node: NodeId,
    pub ring: RingId,
    pub labels: TopologyLabels,
}

/// Recursive-routing request. Unlike the old source-routed
/// placeholder this carries no path; each forwarder computes its
/// own next hop from its local finger table.
#[derive(Clone, Debug)]
pub struct P2pReq {
    pub key: StripeKey,
    /// Cached `stripe_to_ring(key)` so each forwarder can pick a
    /// next hop without rehashing the 32-byte key. The caller is
    /// responsible for keeping this consistent with `key`; tests
    /// may pass a different mapping for coverage.
    pub target: RingId,
    pub deadline: Instant,
    pub trace: TraceCtx,
    pub hops: u32,
}

impl P2pReq {
    pub fn new(key: StripeKey, deadline: Instant, trace: TraceCtx) -> Self {
        Self {
            key,
            target: stripe_to_ring(key),
            deadline,
            trace,
            hops: 0,
        }
    }

    pub fn new_with_target(
        key: StripeKey,
        target: RingId,
        deadline: Instant,
        trace: TraceCtx,
    ) -> Self {
        Self {
            key,
            target,
            deadline,
            trace,
            hops: 0,
        }
    }
}

impl Req for P2pReq {
    fn key(&self) -> StripeKey {
        self.key
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    #[test]
    fn new_populates_fields_with_zero_hops() {
        let key = StripeKey([7u8; 32]);
        let deadline = Instant::now() + Duration::from_secs(1);
        let req = P2pReq::new(key, deadline, TraceCtx::default());
        assert_eq!(req.key, key);
        assert_eq!(req.target, stripe_to_ring(key));
        assert_eq!(req.deadline, deadline);
        assert_eq!(req.hops, 0);
    }

    #[test]
    fn req_trait_returns_key() {
        let key = StripeKey([3u8; 32]);
        let req = P2pReq::new_with_target(key, RingId(0), Instant::now(), TraceCtx::default());
        assert_eq!(<P2pReq as Req>::key(&req), key);
    }

    #[test]
    fn ring_id_orders_by_raw_u64() {
        // Sanity: the derived Ord ignores wraparound; wraparound-aware
        // comparison logic lives in ring.rs.
        assert!(RingId(0) < RingId(1));
        assert!(RingId(u64::MAX - 1) < RingId(u64::MAX));
    }
}
