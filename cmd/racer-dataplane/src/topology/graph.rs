//! Union of incoming/outgoing (18*i+j)%N edges, excluding self and duplicates.
use super::membership::MembershipLease;
use crate::{error::Result, model::identity::NodeId};
pub struct Graph {
    membership: MembershipLease,
}
impl Graph {
    pub fn new(membership: MembershipLease) -> Self {
        Self { membership }
    }
    pub fn neighbors(&self, node: &NodeId) -> Result<Vec<NodeId>> {
        let position = self.membership.position(node)?;
        Ok(
            neighbor_positions(self.membership.members().len(), position)
                .into_iter()
                .map(|index| self.membership.members()[index].node.clone())
                .collect(),
        )
    }
}

pub const RADIX: usize = 18;

/// The incoming intervals are disjoint lifts of `node` modulo N. Their quotient
/// by 18 gives every predecessor without scanning any other member.
pub(super) fn neighbor_positions(count: usize, node: usize) -> Vec<usize> {
    debug_assert!(node < count);
    let mut neighbors = Vec::with_capacity(2 * RADIX);
    for digit in 0..RADIX {
        neighbors.push((RADIX * node + digit) % count);
        neighbors.push((node + digit * count) / RADIX);
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
        for n in 1..=160 {
            for i in 0..n {
                let expected: Vec<_> = (0..n)
                    .filter(|&k| {
                        k != i
                            && (0..RADIX)
                                .any(|j| (RADIX * i + j) % n == k || (RADIX * k + j) % n == i)
                    })
                    .collect();
                assert_eq!(neighbor_positions(n, i), expected, "N={n}, i={i}");
            }
        }
    }

    #[test]
    fn hundred_thousand_nodes_bounded_symmetric_and_four_link_reachable() {
        let n = 100_000;
        for i in 0..n {
            let neighbors = neighbor_positions(n, i);
            assert!(neighbors.len() <= 36);
            for &other in &neighbors {
                assert!(neighbor_positions(n, other).binary_search(&i).is_ok());
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
                for j in neighbor_positions(n, i) {
                    if distance[j] == u8::MAX {
                        distance[j] = distance[i] + 1;
                        queue.push_back(j);
                    }
                }
            }
            assert!(distance.iter().all(|d| *d <= 4));
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
