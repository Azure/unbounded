// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::staging_tests::{address, volumes};
use super::*;
use crate::control::tests::{fixture, prepare_snapshot};

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

    fn headers(socket: &mut impl Read) -> String {
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

    struct PeerStream(crate::tls::TlsSession);
    impl PeerStream {
        fn drive<T>(
            &mut self,
            mut operation: impl FnMut(
                &mut crate::tls::TlsSession,
            ) -> io::Result<crate::tls::TlsProgress<T>>,
        ) -> io::Result<T> {
            let end = Instant::now() + Duration::from_secs(5);
            loop {
                match operation(&mut self.0)? {
                    crate::tls::TlsProgress::Complete(value) => return Ok(value),
                    crate::tls::TlsProgress::Eof => return Err(io::ErrorKind::UnexpectedEof.into()),
                    crate::tls::TlsProgress::WantRead | crate::tls::TlsProgress::WantWrite => {
                        if Instant::now() >= end {
                            return Err(io::ErrorKind::TimedOut.into());
                        }
                        thread::sleep(Duration::from_micros(100));
                    }
                }
            }
        }
    }
    impl Read for PeerStream {
        fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
            self.drive(|s| match s.read(bytes)? {
                crate::tls::TlsProgress::Eof => Ok(crate::tls::TlsProgress::Complete(0)),
                other => Ok(other),
            })
        }
    }
    impl Write for PeerStream {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            self.drive(|s| s.write(bytes))
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    trait Stream: Read + Write {}
    impl Stream for TcpStream {}
    impl Stream for std::os::unix::net::UnixStream {}
    impl Stream for PeerStream {}

    fn request(a: SocketAddr, peer: Option<(&str, &crate::tls::TlsContext)>) -> (u16, Vec<u8>) {
        let mut s: Box<dyn Stream> = if let Some((_, context)) = peer {
            let socket = connect(super::super::tests::peer_address(a));
            socket.set_nonblocking(true).unwrap();
            let mut stream = PeerStream(
                crate::tls::TlsSession::client(
                    context,
                    socket.into(),
                    crate::tls::ExpectedPeer::Identity(local_identity()),
                )
                .unwrap(),
            );
            stream.drive(|s| s.handshake()).unwrap();
            Box::new(stream)
        } else {
            let path = crate::control::tests::test_socket(a, "cache");
            let socket = std::os::unix::net::UnixStream::connect(path).unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            Box::new(socket)
        };
        if let Some((wire, _)) = peer {
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
            let length: usize = h
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    name.eq_ignore_ascii_case("content-length")
                        .then(|| value.trim().parse().unwrap())
                })
                .unwrap();
            body.resize(length, 0);
            s.read_exact(&mut body).unwrap();
        }
        (code, body)
    }

    // The actual kernel listener inode set catches competing SO_REUSEPORT sockets,
    // independently of runtime map counts and probabilistic connection selection.
    fn listener_inodes(a: SocketAddr) -> std::collections::BTreeSet<String> {
        let std::net::IpAddr::V4(ip) = a.ip() else {
            panic!("IPv4 listener observer requires an IPv4 endpoint");
        };
        // /proc/net/tcp is network-namespace-wide, not process-local. Match the
        // full endpoint and owned socket inodes so other tests/daemons using
        // 9443 cannot inflate the exact SO_REUSEPORT worker count.
        let endpoint = format!("{:08X}:{:04X}", u32::from_ne_bytes(ip.octets()), a.port());
        let owned: std::collections::BTreeSet<_> = std::fs::read_dir("/proc/self/fd")
            .unwrap()
            .filter_map(|entry| std::fs::read_link(entry.ok()?.path()).ok())
            .filter_map(|path| {
                path.to_str()?
                    .strip_prefix("socket:[")?
                    .strip_suffix(']')
                    .map(str::to_owned)
            })
            .collect();
        std::fs::read_to_string("/proc/net/tcp")
            .unwrap()
            .lines()
            .skip(1)
            .filter_map(|line| {
                let f: Vec<_> = line.split_whitespace().collect();
                (f[3] == "0A" && f[1] == endpoint && owned.contains(f[9])).then(|| f[9].to_owned())
            })
            .collect()
    }

    pub(super) fn unix_listener_inodes(path: &str) -> std::collections::BTreeSet<String> {
        std::fs::read_to_string("/proc/net/unix")
            .unwrap()
            .lines()
            .skip(1)
            .filter_map(|line| {
                let fields: Vec<_> = line.split_whitespace().collect();
                (fields.get(7) == Some(&path) && fields[3] == "00010000")
                    .then(|| fields[6].to_owned())
            })
            .collect()
    }

    enum Command {
        Inspect(mpsc::Sender<(usize, u64, Vec<u64>, usize, u64)>),
        Stop,
    }

    #[test]
    fn b08_kernel_two_workers_fresh_connections_and_held_old_peer() {
        let a = super::super::tests::address();
        let reservation = TcpListener::bind("127.0.0.1:0").unwrap();
        let (trust, mut config) = peer_fixture(a, reservation.local_addr().unwrap());
        let origin_path = config.volumes[0].origin_socket.clone();
        let backend = std::os::unix::net::UnixListener::bind(&origin_path).unwrap();
        backend.set_nonblocking(true).unwrap();
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
            std::fs::remove_file(origin_path).unwrap();
        });
        let updates = Arc::new(Updates::default());
        let authority = crate::tls::tests::Authority::new();
        let exporter = crate::metrics::Exporter::start(
            "127.0.0.1:0".parse().unwrap(),
            Arc::new(crate::metrics::Registry::new(2, updates.clone())),
        )
        .unwrap();
        assert_ne!(
            exporter.address().ip(),
            a.ip(),
            "regression requires distinct management and peer bind IPs"
        );
        let probe_management = || {
            let mut socket = connect(exporter.address());
            socket
                .write_all(b"GET /status HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
                .unwrap();
            assert!(headers(&mut socket).starts_with("HTTP/1.1 200"));
        };
        probe_management();
        let peer_context = authority.context(&remote_identity(), true);
        updates.set_credentials(crate::control::credentials::Provider::for_test(
            local_identity(),
            Arc::new(authority.context(&local_identity(), true)),
        ));
        let (ready_tx, ready_rx) = mpsc::channel();
        let mut workers = Vec::new();
        for id in 0..2 {
            let updates = updates.clone();
            let ready = ready_tx.clone();
            let (tx, rx) = mpsc::channel();
            let join = thread::spawn(move || {
                let mut ring = crate::control::tests::ring().expect("real io_uring required");
                let mut node = volumes(&ring, &updates, id).with_peer_ip(a.ip());
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
                                .get(&local_key(a))
                                .or_else(|| node.retired.get(&local_key(a)).map(|(_, s)| s))
                                .unwrap();
                            reply
                                .send((
                                    owned(&node, a),
                                    s.handler().current._config.config_snapshot().revision,
                                    s.handler()
                                        .draining
                                        .iter()
                                        .map(|g| g._config.config_snapshot().revision)
                                        .collect(),
                                    node.peer_server.as_ref().unwrap().connections(),
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
        assert!(listener_inodes(a).is_empty(), "cache must not bind TCP");
        let peer_address = super::super::tests::peer_address(a);
        let unrelated_address = super::super::tests::peer_address(super::super::tests::address());
        let unrelated = TcpListener::bind(unrelated_address).unwrap();
        assert_eq!(unrelated.local_addr().unwrap().port(), peer_address.port());
        assert_ne!(unrelated_address.ip(), peer_address.ip());
        let unrelated_inodes = listener_inodes(unrelated_address);
        assert_eq!(unrelated_inodes.len(), 1);
        let original = listener_inodes(peer_address);
        assert_eq!(original.len(), 2);
        assert!(original.is_disjoint(&unrelated_inodes));
        let cache_path = config.volumes[0].cache_socket.clone();
        let unix_original = unix_listener_inodes(&cache_path);
        assert_eq!(
            unix_original.len(),
            1,
            "workers must share one Unix listener"
        );
        let old_config = config.clone();
        let held_wire = peer_wire(&config, "/held");
        let held_context = peer_context.clone();
        let held = thread::spawn(move || request(a, Some((&held_wire, &held_context))));
        held_rx.recv_timeout(Duration::from_secs(3)).unwrap();
        // Old peer is blocked in an actual backend HEAD, with its accepted task live.
        config.revision = 2;
        config.volumes[0].topology.as_mut().unwrap().epoch = 2;
        activate(config.clone());
        let prior_config = config.clone();
        let volume = config.volumes.pop().unwrap();
        config.revision = 3;
        activate(config.clone());
        assert_eq!(listener_inodes(peer_address), original);
        assert_eq!(request(a, None).0, 409);
        config.revision = 4;
        config.volumes.push(volume);
        config.volumes[0].topology.as_mut().unwrap().epoch = 4;
        activate(config.clone());
        assert_eq!(
            listener_inodes(peer_address),
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
        // Complete the retained request within its peer deadline after proving
        // it survived removal/reclamation, before unrelated connection churn.
        release.store(true, Ordering::Release);
        let (status, body) = held.join().unwrap();
        assert_eq!(status, 200);
        assert!(crate::metadata::Metadata::from_bytes(&body).is_ok());
        let mut successes = 0;
        for _ in 0..512 {
            assert_eq!(
                request(a, None).0,
                200,
                "fresh Unix connection after activation"
            );
            successes += 1;
        }
        for c in [&old_config, &prior_config, &config] {
            for n in 0..16 {
                let wire = peer_wire(c, &format!("/peer-{}-{n}", c.revision));
                let (status, body) = request(a, Some((&wire, &peer_context)));
                if c.revision == config.revision {
                    assert_eq!(status, 200);
                    assert!(crate::metadata::Metadata::from_bytes(&body).is_ok());
                } else {
                    assert_eq!(status, 400);
                }
            }
        }
        // Churn the same real address again while old generations still drain.
        for _ in 0..3 {
            let mut volume = config.volumes.pop().unwrap();
            config.revision += 1;
            activate(config.clone());
            config.revision += 1;
            volume.topology.as_mut().unwrap().epoch = config.revision;
            config.volumes.push(volume);
            activate(config.clone());
            assert_eq!(listener_inodes(peer_address), original);
            assert_eq!(unix_listener_inodes(&cache_path), unix_original);
            for _ in 0..128 {
                assert_eq!(request(a, None).0, 200);
                successes += 1;
            }
        }
        eprintln!(
            "B08 UDS/TLS: 2 I/O threads, {successes} fresh ingress 200, foreign topology rejected, current peer 200, held old peer completed, stable listener inodes"
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
        let (tx, worker) = workers.pop().unwrap();
        tx.send(Command::Stop).unwrap();
        worker.join().unwrap();
        assert_eq!(unix_listener_inodes(&cache_path), unix_original);
        for _ in 0..32 {
            assert_eq!(
                request(a, None).0,
                200,
                "surviving worker retains Unix ingress"
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
        assert!(listener_inodes(peer_address).is_empty());
        assert_eq!(listener_inodes(unrelated_address), unrelated_inodes);
        assert!(unix_listener_inodes(&cache_path).is_empty());
        assert!(!std::path::Path::new(&cache_path).exists());
        // Worker/listener retirement must not disturb the independent exporter.
        probe_management();
    }
}

mod overlap {
    use super::*;
    use std::net::TcpStream;

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
    fn audit20_production_snapshot_socket_duplicates_and_negative_controls() {
        for collision in [
            "none",
            "cache-cache",
            "cache-origin",
            "origin-cache",
            "origin-origin",
        ] {
            let (trust, mut config) = fixture();
            let mut extra = config.volumes[0].clone();
            extra.id = "B".into();
            extra.cache_socket = "/dev/racer/b/cache".into();
            extra.origin_socket = "/dev/racer/b/origin".into();
            match collision {
                "cache-cache" => extra.cache_socket = config.volumes[0].cache_socket.clone(),
                "cache-origin" => extra.cache_socket = config.volumes[0].origin_socket.clone(),
                "origin-cache" => extra.origin_socket = config.volumes[0].cache_socket.clone(),
                "origin-origin" => extra.origin_socket = config.volumes[0].origin_socket.clone(),
                _ => {}
            }
            config.volumes.push(extra);
            crate::control::tests::scope_peers(&mut config);
            let result = trust.prepare(crate::control::proto::Configuration {
                contents: Some(crate::control::proto::configuration::Contents::Snapshot(
                    config,
                )),
            });
            assert_eq!(result.is_err(), collision != "none", "{collision}");
            if let Err(error) = result {
                assert!(
                    error
                        .to_string()
                        .contains("duplicate cache or origin socket")
                );
            }
        }
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
        let node = &workers[0].0;
        let server = node
            .servers
            .iter()
            .chain(node.retired.iter().map(|(key, (_, server))| (key, server)))
            .find(|(key, _)| **key == local_key(address))
            .unwrap()
            .1;
        let path = server
            .handler()
            .current
            ._config
            .volumes()
            .iter()
            .find(|v| Address::Unix(v.cache_socket()) == local_key(address))
            .unwrap()
            .config()
            .cache_socket
            .clone();
        let (tx, rx) = std::sync::mpsc::channel();
        let client = std::thread::spawn(move || {
            let timeout = Duration::from_secs(3);
            assert!(
                TcpStream::connect_timeout(&address, timeout).is_err(),
                "cache unexpectedly exposes TCP ingress"
            );
            let mut socket = std::os::unix::net::UnixStream::connect(path).unwrap();
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
        a
    }

    #[test]
    fn audit20_kernel_two_workers_socket_transitions_and_multivolume() {
        use std::sync::{
            Mutex,
            atomic::{AtomicBool, Ordering},
        };
        let stop = Arc::new(AtomicBool::new(false));
        let (backend, origin) =
            crate::conformance::origin(0, stop.clone(), Arc::new(Mutex::new(Vec::new())));
        let (extra_backend, extra_origin) =
            crate::conformance::origin(1, stop.clone(), Arc::new(Mutex::new(Vec::new())));
        // Local UDS ownership is independent of the addresses used for peer TCP.
        for &(left, right, _) in &PAIRS[..6] {
            for (left, right) in [(left, right), (right, left)] {
                let port = address().port();
                let a = addr(left, port);
                let b = addr(right, address().port());
                let other = address();
                let (trust, mut config) = local_fixture(a, backend);
                let mut extra = config.volumes[0].clone();
                extra.id = "unrelated".into();
                extra.cache_socket = crate::control::tests::test_socket(other, "cache");
                extra.origin_socket = crate::control::tests::test_socket(extra_backend, "origin");
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
                let original = tcp::unix_listener_inodes(&config.volumes[0].cache_socket);
                assert_eq!(original.len(), 1);
                assert!(inodes(port).is_empty());
                assert_eq!(head(&mut workers, destination(a)), 200);
                let active = config.clone();
                config.revision = 2;
                config.epoch = 102;
                config.volumes[0].cache_socket = local_key(b).to_string();
                let blocker =
                    std::os::unix::net::UnixListener::bind(&config.volumes[0].cache_socket)
                        .unwrap();
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                assert_eq!(updates.status()["rejected"], true, "{a} -> {b}");
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(updates.applied_epoch(), 101);
                assert_eq!(
                    tcp::unix_listener_inodes(&active.volumes[0].cache_socket),
                    original,
                    "staging diverted kernel traffic"
                );
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
                moved.cache_socket = local_key(b).to_string();
                config.volumes.insert(0, moved);
                config.revision = 4;
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                assert_eq!(updates.status()["rejected"], true);
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(updates.status()["activeRevision"], 3);
                assert_eq!(
                    tcp::unix_listener_inodes(&active.volumes[0].cache_socket),
                    original,
                    "retired socket shadowed candidate"
                );
                assert_eq!(head(&mut workers, other), 200);
                // Exact-path reuse reclaims both workers' shared listener handles.
                config.revision = 5;
                config.volumes[0].cache_socket = local_key(a).to_string();
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                poll(&mut workers);
                poll(&mut workers);
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(
                    tcp::unix_listener_inodes(&active.volumes[0].cache_socket),
                    original
                );
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
                    for retired in node.retired.values_mut() {
                        retired.0 = Instant::now();
                    }
                    node.poll_listeners(ring, 64).unwrap();
                    assert!(node.retired.is_empty());
                }
                let retired_path = &active.volumes[0].cache_socket;
                assert!(
                    !std::path::Path::new(retired_path).exists(),
                    "retirement must unlink the shared filesystem endpoint"
                );
                assert!(
                    std::os::unix::net::UnixStream::connect(retired_path).is_err(),
                    "pending ACCEPT kept endpoint reachable"
                );
                // /proc/net/unix can retain the shutdown socket inode until the
                // pending ACCEPT drops its descriptor. It must then disappear too.
                let deadline = Instant::now() + Duration::from_secs(3);
                while !tcp::unix_listener_inodes(retired_path).is_empty() {
                    for (_, ring) in &mut workers {
                        ring.progress().unwrap();
                    }
                    assert!(
                        Instant::now() < deadline,
                        "retired ACCEPT descriptor leaked"
                    );
                    std::thread::yield_now();
                }
                moved.cache_socket = local_key(b).to_string();
                drop(blocker);
                config.volumes.insert(0, moved);
                config.revision = 7;
                updates.publish(prepare_snapshot(&trust, config)).unwrap();
                poll(&mut workers);
                poll(&mut workers);
                assert_eq!(updates.status()["ready"], true);
                assert_eq!(updates.status()["activeRevision"], 7);
                let replacement = tcp::unix_listener_inodes(&local_key(b).to_string());
                assert_eq!(replacement.len(), 1);
                assert!(replacement.is_disjoint(&original));
                assert!(inodes(port).is_empty());
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
        extra_origin.join().unwrap();
    }

    #[test]
    fn audit20_kernel_disjoint_sockets_serve_each_volume() {
        use std::sync::{
            Mutex,
            atomic::{AtomicBool, Ordering},
        };
        let stop = Arc::new(AtomicBool::new(false));
        let (backend, origin) =
            crate::conformance::origin(0, stop.clone(), Arc::new(Mutex::new(Vec::new())));
        let (extra_backend, extra_origin) =
            crate::conformance::origin(1, stop.clone(), Arc::new(Mutex::new(Vec::new())));
        for &(left, right, overlap) in PAIRS {
            if overlap {
                continue;
            }
            let a = addr(left, address().port());
            let b = loop {
                let candidate = addr(right, address().port());
                if candidate.port() != a.port() {
                    break candidate;
                }
            };
            let (trust, mut config) = local_fixture(a, backend);
            let mut extra = config.volumes[0].clone();
            extra.id = "B".into();
            extra.cache_socket = crate::control::tests::test_socket(b, "cache");
            extra.origin_socket = crate::control::tests::test_socket(extra_backend, "origin");
            config.volumes.push(extra);
            let updates = Arc::new(Updates::default());
            let ring = crate::control::tests::ring().expect("real io_uring required");
            let mut workers = vec![(volumes(&ring, &updates, 0), ring)];
            updates.publish(prepare_snapshot(&trust, config)).unwrap();
            poll(&mut workers);
            assert_eq!(updates.status()["ready"], true, "{a}, {b}");
            assert!(inodes(a.port()).is_empty());
            assert_eq!(
                tcp::unix_listener_inodes(&local_key(a).to_string()).len(),
                1
            );
            assert_eq!(
                tcp::unix_listener_inodes(&local_key(b).to_string()).len(),
                1
            );
            assert_eq!(head(&mut workers, destination(a)), 200);
            assert_eq!(head(&mut workers, destination(b)), 200);
            for (node, ring) in &mut workers {
                node.shutdown(ring).unwrap();
            }
        }
        stop.store(true, Ordering::Relaxed);
        origin.join().unwrap();
        extra_origin.join().unwrap();
    }
}

fn local_fixture(
    a: SocketAddr,
    backend: SocketAddr,
) -> (crate::control::Trust, crate::control::proto::Snapshot) {
    let (trust, mut config) = fixture();
    config.peers.clear();
    let v = &mut config.volumes[0];
    v.cache_socket = crate::control::tests::test_socket(a, "cache");
    v.origin_socket = crate::control::tests::test_socket(backend, "origin");
    v.peers.clear();
    let topology = v.topology.as_mut().unwrap();
    topology.local_slots = vec![0, 1];
    topology.product = Some(crate::control::proto::ProductTopology {
        left_factor: 1,
        right_factor: 1,
        members: vec!["02".repeat(32)],
        roles: vec![0],
        local_member: 0,
        candidate_width: 1,
        candidates: vec![0; topology.slot_count as usize],
    });
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
    v.cache_socket = crate::control::tests::test_socket(a, "cache");
    v.origin_socket = crate::control::tests::test_socket(backend, "origin");
    v.peers = vec![peer.clone()];
    v.topology
        .as_mut()
        .unwrap()
        .product
        .as_mut()
        .unwrap()
        .members[1] = peer;
    (trust, config)
}

fn peer_wire(config: &crate::control::proto::Snapshot, target: &str) -> String {
    let routing = crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap();
    let target = (0..)
        .map(|n| format!("{target}-{n}"))
        .find(|t| routing.start(t).owner == 0)
        .unwrap();
    let cursor = routing.start(&target);
    let mut bytes = b"RB01".to_vec();
    bytes.extend(5000u32.to_le_bytes());
    bytes.extend(crate::routing::Cursor::MAGIC);
    bytes.extend(cursor.encode());
    bytes.extend(b"RD01\0");
    bytes.extend(target.as_bytes());
    let volume = &config.volumes[0];
    let backend = crate::handlers::Backend::unix(&volume.origin_socket, &volume.id).unwrap();
    let namespace = crate::cache::Namespace::volume(
        &config.universe,
        &volume.id,
        volume.cache_generation,
        backend.namespace(),
    );
    let bytes = crate::cache::peer_wire::with_chain(
        bytes,
        *namespace.digest(),
        8,
        255,
        routing.destination(&cursor),
    )
    .unwrap();
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn peer_headers(wire: &str) -> Vec<(String, String)> {
    let (_, config) = fixture();
    vec![
        ("X-Racer-Fault".into(), wire.into()),
        (
            "X-Racer-Attempt".into(),
            format!(
                "{}{}",
                crate::cache::peer_wire::hex(
                    crate::origin_data::binding(
                        &crate::cache::peer_wire::unhex(wire).unwrap(),
                        &Default::default()
                    )
                    .as_bytes()
                ),
                "a".repeat(32)
            ),
        ),
        ("X-Racer-Volume".into(), config.volumes[0].id.clone()),
    ]
}

fn local_identity() -> crate::tls::PeerIdentity {
    let (trust, _) = fixture();
    crate::tls::PeerIdentity::new(
        &crate::cache::peer_wire::hex(&trust.universe),
        &crate::cache::peer_wire::hex(&trust.node),
        "test-pod",
    )
    .unwrap()
}

fn remote_identity() -> crate::tls::PeerIdentity {
    let mut identity = local_identity();
    identity.node = "03".repeat(32);
    identity
}

fn owned(node: &Volumes, address: impl Into<Address>) -> usize {
    let address = match address.into() {
        Address::Tcp(a) => local_key(a),
        unix => unix,
    };
    usize::from(node.servers.contains_key(&address))
        + usize::from(node.retired.contains_key(&address))
        + usize::from(
            node.staged
                .as_ref()
                .is_some_and(|s| s.listeners.contains_key(&address)),
        )
}

fn local_key(address: SocketAddr) -> Address {
    Address::unix(&crate::control::tests::test_socket(address, "cache")).unwrap()
}
