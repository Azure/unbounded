// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Synthetic cluster used by the p2p DST. Builds a [`FingerTable`]
//! per peer up front and routes one request at a time through the
//! framework executor: every hop yields a random number of times to
//! model fabric latency, and an optional per-hop fault rate stalls
//! progress so retry / liveness paths get exercised.
//!
//! Hop semantics: `forward_once` returns the next hop computed from
//! the current node's finger table; the workload converts a `None`
//! into "this node is the destination" and walks the return path
//! back through the path stack. A fault stalls progress (returns
//! `current`) so the workload can bound retries via the executor's
//! step budget rather than via an explicit per-hop retry counter.

use std::cell::{Cell, RefCell};
use std::collections::{BTreeMap, HashMap};
use std::rc::Rc;

use rand::Rng;
use unbounded_storage::p2p::{FingerTable, FingerTableConfig, NodeId, PeerEntry, RingId};

use crate::framework::executor::{with_sim, yield_n};

/// P2p-specific simulation knobs. Held behind an `Rc` so the
/// cluster and any future return-path workers can share one
/// configuration without leaking into [`SimState`].
#[derive(Default)]
pub struct P2pSimCfg {
    /// Upper bound on the per-hop yield count drawn from the
    /// framework PRNG.
    pub max_hop_delay: Cell<u32>,
    /// Fault probability per hop, expressed in 1000ths (`0` = off,
    /// `1000` = every hop). 1000ths instead of 100ths so the
    /// strategy can dial in low-but-nonzero rates without rounding
    /// everything to "always" or "never".
    pub hop_fault_rate: Cell<u32>,
}

impl P2pSimCfg {
    pub fn new() -> Rc<Self> {
        let cfg = Rc::new(Self::default());
        cfg.max_hop_delay.set(8);
        cfg.hop_fault_rate.set(0);
        cfg
    }
}

/// Draw `[0, max_hop_delay]` yield count from the framework PRNG.
pub fn draw_hop_delay(cfg: &P2pSimCfg) -> u32 {
    let max = cfg.max_hop_delay.get();
    if max == 0 {
        0
    } else {
        with_sim(|s| s.rng.gen_range(0..=max))
    }
}

/// Draw a fault decision from the framework PRNG.
pub fn draw_hop_fault(cfg: &P2pSimCfg) -> bool {
    let rate = cfg.hop_fault_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(1000), 1000))
}

/// Synchronous outcome of [`SimCluster::route`].
#[derive(Clone, Debug)]
#[allow(dead_code)]
pub enum RouteOutcome {
    /// Forwarding reached a node whose finger table has no further
    /// progress to make: the target lies in the node's arc. The
    /// terminal node is the destination of the lookup.
    Reached { hops: u32, terminal: NodeId },
    /// A finger table returned `None` for a non-destination
    /// reason (no finger preceding the target). Pinned in the
    /// outcome enum so tests can distinguish "we arrived" from
    /// "we stalled".
    #[allow(dead_code)]
    Stalled { hops: u32, terminal: NodeId },
    /// Hit the hop budget; ran out before reaching the destination.
    /// Kept as a distinct variant so the route helper never returns
    /// a misleading `Reached` after looping forever.
    OutOfBudget { hops: u32, terminal: NodeId },
}

/// Record of one forwarding step. The workload records both the
/// successful and faulted hops so tests can audit the path.
#[derive(Clone, Debug)]
#[allow(dead_code)]
pub struct HopRecord {
    pub from: NodeId,
    /// `None` means the current node had no further finger to
    /// forward to (it is the destination, or routing has stalled).
    pub to: Option<NodeId>,
    pub target: RingId,
    pub fault: bool,
}

pub struct SimCluster {
    tables: HashMap<NodeId, FingerTable>,
    /// Ring position -> node id; the oracle uses this to compute
    /// the naive destination predecessor for a given target.
    ring_to_node: BTreeMap<u64, NodeId>,
    cfg: Rc<P2pSimCfg>,
    pub records: RefCell<Vec<HopRecord>>,
}

impl SimCluster {
    pub fn new(peers: Vec<PeerEntry>, k: u32, cfg: Rc<P2pSimCfg>) -> Self {
        let mut tables = HashMap::with_capacity(peers.len());
        let mut ring_to_node = BTreeMap::new();
        for local in &peers {
            let table = FingerTable::build(local.clone(), &peers, FingerTableConfig { k });
            tables.insert(local.node, table);
            ring_to_node.insert(local.ring.0, local.node);
        }
        Self {
            tables,
            ring_to_node,
            cfg,
            records: RefCell::new(Vec::new()),
        }
    }

    /// Disjoint-discovery counterpart to [`Self::new`]: instead of
    /// giving every node the full peer set, each node's table is
    /// rebuilt via [`FingerTable::from_explicit`] from only the
    /// neighbors the global build would have selected (its distinct
    /// non-self fingers plus successor and predecessor). This mirrors
    /// exactly what the process route builder does when a `[routing_plan]`
    /// is supplied. A cluster built this way must route every target
    /// identically to one built with [`Self::new`]; the proptest
    /// `disjoint_routing_matches_global` pins that equivalence.
    pub fn new_disjoint(peers: Vec<PeerEntry>, k: u32, cfg: Rc<P2pSimCfg>) -> Self {
        let mut tables = HashMap::with_capacity(peers.len());
        let mut ring_to_node = BTreeMap::new();
        for local in &peers {
            let built = FingerTable::build(local.clone(), &peers, FingerTableConfig { k });
            let mut fingers: Vec<PeerEntry> = Vec::new();
            for f in built.fingers() {
                if f.node == local.node {
                    continue;
                }
                if !fingers.iter().any(|e| e.node == f.node) {
                    fingers.push(f.clone());
                }
            }
            let table = FingerTable::from_explicit(
                local.clone(),
                fingers,
                built.successor().cloned(),
                built.predecessor().cloned(),
            );
            tables.insert(local.node, table);
            ring_to_node.insert(local.ring.0, local.node);
        }
        Self {
            tables,
            ring_to_node,
            cfg,
            records: RefCell::new(Vec::new()),
        }
    }

    #[allow(dead_code)]
    pub fn cfg(&self) -> &Rc<P2pSimCfg> {
        &self.cfg
    }

    pub fn contains(&self, node: NodeId) -> bool {
        self.tables.contains_key(&node)
    }

    /// Look up the ring id of `node`. Panics if the node is not
    /// in the cluster; invariants only call this with terminals
    /// drawn from the run, which always exist.
    #[allow(dead_code)]
    pub fn ring_of(&self, node: NodeId) -> u64 {
        self.tables
            .get(&node)
            .expect("ring_of: node not in cluster")
            .local()
            .ring
            .0
    }

    /// Async forwarding step: yields a random number of times, then
    /// either records a fault (returning `current` unchanged) or
    /// asks the local table for the next hop.
    pub async fn forward_once(&self, current: NodeId, target: RingId) -> Option<NodeId> {
        let delay = draw_hop_delay(&self.cfg);
        let fault = draw_hop_fault(&self.cfg);
        yield_n(delay).await;
        if fault {
            self.records.borrow_mut().push(HopRecord {
                from: current,
                to: Some(current),
                target,
                fault: true,
            });
            return Some(current);
        }
        let next = self
            .tables
            .get(&current)
            .expect("current node must exist in cluster")
            .next_hop(target)
            .map(|p| p.node);
        self.records.borrow_mut().push(HopRecord {
            from: current,
            to: next,
            target,
            fault: false,
        });
        next
    }

    /// Synchronous, fault-free reference walk. Used by the
    /// hand-rolled scenarios and as an oracle for the proptest
    /// invariants that need a faithful path.
    pub fn route(&self, start: NodeId, target: RingId, max_hops: u32) -> RouteOutcome {
        let mut current = start;
        let mut hops = 0u32;
        loop {
            let table = self.tables.get(&current).expect("start node must exist");
            match table.next_hop(target).map(|p| p.node) {
                None => {
                    return RouteOutcome::Reached {
                        hops,
                        terminal: current,
                    };
                }
                Some(next) => {
                    if next == current {
                        return RouteOutcome::Stalled {
                            hops,
                            terminal: current,
                        };
                    }
                    current = next;
                    hops += 1;
                    if hops >= max_hops {
                        return RouteOutcome::OutOfBudget {
                            hops,
                            terminal: current,
                        };
                    }
                }
            }
        }
    }

    /// Oracle: the destination of a lookup for `target` is the
    /// node whose ring id is the *successor* of `target` on the
    /// 64-bit ring, i.e. the smallest ring id `>= target`, wrapping
    /// around to the smallest ring id overall when no peer sits at
    /// or above `target`.
    ///
    /// This is Chord successor routing as the protocol specifies it:
    /// keys are SHA-256-derived ring positions and the protocol is
    /// a variant of Chord; a lookup terminates at "the target node
    /// for the key" which on the ring is the successor.
    ///
    /// Reference successor for `target`: the node with the smallest
    /// forward `ring_distance(target, node.ring)` over the peer set.
    /// The DST workload's `terminal` for every completed client must
    /// equal this value; that equality is pinned by
    /// `assert_progress_or_destination` in `tests/p2p/tests.rs`.
    pub fn naive_destination(&self, target: RingId) -> NodeId {
        // Smallest ring id >= target.
        if let Some((_, &node)) = self.ring_to_node.range(target.0..).next() {
            return node;
        }
        // Wrap: no peer at or above target, so the successor is the
        // lowest peer on the ring.
        let (_, &node) = self
            .ring_to_node
            .iter()
            .next()
            .expect("cluster has at least one peer");
        node
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use unbounded_storage::p2p::TopologyTags;

    fn peer(node: u64, ring: u64) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: RingId(ring),
            tags: TopologyTags(vec!["g0".to_string()]),
        }
    }

    fn cluster(peers: Vec<PeerEntry>) -> SimCluster {
        SimCluster::new(peers, 4, P2pSimCfg::new())
    }

    #[test]
    fn naive_destination_picks_successor_at_exact_ring_id() {
        let c = cluster(vec![peer(1, 100), peer(2, 200), peer(3, 300)]);
        // Target equals a peer's ring id: successor is that peer.
        assert_eq!(c.naive_destination(RingId(200)), NodeId(2));
        assert_eq!(c.naive_destination(RingId(100)), NodeId(1));
        assert_eq!(c.naive_destination(RingId(300)), NodeId(3));
    }

    #[test]
    fn naive_destination_picks_smallest_ring_id_at_or_above_target() {
        let c = cluster(vec![peer(1, 100), peer(2, 200), peer(3, 300)]);
        assert_eq!(c.naive_destination(RingId(0)), NodeId(1));
        assert_eq!(c.naive_destination(RingId(150)), NodeId(2));
        assert_eq!(c.naive_destination(RingId(201)), NodeId(3));
    }

    #[test]
    fn naive_destination_wraps_when_target_exceeds_all_peers() {
        // Target between the largest peer ring id and u64::MAX must
        // wrap around to the smallest peer on the ring.
        let c = cluster(vec![peer(1, 100), peer(2, 200), peer(3, 300)]);
        assert_eq!(c.naive_destination(RingId(301)), NodeId(1));
        assert_eq!(c.naive_destination(RingId(u64::MAX)), NodeId(1));
    }

    #[test]
    fn naive_destination_wraparound_with_high_ring_ids() {
        // A more realistic spread that pins the wraparound case:
        // the largest peer sits well below u64::MAX, and a target
        // between it and u64::MAX must return the lowest peer.
        let lo = peer(10, 5);
        let mid = peer(20, u64::MAX / 2);
        let hi = peer(30, u64::MAX - 1000);
        let c = cluster(vec![lo.clone(), mid.clone(), hi.clone()]);
        assert_eq!(c.naive_destination(RingId(u64::MAX - 999)), lo.node);
        assert_eq!(c.naive_destination(RingId(u64::MAX)), lo.node);
        assert_eq!(c.naive_destination(RingId(hi.ring.0)), hi.node);
        assert_eq!(c.naive_destination(RingId(hi.ring.0 - 1)), hi.node);
        assert_eq!(c.naive_destination(RingId(mid.ring.0 + 1)), hi.node);
    }

    #[test]
    fn naive_destination_single_peer_is_always_destination() {
        let only = peer(7, 12345);
        let c = cluster(vec![only.clone()]);
        for t in [0u64, 1, 12345, 12346, u64::MAX] {
            assert_eq!(c.naive_destination(RingId(t)), only.node);
        }
    }
}
