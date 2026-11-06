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

use racer_io::{
    SimClock,
    exec::{Next, Role},
};
use rdma_verbs::{
    Arrival, BACKOFF_MAX, BACKOFF_MIN, BreakReason, BufferState, COMMAND_QUEUE, Client,
    DRAIN_REPORT, EXPOSED_GRACE, Epoch, Error, Granted, LinkKey, LinkState, NodeConfig, Op, Opened,
    Owner, OwnerRole, SETUP_TIMEOUT, Setup, Source,
    simulation::{
        Device, Fault, MemfdMemory, Operation, Profile, Simulation, current_profile,
        default_profile,
    },
};

/// Page buffer length: registration mode needs whole 2 MiB pages.
fn len() -> usize {
    match current_profile() {
        Profile::Window => 4096,
        Profile::Registration => 2 << 20,
    }
}

/// Run `test` once per device profile; the failing profile is printed.
fn profiles(test: impl Fn()) {
    for profile in [Profile::Window, Profile::Registration] {
        let _profile = default_profile(profile);
        eprintln!("profile: {profile:?}");
        test();
    }
}

/// Run `test` in window mode only, for contracts registration mode does not
/// have (dma-buf device pools, implicit on-demand paging).
fn window_only(test: impl Fn()) {
    let _profile = default_profile(Profile::Window);
    test();
}
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

    clock: SimClock,

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
        pool_bytes: BUFFERS * len() + 17,
        buffer_len: len(),
        max_links: 2,
        grants_per_link: 2,
        idle: Duration::from_secs(120),
        guard: Arc::new(Charge(charge.clone())),
    }
}

fn node(sim: &Simulation, n: u8, ctx: racer_io::WorkerCtx) -> Node {
    let view = sim
        .with_devices(vec![Device::new(format!("sim{n}"), [n; 16])])
        .unwrap();
    let (clients, port) = rdma_verbs::node(2).unwrap();
    let owner = {
        let _scope = view.enter();
        Owner::new(port, ctx)
    };
    Node {
        clients,
        owner: Some(owner),
        charge: Arc::new(AtomicUsize::new(0)),
    }
}

fn world() -> World {
    let sim = Simulation::new();
    let clock = SimClock::new(7);
    let mut a = node(&sim, 1, clock.ctx(1));
    let mut b = node(&sim, 2, clock.ctx(1));
    for node in [&mut a, &mut b] {
        let activation = node.clients[0].activate(config(&node.charge));
        node.owner().poll_budgeted(4).unwrap();
        let selected = ready(&activation).unwrap();
        assert_eq!(selected[0].tag, 7);
        assert_eq!(node.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
    }
    World { sim, clock, a, b }
}

impl World {
    /// The world's simulated monotonic time.
    fn now(&self) -> Instant {
        self.clock.virtual_clock().now()
    }
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
    Source::new(Arc::new(data))
}

fn page(arrival: rdma_verbs::Result<Arrival>) -> rdma_verbs::RegisteredPage {
    match arrival.unwrap() {
        Arrival::Page(page) => page,
        Arrival::Released | Arrival::Device(_) => panic!("expected a host page"),
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
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        for round in 0..10u8 {
            let deadline = w.now() + 5 * SECOND;
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
        // A static source is written in place too, and no write takes a pool
        // buffer: page buffers only receive.
        static ZERO: &[u8] = b"zero";
        let deadline = w.now() + 5 * SECOND;
        let granted = w.b.clients[0].grant(TO_A, eb, [9; 16], 4, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let source = Source::new(Arc::new(ZERO));
        let written = w.a.clients[1].write(TO_B, ea, granted.tag, source, deadline);
        assert_eq!(w.a.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
        assert_eq!(wait(&mut w, &written), Ok(()));
        assert_eq!(page(ready(&granted.arrival)).bytes(), b"zero");
        shutdown(w);
    });
}

#[test]
/// Window mode prefetches write sources into the rail's implicit region from
/// any thread; registration mode, unknown rails, and closed nodes have
/// nothing to prefetch into. Prefetch is advice only: the write still delivers.
fn prefetch_maps_sources_on_window_rails_only() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let source: Arc<Vec<u8>> = Arc::new(vec![5; 64]);
        let window = current_profile() == Profile::Window;
        let client = &w.a.clients[1];
        let prefetched = std::thread::scope(|s| {
            s.spawn(|| client.prefetch(TO_B.rail, &source))
                .join()
                .unwrap()
        });
        let (address, length) = (source.as_ptr() as u64, source.len() as u32);
        assert_eq!(prefetched, Ok(window));
        // Only window mode logs: an earlier run may reuse the address.
        assert!(!window || rdma_verbs::simulation::prefetched(address, length));
        assert_eq!(w.a.clients[0].prefetch(TO_B.rail + 1, &source), Ok(false));
        assert_eq!(w.a.clients[0].prefetch(TO_B.rail, &[]), Ok(window));

        let deadline = w.now() + 5 * SECOND;
        let granted = w.b.clients[0].grant(TO_A, eb, [4; 16], 64, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let written = w.a.clients[0].write(TO_B, ea, granted.tag, Source::new(source), deadline);
        assert_eq!(wait(&mut w, &written), Ok(()));
        assert_eq!(page(ready(&granted.arrival)).bytes(), [5; 64]);

        // Teardown withdraws the handle before the implicit region drops.
        w.a.clients[0].close();
        pump(&mut w);
        assert_eq!(w.a.clients[0].prefetch(TO_B.rail, &[1]), Ok(false));
        shutdown(w);
    });
}

#[test]
/// A device without implicit on-demand paging cannot send in place, and
/// there is no copy path: activation fails, frees every native object, and
/// returns the charge. Once the device supports it, activation succeeds.
fn activation_requires_implicit_odp_and_releases_on_failure() {
    window_only(|| {
        let sim = Simulation::new();
        let clock = SimClock::new(7);
        let mut node = node(&sim, 1, clock.ctx(1));
        sim.implicit_odp(false);
        let activation = node.clients[0].activate(config(&node.charge));
        node.owner().poll_budgeted(4).unwrap();
        assert_eq!(ready(&activation).err(), Some(Error::Unavailable));
        // The capability check fails before any registration: the receive pool
        // is never registered on a device that lacks implicit ODP.
        let registers: Vec<i64> = sim
            .trace()
            .iter()
            .filter(|e| e.operation == Operation::Register)
            .map(|e| e.result)
            .collect();
        assert_eq!(registers, [0i64; 0], "no MR before the failed check");
        assert_eq!(node.charge.load(Ordering::Acquire), 0, "charge returned");
        assert_eq!(sim.live_resources(), 0, "nothing registered stays");
        assert_eq!(node.clients[0].buffers(7, BufferState::Free), None);
        sim.implicit_odp(true);
        let activation = node.clients[0].activate(config(&node.charge));
        node.owner().poll_budgeted(4).unwrap();
        assert_eq!(ready(&activation).unwrap()[0].tag, 7);
        assert_eq!(node.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
        node.clients[0].close();
        node.owner().poll_budgeted(4).unwrap();
        assert!(node.clients[0].drained());
        assert_eq!(node.charge.load(Ordering::Acquire), 0);
    });
}

#[test]
/// Grants per link are capped so each link's QP and CQ fit the device's
/// reported `max_qp_wr` and `max_cqe`: the advertised setup carries the
/// capped value, and links still come up.
fn grants_are_capped_by_device_limits() {
    profiles(|| {
        let sim = Simulation::new();
        // Registration posts 4 WRs per send: 4 * (4g + 8) <= 64 gives 2.
        // Window mode: 4g + 8 <= 64 gives 14.
        sim.device_limits(64, 2048, 0);
        let clock = SimClock::new(7);
        let mut a = node(&sim, 1, clock.ctx(1));
        let mut b = node(&sim, 2, clock.ctx(1));
        for node in [&mut a, &mut b] {
            let mut config = config(&node.charge);
            config.grants_per_link = 1000;
            let activation = node.clients[0].activate(config);
            node.owner().poll_budgeted(4).unwrap();
            assert_eq!(ready(&activation).unwrap()[0].tag, 7);
        }
        let mut w = World { sim, clock, a, b };
        let opened = w.a.clients[0].open(TO_B);
        let Opened { epoch, setup } = wait(&mut w, &opened).unwrap();
        let expected = match current_profile() {
            Profile::Window => 14,
            Profile::Registration => 2,
        };
        assert_eq!(setup.grants_per_link, expected);
        let accepted = w.b.clients[0].accept(TO_A, setup, ID, false);
        let remote = wait(&mut w, &accepted).unwrap().setup;
        assert_eq!(remote.grants_per_link, expected);
        let connected = w.a.clients[0].connect(TO_B, epoch, remote, ID);
        wait(&mut w, &connected).unwrap();
        pump(&mut w);
        assert!(matches!(w.a.clients[1].link(TO_B), LinkState::Ready { .. }));
        shutdown(w);
    });
}

#[test]
/// A device whose limits cannot fit one grant's QP and CQ fails activation
/// instead of failing every link's QP creation later.
fn activation_fails_when_device_limits_fit_no_grant() {
    profiles(|| {
        let sim = Simulation::new();
        // One grant needs 12 send WRs (48 in registration mode) and 6 receives.
        sim.device_limits(8, 2048, 0);
        let clock = SimClock::new(7);
        let mut node = node(&sim, 1, clock.ctx(1));
        let activation = node.clients[0].activate(config(&node.charge));
        node.owner().poll_budgeted(4).unwrap();
        assert_eq!(ready(&activation).err(), Some(Error::Unavailable));
        assert_eq!(node.charge.load(Ordering::Acquire), 0, "charge returned");
        assert_eq!(sim.live_resources(), 0);
    });
}

#[test]
/// `release_link` names the link by handshake id: the owner resolves it, so
/// the release goes out without the caller's snapshot. A wrong id is refused
/// and leaves the grant and link alone.
fn release_by_link_id_reaches_the_current_link() {
    profiles(|| {
        let mut w = world();
        let (ea, _) = link_up(&mut w);
        let LinkState::Ready { link_id, .. } = w.b.clients[0].link(TO_A) else {
            panic!("link not ready");
        };
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let wrong = w.b.clients[0].release_link(TO_A, link_id ^ 1, granted.tag);
        assert_eq!(wait(&mut w, &wrong), Err(Error::Unavailable));
        assert!(pending(&granted.arrival));
        let released = w.b.clients[0].release_link(TO_A, link_id, granted.tag);
        assert_eq!(wait(&mut w, &released), Ok(()));
        assert!(matches!(ready(&granted.arrival), Ok(Arrival::Released)));
        // Idempotent: a second release of the same tag completes too.
        let again = w.b.clients[0].release_link(TO_A, link_id, granted.tag);
        assert_eq!(wait(&mut w, &again), Ok(()));
        assert!(matches!(w.b.clients[0].link(TO_A), LinkState::Ready { .. }));
    });
}

#[test]
/// The sender releases instead of writing; a write for a tag never
/// granted times out; neither breaks the link.
fn release_and_wrong_tag_fall_back_without_break() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let released = w.b.clients[0].release(TO_A, eb, granted.tag);
        assert_eq!(wait(&mut w, &released), Ok(()));
        assert!(matches!(ready(&granted.arrival), Ok(Arrival::Released)));
        let wrong = w.b.clients[0].write(TO_A, eb, 999, bytes(vec![1]), w.now() + SECOND);
        pump(&mut w);
        assert!(pending(&wrong));
        w.clock.advance(SECOND);
        assert_eq!(wait(&mut w, &wrong), Err(Error::DeadlineExceeded));
        // A stale epoch is refused.
        let stale = w.b.clients[0].write(TO_A, Epoch(eb.0 + 100), 1, bytes(vec![1]), deadline);
        assert_eq!(wait(&mut w, &stale), Err(Error::Unavailable));
        assert!(matches!(w.a.clients[0].link(TO_B), LinkState::Ready { .. }));
        shutdown(w);
    });
}

#[test]
/// Fabric faults during a write break the link: the arrival fails with
/// `Unavailable`, no page is produced, buffers return, and the receiver
/// backs off.
fn fabric_faults_break_without_producing_pages() {
    profiles(|| {
        for fault in [
            Fault::DuplicateImmediate,
            Fault::Drop,
            Fault::ByteLength(11),
            Fault::Completion(10),
        ] {
            let mut w = world();
            let (ea, eb) = link_up(&mut w);
            let deadline = w.now() + 5 * SECOND;
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
            let (arrival, orphaned) = match granted.arrival.poll(&mut cx) {
                Poll::Ready(result) => (result, false),
                Poll::Pending => {
                    // Nothing reached a: its exposed grant waits out the
                    // grace past its deadline, then the link closes.
                    w.clock.advance(5 * SECOND);
                    pump(&mut w);
                    assert!(pending(&granted.arrival), "{fault:?}");
                    w.clock.advance(EXPOSED_GRACE);
                    pump(&mut w);
                    (ready(&granted.arrival), true)
                }
            };
            assert_eq!(arrival.err(), Some(Error::Unavailable), "{fault:?}");
            assert_eq!(w.a.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
            if orphaned {
                // A close, counted as an expired grant.
                assert_eq!(w.a.clients[0].link_breaks(BreakReason::GrantExpired), 1);
                // With only the write dropped, the peer is healthy and the
                // close is clean. With a failed write, the peer already broke
                // and the `Close` fails.
                if fault == Fault::Drop {
                    assert_eq!(w.a.clients[0].link_breaks(BreakReason::Error), 0);
                    assert_eq!(w.a.clients[0].link_breaks(BreakReason::Closed), 1);
                    assert_eq!(w.a.clients[0].link(TO_B), LinkState::Idle);
                }
                shutdown(w);
                continue;
            }
            assert!(
                matches!(w.a.clients[0].link(TO_B), LinkState::Backoff(_)),
                "{fault:?}"
            );
            let reopen = w.a.clients[0].open(TO_B);
            assert_eq!(
                wait(&mut w, &reopen).err(),
                Some(Error::Overloaded),
                "backing off"
            );
            shutdown(w);
        }
    });
}

#[test]
/// `break_link` aborts outstanding ops; backoff doubles and then lets
/// `open` through again.
fn abort_and_backoff() {
    profiles(|| {
        let mut w = world();
        let (ea, _) = link_up(&mut w);
        let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, w.now() + 5 * SECOND);
        let granted = wait(&mut w, &granted).unwrap();
        w.a.clients[1].break_link(TO_B, ea);
        pump(&mut w);
        assert_eq!(ready(&granted.arrival).err(), Some(Error::Unavailable));
        let LinkState::Backoff(first) = w.a.clients[0].link(TO_B) else {
            panic!("not backing off");
        };
        let delay = first - w.now();
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
        let delay = second - w.now();
        assert!(
            delay >= BACKOFF_MIN * 16 / 10 && delay <= BACKOFF_MIN * 24 / 10,
            "{delay:?}"
        );
        shutdown(w);
    });
}

#[test]
/// `decline_link` (a benign setup refusal) retries after `BACKOFF_MIN`
/// without escalating, even after earlier failures, and every break is
/// counted by reason. Link states are counted from the snapshot.
fn decline_does_not_escalate_and_breaks_are_counted() {
    profiles(|| {
        let mut w = world();
        let (ea, _) = link_up(&mut w);
        let counts = w.a.clients[0].link_counts();
        assert_eq!(counts.ready, 1);
        assert_eq!(counts.backoff, 0);
        // A failed setup escalates the backoff.
        w.a.clients[1].break_link(TO_B, ea);
        pump(&mut w);
        assert_eq!(w.a.clients[0].link_breaks(BreakReason::Abandoned), 1);
        assert_eq!(w.a.clients[0].link_counts().backoff, 1);
        let LinkState::Backoff(until) = w.a.clients[0].link(TO_B) else {
            panic!("not backing off");
        };
        w.clock.advance(until - w.now());
        pump(&mut w);
        // Escalate once more with a setup timeout.
        let opened = w.a.clients[0].open(TO_B);
        wait(&mut w, &opened).unwrap();
        w.clock.advance(SETUP_TIMEOUT);
        pump(&mut w);
        assert_eq!(w.a.clients[0].link_breaks(BreakReason::Timeout), 1);
        let LinkState::Backoff(until) = w.a.clients[0].link(TO_B) else {
            panic!("not backing off");
        };
        w.clock.advance(until - w.now());
        pump(&mut w);
        // A declined setup backs off by `BACKOFF_MIN` only.
        let opened = w.a.clients[0].open(TO_B);
        let opened = wait(&mut w, &opened).unwrap();
        assert_eq!(w.a.clients[0].link_counts().connecting, 1);
        w.a.clients[1].decline_link(TO_B, opened.epoch);
        pump(&mut w);
        assert_eq!(w.a.clients[0].link_breaks(BreakReason::Declined), 1);
        let LinkState::Backoff(until) = w.a.clients[0].link(TO_B) else {
            panic!("not backing off");
        };
        let delay = until - w.now();
        assert!(
            delay >= BACKOFF_MIN * 8 / 10 && delay <= BACKOFF_MIN * 12 / 10,
            "{delay:?}"
        );
        assert_eq!(w.a.clients[0].link_breaks(BreakReason::Error), 0);
        shutdown(w);
    });
}

#[test]
/// The backoff never exceeds `BACKOFF_MAX` (plus jitter).
fn backoff_is_capped() {
    profiles(|| {
        let mut w = world();
        let mut last = Duration::ZERO;
        for _ in 0..6 {
            let opened = w.a.clients[0].open(TO_B);
            wait(&mut w, &opened).unwrap();
            w.clock.advance(SETUP_TIMEOUT);
            pump(&mut w);
            let LinkState::Backoff(until) = w.a.clients[0].link(TO_B) else {
                panic!("not backing off");
            };
            last = until - w.now();
            assert!(last <= BACKOFF_MAX * 12 / 10, "{last:?}");
            w.clock.advance(last);
            pump(&mut w);
        }
        assert!(last >= BACKOFF_MAX * 8 / 10, "{last:?}");
        shutdown(w);
    });
}

#[test]
/// Setup rules: busy keys, unknown rails, wrong epochs, bad credits, and
/// a new accept replacing a ready link.
fn setup_rules() {
    profiles(|| {
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
        let granted = w.b.clients[0].grant(TO_A, eb, [1; 16], 10, w.now() + 5 * SECOND);
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
    });
}

#[test]
/// A replacement setup on a full rail retires the old link instead of
/// destroying it and then failing; a setup refused for capacity backs off.
fn replacement_on_full_rail_succeeds_and_refusals_back_off() {
    profiles(|| {
        let mut w = world();
        let (_, eb) = link_up(&mut w);
        // b's rail is full: its link to a plus a setup to another peer.
        let other = w.b.clients[0].open(LinkKey {
            peer: [4; 32],
            rail: 7,
        });
        wait(&mut w, &other).unwrap();
        let spare = w.a.clients[0].open(LinkKey {
            peer: [6; 32],
            rail: 7,
        });
        let offer = Setup {
            endpoint: wait(&mut w, &spare).unwrap().setup.endpoint,
            recv_credits: 8,
            grants_per_link: 2,
        };
        let replaced = w.b.clients[0].accept(TO_A, offer, ID + 1, false);
        let replaced = wait(&mut w, &replaced).unwrap();
        assert_ne!(replaced.epoch, eb);
        assert!(!matches!(
            w.b.clients[0].link(TO_A),
            LinkState::Idle | LinkState::Backoff(_)
        ));
        // A new key on the full rail is refused and backs off.
        let third = LinkKey {
            peer: [5; 32],
            rail: 7,
        };
        let refused = w.b.clients[0].accept(third, offer, ID + 2, false);
        assert_eq!(wait(&mut w, &refused).err(), Some(Error::Overloaded));
        assert!(matches!(w.b.clients[0].link(third), LinkState::Backoff(_)));
        let reopen = w.b.clients[0].open(third);
        assert_eq!(wait(&mut w, &reopen).err(), Some(Error::Overloaded));
        shutdown(w);
    });
}

#[test]
/// `retain` closes links whose keys were dropped (a ready link sends
/// `Close`, a setup breaks) without backoff on either side, and keeps the
/// rest.
fn retain_closes_dropped_links_without_backoff() {
    profiles(|| {
        let mut w = world();
        link_up(&mut w);
        let other = LinkKey {
            peer: [4; 32],
            rail: 7,
        };
        let opened = w.a.clients[0].open(other);
        wait(&mut w, &opened).unwrap();
        w.a.clients[1].retain([other].into());
        pump(&mut w);
        assert_eq!(w.a.clients[0].link(TO_B), LinkState::Idle);
        assert_eq!(w.b.clients[0].link(TO_A), LinkState::Idle);
        assert!(matches!(
            w.a.clients[0].link(other),
            LinkState::Connecting(_)
        ));
        w.a.clients[0].retain(Default::default());
        pump(&mut w);
        assert_eq!(w.a.clients[0].link(other), LinkState::Idle);
        // Nothing backs off: the dropped peer can link again at once.
        let again = w.a.clients[0].open(TO_B);
        wait(&mut w, &again).unwrap();
        shutdown(w);
    });
}

#[test]
/// Closing one node sends `Close`: the peer returns to `Idle`, not backoff.
/// An idle link closes itself the same way.
fn peer_close_and_idle_close() {
    profiles(|| {
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
    });
}

#[test]
/// A full command queue reports `Overloaded`; `poll_ready` waits for room.
fn full_queue_is_overloaded() {
    profiles(|| {
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
    });
}

#[test]
/// Shutdown waits for pages held by the caller; reopen is blocked until
/// then. A failed destroy keeps the node from ever draining.
fn shutdown_waits_for_pages_and_destroys() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let deadline = w.now() + 5 * SECOND;
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
    });
}

#[test]
/// A failed destroy is retried and blocks draining until it succeeds;
/// dropping the owner while it fails leaks the link and its charge.
fn failed_destroy_blocks_drain_or_leaks() {
    profiles(|| {
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
    });
}

#[test]
/// Dropping the owner fails outstanding ops, makes clients unavailable,
/// and leaves held pages readable.
fn owner_drop() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let deadline = w.now() + 5 * SECOND;
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
        assert_eq!(ready(&queued).err(), Some(Error::Canceled));
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
    });
}

#[test]
/// Dropping a worker's client keeps its ops and the node running.
fn worker_drop() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[1].grant(TO_B, ea, [1; 16], 10, deadline);
        drop(w.a.clients.pop());
        let granted = wait(&mut w, &granted).unwrap();
        let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![5; 10]), deadline);
        assert_eq!(wait(&mut w, &written), Ok(()));
        assert_eq!(page(ready(&granted.arrival)).bytes(), [5; 10]);
        shutdown(w);
    });
}

/// Step `role` until it finishes, at most `turns` times.
fn finish(role: &mut OwnerRole, turns: usize) -> bool {
    (0..turns).any(|_| role.step().unwrap() == Next::Done)
}

#[test]
/// The owner runs as its own role: it serves commands, and once stopped it
/// closes, drains, and finishes.
fn owner_role_serves_and_drains_on_stop() {
    profiles(|| {
        let mut w = world();
        let (mut role, stop) = OwnerRole::new(w.a.owner.take().unwrap()).unwrap();
        let opened = w.a.clients[0].open(TO_B);
        role.step().unwrap();
        assert!(ready(&opened).is_ok());
        assert!(!stop.requested());
        stop.stop();
        assert!(finish(&mut role, 64), "owner role did not finish");
        assert!(w.a.clients[0].drained());
        drop(role);
        w.b.clients[0].close();
        pump(&mut w);
        assert_eq!(w.sim.live_resources(), 0);
    });
}

#[test]
/// A stopped owner waits for a page buffer held elsewhere (by a crypto job,
/// say): it reports the slow drain but never finishes early, and the
/// buffer's return wakes it without any timer.
fn owner_role_drain_waits_for_held_buffer() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 4, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![2; 4]), deadline);
        assert_eq!(wait(&mut w, &written), Ok(()));
        let held = page(ready(&granted.arrival));
        let (mut role, stop) = OwnerRole::new(w.a.owner.take().unwrap()).unwrap();
        stop.stop();
        assert!(!finish(&mut role, 64), "drained with a buffer still held");
        // Past the report period: still waiting, never freeing early.
        w.clock.advance(DRAIN_REPORT + SECOND);
        assert!(!finish(&mut role, 4));
        assert!(!w.a.clients[0].drained());
        // The return wakes the parked role at once: no tick needed.
        drop(held);
        let parked = Instant::now();
        role.park(Next::Idle).unwrap();
        assert!(parked.elapsed() < Duration::from_secs(5));
        assert!(finish(&mut role, 64), "owner role did not finish");
        assert!(w.a.clients[0].drained());
        drop(role);
        w.b.clients[0].close();
        pump(&mut w);
        assert_eq!(w.sim.live_resources(), 0);
    });
}

#[test]
/// An idle owner with a ready link needs no periodic poll: its next wake is
/// the link's idle timer, far in the future. A peer write wakes it through
/// the completion channel, and a command through the eventfd.
fn owner_role_parks_on_wake_sources_without_ticks() {
    profiles(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let (mut role, stop) = OwnerRole::new(w.a.owner.take().unwrap()).unwrap();
        let mut next = role.step().unwrap();
        for _ in 0..8 {
            if next != Next::Ready {
                break;
            }
            next = role.step().unwrap();
        }
        match next {
            Next::Idle => {}
            Next::WaitUntil(at) => assert!(at >= w.now() + Duration::from_secs(60), "ticks"),
            other => panic!("owner not idle: {other:?}"),
        }
        // A command wakes the parked owner.
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[0].grant(TO_B, ea, [3; 16], 4, deadline);
        let parked = Instant::now();
        role.park(Next::Idle).unwrap();
        assert!(parked.elapsed() < Duration::from_secs(5));
        for _ in 0..8 {
            role.step().unwrap();
            w.b.owner().poll_budgeted(64).unwrap();
        }
        let granted = ready(&granted).unwrap();
        // A peer write wakes it through its completion channel.
        let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![4; 4]), deadline);
        for _ in 0..4 {
            w.b.owner().poll_budgeted(64).unwrap();
        }
        let parked = Instant::now();
        role.park(Next::Idle).unwrap();
        assert!(parked.elapsed() < Duration::from_secs(5));
        for _ in 0..8 {
            role.step().unwrap();
            w.b.owner().poll_budgeted(64).unwrap();
        }
        assert_eq!(ready(&written), Ok(()));
        assert_eq!(page(ready(&granted.arrival)).bytes(), [4; 4]);
        assert!(stop.wakes() >= 2);
        stop.stop();
        assert!(finish(&mut role, 64));
        drop(role);
        w.b.clients[0].close();
        pump(&mut w);
        assert_eq!(w.sim.live_resources(), 0);
    });
}

/// Device pool memory: two buffers plus a tail too small for a third.
fn device_memory() -> Arc<MemfdMemory> {
    MemfdMemory::new(2 * len() + 5, 0x7000_0000).unwrap()
}

fn device_page(arrival: rdma_verbs::Result<Arrival>) -> rdma_verbs::DevicePage {
    match arrival.unwrap() {
        Arrival::Device(page) => page,
        other => panic!("expected a device page, got {other:?}"),
    }
}

#[test]
/// A device grant lands the peer's write in dma-buf memory with no CPU copy:
/// the bytes are only visible through the memory owner, the page holds its
/// buffer until dropped, and host grants keep using the host pool.
fn device_grants_land_in_dmabuf_memory() {
    window_only(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let memory = device_memory();
        let attached = w.a.clients[0].attach_device(7, 0, memory.clone());
        assert_eq!(wait(&mut w, &attached), Ok(2));
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Free),
            Some(2)
        );
        assert_eq!(w.b.clients[0].device_buffers(7, 0, BufferState::Free), None);
        let deadline = w.now() + 5 * SECOND;
        let mut held = Vec::new();
        for round in 0..2u8 {
            let granted = w.a.clients[0].grant_device(TO_B, ea, 0, [round; 16], 100, deadline);
            let granted = wait(&mut w, &granted).unwrap();
            let data = vec![0xa0 | round; 100];
            let written =
                w.b.clients[1].write(TO_A, eb, granted.tag, bytes(data.clone()), deadline);
            assert_eq!(wait(&mut w, &written), Ok(()));
            let page = device_page(ready(&granted.arrival));
            assert_eq!((page.len(), page.is_empty()), (100, false));
            assert_eq!(page.offset(), u64::from(round) * len() as u64);
            assert!(Arc::ptr_eq(
                page.memory(),
                &(memory.clone() as rdma_verbs::DeviceMemory)
            ));
            assert_eq!(memory.read(page.offset() as usize, 100), data);
            held.push(page);
        }
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Owned),
            Some(2)
        );
        // Both device buffers are held: the next device grant is refused, and
        // nothing falls back to host memory.
        let refused = w.a.clients[0].grant_device(TO_B, ea, 0, [7; 16], 10, deadline);
        assert_eq!(wait(&mut w, &refused).err(), Some(Error::Overloaded));
        assert_eq!(w.a.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
        // Host grants still use the host pool.
        let host = w.a.clients[0].grant(TO_B, ea, [8; 16], 3, deadline);
        let host = wait(&mut w, &host).unwrap();
        let written = w.b.clients[0].write(TO_A, eb, host.tag, bytes(vec![1, 2, 3]), deadline);
        assert_eq!(wait(&mut w, &written), Ok(()));
        assert_eq!(page(ready(&host.arrival)).bytes(), [1, 2, 3]);
        // A page dropped on another thread frees its buffer for the next grant,
        // and a release returns a device buffer without producing a page.
        let page = held.pop().unwrap();
        std::thread::spawn(move || drop(page)).join().unwrap();
        pump(&mut w);
        let granted = w.a.clients[0].grant_device(TO_B, ea, 0, [9; 16], 10, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let released = w.b.clients[0].release(TO_A, eb, granted.tag);
        assert_eq!(wait(&mut w, &released), Ok(()));
        assert!(matches!(ready(&granted.arrival), Ok(Arrival::Released)));
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Free),
            Some(1)
        );
        // Shutdown waits for the held device page.
        w.a.clients[0].close();
        pump(&mut w);
        assert!(!w.a.clients[0].drained());
        drop(held);
        pump(&mut w);
        assert!(w.a.clients[0].drained());
        assert_eq!(
            Arc::strong_count(&memory),
            1,
            "the memory owner is released"
        );
        shutdown(w);
    });
}

#[test]
/// One rail holds several device pools (one per GPU, say). Each grant names
/// its pool: the write lands in that pool's memory only, one full pool does
/// not refuse grants on another, and an unknown pool id is unavailable.
fn device_pools_on_one_rail_are_independent() {
    window_only(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let first = MemfdMemory::new(len(), 0x7000_0000).unwrap();
        let second = MemfdMemory::new(len(), 0x7100_0000).unwrap();
        let attached = w.a.clients[0].attach_device(7, 0, first.clone());
        assert_eq!(wait(&mut w, &attached), Ok(1));
        let attached = w.a.clients[0].attach_device(7, 1, second.clone());
        assert_eq!(wait(&mut w, &attached), Ok(1));
        let deadline = w.now() + 5 * SECOND;
        let mut held = Vec::new();
        for (id, memory, fill) in [(0u32, &first, 0x11u8), (1, &second, 0x22)] {
            let granted = w.a.clients[0].grant_device(TO_B, ea, id, [fill; 16], 50, deadline);
            let granted = wait(&mut w, &granted).unwrap();
            let written =
                w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![fill; 50]), deadline);
            assert_eq!(wait(&mut w, &written), Ok(()));
            let page = device_page(ready(&granted.arrival));
            assert!(Arc::ptr_eq(
                page.memory(),
                &(memory.clone() as rdma_verbs::DeviceMemory)
            ));
            assert_eq!(memory.read(0, 50), vec![fill; 50]);
            held.push(page);
        }
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Owned),
            Some(1)
        );
        assert_eq!(
            w.a.clients[0].device_buffers(7, 1, BufferState::Owned),
            Some(1)
        );
        let full = w.a.clients[0].grant_device(TO_B, ea, 0, [3; 16], 10, deadline);
        assert_eq!(wait(&mut w, &full).err(), Some(Error::Overloaded));
        let unknown = w.a.clients[0].grant_device(TO_B, ea, 9, [3; 16], 10, deadline);
        assert_eq!(wait(&mut w, &unknown).err(), Some(Error::Unavailable));
        // Freeing pool 1's page lets pool 1 grant again; pool 0 stays full.
        drop(held.pop());
        pump(&mut w);
        let again = w.a.clients[0].grant_device(TO_B, ea, 1, [4; 16], 10, deadline);
        let again = wait(&mut w, &again).unwrap();
        let released = w.b.clients[0].release(TO_A, eb, again.tag);
        assert_eq!(wait(&mut w, &released), Ok(()));
        drop(held);
        shutdown(w);
        assert_eq!(Arc::strong_count(&first), 1);
        assert_eq!(Arc::strong_count(&second), 1);
    });
}

#[test]
/// Attach is once per activated rail; a device grant without a device pool
/// is unavailable; a failed registration leaves nothing behind and can be
/// retried.
fn device_attach_rules_and_registration_faults() {
    window_only(|| {
        let mut w = world();
        let (ea, _) = link_up(&mut w);
        let deadline = w.now() + 5 * SECOND;
        let missing = w.a.clients[0].grant_device(TO_B, ea, 0, [1; 16], 10, deadline);
        assert_eq!(wait(&mut w, &missing).err(), Some(Error::Unavailable));
        let memory = device_memory();
        let wrong_rail = w.a.clients[0].attach_device(3, 0, memory.clone());
        assert_eq!(wait(&mut w, &wrong_rail), Err(Error::InvalidRequest));
        let live = w.sim.live_resources();
        w.sim.fault(Operation::RegisterDmabuf, Fault::Reject);
        let failed = w.a.clients[0].attach_device(7, 0, memory.clone());
        assert_eq!(wait(&mut w, &failed), Err(Error::Unavailable));
        assert_eq!(w.sim.live_resources(), live, "nothing registered stays");
        assert_eq!(Arc::strong_count(&memory), 1);
        // Memory smaller than one buffer is refused after registration, and
        // the region is deregistered again.
        let small = w.a.clients[0].attach_device(7, 0, MemfdMemory::new(len() - 1, 0).unwrap());
        assert_eq!(wait(&mut w, &small), Err(Error::InvalidConfiguration));
        assert_eq!(w.sim.live_resources(), live);
        let attached = w.a.clients[0].attach_device(7, 0, memory.clone());
        assert_eq!(wait(&mut w, &attached), Ok(2));
        assert_eq!(w.sim.live_resources(), live + 1);
        let again = w.a.clients[0].attach_device(7, 0, device_memory());
        assert_eq!(wait(&mut w, &again), Err(Error::InvalidRequest));
        let attempts: Vec<i64> = w
            .sim
            .trace()
            .iter()
            .filter(|e| e.operation == Operation::RegisterDmabuf)
            .map(|e| e.result)
            .collect();
        assert_eq!(attempts, [-1, 0, 0]);
        shutdown(w);
        assert_eq!(Arc::strong_count(&memory), 1);
    });
}

#[test]
/// A link break while a device grant is exposed fails the arrival, and the
/// buffer is reused only after the QP is destroyed (the same fence as host
/// grants).
fn device_grant_break_fences_before_reuse() {
    window_only(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        let attached = w.a.clients[0].attach_device(7, 0, device_memory());
        assert_eq!(wait(&mut w, &attached), Ok(2));
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[0].grant_device(TO_B, ea, 0, [1; 16], 10, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Exposed),
            Some(1)
        );
        w.sim
            .fault_on(Operation::WriteImm, None, Fault::DuplicateImmediate);
        let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![1; 10]), deadline);
        pump(&mut w);
        let _ = ready(&written);
        assert_eq!(ready(&granted.arrival).err(), Some(Error::Unavailable));
        assert!(matches!(w.a.clients[0].link(TO_B), LinkState::Backoff(_)));
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Free),
            Some(2)
        );
        shutdown(w);
    });
}

#[test]
/// Failed deregistration leaks the device region with its memory owner and
/// charge, so the memory is never freed while the NIC may write it.
fn failed_deregister_leaks_device_memory_owner() {
    window_only(|| {
        let mut w = world();
        link_up(&mut w);
        let memory = device_memory();
        let attached = w.a.clients[0].attach_device(7, 0, memory.clone());
        assert_eq!(wait(&mut w, &attached), Ok(2));
        w.sim.reject(Operation::Deregister, None, true);
        drop(w.a.owner.take());
        assert!(Arc::strong_count(&memory) > 1, "memory owner leaked");
        assert!(w.a.charge.load(Ordering::Acquire) >= 1, "charge leaked");
    });
}

#[test]
/// Registration mode needs whole 2 MiB page buffers: anything else fails
/// activation, frees everything, and returns the charge.
fn registration_mode_requires_huge_page_buffers() {
    let _profile = default_profile(Profile::Registration);
    let sim = Simulation::new();
    let clock = SimClock::new(7);
    let mut node = node(&sim, 1, clock.ctx(1));
    let mut small = config(&node.charge);
    small.buffer_len = 4096;
    small.pool_bytes = BUFFERS * 4096;
    let activation = node.clients[0].activate(small);
    node.owner().poll_budgeted(4).unwrap();
    assert_eq!(ready(&activation).err(), Some(Error::InvalidConfiguration));
    assert_eq!(node.charge.load(Ordering::Acquire), 0);
    assert_eq!(sim.live_resources(), 0);
    let activation = node.clients[0].activate(config(&node.charge));
    node.owner().poll_budgeted(4).unwrap();
    assert_eq!(ready(&activation).unwrap()[0].tag, 7);
    assert_eq!(
        node.owner().poll_fds().len(),
        2,
        "completion and CM channels"
    );
    node.clients[0].close();
    node.owner().poll_budgeted(4).unwrap();
    assert!(node.clients[0].drained());
}

#[test]
/// Page pool registration runs off the owner loop: while it is pending the
/// owner keeps answering commands (an early `open` fails `Unavailable`, a
/// second activation is refused, breaks are accepted), and activation
/// completes once the registration finishes.
fn activation_registers_pools_without_blocking_the_owner() {
    profiles(|| {
        let sim = Simulation::new();
        let clock = SimClock::new(7);
        let mut node = node(&sim, 1, clock.ctx(1));
        sim.hold_registrations(true);
        let activation = node.clients[0].activate(config(&node.charge));
        for _ in 0..4 {
            node.owner().poll_budgeted(4).unwrap();
        }
        assert!(pending(&activation));
        assert_eq!(node.owner().next_wake(), None, "no timer for a held MR");
        let early = node.clients[1].open(TO_B);
        let again = node.clients[1].activate(config(&node.charge));
        node.clients[1].break_link(TO_B, Epoch(1));
        node.owner().poll_budgeted(4).unwrap();
        assert_eq!(ready(&early).err(), Some(Error::Unavailable));
        assert_eq!(ready(&again).err(), Some(Error::InvalidRequest));
        assert!(pending(&activation));
        assert_eq!(node.clients[0].buffers(7, BufferState::Free), None);
        sim.hold_registrations(false);
        node.owner().poll_budgeted(4).unwrap();
        assert_eq!(ready(&activation).unwrap()[0].tag, 7);
        assert_eq!(node.clients[0].buffers(7, BufferState::Free), Some(BUFFERS));
        let opened = node.clients[1].open(TO_B);
        node.owner().poll_budgeted(4).unwrap();
        assert!(ready(&opened).is_ok());
    });
}

#[test]
/// A close during a pending activation fails it `Unavailable` once the
/// registration finishes, frees the registered pool, and then drains with
/// the charge returned.
fn close_during_registration_waits_and_drains() {
    profiles(|| {
        let sim = Simulation::new();
        let clock = SimClock::new(7);
        let mut node = node(&sim, 1, clock.ctx(1));
        sim.hold_registrations(true);
        let activation = node.clients[0].activate(config(&node.charge));
        node.owner().poll_budgeted(4).unwrap();
        node.clients[0].close();
        for _ in 0..4 {
            node.owner().poll_budgeted(4).unwrap();
        }
        assert!(pending(&activation));
        assert!(!node.clients[0].drained(), "drain waits for the MR");
        sim.hold_registrations(false);
        node.owner().poll_budgeted(4).unwrap();
        assert_eq!(ready(&activation).err(), Some(Error::Unavailable));
        node.owner().poll_budgeted(4).unwrap();
        assert!(node.clients[0].drained());
        assert_eq!(node.charge.load(Ordering::Acquire), 0);
        assert_eq!(sim.live_resources(), 0);
    });
}

#[test]
/// A dma-buf device pool registers off the owner loop: grants, writes, and
/// releases on a live link keep completing while it is pending.
fn device_attach_does_not_block_live_links() {
    window_only(|| {
        let mut w = world();
        let (ea, eb) = link_up(&mut w);
        w.sim.hold_registrations(true);
        let attached = w.a.clients[0].attach_device(7, 0, device_memory());
        pump(&mut w);
        assert!(pending(&attached));
        let duplicate = w.a.clients[1].attach_device(7, 0, device_memory());
        assert_eq!(wait(&mut w, &duplicate).err(), Some(Error::InvalidRequest));
        let deadline = w.now() + 5 * SECOND;
        let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 3, deadline);
        let granted = wait(&mut w, &granted).unwrap();
        let written = w.b.clients[0].write(TO_A, eb, granted.tag, bytes(vec![4, 5, 6]), deadline);
        assert_eq!(wait(&mut w, &written), Ok(()));
        assert_eq!(page(ready(&granted.arrival)).bytes(), [4, 5, 6]);
        assert!(pending(&attached));
        w.sim.hold_registrations(false);
        assert_eq!(wait(&mut w, &attached), Ok(2));
        assert_eq!(
            w.a.clients[0].device_buffers(7, 0, BufferState::Free),
            Some(2)
        );
    });
}

#[test]
/// A removed device (an Azure VF hot-unplug) breaks every link on its rail
/// at once, fails their ops without waiting for deadlines, counts the loss,
/// and refuses new links on the rail until the round is closed; a fresh
/// round then activates normally.
fn device_removal_breaks_rail_and_recovers_after_reopen() {
    let _profile = default_profile(Profile::Registration);
    let mut w = world();
    let (ea, _) = link_up(&mut w);
    let deadline = w.now() + 60 * SECOND;
    let granted = w.a.clients[0].grant(TO_B, ea, [1; 16], 10, deadline);
    let granted = wait(&mut w, &granted).unwrap();
    assert_eq!(w.a.clients[0].device_losses(), 0);
    assert!(w.sim.remove_device("sim1") >= 2, "listener and link ids");
    pump(&mut w);
    assert_eq!(ready(&granted.arrival).err(), Some(Error::Unavailable));
    assert!(!matches!(
        w.a.clients[0].link(TO_B),
        LinkState::Ready { .. } | LinkState::Backoff(_)
    ));
    assert_eq!(w.a.clients[0].link_breaks(BreakReason::DeviceLost), 1);
    assert_eq!(w.a.clients[1].device_losses(), 1);
    assert_eq!(w.a.clients[1].lost_rails(), vec![7]);
    let refused = w.a.clients[0].open(TO_B);
    assert_eq!(wait(&mut w, &refused).err(), Some(Error::Unavailable));
    // The peer sees the connection end and breaks too.
    assert!(!matches!(
        w.b.clients[0].link(TO_A),
        LinkState::Ready { .. }
    ));
    // A second removal event changes nothing.
    w.sim.remove_device("sim1");
    pump(&mut w);
    assert_eq!(w.a.clients[0].device_losses(), 1);
    w.a.clients[0].close();
    pump(&mut w);
    assert!(w.a.clients[0].drained());
    assert!(w.a.clients[0].lost_rails().is_empty());
    w.a.clients[0].reopen().unwrap();
    let activation = w.a.clients[1].activate(config(&w.a.charge));
    assert!(wait(&mut w, &activation).is_ok());
    assert_eq!(w.a.clients[0].device_losses(), 1, "losses are cumulative");
    link_up(&mut w);
    shutdown(w);
}
