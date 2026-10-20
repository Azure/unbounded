//! Links facade over two simulated nodes: activation, setup, tie-break, rate
//! limit, and page grants without HTTP.

use super::*;
use racer_platform::environment::Environment;
use racer_platform::environment::SimulationClock;
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
    pub clock: SimulationClock,
    pub _environment: Option<racer_platform::environment::Guard>,
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
        racer_platform::environment::now() + Duration::from_secs(5),
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
    use crate::model::*;
    let cache = racer_control_wire::CacheId("cccccccc-1111-4111-8111-111111111111".into());
    let envelope = PageEnvelope {
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
    };
    BufferPool::new(admission.clone())
        .ciphertext(
            admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, length as usize)
                .unwrap(),
            envelope,
            vec![byte; length as usize],
        )
        .unwrap()
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
    assert_eq!(grants_per_link(usize::MAX), u32::MAX);
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
fn activation_without_two_buffers_settles_inactive() {
    let sim = Simulation::new();
    let clock = SimulationClock::new(7);
    let environment = clock.environment(1);
    let mut n = node(&sim, 1, node_id(1), Some(&environment));
    let _guard = environment.enter();
    let scope = scope();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let result = std::pin::pin!(n.links.activate(
        vec![mapping(1)],
        &n.admission,
        2 * BUFFER_LEN - 1,
        &scope
    ))
    .poll(&mut cx);
    assert_eq!(result, Poll::Ready(Err(Error::Unavailable)));
    assert!(n.links.node().settled());
    assert!(n.links.active().is_empty());
    assert!(!n.links.rail_ready(RAIL));
    assert_eq!(n.admission.used(ResourceClass::Registered), 0);
    n.owner.poll_budgeted(8).unwrap();
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
            Arrival::Released => panic!("expected a page"),
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
