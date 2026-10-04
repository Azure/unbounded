//! Real head/body exchanges, parser boundaries, zeroization, and ownership fences.
use super::*;
use crate::model::RequestId;
use http1::Header;
use http1::StartLine;
use uring_runtime::reactor::IoBuffer;
mod delivery;
mod pool {
    use super::*;
    use crate::model::RequestId;
    use crate::test_support::WakeCounter;
    use std::sync::Arc;
    use std::task::Context;
    use std::task::Waker;
    use uring_runtime::reactor::simulation::Fault;
    use uring_runtime::reactor::simulation::Simulation;
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
    fn scope() -> RequestScope {
        RequestScope::new(
            RequestId([7; 16]),
            uring_runtime::environment::now() + Duration::from_secs(5),
        )
        .unwrap()
    }
    fn setup() -> (
        Rc<flow_control::Quotas<AdmissionPolicy>>,
        Rc<Reactor>,
        HttpPool,
    ) {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.queue_entries = std::num::NonZeroUsize::new(2).unwrap();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let mut pool = new_pool(reactor.clone(), admission.clone(), 1);
        pool.config_mut().secondary_cap = 2;
        (admission, reactor, pool)
    }
    // Hold a real checked-out connection only after a complete head/body exchange.
    fn held(
        pool: &HttpPool,
        reactor: &Rc<Reactor>,
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        endpoint: &Endpoint,
        listener: &Listener,
    ) -> (ConnectionLease, ConnectionLease) {
        let scope = scope();
        let io = new_io(reactor.clone(), Codec::new(4096), admission.clone(), 16);
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
                std::future::poll_fn(|_| listener.accept().map_or(Poll::Pending, Poll::Ready))
                    .await;
            let connection = from_accepted(socket, admission)?;
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
        drive(reactor, async { futures::try_join!(client, server) }).unwrap()
    }
    #[test]
    fn peer_tcp_nodelay_does_not_touch_unix_or_origin_sockets() {
        let (admission, reactor, mut pool) = setup();
        pool.config_mut().tcp_nodelay = true;
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            panic!()
        };
        for endpoint in [
            Endpoint::Unix(path.clone()),
            Endpoint::Origin {
                path,
                cache: racer_control_wire::CacheId("nodelay-test".into()),
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
        let clock = uring_runtime::environment::SimulationClock::new(91);
        let _environment = clock.environment(0).enter();
        let (admission, reactor, mut pool) = setup();
        pool.config_mut().idle_timeout = Duration::ZERO;
        let mut peers = Vec::new();
        for _ in 1..=3 {
            let (listener, endpoint) = Listener::peer();
            let (lease, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
        let clock = uring_runtime::environment::SimulationClock::new(92);
        let _environment = clock.environment(0).enter();
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = Listener::origin();
        let (first, a) = held(&pool, &reactor, &admission, &endpoint, &listener);
        let (second, b) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
            let (lease, _peer) = held(&pool, &reactor, &admission, &other, &listener);
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
            let (held, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
            assert_eq!(pool.snapshot().waiting, 0);
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
        use futures::Stream;
        use futures::stream::FuturesUnordered;
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = Listener::peer();
        let (_held, _peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
        assert_eq!(pool.snapshot().waiting, 0);
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
            pool.snapshot().entries.is_empty(),
            "no slot held waiting for global quota"
        );
        drop(quota);
        pool.poll_waiters(1);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        assert_eq!(pool.snapshot().waiting, 0);
        assert_eq!(reactor.in_flight(), 1);
        drop(wait);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(pool.snapshot().entries[&endpoint].0, 1);
        assert!(matches!(
            pool.checkout(&endpoint, &scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        while reactor.in_flight() != 0 {
            scope.check().unwrap();
            reactor.poll_budgeted(32).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert!(pool.snapshot().entries.is_empty());
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }
    #[test]
    fn waiting_endpoint_table_and_context_pressure_remain_bounded() {
        let (admission, reactor, mut pool) = setup();
        pool.config_mut().max_endpoints = 1;
        let (listener, first) = Listener::peer();
        let (second_listener, second) = Listener::peer();
        let (held, peer) = held(&pool, &reactor, &admission, &first, &listener);
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
        assert_eq!(pool.snapshot().waiting, 0);
        assert_eq!(pool.snapshot().entries.len(), 1);
        drop(context);
        let mut wait = pool.checkout_wait(&second, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        assert_eq!(pool.snapshot().entries.len(), 1);
        drop(held);
        // Real checkout evicts the idle-only endpoint before its idle timeout.
        let connection = drive(&reactor, wait.as_mut()).unwrap();
        assert!(second_listener.accept().is_some());
        assert_eq!(pool.snapshot().entries.len(), 1);
        assert!(!pool.snapshot().entries.contains_key(&first));
        drop((connection, wait, peer));
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn origin_uid_churn_preserves_endpoint_and_connection_bounds() {
        let (admission, reactor, mut pool) = setup();
        pool.config_mut().max_endpoints = 1;
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            unreachable!()
        };
        let origin = |uid: usize| Endpoint::Origin {
            cache: racer_control_wire::CacheId(format!("cache-{uid}")),
            path: path.clone(),
        };
        let (first, peer) = held(&pool, &reactor, &admission, &origin(0), &listener);
        assert!(matches!(
            pool.prepare_connection(&origin(1)),
            Err(Error::Overloaded)
        ));
        assert_eq!(pool.snapshot().entries.len(), 1);
        drop((first, peer));
        for uid in 1..32 {
            let (connection, peer) = held(&pool, &reactor, &admission, &origin(uid), &listener);
            assert_eq!(pool.snapshot().entries.len(), 1);
            assert_eq!(admission.used(ResourceClass::OutboundConnection), 1);
            drop((connection, peer));
        }
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn origin_cap_is_independent_of_peer_cap_and_idle_quota_is_reclaimed() {
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            unreachable!()
        };
        let origin = Endpoint::Origin {
            cache: racer_control_wire::CacheId("cache".into()),
            path,
        };
        let (first, a) = held(&pool, &reactor, &admission, &origin, &listener);
        let (second, b) = held(&pool, &reactor, &admission, &origin, &listener);
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
        assert_eq!(pool.snapshot().entries[&origin].1, 0);
        drop((lease, quota));
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
    #[test]
    fn metadata_bypasses_queued_pages_within_existing_origin_cap() {
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = Listener::origin();
        let Endpoint::Unix(path) = endpoint else {
            unreachable!()
        };
        let endpoint = Endpoint::Origin {
            cache: racer_control_wire::CacheId("cache".into()),
            path,
        };
        let (first, a) = held(&pool, &reactor, &admission, &endpoint, &listener);
        let (idle, b) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
        assert_eq!(pool.snapshot().entries[&endpoint].0, 2);
        assert!(page.as_mut().poll(&mut cx).is_pending());
        drop((metadata, first, page, a, b));
        pool.close();
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    }
    #[test]
    fn invalidated_active_generation_cannot_reenter_idle_and_expiry_releases_quota() {
        let (admission, reactor, mut pool) = setup();
        pool.config_mut().idle_timeout = Duration::ZERO;
        let (listener, endpoint) = Listener::peer();
        for invalidate in [true, false] {
            let (connection, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
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
            assert!(pool.snapshot().entries.is_empty());
        }
    }
    #[test]
    fn opaque_staging_and_connection_reservations_survive_reactor_abandonment() {
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let pool = new_pool(reactor.clone(), admission.clone(), 1);
        let (listener, endpoint) = Listener::peer();
        let (connection, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
        let scope =
            RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let staging = OwnedBuffer::new(&HttpContext(admission.clone()), 4096).unwrap();
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
    #[test]
    fn adaptive_connect_errno_preserves_local_exhaustion_as_neutral() {
        for errno in [
            None,
            Some(libc::ENOBUFS),
            Some(libc::ENOMEM),
            Some(libc::EADDRNOTAVAIL),
            Some(libc::ECANCELED),
            Some(libc::EIO),
        ] {
            assert!(!peer_connect_failure(errno));
        }
        for errno in [libc::ECONNREFUSED, libc::ECONNRESET, libc::EPIPE] {
            assert!(peer_connect_failure(Some(errno)));
        }
    }
    #[test]
    fn adaptive_checkout_attributes_actual_connect_completion_errno() {
        use crate::telemetry::Event;
        use crate::telemetry::Gauge;
        use crate::telemetry::Metrics;
        for (errno, blame) in [
            (libc::ENOBUFS, false),
            (libc::ENOMEM, false),
            (libc::EADDRNOTAVAIL, false),
            (libc::ECONNREFUSED, true),
        ] {
            let simulation = Simulation::new();
            let _env = simulation.enter();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let pool = new_pool(reactor.clone(), admission, 1);
            let metrics = Metrics::default();
            let peers = crate::peer::AdaptivePeers::new(
                crate::peer::Config {
                    total: 1,
                    per_peer: 1,
                },
                metrics.clone(),
            )
            .unwrap();
            let node = racer_control_wire::NodeId("peer".into());
            let permit = peers.acquire(&node).unwrap();
            let failure = Rc::new(std::cell::Cell::new(false));
            let scope = RequestScope::new(
                crate::model::RequestId([88; 16]),
                uring_runtime::environment::now() + Duration::from_secs(5),
            )
            .unwrap();
            let endpoint = Endpoint::Peer("127.0.0.1:9999".into());
            simulation.inject("connect", Fault::Errno(errno));
            let mut checkout = checkout_peer(
                &pool,
                &endpoint,
                None,
                Some(permit.clone()),
                Some(failure.clone()),
                &scope,
            );
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let mut result = None;
            for _ in 0..32 {
                if let std::task::Poll::Ready(done) = checkout.as_mut().poll(&mut cx) {
                    result = Some(done);
                    break;
                }
                reactor.poll_budgeted(32).unwrap();
            }
            assert!(matches!(result, Some(Err(Error::Io))));
            drop(checkout);
            drop(permit);
            assert_eq!(failure.get(), blame);
            assert_eq!(peers.available(&node), !blame);
            assert_eq!(metrics.count(Event::PeerLinkFailure), u64::from(blame));
            assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
        }
    }
}
use std::future::Future;
use std::io::Read;
use std::io::Write;
use std::net::TcpListener;
use std::os::unix::net::UnixStream;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;

fn setup() -> (
    Rc<flow_control::Quotas<AdmissionPolicy>>,
    Rc<Reactor>,
    HttpIo,
    RequestScope,
) {
    let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
        crate::test_support::cluster::config(false).limits,
    )));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = new_io(
        reactor.clone(),
        Codec::new(4096),
        admission.clone(),
        8 * 1024 * 1024,
    );
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10)).unwrap();
    (admission, reactor, io, scope)
}
pub(crate) fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(12);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        assert!(
            Instant::now() < deadline,
            "HTTP operation failed to make progress"
        );
        // Repoll completed work before sleeping: a drained CQ has no event left.
        if reactor.poll_budgeted(128).unwrap() == 0 {
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
}
fn request(method: &str) -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: method.into(),
            target: "/test".into(),
        },
        headers: vec![Header {
            name: "Host".into(),
            value: b"localhost".to_vec(),
        }],
    }
}
fn response(length: usize) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![Header {
            name: "Content-Length".into(),
            value: length.to_string().into_bytes(),
        }],
    }
}
fn drain(reactor: &Reactor) {
    drive(reactor, reactor.drain()).unwrap();
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn small_peer_send_stages_actual_head_under_context_pressure() {
    let (admission, reactor, _, scope) = setup();
    reactor.init().unwrap();
    let limit = crate::peer::protocol::MAX_ENVELOPE_HEAD;
    let io = new_io(reactor.clone(), Codec::new(limit), admission.clone(), 16);
    let baseline = admission.used(ResourceClass::RequestContext);
    let held = admission
        .reserve(
            None,
            ResourceClass::RequestContext,
            admission.limit(ResourceClass::RequestContext) - baseline - limit - 4096,
        )
        .unwrap();
    let head = response(0);
    let expected = Codec::new(limit).encode_head(&head).unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let result = drive(&reactor, io.send_head(connection, head, &scope)).unwrap();
    let mut received = vec![0; expected.len()];
    peer.read_exact(&mut received).unwrap();
    assert_eq!(received, expected);
    drop(result);
    drop(held);
    io.reclaim_buffer();
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn staging_recycles_zeroed_backing_with_retained_admission() {
    let (admission, _, io, _) = setup();
    let baseline = admission.used(ResourceClass::RequestContext);
    let mut buffer = io.buffer(4096).unwrap();
    let pointer = buffer.bytes().unwrap().as_ptr();
    buffer.bytes_mut().unwrap().fill(91);
    drop(buffer);
    assert_eq!(io.retained_buffer_bytes(), 4096);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + 4096
    );
    let buffer = io.buffer(4096).unwrap();
    assert_eq!(buffer.bytes().unwrap().as_ptr(), pointer);
    assert!(buffer.bytes().unwrap().iter().all(|b| *b == 0));
    drop(buffer);
    io.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
}
#[test]
fn decoded_field_storage_is_admitted_before_parser_allocations() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let held = admission
        .reserve(
            None,
            ResourceClass::RequestContext,
            admission.limit(ResourceClass::RequestContext) - baseline - 8192,
        )
        .unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let mut head = b"GET / HTTP/1.1\r\n".to_vec();
    for _ in 0..500 {
        head.extend_from_slice(b"X:\r\n");
    }
    head.extend_from_slice(b"\r\n");
    peer.write_all(&head).unwrap();
    assert!(matches!(
        drive(&reactor, io.receive_head(connection, &scope)),
        Err(Error::Overloaded)
    ));
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    drop(held);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
}
#[test]
fn client_constructor_streams_beyond_page_cap_with_bounded_staging() {
    let (admission, reactor, _, scope) = setup();
    let io = client_io(reactor.clone(), admission.clone()).capped(4096);
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"GET /test HTTP/1.1\r\nHost: localhost\r\n\r\n")
        .unwrap();
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    let length = 2 * crate::model::PAGE_BYTES as usize + 113;
    let thread = std::thread::spawn(move || {
        peer.set_read_timeout(Some(Duration::from_secs(10)))
            .unwrap();
        let mut head = Vec::new();
        while !head.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            peer.read_exact(&mut byte).unwrap();
            head.push(byte[0]);
        }
        assert_eq!(
            head,
            format!("HTTP/1.1 200 \r\nContent-Length: {length}\r\n\r\n").as_bytes()
        );
        let mut chunk = [0; 8192];
        let mut remaining = length;
        while remaining != 0 {
            let count = remaining.min(chunk.len());
            peer.read_exact(&mut chunk[..count]).unwrap();
            assert!(chunk[..count].iter().all(|byte| *byte == 91));
            remaining -= count;
        }
    });
    let mut connection = drive(
        &reactor,
        io.send_head(received.connection, response(length), &scope),
    )
    .unwrap()
    .connection;
    drop(received.value);
    drop(received._decoded);
    assert_eq!(connection.finish_exchange(), Err(Error::InvalidRequest));
    let mut buffer = io.buffer(8192).unwrap();
    buffer.bytes_mut().unwrap().fill(91);
    let mut remaining = length;
    while remaining != 0 {
        let count = remaining.min(8192);
        let completed = drive(
            &reactor,
            io.write_body_range(connection, buffer, 0..count, &scope),
        )
        .unwrap();
        assert_eq!(completed.bytes, count);
        buffer = completed.buffer;
        connection = completed.lease;
        remaining -= count;
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + 8192
        );
    }
    connection.finish_exchange().unwrap();
    assert!(connection.is_reusable());
    assert!(matches!(
        drive(
            &reactor,
            io.write_body_range(connection, buffer, 0..1, &scope)
        ),
        Err(Error::InvalidRequest)
    ));
    thread.join().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn client_send_limit_does_not_relax_receive_or_page_transport_limits() {
    let (admission, reactor, _, scope) = setup();
    let page_limit = crate::model::PAGE_BYTES + 16;
    let page_io = new_io(
        reactor.clone(),
        Codec::new(4096),
        admission.clone(),
        page_limit,
    );
    let client_io = client_io(reactor.clone(), admission.clone()).capped(4096);
    for io in [&page_io, &client_io] {
        assert_eq!(
            io.framing(&response(page_limit as usize), false),
            Ok(page_limit)
        );
        for start in ["HTTP/1.1 200 OK", "GET /test HTTP/1.1"] {
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let connection = from_accepted(socket.into(), &admission).unwrap();
            write!(
                peer,
                "{start}\r\nContent-Length: {}\r\n\r\n",
                page_limit + 1
            )
            .unwrap();
            assert!(matches!(
                drive(&reactor, io.receive_head(connection, &scope)),
                Err(Error::InvalidRequest)
            ));
        }
    }
    for (io, length) in [
        (&page_io, page_limit + 1),
        (&client_io, i64::MAX as u64 + 1),
    ] {
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        assert!(matches!(
            drive(
                &reactor,
                io.send_head(connection, response(length as usize), &scope)
            ),
            Err(Error::InvalidRequest)
        ));
    }
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn endpoint_caps_accept_exact_boundary_and_reject_one_extra_byte() {
    let (admission, reactor, _, scope) = setup();
    let peer = new_io(
        reactor.clone(),
        Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
        admission.clone(),
        16,
    );
    let client = client_io(reactor.clone(), admission.clone());
    let origin = peer.capped(crate::http::MAX_HEAD_BYTES);
    for (io, limit) in [
        (&client, admission.policy().limits().header_bytes.get()),
        (&origin, crate::http::MAX_HEAD_BYTES),
        (&peer, crate::peer::protocol::MAX_ENVELOPE_HEAD),
    ] {
        for extra in [0, 1] {
            let mut head = request("GET");
            let overhead = Codec::new(usize::MAX).encode_head(&head).unwrap().len();
            let value_length = head.headers[0].value.len() + limit - overhead + extra;
            head.headers[0].value.resize(value_length, b'x');
            let raw = Codec::new(limit + 1).encode_head(&head).unwrap();
            assert_eq!(raw.len(), limit + extra);
            let (socket, mut other) = UnixStream::pair().unwrap();
            let connection = from_accepted(socket.into(), &admission).unwrap();
            let writer = std::thread::spawn(move || {
                let _ = other.write_all(&raw);
            });
            let result = drive(&reactor, io.receive_head(connection, &scope));
            if extra == 0 {
                assert!(result.is_ok());
            } else {
                assert!(matches!(result, Err(Error::HeaderTooLarge)));
            }
            drop(result);
            writer.join().unwrap();
            let (socket, mut other) = UnixStream::pair().unwrap();
            let connection = from_accepted(socket.into(), &admission).unwrap();
            let reader = std::thread::spawn(move || {
                let mut received = Vec::new();
                other.read_to_end(&mut received).unwrap();
                received.len()
            });
            let result = drive(&reactor, io.send_head(connection, head, &scope));
            if extra == 0 {
                assert!(result.is_ok());
            } else {
                assert!(matches!(result, Err(Error::HeaderTooLarge)));
            }
            drop(result);
            assert_eq!(reader.join().unwrap(), if extra == 0 { limit } else { 0 });
        }
    }
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn real_socket_fragmentation_read_ahead_and_owned_ranges() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let thread = std::thread::spawn(move || {
        peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Len").unwrap();
        std::thread::sleep(Duration::from_millis(5));
        peer.write_all(b"gth: 5\r\n\r\nhello").unwrap();
    });
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    let mut bytes = io.buffer(9).unwrap();
    bytes.bytes_mut().unwrap().fill(b'_');
    let mut connection = received.connection;
    let mut offset = 2;
    while offset < 7 {
        let completed = drive(
            &reactor,
            io.read_body_range(connection, bytes, offset..7, &scope),
        )
        .unwrap();
        offset += completed.bytes;
        bytes = completed.buffer;
        connection = completed.lease;
    }
    assert_eq!(bytes.bytes().unwrap(), b"__hello__");
    assert_eq!(connection.remaining_body(), Some(0));
    drop(connection);
    drop(bytes);
    drop(received.value);
    drop(received._decoded);
    thread.join().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn real_partial_sends_preserve_owned_subrange() {
    let (admission, reactor, io, scope) = setup();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    use std::os::fd::AsRawFd;
    let size: libc::c_int = 4096;
    // SAFETY: setsockopt synchronously reads this correctly sized integer.
    assert_eq!(
        unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as _,
            )
        },
        0
    );
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let length = 1024 * 1024;
    let thread = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(10));
        let mut received = Vec::new();
        peer.read_to_end(&mut received).unwrap();
        let start = received.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
        assert_eq!(received.len() - start, length);
        assert!(received[start..].iter().all(|b| *b == 91));
    });
    let sent = drive(&reactor, io.send_head(connection, response(length), &scope)).unwrap();
    let mut bytes = io.buffer(length + 4).unwrap();
    bytes.bytes_mut().unwrap()[2..length + 2].fill(91);
    let completed = drive(
        &reactor,
        io.write_body_range(sent.connection, bytes, 2..length + 2, &scope),
    )
    .unwrap();
    assert_eq!(completed.bytes, length);
    drop(completed);
    drain(&reactor);
    thread.join().unwrap();
}
#[test]
fn tcp_pool_reuses_only_finished_exchanges_and_enforces_capacity() {
    let (admission, reactor, io, scope) = setup();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let pool = new_pool(reactor.clone(), admission.clone(), 1);
    let thread = std::thread::spawn(move || {
        let (mut socket, _) = listener.accept().unwrap();
        for _ in 0..2 {
            let mut bytes = Vec::new();
            while !bytes.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                bytes.push(byte[0]);
            }
            socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                .unwrap();
        }
    });
    for _ in 0..2 {
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        assert!(matches!(
            drive(&reactor, pool.checkout(&endpoint, &scope)),
            Err(Error::Overloaded)
        ));
        let received = drive(
            &reactor,
            io.exchange_head(connection, request("GET"), &scope),
        )
        .unwrap();
        let mut completed =
            drive(&reactor, io.collect_body(received.connection, 2, &scope)).unwrap();
        assert_eq!(completed.buffer.bytes().unwrap(), b"ok");
        completed.lease.finish_exchange().unwrap();
        assert!(completed.lease.is_reusable());
        drop(completed);
    }
    thread.join().unwrap();
    pool.close();
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn head_has_no_body_even_with_large_representation_length() {
    let (admission, reactor, io, scope) = setup();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let thread = std::thread::spawn(move || {
        let mut request = Vec::new();
        while !request.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            peer.read_exact(&mut byte).unwrap();
            request.push(byte[0]);
        }
        peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 999999999\r\n\r\n")
            .unwrap();
    });
    let mut received = drive(
        &reactor,
        io.exchange_head(connection, request("HEAD"), &scope),
    )
    .unwrap();
    assert_eq!(received.connection.remaining_body(), Some(0));
    received.connection.finish_exchange().unwrap();
    thread.join().unwrap();
}
#[test]
fn truncation_and_future_drop_do_not_recycle_live_buffers() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort")
        .unwrap();
    drop(peer);
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    assert!(matches!(
        drive(&reactor, io.collect_body(received.connection, 9, &scope)),
        Err(Error::Io)
    ));
    drop(received.value);
    drop(received._decoded);
    let (socket, _peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let mut future = io.receive_head(connection, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    scope.cancel().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn deadline_expires_without_peer_traffic() {
    let (admission, reactor, io, _) = setup();
    let scope = RequestScope::new(
        RequestId([2; 16]),
        Instant::now() + Duration::from_millis(30),
    )
    .unwrap();
    let (socket, _peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    assert!(matches!(
        drive(&reactor, io.receive_head(connection, &scope)),
        Err(Error::DeadlineExceeded)
    ));
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn unix_pool_reconnects_after_unread_response_and_bounds_endpoints() {
    use std::os::unix::net::UnixListener;
    let (admission, reactor, io, scope) = setup();
    let mut nonce = [0; 8];
    getrandom::getrandom(&mut nonce).unwrap();
    let path = std::path::PathBuf::from(format!(
        "racer-http-{}-{}.sock",
        std::process::id(),
        u64::from_ne_bytes(nonce)
    ));
    let listener = UnixListener::bind(&path).unwrap();
    let endpoint = Endpoint::Unix(path.clone());
    let pool = pool_with_limits(reactor.clone(), admission.clone(), 1, 1, Duration::ZERO);
    let thread = std::thread::spawn(move || {
        for _ in 0..2 {
            let (mut socket, _) = listener.accept().unwrap();
            let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata");
            let mut sink = Vec::new();
            let _ = socket.read_to_end(&mut sink);
        }
    });
    let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
    let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    assert_eq!(
        received.connection.finish_exchange(),
        Err(Error::InvalidRequest)
    );
    drop(received);
    let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
    let other = Endpoint::Peer("127.0.0.1:1".into());
    assert!(matches!(
        drive(&reactor, pool.checkout(&other, &scope)),
        Err(Error::Overloaded)
    ));
    pool.invalidate(&endpoint);
    drop(connection);
    pool.expire_idle();
    pool.close();
    thread.join().unwrap();
    std::fs::remove_file(path).unwrap();
    assert!(matches!(
        drive(&reactor, pool.checkout(&endpoint, &scope)),
        Err(Error::Unavailable)
    ));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn read_ahead_erases_credentials_and_endpoint_limit_is_enforced() {
    let (admission, reactor, io, scope) = setup();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"POST / HTTP/1.1\r\nAuthorization: secret\r\nContent-Length: 4\r\n\r\nbody")
        .unwrap();
    let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    let (ahead, range) = received
        .connection
        .take_read_ahead()
        .expect("socket supplied head and body together");
    assert!(
        ahead.bytes().unwrap()[..range.start]
            .iter()
            .all(|b| *b == 0)
    );
    assert_eq!(&ahead.bytes().unwrap()[range.clone()], b"body");
    drop(received);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"GET / HTTP/1.1\r\nAuthorization: too-large\r\n\r\n")
        .unwrap();
    assert!(matches!(
        drive(&reactor, io.receive_head_limited(connection, &scope, 20)),
        Err(Error::HeaderTooLarge)
    ));
}
#[test]
fn bodyless_and_malformed_response_framing_is_explicit() {
    let (_, _, io, _) = setup();
    assert_eq!(io.framing(&response(usize::MAX), true), Ok(0));
    assert!(io.framing(&response(usize::MAX), false).is_err());
    assert_eq!(
        io.framing(
            &MessageHead {
                start: StartLine::Response { status: 204 },
                headers: vec![]
            },
            false
        ),
        Ok(0)
    );
    assert!(
        io.framing(
            &MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![]
            },
            false
        )
        .is_err()
    );
    assert!(
        io.framing(
            &MessageHead {
                start: StartLine::Response { status: 101 },
                headers: vec![]
            },
            false
        )
        .is_err()
    );
}
#[test]
fn dropped_connect_retains_slot_until_fd_fence_then_releases_quota() {
    let (admission, reactor, _, scope) = setup();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let pool = new_pool(reactor.clone(), admission.clone(), 1);
    let mut future = pool.checkout(&endpoint, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    assert_eq!(admission.used(ResourceClass::Connection), 1);
    assert!(matches!(
        drive(&reactor, pool.checkout(&endpoint, &scope)),
        Err(Error::Overloaded)
    ));
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn dropped_checkout_and_destroyed_pool_release_quota_only_after_connect_fence() {
    for (cancel_before_drop, submit_before_drop) in [(false, false), (true, false), (true, true)] {
        let (admission, reactor, _, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let pool = new_pool(reactor.clone(), admission.clone(), 1);
        let mut future = pool.checkout(&endpoint, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        if submit_before_drop {
            reactor.poll_budgeted(32).unwrap();
        }
        if cancel_before_drop {
            scope.cancel().unwrap();
        }
        drop(future);
        drop(pool);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(reactor.in_flight(), 1);
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        // No pool sweep exists: prove admission regained the charge rather than leaking it.
        let charge = admission
            .reserve(
                None,
                ResourceClass::Connection,
                admission.policy().limits().client_connections.get(),
            )
            .unwrap();
        drop(charge);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}
#[test]
fn canceled_receive_retains_resources_until_completion_and_reports_canceled() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, _peer) = UnixStream::pair().unwrap();
    let connection = from_accepted(socket.into(), &admission).unwrap();
    let mut future = io.receive_head(connection, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    scope.cancel().unwrap();
    assert_eq!(admission.used(ResourceClass::Connection), 1);
    assert!(matches!(drive(&reactor, future), Err(Error::Cancelled)));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
}
#[test]
fn raw_request_head_errors_return_fenced_socket_for_empty_400_and_431() {
    for oversized in [false, true] {
        let (admission, reactor, _, scope) = setup();
        let io = new_io(
            reactor.clone(),
            Codec::new(32 * 1024),
            admission.clone(),
            1024,
        );
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        let connection = from_accepted(socket.into(), &admission).unwrap();
        let raw = if oversized {
            let mut raw = b"GET / HTTP/1.1\r\nAuthorization: ".to_vec();
            raw.resize(crate::http::MAX_HEAD_BYTES, b'x');
            raw
        } else {
            b"GET / HTTP/1.1\r\nAuthorization:secret\r\nContent-Length: 4\r\n\r\nbody".to_vec()
        };
        peer.write_all(&raw).unwrap();
        let expected = if oversized {
            Error::HeaderTooLarge
        } else {
            Error::InvalidRequest
        };
        let mut outcome = drive(
            &reactor,
            io.receive_request_head_limited(connection, &scope, 32 * 1024),
        )
        .unwrap();
        assert!(matches!(outcome.value, Err(error) if error == expected));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
        assert!(outcome.connection.take_read_ahead().is_none());
        assert!(!outcome.connection.is_reusable());
        assert_eq!(
            outcome.connection.finish_exchange(),
            Err(Error::InvalidRequest)
        );
        let status = if oversized { 431 } else { 400 };
        let mut head = response(0);
        head.start = StartLine::Response { status };
        head.headers.push(Header {
            name: "Connection".into(),
            value: b"close".to_vec(),
        });
        let mut sent = drive(&reactor, io.send_head(outcome.connection, head, &scope)).unwrap();
        assert!(!sent.connection.is_reusable());
        assert_eq!(
            sent.connection.finish_exchange(),
            Err(Error::InvalidRequest)
        );
        let expected =
            format!("HTTP/1.1 {status} \r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
        let mut wire = vec![0; expected.len()];
        peer.read_exact(&mut wire).unwrap();
        assert_eq!(wire, expected.as_bytes());
        drop(sent);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}
mod relay_tests {
    use super::*;
    use crate::peer::transport::relay_body;
    use flow_control::pipe::MAX_PIPE_BYTES;
    use std::net::TcpStream;
    fn pair() -> (TcpStream, TcpStream) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (socket, _) = listener.accept().unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        peer.set_write_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        (socket, peer)
    }
    fn poll<F: Future + ?Sized>(future: std::pin::Pin<&mut F>) -> Poll<F::Output> {
        future.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    // Match the production service's wake-driven FuturesUnordered and reactor wait,
    // rather than repeatedly polling a blocked future with a noop waker.
    fn drive_worker<T>(reactor: &Reactor, work: impl Future<Output = T>) -> T {
        use futures::Stream;
        use futures::stream::FuturesUnordered;
        use std::sync::Arc;
        use std::task::Wake;
        struct WakeReactor(uring_runtime::reactor::ReactorWake);
        impl Wake for WakeReactor {
            fn wake(self: Arc<Self>) {
                self.0.wake().unwrap();
            }
        }
        let waker = std::task::Waker::from(Arc::new(WakeReactor(reactor.waker().unwrap())));
        let mut active = FuturesUnordered::new();
        active.push(work);
        let until = Instant::now() + Duration::from_secs(8);
        loop {
            reactor.poll_budgeted(64).unwrap();
            if let Poll::Ready(Some(result)) =
                std::pin::Pin::new(&mut active).poll_next(&mut Context::from_waker(&waker))
            {
                return result;
            }
            assert!(Instant::now() < until, "wake-driven relay watchdog");
            reactor.wait(Duration::from_millis(10)).unwrap();
        }
    }
    struct Fixture {
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        reactor: Rc<Reactor>,
        io: HttpIo,
        pipes: PipePool<AdmissionPolicy>,
        scope: RequestScope,
    }
    impl Fixture {
        fn new() -> Self {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.pipes = std::num::NonZeroUsize::new(1).unwrap();
            limits.relay_transfers = std::num::NonZeroUsize::new(1).unwrap();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            reactor.init().unwrap();
            Self {
                io: new_io(
                    reactor.clone(),
                    Codec::new(4096),
                    admission.clone(),
                    crate::model::PAGE_BYTES + 16,
                ),
                pipes: new_pipe_pool(admission.clone()),
                admission,
                reactor,
                scope: RequestScope::new(
                    RequestId([3; 16]),
                    Instant::now() + Duration::from_secs(8),
                )
                .unwrap(),
            }
        }
        fn connections(
            &self,
            length: usize,
        ) -> (ConnectionLease, ConnectionLease, TcpStream, TcpStream) {
            let (source, writer) = pair();
            let (destination, reader) = pair();
            let mut source = from_accepted(source.into(), &self.admission).unwrap();
            let mut destination = from_accepted(destination.into(), &self.admission).unwrap();
            let reservation = Rc::new(
                self.admission
                    .reserve(None, ResourceClass::Relay, 1)
                    .unwrap(),
            );
            source.state_mut().relay_reservation = Some(reservation.clone());
            destination.state_mut().relay_reservation = Some(reservation);
            source.set_framing(Some(length as u64), Some(0), false);
            destination.set_framing(Some(0), Some(length as u64), false);
            (source, destination, writer, reader)
        }
        fn drain(&self) {
            drive(&self.reactor, self.reactor.drain()).unwrap();
        }
    }
    #[test]
    fn truncation_after_success_closes_both_without_error_suffix_or_pool_reuse() {
        let f = Fixture::new();
        let (source, mut destination, mut writer, mut reader) = f.connections(1024);
        destination.set_framing(destination.receive_remaining(), None, false);
        destination = drive(
            &f.reactor,
            f.io.send_head(
                destination,
                MessageHead {
                    start: StartLine::Response { status: 200 },
                    headers: vec![Header {
                        name: "Content-Length".into(),
                        value: b"1024".to_vec(),
                    }],
                },
                &f.scope,
            ),
        )
        .unwrap()
        .connection;
        writer.write_all(b"short").unwrap();
        writer.shutdown(std::net::Shutdown::Write).unwrap();
        let pipe = f.pipes.acquire().unwrap();
        assert!(matches!(
            drive(
                &f.reactor,
                relay_body(&f.io, source, destination, Some(pipe), &f.scope)
            ),
            Err(Error::Io)
        ));
        let mut bytes = Vec::new();
        reader.read_to_end(&mut bytes).unwrap();
        assert_eq!(bytes, b"HTTP/1.1 200 \r\nContent-Length: 1024\r\n\r\nshort");
        assert_eq!(writer.read(&mut [0]).unwrap(), 0);
        f.drain();
        assert_eq!(f.admission.used(ResourceClass::Connection), 0);
        assert_eq!(f.admission.used(ResourceClass::Relay), 0);
    }
    #[test]
    fn stalled_transit_retains_both_connections_pipe_and_relay_until_cancel_fence() {
        for writing in [false, true] {
            for end in ["drop", "cancel", "deadline", "disconnect"] {
                let mut f = Fixture::new();
                if end == "deadline" {
                    f.scope.deadline.0 = Instant::now() + Duration::from_millis(40);
                }
                let (source, destination, writer, reader) = f.connections(16 * 1024 * 1024 + 16);
                let pipe = f.pipes.acquire().unwrap();
                let mut held_writer = Some(writer);
                let producer = if writing {
                    let mut writer = held_writer.take().unwrap();
                    Some(std::thread::spawn(move || {
                        let _ = writer.write_all(&vec![91; 16 * 1024 * 1024 + 16]);
                    }))
                } else {
                    None
                };
                let mut work =
                    Box::pin(relay_body(&f.io, source, destination, Some(pipe), &f.scope));
                assert!(poll(work.as_mut()).is_pending());
                if writing {
                    let until = Instant::now() + Duration::from_millis(15);
                    while Instant::now() < until {
                        f.reactor.poll_budgeted(128).unwrap();
                        assert!(poll(work.as_mut()).is_pending());
                    }
                }
                assert_eq!(f.admission.used(ResourceClass::Connection), 2);
                assert_eq!(f.admission.used(ResourceClass::Relay), 1);
                assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
                assert!(matches!(
                    f.pipes.acquire(),
                    Err(flow_control::Error::Overloaded)
                ));
                assert!(matches!(
                    f.admission.reserve(None, ResourceClass::Relay, 1),
                    Err(flow_control::Error::Overloaded)
                ));
                let mut reader = Some(reader);
                if end == "disconnect" {
                    reader.take();
                }
                if end == "cancel" {
                    f.scope.cancel().unwrap();
                }
                if end != "drop" {
                    let result = drive(&f.reactor, work.as_mut());
                    let expected = match end {
                        "cancel" => Error::Cancelled,
                        "deadline" => Error::DeadlineExceeded,
                        _ => Error::Io,
                    };
                    assert!(
                        matches!(result, Err(error) if error == expected),
                        "{writing}/{end}"
                    );
                }
                drop(work);
                if end == "drop" && !writing {
                    assert_eq!(f.reactor.in_flight(), 1);
                    assert_eq!(f.admission.used(ResourceClass::Connection), 2);
                    assert_eq!(f.admission.used(ResourceClass::Relay), 1);
                    assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
                }
                f.drain();
                assert_eq!(f.admission.used(ResourceClass::Connection), 0);
                assert_eq!(f.admission.used(ResourceClass::Relay), 0);
                assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
                assert!(f.admission.used(ResourceClass::Pipe) <= 1);
                if let Some(producer) = producer {
                    producer.join().unwrap();
                }
            }
        }
    }
    #[test]
    fn unsupported_splice_with_buffered_pipe_drains_suffix_and_keeps_exact_frame() {
        let f = Fixture::new();
        let length = 1024 * 1024 + 16;
        let (source, mut destination, mut writer, mut reader) = f.connections(length);
        destination.state_mut().relay_fallback_at = Some(length / 2);
        let producer = std::thread::spawn(move || {
            writer.write_all(&vec![83; length]).unwrap();
            writer
        });
        let consumer = std::thread::spawn(move || {
            let mut body = vec![0; length];
            reader.read_exact(&mut body).unwrap();
            assert!(body.iter().all(|b| *b == 83));
            reader
        });
        let pipe = f.pipes.acquire().unwrap();
        let result = drive(
            &f.reactor,
            relay_body(&f.io, source, destination, Some(pipe), &f.scope),
        )
        .unwrap();
        assert!(result.is_reusable());
        drop(result);
        drop(producer.join().unwrap());
        drop(consumer.join().unwrap());
        f.drain();
        assert_eq!(f.admission.used(ResourceClass::Relay), 0);
        assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
        assert!(f.io.retained_buffer_bytes() <= MAX_PIPE_BYTES);
    }
    #[test]
    fn tcp_backpressure_recovers_and_wakes_queued_pipe_owner_without_losing_frame() {
        use std::os::fd::AsRawFd;
        let f = Fixture::new();
        let length = 16 * 1024 * 1024 + 16;
        let (source, destination, mut writer, mut reader) = f.connections(length);
        let size: libc::c_int = 64 * 1024;
        // SAFETY: live TCP descriptor and correctly sized option storage.
        assert_eq!(
            unsafe {
                libc::setsockopt(
                    destination.socket().as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_SNDBUF,
                    (&size as *const libc::c_int).cast(),
                    std::mem::size_of_val(&size) as libc::socklen_t,
                )
            },
            0
        );
        let producer = std::thread::spawn(move || {
            writer.write_all(&vec![83; length]).unwrap();
            writer
        });
        let mut pipe = f.pipes.acquire().unwrap();
        pipe.prepare_transit();
        let mut relay = Box::pin(relay_body(&f.io, source, destination, Some(pipe), &f.scope));
        let mut waiting = acquire_wait(&f.pipes, &f.scope);
        assert!(poll(waiting.as_mut()).is_pending());
        // The reader is deliberately not running. A page cannot fit in the bounded
        // send/receive buffers, so transit must retain its owners under backpressure.
        let pause = Instant::now() + Duration::from_millis(40);
        while Instant::now() < pause {
            f.reactor.poll_budgeted(64).unwrap();
            assert!(poll(relay.as_mut()).is_pending());
            std::thread::yield_now();
        }
        assert!(poll(waiting.as_mut()).is_pending());
        assert_eq!(f.admission.used(ResourceClass::Relay), 1);
        assert_eq!(f.admission.used(ResourceClass::Pipe), 1);
        assert_eq!(f.admission.used(ResourceClass::Connection), 2);
        let consumer = std::thread::spawn(move || {
            let mut body = vec![0; length];
            reader.read_exact(&mut body).unwrap();
            assert!(body.iter().all(|b| *b == 83));
            reader
        });
        let (connection, pipe) = drive_worker(&f.reactor, async { futures::join!(relay, waiting) });
        let mut connection = connection.unwrap();
        let mut pipe = pipe.unwrap();
        assert!(connection.is_reusable());
        assert_eq!(f.admission.used(ResourceClass::Relay), 0);
        assert_eq!(pipe.buffered(), 0);
        assert_eq!(f.admission.used(ResourceClass::Connection), 1);
        // Reusing the pipe must not corrupt bytes already accepted by the socket.
        pipe.try_write(b"replacement").unwrap();
        let mut bytes = [0; 11];
        assert_eq!(pipe.try_read(&mut bytes).unwrap(), 11);
        assert_eq!(&bytes, b"replacement");
        drop(pipe);
        drop(producer.join().unwrap());
        let mut reader = consumer.join().unwrap();
        connection.set_framing(Some(0), Some(4), false);
        let mut suffix = f.io.buffer(4).unwrap();
        suffix.bytes_mut().unwrap().copy_from_slice(b"next");
        drop(drive_worker(&f.reactor, f.io.write_body(connection, suffix, &f.scope)).unwrap());
        let mut next = [0; 4];
        reader.read_exact(&mut next).unwrap();
        assert_eq!(&next, b"next");
        assert_eq!(reader.read(&mut next).unwrap(), 0);
        f.drain();
        assert_eq!(f.admission.used(ResourceClass::Connection), 0);
        assert_eq!(f.admission.used(ResourceClass::Ciphertext), 0);
        // HttpIo retains its bounded reusable read/write buffers after completion.
        assert!(f.io.retained_buffer_bytes() <= MAX_PIPE_BYTES);
        assert_eq!(f.pipes.idle_count(), 1);
    }
}
