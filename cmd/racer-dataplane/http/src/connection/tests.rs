use super::*;

#[derive(Clone, Copy, Debug, PartialEq)]
enum Failure {
    Http(Error),
    Runtime(uring_runtime::Error),
}
impl From<Error> for Failure {
    fn from(e: Error) -> Self {
        Self::Http(e)
    }
}
impl From<uring_runtime::Error> for Failure {
    fn from(e: uring_runtime::Error) -> Self {
        Self::Runtime(e)
    }
}
#[derive(Clone)]
struct TestScope;
impl Scope for TestScope {
    type Error = Failure;
    fn check(&self) -> std::result::Result<(), Failure> {
        Ok(())
    }
}
struct Charge(Rc<Cell<usize>>, usize);
impl Drop for Charge {
    fn drop(&mut self) {
        self.0.set(self.0.get() - self.1);
    }
}
#[derive(Default)]
struct Policy {
    session: usize,
    finished: usize,
    transient: Option<Charge>,
    on_idle: Option<Box<dyn FnOnce()>>,
    rewrite: Option<Rewrite>,
    hook_calls: Option<Rc<Cell<usize>>>,
}
#[derive(Clone, Copy)]
enum Rewrite {
    Length(u64),
    Status(u16),
    Head,
    Kind,
    Close,
    Transfer,
    Ordinary,
}
impl Policy {
    fn rewrite(&self, mut head: MessageHead) -> MessageHead {
        if let Some(calls) = &self.hook_calls {
            calls.set(calls.get() + 1);
        }
        match self.rewrite {
            Some(Rewrite::Length(n)) => head.headers[0].value = n.to_string().into_bytes(),
            Some(Rewrite::Status(status)) => head.start = StartLine::Response { status },
            Some(Rewrite::Head) => {
                head.start = StartLine::Request {
                    method: "HEAD".into(),
                    target: "/".into(),
                }
            }
            Some(Rewrite::Kind) => head.start = StartLine::Response { status: 200 },
            Some(Rewrite::Close) => head.headers.push(crate::Header {
                name: "Connection".into(),
                value: b"close".to_vec(),
            }),
            Some(Rewrite::Transfer) => head.headers.push(crate::Header {
                name: "Transfer-Encoding".into(),
                value: b"chunked".to_vec(),
            }),
            Some(Rewrite::Ordinary) => head.headers.push(crate::Header {
                name: "X-Checked".into(),
                value: b"yes".to_vec(),
            }),
            None => (),
        }
        head
    }
}
impl State<Failure> for Policy {
    fn admit(&mut self, head: MessageHead) -> std::result::Result<MessageHead, Failure> {
        self.session += 1;
        Ok(self.rewrite(head))
    }
    fn sign(&mut self, head: MessageHead) -> std::result::Result<MessageHead, Failure> {
        self.session += 1;
        Ok(self.rewrite(head))
    }
    fn finished(&mut self) {
        self.finished += 1;
    }
    fn idle(mut self) -> Self {
        if let Some(callback) = self.on_idle.take() {
            callback();
        }
        self.transient.take();
        self
    }
}
#[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
struct Key;
impl Endpoint<Failure> for Key {
    fn address(&self) -> std::result::Result<SocketAddress, Failure> {
        Ok(SocketAddress::Inet("127.0.0.1:9".parse().unwrap()))
    }
}
struct Hooks {
    used: Rc<Cell<usize>>,
    slots: Rc<Cell<usize>>,
    stopped: Cell<bool>,
    reject_charge: Cell<bool>,
}
impl Context for Hooks {
    type Error = Failure;
    type Scope = TestScope;
    type Budget = ();
    type Reactor = Rc<Reactor<TestScope, ()>>;
    type Charge = Charge;
    type Slot = Charge;
    type Opaque = ();
    type State = Policy;
    type Endpoint = Key;
    fn charge(&self, n: usize) -> Result<Self, Charge> {
        if self.stopped.get() {
            return Err(uring_runtime::Error::Unavailable.into());
        }
        if self.reject_charge.get() {
            return Err(uring_runtime::Error::Overloaded.into());
        }
        self.used.set(self.used.get() + n);
        Ok(Charge(self.used.clone(), n))
    }
    fn outbound_slot(&self) -> Result<Self, Charge> {
        self.slots.set(self.slots.get() + 1);
        Ok(Charge(self.slots.clone(), 1))
    }
    fn stopped(&self) -> bool {
        self.stopped.get()
    }
}
fn hooks() -> Rc<Hooks> {
    Rc::new(Hooks {
        used: Rc::default(),
        slots: Rc::default(),
        stopped: Cell::new(false),
        reject_charge: Cell::new(false),
    })
}
fn reactor() -> Rc<Rc<Reactor<TestScope, ()>>> {
    Rc::new(Rc::new(Reactor::new(16, ())))
}
fn head(status: u16, length: u64) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status },
        headers: vec![crate::Header {
            name: "Content-Length".into(),
            value: length.to_string().into_bytes(),
        }],
    }
}
#[test]
fn framing_bounds_and_head_representation_are_independent() {
    assert_eq!(framing(&head(200, 9), false, 8), Err(Error::Malformed));
    assert_eq!(framing(&head(200, u64::MAX), true, 8), Ok(0));
    assert_eq!(framing(&head(204, 1), false, 8), Err(Error::Malformed));
    assert_eq!(framing(&head(304, u64::MAX), false, 8), Ok(0));
    assert_eq!(framing(&head(101, 0), false, 8), Err(Error::Malformed));
    assert_eq!(framing(&head(200, 8), false, 8), Ok(8));
}
#[test]
fn buffer_charge_zeroization_and_stop_use_caller_hooks() {
    let hooks = hooks();
    let io = HttpIo::<Hooks>::new(reactor(), Codec::new(128), hooks.clone(), 8, 16);
    let mut buffer = io.buffer(32).unwrap();
    buffer.bytes_mut().unwrap().fill(99);
    drop(buffer);
    assert_eq!(hooks.used.get(), 32);
    let buffer = io.buffer(32).unwrap();
    assert_eq!(buffer.bytes().unwrap(), &[0; 32]);
    drop(buffer);
    hooks.stopped.set(true);
    assert!(matches!(
        io.buffer(32),
        Err(Failure::Runtime(uring_runtime::Error::Unavailable))
    ));
    io.reclaim_buffer();
    assert_eq!(hooks.used.get(), 0);
}
#[test]
fn checked_consumption_and_taken_excess_cannot_enable_reuse() {
    let hooks = hooks();
    let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
    let mut lease = ConnectionLease::<Hooks>::from_reserved(
        fd.into(),
        hooks.outbound_slot().unwrap(),
        Policy::default(),
    )
    .unwrap();
    lease.rx_remaining = Some(1);
    lease.tx_remaining = Some(0);
    assert_eq!(
        lease.consume_received(2),
        Err(Failure::Http(Error::Malformed))
    );
    assert_eq!(lease.receive_remaining(), Some(1));
    assert_eq!(lease.consume_sent(1), Err(Failure::Http(Error::Malformed)));
    let buffer = OwnedBuffer::copy_from(hooks.as_ref(), b"ab").unwrap();
    lease.restore_read_ahead(buffer, 0..2).unwrap();
    let ahead = lease.take_read_ahead();
    assert!(lease.closing());
    lease.consume_received(1).unwrap();
    lease.finish_exchange().unwrap();
    assert!(!lease.is_reusable());
    assert_eq!(lease.state().finished, 1);
    drop(ahead);
    drop(lease);
    assert_eq!(hooks.used.get(), 0);
    assert_eq!(hooks.slots.get(), 0);
}

#[test]
fn restoring_tail_invalidates_an_already_finished_lease() {
    let hooks = hooks();
    let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
    let mut lease = ConnectionLease::<Hooks>::from_reserved(
        fd.into(),
        hooks.outbound_slot().unwrap(),
        Policy::default(),
    )
    .unwrap();
    lease.rx_remaining = Some(0);
    lease.tx_remaining = Some(0);
    lease.finish_exchange().unwrap();
    assert!(lease.is_reusable());
    let buffer = OwnedBuffer::copy_from(hooks.as_ref(), b"next").unwrap();
    lease.restore_read_ahead(buffer, 0..4).unwrap();
    assert!(!lease.is_reusable());
    assert_eq!(lease.next_round(), Err(Failure::Http(Error::Malformed)));
}
#[test]
fn pool_return_transforms_policy_outside_borrow_and_releases_resources() {
    let hooks = hooks();
    let pool = HttpPool::<Hooks>::new(reactor(), hooks.clone(), PoolConfig::default());
    let (mut lease, _) = pool.prepare_connection(&Key).unwrap();
    assert_eq!(hooks.slots.get(), 1);
    assert!(matches!(
        pool.prepare_connection(&Key),
        Err(Failure::Runtime(uring_runtime::Error::Overloaded))
    ));
    // Replace an unconnected test descriptor with a healthy accepted pair.
    let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
    fd.set_nonblocking(true).unwrap();
    lease.fd = Rc::new(fd.into());
    lease.state_mut().admit(head(200, 0)).unwrap();
    lease.state_mut().sign(head(200, 0)).unwrap();
    lease.state_mut().transient = Some(hooks.charge(7).unwrap());
    let state = pool.state.clone();
    lease.state_mut().on_idle = Some(Box::new(move || {
        assert!(state.try_borrow_mut().is_ok());
    }));
    lease.rx_remaining = Some(0);
    lease.tx_remaining = Some(0);
    lease.finish_exchange().unwrap();
    drop(lease);
    assert_eq!(hooks.used.get(), 0);
    assert_eq!(hooks.slots.get(), 1);
    let (lease, address) = pool.prepare_connection(&Key).unwrap();
    assert!(address.is_none());
    assert_eq!(lease.state().session, 2);
    assert_eq!(lease.state().finished, 1);
    drop(lease);
    assert_eq!(hooks.slots.get(), 0);
    pool.close();
    assert!(matches!(
        pool.prepare_connection(&Key),
        Err(Failure::Runtime(uring_runtime::Error::Unavailable))
    ));
}

fn drive<T>(reactor: &Reactor<TestScope, ()>, future: impl std::future::Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = std::task::Context::from_waker(Waker::noop());
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < deadline, "bounded test driver");
        reactor.poll_budgeted(32).unwrap();
        std::thread::yield_now();
    }
}
fn lease(
    hooks: &Hooks,
    policy: Policy,
) -> (ConnectionLease<Hooks>, std::os::unix::net::UnixStream) {
    let (fd, peer) = std::os::unix::net::UnixStream::pair().unwrap();
    (
        ConnectionLease::from_reserved(fd.into(), hooks.outbound_slot().unwrap(), policy).unwrap(),
        peer,
    )
}

#[test]
fn sign_uses_final_framing_and_rejects_invalid_changes_before_send() {
    use std::io::Read;
    for (rewrite, expected) in [
        (Rewrite::Length(3), Some(3)),
        (Rewrite::Status(304), Some(0)),
        (Rewrite::Length(9), None),
        (Rewrite::Status(204), None),
        (Rewrite::Status(101), None),
        (Rewrite::Transfer, None),
    ] {
        let hooks = hooks();
        let reactor = reactor();
        let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(256), hooks.clone(), 8, 8);
        let (connection, mut peer) = lease(
            &hooks,
            Policy {
                rewrite: Some(rewrite),
                ..Policy::default()
            },
        );
        let result = drive(&reactor, io.send_head(connection, head(200, 1), &TestScope));
        if let Some(expected) = expected {
            let done = result.unwrap();
            assert_eq!(done.connection.send_remaining(), Some(expected));
            drop(done);
            let mut wire = Vec::new();
            peer.read_to_end(&mut wire).unwrap();
            let decoded = Codec::<()>::new(256).decode_head(&wire).unwrap().unwrap().0;
            assert_eq!(framing(&decoded, false, 8).unwrap(), expected);
        } else {
            assert!(matches!(result, Err(Failure::Http(Error::Malformed))));
            let mut wire = Vec::new();
            peer.read_to_end(&mut wire).unwrap();
            assert!(wire.is_empty());
        }
    }
    let hooks = hooks();
    let io = HttpIo::<Hooks>::new(reactor(), Codec::new(256), hooks.clone(), 8, 8);
    let calls = Rc::new(Cell::new(0));
    let (connection, _peer) = lease(
        &hooks,
        Policy {
            rewrite: Some(Rewrite::Length(0)),
            hook_calls: Some(calls.clone()),
            ..Policy::default()
        },
    );
    hooks.reject_charge.set(true);
    let mut future = io.send_head(connection, head(200, 9), &TestScope);
    assert!(matches!(
        future
            .as_mut()
            .poll(&mut std::task::Context::from_waker(Waker::noop())),
        Poll::Ready(Err(Failure::Http(Error::Malformed)))
    ));
    assert_eq!(
        calls.get(),
        0,
        "prevalidation precedes charging and signing"
    );
}

#[test]
fn admission_cannot_rewrite_wire_framing_even_for_bodyless_heads() {
    use std::io::Write;
    for rewrite in [
        Rewrite::Length(3),
        Rewrite::Status(304),
        Rewrite::Head,
        Rewrite::Kind,
        Rewrite::Close,
        Rewrite::Transfer,
        Rewrite::Ordinary,
    ] {
        let hooks = hooks();
        let reactor = reactor();
        let io = HttpIo::<Hooks>::new(reactor.clone(), Codec::new(256), hooks.clone(), 8, 8);
        let (mut connection, mut peer) = lease(
            &hooks,
            Policy {
                rewrite: Some(rewrite),
                ..Policy::default()
            },
        );
        connection.request_is_head = true;
        let wire: &[u8] = if matches!(rewrite, Rewrite::Head | Rewrite::Kind) {
            b"GET / HTTP/1.1\r\nContent-Length: 0\r\n\r\n"
        } else {
            b"HTTP/1.1 200 OK\r\nContent-Length: 1\r\n\r\n"
        };
        peer.write_all(wire).unwrap();
        let result = drive(&reactor, io.receive_head(connection, &TestScope));
        if matches!(rewrite, Rewrite::Ordinary) {
            let done = result.unwrap();
            assert_eq!(
                done.value.unique("x-checked").unwrap(),
                Some(b"yes".as_slice())
            );
            assert_eq!(done.connection.receive_remaining(), Some(0));
            drop(done);
        } else {
            assert!(matches!(result, Err(Failure::Http(Error::Malformed))));
        }
        io.reclaim_buffer();
        assert_eq!(hooks.slots.get(), 0);
        assert_eq!(hooks.used.get(), 0);
    }
}

#[test]
fn default_attach_installs_checkout_state_on_fresh_and_reused_connections() {
    let hooks = hooks();
    let pool = HttpPool::<Hooks>::new(reactor(), hooks.clone(), PoolConfig::default());
    let (mut connection, _) = pool.prepare_connection(&Key).unwrap();
    connection.state_mut().attach(Policy {
        session: 42,
        transient: Some(hooks.charge(7).unwrap()),
        ..Policy::default()
    });
    assert_eq!(connection.state().session, 42);
    assert_eq!(hooks.used.get(), 7);
    let (fd, _peer) = std::os::unix::net::UnixStream::pair().unwrap();
    fd.set_nonblocking(true).unwrap();
    connection.fd = Rc::new(fd.into());
    connection.rx_remaining = Some(0);
    connection.tx_remaining = Some(0);
    connection.finish_exchange().unwrap();
    drop(connection);
    assert_eq!(hooks.used.get(), 0);
    let mut checkout = pool.checkout_with_state(
        &Key,
        Policy {
            session: 99,
            transient: Some(hooks.charge(11).unwrap()),
            ..Policy::default()
        },
        &TestScope,
    );
    let Poll::Ready(Ok(connection)) = checkout
        .as_mut()
        .poll(&mut std::task::Context::from_waker(Waker::noop()))
    else {
        panic!("healthy idle checkout must complete immediately");
    };
    assert_eq!(connection.state().session, 99);
    assert_eq!(hooks.used.get(), 11);
    drop(connection);
    drop(checkout);
    assert_eq!(hooks.used.get(), 0);
}

#[test]
fn connect_observation_charge_rejects_before_submission_and_survives_abandonment() {
    let hooks = hooks();
    let reactor = reactor();
    reactor.init().unwrap();
    let pool = HttpPool::<Hooks>::new(reactor.clone(), hooks.clone(), PoolConfig::default());
    hooks.reject_charge.set(true);
    let mut checkout = pool.checkout(&Key, &TestScope);
    let mut cx = std::task::Context::from_waker(Waker::noop());
    assert!(matches!(
        checkout.as_mut().poll(&mut cx),
        Poll::Ready(Err(Failure::Runtime(uring_runtime::Error::Overloaded)))
    ));
    drop(checkout);
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(hooks.slots.get(), 0);
    assert_eq!(hooks.used.get(), 0);
    hooks.reject_charge.set(false);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = SocketAddress::Inet(listener.local_addr().unwrap());
    let (connection, _) = pool.prepare_connection(&Key).unwrap();
    let mut connect = Box::pin(pool.connect(connection, Some(address), &TestScope));
    assert!(connect.as_mut().poll(&mut cx).is_pending());
    assert_eq!(hooks.used.get(), ConnectOwner::<Hooks>::allocation());
    assert_eq!(reactor.in_flight(), 1);
    drop(connect);
    drop(pool);
    assert_eq!(hooks.used.get(), ConnectOwner::<Hooks>::allocation());
    assert_eq!(hooks.slots.get(), 1);
    drive(&reactor, reactor.drain()).unwrap();
    assert_eq!(hooks.used.get(), 0);
    assert_eq!(hooks.slots.get(), 0);
}

#[test]
fn completed_connect_releases_observation_charge_but_retains_connection_slot() {
    let hooks = hooks();
    let reactor = reactor();
    let pool = HttpPool::<Hooks>::new(reactor.clone(), hooks.clone(), PoolConfig::default());
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let (connection, _) = pool.prepare_connection(&Key).unwrap();
    let connection = drive(
        &reactor,
        pool.connect(
            connection,
            Some(SocketAddress::Inet(listener.local_addr().unwrap())),
            &TestScope,
        ),
    )
    .unwrap();
    assert_eq!(hooks.used.get(), 0);
    assert_eq!(hooks.slots.get(), 1);
    drop(connection);
    assert_eq!(hooks.slots.get(), 0);
}
