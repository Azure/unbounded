// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::http_auth::failure_tests::SEMANTICS;
use crate::tls::{ExpectedPeer, PeerIdentity, TlsContext, TlsProgress, TlsSession};
use cache::adapter_fixture::page_request;
use http::cache_responses::accept;

#[path = "hop_admission.rs"]
mod hop_admission;
#[path = "mixed_version.rs"]
mod mixed_version;
#[path = "peer_recovery.rs"]
mod peer_recovery;

fn peer_policy(node: u8, peer: u8) -> crate::http_auth::Policy {
    let (trust, _) = crate::control::tests::fixture();
    crate::http_auth::Policy {
        members: Arc::new([([peer; 32], ("handler-pod".into(), String::new()))].into()),
        universe: trust.universe,
        node: [node; 32],
    }
}

fn peer_identity(node: u8) -> PeerIdentity {
    PeerIdentity::new(
        &hex(&peer_policy(node, 0).universe),
        &hex(&[node; 32]),
        "handler-pod",
    )
    .unwrap()
}

struct PeerTls {
    client: TlsContext,
    server: TlsContext,
}

impl PeerTls {
    fn new() -> Self {
        let ca = crate::tls::tests::Authority::new();
        Self {
            client: ca.context(&peer_identity(2), false),
            server: ca.context(&peer_identity(3), false),
        }
    }

    fn connect(&self, address: std::net::SocketAddr) -> PeerStream {
        let socket = std::net::TcpStream::connect(address).unwrap();
        socket.set_nonblocking(true).unwrap();
        PeerStream::new(
            TlsSession::client(
                &self.client,
                socket.into(),
                ExpectedPeer::Identity(peer_identity(3)),
            )
            .unwrap(),
            peer_identity(3),
        )
    }
}

// The fixture's blocking facade still drives the native nonblocking TLS session;
// every handshake/read/write is bounded independently of the io_uring worker.
struct PeerStream {
    session: TlsSession,
    end: Instant,
}

impl PeerStream {
    fn new(session: TlsSession, expected: PeerIdentity) -> Self {
        let mut stream = Self {
            session,
            end: deadline(),
        };
        assert_eq!(stream.progress(TlsSession::handshake).unwrap(), Some(()));
        assert_eq!(stream.session.peer_identity(), Some(&expected));
        stream
    }

    fn progress<T>(
        &mut self,
        mut operation: impl FnMut(&mut TlsSession) -> io::Result<TlsProgress<T>>,
    ) -> io::Result<Option<T>> {
        loop {
            if Instant::now() >= self.end {
                return Err(io::ErrorKind::TimedOut.into());
            }
            match operation(&mut self.session)? {
                TlsProgress::Complete(value) => return Ok(Some(value)),
                TlsProgress::Eof => return Ok(None),
                TlsProgress::WantRead | TlsProgress::WantWrite => {
                    thread::sleep(Duration::from_millis(1))
                }
            }
        }
    }
}

impl std::io::Read for PeerStream {
    fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        Ok(self.progress(|session| session.read(bytes))?.unwrap_or(0))
    }
}

impl std::io::Write for PeerStream {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        self.progress(|session| session.write(bytes))?
            .ok_or_else(|| io::ErrorKind::UnexpectedEof.into())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

fn request(stream: &mut (impl std::io::Read + ?Sized)) -> String {
    let mut bytes = Vec::new();
    while !bytes.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        stream.read_exact(&mut byte).unwrap();
        bytes.push(byte[0]);
        assert!(
            bytes.len() <= 8192 + 65536 + 17,
            "HTTP fixture header budget exceeded"
        );
    }
    String::from_utf8(bytes).unwrap()
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
        routing_algorithm: Some(1),
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
            cursor,
            origin: true,
            exhausted: false,
        }))),
        ..Provider::new(Backend::new("127.0.0.1:1", "test-origin").unwrap())
    };
    let first = provider.network_scope([1; 32]).unwrap();
    assert_eq!(first.version, 1);
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
    let routing = prepared.volumes()[0].routing().clone();
    let target = (0..)
        .map(|n| format!("/health-{n}"))
        .find(|t| routing.start(t).owner == 1)
        .unwrap();
    let mut provider = Provider {
        peers: Rc::new(RefCell::new(BTreeMap::from([(
            "03".repeat(32),
            Rc::new(RefCell::new(Peer::from_endpoint(
                prepared.peers()[&"03".repeat(32)].clone(),
            ))),
        )]))),
        routing: Some(routing.clone()),
        ..Provider::new(prepared.volumes()[0].backend().clone())
    };
    let state = Rc::new(RefCell::new(RouteState {
        cursor: routing.start(&target),
        origin: true,
        exhausted: false,
    }));
    provider = provider.routed(Some(state.clone()));
    let error = {
        let mut peer = provider.peer.as_ref().unwrap().borrow_mut();
        peer.http.breaker.try_acquire().unwrap().failure();
        peer.http.connection().err().unwrap()
    };
    assert_eq!(error.io_kind(), io::ErrorKind::WouldBlock);
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
        assert!(provider.owners.borrow().is_empty());
    }
    let route = AttemptRoute {
        cursor: state.borrow().cursor.clone(),
        candidate: 1,
        endpoint: provider
            .peer
            .as_ref()
            .unwrap()
            .borrow()
            .http
            .endpoint
            .address
            .tcp()
            .unwrap(),
        final_hop: true,
        context: "a".repeat(96),
    };
    use crate::outcome::{Cause, Failure, Phase, Transport};
    let evidence = Failure {
        endpoint: route.endpoint.into(),
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
            response: Default::default(),
            identity: route.cursor.identity,
            candidate: 1,
            reason,
            evidence: crate::outcome::PeerEvidence::from_failure(&evidence),
        };
        let decoded = PeerFailure::decode(&failure.encode()).unwrap();
        assert_eq!(failure, decoded);
        let mut attempt = Some(Attempt {
            route: route.clone(),
            owner: Some(
                provider
                    .owners
                    .borrow_mut()
                    .acquire_final(route.cursor.identity, 1, routing.final_peer(&route.cursor))
                    .unwrap(),
            ),
        });
        let error = provider.reported(decoded, &mut attempt).unwrap_err();
        assert_eq!(semantic_failure(&error), Some(failure));
        assert!(provider.peer_failed(error).is_err());
        assert_eq!(state.borrow().cursor.attempt, 0);
        assert!(!provider.owners.borrow().blocked(route.cursor.identity, 1));
        assert!(
            provider
                .owners
                .borrow()
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
                    response: Default::default(),
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
    provider = provider.routed(Some(joined.clone()));
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
    provider = provider.routed(Some(relay.clone()));
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
fn interleaved_request_routes_share_peers_without_sharing_cursors() {
    let (trust, config) = crate::control::tests::fixture();
    let prepared = trust
        .prepare(crate::control::proto::Configuration {
            contents: Some(crate::control::proto::configuration::Contents::Snapshot(
                config,
            )),
        })
        .unwrap();
    let routing = prepared.volumes()[0].routing().clone();
    let target = (0..)
        .map(|n| format!("/interleaved-{n}"))
        .find(|t| routing.start(t).owner == 1)
        .unwrap();
    let peer = Rc::new(RefCell::new(Peer::from_endpoint(
        prepared.peers()[&"03".repeat(32)].clone(),
    )));
    let provider = Provider {
        routing: Some(routing.clone()),
        peers: Rc::new(RefCell::new(BTreeMap::from([(
            "03".repeat(32),
            peer.clone(),
        )]))),
        ..Provider::new(prepared.volumes()[0].backend().clone())
    };
    let state = || {
        Some(Rc::new(RefCell::new(RouteState {
            cursor: routing.start(&target),
            origin: true,
            exhausted: false,
        })))
    };
    let mut first = provider.routed(state());
    let second = provider.routed(state());
    let scope = second.network_scope([42; 32]).unwrap();
    assert_eq!(first.network_scope([42; 32]), Some(scope.clone()));
    assert!(Rc::ptr_eq(
        first.peer.as_ref().unwrap(),
        second.peer.as_ref().unwrap()
    ));
    assert_eq!(
        provider.peers.borrow().len(),
        1,
        "selection never removes shared peers"
    );
    let attempt = first.attempt("a".repeat(96)).unwrap().unwrap();
    let failure = AttemptFailure {
        route: attempt.route.clone(),
        evidence: None,
        reported: true,
    };
    drop(attempt);
    assert!(first.peer_failed(io::Error::other(failure).into()).unwrap());
    assert!(!first.has_peer(), "successor is local");
    assert!(second.has_peer());
    assert_eq!(second.network_scope([42; 32]), Some(scope));
    assert_eq!(second.active.as_ref().unwrap().borrow().cursor.attempt, 0);
    assert_eq!(
        provider.active.as_ref().map(|s| s.borrow().cursor.attempt),
        None
    );

    // Admission and breaker health remain shared even though cursors are private.
    peer.borrow_mut().http.limit = 1;
    let permit = peer.borrow().http.breaker.try_acquire().unwrap();
    assert_eq!(
        second.peer.as_ref().unwrap().borrow().http.breaker.active(),
        1
    );
    permit.failure();
    assert!(
        !second
            .peer
            .as_ref()
            .unwrap()
            .borrow()
            .http
            .breaker
            .available()
    );
    assert!(Rc::ptr_eq(&first.owners, &second.owners));
    // A metadata retry must not set the next page's attempt, and one page's
    // retries must not advance metadata or any other page's owner chain.
    second.active.as_ref().unwrap().borrow_mut().cursor.attempt = 1;
    let namespace = prepared.volumes()[0].namespace(&prepared.config_snapshot().universe);
    let page_key = |offset| {
        cache::PeerDescriptor::page(
            &target,
            cache::PeerPage::new(
                offset,
                2 * BUFFER_SIZE as u64,
                crate::metadata::Checksum([9; 32]),
            ),
        )
        .key(namespace)
        .unwrap()
    };
    let page = second.page_provider(&page_key(0)).unwrap();
    let other = second.page_provider(&page_key(BUFFER_SIZE as u64)).unwrap();
    assert_eq!(page.active.as_ref().unwrap().borrow().cursor.attempt, 0);
    assert_eq!(other.active.as_ref().unwrap().borrow().cursor.attempt, 0);
    assert_eq!(*page.chain.borrow(), Chain::default());
    page.chain.borrow_mut().forward().unwrap();
    assert_eq!(*other.chain.borrow(), Chain::default());
    assert_eq!(*second.chain.borrow(), Chain::default());
    page.active.as_ref().unwrap().borrow_mut().cursor.attempt = 1;
    assert_eq!(other.active.as_ref().unwrap().borrow().cursor.attempt, 0);
    assert_eq!(second.active.as_ref().unwrap().borrow().cursor.attempt, 1);
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

fn hot_cold_head_and_peer_response_with_pinned_payloads(ring: &mut Ring) {
    use std::io::Read;
    trait HttpStream: Read + std::io::Write {}
    impl HttpStream for std::net::TcpStream {}
    impl HttpStream for PeerStream {}
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    let backend = Backend::new(&origin.local_addr().unwrap().to_string(), "test-origin").unwrap();
    let origin_thread = thread::spawn(move || {
        let mut socket = accept(&origin);
        assert!(request(&mut socket).starts_with("HEAD /pinned HTTP/1.1\r\n"));
        write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 7\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"1234567")).unwrap();
    });
    let mut handler = Handler::new(cache(&backend, 1), backend.clone());
    let client_handler =
        Handler::shared(handler.cache.clone(), backend.clone(), backend.namespace());
    let client_listener = http::Listener::bind(
        "127.0.0.1:0".parse().unwrap(),
        std::num::NonZeroU32::new(16).unwrap(),
    )
    .unwrap();
    let client_address = client_listener.local_addr().unwrap();
    let mut client_server =
        http::Server::new(client_listener, client_handler, http::Config::default());
    handler.upstream.authentication = Some(peer_policy(3, 2));
    let tls = PeerTls::new();
    let mut listener = http::Listener::bind(
        "127.0.0.1:0".parse().unwrap(),
        std::num::NonZeroU32::new(16).unwrap(),
    )
    .unwrap();
    listener.set_tls(tls.server.clone(), ExpectedPeer::Identity(peer_identity(2)));
    let address = listener.local_addr().unwrap();
    let mut server = http::Server::new(listener, handler, http::Config::default());
    let mut pinned = Vec::new();
    while let Ok(fill) = ring.pool().private_fill() {
        pinned.push(fill);
    }
    let client = thread::spawn(move || {
        for peer in [false, false, true, true] {
            let mut socket: Box<dyn HttpStream> = if peer {
                Box::new(tls.connect(address))
            } else {
                let socket = std::net::TcpStream::connect(client_address).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(5)))
                    .unwrap();
                Box::new(socket)
            };
            let mut fields = Vec::new();
            if peer {
                fields.push((
                    "X-Racer-Fault".to_owned(),
                    hex(b"RB01\x88\x13\0\0RD01\0/pinned").into_bytes(),
                ));
                fields.push(("X-Racer-Volume".to_owned(), b"test-volume".to_vec()));
                let binding = crate::authorization::binding(
                    b"RB01\x88\x13\0\0RD01\0/pinned",
                    &Default::default(),
                );
                fields.push((
                    "X-Racer-Attempt".into(),
                    format!("{}{}", hex(binding.as_bytes()), "0".repeat(32)).into_bytes(),
                ));
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
        client_server.poll(ring, 16).unwrap();
        server.poll(ring, 16).unwrap();
        thread::yield_now();
    }
    client.join().unwrap();
    origin_thread.join().unwrap();
    assert!(ring.pool().private_fill().is_err());
    drop(pinned);
    client_server.shutdown(ring).unwrap();
    client_server.handler_mut().shutdown(ring).unwrap();
    server.shutdown(ring).unwrap();
    server.handler_mut().shutdown(ring).unwrap();
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

#[path = "authorization.rs"]
mod authorization;
