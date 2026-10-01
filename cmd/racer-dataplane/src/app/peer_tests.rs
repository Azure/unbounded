//! Exercise assembled peer I/O, including socket session signatures and full paths.
use super::test_support::local_worker;
use super::*;

#[test]
fn assembly_applies_configured_client_request_timeout() {
    use crate::{
        http::connection::ConnectionLease, model::ResourceClass, runtime::ingress::Retired,
    };
    for timeout in [Duration::from_millis(250), Duration::from_secs(45)] {
        let clock = crate::runtime::environment::SimulationClock::new(908);
        let _environment = clock.environment(0).enter();
        let mut config = crate::test_support::cluster::config(false);
        config.request_timeout = timeout;
        config.reader_stall_timeout = timeout;
        let (app, runtime, _) = local_worker(&config, &Arc::new(NodeState::default()), 0);
        let (server, mut client) = std::os::unix::net::UnixStream::pair().unwrap();
        client.set_nonblocking(true).unwrap();
        let reservation = runtime
            .admission
            .reserve_connection(ResourceClass::IngressConnection)
            .unwrap();
        let connection = ConnectionLease::from_reserved(server.into(), reservation).unwrap();
        app.clients
            .install_connection(
                connection,
                test_support::definition().id,
                Arc::new(Retired::default()),
            )
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        app.clients.poll_budgeted(&mut cx, 64).unwrap();
        clock.advance(timeout - Duration::from_millis(1));
        app.clients.poll_budgeted(&mut cx, 64).unwrap();
        assert_eq!(app.clients.active_connections(), 1);
        clock.advance(Duration::from_millis(2));
        for _ in 0..64 {
            runtime.reactor.poll_budgeted(64).unwrap();
            app.clients.poll_budgeted(&mut cx, 64).unwrap();
            if app.clients.active_connections() == 0 {
                break;
            }
        }
        assert_eq!(app.clients.active_connections(), 0);
        let mut byte = [0];
        assert_eq!(std::io::Read::read(&mut client, &mut byte).unwrap(), 0);
    }
}

#[test]
fn worker_requesters_share_configured_admission_and_production_metrics() {
    use crate::telemetry::metrics::{Event, Gauge};
    let mut config = crate::test_support::cluster::config(false);
    config.peer_admission = crate::peer::adaptive::Config {
        total: 2,
        per_peer: 1,
    };
    let node = Arc::new(
        NodeState::with_peer_admission(vec![WorkerId(0), WorkerId(1)], 16, config.peer_admission)
            .unwrap(),
    );
    let (first, _, _) = local_worker(&config, &node, 0);
    let (second, _, _) = local_worker(&config, &node, 1);
    let a = first.peer_requester.admission();
    let b = second.peer_requester.admission();
    let peer = NodeId("test-peer".into());
    let permit = a.acquire(&peer).unwrap();
    assert!(matches!(b.acquire(&peer), Err(Error::Overloaded)));
    assert_eq!(node.metrics[1].1.count(Event::PeerAdmissionAccepted), 1);
    assert_eq!(node.metrics[1].1.count(Event::PeerAdmissionRejected), 1);
    assert_eq!(node.metrics[1].1.gauge(Gauge::PeerExchanges), 1);
    assert_eq!(node.metrics[1].1.gauge(Gauge::PeerAdmissionLimit), 2);
    permit.observe(crate::peer::adaptive::Outcome::PeerFailure);
    assert!(!b.available(&peer));
    drop(permit);
    assert_eq!(node.metrics[1].1.gauge(Gauge::PeerExchanges), 0);
}

#[test]
fn workers_share_configured_page_hedge_slots_and_bytes() {
    let clock = crate::runtime::environment::SimulationClock::new(907);
    let _environment = clock.environment(0).enter();
    let mut config = crate::test_support::cluster::config(false);
    config.page_hedge.slots = 1;
    let node = Arc::new(NodeState::default());
    let (first, _, _) = local_worker(&config, &node, 0);
    let owner = first.coordinator.hedge_owner().unwrap();
    let (mut second, _, _) = local_worker(&config, &node, 1);
    let permit = owner.acquire().unwrap();
    let wake = Arc::new(crate::test_support::WakeCounter::default());
    let waker = std::task::Waker::from(wake.clone());
    let mut cx = Context::from_waker(&waker);
    assert!(permit.delay(&mut cx).is_pending());
    clock.advance(config.page_hedge.delay);
    // No listener/checkpoint startup in this composition-only fixture. Alarms
    // must still wake before unrelated unstarted services report unavailable.
    assert_eq!(second.poll_services(&mut cx, 1), Err(Error::Unavailable));
    assert!(wake.count() > 0);
    assert!(permit.delay(&mut cx).is_ready());
    assert!(matches!(
        second.coordinator.hedge_owner().unwrap().acquire(),
        Err(Error::Overloaded)
    ));
    drop(permit);
    assert!(second.coordinator.hedge_owner().unwrap().acquire().is_ok());
}

#[test]
fn distributed_peer_listener_recovers_from_queue_pressure() {
    let mut config = crate::test_support::cluster::config(false);
    config.limits.queue_entries = NonZeroUsize::new(8).unwrap();
    let node = Arc::new(NodeState::default());
    let (app, runtime, _) = local_worker(&config, &node, 0);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    drop(listener);
    let scope = scope(Duration::from_secs(5)).unwrap();
    let mut serving = app.peers.listen(address, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let (reader, _writer) = std::os::unix::net::UnixStream::pair().unwrap();
    let reader = Rc::new(crate::runtime::reactor::Descriptor::from(reader));
    let mut pressure = Vec::new();
    for _ in 0..8 {
        let mut wait = runtime
            .reactor
            .readiness(reader.clone(), libc::POLLIN as u32, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        pressure.push(wait);
    }
    for _ in 0..32 {
        assert!(serving.as_mut().poll(&mut cx).is_pending());
        assert_eq!(runtime.reactor.in_flight(), 8);
    }
    let _client = std::net::TcpStream::connect(address).unwrap();
    drop(pressure);
    let until = Instant::now() + Duration::from_secs(2);
    loop {
        assert!(serving.as_mut().poll(&mut cx).is_pending());
        runtime.reactor.poll_budgeted(64).unwrap();
        if let Some(accepted) = node
            .ingress
            .pop_batch::<1>(WorkerId(0), cx.waker(), 1)
            .unwrap()[0]
            .take()
        {
            assert!(matches!(accepted.kind, crate::runtime::ingress::Kind::Peer));
            break;
        }
        assert!(Instant::now() < until, "distributed peer accept stalled");
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
    }
    scope.cancel().unwrap();
    loop {
        runtime.reactor.poll_budgeted(64).unwrap();
        if let Poll::Ready(result) = serving.as_mut().poll(&mut cx) {
            assert_eq!(result, Err(Error::Cancelled));
            break;
        }
        assert!(Instant::now() < until, "peer cancellation stalled");
        runtime.reactor.wait(Duration::from_millis(1)).unwrap();
    }
}

#[test]
fn assembly_uses_node_metrics_for_sparse_worker_ids() {
    use crate::telemetry::metrics::{Event, Gauge};
    let config = crate::test_support::cluster::config(false);
    let node = Arc::new(NodeState::new(vec![WorkerId(9), WorkerId(2)], 16).unwrap());
    let (first, _, _) = local_worker(&config, &node, 9);
    let (second, _, _) = local_worker(&config, &node, 2);
    first.telemetry.metrics.record(Event::MemoryHit, 2).unwrap();
    second
        .telemetry
        .metrics
        .record(Event::MemoryHit, 3)
        .unwrap();
    let request = first.telemetry.metrics.request().unwrap();
    assert_eq!(second.telemetry.metrics.count(Event::MemoryHit), 5);
    assert_eq!(second.telemetry.metrics.gauge(Gauge::ActiveRequests), 1);
    drop(first);
    drop(request);
    assert_eq!(second.telemetry.metrics.count(Event::RequestError), 1);
    assert_eq!(second.telemetry.metrics.gauge(Gauge::ActiveRequests), 0);
}
use crate::{
    http::{Codec, Header, MessageHead, StartLine, connection::ConnectionLease},
    model::{ExpiresAt, MetadataSelector, ObjectMetadata, ResourceClass, *},
    peer::protocol::{
        self, FetchMode, Operation as PeerOperation, PeerRequest, PeerResponse, WireCodec,
    },
    security::{
        connection,
        signing::tests::{network, node},
    },
    topology::paths::RouteBudget,
};
use std::{future::Future, os::unix::net::UnixStream};

fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let watchdog = Instant::now() + Duration::from_secs(30);
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < watchdog);
        if reactor.poll_budgeted(128).unwrap() == 0 {
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
}

fn application() -> (WorkerApplication, WorkerRuntime, PageCryptoEngine) {
    let mut config = crate::test_support::cluster::config(false);
    // One fixture represents both ends of eight nodes' sockets. Fund those
    // ingress leases plus the reserved outbound/control partition explicitly,
    // including the shared fixture's 16-connection neighbor cap.
    config.limits.client_connections = NonZeroUsize::new(64).unwrap();
    config.limits.header_bytes = NonZeroUsize::new(32 * 1024).unwrap();
    config.limits.range_window_pages = NonZeroUsize::new(1).unwrap();
    // Use the exact worker progress floor rather than the generous fixture budget.
    config.limits.request_context_bytes =
        NonZeroUsize::new(protocol::MIN_REQUEST_CONTEXT_BYTES + 4 * 32 * 1024).unwrap();
    partition_limits(&config.limits, 1, false).unwrap();
    local_worker(&config, &Arc::new(NodeState::default()), 0)
}

#[test]
fn assembled_peer_io_carries_maximum_client_context_over_eight_signed_links() {
    let (app, runtime, _engine) = application();
    let io = app.peers.transport_io();
    let admission = &runtime.admission;
    let reactor = &runtime.reactor;
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let signers = network(protocol::MAX_HOPS + 1);
    let forwarding: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
    let codec = protocol::SecurityCodec::new(admission.clone(), BufferPool::new(admission.clone()));
    let crypto = CredentialCrypto::new(
        Rc::new(crate::security::identity::keyring_tests::keys()),
        admission.clone(),
    );
    let scope = scope(Duration::from_secs(30)).unwrap();
    let cache = CacheId(crate::security::identity::tests::CACHE.into());
    let metadata = vec![b'm'; 8192];
    let authorization = vec![b'a'; 8192];
    let etag = format!("\"{}\"", "v".repeat(8190));
    let head = MessageHead {
        start: StartLine::Request {
            method: "HEAD".into(),
            target: format!("/v1/objects/{}", "ab".repeat(32)),
        },
        headers: vec![
            Header {
                name: "Host".into(),
                value: b"racer".to_vec(),
            },
            Header {
                name: "If-Match".into(),
                value: etag.as_bytes().to_vec(),
            },
            Header {
                name: "Racer-Metadata".into(),
                value: metadata.clone(),
            },
            Header {
                name: "Authorization".into(),
                value: authorization.clone(),
            },
        ],
    };
    let client_codec = Codec::new(32 * 1024, 0);
    let raw = client_codec.encode_head(&head).unwrap();
    let parsed = RequestParser::new(32 * 1024)
        .parse(&cache, client_codec.decode_head(&raw).unwrap().unwrap().0)
        .unwrap();
    let pin = parsed.kind.pin().unwrap().clone();
    let object = parsed.origin.object.clone();
    let attempt = AttemptId([2; 16]);
    let origin = crypto.seal(&parsed.origin, attempt, &scope).unwrap();
    assert_eq!(
        origin.authorization.as_ref().unwrap().ciphertext.len(),
        8208
    );
    let ciphertext = origin.authorization.as_ref().unwrap().ciphertext.clone();
    let request = PeerRequest {
        operation: PeerOperation::Metadata {
            object: object.clone(),
            selector: MetadataSelector::Pinned(pin.clone()),
            mode: FetchMode::Acquire,
        },
        origin,
        route: RouteBudget {
            membership: MembershipVersion(1),
            request: scope.request,
            attempt,
            destination: node(protocol::MAX_HOPS),
            visited: vec![node(0)],
            remaining_links: protocol::MAX_HOPS as u8,
            remaining_attempts: 1,
            deadline: scope.deadline,
        },
    };
    let (mut request, binding) = forwarding[0].sign_request_to(request, &node(1)).unwrap();
    assert!(
        WireCodec::encode(&request.authentication, false, 0)
            .unwrap()
            .unique("racer-original")
            .unwrap()
            .unwrap()
            .len()
            > 32 * 1024
    );
    let mut bindings = vec![binding];
    let mut sockets = Vec::new();
    let mut destination = None;
    for index in 1..=protocol::MAX_HOPS {
        let (left, right) = UnixStream::pair().unwrap();
        let left = ConnectionLease::from_accepted(left.into(), admission).unwrap();
        let right = ConnectionLease::from_accepted(right.into(), admission).unwrap();
        let next = node(index);
        let (left, right) = drive(reactor, async {
            futures::try_join!(
                connection::connect(io, left, signers[index - 1].clone(), &next, &scope),
                connection::accept(io, right, signers[index].clone(), &scope)
            )
        })
        .unwrap();
        let head = WireCodec::encode(&request.authentication, false, 0).unwrap();
        let (sent, received) = drive(reactor, async {
            futures::try_join!(
                io.send_head(left, head, &scope),
                io.receive_head(right, &scope)
            )
        })
        .unwrap();
        sockets.push((sent.connection, received.connection));
        let (auth, length) = WireCodec::decode(received.value, false).unwrap();
        assert_eq!(length, 0);
        assert_eq!(auth.hops.len(), index - 1);
        let decoded = codec.request(auth, &scope).unwrap();
        assert_eq!(
            decoded
                .request
                .origin
                .authorization
                .as_ref()
                .unwrap()
                .ciphertext,
            ciphertext
        );
        let verified = forwarding[index].verify_request(decoded).unwrap();
        bindings.push(verified.binding().clone());
        drop(request);
        if index == protocol::MAX_HOPS {
            destination = Some(verified);
            break;
        }
        let mut route = verified.request().route.clone();
        route.visited.push(node(index));
        route.remaining_links -= 1;
        request = forwarding[index]
            .append_request(verified, &node(index + 1), route)
            .unwrap();
    }
    let destination = destination.unwrap();
    let mut response = forwarding[protocol::MAX_HOPS]
        .sign_response(
            destination.binding(),
            PeerResponse::Metadata(ObjectMetadata {
                content_type: None,
                version: ObjectVersion { object, etag: pin },
                length: 42,
                expires_at: ExpiresAt(std::time::SystemTime::now()),
            }),
        )
        .unwrap();
    let opened = crypto
        .open_charged(
            destination.into_signed().request.origin,
            scope.request,
            attempt,
        )
        .unwrap();
    assert_eq!(opened.metadata.as_ref().unwrap().as_header(), metadata);
    assert_eq!(
        opened.authorization.as_ref().unwrap().expose_for_origin(),
        authorization
    );
    drop(opened);
    for index in (1..=protocol::MAX_HOPS).rev() {
        let (left, right) = sockets.pop().unwrap();
        let head = WireCodec::encode(&response.authentication, true, 0).unwrap();
        let (sent, received) = drive(reactor, async {
            futures::try_join!(
                io.send_head(right, head, &scope),
                io.receive_head(left, &scope)
            )
        })
        .unwrap();
        let (auth, _) = WireCodec::decode(received.value, true).unwrap();
        let verified = forwarding[index - 1]
            .verify_response(
                codec.response(auth, vec![], &scope).unwrap(),
                &bindings[index - 1],
            )
            .unwrap();
        assert_eq!(
            verified.signed().authentication.hops.len(),
            protocol::MAX_HOPS - index
        );
        let PeerResponse::Metadata(result) = verified.response() else {
            panic!("expected metadata")
        };
        assert_eq!(result.version.etag.as_str(), etag);
        let mut left = received.connection;
        let mut right = sent.connection;
        left.finish_exchange().unwrap();
        right.finish_exchange().unwrap();
        if index > 1 {
            response = forwarding[index - 1]
                .append_response(verified, &node(index - 2))
                .unwrap();
        }
    }
    drive(reactor, reactor.drain()).unwrap();
    io.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}

#[test]
fn peer_worker_partition_rejects_underfunding_and_reduces_worker_count() {
    use crate::runtime::affinity::{CpuLocation, EffectiveTopology};
    let mut config = crate::test_support::cluster::config(false);
    config.max_threads = 4;
    config.limits.range_window_pages = NonZeroUsize::new(1).unwrap();
    config.limits.connections_per_neighbor = NonZeroUsize::new(1).unwrap();
    // Fund control progress on all three planned shards so only context bytes
    // determine which worker counts pass the boundary assertions below.
    config.limits.client_connections = NonZeroUsize::new(36).unwrap();
    let floor = protocol::MIN_REQUEST_CONTEXT_BYTES + 4 * config.limits.header_bytes.get();
    for budget in [128 * 1024, floor - 1, floor, 2 * floor - 1, 2 * floor] {
        config.limits.request_context_bytes = NonZeroUsize::new(budget).unwrap();
        assert_eq!(
            partition_limits(&config.limits, 1, false).is_ok(),
            budget >= floor
        );
        assert_eq!(
            partition_limits(&config.limits, 2, false).is_ok(),
            budget >= 2 * floor
        );
        let mut plan = AffinityPlan::from_topology(
            &config,
            EffectiveTopology {
                cpus: (0..4)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: None,
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap();
        assert_eq!(plan.pairs.len(), 3);
        let result = size_workers(&config.limits, &mut plan, false);
        if budget < floor {
            assert!(matches!(result, Err(Error::InvalidConfiguration)));
        } else {
            let limits = result.unwrap();
            assert_eq!(plan.pairs.len(), (budget / floor).min(2));
            assert!(limits.request_context_bytes.get() >= floor);
        }
    }
}

#[test]
fn assembled_peer_io_rejects_oversize_and_admission_pressure_before_submission() {
    use std::io::{Read, Write};
    let (app, runtime, _engine) = application();
    let io = app.peers.transport_io();
    let admission = &runtime.admission;
    let reactor = &runtime.reactor;
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let scope = scope(Duration::from_secs(30)).unwrap();
    let head = || MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: protocol::REQUEST_TARGET.into(),
        },
        headers: vec![Header {
            name: "x".into(),
            value: vec![b'x'; protocol::MAX_ENVELOPE_HEAD],
        }],
    };
    let (socket, _other) = UnixStream::pair().unwrap();
    let conn = ConnectionLease::from_accepted(socket.into(), admission).unwrap();
    assert!(matches!(
        drive(reactor, io.send_head(conn, head(), &scope)),
        Err(Error::HeaderTooLarge)
    ));
    let (socket, mut other) = UnixStream::pair().unwrap();
    let conn = ConnectionLease::from_accepted(socket.into(), admission).unwrap();
    let writer = std::thread::spawn(move || {
        let _ = other.write_all(&vec![b'x'; protocol::MAX_ENVELOPE_HEAD + 1]);
    });
    assert!(matches!(
        drive(reactor, io.receive_head(conn, &scope)),
        Err(Error::HeaderTooLarge)
    ));
    writer.join().unwrap();
    io.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    let response = || MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![Header {
            name: "content-length".into(),
            value: b"0".to_vec(),
        }],
    };
    let encoded = Codec::new(protocol::MAX_ENVELOPE_HEAD, 0)
        .encode_head(&response())
        .unwrap();
    let send_budget = protocol::MAX_ENVELOPE_HEAD + encoded.len();
    // Send scratch uses the head cap, but staging uses the actual encoded size.
    // Insufficient receive staging, send scratch, or send staging rejects before I/O.
    for (send, available, succeeds) in [
        (false, 4096 - 1, false),
        (true, protocol::MAX_ENVELOPE_HEAD - 1, false),
        (true, send_budget - 1, false),
        (true, send_budget, true),
    ] {
        let held = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limits().request_context_bytes.get() - baseline - available,
            )
            .unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut other) = UnixStream::pair().unwrap();
        other
            .set_read_timeout(Some(Duration::from_secs(1)))
            .unwrap();
        let conn = ConnectionLease::from_accepted(socket.into(), admission).unwrap();
        let operation = async {
            if send {
                io.send_head(conn, response(), &scope).await.map(|_| ())
            } else {
                io.receive_head(conn, &scope).await.map(|_| ())
            }
        };
        if succeeds {
            assert_eq!(drive(reactor, operation), Ok(()));
            let mut received = vec![0; encoded.len()];
            other.read_exact(&mut received).unwrap();
            assert_eq!(received, encoded);
        } else {
            let mut operation = std::pin::pin!(operation);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert_eq!(
                operation.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded)),
                "send={send}, available={available}"
            );
            // The failed admission must close without sending even a partial head.
            let mut byte = [0];
            assert_eq!(other.read(&mut byte).unwrap(), 0);
        }
        assert_eq!(reactor.in_flight(), 0);
        io.reclaim_buffer();
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        drop(held);
    }
}
