//! Signed envelopes, routing contracts, and real socket/session exchanges.
use super::*;

#[test]
fn signed_opaque_relay_roundtrip_and_exact_attempt_binding() {
    let signers = signers();
    let auth = signers
        .iter()
        .map(|s| Forwarding::new(s.clone()))
        .collect::<Vec<_>>();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let codec = codec(&admission);
    let local = request(&admission, 7);
    let scope = local.origin.scope().clone();
    let (signed, outstanding) = auth[0].sign_request_to(local, signers[1].node()).unwrap();
    let signature = signed.authentication.original.signature.clone();
    let (envelope, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0).unwrap(),
        false,
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
    )
    .unwrap();
    let decoded = codec.response(envelope, vec![], &scope).unwrap();
    let (_, other) = auth[0]
        .sign_request_to(request(&admission, 8), signers[1].node())
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
    let signers = signers();
    let auth: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let codec = codec(&admission);
    for role in ["fresh", "copy", "pinned", "page", "relay"] {
        let mut local = request(&admission, 20);
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
            &crate::security::connection::signed_digest(&admitted.signed().authentication.original)
                .unwrap(),
            &path,
        )
        .unwrap();
        p::push(&mut head, "racer-receiver", &signers[signer - 1].node().0);
        let response = protocol::SignedResponse {
            authentication: crate::security::forwarding::ForwardedHead {
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
        let (envelope, length) = decode_envelope(envelope, true).unwrap();
        assert_eq!(length, 0);
        let decoded = codec.response(envelope, vec![], &scope).unwrap();
        let (_, other) = auth[0]
            .sign_request_to(request(&admission, 21), signers[1].node())
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
            let mut changed = crate::security::test_support::clone_head(&original);
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
            let envelope = crate::security::forwarding::ForwardedHead {
                original: Arc::new(changed),
                hops: vec![],
            };
            assert!(codec.response(envelope, vec![], &scope).is_err());
        }
        let mut changed = crate::security::test_support::clone_head(&original);
        // A structurally valid alternate outcome/status still needs a valid signature.
        changed.head.start = StartLine::Response { status: 200 };
        changed
            .head
            .headers
            .iter_mut()
            .find(|h| h.name == "racer-outcome")
            .unwrap()
            .value = b"miss".to_vec();
        let envelope = crate::security::forwarding::ForwardedHead {
            original: Arc::new(changed),
            hops: vec![],
        };
        let decoded = codec.response(envelope, vec![], &scope).unwrap();
        assert!(auth[1].verify_response(decoded, &reverse).is_err());
        let envelope = crate::security::forwarding::ForwardedHead {
            original,
            hops: vec![],
        };
        assert!(codec.response(envelope, vec![1], &scope).is_err());
    }
}

#[test]
fn changed_operation_credentials_replay_and_deadlines_fail() {
    let signers = signers();
    let sender = Forwarding::new(signers[0].clone());
    let receiver = Forwarding::new(signers[2].clone());
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let codec = codec(&admission);
    let local = request(&admission, 1);
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
    let (signed, _) = sender.sign_request(request(&admission, 2)).unwrap();
    let (envelope, _) = decode_envelope(
        encode_envelope(&signed.authentication, false, 0).unwrap(),
        false,
    )
    .unwrap();
    receiver.verify_request(signed).unwrap();
    receiver
        .verify_request(codec.request(envelope, &scope).unwrap())
        .unwrap();
    crate::security::connection::tests::replay_and_binding_checks();
    let mut expired = request(&admission, 3);
    expired.route.deadline = Deadline(Instant::now() - Duration::from_secs(1));
    assert!(matches!(
        request_scope(&expired, &scope),
        Err(Error::DeadlineExceeded)
    ));
    let local = request(&admission, 4);
    let narrowed = request_scope(&local, &scope).unwrap();
    scope.cancel().unwrap();
    assert_eq!(narrowed.check(), Err(Error::Cancelled));
}

#[test]
fn search_view_consumes_ingress_without_changing_signed_route() {
    let admission = flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let request = request(&admission, 1);
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
    use crate::{
        http::{Codec, HttpIo},
        runtime::reactor::Reactor,
        topology::{
            health::LinkHealth,
            membership::{Member, Membership},
            routing::Paths,
        },
    };
    use std::{cell::Cell, num::NonZeroU32};
    struct NeverTransport;
    impl PeerTransport for NeverTransport {
        fn exchange<'a>(
            &'a self,
            _: protocol::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, protocol::SignedResponse> {
            Box::pin(async { panic!("local service must not relay") })
        }
    }
    struct Service(Rc<Cell<usize>>);
    impl server::LocalPageService for Service {
        fn serve_peer<'a>(
            &'a self,
            request: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
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
    let signers = signers();
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
            crate::control::state::PublishedState::for_membership(membership),
        )
        .unwrap(),
    );
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
    let relay = Rc::new(Relay::new(
        paths,
        destination.clone(),
        Rc::new(NeverTransport),
        admission.clone(),
        network.clone(),
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
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
    let local = request(&admission, 1);
    let scope = local.origin.scope().clone();
    let (mut forged, _) = sender.sign_request(local).unwrap();
    Arc::get_mut(&mut forged.authentication.original).map(|head| head.signature[0] ^= 1);
    // The binding also retains the original Arc, so mutate the logical fields.
    forged.request.route.destination = NodeId(B.into());
    assert!(futures::executor::block_on(server.dispatch(forged, &scope)).is_err());
    assert_eq!(calls.get(), 0);
    let (valid, binding) = sender.sign_request(request(&admission, 2)).unwrap();
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
fn handshake_capabilities_are_signed_and_bound_to_request_and_membership() {
    use crate::{
        peer::protocol as p,
        security::connection::signed_digest,
        topology::membership::{Member, Membership},
    };
    use http1::{MessageHead, StartLine};
    let signers = signers();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            [A, C]
                .iter()
                .enumerate()
                .map(|(index, name)| Member {
                    node: NodeId((*name).into()),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 9000 + index),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
        )
        .unwrap(),
    );
    let weak = Arc::downgrade(&membership);
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::state::PublishedState::for_membership(membership),
        )
        .unwrap(),
    );
    let response_lease = network.membership(MembershipVersion(1)).unwrap();
    drop(network);
    let mut head = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1/handshake".into(),
        },
        headers: vec![],
    };
    p::push(&mut head, "content-length", 0);
    p::push(&mut head, "racer-kind", "handshake");
    p::push(&mut head, "racer-wire-version", protocol::VERSION);
    p::push(&mut head, "racer-membership", 1);
    p::push(&mut head, "racer-receiver", C);
    let signed = signers[0].sign(head).unwrap();
    let binding = signed_digest(&signed).unwrap();
    // Capabilities are no longer discovered per request. Membership and native
    // availability remain signed application/control data, inside a session head.
    let mut reply = MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![],
    };
    p::push(&mut reply, "content-length", 0);
    p::push(&mut reply, "racer-receiver", A);
    p::push(&mut reply, "racer-membership", 1);
    p::push(&mut reply, "racer-rdma", 0);
    p::push_binary(&mut reply, "racer-request-binding", &binding);
    let reply = signers[2].sign(reply).unwrap();
    assert!(
        weak.upgrade().is_some(),
        "pending reply retains ingress lease"
    );
    assert_eq!(
        p::decode_binary(
            p::field(&reply.head, "racer-request-binding")
                .unwrap()
                .as_bytes()
        )
        .unwrap(),
        binding
    );
    assert_eq!(p::number(&reply.head, "racer-membership").unwrap(), 1);
    assert_eq!(p::number(&reply.head, "racer-rdma").unwrap(), 0);
    let mut tampered = reply;
    tampered
        .head
        .headers
        .iter_mut()
        .find(|h| h.name == "racer-rdma")
        .unwrap()
        .value = b"1".to_vec();
    assert!(signers[0].verify_proof(tampered).is_err());
    crate::security::connection::tests::replay_and_binding_checks();
    drop(response_lease);
    assert!(
        weak.upgrade().is_none(),
        "completed reply releases ingress lease"
    );
}

#[test]
fn relay_dispatch_preserves_reverse_path_and_fails_closed_on_link_loss() {
    use crate::topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        routing::Paths,
    };
    struct Destination {
        auth: Forwarding,
        fail: bool,
    }
    impl PeerTransport for Destination {
        fn exchange<'a>(
            &'a self,
            request: protocol::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            scope: &'a RequestScope,
        ) -> crate::error::Operation<'a, protocol::SignedResponse> {
            Box::pin(async move {
                scope.check()?;
                if self.fail {
                    return Err(Error::Io);
                }
                let admitted = self.auth.verify_request(request)?;
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
                self.auth
                    .sign_response(admitted.binding(), PeerResponse::Miss)
            })
        }
    }
    for fail in [false, true] {
        let signers = signers();
        let origin = Forwarding::new(signers[0].clone());
        let forwarding = Rc::new(Forwarding::new(signers[1].clone()));
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
                        shares: std::num::NonZeroU32::new(1).unwrap(),
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
                NodeId(B.into()),
                crate::control::state::PublishedState::for_membership(membership.clone()),
            )
            .unwrap(),
        );
        let relay = Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 1)),
            forwarding.clone(),
            Rc::new(Destination {
                auth: Forwarding::new(signers[2].clone()),
                fail,
            }),
            admission.clone(),
            network,
        );
        let local = request(&admission, 9);
        let scope = local.origin.scope().clone();
        let (signed, binding) = origin.sign_request_to(local, signers[1].node()).unwrap();
        let ingress = forwarding.verify_request(signed).unwrap();
        let result = futures::executor::block_on(relay.forward(ingress, membership, &scope));
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
    }
}

#[test]
fn v5_equal_cost_signed_receiver_survives_wire_recompute_and_cache_eviction() {
    use crate::topology::{
        health::{LinkHealth, LinkOutcome},
        membership::{Member, Membership},
        routing::Paths,
    };
    let signers = signers();
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
    let paths = Paths::new(Rc::new(LinkHealth), 1);
    let health = Rc::new(LinkHealth);
    let cold = Paths::new(health.clone(), 0);
    let mut selected = std::collections::BTreeSet::new();
    for attempt in 1..=32 {
        let original = request(&admission, attempt);
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
            crate::security::connection::receiver(&signed.authentication.original.head).unwrap();
        let (envelope, _) = decode_envelope(
            encode_envelope(&signed.authentication, false, 0).unwrap(),
            false,
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
            .shortest(members.clone(), &NodeId(A.into()), &search)
            .unwrap();
        assert_ne!(changed.nodes[1], receiver);
        assert_eq!(changed.nodes.len(), planned.nodes.len());
        health.observe(&receiver, LinkOutcome::Success).unwrap();
        // Force eviction before repeating the exact signed state.
        let mut other = budget.clone();
        other.destination = members.members()[1498].node.clone();
        paths
            .shortest(members.clone(), &NodeId(A.into()), &other)
            .unwrap();
        assert_eq!(
            paths
                .shortest(members.clone(), &NodeId(A.into()), &search)
                .unwrap()
                .nodes,
            planned.nodes
        );
    }
    assert!(selected.len() > 1);
}

#[test]
fn refused_socket_opens_only_immediate_link_and_selects_bounded_alternate() {
    use super::PeerClient;
    use crate::{
        http::{Codec, HttpIo, HttpPool},
        runtime::reactor::Reactor,
        topology::{
            health::LinkHealth,
            membership::{Member, Membership},
            routing::Paths,
        },
    };
    use std::task::{Context, Poll};
    let (signers, _) = identities();
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(protocol::MAX_ENVELOPE_HEAD),
        admission.clone(),
        0,
    ));
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2));
    let transfers = Rc::new(transport::Transfers::new(
        pool,
        io,
        None,
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
    let health = Rc::new(LinkHealth::new(36));
    let paths = Rc::new(Paths::new(health.clone(), 4));
    let requester = Requester::new(
        paths.clone(),
        Rc::new(Forwarding::new(signers[0].clone())),
        transfers,
        Rc::new(
            PeerNetwork::new(
                NodeId(A.into()),
                crate::control::state::PublishedState::for_membership(members.clone()),
            )
            .unwrap(),
        ),
    );
    let original = request(&admission, 1);
    let budget = super::search_budget(&original.route, &NodeId(A.into())).unwrap();
    let scope = original.origin.scope().clone();
    let direct = paths
        .shortest(members.clone(), &NodeId(A.into()), &budget)
        .unwrap();
    assert_eq!(direct.nodes, vec![NodeId(A.into()), NodeId(C.into())]);
    // Reject a signed receiver that differs from v3 recomputation before I/O.
    let mut page_request = request(&admission, 2);
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
        futures::executor::block_on(PeerTransport::exchange(
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
    assert!(matches!(result, Err(Error::Io | Error::Unavailable)));
    assert!(!health.available(&NodeId(C.into())).unwrap());
    assert!(health.available(&NodeId(B.into())).unwrap());
    let alternate = paths
        .shortest(members.clone(), &NodeId(A.into()), &budget)
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
            .shortest(members, &NodeId(A.into()), &exhausted)
            .is_err()
    );
    drop(attempt);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}

#[test]
fn requester_and_server_negotiate_and_exchange_over_real_tcp() {
    for case in [
        "success",
        "scope",
        "attempt",
        "membership",
        "cancel",
        "deadline",
    ] {
        signed_tcp_case(case);
    }
}

fn signed_tcp_case(case: &str) {
    use super::PeerClient;
    use crate::topology::{
        health::LinkHealth,
        membership::{Member, Membership},
        routing::Paths,
    };
    use std::{
        net::TcpListener,
        task::{Context, Poll},
    };
    struct Local;
    impl server::LocalPageService for Local {
        fn serve_peer<'a>(
            &'a self,
            request: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
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
    let (signers, _) = identities();
    let fixture = SocketFixture::new(2);
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
            crate::control::state::PublishedState::for_membership(membership.clone()),
        )
        .unwrap(),
    );
    let destination_network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::state::PublishedState::for_membership(membership.clone()),
        )
        .unwrap(),
    );
    let destination_auth = Rc::new(Forwarding::new(signers[2].clone()));
    let metrics = crate::telemetry::metrics::Metrics::default();
    let adaptive =
        crate::peer::adaptive::AdaptivePeers::new(Default::default(), metrics.clone()).unwrap();
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4).with_peer_admission(adaptive));
    let relay = Rc::new(Relay::new(
        paths.clone(),
        destination_auth.clone(),
        Rc::new(NeverTransport),
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
    let mut local = request(&admission, 1);
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
            Some(Error::Cancelled)
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
    let listener_scope = RequestScope::new(
        RequestId([0; 16]),
        scope.deadline.0 + Duration::from_secs(60),
    )
    .unwrap();
    let server_work = async {
        let fd = reactor
            .accept(
                Rc::new(uring_runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
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
        metrics.count(crate::telemetry::metrics::Event::PeerAdmissionAccepted),
        1
    );
    assert_eq!(
        metrics.count(crate::telemetry::metrics::Event::PeerVerified),
        1
    );
    assert_eq!(
        metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
        0
    );
    assert_eq!(
        health.tracked_links(),
        0,
        "application miss is not a broken link"
    );
}

#[test]
fn incoming_header_timeout_closes_silent_partial_and_idle_keepalive_peers() {
    use crate::{
        http::{Codec, HttpIo},
        runtime::reactor::Reactor,
        topology::{health::LinkHealth, routing::Paths},
    };
    use std::{
        future::Future,
        io::{Read, Write},
        os::unix::net::UnixStream,
        task::{Context, Poll},
    };

    struct Never;
    impl PeerTransport for Never {
        fn exchange<'a>(
            &'a self,
            _: protocol::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, protocol::SignedResponse> {
            Box::pin(async { panic!("incomplete headers must not relay") })
        }
    }
    impl server::LocalPageService for Never {
        fn serve_peer<'a>(
            &'a self,
            _: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
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
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(protocol::MAX_ENVELOPE_HEAD),
            admission.clone(),
            crate::model::PAGE_BYTES + 16,
        ));
        let (signers, _) = identities();
        let forwarding = Rc::new(Forwarding::new(signers[2].clone()));
        let relay = Rc::new(Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
            forwarding.clone(),
            Rc::new(Never),
            admission.clone(),
            Rc::new(
                PeerNetwork::new(signers[2].node().clone(), Arc::new(Default::default())).unwrap(),
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
        let scope = RequestScope::new(
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
                    crate::security::connection::connect(
                        &io,
                        client,
                        signers[0].clone(),
                        signers[2].node(),
                        &scope
                    ),
                    crate::security::connection::accept(
                        &io,
                        connection,
                        signers[2].clone(),
                        &scope
                    )
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
                Error::Cancelled
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
    use crate::{
        http::{Codec, Endpoint, HttpIo, HttpPool},
        model::{ExpiresAt, ObjectMetadata, PageEnvelope},
        peer::protocol,
        runtime::reactor::Reactor,
        security::forwarding::ForwardedHead,
        topology::{
            membership::{Member, Membership},
            rails::{self, RailId, RailMapping, TransportPlan},
            routing::Route,
        },
    };
    use std::{
        net::TcpListener,
        task::{Context, Poll},
    };
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(64 * 1024),
        admission.clone(),
        crate::model::PAGE_BYTES + 16,
    ));
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 1));
    let signers = signers();
    let transfers = transport::Transfers::new(
        pool,
        io.clone(),
        None,
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
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let server = async {
        use uring_runtime::reactor::IoBuffer;
        let fd = reactor
            .accept(
                Rc::new(uring_runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        let conn = crate::http::from_accepted(fd, &admission)?;
        let mut conn =
            crate::security::connection::accept(&io, conn, signers[2].clone(), &scope).await?;
        for attempt in 0..3 {
            let mut received = io.receive_head(conn, &scope).await?;
            assert!(
                transport::detach(&mut received.value)?.is_none(),
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
            let mut local = request(&admission, attempt);
            let page = match &response {
                PeerResponse::Page { ciphertext, .. } => ciphertext.envelope().page.clone(),
                _ => unreachable!(),
            };
            let plan =
                rails::select_hop(&route, &page, &NodeId(A.into()), &NodeId(C.into())).unwrap();
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
                    Rc::new(std::cell::Cell::new(false)),
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
    use crate::topology::membership::{Member, Membership};
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
        Arc::new(crate::control::state::PublishedState::default()),
    )
    .unwrap();
    assert!(matches!(
        network.membership(membership.version),
        Err(Error::IncompatibleMembership)
    ));
    let neighbors = membership.neighbors(&local).unwrap();
    assert!(neighbors.len() < membership.members().len() - 1);
    assert_eq!(neighbors.len(), 62);
    {
        let radix = crate::topology::RADIX;
        for (index, member) in membership.members().iter().enumerate() {
            let expected = index != 0
                && (0..radix)
                    .any(|digit| digit % 1500 == index || (radix * index + digit) % 1500 == 0);
            assert_eq!(
                network.endpoint(&membership, &member.node).is_ok(),
                expected,
                "{index}"
            );
        }
    }
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
    let admission = flow_control::Quotas::new(crate::runtime::admission::AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let mut request = request(&admission, 1);
    assert_eq!(
        super::check_membership(&request, &membership),
        Err(Error::IncompatibleMembership)
    );
    request.route.membership = membership.version;
    assert_eq!(super::check_membership(&request, &membership), Ok(()));
}

mod established_sessions {
    use super::*;
    use crate::{
        http::{Codec, ConnectionLease, HttpIo},
        peer::protocol as p,
        runtime::reactor::Reactor,
        security::connection,
        topology::{
            health::LinkHealth,
            membership::{Member, Membership},
            routing::Paths,
        },
    };
    use std::{
        cell::Cell,
        future::Future,
        os::unix::net::UnixStream,
        task::{Context, Poll},
    };

    struct CountedService(Rc<Cell<usize>>);
    impl server::LocalPageService for CountedService {
        fn serve_peer<'a>(
            &'a self,
            _: protocol::VerifiedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async {
                self.0.set(self.0.get() + 1);
                Ok(PeerResponse::Miss)
            })
        }
    }
    impl PeerTransport for CountedService {
        fn exchange<'a>(
            &'a self,
            _: protocol::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, protocol::SignedResponse> {
            Box::pin(async { panic!("direct request must not relay") })
        }
    }
    struct Fixture {
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        io: Rc<HttpIo>,
        server: server::PeerServer,
        signers: Vec<Rc<Signatures>>,
        calls: Rc<Cell<usize>>,
        scope: RequestScope,
    }
    impl Fixture {
        fn new() -> Self {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            reactor.init().unwrap();
            let io = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            let signers = signers();
            let forwarding = Rc::new(Forwarding::new(signers[2].clone()));
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
                    crate::control::state::PublishedState::for_membership(membership),
                )
                .unwrap(),
            );
            let relay = Rc::new(Relay::new(
                Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
                forwarding.clone(),
                service.clone(),
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
                admission,
                reactor,
                io,
                server,
                signers,
                calls,
                scope: RequestScope::new(
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
        fn frame(&self, large: bool) -> http1::MessageHead {
            let mut request = request(&self.admission, 1);
            if large {
                request.origin.metadata =
                    Some(crate::model::OpaqueMetadata::from_header(&vec![b'm'; 8192]).unwrap());
                request.origin.authorization.as_mut().unwrap().ciphertext = vec![5; 8208];
            }
            let (signed, _) = Forwarding::new(self.signers[0].clone())
                .sign_request(request)
                .unwrap();
            encode_envelope(&signed.authentication, false, 0).unwrap()
        }
        // First successful dispatch uses the production server's accept path.
        fn first(&self) -> (ConnectionLease, ConnectionLease) {
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
                        .exchange_head(a, self.frame(false), &self.scope)
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
    fn established_server_rejects_duplicate_wrong_direction_session_and_corruption_before_dispatch()
    {
        for attack in ["duplicate", "direction", "session", "signature"] {
            let f = Fixture::new();
            let baseline = f.admission.used(ResourceClass::RequestContext);
            let (mut a, b) = f.first();
            assert_eq!(f.calls.get(), 1);
            let mut head = a
                .state_mut()
                .session
                .as_mut()
                .unwrap()
                .sign(f.frame(false))
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
                            | "racer-certificates"
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
        let f = Fixture::new();
        let baseline = f.admission.used(ResourceClass::RequestContext);
        let (a, b) = f.first();
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
        let scope = RequestScope::new(f.scope.request, f.scope.deadline.0).unwrap();
        let mut sending = f.io.send_head(a, f.frame(true), &scope);
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
        assert!(matches!(f.drive(sending), Err(Error::Cancelled)));
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
        drop(f.first());
        assert_eq!(f.calls.get(), 2);
        assert_eq!(f.admission.used(ResourceClass::Connection), 0);
        assert_eq!(
            f.admission.used(ResourceClass::RequestContext),
            baseline + f.io.retained_buffer_bytes()
        );
    }
}
use uring_runtime::reactor::IoBuffer;
