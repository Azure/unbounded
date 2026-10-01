//! Shared signed peer fixtures and scenario suites.
use super::*;
mod body_progress;
mod destination_disconnect;
mod encrypted_http;
mod opaque;
mod protocol_socket;
mod requester_safety;
mod subscriptions;
mod timing;
use crate::{
    memory::pool::BufferPool,
    model::{
        EncryptedAuthorization, KeyId, MetadataSelector, Nonce, PeerOriginContext, ResourceClass, *,
    },
    peer::protocol::{
        FetchMode, Operation, PeerRequest, PeerResponse, SecurityCodec, decode_envelope,
        encode_envelope,
    },
    runtime::{
        admission::Admission,
        deadline::{Deadline, RequestScope},
    },
    security::{
        connection::Signatures,
        forwarding::Forwarding,
        identity::{Certificates, Keyring},
    },
    topology::routing::RouteBudget,
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

/// Common signed HTTP plumbing. Scenarios retain their own membership and service.
pub(crate) struct SocketFixture {
    pub admission: Rc<Admission>,
    pub reactor: Rc<crate::runtime::reactor::Reactor>,
    pub io: Rc<crate::http::connection::HttpIo>,
    pub codec: Rc<SecurityCodec>,
    pub pool: Rc<crate::http::connection::HttpPool>,
}

impl SocketFixture {
    pub fn new(pool_limit: usize) -> Self {
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(crate::runtime::reactor::Reactor::new(admission.clone()));
        let io = Rc::new(crate::http::connection::HttpIo::with_admission(
            reactor.clone(),
            crate::http::Codec::new(protocol::MAX_ENVELOPE_HEAD, PAGE_BYTES + 16),
            admission.clone(),
        ));
        Self {
            codec: Rc::new(codec(&admission)),
            pool: Rc::new(crate::http::connection::HttpPool::new(
                reactor.clone(),
                admission.clone(),
                pool_limit,
            )),
            admission,
            reactor,
            io,
        }
    }

    pub fn transfers(&self, signer: Rc<Signatures>) -> Rc<transport::Transfers> {
        Rc::new(transport::Transfers::new(
            self.pool.clone(),
            self.io.clone(),
            None,
            self.admission.clone(),
            self.codec.clone(),
            signer,
        ))
    }
}

struct NeverTransport;
impl PeerTransport for NeverTransport {
    fn exchange<'a>(
        &'a self,
        _: protocol::SignedRequest,
        _: crate::topology::membership::MembershipLease,
        _: &'a RequestScope,
    ) -> crate::error::Operation<'a, protocol::SignedResponse> {
        Box::pin(async { panic!("direct request must not relay") })
    }
}
