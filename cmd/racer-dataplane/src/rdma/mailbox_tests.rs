//! Public handoffs held at deterministic native mailbox boundaries.
use super::{lifecycle::*, test_support::*, *};
use crate::{model::*, runtime::admission::AdmissionPolicy, security::test_support::network};
use rdma_verbs::testing::{Contention, State};
use std::time::Duration;

#[test]
fn sessions_admit_64_neighbors_but_keep_per_neighbor_and_total_bounds() {
    let signers = network(2);
    let peer = verified(&signers, vec![]);
    let (_, io, _native, _) = fixture(2);
    let devices = Rc::new(Devices::test(io));
    let qp = immediate(QueuePairHandle::poll_new(
        devices.select(RailId(0)).unwrap().handle,
    ))
    .unwrap();
    let sessions = Sessions::new(devices, 1);
    for i in 0..63 {
        sessions.track_peer_test(NodeId(format!("peer-{i}")), qp.clone());
    }
    let scope = scope();
    let prepared = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
    assert!(matches!(
        poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
        Poll::Ready(Err(Error::Overloaded))
    ));
    drop(prepared);
    sessions.track_peer_test(NodeId("peer-63".into()), qp);
    assert!(matches!(
        poll(&mut sessions.prepare(&peer.peer, RailId(0), &scope)),
        Poll::Ready(Err(Error::Overloaded))
    ));
}

#[test]
fn signed_setup_waits_for_slot_and_connect_mailboxes_without_consuming_admission() {
    let signers = network(2);
    let peer = verified(&signers, vec![]);
    let (_, io, mut native, _) = fixture(2);
    let sessions = Sessions::new(Rc::new(Devices::test(io.clone())), 2);
    let scope = scope();
    let mut prepare = sessions.prepare(&peer.peer, RailId(0), &scope);
    io.with_contention(Contention::Slot(0), || {
        io.with_contention(Contention::Slot(1), || {
            assert!(poll(&mut prepare).is_pending());
            assert_eq!(io.snapshot(0).state, State::Ready);
        })
    });
    let prepared = done(&mut prepare);
    drop(prepare);
    let remote = done(&mut sessions.prepare(&peer.peer, RailId(0), &scope));
    assert!(matches!(
        QueuePairHandle::poll_new(io.device(0)),
        Poll::Ready(Err(rdma_verbs::Error::Overloaded))
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
    io.with_contention(Contention::Slot(0), || {
        assert!(poll(&mut finish).is_pending());
        assert!(!io.snapshot(0).cancelled);
    });
    let session = done(&mut finish);
    drop(finish);
    let mut ready = session.wait_ready(&scope);
    assert!(poll(&mut ready).is_pending());
    native.poll_budgeted(2).unwrap();
    io.with_contention(Contention::Slot(0), || {
        assert!(poll(&mut ready).is_pending())
    });
    done(&mut ready);
    assert!(session.ready());
}

#[test]
fn receive_preparation_and_sender_wait_at_every_buffer_and_command_boundary() {
    sender_case(None);
}
#[test]
fn successful_write_cancel_and_expiry_leave_failed_terminal_fence_quarantined() {
    for error in [Error::Cancelled, Error::DeadlineExceeded] {
        sender_case(Some(error));
    }
}
fn sender_case(terminal: Option<Error>) {
    let clock = environment::SimulationClock::new(62);
    let _time = clock.environment(0).enter();
    let signers = network(2);
    let (sim, io, mut native, charges) = fixture(2);
    let charged = &charges[1];
    let receiver = claim(&io);
    let sender = claim(&io);
    connect_pair(&receiver, &sender, &mut native);
    let receive = SessionLease::test(receiver.clone(), signers[0].node().clone());
    let send = SessionLease::test(sender.clone(), signers[0].node().clone());
    let devices = Rc::new(Devices::test(io.clone()));
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(true).limits,
    )));
    let transfer = RdmaTransfer::new(Rc::new(Sessions::new(devices, 2)));
    let mut scope = scope();
    if terminal == Some(Error::DeadlineExceeded) {
        scope.deadline.0 = environment::now() + Duration::from_secs(1);
    }
    let envelope = envelope();
    let id = TransferId([9; 16]);
    let mut prepare = transfer.prepare_receive(&receive, &envelope, id, &scope);
    io.with_contention(Contention::Slot(0), || {
        assert!(poll(&mut prepare).is_pending())
    });
    let grant = done(&mut prepare);
    drop(prepare);
    native.poll_budgeted(2).unwrap();
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
    io.with_contention(Contention::Slot(1), || {
        assert!(poll(&mut sending).is_pending());
        assert_eq!(admission.used(ResourceClass::Ciphertext), 32);
    });
    assert!(poll(&mut sending).is_pending());
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert!(poll(&mut sending).is_pending());
    if let Some(error) = terminal {
        let qpn = sender.endpoint.qpn;
        sim.reject(simulation::Operation::Stop, Some(qpn), true);
        native.poll_budgeted(2).unwrap();
        assert!(!sender.stopped());
        assert!(poll(&mut sending).is_pending());
        if error == Error::Cancelled {
            scope.cancel().unwrap();
        } else {
            clock.advance(Duration::from_secs(1));
        }
        assert!(matches!(poll(&mut sending), Poll::Ready(Err(e)) if e == error));
        drop(sending);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        drop((send, sender));
        native.retry_now(1);
        native.poll_budgeted(2).unwrap();
        assert_eq!(charged.get(), 1);
        assert_eq!(io.snapshot(1).state, State::Owned);
        assert!(!io.snapshot(1).fenced);
        sim.reject(simulation::Operation::Stop, Some(qpn), false);
        native.retry_now(1);
        native.poll_budgeted(2).unwrap();
        assert_eq!(io.snapshot(1).state, State::Ready);
        drop((grant, receive, receiver));
        native.close();
        native.poll_budgeted(2).unwrap();
        assert!(native.drained());
        assert_eq!(charged.get(), 0);
        return;
    }
    native.poll_budgeted(2).unwrap();
    done(&mut sending);
    drop(sending);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    drop(grant);
    native.poll_budgeted(2).unwrap();
    drop((receive, send, receiver, sender));
    native.poll_budgeted(2).unwrap();
    let qp = claim(&io);
    mark_connected(&qp, &mut native);
    let session = SessionLease::test(qp.clone(), signers[0].node().clone());
    let mut buffer = done(&mut RegisteredLease::acquire(&session, 32, &scope));
    let mut copy = buffer.copy_from(&[0x5a; 32], &scope);
    io.with_contention(Contention::Slot(0), || {
        assert!(poll(&mut copy).is_pending())
    });
    done(&mut copy);
    drop(copy);
    let mut bind = Grant::bind(&session, buffer, id, &scope);
    io.with_contention(Contention::Slot(0), || {
        assert!(poll(&mut bind).is_pending());
        assert!(!io.snapshot(0).cancelled);
    });
    let grant = done(&mut bind);
    drop(bind);
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    done(&mut grant.wait_bound(&scope));
    io.with_contention(Contention::Slot(0), || {
        assert!(grant.header_value().is_ok(), "bound descriptor is cached")
    });
}

#[test]
fn contended_grant_cancel_expiry_and_drop_abort_without_submitting_bind() {
    for mode in 0..3 {
        let clock = environment::SimulationClock::new(63);
        let _time = clock.environment(0).enter();
        let (_, io, mut native, charges) = fixture(1);
        let charged = &charges[0];
        let qp = claim(&io);
        mark_connected(&qp, &mut native);
        let session = SessionLease::test(qp.clone(), NodeId("peer".into()));
        let mut scope = scope();
        assert!(matches!(
            poll(&mut RegisteredLease::acquire(&session, 33, &scope)),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let buffer = done(&mut RegisteredLease::acquire(&session, 32, &scope));
        if mode == 1 {
            scope.deadline.0 = environment::now() + Duration::from_secs(1);
        }
        let mut bind = Grant::bind(&session, buffer, TransferId([1; 16]), &scope);
        io.with_contention(Contention::Slot(0), || {
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
                    clock.advance(Duration::from_secs(1));
                    assert!(matches!(
                        poll(&mut bind),
                        Poll::Ready(Err(Error::DeadlineExceeded))
                    ));
                }
                _ => {}
            }
            drop(bind);
            assert!(io.snapshot(0).cancelled);
            assert!(!qp.stopped());
            assert_eq!(charged.get(), 1);
        });
        assert!(!io.command_pending(0));
        native.poll_budgeted(1).unwrap();
        assert!(qp.stopped());
        drop((session, qp));
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.snapshot(0).state, State::Ready);
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
        let (_, io, mut native, _) = fixture(2);
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
        io.with_contention(Contention::Slot(0), || {
            assert!(poll(&mut finish).is_pending());
            if cancel {
                scope.cancel().unwrap();
                assert!(matches!(
                    poll(&mut finish),
                    Poll::Ready(Err(Error::Cancelled))
                ));
            }
            drop(finish);
            assert!(io.snapshot(0).cancelled);
            assert!(!io.snapshot(0).fenced);
        });
        assert!(!io.command_pending(0));
        native.poll_budgeted(2).unwrap();
        sessions.progress().unwrap();
        native.poll_budgeted(2).unwrap();
        assert_eq!(io.snapshot(0).state, State::Ready);
    }
}

#[test]
fn activation_contention_retains_quota_and_cancellation_releases_unsubmitted_configuration() {
    for activation_lock in [false, true] {
        let (io, port) = pair(1).unwrap();
        let mut native = NativeService::new(port);
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let scope = scope();
        let plan = || rdma_verbs::Configuration {
            discover: false,
            bytes: 4096,
            selector: Box::new(|_| Ok(vec![])),
            guards: vec![std::sync::Arc::new(
                admission
                    .reserve(None, ResourceClass::Registered, 8192)
                    .unwrap(),
            )],
        };
        let configure = io.configure(plan());
        let mut configure: Operation<'_, ()> = Box::pin(async {
            let mut configure = std::pin::pin!(configure);
            wait(&scope, |cx| {
                std::future::Future::poll(configure.as_mut(), cx)
            })
            .await
        });
        io.with_contention(
            if activation_lock {
                Contention::Activation
            } else {
                Contention::Configuration
            },
            || {
                assert!(poll(&mut configure).is_pending());
                assert_eq!(admission.used(ResourceClass::Registered), 8192);
                scope.cancel().unwrap();
                assert!(matches!(
                    poll(&mut configure),
                    Poll::Ready(Err(Error::Cancelled))
                ));
                drop(configure);
                assert_eq!(admission.used(ResourceClass::Registered), 0);
                assert!(!io.configuration_submitted());
            },
        );
        futures::executor::block_on(io.configure(plan())).unwrap();
        native.poll_budgeted(1).unwrap();
        assert_eq!(admission.used(ResourceClass::Registered), 0);
    }
}
use uring_runtime::environment;
