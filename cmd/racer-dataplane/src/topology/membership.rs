//! Stable sorted node identities and immutable leased membership versions.
//!
//! Readiness never changes ownership. Exclusions, weights, additions, and deletions
//! do. Retain bounded old snapshots until their in-flight leases are released.
//! Endpoint/rail/alignment changes also advance membership version, but placement
//! depends only on node IDs and shares. All inputs are controller-accepted values.
use super::rails::RailMapping;
use crate::{
    error::{Error, Result},
    model::identity::{MembershipVersion, NodeId},
};
use std::{net::SocketAddr, num::NonZeroU32, sync::Arc};

pub const MAX_MEMBERS: usize = 100_000;

#[derive(Clone, Debug)]
pub struct Member {
    pub node: NodeId,
    pub shares: NonZeroU32,
    pub peer_endpoint: String,
    pub rails: Vec<RailMapping>,
    pub alignment_enabled: bool,
}
#[derive(Debug)]
pub struct Membership {
    pub version: MembershipVersion,
    members: Vec<Member>,
}
pub type MembershipLease = Arc<Membership>;
impl Membership {
    pub fn validate(version: MembershipVersion, mut members: Vec<Member>) -> Result<Self> {
        if version.0 == 0 || members.len() > MAX_MEMBERS {
            return Err(Error::InvalidConfiguration);
        }
        for member in &mut members {
            if !valid_identity(&member.node.0) {
                return Err(Error::InvalidConfiguration);
            }
            let endpoint: SocketAddr = member
                .peer_endpoint
                .parse()
                .map_err(|_| Error::InvalidConfiguration)?;
            if endpoint.port() == 0
                || endpoint.ip().is_unspecified()
                || endpoint.ip().is_multicast()
            {
                return Err(Error::InvalidConfiguration);
            }
            member.rails.sort_unstable_by_key(|mapping| mapping.rail.0);
            if member
                .rails
                .iter()
                .any(|mapping| !valid_identity(&mapping.fabric))
                || member
                    .rails
                    .windows(2)
                    .any(|pair| pair[0].rail == pair[1].rail)
            {
                return Err(Error::InvalidConfiguration);
            }
        }
        members.sort_unstable_by(|a, b| a.node.cmp(&b.node));
        if members.windows(2).any(|pair| pair[0].node == pair[1].node) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { version, members })
    }
    pub fn members(&self) -> &[Member] {
        &self.members
    }
    pub fn position(&self, node: &NodeId) -> Result<usize> {
        self.members
            .binary_search_by(|member| member.node.cmp(node))
            .map_err(|_| Error::IncompatibleMembership)
    }
    pub fn member(&self, node: &NodeId) -> Result<&Member> {
        Ok(&self.members[self.position(node)?])
    }
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && value.bytes().all(|byte| byte.is_ascii_graphic())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::topology::{
        fixtures::member,
        rails::{RailId, RailMapping},
    };

    #[test]
    fn validates_and_freezes_sorted_members() {
        let input = vec![member(2, 1), member(0, u32::MAX), member(1, 4)];
        let membership = Membership::validate(MembershipVersion(1), input).unwrap();
        assert_eq!(
            membership
                .members()
                .iter()
                .map(|m| m.node.clone())
                .collect::<Vec<_>>(),
            (0..3).map(|i| member(i, 1).node).collect::<Vec<_>>()
        );
        assert_eq!(
            membership.member(&member(0, 1).node).unwrap().shares.get(),
            u32::MAX
        );
        assert_eq!(
            membership.position(&NodeId("absent".into())),
            Err(Error::IncompatibleMembership)
        );
        assert!(Membership::validate(MembershipVersion(1), vec![]).is_ok());
    }

    #[test]
    fn rejects_bad_members_and_duplicate_rails() {
        assert!(Membership::validate(MembershipVersion(0), vec![]).is_err());
        assert!(Membership::validate(MembershipVersion(1), vec![member(0, 1); 2]).is_err());
        for endpoint in ["host:80", "127.0.0.1:0", "0.0.0.0:80", "[::]:80", "garbage"] {
            let mut node = member(0, 1);
            node.peer_endpoint = endpoint.into();
            assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
        }
        let mut node = member(0, 1);
        node.rails = vec![
            RailMapping {
                rail: RailId(0),
                fabric: "a".into(),
                numa_node: None
            };
            2
        ];
        assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
        let mut node = member(0, 1);
        node.node.0.clear();
        assert!(Membership::validate(MembershipVersion(1), vec![node]).is_err());
    }
}
