//! Links facade over two simulated nodes: activation, setup, tie-break, rate
//! limit, and page grants without HTTP.

use super::*;
use rdma_verbs::BufferState;
use rdma_verbs::Owner;
use rdma_verbs::simulation::Device;
use rdma_verbs::simulation::Simulation;
use std::future::Future;
use std::task::Context;

const RAIL: RailId = RailId(7);

pub(crate) struct Node {
    pub links: Rc<Links>,
    pub owner: Owner,
    pub admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}

pub(crate) struct World {
    pub _sim: Simulation,
    pub clock: racer_io::SimClock,
    pub env: racer_io::Env,
    pub a: Node,
    pub b: Node,
}

pub(crate) fn node_id(n: u8) -> NodeId {
    NodeId(format!("0000000{n}-1111-4111-8111-111111111111"))
}

fn mapping(n: u8) -> RailMapping {
    RailMapping {
        rail: RAIL,
        device: format!("sim{n}"),
        port: 1,
        gid: Some([n; 16]),
        numa_node: None,
    }
}

pub(crate) fn scope(env: &racer_io::Env) -> RequestScope {
    RequestScope::on(
        &env.ctx.clock,
        crate::model::RequestId([1; 16]),
        env.ctx.now() + Duration::from_secs(5),
    )
    .unwrap()
}

fn node(sim: &Simulation, n: u8, id: NodeId, environment: Option<&racer_io::WorkerCtx>) -> Node {
    let view = sim
        .with_devices(vec![Device::new(format!("sim{n}"), [n; 16])])
        .unwrap();
    let (mut clients, port) = rdma_verbs::node(1).unwrap();
    let owner = {
        let _scope = view.enter();
        let ctx = environment.map_or_else(racer_io::WorkerCtx::host, Clone::clone);
        Owner::new(port, ctx)
    };
    let ctx = environment.map_or_else(racer_io::WorkerCtx::host, Clone::clone);
    let limits = crate::test_support::cluster::config(true).limits;
    Node {
        links: Rc::new(Links::new(
            clients.remove(0),
            Arc::new(LinkNode::default()),
            id,
            None,
            ctx,
        )),
        owner,
        admission: Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits))),
    }
}

pub(crate) fn pump(a: &mut Owner, b: &mut Owner) {
    for _ in 0..8 {
        a.poll_budgeted(64).unwrap();
        b.poll_budgeted(64).unwrap();
    }
}

/// Poll `future`, pumping both owners between polls.
pub(crate) fn drive<T>(a: &mut Owner, b: &mut Owner, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..64 {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        pump(a, b);
    }
    panic!("future did not finish");
}

macro_rules! drive {
    ($w:expr, $future:expr) => {
        drive(&mut $w.a.owner, &mut $w.b.owner, $future)
    };
}

macro_rules! pump {
    ($w:expr) => {
        pump(&mut $w.a.owner, &mut $w.b.owner)
    };
}

pub(crate) fn page(
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    length: u32,
    byte: u8,
) -> CiphertextPage {
    page_with(admission, envelope(length), vec![byte; length as usize])
}

pub(crate) fn page_with(
    admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
    envelope: PageEnvelope,
    bytes: Vec<u8>,
) -> CiphertextPage {
    let cache = envelope.page.version.object.cache.clone();
    BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, bytes.len())
                .unwrap(),
            envelope,
            bytes,
        )
        .unwrap()
}

pub(crate) fn envelope(length: u32) -> PageEnvelope {
    use crate::model::*;
    let cache = racer_wire::control::CacheId("cccccccc-1111-4111-8111-111111111111".into());
    PageEnvelope {
        page_size: racer_wire::object::PageSize::DEFAULT,
        cipher: PageCipher::Xaes256Gcm,
        page: PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([3; 32]),
                },
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            },
            number: PageNumber(0),
        },
        key_id: racer_wire::control::KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: length - 16,
        ciphertext_length: length,
    }
}

/// Write `page` from `b` into a buffer granted by `a` over `link_up`'s link and
/// return the received registered page. `a` then has one buffer `Owned`.
pub(crate) fn receive_registered(
    env: &racer_io::Env,
    w: &mut World,
    page: &CiphertextPage,
) -> rdma_verbs::RegisteredPage {
    let (a, b) = (w.a.links.local().clone(), w.b.links.local().clone());
    let scope = scope(env);
    let pending = drive!(w, w.a.links.grant(&b, RAIL, TransferId([9; 16]), &scope)).unwrap();
    let header = STANDARD.decode(pending.header_value()).unwrap();
    let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([9; 16])).unwrap();
    drive!(w, w.b.links.send(&a, &grant, page, &scope)).unwrap();
    match drive!(w, poll_fn(|cx| pending.arrival.poll(cx))).unwrap() {
        Arrival::Page(page) => page,
        Arrival::Released | Arrival::Device(_) => panic!("expected a page"),
    }
}

pub(crate) fn world() -> World {
    world_for([node_id(1), node_id(2)], true)
}

/// Two activated nodes named `ids`, on the simulated clock if `simulated`,
/// else on the real clock (for tests over real sockets).
pub(crate) fn world_for([id_a, id_b]: [NodeId; 2], simulated: bool) -> World {
    let sim = Simulation::new();
    let clock = racer_io::SimClock::new(7);
    let environment = clock.ctx(1);
    let a = node(&sim, 1, id_a, simulated.then_some(&environment));
    let b = node(&sim, 2, id_b, simulated.then_some(&environment));
    let env = racer_io::Env::new(
        racer_io::Driver::Uring,
        if simulated {
            environment.clone()
        } else {
            racer_io::WorkerCtx::host()
        },
    );
    let mut world = World {
        _sim: sim,
        clock,
        env: env.clone(),
        a,
        b,
    };
    for n in [1, 2] {
        let scope = scope(&env);
        let w = &mut world;
        let (links, admission, owner, other) = if n == 1 {
            (
                &w.a.links,
                w.a.admission.clone(),
                &mut w.a.owner,
                &mut w.b.owner,
            )
        } else {
            (
                &w.b.links,
                w.b.admission.clone(),
                &mut w.b.owner,
                &mut w.a.owner,
            )
        };
        let rails = drive(
            owner,
            other,
            links.activate(vec![mapping(n)], &admission, 2 * BUFFER_LEN, &scope),
        )
        .unwrap();
        assert_eq!(rails.len(), 1);
        assert_eq!(rails[0].gid, Some([n; 16]));
        assert!(links.rail_ready(RAIL));
        assert!(links.node().settled());
        assert_eq!(
            admission.used(ResourceClass::Registered),
            2 * BUFFER_LEN,
            "the pool charge is node-wide"
        );
    }
    world
}

/// `a` initiates, `b` accepts, `a` connects. Returns the link id.
pub(crate) fn link_up(env: &racer_io::Env, w: &mut World) -> u64 {
    let (a, b) = (w.a.links.local().clone(), w.b.links.local().clone());
    let scope = scope(env);
    assert!(w.a.links.wants(&b, RAIL));
    let (epoch, ours) = drive!(w, w.a.links.open(&b, RAIL, &scope)).unwrap();
    assert!(!w.a.links.wants(&b, RAIL), "one setup per key");
    assert_eq!(w.b.links.admit_setup(&a, RAIL), SetupAdmission::Accept);
    let theirs = drive!(w, w.b.links.accept(&a, RAIL, &ours, &scope)).unwrap();
    let binding = link_setup::binding(&ours);
    drive!(
        w,
        w.a.links
            .connect(&b, RAIL, epoch, &ours, &theirs, &binding, &scope)
    )
    .unwrap();
    pump!(w);
    let (_, id_a) = w.a.links.ready(&b, RAIL).expect("initiator ready");
    let (_, id_b) = w.b.links.ready(&a, RAIL).expect("responder ready");
    assert_eq!(id_a, id_b);
    id_a
}

#[test]
fn constants_and_keys() {
    assert_eq!(BUFFER_LEN % (2 << 20), 0);
    const { assert!(BUFFER_LEN >= MAX_CIPHERTEXT && BUFFER_LEN - MAX_CIPHERTEXT < (2 << 20)) };
    assert_eq!(buffer_count(2 * BUFFER_LEN - 1), 1);
    assert_eq!(buffer_count(2 * BUFFER_LEN), 2);
    assert_eq!(grants_per_link(0), 1);
    assert_eq!(grants_per_link(16), 16);
    // Default sizing: 32 GiB over 8 rails funds ~229 buffers per rail.
    assert_eq!(grants_per_link(buffer_count(32 << 30) / 8), 227);
    assert_eq!(grants_per_link(usize::MAX), GRANTS_PER_LINK_CAP);
    const { assert!(GRANTS_PER_LINK_CAP <= crate::rdma::link_setup::MAX_GRANTS_PER_LINK) };

    let key = link_key(&node_id(1), RAIL);
    assert_eq!(key, link_key(&node_id(1), RAIL));
    assert_ne!(key, link_key(&node_id(2), RAIL));
    assert_ne!(key, link_key(&node_id(1), RailId(8)));
}

#[test]
fn grant_and_completion_values_round_trip_and_reject_malformed() {
    let transfer = TransferId([9; 16]);
    let bytes = grant_value(5, 6, 7, RAIL);
    let grant = AuthenticatedGrant::decode(&bytes, RAIL, transfer).unwrap();
    assert_eq!(
        grant,
        AuthenticatedGrant {
            link: 5,
            tag: 6,
            length: 7,
            rail: RAIL,
            transfer,
        }
    );
    assert_eq!(
        AuthenticatedGrant::decode(&bytes, RailId(8), transfer),
        Err(Error::Unauthorized)
    );
    assert_eq!(
        AuthenticatedGrant::decode(&bytes[1..], RAIL, transfer),
        Err(Error::InvalidRequest)
    );
    let mut wrong = bytes.clone();
    wrong[0] ^= 1;
    assert_eq!(
        AuthenticatedGrant::decode(&wrong, RAIL, transfer),
        Err(Error::InvalidRequest)
    );
    for length in [0, MAX_CIPHERTEXT as u32 + 1] {
        assert_eq!(
            AuthenticatedGrant::decode(&grant_value(5, 6, length, RAIL), RAIL, transfer),
            Err(Error::InvalidRange)
        );
    }
    let complete = completion_value(1, 2, 3, transfer);
    assert_eq!(complete.len(), COMPLETE_LEN);
    assert!(complete.starts_with(COMPLETE_PREFIX));
    assert_ne!(complete, completion_value(1, 2, 4, transfer));
    assert_ne!(complete, completion_value(1, 2, 3, TransferId([8; 16])));
}

#[test]
fn activation_without_a_buffer_settles_inactive() {
    let sim = Simulation::new();
    let clock = racer_io::SimClock::new(7);
    let environment = clock.ctx(1);
    let mut n = node(&sim, 1, node_id(1), Some(&environment));
    let env = racer_io::Env::new(racer_io::Driver::Uring, environment.clone());
    let scope = scope(&env);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let result =
        std::pin::pin!(
            n.links
                .activate(vec![mapping(1)], &n.admission, BUFFER_LEN - 1, &scope)
        )
        .poll(&mut cx);
    assert_eq!(result, Poll::Ready(Err(Error::Unavailable)));
    assert!(n.links.node().settled());
    assert!(n.links.active().is_empty());
    assert!(!n.links.rail_ready(RAIL));
    assert_eq!(n.admission.used(ResourceClass::Registered), 0);
    n.owner.poll_budgeted(8).unwrap();
}

#[test]
fn activation_with_one_buffer_funds_one_receive_buffer() {
    let sim = Simulation::new();
    let clock = racer_io::SimClock::new(7);
    let environment = clock.ctx(1);
    let mut a = node(&sim, 1, node_id(1), Some(&environment));
    let mut b = node(&sim, 2, node_id(2), Some(&environment));
    let env = racer_io::Env::new(racer_io::Driver::Uring, environment.clone());
    let scope = scope(&env);
    let rails = drive(
        &mut a.owner,
        &mut b.owner,
        a.links
            .activate(vec![mapping(1)], &a.admission, BUFFER_LEN, &scope),
    )
    .unwrap();
    assert_eq!(rails.len(), 1);
    assert!(a.links.node().settled());
    assert!(a.links.rail_ready(RAIL));
    assert_eq!(a.admission.used(ResourceClass::Registered), BUFFER_LEN);
    assert_eq!(a.links.buffers(RAIL, BufferState::Free), Some(1));
}

#[test]
fn local_rails_are_ready_rails_on_the_worker_numa_node() {
    let mut w = world();
    let env = w.env.clone();
    let (a, b) = (node_id(1), node_id(2));
    let set_rail_numa = |w: &World, numa| {
        LinkNode::lock(&w.a.links.node().active)
            .iter_mut()
            .for_each(|m| m.numa_node = numa);
    };
    set_rail_numa(&w, Some(1));
    Rc::get_mut(&mut w.a.links).unwrap().numa = Some(1);
    // A local rail without a ready link to the peer is not offered.
    assert!(w.a.links.local_rails(&b).is_empty());
    link_up(&env, &mut w);
    assert_eq!(w.a.links.local_rails(&b), vec![RAIL]);
    assert!(w.a.links.local_rails(&a).is_empty(), "no link to self");
    // The rail's NIC on another node, or of unknown locality.
    for numa in [Some(0), None] {
        set_rail_numa(&w, numa);
        assert!(w.a.links.local_rails(&b).is_empty());
    }
    // A worker of unknown locality has no local rails.
    set_rail_numa(&w, Some(1));
    assert!(w.b.links.local_rails(&a).is_empty());
    // A reader on another node (a host read's requesting worker) selects
    // the rails on its node, whatever this worker's node is.
    assert_eq!(w.a.links.rails_on(&b, Some(1)), vec![RAIL]);
    assert!(w.a.links.rails_on(&b, Some(0)).is_empty());
    assert!(w.a.links.rails_on(&b, None).is_empty());
    Rc::get_mut(&mut w.a.links).unwrap().numa = Some(0);
    assert!(w.a.links.local_rails(&b).is_empty());
    assert_eq!(w.a.links.numa(), Some(0));
    assert_eq!(w.a.links.rails_on(&b, Some(1)), vec![RAIL]);
}

#[test]
fn setup_page_both_directions_and_release() {
    let mut w = world();
    let env = w.env.clone();
    let link = link_up(&env, &mut w);
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope(&env);
    for (forward, from, to) in [(true, &b, &a), (false, &a, &b)] {
        let (receiver, sender) = if forward {
            (&w.a.links, &w.b.links)
        } else {
            (&w.b.links, &w.a.links)
        };
        let pending = drive!(w, receiver.grant(from, RAIL, TransferId([3; 16]), &scope)).unwrap();
        assert_eq!(pending.link(), link);
        let header = STANDARD.decode(pending.header_value()).unwrap();
        let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([3; 16])).unwrap();
        let admission = w.a.admission.clone();
        let page = page(&admission, 35, 0x5a);
        let sent = drive!(w, sender.send(to, &grant, &page, &scope)).unwrap();
        assert_eq!(
            STANDARD.decode(sent.header_value()).unwrap(),
            completion_value(link, grant.tag, 35, TransferId([3; 16]))
        );
        let arrival = drive!(w, poll_fn(|cx| pending.arrival.poll(cx))).unwrap();
        match arrival {
            Arrival::Page(page) => assert_eq!(page.bytes(), &[0x5a; 35]),
            Arrival::Released | Arrival::Device(_) => panic!("expected a page"),
        }
        // Release path: the receiver sees `Released` and the buffer is free.
        let pending = drive!(w, receiver.grant(from, RAIL, TransferId([4; 16]), &scope)).unwrap();
        let header = STANDARD.decode(pending.header_value()).unwrap();
        let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([4; 16])).unwrap();
        sender.release(to, &grant);
        assert!(matches!(
            drive!(w, poll_fn(|cx| pending.arrival.poll(cx))),
            Ok(Arrival::Released)
        ));
        pump!(w);
        assert_eq!(receiver.buffers(RAIL, BufferState::Free), Some(2));
    }
    // A grant for another link id is never written.
    let pending = drive!(w, w.a.links.grant(&b, RAIL, TransferId([5; 16]), &scope)).unwrap();
    let header = STANDARD.decode(pending.header_value()).unwrap();
    let mut grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([5; 16])).unwrap();
    grant.link ^= 1;
    let page = page(&w.b.admission.clone(), 35, 1);
    assert_eq!(
        drive!(w, w.b.links.send(&a, &grant, &page, &scope)).err(),
        Some(Error::Unavailable)
    );
    grant.link ^= 1;
    grant.length = 34;
    assert_eq!(
        drive!(w, w.b.links.send(&a, &grant, &page, &scope)).err(),
        Some(Error::InvalidRange)
    );
    w.b.links.release(&a, &grant);
    assert!(matches!(
        drive!(w, poll_fn(|cx| pending.arrival.poll(cx))),
        Ok(Arrival::Released)
    ));
}

#[test]
fn tie_break_rate_limit_and_bad_response() {
    let mut w = world();
    let env = w.env.clone();
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope(&env);
    // Both initiate at once. `a` has the lower id: it rejects `b`'s offer, and
    // `b` accepts `a`'s, replacing its own attempt.
    let (epoch_a, ours_a) = drive!(w, w.a.links.open(&b, RAIL, &scope)).unwrap();
    let (epoch_b, _ours_b) = drive!(w, w.b.links.open(&a, RAIL, &scope)).unwrap();
    assert_eq!(w.a.links.admit_setup(&b, RAIL), SetupAdmission::Conflict);
    assert_eq!(w.b.links.admit_setup(&a, RAIL), SetupAdmission::Accept);
    // The rate limit admits one setup per peer and rail per second.
    assert_eq!(w.b.links.admit_setup(&a, RAIL), SetupAdmission::Busy);
    let theirs = drive!(w, w.b.links.accept(&a, RAIL, &ours_a, &scope)).unwrap();
    assert!(
        w.b.links.wants(&a, RAIL) || w.b.links.state(&a, RAIL) != LinkState::Idle,
        "accept clears our initiation"
    );
    drive!(
        w,
        w.a.links.connect(
            &b,
            RAIL,
            epoch_a,
            &ours_a,
            &theirs,
            &link_setup::binding(&ours_a),
            &scope,
        )
    )
    .unwrap();
    pump!(w);
    assert!(w.a.links.ready(&b, RAIL).is_some());
    assert!(w.b.links.ready(&a, RAIL).is_some());
    // `b`'s stale epoch cannot break the accepted link.
    w.b.links.abandon(&a, RAIL, epoch_b);
    pump!(w);
    assert!(w.b.links.ready(&a, RAIL).is_some());
    w.clock.advance(SETUP_INTERVAL);
    assert_eq!(w.b.links.admit_setup(&a, RAIL), SetupAdmission::Accept);
    // A response with a wrong binding fails and backs the link off.
    let mut w = world();
    let (epoch, ours) = drive!(w, w.a.links.open(&b, RAIL, &scope)).unwrap();
    let theirs = drive!(w, w.b.links.accept(&a, RAIL, &ours, &scope)).unwrap();
    assert_eq!(
        drive!(
            w,
            w.a.links.connect(
                &b,
                RAIL,
                epoch,
                &ours,
                &theirs,
                &link_setup::binding(&theirs),
                &scope
            )
        ),
        Err(Error::Unauthorized)
    );
    pump!(w);
    assert!(matches!(
        w.a.links.state(&b, RAIL),
        LinkState::Backoff(_) | LinkState::Breaking
    ));
    assert!(!w.a.links.wants(&b, RAIL));
    // Replay of our own setup is refused.
    assert_eq!(
        drive!(w, w.a.links.open(&b, RAIL, &scope)).err(),
        Some(Error::Overloaded)
    );
}

#[test]
fn canceled_open_clears_its_reservation() {
    let mut w = world();
    let env = w.env.clone();
    let b = node_id(2);
    let scope = scope(&env);
    {
        let mut open = std::pin::pin!(w.a.links.open(&b, RAIL, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(open.as_mut().poll(&mut cx).is_pending());
        assert!(!w.a.links.wants(&b, RAIL), "reserved while waiting");
    }
    pump!(w);
    assert_eq!(LinkNode::lock(&w.a.links.node.setup).len(), 0);
    // The owner broke the link it opened for the dropped request; after the
    // backoff, a later attempt is allowed and succeeds.
    assert!(matches!(
        w.a.links.state(&b, RAIL),
        LinkState::Backoff(_) | LinkState::Breaking
    ));
    for _ in 0..8 {
        w.clock.advance(SETUP_DEADLINE);
        pump!(w);
    }
    assert!(w.a.links.wants(&b, RAIL));
    drive!(w, w.a.links.open(&b, RAIL, &self::scope(&env))).unwrap();
}

#[test]
fn open_dropped_after_the_owner_answered_breaks_its_epoch() {
    let mut w = world();
    let env = w.env.clone();
    let b = node_id(2);
    let scope = scope(&env);
    {
        let mut open = std::pin::pin!(w.a.links.open(&b, RAIL, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(open.as_mut().poll(&mut cx).is_pending());
        // The owner answers, but the caller never polls again.
        pump!(w);
        assert!(matches!(
            w.a.links.state(&b, RAIL),
            LinkState::Connecting(_)
        ));
    }
    pump!(w);
    assert_eq!(LinkNode::lock(&w.a.links.node.setup).len(), 0);
    assert!(matches!(
        w.a.links.state(&b, RAIL),
        LinkState::Backoff(_) | LinkState::Breaking
    ));
    for _ in 0..8 {
        w.clock.advance(SETUP_DEADLINE);
        pump!(w);
    }
    assert!(w.a.links.wants(&b, RAIL));
    drive!(w, w.a.links.open(&b, RAIL, &self::scope(&env))).unwrap();
}

#[test]
fn dropped_setup_attempt_abandons_its_epoch() {
    let mut w = world();
    let env = w.env.clone();
    let b = node_id(2);
    let scope = scope(&env);
    let (epoch, _) = drive!(w, w.a.links.open(&b, RAIL, &scope)).unwrap();
    drop(SetupAttempt::new(&w.a.links, &b, RAIL, epoch));
    pump!(w);
    assert_eq!(LinkNode::lock(&w.a.links.node.setup).len(), 0);
    assert!(matches!(
        w.a.links.state(&b, RAIL),
        LinkState::Backoff(_) | LinkState::Breaking
    ));
    for _ in 0..8 {
        w.clock.advance(SETUP_DEADLINE);
        pump!(w);
    }
    assert!(w.a.links.wants(&b, RAIL));
    drive!(w, w.a.links.open(&b, RAIL, &self::scope(&env))).unwrap();
}

#[test]
fn stale_setup_guards_leave_a_newer_attempt() {
    let mut w = world();
    let env = w.env.clone();
    let b = node_id(2);
    let scope = scope(&env);
    let (epoch, _) = drive!(w, w.a.links.open(&b, RAIL, &scope)).unwrap();
    let key = link_key(&b, RAIL);
    // A reservation token that is not the current entry.
    drop(Reservation {
        node: &w.a.links.node,
        key,
        token: u64::MAX,
        pending: None,
    });
    drop(SetupAttempt::new(&w.a.links, &b, RAIL, Epoch(epoch.0 + 1)));
    pump!(w);
    assert_eq!(
        LinkNode::lock(&w.a.links.node.setup).state(&key),
        Some(crate::rdma::setup::SetupState::Exchanging(epoch))
    );
    assert!(!w.a.links.wants(&b, RAIL));
    // The live attempt still finishes normally.
    SetupAttempt::new(&w.a.links, &b, RAIL, epoch).finish();
    assert!(!LinkNode::lock(&w.a.links.node.setup).idle(&key));
}

#[test]
fn drain_stops_setups_and_closes_the_node() {
    let mut w = world();
    let b = node_id(2);
    w.a.links.spawn(Box::pin(std::future::pending()));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert_eq!(w.a.links.poll(&mut cx), 1);
    drive!(w, w.a.links.drain()).unwrap();
    assert_eq!(w.a.links.poll(&mut cx), 0);
    assert!(!w.a.links.rail_ready(RAIL));
    assert!(!w.a.links.wants(&b, RAIL));
    w.a.links.spawn(Box::pin(std::future::pending()));
    assert_eq!(w.a.links.poll(&mut cx), 0, "no setups after drain");
    pump!(w);
    assert!(w.a.owner.drained());
}

mod zero_copy;

/// Sender failures after the owner may have consumed the grant's tag.
mod send_failures {
    use super::*;

    fn granted(
        env: &racer_io::Env,
        w: &mut World,
        transfer: u8,
    ) -> (PendingReceive, AuthenticatedGrant) {
        let scope = scope(env);
        let pending = drive!(
            w,
            w.a.links
                .grant(&node_id(2), RAIL, TransferId([transfer; 16]), &scope)
        )
        .unwrap();
        let header = STANDARD.decode(pending.header_value()).unwrap();
        let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([transfer; 16])).unwrap();
        (pending, grant)
    }

    fn arrived(w: &mut World, pending: &PendingReceive) -> Vec<u8> {
        match drive!(w, poll_fn(|cx| pending.arrival.poll(cx))).unwrap() {
            Arrival::Page(page) => page.bytes().to_vec(),
            Arrival::Released | Arrival::Device(_) => panic!("expected a page"),
        }
    }

    /// A write canceled after the owner consumed its tag keeps the link:
    /// the follow-up release completes at once for a consumed tag.
    #[test]
    fn canceled_after_the_write_lands_keeps_the_link() {
        let mut w = world();
        let env = w.env.clone();
        link_up(&env, &mut w);
        let (pending, grant) = granted(&env, &mut w, 3);
        let page = page(&w.b.admission.clone(), 35, 0x5a);
        let links = w.b.links.clone();
        let scope = scope(&env);
        let to = node_id(1);
        let mut send = std::pin::pin!(links.send(&to, &grant, &page, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(send.as_mut().poll(&mut cx).is_pending());
        // The owner completes the write before the worker sees the result.
        assert_eq!(arrived(&mut w, &pending), vec![0x5a; 35]);
        scope.cancel().unwrap();
        assert!(matches!(
            send.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Canceled))
        ));
        // The caller still releases, as `send_native` does; the tag is gone.
        links.release(&to, &grant);
        pump!(w);
        assert!(
            links.ready(&to, RAIL).is_some(),
            "an ambiguous write keeps the epoch"
        );
        assert!(w.a.links.ready(&node_id(2), RAIL).is_some());
        // The link still carries pages.
        let (pending, grant) = granted(&env, &mut w, 4);
        let second = super::page(&w.b.admission.clone(), 17, 0x6b);
        drive!(w, links.send(&to, &grant, &second, &super::scope(&env))).unwrap();
        assert_eq!(arrived(&mut w, &pending), vec![0x6b; 17]);
    }

    #[test]
    fn unadmitted_release_breaks_the_epoch() {
        let mut w = world();
        let env = w.env.clone();
        link_up(&env, &mut w);
        let (pending, grant) = granted(&env, &mut w, 3);
        let to = node_id(1);
        let (epoch, _) = w.b.links.ready(&to, RAIL).unwrap();
        let key = link_key(&to, RAIL);
        let mut cx = Context::from_waker(std::task::Waker::noop());
        let mut held = Vec::new();
        loop {
            let op =
                w.b.links
                    .client
                    .release(key, epoch, 1_000 + held.len() as u32);
            match op.poll(&mut cx) {
                Poll::Pending => held.push(op),
                Poll::Ready(error) => {
                    assert_eq!(error, Err(rdma_verbs::Error::Overloaded));
                    break;
                }
            }
        }
        w.b.links.release(&to, &grant);
        drop(held);
        pump!(w);
        assert!(
            w.b.links.ready(&to, RAIL).is_none(),
            "release fell back to a break"
        );
        drop(pending);
    }
}

/// Sender side: the page is written in place through implicit on-demand
/// paging, no pool buffer is used, and the metrics count it.
pub(crate) mod send_in_place {
    use super::*;
    use crate::telemetry::{Event, Gauge, Metrics};

    /// `w` with `metrics` attached to both nodes' links.
    pub(crate) fn metered(w: World) -> (World, Metrics, Metrics) {
        let (ma, mb) = (Metrics::default(), Metrics::default());
        let World {
            _sim,
            clock,
            env,
            a,
            b,
        } = w;
        let a = Node {
            links: Rc::new(
                Rc::into_inner(a.links)
                    .expect("unshared links")
                    .with_metrics(ma.clone()),
            ),
            ..a
        };
        let b = Node {
            links: Rc::new(
                Rc::into_inner(b.links)
                    .expect("unshared links")
                    .with_metrics(mb.clone()),
            ),
            ..b
        };
        (
            World {
                _sim,
                clock,
                env,
                a,
                b,
            },
            ma,
            mb,
        )
    }

    /// Grant on `a`, send `page` from `b`, and return what `a` received.
    fn transfer(env: &racer_io::Env, w: &mut World, page: &CiphertextPage) -> Vec<u8> {
        receive_registered(env, w, page).bytes().to_vec()
    }

    #[test]
    fn breaker_counts_soft_failures_and_trips_on_a_streak_or_a_hard_failure() {
        use crate::rdma::breaker::{SOFT_TRIP_THRESHOLD, TripReason};
        let w = world();
        let (w, ma, _) = metered(w);
        let links = &w.a.links;
        let peer = NodeId("soft-peer".into());
        for _ in 0..4 {
            links.breaker_trip(&peer, RAIL, TripReason::Load);
        }
        for _ in 1..SOFT_TRIP_THRESHOLD {
            links.breaker_trip(&peer, RAIL, TripReason::Timeout);
        }
        links.breaker_success(&peer, RAIL);
        links.breaker_trip(&peer, RAIL, TripReason::SlowFallback);
        assert!(!links.breaker_open(&peer, RAIL));
        assert_eq!(ma.count(Event::RdmaBreakerSoftLoad), 4);
        assert_eq!(
            ma.count(Event::RdmaBreakerSoftTimeout),
            u64::from(SOFT_TRIP_THRESHOLD - 1)
        );
        assert_eq!(ma.count(Event::RdmaBreakerSoftSlowFallback), 1);
        assert_eq!(ma.count(Event::RdmaBreakerTripTimeout), 0);
        // Completing the streak trips it, counted as a trip, not a soft failure.
        for _ in 1..SOFT_TRIP_THRESHOLD {
            links.breaker_trip(&peer, RAIL, TripReason::Timeout);
        }
        assert!(links.breaker_open(&peer, RAIL));
        assert_eq!(ma.count(Event::RdmaBreakerTripTimeout), 1);
        assert_eq!(
            ma.count(Event::RdmaBreakerSoftTimeout),
            u64::from(2 * SOFT_TRIP_THRESHOLD - 3)
        );
        // A hard failure on another peer trips at once.
        let other = NodeId("hard-peer".into());
        links.breaker_trip(&other, RAIL, TripReason::Send);
        assert!(links.breaker_open(&other, RAIL));
        assert_eq!(ma.count(Event::RdmaBreakerTripSend), 1);
    }

    #[test]
    fn send_writes_the_page_in_place_and_counts_it() {
        let mut w = world();
        let env = w.env.clone();
        link_up(&env, &mut w);
        let (mut w, _, mb) = metered(w);
        let page = page(&w.b.admission.clone(), 35, 0x5a);
        let received = transfer(&env, &mut w, &page);
        assert_eq!(received, page.bytes());
        assert_eq!(mb.count(Event::RdmaPagesSent), 1);
        assert_eq!(mb.count(Event::RdmaSentBytes), 35);
        // The worker mapped the page into the NIC before the owner posted it.
        assert!(rdma_verbs::simulation::prefetched(
            page.bytes().as_ptr() as u64,
            35
        ));
        assert_eq!(mb.count(Event::RdmaPrefetchFailed), 0);
        pump!(w);
        assert_eq!(w.b.links.buffers(RAIL, BufferState::Free), Some(2));
        assert_eq!(w.a.links.buffers(RAIL, BufferState::Free), Some(2));
    }

    #[test]
    fn page_dropped_by_the_caller_stays_alive_until_the_completion() {
        let mut w = world();
        let env = w.env.clone();
        link_up(&env, &mut w);
        let scope = scope(&env);
        let pending = drive!(
            w,
            w.a.links
                .grant(&node_id(2), RAIL, TransferId([3; 16]), &scope)
        )
        .unwrap();
        let header = STANDARD.decode(pending.header_value()).unwrap();
        let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([3; 16])).unwrap();
        let page = page(&w.b.admission.clone(), 35, 0xc3);
        let used = w.b.admission.used(ResourceClass::Ciphertext);
        let links = w.b.links.clone();
        let to = node_id(1);
        let mut send = Box::pin(links.send(&to, &grant, &page, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(send.as_mut().poll(&mut cx).is_pending());
        // The caller lets go; the owner's queued write still holds the page.
        drop(send);
        drop(page);
        assert_eq!(
            w.b.admission.used(ResourceClass::Ciphertext),
            used,
            "the queued write pins the page and its charge"
        );
        let arrival = drive!(w, poll_fn(|cx| pending.arrival.poll(cx))).unwrap();
        match arrival {
            Arrival::Page(page) => assert_eq!(page.bytes(), &[0xc3; 35][..]),
            Arrival::Released | Arrival::Device(_) => panic!("expected a page"),
        }
        pump!(w);
        assert!(
            w.b.admission.used(ResourceClass::Ciphertext) < used,
            "the completion released the page"
        );
    }

    #[test]
    /// Without implicit on-demand paging there is no RDMA send path: the
    /// owner refuses activation, the registered charge returns, no rail is
    /// ready, and pages keep using HTTP.
    fn activation_without_odp_fails_and_releases_the_charge() {
        let sim = Simulation::new();
        sim.implicit_odp(false);
        let clock = racer_io::SimClock::new(7);
        let environment = clock.ctx(1);
        let mut a = node(&sim, 1, node_id(1), Some(&environment));
        let mut b = node(&sim, 2, node_id(2), Some(&environment));
        let env = racer_io::Env::new(racer_io::Driver::Uring, environment.clone());
        let scope = scope(&env);
        let admission = a.admission.clone();
        assert_eq!(
            drive(
                &mut a.owner,
                &mut b.owner,
                a.links
                    .activate(vec![mapping(1)], &admission, 2 * BUFFER_LEN, &scope),
            )
            .err(),
            Some(Error::Unavailable)
        );
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert!(!a.links.rail_ready(RAIL));
        assert!(a.links.active().is_empty());
        assert_eq!(a.links.buffers(RAIL, BufferState::Free), None);
        assert!(
            a.links.ready(&node_id(2), RAIL).is_none(),
            "HTTP carries pages"
        );
    }

    #[test]
    fn http_fallback_is_counted() {
        let w = world();
        let (w, ma, _) = metered(w);
        w.a.links.record_http_fallback();
        assert_eq!(ma.count(Event::RdmaHttpFallback), 1);
    }

    #[test]
    /// Link breaks are published by reason, once each, and link states as
    /// gauges; both show up on `/metrics`.
    fn link_breaks_and_states_are_published() {
        let w = world();
        let (mut w, ma, _) = metered(w);
        let env = w.env.clone();
        let b = node_id(2);
        let epoch = match drive!(w, w.a.links.open(&b, RAIL, &scope(&env))) {
            Ok((epoch, _)) => epoch,
            Err(error) => panic!("open: {error:?}"),
        };
        pump!(w);
        w.a.links.sync_link_metrics();
        assert_eq!(ma.gauge(Gauge::RdmaLinksConnecting), 1);
        assert_eq!(ma.gauge(Gauge::RdmaLinksReady), 0);
        w.a.links.abandon(&b, RAIL, epoch);
        pump!(w);
        w.a.links.sync_link_metrics();
        w.a.links.sync_link_metrics();
        assert_eq!(ma.count(Event::RdmaLinkBreakAbandoned), 1);
        assert_eq!(ma.count(Event::RdmaLinkBreakError), 0);
        assert_eq!(ma.gauge(Gauge::RdmaLinksConnecting), 0);
        assert_eq!(
            ma.gauge(Gauge::RdmaLinksBackoff) + ma.gauge(Gauge::RdmaLinksBreaking),
            1
        );
        w.a.links.record_setup(SetupOutcome::Declined);
        let mut out = String::new();
        ma.write_prometheus(&mut out).unwrap();
        for line in [
            "racer_rdma_link_breaks_total{reason=\"abandoned\"} 1",
            "racer_rdma_link_breaks_total{reason=\"grant_expired\"} 0",
            "racer_rdma_link_setups_total{outcome=\"declined\"} 1",
            "racer_rdma_links_ready 0",
            "racer_rdma_links_backoff ",
        ] {
            assert!(out.contains(line), "missing {line}");
        }
        assert_eq!(
            out.matches("# TYPE racer_rdma_link_breaks_total").count(),
            1
        );
    }

    #[test]
    /// A declined setup retries after the minimum backoff, not an escalated one.
    fn declined_setup_retries_after_the_minimum_backoff() {
        let mut w = world();
        let env = w.env.clone();
        let b = node_id(2);
        for _ in 0..3 {
            let epoch = match drive!(w, w.a.links.open(&b, RAIL, &scope(&env))) {
                Ok((epoch, _)) => epoch,
                Err(error) => panic!("open: {error:?}"),
            };
            SetupAttempt::new(&w.a.links, &b, RAIL, epoch).decline();
            pump!(w);
            assert_eq!(LinkNode::lock(&w.a.links.node.setup).len(), 0);
            w.clock.advance(rdma_verbs::BACKOFF_MIN * 12 / 10);
            pump!(w);
            assert!(w.a.links.wants(&b, RAIL), "retries without escalating");
        }
        assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Declined), 3);
        assert_eq!(w.a.links.link_breaks(rdma_verbs::BreakReason::Abandoned), 0);
    }

    #[test]
    fn overflow_is_counted_apart_from_http_fallback() {
        let w = world();
        let (w, ma, _) = metered(w);
        w.a.links.record_fallback_overflow();
        assert_eq!(ma.count(Event::RdmaFallbackOverflow), 1);
        assert_eq!(ma.count(Event::RdmaHttpFallback), 0);
    }

    #[test]
    fn grant_wait_is_bounded_by_its_cap_and_half_the_deadline() {
        let now = Instant::now();
        let long = now + Duration::from_secs(10);
        assert_eq!(grant_wait_until(now, long), now + GRANT_WAIT);
        let short = now + Duration::from_millis(40);
        assert_eq!(
            grant_wait_until(now, short),
            now + Duration::from_millis(20)
        );
        assert_eq!(grant_wait_until(now, now), now);
        assert_eq!(grant_wait_until(now, now - Duration::from_secs(1)), now);
    }

    /// Drive [`retry_overloaded`] with a fake clock that each sleep advances.
    fn run_retry(
        results: Vec<Result<u8>>,
        until_ms: u64,
        sleep_fails: bool,
    ) -> (Result<u8>, u32, usize) {
        let start = Instant::now();
        let clock = Cell::new(start);
        let results = RefCell::new(std::collections::VecDeque::from(results));
        let attempts = Cell::new(0usize);
        let (result, waits) = futures::executor::block_on(retry_overloaded(
            || {
                attempts.set(attempts.get() + 1);
                let next = results
                    .borrow_mut()
                    .pop_front()
                    .unwrap_or(Err(Error::Overloaded));
                async move { next }
            },
            |wake| {
                clock.set(wake);
                async move {
                    if sleep_fails {
                        Err(Error::Canceled)
                    } else {
                        Ok(())
                    }
                }
            },
            || clock.get(),
            start + Duration::from_millis(until_ms),
        ));
        (result, waits, attempts.get())
    }

    #[test]
    fn grant_retry_returns_a_slot_that_frees_within_the_wait() {
        let (result, waits, attempts) = run_retry(
            vec![Err(Error::Overloaded), Err(Error::Overloaded), Ok(7)],
            20,
            false,
        );
        assert_eq!(result, Ok(7));
        assert_eq!((waits, attempts), (2, 3));
    }

    #[test]
    fn grant_retry_succeeds_at_once_without_waiting() {
        assert_eq!(run_retry(vec![Ok(1)], 20, false), (Ok(1), 0, 1));
    }

    #[test]
    fn grant_retry_gives_up_overloaded_at_its_bound() {
        let (result, waits, attempts) = run_retry(vec![], 5, false);
        assert_eq!(result, Err(Error::Overloaded));
        assert_eq!(waits, 5);
        assert_eq!(attempts, 6);
        // No time to wait at all: one attempt.
        assert_eq!(run_retry(vec![], 0, false), (Err(Error::Overloaded), 0, 1));
    }

    #[test]
    fn grant_retry_does_not_wait_on_other_errors() {
        assert_eq!(
            run_retry(vec![Err(Error::Unavailable), Ok(1)], 20, false),
            (Err(Error::Unavailable), 0, 1)
        );
        // A canceled sleep ends the wait with its error.
        assert_eq!(run_retry(vec![], 20, true), (Err(Error::Canceled), 1, 1));
    }

    #[test]
    fn grant_waits_are_counted() {
        let w = world();
        let (w, ma, _) = metered(w);
        w.a.links.record_grant_wait(false, true);
        w.a.links.record_grant_wait(true, true);
        w.a.links.record_grant_wait(true, false);
        assert_eq!(ma.count(Event::RdmaGrantWaits), 2);
        assert_eq!(ma.count(Event::RdmaGrantWaitGranted), 1);
        let mut out = String::new();
        ma.write_prometheus(&mut out).unwrap();
        assert!(out.contains("racer_rdma_grant_waits_total 2"));
        assert!(out.contains("racer_rdma_grant_wait_granted_total 1"));
    }

    /// An ahead host-read grant is deferred only while the rail's pool has
    /// fewer free buffers than the grant reserve, and each deferral counts.
    /// A rail without a pool never defers.
    #[test]
    fn ahead_grant_defers_only_while_the_pool_is_low() {
        let mut w = world();
        let env = w.env.clone();
        let b = w.b.links.local().clone();
        link_up(&env, &mut w);
        let (mut w, ma, _) = metered(w);
        let scope = scope(&env);
        let deferred = || ma.count(Event::RdmaGrantAheadDeferred);
        assert_eq!(crate::read::fill::ahead_grant_reserve(2), 1);
        assert!(!w.a.links.defer_ahead_grant(RAIL));
        let first = drive!(w, w.a.links.grant(&b, RAIL, TransferId([1; 16]), &scope)).unwrap();
        assert!(!w.a.links.defer_ahead_grant(RAIL), "one of two still free");
        let second = drive!(w, w.a.links.grant(&b, RAIL, TransferId([2; 16]), &scope)).unwrap();
        assert!(w.a.links.defer_ahead_grant(RAIL), "none free");
        assert_eq!(deferred(), 1);
        assert!(!w.a.links.defer_ahead_grant(RailId(99)), "no such pool");
        assert_eq!(deferred(), 1);
        drop((first, second));
        drop(w);
    }

    /// A receiver parked on an exhausted rail is woken by the release that
    /// frees a slot, with no polling: the request is pending until the sender
    /// declines a grant, then granted and reported as waited. One whose wait
    /// limit has passed fails `Overloaded`, and one that finds a slot free is
    /// granted without waiting.
    #[test]
    fn parked_grant_wakes_when_a_grant_is_released() {
        let mut w = world();
        let env = w.env.clone();
        let (a, b) = (w.a.links.local().clone(), w.b.links.local().clone());
        link_up(&env, &mut w);
        let scope = scope(&env);
        let until = env.ctx.now() + Duration::from_secs(1);
        let (first, waited) = drive!(
            w,
            w.a.links
                .grant_parked(&b, RAIL, TransferId([1; 16]), until, &scope)
        )
        .unwrap();
        assert!(!waited, "a free slot is granted at once");
        let _second = drive!(w, w.a.links.grant(&b, RAIL, TransferId([2; 16]), &scope)).unwrap();
        assert_eq!(
            drive!(w, w.a.links.grant(&b, RAIL, TransferId([3; 16]), &scope)).err(),
            Some(Error::Overloaded),
            "every buffer is granted"
        );
        // A wait limit already passed fails at once.
        assert_eq!(
            drive!(
                w,
                w.a.links
                    .grant_parked(&b, RAIL, TransferId([3; 16]), env.ctx.now(), &scope)
            )
            .err(),
            Some(Error::Overloaded)
        );
        let mut parked =
            Box::pin(
                w.a.links
                    .grant_parked(&b, RAIL, TransferId([4; 16]), until, &scope),
            );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..4 {
            assert!(parked.as_mut().poll(&mut cx).is_pending());
            pump!(w);
        }
        assert_eq!(w.a.links.grant_waiters(), 1);
        // The sender declines the first grant: the parked request takes the
        // freed buffer.
        let header = STANDARD.decode(first.header_value()).unwrap();
        let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([1; 16])).unwrap();
        w.b.links.release(&a, &grant);
        let (pending, waited) = drive!(w, parked).unwrap();
        assert!(waited);
        assert_eq!(pending.rail(), RAIL);
        assert_eq!(w.a.links.grant_waiters(), 0);
    }

    #[test]
    fn grant_backpressure_is_not_an_http_fallback() {
        let w = world();
        let (w, ma, _) = metered(w);
        w.a.links.record_grant_backpressure();
        assert_eq!(ma.count(Event::RdmaGrantBackpressure), 1);
        assert_eq!(ma.count(Event::RdmaHttpFallback), 0);
        assert_eq!(ma.count(Event::RdmaFallbackGrantOverloaded), 0);
        let mut out = String::new();
        ma.write_prometheus(&mut out).unwrap();
        assert!(out.contains("racer_rdma_grant_backpressure_total 1"));
    }

    /// Submit grants that the owner never drains until this worker's
    /// command queue is full. The returned futures hold the queued ops.
    fn fill_owner_queue<'a>(
        w: &'a World,
        b: &'a NodeId,
        scope: &'a RequestScope,
    ) -> Vec<Operation<'a, PendingReceive>> {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut queued = Vec::new();
        for n in 0..rdma_verbs::COMMAND_QUEUE {
            let mut grant: Operation<'a, PendingReceive> =
                Box::pin(w.a.links.grant(b, RAIL, TransferId([n as u8; 16]), scope));
            assert!(grant.as_mut().poll(&mut cx).is_pending());
            queued.push(grant);
        }
        queued
    }

    /// A grant whose command cannot reach a backed-up owner fails
    /// `Unavailable`, which falls back to HTTP, instead of `Overloaded`,
    /// which a page request would retry forever. It is counted apart.
    #[test]
    fn grant_behind_a_full_owner_queue_falls_back() {
        let mut w = world();
        let env = w.env.clone();
        let b = w.b.links.local().clone();
        link_up(&env, &mut w);
        let (w, ma, _) = metered(w);
        let scope = scope(&env);
        let queued = fill_owner_queue(&w, &b, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut late = Box::pin(w.a.links.grant(&b, RAIL, TransferId([0xff; 16]), &scope));
        assert_eq!(
            late.as_mut().poll(&mut cx).map(|r| r.err()),
            Poll::Ready(Some(Error::Unavailable))
        );
        assert_eq!(ma.count(Event::RdmaGrantOwnerQueueFull), 1);
        let mut out = String::new();
        ma.write_prometheus(&mut out).unwrap();
        assert!(out.contains("racer_rdma_grant_owner_queue_full_total 1"));
        drop(queued);
    }

    /// An owner that leaves queued commands undrained past the stall
    /// threshold makes every rail read as not ready, so new exchanges use
    /// HTTP; once it drains them, the rail is ready again. The owner loop
    /// counters and the stalled gauge are published.
    #[test]
    fn stalled_owner_takes_rails_out_of_service_until_it_drains() {
        let mut w = world();
        let env = w.env.clone();
        let b = w.b.links.local().clone();
        link_up(&env, &mut w);
        let (w, ma, _) = metered(w);
        let threshold = Duration::from_millis(20);
        let World {
            _sim,
            clock,
            env,
            a,
            b: node_b,
        } = w;
        let a = Node {
            links: Rc::new(
                Rc::into_inner(a.links)
                    .expect("unshared links")
                    .with_owner_stall(threshold),
            ),
            ..a
        };
        let mut w = World {
            _sim,
            clock,
            env: env.clone(),
            a,
            b: node_b,
        };
        assert!(w.a.links.ready(&b, RAIL).is_some());
        w.a.links.sync_link_metrics();
        assert_eq!(ma.gauge(Gauge::RdmaOwnerStalled), 0);
        assert!(ma.count(Event::RdmaOwnerPasses) > 0);
        let scope = scope(&env);
        let queued = fill_owner_queue(&w, &b, &scope);
        std::thread::sleep(threshold);
        assert!(w.a.links.owner_stalled());
        assert!(!w.a.links.rail_ready(RAIL));
        assert!(w.a.links.ready(&b, RAIL).is_none());
        w.a.links.sync_link_metrics();
        assert_eq!(ma.gauge(Gauge::RdmaOwnerStalled), 1);
        let passes = ma.count(Event::RdmaOwnerPasses);
        drop(queued);
        pump!(w);
        assert!(!w.a.links.owner_stalled());
        assert!(w.a.links.ready(&b, RAIL).is_some());
        w.a.links.sync_link_metrics();
        assert_eq!(ma.gauge(Gauge::RdmaOwnerStalled), 0);
        assert!(ma.count(Event::RdmaOwnerPasses) > passes);
        assert!(ma.count(Event::RdmaOwnerCommands) >= rdma_verbs::COMMAND_QUEUE as u64);
        let mut out = String::new();
        ma.write_prometheus(&mut out).unwrap();
        assert!(out.contains("racer_rdma_owner_stalled 0"));
        assert!(out.contains("racer_rdma_owner_immediate_wakes_total{reason=\"commands\"}"));
    }

    #[test]
    fn overloaded_grant_fallback_is_counted_apart() {
        let w = world();
        let (w, ma, _) = metered(w);
        w.a.links.record_fallback_grant(Error::Overloaded);
        w.a.links.record_fallback_grant(Error::Unavailable);
        assert_eq!(ma.count(Event::RdmaHttpFallback), 2);
        assert_eq!(ma.count(Event::RdmaFallbackGrant), 2);
        assert_eq!(ma.count(Event::RdmaFallbackGrantOverloaded), 1);
    }

    #[test]
    fn declined_sends_are_counted_by_reason_apart_from_http_fallback() {
        let w = world();
        let (w, ma, _) = metered(w);
        w.a.links.record_send_declined(false);
        w.a.links.record_send_declined(false);
        w.a.links.record_send_declined(true);
        assert_eq!(ma.count(Event::RdmaSendDeclinedPlan), 2);
        assert_eq!(ma.count(Event::RdmaSendDeclinedNotPage), 1);
        assert_eq!(ma.count(Event::RdmaHttpFallback), 0);
    }
}

#[test]
/// Every GPU gets a slice on every rail when slots allow, so no GPU is left
/// without landing on a node with more GPUs than rails. With fewer slots
/// than rails, NUMA-local rails come first. Slots are shared out evenly.
fn plan_landing_covers_every_gpu_and_prefers_numa() {
    let rails = [
        (RailId(1), Some(1)),
        (RailId(2), Some(0)),
        (RailId(3), None),
    ];
    let gpus = [(0, Some(0)), (1, Some(1)), (2, None), (3, Some(0))];
    let plan = plan_landing(&rails, &gpus, 7);
    for &(ordinal, _) in &gpus {
        let mine: Vec<_> = plan.iter().filter(|p| p.0 == ordinal).collect();
        assert_eq!(mine.len(), 3, "GPU {ordinal} is on every rail");
        assert_eq!(mine.iter().map(|p| p.2).sum::<usize>(), 7);
    }
    assert_eq!(
        plan.iter()
            .filter(|p| p.0 == 0)
            .copied()
            .collect::<Vec<_>>(),
        [(0, RailId(2), 3), (0, RailId(1), 2), (0, RailId(3), 2)]
    );
    // One slot per GPU: only the best rail.
    assert_eq!(
        plan_landing(&rails, &gpus, 1),
        [
            (0, RailId(2), 1),
            (1, RailId(1), 1),
            (2, RailId(1), 1),
            (3, RailId(2), 1),
        ]
    );
    // Two slots, two rails: one each, local first.
    assert_eq!(
        plan_landing(&rails[..2], &[(5, Some(1))], 2),
        [(5, RailId(1), 1), (5, RailId(2), 1)]
    );
    assert_eq!(plan_landing(&rails, &[], 4), []);
    assert_eq!(plan_landing(&[], &gpus, 4), []);
    assert_eq!(plan_landing(&rails, &gpus, 0), []);
}

/// Register a simulated GPU's landing buffer on `a`'s rail.
pub(crate) fn attach_landing(
    env: &racer_io::Env,
    w: &mut World,
    helper: &Arc<crate::gpu::GpuHelper>,
    bytes: u64,
) {
    w.a.links.node().set_gpu_landing(helper.clone(), bytes);
    let rails = w.a.links.active();
    let scope = scope(env);
    // The GPU helper answers from its own thread: poll with short sleeps.
    let mut future = std::pin::pin!(w.a.links.attach_landings(&rails, &scope));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..5000 {
        if future.as_mut().poll(&mut cx).is_ready() {
            return;
        }
        pump(&mut w.a.owner, &mut w.b.owner);
        std::thread::sleep(Duration::from_millis(1));
    }
    panic!("landing attach did not finish");
}

#[test]
/// A landing grant takes a slot of the GPU landing buffer: the peer's write
/// lands in GPU memory, the CPU path never sees it, and the slot is held
/// until the landed page drops. A landing grant for another GPU, or with no
/// free slot, is refused so the caller grants host memory instead.
fn landing_grant_receives_into_gpu_memory() {
    use std::os::unix::fs::FileExt;
    let helper = crate::gpu::testing::helper(&[None]);
    let mut w = world();
    let env = w.env.clone();
    let link = link_up(&env, &mut w);
    // One slot only.
    let bytes = (BUFFER_LEN as u64).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    attach_landing(&env, &mut w, &helper, bytes);
    assert!(w.a.links.has_landing(RAIL, 0));
    assert!(!w.a.links.has_landing(RAIL, 1));
    assert!(!w.b.links.has_landing(RAIL, 0));
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope(&env);
    assert_eq!(
        drive!(
            w,
            w.a.links
                .grant_landing(&b, RAIL, TransferId([3; 16]), 1, &scope)
        )
        .err(),
        Some(Error::Unavailable),
        "GPU 1 has no landing buffer"
    );
    let pending = drive!(
        w,
        w.a.links
            .grant_landing(&b, RAIL, TransferId([3; 16]), 0, &scope)
    )
    .unwrap();
    assert!(pending.is_landing());
    assert_eq!(pending.link(), link);
    assert!(
        drive!(
            w,
            w.a.links
                .grant_landing(&b, RAIL, TransferId([4; 16]), 0, &scope)
        )
        .is_err(),
        "the only slot is taken"
    );
    let header = STANDARD.decode(pending.header_value()).unwrap();
    let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([3; 16])).unwrap();
    let page = page(&w.b.admission.clone(), 35, 0x6b);
    drive!(w, w.b.links.send(&a, &grant, &page, &scope)).unwrap();
    let device = match drive!(w, poll_fn(|cx| pending.arrival.poll(cx))).unwrap() {
        Arrival::Device(page) => page,
        _ => panic!("expected a device page"),
    };
    // The bytes are in the GPU buffer, read here through its dma-buf.
    let landing = w.a.links.node().landing(RAIL, 0).unwrap();
    let mut bytes = [0; 35];
    let file = std::fs::File::from(
        rdma_verbs::DmabufMemory::fd(&*landing)
            .try_clone_to_owned()
            .unwrap(),
    );
    file.read_exact_at(&mut bytes, device.offset()).unwrap();
    assert_eq!(bytes, [0x6b; 35]);
    let landed = crate::gpu::LandedCiphertext::new(&landing, device).unwrap();
    assert_eq!((landed.ordinal(), landed.len()), (0, 35));
    // The host pool was not used.
    assert_eq!(w.a.links.buffers(RAIL, BufferState::Free), Some(2));
    drop(landed);
    pump!(w);
    let again = drive!(
        w,
        w.a.links
            .grant_landing(&b, RAIL, TransferId([5; 16]), 0, &scope)
    )
    .unwrap();
    let header = STANDARD.decode(again.header_value()).unwrap();
    let grant = AuthenticatedGrant::decode(&header, RAIL, TransferId([5; 16])).unwrap();
    w.b.links.release(&a, &grant);
    assert!(matches!(
        drive!(w, poll_fn(|cx| again.arrival.poll(cx))),
        Ok(Arrival::Released)
    ));
    // Closing the node forgets the landing buffer for the next round, and
    // once the node drains the GPU memory is freed.
    drop((landing, pending, again));
    w.a.links.close();
    assert!(!w.a.links.has_landing(RAIL, 0));
    for _ in 0..100 {
        pump!(w);
        if helper.run(crate::gpu::testing::live_staging) == 0 {
            return;
        }
        std::thread::sleep(Duration::from_millis(1));
    }
    panic!("landing buffer was not freed after the node drained");
}

#[test]
/// Two GPUs on one rail each get a landing slice and a separate device
/// pool: a read on either GPU lands in its own GPU's memory, and one GPU's
/// full slice does not refuse the other.
fn every_gpu_lands_on_a_shared_rail() {
    let helper = crate::gpu::testing::helper(&[None, None]);
    let mut w = world();
    let env = w.env.clone();
    link_up(&env, &mut w);
    let bytes = (BUFFER_LEN as u64).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    attach_landing(&env, &mut w, &helper, bytes);
    assert!(w.a.links.has_landing(RAIL, 0));
    assert!(w.a.links.has_landing(RAIL, 1));
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope(&env);
    let mut held = Vec::new();
    for (ordinal, fill) in [(0, 0x10u8), (1, 0x20)] {
        let transfer = TransferId([fill; 16]);
        let pending = drive!(
            w,
            w.a.links.grant_landing(&b, RAIL, transfer, ordinal, &scope)
        )
        .unwrap();
        let header = STANDARD.decode(pending.header_value()).unwrap();
        let grant = AuthenticatedGrant::decode(&header, RAIL, transfer).unwrap();
        let page = page(&w.b.admission.clone(), 20, fill);
        drive!(w, w.b.links.send(&a, &grant, &page, &scope)).unwrap();
        let device = match drive!(w, poll_fn(|cx| pending.arrival.poll(cx))).unwrap() {
            Arrival::Device(page) => page,
            _ => panic!("expected a device page"),
        };
        let landing = w.a.links.node().landing(RAIL, ordinal).unwrap();
        let landed = crate::gpu::LandedCiphertext::new(&landing, device).unwrap();
        assert_eq!(landed.ordinal(), ordinal);
        held.push(landed);
    }
    // Each GPU's only slot is taken.
    for ordinal in [0, 1] {
        assert_eq!(
            drive!(
                w,
                w.a.links
                    .grant_landing(&b, RAIL, TransferId([9; 16]), ordinal, &scope)
            )
            .err(),
            Some(Error::Overloaded)
        );
    }
    drop(held);
}

#[test]
/// No usable landing: too little memory, no GPU, or a failed registration
/// leaves the rail host-only.
fn landing_failures_leave_rails_host_only() {
    let helper = crate::gpu::testing::helper(&[None]);
    let mut w = world();
    let env = w.env.clone();
    attach_landing(&env, &mut w, &helper, (BUFFER_LEN as u64) / 2);
    assert!(!w.a.links.has_landing(RAIL, 0));
    attach_landing(&env, &mut w, &helper, 0);
    assert!(!w.a.links.has_landing(RAIL, 0));
    w._sim.fault(
        rdma_verbs::simulation::Operation::RegisterDmabuf,
        rdma_verbs::simulation::Fault::Reject,
    );
    let bytes = (BUFFER_LEN as u64).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    attach_landing(&env, &mut w, &helper, bytes);
    assert!(!w.a.links.has_landing(RAIL, 0));
    // No rail holds the buffer, so its GPU memory is freed again.
    assert_eq!(helper.run(crate::gpu::testing::live_staging), 0);
    // Registration works on the next try, with a new GPU buffer.
    attach_landing(&env, &mut w, &helper, bytes);
    assert!(w.a.links.has_landing(RAIL, 0));
    assert_eq!(helper.run(crate::gpu::testing::live_staging), 1);
}

#[test]
/// A GPU helper that starts after the rails activated (lazy `gpu-read`
/// start) marks them pending; attaching on the active rails registers the
/// landing buffer and consumes the mark, so rails never attach twice.
fn late_gpu_landing_attaches_on_active_rails_once() {
    let helper = crate::gpu::testing::helper(&[None]);
    let mut w = world();
    let env = w.env.clone();
    link_up(&env, &mut w);
    let node = w.a.links.node().clone();
    assert!(!node.take_landing_pending());
    node.set_gpu_landing(helper.clone(), 0);
    assert!(!node.take_landing_pending(), "0 bytes turns landing off");
    let bytes = (BUFFER_LEN as u64).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    node.set_gpu_landing(helper.clone(), bytes);
    assert!(!w.a.links.has_landing(RAIL, 0));
    // `attach_landing` sets the landing again, then attaches like the owner
    // worker does on its next turn.
    attach_landing(&env, &mut w, &helper, bytes);
    assert!(w.a.links.has_landing(RAIL, 0));
    assert!(!node.take_landing_pending(), "the attach consumed the mark");
    node.set_gpu_landing(helper, bytes);
    assert!(node.take_landing_pending());
    assert!(!node.take_landing_pending());
}

/// Crash and restart `b` under the same node id: the new owner has no links,
/// while `a` still holds its `Ready` link to the dead queue pair (half-open).
fn restart_b(env: &racer_io::Env, w: &mut World) {
    let environment = w.clock.ctx(1);
    let fresh = node(&w._sim, 3, node_id(2), Some(&environment));
    drop(std::mem::replace(&mut w.b, fresh));
    let admission = w.b.admission.clone();
    let rails = drive!(
        w,
        w.b.links
            .activate(vec![mapping(3)], &admission, 2 * BUFFER_LEN, &scope(env))
    )
    .unwrap();
    assert_eq!(rails.len(), 1);
    pump!(w);
}

/// A membership change closes links to peers that are no longer RDMA edges
/// at once (here the local node left, so none are), without backoff, and a
/// membership is applied only once.
#[test]
fn membership_change_closes_links_to_non_edges() {
    let mut w = world();
    let env = w.env.clone();
    let b = node_id(2);
    link_up(&env, &mut w);
    let membership = crate::rdma::tests::rails_tests::mesh(8);
    w.a.links.retain_edges(&membership);
    pump!(w);
    assert_eq!(w.a.links.state(&b, RAIL), LinkState::Idle);
    assert_eq!(w.b.links.state(&node_id(1), RAIL), LinkState::Idle);
    // No backoff: the link comes straight back, and the same membership
    // does not close it again. (Wait out the per-peer setup rate limit.)
    w.clock.advance(SETUP_INTERVAL);
    link_up(&env, &mut w);
    w.a.links.retain_edges(&membership);
    pump!(w);
    assert!(w.a.links.ready(&b, RAIL).is_some());
}

/// After a peer restarts, the survivor's link stays `Ready`
/// (no traffic means no completion reports the dead peer), so the survivor
/// never wants a setup. The restarted peer's setup replaces the stale link and
/// both ends become ready again.
#[test]
fn half_open_link_after_peer_restart_recovers_when_the_peer_initiates() {
    let mut w = world();
    let env = w.env.clone();
    let (a, b) = (node_id(1), node_id(2));
    let old = link_up(&env, &mut w);
    restart_b(&env, &mut w);
    assert_eq!(w.a.links.ready(&b, RAIL).map(|(_, id)| id), Some(old));
    assert_eq!(w.b.links.state(&a, RAIL), LinkState::Idle);
    assert!(!w.a.links.wants(&b, RAIL), "the survivor sees a ready link");
    assert!(w.b.links.wants(&a, RAIL), "the restarted peer wants a link");
    let scope = scope(&env);
    let (epoch, ours) = drive!(w, w.b.links.open(&a, RAIL, &scope)).unwrap();
    assert_eq!(w.a.links.admit_setup(&b, RAIL), SetupAdmission::Accept);
    let theirs = drive!(w, w.a.links.accept(&b, RAIL, &ours, &scope)).unwrap();
    drive!(
        w,
        w.b.links.connect(
            &a,
            RAIL,
            epoch,
            &ours,
            &theirs,
            &link_setup::binding(&ours),
            &scope
        )
    )
    .unwrap();
    pump!(w);
    let (_, id_a) = w.a.links.ready(&b, RAIL).expect("survivor ready");
    let (_, id_b) = w.b.links.ready(&a, RAIL).expect("restarted peer ready");
    assert_eq!(id_a, id_b);
    assert_ne!(id_a, old, "the stale link was replaced");
}

/// The survivor's first use of the half-open link fails at
/// the transport and breaks it, so its own setups resume after the backoff.
#[test]
fn half_open_link_after_peer_restart_breaks_on_first_use() {
    let mut w = world();
    let env = w.env.clone();
    let b = node_id(2);
    link_up(&env, &mut w);
    restart_b(&env, &mut w);
    assert!(w.a.links.ready(&b, RAIL).is_some());
    let scope = scope(&env);
    // The grant itself may be posted before the transport reports the dead
    // peer; either way the link must not stay ready.
    let _ = drive!(w, w.a.links.grant(&b, RAIL, TransferId([6; 16]), &scope));
    pump!(w);
    assert!(w.a.links.ready(&b, RAIL).is_none(), "half-open link broke");
    for _ in 0..8 {
        w.clock.advance(SETUP_DEADLINE);
        pump!(w);
    }
    assert!(w.a.links.wants(&b, RAIL));
}

/// Setup tasks hold an `Rc<Links>` (see `connect_link`), so `Links::tasks` is
/// a reference cycle. `abandon_setups` must break it so an undrained owner
/// releases the tasks and their admission charges.
#[test]
fn abandon_setups_breaks_the_task_cycle_and_releases_charges() {
    let w = world_for([node_id(1), node_id(2)], true);
    let links = w.a.links.clone();
    let charge =
        w.a.admission
            .reserve(None, ResourceClass::RequestContext, 128)
            .unwrap();
    let held = links.clone();
    links.spawn(Box::pin(async move {
        let _owned = (held, charge);
        std::future::pending::<()>().await;
        Ok(())
    }));
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert_eq!(links.poll(&mut cx), 1);
    assert_eq!(w.a.admission.used(ResourceClass::RequestContext), 128);
    // The world, this test, and the pending task each own one reference.
    assert_eq!(Rc::strong_count(&links), 3);

    links.abandon_setups();
    assert_eq!(links.setups(), 0);
    assert_eq!(Rc::strong_count(&links), 2, "the task's reference is gone");
    assert_eq!(w.a.admission.used(ResourceClass::RequestContext), 0);

    // An abandoned facade refuses new setups, like a drained one.
    links.spawn(Box::pin(async { Ok(()) }));
    assert_eq!(links.setups(), 0);
    // Idempotent.
    links.abandon_setups();
}
