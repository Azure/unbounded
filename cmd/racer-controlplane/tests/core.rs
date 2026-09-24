// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};

use prost::Message;
use racer_controlplane::model::*;
#[path = "support/publication.rs"]
mod publication;
use publication::*;
use racer_controlplane::storage::*;
use racer_controlplane::topology::{Topology, compile, place_in_universe};

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
    let mut g = compile(&input, None).unwrap();
    g.revision = 7;
    g.volumes[0].slots = slots;
    g.volumes[0].owners.truncate(slots as usize);
    let product = g.product.as_mut().unwrap();
    product
        .candidates
        .truncate(slots as usize * product.candidate_width as usize);
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
    assert_eq!(node_site(Some("")), "");
    assert_eq!(node_site(None), "");
    assert_eq!(node_site(Some("edge")), "edge");
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
    for owner in owners {
        assert!(names.contains(owner));
        *counts.entry(owner).or_default() += 1;
    }
    if owners.len() == SLOT_COUNT as usize {
        let mean = owners.len() as f64 / names.len() as f64;
        let sigma = (mean * (1.0 - 1.0 / names.len() as f64)).sqrt();
        for name in names {
            let count = counts.get(name.as_str()).copied().unwrap_or(0);
            assert!((count as f64 - mean).abs() <= 6.0 * sigma);
        }
    }
}

fn place(slots: u32, names: &[String]) -> racer_controlplane::Result<Vec<String>> {
    let ids: Vec<_> = names.iter().map(|n| identity("node", n)).collect();
    let by_id: BTreeMap<_, _> = ids.iter().zip(names).collect();
    place_in_universe(slots, "placement-fixture", &ids)
        .map(|owners| owners.iter().map(|id| by_id[id].clone()).collect())
}

#[test]
fn membership_history_is_balanced_diverse_and_stable_across_restart_and_list_order() {
    for slots in [8, 17, SLOT_COUNT] {
        for count in [1, 2, 3, 5, 7, 4, 2, 1, 3, 2] {
            let mut names: Vec<_> = (0..count).map(|i| format!("n{i}")).collect();
            let next = place(slots, &names).unwrap();
            assert_placement(&names, &next);
            let persisted = serde_json::to_vec(&next).unwrap();
            let reloaded: Vec<String> = serde_json::from_slice(&persisted).unwrap();
            names.reverse();
            assert_eq!(reloaded, next);
            assert_eq!(place(slots, &names).unwrap(), next);
        }
    }
    let names: Vec<String> = ["a", "b", "c", "d"].map(String::from).into();
    let old = place(64, &names).unwrap();
    let mut joined = names.clone();
    joined.push("e".into());
    let next = place(64, &joined).unwrap();
    assert!(next.iter().any(|owner| owner == "e"));
    for (new, old) in next.iter().zip(&old) {
        assert!(new == old || new == "e");
    }
    let removed = place(64, &names).unwrap();
    assert_eq!(removed, old);
    for (old, new) in next.iter().zip(&removed) {
        if old != "e" {
            assert_eq!(old, new);
        }
    }
    // More participants than slots is valid; some participants own zero slots.
    let one = place(1, &names).unwrap();
    assert_eq!(one.len(), 1);
    assert_placement(&names, &one);
    for (p, n) in [
        (0, vec!["a".into()]),
        (SLOT_COUNT + 1, names),
        (8, vec!["a".into(), "a".into()]),
    ] {
        assert!(place(p, &n).is_err());
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

    // Current Kubernetes inventory is authoritative, including recreation. The
    // subscription adapter handles identities absent from the compiled catalog.
    input.nodes[1].pods.retain(|p| p.uid == "ready-replacement");
    input.nodes[1].uid = "new-node-uid".into();
    let replaced = compile(&input, Some(&first)).unwrap();
    assert!(replaced.nodes["node-00000"].ip.is_some());
    assert_ne!(replaced.nodes["node-00000"].id, id);
    assert_eq!(replaced.volumes, compile(&input, None).unwrap().volumes);
    assert!(Topology::new(&replaced).unwrap().snapshot(&id).is_none());
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
    assert!(topology.snapshot(&id).is_none());
    assert!(topology.snapshot(&identity("node", "unknown")).is_none());

    input.nodes[1].eligible = false;
    let excluded = compile(&input, Some(&idle)).unwrap();
    assert!(
        Topology::new(&excluded)
            .unwrap()
            .snapshot(&idle.nodes["node-00000"].id)
            .is_none()
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
            assert_eq!(snapshot.idle, false);
            assert_eq!(snapshot.volumes.len(), g.volumes.len());
            for actual in &snapshot.volumes {
                let v = g.volumes.iter().find(|v| v.id == actual.id).unwrap();
                let actual_top = actual.topology.as_ref().unwrap();
                let product = actual_top.product.as_ref().unwrap();
                let graph = racer_controlplane::product::Product::new(
                    product.left_factor,
                    product.right_factor,
                )
                .unwrap();
                let adjacent = graph.neighbors(product.roles[product.local_member as usize]);
                let direct: BTreeSet<_> = product
                    .members
                    .iter()
                    .zip(&product.roles)
                    .filter(|(_, role)| adjacent.contains(role))
                    .map(|(id, _)| id.clone())
                    .collect();
                assert_eq!(actual_top.epoch, 7);
                assert_eq!(actual_top.routing_algorithm, Some(1));
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
                    direct
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
    assert_eq!(new, compile(&input, None).unwrap());
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
    let mut legacy = compile(&input, Some(&new)).unwrap();
    legacy.product = None;
    for volume in &mut legacy.volumes {
        volume.routing_algorithm = PRODUCT_ROUTING_ALGORITHM;
    }
    assert!(
        Topology::new(&legacy).is_err(),
        "product topology is mandatory"
    );
    let mut malformed = new.clone();
    malformed.volumes[0].owners.pop();
    assert!(Topology::new(&malformed).is_err());
}

#[test]
fn page_striping_algorithm_survives_compile_persistence_and_wire_publication() {
    let input = inventory(1);
    let compiled = compile(&input, None).unwrap();
    assert_eq!(
        compiled.volumes[0].routing_algorithm,
        PRODUCT_ROUTING_ALGORITHM
    );
    let mut publication = Publication::<Generation>::default();
    publication.publish(compiled, 1).unwrap();
    // Serialization remains a wire/fixture contract, not restart authority.
    let loaded: Generation =
        serde_json::from_slice(&publication.published().unwrap().canonical_bytes()).unwrap();
    assert_eq!(compile(&input, Some(&loaded)).unwrap(), loaded);
    let snapshot = Topology::new(&loaded)
        .unwrap()
        .snapshot(&loaded.nodes["node-00000"].id)
        .unwrap();
    let wire = snapshot.encode_to_vec();
    let decoded = racer_controlplane::proto::Snapshot::decode(wire.as_slice()).unwrap();
    assert_eq!(
        decoded.volumes[0]
            .topology
            .as_ref()
            .unwrap()
            .routing_algorithm,
        Some(PRODUCT_ROUTING_ALGORITHM)
    );
    // Old object routing must not be served to a page-striped dataplane.
    for algorithm in [0, 3, 4, u32::MAX] {
        let mut invalid = loaded.clone();
        invalid.volumes[0].routing_algorithm = algorithm;
        assert!(Topology::new(&invalid).is_err(), "algorithm {algorithm}");
        assert!(
            publication.publish(invalid, 2).is_err(),
            "algorithm {algorithm}"
        );
        assert_eq!(publication.published().unwrap().as_ref(), &loaded);
    }
}

#[test]
fn commit_failures_unknown_outcomes_restart_and_stale_leadership_never_publish_intent() {
    let desired = geometry(64, 4);
    let mut state = Publication::<Generation>::default();
    // Authority and uncertain reservation outcomes are checked by the runtime.
    // Until it supplies a reserved revision there is no local publication.
    assert!(state.publish(desired.clone(), 0).is_err());
    assert!(state.published().is_none());
    let mut invalid = desired.clone();
    invalid.volumes[0].owners.pop();
    assert!(state.publish(invalid.clone(), 1).is_err());
    assert!(state.published().is_none());
    let first = state.publish(desired.clone(), 2).unwrap();
    assert_eq!(first.revision, 2);
    let mut changed = desired.clone();
    changed.volumes[0].cache_generation += 1;
    for revision in [0, 1, 2] {
        assert!(state.publish(changed.clone(), revision).is_err());
        assert!(std::sync::Arc::ptr_eq(state.published().unwrap(), &first));
    }
    assert!(state.publish(invalid.clone(), 3).is_err());
    let mut foreign = changed.clone();
    foreign.universe = "other-universe".into();
    assert!(state.publish(foreign, 3).is_err());
    assert!(std::sync::Arc::ptr_eq(state.published().unwrap(), &first));
    let next = state.publish(changed, 4).unwrap();
    assert_eq!(next.revision, 4);
    assert_eq!(next.volumes[0].cache_generation, 1);
    assert_eq!(first.volumes[0].cache_generation, 0);

    // Restart has no payload to reload. Only a successful fresh rebuild with a
    // newly reserved range can establish a publication, and gaps are expected.
    let mut restarted = Publication::<Generation>::default();
    assert!(restarted.published().is_none());
    let reserved = racer_controlplane::kubernetes::RANGE_SIZE + 1;
    assert!(restarted.publish(invalid, reserved).is_err());
    assert!(restarted.published().is_none());
    let rebuilt = compile(&inventory(2), None).unwrap();
    let current = restarted.publish(rebuilt.clone(), reserved + 1).unwrap();
    assert_eq!(current.nodes, rebuilt.nodes);
    assert_eq!(current.volumes, rebuilt.volumes);
    assert!(restarted.publish(desired, next.revision).is_err());
    assert!(std::sync::Arc::ptr_eq(
        restarted.published().unwrap(),
        &current
    ));
}

#[test]
fn unknown_write_that_did_not_commit_reloads_and_can_retry_without_revision_gaps() {
    let mut publication = Publication::default();
    let desired = geometry(8, 2);
    // No publish call is made while reservation is uncertain. Retrying uses a
    // fresh reserved range rather than recovering a durable topology payload.
    // The historical test name is retained; gaps are valid in the new contract.
    assert!(publication.published().is_none());
    let revision = racer_controlplane::kubernetes::RANGE_SIZE + 1;
    let retry = publication.publish(desired.clone(), revision).unwrap();
    assert_eq!(retry.revision, revision);
    assert_eq!(retry.volumes, desired.volumes);
    for stale in [1, revision - 1, revision] {
        assert!(publication.publish(desired.clone(), stale).is_err());
        assert!(std::sync::Arc::ptr_eq(
            publication.published().unwrap(),
            &retry
        ));
    }
    let successor = revision + racer_controlplane::kubernetes::RANGE_SIZE;
    let next = publication.publish(desired, successor).unwrap();
    assert_eq!(next.revision, successor);
    assert_eq!(next.volumes, retry.volumes);
}

#[test]
fn exact_quantities_and_last_good_storage_versions_are_independent_of_topology() {
    for (value, expected) in [
        ("512Mi", 512 << 20),
        ("536870912", 512 << 20),
        ("536870913", 576 << 20),
        ("576Mi", 576 << 20),
        ("513Mi", 576 << 20),
        ("512.5Mi", 576 << 20),
        ("640M", 640 << 20),
        ("64e7", 640 << 20),
        ("640000000000m", 640 << 20),
        ("640000000000000u", 640 << 20),
        ("640000000000000000n", 640 << 20),
        ("+10Gi", 10 << 30),
        ("10240Mi", 10 << 30),
        ("1.5Gi", 1536 << 20),
        ("2.5Ti", 5 << 39),
        ("1Pi", 1 << 50),
        ("7Ei", 7 << 60),
        ("9223372036787666943", MAX_BYTES),
        ("9223372036787666944", MAX_BYTES),
        ("8589934591.9375Gi", MAX_BYTES),
        ("5.36870912E+8", MIN_BYTES),
        (
            "536870912.000000000000000000000000000000000000000000000000",
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
        "-512Mi",
        "511Mi",
        "536870911",
        "32Mi",
        "64Mi",
        "96Mi",
        "1m",
        "1e-100",
        "536870912.1",
        "512.1Mi",
        "536870912001m",
        "536870912.000000001",
        "536870912.00000000000000000000000000000000000001",
        "9223372036787666945",
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
        resolve_cache_size(Some("512Mi"), Some("invalid")).unwrap(),
        MIN_BYTES
    );

    let mut publication = Publication::<StoragePolicy>::default();
    let initial = StoragePolicy::new(identity("node", "node-uid"), [9; 32]);
    let desired = initial.resolve("site-a", None, Some("513Mi")).unwrap();
    assert!(publication.published().is_none());
    publication.publish(desired, 1).unwrap();
    let good = publication.published().unwrap().as_ref().clone();
    assert_eq!(good.version, 1);
    assert_eq!(good.desired_bytes, 576 << 20);
    assert_eq!(good.resolve("site-a", Some("576Mi"), None).unwrap(), good);
    let invalid = good
        .resolve("site-b", Some("invalid"), Some("10Gi"))
        .unwrap();
    assert_eq!(invalid.version, good.version);
    assert_eq!(invalid.desired_bytes, good.desired_bytes);
    publication.publish(invalid, 2).unwrap();
    let policy = publication.published().unwrap();
    assert_eq!(policy.revision, 2);
    assert!(policy.wire().is_none());
    assert_eq!(policy.universe, "site-b");
    assert!(policy.validation_error.is_some());
    let next = policy.resolve("site-b", None, Some("10Gi")).unwrap();
    assert_eq!(next.version, 2);
    assert!(next.validation_error.is_none());
    assert!(publication.publish(next.clone(), 2).is_err());
    assert_eq!(publication.published().unwrap().version, 1);
    publication.publish(next, 3).unwrap();
    assert_eq!(publication.published().unwrap().version, 2);
    let initial_invalid = initial.resolve("site-a", Some("bad"), None).unwrap();
    assert!(initial_invalid.wire().is_none());
}

#[test]
fn page_geometry_rejects_obsolete_capacity_without_replacing_last_good_policy() {
    let mut publication = Publication::<StoragePolicy>::default();
    let desired = StoragePolicy::new(identity("node", "node-uid"), [9; 32])
        .resolve("site-a", Some("1Gi"), None)
        .unwrap();
    publication.publish(desired, 1).unwrap();
    let good = publication.published().unwrap().as_ref().clone();
    for bytes in [32 << 20, 64 << 20, 96 << 20, 516 << 20, MAX_BYTES + 1] {
        let mut malformed = good.clone();
        malformed.desired_bytes = bytes;
        assert!(publication.publish(malformed, 2).is_err(), "bytes {bytes}");
        assert_eq!(publication.published().unwrap().as_ref(), &good);
    }
    for quantity in ["32Mi", "64Mi", "96Mi", "511Mi"] {
        let invalid = good.resolve("site-a", Some(quantity), Some("2Ti")).unwrap();
        assert!(invalid.validation_error.is_some());
        assert_eq!(invalid.desired_bytes, good.desired_bytes);
        assert_eq!(invalid.version, good.version);
        assert!(invalid.wire().is_none());
    }
    let resized = good.resolve("site-a", Some("1536Mi"), None).unwrap();
    assert_eq!(resized.desired_bytes, 1536 << 20);
    assert_eq!(resized.version, good.version + 1);
    assert_eq!(
        resized.resolve("site-a", Some("1610612736"), None).unwrap(),
        resized
    );
}

#[test]
fn ten_thousand_members_have_sparse_recipient_snapshots() {
    let g = geometry(SLOT_COUNT, 10_000);
    let topology = Topology::new(&g).unwrap();
    for name in ["node-00000", "node-05000", "node-09999"] {
        let snapshot = topology.snapshot(&g.nodes[name].id).unwrap();
        let top = snapshot.volumes[0].topology.as_ref().unwrap();
        assert!((1..=64).contains(&top.local_slots.len()));
        assert!(top.product.is_some());
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
    let old = geometry(64, 16);
    let mut new = old.clone();
    new.revision += 1;
    new.product.as_mut().unwrap().roles.swap(0, 7);
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
    let old_top = Topology::new(&old).unwrap();
    let new_top = Topology::new(&new).unwrap();
    let before = old_top.snapshot(a).unwrap();
    let b = &before.volumes[0].peers[0];
    let after = new_top.snapshot(b).unwrap();
    assert!(before.volumes[0].peers.contains(b));
    assert_eq!(after.member_catalogs.len(), 1);
    assert_eq!(after.member_catalogs[0].members.len(), 16);
    assert!(after.volumes.iter().all(|v| v.member_catalog == Some(0)));
    assert_eq!(before.member_catalogs, after.member_catalogs);
    if let Some(binary) = std::env::var_os("RACER_MEMBERSHIP_DATAPLANE_TEST") {
        let mut command = membership_command(&binary, std::time::Duration::from_secs(60));
        command
            .args([
                "runtime::tests::membership::compiler_snapshots_http_and_rdma",
                "--exact",
                "--ignored",
                "--nocapture",
            ])
            .env("RACER_MEMBERSHIP_OLD", hex::encode(before.encode_to_vec()))
            .env("RACER_MEMBERSHIP_NEW", hex::encode(after.encode_to_vec()));
        membership_status(&mut command, std::time::Duration::from_secs(65)).unwrap();
    }
}

// GNU timeout owns the child process group, including descendants. Inherit the
// suite's output instead of waiting on pipes that a descendant can keep open.
fn membership_command(
    binary: &std::ffi::OsStr,
    limit: std::time::Duration,
) -> std::process::Command {
    let mut command = std::process::Command::new("timeout");
    command
        .args(["--signal=TERM", "--kill-after=2s"])
        .arg(format!("{}s", limit.as_secs_f64()))
        .arg(binary)
        .stdout(std::process::Stdio::inherit())
        .stderr(std::process::Stdio::inherit());
    command
}

fn membership_status(
    command: &mut std::process::Command,
    limit: std::time::Duration,
) -> std::io::Result<()> {
    use std::time::{Duration, Instant};
    let mut child = command.spawn()?;
    let deadline = Instant::now() + limit;
    loop {
        if let Some(status) = child.try_wait()? {
            return if status.success() {
                Ok(())
            } else {
                Err(std::io::Error::other(format!(
                    "dataplane integration failed: {status}"
                )))
            };
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            let reap_deadline = Instant::now() + Duration::from_secs(2);
            while Instant::now() < reap_deadline {
                if child.try_wait()?.is_some() {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::TimedOut,
                        "membership supervisor timed out",
                    ));
                }
                std::thread::sleep(Duration::from_millis(10));
            }
            return Err(std::io::Error::new(
                std::io::ErrorKind::TimedOut,
                format!(
                    "membership supervisor PID {} remains unreaped after KILL",
                    child.id()
                ),
            ));
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

#[test]
fn membership_subprocess_success_failure_and_timeout() {
    use std::time::{Duration, Instant};
    for (script, success) in [
        ("exit 0", true),
        ("exit 7", false),
        ("trap '' TERM; sleep 30 & wait", false),
    ] {
        let mut command =
            membership_command(std::ffi::OsStr::new("sh"), Duration::from_millis(100));
        command.args(["-c", script]);
        let started = Instant::now();
        assert_eq!(
            membership_status(&mut command, Duration::from_secs(4)).is_ok(),
            success
        );
        assert!(
            started.elapsed() < Duration::from_secs(7),
            "unbounded membership child: {script}"
        );
    }
}
