//! Link state machine tests over the simulated fabric.

use std::{
    task::{Context, Poll},
    time::Duration,
};

use super::*;
use crate::{
    ffi::{self, lifetime_tests, status},
    simulation::{Device, Fault, Operation, Simulation},
};

const LEN: usize = 4096;
const GRANTS: u32 = 2;
const ID: u64 = 0x1d;
const SECOND: Duration = Duration::from_secs(1);

struct Side {
    link: Link,

    pool: Pool,

    device: Rc<NativeDevice>,

    quota: Arc<GuardOwner>,
}

struct Pair {
    sim: Simulation,

    a: Side,

    b: Side,

    now: Instant,
}

fn limits() -> Limits {
    Limits {
        grants: GRANTS,
        buffer_len: LEN as u32,
        idle: Duration::from_secs(120),
    }
}

/// One node with a pool of `buffers` page buffers and an unconnected link.
fn side(sim: &Simulation, n: u8, role: Role, buffers: usize, now: Instant) -> Side {
    side_with(sim, n, role, buffers, GRANTS, now)
}

/// Like [`side`], with a local grant limit of `grants`.
fn side_with(
    sim: &Simulation,
    n: u8,
    role: Role,
    buffers: usize,
    grants: u32,
    now: Instant,
) -> Side {
    let node = sim
        .with_devices(vec![Device::new(format!("sim{n}"), [n; 16])])
        .unwrap();
    let _scope = node.enter();
    let device = Rc::new(ffi::discover().unwrap().remove(0));
    let (quota, _) = lifetime_tests::quota();
    let region =
        NativeRegion::with_access(device.clone(), buffers * LEN, Access::PAGES, quota.clone())
            .unwrap();
    let pool = Pool::new(region, LEN, quota.clone()).unwrap();
    let limits = Limits { grants, ..limits() };
    let link = Link::new(Epoch(n.into()), role, &device, &quota, limits, now).unwrap();
    Side {
        link,
        pool,
        device,
        quota,
    }
}

/// `a` initiates, `b` responds, with the given link ids.
fn connect(sim: Simulation, ids: (u64, u64)) -> Pair {
    let now = Instant::now();
    let mut a = side(&sim, 1, Role::Initiator, 4, now);
    let mut b = side(&sim, 2, Role::Responder, 4, now);
    b.link
        .connect(
            a.link.endpoint(),
            a.link.recv_credits(),
            a.link.grant_limit(),
            ids.1,
            now,
        )
        .unwrap();
    assert_eq!(b.link.phase(), Phase::Connecting);
    a.link
        .connect(
            b.link.endpoint(),
            b.link.recv_credits(),
            b.link.grant_limit(),
            ids.0,
            now,
        )
        .unwrap();
    assert_eq!(a.link.phase(), Phase::Ready);
    Pair { sim, a, b, now }
}

/// A ready pair.
fn pair() -> Pair {
    let mut p = connect(Simulation::new(), (ID, ID));
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(p.b.link.phase(), Phase::Ready);
    p
}

/// Poll both sides several rounds at `p.now`. Returns each side's first break.
fn pump(p: &mut Pair) -> (Option<Break>, Option<Break>) {
    let (mut a, mut b) = (None, None);

    for _ in 0..12 {
        if a.is_none() {
            a = p.a.link.poll(&p.a.pool, p.now).err();
        }

        if b.is_none() {
            b = p.b.link.poll(&p.b.pool, p.now).err();
        }
    }

    (a, b)
}

fn poll<T>(op: &Op<T>) -> Option<Result<T>> {
    match op.poll(&mut Context::from_waker(futures::task::noop_waker_ref())) {
        Poll::Ready(result) => Some(result),
        Poll::Pending => None,
    }
}

fn grant(side: &mut Side, length: u32, deadline: Instant, now: Instant) -> Op<Granted> {
    let (granted, done) = op();
    side.link
        .grant(&side.pool, [7; 16], length, deadline, done, now);
    granted
}

fn write(side: &mut Side, tag: u32, bytes: Vec<u8>, deadline: Instant, now: Instant) -> Op<()> {
    let (written, done) = op();
    side.link.write(
        &side.pool,
        tag,
        Source::Bytes(Arc::new(bytes)),
        deadline,
        done,
        now,
    );
    written
}

fn release(side: &mut Side, tag: u32, now: Instant) -> Op<()> {
    let (released, done) = op();
    side.link.release(tag, done, now);
    released
}

fn free(side: &Side) -> usize {
    side.pool.shared().count_in(BufferState::Free)
}

/// Grant on the receiver, write from the sender, and return the page.
fn transfer(p: &mut Pair, to_a: bool, bytes: &[u8]) -> RegisteredPage {
    let deadline = p.now + 5 * SECOND;
    let now = p.now;
    let (receiver, sender) = if to_a {
        (&mut p.a, &mut p.b)
    } else {
        (&mut p.b, &mut p.a)
    };
    let granted = grant(receiver, bytes.len() as u32, deadline, now);
    let _ = (receiver, sender);
    assert_eq!(pump(p), (None, None));
    let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
    let sender = if to_a { &mut p.b } else { &mut p.a };
    let written = write(sender, tag, bytes.to_vec(), deadline, now);
    assert_eq!(pump(p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    match poll(&arrival).unwrap().unwrap() {
        Arrival::Page(page) => page,
        Arrival::Released => panic!("expected a page"),
    }
}

/// Destroy both links and check that every buffer is free.
fn finish(mut p: Pair) {
    assert!(p.a.link.destroy(&p.a.pool));
    assert!(p.b.link.destroy(&p.b.pool));
    assert_eq!(free(&p.a), 4);
    assert_eq!(free(&p.b), 4);
    let sim = p.sim.clone();
    drop(p);
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// Depths follow the design and reject zero grants.
fn depths_match_design() {
    assert_eq!(depths(2), Some((16, 8)));
    assert_eq!(depths(0), None);
    assert_eq!(depths(u32::MAX), None);
}

#[test]
/// Pages flow both ways over one QP, concurrently, and buffers return.
fn pages_in_both_directions_on_one_link() {
    let mut p = pair();
    assert_eq!(p.a.link.link_id(), ID);
    assert_eq!((p.a.link.epoch(), p.b.link.epoch()), (Epoch(1), Epoch(2)));
    assert_eq!(p.a.link.role(), Role::Initiator);
    assert_eq!(p.b.link.ready_since(), Some(p.now));
    let deadline = p.now + 5 * SECOND;
    let now = p.now;
    let into_a = grant(&mut p.a, 100, deadline, now);
    let into_b = grant(&mut p.b, 200, deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    let into_a = poll(&into_a).unwrap().unwrap();
    let into_b = poll(&into_b).unwrap().unwrap();
    let wa = write(&mut p.b, into_a.tag, vec![0xa; 100], deadline, now);
    let wb = write(&mut p.a, into_b.tag, vec![0xb; 150], deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!((poll(&wa), poll(&wb)), (Some(Ok(())), Some(Ok(()))));
    let Some(Ok(Arrival::Page(page_a))) = poll(&into_a.arrival) else {
        panic!("no page for a");
    };
    let Some(Ok(Arrival::Page(page_b))) = poll(&into_b.arrival) else {
        panic!("no page for b");
    };
    assert_eq!(page_a.bytes(), [0xa; 100]);
    assert_eq!(page_b.bytes(), [0xb; 150]);
    assert_eq!(free(&p.a), 3);
    drop((page_a, page_b));
    for round in 0..20u8 {
        let page = transfer(&mut p, round % 2 == 0, &[round; 64]);
        assert_eq!(page.bytes(), [round; 64]);
    }
    assert_eq!((free(&p.a), free(&p.b)), (4, 4));
    finish(p);
}

#[test]
/// A one-directional stream returns credits with standalone `Credit`
/// messages and never hits RNR.
fn long_one_way_run_never_hits_rnr() {
    let mut p = pair();
    for round in 0..100u32 {
        let page = transfer(&mut p, true, &round.to_be_bytes());
        assert_eq!(page.bytes(), round.to_be_bytes());
        assert!(p.a.link.owed < p.a.link.recv_depth && p.b.link.owed < p.b.link.recv_depth);
    }
    assert!(p.a.link.failure().is_none() && p.b.link.failure().is_none());
    finish(p);
}

#[test]
/// A write posted before its `Grant` waits for it; one whose `Grant` never
/// comes fails at its deadline without breaking the link.
fn write_waits_for_grant_then_times_out() {
    let mut p = pair();
    let now = p.now;
    let early = write(&mut p.b, 1, vec![5; 10], now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&early), None);
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    assert_eq!(granted.tag, 1);
    assert_eq!(poll(&early), Some(Ok(())));
    let Some(Ok(Arrival::Page(page))) = poll(&granted.arrival) else {
        panic!("no page");
    };
    assert_eq!(page.bytes(), [5; 10]);
    drop(page);
    let never = write(&mut p.b, 77, vec![1], now + SECOND, now);
    assert_eq!(
        poll(&write(&mut p.b, 77, vec![1], now + SECOND, now)),
        Some(Err(Error::InvalidRequest))
    );
    p.now += SECOND;
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&never), Some(Err(Error::DeadlineExceeded)));
    finish(p);
}

#[test]
/// `Release` ends a grant without CPU access, before or after the `Grant`
/// arrives, and replaces a waiting write.
fn release_frees_without_exposing() {
    let mut p = pair();
    let now = p.now;
    let deadline = now + 5 * SECOND;
    let granted = grant(&mut p.a, 10, deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    let released = release(&mut p.b, granted.tag, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&released), Some(Ok(())));
    assert!(matches!(
        poll(&granted.arrival),
        Some(Ok(Arrival::Released))
    ));
    assert_eq!(free(&p.a), 4);
    assert_eq!(
        poll(&release(&mut p.b, granted.tag, now)),
        None,
        "an unknown tag waits for its Grant"
    );
    // Release before the Grant arrives, replacing a waiting write.
    let waiting = write(&mut p.b, 2, vec![1], deadline, now);
    let early = release(&mut p.b, 2, now);
    assert_eq!(poll(&waiting), Some(Err(Error::Cancelled)));
    assert_eq!(
        poll(&release(&mut p.b, 2, now)),
        Some(Err(Error::InvalidRequest))
    );
    let second = grant(&mut p.a, 10, deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    let second = poll(&second).unwrap().unwrap();
    assert_eq!(second.tag, 2);
    assert_eq!(poll(&early), Some(Ok(())));
    assert!(matches!(poll(&second.arrival), Some(Ok(Arrival::Released))));
    assert_eq!(free(&p.a), 4);
    // Writing a released tag is just a wait for a new Grant.
    finish(p);
}

#[test]
/// An abandoned arrival frees the buffer when the release arrives late.
fn abandoned_arrival_with_late_release() {
    let mut p = pair();
    let now = p.now;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
    drop(arrival);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(p.a.pool.shared().count_in(BufferState::Exposed), 1);
    let released = release(&mut p.b, tag, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&released), Some(Ok(())));
    assert_eq!(free(&p.a), 4);
    // An abandoned arrival whose write lands frees the page right away.
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
    drop(arrival);
    let written = write(&mut p.b, tag, vec![1; 10], now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    assert_eq!(free(&p.a), 4);
    finish(p);
}

#[test]
/// An exposed grant that is never written or released breaks the link at
/// its deadline; destroy fences and frees the buffer.
fn exposed_grant_deadline_breaks() {
    let mut p = pair();
    let now = p.now;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    p.now += 5 * SECOND;
    assert_eq!(p.a.link.poll(&p.a.pool, p.now), Err(Break::Error));
    assert!(p.a.link.destroy(&p.a.pool));
    assert_eq!(
        poll(&granted.arrival).map(|r| r.err()),
        Some(Some(Error::Unavailable))
    );
    assert_eq!(free(&p.a), 4);
    // The peer sees its QP fail on its next post or poll.
    finish(p);
}

#[test]
/// Dropping the grant op before the bind completes frees the buffer and
/// never sends a `Grant`. A bound grant past its deadline is not sent.
fn abandoned_or_late_grant_is_never_exposed() {
    let mut p = pair();
    let now = p.now;
    drop(grant(&mut p.a, 10, now + 5 * SECOND, now));
    assert_eq!(pump(&mut p), (None, None));
    assert!(p.b.link.remote.is_empty());
    assert_eq!(free(&p.a), 4);
    assert_eq!(p.a.link.free_windows.len(), GRANTS as usize);
    // Bound, but the Grant waits for credits past its deadline.
    let saved = p.a.link.credits;
    p.a.link.credits = 1;
    let late = grant(&mut p.a, 10, now + SECOND, now);
    assert_eq!(p.a.link.poll(&p.a.pool, now), Ok(()));
    assert_eq!(p.a.link.poll(&p.a.pool, now), Ok(()));
    assert!(matches!(
        p.a.link.grants.values().next().unwrap().state,
        GrantState::Bound
    ));
    p.now += SECOND;
    p.a.link.credits = saved;
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(
        poll(&late).map(|r| r.err()),
        Some(Some(Error::DeadlineExceeded))
    );
    assert!(p.b.link.remote.is_empty());
    assert_eq!(free(&p.a), 4);
    finish(p);
}

#[test]
/// Data waits while credits are short and goes out once the peer returns
/// them.
fn credit_exhaustion_waits() {
    let mut p = pair();
    let now = p.now;
    // Pretend a's last sends consumed all but the reserved credit.
    let used = p.a.link.credits - 1;
    p.a.link.credits = 1;
    p.a.link.unreturned += used;
    p.b.link.owed += used;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(p.a.link.poll(&p.a.pool, now), Ok(()));
    assert_eq!(p.a.link.poll(&p.a.pool, now), Ok(()));
    assert!(poll(&granted).is_none(), "Grant needs two credits");
    assert_eq!(pump(&mut p), (None, None));
    assert!(
        poll(&granted).unwrap().is_ok(),
        "b's standalone Credit unblocked it"
    );
    finish(p);
}

#[test]
/// Grant requests are validated and bounded.
fn grant_and_write_requests_are_validated() {
    let mut p = pair();
    let now = p.now;
    let deadline = now + 5 * SECOND;
    assert_eq!(
        poll(&grant(&mut p.a, 0, deadline, now)).map(|r| r.err()),
        Some(Some(Error::InvalidRange))
    );
    assert_eq!(
        poll(&grant(&mut p.a, LEN as u32 + 1, deadline, now)).map(|r| r.err()),
        Some(Some(Error::InvalidRange))
    );
    let first = grant(&mut p.a, 10, deadline, now);
    let second = grant(&mut p.a, 10, deadline, now);
    assert_eq!(
        poll(&grant(&mut p.a, 10, deadline, now)).map(|r| r.err()),
        Some(Some(Error::Overloaded))
    );
    assert_eq!(pump(&mut p), (None, None));
    let (first, second) = (
        poll(&first).unwrap().unwrap(),
        poll(&second).unwrap().unwrap(),
    );
    assert_eq!(
        poll(&write(&mut p.b, first.tag, vec![], deadline, now)),
        Some(Err(Error::InvalidRange))
    );
    assert_eq!(
        poll(&write(&mut p.b, first.tag, vec![1; LEN + 1], deadline, now)),
        Some(Err(Error::InvalidRange))
    );
    assert_eq!(
        poll(&write(&mut p.b, first.tag, vec![1; 11], deadline, now)),
        Some(Err(Error::InvalidRange)),
        "longer than granted"
    );
    // A staged buffer from the link's own pool works.
    let mut staged = p.b.pool.shared().stage(4).unwrap();
    staged.bytes_mut().copy_from_slice(b"page");
    let (written, done) = op();
    p.b.link.write(
        &p.b.pool,
        first.tag,
        Source::Staged(staged),
        deadline,
        done,
        now,
    );
    // A staged buffer from another pool is refused.
    let foreign = p.a.pool.shared().stage(4).unwrap();
    let (refused, done) = op();
    p.b.link.write(
        &p.b.pool,
        second.tag,
        Source::Staged(foreign),
        deadline,
        done,
        now,
    );
    assert_eq!(poll(&refused), Some(Err(Error::InvalidRequest)));
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    let Some(Ok(Arrival::Page(page))) = poll(&first.arrival) else {
        panic!("no page");
    };
    assert_eq!(page.bytes(), b"page");
    drop(page);
    assert_eq!(poll(&release(&mut p.b, second.tag, now)), None);
    assert_eq!(pump(&mut p), (None, None));
    // Not ready: everything is unavailable.
    p.a.link.phase = Phase::Created;
    assert_eq!(
        poll(&grant(&mut p.a, 1, deadline, now)).map(|r| r.err()),
        Some(Some(Error::Unavailable))
    );
    assert_eq!(
        poll(&write(&mut p.a, 1, vec![1], deadline, now)),
        Some(Err(Error::Unavailable))
    );
    assert_eq!(
        poll(&release(&mut p.a, 1, now)),
        Some(Err(Error::Unavailable))
    );
    p.a.link.phase = Phase::Ready;
    finish(p);
}

#[test]
/// Unequal grant limits: each side holds at most the smaller limit, so the
/// side configured for more grants never breaks the other. Covers the larger
/// limit on the initiator and on the responder.
fn unequal_grant_limits_use_the_minimum() {
    for (a_grants, b_grants) in [(4, 2), (2, 4)] {
        let sim = Simulation::new();
        let now = Instant::now();
        let a = side_with(&sim, 1, Role::Initiator, 4, a_grants, now);
        let b = side_with(&sim, 2, Role::Responder, 4, b_grants, now);
        let mut p = Pair { sim, a, b, now };
        p.b.link
            .connect(
                p.a.link.endpoint(),
                p.a.link.recv_credits(),
                p.a.link.grant_limit(),
                ID,
                now,
            )
            .unwrap();
        p.a.link
            .connect(
                p.b.link.endpoint(),
                p.b.link.recv_credits(),
                p.b.link.grant_limit(),
                ID,
                now,
            )
            .unwrap();
        assert_eq!(pump(&mut p), (None, None));
        assert_eq!(p.b.link.phase(), Phase::Ready);
        let deadline = now + 5 * SECOND;

        for to_a in [true, false] {
            let receiver = if to_a { &mut p.a } else { &mut p.b };
            let held: Vec<_> = (0..2).map(|_| grant(receiver, 1, deadline, now)).collect();
            assert_eq!(
                poll(&grant(receiver, 1, deadline, now)).map(|r| r.err()),
                Some(Some(Error::Overloaded)),
                "third grant over the peer's limit ({a_grants}, {b_grants}, {to_a})"
            );
            assert_eq!(pump(&mut p), (None, None), "link stays up");

            for granted in held {
                let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
                let sender = if to_a { &mut p.b } else { &mut p.a };
                let written = write(sender, tag, vec![9], deadline, now);
                assert_eq!(pump(&mut p), (None, None));
                assert_eq!(poll(&written), Some(Ok(())));
                assert!(matches!(poll(&arrival), Some(Ok(Arrival::Page(_)))));
            }
        }

        assert_eq!(p.a.link.phase(), Phase::Ready);
        assert_eq!(p.b.link.phase(), Phase::Ready);
        finish(p);
    }
}

#[test]
/// A peer grant limit of zero is refused at connect.
fn zero_peer_grant_limit_is_refused() {
    let sim = Simulation::new();
    let now = Instant::now();
    let mut a = side(&sim, 1, Role::Initiator, 1, now);
    let b = side(&sim, 2, Role::Responder, 1, now);
    assert_eq!(
        a.link
            .connect(b.link.endpoint(), b.link.recv_credits(), 0, ID, now),
        Err(Error::InvalidRequest)
    );
}

#[test]
/// With every page buffer in use, grants and byte writes are overloaded.
fn pool_exhaustion_is_overloaded() {
    let mut p = pair();
    let now = p.now;
    let deadline = now + 5 * SECOND;
    let held: Vec<_> = (0..4)
        .map(|_| p.a.pool.shared().stage(1).unwrap())
        .collect();
    assert_eq!(
        poll(&grant(&mut p.a, 1, deadline, now)).map(|r| r.err()),
        Some(Some(Error::Overloaded))
    );
    let granted = grant(&mut p.b, 1, deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    assert_eq!(
        poll(&write(&mut p.a, granted.tag, vec![1], deadline, now)),
        Some(Err(Error::Overloaded))
    );
    drop(held);
    let written = write(&mut p.a, granted.tag, vec![1], deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    assert!(matches!(poll(&granted.arrival), Some(Ok(Arrival::Page(_)))));
    finish(p);
}

/// Queue a raw control message from `side`.
fn raw(side: &mut Side, kind: ControlKind, credits: u32) {
    let link_id = side.link.link_id;
    side.link.outbox.push_back(Outgoing::Raw(ControlMessage {
        credits,
        link_id,
        kind,
    }));
}

fn forged_grant(tag: u32) -> ControlKind {
    ControlKind::Grant {
        tag,
        rkey: 1,
        addr: 0,
        length: 1,
        transfer_id: [0; 16],
    }
}

#[test]
/// Protocol violations from the peer break the link.
fn peer_protocol_violations_break() {
    type Forge = fn(&mut Side);
    let cases: [(&str, Forge); 6] = [
        ("credit overrun", |b| raw(b, ControlKind::Credit, 1000)),
        ("duplicate grant tag", |b| {
            raw(b, forged_grant(9), 0);
            raw(b, forged_grant(9), 0);
        }),
        ("grant cap exceeded", |b| {
            for tag in 1..=GRANTS + 1 {
                raw(b, forged_grant(tag), 0);
            }
        }),
        ("release of unknown tag", |b| {
            raw(
                b,
                ControlKind::Release {
                    tag: 3,
                    transfer_id: [7; 16],
                },
                0,
            )
        }),
        ("wrong link id", |b| b.link.link_id ^= 1),
        ("bad message", |b| raw(b, ControlKind::Credit, 0)),
    ];
    for (name, forge) in cases {
        let mut p = pair();
        forge(&mut p.b);
        if name == "wrong link id" || name == "bad message" {
            raw(&mut p.b, ControlKind::Credit, 0);
        }
        if name == "bad message" {
            // Corrupt the payload length on the wire.
            p.sim.fault_on(
                Operation::Send,
                Some(p.b.link.endpoint().qpn),
                Fault::ByteLength(63),
            );
        }
        let (a, _) = pump(&mut p);
        assert_eq!(a, Some(Break::Error), "{name}");
        finish(p);
    }
}

#[test]
/// A release whose transfer id does not match the grant breaks the link.
fn release_with_wrong_transfer_breaks() {
    let mut p = pair();
    let now = p.now;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    p.b.link.remote.get_mut(&granted.tag).unwrap().transfer = [9; 16];
    let _released = release(&mut p.b, granted.tag, now);
    assert_eq!(pump(&mut p).0, Some(Break::Error));
    finish(p);
}

#[test]
/// A write with immediate for a tag we never granted breaks the link.
fn immediate_for_unknown_tag_breaks() {
    let mut p = pair();
    let now = p.now;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    let remote = p.b.link.remote.remove(&granted.tag).unwrap();
    p.b.link.remote.insert(granted.tag + 5, remote);
    let _written = write(
        &mut p.b,
        granted.tag + 5,
        vec![1; 10],
        now + 5 * SECOND,
        now,
    );
    assert_eq!(pump(&mut p).0, Some(Break::Error));
    finish(p);
}

#[test]
/// A duplicated immediate or a corrupted length breaks the receiver.
fn duplicate_immediate_and_bad_length_break() {
    for fault in [
        Fault::DuplicateImmediate,
        Fault::ByteLength(11),
        Fault::ByteLength(0),
    ] {
        let mut p = pair();
        let now = p.now;
        let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
        assert_eq!(pump(&mut p), (None, None));
        let granted = poll(&granted).unwrap().unwrap();
        p.sim
            .fault_on(Operation::WriteImm, Some(p.b.link.endpoint().qpn), fault);
        let _written = write(&mut p.b, granted.tag, vec![1; 10], now + 5 * SECOND, now);
        assert_eq!(pump(&mut p).0, Some(Break::Error), "{fault:?}");
        assert!(p.a.link.destroy(&p.a.pool));
        assert!(matches!(
            poll(&granted.arrival),
            Some(Err(Error::Unavailable)) | None
        ));
        finish(p);
    }
}

#[test]
/// A dropped write leaves the grant exposed until its deadline breaks it.
fn dropped_write_breaks_at_grant_deadline() {
    let mut p = pair();
    let now = p.now;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    p.sim.fault_on(
        Operation::WriteImm,
        Some(p.b.link.endpoint().qpn),
        Fault::Drop,
    );
    let written = write(&mut p.b, granted.tag, vec![1; 10], now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    assert_eq!(poll(&granted.arrival).map(|r| r.is_ok()), None);
    p.now += 5 * SECOND;
    assert_eq!(pump(&mut p).0, Some(Break::Error));
    assert!(p.a.link.destroy(&p.a.pool));
    assert!(matches!(
        poll(&granted.arrival),
        Some(Err(Error::Unavailable))
    ));
    finish(p);
}

#[test]
/// A late write through an invalidated grant is refused by the NIC and
/// breaks both sides; the received page is unchanged.
fn late_write_after_invalidate_is_refused() {
    let mut p = pair();
    let now = p.now;
    let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    let granted = poll(&granted).unwrap().unwrap();
    let stale = p.b.link.remote[&granted.tag];
    let written = write(&mut p.b, granted.tag, vec![1; 10], now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    let Some(Ok(Arrival::Page(page))) = poll(&granted.arrival) else {
        panic!("no page");
    };
    p.b.link.remote.insert(granted.tag, stale);
    let late = write(&mut p.b, granted.tag, vec![2; 10], now + 5 * SECOND, now);
    assert_eq!(pump(&mut p), (Some(Break::Error), Some(Break::Error)));
    assert_eq!(p.b.link.failure().unwrap().status, status::REM_ACCESS_ERR);
    assert_eq!(page.bytes(), [1; 10]);
    assert!(p.b.link.destroy(&p.b.pool));
    assert_eq!(poll(&late), Some(Err(Error::Unavailable)));
    drop(page);
    finish(p);
}

#[test]
/// An error completion on any work request kind breaks the link and fails
/// its ops.
fn error_completion_on_each_kind_breaks() {
    for (operation, on_a) in [
        (Operation::Bind, true),
        (Operation::Send, true),
        (Operation::WriteImm, false),
        (Operation::Invalidate, true),
    ] {
        let mut p = pair();
        let now = p.now;
        let qpn = if on_a {
            p.a.link.endpoint().qpn
        } else {
            p.b.link.endpoint().qpn
        };
        p.sim.fault_on(
            operation,
            Some(qpn),
            Fault::Completion(status::REM_ACCESS_ERR),
        );
        let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
        let (a, b) = pump(&mut p);
        let tag = match poll(&granted) {
            Some(Ok(granted)) => Some(granted),
            _ => None,
        };
        let written = tag
            .as_ref()
            .map(|g| write(&mut p.b, g.tag, vec![1; 10], now + 5 * SECOND, now));
        let (a2, b2) = pump(&mut p);
        let broken = if on_a { a.or(a2) } else { b.or(b2) };
        assert_eq!(broken, Some(Break::Error), "{operation:?}");
        assert!(p.a.link.destroy(&p.a.pool) && p.b.link.destroy(&p.b.pool));
        if let Some(written) = written {
            assert!(matches!(poll(&written), Some(Err(_)) | Some(Ok(())) | None));
        }
        if let Some(granted) = tag {
            assert!(!matches!(
                poll(&granted.arrival),
                Some(Ok(Arrival::Page(_)))
            ));
        }
        finish(p);
    }
}

#[test]
/// Sending past the peer's posted receives is RNR and breaks both sides.
fn rnr_breaks_both_sides() {
    let mut p = pair();
    p.b.link.credits = 1000;
    for _ in 0..p.a.link.recv_depth + 2 {
        raw(&mut p.b, ControlKind::Credit, 0);
    }
    let polls = (0..4)
        .map(|_| p.b.link.poll(&p.b.pool, p.now))
        .collect::<Vec<_>>();
    assert!(polls.contains(&Err(Break::Error)), "{polls:?}");
    assert_eq!(
        p.b.link.failure().unwrap().status,
        status::RNR_RETRY_EXC_ERR
    );
    finish(p);
}

#[test]
/// `Close` breaks both sides: ours as closed, the peer's as peer closed.
fn close_breaks_both_sides() {
    let mut p = pair();
    p.a.link.close(p.now, CLOSE_GRACE);
    assert!(matches!(p.a.link.phase(), Phase::Closing { .. }));
    let now = p.now;
    assert_eq!(
        poll(&grant(&mut p.a, 1, now + SECOND, now)).map(|r| r.err()),
        Some(Some(Error::Unavailable))
    );
    assert_eq!(pump(&mut p), (Some(Break::Closed), Some(Break::PeerClosed)));
    finish(p);
}

#[test]
/// An unsent `Close` breaks after the grace period anyway.
fn close_without_credits_breaks_after_grace() {
    let mut p = pair();
    p.a.link.credits = 0;
    p.a.link.close(p.now, CLOSE_GRACE);
    assert_eq!(p.a.link.poll(&p.a.pool, p.now), Ok(()));
    assert_eq!(
        p.a.link.poll(&p.a.pool, p.now + CLOSE_GRACE),
        Err(Break::Closed)
    );
    finish(p);
}

#[test]
/// A ready link with no traffic for the idle time closes itself.
fn idle_link_closes() {
    let mut p = pair();
    p.now += Duration::from_secs(120);
    assert_eq!(pump(&mut p), (Some(Break::Closed), Some(Break::PeerClosed)));
    finish(p);
}

#[test]
/// A link not ready within the setup timeout breaks, in either role.
fn setup_timeout() {
    let sim = Simulation::new();
    let now = Instant::now();
    let mut a = side(&sim, 1, Role::Initiator, 1, now);
    let mut b = side(&sim, 2, Role::Responder, 1, now);
    b.link
        .connect(
            a.link.endpoint(),
            a.link.recv_credits(),
            a.link.grant_limit(),
            ID,
            now,
        )
        .unwrap();
    assert_eq!(
        b.link.connect(
            a.link.endpoint(),
            a.link.recv_credits(),
            a.link.grant_limit(),
            ID,
            now
        ),
        Err(Error::InvalidRequest),
        "connect once"
    );
    assert_eq!(
        a.link.connect(b.link.endpoint(), 1, 1, ID, now),
        Err(Error::InvalidRequest),
        "too few credits"
    );
    let later = now + SETUP_TIMEOUT - Duration::from_millis(1);
    assert_eq!(a.link.poll(&a.pool, later), Ok(()));
    assert_eq!(b.link.poll(&b.pool, later), Ok(()));
    assert_eq!(
        a.link.poll(&a.pool, now + SETUP_TIMEOUT),
        Err(Break::Timeout)
    );
    assert_eq!(
        b.link.poll(&b.pool, now + SETUP_TIMEOUT),
        Err(Break::Timeout)
    );
    assert!(a.link.destroy(&a.pool) && b.link.destroy(&b.pool));
    drop((a, b));
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// A failed destroy keeps buffers quarantined and retries; a final leak
/// marks them leaked and keeps the charge.
fn failed_destroy_quarantines_then_frees_or_leaks() {
    for leak in [false, true] {
        let mut p = pair();
        let now = p.now;
        let granted = grant(&mut p.a, 10, now + 5 * SECOND, now);
        assert_eq!(pump(&mut p), (None, None));
        let granted = poll(&granted).unwrap().unwrap();
        let qpn = p.a.link.endpoint().qpn;
        p.sim.reject(Operation::Stop, Some(qpn), true);
        assert!(!p.a.link.destroy(&p.a.pool));
        assert_eq!(
            poll(&granted.arrival).map(|r| r.err()),
            Some(Some(Error::Unavailable))
        );
        assert_eq!(p.a.pool.shared().count_in(BufferState::Quarantined), 1);
        if leak {
            p.a.link.leak(&p.a.pool);
            assert_eq!(p.a.pool.shared().count_in(BufferState::Leaked), 1);
            assert!(!p.a.pool.idle());
            let quota = p.a.quota.clone();
            drop(p);
            assert!(quota.quarantined());
            continue;
        }
        assert!(!p.a.link.destroy(&p.a.pool));
        p.sim.reject(Operation::Stop, Some(qpn), false);
        assert!(p.a.link.destroy(&p.a.pool));
        assert_eq!(free(&p.a), 4);
        let _ = &p.a.device;
        finish(p);
    }
}

/// Give `side` an implicit on-demand paging region: zero-copy writes.
fn odp(side: &mut Side) {
    let implicit = NativeRegion::implicit(side.device.clone(), side.quota.clone()).unwrap();
    side.pool.set_implicit(implicit);
    assert!(side.pool.shared().zero_copy());
}

/// Grant `len` bytes on `a`, then write `source` from `b` (zero-copy), with
/// the write held in flight by a delay. Returns the write and arrival ops.
fn direct_write(p: &mut Pair, source: &Arc<Vec<u8>>) -> (Op<()>, Op<Arrival>) {
    odp(&mut p.b);
    let (now, deadline) = (p.now, p.now + 5 * SECOND);
    let granted = grant(&mut p.a, source.len() as u32, deadline, now);
    assert_eq!(pump(p), (None, None));
    let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
    p.sim.fault(Operation::WriteImm, Fault::Delay(1_000_000));
    let (written, done) = op();
    p.b.link.write(
        &p.b.pool,
        tag,
        Source::Bytes(source.clone()),
        deadline,
        done,
        now,
    );
    assert_eq!(pump(p), (None, None));
    assert_eq!(poll(&written), None, "held in flight");
    (written, arrival)
}

#[test]
/// A zero-copy write takes no pool buffer and holds its source until the
/// completion, then lets go.
fn zero_copy_write_reads_in_place_and_holds_the_source_until_completion() {
    let mut p = pair();
    odp(&mut p.b);
    let source = Arc::new(vec![0x6b; 100]);
    let (now, deadline) = (p.now, p.now + 5 * SECOND);
    let granted = grant(&mut p.a, 100, deadline, now);
    assert_eq!(pump(&mut p), (None, None));
    let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
    p.sim.fault(Operation::WriteImm, Fault::Delay(3));
    let (written, done) = op();
    p.b.link.write(
        &p.b.pool,
        tag,
        Source::Bytes(source.clone()),
        deadline,
        done,
        now,
    );
    // Queued and posted: the owner holds the source, and no buffer is staged.
    p.b.link.poll(&p.b.pool, now).unwrap();
    assert_eq!(Arc::strong_count(&source), 2);
    assert_eq!(free(&p.b), 4, "no send buffer");
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Ok(())));
    assert_eq!(Arc::strong_count(&source), 1, "released on completion");
    match poll(&arrival).unwrap().unwrap() {
        Arrival::Page(page) => assert_eq!(page.bytes(), &source[..]),
        Arrival::Released => panic!("expected a page"),
    }
    finish(p);
}

#[test]
/// Any `DirectBytes` source goes out in place, and the receiver gets exactly
/// the slice the source lends.
fn zero_copy_writes_accept_boxed_and_static_sources() {
    static BYTES: [u8; 48] = [0x5a; 48];
    let sources: [Arc<dyn DirectBytes>; 2] = [
        Arc::new(vec![0x2e; 80].into_boxed_slice()),
        Arc::new(&BYTES[..]),
    ];
    let mut p = pair();
    odp(&mut p.b);
    for source in sources {
        let expected = source.bytes().to_vec();
        let (now, deadline) = (p.now, p.now + 5 * SECOND);
        let granted = grant(&mut p.a, expected.len() as u32, deadline, now);
        assert_eq!(pump(&mut p), (None, None));
        let Granted { tag, arrival } = poll(&granted).unwrap().unwrap();
        let (written, done) = op();
        p.b.link.write(
            &p.b.pool,
            tag,
            Source::Bytes(source.clone()),
            deadline,
            done,
            now,
        );
        assert_eq!(pump(&mut p), (None, None));
        assert_eq!(poll(&written), Some(Ok(())));
        assert_eq!(free(&p.b), 4, "no send buffer");
        assert_eq!(Arc::strong_count(&source), 1, "released on completion");
        match poll(&arrival).unwrap().unwrap() {
            Arrival::Page(page) => assert_eq!(page.bytes(), &expected[..]),
            Arrival::Released => panic!("expected a page"),
        }
    }
    finish(p);
}

#[test]
/// A broken link holds an in-flight zero-copy source until the QP is
/// destroyed, retrying a failed destroy; a leak never frees it.
fn zero_copy_source_outlives_the_qp_or_leaks() {
    for leak in [false, true] {
        let mut p = pair();
        let source = Arc::new(vec![0x3c; 64]);
        let (written, _arrival) = direct_write(&mut p, &source);
        assert_eq!(Arc::strong_count(&source), 2);
        let qpn = p.b.link.endpoint().qpn;
        p.sim.reject(Operation::Stop, Some(qpn), true);
        assert!(!p.b.link.destroy(&p.b.pool));
        assert_eq!(poll(&written), Some(Err(Error::Unavailable)));
        assert_eq!(
            Arc::strong_count(&source),
            2,
            "the NIC may still read it before the QP is gone"
        );
        if leak {
            p.b.link.leak(&p.b.pool);
            drop(p);
            assert_eq!(Arc::strong_count(&source), 2, "leaked, never freed");
            continue;
        }
        p.sim.reject(Operation::Stop, Some(qpn), false);
        assert!(p.b.link.destroy(&p.b.pool));
        assert_eq!(Arc::strong_count(&source), 1);
        assert!(p.a.link.destroy(&p.a.pool));
    }
}

#[test]
/// A write still waiting in the outbox at its deadline drops its zero-copy
/// source without a fence: it never reached the NIC.
fn unposted_zero_copy_write_expires_and_drops_its_source() {
    let mut p = pair();
    odp(&mut p.b);
    let source = Arc::new(vec![1; 16]);
    let now = p.now;
    let (written, done) = op();
    // No grant yet: the write waits, then its deadline passes.
    p.b.link.write(
        &p.b.pool,
        9,
        Source::Bytes(source.clone()),
        now + SECOND,
        done,
        now,
    );
    assert_eq!(Arc::strong_count(&source), 2);
    p.now += 2 * SECOND;
    assert_eq!(pump(&mut p), (None, None));
    assert_eq!(poll(&written), Some(Err(Error::DeadlineExceeded)));
    assert_eq!(Arc::strong_count(&source), 1);
    finish(p);
}

#[test]
/// Without implicit ODP no region is made and writes copy into a buffer;
/// the implicit region is never a receive target or window.
fn implicit_region_is_optional_and_send_only() {
    let p = pair();
    p.sim.implicit_odp(false);
    assert!(NativeRegion::implicit(p.b.device.clone(), p.b.quota.clone()).is_none());
    assert!(!p.b.pool.shared().zero_copy());
    p.sim.implicit_odp(true);
    let implicit = NativeRegion::implicit(p.b.device.clone(), p.b.quota.clone()).unwrap();
    assert!(implicit.is_implicit());
    let qp = NativeQueuePair::new_with_depth(
        p.b.device.clone(),
        p.b.quota.clone(),
        Arc::new(AtomicBool::new(false)),
        4,
        4,
    )
    .unwrap();
    let bytes = [0u8; 8];
    let id = (1 << 56) | 1;
    assert_eq!(
        qp.post_recv(id, &implicit, bytes.as_ptr() as u64, 8),
        Err(Error::InvalidRequest)
    );
    let control =
        NativeRegion::with_access(p.b.device.clone(), 64, Access::CONTROL, p.b.quota.clone())
            .unwrap();
    assert_eq!(
        qp.post_recv(id, &control, 0, 8),
        Ok(()),
        "same post, normal region"
    );
    assert!(qp.stop().is_ok());
    drop(qp);
    drop(control);
    drop(implicit);
    finish(p);
}
