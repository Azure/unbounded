use super::*;
use rdma_verbs::{pair, simulation};
mod mailbox;

/// Production activation through the public simulated fabric.
mod activation_tests {
    use super::*;
    use crate::{
        model::*,
        runtime::{
            admission::AdmissionPolicy,
            crypto,
            worker::{CryptoRuntime, CryptoService},
        },
        security::aead::PageCryptoEngine,
    };
    use racer_identity::KeyPurpose;
    use rdma_verbs::testing::{Contention, State};
    use simulation::{Fault, Operation as NativeOp};
    use std::task::Context;

    fn fixture(
        slots: usize,
    ) -> (
        simulation::Simulation,
        Devices,
        NativeService,
        flow_control::Quotas<AdmissionPolicy>,
        RequestScope,
    ) {
        let sim = simulation::Simulation::new()
            .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
            .unwrap();
        let (io, port) = pair(slots).unwrap();
        let native = {
            let _environment = sim.enter();
            NativeService::new(port)
        };
        let devices = Devices::new();
        devices.attach(io).unwrap();
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let scope = RequestScope::new(
            RequestId([1; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        (sim, devices, native, admission, scope)
    }
    fn activate<'a>(
        devices: &'a Devices,
        admission: &'a flow_control::Quotas<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<RailMapping>> {
        devices.activate(
            vec![RailMapping {
                rail: RailId(0),
                device: "sim0".into(),
                port: 1,
                gid: None,
                numa_node: None,
            }],
            admission,
            4096,
            scope,
        )
    }
    fn port(devices: &Devices) -> Rc<IoPort> {
        devices.port.borrow().as_ref().unwrap().clone()
    }

    #[test]
    fn repeated_rail_selects_exact_physical_binding_and_revokes_changed_gid() {
        let sim = simulation::Simulation::new()
            .with_devices(vec![
                simulation::Device::new("a", [1; 16]),
                simulation::Device::new("b", [2; 16]),
            ])
            .unwrap();
        let _environment = sim.enter();
        let (io, native) = pair(2).unwrap();
        let mut service = NativeService::new(native);
        let devices = Devices::new();
        devices.attach(io).unwrap();
        let inventory = discovery::inventory();
        let publication: Vec<_> = inventory
            .iter()
            .cloned()
            .map(|mut n| {
                n.rail = RailId(7);
                n
            })
            .collect();
        let selected = discovery::select_worker(&publication, &inventory, 1, None, 2);
        assert_eq!(selected.len(), 1);
        assert_eq!(selected[0].device, "b");
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let scope = RequestScope::new(
            RequestId([3; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        assert!(matches!(
            futures::executor::block_on(devices.activate(publication, &admission, 4096, &scope)),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        let mut activation = devices.activate(selected.clone(), &admission, 4096, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        for _ in 0..8 {
            service.poll_budgeted(8).unwrap();
        }
        let Poll::Ready(Ok(actual)) = activation.as_mut().poll(&mut cx) else {
            panic!("activation pending");
        };
        drop(activation);
        assert_eq!(actual[0].device, "b");
        assert_eq!(actual[0].gid, Some([2; 16]));
        assert!(devices.ready(RailId(7)));
        assert!(devices.revalidate(&selected));
        let mut revoked = selected;
        revoked[0].gid = Some([3; 16]);
        assert!(!devices.revalidate(&revoked));
        assert!(!devices.ready(RailId(7)));
        for _ in 0..8 {
            service.poll_budgeted(8).unwrap();
        }
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        drop(service);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn configured_activation_spends_budget_and_yields_to_sibling_page_jobs() {
        let (sim, devices, mut native, admission, scope) = fixture(4);
        let shared = port(&devices);
        let mut activation = activate(&devices, &admission, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        assert!(
            !native.drained(),
            "queued configuration owns accepted quota"
        );
        native.poll_budgeted(0).unwrap();
        assert!(sim.trace().is_empty());
        assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
        let keys = crate::security::test_support::keys();
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::security::test_support::CACHE.into()),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        };
        let cache = &page.version.object.cache;
        let sibling_admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(sibling_admission.clone());
        let (io, port) = crypto::pair(WorkerId(1), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut sibling = PageCryptoEngine::new(CryptoRuntime { port });
        for turn in 0..5 {
            let Poll::Ready(Ok(permit)) = io.poll_reserve(
                &mut cx,
                crypto::CryptoId {
                    worker: WorkerId(1),
                    generation: 1,
                    sequence: turn + 1,
                },
            ) else {
                panic!("reserve")
            };
            assert!(
                io.try_submit(
                    permit.job(
                        crypto::CryptoInput::Encrypt {
                            page: page.clone(),
                            plaintext: pool
                                .plaintext(
                                    sibling_admission
                                        .reserve(Some(cache), ResourceClass::Plaintext, 5)
                                        .unwrap(),
                                    5
                                )
                                .unwrap(),
                            ciphertext: sibling_admission
                                .reserve(Some(cache), ResourceClass::Ciphertext, 21)
                                .unwrap(),
                        },
                        keys.active(cache, KeyPurpose::Page).unwrap(),
                        scope.clone()
                    )
                )
                .is_ok()
            );
            native.poll_budgeted(1).unwrap();
            assert_eq!(
                sim.take_trace()
                    .iter()
                    .filter(|event| event.operation == NativeOp::Register)
                    .count(),
                usize::from(turn != 0)
            );
            assert_eq!(native.resource_count(), turn as usize);
            if turn < 4 {
                assert!(activation.as_mut().poll(&mut cx).is_pending());
                assert!((0..shared.capacity()).all(|i| shared.snapshot(i).state == State::Idle));
                assert!(!devices.ready(RailId(0)));
            }
            sibling.poll_budgeted(1).unwrap();
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("sibling must progress")
            };
            assert!(matches!(
                completion.outcome,
                crypto::CryptoOutcome::Completed(_)
            ));
            drop(completion);
            assert_eq!(sibling_admission.used(ResourceClass::Plaintext), 0);
        }
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(activation);
        assert!(devices.ready(RailId(0)));
        devices.close();
        for remaining in (0..4).rev() {
            native.poll_budgeted(1).unwrap();
            assert_eq!(native.resource_count(), remaining);
        }
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn partial_activation_errors_fence_before_retry_without_publishing_readiness() {
        for failure in [NativeOp::Register, NativeOp::Qp, NativeOp::Window] {
            let (sim, devices, mut native, admission, scope) = fixture(3);
            let io = port(&devices);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut activation = activate(&devices, &admission, &scope);
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(2).unwrap();
            assert_eq!(native.resource_count(), 1);
            sim.fault(failure, Fault::Reject);
            native.poll_budgeted(1).unwrap();
            assert!(matches!(
                activation.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            ));
            drop(activation);
            assert!(!devices.ready(RailId(0)));
            assert!((0..io.capacity()).all(|i| io.snapshot(i).state == State::Idle));
            sim.fault(NativeOp::Stop, Fault::Reject);
            native.poll_budgeted(1).unwrap();
            assert!(native.resource_present(0));
            assert!(!io.pool_drained());
            assert!(admission.used(ResourceClass::Registered) >= 8192);
            let mut retry = activate(&devices, &admission, &scope);
            assert!(matches!(
                retry.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
            drop(retry);
            native.retry_now(0);
            native.poll_budgeted(3).unwrap();
            assert!(io.pool_drained());
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert_eq!(sim.live_resources(), 0);
            let mut retry = activate(&devices, &admission, &scope);
            assert!(retry.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(3).unwrap();
            assert!(retry.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            assert!(matches!(retry.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
            drop(retry);
            devices.close();
            native.poll_budgeted(3).unwrap();
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    fn abandoned_activation_cleans_queued_discovered_and_partial_owners() {
        for turns in 0..=3 {
            let (sim, devices, mut native, admission, scope) = fixture(4);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let mut activation = activate(&devices, &admission, &scope);
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(turns).unwrap();
            assert!(!native.drained());
            drop(activation);
            native.poll_budgeted(1).unwrap();
            native.poll_budgeted(4).unwrap();
            assert!(native.drained());
            assert!(port(&devices).pool_drained());
            assert!(!devices.ready(RailId(0)));
            assert_eq!(admission.used(ResourceClass::Registered), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    fn activation_mailbox_contention_consumes_turn_without_losing_quota() {
        let (sim, devices, mut native, admission, scope) = fixture(2);
        let shared = port(&devices);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(1).unwrap();
        sim.take_trace();
        shared.with_contention(Contention::Slot(0), || {
            native.poll_budgeted(1).unwrap();
            assert!(sim.trace().is_empty());
            assert_eq!(admission.used(ResourceClass::Registered), 2 * 8192);
        });
        native.poll_budgeted(2).unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(activation);
        devices.close();
        native.poll_budgeted(2).unwrap();
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn configured_native_wrapper_drain_fences_one_slot_per_poll() {
        let sim = simulation::Simulation::new()
            .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
            .unwrap();
        let (native_io, native_port) = pair(4).unwrap();
        let devices = Devices::new();
        devices.attach(native_io).unwrap();
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let scope = super::scope();
        let (io, port) = crypto::pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        io.close_submissions().unwrap();
        let mut service = {
            let _environment = sim.enter();
            WithNative::new(PageCryptoEngine::new(CryptoRuntime { port }), native_port)
        };
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        service.poll_budgeted(4).unwrap();
        service.poll_budgeted(1).unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(_))
        ));
        drop(activation);
        sim.take_trace();
        let mut drain = service.drain(&scope);
        for slot in 0..4 {
            let result = drain.as_mut().poll(&mut cx);
            if slot < 3 {
                assert!(result.is_pending());
                assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
            } else {
                assert_eq!(result, Poll::Ready(Ok(())));
            }
            assert_eq!(
                sim.take_trace()
                    .iter()
                    .filter(|event| event.operation == NativeOp::Stop)
                    .count(),
                1
            );
        }
        drop(drain);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn cancellation_between_activation_turns_preserves_partial_owners_until_fenced() {
        let (sim, devices, mut native, admission, scope) = fixture(4);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(2).unwrap();
        scope.cancel().unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(activation);
        assert_eq!(admission.used(ResourceClass::Registered), 4 * 8192);
        native.poll_budgeted(1).unwrap();
        assert_eq!(admission.used(ResourceClass::Registered), 8192);
        assert!(!native.drained());
        native.poll_budgeted(1).unwrap();
        assert!(native.drained());
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }
}
use crate::{
    http::{Header, MessageHead, StartLine},
    model::*,
    security::connection::{Signatures, VerifiedHead},
};
use std::{
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::Duration,
};

pub(super) struct Charge(Arc<AtomicUsize>);
impl Drop for Charge {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}
pub(super) struct Observer(Arc<AtomicUsize>);
impl Observer {
    pub fn get(&self) -> usize {
        self.0.load(Ordering::Acquire)
    }
}
pub(super) fn fixture(
    slots: usize,
) -> (
    simulation::Simulation,
    Rc<IoPort>,
    NativeService,
    Vec<Observer>,
) {
    let sim = simulation::Simulation::new()
        .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
        .unwrap();
    let (io, port) = pair(slots).unwrap();
    let io = Rc::new(io);
    let mut native = {
        let _scope = sim.enter();
        NativeService::new(port)
    };
    let mut observers = Vec::new();
    let guards = (0..slots)
        .map(|_| {
            let count = Arc::new(AtomicUsize::new(1));
            observers.push(Observer(count.clone()));
            Arc::new(Charge(count)) as rdma_verbs::Guard
        })
        .collect();
    futures::executor::block_on(io.configure(rdma_verbs::Configuration {
        discover: true,
        guards,
        bytes: 32,
        selector: Box::new(|ports| {
            assert_eq!(ports[0].device, "sim0");
            Ok(vec![(0, 0)])
        }),
    }))
    .unwrap();
    for _ in 0..=slots {
        native.poll_budgeted(1).unwrap();
    }
    io.activation().unwrap().unwrap();
    (sim, io, native, observers)
}
pub(super) fn immediate<T>(poll: Poll<rdma_verbs::Result<T>>) -> Result<T> {
    match poll {
        Poll::Ready(result) => result.map_err(Into::into),
        Poll::Pending => Err(Error::Overloaded),
    }
}
pub(super) fn claim(io: &Rc<IoPort>) -> Rc<QueuePairHandle> {
    immediate(QueuePairHandle::poll_new(io.device(0))).unwrap()
}
pub(super) fn connect_pair(a: &QueuePairHandle, b: &QueuePairHandle, native: &mut NativeService) {
    immediate(a.poll_connect(b.endpoint)).unwrap();
    immediate(b.poll_connect(a.endpoint)).unwrap();
    assert!(!a.ready() && !b.ready());
    native.poll_budgeted(256).unwrap();
    a.progress().unwrap();
    b.progress().unwrap();
    assert!(a.ready() && b.ready());
}
pub(super) fn mark_connected(qp: &QueuePairHandle, native: &mut NativeService) {
    immediate(qp.poll_connect(qp.endpoint)).unwrap();
    assert!(!qp.ready(), "connect is not executed on I/O");
    native.poll_budgeted(256).unwrap();
    qp.progress().unwrap();
    assert!(qp.ready());
}
pub(super) fn scope() -> RequestScope {
    RequestScope::new(
        RequestId([1; 16]),
        environment::now() + Duration::from_secs(10),
    )
    .unwrap()
}
pub(super) fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
    operation
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
pub(super) fn done<T>(operation: &mut Operation<'_, T>) -> T {
    match poll(operation) {
        Poll::Ready(Ok(value)) => value,
        Poll::Ready(Err(e)) => panic!("unexpected error: {e:?}"),
        Poll::Pending => panic!("unexpected pending"),
    }
}
pub(super) fn verified(signers: &[Rc<Signatures>], mut headers: Vec<Header>) -> VerifiedHead {
    headers.push(Header {
        name: "racer-receiver".into(),
        value: signers[1].node().0.as_bytes().to_vec(),
    });
    signers[1]
        .verify_proof(
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
pub(super) fn header(name: &str, value: Vec<u8>) -> Header {
    Header {
        name: name.into(),
        value,
    }
}
pub(super) fn envelope() -> PageEnvelope {
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
use uring_runtime::environment;

mod lifecycle_tests {
    use super::*;
    use crate::{
        runtime::admission::AdmissionPolicy,
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
        failed.expire_at(uring_runtime::environment::now());
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
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(true).limits,
        ));
        let scope = scope();
        let mut operation = devices.activate(Vec::new(), &admission, 4096, &scope);
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
}
