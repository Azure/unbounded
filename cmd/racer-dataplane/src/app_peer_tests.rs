//! Exercise assembled peer I/O, including socket session signatures and full paths.
use super::*;
use crate::{
    http::{
        codec::{Codec, Header, MessageHead, StartLine},
        pool::ConnectionLease,
    },
    model::{
        identity::*,
        limits::ResourceClass,
        metadata::{ExpiresAt, MetadataSelector, ObjectMetadata},
    },
    peer::wire::{
        self, FetchMode, LogicalCodec, Operation as PeerOperation, PeerRequest, PeerResponse,
        WireCodec,
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
    config.limits.header_bytes = NonZeroUsize::new(32 * 1024).unwrap();
    config.limits.range_window_pages = NonZeroUsize::new(1).unwrap();
    // Use the exact worker progress floor rather than the generous fixture budget.
    config.limits.request_context_bytes =
        NonZeroUsize::new(wire::MIN_REQUEST_CONTEXT_BYTES + 4 * 32 * 1024).unwrap();
    partition_limits(&config.limits, 1, false).unwrap();
    integration_tests::local_worker(&config, &Arc::new(NodeState::default()), 0)
}

#[test]
fn assembled_peer_io_carries_maximum_client_context_over_eight_signed_links() {
    let (app, runtime, _engine) = application();
    let io = app.peers.transport_io(); // Also asserts requester/server share this I/O.
    let admission = &runtime.admission;
    let reactor = &runtime.reactor;
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let signers = network(wire::MAX_HOPS + 1);
    let forwarding: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
    let codec = wire::SecurityCodec::new(
        admission.clone(),
        Rc::new(BufferPool::new(admission.clone())),
    );
    let crypto = CredentialCrypto::new(
        Rc::new(crate::security::keyring::tests::keys()),
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
            destination: node(wire::MAX_HOPS),
            visited: vec![node(0)],
            remaining_links: wire::MAX_HOPS as u8,
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
    for index in 1..=wire::MAX_HOPS {
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
        if index == wire::MAX_HOPS {
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
    let mut response = forwarding[wire::MAX_HOPS]
        .sign_response(
            destination.binding(),
            PeerResponse::Metadata(ObjectMetadata {
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
    assert_eq!(
        opened.metadata.as_ref().unwrap().as_header().unwrap(),
        metadata
    );
    assert_eq!(
        opened
            .authorization
            .as_ref()
            .unwrap()
            .expose_for_origin()
            .unwrap(),
        authorization
    );
    drop(opened);
    for index in (1..=wire::MAX_HOPS).rev() {
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
            wire::MAX_HOPS - index
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
    let floor = wire::MIN_REQUEST_CONTEXT_BYTES + 4 * config.limits.header_bytes.get();
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
        assert_eq!(plan.pairs.len(), 2);
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
    use std::io::Write;
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
            target: wire::REQUEST_TARGET.into(),
        },
        headers: vec![Header {
            name: "x".into(),
            value: vec![b'x'; wire::MAX_ENVELOPE_HEAD],
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
        let _ = other.write_all(&vec![b'x'; wire::MAX_ENVELOPE_HEAD + 1]);
    });
    assert!(matches!(
        drive(reactor, io.receive_head(conn, &scope)),
        Err(Error::HeaderTooLarge)
    ));
    writer.join().unwrap();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    // No I/O or signing begins when receive staging or send scratch cannot fit.
    for (send, available) in [
        (false, wire::MAX_ENVELOPE_HEAD - 1),
        (true, 2 * wire::MAX_ENVELOPE_HEAD - 1),
    ] {
        let held = admission
            .reserve(
                None,
                ResourceClass::RequestContext,
                admission.limits().request_context_bytes.get() - baseline - available,
            )
            .unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, _other) = UnixStream::pair().unwrap();
        let conn = ConnectionLease::from_accepted(socket.into(), admission).unwrap();
        let result = if send {
            drive(
                reactor,
                io.send_head(
                    conn,
                    MessageHead {
                        start: StartLine::Response { status: 200 },
                        headers: vec![Header {
                            name: "content-length".into(),
                            value: b"0".to_vec(),
                        }],
                    },
                    &scope,
                ),
            )
            .map(|_| ())
        } else {
            drive(reactor, io.receive_head(conn, &scope)).map(|_| ())
        };
        assert_eq!(result, Err(Error::Overloaded));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        drop(held);
    }
}
