//! Deterministic mailbox schedules through the authenticated receive completion path.
use super::tests::{claim, mark_connected, provision_test};
use super::*;
use crate::{
    http::codec::{Header, MessageHead, StartLine},
    model::{
        envelope::{KeyId, Nonce, PageEnvelope},
        identity::{
            CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, RequestId, StrongEtag,
            TransferId,
        },
        limits::ResourceClass,
    },
    rdma::{
        device::Devices,
        permission::{COMPLETION_HEADER, Permissions, completion_bytes},
        registered::RegisteredPool,
        session::{SessionLease, Sessions},
        transfer::RdmaTransfer,
        verbs::Verbs,
    },
    runtime::{admission::Admission, deadline::RequestScope},
    security::signing::tests::network,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{
    task::{Context, Poll},
    time::{Duration, Instant},
};

#[test]
fn receive_completion_waits_for_invalidation_mailbox() {
    receive_contended(false, None);
}

#[test]
fn receive_completion_waits_for_fenced_readback_mailbox() {
    receive_contended(true, None);
}

#[test]
fn receive_completion_contention_obeys_cancellation_and_deadline() {
    for readback in [false, true] {
        for error in [Error::Cancelled, Error::DeadlineExceeded] {
            receive_contended(readback, Some(error));
        }
    }
}

#[test]
fn receive_completion_does_not_retry_ciphertext_quota_exhaustion() {
    receive_contended(false, Some(Error::Overloaded));
}

fn receive_contended(readback: bool, terminal: Option<Error>) {
    let (io, port) = pair(1).unwrap();
    let io = Rc::new(io);
    let mut native = NativeService::new(port);
    let charged = provision_test(&mut native, 0);
    let qp = claim(&io);
    mark_connected(&qp, &mut native);
    let signers = network(2);
    let session = SessionLease::test(qp.clone(), signers[0].node().clone());
    let devices = Rc::new(Devices::new(Rc::new(Verbs)));
    let sessions = Rc::new(Sessions::new(devices.clone(), 1));
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(true).limits,
    ));
    let transfer = RdmaTransfer::new(
        sessions,
        Rc::new(RegisteredPool::new(devices, admission.clone())),
        Rc::new(Permissions),
    );
    let envelope = PageEnvelope {
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("receive-test".into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        },
        key_id: KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: 16,
        ciphertext_length: 32,
    };
    let mut scope =
        RequestScope::new(RequestId([3; 16]), Instant::now() + Duration::from_secs(10)).unwrap();
    let id = TransferId([4; 16]);
    native.resources[0]
        .as_ref()
        .unwrap()
        .region
        .copy_from(&[0xa5; 32])
        .unwrap();
    let grant = transfer
        .prepare_receive(&session, &envelope, id, &scope)
        .unwrap();
    native.poll_budgeted(1).unwrap();
    backend::lifetime_tests::complete(1, 0, 5);
    native.poll_budgeted(1).unwrap();
    grant.descriptor().unwrap();
    let signed = signers[0]
        .sign(MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/racer/peer/v1/rdma".into(),
            },
            headers: vec![
                Header {
                    name: "racer-receiver".into(),
                    value: signers[1].node().0.as_bytes().to_vec(),
                },
                Header {
                    name: COMPLETION_HEADER.into(),
                    value: STANDARD
                        .encode(completion_bytes(session.binding(), id))
                        .into_bytes(),
                },
            ],
        })
        .unwrap();
    let completion = signers[1].verify(signed).unwrap();
    // Expire only the finish scope; the grant remains valid so admission/binding
    // cannot be mistaken for the receive-completion deadline check.
    if terminal == Some(Error::DeadlineExceeded) {
        scope.deadline.0 = Instant::now() + Duration::from_secs(1);
    }
    let quota = (terminal == Some(Error::Overloaded)).then(|| {
        admission
            .reserve(
                None,
                ResourceClass::Ciphertext,
                admission.limit(ResourceClass::Ciphertext),
            )
            .unwrap()
    });
    let mut finish =
        transfer.finish_receive(&session, grant, &completion, envelope, &admission, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    if readback {
        assert!(finish.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(1).unwrap();
        backend::lifetime_tests::complete(2, 0, 6);
        native.poll_budgeted(1).unwrap();
        assert!(finish.as_mut().poll(&mut cx).is_pending());
        assert!(!qp.stopped());
        native.poll_budgeted(1).unwrap();
        assert!(qp.stopped());
    }
    // Hold the same mutex as the native role at invalidation submission or
    // immediately after it publishes the fence, before it releases the mailbox.
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    if terminal != Some(Error::Overloaded) {
        match finish.as_mut().poll(&mut cx) {
            Poll::Pending => {}
            Poll::Ready(Err(error)) => {
                panic!("mailbox contention failed an admitted receive: {error:?}")
            }
            Poll::Ready(Ok(_)) => panic!("readback bypassed the held mailbox"),
        }
        assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
    }
    if terminal == Some(Error::Cancelled) {
        scope.cancel().unwrap();
    }
    if terminal == Some(Error::DeadlineExceeded) {
        std::thread::sleep(scope.deadline.0.saturating_duration_since(Instant::now()));
    }
    if let Some(error) = terminal {
        assert!(matches!(finish.as_mut().poll(&mut cx), Poll::Ready(Err(e)) if e == error));
    }
    drop(guard);
    if terminal.is_none() {
        if !readback {
            assert!(finish.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            backend::lifetime_tests::complete(2, 0, 6);
            native.poll_budgeted(1).unwrap();
            assert!(finish.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
        }
        let Poll::Ready(Ok(page)) = finish.as_mut().poll(&mut cx) else {
            panic!("receive did not finish after mailbox release")
        };
        assert_eq!(page.bytes(), &[0xa5; 32]);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
        drop(page);
    }
    drop(finish);
    drop(quota);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    native.poll_budgeted(1).unwrap();
    assert!(qp.stopped());
    assert_eq!(charged.get(), 1);
    assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), OWNED);
    drop(session);
    drop(qp);
    native.poll_budgeted(1).unwrap();
    assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
    native.close();
    native.poll_budgeted(1).unwrap();
    assert!(native.drained());
    assert_eq!(charged.get(), 0);
}
