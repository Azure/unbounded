//! Stable overlay: union of 32 independently domain-separated SHA-256 rings.
//!
//! Each ring orders frozen IDs by (digest, ID), then joins consecutive members
//! including the wraparound edge. One ring connects every nonempty membership;
//! their union is symmetric, self-free, and has at most 64 neighbors per member.
//! Adding/removing one ID changes only its predecessor/successor edges in each
//! ring, regardless of how membership positions shift. Weights play no role.
//!
//! Construction hashes each ID 32 times and sorts 32 arrays of N entries:
//! O(32 * (total ID bytes + N log N) + 64 N log 64) time. Retained adjacency
//! uses at most 64 N usize slots plus N Vec headers; scratch is one reused
//! N-entry array of ([u8; 32], usize), with no retained ring arrays.
use crate::{Member, hash};
use sha2::Digest;

pub(crate) const RINGS: u32 = 32;
const RING_DOMAIN: &[u8] = b"/overlay/sha256-rings-32/v1\0";
const IDENTITY_DOMAIN: &[u8] = b"/topology-identity/sha256-rings-32/v1\0";

pub(crate) fn identity<M: Member>(ids: &[Box<[u8]>]) -> [u8; 32] {
    let mut digest = hash::domain::<M>(IDENTITY_DOMAIN);
    for id in ids {
        digest.update((id.len() as u64).to_be_bytes());
        digest.update(id);
    }
    hash::finish(digest)
}

pub(crate) fn build<M: Member>(ids: &[Box<[u8]>]) -> Vec<Vec<usize>> {
    let mut graph: Vec<_> = (0..ids.len())
        .map(|_| Vec::with_capacity(if ids.len() > 1 { 2 * RINGS as usize } else { 0 }))
        .collect();
    if ids.len() < 2 {
        return graph;
    }
    let mut order = Vec::with_capacity(ids.len());
    for ring in 0..RINGS {
        order.clear();
        let mut prefix = hash::domain::<M>(RING_DOMAIN);
        prefix.update(ring.to_be_bytes());
        for (position, id) in ids.iter().enumerate() {
            let mut digest = prefix.clone();
            digest.update((id.len() as u64).to_be_bytes());
            digest.update(id);
            order.push((hash::finish(digest), position));
        }
        // Positions are in frozen ID order, so they break digest ties by ID
        // without making edge selection depend on the membership's size.
        order.sort_unstable();
        for index in 0..order.len() {
            let a = order[index].1;
            let b = order[(index + 1) % order.len()].1;
            graph[a].push(b);
            graph[b].push(a);
        }
    }
    for neighbors in &mut graph {
        neighbors.sort_unstable();
        neighbors.dedup();
    }
    graph
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Membership;
    use std::{collections::BTreeSet, num::NonZeroU32};

    #[derive(Clone, Debug)]
    struct Node([u8; 4], NonZeroU32);
    impl Member for Node {
        const DOMAIN: &'static str = "overlay-tests";
        fn id(&self) -> &[u8] {
            &self.0
        }
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }
    fn node(id: u32) -> Node {
        Node(id.to_be_bytes(), NonZeroU32::new(1).unwrap())
    }
    fn membership(count: u32) -> Membership<Node> {
        Membership::new((0..count).map(|id| node(2 * id)).collect()).unwrap()
    }
    fn edges(members: &Membership<Node>) -> BTreeSet<(Vec<u8>, Vec<u8>)> {
        let mut edges = BTreeSet::new();
        for position in 0..members.members().len() {
            for &other in members.neighbor_slice(position) {
                if position < other {
                    edges.insert((members.id(position).to_vec(), members.id(other).to_vec()));
                }
            }
        }
        edges
    }

    #[test]
    fn symmetric_connected_bounded_and_deterministic() {
        for count in [0, 1, 2, 3, 32, 65, 257, 1024] {
            let members = membership(count);
            let mut permuted = members.members().to_vec();
            permuted.reverse();
            for member in &mut permuted {
                member.1 = NonZeroU32::new(u32::MAX).unwrap();
            }
            let reordered = Membership::new(permuted).unwrap();
            assert_eq!(members.graph(), reordered.graph());
            assert_eq!(members.topology_identity(), reordered.topology_identity());
            for position in 0..count as usize {
                let neighbors = members.neighbor_slice(position);
                assert_eq!(members.neighbors(position), neighbors);
                assert!(neighbors.len() <= crate::MAX_DEGREE);
                assert!(neighbors.windows(2).all(|pair| pair[0] < pair[1]));
                assert!(!neighbors.contains(&position));
                for &other in neighbors {
                    assert!(other < count as usize);
                    assert!(
                        members
                            .neighbor_slice(other)
                            .binary_search(&position)
                            .is_ok()
                    );
                }
            }
            let mut seen = BTreeSet::new();
            let mut pending = if count == 0 { vec![] } else { vec![0] };
            while let Some(position) = pending.pop() {
                if seen.insert(position) {
                    pending.extend(members.neighbor_slice(position));
                }
            }
            assert_eq!(seen.len(), count as usize);
            assert!(members.neighbors(count as usize).is_empty());
            assert!(members.neighbor_slice(usize::MAX).is_empty());
            assert!(members.neighbors(usize::MAX).is_empty());
        }
    }

    #[test]
    fn joins_and_leaves_change_only_local_id_edges() {
        let old = membership(512);
        let before = edges(&old);
        // Insert near the beginning, middle, and end of ID order. Comparing
        // positions would hide churn caused by shifted membership indices.
        for id in [1, 511, 1023] {
            let mut values = old.members().to_vec();
            values.push(node(id));
            let joined = Membership::new(values).unwrap();
            let after = edges(&joined);
            assert!(before.symmetric_difference(&after).count() <= 3 * RINGS as usize);
            let removed: Vec<_> = before.difference(&after).collect();
            assert!(removed.len() <= RINGS as usize);
            let added: Vec<_> = after.difference(&before).collect();
            assert!(added.len() <= 2 * RINGS as usize);
            assert!(
                added
                    .iter()
                    .all(|(a, b)| a.as_slice() == id.to_be_bytes()
                        || b.as_slice() == id.to_be_bytes())
            );
            let remaining = Membership::new(
                joined
                    .members()
                    .iter()
                    .filter(|member| member.id() != id.to_be_bytes())
                    .cloned()
                    .collect(),
            )
            .unwrap();
            assert_eq!(edges(&remaining), before);
        }
        // Removing an existing member has the same local bound, not merely
        // reverting a just-added ID.
        for id in [0u32, 512, 1022] {
            let remaining = Membership::new(
                old.members()
                    .iter()
                    .filter(|member| member.id() != id.to_be_bytes())
                    .cloned()
                    .collect(),
            )
            .unwrap();
            assert!(before.symmetric_difference(&edges(&remaining)).count() <= 3 * RINGS as usize);
        }
    }
}
