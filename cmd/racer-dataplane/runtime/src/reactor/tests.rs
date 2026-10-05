use super::*;
use fixtures::{Admission, Limits, Reactor, RequestScope, ResourceClass};
pub(super) mod fixtures;

#[test]
fn operation_preserves_scope_and_buffer_errors_before_submission() {
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum AppError {
        Runtime(Error),
        Buffer,
    }
    impl From<Error> for AppError {
        fn from(error: Error) -> Self {
            Self::Runtime(error)
        }
    }
    #[derive(Clone)]
    struct AppScope;
    impl Scope for AppScope {
        type Error = AppError;
        fn check(&self) -> Result<(), AppError> {
            Ok(())
        }
    }
    struct FailedBuffer;
    // SAFETY: never exposes backing pointers; every accessor fails.
    unsafe impl IoBuffer for FailedBuffer {
        type Error = AppError;
        fn bytes(&self) -> Result<&[u8], AppError> {
            Err(AppError::Buffer)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8], AppError> {
            Err(AppError::Buffer)
        }
    }
    let reactor = super::Reactor::<AppScope, ()>::new(4, ());
    let (fd, _peer) = UnixStream::pair().unwrap();
    let mut operation = reactor.send(Rc::new(fd.into()), FailedBuffer, (), &AppScope);
    assert!(matches!(
        operation
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref())),
        Poll::Ready(Err(AppError::Buffer))
    ));
    assert_eq!(reactor.in_flight(), 0);
    assert!(reactor.state.borrow().ring.is_none());
}

#[test]
fn rejected_budget_and_invalid_capacity_publish_no_kernel_owners() {
    struct Reject;
    impl Budget for Reject {
        type Charge = ();
        fn charge(&self, _: usize) -> Result<()> {
            Err(Error::Overloaded)
        }
    }
    let reactor = super::Reactor::<RequestScope, Reject>::new(4, Reject);
    assert_eq!(reactor.init(), Err(Error::Overloaded));
    assert_eq!(reactor.in_flight(), 0);
    assert!(reactor.state.borrow().ring.is_none());
    assert!(reactor.state.borrow().ring_reservation.is_none());
    assert!(matches!(
        reactor.reserve_submissions(5, ()),
        Err(Error::InvalidConfiguration)
    ));
    let overflow = super::Reactor::<RequestScope, ()>::new(usize::MAX, ());
    assert_eq!(overflow.init(), Err(Error::InvalidConfiguration));
    let zero = super::Reactor::<RequestScope, ()>::new(0, ());
    assert_eq!(zero.init(), Err(Error::InvalidConfiguration));
}
mod completion {
    use super::*;

    #[test]
    fn finish_releases_slot_and_waker_borrow_before_result_destruction() {
        struct Probe {
            signal: Rc<Signal>,
            drops: Rc<Cell<usize>>,
        }
        impl Drop for Probe {
            fn drop(&mut self) {
                assert!(self.signal.waker.try_borrow_mut().unwrap().is_none());
                self.drops.set(self.drops.get() + 1);
            }
        }
        for abandoned in [false, true] {
            let Some(reactor) = kernel_reactor(1) else {
                return;
            };
            let signal = Rc::new(RefCell::new(None::<Rc<Signal>>));
            let finish_signal = signal.clone();
            let drops = Rc::new(Cell::new(0));
            let finish_drops = drops.clone();
            let ordinary = reactor.ordinary.clone();
            let waiting = reactor
                .submit(
                    Submission::Real(opcode::Nop::new().build()),
                    &scope(),
                    false,
                    move |result| {
                        assert_eq!(ordinary.get(), 0, "slot released before finish");
                        match result {
                            Ok(result) => assert_eq!(result.value(), Ok(0)),
                            Err(error) => {
                                assert!(abandoned);
                                assert_eq!(error, Error::Cancelled);
                            }
                        }
                        Ok(Probe {
                            signal: finish_signal.borrow().as_ref().unwrap().clone(),
                            drops: finish_drops,
                        })
                    },
                )
                .unwrap();
            *signal.borrow_mut() = Some(waiting.signal.clone());
            let counter = Arc::new(Count::default());
            *waiting.signal.waker.borrow_mut() = Some(Waker::from(counter.clone()));
            let reply = waiting.reply.clone();
            let mut waiting = Some(waiting);
            if abandoned {
                drop(waiting.take());
            }
            let deadline = Instant::now() + Duration::from_secs(2);
            while reactor.in_flight() != 0 {
                assert!(Instant::now() < deadline);
                reactor.poll_budgeted(1).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            assert_eq!(drops.get(), usize::from(abandoned));
            assert_eq!(counter.0.load(Ordering::Relaxed), usize::from(!abandoned));
            assert_eq!(reply.borrow().result.is_some(), !abandoned);
            drop(reply.borrow_mut().result.take());
            assert_eq!(drops.get(), 1);
        }
    }

    #[test]
    fn connecting_lease_survives_abandonment_until_kernel_fence() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let fd = unsafe {
            libc::socket(
                libc::AF_INET,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(fd >= 0);
        let fd = Rc::new(unsafe { Descriptor::from_raw_fd(fd) });
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let quota = reactor
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let scope = scope();
        let mut operation = reactor.connect_with_lease(
            fd,
            SocketAddress::Inet(listener.local_addr().unwrap()),
            (Lease(drops.clone()), quota),
            &scope,
        );
        assert!(poll(&mut operation).is_pending());
        drop(operation);
        assert_eq!(
            drops.get(),
            0,
            "dropped future cannot release connecting admission"
        );
        assert!(weak.upgrade().is_some());
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(drops.get(), 1);
        assert!(weak.upgrade().is_none());
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 0);
        assert_eq!(reactor.in_flight(), 0);
    }

    #[test]
    fn fence_waiters_sleep_until_both_cqes_and_unregister_on_drop() {
        for cancel_first in [false, true] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(4))));
            reactor.state.borrow_mut().entries.insert(
                IoId(1),
                Entry {
                    finish: Box::new(|_| None),
                    signal: Rc::new(Signal {
                        abandoned: Cell::new(false),
                        waker: RefCell::new(None),
                    }),
                    scope: Rc::new(scope()),
                    original: None,
                    accept: false,
                    cancel_reason: None,
                    cancel_sent: true,
                    cancel_done: false,
                },
            );
            let counter = Arc::new(Count(AtomicUsize::new(0)));
            let waker = Waker::from(counter.clone());
            let mut cx = Context::from_waker(&waker);
            let mut cancel = reactor.cancel_and_fence(IoId(1));
            let mut drain = reactor.drain();
            let mut abandoned = reactor.cancel_and_fence(IoId(1));
            for future in [&mut cancel, &mut drain, &mut abandoned] {
                for _ in 0..3 {
                    assert!(future.as_mut().poll(&mut cx).is_pending());
                }
            }
            assert_eq!(counter.0.load(Ordering::Relaxed), 0);
            assert_eq!(reactor.state.borrow().fence_waiters.len(), 3);
            drop(abandoned);
            assert_eq!(reactor.state.borrow().fence_waiters.len(), 2);
            let first = if cancel_first { 1 | CANCEL_BIT } else { 1 };
            let second = if cancel_first { 1 } else { 1 | CANCEL_BIT };
            assert!(
                reactor
                    .state
                    .borrow_mut()
                    .complete(first, -libc::ECANCELED)
                    .unwrap()
                    .is_none()
            );
            assert!(cancel.as_mut().poll(&mut cx).is_pending());
            assert!(drain.as_mut().poll(&mut cx).is_pending());
            assert_eq!(counter.0.load(Ordering::Relaxed), 0);
            let (entry, wakes) = {
                let mut state = reactor.state.borrow_mut();
                let entry = state.complete(second, -libc::ENOENT).unwrap().unwrap();
                let mut wakes = state.take_fence_wakers(Some(IoId(1)));
                wakes.extend(state.take_fence_wakers(None));
                (entry, wakes)
            };
            entry.finish();
            for waker in wakes {
                waker.wake();
            }
            assert_eq!(counter.0.load(Ordering::Relaxed), 2);
            assert!(matches!(cancel.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
            assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
            assert!(reactor.state.borrow().fence_waiters.is_empty());
            assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 0);
        }
    }

    #[test]
    fn fence_waiters_are_bounded_and_refresh_executor_wakers() {
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        reactor.state.borrow_mut().entries.insert(
            IoId(1),
            Entry {
                finish: Box::new(|_| None),
                signal: Rc::new(Signal {
                    abandoned: Cell::new(false),
                    waker: RefCell::new(None),
                }),
                scope: Rc::new(scope()),
                original: None,
                accept: false,
                cancel_reason: None,
                cancel_sent: false,
                cancel_done: false,
            },
        );
        let mut first = reactor.cancel_and_fence(IoId(1));
        assert!(poll(&mut first).is_pending());
        let refreshed = Waker::from(Arc::new(Count::default()));
        assert!(
            first
                .as_mut()
                .poll(&mut Context::from_waker(&refreshed))
                .is_pending()
        );
        assert!(
            reactor
                .state
                .borrow()
                .fence_waiters
                .first_key_value()
                .unwrap()
                .1
                .waker
                .will_wake(&refreshed)
        );
        let mut overflow = reactor.drain();
        assert!(matches!(
            poll(&mut overflow),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(first);
        let mut replacement = reactor.drain();
        assert!(poll(&mut replacement).is_pending());
        drop(replacement);
        assert!(reactor.state.borrow().fence_waiters.is_empty());
        reactor
            .state
            .borrow_mut()
            .complete(1, -libc::ECANCELED)
            .unwrap()
            .unwrap()
            .finish();
    }

    #[test]
    fn kernel_cancellation_wakes_registered_fences_without_self_waking() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let (socket, _peer) = UnixStream::pair().unwrap();
        let request = scope();
        let mut recv = reactor.recv(
            Rc::new(Descriptor::from(socket)),
            buffer(&[0; 8]),
            (),
            &request,
        );
        assert!(poll(&mut recv).is_pending());
        let id = *reactor.state.borrow().entries.first_key_value().unwrap().0;
        let counter = Arc::new(Count::default());
        let waker = Waker::from(counter.clone());
        let mut cx = Context::from_waker(&waker);
        let mut cancel = reactor.cancel_and_fence(id);
        let mut drain = reactor.drain();
        assert!(cancel.as_mut().poll(&mut cx).is_pending());
        assert!(drain.as_mut().poll(&mut cx).is_pending());
        assert_eq!(counter.0.load(Ordering::Relaxed), 0);
        let deadline = Instant::now() + Duration::from_secs(5);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(1).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(counter.0.load(Ordering::Relaxed), 2);
        assert!(matches!(cancel.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
        assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
        assert!(matches!(
            poll(&mut recv),
            Poll::Ready(Err(Error::Cancelled))
        ));
    }

    #[test]
    fn delayed_and_reordered_cancel_cqes_retain_every_owner() {
        for cancel_first in [false, true] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
            let (socket, _peer) = UnixStream::pair().unwrap();
            let fd = Rc::new(Descriptor::from(socket));
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let owned = InFlight {
                file: fd,
                buffer: Buffer(vec![0; 32], drops.clone()),
                lease: Lease(drops.clone()),
            };
            let signal = Rc::new(Signal {
                abandoned: Cell::new(false),
                waker: RefCell::new(None),
            });
            let reservation: Rc<Charge> = Rc::new(Box::new(
                reactor
                    .admission
                    .reserve(None, ResourceClass::RequestContext, 64)
                    .unwrap(),
            ));
            let reply = Rc::new(RefCell::new(Reply::<(), Error> {
                _slot: None,
                result: None,
                _reservation: reservation.clone(),
            }));
            let waiting = Waiting {
                reply: reply.clone(),
                signal: signal.clone(),
            };
            reactor.state.borrow_mut().entries.insert(
                IoId(1),
                Entry {
                    finish: Box::new(move |result| {
                        drop((owned, reservation));
                        assert!(matches!(result, Err(Error::Cancelled)));
                        None
                    }),
                    scope: Rc::new(scope()),
                    signal,
                    original: None,
                    accept: false,
                    cancel_reason: Some(Error::Cancelled),
                    cancel_sent: true,
                    cancel_done: false,
                },
            );
            drop(waiting);
            drop(reply);
            assert_eq!(drops.get(), 0);
            let (first, second) = if cancel_first {
                ((1 | CANCEL_BIT, 0), (1, -libc::ECANCELED))
            } else {
                ((1, 7), (1 | CANCEL_BIT, -libc::ENOENT))
            };
            assert!(
                reactor
                    .state
                    .borrow_mut()
                    .complete(first.0, first.1)
                    .unwrap()
                    .is_none()
            );
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 64);
            let done = reactor
                .state
                .borrow_mut()
                .complete(second.0, second.1)
                .unwrap()
                .unwrap();
            done.finish();
            assert_eq!(drops.get(), 2);
            assert!(weak.upgrade().is_none());
            assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 0);
        }
    }

    #[test]
    fn accepted_descriptor_is_retained_until_cancel_fence() {
        use std::io::Read;
        use std::os::fd::IntoRawFd;
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        let (socket, mut peer) = UnixStream::pair().unwrap();
        peer.set_nonblocking(true).unwrap();
        let fd = socket.into_raw_fd();
        reactor.state.borrow_mut().entries.insert(
            IoId(1),
            Entry {
                finish: Box::new(|result| {
                    assert!(matches!(result, Err(Error::Cancelled)));
                    None
                }),
                scope: Rc::new(scope()),
                signal: Rc::new(Signal {
                    abandoned: Cell::new(true),
                    waker: RefCell::new(None),
                }),
                original: None,
                accept: true,
                cancel_reason: Some(Error::Cancelled),
                cancel_sent: true,
                cancel_done: false,
            },
        );
        assert!(
            reactor
                .state
                .borrow_mut()
                .complete(1, fd)
                .unwrap()
                .is_none()
        );
        let mut byte = [0];
        assert_eq!(
            peer.read(&mut byte).unwrap_err().kind(),
            std::io::ErrorKind::WouldBlock
        );
        reactor
            .state
            .borrow_mut()
            .complete(1 | CANCEL_BIT, -libc::ENOENT)
            .unwrap()
            .unwrap()
            .finish();
        // Peer EOF observes this socket closing even if another test reuses fd.
        assert_eq!(peer.read(&mut byte).unwrap(), 0);
    }

    #[test]
    fn delayed_short_success_and_error_return_only_on_original_cqe() {
        for result in [3, -libc::EIO] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
            let delivered = Rc::new(Cell::new(None));
            let output = delivered.clone();
            let drops = Rc::new(Cell::new(0));
            let lease = Lease(drops.clone());
            reactor.state.borrow_mut().entries.insert(
                IoId(1),
                Entry {
                    finish: Box::new(move |result| {
                        output.set(Some(result.and_then(KernelResult::value)));
                        drop(lease);
                        None
                    }),
                    scope: Rc::new(scope()),
                    signal: Rc::new(Signal {
                        abandoned: Cell::new(false),
                        waker: RefCell::new(None),
                    }),
                    original: None,
                    accept: false,
                    cancel_reason: None,
                    cancel_sent: false,
                    cancel_done: false,
                },
            );
            assert_eq!(delivered.get(), None);
            assert_eq!(drops.get(), 0);
            reactor
                .state
                .borrow_mut()
                .complete(1, result)
                .unwrap()
                .unwrap()
                .finish();
            assert_eq!(
                delivered.get(),
                Some(if result >= 0 {
                    Ok(3)
                } else {
                    Err(Error::Os(libc::EIO))
                })
            );
            assert_eq!(drops.get(), 1);
        }
    }
}

mod empty_submit_tests {
    use super::*;

    #[test]
    fn empty_submit_decision_truth_table() {
        // Cover every setup bit independently, including SQPOLL (1), IOPOLL (0),
        // COOP_TASKRUN (8), TASKRUN_FLAG (9), SINGLE_ISSUER (12), DEFER_TASKRUN (13),
        // and currently unknown bits. Only CQSIZE is allowlisted.
        for flags in std::iter::once(0)
            .chain((0..32).map(|bit| 1u32 << bit))
            .chain((0..32).map(|bit| SETUP_CQSIZE | (1u32 << bit)))
            .chain([u32::MAX])
        {
            for empty in [false, true] {
                for overflow in [false, true] {
                    for taskrun in [false, true] {
                        let skip =
                            matches!(flags, 0 | SETUP_CQSIZE) && empty && !overflow && !taskrun;
                        assert_eq!(
                            submission_required(flags, empty, overflow, taskrun),
                            !skip,
                            "flags={flags:#x} empty={empty} overflow={overflow} taskrun={taskrun}"
                        );
                    }
                }
            }
        }
    }

    #[test]
    fn empty_submit_skips_both_worker_submit_points() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        assert_eq!(
            ring_setup_flags(reactor.state.borrow().ring.as_ref().unwrap().params()),
            SETUP_CQSIZE,
            "suppression must match the actual builder configuration"
        );
        for _ in 0..3 {
            assert_eq!(reactor.poll_budgeted(8).unwrap(), 0);
            reactor.wait(Duration::ZERO).unwrap();
        }
        assert_eq!(reactor.state.borrow().submit_attempts, 0);
    }

    #[test]
    fn empty_submit_retries_shared_sq_after_transient_partial_and_fatal_results() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let mut state = reactor.state.borrow_mut();
        // NOPs have no borrowed resources. Do not feed their CQEs into the reactor's
        // operation table; this test isolates real shared SQ consumption.
        unsafe {
            state
                .ring
                .as_mut()
                .unwrap()
                .submission()
                .push_multiple(&[
                    opcode::Nop::new().build().user_data(1),
                    opcode::Nop::new().build().user_data(2),
                ])
                .unwrap();
        }
        for (result, expected) in [
            (Ok(0), Ok(())),
            (Err(std::io::Error::from_raw_os_error(libc::EINTR)), Ok(())),
            (Err(std::io::Error::from_raw_os_error(libc::EAGAIN)), Ok(())),
            (Err(std::io::Error::from_raw_os_error(libc::EBUSY)), Ok(())),
            (
                Err(std::io::Error::from_raw_os_error(libc::EIO)),
                Err(Error::Io),
            ),
        ] {
            let attempts = state.submit_attempts;
            state.submit_result = Some(result);
            assert_eq!(state.submit_pending(), expected);
            assert_eq!(state.submit_attempts, attempts + 1);
            assert_eq!(state.ring.as_mut().unwrap().submission().len(), 2);
        }
        // Consume exactly one real SQE. The production decision must observe the
        // shared head on the next call, not infer emptiness from a successful count.
        let partial = unsafe {
            // SAFETY: normal ring, one published NOP, no wait or extended argument.
            state
                .ring
                .as_ref()
                .unwrap()
                .submitter()
                .enter::<libc::sigset_t>(1, 0, 0, None)
        };
        assert_eq!(partial.as_ref().unwrap(), &1);
        state.submit_result = Some(partial);
        state.submit_pending().unwrap();
        assert_eq!(state.ring.as_mut().unwrap().submission().len(), 1);
        let attempts = state.submit_attempts;
        state.submit_pending().unwrap();
        assert_eq!(state.submit_attempts, attempts + 1);
        assert!(state.ring.as_mut().unwrap().submission().is_empty());
        state.submit_pending().unwrap();
        assert_eq!(state.submit_attempts, attempts + 1);
        assert_eq!(state.ring.as_mut().unwrap().completion().count(), 2);
    }

    #[test]
    fn empty_submit_wait_preserves_submitted_receive_and_external_wake_progress() {
        use std::io::Write;

        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let request = scope();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let drops = Rc::new(Cell::new(0));
        let mut receive = reactor.recv(
            Rc::new(Descriptor::from(socket)),
            Buffer(vec![0; 1], drops.clone()),
            Lease(drops.clone()),
            &request,
        );
        assert!(poll(&mut receive).is_pending());
        reactor.poll_budgeted(8).unwrap();
        assert_eq!(reactor.state.borrow().submit_attempts, 1);
        assert!(
            reactor
                .state
                .borrow_mut()
                .ring
                .as_mut()
                .unwrap()
                .submission()
                .is_empty()
        );

        // An eventfd notification must still be consumed while the SQ is empty and
        // the already-submitted receive owns its buffer/lease.
        reactor.waker().unwrap().wake().unwrap();
        reactor.wait(MAX_WAIT).unwrap();
        let mut value = 0u64;
        let wake_fd = reactor.state.borrow().wake.as_ref().unwrap().as_raw_fd();
        // SAFETY: valid nonblocking eventfd and initialized local output storage.
        assert_eq!(
            unsafe { libc::read(wake_fd, (&mut value as *mut u64).cast(), 8) },
            -1
        );
        assert_eq!(
            std::io::Error::last_os_error().raw_os_error(),
            Some(libc::EAGAIN)
        );
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.state.borrow().submit_attempts, 1);

        peer.set_nonblocking(true).unwrap();
        std::thread::scope(|threads| {
            let writer = threads.spawn(move || {
                // Write off-thread so the write syscall cannot run the ring owner's
                // taskwork. Delay only to exercise waiting, not to assert latency.
                std::thread::sleep(Duration::from_millis(20));
                // One nonblocking write bounds cleanup even if the owner panics;
                // the scoped thread is always joined, including during unwinding.
                peer.write(b"x")
            });
            let deadline = Instant::now() + Duration::from_secs(2);
            loop {
                // Only wait and CQ inspection run on the owner until completion.
                // No new SQEs or poll_budgeted calls can make this receive progress.
                reactor.wait(MAX_WAIT).unwrap();
                if !reactor
                    .state
                    .borrow_mut()
                    .ring
                    .as_mut()
                    .unwrap()
                    .completion()
                    .is_empty()
                {
                    break;
                }
                assert!(
                    Instant::now() < deadline,
                    "submitted receive did not complete"
                );
            }
            assert_eq!(writer.join().unwrap().unwrap(), 1);
        });
        assert_eq!(reactor.state.borrow().submit_attempts, 1);
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.poll_budgeted(1).unwrap(), 1);
        let Poll::Ready(Ok(completed)) = poll(&mut receive) else {
            panic!("receive must be ready after its CQE");
        };
        assert_eq!(completed.bytes, 1);
        assert_eq!(completed.buffer.bytes().unwrap(), b"x");
        drop(completed);
        assert_eq!(drops.get(), 2);
        assert_eq!(reactor.state.borrow().submit_attempts, 1);
    }

    #[test]
    fn empty_submit_still_submits_cancel_and_drains_submitted_receive() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let request = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let drops = Rc::new(Cell::new(0));
        let mut receive = reactor.recv(
            Rc::new(Descriptor::from(socket)),
            Buffer(vec![0; 1], drops.clone()),
            Lease(drops.clone()),
            &request,
        );
        assert!(poll(&mut receive).is_pending());
        reactor.wait(Duration::ZERO).unwrap();
        let id = *reactor.state.borrow().entries.first_key_value().unwrap().0;
        assert_eq!(reactor.state.borrow().submit_attempts, 1);
        let mut cancel = reactor.cancel_and_fence(id);
        let mut drain = reactor.drain();
        assert!(poll(&mut cancel).is_pending());
        assert!(poll(&mut drain).is_pending());
        reactor.wait(Duration::ZERO).unwrap();
        assert_eq!(
            reactor.state.borrow().submit_attempts,
            1,
            "wait cannot scan cancels"
        );
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.poll_budgeted(1).unwrap(), 0);
        assert_eq!(
            reactor.state.borrow().submit_attempts,
            2,
            "cancel SQE must be submitted"
        );
        assert_eq!(drops.get(), 0, "submission is not a completion fence");
        drive(&reactor, drain).unwrap();
        assert!(matches!(poll(&mut cancel), Poll::Ready(Ok(()))));
        assert!(matches!(
            poll(&mut receive),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(drops.get(), 2);
        reactor.wait(Duration::ZERO).unwrap();
        assert_eq!(reactor.state.borrow().submit_attempts, 2);
    }
}
mod reserved_submission_tests {
    use super::*;
    #[test]
    fn ordinary_completed_reply_does_not_hold_queue_capacity() {
        let Some(reactor) = kernel_reactor(1) else {
            return;
        };
        let request = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut send = reactor.send(fd.clone(), buffer(b"x"), (), &request);
        assert!(poll(&mut send).is_pending());
        let deadline = Instant::now() + Duration::from_secs(2);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        // Ordinary reply memory remains dynamically charged, not prepaid. Preserve
        // the original queue semantics while the completed result is still unpolled.
        assert_eq!(reactor.ordinary.get(), 0);
        let mut receive = reactor.recv(fd, buffer(&[0]), (), &request);
        assert!(poll(&mut receive).is_pending());
        assert!(matches!(poll(&mut send), Poll::Ready(Ok(_))));
        drop((send, receive));
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.ordinary.get(), 0);
    }
}
mod socket {
    //! Socket scenarios exercise ownership through the reactor's public operations.
    use super::*;

    #[test]
    fn wait_submits_service_turn_sqe_before_next_completion_poll() {
        use std::io::Write;

        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let baseline = reactor.admission.used(ResourceClass::RequestContext);
        let request = scope();
        let (socket, mut peer) = UnixStream::pair().unwrap();
        socket.set_nonblocking(true).unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));

        // Match the worker order: poll_runtime's empty reactor poll, then service
        // polling queues I/O, then wait. No second completion poll may submit it.
        assert_eq!(reactor.poll_budgeted(8).unwrap(), 0);
        let mut receive = reactor.recv(
            fd,
            Buffer(vec![0; 1], drops.clone()),
            Lease(drops.clone()),
            &request,
        );
        assert!(poll(&mut receive).is_pending());
        assert_eq!(
            reactor
                .state
                .borrow_mut()
                .ring
                .as_mut()
                .unwrap()
                .submission()
                .len(),
            1
        );
        drop(receive);
        let retained = reactor.admission.used(ResourceClass::RequestContext);
        assert!(retained > baseline);

        reactor.wait(Duration::ZERO).unwrap();
        assert_eq!(reactor.state.borrow().submit_attempts, 1);
        {
            let mut state = reactor.state.borrow_mut();
            let ring = state.ring.as_mut().unwrap();
            assert_eq!(ring.submission().len(), 0, "wait must submit queued SQEs");
            assert_eq!(ring.completion().len(), 0, "receive still awaits peer data");
            let entry = state.entries.first_key_value().unwrap().1;
            assert!(entry.original.is_none());
            assert!(!entry.cancel_sent, "wait must not scan cancellations");
        }
        assert_eq!(reactor.in_flight(), 1);
        assert_eq!(drops.get(), 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            retained
        );

        // Make the submitted receive complete without driving poll_budgeted.
        peer.write_all(b"x").unwrap();
        let mut descriptor = libc::pollfd {
            fd: reactor.state.borrow().ring.as_ref().unwrap().as_raw_fd(),
            events: libc::POLLIN,
            revents: 0,
        };
        // SAFETY: poll borrows one initialized descriptor for this bounded call.
        assert_eq!(unsafe { libc::poll(&mut descriptor, 1, 1000) }, 1);
        assert_ne!(descriptor.revents & libc::POLLIN, 0);
        reactor.wait(Duration::ZERO).unwrap();
        assert_eq!(reactor.poll_budgeted(0).unwrap(), 0);
        assert_eq!(reactor.state.borrow().submit_attempts, 1);
        assert_eq!(
            reactor
                .state
                .borrow_mut()
                .ring
                .as_mut()
                .unwrap()
                .completion()
                .len(),
            1,
            "wait and a zero budget must leave the CQE for the next worker turn"
        );
        assert_eq!(reactor.in_flight(), 1);
        assert_eq!(drops.get(), 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            retained
        );

        assert_eq!(reactor.poll_budgeted(1).unwrap(), 1);
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(drops.get(), 2);
        assert!(weak.upgrade().is_none());
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            baseline
        );
    }

    #[test]
    fn listener_retry_recovers_from_full_sq_without_losing_owners() {
        // Cancellation SQEs also consume SQ space without new table entries.
        // A smaller test ring isolates SQ publication failure from table pressure
        // deterministically, without depending on kernel cancellation timing.
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        reactor.state.borrow_mut().ring = Some(IoUring::new(2).unwrap());
        let scope = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut first = reactor.readiness(fd.clone(), libc::POLLIN as u32, &scope);
        let mut second = reactor.readiness(fd, libc::POLLIN as u32, &scope);
        assert!(poll(&mut first).is_pending());
        assert!(poll(&mut second).is_pending());
        assert_eq!(reactor.in_flight(), 2);
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap();
        let listener = Rc::new(Descriptor::from(listener));
        let baseline = reactor.admission.used(ResourceClass::RequestContext);
        let mut raw = reactor.accept(listener.clone(), &scope);
        assert!(matches!(
            poll(&mut raw),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(raw);
        assert_eq!(reactor.in_flight(), 2);
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            baseline
        );
        // The retry policy itself is tested in Racer. This raw-ring test uses
        // a minimal yielding retry to exercise SQ publication recovery.
        let mut accept: Operation<'_, Descriptor> = Box::pin(async {
            loop {
                match reactor.accept(listener.clone(), &scope).await {
                    Err(Error::Overloaded) => {
                        crate::yield_now().await;
                    }
                    result => break result,
                }
            }
        });
        assert!(poll(&mut accept).is_pending());
        assert_eq!(reactor.in_flight(), 2);
        assert_eq!(Rc::strong_count(&listener), 1);
        let _client = std::net::TcpStream::connect(address).unwrap();
        drop(drive(&reactor, accept).unwrap());
        // Retrying the listener did not abandon unrelated operations.
        assert_eq!(reactor.in_flight(), 2);
        drop(first);
        drop(second);
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(Rc::strong_count(&listener), 1);
    }

    #[test]
    fn real_cancellation_abandonment_limits_and_drop_fence() {
        let Some(reactor) = kernel_reactor(1) else {
            return;
        };
        let scope = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let mut receive = reactor.recv(
            fd.clone(),
            Buffer(vec![0; 32], drops.clone()),
            Lease(drops.clone()),
            &scope,
        );
        assert!(poll(&mut receive).is_pending());
        reactor.poll_budgeted(1).unwrap();
        assert_eq!(drops.get(), 0);
        let mut overflow = reactor.recv(fd.clone(), buffer(&[0; 1]), (), &scope);
        assert!(matches!(
            poll(&mut overflow),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(overflow);
        assert_eq!(reactor.poll_budgeted(0).unwrap(), 0);
        drop(receive);
        drop(fd);
        assert_eq!(drops.get(), 0);
        assert!(weak.upgrade().is_some());
        // Drop must submit cancellation and consume both CQEs before ownership ends.
        drop(reactor);
        assert_eq!(drops.get(), 2);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn real_deadline_and_explicit_cancel_fences() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let scope = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut ready = reactor.readiness(fd.clone(), libc::POLLIN as u32, &scope);
        assert!(poll(&mut ready).is_pending());
        let id = *reactor.state.borrow().entries.first_key_value().unwrap().0;
        drive(&reactor, reactor.cancel_and_fence(id)).unwrap();
        assert!(matches!(
            poll(&mut ready),
            Poll::Ready(Err(Error::Cancelled))
        ));
        let short = RequestScope {
            deadline: Deadline(Instant::now() + Duration::from_millis(20)),
            ..scope.clone()
        };
        assert!(matches!(
            drive(
                &reactor,
                reactor.readiness(fd.clone(), libc::POLLIN as u32, &short)
            ),
            Err(Error::DeadlineExceeded)
        ));
        scope.cancel().unwrap();
        assert!(matches!(
            drive(&reactor, reactor.recv(fd, buffer(&[0; 1]), (), &scope)),
            Err(Error::Cancelled)
        ));
        assert_eq!(reactor.in_flight(), 0);
    }

    #[test]
    fn real_offset_file_io_and_quota_release() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let scope = scope();
        let baseline = reactor.admission.used(ResourceClass::RequestContext);
        // Anonymous memory-backed regular file avoids filesystem fixture side effects.
        let raw = unsafe { libc::memfd_create(c"reactor-test".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(raw >= 0);
        let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
        let written = drive(
            &reactor,
            reactor.write_at(fd.clone(), 7, buffer(b"payload"), (), &scope),
        )
        .unwrap();
        assert_eq!(written.bytes, 7);
        let read = drive(
            &reactor,
            reactor.read_at(fd, 7, buffer(&[0; 32]), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 7);
        assert_eq!(&read.buffer.bytes().unwrap()[..7], b"payload");
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            baseline
        );
    }

    #[test]
    fn real_external_wake_is_persistent_and_bounded() {
        let Some(reactor) = kernel_reactor(1) else {
            return;
        };
        let wake = reactor.waker().unwrap();
        std::thread::spawn(move || wake.wake().unwrap())
            .join()
            .unwrap();
        let fd = reactor.state.borrow().wake.as_ref().unwrap().as_raw_fd();
        let mut descriptor = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        assert_eq!(unsafe { libc::poll(&mut descriptor, 1, 0) }, 1);
        reactor.wait(Duration::from_secs(60)).unwrap();
        assert_eq!(unsafe { libc::poll(&mut descriptor, 1, 0) }, 0);
    }

    #[test]
    fn real_socket_short_io_readiness_eof_and_broken_pipe() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        let (left, right) = UnixStream::pair().unwrap();
        left.set_nonblocking(true).unwrap();
        right.set_nonblocking(true).unwrap();
        let left = Rc::new(Descriptor::from(left));
        let right = Rc::new(Descriptor::from(right));
        let sent = drive(
            &reactor,
            reactor.send(left.clone(), buffer(b"hello"), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 5);
        let ready = drive(
            &reactor,
            reactor.readiness(right.clone(), libc::POLLIN as u32, &scope),
        )
        .unwrap();
        assert_ne!(ready & libc::POLLIN as u32, 0);
        let received = drive(
            &reactor,
            reactor.recv(right.clone(), buffer(&[0; 64]), (), &scope),
        )
        .unwrap();
        assert_eq!(received.bytes, 5);
        assert_eq!(&received.buffer.bytes().unwrap()[..5], b"hello");
        drop(left);
        assert_eq!(
            drive(
                &reactor,
                reactor.recv(right.clone(), buffer(&[0; 64]), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        assert!(matches!(
            drive(&reactor, reactor.send(right, buffer(b"x"), (), &scope)),
            Err(Error::Os(libc::EPIPE))
        ));
    }

    #[test]
    fn real_connect_lease_survives_cqes_until_result_is_consumed() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let request = scope();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let raw = unsafe {
            libc::socket(
                libc::AF_INET,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(raw >= 0);
        let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let quota = reactor
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let mut connect = reactor.connect_with_lease(
            fd,
            SocketAddress::Inet(listener.local_addr().unwrap()),
            (Lease(drops.clone()), quota),
            &request,
        );
        assert!(poll(&mut connect).is_pending());
        assert!(weak.upgrade().is_some());
        let deadline = Instant::now() + Duration::from_secs(5);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(1).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        // A completed but unconsumed reply must still quarantine the endpoint slot.
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
        let Poll::Ready(Ok(lease)) = poll(&mut connect) else {
            panic!("connect did not return its lease");
        };
        drop(connect);
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
        drop(lease);
        assert_eq!(drops.get(), 1);
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 0);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn real_connect_lease_is_quarantined_after_cancel_and_error() {
        for (cancel, invalid_family) in [(true, false), (false, true)] {
            let Some(reactor) = kernel_reactor(2) else {
                return;
            };
            let request = scope();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            // An AF_UNIX socket with an Inet address deterministically fails in
            // the kernel, without racing another listener for an unused TCP port.
            let raw = unsafe {
                libc::socket(
                    if invalid_family {
                        libc::AF_UNIX
                    } else {
                        libc::AF_INET
                    },
                    libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                    0,
                )
            };
            assert!(raw >= 0);
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let quota = reactor
                .admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap();
            let mut connect = reactor.connect_with_lease(
                fd,
                SocketAddress::Inet(listener.local_addr().unwrap()),
                (Lease(drops.clone()), quota),
                &request,
            );
            assert!(poll(&mut connect).is_pending());
            if cancel {
                request.cancel().unwrap();
            }
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
            let deadline = Instant::now() + Duration::from_secs(5);
            while reactor.in_flight() != 0 {
                assert!(Instant::now() < deadline);
                reactor.poll_budgeted(1).unwrap();
                if reactor.in_flight() != 0 {
                    // Includes the interval between original and cancel CQEs
                    // when a cancel SQE wins the race against connect completion.
                    assert_eq!(drops.get(), 0);
                    assert!(weak.upgrade().is_some());
                    assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
                }
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            let expected = if cancel {
                Error::Cancelled
            } else {
                Error::Os(libc::EINVAL)
            };
            let Poll::Ready(Err(error)) = poll(&mut connect) else {
                panic!("connect must fail")
            };
            assert_eq!(error, expected);
            assert_eq!(drops.get(), 1);
            assert!(weak.upgrade().is_none());
            assert_eq!(reactor.admission.used(ResourceClass::Connection), 0);
        }
    }

    #[test]
    fn real_tcp_accept_connect() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap();
        let listener = Rc::new(Descriptor::from(listener));
        let raw = unsafe {
            libc::socket(
                libc::AF_INET,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(raw >= 0);
        let client = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
        let mut accept = reactor.accept(listener, &scope);
        assert!(poll(&mut accept).is_pending());
        drive(
            &reactor,
            reactor.connect(client.clone(), SocketAddress::Inet(address), &scope),
        )
        .unwrap();
        let server = Rc::new(drive(&reactor, accept).unwrap());
        drive(
            &reactor,
            reactor.send(client, buffer(b"connected"), (), &scope),
        )
        .unwrap();
        let received = drive(&reactor, reactor.recv(server, buffer(&[0; 32]), (), &scope)).unwrap();
        assert_eq!(
            &received.buffer.bytes().unwrap()[..received.bytes],
            b"connected"
        );
    }

    #[test]
    fn real_unix_connect_keeps_sockaddr_alive() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        // Linux exposes an unnamed Unix listener's autobound abstract name via
        // getsockname, but SocketAddress::Unix intentionally means a filesystem
        // path. Use a process-unique socket in the existing build directory.
        let path =
            PathBuf::from("target").join(format!("reactor-unix-{}.sock", std::process::id()));
        struct RemoveSocket(PathBuf);
        impl Drop for RemoveSocket {
            fn drop(&mut self) {
                let _ = std::fs::remove_file(&self.0);
            }
        }
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let _cleanup = RemoveSocket(path.clone());
        listener.set_nonblocking(true).unwrap();
        let raw = unsafe {
            libc::socket(
                libc::AF_UNIX,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(raw >= 0);
        let client = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
        let mut accept = reactor.accept(Rc::new(listener.into()), &scope);
        assert!(poll(&mut accept).is_pending());
        drive(
            &reactor,
            reactor.connect(client.clone(), SocketAddress::Unix(path), &scope),
        )
        .unwrap();
        let server = Rc::new(drive(&reactor, accept).unwrap());
        drive(&reactor, reactor.send(client, buffer(b"unix"), (), &scope)).unwrap();
        let received = drive(&reactor, reactor.recv(server, buffer(&[0; 32]), (), &scope)).unwrap();
        assert_eq!(&received.buffer.bytes().unwrap()[..received.bytes], b"unix");
    }
}
use crate::deadline::{Cancellation, Deadline};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::{num::NonZeroUsize, os::unix::net::UnixStream, time::Instant};

#[derive(Default)]
struct Count(AtomicUsize);
impl std::task::Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

struct Buffer(Vec<u8>, Rc<Cell<usize>>);
// SAFETY: private fixed Vec owns independently allocated backing.
unsafe impl IoBuffer for Buffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.0)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.0)
    }
}
impl Drop for Buffer {
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
fn buffer(bytes: &[u8]) -> Buffer {
    Buffer(bytes.into(), Rc::new(Cell::new(0)))
}

#[test]
fn connect_observation_retains_errno_before_generic_boundary_mapping() {
    for errno in [
        libc::ENOBUFS,
        libc::ENOMEM,
        libc::EADDRNOTAVAIL,
        libc::ECONNREFUSED,
        libc::ECONNRESET,
    ] {
        let observation = Cell::new(None);
        let result = KernelResult::Value(-errno);
        result.observe_errno(&observation);
        assert_eq!(observation.get(), Some(errno));
        assert_eq!(result.value(), Err(Error::Os(errno)));
    }
    let observation = Cell::new(None);
    KernelResult::Value(0).observe_errno(&observation);
    assert_eq!(observation.get(), None);
}
#[test]
fn sockaddr_encoding_is_owned_and_validated() {
    let (address, len) =
        encode_address(SocketAddress::Inet("127.0.0.1:1234".parse().unwrap())).unwrap();
    assert_eq!(len as usize, std::mem::size_of::<libc::sockaddr_in>());
    let value = unsafe { &*address.as_ptr().cast::<libc::sockaddr_in>() };
    assert_eq!(value.sin_port, 1234u16.to_be());
    assert_eq!(value.sin_addr.s_addr.to_ne_bytes(), [127, 0, 0, 1]);
    let (address, _) = encode_address(SocketAddress::Inet("[::1]:4321".parse().unwrap())).unwrap();
    assert_eq!(
        address.into_inner().ss_family,
        libc::AF_INET6 as libc::sa_family_t
    );
    let (address, len) = encode_address(SocketAddress::Unix("/a/b".into())).unwrap();
    assert_eq!(
        address.into_inner().ss_family,
        libc::AF_UNIX as libc::sa_family_t
    );
    assert_eq!(
        len as usize,
        std::mem::offset_of!(libc::sockaddr_un, sun_path) + 5
    );
    for path in [
        PathBuf::new(),
        PathBuf::from("a\0b"),
        PathBuf::from("x".repeat(108)),
    ] {
        assert!(matches!(
            encode_address(SocketAddress::Unix(path)),
            Err(Error::InvalidInput)
        ));
    }
}
#[test]
fn constructor_does_not_open_kernel_resources() {
    let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
    assert!(reactor.state.borrow().ring.is_none());
    assert!(reactor.state.borrow().wake.is_none());
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(reactor.poll_budgeted(1).unwrap(), 0);
}
#[test]
fn wait_does_not_initialize_absent_ring() {
    let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
    reactor.wait(Duration::ZERO).unwrap();
    let state = reactor.state.borrow();
    assert!(state.ring.is_none());
    assert!(state.wake.is_none());
    assert!(state.ring_reservation.is_none());
    assert!(state.entries.is_empty());
    assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 0);
}
#[test]
fn submission_classifies_transient_and_fatal_errors() {
    for count in [0, 1, 8] {
        assert_eq!(submission_result(Ok(count)), Ok(()));
    }
    for errno in [libc::EINTR, libc::EAGAIN, libc::EBUSY] {
        assert_eq!(
            submission_result(Err(std::io::Error::from_raw_os_error(errno))),
            Ok(())
        );
    }
    for errno in [libc::EIO, libc::EBADF, libc::EINVAL, libc::ENOMEM] {
        assert_eq!(
            submission_result(Err(std::io::Error::from_raw_os_error(errno))),
            Err(Error::Io)
        );
    }
    assert_eq!(
        submission_result(Err(std::io::Error::other("submission failed"))),
        Err(Error::Io)
    );
}

fn limits(capacity: usize) -> Limits {
    Limits {
        queue_entries: NonZeroUsize::new(capacity).unwrap(),
    }
}
pub(super) fn scope() -> RequestScope {
    RequestScope {
        request: (),
        deadline: Deadline(Instant::now() + Duration::from_secs(5)),
        cancellation: Cancellation::new().unwrap(),
    }
}
pub(super) fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
pub(super) fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(result) = poll(&mut future) {
            return result;
        }
        assert!(Instant::now() < deadline, "reactor failed to make progress");
        reactor.poll_budgeted(8).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}
pub(super) fn kernel_reactor(capacity: usize) -> Option<Reactor> {
    // Skip only when the kernel lacks io_uring or policy denies ring creation.
    // Configuration, quota, opcode, and ordinary I/O errors must fail tests.
    match IoUring::new(2) {
        Ok(ring) => drop(ring),
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
            ) =>
        {
            assert_ne!(
                std::env::var("RUNTIME_REQUIRE_IO_URING").as_deref(),
                Ok("1"),
                "RUNTIME_REQUIRE_IO_URING=1 but io_uring is unavailable: {error}"
            );
            eprintln!("io_uring kernel test unavailable: {error}");
            return None;
        }
        Err(error) => panic!("unexpected io_uring setup failure: {error}"),
    }
    let reactor = Reactor::new(Rc::new(Admission::new(limits(capacity))));
    reactor.init().unwrap();
    Some(reactor)
}

#[cfg(feature = "simulation")]
mod retry_regressions {
    use super::*;
    use simulation::{Fault, Simulation};

    fn submissions(sim: &Simulation, name: &str) -> usize {
        sim.trace()
            .iter()
            .filter(|event| event.operation == format!("submit:{name}"))
            .count()
    }

    #[test]
    fn transient_data_errors_retain_exact_owners_and_return_partial_progress_once() {
        for reserved in [false, true] {
            for errno in [libc::EINTR, libc::EAGAIN] {
                let sim = Simulation::new();
                let _environment = sim.enter();
                let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
                let request = scope();
                let capacity = reserved.then(|| reactor.reserve_submissions(1, ()).unwrap());
                let (fd, peer) = sim.socket_pair();
                let drops = Rc::new(Cell::new(0));
                let owned = Buffer(b"abcdef".to_vec(), drops.clone());
                let ptr = owned.0.as_ptr();
                sim.inject("send", Fault::Errno(errno)).unwrap();
                sim.inject("send", Fault::Short(2)).unwrap();
                let fd = Rc::new(fd);
                let weak = Rc::downgrade(&fd);
                let mut operation = reactor.buffer_io(
                    fd,
                    owned,
                    Lease(drops.clone()),
                    &request,
                    BufferOperation::Send,
                    capacity,
                );
                assert!(poll(&mut operation).is_pending());
                for _ in 0..2 {
                    reactor.poll_budgeted(1).unwrap();
                }
                assert_eq!(drops.get(), 0, "transient CQE retains owners before repoll");
                assert!(weak.upgrade().is_some());
                let completion = drive(&reactor, operation).unwrap();
                assert_eq!(completion.bytes, 2);
                assert_eq!(completion.buffer.0.as_ptr(), ptr);
                assert_eq!(drops.get(), 0);
                assert_eq!(submissions(&sim, "send"), 2);
                assert_eq!(
                    submissions(&sim, "poll"),
                    usize::from(errno == libc::EAGAIN)
                );
                let mut bytes = [0; 8];
                assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
                assert_eq!(&bytes[..2], b"ab");
                assert_eq!(
                    peer.try_recv(&mut bytes).unwrap(),
                    0,
                    "sender closed without replaying partial bytes"
                );
                drop(completion);
                assert_eq!(drops.get(), 2);
                assert!(weak.upgrade().is_none());
            }
        }
    }

    #[test]
    fn retries_are_bounded_and_readiness_does_not_spin() {
        for errno in [libc::EINTR, libc::EAGAIN] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
            let request = scope();
            let (fd, _peer) = sim.socket_pair();
            for _ in 0..=DATA_RETRIES {
                sim.inject("send", Fault::Errno(errno)).unwrap();
            }
            let drops = Rc::new(Cell::new(0));
            let result = drive(
                &reactor,
                reactor.send(
                    Rc::new(fd),
                    Buffer(vec![1], drops.clone()),
                    Lease(drops.clone()),
                    &request,
                ),
            );
            assert!(matches!(result, Err(Error::Os(error)) if error == errno));
            assert_eq!(submissions(&sim, "send"), DATA_RETRIES + 1);
            assert_eq!(
                submissions(&sim, "poll"),
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
            let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
            let request = scope();
            let (fd, _peer) = sim.socket_pair();
            sim.inject("recv", Fault::Errno(libc::EAGAIN)).unwrap();
            let drops = Rc::new(Cell::new(0));
            let mut operation = reactor.recv(
                Rc::new(fd),
                Buffer(vec![0; 8], drops.clone()),
                Lease(drops.clone()),
                &request,
            );
            assert!(poll(&mut operation).is_pending());
            for _ in 0..20 {
                reactor.poll_budgeted(1).unwrap();
                assert!(poll(&mut operation).is_pending());
            }
            assert_eq!(submissions(&sim, "recv"), 1);
            assert_eq!(submissions(&sim, "poll"), 1);
            assert_eq!(drops.get(), 0);
            let mut operation = Some(operation);
            if abandon {
                drop(operation.take());
            } else {
                request.cancellation.cancel().unwrap();
            }
            reactor.poll_budgeted(1).unwrap(); // submit cancellation
            assert_eq!(drops.get(), 0);
            reactor.poll_budgeted(1).unwrap(); // first CQE is not the fence
            assert_eq!(drops.get(), 0);
            reactor.poll_budgeted(1).unwrap();
            assert_eq!(drops.get(), 2);
            if let Some(mut operation) = operation {
                assert!(matches!(
                    poll(&mut operation),
                    Poll::Ready(Err(Error::Cancelled))
                ));
            }
            assert_eq!(reactor.in_flight(), 0);
        }
    }

    #[test]
    fn cancellation_between_transient_completion_and_retry_prevents_resubmission() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        let request = scope();
        let (fd, _peer) = sim.socket_pair();
        sim.inject("send", Fault::Errno(libc::EINTR)).unwrap();
        let drops = Rc::new(Cell::new(0));
        let mut operation = reactor.send(
            Rc::new(fd),
            Buffer(vec![1], drops.clone()),
            Lease(drops.clone()),
            &request,
        );
        assert!(poll(&mut operation).is_pending());
        reactor.poll_budgeted(1).unwrap();
        reactor.poll_budgeted(1).unwrap();
        assert_eq!(drops.get(), 0);
        request.cancellation.cancel().unwrap();
        assert!(matches!(
            poll(&mut operation),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(submissions(&sim, "send"), 1);
        assert_eq!(drops.get(), 2);
    }

    #[test]
    fn recv_and_positioned_io_retry_without_changing_buffer_or_offset() {
        for operation in [
            BufferOperation::Recv,
            BufferOperation::Read(2),
            BufferOperation::Write(2),
        ] {
            for errno in [libc::EINTR, libc::EAGAIN] {
                let sim = Simulation::new();
                let _environment = sim.enter();
                let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
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
                    reactor.write_at(file.clone(), 0, buffer(b"01234567"), (), &request),
                )
                .unwrap();
                assert_eq!(written.bytes, 8);
                let (name, fd) = match operation {
                    BufferOperation::Recv => ("recv", Rc::new(socket)),
                    BufferOperation::Read(_) => ("read", file.clone()),
                    BufferOperation::Write(_) => ("write", file.clone()),
                    _ => unreachable!(),
                };
                sim.inject(name, Fault::Errno(errno)).unwrap();
                sim.inject(name, Fault::Short(2)).unwrap();
                let drops = Rc::new(Cell::new(0));
                let operation_future = reactor.buffer_io(
                    fd,
                    Buffer(b"ABCD".to_vec(), drops.clone()),
                    Lease(drops.clone()),
                    &request,
                    operation,
                    None,
                );
                // Exercise real simulated file readiness, not an injected CQE.
                let result = drive(&reactor, operation_future).unwrap();
                assert_eq!(result.bytes, 2);
                assert_eq!(drops.get(), 0);
                match operation {
                    BufferOperation::Recv => assert_eq!(&result.buffer.0[..2], b"ab"),
                    BufferOperation::Read(_) => assert_eq!(&result.buffer.0[..2], b"23"),
                    BufferOperation::Write(_) => {
                        let read = drive(
                            &reactor,
                            reactor.read_at(file.clone(), 0, buffer(&[0; 8]), (), &request),
                        )
                        .unwrap();
                        assert_eq!(&read.buffer.0, b"01AB4567");
                    }
                    _ => unreachable!(),
                }
                drop(result);
                assert_eq!(drops.get(), 2);
            }
        }
    }

    #[test]
    fn arbitrary_error_permutations_use_published_reserved_entries() {
        for accept in [false, true] {
            for cancel_first in [false, true] {
                let sim = Simulation::new();
                let _environment = sim.enter();
                let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
                let request = scope();
                let capacity = reactor.reserve_submissions(1, ()).unwrap();
                let (fd, _peer) = sim.socket_pair();
                let listener = sim
                    .listen(SocketAddress::Unix("/injected-fences".into()))
                    .unwrap();
                let drops = Rc::new(Cell::new(0));
                let mut operation: Operation<'_, ()> = if accept {
                    let future = reactor.accept_reserved(
                        Rc::new(listener),
                        Some(capacity.clone()),
                        &request,
                    );
                    Box::pin(async move { future.await.map(drop) })
                } else {
                    let future = reactor.recv_reserved(
                        Rc::new(fd),
                        Buffer(vec![0; 4], drops.clone()),
                        capacity.clone(),
                        &request,
                    );
                    Box::pin(async move { future.await.map(drop) })
                };
                assert!(poll(&mut operation).is_pending());
                {
                    let mut state = reactor.state.borrow_mut();
                    let (&id, entry) = state.entries.first_key_value().unwrap();
                    assert!(!entry.cancel_sent);
                    // Model an already-published cancellation whose two CQEs can
                    // report an original error and ENOENT in either order.
                    state.entries.get_mut(&id).unwrap().cancel_sent = true;
                    let driver = state.simulation.as_mut().unwrap();
                    driver.inject_completion(id.0, -libc::EIO).unwrap();
                    driver
                        .inject_completion(id.0 | CANCEL_BIT, -libc::ENOENT)
                        .unwrap();
                    if cancel_first {
                        driver.reorder_completions(0, 1).unwrap();
                    }
                }
                reactor.poll_budgeted(1).unwrap();
                assert_eq!(capacity.active.get(), 1);
                assert_eq!(reactor.in_flight(), 1);
                assert_eq!(drops.get(), 0);
                reactor.poll_budgeted(1).unwrap();
                assert_eq!(reactor.in_flight(), 0);
                assert_eq!(
                    capacity.active.get(),
                    1,
                    "unread error still owns reserved bookkeeping"
                );
                assert!(matches!(
                    poll(&mut operation),
                    Poll::Ready(Err(Error::Os(libc::EIO)))
                ));
                assert_eq!(capacity.active.get(), 0);
                assert_eq!(drops.get(), usize::from(!accept));
            }
        }
    }

    #[test]
    fn actual_reserved_entries_fence_original_and_cancel_in_both_orders() {
        for accept in [false, true] {
            for cancel_first in [false, true] {
                for abandon in [false, true] {
                    let sim = Simulation::new();
                    let _environment = sim.enter();
                    let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
                    let request = scope();
                    let charge = Rc::new(());
                    let charge_weak = Rc::downgrade(&charge);
                    let capacity = reactor.reserve_submissions(1, charge).unwrap();
                    let drops = Rc::new(Cell::new(0));
                    let (socket, peer) = sim.socket_pair();
                    let listener = sim.listen(SocketAddress::Unix("/fences".into())).unwrap();
                    let mut operation: Operation<'_, ()> = if accept {
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
                            Buffer(vec![0; 4], drops.clone()),
                            capacity.clone(),
                            &request,
                        );
                        Box::pin(async move { future.await.map(drop) })
                    };
                    assert!(poll(&mut operation).is_pending());
                    let mut operation = Some(operation);
                    if abandon {
                        drop(operation.take());
                    } else {
                        request.cancellation.cancel().unwrap();
                    }
                    reactor.poll_budgeted(1).unwrap(); // real cancellation on the published entry
                    if !cancel_first {
                        reactor
                            .state
                            .borrow_mut()
                            .simulation
                            .as_mut()
                            .unwrap()
                            .reorder_completions(0, 1)
                            .unwrap();
                    }
                    reactor.poll_budgeted(1).unwrap();
                    assert_eq!(reactor.in_flight(), 1);
                    assert_eq!(capacity.active.get(), 1);
                    assert_eq!(drops.get(), 0);
                    reactor.poll_budgeted(1).unwrap();
                    assert_eq!(reactor.in_flight(), 0);
                    assert_eq!(capacity.active.get(), usize::from(!abandon));
                    if let Some(mut operation) = operation {
                        assert!(matches!(
                            poll(&mut operation),
                            Poll::Ready(Err(Error::Cancelled))
                        ));
                    }
                    assert_eq!(capacity.active.get(), 0);
                    assert_eq!(drops.get(), usize::from(!accept));
                    drop((capacity, peer));
                    assert!(charge_weak.upgrade().is_none());
                }
            }
        }
    }

    #[test]
    fn successful_accept_is_reclaimed_only_after_both_cqes_when_abandoned() {
        for cancel_first in [false, true] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
            let request = scope();
            let capacity = reactor.reserve_submissions(1, ()).unwrap();
            let address = SocketAddress::Unix("/accepted-owner".into());
            let listener = Rc::new(sim.listen(address.clone()).unwrap());
            let mut accept =
                reactor.accept_reserved(listener.clone(), Some(capacity.clone()), &request);
            assert!(poll(&mut accept).is_pending());
            let client = sim.connect(address).unwrap();
            reactor.poll_budgeted(1).unwrap(); // execute accept, leave its CQE queued
            assert_eq!(sim.live_handles(), 3);
            {
                let mut state = reactor.state.borrow_mut();
                let id = *state.entries.first_key_value().unwrap().0;
                state.entries.get_mut(&id).unwrap().cancel_sent = true;
                let driver = state.simulation.as_mut().unwrap();
                driver
                    .inject_completion(id.0 | CANCEL_BIT, -libc::ENOENT)
                    .unwrap();
                if cancel_first {
                    driver.reorder_completions(0, 1).unwrap();
                }
            }
            drop(accept);
            reactor.poll_budgeted(1).unwrap();
            assert_eq!(
                sim.live_handles(),
                3,
                "accepted FD remains fenced after first CQE"
            );
            assert_eq!(capacity.active.get(), 1);
            reactor.poll_budgeted(1).unwrap();
            assert_eq!(sim.live_handles(), 2);
            assert_eq!(capacity.active.get(), 0);
            drop((client, listener));
            assert_eq!(sim.live_handles(), 0);
        }
    }

    #[test]
    fn reserved_retry_publication_rejection_drops_buffer_and_lease_once_and_reuses_slot() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        let request = scope();
        let capacity = reactor.reserve_submissions(1, ()).unwrap();
        let (socket, peer) = sim.socket_pair();
        let fd = Rc::new(socket);
        let drops = Rc::new(Cell::new(0));
        sim.inject("send", Fault::Errno(libc::EAGAIN)).unwrap();
        let mut operation = reactor.buffer_io(
            fd.clone(),
            Buffer(vec![1], drops.clone()),
            Lease(drops.clone()),
            &request,
            BufferOperation::Send,
            Some(capacity.clone()),
        );
        assert!(poll(&mut operation).is_pending());
        reactor.poll_budgeted(1).unwrap();
        reactor.poll_budgeted(1).unwrap();
        assert_eq!(capacity.active.get(), 1);
        assert_eq!(drops.get(), 0);
        sim.reject_submissions(1).unwrap(); // reject readiness, not the original data SQE
        assert!(matches!(
            poll(&mut operation),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(drops.get(), 2);
        assert_eq!(capacity.active.get(), 0);
        assert_eq!(reactor.in_flight(), 0);
        let result = drive(
            &reactor,
            reactor.send_reserved(fd, buffer(b"ok"), capacity.clone(), &request),
        )
        .unwrap();
        assert_eq!(result.bytes, 2);
        assert_eq!(capacity.active.get(), 0);
        let mut bytes = [0; 8];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
        assert_eq!(&bytes[..2], b"ok");
    }

    #[test]
    fn scope_destructor_reenters_reactor_after_completion_and_sq_rejection() {
        #[derive(Clone)]
        struct ReenterScope {
            reactor: Rc<RefCell<Weak<super::super::Reactor<ReenterScope, ()>>>>,
            drops: Rc<Cell<usize>>,
        }
        impl Scope for ReenterScope {
            type Error = Error;
            fn check(&self) -> Result<()> {
                Ok(())
            }
        }
        impl Drop for ReenterScope {
            fn drop(&mut self) {
                if let Some(reactor) = self.reactor.borrow().upgrade() {
                    assert!(reactor.state.try_borrow_mut().is_ok());
                    reactor.poll_budgeted(1).unwrap();
                }
                self.drops.set(self.drops.get() + 1);
            }
        }
        let sim = Simulation::new();
        let _environment = sim.enter();
        let reactor = Rc::new(super::super::Reactor::new(1, ()));
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
            let mut operation = reactor.send(fd.clone(), buffer(b"x"), (), &scope);
            if reject {
                assert!(matches!(
                    poll(&mut operation),
                    Poll::Ready(Err(Error::Overloaded))
                ));
            } else {
                assert!(poll(&mut operation).is_pending());
                reactor.poll_budgeted(1).unwrap();
                reactor.poll_budgeted(1).unwrap();
                assert!(matches!(poll(&mut operation), Poll::Ready(Ok(done)) if done.bytes == 1));
            }
            assert_eq!(drops.get(), if reject { 1 } else { 2 });
        }
        drop(scope);
        assert_eq!(drops.get(), 3);
    }
}

mod review_regressions {
    use super::*;

    #[cfg(feature = "simulation")]
    #[test]
    fn simulated_publication_rejection_rolls_back_owners_and_admission() {
        let sim = simulation::Simulation::new();
        let _environment = sim.enter();
        let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
        reactor.init().unwrap();
        let baseline = reactor.admission.used(ResourceClass::RequestContext);
        let request = scope();
        let (fd, peer) = sim.socket_pair();
        let fd = Rc::new(fd);
        let weak = Rc::downgrade(&fd);
        sim.reject_submissions(1).unwrap();
        assert!(matches!(
            drive(
                &reactor,
                reactor.send(fd, reactor.file_bytes(b"x").unwrap(), (), &request)
            ),
            Err(Error::Overloaded)
        ));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(reactor.ordinary.get(), 0);
        assert!(weak.upgrade().is_none());
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            baseline
        );
        let (fd, _) = sim.socket_pair();
        assert!(matches!(
            drive(
                &reactor,
                reactor.send(Rc::new(fd), reactor.file_bytes(b"x").unwrap(), (), &request)
            ),
            Err(Error::Os(libc::EPIPE))
        ));
        drop(peer);
    }

    #[test]
    fn positioned_io_rejects_kernel_sentinel_offsets_before_publication() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let request = scope();
        let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(fd));
        for offset in [i64::MAX as u64 + 1, u64::MAX] {
            assert!(matches!(
                drive(
                    &reactor,
                    reactor.read_at(
                        fd.clone(),
                        offset,
                        reactor.file_buffer(1).unwrap(),
                        (),
                        &request
                    )
                ),
                Err(Error::InvalidInput)
            ));
            assert!(matches!(
                drive(
                    &reactor,
                    reactor.write_at(
                        fd.clone(),
                        offset,
                        reactor.file_bytes(b"x").unwrap(),
                        (),
                        &request
                    )
                ),
                Err(Error::InvalidInput)
            ));
            assert_eq!(reactor.in_flight(), 0);
        }
    }

    thread_local! { static WAKER_CALLBACK: RefCell<Option<Box<dyn Fn()>>> = RefCell::new(None); }
    // No raw pointer ownership or non-Send data crosses threads. Each callback
    // inspects only the current test thread's optional probe.
    fn callback_waker() -> Waker {
        unsafe fn clone(_: *const ()) -> std::task::RawWaker {
            unsafe {
                wake(std::ptr::null());
            }
            std::task::RawWaker::new(std::ptr::null(), &VTABLE)
        }
        unsafe fn wake(_: *const ()) {
            WAKER_CALLBACK.with(|probe| {
                if let Some(callback) = &*probe.borrow() {
                    callback();
                }
            });
        }
        unsafe fn wake_ref(data: *const ()) {
            unsafe {
                wake(data);
            }
        }
        unsafe fn drop_waker(data: *const ()) {
            unsafe {
                wake(data);
            }
        }
        static VTABLE: std::task::RawWakerVTable =
            std::task::RawWakerVTable::new(clone, wake, wake_ref, drop_waker);
        // SAFETY: null data is never dereferenced; TLS callbacks are thread-safe.
        unsafe { Waker::from_raw(std::task::RawWaker::new(std::ptr::null(), &VTABLE)) }
    }

    #[test]
    fn fence_and_waiting_waker_clone_drop_and_wake_allow_reentry() {
        let reactor = Rc::new(Reactor::new(Rc::new(Admission::new(limits(2)))));
        reactor
            .state
            .borrow_mut()
            .entries
            .insert(IoId(1), entry(|_| None));
        let weak = Rc::downgrade(&reactor);
        let signal = Rc::new(Signal {
            abandoned: Cell::new(false),
            waker: RefCell::new(None),
        });
        let observed = signal.clone();
        WAKER_CALLBACK.with(|probe| {
            *probe.borrow_mut() = Some(Box::new(move || {
                assert!(weak.upgrade().unwrap().state.try_borrow_mut().is_ok());
                assert!(observed.waker.try_borrow_mut().is_ok());
            }))
        });
        let waker = callback_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fence = reactor.cancel_and_fence(IoId(1));
        assert!(fence.as_mut().poll(&mut cx).is_pending());
        assert!(fence.as_mut().poll(&mut cx).is_pending());
        drop(fence);
        let reply = Rc::new(RefCell::new(Reply::<(), Error> {
            _slot: None,
            result: None,
            _reservation: Rc::new(Box::new(())),
        }));
        let mut waiting = Waiting { reply, signal };
        assert!(Pin::new(&mut waiting).poll(&mut cx).is_pending());
        assert!(Pin::new(&mut waiting).poll(&mut cx).is_pending());
        drop(waiting);
        let completed = reactor.state.borrow_mut().complete(1, 0).unwrap().unwrap();
        completed.finish();
        waker.wake();
        WAKER_CALLBACK.with(|probe| probe.borrow_mut().take());
    }

    fn entry(
        finish: impl FnOnce(Result<KernelResult>) -> Option<Waker> + 'static,
    ) -> Entry<RequestScope> {
        Entry {
            finish: Box::new(finish),
            signal: Rc::new(Signal {
                abandoned: Cell::new(false),
                waker: RefCell::new(None),
            }),
            scope: Rc::new(scope()),
            original: None,
            accept: false,
            cancel_reason: None,
            cancel_sent: false,
            cancel_done: false,
        }
    }

    #[test]
    fn unknown_completion_and_fatal_submit_do_not_discard_prior_delivery() {
        for unknown in [false, true] {
            let reactor = Rc::new(Reactor::new(Rc::new(Admission::new(limits(4)))));
            let delivered = Rc::new(Cell::new(false));
            let observed = delivered.clone();
            let weak = Rc::downgrade(&reactor);
            let mut state = reactor.state.borrow_mut();
            state.ring_reservation = Some(Box::new(()));
            state.entries.insert(
                IoId(1),
                entry(move |result| {
                    assert_eq!(result.unwrap().value(), Ok(7));
                    assert_eq!(weak.upgrade().unwrap().in_flight(), 0);
                    observed.set(true);
                    None
                }),
            );
            state.completions.push_back((1, 7.into()));
            if unknown {
                state.completions.push_back((999, 0.into()));
            } else {
                state.submit_result = Some(Err(std::io::Error::from_raw_os_error(libc::EIO)));
            }
            drop(state);
            let counter = Arc::new(Count::default());
            let waker = Waker::from(counter.clone());
            let mut fence = reactor.cancel_and_fence(IoId(1));
            assert!(
                fence
                    .as_mut()
                    .poll(&mut Context::from_waker(&waker))
                    .is_pending()
            );
            // Test delivery rather than cancellation precedence.
            reactor
                .state
                .borrow_mut()
                .entries
                .get_mut(&IoId(1))
                .unwrap()
                .cancel_reason = None;
            assert_eq!(reactor.poll_budgeted(4), Err(Error::Io));
            assert!(delivered.get());
            assert_eq!(counter.0.load(Ordering::Relaxed), 1);
            assert!(poll(&mut fence).is_ready());
        }
    }

    #[test]
    fn panicking_finish_or_waker_does_not_discard_other_completions() {
        struct PanicWake;
        impl std::task::Wake for PanicWake {
            fn wake(self: Arc<Self>) {
                panic!("injected wake panic");
            }
        }
        struct PanicDrop;
        impl Drop for PanicDrop {
            fn drop(&mut self) {
                panic!("injected destructor panic");
            }
        }
        for destructor in [false, true] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(4))));
            let delivered = Rc::new(Cell::new(false));
            let output = delivered.clone();
            let mut state = reactor.state.borrow_mut();
            state.ring_reservation = Some(Box::new(()));
            state.entries.insert(
                IoId(1),
                entry(move |_| {
                    if destructor {
                        drop(PanicDrop);
                    }
                    Some(Waker::from(Arc::new(PanicWake)))
                }),
            );
            state.entries.insert(
                IoId(2),
                entry(move |_| {
                    output.set(true);
                    None
                }),
            );
            state.completions.extend([(1, 0.into()), (2, 0.into())]);
            drop(state);
            assert!(
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| reactor.poll_budgeted(4)))
                    .is_err()
            );
            assert!(delivered.get());
            assert_eq!(reactor.in_flight(), 0);
        }
    }

    #[test]
    fn shutdown_timeout_and_drop_never_release_missing_original_owner() {
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        let drops = Rc::new(Cell::new(0));
        let lease = Lease(drops.clone());
        let mut pending = entry(move |_| {
            drop(lease);
            None
        });
        pending.cancel_sent = true;
        pending.cancel_done = true;
        reactor.state.borrow_mut().entries.insert(IoId(1), pending);
        assert_eq!(
            reactor.shutdown(Duration::ZERO),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(reactor.in_flight(), 1);
        assert_eq!(drops.get(), 0);
        let start = Instant::now();
        drop(reactor);
        assert!(start.elapsed() < Duration::from_secs(2));
        assert_eq!(drops.get(), 0, "elapsed time is never a fence");
    }

    #[test]
    fn fatal_drop_retains_ring_and_unfenced_resources() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let request = scope();
        let (fd, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(fd));
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let mut receive = reactor.recv(
            fd,
            Buffer(vec![0], drops.clone()),
            Lease(drops.clone()),
            &request,
        );
        assert!(poll(&mut receive).is_pending());
        drop(receive);
        let ring_fd = reactor.state.borrow().ring.as_ref().unwrap().as_raw_fd();
        let admission = reactor.admission.clone();
        let charged = admission.used(ResourceClass::RequestContext);
        reactor.state.borrow_mut().submit_result =
            Some(Err(std::io::Error::from_raw_os_error(libc::EIO)));
        drop(reactor);
        assert_eq!(drops.get(), 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(admission.used(ResourceClass::RequestContext), charged);
        // SAFETY: F_GETFD only queries; leaked ring must still own this descriptor.
        assert!(unsafe { libc::fcntl(ring_fd, libc::F_GETFD) } >= 0);
    }

    #[test]
    fn selective_fence_cancels_whole_snapshot_before_first_wait() {
        let reactor = Reactor::new(Rc::new(Admission::new(limits(4))));
        for id in 1..=3 {
            let mut pending = entry(|_| None);
            if id == 3 {
                pending.scope = Rc::new(RequestScope {
                    deadline: Deadline(Instant::now()),
                    ..scope()
                });
            }
            reactor.state.borrow_mut().entries.insert(IoId(id), pending);
        }
        let mut fence = reactor.fence_matching(|scope| {
            assert_eq!(reactor.in_flight(), 3, "predicate can reenter");
            scope.deadline.0 > Instant::now()
        });
        assert!(poll(&mut fence).is_pending());
        {
            let state = reactor.state.borrow();
            assert!(state.entries[&IoId(1)].cancel_reason.is_some());
            assert!(state.entries[&IoId(2)].cancel_reason.is_some());
            assert!(state.entries[&IoId(3)].cancel_reason.is_none());
        }
        for id in 1..=3 {
            let completed = reactor.state.borrow_mut().complete(id, 0).unwrap().unwrap();
            completed.finish();
        }
        assert_eq!(poll(&mut fence), Poll::Ready(Ok(())));
    }

    #[test]
    fn exhausted_operation_and_waiter_ids_fail_without_wrapping() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        reactor.state.borrow_mut().next = CANCEL_BIT;
        assert!(matches!(
            reactor.submit(
                Submission::Real(opcode::Nop::new().build()),
                &scope(),
                false,
                |result| result?.value()
            ),
            Err(Error::Overloaded)
        ));
        assert_eq!(reactor.in_flight(), 0);
        reactor
            .state
            .borrow_mut()
            .entries
            .insert(IoId(1), entry(|_| None));
        reactor.state.borrow_mut().next_waiter = u64::MAX;
        assert_eq!(
            poll(&mut reactor.cancel_and_fence(IoId(1))),
            Poll::Ready(Err(Error::Overloaded))
        );
        let completed = reactor.state.borrow_mut().complete(1, 0).unwrap().unwrap();
        completed.finish();
    }

    #[test]
    fn cancellation_scan_rotates_beyond_a_single_budget() {
        let Some(reactor) = kernel_reactor(8) else {
            return;
        };
        let request = scope();
        let mut receives = Vec::new();
        let mut peers = Vec::new();
        for _ in 0..7 {
            let (fd, peer) = UnixStream::pair().unwrap();
            let mut receive = reactor.recv(Rc::new(fd.into()), buffer(&[0]), (), &request);
            assert!(poll(&mut receive).is_pending());
            receives.push(receive);
            peers.push(peer);
        }
        request.cancel().unwrap();
        for _ in 0..4 {
            reactor.poll_budgeted(2).unwrap();
        }
        assert!(
            reactor
                .state
                .borrow()
                .entries
                .values()
                .all(|e| e.cancel_sent)
        );
        drive(&reactor, reactor.drain()).unwrap();
        for mut receive in receives {
            assert!(matches!(
                poll(&mut receive),
                Poll::Ready(Err(Error::Cancelled))
            ));
        }
    }

    #[cfg(feature = "simulation")]
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
        impl Scope for ReentrantScope {
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
        let sim = simulation::Simulation::new();
        let _sim = sim.enter();
        let callback: Callback = Rc::default();
        let reactor = Rc::new(super::super::Reactor::new(
            4,
            ReentrantBudget(callback.clone()),
        ));
        let weak = Rc::downgrade(&reactor);
        *callback.borrow_mut() = Some(Box::new(move || {
            let reactor = weak.upgrade().unwrap();
            assert!(reactor.state.try_borrow_mut().is_ok());
        }));
        let request = ReentrantScope(callback);
        let (fd, peer) = sim.socket_pair();
        let mut receive = reactor.recv(Rc::new(fd), buffer(&[0]), (), &request);
        assert!(poll(&mut receive).is_pending());
        reactor.poll_budgeted(1).unwrap();
        peer.try_send(b"x").unwrap();
        for _ in 0..4 {
            reactor.poll_budgeted(1).unwrap();
        }
        assert!(matches!(poll(&mut receive), Poll::Ready(Ok(_))));
    }
}
