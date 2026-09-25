// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::control::product_routing::tests::volume;
use crate::outcome::{Cause, Failure, Phase, Transport};

#[test]
fn stalled_final_owner_headers_and_body_advance_within_original_caller_deadline() {
    let Some(mut ring) = crate::conformance::kernel_ring(8, uring::Config::default()) else {
        return;
    };
    for payload in [false, true] {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let ca = crate::tls::tests::Authority::new();
        let id =
            |m| PeerIdentity::new(&hex(&[1; 32]), &format!("{m:064x}"), "timeout-pod").unwrap();
        let local = id(0);
        let remote = id(1);
        let tls = ca.context(&remote);
        let credentials = crate::control::credentials::Provider::for_test(
            local.clone(),
            Arc::new(ca.context(&local)),
        );
        let (stop, stopped) = std::sync::mpsc::channel();
        let server = thread::spawn(move || {
            let socket = accept(&listener);
            socket.set_nonblocking(true).unwrap();
            let mut stream = PeerStream::new(
                TlsSession::server(&tls, socket.into(), ExpectedPeer::Identity(local.clone()))
                    .unwrap(),
                local,
            );
            let wire = request(&mut stream);
            assert!(wire.starts_with("GET / "));
            if payload {
                // Complete valid headers, then stall the actual response body.
                write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nX-Racer-Crc64: {:016x}\r\n\r\na", crate::conformance::etag(b"abc"), crate::allocator::crc64(b"abc")).unwrap();
            }
            stopped.recv_timeout(Duration::from_secs(25)).unwrap();
        });
        let origin = TcpListener::bind("127.0.0.1:0").unwrap();
        let backend =
            Backend::new(&origin.local_addr().unwrap().to_string(), "product-test").unwrap();
        let origin_thread = thread::spawn(move || {
            let mut socket = accept(&origin);
            let wire = request(&mut socket);
            assert!(wire.starts_with(if payload {
                "GET /stalled-owner "
            } else {
                "HEAD /stalled-owner "
            }));
            if payload {
                write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Range: bytes 0-2/3\r\nETag: {}\r\nConnection: close\r\n\r\nabc", crate::conformance::etag(b"abc")).unwrap();
            } else {
                write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"abc")).unwrap();
            }
        });
        let mut handler = Handler::new(cache(&backend, 1), backend);
        let v = volume(1, 3, vec![0, 1, 2], 0, vec![1, 0, 2]);
        handler.set_routing(
            Arc::new(crate::routing::Routing::new(&[1; 32], &v).unwrap()),
            v.peers
                .iter()
                .map(|id| (id.clone(), Peer::new(&address.to_string(), None).unwrap()))
                .collect(),
        );
        handler.set_attempt_policy(3).unwrap();
        handler.set_peer_tls(
            "product-test",
            credentials,
            &[(remote.node.clone(), remote)].into(),
        );
        let descriptor = if payload {
            cache::PeerDescriptor::page(
                "/stalled-owner",
                cache::PeerPage::new(
                    0,
                    3,
                    crate::metadata::Checksum(*blake3::hash(b"abc").as_bytes()),
                ),
            )
        } else {
            cache::PeerDescriptor::metadata("/stalled-owner")
        };
        let key = descriptor.key(handler.namespace).unwrap();
        let mut p = handler.upstream.page_provider(&key).unwrap();
        let start = Instant::now();
        let caller = start + TIMEOUT;
        let cap = p.candidate_deadline(caller);
        assert!(cap > start + Duration::from_secs(9));
        assert!(cap < start + Duration::from_secs(10));
        assert!(p.private_service_deadline(p.service_end(cap), cap));
        let mut fault = handler
            .cache
            .borrow_mut()
            .peer_fault_in::<Provider>(&cache::Context::new(handler.namespace), descriptor, caller)
            .unwrap();
        let result = loop {
            assert!(
                Instant::now() < caller,
                "owner fallback exceeded caller deadline"
            );
            ring.progress().unwrap();
            handler.cache.borrow_mut().poll(&mut ring, 64).unwrap();
            match handler
                .cache
                .borrow_mut()
                .poll_value(fault, &mut ring, &mut p)
            {
                Ok(cache::Progress::Pending { fault: next, .. }) => fault = next,
                other => break other,
            }
            thread::sleep(Duration::from_millis(1));
        };
        stop.send(()).unwrap();
        server.join().unwrap();
        let value = match result {
            Ok(cache::Progress::Ready(value)) => value,
            Err(error) => panic!(
                "stalled final owner payload={payload} attempt={} error={error:?}",
                p.active.as_ref().unwrap().borrow().cursor.attempt
            ),
            _ => panic!("unexpected pending result"),
        };
        assert_eq!(value.len(), if payload { 3 } else { cache::METADATA_SIZE });
        assert_eq!(p.active.as_ref().unwrap().borrow().cursor.attempt, 1);
        assert_eq!(p.caller_deadline, Some(caller));
        assert!(Instant::now() >= cap && Instant::now() < caller);
        drop(value);
        origin_thread.join().unwrap();
        handler.shutdown(&mut ring).unwrap();
    }
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

fn provider(local: u32) -> Provider {
    let volume = volume(2, 2, (0..4).collect(), local, vec![3, 0]);
    let routing = Arc::new(crate::routing::Routing::new(&[1; 32], &volume).unwrap());
    let mut provider = Provider::new(Backend::new("127.0.0.1:1", "product-test").unwrap());
    provider.peers = Rc::new(RefCell::new(
        volume
            .peers
            .iter()
            .enumerate()
            .map(|(i, id)| {
                (
                    id.clone(),
                    Rc::new(RefCell::new(
                        Peer::new(&format!("127.0.0.1:{}", 1000 + i), None).unwrap(),
                    )),
                )
            })
            .collect(),
    ));
    provider.routing = Some(routing);
    provider.routed(provider.route_state(None, &[0; 32], false).unwrap())
}

#[test]
fn intermediate_private_timeout_repairs_but_caller_expiry_never_does() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, uring::Config::default()) else {
        return;
    };
    for (private, late_poll) in [(true, false), (false, false), (true, true)] {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let ca = crate::tls::tests::Authority::new();
        let local = peer_identity(2);
        let remote = peer_identity(3);
        let server_tls = ca.context(&remote);
        let credentials = crate::control::credentials::Provider::for_test(
            local.clone(),
            Arc::new(ca.context(&local)),
        );
        let (stop, stopped) = std::sync::mpsc::channel();
        let (ready, started) = std::sync::mpsc::channel();
        let server = thread::spawn(move || {
            let socket = accept(&listener);
            socket.set_nonblocking(true).unwrap();
            let mut stream = PeerStream::new(
                TlsSession::server(
                    &server_tls,
                    socket.into(),
                    ExpectedPeer::Identity(local.clone()),
                )
                .unwrap(),
                local,
            );
            request(&mut stream);
            ready.send(()).unwrap();
            stopped.recv_timeout(Duration::from_secs(5)).unwrap();
        });
        let backend = Backend::new("127.0.0.1:1", "product-test").unwrap();
        let handler = Handler::new(cache(&backend, 1), backend);
        let page = page_request(
            &mut handler.cache.borrow_mut(),
            &mut ring,
            "/intermediate-timeout",
        );
        let mut p = provider(0);
        let peer = p.peer.as_ref().unwrap().clone();
        peer.borrow_mut().http = HttpOrigin::peer(Endpoint::parse(&address.to_string()).unwrap());
        peer.borrow_mut().http.set_tls(credentials, remote);
        let start = Instant::now();
        let service = start + Duration::from_millis(800);
        let caller = if private {
            start + Duration::from_millis(1600)
        } else {
            service
        };
        p.caller_deadline = Some(caller);
        let (authority, destination) = destination(&ring, *page.key());
        let mut exchange = p
            .http_peer_attempt(
                UpstreamRequest::PeerPage(page),
                Some(destination),
                service,
                None,
            )
            .unwrap();
        let mut delayed = false;
        let error = loop {
            ring.progress().unwrap();
            if late_poll && !delayed && started.try_recv().is_ok() {
                thread::sleep(
                    (caller + Duration::from_millis(20)).saturating_duration_since(Instant::now()),
                );
                delayed = true;
            }
            match p.poll(exchange, &mut ring) {
                Ok(ExchangeProgress::Pending { exchange: next, .. }) => exchange = next,
                Err(error) => break error,
                _ => panic!("stalled intermediate returned success"),
            }
            assert!(Instant::now() < start + Duration::from_secs(4));
            thread::sleep(Duration::from_millis(1));
        };
        stop.send(()).unwrap();
        server.join().unwrap();
        let can_repair = private && !late_poll;
        assert_eq!(
            error
                .attempt_failure()
                .unwrap()
                .evidence
                .as_ref()
                .unwrap()
                .cause,
            if can_repair {
                Cause::ServiceTimeout
            } else {
                Cause::CallerDeadline
            }
        );
        let chain = *p.chain.borrow();
        if can_repair {
            assert!(p.peer_failed(error).unwrap());
            assert!(p.repaired_candidate());
            assert_eq!(p.active.as_ref().unwrap().borrow().cursor.path, [0, 1, 3]);
        } else {
            assert!(p.peer_failed(error).is_err());
            assert_eq!(p.active.as_ref().unwrap().borrow().cursor.failed, u32::MAX);
        }
        assert_eq!(p.active.as_ref().unwrap().borrow().cursor.attempt, 0);
        assert_eq!(p.caller_deadline, Some(caller));
        assert_eq!(*p.chain.borrow(), chain);
        drop(authority);
    }
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

fn failure(provider: &mut Provider, cause: Cause, initiated: bool) -> cache::Error {
    let a = provider.attempt("a".repeat(96)).unwrap().unwrap();
    AttemptFailure {
        evidence: Some(Failure {
            endpoint: a.route.endpoint.into(),
            transport: Transport::Http,
            phase: Phase::Connect,
            cause,
            initiated,
            kind: io::ErrorKind::ConnectionRefused,
            message: "injected peer failure".into(),
        }),
        route: a.route,
        reported: false,
    }
    .into()
}

#[test]
fn intermediate_repair_preserves_owner_budget_and_rank() {
    let mut p = provider(0);
    assert_eq!(p.active.as_ref().unwrap().borrow().cursor.path, [0, 2, 3]);
    p.receive_reserve(8).unwrap();
    p.chain.borrow_mut().forward().unwrap();
    let chain = *p.chain.borrow();
    let rank = p.receive_rank;
    let end = p.candidate_deadline(deadline());
    for cause in [
        Cause::LocalPressure,
        Cause::Cancelled,
        Cause::CallerDeadline,
        Cause::Protocol,
        Cause::BreakerRejected,
    ] {
        let error = failure(&mut p, cause, true);
        assert!(p.peer_failed(error).is_err());
        assert!(!p.repaired_candidate());
    }
    let error = failure(&mut p, Cause::Connection, false);
    assert!(p.peer_failed(error).is_err());
    let error = failure(&mut p, Cause::Connection, true);
    assert!(p.peer_failed(error).unwrap());
    assert!(p.repaired_candidate());
    let state = p.active.as_ref().unwrap().borrow();
    assert_eq!(state.cursor.path, [0, 1, 3]);
    assert_eq!(state.cursor.attempt, 0);
    assert_eq!(p.routing.as_ref().unwrap().destination(&state.cursor), 3);
    assert_eq!(*p.chain.borrow(), chain);
    assert_eq!(p.receive_rank, rank);
    assert!(!p.owners.borrow().blocked(state.cursor.identity, 3));
    assert!(end > Instant::now());
    drop(state);
    let error = failure(&mut p, Cause::Connection, true);
    assert!(
        p.peer_failed(error).is_err(),
        "second repair must fail closed"
    );
}

#[test]
fn relay_repairs_locally_but_only_origin_changes_owner() {
    let source = provider(0);
    let mut relay = provider(0);
    relay.active.as_ref().unwrap().borrow_mut().origin = false;
    let error = failure(&mut relay, Cause::Connection, true);
    assert!(relay.peer_failed(error).unwrap());
    assert_eq!(relay.active.as_ref().unwrap().borrow().cursor.attempt, 0);
    // A report about a downstream owner cannot be interpreted as a failure of
    // our immediate intermediate. The origin changes the physical candidate.
    let mut origin = source;
    let a = origin.attempt("b".repeat(96)).unwrap().unwrap();
    assert!(
        origin
            .peer_failed(
                AttemptFailure {
                    route: a.route,
                    evidence: None,
                    reported: true
                }
                .into()
            )
            .unwrap()
    );
    assert!(!origin.repaired_candidate());
    assert_eq!(origin.active.as_ref().unwrap().borrow().cursor.attempt, 1);
    assert!(!origin.has_peer());
}

#[test]
fn only_fresh_transport_evidence_allows_new_page_to_avoid_failed_intermediate() {
    let p = provider(0);
    let peer = p.peer.as_ref().unwrap().clone();
    peer.borrow_mut()
        .http
        .breaker
        .try_acquire()
        .unwrap()
        .failure();
    let first = p.page_provider(&[0; 32]).unwrap();
    assert_eq!(
        first.active.as_ref().unwrap().borrow().cursor.failed,
        u32::MAX
    );
    peer.borrow_mut().repair_evidence = Some(crate::environment::now());
    let second = p.page_provider(&[0; 32]).unwrap();
    assert_eq!(
        second.active.as_ref().unwrap().borrow().cursor.path,
        [0, 1, 3]
    );
    assert_eq!(*second.chain.borrow(), Chain::default());
    peer.borrow_mut().repair_evidence = Some(crate::environment::now() - COOLDOWN);
    let third = p.page_provider(&[0; 32]).unwrap();
    assert_eq!(
        third.active.as_ref().unwrap().borrow().cursor.failed,
        u32::MAX
    );
    assert_eq!(
        first.active.as_ref().unwrap().borrow().cursor.failed,
        u32::MAX
    );
}

#[test]
fn stale_http_completion_cannot_erase_newer_repair_evidence() {
    let mut p = provider(0);
    let peer = p.peer.as_ref().unwrap().clone();
    let now = Rc::new(std::cell::Cell::new(crate::environment::now()));
    let clock = now.clone();
    peer.borrow_mut().http.breaker =
        crate::breaker::CircuitBreaker::with_clock(COOLDOWN, move || clock.get());
    let old_success = peer.borrow().http.breaker.try_acquire().unwrap();
    let old_failure = peer.borrow().http.breaker.try_acquire().unwrap();
    let failure = peer.borrow().http.breaker.try_acquire().unwrap();
    peer.borrow_mut().record_repair_failure(&failure);
    let evidence = peer.borrow().repair_evidence;
    assert!(evidence.is_some());
    failure.failure();
    let mut validation = Exchange::ValidateHttp(HttpValidation {
        connection: None,
        permit: Some(old_success),
        attempt: None,
    });
    p.peer_validated(&mut validation, true);
    assert_eq!(peer.borrow().repair_evidence, evidence);
    assert_eq!(
        peer.borrow().http.breaker.status(),
        crate::breaker::Status::Open
    );
    let next = p.page_provider(&[0; 32]).unwrap();
    assert_eq!(
        next.active.as_ref().unwrap().borrow().cursor.path,
        [0, 1, 3]
    );
    now.set(now.get() + COOLDOWN);
    let fresh = peer.borrow().http.breaker.try_acquire().unwrap();
    let mut validation = Exchange::ValidateHttp(HttpValidation {
        connection: None,
        permit: Some(fresh),
        attempt: None,
    });
    p.peer_validated(&mut validation, true);
    assert!(peer.borrow().repair_evidence.is_none());
    assert_eq!(
        peer.borrow().http.breaker.status(),
        crate::breaker::Status::Closed
    );
    // An old failure cannot reintroduce evidence after a successful new probe.
    peer.borrow_mut().record_repair_failure(&old_failure);
    old_failure.failure();
    assert!(peer.borrow().repair_evidence.is_none());
    assert_eq!(
        peer.borrow().http.breaker.status(),
        crate::breaker::Status::Closed
    );
    assert_eq!(
        p.page_provider(&[0; 32])
            .unwrap()
            .active
            .as_ref()
            .unwrap()
            .borrow()
            .cursor
            .failed,
        u32::MAX
    );
}

#[test]
fn four_hop_eight_candidate_budget_reserves_return_slack_once() {
    let Some(mut ring) = crate::conformance::kernel_ring(2, uring::Config::default()) else {
        return;
    };
    let backend = Backend::new("127.0.0.1:1", "product-test").unwrap();
    let handler = Handler::new(cache(&backend, 1), backend);
    let page = page_request(&mut handler.cache.borrow_mut(), &mut ring, "/budget");
    let request = UpstreamRequest::PeerPage(page);
    let p = provider(0);
    let start = crate::environment::now();
    let cap = candidate_end(start, start + TIMEOUT, 8);
    let mut inherited = cap;
    for hop in 1..=4 {
        let service = p.service_end(inherited);
        assert_eq!(
            service, inherited,
            "exchange cannot charge a second reserve"
        );
        let wire = p.budget_wire(&request, service).unwrap();
        let (_, budget) = budget_descriptor(&wire).unwrap();
        let child = crate::environment::now() + budget;
        // Relative millisecond framing rounds down; allow bounded local test work.
        assert!(child <= inherited);
        assert!(child > cap - RETURN_SLACK * hop - Duration::from_millis(100));
        inherited = child;
    }
    assert!(inherited - RETURN_SLACK > start + Duration::from_secs(1));
    assert_eq!(p.chain.borrow().hops, cache::peer_wire::MAX_HOPS - 4);
    assert!(p.chain.borrow().work < cache::peer_wire::MAX_WORK);
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

#[test]
fn product_wire_binds_candidate_and_authentication_across_repair() {
    let Some(mut ring) = crate::conformance::kernel_ring(2, uring::Config::default()) else {
        return;
    };
    let backend = Backend::new("127.0.0.1:1", "product-test").unwrap();
    let mut handler = Handler::new(cache(&backend, 1), backend);
    handler.upstream = provider(0);
    let page = page_request(&mut handler.cache.borrow_mut(), &mut ring, "/product-wire");
    let request = UpstreamRequest::PeerPage(page.clone());
    let mut p = handler.upstream.page_provider(page.key()).unwrap();
    let error = failure(&mut p, Cause::Connection, true);
    assert!(p.peer_failed(error).unwrap());
    let wire = p.budget_wire(&request, deadline()).unwrap();
    let (cursor, decoded) = routed_descriptor(&wire).unwrap();
    let cursor = cursor.unwrap();
    assert_eq!(&budget_descriptor(&wire).unwrap().0[..4], b"RR01");
    assert_eq!(cursor.position, 1);
    assert_eq!(decoded.key(handler.namespace).unwrap(), *page.key());
    handler.upstream = provider(1);
    let received = handler.peer_provider(&wire).unwrap();
    assert_eq!(received.active.as_ref().unwrap().borrow().cursor, cursor);
    let data = crate::origin_data::OriginData::new(b"Bearer product-wire").unwrap();
    let envelope = crate::origin_data::rdma_envelope(&wire, &data).unwrap();
    let (decoded, forwarded) = crate::origin_data::rdma_decode(&envelope).unwrap();
    assert_eq!(decoded, wire);
    assert_eq!(forwarded.as_bytes(), data.as_bytes());
    let mut foreign = wire.clone();
    foreign[38..42].copy_from_slice(&0u32.to_le_bytes());
    assert!(handler.peer_provider(&foreign).is_err());
    for end in 0..wire.len() - "/product-wire".len() {
        assert!(routed_descriptor(&wire[..end]).is_err());
    }
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}

#[test]
fn four_hop_product_http_stream_and_intermediate_repair() {
    product_http_stream(1, BUFFER_SIZE + 123, &[false, true]);
}

#[test]
fn cold_four_hop_product_with_eight_distinct_candidates() {
    product_http_stream(7, 4096, &[false]);
    product_http_stream(8, 4096, &[false]);
}

fn product_http_stream(candidate_width: u32, body_len: usize, repairs: &[bool]) {
    use std::io::Read;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    let Some(mut ring) = crate::conformance::kernel_ring(12, uring::Config::default()) else {
        return;
    };
    let graph = crate::product::Product::new(50, 200).unwrap();
    let target = (1..10_000)
        .find(|&t| graph.route(0, t).unwrap().len() == 5)
        .unwrap();
    let healthy = graph.route(0, target).unwrap();
    let at = 1;
    let r = crate::routing::Routing::new(
        &[1; 32],
        &volume(50, 200, (0..10_000).collect(), healthy[at], vec![target]),
    )
    .unwrap();
    let origin_route = crate::routing::Routing::new(
        &[1; 32],
        &volume(50, 200, (0..10_000).collect(), 0, vec![target]),
    )
    .unwrap();
    let mut c = origin_route.start_key(&[0; 32]);
    c.position = at as u8;
    let repaired = r.repair(&c).unwrap().path;
    assert_eq!(repaired.len(), 5);
    assert_eq!(&repaired[..=at], &healthy[..=at]);
    let candidates: Vec<_> = std::iter::once(target)
        .chain((0..10_000).filter(|&m| m != target))
        .take(candidate_width as usize)
        .collect();
    assert_eq!(
        candidates
            .iter()
            .collect::<std::collections::BTreeSet<_>>()
            .len(),
        candidate_width as usize
    );
    for &repair in repairs {
        let path = if repair { &repaired } else { &healthy };
        let body: Arc<Vec<u8>> = Arc::new((0..body_len).map(|i| (i % 251) as u8).collect());
        let etag = crate::conformance::etag(&body);
        let origin = TcpListener::bind("127.0.0.1:0").unwrap();
        origin.set_nonblocking(true).unwrap();
        let backend =
            Backend::new(&origin.local_addr().unwrap().to_string(), "product-test").unwrap();
        let stop = Arc::new(AtomicBool::new(false));
        let gets = Arc::new(AtomicUsize::new(0));
        let stopping = stop.clone();
        let served = gets.clone();
        let source_body = body.clone();
        let end = Instant::now() + Duration::from_secs(20);
        let origin_thread = thread::spawn(move || {
            let mut tasks = Vec::new();
            while !stopping.load(Ordering::Acquire) {
                assert!(Instant::now() < end, "product origin deadline");
                let (mut socket, _) = match origin.accept() {
                    Ok(v) => v,
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    Err(e) => panic!("{e}"),
                };
                let body = source_body.clone();
                let etag = etag.clone();
                let served = served.clone();
                tasks.push(thread::spawn(move || {
                    socket.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
                    socket.set_write_timeout(Some(Duration::from_secs(10))).unwrap();
                    let headers = request(&mut socket);
                    if candidate_width >= 7 {
                        // Leave real backend work in the smallest supported
                        // candidate window, after four forwarding reserves.
                        thread::sleep(Duration::from_millis(200));
                    }
                    if headers.starts_with("HEAD ") {
                        write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: {etag}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", body.len()).unwrap();
                    } else {
                        let range = headers.lines().find_map(|l| l.strip_prefix("Range: bytes=")).unwrap();
                        let (start, last) = range.split_once('-').unwrap();
                        let (start, last): (usize, usize) = (start.parse().unwrap(), last.parse().unwrap());
                        assert!(headers.contains(&format!("If-Match: {etag}\r\n")));
                        served.fetch_add(1, Ordering::Relaxed);
                        write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes {start}-{last}/{}\r\nETag: {etag}\r\nConnection: close\r\n\r\n", last-start+1, body.len()).unwrap();
                        socket.write_all(&body[start..=last]).unwrap();
                    }
                }));
            }
            for task in tasks {
                task.join().unwrap();
            }
        });
        let ca = crate::tls::tests::Authority::new();
        let id = |member: u32| {
            PeerIdentity::new(&hex(&[1; 32]), &format!("{member:064x}"), "product-pod").unwrap()
        };
        let mut listeners: BTreeMap<_, _> = path
            .iter()
            .map(|&member| {
                (
                    member,
                    http::Listener::bind(
                        "127.0.0.1:0".parse().unwrap(),
                        std::num::NonZeroU32::new(32).unwrap(),
                    )
                    .unwrap(),
                )
            })
            .collect();
        let addresses: BTreeMap<_, _> = listeners
            .iter()
            .map(|(&member, l)| (member, l.local_addr().unwrap()))
            .collect();
        let dead = TcpListener::bind("127.0.0.1:0").unwrap();
        let dead_address = dead.local_addr().unwrap();
        drop(dead);
        let members: Arc<crate::http_auth::Members> = Arc::new(
            path.iter()
                .map(|&m| {
                    (
                        crate::http_auth::identity_bytes(&id(m).node).unwrap(),
                        ("product-pod".into(), String::new()),
                    )
                })
                .collect(),
        );
        let mut servers = Vec::new();
        for &member in path {
            let mut listener = listeners.remove(&member).unwrap();
            if member != 0 {
                listener.set_tls(
                    ca.context(&id(member)),
                    ExpectedPeer::Universe(hex(&[1; 32])),
                );
            }
            let local_backend = if member == target {
                backend.clone()
            } else {
                Backend::new("127.0.0.1:1", "product-test").unwrap()
            };
            let mut handler = Handler::new(cache(&local_backend, 1), local_backend);
            let v = volume(50, 200, (0..10_000).collect(), member, candidates.clone());
            let routing = Arc::new(crate::routing::Routing::new(&[1; 32], &v).unwrap());
            assert_eq!(routing.candidate_count(), candidate_width);
            let identities: BTreeMap<_, _> = v
                .peers
                .iter()
                .map(|peer| {
                    let member = u32::from_str_radix(peer, 16).unwrap();
                    (peer.clone(), id(member))
                })
                .collect();
            handler.set_routing(
                routing,
                v.peers
                    .iter()
                    .map(|peer| {
                        let member = u32::from_str_radix(peer, 16).unwrap();
                        (
                            peer.clone(),
                            Peer::new(
                                &addresses
                                    .get(&member)
                                    .copied()
                                    .unwrap_or(dead_address)
                                    .to_string(),
                                None,
                            )
                            .unwrap(),
                        )
                    })
                    .collect(),
            );
            handler.set_attempt_policy(candidate_width).unwrap();
            handler.set_authentication(crate::http_auth::Policy {
                universe: [1; 32],
                node: crate::http_auth::identity_bytes(&id(member).node).unwrap(),
                members: members.clone(),
            });
            let credentials = crate::control::credentials::Provider::for_test(
                id(member),
                Arc::new(ca.context(&id(member))),
            );
            handler.set_peer_tls("product-test", credentials, &identities);
            servers.push(http::Server::new(
                listener,
                handler,
                http::Config::default(),
            ));
        }
        let address = addresses[&0];
        let expected = body.clone();
        let client = thread::spawn(move || {
            let mut socket = std::net::TcpStream::connect(address).unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(20)))
                .unwrap();
            write!(
                socket,
                "GET /product-stream HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
            )
            .unwrap();
            let headers = request(&mut socket);
            assert!(headers.starts_with("HTTP/1.1 200"), "{headers}");
            let mut bytes = Vec::new();
            socket.read_to_end(&mut bytes).unwrap();
            assert_eq!(bytes.len(), expected.len());
            assert_eq!(blake3::hash(&bytes), blake3::hash(&expected));
        });
        while !client.is_finished() {
            assert!(Instant::now() < end, "four-hop product stream stalled");
            ring.progress().unwrap();
            for server in &mut servers {
                server.handler_mut().poll_background(&mut ring, 64).unwrap();
                server.poll(&mut ring, 64).unwrap();
            }
            thread::yield_now();
        }
        let result = client.join();
        if result.is_err() {
            eprintln!("product stream repair={repair} path={path:?}");
            for (member, server) in path.iter().zip(&servers) {
                eprintln!(
                    "member={member} metrics={:?}",
                    server.handler().upstream.metrics.values()
                );
            }
        }
        stop.store(true, Ordering::Release);
        origin_thread.join().unwrap();
        for server in &mut servers {
            server.shutdown(&mut ring).unwrap();
            server.handler_mut().shutdown(&mut ring).unwrap();
        }
        result.unwrap();
        assert_eq!(gets.load(Ordering::Relaxed), body_len.div_ceil(BUFFER_SIZE));
    }
    ring.shutdown().unwrap();
    ring.pool().assert_recovered();
}
