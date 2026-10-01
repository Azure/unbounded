//! Shared signed peer fixtures and scenario suites.
use super::*;
mod body_progress;
#[path = "destination_disconnect_tests.rs"]
mod destination_disconnect;
mod opaque;
mod protocol_socket;
mod subscriptions;
use crate::{
    control::wire::{BundleGeneration, KeyringBundle, SCHEMA_VERSION},
    memory::pool::BufferPool,
    model::{
        EncryptedAuthorization, KeyId, MetadataSelector, Nonce, PeerOriginContext, ResourceClass, *,
    },
    peer::protocol::{FetchMode, Operation, PeerRequest, PeerResponse, SecurityCodec, WireCodec},
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
            cache_keys: crate::security::signing::tests::mac_test_key(CACHE),
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
pub(super) fn request(admission: &Admission, attempt: u8) -> PeerRequest {
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
