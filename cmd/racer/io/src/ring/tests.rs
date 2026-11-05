//! Reactor contracts against the real kernel (skipped where io_uring is
//! unavailable unless `RUNTIME_REQUIRE_IO_URING=1`) and the simulated OS.
use super::*;
use crate::{
    exec::LocalExecutor,
    sim::{Fault, SimOs},
};
use std::{
    cell::Cell,
    io::{Read as _, Write as _},
    os::unix::net::UnixStream,
    time::Duration,
};

/// A kernel reactor, or `None` when io_uring is unavailable.
pub(crate) fn kernel(entries: u32) -> Option<Reactor> {
    match Reactor::new(Driver::Uring, Config::new(entries), WorkerCtx::host()) {
        Ok(reactor) => Some(reactor),
        Err(Error::Os(libc::ENOSYS | libc::EPERM))
            if std::env::var_os("RUNTIME_REQUIRE_IO_URING").is_none() =>
        {
            None
        }
        Err(error) => panic!("io_uring setup failed: {error}"),
    }
}

/// A simulated reactor over `os` with a virtual clock.
fn simulated(os: &SimOs, clock: &crate::ctx::VirtualClock) -> Reactor {
    Reactor::new(
        Driver::Sim(os.clone()),
        Config::new(64),
        WorkerCtx::simulated(clock, 1, 1),
    )
    .unwrap()
}

/// Run `future` on `reactor` to completion.
fn block_on<F: Future + 'static>(reactor: &Reactor, future: F) -> F::Output {
    LocalExecutor::new(Some(reactor.wake_handle()))
        .block_on(reactor, future)
        .unwrap()
}

/// A lease that counts its drops.
#[derive(Debug)]
struct Lease(Rc<Cell<usize>>);

impl Drop for Lease {
    fn drop(&mut self) {
        self.0.set(self.0.get() + 1);
    }
}

#[test]
fn fence_accounting_requires_both_cqes() {
    let mut fence = Fence::Pending(CancelFence::NotSubmitted);
    assert!(fence.needs_cancel());
    assert_eq!(fence.complete(false, 5), Ok(Some(5)));
    assert_eq!(fence, Fence::Retired);
    assert_eq!(fence.complete(false, 5), Err(Error::Io));

    for cancel_first in [true, false] {
        let mut fence = Fence::Pending(CancelFence::Pending);
        assert!(!fence.needs_cancel());
        if cancel_first {
            assert_eq!(fence.complete(true, 0), Ok(None));
            assert_eq!(
                fence.complete(false, -libc::ECANCELED),
                Ok(Some(-libc::ECANCELED))
            );
        } else {
            assert_eq!(fence.complete(false, 7), Ok(None));
            assert_eq!(fence, Fence::AwaitingCancel(7));
            assert_eq!(fence.complete(true, -libc::ENOENT), Ok(Some(7)));
        }
        assert_eq!(fence.complete(true, 0), Err(Error::Io));
    }
    // A cancel CQE without a submitted cancel is a protocol violation.
    let mut fence = Fence::Pending(CancelFence::NotSubmitted);
    assert_eq!(fence.complete(true, 0), Err(Error::Io));
}

#[test]
fn slab_reuses_slots_with_new_generations() {
    let mut slab = Slab::new(2);
    let a = slab.alloc().unwrap();
    let b = slab.alloc().unwrap();
    assert!(slab.alloc().is_none());
    assert_ne!(a.index(), b.index());
    slab.release(a);
    let c = slab.alloc().unwrap();
    assert_eq!(c.index(), a.index());
    assert_ne!(c.generation(), a.generation());
    assert!(slab.lookup(a).is_none(), "stale ids never match");
    assert!(slab.lookup(c).is_some());
    let address = slab.lookup(c).unwrap() as *const Slot;
    slab.release(b);
    let _d = slab.alloc().unwrap();
    assert_eq!(
        slab.lookup(c).unwrap() as *const Slot,
        address,
        "slots never move"
    );
}

#[test]
fn kernel_context_must_not_be_simulated() {
    let clock = crate::ctx::VirtualClock::new();
    assert_eq!(
        Reactor::new(
            Driver::Uring,
            Config::default(),
            WorkerCtx::simulated(&clock, 1, 1)
        )
        .unwrap_err(),
        Error::InvalidConfiguration
    );
    assert_eq!(
        Reactor::new(Driver::Uring, Config::new(0), WorkerCtx::host()).unwrap_err(),
        Error::InvalidConfiguration
    );
}

#[test]
fn kernel_file_reads_writes_and_vectors() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    assert_ne!(reactor.setup_flags(), 0);
    let file = Rc::new(Descriptor::from(tempfile()));
    let ctx = OpCtx::none();
    let written = block_on(
        &reactor,
        reactor.write_at(file.clone(), 0, b"hello world".to_vec(), (), &ctx),
    )
    .unwrap();
    assert_eq!(written.bytes, 11);
    let read = block_on(
        &reactor,
        reactor.read_at(file.clone(), 6, vec![0; 5], (), &ctx),
    )
    .unwrap();
    assert_eq!(read.buffer, b"world");
    let header: Rc<[u8]> = Rc::from(&b"HEAD"[..]);
    let vectored = block_on(
        &reactor,
        reactor.write_vectored_at(file.clone(), 20, header, b"BODY".to_vec(), (), &ctx),
    )
    .unwrap();
    assert_eq!(vectored.bytes, 8);
    let back = block_on(
        &reactor,
        reactor.read_vectored_at(file.clone(), 20, vec![0; 2], vec![0; 6], (), &ctx),
    )
    .unwrap();
    assert_eq!(
        (&back.header[..], &back.body[..]),
        (&b"HE"[..], &b"ADBODY"[..])
    );
    block_on(&reactor, reactor.fsync(file.clone(), true, &ctx)).unwrap();
    let stat = block_on(&reactor, reactor.stat(file.clone(), &ctx)).unwrap();
    assert_eq!(stat.stx_size, 28);
    let bad = block_on(
        &reactor,
        reactor.read_at(file, u64::MAX, vec![0; 1], (), &ctx),
    );
    assert_eq!(bad.unwrap_err(), Error::InvalidInput);
    assert_eq!(reactor.in_flight(), 0);
}

/// An unlinked temporary file in the project-local tmp directory.
fn tempfile() -> std::fs::File {
    let dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../tmp");
    std::fs::create_dir_all(&dir).unwrap();
    let path = dir.join(format!(
        "racer-io-{}-{}",
        std::process::id(),
        NEXT_FILE.fetch_add(1, Ordering::Relaxed)
    ));
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create_new(true)
        .open(&path)
        .unwrap();
    std::fs::remove_file(&path).unwrap();
    file
}

static NEXT_FILE: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

#[test]
fn kernel_sockets_send_recv_and_pass_descriptors() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    let (left, right) = UnixStream::pair().unwrap();
    let left = Rc::new(Descriptor::from(left));
    let right = Rc::new(Descriptor::from(right));
    let ctx = OpCtx::none();
    let sent = block_on(
        &reactor,
        reactor.send(left.clone(), b"ping".to_vec(), (), &ctx),
    )
    .unwrap();
    assert_eq!(sent.bytes, 4);
    let got = block_on(&reactor, reactor.recv(right.clone(), vec![0; 16], (), &ctx)).unwrap();
    assert_eq!(&got.buffer[..got.bytes], b"ping");

    let (passed, mut peer) = UnixStream::pair().unwrap();
    let passed = Rc::new(Descriptor::from(passed));
    block_on(
        &reactor,
        reactor.send_msg(left.clone(), b"fd".to_vec(), vec![passed], (), &ctx),
    )
    .unwrap();
    let message = block_on(&reactor, reactor.recv_msg(right, vec![0; 8], (), &ctx)).unwrap();
    assert_eq!(&message.buffer[..message.bytes], b"fd");
    assert_eq!(message.descriptors.len(), 1);
    assert!(!message.truncated);
    assert_eq!(message.tls_record_type, None);
    let mut received = UnixStream::from(message.descriptors.into_iter().next().unwrap());
    received.write_all(b"x").unwrap();
    let mut byte = [0];
    peer.read_exact(&mut byte).unwrap();
    assert_eq!(&byte, b"x");
}

#[test]
fn kernel_accept_connect_and_poll() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    let listener = Rc::new(Descriptor::tcp_listener("127.0.0.1:0".parse().unwrap()).unwrap());
    let address = {
        let fd = listener.host().unwrap();
        // SAFETY: borrows the live listener without closing it.
        let socket = std::mem::ManuallyDrop::new(unsafe {
            std::net::TcpListener::from_raw_fd(std::os::fd::AsRawFd::as_raw_fd(&fd))
        });
        socket.local_addr().unwrap()
    };
    let client = Rc::new(Descriptor::socket(libc::AF_INET).unwrap());
    let ctx = OpCtx::none();
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let accept = executor.spawn(reactor.accept(listener, &ctx));
    let connect = reactor.connect(client.clone(), SocketAddress::Inet(address), 9u8, &ctx);
    let lease = executor.block_on(&reactor, connect).unwrap();
    let server = executor.block_on(&reactor, accept).unwrap();
    assert_eq!(lease.unwrap(), 9);
    let server = Rc::new(server.unwrap());
    server.enable_tcp_nodelay(true).unwrap();
    client.try_send(b"x").unwrap();
    let ready = block_on(
        &reactor,
        reactor.poll_ready(server, libc::POLLIN as u32, (), &ctx),
    )
    .unwrap();
    assert_ne!(ready & libc::POLLIN as u32, 0);
    let invalid = block_on(&reactor, reactor.poll_ready(client, 0, (), &ctx));
    assert_eq!(invalid.unwrap_err(), Error::InvalidInput);
}

#[test]
fn kernel_cancel_token_and_abandonment_retain_owners_until_fenced() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    let (left, _right) = UnixStream::pair().unwrap();
    let left = Rc::new(Descriptor::from(left));
    let drops = Rc::new(Cell::new(0));
    let token = CancelToken::new();
    let recv = reactor.recv(
        left.clone(),
        vec![0; 4],
        Lease(drops.clone()),
        &OpCtx::with_cancel(&token),
    );
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let handle = executor.spawn(recv);
    reactor.reap(16).unwrap();
    executor.run_ready(16);
    assert_eq!(reactor.in_flight(), 1);
    let remote = token.clone();
    std::thread::spawn(move || remote.cancel()).join().unwrap();
    let result = executor.block_on(&reactor, handle).unwrap();
    assert_eq!(result.unwrap_err(), Error::Cancelled);
    assert_eq!(drops.get(), 1);
    assert_eq!(reactor.in_flight(), 0);

    // Abandonment: the owner stays until both CQEs arrive.
    let mut recv = reactor.recv(left, vec![0; 4], Lease(drops.clone()), &OpCtx::none());
    let waker = Waker::noop();
    assert!(
        Pin::new(&mut recv)
            .poll(&mut Context::from_waker(waker))
            .is_pending()
    );
    drop(recv);
    assert_eq!(drops.get(), 1, "abandoned owners stay in the slot");
    assert_eq!(reactor.in_flight(), 1);
    reactor
        .shutdown(Instant::now() + Duration::from_secs(5))
        .unwrap();
    assert_eq!(drops.get(), 2);
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn kernel_deadlines_expire_by_timer_not_by_polling() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    let (left, _right) = UnixStream::pair().unwrap();
    let left = Rc::new(Descriptor::from(left));
    let start = Instant::now();
    let ctx = OpCtx::none().deadline(Deadline::after(start, Duration::from_millis(30)));
    let recv = reactor.recv(left, vec![0; 4], (), &ctx);
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let handle = executor.spawn(recv);
    let mut turns = 0;
    let result = loop {
        turns += 1;
        reactor.reap(64).unwrap();
        executor.run_ready(64);
        if let Some(result) = handle.try_take() {
            break result;
        }
        reactor.park(reactor.next_deadline()).unwrap();
    };
    assert_eq!(result.unwrap_err(), Error::DeadlineExceeded);
    assert!(start.elapsed() >= Duration::from_millis(30));
    assert!(turns < 10, "deadline wait must not spin ({turns} turns)");
    assert_eq!(reactor.stats().expired, 1);
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn kernel_idle_turns_never_enter_with_an_empty_queue() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    // The first turn submits the eventfd poll.
    reactor.reap(16).unwrap();
    let before = reactor.stats().enters;
    for _ in 0..10 {
        reactor.reap(16).unwrap();
    }
    let after = reactor.stats().enters;
    assert_eq!(after.submits, before.submits);
    assert_eq!(after.skipped, before.skipped + 10);
}

#[test]
fn kernel_lane_wakes_from_another_thread_without_polling() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let slot: Rc<RefCell<Option<Waker>>> = Rc::default();
    let flag = Arc::new(AtomicBool::new(false));
    let (task_slot, task_flag) = (slot.clone(), flag.clone());
    let handle = executor.spawn(std::future::poll_fn(move |cx| {
        if task_flag.load(Ordering::Acquire) {
            Poll::Ready(())
        } else {
            *task_slot.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }
    }));
    reactor.reap(16).unwrap();
    executor.run_ready(16);
    let waker = slot.borrow_mut().take().unwrap();
    let remote = flag.clone();
    let thread = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(20));
        remote.store(true, Ordering::Release);
        waker.wake();
    });
    let mut parks = 0;
    while !handle.is_finished() {
        reactor.park(None).unwrap();
        parks += 1;
        reactor.reap(16).unwrap();
        executor.run_ready(16);
    }
    thread.join().unwrap();
    assert!(parks <= 2, "one wake, one park ({parks})");
}

#[test]
fn kernel_timers_and_class_fences() {
    let Some(reactor) = kernel(8) else {
        return;
    };
    let start = reactor.now();
    let sleep = reactor.sleep_until(start + Duration::from_millis(10));
    block_on(&reactor, sleep);
    assert!(reactor.now() >= start + Duration::from_millis(10));

    let (left, _right) = UnixStream::pair().unwrap();
    let left = Rc::new(Descriptor::from(left));
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let class = OpCtx::none().class(IoClass(3));
    let other = executor.spawn(reactor.recv(left.clone(), vec![0; 1], (), &OpCtx::none()));
    let fenced = executor.spawn(reactor.recv(left, vec![0; 1], (), &class));
    reactor.reap(16).unwrap();
    executor.run_ready(16);
    executor
        .block_on(&reactor, reactor.fence_class(IoClass(3)))
        .unwrap();
    assert_eq!(fenced.try_take().unwrap().unwrap_err(), Error::Cancelled);
    assert!(!other.is_finished());
    assert_eq!(reactor.in_flight(), 1);
    executor.block_on(&reactor, reactor.fence_all()).unwrap();
    assert_eq!(other.try_take().unwrap().unwrap_err(), Error::Cancelled);
    assert!(reactor.is_stopped());
}

#[test]
fn sim_socket_round_trip_and_eagain_retry() {
    let os = SimOs::new();
    let clock = crate::ctx::VirtualClock::new();
    let reactor = simulated(&os, &clock);
    let (left, right) = os.socket_pair();
    let (left, right) = (Rc::new(left), Rc::new(right));
    let ctx = OpCtx::none();
    // The receive waits for data across turns, then completes.
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let recv = executor.spawn(reactor.recv(right.clone(), vec![0; 8], (), &ctx));
    reactor.reap(16).unwrap();
    executor.run_ready(16);
    assert!(!recv.is_finished());
    executor
        .block_on(
            &reactor,
            reactor.send(left.clone(), b"abc".to_vec(), (), &ctx),
        )
        .unwrap()
        .unwrap();
    let got = executor.block_on(&reactor, recv).unwrap().unwrap();
    assert_eq!(&got.buffer[..got.bytes], b"abc");

    // Zero-progress EAGAIN and EINTR are retried inside the reactor.
    os.inject("recv", Fault::Errno(libc::EAGAIN)).unwrap();
    os.inject("recv", Fault::Errno(libc::EINTR)).unwrap();
    left.try_send(b"z").unwrap();
    let got = block_on(&reactor, reactor.recv(right.clone(), vec![0; 8], (), &ctx)).unwrap();
    assert_eq!(&got.buffer[..got.bytes], b"z");
    assert_eq!(
        reactor.stats().retries,
        3,
        "EAGAIN poll, EAGAIN reissue, EINTR"
    );

    // The retry bound returns the last errno.
    for _ in 0..9 {
        os.inject("recv", Fault::Errno(libc::EINTR)).unwrap();
    }
    let error = block_on(&reactor, reactor.recv(right, vec![0; 8], (), &ctx)).unwrap_err();
    assert_eq!(error, Error::Os(libc::EINTR));
}

#[test]
fn reserved_partition_is_kept_for_reserved_operations() {
    let os = SimOs::new();
    let clock = crate::ctx::VirtualClock::new();
    let reactor = Reactor::new(
        Driver::Sim(os.clone()),
        Config {
            queue_entries: 3,
            reserved_entries: 1,
        },
        WorkerCtx::simulated(&clock, 1, 1),
    )
    .unwrap();
    let (_left, right) = os.socket_pair();
    let right = Rc::new(right);
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let ordinary = OpCtx::none();
    let reserved = OpCtx::none().reserved();
    let a = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &ordinary));
    let b = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &ordinary));
    let c = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &ordinary));
    let d = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &reserved));
    let e = executor.spawn(reactor.recv(right, vec![0; 1], (), &reserved));
    executor.run_ready(16);
    assert_eq!(c.try_take().unwrap().unwrap_err(), Error::Overloaded);
    assert_eq!(e.try_take().unwrap().unwrap_err(), Error::Overloaded);
    assert_eq!(reactor.in_flight(), 3);
    assert_eq!(reactor.stats().rejected, 2);
    executor.block_on(&reactor, reactor.fence_all()).unwrap();
    for handle in [a, b, d] {
        assert_eq!(
            executor.block_on(&reactor, handle).unwrap().unwrap_err(),
            Error::Cancelled
        );
    }
    assert!(
        Reactor::new(
            Driver::Sim(os),
            Config {
                queue_entries: 1,
                reserved_entries: 2
            },
            WorkerCtx::simulated(&clock, 1, 1)
        )
        .is_err()
    );
}

#[test]
fn reserved_partition_can_be_resized_while_running() {
    let os = SimOs::new();
    let clock = crate::ctx::VirtualClock::new();
    let reactor = Reactor::new(
        Driver::Sim(os.clone()),
        Config::new(3),
        WorkerCtx::simulated(&clock, 1, 1),
    )
    .unwrap();
    let (_left, right) = os.socket_pair();
    let right = Rc::new(right);
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let ordinary = OpCtx::none();
    let a = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &ordinary));
    let b = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &ordinary));
    executor.run_ready(16);
    // Two live ordinary operations leave room for only one reserved slot.
    assert_eq!(
        reactor.set_reserved_entries(2),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(
        reactor.set_reserved_entries(4),
        Err(Error::InvalidConfiguration)
    );
    reactor.set_reserved_entries(1).unwrap();
    let c = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &ordinary));
    let d = executor.spawn(reactor.recv(right.clone(), vec![0; 1], (), &OpCtx::none().reserved()));
    executor.run_ready(16);
    assert_eq!(c.try_take().unwrap().unwrap_err(), Error::Overloaded);
    assert_eq!(reactor.in_flight(), 3);
    executor.block_on(&reactor, reactor.fence_all()).unwrap();
    for handle in [a, b, d] {
        assert_eq!(
            executor.block_on(&reactor, handle).unwrap().unwrap_err(),
            Error::Cancelled
        );
    }
    reactor.set_reserved_entries(0).unwrap();
}

#[test]
fn fence_ids_waits_for_named_operations_only() {
    let os = SimOs::new();
    let clock = crate::ctx::VirtualClock::new();
    let reactor = simulated(&os, &clock);
    let (_left, right) = os.socket_pair();
    let right = Rc::new(right);
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let mut first = reactor.recv(right.clone(), vec![0; 1], (), &OpCtx::none());
    let mut second = reactor.recv(right, vec![0; 1], (), &OpCtx::none());
    let waker = std::task::Waker::noop();
    let mut cx = Context::from_waker(waker);
    assert!(Pin::new(&mut first).poll(&mut cx).is_pending());
    assert!(Pin::new(&mut second).poll(&mut cx).is_pending());
    let (first_id, second_id) = (first.id().unwrap(), second.id().unwrap());
    assert!(!reactor.is_fenced(first_id));
    // Abandoning the future cancels it; the fence still waits for the kernel.
    drop(first);
    executor
        .block_on(&reactor, reactor.fence_ids(vec![first_id]))
        .unwrap();
    assert!(reactor.is_fenced(first_id));
    assert!(!reactor.is_fenced(second_id));
    reactor.cancel_op(second_id, Error::DeadlineExceeded);
    executor
        .block_on(&reactor, reactor.fence_ids(vec![first_id, second_id]))
        .unwrap();
    assert_eq!(
        executor.block_on(&reactor, second).unwrap().unwrap_err(),
        Error::DeadlineExceeded
    );
}

#[test]
fn sim_cost_model_charges_virtual_time() {
    let os = SimOs::new();
    let seen = Rc::new(std::cell::RefCell::new(Vec::new()));
    let log = seen.clone();
    os.set_op_cost(move |desc| {
        log.borrow_mut().push(*desc);
        match desc.kind {
            "send" => Duration::from_millis(desc.bytes),
            _ => Duration::ZERO,
        }
    });
    let clock = crate::ctx::VirtualClock::new();
    let reactor = simulated(&os, &clock);
    let (left, right) = os.socket_pair();
    let ctx = OpCtx::none();
    let start = clock.now();
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let send = executor.spawn(reactor.send(Rc::new(left), b"abcde".to_vec(), (), &ctx));
    reactor.reap(16).unwrap();
    executor.run_ready(16);
    reactor.reap(16).unwrap();
    // Latency-gated: not runnable, but the reactor reports when it is due.
    assert!(!send.is_finished());
    assert!(!reactor.has_ready());
    assert_eq!(
        reactor.next_deadline(),
        Some(start + Duration::from_millis(5))
    );
    executor.block_on(&reactor, send).unwrap().unwrap();
    assert_eq!(clock.now() - start, Duration::from_millis(5));
    let got = block_on(&reactor, reactor.recv(Rc::new(right), vec![0; 8], (), &ctx)).unwrap();
    assert_eq!(&got.buffer[..got.bytes], b"abcde");
    assert_eq!(clock.now() - start, Duration::from_millis(5));
    let kinds: Vec<_> = seen.borrow().iter().map(|d| (d.kind, d.bytes)).collect();
    assert_eq!(kinds, [("send", 5), ("recv", 8)]);
    assert_eq!(
        os.op_cost(&crate::sim::OpDesc {
            kind: "send",
            bytes: 2
        }),
        Duration::from_millis(2)
    );
    assert_eq!(
        SimOs::new().op_cost(&crate::sim::OpDesc {
            kind: "send",
            bytes: 2
        }),
        Duration::ZERO
    );
}

#[test]
fn sim_deadline_advances_virtual_time_and_expires() {
    let os = SimOs::new();
    let clock = crate::ctx::VirtualClock::new();
    let reactor = simulated(&os, &clock);
    let (_left, right) = os.socket_pair();
    let start = clock.now();
    let ctx = OpCtx::none().deadline(Deadline::after(start, Duration::from_secs(3)));
    let result = block_on(&reactor, reactor.recv(Rc::new(right), vec![0; 8], (), &ctx));
    assert_eq!(result.unwrap_err(), Error::DeadlineExceeded);
    assert_eq!(clock.now() - start, Duration::from_secs(3));
    let sleep = reactor.sleep_until(clock.now() + Duration::from_secs(1));
    block_on(&reactor, sleep);
    assert_eq!(clock.now() - start, Duration::from_secs(4));
}

/// Run a fixed workload on a seeded world and return its trace.
fn seeded_trace(seed: u64) -> Vec<crate::sim::Event> {
    let os = SimOs::seeded(seed);
    let clock = crate::ctx::VirtualClock::new();
    let reactor = simulated(&os, &clock);
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let ctx = OpCtx::none();
    let mut handles = Vec::new();
    for _ in 0..4 {
        let (left, right) = os.socket_pair();
        let (left, right) = (Rc::new(left), Rc::new(right));
        let recv = reactor.recv(right, vec![0; 4], (), &ctx);
        let send = reactor.send(left, b"data".to_vec(), (), &ctx);
        handles.push(executor.spawn(async move {
            recv.await.unwrap();
        }));
        handles.push(executor.spawn(async move {
            send.await.unwrap();
        }));
    }
    for handle in handles {
        executor.block_on(&reactor, handle).unwrap();
    }
    os.take_trace()
}

#[test]
fn sim_same_seed_same_trace() {
    let a = seeded_trace(42);
    let b = seeded_trace(42);
    assert!(!a.is_empty());
    assert_eq!(a, b);
    let different = (1..8).any(|seed| seeded_trace(seed) != a);
    assert!(different, "seeds change the schedule");
}

#[test]
fn sim_cancel_fences_and_file_ops() {
    let os = SimOs::new();
    let clock = crate::ctx::VirtualClock::new();
    let reactor = simulated(&os, &clock);
    let ctx = OpCtx::none();
    let root = block_on(
        &reactor,
        reactor
            .open(
                None,
                c"/".into(),
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                &ctx,
            )
            .unwrap(),
    )
    .unwrap();
    let root = Rc::new(root);
    block_on(
        &reactor,
        reactor
            .mkdir(root.clone(), c"d".into(), 0o700, false, &ctx)
            .unwrap(),
    )
    .unwrap();
    block_on(
        &reactor,
        reactor
            .mkdir(root.clone(), c"d".into(), 0o700, true, &ctx)
            .unwrap(),
    )
    .unwrap();
    let file = block_on(
        &reactor,
        reactor
            .open(
                Some(root.clone()),
                c"d/f".into(),
                libc::O_CREAT | libc::O_RDWR,
                0,
                &ctx,
            )
            .unwrap(),
    )
    .unwrap();
    let file = Rc::new(file);
    block_on(
        &reactor,
        reactor.write_at(file.clone(), 0, b"xyz".to_vec(), (), &ctx),
    )
    .unwrap();
    block_on(&reactor, reactor.fsync(file.clone(), false, &ctx)).unwrap();
    block_on(
        &reactor,
        reactor
            .rename(root.clone(), c"d/f".into(), c"d/g".into(), 0, &ctx)
            .unwrap(),
    )
    .unwrap();
    assert_eq!(os.read_file(std::path::Path::new("/d/g")).unwrap(), b"xyz");
    block_on(
        &reactor,
        reactor
            .unlink(root.clone(), c"d/g".into(), 0, false, &ctx)
            .unwrap(),
    )
    .unwrap();
    let missing = block_on(
        &reactor,
        reactor
            .unlink(root.clone(), c"d/g".into(), 0, false, &ctx)
            .unwrap(),
    );
    assert_eq!(missing.unwrap_err(), Error::NotFound);
    block_on(
        &reactor,
        reactor.unlink(root, c"d/g".into(), 0, true, &ctx).unwrap(),
    )
    .unwrap();

    // A delayed operation canceled before it runs releases its owners once.
    let drops = Rc::new(Cell::new(0));
    os.inject("read", Fault::Delay(5)).unwrap();
    let token = CancelToken::new();
    let read = reactor.read_at(
        file,
        0,
        vec![0; 3],
        Lease(drops.clone()),
        &OpCtx::with_cancel(&token),
    );
    let executor = LocalExecutor::new(Some(reactor.wake_handle()));
    let handle = executor.spawn(read);
    reactor.reap(16).unwrap();
    executor.run_ready(16);
    token.cancel();
    let result = executor.block_on(&reactor, handle).unwrap();
    assert_eq!(result.unwrap_err(), Error::Cancelled);
    assert_eq!(drops.get(), 1);
    assert_eq!(reactor.in_flight(), 0);
}
