//! Stable sorted node identities and immutable leased membership versions.
//!
//! Readiness never changes ownership. Exclusions, weights, additions, and deletions
//! do. Retain bounded old snapshots until their in-flight leases are released.
use super::rails::RailMapping;
use crate::{
    error::{Result, pending},
    model::identity::{MembershipVersion, NodeId},
};
use std::{num::NonZeroU32, sync::Arc};

#[derive(Clone, Debug)]
pub struct Member {
    pub node: NodeId,
    pub shares: NonZeroU32,
    pub peer_endpoint: String,
    pub rails: Vec<RailMapping>,
    pub alignment_enabled: bool,
}
pub struct Membership {
    pub version: MembershipVersion,
    members: Vec<Member>,
}
pub type MembershipLease = Arc<Membership>;
impl Membership {
    pub fn validate(_version: MembershipVersion, _members: Vec<Member>) -> Result<Self> {
        pending("membership.validate")
    }
    pub fn members(&self) -> &[Member] {
        &self.members
    }
}
#[cfg(test)]
mod tests { /* Cover stable ordering, duplicates, exclusion, weights, and readiness. */
}
