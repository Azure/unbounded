// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use racer_controlplane::model::*;
use racer_controlplane::topology::{
    PlacementCache, Topology, compile, compile_cached, place_in_universe,
};
use std::collections::BTreeMap;
use std::time::Instant;

fn ids(count: usize) -> Vec<String> {
    (0..count)
        .map(|i| identity("node", &format!("uid-{i}")))
        .collect()
}

fn input(count: usize) -> Inventory {
    Inventory {
        universe: "site-a".into(),
        socket_root: SOCKET_ROOT.into(),
        nodes: (0..count)
            .map(|i| Node {
                name: format!("node-{i}"),
                uid: format!("uid-{i}"),
                universe: "site-a".into(),
                eligible: true,
                ready: true,
                fabric: "rack".into(),
                pods: vec![Pod {
                    uid: format!("pod-{i}"),
                    name: format!("pod-{i}"),
                    namespace: "racer".into(),
                    ip: format!("10.{}.{}.{}", i / 65536, i / 256 % 256, i % 256),
                    available: true,
                    ready: true,
                    created_at: 1,
                }],
            })
            .collect(),
        caches: vec![Cache {
            name: "cache-a".into(),
            uid: "cache-a-uid".into(),
            resource_generation: 1,
            cache_generation: 0,
            max_candidate_attempts: 3,
        }],
    }
}

#[test]
fn canonical_placement_matches_independent_golden_vector() {
    let nodes = ids(4);
    let expected = [3, 0, 3, 1, 2, 1, 0, 2, 3, 3, 1, 3, 3, 0, 1, 0, 0];
    assert_eq!(
        place_in_universe(17, "site-a", &nodes).unwrap(),
        expected.map(|i| nodes[i].clone())
    );
    assert_ne!(
        place_in_universe(17, "site-a", &nodes).unwrap(),
        place_in_universe(17, "site-b", &nodes).unwrap()
    );
}

#[test]
fn exact_incremental_matches_cold_across_add_remove_replace_reorder_and_reset() {
    let mut cached = PlacementCache::default();
    let mut nodes = ids(40);
    let mut old_nodes = Vec::new();
    let mut old = Vec::<String>::new();
    for step in 0..80 {
        match step % 5 {
            0 => nodes.push(identity("node", &format!("join-{step}"))),
            1 => {
                nodes.remove(step % nodes.len());
            }
            2 => nodes.rotate_left(3),
            3 => nodes[step % 30] = identity("node", &format!("replacement-{step}")),
            _ => nodes.reverse(),
        }
        let next = cached.place(1024, "site-a", &nodes).unwrap();
        assert_eq!(next, place_in_universe(1024, "site-a", &nodes).unwrap());
        for (before, after) in old.iter().zip(&next) {
            if nodes.contains(before) && old_nodes.contains(after) {
                assert_eq!(before, after, "surviving old nodes cannot exchange slots");
            }
        }
        old = next;
        old_nodes = nodes.clone();
    }
    for (slots, universe, members) in [
        (17, "site-a", nodes.clone()),
        (17, "site-b", nodes.clone()),
        (17, "site-b", vec![]),
        (17, "site-b", nodes.clone()),
        (1, "site-a", nodes),
    ] {
        assert_eq!(
            cached.place(slots, universe, &members).unwrap(),
            place_in_universe(slots, universe, &members).unwrap()
        );
    }
}

#[test]
fn invalid_inputs_do_not_poison_ephemeral_acceleration() {
    let mut cached = PlacementCache::default();
    let nodes = ids(8);
    let good = cached.place(64, "site-a", &nodes).unwrap();
    for (slots, universe, members) in [
        (0, "site-a", nodes.clone()),
        (SLOT_COUNT + 1, "site-a", nodes.clone()),
        (64, "", nodes.clone()),
        (64, "site-a", vec![nodes[0].clone(); 2]),
        (64, "site-a", vec!["invalid".into()]),
        (64, "site-a", vec![nodes[0].to_uppercase()]),
    ] {
        assert!(cached.place(slots, universe, &members).is_err());
        assert_eq!(cached.place(64, "site-a", &nodes).unwrap(), good);
    }
    assert!(cached.place(64, "site-a", &[]).unwrap().is_empty());
}

#[test]
fn compiler_ignores_history_and_volatile_fields_and_shares_universe_placement() {
    let mut input = input(7);
    let mut cache = PlacementCache::default();
    let first = compile_cached(&input, None, &mut cache).unwrap();
    let mut historical = first.clone();
    historical.revision = 99;
    historical.volumes[0].owners.reverse();
    historical
        .nodes
        .insert("deleted/obsolete".into(), Member::default());
    let mut rebuilt = compile_cached(&input, Some(&historical), &mut cache).unwrap();
    assert_eq!(rebuilt.revision, 99);
    rebuilt.revision = 0;
    assert_eq!(rebuilt, first);
    input.nodes.reverse();
    for node in &mut input.nodes {
        node.pods[0].uid.push_str("-replacement");
        node.pods[0].ip = node.pods[0].ip.replace("10.0.", "10.1.");
        node.pods[0].created_at += 99;
        node.fabric = "new-rack".into();
    }
    input.caches[0].uid = "recreated-cache".into();
    input.caches[0].cache_generation = 9;
    input.caches[0].resource_generation = 77;
    input.caches[0].max_candidate_attempts = 8;
    let mut second = input.caches[0].clone();
    second.name = "cache-b".into();
    second.uid = "cache-b-uid".into();
    input.caches.push(second);
    let current = compile_cached(&input, Some(&historical), &mut cache).unwrap();
    for volume in &current.volumes {
        assert_eq!(volume.owners, first.volumes[0].owners);
        assert_eq!(volume.slots, SLOT_COUNT);
        assert_eq!(volume.routing_algorithm, ROUTING_ALGORITHM);
    }
    let cold = compile(&input, None).unwrap();
    assert_eq!(current.volumes, cold.volumes);
    input.nodes[0].uid.push_str("-recreated");
    let replaced = compile_cached(&input, None, &mut cache).unwrap();
    assert_ne!(replaced.volumes[0].owners, cold.volumes[0].owners);
    for (before, after) in cold.volumes[0]
        .owners
        .iter()
        .zip(&replaced.volumes[0].owners)
    {
        assert!(before == after || before == &input.nodes[0].name || after == &input.nodes[0].name);
    }
}

#[test]
fn zero_slot_live_nodes_are_idle_and_remain_in_membership_catalog() {
    let mut generation = compile(&input(4), None).unwrap();
    let by_id: BTreeMap<_, _> = generation
        .nodes
        .iter()
        .map(|(name, member)| (member.id.clone(), name.clone()))
        .collect();
    generation.volumes[0].slots = 1;
    generation.volumes[0].owners =
        place_in_universe(1, "site-a", &by_id.keys().cloned().collect::<Vec<_>>())
            .unwrap()
            .into_iter()
            .map(|id| by_id[&id].clone())
            .collect();
    let topology = Topology::new(&generation).unwrap();
    for (name, member) in &generation.nodes {
        let snapshot = topology.snapshot(&member.id).unwrap();
        if generation.volumes[0].owners.contains(name) {
            assert!(!snapshot.idle);
            assert_eq!(snapshot.volumes.len(), 1);
            assert_eq!(snapshot.member_catalogs[0].members.len(), 4);
        } else {
            assert!(snapshot.idle);
            assert!(snapshot.volumes.is_empty() && snapshot.peers.is_empty());
        }
    }
}

#[test]
fn process_absence_and_return_are_exact_and_node_names_are_not_hash_inputs() {
    let mut inventory = input(7);
    let mut cache = PlacementCache::default();
    let before = compile_cached(&inventory, None, &mut cache).unwrap();
    inventory.nodes[0].ready = false;
    let absent = compile_cached(&inventory, Some(&before), &mut cache).unwrap();
    for (old, new) in before.volumes[0]
        .owners
        .iter()
        .zip(&absent.volumes[0].owners)
    {
        if old != "node-0" {
            assert_eq!(old, new);
        }
        assert_ne!(new, "node-0");
    }
    let removed = Topology::new(&absent)
        .unwrap()
        .snapshot(&before.nodes["node-0"].id)
        .unwrap();
    assert!(!removed.idle && removed.volumes.is_empty());
    inventory.nodes[0].ready = true;
    inventory.nodes[0].pods[0].uid = "returned-process".into();
    let returned = compile_cached(&inventory, Some(&absent), &mut cache).unwrap();
    assert_eq!(returned.volumes[0].owners, before.volumes[0].owners);
    for node in &mut inventory.nodes {
        node.name.push_str("-renamed");
    }
    let renamed = compile_cached(&inventory, Some(&returned), &mut cache).unwrap();
    assert_eq!(
        renamed.volumes[0]
            .owners
            .iter()
            .map(|name| name.strip_suffix("-renamed").unwrap())
            .collect::<Vec<_>>(),
        before.volumes[0].owners
    );
}

#[test]
fn statistical_balance_and_adjacent_repetition_without_quota_repair() {
    for count in [2, 7, 128] {
        let nodes = ids(count);
        let owners = place_in_universe(SLOT_COUNT, "site-a", &nodes).unwrap();
        let mut counts = BTreeMap::new();
        for owner in &owners {
            *counts.entry(owner).or_insert(0usize) += 1;
        }
        let mean = SLOT_COUNT as f64 / count as f64;
        let sigma = (mean * (1.0 - 1.0 / count as f64)).sqrt();
        assert!(
            counts
                .values()
                .all(|&n| (n as f64 - mean).abs() < 6.0 * sigma)
        );
        let repeats = owners.windows(2).filter(|w| w[0] == w[1]).count();
        assert!((repeats as f64 - mean).abs() < 6.0 * sigma);
        assert!(counts.values().any(|&n| n.abs_diff(mean as usize) > 1));
    }
}

/// Run with cargo test --release --test placement cold_ten_thousand -- --ignored --nocapture.
#[test]
#[ignore = "explicit cold 2.6-billion-score performance campaign"]
fn cold_ten_thousand_nodes_and_exact_incremental_performance() {
    let mut nodes = ids(10_000);
    let mut cache = PlacementCache::default();
    let start = Instant::now();
    let cold = cache.place(SLOT_COUNT, "scale", &nodes).unwrap();
    let cold_time = start.elapsed();
    let mut counts = BTreeMap::new();
    for owner in &cold {
        *counts.entry(owner).or_insert(0usize) += 1;
    }
    let mean = SLOT_COUNT as f64 / nodes.len() as f64;
    let chi_square: f64 = nodes
        .iter()
        .map(|id| {
            let n = counts.get(id).copied().unwrap_or(0) as f64;
            (n - mean).powi(2) / mean
        })
        .sum();
    assert!((8500.0..11500.0).contains(&chi_square));
    let start = Instant::now();
    nodes.reverse();
    assert_eq!(cache.place(SLOT_COUNT, "scale", &nodes).unwrap(), cold);
    let unchanged_time = start.elapsed();
    let added_id = identity("node", "extra-scale-node");
    nodes.push(added_id.clone());
    let start = Instant::now();
    let added = cache.place(SLOT_COUNT, "scale", &nodes).unwrap();
    let add_time = start.elapsed();
    let moved = added.iter().zip(&cold).filter(|(a, b)| a != b).count();
    assert!(moved > 0);
    assert!(
        added
            .iter()
            .zip(&cold)
            .all(|(a, b)| a == b || a == &added_id)
    );
    let start = Instant::now();
    assert_eq!(
        added,
        place_in_universe(SLOT_COUNT, "scale", &nodes).unwrap()
    );
    let added_cold_time = start.elapsed();
    nodes.pop();
    let start = Instant::now();
    assert_eq!(cache.place(SLOT_COUNT, "scale", &nodes).unwrap(), cold);
    let remove_time = start.elapsed();
    eprintln!(
        "slots={SLOT_COUNT} nodes=10000 cold={cold_time:?} cold_10001={added_cold_time:?} unchanged={unchanged_time:?} add={add_time:?} remove={remove_time:?} moved={moved} min={} max={} zero={} chi_square={chi_square:.2}",
        counts.values().min().unwrap(),
        counts.values().max().unwrap(),
        nodes.len() - counts.len()
    );
    let mut inventory = input(10_000);
    inventory.universe = "scale".into();
    for node in &mut inventory.nodes {
        node.universe = "scale".into();
    }
    let mut compiler_cache = PlacementCache::default();
    let start = Instant::now();
    let generation = compile_cached(&inventory, None, &mut compiler_cache).unwrap();
    let compile_time = start.elapsed();
    assert_eq!(
        generation.volumes[0]
            .owners
            .iter()
            .map(|name| generation.nodes[name].id.clone())
            .collect::<Vec<_>>(),
        cold
    );
    let start = Instant::now();
    assert_eq!(
        compile_cached(&inventory, Some(&generation), &mut compiler_cache).unwrap(),
        generation
    );
    eprintln!(
        "compile_cold={compile_time:?} compile_unchanged={:?}",
        start.elapsed()
    );
}
