use super::*;
mod completion {
    use super::*;

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
                    scope: scope(),
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
                scope: scope(),
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
                buffer: Buffer(vec![0; 32].into(), drops.clone()),
                lease: Lease(drops.clone()),
            };
            let signal = Rc::new(Signal {
                abandoned: Cell::new(false),
                waker: RefCell::new(None),
            });
            let reservation = Rc::new(
                reactor
                    .admission
                    .reserve(None, ResourceClass::RequestContext, 64)
                    .unwrap(),
            );
            let reply = Rc::new(RefCell::new(Reply::<()> {
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
                    scope: scope(),
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
                scope: scope(),
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
                    scope: scope(),
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
                Some(if result >= 0 { Ok(3) } else { Err(Error::Io) })
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
            Buffer(vec![0; 1].into(), drops.clone()),
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
            Buffer(vec![0; 1].into(), drops.clone()),
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

    fn reserve(admission: &Admission) -> (Reservation, Reservation) {
        (
            admission
                .reserve(None, ResourceClass::ControlProgress, 1)
                .unwrap(),
            admission
                .reserve(None, ResourceClass::RequestContext, SUBMISSION_BYTES)
                .unwrap(),
        )
    }

    #[test]
    fn provenance_capacity_and_completion_ownership() {
        let reactor = kernel_reactor(2).expect("real io_uring reserved submissions");
        let request = scope();
        let foreign = Admission::new(limits(2));
        let (slots, memory) = reserve(&foreign);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(foreign.used(ResourceClass::ControlProgress), 0);
        assert_eq!(foreign.used(ResourceClass::RequestContext), 0);
        let (slots, memory) = reserve(&reactor.admission);
        let capacity = reactor.reserve_submissions(slots, memory).unwrap();
        let (slots, memory) = reserve(&reactor.admission);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::InvalidConfiguration)
        ));
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut ordinary = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
        assert!(poll(&mut ordinary).is_pending());
        let mut overflow = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
        assert!(matches!(
            poll(&mut overflow),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let mut send = reactor.send_reserved(fd.clone(), buffer(b"x"), capacity.clone(), &request);
        assert!(poll(&mut send).is_pending());
        let deadline = Instant::now() + Duration::from_secs(2);
        while reactor.in_flight() != 1 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        // A completed but unconsumed result still owns its reserved bookkeeping.
        assert_eq!(capacity.active.get(), 1);
        let mut excess =
            reactor.send_reserved(fd.clone(), buffer(b"y"), capacity.clone(), &request);
        assert!(matches!(
            poll(&mut excess),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(matches!(poll(&mut send), Poll::Ready(Ok(_))));
        drop(send);
        assert_eq!(capacity.active.get(), 0);
        let mut receive = reactor.recv_reserved(fd, buffer(&[0]), capacity.clone(), &request);
        assert!(poll(&mut receive).is_pending());
        let weak = Rc::downgrade(&capacity);
        drop((receive, capacity, ordinary));
        assert!(
            weak.upgrade().is_some(),
            "abandoned I/O must retain the partition"
        );
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 1);
        drive(&reactor, reactor.drain()).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
        use std::io::Read;
        let mut byte = [0];
        peer.read_exact(&mut byte).unwrap();
        assert_eq!(&byte, b"x");
        let (slots, memory) = reserve(&reactor.admission);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::Unavailable)
        ));
    }

    #[test]
    fn attachment_cannot_overcommit_existing_ordinary_entries() {
        let reactor = kernel_reactor(2).expect("real io_uring reserved attachment");
        let request = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut first = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
        let mut second = reactor.readiness(fd, libc::POLLIN as u32, &request);
        assert!(poll(&mut first).is_pending());
        assert!(poll(&mut second).is_pending());
        let (slots, memory) = reserve(&reactor.admission);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
        drop((first, second));
        drive(&reactor, reactor.drain()).unwrap();
    }

    #[test]
    fn ordinary_completed_reply_does_not_hold_queue_capacity() {
        let reactor = kernel_reactor(1).expect("real io_uring ordinary completion");
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
            Buffer(vec![0; 1].into(), drops.clone()),
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
        let reactor = Reactor::new(Rc::new(Admission::new(limits(8))));
        reactor.init().expect("real io_uring required");
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
        let mut accept =
            crate::runtime::retry_listener(&scope, || reactor.accept(listener.clone(), &scope));
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
            Buffer(vec![0; 32].into(), drops.clone()),
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
    fn real_drain_io_preserves_control_capacity_after_admission_stop() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
        let control = reactor
            .admission
            .reserve(None, ResourceClass::ControlProgress, 2)
            .unwrap();
        reactor.admission.stop();
        let scope = scope();
        let (left, right) = UnixStream::pair().unwrap();
        let left = Rc::new(Descriptor::from(left));
        let right = Rc::new(Descriptor::from(right));
        drive(&reactor, reactor.send(left, buffer(b"drain"), (), &scope)).unwrap();
        let read = drive(
            &reactor,
            reactor.recv(right.clone(), buffer(&[0; 8]), (), &scope),
        )
        .unwrap();
        assert_eq!(&read.buffer.bytes().unwrap()[..read.bytes], b"drain");
        let mut pending = reactor.readiness(right, libc::POLLOUT as u32, &scope);
        assert!(poll(&mut pending).is_pending());
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.in_flight(), 0);
        assert!(matches!(
            poll(&mut pending),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(reactor.init(), Err(Error::Unavailable));
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 2);
        drop(control);
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
            Err(Error::Io)
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
            let expected = if cancel { Error::Cancelled } else { Error::Io };
            assert!(matches!(poll(&mut connect), Poll::Ready(Err(error)) if error == expected));
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
use crate::{
    model::{Limits, RequestId},
    runtime::deadline::{Cancellation, Deadline},
};
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
impl sealed::Sealed for Buffer {}
impl IoBuffer for Buffer {
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
        assert_eq!(result.value(), Err(Error::Io));
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
            Err(Error::InvalidRequest)
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

#[test]
fn movable_production_buffers_preserve_subrange_through_completion() {
    use crate::{
        http::connection::{BufferRange, OwnedBuffer},
        memory::pool::BufferPool,
        peer::transport::WireBuffer,
    };
    fn check<B: IoBuffer>(buffer: B) {
        let mut range = BufferRange::new(buffer, 1..2).unwrap();
        let ptr = range.bytes_mut().unwrap().as_mut_ptr();
        let finish: Box<dyn FnOnce() -> B> = Box::new(move || range.into_inner());
        // SAFETY: completion owns the fixed backing until after this write.
        unsafe { ptr.write(7) };
        let buffer = finish();
        assert_eq!(buffer.bytes().unwrap(), &[0, 7, 0]);
    }
    let admission = Rc::new(Admission::new(limits(4)));
    check(OwnedBuffer::new(&admission, 3).unwrap());
    check(WireBuffer::new(&admission, 3).unwrap());
    let pool = BufferPool::new(admission.clone());
    check(
        pool.plaintext(
            admission
                .reserve(
                    Some(&crate::model::CacheId("provenance".into())),
                    ResourceClass::Plaintext,
                    3,
                )
                .unwrap(),
            3,
        )
        .unwrap(),
    );
    let reactor = Reactor::new(admission);
    check(reactor.file_buffer(3).unwrap());
}
fn limits(capacity: usize) -> Limits {
    let n = NonZeroUsize::new(1024 * 1024).unwrap();
    Limits {
        plaintext_bytes: n,
        ciphertext_bytes: n,
        dirty_bytes: n,
        registered_bytes: n,
        request_context_bytes: n,
        flights: n,
        waiters_per_flight: n,
        queue_entries: NonZeroUsize::new(capacity).unwrap(),
        connections_per_neighbor: n,
        client_connections: n,
        pipes: n,
        range_window_pages: n,
        header_bytes: n,
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    }
}
pub(super) fn scope() -> RequestScope {
    RequestScope {
        body_deadlines: None,
        request: RequestId([0; 16]),
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
            eprintln!("io_uring kernel test unavailable: {error}");
            return None;
        }
        Err(error) => panic!("unexpected io_uring setup failure: {error}"),
    }
    let reactor = Reactor::new(Rc::new(Admission::new(limits(capacity))));
    reactor.init().unwrap();
    Some(reactor)
}
