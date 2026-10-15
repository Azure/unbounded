use http1::{
    Codec, Header, MessageHead, StartLine,
    connection::{ConnectionLease, HttpIo, HttpPool, PoolConfig, State},
};
use peer_wire::{self as session, Error, Opaque, RequestMac, Result, Session, Signatures};
use racer_control_wire::{BundleGeneration, ClusterId, KeyringBundle, NodeId, SCHEMA_VERSION};
use racer_crypto::identity::{
    Certificates, KeyEpochs, Keyring,
    test_util::{ca, issue},
};
use std::{
    cell::Cell,
    future::{Future, poll_fn},
    rc::Rc,
    sync::Arc,
    task::{Poll, Waker},
    time::{Duration, Instant, SystemTime},
};
use uring_runtime::{
    Scope,
    environment::{SimulationClock, now},
    reactor::{Reactor, SocketAddress, simulation::Simulation},
};

struct Mac(bool);

impl RequestMac for Mac {
    fn sign(&self, _: &Keyring, _: &mut MessageHead) -> Result<()> {
        if self.0 {
            Ok(())
        } else {
            Err(Error::Unauthorized)
        }
    }

    fn verify(&self, _: &Keyring, _: &MessageHead) -> Result<()> {
        if self.0 {
            Ok(())
        } else {
            Err(Error::Unauthorized)
        }
    }
}

struct Authority {
    keys: Rc<Keyring>,
    certificates: Rc<Certificates>,
    mac: Rc<dyn RequestMac>,
    signatures: Rc<Signatures>,
}

fn authorities() -> Vec<Authority> {
    let (ca, key) = ca();
    let roots = vec![ca.der().to_vec()];
    let cluster = ClusterId("00000099-1111-4111-8111-111111111111".into());
    (1..=3)
        .map(|n| {
            let node = NodeId(format!("{n:08}-1111-4111-8111-111111111111"));
            let (pending, chain) = issue(&ca, &key, &cluster, &node, |_| {});
            let identity = pending
                .accept(cluster.clone(), node.clone(), chain, &roots)
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node,
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots.clone(),
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
            let mac: Rc<dyn RequestMac> = Rc::new(Mac(true));
            let signatures = Rc::new(Signatures::new(
                keys.clone(),
                certificates.clone(),
                mac.clone(),
            ));
            Authority {
                keys,
                certificates,
                mac,
                signatures,
            }
        })
        .collect()
}

#[derive(Clone)]
struct TestScope(Instant);

impl Scope for TestScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        if now() >= self.0 {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }
}

#[derive(Default)]
struct Policy(Option<Session>);

impl State<Error> for Policy {
    fn sign(&mut self, head: MessageHead) -> Result<MessageHead> {
        match &mut self.0 {
            Some(session) => session.sign(head),
            None => Ok(head),
        }
    }

    fn admit(&mut self, head: MessageHead) -> Result<MessageHead> {
        match &mut self.0 {
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
struct Hooks(Rc<Cell<usize>>);

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
        self.0.set(self.0.get() + 1);
        Ok(Slot(self.0.clone()))
    }

    fn stopped(&self) -> bool {
        false
    }
}

impl session::Context for Hooks {
    fn session(state: &Policy) -> Option<&Session> {
        state.0.as_ref()
    }

    fn session_mut(state: &mut Policy) -> &mut Option<Session> {
        &mut state.0
    }

    fn handshake_scope(parent: &TestScope) -> Result<TestScope> {
        parent.check()?;
        Ok(TestScope(parent.0.min(now() + Duration::from_secs(5))))
    }
}

type Lease = ConnectionLease<Hooks>;

struct Harness {
    reactor: Rc<TestReactor>,
    io: HttpIo<Hooks>,
    pool: HttpPool<Hooks>,
    hooks: Rc<Hooks>,
    scope: TestScope,
    authorities: Vec<Authority>,
}

impl Harness {
    fn new() -> Self {
        let reactor = Rc::new(TestReactor(Reactor::new(32, ())));
        let hooks = Rc::new(Hooks::default());
        Self {
            io: HttpIo::new(reactor.clone(), Codec::new(65536), hooks.clone(), 0, 0),
            pool: HttpPool::new(reactor.clone(), hooks.clone(), PoolConfig::default()),
            reactor,
            hooks,
            scope: TestScope(now() + Duration::from_secs(30)),
            authorities: authorities(),
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
        let client = self
            .drive(self.pool.checkout(&Endpoint, &self.scope))
            .unwrap();
        let server = ConnectionLease::from_reserved(
            listener.as_sim().unwrap().accept().unwrap(),
            self.hooks.outbound_slot().unwrap(),
            Policy::default(),
        )
        .unwrap();
        (client, server)
    }

    fn handshake(&self, client: Lease, server: Lease, local: Rc<Signatures>) -> (Lease, Lease) {
        let remote = self.authorities[1].signatures.clone();
        let (client, server) = self.drive(join(
            session::connect(&self.io, client, local, remote.node(), &self.scope),
            session::accept(&self.io, server, remote.clone(), &self.scope),
        ));
        (client.unwrap(), server.unwrap())
    }

    fn round(&self, client: Lease, server: Lease) -> (Lease, Lease) {
        let request = MessageHead {
            start: StartLine::Request {
                method: "POST".into(),
                target: "/test".into(),
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

    fn reuse(&self, outbound: bool, lease: Lease, signatures: Rc<Signatures>) -> Result<Lease> {
        if outbound {
            self.drive(session::connect(
                &self.io,
                lease,
                signatures,
                self.authorities[1].signatures.node(),
                &self.scope,
            ))
        } else {
            self.drive(session::accept(&self.io, lease, signatures, &self.scope))
        }
    }
}

async fn join<A: Future, B: Future>(a: A, b: B) -> (A::Output, B::Output) {
    let mut a = std::pin::pin!(a);
    let mut b = std::pin::pin!(b);
    let (mut left, mut right) = (None, None);
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

fn simulation(test: impl FnOnce(&Harness, &Simulation)) {
    let clock = SimulationClock::new_at(899, Instant::now(), SystemTime::now());
    let _clock = clock.environment(0).enter();
    let sim = Simulation::new();
    let _sim = sim.enter();
    let h = Harness::new();
    test(&h, &sim);
    h.pool.close();
    h.drive(h.reactor.drain()).unwrap();
    assert_eq!(h.reactor.in_flight(), 0);
    assert_eq!(h.hooks.0.get(), 0);
}

#[test]
fn both_fast_paths_reuse_the_exact_authority_without_io() {
    for outbound in [false, true] {
        simulation(|h, sim| {
            let local = h.authorities[0].signatures.clone();
            let (client, server) = h.pair(sim);
            let (client, server) = h.handshake(client, server, local.clone());
            let (client, server) = h.round(client, server);
            let socket = Rc::downgrade(&client.socket());
            drop(client);
            let client = h.drive(h.pool.checkout(&Endpoint, &h.scope)).unwrap();
            assert!(socket.ptr_eq(&Rc::downgrade(&client.socket())));
            let trace = sim.trace();
            let client = h.reuse(outbound, client, local).unwrap();
            assert_eq!(sim.trace(), trace);
            let (_client, _server) = h.round(client, server);
        });
    }
}

#[test]
fn both_fast_paths_poison_changed_authority_and_allow_fresh_recovery() {
    for outbound in [false, true] {
        for change in ["node", "mac", "equivalent-owner"] {
            simulation(|h, sim| {
                let original = &h.authorities[0];
                let replacement = match change {
                    "node" => h.authorities[2].signatures.clone(),
                    "mac" => Rc::new(Signatures::new(
                        original.keys.clone(),
                        original.certificates.clone(),
                        Rc::new(Mac(false)),
                    )),
                    _ => Rc::new(Signatures::new(
                        original.keys.clone(),
                        original.certificates.clone(),
                        original.mac.clone(),
                    )),
                };
                assert!(!Rc::ptr_eq(&original.signatures, &replacement));
                if change != "node" {
                    assert_eq!(original.signatures.node(), replacement.node());
                }
                let (client, server) = h.pair(sim);
                let (client, server) = h.handshake(client, server, original.signatures.clone());
                let (client, server) = h.round(client, server);
                let socket = Rc::downgrade(&client.socket());
                drop(client);
                let client = h.drive(h.pool.checkout(&Endpoint, &h.scope)).unwrap();
                assert!(socket.ptr_eq(&Rc::downgrade(&client.socket())));
                let (client, server) = h.round(client, server);
                assert!(client.is_reusable());
                let trace = sim.trace();
                let result = h.reuse(outbound, client, replacement.clone());
                assert!(
                    matches!(result, Err(Error::Unauthorized)),
                    "outbound={outbound} change={change}"
                );
                assert!(
                    socket.upgrade().is_none(),
                    "mismatched authority returned socket to pool"
                );
                assert!(
                    sim.trace()[trace.len()..]
                        .iter()
                        .all(|event| event.operation != "send" && event.operation != "recv")
                );
                drop(server);
                assert_eq!(h.hooks.0.get(), 0);
                let (client, server) = h.pair(sim);
                assert!(client.state().0.is_none());
                assert!(!socket.ptr_eq(&Rc::downgrade(&client.socket())));
                let local = if change == "mac" {
                    original.signatures.clone()
                } else {
                    replacement
                };
                let (client, server) = h.handshake(client, server, local);
                let (_client, _server) = h.round(client, server);
            });
        }
    }
}
