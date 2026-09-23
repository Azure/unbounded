// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use std::io::Write;

pub(crate) fn test_socket(address: SocketAddr, kind: &str) -> String {
    std::env::temp_dir()
        .join(format!(
            "racer-{}-{}-{kind}",
            std::process::id(),
            address.port()
        ))
        .to_str()
        .unwrap()
        .to_owned()
}

#[test]
fn topology_reload_retains_wire_epoch_and_rejects_unknown_or_malformed_cursors() {
    use crate::runtime::tests::Cluster;
    let Some(mut c) = Cluster::new() else { return };
    let target = c.target(7, "reload-wire");
    let routing = c.config(0).volumes[0].routing.clone();
    let (_, cursor) = routing.next(&routing.start(&target)).unwrap().unwrap();
    let wire = |cursor: &crate::routing::Cursor| {
        let mut bytes = b"RF04".to_vec();
        bytes.extend(5000u32.to_le_bytes());
        bytes.extend(cursor.algorithm.magic());
        bytes.extend(cursor.encode());
        bytes.extend(b"RF05\0");
        bytes.extend(target.as_bytes());
        bytes.iter().map(|b| format!("{b:02x}")).collect::<String>()
    };
    c.reload(1, Some(2));
    let result = c.get_headers(1, "/", &[("X-Racer-Fault", &wire(&cursor))]);
    assert_eq!(result.0, 200);
    assert_eq!(result.1.len(), 48);
    assert_eq!(c.hits.lock().unwrap().len(), 1);
    let current = c.config(1);
    let new_routing = &current.volumes[0].routing;
    assert_eq!(new_routing.algorithm, crate::routing::Algorithm::Canonical);
    assert_ne!(new_routing.identity, routing.identity);
    let current_cursor = new_routing.start(&target);
    assert_eq!(
        c.get_headers(1, "/", &[("X-Racer-Fault", &wire(&current_cursor))])
            .0,
        200
    );
    let unknown = wire(&cursor).replacen("52463035", "52463039", 1);
    assert_eq!(c.get_headers(1, "/", &[("X-Racer-Fault", &unknown)]).0, 409);
    for (field, status) in [("position", 400), ("identity", 409), ("attempt", 400)] {
        let mut bad = cursor.clone();
        match field {
            "position" => bad.position = 3,
            "identity" => bad.identity[0] ^= 1,
            _ => bad.attempt = 8,
        }
        assert_eq!(
            c.get_headers(1, "/", &[("X-Racer-Fault", &wire(&bad))]).0,
            status
        );
    }
    assert_eq!(c.hits.lock().unwrap().len(), 1);
    c.expire_previous(1);
    let mut expired = cursor;
    expired.attempt = 0;
    expired.position = 1;
    assert_eq!(
        c.get_headers(1, "/", &[("X-Racer-Fault", &wire(&expired))])
            .0,
        409
    );
}

pub(crate) fn fixture() -> (Trust, proto::Snapshot) {
    let trust = Trust {
        universe: [1; 32],
        node: [2; 32],
    };
    let snapshot = proto::Snapshot {
        universe: trust.universe.to_vec(),
        node: trust.node.to_vec(),
        revision: 1,
        peers: vec![proto::Peer {
            pod_uid: "test-pod".into(),
            id: "p1".into(),
            http_address: "127.0.0.1:8081".into(),
            ..Default::default()
        }],
        volumes: vec![proto::Volume {
            id: "v1".into(),
            cache_socket: "/dev/racer/v1/cache".into(),
            origin_socket: "/dev/racer/v1/origin".into(),
            peers: vec!["p1".into()],
            peer_endpoints: Some(proto::VolumePeerEndpoints {
                peers: vec![proto::VolumePeerEndpoint {
                    peer: "p1".into(),
                    http_address: String::new(),
                }],
            }),
            topology: Some(proto::Topology {
                routing_algorithm: None,
                epoch: 1,
                slot_count: 2,
                local_slots: vec![0],
                neighbors: vec![proto::SlotPeer {
                    slot: 1,
                    peer: "p1".into(),
                }],
            }),
            ..Default::default()
        }],
        ..Default::default()
    };
    (trust, snapshot)
}
pub(crate) fn prepare_snapshot(trust: &Trust, config: proto::Snapshot) -> Prepared {
    trust.prepare(envelope(config)).unwrap()
}
pub(crate) fn scope_peers(snapshot: &mut proto::Snapshot) {
    for volume in &mut snapshot.volumes {
        if volume.peer_endpoints.as_ref().is_some_and(|scope| {
            scope.peers.len() != 1
                || scope.peers[0].peer != "p1"
                || !scope.peers[0].http_address.is_empty()
        }) {
            continue;
        }
        volume.peer_endpoints = Some(proto::VolumePeerEndpoints {
            peers: snapshot
                .peers
                .iter()
                .map(|peer| proto::VolumePeerEndpoint {
                    peer: peer.id.clone(),
                    http_address: String::new(),
                })
                .collect(),
        });
    }
}
/// Replicas share the P2PCache UID while using node-local origins.
pub(crate) fn prepare_cluster_snapshot(trust: &Trust, config: proto::Snapshot) -> Prepared {
    prepare_snapshot(trust, config)
}

/// Shared eight-node configuration for virtual and real transport scenarios.
pub(crate) fn cluster_config(
    node: usize,
    addresses: &[std::net::SocketAddr],
    backend: std::net::SocketAddr,
    algorithm: Option<u32>,
    fabric: &str,
) -> proto::Snapshot {
    let (_, mut config) = fixture();
    config.node = vec![node as u8 + 10; 32];
    config.fabric = fabric.into();
    config.peers.clear();
    let volume = &mut config.volumes[0];
    volume.cache_socket = test_socket(addresses[node], "cache");
    volume.origin_socket = test_socket(backend, "origin");
    volume.peers.clear();
    let mut neighbors = Vec::new();
    let id = |n: usize| {
        crate::peer_identity::NodeId::from_bytes(&[n as u8 + 10; 32])
            .unwrap()
            .to_string()
    };
    for next in [node * 2 % 8, (node * 2 + 1) % 8] {
        if next != node {
            volume.peers.push(id(next));
            neighbors.push(proto::SlotPeer {
                slot: next as u32,
                peer: id(next),
            });
            config.peers.push(proto::Peer {
                pod_uid: format!("pod-{next}"),
                id: id(next),
                http_address: addresses[next].to_string(),
                fabric: config.fabric.clone(),
            });
        }
    }
    volume.topology = Some(proto::Topology {
        routing_algorithm: algorithm,
        epoch: 1,
        slot_count: 8,
        local_slots: vec![node as u32],
        neighbors,
    });
    for source in 0..8 {
        if source != node
            && [source * 2 % 8, (source * 2 + 1) % 8].contains(&node)
            && !config.peers.iter().any(|p| p.id == id(source))
        {
            config.peers.push(proto::Peer {
                pod_uid: format!("pod-{source}"),
                id: id(source),
                http_address: addresses[source].to_string(),
                fabric: config.fabric.clone(),
            });
        }
    }
    config
}
pub(crate) fn runtime_pair(
    node: u8,
    listen: std::net::SocketAddr,
    remote: std::net::SocketAddr,
    _outbound: bool,
) -> Prepared {
    use crate::peer_identity::NodeId;
    let (mut trust, mut config) = fixture();
    trust.node = [node; 32];
    config.node = trust.node.to_vec();
    config.fabric = "runtime-fabric".into();
    config.volumes[0].cache_socket = test_socket(listen, "cache");
    config.peers[0].id = NodeId::from_bytes(&[if node == 2 { 3 } else { 2 }; 32])
        .unwrap()
        .to_string();
    config.peers[0].fabric = config.fabric.clone();
    config.peers[0].http_address = remote.to_string();
    config.volumes[0].topology = Some(proto::Topology {
        routing_algorithm: None,
        epoch: 1,
        slot_count: 2,
        local_slots: vec![if node == 2 { 0 } else { 1 }],
        neighbors: vec![proto::SlotPeer {
            slot: if node == 2 { 1 } else { 0 },
            peer: config.peers[0].id.clone(),
        }],
    });
    config.volumes[0].peers = vec![config.peers[0].id.clone()];
    prepare_snapshot(&trust, config)
}

#[test]
fn tls_configuration_rejects_tcp_ingress_and_missing_pod_identity() {
    let (trust, mut config) = fixture();
    config.volumes[0].cache_socket = "127.0.0.1:9443".into();
    assert!(trust.prepare_http(envelope(config)).is_err());
    let (_, mut config) = fixture();
    config.peers[0].pod_uid.clear();
    assert!(trust.prepare_http(envelope(config)).is_err());
    assert!(Source::parse("http://127.0.0.1:8443/v3/control").is_err());
    assert!(Source::parse("https://user:password@127.0.0.1:8443/v3/control").is_err());
}
fn envelope(mut snapshot: proto::Snapshot) -> proto::Configuration {
    scope_peers(&mut snapshot);
    proto::Configuration {
        contents: Some(proto::configuration::Contents::Snapshot(snapshot)),
    }
}

#[test]
fn protojson_wire_and_complete_replacement() {
    let (trust, first) = fixture();
    let wire = envelope(first.clone());
    let json = serde_json::to_string(&wire).unwrap();
    assert!(json.contains("\"revision\":\"1\""));
    assert_eq!(
        serde_json::from_str::<proto::Configuration>(&json).unwrap(),
        wire
    );
    assert_eq!(
        proto::Configuration::decode(wire.encode_to_vec().as_slice()).unwrap(),
        wire
    );
    for epoch in [0, 42, u64::MAX] {
        let mut snapshot = first.clone();
        snapshot.epoch = epoch;
        let wire = envelope(snapshot);
        let json = serde_json::to_string(&wire).unwrap();
        // The legacy fixture omits snapshot.epoch (topology has its own).
        let value: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(value["snapshot"].get("epoch").is_some(), epoch != 0);
        let decoded = serde_json::from_str::<proto::Configuration>(&json).unwrap();
        assert_eq!(decoded, wire);
        assert_eq!(trust.prepare(decoded).unwrap().config.epoch, epoch);
        assert_eq!(
            proto::Configuration::decode(wire.encode_to_vec().as_slice()).unwrap(),
            wire
        );
    }
    for algorithm in [None, Some(0), Some(1), Some(2), Some(u32::MAX)] {
        let mut config = first.clone();
        config.volumes[0]
            .topology
            .as_mut()
            .unwrap()
            .routing_algorithm = algorithm;
        let encoded = envelope(config);
        let json = serde_json::to_string(&encoded).unwrap();
        assert_eq!(json.contains("routingAlgorithm"), algorithm.is_some());
        assert_eq!(
            serde_json::from_str::<proto::Configuration>(&json).unwrap(),
            encoded
        );
        assert_eq!(
            proto::Configuration::decode(encoded.encode_to_vec().as_slice()).unwrap(),
            encoded
        );
        assert_eq!(
            trust.prepare(encoded).is_ok(),
            matches!(algorithm, None | Some(2))
        );
    }
    let updates = Updates::default();
    updates.publish(trust.prepare(wire).unwrap()).unwrap();
    for cap in [
        None,
        Some(0),
        Some(1),
        Some(3),
        Some(8),
        Some(9),
        Some(u32::MAX),
    ] {
        let mut config = first.clone();
        config.volumes[0].max_candidate_attempts = cap;
        let wire = envelope(config);
        let json = serde_json::to_string(&wire).unwrap();
        assert_eq!(
            serde_json::from_str::<proto::Configuration>(&json).unwrap(),
            wire
        );
        assert_eq!(
            proto::Configuration::decode(wire.encode_to_vec().as_slice()).unwrap(),
            wire
        );
        assert_eq!(
            trust.prepare(wire).is_ok(),
            cap.is_none_or(|n| (1..=8).contains(&n))
        );
    }
    let pinned = updates.latest(0).unwrap();
    let mut next = first.clone();
    next.revision = 2;
    next.peers.clear();
    next.volumes.clear();
    updates
        .publish(trust.prepare(envelope(next.clone())).unwrap())
        .unwrap();
    assert!(updates.latest(1).unwrap().volumes.is_empty());
    assert_eq!(pinned.volumes.len(), 1);
    assert_eq!(pinned.peers.len(), 1);
    assert!(
        updates
            .publish(trust.prepare(envelope(first)).unwrap())
            .is_err()
    );
    next.fabric = "changed".into();
    assert!(
        updates
            .publish(trust.prepare(envelope(next)).unwrap())
            .is_err()
    );
}

#[test]
fn rejects_partial_invalid_and_untrusted_snapshots() {
    let (trust, original) = fixture();
    for mutate in [
        |s: &mut proto::Snapshot| s.volumes.push(s.volumes[0].clone()),
        |s: &mut proto::Snapshot| s.peers.clear(),
        |s: &mut proto::Snapshot| s.universe[0] ^= 1,
        |s: &mut proto::Snapshot| s.node[0] ^= 1,
        |s: &mut proto::Snapshot| s.revision = 0,
        |s: &mut proto::Snapshot| s.volumes[0].origin_socket = "https://example.org".into(),
    ] {
        let mut s = original.clone();
        mutate(&mut s);
        assert!(trust.prepare(envelope(s)).is_err());
    }
    assert!(trust.prepare_http(envelope(original.clone())).is_ok());
    let mut unscoped = original.clone();
    unscoped.volumes[0].peer_endpoints = None;
    assert!(
        trust
            .prepare(proto::Configuration {
                contents: Some(proto::configuration::Contents::Snapshot(unscoped)),
            })
            .is_err()
    );
    assert!(trust.prepare(envelope(original)).is_ok());
    assert!(serde_json::from_str::<proto::Configuration>(r#"{"snapshot":{"unknown":1}}"#).is_err());
}

#[test]
fn topology_validation_and_reload_are_atomic() {
    let (trust, original) = fixture();
    for mutate in [
        |v: &mut proto::Volume| v.topology = None,
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().epoch = 0,
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().slot_count = 0,
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().routing_algorithm = Some(0),
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().routing_algorithm = Some(1),
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().routing_algorithm = Some(3),
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().local_slots.push(0),
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().neighbors.clear(),
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().neighbors[0].slot = 0,
        |v: &mut proto::Volume| v.topology.as_mut().unwrap().neighbors[0].peer = "unknown".into(),
    ] {
        let mut config = original.clone();
        mutate(&mut config.volumes[0]);
        assert!(trust.prepare(envelope(config)).is_err());
    }
    let updates = Updates::default();
    updates
        .publish(trust.prepare(envelope(original.clone())).unwrap())
        .unwrap();
    let mut next = original.clone();
    next.revision = 2;
    next.volumes[0].origin_socket = "/dev/racer/v1/rebound-origin".into();
    assert!(
        updates
            .publish(trust.prepare(envelope(next.clone())).unwrap())
            .is_err()
    );
    next.peers[0].http_address = "127.0.0.1:8099".into();
    assert!(
        updates
            .publish(trust.prepare(envelope(next.clone())).unwrap())
            .is_err()
    );
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    next.volumes[0].topology.as_mut().unwrap().epoch = 2;
    updates
        .publish(trust.prepare(envelope(next)).unwrap())
        .unwrap();
    let mut standalone = original;
    standalone.volumes[0].peers.clear();
    standalone.volumes[0].topology = None;
    standalone.peers.clear();
    assert_eq!(
        trust.prepare(envelope(standalone)).unwrap().volumes[0]
            .routing
            .geometry
            .slot_count(),
        1
    );
}

#[test]
fn activation_requires_every_worker_and_failure_blocks_activation() {
    let (trust, snapshot) = fixture();
    let updates = Arc::new(Updates::default());
    let registry = crate::metrics::Registry::new(2, updates.clone());
    let assert_epoch = |epoch| {
        assert_eq!(updates.applied_epoch(), epoch);
        let metrics = registry.render();
        assert!(metrics.contains("# TYPE racer_dataplane_config_epoch gauge\n"));
        assert!(metrics.contains(&format!("\nracer_dataplane_config_epoch {epoch}\n")));
    };
    assert_epoch(0);
    for _ in 0..2 {
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    }
    updates
        .publish(trust.prepare(envelope(snapshot.clone())).unwrap())
        .unwrap();
    updates.staged(1, 0, true);
    assert_eq!(updates.decision(1), Decision::Waiting);
    updates.staged(1, 1, false);
    assert_eq!(updates.decision(1), Decision::Waiting);
    updates.activated(1, 0);
    updates.activated(1, 1);
    assert_epoch(0);
    let mut next = snapshot;
    next.revision = 2;
    next.epoch = 42;
    updates
        .publish(trust.prepare(envelope(next.clone())).unwrap())
        .unwrap();
    assert_epoch(0);
    updates.staged(2, 0, true);
    updates.staged(2, 1, true);
    assert_eq!(updates.decision(2), Decision::Activate);
    updates.activated(2, 0);
    updates.activated(2, 0); // Duplicate acknowledgments cannot finish activation.
    assert_epoch(0);
    next.revision = 3;
    next.epoch = 43;
    assert!(
        updates
            .publish(trust.prepare(envelope(next.clone())).unwrap())
            .is_err()
    );
    updates.activated(2, 1);
    assert_epoch(42);
    updates
        .publish(trust.prepare(envelope(next.clone())).unwrap())
        .unwrap();
    assert_eq!(updates.decision(3), Decision::Waiting);
    assert_epoch(42);
    updates.staged(3, 0, true);
    updates.staged(3, 1, false);
    updates.activated(3, 0);
    updates.activated(3, 1);
    assert_epoch(42);
    // Empty removal snapshots also carry an epoch and must fully activate.
    next.revision = 4;
    next.epoch = 44;
    next.peers.clear();
    next.volumes.clear();
    updates
        .publish(trust.prepare(envelope(next.clone())).unwrap())
        .unwrap();
    updates.staged(4, 0, true);
    updates.staged(4, 1, true);
    updates.activated(4, 0);
    assert_epoch(42);
    updates.activated(4, 1);
    assert_epoch(44);
    updates.activated(2, 1); // Stale acknowledgments cannot change the epoch.
    assert_epoch(44);
    // Loading an older-format config reports unknown, not the previous epoch.
    next.revision = 5;
    next.epoch = 0;
    updates
        .publish(trust.prepare(envelope(next)).unwrap())
        .unwrap();
    updates.staged(5, 0, true);
    updates.staged(5, 1, true);
    updates.activated(5, 0);
    assert_epoch(44);
    updates.activated(5, 1);
    assert_epoch(0);
}

pub(crate) fn ring() -> Option<uring::Ring> {
    match uring::Ring::http_test_ring(crate::buffers::io_test_pool(8), uring::Config::default()) {
        Ok(ring) => Some(ring),
        Err(e)
            if std::env::var_os("RACER_REQUIRE_URING").is_none()
                && matches!(
                    e.raw_os_error(),
                    Some(libc::EPERM | libc::ENOSYS | libc::ENOMEM)
                ) =>
        {
            eprintln!("io_uring unavailable: {e}");
            None
        }
        Err(e) => panic!("{e}"),
    }
}
fn drive(ring: &mut uring::Ring, client: &mut Client, mut ready: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        ring.progress().unwrap();
        let work = client.poll(ring, 16);
        if ready() {
            return;
        }
        assert!(Instant::now() < deadline, "control update timeout");
        if !work.runnable {
            ring.wait(Some(work.deadline.unwrap_or(deadline).min(deadline)))
                .unwrap();
        }
    }
}

#[test]
fn inotify_replacement_invalid_update_and_symlink_swap() {
    let Some(mut ring) = ring() else { return };
    let path = std::env::temp_dir().join(format!("racer-control-{}", std::process::id()));
    std::fs::create_dir_all(&path).unwrap();
    let (trust, mut snapshot) = fixture();
    let config = path.join("config.json");
    std::fs::write(
        &config,
        serde_json::to_vec(&envelope(snapshot.clone())).unwrap(),
    )
    .unwrap();
    let updates = Arc::new(Updates::default());
    let mut client = Client::new(
        Source::File(config.clone()),
        Arc::new(trust),
        updates.clone(),
    )
    .unwrap();
    drive(&mut ring, &mut client, || updates.latest(0).is_some());
    std::fs::write(&config, b"{").unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while client.failures == 0 {
        ring.progress().unwrap();
        client.poll(&mut ring, 16);
        assert!(Instant::now() < deadline);
        std::thread::yield_now();
    }
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    snapshot.revision = 2;
    snapshot.volumes.clear();
    snapshot.peers.clear();
    let replacement = path.join("new.json");
    std::fs::write(
        &replacement,
        serde_json::to_vec(&envelope(snapshot.clone())).unwrap(),
    )
    .unwrap();
    std::fs::rename(&replacement, &config).unwrap();
    drive(&mut ring, &mut client, || updates.latest(1).is_some());
    snapshot.revision = 3;
    std::fs::write(
        &replacement,
        serde_json::to_vec(&envelope(snapshot)).unwrap(),
    )
    .unwrap();
    let link = path.join("link");
    std::os::unix::fs::symlink(&replacement, &link).unwrap();
    std::fs::rename(&link, &config).unwrap();
    drive(&mut ring, &mut client, || updates.latest(2).is_some());
    client.shutdown(&mut ring).unwrap();
    std::fs::remove_dir_all(path).unwrap();
}

pub(crate) fn rdma_fixture() -> (Trust, proto::Snapshot) {
    let (trust, mut snapshot) = fixture();
    snapshot.fabric = "rack-1".into();
    snapshot.peers[0].id = "ab".repeat(32);
    snapshot.peers[0].fabric = snapshot.fabric.clone();
    snapshot.volumes[0].peers = vec![snapshot.peers[0].id.clone()];
    snapshot.volumes[0].topology.as_mut().unwrap().neighbors[0].peer = snapshot.peers[0].id.clone();
    (trust, snapshot)
}

#[test]
fn coordinated_receive_barrier_reorder_abort_and_retirement() {
    let (trust, mut snapshot) = fixture();
    let updates = Updates::default();
    for _ in 0..2 {
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    }
    updates
        .command(trust.prepare(envelope(snapshot.clone())).unwrap(), 1)
        .unwrap();
    updates.staged(1, 0, true);
    updates.staged(1, 1, true);
    assert_eq!(updates.acknowledged_phase(), 1);
    assert_eq!(updates.decision(1), Decision::Waiting);
    // An activation command still cannot bypass the local receive barrier.
    updates.command_phase(1, 3).unwrap();
    updates.received(1, 0);
    assert_eq!(updates.decision(1), Decision::Waiting);
    updates.received(1, 1);
    assert_eq!(updates.decision(1), Decision::Activate);
    assert!(updates.command_phase(1, 5).is_err());
    updates.command_phase(1, 1).unwrap();
    assert_eq!(updates.decision(1), Decision::Activate);
    updates.activated(1, 0);
    assert_eq!(updates.status()["ready"], false);
    updates.activated(1, 1);
    assert_eq!(updates.status()["ready"], true);
    snapshot.revision = 2;
    assert!(
        updates
            .command(trust.prepare(envelope(snapshot.clone())).unwrap(), 1)
            .is_err()
    );
    updates.command_phase(1, 4).unwrap();
    updates.retired(1, 0);
    assert_eq!(updates.acknowledged_phase(), 3);
    updates.retired(1, 1);
    assert_eq!(updates.acknowledged_phase(), 4);
    updates
        .command(trust.prepare(envelope(snapshot.clone())).unwrap(), 1)
        .unwrap();
    // Controller durably aborted revision 2, but its Abort response was lost.
    snapshot.revision = 3;
    updates
        .command(trust.prepare(envelope(snapshot)).unwrap(), 1)
        .unwrap();
    assert_eq!(updates.decision(2), Decision::Discard);
    updates.command_phase(3, 5).unwrap();
    assert_eq!(updates.decision(3), Decision::Discard);
    assert_eq!(updates.status()["activeRevision"], 1);
}

#[test]
fn retired_workers_report_phase_four_only_after_terminal_command() {
    let (trust, snapshot) = fixture();
    let updates = Updates::default();
    for _ in 0..2 {
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    }
    updates
        .command(trust.prepare(envelope(snapshot)).unwrap(), 3)
        .unwrap();
    for worker in 0..2 {
        updates.staged(1, worker, true);
    }
    assert!(updates.receive_decision(1));
    for worker in 0..2 {
        updates.received(1, worker);
    }
    for worker in 0..2 {
        updates.activated(1, worker);
        updates.retired(1, worker);
    }
    let rejection = Rejection::default();
    let headers = || rejection.headers(&updates, "accepted-digest", "boot");
    assert_eq!(updates.status()["retiredWorkers"], 2);
    assert_eq!(updates.status()["phase"], 3);
    assert!(headers().contains(&("X-Racer-Phase", "3".into())));
    // A delayed terminal command changes the wire acknowledgment immediately,
    // without requiring another worker poll or reopening an expired generation.
    updates.command_phase(1, 4).unwrap();
    assert!(headers().contains(&("X-Racer-Phase", "4".into())));
    updates.command_phase(1, 3).unwrap();
    assert!(headers().contains(&("X-Racer-Phase", "4".into())));
}

#[test]
fn full_geometry_bootstrap_and_exact_large_successor_set() {
    let (trust, mut snapshot) = fixture();
    snapshot.peers.clear();
    let v = &mut snapshot.volumes[0];
    v.peers.clear();
    v.topology = Some(proto::Topology {
        epoch: 1,
        slot_count: MAX_SLOTS,
        local_slots: (0..MAX_SLOTS).collect(),
        neighbors: vec![],
        routing_algorithm: Some(2),
    });
    let start = Instant::now();
    let prepared = trust.prepare(envelope(snapshot.clone())).unwrap();
    eprintln!(
        "262144 local slots: {:?}, {} protobuf bytes",
        start.elapsed(),
        snapshot.encoded_len()
    );
    assert_eq!(prepared.volumes[0].routing.geometry.slot_count(), MAX_SLOTS);
    let peer = "ab".repeat(32);
    snapshot.peers = vec![proto::Peer {
        pod_uid: "test-pod".into(),
        id: peer.clone(),
        http_address: "127.0.0.1:8081".into(),
        fabric: String::new(),
    }];
    let v = &mut snapshot.volumes[0];
    v.peers = vec![peer.clone()];
    v.topology = Some(proto::Topology {
        epoch: 1,
        slot_count: 512,
        local_slots: (0..65).collect(),
        neighbors: (65..512)
            .map(|slot| proto::SlotPeer {
                slot,
                peer: peer.clone(),
            })
            .collect(),
        routing_algorithm: Some(2),
    });
    trust.prepare(envelope(snapshot.clone())).unwrap();
    snapshot.volumes[0]
        .topology
        .as_mut()
        .unwrap()
        .neighbors
        .pop();
    assert!(trust.prepare(envelope(snapshot.clone())).is_err());
    snapshot.volumes[0]
        .topology
        .as_mut()
        .unwrap()
        .neighbors
        .push(proto::SlotPeer { slot: 0, peer });
    assert!(trust.prepare(envelope(snapshot)).is_err());
}

#[test]
fn control_receiver_exceeds_payload_buffer_and_rejects_ambiguous_framing() {
    for duplicate in [false, true] {
        let fixture = credentials::tests::Fixture::new();
        let provider = fixture.provider(0);
        let context = fixture.context("spiffe://racer/controlplane", Some("localhost"));
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (socket, _) = listener.accept().unwrap();
            let mut socket = credentials::tests::server(socket, &context);
            let mut byte = [0];
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                socket.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            if duplicate {
                socket
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n")
                    .unwrap();
            } else {
                let body = vec![7; 5 * 1024 * 1024];
                write!(
                    socket,
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\n\r\n",
                    body.len()
                )
                .unwrap();
                socket.write_all(&body).unwrap();
            }
        });
        let result = fetch_control(address, "localhost", "/", None, &[], &provider, &mut || {
            Ok(())
        });
        if duplicate {
            assert!(result.is_err());
        } else {
            assert_eq!(result.unwrap().0.unwrap().len(), 5 * 1024 * 1024);
        }
        server.join().unwrap();
    }
}

#[test]
fn rdma_eligibility_binds_direct_membership_endpoint_and_snapshot() {
    let (trust, mut snapshot) = rdma_fixture();
    scope_peers(&mut snapshot);
    let peer_id = snapshot.peers[0].id.clone();
    let prepared = trust.prepare(envelope(snapshot.clone())).unwrap();
    let peer = prepared.eligible_peer(&peer_id).unwrap();
    assert_eq!(peer.node().bytes(), [0xab; 32]);
    assert_eq!(peer.local_node().bytes(), trust.node);
    assert_eq!(peer.fabric().as_str(), "rack-1");
    assert_eq!(
        peer.endpoint().address().tcp().unwrap(),
        "127.0.0.1:8081".parse().unwrap()
    );
    assert_eq!(peer.endpoint().host(), "127.0.0.1:8081");
    // The prepared address and authority are sufficient to start HTTP without
    // reparsing the original URL or performing DNS in the negotiator.
    assert!(
        http::Connection::new_address(peer.endpoint().address(), peer.endpoint().host()).is_ok()
    );
    assert_eq!(peer.config_snapshot(), &snapshot);
    assert_eq!(peer.pod_uid(), "test-pod");
    assert_eq!(prepared.eligible_node(peer.node()).unwrap().id(), peer_id);
    assert_eq!(prepared.eligible_peers().count(), 1);
    assert!(prepared.eligible_peer(&"cd".repeat(32)).is_none());
    assert!(
        prepared
            .eligible_node(NodeId::from_bytes(&[0xcd; 32]).unwrap())
            .is_none()
    );
    assert!(
        prepared
            .eligible_peer_for_volume("missing", &peer_id)
            .is_none()
    );
}

#[test]
fn rdma_ineligible_configuration_preserves_http_routes() {
    let (trust, original) = rdma_fixture();
    for mutate in [
        |s: &mut proto::Snapshot| s.peers[0].id = "neighbor".into(),
        |s: &mut proto::Snapshot| s.peers[0].id = "g".repeat(64),
        |s: &mut proto::Snapshot| s.peers[0].id = "a".repeat(63),
        |s: &mut proto::Snapshot| s.peers[0].id = "02".repeat(32),
        |s: &mut proto::Snapshot| s.fabric.clear(),
        |s: &mut proto::Snapshot| s.peers[0].fabric.clear(),
        |s: &mut proto::Snapshot| s.peers[0].fabric = "rack-2".into(),
        |s: &mut proto::Snapshot| s.peers[0].fabric = "RACK-1".into(),
        |s: &mut proto::Snapshot| {
            s.fabric = "x".repeat(crate::peer_identity::MAX_FABRIC_LEN + 1);
            s.peers[0].fabric = s.fabric.clone();
        },
        |s: &mut proto::Snapshot| {
            s.fabric = "rack\r\ninjected: header".into();
            s.peers[0].fabric = s.fabric.clone();
        },
        |s: &mut proto::Snapshot| s.peers[0].http_address = "0.0.0.0:80".into(),
        |s: &mut proto::Snapshot| s.peers[0].http_address = "[::]:80".into(),
        |s: &mut proto::Snapshot| s.peers[0].http_address = "224.0.0.1:80".into(),
        |s: &mut proto::Snapshot| s.peers[0].http_address = "[ff02::1]:80".into(),
        |s: &mut proto::Snapshot| s.peers[0].http_address = "255.255.255.255:80".into(),
    ] {
        let mut snapshot = original.clone();
        mutate(&mut snapshot);
        snapshot.volumes[0].peers = vec![snapshot.peers[0].id.clone()];
        snapshot.volumes[0].topology.as_mut().unwrap().neighbors[0].peer =
            snapshot.peers[0].id.clone();
        let prepared = trust.prepare(envelope(snapshot.clone())).unwrap();
        assert_eq!(prepared.eligible_peers().count(), 0, "{snapshot:?}");
        assert_eq!(prepared.peers.len(), 1);
        assert_eq!(prepared.volumes[0].config.peers, snapshot.volumes[0].peers);
        assert!(
            prepared
                .select_eligible_peer("v1", "/object?version=1")
                .is_none()
        );
    }
    // Shared endpoint grammar rejects zero ports before HTTP/RDMA setup.
    for url in ["https://127.0.0.1", "http://127.0.0.1:0"] {
        let mut snapshot = original.clone();
        snapshot.peers[0].http_address = url.trim_start_matches("http://").into();
        assert!(trust.prepare(envelope(snapshot)).is_err());
    }
}

#[test]
fn rdma_node_hex_is_case_insensitive_but_ambiguous_duplicates_are_http_only() {
    let (trust, mut snapshot) = rdma_fixture();
    snapshot.peers[0].id.make_ascii_uppercase();
    snapshot.volumes[0].peers = vec![snapshot.peers[0].id.clone()];
    snapshot.volumes[0].topology.as_mut().unwrap().neighbors[0].peer = snapshot.peers[0].id.clone();
    let prepared = trust.prepare(envelope(snapshot.clone())).unwrap();
    assert_eq!(
        prepared.eligible_peers().next().unwrap().node().bytes(),
        [0xab; 32]
    );
    let mut duplicate = snapshot.peers[0].clone();
    duplicate.id.make_ascii_lowercase();
    duplicate.http_address = "127.0.0.1:8083".into();
    snapshot.peers.push(duplicate);
    let prepared = trust.prepare(envelope(snapshot)).unwrap();
    assert_eq!(prepared.peers.len(), 2);
    assert_eq!(prepared.eligible_peers().count(), 0);
}

#[test]
fn rdma_volume_selection_preserves_http_slots_and_order() {
    let (trust, mut snapshot) = rdma_fixture();
    let first = snapshot.peers[0].id.clone();
    let mut second = snapshot.peers[0].clone();
    second.id = "cd".repeat(32);
    snapshot.peers.push(second.clone());
    snapshot.peers.push(proto::Peer {
        pod_uid: "test-pod".into(),
        id: "alias".into(),
        http_address: "127.0.0.1:8084".into(),
        fabric: "rack-1".into(),
    });
    snapshot.volumes[0].peers = vec![second.id.clone(), "alias".into(), first.clone()];
    snapshot.volumes[0].topology = Some(proto::Topology {
        routing_algorithm: None,
        epoch: 1,
        slot_count: 27,
        local_slots: vec![1],
        neighbors: vec![
            proto::SlotPeer {
                slot: 3,
                peer: second.id.clone(),
            },
            proto::SlotPeer {
                slot: 4,
                peer: "alias".into(),
            },
            proto::SlotPeer {
                slot: 5,
                peer: first.clone(),
            },
        ],
    });
    let mut volume = snapshot.volumes[0].clone();
    volume.id = "v2".into();
    volume.cache_socket = "/dev/racer/second/cache".into();
    volume.origin_socket = "/dev/racer/second/origin".into();
    volume.peers = vec![first.clone()];
    volume.topology = Some(proto::Topology {
        routing_algorithm: None,
        epoch: 1,
        slot_count: 2,
        local_slots: vec![0],
        neighbors: vec![proto::SlotPeer {
            slot: 1,
            peer: first.clone(),
        }],
    });
    snapshot.volumes.push(volume);
    let prepared = trust.prepare(envelope(snapshot)).unwrap();
    let ordered: Vec<_> = prepared
        .eligible_peers_for_volume("v1")
        .map(|p| p.id())
        .collect();
    assert_eq!(ordered, [second.id.as_str(), first.as_str()]);
    assert_eq!(prepared.eligible_peers_for_volume("unknown").count(), 0);
    assert!(
        prepared
            .eligible_peer_for_volume("v2", &second.id)
            .is_none()
    );
    assert!(prepared.select_eligible_peer("unknown", "/").is_none());
    let mut slots = BTreeSet::new();
    for n in 0..100 {
        let target = format!("/object?version={n}");
        let routing = &prepared.volumes[0].routing;
        let next = routing.next(&routing.start(&target)).unwrap();
        let expected = next.map(|(id, _)| id);
        slots.insert(expected.clone());
        let selected = prepared.select_eligible_peer("v1", &target);
        assert_eq!(
            selected.map(|p| p.id().to_owned()),
            expected.filter(|id| id != "alias")
        );
    }
    assert!(slots.len() >= 3);
}

#[test]
fn rdma_capabilities_pin_original_policy_and_do_not_survive_removal_in_new_generation() {
    let (trust, snapshot) = rdma_fixture();
    let id = snapshot.peers[0].id.clone();
    let updates = Updates::default();
    updates
        .publish(trust.prepare(envelope(snapshot.clone())).unwrap())
        .unwrap();
    let old = updates.latest(0).unwrap();
    let peer = old.eligible_peer(&id).unwrap();
    let mut next = snapshot.clone();
    next.revision += 1;
    next.peers.clear();
    next.volumes[0].peers.clear();
    next.volumes[0].topology = Some(proto::Topology {
        routing_algorithm: None,
        epoch: 2,
        slot_count: 2,
        local_slots: vec![0, 1],
        neighbors: vec![],
    });
    updates
        .publish(trust.prepare(envelope(next)).unwrap())
        .unwrap();
    let current = updates.latest(1).unwrap();
    assert!(current.eligible_peer(&id).is_none());
    assert!(current.select_eligible_peer("v1", "/").is_none());
    assert_eq!(
        current.config_snapshot().universe,
        peer.config_snapshot().universe
    );
    assert_eq!(peer.config_snapshot().revision, 1);

    // Editing builder input cannot alter a previously validated authority, and
    // substituted identity must pass validation before it can mint capabilities.
    let staged = trust.prepare(envelope(snapshot)).unwrap();
    let mut builder = trust.builder(staged.config_snapshot().clone());
    builder.snapshot_mut().node = vec![9; 32];
    assert!(builder.build().is_err());
    let peer = staged.eligible_peer(&id).unwrap();
    assert_eq!(peer.local_node().bytes(), trust.node);
    assert_eq!(peer.config_snapshot().peers.len(), 1);
    assert_eq!(peer.endpoint().host(), "127.0.0.1:8081");
    assert!(std::ptr::eq(
        peer.config_snapshot(),
        staged.config_snapshot()
    ));
    assert!(std::ptr::eq(
        peer.crypto_snapshot(),
        staged.crypto_snapshot()
    ));
    let mut builder = trust.builder(staged.config_snapshot().clone());
    builder.snapshot_mut().peers[0].http_address = "127.0.0.1:9091".into();
    let changed = builder.build().unwrap();
    assert_eq!(
        changed.eligible_peer(&id).unwrap().endpoint().host(),
        "127.0.0.1:9091"
    );
    assert_eq!(peer.endpoint().host(), "127.0.0.1:8081");
}

pub(crate) mod activation_tests {
    use super::*;
    use std::sync::{Barrier, TryLockError, mpsc};

    #[test]
    fn transmit_acknowledgments_cannot_bypass_receive_or_abort() {
        let (trust, config) = fixture();
        let updates = Updates::default();
        for _ in 0..2 {
            updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        }
        updates
            .command(prepare_snapshot(&trust, config.clone()), 1)
            .unwrap();
        for worker in 0..2 {
            updates.staged(1, worker, true);
        }
        // Preparation alone never grants coordinated transmit authority.
        for worker in 0..2 {
            updates.activated(1, worker);
        }
        assert_eq!(updates.status()["activatedWorkers"], 0);
        assert!(updates.active().is_none());
        updates.command_phase(1, 3).unwrap();
        assert!(updates.receive_decision(1));
        updates.received(1, 0);
        for worker in 0..2 {
            updates.activated(1, worker);
        }
        assert_eq!(updates.decision(1), Decision::Waiting);
        assert_eq!(updates.status()["activatedWorkers"], 0);
        updates.received(1, 1);
        assert_eq!(updates.decision(1), Decision::Activate);
        updates.activated(1, 0);
        assert!(updates.active().is_none());
        updates.activated(1, 1);
        assert_eq!(updates.active().unwrap().config_snapshot().revision, 1);
        assert!(updates.command_phase(1, 5).is_err());

        let aborted = Updates::default();
        aborted.subscribe(Arc::new(uring::Wake::new().unwrap()));
        aborted
            .command(prepare_snapshot(&trust, config), 1)
            .unwrap();
        aborted.staged(1, 0, true);
        aborted.command_phase(1, 5).unwrap();
        aborted.received(1, 0);
        aborted.activated(1, 0);
        aborted.retired(1, 0);
        assert_eq!(aborted.decision(1), Decision::Discard);
        assert!(!aborted.receive_decision(1));
        assert!(aborted.active().is_none());
        assert_eq!(aborted.status()["retiredWorkers"], 0);
    }

    #[test]
    fn command_codes_are_validated_and_monotonic_with_stale_feedback_fenced() {
        let (trust, mut config) = fixture();
        let updates = Updates::default();
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        for code in [0, 6, u32::MAX] {
            assert!(
                updates
                    .command(prepare_snapshot(&trust, config.clone()), code)
                    .is_err()
            );
            assert!(updates.latest(0).is_none());
        }
        updates
            .command(prepare_snapshot(&trust, config.clone()), 1)
            .unwrap();
        updates.staged(1, 0, false);
        config.revision = 2;
        updates
            .command(prepare_snapshot(&trust, config), 4)
            .unwrap();
        let before = updates.status();
        updates.staged(1, 0, true);
        updates.received(1, 0);
        updates.activated(1, 0);
        updates.retired(1, 0);
        assert_eq!(updates.status(), before);
        assert!(!updates.receive_decision(1));
        assert!(updates.command_phase(1, 2).is_err());
        for code in [1, 2, 3, 4, 2] {
            updates.command_phase(2, code).unwrap();
            assert_eq!(updates.status()["phase"], 4);
        }
        updates.staged(2, 0, true);
        assert!(updates.receive_decision(2));
        assert_eq!(updates.acknowledged_phase(), 1);
        updates.received(2, 0);
        assert_eq!(updates.acknowledged_phase(), 2);
        updates.activated(2, 0);
        assert_eq!(updates.acknowledged_phase(), 3);
        updates.retired(2, 0);
        assert_eq!(updates.acknowledged_phase(), 4);
    }

    #[test]
    fn b05_exporter_readiness_tracks_required_identity_and_listener() {
        use std::io::{Read, Write};
        use std::net::TcpStream;
        use std::time::Duration;

        for case in [
            "add",
            "initial",
            "same",
            "address",
            "identity",
            "empty",
            "idle",
            "initial-idle",
        ] {
            let (trust, mut config) = fixture();
            config.epoch = 91;
            let updates = Arc::new(Updates::default());
            for _ in 0..2 {
                updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
            }
            let exporter = crate::metrics::Exporter::start(
                "127.0.0.1:0".parse().unwrap(),
                Arc::new(crate::metrics::Registry::new(2, updates.clone())),
            )
            .unwrap();
            let check = |label: &str, ready: bool, active: u64, volumes: serde_json::Value| {
                // Workers are stationary at each boundary: both real HTTP endpoints
                // must return the exact same production status, with different codes.
                let expected = updates.status();
                for path in ["/readyz", "/status"] {
                    let timeout = Duration::from_secs(3);
                    let mut socket =
                        TcpStream::connect_timeout(&exporter.address(), timeout).unwrap();
                    socket.set_read_timeout(Some(timeout)).unwrap();
                    socket.set_write_timeout(Some(timeout)).unwrap();
                    write!(socket, "GET {path} HTTP/1.1\r\nHost: localhost\r\n\r\n").unwrap();
                    let mut response = String::new();
                    socket.read_to_string(&mut response).unwrap();
                    let (headers, body) = response.split_once("\r\n\r\n").unwrap();
                    let body: serde_json::Value = serde_json::from_str(body).unwrap();
                    assert_eq!(body, expected);
                    let code = if path == "/status" || ready { 200 } else { 503 };
                    assert!(
                        headers.starts_with(&format!("HTTP/1.1 {code} ")),
                        "B05 {case}/{label} {path}: {headers}; {body}"
                    );
                }
                assert_eq!(expected["ready"], ready, "{case}/{label}");
                assert_eq!(expected["activeRevision"], active, "{case}/{label}");
                assert_eq!(expected["volumes"], volumes, "{case}/{label}");
            };
            let old_volumes = serde_json::json!([{"id":"v1", "epoch":1, "ready":true}]);
            check("startup", false, 0, serde_json::json!([]));
            if !matches!(case, "initial" | "initial-idle") {
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                for worker in 0..2 {
                    updates.staged(1, worker, true);
                }
                for worker in 0..2 {
                    updates.activated(1, worker);
                }
                check("active A", true, 1, old_volumes.clone());
                config.revision = 2;
            }
            match case {
                "add" => {
                    let mut b = config.volumes[0].clone();
                    b.id = "B".into();
                    b.cache_socket = "/dev/racer/second/cache".into();
                    b.origin_socket = "/dev/racer/second/origin".into();
                    config.volumes.push(b);
                }
                "same" => {
                    config.volumes[0].origin_socket = "/dev/racer/changed/origin".into();
                    config.volumes[0].topology.as_mut().unwrap().epoch = 2;
                }
                "address" => config.volumes[0].cache_socket = "/dev/racer/moved/cache".into(),
                "identity" => config.volumes[0].id = "B".into(),
                "empty" => config.volumes.clear(),
                "idle" | "initial-idle" => {
                    config.volumes.clear();
                    config.peers.clear();
                    config.idle = true;
                }
                _ => {}
            }
            config.epoch = 92;
            let revision = config.revision;
            updates
                .publish(prepare_snapshot(&trust, config.clone()))
                .unwrap();
            let prior = if matches!(case, "initial" | "initial-idle") {
                0
            } else {
                1
            };
            let listed = if prior == 0 {
                serde_json::json!([])
            } else {
                old_volumes
            };
            let ready = matches!(case, "same" | "empty" | "idle");
            check("pending", ready, prior, listed.clone());
            updates.staged(revision, 0, true);
            updates.staged(revision, 1, false);
            assert_eq!(updates.status()["rejected"], true);
            check("rejected", ready, prior, listed.clone());
            // A successful retry clears only the failed worker, without republication.
            updates.staged(revision, 1, true);
            assert_eq!(updates.status()["rejected"], false);
            updates.activated(revision, 0);
            assert_eq!(updates.status()["activatedWorkers"], 1);
            check("partial activation", ready, prior, listed);
            assert_eq!(updates.applied_epoch(), if prior == 0 { 0 } else { 91 });
            updates.activated(revision, 1);
            let listed = config
                .volumes
                .iter()
                .map(|v| {
                    serde_json::json!({
                        "id":v.id, "epoch":v.topology.as_ref().unwrap().epoch, "ready":true
                    })
                })
                .collect::<Vec<_>>();
            check(
                "all workers active",
                case != "empty",
                revision,
                serde_json::json!(listed),
            );
            assert_eq!(updates.applied_epoch(), 92);
            eprintln!("B05 exporter case={case}: pending/rejected/partial/all-active checked");
        }
    }

    #[test]
    fn idle_configuration_is_explicit_and_cannot_authorize_traffic() {
        let (trust, mut config) = fixture();
        config.idle = true;
        assert!(trust.prepare(envelope(config.clone())).is_err());
        config.volumes.clear();
        assert!(trust.prepare(envelope(config.clone())).is_err());
        config.peers.clear();
        assert!(trust.prepare(envelope(config.clone())).is_ok());
        // HTTPS authenticates the control plane; snapshots still bind node identity.
        assert!(trust.prepare_http(envelope(config)).is_ok());
    }

    #[test]
    fn storage_failure_keeps_activated_cache_ready_and_topology_error_separate() {
        let (trust, config) = fixture();
        let updates = Updates::default();
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        let revision = config.revision;
        updates.publish(prepare_snapshot(&trust, config)).unwrap();
        updates.staged(revision, 0, true);
        updates.activated(revision, 0);
        assert_eq!(updates.status()["ready"], true);
        updates.observe_storage(1 << 30, 1);
        updates.test_storage_policy(1, 2 << 30);
        let request = updates.desired_storage().unwrap();
        assert!(updates.report_storage(
            &request,
            StorageResult::Failed("disk full".into()),
            1 << 30
        ));
        let status = updates.status();
        assert_eq!(status["ready"], true);
        assert_eq!(status["volumes"][0]["ready"], true);
        assert_eq!(status["lastError"], serde_json::Value::Null);
        assert_eq!(status["storage"]["error"], "disk full");
        assert_eq!(status["activeRevision"], revision);
    }

    #[test]
    fn idle_readiness_waits_for_added_listener_and_survives_idle_update() {
        let (trust, volume) = fixture();
        let mut idle = volume.clone();
        idle.volumes.clear();
        idle.peers.clear();
        idle.idle = true;
        let updates = Updates::default();
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        for revision in 1..=4 {
            let mut config = if revision == 3 {
                volume.clone()
            } else {
                idle.clone()
            };
            config.revision = revision;
            if revision == 4 {
                config.idle = false;
            }
            updates.publish(prepare_snapshot(&trust, config)).unwrap();
            // Initial activation and a newly required listener are not ready;
            // idle-to-idle updates keep the last-good readiness until activation.
            assert_eq!(updates.status()["ready"], matches!(revision, 2 | 4));
            updates.staged(revision, 0, true);
            updates.activated(revision, 0);
            assert_eq!(updates.status()["ready"], revision != 4);
        }
    }

    #[test]
    fn b04_each_failure_must_clear_before_coordinated_barriers() {
        for phase in 1..=4 {
            let (trust, mut config) = fixture();
            let updates = Updates::default();
            for _ in 0..3 {
                updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
            }
            updates
                .command(prepare_snapshot(&trust, config.clone()), phase)
                .unwrap();
            updates.staged(1, 0, true);
            updates.staged(1, 1, false);
            updates.staged(1, 2, false);
            updates.staged(1, 1, true);
            assert_eq!(updates.status()["rejected"], true);
            assert_eq!(updates.status()["preparedWorkers"], 2);
            assert_eq!(updates.acknowledged_phase(), 0);
            assert_eq!(updates.decision(1), Decision::Waiting);
            assert!(!updates.receive_decision(1));
            if phase >= 2 {
                // A committed command may arrive before staging succeeds. Failure
                // cannot bypass receive commitment to abort/supersede that revision.
                assert!(updates.command_phase(1, 5).is_err());
                config.revision = 2;
                assert_eq!(
                    updates
                        .command(prepare_snapshot(&trust, config.clone()), 1)
                        .unwrap_err()
                        .kind(),
                    io::ErrorKind::WouldBlock
                );
            }
            updates.staged(1, 2, true);
            assert_eq!(updates.status()["rejected"], false);
            assert_eq!(updates.acknowledged_phase(), 1);
            assert_eq!(updates.decision(1), Decision::Waiting);
            updates.command_phase(1, 2).unwrap();
            assert!(updates.receive_decision(1));
            updates.received(1, 0);
            updates.received(1, 1);
            assert_eq!(updates.decision(1), Decision::Waiting);
            updates.received(1, 2);
            assert_eq!(updates.acknowledged_phase(), 2);
            updates.command_phase(1, 3).unwrap();
            assert_eq!(updates.decision(1), Decision::Activate);
            for worker in 0..3 {
                updates.activated(1, worker);
            }
            assert_eq!(updates.status()["activeRevision"], 1);
            updates.command_phase(1, 4).unwrap();
            for worker in 0..3 {
                updates.retired(1, worker);
            }
            assert_eq!(updates.acknowledged_phase(), 4);
        }
    }

    // Per-Updates, one-shot barriers coordinate the participating threads.
    pub(crate) struct ActivationPause {
        pub(crate) entered: Barrier,
        pub(crate) resume: Barrier,
    }
    impl ActivationPause {
        pub(crate) fn new() -> Self {
            Self {
                entered: Barrier::new(2),
                resume: Barrier::new(2),
            }
        }
        pub(in crate::control) fn wait(&self) {
            self.entered.wait();
            self.resume.wait();
        }
    }

    #[test]
    fn b07_active_snapshot_acknowledgment_schedules() {
        for coordinated in [false, true] {
            for order in [[0, 1], [1, 0]] {
                let (trust, mut config) = fixture();
                config.epoch = 51;
                let updates = Updates::default();
                for _ in 0..2 {
                    updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
                }
                let publish = |config| {
                    let prepared = prepare_snapshot(&trust, config);
                    if coordinated {
                        updates.command(prepared, 1)
                    } else {
                        updates.publish(prepared)
                    }
                };
                publish(config.clone()).unwrap();
                let first = updates.latest(0).unwrap();
                for revision in [1, 2] {
                    assert_eq!(updates.status()["activeRevision"], revision - 1);
                    updates.staged(revision, order[0], true);
                    updates.staged(revision, order[0], true);
                    updates.activated(revision, order[0]); // Incomplete preparation.
                    assert_eq!(updates.status()["activatedWorkers"], 0);
                    updates.staged(revision, order[1], true);
                    if coordinated {
                        assert_eq!(updates.decision(revision), Decision::Waiting);
                        updates.command_phase(revision, 3).unwrap();
                        updates.received(revision, order[0]);
                        updates.received(revision, order[0]);
                        assert_eq!(updates.decision(revision), Decision::Waiting);
                        updates.received(revision, order[1]);
                    }
                    assert_eq!(updates.decision(revision), Decision::Activate);
                    updates.activated(revision, order[0]);
                    updates.activated(revision, order[0]);
                    updates.activated(revision - 1, order[1]);
                    let partial = updates.status();
                    assert_eq!(partial["activatedWorkers"], 1);
                    assert_eq!(partial["activeRevision"], revision - 1);
                    assert_eq!(updates.applied_epoch(), if revision == 1 { 0 } else { 51 });
                    config.revision = revision + 1;
                    config.epoch = 52;
                    config.volumes.clear();
                    config.peers.clear();
                    assert_eq!(
                        publish(config.clone()).unwrap_err().kind(),
                        io::ErrorKind::WouldBlock
                    );
                    let pinned = updates.latest(0).unwrap();
                    updates.activated(revision, order[1]);
                    updates.activated(revision, order[1]);
                    let complete = updates.status();
                    assert_eq!(complete["activeRevision"], revision);
                    assert_eq!(complete["ready"], revision == 1);
                    assert_eq!(
                        complete["volumes"].as_array().unwrap().len(),
                        usize::from(revision == 1)
                    );
                    assert_eq!(updates.applied_epoch(), if revision == 1 { 51 } else { 52 });
                    assert!(Arc::ptr_eq(updates.active().as_ref().unwrap(), &pinned));
                    if coordinated {
                        assert_eq!(
                            publish(config.clone()).unwrap_err().kind(),
                            io::ErrorKind::WouldBlock
                        );
                        updates.command_phase(revision, 4).unwrap();
                        updates.retired(revision, order[0]);
                        assert_eq!(updates.acknowledged_phase(), 3);
                        updates.retired(revision, order[1]);
                        assert_eq!(updates.acknowledged_phase(), 4);
                    }
                    if revision == 1 {
                        publish(config.clone()).unwrap();
                    }
                }
                assert_eq!(first.config.revision, 1);
                assert_eq!(
                    first.config.volumes.len(),
                    1,
                    "old in-flight snapshot remains pinned"
                );
                eprintln!("B07 acknowledgment schedule coordinated={coordinated} order={order:?}");
            }
        }
    }

    #[test]
    fn b07_active_snapshot_real_thread_barriers() {
        active_snapshot_contender(false);
    }

    #[test]
    fn b07_active_snapshot_real_thread_stale_overwrite() {
        active_snapshot_contender(true);
    }

    fn active_snapshot_contender(activate_successor: bool) {
        // Pause BEFORE active publication in both production and mutated code. A real
        // contender observes exclusion without waiting for a timeout: under the fix it
        // cannot acquire the coordinator. With an unlocked-late-swap mutation,
        // it publishes R+1 (and optionally activates it) before letting R resume.
        let (trust, mut config) = fixture();
        config.epoch = 41;
        let updates = Arc::new(Updates::default());
        for _ in 0..2 {
            updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        }
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        let first = updates.latest(0).unwrap();
        updates.staged(1, 0, true);
        updates.staged(1, 1, true);
        updates.activated(1, 0);
        updates.activated(1, 0);
        assert_eq!(updates.status()["activeRevision"], 0);
        assert_eq!(updates.applied_epoch(), 0);

        let pause = Arc::new(ActivationPause::new());
        *updates.before_active_publication.lock().unwrap() = Some(pause.clone());
        let worker_updates = updates.clone();
        let worker = std::thread::spawn(move || worker_updates.activated(1, 1));
        pause.entered.wait();

        config.revision = 2;
        config.epoch = 42;
        config.volumes[0].cache_socket = "/dev/racer/moved/cache".into();
        config.volumes[0].topology.as_mut().unwrap().epoch = 2;
        let next = prepare_snapshot(&trust, config);
        let publisher_updates = updates.clone();
        let (observed_tx, observed_rx) = mpsc::channel();
        let publisher = std::thread::spawn(move || {
            fn blocked<T>(result: std::sync::TryLockResult<T>) -> bool {
                match result {
                    Ok(_) => false,
                    Err(TryLockError::WouldBlock) => true,
                    Err(TryLockError::Poisoned(_)) => panic!("poisoned boundary lock"),
                }
            }
            // Drop each successful guard immediately; observation holds no locks
            // across the channel or barrier, and never reverses production order.
            let coordinator_blocked = blocked(publisher_updates.coordinator.try_lock());
            observed_tx.send(coordinator_blocked).unwrap();
            publisher_updates.publish(next).unwrap();
            if activate_successor {
                publisher_updates.staged(2, 0, true);
                publisher_updates.staged(2, 1, true);
                publisher_updates.activated(2, 0);
                publisher_updates.activated(2, 1);
            }
            publisher_updates.status()
        });
        let excluded = observed_rx.recv().unwrap();
        if excluded {
            // publish cannot finish while R holds these locks. Release R before
            // joining the contender; no sleep or blocking-join deadlock is needed.
            pause.resume.wait();
        }
        let published = publisher.join();
        if !excluded {
            // Mutated path: force the successor ahead of the old active swap.
            pause.resume.wait();
        }
        worker.join().unwrap();
        let during = published.unwrap();

        let expected = if activate_successor { 2 } else { 1 };
        let after = updates.status();
        eprintln!(
            "B07 successor_activated={activate_successor} coordinator_blocked={excluded}: during={during}; after={after}"
        );
        assert_eq!(
            after["activeRevision"], expected,
            "active must be the last fully acknowledged revision; no premature successor or stale overwrite"
        );
        assert!(
            excluded,
            "final acknowledgment must exclude publication until the active swap"
        );
        assert_eq!(
            during["activeRevision"], expected,
            "completed R must already be visible before publishing R+1"
        );
        assert_eq!(after["candidateRevision"], 2);
        assert_eq!(
            after["activatedWorkers"],
            if activate_successor { 2 } else { 0 }
        );
        assert_eq!(after["ready"], activate_successor);
        assert_eq!(
            updates.applied_epoch(),
            if activate_successor { 42 } else { 41 }
        );
        let active = updates.active().unwrap();
        let pinned = if activate_successor {
            updates.latest(1).unwrap()
        } else {
            first
        };
        assert!(
            Arc::ptr_eq(&active, &pinned),
            "activation must pin the exact Prepared Arc"
        );

        // Late/stale acknowledgments cannot change active authority or its epoch.
        updates.activated(1, 0);
        updates.activated(1, 1);
        assert_eq!(updates.status(), after);
        if !activate_successor {
            updates.staged(2, 0, true);
            updates.staged(2, 1, false);
            updates.activated(2, 0);
            updates.activated(2, 1);
            let rejected = updates.status();
            assert_eq!(rejected["activeRevision"], 1);
            assert_eq!(rejected["candidateRevision"], 2);
            assert_eq!(rejected["rejected"], true);
            assert_eq!(rejected["ready"], false); // Candidate requires a different listener.
            assert_eq!(rejected["volumes"][0]["epoch"], 1);
            assert_eq!(updates.applied_epoch(), 41);
        }
    }
}

mod subscriber_tests {
    //! Production Subscriber over real TCP. Server actions, not sleeps, gate progress.
    use super::*;
    use std::io::Write;
    use std::net::{TcpListener, TcpStream};
    use std::sync::mpsc;
    thread_local! { static BOOT: std::cell::RefCell<Vec<u8>> = const { std::cell::RefCell::new(Vec::new()) }; }

    struct Server {
        address: SocketAddr,
        requests: mpsc::Receiver<(credentials::Stream, String, Instant)>,
        stop: Arc<std::sync::atomic::AtomicBool>,
        thread: Option<std::thread::JoinHandle<()>>,
        provider: Arc<credentials::Provider>,
    }
    impl Server {
        fn new() -> Self {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let fixture = credentials::tests::Fixture::new();
            let provider = fixture.provider(0);
            let context = fixture.context("spiffe://racer/controlplane", Some("localhost"));
            let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
            let stopping = stop.clone();
            let (tx, requests) = mpsc::channel();
            let thread = std::thread::spawn(move || {
                while let Ok((socket, _)) = listener.accept() {
                    if stopping.load(Ordering::Relaxed) {
                        break;
                    }
                    let mut socket = credentials::tests::server(socket, &context);
                    socket
                        .set_read_timeout(Some(Duration::from_secs(3)))
                        .unwrap();
                    let mut request = Vec::new();
                    while !request.ends_with(b"\r\n\r\n") {
                        let mut byte = [0];
                        socket.read_exact(&mut byte).unwrap();
                        request.push(byte[0]);
                        assert!(request.len() < 32768);
                    }
                    if tx
                        .send((socket, String::from_utf8(request).unwrap(), Instant::now()))
                        .is_err()
                    {
                        break;
                    }
                }
            });
            Self {
                address,
                requests,
                stop,
                thread: Some(thread),
                provider,
            }
        }
        fn next(&self) -> (credentials::Stream, String, Instant) {
            let result = self
                .requests
                .recv_timeout(Duration::from_secs(5))
                .expect("Subscriber request");
            let boot = result
                .1
                .lines()
                .find_map(|line| line.strip_prefix("X-Racer-Boot: "))
                .unwrap();
            BOOT.with_borrow_mut(|value| {
                *value = (0..boot.len())
                    .step_by(2)
                    .map(|i| u8::from_str_radix(&boot[i..i + 2], 16).unwrap())
                    .collect()
            });
            result
        }
        fn start(&self) -> (Subscriber, Arc<Updates>, Trust, proto::Snapshot) {
            let (trust, config) = fixture();
            let updates = Arc::new(Updates::default());
            updates.set_credentials(self.provider.clone());
            let subscriber = Subscriber::start(
                Source::parse(&format!("https://{}/configuration", self.address)).unwrap(),
                Arc::new(Trust {
                    universe: trust.universe,
                    node: trust.node,
                }),
                updates.clone(),
            )
            .unwrap();
            (subscriber, updates, trust, config)
        }
    }
    impl Drop for Server {
        fn drop(&mut self) {
            self.stop.store(true, Ordering::Relaxed);
            let _ = TcpStream::connect(self.address);
            self.thread.take().unwrap().join().unwrap();
        }
    }
    fn signed(trust: &Trust, snapshot: proto::Snapshot) -> Vec<u8> {
        use sha2::Digest;
        proto::ControlCommand {
            universe: snapshot.universe.clone(),
            node: snapshot.node.clone(),
            revision: snapshot.revision,
            incarnation: BOOT.with_borrow(Clone::clone),
            profile: 1,
            phase: 1,
            pod_uid: "test-pod".into(),
            snapshot_digest: sha2::Sha256::digest(snapshot.encode_to_vec()).to_vec(),
            configuration: Some(
                proto::Configuration::decode(signed_config(trust, snapshot).as_slice()).unwrap(),
            ),
            ..Default::default()
        }
        .encode_to_vec()
    }
    include!("storage_subscription.rs");
    fn signed_config(_trust: &Trust, snapshot: proto::Snapshot) -> Vec<u8> {
        envelope(snapshot).encode_to_vec()
    }
    fn reply(socket: &mut credentials::Stream, body: &[u8], etag: &str) {
        write!(
            socket,
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: {etag}\r\n\r\n",
            body.len()
        )
        .unwrap();
        socket.write_all(body).unwrap();
    }
    fn held(socket: &mut credentials::Stream, duration: Duration) {
        socket.set_read_timeout(Some(duration)).unwrap();
        let error = socket
            .read(&mut [0])
            .expect_err("Subscriber closed a held long poll");
        assert!(matches!(
            error.kind(),
            io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut
        ));
    }

    #[test]
    fn b16_production_subscriber_prefer_etag_wait_publication() {
        let server = Server::new();
        let (subscriber, updates, trust, mut config) = server.start();
        let (mut first, request, _) = server.next();
        assert!(
            request.contains("Prefer: wait=0\r\n"),
            "production Subscriber missing Prefer: {request}"
        );
        assert!(!request.contains("If-None-Match:"));
        reply(&mut first, &signed(&trust, config.clone()), "\"one\"");
        let (mut waiting, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"one\"\r\n"));
        // A conforming server holds the unchanged ETag, beyond the old 2s timeout.
        held(&mut waiting, Duration::from_millis(300));
        assert!(
            server.requests.try_recv().is_err(),
            "shortpoll while unchanged"
        );
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        config.revision = 2;
        config.volumes.clear();
        config.peers.clear();
        let published = Instant::now();
        reply(&mut waiting, &signed(&trust, config), "\"two\"");
        let (mut next, request, _) = server.next();
        assert!(
            published.elapsed() < Duration::from_secs(1),
            "publication did not wake poll"
        );
        assert!(request.contains("If-None-Match: \"two\"\r\n"));
        assert_eq!(updates.latest(0).unwrap().config.revision, 2);
        next.write_all(b"HTTP/1.1 304 Not Modified\r\nContent-Length: 0\r\n\r\n")
            .unwrap();
        let (_last, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"two\"\r\n"));
        drop(subscriber);
    }

    #[test]
    fn b16_production_subscriber_shutdown_held_poll() {
        let server = Server::new();
        let (subscriber, _, trust, config) = server.start();
        let (mut socket, _, _) = server.next();
        reply(&mut socket, &signed(&trust, config), "\"one\"");
        let (mut held, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"one\""));
        assert!(request.contains("Prefer: wait=0"));
        let start = Instant::now();
        drop(subscriber);
        assert!(
            start.elapsed() < Duration::from_millis(600),
            "shutdown blocked on socket: {:?}",
            start.elapsed()
        );
        assert!(matches!(held.read(&mut [0]), Ok(0) | Err(_)));
    }

    #[test]
    fn b16_production_subscriber_immediate_responses_are_spaced() {
        let server = Server::new();
        let (subscriber, _, trust, config) = server.start();
        let (mut socket, _, mut previous) = server.next();
        reply(&mut socket, &signed(&trust, config), "\"one\"");
        for _ in 0..5 {
            let (mut socket, request, now) = server.next();
            assert!(now.duration_since(previous) >= Duration::from_millis(100));
            assert!(request.contains("If-None-Match: \"one\""));
            socket
                .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
                .unwrap();
            previous = now;
        }
        drop(subscriber);
    }

    #[test]
    fn b16_production_subscriber_full_server_wait() {
        let server = Server::new();
        let (subscriber, updates, trust, config) = server.start();
        let (mut socket, _, _) = server.next();
        reply(&mut socket, &signed(&trust, config), "\"one\"");
        let (mut socket, request, _) = server.next();
        assert!(request.contains("Prefer: wait=0\r\n"));
        // A briefly held unchanged response preserves the current ETag and
        // revision without opening another subscription request.
        held(&mut socket, Duration::from_millis(300));
        assert!(server.requests.try_recv().is_err());
        socket
            .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
            .unwrap();
        let (_socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"one\""));
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        drop(subscriber);
    }

    #[test]
    fn b16_production_subscriber_fragment_idle_timeout_and_recovery() {
        let server = Server::new();
        let (subscriber, updates, trust, config) = server.start();
        let (mut socket, _, _) = server.next();
        let bytes = signed(&trust, config);
        socket.write_all(b"HTTP/1.1 200").unwrap();
        held(&mut socket, Duration::from_millis(300));
        write!(
            socket,
            " OK\r\nContent-Length: {}\r\nETag: \"one\"\r\n\r\n",
            bytes.len()
        )
        .unwrap();
        socket.write_all(&bytes[..3]).unwrap();
        held(&mut socket, Duration::from_millis(300));
        socket.write_all(&bytes[3..]).unwrap();
        let (mut socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"one\""));
        write!(
            socket,
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: \"bad\"\r\n\r\n",
            bytes.len()
        )
        .unwrap();
        socket.write_all(&bytes[..3]).unwrap();
        let start = Instant::now();
        let (mut retry, request, _) = server.next();
        assert!(start.elapsed() >= Duration::from_secs(2));
        assert!(start.elapsed() < Duration::from_secs(4));
        assert!(
            request.contains("If-None-Match: \"one\""),
            "partial response changed ETag"
        );
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        assert!(
            updates.status()["lastError"]
                .as_str()
                .unwrap()
                .contains("deadline")
        );
        retry
            .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
            .unwrap();
        let (_socket, _, _) = server.next();
        assert!(updates.status()["lastError"].is_null());
        drop(subscriber);
    }

    #[test]
    fn b16_production_subscriber_fragment_absolute_deadline() {
        let server = Server::new();
        let (subscriber, updates, _, _) = server.start();
        let (mut socket, _, _) = server.next();
        socket.write_all(b"HTTP/1.1 200 OK\r\nX-Slow: ").unwrap();
        let start = Instant::now();
        // Timed channel waits drive an actual trickle (<2s idle). The oracle is
        // connection closure plus a fresh request, not elapsed sleep alone.
        let (stop, stopped) = mpsc::channel::<()>();
        let drip = std::thread::spawn(move || {
            let end = Instant::now() + Duration::from_secs(12);
            while Instant::now() < end {
                if stopped.recv_timeout(Duration::from_millis(400)).is_ok() {
                    break;
                }
                if socket.write_all(b"x").is_err() {
                    break;
                }
            }
        });
        let (_socket, _, _) = server
            .requests
            .recv_timeout(Duration::from_secs(12))
            .expect("absolute transfer deadline must defeat trickle");
        assert!(start.elapsed() >= Duration::from_secs(5));
        assert!(start.elapsed() < Duration::from_secs(8));
        assert!(updates.latest(0).is_none());
        assert!(
            updates.status()["lastError"]
                .as_str()
                .unwrap()
                .contains("deadline")
        );
        let _ = stop.send(());
        drip.join().unwrap();
        drop(subscriber);
    }

    #[test]
    fn b16_production_subscriber_trust_rotation_while_held() {
        let server = Server::new();
        let (subscriber, updates, trust, config) = server.start();
        let (mut socket, _, _) = server.next();
        reply(&mut socket, &signed(&trust, config.clone()), "\"one\"");
        let (mut socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"one\""));
        let provider = updates.credentials().unwrap();
        let original = updates.status()["tls"]["trustDigest"].clone();
        // Independent publication changes the worker barrier even while a poll is held.
        credentials::tests::Fixture::advance(&provider);
        held(&mut socket, Duration::from_millis(300));
        assert_ne!(updates.status()["tls"]["trustDigest"], original);
        assert_eq!(
            provider.headers()[3].1,
            "1",
            "held old-context poll is counted"
        );
        reply(&mut socket, &signed(&trust, config), "\"new\"");
        let (_socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"new\""));
        assert!(request.contains("X-Racer-Trust-Generation: 2"));
        assert!(request.contains("X-Racer-Old-Connections: 0"));
        assert!(updates.status()["lastError"].is_null());
        drop(subscriber);
    }

    #[test]
    fn b16_retry_jitter_bounds_and_heartbeat_budget() {
        let mut random = 123;
        let mut draws = BTreeSet::new();
        for failures in 1..=6 {
            let cap = (250u64 << failures).min(4000);
            for _ in 0..100 {
                let delay = retry_delay(failures, &mut random);
                assert!(delay >= Duration::from_millis(cap * 4 / 5));
                assert!(delay <= Duration::from_millis(cap));
                assert!(delay + Duration::from_secs(5) < Duration::from_secs(15));
                draws.insert(delay);
            }
        }
        assert!(draws.len() > 100, "retry jitter degenerated");
    }

    struct ConfigFile(PathBuf);
    impl ConfigFile {
        fn new(name: &str) -> Self {
            let path =
                std::env::temp_dir().join(format!("racer-a21-{name}-{}", std::process::id()));
            std::fs::create_dir(&path).unwrap();
            Self(path)
        }
        fn path(&self) -> PathBuf {
            self.0.join("config")
        }
        fn write(&self, bytes: &[u8]) {
            std::fs::write(self.0.join("next"), bytes).unwrap();
            std::fs::rename(self.0.join("next"), self.path()).unwrap();
        }
    }
    impl Drop for ConfigFile {
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }
    fn wait_for(description: &str, mut ready: impl FnMut() -> bool) {
        let end = Instant::now() + Duration::from_secs(6);
        while !ready() {
            assert!(Instant::now() < end, "timed out: {description}");
            std::thread::sleep(Duration::from_millis(10));
        }
    }
    fn revision(updates: &Updates, revision: u64) {
        wait_for("published revision", || {
            updates
                .latest(0)
                .is_some_and(|p| p.config.revision == revision)
                && updates.status()["lastError"].is_null()
        });
    }
    fn quiet(updates: &Updates) {
        let p = &updates.subscription_probe;
        let reads = p.reads.load(Ordering::SeqCst);
        let prepares = p.prepares.load(Ordering::SeqCst);
        let polls = p.polls.load(Ordering::SeqCst);
        wait_for("four unchanged production polls", || {
            p.polls.load(Ordering::SeqCst) >= polls + 4
        });
        assert_eq!(
            p.reads.load(Ordering::SeqCst),
            reads,
            "unchanged file reread"
        );
        assert_eq!(
            p.prepares.load(Ordering::SeqCst),
            prepares,
            "unchanged input reprepared"
        );
    }
    fn json(config: &proto::Snapshot) -> Vec<u8> {
        serde_json::to_vec(&proto::Configuration {
            contents: Some(proto::configuration::Contents::Snapshot(config.clone())),
        })
        .unwrap()
    }

    #[test]
    fn a21_production_file_changes_and_accepted_gate() {
        let file = ConfigFile::new("changes");
        let (trust, mut config) = fixture();
        file.write(&json(&config));
        let updates = Arc::new(Updates::default());
        let subscriber =
            Subscriber::start(Source::File(file.path()), Arc::new(trust), updates.clone()).unwrap();
        revision(&updates, 1);
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            1
        );
        quiet(&updates);

        // A new inode containing identical bytes is read once but never prepared.
        let reads = updates.subscription_probe.reads.load(Ordering::SeqCst);
        file.write(&json(&config));
        wait_for("identical atomic replacement read", || {
            updates.subscription_probe.reads.load(Ordering::SeqCst) > reads
        });
        quiet(&updates);
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            1
        );

        config.revision = 2;
        file.write(&json(&config));
        revision(&updates, 2);
        // In-place writes of equal length must also invalidate the metadata gate.
        config.revision = 3;
        std::fs::write(file.path(), json(&config)).unwrap();
        revision(&updates, 3);

        // Replace the configuration with a symlink, then atomically swap that link.
        config.revision = 4;
        std::fs::write(file.0.join("four"), json(&config)).unwrap();
        std::os::unix::fs::symlink("four", file.0.join("link")).unwrap();
        std::fs::rename(file.0.join("link"), file.path()).unwrap();
        revision(&updates, 4);
        config.revision = 5;
        std::fs::write(file.0.join("five"), json(&config)).unwrap();
        std::os::unix::fs::symlink("five", file.0.join("link")).unwrap();
        std::fs::rename(file.0.join("link"), file.path()).unwrap();
        revision(&updates, 5);

        // Kubernetes projection: the outer key symlink is never changed, and the
        // old target remains present. Only ..data is atomically retargeted.
        std::fs::create_dir(file.0.join("old")).unwrap();
        std::fs::create_dir(file.0.join("new")).unwrap();
        config.revision = 6;
        std::fs::write(file.0.join("old/config"), json(&config)).unwrap();
        std::os::unix::fs::symlink("old", file.0.join("..data")).unwrap();
        std::os::unix::fs::symlink("..data/config", file.0.join("link")).unwrap();
        std::fs::rename(file.0.join("link"), file.path()).unwrap();
        revision(&updates, 6);
        config.revision = 7;
        std::fs::write(file.0.join("new/config"), json(&config)).unwrap();
        std::os::unix::fs::symlink("new", file.0.join("..next")).unwrap();
        std::fs::rename(file.0.join("..next"), file.0.join("..data")).unwrap();
        revision(&updates, 7);
        quiet(&updates);
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            7
        );
        let start = Instant::now();
        drop(subscriber);
        assert!(start.elapsed() < Duration::from_millis(600));
    }

    #[test]
    fn a21_production_file_rejections_and_transient_prepare_retry() {
        let file = ConfigFile::new("retry");
        let (trust, mut config) = fixture();
        file.write(&json(&config));
        let updates = Arc::new(Updates::default());
        let subscriber =
            Subscriber::start(Source::File(file.path()), Arc::new(trust), updates.clone()).unwrap();
        revision(&updates, 1);
        file.write(b"invalid JSON");
        let reads = updates.subscription_probe.reads.load(Ordering::SeqCst);
        wait_for("unchanged rejected bytes retried", || {
            updates.subscription_probe.reads.load(Ordering::SeqCst) >= reads + 2
        });
        assert!(updates.status()["lastError"].is_string());
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            1
        );

        // The exact same bytes and metadata must be retried after a transient error
        // at the real Trust::prepare_with entry, without requiring a file event.
        updates
            .subscription_probe
            .fail_prepare
            .store(true, Ordering::SeqCst);
        config.revision = 2;
        file.write(&json(&config));
        wait_for("transient prepare rejection", || {
            updates.status()["lastError"]
                .as_str()
                .is_some_and(|e| e.contains("injected transient"))
        });
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        revision(&updates, 2);
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            3
        );
        quiet(&updates);

        // Publication, not preparation, defines acceptance: conflicting revision
        // reuse is rejected and retried, as is a rollback to an older revision.
        config.volumes.clear();
        config.peers.clear();
        let prepares = updates.subscription_probe.prepares.load(Ordering::SeqCst);
        file.write(&json(&config));
        wait_for("conflicting revision retried", || {
            updates.subscription_probe.prepares.load(Ordering::SeqCst) >= prepares + 2
        });
        assert!(!updates.latest(0).unwrap().config.volumes.is_empty());
        config.revision = 1;
        file.write(&json(&config));
        wait_for("rollback rejected", || {
            updates.status()["lastError"]
                .as_str()
                .is_some_and(|e| e.contains("rollback"))
        });
        config.revision = 3;
        file.write(&json(&config));
        revision(&updates, 3);
        assert!(updates.latest(0).unwrap().config.volumes.is_empty());
        quiet(&updates);

        std::fs::remove_file(file.path()).unwrap();
        wait_for("missing file error", || {
            updates.status()["lastError"].is_string()
        });
        file.write(&json(&config));
        revision(&updates, 3);
        quiet(&updates);
        file.write(b"bad");
        wait_for("shutdown during rejected input backoff", || {
            updates.status()["lastError"].is_string()
        });
        let start = Instant::now();
        drop(subscriber);
        assert!(start.elapsed() < Duration::from_millis(600));
    }

    #[test]
    fn a21_production_http_identical_200_and_rejected_retry() {
        let server = Server::new();
        let (subscriber, updates, trust, config) = server.start();
        let (mut socket, _, _) = server.next();
        let bytes = signed(&trust, config.clone());
        reply(&mut socket, &bytes, "\"one\"");
        for _ in 0..4 {
            let (mut socket, request, _) = server.next();
            assert!(request.contains("If-None-Match: \"one\""));
            assert_eq!(
                updates.subscription_probe.prepares.load(Ordering::SeqCst),
                1
            );
            reply(&mut socket, &bytes, "\"one\"");
        }
        let (mut socket, _, _) = server.next();
        reply(&mut socket, &bytes, "\"same-bytes-new-etag\"");
        let (mut socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"same-bytes-new-etag\""));
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            1
        );
        let mut config = config;
        config.revision = 2;
        let next = signed(&trust, config);
        updates
            .subscription_probe
            .fail_prepare
            .store(true, Ordering::SeqCst);
        reply(&mut socket, &next, "\"two\"");
        let (mut socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"same-bytes-new-etag\""));
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        reply(&mut socket, &next, "\"two\"");
        let (_socket, request, _) = server.next();
        assert!(request.contains("If-None-Match: \"two\""));
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            3
        );
        revision(&updates, 2);
        drop(subscriber);
    }

    #[test]
    fn a21_production_file_trust_rotation_and_signatures() {
        let file = ConfigFile::new("identity");
        let path = file.path();
        let (trust, config) = fixture();
        let signed_json = |trust: &Trust| {
            serde_json::to_vec(
                &proto::Configuration::decode(signed_config(trust, config.clone()).as_slice())
                    .unwrap(),
            )
            .unwrap()
        };
        std::fs::write(&path, signed_json(&trust)).unwrap();
        let updates = Arc::new(Updates::default());
        let subscriber = Subscriber::start(
            Source::File(path.clone()),
            Arc::new(Trust {
                universe: trust.universe,
                node: trust.node,
            }),
            updates.clone(),
        )
        .unwrap();
        revision(&updates, 1);
        quiet(&updates);
        assert_eq!(
            updates.subscription_probe.prepares.load(Ordering::SeqCst),
            1
        );

        let prepares = updates.subscription_probe.prepares.load(Ordering::SeqCst);
        let mut envelope =
            proto::Configuration::decode(signed_config(&trust, config.clone()).as_slice()).unwrap();
        if let Some(proto::configuration::Contents::Snapshot(s)) = &mut envelope.contents {
            s.node[0] ^= 1;
        }
        std::fs::write(&path, serde_json::to_vec(&envelope).unwrap()).unwrap();
        wait_for("foreign node snapshot retried", || {
            updates.subscription_probe.prepares.load(Ordering::SeqCst) >= prepares + 2
        });
        assert!(updates.status()["lastError"].is_string());
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        std::fs::write(&path, signed_json(&trust)).unwrap();
        revision(&updates, 1);
        quiet(&updates);
        drop(subscriber);
    }

    #[test]
    fn a21_production_file_retries_publication_barrier_without_file_change() {
        let file = ConfigFile::new("barrier");
        let (trust, mut config) = fixture();
        let updates = Arc::new(Updates::default());
        for _ in 0..2 {
            updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        }
        file.write(&json(&config));
        let subscriber =
            Subscriber::start(Source::File(file.path()), Arc::new(trust), updates.clone()).unwrap();
        revision(&updates, 1);
        updates.staged(1, 0, true);
        updates.staged(1, 1, true);
        assert_eq!(updates.decision(1), Decision::Activate);
        updates.activated(1, 0);
        config.revision = 2;
        file.write(&json(&config));
        wait_for("publication blocked until all-worker activation", || {
            updates.status()["lastError"]
                .as_str()
                .is_some_and(|e| e.contains("still activating"))
        });
        assert_eq!(updates.latest(0).unwrap().config.revision, 1);
        let prepares = updates.subscription_probe.prepares.load(Ordering::SeqCst);
        updates.activated(1, 1);
        revision(&updates, 2);
        assert!(updates.subscription_probe.prepares.load(Ordering::SeqCst) > prepares);
        assert_eq!(updates.decision(2), Decision::Waiting);
        updates.staged(2, 0, false);
        quiet(&updates);
        assert_eq!(updates.decision(2), Decision::Waiting);
        updates.staged(2, 0, true);
        updates.staged(2, 1, true);
        assert_eq!(updates.decision(2), Decision::Activate);
        updates.activated(2, 0);
        updates.activated(2, 1);
        quiet(&updates);
        drop(subscriber);
    }
}

mod forward_tests {
    use super::*;

    #[test]
    fn b15_rejected_newer_candidate_cannot_hide_receive_and_stale_rejection() {
        let updates = Updates::default();
        let (trust, mut config) = fixture();
        config.revision = 2;
        updates
            .command(prepare_snapshot(&trust, config), 2)
            .unwrap();
        updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
        updates.staged(2, 0, true);
        assert!(updates.receive_decision(2));
        updates.received(2, 0);
        let mut rejected = Rejection::default();
        rejected.record(3, "rejected3".into());
        let headers = rejected.headers(&updates, "accepted2", "boot");
        assert!(headers.contains(&("X-Racer-Digest", "accepted2".into())));
        assert!(headers.contains(&("X-Racer-Phase", "2".into())));
        assert!(rejected.check_revision(&updates, 1).is_err());
        assert!(rejected.check_revision(&updates, 2).is_ok());
        rejected.accepted(2); // terminal status for R2 must retain rejected R3
        assert_eq!(rejected.revision, 3);
        updates.command_phase(2, 3).unwrap();
        updates.activated(2, 0);
        updates.retired(2, 0);
        let headers = rejected.headers(&updates, "accepted2", "boot");
        assert!(headers.contains(&("X-Racer-Digest", "rejected3".into())));
        assert!(headers.contains(&("X-Racer-Needs-Config", "1".into())));
        rejected.record(1, "stale1".into());
        assert_eq!(rejected.digest, "rejected3");
    }

    #[test]
    fn b15_atomic_receive_latch_and_binding() {
        for received in [false, true] {
            let updates = Updates::default();
            let (trust, mut old) = fixture();
            updates
                .command(prepare_snapshot(&trust, old.clone()), 2)
                .unwrap();
            if received {
                assert!(updates.receive_decision(1));
            }
            // The grant could have raced a receive decision after eligibility.
            let digest = "ab".repeat(32);
            old.revision = 2;
            old.volumes[0].topology.as_mut().unwrap().epoch = 2;
            let mut c = proto::ControlCommand {
                revision: 2,
                phase: 1,
                forward_digest: vec![0xab; 32],
                forward_revision: 1,
                ..Default::default()
            };
            c.forward_revision = 3;
            assert!(
                updates
                    .forward_command(prepare_snapshot(&trust, old.clone()), &c, &digest)
                    .is_err()
            );
            c.forward_revision = 1;
            assert!(
                updates
                    .forward_command(prepare_snapshot(&trust, old.clone()), &c, &"cd".repeat(32))
                    .is_err()
            );
            let result = updates.forward_command(prepare_snapshot(&trust, old), &c, &digest);
            assert_eq!(result.is_ok(), !received);
            assert_eq!(
                updates.status()["candidateRevision"],
                if received { 1 } else { 2 }
            );
            if !received {
                assert!(
                    updates
                        .command(prepare_snapshot(&trust, fixture().1), 4)
                        .is_err(),
                    "rollback after correction"
                );
            }
        }
    }
}

#[cfg(test)]
#[derive(Default)]
pub(super) struct Probe {
    pub polls: AtomicU64,
    pub reads: AtomicU64,
    pub prepares: AtomicU64,
    pub fail_prepare: std::sync::atomic::AtomicBool,
}
#[cfg(test)]
thread_local! {
    pub(super) static PROBE: std::cell::RefCell<Option<Arc<Probe>>> = const { std::cell::RefCell::new(None) };
}
#[cfg(test)]
pub(super) fn probe(f: impl FnOnce(&Probe)) {
    PROBE.with_borrow(|p| {
        if let Some(p) = p {
            f(p);
        }
    });
}
#[cfg(test)]
pub(super) fn probe_prepare() -> io::Result<()> {
    let mut fail = false;
    probe(|p| {
        p.prepares.fetch_add(1, Ordering::SeqCst);
        fail = p.fail_prepare.swap(false, Ordering::SeqCst);
    });
    if fail {
        Err(io::Error::other("injected transient preparation failure"))
    } else {
        Ok(())
    }
}
