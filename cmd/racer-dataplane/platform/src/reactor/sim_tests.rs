//! Adapter tests on the simulated backend: real retry, fence and rejection
//! paths driven by simulated faults instead of scripted completions.
//!
//! Ported from the legacy runtime's simulation-only reactor tests
//! (`gate_callback_*`, `retry_regressions::*`,
//! `simulated_publication_rejection_*`, `budget_scope_*`, and the
//! `ClockPoll` timer tests). The legacy tests that drove private state
//! (`state.next`, `CANCEL_BIT`, injected completions on published entries)
//! assert the same ownership contracts here through public behavior.

use super::tests::{RequestScope, drive, poll, scope};
use super::timer::SleepMode;
use super::*;
use crate::simulation::{EventKind, Fault, Simulation};
use std::{
    rc::Weak,
    task::{Context, Poll, Waker},
};
use uring::backend::OpKind;

/// Counts its drops: one for the buffer, one for the lease.
struct Counted(Vec<u8>, Rc<Cell<usize>>);

// SAFETY: the Vec is never resized, so its heap storage is stable across moves.
unsafe impl IoBuffer for Counted {
    type Error = Error;

    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.0)
    }

    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.0)
    }
}

impl Drop for Counted {
    fn drop(&mut self) {
        self.1.set(self.1.get() + 1);
    }
}

struct Lease(Rc<Cell<usize>>);

impl Drop for Lease {
    fn drop(&mut self) {
        self.0.set(self.0.get() + 1);
    }
}

type SimReactor = Reactor<RequestScope, ()>;

fn submissions(sim: &Simulation, kind: OpKind) -> usize {
    sim.trace()
        .iter()
        .filter(|event| event.kind == EventKind::Submit(kind))
        .count()
}

fn fence(reactor: &SimReactor) {
    drive(reactor, reactor.fence_matching(|_| true)).unwrap();
}

/// Reactors constructed under a simulation run on the simulated backend.
#[test]
fn reactor_selects_the_simulated_backend_at_construction() {
    let sim = Simulation::new();
    let reactor = {
        let _environment = sim.enter();
        SimReactor::new(2, ())
    };
    assert!(reactor.is_simulated());
    reactor.init().unwrap();
    assert!(reactor.core().unwrap().backend().is_simulated());
    let host = SimReactor::new(2, ());
    assert!(!host.is_simulated());
}

/// Observers may reenter the reactor for budget and publication rejections.
#[test]
fn gate_callback_sq_id_and_bookkeeping_rejections_allow_reentry() {
    struct BudgetSwitch(Rc<Cell<bool>>);

    impl Budget for BudgetSwitch {
        type Charge = ();

        fn charge(&self, _: usize) -> Result<()> {
            if self.0.get() {
                Err(Error::Overloaded)
            } else {
                Ok(())
            }
        }
    }
    let sim = Simulation::new();
    let _environment = sim.enter();
    let blocked = Rc::new(Cell::new(false));
    let events = Rc::new(RefCell::new(Vec::new()));
    let sink = events.clone();
    let reactor = Rc::new_cyclic(|weak: &Weak<Reactor<RequestScope, BudgetSwitch>>| {
        let weak = weak.clone();
        Reactor::new(2, BudgetSwitch(blocked.clone())).with_rejection_observer(move |_, facts| {
            let reactor = weak.upgrade().unwrap();
            assert_eq!(reactor.in_flight(), 0);
            reactor.poll_budgeted(1).unwrap();
            sink.borrow_mut().push(facts.reason);
        })
    });
    reactor.init().unwrap();
    let request = scope();
    let (fd, _peer) = sim.socket_pair();
    let fd = Rc::new(fd);
    blocked.set(true);
    assert!(matches!(
        poll(&mut reactor.readiness(fd.clone(), libc::POLLIN as u32, &request)),
        Poll::Ready(Err(Error::Overloaded))
    ));
    blocked.set(false);
    sim.reject_submissions(1).unwrap();
    assert!(matches!(
        poll(&mut reactor.readiness(fd, libc::POLLIN as u32, &request)),
        Poll::Ready(Err(Error::Overloaded))
    ));
    // Identifier exhaustion (`IdExhausted`) can no longer happen: the library
    // reuses generation-tagged slots.
    assert_eq!(
        events.borrow().as_slice(),
        [
            RejectionReason::Bookkeeping,
            RejectionReason::SubmissionQueueFull
        ]
    );
}

/// Retain exact owners through transient errors and never replay progress.
#[test]
fn transient_data_errors_retain_exact_owners_and_return_partial_progress_once() {
    for reserved in [false, true] {
        for errno in [libc::EINTR, libc::EAGAIN] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let reactor = SimReactor::new(1, ());
            let request = scope();
            let (fd, peer) = sim.socket_pair();
            let drops = Rc::new(Cell::new(0));
            let owned = Counted(b"abcdef".to_vec(), drops.clone());
            let ptr = owned.0.as_ptr();
            sim.inject("send", Fault::Errno(errno)).unwrap();
            sim.inject("send", Fault::Short(2)).unwrap();
            let fd = Rc::new(fd);
            let weak = Rc::downgrade(&fd);
            let (bytes, buffer) = if reserved {
                let capacity = reactor.reserve_submissions(1, ()).unwrap();
                let done = drive(
                    &reactor,
                    reactor.send_reserved(fd, owned, capacity.clone(), &request),
                )
                .unwrap();
                assert_eq!(capacity.active(), 0);
                (done.bytes, done.buffer)
            } else {
                let done = drive(
                    &reactor,
                    reactor.send(fd, owned, Lease(drops.clone()), &request),
                )
                .unwrap();
                assert_eq!(drops.get(), 0, "the lease is returned with the result");
                drop(done.lease);
                (done.bytes, done.buffer)
            };
            assert_eq!(bytes, 2);
            assert_eq!(buffer.0.as_ptr(), ptr, "the same allocation is returned");
            assert_eq!(submissions(&sim, OpKind::Send), 2);
            assert_eq!(
                submissions(&sim, OpKind::PollAdd),
                usize::from(errno == libc::EAGAIN)
            );
            assert!(
                weak.upgrade().is_none(),
                "descriptor released after the fence"
            );
            let mut received = [0; 8];
            assert_eq!(peer.try_recv(&mut received).unwrap(), 2);
            assert_eq!(&received[..2], b"ab");
            assert_eq!(
                peer.try_recv(&mut received).unwrap(),
                0,
                "sender closed without replaying partial bytes"
            );
            drop(buffer);
            assert_eq!(drops.get(), usize::from(!reserved) + 1);
        }
    }
}

/// Bound retry attempts and retain readiness owners until the fence.
#[test]
fn retries_are_bounded_and_readiness_does_not_spin() {
    for errno in [libc::EINTR, libc::EAGAIN] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = SimReactor::new(1, ());
        let (fd, _peer) = sim.socket_pair();
        for _ in 0..=DATA_RETRIES {
            sim.inject("send", Fault::Errno(errno)).unwrap();
        }
        let drops = Rc::new(Cell::new(0));
        let result = drive(
            &reactor,
            reactor.send(
                Rc::new(fd),
                Counted(vec![1], drops.clone()),
                Lease(drops.clone()),
                &scope(),
            ),
        );
        assert!(matches!(result, Err(Error::Os(error)) if error == errno));
        assert_eq!(submissions(&sim, OpKind::Send), DATA_RETRIES + 1);
        assert_eq!(
            submissions(&sim, OpKind::PollAdd),
            if errno == libc::EAGAIN {
                DATA_RETRIES
            } else {
                0
            }
        );
        assert_eq!(drops.get(), 2);
        assert_eq!(reactor.in_flight(), 0);
    }
    for abandon in [false, true] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = SimReactor::new(1, ());
        let request = scope();
        let (fd, _peer) = sim.socket_pair();
        sim.inject("recv", Fault::Errno(libc::EAGAIN)).unwrap();
        let drops = Rc::new(Cell::new(0));
        let mut operation = reactor.recv(
            Rc::new(fd),
            Counted(vec![0; 8], drops.clone()),
            Lease(drops.clone()),
            &request,
        );
        assert!(poll(&mut operation).is_pending());
        for _ in 0..20 {
            reactor.poll_budgeted(1).unwrap();
            assert!(poll(&mut operation).is_pending());
        }
        assert_eq!(submissions(&sim, OpKind::Recv), 1);
        assert_eq!(submissions(&sim, OpKind::PollAdd), 1);
        assert_eq!(drops.get(), 0);
        let mut operation = Some(operation);
        if abandon {
            drop(operation.take());
        } else {
            request.cancel();
        }
        assert_eq!(drops.get(), 0, "owners stay until the readiness fence");
        for _ in 0..4 {
            reactor.poll_budgeted(1).unwrap();
        }
        if let Some(mut operation) = operation {
            assert!(matches!(
                poll(&mut operation),
                Poll::Ready(Err(Error::Canceled))
            ));
        }
        assert_eq!(drops.get(), 2);
        assert_eq!(reactor.in_flight(), 0);
    }
}

/// Recheck cancellation after a transient completion before resubmitting.
#[test]
fn cancellation_between_transient_completion_and_retry_prevents_resubmission() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let reactor = SimReactor::new(1, ());
    let request = scope();
    let (fd, _peer) = sim.socket_pair();
    sim.inject("send", Fault::Errno(libc::EINTR)).unwrap();
    let drops = Rc::new(Cell::new(0));
    let mut operation = reactor.send(
        Rc::new(fd),
        Counted(vec![1], drops.clone()),
        Lease(drops.clone()),
        &request,
    );
    assert!(poll(&mut operation).is_pending());
    reactor.poll_budgeted(1).unwrap();
    reactor.poll_budgeted(1).unwrap();
    assert_eq!(drops.get(), 0);
    request.cancel();
    assert!(matches!(
        poll(&mut operation),
        Poll::Ready(Err(Error::Canceled))
    ));
    assert_eq!(submissions(&sim, OpKind::Send), 1);
    assert_eq!(drops.get(), 2);
}

/// Preserve receive and positioned I/O contents and offsets across retries.
#[test]
fn recv_and_positioned_io_retry_without_changing_buffer_or_offset() {
    for operation in ["recv", "read", "write"] {
        for errno in [libc::EINTR, libc::EAGAIN] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let reactor = SimReactor::new(1, ());
            let request = scope();
            let (socket, peer) = sim.socket_pair();
            peer.try_send(b"abcd").unwrap();
            let file = Rc::new(
                sim.open(
                    None,
                    std::path::Path::new("/retry-data"),
                    libc::O_CREAT | libc::O_RDWR,
                )
                .unwrap(),
            );
            let written = drive(
                &reactor,
                reactor.write_at(
                    file.clone(),
                    0,
                    Counted(b"01234567".to_vec(), Rc::default()),
                    (),
                    &request,
                ),
            )
            .unwrap();
            assert_eq!(written.bytes, 8);
            sim.inject(operation, Fault::Errno(errno)).unwrap();
            sim.inject(operation, Fault::Short(2)).unwrap();
            let drops = Rc::new(Cell::new(0));
            let buffer = Counted(b"ABCD".to_vec(), drops.clone());
            let lease = Lease(drops.clone());
            let result = match operation {
                "recv" => drive(
                    &reactor,
                    reactor.recv(Rc::new(socket), buffer, lease, &request),
                ),
                "read" => drive(
                    &reactor,
                    reactor.read_at(file.clone(), 2, buffer, lease, &request),
                ),
                _ => drive(
                    &reactor,
                    reactor.write_at(file.clone(), 2, buffer, lease, &request),
                ),
            }
            .unwrap();
            assert_eq!(result.bytes, 2);
            assert_eq!(drops.get(), 0);
            match operation {
                "recv" => assert_eq!(&result.buffer.0[..2], b"ab"),
                "read" => assert_eq!(&result.buffer.0[..2], b"23"),
                _ => {
                    let read = drive(
                        &reactor,
                        reactor.read_at(
                            file.clone(),
                            0,
                            Counted(vec![0; 8], Rc::default()),
                            (),
                            &request,
                        ),
                    )
                    .unwrap();
                    assert_eq!(&read.buffer.0, b"01AB4567");
                }
            }
            drop(result);
            assert_eq!(drops.get(), 2);
        }
    }
}

/// Reserved accept and receive keep their slot until both completions, in
/// either cancel completion order, whether canceled or abandoned.
#[test]
fn actual_reserved_entries_fence_original_and_cancel_in_both_orders() {
    for accept in [false, true] {
        for cancel_first in [false, true] {
            for abandon in [false, true] {
                let sim = Simulation::new();
                let _environment = sim.enter();
                sim.set_cancel_first(cancel_first);
                let reactor = SimReactor::new(1, ());
                let request = scope();
                let charge = Rc::new(());
                let charge_weak = Rc::downgrade(&charge);
                let capacity = reactor.reserve_submissions(1, charge).unwrap();
                let drops = Rc::new(Cell::new(0));
                let (socket, peer) = sim.socket_pair();
                let listener = sim.listen(SocketAddress::Unix("/fences".into())).unwrap();
                let mut operation: Operation<'_, (), Error> = if accept {
                    let future = reactor.accept_reserved(
                        Rc::new(listener),
                        Some(capacity.clone()),
                        &request,
                    );
                    Box::pin(async move { future.await.map(drop) })
                } else {
                    drop(listener);
                    let future = reactor.recv_reserved(
                        Rc::new(socket),
                        Counted(vec![0; 4], drops.clone()),
                        capacity.clone(),
                        &request,
                    );
                    Box::pin(async move { future.await.map(drop) })
                };
                assert!(poll(&mut operation).is_pending());
                reactor.poll_budgeted(1).unwrap();
                assert_eq!(capacity.active(), 1);
                let mut operation = Some(operation);
                if abandon {
                    drop(operation.take());
                } else {
                    request.cancel();
                }
                assert_eq!(capacity.active(), 1, "held until the fence");
                for _ in 0..4 {
                    reactor.poll_budgeted(1).unwrap();
                }
                assert_eq!(reactor.in_flight(), 0);
                if let Some(mut operation) = operation {
                    assert!(matches!(
                        poll(&mut operation),
                        Poll::Ready(Err(Error::Canceled))
                    ));
                }
                assert_eq!(capacity.active(), 0);
                assert_eq!(drops.get(), usize::from(!accept));
                drop((capacity, peer));
                assert!(charge_weak.upgrade().is_none());
            }
        }
    }
}

/// An abandoned accept whose connection was already accepted closes the new
/// descriptor only after the fence.
#[test]
fn successful_accept_is_reclaimed_only_after_both_cqes_when_abandoned() {
    for cancel_first in [false, true] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        sim.set_cancel_first(cancel_first);
        let reactor = SimReactor::new(1, ());
        let request = scope();
        let capacity = reactor.reserve_submissions(1, ()).unwrap();
        let address = SocketAddress::Unix("/accepted-owner".into());
        let listener = Rc::new(sim.listen(address.clone()).unwrap());
        sim.inject("accept", Fault::HoldCompletion(2)).unwrap();
        let mut accept =
            reactor.accept_reserved(listener.clone(), Some(capacity.clone()), &request);
        assert!(poll(&mut accept).is_pending());
        let client = sim.connect(address).unwrap();
        reactor.poll_budgeted(1).unwrap(); // execute accept, hold its completion
        assert_eq!(sim.live_handles(), 3);
        drop(accept);
        assert_eq!(capacity.active(), 1);
        fence(&reactor);
        assert_eq!(
            sim.live_handles(),
            2,
            "accepted descriptor closed after the fence"
        );
        assert_eq!(capacity.active(), 0);
        drop((client, listener));
        assert_eq!(sim.live_handles(), 0);
    }
}

/// A rejected retry publication drops its buffer and lease once and frees the
/// reserved slot for reuse.
#[test]
fn reserved_retry_publication_rejection_drops_buffer_once_and_reuses_slot() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let reactor = SimReactor::new(1, ());
    let request = scope();
    let capacity = reactor.reserve_submissions(1, ()).unwrap();
    let (socket, peer) = sim.socket_pair();
    let fd = Rc::new(socket);
    let drops = Rc::new(Cell::new(0));
    sim.inject("send", Fault::Errno(libc::EAGAIN)).unwrap();
    let mut operation = reactor.send_reserved(
        fd.clone(),
        Counted(vec![1], drops.clone()),
        capacity.clone(),
        &request,
    );
    assert!(poll(&mut operation).is_pending());
    reactor.poll_budgeted(1).unwrap();
    reactor.poll_budgeted(1).unwrap();
    assert_eq!(capacity.active(), 1);
    assert_eq!(drops.get(), 0);
    sim.reject_submissions(1).unwrap(); // reject readiness, not the original send
    assert!(matches!(
        poll(&mut operation),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert_eq!(drops.get(), 1);
    assert_eq!(capacity.active(), 0);
    assert_eq!(reactor.in_flight(), 0);
    let result = drive(
        &reactor,
        reactor.send_reserved(
            fd,
            Counted(b"ok".to_vec(), Rc::default()),
            capacity.clone(),
            &request,
        ),
    )
    .unwrap();
    assert_eq!(result.bytes, 2);
    assert_eq!(capacity.active(), 0);
    let mut bytes = [0; 8];
    assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
    assert_eq!(&bytes[..2], b"ok");
}

/// Scope destructors may reenter the reactor after completion and rejection.
#[test]
fn scope_destructor_reenters_reactor_after_completion_and_sq_rejection() {
    #[derive(Clone)]
    struct ReenterScope {
        reactor: Rc<RefCell<Weak<Reactor<ReenterScope, ()>>>>,
        drops: Rc<Cell<usize>>,
    }

    impl LibraryScope for ReenterScope {
        type Error = Error;

        fn check(&self) -> Result<()> {
            Ok(())
        }
    }

    impl Drop for ReenterScope {
        fn drop(&mut self) {
            if let Some(reactor) = self.reactor.borrow().upgrade() {
                let _ = reactor.in_flight();
                let _ = reactor.poll_budgeted(1);
            }
            self.drops.set(self.drops.get() + 1);
        }
    }
    let sim = Simulation::new();
    let _environment = sim.enter();
    let reactor = Rc::new(Reactor::new(1, ()));
    let drops = Rc::new(Cell::new(0));
    let scope = ReenterScope {
        reactor: Rc::new(RefCell::new(Rc::downgrade(&reactor))),
        drops: drops.clone(),
    };
    let (fd, _peer) = sim.socket_pair();
    let fd = Rc::new(fd);
    for reject in [true, false] {
        if reject {
            sim.reject_submissions(1).unwrap();
        }
        let mut operation = reactor.send(
            fd.clone(),
            Counted(b"x".to_vec(), Rc::default()),
            (),
            &scope,
        );
        if reject {
            assert!(matches!(
                poll(&mut operation),
                Poll::Ready(Err(Error::Overloaded))
            ));
        } else {
            assert!(matches!(drive(&reactor, operation), Ok(done) if done.bytes == 1));
        }
    }
    let before = drops.get();
    assert!(before >= 1, "retained scope clones were released");
    drop(scope);
    assert_eq!(drops.get(), before + 1);
}

/// Roll back failed publication without retaining owners or admission.
#[test]
fn simulated_publication_rejection_rolls_back_owners_and_admission() {
    #[derive(Clone, Default)]
    struct Meter(Rc<Cell<usize>>);
    struct Charge(Rc<Cell<usize>>, usize);
    impl Drop for Charge {
        fn drop(&mut self) {
            self.0.set(self.0.get() - self.1);
        }
    }
    impl Budget for Meter {
        type Charge = Charge;

        fn charge(&self, bytes: usize) -> Result<Charge> {
            self.0.set(self.0.get() + bytes);
            Ok(Charge(self.0.clone(), bytes))
        }
    }
    let sim = Simulation::new();
    let _environment = sim.enter();
    let meter = Meter::default();
    let reactor = Reactor::<RequestScope, Meter>::new(2, meter.clone());
    reactor.init().unwrap();
    let baseline = meter.0.get();
    let request = scope();
    let (fd, peer) = sim.socket_pair();
    let fd = Rc::new(fd);
    let weak = Rc::downgrade(&fd);
    sim.reject_submissions(1).unwrap();
    assert!(matches!(
        drive(
            &reactor,
            reactor.send(fd, Counted(b"x".to_vec(), Rc::default()), (), &request)
        ),
        Err(Error::Overloaded)
    ));
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(reactor.ordinary.get(), 0);
    assert!(weak.upgrade().is_none());
    assert_eq!(meter.0.get(), baseline);
    let (fd, _) = sim.socket_pair();
    assert!(matches!(
        drive(
            &reactor,
            reactor.send(
                Rc::new(fd),
                Counted(b"x".to_vec(), Rc::default()),
                (),
                &request
            )
        ),
        Err(Error::Os(libc::EPIPE))
    ));
    drop(peer);
}

/// Budget, scope check and scope clone callbacks run without reactor borrows.
#[test]
fn budget_scope_and_scope_clone_can_reenter_without_refcell_borrows() {
    type Callback = Rc<RefCell<Option<Box<dyn Fn()>>>>;

    struct ReentrantScope(Callback);

    impl Clone for ReentrantScope {
        fn clone(&self) -> Self {
            if let Some(callback) = &*self.0.borrow() {
                callback();
            }
            Self(self.0.clone())
        }
    }

    impl LibraryScope for ReentrantScope {
        type Error = Error;

        fn check(&self) -> Result<()> {
            if let Some(callback) = &*self.0.borrow() {
                callback();
            }
            Ok(())
        }
    }

    struct ReentrantBudget(Callback);

    impl Budget for ReentrantBudget {
        type Charge = ();

        fn charge(&self, _: usize) -> Result<()> {
            if let Some(callback) = &*self.0.borrow() {
                callback();
            }
            Ok(())
        }
    }
    let sim = Simulation::new();
    let _sim = sim.enter();
    let callback: Callback = Rc::default();
    let reactor = Rc::new(Reactor::<ReentrantScope, _>::new(
        4,
        ReentrantBudget(callback.clone()),
    ));
    let weak = Rc::downgrade(&reactor);
    let calls = Rc::new(Cell::new(0));
    let counted = calls.clone();
    *callback.borrow_mut() = Some(Box::new(move || {
        let reactor = weak.upgrade().unwrap();
        // Both borrow the adapter's and the library's state.
        let _ = reactor.in_flight();
        let _ = reactor.is_simulated();
        counted.set(counted.get() + 1);
    }));
    let request = ReentrantScope(callback.clone());
    let (fd, peer) = sim.socket_pair();
    let mut receive = reactor.recv(Rc::new(fd), Counted(vec![0], Rc::default()), (), &request);
    assert!(poll(&mut receive).is_pending());
    reactor.poll_budgeted(1).unwrap();
    peer.try_send(b"x").unwrap();
    let mut done = None;
    for _ in 0..8 {
        reactor.poll_budgeted(1).unwrap();
        if let Poll::Ready(result) = poll(&mut receive) {
            done = Some(result);
            break;
        }
    }
    assert_eq!(done.unwrap().unwrap().bytes, 1);
    assert!(calls.get() > 0);
    callback.borrow_mut().take();
}

/// Simulated clock polling wakes cooperatively without submissions.
#[test]
fn clock_poll_never_submits_and_preserves_scope_precedence() {
    use crate::test_util::WakeCounter;
    let clock = crate::environment::SimulationClock::new(33);
    let _clock = clock.environment(0).enter();
    let sim = Simulation::new();
    let _sim = sim.enter();
    let reactor = SimReactor::new(4, ());
    let scope = scope();
    let until = crate::environment::now() + Duration::from_millis(5);
    let count = std::sync::Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let mut future = reactor.sleep_until(until, SleepMode::ClockPoll, &scope);
    assert!(future.as_mut().poll(&mut cx).is_pending());
    assert_eq!(count.count(), 1);
    assert_eq!(reactor.in_flight(), 0);
    clock.advance(Duration::from_millis(5));
    assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
    assert_eq!(submissions(&sim, OpKind::Timeout), 0);
    scope.cancel();
    assert_eq!(
        poll(&mut reactor.sleep_until(until, SleepMode::ClockPoll, &scope)),
        Poll::Ready(Err(Error::Canceled))
    );
}

/// Mixed clock domains are rejected even for expired timers.
#[test]
fn timer_rejects_mixed_clock_and_backend_even_for_expired_deadlines() {
    let request = scope();
    let reactor = SimReactor::new(2, ());
    let expired = crate::environment::now();
    assert_eq!(
        poll(&mut reactor.sleep_until(expired, SleepMode::ClockPoll, &request)),
        Poll::Ready(Err(Error::InvalidConfiguration))
    );
    let clock = crate::environment::SimulationClock::new(4);
    {
        let _clock = clock.environment(0).enter();
        assert_eq!(
            poll(&mut reactor.sleep_until(expired, SleepMode::Kernel, &request)),
            Poll::Ready(Err(Error::InvalidConfiguration))
        );
    }
    // A simulated backend rejects kernel timers even on the host clock.
    let sim = Simulation::new();
    let simulated = {
        let _sim = sim.enter();
        SimReactor::new(2, ())
    };
    assert_eq!(
        poll(&mut simulated.sleep_until(expired, SleepMode::Kernel, &request)),
        Poll::Ready(Err(Error::InvalidConfiguration))
    );
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(simulated.in_flight(), 0);
}
