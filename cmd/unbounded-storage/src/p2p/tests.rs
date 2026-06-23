// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Module integration tests for the p2p public surface. Hop-by-hop
//! recursive routing convergence on a clean ring and across the
//! 64-bit wraparound, driven through hand-built finger tables.
//! Subsystem-wide invariants (routing convergence, fault recovery,
//! etc.) live in the DST harness under `tests/p2p/`.

use std::collections::HashSet;
use std::future::Future;
use std::pin::pin;
use std::task::{Context, Poll};

use crate::p2p::{FingerTable, FingerTableConfig, NodeId, PeerEntry, RingId, TopologyTags};
use crate::runtime::noop_waker;

// ---------------------------------------------------------------------------
// Noop-waker block_on built on the shared `runtime::noop_waker` helper
// to keep the crate runtime-agnostic.
// ---------------------------------------------------------------------------

fn block_on<F: Future>(future: F) -> F::Output {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = pin!(future);
    let mut spins: u64 = 0;
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < 1_000_000, "block_on stuck (no progress)");
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Synthetic ring builder.
// ---------------------------------------------------------------------------

fn tags(parts: &[&str]) -> TopologyTags {
    TopologyTags(parts.iter().map(|s| s.to_string()).collect())
}

/// Build N peers spread evenly around the ring with identical
/// topology tags so finger selection is driven purely by ring
/// distance; the `topology_preference_beats_random_tiebreak` unit
/// test in `fingers.rs` covers the topology tiebreak.
fn synthetic_ring(n: u64) -> Vec<PeerEntry> {
    let step = u64::MAX / n;
    (0..n)
        .map(|i| PeerEntry {
            node: NodeId(i),
            ring: RingId(i.wrapping_mul(step)),
            tags: tags(&["us", "z1", "row1", "rack1"]),
        })
        .collect()
}

fn build_tables(peers: &[PeerEntry], k: u32) -> Vec<FingerTable> {
    peers
        .iter()
        .map(|p| FingerTable::build(p.clone(), peers, FingerTableConfig { k }))
        .collect()
}

fn node_index(peers: &[PeerEntry], node: NodeId) -> usize {
    peers.iter().position(|p| p.node == node).expect("node")
}

/// Walk the recursive-routing chain from `source_idx` toward
/// `target` and assert the standard convergence properties:
/// every hop is to a fresh node, hops stay within
/// `log2(n) + 4`, and the chain terminates exactly at the target.
fn assert_routes_to(
    peers: &[PeerEntry],
    tables: &[FingerTable],
    source_idx: usize,
    target_idx: usize,
) {
    let target = peers[target_idx].ring;
    let mut current = source_idx;
    let mut hops = 0u32;
    let mut visited: HashSet<NodeId> = HashSet::new();
    visited.insert(peers[current].node);
    let max_hops = (peers.len() as f64).log2().ceil() as u32 + 4;
    while peers[current].node != peers[target_idx].node {
        let next = tables[current]
            .next_hop(target)
            .expect("next hop should exist while routing toward a distinct target")
            .node;
        assert!(visited.insert(next), "cycle: revisited {next:?}");
        current = node_index(peers, next);
        hops += 1;
        assert!(hops <= max_hops, "hop budget exceeded: {hops} > {max_hops}");
    }
    assert!(
        hops >= 1,
        "at least one hop expected for distinct source/target"
    );
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

#[test]
fn recursive_routing_converges() {
    block_on(async {
        let peers = synthetic_ring(16);
        let tables = build_tables(&peers, 8);
        // Source and target on the same forward arc (no wrap).
        assert_routes_to(&peers, &tables, 0, 11);
    });
}

#[test]
fn recursive_routing_wraps_at_ring_boundary() {
    block_on(async {
        let peers = synthetic_ring(16);
        let tables = build_tables(&peers, 8);
        // Source near the top, target near the bottom: the
        // shortest forward arc crosses the `u64::MAX -> 0`
        // wraparound. Convergence here exercises the wrapping
        // ring-distance metric throughout finger lookup.
        assert_routes_to(&peers, &tables, 14, 1);
    });
}
