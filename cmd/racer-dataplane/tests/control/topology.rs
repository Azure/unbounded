// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;
    fn topology(p: u32) -> Topology {
        Topology::new(p, Epoch::new(17)).unwrap()
    }
    #[test]
    fn validates_slot_space_and_foreign_slots() {
        assert!(matches!(
            Topology::new(0, Epoch::new(0)),
            Err(Error::ZeroSlots)
        ));
        let t = topology(7);
        assert_eq!(t.slot(6).unwrap().get(), 6);
        let foreign = topology(8).slot(7).unwrap();
        let error = Error::SlotOutOfRange {
            slot: 7,
            slot_count: 7,
        };
        assert_eq!(t.slot(7), Err(error));
        assert_eq!(
            t.slot(u32::MAX),
            Err(Error::SlotOutOfRange {
                slot: u32::MAX,
                slot_count: 7
            })
        );
        assert!(matches!(t.candidates(foreign), Err(e) if e == error));
        let local = t.slot(0).unwrap();
        assert!(matches!(t.route(foreign, local), Err(e) if e == error));
        assert!(matches!(t.route(local, foreign), Err(e) if e == error));
    }
    #[test]
    fn exact_cube_root_boundaries() {
        for d in 1_u64..=1626 {
            let first = (d - 1).pow(3) + 1;
            let last = d.pow(3).min(u64::from(u32::MAX));
            for p in [first, last] {
                let t = topology(p as u32);
                assert_eq!(u64::from(t.degree()), d, "P={p}");
                assert!((d - 1).pow(3) < p && p <= d.pow(3));
            }
        }
    }
    #[test]
    fn fixed_placement_vectors_and_epoch_independence() {
        let t = topology(1000);
        let mut digest = [0; 32];
        assert_eq!(t.owner(&digest).get(), 0);
        digest[0] = 1;
        digest[1] = 2;
        assert_eq!(t.owner(&digest).get(), 513);
        digest[8..].fill(255);
        assert_eq!(t.owner(&digest).get(), 513);
        digest[..8].fill(255);
        assert_eq!(t.owner(&digest).get(), 615);
        let next_epoch = Topology::new(1000, Epoch::new(18)).unwrap();
        assert_eq!(next_epoch.owner(&digest), t.owner(&digest));
        assert_eq!(topology(4096).owner(&digest).get(), 4095);
        assert_eq!(topology(u32::MAX).owner(&digest).get(), 0);
    }
    #[test]
    fn candidates_wrap_visit_once_and_stay_exhausted() {
        for p in 1..=64 {
            let t = topology(p);
            for start in 0..p {
                let mut candidates = t.candidates(t.slot(start).unwrap()).unwrap();
                let mut seen = vec![false; p as usize];
                for offset in 0..p {
                    let n = (p - offset) as usize;
                    assert_eq!(candidates.size_hint(), (n, Some(n)));
                    let slot = candidates.next().unwrap().get();
                    assert_eq!(slot, (start + offset) % p);
                    assert!(!seen[slot as usize]);
                    seen[slot as usize] = true;
                }
                assert!(seen.into_iter().all(|s| s));
                assert_eq!(candidates.size_hint(), (0, Some(0)));
                assert_eq!(candidates.next(), None);
                assert_eq!(candidates.next(), None);
            }
        }
        let t = topology(u32::MAX);
        let mut candidates = t.candidates(t.slot(u32::MAX - 1).unwrap()).unwrap();
        assert_eq!(candidates.next().unwrap().get(), u32::MAX - 1);
        assert_eq!(candidates.next().unwrap().get(), 0);
    }
    fn check_route(t: &Topology, source: u32, destination: u32) -> usize {
        let mut route = t
            .route(t.slot(source).unwrap(), t.slot(destination).unwrap())
            .unwrap();
        assert_eq!(route.epoch(), Epoch::new(17));
        assert_eq!(route.destination().get(), destination);
        let mut edges = 0;
        loop {
            let previous = route.current().get();
            match route.advance() {
                Step::Arrived => break,
                Step::Forward { next } => {
                    let previous = t.slot(previous).unwrap();
                    assert_eq!(
                        t.distance(previous, route.destination()).unwrap(),
                        t.distance(next, route.destination()).unwrap() + 1
                    );
                    let mut suffix = t.route(previous, route.destination()).unwrap();
                    assert_eq!(suffix.advance(), Step::Forward { next });
                    let previous = previous.get();
                    assert_ne!(previous, destination, "must stop early");
                    edges += 1;
                    assert!(edges <= 3, "P={} {source}->{destination}", t.slot_count());
                    assert_eq!(route.current(), next);
                    assert!((0..t.degree()).any(|a| {
                        (u128::from(t.degree()) * u128::from(previous) + u128::from(a))
                            % u128::from(t.slot_count())
                            == u128::from(next.get())
                    }));
                }
            }
        }
        assert_eq!(route.current().get(), destination);
        assert_eq!(route.advance(), Step::Arrived);
        assert_eq!(route.advance(), Step::Arrived);
        edges
    }
    #[test]
    fn exhaustive_small_graph_reachability() {
        for p in 1..=128 {
            let t = topology(p);
            for source in 0..p {
                let mut distances = vec![u8::MAX; p as usize];
                distances[source as usize] = 0;
                let mut queue = std::collections::VecDeque::from([source]);
                while let Some(u) = queue.pop_front() {
                    for a in 0..t.degree() {
                        let v = (t.degree() * u + a) % p;
                        if distances[v as usize] == u8::MAX {
                            distances[v as usize] = distances[u as usize] + 1;
                            queue.push_back(v);
                        }
                    }
                }
                for destination in 0..p {
                    assert_eq!(
                        check_route(&t, source, destination),
                        distances[destination as usize] as usize
                    );
                    let collect = |source| {
                        let mut r = t
                            .route(t.slot(source).unwrap(), t.slot(destination).unwrap())
                            .unwrap();
                        let mut path = vec![source];
                        while let Step::Forward { next } = r.advance() {
                            path.push(next.get());
                        }
                        path
                    };
                    let path = collect(source);
                    for (i, &slot) in path.iter().enumerate() {
                        assert_eq!(&path[i..], collect(slot));
                    }
                }
            }
        }
    }
    #[test]
    fn large_graphs_and_overflow_boundaries() {
        for p in [
            4095,
            4096,
            4097,
            1625_u32.pow(3) - 1,
            1625_u32.pow(3),
            1625_u32.pow(3) + 1,
            u32::MAX - 1,
            u32::MAX,
        ] {
            let t = topology(p);
            let slots = [0, 1, p / 3, p / 2, p - 2, p - 1];
            for source in slots {
                for destination in slots {
                    check_route(&t, source, destination);
                }
            }
        }
    }
    #[test]
    fn local_and_early_arrival() {
        assert_eq!(check_route(&topology(1), 0, 0), 0);
        let t = topology(8);
        assert_eq!(check_route(&t, 3, 3), 0);
        assert_eq!(check_route(&t, 1, 2), 1);
        assert_eq!(check_route(&t, 1, 7), 2);
        assert_eq!(check_route(&t, 0, 7), 3);
    }
}

#[cfg(test)]
mod placement_tests {
    use crate::control::{Trust, proto};
    use crate::routing::Routing;
    use prost::Message;

    fn snapshot(path: &std::path::Path) -> proto::Snapshot {
        let c: proto::Configuration =
            serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        let Some(proto::configuration::Contents::Snapshot(s)) = c.contents else {
            panic!()
        };
        s
    }
    fn envelope(s: proto::Snapshot) -> proto::Configuration {
        proto::Configuration {
            contents: Some(proto::configuration::Contents::Snapshot(s)),
        }
    }
    fn trust(s: &proto::Snapshot) -> Trust {
        Trust {
            universe: s.universe.clone().try_into().unwrap(),
            node: s.node.clone().try_into().unwrap(),
        }
    }

    #[test]
    #[ignore = "requires B02_EXPORT from TestB02ProductionSnapshots"]
    fn b02_go_placement_conformance() {
        let dir = std::path::PathBuf::from(std::env::var("B02_EXPORT").unwrap());
        let files: Vec<String> =
            serde_json::from_slice(&std::fs::read(dir.join("files.json")).unwrap()).unwrap();
        for file in files {
            let s = snapshot(&dir.join(&file));
            let started = std::time::Instant::now();
            let prepared = trust(&s).prepare(envelope(s.clone())).unwrap();
            let r = &prepared.volumes[0].routing;
            let p = r.geometry.slot_count();
            let v = &s.volumes[0];
            let t = v.topology.as_ref().unwrap();
            let (prefix, recipient) = file.rsplit_once('-').unwrap();
            let owners: Vec<String> = serde_json::from_slice(
                &std::fs::read(dir.join(format!("{prefix}-owners.json"))).unwrap(),
            )
            .unwrap();
            let name = format!(
                "node-{:06}",
                recipient
                    .trim_end_matches(".json")
                    .parse::<usize>()
                    .unwrap()
            );
            for primary in 0..p as usize {
                assert_eq!(r.local.contains(&(primary as u32)), owners[primary] == name);
                if !file.contains("historical") {
                    assert!(
                        (1..3).any(|i| owners[primary] != owners[(primary + i) % owners.len()])
                    );
                    if !file.contains("-n2-") || p % 2 == 0 {
                        assert_ne!(owners[primary], owners[(primary + 1) % owners.len()]);
                    }
                }
            }
            assert!(t.local_slots.len() + t.neighbors.len() <= p as usize);
            assert_eq!(
                v.peer_endpoints.as_ref().unwrap().peers.len(),
                s.peers.len()
            );
            // In two-owner snapshots, local/not-local independently identifies each
            // physical owner even when the outgoing proof map is genuinely sparse.
            if file.contains("-n2-") && !file.contains("historical") {
                let mut cursor = r.start("/all-primary-proof");
                for primary in 0..p {
                    assert!(
                        (1..3).any(|i| r.local.contains(&primary)
                            != r.local.contains(&((primary + i) % p)))
                    );
                    if p == 131072 && !r.local.contains(&primary) {
                        cursor.owner = primary;
                        assert!(r.last_hop(&cursor));
                        cursor.attempt = 1;
                        assert!(r.next(&cursor).unwrap().is_none());
                        cursor.attempt = 0;
                    }
                }
            }
            for algorithm in [None, Some(2)] {
                let mut v = v.clone();
                v.topology.as_mut().unwrap().routing_algorithm = algorithm;
                let canonical = Routing::new(&s.universe, &v).unwrap();
                assert_eq!(canonical.algorithm, crate::routing::Algorithm::Canonical);
                assert_eq!(canonical.identity, r.identity);
            }
            let mut legacy = s.clone();
            legacy.volumes[0]
                .topology
                .as_mut()
                .unwrap()
                .routing_algorithm = Some(1);
            assert!(Routing::new(&legacy.universe, &legacy.volumes[0]).is_err());
            assert!(trust(&legacy).prepare(envelope(legacy)).is_err());
            // Exact sparse and endpoint checks cannot be bypassed by interleaving.
            let mut bad = s.clone();
            bad.volumes[0].topology.as_mut().unwrap().neighbors.pop();
            assert!(trust(&bad).prepare(envelope(bad.clone())).is_err());
            bad = s.clone();
            bad.volumes[0].peer_endpoints.as_mut().unwrap().peers[0].peer = "unknown".into();
            assert!(trust(&bad).prepare(envelope(bad)).is_err());
            println!(
                "B02_CONFIG {file} binary={} prepare_and_checks_ms={}",
                s.encoded_len(),
                started.elapsed().as_millis()
            );
        }
        let s = snapshot(&dir.join("b.json"));
        let r = Routing::new(&s.universe, &s.volumes[0]).unwrap();
        for label in [
            "live-head",
            "live-get",
            "stopped-head",
            "stopped-get",
            "local-head",
            "local-get",
            "semantic-head",
            "semantic-get",
        ] {
            let local = label.starts_with("local");
            let target = (0..)
                .map(|i| format!("/b02-{label}-{i}"))
                .find(|t| {
                    let c = r.start(t);
                    r.local.contains(&c.owner) == local && (local || r.last_hop(&c))
                })
                .unwrap();
            let c = r.start(&target);
            if !local {
                assert!(r.last_hop(&c));
                assert!((1..3).any(|i| r.local.contains(&((c.owner + i) % 131072))));
            }
            println!("B03_TARGET {label} {target}");
        }
        // Same production budget rejects nine single-owner default-slot volumes.
        let mut large = s.clone();
        large.peers.clear();
        large.volumes.clear();
        for i in 0..9 {
            let mut v = s.volumes[0].clone();
            v.id = format!("v{i}");
            v.listen = format!("127.0.0.1:{}", 20000 + i);
            v.peers.clear();
            v.peer_endpoints = Some(Default::default());
            let t = v.topology.as_mut().unwrap();
            t.local_slots = (0..131072).collect();
            t.neighbors.clear();
            large.volumes.push(v);
        }
        let e = trust(&large).prepare(envelope(large)).err().unwrap();
        assert!(e.to_string().contains("preparation budget"));
        let mut bad = s.clone();
        bad.volumes[0].topology.as_mut().unwrap().slot_count = 262145;
        assert!(
            trust(&bad)
                .prepare(envelope(bad))
                .err()
                .unwrap()
                .to_string()
                .contains("geometry budget")
        );
        let mut bad = s.clone();
        bad.volumes[0].topology.as_mut().unwrap().neighbors =
            vec![proto::SlotPeer::default(); 2 * 1024 * 1024];
        assert!(
            trust(&bad)
                .prepare(envelope(bad))
                .err()
                .unwrap()
                .to_string()
                .contains("preparation budget")
        );
        let mut bad = s.clone();
        bad.peers = vec![proto::Peer::default(); 100001];
        assert!(
            trust(&bad)
                .prepare(envelope(bad))
                .err()
                .unwrap()
                .to_string()
                .contains("object budget")
        );
        let mut bad = s.clone();
        bad.volumes[0].origin_address = "x".repeat(64 * 1024 * 1024);
        assert!(
            trust(&bad)
                .prepare(envelope(bad))
                .err()
                .unwrap()
                .to_string()
                .contains("byte budget")
        );
    }
}

#[cfg(test)]
mod physical_owner_proof {
    use crate::{control::proto, routing::Routing};

    fn volume(local: &[u32], owners: &[&str; 8]) -> proto::Volume {
        let slots: std::collections::BTreeSet<_> = local
            .iter()
            .flat_map(|s| [s * 2 % 8, (s * 2 + 1) % 8])
            .filter(|s| !local.contains(s))
            .collect();
        proto::Volume {
            id: "b03".into(),
            peers: slots
                .iter()
                .map(|s| owners[*s as usize].to_string())
                .collect::<std::collections::BTreeSet<_>>()
                .into_iter()
                .collect(),
            topology: Some(proto::Topology {
                epoch: 1,
                slot_count: 8,
                local_slots: local.to_vec(),
                neighbors: slots
                    .into_iter()
                    .map(|slot| proto::SlotPeer {
                        slot,
                        peer: owners[slot as usize].into(),
                    })
                    .collect(),
                ..Default::default()
            }),
            ..Default::default()
        }
    }

    #[test]
    fn routing_algorithm_defaults_to_canonical_and_rejects_legacy() {
        let mut v = volume(&[4, 5, 6, 7], &["A", "A", "A", "A", "B", "B", "B", "B"]);
        let default = Routing::new(&[1; 32], &v).unwrap();
        assert_eq!(default.algorithm, crate::routing::Algorithm::Canonical);
        v.topology.as_mut().unwrap().routing_algorithm = Some(2);
        let explicit = Routing::new(&[1; 32], &v).unwrap();
        assert_eq!(explicit.algorithm, default.algorithm);
        assert_eq!(explicit.identity, default.identity);
        for algorithm in [0, 1, 3, u32::MAX] {
            v.topology.as_mut().unwrap().routing_algorithm = Some(algorithm);
            let error = Routing::new(&[1; 32], &v).err().unwrap();
            assert_eq!(error.kind(), std::io::ErrorKind::InvalidData);
        }
    }

    #[test]
    fn b03_sparse_identity_proof_and_conservative_unknowns() {
        for algorithm in [None, Some(2)] {
            let mut v = volume(&[4, 5, 6, 7], &["A", "A", "A", "A", "B", "B", "B", "B"]);
            v.topology.as_mut().unwrap().routing_algorithm = algorithm;
            let r = Routing::new(&[1; 32], &v).unwrap();
            let target = (0..)
                .map(|i| format!("/b03-{i}"))
                .find(|t| r.start(t).owner == 3)
                .unwrap();
            let c = r.start(&target);
            assert!(r.last_hop(&c));
            let mut stale = c.clone();
            stale.identity[0] ^= 1;
            assert!(!r.last_hop(&stale));
            let mut invalid = c.clone();
            invalid.position = 3;
            assert!(!r.last_hop(&invalid));
            let mut successor = c.clone();
            successor.attempt = 1;
            assert_eq!(r.destination(&successor), 4);
            assert!(!r.last_hop(&successor));
            // Different peer identities remain distinct even if endpoints share an IP.
            v.topology.as_mut().unwrap().neighbors[3].peer = "C".into();
            v.peers.push("C".into());
            let distinct = Routing::new(&[1; 32], &v).unwrap();
            if distinct.next(&c).unwrap().unwrap().0 != "C" {
                assert!(!distinct.last_hop(&c));
            }
        }
        // Only outgoing slot 1 is known; slot 3 is not. Even a sole peer is no proof.
        let r = Routing::new(
            &[1; 32],
            &volume(&[0], &["B", "A", "A", "A", "A", "A", "A", "A"]),
        )
        .unwrap();
        let target = (0..)
            .map(|i| format!("/unknown-{i}"))
            .find(|t| r.start(t).owner == 3)
            .unwrap();
        assert!(!r.neighbors.contains_key(&3));
        assert!(!r.last_hop(&r.start(&target)));
    }

    #[test]
    #[ignore = "requires B03_GO_SNAPSHOT from the Go production snapshot producer"]
    fn b03_go_snapshot_consistency() {
        let path = std::env::var("B03_GO_SNAPSHOT").unwrap();
        let config: proto::Configuration =
            serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        let Some(proto::configuration::Contents::Snapshot(s)) = config.contents else {
            panic!()
        };
        let r = Routing::new(&s.universe, &s.volumes[0]).unwrap();
        assert_eq!(r.local.iter().copied().collect::<Vec<_>>(), [4, 5, 6, 7]);
        assert_eq!(
            r.neighbors.keys().copied().collect::<Vec<_>>(),
            [0, 1, 2, 3]
        );
        for label in ["live-head", "live-get", "stopped-head", "stopped-get"] {
            let target = (0..)
                .map(|i| format!("/b03-{label}-{i}"))
                .find(|t| r.start(t).owner == 3)
                .unwrap();
            let c = r.start(&target);
            assert_eq!(c.source, 4);
            assert_eq!(r.next(&c).unwrap().unwrap().1.position, 1);
            assert!(r.last_hop(&c));
            println!("B03_TARGET {label} {target}");
        }
    }
}
