// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

pub(super) fn cycle_handler(epoch: u64, remote: &str, address: std::net::SocketAddr) -> Handler {
    let (_, mut config) = crate::control::tests::fixture();
    let volume = &mut config.volumes[0];
    volume.peers = vec![remote.into()];
    let topology = volume.topology.as_mut().unwrap();
    topology.epoch = epoch;
    topology.product.as_mut().unwrap().members[1] = remote.into();
    let routing = Arc::new(crate::routing::Routing::new(&config.universe, volume).unwrap());
    // Both independently converging nodes currently believe they own slot zero.
    let backend = Backend::new("127.0.0.1:1", "cycle-origin").unwrap();
    let mut handler = Handler::new(cache(&backend, 1), backend);
    handler.set_attempt_policy(1).unwrap();
    handler.set_routing(
        routing,
        BTreeMap::from([(
            remote.into(),
            Peer::new(&address.to_string(), None).unwrap(),
        )]),
    );
    handler
}

fn target(handler: &Handler) -> String {
    (0..)
        .map(|n| format!("/cycle-{n}"))
        .find(|t| {
            let key = cache::PeerDescriptor::metadata(t)
                .key(handler.namespace)
                .unwrap();
            handler
                .upstream
                .routing
                .as_ref()
                .unwrap()
                .start_key(&key)
                .owner
                == 1
        })
        .unwrap()
}

pub(super) fn page_target(handler: &Handler) -> String {
    (0..)
        .map(|n| format!("/cycle-page-{n}"))
        .find(|t| {
            let key = cache::PeerDescriptor::page(
                t,
                cache::PeerPage::new(
                    0,
                    3,
                    crate::metadata::Checksum(*blake3::hash(b"abc").as_bytes()),
                ),
            )
            .key(handler.namespace)
            .unwrap();
            handler
                .upstream
                .routing
                .as_ref()
                .unwrap()
                .start_key(&key)
                .owner
                == 1
        })
        .unwrap()
}

#[test]
fn http_a_b_a_exhausts_without_origin_or_coalescing_deadlock() {
    let Some(mut ring) = crate::conformance::kernel_ring(12, uring::Config::default()) else {
        return;
    };
    let bind = || {
        http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(64).unwrap(),
        )
        .unwrap()
    };
    let mut al = bind();
    let mut bl = bind();
    let ca = crate::tls::tests::Authority::new();
    let aid = peer_identity(2);
    let bid = peer_identity(3);
    let ap = crate::control::credentials::Provider::for_test(
        aid.clone(),
        Arc::new(ca.context(&aid, false)),
    );
    let bp = crate::control::credentials::Provider::for_test(
        bid.clone(),
        Arc::new(ca.context(&bid, false)),
    );
    al.set_tls(
        (*ap.current().context).clone(),
        ExpectedPeer::Identity(bid.clone()),
    );
    bl.set_tls(
        (*bp.current().context).clone(),
        ExpectedPeer::Identity(aid.clone()),
    );
    let mut a = cycle_handler(1, &bid.node, bl.local_addr().unwrap());
    let mut b = cycle_handler(2, &aid.node, al.local_addr().unwrap());
    a.set_authentication(peer_policy(2, 3));
    b.set_authentication(peer_policy(3, 2));
    a.set_peer_tls("v1", ap, &BTreeMap::from([(bid.node.clone(), bid)]));
    b.set_peer_tls("v1", bp, &BTreeMap::from([(aid.node.clone(), aid)]));
    let object = target(&a);
    let local = bind();
    let local_address = local.local_addr().unwrap();
    // Ingress and A's peer listener share cache/endpoint pools, just as runtime does.
    let ingress = Handler {
        upstream: a.upstream.routed(None),
        ..Handler::shared(
            a.cache.clone(),
            Backend::new("127.0.0.1:1", "cycle-origin").unwrap(),
            a.namespace,
        )
    };
    let mut local = http::Server::new(local, ingress, http::Config::default());
    let mut a = http::Server::new(al, a, http::Config::default());
    let mut b = http::Server::new(bl, b, http::Config::default());
    for saturated in [false, true] {
        for server in [&mut local, &mut a, &mut b] {
            server
                .handler_mut()
                .set_attempt_policy(if saturated { 3 } else { 1 })
                .unwrap();
        }
        let mut pins = Vec::new();
        if saturated {
            while let Ok(fill) = ring.pool().private_fill() {
                pins.push(fill);
            }
        }
        // Storage-free metadata can traverse the cycle even when payload slots
        // are saturated; each returning request must be an independent producer.
        let end = Instant::now() + Duration::from_secs(15);
        let mut get = client::Connection::new(local_address, "localhost")
            .unwrap()
            .get_small(
                client::Request::new(&object, &[]).unwrap(),
                cache::METADATA_SIZE,
                end,
            )
            .unwrap();
        let status = loop {
            assert!(Instant::now() < end, "cyclic HTTP chain stalled");
            ring.progress().unwrap();
            for server in [&mut local, &mut a, &mut b] {
                server.handler_mut().poll_background(&mut ring, 64).unwrap();
                server.poll(&mut ring, 64).unwrap();
            }
            if let Progress::Ready(response) = get.poll(&mut ring, 64).unwrap() {
                break response.status();
            }
        };
        assert_eq!(
            status, 503,
            "hop exhaustion must not become origin failure/fallback"
        );
        drop(pins);
    }
    for server in [&mut local, &mut a, &mut b] {
        server.shutdown(&mut ring).unwrap();
        server.handler_mut().shutdown(&mut ring).unwrap();
    }
    ring.shutdown().unwrap();
}

#[test]
fn rdma_wire_a_b_a_preserves_identity_and_affine_fallback_budget() {
    let Some(mut ring) = crate::conformance::kernel_ring(8, uring::Config::default()) else {
        return;
    };
    let a = cycle_handler(1, "b", "127.0.0.1:1".parse().unwrap());
    let b = cycle_handler(2, "a", "127.0.0.1:1".parse().unwrap());
    let object = page_target(&a);
    let page = page_request(&mut a.cache.borrow_mut(), &mut ring, &object);
    let request = UpstreamRequest::PeerPage(page.clone());
    let mut provider = a
        .upstream
        .routed(a.upstream.route_state(None, page.key(), false).unwrap());
    let ao = crate::negotiation::tests::offer(1, [3; 16], 0, 1, "fabric");
    let bo = crate::negotiation::tests::offer(2, [4; 16], 0, 1, "fabric");
    let ((_, mut ac), (_, mut bc)) =
        crate::negotiation::tests::tls_channels(&ao, &bo, &mut ring, false);
    let end = Instant::now() + Duration::from_secs(5);
    let mut last_deadline = end;
    let authorization = crate::authorization::Authorization::new("Bearer cyclic-page").unwrap();
    for hop in 0..8 {
        let wire = provider.budget_wire(&request, last_deadline).unwrap();
        let (_, remaining, _, _) = cache::peer_wire::chain(&wire).unwrap().unwrap();
        assert_eq!(remaining, 7 - hop);
        let wire = crate::authorization::rdma_envelope(&wire, &authorization).unwrap();
        let frame = crate::negotiation::control_wire::Frame {
            kind: 1,
            session: [7; 16],
            request: u64::from(hop) + 1,
            value: *page.key(),
            len: page.len() as u32,
            metadata: wire.len() as u16,
            ..Default::default()
        };
        let mut bytes = vec![0; crate::negotiation::control_wire::HEADER];
        frame.encode(&mut bytes);
        bytes.extend(&wire);
        let (send, recv) = if hop % 2 == 0 {
            (&mut ac, &mut bc)
        } else {
            (&mut bc, &mut ac)
        };
        send.enqueue(u64::from(hop), &bytes).unwrap();
        let received = loop {
            assert!(Instant::now() < end);
            ring.progress().unwrap();
            send.poll(&mut ring, 8).unwrap();
            recv.poll(&mut ring, 8).unwrap();
            if let Some(bytes) = recv.take_received() {
                break bytes;
            }
        };
        let decoded = crate::negotiation::control_wire::Frame::decode(&received).unwrap();
        assert_eq!(decoded.value, *page.key());
        assert_eq!(decoded.len, page.len() as u32);
        let metadata = &received[crate::negotiation::control_wire::HEADER..];
        let (metadata, forwarded) = crate::authorization::rdma_decode(metadata).unwrap();
        assert_eq!(forwarded.as_str(), authorization.as_str());
        let receiver = if hop % 2 == 0 { &b } else { &a };
        provider = receiver.peer_provider(metadata).unwrap();
        assert!(
            provider.has_peer(),
            "local placement must keep the cycle visible"
        );
        let next_deadline = remote_deadline(metadata, end).unwrap();
        assert!(next_deadline < last_deadline);
        last_deadline = next_deadline;
        let (_, descriptor) = routed_descriptor(metadata).unwrap();
        assert_eq!(descriptor.target(), object);
        let fault = receiver
            .cache
            .borrow_mut()
            .peer_fault_in::<Provider>(
                &cache::Context::new(receiver.namespace).with_authorization(forwarded),
                descriptor.with_expected(decoded.value, decoded.len as usize),
                next_deadline,
            )
            .unwrap();
        assert_eq!(fault.representation_checksum(), Some(page.checksum()));
        drop(fault);
    }
    assert!(provider.budget_wire(&request, end).is_err());
    let (authority, destination) = destination(&ring, *page.key());
    // This is the production RDMA recovery continuation, with exhausted state.
    let result = provider.resume_peer(
        Exchange::RecoverHttp(HttpRecovery {
            request,
            attempt: None,
        }),
        destination,
        end,
        &mut ring,
    );
    assert!(matches!(result, Err(cache::Error::Unavailable)));
    drop(authority);
    ac.close();
    bc.close();
    ring.shutdown().unwrap();
}

#[test]
fn namespace_isolation_and_relay_flights_under_saturation() {
    let Some(mut ring) = crate::conformance::kernel_ring(2, uring::Config::default()) else {
        return;
    };
    let a = cycle_handler(1, "b", "127.0.0.1:1".parse().unwrap());
    let b = cycle_handler(2, "a", "127.0.0.1:1".parse().unwrap());
    let object = page_target(&a);
    let page = page_request(&mut a.cache.borrow_mut(), &mut ring, &object);
    let request = UpstreamRequest::PeerPage(page.clone());
    let provider = a
        .upstream
        .routed(a.upstream.route_state(None, page.key(), false).unwrap());
    let wire = provider.budget_wire(&request, deadline()).unwrap();
    let mut relays = [
        b.peer_provider(&wire).unwrap(),
        b.peer_provider(&wire).unwrap(),
    ];
    let keys: Vec<_> = relays
        .iter()
        .map(|p| p.network_scope(*page.key()).unwrap())
        .collect();
    assert_ne!(keys[0], keys[1]);
    let mut flights: Vec<_> = keys
        .into_iter()
        .map(|k| ring.pool().network_flight(k).unwrap())
        .collect();
    for flight in &mut flights {
        assert!(matches!(
            flight.poll(std::task::Waker::noop()),
            buffers::NetworkProgress::Produce
        ));
    }
    let mut foreign = wire.clone();
    foreign[4] ^= 1;
    assert!(b.peer_provider(&foreign).is_err());
    for (universe, uid, generation) in [
        ([9; 32], "v1", 0),
        ([1; 32], "other", 0),
        ([1; 32], "v1", 1),
    ] {
        let namespace = cache::Namespace::volume(&universe, uid, generation, b.namespace);
        let mut foreign = wire.clone();
        foreign[4..36].copy_from_slice(namespace.digest());
        assert!(b.peer_provider(&foreign).is_err());
    }
    let mut pins = Vec::new();
    while let Ok(fill) = ring.pool().private_fill() {
        pins.push(fill);
    }
    let end = Instant::now() + Duration::from_millis(100);
    let (_, descriptor) = routed_descriptor(&wire).unwrap();
    let mut fault = b
        .cache
        .borrow_mut()
        .peer_fault_in::<Provider>(&cache::Context::new(b.namespace), descriptor, end)
        .unwrap();
    let mut polls = 0;
    loop {
        polls += 1;
        match b
            .cache
            .borrow_mut()
            .poll_fault(fault, &mut ring, &mut relays[0])
        {
            Ok(cache::Progress::Pending { fault: next, work }) => {
                fault = next;
                if let Some(until) = work.deadline {
                    std::thread::sleep(until.saturating_duration_since(Instant::now()));
                }
            }
            Err(error) => {
                assert!(matches!(
                    error,
                    cache::Error::Timeout | cache::Error::Admission(_)
                ));
                break;
            }
            _ => panic!("saturated receive unexpectedly completed"),
        }
        assert!(polls < 50);
    }
    drop((pins, flights));
    ring.shutdown().unwrap();
}

#[test]
fn retry_tree_has_finite_work_and_never_renews_hops() {
    fn work(mut chain: Chain) -> usize {
        let mut total = 0;
        while let Ok((hops, allowance)) = chain.forward() {
            total += 1 + work(Chain {
                hops,
                work: allowance,
            });
        }
        total
    }
    assert_eq!(
        work(Chain::default()),
        usize::from(cache::peer_wire::MAX_WORK)
    );
}

#[test]
fn production_http_recovery_and_reroute_spend_existing_chain() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, uring::Config::default()) else {
        return;
    };
    let mut a = cycle_handler(1, "b", "127.0.0.1:1".parse().unwrap());
    a.set_attempt_policy(2).unwrap();
    let ca = crate::tls::tests::Authority::new();
    let identity = peer_identity(2);
    let credentials = crate::control::credentials::Provider::for_test(
        identity.clone(),
        Arc::new(ca.context(&identity, false)),
    );
    a.set_peer_tls(
        "v1",
        credentials,
        &BTreeMap::from([("b".into(), peer_identity(3))]),
    );
    let object = page_target(&a);
    let page = page_request(&mut a.cache.borrow_mut(), &mut ring, &object);
    let request = UpstreamRequest::PeerPage(page.clone());
    let mut provider = a
        .upstream
        .routed(a.upstream.route_state(None, page.key(), false).unwrap());
    let wire = provider.budget_wire(&request, deadline()).unwrap();
    assert_eq!(cache::peer_wire::chain(&wire).unwrap().unwrap().1, 7);
    let (authority, destination) = destination(&ring, *page.key());
    let recovery = provider
        .resume_peer(
            Exchange::RecoverHttp(HttpRecovery {
                request,
                attempt: None,
            }),
            destination,
            deadline(),
            &mut ring,
        )
        .unwrap();
    assert_eq!(
        provider.chain.borrow().hops,
        6,
        "HTTP recovery is another forward"
    );
    assert!(provider.chain.borrow().work < 128);
    drop((recovery, authority));
    let route = provider.attempt("a".repeat(96)).unwrap().unwrap().route;
    assert!(
        provider
            .peer_failed(
                AttemptFailure {
                    route,
                    evidence: None,
                    reported: true
                }
                .into()
            )
            .unwrap()
    );
    assert_eq!(
        provider.chain.borrow().hops,
        6,
        "candidate change never renews the chain"
    );
    let fork = provider.page_provider(page.key()).unwrap();
    assert!(
        !Rc::ptr_eq(&fork.chain, &provider.chain),
        "a new page owns a separate resolution budget"
    );
    assert_eq!(*fork.chain.borrow(), Chain::default());
    assert_eq!(provider.chain.borrow().hops, 6);
    ring.shutdown().unwrap();
}

#[test]
fn http_admission_retries_do_not_spend_resolution_budget() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, uring::Config::default()) else {
        return;
    };
    let a = cycle_handler(1, "b", "127.0.0.1:1".parse().unwrap());
    let ca = crate::tls::tests::Authority::new();
    let identity = peer_identity(2);
    let credentials = crate::control::credentials::Provider::for_test(
        identity.clone(),
        Arc::new(ca.context(&identity, false)),
    );
    let mut a = a;
    a.set_peer_tls(
        "v1",
        credentials,
        &BTreeMap::from([("b".into(), peer_identity(3))]),
    );
    let object = page_target(&a);
    let page = page_request(&mut a.cache.borrow_mut(), &mut ring, &object);
    let request = UpstreamRequest::PeerPage(page.clone());
    let mut provider = a
        .upstream
        .routed(a.upstream.route_state(None, page.key(), false).unwrap());
    let peer = provider.peer.as_ref().unwrap().clone();
    peer.borrow_mut().http.limit = 1;
    let permit = peer.borrow().http.breaker.try_acquire().unwrap();
    for _ in 0..16 {
        let (authority, destination) = destination(&ring, *page.key());
        assert!(
            provider
                .http_peer_attempt(request.clone(), Some(destination), deadline(), None)
                .is_err()
        );
        assert_eq!(*provider.chain.borrow(), Chain::default());
        drop(authority);
    }
    drop(permit);
    // An open transport breaker is also unsubmitted admission, not a hop.
    peer.borrow().http.breaker.try_acquire().unwrap().failure();
    let (authority, destination) = destination(&ring, *page.key());
    assert!(
        provider
            .http_peer_attempt(request.clone(), Some(destination), deadline(), None)
            .is_err()
    );
    assert_eq!(*provider.chain.borrow(), Chain::default());
    drop(authority);
    peer.borrow_mut().http.breaker = crate::breaker::CircuitBreaker::new(COOLDOWN);
    let (authority, destination) = super::destination(&ring, *page.key());
    let exchange = provider
        .http_peer_attempt(request, Some(destination), deadline(), None)
        .unwrap();
    assert_eq!(provider.chain.borrow().hops, 7);
    assert_eq!(provider.chain.borrow().work, 127);
    // An accepted exchange conservatively retains its spend even when dropped.
    drop((exchange, authority));
    assert_eq!(provider.chain.borrow().hops, 7);
    ring.shutdown().unwrap();
}

include!("large_peer_stream.rs");
include!("peer_rotation.rs");
