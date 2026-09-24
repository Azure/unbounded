// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::collections::BTreeMap;

pub(crate) fn volume(
    left: u32,
    right: u32,
    roles: Vec<u32>,
    local: u32,
    candidates: Vec<u32>,
) -> proto::Volume {
    let graph = Product::new(left, right).unwrap();
    let members: Vec<_> = (0..roles.len()).map(|m| format!("{m:064x}")).collect();
    let adjacent = graph.neighbors(roles[local as usize]);
    proto::Volume {
        id: "product-test".into(),
        peers: roles
            .iter()
            .enumerate()
            .filter(|(_, r)| adjacent.contains(r))
            .map(|(m, _)| members[m].clone())
            .collect(),
        topology: Some(proto::Topology {
            epoch: 1,
            slot_count: 1,
            local_slots: if candidates[0] == local {
                vec![0]
            } else {
                vec![]
            },
            routing_algorithm: Some(1),
            product: Some(proto::ProductTopology {
                left_factor: left,
                right_factor: right,
                members,
                roles,
                local_member: local,
                candidate_width: candidates.len() as u32,
                candidates,
            }),
            ..Default::default()
        }),
        ..Default::default()
    }
}

#[test]
fn product_cursor_and_prefix_repair() {
    let roles: Vec<_> = (0..2500).collect();
    let mut exercised = [false; 4];
    for target in [1, 7, 53, 99, 377, 2499] {
        let v = volume(50, 50, roles.clone(), 0, vec![target]);
        let r = Routing::new(&[1; 32], &v).unwrap();
        let c = r.start_key(&[0; 32]);
        assert!(c.path.len() <= 5);
        for (at, exercised) in exercised
            .iter_mut()
            .enumerate()
            .take(c.path.len().saturating_sub(2))
        {
            let local = c.path[at];
            let r = Routing::new(
                &[1; 32],
                &volume(50, 50, roles.clone(), local, vec![target]),
            )
            .unwrap();
            let mut c = c.clone();
            c.position = at as u8;
            let fixed = r.repair(&c).unwrap();
            *exercised = true;
            assert_eq!(&fixed.path[..=at], &c.path[..=at]);
            assert!(fixed.path.len() <= 5);
            assert!(!fixed.path.contains(&c.path[at + 1]));
            assert_eq!(fixed.attempt, c.attempt);
            assert!(r.repair(&fixed).is_err());
            let decoded = Cursor::decode_algorithm(&fixed.encode(), Algorithm::Product).unwrap();
            assert_eq!(decoded, fixed);
            for (position, &member) in fixed.path.iter().enumerate().skip(at) {
                let receiver = Routing::new(
                    &[1; 32],
                    &volume(50, 50, roles.clone(), member, vec![target]),
                )
                .unwrap();
                let mut delivered = fixed.clone();
                delivered.position = position as u8;
                receiver.validate(&delivered, &[0; 32]).unwrap();
                let mut bad = delivered.clone();
                bad.path[0] = target;
                assert!(receiver.validate(&bad, &[0; 32]).is_err());
                bad = delivered;
                bad.identity[0] ^= 1;
                assert!(receiver.receive(bad, &[0; 32]).is_err());
            }
        }
    }
    assert!(exercised[..3].iter().all(|v| *v));
}

#[test]
fn bundles_candidates_and_validation() {
    let roles = vec![0, 1, 2, 3, 0, 1];
    let v = volume(2, 2, roles.clone(), 0, vec![4, 3]);
    let r = Routing::new(&[1; 32], &v).unwrap();
    let mut c = r.start_key(&[0; 32]);
    assert_eq!(c.path, [0, 1, 4]);
    assert_eq!(r.repair(&c).unwrap().path, [0, 5, 4]);
    r.advance_candidate(&mut c).unwrap();
    assert_eq!(r.destination(&c), 3);
    assert_eq!(c.failed, u32::MAX);
    assert!(r.advance_candidate(&mut c).is_err());
    for mutation in 0..6 {
        let mut bad = v.clone();
        let p = bad.topology.as_mut().unwrap().product.as_mut().unwrap();
        match mutation {
            0 => p.candidates[1] = p.candidates[0],
            1 => p.roles[3] = 0,
            2 => p.members.swap(0, 1),
            3 => p.candidate_width = 9,
            4 => p.candidates[0] = 100,
            _ => {
                bad.peers.pop();
            }
        }
        assert!(Routing::new(&[1; 32], &bad).is_err());
    }
    let bytes = r.start_key(&[0; 32]).encode();
    assert!(Cursor::decode(&bytes).is_ok());
    for (index, value) in [(44, 5), (45, 0), (65, 0), (70, 1)] {
        let mut bad = bytes.clone();
        bad[index] = value;
        assert!(Cursor::decode_algorithm(&bad, Algorithm::Product).is_err());
    }
}

#[test]
fn production_compiler_product_snapshots() {
    use prost::Message;
    let export = std::env::var_os("RACER_PRODUCT_EXPORT")
        .map(std::path::PathBuf::from)
        .unwrap_or_else(|| crate::conformance::compiler_snapshots().to_path_buf());
    for count in [2, 3, 7] {
        let snapshots: Vec<_> = (0..count)
            .map(|i| {
                proto::Snapshot::decode(
                    std::fs::read(export.join(format!("p262144-n{count}-fresh-{i}.pb")))
                        .unwrap()
                        .as_slice(),
                )
                .unwrap()
            })
            .collect();
        let routes: BTreeMap<_, _> = snapshots
            .iter()
            .map(|snapshot| {
                let trust = super::super::Trust {
                    universe: snapshot.universe.clone().try_into().unwrap(),
                    node: snapshot.node.clone().try_into().unwrap(),
                };
                let prepared = trust
                    .prepare(proto::Configuration {
                        contents: Some(proto::configuration::Contents::Snapshot(snapshot.clone())),
                    })
                    .unwrap();
                let r = prepared.volumes()[0].routing().clone();
                assert_eq!(r.algorithm, Algorithm::Product);
                let local = r.product.config.local_member;
                let mut bad = snapshot.clone();
                bad.volumes[0]
                    .topology
                    .as_mut()
                    .unwrap()
                    .product
                    .as_mut()
                    .unwrap()
                    .local_member = (local + 1) % count;
                assert!(
                    trust
                        .prepare(proto::Configuration {
                            contents: Some(proto::configuration::Contents::Snapshot(bad))
                        })
                        .is_err()
                );
                (local, r)
            })
            .collect();
        for r in routes.values() {
            for value in 0..32u32 {
                let key = *blake3::hash(&value.to_le_bytes()).as_bytes();
                let mut c = r.start_key(&key);
                let mut destinations = BTreeSet::new();
                for attempt in 0..r.candidate_count() {
                    assert_eq!(c.attempt, attempt);
                    assert!(destinations.insert(r.destination(&c)));
                    let mut delivered = c.clone();
                    loop {
                        let receiver = &routes[&delivered.path[delivered.position as usize]];
                        receiver.validate(&delivered, &key).unwrap();
                        match receiver.next(&delivered).unwrap() {
                            Some((_, next)) => delivered = next,
                            None => break,
                        }
                    }
                    assert_eq!(
                        delivered.path[delivered.position as usize],
                        r.destination(&c)
                    );
                    if attempt + 1 < r.candidate_count() {
                        r.advance_candidate(&mut c).unwrap();
                    }
                }
            }
        }
    }
}

#[test]
fn exact_ten_thousand_catalog_has_twenty_three_physical_peers() {
    let graph = crate::product::choose(10_000).unwrap();
    assert_eq!(graph.codes(), (50, 200));
    for source in [0, 199, 200, 5432, 9999] {
        let v = volume(50, 200, (0..10_000).collect(), source, vec![9998, 17, 9999]);
        assert_eq!(v.peers.len(), 23);
        let r = Routing::new(&[1; 32], &v).unwrap();
        let c = r.start_key(&[0; 32]);
        assert!(c.path.len() <= 5);
        r.validate(&c, &[0; 32]).unwrap();
    }
}

#[test]
fn product_cursor_rejects_forged_ownership_paths_and_repairs() {
    let r = Routing::new(&[1; 32], &volume(2, 2, (0..4).collect(), 0, vec![3, 1])).unwrap();
    let original = r.start_key(&[0; 32]);
    for mutate in 0..10 {
        let mut c = original.clone();
        match mutate {
            0 => c.owner = 1,
            1 => c.attempt = 2,
            2 => c.source = u32::MAX,
            3 => c.path[1] = 0,
            4 => c.path[2] = 1,
            5 => c.position = 1,
            6 => c.failed = 3,
            7 => c.repair_position = 1,
            8 => c.path.push(3),
            _ => c.identity[0] ^= 1,
        }
        assert!(r.validate(&c, &[0; 32]).is_err(), "mutation {mutate}");
    }
    let owner = Routing::new(&[1; 32], &volume(2, 2, (0..4).collect(), 3, vec![3])).unwrap();
    let local = owner.start_key(&[0; 32]);
    assert_eq!(local.path, [3]);
    assert!(owner.next(&local).unwrap().is_none());
    assert!(owner.repair(&local).is_err());
    let singleton = Routing::new(&[1; 32], &volume(1, 1, vec![0], 0, vec![0])).unwrap();
    assert!(!singleton.distributed());
    assert_eq!(singleton.start_key(&[0; 32]).path, [0]);
}

#[test]
fn disconnected_bundle_failure_fails_closed_without_changing_owner() {
    // Reject a singleton bridge at configuration admission, before serving any
    // owner. Bundled roles require at least two neighboring roles.
    for (left, right, roles) in [
        (1, 2, vec![0, 1, 0]),
        (2, 1, vec![0, 1, 0]),
        (1, 1, vec![0, 0]),
    ] {
        assert!(Routing::new(&[1; 32], &volume(left, right, roles, 0, vec![0])).is_err());
    }
    for (left, right, roles) in [
        (1, 1, vec![0]),
        (1, 2, vec![0, 1]),
        (2, 1, vec![0, 1]),
        (1, 3, vec![0, 1, 2, 0]),
    ] {
        Routing::new(&[1; 32], &volume(left, right, roles, 0, vec![0])).unwrap();
    }
}
