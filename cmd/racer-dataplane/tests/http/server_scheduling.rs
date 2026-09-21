mod pressure_tests {
    use super::*;

    mod inbound_pressure {
        use super::*;
        use crate::{http_client as client, simulation::World, uring, workers::Driver as _};

        struct Proxy(SocketAddr);
        enum Task {
            Fetch(Option<HeadRequest>, client::HeadExchange),
            Reply(SendingHeadHeaders),
        }
        impl Handler for Proxy {
            type Task = Task;
            fn start(&mut self, request: Request) -> Task {
                let Request::Head(request) = request else {
                    panic!("HEAD fixture")
                };
                let fetch = client::Connection::new(self.0, "cold-origin")
                    .unwrap()
                    .head(
                        client::Request::new("/cold", &[]).unwrap(),
                        request.deadline(),
                    )
                    .unwrap();
                Task::Fetch(Some(request), fetch)
            }
            fn poll(
                &mut self,
                task: &mut Task,
                ring: &mut Ring,
                budget: usize,
            ) -> io::Result<Progress<Completed>> {
                match task {
                    Task::Fetch(request, fetch) => match fetch.poll(ring, budget)? {
                        Progress::Pending(w) => Ok(Progress::Pending(w)),
                        Progress::Ready(response) => {
                            assert_eq!(response.status(), 200);
                            assert_eq!(response.content_length(), Some(3));
                            *task = Task::Reply(
                                request
                                    .take()
                                    .unwrap()
                                    .respond(ResponseHead::new(200, Some(3), &[])?.close())?,
                            );
                            Ok(pending(true, None))
                        }
                    },
                    Task::Reply(reply) => reply.poll(ring, budget),
                }
            }
        }
        struct Origin(usize);
        impl Handler for Origin {
            type Task = SendingHeadHeaders;
            fn start(&mut self, request: Request) -> Self::Task {
                self.0 += 1;
                assert_eq!(request.target(), "/cold");
                let Request::Head(request) = request else {
                    panic!()
                };
                request
                    .respond(ResponseHead::new(200, Some(3), &[]).unwrap().close())
                    .unwrap()
            }
            fn poll(
                &mut self,
                task: &mut Self::Task,
                ring: &mut Ring,
                budget: usize,
            ) -> io::Result<Progress<Completed>> {
                task.poll(ring, budget)
            }
        }
        struct App(Vec<Server<Proxy>>);
        impl uring::Application for App {
            fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
                let mut work = Work::default();
                for server in &mut self.0 {
                    work.merge(server.poll(ring, budget)?);
                }
                Ok(work)
            }
            fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
                for server in &mut self.0 {
                    server.shutdown(ring)?;
                }
                Ok(())
            }
        }
        struct Fixture {
            driver: uring::Driver<App>,
            remote: Ring,
            origin: Server<Origin>,
            addresses: Vec<SocketAddr>,
            clients: Vec<File>,
        }
        impl Fixture {
            fn new(files: u32) -> Self {
                let simulated = crate::simulation::current().is_some();
                let bind = |port| {
                    Listener::bind(
                        SocketAddr::from(([127, 0, 0, 1], if simulated { port } else { 0 })),
                        NonZeroU32::new(512).unwrap(),
                    )
                    .unwrap()
                };
                let origin = bind(18960);
                let endpoint = origin.local_addr().unwrap();
                let mut addresses = Vec::new();
                let servers = (18961..18963)
                    .map(|port| {
                        let listener = bind(port);
                        addresses.push(listener.local_addr().unwrap());
                        Server::new(listener, Proxy(endpoint), Config::default())
                    })
                    .collect();
                Self {
                    driver: uring::Driver::new(
                        crate::conformance::ring(
                            2,
                            uring::Config {
                                fixed_files: files,
                                ..Default::default()
                            },
                        ),
                        App(servers),
                        4096,
                    )
                    .unwrap(),
                    remote: crate::conformance::ring(2, Default::default()),
                    origin: Server::new(origin, Origin(0), Config::default()),
                    addresses,
                    clients: Vec::new(),
                }
            }
            fn tick(&mut self) -> Work {
                use uring::Application as _;
                self.remote.progress().unwrap();
                self.origin.poll(&mut self.remote, 4096).unwrap();
                let (app, ring) = self.driver.parts_mut();
                ring.progress().unwrap();
                let work = app.poll(ring, 4096).unwrap();
                if let Some(world) = crate::simulation::current() {
                    world.service_tick();
                } else {
                    std::thread::sleep(Duration::from_micros(100));
                }
                work
            }
            fn connect(&mut self, listener: usize) {
                let address = self.addresses[listener];
                let file = if let Some(world) = crate::simulation::current() {
                    let file = File::simulated(world.socket());
                    let mut ticket = self.remote.connect(file.clone().into(), address).unwrap();
                    let mut done = false;
                    for _ in 0..1000 {
                        self.tick();
                        if let Some(c) = self.remote.take_control(&mut ticket).unwrap() {
                            c.result.unwrap();
                            done = true;
                            break;
                        }
                    }
                    assert!(done, "connect watchdog");
                    file
                } else {
                    File::new(std::net::TcpStream::connect(address).unwrap().into())
                };
                self.clients.push(file);
                for _ in 0..8 {
                    self.tick();
                }
            }
            fn settled(&mut self) -> Work {
                for _ in 0..1000 {
                    let work = self.tick();
                    if !work.runnable {
                        return work;
                    }
                }
                panic!("idle inbound load must not spin");
            }
            fn request(&mut self, index: usize) {
                let mut send = self
                    .remote
                    .send_bytes(
                        self.clients[index].clone().into(),
                        b"HEAD / HTTP/1.1\r\nHost: ingress\r\nConnection: close\r\n\r\n"
                            .to_vec()
                            .into_boxed_slice(),
                    )
                    .unwrap();
                let mut recv = self
                    .remote
                    .recv_bytes(self.clients[index].clone().into(), vec![0; 8192].into())
                    .unwrap();
                let mut sent = false;
                let hits = self.origin.handler_mut().0;
                let end = Instant::now() + Duration::from_secs(5);
                for _ in 0..5000 {
                    assert!(Instant::now() < end);
                    self.tick();
                    if !sent && let Some(c) = self.remote.take_bytes(&mut send).unwrap() {
                        assert!(c.result.unwrap() > 0);
                        sent = true;
                    }
                    if let Some(c) = self.remote.take_bytes(&mut recv).unwrap() {
                        let n = c.result.unwrap();
                        assert!(
                            c.resource[..n].starts_with(b"HTTP/1.1 200 "),
                            "cold fetch failed: {:?}",
                            &c.resource[..n]
                        );
                        assert_eq!(self.origin.handler_mut().0, hits + 1);
                        return;
                    }
                }
                panic!("cold request watchdog");
            }
            fn finish(mut self) {
                for client in &self.clients {
                    client.shutdown_socket();
                }
                self.clients.clear();
                self.driver.shutdown().unwrap();
                self.origin.shutdown(&mut self.remote).unwrap();
                self.remote.shutdown().unwrap();
            }
        }

        fn flood(files: u32) {
            let mut f = Fixture::new(files);
            // Both listeners already own idle inbound connections before overload.
            let limit = files as usize - files.div_ceil(4).min(8) as usize;
            for i in 0..limit + 32 {
                f.connect(i % 2);
            }
            let work = f.settled();
            assert_eq!(
                f.driver
                    .application()
                    .0
                    .iter()
                    .map(Server::connections)
                    .sum::<usize>(),
                limit
            );
            assert!(
                f.driver
                    .application()
                    .0
                    .iter()
                    .all(|s| s.connections() > 0 && s.listener.as_ref().unwrap().accepted.is_some())
            );
            assert!(work.deadline.unwrap() <= crate::environment::now() + ADMISSION_RETRY);
            // Repeated polls at the same time cannot bypass the admission timer.
            if crate::simulation::current().is_some() {
                use uring::Application as _;
                let before = crate::simulation::current().unwrap().counts();
                let (app, ring) = f.driver.parts_mut();
                let work = app.poll(ring, 4096).unwrap();
                for _ in 0..32 {
                    let (app, ring) = f.driver.parts_mut();
                    let w = app.poll(ring, 4096).unwrap();
                    assert!(!w.runnable);
                    assert_eq!(w.deadline, work.deadline);
                }
                assert_eq!(crate::simulation::current().unwrap().counts(), before);
                f.driver.turn().unwrap();
                assert!(
                    f.driver.parked() && !f.driver.ready(),
                    "production driver must park"
                );
            }
            assert_eq!(f.origin.handler_mut().0, 0);
            f.request(0);
            f.request(1);
            // Release the original idle population; queued sockets across both listeners
            // must be admitted using reclaimed capacity, without waiting for idle expiry.
            for client in &f.clients[..limit] {
                client.shutdown_socket();
            }
            for _ in 0..100 {
                f.tick();
            }
            f.request(limit);
            f.request(limit + 1);
            f.settled();
            f.finish();
        }

        #[test]
        fn audit16_dst_multilistener_idle_pressure() {
            for files in [4, 256] {
                let run = || {
                    let world = World::new(516);
                    let _scope = world.enter();
                    world.enable_scheduler();
                    flood(files);
                    world.assert_clean();
                    world.digest()
                };
                assert_eq!(run(), run());
            }
        }

        #[test]
        fn audit16_dst_registration_pressure_parks_and_recovers() {
            let world = World::new(516);
            let _scope = world.enter();
            world.enable_scheduler();
            let mut f = Fixture::new(4);
            // One physically free slot is the outbound reserve, not inbound capacity.
            let pinned: Vec<_> = (0..3)
                .map(|_| {
                    f.driver
                        .ring_mut()
                        .register_file(File::simulated(world.socket()))
                        .unwrap()
                })
                .collect();
            f.connect(0);
            let work = f.settled();
            let receiving = f.driver.application().0[0].slots[0]
                .receiving
                .as_ref()
                .unwrap();
            assert!(receiving.connection.as_ref().unwrap().fixed.is_none());
            assert!(!work.runnable);
            assert!(receiving.retry_at.is_some());
            let original_deadline = receiving.deadline;
            f.driver.turn().unwrap();
            assert!(f.driver.parked() && !f.driver.ready());
            drop(pinned);
            for _ in 0..20 {
                f.tick();
            }
            let receiving = f.driver.application().0[0].slots[0]
                .receiving
                .as_ref()
                .unwrap();
            assert!(receiving.connection.as_ref().unwrap().fixed.is_some());
            assert_eq!(receiving.deadline, original_deadline);
            f.request(0);
            f.finish();
            world.assert_clean();
        }

        #[test]
        fn audit16_dst_admission_waits_for_target_collection() {
            let world = World::new(516);
            let _scope = world.enter();
            world.enable_scheduler();
            let mut f = Fixture::new(4);
            for _ in 0..3 {
                f.connect(0);
            }
            f.settled();
            let (app, ring) = f.driver.parts_mut();
            assert!(ring.admit_inbound().is_none());
            let slot = app.0[0].slots.pop_front().unwrap();
            let fixed = slot
                .receiving
                .as_ref()
                .unwrap()
                .connection
                .as_ref()
                .unwrap()
                .fixed
                .as_ref()
                .unwrap()
                .clone();
            let mut target = ring
                .recv_bytes(fixed.clone().into(), vec![0; 1].into())
                .unwrap();
            ring.progress().unwrap();
            world.service_tick();
            let mut cancel = ring.cancel(&target).unwrap();
            drop((slot, fixed));
            assert!(
                ring.admit_inbound().is_none(),
                "dropped connection is not quiescence"
            );
            let mut ack = false;
            for _ in 0..100 {
                ring.progress().unwrap();
                world.service_tick();
                if ring.take_cancel(&mut cancel).unwrap().is_some() {
                    ack = true;
                    break;
                }
            }
            assert!(ack);
            assert!(
                ring.admit_inbound().is_none(),
                "cancel acknowledgment cannot release target ownership"
            );
            // Cancel acknowledgment and target completion are independently
            // scheduled. Neither an ack nor an uncollected target CQE releases
            // admission, regardless of the server's per-connection poll quantum.
            let mut completion = None;
            for _ in 0..100 {
                ring.progress().unwrap();
                world.service_tick();
                assert!(ring.admit_inbound().is_none());
                completion = ring.take_bytes(&mut target).unwrap();
                if completion.is_some() {
                    break;
                }
            }
            let completion = completion.expect("target CQE");
            match completion.result {
                Ok(n) => assert_eq!(n, 0),
                Err(error) => assert_eq!(error.raw_os_error(), Some(libc::ECANCELED)),
            }
            ring.progress().unwrap();
            let admission = ring
                .admit_inbound()
                .expect("collected target permits reclaimed admission");
            drop(admission);
            f.finish();
            world.assert_clean();
        }

        #[test]
        fn audit16_dst_tiny_tables_and_request_capacity_bound() {
            let world = World::new(516);
            let _scope = world.enter();
            for (files, requests, expected) in [(0, 16, 0), (1, 16, 0), (2, 16, 1), (256, 8, 4)] {
                let mut ring = crate::conformance::ring(
                    1,
                    uring::Config {
                        fixed_files: files,
                        requests,
                        ..Default::default()
                    },
                );
                let leases: Vec<_> = (0..expected)
                    .map(|_| ring.admit_inbound().unwrap())
                    .collect();
                assert!(ring.admit_inbound().is_none());
                drop(leases);
                if expected != 0 {
                    assert!(ring.admit_inbound().is_some());
                }
                ring.shutdown().unwrap();
            }
            world.assert_clean();
        }

        #[test]
        fn audit16_kernel_multilistener_idle_pressure() {
            cache_responses::kernel_child(
                "http_server::pressure_tests::inbound_pressure::audit16_kernel_child",
                "RACER_AUDIT16_CHILD",
            );
        }
        #[test]
        #[ignore = "bounded subprocess helper"]
        fn audit16_kernel_child() {
            if std::env::var_os("RACER_AUDIT16_CHILD").is_some() {
                flood(256);
            }
        }
    }

    mod accept_pressure {
        use super::*;
        use crate::{http_client as client, simulation::World, uring, workers::Driver as _};

        mod tcp {
            use super::*;
            use std::{
                io::{BufRead, BufReader, Read},
                net::TcpStream,
                process::{Command, Stdio},
                sync::{
                    Arc,
                    atomic::{AtomicUsize, Ordering},
                },
                thread,
            };

            #[derive(Default)]
            struct Stats {
                pressure: AtomicUsize,
                requests: AtomicUsize,
                polls: AtomicUsize,
            }
            struct CountedHead(Arc<Stats>);
            impl Handler for CountedHead {
                type Task = SendingHeadHeaders;
                fn start(&mut self, request: Request) -> Self::Task {
                    self.0.requests.fetch_add(1, Ordering::Relaxed);
                    Head.start(request)
                }
                fn poll(
                    &mut self,
                    task: &mut Self::Task,
                    ring: &mut Ring,
                    budget: usize,
                ) -> io::Result<Progress<Completed>> {
                    task.poll(ring, budget)
                }
            }
            struct KernelApp(Server<CountedHead>);
            impl uring::Application for KernelApp {
                fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
                    let work = self.0.poll(ring, budget)?;
                    let stats = &self.0.handler().0;
                    stats.polls.fetch_add(1, Ordering::Relaxed);
                    // Observe the actual listener's completed resource errors, not client
                    // connection failures or /proc counts as a proxy for ACCEPT pressure.
                    stats.pressure.fetch_max(
                        self.0.listener.as_ref().unwrap().pressure_failures as usize,
                        Ordering::Relaxed,
                    );
                    Ok(work)
                }
                fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
                    self.0.shutdown(ring)
                }
            }
            fn socket(address: SocketAddr) -> TcpStream {
                let s = TcpStream::connect_timeout(&address, Duration::from_secs(3)).unwrap();
                s.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
                s.set_write_timeout(Some(Duration::from_secs(5))).unwrap();
                s
            }
            fn request(s: &mut TcpStream) {
                s.write_all(b"HEAD / HTTP/1.1\r\nHost: localhost\r\n\r\n")
                    .unwrap();
                let mut bytes = Vec::new();
                while !bytes.ends_with(b"\r\n\r\n") {
                    let mut byte = [0];
                    s.read_exact(&mut byte).unwrap();
                    bytes.push(byte[0]);
                    assert!(bytes.len() < 8192);
                }
                assert!(
                    bytes.starts_with(b"HTTP/1.1 200 "),
                    "{:?}",
                    String::from_utf8_lossy(&bytes)
                );
            }
            fn line(s: &mut BufReader<TcpStream>) -> String {
                let mut value = String::new();
                assert!(
                    s.read_line(&mut value).unwrap() > 0,
                    "child control channel closed"
                );
                value.trim().into()
            }
            fn wait(mut ready: impl FnMut() -> bool) {
                let end = Instant::now() + Duration::from_secs(5);
                while !ready() {
                    assert!(Instant::now() < end, "two-worker progress watchdog");
                    thread::sleep(Duration::from_millis(1));
                }
            }

            #[test]
            fn b09_kernel_child_nofile_recovery() {
                cache_responses::assert_child_selected(
                    "http_server::pressure_tests::accept_pressure::tcp::b09_nofile_child",
                );
                let control = TcpListener::bind("127.0.0.1:0").unwrap();
                control.set_nonblocking(true).unwrap();
                let mut child = Command::new(std::env::current_exe().unwrap())
                    .args([
                        "--exact",
                        "http_server::pressure_tests::accept_pressure::tcp::b09_nofile_child",
                        "--ignored",
                        "--nocapture",
                        "--test-threads=1",
                    ])
                    .env("RACER_B09_CHILD", control.local_addr().unwrap().to_string())
                    .stdout(Stdio::inherit())
                    .stderr(Stdio::inherit())
                    .spawn()
                    .unwrap();
                // Kill/reap on assertion failure as well as the normal bounded wait.
                struct ChildGuard<'a>(&'a mut std::process::Child);
                impl Drop for ChildGuard<'_> {
                    fn drop(&mut self) {
                        let _ = self.0.kill();
                        let _ = self.0.wait();
                    }
                }
                let guard = ChildGuard(&mut child);
                let end = Instant::now() + Duration::from_secs(10);
                let s = loop {
                    match control.accept() {
                        Ok((s, _)) => break s,
                        Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                            assert!(Instant::now() < end, "child startup watchdog");
                            assert!(
                                guard.0.try_wait().unwrap().is_none(),
                                "child exited at startup"
                            );
                            thread::sleep(Duration::from_millis(1));
                        }
                        Err(e) => panic!("{e}"),
                    }
                };
                s.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
                s.set_write_timeout(Some(Duration::from_secs(5))).unwrap();
                let mut channel = BufReader::new(s);
                let address = line(&mut channel).parse().unwrap();
                let mut existing: Vec<_> = (0..32)
                    .map(|_| {
                        let mut s = socket(address);
                        request(&mut s);
                        s
                    })
                    .collect();
                channel.get_mut().write_all(b"limit\n").unwrap();
                assert!(line(&mut channel).starts_with("limited "));
                // All load descriptors belong to the unrestricted parent. Only the child's
                // accepts consume its remaining soft-nofile allowance; no host ENFILE attempt.
                let load: Vec<_> = (0..128).map(|_| socket(address)).collect();
                assert_eq!(line(&mut channel), "pressure");
                for s in &mut existing {
                    request(s);
                }
                drop(load);
                drop(existing);
                thread::sleep(Duration::from_millis(50));
                for _ in 0..64 {
                    request(&mut socket(address));
                }
                channel.get_mut().write_all(b"done\n").unwrap();
                assert_eq!(line(&mut channel), "recovered");
                wait(|| guard.0.try_wait().unwrap().is_some());
                assert!(guard.0.wait().unwrap().success());
            }

            #[test]
            #[ignore = "bounded subprocess; lowers only this child's soft RLIMIT_NOFILE"]
            fn b09_nofile_child() {
                let Some(control) = std::env::var_os("RACER_B09_CHILD") else {
                    return;
                };
                let mut channel =
                    BufReader::new(socket(control.to_str().unwrap().parse().unwrap()));
                let baseline_fds = std::fs::read_dir("/proc/self/fd").unwrap().count();
                let address = TcpListener::bind("127.0.0.1:0")
                    .unwrap()
                    .local_addr()
                    .unwrap();
                // Independent per-worker shared observations; production Workers retains the
                // real fatal-error fanout/stop behavior and production Driver does the parking.
                let worker_stats: Arc<Vec<Arc<Stats>>> =
                    Arc::new((0..2).map(|_| Arc::new(Stats::default())).collect());
                let observed = worker_stats.clone();
                let workers = crate::workers::Workers::start(
                    crate::workers::Config {
                        shard_count: NonZeroUsize::new(2).unwrap(),
                    },
                    move |placement| {
                        let ring = crate::conformance::kernel_ring(2, Default::default())
                            .expect("real io_uring required");
                        let listener = Listener::bind(address, NonZeroU32::new(256).unwrap())?;
                        let stats = observed[placement.worker.0].clone();
                        uring::Driver::new(
                            ring,
                            KernelApp(Server::new(listener, CountedHead(stats), Config::default())),
                            64,
                        )
                    },
                )
                .unwrap();
                assert_eq!(workers.placements().len(), 2);
                writeln!(channel.get_mut(), "{address}").unwrap();
                assert_eq!(line(&mut channel), "limit");
                assert!(
                    worker_stats
                        .iter()
                        .all(|s| s.requests.load(Ordering::Relaxed) > 0),
                    "both workers must serve before limiting"
                );
                let max_fd = std::fs::read_dir("/proc/self/fd")
                    .unwrap()
                    .map(|e| {
                        e.unwrap()
                            .file_name()
                            .to_str()
                            .unwrap()
                            .parse::<u64>()
                            .unwrap()
                    })
                    .max()
                    .unwrap();
                let mut limit = libc::rlimit {
                    rlim_cur: 0,
                    rlim_max: 0,
                };
                // SAFETY: valid output pointer; affects this subprocess only, after startup.
                assert_eq!(
                    unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut limit) },
                    0
                );
                let hard = limit.rlim_max;
                assert!(max_fd + 40 < limit.rlim_cur);
                limit.rlim_cur = max_fd + 40;
                assert_eq!(unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &limit) }, 0);
                writeln!(channel.get_mut(), "limited {}", limit.rlim_cur).unwrap();
                wait(|| {
                    worker_stats
                        .iter()
                        .all(|s| s.pressure.load(Ordering::Relaxed) >= 3)
                });
                let polls: Vec<_> = worker_stats
                    .iter()
                    .map(|s| s.polls.load(Ordering::Relaxed))
                    .collect();
                thread::sleep(Duration::from_millis(300));
                for (i, s) in worker_stats.iter().enumerate() {
                    let delta = s.polls.load(Ordering::Relaxed) - polls[i];
                    assert!(
                        delta < 100,
                        "accept pressure spun worker {i}: {delta} polls in 300ms"
                    );
                    eprintln!(
                        "B09 kernel worker={i}: {delta} application polls in 300ms sustained pressure"
                    );
                }
                let before: Vec<_> = worker_stats
                    .iter()
                    .map(|s| s.requests.load(Ordering::Relaxed))
                    .collect();
                writeln!(channel.get_mut(), "pressure").unwrap();
                assert_eq!(line(&mut channel), "done");
                assert_eq!(
                    unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut limit) },
                    0
                );
                assert_eq!(
                    limit.rlim_cur,
                    max_fd + 40,
                    "recovery must not raise the soft limit"
                );
                assert_eq!(limit.rlim_max, hard);
                for (i, s) in worker_stats.iter().enumerate() {
                    assert!(s.requests.load(Ordering::Relaxed) > before[i]);
                    eprintln!(
                        "B09 kernel pid={} worker={i}: pressure_streak={} requests={} polls={} soft_nofile={} (unchanged through recovery)",
                        std::process::id(),
                        s.pressure.load(Ordering::Relaxed),
                        s.requests.load(Ordering::Relaxed),
                        s.polls.load(Ordering::Relaxed),
                        limit.rlim_cur
                    );
                }
                workers.stop_handle().request_stop();
                workers.join().unwrap();
                assert_eq!(
                    std::fs::read_dir("/proc/self/fd").unwrap().count(),
                    baseline_fds,
                    "child must release worker rings, listeners and accepted sockets"
                );
                drop(
                    TcpListener::bind(address)
                        .expect("listener address must be released after join"),
                );
                writeln!(channel.get_mut(), "recovered").unwrap();
            }
        }

        struct Head;
        impl Handler for Head {
            type Task = SendingHeadHeaders;
            fn start(&mut self, request: Request) -> Self::Task {
                let Request::Head(r) = request else {
                    panic!("HEAD only")
                };
                r.respond(ResponseHead::new(200, Some(3), &[]).unwrap())
                    .unwrap()
            }
            fn poll(
                &mut self,
                task: &mut Self::Task,
                ring: &mut Ring,
                budget: usize,
            ) -> io::Result<Progress<Completed>> {
                task.poll(ring, budget)
            }
        }
        struct App {
            server: Server<Head>,
            work: Work,
            observed: Instant,
        }
        impl uring::Application for App {
            fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work> {
                self.observed = crate::environment::now();
                self.work = self.server.poll(ring, budget)?;
                Ok(self.work)
            }
            fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()> {
                self.server.shutdown(ring)
            }
        }
        type Driver = uring::Driver<App>;
        fn setup(world: &World) -> (Driver, SocketAddr) {
            let address = "127.0.0.1:18909".parse().unwrap();
            let listener = Listener::bind(address, NonZeroU32::new(32).unwrap()).unwrap();
            let ring = crate::conformance::ring(2, Default::default());
            (
                Driver::new(
                    ring,
                    App {
                        server: Server::new(listener, Head, Config::default()),
                        work: Work::default(),
                        observed: world.now(),
                    },
                    64,
                )
                .unwrap(),
                address,
            )
        }
        fn tick(world: &World, driver: &mut Driver) {
            if driver.ready() {
                driver
                    .turn()
                    .expect("accept pressure must not escape into worker failure");
            }
            world.service_tick();
        }
        fn head(
            world: &World,
            driver: &mut Driver,
            connection: client::Connection,
        ) -> client::Connection {
            let end = world.now() + Duration::from_secs(2);
            let mut request = connection
                .head(client::Request::new("/", &[]).unwrap(), end)
                .unwrap();
            for _ in 0..2000 {
                match request.poll(driver.ring_mut(), 64).unwrap() {
                    Progress::Ready(response) => {
                        assert_eq!(response.status(), 200);
                        assert_eq!(response.content_length(), Some(3));
                        return response.recycle().unwrap();
                    }
                    Progress::Pending(_) => tick(world, driver),
                }
            }
            panic!("HTTP progress deadline");
        }
        fn pressure(world: &World, driver: &mut Driver, errno: i32, delay: u64) -> Instant {
            world.fail_next_errno(crate::uring_sys::abi::ACCEPT, errno);
            for _ in 0..1100 {
                tick(world, driver);
                let app = driver.application();
                if world.fault_fired()
                    && let Some(deadline) = app.work.deadline
                    && deadline <= app.observed + Duration::from_secs(1)
                {
                    assert_eq!(deadline - app.observed, Duration::from_millis(delay));
                    assert!(!app.work.runnable);
                    let counts = world.counts();
                    // Unrelated wakes / repeated polls cannot restart or bypass the timer.
                    for _ in 0..32 {
                        driver.turn().unwrap();
                        assert_eq!(driver.application().work.deadline, Some(deadline));
                        assert!(!driver.application().work.runnable);
                    }
                    assert_eq!(world.counts(), counts);
                    assert!(
                        driver.parked() && !driver.ready(),
                        "driver must park during backoff"
                    );
                    assert_eq!(driver.deadline(), Some(deadline));
                    return deadline;
                }
            }
            panic!("selected ACCEPT fault did not produce bounded backoff");
        }
        fn run(errno: i32, shutdown: bool) -> [u8; 32] {
            let world = World::new(509);
            let _scope = world.enter();
            world.enable_scheduler();
            let (mut driver, address) = setup(&world);
            let connection = head(
                &world,
                &mut driver,
                client::Connection::new(address, "localhost").unwrap(),
            );
            let mut deadline = world.now();
            for (i, delay) in [10, 20, 40, 80, 160, 320, 640, 1000, 1000]
                .into_iter()
                .enumerate()
            {
                deadline = pressure(&world, &mut driver, errno, delay);
                if i == 8 {
                    break;
                }
                let counts = world.counts();
                while world.now() + Duration::from_millis(1) < deadline {
                    tick(&world, &mut driver);
                }
                assert_eq!(world.counts(), counts, "no ACCEPT retry before deadline");
                assert!(!driver.ready());
                world.service_tick();
                assert!(driver.ready(), "timer must wake driver");
            }
            // Still in the last pressure interval: already accepted keep-alive work runs.
            let connection = head(&world, &mut driver, connection);
            assert!(
                world.now() < deadline,
                "existing HTTP must finish during accept backoff"
            );
            if !shutdown {
                // No more injected faults: a fresh connection recovers at the next retry.
                drop(head(
                    &world,
                    &mut driver,
                    client::Connection::new(address, "localhost").unwrap(),
                ));
                // A successful accept resets the next pressure delay, even after the cap.
                pressure(&world, &mut driver, errno, 10);
            } else {
                // Re-enter backoff then shut down without advancing to its deadline.
                pressure(&world, &mut driver, errno, 1000);
            }
            drop(connection);
            driver.shutdown().unwrap();
            drop(driver);
            world.assert_clean();
            eprintln!(
                "B09 DST errno={errno} shutdown={shutdown}: 9 repeated faults, capped deadlines, parked driver, existing HTTP progress, cleanup"
            );
            world.digest()
        }
        #[test]
        fn b09_dst_emfile_recovery_and_shutdown_replay() {
            for shutdown in [false, true] {
                assert_eq!(run(libc::EMFILE, shutdown), run(libc::EMFILE, shutdown));
            }
        }
        #[test]
        fn b09_dst_enfile_recovery_and_shutdown_replay() {
            for shutdown in [false, true] {
                assert_eq!(run(libc::ENFILE, shutdown), run(libc::ENFILE, shutdown));
            }
        }
        #[test]
        fn b09_dst_other_accept_errors_remain_fatal() {
            for errno in [libc::EIO, libc::EBADF, libc::EINVAL] {
                let world = World::new(509);
                let _scope = world.enter();
                world.enable_scheduler();
                let (mut driver, _) = setup(&world);
                world.fail_next_errno(crate::uring_sys::abi::ACCEPT, errno);
                let mut hit = false;
                for _ in 0..100 {
                    if driver.ready()
                        && let Err(error) = driver.turn()
                    {
                        assert_eq!(error.raw_os_error(), Some(errno));
                        assert!(world.fault_fired());
                        hit = true;
                        break;
                    }
                    world.service_tick();
                }
                assert!(hit, "fatal ACCEPT control must fire");
                driver.shutdown().unwrap();
                drop(driver);
                world.assert_clean();
            }
        }
    }
}

mod scheduler_tests {
    use super::*;
    use crate::simulation::World;

    struct Script {
        polls: Vec<String>,
        early: Instant,
        late: Instant,
    }
    impl Handler for Script {
        type Task = (Request, usize);

        fn start(&mut self, request: Request) -> Self::Task {
            (request, 0)
        }

        fn poll(
            &mut self,
            (request, step): &mut Self::Task,
            _: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<Completed>> {
            assert_eq!(budget, 1);
            self.polls.push(request.target().to_owned());
            *step += 1;
            Ok(match request.target() {
                "/hot" => pending(true, None),
                "/settle" if *step == 1 => pending(true, Some(self.early)),
                "/expire" => {
                    request.cap_deadline(crate::environment::now());
                    pending(true, None)
                }
                _ => pending(false, Some(self.late)),
            })
        }
    }

    struct Fixture {
        server: Server<Script>,
        ring: Ring,
        peers: Vec<File>,
    }
    impl Fixture {
        fn new(world: &World, targets: &[&str], parse: bool) -> Self {
            let address = "127.0.0.1:18919".parse().unwrap();
            let listener = Listener::bind(address, NonZeroU32::new(8).unwrap()).unwrap();
            let mut server = Server::new(
                listener,
                Script {
                    polls: Vec::new(),
                    early: world.now() + Duration::from_secs(1),
                    late: world.now() + Duration::from_secs(2),
                },
                Config {
                    max_connections: NonZeroUsize::new(targets.len()).unwrap(),
                    ..Config::default()
                },
            );
            let mut ring = crate::conformance::ring(4, Default::default());
            let mut peers = Vec::new();
            for target in targets {
                let peer = File::simulated(world.socket());
                let mut connect = ring.connect(peer.clone().into(), address).unwrap();
                drive(world, &mut ring, |ring| {
                    ring.take_control(&mut connect)
                        .unwrap()
                        .map(|c| c.result.unwrap())
                });
                let mut connection = drive(world, &mut ring, |ring| {
                    match server
                        .listener
                        .as_mut()
                        .unwrap()
                        .poll_accept(ring, 1)
                        .unwrap()
                    {
                        Progress::Ready(connection) => Some(connection),
                        Progress::Pending(_) => None,
                    }
                });
                // Deterministic read-ahead: registration and parsing are still real
                // receive state transitions, but no TCP packet timing is involved.
                let bytes = format!("GET {target} HTTP/1.1\r\nHost: h\r\n\r\n");
                connection.input.as_mut().unwrap()[..bytes.len()].copy_from_slice(bytes.as_bytes());
                connection.used = bytes.len();
                let mut slot = Slot::new(connection, &server.config).unwrap();
                if parse {
                    drive(world, &mut ring, |ring| {
                        server.poll_slot(&mut slot, ring).unwrap();
                        slot.task.as_ref().map(|_| ())
                    });
                }
                server.slots.push_back(slot);
                peers.push(peer);
            }
            Self {
                server,
                ring,
                peers,
            }
        }

        fn poll(&mut self, budget: usize) -> Work {
            self.server.poll(&mut self.ring, budget).unwrap()
        }

        fn finish(mut self, world: &World) {
            self.server.shutdown(&mut self.ring).unwrap();
            self.peers.clear();
            self.ring.shutdown().unwrap();
            drop(self);
            world.assert_clean();
        }
    }

    fn drive<T>(world: &World, ring: &mut Ring, mut poll: impl FnMut(&mut Ring) -> Option<T>) -> T {
        for _ in 0..1000 {
            ring.progress().unwrap();
            if let Some(value) = poll(ring) {
                return value;
            }
            world.service_tick();
        }
        panic!("scheduler fixture stalled");
    }

    #[test]
    fn immediate_receive_and_handler_transitions_settle_in_one_visit() {
        let world = World::new(605);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut f = Fixture::new(&world, &["/settle"], false);
        let work = f.poll(1 + SLOT_QUANTUM);
        assert_eq!(f.server.handler.polls, ["/settle", "/settle"]);
        assert_eq!(f.server.sweep_left, 0);
        // Consumed runnable work and its old timer must not force another sweep.
        assert!(!work.runnable);
        assert_eq!(work.deadline, Some(f.server.handler.late));
        f.finish(&world);
    }

    #[test]
    fn hot_slot_yields_to_cold_slot_at_quantum_and_caller_budget_boundaries() {
        for budget in 0..=1 + SLOT_QUANTUM + 2 {
            let world = World::new(606);
            let _scope = world.enter();
            world.enable_scheduler();
            let mut f = Fixture::new(&world, &["/hot", "/cold"], true);
            let work = f.poll(budget);
            let hot = budget.saturating_sub(1).min(SLOT_QUANTUM);
            let cold = usize::from(budget > 1 + SLOT_QUANTUM);
            assert_eq!(f.server.handler.polls.len(), hot + cold, "budget={budget}");
            assert!(f.server.handler.polls[..hot].iter().all(|p| p == "/hot"));
            assert!(work.runnable);
            assert_eq!(
                f.server.sweep_left,
                if budget == 0 {
                    3
                } else {
                    2 - usize::from(hot != 0) - cold
                }
            );
            // Exhausting a small budget requeues the hot slot; it does not keep
            // unused quantum ahead of the cold slot on the next call.
            if hot != 0 && cold == 0 {
                f.poll(1);
                assert_eq!(f.server.handler.polls.last().unwrap(), "/cold");
                assert_eq!(f.server.sweep_left, 0);
            }
            f.finish(&world);
        }
    }

    #[test]
    fn budget_one_rotates_after_each_slot_poll() {
        let world = World::new(610);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut f = Fixture::new(&world, &["/hot", "/cold"], true);
        for sweep in 1..=3 {
            assert!(f.poll(1).runnable); // admission
            assert_eq!(f.server.handler.polls.len(), (sweep - 1) * 2);
            assert!(f.poll(1).runnable); // hot
            assert_eq!(f.server.handler.polls.len(), sweep * 2 - 1);
            assert!(f.poll(1).runnable); // cold, with hot work retained
            assert_eq!(f.server.handler.polls.len(), sweep * 2);
            assert_eq!(f.server.handler.polls.last().unwrap(), "/cold");
            assert_eq!(f.server.sweep_left, 0);
        }
        f.finish(&world);
    }

    #[test]
    fn sleepers_cost_one_poll_and_keep_deadlines_across_partial_sweeps() {
        let world = World::new(607);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut f = Fixture::new(&world, &["/cold", "/settle"], true);
        let work = f.poll(3); // admission, sleeping slot, first immediate step
        assert_eq!(f.server.handler.polls, ["/cold", "/settle"]);
        assert_eq!(f.server.sweep_left, 0);
        assert!(work.runnable);
        assert_eq!(work.deadline, Some(f.server.handler.early));
        let work = f.poll(2); // next sweep: admission and sleeping slot
        assert!(work.runnable);
        assert_eq!(f.server.sweep_left, 1);
        assert_eq!(work.deadline, Some(f.server.handler.late));
        let before = f.server.handler.polls.len();
        assert_eq!(f.poll(0).deadline, work.deadline);
        assert_eq!(f.server.handler.polls.len(), before);
        let work = f.poll(1);
        assert!(!work.runnable);
        assert_eq!(work.deadline, Some(f.server.handler.late));
        f.finish(&world);
    }

    #[test]
    fn deadline_is_checked_between_immediate_handler_steps() {
        let world = World::new(608);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut f = Fixture::new(&world, &["/expire", "/cold"], true);
        let work = f.poll(4); // admission, handler, expired slot, cold slot
        assert_eq!(f.server.handler.polls, ["/expire", "/cold"]);
        assert_eq!(f.server.connections(), 1);
        assert_eq!(f.server.sweep_left, 0);
        assert!(work.runnable); // retry admission after closing the expired slot
        assert_eq!(work.deadline, Some(f.server.handler.late));
        f.finish(&world);
    }

    #[test]
    fn completion_during_partial_sweep_requires_rescan_before_parking() {
        let world = World::new(609);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut f = Fixture::new(&world, &["/settle", "/cold"], true);
        let work = f.poll(3); // admission and a slot that settles within its quantum
        assert!(work.runnable);
        assert_eq!(f.server.sweep_left, 1);
        let epoch = f.ring.completion_epoch();
        let mut send = f
            .ring
            .send_bytes(f.peers[0].clone().into(), b"x".as_slice().into())
            .unwrap();
        drive(&world, &mut f.ring, |ring| {
            ring.take_bytes(&mut send)
                .unwrap()
                .map(|c| c.result.unwrap())
        });
        assert_ne!(f.ring.completion_epoch(), epoch);
        assert!(f.poll(1).runnable);
        assert_eq!(f.server.sweep_left, 0);
        let work = f.poll(3);
        assert!(!work.runnable);
        assert_eq!(work.deadline, Some(f.server.handler.late));
        assert_eq!(
            f.server.handler.polls,
            ["/settle", "/settle", "/cold", "/settle", "/cold"]
        );
        f.finish(&world);
    }
}
