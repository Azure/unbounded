use crate::{Error, Member, hash};
use sha2::Digest;

/// Immutable ID-sorted members and bounded predecessor hints.
#[derive(Debug)]
pub struct Membership<M: Member> {
    members: Vec<M>,
    identity: [u8; 32],
    pub(crate) placement_delta: Option<PlacementDelta>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::num::NonZeroU32;

    #[derive(Clone, Debug)]
    struct BinaryMember(Vec<u8>, NonZeroU32);
    impl Member for BinaryMember {
        const DOMAIN: &'static str = "binary-store";
        fn id(&self) -> &[u8] {
            &self.0
        }
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }
    fn member(id: &[u8], weight: u32) -> BinaryMember {
        BinaryMember(id.to_vec(), NonZeroU32::new(weight).unwrap())
    }

    macro_rules! domain_member {
        ($name:ident, $domain:expr) => {
            #[derive(Debug)]
            struct $name(BinaryMember);
            impl Member for $name {
                const DOMAIN: &'static str = $domain;
                fn id(&self) -> &[u8] {
                    self.0.id()
                }
                fn weight(&self) -> NonZeroU32 {
                    self.0.weight()
                }
            }
        };
    }

    #[test]
    fn domain_member_concatenation_collision_is_rejected() {
        domain_member!(Plain, "x");
        domain_member!(
            Adversarial,
            concat!("x", "/placement-identity/v1\0", "\0\0\0\x13")
        );
        let members = Membership::new(vec![Plain(member(
            b"/placement-identity",
            u32::from_be_bytes(*b"/v1\0"),
        ))])
        .unwrap();
        // Without validation, the empty adversarial membership hashes exactly
        // like the one-member plain membership despite having no valid indices.
        assert_eq!(
            members.identity(),
            hash::finish(hash::domain::<Adversarial>(b"/placement-identity/v1\0"))
        );
        let placement = crate::Placement::new(2);
        assert_eq!(placement.rank(&members, b"key").unwrap(), vec![0]);
        assert_eq!(
            Membership::<Adversarial>::new(vec![]).unwrap_err(),
            Error::InvalidDomain
        );
    }

    #[test]
    fn nul_domains_rejected_for_empty_and_nonempty_memberships() {
        macro_rules! check {
            ($domain:expr) => {{
                domain_member!(Invalid, $domain);
                for members in [vec![], vec![Invalid(member(b"id", 1))]] {
                    assert_eq!(Membership::new(members).unwrap_err(), Error::InvalidDomain);
                }
            }};
        }
        check!("\0");
        check!("\0prefix");
        check!("pre\0fix");
        check!("prefix\0");
        assert_eq!(
            Error::InvalidDomain.to_string(),
            "member domain must not contain NUL"
        );
    }

    #[test]
    fn domains_without_nul_remain_valid() {
        macro_rules! check {
            ($domain:expr) => {{
                domain_member!(Valid, $domain);
                let empty = Membership::<Valid>::new(vec![]).unwrap();
                assert!(empty.members().is_empty());
                let members = Membership::new(vec![Valid(member(b"\0", 1))]).unwrap();
                assert_eq!(members.position(b"\0"), Some(0));
                assert_ne!(empty.identity(), members.identity());
            }};
        }
        check!("");
        check!("racer");
        check!("x/placement-identity/v1");
        check!("prefix with spaces/and\ncontrols\x01");
        check!("存储/é");
    }

    #[test]
    fn binary_ids_sorted_duplicate_rejected_and_empty_allowed() {
        let members = Membership::new(vec![
            member(&[255, 0], u32::MAX),
            member(&[], 1),
            member(&[0], 2),
        ])
        .unwrap();
        assert_eq!(members.position(&[]), Some(0));
        assert_eq!(members.position(&[0]), Some(1));
        assert_eq!(members.position(&[255, 0]), Some(2));
        assert_eq!(members.position(&[255]), None);
        assert_eq!(members.members()[2].weight().get(), u32::MAX);
        assert_eq!(
            Membership::new(vec![member(&[0], 1), member(&[0], 2)]).unwrap_err(),
            Error::DuplicateMember
        );
        let empty = Membership::<BinaryMember>::new(vec![]).unwrap();
        assert_eq!(empty.position(&[]), None);
        assert!(empty.members().is_empty());
        let reordered = Membership::new(vec![
            member(&[0], 2),
            member(&[255, 0], u32::MAX),
            member(&[], 1),
        ])
        .unwrap();
        assert_eq!(members.identity(), reordered.identity());
    }

    #[test]
    fn predecessor_delta_preserves_indices_and_caps_changes() {
        let old = Membership::new(vec![member(b"a", 1), member(b"c", 1), member(b"d", 1)]).unwrap();
        let new = Membership::new(vec![member(b"b", 1), member(b"c", 2), member(b"d", 1)])
            .unwrap()
            .with_predecessor(&old);
        let delta = new.placement_delta.unwrap();
        assert_eq!(delta.base, old.identity());
        assert_eq!(delta.old_count, 3);
        assert_eq!(
            delta.changes,
            vec![(Some(0), None), (None, Some(0)), (Some(1), Some(1))]
        );
        let unchanged = Membership::new(old.members().to_vec())
            .unwrap()
            .with_predecessor(&old);
        assert!(unchanged.placement_delta.is_none());
        let empty = Membership::new(vec![]).unwrap();
        for count in [64, 65] {
            let new = Membership::new((0..count).map(|i| member(&[i], 1)).collect())
                .unwrap()
                .with_predecessor(&empty);
            assert_eq!(new.placement_delta.is_some(), count == 64);
        }
    }
}

#[derive(Debug)]
pub(crate) struct PlacementDelta {
    pub base: [u8; 32],
    pub old_count: usize,
    pub changes: Vec<(Option<usize>, Option<usize>)>,
}

impl<M: Member> Membership<M> {
    /// Reject NUL-containing domains and duplicate member IDs.
    pub fn new(mut members: Vec<M>) -> Result<Self, Error> {
        if M::DOMAIN.as_bytes().contains(&0) {
            return Err(Error::InvalidDomain);
        }
        members.sort_unstable_by(|a, b| a.id().cmp(b.id()));
        if members.windows(2).any(|pair| pair[0].id() == pair[1].id()) {
            return Err(Error::DuplicateMember);
        }
        let mut digest = hash::domain::<M>(b"/placement-identity/v1\0");
        for member in &members {
            hash::bytes(&mut digest, member.id());
            digest.update(member.weight().get().to_be_bytes());
        }
        Ok(Self {
            members,
            identity: hash::finish(digest),
            placement_delta: None,
        })
    }

    /// Prepare bounded incremental ranking hints outside request processing.
    /// Larger changes use exact cooperative cold computation on demand.
    pub fn with_predecessor(mut self, old: &Self) -> Self {
        if self.identity == old.identity {
            return self;
        }
        let mut changes = Vec::new();
        let (mut a, mut b) = (0, 0);
        while a < old.members.len() || b < self.members.len() {
            let order = match (old.members.get(a), self.members.get(b)) {
                (Some(a), Some(b)) => a.id().cmp(b.id()),
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
                    if old.members[a].weight() != self.members[b].weight() {
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
            base: old.identity,
            old_count: old.members.len(),
            changes,
        });
        self
    }

    pub fn members(&self) -> &[M] {
        &self.members
    }

    pub fn position(&self, id: &[u8]) -> Option<usize> {
        self.members.binary_search_by(|m| m.id().cmp(id)).ok()
    }

    /// Domain, IDs, and weights only, independent of application metadata.
    pub fn identity(&self) -> [u8; 32] {
        self.identity
    }
}
