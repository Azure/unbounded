use super::*;
use http1::connection::{HttpPool, PoolConfig, State};
use std::{
    cell::Cell,
    future::{Future, poll_fn},
    task::{Poll, Waker},
    time::Instant,
};
use uring_runtime::{
    environment::{Cancellation, SimulationClock, now},
    reactor::{Reactor, SocketAddress, simulation::Simulation},
};

#[derive(Clone)]
struct TestScope {
    cancellation: Cancellation,
    deadline: Instant,
}

impl TestScope {
    fn new(duration: Duration) -> Self {
        Self {
            cancellation: Cancellation::new().unwrap(),
            deadline: now() + duration,
        }
    }
}

impl Scope for TestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            return Err(uring_runtime::Error::Cancelled.into());
        }
        if now() >= self.deadline {
            return Err(Error::DeadlineExceeded);
        }
        Ok(())
    }

    fn cancellation(&self) -> Option<&Cancellation> {
        Some(&self.cancellation)
    }
}

#[derive(Default)]
struct Policy {
    session: Option<Session>,
    installations: Rc<Cell<usize>>,
}

impl State<Error> for Policy {
    fn sign(&mut self, head: MessageHead) -> Result<MessageHead> {
        match &mut self.session {
            Some(session) => session.sign(head),
            None => Ok(head),
        }
    }

    fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
        match &mut self.session {
            Some(session) => session.admit(head),
            None => Ok(head),
        }
    }

    fn attach(&mut self, _: Self) {}
}

#[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
struct Endpoint;

impl http1::connection::Endpoint<Error> for Endpoint {
    fn address(&self) -> Result<SocketAddress> {
        Ok(SocketAddress::Inet("127.0.0.1:899".parse().unwrap()))
    }
}

struct Slot(Rc<Cell<usize>>);

impl Drop for Slot {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}

#[derive(Default)]
struct Hooks {
    slots: Rc<Cell<usize>>,
    installations: Rc<Cell<usize>>,
}

struct TestReactor(Reactor<TestScope, ()>);

impl std::ops::Deref for TestReactor {
    type Target = Reactor<TestScope, ()>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl http1::connection::Context for Hooks {
    type Error = Error;
    type Scope = TestScope;
    type Budget = ();
    type Reactor = TestReactor;
    type Charge = ();
    type Slot = Slot;
    type Opaque = Opaque;
    type State = Policy;
    type Endpoint = Endpoint;

    fn charge(&self, _: usize) -> Result<()> {
        Ok(())
    }

    fn outbound_slot(&self) -> Result<Slot> {
        self.slots.set(self.slots.get() + 1);
        Ok(Slot(self.slots.clone()))
    }

    fn stopped(&self) -> bool {
        false
    }
}

impl Context for Hooks {
    fn session(state: &Policy) -> Option<&Session> {
        state.session.as_ref()
    }

    fn session_mut(state: &mut Policy) -> &mut Option<Session> {
        state.installations.set(state.installations.get() + 1);
        &mut state.session
    }

    fn handshake_scope(parent: &TestScope) -> Result<TestScope> {
        parent.check()?;
        Ok(TestScope {
            cancellation: parent.cancellation.clone(),
            deadline: parent.deadline.min(now() + Duration::from_secs(5)),
        })
    }
}

type Lease = ConnectionLease<Hooks>;

struct Harness {
    reactor: Rc<TestReactor>,
    io: HttpIo<Hooks>,
    pool: HttpPool<Hooks>,
    hooks: Rc<Hooks>,
    scope: TestScope,
    identities: Vec<Rc<Signatures>>,
}

impl Harness {
    fn new() -> Self {
        let reactor = Rc::new(TestReactor(Reactor::new(32, ())));
        let hooks = Rc::new(Hooks::default());
        Self {
            io: HttpIo::new(reactor.clone(), Codec::new(MAX_HEAD), hooks.clone(), 0, 0),
            pool: HttpPool::new(reactor.clone(), hooks.clone(), PoolConfig::default()),
            reactor,
            hooks,
            scope: TestScope::new(Duration::from_secs(30)),
            identities: crate::tests::network(3),
        }
    }

    fn drive<T>(&self, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = std::task::Context::from_waker(Waker::noop());
        for _ in 0..10_000 {
            if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
            self.reactor.poll_budgeted(64).unwrap();
        }
        panic!("simulation exceeded bounded polling budget");
    }

    fn pair(&self, sim: &Simulation) -> (Lease, Lease) {
        use http1::connection::{Context as _, Endpoint as _};
        let listener = sim.listen(Endpoint.address().unwrap()).unwrap();
        let mut client = self
            .drive(self.pool.checkout(&Endpoint, &self.scope))
            .unwrap();
        client.state_mut().installations = self.hooks.installations.clone();
        let server = ConnectionLease::from_reserved(
            listener.as_sim().unwrap().accept().unwrap(),
            self.hooks.outbound_slot().unwrap(),
            Policy {
                installations: self.hooks.installations.clone(),
                ..Policy::default()
            },
        )
        .unwrap();
        (client, server)
    }

    fn connect<'a>(
        &'a self,
        lease: Lease,
        scope: &'a TestScope,
    ) -> impl Future<Output = Result<Lease>> + 'a {
        connect(
            &self.io,
            lease,
            self.identities[0].clone(),
            self.identities[1].node(),
            scope,
        )
    }

    fn accept<'a>(
        &'a self,
        lease: Lease,
        scope: &'a TestScope,
    ) -> impl Future<Output = Result<Lease>> + 'a {
        accept(&self.io, lease, self.identities[1].clone(), scope)
    }

    fn application_round(&self, client: Lease, server: Lease) -> (Lease, Lease) {
        let request = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: REQUEST_TARGET.into(),
            },
            headers: vec![
                Header::new("host", b""),
                Header::new("content-length", b"0"),
            ],
        };
        let response = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: vec![Header::new("content-length", b"0")],
        };
        let (client, server) = self.drive(join(
            self.io.exchange_head(client, request, &self.scope),
            async {
                let incoming = self.io.receive_head(server, &self.scope).await.unwrap();
                assert_eq!(
                    incoming.value.value().unique("racer-session").unwrap(),
                    None
                );
                self.io
                    .send_head(incoming.connection, response, &self.scope)
                    .await
            },
        ));
        let mut client = client.unwrap().connection;
        let mut server = server.unwrap().connection;
        client.finish_exchange().unwrap();
        server.finish_exchange().unwrap();
        (client, server)
    }
}

async fn join<A: Future, B: Future>(a: A, b: B) -> (A::Output, B::Output) {
    let mut a = std::pin::pin!(a);
    let mut b = std::pin::pin!(b);
    let mut left = None;
    let mut right = None;
    poll_fn(|cx| {
        if left.is_none()
            && let Poll::Ready(value) = a.as_mut().poll(cx)
        {
            left = Some(value);
        }
        if right.is_none()
            && let Poll::Ready(value) = b.as_mut().poll(cx)
        {
            right = Some(value);
        }
        if left.is_some() && right.is_some() {
            Poll::Ready((left.take().unwrap(), right.take().unwrap()))
        } else {
            Poll::Pending
        }
    })
    .await
}

fn simulation(test: impl FnOnce(&Harness, &Simulation, &SimulationClock)) {
    let clock = SimulationClock::new_at(899, Instant::now(), SystemTime::now());
    let _clock = clock.environment(0).enter();
    let sim = Simulation::new();
    let _sim = sim.enter();
    let h = Harness::new();
    test(&h, &sim, &clock);
    h.pool.close();
    h.drive(h.reactor.drain()).unwrap();
    assert_eq!(h.reactor.in_flight(), 0);
    assert_eq!(h.hooks.slots.get(), 0);
}

#[test]
fn connect_accept_exchange_and_pool_reuse_preserve_transcript_and_sequences() {
    simulation(|h, sim, _| {
        let (client, server) = h.pair(sim);
        let socket = Rc::downgrade(&client.socket());
        let (client, server) = h.drive(join(
            h.connect(client, &h.scope),
            h.accept(server, &h.scope),
        ));
        let mut client = client.unwrap();
        let mut server = server.unwrap();
        assert_eq!(h.hooks.installations.get(), 2);
        let a = &client.state().session.as_ref().unwrap().state;
        let b = &server.state().session.as_ref().unwrap().state;
        assert_eq!(a.id, b.id);
        assert_ne!(a.id, [0; 32]);
        assert_eq!((a.direction, b.direction), (0, 1));
        assert_eq!(
            (&a.peer, &b.peer),
            (h.identities[1].node(), h.identities[0].node())
        );
        assert_eq!((a.tx, a.rx, b.tx, b.rx), (0, 0, 0, 0));
        let id = a.id;
        assert!(!client.is_reusable());
        assert!(!server.is_reusable());
        assert_eq!(client.finish_exchange(), Err(Error::InvalidRequest));
        assert_eq!(server.finish_exchange(), Err(Error::InvalidRequest));
        let (client, server) = h.application_round(client, server);
        drop(client);
        let client = h.drive(h.pool.checkout(&Endpoint, &h.scope)).unwrap();
        assert!(socket.ptr_eq(&Rc::downgrade(&client.socket())));
        let trace = sim.trace();
        let client = h.drive(h.connect(client, &h.scope)).unwrap();
        let server = h.drive(h.accept(server, &h.scope)).unwrap();
        assert_eq!(h.hooks.installations.get(), 2);
        assert_eq!(
            sim.trace(),
            trace,
            "pooled authentication must not send a new hello"
        );
        for lease in [&client, &server] {
            let state = &lease.state().session.as_ref().unwrap().state;
            assert_eq!(state.id, id);
            assert_eq!((state.tx, state.rx), (1, 1));
        }
        let (client, server) = h.application_round(client, server);
        for lease in [&client, &server] {
            let state = &lease.state().session.as_ref().unwrap().state;
            assert_eq!(state.id, id);
            assert_eq!((state.tx, state.rx), (2, 2));
        }
    });
}

#[derive(Clone, Copy, Debug)]
enum Fault {
    Phase,
    ClientEcho,
    ServerEcho,
    ShortChallenge,
}

fn faulty_head(
    h: &Harness,
    phase: &str,
    mut a: [u8; 32],
    mut b: [u8; 32],
    fault: Fault,
) -> MessageHead {
    let response = matches!(phase, "challenge" | "ready");
    let (signer, peer) = if response { (1, 0) } else { (0, 1) };
    let mut actual_phase = phase;
    match fault {
        Fault::Phase => actual_phase = if response { "hello" } else { "ready" },
        Fault::ClientEcho => {
            if phase == "hello" {
                a = [0; 32];
            } else {
                a[0] ^= 1;
            }
        }
        Fault::ServerEcho => {
            if phase == "challenge" {
                b = [0; 32];
            } else {
                b[0] ^= 1;
            }
        }
        Fault::ShortChallenge => (),
    }
    let mut head = session_head(
        &h.identities[signer],
        h.identities[peer].node(),
        actual_phase,
        &a,
        &b,
        response,
    )
    .unwrap();
    if matches!(fault, Fault::ShortChallenge) {
        head.headers
            .retain(|field| !is_auth_field(&field.name) || field.name == "racer-receiver");
        head.headers
            .iter_mut()
            .find(|field| field.name == "racer-client-challenge")
            .unwrap()
            .value = binary(&[1; 31]).into_bytes();
        head = h.identities[signer].sign(head).unwrap().head;
    }
    head
}

#[test]
fn connect_rejects_signed_bad_challenge_and_ready_without_pooling() {
    for phase in ["challenge", "ready"] {
        for fault in [
            Fault::Phase,
            Fault::ClientEcho,
            Fault::ServerEcho,
            Fault::ShortChallenge,
        ] {
            simulation(|h, sim, _| {
                let (client, server) = h.pair(sim);
                let (result, ()) = h.drive(join(h.connect(client, &h.scope), async {
                    let hello = h.io.receive_head(server, &h.scope).await.unwrap();
                    let (_, a, zero) = verify_session(
                        &h.identities[1],
                        hello.value.value(),
                        Some(h.identities[0].node()),
                        "hello",
                        false,
                    )
                    .unwrap();
                    assert_eq!(zero, [0; 32]);
                    assert_ne!(a, [0; 32]);
                    let b = [7; 32];
                    let challenge = if phase == "challenge" {
                        faulty_head(h, phase, a, b, fault)
                    } else {
                        session_head(
                            &h.identities[1],
                            h.identities[0].node(),
                            "challenge",
                            &a,
                            &b,
                            true,
                        )
                        .unwrap()
                    };
                    let mut server =
                        h.io.send_head(hello.connection, challenge, &h.scope)
                            .await
                            .unwrap()
                            .connection;
                    if phase == "ready" {
                        server.next_round().unwrap();
                        let finish = h.io.receive_head(server, &h.scope).await.unwrap();
                        let (_, echoed, remote) = verify_session(
                            &h.identities[1],
                            finish.value.value(),
                            Some(h.identities[0].node()),
                            "finish",
                            false,
                        )
                        .unwrap();
                        assert_eq!((echoed, remote), (a, b));
                        server =
                            h.io.send_head(
                                finish.connection,
                                faulty_head(h, phase, a, b, fault),
                                &h.scope,
                            )
                            .await
                            .unwrap()
                            .connection;
                    }
                    assert!(server.state().session.is_none());
                }));
                assert!(
                    matches!(result, Err(Error::Unauthorized)),
                    "{phase} {fault:?}"
                );
                assert_eq!(h.hooks.installations.get(), 0);
                h.drive(h.reactor.drain()).unwrap();
                assert_eq!(
                    h.hooks.slots.get(),
                    0,
                    "failed handshake returned an idle socket"
                );
            });
        }
    }
}

#[test]
fn accept_rejects_signed_bad_hello_and_finish_without_installing_session() {
    for phase in ["hello", "finish"] {
        for fault in [
            Fault::Phase,
            Fault::ClientEcho,
            Fault::ServerEcho,
            Fault::ShortChallenge,
        ] {
            simulation(|h, sim, _| {
                let (client, server) = h.pair(sim);
                let (result, ()) = h.drive(join(h.accept(server, &h.scope), async {
                    let a = [3; 32];
                    let hello = if phase == "hello" {
                        faulty_head(h, phase, a, [0; 32], fault)
                    } else {
                        session_head(
                            &h.identities[0],
                            h.identities[1].node(),
                            "hello",
                            &a,
                            &[0; 32],
                            false,
                        )
                        .unwrap()
                    };
                    let mut client =
                        h.io.send_head(client, hello, &h.scope)
                            .await
                            .unwrap()
                            .connection;
                    if phase == "finish" {
                        let challenge = h.io.receive_head(client, &h.scope).await.unwrap();
                        let (_, echoed, b) = verify_session(
                            &h.identities[0],
                            challenge.value.value(),
                            Some(h.identities[1].node()),
                            "challenge",
                            true,
                        )
                        .unwrap();
                        assert_eq!(echoed, a);
                        assert_ne!(b, [0; 32]);
                        client = challenge.connection;
                        client.next_round().unwrap();
                        client =
                            h.io.send_head(client, faulty_head(h, phase, a, b, fault), &h.scope)
                                .await
                                .unwrap()
                                .connection;
                    }
                    assert!(client.state().session.is_none());
                }));
                assert!(
                    matches!(result, Err(Error::Unauthorized)),
                    "{phase} {fault:?}"
                );
                assert_eq!(h.hooks.installations.get(), 0);
                h.drive(h.reactor.drain()).unwrap();
                assert_eq!(h.hooks.slots.get(), 0);
            });
        }
    }
}

#[test]
fn cancellation_and_deadlines_abort_each_handshake_wait_and_release_slots() {
    for outbound in [false, true] {
        for second_round in [false, true] {
            for stop in ["cancel", "parent-deadline", "handshake-deadline"] {
                simulation(|h, sim, clock| {
                    let (client, server) = h.pair(sim);
                    let scope = TestScope::new(Duration::from_secs(if stop == "parent-deadline" {
                        2
                    } else {
                        30
                    }));
                    let (target, peer) = if outbound {
                        (client, server)
                    } else {
                        (server, client)
                    };
                    let operation = async {
                        if outbound {
                            h.connect(target, &scope).await
                        } else {
                            h.accept(target, &scope).await
                        }
                    };
                    let mut operation = std::pin::pin!(operation);
                    let peer_script = async {
                        if outbound {
                            let hello = h.io.receive_head(peer, &h.scope).await.unwrap();
                            if !second_round {
                                return hello.connection;
                            }
                            clock.advance(Duration::from_secs(1));
                            let (_, a, _) = verify_session(
                                &h.identities[1],
                                hello.value.value(),
                                None,
                                "hello",
                                false,
                            )
                            .unwrap();
                            let challenge = session_head(
                                &h.identities[1],
                                h.identities[0].node(),
                                "challenge",
                                &a,
                                &[7; 32],
                                true,
                            )
                            .unwrap();
                            let mut peer =
                                h.io.send_head(hello.connection, challenge, &h.scope)
                                    .await
                                    .unwrap()
                                    .connection;
                            peer.next_round().unwrap();
                            h.io.receive_head(peer, &h.scope).await.unwrap().connection
                        } else if second_round {
                            clock.advance(Duration::from_secs(1));
                            let hello = session_head(
                                &h.identities[0],
                                h.identities[1].node(),
                                "hello",
                                &[3; 32],
                                &[0; 32],
                                false,
                            )
                            .unwrap();
                            h.io.exchange_head(peer, hello, &h.scope)
                                .await
                                .unwrap()
                                .connection
                        } else {
                            peer
                        }
                    };
                    let mut peer_script = std::pin::pin!(peer_script);
                    let peer = h.drive(poll_fn(|cx| {
                        assert!(
                            operation.as_mut().poll(cx).is_pending(),
                            "handshake finished before stalled phase"
                        );
                        peer_script.as_mut().poll(cx)
                    }));
                    let mut cx = std::task::Context::from_waker(Waker::noop());
                    for _ in 0..4 {
                        h.reactor.poll_budgeted(64).unwrap();
                        assert!(operation.as_mut().poll(&mut cx).is_pending());
                    }
                    let expected = if stop == "cancel" {
                        scope.cancellation.cancel().unwrap();
                        Error::Runtime(uring_runtime::Error::Cancelled)
                    } else {
                        let seconds = if stop == "parent-deadline" { 2 } else { 5 };
                        let remaining = Duration::from_secs(seconds - u64::from(second_round));
                        clock.advance(remaining - Duration::from_nanos(1));
                        h.reactor.poll_budgeted(64).unwrap();
                        assert!(operation.as_mut().poll(&mut cx).is_pending());
                        clock.advance(Duration::from_nanos(1));
                        Error::DeadlineExceeded
                    };
                    assert!(
                        matches!(h.drive(operation.as_mut()), Err(error) if error == expected),
                        "outbound={outbound} second_round={second_round} stop={stop}"
                    );
                    assert_eq!(h.hooks.installations.get(), 0);
                    drop(peer);
                    h.drive(h.reactor.drain()).unwrap();
                    assert_eq!(h.hooks.slots.get(), 0);
                });
            }
        }
    }
}

#[test]
fn pooled_sessions_reject_wrong_peer_expiry_and_canceled_parent_without_io() {
    for fault in ["peer", "expired", "cancel", "deadline"] {
        simulation(|h, sim, clock| {
            let (client, server) = h.pair(sim);
            let (client, server) = h.drive(join(
                h.connect(client, &h.scope),
                h.accept(server, &h.scope),
            ));
            let (client, server) = h.application_round(client.unwrap(), server.unwrap());
            let socket = Rc::downgrade(&client.socket());
            drop(client);
            let client = h.drive(h.pool.checkout(&Endpoint, &h.scope)).unwrap();
            assert!(socket.ptr_eq(&Rc::downgrade(&client.socket())));
            drop(socket);
            let scope = TestScope::new(Duration::from_secs(7200));
            match fault {
                "expired" => clock.advance(Duration::from_secs(3600)),
                "cancel" => scope.cancellation.cancel().unwrap(),
                "deadline" => clock.advance(Duration::from_secs(7200)),
                _ => (),
            }
            let trace = sim.trace();
            let peer = h.identities[if fault == "peer" { 2 } else { 1 }].node();
            let result = h.drive(connect(
                &h.io,
                client,
                h.identities[0].clone(),
                peer,
                &scope,
            ));
            let expected = match fault {
                "peer" => Error::Unauthorized,
                "cancel" => Error::Runtime(uring_runtime::Error::Cancelled),
                _ => Error::DeadlineExceeded,
            };
            assert!(matches!(result, Err(error) if error == expected), "{fault}");
            let new_events = sim.trace();
            assert!(
                new_events[trace.len()..]
                    .iter()
                    .all(|event| event.operation != "send" && event.operation != "recv")
            );
            if fault != "peer" {
                assert!(
                    matches!(h.drive(h.accept(server, &scope)), Err(error) if error == expected)
                );
            }
        });
    }
}
