// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[path = "../storage/http_io_pressure.rs"]
mod http_io_pressure;
#[path = "../storage/slab_io_ring.rs"]
mod slab_io_ring;
use crate::buffers::{self, Key};
use std::io::{Read as _, Write as _};
use std::os::unix::net::UnixStream;
use std::process::Command;

fn fill(pool: &WorkerPool, key: u8) -> Fill {
    pool.stage(Key::new([key; 32])).unwrap()
}
fn request(resource: Resource, opcode: u8, abandoned: bool) -> Request {
    Request {
        slab_pending: None,
        slab_charge: None,
        metric_traffic: None,
        _keepalive: None,
        resource,
        _fd: None,
        opcode,
        state: State::InFlight,
        abandoned,
    }
}

#[test]
fn dst_step7_receive_pressure_retains_reply_and_cancel_capacity() {
    for table in [false, true] {
        let world = crate::simulation::World::new(149);
        let _scope = world.enter();
        let pool = buffers::io_test_pool(1);
        let mut ring = Ring::http_test_ring(
            pool.clone(),
            Config {
                entries: if table { 64 } else { 8 },
                requests: 8,
                progress_reserve: 2,
                ..Default::default()
            },
        )
        .unwrap();
        let mut receives = Vec::new();
        loop {
            match ring.enqueue::<Bytes>(
                abi::Sqe {
                    opcode: abi::RECV,
                    ..Default::default()
                },
                Resource::Bytes(vec![0; 1].into_boxed_slice()),
                None,
            ) {
                Ok(ticket) => receives.push(ticket),
                Err((error, _)) => {
                    assert_eq!(error.kind(), io::ErrorKind::WouldBlock);
                    break;
                }
            }
        }
        assert_eq!(receives.len(), if table { 6 } else { 5 });
        let reply = ring
            .enqueue::<Bytes>(
                abi::Sqe {
                    opcode: abi::SEND,
                    ..Default::default()
                },
                Resource::Bytes(vec![0; 1].into_boxed_slice()),
                None,
            )
            .unwrap_or_else(|_| panic!("reply reserve lost"));
        // These are intentionally unsubmitted SQEs: cancellation must retire
        // them without a virtual socket or a terminal control buffer.
        drop((receives, reply));
        ring.shutdown().unwrap();
        pool.assert_recovered();
        world.assert_clean();
    }
}

#[test]
fn zc_error_and_cancel_do_not_release_before_notification() {
    for initial in [17, -libc::EIO, -libc::ECANCELED] {
        let pool = buffers::io_test_pool(1);
        let buffer = fill(&pool, 1).publish(17).unwrap();
        let mut send = request(Resource::Buffer(buffer), abi::SEND_ZC, true);
        assert!(!send.complete(initial, abi::MORE).unwrap());
        assert!(pool.stage(Key::new([2; 32])).is_err());
        assert!(send.complete(0, 0).is_err());
        assert!(pool.stage(Key::new([2; 32])).is_err());
        assert!(send.complete(0, abi::NOTIF).unwrap());
        assert!(matches!(send.state, State::Complete(res) if res == initial));
        assert!(send.complete(0, abi::NOTIF).is_err());
        drop(send);
        drop(fill(&pool, 2));
    }
}

pub(super) fn zc_retirement() {
    use crate::simulation::history::{Transition, require};
    let world = crate::simulation::current().unwrap();
    for initial in [17, -libc::EIO, -libc::ECANCELED] {
        let pool = buffers::io_test_pool(1);
        let buffer = fill(&pool, 1).publish(17).unwrap();
        // No kernel SQE references this allocation. Exercise the real transition
        // and retirement decision without introducing unsafe mutant DMA access.
        let mut send = Some(request(Resource::Buffer(buffer), abi::SEND_ZC, true));
        world.observation(Transition::ZcPrimaryCompletion { result: initial });
        if send.as_mut().unwrap().complete(initial, abi::MORE).unwrap() {
            drop(send.take());
        }
        let premature = pool.stage(Key::new([2; 32]));
        require(
            premature.is_err(),
            "ownership.zc-notification",
            format!("SEND_ZC primary result {initial} released its slot before notification"),
        );
        drop(premature);
        let retained = send.as_mut().unwrap();
        require(
            retained.complete(0, abi::NOTIF).unwrap(),
            "ownership.zc-terminal",
            "notification did not terminate the retained request",
        );
        require(
            matches!(retained.state, State::Complete(result) if result == initial),
            "ownership.zc-result",
            "notification lost the primary result",
        );
        // Dropping the request, rather than receipt of the CQE alone, returns
        // its owned buffer to the pool.
        require(
            pool.stage(Key::new([2; 32])).is_err(),
            "ownership.zc-retained",
            "completed request lost its resource",
        );
        drop(send);
        drop(fill(&pool, 2));
        pool.assert_recovered();
        world.observation(Transition::ZcNotificationRetired { result: initial });
    }
}

#[test]
fn premature_zc_mutant_requires_notification_oracle() {
    use crate::simulation::history::{Failure, Mutant};
    for mutant in [None, Some(Mutant::PrematureZcRetirement)] {
        let world = crate::simulation::World::new(19);
        let _scope = world.enter();
        world.enable_scheduler();
        world.mutant(mutant);
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(zc_retirement));
        match (mutant, result) {
            (None, Ok(())) => (),
            (Some(_), Err(failure)) => assert_eq!(
                failure
                    .downcast_ref::<Failure>()
                    .expect("named ownership failure")
                    .oracle,
                "ownership.zc-notification"
            ),
            _ => panic!("unexpected negative-control outcome"),
        }
    }
}

#[test]
fn zc_without_more_and_invalid_notifications() {
    let pool = buffers::io_test_pool(1);
    let mut send = request(
        Resource::Buffer(fill(&pool, 1).publish(1).unwrap()),
        abi::SEND_ZC,
        true,
    );
    assert!(send.complete(0, abi::NOTIF).is_err());
    assert!(matches!(send.state, State::InFlight));
    assert!(send.complete(-libc::EOPNOTSUPP, 0).unwrap());
    drop(send);
    let mut read = request(
        Resource::Writable(fill(&pool, 2).into_storage()),
        abi::READ_FIXED,
        false,
    );
    assert!(read.complete(1, abi::MORE).is_err());
    assert!(read.complete(1, 0).unwrap());
    let Resource::Writable(storage) = read.resource else {
        unreachable!()
    };
    let fill = Fill::from_storage(storage).unwrap_or_else(|_| unreachable!());
    assert_eq!(fill.publish(1).unwrap().as_slice().len(), 1);
}

#[test]
fn cancel_ack_is_independent_of_target_and_fill() {
    let pool = buffers::io_test_pool(1);
    let mut target = request(
        Resource::Writable(fill(&pool, 1).into_storage()),
        abi::RECV,
        false,
    );
    let mut cancel = request(Resource::None, abi::CANCEL, false);
    assert!(cancel.complete(0, 0).unwrap());
    drop(cancel);
    assert!(pool.stage(Key::new([2; 32])).is_err());
    assert!(target.complete(-libc::ECANCELED, 0).unwrap());
    drop(target);
    drop(fill(&pool, 2));
}

#[test]
fn destination_outlives_authority_and_cancel_ack_until_terminal_completion() {
    let pool = buffers::io_test_pool(1);
    let (authority, destination) = fill(&pool, 1).split_destination();
    let mut target = request(
        Resource::Writable(destination.into_storage()),
        abi::RECV,
        true,
    );
    drop(authority);
    let mut cancel = request(Resource::None, abi::CANCEL, true);
    assert!(cancel.complete(0, 0).unwrap());
    drop(cancel);
    assert!(pool.stage(Key::new([2; 32])).is_err());
    assert!(target.complete(-libc::ECANCELED, 0).unwrap());
    assert!(pool.stage(Key::new([2; 32])).is_err());
    drop(target);
    drop(fill(&pool, 2));
}

#[test]
fn wake_is_retained_and_nonblocking() {
    let wake = Arc::new(Wake::new().unwrap());
    let waker = std::task::Waker::from(wake.clone());
    let other = wake.clone();
    std::thread::spawn(move || {
        for _ in 0..1000 {
            workers::Wake::wake(&*other);
        }
    })
    .join()
    .unwrap();
    waker.wake_by_ref();
    let mut fd = libc::pollfd {
        fd: wake.fd.as_ref().unwrap().as_raw_fd(),
        events: libc::POLLIN,
        revents: 0,
    };
    // SAFETY: valid one-element poll array.
    assert_eq!(unsafe { libc::poll(&mut fd, 1, 0) }, 1);
    wake.drain().unwrap();
    assert_eq!(unsafe { libc::poll(&mut fd, 1, 0) }, 0);
    wake.drain().unwrap();
}

#[test]
fn splice_owned_retains_original_allocation_through_cancel_ack() {
    let world = crate::simulation::World::new(605);
    let _scope = world.enter();
    world.enable_scheduler();
    let mut ring = crate::conformance::ring(1, Default::default());
    let (input, output) = File::pipe().unwrap();
    let input_handle = Rc::downgrade(&input.0);
    let output_handle = Rc::downgrade(&output.0);
    let owner = Rc::new((input, [0u8; 16]));
    let weak = Rc::downgrade(&owner);
    let mut target = ring
        .splice_owned(owner.clone(), |owner| &owner.0, output.into(), None, 1)
        .unwrap();
    let retained = ring.request(&target).unwrap()._keepalive.as_ref().unwrap();
    assert!(Rc::ptr_eq(
        retained,
        &(owner.clone() as Rc<dyn std::any::Any>)
    ));
    let mut cancel = ring.cancel(&target).unwrap();
    drop(owner);

    // Drive the real ownership table with an acknowledgment strictly before the
    // target CQE, without letting the simulator choose their completion order.
    let core = ring.core.as_mut().unwrap();
    let RawRing::Sim(sim) = &mut core.raw else {
        unreachable!()
    };
    assert_eq!(sim.staged.len(), 2);
    sim.staged.clear();
    core.complete(
        abi::Cqe {
            user_data: cancel.id,
            res: 0,
            flags: 0,
        },
        &ring.metrics,
    )
    .unwrap();
    ring.take_cancel(&mut cancel)
        .unwrap()
        .unwrap()
        .result
        .unwrap();
    assert!(ring.take_splice(&mut target).unwrap().is_none());
    assert!(weak.upgrade().is_some());
    assert!(input_handle.upgrade().is_some());
    assert!(output_handle.upgrade().is_some());

    ring.core
        .as_mut()
        .unwrap()
        .complete(
            abi::Cqe {
                user_data: target.id,
                res: -libc::ECANCELED,
                flags: 0,
            },
            &ring.metrics,
        )
        .unwrap();
    assert!(
        weak.upgrade().is_some(),
        "uncollected target still owns resources"
    );
    assert_eq!(
        ring.take_splice(&mut target)
            .unwrap()
            .unwrap()
            .unwrap_err()
            .raw_os_error(),
        Some(libc::ECANCELED)
    );
    assert!(weak.upgrade().is_none());
    assert!(input_handle.upgrade().is_none());
    assert!(output_handle.upgrade().is_none());
    ring.shutdown().unwrap();
    drop(ring);
    world.assert_clean();
}

#[test]
fn kernel_integration() {
    // Bound blocking-syscall bugs outside the process under test. Set
    // RACER_REQUIRE_URING=1 in Linux CI to prohibit environmental skips.
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "uring::tests::kernel_child",
            "--ignored",
            "--nocapture",
        ])
        .env("RACER_URING_CHILD", "1")
        .spawn()
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(20);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(status.success());
            break;
        }
        if Instant::now() >= deadline {
            child.kill().unwrap();
            let _ = child.wait();
            panic!("io_uring integration child timed out");
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}

fn drive(ring: &mut Ring, mut done: impl FnMut(&mut Ring) -> bool) {
    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        let runnable = ring.progress().unwrap();
        if done(ring) {
            return;
        }
        assert!(Instant::now() < deadline, "completion timed out");
        if !runnable {
            ring.wait(Some(deadline)).unwrap();
        }
    }
}

fn composite_checks(config: Config) {
    use std::cell::Cell;
    use workers::Driver as _;
    struct App;
    impl Application for App {
        fn poll(&mut self, _: &mut Ring, _: usize) -> io::Result<Work> {
            Ok(Work::default())
        }
        fn shutdown(&mut self, _: &mut Ring) -> io::Result<()> {
            Ok(())
        }
    }
    struct Source(Rc<Cell<usize>>);
    impl CompletionSource for Source {
        fn poll(&mut self, _: &mut Ring, _: usize) -> io::Result<Work> {
            let armed = self.0.get() != 0;
            if armed {
                self.0.set(self.0.get() + 1);
            }
            Ok(Work {
                runnable: armed,
                deadline: None,
            })
        }
        fn arm(&mut self, _: &mut Ring) -> io::Result<()> {
            self.0.set(1);
            Ok(())
        }
        fn shutdown(&mut self, _: &mut Ring) -> io::Result<()> {
            Ok(())
        }
    }
    let ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let mut driver = Driver::new(ring, App, 1).unwrap();
    // A retained empty software wake must return to Workers without sleeping.
    workers::Wake::wake(&*driver.wake_handle());
    driver.turn().unwrap();
    let state = Rc::new(Cell::new(0));
    driver.add_source(Source(state.clone()));
    driver.turn().unwrap();
    assert!(state.get() >= 2, "source was not rechecked after arm");
    driver.shutdown().unwrap();
}

fn lifecycle_checks(config: Config) {
    let mut ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let f = fill(ring.pool(), 31);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let fd = File::new(socket.into());
    peer.write_all(b"x").unwrap();
    drop(
        ring.recv(fd.clone().into(), f, BufferRange::new(0..1).unwrap())
            .unwrap(),
    );
    let mut send = ring
        .send_bytes(fd.clone().into(), vec![1].into_boxed_slice())
        .unwrap();
    // Stage both real CQEs before invoking any callbacks, regardless of timing.
    let deadline = Instant::now() + Duration::from_secs(2);
    let core = ring.core.as_mut().unwrap();
    while core.cqes.len() < 2 {
        assert!(Instant::now() < deadline);
        core.raw
            .enter(true, Some(Duration::from_millis(10)))
            .unwrap();
        core.raw.reap(&mut core.cqes, 2).unwrap();
    }
    // Process the abandoned read before the live send completion.
    core.cqes.sort_by_key(|cqe| cqe.user_data);
    let start = Instant::now();
    ring.wait(Some(start + Duration::from_secs(2))).unwrap();
    assert!(
        start.elapsed() < Duration::from_secs(1),
        "cached CQEs must prevent sleeping"
    );
    ring.progress().unwrap();
    drive(&mut ring, |r| r.take_bytes(&mut send).unwrap().is_some());
    drop(fill(ring.pool(), 32));
    ring.shutdown().unwrap();

    let mut ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    peer.set_nonblocking(true).unwrap();
    let fixed = ring.register_file(File::new(socket.into())).unwrap();
    let mut recv = ring
        .recv_bytes(fixed.clone().into(), vec![0; 1].into_boxed_slice())
        .unwrap();
    drop(fixed);
    ring.progress().unwrap();
    assert_eq!(
        peer.read(&mut [0]).unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
    peer.write_all(b"x").unwrap();
    drive(&mut ring, |r| r.take_bytes(&mut recv).unwrap().is_some());
    ring.progress().unwrap();
    assert_eq!(
        peer.read(&mut [0]).unwrap(),
        0,
        "unused fixed socket must close"
    );
    assert!(
        ring.core
            .as_ref()
            .unwrap()
            .fixed
            .iter()
            .all(Option::is_none)
    );

    // Reusing a slot before its cleanup notification is processed must not
    // unregister a newly installed, live capability.
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let file = File::new(socket.into());
    drop(ring.register_file(file.clone()).unwrap());
    let fixed = ring.register_file(file).unwrap();
    ring.progress().unwrap();
    let mut send = ring
        .send_bytes(fixed.into(), vec![9].into_boxed_slice())
        .unwrap();
    drive(&mut ring, |r| {
        r.take_bytes(&mut send)
            .unwrap()
            .is_some_and(|c| c.result.unwrap() == 1)
    });
    assert_eq!(peer.read(&mut [0]).unwrap(), 1);
    ring.progress().unwrap();
    assert_eq!(peer.read(&mut [0]).unwrap(), 0);
    ring.shutdown().unwrap();
}

fn capacity_and_transfer_checks(config: Config) {
    let mut ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let fd = File::new(socket.into());
    let range = BufferRange::new(0..8).unwrap();
    // Dropping before completion retains the Fill, then reclaims it normally.
    drop(
        ring.recv(fd.clone().into(), fill(ring.pool(), 40), range)
            .unwrap(),
    );
    ring.progress().unwrap();
    assert!(ring.pool().stage(Key::new([41; 32])).is_err());
    peer.write_all(b"x").unwrap();
    drive(&mut ring, |r| {
        r.core.as_ref().unwrap().free.len() == config.requests as usize
    });
    // Short reads preserve the actual count and leave publication to the caller.
    let mut recv = ring
        .recv(fd.clone().into(), fill(ring.pool(), 41), range)
        .unwrap();
    peer.write_all(b"abc").unwrap();
    drive(&mut ring, |r| {
        r.take_read(&mut recv).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 3);
            assert_eq!(c.resource.publish(3).unwrap().as_slice(), b"abc");
            true
        })
    });
    // Drop after completion, without collecting, also frees storage/capacity.
    let recv = ring
        .recv(fd.clone().into(), fill(ring.pool(), 42), range)
        .unwrap();
    peer.shutdown(std::net::Shutdown::Write).unwrap();
    drive(&mut ring, |r| {
        matches!(r.request(&recv).unwrap().state, State::Complete(0))
    });
    drop(recv);
    ring.progress().unwrap();
    drop(fill(ring.pool(), 43));

    let mut tickets = Vec::new();
    for _ in 0..config.entries - 1 {
        tickets.push(
            ring.send_bytes(fd.clone().into(), vec![7].into_boxed_slice())
                .unwrap(),
        );
    }
    let rejected = ring
        .send_bytes(fd.clone().into(), vec![8, 9].into_boxed_slice())
        .unwrap_err();
    assert_eq!(rejected.error.kind(), io::ErrorKind::WouldBlock);
    assert_eq!(&*rejected.resource, &[8, 9]);
    drive(&mut ring, |r| {
        tickets
            .iter()
            .all(|t| matches!(r.request(t).unwrap().state, State::Complete(_)))
    });
    // Fill the request table with completed but uncollected operations.
    while tickets.len() < config.requests as usize {
        let t = ring
            .send_bytes(fd.clone().into(), vec![7].into_boxed_slice())
            .unwrap();
        drive(&mut ring, |r| {
            matches!(r.request(&t).unwrap().state, State::Complete(_))
        });
        tickets.push(t);
    }
    let rejected = ring
        .recv(fd.clone().into(), fill(ring.pool(), 44), range)
        .unwrap_err();
    assert_eq!(rejected.error.kind(), io::ErrorKind::WouldBlock);
    let mut first = tickets.pop().unwrap();
    assert!(ring.take_bytes(&mut first).unwrap().is_some());
    let mut eof = ring.recv(fd.into(), rejected.resource, range).unwrap();
    drive(&mut ring, |r| {
        r.take_read(&mut eof).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 0);
            true
        })
    });
    drop(tickets);
    drive(&mut ring, |r| {
        r.core.as_ref().unwrap().free.len() == config.requests as usize
    });
    ring.shutdown().unwrap();

    // A small socket send buffer and a non-reading peer force a partial send.
    let mut ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let (socket, _peer) = UnixStream::pair().unwrap();
    let size = 4096i32;
    assert_eq!(
        unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const i32).cast(),
                size_of_val(&size) as libc::socklen_t,
            )
        },
        0
    );
    let mut send = ring
        .send_bytes(
            File::new(socket.into()).into(),
            vec![1; BUFFER_SIZE].into_boxed_slice(),
        )
        .unwrap();
    drive(&mut ring, |r| {
        r.take_bytes(&mut send).unwrap().is_some_and(|c| {
            let count = c.result.unwrap();
            assert!(
                count > 0 && count < BUFFER_SIZE,
                "expected partial send, got {count}"
            );
            assert_eq!(c.resource.len(), BUFFER_SIZE);
            true
        })
    });
    ring.shutdown().unwrap();
}

fn scheduling_checks(config: Config) {
    use std::cell::Cell;
    use std::sync::atomic::AtomicBool;
    use workers::Driver as _;
    struct App {
        deadline: Instant,
        polls: usize,
        rechecked: Option<std::sync::mpsc::Sender<()>>,
        wake_on_recheck: bool,
    }
    impl Application for App {
        fn poll(&mut self, ring: &mut Ring, _: usize) -> io::Result<Work> {
            self.polls += 1;
            if self.polls == 2 {
                if self.wake_on_recheck {
                    workers::Wake::wake(&*ring.wake_handle());
                }
                if let Some(sender) = self.rechecked.take() {
                    sender.send(()).unwrap();
                }
            }
            Ok(Work {
                runnable: false,
                deadline: Some(self.deadline),
            })
        }
        fn shutdown(&mut self, _: &mut Ring) -> io::Result<()> {
            Ok(())
        }
    }
    // Wake in the final application recheck, after ring.progress has already
    // run. The eventfd must retain this notification until the sleep syscall.
    let ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let start = Instant::now();
    let mut driver = Driver::new(
        ring,
        App {
            deadline: start + Duration::from_secs(2),
            polls: 0,
            rechecked: None,
            wake_on_recheck: true,
        },
        1,
    )
    .unwrap();
    driver.turn().unwrap();
    assert!(start.elapsed() < Duration::from_secs(1));
    driver.shutdown().unwrap();

    // A concurrent stop wake delivered after recheck must return control to
    // the worker, even though it produces no application I/O.
    let ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let wake = ring.wake_handle();
    let stop = Arc::new(AtomicBool::new(false));
    let other_stop = stop.clone();
    let (tx, rx) = std::sync::mpsc::channel();
    let thread = std::thread::spawn(move || {
        rx.recv_timeout(Duration::from_secs(2)).unwrap();
        std::thread::sleep(Duration::from_millis(20));
        other_stop.store(true, Ordering::Release);
        workers::Wake::wake(&*wake);
    });
    let start = Instant::now();
    let mut driver = Driver::new(
        ring,
        App {
            deadline: start + Duration::from_secs(2),
            polls: 0,
            rechecked: Some(tx),
            wake_on_recheck: false,
        },
        1,
    )
    .unwrap();
    driver.turn().unwrap();
    assert!(stop.load(Ordering::Acquire));
    assert!(start.elapsed() < Duration::from_secs(1));
    thread.join().unwrap();
    driver.shutdown().unwrap();

    let ring = Ring::create(buffers::io_test_pool(1), config).unwrap();
    let deadline = Instant::now() + Duration::from_millis(20);
    let mut driver = Driver::new(
        ring,
        App {
            deadline,
            polls: 0,
            rechecked: None,
            wake_on_recheck: false,
        },
        3,
    )
    .unwrap();
    driver.turn().unwrap();
    assert!(Instant::now() >= deadline);
    assert!(Instant::now() < deadline + Duration::from_secs(1));
    struct BusySource {
        id: usize,
        order: Rc<RefCell<Vec<usize>>>,
        arms: Rc<Cell<usize>>,
    }
    impl CompletionSource for BusySource {
        fn poll(&mut self, _: &mut Ring, budget: usize) -> io::Result<Work> {
            assert_eq!(budget, 3);
            self.order.borrow_mut().push(self.id);
            Ok(Work {
                runnable: true,
                deadline: None,
            })
        }
        fn arm(&mut self, _: &mut Ring) -> io::Result<()> {
            self.arms.set(self.arms.get() + 1);
            Ok(())
        }
        fn shutdown(&mut self, _: &mut Ring) -> io::Result<()> {
            Ok(())
        }
    }
    let order = Rc::new(RefCell::new(Vec::new()));
    let arms = Rc::new(Cell::new(0));
    for id in 0..3 {
        driver.add_source(BusySource {
            id,
            order: order.clone(),
            arms: arms.clone(),
        });
    }
    let polls = driver.application.polls;
    for _ in 0..3 {
        driver.turn().unwrap();
    }
    assert_eq!(&*order.borrow(), &[0, 1, 2, 1, 2, 0, 2, 0, 1]);
    assert_eq!(driver.application.polls, polls + 3);
    assert_eq!(arms.get(), 0, "budget exhaustion must prevent sleeping");
    driver.shutdown().unwrap();
}

#[test]
#[ignore = "subprocess helper; run kernel_integration instead"]
fn kernel_child() {
    assert_eq!(std::env::var("RACER_URING_CHILD").as_deref(), Ok("1"));
    let pool = buffers::io_test_pool(2);
    let config = Config {
        progress_reserve: 0,
        entries: 8,
        requests: 16,
        fixed_files: 2,
        completion_budget: 2,
        shutdown_timeout: Duration::from_millis(200),
    };
    let mut ring = match Ring::create(pool, config) {
        Ok(ring) => ring,
        Err(error)
            if std::env::var("RACER_REQUIRE_URING").as_deref() != Ok("1")
                && (matches!(
                    error.raw_os_error(),
                    Some(libc::EPERM | libc::ENOSYS | libc::ENOMEM)
                ) || error.kind() == io::ErrorKind::Unsupported) =>
        {
            eprintln!(
                "SKIP io_uring kernel tests: {error}; set RACER_REQUIRE_URING=1 to require them"
            );
            return;
        }
        Err(error) => panic!("ring setup: {error}"),
    };
    let (socket, mut peer) = UnixStream::pair().unwrap();
    peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
    let fd = File::new(socket.into());
    let fixed = ring.register_file(fd.clone()).unwrap();
    let range = BufferRange::new(0..4).unwrap();
    let f = fill(ring.pool(), 1);
    let mut ticket = ring.recv(fixed.clone().into(), f, range).unwrap();
    peer.write_all(b"test").unwrap();
    let mut buffer = None;
    drive(&mut ring, |r| {
        if let Some(done) = r.take_read(&mut ticket).unwrap() {
            assert_eq!(done.result.unwrap(), 4);
            buffer = Some(done.resource.publish(4).unwrap());
            true
        } else {
            false
        }
    });
    let buffer = buffer.unwrap();
    assert_eq!(buffer.as_slice(), b"test");
    let mut send = ring.send(fd.clone().into(), buffer.clone(), range).unwrap();
    drive(&mut ring, |r| {
        r.take_write(&mut send).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 4);
            true
        })
    });
    let mut received = [0; 4];
    peer.read_exact(&mut received).unwrap();
    assert_eq!(&received, b"test");

    // Explicit file position, registered file and registered pool storage.
    let name = std::ffi::CString::new("racer-uring-test").unwrap();
    let raw = unsafe { libc::memfd_create(name.as_ptr(), libc::MFD_CLOEXEC) };
    assert!(raw >= 0);
    let disk = File::new(unsafe { OwnedFd::from_raw_fd(raw) })
        .with_slab_io(crate::slab_io::Io::testing(20, 1, false));
    let mut write = ring
        .write(
            disk.clone().into(),
            buffer.clone(),
            range,
            FileOffset::new(4096).unwrap(),
        )
        .unwrap();
    drive(&mut ring, |r| {
        r.take_write(&mut write).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 4);
            true
        })
    });
    let f = fill(ring.pool(), 2);
    let mut read = ring
        .read(disk.into(), f, range, FileOffset::new(4096).unwrap())
        .unwrap();
    drive(&mut ring, |r| {
        r.take_read(&mut read).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 4);
            assert_eq!(c.resource.publish(4).unwrap().as_slice(), b"test");
            true
        })
    });
    slab_io_ring::kernel_wait_and_cancel(&mut ring);

    // Dropped tickets retain storage until the target CQE, not cancel ack.
    let mut pending = ring
        .recv_bytes(fd.clone().into(), vec![0; 8].into_boxed_slice())
        .unwrap();
    ring.progress().unwrap();
    let mut cancel = ring.cancel(&pending).unwrap();
    drive(&mut ring, |r| r.take_cancel(&mut cancel).unwrap().is_some());
    drive(&mut ring, |r| {
        r.take_bytes(&mut pending).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap_err().raw_os_error(), Some(libc::ECANCELED));
            true
        })
    });

    // Fixed handles prevent slot reuse; foreign ranges/storage are rejected.
    let _second = ring.register_file(fd.clone()).unwrap();
    assert_eq!(
        ring.register_file(fd.clone()).err().unwrap().kind(),
        io::ErrorKind::WouldBlock
    );
    let bad = ring
        .send(
            fd.clone().into(),
            buffer.clone(),
            BufferRange::new(0..5).unwrap(),
        )
        .unwrap_err();
    assert_eq!(bad.resource.as_slice(), b"test");
    let foreign = buffers::io_test_pool(1);
    assert!(
        ring.recv(fd.clone().into(), fill(&foreign, 3), range)
            .is_err()
    );

    // TCP connect/accept and real SEND_ZC (including its release notification).
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let listener = File::new(listener.into());
    let client = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
    assert!(client >= 0);
    let client = File::new(unsafe { OwnedFd::from_raw_fd(client) });
    let mut accept = ring.accept(listener.into()).unwrap();
    let mut connect = ring.connect(client.clone().into(), address).unwrap();
    drive(&mut ring, |r| {
        r.take_control(&mut connect).unwrap().is_some_and(|c| {
            c.result.unwrap();
            true
        })
    });
    let mut server = None;
    drive(&mut ring, |r| {
        r.take_accept(&mut accept).unwrap().is_some_and(|c| {
            c.result.unwrap();
            server = c.resource;
            true
        })
    });
    let mut zc = ring.send_zc(client.into(), buffer.clone(), range).unwrap();
    // Process exactly one CQE at a time so even an already-queued NOTIF
    // cannot hide premature ownership transfer after the early result.
    ring.config.completion_budget = 1;
    let mut saw_notification_wait = false;
    drive(&mut ring, |r| {
        if let Some(early) = r.send_result(&zc).unwrap() {
            assert_eq!(early.unwrap(), 4);
        }
        if matches!(r.request(&zc).unwrap().state, State::Notification(4)) {
            saw_notification_wait = true;
            assert!(r.take_send_zc(&mut zc).unwrap().is_none());
            return false;
        }
        r.take_send_zc(&mut zc).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 4);
            true
        })
    });
    assert!(
        saw_notification_wait,
        "kernel SEND_ZC did not exercise MORE/NOTIF"
    );
    ring.config.completion_budget = config.completion_budget;
    let mut tcp_read = ring
        .recv_bytes(server.unwrap().into(), vec![0; 4].into_boxed_slice())
        .unwrap();
    drive(&mut ring, |r| {
        r.take_bytes(&mut tcp_read).unwrap().is_some_and(|c| {
            assert_eq!(c.result.unwrap(), 4);
            assert_eq!(&*c.resource, b"test");
            true
        })
    });

    // Exercise queue wrap and generation reuse with bounded CQ batches.
    for _ in 0..64 {
        let mut sends = Vec::new();
        for _ in 0..6 {
            sends.push(
                ring.send_bytes(fd.clone().into(), vec![7].into_boxed_slice())
                    .unwrap(),
            );
        }
        for ticket in &mut sends {
            drive(&mut ring, |r| r.take_bytes(ticket).unwrap().is_some());
        }
        let mut bytes = [0; 6];
        peer.read_exact(&mut bytes).unwrap();
        assert_eq!(bytes, [7; 6]);
    }
    let wake = ring.wake_handle();
    workers::Wake::wake(&*wake);
    ring.wait(Some(Instant::now() + Duration::from_secs(1)))
        .unwrap();
    assert!(ring.progress().unwrap());
    let forgotten = ring
        .recv_bytes(fd.into(), vec![0; 8].into_boxed_slice())
        .unwrap();
    std::mem::forget(forgotten);
    ring.shutdown().unwrap();
    workers::Wake::wake(&*wake);
    ring.shutdown().unwrap();

    composite_checks(config);
    lifecycle_checks(config);
    capacity_and_transfer_checks(config);
    scheduling_checks(config);

    // Generation exhaustion retires a slot; foreign capabilities/tickets
    // cannot address another ring even with numerically equal slot IDs.
    let mut other = Ring::create(buffers::io_test_pool(1), config).unwrap();
    assert!(other.register_file(fixed.0._file.clone()).is_ok());
    assert!(other.poll_fd(fixed.into(), Readiness::Readable).is_err());
    assert!(other.take_bytes(&mut pending).is_err());
    let (socket, _peer) = UnixStream::pair().unwrap();
    let file = File::new(socket.into());
    let core = other.core.as_mut().unwrap();
    let index = *core.free.last().unwrap() as usize;
    core.slots[index].generation = u32::MAX - 1;
    let mut poll = other
        .poll_fd(file.clone().into(), Readiness::Writable)
        .unwrap();
    drive(&mut other, |r| r.take_control(&mut poll).unwrap().is_some());
    assert!(!other.core.as_ref().unwrap().free.contains(&(index as u32)));

    // A failed quiescence proof must retain the Fill even after Drop. Inject
    // an unresolved request with no kernel CQE; this subprocess bounds the
    // deliberate fallback leak and its real pool mapping.
    let retained_pool = other.pool.clone();
    let f = fill(&retained_pool, 9);
    let core = other.core.as_mut().unwrap();
    let index = core.free.pop().unwrap() as usize;
    core.slots[index].request = Some(request(
        Resource::Writable(f.into_storage()),
        abi::RECV,
        true,
    ));
    assert_eq!(
        other.shutdown().unwrap_err().kind(),
        io::ErrorKind::TimedOut
    );
    drop(other);
    assert!(retained_pool.stage(Key::new([10; 32])).is_err());
    eprintln!("io_uring kernel assertions completed");
}
