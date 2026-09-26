use super::*;
use crate::model::identity::RequestId;
use std::{
    num::NonZeroUsize,
    os::unix::net::UnixStream,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Wake},
};

struct Count(AtomicUsize);

#[test]
fn completed_incoming_keepalives_do_not_block_outbound_progress() {
    use crate::http::{
        codec::{Codec, Header, MessageHead, StartLine},
        io::HttpIo,
    };
    use std::{
        io::Write,
        net::{TcpListener, TcpStream},
    };
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.client_connections = NonZeroUsize::new(2).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let pool = HttpPool::new(reactor.clone(), admission.clone(), 2);
    let io = HttpIo::with_admission(reactor.clone(), Codec::new(32768, 1024), admission.clone());
    let scope =
        RequestScope::new(RequestId([71; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
    let drive = |mut work: Operation<'_, ConnectionLease>| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        loop {
            if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                break result;
            }
            scope.check().unwrap();
            reactor.poll_budgeted(64).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    };
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let mut peers = Vec::new();
    let mut idle = Vec::new();
    for _ in 0..2 {
        let mut peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (socket, _) = listener.accept().unwrap();
        let connection = pool.accept(socket.into()).unwrap();
        peer.write_all(b"GET / HTTP/1.1\r\nHost: fixture\r\ncontent-length: 0\r\n\r\n")
            .unwrap();
        let connection = drive(Box::pin(async {
            let request = io.receive_head(connection, &scope).await?;
            let head = MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![Header {
                    name: "content-length".into(),
                    value: b"0".to_vec(),
                }],
            };
            let mut connection = io
                .send_head(request.connection, head, &scope)
                .await?
                .connection;
            connection.finish_exchange()?;
            Ok(connection)
        }))
        .unwrap();
        assert!(connection.is_reusable());
        idle.push(connection);
        peers.push(peer);
    }
    assert_eq!(admission.used(ResourceClass::Connection), 2);
    let idle_scope = RequestScope::new(RequestId([72; 16]), scope.deadline.0).unwrap();
    let mut waiting: Vec<_> = idle
        .into_iter()
        .map(|connection| Some(io.receive_peer_head(connection, &pool, &idle_scope)))
        .collect();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for work in &mut waiting {
        assert!(work.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
    }
    reactor.poll_budgeted(64).unwrap();
    let destination = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(destination.local_addr().unwrap().to_string());
    let mut checkout = pool.checkout_peer(&endpoint, &scope);
    let result = drive(Box::pin(std::future::poll_fn(|cx| {
        for work in &mut waiting {
            if let Some(wait) = work
                && let Poll::Ready(result) = wait.as_mut().poll(cx)
            {
                assert!(matches!(result, Err(Error::Cancelled)));
                *work = None;
            }
        }
        pool.poll_peer_waiters();
        checkout.as_mut().poll(cx)
    })));
    let error = result.as_ref().err().copied();
    drop(result);
    drop(checkout);
    idle_scope.cancel().unwrap();
    drop(waiting);
    let mut drain = reactor.drain();
    while drain.as_mut().poll(&mut cx).is_pending() {
        scope.check().unwrap();
        reactor.poll_budgeted(64).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    drop(peers);
    pool.close();
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(
        error, None,
        "completed incoming HTTP keepalives blocked outbound progress"
    );
}
impl Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

#[test]
fn incoming_idle_preserves_partial_heads_and_fences_cancel_races() {
    use crate::http::{codec::Codec, io::HttpIo};
    use std::io::Write;
    for mode in [
        "active",
        "partial",
        "ready",
        "cancel",
        "drop",
        "deadline",
        "arrival-after-reclaim",
    ] {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.client_connections = NonZeroUsize::new(1).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let io =
            HttpIo::with_admission(reactor.clone(), Codec::new(32768, 1024), admission.clone());
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let mut connection = pool.accept(socket.into()).unwrap();
        if mode != "active" {
            connection.rx_remaining = Some(0);
            connection.tx_remaining = Some(0);
            connection.finish_exchange().unwrap();
        }
        let scope = RequestScope::new(
            RequestId([73; 16]),
            Instant::now() + Duration::from_millis(if mode == "deadline" { 10 } else { 5000 }),
        )
        .unwrap();
        let mut work = io.receive_peer_head(connection, &pool, &scope);
        let wakes = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(64).unwrap();
        match mode {
            "active" => assert!(!pool.reclaim_incoming_idle().unwrap()),
            "partial" | "ready" => {
                peer.write_all(b"GET / HTTP/1.1\r\n").unwrap();
                // Bytes queued before reclaim are protected even before the
                // readiness completion is reaped by its owning task.
                assert!(!pool.reclaim_incoming_idle().unwrap());
                reactor.poll_budgeted(64).unwrap();
                assert!(work.as_mut().poll(&mut cx).is_pending());
                if mode == "ready" {
                    peer.write_all(b"Host: fixture\r\ncontent-length: 0\r\n\r\n")
                        .unwrap();
                    loop {
                        reactor.poll_budgeted(64).unwrap();
                        if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                            let head = result.unwrap();
                            assert_eq!(head.connection.remaining_body(), Some(0));
                            assert!(!pool.reclaim_incoming_idle().unwrap());
                            drop(head);
                            break;
                        }
                        scope.check().unwrap();
                        reactor.wait(Duration::from_millis(1)).unwrap();
                    }
                } else {
                    assert!(!pool.reclaim_incoming_idle().unwrap());
                }
            }
            "cancel" => {
                let before = wakes.0.load(Ordering::Relaxed);
                scope.cancel().unwrap();
                assert!(
                    wakes.0.load(Ordering::Relaxed) > before,
                    "parent cancellation must wake idle owner"
                );
            }
            "deadline" => std::thread::sleep(Duration::from_millis(15)),
            "arrival-after-reclaim" => {
                assert!(pool.reclaim_incoming_idle().unwrap());
                peer.write_all(b"GET / HTTP/1.1\r\n").unwrap();
            }
            "drop" => (),
            _ => unreachable!(),
        }
        if mode != "ready" {
            assert_eq!(
                admission.used(ResourceClass::Connection),
                1,
                "{mode}: charge before fence"
            );
        }
        if matches!(mode, "cancel" | "deadline" | "arrival-after-reclaim") {
            let until = Instant::now() + Duration::from_secs(2);
            loop {
                reactor.poll_budgeted(64).unwrap();
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    assert!(
                        matches!(result, Err(Error::Cancelled | Error::DeadlineExceeded)),
                        "{mode}"
                    );
                    break;
                }
                assert!(Instant::now() < until);
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        }
        drop(work);
        let mut drain = reactor.drain();
        let until = Instant::now() + Duration::from_secs(2);
        loop {
            reactor.poll_budgeted(64).unwrap();
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            assert!(Instant::now() < until);
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(
            admission.used(ResourceClass::Connection),
            0,
            "{mode}: fenced release"
        );
        assert!(!pool.reclaim_incoming_idle().unwrap());
    }
}

#[test]
fn peer_slot_wait_is_bounded_cancelable_and_completion_owned() {
    for outcome in ["release", "cancel", "drop", "close"] {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.queue_entries = NonZeroUsize::new(1).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        pool.state.borrow_mut().entries.insert(
            endpoint.clone(),
            Entry {
                active: 1,
                generation: 1,
                idle: vec![],
            },
        );
        let (socket, _peer) = UnixStream::pair().unwrap();
        let active = ConnectionLease::new(
            Rc::new(socket.into()),
            admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap(),
            Some(ReturnToPool {
                state: Rc::downgrade(&pool.state),
                endpoint: endpoint.clone(),
                generation: 1,
            }),
        );
        let scope =
            RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut work = pool.checkout_peer(&endpoint, &scope);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert_eq!(count.0.load(Ordering::Relaxed), 0, "no self-wake spin");
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(admission.used(ResourceClass::Waiter), 1);
        assert_eq!(pool.state.borrow().waiters.len(), 1);
        assert!(matches!(
            futures::executor::block_on(pool.checkout_peer(&endpoint, &scope)),
            Err(Error::Overloaded)
        ));
        assert!(
            matches!(
                futures::executor::block_on(pool.checkout(&endpoint, &scope)),
                Err(Error::Overloaded)
            ),
            "fail-fast API retained"
        );
        let active = if outcome == "release" {
            drop(active);
            None
        } else {
            Some(active)
        };
        match outcome {
            "cancel" => scope.cancel().unwrap(),
            "close" => pool.close(),
            _ => {}
        }
        if outcome != "drop" {
            assert!(count.0.load(Ordering::Relaxed) > 0, "{outcome}");
        }
        if outcome == "release" {
            let connection = loop {
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    break result.unwrap();
                }
                scope.check().unwrap();
                reactor.poll_budgeted(32).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
            };
            assert_eq!(admission.used(ResourceClass::Connection), 1);
            drop(connection);
        } else if matches!(outcome, "cancel" | "close") {
            let expected = if outcome == "cancel" {
                Error::Cancelled
            } else {
                Error::Unavailable
            };
            assert!(
                matches!(work.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
            );
        }
        drop(work);
        assert_eq!(admission.used(ResourceClass::Waiter), 0);
        assert!(pool.state.borrow().waiters.is_empty());
        drop(active);
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}

#[test]
fn global_idle_fence_wait_cancel_drop_close_deadline_and_release() {
    use crate::http::{codec::Codec, io::HttpIo};
    for mode in ["cancel", "drop", "close", "deadline", "release"] {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.client_connections = NonZeroUsize::new(1).unwrap();
        let admission = Rc::new(Admission::new(limits));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let context_baseline = admission.used(ResourceClass::RequestContext);
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let io =
            HttpIo::with_admission(reactor.clone(), Codec::new(32768, 1024), admission.clone());
        let (socket, _peer) = UnixStream::pair().unwrap();
        let mut connection = pool.accept(socket.into()).unwrap();
        connection.rx_remaining = Some(0);
        connection.tx_remaining = Some(0);
        connection.finish_exchange().unwrap();
        let idle_scope =
            RequestScope::new(RequestId([80; 16]), Instant::now() + Duration::from_secs(5))
                .unwrap();
        let mut idle = io.receive_peer_head(connection, &pool, &idle_scope);
        let wakes = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(wakes.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(idle.as_mut().poll(&mut cx).is_pending());
        reactor.poll_budgeted(64).unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let scope = RequestScope::new(
            RequestId([81; 16]),
            Instant::now() + Duration::from_millis(if mode == "deadline" { 10 } else { 5000 }),
        )
        .unwrap();
        let mut checkout = pool.checkout_peer(&endpoint, &scope);
        assert!(checkout.as_mut().poll(&mut cx).is_pending());
        assert_eq!(admission.used(ResourceClass::Waiter), 1);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(pool.state.borrow().entries[&endpoint].active, 1);
        assert!(
            pool.state
                .borrow()
                .waiters
                .values()
                .all(|waiter| waiter.global_capacity)
        );
        let before = wakes.0.load(Ordering::Relaxed);
        pool.poll_peer_waiters();
        assert_eq!(
            wakes.0.load(Ordering::Relaxed),
            before,
            "no spin before fence"
        );
        match mode {
            "cancel" => scope.cancel().unwrap(),
            "close" => pool.close(),
            "deadline" => {
                std::thread::sleep(Duration::from_millis(15));
                pool.poll_peer_waiters();
            }
            _ => (),
        }
        if matches!(mode, "cancel" | "close" | "deadline") {
            assert!(
                wakes.0.load(Ordering::Relaxed) > before,
                "{mode}: waiter wake"
            );
            let expected = match mode {
                "cancel" => Error::Cancelled,
                "close" => Error::Unavailable,
                _ => Error::DeadlineExceeded,
            };
            assert!(
                matches!(checkout.as_mut().poll(&mut cx),Poll::Ready(Err(error)) if error==expected)
            );
        }
        if mode == "release" {
            let until = Instant::now() + Duration::from_secs(2);
            loop {
                reactor.poll_budgeted(64).unwrap();
                if idle.as_mut().poll(&mut cx).is_ready() {
                    break;
                }
                assert!(Instant::now() < until);
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            let before = wakes.0.load(Ordering::Relaxed);
            pool.poll_peer_waiters();
            assert!(
                wakes.0.load(Ordering::Relaxed) > before,
                "fenced release wakes global waiter"
            );
            loop {
                reactor.poll_budgeted(64).unwrap();
                if let Poll::Ready(result) = checkout.as_mut().poll(&mut cx) {
                    drop(result.unwrap());
                    break;
                }
                assert!(Instant::now() < until);
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
        } else {
            assert_eq!(
                admission.used(ResourceClass::Connection),
                1,
                "{mode}: idle connection retained before fence"
            );
        }
        drop(checkout);
        assert_eq!(admission.used(ResourceClass::Waiter), 0);
        assert!(pool.state.borrow().waiters.is_empty());
        assert!(pool.state.borrow().entries.is_empty());
        drop(idle);
        let mut drain = reactor.drain();
        let until = Instant::now() + Duration::from_secs(2);
        loop {
            reactor.poll_budgeted(64).unwrap();
            if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
                result.unwrap();
                break;
            }
            assert!(Instant::now() < until);
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        drop(drain);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            context_baseline
        );
    }
}

#[test]
fn peer_wait_expires_without_a_connection_release() {
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let pool = HttpPool::new(
        Rc::new(Reactor::new(admission.clone())),
        admission.clone(),
        1,
    );
    let endpoint = Endpoint::Peer("127.0.0.1:1".into());
    pool.state.borrow_mut().entries.insert(
        endpoint.clone(),
        Entry {
            active: 1,
            ..Entry::default()
        },
    );
    let scope = RequestScope::new(
        RequestId([8; 16]),
        Instant::now() + Duration::from_millis(20),
    )
    .unwrap();
    let mut work = pool.checkout_peer(&endpoint, &scope);
    let count = Arc::new(Count(AtomicUsize::new(0)));
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    assert!(work.as_mut().poll(&mut cx).is_pending());
    std::thread::sleep(Duration::from_millis(25));
    pool.poll_peer_waiters();
    assert_eq!(count.0.load(Ordering::Relaxed), 1);
    assert!(matches!(
        work.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::DeadlineExceeded))
    ));
    drop(work);
    assert!(pool.state.borrow().waiters.is_empty());
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    assert_eq!(admission.used(ResourceClass::RequestContext), 0);
}

#[test]
fn peer_wait_does_not_reuse_an_abandoned_connection_before_its_fence() {
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
    let endpoint = Endpoint::Peer("127.0.0.1:1".into());
    pool.state.borrow_mut().entries.insert(
        endpoint.clone(),
        Entry {
            active: 1,
            generation: 1,
            idle: vec![],
        },
    );
    let (socket, _peer) = UnixStream::pair().unwrap();
    socket.set_nonblocking(true).unwrap();
    let connection = ConnectionLease::new(
        Rc::new(socket.into()),
        admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap(),
        Some(ReturnToPool {
            state: Rc::downgrade(&pool.state),
            endpoint: endpoint.clone(),
            generation: 1,
        }),
    );
    let scope =
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
    let count = Arc::new(Count(AtomicUsize::new(0)));
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let mut receive = reactor.recv(
        connection.socket(),
        OwnedBuffer::new(&admission, 1).unwrap(),
        connection,
        &scope,
    );
    assert!(receive.as_mut().poll(&mut cx).is_pending());
    let mut waiting = pool.checkout_peer(&endpoint, &scope);
    assert!(waiting.as_mut().poll(&mut cx).is_pending());
    drop(receive);
    assert_eq!(count.0.load(Ordering::Relaxed), 0);
    assert_eq!(pool.state.borrow().entries[&endpoint].active, 1);
    assert_eq!(admission.used(ResourceClass::Connection), 1);
    while reactor.in_flight() != 0 {
        scope.check().unwrap();
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert!(count.0.load(Ordering::Relaxed) > 0);
    assert!(!pool.state.borrow().entries.contains_key(&endpoint));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    drop(waiting);
    assert_eq!(admission.used(ResourceClass::Waiter), 0);
    assert!(pool.state.borrow().waiters.is_empty());
}
