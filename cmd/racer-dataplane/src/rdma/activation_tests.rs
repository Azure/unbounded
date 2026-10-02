//! Production activation through the public simulated fabric.
use super::{lifecycle::*, *};
use crate::{
    model::*,
    runtime::{
        admission::AdmissionPolicy,
        crypto,
        worker::{CryptoRuntime, CryptoService},
    },
    security::{aead::PageCryptoEngine, identity::KeyPurpose},
};
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
        crate::runtime::environment::now() + std::time::Duration::from_secs(30),
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
    let inventory = super::discovery::inventory();
    let publication: Vec<_> = inventory
        .iter()
        .cloned()
        .map(|mut n| {
            n.rail = RailId(7);
            n
        })
        .collect();
    let selected = super::discovery::select_worker(&publication, &inventory, 1, None, 2);
    assert_eq!(selected.len(), 1);
    assert_eq!(selected[0].device, "b");
    let admission = flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(true).limits,
    ));
    let scope = RequestScope::new(
        RequestId([3; 16]),
        crate::runtime::environment::now() + std::time::Duration::from_secs(30),
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
    let keys = crate::security::identity::keyring_tests::keys();
    let page = PageId {
        version: ObjectVersion {
            object: ObjectId {
                cache: CacheId(crate::security::identity::tests::CACHE.into()),
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
    let scope = super::test_support::scope();
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
