//! Reactor-level simulation tests: `uring_runtime::Reactor<SimBackend>`
//! driven end to end against a deterministic [`World`].

use std::cell::{Cell, RefCell};
use std::ffi::CString;
use std::future::Future;
use std::pin::{Pin, pin};
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::Duration;

use uring_runtime::backend::{OpKind, OpenHow};
use uring_runtime::reactor::{Reactor, ReactorConfig, RejectionReason};
use uring_runtime::{CancelToken, Clock, ErrorKind, Unscoped};
use uring_runtime_sim::{
    DEFAULT_ENTROPY_DOMAIN, Fault, FaultTarget, SimBackend, SimClock, SimEntropy, World,
};

/// The most reactor turns a single test step may take.
const MAX_TURNS: usize = 200;

fn reactor_with(world: &World, clock: &SimClock, config: ReactorConfig) -> Reactor<SimBackend> {
    let backend = SimBackend::new(
        world,
        clock.clone(),
        SimEntropy::new(DEFAULT_ENTROPY_DOMAIN, clock.seed(), 0),
    );
    Reactor::new(backend, config).unwrap()
}

fn setup() -> (World, SimClock, Reactor<SimBackend>) {
    let world = World::new();
    let clock = SimClock::new(7);
    let reactor = reactor_with(&world, &clock, ReactorConfig::default());
    (world, clock, reactor)
}

/// One reactor turn: reap completions, then let the backend run.
fn turn(reactor: &Reactor<SimBackend>) {
    reactor.poll(64).unwrap();
    reactor.wait(Duration::from_secs(1)).unwrap();
}

/// Poll `future` once with a no-op waker.
fn poll_once<F: Future + ?Sized>(future: Pin<&mut F>) -> Poll<F::Output> {
    future.poll(&mut Context::from_waker(Waker::noop()))
}

/// Drive `future` to completion, turning the reactor between polls.
fn run<F: Future>(reactor: &Reactor<SimBackend>, future: F) -> F::Output {
    let mut future = pin!(future);
    for _ in 0..MAX_TURNS {
        if let Poll::Ready(output) = poll_once(future.as_mut()) {
            return output;
        }
        turn(reactor);
    }
    panic!("operation did not complete within {MAX_TURNS} turns");
}

/// Turn the reactor until nothing is in flight.
fn settle(reactor: &Reactor<SimBackend>) {
    for _ in 0..MAX_TURNS {
        if reactor.in_flight() == 0 {
            return;
        }
        turn(reactor);
    }
    panic!("reactor did not settle: {} in flight", reactor.in_flight());
}

fn how(flags: i32, mode: u64) -> OpenHow {
    OpenHow {
        flags: flags as u64,
        mode,
        resolve: 0,
    }
}

#[test]
fn recv_and_send_over_a_socket_pair() {
    let (world, _clock, reactor) = setup();
    let (a, b) = world.socket_pair().unwrap();

    let sent = run(&reactor, reactor.send(&a, b"hello".to_vec(), &Unscoped));
    assert_eq!(sent.result.unwrap(), 5);
    assert_eq!(sent.buf, b"hello");

    let received = run(
        &reactor,
        reactor.recv(&b, Vec::with_capacity(16), &Unscoped),
    );
    assert_eq!(received.result.unwrap(), 5);
    assert_eq!(received.buf, b"hello");
}

#[test]
fn recv_waits_for_data_sent_later() {
    let (world, _clock, reactor) = setup();
    let (a, b) = world.socket_pair().unwrap();
    let mut recv = pin!(reactor.recv(&b, Vec::with_capacity(8), &Unscoped));
    for _ in 0..3 {
        assert!(poll_once(recv.as_mut()).is_pending());
        turn(&reactor);
    }
    assert_eq!(a.send(b"late").unwrap(), 4);
    let received = run(&reactor, recv);
    assert_eq!(received.result.unwrap(), 4);
    assert_eq!(received.buf, b"late");
}

#[test]
fn listen_connect_accept_through_reactor() {
    let (world, _clock, reactor) = setup();
    let listener = world
        .listen("10.0.0.1:80".parse::<std::net::SocketAddr>().unwrap())
        .unwrap();
    let address = listener.local_addr().unwrap();
    let before = world.live_handles();

    let client = world.socket(libc::AF_INET, libc::SOCK_STREAM).unwrap();
    let mut accept = pin!(reactor.accept(&listener, &Unscoped));
    assert!(poll_once(accept.as_mut()).is_pending());
    run(&reactor, reactor.connect(&client, &address, &Unscoped)).unwrap();
    let (accepted, _peer) = run(&reactor, accept).unwrap();

    run(&reactor, reactor.send(&client, b"ping".to_vec(), &Unscoped))
        .result
        .unwrap();
    let got = run(
        &reactor,
        reactor.recv(&accepted, Vec::with_capacity(4), &Unscoped),
    );
    assert_eq!(got.buf, b"ping");

    drop(accepted);
    drop(client);
    assert_eq!(world.live_handles(), before);
}

#[test]
fn accepted_descriptor_is_closed_when_the_future_is_dropped() {
    let (world, _clock, reactor) = setup();
    let listener = world
        .listen("10.0.0.1:81".parse::<std::net::SocketAddr>().unwrap())
        .unwrap();
    let address = listener.local_addr().unwrap();
    let before = world.live_handles();
    {
        let mut accept = pin!(reactor.accept(&listener, &Unscoped));
        assert!(poll_once(accept.as_mut()).is_pending());
        let client = world.connect(address).unwrap();
        // Let the accept complete inside the reactor without collecting it.
        for _ in 0..3 {
            turn(&reactor);
        }
        assert_eq!(reactor.in_flight(), 0, "the accept is fenced");
        drop(client);
    }
    // The adopted descriptor was owned by the operation node and closed with it.
    assert_eq!(world.live_handles(), before);
}

#[test]
fn files_open_write_read_through_reactor() {
    let (world, _clock, reactor) = setup();
    world.create_dir_all("/data").unwrap();
    let path = CString::new("/data/file").unwrap();
    let file = run(
        &reactor,
        reactor.open_at2(
            None,
            &path,
            how(libc::O_CREAT | libc::O_RDWR, 0o600),
            &Unscoped,
        ),
    )
    .unwrap();
    let wrote = run(
        &reactor,
        reactor.write_at(&file, 3, b"abc".to_vec(), &Unscoped),
    );
    assert_eq!(wrote.result.unwrap(), 3);
    let read = run(
        &reactor,
        reactor.read_at(&file, 0, Vec::with_capacity(16), &Unscoped),
    );
    assert_eq!(read.result.unwrap(), 6);
    assert_eq!(read.buf, b"\0\0\0abc");
    assert_eq!(world.read_file("/data/file").unwrap(), b"\0\0\0abc");

    let missing = CString::new("/data/missing").unwrap();
    let error = run(
        &reactor,
        reactor.open_at2(None, &missing, how(libc::O_RDONLY, 0), &Unscoped),
    )
    .unwrap_err();
    assert_eq!(error.raw_os_error(), Some(libc::ENOENT));
}

#[test]
fn vectored_writes_gather_segments_and_honor_short_and_error_faults() {
    let (world, _clock, reactor) = setup();
    world.create_dir_all("/data").unwrap();
    let path = CString::new("/data/file").unwrap();
    let file = run(
        &reactor,
        reactor.open_at2(
            None,
            &path,
            how(libc::O_CREAT | libc::O_RDWR, 0o600),
            &Unscoped,
        ),
    )
    .unwrap();
    let wrote = run(
        &reactor,
        reactor.writev_at(&file, 2, (b"hd".to_vec(), b"body".to_vec()), &Unscoped),
    );
    assert_eq!(wrote.result.unwrap(), 6);
    assert_eq!(wrote.buf, (b"hd".to_vec(), b"body".to_vec()));
    assert_eq!(world.read_file("/data/file").unwrap(), b"\0\0hdbody");

    // A short write spans the segment boundary, like the kernel.
    world
        .inject(FaultTarget::Op(OpKind::Writev), Fault::Short(3))
        .unwrap();
    let wrote = run(
        &reactor,
        reactor.writev_at(&file, 0, (b"XY".to_vec(), b"Z!".to_vec()), &Unscoped),
    );
    assert_eq!(wrote.result.unwrap(), 3, "the reactor never retries");
    assert_eq!(world.read_file("/data/file").unwrap(), b"XYZdbody");

    world
        .inject(FaultTarget::Op(OpKind::Writev), Fault::Errno(libc::EIO))
        .unwrap();
    let failed = run(
        &reactor,
        reactor.writev_at(&file, 0, vec![b"q".to_vec()], &Unscoped),
    );
    assert_eq!(failed.result.unwrap_err().raw_os_error(), Some(libc::EIO));
    assert_eq!(failed.buf, [b"q".to_vec()]);
    assert_eq!(world.read_file("/data/file").unwrap(), b"XYZdbody");
}

#[test]
fn cancellation_of_a_blocked_recv_in_both_completion_orders() {
    for cancel_first in [true, false] {
        let (world, _clock, reactor) = setup();
        world.set_cancel_first(cancel_first);
        let (_a, b) = world.socket_pair().unwrap();
        let token = CancelToken::new();
        let mut recv = pin!(reactor.recv(&b, Vec::with_capacity(4), &token));
        assert!(poll_once(recv.as_mut()).is_pending());
        turn(&reactor);
        token.cancel();
        let out = run(&reactor, recv);
        assert_eq!(out.result.unwrap_err().kind(), ErrorKind::Canceled);
        assert_eq!(out.buf.capacity(), 4, "the buffer is returned");
        assert_eq!(reactor.in_flight(), 0, "cancel_first={cancel_first}");
    }
}

/// Cancel a recv whose data already arrived but whose completion is held:
/// the backend cannot cancel it, so the kernel result decides.
fn held_recv_after_cancel(cancel_wins: bool) -> (uring_runtime::Result<usize>, Vec<u8>) {
    let world = World::new();
    let clock = SimClock::new(1);
    let reactor = reactor_with(
        &world,
        &clock,
        ReactorConfig::default().with_cancel_wins(cancel_wins),
    );
    let (a, b) = world.socket_pair().unwrap();
    a.send(b"data").unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Recv), Fault::HoldCompletion(3))
        .unwrap();
    let token = CancelToken::new();
    let mut recv = pin!(reactor.recv(&b, Vec::with_capacity(4), &token));
    assert!(poll_once(recv.as_mut()).is_pending());
    turn(&reactor);
    token.cancel();
    let out = run(&reactor, recv);
    (out.result, out.buf)
}

#[test]
fn kernel_result_wins_over_cancellation_by_default() {
    let (result, buf) = held_recv_after_cancel(false);
    assert_eq!(result.unwrap(), 4);
    assert_eq!(buf, b"data", "received bytes are never silently lost");
}

#[test]
fn cancel_wins_discards_the_kernel_result() {
    let (result, _buf) = held_recv_after_cancel(true);
    assert_eq!(result.unwrap_err().kind(), ErrorKind::Canceled);
}

#[test]
fn dropped_operation_keeps_its_lease_until_the_held_completion_arrives() {
    let (world, _clock, reactor) = setup();
    let (a, b) = world.socket_pair().unwrap();
    a.send(b"x").unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Recv), Fault::HoldCompletion(4))
        .unwrap();
    let lease = Rc::new(());
    {
        let mut recv = pin!(
            reactor
                .recv(&b, Vec::with_capacity(1), &Unscoped)
                .lease(Rc::clone(&lease))
        );
        assert!(poll_once(recv.as_mut()).is_pending());
        turn(&reactor);
    }
    // The future is gone, but the backend still owns the buffer.
    assert_eq!(Rc::strong_count(&lease), 2, "lease held until the fence");
    assert_eq!(reactor.in_flight(), 1);
    settle(&reactor);
    assert_eq!(Rc::strong_count(&lease), 1, "lease released at the fence");
}

#[test]
fn drain_fences_dropped_and_pending_operations() {
    let (world, _clock, reactor) = setup();
    let (_a, b) = world.socket_pair().unwrap();
    let mut recvs: Vec<_> = (0..4)
        .map(|_| Box::pin(reactor.recv(&b, Vec::with_capacity(1), &Unscoped)))
        .collect();
    for recv in &mut recvs {
        assert!(poll_once(recv.as_mut()).is_pending());
    }
    turn(&reactor);
    recvs.truncate(2);
    let fence = reactor.drain();
    let mut fence = pin!(fence);
    let mut done = false;
    for _ in 0..MAX_TURNS {
        if poll_once(fence.as_mut()).is_ready() {
            done = true;
            break;
        }
        turn(&reactor);
    }
    assert!(done, "drain completes");
    assert_eq!(reactor.in_flight(), 0);
    for recv in &mut recvs {
        let Poll::Ready(out) = poll_once(recv.as_mut()) else {
            panic!("fenced operation is ready");
        };
        assert!(out.result.is_err());
    }
}

#[test]
fn sleep_until_fires_with_auto_advance() {
    let world = World::new();
    let clock = SimClock::new(5);
    // Each reactor wait advances the clock by at most `max_wait`.
    let reactor = reactor_with(
        &world,
        &clock,
        ReactorConfig::default().with_max_wait(Duration::from_secs(1)),
    );
    reactor.backend_mut().set_auto_advance(true);
    let start = clock.now();
    let deadline = start + Duration::from_secs(5);
    run(&reactor, reactor.sleep_until(deadline, &Unscoped)).unwrap();
    // The timer fires at the deadline; an idle wait in the same driver turn
    // may then advance by at most one more `max_wait`.
    assert!(clock.now() >= deadline);
    assert!(clock.now() <= deadline + Duration::from_secs(1));
}

#[test]
fn sleep_until_without_auto_advance_waits_for_the_clock() {
    let (_world, clock, reactor) = setup();
    let deadline = clock.now() + Duration::from_millis(10);
    let mut sleep = pin!(reactor.sleep_until(deadline, &Unscoped));
    for _ in 0..5 {
        assert!(poll_once(sleep.as_mut()).is_pending());
        turn(&reactor);
    }
    clock.advance(Duration::from_millis(10));
    run(&reactor, sleep).unwrap();
}

#[test]
fn injected_errno_is_returned_with_the_buffer() {
    let (world, _clock, reactor) = setup();
    let (a, _b) = world.socket_pair().unwrap();
    world
        .inject(
            FaultTarget::Op(OpKind::Send),
            Fault::Errno(libc::ECONNRESET),
        )
        .unwrap();
    let out = run(&reactor, reactor.send(&a, b"abc".to_vec(), &Unscoped));
    assert_eq!(
        out.result.unwrap_err().raw_os_error(),
        Some(libc::ECONNRESET)
    );
    assert_eq!(out.buf, b"abc");
}

#[test]
fn short_transfers_report_partial_progress_without_retry() {
    let (world, _clock, reactor) = setup();
    let (a, b) = world.socket_pair().unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Send), Fault::Short(2))
        .unwrap();
    let out = run(&reactor, reactor.send(&a, b"hello".to_vec(), &Unscoped));
    assert_eq!(out.result.unwrap(), 2, "the reactor never retries");

    // The caller resumes from the reported progress.
    let rest = out.buf[2..].to_vec();
    let out = run(&reactor, reactor.send(&a, rest, &Unscoped));
    assert_eq!(out.result.unwrap(), 3);

    world
        .inject(FaultTarget::Op(OpKind::Recv), Fault::Short(1))
        .unwrap();
    let first = run(&reactor, reactor.recv(&b, Vec::with_capacity(8), &Unscoped));
    assert_eq!(first.buf, b"h");
    let second = run(&reactor, reactor.recv(&b, Vec::with_capacity(8), &Unscoped));
    assert_eq!(second.buf, b"ello");
}

#[test]
fn disk_crash_fails_pending_file_operations_with_eio() {
    let (world, _clock, reactor) = setup();
    world.create_dir_all("/storage").unwrap();
    world.write_file("/storage/file", b"old").unwrap();
    world.disk().sync_all().unwrap();
    let file = world.open("/storage/file", libc::O_RDWR).unwrap();
    world
        .inject(FaultTarget::Op(OpKind::Write), Fault::Delay(10))
        .unwrap();
    let mut write = pin!(reactor.write_at(&file, 0, b"new".to_vec(), &Unscoped));
    assert!(poll_once(write.as_mut()).is_pending());
    turn(&reactor);
    world.disk().crash_under("/storage").unwrap();
    let out = run(&reactor, write);
    assert_eq!(out.result.unwrap_err().raw_os_error(), Some(libc::EIO));
    assert_eq!(world.read_file("/storage/file").unwrap(), b"old");
}

#[test]
fn reserved_partition_admits_work_when_ordinary_capacity_is_full() {
    let world = World::new();
    let clock = SimClock::new(3);
    let reactor = reactor_with(
        &world,
        &clock,
        ReactorConfig::default()
            .with_max_in_flight(1)
            .with_max_reserved(1),
    );
    let (a, b) = world.socket_pair().unwrap();
    let mut blocked = pin!(reactor.recv(&b, Vec::with_capacity(1), &Unscoped));
    assert!(poll_once(blocked.as_mut()).is_pending());

    let rejected = run(&reactor, reactor.nop(&Unscoped)).unwrap_err();
    assert_eq!(rejected.kind(), ErrorKind::Overloaded);

    let reservation = reactor.reserve(1).unwrap();
    assert_eq!(reactor.reserved_available(), 0);
    run(&reactor, reactor.nop(&Unscoped).reserved(&reservation)).unwrap();
    assert_eq!(reservation.in_use(), 0);

    // Exhausting the reservation rejects only reserved work.
    let mut held = pin!(
        reactor
            .recv(&a, Vec::with_capacity(1), &Unscoped)
            .reserved(&reservation)
    );
    assert!(poll_once(held.as_mut()).is_pending());
    let exhausted = run(&reactor, reactor.nop(&Unscoped).reserved(&reservation)).unwrap_err();
    assert_eq!(exhausted.kind(), ErrorKind::Overloaded);

    drop(reservation);
    a.send(b"1").unwrap();
    b.send(b"2").unwrap();
    assert_eq!(run(&reactor, blocked).result.unwrap(), 1);
    assert_eq!(run(&reactor, held).result.unwrap(), 1);
    settle(&reactor);
    assert_eq!(
        reactor.reserved_available(),
        1,
        "capacity returns to the pool"
    );
}

#[test]
fn rejection_observer_may_reenter_the_reactor() {
    let world = World::new();
    let clock = SimClock::new(4);
    let reactor = Rc::new(reactor_with(
        &world,
        &clock,
        ReactorConfig::default().with_max_in_flight(1),
    ));
    let seen = Rc::new(RefCell::new(Vec::new()));
    let observed_in_flight = Rc::new(Cell::new(usize::MAX));
    {
        let weak = Rc::downgrade(&reactor);
        let seen = Rc::clone(&seen);
        let observed = Rc::clone(&observed_in_flight);
        reactor.set_rejection_observer(move |rejection| {
            seen.borrow_mut().push((rejection.reason, rejection.kind));
            let reactor = weak.upgrade().expect("reactor alive");
            observed.set(reactor.in_flight());
            // Re-entering mutating calls is allowed too.
            reactor.poll(8).unwrap();
            reactor.clear_rejection_observer();
        });
    }
    let (_a, b) = world.socket_pair().unwrap();
    let mut blocked = pin!(reactor.recv(&b, Vec::with_capacity(1), &Unscoped));
    assert!(poll_once(blocked.as_mut()).is_pending());
    assert!(run(&reactor, reactor.nop(&Unscoped)).is_err());
    assert!(run(&reactor, reactor.nop(&Unscoped)).is_err());
    assert_eq!(
        *seen.borrow(),
        [(RejectionReason::Capacity, OpKind::Nop)],
        "the observer cleared itself on first use"
    );
    assert_eq!(observed_in_flight.get(), 1);
}

#[test]
fn backend_rejections_surface_as_queue_full() {
    let (world, _clock, reactor) = setup();
    world.reject_submissions(1_000).unwrap();
    let error = run(&reactor, reactor.nop(&Unscoped)).unwrap_err();
    assert_eq!(error.kind(), ErrorKind::Overloaded);
    assert_eq!(reactor.in_flight(), 0);
    world.reject_submissions(0).unwrap();
    run(&reactor, reactor.nop(&Unscoped)).unwrap();
}

#[test]
fn identical_seeds_replay_identical_traces() {
    fn scenario(seed: u64) -> Vec<uring_runtime_sim::Event> {
        let world = World::new();
        let clock = SimClock::new(seed);
        let reactor = reactor_with(&world, &clock, ReactorConfig::default());
        reactor.backend_mut().set_auto_advance(true);
        let (a, b) = world.socket_pair().unwrap();
        world
            .inject(FaultTarget::Op(OpKind::Send), Fault::Short(3))
            .unwrap();
        run(&reactor, reactor.send(&a, b"abcdef".to_vec(), &Unscoped))
            .result
            .unwrap();
        run(&reactor, reactor.recv(&b, Vec::with_capacity(8), &Unscoped))
            .result
            .unwrap();
        let deadline = clock.now() + Duration::from_millis(250);
        run(&reactor, reactor.sleep_until(deadline, &Unscoped)).unwrap();
        world.take_trace()
    }
    assert_eq!(scenario(11), scenario(11));
}
