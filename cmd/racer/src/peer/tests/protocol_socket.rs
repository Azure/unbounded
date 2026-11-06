//! Signed envelopes, routing contracts, and real socket/session exchanges.

use super::*;
use racer_io::ring::IoBuffer;
use racer_wire::control::CacheId;

#[test]
fn signed_opaque_relay_roundtrip_and_exact_attempt_binding() {
    let env = racer_io::Env::host();
    let signers = signers(&env);
    let auth = signers
        .iter()
        .map(|s| Forwarding::new(s.clone()))
        .collect::<Vec<_>>();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let codec = codec(&admission);
    let local = request(&env, &admission, 7);
    let scope = local.origin.scope().clone();
    let (signed, outstanding) = auth[0].sign_request_to(local, signers[1].node()).unwrap();
    let signature = signed.authentication.original.signature.clone();
    let (envelope, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0).unwrap(),
        false,
        &peer_wire::test_util::AnyChain,
    )
    .unwrap();
    let verified = auth[1]
        .verify_request(codec.request(envelope, &scope).unwrap())
        .unwrap();
    assert_eq!(
        verified.request().origin.reservation.key(),
        Some(&CacheId(CACHE.into()))
    );
    let reverse = verified.binding().clone();
    let mut budget = verified.request().route.clone();
    budget.visited.push(signers[1].node().clone());
    budget.remaining_links -= 1;
    let forwarded = auth[1]
        .append_request(verified, signers[2].node(), budget)
        .unwrap();
    assert_eq!(forwarded.authentication.original.signature, signature);
    assert_eq!(
        forwarded
            .request
            .origin
            .authorization
            .as_ref()
            .unwrap()
            .ciphertext,
        vec![5; 32]
    );
    let (envelope, _) = decode_envelope(
        encode_envelope(&forwarded.authentication, false, 0).unwrap(),
        false,
        &peer_wire::test_util::AnyChain,
    )
    .unwrap();
    let admitted = auth[2]
        .verify_request(codec.request(envelope, &scope).unwrap())
        .unwrap();
    let reply = auth[2]
        .sign_response(admitted.binding(), PeerResponse::Miss)
        .unwrap();
    let reply_signature = reply.authentication.original.signature.clone();
    let verified = auth[1].verify_response(reply, &reverse).unwrap();
    let reply = auth[1]
        .append_response(verified, signers[0].node())
        .unwrap();
    assert_eq!(reply.authentication.original.signature, reply_signature);
    let (envelope, _) = decode_envelope(
        encode_envelope(&reply.authentication, true, 0).unwrap(),
        true,
        &peer_wire::test_util::AnyChain,
    )
    .unwrap();
    let decoded = codec.response(envelope, vec![], &scope).unwrap();
    let (_, other) = auth[0]
        .sign_request_to(request(&env, &admission, 8), signers[1].node())
        .unwrap();
    assert!(auth[0].verify_response(decoded, &other).is_err());
    assert!(matches!(
        auth[0]
            .verify_response(reply, &outstanding)
            .unwrap()
            .response(),
        PeerResponse::Miss
    ));
}

#[test]
fn not_found_is_authenticated_through_relay_and_restricted_to_fresh_acquire() {
    use crate::peer::protocol as p;
    use http1::StartLine;
    let env = racer_io::Env::host();
    let signers = signers(&env);
    let auth: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let codec = codec(&admission);
    for role in ["fresh", "copy", "pinned", "page", "relay"] {
        let mut local = request(&env, &admission, 20);
        let object = local.origin.object.clone();
        local.operation = if role == "page" {
            Operation::Page {
                page: PageId {
                    version: ObjectVersion {
                        object,
                        etag: StrongEtag::test_value("old"),
                    },
                    number: PageNumber(0),
                },
                mode: FetchMode::Acquire,
            }
        } else {
            Operation::Metadata {
                object,
                selector: if role == "pinned" {
                    MetadataSelector::Pinned(StrongEtag::test_value("old"))
                } else {
                    MetadataSelector::Fresh
                },
                mode: if role == "copy" {
                    FetchMode::CopyOnly
                } else {
                    FetchMode::Acquire
                },
            }
        };
        let scope = local.origin.scope().clone();
        let (signed, outstanding) = auth[0].sign_request_to(local, signers[1].node()).unwrap();
        let relay = auth[1].verify_request(signed).unwrap();
        let reverse = relay.binding().clone();
        let mut budget = relay.request().route.clone();
        budget.visited.push(signers[1].node().clone());
        budget.remaining_links -= 1;
        if role == "relay" {
            assert!(matches!(
                auth[1].sign_response(&reverse, PeerResponse::NotFound),
                Err(Error::Unauthorized)
            ));
        }
        let forwarded = auth[1]
            .append_request(relay, signers[2].node(), budget)
            .unwrap();
        let admitted = auth[2].verify_request(forwarded).unwrap();
        if matches!(role, "copy" | "pinned" | "page") {
            assert!(matches!(
                auth[2].sign_response(admitted.binding(), PeerResponse::NotFound),
                Err(Error::Unauthorized)
            ));
        }
        // Construct valid signatures directly to exercise receiver-side role
        // enforcement independently of the signing facade's checks.
        let signer = if role == "relay" { 1 } else { 2 };
        let path: Vec<_> = signers[..=signer]
            .iter()
            .map(|s| s.node().clone())
            .collect();
        let mut head = p::response_head(
            &PeerResponse::NotFound,
            &crate::peer::protocol::signed_digest(&admitted.signed().authentication.original)
                .unwrap(),
            &path,
        )
        .unwrap();
        p::push(&mut head, "racer-receiver", &signers[signer - 1].node().0);
        let response = protocol::SignedResponse {
            authentication: crate::peer::forwarding::ForwardedHead {
                original: Arc::new(signers[signer].sign(head).unwrap()),
                hops: vec![],
            },
            response: PeerResponse::NotFound,
        };
        if role != "fresh" {
            let (receiver, binding) = if role == "relay" {
                (0, &outstanding)
            } else {
                (1, &reverse)
            };
            assert!(matches!(
                auth[receiver].verify_response(response, binding),
                Err(Error::Unauthorized)
            ));
            continue;
        }
        assert!(matches!(
            response.authentication.original.head.start,
            StartLine::Response { status: 404 }
        ));
        let original = response.authentication.original.clone();
        let reverse_response = auth[1]
            .append_response(
                auth[1].verify_response(response, &reverse).unwrap(),
                signers[0].node(),
            )
            .unwrap();
        assert_eq!(
            reverse_response.authentication.original.signature,
            original.signature
        );
        // The outer HTTP status is framing, while the inner 404 and outcome are signed.
        let envelope = encode_envelope(&reverse_response.authentication, true, 0).unwrap();
        assert!(matches!(
            envelope.start,
            StartLine::Response { status: 200 }
        ));
        let (envelope, length) =
            decode_envelope(envelope, true, &peer_wire::test_util::AnyChain).unwrap();
        assert_eq!(length, 0);
        let decoded = codec.response(envelope, vec![], &scope).unwrap();
        let (_, other) = auth[0]
            .sign_request_to(request(&env, &admission, 21), signers[1].node())
            .unwrap();
        assert!(auth[0].verify_response(decoded, &other).is_err());
        assert!(matches!(
            auth[0]
                .verify_response(reverse_response, &outstanding)
                .unwrap()
                .response(),
            PeerResponse::NotFound
        ));
        for (field, value) in [
            ("status", "200"),
            ("racer-outcome", "miss"),
            ("racer-outcome", "unknown"),
        ] {
            let mut changed = crate::test_support::security::clone_head(&original);
            if field == "status" {
                changed.head.start = StartLine::Response { status: 200 };
            } else {
                changed
                    .head
                    .headers
                    .iter_mut()
                    .find(|h| h.name == field)
                    .unwrap()
                    .value = value.as_bytes().to_vec();
            }
            let envelope = crate::peer::forwarding::ForwardedHead {
                original: Arc::new(changed),
                hops: vec![],
            };
            assert!(codec.response(envelope, vec![], &scope).is_err());
        }
        let mut changed = crate::test_support::security::clone_head(&original);
        // A structurally valid alternate outcome/status still needs a valid signature.
        changed.head.start = StartLine::Response { status: 200 };
        changed
            .head
            .headers
            .iter_mut()
            .find(|h| h.name == "racer-outcome")
            .unwrap()
            .value = b"miss".to_vec();
        let envelope = crate::peer::forwarding::ForwardedHead {
            original: Arc::new(changed),
            hops: vec![],
        };
        let decoded = codec.response(envelope, vec![], &scope).unwrap();
        assert!(auth[1].verify_response(decoded, &reverse).is_err());
        let envelope = crate::peer::forwarding::ForwardedHead {
            original,
            hops: vec![],
        };
        assert!(codec.response(envelope, vec![1], &scope).is_err());
    }
}

#[test]
fn changed_operation_credentials_replay_and_deadlines_fail() {
    let env = racer_io::Env::host();
    let signers = signers(&env);
    let sender = Forwarding::new(signers[0].clone());
    let receiver = Forwarding::new(signers[2].clone());
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let codec = codec(&admission);
    let local = request(&env, &admission, 1);
    let scope = local.origin.scope().clone();
    let (mut signed, _) = sender.sign_request(local).unwrap();
    signed
        .request
        .origin
        .authorization
        .as_mut()
        .unwrap()
        .ciphertext[0] ^= 1;
    assert!(receiver.verify_request(signed).is_err());
    let (signed, _) = sender.sign_request(request(&env, &admission, 2)).unwrap();
    let (envelope, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0).unwrap(),
        false,
        &peer_wire::test_util::AnyChain,
    )
    .unwrap();
    receiver.verify_request(signed).unwrap();
    receiver
        .verify_request(codec.request(envelope, &scope).unwrap())
        .unwrap();
    let mut expired = request(&env, &admission, 3);
    expired.route.deadline = Deadline(Instant::now() - Duration::from_secs(1));
    assert!(matches!(
        request_scope(&expired, &scope),
        Err(Error::DeadlineExceeded)
    ));
    let local = request(&env, &admission, 4);
    let narrowed = request_scope(&local, &scope).unwrap();
    scope.cancel().unwrap();
    assert_eq!(narrowed.check(), Err(Error::Canceled));
}

#[test]
fn search_view_consumes_ingress_without_changing_signed_route() {
    let env = racer_io::Env::host();
    let admission = flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let request = request(&env, &admission, 1);
    let initial = search_budget(&request.route, &NodeId(A.into())).unwrap();
    assert!(initial.visited.is_empty());
    assert_eq!(initial.remaining_links, 4);
    let relay = search_budget(&request.route, &NodeId(B.into())).unwrap();
    assert_eq!(relay.visited, request.route.visited);
    assert_eq!(relay.remaining_links, 3);
    assert_eq!(relay.deadline.0, request.route.deadline.0);
    assert_eq!(request.route.remaining_links, 4);
}

#[test]
fn server_authenticates_before_copy_only_service_and_signs_failures() {
    use crate::http::Codec;
    use crate::runtime::Reactor;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use std::cell::Cell;
    use std::num::NonZeroU32;
    let env = racer_io::Env::host();
    struct Service(Rc<Cell<usize>>);
    impl server::LocalPageService for Service {
        fn serve_peer<'a>(
            &'a self,
            request: crate::peer::forwarding::VerifiedRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                assert!(matches!(
                    request.request().operation,
                    Operation::Metadata {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                ));
                self.0.set(self.0.get() + 1);
                Err(Error::VersionUnavailable)
            })
        }
    }
    let signers = signers(&env);
    let sender = Forwarding::new(signers[0].clone());
    let destination = Rc::new(Forwarding::new(signers[2].clone()));
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, B, C]
                .iter()
                .enumerate()
                .map(|(i, n)| Member {
                    node: NodeId((*n).into()),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::test_support::published_membership(membership),
            racer_io::Clock::host(),
        )
        .unwrap(),
    );
    let paths = Rc::new(Paths::new(
        Rc::new(LinkHealth::on(env.ctx.clock.clone())),
        4,
    ));
    let outbound = NoOutbound::new(&env, signers[2].clone(), network.clone());
    let relay = Rc::new(Relay::new(
        paths,
        destination.clone(),
        outbound.requester.clone(),
        admission.clone(),
        network.clone(),
    ));
    let reactor = Rc::new(Reactor::new(admission.clone(), &env));
    let io = Rc::new(crate::http::new_io(
        reactor,
        Codec::new(65536),
        admission.clone(),
        16777232,
    ));
    let calls = Rc::new(Cell::new(0));
    let server = server::PeerServer::for_test(
        io,
        destination,
        admission.clone(),
        Rc::new(Service(calls.clone())),
        relay,
        Rc::new(codec(&admission)),
        signers[2].clone(),
    );
    let local = request(&env, &admission, 1);
    let scope = local.origin.scope().clone();
    let (mut forged, _) = sender.sign_request(local).unwrap();
    if let Some(head) = Arc::get_mut(&mut forged.authentication.original) {
        head.signature[0] ^= 1;
    }
    // The binding also retains the original Arc, so mutate the logical fields.
    forged.request.route.destination = NodeId(B.into());
    assert!(futures::executor::block_on(server.dispatch(forged, &scope)).is_err());
    assert_eq!(calls.get(), 0);
    let (valid, binding) = sender.sign_request(request(&env, &admission, 2)).unwrap();
    let response = futures::executor::block_on(server.dispatch(valid, &scope)).unwrap();
    assert_eq!(calls.get(), 1);
    assert!(matches!(
        sender
            .verify_response(response, &binding)
            .unwrap()
            .response(),
        PeerResponse::VersionUnavailable
    ));
}

#[test]
fn relay_dispatch_preserves_reverse_path_and_fails_closed_on_link_loss() {
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    let env = racer_io::Env::host();
    for fail in [false, true] {
        let signers = signers(&env);
        let origin = Forwarding::new(signers[0].clone());
        let forwarding = Rc::new(Forwarding::new(signers[1].clone()));
        let socket = SocketFixture::new(&env, 2);
        let admission = socket.admission.clone();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap().to_string();
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [A, B, C]
                    .iter()
                    .map(|n| Member {
                        node: NodeId((*n).into()),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: address.clone(),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let network = Rc::new(
            PeerNetwork::new(
                NodeId(B.into()),
                crate::test_support::published_membership(membership.clone()),
                racer_io::Clock::host(),
            )
            .unwrap(),
        );
        let paths = Rc::new(Paths::new(
            Rc::new(LinkHealth::on(env.ctx.clock.clone())),
            1,
        ));
        let requester = Rc::new(Requester::new(
            paths.clone(),
            forwarding.clone(),
            socket.transfers(signers[1].clone()),
            network.clone(),
        ));
        let relay = Relay::new(
            paths,
            forwarding.clone(),
            requester.clone(),
            admission.clone(),
            network,
        );
        let local = request(&env, &admission, 9);
        let scope = local.origin.scope().clone();
        let (signed, binding) = origin.sign_request_to(local, signers[1].node()).unwrap();
        let ingress = forwarding.verify_request(signed).unwrap();
        let destination = async {
            let fd = socket
                .reactor
                .accept(Rc::new(racer_io::ring::Descriptor::from(listener)), &scope)
                .await?;
            let connection = crate::http::from_accepted(fd, &admission)?;
            let connection =
                crate::peer::protocol::accept(&socket.io, connection, signers[2].clone(), &scope)
                    .await?;
            let received = socket.io.receive_head(connection, &scope).await?;
            let (envelope, length) = decode_envelope(
                received.value,
                false,
                crate::peer::protocol::chains(&received.connection)?,
            )?;
            assert_eq!(length, 0);
            let auth = Forwarding::new(signers[2].clone());
            let admitted = auth.verify_request(socket.codec.request(envelope, &scope)?)?;
            assert_eq!(admitted.request().route.remaining_links, 3);
            assert_eq!(
                admitted
                    .request()
                    .origin
                    .authorization
                    .as_ref()
                    .unwrap()
                    .ciphertext,
                vec![5; 32]
            );
            assert_eq!(
                admission.used(ResourceClass::Relay),
                1,
                "relay reservation spans downstream I/O"
            );
            if !fail {
                let response = auth.sign_response(admitted.binding(), PeerResponse::Miss)?;
                socket
                    .io
                    .send_head(
                        received.connection,
                        encode_envelope(&response.authentication, true, 0)?,
                        &scope,
                    )
                    .await?;
            }
            Ok::<_, Error>(())
        };
        crate::peer::forwarding::ENVELOPE_VERIFICATIONS.with(|count| count.set(0));
        let (result, served) = crate::http::tests::drive(&socket.reactor, async {
            futures::join!(relay.forward(ingress, membership, &scope), destination)
        });
        served.unwrap();
        assert_eq!(requester.outbound_requests(), 1);
        // The relay verifies the downstream response once, inside its exchange,
        // and appends its reverse hop without verifying it again.
        assert_eq!(
            crate::peer::forwarding::ENVELOPE_VERIFICATIONS.with(std::cell::Cell::get),
            usize::from(!fail)
        );
        if fail {
            assert!(matches!(result, Err(Error::Io)));
        } else {
            let response = result.unwrap();
            assert_eq!(response.authentication.hops.len(), 1);
            assert!(matches!(
                origin
                    .verify_response(response, &binding)
                    .unwrap()
                    .response(),
                PeerResponse::Miss
            ));
        }
        assert_eq!(admission.used(ResourceClass::Relay), 0);
        assert_eq!(socket.reactor.in_flight(), 0);
    }
}

/// A relay that holds the destination silent (a ping
/// to it went unanswered) answers `Unavailable` at once instead of sending
/// the request to a peer that would never answer it.
#[test]
fn relay_fails_a_request_for_a_silent_destination_at_once() {
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    let env = racer_io::Env::host();
    let signers = signers(&env);
    let origin = Forwarding::new(signers[0].clone());
    let forwarding = Rc::new(Forwarding::new(signers[1].clone()));
    let socket = SocketFixture::new(&env, 2);
    let admission = socket.admission.clone();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, B, C]
                .iter()
                .map(|n| Member {
                    node: NodeId((*n).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:9".into(),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(B.into()),
            crate::test_support::published_membership(membership.clone()),
            racer_io::Clock::host(),
        )
        .unwrap(),
    );
    let health = Rc::new(
        LinkHealth::on(env.ctx.clock.clone())
            .with_silence(std::time::Duration::ZERO, std::time::Duration::from_secs(1)),
    );
    let destination = NodeId(C.into());
    let page = crate::model::PageId {
        version: crate::model::ObjectVersion {
            object: crate::model::ObjectId {
                cache: CacheId("cache".into()),
                key: crate::model::CacheKey([3; 32]),
            },
            etag: crate::model::StrongEtag::test_value("v"),
        },
        number: crate::model::PageNumber(0),
    };
    let now = racer_io::Clock::host().now();
    assert!(health.silence_timed_out(&destination, page, now, 1));
    assert_eq!(health.due_pings(now, 1).len(), 1);
    assert!(health.ping_outcome(&destination, Some(false), now));
    let paths = Rc::new(Paths::new(health, 1));
    let requester = Rc::new(Requester::new(
        paths.clone(),
        forwarding.clone(),
        socket.transfers(signers[1].clone()),
        network.clone(),
    ));
    let relay = Relay::new(
        paths,
        forwarding.clone(),
        requester.clone(),
        admission.clone(),
        network,
    );
    let local = request(&env, &admission, 11);
    assert_eq!(local.route.destination, destination);
    let scope = local.origin.scope().clone();
    let (signed, _) = origin.sign_request_to(local, signers[1].node()).unwrap();
    let ingress = forwarding.verify_request(signed).unwrap();
    let result =
        crate::http::tests::drive(&socket.reactor, relay.forward(ingress, membership, &scope));
    assert!(
        matches!(result, Err(Error::Unavailable)),
        "{:?}",
        result.err()
    );
    assert_eq!(requester.outbound_requests(), 0);
    assert_eq!(admission.used(ResourceClass::Relay), 0);
}

#[test]
fn equal_cost_signed_receiver_survives_wire_recompute_and_cache_eviction() {
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use racer_core::route::LinkOutcome;
    let env = racer_io::Env::host();
    let signers = signers(&env);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let nodes = std::iter::once(A.to_owned())
        .chain((0..1498).map(|i| format!("00000002-1111-4111-8111-{i:012x}")))
        .chain(std::iter::once(C.to_owned()));
    let members = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            nodes
                .enumerate()
                .map(|(i, node)| Member {
                    node: NodeId(node),
                    shares: std::num::NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap(),
                    peer_endpoint: "127.0.0.1:7443".into(),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let paths = Paths::new(Rc::new(LinkHealth::on(env.ctx.clock.clone())), 1);
    let health = Rc::new(LinkHealth::on(env.ctx.clock.clone()));
    let cold = Paths::new(health.clone(), 0);
    let mut selected = std::collections::BTreeSet::new();
    for attempt in 1..=32 {
        let original = request(&env, &admission, attempt);
        let scope = original.origin.scope().clone();
        let budget = super::search_budget(&original.route, &NodeId(A.into())).unwrap();
        let planned = futures::executor::block_on(paths.shortest_async(
            members.clone(),
            &NodeId(A.into()),
            &budget,
            &scope,
        ))
        .unwrap();
        let next = &planned.nodes[1];
        selected.insert(next.clone());
        let (signed, _) = Forwarding::new(signers[0].clone())
            .sign_request_to(original, next)
            .unwrap();
        let receiver =
            crate::peer::protocol::receiver(&signed.authentication.original.head).unwrap();
        let (envelope, _) = decode_envelope(
            encode_envelope(&signed.authentication, false, 0).unwrap(),
            false,
            &peer_wire::test_util::AnyChain,
        )
        .unwrap();
        let decoded = codec(&admission).request(envelope, &scope).unwrap();
        let search = super::search_budget(&decoded.request.route, &NodeId(A.into())).unwrap();
        let recomputed = futures::executor::block_on(cold.shortest_async(
            members.clone(),
            &NodeId(A.into()),
            &search,
            &scope,
        ))
        .unwrap();
        assert_eq!(recomputed.nodes, planned.nodes);
        assert_eq!(recomputed.nodes[1], receiver);
        health
            .observe_at(
                &receiver,
                LinkOutcome::Timeout,
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
        let changed = cold
            .shortest(&env, members.clone(), &NodeId(A.into()), &search)
            .unwrap();
        assert_ne!(changed.nodes[1], receiver);
        assert_eq!(changed.nodes.len(), planned.nodes.len());
        health.observe(&receiver, LinkOutcome::Success).unwrap();
        // Force eviction before repeating the exact signed state.
        let mut other = budget.clone();
        other.destination = members.members()[1498].node.clone();
        paths
            .shortest(&env, members.clone(), &NodeId(A.into()), &other)
            .unwrap();
        assert_eq!(
            paths
                .shortest(&env, members.clone(), &NodeId(A.into()), &search)
                .unwrap()
                .nodes,
            planned.nodes
        );
    }
    // Mesh v2: the primary delegate is unique, so every attempt picks it.
    assert_eq!(selected.len(), 1);
}

#[test]
fn refused_socket_opens_only_immediate_link_and_selects_bounded_alternate() {
    use crate::http::Codec;
    use crate::runtime::Reactor;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use std::task::Context;
    use std::task::Poll;
    let env = racer_io::Env::host();
    let (signers, _) = identities(&env);
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let reactor = Rc::new(Reactor::new(admission.clone(), &env));
    let io = Rc::new(crate::http::new_io(
        reactor.clone(),
        Codec::new(protocol::MAX_ENVELOPE_HEAD),
        admission.clone(),
        0,
    ));
    let pool = Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 2));
    let transfers = Rc::new(transport::Transfers::new(
        pool,
        io,
        admission.clone(),
        Rc::new(codec(&admission)),
        signers[0].clone(),
    ));
    let closed = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = closed.local_addr().unwrap().to_string();
    drop(closed);
    let members = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, B, C]
                .iter()
                .map(|name| Member {
                    node: NodeId((*name).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: address.clone(),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let health = Rc::new(LinkHealth::new(36, racer_io::Clock::host()));
    let paths = Rc::new(Paths::new(health.clone(), 4));
    let requester = Requester::new(
        paths.clone(),
        Rc::new(Forwarding::new(signers[0].clone())),
        transfers,
        Rc::new(
            PeerNetwork::new(
                NodeId(A.into()),
                crate::test_support::published_membership(members.clone()),
                racer_io::Clock::host(),
            )
            .unwrap(),
        ),
    );
    let original = request(&env, &admission, 1);
    let budget = super::search_budget(&original.route, &NodeId(A.into())).unwrap();
    let scope = original.origin.scope().clone();
    let direct = paths
        .shortest(&env, members.clone(), &NodeId(A.into()), &budget)
        .unwrap();
    assert_eq!(direct.nodes, vec![NodeId(A.into()), NodeId(C.into())]);
    // Reject a signed receiver that differs from wire recomputation before I/O.
    let mut page_request = request(&env, &admission, 2);
    page_request.operation = Operation::Page {
        page: PageId {
            version: ObjectVersion {
                object: page_request.origin.object.clone(),
                etag: StrongEtag::test_value("v3"),
            },
            number: PageNumber(0),
        },
        mode: FetchMode::CopyOnly,
    };
    let (wrong, _) = Forwarding::new(signers[0].clone())
        .sign_request_to(page_request, &NodeId(B.into()))
        .unwrap();
    assert!(matches!(
        futures::executor::block_on(Requester::exchange(
            &requester,
            wrong,
            members.clone(),
            &scope
        )),
        Err(Error::Unavailable)
    ));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert!(health.available(&NodeId(B.into())).unwrap());
    let mut original = original;
    original.operation = Operation::Page {
        page: PageId {
            version: ObjectVersion {
                object: original.origin.object.clone(),
                etag: StrongEtag::test_value("v3"),
            },
            number: PageNumber(0),
        },
        mode: FetchMode::CopyOnly,
    };
    let mut attempt = requester.request(original, members.clone(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let result = loop {
        scope.check().unwrap();
        if let Poll::Ready(result) = attempt.as_mut().poll(&mut cx) {
            break result;
        }
        reactor.poll_budgeted(64).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };
    assert!(matches!(
        result,
        Err(Error::Os(libc::ECONNREFUSED) | Error::Unavailable)
    ));
    assert!(!health.available(&NodeId(C.into())).unwrap());
    assert!(health.available(&NodeId(B.into())).unwrap());
    let alternate = paths
        .shortest(&env, members.clone(), &NodeId(A.into()), &budget)
        .unwrap();
    assert_eq!(
        alternate.nodes,
        vec![NodeId(A.into()), NodeId(B.into()), NodeId(C.into())]
    );
    assert!(alternate.nodes.len() - 1 <= usize::from(budget.remaining_links));
    let mut exhausted = budget.clone();
    exhausted.remaining_links = 1;
    assert!(
        paths
            .shortest(&env, members, &NodeId(A.into()), &exhausted)
            .is_err()
    );
    drop(attempt);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}

#[test]
fn requester_and_server_negotiate_and_exchange_over_real_tcp() {
    let env = racer_io::Env::host();
    for case in [
        "success",
        "scope",
        "attempt",
        "membership",
        "cancel",
        "deadline",
    ] {
        signed_tcp_case(&env, case);
    }
}

fn signed_tcp_case(env: &racer_io::Env, case: &str) {
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use std::net::TcpListener;
    use std::task::Context;
    use std::task::Poll;
    struct Local;
    impl server::LocalPageService for Local {
        fn serve_peer<'a>(
            &'a self,
            request: crate::peer::forwarding::VerifiedRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            scope: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                scope.check()?;
                // The authenticated request has its own budget, independent of
                // the server's five-second header cap.
                assert_eq!(scope.deadline.0, request.request().route.deadline.0);
                assert_eq!(
                    request
                        .request()
                        .origin
                        .authorization
                        .as_ref()
                        .unwrap()
                        .ciphertext,
                    vec![5; 32]
                );
                Ok(PeerResponse::Miss)
            })
        }
    }
    let (signers, _) = identities(env);
    let fixture = SocketFixture::new(env, 2);
    let transfers = fixture.transfers(signers[0].clone());
    let SocketFixture {
        admission,
        reactor,
        io,
        codec,
        ..
    } = fixture;
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let address = listener.local_addr().unwrap();
    // Missing authentication is rejected by the constructor's compile-fail test.
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, C]
                .iter()
                .enumerate()
                .map(|(i, n)| Member {
                    node: NodeId((*n).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: if i == 1 {
                        address.to_string()
                    } else {
                        "127.0.0.1:9000".into()
                    },
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let source_network = Rc::new(
        PeerNetwork::new(
            NodeId(A.into()),
            crate::test_support::published_membership(membership.clone()),
            racer_io::Clock::host(),
        )
        .unwrap(),
    );
    let destination_network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::test_support::published_membership(membership.clone()),
            racer_io::Clock::host(),
        )
        .unwrap(),
    );
    let destination_auth = Rc::new(Forwarding::new(signers[2].clone()));
    let metrics = crate::telemetry::Metrics::default();
    let adaptive = crate::peer::AdaptivePeers::new(
        Default::default(),
        metrics.clone(),
        racer_io::Clock::host(),
    )
    .unwrap();
    let paths = Rc::new(
        Paths::new(Rc::new(LinkHealth::on(env.ctx.clock.clone())), 4).with_peer_admission(adaptive),
    );
    let outbound = NoOutbound::new(env, signers[2].clone(), destination_network.clone());
    let relay = Rc::new(Relay::new(
        paths.clone(),
        destination_auth.clone(),
        outbound.requester.clone(),
        admission.clone(),
        destination_network.clone(),
    ));
    let server = server::PeerServer::for_test(
        io,
        destination_auth,
        admission.clone(),
        Rc::new(Local),
        relay,
        codec,
        signers[2].clone(),
    )
    .with_request_timeout(Duration::from_secs(5));
    let health = paths.link_health();
    let requester = Requester::new(
        paths,
        Rc::new(Forwarding::new(signers[0].clone())),
        transfers,
        source_network,
    );
    let mut local = request(env, &admission, 1);
    let scope = local.origin.scope().clone();
    let expected = match case {
        "scope" => {
            local.route.request = RequestId([9; 16]);
            Some(Error::InvalidRequest)
        }
        "attempt" => {
            local.route.attempt = AttemptId([9; 16]);
            Some(Error::InvalidRequest)
        }
        "membership" => {
            local.route.membership = MembershipVersion(2);
            Some(Error::IncompatibleMembership)
        }
        "cancel" => {
            scope.cancel().unwrap();
            Some(Error::Canceled)
        }
        "deadline" => {
            local.route.deadline.0 = Instant::now();
            Some(Error::DeadlineExceeded)
        }
        _ => None,
    };
    if let Some(expected) = expected {
        let result = futures::executor::block_on(requester.request(local, membership, &scope));
        assert!(matches!(result, Err(error) if error == expected), "{case}");
        assert_eq!(admission.used(ResourceClass::Connection), 0, "{case}");
        assert_eq!(reactor.in_flight(), 0, "{case}");
        assert!(
            matches!(listener.accept(), Err(error) if error.kind() == std::io::ErrorKind::WouldBlock),
            "{case} must fail before socket checkout"
        );
        return;
    }
    let listener_scope = RequestScope::host(
        RequestId([0; 16]),
        scope.deadline.0 + Duration::from_secs(60),
    )
    .unwrap();
    let server_work = async {
        let fd = reactor
            .accept(Rc::new(racer_io::ring::Descriptor::from(listener)), &scope)
            .await?;
        let connection = crate::http::from_accepted(fd, &admission)?;
        server.serve_connection(connection, &listener_scope).await?;
        Ok::<(), Error>(())
    };
    let exchange =
        async { futures::try_join!(requester.request(local, membership, &scope), server_work) };
    let mut exchange = std::pin::pin!(exchange);
    let mut context = Context::from_waker(futures::task::noop_waker_ref());
    let (response, ()) = loop {
        if let Poll::Ready(result) = std::future::Future::poll(exchange.as_mut(), &mut context) {
            break result.unwrap();
        }
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    };
    assert!(matches!(response.response(), PeerResponse::Miss));
    assert_eq!(
        metrics.count(crate::telemetry::Event::PeerAdmissionAccepted),
        1
    );
    assert_eq!(metrics.count(crate::telemetry::Event::PeerVerified), 1);
    assert_eq!(metrics.gauge(crate::telemetry::Gauge::PeerExchanges), 0);
    assert_eq!(
        health.tracked_links(),
        0,
        "application miss is not a broken link"
    );
}

#[test]
fn incoming_header_timeout_closes_silent_partial_and_idle_keepalive_peers() {
    use crate::http::Codec;
    use crate::runtime::Reactor;
    use crate::topology::LinkHealth;
    use crate::topology::Paths;
    use std::future::Future;
    use std::io::Read;
    use std::io::Write;
    use std::os::unix::net::UnixStream;
    use std::task::Context;
    use std::task::Poll;
    let env = racer_io::Env::host();

    struct Never;
    impl server::LocalPageService for Never {
        fn serve_peer<'a>(
            &'a self,
            _: crate::peer::forwarding::VerifiedRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async { panic!("incomplete headers must not dispatch") })
        }
    }
    fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        // Harness watchdog only, not an assertion about timeout latency.
        let watchdog = Instant::now() + Duration::from_secs(10);
        loop {
            if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
            assert!(Instant::now() < watchdog, "peer exchange did not finish");
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    // The long listener would trip the watchdog if header timeout wiring were
    // absent. The short listener case verifies the opposite deadline ordering.
    for case in [
        "silent", "partial", "trickle", "idle", "listener", "cancel", "drop",
    ] {
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone(), &env));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let io = Rc::new(crate::http::new_io(
            reactor.clone(),
            Codec::new(protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            crate::model::PAGE_BYTES + 16,
        ));
        let (signers, _) = identities(&env);
        let forwarding = Rc::new(Forwarding::new(signers[2].clone()));
        let outbound = NoOutbound::new(
            &env,
            signers[2].clone(),
            Rc::new(
                PeerNetwork::new(
                    signers[2].node().clone(),
                    Arc::new(crate::control::cell::PublicationCell::new(
                        crate::control::Snapshot::retention(2),
                    )),
                    racer_io::Clock::host(),
                )
                .unwrap(),
            ),
        );
        let relay = Rc::new(Relay::new(
            Rc::new(Paths::new(
                Rc::new(LinkHealth::on(env.ctx.clock.clone())),
                4,
            )),
            forwarding.clone(),
            outbound.requester.clone(),
            admission.clone(),
            Rc::new(
                PeerNetwork::new(
                    signers[2].node().clone(),
                    Arc::new(crate::control::cell::PublicationCell::new(
                        crate::control::Snapshot::retention(2),
                    )),
                    racer_io::Clock::host(),
                )
                .unwrap(),
            ),
        ));
        let mut server = server::PeerServer::for_test(
            io.clone(),
            forwarding,
            admission.clone(),
            Rc::new(Never),
            relay,
            Rc::new(codec(&admission)),
            signers[2].clone(),
        )
        .with_request_timeout(if matches!(case, "listener" | "cancel" | "drop") {
            Duration::from_secs(60)
        } else if case == "idle" {
            Duration::from_secs(5)
        } else {
            Duration::from_millis(30)
        });
        let scope = RequestScope::host(
            RequestId([7; 16]),
            Instant::now()
                + if case == "listener" {
                    Duration::from_millis(30)
                } else {
                    Duration::from_secs(60)
                },
        )
        .unwrap();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(10)))
            .unwrap();
        let mut connection = crate::http::from_accepted(socket.into(), &admission).unwrap();
        if case == "idle" {
            let client =
                crate::http::from_accepted(peer.try_clone().unwrap().into(), &admission).unwrap();
            let (client, accepted) = drive(&reactor, async {
                futures::try_join!(
                    crate::peer::protocol::connect(
                        &io,
                        client,
                        signers[0].clone(),
                        signers[2].node(),
                        &scope
                    ),
                    crate::peer::protocol::accept(&io, connection, signers[2].clone(), &scope)
                )
            })
            .unwrap();
            connection = accepted;
            drop(client);
            server = server.with_request_timeout(Duration::from_millis(30));
        }
        if matches!(case, "partial" | "trickle") {
            peer.write_all(b"POST /racer/peer/v1/request HTTP/1.1\r\nRacer-")
                .unwrap();
        }
        let mut work = server.serve_connection(connection, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert!(admission.used(ResourceClass::RequestContext) > baseline);
        assert_eq!(reactor.in_flight(), 1);
        if case == "silent" {
            reactor.poll_budgeted(128).unwrap();
            // Let the header deadline pass without driving completion. Expiry
            // alone must not release resources still owned by the reactor.
            std::thread::sleep(Duration::from_millis(30));
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert!(admission.used(ResourceClass::RequestContext) > baseline);
            assert_eq!(reactor.in_flight(), 1);
        }
        if matches!(case, "cancel" | "drop") {
            reactor.poll_budgeted(128).unwrap();
            scope.cancel().unwrap();
            // Cancellation cannot release the socket or staging before its CQE
            // fence, even when the whole peer exchange future is abandoned.
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert!(admission.used(ResourceClass::RequestContext) > baseline);
        }
        if case == "drop" {
            drop(work);
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            assert!(admission.used(ResourceClass::RequestContext) > baseline);
        } else {
            let expected = if case == "cancel" {
                Error::Canceled
            } else {
                Error::DeadlineExceeded
            };
            let result = drive(
                &reactor,
                futures::future::poll_fn(|cx| {
                    let result = work.as_mut().poll(cx);
                    if result.is_pending() && case == "trickle" {
                        // Continued progress must not renew receive_head's budget.
                        peer.write_all(b"x").unwrap();
                    }
                    result
                }),
            );
            assert!(matches!(result, Err(error) if error == expected), "{case}");
            drop(work);
        }
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
        let closed = peer.read(&mut [0; 1]);
        assert!(
            matches!(closed, Ok(0))
                || closed.is_err_and(|error| error.kind() == std::io::ErrorKind::ConnectionReset),
            "{case}"
        );
    }
}

#[test]
fn real_http_ciphertext_fragmentation_pool_reuse_and_truncation() {
    use crate::http::Codec;
    use crate::http::Endpoint;
    use crate::model::ExpiresAt;
    use crate::model::ObjectMetadata;
    use crate::model::PageEnvelope;
    use crate::peer::forwarding::ForwardedHead;
    use crate::peer::protocol;
    use crate::rdma;
    use crate::rdma::TransportPlan;
    use crate::runtime::Reactor;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Route;
    use racer_wire::control::RailId;
    use racer_wire::control::RailMapping;
    use std::net::TcpListener;
    use std::task::Context;
    use std::task::Poll;
    let env = racer_io::Env::host();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let reactor = Rc::new(Reactor::new(admission.clone(), &env));
    let io = Rc::new(crate::http::new_io(
        reactor.clone(),
        Codec::new(64 * 1024),
        admission.clone(),
        crate::model::PAGE_BYTES + 16,
    ));
    let pool = Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 1));
    let signers = signers(&env);
    let transfers = transport::Transfers::new(
        pool,
        io.clone(),
        admission.clone(),
        Rc::new(codec(&admission)),
        signers[0].clone(),
    );
    let buffers = BufferPool::new(admission.clone());
    let version = ObjectVersion {
        object: ObjectId {
            cache: CacheId(CACHE.into()),
            key: CacheKey([3; 32]),
        },
        etag: StrongEtag::parse(b"\"v1\"").unwrap(),
    };
    let body = vec![0x9a; 8192 + 16];
    let envelope = PageEnvelope {
        page_size: racer_wire::object::PageSize::DEFAULT,
        cipher: crate::model::PageCipher::Xaes256Gcm,
        page: PageId {
            version: version.clone(),
            number: PageNumber(0),
        },
        key_id: KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: 8192,
        ciphertext_length: body.len() as u32,
    };
    let page = buffers
        .ciphertext(
            admission
                .reserve(
                    Some(&CacheId(CACHE.into())),
                    ResourceClass::Ciphertext,
                    body.len(),
                )
                .unwrap(),
            envelope,
            body.clone(),
        )
        .unwrap();
    let response = PeerResponse::Page {
        metadata: ObjectMetadata {
            content_type: None,
            version,
            length: 8192,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        },
        ciphertext: page,
    };
    let mut head =
        protocol::response_head(&response, &[3; 32], &[NodeId(A.into()), NodeId(C.into())])
            .unwrap();
    protocol::push(&mut head, "racer-receiver", A);
    let authentication = ForwardedHead {
        original: Arc::new(signers[2].sign(head).unwrap()),
        hops: vec![],
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let expected = body.clone();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, C]
                .into_iter()
                .map(|node| Member {
                    node: NodeId(node.into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: listener.local_addr().unwrap().to_string(),
                    rails: vec![RailMapping {
                        rail: RailId(0),
                        device: "fabric".into(),
                        port: 1,
                        gid: None,
                        numa_node: None,
                    }],
                    site: "same-site".into(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let route = Route {
        membership: membership.clone(),
        nodes: vec![NodeId(A.into()), NodeId(C.into())],
    };
    let scope =
        RequestScope::host(RequestId([7; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let server = async {
        use racer_io::ring::IoBuffer;
        let fd = reactor
            .accept(Rc::new(racer_io::ring::Descriptor::from(listener)), &scope)
            .await?;
        let conn = crate::http::from_accepted(fd, &admission)?;
        let mut conn = crate::peer::protocol::accept(&io, conn, signers[2].clone(), &scope).await?;
        for attempt in 0..3 {
            let mut received = io.receive_head(conn, &scope).await?;
            assert!(
                transport::detach(&mut received.value, &peer_wire::test_util::AnyChain)?.is_none(),
                "no native control without capability"
            );
            conn = received.connection;
            conn = io
                .send_head(
                    conn,
                    encode_envelope(&authentication, true, body.len())?,
                    &scope,
                )
                .await?
                .connection;
            let bytes = if attempt == 2 { &body[..5] } else { &body[..] };
            for chunk in bytes.chunks(257) {
                let mut buffer = io.buffer(chunk.len())?;
                buffer.bytes_mut()?.copy_from_slice(chunk);
                conn = io.write_body(conn, buffer, &scope).await?.lease;
            }
            if attempt != 2 {
                conn.finish_exchange()?;
            }
        }
        Ok::<_, Error>(())
    };
    let client = async {
        for attempt in 0..3 {
            let mut local = request(&env, &admission, attempt);
            let page = match &response {
                PeerResponse::Page { ciphertext, .. } => ciphertext.envelope().page.clone(),
                _ => unreachable!(),
            };
            let plan =
                rdma::select_hop(&route, &page, &NodeId(A.into()), &NodeId(C.into())).unwrap();
            assert_eq!(plan, TransportPlan::Rdma { rail: RailId(0) });
            local.operation = Operation::Page {
                page,
                mode: FetchMode::CopyOnly,
            };
            let scope = local.origin.scope().clone();
            let (signed, _) = Forwarding::new(signers[0].clone())
                .sign_request(local)
                .unwrap();
            // A proposed native route without native capability must complete the
            // real signed exchange over HTTP, not merely pass a selector test.
            let result = transfers
                .exchange_timed(
                    endpoint.clone(),
                    signed,
                    plan,
                    Some(membership.clone()),
                    None,
                    None,
                    None,
                    Rc::new(std::cell::Cell::new(false)),
                    None,
                    None,
                    &scope,
                )
                .await;
            if attempt == 2 {
                assert!(result.is_err());
            } else {
                let transport::RelayResponse::Complete(response) = result.unwrap() else {
                    panic!("requester must receive a complete HTTP body")
                };
                match response.response {
                    PeerResponse::Page { ciphertext, .. } => {
                        assert_eq!(ciphertext.bytes(), expected)
                    }
                    _ => panic!("wrong response"),
                }
            }
        }
        Ok::<_, Error>(())
    };
    {
        let mut operation = std::pin::pin!(async { futures::try_join!(client, server) });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = std::future::Future::poll(operation.as_mut(), &mut cx) {
                result.unwrap();
                break;
            }
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
}
#[test]
fn outbound_lease_routes_without_registry_and_rejects_non_neighbors() {
    use crate::topology::Member;
    use crate::topology::Membership;
    let env = racer_io::Env::host();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(7),
            (0..1500)
                .map(|index| Member {
                    node: NodeId(format!("node-{index:06}")),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + index),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let local = membership.members()[0].node.clone();
    let network = PeerNetwork::new(
        local.clone(),
        Arc::new(crate::control::cell::PublicationCell::new(
            crate::control::Snapshot::retention(2),
        )),
        racer_io::Clock::host(),
    )
    .unwrap();
    assert!(matches!(
        network.membership(membership.version),
        Err(Error::IncompatibleMembership)
    ));
    let neighbors = membership.neighbors(&local).unwrap();
    assert!(neighbors.len() < membership.members().len() - 1);
    assert!(neighbors.len() <= membership.inner.max_degree());
    for member in membership.members() {
        let endpoint = network.endpoint(&membership, &member.node);
        if neighbors.contains(&member.node) {
            assert_eq!(
                endpoint.unwrap(),
                crate::http::Endpoint::Peer(member.peer_endpoint.clone())
            );
        } else {
            assert!(matches!(endpoint, Err(Error::InvalidRequest)));
        }
    }
    let admission = flow_control::Quotas::new(crate::admission::AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let mut request = request(&env, &admission, 1);
    assert_eq!(
        super::check_membership(&request, &membership),
        Err(Error::IncompatibleMembership)
    );
    request.route.membership = membership.version;
    assert_eq!(super::check_membership(&request, &membership), Ok(()));
}

mod established_sessions {
    use super::*;
    use crate::http::Codec;
    use crate::http::ConnectionLease;
    use crate::http::HttpIo;
    use crate::peer::protocol as p;
    use crate::peer::protocol as connection;
    use crate::runtime::Reactor;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use std::cell::Cell;
    use std::future::Future;
    use std::os::unix::net::UnixStream;
    use std::task::Context;
    use std::task::Poll;

    struct CountedService(Rc<Cell<usize>>);
    impl server::LocalPageService for CountedService {
        fn serve_peer<'a>(
            &'a self,
            _: crate::peer::forwarding::VerifiedRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async {
                self.0.set(self.0.get() + 1);
                Ok(PeerResponse::Miss)
            })
        }
    }
    struct Fixture {
        _outbound: NoOutbound,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        io: Rc<HttpIo>,
        server: server::PeerServer,
        signers: Vec<Rc<Signatures>>,
        calls: Rc<Cell<usize>>,
        scope: RequestScope,
    }
    impl Fixture {
        fn new(env: &racer_io::Env) -> Self {
            Self::with_forwarding(env, |signers| Forwarding::new(signers[2].clone()))
        }
        fn with_forwarding(
            env: &racer_io::Env,
            forwarding: impl FnOnce(&[Rc<Signatures>]) -> Forwarding,
        ) -> Self {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), env));
            reactor.init().unwrap();
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            let signers = signers(env);
            let forwarding = Rc::new(forwarding(&signers));
            let calls = Rc::new(Cell::new(0));
            let service = Rc::new(CountedService(calls.clone()));
            let membership = Arc::new(
                Membership::validate(
                    MembershipVersion(1),
                    [A, C]
                        .iter()
                        .enumerate()
                        .map(|(i, n)| Member {
                            node: NodeId((*n).into()),
                            shares: std::num::NonZeroU32::new(1).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                            rails: vec![],
                            site: String::new(),
                        })
                        .collect(),
                )
                .unwrap(),
            );
            let network = Rc::new(
                PeerNetwork::new(
                    NodeId(C.into()),
                    crate::test_support::published_membership(membership),
                    racer_io::Clock::host(),
                )
                .unwrap(),
            );
            let outbound = NoOutbound::new(env, signers[2].clone(), network.clone());
            let relay = Rc::new(Relay::new(
                Rc::new(Paths::new(
                    Rc::new(LinkHealth::on(env.ctx.clock.clone())),
                    4,
                )),
                forwarding.clone(),
                outbound.requester.clone(),
                admission.clone(),
                network.clone(),
            ));
            let server = server::PeerServer::for_test(
                io.clone(),
                forwarding,
                admission.clone(),
                service,
                relay,
                Rc::new(codec(&admission)),
                signers[2].clone(),
            );
            Self {
                _outbound: outbound,
                admission,
                reactor,
                io,
                server,
                signers,
                calls,
                scope: RequestScope::host(
                    RequestId([7; 16]),
                    Instant::now() + Duration::from_secs(30),
                )
                .unwrap(),
            }
        }
        fn drive<T>(&self, work: impl Future<Output = T>) -> T {
            let mut work = std::pin::pin!(work);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            loop {
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    return result;
                }
                self.scope.check().unwrap();
                self.reactor.poll_budgeted(128).unwrap();
                self.reactor.wait(Duration::from_millis(1)).unwrap();
            }
        }
        fn sockets(&self) -> (ConnectionLease, ConnectionLease) {
            let (a, b) = UnixStream::pair().unwrap();
            (
                crate::http::from_accepted(a.into(), &self.admission).unwrap(),
                crate::http::from_accepted(b.into(), &self.admission).unwrap(),
            )
        }
        fn frame(&self, env: &racer_io::Env, large: bool) -> http1::MessageHead {
            let mut request = request(env, &self.admission, 1);
            if large {
                request.origin.metadata =
                    Some(crate::security::OpaqueMetadata::from_header(&vec![b'm'; 8192]).unwrap());
                request.origin.authorization.as_mut().unwrap().ciphertext = vec![5; 8208];
            }
            let (signed, _) = Forwarding::new(self.signers[0].clone())
                .sign_request(request)
                .unwrap();
            encode_envelope(&signed.authentication, false, 0).unwrap()
        }
        // First successful dispatch uses the production server's accept path.
        fn first(&self, env: &racer_io::Env) -> (ConnectionLease, ConnectionLease) {
            let (a, b) = self.sockets();
            self.drive(async {
                let client = async {
                    let a = connection::connect(
                        &self.io,
                        a,
                        self.signers[0].clone(),
                        self.signers[2].node(),
                        &self.scope,
                    )
                    .await?;
                    let response = self
                        .io
                        .exchange_head(a, self.frame(env, false), &self.scope)
                        .await?;
                    let mut a = response.connection;
                    a.finish_exchange()?;
                    Ok::<_, Error>(a)
                };
                futures::try_join!(client, self.server.serve_connection(b, &self.scope))
            })
            .unwrap()
        }
    }

    #[test]
    fn crypto_queue_full_answers_unsigned_overload_before_verification() {
        use std::num::NonZeroUsize;
        let env = racer_io::Env::host();
        let (crypto_io, _port) =
            crate::security::pair(crate::model::WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
        let client = Rc::new(crate::security::CryptoClient::new(crypto_io));
        let f = Fixture::with_forwarding(&env, |signers| {
            Forwarding::with_crypto(signers[2].clone(), client.clone())
        });
        // Fill the one port slot and the one admission waiter; nothing completes them.
        let forwarding = Forwarding::with_crypto(f.signers[2].clone(), client.clone());
        let signed = |n| {
            Forwarding::new(f.signers[0].clone())
                .sign_request(request(&env, &f.admission, n))
                .unwrap()
                .0
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut held = Vec::new();
        for n in 2..4 {
            let mut verify = Box::pin(forwarding.verify_request_async(signed(n), &f.scope));
            assert!(verify.as_mut().poll(&mut cx).is_pending());
            held.push(verify);
        }
        let (a, b) = f.sockets();
        let (response, served) = f.drive(async {
            let client = async {
                let a = connection::connect(
                    &f.io,
                    a,
                    f.signers[0].clone(),
                    f.signers[2].node(),
                    &f.scope,
                )
                .await?;
                f.io.exchange_head(a, f.frame(&env, false), &f.scope).await
            };
            futures::join!(client, f.server.serve_connection(b, &f.scope))
        });
        let response = response.unwrap();
        // The answer is the bare unsigned overload head: no envelope, no body.
        assert!(peer_wire::is_overloaded(&response.value));
        assert_eq!(response.connection.remaining_body(), Some(0));
        assert!(served.is_ok(), "{:?}", served.err());
        assert_eq!(f.calls.get(), 0, "an unverified request reached service");
        drop(held);
    }

    #[test]
    fn established_server_rejects_duplicate_wrong_direction_session_and_corruption_before_dispatch()
    {
        let env = racer_io::Env::host();
        for attack in ["duplicate", "direction", "session", "signature"] {
            let f = Fixture::new(&env);
            let baseline = f.admission.used(ResourceClass::RequestContext);
            let (mut a, b) = f.first(&env);
            assert_eq!(f.calls.get(), 1);
            let mut head = a
                .state_mut()
                .session
                .as_mut()
                .unwrap()
                .sign(f.frame(&env, false))
                .unwrap();
            // Re-sign incorrect bindings with the real peer key. Rejection must
            // test ordering/session/direction rather than signature corruption.
            if attack != "signature" {
                let (name, value) = match attack {
                    "duplicate" => ("racer-sequence", "1".to_owned()),
                    "direction" => ("racer-direction", "1".to_owned()),
                    _ => ("racer-session", p::binary(&[0; 32])),
                };
                head.headers
                    .iter_mut()
                    .find(|h| h.name == name)
                    .unwrap()
                    .value = value.into_bytes();
                head.headers.retain(|h| {
                    !matches!(
                        h.name.as_str(),
                        "signature"
                            | "signature-input"
                            | "racer-profile"
                            | "racer-cluster"
                            | "racer-signer"
                            | "racer-certificate"
                            | "racer-chains"
                            | "racer-timestamp"
                    )
                });
                head = f.signers[0].sign_fields(head).unwrap().head;
            } else {
                let signature = &mut head
                    .headers
                    .iter_mut()
                    .find(|h| h.name == "signature")
                    .unwrap()
                    .value;
                signature[8] = if signature[8] == b'A' { b'B' } else { b'A' };
            }
            let bytes = Codec::new(protocol::MAX_ENVELOPE_HEAD)
                .encode_head(&head)
                .unwrap();
            let mut buffer = f.io.buffer(bytes.len()).unwrap();
            buffer.bytes_mut().unwrap().copy_from_slice(&bytes);
            let (sent, received) = f.drive(async {
                futures::join!(
                    f.reactor.send(a.socket(), buffer, a, &f.scope),
                    f.server.serve_connection(b, &f.scope)
                )
            });
            assert!(sent.is_ok());
            assert!(
                matches!(received, Err(Error::Replay | Error::Unauthorized)),
                "{attack}"
            );
            assert_eq!(f.calls.get(), 1, "{attack} reached service");
            drop(sent);
            f.drive(f.reactor.drain()).unwrap();
            assert_eq!(f.admission.used(ResourceClass::Connection), 0);
            assert_eq!(
                f.admission.used(ResourceClass::RequestContext),
                baseline + f.io.retained_buffer_bytes()
            );
        }
    }

    #[test]
    fn partial_signed_head_cancel_closes_socket_and_fresh_handshake_dispatches() {
        use std::os::fd::AsRawFd;
        let env = racer_io::Env::host();
        let f = Fixture::new(&env);
        let baseline = f.admission.used(ResourceClass::RequestContext);
        let (a, b) = f.first(&env);
        let socket = a.socket();
        let size: libc::c_int = 1024;
        // SAFETY: setsockopt synchronously reads this correctly sized integer.
        assert_eq!(
            unsafe {
                libc::setsockopt(
                    socket.as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_SNDBUF,
                    (&size as *const libc::c_int).cast(),
                    std::mem::size_of_val(&size) as _,
                )
            },
            0
        );
        drop(socket);
        let scope = RequestScope::host(f.scope.request, f.scope.deadline.0).unwrap();
        let mut sending = f.io.send_head(a, f.frame(&env, true), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(sending.as_mut().poll(&mut cx).is_pending());
        let mut prefix = vec![0; 65536];
        let count = loop {
            f.reactor.poll_budgeted(128).unwrap();
            assert!(sending.as_mut().poll(&mut cx).is_pending());
            // SAFETY: recv writes within prefix; PEEK leaves partial bytes for
            // the production server to consume after cancellation closes sender.
            let n = unsafe {
                libc::recv(
                    b.socket().as_raw_fd(),
                    prefix.as_mut_ptr().cast(),
                    prefix.len(),
                    libc::MSG_PEEK | libc::MSG_DONTWAIT,
                )
            };
            if n > 0 {
                break n as usize;
            }
            f.scope.check().unwrap();
        };
        assert!(prefix[..count].starts_with(b"POST "));
        assert!(!prefix[..count].windows(4).any(|w| w == b"\r\n\r\n"));
        scope.cancel().unwrap();
        assert!(matches!(f.drive(sending), Err(Error::Canceled)));
        assert!(matches!(
            f.drive(f.server.serve_connection(b, &f.scope)),
            Err(Error::Io)
        ));
        assert_eq!(f.calls.get(), 1);
        assert_eq!(f.reactor.in_flight(), 0);
        assert_eq!(f.admission.used(ResourceClass::Connection), 0);
        assert_eq!(
            f.admission.used(ResourceClass::RequestContext),
            baseline + f.io.retained_buffer_bytes()
        );
        // A newly handshaken socket starts at sequence one and dispatches normally.
        drop(f.first(&env));
        assert_eq!(f.calls.get(), 2);
        assert_eq!(f.admission.used(ResourceClass::Connection), 0);
        assert_eq!(
            f.admission.used(ResourceClass::RequestContext),
            baseline + f.io.retained_buffer_bytes()
        );
    }
}

/// An owner already writing `hot_page_threshold()` bodies of a page,
/// at least half of all bodies it writes, answers a signed `Overloaded` to a
/// requester that is neither an owner nor a helper of the page, without
/// calling the page service; below either threshold, for a page it does not
/// own, or for a helper requester, it serves.
#[test]
fn hot_page_owner_sheds_non_helper_readers_with_signed_overload() {
    let env = racer_io::Env::host();
    hot_page_shedding(&env, false);
}

/// A hot owner sheds a requester in its own mesh column of the page's pool
/// only past the exempt cap: with more than 64 members the pool has several
/// columns, and the same hot page that sheds a requester in another column is
/// served to one in the owner's column until the cap.
#[test]
fn hot_page_owner_serves_its_column_mates() {
    let env = racer_io::Env::host();
    hot_page_shedding(&env, true);
}

/// Drive a hot owner C with requester A, where A is in C's mesh column of the
/// cache's pool exactly when `mate`.
fn hot_page_shedding(env: &racer_io::Env, mate: bool) {
    use crate::http::Codec;
    use crate::runtime::Reactor;
    use crate::topology::LinkHealth;
    use crate::topology::Member;
    use crate::topology::Membership;
    use crate::topology::Paths;
    use std::cell::Cell;
    use std::num::NonZeroU32;
    let hot = crate::peer::server::hot_page_threshold();
    type Counters =
        Rc<std::cell::RefCell<racer_core::read::serve::ServeDecision<crate::model::PageId>>>;
    /// Counts served requests and records the requested page's hot-page count while serving.
    struct Service(
        Rc<Cell<usize>>,
        Rc<std::cell::RefCell<Option<Counters>>>,
        Rc<Cell<usize>>,
    );
    impl server::LocalPageService for Service {
        fn serve_peer<'a>(
            &'a self,
            request: crate::peer::forwarding::VerifiedRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                self.0.set(self.0.get() + 1);
                if let (Some(counters), Operation::Page { page, .. }) =
                    (&*self.1.borrow(), &request.request().operation)
                {
                    self.2.set(counters.borrow().streaming(page).0);
                }
                Ok(PeerResponse::Miss)
            })
        }
    }
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    // 80 members, so the pool has several columns. The requester is the
    // first neighbor of C past A, B, and C that is in C's column exactly
    // when `mate` (column mates are never shed).
    let nodes: Vec<String> = [A, B, C]
        .iter()
        .map(|n| (*n).to_owned())
        .chain((4..80).map(|i| format!("{i:08}-1111-4111-8111-111111111111")))
        .collect();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            nodes
                .iter()
                .enumerate()
                .map(|(i, n)| Member {
                    node: NodeId(n.clone()),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    assert!(
        membership
            .cache_pool(&CacheId(CACHE.into()))
            .mesh
            .is_some_and(|pool| pool.columns() > 1)
    );
    let requester_name = nodes[3..]
        .iter()
        .find(|n| {
            let (owner, node) = (NodeId(C.into()), NodeId((*n).clone()));
            // A direct requester must be one of C's mesh neighbors.
            membership.is_neighbor(&owner, &node)
                && membership.same_column(&CacheId(CACHE.into()), &owner, &node) == mate
        })
        .expect("a requester in or outside C's column as asked")
        .as_str();
    let signers: Vec<_> = crate::test_support::security::identities(
        env,
        ClusterId(CLUSTER.into()),
        &[A, B, C, requester_name].map(|name| NodeId(name.into())),
        || crate::test_support::security::mac_test_key(CACHE),
    )
    .into_iter()
    .map(|identity| identity.signatures)
    .collect();
    let destination = Rc::new(Forwarding::new(signers[2].clone()));
    let sender = Forwarding::new(signers[3].clone());
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::test_support::published_membership(membership.clone()),
            racer_io::Clock::host(),
        )
        .unwrap(),
    );
    let paths = Rc::new(Paths::new(
        Rc::new(LinkHealth::on(env.ctx.clock.clone())),
        4,
    ));
    let outbound = NoOutbound::new(env, signers[2].clone(), network.clone());
    let relay = Rc::new(Relay::new(
        paths,
        destination.clone(),
        outbound.requester.clone(),
        admission.clone(),
        network.clone(),
    ));
    let io = Rc::new(crate::http::new_io(
        Rc::new(Reactor::new(admission.clone(), env)),
        Codec::new(65536),
        admission.clone(),
        16777232,
    ));
    let calls = Rc::new(Cell::new(0));
    let counters = Rc::new(std::cell::RefCell::new(None));
    let seen = Rc::new(Cell::new(0));
    let server = server::PeerServer::for_test(
        io,
        destination,
        admission.clone(),
        Rc::new(Service(calls.clone(), counters.clone(), seen.clone())),
        relay,
        Rc::new(codec(&admission)),
        signers[2].clone(),
    );
    let object = ObjectId {
        cache: CacheId(CACHE.into()),
        key: CacheKey([3; 32]),
    };
    let page_id = |number: u64| crate::model::PageId {
        version: crate::model::ObjectVersion {
            object: object.clone(),
            etag: crate::model::StrongEtag::test_value("v1"),
        },
        number: PageNumber(number),
    };
    // Pages C owns that A does not, and pages neither owns.
    let placement = crate::topology::Placement::new(64);
    let mut shed_page = None;
    let mut foreign_page = None;
    for number in 0..4096 {
        let owners = placement
            .rank(membership.clone(), &object, PageNumber(number))
            .ordered;
        let owns = |n: &str| owners.contains(&NodeId(n.into()));
        match (owns(C), owns(requester_name)) {
            (true, false) => shed_page = shed_page.or(Some(number)),
            (false, false) => foreign_page = foreign_page.or(Some(number)),
            _ => {}
        }
    }
    let (shed_page, foreign_page) = (shed_page.unwrap(), foreign_page.unwrap());
    let mut attempt = 0u8;
    let mut ask = |number: u64| {
        attempt += 1;
        let mut request = request(env, &admission, attempt);
        request.operation = Operation::Page {
            page: page_id(number),
            mode: FetchMode::CopyOnly,
        };
        request.route.visited = vec![NodeId(requester_name.into())];
        let scope = request.origin.scope().clone();
        let (signed, binding) = sender.sign_request(request).unwrap();
        let response = futures::executor::block_on(server.dispatch(signed, &scope)).unwrap();
        matches!(
            sender
                .verify_response(response, &binding)
                .unwrap()
                .response(),
            PeerResponse::Overloaded
        )
    };
    *counters.borrow_mut() = Some(server.serve_counters());
    // Below the threshold every page is served. The admitted request counts toward the page's
    // fan-in while it is served, so requests waiting on a cold page's fill are counted before
    // their bodies start.
    let held = server.hold_streaming(&page_id(shed_page), hot - 1);
    assert!(!ask(shed_page));
    assert_eq!(calls.get(), 1);
    assert_eq!(seen.get(), hot);
    assert_eq!(server.streaming(&page_id(shed_page)), (hot - 1, hot - 1));
    drop(held);
    assert_eq!(server.streaming(&page_id(shed_page)), (0, 0));
    // At the threshold a non-owner outside C's column is shed unserved, and
    // a column mate is served.
    let held = server.hold_streaming(&page_id(shed_page), hot);
    assert_eq!(server.streaming(&page_id(shed_page)), (hot, hot));
    assert_eq!(ask(shed_page), !mate);
    let served = if mate { 2 } else { 1 };
    assert_eq!(calls.get(), served);
    // Not while the page is less than half of what the owner is writing.
    let other = server.hold_streaming(&page_id(foreign_page), hot + 1);
    assert!(!ask(shed_page));
    assert_eq!(calls.get(), served + 1);
    drop((held, other));
    // A node that does not own the page (for example a helper) never sheds.
    let held = server.hold_streaming(&page_id(foreign_page), hot);
    assert!(!ask(foreign_page));
    assert_eq!(calls.get(), served + 2);
    drop(held);
    // Past the exempt cap a column mate is shed too, so the column cannot pile onto C.
    let cap = racer_core::read::budget::hot_page_exempt_bodies(hot);
    let held = server.hold_streaming(&page_id(shed_page), cap - 1);
    assert_eq!(ask(shed_page), !mate);
    let served = served + usize::from(mate) + 2;
    assert_eq!(calls.get(), served);
    drop(held);
    let _held = server.hold_streaming(&page_id(shed_page), cap);
    assert!(ask(shed_page));
    assert_eq!(calls.get(), served);
}
