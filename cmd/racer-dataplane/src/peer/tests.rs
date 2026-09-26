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
        replay::{ReplayState, ReplayWindow},
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
type Discovery = (Rc<Keyring>, Rc<Certificates>, Rc<ReplayWindow>);
fn identities() -> (Vec<Rc<Signatures>>, Vec<Discovery>) {
    identities_with_replay_capacity(100)
}
fn identities_with_replay_capacity(capacity: usize) -> (Vec<Rc<Signatures>>, Vec<Discovery>) {
    named_identities(&[A, B, C], capacity)
}
fn named_identities(names: &[&str], capacity: usize) -> (Vec<Rc<Signatures>>, Vec<Discovery>) {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let roots = vec![ca.der().to_vec()];
    let mut signers = Vec::new();
    let mut discovery = Vec::new();
    for &name in names {
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
        let replay = Rc::new(ReplayWindow::new(
            Arc::new(ReplayState::default()),
            capacity,
        ));
        discovery.push((keys.clone(), certificates.clone(), replay.clone()));
        signers.push(Rc::new(Signatures::new(keys, certificates, replay)));
    }
    (signers, discovery)
}

#[path = "fill_admission_tests.rs"]
mod fill_admission_tests;
#[path = "fleet_layer_tests.rs"]
mod fleet_layer_tests;
#[path = "replay_load_tests.rs"]
mod replay_load_tests;
pub(super) fn signers() -> Vec<Rc<Signatures>> {
    let (signers, _) = identities();
    for signer in &signers {
        for peer in &signers {
            signer
                .configure_authenticated_peer_challenge(
                    peer.node().clone(),
                    peer.challenge().unwrap(),
                )
                .unwrap();
        }
    }
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
    assert!(matches!(
        receiver.verify_request(codec.request(envelope, &scope).unwrap()),
        Err(Error::Replay)
    ));
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
    let network = Rc::new(PeerNetwork::new(NodeId(C.into()), 2).unwrap());
    network
        .install(Arc::new(
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
        ))
        .unwrap();
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4, 1000));
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
    let network = Rc::new(PeerNetwork::new(NodeId(C.into()), 2).unwrap());
    network
        .install(Arc::new(
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
        ))
        .unwrap();
    let handshake =
        handshake::Handshake::new(signers[2].clone(), None).with_http(network, transfers);
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
    p::push_binary(
        &mut head,
        "racer-session-challenge",
        &signers[0].challenge().unwrap(),
    );
    let signed = signers[0].sign(head).unwrap();
    let binding = signed_digest(&signed).unwrap();
    let reply = handshake.respond(signed).unwrap();
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
    assert!(signers[0].verify(tampered).is_err());
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
        let network = Rc::new(PeerNetwork::new(NodeId(B.into()), 1).unwrap());
        network
            .install(Arc::new(
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
            ))
            .unwrap();
        let relay = relay::Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 1, 1000)),
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
        let result = futures::executor::block_on(relay.forward(ingress, &scope));
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
        os::fd::OwnedFd,
        task::{Context, Poll},
    };
    struct NeverTransport;
    impl requester::PeerTransport for NeverTransport {
        fn exchange<'a>(
            &'a self,
            _: wire::SignedRequest,
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, wire::SignedResponse> {
            Box::pin(async { panic!("direct request must not relay") })
        }
    }
    struct Local(Rc<std::cell::Cell<usize>>);
    impl server::LocalPageService for Local {
        fn serve_peer<'a>(
            &'a self,
            request: wire::VerifiedRequest,
            scope: &'a RequestScope,
        ) -> crate::error::Operation<'a, PeerResponse> {
            Box::pin(async move {
                scope.check()?;
                // The authenticated request has its own budget, independent of
                // the server's five-second header cap.
                assert_eq!(scope.deadline.0, request.request().route.deadline.0);
                self.0.set(self.0.get() + 1);
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
    let (signers, discovery) = identities();
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
    let source_network = Rc::new(PeerNetwork::new(NodeId(A.into()), 1).unwrap());
    let destination_network = Rc::new(PeerNetwork::new(NodeId(C.into()), 1).unwrap());
    source_network.install(membership.clone()).unwrap();
    destination_network.install(membership).unwrap();
    let source_handshake = Rc::new(
        handshake::Handshake::new(signers[0].clone(), None)
            .with_http(source_network.clone(), transfers.clone())
            .with_discovery(
                discovery[0].0.clone(),
                discovery[0].1.clone(),
                discovery[0].2.clone(),
            ),
    );
    let destination_handshake = Rc::new(
        handshake::Handshake::new(signers[2].clone(), None)
            .with_http(destination_network.clone(), transfers.clone())
            .with_discovery(
                discovery[2].0.clone(),
                discovery[2].1.clone(),
                discovery[2].2.clone(),
            ),
    );
    let destination_auth = Rc::new(Forwarding::new(signers[2].clone()));
    let paths = Rc::new(Paths::new(Rc::new(LinkHealth), 4, 1000));
    let relay = Rc::new(
        relay::Relay::new(
            paths.clone(),
            destination_auth.clone(),
            Rc::new(NeverTransport),
            admission.clone(),
        )
        .with_network(destination_network.clone()),
    );
    let served = Rc::new(std::cell::Cell::new(0));
    let server = server::PeerServer::new(
        io,
        destination_auth,
        admission.clone(),
        Rc::new(Local(served.clone())),
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
        source_handshake.clone(),
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
            .accept(Rc::new(OwnedFd::from(listener)), &scope)
            .await?;
        let mut connection = ConnectionLease::from_accepted(fd, &admission)?;
        // Each repeated request uses one exchange, not a probe plus handshake.
        // With the fixture's 100-entry replay window, redundant negotiation also
        // exhausts replay admission before this sequence can complete.
        let mut exchanges = 0;
        while served.get() < 66 {
            connection = server.serve_connection(connection, &listener_scope).await?;
            exchanges += 1;
        }
        assert_eq!(exchanges, 66 + 2 * 2);
        Ok::<(), Error>(())
    };
    let requests = async {
        let response = requester.request(local, &scope).await?;
        assert!(matches!(response.response(), PeerResponse::Miss));
        for attempt in 2..66 {
            let response = requester
                .request(request(&admission, attempt), &scope)
                .await?;
            assert!(matches!(response.response(), PeerResponse::Miss));
        }
        // Explicit invalidation rediscovers the authenticated challenge and
        // capabilities before the next request, as required after peer restart.
        source_handshake.invalidate(&NodeId(C.into()));
        let response = requester.request(request(&admission, 66), &scope).await?;
        // The server has closed. A failed exchange invalidates negotiation so a
        // later attempt can discover a restarted receiver's new challenge.
        assert!(matches!(
            requester.request(request(&admission, 67), &scope).await,
            Err(Error::Io)
        ));
        assert_eq!(
            source_handshake.negotiate(&NodeId(C.into())).await,
            Err(Error::Unavailable)
        );
        Ok::<_, Error>(response)
    };
    let exchange = async { futures::try_join!(requests, server_work) };
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
            _: &'a RequestScope,
        ) -> crate::error::Operation<'a, wire::SignedResponse> {
            Box::pin(async { panic!("incomplete headers must not relay") })
        }
    }
    impl server::LocalPageService for Never {
        fn serve_peer<'a>(
            &'a self,
            _: wire::VerifiedRequest,
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
        let (signers, discovery) = identities();
        let forwarding = Rc::new(Forwarding::new(signers[2].clone()));
        let relay = Rc::new(relay::Relay::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4, 1000)),
            forwarding.clone(),
            Rc::new(Never),
            admission.clone(),
        ));
        let mut server =
            server::PeerServer::new(io, forwarding, admission.clone(), Rc::new(Never), relay)
                .with_wire(Rc::new(codec(&admission)))
                .with_request_timeout(if matches!(case, "listener" | "cancel" | "drop") {
                    Duration::from_secs(60)
                } else if case == "idle" {
                    Duration::from_secs(5)
                } else {
                    Duration::from_millis(30)
                })
                .with_handshake(Rc::new(
                    handshake::Handshake::new(signers[2].clone(), None).with_discovery(
                        discovery[2].0.clone(),
                        discovery[2].1.clone(),
                        discovery[2].2.clone(),
                    ),
                ));
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
            // Real successful challenge exchanges leave the same connection
            // reusable, then an idle next exchange must time out too.
            for _ in 0..2 {
                use crate::{
                    http::codec::{MessageHead, StartLine},
                    security::protocol as p,
                };
                let probe = crate::security::session::ChallengeProbe::new(
                    NodeId(A.into()),
                    NodeId(C.into()),
                )
                .unwrap();
                let mut head = MessageHead {
                    start: StartLine::Request {
                        method: "POST".into(),
                        target: "/racer/peer/v1/challenge".into(),
                    },
                    headers: vec![],
                };
                p::push(&mut head, "content-length", 0);
                p::push_binary(&mut head, "racer-probe", &probe.request_bytes());
                let http = Codec::new(
                    wire::MAX_ENVELOPE_HEAD,
                    crate::model::range::PAGE_BYTES + 16,
                );
                peer.write_all(&http.encode_head(&head).unwrap()).unwrap();
                connection = drive(&reactor, server.serve_connection(connection, &scope)).unwrap();
                assert!(connection.is_reusable());
                let mut bytes = Vec::new();
                while !bytes.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    peer.read_exact(&mut byte).unwrap();
                    bytes.push(byte[0]);
                }
                let (head, _) = http.decode_head(&bytes).unwrap().unwrap();
                assert!(matches!(head.start, StartLine::Response { status: 200 }));
                let mut body = vec![0; head.content_length().unwrap().unwrap() as usize];
                peer.read_exact(&mut body).unwrap();
                probe
                    .verify(
                        &discovery[0].1,
                        crate::security::session::ChallengeReply::decode(&body).unwrap(),
                    )
                    .unwrap();
            }
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
        security::{forwarding::ForwardedHead, protocol, signing::SignedHead},
    };
    use std::{
        io::{Read, Write},
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
    let transfers = transfer::Transfers::new(pool, io, None)
        .with_wire(admission.clone(), Rc::new(codec(&admission)));
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
    let head = protocol::response_head(&response, &[3; 32], &[NodeId(A.into()), NodeId(C.into())])
        .unwrap();
    // This test isolates transport framing. Cryptographic verification is tested
    // above using real certificates; these bytes are deliberately unverified.
    let authentication = ForwardedHead {
        original: Arc::new(SignedHead {
            head,
            signature: vec![1; 64],
        }),
        hops: vec![],
    };
    let encoded = Codec::new(64 * 1024, crate::model::range::PAGE_BYTES + 16)
        .encode_head(&WireCodec::encode(&authentication, true, body.len()).unwrap())
        .unwrap();
    // The moved receive allocation must retain exactly one owned cache charge.
    // Invalid supplied charges must fail closed and release their accounting.
    let scope = request(&admission, 99).origin.scope().clone();
    let baseline = admission.used(ResourceClass::Ciphertext);
    for case in ["valid", "owner", "cache", "class", "size", "body"] {
        let other = Admission::new(crate::test_support::cluster::config(false).limits);
        let owner = if case == "owner" { &other } else { &admission };
        let cache = CacheId(if case == "cache" { A } else { CACHE }.into());
        let reservation = owner
            .reserve(
                Some(&cache),
                if case == "class" {
                    ResourceClass::Plaintext
                } else {
                    ResourceClass::Ciphertext
                },
                if case == "size" { 1 } else { body.len() },
            )
            .unwrap();
        let result = codec(&admission).response_reserved(
            ForwardedHead {
                original: authentication.original.clone(),
                hops: vec![],
            },
            if case == "body" {
                vec![0; 1]
            } else {
                body.clone()
            },
            Some(reservation),
            &scope,
        );
        if case == "valid" {
            let result = result.unwrap();
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                baseline + body.len()
            );
            drop(result);
        } else {
            assert!(result.is_err(), "{case}");
        }
        assert_eq!(
            admission.used(ResourceClass::Ciphertext),
            baseline,
            "{case}"
        );
        assert_eq!(other.used(ResourceClass::Ciphertext), 0, "{case}");
    }
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let expected = body.clone();
    let thread = std::thread::spawn(move || {
        let (mut socket, _) = listener.accept().unwrap();
        socket
            .set_read_timeout(Some(Duration::from_secs(10)))
            .unwrap();
        for attempt in 0..3 {
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
            }
            socket.write_all(&encoded).unwrap();
            let bytes = if attempt == 2 { &body[..5] } else { &body[..] };
            for chunk in bytes.chunks(257) {
                socket.write_all(chunk).unwrap();
            }
        }
    });
    let drive = |mut operation: crate::error::Operation<'_, wire::SignedResponse>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                break result;
            }
            reactor.poll_budgeted(128).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    for attempt in 0..3 {
        let local = request(&admission, attempt);
        let scope = local.origin.scope().clone();
        let authentication = ForwardedHead {
            original: Arc::new(SignedHead {
                head: protocol::request_head(&local).unwrap(),
                signature: vec![2; 64],
            }),
            hops: vec![],
        };
        let mut output = (attempt != 0).then(|| {
            admission
                .reserve(
                    Some(&CacheId(CACHE.into())),
                    ResourceClass::Ciphertext,
                    expected.len(),
                )
                .unwrap()
        });
        let before = admission.used(ResourceClass::Ciphertext);
        let result = drive(transfers.exchange_reserved(
            endpoint.clone(),
            wire::SignedRequest {
                authentication,
                request: local,
            },
            crate::topology::rails::TransportPlan::Http,
            &scope,
            &mut output,
        ));
        assert!(output.is_none());
        if attempt == 2 {
            assert!(result.is_err());
        } else {
            if attempt == 1 {
                assert_eq!(admission.used(ResourceClass::Ciphertext), before);
            }
            match result.unwrap().response {
                PeerResponse::Page { ciphertext, .. } => assert_eq!(ciphertext.bytes(), expected),
                _ => panic!("wrong response"),
            }
        }
    }
    thread.join().unwrap();
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
