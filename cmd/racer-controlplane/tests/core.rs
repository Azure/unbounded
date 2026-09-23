// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};

use prost::Message;
use racer_controlplane::model::*;
use racer_controlplane::publication::*;
use racer_controlplane::storage::*;
use racer_controlplane::topology::{Topology, compile, degree, place};

fn inventory(count: usize) -> Inventory {
    Inventory {
        universe: "site-a".into(),
        socket_root: SOCKET_ROOT.into(),
        nodes: (0..count)
            .map(|i| Node {
                name: format!("node-{i:05}"),
                uid: format!("uid-{i}"),
                universe: "site-a".into(),
                eligible: true,
                ready: true,
                fabric: "rack-a".into(),
                pods: vec![Pod {
                    uid: format!("pod-{i}"),
                    namespace: "racer".into(),
                    name: format!("racer-{i}"),
                    ip: format!("10.{}.{}.{}", i / 65536, i / 256 % 256, i % 256),
                    available: true,
                    ready: false,
                    created_at: 1,
                }],
            })
            .collect(),
        caches: vec![Cache {
            name: "cache-a".into(),
            uid: "cache-uid".into(),
            resource_generation: 1,
            cache_generation: 0,
            max_candidate_attempts: 3,
        }],
    }
}

fn geometry(slots: u32, count: usize) -> Generation {
    let input = inventory(count);
    let mut g = Generation::empty(&input.universe);
    g.revision = 7;
    for node in &input.nodes {
        let p = &node.pods[0];
        g.nodes.insert(
            node.name.clone(),
            Member {
                id: identity("node", &node.uid),
                ip: Some(p.ip.parse().unwrap()),
                fabric: node.fabric.clone(),
                pod_uid: p.uid.clone(),
                pod_namespace: p.namespace.clone(),
                pod_name: p.name.clone(),
            },
        );
    }
    let names: Vec<_> = g.nodes.keys().cloned().collect();
    g.volumes.push(Volume {
        id: "cache-uid".into(),
        name: "cache-a".into(),
        resource_generation: 1,
        cache_socket: "/dev/racer/cache-a/cache".into(),
        origin_socket: "/dev/racer/cache-a/origin".into(),
        slots,
        cache_generation: 0,
        routing_algorithm: 2,
        max_candidate_attempts: 3,
        owners: place(slots, &names, &[]).unwrap(),
    });
    g.slot_history.insert("cache-uid".into(), slots);
    g
}

#[test]
fn identity_and_socket_contract_survives_the_language_cutover() {
    assert_eq!(
        identity("node", "node-uid"),
        "15be4d45ed47a056073dfd053b9903f3bec1021d8791f13611ce7f424450d6da"
    );
    assert_eq!(
        universe_id_for_site("site-a"),
        "006b082d36689058fb63d9eaeb8af2aa4dc52210879dd3d2a59b409c90e8675c"
    );
    assert_eq!(
        universe_for_site(&"a".repeat(64)),
        "site_77qfj7t24dfw3rs4hl43mhksbh2dtbi5wq6qxjmzom356fkgndvq"
    );
    assert_eq!(
        universe_id_for_site(&"a".repeat(64)),
        "01153fb86c130e17c86d292cd7205d455fc4c6dff8084713429b1a074b31c174"
    );
    for value in ["", "site.a", "A_site.1"] {
        assert_eq!(universe_for_site(value), value);
    }
    assert_eq!(node_site(Some(""), Some("fallback")), "");
    assert_eq!(node_site(None, Some("fallback")), "fallback");
    assert_eq!(
        cache_sockets("/dev//racer/../racer/.", "cache-a").unwrap(),
        (
            "/dev/racer/cache-a/cache".into(),
            "/dev/racer/cache-a/origin".into()
        )
    );
    for (root, name) in [
        ("relative", "a"),
        ("/dev", "A"),
        ("/dev", "a/b"),
        ("/dev", "-a"),
        ("/dev\0", "a"),
    ] {
        assert!(cache_sockets(root, name).is_err());
    }
    assert!(cache_sockets(&format!("/{}", "a".repeat(99)), "b").is_err());
}

fn assert_placement(names: &[String], owners: &[String]) {
    let mut counts = BTreeMap::<&str, usize>::new();
    let mut repeats = 0;
    for (i, owner) in owners.iter().enumerate() {
        assert!(names.contains(owner));
        *counts.entry(owner).or_default() += 1;
        repeats += usize::from(owner == &owners[(i + 1) % owners.len()]);
    }
    assert_eq!(counts.len(), names.len());
    for count in counts.values() {
        assert!(
            *count >= owners.len() / names.len() && *count <= owners.len().div_ceil(names.len())
        );
    }
    if names.len() > 1 {
        assert_eq!(
            repeats,
            usize::from(names.len() == 2 && owners.len() % 2 == 1)
        );
    }
}

#[test]
fn membership_history_is_balanced_diverse_and_stable_across_restart_and_list_order() {
    for slots in [8, 17, SLOT_COUNT] {
        let mut prior = Vec::new();
        for count in [1, 2, 3, 5, 7, 4, 2, 1, 3, 2] {
            let mut names: Vec<_> = (0..count).map(|i| format!("n{i}")).collect();
            let next = place(slots, &names, &prior).unwrap();
            assert_placement(&names, &next);
            let persisted = serde_json::to_vec(&next).unwrap();
            let reloaded: Vec<String> = serde_json::from_slice(&persisted).unwrap();
            names.reverse();
            assert_eq!(place(slots, &names, &reloaded).unwrap(), next);
            assert_eq!(place(slots, &names, &prior).unwrap(), next);
            prior = next;
        }
    }
    // This historical phase differs from a fresh lexical layout and must survive.
    let names = vec!["a".into(), "b".into()];
    let prior = vec!["b".into(), "a".into(), "b".into(), "a".into()];
    assert_eq!(place(4, &names, &prior).unwrap(), prior);
    assert_ne!(place(4, &names, &[]).unwrap(), prior);
    // The odd pair seam is selected for minimum movement, not fixed at slot zero.
    for slots in [7u32, 17] {
        let prior: Vec<_> = (0..slots)
            .map(|s| names[usize::from(s >= slots.div_ceil(2))].clone())
            .collect();
        let actual = place(slots, &names, &prior).unwrap();
        let moved = actual.iter().zip(&prior).filter(|(a, b)| a != b).count();
        let best = (0..slots)
            .map(|shift| {
                (0..slots)
                    .filter(|&s| prior[s as usize] != names[((s + shift) % slots % 2) as usize])
                    .count()
            })
            .min()
            .unwrap();
        assert_eq!(moved, best);
        assert_placement(&names, &actual);
    }
    let names: Vec<String> = ["a", "b", "c", "d"].map(String::from).into();
    let old = place(64, &names, &[]).unwrap();
    let mut joined = names.clone();
    joined.push("e".into());
    let next = place(64, &joined, &old).unwrap();
    assert_eq!(next.iter().zip(&old).filter(|(a, b)| a != b).count(), 12);
    let removed = place(64, &names, &next).unwrap();
    for (old, new) in next.iter().zip(&removed) {
        if old != "e" {
            assert_eq!(old, new);
        }
    }
    for (p, n) in [
        (0, vec!["a".into()]),
        (1, names.clone()),
        (SLOT_COUNT + 1, names),
        (8, vec!["a".into(), "a".into()]),
    ] {
        assert!(place(p, &n, &[]).is_err());
    }
}

#[test]
fn normalized_inventory_selects_processes_and_keeps_removal_authority() {
    let mut input = inventory(2);
    let mut preferred = input.nodes[0].pods[0].clone();
    preferred.uid = "ready-replacement".into();
    preferred.ready = true;
    preferred.created_at = 20;
    input.nodes[0].pods.push(preferred);
    let first = compile(&input, None).unwrap();
    let id = first.nodes["node-00000"].id.clone();
    assert_eq!(first.nodes["node-00000"].pod_uid, "ready-replacement");
    assert!(
        !first.volumes[0].owners.is_empty(),
        "starting Pods need configuration"
    );
    input.nodes.reverse();
    input.nodes[1].pods.reverse();
    assert_eq!(compile(&input, Some(&first)).unwrap(), first);
    let bytes = first.canonical_bytes();
    let loaded = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(compile(&input, Some(&loaded)).unwrap(), first);

    // Recreated node cannot adopt its former process. The old UID still receives removal.
    input.nodes[1].pods.retain(|p| p.uid == "ready-replacement");
    input.nodes[1].uid = "new-node-uid".into();
    let replaced = compile(&input, Some(&first)).unwrap();
    assert!(replaced.nodes["node-00000"].ip.is_none());
    assert!(replaced.nodes.contains_key(&format!("deleted/{id}")));
    let removal = Topology::new(&replaced).unwrap().snapshot(&id).unwrap();
    assert!(!removal.idle && removal.volumes.is_empty());
    input.nodes[1].pods[0].uid = "new-process".into();
    input.nodes[1].pods[0].ready = true;
    let active = compile(&input, Some(&replaced)).unwrap();
    assert!(active.nodes["node-00000"].ip.is_some());

    input.caches.clear();
    let idle = compile(&input, Some(&active)).unwrap();
    let topology = Topology::new(&idle).unwrap();
    assert!(
        topology
            .snapshot(&idle.nodes["node-00000"].id)
            .unwrap()
            .idle
    );
    assert!(!topology.snapshot(&id).unwrap().idle);
    assert!(idle.withdrawn.contains("cache-uid"));
    assert_eq!(idle.slot_history["cache-uid"], SLOT_COUNT);
    assert!(topology.snapshot(&identity("node", "unknown")).is_none());

    input.nodes[1].eligible = false;
    let excluded = compile(&input, Some(&idle)).unwrap();
    assert!(
        !Topology::new(&excluded)
            .unwrap()
            .snapshot(&idle.nodes["node-00000"].id)
            .unwrap()
            .idle
    );
    input.nodes[0].fabric = "bad fabric".into();
    assert!(compile(&input, Some(&excluded)).is_err());
}

#[test]
fn snapshots_match_an_independent_directed_graph_and_volume_scopes() {
    for p in [1, 7, 8, 17, 64, 127] {
        let mut g = geometry(p, (p as usize).min(16));
        g.nodes.get_mut("node-00000").unwrap().ip = Some("2001:db8::1".parse().unwrap());
        let mut second = g.volumes[0].clone();
        second.id = "other-cache-uid".into();
        second.name = "other".into();
        second.cache_socket = "/dev/racer/other/cache".into();
        second.origin_socket = "/dev/racer/other/origin".into();
        second.owners.rotate_left(1);
        g.volumes.push(second);
        let topology = Topology::new(&g).unwrap();
        for (name, member) in &g.nodes {
            let snapshot = topology.snapshot(&member.id).unwrap();
            let encoded = snapshot.encode_to_vec();
            assert_eq!(
                racer_controlplane::proto::Snapshot::decode(encoded.as_slice()).unwrap(),
                snapshot
            );
            assert_eq!(snapshot.revision, 7);
            let mut all_direct = BTreeSet::new();
            for (v, actual) in g.volumes.iter().zip(&snapshot.volumes) {
                let mut edges = BTreeMap::new();
                let mut direct = BTreeSet::new();
                for source in 0..p {
                    for digit in 0..degree(p) {
                        let dest = (source * degree(p) + digit) % p;
                        let from = &v.owners[source as usize];
                        let to = &v.owners[dest as usize];
                        if from == name && to != name {
                            edges.insert(dest, g.nodes[to].id.clone());
                            direct.insert(g.nodes[to].id.clone());
                        }
                        if to == name && from != name {
                            direct.insert(g.nodes[from].id.clone());
                        }
                    }
                }
                let actual_top = actual.topology.as_ref().unwrap();
                assert_eq!(actual_top.epoch, 7);
                assert_eq!(
                    actual_top
                        .neighbors
                        .iter()
                        .map(|e| (e.slot, e.peer.clone()))
                        .collect::<BTreeMap<_, _>>(),
                    edges
                );
                assert_eq!(
                    actual_top.local_slots,
                    v.owners
                        .iter()
                        .enumerate()
                        .filter(|(_, n)| *n == name)
                        .map(|(i, _)| i as u32)
                        .collect::<Vec<_>>()
                );
                assert_eq!(
                    actual.peers.iter().cloned().collect::<BTreeSet<_>>(),
                    edges.into_values().collect()
                );
                assert_eq!(
                    actual
                        .peer_endpoints
                        .as_ref()
                        .unwrap()
                        .peers
                        .iter()
                        .map(|e| e.peer.clone())
                        .collect::<BTreeSet<_>>(),
                    direct
                );
                assert_eq!(actual.cache_socket, v.cache_socket);
                assert_eq!(actual.origin_socket, v.origin_socket);
                all_direct.extend(direct);
            }
            assert_eq!(
                snapshot
                    .peers
                    .iter()
                    .map(|p| p.id.clone())
                    .collect::<BTreeSet<_>>(),
                all_direct
            );
            assert!(snapshot.peers.windows(2).all(|w| w[0].id < w[1].id));
            for peer in &snapshot.peers {
                let remote = g.nodes.values().find(|n| n.id == peer.id).unwrap();
                assert_eq!(peer.pod_uid, remote.pod_uid);
                assert_eq!(
                    peer.http_address,
                    std::net::SocketAddr::new(remote.ip.unwrap(), 9443).to_string()
                );
            }
        }
    }
}

#[test]
fn cache_recreation_empty_membership_and_admission_are_checked_before_commit() {
    let mut input = inventory(1);
    let old = compile(&input, None).unwrap();
    input.caches[0].uid = "recreated-cache".into();
    input.caches[0].cache_generation = 9;
    let new = compile(&input, Some(&old)).unwrap();
    assert!(new.withdrawn.contains("cache-uid"));
    assert_eq!(new.volumes[0].cache_socket, old.volumes[0].cache_socket);
    assert_eq!(new.volumes[0].id, "recreated-cache");
    assert_eq!(new.volumes[0].cache_generation, 9);
    input.nodes[0].ready = false;
    let empty = compile(&input, Some(&new)).unwrap();
    assert!(empty.volumes[0].owners.is_empty());
    let snap = Topology::new(&empty)
        .unwrap()
        .snapshot(&empty.nodes["node-00000"].id)
        .unwrap();
    assert!(!snap.idle && snap.volumes.is_empty());
    input.nodes[0].ready = true;
    for i in 1..5 {
        let mut cache = input.caches[0].clone();
        cache.name = format!("cache-{i}");
        cache.uid = format!("cache-uid-{i}");
        input.caches.push(cache);
    }
    assert!(
        compile(&input, Some(&new)).is_err(),
        "five full local volumes exceed profile work budget"
    );
    let mut malformed = new.clone();
    malformed.volumes[0].owners.pop();
    assert!(Topology::new(&malformed).is_err());
}

#[test]
fn commit_failures_unknown_outcomes_restart_and_stale_leadership_never_publish_intent() {
    let desired = geometry(64, 4);
    let mut state = Publication::<Generation>::default();
    assert!(state.prepare(desired.clone()).is_err());
    let load = state.acquire_leadership();
    state.loaded(load, None).unwrap();
    let commit = state.prepare(desired.clone()).unwrap().unwrap();
    assert!(state.published().is_none());
    assert_eq!(commit.value.revision, 1);
    assert_eq!(commit.digest, commit.value.digest());
    assert_eq!(
        state.complete(commit.ticket, CommitOutcome::Rejected),
        Completion::Unchanged
    );
    assert!(state.published().is_none());
    let commit = state.prepare(desired.clone()).unwrap().unwrap();
    assert_eq!(
        state.complete(commit.ticket, CommitOutcome::Committed),
        Completion::Published
    );
    assert_eq!(state.published().unwrap().revision, 1);
    assert!(state.prepare(desired.clone()).unwrap().is_none());
    let durable = state.published().unwrap().as_ref().clone();

    let mut changed = desired.clone();
    changed.volumes[0].cache_generation += 1;
    let commit = state.prepare(changed.clone()).unwrap().unwrap();
    assert_eq!(commit.expected_digest, Some(durable.digest()));
    assert_eq!(
        state.complete(commit.ticket, CommitOutcome::ReloadRequired),
        Completion::ReloadRequired
    );
    assert_eq!(state.published().unwrap().revision, 1);
    assert!(state.prepare(changed.clone()).is_err());
    let load = state.begin_reload().unwrap();
    assert_eq!(
        state
            .loaded(load, Some(commit.value.as_ref().clone()))
            .unwrap(),
        Completion::Published
    );
    assert_eq!(state.published().unwrap().revision, 2);
    assert!(state.prepare(changed.clone()).unwrap().is_none());

    let bytes = state.published().unwrap().canonical_bytes();
    let mut restarted = Publication::<Generation>::default();
    let load = restarted.acquire_leadership();
    restarted
        .loaded(load, Some(serde_json::from_slice(&bytes).unwrap()))
        .unwrap();
    assert!(restarted.prepare(changed.clone()).unwrap().is_none());
    changed.volumes[0].cache_generation += 1;
    let commit = restarted.prepare(changed.clone()).unwrap().unwrap();
    restarted.lose_leadership();
    assert_eq!(
        restarted.complete(commit.ticket, CommitOutcome::Committed),
        Completion::Stale
    );
    assert_eq!(restarted.published().unwrap().revision, 2);
    let old_load = restarted.acquire_leadership();
    let load = restarted.acquire_leadership();
    assert_eq!(
        restarted
            .loaded(old_load, Some(commit.value.as_ref().clone()))
            .unwrap(),
        Completion::Stale
    );
    restarted
        .loaded(load, Some(commit.value.as_ref().clone()))
        .unwrap();
    assert_eq!(restarted.published().unwrap().revision, 3);
    let load = restarted.begin_reload().unwrap();
    assert!(restarted.loaded(load, Some(durable)).is_err());
    assert!(restarted.needs_reload());
    assert_eq!(restarted.published().unwrap().revision, 3);
}

#[test]
fn unknown_write_that_did_not_commit_reloads_and_can_retry_without_revision_gaps() {
    let mut publication = Publication::default();
    let load = publication.acquire_leadership();
    publication.loaded(load, None).unwrap();
    let commit = publication.prepare(geometry(8, 2)).unwrap().unwrap();
    publication.complete(commit.ticket, CommitOutcome::ReloadRequired);
    let load = publication.begin_reload().unwrap();
    publication.loaded(load, None).unwrap();
    let retry = publication.prepare(geometry(8, 2)).unwrap().unwrap();
    assert_eq!(retry.digest, commit.digest);
    assert_ne!(retry.ticket, commit.ticket);
    assert_eq!(
        publication.complete(commit.ticket, CommitOutcome::Committed),
        Completion::Stale
    );
    assert!(publication.published().is_none());
    publication.complete(retry.ticket, CommitOutcome::Committed);
    assert_eq!(publication.published().unwrap().revision, 1);
}

#[test]
fn exact_quantities_and_last_good_storage_versions_are_independent_of_topology() {
    for (value, expected) in [
        ("32Mi", 32 << 20),
        ("33554433", 36 << 20),
        ("32.5Mi", 36 << 20),
        ("40M", 40 << 20),
        ("4e7", 40 << 20),
        ("40000000000m", 40 << 20),
        ("40000000000000u", 40 << 20),
        ("40000000000000000n", 40 << 20),
        ("+10Gi", 10 << 30),
        ("10240Mi", 10 << 30),
        ("1.5Gi", 1536 << 20),
        ("2.5Ti", 5 << 39),
        ("1Pi", 1 << 50),
        ("7Ei", 7 << 60),
        ("9223372036850581503", MAX_BYTES),
        ("9223372036850581504", MAX_BYTES),
        ("8589934591.99609375Gi", MAX_BYTES),
        ("3.3554432E+7", MIN_BYTES),
        (
            "33554432.000000000000000000000000000000000000000000000000",
            MIN_BYTES,
        ),
    ] {
        assert_eq!(parse_cache_size(value).unwrap(), expected, "{value}");
    }
    for value in [
        "",
        "garbage",
        "10GiB",
        "10GB",
        " 10Gi",
        "10Gi ",
        "NaN",
        "Inf",
        "0",
        "-32Mi",
        "31Mi",
        "1m",
        "1e-100",
        "33554432.1",
        "32.1Mi",
        "33554432001m",
        "33554432.000000001",
        "33554432.00000000000000000000000000000000000001",
        "9223372036850581505",
        "9223372036854775807",
        "8Ei",
        "100000000000000000000Ti",
        "1e100",
        ".Mi",
        "1..0Gi",
        "10K",
        "1e++8",
    ] {
        assert!(parse_cache_size(value).is_err(), "{value}");
    }
    assert_eq!(resolve_cache_size(None, None).unwrap(), DEFAULT_BYTES);
    assert!(resolve_cache_size(Some(""), Some("10Gi")).is_err());
    assert_eq!(
        resolve_cache_size(Some("32Mi"), Some("invalid")).unwrap(),
        MIN_BYTES
    );

    let mut publication = Publication::<StoragePolicy>::default();
    let load = publication.acquire_leadership();
    publication.loaded(load, None).unwrap();
    let initial = StoragePolicy::new(identity("node", "node-uid"), [9; 32]);
    let desired = initial.resolve("site-a", None, Some("33Mi")).unwrap();
    let commit = publication.prepare(desired).unwrap().unwrap();
    assert!(publication.published().is_none());
    publication.complete(commit.ticket, CommitOutcome::Committed);
    let good = publication.published().unwrap().as_ref().clone();
    assert_eq!(good.version, 1);
    assert_eq!(good.desired_bytes, 36 << 20);
    assert!(
        publication
            .prepare(good.resolve("site-a", Some("36Mi"), None).unwrap())
            .unwrap()
            .is_none()
    );
    let invalid = good
        .resolve("site-b", Some("invalid"), Some("10Gi"))
        .unwrap();
    assert_eq!(invalid.version, good.version);
    assert_eq!(invalid.desired_bytes, good.desired_bytes);
    let commit = publication.prepare(invalid).unwrap().unwrap();
    publication.complete(commit.ticket, CommitOutcome::Committed);
    let policy = publication.published().unwrap();
    assert_eq!(policy.revision, 2);
    assert_eq!(policy.wire().unwrap().version, 1);
    assert_eq!(policy.universe, "site-b");
    assert!(policy.validation_error.is_some());
    let next = policy.resolve("site-b", None, Some("10Gi")).unwrap();
    assert_eq!(next.version, 2);
    assert!(next.validation_error.is_none());
    let commit = publication.prepare(next).unwrap().unwrap();
    publication.complete(commit.ticket, CommitOutcome::ReloadRequired);
    assert_eq!(publication.published().unwrap().version, 1);
    let load = publication.begin_reload().unwrap();
    publication
        .loaded(load, Some(commit.value.as_ref().clone()))
        .unwrap();
    assert_eq!(publication.published().unwrap().version, 2);
    let initial_invalid = initial.resolve("site-a", Some("bad"), None).unwrap();
    assert!(initial_invalid.wire().is_none());
}

#[test]
fn ten_thousand_members_have_sparse_recipient_snapshots() {
    let g = geometry(SLOT_COUNT, 10_000);
    let topology = Topology::new(&g).unwrap();
    for name in ["node-00000", "node-05000", "node-09999"] {
        let snapshot = topology.snapshot(&g.nodes[name].id).unwrap();
        let top = snapshot.volumes[0].topology.as_ref().unwrap();
        assert!((26..=27).contains(&top.local_slots.len()));
        assert!(top.neighbors.len() <= top.local_slots.len() * 64);
        assert!(snapshot.peers.len() <= top.local_slots.len() * 128);
        assert!(snapshot.peers.len() < g.nodes.len());
        assert!(snapshot.encoded_len() < 4 * 1024 * 1024);
        assert_eq!(snapshot.member_catalogs.len(), 1);
        assert_eq!(snapshot.member_catalogs[0].members.len(), 10_000);
        assert_eq!(snapshot.volumes[0].member_catalog, Some(0));
        eprintln!(
            "{name}: members={} sparse_endpoints={} wire_bytes={} membership_bytes={}",
            snapshot.member_catalogs[0].members.len(),
            snapshot.peers.len(),
            snapshot.encoded_len(),
            snapshot.member_catalogs[0].encoded_len()
        );
    }
}

#[test]
fn placement_independent_membership_compiler_to_dataplane() {
    let mut old = geometry(64, 16);
    old.volumes[0].owners = (0..64).map(|s| format!("node-{:05}", s % 16)).collect();
    let mut new = old.clone();
    new.revision += 1;
    for owner in &mut new.volumes[0].owners {
        if owner == "node-00000" {
            *owner = "node-00007".into();
        } else if owner == "node-00007" {
            *owner = "node-00000".into();
        }
    }
    // Same inventory participates in every selected volume. One catalog, not
    // sixteen copies of process identities or expanded per-volume endpoint maps.
    for i in 1..16 {
        let mut v = new.volumes[0].clone();
        v.id = format!("cache-{i}");
        v.cache_socket = format!("/dev/racer/cache-{i}/cache");
        v.origin_socket = format!("/dev/racer/cache-{i}/origin");
        new.volumes.push(v);
    }
    let a = &old.nodes["node-00000"].id;
    let b = &old.nodes["node-00002"].id;
    let old_top = Topology::new(&old).unwrap();
    let new_top = Topology::new(&new).unwrap();
    let before = old_top.snapshot(a).unwrap();
    let after = new_top.snapshot(b).unwrap();
    assert_eq!(degree(64), 4);
    assert!(before.volumes[0].peers.contains(b));
    assert!(!after.peers.iter().any(|p| &p.id == a));
    assert!(
        !new_top
            .snapshot(a)
            .unwrap()
            .peers
            .iter()
            .any(|p| &p.id == b)
    );
    assert_eq!(after.member_catalogs.len(), 1);
    assert_eq!(after.member_catalogs[0].members.len(), 16);
    assert!(after.volumes.iter().all(|v| v.member_catalog == Some(0)));
    assert_eq!(before.member_catalogs, after.member_catalogs);
    if let Some(binary) = std::env::var_os("RACER_MEMBERSHIP_DATAPLANE_TEST") {
        let output = std::process::Command::new(binary)
            .args([
                "runtime::tests::membership::compiler_snapshots_http_and_rdma",
                "--exact",
                "--ignored",
                "--nocapture",
            ])
            .env("RACER_MEMBERSHIP_OLD", hex::encode(before.encode_to_vec()))
            .env("RACER_MEMBERSHIP_NEW", hex::encode(after.encode_to_vec()))
            .output()
            .unwrap();
        assert!(
            output.status.success(),
            "dataplane integration failed:\n{}\n{}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        eprintln!("{}", String::from_utf8_lossy(&output.stdout));
    }
}
