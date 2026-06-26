// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Ring math: hashing nodes onto the 64-bit ring, forward ring
//! distance, topology distance, and rendezvous hashing used to
//! break ties when picking a finger.

use crate::bufferpool::StripeKey;

use crate::p2p::types::{NodeId, RingId, TopologyTags};
use crate::storage::types::GOLDEN_RATIO_64;

/// Sentinel used when comparing topology tag vectors of unequal
/// length. A wildcard never compares equal to a real tag or to
/// another wildcard, so unknown slots are treated as "farther".
pub const WILDCARD_TAG: &str = "*";

/// Standard splitmix64 finalizer. Bijective on `u64`, used as a
/// cheap, well-mixed hash where we need determinism but not a
/// cryptographic guarantee.
pub const fn splitmix64(x: u64) -> u64 {
    let mut x = x;
    x = (x ^ (x >> 30)).wrapping_mul(0xbf58476d1ce4e5b9);
    x = (x ^ (x >> 27)).wrapping_mul(0x94d049bb133111eb);
    x ^ (x >> 31)
}

/// Map a [`StripeKey`] to a 64-bit ring position. The input is a
/// SHA-256 digest and therefore already uniformly distributed; we
/// take the leading 8 bytes as a little-endian `u64` and treat
/// that as the ring id. See [`RingId`](crate::p2p::RingId) for why
/// the ring is 64 bits wide.
pub fn stripe_to_ring(key: StripeKey) -> RingId {
    let mut buf = [0u8; 8];
    buf.copy_from_slice(&key.0[..8]);
    RingId(u64::from_le_bytes(buf))
}

/// Run a raw [`NodeId`] through `splitmix64` so config-supplied
/// integer ids do not cluster on the ring.
pub fn node_to_ring(node: NodeId) -> RingId {
    RingId(splitmix64(node.0))
}

/// Derive a stable internal node id from an operator-facing peer name.
pub fn node_id_from_name(name: &str) -> NodeId {
    let digest = blake3::hash(name.as_bytes());
    let mut buf = [0u8; 8];
    buf.copy_from_slice(&digest.as_bytes()[..8]);
    NodeId(u64::from_le_bytes(buf))
}

/// Forward distance on the ring from `from` to `to`. Returns 0 only
/// when `from == to`; wraps modulo `2^64` otherwise.
pub fn ring_distance(from: RingId, to: RingId) -> u64 {
    to.0.wrapping_sub(from.0)
}

/// Topology distance between two tag vectors, mirroring the
/// simulator: right-pad both to the longer length with
/// [`WILDCARD_TAG`], scan left-to-right to find the longest
/// prefix of equal, non-wildcard slots, and return
/// `len - prefix_len`.
///
/// Concretely on a `[region, zone, row, rack]` vector: identical is
/// 0, same row but different rack is 1, only the region matches is
/// 3, entirely disjoint is 4.
pub fn topology_distance(local: &TopologyTags, peer: &TopologyTags) -> u32 {
    let n = local.0.len().max(peer.0.len());
    if n == 0 {
        return 0;
    }
    let mut prefix = 0usize;
    for i in 0..n {
        let l = local.0.get(i).map(String::as_str).unwrap_or(WILDCARD_TAG);
        let p = peer.0.get(i).map(String::as_str).unwrap_or(WILDCARD_TAG);
        if l == WILDCARD_TAG || p == WILDCARD_TAG || l != p {
            break;
        }
        prefix += 1;
    }
    (n - prefix) as u32
}

/// Rendezvous hash used to break ties across arcs without
/// coordination. The `arc` tag decorrelates picks across arcs of
/// the same `(local, candidate)` pair.
pub fn rendezvous_hash(local: RingId, candidate: RingId, arc: u32) -> u64 {
    splitmix64(local.0 ^ candidate.0 ^ (arc as u64).wrapping_mul(GOLDEN_RATIO_64))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splitmix64_is_deterministic_and_bijective_on_samples() {
        // Determinism.
        assert_eq!(splitmix64(0), splitmix64(0));
        assert_eq!(splitmix64(42), splitmix64(42));
        // Distinct inputs yield distinct outputs on a small sample.
        let mut seen = std::collections::HashSet::new();
        for i in 0..1024u64 {
            assert!(seen.insert(splitmix64(i)), "collision at {i}");
        }
        // Determinism over a high input: same input always
        // produces the same output. We assert no specific value
        // here; the bijectivity loop above covers spread.
        let _ = splitmix64(u64::MAX);
    }

    #[test]
    fn stripe_to_ring_uses_leading_8_bytes_le() {
        let mut key = [0u8; 32];
        key[..8].copy_from_slice(&0x0123_4567_89ab_cdefu64.to_le_bytes());
        assert_eq!(
            stripe_to_ring(StripeKey(key)),
            RingId(0x0123_4567_89ab_cdef)
        );
    }

    #[test]
    fn ring_distance_basics() {
        assert_eq!(ring_distance(RingId(0), RingId(0)), 0);
        assert_eq!(ring_distance(RingId(0), RingId(u64::MAX)), u64::MAX);
        assert_eq!(ring_distance(RingId(u64::MAX), RingId(0)), 1);
        assert_eq!(ring_distance(RingId(100), RingId(110)), 10);
    }

    #[test]
    fn node_to_ring_mixes() {
        let a = node_to_ring(NodeId(0));
        let b = node_to_ring(NodeId(1));
        assert_ne!(a, b);
        assert_eq!(a, node_to_ring(NodeId(0)));
    }

    #[test]
    fn node_id_from_name_is_stable_and_distinguishes_names() {
        let a = node_id_from_name("node-a");
        let b = node_id_from_name("node-b");
        assert_eq!(a, node_id_from_name("node-a"));
        assert_ne!(a, b);
    }

    fn tags(parts: &[&str]) -> TopologyTags {
        TopologyTags(parts.iter().map(|s| s.to_string()).collect())
    }

    #[test]
    fn topology_distance_identical_is_zero() {
        let a = tags(&["us", "zone1", "row3", "rack9"]);
        assert_eq!(topology_distance(&a, &a), 0);
    }

    #[test]
    fn topology_distance_same_row_different_rack_is_one() {
        let a = tags(&["us", "zone1", "row3", "rack9"]);
        let b = tags(&["us", "zone1", "row3", "rack8"]);
        assert_eq!(topology_distance(&a, &b), 1);
    }

    #[test]
    fn topology_distance_only_region_matches_is_three() {
        let a = tags(&["us", "zone1", "row3", "rack9"]);
        let b = tags(&["us", "zone2", "rowX", "rackY"]);
        assert_eq!(topology_distance(&a, &b), 3);
    }

    #[test]
    fn topology_distance_disjoint_is_full_length() {
        let a = tags(&["us", "zone1", "row3", "rack9"]);
        let b = tags(&["eu", "zone1", "row3", "rack9"]);
        assert_eq!(topology_distance(&a, &b), 4);
    }

    #[test]
    fn topology_distance_unequal_lengths_pad_with_wildcard() {
        // Shorter peer right-pads with wildcards; wildcard never
        // equals a real label, so the prefix only counts the
        // explicitly-matching slots.
        let a = tags(&["us", "zone1", "row3", "rack9"]);
        let b = tags(&["us", "zone1"]);
        // Prefix matches 2 slots; padded length is 4; distance = 2.
        assert_eq!(topology_distance(&a, &b), 2);
    }

    #[test]
    fn topology_distance_empty_vectors_is_zero() {
        let e = TopologyTags::default();
        assert_eq!(topology_distance(&e, &e), 0);
    }

    #[test]
    fn topology_distance_empty_vs_nonempty_is_length_of_nonempty() {
        // An empty vector right-pads to the other's length entirely
        // with wildcards, so no slot matches and the distance equals
        // the nonempty vector's length.
        let e = TopologyTags::default();
        let a = tags(&["a"]);
        assert_eq!(topology_distance(&e, &a), 1);
        assert_eq!(topology_distance(&a, &e), 1);
        let three = tags(&["a", "b", "c"]);
        assert_eq!(topology_distance(&e, &three), 3);
        assert_eq!(topology_distance(&three, &e), 3);
    }

    #[test]
    fn rendezvous_hash_is_stable_and_differs_across_arcs() {
        let l = RingId(0xdead_beef);
        let c = RingId(0xfeed_face);
        assert_eq!(rendezvous_hash(l, c, 0), rendezvous_hash(l, c, 0));
        assert_ne!(rendezvous_hash(l, c, 0), rendezvous_hash(l, c, 1));
        assert_ne!(rendezvous_hash(l, c, 7), rendezvous_hash(l, c, 8));
    }
}
