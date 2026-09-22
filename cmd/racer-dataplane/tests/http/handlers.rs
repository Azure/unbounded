// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::http_auth::failure_tests::SEMANTICS;
use cache::adapter_fixture::page_request;
use cache::peer_wire::decode_descriptor;
use http::cache_responses::{accept, request};

#[test]
fn receive_reservation_tracks_candidate_and_colocated_route_rank() {
    let world = crate::simulation::World::new(422);
    let _scope = world.enter();
    let addresses: Vec<_> = (0..8)
        .map(|node| format!("127.0.0.1:{}", 10000 + node).parse().unwrap())
        .collect();
    for local in [vec![0], vec![0, 3]] {
        let mut config = crate::control::tests::cluster_config(
            0,
            &addresses,
            "127.0.0.1:11000".parse().unwrap(),
            Some(2),
            "reservation",
        );
        config.volumes[0].topology.as_mut().unwrap().local_slots = local.clone();
        if local.len() > 1 {
            for slot in [6, 7] {
                let peer = crate::peer_identity::NodeId::from_bytes(&[slot as u8 + 10; 32])
                    .unwrap()
                    .to_string();
                config.volumes[0].peers.push(peer.clone());
                config.volumes[0]
                    .topology
                    .as_mut()
                    .unwrap()
                    .neighbors
                    .push(crate::control::proto::SlotPeer { slot, peer });
            }
        }
        let routing =
            Arc::new(crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap());
        let backend = Backend::new("127.0.0.1:11000", "reserve").unwrap();
        let mut handler = Handler::new(cache(&backend, 1), backend);
        handler.upstream.routing = Some(routing.clone());
        let target = (0..1000)
            .map(|i| format!("/reserve-{i}"))
            .find(|t| routing.start(t).owner == 7)
            .unwrap();
        let cursor = routing.start(&target);
        let state = Rc::new(RefCell::new(RouteState {
            target,
            cursor,
            origin: true,
            exhausted: false,
        }));
        handler.upstream.active = Some(state.clone());
        assert_eq!(
            handler.upstream.receive_reserve().unwrap(),
            if local.len() == 1 { 3 } else { 1 }
        );
        // Successor owner 0 is local; the reservation must be recomputed rather
        // than retaining the original owner's rank during candidate fallback.
        state.borrow_mut().cursor.attempt = 1;
        assert_eq!(handler.upstream.receive_reserve().unwrap(), 0);
        state.borrow_mut().cursor.attempt = 2;
        assert_eq!(handler.upstream.receive_reserve().unwrap(), 1);
    }
}

fn peer_policy(node: u8, peer: u8) -> crate::http_auth::Policy {
    let (trust, _) = crate::control::tests::fixture();
    crate::http_auth::Policy {
        keys: trust.keys,
        universe: trust.universe,
        node: [node; 32],
        peers: [[peer; 32]].into(),
    }
}

fn authenticate_provider(provider: &mut Provider) {
    provider.authentication = Some(peer_policy(2, 3));
    provider.selected = Some(hex(&[3; 32]));
}

fn peer_response(request: &str, len: usize, checksum: u64, etag: &str) -> String {
    let policy = peer_policy(3, 2);
    let wire = request
        .split_once("\r\n")
        .unwrap()
        .1
        .strip_suffix("\r\n")
        .unwrap();
    let incoming =
        cache::http_metadata::headers(wire, |headers| policy.receive("GET", "/", headers).unwrap());
    let checksum = format!("{checksum:016x}");
    let signature = incoming
        .response(
            &policy.keys,
            200,
            len as u64,
            &[
                ("X-Racer-Crc64", checksum.as_bytes()),
                ("ETag", etag.as_bytes()),
            ],
        )
        .unwrap();
    format!(
        "HTTP/1.1 200 OK\r\nContent-Length: {len}\r\nETag: {etag}\r\nX-Racer-Crc64: {checksum}\r\nX-Racer-Signature: {signature}\r\nConnection: close\r\n\r\n"
    )
}

#[test]
fn candidate_reservation_is_bounded_and_strict_at_exhaustion() {
    let now = Instant::now();
    let caller = now + TIMEOUT;
    assert_eq!(
        candidate_end(now, caller, 3) - now,
        Duration::from_nanos(9_833_333_333)
    );
    assert_eq!(candidate_end(now, caller, 1) - now, MAX_CANDIDATE);
    assert_eq!(candidate_end(now, now, 3), now);
    assert_eq!(candidate_end(now, now + RETURN_SLACK, 3), now);
    let first = candidate_end(now, caller, 3);
    let second = candidate_end(first, caller, 2);
    let third = candidate_end(second, caller, 1);
    assert!(now < first && first < second && second < third && third < caller);
}

#[test]
fn colocated_positions_share_only_normalized_candidate_scope() {
    let (_, mut config) = crate::control::tests::fixture();
    let volume = &mut config.volumes[0];
    volume.peers = vec!["p2".into(), "p3".into()];
    volume.topology = Some(crate::control::proto::Topology {
        routing_algorithm: None,
        epoch: 1,
        slot_count: 8,
        local_slots: vec![0, 1],
        neighbors: vec![
            crate::control::proto::SlotPeer {
                slot: 2,
                peer: "p2".into(),
            },
            crate::control::proto::SlotPeer {
                slot: 3,
                peer: "p3".into(),
            },
        ],
    });
    let routing = Arc::new(crate::routing::Routing::new(&config.universe, volume).unwrap());
    let target = (0..)
        .map(|i| format!("/colocated-{i}"))
        .find(|t| routing.start(t).owner == 7)
        .unwrap();
    let cursor = routing.start(&target);
    assert_eq!(routing.normalized_position(&cursor).unwrap(), 1);
    let mut provider = Provider {
        routing: Some(routing.clone()),
        active: Some(Rc::new(RefCell::new(RouteState {
            target,
            cursor,
            origin: true,
            exhausted: false,
        }))),
        ..Provider::new(Backend::new("127.0.0.1:1", "test-origin").unwrap())
    };
    let first = provider.network_scope([1; 32]).unwrap();
    assert_eq!(first.version, 5);
    let state = provider.active.as_ref().unwrap().clone();
    state.borrow_mut().cursor.position = 1;
    assert_eq!(first, provider.network_scope([1; 32]).unwrap());
    state.borrow_mut().cursor.attempt = 1;
    state.borrow_mut().cursor.position = 0;
    assert_ne!(first, provider.network_scope([1; 32]).unwrap());
    // Versioned generations must remain isolated even with identical paths.
    volume.topology.as_mut().unwrap().epoch += 1;
    let newer = Arc::new(crate::routing::Routing::new(&config.universe, volume).unwrap());
    state.borrow_mut().cursor.identity = newer.identity;
    provider.routing = Some(newer);
    assert_ne!(
        first.routing,
        provider.network_scope([1; 32]).unwrap().routing
    );
}

#[test]
fn topology_application_errors_and_transport_breaker_rejection_do_not_mark_owner_down() {
    let (trust, config) = crate::control::tests::fixture();
    let prepared = trust
        .prepare(crate::control::proto::Configuration {
            contents: Some(crate::control::proto::configuration::Contents::Snapshot(
                config,
            )),
        })
        .unwrap();
    let routing = prepared.volumes[0].routing.clone();
    let target = (0..)
        .map(|n| format!("/health-{n}"))
        .find(|t| routing.start(t).owner == 1)
        .unwrap();
    let mut provider = Provider {
        peers: BTreeMap::from([(
            "p1".into(),
            Peer::from_endpoint(prepared.peers["p1"].clone()),
        )]),
        routing: Some(routing.clone()),
        ..Provider::new(prepared.volumes[0].backend.clone())
    };
    let state = Rc::new(RefCell::new(RouteState {
        target: target.clone(),
        cursor: routing.start(&target),
        origin: true,
        exhausted: false,
    }));
    provider.activate(Some(state.clone()));
    let http = &mut provider.peer.as_mut().unwrap().http;
    http.breaker.try_acquire().unwrap().failure();
    let error = http.connection().err().unwrap();
    assert_eq!(error.kind(), io::ErrorKind::WouldBlock);
    for error in [
        error.into(),
        status(503),
        cache::Error::InvalidData("corrupt peer"),
        io::Error::other(OwnerUnavailable(0)).into(),
        cache::Error::Timeout,
        cache::Error::Admission(io::ErrorKind::WouldBlock.into()),
        io::Error::from(io::ErrorKind::ConnectionRefused).into(),
    ] {
        assert!(provider.peer_failed(error).is_err());
        assert_eq!(state.borrow().cursor.attempt, 0);
        assert!(provider.owners.is_empty());
    }
    let route = AttemptRoute {
        cursor: state.borrow().cursor.clone(),
        candidate: 1,
        endpoint: provider.peer.as_ref().unwrap().http.endpoint.address,
        final_hop: true,
        context: "a".repeat(96),
    };
    use client::attempt::{Cause, Failure, Phase, Transport};
    let evidence = Failure {
        endpoint: route.endpoint,
        transport: Transport::Http,
        phase: Phase::Headers,
        cause: Cause::ServiceTimeout,
        initiated: true,
        kind: io::ErrorKind::TimedOut,
        message: "test service timeout".into(),
    };
    for cause in [
        Cause::CallerDeadline,
        Cause::LocalPressure,
        Cause::Cancelled,
        Cause::Protocol,
        Cause::BreakerRejected,
        Cause::Other,
    ] {
        let failure = AttemptFailure {
            route: route.clone(),
            evidence: Some(Failure {
                cause,
                ..evidence.clone()
            }),
            reported: false,
        };
        assert!(
            provider
                .peer_failed(io::Error::other(failure).into())
                .is_err()
        );
    }
    for (final_hop, initiated, stale) in [
        (false, true, false),
        (true, false, false),
        (true, true, true),
    ] {
        let mut route = route.clone();
        route.final_hop = final_hop;
        if stale {
            route.cursor.identity[0] ^= 1;
        }
        let failure = AttemptFailure {
            route,
            evidence: Some(Failure {
                initiated,
                ..evidence.clone()
            }),
            reported: stale,
        };
        assert!(
            provider
                .peer_failed(io::Error::other(failure).into())
                .is_err()
        );
    }
    assert_eq!(state.borrow().cursor.attempt, 0);
    // Semantic responses from a healthy relay never imply failure of that
    // relay, nor owner failure unless the exact owner reason is validated.
    for (reason, _, _) in SEMANTICS
        .into_iter()
        .filter(|(r, _, _)| *r != PeerReason::OwnerUnavailable)
    {
        let failure = PeerFailure {
            identity: route.cursor.identity,
            candidate: 1,
            reason,
            evidence: Some((&evidence).into()),
        };
        let decoded = PeerFailure::decode(&failure.encode()).unwrap();
        assert_eq!(failure, decoded);
        let mut attempt = Some(Attempt {
            route: route.clone(),
            owner: Some(
                provider
                    .owners
                    .acquire_final(route.cursor.identity, 1, routing.final_peer(&route.cursor))
                    .unwrap(),
            ),
        });
        let error = provider.reported(decoded, &mut attempt).unwrap_err();
        assert_eq!(semantic_failure(&error), Some(failure));
        assert!(provider.peer_failed(error).is_err());
        assert_eq!(state.borrow().cursor.attempt, 0);
        assert!(!provider.owners.blocked(route.cursor.identity, 1));
        assert!(
            provider
                .owners
                .physical_evidence(
                    route.cursor.identity,
                    routing.final_peer(&route.cursor).unwrap()
                )
                .is_none()
        );
    }
    for (identity, candidate) in [([99; 32], 1), (route.cursor.identity, 0)] {
        let mut attempt = Some(Attempt {
            route: route.clone(),
            owner: None,
        });
        let error = provider
            .reported(
                PeerFailure {
                    identity,
                    candidate,
                    reason: PeerReason::OwnerUnavailable,
                    evidence: None,
                },
                &mut attempt,
            )
            .unwrap_err();
        assert!(!provider.proven_failure(&error));
        assert!(provider.peer_failed(error).is_err());
    }
    let joined = Rc::new(RefCell::new(state.borrow().clone()));
    let relay = Rc::new(RefCell::new(state.borrow().clone()));
    relay.borrow_mut().origin = false;
    let shared = Arc::new(cache::Error::from(io::Error::other(AttemptFailure {
        route,
        evidence: Some(evidence),
        reported: false,
    })));
    assert!(
        provider
            .peer_failed(cache::Error::Shared(shared.clone()))
            .unwrap()
    );
    assert_eq!(state.borrow().cursor.attempt, 1);
    assert!(!provider.has_peer()); // ring successor 0 is local
    provider.activate(Some(joined.clone()));
    assert!(
        provider
            .peer_failed(cache::Error::Shared(shared.clone()))
            .unwrap()
    );
    assert_eq!(
        joined.borrow().cursor.attempt,
        1,
        "joiner gets exact owner evidence"
    );
    provider.activate(Some(relay.clone()));
    let error = provider
        .peer_failed(cache::Error::Shared(shared))
        .unwrap_err();
    assert_eq!(owner_failure(&error), Some(1));
    assert_eq!(
        relay.borrow().cursor.attempt,
        0,
        "shared attempt never advances at relay"
    );
}

use crate::{
    allocator,
    buffers::{self, Key},
    uring,
};
use std::{io::Write as _, net::TcpListener, sync::Arc, thread};

fn deadline() -> Instant {
    Instant::now() + Duration::from_secs(10)
}
fn cache(backend: &Backend, shards: usize) -> Cache {
    cache::adapter_fixture::cache(backend.namespace(), shards)
}

#[test]
fn peer_metrics_include_selected_and_unselected_routes_once() {
    let world = crate::simulation::World::new(915);
    let _scope = world.enter();
    let (trust, config) = crate::control::tests::fixture();
    let prepared = crate::control::tests::prepare_snapshot(&trust, config);
    let backend = prepared.volumes[0].backend.clone();
    let mut handler = Handler::new(cache(&backend, 1), backend);
    let routing = prepared.volumes[0].routing.clone();
    handler.set_routing(
        routing.clone(),
        BTreeMap::from([
            (
                "p1".into(),
                Peer::from_endpoint(prepared.peers["p1"].clone()),
            ),
            ("p2".into(), Peer::new("127.0.0.1:8083", None).unwrap()),
        ]),
    );
    handler.upstream.peers["p1"]
        .http
        .breaker
        .try_acquire()
        .unwrap()
        .failure();
    let observe = |handler: &Handler| {
        let mut peers = Vec::new();
        handler.peer_metrics("v1", &mut peers);
        peers.sort_by(|a, b| a.peer.cmp(&b.peer));
        peers
    };
    let before = observe(&handler);
    assert_eq!(before.len(), 2);
    assert_eq!(before[0].http, crate::breaker::Status::Open);
    assert_eq!(before[1].http, crate::breaker::Status::Closed);
    let target = (0..)
        .map(|i| format!("/metrics-{i}"))
        .find(|target| routing.start(target).owner == 1)
        .unwrap();
    let route = handler.route_state(None, &target, false).unwrap();
    handler.upstream.activate(route);
    assert_eq!(handler.upstream.selected.as_deref(), Some("p1"));
    assert!(!handler.upstream.peers.contains_key("p1"));
    assert_eq!(observe(&handler), before);
    handler.upstream.activate(None);
    assert!(handler.upstream.peer.is_none());
    assert_eq!(observe(&handler), before);
}

#[test]
fn canonical_namespace_and_setup() {
    let a = Backend::new("127.0.0.1:80", "test-origin").unwrap();
    let b = Backend::new("[::1]:81", "test-origin").unwrap();
    assert_eq!(a.namespace(), b.namespace());
    assert_eq!(a.namespace(), cache::Namespace::new("test-origin").unwrap());
    assert_ne!(
        a.namespace(),
        Backend::new("127.0.0.1:80", "other-origin")
            .unwrap()
            .namespace()
    );
    for url in [
        "https://127.0.0.1",
        "http://127.0.0.1/path",
        "http://127.0.0.1?x",
        "http://a@127.0.0.1",
        "http://",
    ] {
        assert!(Backend::new(url, "test-origin").is_err(), "{url}");
    }
}

#[test]
fn upstream_status_mapping() {
    for (code, expected) in [
        (404, 404),
        (410, 410),
        (412, 412),
        (500, 502),
        (302, 502),
        (204, 502),
    ] {
        assert_eq!(error_status(&status(code)), expected);
    }
    assert_eq!(error_status(&cache::Error::Timeout), 504);
}

fn destination(ring: &Ring, key: [u8; 32]) -> (buffers::PublicationAuthority, Destination) {
    let fill = ring.pool().stage(Key::new(key)).unwrap();
    fill.split_destination()
}
fn grant(provider: &mut Provider, exchange: Exchange, ring: &mut Ring) -> Exchange {
    let Exchange::Grant {
        connection, ticket, ..
    } = &exchange
    else {
        panic!("grant")
    };
    connection.test_grant(ticket);
    let ExchangeProgress::Pending { exchange, .. } = provider.poll(exchange, ring).unwrap() else {
        panic!("pending read")
    };
    assert!(matches!(exchange, Exchange::Read { .. }));
    exchange
}
fn simulated_handler(target: &str) -> (allocator::Slab, Ring, Handler, cache::PageRequest) {
    let size = 32 * 1024 * 1024;
    let mut slab =
        allocator::Slab::simulated(crate::simulation::Disk::new(size), size, 1, true).unwrap();
    let cache = cache::tests::cache_from_slab(&mut slab, 1, allocator::Config::default());
    let mut ring = crate::control::tests::ring().unwrap();
    let mut handler = Handler::new(cache, Backend::new("127.0.0.1:1", "test-origin").unwrap());
    authenticate_provider(&mut handler.upstream);
    let page = page_request(&mut handler.cache.borrow_mut(), &mut ring, target);
    handler.set_peer(Peer::new("127.0.0.1:2", Some(rdma::test_connection(ring.pool()))).unwrap());
    (slab, ring, handler, page)
}

#[test]
fn http_error_metrics_count_sent_headers_and_stream_abort_once() {
    use crate::metrics::{Local, Registry};
    use crate::simulation::World;

    // Drive actual header sends, including partial sends, through a small wrapper
    // that injects failures at the owning handler's metadata/page boundaries.
    struct Inject {
        handler: Handler,
        error: Option<cache::Error>,
        abort: bool,
        expire_before_headers: bool,
        early: Option<bool>,
        page: cache::PageRequest,
    }
    impl http::Handler for Inject {
        type Task = Task;
        fn start(&mut self, request: http::Request) -> Task {
            let mut task = self.handler.start(request);
            task.failure = None;
            task.fault = self.error.take().map(Initial::Rejected);
            if self.abort {
                task.respond(200, 3, &[]).unwrap();
            }
            if let Some(fail) = self.early {
                task.respond(if fail { 503 } else { 200 }, 0, &[]).unwrap();
                if fail {
                    // Abandon prepared headers before a single send completes.
                    task.response = Response::Done;
                }
            }
            self.handler.upstream.metrics.publish();
            assert_eq!(
                self.handler.upstream.metrics.values()[21..]
                    .iter()
                    .sum::<u64>(),
                0,
                "choosing an error before sending headers must not count"
            );
            task
        }
        fn poll(
            &mut self,
            task: &mut Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<http::Completed>> {
            if self.expire_before_headers || self.abort && task.headers_sent {
                self.abort = false;
                self.expire_before_headers = false;
                task.fault = None;
                // An expired payload fault after successful headers cannot emit
                // a replacement 504. It must abort and retain the deadline reason.
                let wire = descriptor(&UpstreamRequest::PeerPage(self.page.clone())).unwrap();
                let fault = self
                    .handler
                    .cache
                    .borrow_mut()
                    .peer_fault::<Provider>(
                        decode_descriptor(&wire).unwrap(),
                        crate::environment::now(),
                    )
                    .unwrap();
                task.pages.push_back((0, PageLoad::Loading(fault, None)));
            }
            self.handler.poll(task, ring, budget)
        }
    }

    for (case, error, reason, status) in [
        (0, Some(cache::busy("test pressure")), "busy", 503),
        (
            1,
            Some(io::Error::other(OwnerUnavailable(7)).into()),
            "owner_unavailable",
            503,
        ),
        (2, Some(cache::Error::Unavailable), "unavailable", 503),
        (3, Some(cache::Error::NotFound), "not_found", 404),
        (4, None, "deadline", 200),
        (5, None, "deadline", 504),
        (6, Some(cache::Error::Unavailable), "unavailable", 503),
        (7, Some(cache::busy("HEAD admission")), "busy", 503),
        (8, None, "busy", 503),
        (9, None, "other", 200),
    ] {
        let world = World::new(817 + case);
        let _scope = world.enter();
        world.short_transfers(7);
        let (slab, mut ring, mut handler, page) = simulated_handler("/metric-object");
        let metrics: Local = handler.upstream.metrics.clone();
        let registry = Registry::new(1, Arc::new(crate::control::Updates::default()));
        registry.register(0, &metrics);
        // The injected metadata errors do not initiate upstream work.
        handler.upstream.peer = None;
        let address = "127.0.0.1:18929".parse().unwrap();
        let listener =
            http::Listener::bind(address, std::num::NonZeroU32::new(8).unwrap()).unwrap();
        let mut server = http::Server::new(
            listener,
            Inject {
                handler,
                error,
                abort: case == 4,
                expire_before_headers: case == 5,
                early: (case >= 8).then_some(case == 8),
                page,
            },
            http::Config::default(),
        );
        let peer_wire = hex(b"RF04\x88\x13\0\0RF05\0/metric-object");
        let fields = if case == 6 {
            vec![("X-Racer-Fault", peer_wire.as_str())]
        } else {
            Vec::new()
        };
        let connection = client::Connection::new(address, "cache").unwrap();
        let request = client::Request::new("/metric-object", &fields).unwrap();
        let end = world.now() + Duration::from_secs(10);
        let (mut get, mut head) = if case == 7 {
            (None, Some(connection.head(request, end).unwrap()))
        } else {
            (
                Some(
                    connection
                        .get(request, ring.pool().private_fill().unwrap(), end)
                        .unwrap(),
                ),
                None,
            )
        };
        let mut finished = false;
        for _ in 0..10_000 {
            ring.progress().unwrap();
            server.poll(&mut ring, 16).unwrap();
            let response = if let Some(head) = &mut head {
                head.poll(&mut ring, 16).map(|p| match p {
                    Progress::Ready(r) => Progress::Ready(r.status()),
                    Progress::Pending(w) => Progress::Pending(w),
                })
            } else {
                get.as_mut().unwrap().poll(&mut ring, 16).map(|p| match p {
                    Progress::Ready(r) => Progress::Ready(r.status()),
                    Progress::Pending(w) => Progress::Pending(w),
                })
            };
            match response {
                Ok(Progress::Ready(code)) => {
                    assert_ne!(case, 4, "truncated success must not complete");
                    assert_eq!(code, status);
                    finished = true;
                    break;
                }
                Err(_) => {
                    assert!(case == 4 || case == 8);
                    finished = true;
                    break;
                }
                _ => {}
            }
            world.service_tick();
        }
        assert!(finished, "case {case} stalled");
        for _ in 0..10 {
            ring.progress().unwrap();
            server.poll(&mut ring, 16).unwrap();
            world.service_tick();
        }
        metrics.publish();
        let text = registry.render();
        let family = if case == 4 {
            "stream_aborts"
        } else {
            "error_responses"
        };
        let source = if case == 6 { "peer" } else { "client" };
        let labels = if case == 4 {
            format!("source=\"client\",reason=\"{reason}\"")
        } else {
            format!("source=\"{source}\",status=\"{status}\",reason=\"{reason}\"")
        };
        assert!(
            case == 9
                || text.contains(&format!(
                    "racer_dataplane_http_{family}_total{{{labels}}} {}\n",
                    u8::from(case < 8)
                )),
            "{text}"
        );
        let sum: u64 = text
            .lines()
            .filter(|l| {
                l.starts_with("racer_dataplane_http_error_responses_total")
                    || l.starts_with("racer_dataplane_http_stream_aborts_total")
            })
            .map(|l| l.rsplit_once(' ').unwrap().1.parse::<u64>().unwrap())
            .sum();
        assert_eq!(
            sum,
            u64::from(case < 8),
            "no unsent, success, duplicate or replacement response counts"
        );
        if case == 0 {
            assert!(text.contains("event=\"error_response\",cause=\"admission\"} 1\n"));
        }
        drop((get, head));
        server.shutdown(&mut ring).unwrap();
        server.handler_mut().handler.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
        drop((server, ring, slab));
        world.run_tasks();
        world.assert_clean();
    }
}
fn resolve<F, T>(
    cache: &mut Cache,
    ring: &mut Ring,
    upstream: &mut FailRead,
    mut fault: F,
    poll: impl Fn(&mut Cache, F, &mut Ring, &mut FailRead) -> cache::Result<cache::Progress<F, T>>,
) -> T {
    loop {
        ring.progress().unwrap();
        let mut work = cache.poll(ring, 1).unwrap();
        match poll(cache, fault, ring, upstream).unwrap() {
            cache::Progress::Ready(value) => return value,
            cache::Progress::Pending {
                fault: next,
                work: pending,
            } => {
                fault = next;
                work.merge(pending);
            }
        }
        if !work.runnable {
            ring.wait(work.deadline).unwrap();
        }
    }
}

#[test]
fn kernel_integration() {
    http::cache_responses::kernel_child("handlers::tests::kernel_child", "RACER_HANDLERS_CHILD");
}

#[test]
#[ignore = "run via bounded kernel_integration subprocess"]
fn kernel_child() {
    if std::env::var_os("RACER_HANDLERS_CHILD").is_none() {
        return;
    }
    let Some(mut ring) = crate::conformance::kernel_ring(2, uring::Config::default()) else {
        return;
    };
    rdma_completion_and_http_fallback(&mut ring);
    rdma_http_backend_chain(&mut ring);
    metadata_http_with_rdma_available_and_pinned_payloads(&mut ring);
    hot_cold_head_and_peer_response_with_pinned_payloads(&mut ring);
    backend_crc_headers_ignored(&mut ring);
    http::cache_responses::http_streaming(&mut ring);
    http::cache_responses::http_error_responses(&mut ring);
    ring.shutdown().unwrap();
}

fn backend_crc_headers_ignored(ring: &mut Ring) {
    for header in [
        "",
        "X-Racer-Crc64: 0000000000000000\r\n",
        "X-Racer-Crc64: invalid\r\n",
        "X-Racer-Crc64: 0000000000000000\r\nX-Racer-Crc64: invalid\r\n",
    ] {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let backend =
            Backend::new(&listener.local_addr().unwrap().to_string(), "test-origin").unwrap();
        let mut handler = Handler::new(cache(&backend, 1), backend);
        let page = page_request(&mut handler.cache.borrow_mut(), ring, "/origin-checksum");
        let (authority, dest) = destination(ring, *page.key());
        let server = thread::spawn(move || {
            let mut stream = accept(&listener);
            assert!(request(&mut stream).starts_with("GET /origin-checksum HTTP/1.1\r\n"));
            write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\n{header}Connection: close\r\n\r\nabc",
                crate::conformance::etag(b"abc")
            )
            .unwrap();
        });
        let end = deadline();
        let mut exchange = handler
            .upstream
            .start(UpstreamRequest::BackendPage(page), dest, end, ring)
            .unwrap();
        let received = loop {
            assert!(Instant::now() < end);
            ring.progress().unwrap();
            match handler.upstream.poll(exchange, ring).unwrap() {
                ExchangeProgress::Pending {
                    exchange: next,
                    work,
                } => {
                    exchange = next;
                    if !work.runnable {
                        ring.wait(Some(work.deadline.unwrap_or(end).min(end)))
                            .unwrap();
                    }
                }
                ExchangeProgress::Ready(UpstreamResult::BackendPage { received, .. }) => {
                    break received;
                }
                _ => panic!("unexpected backend result"),
            }
        };
        assert_eq!(received.checksum, None);
        let mut fill = authority.reunite(received.destination).ok().unwrap();
        assert_eq!(&fill.as_mut_slice()[..received.len], b"abc");
        drop(fill);
        server.join().unwrap();
        handler.shutdown(ring).unwrap();
    }
}

// Inject a transport deadline after READ acceptance while cache retains the
// publication authority. Everything after that point uses the real retry path.
struct FailRead {
    provider: Provider,
    rdma: usize,
    http: usize,
    backend: usize,
    completed_bytes: Option<Vec<u8>>,
}
impl Upstream for FailRead {
    type Exchange = Exchange;
    fn start_metadata(
        &mut self,
        request: UpstreamRequest,
        deadline: Instant,
        ring: &mut Ring,
    ) -> cache::Result<Exchange> {
        if matches!(request, UpstreamRequest::BackendMetadata(_)) {
            self.backend += 1;
        } else {
            self.http += 1;
        }
        self.provider.start_metadata(request, deadline, ring)
    }
    fn peer_validated(&mut self, retry: &mut Exchange, valid: bool) {
        self.provider.peer_validated(retry, valid);
    }
    fn has_peer(&self) -> bool {
        true
    }
    fn start(
        &mut self,
        request: UpstreamRequest,
        destination: Destination,
        deadline: Instant,
        ring: &mut Ring,
    ) -> cache::Result<Exchange> {
        if matches!(
            request,
            UpstreamRequest::BackendPage(_) | UpstreamRequest::BackendMetadata(_)
        ) {
            self.backend += 1;
        }
        let exchange = self.provider.start(request, destination, deadline, ring)?;
        if let Exchange::Grant {
            connection, ticket, ..
        } = &exchange
        {
            self.rdma += 1;
            connection.test_grant(ticket);
        }
        Ok(exchange)
    }
    fn resume_peer(
        &mut self,
        exchange: Exchange,
        destination: Destination,
        deadline: Instant,
        ring: &mut Ring,
    ) -> cache::Result<Exchange> {
        self.http += 1;
        self.provider
            .resume_peer(exchange, destination, deadline, ring)
    }
    fn poll(
        &mut self,
        mut exchange: Exchange,
        ring: &mut Ring,
    ) -> cache::Result<ExchangeProgress<Exchange>> {
        if let Exchange::Read {
            connection,
            ticket,
            deadline,
            checksum,
            ..
        } = &mut exchange
        {
            if let Some(bytes) = self.completed_bytes.take() {
                *checksum = Some(crate::allocator::crc64(&bytes));
                connection.test_read(ticket, &bytes);
            } else {
                connection.cancel(ticket)?;
                *deadline = Instant::now();
            }
        }
        self.provider.poll(exchange, ring)
    }
}

fn rdma_http_backend_chain(ring: &mut Ring) {
    let peer = TcpListener::bind("127.0.0.1:0").unwrap();
    let backend = TcpListener::bind("127.0.0.1:0").unwrap();
    let peer_url = peer.local_addr().unwrap().to_string();
    let backend = Backend::new(&backend.local_addr().unwrap().to_string(), "test-origin")
        .map(|config| (config, backend))
        .unwrap();
    let mut cache = cache(&backend.0, 1);
    let page = page_request(&mut cache, ring, "/fallback-chain");
    let wire = descriptor(&UpstreamRequest::PeerPage(page)).unwrap();
    let mut upstream = FailRead {
        provider: Provider {
            peer: Some(Peer::new(&peer_url, Some(rdma::test_connection(ring.pool()))).unwrap()),
            ..Provider::new(backend.0)
        },
        rdma: 0,
        http: 0,
        backend: 0,
        completed_bytes: None,
    };
    authenticate_provider(&mut upstream.provider);
    let peer_thread = thread::spawn(move || {
        let mut stream = accept(&peer);
        assert!(request(&mut stream).starts_with("GET / HTTP/1.1\r\n"));
        stream
            .write_all(
                b"HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
            )
            .unwrap();
    });
    let backend_thread = thread::spawn(move || {
        let mut stream = accept(&backend.1);
        let request = request(&mut stream);
        assert!(request.starts_with("GET /fallback-chain HTTP/1.1\r\n"));
        write!(stream, "HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Range: bytes 0-2/3\r\nETag: {}\r\nConnection: close\r\n\r\nabc", crate::conformance::etag(b"abc")).unwrap();
    });
    let fault = cache
        .peer_fault::<FailRead>(decode_descriptor(&wire).unwrap(), deadline())
        .unwrap();
    let buffer = resolve(&mut cache, ring, &mut upstream, fault, Cache::poll_fault);
    assert_eq!(buffer.as_slice(), b"abc");
    drop(buffer);
    assert_eq!((upstream.rdma, upstream.http, upstream.backend), (1, 1, 1));
    peer_thread.join().unwrap();
    backend_thread.join().unwrap();
    cache.shutdown(ring).unwrap();
}

fn metadata_http_with_rdma_available_and_pinned_payloads(ring: &mut Ring) {
    for stale in [false, true] {
        let peer = TcpListener::bind("127.0.0.1:0").unwrap();
        let backend_listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let peer_url = peer.local_addr().unwrap().to_string();
        let backend = Backend::new(
            &backend_listener.local_addr().unwrap().to_string(),
            "test-origin",
        )
        .unwrap();
        let mut cache = cache(&backend, 1);
        let expires = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs()
            + 60;
        let fresh = crate::metadata::Metadata {
            checksum: crate::metadata::Checksum(*blake3::hash(b"1234567").as_bytes()),
            len: 7,
            expires,
        }
        .to_bytes()
        .to_vec();
        let mut upstream = FailRead {
            provider: Provider {
                peer: Some(Peer::new(&peer_url, Some(rdma::test_connection(ring.pool()))).unwrap()),
                ..Provider::new(backend)
            },
            rdma: 0,
            http: 0,
            backend: 0,
            completed_bytes: None,
        };
        authenticate_provider(&mut upstream.provider);
        let server = thread::spawn(move || {
            let mut stream = accept(&peer);
            let request = request(&mut stream);
            {
                assert!(request.starts_with("GET / HTTP/1.1\r\n"));
                let encoded = request
                    .lines()
                    .find_map(|line| line.strip_prefix("X-Racer-Fault: "))
                    .unwrap();
                let bytes = unhex(encoded).unwrap();
                assert_eq!(
                    budget_descriptor(&bytes).unwrap().0,
                    b"RF05\0//metadata%2f?x=1&x=2"
                );
                let mut fresh = fresh;
                if stale {
                    fresh[40..48].copy_from_slice(&(expires - 61).to_le_bytes());
                }
                stream
                    .write_all(
                        peer_response(
                            &request,
                            fresh.len(),
                            crate::allocator::crc64(&fresh),
                            &crate::conformance::etag(b"1234567"),
                        )
                        .as_bytes(),
                    )
                    .unwrap();
                stream.write_all(&fresh).unwrap();
            }
            if stale {
                let mut stream = accept(&backend_listener);
                assert!(
                    self::request(&mut stream)
                        .starts_with("HEAD //metadata%2f?x=1&x=2 HTTP/1.1\r\n")
                );
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"1234567")).unwrap();
            } else {
                backend_listener.set_nonblocking(true).unwrap();
                assert_eq!(
                    backend_listener.accept().unwrap_err().kind(),
                    io::ErrorKind::WouldBlock
                );
            }
        });
        let mut pinned = Vec::new();
        while let Ok(fill) = ring.pool().private_fill() {
            pinned.push(fill);
        }
        let fault = cache.metadata("//metadata%2f?x=1&x=2", deadline()).unwrap();
        let meta = resolve(&mut cache, ring, &mut upstream, fault, Cache::poll_metadata);
        assert_eq!(meta.len(), 7);
        assert!(meta.expires() >= expires);
        drop(meta);
        assert_eq!(
            (upstream.rdma, upstream.http, upstream.backend),
            if stale { (0, 1, 1) } else { (0, 1, 0) }
        );
        // Metadata uses small HTTP storage even when an RDMA connection is available.
        assert_eq!(
            upstream
                .provider
                .peer
                .as_mut()
                .unwrap()
                .breaker
                .try_acquire()
                .is_ok(),
            true
        );
        drop(pinned);
        server.join().unwrap();
        cache.shutdown(ring).unwrap();
    }
}

fn hot_cold_head_and_peer_response_with_pinned_payloads(ring: &mut Ring) {
    use std::io::Read;
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    let backend = Backend::new(&origin.local_addr().unwrap().to_string(), "test-origin").unwrap();
    let origin_thread = thread::spawn(move || {
        let mut socket = accept(&origin);
        assert!(request(&mut socket).starts_with("HEAD /pinned HTTP/1.1\r\n"));
        write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"1234567")).unwrap();
    });
    let mut handler = Handler::new(cache(&backend, 1), backend);
    handler.upstream.authentication = Some(peer_policy(3, 2));
    let listener = http::Listener::bind(
        "127.0.0.1:0".parse().unwrap(),
        std::num::NonZeroU32::new(16).unwrap(),
    )
    .unwrap();
    let address = listener.local_addr().unwrap();
    let mut server = http::Server::new(listener, handler, http::Config::default());
    let mut pinned = Vec::new();
    while let Ok(fill) = ring.pool().private_fill() {
        pinned.push(fill);
    }
    let client = thread::spawn(move || {
        for peer in [false, false, true, true] {
            let mut socket = std::net::TcpStream::connect(address).unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let mut fields = Vec::new();
            if peer {
                fields.push((
                    "X-Racer-Fault".to_owned(),
                    hex(b"RF04\x88\x13\0\0RF05\0/pinned").into_bytes(),
                ));
                peer_policy(2, 3)
                    .request([3; 32], "GET", "/", &mut fields)
                    .unwrap();
            }
            write!(
                socket,
                "{} {} HTTP/1.1\r\nHost: cache\r\nConnection: close\r\n",
                if peer { "GET" } else { "HEAD" },
                if peer { "/" } else { "/pinned" }
            )
            .unwrap();
            for (name, value) in fields {
                write!(
                    socket,
                    "{name}: {}\r\n",
                    std::str::from_utf8(&value).unwrap()
                )
                .unwrap();
            }
            socket.write_all(b"\r\n").unwrap();
            let headers = request(&mut socket);
            assert!(headers.starts_with("HTTP/1.1 200"), "{headers}");
            if peer {
                let mut bytes = [0; cache::METADATA_SIZE];
                socket.read_exact(&mut bytes).unwrap();
                let record = crate::metadata::Metadata::from_bytes(&bytes).unwrap();
                assert_eq!(record.len, 7);
                assert_eq!(
                    record.checksum.etag().as_str(),
                    crate::conformance::etag(b"1234567")
                );
            }
        }
    });
    let end = deadline();
    while !client.is_finished() {
        assert!(Instant::now() < end);
        ring.progress().unwrap();
        server.handler_mut().poll_background(ring, 16).unwrap();
        server.poll(ring, 16).unwrap();
        thread::yield_now();
    }
    client.join().unwrap();
    origin_thread.join().unwrap();
    assert!(ring.pool().private_fill().is_err());
    drop(pinned);
    server.shutdown(ring).unwrap();
    server.handler_mut().shutdown(ring).unwrap();
}

fn rdma_completion_and_http_fallback(ring: &mut Ring) {
    let backend = Backend::new("127.0.0.1:1", "test-origin").unwrap();
    let mut handler = Handler::new(cache(&backend, 1), backend);
    authenticate_provider(&mut handler.upstream);
    let page = page_request(&mut handler.cache.borrow_mut(), ring, "//rdma%2f?x=1&x=2");
    let wire = descriptor(&UpstreamRequest::PeerPage(page.clone())).unwrap();
    let decoded = handler
        .cache
        .borrow_mut()
        .peer_fault::<Provider>(decode_descriptor(&wire).unwrap(), deadline())
        .unwrap();
    assert_eq!(decoded.key(), page.key());
    assert_eq!(decoded.len(), page.len());
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = listener.local_addr().unwrap().to_string();
    handler.set_peer(Peer::new(&url, Some(rdma::test_connection(ring.pool()))).unwrap());
    assert_eq!(handler.connections.len(), 1);
    let (authority, dest) = destination(ring, *page.key());
    let exchange = handler
        .upstream
        .start(
            UpstreamRequest::PeerPage(page.clone()),
            dest,
            deadline(),
            ring,
        )
        .unwrap();
    let exchange = grant(&mut handler.upstream, exchange, ring);
    let Exchange::Read {
        connection, ticket, ..
    } = &exchange
    else {
        panic!("read")
    };
    connection.test_read(ticket, b"abc");
    let ExchangeProgress::ReadyPeer {
        result: UpstreamResult::PeerPage(received),
        retry,
    } = handler.upstream.poll(exchange, ring).unwrap()
    else {
        panic!("completion")
    };
    assert!(matches!(retry, Exchange::ValidateRdma(..)));
    drop(retry);
    let mut fill = authority.reunite(received.destination).ok().unwrap();
    assert_eq!(received.len, 3);
    assert_eq!(&fill.as_mut_slice()[..3], b"abc");
    drop(fill); // The provider did not publish; the identity can be acquired again.

    let (authority, dest) = destination(ring, *page.key());
    let exchange = handler
        .upstream
        .start(
            UpstreamRequest::PeerPage(page.clone()),
            dest,
            deadline(),
            ring,
        )
        .unwrap();
    let mut exchange = grant(&mut handler.upstream, exchange, ring);
    let Exchange::Read {
        connection,
        ticket,
        deadline: end,
        ..
    } = &mut exchange
    else {
        panic!("read")
    };
    // Accepted READ failure quiesces its old slot; adapter asks cache to reacquire.
    connection.cancel(ticket).unwrap();
    *end = Instant::now();
    let ExchangeProgress::RetryPeer { exchange } = handler.upstream.poll(exchange, ring).unwrap()
    else {
        panic!("HTTP retry required")
    };
    assert!(matches!(exchange, Exchange::RecoverHttp(..)));
    drop(authority);
    let (authority, dest) = destination(ring, *page.key());
    let server = thread::spawn(move || {
        let mut stream = accept(&listener);
        let request = request(&mut stream);
        assert!(request.starts_with("GET / HTTP/1.1\r\n"));
        let encoded = request
            .lines()
            .find_map(|line| line.strip_prefix("X-Racer-Fault: "))
            .unwrap();
        let bytes = unhex(encoded).unwrap();
        let (inner, budget) = budget_descriptor(&bytes).unwrap();
        assert_eq!(inner, wire);
        assert!(budget.is_some_and(|d| d <= MAX_CANDIDATE));
        stream
            .write_all(
                peer_response(&request, 3, 0x42, &crate::conformance::etag(b"abc")).as_bytes(),
            )
            .unwrap();
        stream.write_all(b"abc").unwrap();
    });
    let mut exchange = handler
        .upstream
        .resume_peer(exchange, dest, deadline(), ring)
        .unwrap();
    assert_eq!(&handler.upstream.metrics.values()[16..20], &[0, 1, 0, 2]);
    let end = deadline();
    let received = loop {
        assert!(Instant::now() < end);
        ring.progress().unwrap();
        match handler.upstream.poll(exchange, ring).unwrap() {
            ExchangeProgress::Pending {
                exchange: next,
                work,
            } => {
                exchange = next;
                if !work.runnable {
                    ring.wait(work.deadline).unwrap();
                }
            }
            ExchangeProgress::ReadyPeer {
                result: UpstreamResult::PeerPage(received),
                mut retry,
            } => {
                handler.upstream.peer_validated(&mut retry, true);
                break received;
            }
            _ => panic!("unexpected retry/result"),
        }
    };
    assert_eq!(received.checksum, Some(0x42));
    let mut fill = authority.reunite(received.destination).ok().unwrap();
    assert_eq!(&fill.as_mut_slice()[..received.len], b"abc");
    drop(fill);
    server.join().unwrap();
    handler.shutdown(ring).unwrap();
}

#[test]
fn dst_window_renewal_preserves_breaker_at_admission_grant_and_read() {
    for phase in 0..3 {
        let world = crate::simulation::World::new(163);
        let _scope = world.enter();
        let (_slab, mut ring, mut handler, page) = simulated_handler("/renewal");
        let connection = handler
            .upstream
            .peer
            .as_ref()
            .unwrap()
            .rdma
            .as_ref()
            .unwrap()
            .clone();
        if phase == 0 {
            connection.test_window_renewal(true);
        }
        let (authority, dest) = destination(&ring, *page.key());
        let mut exchange = handler
            .upstream
            .start(UpstreamRequest::PeerPage(page), dest, deadline(), &mut ring)
            .unwrap();
        if phase == 2 {
            exchange = grant(&mut handler.upstream, exchange, &mut ring);
        }
        if phase != 0 {
            connection.test_window_renewal(true);
            let ExchangeProgress::RetryPeer { exchange: next } =
                handler.upstream.poll(exchange, &mut ring).unwrap()
            else {
                panic!()
            };
            assert!(matches!(next, Exchange::RecoverHttp(..)));
            drop(next);
        } else {
            assert!(matches!(exchange, Exchange::Get(..)));
            drop(exchange);
        }
        assert!(
            handler
                .upstream
                .peer
                .as_ref()
                .unwrap()
                .breaker
                .try_acquire()
                .is_ok()
        );
        drop(authority);
        connection.test_window_renewal(false);
        handler.shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
    }
}

#[test]
fn dst_rdma_grant_read_http_share_absolute_candidate_budget() {
    for read in [false, true] {
        let world = crate::simulation::World::new(107);
        let _scope = world.enter();
        let (slab, mut ring, mut handler, page) = simulated_handler("/rdma-budget");
        let candidate = world.now() + Duration::from_millis(1800);
        let (authority, dest) = destination(&ring, *page.key());
        let mut exchange = handler
            .upstream
            .start(
                UpstreamRequest::PeerPage(page.clone()),
                dest,
                candidate,
                &mut ring,
            )
            .unwrap();
        let identity = [23; 32];
        handler
            .upstream
            .owners
            .acquire(identity, 3)
            .unwrap()
            .failure(world.now());
        world.advance(COOLDOWN);
        let probe = handler.upstream.owners.acquire(identity, 3).unwrap();
        let Exchange::Grant {
            attempt, deadline, ..
        } = &mut exchange
        else {
            panic!()
        };
        // Restore the transport window after advancing the virtual clock to
        // open an exclusive owner recovery probe.
        *deadline = world.now() + COOLDOWN;
        let candidate = candidate + COOLDOWN;
        *attempt = Some(Attempt {
            route: AttemptRoute {
                cursor: crate::routing::Cursor::decode(&[0; crate::routing::Cursor::LEN]).unwrap(),
                candidate: 3,
                endpoint: "127.0.0.1:2".parse().unwrap(),
                final_hop: true,
                context: String::new(),
            },
            owner: Some(probe),
        });
        if read {
            exchange = grant(&mut handler.upstream, exchange, &mut ring);
        }
        world.advance(Duration::from_secs(1));
        let ExchangeProgress::RetryPeer { exchange } =
            handler.upstream.poll(exchange, &mut ring).unwrap()
        else {
            panic!()
        };
        assert!(matches!(
            &exchange,
            Exchange::RecoverHttp(_, Some(Attempt { owner: Some(_), .. }))
        ));
        assert!(
            handler.upstream.owners.acquire(identity, 3).is_err(),
            "same-hop recovery retains exclusive probe"
        );
        drop(authority);
        // Fresh private storage while the old RDMA destination quiesces.
        let fill = ring
            .pool()
            .stage(crate::buffers::Key::new(*page.key()))
            .unwrap();
        let (authority, dest) = fill.split_destination();
        // Fallback has only 800ms left, less than the normal 1s HTTP cap.
        // Its reserved return slack leaves too little time to forward, so it
        // fails locally rather than minting another transport window.
        assert!(
            handler
                .upstream
                .resume_peer(exchange, dest, candidate, &mut ring)
                .is_err()
        );
        assert!(handler.upstream.owners.blocked(identity, 3));
        assert!(
            handler.upstream.owners.evidence(identity, 3).is_none(),
            "deadline does not refresh owner evidence"
        );
        drop(authority);
        handler.shutdown(&mut ring).unwrap();
        drop((handler, ring, slab));
        world.run_tasks();
        world.assert_clean();
    }
}

mod preconditions {
    use super::super::response::matches;
    use super::*;
    use std::{
        io::{Read, Write},
        net::{TcpListener, TcpStream},
        num::NonZeroU32,
        sync::{
            Arc,
            atomic::{AtomicBool, AtomicUsize, Ordering},
        },
        thread,
    };

    #[test]
    fn entity_tag_list_grammar_and_comparisons() {
        fn check(value: &[u8], tag: &[u8], strong: bool) -> io::Result<Option<bool>> {
            let mut bytes = b"If-Match: ".to_vec();
            bytes.extend_from_slice(value);
            let fields = [crate::http::field(&bytes, 0, bytes.len())?];
            matches(
                Headers {
                    bytes: &bytes,
                    headers: &fields,
                },
                "if-match",
                tag,
                strong,
            )
        }
        for (value, tag, strong, expected) in [
            (b"*".as_slice(), b"".as_slice(), true, true),
            (b"\"\"", b"", false, false),
            (b"\"\"", b"\"\"", true, true),
            (b"W/\"v1\"", b"\"v1\"", true, false),
            (b"\"v1\"", b"W/\"v1\"", true, false),
            (b"W/\"v1\"", b"W/\"v1\"", false, true),
            (b"\"v1\"", b"W/\"v1\"", false, true),
            (b" , \"old\",,\tW/\"v1\", ", b"\"v1\"", false, true),
            (b"\"a,b\\c\", \"other\"", b"\"a,b\\c\"", true, true),
            (b"\"\xff\"", b"\"\xff\"", true, true),
            (b"\"V1\"", b"\"v1\"", false, false),
        ] {
            assert_eq!(
                check(value, tag, strong).unwrap(),
                Some(expected),
                "{value:?}"
            );
        }
        for value in [
            b"".as_slice(),
            b", ,",
            b"v1",
            b"w/\"v1\"",
            b"W/ \"v1\"",
            b"\"v1",
            b"\"v1\" garbage",
            b"\"v1\" \"v2\"",
            b"*, \"v1\"",
            b"\"v1\", *",
            b"\"v1\", broken",
            b"\"white space\"",
            b"\"tab\t\"",
            b"\"del\x7f\"",
            b"\"control\x01\"",
        ] {
            assert!(check(value, b"\"v1\"", false).is_err(), "{value:?}");
        }
    }

    #[test]
    fn conditional_reads_tcp() {
        http::cache_responses::kernel_child(
            "handlers::tests::preconditions::conditional_reads_tcp_child",
            "RACER_PRECONDITIONS_CHILD",
        );
    }

    #[test]
    #[ignore = "run via bounded conditional_reads_tcp subprocess"]
    fn conditional_reads_tcp_child() {
        if std::env::var_os("RACER_PRECONDITIONS_CHILD").is_none() {
            return;
        }
        let Some(mut ring) = crate::conformance::kernel_ring(4, crate::uring::Config::default())
        else {
            return;
        };
        use crate::http_server::cache_responses::request;
        let origin = TcpListener::bind("127.0.0.1:0").unwrap();
        origin.set_nonblocking(true).unwrap();
        let backend =
            Backend::new(&origin.local_addr().unwrap().to_string(), "test-origin").unwrap();
        let revision = Arc::new(AtomicUsize::new(1));
        let pages = Arc::new(AtomicUsize::new(0));
        let stop = Arc::new(AtomicBool::new(false));
        let (version, count, done) = (revision.clone(), pages.clone(), stop.clone());
        let upstream = thread::spawn(move || {
            while !done.load(Ordering::Acquire) {
                let mut socket = match origin.accept() {
                    Ok((s, _)) => s,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    Err(e) => panic!("{e}"),
                };
                socket
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                let req = request(&mut socket);
                let rev = version.load(Ordering::Acquire);
                let tag = match rev {
                    1 => crate::conformance::etag(b"abcdef"),
                    2 => crate::conformance::etag(b"UVWXYZ"),
                    3 => format!("W/{}", crate::conformance::etag(b"UVWXYZ")),
                    _ => String::new(),
                };
                if req.starts_with("HEAD ") {
                    assert!(!req.contains("If-Match:"));
                    assert!(!req.contains("If-None-Match:"));
                    write!(
                        socket,
                        "HTTP/1.1 200 OK\r\nContent-Length: 6\r\nCache-Control: max-age=0\r\n"
                    )
                    .unwrap();
                    if !tag.is_empty() {
                        write!(socket, "ETag: {tag}\r\n").unwrap();
                    }
                    socket.write_all(b"Connection: close\r\n\r\n").unwrap();
                } else {
                    count.fetch_add(1, Ordering::Release);
                    assert!(req.starts_with("GET "));
                    assert!(req.contains("Range: bytes=0-5\r\n"));
                    assert!(req.contains(&format!("If-Match: {tag}\r\n")), "{req}");
                    assert!(!req.contains("If-None-Match:"));
                    write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: 6\r\nContent-Range: bytes 0-5/6\r\nETag: {tag}\r\nConnection: close\r\n\r\n").unwrap();
                    socket
                        .write_all(if rev == 1 { b"abcdef" } else { b"UVWXYZ" })
                        .unwrap();
                }
            }
        });
        let handler = Handler::new(
            cache::adapter_fixture::cache(backend.namespace(), 1),
            backend,
        );
        let listener =
            http::Listener::bind("127.0.0.1:0".parse().unwrap(), NonZeroU32::new(16).unwrap())
                .unwrap();
        let address = listener.local_addr().unwrap();
        let mut server = http::Server::new(listener, handler, http::Config::default());
        let client = thread::spawn(move || {
            let check = |method: &str, extra: &str, status: u16, body: &[u8], tag: &str| {
                // Symbolic revision names in this table render real body checksums.
                // Arbitrary nonmatching tags ("old") retain their HTTP meaning.
                let render = |value: &str| {
                    value
                        .replace("\"v1\"", &crate::conformance::etag(b"abcdef"))
                        .replace("\"v2\"", &crate::conformance::etag(b"UVWXYZ"))
                };
                let extra = render(extra);
                let tag = render(tag);
                let before = pages.load(Ordering::Acquire);
                let mut socket = TcpStream::connect(address).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                write!(
                    socket,
                    "{method} /mutable HTTP/1.1\r\nHost: cache\r\n{extra}Connection: close\r\n\r\n"
                )
                .unwrap();
                let head = request(&mut socket).to_ascii_lowercase();
                assert!(
                    head.starts_with(&format!("http/1.1 {status} ")),
                    "{method} {extra}: {head}"
                );
                let mut actual = Vec::new();
                socket.read_to_end(&mut actual).unwrap();
                assert_eq!(actual, body, "{method} {extra}");
                let len = if status == 304 || (method == "HEAD" && status == 200) {
                    6
                } else {
                    body.len()
                };
                assert!(
                    head.contains(&format!("content-length: {len}\r\n")),
                    "{head}"
                );
                if status != 400 && !tag.is_empty() {
                    assert!(
                        head.contains(&format!("etag: {}\r\n", tag.to_ascii_lowercase())),
                        "{head}"
                    );
                }
                if status == 206 {
                    let range = if extra.contains("Range: bytes=3-5") {
                        "bytes 3-5/6"
                    } else {
                        "bytes 0-2/6"
                    };
                    assert!(
                        head.contains(&format!("content-range: {range}\r\n")),
                        "{head}"
                    );
                } else if status == 416 {
                    assert!(head.contains("content-range: bytes */6\r\n"), "{head}");
                } else {
                    assert!(!head.contains("content-range:"), "{head}");
                }
                if method == "HEAD" || matches!(status, 304 | 400 | 412) {
                    assert_eq!(
                        pages.load(Ordering::Acquire),
                        before,
                        "no page faults for {status}"
                    );
                }
            };
            check("HEAD", "", 200, b"", "\"v1\"");
            check(
                "GET",
                "If-Match: \"v1\"\r\nRange: bytes=0-2\r\n",
                206,
                b"abc",
                "\"v1\"",
            );
            revision.store(2, Ordering::Release);
            for method in ["GET", "HEAD"] {
                // Same pinned fanout, after mutation and with the old page still cached.
                check(
                    method,
                    "If-Match: \"v1\"\r\nRange: bytes=3-5\r\n",
                    412,
                    b"",
                    "\"v2\"",
                );
                check(
                    method,
                    "If-Match: \"old\"\r\nIf-None-Match: *\r\nRange: bytes=999-\r\nIf-Range: \"v1\"\r\n",
                    412,
                    b"",
                    "\"v2\"",
                );
                check(
                    method,
                    "If-Match: *\r\nIf-None-Match: \"old\", W/\"v2\"\r\nRange: bytes=999-\r\n",
                    304,
                    b"",
                    "\"v2\"",
                );
                check(
                    method,
                    "If-None-Match: *\r\nIf-Range: \"old\"\r\nRange: bytes=0-1\r\n",
                    304,
                    b"",
                    "\"v2\"",
                );
                check(method, "If-Match: W/\"v2\"\r\n", 412, b"", "\"v2\"");
                check(
                    method,
                    "If-None-Match: *\r\nIf-Match: \"old\"\r\n",
                    412,
                    b"",
                    "\"v2\"",
                );
                check(
                    method,
                    "If-None-Match: \"old\"\r\niF-NoNe-MaTcH: W/\"v2\"\r\n",
                    304,
                    b"",
                    "\"v2\"",
                );
                for malformed in [
                    "If-Match: \"v2\", broken\r\n",
                    "If-None-Match: \"v2\", broken\r\n",
                    "If-Match: *\r\nIf-Match: \"v2\"\r\n",
                    "If-None-Match: \"v2\"\r\nIf-None-Match: *\r\n",
                ] {
                    check(method, malformed, 400, b"", "");
                }
            }
            check(
                "HEAD",
                "If-Match: \"old\", \"v2\"\r\nRange: bytes=999-\r\n",
                200,
                b"",
                "\"v2\"",
            );
            check(
                "GET",
                "If-Match: \"v2\"\r\nIf-None-Match: \"old\"\r\nRange: bytes=999-\r\n",
                416,
                b"",
                "\"v2\"",
            );
            check(
                "GET",
                "If-Match: \"old\"\r\nIF-MATCH: \"v2\"\r\nIf-None-Match: \"v1\"\r\nRange: bytes=3-5\r\n",
                206,
                b"XYZ",
                "\"v2\"",
            );
            check(
                "GET",
                "If-Match: *\r\nRange: bytes=0-2\r\nIf-Range: \"v2\"\r\n",
                206,
                b"UVW",
                "\"v2\"",
            );
            check(
                "GET",
                "If-Match: \"v2\"\r\nRange: bytes=0-1\r\nIf-Range: \"v1\"\r\n",
                200,
                b"UVWXYZ",
                "\"v2\"",
            );
            check(
                "GET",
                "If-Match: \"v2\"\r\nIf-None-Match: \"v1\"\r\n",
                200,
                b"UVWXYZ",
                "\"v2\"",
            );
            revision.store(3, Ordering::Release);
            check("HEAD", "If-Match: *\r\n", 502, b"", "");
            assert_eq!(
                pages.load(Ordering::Acquire),
                2,
                "one generated pinned page per version"
            );
        });
        let end = Instant::now() + Duration::from_secs(30);
        while !client.is_finished() {
            assert!(Instant::now() < end, "conditional reads stalled");
            ring.progress().unwrap();
            let mut work = server.handler_mut().poll_background(&mut ring, 8).unwrap();
            work.merge(server.poll(&mut ring, 8).unwrap());
            if !work.runnable {
                ring.wait(Some(work.deadline.unwrap_or(end).min(end)))
                    .unwrap();
            }
        }
        client.join().unwrap();
        stop.store(true, Ordering::Release);
        upstream.join().unwrap();
        server.shutdown(&mut ring).unwrap();
        server.handler_mut().shutdown(&mut ring).unwrap();
        ring.shutdown().unwrap();
    }
}
