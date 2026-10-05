use super::*;
use std::{num::NonZeroU32, rc::Rc};

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
    let delta = new.delta.unwrap();
    assert_eq!(delta.base, old.identity());
    assert_eq!(delta.old_count, 3);
    assert_eq!(
        delta.changes,
        vec![(Some(0), None), (None, Some(0)), (Some(1), Some(1))]
    );
    let unchanged = Membership::new(old.members().to_vec())
        .unwrap()
        .with_predecessor(&old);
    assert!(unchanged.delta.is_none());
    let empty = Membership::new(vec![]).unwrap();
    for count in [64, 65] {
        let new = Membership::new((0..count).map(|i| member(&[i], 1)).collect())
            .unwrap()
            .with_predecessor(&empty);
        assert_eq!(
            new.delta.is_some(),
            usize::from(count) == MAX_INCREMENTAL_CHANGES
        );
    }
}

#[test]
fn interior_mutation_cannot_change_frozen_inputs() {
    use std::cell::Cell;
    #[derive(Debug)]
    struct Mutable {
        id: Rc<Cell<&'static [u8]>>,
        weight: Rc<Cell<NonZeroU32>>,
    }
    impl Member for Mutable {
        const DOMAIN: &'static str = "mutable";
        fn id(&self) -> &[u8] {
            self.id.get()
        }
        fn weight(&self) -> NonZeroU32 {
            self.weight.get()
        }
    }
    let id = Rc::new(Cell::new(b"a".as_slice()));
    let weight = Rc::new(Cell::new(NonZeroU32::new(7).unwrap()));
    let frozen = Membership::new(vec![Mutable {
        id: id.clone(),
        weight: weight.clone(),
    }])
    .unwrap();
    let placement_identity = frozen.identity();
    let topology_identity = frozen.topology_identity();
    let graph = frozen.graph();
    id.set(b"z");
    weight.set(NonZeroU32::new(99).unwrap());
    assert_eq!(frozen.members()[0].id(), b"z");
    assert_eq!(frozen.members()[0].weight().get(), 99);
    assert_eq!(frozen.id(0), b"a");
    assert_eq!(frozen.weight(0).get(), 7);
    assert_eq!(frozen.position(b"a"), Some(0));
    assert_eq!(frozen.position(b"z"), None);
    assert_eq!(frozen.identity(), placement_identity);
    assert_eq!(frozen.topology_identity(), topology_identity);
    assert!(Arc::ptr_eq(&graph, &frozen.graph()));
    let replacement = Membership::new(vec![Mutable { id, weight }])
        .unwrap()
        .with_predecessor(&frozen);
    assert_eq!(
        replacement.delta.unwrap().changes,
        vec![(Some(0), None), (None, Some(0))]
    );
}

#[test]
fn placement_identity_preserves_v1_and_topology_ignores_weights() {
    let old = Membership::new(vec![member(b"b", 9), member(b"a", 1)]).unwrap();
    let expected: [u8; 32] = sha2::Sha256::digest(
        b"binary-store/placement-identity/v1\0\0\0\0\x01a\0\0\0\x01\0\0\0\x01b\0\0\0\x09",
    )
    .into();
    assert_eq!(old.identity(), expected);
    let changed = Membership::new(vec![member(b"b", u32::MAX), member(b"a", 2)]).unwrap();
    assert_ne!(old.identity(), changed.identity());
    assert_eq!(old.topology_identity(), changed.topology_identity());
    assert_eq!(old.graph(), changed.graph());
    let removed = Membership::new(vec![member(b"a", 1)]).unwrap();
    assert_ne!(old.topology_identity(), removed.topology_identity());
    domain_member!(OtherDomain, "another-domain");
    let other = Membership::new(vec![
        OtherDomain(member(b"a", 1)),
        OtherDomain(member(b"b", 9)),
    ])
    .unwrap();
    assert_ne!(old.topology_identity(), other.topology_identity());
}

#[test]
fn replacing_predecessor_clears_stale_delta() {
    let old = Membership::new(vec![member(b"a", 1)]).unwrap();
    let identical = Membership::new(vec![member(b"b", 1)]).unwrap();
    let new = Membership::new(vec![member(b"b", 1)])
        .unwrap()
        .with_predecessor(&old)
        .with_predecessor(&identical);
    assert!(new.delta.is_none());
    let large = Membership::new((0..100).map(|i| member(&[i], 1)).collect()).unwrap();
    let new = Membership::new(vec![member(b"b", 1)])
        .unwrap()
        .with_predecessor(&old)
        .with_predecessor(&large);
    assert!(new.delta.is_none());
}

#[test]
fn randomized_diff_matches_independent_id_map() {
    use std::collections::{BTreeMap, BTreeSet};
    let mut state = 0x3d89_720a_481c_652fu64;
    let mut draw = || {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        state
    };
    let mut incremental_cases = 0;
    let mut cold_cases = 0;
    for iteration in 0..64 {
        let mut make = || {
            let mut values = Vec::new();
            for id in 0..if iteration % 2 == 0 { 24u8 } else { 96u8 } {
                let sample = draw();
                if sample % 3 != 0 {
                    values.push(member(&[id], (sample >> 32) as u32 | 1));
                }
            }
            Membership::new(values).unwrap()
        };
        let old = make();
        let new = make();
        let map = |members: &Membership<BinaryMember>| -> BTreeMap<Vec<u8>, (usize, NonZeroU32)> {
            members
                .members()
                .iter()
                .enumerate()
                .map(|(position, value)| (value.id().to_vec(), (position, value.weight())))
                .collect()
        };
        let before = map(&old);
        let after = map(&new);
        let ids: BTreeSet<_> = before.keys().chain(after.keys()).collect();
        let expected: Vec<_> = ids
            .into_iter()
            .filter_map(|id| {
                let a = before.get(id);
                let b = after.get(id);
                if a.map(|v| v.1) == b.map(|v| v.1) {
                    None
                } else {
                    Some((a.map(|v| v.0), b.map(|v| v.0)))
                }
            })
            .collect();
        let new = new.with_predecessor(&old);
        if expected.is_empty() || expected.len() > MAX_INCREMENTAL_CHANGES {
            cold_cases += 1;
            assert!(new.delta.is_none());
        } else {
            incremental_cases += 1;
            let delta = new.delta.unwrap();
            assert_eq!(delta.base, old.identity());
            assert_eq!(delta.old_count, old.members().len());
            assert_eq!(delta.changes, expected);
        }
    }
    assert!(incremental_cases > 0);
    assert!(cold_cases > 0);
}

#[test]
fn retained_storage_counts_capacities_and_shared_graph_once() {
    let old = Membership::new(vec![member(b"a", 1)]).unwrap();
    let new = Membership::new(vec![member(b"a", 2), member(b"longer-id", 3)])
        .unwrap()
        .with_predecessor(&old);
    let expected = new.members.capacity() * size_of::<BinaryMember>()
        + new.ids.capacity() * size_of::<Box<[u8]>>()
        + new.ids.iter().map(|id| id.len()).sum::<usize>()
        + new.weights.capacity() * size_of::<NonZeroU32>()
        + new.delta.as_ref().unwrap().changes.capacity()
            * size_of::<(Option<usize>, Option<usize>)>()
        + size_of::<Vec<Vec<usize>>>()
        + 2 * size_of::<usize>()
        + new.graph.capacity() * size_of::<Vec<usize>>()
        + new
            .graph
            .iter()
            .map(|row| row.capacity() * size_of::<usize>())
            .sum::<usize>();
    assert_eq!(new.retained_bytes(), expected);
    let shared = new.graph();
    assert_eq!(new.retained_bytes(), expected);
    assert!(Arc::ptr_eq(&shared, &new.graph()));
    let empty = Membership::<BinaryMember>::new(vec![]).unwrap();
    assert_eq!(
        empty.retained_bytes(),
        size_of::<Vec<Vec<usize>>>() + 2 * size_of::<usize>()
    );
}

#[test]
fn membership_is_send_and_sync_for_plain_members() {
    fn assert_send_sync<T: Send + Sync>() {}
    assert_send_sync::<Membership<BinaryMember>>();
    let members = Membership::new(vec![member(b"a", 1), member(b"b", 2)]).unwrap();
    let graph = members.graph();
    std::thread::spawn(move || {
        assert_eq!(members.position(b"a"), Some(0));
        assert!(Arc::ptr_eq(&graph, &members.graph()));
        assert_eq!(members.neighbors(0), vec![1]);
    })
    .join()
    .unwrap();
}

#[test]
fn member_id_length_is_checked_without_large_allocations() {
    assert_eq!(validate_id_length(0), Ok(()));
    if let Ok(maximum) = usize::try_from(u32::MAX) {
        assert_eq!(validate_id_length(maximum), Ok(()));
        if let Some(oversized) = maximum.checked_add(1) {
            assert_eq!(validate_id_length(oversized), Err(Error::InvalidMember));
        }
    }
}

#[test]
fn predecessor_constructor_shares_graph_only_for_identical_frozen_ids() {
    let old = Membership::new(vec![member(b"a", 1), member(b"b", 2)]).unwrap();
    let same =
        Membership::new_with_predecessor(vec![member(b"b", 9), member(b"a", 7)], &old).unwrap();
    assert!(Arc::ptr_eq(&old.graph(), &same.graph()));
    assert_ne!(old.identity(), same.identity());
    assert_eq!(same.delta.as_ref().unwrap().changes.len(), 2);
    let changed =
        Membership::new_with_predecessor(vec![member(b"a", 1), member(b"c", 2)], &old).unwrap();
    assert!(!Arc::ptr_eq(&old.graph(), &changed.graph()));
    assert_eq!(
        changed.graph(),
        Membership::new(changed.members().to_vec()).unwrap().graph()
    );
    assert!(
        Membership::new_with_predecessor(vec![member(b"a", 1), member(b"a", 2)], &old).is_err()
    );
}

#[test]
fn retained_bytes_uses_cached_storage_and_bounded_delta_capacity() {
    let old = Membership::new(vec![member(b"a", 1)]).unwrap();
    let mut current = Membership::new_with_predecessor(vec![member(b"a", 2)], &old).unwrap();
    assert_eq!(current.owned_bytes, current.measure_owned_bytes());
    let delta_bytes = current.delta.as_ref().unwrap().changes.capacity()
        * size_of::<(Option<usize>, Option<usize>)>();
    assert_eq!(current.retained_bytes(), current.owned_bytes + delta_bytes);
    // White-box sentinel proves the getter uses the cached total, not another
    // traversal. It also covers replacement/removal of the bounded delta hint.
    current.owned_bytes = 17;
    assert_eq!(current.retained_bytes(), 17 + delta_bytes);
    let identical = Membership::new(vec![member(b"a", 2)]).unwrap();
    current = current.with_predecessor(&identical);
    assert_eq!(current.retained_bytes(), 17);
}
