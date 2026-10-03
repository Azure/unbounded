use super::*;
use crate::model::AttemptId;
mod fill;
mod flight;
mod hot_reads;
mod peer_copies;
mod remote;
mod timeouts;

pub(crate) fn page(byte: u8) -> crate::memory::page::PageResult {
    use crate::{
        memory::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
        model::*,
    };
    use std::sync::Arc;
    let admission = flow_control::Quotas::new(crate::runtime::admission::AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([byte; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        },
        number: PageNumber(0),
    };
    crate::memory::page::PageResult {
        metadata: ObjectMetadata {
            version: page.version.clone(),
            length: 1,
            content_type: None,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        },
        plaintext: VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: page.clone(),
                bytes: vec![byte],
                reservation: admission
                    .reserve(None, ResourceClass::Plaintext, 1)
                    .unwrap(),
            }),
        },
        ciphertext: CiphertextPage {
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                checksum: Default::default(),
                envelope: PageEnvelope {
                    page,
                    key_id: key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([0; 24]),
                    plaintext_length: 1,
                    ciphertext_length: 17,
                },
                bytes: vec![0; 17],
                reservation: admission
                    .reserve(None, ResourceClass::Ciphertext, 17)
                    .unwrap(),
            }),
        },
    }
}
#[test]
fn racer_capacity_and_reservation_errors_are_preserved() {
    use uring_runtime::drivers::*;
    assert!(matches!(
        reserve().map_err(Error::from),
        Err(Error::InvalidConfiguration)
    ));
    let queue = Rc::new(DriverQueue::new(1024));
    let _owner = queue.enter();
    let permits: Vec<_> = (0..1024).map(|_| reserve().unwrap()).collect();
    assert_eq!(queue.pending(), 1024);
    assert!(matches!(
        reserve().map_err(Error::from),
        Err(Error::Overloaded)
    ));
    drop(permits);
    assert_eq!(queue.pending(), 0);
}
#[test]
fn detached_operation_errors_release_capacity() {
    use uring_runtime::drivers::*;
    let queue = Rc::new(DriverQueue::new(1024));
    let _owner = queue.enter();
    reserve()
        .unwrap()
        .submit_detached(async { Err::<(), _>(Error::Io) });
    poll(
        &mut std::task::Context::from_waker(futures::task::noop_waker_ref()),
        1,
    );
    assert_eq!(pending(), 0);
}
#[test]
fn remote_budget_charges_final_incoming_link_and_never_restores_attempts() {
    let now = uring_runtime::environment::now();
    let scope = RequestScope::new(
        crate::model::RequestId([1; 16]),
        now + std::time::Duration::from_secs(60),
    )
    .unwrap();
    let mut route = crate::topology::routing::RouteBudget {
        membership: crate::model::MembershipVersion(1),
        request: scope.request,
        attempt: AttemptId([2; 16]),
        destination: crate::model::NodeId("destination".into()),
        visited: vec![crate::model::NodeId("sender".into())],
        remaining_links: 1,
        remaining_attempts: 0,
        deadline: scope.deadline,
    };
    let mut budget = inherited_budget(&route, &scope).unwrap();
    assert_eq!(budget.remaining_links(), 0);
    assert_eq!(
        budget.begin_attempt(now, scope.deadline.0),
        Err(Error::Unavailable)
    );
    route.remaining_links = 8;
    route.remaining_attempts = 3;
    route.deadline.0 = now + std::time::Duration::from_secs(5);
    let mut budget = inherited_budget(&route, &scope).unwrap();
    assert_eq!(budget.route_links(), 7);
    assert_eq!(
        budget.begin_attempt(now, scope.deadline.0),
        Ok(route.deadline.0)
    );
    assert_eq!(budget.remaining_attempts(), 2);
    route.remaining_links = 0;
    assert!(matches!(
        inherited_budget(&route, &scope),
        Err(Error::HopBudgetExhausted)
    ));
}
#[test]
fn peer_failures_are_not_copy_misses() {
    assert!(matches!(
        peer_error(Error::NotFound),
        Ok(PeerResponse::NotFound)
    ));
    assert!(matches!(
        peer_error(Error::VersionUnavailable),
        Ok(PeerResponse::VersionUnavailable)
    ));
    assert!(matches!(
        peer_error(Error::OriginRejected),
        Ok(PeerResponse::OriginRejected)
    ));
    assert!(matches!(
        peer_error(Error::Unauthorized),
        Err(Error::Unauthorized)
    ));
    assert!(matches!(
        peer_error(Error::OriginForbidden),
        Ok(PeerResponse::OriginForbidden)
    ));
    assert!(matches!(
        peer_error(Error::Overloaded),
        Ok(PeerResponse::Overloaded)
    ));
    assert!(matches!(
        peer_error(Error::CorruptRecord),
        Err(Error::CorruptRecord)
    ));
}
