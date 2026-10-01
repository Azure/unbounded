//! Configured production activation through the simulated native ABI, not injected slots.
use super::*;
use crate::{
    memory::pool::BufferPool,
    model::{ResourceClass, *},
    rdma::Devices,
    runtime::{admission::Admission, crypto, worker::CryptoRuntime},
    security::{aead::PageCryptoEngine, identity::KeyPurpose},
};
use simulation::{Fault, Operation as NativeOp};
use std::task::{Context, Poll};

fn fixture(
    slots: usize,
) -> (
    simulation::Simulation,
    Devices,
    NativeService,
    Admission,
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
    let admission = Admission::new(crate::test_support::cluster::config(true).limits);
    let scope = RequestScope::new(
        RequestId([1; 16]),
        crate::runtime::environment::now() + std::time::Duration::from_secs(30),
    )
    .unwrap();
    (sim, devices, native, admission, scope)
}

fn activate<'a>(
    devices: &'a Devices,
    admission: &'a Admission,
    scope: &'a RequestScope,
) -> crate::error::Operation<'a, Vec<RailMapping>> {
    devices.activate(
        vec![RailMapping {
            rail: RailId(0),
            fabric: "sim".into(),
            numa_node: None,
        }],
        vec![FabricPort {
            fabric: "sim".into(),
            device: "sim0".into(),
            port: 1,
            gid: None,
        }],
        admission,
        4096,
        scope,
    )
}

#[test]
fn configured_activation_spends_budget_and_yields_to_sibling_page_jobs() {
    let (sim, devices, mut native, admission, scope) = fixture(4);
    let shared = native.port.shared.clone();
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

    // Real page jobs on a sibling engine, polled on this same thread between
    // native turns just as the shared executor does. No synthetic progress counter.
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
    let sibling_admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
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
                    scope.clone(),
                )
            )
            .is_ok()
        );
        native.poll_budgeted(1).unwrap();
        let trace = sim.take_trace();
        assert_eq!(
            trace
                .iter()
                .filter(|event| event.operation == NativeOp::Register)
                .count(),
            usize::from(turn != 0)
        );
        assert_eq!(native.resources.iter().flatten().count(), turn as usize);
        if turn < 4 {
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            assert!(
                shared
                    .slots
                    .iter()
                    .all(|slot| slot.state.load(Ordering::Acquire) == IDLE)
            );
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
        assert_eq!(native.resources.iter().flatten().count(), remaining);
    }
    assert_eq!(admission.used(ResourceClass::Registered), 0);
    assert_eq!(sim.live_resources(), 0);
}

#[test]
fn partial_activation_errors_fence_before_retry_without_publishing_readiness() {
    for failure in [NativeOp::Register, NativeOp::Qp, NativeOp::Window] {
        let (sim, devices, mut native, admission, scope) = fixture(3);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut activation = activate(&devices, &admission, &scope);
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(2).unwrap(); // Discovery plus exactly one slot.
        assert_eq!(native.resources.iter().flatten().count(), 1);
        sim.fault(failure, Fault::Reject);
        native.poll_budgeted(1).unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Unavailable))
        ));
        drop(activation);
        assert!(!devices.ready(RailId(0)));
        assert!(
            native
                .port
                .shared
                .slots
                .iter()
                .all(|slot| slot.state.load(Ordering::Acquire) == IDLE)
        );
        sim.fault(NativeOp::Stop, Fault::Reject);
        native.poll_budgeted(1).unwrap();
        assert!(native.resources[0].is_some());
        assert!(!native.port.shared.drained.load(Ordering::Acquire));
        assert!(admission.used(ResourceClass::Registered) >= 8192);
        let mut retry = activate(&devices, &admission, &scope);
        assert!(matches!(
            retry.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(retry);
        native.resources[0].as_mut().unwrap().next_retry = None;
        native.poll_budgeted(3).unwrap();
        assert!(native.port.shared.drained.load(Ordering::Acquire));
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
        assert!(native.port.shared.drained.load(Ordering::Acquire));
        assert!(!devices.ready(RailId(0)));
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }
}

#[test]
fn activation_mailbox_contention_consumes_turn_without_losing_quota() {
    let (sim, devices, mut native, admission, scope) = fixture(2);
    let shared = native.port.shared.clone();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut activation = activate(&devices, &admission, &scope);
    assert!(activation.as_mut().poll(&mut cx).is_pending());
    native.poll_budgeted(1).unwrap();
    sim.take_trace();
    let guard = shared.slots[0].mailbox.lock().unwrap();
    native.poll_budgeted(1).unwrap();
    assert!(sim.trace().is_empty());
    assert_eq!(admission.used(ResourceClass::Registered), 2 * 8192);
    drop(guard);
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
    let (sim, devices, native, admission, scope) = fixture(4);
    let (io, port) = crypto::pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
    io.close_submissions().unwrap();
    let mut service = WithNative {
        inner: PageCryptoEngine::new(CryptoRuntime { port }),
        native,
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
    native.poll_budgeted(1).unwrap(); // Drop only the unprovisioned configuration.
    assert_eq!(admission.used(ResourceClass::Registered), 8192);
    assert!(!native.drained());
    native.poll_budgeted(1).unwrap();
    assert!(native.drained());
    assert_eq!(admission.used(ResourceClass::Registered), 0);
    assert_eq!(sim.live_resources(), 0);
}
