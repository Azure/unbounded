//! Links facade over two simulated nodes: activation, setup, tie-break, rate
//! limit, and page grants without HTTP.

use super::*;
use rdma_verbs::BufferState;
use rdma_verbs::Owner;
use rdma_verbs::simulation::Device;
use rdma_verbs::simulation::Simulation;
use std::future::Future;
use std::task::Context;
use uring_runtime::environment::Environment;
use uring_runtime::environment::SimulationClock;

const RAIL: RailId = RailId(7);

pub(crate) struct Node {
    pub links: Rc<Links>,
    pub owner: Owner,
    pub admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}

pub(crate) struct World {
    pub _sim: Simulation,
    pub clock: SimulationClock,
    pub _environment: Option<uring_runtime::environment::Guard>,
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

pub(crate) fn scope() -> RequestScope {
    RequestScope::new(
        crate::model::RequestId([1; 16]),
        uring_runtime::environment::now() + Duration::from_secs(5),
    )
    .unwrap()
}

fn node(sim: &Simulation, n: u8, id: NodeId, environment: Option<&Environment>) -> Node {
    let view = sim
        .with_devices(vec![Device::new(format!("sim{n}"), [n; 16])])
        .unwrap();
    let (mut clients, port) = rdma_verbs::node(1).unwrap();
    let owner = {
        let _scope = view.enter();
        let _environment = environment.map(Environment::enter);
        Owner::new(port)
    };
    let limits = crate::test_support::cluster::config(true).limits;
    Node {
        links: Rc::new(Links::new(
            clients.remove(0),
            Arc::new(LinkNode::default()),
            id,
            2,
            None,
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
    let cache = racer_control_wire::CacheId("cccccccc-1111-4111-8111-111111111111".into());
    PageEnvelope {
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
        key_id: racer_control_wire::KeyId([1; 16]),
        nonce: Nonce([2; 24]),
        plaintext_length: length - 16,
        ciphertext_length: length,
    }
}

/// Write `page` from `b` into a buffer granted by `a` over `link_up`'s link and
/// return the received registered page. `a` then has one buffer `Owned`.
pub(crate) fn receive_registered(
    w: &mut World,
    page: &CiphertextPage,
) -> rdma_verbs::RegisteredPage {
    let (a, b) = (w.a.links.local().clone(), w.b.links.local().clone());
    let scope = scope();
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
    let clock = SimulationClock::new(7);
    let environment = clock.environment(1);
    let a = node(&sim, 1, id_a, simulated.then_some(&environment));
    let b = node(&sim, 2, id_b, simulated.then_some(&environment));
    let guard = simulated.then(|| environment.enter());
    let mut world = World {
        _sim: sim,
        clock,
        _environment: guard,
        a,
        b,
    };
    for n in [1, 2] {
        let scope = scope();
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
pub(crate) fn link_up(w: &mut World) -> u64 {
    let (a, b) = (w.a.links.local().clone(), w.b.links.local().clone());
    let scope = scope();
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
    assert_eq!(BUFFER_LEN % 4096, 0);
    const { assert!(BUFFER_LEN >= MAX_CIPHERTEXT && BUFFER_LEN - MAX_CIPHERTEXT < 4096) };
    assert_eq!(buffer_count(2 * BUFFER_LEN - 1), 1);
    assert_eq!(buffer_count(2 * BUFFER_LEN), 2);
    assert_eq!(grants_per_link(0), 1);
    assert_eq!(grants_per_link(16), 16);
    assert_eq!(
        grants_per_link(usize::MAX),
        crate::rdma::link_setup::MAX_GRANTS_PER_LINK
    );
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
    let clock = SimulationClock::new(7);
    let environment = clock.environment(1);
    let mut n = node(&sim, 1, node_id(1), Some(&environment));
    let _guard = environment.enter();
    let scope = scope();
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
    let clock = SimulationClock::new(7);
    let environment = clock.environment(1);
    let mut a = node(&sim, 1, node_id(1), Some(&environment));
    let mut b = node(&sim, 2, node_id(2), Some(&environment));
    let _guard = environment.enter();
    let scope = scope();
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
fn setup_page_both_directions_and_release() {
    let mut w = world();
    let link = link_up(&mut w);
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope();
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
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope();
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
fn cancelled_open_clears_its_reservation() {
    let mut w = world();
    let b = node_id(2);
    let scope = scope();
    {
        let mut open = std::pin::pin!(w.a.links.open(&b, RAIL, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(open.as_mut().poll(&mut cx).is_pending());
        assert!(!w.a.links.wants(&b, RAIL), "reserved while waiting");
    }
    pump!(w);
    assert!(LinkNode::lock(&w.a.links.node.initiating).is_empty());
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
    drive!(w, w.a.links.open(&b, RAIL, &self::scope())).unwrap();
}

#[test]
fn open_dropped_after_the_owner_answered_breaks_its_epoch() {
    let mut w = world();
    let b = node_id(2);
    let scope = scope();
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
    assert!(LinkNode::lock(&w.a.links.node.initiating).is_empty());
    assert!(matches!(
        w.a.links.state(&b, RAIL),
        LinkState::Backoff(_) | LinkState::Breaking
    ));
    for _ in 0..8 {
        w.clock.advance(SETUP_DEADLINE);
        pump!(w);
    }
    assert!(w.a.links.wants(&b, RAIL));
    drive!(w, w.a.links.open(&b, RAIL, &self::scope())).unwrap();
}

#[test]
fn dropped_setup_attempt_abandons_its_epoch() {
    let mut w = world();
    let b = node_id(2);
    let scope = scope();
    let (epoch, _) = drive!(w, w.a.links.open(&b, RAIL, &scope)).unwrap();
    drop(SetupAttempt::new(&w.a.links, &b, RAIL, epoch));
    pump!(w);
    assert!(LinkNode::lock(&w.a.links.node.initiating).is_empty());
    assert!(matches!(
        w.a.links.state(&b, RAIL),
        LinkState::Backoff(_) | LinkState::Breaking
    ));
    for _ in 0..8 {
        w.clock.advance(SETUP_DEADLINE);
        pump!(w);
    }
    assert!(w.a.links.wants(&b, RAIL));
    drive!(w, w.a.links.open(&b, RAIL, &self::scope())).unwrap();
}

#[test]
fn stale_setup_guards_leave_a_newer_attempt() {
    let mut w = world();
    let b = node_id(2);
    let scope = scope();
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
        LinkNode::lock(&w.a.links.node.initiating).get(&key),
        Some(&Initiating::Opened(epoch))
    );
    assert!(!w.a.links.wants(&b, RAIL));
    // The live attempt still finishes normally.
    SetupAttempt::new(&w.a.links, &b, RAIL, epoch).finish();
    assert!(LinkNode::lock(&w.a.links.node.initiating).contains_key(&key));
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

    fn granted(w: &mut World, transfer: u8) -> (PendingReceive, AuthenticatedGrant) {
        let scope = scope();
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

    #[test]
    fn cancelled_after_the_write_lands_breaks_the_epoch() {
        let mut w = world();
        link_up(&mut w);
        let (pending, grant) = granted(&mut w, 3);
        let page = page(&w.b.admission.clone(), 35, 0x5a);
        let links = w.b.links.clone();
        let scope = scope();
        let to = node_id(1);
        let mut send = std::pin::pin!(links.send(&to, &grant, &page, &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(send.as_mut().poll(&mut cx).is_pending());
        // The owner completes the write before the worker sees the result.
        assert_eq!(arrived(&mut w, &pending), vec![0x5a; 35]);
        scope.cancel().unwrap();
        assert!(matches!(
            send.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        // The caller still releases, as `send_native` does; the tag is gone.
        links.release(&to, &grant);
        pump!(w);
        assert!(
            links.ready(&to, RAIL).is_none(),
            "ambiguous write broke the epoch"
        );
    }

    #[test]
    fn unadmitted_release_breaks_the_epoch() {
        let mut w = world();
        link_up(&mut w);
        let (pending, grant) = granted(&mut w, 3);
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
mod send_in_place {
    use super::*;
    use crate::telemetry::{Event, Metrics};

    /// `w` with `metrics` attached to both nodes' links.
    fn metered(w: World) -> (World, Metrics, Metrics) {
        let (ma, mb) = (Metrics::default(), Metrics::default());
        let World {
            _sim,
            clock,
            _environment,
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
                _environment,
                a,
                b,
            },
            ma,
            mb,
        )
    }

    /// Grant on `a`, send `page` from `b`, and return what `a` received.
    fn transfer(w: &mut World, page: &CiphertextPage) -> Vec<u8> {
        receive_registered(w, page).bytes().to_vec()
    }

    #[test]
    fn send_writes_the_page_in_place_and_counts_it() {
        let mut w = world();
        link_up(&mut w);
        let (mut w, _, mb) = metered(w);
        let page = page(&w.b.admission.clone(), 35, 0x5a);
        let received = transfer(&mut w, &page);
        assert_eq!(received, page.bytes());
        assert_eq!(mb.count(Event::RdmaPagesSent), 1);
        assert_eq!(mb.count(Event::RdmaSentBytes), 35);
        pump!(w);
        assert_eq!(w.b.links.buffers(RAIL, BufferState::Free), Some(2));
        assert_eq!(w.a.links.buffers(RAIL, BufferState::Free), Some(2));
    }

    #[test]
    fn page_dropped_by_the_caller_stays_alive_until_the_completion() {
        let mut w = world();
        link_up(&mut w);
        let scope = scope();
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
        let clock = SimulationClock::new(7);
        let environment = clock.environment(1);
        let mut a = node(&sim, 1, node_id(1), Some(&environment));
        let mut b = node(&sim, 2, node_id(2), Some(&environment));
        let _guard = environment.enter();
        let scope = scope();
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
}

#[test]
/// Rails take a free GPU on their NUMA node first, then any free GPU; each
/// GPU serves one rail at most.
fn assign_landing_prefers_numa_then_any() {
    let rails = [
        (RailId(1), Some(1)),
        (RailId(2), Some(0)),
        (RailId(3), None),
        (RailId(4), Some(0)),
    ];
    let gpus = [(0, Some(0)), (1, Some(1)), (2, None)];
    assert_eq!(
        assign_landing(&rails, &gpus),
        [(RailId(1), 1), (RailId(2), 0), (RailId(3), 2)]
    );
    assert_eq!(assign_landing(&rails, &[]), []);
    assert_eq!(
        assign_landing(&[(RailId(9), Some(3))], &[(5, Some(1))]),
        [(RailId(9), 5)]
    );
}

/// Register a simulated GPU's landing buffer on `a`'s rail.
fn attach_landing(w: &mut World, helper: &Arc<crate::gpu::GpuHelper>, bytes: u64) {
    w.a.links.node().set_gpu_landing(helper.clone(), bytes);
    let rails = w.a.links.active();
    let scope = scope();
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
    let link = link_up(&mut w);
    // One slot only.
    let bytes = (BUFFER_LEN as u64).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    attach_landing(&mut w, &helper, bytes);
    assert_eq!(w.a.links.landing_ordinal(RAIL), Some(0));
    assert_eq!(w.b.links.landing_ordinal(RAIL), None);
    let (a, b) = (node_id(1), node_id(2));
    let scope = scope();
    assert_eq!(
        drive!(
            w,
            w.a.links
                .grant_landing(&b, RAIL, TransferId([3; 16]), 1, &scope)
        )
        .err(),
        Some(Error::Unavailable),
        "landing buffer is on another GPU"
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
    let landing = w.a.links.node().landing(RAIL).unwrap();
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
    // Closing the node forgets the landing buffer for the next round.
    w.a.links.close();
    assert_eq!(w.a.links.landing_ordinal(RAIL), None);
}

#[test]
/// No usable landing: too little memory, no GPU, or a failed registration
/// leaves the rail host-only.
fn landing_failures_leave_rails_host_only() {
    let helper = crate::gpu::testing::helper(&[None]);
    let mut w = world();
    attach_landing(&mut w, &helper, (BUFFER_LEN as u64) / 2);
    assert_eq!(w.a.links.landing_ordinal(RAIL), None);
    attach_landing(&mut w, &helper, 0);
    assert_eq!(w.a.links.landing_ordinal(RAIL), None);
    w._sim.fault(
        rdma_verbs::simulation::Operation::RegisterDmabuf,
        rdma_verbs::simulation::Fault::Reject,
    );
    let bytes = (BUFFER_LEN as u64).next_multiple_of(crate::config::GPU_STAGING_UNIT);
    attach_landing(&mut w, &helper, bytes);
    assert_eq!(w.a.links.landing_ordinal(RAIL), None);
    // Registration works on the next try, reusing the same GPU buffer.
    attach_landing(&mut w, &helper, bytes);
    assert_eq!(w.a.links.landing_ordinal(RAIL), Some(0));
}
