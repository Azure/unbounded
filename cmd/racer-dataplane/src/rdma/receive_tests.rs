//! Deterministic mailbox schedules through authenticated receive completion.
use super::lifecycle::*;
use super::test_support::*;
use super::*;
use crate::{
    model::{ResourceClass, TransferId},
    runtime::admission::AdmissionPolicy,
    security::test_support::network,
};
use rdma_verbs::testing::{Contention, State};
use std::{
    task::{Context, Poll},
    time::Duration,
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
    receive_case(readback, terminal, false);
}
#[test]
fn successful_invalidation_cancel_and_expiry_leave_failed_terminal_fence_quarantined() {
    for error in [Error::Cancelled, Error::DeadlineExceeded] {
        receive_case(true, Some(error), true);
    }
}

fn receive_case(readback: bool, terminal: Option<Error>, failed_fence: bool) {
    let clock = environment::SimulationClock::new(61);
    let _time = clock.environment(0).enter();
    let (sim, io, mut native, charges) = fixture(2);
    let charged = &charges[0];
    let peer_metrics = crate::telemetry::metrics::Metrics::default();
    let peer_admission = crate::peer::adaptive::AdaptivePeers::new(
        crate::peer::adaptive::Config {
            total: 1,
            per_peer: 1,
        },
        peer_metrics.clone(),
    )
    .unwrap();
    let peer = crate::model::NodeId("native-peer".into());
    let permit = peer_admission.acquire(&peer).unwrap();
    let qp = immediate(QueuePairHandle::poll_new_admitted(
        io.device(0),
        Some(permit),
    ))
    .unwrap();
    let writer = claim(&io);
    connect_pair(&qp, &writer, &mut native);
    let signers = network(2);
    let session = SessionLease::test(qp.clone(), signers[0].node().clone());
    let devices = Rc::new(Devices::new());
    let sessions = Rc::new(Sessions::new(devices, 1));
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(true).limits,
    )));
    let transfer = RdmaTransfer::new(sessions);
    let envelope = envelope();
    let mut scope = scope();
    let id = TransferId([4; 16]);
    let grant =
        futures::executor::block_on(transfer.prepare_receive(&session, &envelope, id, &scope))
            .unwrap();
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    let descriptor = grant.descriptor().unwrap();
    // Populate the receive allocation with real simulated DMA, not a private
    // native-memory fixture. Readback must still wait for the receiver fence.
    let source = immediate(Region::poll_acquire(&writer, 32)).unwrap();
    immediate(source.poll_copy_from(&[0xa5; 32])).unwrap();
    let written =
        immediate(writer.poll_write(source.clone(), descriptor.address, descriptor.scoped_key))
            .unwrap();
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(written.result(), Some(Ok(())));
    drop((source, written));
    writer.stop().unwrap();
    native.poll_budgeted(2).unwrap();
    drop(writer);
    let completion = verified(
        &signers,
        vec![header(
            COMPLETION_HEADER,
            STANDARD
                .encode(completion_bytes(session.binding(), id))
                .into_bytes(),
        )],
    );
    if terminal == Some(Error::DeadlineExceeded) {
        scope.deadline.0 = environment::now() + Duration::from_secs(1);
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
        native.poll_budgeted(2).unwrap();
        native.poll_budgeted(2).unwrap();
        assert!(finish.as_mut().poll(&mut cx).is_pending());
        assert!(!qp.stopped());
        if failed_fence {
            sim.reject(simulation::Operation::Stop, Some(qp.endpoint.qpn), true);
        }
        native.poll_budgeted(2).unwrap();
        assert_eq!(qp.stopped(), !failed_fence);
    }
    io.with_contention(Contention::Slot(0), || {
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
            clock.advance(Duration::from_secs(1));
        }
        if let Some(error) = terminal {
            assert!(matches!(finish.as_mut().poll(&mut cx), Poll::Ready(Err(e)) if e == error));
        }
    });
    if terminal.is_none() {
        if !readback {
            assert!(finish.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(2).unwrap();
            native.poll_budgeted(2).unwrap();
            assert!(finish.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(2).unwrap();
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
    if failed_fence {
        assert!(!qp.stopped());
        assert_eq!(charged.get(), 1);
        assert!(
            !io.snapshot(0).fenced,
            "native receive bytes remain unavailable before the fence"
        );
        let qpn = qp.endpoint.qpn;
        drop(session);
        drop(qp);
        assert_eq!(
            peer_metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
            1
        );
        assert!(matches!(
            peer_admission.acquire(&peer),
            Err(Error::Overloaded)
        ));
        native.retry_now(0);
        native.poll_budgeted(2).unwrap();
        assert_eq!(charged.get(), 1);
        assert_eq!(io.snapshot(0).state, State::Owned);
        sim.reject(simulation::Operation::Stop, Some(qpn), false);
        native.retry_now(0);
        native.poll_budgeted(2).unwrap();
        assert_eq!(io.snapshot(0).state, State::Ready);
        assert_eq!(
            peer_metrics.gauge(crate::telemetry::metrics::Gauge::PeerExchanges),
            0
        );
        native.close();
        native.poll_budgeted(2).unwrap();
        assert!(native.drained());
        assert_eq!(charged.get(), 0);
        return;
    }
    native.poll_budgeted(2).unwrap();
    assert!(qp.stopped());
    assert_eq!(charged.get(), 1);
    assert_eq!(io.snapshot(0).state, State::Owned);
    drop(session);
    drop(qp);
    native.poll_budgeted(2).unwrap();
    assert_eq!(io.snapshot(0).state, State::Ready);
    native.close();
    native.poll_budgeted(2).unwrap();
    assert!(native.drained());
    assert_eq!(charged.get(), 0);
}
use uring_runtime::environment;
