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
    placement_identity: [u8; 32],
    pub(crate) placement_delta: Option<PlacementDelta>,
}
#[derive(Debug)]
pub(crate) struct PlacementDelta {
    pub base: [u8; 32],
    pub old_count: usize,
    pub changes: Vec<(Option<usize>, Option<usize>)>,
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
            // Match Go netip.ParseAddrPort: publication validity is independent
            // of local reachability, but zones are not portable topology inputs.
            if endpoint.port() == 0 || member.peer_endpoint.contains('%') {
                return Err(Error::InvalidConfiguration);
            }
            member.rails.sort_unstable_by_key(|mapping| mapping.rail.0);
            if member.rails.iter().any(|mapping| {
                !valid_fabric(&mapping.fabric)
                    || mapping
                        .numa_node
                        .is_some_and(|numa| u32::try_from(numa).is_err())
            }) || member
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
        use sha2::Digest;
        let mut hash = super::hash::domain(b"racer/placement-identity/v1\0");
        for member in &members {
            super::hash::bytes(&mut hash, member.node.0.as_bytes());
            hash.update(member.shares.get().to_be_bytes());
        }
        Ok(Self {
            version,
            placement_identity: super::hash::finish(hash),
            members,
            placement_delta: None,
        })
    }
    /// Prepare bounded incremental ranking hints outside the publication lock.
    /// Larger changes use exact cooperative cold computation on demand.
    pub fn with_predecessor(mut self, old: &Membership) -> Self {
        if self.placement_identity == old.placement_identity {
            return self;
        }
        let mut changes = Vec::new();
        let (mut a, mut b) = (0, 0);
        while a < old.members.len() || b < self.members.len() {
            let order = match (old.members.get(a), self.members.get(b)) {
                (Some(a), Some(b)) => a.node.cmp(&b.node),
                (Some(_), None) => std::cmp::Ordering::Less,
                _ => std::cmp::Ordering::Greater,
            };
            match order {
                std::cmp::Ordering::Less => {
                    changes.push((Some(a), None));
                    a += 1;
                }
                std::cmp::Ordering::Greater => {
                    changes.push((None, Some(b)));
                    b += 1;
                }
                std::cmp::Ordering::Equal => {
                    if old.members[a].shares != self.members[b].shares {
                        changes.push((Some(a), Some(b)));
                    }
                    a += 1;
                    b += 1;
                }
            }
            if changes.len() > 64 {
                return self;
            }
        }
        self.placement_delta = Some(PlacementDelta {
            base: old.placement_identity,
            old_count: old.members.len(),
            changes,
        });
        self
    }
    /// Local cache identity only. Routing still uses the authenticated version.
    pub fn placement_identity(&self) -> [u8; 32] {
        self.placement_identity
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

fn valid_fabric(value: &str) -> bool {
    // Go wire.validRail accepts any nonempty UTF-8 string except NUL/CR/LF.
    // String guarantees UTF-8 here. The bounded publication codec owns the
    // aggregate byte limit; the wire contract has no per-fabric length limit.
    !value.is_empty() && !value.contains(['\0', '\r', '\n'])
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
        for endpoint in [
            "host:80",
            "127.0.0.1:0",
            "[::]:0",
            "[fe80::1%3]:80",
            "[fe80::1%eth0]:80",
            "garbage",
        ] {
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

    #[test]
    fn fabric_matches_go_wire_utf8_and_control_contract() {
        // Includes the Go shared publication vector, whitespace/other controls
        // allowed by validRail, and names beyond the former local 256-byte cap.
        for fabric in [
            "β<&>\u{2028}".to_owned(),
            "网络 fabric 🚆".to_owned(),
            " \t\u{0001}\u{007f}".to_owned(),
            "é".repeat(257),
        ] {
            let mut node = member(0, 4);
            node.rails = vec![RailMapping {
                rail: RailId(u16::MAX),
                fabric: fabric.clone(),
                numa_node: Some(u32::MAX as usize),
            }];
            let accepted = Membership::validate(MembershipVersion(1), vec![node]).unwrap();
            assert_eq!(
                accepted.members()[0].rails[0].fabric.as_bytes(),
                fabric.as_bytes()
            );
            assert_eq!(
                accepted.members()[0].rails[0].numa_node,
                Some(u32::MAX as usize)
            );
        }
        for fabric in ["", "\0", "\r", "\n", "β\0fabric", "β\rfabric", "β\nfabric"] {
            let mut node = member(0, 4);
            node.rails = vec![RailMapping {
                rail: RailId(0),
                fabric: fabric.into(),
                numa_node: None,
            }];
            assert_eq!(
                Membership::validate(MembershipVersion(1), vec![node]).unwrap_err(),
                Error::InvalidConfiguration
            );
        }
    }

    #[test]
    fn endpoint_validation_matches_go_wire_without_reachability_policy() {
        for endpoint in [
            "0.0.0.0:80",
            "[::]:80",
            "224.0.0.1:80",
            "[ff02::1]:80",
            "[2001:db8::1]:7443",
            "192.0.2.1:65535",
        ] {
            let mut node = member(0, 4);
            node.peer_endpoint = endpoint.into();
            let accepted = Membership::validate(MembershipVersion(1), vec![node]).unwrap();
            assert_eq!(accepted.members()[0].peer_endpoint, endpoint);
        }
    }

    #[test]
    fn fabric_ascii_controls_match_wire_exactly() {
        for byte in 0u8..=127 {
            let mut node = member(0, 4);
            node.rails = vec![RailMapping {
                rail: RailId(0),
                fabric: format!("β{}fabric", char::from(byte)),
                numa_node: None,
            }];
            assert_eq!(
                Membership::validate(MembershipVersion(1), vec![node]).is_ok(),
                !matches!(byte, 0 | b'\r' | b'\n'),
                "ASCII byte {byte}",
            );
        }
    }

    #[test]
    fn numa_id_cannot_exceed_wire_u32() {
        if let Some(oversized) = (u32::MAX as usize).checked_add(1) {
            let mut node = member(0, 4);
            node.rails = vec![RailMapping {
                rail: RailId(0),
                fabric: "fabric".into(),
                numa_node: Some(oversized),
            }];
            assert_eq!(
                Membership::validate(MembershipVersion(1), vec![node]).unwrap_err(),
                Error::InvalidConfiguration
            );
        }
    }
}
