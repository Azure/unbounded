//! Discovery cleanup must keep failed closes charged, even without selected slots.
#![cfg(feature = "simulation")]

use rdma_verbs::{
    Configuration, Error, Guard, NativeService, pair,
    simulation::{Device, Fault, Operation, Simulation},
};
use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};

/// Observe the final release without holding the admission guard itself.
struct Charge(Arc<AtomicUsize>);

impl Drop for Charge {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

/// Run one activation and check cleanup, reopen, and final guard ownership.
fn cleanup(
    plan: rdma_verbs::Result<Vec<(u32, usize)>>,
    expected: rdma_verbs::Result<usize>,
    reject_close: bool,
) {
    let sim = Simulation::new()
        .with_devices(vec![
            Device::new("unselected-first", [1; 16]),
            Device::new("selected", [2; 16]),
            Device::new("unselected-last", [3; 16]),
        ])
        .unwrap();
    // More discovered devices than guards exercises shared admission ownership.
    let (io, port) = pair(1).unwrap();
    let mut native = {
        let _scope = sim.enter();
        NativeService::new(port)
    };
    let charges = Arc::new(AtomicUsize::new(1));
    futures::executor::block_on(io.configure(Configuration {
        discover: true,
        guards: vec![Arc::new(Charge(charges.clone())) as Guard],
        bytes: 32,
        selector: Box::new(move |ports| {
            assert_eq!(ports.len(), 3);
            assert_eq!(ports[1].device, "selected");
            plan
        }),
    }))
    .unwrap();
    if reject_close {
        // The first close is always an unselected device, never a slot device.
        sim.fault(Operation::Close, Fault::Reject);
    }
    for _ in 0..2 {
        native.poll_budgeted(1).unwrap();
    }
    assert_eq!(io.activation().unwrap().map(|ports| ports.len()), expected);
    assert_eq!(sim.pending_faults(), 0);
    io.close();
    for _ in 0..3 {
        native.poll_budgeted(1).unwrap();
    }
    assert!(native.drained(), "service-owned teardown finished");
    assert_eq!(sim.live_resources(), usize::from(reject_close));
    assert_eq!(charges.load(Ordering::Acquire), usize::from(reject_close));
    if reject_close {
        for _ in 0..3 {
            assert_eq!(io.reopen(), Err(Error::Overloaded));
            native.poll_budgeted(1).unwrap();
        }
    } else {
        io.reopen().unwrap();
    }
    drop(native);
    drop(io);
    assert_eq!(charges.load(Ordering::Acquire), usize::from(reject_close));
}

/// A failed unselected close must block reuse after a successful activation.
#[test]
fn unselected_device_close_failure_stays_charged() {
    cleanup(Ok(vec![(7, 1)]), Ok(1), true);
}

/// An empty plan still owns every device opened to build the selector's input.
#[test]
fn empty_selection_close_failure_stays_charged() {
    cleanup(Ok(Vec::new()), Ok(0), true);
}

/// Selector errors and every plan-validation exit retain failed device owners.
#[test]
fn failed_selection_close_failure_stays_charged() {
    for reject_close in [false, true] {
        cleanup(Err(Error::Io), Err(Error::Io), reject_close);
        for plan in [vec![(7, 3)], vec![(7, 0), (7, 1)], vec![(7, 0), (8, 0)]] {
            cleanup(Ok(plan), Err(Error::InvalidConfiguration), reject_close);
        }
        cleanup(
            Ok(vec![(7, 0), (8, 1)]),
            Err(Error::Overloaded),
            reject_close,
        );
    }
}

/// Successful closes release all charges and permit reuse for selected or empty plans.
#[test]
fn successful_discovery_cleanup_releases_charges() {
    cleanup(Ok(vec![(7, 1)]), Ok(1), false);
    cleanup(Ok(Vec::new()), Ok(0), false);
}
