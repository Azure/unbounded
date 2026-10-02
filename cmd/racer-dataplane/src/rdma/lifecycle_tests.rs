use super::{lifecycle::*, test_support::*, *};
use crate::{
    model::*,
    runtime::admission::Admission,
    telemetry::metrics::{Gauge, Metrics},
};
use rdma_verbs::testing::State;
use std::task::Context;

#[test]
fn admitted_native_claim_keeps_capacity_after_proxy_drop_until_service_fence() {
    let (sim, io, mut native, _) = fixture(1);
    let metrics = Metrics::default();
    let admission = crate::peer::adaptive::AdaptivePeers::new(
        crate::peer::adaptive::Config {
            total: 1,
            per_peer: 1,
        },
        metrics.clone(),
    )
    .unwrap();
    let peer = NodeId("native-peer".into());
    let qp = immediate(QueuePairHandle::poll_new_admitted(
        io.device(0),
        Some(admission.acquire(&peer).unwrap()),
    ))
    .unwrap();
    sim.reject(simulation::Operation::Stop, None, true);
    drop(qp);
    native.poll_budgeted(1).unwrap();
    assert_eq!(metrics.gauge(Gauge::PeerExchanges), 1);
    assert!(matches!(admission.acquire(&peer), Err(Error::Overloaded)));
    sim.reject(simulation::Operation::Stop, None, false);
    native.retry_now(0);
    native.poll_budgeted(1).unwrap();
    assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
}

#[test]
fn failed_native_service_teardown_quarantines_adaptive_permit_after_both_roles_drop() {
    let (sim, io, mut native, charges) = fixture(1);
    let charged = &charges[0];
    let metrics = Metrics::default();
    let admission = crate::peer::adaptive::AdaptivePeers::new(
        crate::peer::adaptive::Config {
            total: 1,
            per_peer: 1,
        },
        metrics.clone(),
    )
    .unwrap();
    let peer = NodeId("native-quarantine".into());
    let qp = immediate(QueuePairHandle::poll_new_admitted(
        io.device(0),
        Some(admission.acquire(&peer).unwrap()),
    ))
    .unwrap();
    mark_connected(&qp, &mut native);
    let region = immediate(Region::poll_acquire(&qp, 16)).unwrap();
    let (window, ticket) = immediate(qp.poll_bind(region)).unwrap();
    native.poll_budgeted(1).unwrap();
    drop((window, ticket));
    sim.reject(simulation::Operation::Stop, None, true);
    drop(qp);
    drop(native);
    drop(io);
    assert_eq!(charged.get(), 1, "failed teardown keeps native ownership");
    assert_eq!(metrics.gauge(Gauge::PeerExchanges), 1);
    assert!(matches!(admission.acquire(&peer), Err(Error::Overloaded)));
    sim.reject(simulation::Operation::Stop, None, false);
}

#[test]
fn simultaneous_timeout_and_healthy_write_preserve_worker_and_quarantine() {
    let (sim, io, mut native, charges) = fixture(2);
    let failed = claim(&io);
    let healthy = claim(&io);
    connect_pair(&failed, &healthy, &mut native);
    let receive = immediate(Region::poll_acquire(&failed, 16)).unwrap();
    let (grant, binding) = immediate(failed.poll_bind(receive.clone())).unwrap();
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(binding.result(), Some(Ok(())));
    let source = immediate(Region::poll_acquire(&healthy, 16)).unwrap();
    immediate(source.poll_copy_from(&[7; 16])).unwrap();
    let written = immediate(healthy.poll_write(source, grant.address(), grant.key())).unwrap();
    native.poll_budgeted(2).unwrap();
    failed.expire_at(crate::runtime::environment::now());
    let sessions = Sessions::new(Rc::new(Devices::new()), 2);
    sessions.track_test(failed.clone());
    sessions.track_test(healthy.clone());
    sim.reject(simulation::Operation::Stop, Some(failed.endpoint.qpn), true);
    assert!(
        sessions.progress().is_ok(),
        "attempt timeout must not fail app's worker poll"
    );
    assert_eq!(failed.progress(), Err(rdma_verbs::Error::DeadlineExceeded));
    assert!(!failed.stopped());
    assert!(healthy.ready());
    native.poll_budgeted(2).unwrap();
    assert!(sessions.progress().is_ok());
    assert_eq!(written.result(), Some(Ok(())));
    assert_eq!(charges[0].get(), 1);
    assert_eq!(charges[1].get(), 1);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert_eq!(
        receive.poll_copy_to(&mut cx),
        Poll::Ready(Err(rdma_verbs::Error::Unavailable))
    );
    sim.reject(
        simulation::Operation::Stop,
        Some(failed.endpoint.qpn),
        false,
    );
    native.retry_now(0);
    native.poll_budgeted(2).unwrap();
    assert!(failed.stopped());
    assert!(immediate(receive.poll_copy_to(&mut cx)).is_ok());
    assert!(healthy.ready());
    assert_eq!(immediate(receive.poll_copy_to(&mut cx)).unwrap(), [7; 16]);
}

#[test]
fn retirement_cut_is_captured_and_does_not_stop_later_sessions() {
    let (_, io, mut native, _) = fixture(2);
    let first = claim(&io);
    mark_connected(&first, &mut native);
    let sessions = Sessions::new(Rc::new(Devices::new()), 2);
    sessions.track_test(first.clone());
    let mut cut = sessions.fence_cut();
    let second = claim(&io);
    mark_connected(&second, &mut native);
    sessions.track_test(second.clone());
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(cut.as_mut().poll(&mut cx).is_pending());
    assert!(!first.ready());
    assert!(!first.stopped());
    assert!(second.ready());
    native.poll_budgeted(2).unwrap();
    assert!(matches!(cut.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
    assert!(first.stopped());
    assert!(second.ready());
    assert!(!io.closed());
    assert!(sessions.progress().is_ok());
}

#[test]
fn cq_failure_is_attempt_local_and_slot_waits_for_all_leases_before_reuse() {
    let (sim, io, mut native, _) = fixture(2);
    let failed = claim(&io);
    let healthy = claim(&io);
    connect_pair(&failed, &healthy, &mut native);
    let region = immediate(Region::poll_acquire(&failed, 16)).unwrap();
    immediate(region.poll_copy_from(&[1; 16])).unwrap();
    sim.fault(
        simulation::Operation::Write,
        simulation::Fault::Completion(10),
    );
    let ticket = immediate(failed.poll_write(region.clone(), 4096, 7)).unwrap();
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    let sessions = Sessions::new(Rc::new(Devices::new()), 2);
    sessions.track_test(failed.clone());
    sessions.track_test(healthy.clone());
    assert!(sessions.progress().is_ok());
    assert_eq!(ticket.result(), Some(Err(rdma_verbs::Error::Io)));
    assert!(healthy.ready());
    assert!(!failed.stopped());
    native.poll_budgeted(2).unwrap();
    sessions.progress().unwrap();
    assert!(failed.stopped());
    drop(failed);
    drop(ticket);
    native.poll_budgeted(2).unwrap();
    assert_eq!(io.snapshot(0).state, State::Owned);
    drop(region);
    native.poll_budgeted(2).unwrap();
    assert_eq!(io.snapshot(0).state, State::Ready);
    assert!(healthy.ready());
}

#[test]
fn dropped_activation_does_not_publish_readiness_or_release_accepted_quota_early() {
    let (io, port) = pair(1).unwrap();
    let devices = Devices::new();
    devices.attach(io).unwrap();
    let admission = Admission::new(crate::test_support::cluster::config(true).limits);
    let scope = scope();
    let mut operation = devices.activate(Vec::new(), Vec::new(), &admission, 4096, &scope);
    assert!(
        operation
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending()
    );
    drop(operation);
    assert_eq!(admission.used(ResourceClass::Registered), 8192);
    let mut service = NativeService::new(port);
    service.poll_budgeted(1).unwrap();
    assert_eq!(admission.used(ResourceClass::Registered), 0);
    assert!(!devices.ready(RailId(0)));
}
