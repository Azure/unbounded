//! Deterministic bounded shortest-path search; no all-pairs table.
use super::{health::LinkHealth, membership::MembershipLease};
use crate::{
    error::{Result, pending},
    model::identity::{AttemptId, NodeId, RequestId},
    runtime::deadline::Deadline,
};
use std::rc::Rc;

pub struct Route {
    pub membership: MembershipLease,
    pub nodes: Vec<NodeId>,
}
/// Signed forwarding state. Retries preserve deadline and consumed link budget.
pub struct RouteBudget {
    pub membership: crate::model::identity::MembershipVersion,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub destination: NodeId,
    pub visited: Vec<NodeId>,
    pub remaining_links: u8,
    pub deadline: Deadline,
}
pub struct Paths {
    health: Rc<LinkHealth>,
    capacity: usize,
    search_work: usize,
}
impl Paths {
    pub fn new(health: Rc<LinkHealth>, capacity: usize, search_work: usize) -> Self {
        Self {
            health,
            capacity,
            search_work,
        }
    }
    pub fn shortest(
        &self,
        _membership: MembershipLease,
        _from: &NodeId,
        _budget: &RouteBudget,
    ) -> Result<Route> {
        pending("paths.shortest")
    }
}
#[cfg(test)]
mod tests { /* Ties, partitions, loops, old snapshots, and inherited 4/8-link budgets. */
}
