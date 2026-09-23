// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod pressure_tests {
    use super::*;

    mod inbound_pressure {
        use super::*;
        use crate::{http_client as client, uring, workers::Driver as _};

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
                let bind = || {
                    Listener::bind(
                        SocketAddr::from(([127, 0, 0, 1], 0)),
                        NonZeroU32::new(512).unwrap(),
                    )
                    .unwrap()
                };
                let origin = bind();
                let endpoint = origin.local_addr().unwrap();
                let mut addresses = Vec::new();
                let servers = (18961..18963)
                    .map(|_| {
                        let listener = bind();
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
                std::thread::sleep(Duration::from_micros(100));
                work
            }
            fn connect(&mut self, listener: usize) {
                self.clients.push(File::new(
                    std::net::TcpStream::connect(self.addresses[listener])
                        .unwrap()
                        .into(),
                ));
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
        use crate::uring;

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
    }
}
