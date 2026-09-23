// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod streaming_deadlines {
    use super::*;
    use crate::buffers::{BUFFER_SIZE, Key};

    struct Stream {
        length: usize,
        pause: Duration,
        hold: bool,
        cap: Option<Instant>,
        sent: usize,
        deadline: Option<Deadline>,
    }
    enum Task {
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

    const GET: &[u8] = b"GET / HTTP/1.1\r\nHost: h\r\n\r\n";

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
        }
        impl uring::Application for App {
            fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
                if self.stall.load(Ordering::Acquire) {
                    loop {
                        thread::park();
                    }
                }
                let work = self.server.poll(ring, budget)?;
                self.admitted
                    .store(self.server.handler().deadline.is_some(), Ordering::Release);
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
            for held in [false, true] {
                SIGNAL.store(false, Ordering::Relaxed);
                let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config {
                    drain: Duration::from_millis(600),
                    stall: Duration::from_secs(1),
                    ..Default::default()
                }));
                let stop = workers::StopHandle::supervised(life.clone());
                let _monitor =
                    lifecycle::Monitor::start(life.clone(), stop.clone(), &SIGNAL).unwrap();
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

                let address = TcpListener::bind("127.0.0.1:0")
                    .unwrap()
                    .local_addr()
                    .unwrap();
                let admitted = Arc::new(AtomicBool::new(false));
                let flag = admitted.clone();
                let health = life.clone();
                let workers = workers::Workers::start_supervised(plan, stop, move |placement| {
                    let ring = crate::conformance::kernel_ring(4, Default::default())
                        .expect("real io_uring required");
                    let listener = Listener::bind(address, NonZeroU32::new(8).unwrap())?;
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

                workers.join().unwrap();

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
}
