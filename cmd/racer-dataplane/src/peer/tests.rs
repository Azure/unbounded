//! Shared signed peer fixtures and scenario suites.
use super::*;
mod body_progress;
mod destination_disconnect;
mod opaque;
mod protocol_socket;
mod requester_safety;
mod subscriptions;
use crate::{
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
        connection::Signatures,
        forwarding::Forwarding,
        identity::{Certificates, Keyring},
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
    crate::security::test_support::identities(
        ClusterId(CLUSTER.into()),
        &[A, B, C].map(|name| NodeId(name.into())),
        || crate::security::connection::signature_tests::mac_test_key(CACHE),
    )
    .into_iter()
    .map(|identity| (identity.signatures, (identity.keys, identity.certificates)))
    .unzip()
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
pub(super) fn codec(admission: &Rc<Admission>) -> SecurityCodec {
    SecurityCodec::new(admission.clone(), BufferPool::new(admission.clone()))
}
