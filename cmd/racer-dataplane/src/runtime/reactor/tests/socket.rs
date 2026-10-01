//! Socket scenarios exercise ownership through the reactor's public operations.
use super::*;

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
