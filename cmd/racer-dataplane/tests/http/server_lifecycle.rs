// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod streaming_deadlines {
    use super::*;
    use crate::{
        buffers::{BUFFER_SIZE, Key},
        simulation::World,
    };

    // Raw client permits incomplete headers and reads bodies larger than one pool
    // buffer. All timing is virtual; no wall-clock sleeps or timing tolerances.
    struct Fixture<H: Handler> {
        world: World,
        ring: Ring,
        server: Server<H>,
        client: File,
        recv: Option<Ticket<Bytes>>,
        bytes: Vec<u8>,
        eof: bool,
        rdma: Option<crate::runtime::RdmaSource>,
    }
    impl<H: Handler> Fixture<H> {
        fn new(world: &World, handler: H, config: Config) -> Self {
            let address = "127.0.0.1:18914".parse().unwrap();
            let listener = Listener::bind(address, NonZeroU32::new(8).unwrap()).unwrap();
            let mut ring = crate::conformance::ring(8, Default::default());
            let client = File::simulated(world.socket());
            let mut connect = ring.connect(client.clone().into(), address).unwrap();
            for _ in 0..100 {
                ring.progress().unwrap();
                world.service_tick();
                if let Some(c) = ring.take_control(&mut connect).unwrap() {
                    c.result.unwrap();
                    return Self {
                        world: world.clone(),
                        ring,
                        server: Server::new(listener, handler, config),
                        client,
                        recv: None,
                        bytes: Vec::new(),
                        eof: false,
                        rdma: None,
                    };
                }
            }
            panic!("connect stalled");
        }
        fn tick(&mut self) -> Work {
            self.ring.progress().unwrap();
            if let Some(source) = &mut self.rdma {
                // Match Driver order: external completions before the HTTP app.
                crate::uring::CompletionSource::poll(source, &mut self.ring, 16).unwrap();
                crate::uring::CompletionSource::arm(source, &mut self.ring).unwrap();
            }
            let work = self.server.poll(&mut self.ring, 64).unwrap();
            if let Some(t) = &mut self.recv
                && let Some(c) = self.ring.take_bytes(t).unwrap()
            {
                let n = c.result.unwrap();
                self.eof = n == 0;
                self.bytes.extend_from_slice(&c.resource[..n]);
                self.recv = None;
            }
            if self.recv.is_none() && !self.eof {
                self.recv = Some(
                    self.ring
                        .recv_bytes(self.client.clone().into(), vec![0; 65536].into())
                        .unwrap()
                        .cancel_on_drop(),
                );
            }
            self.world.service_tick();
            work
        }
        fn send(&mut self, bytes: &[u8]) {
            let mut sent = 0;
            while sent < bytes.len() {
                let mut ticket = self
                    .ring
                    .send_bytes(self.client.clone().into(), bytes[sent..].into())
                    .unwrap();
                loop {
                    self.tick();
                    if let Some(c) = self.ring.take_bytes(&mut ticket).unwrap() {
                        sent += c.result.unwrap();
                        break;
                    }
                }
            }
        }
        fn until(&mut self, predicate: impl Fn(&Self) -> bool) {
            for _ in 0..100_000 {
                if predicate(self) {
                    return;
                }
                self.tick();
            }
            panic!("virtual progress watchdog");
        }
        fn finish(mut self) {
            self.server.shutdown(&mut self.ring).unwrap();
            assert_eq!(self.server.connections(), 0);
            self.recv.take();
            self.client.shutdown_socket();
            self.ring.shutdown().unwrap();
        }
    }

    struct Stream {
        length: usize,
        pause: Duration,
        hold: bool,
        cap: Option<Instant>,
        sent: usize,
        deadline: Option<Deadline>,
    }
    enum Task {
        Head(SendingHeadHeaders),
        Headers(SendingGetHeaders),
        Wait(BodyWriter, Instant),
        Body(SendingBody),
        Done,
    }
    impl Handler for Stream {
        type Task = Task;
        fn start(&mut self, mut request: Request) -> Task {
            if let Some(cap) = self.cap {
                request.cap_deadline(cap);
            }
            self.deadline = Some(request.response_deadline());
            let Request::Get(request) = request else {
                panic!()
            };
            Task::Headers(
                request
                    .respond(
                        ResponseHead::new(200, Some(self.length as u64), &[])
                            .unwrap()
                            .close(),
                    )
                    .unwrap(),
            )
        }
        fn poll(
            &mut self,
            task: &mut Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<Completed>> {
            if self.hold {
                return Ok(pending(false, None));
            }
            let progress = match task {
                Task::Head(h) => return h.poll(ring, budget),
                Task::Headers(h) => h.poll(ring, budget)?,
                Task::Body(b) => b.poll(ring, budget)?,
                Task::Wait(_, at) if crate::environment::now() < *at => {
                    return Ok(pending(false, Some(*at)));
                }
                Task::Wait(..) => {
                    let Task::Wait(writer, _) = std::mem::replace(task, Task::Done) else {
                        unreachable!()
                    };
                    let len = (writer.remaining() as usize).min(BUFFER_SIZE);
                    let mut f = ring.pool().stage(Key::new([201; 32])).unwrap();
                    f.as_mut_slice().fill(201);
                    let buffer = f.publish(BUFFER_SIZE).unwrap();
                    *task = Task::Body(
                        writer
                            .send(BodyChunk::new(buffer, 0..len).unwrap())
                            .unwrap(),
                    );
                    return Ok(pending(true, None));
                }
                Task::Done => panic!(),
            };
            match progress {
                Progress::Pending(w) => Ok(Progress::Pending(w)),
                Progress::Ready(BodyProgress::More(w)) => {
                    self.sent = self.length - w.remaining() as usize;
                    *task = Task::Wait(w, crate::environment::now() + self.pause);
                    Ok(pending(true, None))
                }
                Progress::Ready(BodyProgress::Done(c)) => {
                    self.sent = self.length;
                    *task = Task::Done;
                    Ok(Progress::Ready(c))
                }
            }
        }
    }
    fn stream(length: usize, pause: Duration) -> Stream {
        Stream {
            length,
            pause,
            hold: false,
            cap: None,
            sent: 0,
            deadline: None,
        }
    }
    fn short() -> Config {
        Config {
            idle_timeout: Duration::from_millis(200),
            request_timeout: Duration::from_millis(200),
            streaming_timeout: Duration::from_millis(200),
            ..Default::default()
        }
    }
    const GET: &[u8] = b"GET / HTTP/1.1\r\nHost: h\r\n\r\n";

    #[test]
    fn readiness25_blocked_provider_preserves_http_and_deadline_progress() {
        for timeout in [false, true] {
            let world = World::new(2501);
            let _scope = world.enter();
            world.enable_scheduler();
            let mut handler = stream(32, Duration::ZERO);
            handler.hold = timeout;
            let mut f = Fixture::new(&world, handler, short());
            let (transport, connection) = crate::rdma::tests::transport_config(f.ring.pool(), 1, 4);
            let mut request = connection.request([9; 32], 3, b"blocked READ").unwrap();
            connection.test_grant(&request);
            let grant = connection.take_grant(&mut request).unwrap().unwrap();
            let fill = f.ring.pool().stage(Key::new([9; 32])).unwrap();
            let read = connection.read(grant, fill).unwrap();
            transport.test_block_destroy(true);
            drop(read);
            f.rdma = Some(crate::runtime::RdmaSource::new(transport.test_source()));
            let start = world.now();
            f.send(GET);
            f.until(|f| f.eof || (!timeout && f.bytes.ends_with(&[201; 32])));
            assert!(world.now() - start < Duration::from_secs(1));
            if timeout {
                assert!(world.now() - start >= Duration::from_millis(200));
            } else {
                assert!(f.bytes.starts_with(b"HTTP/1.1 200"));
                assert!(f.bytes.ends_with(&[201; 32]));
            }
            assert_eq!(transport.test_invariants().2, 1);
            assert_eq!(transport.test_observe().qps, 1);
            // Source shutdown and its retry path must also return without releasing
            // the old destination; even a process-scale timeout is not quiescence.
            assert_eq!(
                transport.shutdown().unwrap_err().kind(),
                io::ErrorKind::WouldBlock
            );
            world.advance(Duration::from_secs(60));
            for _ in 0..100 {
                f.tick();
                assert_eq!(transport.test_invariants().2, 1);
                assert!(transport.prepare([7; 16], 0, 1).is_err());
            }
            transport.test_block_destroy(false);
            world.advance(Duration::from_millis(100));
            transport.shutdown().unwrap();
            assert_eq!(transport.test_invariants().2, 0);
            drop(connection);
            f.finish();
            world.assert_clean();
        }
    }

    #[test]
    fn lifecycle_drain_preserves_active_get_and_rejects_partial_admission() {
        for active in [false, true] {
            let world = World::new(622);
            let _scope = world.enter();
            world.enable_scheduler();
            world.link_profile(65536, Some(1));
            let mut f = Fixture::new(
                &world,
                stream(2 * BUFFER_SIZE, Duration::from_millis(10)),
                Config::default(),
            );
            f.send(if active { GET } else { b"GET /" });
            f.until(|f| {
                f.server.connections() == 1 && (!active || f.server.handler().deadline.is_some())
            });
            f.server.begin_drain();
            assert_eq!(f.server.connections(), usize::from(active));
            f.until(|f| f.eof);
            if active {
                let end = f.bytes.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
                assert_eq!(f.bytes.len() - end, 2 * BUFFER_SIZE);
                assert!(f.bytes[end..].iter().all(|b| *b == 201));
            } else {
                assert!(f.bytes.is_empty());
            }
            f.finish();
            world.assert_clean();
        }
    }

    mod lifecycle_tcp {
        use super::*;
        use crate::{lifecycle, uring, workers};
        use std::{
            io::Read,
            net::TcpStream,
            sync::{
                Arc,
                atomic::{AtomicBool, Ordering},
            },
            thread,
        };

        static SIGNAL: AtomicBool = AtomicBool::new(false);
        extern "C" fn stop_signal(_: libc::c_int) {
            SIGNAL.store(true, Ordering::Relaxed);
        }

        struct App {
            server: Server<Stream>,
            admitted: Arc<AtomicBool>,
            stall: Arc<AtomicBool>,
            cleanup: Option<(crate::runtime::Volumes, crate::rdma::Transport)>,
            cleanup_started: Option<Arc<AtomicBool>>,
            cleanup_done: Arc<AtomicBool>,
        }
        impl uring::Application for App {
            fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
                if self.stall.load(Ordering::Acquire) {
                    loop {
                        thread::park();
                    }
                }
                if self.cleanup.is_none()
                    && let Some(started) = &self.cleanup_started
                {
                    self.server.handler_mut().hold = !started.load(Ordering::Acquire);
                }
                let work = self.server.poll(ring, budget)?;
                if self.cleanup.is_none() {
                    self.admitted
                        .store(self.server.handler().deadline.is_some(), Ordering::Release);
                }
                Ok(work)
            }
            fn begin_drain(&mut self) {
                self.server.begin_drain();
            }
            fn drained(&self) -> bool {
                self.server.connections() == 0
            }
            fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
                self.server.shutdown(ring)?;
                if let Some((node, transport)) = &mut self.cleanup {
                    // Model the native helper's normal asynchronous start.
                    // Other workers must keep polling admitted HTTP meanwhile.
                    transport.test_destroy_after(Instant::now() + Duration::from_millis(900));
                    transport.test_block_destroy(false);
                    self.cleanup_started
                        .as_ref()
                        .unwrap()
                        .store(true, Ordering::Release);
                    node.shutdown(ring)?;
                    crate::runtime::staging_tests::assert_cleanup_complete(node);
                    assert_eq!(transport.test_invariants(), (0, 0, 0));
                    self.cleanup_done.store(true, Ordering::Release);
                }
                Ok(())
            }
        }

        #[test]
        fn lifecycle_kernel_process_active_get_drain_and_deadline() {
            cache_responses::kernel_child(
                "http_server::tests::streaming_deadlines::lifecycle_tcp::lifecycle_tcp_child",
                "RACER_LIFECYCLE_TCP",
            );
        }

        #[test]
        #[ignore = "bounded real io_uring/TCP subprocess"]
        fn lifecycle_tcp_child() {
            if std::env::var_os("RACER_LIFECYCLE_TCP").is_none() {
                return;
            }
            // SAFETY: initialized action; subprocess-only handler stores a lock-free atomic.
            unsafe {
                let mut action: libc::sigaction = std::mem::zeroed();
                action.sa_sigaction = stop_signal as *const () as usize;
                libc::sigemptyset(&mut action.sa_mask);
                assert_eq!(
                    libc::sigaction(libc::SIGTERM, &action, std::ptr::null_mut()),
                    0
                );
            }
            for (held, cleanup) in [(false, false), (true, false), (false, true)] {
                SIGNAL.store(false, Ordering::Relaxed);
                let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config {
                    drain: Duration::from_millis(if cleanup { 2000 } else { 600 }),
                    stall: Duration::from_secs(1),
                    ..Default::default()
                }));
                let stop = workers::StopHandle::supervised(life.clone());
                let _monitor =
                    lifecycle::Monitor::start(life.clone(), stop.clone(), &SIGNAL).unwrap();
                let plan = workers::CpuPlan::discover(
                    workers::Config {
                        shard_count: NonZeroUsize::new(if cleanup { 2 } else { 1 }).unwrap(),
                    },
                    workers::WorkerCounts {
                        io_per_node: if cleanup { None } else { NonZeroUsize::new(1) },
                        compute_per_node: NonZeroUsize::new(1),
                    },
                )
                .unwrap();
                life.configure_workers(plan.io().len());
                if cleanup {
                    assert_eq!(plan.io().len(), 2);
                }
                let address = TcpListener::bind("127.0.0.1:0")
                    .unwrap()
                    .local_addr()
                    .unwrap();
                let admitted = Arc::new(AtomicBool::new(false));
                let flag = admitted.clone();
                let health = life.clone();
                let cleanup_started = cleanup.then(|| Arc::new(AtomicBool::new(false)));
                let cleanup_done = Arc::new(AtomicBool::new(false));
                let started = cleanup_started.clone();
                let finished = cleanup_done.clone();
                let workers = workers::Workers::start_supervised(plan, stop, move |placement| {
                    let ring = crate::conformance::kernel_ring(4, Default::default())
                        .expect("real io_uring required");
                    let cleanup = if cleanup && placement.worker_id().0 == 0 {
                        let mut node = crate::runtime::staging_tests::volumes(
                            &ring,
                            &Arc::new(crate::control::Updates::default()),
                            0,
                        );
                        let transport =
                            crate::runtime::staging_tests::pending_cleanup(&mut node, &ring);
                        Some((node, transport))
                    } else {
                        None
                    };
                    let listener = Listener::bind(
                        if cleanup.is_some() {
                            "127.0.0.1:0".parse().unwrap()
                        } else {
                            address
                        },
                        NonZeroU32::new(8).unwrap(),
                    )?;
                    let mut handler = stream(
                        2 * BUFFER_SIZE,
                        if held {
                            Duration::from_secs(10)
                        } else {
                            Duration::from_millis(150)
                        },
                    );
                    handler.hold = false;
                    Ok(uring::Driver::new(
                        ring,
                        App {
                            server: Server::new(listener, handler, Config::default()),
                            admitted: flag.clone(),
                            stall: Arc::new(AtomicBool::new(false)),
                            cleanup,
                            cleanup_started: started.clone(),
                            cleanup_done: finished.clone(),
                        },
                        64,
                    )?
                    .with_lifecycle(health.clone(), placement.worker_id().0))
                })
                .unwrap();
                // Remain idle beyond the stall threshold: only reactor timer progress can
                // keep this healthy; metrics publication has no dirty counters to flush.
                thread::sleep(Duration::from_millis(1300));
                assert!(life.healthy(), "idle worker falsely stalled");
                let mut socket = TcpStream::connect(address).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(3)))
                    .unwrap();
                socket.write_all(GET).unwrap();
                let end = Instant::now() + Duration::from_secs(2);
                while !admitted.load(Ordering::Acquire) {
                    assert!(Instant::now() < end);
                    thread::sleep(Duration::from_millis(1));
                }
                let start = Instant::now();
                // SAFETY: this bounded subprocess has installed its SIGTERM handler.
                assert_eq!(unsafe { libc::kill(libc::getpid(), libc::SIGTERM) }, 0);
                while !life.draining() {
                    assert!(start.elapsed() < Duration::from_secs(1));
                    thread::sleep(Duration::from_millis(1));
                }
                assert!(!life.healthy());
                let mut bytes = Vec::new();
                socket.read_to_end(&mut bytes).unwrap();
                if cleanup {
                    assert!(
                        !cleanup_done.load(Ordering::Acquire),
                        "HTTP must drain during cleanup"
                    );
                }
                workers.join().unwrap();
                if cleanup {
                    assert!(cleanup_started.unwrap().load(Ordering::Acquire));
                    assert!(cleanup_done.load(Ordering::Acquire));
                    assert!(start.elapsed() >= Duration::from_millis(900));
                }
                assert!(start.elapsed() < Duration::from_secs(2));
                let body = bytes.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
                if held {
                    assert_eq!(bytes.len(), body);
                    assert!(start.elapsed() >= Duration::from_millis(600));
                } else {
                    assert_eq!(bytes.len() - body, 2 * BUFFER_SIZE);
                    assert!(bytes[body..].iter().all(|b| *b == 201));
                }
                assert!(TcpStream::connect(address).is_err());
            }
        }

        #[test]
        fn lifecycle_stalled_worker_process_exits_at_hard_deadline() {
            cache_responses::assert_child_selected(
                "http_server::tests::streaming_deadlines::lifecycle_tcp::lifecycle_stall_child",
            );
            let child = std::process::Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "http_server::tests::streaming_deadlines::lifecycle_tcp::lifecycle_stall_child",
                    "--ignored",
                    "--nocapture",
                    "--test-threads=2",
                ])
                .env("RACER_LIFECYCLE_STALL", "1")
                .spawn()
                .unwrap();
            struct Guard(std::process::Child);
            impl Drop for Guard {
                fn drop(&mut self) {
                    let _ = self.0.kill();
                    let _ = self.0.wait();
                }
            }
            let mut child = Guard(child);
            let end = Instant::now() + Duration::from_secs(8);
            loop {
                if let Some(status) = child.0.try_wait().unwrap() {
                    assert_eq!(status.code(), Some(124));
                    break;
                }
                assert!(
                    Instant::now() < end,
                    "stalled reactor escaped hard deadline"
                );
                thread::sleep(Duration::from_millis(5));
            }
        }

        #[test]
        #[ignore = "process helper: deliberately blocks inside a real driver poll"]
        fn lifecycle_stall_child() {
            if std::env::var_os("RACER_LIFECYCLE_STALL").is_none() {
                return;
            }
            let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config {
                stall: Duration::from_millis(700),
                drain: Duration::from_millis(100),
                quiesce: Duration::from_millis(100),
                ..Default::default()
            }));
            let stop = workers::StopHandle::supervised(life.clone());
            let _monitor = lifecycle::Monitor::start(life.clone(), stop.clone(), &SIGNAL).unwrap();
            let plan = workers::CpuPlan::discover(
                workers::Config {
                    shard_count: NonZeroUsize::new(1).unwrap(),
                },
                workers::WorkerCounts {
                    io_per_node: NonZeroUsize::new(1),
                    compute_per_node: NonZeroUsize::new(1),
                },
            )
            .unwrap();
            life.configure_workers(plan.io().len());
            let stall = Arc::new(AtomicBool::new(false));
            let flag = stall.clone();
            let health = life.clone();
            let workers = workers::Workers::start_supervised(plan, stop, move |placement| {
                let ring = crate::conformance::kernel_ring(4, Default::default())
                    .expect("real io_uring required");
                let listener =
                    Listener::bind("127.0.0.1:0".parse().unwrap(), NonZeroU32::new(8).unwrap())?;
                Ok(uring::Driver::new(
                    ring,
                    App {
                        server: Server::new(listener, stream(1, Duration::ZERO), Config::default()),
                        admitted: Arc::new(AtomicBool::new(false)),
                        stall: flag.clone(),
                        cleanup: None,
                        cleanup_started: None,
                        cleanup_done: Arc::new(AtomicBool::new(false)),
                    },
                    64,
                )?
                .with_lifecycle(health.clone(), placement.worker_id().0))
            })
            .unwrap();
            let end = Instant::now() + Duration::from_secs(2);
            while !life.healthy() {
                assert!(Instant::now() < end);
                thread::sleep(Duration::from_millis(5));
            }
            thread::sleep(Duration::from_secs(1));
            assert!(life.healthy(), "idle loop must remain healthy before fault");
            stall.store(true, Ordering::Release);
            workers.join().unwrap();
            panic!("blocked poll returned");
        }
    }

    #[test]
    fn lifecycle_drain_does_not_dispatch_pipelined_keepalive_request() {
        let world = World::new(623);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut f = Fixture::new(&world, Origin(stream(1, Duration::ZERO)), Config::default());
        f.server.handler_mut().0.hold = true;
        f.send(b"HEAD /first HTTP/1.1\r\nHost: h\r\n\r\nHEAD /second HTTP/1.1\r\nHost: h\r\n\r\n");
        f.until(|f| f.server.slots.front().is_some_and(|s| s.task.is_some()));
        f.server.begin_drain();
        f.server.handler_mut().0.hold = false;
        f.until(|f| f.eof);
        assert_eq!(
            f.bytes
                .windows(12)
                .filter(|w| *w == b"HTTP/1.1 200")
                .count(),
            1
        );
        assert_eq!(f.server.connections(), 0);
        f.finish();
        world.assert_clean();
    }

    #[test]
    fn progressing_stream_outlives_admission_and_default_30_seconds() {
        for (config, pause, length) in [
            (short(), Duration::from_millis(80), 8 * BUFFER_SIZE),
            (Config::default(), Duration::from_secs(2), 64 * 1024 * 1024),
        ] {
            let world = World::new(404);
            let _scope = world.enter();
            world.enable_scheduler();
            world.link_profile(65536, Some(1));
            let start = world.now();
            let admission = config.request_timeout;
            let mut f = Fixture::new(&world, stream(length, pause), config);
            f.send(GET);
            f.until(|f| f.eof);
            assert!(world.now() - start > admission);
            let end = f.bytes.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
            assert_eq!(f.bytes.len() - end, length);
            assert!(f.bytes[end..].iter().all(|&b| b == 201));
            f.finish();
            world.assert_clean();
        }
    }

    #[test]
    fn idle_slowloris_and_unpolled_handler_are_bounded() {
        for mode in 0..3 {
            let world = World::new(405);
            let _scope = world.enter();
            world.enable_scheduler();
            let mut handler = stream(1, Duration::ZERO);
            handler.hold = true;
            let mut f = Fixture::new(&world, handler, short());
            if mode == 1 {
                f.send(b"G");
            }
            if mode == 2 {
                f.send(GET);
            }
            f.until(|f| f.server.connections() == 1);
            if mode == 1 {
                f.until(|f| {
                    f.server
                        .slots
                        .front()
                        .unwrap()
                        .receiving
                        .as_ref()
                        .unwrap()
                        .first_byte_timeout
                        .is_none()
                });
            }
            let end = if mode == 2 {
                f.until(|f| f.server.handler().deadline.is_some());
                f.server.handler().deadline.as_ref().unwrap().get()
            } else {
                f.server.slots.front().unwrap().deadline
            };
            if mode == 1 {
                for byte in b"ET /" {
                    world.advance(Duration::from_millis(25));
                    f.send(&[*byte]);
                    assert_eq!(f.server.slots.front().unwrap().deadline, end);
                }
            }
            f.until(|f| f.eof);
            assert!(world.now() >= end);
            assert!(world.now() < end + Duration::from_millis(20));
            assert!(f.bytes.is_empty());
            f.finish();
            world.assert_clean();
        }
    }

    #[test]
    fn stalled_stream_and_peer_absolute_cap_and_shutdown() {
        for mode in 0..4 {
            let world = World::new(406);
            let _scope = world.enter();
            world.enable_scheduler();
            world.link_profile(65536, Some(1));
            let mut handler = stream(8 * BUFFER_SIZE, Duration::from_millis(20));
            if mode == 1 {
                handler.cap = Some(world.now() + Duration::from_millis(350));
            }
            let mut config = short();
            if mode == 1 {
                config.request_timeout = Duration::from_millis(500);
            }
            let mut f = Fixture::new(&world, handler, config);
            f.send(GET);
            f.until(|f| f.server.handler().sent >= BUFFER_SIZE);
            let deadline = f.server.handler().deadline.as_ref().unwrap().get();
            if mode == 0 || mode == 2 {
                f.server.handler_mut().hold = true;
                let work = f.tick();
                assert_eq!(work.deadline, Some(deadline));
                assert!(
                    !work.runnable,
                    "held handler must park with its transport deadline"
                );
            }
            let gate = if mode == 3 {
                // Hold actual socket sends, modelling client backpressure while the
                // handler continues polling. Submission/readiness is not progress.
                use crate::simulation::{Gate, Phase};
                let _node = world.scoped_node(Some(0));
                let fd = f
                    .server
                    .slots
                    .front()
                    .unwrap()
                    .control
                    .file
                    .simulation_id()
                    .unwrap();
                let endpoint = crate::socket::Address::Tcp("127.0.0.1:18914".parse().unwrap());
                world.tag_socket(fd, endpoint, "/stalled-send".into());
                world.socket_phase(fd, Phase::Request);
                Some(world.gate(Gate::new(
                    0,
                    endpoint,
                    "/stalled-send",
                    Phase::Request,
                    None,
                )))
            } else {
                None
            };
            if mode == 2 {
                // Shutdown closes a progressing/held stream immediately, not at its
                // renewable deadline. Ring cancellation still owns in-flight memory.
                f.server.shutdown(&mut f.ring).unwrap();
                f.until(|f| f.eof);
                assert!(world.now() < deadline);
            } else {
                f.until(|f| f.eof);
                assert!(world.now() >= deadline);
                assert!(world.now() < deadline + Duration::from_millis(20));
            }
            assert!(f.bytes.len() < 8 * BUFFER_SIZE);
            if let Some(gate) = gate {
                assert!(world.hits(gate) > 0);
            }
            f.finish();
            world.assert_clean();
        }
    }

    struct Origin(Stream);
    impl Handler for Origin {
        type Task = Task;
        fn start(&mut self, request: Request) -> Task {
            let tag = crate::conformance::etag(&vec![b'x'; self.0.length]);
            let headers = [
                ("ETag", tag.as_bytes()),
                ("Cache-Control", b"max-age=60".as_slice()),
            ];
            match request {
                Request::Head(r) => Task::Head(
                    r.respond(
                        ResponseHead::new(200, Some(self.0.length as u64), &headers).unwrap(),
                    )
                    .unwrap(),
                ),
                Request::Get(r) => {
                    let range = std::str::from_utf8(r.headers().get("range").unwrap())
                        .unwrap()
                        .strip_prefix("bytes=")
                        .unwrap();
                    let content_range = format!("bytes {range}/{}", self.0.length);
                    Task::Headers(
                        r.respond(
                            ResponseHead::new(
                                206,
                                Some(BUFFER_SIZE as u64),
                                &[
                                    ("ETag", tag.as_bytes()),
                                    ("Content-Range", content_range.as_bytes()),
                                ],
                            )
                            .unwrap(),
                        )
                        .unwrap(),
                    )
                }
            }
        }
        fn poll(
            &mut self,
            task: &mut Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<Completed>> {
            self.0.poll(task, ring, budget)
        }
    }

    #[test]
    fn production_handler_admits_later_pages_with_current_stream_budget() {
        for drain in [false, true] {
            let world = World::new(407);
            let _scope = world.enter();
            world.enable_scheduler();
            world.link_profile(65536, Some(4));
            let length = 5 * BUFFER_SIZE;
            let address = "127.0.0.1:18915".parse().unwrap();
            let mut origin = Server::new(
                Listener::bind(address, NonZeroU32::new(8).unwrap()).unwrap(),
                Origin(stream(length, Duration::ZERO)),
                Config::default(),
            );
            let backend =
                crate::handlers::Backend::new(&address.to_string(), "test-origin").unwrap();
            let size = 64 * 1024 * 1024;
            let mut slab = crate::allocator::Slab::simulated(
                crate::simulation::Disk::new(size),
                size,
                2,
                true,
            )
            .unwrap();
            let cache = crate::cache::tests::cache_from_slab(&mut slab, 2, Default::default());
            let handler = crate::handlers::Handler::new(cache, backend);
            let config = Config {
                request_timeout: Duration::from_secs(2),
                streaming_timeout: Duration::from_secs(2),
                ..Default::default()
            };
            let start = world.now();
            let mut f = Fixture::new(&world, handler, config);
            f.send(b"GET / HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n");
            let mut draining = false;
            for _ in 0..20_000 {
                f.server
                    .handler_mut()
                    .poll_background(&mut f.ring, 64)
                    .unwrap();
                origin.poll(&mut f.ring, 64).unwrap();
                f.tick();
                if drain && !draining && f.bytes.len() > BUFFER_SIZE {
                    f.server.begin_drain();
                    f.server.handler_mut().begin_drain();
                    draining = true;
                }
                if f.eof {
                    break;
                }
            }
            assert!(f.eof, "stream must finish");
            assert_eq!(draining, drain);
            assert!(world.now() - start > Duration::from_secs(2));
            let end = f.bytes.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
            assert_eq!(
                f.bytes.len() - end,
                length,
                "{}",
                String::from_utf8_lossy(&f.bytes[..end])
            );
            assert!(f.bytes[end..].iter().all(|&b| b == 201));
            assert!(
                world.counts()[30] > 0,
                "production file-backed SPLICE path must run"
            );
            origin.shutdown(&mut f.ring).unwrap();
            f.server.handler_mut().shutdown(&mut f.ring).unwrap();
            f.finish();
            drop(origin);
            world.assert_clean();
        }
    }

    #[test]
    fn recycled_idle_and_pipelined_admission_get_fresh_bounded_clocks() {
        for read_ahead in [false, true] {
            let world = World::new(408);
            let _scope = world.enter();
            world.enable_scheduler();
            let config = Config {
                idle_timeout: Duration::from_millis(100),
                request_timeout: Duration::from_millis(300),
                streaming_timeout: Duration::from_millis(50),
                ..Default::default()
            };
            let mut f = Fixture::new(&world, Origin(stream(1, Duration::ZERO)), config);
            let mut request = b"HEAD / HTTP/1.1\r\nHost: h\r\n\r\n".to_vec();
            if read_ahead {
                request.push(b'G');
            }
            f.send(&request);
            f.until(|f| {
                !f.bytes.is_empty()
                    && f.server
                        .slots
                        .front()
                        .is_some_and(|s| s.receiving.is_some())
            });
            let slot = f.server.slots.front().unwrap();
            let receiving = slot.receiving.as_ref().unwrap();
            assert_eq!(receiving.first_byte_timeout.is_none(), read_ahead);
            assert_eq!(
                receiving.connection.as_ref().unwrap().used,
                usize::from(read_ahead)
            );
            assert!(slot.response_deadline.is_none());
            let end = slot.deadline;
            let remaining = end - world.now();
            assert!(remaining > Duration::from_millis(if read_ahead { 250 } else { 50 }));
            f.until(|f| f.eof);
            assert!(world.now() >= end);
            assert!(world.now() < end + Duration::from_millis(20));
            f.finish();
            world.assert_clean();
        }
    }
}
