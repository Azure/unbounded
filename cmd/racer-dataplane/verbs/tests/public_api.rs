#![cfg(feature = "simulation")]

use rdma_verbs::{
    Configuration, Error, Guard, IoPort, NativeService, QueuePair, Region, pair, simulation,
};
use std::{
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll},
};

struct Charge(Arc<AtomicUsize>);
impl Drop for Charge {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

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

fn ready<T>(poll: Poll<rdma_verbs::Result<T>>) -> T {
    match poll {
        Poll::Ready(Ok(value)) => value,
        Poll::Ready(Err(error)) => panic!("unexpected error: {error}"),
        Poll::Pending => panic!("unexpected contention"),
    }
}

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
                QueuePair::poll_new(io.device(7)),
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
                QueuePair::poll_new(stale.clone()),
                Poll::Ready(Err(Error::Unavailable))
            ));
        }
        let second = io.device(7);
        let first = io.device(u32::MAX);
        let a = ready(QueuePair::poll_new(second.clone()));
        let b = ready(QueuePair::poll_new(second.clone()));
        assert_eq!((a.endpoint.gid, a.endpoint.port), ([2; 16], 2));
        assert_eq!((b.endpoint.gid, b.endpoint.port), ([2; 16], 2));
        assert_ne!(a.endpoint.qpn, b.endpoint.qpn);
        for device in [second.clone(), io.device(99)] {
            assert!(matches!(
                QueuePair::poll_new(device),
                Poll::Ready(Err(Error::Overloaded))
            ));
        }
        // Exhausting one tag must not consume another tag's remaining slot.
        let c = ready(QueuePair::poll_new(first.clone()));
        assert_eq!((c.endpoint.gid, c.endpoint.port), ([1; 16], 1));
        assert!(matches!(
            QueuePair::poll_new(first.clone()),
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
            QueuePair::poll_new(io.device(0)),
            Poll::Ready(Err(Error::Overloaded))
        ));
        io.close();
        native.poll_budgeted(2).unwrap();
        io.reopen().unwrap();
    }
}

#[test]
fn configured_guards_survive_failed_fence_and_last_io_lease() {
    let (sim, io, mut native) = fixture(1);
    let count = Arc::new(AtomicUsize::new(1));
    configure(&io, vec![Arc::new(Charge(count.clone()))]);
    native.poll_budgeted(1).unwrap();
    native.poll_budgeted(1).unwrap();
    io.activation().unwrap().unwrap();
    let Poll::Ready(Ok(qp)) = QueuePair::poll_new(io.device(u32::MAX)) else {
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

#[test]
fn public_ports_copy_only_after_terminal_fence() {
    let (sim, io, mut native) = fixture(2);
    configure(&io, vec![Arc::new(()), Arc::new(())]);
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(1).unwrap();
    io.activation().unwrap().unwrap();
    let Poll::Ready(Ok(sender)) = QueuePair::poll_new(io.device(u32::MAX)) else {
        panic!("sender")
    };
    let Poll::Ready(Ok(receiver)) = QueuePair::poll_new(io.device(u32::MAX)) else {
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
    receiver.stop().unwrap();
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
