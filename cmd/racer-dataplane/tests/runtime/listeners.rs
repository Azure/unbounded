// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::staging_tests::{Worker, address, volumes};
use super::*;
use crate::control::tests::{fixture, prepare_snapshot};
use crate::simulation::World;

mod tcp {
    use super::*;
    use std::{
        io::{Read, Write},
        net::{TcpListener, TcpStream},
        sync::{
            atomic::{AtomicBool, Ordering},
            mpsc,
        },
        thread,
    };

    fn headers(socket: &mut TcpStream) -> String {
        let mut bytes = Vec::new();
        while !bytes.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            socket.read_exact(&mut byte).unwrap();
            bytes.push(byte[0]);
            assert!(bytes.len() < 16384);
        }
        String::from_utf8(bytes).unwrap()
    }

    fn connect(a: SocketAddr) -> TcpStream {
        let timeout = Duration::from_secs(5);
        let s = TcpStream::connect_timeout(&a, timeout).unwrap();
        s.set_read_timeout(Some(timeout)).unwrap();
        s.set_write_timeout(Some(timeout)).unwrap();
        s
    }

    fn request(a: SocketAddr, peer: Option<&str>) -> (u16, Vec<u8>) {
        let mut s = connect(a);
        if let Some(wire) = peer {
            write!(s, "GET / HTTP/1.1\r\nHost: localhost\r\n").unwrap();
            for (name, value) in peer_headers(wire) {
                write!(s, "{name}: {value}\r\n").unwrap();
            }
            write!(s, "Connection: close\r\n\r\n").unwrap();
        } else {
            write!(
                s,
                "HEAD /fresh HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
            )
            .unwrap();
        }
        let h = headers(&mut s);
        let code = h.split_whitespace().nth(1).unwrap().parse().unwrap();
        let mut body = Vec::new();
        if peer.is_some() {
            s.read_to_end(&mut body).unwrap();
        }
        (code, body)
    }

    // The actual kernel listener inode set catches competing SO_REUSEPORT sockets,
    // independently of runtime map counts and probabilistic connection selection.
    fn listener_inodes(a: SocketAddr) -> std::collections::BTreeSet<String> {
        std::fs::read_to_string("/proc/net/tcp")
            .unwrap()
            .lines()
            .skip(1)
            .filter_map(|line| {
                let f: Vec<_> = line.split_whitespace().collect();
                (f[3] == "0A" && f[1].ends_with(&format!(":{:04X}", a.port())))
                    .then(|| f[9].to_owned())
            })
            .collect()
    }

    enum Command {
        Inspect(mpsc::Sender<(usize, u64, Vec<u64>, usize, u64)>),
        Stop,
    }

    #[test]
    fn b08_kernel_two_workers_fresh_connections_and_held_old_peer() {
        let a = address();
        let backend = TcpListener::bind("127.0.0.1:0").unwrap();
        backend.set_nonblocking(true).unwrap();
        let (trust, mut config) = peer_fixture(a, backend.local_addr().unwrap());
        let stop = Arc::new(AtomicBool::new(false));
        let release = Arc::new(AtomicBool::new(false));
        let (held_tx, held_rx) = mpsc::channel();
        let origin_stop = stop.clone();
        let origin_release = release.clone();
        let origin = thread::spawn(move || {
            let end = Instant::now() + Duration::from_secs(20);
            let mut tasks = Vec::new();
            while !origin_stop.load(Ordering::Acquire) {
                assert!(Instant::now() < end);
                match backend.accept() {
                    Ok((mut socket, _)) => {
                        let release = origin_release.clone();
                        let held = held_tx.clone();
                        tasks.push(thread::spawn(move || {
                            socket.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
                            socket.set_write_timeout(Some(Duration::from_secs(5))).unwrap();
                            let h = headers(&mut socket);
                            if h.contains("/held") {
                                held.send(()).unwrap();
                                while !release.load(Ordering::Acquire) {
                                    assert!(Instant::now() < end);
                                    thread::sleep(Duration::from_millis(1));
                                }
                            }
                            write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"abc")).unwrap();
                            if h.starts_with("GET ") { socket.write_all(b"abc").unwrap(); }
                        }));
                    }
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        thread::sleep(Duration::from_millis(1))
                    }
                    Err(e) => panic!("{e}"),
                }
            }
            for t in tasks {
                t.join().unwrap();
            }
        });
        let updates = Arc::new(Updates::default());
        let (ready_tx, ready_rx) = mpsc::channel();
        let mut workers = Vec::new();
        for id in 0..2 {
            let updates = updates.clone();
            let ready = ready_tx.clone();
            let (tx, rx) = mpsc::channel();
            let join = thread::spawn(move || {
                let mut ring = crate::control::tests::ring().expect("real io_uring required");
                let mut node = volumes(&ring, &updates, id);
                node.cache.borrow_mut().set_metrics(ring.metrics().clone());
                ready.send(()).unwrap();
                let end = Instant::now() + Duration::from_secs(20);
                loop {
                    assert!(Instant::now() < end, "I/O worker watchdog");
                    ring.progress().unwrap();
                    let work = node.poll(&mut ring, 64).unwrap();
                    match rx.try_recv() {
                        Ok(Command::Stop) => break,
                        Ok(Command::Inspect(reply)) => {
                            let s = node
                                .servers
                                .get(&a)
                                .or_else(|| node.retired.get(&a).map(|(_, s)| s))
                                .unwrap();
                            reply
                                .send((
                                    owned(&node, a),
                                    s.handler().current._config.config.revision,
                                    s.handler()
                                        .draining
                                        .iter()
                                        .map(|g| g._config.config.revision)
                                        .collect(),
                                    s.connections(),
                                    ring.metrics().values()[0],
                                ))
                                .unwrap();
                        }
                        Err(mpsc::TryRecvError::Empty) => {}
                        Err(e) => panic!("{e}"),
                    }
                    if !work.runnable {
                        thread::sleep(Duration::from_micros(100));
                    }
                }
                node.shutdown(&mut ring).unwrap();
            });
            workers.push((tx, join));
        }
        for _ in 0..2 {
            ready_rx.recv_timeout(Duration::from_secs(5)).unwrap();
        }
        let activate = |config: crate::control::proto::Snapshot| {
            let revision = config.revision;
            updates.publish(prepare_snapshot(&trust, config)).unwrap();
            let end = Instant::now() + Duration::from_secs(3);
            while updates.status()["activeRevision"] != revision {
                assert!(
                    Instant::now() < end,
                    "activation stalled: {}",
                    updates.status()
                );
                thread::sleep(Duration::from_millis(1));
            }
        };
        activate(config.clone());
        let original = listener_inodes(a);
        assert_eq!(original.len(), 2);
        let old_config = config.clone();
        let held_wire = peer_wire(&config, "/held");
        let held = thread::spawn(move || request(a, Some(&held_wire)));
        held_rx.recv_timeout(Duration::from_secs(3)).unwrap();
        // Old peer is blocked in an actual backend HEAD, with its accepted task live.
        config.revision = 2;
        config.volumes[0].topology.as_mut().unwrap().epoch = 2;
        activate(config.clone());
        let prior_config = config.clone();
        let volume = config.volumes.pop().unwrap();
        config.revision = 3;
        activate(config.clone());
        assert_eq!(listener_inodes(a), original);
        assert_eq!(request(a, None).0, 409);
        config.revision = 4;
        config.volumes.push(volume);
        config.volumes[0].topology.as_mut().unwrap().epoch = 4;
        activate(config.clone());
        assert_eq!(
            listener_inodes(a),
            original,
            "B08 created competing retired+new reuseport listeners"
        );
        let mut accepted = 0;
        for (tx, _) in &workers {
            let (reply, rx) = mpsc::channel();
            tx.send(Command::Inspect(reply)).unwrap();
            let (owners, revision, draining, connections, _) =
                rx.recv_timeout(Duration::from_secs(3)).unwrap();
            assert_eq!((owners, revision), (1, 4));
            assert_eq!(draining, vec![1, 2]);
            accepted += connections;
        }
        assert!(
            accepted > 0,
            "reclaimed servers must retain held accepted work"
        );
        let mut successes = 0;
        for _ in 0..512 {
            assert_eq!(request(a, None).0, 200, "fresh TCP after activation");
            successes += 1;
        }
        for c in [&old_config, &prior_config, &config] {
            for n in 0..16 {
                let wire = peer_wire(c, &format!("/peer-{}-{n}", c.revision));
                let (status, body) = request(a, Some(&wire));
                assert_eq!(status, 200);
                assert!(crate::metadata::Metadata::from_bytes(&body).is_ok());
            }
        }
        release.store(true, Ordering::Release);
        let (status, body) = held.join().unwrap();
        assert_eq!(status, 200);
        assert!(crate::metadata::Metadata::from_bytes(&body).is_ok());
        // Churn the same real address again while old generations still drain.
        for _ in 0..3 {
            let mut volume = config.volumes.pop().unwrap();
            config.revision += 1;
            activate(config.clone());
            config.revision += 1;
            volume.topology.as_mut().unwrap().epoch = config.revision;
            config.volumes.push(volume);
            activate(config.clone());
            assert_eq!(listener_inodes(a), original);
            for _ in 0..128 {
                assert_eq!(request(a, None).0, 200);
                successes += 1;
            }
        }
        eprintln!(
            "B08 TCP: 2 I/O threads, {successes} fresh ingress 200, 48 old/current peer 200, held old peer completed, same two kernel listener inodes across four readds"
        );
        for (id, (tx, _)) in workers.iter().enumerate() {
            let (reply, rx) = mpsc::channel();
            tx.send(Command::Inspect(reply)).unwrap();
            let (owners, revision, draining, _, ingress) =
                rx.recv_timeout(Duration::from_secs(3)).unwrap();
            assert_eq!((owners, revision), (1, config.revision));
            assert!(draining.len() <= MAX_DRAINING);
            assert!(
                ingress > 0,
                "both kernel workers must actually serve ingress"
            );
            eprintln!(
                "B08 TCP worker {id}: {ingress} dispatched ingress, one listener, revision {revision}, draining={draining:?}"
            );
        }
        for (tx, _) in &workers {
            tx.send(Command::Stop).unwrap();
        }
        for (_, t) in workers {
            t.join().unwrap();
        }
        stop.store(true, Ordering::Release);
        origin.join().unwrap();
        assert!(listener_inodes(a).is_empty());
    }
}

mod overlap {
    use super::*;
    use std::net::{IpAddr, TcpStream};

    const PAIRS: &[(&str, &str, bool)] = &[
        ("0.0.0.0", "127.0.0.1", true),
        ("::", "::1", true),
        ("::", "127.0.0.1", true),
        ("::", "0.0.0.0", true),
        ("::ffff:127.0.0.1", "127.0.0.1", true),
        ("::ffff:127.0.0.1", "0.0.0.0", true),
        ("::ffff:0.0.0.0", "127.0.0.1", true),
        ("127.0.0.1", "127.0.0.2", false),
        ("::1", "127.0.0.1", false),
        ("::1", "0.0.0.0", false),
        ("::1", "::ffff:127.0.0.1", false),
        ("127.0.0.1", "127.0.0.1", true),
    ];

    fn addr(ip: &str, port: u16) -> SocketAddr {
        SocketAddr::new(ip.parse().unwrap(), port)
    }

    #[test]
    fn audit20_production_snapshot_overlap_and_negative_controls() {
        for &(a, b, overlap) in PAIRS {
            for (a, b) in [(a, b), (b, a)] {
                for port in [18080, 18081] {
                    let (trust, mut config) =
                        local_fixture(addr(a, 18080), addr("127.0.0.1", 19000));
                    let mut extra = config.volumes[0].clone();
                    extra.id = "B".into();
                    extra.listen = addr(b, port).to_string();
                    config.volumes.push(extra);
                    crate::control::tests::scope_peers(&mut config);
                    let result = trust.prepare(crate::control::proto::Configuration {
                        contents: Some(crate::control::proto::configuration::Contents::Snapshot(
                            config,
                        )),
                    });
                    assert_eq!(
                        result.is_err(),
                        overlap && port == 18080,
                        "{a} -> {b}:{port}"
                    );
                    if let Err(e) = result {
                        assert!(e.to_string().contains("overlaps candidate"));
                    }
                }
            }
        }
        // Kernel aliases are not exact socket reuse keys, even within IPv6.
        let a: SocketAddr = "[fe80::1%1]:18080".parse().unwrap();
        let b: SocketAddr = "[fe80::1%2]:18080".parse().unwrap();
        assert!(crate::listener_policy::overlaps(a, b));
        let mut b = a;
        if let SocketAddr::V6(b) = &mut b {
            b.set_flowinfo(1);
        }
        assert!(crate::listener_policy::overlaps(a, b));
    }

    #[test]
    fn audit20_worker_staged_conflict_and_retirement_retry() {
        let updates = Arc::new(Updates::default());
        let mut w = Worker::new(&updates, 0);
        let (trust, mut config) = local_fixture(addr("127.0.0.1", 18080), addr("127.0.0.1", 19000));
        let a = config.volumes[0].listen.parse().unwrap();
        {
            let _scope = w.world.enter();
            w.node
                .prepare(
                    Arc::new(prepare_snapshot(&trust, config.clone())),
                    &mut w.ring,
                )
                .unwrap();
            let sources = w.node.crypto_sources.len();
            for ip in ["0.0.0.0", "127.0.0.1", "::", "::ffff:127.0.0.1"] {
                let mut next = config.clone();
                next.volumes[0].listen = addr(ip, 18080).to_string();
                let error = w
                    .node
                    .prepare(Arc::new(prepare_snapshot(&trust, next)), &mut w.ring)
                    .unwrap_err();
                assert!(error.to_string().contains("staged listener"));
                assert_eq!(w.node.crypto_sources.len(), sources);
                assert_eq!(owned(&w.node, a), 1);
            }
            w.node.staged = None;
            // Prepared is public: worker admission independently checks the entire
            // candidate before binding even its first (otherwise safe) listener.
            let mut candidate = prepare_snapshot(&trust, config.clone());
            let mut extra = config.clone();
            extra.volumes[0].id = "B".into();
            extra.volumes[0].listen = "0.0.0.0:18080".into();
            candidate
                .volumes
                .push(prepare_snapshot(&trust, extra).volumes.remove(0));
            let error = w
                .node
                .prepare(Arc::new(candidate), &mut w.ring)
                .unwrap_err();
            assert!(error.to_string().contains("candidate listener"));
            assert!(w.node.staged.is_none());
            let unused = w.world.listen(a).unwrap();
            drop(unused);
        }
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        w.poll();
        let volume = config.volumes.pop().unwrap();
        config.revision = 2;
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        w.poll();
        config.revision = 3;
        config.volumes.push(volume);
        config.volumes[0].listen = "0.0.0.0:18080".into();
        updates.publish(prepare_snapshot(&trust, config)).unwrap();
        w.poll();
        assert_eq!(updates.status()["rejected"], true);
        assert_eq!(updates.status()["ready"], false);
        w.world.advance(DRAIN_TIMEOUT + Duration::from_secs(1));
        w.poll(); // prepare still sees the socket; then retirement closes it
        assert!(w.node.retired.is_empty());
        quiesce(&mut w);
        w.world.advance(Duration::from_secs(1));
        w.poll();
        assert_eq!(updates.status()["activeRevision"], 3);
        assert_eq!(updates.status()["ready"], true);
        w.finish();
    }

    fn inodes(port: u16) -> std::collections::BTreeSet<String> {
        ["/proc/net/tcp", "/proc/net/tcp6"]
            .into_iter()
            .flat_map(|path| {
                std::fs::read_to_string(path)
                    .unwrap()
                    .lines()
                    .skip(1)
                    .filter_map(|line| {
                        let fields: Vec<_> = line.split_whitespace().collect();
                        (fields[3] == "0A" && fields[1].ends_with(&format!(":{port:04X}")))
                            .then(|| fields[9].to_owned())
                    })
                    .collect::<Vec<_>>()
            })
            .collect()
    }

    fn poll(workers: &mut [(Volumes, uring::Ring)]) {
        for (node, ring) in workers {
            ring.progress().unwrap();
            node.poll(ring, 64).unwrap();
        }
    }

    fn head(workers: &mut [(Volumes, uring::Ring)], address: SocketAddr) -> u16 {
        use std::io::{Read, Write};
        let (tx, rx) = std::sync::mpsc::channel();
        let client = std::thread::spawn(move || {
            let timeout = Duration::from_secs(3);
            let mut socket = TcpStream::connect_timeout(&address, timeout).unwrap();
            socket.set_read_timeout(Some(timeout)).unwrap();
            socket.set_write_timeout(Some(timeout)).unwrap();
            socket
                .write_all(b"HEAD /fresh HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                .unwrap();
            let mut response = String::new();
            socket.read_to_string(&mut response).unwrap();
            tx.send(
                response
                    .split_whitespace()
                    .nth(1)
                    .unwrap()
                    .parse::<u16>()
                    .unwrap(),
            )
            .unwrap();
        });
        let end = Instant::now() + Duration::from_secs(4);
        let result = loop {
            poll(workers);
            if let Ok(code) = rx.try_recv() {
                break code;
            }
            assert!(Instant::now() < end, "TCP request watchdog: {address}");
            std::thread::sleep(Duration::from_micros(100));
        };
        client.join().unwrap();
        result
    }

    fn destination(a: SocketAddr) -> SocketAddr {
        if a.ip().is_unspecified() {
            SocketAddr::new(
                if a.is_ipv4() {
                    IpAddr::V4(std::net::Ipv4Addr::LOCALHOST)
                } else {
                    IpAddr::V6(std::net::Ipv6Addr::LOCALHOST)
                },
                a.port(),
            )
        } else {
            a
        }
    }

    #[test]
    fn audit20_kernel_two_workers_overlap_transitions_and_multivolume() {
        use std::sync::{
            Mutex,
            atomic::{AtomicBool, Ordering},
        };
        let stop = Arc::new(AtomicBool::new(false));
        let (backend, origin) =
            crate::conformance::origin(0, stop.clone(), Arc::new(Mutex::new(Vec::new())));
        // Real dual-stack and mapped IPv6 sockets are mandatory on this Linux test.
        for &(left, right, _) in &PAIRS[..6] {
            for (left, right) in [(left, right), (right, left)] {
                let port = address().port();
                let a = addr(left, port);
                let b = addr(right, port);
                let other = address();
                let (trust, mut config) = local_fixture(a, backend);
                let mut extra = config.volumes[0].clone();
                extra.id = "unrelated".into();
                extra.listen = other.to_string();
                config.volumes.push(extra);
                config.epoch = 101;
                let updates = Arc::new(Updates::default());
                let mut workers: Vec<_> = (0..2)
                    .map(|id| {
                        let ring = crate::control::tests::ring().expect("real io_uring required");
                        (volumes(&ring, &updates, id), ring)
                    })
                    .collect();
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                poll(&mut workers);
                assert_eq!(updates.status()["ready"], true);
                let original = inodes(port);
                assert_eq!(original.len(), 2);
                assert_eq!(head(&mut workers, destination(a)), 200);
                // A dual-stack wildcard still serves actual IPv4 traffic.
                if left == "::" {
                    assert_eq!(head(&mut workers, addr("127.0.0.1", port)), 200);
                }
                let active = config.clone();
                config.revision = 2;
                config.epoch = 102;
                config.volumes[0].listen = b.to_string();
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                assert_eq!(updates.status()["rejected"], true, "{a} -> {b}");
                assert_eq!(updates.status()["ready"], false);
                assert_eq!(updates.applied_epoch(), 101);
                assert_eq!(inodes(port), original, "staging diverted kernel traffic");
                for _ in 0..8 {
                    assert_eq!(head(&mut workers, destination(a)), 200);
                    assert_eq!(head(&mut workers, other), 200);
                }
                // Supersede the failed move and remove only A, leaving B ready.
                config.revision = 3;
                config.volumes.remove(0);
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                poll(&mut workers);
                assert_eq!(updates.status()["activeRevision"], 3);
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(head(&mut workers, destination(a)), 409);
                let mut moved = active.volumes[0].clone();
                moved.listen = b.to_string();
                config.volumes.insert(0, moved);
                config.revision = 4;
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                assert_eq!(updates.status()["rejected"], true);
                assert_eq!(updates.status()["ready"], false);
                assert_eq!(updates.status()["activeRevision"], 3);
                assert_eq!(inodes(port), original, "retired socket shadowed candidate");
                assert_eq!(head(&mut workers, other), 200);
                // Exact-address reuse reclaims both original sockets.
                config.revision = 5;
                config.volumes[0].listen = a.to_string();
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                poll(&mut workers);
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(inodes(port), original);
                for _ in 0..8 {
                    assert_eq!(head(&mut workers, destination(a)), 200);
                }
                // End retirement without progressing ACCEPT cancellation: the kernel
                // endpoint must disappear even while the ring still owns its file.
                let mut moved = config.volumes.remove(0);
                config.revision = 6;
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                poll(&mut workers);
                for (node, ring) in &mut workers {
                    node.retired.get_mut(&a).unwrap().0 = Instant::now();
                    node.poll_retired(ring, 64).unwrap();
                    assert!(node.retired.is_empty());
                }
                assert!(
                    inodes(port).is_empty(),
                    "pending ACCEPT kept endpoint listening"
                );
                moved.listen = b.to_string();
                config.volumes.insert(0, moved);
                config.revision = 7;
                updates.publish(prepare_snapshot(&trust, config)).unwrap();
                poll(&mut workers);
                poll(&mut workers);
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(updates.status()["activeRevision"], 7);
                assert_eq!(inodes(port).len(), 2);
                assert!(inodes(port).is_disjoint(&original));
                assert_eq!(head(&mut workers, destination(b)), 200);
                assert_eq!(head(&mut workers, other), 200);
                for (node, ring) in &mut workers {
                    node.shutdown(ring).unwrap();
                }
                drop(workers);
                assert!(inodes(port).is_empty());
            }
        }
        stop.store(true, Ordering::Relaxed);
        origin.join().unwrap();
    }

    #[test]
    fn audit20_kernel_disjoint_same_port_listeners_serve_each_volume() {
        use std::sync::{
            Mutex,
            atomic::{AtomicBool, Ordering},
        };
        let stop = Arc::new(AtomicBool::new(false));
        let (backend, origin) =
            crate::conformance::origin(0, stop.clone(), Arc::new(Mutex::new(Vec::new())));
        for &(left, right, overlap) in PAIRS {
            if overlap {
                continue;
            }
            let a = addr(left, address().port());
            let b = addr(right, a.port());
            let (trust, mut config) = local_fixture(a, backend);
            let mut extra = config.volumes[0].clone();
            extra.id = "B".into();
            extra.listen = b.to_string();
            config.volumes.push(extra);
            let updates = Arc::new(Updates::default());
            let ring = crate::control::tests::ring().expect("real io_uring required");
            let mut workers = vec![(volumes(&ring, &updates, 0), ring)];
            updates.publish(prepare_snapshot(&trust, config)).unwrap();
            poll(&mut workers);
            assert_eq!(updates.status()["ready"], true, "{a}, {b}");
            assert_eq!(inodes(a.port()).len(), 2);
            assert_eq!(head(&mut workers, destination(a)), 200);
            assert_eq!(head(&mut workers, destination(b)), 200);
            for (node, ring) in &mut workers {
                node.shutdown(ring).unwrap();
            }
        }
        stop.store(true, Ordering::Relaxed);
        origin.join().unwrap();
    }
}

fn local_fixture(
    a: SocketAddr,
    backend: SocketAddr,
) -> (crate::control::Trust, crate::control::proto::Snapshot) {
    let (trust, mut config) = fixture();
    config.peers.clear();
    let v = &mut config.volumes[0];
    v.listen = a.to_string();
    v.origin_address = backend.to_string();
    v.peers.clear();
    let topology = v.topology.as_mut().unwrap();
    topology.local_slots = vec![0, 1];
    topology.neighbors.clear();
    (trust, config)
}

fn peer_fixture(
    a: SocketAddr,
    backend: SocketAddr,
) -> (crate::control::Trust, crate::control::proto::Snapshot) {
    let (trust, mut config) = fixture();
    let peer = "03".repeat(32);
    config.peers[0].id = peer.clone();
    let v = &mut config.volumes[0];
    v.listen = a.to_string();
    v.origin_address = backend.to_string();
    v.peers = vec![peer.clone()];
    v.topology.as_mut().unwrap().neighbors[0].peer = peer;
    (trust, config)
}

fn peer_wire(config: &crate::control::proto::Snapshot, target: &str) -> String {
    let routing = crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap();
    let target = (0..)
        .map(|n| format!("{target}-{n}"))
        .find(|t| routing.start(t).owner == 0)
        .unwrap();
    let cursor = routing.start(&target);
    let mut bytes = b"RF04".to_vec();
    bytes.extend(5000u32.to_le_bytes());
    bytes.extend(cursor.algorithm.magic());
    bytes.extend(cursor.encode());
    bytes.extend(b"RF05\0");
    bytes.extend(target.as_bytes());
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn peer_headers(wire: &str) -> Vec<(String, String)> {
    let (trust, _) = fixture();
    let policy = crate::http_auth::Policy {
        keys: trust.keys,
        universe: trust.universe,
        node: [3; 32],
        peers: [trust.node].into(),
    };
    let mut headers = vec![("X-Racer-Fault".into(), wire.as_bytes().to_vec())];
    policy
        .request(trust.node, "GET", "/", &mut headers)
        .unwrap();
    headers
        .into_iter()
        .map(|(k, v)| (k, String::from_utf8(v).unwrap()))
        .collect()
}

fn owned(node: &Volumes, address: SocketAddr) -> usize {
    usize::from(node.servers.contains_key(&address))
        + usize::from(node.retired.contains_key(&address))
        + usize::from(
            node.staged
                .as_ref()
                .is_some_and(|s| s.listeners.contains_key(&address)),
        )
}

fn quiesce(w: &mut Worker) {
    let _scope = w.world.enter();
    for _ in 0..16 {
        w.ring.progress().unwrap();
        w.world.advance(Duration::from_millis(1));
        w.world.run_tasks();
    }
}

#[test]
fn b08_dst_failed_stage_abort_supersession_and_expiry_reservation() {
    for end in ["commit", "arm", "abort", "supersede"] {
        let updates = Arc::new(Updates::default());
        let mut w = Worker::new(&updates, 0);
        let (trust, mut config) = fixture();
        let a = config.volumes[0].listen.parse().unwrap();
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        w.poll();
        let old = w.node.servers[&a].handler().current.clone();
        let volume = config.volumes.pop().unwrap();
        config.revision = 2;
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        w.poll();
        let deadline = old.drain.get().unwrap();
        w.world.advance(Duration::from_secs(1));
        config.revision = 3;
        config.volumes.push(volume);
        config.volumes[0].topology.as_mut().unwrap().epoch = 3;
        let mut extra = config.volumes[0].clone();
        extra.id = "B".into();
        let b: SocketAddr = "127.0.0.1:18089".parse().unwrap();
        extra.listen = b.to_string();
        config.volumes.push(extra);
        let blocker = w.world.listen(b).unwrap();
        // Publish first, then coordinate the same candidate before any worker
        // observes it. Switching modes on the previous draining revision is
        // deliberately rejected by Updates::command.
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        updates
            .command(prepare_snapshot(&trust, config.clone()), 1)
            .unwrap();
        w.poll();
        assert_eq!(updates.status()["rejected"], true);
        assert!(w.node.staged.is_none());
        assert_eq!(owned(&w.node, a), 1);
        assert!(Rc::ptr_eq(&old, &w.node.retired[&a].1.handler().current));
        assert_eq!(old.drain.get(), Some(deadline));
        drop(blocker);
        w.world.advance(Duration::from_millis(250));
        w.poll();
        assert_eq!(updates.status()["preparedWorkers"], 1);
        assert!(w.node.staged.as_ref().unwrap().listeners.contains_key(&b));
        assert!(!w.node.staged.as_ref().unwrap().listeners.contains_key(&a));
        assert_eq!(owned(&w.node, a), 1);
        let candidate = w.node.staged.as_ref().unwrap().generations[&a].clone();
        if end == "arm" {
            updates
                .command(prepare_snapshot(&trust, config.clone()), 2)
                .unwrap();
            w.poll();
            assert!(w.node.retired.is_empty());
            assert_eq!(updates.status()["receiveReadyWorkers"], 1);
            assert!(!candidate.active.get());
        }
        w.world.advance(deadline.duration_since(w.world.now()));
        w.poll();
        assert!(
            old.expired.get(),
            "{end}: old authority must expire while staged"
        );
        assert_eq!(old.drain.get(), Some(deadline));
        assert!(!candidate.expired.get());
        assert_eq!(owned(&w.node, a), 1);
        assert!(w.world.listen(a).is_err());
        for _ in 0..8 {
            let work = w.poll();
            assert!(
                !work.deadline.is_some_and(|d| d < w.world.now()),
                "expired reservation must not spin on an old drain deadline"
            );
        }
        match end {
            "commit" | "arm" => {
                updates
                    .command(prepare_snapshot(&trust, config.clone()), 3)
                    .unwrap();
                w.poll();
                assert_eq!(updates.status()["activeRevision"], 3);
                assert!(candidate.active.get());
                assert!(w.node.retired.is_empty());
                assert_eq!(owned(&w.node, a), 1);
                assert!(w.node.servers[&a].handler().draining.is_empty());
            }
            "abort" => {
                updates
                    .command(prepare_snapshot(&trust, config.clone()), 5)
                    .unwrap();
                w.poll();
                assert_eq!(owned(&w.node, a), 0);
                quiesce(&mut w);
                drop(w.world.listen(a).unwrap());
                drop(w.world.listen(b).unwrap());
            }
            _ => {
                config.revision = 4;
                config.volumes.clear();
                updates
                    .command(prepare_snapshot(&trust, config), 1)
                    .unwrap();
                w.poll();
                assert_eq!(owned(&w.node, a), 0);
                quiesce(&mut w);
                drop(w.world.listen(a).unwrap());
                drop(w.world.listen(b).unwrap());
            }
        }
        drop((old, candidate));
        w.finish();
    }
}

#[test]
fn generated_listener_churn_preserves_ownership_and_deadlines() {
    for seed in 0..8 {
        let mut random = crate::simulation::corpus::Random(seed);
        let updates = Arc::new(Updates::default());
        let mut w = Worker::new(&updates, 0);
        let (trust, mut config) = fixture();
        let a = config.volumes[0].listen.parse().unwrap();
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        w.poll();
        let mut generations = Vec::new();
        for cycle in 0..16 {
            let old = w.node.servers[&a].handler().current.clone();
            let removed = cycle % 2 == 0 || random.index(2) == 0;
            let mut removed_deadline = None;
            if removed {
                let volume = config.volumes.pop().unwrap();
                config.revision += 1;
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                w.poll();
                assert!(w.node.servers.is_empty());
                assert_eq!(owned(&w.node, a), 1);
                removed_deadline = old.drain.get();
                config.volumes.push(volume);
            }
            w.world
                .advance(Duration::from_millis(1 + random.next() % 100));
            config.revision += 1;
            // Address is the ownership key even across a changed volume identity.
            let volume = &mut config.volumes[0];
            if random.index(2) == 0 {
                volume.id = format!("replacement-{cycle}");
            }
            volume.topology.as_mut().unwrap().epoch = config.revision;
            updates
                .publish(prepare_snapshot(&trust, config.clone()))
                .unwrap();
            w.poll();
            let deadline = old.drain.get().unwrap();
            if let Some(original) = removed_deadline {
                assert_eq!(deadline, original, "readd must not renew the drain");
            }
            generations.push((old, deadline));
            assert_eq!(updates.status()["activeRevision"], config.revision);
            assert_eq!(owned(&w.node, a), 1);
            assert!(w.node.retired.is_empty());
            let handler = w.node.servers[&a].handler();
            assert!(handler.current.active.get());
            assert_eq!(handler.draining.len(), generations.len().min(MAX_DRAINING));
            assert!(w.world.listen(a).is_err());
            for (i, (g, deadline)) in generations.iter().enumerate() {
                assert_eq!(g.drain.get(), Some(*deadline));
                assert_eq!(g.expired.get(), i + MAX_DRAINING < generations.len());
                if !g.expired.get() {
                    assert!(handler.draining.iter().any(|d| Rc::ptr_eq(d, g)));
                }
            }
        }
        config.revision += 1;
        config.volumes.clear();
        updates.publish(prepare_snapshot(&trust, config)).unwrap();
        w.poll();
        w.world.advance(DRAIN_TIMEOUT);
        w.poll();
        assert_eq!(owned(&w.node, a), 0);
        assert!(generations.iter().all(|(g, _)| g.expired.get()));
        quiesce(&mut w);
        drop(w.world.listen(a).unwrap());
        drop(generations);
        w.finish();
    }
}

#[test]
fn b08_dst_two_worker_unarmed_readd_survives_expiry_then_direct_commit() {
    let updates = Arc::new(Updates::default());
    let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
    let (trust, mut config) = fixture();
    let a = config.volumes[0].listen.parse().unwrap();
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    for i in [0, 1, 0] {
        workers[i].poll();
    }
    let volume = config.volumes.pop().unwrap();
    config.revision = 2;
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    for i in [0, 1, 0] {
        workers[i].poll();
    }
    let old = workers[0].node.retired[&a].1.handler().current.clone();
    config.revision = 3;
    config.volumes.push(volume);
    config.volumes[0].topology.as_mut().unwrap().epoch = 3;
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    workers[0].poll();
    assert_eq!(updates.status()["preparedWorkers"], 1);
    assert!(
        workers[0]
            .node
            .staged
            .as_ref()
            .unwrap()
            .listeners
            .is_empty()
    );
    workers[0].world.advance(DRAIN_TIMEOUT);
    workers[0].poll();
    assert!(old.expired.get());
    assert_eq!(owned(&workers[0].node, a), 1);
    assert_eq!(updates.status()["activeRevision"], 2);
    // Neither worker uses receive-arm: direct commit must reclaim the reservation.
    workers[1].poll();
    workers[0].poll();
    assert_eq!(updates.status()["activeRevision"], 3);
    for w in &workers {
        assert_eq!(owned(&w.node, a), 1);
        assert!(w.node.retired.is_empty());
        assert!(w.node.servers[&a].handler().current.active.get());
    }
    drop(old);
    for w in workers {
        w.finish();
    }
}

#[test]
fn b08_dst_held_peer_and_receive_arm_preserve_tasks() {
    use crate::{buffers::Key, http_client as client, http_server::scenario_origin::Origin};
    fn run() -> [u8; 32] {
        let world = World::new(487);
        let _scope = world.enter();
        let mut ring = crate::conformance::ring(16, Default::default());
        let updates = Arc::new(Updates::default());
        let mut node = volumes(&ring, &updates, 0);
        let a = "127.0.0.1:18080".parse().unwrap();
        let backend = "127.0.0.1:18082".parse().unwrap();
        let (trust, mut config) = peer_fixture(a, backend);
        let mut origin = http::Server::new(
            http::Listener::bind(backend, NonZeroU32::new(16).unwrap()).unwrap(),
            Origin {
                hits: Rc::new(RefCell::new(Vec::new())),
                node: 1,
            },
            http::Config::default(),
        );
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        node.poll(&mut ring, 32).unwrap();
        let old = node.servers[&a].handler().current.clone();
        let wire = peer_wire(&config, "/held");
        let headers = peer_headers(&wire);
        let headers: Vec<_> = headers
            .iter()
            .map(|(k, v)| (k.as_str(), v.as_str()))
            .collect();
        let fill = ring.pool().stage(Key::new([201; 32])).unwrap();
        let mut held = client::Connection::new(a, "localhost")
            .unwrap()
            .get(
                client::Request::new("/", &headers).unwrap(),
                fill,
                world.now() + Duration::from_secs(10),
            )
            .unwrap();
        // Stop backend service only after the real old peer task starts a fault.
        for _ in 0..300 {
            ring.progress().unwrap();
            node.poll(&mut ring, 32).unwrap();
            assert!(matches!(
                held.poll(&mut ring, 32).unwrap(),
                Progress::Pending(_)
            ));
            world.advance(Duration::from_millis(1));
            world.run_tasks();
            if Rc::strong_count(&old) >= 3 && node.servers[&a].connections() == 1 {
                break;
            }
        }
        assert!(
            Rc::strong_count(&old) >= 3,
            "accepted task must pin old generation"
        );
        let volume = config.volumes.pop().unwrap();
        config.revision = 2;
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        node.poll(&mut ring, 32).unwrap();
        let deadline = old.drain.get();
        config.revision = 3;
        config.volumes.push(volume);
        config.volumes[0].topology.as_mut().unwrap().epoch = 3;
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        updates
            .command(prepare_snapshot(&trust, config.clone()), 1)
            .unwrap();
        node.poll(&mut ring, 32).unwrap();
        assert_eq!(owned(&node, a), 1);
        updates
            .command(prepare_snapshot(&trust, config.clone()), 2)
            .unwrap();
        node.poll(&mut ring, 32).unwrap();
        assert_eq!(node.servers[&a].connections(), 1);
        assert!(!node.servers[&a].handler().current.active.get());
        let candidate = node.staged.as_ref().unwrap().generations[&a].clone();
        assert!(!candidate.active.get());
        // Fresh TCP peers for both identities work during receive-only arm;
        // ordinary ingress still rejects until the activation barrier.
        for (key, headers, expected) in [
            (
                202,
                peer_headers(&peer_wire(&old._config.config, "/old-peer")),
                200,
            ),
            (203, peer_headers(&peer_wire(&config, "/new-peer")), 200),
            (204, vec![], 409),
        ] {
            let fill = ring.pool().stage(Key::new([key; 32])).unwrap();
            let headers: Vec<_> = headers
                .iter()
                .map(|(k, v)| (k.as_str(), v.as_str()))
                .collect();
            let mut request = client::Connection::new(a, "localhost")
                .unwrap()
                .get(
                    client::Request::new("/", &headers).unwrap(),
                    fill,
                    world.now() + Duration::from_secs(5),
                )
                .unwrap();
            let mut done = false;
            for _ in 0..1000 {
                ring.progress().unwrap();
                node.poll(&mut ring, 32).unwrap();
                origin.poll(&mut ring, 32).unwrap();
                if let Progress::Ready(reply) = request.poll(&mut ring, 32).unwrap() {
                    assert_eq!(reply.status(), expected);
                    done = true;
                    break;
                }
                world.advance(Duration::from_millis(1));
                world.run_tasks();
            }
            assert!(done);
        }
        updates
            .command(prepare_snapshot(&trust, config.clone()), 3)
            .unwrap();
        node.poll(&mut ring, 32).unwrap();
        assert!(candidate.active.get());
        assert_eq!(old.drain.get(), deadline);
        let mut done = false;
        for _ in 0..1000 {
            ring.progress().unwrap();
            node.poll(&mut ring, 32).unwrap();
            origin.poll(&mut ring, 32).unwrap();
            if let Progress::Ready(reply) = held.poll(&mut ring, 32).unwrap() {
                assert_eq!(reply.status(), 200);
                done = true;
                break;
            }
            world.advance(Duration::from_millis(1));
            world.run_tasks();
        }
        assert!(done, "held old peer must complete after readd activation");
        drop((held, old, candidate));
        node.shutdown(&mut ring).unwrap();
        origin.shutdown(&mut ring).unwrap();
        drop((node, origin, ring));
        world.advance(Duration::from_millis(10));
        world.run_tasks();
        world.assert_clean();
        world.digest()
    }
    assert_eq!(run(), run());
}
