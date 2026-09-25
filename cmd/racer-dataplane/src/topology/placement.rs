//! Canonical slot encoding and weighted rendezvous ranking of up to three nodes.
//!
//! Freeze hash, integer arithmetic, and tie vectors before interoperability work.
//! Coalesce O(N) cold rankings under a CPU budget and cache by membership version.
use super::membership::MembershipLease;
use crate::{
    error::{Result, pending},
    model::identity::{NodeId, ObjectId, PageNumber},
};
pub const SLOT_COUNT: u32 = 1 << 20;
pub struct Placement {
    capacity: usize,
}
pub struct Candidates {
    pub membership: MembershipLease,
    pub ordered: Vec<NodeId>,
}
impl Placement {
    pub fn new(capacity: usize) -> Self {
        Self { capacity }
    }
    pub fn rank(
        &self,
        _membership: MembershipLease,
        _object: &ObjectId,
        _page: PageNumber,
    ) -> Result<Candidates> {
        pending("placement.rank")
    }
}
#[cfg(test)]
mod tests { /* Golden vectors, ties, weighted distribution, churn, and 0/1/2 nodes. */
}
