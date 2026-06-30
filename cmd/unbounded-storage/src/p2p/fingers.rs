// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-node finger table: a deterministic k-arc partition of the
//! 64-bit ring with one chosen peer per arc, plus the Chord
//! closest-preceding-finger lookup used by recursive routing.

use crate::p2p::ring::{WILDCARD_TAG, rendezvous_hash, ring_distance, topology_distance};
use crate::p2p::types::{PeerEntry, RingId};

/// Build-time knobs for [`FingerTable`]. Targets `k` in the
/// 100-150 range against the ~200 RDMA QP budget per node; we
/// default to 100.
#[derive(Clone, Debug)]
pub struct FingerTableConfig {
    /// Arcs per node.
    pub k: u32,
    /// How automatic finger selection treats topology tags.
    pub topology: TopologySelection,
}

impl Default for FingerTableConfig {
    fn default() -> Self {
        Self {
            k: 100,
            topology: TopologySelection::default(),
        }
    }
}

impl FingerTableConfig {
    pub fn with_k(k: u32) -> Self {
        Self {
            k,
            ..Self::default()
        }
    }
}

/// Topology policy used when selecting one candidate per finger-table arc.
#[derive(Clone, Debug, Default, PartialEq)]
pub enum TopologySelection {
    /// Historical behavior: topology distance is a hard priority before
    /// rendezvous hashing.
    #[default]
    HardLocality,
    /// Weighted rendezvous behavior: topology adjusts candidate score but
    /// does not dominate ring entropy.
    Weighted(TopologyWeighting),
}

/// Weighted topology preference for automatic finger selection.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct TopologyWeighting {
    pub prefix_weights: Vec<TopologyPrefixWeight>,
}

/// Weight applied when local and candidate share the configured tag prefix.
#[derive(Copy, Clone, Debug, PartialEq)]
pub struct TopologyPrefixWeight {
    pub tag_index: u32,
    pub weight: f64,
}

/// Finger table for a single local node. Fully deterministic in
/// `(local, peers, k)`: given the same inputs every node in the
/// cluster computes the same table for itself, which is how the
/// protocol avoids a stabilization phase.
pub struct FingerTable {
    local: PeerEntry,
    /// Length `k`. Entry `i` is the chosen peer for arc `i` (the
    /// half-open arc starting at `local.ring + i * arc_span` on
    /// the 64-bit ring). When an arc contains no peer the slot
    /// stores a clone of `local`; [`Self::next_hop`] filters
    /// these out.
    fingers: Vec<PeerEntry>,
    /// Immediate ring successor (smallest forward ring distance
    /// from `local`). `None` only for a single-node cluster.
    successor: Option<PeerEntry>,
    /// Immediate ring predecessor (smallest forward ring distance
    /// to `local`). `None` only for a single-node cluster.
    predecessor: Option<PeerEntry>,
}

impl FingerTable {
    /// Build the table for `local` against the candidate peer set
    /// `peers`. `peers` may include `local`; it is filtered out
    /// internally. Within each arc the winner is chosen by the
    /// configured topology policy, with stable ring-position tie-breaks
    /// so the build is fully deterministic.
    pub fn build(local: PeerEntry, peers: &[PeerEntry], cfg: FingerTableConfig) -> Self {
        let k = cfg.k.max(1);
        let arc_span = u64::MAX / k as u64;
        let mut fingers: Vec<PeerEntry> = (0..k).map(|_| local.clone()).collect();

        let mut successor: Option<PeerEntry> = None;
        let mut succ_dist: u64 = 0;
        let mut predecessor: Option<PeerEntry> = None;
        let mut pred_dist: u64 = 0;

        for cand in peers {
            if cand.node == local.node {
                continue;
            }
            let arc = arc_index(local.ring, cand.ring, arc_span, k);
            let current = &fingers[arc as usize];
            let challenger_is_self = current.node == local.node;
            if challenger_is_self
                || better(local.ring, &local.tags, cand, current, arc, &cfg.topology)
            {
                fingers[arc as usize] = cand.clone();
            }

            let fwd = ring_distance(local.ring, cand.ring);
            if fwd != 0 && (successor.is_none() || fwd < succ_dist) {
                succ_dist = fwd;
                successor = Some(cand.clone());
            }
            let back = ring_distance(cand.ring, local.ring);
            if back != 0 && (predecessor.is_none() || back < pred_dist) {
                pred_dist = back;
                predecessor = Some(cand.clone());
            }
        }

        Self {
            local,
            fingers,
            successor,
            predecessor,
        }
    }

    /// Build the table from an explicit, precomputed neighbor set
    /// rather than deriving it from the full peer list. This is the
    /// "disjoint discovery" counterpart to [`Self::build`]: a
    /// global-view planner runs the same selection [`Self::build`]
    /// would and ships each node only the neighbors it selected, so
    /// the node never needs global knowledge yet reconstructs an
    /// identical table (and therefore identical routing paths).
    ///
    /// `fingers` are the chosen finger peers (the local node must
    /// not be included; order is irrelevant since [`Self::next_hop`]
    /// scans them all). `successor`/`predecessor` are the immediate
    /// ring neighbors, `None` only for a single-node cluster. Unlike
    /// [`Self::build`], empty arcs are not materialized as self
    /// clones: only real fingers are stored, which the lookup path
    /// already handles.
    pub fn from_explicit(
        local: PeerEntry,
        fingers: Vec<PeerEntry>,
        successor: Option<PeerEntry>,
        predecessor: Option<PeerEntry>,
    ) -> Self {
        Self {
            local,
            fingers,
            successor,
            predecessor,
        }
    }

    pub fn local(&self) -> &PeerEntry {
        &self.local
    }

    pub fn k(&self) -> u32 {
        self.fingers.len() as u32
    }

    pub fn fingers(&self) -> &[PeerEntry] {
        &self.fingers
    }

    pub fn successor(&self) -> Option<&PeerEntry> {
        self.successor.as_ref()
    }

    pub fn predecessor(&self) -> Option<&PeerEntry> {
        self.predecessor.as_ref()
    }

    /// Closest-preceding-finger lookup, following the spirit of
    /// Chord with one deliberate deviation: the bound on a
    /// finger's forward distance is inclusive (`d <= limit`)
    /// rather than the textbook strict `d < limit`. The inclusive
    /// bound lets a finger that sits exactly on `target` itself
    /// be returned as the next hop, so when some node in the
    /// recursive chain holds a finger AT the target the lookup
    /// terminates at the destination directly instead of taking
    /// an extra hop to the strict predecessor.
    fn closest_preceding(&self, target: RingId) -> Option<&PeerEntry> {
        let limit = ring_distance(self.local.ring, target);
        if limit == 0 {
            return None;
        }
        let mut best: Option<&PeerEntry> = None;
        let mut best_dist: u64 = 0;
        for f in &self.fingers {
            if f.node == self.local.node {
                continue;
            }
            let d = ring_distance(self.local.ring, f.ring);
            if d == 0 || d > limit {
                continue;
            }
            if d > best_dist {
                best_dist = d;
                best = Some(f);
            }
        }
        best
    }

    /// Chord `find_successor`. Returns `None` when this node is
    /// the destination (owner of `target`); otherwise returns the
    /// peer to forward to.
    ///
    /// Termination rules, applied in order:
    /// 1. If we have a predecessor and `target` falls in
    ///    `(predecessor.ring, self.ring]`, we own it. Return None.
    /// 2. If we have a successor and `target` falls in
    ///    `(self.ring, successor.ring]`, the successor owns it.
    ///    Forward there; it will terminate.
    /// 3. Otherwise hop through the closest preceding finger.
    /// 4. Fallback: no finger is ahead; hand off to the successor
    ///    as a last attempt.
    ///
    /// For a single-node cluster (no successor, no predecessor)
    /// every call returns `None`: we are the only owner.
    pub fn next_hop(&self, target: RingId) -> Option<PeerEntry> {
        // Owner check: target in (predecessor, self].
        if target == self.local.ring {
            return None;
        }
        if let Some(pred) = &self.predecessor {
            let span = ring_distance(pred.ring, self.local.ring);
            let off = ring_distance(pred.ring, target);
            if off != 0 && off <= span {
                return None;
            }
        } else {
            // Single-node cluster: we own everything.
            return None;
        }

        // Successor-forward check: target in (self, successor].
        if let Some(succ) = &self.successor {
            let span = ring_distance(self.local.ring, succ.ring);
            let off = ring_distance(self.local.ring, target);
            if off != 0 && off <= span {
                return Some(succ.clone());
            }
        }

        // Route via finger table; fall back to successor.
        if let Some(hop) = self.closest_preceding(target) {
            return Some(hop.clone());
        }
        self.successor.clone()
    }
}

fn arc_index(local: RingId, candidate: RingId, arc_span: u64, k: u32) -> u32 {
    // Last arc absorbs the remainder so the partition is total
    // even when u64::MAX is not divisible by k.
    let d = ring_distance(local, candidate);
    let raw = d / arc_span;
    if raw >= k as u64 { k - 1 } else { raw as u32 }
}

fn better(
    local_ring: RingId,
    local_tags: &crate::p2p::types::TopologyTags,
    challenger: &PeerEntry,
    incumbent: &PeerEntry,
    arc: u32,
    topology: &TopologySelection,
) -> bool {
    match topology {
        TopologySelection::HardLocality => {
            hard_locality_better(local_ring, local_tags, challenger, incumbent, arc)
        }
        TopologySelection::Weighted(weighting) => weighted_better(
            local_ring, local_tags, challenger, incumbent, arc, weighting,
        ),
    }
}

fn hard_locality_better(
    local_ring: RingId,
    local_tags: &crate::p2p::types::TopologyTags,
    challenger: &PeerEntry,
    incumbent: &PeerEntry,
    arc: u32,
) -> bool {
    let c_topo = topology_distance(local_tags, &challenger.tags);
    let i_topo = topology_distance(local_tags, &incumbent.tags);
    if c_topo != i_topo {
        return c_topo < i_topo;
    }
    let c_rh = rendezvous_hash(local_ring, challenger.ring, arc);
    let i_rh = rendezvous_hash(local_ring, incumbent.ring, arc);
    if c_rh != i_rh {
        return c_rh < i_rh;
    }
    challenger.ring.0 < incumbent.ring.0
}

fn weighted_better(
    local_ring: RingId,
    local_tags: &crate::p2p::types::TopologyTags,
    challenger: &PeerEntry,
    incumbent: &PeerEntry,
    arc: u32,
    weighting: &TopologyWeighting,
) -> bool {
    let c = weighted_score(local_ring, local_tags, challenger, arc, weighting);
    let i = weighted_score(local_ring, local_tags, incumbent, arc, weighting);
    if c.score != i.score {
        return c.score < i.score;
    }
    if c.hash != i.hash {
        return c.hash < i.hash;
    }
    challenger.ring.0 < incumbent.ring.0
}

#[derive(Copy, Clone, Debug)]
struct WeightedScore {
    score: f64,
    hash: u64,
}

fn weighted_score(
    local_ring: RingId,
    local_tags: &crate::p2p::types::TopologyTags,
    peer: &PeerEntry,
    arc: u32,
    weighting: &TopologyWeighting,
) -> WeightedScore {
    let hash = rendezvous_hash(local_ring, peer.ring, arc);
    let base = rendezvous_unit(hash);
    let prefix_len = shared_prefix_len(local_tags, &peer.tags);
    let weight = matching_weight(prefix_len, weighting);
    WeightedScore {
        score: base - weight,
        hash,
    }
}

fn rendezvous_unit(hash: u64) -> f64 {
    const DENOMINATOR: f64 = (1u64 << 53) as f64;
    ((hash >> 11) as f64) / DENOMINATOR
}

fn matching_weight(prefix_len: usize, weighting: &TopologyWeighting) -> f64 {
    weighting
        .prefix_weights
        .iter()
        .filter(|weight| (weight.tag_index as usize) < prefix_len)
        .max_by_key(|weight| weight.tag_index)
        .map(|weight| {
            if weight.weight.is_finite() {
                weight.weight
            } else {
                0.0
            }
        })
        .unwrap_or(0.0)
}

fn shared_prefix_len(
    local: &crate::p2p::types::TopologyTags,
    peer: &crate::p2p::types::TopologyTags,
) -> usize {
    let mut prefix = 0;
    for (l, p) in local.0.iter().zip(peer.0.iter()) {
        if l == WILDCARD_TAG || p == WILDCARD_TAG || l != p {
            break;
        }
        prefix += 1;
    }
    prefix
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::p2p::types::{NodeId, TopologyTags};

    fn peer(node: u64, ring: u64, parts: &[&str]) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: RingId(ring),
            tags: TopologyTags(parts.iter().map(|s| s.to_string()).collect()),
        }
    }

    #[test]
    fn single_node_ring_has_no_next_hop() {
        let me = peer(0, 0, &["r", "z", "row", "rack"]);
        let ft = FingerTable::build(me.clone(), &[], FingerTableConfig::default());
        assert_eq!(ft.k(), 100);
        assert!(ft.successor().is_none());
        assert!(ft.predecessor().is_none());
        for f in ft.fingers() {
            assert_eq!(f.node, me.node);
        }
        for t in [0u64, 1, 12345, u64::MAX / 2, u64::MAX] {
            assert!(ft.next_hop(RingId(t)).is_none());
        }
    }

    #[test]
    fn two_node_ring_routes_to_other_node() {
        let me = peer(1, 0, &["r", "z", "row", "rack"]);
        let other = peer(2, u64::MAX / 2, &["r", "z", "row", "rack"]);
        let ft_me = FingerTable::build(me.clone(), &[other.clone()], FingerTableConfig::with_k(8));
        let ft_other =
            FingerTable::build(other.clone(), &[me.clone()], FingerTableConfig::with_k(8));

        // Both nodes see each other as both successor and predecessor.
        assert_eq!(ft_me.successor().unwrap().node, other.node);
        assert_eq!(ft_me.predecessor().unwrap().node, other.node);

        // Target = other's ring id from me: me forwards to other.
        let hop = ft_me.next_hop(other.ring).expect("forward to other");
        assert_eq!(hop.node, other.node);
        // Then other owns it.
        assert!(ft_other.next_hop(other.ring).is_none());

        // Target = our own ring id: no next hop (we own it).
        assert!(ft_me.next_hop(me.ring).is_none());

        // A target between us forward-of-me belongs to other.
        let mid = RingId(u64::MAX / 4);
        let hop = ft_me.next_hop(mid).expect("forward to other");
        assert_eq!(hop.node, other.node);
        // From other's side, mid is in (predecessor=me, self=other],
        // so other owns it and terminates.
        assert!(ft_other.next_hop(mid).is_none());

        // A target past other (in the wraparound arc back to me)
        // belongs to me. From other, me is the successor; forward.
        let wrap_target = RingId(u64::MAX - 1000);
        let hop = ft_other.next_hop(wrap_target).expect("forward to me");
        assert_eq!(hop.node, me.node);
        assert!(ft_me.next_hop(wrap_target).is_none());
    }

    #[test]
    fn successor_is_owner_lookup_terminates_in_two_hops() {
        // Three-node ring: lookup from the predecessor of the
        // owner should forward straight to the successor, which
        // then terminates.
        let a = peer(1, 0, &["r"]);
        let b = peer(2, u64::MAX / 3, &["r"]);
        let c = peer(3, (u64::MAX / 3) * 2, &["r"]);
        let peers = vec![a.clone(), b.clone(), c.clone()];
        let ft_a = FingerTable::build(a.clone(), &peers, FingerTableConfig::with_k(8));
        let ft_b = FingerTable::build(b.clone(), &peers, FingerTableConfig::with_k(8));

        // a's successor is b. Lookup for target=b.ring from a:
        // a sees target in (a, b] and forwards to b; b owns it.
        let hop = ft_a.next_hop(b.ring).expect("a forwards to b");
        assert_eq!(hop.node, b.node);
        assert!(ft_b.next_hop(b.ring).is_none());
    }

    #[test]
    fn target_at_successor_ring_routes_to_successor() {
        // Edge case: start has a successor whose ring id == target.
        let a = peer(1, 100, &["r"]);
        let b = peer(2, 200, &["r"]);
        let c = peer(3, 300, &["r"]);
        let peers = vec![a.clone(), b.clone(), c.clone()];
        let ft_a = FingerTable::build(a.clone(), &peers, FingerTableConfig::with_k(8));
        assert_eq!(ft_a.successor().unwrap().node, b.node);
        let hop = ft_a.next_hop(b.ring).expect("forward to successor");
        assert_eq!(hop.node, b.node);
    }

    #[test]
    fn build_is_deterministic() {
        let me = peer(0, 100, &["us", "z1", "r1", "k1"]);
        let peers = vec![
            peer(1, 200, &["us", "z1", "r1", "k2"]),
            peer(2, 300, &["us", "z1", "r2", "k1"]),
            peer(3, 400, &["us", "z2", "r1", "k1"]),
            peer(4, u64::MAX - 1000, &["eu", "z1", "r1", "k1"]),
        ];
        let cfg = FingerTableConfig::with_k(16);
        let a = FingerTable::build(me.clone(), &peers, cfg.clone());
        let b = FingerTable::build(me.clone(), &peers, cfg);
        let a_ids: Vec<_> = a.fingers().iter().map(|p| (p.node, p.ring)).collect();
        let b_ids: Vec<_> = b.fingers().iter().map(|p| (p.node, p.ring)).collect();
        assert_eq!(a_ids, b_ids);
        assert_eq!(a.successor().map(|p| p.node), b.successor().map(|p| p.node));
        assert_eq!(
            a.predecessor().map(|p| p.node),
            b.predecessor().map(|p| p.node)
        );
    }

    #[test]
    fn topology_preference_beats_random_tiebreak() {
        let me = peer(0, 0, &["us", "z1", "r1", "k1"]);
        let span = u64::MAX / 4;
        let close_topo = peer(1, span + 100, &["us", "z1", "r1", "k1"]);
        let far_topo = peer(2, span + 200, &["us", "z2", "rX", "kY"]);
        let ft = FingerTable::build(
            me.clone(),
            &[close_topo.clone(), far_topo.clone()],
            FingerTableConfig::with_k(4),
        );
        let arc = arc_index(me.ring, close_topo.ring, span, 4);
        assert_eq!(ft.fingers()[arc as usize].node, close_topo.node);
    }

    fn weighted_cfg(k: u32, weights: &[(u32, f64)]) -> FingerTableConfig {
        FingerTableConfig {
            k,
            topology: TopologySelection::Weighted(TopologyWeighting {
                prefix_weights: weights
                    .iter()
                    .map(|(tag_index, weight)| TopologyPrefixWeight {
                        tag_index: *tag_index,
                        weight: *weight,
                    })
                    .collect(),
            }),
        }
    }

    fn same_arc_pair_with_hash_order(far_hash_wins: bool) -> (PeerEntry, PeerEntry, u32) {
        let me = peer(0, 0, &["us", "z1"]);
        let k = 4;
        let span = u64::MAX / k;
        for offset in 1..10_000u64 {
            let close = peer(1, span + offset, &["us", "z1"]);
            let far = peer(2, span + 10_000 + offset, &["eu", "z9"]);
            let arc = arc_index(me.ring, close.ring, span, k as u32);
            assert_eq!(arc, arc_index(me.ring, far.ring, span, k as u32));
            let close_hash = rendezvous_hash(me.ring, close.ring, arc);
            let far_hash = rendezvous_hash(me.ring, far.ring, arc);
            if (far_hash < close_hash) == far_hash_wins {
                return (close, far, arc);
            }
        }
        panic!("could not find same-arc candidates with requested hash order");
    }

    #[test]
    fn weighted_neutral_uses_rendezvous_hash() {
        let me = peer(0, 0, &["us", "z1"]);
        let (close, far, arc) = same_arc_pair_with_hash_order(true);
        let ft = FingerTable::build(me, &[close.clone(), far.clone()], weighted_cfg(4, &[]));

        assert_eq!(ft.fingers()[arc as usize].node, far.node);
    }

    #[test]
    fn weighted_positive_prefix_weight_can_favor_local_peer() {
        let me = peer(0, 0, &["us", "z1"]);
        let (close, far, arc) = same_arc_pair_with_hash_order(true);
        let ft = FingerTable::build(
            me,
            &[close.clone(), far.clone()],
            weighted_cfg(4, &[(1, 1.0)]),
        );

        assert_eq!(ft.fingers()[arc as usize].node, close.node);
    }

    #[test]
    fn weighted_negative_prefix_weight_can_penalize_local_peer() {
        let me = peer(0, 0, &["us", "z1"]);
        let (close, far, arc) = same_arc_pair_with_hash_order(false);
        let ft = FingerTable::build(
            me,
            &[close.clone(), far.clone()],
            weighted_cfg(4, &[(1, -1.0)]),
        );

        assert_eq!(ft.fingers()[arc as usize].node, far.node);
    }

    #[test]
    fn most_specific_weight_wins() {
        let me = peer(0, 0, &["us", "z1"]);
        let same_az = peer(1, 100, &["us", "z1"]);
        let same_region = peer(2, 200, &["us", "z2"]);

        let weighting = TopologyWeighting {
            prefix_weights: vec![
                TopologyPrefixWeight {
                    tag_index: 0,
                    weight: 0.25,
                },
                TopologyPrefixWeight {
                    tag_index: 1,
                    weight: -0.5,
                },
            ],
        };

        assert_eq!(
            matching_weight(shared_prefix_len(&me.tags, &same_region.tags), &weighting),
            0.25
        );
        assert_eq!(
            matching_weight(shared_prefix_len(&me.tags, &same_az.tags), &weighting),
            -0.5
        );
    }

    #[test]
    fn build_is_order_invariant() {
        use rand::SeedableRng;
        use rand::seq::SliceRandom;
        use rand_chacha::ChaCha8Rng;

        let me = peer(0, 1 << 32, &["us", "z1", "r1", "k1"]);
        let topo_groups: &[&[&str]] = &[
            &["us", "z1", "r1", "k1"],
            &["us", "z1", "r1", "k2"],
            &["us", "z1", "r2", "k1"],
            &["us", "z2", "r1", "k1"],
            &["eu", "z1", "r1", "k1"],
            &["eu", "z2", "r3", "k7"],
            &["ap", "z1", "r1", "k1"],
        ];
        let mut peers: Vec<PeerEntry> = Vec::new();
        for i in 1u64..=20 {
            let labels = topo_groups[(i as usize) % topo_groups.len()];
            let ring = i.wrapping_mul(0x9E37_79B9_7F4A_7C15);
            peers.push(peer(i, ring, labels));
        }
        peers.push(peer(
            99,
            peers[3].ring.0.wrapping_add(7),
            &["us", "z1", "r2", "k1"],
        ));

        let cfg = FingerTableConfig::with_k(8);
        let base = FingerTable::build(me.clone(), &peers, cfg.clone());

        for seed in [1u64, 2, 17, 12345] {
            let mut rng = ChaCha8Rng::seed_from_u64(seed);
            let mut shuffled = peers.clone();
            shuffled.shuffle(&mut rng);
            let other = FingerTable::build(me.clone(), &shuffled, cfg.clone());
            assert_eq!(base.k(), other.k(), "seed={seed}");
            assert_eq!(base.local().node, other.local().node, "seed={seed}");
            assert_eq!(base.local().ring, other.local().ring, "seed={seed}");
            assert_eq!(base.fingers().len(), other.fingers().len(), "seed={seed}");
            for (i, (a, b)) in base.fingers().iter().zip(other.fingers()).enumerate() {
                assert_eq!(a.node, b.node, "seed={seed} arc={i}");
                assert_eq!(a.ring, b.ring, "seed={seed} arc={i}");
                assert_eq!(a.tags.0, b.tags.0, "seed={seed} arc={i}");
            }
            assert_eq!(
                base.successor().map(|p| p.node),
                other.successor().map(|p| p.node),
                "seed={seed}"
            );
            assert_eq!(
                base.predecessor().map(|p| p.node),
                other.predecessor().map(|p| p.node),
                "seed={seed}"
            );
        }
    }

    #[test]
    fn weighted_build_is_order_invariant() {
        use rand::SeedableRng;
        use rand::seq::SliceRandom;
        use rand_chacha::ChaCha8Rng;

        let me = peer(0, 1 << 32, &["us", "z1", "r1", "k1"]);
        let topo_groups: &[&[&str]] = &[
            &["us", "z1", "r1", "k1"],
            &["us", "z1", "r1", "k2"],
            &["us", "z1", "r2", "k1"],
            &["us", "z2", "r1", "k1"],
            &["eu", "z1", "r1", "k1"],
            &["ap", "z1", "r1", "k1"],
        ];
        let mut peers: Vec<PeerEntry> = Vec::new();
        for i in 1u64..=30 {
            let labels = topo_groups[(i as usize) % topo_groups.len()];
            let ring = i.wrapping_mul(0x9E37_79B9_7F4A_7C15);
            peers.push(peer(i, ring, labels));
        }

        let cfg = weighted_cfg(8, &[(0, 0.15), (1, -0.2), (2, 0.05)]);
        let base = FingerTable::build(me.clone(), &peers, cfg.clone());

        for seed in [1u64, 2, 17, 12345] {
            let mut rng = ChaCha8Rng::seed_from_u64(seed);
            let mut shuffled = peers.clone();
            shuffled.shuffle(&mut rng);
            let other = FingerTable::build(me.clone(), &shuffled, cfg.clone());
            for (i, (a, b)) in base.fingers().iter().zip(other.fingers()).enumerate() {
                assert_eq!(a.node, b.node, "seed={seed} arc={i}");
                assert_eq!(a.ring, b.ring, "seed={seed} arc={i}");
            }
            assert_eq!(
                base.successor().map(|p| p.node),
                other.successor().map(|p| p.node),
                "seed={seed}"
            );
            assert_eq!(
                base.predecessor().map(|p| p.node),
                other.predecessor().map(|p| p.node),
                "seed={seed}"
            );
        }
    }

    #[test]
    fn no_self_loop_in_next_hop() {
        let me = peer(0, 12345, &["us", "z1", "r1", "k1"]);
        let peers = vec![
            peer(1, 22345, &["us", "z1", "r1", "k2"]),
            peer(2, 32345, &["us", "z1", "r2", "k1"]),
            peer(3, 42345, &["us", "z2", "r1", "k1"]),
        ];
        let ft = FingerTable::build(me.clone(), &peers, FingerTableConfig::with_k(32));
        for t in [0u64, 1, 12345, 22345, 99999, u64::MAX] {
            if let Some(p) = ft.next_hop(RingId(t)) {
                assert_ne!(p.node, me.node, "self loop at target={t}");
            }
        }
    }

    // Collect a built table's selected neighbors the way a global-view
    // planner would: the distinct non-self finger peers, plus the
    // successor and predecessor. Feeding these back into
    // `from_explicit` must reproduce the table's routing exactly.
    fn explicit_from_built(ft: &FingerTable) -> FingerTable {
        let mut fingers: Vec<PeerEntry> = Vec::new();
        for f in ft.fingers() {
            if f.node == ft.local().node {
                continue;
            }
            if !fingers.iter().any(|e| e.node == f.node) {
                fingers.push(f.clone());
            }
        }
        FingerTable::from_explicit(
            ft.local().clone(),
            fingers,
            ft.successor().cloned(),
            ft.predecessor().cloned(),
        )
    }

    #[test]
    fn from_explicit_reproduces_built_routing() {
        // A node configured with only its selected neighbors must
        // route every target identically to the globally-built table.
        let me = peer(0, 1 << 40, &["us", "z1", "r1", "k1"]);
        let topo_groups: &[&[&str]] = &[
            &["us", "z1", "r1", "k1"],
            &["us", "z1", "r2", "k3"],
            &["us", "z2", "r1", "k1"],
            &["eu", "z1", "r1", "k1"],
            &["ap", "z3", "r2", "k4"],
        ];
        let mut peers: Vec<PeerEntry> = Vec::new();
        for i in 1u64..=40 {
            let labels = topo_groups[(i as usize) % topo_groups.len()];
            let ring = i.wrapping_mul(0x9E37_79B9_7F4A_7C15);
            peers.push(peer(i, ring, labels));
        }

        let built = FingerTable::build(me.clone(), &peers, FingerTableConfig::with_k(16));
        let explicit = explicit_from_built(&built);

        assert_eq!(
            built.successor().map(|p| p.node),
            explicit.successor().map(|p| p.node)
        );
        assert_eq!(
            built.predecessor().map(|p| p.node),
            explicit.predecessor().map(|p| p.node)
        );

        for t in 0u64..2000 {
            let target = RingId(t.wrapping_mul(0x1234_5678_9ABC_DEF1));
            assert_eq!(
                built.next_hop(target).map(|p| p.node),
                explicit.next_hop(target).map(|p| p.node),
                "next_hop diverged at target={}",
                target.0
            );
        }
    }

    #[test]
    fn from_explicit_reproduces_weighted_built_routing() {
        // Weighted topology still only affects build-time finger selection.
        // Once the selected neighbors are made explicit, runtime routing must
        // be byte-for-byte equivalent to the globally-built table.
        let me = peer(0, 1 << 40, &["us", "z1", "r1", "k1"]);
        let topo_groups: &[&[&str]] = &[
            &["us", "z1", "r1", "k1"],
            &["us", "z1", "r2", "k3"],
            &["us", "z2", "r1", "k1"],
            &["eu", "z1", "r1", "k1"],
            &["ap", "z3", "r2", "k4"],
        ];
        let mut peers: Vec<PeerEntry> = Vec::new();
        for i in 1u64..=40 {
            let labels = topo_groups[(i as usize) % topo_groups.len()];
            let ring = i.wrapping_mul(0x9E37_79B9_7F4A_7C15);
            peers.push(peer(i, ring, labels));
        }

        let built = FingerTable::build(
            me.clone(),
            &peers,
            weighted_cfg(16, &[(0, 0.2), (1, -0.35), (2, 0.1)]),
        );
        let explicit = explicit_from_built(&built);

        assert_eq!(
            built.successor().map(|p| p.node),
            explicit.successor().map(|p| p.node)
        );
        assert_eq!(
            built.predecessor().map(|p| p.node),
            explicit.predecessor().map(|p| p.node)
        );

        for t in 0u64..2000 {
            let target = RingId(t.wrapping_mul(0x1234_5678_9ABC_DEF1));
            assert_eq!(
                built.next_hop(target).map(|p| p.node),
                explicit.next_hop(target).map(|p| p.node),
                "next_hop diverged at target={}",
                target.0
            );
        }
    }

    #[test]
    fn from_explicit_single_node_owns_everything() {
        let me = peer(7, 999, &["r"]);
        let ft = FingerTable::from_explicit(me.clone(), Vec::new(), None, None);
        assert!(ft.successor().is_none());
        assert!(ft.predecessor().is_none());
        for t in [0u64, 1, 999, u64::MAX / 3, u64::MAX] {
            assert!(ft.next_hop(RingId(t)).is_none());
        }
    }

    #[test]
    fn from_explicit_forwards_to_configured_neighbors() {
        // Two-node ring expressed purely through an explicit plan.
        let me = peer(1, 0, &["r"]);
        let other = peer(2, u64::MAX / 2, &["r"]);
        let ft = FingerTable::from_explicit(
            me.clone(),
            vec![other.clone()],
            Some(other.clone()),
            Some(other.clone()),
        );
        // We own our own ring id.
        assert!(ft.next_hop(me.ring).is_none());
        // A forward target routes to the configured successor.
        let hop = ft.next_hop(RingId(u64::MAX / 4)).expect("forward to other");
        assert_eq!(hop.node, other.node);
        // next_hop only ever returns a configured neighbor.
        for t in [1u64, 100, u64::MAX / 2, u64::MAX - 1] {
            if let Some(p) = ft.next_hop(RingId(t)) {
                assert_eq!(p.node, other.node);
            }
        }
    }
}
