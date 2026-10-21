//! Public contracts for persistent links: two simulated nodes exchanging
//! pages in both directions over one link, and every failure path a caller
//! can see.
#![cfg(feature = "simulation")]

use std::{
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    task::{Context, Poll},
    time::{Duration, Instant},
};

use racer_platform::environment::{Environment, SimulationClock};
use rdma_verbs::{
    Arrival, BACKOFF_MIN, BufferState, COMMAND_QUEUE, Client, Epoch, Error, Granted, LinkKey,
    LinkState, NodeConfig, Op, Opened, Owner, SETUP_TIMEOUT, Setup, Source,
    simulation::{Device, Fault, Operation, Simulation},
};

const LEN: usize = 4096;
const BUFFERS: usize = 4;
const ID: u64 = 0xfeed;
const SECOND: Duration = Duration::from_secs(1);

struct Charge(Arc<AtomicUsize>);

impl Drop for Charge {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::AcqRel);
    }
}

struct Node {
    clients: Vec<Client>,

    owner: Option<Owner>,

    charge: Arc<AtomicUsize>,
}

impl Node {
    fn owner(&mut self) -> &mut Owner {
        self.owner.as_mut().unwrap()
    }
}

struct World {
    sim: Simulation,

    clock: SimulationClock,

    _environment: racer_platform::environment::Guard,

    a: Node,

    b: Node,
}

/// `a`'s key for its link to `b`, and the reverse.
const TO_B: LinkKey = LinkKey {
    peer: [2; 32],
    rail: 7,
};

const TO_A: LinkKey = LinkKey {
    peer: [1; 32],
    rail: 7,
};

fn config(charge: &Arc<AtomicUsize>) -> NodeConfig {
    charge.fetch_add(1, Ordering::AcqRel);
    NodeConfig {
        selector: Box::new(|ports| {
            assert_eq!(ports.len(), 1);
            Ok(vec![(7, 0)])
        }),
        pool_bytes: BUFFERS * LEN + 17,
        buffer_len: LEN,
        max_links: 2,
        grants_per_link: 2,
        idle: Duration::from_secs(120),
        mr_remote_write: false,
        zero_copy: false,
        guard: Arc::new(Charge(charge.clone())),
    }
}

fn node(sim: &Simulation, n: u8, environment: &Environment) -> Node {
    let view = sim
        .with_devices(vec![Device::new(format!("sim{n}"), [n; 16])])
        .unwrap();
    let (clients, port) = rdma_verbs::node(2).unwrap();
    let owner = {
        let _scope = view.enter();
        let _environment = environment.enter();
        Owner::new(port)
    };
    Node {
        clients,
        owner: Some(owner),
        charge: Arc::new(AtomicUsize::new(0)),
    }
}

fn world() -> World {
    let sim = Simulation::new();
    let clock = SimulationClock::new(7);
    let environment = clock.environment(1);
    let mut a = node(&sim, 1, &environment);
    let mut b = node(&sim, 2, &environment);
    let guard = environment.enter();
    for node in [&mut a, &mut b] {
        let activation = node.clients[0].activate(config(&node.charge));
        node.owner().poll_budgeted(4).unwrap();
        let selected = ready(&activation).unwrap();
        assert_eq!(selected[0].tag, 7);
        assert_eq!(node.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
    }
    World {
        sim,
        clock,
        _environment: guard,
        a,
        b,
    }
}

fn now() -> Instant {
    racer_platform::environment::now()
}

fn ready<T>(op: &Op<T>) -> rdma_verbs::Result<T> {
    match op.poll(&mut Context::from_waker(futures::task::noop_waker_ref())) {
        Poll::Ready(result) => result,
        Poll::Pending => panic!("op still pending"),
    }
}

fn pending<T>(op: &Op<T>) -> bool {
    op.poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
        .is_pending()
}

/// Poll both owners several rounds.
fn pump(w: &mut World) {
    for _ in 0..12 {
        if let Some(owner) = w.a.owner.as_mut() {
            owner.poll_budgeted(64).unwrap();
        }
        if let Some(owner) = w.b.owner.as_mut() {
            owner.poll_budgeted(64).unwrap();
        }
    }
}

/// Pump, then take the op's result.
fn wait<T>(w: &mut World, op: &Op<T>) -> rdma_verbs::Result<T> {
    pump(w);
    ready(op)
}

/// `a` opens, `b` accepts, `a` connects. Returns both epochs.
fn link_up(w: &mut World) -> (Epoch, Epoch) {
    let opened = w.a.clients[0].open(TO_B);
    let Opened { epoch: ea, setup } = wait(w, &opened).unwrap();
    assert_eq!(w.a.clients[1].link(TO_B), LinkState::Connecting(ea));
    let accepted = w.b.clients[0].accept(TO_A, setup, ID, false);
    let Opened {
        epoch: eb,
        setup: remote,
    } = wait(w, &accepted).unwrap();
    assert!(remote.recv_credits >= 2);
    let connected = w.a.clients[0].connect(TO_B, ea, remote, ID);
    wait(w, &connected).unwrap();
    pump(w);
    assert_eq!(
        w.a.clients[1].link(TO_B),
        LinkState::Ready {
            epoch: ea,
            link_id: ID
        }
    );
    assert_eq!(
        w.b.clients[1].link(TO_A),
        LinkState::Ready {
            epoch: eb,
            link_id: ID
        }
    );
    (ea, eb)
}

fn bytes(data: Vec<u8>) -> Source {
    Source::Bytes(Arc::new(data))
}

fn page(arrival: rdma_verbs::Result<Arrival>) -> rdma_verbs::RegisteredPage {
    match arrival.unwrap() {
        Arrival::Page(page) => page,
        Arrival::Released => panic!("released"),
    }
}

/// Close both nodes and require them drained, uncharged, and empty.
fn shutdown(mut w: World) {
    w.a.clients[0].close();
    w.b.clients[1].close();
    pump(&mut w);
    assert!(w.a.clients[0].drained() && w.b.clients[0].drained());
    assert_eq!(w.a.charge.load(Ordering::Acquire), 0);
    assert_eq!(w.b.charge.load(Ordering::Acquire), 0);
    assert_eq!(w.sim.live_resources(), 0);
}

#[test]
/// Both nodes grant and write concurrently from every worker over one link
/// per peer; every buffer comes back.
fn pages_flow_both_ways_over_one_link() {
    let mut w = world();
    let (ea, eb) = link_up(&mut w);
    for round in 0..10u8 {
        let deadline = now() + 5 * SECOND;
        let worker = usize::from(round % 2);
        let into_a = w.a.clients[worker].grant(TO_B, ea, [round; 16], 100, deadline);
        let into_b = w.b.clients[worker].grant(TO_A, eb, [round; 16], 200, deadline);
        pump(&mut w);
        let Granted {
            tag: tag_a,
            arrival: arrival_a,
        } = ready(&into_a).unwrap();
        let Granted {
            tag: tag_b,
            arrival: arrival_b,
        } = ready(&into_b).unwrap();
        let to_a =
            w.b.clients[1 - worker].write(TO_A, eb, tag_a, bytes(vec![round; 100]), deadline);
        let to_b =
            w.a.clients[1 - worker].write(TO_B, ea, tag_b, bytes(vec![!round; 200]), deadline);
        pump(&mut w);
        assert_eq!((ready(&to_a), ready(&to_b)), (Ok(()), Ok(())));
        assert_eq!(page(ready(&arrival_a)).bytes(), [round; 100]);
        assert_eq!(page(ready(&arrival_b)).bytes(), [!round; 200]);
    }
    assert_eq!(w.a.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
    assert_eq!(w.b.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
    // A staged send buffer from the client works too.
    let deadline = now() + 5 * SECOND;
    let granted = w.b.clients[0].grant(TO_A, eb, [9; 16], 4, deadline);
    let granted = wait(&mut w, &granted).unwrap();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let Poll::Ready(Ok(mut staged)) = w.a.clients[1].send_buffer(7, 4, &mut cx) else {
        panic!("no send buffer");
    };
    staged.bytes_mut().copy_from_slice(b"zero");
    assert!(matches!(
        w.a.clients[1].send_buffer(9, 4, &mut cx),
        Poll::Ready(Err(Error::Unavailable))
    ));
    let written = w.a.clients[1].write(TO_B, ea, granted.tag, Source::Staged(staged), deadline);
    assert_eq!(wait(&mut w, &written), Ok(()));
    assert_eq!(page(ready(&granted.arrival)).bytes(), b"zero");
    shutdown(w);
}

#[test]
/// The sender releases instead of writing; a write for a tag never
/// granted times out; neither breaks the link.
fn release_and_wrong_tag_fall_back_without_break() {
    let mut w = world();
    let (ea, eb) = link_up(&mut w);
    let deadline = now() + 5 * SECOND;
    let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
    let granted = wait(&mut w, &granted).unwrap();
    let released = w.b.clients[0].release(TO_A, eb, granted.tag);
    assert_eq!(wait(&mut w, &released), Ok(()));
    assert!(matches!(ready(&granted.arrival), Ok(Arrival::Released)));
    let wrong = w.b.clients[0].write(TO_A, eb, 999, bytes(vec![1]), now() + SECOND);
    pump(&mut w);
    assert!(pending(&wrong));
    w.clock.advance(SECOND);
    assert_eq!(wait(&mut w, &wrong), Err(Error::DeadlineExceeded));
    // A stale epoch is refused.
    let stale = w.b.clients[0].write(TO_A, Epoch(eb.0 + 100), 1, bytes(vec![1]), deadline);
    assert_eq!(wait(&mut w, &stale), Err(Error::Unavailable));
    assert!(matches!(w.a.clients[0].link(TO_B), LinkState::Ready { .. }));
    shutdown(w);
}

#[test]
/// Fabric faults during a write break the link: the arrival fails with
/// `Unavailable`, no page is produced, buffers return, and the receiver
/// backs off.
fn fabric_faults_break_without_producing_pages() {
    for fault in [
        Fault::DuplicateImmediate,
        Fault::Drop,
        Fault::ByteLength(11),
        Fault::Completion(10),
    ] {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let deadline = now() + 5 * SECOND;
        let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        // Only node b posts writes with immediate.
        w.sim.fault_on(Operation::WriteImm, None, fault);
        let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![1; 10]), deadline);
        pump(&mut w);
        if fault == Fault::Drop {
            assert_eq!(ready(&written), Ok(()));
        }
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let arrival = match granted.arrival.poll(&mut cx) {
            Poll::Ready(result) => result,
            Poll::Pending => {
                // Nothing reached a: its exposed grant breaks at the deadline.
                w.clock.advance(5 * SECOND);
                pump(&mut w);
                ready(&granted.arrival)
            }
        };
        assert_eq!(arrival.err(), Some(Error::Unavailable), "{fault:?}");
        assert!(
            matches!(w.a.clients[0].link(TO_B), LinkState::Backoff(_)),
            "{fault:?}"
        );
        assert_eq!(w.a.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
        let reopen = w.a.clients[0].open(TO_B);
        assert_eq!(
            wait(&mut w, &reopen).err(),
            Some(Error::Overloaded),
            "backing off"
        );
        shutdown(w);
    }
}

#[test]
/// `break_link` aborts outstanding ops; backoff doubles and then lets
/// `open` through again.
fn abort_and_backoff() {
    let mut w = world();
    let (ea, _) = link_up(&mut w);
    let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, now() + 5 * SECOND);
    let granted = wait(&mut w, &granted).unwrap();
    w.a.clients[1].break_link(TO_B, ea);
    pump(&mut w);
    assert_eq!(ready(&granted.arrival).err(), Some(Error::Unavailable));
    let LinkState::Backoff(first) = w.a.clients[0].link(TO_B) else {
        panic!("not backing off");
    };
    let delay = first - now();
    assert!(
        delay >= BACKOFF_MIN * 8 / 10 && delay <= BACKOFF_MIN * 12 / 10,
        "{delay:?}"
    );
    // The peer notices on its next use or at its idle time; break it too.
    w.clock.advance(delay);
    pump(&mut w);
    assert_eq!(w.a.clients[0].link(TO_B), LinkState::Idle);
    // A setup that never completes times out and doubles the backoff.
    let opened = w.a.clients[0].open(TO_B);
    wait(&mut w, &opened).unwrap();
    w.clock.advance(SETUP_TIMEOUT);
    pump(&mut w);
    let LinkState::Backoff(second) = w.a.clients[0].link(TO_B) else {
        panic!("not backing off");
    };
    let delay = second - now();
    assert!(
        delay >= BACKOFF_MIN * 16 / 10 && delay <= BACKOFF_MIN * 24 / 10,
        "{delay:?}"
    );
    shutdown(w);
}

#[test]
/// Setup rules: busy keys, unknown rails, wrong epochs, bad credits, and
/// a new accept replacing a ready link.
fn setup_rules() {
    let mut w = world();
    let (ea, eb) = link_up(&mut w);
    let again = w.a.clients[0].open(TO_B);
    assert_eq!(wait(&mut w, &again).err(), Some(Error::Overloaded));
    let unknown = w.a.clients[0].open(LinkKey {
        peer: [3; 32],
        rail: 9,
    });
    assert_eq!(wait(&mut w, &unknown).err(), Some(Error::Unavailable));
    let other = w.a.clients[0].open(LinkKey {
        peer: [4; 32],
        rail: 7,
    });
    let setup = Setup {
        endpoint: wait(&mut w, &other).unwrap().setup.endpoint,
        recv_credits: 1,
        grants_per_link: 2,
    };
    let bad = w.b.clients[0].accept(TO_A, setup, ID, false);
    assert_eq!(wait(&mut w, &bad).err(), Some(Error::InvalidRequest));
    let no_grants = Setup {
        recv_credits: 8,
        grants_per_link: 0,
        ..setup
    };
    let bad = w.b.clients[0].accept(TO_A, no_grants, ID, false);
    assert_eq!(wait(&mut w, &bad).err(), Some(Error::InvalidRequest));
    let wrong = w.a.clients[0].connect(TO_B, Epoch(ea.0 + 50), setup, ID);
    assert_eq!(wait(&mut w, &wrong).err(), Some(Error::Unavailable));
    let ready_link = w.a.clients[0].connect(TO_B, ea, setup, ID);
    assert_eq!(wait(&mut w, &ready_link).err(), Some(Error::InvalidRequest));
    // max_links is 2 per device: a third link on the rail is refused.
    let third = w.a.clients[0].open(LinkKey {
        peer: [5; 32],
        rail: 7,
    });
    assert_eq!(wait(&mut w, &third).err(), Some(Error::Overloaded));
    // b's peer restarted: a new offer replaces the ready link.
    let granted = w.b.clients[0].grant(TO_A, eb, [1; 16], 10, now() + 5 * SECOND);
    let granted = wait(&mut w, &granted).unwrap();
    let offer = Setup {
        endpoint: setup.endpoint,
        recv_credits: 8,
        grants_per_link: 2,
    };
    let replaced = w.b.clients[0].accept(TO_A, offer, ID + 1, false);
    let replaced = wait(&mut w, &replaced).unwrap();
    assert_ne!(replaced.epoch, eb);
    assert_eq!(ready(&granted.arrival).err(), Some(Error::Unavailable));
    pump(&mut w);
    shutdown(w);
}

#[test]
/// Closing one node sends `Close`: the peer returns to `Idle`, not backoff.
/// An idle link closes itself the same way.
fn peer_close_and_idle_close() {
    for idle in [false, true] {
        let mut w = world();
        link_up(&mut w);
        if idle {
            w.clock.advance(Duration::from_secs(120));
            pump(&mut w);
            assert_eq!(w.a.clients[0].link(TO_B), LinkState::Idle);
        } else {
            w.a.clients[0].close();
            pump(&mut w);
            assert!(w.a.clients[0].drained());
        }
        assert_eq!(w.b.clients[0].link(TO_A), LinkState::Idle);
        shutdown(w);
    }
}

#[test]
/// A full command queue reports `Overloaded`; `poll_ready` waits for room.
fn full_queue_is_overloaded() {
    let mut w = world();
    let key = LinkKey {
        peer: [8; 32],
        rail: 1,
    };
    let ops: Vec<_> = (0..COMMAND_QUEUE)
        .map(|_| w.a.clients[0].open(key))
        .collect();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(w.a.clients[0].poll_ready(&mut cx).is_pending());
    assert_eq!(
        ready(&w.a.clients[0].open(key)).err(),
        Some(Error::Overloaded)
    );
    assert!(w.a.clients[1].poll_ready(&mut cx).is_ready(), "per worker");
    pump(&mut w);
    assert!(w.a.clients[0].poll_ready(&mut cx).is_ready());
    for op in ops {
        assert_eq!(ready(&op).err(), Some(Error::Unavailable));
    }
    shutdown(w);
}

#[test]
/// Shutdown waits for pages held by the caller; reopen is blocked until
/// then. A failed destroy keeps the node from ever draining.
fn shutdown_waits_for_pages_and_destroys() {
    let mut w = world();
    let (ea, eb) = link_up(&mut w);
    let deadline = now() + 5 * SECOND;
    let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
    let granted = wait(&mut w, &granted).unwrap();
    let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![3; 10]), deadline);
    assert_eq!(wait(&mut w, &written), Ok(()));
    let held = page(ready(&granted.arrival));
    w.a.clients[0].close();
    pump(&mut w);
    assert!(!w.a.clients[0].drained());
    assert_eq!(w.a.clients[0].reopen(), Err(Error::Overloaded));
    assert_eq!(
        ready(&w.a.clients[0].open(TO_B)).err(),
        Some(Error::Unavailable)
    );
    assert_eq!(held.bytes(), [3; 10]);
    std::thread::spawn(move || drop(held)).join().unwrap();
    pump(&mut w);
    assert!(w.a.clients[0].drained());
    assert_eq!(w.a.charge.load(Ordering::Acquire), 0);
    // Reopen and activate a fresh round.
    w.a.clients[0].reopen().unwrap();
    let activation = w.a.clients[1].activate(config(&w.a.charge));
    assert!(wait(&mut w, &activation).is_ok());
    shutdown(w);
}

#[test]
/// A failed destroy is retried and blocks draining until it succeeds;
/// dropping the owner while it fails leaks the link and its charge.
fn failed_destroy_blocks_drain_or_leaks() {
    let mut w = world();
    link_up(&mut w);
    w.sim.reject(Operation::Stop, None, true);
    w.a.clients[0].close();
    pump(&mut w);
    assert!(!w.a.clients[0].drained());
    assert!(matches!(w.a.clients[0].link(TO_B), LinkState::Breaking));
    w.sim.reject(Operation::Stop, None, false);
    pump(&mut w);
    assert!(w.a.clients[0].drained());
    // Dropping the owner while destroy fails leaks for good.
    let mut w2 = world();
    link_up(&mut w2);
    w2.sim.reject(Operation::Stop, None, true);
    drop(w2.a.owner.take());
    assert_eq!(w2.a.charge.load(Ordering::Acquire), 1, "leaked charge");
    w2.sim.reject(Operation::Stop, None, false);
    drop(w2);
    shutdown(w);
}

#[test]
/// Dropping the owner fails outstanding ops, makes clients unavailable,
/// and leaves held pages readable.
fn owner_drop() {
    let mut w = world();
    let (ea, eb) = link_up(&mut w);
    let deadline = now() + 5 * SECOND;
    let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
    let granted = wait(&mut w, &granted).unwrap();
    let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![4; 10]), deadline);
    assert_eq!(wait(&mut w, &written), Ok(()));
    let held = page(ready(&granted.arrival));
    let second = w.a.clients[0].grant(TO_B, ea, [2; 16], 10, deadline);
    let second = wait(&mut w, &second).unwrap();
    let queued = w.a.clients[1].grant(TO_B, ea, [3; 16], 10, deadline);
    drop(w.a.owner.take());
    assert_eq!(ready(&second.arrival).err(), Some(Error::Unavailable));
    assert_eq!(ready(&queued).err(), Some(Error::Cancelled));
    assert_eq!(
        ready(&w.a.clients[0].open(TO_B)).err(),
        Some(Error::Unavailable)
    );
    assert_eq!(w.a.clients[0].reopen(), Err(Error::Unavailable));
    assert_eq!(w.a.clients[0].link(TO_B), LinkState::Idle);
    assert_eq!(held.bytes(), [4; 10]);
    drop(held);
    assert_eq!(
        w.a.charge.load(Ordering::Acquire),
        1,
        "region leaked with the page out"
    );
    w.b.clients[0].close();
    pump(&mut w);
    assert!(w.b.clients[0].drained());
}

#[test]
/// Dropping a worker's client keeps its ops and the node running.
fn worker_drop() {
    let mut w = world();
    let (ea, eb) = link_up(&mut w);
    let deadline = now() + 5 * SECOND;
    let granted = w.a.clients[1].grant(TO_B, ea, [1; 16], 10, deadline);
    drop(w.a.clients.pop());
    let granted = wait(&mut w, &granted).unwrap();
    let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![5; 10]), deadline);
    assert_eq!(wait(&mut w, &written), Ok(()));
    assert_eq!(page(ready(&granted.arrival)).bytes(), [5; 10]);
    shutdown(w);
}

#[test]
/// The owner can be hosted by `WithNative` and drained by its fence.
fn owner_hosted_by_with_native() {
    use racer_platform::group::Service;

    let mut w = world();
    let owner = w.a.owner.take().unwrap();
    let mut service = rdma_verbs::WithNative::with_host(inner::Inner, owner);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    service.register_driver(cx.waker());
    let opened = w.a.clients[0].open(TO_B);
    service.poll(&mut cx, 8).unwrap();
    assert!(ready(&opened).is_ok());
    futures::executor::block_on(service.fence(&inner::Scope)).unwrap();
    assert!(w.a.clients[0].drained());
    drop(service);
    w.b.clients[0].close();
    pump(&mut w);
    assert_eq!(w.sim.live_resources(), 0);
}

#[test]
/// Drain and fence drive inner work alongside the native drain. The inner
/// side holds a pool buffer (like a queued crypto Stage job) that it only
/// returns while its own drain or fence runs; draining the native side first
/// would wait forever.
fn with_native_drives_inner_work_holding_buffers() {
    use racer_platform::group::Service;

    for fence in [false, true] {
        let mut w = world();
        let owner = w.a.owner.take().unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(staged)) = w.a.clients[1].send_buffer(7, 4, &mut cx) else {
            panic!("no staged buffer");
        };
        assert_eq!(
            w.a.clients[1].buffers(7, BufferState::Free),
            Some(BUFFERS - 1)
        );
        let holder = inner::Holder(Some(staged));
        let mut service = rdma_verbs::WithNative::with_host(holder, owner);
        service.register_driver(cx.waker());
        service.poll(&mut cx, 8).unwrap();
        let scope = inner::Scope;
        let mut teardown = if fence {
            service.fence(&scope)
        } else {
            service.drain(&scope)
        };
        let mut finished = None;
        for _ in 0..64 {
            if let Poll::Ready(result) = teardown.as_mut().poll(&mut cx) {
                finished = Some(result);
                break;
            }
        }
        assert!(
            matches!(finished, Some(Ok(()))),
            "teardown stalled (fence={fence})"
        );
        drop(teardown);
        assert!(w.a.clients[0].drained());
        drop(service);
        w.b.clients[0].close();
        pump(&mut w);
        assert_eq!(w.sim.live_resources(), 0);
    }
}

mod inner {
    use std::task::{Context, Waker};

    use racer_platform::{Operation, group::Service};

    #[derive(Clone, Copy, Debug)]
    pub struct Failure;

    impl From<racer_platform::Error> for Failure {
        fn from(_: racer_platform::Error) -> Self {
            Self
        }
    }

    impl From<racer_platform::uring::Error> for Failure {
        fn from(_: racer_platform::uring::Error) -> Self {
            Self
        }
    }

    impl From<rdma_verbs::Error> for Failure {
        fn from(_: rdma_verbs::Error) -> Self {
            Self
        }
    }

    #[derive(Clone)]
    pub struct Scope;

    impl racer_platform::scope::LibraryScope for Scope {
        type Error = Failure;

        fn check(&self) -> Result<(), Failure> {
            Ok(())
        }

        fn cancellation(&self) -> Option<&racer_platform::Cancellation> {
            None
        }
    }

    pub struct Inner;

    /// Holds a buffer until its drain or fence runs, like a queued crypto job.
    pub struct Holder(pub Option<rdma_verbs::SendBuffer>);

    impl Service<Scope> for Holder {
        fn waker(&self) -> Result<Waker, Failure> {
            Ok(Waker::noop().clone())
        }

        fn register_driver(&self, _: &Waker) {}

        fn start<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async { Ok(()) })
        }

        fn poll(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Failure> {
            Ok(())
        }

        fn wait_timeout(&self, maximum: std::time::Duration) -> std::time::Duration {
            maximum
        }

        fn drain<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async move {
                self.0.take();
                Ok(())
            })
        }

        fn stop_admission(&mut self) -> Result<(), Failure> {
            Ok(())
        }

        fn close(&mut self) -> Result<(), Failure> {
            Ok(())
        }

        fn fence<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async move {
                self.0.take();
                Ok(())
            })
        }

        fn shutdown<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async { Ok(()) })
        }
    }

    impl Service<Scope> for Inner {
        fn waker(&self) -> Result<Waker, Failure> {
            Ok(Waker::noop().clone())
        }

        fn register_driver(&self, _: &Waker) {}

        fn start<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async { Ok(()) })
        }

        fn poll(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Failure> {
            Ok(())
        }

        fn wait_timeout(&self, maximum: std::time::Duration) -> std::time::Duration {
            maximum
        }

        fn drain<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async { Ok(()) })
        }

        fn stop_admission(&mut self) -> Result<(), Failure> {
            Ok(())
        }

        fn close(&mut self) -> Result<(), Failure> {
            Ok(())
        }

        fn fence<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async { Ok(()) })
        }

        fn shutdown<'a>(&'a mut self, _: &'a Scope) -> Operation<'a, (), Failure> {
            Box::pin(async { Ok(()) })
        }
    }
}
