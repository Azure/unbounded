//! Union of incoming/outgoing (18*i+j)%N edges, excluding self and duplicates.
use super::membership::MembershipLease;
use crate::{
    error::{Result, pending},
    model::identity::NodeId,
};
pub struct Graph {
    membership: MembershipLease,
}
impl Graph {
    pub fn new(membership: MembershipLease) -> Self {
        Self { membership }
    }
    pub fn neighbors(&self, _node: &NodeId) -> Result<Vec<NodeId>> {
        pending("graph.neighbors")
    }
}
#[cfg(test)]
mod tests { /* Exact small graphs, <=36 neighbors, and four-link reachability at 100k. */
}
