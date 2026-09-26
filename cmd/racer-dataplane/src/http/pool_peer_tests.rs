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
impl Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
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
