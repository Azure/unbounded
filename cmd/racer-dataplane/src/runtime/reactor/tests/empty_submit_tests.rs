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
                    let skip = matches!(flags, 0 | SETUP_CQSIZE) && empty && !overflow && !taskrun;
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
