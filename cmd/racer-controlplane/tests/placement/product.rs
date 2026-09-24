// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Included in the library to exercise role assignment alongside real snapshots.
use super::*;
use crate::topology::{PlacementCache, Topology, place_in_universe};
use prost::Message;

fn fixture(count: usize, slots: u32, previous: Option<&Generation>) -> Generation {
    let mut g = Generation::empty("product-test");
    for i in 0..count {
        g.nodes.insert(
            format!("n{i}"),
            Member {
                id: identity("node", &format!("uid-{i}")),
                ip: Some(
                    format!("10.{}.{}.{}", i / 65536, i / 256 % 256, i % 256)
                        .parse()
                        .unwrap(),
                ),
                pod_uid: format!("p{i}"),
                pod_name: format!("p{i}"),
                pod_namespace: "system".into(),
                fabric: "rack".into(),
            },
        );
    }
    let by_id: BTreeMap<_, _> = g
        .nodes
        .iter()
        .map(|(name, member)| (member.id.clone(), name.clone()))
        .collect();
    let members: Vec<_> = by_id.keys().cloned().collect();
    let (graph, roles) = assign(&members, previous.and_then(|g| g.product.as_ref())).unwrap();
    let width = count.min(8) as u32;
    let candidates = PlacementCache::default()
        .candidates(slots, &g.universe, &members, width)
        .unwrap();
    let owners = candidates
        .chunks_exact(width as usize)
        .map(|r| by_id[&members[r[0] as usize]].clone())
        .collect();
    g.product = Some(ProductPlacement {
        left_factor: graph.left.order(),
        right_factor: graph.right.order(),
        members,
        roles,
        candidate_width: width,
        candidates,
    });
    g.volumes.push(Volume {
        id: "cache".into(),
        name: "cache".into(),
        resource_generation: 1,
        client_socket: "/run/racer/cache/client/socket".into(),
        origin_socket: "/run/racer/cache/origin/socket".into(),
        slots,
        cache_generation: 1,
        routing_algorithm: PRODUCT_ROUTING_ALGORITHM,
        max_candidate_attempts: 8,
        owners,
    });
    g
}

#[test]
fn physical_candidates_preserve_primary_and_exact_ranked_survivors() {
    let mut ids: Vec<_> = (0..12)
        .map(|i| identity("node", &format!("u{i}")))
        .collect();
    ids.sort();
    let mut cache = PlacementCache::default();
    let rows = cache.candidates(127, "u", &ids, 8).unwrap();
    let primary = place_in_universe(127, "u", &ids).unwrap();
    for (row, owner) in rows.chunks_exact(8).zip(primary) {
        assert_eq!(ids[row[0] as usize], owner);
        assert_eq!(row.iter().collect::<BTreeSet<_>>().len(), 8);
    }
    // Independently recover ranks by repeatedly invoking the existing primary
    // winner algorithm with the preceding winner removed from membership.
    for slot in 0..12 {
        let mut remaining = ids.clone();
        for &candidate in &rows[slot * 8..(slot + 1) * 8] {
            let winner = place_in_universe(127, "u", &remaining).unwrap()[slot].clone();
            assert_eq!(ids[candidate as usize], winner);
            remaining.retain(|id| id != &winner);
        }
    }
    let old_ids = ids.clone();
    let new_id = identity("node", "joining");
    ids.push(new_id.clone());
    ids.sort();
    let joined = cache.candidates(127, "u", &ids, 8).unwrap();
    assert_eq!(
        joined,
        PlacementCache::default()
            .candidates(127, "u", &ids, 8)
            .unwrap()
    );
    for (old, new) in rows.chunks_exact(8).zip(joined.chunks_exact(8)) {
        let before: Vec<_> = old.iter().map(|&i| &old_ids[i as usize]).collect();
        let retained: Vec<_> = new
            .iter()
            .map(|&i| &ids[i as usize])
            .filter(|&id| id != &new_id)
            .collect();
        assert_eq!(retained, before[..retained.len()]);
    }
    ids.retain(|id| id != &new_id);
    assert_eq!(cache.candidates(127, "u", &ids, 8).unwrap(), rows);
    ids.remove(3);
    assert_eq!(
        cache.candidates(127, "u", &ids, 8).unwrap(),
        PlacementCache::default()
            .candidates(127, "u", &ids, 8)
            .unwrap()
    );
    ids.reverse();
    assert_eq!(
        cache.candidates(127, "u", &ids, 3).unwrap(),
        PlacementCache::default()
            .candidates(127, "u", &ids, 3)
            .unwrap()
    );
    let good = cache.candidates(127, "u", &ids, 3).unwrap();
    assert!(cache.candidates(127, "u", &ids, 9).is_err());
    assert_eq!(cache.candidates(127, "u", &ids, 3).unwrap(), good);
}

#[test]
fn small_clusters_have_exact_graph_peers_and_zero_owner_forwarders() {
    for n in 1..=50 {
        let g = fixture(n, 3, None);
        let p = g.product.as_ref().unwrap();
        let graph = Product::new(p.left_factor, p.right_factor).unwrap();
        let topology = Topology::new(&g).unwrap();
        for (i, id) in p.members.iter().enumerate() {
            let snap = topology.snapshot(id).unwrap();
            assert!(!snap.idle);
            assert_eq!(snap.volumes.len(), 1);
            let volume = &snap.volumes[0];
            let top = volume.topology.as_ref().unwrap();
            let wire = top.product.as_ref().unwrap();
            assert_eq!(wire.local_member as usize, i);
            assert_eq!(wire.candidate_width, n.min(8) as u32);
            assert_eq!(wire.candidates, p.candidates);
            let roles = graph.neighbors(p.roles[i]);
            let expected: Vec<_> = p
                .members
                .iter()
                .zip(&p.roles)
                .filter(|(_, r)| roles.contains(r))
                .map(|(id, _)| id.clone())
                .collect();
            assert_eq!(volume.peers, expected);
            assert_eq!(
                snap.peers.iter().map(|p| p.id.clone()).collect::<Vec<_>>(),
                expected
            );
            assert_eq!(
                volume.peer_endpoints.as_ref().unwrap().peers.len(),
                expected.len()
            );
            assert_eq!(
                crate::proto::Snapshot::decode(snap.encode_to_vec().as_slice()).unwrap(),
                snap
            );
        }
    }
}

#[test]
fn ten_thousand_physical_members_have_exactly_twenty_three_peers() {
    let g = fixture(10_000, 3, None);
    let p = g.product.as_ref().unwrap();
    assert_eq!((p.left_factor, p.right_factor), (50, 200));
    assert_eq!(
        p.roles.iter().copied().collect::<BTreeSet<_>>().len(),
        10_000
    );
    assert!(peer_counts(p).iter().all(|&count| count == 23));
    let topology = Topology::new(&g).unwrap();
    for i in [0, 1, 4999, 9999] {
        let snap = topology.snapshot(&p.members[i]).unwrap();
        assert_eq!(snap.peers.len(), 23);
        assert_eq!(snap.volumes[0].peers.len(), 23);
        assert_eq!(
            snap.volumes[0]
                .topology
                .as_ref()
                .unwrap()
                .product
                .as_ref()
                .unwrap()
                .members
                .len(),
            10_000
        );
    }
}

#[test]
fn role_churn_and_graph_reassignment_never_change_physical_owners() {
    let before = fixture(50, 17, None);
    let joined = fixture(51, 17, Some(&before));
    let a = before.product.as_ref().unwrap();
    let b = joined.product.as_ref().unwrap();
    assert_eq!(
        (a.left_factor, a.right_factor),
        (b.left_factor, b.right_factor)
    );
    for (id, role) in a.members.iter().zip(&a.roles) {
        assert_eq!(*role, b.roles[b.members.binary_search(id).unwrap()]);
    }
    let removed = fixture(50, 17, Some(&joined));
    assert_eq!(removed.product, before.product);
    let mut rewired = before.clone();
    rewired.product.as_mut().unwrap().roles.rotate_left(1);
    Topology::new(&rewired).unwrap();
    let compiled = fixture(50, 17, Some(&rewired));
    assert_eq!(compiled.volumes, before.volumes);
    assert_eq!(compiled.product.as_ref().unwrap().candidates, a.candidates);
    assert_ne!(compiled.product.as_ref().unwrap().roles, a.roles);
    // Restart may rewire, but the exact ranked owner table survives.
    assert_eq!(fixture(50, 17, None).product, before.product);
}

#[test]
fn malformed_product_generations_fail_before_publication() {
    let good = fixture(7, 17, None);
    for mutation in 0..9 {
        let mut bad = good.clone();
        let p = bad.product.as_mut().unwrap();
        match mutation {
            0 => p.left_factor = 99,
            1 => {
                p.members.swap(0, 1);
            }
            2 => {
                p.roles.pop();
            }
            3 => p.roles[0] = u32::MAX,
            4 => p.candidate_width = 0,
            5 => {
                p.candidates.pop();
            }
            6 => p.candidates[0] = u32::MAX,
            7 => p.candidates[0] = p.candidates[1],
            _ => {
                bad.product = None;
            }
        }
        assert!(Topology::new(&bad).is_err(), "mutation {mutation}");
    }
    let mut bad = good.clone();
    bad.product.as_mut().unwrap().roles.fill(0);
    assert!(Topology::new(&bad).is_err());
    let mut bad = good;
    let row = &mut bad.product.as_mut().unwrap().candidates;
    row.swap(0, 1);
    assert!(Topology::new(&bad).is_err());
    let mut bad = fixture(7, 17, None);
    bad.product = None;
    bad.volumes[0].owners.clear();
    assert!(Topology::new(&bad).is_err());
}

#[test]
fn vacant_roles_are_repaired_by_one_duplicate_member_and_profiles_resize() {
    let base = fixture(50, 7, None);
    let expanded = fixture(51, 7, Some(&base));
    let old = expanded.product.as_ref().unwrap();
    let mut loads = vec![0; 50];
    for &role in &old.roles {
        loads[role as usize] += 1;
    }
    let removed = old
        .roles
        .iter()
        .position(|&role| loads[role as usize] == 1)
        .unwrap();
    let mut members = old.members.clone();
    members.remove(removed);
    let (profile, roles) = assign(&members, Some(old)).unwrap();
    assert_eq!(profile.order(), 50);
    assert_eq!(roles.iter().copied().collect::<BTreeSet<_>>().len(), 50);
    assert_eq!(
        members
            .iter()
            .zip(&roles)
            .filter(|(id, role)| old.roles[old.members.binary_search(id).unwrap()] != **role)
            .count(),
        1
    );
    members.pop();
    let (smaller, roles) = assign(&members, Some(old)).unwrap();
    assert!(smaller.order() <= 49);
    assert_eq!(roles.len(), 49);
    let many: Vec<_> = (0..101).map(|i| format!("{i:064x}")).collect();
    let (larger, roles) = assign(&many, Some(old)).unwrap();
    assert_eq!(larger, choose(101).unwrap());
    assert_eq!(roles.len(), 101);
}

#[test]
fn growth_from_two_to_three_resizes_and_rejects_bundled_k2() {
    let two = fixture(2, 17, None);
    let three = fixture(3, 17, Some(&two));
    let p = three.product.as_ref().unwrap();
    assert_eq!((p.left_factor, p.right_factor), (1, 3));
    let topology = Topology::new(&three).unwrap();
    for id in &p.members {
        assert_eq!(topology.snapshot(id).unwrap().peers.len(), 2);
    }
    assert_eq!(three.product, fixture(3, 17, None).product);
    for n in [3, 4] {
        for (left, right) in [(1, 2), (2, 1)] {
            let mut invalid = fixture(n, 17, None);
            let p = invalid.product.as_mut().unwrap();
            p.left_factor = left;
            p.right_factor = right;
            p.roles = (0..n).map(|i| (i % 2) as u32).collect();
            let members = p.members.clone();
            assert!(Topology::new(&invalid).is_err());
            let (replacement, roles) = assign(&members, invalid.product.as_ref()).unwrap();
            assert!(replacement.order() >= 3);
            assert_eq!(roles.len(), n);
        }
    }
}

#[test]
fn candidate_width_is_volume_scoped_and_aggregate_budget_is_enforced() {
    let mut g = fixture(7, 17, None);
    let mut other = g.volumes[0].clone();
    other.id = "other".into();
    other.max_candidate_attempts = 2;
    g.volumes.push(other);
    let topology = Topology::new(&g).unwrap();
    let p = g.product.as_ref().unwrap();
    let snap = topology.snapshot(&p.members[0]).unwrap();
    let a = snap.volumes[0]
        .topology
        .as_ref()
        .unwrap()
        .product
        .as_ref()
        .unwrap();
    let b = snap.volumes[1]
        .topology
        .as_ref()
        .unwrap()
        .product
        .as_ref()
        .unwrap();
    assert_eq!(b.candidate_width, 2);
    for (full, short) in a
        .candidates
        .chunks_exact(7)
        .zip(b.candidates.chunks_exact(2))
    {
        assert_eq!(&full[..2], short);
    }
    let mut large = fixture(8, SLOT_COUNT, None);
    for i in 1..8 {
        let mut volume = large.volumes[0].clone();
        volume.id = format!("cache-{i}");
        large.volumes.push(volume);
    }
    assert!(Topology::new(&large).is_err());
}
