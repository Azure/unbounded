//! Hold native mailboxes at each public handoff, independently of thread timing.
use super::tests::{claim, mark_connected, provision_test};
use super::*;
use crate::{
    error::Operation,
    http::codec::{Header, MessageHead, StartLine},
    memory::pool::BufferPool,
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
        permission::{AuthenticatedDescriptor, DESCRIPTOR_HEADER, Permissions},
        registered::RegisteredPool,
        session::{SETUP_BINDING_HEADER, SETUP_HEADER, SessionLease, Sessions},
        transfer::RdmaTransfer,
        verbs::{QueuePairHandle, Region},
    },
    runtime::admission::Admission,
    security::signing::{Signatures, VerifiedHead, tests::network},
};
use std::{
    task::{Context, Poll},
    time::{Duration, Instant},
};

fn scope() -> RequestScope {
    RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10)).unwrap()
}
fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
    operation
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
fn done<T>(operation: &mut Operation<'_, T>) -> T {
    match poll(operation) {
        Poll::Ready(Ok(value)) => value,
        Poll::Ready(Err(error)) => panic!("unexpected error: {error:?}"),
        Poll::Pending => panic!("unexpected pending operation"),
    }
}
fn verified(signers: &[Rc<Signatures>], headers: Vec<Header>) -> VerifiedHead {
    let mut headers = headers;
    headers.push(Header {
        name: "racer-receiver".into(),
        value: signers[1].node().0.as_bytes().to_vec(),
    });
    signers[1]
        .verify(
            signers[0]
                .sign(MessageHead {
                    start: StartLine::Request {
                        method: "POST".into(),
                        target: "/racer/peer/v1/rdma".into(),
                    },
                    headers,
                })
                .unwrap(),
        )
        .unwrap()
}
fn header(name: &str, value: Vec<u8>) -> Header {
    Header {
        name: name.into(),
        value,
    }
}

#[test]
fn signed_setup_waits_for_slot_and_connect_mailboxes_without_consuming_admission() {
    let signers = network(2);
    let peer = verified(&signers, vec![]);
    let (io, port) = pair(2).unwrap();
    let io = Rc::new(io);
    let mut native = NativeService::new(port);
    provision_test(&mut native, 0);
    provision_test(&mut native, 1);
    let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
    let scope = scope();
    let mut prepare = sessions.prepare(&peer.peer, RailId(0), &scope);
    let guard0 = io.shared.slots[0].mailbox.lock().unwrap();
    let guard1 = io.shared.slots[1].mailbox.lock().unwrap();
    assert!(poll(&mut prepare).is_pending());
    assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
    drop((guard0, guard1));
    let prepared = done(&mut prepare);
    drop(prepare);
    let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
    let device = Rc::new(crate::rdma::verbs::DeviceHandle {
        port: io.clone(),
        rail: RailId(0),
    });
    assert!(matches!(
        QueuePairHandle::poll_new(device),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(matches!(
        poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
        Poll::Ready(Err(Error::Overloaded))
    ));
    let ack = verified(
        &signers,
        vec![
            header(SETUP_HEADER, remote.setup().header_value()),
            header(
                SETUP_BINDING_HEADER,
                prepared.setup().binding_header_value(),
            ),
        ],
    );
    let mut finish = prepared.finish(&ack, &scope);
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(poll(&mut finish).is_pending());
    assert!(!io.shared.slots[0].cancel.load(Ordering::Acquire));
    drop(guard);
    let session = done(&mut finish);
    drop(finish);
    let mut ready = session.wait_ready(&scope);
    assert!(poll(&mut ready).is_pending());
    native.poll_budgeted(2).unwrap();
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(poll(&mut ready).is_pending());
    drop(guard);
    done(&mut ready);
    assert!(session.ready());
}

fn envelope() -> PageEnvelope {
    PageEnvelope {
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("mailbox-test".into()),
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
    }
}

#[test]
fn receive_preparation_and_sender_wait_at_every_buffer_and_command_boundary() {
    let signers = network(2);
    let (io, port) = pair(2).unwrap();
    let io = Rc::new(io);
    let mut native = NativeService::new(port);
    provision_test(&mut native, 0);
    provision_test(&mut native, 1);
    let receiver = claim(&io);
    let sender = claim(&io);
    mark_connected(&receiver, &mut native);
    mark_connected(&sender, &mut native);
    let receive = SessionLease::test(receiver.clone(), signers[0].node().clone());
    let send = SessionLease::test(sender.clone(), signers[0].node().clone());
    let devices = Rc::new(Devices::test(io.clone()));
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(true).limits,
    ));
    let buffers = Rc::new(RegisteredPool::new(devices.clone(), admission.clone()));
    let transfer = RdmaTransfer::new(
        Rc::new(Sessions::new(devices, 2)),
        buffers.clone(),
        Rc::new(Permissions),
    );
    let scope = scope();
    let envelope = envelope();
    let id = TransferId([9; 16]);
    let mut prepare = transfer.prepare_receive(&receive, &envelope, id, &scope);
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(poll(&mut prepare).is_pending());
    drop(guard);
    let grant = done(&mut prepare);
    drop(prepare);
    native.poll_budgeted(2).unwrap();
    backend::lifetime_tests::complete(1, 0, 5);
    native.poll_budgeted(2).unwrap();
    done(&mut grant.wait_bound(&scope));
    let signed = verified(
        &signers,
        vec![header(DESCRIPTOR_HEADER, grant.header_value().unwrap())],
    );
    let descriptor = AuthenticatedDescriptor::from_verified(&signed, &send, id).unwrap();
    let page = BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(
                    Some(&envelope.page.version.object.cache),
                    ResourceClass::Ciphertext,
                    32,
                )
                .unwrap(),
            envelope,
            vec![0xa5; 32],
        )
        .unwrap();
    let mut sending = transfer.send_to(&send, page, descriptor, &scope);
    let guard = io.shared.slots[1].mailbox.lock().unwrap();
    assert!(poll(&mut sending).is_pending());
    assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
    drop(guard);
    assert!(poll(&mut sending).is_pending());
    native.poll_budgeted(2).unwrap();
    backend::lifetime_tests::complete(1, 0, 1);
    native.poll_budgeted(2).unwrap();
    assert!(poll(&mut sending).is_pending());
    native.poll_budgeted(2).unwrap();
    done(&mut sending);
    drop(sending);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);

    // Independent public buffer copy and grant bind boundaries after acquisition.
    drop(grant);
    native.poll_budgeted(2).unwrap();
    drop((receive, send, receiver, sender));
    native.poll_budgeted(2).unwrap();
    let qp = claim(&io);
    mark_connected(&qp, &mut native);
    let session = SessionLease::test(qp.clone(), signers[0].node().clone());
    let mut buffer = done(&mut buffers.acquire_for(&session, 32, &scope));
    let mut copy = buffer.copy_from(&[0x5a; 32], &scope);
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(poll(&mut copy).is_pending());
    drop(guard);
    done(&mut copy);
    drop(copy);
    let permissions = Permissions;
    let mut bind = permissions.grant(&session, buffer, id, &scope);
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(poll(&mut bind).is_pending());
    assert!(!io.shared.slots[0].cancel.load(Ordering::Acquire));
    drop(guard);
    let grant = done(&mut bind);
    drop(bind);
    native.poll_budgeted(2).unwrap();
    backend::lifetime_tests::complete(1, 0, 5);
    native.poll_budgeted(2).unwrap();
    done(&mut grant.wait_bound(&scope));
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(grant.header_value().is_ok(), "bound descriptor is cached");
    drop(guard);
}

#[test]
fn command_poll_distinguishes_contended_completion_from_full_queue() {
    let (io, port) = pair(1).unwrap();
    let io = Rc::new(io);
    let mut native = NativeService::new(port);
    provision_test(&mut native, 0);
    let qp = claim(&io);
    mark_connected(&qp, &mut native);
    let region = Region::acquire(&qp, 32).unwrap();
    assert!(matches!(
        Region::poll_acquire(&qp, 32),
        Poll::Ready(Err(Error::Overloaded))
    ));
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(qp.poll_write(region.clone(), 4096, 7).is_pending());
    drop(guard);
    assert!(matches!(
        qp.poll_write(region.clone(), 4096, 7),
        Poll::Ready(Ok(_))
    ));
    assert!(matches!(
        qp.poll_write(region.clone(), 4096, 7),
        Poll::Ready(Err(Error::Overloaded))
    ));
    native.poll_budgeted(1).unwrap();
    backend::lifetime_tests::complete(1, 0, 1);
    native.poll_budgeted(1).unwrap();
    let guard = io.shared.slots[0].mailbox.lock().unwrap();
    assert!(qp.poll_write(region.clone(), 4096, 7).is_pending());
    drop(guard);
    assert!(matches!(qp.poll_write(region, 4096, 7), Poll::Ready(Ok(_))));
}

#[test]
fn contended_grant_cancel_expiry_and_drop_abort_without_submitting_bind() {
    for mode in 0..3 {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let charged = provision_test(&mut native, 0);
        let qp = claim(&io);
        mark_connected(&qp, &mut native);
        let session = SessionLease::test(qp.clone(), crate::model::identity::NodeId("peer".into()));
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let buffers = RegisteredPool::new(Rc::new(Devices::test(io.clone())), admission);
        let mut scope = scope();
        assert!(matches!(
            poll(&mut buffers.acquire_for(&session, 33, &scope)),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let buffer = done(&mut buffers.acquire_for(&session, 32, &scope));
        if mode == 1 {
            scope.deadline.0 = Instant::now() + Duration::from_secs(1);
        }
        let permissions = Permissions;
        let mut bind = permissions.grant(&session, buffer, TransferId([1; 16]), &scope);
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut bind).is_pending());
        match mode {
            0 => {
                scope.cancel().unwrap();
                assert!(matches!(
                    poll(&mut bind),
                    Poll::Ready(Err(Error::Cancelled))
                ));
            }
            1 => {
                std::thread::sleep(scope.deadline.0.saturating_duration_since(Instant::now()));
                assert!(matches!(
                    poll(&mut bind),
                    Poll::Ready(Err(Error::DeadlineExceeded))
                ));
            }
            _ => {}
        }
        drop(bind);
        assert!(guard.command.is_none());
        assert!(io.shared.slots[0].cancel.load(Ordering::Acquire));
        assert!(!qp.stopped());
        assert_eq!(charged.get(), 1);
        drop(guard);
        native.poll_budgeted(1).unwrap();
        assert!(qp.stopped());
        drop((session, qp));
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
        native.close();
        native.poll_budgeted(1).unwrap();
        assert_eq!(charged.get(), 0);
    }
}

#[test]
fn canceled_or_abandoned_contended_signed_setup_releases_only_after_fence() {
    let signers = network(2);
    let peer = verified(&signers, vec![]);
    for cancel in [false, true] {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
        let scope = scope();
        let prepared = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
        let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
        let ack = verified(
            &signers,
            vec![
                header(SETUP_HEADER, remote.setup().header_value()),
                header(
                    SETUP_BINDING_HEADER,
                    prepared.setup().binding_header_value(),
                ),
            ],
        );
        let mut finish = prepared.finish(&ack, &scope);
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(poll(&mut finish).is_pending());
        if cancel {
            scope.cancel().unwrap();
            assert!(matches!(
                poll(&mut finish),
                Poll::Ready(Err(Error::Cancelled))
            ));
        }
        drop(finish);
        assert!(guard.command.is_none());
        assert!(io.shared.slots[0].cancel.load(Ordering::Acquire));
        assert!(!io.shared.slots[0].fenced.load(Ordering::Acquire));
        drop(guard);
        native.poll_budgeted(2).unwrap();
        sessions.progress().unwrap();
        native.poll_budgeted(2).unwrap();
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), READY);
    }
}

#[test]
fn activation_contention_retains_quota_and_cancellation_releases_unsubmitted_configuration() {
    for activation_lock in [false, true] {
        let (io, port) = pair(1).unwrap();
        let mut native = NativeService::new(port);
        let admission = Admission::new(crate::test_support::cluster::config(true).limits);
        let scope = scope();
        let quotas = vec![
            admission
                .reserve(None, ResourceClass::Registered, 8192)
                .unwrap(),
        ];
        let mut configure: Operation<'_, ()> =
            Box::pin(io.configure(vec![], vec![], quotas, 4096, &scope));
        let config = (!activation_lock).then(|| io.shared.config.lock().unwrap());
        let activation = activation_lock.then(|| io.shared.activation.lock().unwrap());
        assert!(poll(&mut configure).is_pending());
        assert_eq!(admission.used(ResourceClass::Registered), 8192);
        scope.cancel().unwrap();
        assert!(matches!(
            poll(&mut configure),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(configure);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert!(!io.shared.configured.load(Ordering::Acquire));
        drop((config, activation));
        let scope = super::mailbox_tests::scope();
        let mut configure: Operation<'_, ()> = Box::pin(io.configure(
            vec![],
            vec![],
            vec![admission.reserve(None, ResourceClass::Registered, 8192).unwrap()],
            4096,
            &scope,
        ));
        done(&mut configure);
        native.poll_budgeted(1).unwrap();
        assert_eq!(admission.used(ResourceClass::Registered), 0);
    }
}

#[test]
fn poisoned_mailbox_is_terminal_io_error_not_contention() {
    let mutex = std::sync::Mutex::new(());
    let _ = std::panic::catch_unwind(|| {
        let _guard = mutex.lock().unwrap();
        panic!("poison test mailbox");
    });
    assert!(matches!(
        crate::rdma::verbs::try_mailbox(&mutex),
        Poll::Ready(Err(Error::Io))
    ));
}
