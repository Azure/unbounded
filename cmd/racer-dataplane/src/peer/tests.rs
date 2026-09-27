use super::*;
use crate::{
    control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION},
    memory::pool::BufferPool,
    model::{
        context::{EncryptedAuthorization, PeerOriginContext},
        envelope::{KeyId, Nonce},
        identity::*,
        limits::ResourceClass,
        metadata::MetadataSelector,
    },
    peer::wire::{
        FetchMode, LogicalCodec, Operation, PeerRequest, PeerResponse, SecurityCodec, WireCodec,
    },
    runtime::{
        admission::Admission,
        deadline::{Deadline, RequestScope},
    },
    security::{
        certificates::Certificates,
        forwarding::Forwarding,
        identity::PendingIdentity,
        keyring::{KeyEpochs, Keyring},
        signing::Signatures,
    },
    topology::paths::RouteBudget,
};
use std::{
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

const A: &str = "00000001-1111-4111-8111-111111111111";
const B: &str = "00000002-1111-4111-8111-111111111111";
const C: &str = "00000003-1111-4111-8111-111111111111";
const CACHE: &str = "cccccccc-1111-4111-8111-111111111111";
const CLUSTER: &str = "dddddddd-1111-4111-8111-111111111111";
type Discovery = (Rc<Keyring>, Rc<Certificates>);
fn identities() -> (Vec<Rc<Signatures>>, Vec<Discovery>) {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let roots = vec![ca.der().to_vec()];
    let mut signers = Vec::new();
    let mut discovery = Vec::new();
    for name in [A, B, C] {
        let pending = PendingIdentity::generate().unwrap();
        let bytes = pending.export_pkcs8_for_persistence().unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(bytes.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{CLUSTER}/node/{name}")
                .try_into()
                .unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
        let cluster = ClusterId(CLUSTER.into());
        let node = NodeId(name.into());
        let identity = pending
            .accept(
                cluster.clone(),
                node.clone(),
                vec![cert.der().to_vec()],
                &roots,
            )
            .unwrap();
        let keys = Rc::new(Keyring::new(
            cluster.clone(),
            node,
            Arc::new(KeyEpochs::default()),
        ));
        keys.install(KeyringBundle {
            schema_version: SCHEMA_VERSION,
            cluster: cluster.clone(),
            generation: BundleGeneration(1),
            peer_trust_roots: roots.clone(),
            cache_keys: vec![],
        })
        .unwrap();
        keys.install_signing_identity(Arc::new(identity)).unwrap();
        let certificates = Rc::new(Certificates::new(cluster, keys.clone()));
        discovery.push((keys.clone(), certificates.clone()));
        signers.push(Rc::new(Signatures::new(keys, certificates)));
    }
    (signers, discovery)
}
pub(super) fn signers() -> Vec<Rc<Signatures>> {
    let (signers, _) = identities();
    signers
}
fn request(admission: &Admission, attempt: u8) -> PeerRequest {
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let object = ObjectId {
        cache: CacheId(CACHE.into()),
        key: CacheKey([3; 32]),
    };
    let route = RouteBudget {
        membership: MembershipVersion(1),
        request: scope.request,
        attempt: AttemptId([attempt; 16]),
        destination: NodeId(C.into()),
        visited: vec![NodeId(A.into())],
        remaining_links: 4,
        remaining_attempts: 0,
        deadline: scope.deadline,
    };
    let origin = PeerOriginContext {
        object: object.clone(),
        request: scope.request,
        attempt: route.attempt,
        metadata: None,
        authorization: Some(EncryptedAuthorization {
            key_id: KeyId([2; 16]),
            nonce: Nonce([4; 24]),
            ciphertext: vec![5; 32],
        }),
        reservation: admission
            .reserve(None, ResourceClass::RequestContext, 4096)
            .unwrap(),
        scope,
    };
    PeerRequest {
        operation: Operation::Metadata {
            object,
            selector: MetadataSelector::Fresh,
            mode: FetchMode::CopyOnly,
        },
        origin,
        route,
    }
}
fn codec(admission: &Rc<Admission>) -> SecurityCodec {
    SecurityCodec::new(
        admission.clone(),
        Rc::new(BufferPool::new(admission.clone())),
    )
}

#[test]
fn signed_opaque_relay_roundtrip_and_exact_attempt_binding() {
    let signers = signers();
    let auth = signers
        .iter()
        .map(|s| Forwarding::new(s.clone()))
        .collect::<Vec<_>>();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let codec = codec(&admission);
    let local = request(&admission, 7);
    let scope = local.origin.scope().clone();
    let (signed, outstanding) = auth[0].sign_request_to(local, signers[1].node()).unwrap();
    let signature = signed.authentication.original.signature.clone();
    let (envelope, _) = WireCodec::decode(
        WireCodec::encode(&signed.authentication, false, 0).unwrap(),
        false,
    )
    .unwrap();
    let verified = auth[1]
        .verify_request(codec.request(envelope, &scope).unwrap())
        .unwrap();
    assert_eq!(
        verified.request().origin.reservation.cache(),
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
    let (envelope, _) = WireCodec::decode(
        WireCodec::encode(&forwarded.authentication, false, 0).unwrap(),
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
    let (envelope, _) = WireCodec::decode(
        WireCodec::encode(&reply.authentication, true, 0).unwrap(),
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
    use crate::{http::codec::StartLine, security::protocol as p};
    let signers = signers();
    let auth: Vec<_> = signers.iter().map(|s| Forwarding::new(s.clone())).collect();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
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
            &crate::security::signing::signed_digest(&admitted.signed().authentication.original)
                .unwrap(),
            &path,
        )
        .unwrap();
        p::push(&mut head, "racer-receiver", &signers[signer - 1].node().0);
        let response = wire::SignedResponse {
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
        let envelope = WireCodec::encode(&reverse_response.authentication, true, 0).unwrap();
        assert!(matches!(
            envelope.start,
            StartLine::Response { status: 200 }
        ));
        let (envelope, length) = WireCodec::decode(envelope, true).unwrap();
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
            let mut changed = crate::security::signing::tests::clone_head(&original);
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
        let mut changed = crate::security::signing::tests::clone_head(&original);
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
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
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
    let (envelope, _) = WireCodec::decode(
        WireCodec::encode(&signed.authentication, false, 0).unwrap(),
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
    let admission = Admission::new(crate::test_support::cluster::config(false).limits);
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
        http::{codec::Codec, io::HttpIo},
        runtime::reactor::Reactor,
        topology::{
            health::LinkHealth,
            membership::{Member, Membership},
            paths::Paths,
        },
    };
    use std::{cell::Cell, num::NonZeroU32};
    struct NeverTransport;
    impl requester::PeerTransport for NeverTransport {
        fn exchange<'a>(
            &'a self,
            _: wire::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, wire::SignedResponse> {
            Box::pin(async { panic!("local service must not relay") })
        }
    }
    struct Service(Rc<Cell<usize>>);
    impl server::LocalPageService for Service {
        fn serve_peer<'a>(
            &'a self,
            request: wire::VerifiedRequest,
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
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
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
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::snapshot::PublishedState::for_membership(membership),
        )
        .unwrap(),
    );
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
    let relay = Rc::new(
        relay::Relay::new(
            paths,
            destination.clone(),
            Rc::new(NeverTransport),
            admission.clone(),
        )
        .with_network(network.clone()),
    );
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor,
        Codec::new(65536, 16777232),
        admission.clone(),
    ));
    let calls = Rc::new(Cell::new(0));
    let server = server::PeerServer::new(
        io,
        destination,
        admission.clone(),
        Rc::new(Service(calls.clone())),
        relay,
    )
    .with_network(network);
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
        http::{
            codec::{Codec, MessageHead, StartLine},
            io::HttpIo,
            pool::HttpPool,
        },
        runtime::reactor::Reactor,
        security::{protocol as p, signing::signed_digest},
        topology::membership::{Member, Membership},
    };
    let signers = signers();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(65536, 16777232),
        admission.clone(),
    ));
    let transfers = Rc::new(transfer::Transfers::new(
        Rc::new(HttpPool::new(reactor, admission, 2)),
        io,
        None,
    ));
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
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let weak = Arc::downgrade(&membership);
    let network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::snapshot::PublishedState::for_membership(membership),
        )
        .unwrap(),
    );
    let handshake = handshake::Handshake::new(signers[2].clone(), None);
    drop(transfers);
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
    p::push(&mut head, "racer-wire-version", wire::VERSION);
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
    drop(handshake);
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
        paths::Paths,
    };
    struct Destination {
        auth: Forwarding,
        fail: bool,
    }
    impl requester::PeerTransport for Destination {
        fn exchange<'a>(
            &'a self,
            request: wire::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            scope: &'a RequestScope,
        ) -> crate::error::Operation<'a, wire::SignedResponse> {
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
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
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
                        alignment_enabled: false,
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let network = Rc::new(
            PeerNetwork::new(
                NodeId(B.into()),
                crate::control::snapshot::PublishedState::for_membership(membership.clone()),
            )
            .unwrap(),
        );
        let relay = relay::Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 1)),
            forwarding.clone(),
            Rc::new(Destination {
                auth: Forwarding::new(signers[2].clone()),
                fail,
            }),
            admission.clone(),
        )
        .with_network(network);
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
fn requester_and_server_negotiate_and_exchange_over_real_tcp() {
    use super::requester::PeerClient;
    use crate::{
        http::{
            codec::Codec,
            io::HttpIo,
            pool::{ConnectionLease, HttpPool},
        },
        runtime::reactor::Reactor,
        topology::{
            health::LinkHealth,
            membership::{Member, Membership},
            paths::Paths,
            rails::Rails,
        },
    };
    use std::{
        net::TcpListener,
        task::{Context, Poll},
    };
    struct NeverTransport;
    impl requester::PeerTransport for NeverTransport {
        fn exchange<'a>(
            &'a self,
            _: wire::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, wire::SignedResponse> {
            Box::pin(async { panic!("direct request must not relay") })
        }
    }
    struct Local;
    impl server::LocalPageService for Local {
        fn serve_peer<'a>(
            &'a self,
            request: wire::VerifiedRequest,
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
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(
            wire::MAX_ENVELOPE_HEAD,
            crate::model::range::PAGE_BYTES + 16,
        ),
        admission.clone(),
    ));
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 2));
    let codec = Rc::new(codec(&admission));
    let transfers = Rc::new(
        transfer::Transfers::new(pool, io.clone(), None)
            .with_wire(admission.clone(), codec.clone()),
    );
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let address = listener.local_addr().unwrap();
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
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let source_network = Rc::new(
        PeerNetwork::new(
            NodeId(A.into()),
            crate::control::snapshot::PublishedState::for_membership(membership.clone()),
        )
        .unwrap(),
    );
    let destination_network = Rc::new(
        PeerNetwork::new(
            NodeId(C.into()),
            crate::control::snapshot::PublishedState::for_membership(membership.clone()),
        )
        .unwrap(),
    );
    let source_handshake = Rc::new(handshake::Handshake::new(signers[0].clone(), None));
    let destination_handshake = Rc::new(handshake::Handshake::new(signers[2].clone(), None));
    let destination_auth = Rc::new(Forwarding::new(signers[2].clone()));
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4));
    let relay = Rc::new(
        relay::Relay::new(
            paths.clone(),
            destination_auth.clone(),
            Rc::new(NeverTransport),
            admission.clone(),
        )
        .with_network(destination_network.clone()),
    );
    let server = server::PeerServer::new(
        io,
        destination_auth,
        admission.clone(),
        Rc::new(Local),
        relay,
    )
    .with_request_timeout(Duration::from_secs(5))
    .with_network(destination_network)
    .with_wire(codec)
    .with_handshake(destination_handshake);
    let requester = requester::Requester::new(
        paths,
        Rc::new(Rails),
        Rc::new(Forwarding::new(signers[0].clone())),
        source_handshake,
        transfers,
    )
    .with_network(source_network);
    let local = request(&admission, 1);
    let scope = local.origin.scope().clone();
    let listener_scope = RequestScope::new(
        RequestId([0; 16]),
        scope.deadline.0 + Duration::from_secs(60),
    )
    .unwrap();
    let server_work = async {
        let fd = reactor
            .accept(
                Rc::new(crate::runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        let connection = ConnectionLease::from_accepted(fd, &admission)?;
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
}

#[test]
fn incoming_header_timeout_closes_silent_partial_and_idle_keepalive_peers() {
    use crate::{
        http::{codec::Codec, io::HttpIo, pool::ConnectionLease},
        runtime::reactor::Reactor,
        topology::{health::LinkHealth, paths::Paths},
    };
    use std::{
        future::Future,
        io::{Read, Write},
        os::unix::net::UnixStream,
        task::{Context, Poll},
    };

    struct Never;
    impl requester::PeerTransport for Never {
        fn exchange<'a>(
            &'a self,
            _: wire::SignedRequest,
            _: crate::topology::membership::MembershipLease,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, wire::SignedResponse> {
            Box::pin(async { panic!("incomplete headers must not relay") })
        }
    }
    impl server::LocalPageService for Never {
        fn serve_peer<'a>(
            &'a self,
            _: wire::VerifiedRequest,
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
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let io = Rc::new(HttpIo::with_admission(
            reactor.clone(),
            Codec::new(
                wire::MAX_ENVELOPE_HEAD,
                crate::model::range::PAGE_BYTES + 16,
            ),
            admission.clone(),
        ));
        let (signers, _) = identities();
        let forwarding = Rc::new(Forwarding::new(signers[2].clone()));
        let relay = Rc::new(relay::Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
            forwarding.clone(),
            Rc::new(Never),
            admission.clone(),
        ));
        let mut server = server::PeerServer::new(
            io.clone(),
            forwarding,
            admission.clone(),
            Rc::new(Never),
            relay,
        )
        .with_wire(Rc::new(codec(&admission)))
        .with_request_timeout(if matches!(case, "listener" | "cancel" | "drop") {
            Duration::from_secs(60)
        } else if case == "idle" {
            Duration::from_secs(5)
        } else {
            Duration::from_millis(30)
        })
        .with_handshake(Rc::new(handshake::Handshake::new(signers[2].clone(), None)));
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
        let mut connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        if case == "idle" {
            let client =
                ConnectionLease::from_accepted(peer.try_clone().unwrap().into(), &admission)
                    .unwrap();
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
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
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
        http::{
            codec::Codec,
            io::HttpIo,
            pool::{Endpoint, HttpPool},
        },
        model::{
            envelope::PageEnvelope,
            metadata::{ExpiresAt, ObjectMetadata},
        },
        runtime::reactor::Reactor,
        security::{forwarding::ForwardedHead, protocol},
    };
    use std::{
        net::TcpListener,
        task::{Context, Poll},
    };
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(HttpIo::with_admission(
        reactor.clone(),
        Codec::new(64 * 1024, crate::model::range::PAGE_BYTES + 16),
        admission.clone(),
    ));
    let pool = Rc::new(HttpPool::new(reactor.clone(), admission.clone(), 1));
    let signers = signers();
    let transfers = transfer::Transfers::new(pool, io.clone(), None)
        .with_wire(admission.clone(), Rc::new(codec(&admission)));
    transfers.set_signatures(signers[0].clone());
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
            version,
            length: 8192,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
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
    let scope =
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(30)).unwrap();
    let server = async {
        use crate::{http::pool::ConnectionLease, runtime::reactor::IoBuffer};
        let fd = reactor
            .accept(
                Rc::new(crate::runtime::reactor::Descriptor::from(listener)),
                &scope,
            )
            .await?;
        let conn = ConnectionLease::from_accepted(fd, &admission)?;
        let mut conn =
            crate::security::connection::accept(&io, conn, signers[2].clone(), &scope).await?;
        for attempt in 0..3 {
            conn = io.receive_head(conn, &scope).await?.connection;
            conn = io
                .send_head(
                    conn,
                    WireCodec::encode(&authentication, true, body.len())?,
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
            let local = request(&admission, attempt);
            let scope = local.origin.scope().clone();
            let (signed, _) = Forwarding::new(signers[0].clone())
                .sign_request(local)
                .unwrap();
            let result = transfers.exchange(endpoint.clone(), signed, &scope).await;
            if attempt == 2 {
                assert!(result.is_err());
            } else {
                match result.unwrap().response {
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
    assert!(matches!(
        transfers.select(
            crate::topology::rails::TransportPlan::Rdma {
                rail: crate::topology::rails::RailId(0)
            },
            handshake::Capabilities {
                rdma: true,
                scoped_grants: true
            },
            None
        ),
        crate::topology::rails::TransportPlan::Http
    ));
}
#[test]
fn outbound_lease_routes_without_registry_and_rejects_non_neighbors() {
    use crate::topology::{
        graph::Graph,
        membership::{Member, Membership},
    };
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(7),
            (0..64)
                .map(|index| Member {
                    node: NodeId(format!("node-{index:02}")),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: format!("127.0.0.1:{}", 8000 + index),
                    rails: vec![],
                    alignment_enabled: false,
                })
                .collect(),
        )
        .unwrap(),
    );
    let local = membership.members()[0].node.clone();
    let network = PeerNetwork::new(
        local.clone(),
        Arc::new(crate::control::snapshot::PublishedState::default()),
    )
    .unwrap();
    assert!(matches!(
        network.membership(membership.version),
        Err(Error::IncompatibleMembership)
    ));
    let neighbors = Graph::new(membership.clone()).neighbors(&local).unwrap();
    assert!(neighbors.len() < membership.members().len() - 1);
    for member in membership.members() {
        let endpoint = network.endpoint(&membership, &member.node);
        if neighbors.contains(&member.node) {
            assert_eq!(
                endpoint.unwrap(),
                crate::http::pool::Endpoint::Peer(member.peer_endpoint.clone())
            );
        } else {
            assert!(matches!(endpoint, Err(Error::InvalidRequest)));
        }
    }
    let admission = crate::runtime::admission::Admission::new(
        crate::test_support::cluster::config(false).limits,
    );
    let mut request = request(&admission, 1);
    assert_eq!(
        super::check_membership(&request, &membership),
        Err(Error::IncompatibleMembership)
    );
    request.route.membership = membership.version;
    assert_eq!(super::check_membership(&request, &membership), Ok(()));
}
