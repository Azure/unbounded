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
    assert_eq!(sender.poll_connect(receiver.endpoint), Poll::Ready(Ok(())));
    assert_eq!(receiver.poll_connect(sender.endpoint), Poll::Ready(Ok(())));
    native.poll_budgeted(2).unwrap();
    sender.progress().unwrap();
    receiver.progress().unwrap();
    let Poll::Ready(Ok(target)) = Region::poll_acquire(&receiver, 17) else {
        panic!("target")
    };
    let Poll::Ready(Ok((window, bind))) = receiver.poll_bind(target.clone()) else {
        panic!("bind")
    };
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(bind.result(), Some(Ok(())));
    let Poll::Ready(Ok(source)) = Region::poll_acquire(&sender, 17) else {
        panic!("source")
    };
    assert_eq!(source.poll_copy_from(&[0xa5; 17]), Poll::Ready(Ok(())));
    let Poll::Ready(Ok(write)) = sender.poll_write(source.clone(), window.address(), window.key())
    else {
        panic!("write")
    };
    native.poll_budgeted(2).unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(write.result(), Some(Ok(())));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert_eq!(
        target.poll_copy_to(&mut cx),
        Poll::Ready(Err(Error::Unavailable))
    );
    receiver.stop().unwrap();
    native.poll_budgeted(2).unwrap();
    assert_eq!(
        target.poll_copy_to(&mut cx),
        Poll::Ready(Ok(vec![0xa5; 17]))
    );
    drop((source, target, window, bind, write, receiver, sender));
    io.close();
    native.poll_budgeted(2).unwrap();
    assert!(native.drained());
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
