//! Public contracts for tagged activation, ownership, scoped waits, and fencing.
use rdma_verbs::{Configuration, Error, Guard, IoPort, NativeService, pair};

#[cfg(feature = "simulation")]
use rdma_verbs::{QueuePairHandle, Region, simulation};

use std::{
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll},
};

/// Lifetime charge whose final drop is observable without retaining the charge.
struct Charge(Arc<AtomicUsize>);

impl Drop for Charge {
    /// Record final guard release without extending the guard's lifetime.
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

/// Create a simulated native service while retaining its I/O port and fabric.
#[cfg(feature = "simulation")]
fn fixture(slots: usize) -> (simulation::Simulation, Rc<IoPort>, NativeService) {
    let sim = simulation::Simulation::new()
        .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
        .unwrap();
    let (io, port) = pair(slots).unwrap();
    let native = {
        let _scope = sim.enter();
        NativeService::new(port)
    };
    (sim, Rc::new(io), native)
}

/// Submit the shared one-device configuration without advancing native progress.
#[cfg(feature = "simulation")]
fn configure(io: &IoPort, guards: Vec<Guard>) {
    futures::executor::block_on(io.configure(Configuration {
        discover: true,
        guards,
        bytes: 32,
        selector: Box::new(|ports| {
            assert_eq!(ports[0].device, "sim0");
            Ok(vec![(u32::MAX, 0)])
        }),
    }))
    .unwrap();
}

/// Require success in a fixture that deliberately has no mailbox contention.
#[cfg(feature = "simulation")]
fn ready<T>(poll: Poll<rdma_verbs::Result<T>>) -> T {
    match poll {
        Poll::Ready(Ok(value)) => value,
        Poll::Ready(Err(error)) => panic!("unexpected error: {error}"),
        Poll::Pending => panic!("unexpected contention"),
    }
}

/// Tagged claims stay bounded and reopening rejects all previous-generation handles.
#[cfg(feature = "simulation")]
#[test]
fn selected_ports_route_bounded_claims_and_reopen_revokes_old_devices() {
    let sim = simulation::Simulation::new()
        .with_devices(vec![
            simulation::Device::new("first", [1; 16]),
            simulation::Device {
                port: 2,
                numa_node: Some(3),
                ..simulation::Device::new("second", [2; 16])
            },
        ])
        .unwrap();
    let (io, port) = pair(3).unwrap();
    let io = Rc::new(io);
    let mut native = {
        let _scope = sim.enter();
        let inventory = rdma_verbs::inventory().unwrap();
        assert_eq!(inventory.len(), 2);
        assert_eq!(inventory[1].device, "second");
        assert_eq!(inventory[1].port, 2);
        assert_eq!(inventory[1].gid, [2; 16]);
        assert_eq!(sim.live_resources(), 0, "inventory owns no lasting handles");
        NativeService::new(port)
    };
    let mut stale_devices: Vec<Rc<rdma_verbs::DeviceHandle>> = Vec::new();
    for _ in 0..2 {
        let charges = Arc::new(AtomicUsize::new(3));
        futures::executor::block_on(
            io.configure(Configuration {
                discover: true,
                guards: (0..3)
                    .map(|_| Arc::new(Charge(charges.clone())) as Guard)
                    .collect(),
                bytes: 32,
                selector: Box::new(|ports| {
                    assert_eq!(ports[1].numa_node, Some(3));
                    // Caller tags and selection order are unrelated to discovery order.
                    Ok(vec![(7, 1), (u32::MAX, 0)])
                }),
            }),
        )
        .unwrap();
        assert_eq!(io.capacity(), 3);
        native.poll_budgeted(0).unwrap();
        assert!(io.activation().is_none());
        for _ in 0..3 {
            native.poll_budgeted(1).unwrap();
            assert!(io.activation().is_none());
            assert!(matches!(
                QueuePairHandle::poll_new(io.device(7), None),
                Poll::Ready(Err(Error::Overloaded))
            ));
        }
        native.poll_budgeted(1).unwrap();
        let selected = io.activation().unwrap().unwrap();
        assert_eq!(selected.len(), 2);
        assert_eq!((selected[0].tag, selected[0].index), (7, 1));
        assert_eq!(selected[0].port.device, "second");
        assert_eq!((selected[1].tag, selected[1].index), (u32::MAX, 0));
        assert_eq!(selected[1].port.device, "first");
        assert!(io.activation().is_none(), "activation is consumed once");
        for stale in &stale_devices {
            assert!(matches!(
                QueuePairHandle::poll_new(stale.clone(), None),
                Poll::Ready(Err(Error::Unavailable))
            ));
        }
        let second = io.device(7);
        let first = io.device(u32::MAX);
        let a = ready(QueuePairHandle::poll_new(second.clone(), None));
        let b = ready(QueuePairHandle::poll_new(second.clone(), None));
        assert_eq!((a.endpoint.gid, a.endpoint.port), ([2; 16], 2));
        assert_eq!((b.endpoint.gid, b.endpoint.port), ([2; 16], 2));
        assert_ne!(a.endpoint.qpn, b.endpoint.qpn);
        for device in [second.clone(), io.device(99)] {
            assert!(matches!(
                QueuePairHandle::poll_new(device, None),
                Poll::Ready(Err(Error::Overloaded))
            ));
        }
        // Exhausting one tag must not consume another tag's remaining slot.
        let c = ready(QueuePairHandle::poll_new(first.clone(), None));
        assert_eq!((c.endpoint.gid, c.endpoint.port), ([1; 16], 1));
        assert!(matches!(
            QueuePairHandle::poll_new(first.clone(), None),
            Poll::Ready(Err(Error::Overloaded))
        ));
        stale_devices.extend([first, second]);
        io.close();
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        native.poll_budgeted(3).unwrap();
        assert!(native.drained());
        assert!(a.stopped() && b.stopped() && c.stopped());
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(charges.load(Ordering::Acquire), 3);
        drop((a, b, c));
        native.poll_budgeted(3).unwrap();
        assert_eq!(charges.load(Ordering::Acquire), 0);
        assert_eq!(sim.live_resources(), 0);
        io.reopen().unwrap();
        assert!(!io.closed());
    }
}

/// Caller-held guard clones do not masquerade as retained native quarantine owners.
#[cfg(feature = "simulation")]
#[test]
fn caller_arc_clones_do_not_block_quarantine_drain_and_reopen() {
    let (sim, io, mut native) = fixture(1);
    let count = Arc::new(AtomicUsize::new(1));
    let caller: Guard = Arc::new(Charge(count.clone()));
    configure(&io, vec![caller.clone()]);
    assert!(io.activation().is_none());
    native.poll_budgeted(1).unwrap();
    assert!(io.activation().is_none());
    native.poll_budgeted(1).unwrap();
    assert_eq!(io.activation().unwrap().unwrap()[0].tag, u32::MAX);
    io.close();
    native.poll_budgeted(1).unwrap();
    assert!(native.drained());
    assert_eq!(sim.live_resources(), 0);
    io.reopen().unwrap();
    assert_eq!(count.load(Ordering::Acquire), 1);
    drop(caller);
    assert_eq!(count.load(Ordering::Acquire), 0);
}

/// Selection executes during native progress and rejects invalid tag/index plans.
#[cfg(feature = "simulation")]
#[test]
fn selectors_run_on_native_role_and_invalid_plans_fail_closed() {
    for plan in [vec![(0, 1)], vec![(0, 0), (0, 0)], vec![(0, 0), (1, 0)]] {
        let (_, io, mut native) = fixture(2);
        let calls = Arc::new(AtomicUsize::new(0));
        let observed = calls.clone();
        futures::executor::block_on(io.configure(Configuration {
            discover: true,
            guards: vec![Arc::new(()), Arc::new(())],
            bytes: 32,
            selector: Box::new(move |_| {
                observed.fetch_add(1, Ordering::AcqRel);
                Ok(plan)
            }),
        }))
        .unwrap();
        assert_eq!(calls.load(Ordering::Acquire), 0);
        native.poll_budgeted(1).unwrap();
        assert_eq!(calls.load(Ordering::Acquire), 1);
        assert_eq!(io.activation(), Some(Err(Error::InvalidConfiguration)));
        assert!(matches!(
            QueuePairHandle::poll_new(io.device(0), None),
            Poll::Ready(Err(Error::Overloaded))
        ));
        io.close();
        native.poll_budgeted(2).unwrap();
        io.reopen().unwrap();
    }
}

/// Failed fencing and retained staging keep the configured guard alive.
#[cfg(feature = "simulation")]
#[test]
fn configured_guards_survive_failed_fence_and_last_io_lease() {
    let (sim, io, mut native) = fixture(1);
    let count = Arc::new(AtomicUsize::new(1));
    configure(&io, vec![Arc::new(Charge(count.clone()))]);
    native.poll_budgeted(1).unwrap();
    native.poll_budgeted(1).unwrap();
    io.activation().unwrap().unwrap();
    let Poll::Ready(Ok(qp)) = QueuePairHandle::poll_new(io.device(u32::MAX), None) else {
        panic!("claim")
    };
    let Poll::Ready(Ok(region)) = Region::poll_acquire(&qp, 17) else {
        panic!("region")
    };
    io.close();
    sim.fault(simulation::Operation::Stop, simulation::Fault::Reject);
    native.poll_budgeted(1).unwrap();
    assert!(!qp.stopped());
    assert_eq!(io.reopen(), Err(Error::Overloaded));
    assert_eq!(count.load(Ordering::Acquire), 1);
    drop(qp);
    drop(native);
    assert_eq!(
        count.load(Ordering::Acquire),
        1,
        "I/O staging still owns its guard"
    );
    drop(region);
    drop(io);
    assert_eq!(count.load(Ordering::Acquire), 0);
    assert_eq!(sim.live_resources(), 0);
}

/// Connected DMA preserves payloads and only exposes readback after a terminal fence.
#[cfg(feature = "simulation")]
#[test]
fn public_ports_copy_only_after_terminal_fence() {
    let (sim, io, mut native) = fixture(2);
    configure(&io, vec![Arc::new(()), Arc::new(())]);
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(1).unwrap();
    io.activation().unwrap().unwrap();
    let Poll::Ready(Ok(sender)) = QueuePairHandle::poll_new(io.device(u32::MAX), None) else {
        panic!("sender")
    };
    let Poll::Ready(Ok(receiver)) = QueuePairHandle::poll_new(io.device(u32::MAX), None) else {
        panic!("receiver")
    };
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for remote in [
        rdma_verbs::Endpoint {
            qpn: 0,
            ..receiver.endpoint
        },
        rdma_verbs::Endpoint {
            link_layer: 1,
            ..receiver.endpoint
        },
    ] {
        assert_eq!(
            sender.poll_connect(remote),
            Poll::Ready(Err(Error::InvalidRequest))
        );
    }
    assert_eq!(sender.poll_connect(receiver.endpoint), Poll::Ready(Ok(())));
    assert_eq!(receiver.poll_connect(sender.endpoint), Poll::Ready(Ok(())));
    assert!(sender.poll_connected(&mut cx).is_pending());
    assert!(!sender.ready());
    native.poll_budgeted(2).unwrap();
    assert_eq!(sender.poll_connected(&mut cx), Poll::Ready(Ok(())));
    assert_eq!(receiver.poll_connected(&mut cx), Poll::Ready(Ok(())));
    assert_eq!(
        sender.poll_connect(receiver.endpoint),
        Poll::Ready(Err(Error::InvalidRequest))
    );
    for (length, error) in [(0, Error::InvalidRange), (33, Error::Overloaded)] {
        assert!(
            matches!(Region::poll_acquire(&receiver, length), Poll::Ready(Err(e)) if e == error)
        );
    }
    let Poll::Ready(Ok(target)) = Region::poll_acquire(&receiver, 17) else {
        panic!("target")
    };
    assert_eq!(target.length(), 17);
    assert!(matches!(
        Region::poll_acquire(&receiver, 17),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(matches!(
        sender.poll_bind(target.clone()),
        Poll::Ready(Err(Error::Unavailable))
    ));
    let Poll::Ready(Ok((window, bind))) = receiver.poll_bind(target.clone()) else {
        panic!("bind")
    };
    assert_eq!((window.address(), window.key()), (0, 0));
    assert!(bind.poll(&mut cx).is_pending());
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(bind.poll(&mut cx), Poll::Ready(Ok(())));
    assert_ne!(window.address(), 0);
    assert_ne!(window.key(), 0);
    let Poll::Ready(Ok(source)) = Region::poll_acquire(&sender, 17) else {
        panic!("source")
    };
    assert_eq!(
        source.poll_copy_from(&[0xa5; 16]),
        Poll::Ready(Err(Error::InvalidRange))
    );
    assert_eq!(source.poll_copy_from(&[0xa5; 17]), Poll::Ready(Ok(())));
    assert!(matches!(
        receiver.poll_write(source.clone(), window.address(), window.key()),
        Poll::Ready(Err(Error::Unavailable))
    ));
    sim.fault(simulation::Operation::Write, simulation::Fault::Delay(1));
    let Poll::Ready(Ok(write)) = sender.poll_write(source.clone(), window.address(), window.key())
    else {
        panic!("write")
    };
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert!(write.poll(&mut cx).is_pending());
    assert!(matches!(
        sender.poll_write(source.clone(), window.address(), window.key()),
        Poll::Ready(Err(Error::Overloaded))
    ));
    native.poll_budgeted(2).unwrap();
    assert_eq!(write.poll(&mut cx), Poll::Ready(Ok(())));
    assert_eq!(
        target.poll_copy_to(&mut cx),
        Poll::Ready(Err(Error::Unavailable))
    );
    let invalidation = ready(receiver.poll_invalidate(window.clone(), &mut cx));
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(invalidation.poll(&mut cx), Poll::Ready(Ok(())));
    assert_eq!(
        target.poll_copy_to(&mut cx),
        Poll::Ready(Err(Error::Unavailable))
    );
    // The old capability is rejected without overwriting the completed payload.
    ready(source.poll_copy_from(&[0xff; 17]));
    let stale = ready(sender.poll_write(source.clone(), window.address(), window.key()));
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(stale.poll(&mut cx), Poll::Ready(Err(Error::Io)));
    assert_eq!(
        write.result(),
        Some(Ok(())),
        "earlier tickets keep their result"
    );
    receiver.stop();
    assert!(receiver.poll_stopped(&mut cx).is_pending());
    native.poll_budgeted(2).unwrap();
    assert_eq!(receiver.poll_stopped(&mut cx), Poll::Ready(Ok(())));
    assert_eq!(
        target.poll_copy_to(&mut cx),
        Poll::Ready(Ok(vec![0xa5; 17]))
    );
    drop((
        source,
        target,
        window,
        bind,
        write,
        invalidation,
        stale,
        receiver,
        sender,
    ));
    io.close();
    native.poll_budgeted(2).unwrap();
    assert!(native.drained());
    assert_eq!(sim.pending_faults(), 0);
    assert_eq!(sim.live_resources(), 0);
}

/// Failed QP and probe frees permanently retain the slot charge and forbid reuse.
#[cfg(feature = "simulation")]
#[test]
fn leaked_qp_or_probe_keeps_charge_and_blocks_replacement_and_reopen() {
    for operation in [
        simulation::Operation::QpFree,
        simulation::Operation::WindowFree,
    ] {
        let (sim, io, mut native) = fixture(1);
        let count = Arc::new(AtomicUsize::new(1));
        configure(&io, vec![Arc::new(Charge(count.clone()))]);
        if operation == simulation::Operation::WindowFree {
            sim.fault(operation, simulation::Fault::Reject);
        }
        native.poll_budgeted(1).unwrap();
        native.poll_budgeted(1).unwrap();
        if operation == simulation::Operation::QpFree {
            io.activation().unwrap().unwrap();
            let qp = ready(QueuePairHandle::poll_new(io.device(u32::MAX), None));
            sim.fault(operation, simulation::Fault::Reject);
            drop(qp);
        } else {
            assert_eq!(io.activation(), Some(Err(Error::Io)));
            io.close();
        }
        native.poll_budgeted(1).unwrap();
        let allocated = sim
            .trace()
            .iter()
            .filter(|event| event.operation == simulation::Operation::Qp)
            .count();
        for _ in 0..8 {
            native.poll_budgeted(1).unwrap();
        }
        assert_eq!(
            sim.trace()
                .iter()
                .filter(|event| event.operation == simulation::Operation::Qp)
                .count(),
            allocated
        );
        assert!(matches!(
            QueuePairHandle::poll_new(io.device(u32::MAX), None),
            Poll::Ready(Err(Error::Overloaded | Error::Unavailable))
        ));
        io.close();
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(sim.pending_faults(), 0);
        drop(native);
        drop(io);
        assert_eq!(count.load(Ordering::Acquire), 1, "leak must stay charged");
        assert!(sim.live_resources() > 0);
    }
}

/// Ordinary native destruction releases a lease permit, including under contention.
#[cfg(feature = "simulation")]
#[test]
fn native_drop_releases_permit_only_after_successful_stop() {
    for (fail, contended, transient) in [
        (false, false, false),
        (false, true, false),
        (true, false, false),
        (false, false, true),
        (false, true, true),
    ] {
        let (sim, io, mut native) = fixture(1);
        configure(&io, vec![Arc::new(())]);
        native.poll_budgeted(1).unwrap();
        native.poll_budgeted(1).unwrap();
        io.activation().unwrap().unwrap();
        let count = Arc::new(AtomicUsize::new(1));
        let permit: Guard = Arc::new(Charge(count.clone()));
        let qp = ready(QueuePairHandle::poll_new(io.device(u32::MAX), Some(permit)));
        if fail {
            sim.reject(simulation::Operation::Stop, None, true);
        }
        if transient {
            sim.fault(simulation::Operation::Stop, simulation::Fault::Reject);
        }
        if contended {
            io.with_contention(rdma_verbs::testing::Contention::Slot(0), || drop(native));
        } else {
            drop(native);
        }
        if !fail && !contended && !transient {
            assert!(qp.stopped());
            assert_eq!(count.load(Ordering::Acquire), 0);
        }
        drop(qp);
        drop(io);
        assert_eq!(count.load(Ordering::Acquire), usize::from(fail));
        if !fail {
            assert_eq!(sim.live_resources(), 0);
        }
        assert_eq!(sim.pending_faults(), 0);
    }
}

/// Busy configuration and result locks do not block native polling or lose results.
#[cfg(feature = "simulation")]
#[test]
fn native_poll_retains_activation_across_mailbox_contention() {
    use rdma_verbs::testing::Contention;

    for fail in [false, true] {
        let (io, port) = pair(1).unwrap();
        let mut native = NativeService::new(port);
        futures::executor::block_on(io.configure(Configuration {
            discover: false,
            guards: vec![Arc::new(())],
            bytes: 32,
            selector: Box::new(move |_| {
                if fail {
                    Err(Error::InvalidConfiguration)
                } else {
                    Ok(vec![])
                }
            }),
        }))
        .unwrap();
        io.with_contention(Contention::Configuration, || {
            native.poll_budgeted(1).unwrap();
            assert!(!native.drained());
        });
        assert!(io.activation().is_none());
        io.with_contention(Contention::Activation, || {
            native.poll_budgeted(1).unwrap();
            native.poll_budgeted(1).unwrap();
            assert!(!native.drained());
        });
        native.poll_budgeted(1).unwrap();
        assert_eq!(
            io.activation(),
            Some(if fail {
                Err(Error::InvalidConfiguration)
            } else {
                Ok(vec![])
            })
        );
        native.poll_budgeted(1).unwrap();
        assert!(
            io.activation().is_none(),
            "result is published exactly once"
        );
    }
}

/// An empty plan releases its guards without requiring a native library.
#[test]
fn empty_plan_releases_guards_without_loading_native_adapter() {
    let (io, port) = pair(1).unwrap();
    let mut native = NativeService::new(port);
    let count = Arc::new(AtomicUsize::new(1));
    futures::executor::block_on(io.configure(Configuration {
        discover: false,
        guards: vec![Arc::new(Charge(count.clone()))],
        bytes: 32,
        selector: Box::new(|ports| {
            assert!(ports.is_empty());
            Ok(vec![])
        }),
    }))
    .unwrap();
    assert_eq!(count.load(Ordering::Acquire), 1);
    native.poll_budgeted(1).unwrap();
    assert_eq!(io.activation(), Some(Ok(vec![])));
    assert_eq!(count.load(Ordering::Acquire), 0);
}

/// Ports can cross threads but capacities outside the fixed bound are rejected.
#[test]
fn endpoint_handoff_is_send_and_capacity_is_bounded() {
    /// Check a handoff type's Send bound at compile time.
    fn send<T: Send>() {}

    send::<IoPort>();
    send::<rdma_verbs::NativePort>();
    assert!(pair(0).is_err());
    assert!(pair(257).is_err());
}

/// Scoped activation and service composition through the public crate boundary.
mod scoped {
    use super::*;

    use rdma_verbs::WithNative;

    use std::{
        cell::RefCell,
        task::{Wake, Waker},
    };

    use uring_runtime::{
        Operation, Scope,
        drivers::poll_scoped,
        environment::Cancellation,
        group::{FailureReporter, Service},
    };

    /// Preserve runtime and verbs failures as distinct observable outcomes.
    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum Failure {
        Runtime(uring_runtime::Error),

        Verbs(Error),
    }

    impl From<uring_runtime::Error> for Failure {
        /// Preserve runtime cancellation as a distinct failure source.
        fn from(e: uring_runtime::Error) -> Self {
            Self::Runtime(e)
        }
    }

    impl From<Error> for Failure {
        /// Preserve native-operation errors as a distinct failure source.
        fn from(e: Error) -> Self {
            Self::Verbs(e)
        }
    }

    /// Minimal scope with optional cancellation and no wall-clock dependency.
    #[derive(Clone)]
    struct TestScope(Option<Cancellation>);

    impl Scope for TestScope {
        type Error = Failure;

        /// Reject canceled scopes without introducing a clock dependency.
        fn check(&self) -> Result<(), Failure> {
            if self.0.as_ref().is_some_and(|c| c.is_cancelled()) {
                Err(uring_runtime::Error::Cancelled.into())
            } else {
                Ok(())
            }
        }

        /// Expose the optional cancellation source for waiter registration.
        fn cancellation(&self) -> Option<&Cancellation> {
            self.0.as_ref()
        }
    }

    /// Build an empty plan that avoids native discovery but still owns a guard.
    fn config(guard: Guard) -> Configuration {
        Configuration {
            discover: false,
            selector: Box::new(|ports| {
                assert!(ports.is_empty());
                Ok(vec![])
            }),
            guards: vec![guard],
            bytes: 32,
        }
    }

    /// Count notifications issued to a registered task waker.
    struct Count(AtomicUsize);

    impl Wake for Count {
        /// Count each notification received through the test waker.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    /// Read a lifetime charge count without holding the underlying guard.
    struct Observer(Arc<AtomicUsize>);

    impl Observer {
        /// Return the number of live fixture charges.
        fn get(&self) -> usize {
            self.0.load(Ordering::Acquire)
        }
    }

    /// Create one independently observable charge for cancellation assertions.
    fn guard() -> (Guard, Observer) {
        let count = Arc::new(AtomicUsize::new(1));
        (Arc::new(Charge(count.clone())), Observer(count))
    }

    /// Cancellation wakes the waiter and takes precedence over another operation poll.
    #[test]
    fn scoped_wait_wakes_and_checks_cancellation_before_operation() {
        let scope = TestScope(Some(Cancellation::new().unwrap()));
        let calls = std::cell::Cell::new(0);
        let mut future = Box::pin(poll_scoped(&scope, |_| -> Poll<Result<(), Error>> {
            calls.set(calls.get() + 1);
            Poll::Pending
        }));
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.0.as_ref().unwrap().cancel().unwrap();
        assert_eq!(count.0.load(Ordering::Relaxed), 1);
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Cancelled)))
        );
        assert_eq!(calls.get(), 1);
        assert_eq!(
            futures::executor::block_on(poll_scoped(&TestScope(None), |_| Poll::Ready(
                Err::<(), _>(Error::Io)
            ))),
            Err(Failure::Verbs(Error::Io))
        );
    }

    /// Abandoned activation retains its submitted guard until native progress.
    #[test]
    fn activation_drop_or_cancel_retains_submitted_guard_until_native_turn() {
        for cancel in [false, true] {
            let (io, port) = pair(1).unwrap();
            let mut native = NativeService::new(port);
            let scope = TestScope(Some(Cancellation::new().unwrap()));
            let (guard, observer) = guard();
            let mut future = Box::pin(io.activate(config(guard), &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            if cancel {
                scope.0.as_ref().unwrap().cancel().unwrap();
                assert_eq!(
                    future.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Cancelled)))
                );
            }
            drop(future);
            assert!(io.closed());
            assert_eq!(observer.get(), 1);
            native.poll_budgeted(1).unwrap();
            assert_eq!(observer.get(), 0);
            assert!(native.drained());
        }
    }

    /// Scope-free configuration still supports both empty success and selector failure.
    #[test]
    fn activation_success_and_selector_error_preserve_scope_free_use() {
        for fail in [false, true] {
            let (io, port) = pair(1).unwrap();
            let mut native = NativeService::new(port);
            let scope = TestScope(None);
            let mut config = config(Arc::new(()));
            if fail {
                config.selector = Box::new(|_| Err(Error::InvalidConfiguration));
            }
            let mut future = Box::pin(io.activate(config, &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            let result = future.as_mut().poll(&mut cx);
            if fail {
                assert_eq!(
                    result,
                    Poll::Ready(Err(Failure::Verbs(Error::InvalidConfiguration)))
                );
                assert!(io.closed());
            } else {
                assert_eq!(result, Poll::Ready(Ok(vec![])));
                assert!(!io.closed());
            }
        }
    }

    /// Record hook order and assert admission/fence state from an inner service.
    struct Inner {
        calls: Rc<RefCell<Vec<&'static str>>>,

        io: Rc<IoPort>,

        failure: Option<Failure>,

        wake: Waker,
    }

    impl Service<TestScope> for Inner {
        /// Record lookup and return either the configured waker or failure.
        fn waker(&self) -> Result<Waker, Failure> {
            self.calls.borrow_mut().push("waker");
            self.failure.map_or_else(|| Ok(self.wake.clone()), Err)
        }

        /// Record that driver registration reached the inner service.
        fn register_driver(&self, _: &Waker) {
            self.calls.borrow_mut().push("register");
        }

        /// Record successful startup without allocating native resources.
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            self.calls.borrow_mut().push("start");
            Box::pin(async { Ok(()) })
        }

        /// Assert unchanged budget forwarding and record the polling turn.
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<(), Failure> {
            assert_eq!(budget, 7);
            self.calls.borrow_mut().push("poll");
            Ok(())
        }

        /// Verify native drainage permits reopening before the inner drain runs.
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            assert!(self.io.closed());
            self.io
                .reopen()
                .expect("native fence must precede inner drain");
            self.calls.borrow_mut().push("drain");
            Box::pin(async { Ok(()) })
        }

        /// Assert accepted native work remains available after inner admission stops.
        fn stop_admission(&mut self) -> Result<(), Failure> {
            assert!(
                !self.io.closed(),
                "accepted native work must remain available"
            );
            self.calls.borrow_mut().push("stop");
            self.failure.map_or(Ok(()), Err)
        }

        /// Verify native admission is already closed before returning an inner error.
        fn close(&mut self) -> Result<(), Failure> {
            assert!(self.io.closed());
            self.calls.borrow_mut().push("close");
            self.failure.map_or(Ok(()), Err)
        }

        /// Record fencing only after native admission closes.
        fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            assert!(self.io.closed());
            self.calls.borrow_mut().push("fence");
            Box::pin(async move { self.failure.map_or(Ok(()), Err) })
        }

        /// Record successful forwarding of the shutdown hook.
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
            self.calls.borrow_mut().push("shutdown");
            Box::pin(async { Ok(()) })
        }
    }

    /// Wrapper hooks retain their ordering even when the drain scope is canceled.
    #[test]
    fn wrapper_drives_inner_and_fences_native_before_canceled_scope_drain() {
        let (io, port) = pair(1).unwrap();
        let calls = Rc::new(RefCell::new(vec![]));
        let wake = Waker::from(Arc::new(Count(AtomicUsize::new(0))));
        let mut service = WithNative::new(
            Inner {
                calls: calls.clone(),
                io: Rc::new(io),
                failure: None,
                wake: wake.clone(),
            },
            port,
        );
        let scope = TestScope(Some(Cancellation::new().unwrap()));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(service.waker().unwrap().will_wake(&wake));
        service.register_driver(cx.waker());
        futures::executor::block_on(service.start(&scope)).unwrap();
        service.poll_budgeted(&mut cx, 7).unwrap();
        service.stop_admission().unwrap();
        scope.0.as_ref().unwrap().cancel().unwrap();
        let mut drain = service.drain(&scope);
        assert_eq!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        drop(drain);
        service.close().unwrap();
        futures::executor::block_on(service.fence(&scope)).unwrap();
        futures::executor::block_on(service.shutdown(&scope)).unwrap();
        futures::executor::block_on(service.fence(&scope)).unwrap();
        assert_eq!(
            *calls.borrow(),
            [
                "waker", "register", "start", "poll", "stop", "drain", "close", "fence",
                "shutdown", "fence"
            ]
        );
    }

    /// Inner hook failures do not prevent native admission from being closed.
    #[test]
    fn wrapper_forwards_hook_errors_and_closes_native_on_inner_close_failure() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut service = WithNative::new(
            Inner {
                calls: Rc::default(),
                io: io.clone(),
                failure: Some(Failure::Verbs(Error::Io)),
                wake: Waker::noop().clone(),
            },
            port,
        );
        assert!(matches!(service.waker(), Err(Failure::Verbs(Error::Io))));
        assert_eq!(service.stop_admission(), Err(Failure::Verbs(Error::Io)));
        assert_eq!(service.close(), Err(Failure::Verbs(Error::Io)));
        assert!(io.closed());
        assert_eq!(
            futures::executor::block_on(service.fence(&TestScope(None))),
            Err(Failure::Verbs(Error::Io))
        );
    }

    /// Run a reporting service through a real one-lane group and check its result.
    fn reported_failure(error: Error, name: &str) {
        use uring_runtime::group::{Factory, Group, Lane, Plan};

        /// Inner service that reports the selected failure on its first poll.
        struct Reporting {
            reporter: Option<FailureReporter<Failure>>,

            error: Error,
        }

        impl Service<TestScope> for Reporting {
            /// Store the group reporter forwarded through the wrapper.
            fn set_failure_reporter(&mut self, reporter: FailureReporter<Failure>) {
                self.reporter = Some(reporter);
            }

            /// Start successfully so the test reaches asynchronous reporting.
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
                Box::pin(async { Ok(()) })
            }

            /// Report the configured failure through the installed group reporter.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Failure> {
                self.reporter
                    .as_ref()
                    .expect("forwarded reporter")
                    .report(self.error.into());
                Ok(())
            }

            /// Drain successfully without masking the reported failure.
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
                Box::pin(async { Ok(()) })
            }

            /// Complete shutdown without masking the reported failure.
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, (), Failure> {
                Box::pin(async { Ok(()) })
            }
        }

        /// Build the same wrapper fixture for either reported failure.
        struct Recipe(Error);

        impl Factory<TestScope> for Recipe {
            /// Build a native wrapper around the selected reporting scenario.
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>, Failure> {
                let (_io, port) = pair(1)?;
                Ok(Box::new(WithNative::new(
                    Reporting {
                        reporter: None,
                        error: self.0,
                    },
                    port,
                )))
            }
        }

        let cpu = *uring_runtime::group::affinity::current_cpus()
            .unwrap()
            .first()
            .unwrap();
        let mut group = Group::new(Plan {
            lanes: vec![Lane {
                name: name.into(),
                cpu,
            }],
            helpers: vec![],
            max_threads: 1,
        });
        assert_eq!(
            group.run(&Recipe(error), &TestScope(None)),
            Err(Failure::Verbs(error))
        );
        assert_eq!(group.stats().done, 1);
    }

    /// Cancellation reports travel through the wrapper to the owning group.
    #[test]
    fn wrapper_failure_reporter_reaches_inner_service_and_group() {
        reported_failure(Error::Cancelled, "verbs-forwarding");
    }

    /// Native teardown retries outlive cancellation and precede either inner hook.
    #[cfg(feature = "simulation")]
    #[test]
    fn wrapper_drain_and_fence_retry_native_failure_despite_cancellation() {
        for fence in [false, true] {
            let clock = uring_runtime::environment::SimulationClock::new(17);
            let _clock = clock.environment(0).enter();
            let sim = simulation::Simulation::new()
                .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
                .unwrap();
            let _sim = sim.enter();
            let (io, port) = pair(1).unwrap();
            let io = Rc::new(io);
            let calls = Rc::new(RefCell::new(vec![]));
            let mut service = WithNative::new(
                Inner {
                    calls: calls.clone(),
                    io: io.clone(),
                    failure: None,
                    wake: Waker::noop().clone(),
                },
                port,
            );
            let (guard, observer) = guard();
            futures::executor::block_on(io.configure(Configuration {
                discover: true,
                guards: vec![guard],
                bytes: 32,
                selector: Box::new(|_| Ok(vec![(0, 0)])),
            }))
            .unwrap();
            let mut cx = Context::from_waker(Waker::noop());
            service.poll_budgeted(&mut cx, 7).unwrap();
            service.poll_budgeted(&mut cx, 7).unwrap();
            io.activation().unwrap().unwrap();
            // Public wrapper polling also drives the inner service during setup.
            calls.borrow_mut().clear();
            let scope = TestScope(Some(Cancellation::new().unwrap()));
            scope.0.as_ref().unwrap().cancel().unwrap();
            sim.reject(simulation::Operation::Stop, None, true);
            let mut operation = if fence {
                service.fence(&scope)
            } else {
                service.drain(&scope)
            };
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert!(
                calls.borrow().is_empty(),
                "inner hook must wait for native destruction"
            );
            assert_eq!(observer.get(), 1);
            sim.reject(simulation::Operation::Stop, None, false);
            clock.advance(std::time::Duration::from_millis(10));
            assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            drop(operation);
            assert_eq!(*calls.borrow(), [if fence { "fence" } else { "drain" }]);
            assert_eq!(observer.get(), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    /// Configuration failure reports also propagate through the wrapper and group.
    #[test]
    fn wrapper_forwards_reporter_and_lifecycle_errors_through_group() {
        reported_failure(Error::InvalidConfiguration, "native-wrapper");
    }
}
