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
fn signers() -> Vec<Rc<Signatures>> {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let roots = vec![ca.der().to_vec()];
    let mut signers = Vec::new();
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
        signers.push(Rc::new(Signatures::new(
            keys,
            certificates,
            Rc::new(ReplayWindow::new(Arc::new(ReplayState::default()), 100)),
        )));
    }
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
        let result = drive(transfers.exchange(
            endpoint.clone(),
            wire::SignedRequest {
                authentication,
                request: local,
            },
            &scope,
        ));
        if attempt == 2 {
            assert!(result.is_err());
        } else {
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
