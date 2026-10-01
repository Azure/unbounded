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
    let path = PathBuf::from("target").join(format!("reactor-unix-{}.sock", std::process::id()));
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
