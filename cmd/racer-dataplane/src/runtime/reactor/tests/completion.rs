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
