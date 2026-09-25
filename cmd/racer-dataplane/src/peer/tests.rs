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

fn signers() -> Vec<Rc<Signatures>> {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let roots = vec![ca.der().to_vec()];
    let mut signers = Vec::new();
    for name in ["a", "b", "c"] {
        let pending = PendingIdentity::generate().unwrap();
        let bytes = pending.export_pkcs8_for_persistence().unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(bytes.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://cluster/node/{name}").try_into().unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
        let cluster = ClusterId("cluster".into());
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
        cache: CacheId("cache".into()),
        key: CacheKey([3; 32]),
    };
    let route = RouteBudget {
        membership: MembershipVersion(1),
        request: scope.request,
        attempt: AttemptId([attempt; 16]),
        destination: NodeId("c".into()),
        visited: vec![NodeId("a".into())],
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
    let initial = search_budget(&request.route, &NodeId("a".into())).unwrap();
    assert!(initial.visited.is_empty());
    assert_eq!(initial.remaining_links, 4);
    let relay = search_budget(&request.route, &NodeId("b".into())).unwrap();
    assert_eq!(relay.visited, request.route.visited);
    assert_eq!(relay.remaining_links, 3);
    assert_eq!(relay.deadline.0, request.route.deadline.0);
    assert_eq!(request.route.remaining_links, 4);
}
