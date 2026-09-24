// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

fn static_provider() -> Provider {
    let mut provider = Provider::new(Backend::new("127.0.0.1:1", "hop-admission").unwrap());
    provider.peer = Some(Rc::new(RefCell::new(
        Peer::new("127.0.0.1:2", None).unwrap(),
    )));
    provider
}

#[test]
fn payload_rank_is_frozen_across_retries_and_small_pools_fail_closed() {
    for (capacity, expected) in [(2, 1), (4, 3), (8, 7), (9, 8), (32, 8)] {
        let mut provider = static_provider();
        assert_eq!(provider.receive_reserve(capacity).unwrap(), expected);
        assert_eq!(usize::from(provider.chain.borrow().hops), expected);
        let child = provider.chain.borrow_mut().forward().unwrap().0;
        assert!(usize::from(child) < expected);
        // Reacquisition must not borrow protected capacity while a previous
        // child or canceled transport may still be using its lower rank.
        if child == 0 {
            assert!(provider.receive_reserve(capacity).is_err());
        } else {
            assert_eq!(provider.receive_reserve(capacity).unwrap(), expected);
        }
        assert_eq!(provider.receive_rank, Some(expected));
    }
    let mut tiny = static_provider();
    assert!(tiny.receive_reserve(1).is_err());
    assert_eq!(tiny.chain.borrow().hops, 0);

    let mut received = static_provider();
    received.receive_rank = Some(7);
    received.chain.borrow_mut().hops = 7;
    let before = *received.chain.borrow();
    let error = received.receive_reserve(4).unwrap_err();
    let failure = metric_failure(&error);
    assert_eq!(failure.reason, crate::metrics::HttpErrorReason::Busy);
    assert_eq!(
        failure.pressure,
        Some(crate::metrics::HttpPressure::Admission)
    );
    assert_eq!(*received.chain.borrow(), before);
    assert_eq!(received.receive_rank, Some(7));
    // A local hit needs no receive admission; a local owner can finish even
    // when its incoming allowance exceeds the pool's forwarding capacity.
    received.peer = None;
    assert_eq!(received.receive_reserve(4).unwrap(), 0);
    assert_eq!(*received.chain.borrow(), before);
}

#[test]
fn local_candidate_then_remote_candidate_cannot_reset_receive_rank() {
    let mut provider = static_provider();
    let peer = provider.peer.take();
    assert_eq!(provider.receive_reserve(4).unwrap(), 0);
    assert_eq!(provider.receive_rank, Some(3));
    provider.peer = peer;
    assert_eq!(provider.receive_reserve(4).unwrap(), 3);
    provider.chain.borrow_mut().forward().unwrap();
    provider.chain.borrow_mut().forward().unwrap();
    assert_eq!(provider.receive_reserve(4).unwrap(), 3);
    assert_eq!(provider.chain.borrow().hops, 1);
    // A fresh page is a new resolution; it must not inherit spent hops/rank.
    let mut next = provider.routed(None);
    assert_eq!(next.receive_rank, None);
    assert_eq!(next.receive_reserve(8).unwrap(), 7);
    assert_eq!(provider.receive_rank, Some(3));
}

#[test]
fn flight_rank_covers_metadata_and_both_payload_capacities_without_retry_rebase() {
    for (flights, payloads, expected) in [
        (128, None, 8),
        (4, None, 3),
        (128, Some(4), 3),
        (4, Some(16), 3),
    ] {
        let mut provider = static_provider();
        assert_eq!(
            provider.flight_reserve(flights, payloads).unwrap(),
            expected
        );
        provider.chain.borrow_mut().forward().unwrap();
        assert_eq!(provider.flight_reserve(128, Some(16)).unwrap(), expected);
        assert_eq!(provider.receive_reserve(16).unwrap(), expected);
        assert_eq!(provider.receive_rank, Some(expected));
        let peer = provider.peer.take();
        assert_eq!(provider.flight_reserve(1, None).unwrap(), 0);
        provider.peer = peer;
        assert!(provider.flight_reserve(expected, None).is_err());
        assert_eq!(provider.receive_rank, Some(expected));
        let mut next = provider.routed(None);
        assert_eq!(next.flight_reserve(128, None).unwrap(), 8);
    }
    let mut received = static_provider();
    received.receive_rank = Some(7);
    received.chain.borrow_mut().hops = 7;
    assert!(received.flight_reserve(4, None).is_err());
    assert_eq!(received.chain.borrow().hops, 7);
    received.peer = None;
    assert_eq!(received.flight_reserve(4, None).unwrap(), 0);
}

#[test]
fn local_flight_candidate_freezes_rank_before_remote_reselection() {
    let mut provider = static_provider();
    let peer = provider.peer.take();
    assert_eq!(provider.flight_reserve(128, Some(4)).unwrap(), 0);
    assert_eq!(provider.receive_rank, Some(3));
    provider.peer = peer;
    assert_eq!(provider.flight_reserve(128, Some(16)).unwrap(), 3);
    assert_eq!(provider.receive_reserve(16).unwrap(), 3);
}

#[test]
fn product_payload_chain_keeps_decreasing_rank_and_cancellation_recovers_grants() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, uring::Config::default()) else {
        return;
    };
    let nodes: Vec<_> = (0..4)
        .map(|index| {
            let backend = Backend::new("127.0.0.1:1", "chain").unwrap();
            let mut handler = Handler::new(cache(&backend, 1), backend);
            handler.set_routing(
                chain_routing(index, 4, 2),
                if index < 3 {
                    BTreeMap::from([(
                        format!("{:02x}", index + 3).repeat(32),
                        Peer::new("127.0.0.1:1", None).unwrap(),
                    )])
                } else {
                    BTreeMap::new()
                },
            );
            handler
        })
        .collect();
    let a = &nodes[0];
    let object = "/chain".to_owned();
    let page = page_request(&mut a.cache.borrow_mut(), &mut ring, &object);
    let request = UpstreamRequest::PeerPage(page.clone());
    let mut provider = a
        .upstream
        .routed(a.upstream.route_state(None, page.key(), false).unwrap());
    let pool = ring.pool().clone();
    let mut held = Vec::new();
    let mut blocked = Vec::new();
    let mut flights = Vec::new();
    for (hop, expected) in [3, 2, 1].into_iter().enumerate() {
        assert!(provider.has_peer());
        let rank = provider
            .flight_reserve(pool.flight_capacity(), Some(pool.capacity()))
            .unwrap();
        assert_eq!(rank, expected);
        assert_eq!(provider.receive_reserve(pool.capacity()).unwrap(), rank);
        let key = provider.network_scope([hop as u8; 32]).unwrap();
        flights.push(pool.network_flight_reserved(key, rank).unwrap());
        let mut wait =
            pool.wait_stage_reserved(crate::buffers::Key::new(*page.key()), rank, deadline());
        let std::task::Poll::Ready(Ok(fill)) = wait.poll(std::task::Waker::noop()) else {
            panic!("downstream rank {rank} could not acquire");
        };
        held.push(fill);
        let mut sibling =
            pool.wait_stage_reserved(crate::buffers::Key::new([hop as u8; 32]), rank, deadline());
        assert!(sibling.poll(std::task::Waker::noop()).is_pending());
        blocked.push(sibling);
        let wire = provider.budget_wire(&request, deadline()).unwrap();
        if rank == 1 {
            assert!(provider.receive_reserve(pool.capacity()).is_err());
        } else {
            assert_eq!(provider.receive_reserve(pool.capacity()).unwrap(), rank);
        }
        provider = nodes[hop + 1].peer_provider(&wire).unwrap();
        assert_eq!(provider.receive_rank, Some(rank - 1));
        assert_eq!(
            provider.active.as_ref().unwrap().borrow().cursor.position,
            hop as u8 + 1,
            "product cursor advances without rebasing"
        );
    }
    // The final slot is protected from every forwarding rank. Exhausted cyclic
    // work cannot consume it or create a new chain; owner work still can.
    assert_eq!(provider.receive_reserve(pool.capacity()).unwrap(), 0);
    let rank = provider
        .flight_reserve(pool.flight_capacity(), Some(pool.capacity()))
        .unwrap();
    assert_eq!(rank, 0);
    flights.push(
        pool.network_flight_reserved(provider.network_scope([99; 32]).unwrap(), rank)
            .unwrap(),
    );
    let owner = pool.private_fill().unwrap();
    drop(owner);
    for wait in &mut blocked {
        assert!(wait.poll(std::task::Waker::noop()).is_pending());
    }
    // Release hands the rank-one waiter an exclusive grant. Cancel that grant
    // before polling, then recover all capacity without renewing any budgets.
    drop(held.pop());
    drop(blocked.pop());
    drop((held, blocked, flights));
    pool.assert_recovered();
    ring.shutdown().unwrap();
}

#[test]
fn saturated_three_hop_http_payloads_complete_with_four_buffers() {
    use std::io::Read;
    use std::num::NonZeroU32;
    use std::sync::atomic::{AtomicBool, Ordering};
    let Some(ring) = crate::conformance::kernel_ring(4, uring::Config::default()) else {
        return;
    };
    let mut rings = vec![ring];
    for _ in 1..4 {
        rings.push(crate::conformance::kernel_ring(4, uring::Config::default()).unwrap());
    }
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    origin.set_nonblocking(true).unwrap();
    let backend = Backend::new(&origin.local_addr().unwrap().to_string(), "hop-admission").unwrap();
    let ca = crate::tls::tests::Authority::new();
    let mut listeners: Vec<_> = (0..4)
        .map(|_| {
            http::Listener::bind("127.0.0.1:0".parse().unwrap(), NonZeroU32::new(16).unwrap())
                .unwrap()
        })
        .collect();
    let addresses: Vec<_> = listeners.iter().map(|l| l.local_addr().unwrap()).collect();
    for (index, listener) in listeners.iter_mut().enumerate().skip(1) {
        listener.set_tls(
            ca.context(&peer_identity(index as u8 + 2), false),
            ExpectedPeer::Identity(peer_identity(index as u8 + 1)),
        );
    }
    let mut servers: Vec<_> = listeners
        .into_iter()
        .enumerate()
        .map(|(index, listener)| {
            let backend = if index == 3 {
                backend.clone()
            } else {
                Backend::new("127.0.0.1:1", "hop-admission").unwrap()
            };
            let mut handler = Handler::new(cache(&backend, 1), backend);
            handler.upstream.routing = Some(chain_routing(index, 4, 2));
            if index > 0 {
                handler.set_authentication(peer_policy(index as u8 + 2, index as u8 + 1));
            }
            if index < 3 {
                let peer = peer_identity(index as u8 + 3).node;
                handler.upstream.peers.borrow_mut().insert(
                    peer.clone(),
                    Rc::new(RefCell::new(
                        Peer::new(&addresses[index + 1].to_string(), None).unwrap(),
                    )),
                );
                let id = peer_identity(index as u8 + 2);
                let credentials = crate::control::credentials::Provider::for_test(
                    id.clone(),
                    Arc::new(ca.context(&id, false)),
                );
                handler.set_peer_tls(
                    "v1",
                    credentials,
                    &BTreeMap::from([(peer, peer_identity(index as u8 + 3))]),
                );
            }
            http::Server::new(listener, handler, http::Config::default())
        })
        .collect();
    // Four distinct cold pages compete for a four-slot ingress pool. Each hop
    // has a separate NUMA pool/flight registry, as on separate physical nodes.
    let clients: Vec<_> = (0..4)
        .map(|index| {
            let address = addresses[0];
            thread::spawn(move || {
                let mut socket = std::net::TcpStream::connect(address).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(10)))
                    .unwrap();
                write!(
                    socket,
                    "GET /payload-{index} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
                )
                .unwrap();
                let headers = request(&mut socket);
                assert!(headers.starts_with("HTTP/1.1 200"), "{headers}");
                let mut body = Vec::new();
                socket.read_to_end(&mut body).unwrap();
                assert_eq!(body, b"abc");
            })
        })
        .collect();
    let mut origins = Vec::new();
    let release = Arc::new(AtomicBool::new(false));
    let started = Arc::new(AtomicBool::new(false));
    let mut saturated = false;
    let end = Instant::now() + Duration::from_secs(10);
    while clients.iter().any(|c| !c.is_finished()) {
        assert!(Instant::now() < end, "saturated payload chain stalled");
        while let Ok((mut socket, _)) = origin.accept() {
            let release = release.clone();
            let started = started.clone();
            origins.push(thread::spawn(move || {
                socket.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
                let headers = request(&mut socket);
                let tag = crate::conformance::etag(b"abc");
                if headers.starts_with("HEAD ") {
                    write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {tag}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n").unwrap();
                } else {
                    assert!(headers.contains("Range: bytes=0-2\r\n"));
                    started.store(true, Ordering::Release);
                    while !release.load(Ordering::Acquire) {
                        assert!(Instant::now() < end, "origin release deadline");
                        thread::sleep(Duration::from_millis(1));
                    }
                    write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nContent-Range: bytes 0-2/3\r\nETag: {tag}\r\nConnection: close\r\n\r\nabc").unwrap();
                }
            }));
        }
        for (server, ring) in servers.iter_mut().zip(&mut rings) {
            ring.progress().unwrap();
            server.handler_mut().poll_background(ring, 64).unwrap();
            server.poll(ring, 64).unwrap();
        }
        if !saturated && started.load(Ordering::Acquire) {
            // All three forwarding nodes hold one receive. Verify ingress
            // cannot admit another rank-three miss, while terminal capacity
            // remains usable, before allowing the origin to complete.
            assert_eq!(rings[0].pool().invariant_snapshot().loading, 1);
            let mut wait =
                rings[0]
                    .pool()
                    .wait_stage_reserved(crate::buffers::Key::new([99; 32]), 3, end);
            assert!(wait.poll(std::task::Waker::noop()).is_pending());
            let terminal = rings[0].pool().private_fill().unwrap();
            drop((terminal, wait));
            saturated = true;
            release.store(true, Ordering::Release);
        }
        thread::yield_now();
    }
    for (server, ring) in servers.iter_mut().zip(&mut rings) {
        server.shutdown(ring).unwrap();
        server.handler_mut().shutdown(ring).unwrap();
        ring.shutdown().unwrap();
        ring.pool().assert_recovered();
    }
    for task in clients.into_iter().chain(origins) {
        task.join().unwrap();
    }
    for server in &mut servers[..3] {
        assert_eq!(
            server.handler_mut().upstream.metrics.values()[17],
            4,
            "each forwarding hop must fetch all four payloads over HTTP"
        );
    }
    assert!(
        saturated,
        "fixture never exercised reserved-capacity pressure"
    );
}
