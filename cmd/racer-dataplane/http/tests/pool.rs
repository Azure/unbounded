//! Quota-backed HTTP pool mechanisms, independent of application resource policy.
use flow_control::{Charge, Policy, Quotas, Rejection};
use http1::connection::{self, Context as HttpContext, Endpoint as EndpointPolicy, PoolConfig};
use http1::{Codec, Header, MessageHead, StartLine};
use std::{
    future::Future,
    io::{Read, Write},
    net::TcpListener,
    os::unix::net::UnixStream,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll, Wake, Waker},
    time::{Duration, Instant},
};
use uring_runtime::{
    Scope,
    environment::{Cancellation, now},
    reactor::{IoBuffer, SocketAddress, descriptor::Descriptor},
};

/// Keep protocol failures distinct from runtime and quota admission failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Error {
    Http(http1::Error),
    Runtime(uring_runtime::Error),
    Quota(flow_control::Error),
}
impl From<http1::Error> for Error {
    fn from(e: http1::Error) -> Self {
        Self::Http(e)
    }
}
impl From<uring_runtime::Error> for Error {
    fn from(e: uring_runtime::Error) -> Self {
        Self::Runtime(e)
    }
}
impl From<flow_control::Error> for Error {
    fn from(e: flow_control::Error) -> Self {
        match e {
            flow_control::Error::Overloaded => Self::Runtime(uring_runtime::Error::Overloaded),
            other => Self::Quota(other),
        }
    }
}
type Result<T> = std::result::Result<T, Error>;

/// Separate buffer and socket admission so abandonment exposes early release.
#[derive(Clone, Copy)]
enum Resource {
    Context,
    Connection,
}
impl flow_control::Class for Resource {
    const COUNT: usize = 2;

    fn index(self) -> usize {
        self as usize
    }
}
/// Fixed quotas keep test bounds independent of Racer configuration defaults.
struct Limits;
impl Policy for Limits {
    type Class = Resource;

    type Key = ();

    fn limit(&self, class: Resource) -> usize {
        match class {
            Resource::Context => 1 << 20,
            Resource::Connection => 64,
        }
    }

    fn max_keys(&self) -> usize {
        1
    }

    fn wakes(_: Resource) -> bool {
        true
    }

    fn covers(_: Resource) -> bool {
        false
    }

    fn rejected(&self, _: Rejection<Resource>) {}
}
/// Deadline and independent cancellation used by the generic reactor.
#[derive(Clone)]
struct RequestScope {
    deadline: Instant,

    cancellation: Cancellation,
}
impl Scope for RequestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            return Err(uring_runtime::Error::Cancelled.into());
        }
        if now() >= self.deadline {
            return Err(uring_runtime::Error::DeadlineExceeded.into());
        }
        Ok(())
    }

    fn cancellation(&self) -> Option<&Cancellation> {
        Some(&self.cancellation)
    }
}
/// Give each operation a fresh, bounded cancellation lifetime.
fn scope() -> RequestScope {
    RequestScope {
        deadline: now() + Duration::from_secs(5),
        cancellation: Cancellation::new().unwrap(),
    }
}
#[derive(Clone, Debug, Eq, PartialEq, Ord, PartialOrd)]
/// Plain socket identity with no peer or cache namespace policy.
struct Endpoint(SocketAddress);
impl EndpointPolicy<Error> for Endpoint {
    fn address(&self) -> Result<SocketAddress> {
        Ok(self.0.clone())
    }
}
/// Admission authority shared by buffers and connection slots in this fixture.
struct Caller(Rc<Quotas<Limits>>);
/// Exercise the codec's configurable strict opaque separator profile.
struct Opaque;
impl http1::Opaque for Opaque {
    const NAMES: &'static [&'static str] = &["Authorization"];
}
impl HttpContext for Caller {
    type Error = Error;

    type Scope = RequestScope;

    type Budget = ();

    type Reactor = Rc<Reactor>;

    type Charge = Charge<Limits>;

    type Slot = Charge<Limits>;

    type Opaque = Opaque;

    type State = ();

    type Endpoint = Endpoint;

    fn charge(&self, bytes: usize) -> Result<Self::Charge> {
        Ok(self.0.reserve(None, Resource::Context, bytes)?)
    }

    fn outbound_slot(&self) -> Result<Self::Slot> {
        Ok(self.0.reserve(None, Resource::Connection, 1)?)
    }

    fn stopped(&self) -> bool {
        self.0.is_stopped()
    }
}
type Reactor = uring_runtime::reactor::Reactor<RequestScope, ()>;
type Pool = connection::HttpPool<Caller>;
type Io = connection::HttpIo<Caller>;
type Lease = connection::ConnectionLease<Caller>;
/// One active connection per endpoint and two queued checkout registrations.
fn new_pool(reactor: Rc<Reactor>, admission: Rc<Quotas<Limits>>) -> Pool {
    Pool::new(
        Rc::new(reactor),
        Rc::new(Caller(admission)),
        PoolConfig {
            per_endpoint: 1,
            secondary_cap: 2,
            max_endpoints: 32,
            waiter_cap: 2,
            idle_timeout: Duration::from_secs(30),
            tcp_nodelay: false,
        },
    )
}
/// Small bounded I/O scratch makes live accounting observable.
fn new_io(reactor: Rc<Reactor>, admission: Rc<Quotas<Limits>>) -> Io {
    Io::new(
        Rc::new(reactor),
        Codec::new(4096),
        Rc::new(Caller(admission)),
        16,
        16,
    )
}
/// Initialize the explicitly polled reactor before recording accounting baselines.
fn setup() -> (Rc<Quotas<Limits>>, Rc<Reactor>, Pool) {
    let admission = Rc::new(Quotas::new(Limits));
    let reactor = Rc::new(Reactor::new(32, ()));
    reactor.init().unwrap();
    let pool = new_pool(reactor.clone(), admission.clone());
    (admission, reactor, pool)
}
/// Charge server sockets just as strictly as outbound client sockets.
fn accepted(socket: Descriptor, admission: &Quotas<Limits>) -> Lease {
    Lease::from_reserved(
        socket,
        admission.reserve(None, Resource::Connection, 1).unwrap(),
        (),
    )
    .unwrap()
}
/// Drive a single future and its reactor with a strict fixture watchdog.
fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let until = Instant::now() + Duration::from_secs(5);
    let mut cx = Context::from_waker(Waker::noop());
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < until, "fixture watchdog");
        reactor.poll_budgeted(32).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}
/// Wait for accepted I/O owners rather than treating future drop as a fence.
fn drain(reactor: &Reactor) {
    drive(reactor, reactor.drain()).unwrap();
}
/// Encode fixed-length success framing.
fn response(length: usize) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![Header::new("Content-Length", length.to_string())],
    }
}
/// Encode the bodyless request used by pool reuse scenarios.
fn request() -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: "GET".into(),
            target: "/pool".into(),
        },
        headers: vec![Header::new("Content-Length", "0")],
    }
}
/// Bind a distinct nonblocking endpoint for each pool identity.
fn listener() -> (TcpListener, Endpoint) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let endpoint = Endpoint(SocketAddress::Inet(listener.local_addr().unwrap()));
    (listener, endpoint)
}
/// Complete both cursors before offering the client to the idle pool.
fn held(
    pool: &Pool,
    reactor: &Rc<Reactor>,
    admission: &Rc<Quotas<Limits>>,
    endpoint: &Endpoint,
    listener: &TcpListener,
) -> (Lease, Lease) {
    let scope = scope();
    let io = new_io(reactor.clone(), admission.clone());
    let client = async {
        let connection = pool.checkout_metadata(endpoint, &scope).await?;
        let sent = io.send_head(connection, request(), &scope).await?;
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
        let socket = std::future::poll_fn(|_| match listener.accept() {
            Ok((socket, _)) => Poll::Ready(socket),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => Poll::Pending,
            Err(e) => panic!("accept: {e}"),
        })
        .await;
        let received = io
            .receive_head(accepted(socket.into(), admission), &scope)
            .await?;
        assert!(
            matches!(received.value.start, StartLine::Request { ref target, .. } if target == "/pool")
        );
        let sent = io
            .send_head(received.connection, response(1), &scope)
            .await?;
        let mut buffer = io.buffer(1)?;
        buffer.bytes_mut()?.copy_from_slice(b"x");
        let sent = io.write_body(sent.connection, buffer, &scope).await?;
        let mut connection = sent.lease;
        connection.finish_exchange()?;
        Ok::<_, Error>(connection)
    };
    drive(reactor, async { futures::try_join!(client, server) }).unwrap()
}
#[derive(Default)]
struct WakeCounter(AtomicUsize);
impl Wake for WakeCounter {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}
impl WakeCounter {
    fn count(&self) -> usize {
        self.0.load(Ordering::SeqCst)
    }
}

#[test]
fn idle_expiration_runs_without_checkout_or_waiters_and_is_budgeted() {
    let (admission, reactor, mut pool) = setup();
    pool.config_mut().idle_timeout = Duration::ZERO;
    let mut peers = Vec::new();
    for _ in 1..=3 {
        let (listener, endpoint) = listener();
        let (lease, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
        peers.push(peer);
        drop(lease);
    }
    for remaining in (0..3).rev() {
        std::thread::sleep(Duration::from_millis(110));
        pool.poll_waiters(1);
        assert_eq!(
            admission.used(Resource::Connection),
            remaining + peers.len()
        );
    }
}

#[test]
fn waiting_cancel_deadline_close_stop_and_drop_release_only_waiter_quota() {
    for case in ["cancel", "deadline", "close", "stop", "drop"] {
        let (admission, reactor, pool) = setup();
        let (listener, endpoint) = listener();
        let (held, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
        let baseline = admission.used(Resource::Context);
        let mut scope = scope();
        if case == "deadline" {
            scope.deadline = now() + Duration::from_millis(20);
        }
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut wait = pool.checkout_wait(&endpoint, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        let expected = match case {
            "cancel" => {
                scope.cancellation.cancel().unwrap();
                uring_runtime::Error::Cancelled
            }
            "deadline" => {
                std::thread::sleep(Duration::from_millis(30));
                pool.poll_waiters(1);
                uring_runtime::Error::DeadlineExceeded
            }
            "close" => {
                pool.close();
                uring_runtime::Error::Unavailable
            }
            "stop" => {
                admission.stop();
                pool.poll_waiters(1);
                uring_runtime::Error::Unavailable
            }
            _ => uring_runtime::Error::InvalidInput,
        };
        if case != "drop" {
            assert!(count.count() > 0);
            assert!(
                matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected.into())
            );
        }
        drop(wait);
        assert_eq!(pool.snapshot().waiting, 0);
        assert_eq!(admission.used(Resource::Context), baseline);
        assert_eq!(admission.used(Resource::Connection), 2);
        assert_eq!(reactor.in_flight(), 0);
        drop((held, peer));
        pool.close();
        assert_eq!(admission.used(Resource::Connection), 0);
    }
}

#[test]
fn worker_tick_wakes_bounded_round_robin_waiters_even_in_nested_executor() {
    use futures::{Stream, stream::FuturesUnordered};
    let (admission, reactor, pool) = setup();
    let (listener, endpoint) = listener();
    let (_held, _peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
    let mut scope = scope();
    scope.deadline = now() + Duration::from_millis(20);
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
            Poll::Ready(Some(Err(Error::Runtime(
                uring_runtime::Error::DeadlineExceeded
            ))))
        ));
        std::thread::sleep(Duration::from_millis(2));
    }
    assert_eq!(pool.snapshot().waiting, 0);
}

#[test]
fn waiting_connection_quota_releases_and_connect_abandonment_keeps_fence() {
    let (admission, reactor, pool) = setup();
    let baseline = admission.used(Resource::Context);
    let quota = admission
        .reserve(
            None,
            Resource::Connection,
            admission.limit(Resource::Connection),
        )
        .unwrap();
    let (_listener, endpoint) = listener();
    let scope = scope();
    let mut cx = Context::from_waker(Waker::noop());
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
    assert_eq!(admission.used(Resource::Connection), 1);
    assert_eq!(pool.snapshot().entries[&endpoint].0, 1);
    assert!(matches!(
        pool.checkout(&endpoint, &scope).as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Runtime(uring_runtime::Error::Overloaded)))
    ));
    drain(&reactor);
    assert!(pool.snapshot().entries.is_empty());
    assert_eq!(admission.used(Resource::Connection), 0);
    assert_eq!(admission.used(Resource::Context), baseline);
}

#[test]
fn waiting_endpoint_table_and_context_pressure_remain_bounded() {
    let (admission, reactor, mut pool) = setup();
    pool.config_mut().max_endpoints = 1;
    let (listener, first) = listener();
    let (second_listener, second) = self::listener();
    let (held, peer) = held(&pool, &reactor, &admission, &first, &listener);
    let baseline = admission.used(Resource::Context);
    let scope = scope();
    let mut cx = Context::from_waker(Waker::noop());
    let context = admission
        .reserve(
            None,
            Resource::Context,
            admission.limit(Resource::Context) - baseline,
        )
        .unwrap();
    assert!(matches!(
        pool.checkout_wait(&second, &scope).as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Runtime(uring_runtime::Error::Overloaded)))
    ));
    assert_eq!(pool.snapshot().waiting, 0);
    assert_eq!(pool.snapshot().entries.len(), 1);
    drop(context);
    let mut wait = pool.checkout_wait(&second, &scope);
    assert!(wait.as_mut().poll(&mut cx).is_pending());
    assert_eq!(pool.snapshot().entries.len(), 1);
    drop(held);
    let connection = drive(&reactor, wait.as_mut()).unwrap();
    assert!(second_listener.accept().is_ok());
    assert_eq!(pool.snapshot().entries.len(), 1);
    assert!(!pool.snapshot().entries.contains_key(&first));
    drop((connection, wait, peer));
    assert_eq!(admission.used(Resource::Context), baseline);
    assert_eq!(admission.used(Resource::Connection), 0);
}

#[test]
fn invalidated_active_generation_cannot_reenter_idle_and_expiry_releases_quota() {
    let (admission, reactor, mut pool) = setup();
    pool.config_mut().idle_timeout = Duration::ZERO;
    let (listener, endpoint) = listener();
    for invalidate in [true, false] {
        let (connection, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
        if invalidate {
            pool.invalidate(&endpoint);
        }
        drop((connection, peer));
        assert_eq!(
            admission.used(Resource::Connection),
            usize::from(!invalidate)
        );
        pool.expire_idle();
        assert_eq!(admission.used(Resource::Connection), 0);
        assert!(pool.snapshot().entries.is_empty());
    }
}

#[test]
fn tcp_pool_reuses_only_finished_exchanges_and_enforces_capacity() {
    let (admission, reactor, pool) = setup();
    let io = new_io(reactor.clone(), admission.clone());
    let scope = scope();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint(SocketAddress::Inet(listener.local_addr().unwrap()));
    let thread = std::thread::spawn(move || {
        let (mut socket, _) = listener.accept().unwrap();
        socket
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
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
            Err(Error::Runtime(uring_runtime::Error::Overloaded))
        ));
        let received = drive(&reactor, io.exchange_head(connection, request(), &scope)).unwrap();
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
    assert_eq!(admission.used(Resource::Connection), 0);
}

#[test]
fn truncation_and_future_drop_do_not_recycle_live_buffers() {
    let (admission, reactor, _) = setup();
    let io = new_io(reactor.clone(), admission.clone());
    let scope = scope();
    let baseline = admission.used(Resource::Context);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = accepted(socket.into(), &admission);
    peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort")
        .unwrap();
    drop(peer);
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    assert!(matches!(
        drive(&reactor, io.collect_body(received.connection, 9, &scope)),
        Err(Error::Runtime(uring_runtime::Error::Io))
    ));
    drop(received.value);
    drop(received._decoded);
    let (socket, _peer) = UnixStream::pair().unwrap();
    let mut future = io.receive_head(accepted(socket.into(), &admission), &scope);
    let mut cx = Context::from_waker(Waker::noop());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    scope.cancellation.cancel().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(Resource::Context),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(Resource::Connection), 0);
}

#[test]
fn deadline_expires_without_peer_traffic() {
    let (admission, reactor, _) = setup();
    let io = new_io(reactor.clone(), admission.clone());
    let mut scope = scope();
    scope.deadline = now() + Duration::from_millis(30);
    let (socket, _peer) = UnixStream::pair().unwrap();
    assert!(matches!(
        drive(
            &reactor,
            io.receive_head(accepted(socket.into(), &admission), &scope)
        ),
        Err(Error::Runtime(uring_runtime::Error::DeadlineExceeded))
    ));
    drain(&reactor);
    assert_eq!(admission.used(Resource::Connection), 0);
}

#[test]
fn dropped_connect_retains_slot_until_fd_fence_then_releases_quota() {
    let (admission, reactor, pool) = setup();
    let (_listener, endpoint) = listener();
    let scope = scope();
    let mut future = pool.checkout(&endpoint, &scope);
    let mut cx = Context::from_waker(Waker::noop());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    assert_eq!(admission.used(Resource::Connection), 1);
    assert!(matches!(
        drive(&reactor, pool.checkout(&endpoint, &scope)),
        Err(Error::Runtime(uring_runtime::Error::Overloaded))
    ));
    drain(&reactor);
    assert_eq!(admission.used(Resource::Connection), 0);
}

#[test]
fn canceled_receive_retains_resources_until_completion_and_reports_canceled() {
    let (admission, reactor, _) = setup();
    let io = new_io(reactor.clone(), admission.clone());
    let scope = scope();
    let baseline = admission.used(Resource::Context);
    let (socket, _peer) = UnixStream::pair().unwrap();
    let mut future = io.receive_head(accepted(socket.into(), &admission), &scope);
    let mut cx = Context::from_waker(Waker::noop());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    scope.cancellation.cancel().unwrap();
    assert_eq!(admission.used(Resource::Connection), 1);
    assert!(matches!(
        drive(&reactor, future),
        Err(Error::Runtime(uring_runtime::Error::Cancelled))
    ));
    assert_eq!(admission.used(Resource::Connection), 0);
    assert_eq!(
        admission.used(Resource::Context),
        baseline + io.retained_buffer_bytes()
    );
}

#[test]
fn unix_pool_reconnects_after_unread_response_and_bounds_endpoints() {
    use std::os::unix::net::UnixListener;
    let (admission, reactor, mut pool) = setup();
    pool.config_mut().max_endpoints = 1;
    pool.config_mut().secondary_cap = 1;
    pool.config_mut().idle_timeout = Duration::ZERO;
    let io = new_io(reactor.clone(), admission.clone());
    let scope = scope();
    static NEXT: AtomicUsize = AtomicUsize::new(0);
    let path = std::path::PathBuf::from(format!(
        "http-pool-{}-{}.sock",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let listener = UnixListener::bind(&path).unwrap();
    let endpoint = Endpoint(SocketAddress::Unix(path.clone()));
    let thread = std::thread::spawn(move || {
        for _ in 0..2 {
            let (mut socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata");
            let mut sink = Vec::new();
            let _ = socket.read_to_end(&mut sink);
        }
    });
    let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
    let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    assert_eq!(
        received.connection.finish_exchange(),
        Err(Error::Http(http1::Error::Malformed))
    );
    drop(received);
    let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
    let other = Endpoint(SocketAddress::Inet("127.0.0.1:1".parse().unwrap()));
    assert!(matches!(
        drive(&reactor, pool.checkout(&other, &scope)),
        Err(Error::Runtime(uring_runtime::Error::Overloaded))
    ));
    pool.invalidate(&endpoint);
    drop(connection);
    pool.expire_idle();
    pool.close();
    thread.join().unwrap();
    std::fs::remove_file(path).unwrap();
    assert!(matches!(
        drive(&reactor, pool.checkout(&endpoint, &scope)),
        Err(Error::Runtime(uring_runtime::Error::Unavailable))
    ));
    assert_eq!(admission.used(Resource::Connection), 0);
}

#[test]
fn read_ahead_erases_credentials_and_endpoint_limit_is_enforced() {
    let (admission, reactor, _) = setup();
    let io = new_io(reactor.clone(), admission.clone());
    let scope = scope();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    peer.write_all(b"POST / HTTP/1.1\r\nAuthorization: secret\r\nContent-Length: 4\r\n\r\nbody")
        .unwrap();
    let mut received = drive(
        &reactor,
        io.receive_head(accepted(socket.into(), &admission), &scope),
    )
    .unwrap();
    let (ahead, range) = received
        .connection
        .take_read_ahead()
        .expect("head and body together");
    assert!(
        ahead.bytes().unwrap()[..range.start]
            .iter()
            .all(|b| *b == 0)
    );
    assert_eq!(&ahead.bytes().unwrap()[range], b"body");
    drop(received);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    peer.write_all(b"GET / HTTP/1.1\r\nAuthorization: too-large\r\n\r\n")
        .unwrap();
    assert!(matches!(
        drive(
            &reactor,
            io.receive_head_limited(accepted(socket.into(), &admission), &scope, 20)
        ),
        Err(Error::Http(http1::Error::HeadTooLarge))
    ));
}

#[test]
fn bodyless_and_malformed_response_framing_is_explicit() {
    let (admission, reactor, _) = setup();
    let io = new_io(reactor, admission);
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
    for status in [200, 101] {
        assert!(
            io.framing(
                &MessageHead {
                    start: StartLine::Response { status },
                    headers: vec![]
                },
                false
            )
            .is_err()
        );
    }
}

/// A malformed head returns a fenced socket that can send only a closing error.
#[test]
fn raw_request_head_errors_return_fenced_socket_for_empty_400_and_431() {
    for oversized in [false, true] {
        let (admission, reactor, _) = setup();
        let scope = scope();
        let io = Io::new(
            Rc::new(reactor.clone()),
            Codec::new(32 * 1024),
            Rc::new(Caller(admission.clone())),
            1024,
            1024,
        );
        let baseline = admission.used(Resource::Context);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        let raw = if oversized {
            let mut raw = b"GET / HTTP/1.1\r\nAuthorization: ".to_vec();
            raw.resize(32 * 1024, b'x');
            raw
        } else {
            b"GET / HTTP/1.1\r\nAuthorization:secret\r\nContent-Length: 4\r\n\r\nbody".to_vec()
        };
        peer.write_all(&raw).unwrap();
        let expected = Error::Http(if oversized {
            http1::Error::HeadTooLarge
        } else {
            http1::Error::Malformed
        });
        let mut outcome = drive(
            &reactor,
            io.receive_request_head_limited(accepted(socket.into(), &admission), &scope, 32 * 1024),
        )
        .unwrap();
        assert!(matches!(outcome.value, Err(error) if error == expected));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(
            admission.used(Resource::Context),
            baseline + io.retained_buffer_bytes()
        );
        assert!(outcome.connection.take_read_ahead().is_none());
        assert!(!outcome.connection.is_reusable());
        assert_eq!(
            outcome.connection.finish_exchange(),
            Err(Error::Http(http1::Error::Malformed))
        );
        let status = if oversized { 431 } else { 400 };
        let mut head = response(0);
        head.start = StartLine::Response { status };
        head.headers.push(Header::new("Connection", "close"));
        let mut sent = drive(&reactor, io.send_head(outcome.connection, head, &scope)).unwrap();
        assert!(!sent.connection.is_reusable());
        assert_eq!(
            sent.connection.finish_exchange(),
            Err(Error::Http(http1::Error::Malformed))
        );
        let expected =
            format!("HTTP/1.1 {status} \r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
        let mut wire = vec![0; expected.len()];
        peer.read_exact(&mut wire).unwrap();
        assert_eq!(wire, expected.as_bytes());
        drop(sent);
        assert_eq!(admission.used(Resource::Connection), 0);
    }
}

/// FIFO fairness and independent endpoints are mechanism policy, not cache identity.
/// Racer retains its own endpoint/class/control-reserve assertions in its adapter test.
#[test]
fn waiter_fifo_capacity_and_independent_endpoint_progress() {
    let (admission, reactor, mut pool) = setup();
    pool.config_mut().per_endpoint = 2;
    let (listener, endpoint) = listener();
    let (first, peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
    let (second, second_peer) = held(&pool, &reactor, &admission, &endpoint, &listener);
    let baseline = admission.used(Resource::Context);
    let scope = scope();
    let count = Arc::new(WakeCounter::default());
    let waker = Waker::from(count.clone());
    let mut cx = Context::from_waker(&waker);
    let mut older = pool.checkout_wait(&endpoint, &scope);
    let mut newer = pool.checkout_wait(&endpoint, &scope);
    assert!(older.as_mut().poll(&mut cx).is_pending());
    assert!(newer.as_mut().poll(&mut cx).is_pending());
    assert_eq!(count.count(), 0);
    pool.poll_waiters(1);
    assert_eq!(count.count(), 1);
    pool.poll_waiters(2);
    assert_eq!(count.count(), 1);
    assert!(matches!(
        pool.checkout_wait(&endpoint, &scope).as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Runtime(uring_runtime::Error::Overloaded)))
    ));
    assert!(admission.used(Resource::Context) > baseline);
    assert_eq!(reactor.in_flight(), 0);
    let (other_listener, other) = self::listener();
    let (lease, other_peer) = held(&pool, &reactor, &admission, &other, &other_listener);
    drop(lease);
    let Poll::Ready(Ok(lease)) = pool.checkout_wait(&other, &scope).as_mut().poll(&mut cx) else {
        panic!("independent endpoint blocked")
    };
    drop(lease);
    drop(first);
    assert!(count.count() > 0);
    assert!(newer.as_mut().poll(&mut cx).is_pending());
    let Poll::Ready(Ok(lease)) = older.as_mut().poll(&mut cx) else {
        panic!("oldest did not progress")
    };
    assert!(newer.as_mut().poll(&mut cx).is_pending());
    drop(second);
    let Poll::Ready(Ok(next)) = newer.as_mut().poll(&mut cx) else {
        panic!("second did not progress")
    };
    assert_eq!(admission.used(Resource::Context), baseline);
    drop((lease, next, peer, second_peer, other_peer));
    pool.close();
    assert_eq!(admission.used(Resource::Connection), 0);
}
