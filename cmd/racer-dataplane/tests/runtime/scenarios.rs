// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Retained scenario coverage ledger:
// Basic full-stack traces now use runtime::dst: generated_request_lifecycle,
// managed_rdma_uses_real_sources_and_reads, and the durable action corpus.
// - crash seeds 0..8, signed-control 11/23, wide-page 99: cancellation, torn
//   persistence, read failure, signing rotation, aligned two-page cache reuse;
// - crossing HTTP/mixed/RDMA metadata + payload: route identity, independent
//   producers, concurrent cold metadata, no deadline escape, healthy sessions;
// - canonical convergence success/failure, shared-NUMA takeover, four HTTP
//   cancellation phases, window renewal/re-upgrade, 3x3 pressure cases;
// - http_auth::attribution: BackendScenario direct/HTTP/RDMA x admission/registration/headers plus
//   refused RDMA origin; statuses, cause/endpoint attribution, reuse retained;
// - http_auth::attribution: ProbeScenario 2 transports x 3 owner and 2 x 4 relay
//   statuses; real failure vs suppression, cooldown and successor checks;
// - bounded candidate limits 1/2/3 and suppressed 1/3, shared-relay refusal,
//   final-hop connect/headers/body timeouts, all-RDMA negative/reuse;
// - real TCP payload corruption/reconnect, both algorithm rollovers, three-hop
//   intermediate cache, owner-vs-relay failure, malformed/retired cursors;
// - cross-worker handshake, confirmation expiry, backoff/staging barrier,
//   inbound bounds, pinned Finish, multi-volume reload/failed bind.
// control::tests retains signed-control, rollover and malformed cursor cases;
// crate::conformance retains convergence/wide ranges; http_auth retains failure,
// cancellation, renewal, pressure and authenticated payload cases. All share
// these fixtures. HTTP origin/control states live in http_server.rs.

// Borrowed observers keep generation/session assertions readable without
// widening runtime's production API or cloning mutable manager state.
fn manager(generation: &Generation) -> std::cell::Ref<'_, Manager> {
    generation.manager.as_ref().unwrap().borrow()
}
fn manager_mut(generation: &Generation) -> std::cell::RefMut<'_, Manager> {
    generation.manager.as_ref().unwrap().borrow_mut()
}
fn session(generation: &Generation) -> Rc<rdma::Connection> {
    manager(generation).live[0].connection.clone()
}
fn generation(volumes: &Volumes, address: SocketAddr) -> Rc<Generation> {
    volumes.servers[&address].handler().current.clone()
}
pub(crate) mod dst {
    // B03 uses the production runtime, cache flights, signed HTTP and negotiated RDMA.
    fn b03_cluster(world: World, rdma: bool) -> Cluster {
        let mut s = Cluster::with_connections(world, rdma, true);
        for n in 0..8 {
            s.stop(n);
        }
        s.turns(20); // retire cancelled accepts before rebinding the two listeners
        for (n, remote) in [(0, 4), (4, 0)] {
            let id = NodeId::from_bytes(&[remote as u8 + 10; 32])
                .unwrap()
                .to_string();
            let c = &mut s.machines[n].config;
            c.peers = vec![proto::Peer {
                id: id.clone(),
                http_address: format!("127.0.0.1:{}", 10000 + remote),
                fabric: "dst".into(),
            }];
            let v = &mut c.volumes[0];
            v.peers = vec![id.clone()];
            v.topology = Some(proto::Topology {
                epoch: 1,
                slot_count: 8,
                local_slots: (n as u32..n as u32 + 4).collect(),
                neighbors: (remote as u32..remote as u32 + 4)
                    .map(|slot| proto::SlotPeer {
                        slot,
                        peer: id.clone(),
                    })
                    .collect(),
                ..Default::default()
            });
            s.boot(n, false);
        }
        s
    }

    #[test]
    fn b03_colocated_stopped_owner_head_get_strict_replay() {
        for rdma in [false, true] {
            let run = |replay: Option<Vec<crate::simulation::Choice>>| {
                let world = World::new(503);
                world.enable_scheduler();
                if let Some(replay) = replay {
                    world.replay(replay);
                }
                let _scope = world.enter();
                let mut s = b03_cluster(world.clone(), rdma);
                let routing = s.prepared(4).volumes[0].routing.clone();
                let target = s.target(3, "b03-stopped-head");
                let cursor = routing.start(&target);
                assert_eq!((cursor.source, cursor.owner), (4, 3));
                let (_, next) = routing.next(&cursor).unwrap().unwrap();
                assert_eq!(next.position, 1); // logical 4 -> 1 -> 3
                let a = s.prepared(0).volumes[0].routing.clone();
                a.validate(&next, &target).unwrap();
                assert_eq!(a.normalized_position(&next).unwrap(), 2);
                assert!(a.next(&next).unwrap().is_none());
                let sessions = s.warm_transport(rdma, &[(4, 0)]);
                // Validate a real signed exchange / negotiated READ before stopping A.
                let warm = s.target(3, "b03-live");
                assert_eq!(s.get(4, &warm, &[]), (200, b"abc".to_vec()));
                assert_transport(&world.events(), &warm, 4, 0, rdma);
                if rdma {
                    assert!(s.reads >= 1);
                }
                s.origin_only(&warm, 0);
                s.stop(0);
                let start = world.tick();
                let mut head = cold_head(&s, 4, &target);
                assert_eq!(
                    s.poll_head(4, &mut head),
                    200,
                    "B03 physical owner A refused: B must fall back to local candidate 4"
                );
                drop(head);
                // A different logical primary uses the very same stopped physical
                // owner while its transport breaker is still open. Slot 3 is also
                // unavailable; slot 4 must remain reachable within the three attempts.
                let cross = s.target(2, "b03-stopped-cross-slot");
                assert!(routing.last_hop(&routing.start(&cross)));
                let cross_start = world.tick();
                assert_eq!(s.get(4, &cross, &[]), (200, b"abc".to_vec()));
                assert!(world.tick() - cross_start < 1000, "same cooldown");
                s.origin_only(&cross, 4);
                s.absent(&cross, &["transport-http", "transport-rdma"]);
                world.advance(Duration::from_secs(2)); // GET independently proves failure after cooldown
                let get = s.target(3, "b03-stopped-get");
                assert_eq!(s.get(4, &get, &[]), (200, b"abc".to_vec()));
                for t in [&target, &get] {
                    let hits = s.hits.borrow();
                    assert!(hits.iter().any(|(n, x)| *n == 4 && x == t));
                    assert!(hits.iter().filter(|(_, x)| x == t).all(|(n, _)| *n == 4));
                    assert!(world.events().iter().any(|e| e.node == Some(4)
                        && e.target == *t
                        && e.kind == "candidate"
                        && e.detail == "owner=3 next=4 attempt=1"));
                }
                assert!(world.tick() - start < 20_000);
                assert!(s.handler(4).test_has_owner_evidence());
                drop(sessions);
                s.turns(100);
                let digest = clean_repro(s, &world);
                world.assert_replay_consumed();
                (digest, world.choices())
            };
            let (digest, choices) = run(None);
            assert!(!choices.is_empty());
            assert_eq!(run(Some(choices.clone())), (digest, choices.clone()));
            eprintln!(
                "B03 rdma={rdma} choices={} digest={digest:02x?}",
                choices.len()
            );
        }
    }

    #[test]
    fn b03_healthy_signed_peer_after_server_idle_close() {
        for head_first in [true, false] {
            let run = |replay: Option<Vec<crate::simulation::Choice>>| {
                let world = World::new(502);
                world.enable_scheduler();
                if let Some(replay) = replay {
                    world.replay(replay);
                }
                let _scope = world.enter();
                let mut s = b03_cluster(world.clone(), false);
                let warm = s.target(3, "idle-peer-warm");
                assert_eq!(s.get(4, &warm, &[]), (200, b"abc".to_vec()));
                // Cover both pre-timeout retirement and actual server-side idle close.
                for seconds in [21, 31] {
                    world.advance(Duration::from_secs(seconds));
                    s.turns(20);
                    let target = s.target(2, &format!("idle-peer-head-{seconds}"));
                    if head_first {
                        let mut head = cold_head(&s, 4, &target);
                        assert_eq!(s.poll_head(4, &mut head), 200);
                        drop(head);
                    } else {
                        assert_eq!(s.get(4, &target, &[]), (200, b"abc".to_vec()));
                    }
                    s.origin_only(&target, 0);
                    s.absent(&target, &["candidate"]);
                    let get = s.target(3, &format!("idle-peer-get-{seconds}"));
                    assert_eq!(s.get(4, &get, &[]), (200, b"abc".to_vec()));
                    s.origin_only(&get, 0);
                    s.absent(&get, &["candidate"]);
                    assert!(!s.handler(4).test_has_owner_evidence());
                }
                s.turns(100); // settle backend/cache completions before shutdown
                let digest = clean_repro(s, &world);
                world.assert_replay_consumed();
                (digest, world.choices())
            };
            let (digest, choices) = run(None);
            assert_eq!(run(Some(choices.clone())), (digest, choices));
        }
    }

    #[test]
    fn b03_genuine_relay_failure_strict_replay() {
        let run = |replay: Option<Vec<crate::simulation::Choice>>| {
            let world = World::new(505);
            world.enable_scheduler();
            if let Some(replay) = replay {
                world.replay(replay);
            }
            let _scope = world.enter();
            let mut s = Cluster::new(world.clone(), false);
            let target = s.target(3, "b03-genuine-relay");
            assert_route(&s, &target, 0, &[0, 1, 3]);
            let gate = s.gate(
                (0, 1),
                &target,
                crate::simulation::Phase::Connect,
                Some(libc::ECONNREFUSED),
                true,
            );
            let mut request = cold_head(&s, 0, &target);
            assert_eq!(s.poll_head(0, &mut request), 502);
            assert!(world.hits(gate) > 0);
            assert!(!s.handler(0).test_has_owner_evidence());
            s.absent(&target, &["candidate"]);
            assert!(s.hits.borrow().is_empty());
            world.release(gate);
            drop(request);
            let digest = clean_repro(s, &world);
            world.assert_replay_consumed();
            (digest, world.choices())
        };
        let (digest, choices) = run(None);
        assert_eq!(run(Some(choices.clone())), (digest, choices.clone()));
        eprintln!("B03 relay choices={} digest={digest:02x?}", choices.len());
    }

    #[test]
    fn b03_colocated_local_pressure_breaker_cancel_strict_replay() {
        use crate::simulation::{Choice, Phase};
        let run = |replay: Option<Vec<Choice>>| {
            let world = World::new(504);
            world.enable_scheduler();
            if let Some(replay) = replay {
                world.replay(replay);
            }
            let _scope = world.enter();
            let mut s = b03_cluster(world.clone(), false);
            for phase in [Phase::ConnectAdmission, Phase::Registration, Phase::Request] {
                let target = s.target(3, &format!("b03-local-{phase:?}"));
                let gate = s.gate((4, 0), &target, phase, None, false);
                let mut request = cold_head(&s, 4, &target);
                if phase == Phase::Request {
                    for _ in 0..500 {
                        s.turn();
                        assert!(matches!(
                            request.poll(s.ring(4), 8).unwrap(),
                            Progress::Pending(_)
                        ));
                        if world.hits(gate) > 0 {
                            break;
                        }
                    }
                    request.cancel(s.ring(4)).unwrap();
                    s.stop(4); // cancellation tears down the server's in-flight producer
                } else {
                    assert_eq!(s.poll_head(4, &mut request), 503);
                    assert!(!s.handler(4).test_has_owner_evidence());
                    drop(request);
                }
                assert!(world.hits(gate) > 0);
                s.absent(&target, &["candidate"]);
                assert!(s.hits.borrow().iter().all(|(_, t)| t != &target));
                world.release(gate);
            }
            s.turns(20);
            s.boot(4, false);
            let target = s.target(3, "b03-breaker");
            // Select the peer before testing its local breaker rejection.
            let warm = s.target(3, "b03-breaker-warm");
            assert_eq!(s.get(4, &warm, &[]), (200, b"abc".to_vec()));
            let (_, breaker) = s.handler(4).test_peer_breakers();
            breaker.try_acquire().unwrap().failure();
            let mut request = cold_head(&s, 4, &target);
            assert_eq!(s.poll_head(4, &mut request), 503);
            s.absent(&target, &["candidate"]);
            assert!(!s.handler(4).test_has_owner_evidence());
            drop(request);
            let digest = clean_repro(s, &world);
            world.assert_replay_consumed();
            (digest, world.choices())
        };
        let (digest, choices) = run(None);
        assert_eq!(run(Some(choices.clone())), (digest, choices.clone()));
        eprintln!(
            "B03 negatives choices={} digest={digest:02x?}",
            choices.len()
        );
    }

    #[test]
    #[ignore = "requires B02_EXPORT from Go production placement"]
    fn b02_go_interleaved_stopped_restart_strict_replay() {
        let dir = std::path::PathBuf::from(std::env::var("B02_EXPORT").unwrap());
        for p in [8, 131072] {
            for layout in ["fresh", "migrated"] {
                let run = |replay: Option<Vec<crate::simulation::Choice>>| {
                    let world = World::new(502);
                    world.enable_scheduler();
                    if let Some(replay) = replay {
                        world.replay(replay);
                    }
                    let _scope = world.enter();
                    let mut s = b03_cluster(world.clone(), false);
                    for n in [0, 4] {
                        s.stop(n);
                    }
                    s.turns(20);
                    for (n, i, remote) in [(0, 0, 4), (4, 1, 0)] {
                        let c: proto::Configuration = serde_json::from_slice(
                            &std::fs::read(dir.join(format!("p{p}-n2-{layout}-{i}.json"))).unwrap(),
                        )
                        .unwrap();
                        let Some(proto::configuration::Contents::Snapshot(c)) = c.contents else {
                            panic!()
                        };
                        let mut topology = c.volumes[0].topology.clone().unwrap();
                        let peer = NodeId::from_bytes(&[remote as u8 + 10; 32])
                            .unwrap()
                            .to_string();
                        for edge in &mut topology.neighbors {
                            edge.peer = peer.clone();
                        }
                        s.machines[n].config.volumes[0].topology = Some(topology);
                        s.boot(n, false);
                    }
                    let r = s.prepared(4).volumes[0].routing.clone();
                    let target = |label: &str| {
                        (0..)
                            .map(|i| format!("/b02-{p}-{layout}-{label}-{i}"))
                            .find(|t| {
                                let c = r.start(t);
                                !r.local.contains(&c.owner) && r.last_hop(&c)
                            })
                            .unwrap()
                    };
                    let warm = target("live");
                    assert_eq!(s.get(4, &warm, &[]), (200, b"abc".to_vec()));
                    s.origin_only(&warm, 0);
                    s.stop(0);
                    for label in ["head", "get"] {
                        world.advance(Duration::from_secs(2));
                        let t = target(label);
                        let owner = r.start(&t).owner;
                        if label == "head" {
                            assert_eq!(s.head(4, &t), 200);
                        } else {
                            assert_eq!(s.get(4, &t, &[]), (200, b"abc".to_vec()));
                        }
                        s.origin_only(&t, 4);
                        assert!(world.events().iter().any(|e| e.target == t
                            && e.kind == "candidate"
                            && e.detail.starts_with(&format!("owner={owner} next="))));
                    }
                    s.turns(100);
                    s.boot(0, false);
                    world.advance(Duration::from_secs(2));
                    let t = target("restart");
                    assert_eq!(s.get(4, &t, &[]), (200, b"abc".to_vec()));
                    s.origin_only(&t, 0);
                    s.turns(100);
                    let digest = clean_repro(s, &world);
                    world.assert_replay_consumed();
                    (digest, world.choices())
                };
                let (digest, choices) = run(None);
                assert_eq!(run(Some(choices.clone())), (digest, choices.clone()));
                eprintln!(
                    "B02 P={p} {layout} choices={} digest={digest:02x?}",
                    choices.len()
                );
            }
        }
    }
    #[test]
    fn peer_admission_owner_nonowner_and_local_contract() {
        for algorithm in [2] {
            for rdma in [false, true] {
                let world = World::new(12);
                let _entered = world.enter();
                let mut s = Cluster::with_algorithm(world.clone(), rdma, true, Some(algorithm));
                let _connections = s.warm_transport(rdma, &[(0, 1), (1, 3)]);
                for n in 0..8 {
                    s.handler(n).set_attempt_policy(3).unwrap();
                }
                let geometry =
                    crate::topology::Topology::new(8, crate::topology::Epoch::new(1)).unwrap();
                let target = |len: usize, tag: &str| {
                    (0..)
                        .map(|i| {
                            let prefix = format!("/wire-tag-{tag}/{i:08}/");
                            format!("{prefix}{}", "x".repeat(len - prefix.len()))
                        })
                        .find(|t| geometry.owner(blake3::hash(t.as_bytes()).as_bytes()).get() == 3)
                        .unwrap()
                };
                // Exact page boundary with strong tags, then one byte over. Repeat
                // from both placements and warm caches; HEAD and ranges agree.
                for (len, tag, expected) in
                    [(3389, "66", 200), (3390, "66", 200), (3391, "66", 414)]
                {
                    let t = target(len, tag);
                    let before = s.hits.borrow().len();
                    for _ in 0..2 {
                        for n in if len % 2 == 0 { [0, 3] } else { [3, 0] } {
                            let mut head = cold_head(&s, n, &t);
                            assert_eq!(
                                finish_head(&mut s, n, &mut head),
                                expected,
                                "HEAD len={len} tag={tag} node={n}"
                            );
                            for headers in [
                                &[][..],
                                &[("Range", "bytes=0-1")][..],
                                &[("If-None-Match", "*")][..],
                            ] {
                                let (status, body) = s.get(n, &t, headers);
                                let want = if expected != 200 {
                                    expected
                                } else if headers.is_empty() {
                                    200
                                } else if headers[0].0 == "Range" {
                                    206
                                } else {
                                    304
                                };
                                assert_eq!(status, want, "GET len={len} tag={tag} node={n}");
                                assert_eq!(
                                    body,
                                    match want {
                                        200 => b"abc".as_slice(),
                                        206 => b"ab",
                                        _ => b"",
                                    }
                                );
                            }
                        }
                    }
                    s.origin_only(&t, 3);
                    if expected == 200 {
                        assert_transport(&world.events(), &t, 0, 1, rdma);
                    }
                    s.absent(&t, &["candidate"]);
                    if expected == 414 {
                        assert_eq!(s.hits.borrow().len(), before);
                    } else {
                        assert!(s.hits.borrow().len() > before);
                    }
                }
                // All slots local: no distributed admission, preserving long URLs.
                let m = &mut s.machines[3];
                let volume = &mut m.config.volumes[0];
                volume.peers.clear();
                let topology = volume.topology.as_mut().unwrap();
                topology.local_slots = (0..8).collect();
                topology.neighbors.clear();
                s.reload(3, 2);
                let t = target(5000, "66");
                assert_eq!(s.head(3, &t), 200);
                assert_eq!(s.get(3, &t, &[]), (200, b"abc".to_vec()));
                s.origin_only(&t, 3);
                clean_repro(s, &world);
            }
        }
    }
    // Signed production Volumes/Handler/Cache exchanges on independently owned nodes.
    // No injected owner-health state: a different cold object establishes the failure.
    fn exhausted_cache_scenario(cap: u32) {
        let run = |replay: Option<Vec<crate::simulation::Choice>>| {
            let world = World::new(613);
            world.enable_scheduler();
            if let Some(replay) = replay {
                world.replay(replay);
            }
            let _scope = world.enter();
            let mut s = Cluster::new(world.clone(), false);
            if cap == 1 {
                s.handler(0).set_attempt_policy(1).unwrap();
            } // cap=3 deliberately uses the production default.
            let expired = s.target(3, "readiness13-expired");
            assert_eq!(s.get(0, &expired, &[]), (200, b"abc".to_vec()));
            world.advance(Duration::from_secs(61));
            s.turns(20);
            let warm = s.target(3, "readiness13-warm");
            let partial = s.target(3, "readiness13-metadata-only");
            assert_eq!(s.get(0, &warm, &[]), (200, b"abc".to_vec()));
            assert_eq!(s.head(0, &partial), 200);
            for target in [&expired, &warm, &partial] {
                assert_transport(&world.events(), target, 0, 1, false);
                assert_transport(&world.events(), target, 1, 3, false);
                s.origin_only(target, 3);
            }
            for node in 3..3 + cap {
                s.stop(node as usize);
            }
            let failure = s.target(3, "readiness13-independent-failure");
            let mut request = cold_head(&s, 0, &failure);
            assert_eq!(finish_head(&mut s, 0, &mut request), 503);
            drop(request);
            assert!(s.handler(0).test_has_owner_evidence());
            assert_eq!(
                world
                    .events()
                    .iter()
                    .filter(|e| e.node == Some(0) && e.target == failure && e.kind == "candidate")
                    .count(),
                cap as usize - 1,
                "every allowed candidate, and no fourth candidate"
            );
            let hits = s.hits.borrow().len();
            let event_start = world.events().len();
            let start = world.tick();
            // Both independent warm methods must succeed during suppression.
            let mut head = cold_head(&s, 0, &warm);
            assert_eq!(
                finish_head(&mut s, 0, &mut head),
                200,
                "warm HEAD during suppression"
            );
            drop(head);
            assert_eq!(s.get(0, &warm, &[]), (200, b"abc".to_vec()));
            assert_eq!(
                s.get(0, &warm, &[("Range", "bytes=1-2")]),
                (206, b"bc".to_vec())
            );
            assert_eq!(s.head(0, &partial), 200);
            // Metadata is present, but the first payload miss still needs upstream.
            assert_eq!(s.get(0, &partial, &[]), (503, Vec::new()));
            let cold = s.target(3, "readiness13-cold-control");
            for target in [&cold, &expired] {
                let mut request = cold_head(&s, 0, target);
                assert_eq!(finish_head(&mut s, 0, &mut request), 503);
                assert_eq!(s.get(0, target, &[]), (503, Vec::new()));
            }
            // Completed hits still require ordinary ingress fault admission.
            s.cache(0)
                .set_limits(crate::cache::Limits {
                    active_faults: 1,
                    internal_reserve: 0,
                    resource_retries: 2,
                })
                .unwrap();
            let held = s
                .cache(0)
                .metadata::<crate::handlers::Provider>(
                    "/readiness13-admission-holder",
                    world.now() + Duration::from_secs(5),
                )
                .unwrap();
            let mut request = cold_head(&s, 0, &warm);
            assert_eq!(finish_head(&mut s, 0, &mut request), 503);
            drop((held, request));
            s.cache(0)
                .set_limits(crate::cache::Limits::default())
                .unwrap();
            assert_eq!(s.head(0, &warm), 200);
            assert!(
                world.tick() - start < 1000,
                "all checks within the same cooldown"
            );
            assert_eq!(s.hits.borrow().len(), hits, "no nonowner origin access");
            assert!(
                !world.events()[event_start..]
                    .iter()
                    .any(|e| matches!(e.kind, "transport-http" | "transport-rdma" | "candidate")),
                "hits stay local and exhausted misses cannot start another attempt"
            );
            for node in 3..3 + cap {
                s.boot(node as usize, false);
            }
            world.advance(Duration::from_secs(2));
            assert_eq!(s.get(0, &expired, &[]), (200, b"abc".to_vec()));
            assert!(
                s.hits.borrow().len() > hits,
                "expired metadata must refresh"
            );
            s.origin_only(&expired, 3);
            s.turns(100);
            let digest = clean_repro(s, &world);
            world.assert_replay_consumed();
            (digest, world.choices())
        };
        let (digest, choices) = run(None);
        assert!(!choices.is_empty());
        assert_eq!(run(Some(choices.clone())), (digest, choices));
    }

    #[test]
    fn readiness13_warm_cache_cap_one() {
        exhausted_cache_scenario(1);
    }

    #[test]
    fn readiness13_warm_cache_default_cap() {
        exhausted_cache_scenario(3);
    }

    #[test]
    fn readiness13_warm_cache_physical_evidence() {
        let run = |replay: Option<Vec<crate::simulation::Choice>>| {
            let world = World::new(614);
            world.enable_scheduler();
            if let Some(replay) = replay {
                world.replay(replay);
            }
            let _scope = world.enter();
            let mut s = b03_cluster(world.clone(), false);
            // All three allowed logical candidates (1,2,3) belong to physical node 0.
            // Candidate 4 is local and healthy, but outside the default attempt cap.
            let warm = s.target(1, "readiness13-physical-warm");
            assert_eq!(s.get(4, &warm, &[]), (200, b"abc".to_vec()));
            assert_transport(&world.events(), &warm, 4, 0, false);
            s.origin_only(&warm, 0);
            s.stop(0);
            let failure = s.target(1, "readiness13-physical-independent");
            let mut head = cold_head(&s, 4, &failure);
            assert_eq!(finish_head(&mut s, 4, &mut head), 503);
            drop(head);
            assert!(s.handler(4).test_has_owner_evidence());
            let hits = s.hits.borrow().len();
            let start = world.tick();
            let event_start = world.events().len();
            assert_eq!(s.head(4, &warm), 200);
            assert_eq!(s.get(4, &warm, &[]), (200, b"abc".to_vec()));
            let cold = s.target(1, "readiness13-physical-cold");
            assert_eq!(s.get(4, &cold, &[]), (503, Vec::new()));
            assert!(world.tick() - start < 1000);
            assert_eq!(s.hits.borrow().len(), hits);
            let events = world.events();
            assert!(
                !events[event_start..]
                    .iter()
                    .any(|e| matches!(e.kind, "transport-http" | "transport-rdma"))
            );
            assert_eq!(
                events
                    .iter()
                    .filter(|e| e.target == failure && e.node == Some(4) && e.kind == "candidate")
                    .count(),
                2
            );
            assert!(
                !events.iter().any(|e| e.node == Some(4)
                    && e.kind == "candidate"
                    && e.detail.contains("attempt=3")),
                "local fourth owner cannot be used"
            );
            s.turns(100);
            let digest = clean_repro(s, &world);
            world.assert_replay_consumed();
            (digest, world.choices())
        };
        let (digest, choices) = run(None);
        assert!(!choices.is_empty());
        assert_eq!(run(Some(choices.clone())), (digest, choices));
    }
    #[test]
    fn replay_http_signed_busy_duplicate_and_invalid_context_never_execute() {
        use crate::{
            cache::peer_wire::hex,
            http_auth::{Policy, failure},
            http_client::attempt::PeerReason,
        };
        let world = World::new(714);
        let _scope = world.enter();
        let mut s = Cluster::with_algorithm(world.clone(), false, true, Some(2));
        world.node(Some(1));
        world.configure_replay(crate::http_auth::replay::Config {
            capacity: 1,
            shards: 1,
        });
        world.node(None);
        let (trust, _) = fixture();
        let policy = Policy {
            keys: trust.keys,
            universe: trust.universe,
            node: [10; 32],
            peers: [[11; 32]].into(),
        };
        let target = s.target(3, "replay-pressure");
        let routing = &s.prepared(0).volumes[0].routing;
        let mut cursor = routing.start(&target);
        cursor.position += 1; // 0 -> 1 -> 3
        let mut wire = b"RF04".to_vec();
        wire.extend(5000u32.to_le_bytes());
        wire.extend(cursor.algorithm.magic());
        wire.extend(cursor.encode());
        wire.extend(b"RF05\0");
        wire.extend(target.as_bytes());
        let context = "a".repeat(96);
        let mut headers = vec![
            ("X-Racer-Fault".into(), hex(&wire).into_bytes()),
            ("X-Racer-Attempt".into(), context.as_bytes().to_vec()),
        ];
        let pending = policy.request([11; 32], "GET", "/", &mut headers).unwrap();
        // Fill with a different nonce; Busy cannot admit a cache fault or backend IO.
        world.node(Some(1));
        world.accept_nonce([99; 32]).unwrap();
        world.node(None);
        let exchange =
            |s: &mut Cluster, headers: &[(String, Vec<u8>)], expected: u16, signed: bool| {
                let refs: Vec<_> = headers
                    .iter()
                    .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
                    .collect();
                let fill = s.ring(0).pool().private_fill().unwrap();
                let end = world.now() + Duration::from_secs(5);
                let mut request =
                    client::Connection::new("127.0.0.1:10001".parse().unwrap(), "localhost")
                        .unwrap()
                        .get(client::Request::new("/", &refs).unwrap(), fill, end)
                        .unwrap();
                let response = s.response(0, end, |ring| request.poll(ring, 32));
                assert_eq!(response.status(), expected);
                assert_eq!(response.content_length(), Some(0));
                if signed {
                    pending
                        .verify(&policy.keys, expected, 0, response.headers())
                        .unwrap();
                } else {
                    assert!(response.headers().get("x-racer-signature").is_none());
                }
                if expected == 503 {
                    let route = failure::AttemptRoute {
                        cursor: cursor.clone(),
                        candidate: routing.destination(&cursor),
                        endpoint: "127.0.0.1:10001".parse().unwrap(),
                        final_hop: false,
                        context: context.clone(),
                    };
                    let report =
                        failure::validate_peer_report(response.headers(), Some(0), 503, &route)
                            .unwrap();
                    assert_eq!(report.reason, PeerReason::Busy);
                    assert!(
                        response
                            .headers()
                            .get("x-racer-owner-unavailable")
                            .is_none()
                    );
                }
                assert!(
                    s.hits.borrow().is_empty(),
                    "rejected request executed at origin"
                );
                assert_eq!(
                    s.cache(1).metrics().values()[6..20],
                    [0; 14],
                    "rejected request reached cache"
                );
            };
        exchange(&mut s, &headers, 503, true);
        let mut bad = headers.clone();
        bad.iter_mut()
            .find(|(n, _)| n == "X-Racer-Signature")
            .unwrap()
            .1[0] ^= 1;
        exchange(&mut s, &bad, 400, false);
        // A valid signature over invalid routing context must not turn into Busy.
        let mut bad = headers[..2].to_vec();
        let mut wrong_cursor = cursor.clone();
        wrong_cursor.position = 0;
        let mut wrong_wire = b"RF04".to_vec();
        wrong_wire.extend(5000u32.to_le_bytes());
        wrong_wire.extend(wrong_cursor.algorithm.magic());
        wrong_wire.extend(wrong_cursor.encode());
        wrong_wire.extend(b"RF05\0");
        wrong_wire.extend(target.as_bytes());
        bad[0].1 = hex(&wrong_wire).into_bytes();
        let bad_pending = policy.request([11; 32], "GET", "/", &mut bad).unwrap();
        let refs: Vec<_> = bad
            .iter()
            .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
            .collect();
        let fill = s.ring(0).pool().private_fill().unwrap();
        let end = world.now() + Duration::from_secs(5);
        let mut request = client::Connection::new("127.0.0.1:10001".parse().unwrap(), "localhost")
            .unwrap()
            .get(client::Request::new("/", &refs).unwrap(), fill, end)
            .unwrap();
        let response = s.response(0, end, |ring| request.poll(ring, 32));
        assert_eq!(response.status(), 400);
        bad_pending
            .verify(&policy.keys, 400, 0, response.headers())
            .unwrap();
        drop((request, response));
        // After expiry, explicitly admit the captured nonce to represent its first
        // execution on another worker/volume, then replay the exact signed request.
        world.advance(Duration::from_secs(121));
        let mut fresh = headers[..2].to_vec();
        let fresh_pending = policy.request([11; 32], "GET", "/", &mut fresh).unwrap();
        let text = fresh
            .iter()
            .map(|(n, v)| format!("{n}: {}\r\n", std::str::from_utf8(v).unwrap()))
            .collect::<String>();
        let receiver = Policy {
            node: [11; 32],
            peers: [[10; 32]].into(),
            ..policy.clone()
        };
        let incoming =
            crate::cache::http_metadata::headers(&text, |h| receiver.receive("GET", "/", h))
                .unwrap();
        world.node(Some(1));
        incoming.accept_once().unwrap();
        world.node(None);
        // This duplicate returns 400 rather than conflating replay with capacity.
        // Use the matching pending context for signature verification below.
        let refs: Vec<_> = fresh
            .iter()
            .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
            .collect();
        let fill = s.ring(0).pool().private_fill().unwrap();
        let end = world.now() + Duration::from_secs(5);
        let mut request = client::Connection::new("127.0.0.1:10001".parse().unwrap(), "localhost")
            .unwrap()
            .get(client::Request::new("/", &refs).unwrap(), fill, end)
            .unwrap();
        let response = s.response(0, end, |ring| request.poll(ring, 32));
        assert_eq!(response.status(), 400);
        fresh_pending
            .verify(&policy.keys, 400, 0, response.headers())
            .unwrap();
        assert!(s.hits.borrow().is_empty());
        assert_eq!(s.cache(1).metrics().values()[6..20], [0; 14]);
        drop((request, response));
        clean_repro(s, &world);
    }
    use super::*;
    pub(crate) use crate::http_auth::attribution::assert_transport;
    use crate::{
        buffers::Key, http_client as client, simulation::World, simulation::corpus::Origin,
        workers::Driver as _,
    };

    pub(crate) use crate::runtime::dst::Cluster;
    impl Cluster {
        pub(crate) fn rollover(&mut self, algorithm: u32) -> impl Sized + use<> {
            let old = self.generation(0).unwrap();
            let identity = old._config.volumes[0].routing.identity;
            for n in 0..8 {
                self.machines[n].config.volumes[0]
                    .topology
                    .as_mut()
                    .unwrap()
                    .routing_algorithm = Some(algorithm);
                self.reload(n, 2);
            }
            assert!(!old.active.get());
            assert_eq!(old._config.volumes[0].routing.identity, identity);
            let current = self.prepared(0);
            assert_ne!(current.volumes[0].routing.identity, identity);
            assert_eq!(
                current.volumes[0].routing.algorithm,
                crate::routing::Algorithm::Canonical
            );
            old
        }
        pub(crate) fn cache(&self, n: usize) -> std::cell::RefMut<'_, crate::cache::Cache> {
            self.machines[n]
                .driver
                .application()
                .volumes
                .cache
                .borrow_mut()
        }
        pub(crate) fn manager_bounds(&self) {
            for n in 0..8 {
                let g = self.generation(n).unwrap();
                let manager = manager(&g);
                assert!(manager.live.len() <= 2 * manager.outbound.len().max(2));
                assert!(manager.outbound.len() <= 2);
            }
        }
        pub(crate) fn prepared(&self, n: usize) -> Arc<Prepared> {
            self.generation(n).unwrap()._config.clone()
        }
        pub(crate) fn updates(&self, n: usize) -> Arc<Updates> {
            self.machines[n]
                .driver
                .application()
                .volumes
                .updates
                .clone()
        }
        pub(crate) fn ring(&mut self, n: usize) -> &mut uring::Ring {
            self.machines[n].driver.ring_mut()
        }
        pub(crate) fn handler(&self, n: usize) -> std::cell::RefMut<'_, Handler> {
            self.machines[n].driver.application().volumes.servers[&self.address(n)]
                .handler()
                .current
                .handlers[0]
                .borrow_mut()
        }
        pub(crate) fn poll_head(&mut self, n: usize, request: &mut client::HeadExchange) -> u16 {
            loop {
                self.world.node(None);
                match request.poll(self.ring(n), 8).unwrap() {
                    Progress::Ready(response) => return response.status(),
                    Progress::Pending(_) => self.turn(),
                }
            }
        }
        pub(crate) fn gate(
            &self,
            edge: (usize, usize),
            target: &str,
            phase: crate::simulation::Phase,
            errno: Option<i32>,
            persistent: bool,
        ) -> usize {
            let gate =
                crate::simulation::Gate::new(edge.0, self.address(edge.1), target, phase, errno);
            self.world
                .gate(if persistent { gate.persistent() } else { gate })
        }
        pub(crate) fn warm_transport(
            &mut self,
            rdma: bool,
            edges: &[(usize, usize)],
        ) -> Vec<Rc<rdma::Connection>> {
            if rdma {
                self.warm_edges(edges)
            } else {
                Vec::new()
            }
        }
        pub(crate) fn probe_response(
            &mut self,
            node: usize,
            target: &str,
            status: u16,
        ) -> client::HeadExchange {
            let mut request = cold_head(self, node, target);
            assert_eq!(finish_head(self, node, &mut request), status);
            assert_transport(
                &self.world.events(),
                target,
                node,
                if node == 1 { 3 } else { 1 },
                false, // HEAD resolves typed metadata over HTTP
            );
            request
        }
        pub(crate) fn probe_reuse(&mut self, node: usize, target: &str, owner: usize) {
            assert_eq!(self.get(node, target, &[]), (200, b"abc".to_vec()));
            let hits = self.hits.borrow();
            let actual: Vec<_> = hits.iter().filter(|(_, t)| t == target).collect();
            assert!(!actual.is_empty());
            assert!(actual.iter().all(|(n, _)| *n == owner), "{actual:?}");
        }
        fn restart_origin(&mut self, n: usize) {
            let _scope = self.world.scoped_node(Some(n));
            let listener = http::Listener::bind(
                format!("127.0.0.1:{}", 11000 + n).parse().unwrap(),
                NonZeroU32::new(16).unwrap(),
            )
            .unwrap();
            self.machines[n].driver.application_mut().origin = Some(http::Server::new(
                listener,
                Origin {
                    hits: self.hits.clone(),
                    node: n,
                    scenario: true,
                },
                http::Config::default(),
            ));
            crate::workers::Wake::wake(&*self.machines[n].driver.wake_handle());
        }
        fn replace_ring(&mut self, n: usize) {
            self.reboot(n, false, None);
        }
        pub(crate) fn turns(&mut self, count: usize) {
            for _ in 0..count {
                self.turn();
            }
        }
        pub(crate) fn pending_heads(
            &mut self,
            requests: &mut [(usize, &mut client::HeadExchange)],
            turns: usize,
            ready: impl Fn(&World) -> bool,
        ) {
            for _ in 0..turns {
                self.turn();
                for (node, request) in requests.iter_mut() {
                    assert!(matches!(
                        request.poll(self.ring(*node), 32).unwrap(),
                        Progress::Pending(_)
                    ));
                }
                if ready(&self.world) {
                    break;
                }
            }
        }
        pub(crate) fn origin_only(&self, target: &str, owner: usize) {
            assert!(
                self.hits
                    .borrow()
                    .iter()
                    .filter(|(_, t)| t == target)
                    .all(|(node, _)| *node == owner)
            );
        }
        pub(crate) fn absent(&self, target: &str, kinds: &[&str]) {
            assert!(
                !self
                    .world
                    .events()
                    .iter()
                    .any(|e| e.target == target && kinds.contains(&e.kind))
            );
        }
        pub(crate) fn new(world: World, rdma: bool) -> Self {
            Self::with_connections(world, rdma, false)
        }
        pub(crate) fn with_connections(world: World, rdma: bool, multi_rdma: bool) -> Self {
            Self::with_algorithm(world, rdma, multi_rdma, None)
        }
        pub(crate) fn with_algorithm(
            world: World,
            rdma: bool,
            multi_rdma: bool,
            algorithm: Option<u32>,
        ) -> Self {
            Self::with_pool(world, rdma, multi_rdma, algorithm, 24)
        }
        pub(crate) fn with_pool(
            world: World,
            rdma: bool,
            multi_rdma: bool,
            algorithm: Option<u32>,
            count: usize,
        ) -> Self {
            use crate::runtime::dst::oracles::{Capabilities, Check};
            // These fixtures alter logical placement and inject failures outside
            // the canonical action corpus. Keep each replacement explicit;
            // continuous dependency/resource checks remain in the shared loop.
            let replacement = |reason, check, independence| Check::FixtureReplacement {
                reason,
                check,
                scope: "targeted runtime/security fixtures using with_pool",
                independence,
            };
            let oracles = Capabilities([
                replacement(
                    "faults are injected by fixture steps, not corpus fault_targets",
                    "absent; bounded candidate/timeout assertions at scenario call sites",
                    "expected event and status assertions",
                ),
                replacement(
                    "fixture publications change physical peers and colocated logical slots",
                    "assert_transport; b03_colocated_stopped_owner_head_get_strict_replay",
                    "explicit physical edge expectations",
                ),
                replacement(
                    "cursor normalization and alternate routing algorithms change rank",
                    "b03 logical cursor positions; algorithm rollover route assertions",
                    "fixture expectations plus production cursor validation",
                ),
                replacement(
                    "negotiated fixture edges include altered logical placement",
                    "assert_transport; warm_edges; READ counters at scenario call sites",
                    "physical path evidence, not an independent rank model",
                ),
                replacement(
                    "owner failure and colocation change allowed physical origins",
                    "origin_only; owner/relay attribution scenario hit assertions",
                    "explicit expected origin IDs",
                ),
            ]);
            Self::build(
                world,
                8,
                rdma,
                Some(crate::runtime::dst::Scenario {
                    oracles,
                    algorithm,
                    slots: count,
                    multi_rdma,
                }),
            )
        }
        fn boot(&mut self, n: usize, format: bool) {
            self.reboot(n, format, None);
            self.turn();
        }
        fn address(&self, n: usize) -> SocketAddr {
            self.machines[n].config.volumes[0].listen.parse().unwrap()
        }
        fn generation(&self, n: usize) -> Option<Rc<Generation>> {
            let m = &self.machines[n];
            Some(
                m.driver
                    .application()
                    .volumes
                    .servers
                    .get(&self.address(n))?
                    .handler()
                    .current
                    .clone(),
            )
        }
        pub(crate) fn warm_edges(&mut self, edges: &[(usize, usize)]) -> Vec<Rc<rdma::Connection>> {
            for &(a, b) in edges {
                let id = NodeId::from_bytes(&[b as u8 + 10; 32]).unwrap().to_string();
                self.generation(a)
                    .unwrap()
                    .manager
                    .as_ref()
                    .unwrap()
                    .borrow_mut()
                    .trigger(&id, "/warm-rdma-edges");
            }
            self.turns(250);
            edges
                .iter()
                .map(|&(a, b)| {
                    self.generation(a)
                        .unwrap()
                        .manager
                        .as_ref()
                        .unwrap()
                        .borrow()
                        .live
                        .iter()
                        .find(|p| p.outbound.is_some() && p.peer.bytes() == [b as u8 + 10; 32])
                        .unwrap_or_else(|| panic!("missing negotiated edge {a}->{b}"))
                        .connection
                        .clone()
                })
                .collect()
        }
        pub(crate) fn target(&self, owner: u32, label: &str) -> String {
            let t = crate::topology::Topology::new(8, crate::topology::Epoch::new(1)).unwrap();
            (0..)
                .map(|i| format!("/{label}?exact=%2f&v={i}"))
                .find(|s| t.owner(blake3::hash(s.as_bytes()).as_bytes()).get() == owner)
                .unwrap()
        }
        fn response<T>(
            &mut self,
            n: usize,
            end: Instant,
            mut poll: impl FnMut(&mut uring::Ring) -> io::Result<Progress<T>>,
        ) -> T {
            loop {
                self.turn();
                if let Progress::Ready(response) = poll(self.ring(n)).unwrap() {
                    return response;
                }
                assert!(
                    self.world.now() < end,
                    "DST stalled at {}",
                    self.world.tick()
                );
            }
        }
        pub(crate) fn get(
            &mut self,
            n: usize,
            target: &str,
            headers: &[(&str, &str)],
        ) -> (u16, Vec<u8>) {
            self.world.node(None);
            let fill = self.ring(n).pool().private_fill().unwrap();
            let end = self.world.now() + Duration::from_secs(15);
            let mut request = client::Connection::new(self.address(n), "localhost")
                .unwrap()
                .get(client::Request::new(target, headers).unwrap(), fill, end)
                .unwrap();
            let mut response = self.response(n, end, |ring| request.poll(ring, 32));
            (response.status(), response.body().to_vec())
        }
        pub(crate) fn head(&mut self, n: usize, target: &str) -> u16 {
            self.world.node(None);
            let end = self.world.now() + Duration::from_secs(15);
            let mut request = client::Connection::new(self.address(n), "localhost")
                .unwrap()
                .head(client::Request::new(target, &[]).unwrap(), end)
                .unwrap();
            let response = self.response(n, end, |ring| request.poll(ring, 32));
            assert_eq!(response.content_length(), Some(3));
            response.status()
        }
        fn stop(&mut self, n: usize) {
            self.world.node(Some(n));
            let (app, ring) = self.machines[n].driver.parts_mut();
            app.volumes.shutdown(ring).unwrap();
        }
        fn reload(&mut self, n: usize, epoch: u64) {
            let m = &mut self.machines[n];
            m.config.revision += 1;
            m.config.volumes[0].cache_generation = epoch;
            m.config.volumes[0].topology.as_mut().unwrap().epoch = epoch;
            let (mut trust, _) = fixture();
            trust.node = [n as u8 + 10; 32];
            m.driver
                .application()
                .volumes
                .updates
                .publish(crate::control::tests::prepare_cluster_snapshot(
                    &trust,
                    m.config.clone(),
                ))
                .unwrap();
            self.turn();
        }
    }

    fn fault_campaign(seed: u64) -> ([u8; 32], [u8; 32]) {
        let world = World::new(seed);
        let _scope = world.enter();
        world.short_transfers(31);
        let mut s = Cluster::new(world.clone(), false);
        let stable = s.target(0, "durable-before-crash");
        assert_eq!(s.get(0, &stable, &[]).0, 200);
        assert_eq!(s.get(0, &stable, &[]).0, 200);
        s.turns(100);
        // Cancellation at different connect/send/receive/crypto states. Outstanding
        // operations still own their buffers until completion or simulated death.
        let volatile = s.target(0, "cancelled-during-crash");
        let end = world.now() + Duration::from_secs(10);
        let mut pending = client::Connection::new(s.address(0), "localhost")
            .unwrap()
            .head(client::Request::new(&volatile, &[]).unwrap(), end)
            .unwrap();
        for _ in 0..(seed as usize % 40 + 1) {
            s.turn();
            if matches!(pending.poll(s.ring(0), 1).unwrap(), Progress::Ready(_)) {
                break;
            }
        }
        drop(pending);
        // Drive a second-sight admission, then kill the node at a seeded checkpoint
        // boundary without calling Volumes/Cache::shutdown or syncing its disk.
        let dirty = s.target(0, "dirty-admission");
        assert_eq!(s.get(0, &dirty, &[]).0, 200);
        assert_eq!(s.get(0, &dirty, &[]).0, 200);
        s.turns(seed as usize % 9);
        s.reboot(0, false, Some(seed as usize % 17));
        s.origin_off(0);
        // Pending compute observes cancelled/closed endpoints and wipes its result.
        world.advance(Duration::from_millis(10));
        world.run_tasks();
        let hits = s.hits.borrow().len();
        assert_eq!(s.get(0, &stable, &[]), (200, b"abc".to_vec()));
        assert_eq!(s.hits.borrow().len(), hits);
        // Inject a file-to-pipe splice failure after recovery. The cache
        // cannot complete a successful body after headers have been sent; the
        // client sees truncation, then a subsequent read recovers.
        s.stop(0);
        s.replace_ring(0);
        s.origin_off(0);
        world.fail_next(30); // IORING_OP_SPLICE
        let end = world.now() + Duration::from_secs(15);
        let fill = s.ring(0).pool().private_fill().unwrap();
        let mut request = client::Connection::new(s.address(0), "localhost")
            .unwrap()
            .get(client::Request::new(&stable, &[]).unwrap(), fill, end)
            .unwrap();
        loop {
            s.turn();
            match request.poll(s.ring(0), 32) {
                Err(error) => {
                    assert_eq!(error.kind(), io::ErrorKind::UnexpectedEof);
                    break;
                }
                Ok(Progress::Ready(_)) => panic!("failed splice completed a body"),
                Ok(Progress::Pending(_)) => assert!(world.now() < end),
            }
        }
        drop(request);
        assert!(world.fault_fired());
        s.restart_origin(0);
        world.advance(Duration::from_secs(31));
        assert_eq!(s.get(0, &stable, &[]), (200, b"abc".to_vec()));
        s.stop(0);
        let disk = s.machines[0].disk.digest();
        (clean_repro(s, &world), disk)
    }
    #[test]
    fn dst_crash_cancel_disk_fault_campaign() {
        for seed in 0..8 {
            assert_eq!(fault_campaign(seed), fault_campaign(seed), "seed {seed}");
        }
    }

    // Gated crossing routes must complete without deadline-driven teardown.
    pub(crate) fn trace_target(world: &World, target: &str) {
        for e in world
            .events()
            .iter()
            .filter(|e| e.target == target || matches!(e.kind, "breaker-error" | "http-timeout"))
        {
            eprintln!(
                "t={} node={:?} {} {} {}",
                e.tick, e.node, e.kind, e.target, e.detail
            );
        }
    }
    pub(crate) fn finish_head(
        s: &mut Cluster,
        n: usize,
        request: &mut client::HeadExchange,
    ) -> u16 {
        s.response(n, s.world.now() + Duration::from_secs(16), |ring| {
            request.poll(ring, 32)
        })
        .status()
    }
    pub(crate) fn cold_head(s: &Cluster, n: usize, target: &str) -> client::HeadExchange {
        s.world.node(None);
        client::Connection::new(s.address(n), "localhost")
            .unwrap()
            .head(
                client::Request::new(target, &[]).unwrap(),
                s.world.now() + Duration::from_secs(20),
            )
            .unwrap()
    }
    pub(crate) fn clean_repro(s: Cluster, _world: &World) -> [u8; 32] {
        s.finish().0
    }
    pub(crate) fn assert_route(s: &Cluster, target: &str, attempt: u32, path: &[usize]) {
        let routing = |n: usize| {
            let c = &s.machines[n].config;
            crate::routing::Routing::new(&c.universe, &c.volumes[0]).unwrap()
        };
        let mut cursor = routing(path[0]).start(target);
        cursor.attempt = attempt;
        for edge in path.windows(2) {
            let (peer, next) = routing(edge[0]).next(&cursor).unwrap().unwrap();
            assert_eq!(
                peer,
                NodeId::from_bytes(&[edge[1] as u8 + 10; 32])
                    .unwrap()
                    .to_string()
            );
            routing(edge[1]).validate(&next, target).unwrap();
            cursor = next;
        }
        assert!(
            routing(*path.last().unwrap())
                .next(&cursor)
                .unwrap()
                .is_none()
        );
    }
    fn cycle_repro(mode: &str) -> [u8; 32] {
        use crate::simulation::Phase;
        let world = World::new(71);
        let _scope = world.enter();
        let mut s = Cluster::with_algorithm(world.clone(), mode != "http", mode == "rdma", Some(2));
        let sessions = if mode == "rdma" {
            s.warm_edges(&[(1, 3), (2, 5), (5, 3)])
        } else if mode == "mixed" {
            s.warm_edges(&[(1, 3)])
        } else {
            Vec::new()
        };
        let target = s.target(3, "cold-crossing");
        assert_route(&s, &target, 0, &[1, 3]);
        assert_route(&s, &target, 0, &[2, 5, 3]);
        // Metadata always uses bounded HTTP exchanges, including with live QPs.
        let first_phase = Phase::Request;
        let second_phase = Phase::Request;
        let a = s.gate((1, 3), &target, first_phase, None, false);
        let b = s.gate((2, 5), &target, second_phase, None, false);
        let mut one = cold_head(&s, 1, &target);
        let mut two = cold_head(&s, 2, &target);
        s.pending_heads(&mut [(1, &mut one), (2, &mut two)], 500, |world| {
            world.hits(a) > 0 && world.hits(b) > 0
        });
        for n in [1, 2] {
            assert!(
                world
                    .events()
                    .iter()
                    .any(|e| e.kind == "flight-fill" && e.node == Some(n) && e.target == target),
                "both ingress producers must exist before crossing delivery"
            );
        }
        if world.hits(a) == 0 || world.hits(b) == 0 {
            trace_target(&world, &target);
        }
        world.release(a);
        world.release(b);
        let released = world.tick();
        s.turns(200);
        trace_target(&world, &target);
        let events = world.events();
        // Canonical routes converge at the owner instead of forming crossing cycles.
        let keys: std::collections::BTreeSet<_> = events
            .iter()
            .filter(|e| e.target == target && e.kind == "flight-fill")
            .filter_map(|e| e.key)
            .collect();
        assert_eq!(
            keys.len(),
            1,
            "converging metadata flights share one identity"
        );
        assert_transport(&events, &target, 1, 3, false);
        assert_transport(&events, &target, 2, 5, false);
        assert_transport(&events, &target, 5, 3, false);
        if mode == "rdma" {
            for (a, b) in [(2, 5), (5, 3), (1, 3)] {
                assert_transport(&events, &target, a, b, false);
            }
            assert!(
                !events
                    .iter()
                    .any(|e| e.target == target && e.kind == "transport-rdma")
            );
        }
        assert!(sessions.iter().all(|c| c.is_healthy()));
        assert!(
            s.hits.borrow().iter().any(|(_, t)| t == &target),
            "both routes reach owner 3 without waiting for a timeout"
        );
        let statuses = (
            finish_head(&mut s, 1, &mut one),
            finish_head(&mut s, 2, &mut two),
        );
        assert_eq!(statuses, (200, 200));
        assert!(
            world.tick() - released < 2_000,
            "success before service timeout"
        );
        s.absent(&target, &["http-timeout", "candidate"]);
        drop((one, two, sessions));
        clean_repro(s, &world)
    }
    #[test]
    fn dst_crossing_flights_http() {
        assert_eq!(cycle_repro("http"), cycle_repro("http"));
    }
    #[test]
    fn dst_step7_full_stack_two_workers_shared_numa_takeover() {
        fn scenario() -> [u8; 32] {
            use crate::simulation::Phase;
            let world = World::new(151);
            let _scope = world.enter();
            let mut s = Cluster::with_pool(world.clone(), false, false, None, 6);
            // This targeted takeover fixture uses an explicit second listener
            // so each caller selects its worker, with identical routing/ValueIds.
            let mut config = s.machines[0].config.clone();
            let address: SocketAddr = "127.0.0.1:12000".parse().unwrap();
            config.volumes[0].listen = address.to_string();
            let _worker = world.scoped_node(Some(8));
            let ring = uring::Ring::http_test_ring(
                s.ring(0).pool().test_other_worker(),
                uring::Config::default(),
            )
            .unwrap();
            s.add_worker(config, ring);
            let target = s.target(3, "full-stack-worker-takeover");
            let gate = s.gate((0, 1), &target, Phase::Connect, None, false);
            let mut producer = cold_head(&s, 0, &target);
            s.pending_heads(&mut [(0, &mut producer)], 500, |w| w.hits(gate) > 0);
            assert!(world.hits(gate) > 0);
            let mut survivor = cold_head(&s, 8, &target);
            s.pending_heads(&mut [(8, &mut survivor)], 100, |world| {
                world
                    .events()
                    .iter()
                    .any(|e| e.target == target && e.node == Some(8) && e.kind == "network-wait")
            });
            assert!(
                world
                    .events()
                    .iter()
                    .any(|e| e.target == target && e.node == Some(8) && e.kind == "network-wait")
            );
            producer.cancel(s.ring(0)).unwrap();
            s.stop(0); // drops the actual producing server task, not just its client
            world.release(gate);
            let start = world.tick();
            assert_eq!(finish_head(&mut s, 8, &mut survivor), 200);
            assert!(world.tick() - start < 2000);
            let events = world.events();
            for n in [0, 8] {
                assert!(
                    events.iter().any(|e| e.target == target
                        && e.node == Some(n)
                        && e.kind == "flight-fill")
                );
            }
            assert!(
                !events
                    .iter()
                    .any(|e| e.target == target && matches!(e.kind, "candidate" | "http-timeout"))
            );
            assert_eq!(s.get(8, &target, &[]), (200, b"abc".to_vec()));
            drop(survivor);
            clean_repro(s, &world)
        }
        assert_eq!(scenario(), scenario());
    }

    #[test]
    fn dst_crossing_flights_rdma_ingresses() {
        assert_eq!(cycle_repro("rdma"), cycle_repro("rdma"));
    }
    #[test]
    fn dst_crossing_flights_mixed() {
        assert_eq!(cycle_repro("mixed"), cycle_repro("mixed"));
    }

    #[test]
    fn dst_crossing_payload_flights() {
        for mode in ["http", "mixed", "rdma"] {
            assert_eq!(payload_crossing(mode), payload_crossing(mode));
        }
    }
    fn payload_crossing(mode: &str) -> [u8; 32] {
        use crate::simulation::Phase;
        let world = World::new(73);
        let _scope = world.enter();
        let mut s = Cluster::with_pool(world.clone(), mode != "http", true, Some(2), 12);
        let edges = [(1, 3), (2, 5), (5, 3)];
        let sessions = if mode == "rdma" {
            s.warm_edges(&edges)
        } else if mode == "mixed" {
            s.warm_edges(&edges[..1])
        } else {
            Vec::new()
        };
        let target = s.target(3, "payload-crossing");
        assert_route(&s, &target, 0, &[1, 3]);
        assert_route(&s, &target, 0, &[2, 5, 3]);
        for ingress in [1, 2] {
            assert_eq!(s.head(ingress, &target), 200);
        }
        let start = world.tick();
        let a = s.gate(
            (1, 3),
            &target,
            if mode == "http" {
                Phase::Request
            } else {
                Phase::RdmaRequest
            },
            None,
            false,
        );
        let b = s.gate(
            (2, 5),
            &target,
            if mode == "rdma" {
                Phase::RdmaRequest
            } else {
                Phase::Request
            },
            None,
            false,
        );
        let mut requests = Vec::new();
        world.node(None);
        for n in [1, 2] {
            let fill = s
                .ring(n)
                .pool()
                .stage(Key::new([240 + n as u8; 32]))
                .unwrap();
            requests.push(
                client::Connection::new(s.address(n), "localhost")
                    .unwrap()
                    .get(
                        client::Request::new(&target, &[]).unwrap(),
                        fill,
                        world.now() + Duration::from_secs(20),
                    )
                    .unwrap(),
            );
        }
        for _ in 0..500 {
            s.turn();
            for (i, request) in requests.iter_mut().enumerate() {
                assert!(matches!(
                    request.poll(s.ring(i + 1), 32).unwrap(),
                    Progress::Pending(_)
                ));
            }
            if world.hits(a) > 0 && world.hits(b) > 0 {
                break;
            }
        }
        assert!(world.hits(a) > 0 && world.hits(b) > 0);
        // Cold metadata overlaps blocked payload flights without taking a slot
        // from their small NUMA pool or using their crypto queue.
        let cold = s.target(3, "simultaneous-cold-metadata");
        let mut metadata = cold_head(&s, 1, &cold);
        for _ in 0..100 {
            s.turn();
            for (i, request) in requests.iter_mut().enumerate() {
                assert!(matches!(
                    request.poll(s.ring(i + 1), 32).unwrap(),
                    Progress::Pending(_)
                ));
            }
            assert!(matches!(
                metadata.poll(s.ring(1), 32).unwrap(),
                Progress::Pending(_)
            ));
            if world
                .events()
                .iter()
                .any(|e| e.target == cold && e.node == Some(1) && e.kind == "flight-fill")
            {
                break;
            }
        }
        assert!(
            world
                .events()
                .iter()
                .any(|e| e.target == cold && e.node == Some(1) && e.kind == "flight-fill"),
            "cold metadata producer must overlap gated payload producers"
        );
        let mut metadata_status = None;
        world.release(a);
        world.release(b);
        let mut done = [false; 2];
        while !done.iter().all(|d| *d) {
            s.turn();
            if metadata_status.is_none()
                && let Progress::Ready(response) = metadata.poll(s.ring(1), 32).unwrap()
            {
                metadata_status = Some(response.status());
            }
            for (i, request) in requests.iter_mut().enumerate() {
                if !done[i]
                    && let Progress::Ready(response) = request.poll(s.ring(i + 1), 32).unwrap()
                {
                    assert_eq!(response.status(), 200);
                    let (_, mut fill, len) = response.recycle();
                    assert_eq!(&fill.as_mut_slice()[..len], b"abc");
                    done[i] = true;
                }
            }
            assert!(
                world.tick() - start < 2_000,
                "payload crossing must finish before timeout"
            );
        }
        let events: Vec<_> = world
            .events()
            .into_iter()
            .filter(|e| e.tick >= start)
            .collect();
        for (from, to) in edges {
            if mode != "mixed" {
                assert_transport(&events, &target, from, to, mode == "rdma");
            } else {
                let endpoint = format!("127.0.0.1:{}", 10000 + to);
                assert!(events.iter().any(|e| e.target == target
                    && e.node == Some(from)
                    && matches!(e.kind, "rdma-deliver" | "transport-http")
                    && e.detail.contains(&endpoint)));
            }
        }
        for n in [1, 2] {
            let fills: Vec<_> = events
                .iter()
                .filter(|e| e.target == target && e.node == Some(n) && e.kind == "flight-fill")
                .collect();
            assert_eq!(fills.len(), 1);
        }
        assert_eq!(
            metadata_status.unwrap_or_else(|| finish_head(&mut s, 1, &mut metadata)),
            200
        );
        assert!(sessions.iter().all(|c| c.is_healthy()));
        assert!(
            !events
                .iter()
                .any(|e| e.target == target && matches!(e.kind, "candidate" | "http-timeout"))
        );
        if mode == "rdma" {
            assert!(
                !events
                    .iter()
                    .any(|e| e.target == target && e.kind == "transport-http")
            );
        }
        drop((requests, metadata, sessions));
        clean_repro(s, &world)
    }
}
// Included in runtime::tests. Real TCP/io_uring, independent node caches/pools.
pub(crate) struct Cluster {
    rings: Vec<uring::Ring>,
    nodes: Vec<Option<Volumes>>,
    addresses: Vec<SocketAddr>,
    pub(crate) hits: Arc<std::sync::Mutex<Vec<(usize, String)>>>,
    stop: Arc<std::sync::atomic::AtomicBool>,
    backends: Vec<std::thread::JoinHandle<()>>,
}
impl Cluster {
    pub(crate) fn config(&self, node: usize) -> Arc<Prepared> {
        self.generation(node)._config.clone()
    }
    pub(crate) fn reload(&mut self, node: usize, algorithm: Option<u32>) {
        let old = self.generation(node);
        let (mut trust, _) = fixture();
        trust.node = [node as u8 + 10; 32];
        let mut config = old._config.config.clone();
        config.volumes[0].origin_address = old._config.volumes[0].backend.address().to_string();
        config.revision = 2;
        let topology = config.volumes[0].topology.as_mut().unwrap();
        topology.epoch = 2;
        topology.routing_algorithm = algorithm;
        self.nodes[node]
            .as_ref()
            .unwrap()
            .updates
            .publish(crate::control::tests::prepare_cluster_snapshot(
                &trust, config,
            ))
            .unwrap();
        self.turn();
        assert!(!old.active.get());
        assert!(
            old.manager.as_ref().is_none_or(|m| m
                .borrow()
                .live
                .iter()
                .all(|p| p.connection.is_healthy()))
        );
    }
    pub(crate) fn expire_previous(&mut self, node: usize) {
        let server = &self.nodes[node].as_ref().unwrap().servers[&self.addresses[node]];
        for old in &server.handler().draining {
            old.drain.set(Some(Instant::now()));
        }
        self.turn();
    }
    fn generation(&self, node: usize) -> Rc<Generation> {
        generation(self.nodes[node].as_ref().unwrap(), self.addresses[node])
    }
    pub(crate) fn new() -> Option<Self> {
        Self::with_rdma(false)
    }
    fn with_rdma(rdma_enabled: bool) -> Option<Self> {
        Self::with_algorithm(rdma_enabled, None)
    }
    fn with_algorithm(rdma_enabled: bool, algorithm: Option<u32>) -> Option<Self> {
        // Leave ample retained-cache capacity for response destinations as well
        // as metadata/page buffers and private receive staging.
        drop(ring()?);
        let new_ring = || crate::conformance::ring(32, uring::Config::default());
        let first = new_ring();
        let reservations: Vec<_> = (0..8).map(|_| crate::conformance::reserve()).collect();
        let addresses: Vec<_> = reservations
            .iter()
            .map(|l| l.local_addr().unwrap())
            .collect();
        let hits = Arc::new(std::sync::Mutex::new(Vec::new()));
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let mut cluster = Self {
            rings: vec![first],
            nodes: vec![],
            addresses,
            hits,
            stop,
            backends: vec![],
        };
        for node in 0..8 {
            if node > 0 {
                cluster.rings.push(new_ring());
            }
            let (backend, task) =
                crate::conformance::origin(node, cluster.stop.clone(), cluster.hits.clone());
            cluster.backends.push(task);
            let (mut trust, _) = fixture();
            trust.node = [node as u8 + 10; 32];
            let config = cluster_config(
                node,
                &cluster.addresses,
                backend,
                algorithm,
                "topology-test",
            );
            let prepared = crate::control::tests::prepare_cluster_snapshot(&trust, config);
            let updates = Arc::new(Updates::default());
            updates.subscribe(cluster.rings[node].wake_handle());
            updates.publish(prepared).unwrap();
            let crypto = Arc::new(crate::crypto::Pool::test_pool(cluster.rings[node].pool()));
            let worker = usize::from(node == 1 || node == 7);
            let mut volumes = Volumes::new(crate::cache::tests::cache(1), updates, crypto, worker);
            // The simulated transport supports one connection per rail. Separate
            // inbound/outbound rails at intermediates; production rails multiplex.
            if rdma_enabled {
                volumes = volumes.with_rdma(Some(
                    negotiation::Rails::new(
                        vec![
                            Some(rdma::test_transport(cluster.rings[node].pool())),
                            Some(rdma::test_transport(cluster.rings[node].pool())),
                        ],
                        2,
                    )
                    .unwrap(),
                ));
            }
            volumes.poll(&mut cluster.rings[node], 64).unwrap();
            cluster.nodes.push(Some(volumes));
        }
        drop(reservations);
        Some(cluster)
    }
    fn turn(&mut self) {
        for (ring, node) in self.rings.iter_mut().zip(&mut self.nodes) {
            ring.progress().unwrap();
            if let Some(node) = node {
                node.poll(ring, 64).unwrap();
                for server in node.servers.values() {
                    if let Some(manager) = &server.handler().current.manager {
                        negotiation::test_confirmations(&manager.borrow().rails);
                    }
                }
            }
        }
    }
    pub(crate) fn target(&self, owner: u32, prefix: &str) -> String {
        let geometry = crate::topology::Topology::new(8, crate::topology::Epoch::new(1)).unwrap();
        (0..)
            .map(|n| format!("/{prefix}?exact=%2f&v={n}"))
            .find(|t| geometry.owner(blake3::hash(t.as_bytes()).as_bytes()).get() == owner)
            .unwrap()
    }
    pub(crate) fn get(&mut self, node: usize, target: &str) -> (u16, Vec<u8>) {
        self.get_headers(node, target, &[])
    }
    pub(crate) fn get_headers(
        &mut self,
        node: usize,
        target: &str,
        headers: &[(&str, &str)],
    ) -> (u16, Vec<u8>) {
        use crate::{buffers::Key, http_client as client};
        let key =
            *blake3::hash(format!("response {node} {target} {headers:?}").as_bytes()).as_bytes();
        let fill = self.rings[node].pool().stage(Key::new(key)).unwrap();
        let end = Instant::now() + Duration::from_secs(15);
        let mut signed_headers: Vec<_> = headers
            .iter()
            .map(|(n, v)| (n.to_string(), v.as_bytes().to_vec()))
            .collect();
        if headers
            .iter()
            .any(|(n, _)| n.eq_ignore_ascii_case("x-racer-fault"))
        {
            let (trust, _) = fixture();
            let policy = crate::http_auth::Policy {
                keys: trust.keys,
                universe: trust.universe,
                node: [10; 32],
                peers: Default::default(),
            };
            policy
                .request([(10 + node) as u8; 32], "GET", target, &mut signed_headers)
                .unwrap();
        }
        let headers: Vec<_> = signed_headers
            .iter()
            .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
            .collect();
        let mut request = client::Connection::new(self.addresses[node], "localhost")
            .unwrap()
            .get(client::Request::new(target, &headers).unwrap(), fill, end)
            .unwrap();
        loop {
            self.turn();
            if let Progress::Ready(mut response) = request.poll(&mut self.rings[node], 64).unwrap()
            {
                return (response.status(), response.body().to_vec());
            }
            assert!(Instant::now() < end, "topology request stalled");
        }
    }
    pub(crate) fn remove(&mut self, node: usize) {
        if let Some(mut volume) = self.nodes[node].take() {
            volume.shutdown(&mut self.rings[node]).unwrap();
        }
    }
}

#[test]
fn topology_negotiated_rdma_three_edges_plaintext_forwarding() {
    topology_negotiated_algorithm_rollover(None, Some(2));
    topology_negotiated_algorithm_rollover(Some(2), None);
}
fn topology_negotiated_algorithm_rollover(initial: Option<u32>, replacement: Option<u32>) {
    let Some(mut c) = Cluster::with_algorithm(true, initial) else {
        return;
    };
    // First upgrade only 0 -> 1 from an ordinary payload miss. Node 1 is the
    // owner here, so it must not eagerly negotiate its unrelated neighbors.
    let warmup = c.target(1, "automatic-first-edge");
    use crate::{buffers::Key, http_client as client};
    let end = Instant::now() + Duration::from_secs(5);
    let fill = c.rings[0].pool().private_fill().unwrap();
    let mut head = client::Connection::new(c.addresses[0], "localhost")
        .unwrap()
        .get(client::Request::new(&warmup, &[]).unwrap(), fill, end)
        .unwrap();
    loop {
        c.turn();
        if let Progress::Ready(response) = head.poll(&mut c.rings[0], 64).unwrap() {
            assert_eq!(response.status(), 200);
            break;
        }
        assert!(Instant::now() < end);
    }
    let get = |c: &Cluster, node: usize, peer: usize, outbound: bool| {
        let generation = c.generation(node);
        manager(&generation)
            .live
            .iter()
            .find(|p| p.peer.bytes() == [peer as u8 + 10; 32] && p.outbound.is_some() == outbound)
            .map(|p| p.connection.clone())
    };
    let end = Instant::now() + Duration::from_secs(5);
    let first = loop {
        c.turn();
        if let (Some(a), Some(b)) = (get(&c, 0, 1, true), get(&c, 1, 0, false)) {
            break (a, b);
        }
        assert!(Instant::now() < end);
    };
    assert!(manager(&c.generation(1)).outbound.is_empty());
    // Only RDMA faults reach relay 1 for this target. Their outgoing HTTP
    // fallback must automatically negotiate 1 -> 3, then 3 -> 7.
    let target = c.target(7, "automatic-relay-upgrade");
    let fill = c.rings[0].pool().stage(Key::new([230; 32])).unwrap();
    let mut request = client::Connection::new(c.addresses[0], "localhost")
        .unwrap()
        .get(client::Request::new(&target, &[]).unwrap(), fill, end)
        .unwrap();
    let mut first_reads = 0;
    loop {
        c.turn();
        first_reads += first.0.test_pump(&first.1, false);
        first.1.test_pump(&first.0, false);
        for (a, b) in [(1, 3), (3, 7)] {
            if let (Some(a), Some(b)) = (get(&c, a, b, true), get(&c, b, a, false)) {
                a.test_pump(&b, false);
                b.test_pump(&a, false);
            }
        }
        if let Progress::Ready(mut response) = request.poll(&mut c.rings[0], 64).unwrap() {
            assert_eq!(response.status(), 200);
            assert_eq!(response.body(), b"abc");
            break;
        }
        assert!(Instant::now() < end, "automatic relay upgrade stalled");
    }
    assert_eq!(first_reads, 1);
    let pairs = loop {
        c.turn();
        let mut pairs = Vec::new();
        for (a, b) in [(0, 1), (1, 3), (3, 7)] {
            if let (Some(a), Some(b)) = (get(&c, a, b, true), get(&c, b, a, false)) {
                pairs.push((a, b));
            }
        }
        if pairs.len() == 3 {
            break pairs;
        }
        assert!(Instant::now() < end, "negotiated pairs {}", pairs.len());
    };
    let old = c.generation(1);
    for reload in [false, true] {
        if reload {
            c.reload(1, replacement);
        }
        // After reload, incoming and outgoing old sessions must still service
        // their pinned routing identity, even though the algorithm changed.
        let target = c.target(7, &format!("plaintext-three-edges-{reload}"));
        let fill = c.rings[0]
            .pool()
            .stage(Key::new([231 + u8::from(reload); 32]))
            .unwrap();
        let mut request = client::Connection::new(c.addresses[0], "localhost")
            .unwrap()
            .get(client::Request::new(&target, &[]).unwrap(), fill, end)
            .unwrap();
        let mut reads = [0; 3];
        loop {
            c.turn();
            for (i, (a, b)) in pairs.iter().enumerate() {
                reads[i] += a.test_pump(b, false);
                b.test_pump(a, false);
            }
            if let Progress::Ready(mut response) = request.poll(&mut c.rings[0], 64).unwrap() {
                assert_eq!(response.status(), 200);
                assert_eq!(response.body(), b"abc");
                break;
            }
            assert!(Instant::now() < end, "RDMA route stalled: {reads:?}");
        }
        assert_eq!(reads, [1, 1, 1]);
        let hits = c.hits.lock().unwrap();
        assert_eq!(hits.len(), if reload { 8 } else { 6 });
        assert!(
            hits[hits.len() - 2..]
                .iter()
                .all(|(n, r)| *n == 7 && r.contains(&target))
        );
    }
    old.drain.set(Some(Instant::now()));
    c.turn();
    assert!(!pairs[0].1.is_healthy() && !pairs[1].0.is_healthy());
    assert!(manager(&old).live.is_empty());
}
impl Drop for Cluster {
    fn drop(&mut self) {
        for node in 0..self.nodes.len() {
            self.remove(node);
        }
        self.stop.store(true, std::sync::atomic::Ordering::Relaxed);
        for backend in self.backends.drain(..) {
            backend.join().unwrap();
        }
    }
}

#[test]
fn topology_three_edges_intermediate_cache_and_original_target() {
    let Some(mut c) = Cluster::new() else { return };
    let target = c.target(7, "three-edges");
    // Canonical 0 -> 1 -> 3 -> 7: exactly two intermediate nodes.
    assert_eq!(c.get(0, &target), (200, b"abc".to_vec()));
    let hits = c.hits.lock().unwrap().clone();
    assert_eq!(hits.len(), 2);
    for (node, request) in hits {
        assert_eq!(node, 7);
        assert!(request.contains(&format!(" {target} HTTP/1.1\r\n")));
    }
    assert!(c.hits.lock().unwrap()[1].1.contains("Range: bytes=0-2\r\n"));
    c.remove(7);
    assert_eq!(c.get(1, &target), (200, b"abc".to_vec()));
    assert_eq!(c.get(3, &target), (200, b"abc".to_vec()));
    assert_eq!(
        c.hits.lock().unwrap().len(),
        2,
        "intermediates retain both metadata and payload"
    );
}

use super::*;
use crate::control::{
    proto,
    tests::{cluster_config, fixture, prepare_snapshot, ring, runtime_pair as prepared},
};
fn address() -> SocketAddr {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
}
pub(crate) fn activate(
    ring: &mut uring::Ring,
    config: Prepared,
    worker: usize,
    rails: negotiation::Rails,
) -> Volumes {
    let updates = Arc::new(Updates::default());
    updates.subscribe(ring.wake_handle());
    updates.publish(config).unwrap();
    let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));
    let mut volumes =
        Volumes::new(crate::cache::tests::cache(1), updates, crypto, worker).with_rdma(Some(rails));
    volumes.poll(ring, 32).unwrap();
    volumes
}
// Narrow observers for authenticated transport scenarios in http_auth.
pub(crate) fn live(volumes: &Volumes, address: SocketAddr) -> Option<Rc<rdma::Connection>> {
    manager(&generation(volumes, address))
        .live
        .first()
        .map(|p| p.connection.clone())
}
pub(crate) fn warm(volumes: &Volumes, address: SocketAddr) {
    let g = generation(volumes, address);
    manager_mut(&g).trigger(&g._config.volumes[0].config.peers[0], "/warmup");
}
pub(crate) fn confirm(volumes: &Volumes, address: SocketAddr) {
    negotiation::test_confirmations(&manager(&generation(volumes, address)).rails);
}
pub(crate) fn peer_breakers(
    volumes: &Volumes,
    address: SocketAddr,
) -> (
    crate::breaker::CircuitBreaker,
    crate::breaker::CircuitBreaker,
) {
    generation(volumes, address).handlers[0]
        .borrow()
        .test_peer_breakers()
}
pub(crate) fn owned_target(volumes: &Volumes, address: SocketAddr, label: &str) -> String {
    let g = generation(volumes, address);
    (0..)
        .map(|n| format!("/{label}?n={n}"))
        .find(|t| g._config.volumes[0].routing.start(t).owner == 1)
        .unwrap()
}
fn handshake(
    ring: &mut uring::Ring,
    a: &mut Volumes,
    b: &mut Volumes,
    aa: SocketAddr,
    ba: SocketAddr,
) {
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        ring.progress().unwrap();
        a.poll(ring, 64).unwrap();
        b.poll(ring, 64).unwrap();
        negotiation::test_confirmations(&manager(&generation(a, aa)).rails);
        negotiation::test_confirmations(&manager(&generation(b, ba)).rails);
        let alive = |v: &Volumes, addr| !manager(&generation(v, addr)).live.is_empty();
        if alive(a, aa) && alive(b, ba) {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "automatic negotiation did not complete"
        );
        std::thread::yield_now();
    }
}
#[test]
fn automatic_cross_worker_handshake_confirmation_reconnect_and_drain() {
    let Some(mut ring) = ring() else { return };
    let aa = address();
    let ba = address();
    // Initiator shard 5 selects physical index 1, responder worker 9 also
    // uses incoming shard 5 (index 2), with different catalog lengths.
    let a_rails =
        negotiation::Rails::new(vec![None, Some(rdma::test_transport(ring.pool()))], 2).unwrap();
    let b_rails =
        negotiation::Rails::new(vec![None, None, Some(rdma::test_transport(ring.pool()))], 3)
            .unwrap();
    let mut a = activate(&mut ring, prepared(2, aa, ba, true), 5, a_rails);
    let mut b = activate(&mut ring, prepared(3, ba, aa, false), 9, b_rails);
    let ag = generation(&a, aa);
    let bg = generation(&b, ba);
    // Metadata is HTTP-only; a payload fault triggers RDMA negotiation. Trigger
    // that notification directly here because this fixture has no origin server.
    let target = (0..)
        .map(|n| format!("/object?version={n}"))
        .find(|t| ag._config.volumes[0].routing.start(t).owner == 1)
        .unwrap();
    let mut request = crate::http_client::Connection::new(aa, "localhost")
        .unwrap()
        .head(
            crate::http_client::Request::new(&target, &[]).unwrap(),
            Instant::now() + Duration::from_secs(3),
        )
        .unwrap();
    manager_mut(&ag).trigger(&ag._config.volumes[0].config.peers[0], &target);
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        ring.progress().unwrap();
        a.poll(&mut ring, 64).unwrap();
        assert!(matches!(
            request.poll(&mut ring, 64).unwrap(),
            Progress::Pending(_)
        ));
        if !manager(&ag).outbound.is_empty() {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "fault did not trigger negotiation"
        );
    }
    a.poll(&mut ring, 64).unwrap();
    assert!(manager(&ag).outbound[0].client.is_some());
    for _ in 0..10 {
        manager_mut(&ag).trigger(&ag._config.volumes[0].config.peers[0], &target);
    }
    assert_eq!(manager(&ag).outbound.len(), 1);
    handshake(&mut ring, &mut a, &mut b, aa, ba);
    let ac = session(&ag);
    let bc = session(&bg);
    assert!(ac.is_authenticated() && bc.is_authenticated());
    assert_eq!(manager(&bg).live[0].context.shard(), 5);
    assert_eq!(bg._config.config.volumes[0].peers.len(), 1);
    assert!(ac.is_confirmed() && bc.is_confirmed());
    let ticket = ac.test_deliver_request(&bc);
    assert!(bc.authenticated_received());
    b.poll(&mut ring, 32).unwrap(); // services inbound without HTTP requests
    assert!(manager(&bg).live[0].confirmation.is_none());
    assert!(bc.next_request().unwrap().is_none()); // handler drained the CQ request
    drop(ticket);
    ac.disconnect().unwrap();
    bc.disconnect().unwrap();
    a.poll(&mut ring, 32).unwrap();
    b.poll(&mut ring, 32).unwrap();
    {
        let mut manager = manager_mut(&ag);
        assert!(manager.live.is_empty());
        assert!(manager.outbound[0].retry.after > Instant::now());
        manager.outbound[0].retry.after = Instant::now();
    }
    handshake(&mut ring, &mut a, &mut b, aa, ba);
    // Confirmed idle sessions survive the old confirmation deadline.
    let lost = session(&bg);
    manager_mut(&bg).live[0].confirmation = Some(Instant::now());
    b.poll(&mut ring, 32).unwrap();
    assert!(lost.is_healthy());
    assert!(manager(&bg).live[0].confirmation.is_none());
    let pinned = session(&ag);
    ag.retire(Instant::now());
    assert!(pinned.is_healthy());
    ag.drain.set(Some(Instant::now()));
    a.poll(&mut ring, 32).unwrap();
    assert!(!pinned.is_healthy());
    a.shutdown(&mut ring).unwrap();
    b.shutdown(&mut ring).unwrap();
}
#[test]
fn stale_inbound_replacement_preserves_reverse_session_at_two_qp_capacity() {
    let Some(mut ring) = ring() else { return };
    let aa = address();
    let ba = address();
    let at = rdma::test_transport_config(ring.pool(), 2, 1);
    let bt = rdma::test_transport_config(ring.pool(), 2, 1);
    let ar = negotiation::Rails::new(vec![Some(at.clone())], 1).unwrap();
    let br = negotiation::Rails::new(vec![Some(bt.clone())], 1).unwrap();
    let mut a = activate(&mut ring, prepared(2, aa, ba, true), 0, ar);
    let mut b = activate(&mut ring, prepared(3, ba, aa, true), 0, br);
    warm(&a, aa);
    warm(&b, ba);
    let ag = generation(&a, aa);
    let bg = generation(&b, ba);
    let end = Instant::now() + Duration::from_secs(3);
    loop {
        ring.progress().unwrap();
        a.poll(&mut ring, 64).unwrap();
        b.poll(&mut ring, 64).unwrap();
        confirm(&a, aa);
        confirm(&b, ba);
        if [&ag, &bg].iter().all(|g| {
            let m = manager(g);
            m.live.len() == 2 && m.live.iter().all(|l| l.connection.is_confirmed())
        }) {
            break;
        }
        assert!(Instant::now() < end);
    }
    let get = |g: &Generation, outbound: bool| {
        manager(g)
            .live
            .iter()
            .find(|l| l.outbound.is_some() == outbound)
            .unwrap()
            .connection
            .clone()
    };
    let abandoned = get(&ag, true);
    let stale = get(&bg, false);
    let reverse_a = get(&ag, false);
    let reverse_b = get(&bg, true);
    abandoned.disconnect().unwrap();
    assert!(stale.is_healthy()); // remote QP disappearance has no idle notification
    let end = Instant::now() + Duration::from_secs(5);
    loop {
        ring.progress().unwrap();
        a.poll(&mut ring, 64).unwrap();
        b.poll(&mut ring, 64).unwrap();
        confirm(&a, aa);
        confirm(&b, ba);
        assert!(reverse_a.is_healthy() && reverse_b.is_healthy());
        assert!(at.test_observe().qps <= 2 && bt.test_observe().qps <= 2);
        assert!(manager(&bg).inbound.len() <= 1);
        if !stale.is_healthy()
            && [&ag, &bg].iter().all(|g| {
                let m = manager(g);
                m.live.len() == 2 && m.live.iter().all(|l| l.connection.is_confirmed())
            })
        {
            break;
        }
        assert!(
            Instant::now() < end,
            "authenticated replacement did not reconnect promptly"
        );
    }
    assert!(!stale.is_healthy());
    assert!(!Rc::ptr_eq(&get(&ag, true), &abandoned));
    assert!(!Rc::ptr_eq(&get(&bg, false), &stale));
    a.shutdown(&mut ring).unwrap();
    b.shutdown(&mut ring).unwrap();
}

#[test]
fn sparse_failure_backoff_and_barrier_do_not_activate_staged_policy() {
    let Some(mut ring) = ring() else { return };
    let addr = address();
    let updates = Arc::new(Updates::default());
    updates.subscribe(ring.wake_handle());
    updates.subscribe(ring.wake_handle());
    updates.publish(prepared(2, addr, address(), true)).unwrap();
    let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));
    let mut volumes = Volumes::new(crate::cache::tests::cache(1), updates.clone(), crypto, 0)
        .with_rdma(Some(negotiation::Rails::new(vec![None; 3], 3).unwrap()));
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers.is_empty());
    let staged = volumes.staged.as_ref().unwrap().generations[&addr].clone();
    assert!(!staged.active.get());
    updates.staged(1, 1, true);
    volumes.poll(&mut ring, 16).unwrap();
    updates.activated(1, 1);
    assert!(staged.active.get());
    manager_mut(&staged).trigger(&staged._config.volumes[0].config.peers[0], "/object");
    volumes.poll(&mut ring, 16).unwrap();
    let after = manager(&staged).outbound[0].retry.after;
    for _ in 0..5 {
        manager_mut(&staged).trigger(&staged._config.volumes[0].config.peers[0], "/different");
        volumes.poll(&mut ring, 16).unwrap();
    }
    let manager = manager(&staged);
    assert_eq!(manager.outbound.len(), 1);
    assert_eq!(manager.outbound[0].retry.after, after);
    assert_eq!(manager.rails.total(), 3);
    assert!(manager.live.is_empty());
    drop(manager);
    let (trust, _) = fixture();
    let mut config = staged._config.config.clone();
    config.revision = 2;
    config.fabric.clear();
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    let disabled = volumes.staged.as_ref().unwrap().generations[&addr].clone();
    assert!(disabled.manager.is_none());
    assert!(!disabled.active.get());
    assert!(staged.active.get());
    updates.staged(2, 1, true);
    volumes.poll(&mut ring, 16).unwrap();
    assert!(disabled.active.get());
    assert!(!staged.active.get());
    volumes.shutdown(&mut ring).unwrap();
}
#[test]
fn inbound_membership_shard_cardinality_and_expiry() {
    let Some(mut ring) = ring() else { return };
    let addr = address();
    let mut volumes = activate(
        &mut ring,
        prepared(3, addr, address(), false),
        9,
        negotiation::Rails::new(vec![None], 1).unwrap(),
    );
    let generation = generation(&volumes, addr);
    let mut manager = manager_mut(&generation);
    let mut hint = negotiation::RequestHint {
        node: NodeId::from_bytes(&[2; 32]).unwrap(),
        volume: manager.context.volume(),
        shard: 1000,
        is_finish: false,
    };
    let first = manager.incoming(&hint).unwrap();
    assert!(Rc::ptr_eq(&first, &manager.incoming(&hint).unwrap()));
    hint.node = NodeId::from_bytes(&[8; 32]).unwrap();
    assert!(manager.incoming(&hint).is_err());
    hint.node = NodeId::from_bytes(&[2; 32]).unwrap();
    for shard in 0..MAX_PATHS - 1 {
        hint.shard = shard as u64;
        manager.incoming(&hint).unwrap();
    }
    hint.shard = u64::MAX;
    assert!(manager.incoming(&hint).is_err());
    manager.poll(&generation, &mut ring, 16);
    assert!(manager.inbound.is_empty()); // failed/empty reservations don't accumulate
    drop(manager);
    volumes.shutdown(&mut ring).unwrap();
}
#[test]
fn old_tcp_finish_routes_to_pinned_server_without_stale_install() {
    let Some(mut ring) = ring() else { return };
    let aa = address();
    let ba = address();
    let rails = negotiation::Rails::new(vec![Some(rdma::test_transport(ring.pool()))], 1).unwrap();
    let mut b = activate(&mut ring, prepared(3, ba, aa, false), 7, rails);
    let old = generation(&b, ba);
    let context = Rc::new(
        negotiation::Context::new(Arc::new(prepared(2, aa, ba, true)), "v1", 3, ROUTING).unwrap(),
    );
    let rails = negotiation::Rails::new(vec![Some(rdma::test_transport(ring.pool()))], 1).unwrap();
    let id = NodeId::from_bytes(&[3; 32]).unwrap().to_string();
    let mut client = negotiation::Client::start(
        context,
        rails.clone(),
        &id,
        "/arbitrary",
        NEGOTIATION_TIMEOUT,
    )
    .unwrap();
    // Stop client polling at AwaitReply so no Finish can be sent yet.
    assert!(matches!(
        client.poll(&mut ring, 64).unwrap(),
        Progress::Pending(_)
    ));
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        ring.progress().unwrap();
        // Advance TCP connect/send until responder holds a replied Hello.
        if manager(&old)
            .inbound
            .values()
            .any(|s| s.borrow().reserved() != 0)
        {
            break;
        }
        assert!(matches!(
            client.poll(&mut ring, 1).unwrap(),
            Progress::Pending(_)
        ));
        b.poll(&mut ring, 64).unwrap();
        assert!(Instant::now() < deadline);
    }
    // Poll responder until reply is sent and ownership is in has_pending.
    for _ in 0..100 {
        ring.progress().unwrap();
        b.poll(&mut ring, 64).unwrap();
    }
    let pinned = manager(&old).inbound.values().next().unwrap().clone();
    assert_eq!(pinned.borrow().reserved(), 1);
    let mut next = prepared(3, ba, aa, false);
    // Prepared's immutable eligibility snapshot must also carry revision 2.
    let (trust, _) = fixture();
    let mut config = next.config.clone();
    config.revision = 2;
    let mut trust = trust;
    trust.node = [3; 32];
    next = prepare_snapshot(&trust, config);
    b.updates.publish(next).unwrap();
    b.poll(&mut ring, 64).unwrap();
    let current = generation(&b, ba);
    assert!(!old.active.get() && current.active.get());
    assert!(!Rc::ptr_eq(&old, &current));
    // Drive the real HTTP dispatcher, but defer Manager::poll so its stale
    // admission rejection cannot destroy the QP before we observe Ready and
    // complete confirmation. The server still selects the pinned continuation
    // through VolumeHandler::negotiation exactly as it does in production.
    let mut inbound = None;
    let outbound = loop {
        ring.progress().unwrap();
        b.servers.get_mut(&ba).unwrap().poll(&mut ring, 64).unwrap();
        if inbound.is_none() {
            inbound = pinned.borrow_mut().take_completed(Instant::now());
        }
        negotiation::test_confirmations(&rails);
        negotiation::test_confirmations(&manager(&old).rails);
        match client.poll(&mut ring, 64).unwrap() {
            Progress::Ready(established) => break established,
            Progress::Pending(_) => assert!(Instant::now() < deadline),
        }
    };
    let inbound = inbound.expect("original pinned server must complete Finish/Ready");
    assert!(Arc::ptr_eq(inbound.context.prepared(), &old._config));
    assert!(!Arc::ptr_eq(inbound.context.prepared(), &current._config));
    assert_eq!(inbound.context.shard(), 3);
    assert_eq!(pinned.borrow().reserved(), 0);
    assert!(inbound.connection.is_confirmed());
    // Established proves the client verified both original Ready and ConfirmAck;
    // a timeout, wrong-server dispatch, or failed proof cannot satisfy this test.
    assert!(outbound.connection.is_confirmed());
    assert!(manager_mut(&old).admit(inbound, None, &old).is_err());
    assert!(manager(&old).live.is_empty());
    assert!(manager(&current).live.is_empty());
    drop(outbound);
    b.shutdown(&mut ring).unwrap();
}
#[test]
fn reloads_all_volumes_and_peers_and_failed_bind_preserves_generation() {
    let Some(mut ring) = ring() else { return };
    let (trust, mut config) = fixture();
    let first = address();
    config.volumes[0].listen = first.to_string();
    let updates = Arc::new(Updates::default());
    updates.subscribe(ring.wake_handle());
    let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));
    let mut cache = crate::cache::tests::cache(1);
    cache.set_metrics(ring.metrics().clone());
    let mut volumes = Volumes::new(cache, updates.clone(), crypto.clone(), 0);
    let prepare = |s| prepare_snapshot(&trust, s);
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    let old = volumes.servers[&first].handler().current.clone();
    assert_eq!(old._config.config.revision, 1);
    old.handlers[0]
        .borrow_mut()
        .cache_mut()
        .metrics()
        .request(crate::metrics::Traffic::ClientHttp);
    config.revision = 2;
    config.volumes[0].peers.clear();
    config.volumes[0].topology = Some(proto::Topology {
        routing_algorithm: None,
        epoch: 2,
        slot_count: 2,
        local_slots: vec![0, 1],
        neighbors: vec![],
    });
    config.peers.clear();
    let second = address();
    let mut extra = config.volumes[0].clone();
    extra.id = "second".into();
    extra.listen = second.to_string();
    config.volumes.push(extra);
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert_eq!(volumes.servers.len(), 2);
    assert!(generation(&volumes, first)._config.peers.is_empty());
    assert_eq!(old._config.peers.len(), 1);
    for server in volumes.servers.values() {
        for handler in &server.handler().current.handlers {
            handler
                .borrow_mut()
                .cache_mut()
                .metrics()
                .request(crate::metrics::Traffic::ClientHttp);
        }
    }
    assert_eq!(
        ring.metrics().values()[0],
        3,
        "old and both new volume handlers retain worker counters"
    );
    let occupied = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    config.revision = 3;
    config.volumes[1].listen = occupied.local_addr().unwrap().to_string();
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert_eq!(updates.decision(3), None);
    assert_eq!(generation(&volumes, first)._config.config.revision, 2);
    assert!(volumes.servers.contains_key(&second));
    config.revision = 4;
    config.volumes.clear();
    updates.publish(prepare(config)).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers.is_empty());
    drop(old);
    volumes.shutdown(&mut ring).unwrap();
    drop(volumes);
    Arc::try_unwrap(crypto).ok().unwrap().shutdown().unwrap();
}
