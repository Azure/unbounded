//! Versioned union of incoming/outgoing (radix*i+j)%N edges, excluding self and duplicates.
use super::{RoutingAlgorithm, membership::MembershipLease};
use crate::{error::Result, model::NodeId};
pub struct Graph {
    membership: MembershipLease,
    algorithm: RoutingAlgorithm,
}
impl Graph {
    pub fn new(membership: MembershipLease) -> Self {
        Self::with_algorithm(membership, RoutingAlgorithm::default())
    }
    pub fn with_algorithm(membership: MembershipLease, algorithm: RoutingAlgorithm) -> Self {
        Self {
            membership,
            algorithm,
        }
    }
    pub fn neighbors(&self, node: &NodeId) -> Result<Vec<NodeId>> {
        let position = self.membership.position(node)?;
        Ok(
            neighbor_positions_for(self.membership.members().len(), position, self.algorithm)
                .into_iter()
                .map(|index| self.membership.members()[index].node.clone())
                .collect(),
        )
    }
}

/// Explicit legacy graph for the V2 search and its interoperability fixtures.
pub(super) fn neighbor_positions(count: usize, node: usize) -> Vec<usize> {
    neighbor_positions_for(count, node, RoutingAlgorithm::V2)
}

/// The incoming intervals are disjoint lifts of `node` modulo N. Their quotient
/// by the radix gives every predecessor without scanning any other member.
pub(super) fn neighbor_positions_for(
    count: usize,
    node: usize,
    algorithm: RoutingAlgorithm,
) -> Vec<usize> {
    debug_assert!(node < count);
    let radix = algorithm.radix();
    let mut neighbors = Vec::with_capacity(algorithm.max_degree());
    for digit in 0..radix {
        neighbors.push((radix * node + digit) % count);
        neighbors.push((node + digit * count) / radix);
    }
    neighbors.sort_unstable();
    neighbors.dedup();
    neighbors.retain(|other| *other != node);
    neighbors
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::topology::fixtures::membership;

    #[test]
    fn exhaustive_inverse_matches_definition() {
        for algorithm in [
            RoutingAlgorithm::V2,
            RoutingAlgorithm::V3,
            RoutingAlgorithm::V4,
            RoutingAlgorithm::V5,
        ] {
            let radix = algorithm.radix();
            for n in 1..=160 {
                for i in 0..n {
                    let expected: Vec<_> = (0..n)
                        .filter(|&k| {
                            k != i
                                && (0..radix)
                                    .any(|j| (radix * i + j) % n == k || (radix * k + j) % n == i)
                        })
                        .collect();
                    assert_eq!(
                        neighbor_positions_for(n, i, algorithm),
                        expected,
                        "N={n}, i={i}, {algorithm:?}"
                    );
                }
            }
        }
    }

    #[test]
    fn hundred_thousand_nodes_bounded_symmetric_and_four_link_reachable() {
        for algorithm in [RoutingAlgorithm::V2, RoutingAlgorithm::V5] {
            let n = 100_000;
            for i in 0..n {
                let neighbors = neighbor_positions_for(n, i, algorithm);
                assert!(neighbors.len() <= algorithm.max_degree());
                for &other in &neighbors {
                    assert!(
                        neighbor_positions_for(n, other, algorithm)
                            .binary_search(&i)
                            .is_ok()
                    );
                }
            }
            // All destinations from representative sources, not just random pairs.
            for source in [0, 1, 17, 49_999, 99_999] {
                let mut distance = vec![u8::MAX; n];
                distance[source] = 0;
                let mut queue = std::collections::VecDeque::from([source]);
                while let Some(i) = queue.pop_front() {
                    if distance[i] == 4 {
                        continue;
                    }
                    for j in neighbor_positions_for(n, i, algorithm) {
                        if distance[j] == u8::MAX {
                            distance[j] = distance[i] + 1;
                            queue.push_back(j);
                        }
                    }
                }
                assert!(distance.iter().all(|d| *d <= 4));
            }
        }
    }

    #[test]
    fn rejects_unknown_and_empty_membership() {
        let graph = Graph::new(membership(0));
        assert!(graph.neighbors(&NodeId("unknown".into())).is_err());
        let one = membership(1);
        assert!(
            Graph::new(one.clone())
                .neighbors(&one.members()[0].node)
                .unwrap()
                .is_empty()
        );
    }
}
