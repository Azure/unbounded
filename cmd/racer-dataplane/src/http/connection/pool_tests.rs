//! Real checkout/exchange fixtures for pool queueing, reuse, expiry, and fences.
use super::*;
use crate::http::{Codec, Header, MessageHead, StartLine};
use crate::{model::RequestId, test_support::WakeCounter};
use std::{sync::Arc, task::Context};

enum Listener {
    Tcp(std::net::TcpListener),
    Unix(std::os::unix::net::UnixListener, PathBuf),
}
impl Listener {
    fn peer() -> (Self, Endpoint) {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        listener.set_nonblocking(true).unwrap();
        (Self::Tcp(listener), endpoint)
    }
    fn origin() -> (Self, Endpoint) {
        static NEXT: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
        let path = PathBuf::from(format!(
            "pool-test-{}-{}.sock",
            std::process::id(),
            NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
        ));
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        listener.set_nonblocking(true).unwrap();
        (Self::Unix(listener, path.clone()), Endpoint::Unix(path))
    }
    fn accept(&self) -> Option<Descriptor> {
        let result = match self {
            Self::Tcp(listener) => listener.accept().map(|(socket, _)| socket.into()),
            Self::Unix(listener, path) => {
                assert!(path.exists());
                listener.accept().map(|(socket, _)| socket.into())
            }
        };
        match result {
            Ok(socket) => Some(socket),
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => None,
            Err(error) => panic!("accept: {error}"),
        }
    }
}
impl Drop for Listener {
    fn drop(&mut self) {
        if let Self::Unix(_, path) = self {
            std::fs::remove_file(path).unwrap();
        }
    }
}
use super::io_tests::drive;
fn scope() -> RequestScope {
    RequestScope::new(
        RequestId([7; 16]),
        crate::runtime::environment::now() + Duration::from_secs(5),
    )
    .unwrap()
}
fn setup() -> (Rc<Admission>, Rc<Reactor>, HttpPool) {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.queue_entries = std::num::NonZeroUsize::new(2).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    reactor.init().unwrap();
    let pool = HttpPool::new(reactor.clone(), admission.clone(), 1).with_origin_limit(2);
    (admission, reactor, pool)
}
// Hold a real checked-out connection only after a complete head/body exchange.
fn held(
    pool: &HttpPool,
    endpoint: &Endpoint,
    listener: &Listener,
) -> (ConnectionLease, ConnectionLease) {
    let scope = scope();
    let io = HttpIo::with_admission(
        pool.reactor.clone(),
        Codec::new(4096, 16),
        pool.admission.clone(),
    );
    let client = async {
        let connection = pool.checkout_metadata(endpoint, &scope).await?;
        let request = MessageHead {
            start: StartLine::Request {
                method: "GET".into(),
                target: "/pool".into(),
            },
            headers: vec![Header {
                name: "content-length".into(),
                value: b"0".to_vec(),
            }],
        };
        let sent = io.send_head(connection, request, &scope).await?;
        let received = io.receive_head(sent.connection, &scope).await?;
        assert!(matches!(
            received.value.start,
            StartLine::Response { status: 200 }
        ));
        let read = io
            .read_body(received.connection, io.buffer(1)?, &scope)
            .await?;
        assert_eq!(read.bytes, 1);
        assert_eq!(read.buffer.bytes()?, b"x");
        let mut connection = read.lease;
        connection.finish_exchange()?;
        Ok::<_, Error>(connection)
    };
    let server = async {
        let socket =
            std::future::poll_fn(|_| listener.accept().map_or(Poll::Pending, Poll::Ready)).await;
        let connection = ConnectionLease::from_accepted(socket, &pool.admission)?;
        let received = io.receive_head(connection, &scope).await?;
        assert!(
            matches!(received.value.start, StartLine::Request { ref target, .. } if target == "/pool")
        );
        let response = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header {
                name: "content-length".into(),
                value: b"1".to_vec(),
            }],
        };
        let sent = io.send_head(received.connection, response, &scope).await?;
        let mut buffer = io.buffer(1)?;
        buffer.bytes_mut()?.copy_from_slice(b"x");
        let sent = io.write_body(sent.connection, buffer, &scope).await?;
        let mut connection = sent.lease;
        connection.finish_exchange()?;
        Ok::<_, Error>(connection)
    };
    drive(&pool.reactor, async { futures::try_join!(client, server) }).unwrap()
}
#[test]
fn peer_tcp_nodelay_does_not_touch_unix_or_origin_sockets() {
    let (admission, reactor, pool) = setup();
    let pool = pool.with_peer_tcp_nodelay(true);
    let (listener, endpoint) = Listener::origin();
    let Endpoint::Unix(path) = endpoint else {
        panic!()
    };
    for endpoint in [
        Endpoint::Unix(path.clone()),
        Endpoint::Origin {
            path,
            cache: crate::model::CacheId("nodelay-test".into()),
        },
    ] {
        let scope = scope();
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        let accepted = listener.accept().unwrap();
        drop(connection);
        drop(accepted);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}

#[test]
fn idle_expiration_runs_without_checkout_or_waiters_and_is_budgeted() {
    let clock = crate::runtime::environment::SimulationClock::new(91);
    let _environment = clock.environment(0).enter();
    let (admission, _, mut pool) = setup();
    pool.idle_timeout = Duration::ZERO;
    let mut peers = Vec::new();
    for _ in 1..=3 {
        let (listener, endpoint) = Listener::peer();
        let (lease, peer) = held(&pool, &endpoint, &listener);
        peers.push(peer);
        drop(lease);
    }
    for remaining in (0..3).rev() {
        clock.advance(Duration::from_secs(1));
        pool.poll_waiters(1);
        assert_eq!(admission.used(ResourceClass::OutboundConnection), remaining);
    }
}
#[test]
fn origin_wait_is_bounded_fifo_without_blocking_peers_or_other_caches() {
    let clock = crate::runtime::environment::SimulationClock::new(92);
    let _environment = clock.environment(0).enter();
    let (admission, reactor, pool) = setup();
    let (listener, endpoint) = Listener::origin();
    let (first, a) = held(&pool, &endpoint, &listener);
    let (second, b) = held(&pool, &endpoint, &listener);
    let baseline = admission.used(ResourceClass::RequestContext);
    let scope = scope();
    let count = Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let mut older = pool.checkout_wait(&endpoint, &scope);
    let mut newer = pool.checkout_wait(&endpoint, &scope);
    assert!(older.as_mut().poll(&mut cx).is_pending());
    assert!(newer.as_mut().poll(&mut cx).is_pending());
    assert_eq!(count.count(), 0, "no self-wake spin");
    pool.poll_waiters(1);
    assert_eq!(count.count(), 1, "tick wake is budgeted");
    pool.poll_waiters(2);
    assert_eq!(count.count(), 1, "early worker repolls do not busy-wake");
    assert!(matches!(
        pool.checkout_wait(&endpoint, &scope).as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(admission.used(ResourceClass::RequestContext) > 0);
    assert_eq!(reactor.in_flight(), 0);
    // A full origin queue does not gate peers or unrelated caches; peer saturation is immediate.
    for (listener, other) in [Listener::peer(), Listener::origin()] {
        let (lease, _peer) = held(&pool, &other, &listener);
        if matches!(other, Endpoint::Peer(_)) {
            assert!(matches!(
                pool.checkout(&other, &scope).as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Overloaded))
            ));
        }
        drop(lease);
        let Poll::Ready(Ok(lease)) = pool.checkout_wait(&other, &scope).as_mut().poll(&mut cx)
        else {
            panic!("unrelated endpoint blocked")
        };
        drop(lease);
    }
    let control = admission
        .reserve(None, ResourceClass::ControlProgress, 1)
        .unwrap();
    drop(control);
    drop(first);
    assert!(count.count() > 0);
    assert!(
        newer.as_mut().poll(&mut cx).is_pending(),
        "FIFO cannot be bypassed"
    );
    let Poll::Ready(Ok(lease)) = older.as_mut().poll(&mut cx) else {
        panic!("oldest must reuse released slot")
    };
    assert!(newer.as_mut().poll(&mut cx).is_pending());
    drop(second);
    let Poll::Ready(Ok(next)) = newer.as_mut().poll(&mut cx) else {
        panic!("second waiter must progress")
    };
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    drop((lease, next, a, b));
    pool.close();
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn waiting_cancel_deadline_close_stop_and_drop_release_only_waiter_quota() {
    for case in ["cancel", "deadline", "close", "stop", "drop"] {
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = Listener::peer();
        let (held, peer) = held(&pool, &endpoint, &listener);
        let baseline = admission.used(ResourceClass::RequestContext);
        let mut scope = scope();
        if case == "deadline" {
            scope.deadline.0 = Instant::now() + Duration::from_millis(20);
        }
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut wait = pool.checkout_wait(&endpoint, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        let expected = match case {
            "cancel" => {
                scope.cancel().unwrap();
                Error::Cancelled
            }
            "deadline" => {
                std::thread::sleep(Duration::from_millis(30));
                pool.poll_waiters(1);
                Error::DeadlineExceeded
            }
            "close" => {
                pool.close();
                Error::Unavailable
            }
            "stop" => {
                admission.stop();
                pool.poll_waiters(1);
                Error::Unavailable
            }
            _ => Error::Internal,
        };
        if case != "drop" {
            assert!(count.count() > 0);
            assert!(
                matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
            );
        }
        drop(wait);
        assert!(pool.state.borrow().waiting.is_empty());
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
        assert_eq!(reactor.in_flight(), 0);
        drop((held, peer));
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}
#[test]
fn worker_tick_wakes_bounded_round_robin_waiters_even_in_nested_executor() {
    use futures::{Stream, stream::FuturesUnordered};
    let (_, _, pool) = setup();
    let (listener, endpoint) = Listener::peer();
    let (_held, _peer) = held(&pool, &endpoint, &listener);
    let mut scope = scope();
    scope.deadline.0 = Instant::now() + Duration::from_millis(20);
    let mut futures = FuturesUnordered::new();
    futures.push(pool.checkout_wait(&endpoint, &scope));
    futures.push(pool.checkout_wait(&endpoint, &scope));
    let count = Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    assert!(
        std::pin::Pin::new(&mut futures)
            .poll_next(&mut cx)
            .is_pending()
    );
    std::thread::sleep(Duration::from_millis(30));
    // Outer polling alone does not drive a sleeping child.
    assert!(
        std::pin::Pin::new(&mut futures)
            .poll_next(&mut cx)
            .is_pending()
    );
    pool.poll_waiters(0);
    assert!(
        std::pin::Pin::new(&mut futures)
            .poll_next(&mut cx)
            .is_pending()
    );
    for _ in 0..2 {
        pool.poll_waiters(1);
        assert!(matches!(
            std::pin::Pin::new(&mut futures).poll_next(&mut cx),
            Poll::Ready(Some(Err(Error::DeadlineExceeded)))
        ));
        std::thread::sleep(Duration::from_millis(2));
    }
    assert!(pool.state.borrow().waiting.is_empty());
}
#[test]
fn waiting_connection_quota_releases_and_connect_abandonment_keeps_fence() {
    let (admission, reactor, pool) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let quota = admission
        .reserve(
            None,
            ResourceClass::Connection,
            admission.limit(ResourceClass::Connection),
        )
        .unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let scope = scope();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut wait = pool.checkout_wait(&endpoint, &scope);
    assert!(wait.as_mut().poll(&mut cx).is_pending());
    assert_eq!(reactor.in_flight(), 0);
    assert!(
        pool.state.borrow().entries.is_empty(),
        "no slot held waiting for global quota"
    );
    drop(quota);
    pool.poll_waiters(1);
    assert!(wait.as_mut().poll(&mut cx).is_pending());
    assert!(pool.state.borrow().waiting.is_empty());
    assert_eq!(reactor.in_flight(), 1);
    drop(wait);
    assert_eq!(admission.used(ResourceClass::Connection), 1);
    assert_eq!(pool.state.borrow().entries[&endpoint].active, 1);
    assert!(matches!(
        pool.checkout(&endpoint, &scope).as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Overloaded))
    ));
    while reactor.in_flight() != 0 {
        scope.check().unwrap();
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert!(pool.state.borrow().entries.is_empty());
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
}
#[test]
fn waiting_endpoint_table_and_context_pressure_remain_bounded() {
    let (admission, reactor, mut pool) = setup();
    pool.max_endpoints = 1;
    let (listener, first) = Listener::peer();
    let (second_listener, second) = Listener::peer();
    let (held, peer) = held(&pool, &first, &listener);
    let baseline = admission.used(ResourceClass::RequestContext);
    let scope = scope();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let context = admission
        .reserve(
            None,
            ResourceClass::RequestContext,
            admission.limit(ResourceClass::RequestContext) - baseline,
        )
        .unwrap();
    assert!(matches!(
        pool.checkout_wait(&second, &scope).as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(pool.state.borrow().waiting.is_empty());
    assert_eq!(pool.state.borrow().entries.len(), 1);
    drop(context);
    let mut wait = pool.checkout_wait(&second, &scope);
    assert!(wait.as_mut().poll(&mut cx).is_pending());
    assert_eq!(pool.state.borrow().entries.len(), 1);
    drop(held);
    // Real checkout evicts the idle-only endpoint before its idle timeout.
    let connection = drive(&reactor, wait.as_mut()).unwrap();
    assert!(second_listener.accept().is_some());
    assert_eq!(pool.state.borrow().entries.len(), 1);
    assert!(!pool.state.borrow().entries.contains_key(&first));
    drop((connection, wait, peer));
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn origin_uid_churn_preserves_endpoint_and_connection_bounds() {
    let (admission, _, mut pool) = setup();
    pool.max_endpoints = 1;
    let (listener, endpoint) = Listener::origin();
    let Endpoint::Unix(path) = endpoint else {
        unreachable!()
    };
    let origin = |uid: usize| Endpoint::Origin {
        cache: crate::model::CacheId(format!("cache-{uid}")),
        path: path.clone(),
    };
    let (first, peer) = held(&pool, &origin(0), &listener);
    assert!(matches!(
        pool.prepare_connection(&origin(1)),
        Err(Error::Overloaded)
    ));
    assert_eq!(pool.state.borrow().entries.len(), 1);
    drop((first, peer));
    for uid in 1..32 {
        let (connection, peer) = held(&pool, &origin(uid), &listener);
        assert_eq!(pool.state.borrow().entries.len(), 1);
        assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
        drop((connection, peer));
    }
    pool.close();
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn origin_cap_is_independent_of_peer_cap_and_idle_quota_is_reclaimed() {
    let (admission, _, pool) = setup();
    let (listener, endpoint) = Listener::origin();
    let Endpoint::Unix(path) = endpoint else {
        unreachable!()
    };
    let origin = Endpoint::Origin {
        cache: crate::model::CacheId("cache".into()),
        path,
    };
    let (first, a) = held(&pool, &origin, &listener);
    let (second, b) = held(&pool, &origin, &listener);
    drop(second);
    let (second, address) = pool.prepare_connection(&origin).unwrap();
    assert!(
        address.is_none(),
        "second origin slot available despite peer cap one"
    );
    assert!(matches!(
        pool.prepare_connection(&origin),
        Err(Error::Overloaded)
    ));
    drop((first, second, a, b));
    let quota = admission
        .reserve(
            None,
            ResourceClass::Connection,
            admission.limit(ResourceClass::Connection) - 1,
        )
        .unwrap();
    let other = Endpoint::Peer("127.0.0.1:9".into());
    let (lease, _) = pool.prepare_connection(&other).unwrap();
    assert_eq!(pool.state.borrow().entries[&origin].idle.len(), 0);
    drop((lease, quota));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn metadata_bypasses_queued_pages_within_existing_origin_cap() {
    let (admission, _, pool) = setup();
    let (listener, endpoint) = Listener::origin();
    let Endpoint::Unix(path) = endpoint else {
        unreachable!()
    };
    let endpoint = Endpoint::Origin {
        cache: crate::model::CacheId("cache".into()),
        path,
    };
    let (first, a) = held(&pool, &endpoint, &listener);
    let (idle, b) = held(&pool, &endpoint, &listener);
    let baseline = admission.used(ResourceClass::RequestContext);
    drop(idle);
    let scope = scope();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let mut page = pool.checkout_wait(&endpoint, &scope);
    assert!(page.as_mut().poll(&mut cx).is_pending());
    let mut metadata = pool.checkout_metadata(&endpoint, &scope);
    let Poll::Ready(Ok(metadata)) = metadata.as_mut().poll(&mut cx) else {
        panic!("metadata queued behind page")
    };
    assert_eq!(pool.state.borrow().entries[&endpoint].active, 2);
    assert!(page.as_mut().poll(&mut cx).is_pending());
    drop((metadata, first, page, a, b));
    pool.close();
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
}
#[test]
fn invalidated_active_generation_cannot_reenter_idle_and_expiry_releases_quota() {
    let (admission, _, mut pool) = setup();
    pool.idle_timeout = Duration::ZERO;
    let (listener, endpoint) = Listener::peer();
    for invalidate in [true, false] {
        let (connection, peer) = held(&pool, &endpoint, &listener);
        if invalidate {
            pool.invalidate(&endpoint);
        }
        drop((connection, peer));
        assert_eq!(
            admission.used(ResourceClass::Connection),
            usize::from(!invalidate)
        );
        pool.expire_idle();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert!(pool.state.borrow().entries.is_empty());
    }
}
#[test]
fn opaque_staging_and_connection_reservations_survive_reactor_abandonment() {
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
    let (listener, endpoint) = Listener::peer();
    let (connection, peer) = held(&pool, &endpoint, &listener);
    let scope =
        RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
    let staging = OwnedBuffer::new(&admission, 4096).unwrap();
    let mut receive = reactor.recv(connection.socket(), staging, connection, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(matches!(receive.as_mut().poll(&mut cx), Poll::Pending));
    drop(receive);
    drop(pool);
    assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
    assert!(admission.used(ResourceClass::RequestContext) >= baseline + 4096);
    let deadline = Instant::now() + Duration::from_secs(5);
    while reactor.in_flight() != 0 {
        assert!(Instant::now() < deadline);
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    drop(peer);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
}
