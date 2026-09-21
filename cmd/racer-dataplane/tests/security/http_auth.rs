// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
pub(crate) mod failure_tests {
    use super::failure::*;
    use crate::{cache, http_client as client};
    use cache::{http_metadata::headers, peer_wire::hex};
    use client::attempt::{PeerFailure, PeerReason};
    use std::{io, sync::Arc};

    // Independent expected outcomes shared by framing and owner-probe corpora.
    pub(crate) const SEMANTICS: [(PeerReason, u16, bool); 10] = [
        (PeerReason::OwnerUnavailable, 503, false),
        (PeerReason::Busy, 503, false),
        (PeerReason::Unavailable, 503, false),
        (PeerReason::Protocol, 502, false),
        (PeerReason::Service, 502, false),
        (PeerReason::Deadline, 504, false),
        (PeerReason::Cancelled, 502, false),
        (PeerReason::NotFound, 404, true),
        (PeerReason::Gone, 410, true),
        (PeerReason::Precondition, 412, true),
    ];
    pub(crate) fn route(final_hop: bool) -> AttemptRoute {
        AttemptRoute {
            cursor: crate::routing::Cursor::decode(&[0; crate::routing::Cursor::LEN]).unwrap(),
            candidate: 3,
            endpoint: "127.0.0.1:1".parse().unwrap(),
            final_hop,
            context: "a".repeat(96),
        }
    }
    fn probe(
        owners: &mut client::owner_health::Owners,
        world: &crate::simulation::World,
        route: &AttemptRoute,
    ) -> Option<crate::handlers::Attempt> {
        owners
            .acquire(route.cursor.identity, route.candidate)
            .unwrap()
            .failure(world.now());
        world.advance(std::time::Duration::from_secs(1));
        Some(crate::handlers::Attempt {
            route: route.clone(),
            owner: Some(
                owners
                    .acquire(route.cursor.identity, route.candidate)
                    .unwrap(),
            ),
        })
    }
    #[test]
    fn owner_semantic_probe_policy_context_and_stale_generation() {
        use crate::handlers::Attempt;
        use client::owner_health::Owners;
        let world = crate::simulation::World::new(173);
        let _scope = world.enter();
        let route = route(true);
        let identity = route.cursor.identity;
        for (reason, _, reachable) in SEMANTICS {
            let mut owners = Owners::default();
            let stale = owners.acquire(identity, 3).unwrap();
            let mut probe = probe(&mut owners, &world, &route);
            assert!(owners.acquire(identity, 3).is_err());
            let failure = PeerFailure {
                identity,
                candidate: 3,
                reason,
                evidence: None,
            };
            let mut stale = Some(Attempt {
                route: route.clone(),
                owner: Some(stale),
            });
            reported(failure, &mut stale).unwrap_err();
            drop(stale);
            assert!(owners.blocked(identity, 3));
            assert!(owners.needs_probe(identity, 3));
            let error = reported(failure, &mut probe).unwrap_err();
            if reason != PeerReason::OwnerUnavailable {
                assert_eq!(semantic_failure(&error), Some(failure));
            }
            drop(probe);
            assert_eq!(owners.blocked(identity, 3), !reachable);
            assert_eq!(owners.needs_probe(identity, 3), !reachable);
            assert!(!owners.blocked([1; 32], 3));
            assert!(!owners.blocked(identity, 4));
        }
        for (reason, status, _) in SEMANTICS.into_iter().filter(|(_, _, reachable)| *reachable) {
            let failure = PeerFailure {
                identity,
                candidate: 3,
                reason,
                evidence: None,
            };
            let encoded = hex(&failure.encode());
            let wire = format!(
                "X-Racer-Failure: {encoded}\r\nX-Racer-Attempt: {}\r\n",
                route.context
            );
            assert_eq!(error_status(&io::Error::other(failure).into()), status);
            for (wire, length, status) in [
                (
                    wire.replace(&route.context, &"b".repeat(96)),
                    Some(0),
                    status,
                ),
                (wire.replace(&encoded, "00"), Some(0), status),
                (
                    format!("{wire}X-Racer-Failure: {encoded}\r\n"),
                    Some(0),
                    status,
                ),
                (wire.clone(), Some(1), status),
                (wire.clone(), Some(0), 503),
            ] {
                let mut owners = Owners::default();
                let mut probe = probe(&mut owners, &world, &route);
                let result = headers(&wire, |h| validate_peer_report(h, length, status, &route));
                assert!(result.is_err());
                if let Ok(failure) = result {
                    reported(failure, &mut probe).unwrap_err();
                }
                drop(probe);
                assert!(owners.blocked(identity, 3));
                assert!(owners.needs_probe(identity, 3));
            }
            for (foreign_identity, candidate) in [([1; 32], 3), (identity, 4)] {
                let mut owners = Owners::default();
                let mut probe = probe(&mut owners, &world, &route);
                let error = reported(
                    PeerFailure {
                        identity: foreign_identity,
                        candidate,
                        ..failure
                    },
                    &mut probe,
                )
                .unwrap_err();
                assert!(semantic_failure(&error).is_none());
                assert!(probe.as_ref().unwrap().owner.is_some());
                drop(probe);
                assert!(owners.blocked(identity, 3));
                assert!(owners.needs_probe(identity, 3));
            }
        }
    }
    #[test]
    fn local_pressure_classification_survives_adapter_and_fanout_wrappers() {
        use client::attempt::{Cause, Failure, Phase, Transport};
        let route = route(true);
        for (cause, kind, reason, status) in [
            (
                Cause::LocalPressure,
                io::ErrorKind::TimedOut,
                PeerReason::Busy,
                503,
            ),
            (
                Cause::BreakerRejected,
                io::ErrorKind::WouldBlock,
                PeerReason::Busy,
                503,
            ),
            (
                Cause::CallerDeadline,
                io::ErrorKind::TimedOut,
                PeerReason::Deadline,
                504,
            ),
            (
                Cause::ServiceTimeout,
                io::ErrorKind::TimedOut,
                PeerReason::Deadline,
                504,
            ),
            (
                Cause::Cancelled,
                io::ErrorKind::TimedOut,
                PeerReason::Cancelled,
                502,
            ),
            (
                Cause::Connection,
                io::ErrorKind::ConnectionRefused,
                PeerReason::Service,
                502,
            ),
            (
                Cause::Other,
                io::ErrorKind::WouldBlock,
                PeerReason::Service,
                502,
            ),
            (
                Cause::Protocol,
                io::ErrorKind::TimedOut,
                PeerReason::Protocol,
                502,
            ),
        ] {
            for routed in [false, true] {
                for wrapper in 0..4 {
                    let evidence = Failure {
                        endpoint: route.endpoint,
                        transport: Transport::Http,
                        phase: Phase::LocalAdmission,
                        cause,
                        initiated: false,
                        kind,
                        message: "classification regression".into(),
                    };
                    let expected = (&evidence).into();
                    let error = if routed {
                        io::Error::other(AttemptFailure {
                            route: route.clone(),
                            evidence: Some(evidence),
                            reported: false,
                        })
                    } else {
                        io::Error::new(kind, evidence)
                    };
                    let error = cache::Error::from(error);
                    let error = match wrapper {
                        0 => error,
                        1 => cache::Error::Shared(Arc::new(error)),
                        2 => io_error(cache::Error::Shared(Arc::new(error))).into(),
                        _ => io::Error::new(
                            io::ErrorKind::TimedOut,
                            io_error(cache::Error::Shared(Arc::new(error))),
                        )
                        .into(),
                    };
                    assert_eq!(error_status(&error), status, "{error:?}");
                    let report = peer_failure(&error, route.cursor.identity, 3);
                    assert_eq!(report.reason, reason, "{error:?}");
                    assert_eq!(report.evidence, Some(expected));
                    assert_eq!(owner_failure(&error), None);
                    assert!(
                        !error_detail::<AttemptFailure>(&error).is_some_and(|f| f.owner_evidence())
                    );
                    if matches!(
                        cause,
                        Cause::LocalPressure
                            | Cause::BreakerRejected
                            | Cause::CallerDeadline
                            | Cause::Cancelled
                    ) {
                        let breaker =
                            crate::breaker::CircuitBreaker::new(std::time::Duration::from_secs(1));
                        client::Origin::error(breaker.try_acquire().unwrap(), &error, false);
                        assert!(breaker.available());
                    }
                }
            }
        }
        let busy = cache::Error::from(io::Error::from(io::ErrorKind::WouldBlock));
        assert_eq!(error_status(&busy), 503);
        assert_eq!(peer_failure(&busy, [0; 32], 3).reason, PeerReason::Busy);
        assert!(attempt_evidence(&busy).is_none());
        assert_eq!(owner_failure(&busy), None);
        for error in [
            cache::Error::Timeout,
            io::Error::from(io::ErrorKind::TimedOut).into(),
        ] {
            assert_eq!(error_status(&error), 504);
            assert_eq!(
                peer_failure(&error, [0; 32], 3).reason,
                PeerReason::Deadline
            );
        }
    }
    #[test]
    fn owner_reports_require_exact_bounded_request_context() {
        let route = route(false);
        let valid = format!(
            "X-Racer-Owner-Unavailable: 3\r\nX-Racer-Attempt: {}\r\n",
            route.context
        );
        assert!(headers(&valid, |h| validate_owner_report(h, Some(0), &route)).is_ok());
        for wire in [
            valid.replace("Unavailable: 3", "Unavailable: 4"),
            valid.replace(&route.context, &"b".repeat(96)),
            valid.replace("Unavailable: 3", "Unavailable: nope"),
            format!("{valid}X-Racer-Owner-Unavailable: 3\r\n"),
            format!("{valid}X-Racer-Attempt: {}\r\n", route.context),
            "X-Racer-Owner-Unavailable: 3\r\n".into(),
            format!("{valid}Content-Encoding: gzip\r\n"),
        ] {
            assert!(headers(&wire, |h| validate_owner_report(h, Some(0), &route)).is_err());
        }
        for length in [None, Some(1), Some(u64::MAX)] {
            assert!(headers(&valid, |h| validate_owner_report(h, length, &route)).is_err());
        }
        for (reason, status, _) in SEMANTICS {
            let failure = PeerFailure {
                identity: route.cursor.identity,
                candidate: 3,
                reason,
                evidence: None,
            };
            assert_eq!(error_status(&io::Error::other(failure).into()), status);
            let encoded = hex(&failure.encode());
            let wire = format!(
                "X-Racer-Failure: {encoded}\r\nX-Racer-Attempt: {}\r\n",
                route.context
            );
            assert_eq!(
                headers(&wire, |h| validate_peer_report(h, Some(0), status, &route)).unwrap(),
                failure
            );
            for malformed in [
                format!("{wire}X-Racer-Failure: {encoded}\r\n"),
                format!("{wire}Content-Encoding: gzip\r\n"),
                format!("{wire}X-Racer-Owner-Unavailable: 4\r\n"),
                wire.replace(&route.context, &"b".repeat(96)),
                wire.replace(&encoded, "00"),
            ] {
                assert!(
                    headers(&malformed, |h| validate_peer_report(
                        h,
                        Some(0),
                        status,
                        &route
                    ))
                    .is_err()
                );
            }
            assert!(headers(&wire, |h| validate_peer_report(h, Some(0), 200, &route)).is_err());
            assert!(headers(&wire, |h| validate_peer_report(h, Some(1), status, &route)).is_err());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::http::{Header, Span};
    use std::time::{Duration, Instant};

    fn with_headers<T>(values: &[(String, Vec<u8>)], f: impl FnOnce(Headers<'_>) -> T) -> T {
        let mut bytes = Vec::new();
        let mut headers = Vec::new();
        for (name, value) in values {
            let start = bytes.len() as u16;
            bytes.extend_from_slice(name.as_bytes());
            let end = bytes.len() as u16;
            let name = Span { start, end };
            let start = end;
            bytes.extend_from_slice(value);
            headers.push(Header {
                name,
                value: Span {
                    start,
                    end: bytes.len() as u16,
                },
            });
        }
        f(Headers {
            bytes: &bytes,
            headers: &headers,
        })
    }

    fn policies() -> (Policy, Policy) {
        let public = |seed| {
            ed25519_dalek::SigningKey::from_bytes(&[seed; 32])
                .verifying_key()
                .to_bytes()
        };
        let policy = |local, remote| Policy {
            keys: Keys::new(Some([local; 32]), vec![public(remote)]).unwrap(),
            universe: [8; 32],
            node: [local; 32],
            peers: [[remote; 32]].into_iter().collect(),
        };
        (policy(1, 2), policy(2, 1))
    }

    #[test]
    fn independent_keys_bind_request_and_error_response_semantics() {
        let (client, server) = policies();
        let mut request = vec![(
            "X-Racer-Fault".into(),
            b"routing and value descriptor".to_vec(),
        )];
        let pending = client
            .request(server.node, "GET", "/", &mut request)
            .unwrap();
        let incoming = with_headers(&request, |h| server.receive("GET", "/", h)).unwrap();
        for index in 0..request.len() {
            let mut bad = request.clone();
            bad[index].1[0] ^= 1;
            assert!(with_headers(&bad, |h| server.receive("GET", "/", h)).is_err());
        }
        for (method, target) in [("HEAD", "/"), ("GET", "/other")] {
            assert!(with_headers(&request, |h| server.receive(method, target, h)).is_err());
        }
        let mut unsigned = request.clone();
        unsigned.retain(|(n, _)| n != "X-Racer-Signature");
        assert!(with_headers(&unsigned, |h| server.receive("GET", "/", h)).is_err());
        let mut duplicate = request.clone();
        duplicate.push(request[0].clone());
        assert!(with_headers(&duplicate, |h| server.receive("GET", "/", h)).is_err());

        let mut response = vec![
            ("X-Racer-Owner-Failure".into(), b"unavailable".to_vec()),
            ("X-Racer-Crc64".into(), b"0000000000000000".to_vec()),
        ];
        let headers: Vec<_> = response
            .iter()
            .map(|(n, v): &(String, Vec<u8>)| (n.as_str(), v.as_slice()))
            .collect();
        let signature = incoming.response(&server.keys, 503, 0, &headers).unwrap();
        response.push(("X-Racer-Signature".into(), signature.into_bytes()));
        with_headers(&response, |h| pending.verify(&client.keys, 503, 0, h)).unwrap();
        for index in 0..response.len() {
            let mut bad = response.clone();
            bad[index].1[0] ^= 1;
            assert!(with_headers(&bad, |h| pending.verify(&client.keys, 503, 0, h)).is_err());
        }
        for (status, len) in [(200, 0), (503, 1)] {
            assert!(
                with_headers(&response, |h| pending.verify(&client.keys, status, len, h)).is_err()
            );
        }
        let another = client
            .request(server.node, "GET", "/", &mut vec![])
            .unwrap();
        assert!(with_headers(&response, |h| another.verify(&client.keys, 503, 0, h)).is_err());
        response.pop();
        assert!(with_headers(&response, |h| pending.verify(&client.keys, 503, 0, h)).is_err());
    }

    #[test]
    fn missing_keys_never_disable_request_or_response_authentication() {
        let (client, server) = policies();
        let mut request = vec![];
        let pending = client
            .request(server.node, "GET", "/", &mut request)
            .unwrap();
        let incoming = with_headers(&request, |h| server.receive("GET", "/", h)).unwrap();
        let signature = incoming.response(&server.keys, 200, 0, &[]).unwrap();
        let response = vec![("X-Racer-Signature".into(), signature.into_bytes())];
        for keys in [Keys::default(), Keys::new(Some([2; 32]), vec![]).unwrap()] {
            let receiver = Policy {
                keys,
                ..server.clone()
            };
            assert!(with_headers(&[], |h| receiver.receive("GET", "/", h)).is_err());
            assert!(with_headers(&request, |h| receiver.receive("GET", "/", h)).is_err());
            assert!(with_headers(&[], |h| pending.verify(&receiver.keys, 200, 0, h)).is_err());
            assert!(
                with_headers(&response, |h| pending.verify(&receiver.keys, 200, 0, h)).is_err()
            );
        }
        let sender = Policy {
            keys: Keys::new(
                None,
                vec![
                    ed25519_dalek::SigningKey::from_bytes(&[2; 32])
                        .verifying_key()
                        .to_bytes(),
                ],
            )
            .unwrap(),
            ..client
        };
        assert!(
            sender
                .request(server.node, "GET", "/", &mut vec![])
                .is_err()
        );
        assert!(incoming.response(&sender.keys, 200, 0, &[]).is_err());
    }

    #[test]
    fn replay_window_is_bounded() {
        let (client, server) = policies();
        let mut request = vec![];
        let pending = client
            .request(server.node, "GET", "/", &mut request)
            .unwrap();
        let incoming = with_headers(&request, |h| server.receive("GET", "/", h)).unwrap();
        let signature = incoming.response(&server.keys, 200, 0, &[]).unwrap();
        with_headers(
            &[("X-Racer-Signature".into(), signature.into_bytes())],
            |h| pending.verify(&client.keys, 200, 0, h),
        )
        .unwrap();
        let ledger = ReplayLedger::new(replay::Config {
            capacity: 16,
            shards: 1,
        })
        .unwrap();
        let now = Instant::now();
        ledger.accept(incoming.nonce, now).unwrap();
        assert!(
            ledger
                .accept(incoming.nonce, now + Duration::from_secs(120))
                .is_err()
        );
        ledger
            .accept([9; 32], now + Duration::from_secs(121))
            .unwrap();
        ledger
            .accept(incoming.nonce, now + Duration::from_secs(121))
            .unwrap();
        let time = request
            .iter_mut()
            .find(|(n, _)| n == "X-Racer-Time")
            .unwrap();
        time.1 = timestamp()
            .unwrap()
            .saturating_add(CLOCK_WINDOW + 1)
            .to_string()
            .into_bytes();
        assert!(with_headers(&request, |h| server.receive("GET", "/", h)).is_err());
    }
}

#[cfg(test)]
pub(crate) mod attribution {
    use crate::{
        runtime::tests::dst::*,
        simulation::{Phase, World},
    };
    use std::time::Duration;

    pub(crate) fn assert_transport(
        events: &[crate::simulation::Event],
        target: &str,
        source: usize,
        destination: usize,
        rdma: bool,
    ) {
        let endpoint = format!("127.0.0.1:{}", 10000 + destination);
        assert!(
            events.iter().any(|e| e.target == target
                && e.node == Some(source)
                && e.kind
                    == if rdma {
                        "rdma-deliver"
                    } else {
                        "transport-http"
                    }
                && e.detail.contains(&endpoint)),
            "missing actual {} edge {source}->{destination}",
            if rdma { "RDMA" } else { "HTTP" }
        );
    }

    // Migration ledger: these are the retained legacy scripts, not coverage
    // inferred from the cluster harness. Shared Cluster drives identical
    // request paths, gates and independent expected bytes/statuses.
    // BackendScenario: seed 173, 3 modes x 3 phases; seed 109 refused backend.
    // ProbeScenario: seeds 163/167, HTTP/RDMA x owner 404/410/412 or relay
    // 200/404/410/412, including actual failure, exclusive probe and cooldown.
    // Owner refusal/timeouts: seed 83, HTTP/mixed x connect/headers/body;
    // bounded candidates: seed 103, limits 1/2/3 and suppressed 1/3.
    // Pressure: seed 127, HTTP/mixed/RDMA x tasks/pool/endpoint, <800ms;
    // cancellation: seed 139, four phases, survivor <2s and healthy reuse.
    // Renewal: seed 157, three HTTP fallbacks then automatic RDMA upgrade;
    // RDMA payload negative: seed 107, exact healthy QPs and >=3 reuse READs.
    // Authenticated payload below retains real TCP, payload-only corruption,
    // exactly one payload READ, breaker isolation and a different reconnected QP.

    #[test]
    fn dst_step7_http_phase_cancellation_replay() {
        fn run(phase: Phase) -> [u8; 32] {
            let world = World::new(139);
            let _scope = world.enter();
            world.short_transfers(7); // split the 48-byte peer metadata body
            let mut s = Cluster::with_pool(world.clone(), false, false, None, 6);
            let target = s.target(3, "cancel-phase");
            let gate = s.gate((1, 3), &target, phase, None, false);
            let mut cancelled = cold_head(&s, 0, &target);
            s.pending_heads(&mut [(0, &mut cancelled)], 2000, |w| w.hits(gate) > 0);
            assert!(world.hits(gate) > 0, "phase injection must fire: {phase:?}");
            let mut survivor = cold_head(&s, 0, &target);
            s.pending_heads(&mut [(0, &mut survivor)], 10, |_| false);
            cancelled.cancel(s.ring(0)).unwrap();
            world.release(gate);
            let start = world.tick();
            assert_eq!(finish_head(&mut s, 0, &mut survivor), 200);
            assert!(
                world.tick() - start < 2000,
                "{phase:?}: {}ms",
                world.tick() - start
            );
            s.absent(&target, &["candidate", "http-timeout"]);
            let reuse = s.target(3, "reuse-after-phase-cancel");
            assert_eq!(s.get(0, &reuse, &[]), (200, b"abc".to_vec()));
            drop(survivor);
            clean_repro(s, &world)
        }
        for phase in [
            Phase::Connect,
            Phase::Request,
            Phase::Headers,
            Phase::PartialBody,
        ] {
            assert_eq!(run(phase), run(phase));
        }
    }
    #[test]
    fn topology_owner_failure_agrees_on_ring_successor_but_relay_failure_does_not() {
        let Some(mut c) = crate::runtime::tests::Cluster::new() else {
            return;
        };
        c.remove(7);
        for client in [0, 2] {
            let target = c.target(7, &format!("failed-owner-client-{client}"));
            assert_eq!(c.get(client, &target), (200, b"abc".to_vec()));
        }
        let hits = c.hits.lock().unwrap().clone();
        assert_eq!(hits.len(), 4);
        assert!(
            hits.iter().all(|(node, _)| *node == 0),
            "both clients must choose successor 0"
        );
        c.remove(1);
        let target = c.target(7, "failed-relay");
        assert_eq!(c.get(4, &target).0, 502);
        assert_eq!(
            c.hits.lock().unwrap().len(),
            4,
            "relay failure cannot authorize a backend"
        );
    }

    #[test]
    fn dst_window_renewal_http_service_and_automatic_upgrade() {
        fn run() -> [u8; 32] {
            let world = World::new(157);
            let _scope = world.enter();
            let mut s = Cluster::with_connections(world.clone(), true, true);
            let sessions = s.warm_edges(&[(0, 1), (1, 3)]);
            sessions[1].test_window_renewal(true);
            for n in 0..3 {
                let target = s.target(3, &format!("window-renewal-http-{n}"));
                assert_eq!(s.get(0, &target, &[]), (200, b"abc".to_vec()));
                assert!(world.events().iter().any(|e| e.target == target
                    && e.kind == "transport-http"
                    && e.node == Some(1)));
                s.absent(&target, &["candidate", "http-timeout", "rdma-negative"]);
                s.origin_only(&target, 3);
                assert!(sessions[0].is_healthy());
            }
            sessions[1].test_window_renewal(false);
            s.turns(1500);
            let target = s.target(3, "window-renewal-upgraded");
            assert_eq!(s.get(0, &target, &[]), (200, b"abc".to_vec()));
            assert!(
                world
                    .events()
                    .iter()
                    .any(|e| e.target == target && e.kind == "transport-rdma" && e.node == Some(1))
            );
            s.absent(&target, &["candidate", "http-timeout"]);
            s.manager_bounds();
            drop(sessions);
            clean_repro(s, &world)
        }
        assert_eq!(run(), run());
    }
    #[test]
    fn dst_step7_pressure_acceptance() {
        fn run(mode: &str, pressure: &str) -> [u8; 32] {
            let world = World::new(127);
            let _scope = world.enter();
            let mut s = Cluster::with_pool(world.clone(), mode != "http", true, None, 6);
            let sessions = match mode {
                "rdma" => s.warm_edges(&[(0, 1), (1, 3)]),
                "mixed" => s.warm_edges(&[(0, 1)]),
                _ => Vec::new(),
            };
            let first = s.target(3, "pressure-holder");
            let second = s.target(3, "pressure-rejected");
            let gate = world.gate(crate::simulation::Gate::new(
                3,
                "127.0.0.1:11003".parse().unwrap(),
                &first,
                Phase::Request,
                None,
            ));
            if pressure == "tasks" {
                // Typed metadata uses cache admission even with live RDMA QPs.
                s.cache(1)
                    .set_limits(crate::cache::Limits {
                        active_faults: 1,
                        internal_reserve: 0,
                        resource_retries: 2,
                    })
                    .unwrap();
            } else if pressure == "endpoint" {
                s.handler(1).set_resource_limits(64, 1).unwrap();
            }
            let mut holder = cold_head(&s, 0, &first);
            s.pending_heads(&mut [(0, &mut holder)], 500, |w| w.hits(gate) > 0);
            assert!(world.hits(gate) > 0, "holder injection fired");
            let mut held = Vec::new();
            if pressure == "pool" {
                while let Ok(slot) = s.ring(1).pool().private_fill() {
                    held.push(slot);
                }
                assert!(!held.is_empty());
            }
            let started = world.tick();
            let mut rejected = cold_head(&s, 0, &second);
            assert_eq!(
                finish_head(&mut s, 0, &mut rejected),
                if pressure == "pool" { 200 } else { 503 },
                "{mode}/{pressure}"
            );
            assert!(world.tick() - started < 800, "bounded local overload");
            assert_eq!(
                s.hits.borrow().iter().any(|(_, t)| t == &second),
                pressure == "pool"
            );
            s.absent(&second, &["candidate", "http-timeout"]);
            assert_transport(&world.events(), &second, 0, 1, false);
            assert!(sessions.iter().all(|c| c.is_healthy()));
            drop(held);
            world.release(gate);
            assert_eq!(finish_head(&mut s, 0, &mut holder), 200);
            let reuse = s.target(3, "healthy-relay-after-pressure");
            assert_eq!(s.get(0, &reuse, &[]), (200, b"abc".to_vec()));
            assert!(sessions.iter().all(|c| c.is_healthy()));
            drop((holder, rejected, sessions));
            clean_repro(s, &world)
        }
        for mode in ["http", "mixed", "rdma"] {
            for pressure in ["tasks", "pool", "endpoint"] {
                assert_eq!(run(mode, pressure), run(mode, pressure));
            }
        }
    }
    #[test]
    fn dst_shared_relay_negative_all_rdma_successor_and_reuse() {
        fn run() -> [u8; 32] {
            let world = World::new(107);
            let _scope = world.enter();
            let mut s = Cluster::with_connections(world.clone(), true, true);
            let sessions = s.warm_edges(&[(0, 1), (1, 3), (1, 2), (2, 4)]);
            let target = s.target(3, "rdma-owner-refusal");
            assert_route(&s, &target, 0, &[0, 1, 3]);
            assert_route(&s, &target, 1, &[0, 1, 2, 4]);
            assert_eq!(s.head(0, &target), 200); // fault the payload over RDMA
            let start_events = world.events().len();
            let refused = s.gate(
                (1, 3),
                &target,
                Phase::RdmaRequest,
                Some(libc::ECONNREFUSED),
                false,
            );
            let recovery = s.gate(
                (1, 3),
                &target,
                Phase::Headers, // metadata left a reusable HTTP connection
                Some(libc::ECONNREFUSED),
                true,
            );
            let started = world.tick();
            assert_eq!(s.get(0, &target, &[]), (200, b"abc".to_vec()));
            assert!(world.tick() - started < 3_000);
            assert!(world.hits(refused) > 0 && world.hits(recovery) > 0);
            let events = world.events()[start_events..].to_vec();
            for (a, b) in [(0, 1), (1, 2), (2, 4)] {
                assert_transport(&events, &target, a, b, true);
            }
            assert!(
                events
                    .iter()
                    .any(|e| e.target == target && e.kind == "rdma-negative" && e.node == Some(1))
            );
            assert!(events.iter().any(|e| e.target == target
                && e.kind == "candidate"
                && e.node == Some(0)
                && e.detail == "owner=3 next=4 attempt=1"));
            assert!(!events.iter().any(|e| e.target == target
                && e.kind == "transport-http"
                && !(e.node == Some(1) && e.detail.contains("10003"))));
            for i in [0, 2, 3] {
                assert!(
                    sessions[i].is_healthy(),
                    "healthy edge must retain exact QP"
                );
            }
            assert!(s.hits.borrow().iter().any(|(n, t)| *n == 4 && t == &target));
            let reuse = s.target(4, "rdma-reuse-after-negative");
            let reads = s.reads;
            assert_eq!(s.get(0, &reuse, &[]), (200, b"abc".to_vec()));
            assert!(s.reads >= reads + 3, "payload READs on all three edges");
            for (a, b) in [(0, 1), (1, 2), (2, 4)] {
                assert_transport(&world.events(), &reuse, a, b, true);
            }
            for i in [0, 2, 3] {
                assert!(sessions[i].is_healthy());
            }
            world.release(refused);
            world.release(recovery);
            drop(sessions);
            clean_repro(s, &world)
        }
        assert_eq!(run(), run());
    }

    struct BackendScenario {
        seed: u64,
        label: &'static str,
        reuse: &'static str,
        phase: Phase,
        status: u16,
        reason: &'static str,
        cause: &'static str,
        errno: Option<i32>,
    }
    impl BackendScenario {
        fn run(&self, mode: &str) -> [u8; 32] {
            let world = World::new(self.seed);
            let _scope = world.enter();
            let mut s = Cluster::with_connections(world.clone(), mode == "rdma", true);
            let sessions = s.warm_transport(mode == "rdma", &[(0, 1), (1, 3)]);
            let ingress = if mode == "direct" { 3 } else { 0 };
            let target = s.target(3, self.label);
            let phase = self.phase;
            let pressure = matches!(phase, Phase::ConnectAdmission | Phase::Registration);
            let gate = world.gate(crate::simulation::Gate::new(
                3,
                "127.0.0.1:11003".parse().unwrap(),
                &target,
                phase,
                self.errno
                    .or_else(|| (mode == "rdma" && !pressure).then_some(libc::ETIMEDOUT)),
            ));
            let mut request = cold_head(&s, ingress, &target);
            let started = world.tick();
            assert_eq!(
                finish_head(&mut s, ingress, &mut request),
                self.status,
                "{mode}/{phase:?}"
            );
            if pressure {
                assert!(world.tick() - started < 800, "bounded backend admission");
                assert!(s.hits.borrow().iter().all(|(_, t)| t != &target));
            }
            assert!(world.hits(gate) > 0);
            let events = world.events();
            s.absent(&target, &["candidate"]);
            if self.errno.is_some() {
                assert!(s.hits.borrow().iter().all(|(_, t)| t != &target));
            }
            for n in 0..8 {
                assert!(!s.handler(n).test_has_owner_evidence());
            }
            if mode != "direct" {
                assert!(events.iter().any(|e| e.target == target
                    && e.node == Some(0)
                    && e.kind == "classify"
                    && e.detail.contains(self.reason)
                    && e.detail.contains(self.cause)
                    && e.detail.contains("11003")));
                if self.errno.is_some() {
                    assert!(events.iter().any(|e| e.target == target
                        && e.node == Some(0)
                        && e.kind == "classify"
                        && e.detail.contains("transport_failure=false")
                        && e.detail.contains("11003")
                        && e.detail.contains(self.cause)));
                }
                for (a, b) in [(0, 1), (1, 3)] {
                    assert_transport(&events, &target, a, b, false);
                }
            }
            s.absent(&target, &["transport-rdma"]);
            assert!(
                !events
                    .iter()
                    .any(|e| e.kind == "breaker-error" && (pressure || e.node != Some(3)))
            );
            assert!(sessions.iter().all(|c| c.is_healthy()), "{mode}/{phase:?}");
            world.release(gate);
            if !pressure {
                world.advance(Duration::from_secs(1));
            }
            let reuse = s.target(3, self.reuse);
            assert_eq!(s.get(ingress, &reuse, &[]), (200, b"abc".to_vec()));
            s.origin_only(&reuse, 3);
            assert!(sessions.iter().all(|c| c.is_healthy()));
            if mode != "direct" {
                for (a, b) in [(0, 1), (1, 3)] {
                    assert_transport(&world.events(), &reuse, a, b, mode == "rdma");
                }
            }
            drop((request, sessions));
            clean_repro(s, &world)
        }
    }
    #[test]
    fn dst_local_backend_pressure_and_deadline_classification() {
        for mode in ["direct", "http", "rdma"] {
            for (phase, status, reason, cause) in [
                (
                    Phase::ConnectAdmission,
                    503,
                    "reason: Busy",
                    "LocalPressure",
                ),
                (Phase::Registration, 503, "reason: Busy", "LocalPressure"),
                (Phase::Headers, 504, "reason: Deadline", "ServiceTimeout"),
            ] {
                let case = BackendScenario {
                    seed: 173,
                    label: "backend-admission-classification",
                    reuse: "same-relay-after-backend-pressure",
                    phase,
                    status,
                    reason,
                    cause,
                    errno: None,
                };
                assert_eq!(case.run(mode), case.run(mode));
            }
        }
    }
    #[test]
    fn dst_all_rdma_service_negative_retains_attribution_without_owner_fallback() {
        let case = BackendScenario {
            seed: 109,
            label: "rdma-origin-service-failure",
            reuse: "reuse-after-origin-error",
            phase: Phase::Connect,
            status: 502,
            reason: "reason: Service",
            cause: "Connection",
            errno: Some(libc::ECONNREFUSED),
        };
        assert_eq!(case.run("rdma"), case.run("rdma"));
    }
    #[test]
    fn dst_ingress_breaker_rejection_is_busy_without_owner_evidence() {
        fn run() -> [u8; 32] {
            let world = World::new(179);
            let _scope = world.enter();
            let mut s = Cluster::new(world.clone(), false);
            let (_, breaker) = s.handler(0).test_peer_breakers();
            breaker.try_acquire().unwrap().failure();
            let target = s.target(3, "ingress-breaker-rejected");
            let mut request = cold_head(&s, 0, &target);
            assert_eq!(finish_head(&mut s, 0, &mut request), 503);
            assert!(world.events().iter().any(|e| e.target == target
                && e.kind == "classify"
                && e.detail.contains("BreakerRejected")));
            s.absent(&target, &["candidate"]);
            assert!(!s.handler(0).test_has_owner_evidence());
            assert!(s.hits.borrow().iter().all(|(_, t)| t != &target));
            world.advance(Duration::from_secs(1));
            let reuse = s.target(3, "ingress-breaker-reuse");
            assert_eq!(s.get(0, &reuse, &[]), (200, b"abc".to_vec()));
            s.origin_only(&reuse, 3);
            drop(request);
            clean_repro(s, &world)
        }
        assert_eq!(run(), run());
    }

    fn owner_failure_repro(phase: Phase, timeout: bool, mixed: bool) -> [u8; 32] {
        let world = World::new(83);
        let _scope = world.enter();
        let mut s = Cluster::new(world.clone(), mixed);
        let sessions = s.warm_transport(mixed, &[(0, 1)]);
        if mixed {
            let warm = s.target(1, "confirm-relay-session");
            assert_eq!(s.get(0, &warm, &[]), (200, b"abc".to_vec()));
            assert_transport(&world.events(), &warm, 0, 1, true);
        }
        world.short_transfers(7); // split the 48-byte peer metadata body
        let target = s.target(3, "shared-relay");
        assert_route(&s, &target, 0, &[0, 1, 3]);
        assert_route(&s, &target, 1, &[0, 1, 2, 4]);
        let gate = s.gate(
            (1, 3),
            &target,
            phase,
            (!timeout).then_some(libc::ECONNREFUSED),
            true,
        );
        let started = world.tick();
        let mut request = cold_head(&s, 0, &target);
        let status = s.poll_head(0, &mut request);
        assert!(world.hits(gate) > 0, "exact final-hop injection must fire");
        world.release(gate);
        trace_target(&world, &target);
        let events = world.events();
        assert_transport(&events, &target, 0, 1, false);
        assert_transport(&events, &target, 1, 3, false);
        s.absent(&target, &["transport-rdma"]);
        if timeout {
            assert!(events.iter().any(|e| e.kind == "http-timeout"
                && e.node == Some(1)
                && e.target == target
                && e.detail
                    == format!("endpoint=127.0.0.1:10003 phase={phase:?} cause=Io(TimedOut)")));
            assert!(events.iter().any(|e| e.kind == "classify"
                && e.node == Some(1)
                && e.target == target
                && e.detail.contains("last_hop=true transport_failure=true")
                && e.detail.contains("ServiceTimeout")));
        }
        assert!(events.iter().any(|e| e.kind == "candidate"
            && e.node == Some(0)
            && e.target == target
            && e.detail == "owner=3 next=4 attempt=1"));
        assert!(
            !events.iter().any(|e| e.kind == "http-rejected"
                && e.node == Some(0)
                && e.target == target
                && e.detail == "endpoint=127.0.0.1:10001"),
            "healthy shared relay must admit the successor retry"
        );
        let (_, http) = s.handler(0).test_peer_breakers();
        assert!(
            http.try_acquire().is_ok(),
            "semantic report must leave healthy relay breaker usable"
        );
        assert!(s.hits.borrow().iter().any(|(n, t)| *n == 4 && t == &target));
        assert_eq!(status, 200);
        assert!(
            world.tick() - started < 20_000,
            "healthy successor must fit the original caller budget"
        );
        assert!(sessions.iter().all(|c| c.is_healthy()));
        drop((request, sessions));
        clean_repro(s, &world)
    }
    #[test]
    fn dst_bounded_candidates_and_known_suppressed_skip() {
        fn scenario(limit: u32, suppressed: bool) -> [u8; 32] {
            let world = World::new(103);
            let _scope = world.enter();
            let mut s = Cluster::new(world.clone(), false);
            s.handler(0).set_attempt_policy(limit).unwrap();
            let target = s.target(3, "bounded-owners");
            if suppressed {
                s.handler(0).test_suppress_owner(3);
            }
            let mut gates = Vec::new();
            if !suppressed {
                for edge in [(1, 3), (2, 4), (2, 5), (3, 6)] {
                    gates.push(s.gate(
                        edge,
                        &target,
                        Phase::Connect,
                        Some(libc::ECONNREFUSED),
                        true,
                    ));
                }
            }
            let mut request = cold_head(&s, 0, &target);
            let status = s.poll_head(0, &mut request);
            let events = world.events();
            if suppressed {
                assert_eq!(status, if limit == 1 { 503 } else { 200 });
                assert!(!events.iter().any(|e| e.target == target
                    && e.kind == "transport-http"
                    && e.detail.contains("10003")));
                assert_eq!(
                    s.hits.borrow().iter().any(|(n, t)| *n == 4 && t == &target),
                    limit > 1
                );
            } else {
                assert_eq!(status, 503);
                let advances = events
                    .iter()
                    .filter(|e| e.target == target && e.node == Some(0) && e.kind == "candidate")
                    .count();
                assert_eq!(advances, limit as usize - 1);
                assert!(s.hits.borrow().iter().all(|(_, t)| t != &target));
                for (i, gate) in gates.iter().enumerate() {
                    assert_eq!(
                        world.hits(*gate) > 0,
                        i < limit as usize,
                        "limit={limit} gate={i}"
                    );
                }
            }
            for gate in gates {
                if world.hits(gate) > 0 {
                    world.release(gate);
                }
            }
            drop(request);
            clean_repro(s, &world)
        }
        for (limit, suppressed) in [(1, false), (2, false), (3, false), (1, true), (3, true)] {
            assert_eq!(scenario(limit, suppressed), scenario(limit, suppressed));
        }
    }
    #[test]
    fn dst_shared_relay_breaker_http() {
        assert_eq!(
            owner_failure_repro(Phase::Connect, false, false),
            owner_failure_repro(Phase::Connect, false, false)
        );
    }
    #[test]
    fn dst_shared_relay_breaker_mixed() {
        assert_eq!(
            owner_failure_repro(Phase::Connect, false, true),
            owner_failure_repro(Phase::Connect, false, true)
        );
    }
    fn final_hop_timeouts(mixed: bool) {
        for phase in [Phase::Connect, Phase::Headers, Phase::PartialBody] {
            assert_eq!(
                owner_failure_repro(phase, true, mixed),
                owner_failure_repro(phase, true, mixed)
            );
        }
    }
    #[test]
    fn dst_final_hop_timeout_phases_http() {
        final_hop_timeouts(false);
    }
    #[test]
    fn dst_final_hop_timeout_phases_mixed() {
        final_hop_timeouts(true);
    }

    struct ProbeScenario {
        final_hop: bool,
        status: u16,
        rdma: bool,
    }
    impl ProbeScenario {
        fn run(&self) -> [u8; 32] {
            let (rdma, status) = (self.rdma, self.status);
            let world = World::new(if self.final_hop { 163 } else { 167 });
            let _scope = world.enter();
            let mut s = Cluster::with_connections(world.clone(), rdma, rdma);
            let ingress = usize::from(self.final_hop);
            let edges: &[(usize, usize)] = if self.final_hop {
                &[(1, 3)]
            } else {
                &[(0, 1), (1, 3), (1, 2), (2, 4)]
            };
            let label = if self.final_hop {
                format!("semantic-{status}")
            } else {
                format!("semantic-{status}-relay")
            };
            let semantic = s.target(3, &label);
            let mut sessions = s.warm_transport(rdma, edges);
            if self.final_hop {
                let failed = s.target(3, "owner-probe-failure");
                assert_eq!(s.head(1, &failed), 200); // pin metadata before payload failure
                let rdma_gate = rdma.then(|| {
                    s.gate(
                        (1, 3),
                        &failed,
                        Phase::RdmaRequest,
                        Some(libc::ECONNREFUSED),
                        false,
                    )
                });
                let gate = s.gate(
                    (1, 3),
                    &failed,
                    Phase::Headers, // metadata left a reusable HTTP connection
                    Some(libc::ECONNREFUSED),
                    true,
                );
                assert_eq!(s.get(1, &failed, &[]), (200, b"abc".to_vec()));
                assert!(world.hits(gate) > 0);
                if let Some(gate) = rdma_gate {
                    assert!(world.hits(gate) > 0);
                    world.release(gate);
                }
                drop(sessions);
                assert!(s.hits.borrow().iter().any(|(n, t)| *n == 4 && t == &failed));
                assert!(world.events().iter().any(|e| e.target == failed
                    && e.node == Some(1)
                    && e.kind == "candidate"
                    && e.detail == "owner=3 next=4 attempt=1"));
                world.release(gate);
                world.advance(Duration::from_secs(1));
                sessions = s.warm_transport(rdma, edges);
                assert!(
                    s.handler(ingress).test_has_owner_evidence(),
                    "the failed owner must still need a semantic recovery probe"
                );
            } else {
                if status == 200 {
                    assert_eq!(s.get(1, &semantic, &[]), (200, b"abc".to_vec()));
                }
                s.handler(0).test_suppress_owner(3);
                world.advance(Duration::from_secs(1));
            }
            let start = world.tick();
            let hits = s.hits.borrow().len();
            let request = s.probe_response(ingress, &semantic, status);
            let replied = world.tick();
            if self.final_hop {
                // A terminal owner reply must close the probe immediately.
                // Payload completion is not the owner-health decision: READ/
                // release progress can outlast the one-second cooldown.
                assert!(
                    !s.handler(ingress).test_has_owner_evidence(),
                    "terminal owner semantics must recover health before reuse"
                );
                assert!(
                    s.hits
                        .borrow()
                        .iter()
                        .any(|(n, t)| *n == 3 && t == &semantic)
                );
                s.absent(&semantic, &["transport-rdma"]);
            } else if status == 200 {
                assert_eq!(s.hits.borrow().len(), hits, "relay must answer from cache");
            }
            let future = s.target(
                3,
                if self.final_hop {
                    "owner-after-semantic-probe"
                } else {
                    "owner-after-intermediate-probe"
                },
            );
            if !self.final_hop {
                assert!(world.tick() - start < 1_000);
            }
            let issued = world.tick();
            s.probe_reuse(ingress, &future, if self.final_hop { 3 } else { 4 });
            if self.final_hop {
                // No clock advance between the reply and this fresh key. Check
                // the actual ingress decision, not the later GET completion,
                // so an expired cooldown cannot masquerade as probe recovery.
                assert!(world.events().iter().any(|e| e.target == future
                    && e.node == Some(ingress)
                    && e.kind == "route"
                    && e.tick >= issued
                    && e.tick - replied < 1_000
                    && e.detail == "source=1 owner=3 attempt=0 position=0 origin=true"));
                assert_transport(&world.events(), &future, 1, 3, rdma);
                s.absent(&semantic, &["candidate"]);
                s.absent(&future, &["candidate"]);
            } else {
                assert!(world.events().iter().any(|e| e.target == future
                    && e.node == Some(0)
                    && e.kind == "route"
                    && e.tick - issued < 1_000
                    && e.detail.contains("attempt=1")));
            }
            assert!(sessions.iter().all(|c| c.is_healthy()));
            drop((request, sessions));
            clean_repro(s, &world)
        }
    }
    fn probe_corpus(final_hop: bool, statuses: &[u16]) {
        for rdma in [false, true] {
            for &status in statuses {
                let case = ProbeScenario {
                    final_hop,
                    status,
                    rdma,
                };
                assert_eq!(case.run(), case.run());
            }
        }
    }
    #[test]
    fn dst_owner_semantic_probe_recovery_http_and_rdma() {
        probe_corpus(true, &[404, 410, 412]);
    }
    #[test]
    fn dst_owner_probe_intermediate_replies_do_not_recover_http_and_rdma() {
        probe_corpus(false, &[200, 404, 410, 412]);
    }
}

#[cfg(test)]
mod authenticated_payload {
    use crate::{
        buffers::Key,
        control::tests::{fixture, prepare_snapshot, ring, runtime_pair},
        http::Progress,
        http_client as client, http_server as http, negotiation, rdma,
        runtime::tests::*,
        uring,
    };
    use std::{
        io::{self, Read, Write},
        num::NonZeroU32,
        rc::Rc,
        sync::{
            Arc,
            atomic::{AtomicBool, AtomicUsize, Ordering},
        },
        time::{Duration, Instant},
    };

    #[test]
    fn negotiated_plaintext_copy_and_corruption_recover_over_http() {
        let Some(mut ring) = ring() else { return };
        let reserve = || {
            http::Listener::bind("127.0.0.1:0".parse().unwrap(), NonZeroU32::new(8).unwrap())
                .unwrap()
        };
        let a_listener = reserve();
        let b_listener = reserve();
        let aa = a_listener.local_addr().unwrap();
        let ba = b_listener.local_addr().unwrap();
        let backend = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let backend_addr = backend.local_addr().unwrap();
        backend.set_nonblocking(true).unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let stopped = stop.clone();
        let hits = Arc::new(AtomicUsize::new(0));
        let backend_hits = hits.clone();
        let backend_thread = std::thread::spawn(move || {
            let end = Instant::now() + Duration::from_secs(15);
            while !stopped.load(Ordering::Relaxed) && Instant::now() < end {
                let (mut stream, _) = match backend.accept() {
                    Ok(pair) => pair,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        std::thread::yield_now();
                        continue;
                    }
                    Err(e) => panic!("{e}"),
                };
                stream
                    .set_read_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                backend_hits.fetch_add(1, Ordering::Relaxed);
                let mut request = Vec::new();
                while !request.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    stream.read_exact(&mut byte).unwrap();
                    request.push(byte[0]);
                }
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"abc")).unwrap();
                if request.starts_with(b"GET ") {
                    stream.write_all(b"abc").unwrap();
                }
            }
        });
        let prepare = |node, listen, remote, outbound| {
            let p = runtime_pair(node, listen, remote, outbound);
            let (mut trust, _) = fixture();
            trust.node = [node; 32];
            let mut config = p.config.clone();
            config.volumes[0].origin_address = backend_addr.to_string();
            prepare_snapshot(&trust, config)
        };
        let mut remote_ring =
            uring::Ring::http_test_ring(crate::buffers::io_test_pool(8), uring::Config::default())
                .unwrap();
        let ar = negotiation::Rails::new(vec![Some(rdma::test_transport(ring.pool()))], 1).unwrap();
        let br = negotiation::Rails::new(vec![Some(rdma::test_transport(remote_ring.pool()))], 1)
            .unwrap();
        let mut a = activate(&mut ring, prepare(2, aa, ba, true), 0, ar);
        let mut b = activate(&mut remote_ring, prepare(3, ba, aa, false), 0, br);
        drop((a_listener, b_listener));
        warm(&a, aa);
        let turn = |a: &mut crate::runtime::Volumes,
                    b: &mut crate::runtime::Volumes,
                    ring: &mut uring::Ring,
                    remote: &mut uring::Ring| {
            ring.progress().unwrap();
            remote.progress().unwrap();
            a.poll(ring, 64).unwrap();
            b.poll(remote, 64).unwrap();
        };
        let end = Instant::now() + Duration::from_secs(5);
        let (mut ac, mut bc) = loop {
            turn(&mut a, &mut b, &mut ring, &mut remote_ring);
            if let (Some(ac), Some(bc)) = (live(&a, aa), live(&b, ba)) {
                break (ac, bc);
            }
            assert!(Instant::now() < end);
        };
        let (rdma_breaker, http_breaker) = peer_breakers(&a, aa);
        for (case, corrupt) in [false, true, false].into_iter().enumerate() {
            if case == 2 {
                let previous = ac.clone();
                bc.disconnect().unwrap();
                let cooldown = Instant::now() + Duration::from_secs(1);
                let end = cooldown + Duration::from_secs(4);
                loop {
                    turn(&mut a, &mut b, &mut ring, &mut remote_ring);
                    if Instant::now() >= cooldown {
                        if let (Some(next_a), Some(next_b)) = (live(&a, aa), live(&b, ba)) {
                            ac = next_a;
                            bc = next_b;
                            break;
                        }
                    }
                    assert!(Instant::now() < end, "automatic reconnect stalled");
                    std::thread::sleep(Duration::from_millis(1));
                }
                assert!(!Rc::ptr_eq(&previous, &ac));
                assert!(!previous.is_healthy());
            }
            let fill = ring.pool().stage(Key::new([210 + case as u8; 32])).unwrap();
            let target = owned_target(&a, aa, &format!("payload-{case}"));
            let end = Instant::now() + Duration::from_secs(5);
            let mut request = client::Connection::new(aa, "localhost")
                .unwrap()
                .get(client::Request::new(&target, &[]).unwrap(), fill, end)
                .unwrap();
            let mut reads = 0;
            loop {
                turn(&mut a, &mut b, &mut ring, &mut remote_ring);
                // Metadata is a small HTTP exchange; the only READ is payload.
                reads += ac.test_pump(&bc, corrupt && reads == 0);
                bc.test_pump(&ac, false);
                if let Progress::Ready(mut response) = request.poll(&mut ring, 64).unwrap() {
                    assert_eq!(response.status(), 200);
                    assert_eq!(response.body(), b"abc");
                    break;
                }
                assert!(
                    Instant::now() < end,
                    "payload transfer stalled, reads={reads}"
                );
                std::thread::yield_now();
            }
            assert_eq!(reads, 1, "only payload traverses RDMA");
            assert!(bc.authenticated_received());
            assert_eq!(
                ac.is_healthy(),
                !corrupt,
                "CRC failure retires the RDMA connection"
            );
            assert!(bc.is_healthy());
            assert_eq!(rdma_breaker.try_acquire().is_err(), corrupt);
            assert!(
                http_breaker.try_acquire().is_ok(),
                "RDMA corruption must not poison HTTP health"
            );
            assert_eq!(
                hits.load(Ordering::Relaxed),
                2 * (case + 1),
                "recovery must use the HTTP peer's cached plaintext, not refault the backend"
            );
        }
        a.shutdown(&mut ring).unwrap();
        b.shutdown(&mut remote_ring).unwrap();
        stop.store(true, Ordering::Relaxed);
        backend_thread.join().unwrap();
    }
}
