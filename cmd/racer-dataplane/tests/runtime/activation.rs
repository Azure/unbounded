// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::control::tests::{fixture, prepare_snapshot};
use crate::simulation::World;

#[test]
fn peer_metrics_follow_active_generation_and_idle_transport_changes() {
    let world = World::new(914);
    let _scope = world.enter();
    let mut ring = crate::conformance::ring(8, Default::default());
    let updates = Arc::new(Updates::default());
    let registry = crate::metrics::Registry::new(1, updates.clone());
    registry.register(0, ring.metrics());
    let mut node = volumes(&ring, &updates, 0);
    let (trust, mut config) = fixture();
    let address = config.volumes[0].listen.parse().unwrap();
    // A second volume exercises independent breakers to the same peer.
    let mut second = config.volumes[0].clone();
    second.id = "v2".into();
    second.listen = "127.0.0.1:8090".into();
    config.volumes.push(second);
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let current = node.servers[&address].handler().current.clone();
    let handler = current.handlers[0].clone();
    let (rdma_breaker, http_breaker) = handler.borrow().test_peer_breakers();
    let sample = |volume, family, suffix: &str, value| {
        format!(
            "racer_dataplane_peer_{family}{{worker=\"0\",volume=\"{volume}\",peer=\"p1\",{suffix}}} {value}\n"
        )
    };
    let assert_transport = |volume, rdma: bool| {
        let text = registry.render();
        for (transport, value) in [("http", !rdma), ("rdma", rdma)] {
            assert!(
                text.contains(&sample(
                    volume,
                    "transport",
                    &format!("transport=\"{transport}\""),
                    u8::from(value)
                )),
                "{text}"
            );
        }
    };
    let assert_state = |volume, transport, expected| {
        let text = registry.render();
        for state in ["closed", "open", "half_open"] {
            assert!(
                text.contains(&sample(
                    volume,
                    "circuit_breaker_state",
                    &format!("transport=\"{transport}\",state=\"{state}\""),
                    u8::from(state == expected)
                )),
                "{text}"
            );
        }
    };
    let tick = |node: &mut Volumes, ring: &mut uring::Ring| {
        world.advance(crate::metrics::INTERVAL);
        let work = node.poll_peer_metrics(ring.metrics());
        assert_eq!(work.deadline, Some(world.now() + crate::metrics::INTERVAL));
    };
    assert_transport("v1", false);
    assert_transport("v2", false);
    assert_state("v1", "http", "closed");
    assert_state("v1", "rdma", "closed");
    let connection = Rc::new(rdma::test_connection(ring.pool()));
    handler
        .borrow_mut()
        .set_routed_connection("p1", connection.clone());
    assert_transport("v1", false); // Snapshot remains unchanged until the tick.
    tick(&mut node, &mut ring);
    assert_transport("v1", true);
    rdma_breaker.try_acquire().unwrap().failure();
    http_breaker.try_acquire().unwrap().failure();
    tick(&mut node, &mut ring);
    assert_transport("v1", false);
    assert_state("v1", "http", "open");
    assert_state("v1", "rdma", "open");
    assert_state("v2", "http", "closed");
    assert_state("v2", "rdma", "closed");
    world.advance(Duration::from_secs(1));
    tick(&mut node, &mut ring);
    assert_transport("v1", true); // Eligible probe, still open until acquired.
    assert_state("v1", "rdma", "open");
    let probe = rdma_breaker.try_acquire().unwrap();
    tick(&mut node, &mut ring);
    assert_state("v1", "rdma", "half_open");
    assert_transport("v1", false);
    probe.success();
    tick(&mut node, &mut ring);
    assert_state("v1", "rdma", "closed");
    assert_transport("v1", true);
    connection.disconnect().unwrap();
    tick(&mut node, &mut ring);
    assert_transport("v1", false); // Dead session, even before manager removal.
    handler.borrow_mut().remove_connection(&connection);
    let replacement = Rc::new(rdma::test_connection(ring.pool()));
    handler
        .borrow_mut()
        .set_routed_connection("p1", replacement.clone());
    tick(&mut node, &mut ring);
    assert_transport("v1", true);

    // Prepared/receive-only state must not hide the active, failed HTTP breaker.
    config.revision += 1;
    config.volumes[0].topology.as_mut().unwrap().epoch += 1;
    node.prepare(
        Arc::new(prepare_snapshot(&trust, config.clone())),
        &mut ring,
    )
    .unwrap();
    let mut staged = node.staged.take().unwrap();
    node.arm(&mut staged);
    tick(&mut node, &mut ring);
    assert_state("v1", "http", "open");
    node.commit(staged);
    tick(&mut node, &mut ring);
    assert_state("v1", "http", "closed");
    assert_transport("v1", false);
    assert!(!current.active.get());
    // Retained old permits and sessions cannot overwrite the new generation.
    rdma_breaker.try_acquire().unwrap().failure();
    tick(&mut node, &mut ring);
    assert_state("v1", "rdma", "closed");
    assert_eq!(
        registry
            .render()
            .lines()
            .filter(|s| s.starts_with("racer_dataplane_peer_"))
            .count(),
        16
    );

    config.revision += 1;
    config.volumes.remove(0);
    node.prepare(
        Arc::new(prepare_snapshot(&trust, config.clone())),
        &mut ring,
    )
    .unwrap();
    let staged = node.staged.take().unwrap();
    node.commit(staged);
    tick(&mut node, &mut ring);
    assert!(!registry.render().contains("volume=\"v1\""));
    assert_transport("v2", false);
    config.revision += 1;
    config.peers.clear();
    config.volumes[0].peers.clear();
    let topology = config.volumes[0].topology.as_mut().unwrap();
    topology.epoch += 1;
    topology.local_slots = vec![0, 1];
    topology.neighbors.clear();
    node.prepare(Arc::new(prepare_snapshot(&trust, config)), &mut ring)
        .unwrap();
    let staged = node.staged.take().unwrap();
    node.commit(staged);
    tick(&mut node, &mut ring);
    assert!(!registry.render().contains("peer=\"p1\""));
    replacement.disconnect().unwrap();
    handler.borrow_mut().remove_connection(&replacement);
    node.shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
    drop((replacement, connection, handler, current, node, ring));
    world.assert_clean();
}

mod namespace_tests {
    use super::*;
    use crate::{
        http_client::{self as client},
        http_server::scenario_origin::{Origin, OriginTask, Payload},
    };

    const TARGET: &str = "/same%2fobject?exact=1";

    // Both origins deliberately advertise the same checksum ETag, length (3),
    // target and TTL (60). Only the bytes and accepted backend identity change.
    struct Backend {
        inner: Origin,
        byte: Rc<Cell<u8>>,
        hits: Rc<RefCell<Vec<(String, bool)>>>,
    }
    impl http::Handler for Backend {
        type Task = OriginTask;

        fn start(&mut self, request: http::Request) -> Self::Task {
            assert_eq!(request.target(), TARGET);
            let host = std::str::from_utf8(request.headers().get("host").unwrap())
                .unwrap()
                .to_owned();
            let head = matches!(request, http::Request::Head(_));
            if !head {
                assert_eq!(
                    request.headers().get("if-match"),
                    Some(crate::conformance::etag(b"abc").as_bytes())
                );
            }
            self.hits.borrow_mut().push((host, head));
            let mut task = self.inner.start(request);
            if let OriginTask::Headers(_, payload) = &mut task {
                // Each response materializes the current fixture bytes privately.
                let byte = self.byte.get();
                *payload = Some(Payload::Bytes(byte, vec![byte; 3]));
            }
            task
        }

        fn poll(
            &mut self,
            task: &mut Self::Task,
            ring: &mut uring::Ring,
            budget: usize,
        ) -> io::Result<Progress<http::Completed>> {
            self.inner.poll(task, ring, budget)
        }
    }

    fn get(
        world: &World,
        ring: &mut uring::Ring,
        node: &mut Volumes,
        origins: &mut [http::Server<Backend>],
        address: SocketAddr,
    ) -> Vec<u8> {
        let fill = ring.pool().private_fill().unwrap();
        let end = world.now() + Duration::from_secs(5);
        let mut request = client::Connection::new(address, "localhost")
            .unwrap()
            .get(client::Request::new(TARGET, &[]).unwrap(), fill, end)
            .unwrap();
        loop {
            ring.progress().unwrap();
            node.poll(ring, 32).unwrap();
            for origin in origins.iter_mut() {
                origin.poll(ring, 32).unwrap();
            }
            if let Progress::Ready(mut response) = request.poll(ring, 32).unwrap() {
                assert_eq!(response.status(), 200);
                return response.body().to_vec();
            }
            assert!(world.now() < end, "namespace regression stalled");
            world.advance(Duration::from_millis(1));
            world.run_tasks();
        }
    }

    #[test]
    fn generated_namespace_lifecycle() {
        for seed in 0..8 {
            let mut random = crate::simulation::corpus::Random(seed);
            let world = World::new(701 + seed);
            let _scope = world.enter();
            let mut ring = crate::conformance::ring(16, Default::default());
            let updates = Arc::new(Updates::default());
            let mut node = volumes(&ring, &updates, 0);
            let (mut trust, mut config) = fixture();
            config.peers.clear();
            let volume = &mut config.volumes[0];
            volume.origin_address = "127.0.0.1:80".into();
            volume.origin_identity = "test/origin:80".into();
            volume.peers.clear();
            let topology = volume.topology.as_mut().unwrap();
            topology.local_slots = vec![0, 1];
            topology.neighbors.clear();
            let address = volume.listen.parse().unwrap();
            let byte = Rc::new(Cell::new(b'a'));
            let hits = Rc::new(RefCell::new(Vec::new()));
            let mut origins: Vec<_> = [80, 81]
                .into_iter()
                .map(|port| {
                    http::Server::new(
                        http::Listener::bind(
                            SocketAddr::from(([127, 0, 0, 1], port)),
                            NonZeroU32::new(16).unwrap(),
                        )
                        .unwrap(),
                        Backend {
                            inner: Origin {
                                hits: Rc::new(RefCell::new(Vec::new())),
                                node: 0,
                            },
                            byte: byte.clone(),
                            hits: hits.clone(),
                        },
                        http::Config::default(),
                    )
                })
                .collect();
            // Mandatory identity boundaries followed by generated changes.
            // Every identity is new, but version/length/target stay equal:
            // fresh metadata must not conceal a payload-key collision.
            for step in 0..24 {
                let change = if step < 4 { step } else { random.index(4) };
                // Establish a fresh baseline, then change exactly one
                // namespace component while retaining its generation.
                config.volumes[0].cache_generation += 1;
                config.volumes[0].origin_address = "127.0.0.1:80".into();
                config.volumes[0].origin_identity = "test/origin:80".into();
                config.volumes[0].topology.as_mut().unwrap().epoch += 1;
                config.revision += 1;
                byte.set(b'a');
                updates
                    .publish(prepare_snapshot(&trust, config.clone()))
                    .unwrap();
                node.poll(&mut ring, 32).unwrap();
                assert_eq!(
                    get(&world, &mut ring, &mut node, &mut origins, address),
                    b"aaa"
                );
                let generation = config.volumes[0].cache_generation;
                let v = &mut config.volumes[0];
                match change {
                    0 => v.origin_identity = format!("test/other-{step}:80"),
                    1 => {
                        trust.universe = [step as u8 + 2; 32];
                        config.universe = trust.universe.to_vec();
                    }
                    2 => v.id = format!("volume-{step}"),
                    _ => v.cache_generation += 1,
                }
                if change != 3 {
                    assert_eq!(config.volumes[0].cache_generation, generation);
                }
                if random.index(2) == 0 {
                    world.advance(Duration::from_secs(61));
                }
                byte.set(b'b');
                let before = hits.borrow().len();
                // Repeated reads and endpoint-only publication reuse
                // the same namespace; one HEAD + GET reaches the backend.
                for reuse in [false, true] {
                    config.revision += 1;
                    let v = &mut config.volumes[0];
                    v.topology.as_mut().unwrap().epoch += 1;
                    if reuse {
                        v.origin_address = "127.0.0.1:81".into();
                    }
                    let prepared = prepare_snapshot(&trust, config.clone());
                    let host = prepared.volumes[0].backend.host().to_owned();
                    updates.publish(prepared).unwrap();
                    node.poll(&mut ring, 32).unwrap();
                    assert_eq!(updates.status()["activeRevision"], config.revision);
                    for _ in 0..2 {
                        assert_eq!(
                            get(&world, &mut ring, &mut node, &mut origins, address),
                            vec![byte.get(); 3],
                            "seed={seed} step={step} reuse={reuse}"
                        );
                    }
                    if !reuse {
                        assert_eq!(
                            &hits.borrow()[before..],
                            &[(host.clone(), true), (host, false)]
                        );
                    }
                    assert_eq!(
                        hits.borrow().len(),
                        before + 2,
                        "endpoint changes must preserve warm cache"
                    );
                }
            }
            node.shutdown(&mut ring).unwrap();
            for origin in &mut origins {
                origin.shutdown(&mut ring).unwrap();
            }
            ring.shutdown().unwrap();
            drop((origins, node, ring));
            world.assert_clean();
        }
    }
}

mod management_tests {
    use super::*;

    #[test]
    fn b06_dst_management_rejection_preserves_desired_listener_visibility() {
        let world = World::new(461);
        let _scope = world.enter();
        let mut ring = crate::conformance::ring(8, Default::default());
        let updates = Arc::new(Updates::default());
        let mut node = volumes(&ring, &updates, 0);
        let (trust, mut config) = fixture();
        config.epoch = 101;
        let a = config.volumes[0].listen.parse().unwrap();
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        node.poll(&mut ring, 16).unwrap();
        assert_eq!(updates.status()["ready"], true);
        let old = node.servers[&a].handler().current.clone();
        config.revision = 2;
        config.epoch = 102;
        let mut b = config.volumes[0].clone();
        b.id = "B".into();
        b.listen = "127.0.0.1:9090".into();
        config.volumes.push(b);
        // No simulated blocker: this must be policy rejection, not EADDRINUSE.
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        let candidate = updates.latest(1).unwrap();
        assert_eq!(updates.status()["ready"], false);
        node.poll(&mut ring, 16).unwrap();
        let status = updates.status();
        eprintln!("B06 DST after staging: {status}");
        assert_eq!(
            status["rejected"], true,
            "B06 must reject management before binding"
        );
        assert_eq!(status["ready"], false, "B05 required B remains visible");
        assert_eq!(status["candidateRevision"], 2);
        assert_eq!(status["activeRevision"], 1);
        assert_eq!(
            status["volumes"],
            serde_json::json!([{"id":"v1","epoch":1,"ready":true}])
        );
        assert_eq!(updates.applied_epoch(), 101);
        assert!(Arc::ptr_eq(&candidate, &updates.latest(1).unwrap()));
        assert!(Rc::ptr_eq(&old, &node.servers[&a].handler().current));
        assert_eq!(node.servers.len(), 1);
        // Staging retries cannot turn a permanent management conflict into activation.
        world.advance(Duration::from_millis(250));
        node.poll(&mut ring, 16).unwrap();
        assert_eq!(updates.status()["rejected"], true);
        assert_eq!(updates.status()["ready"], false);
        let unused = world.listen("127.0.0.1:9090".parse().unwrap()).unwrap();
        drop(unused);
        node.shutdown(&mut ring).unwrap();
        drop((old, node, ring));
        world.assert_clean();
    }

    fn get(exporter: &crate::metrics::Exporter, path: &str) -> (u16, String) {
        use std::io::{Read, Write};
        let mut address = exporter.address();
        if address.ip().is_unspecified() {
            address.set_ip(if address.is_ipv4() {
                std::net::Ipv4Addr::LOCALHOST.into()
            } else {
                std::net::Ipv6Addr::LOCALHOST.into()
            });
        }
        let timeout = Duration::from_secs(3);
        let mut socket = std::net::TcpStream::connect_timeout(&address, timeout).unwrap();
        socket.set_read_timeout(Some(timeout)).unwrap();
        socket.set_write_timeout(Some(timeout)).unwrap();
        write!(
            socket,
            "GET {path} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
        )
        .unwrap();
        let mut response = String::new();
        socket.read_to_string(&mut response).unwrap();
        let (headers, body) = response.split_once("\r\n\r\n").unwrap();
        (
            headers.split_whitespace().nth(1).unwrap().parse().unwrap(),
            body.into(),
        )
    }

    fn check_http(exporter: &crate::metrics::Exporter, updates: &Updates, ready: bool) {
        for path in ["/readyz", "/status"] {
            let (code, body) = get(exporter, path);
            assert_eq!(
                code,
                if path == "/readyz" && !ready {
                    503
                } else {
                    200
                }
            );
            assert_eq!(
                serde_json::from_str::<serde_json::Value>(&body).unwrap(),
                updates.status()
            );
        }
        assert_eq!(updates.status()["ready"], ready);
    }

    #[test]
    fn b06_real_exporter_socket_default_custom_and_family_reservations() {
        // Fixed default verifies deployment behavior; :0 verifies the actual assigned
        // Exporter.address(), not the requested port or a hard-coded 9090 check.
        for management in ["0.0.0.0:9090", "127.0.0.1:0", "[::1]:0", "[::]:0"] {
            for initial in [false, true] {
                for host in [
                    "127.0.0.1",
                    "0.0.0.0",
                    "127.0.0.2",
                    "[::1]",
                    "[::]",
                    "[::ffff:127.0.0.1]",
                ] {
                    let updates = Arc::new(Updates::default());
                    let registry = Arc::new(crate::metrics::Registry::new(1, updates.clone()));
                    let exporter =
                        crate::metrics::Exporter::start(management.parse().unwrap(), registry)
                            .unwrap();
                    let Some(mut ring) = crate::control::tests::ring() else {
                        return;
                    };
                    let mut node = volumes(&ring, &updates, 0).with_management(exporter.address());
                    let (trust, mut config) = fixture();
                    let a = address();
                    config.volumes[0].listen = a.to_string();
                    config.epoch = 111;
                    if !initial {
                        updates
                            .publish(prepare_snapshot(&trust, config.clone()))
                            .unwrap();
                        node.poll(&mut ring, 16).unwrap();
                        check_http(&exporter, &updates, true);
                        config.revision = 2;
                    }
                    let old = node.servers.get(&a).map(|s| s.handler().current.clone());
                    let mut b = config.volumes[0].clone();
                    b.id = "B".into();
                    b.listen = format!("{host}:{}", exporter.address().port());
                    config.volumes.push(b);
                    config.epoch = 112;
                    updates
                        .publish(prepare_snapshot(&trust, config.clone()))
                        .unwrap();
                    let candidate = updates.latest(0).unwrap();
                    check_http(&exporter, &updates, false);
                    node.poll(&mut ring, 16).unwrap();
                    let status = updates.status();
                    eprintln!(
                        "B06 real management={} B={} initial={initial}: {status}",
                        exporter.address(),
                        config.volumes[1].listen
                    );
                    assert_eq!(status["rejected"], true);
                    assert_eq!(status["activeRevision"], if initial { 0 } else { 1 });
                    assert_eq!(status["candidateRevision"], config.revision);
                    assert_eq!(status["preparedWorkers"], 0);
                    assert_eq!(updates.applied_epoch(), if initial { 0 } else { 111 });
                    assert_eq!(
                        status["volumes"],
                        if initial {
                            serde_json::json!([])
                        } else {
                            serde_json::json!([{"id":"v1","epoch":1,"ready":true}])
                        }
                    );
                    assert!(Arc::ptr_eq(&candidate, &updates.latest(0).unwrap()));
                    assert!(node.staged.is_none());
                    assert_eq!(node.servers.len(), usize::from(!initial));
                    if let Some(old) = &old {
                        assert!(old.active.get());
                        assert!(Rc::ptr_eq(old, &node.servers[&a].handler().current));
                    }
                    check_http(&exporter, &updates, false);
                    let (code, metrics) = get(&exporter, "/metrics");
                    assert_eq!(code, 200);
                    assert!(metrics.contains(&format!(
                        "racer_dataplane_config_epoch {}\n",
                        if initial { 0 } else { 111 }
                    )));
                    // A distinct data port stages/activates normally, while management
                    // continues serving status. Supersession clears the failed retry.
                    config.revision += 1;
                    config.volumes[1].listen = address().to_string();
                    updates.publish(prepare_snapshot(&trust, config)).unwrap();
                    node.poll(&mut ring, 16).unwrap();
                    check_http(&exporter, &updates, true);
                    assert_eq!(updates.applied_epoch(), 112);
                    assert_eq!(node.servers.len(), 2);
                    assert!(node.preparing.is_none());
                    node.shutdown(&mut ring).unwrap();
                }
            }
        }
    }
}

mod coordination_tests {
    //! Real Go HTTP + production Subscriber, with deterministic local worker IO.
    //! Reused through staging's Worker fixture; no fabricated worker acknowledgments.
    use super::*;
    use crate::control::{Source, Subscriber, Trust};

    #[test]
    #[ignore = "launched by Go TestProductionIdleSiteLifecycle"]
    fn production_idle_site_child() {
        use std::io::{Read, Write};
        let source = Source::from_env().unwrap();
        let trust = Arc::new(Trust::from_env().unwrap());
        let updates = Arc::new(Updates::default());
        let credentials =
            crate::control::credentials::Provider::fixture_from_env(2, &updates).unwrap();
        let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
        assert_eq!(updates.status()["ready"], false);
        let subscriber = Subscriber::start(source.clone(), trust, updates.clone()).unwrap();
        let end = Instant::now() + Duration::from_secs(22);
        for revision in 1..=5 {
            let mut complete_at = None;
            loop {
                for w in &mut workers {
                    let _scope = w.world.enter();
                    w.ring.progress().unwrap();
                    w.world.run_tasks();
                    w.world.advance(Duration::from_secs(1));
                    w.poll();
                }
                let status = updates.status();
                if status["activeRevision"] == revision
                    && status["retiredWorkers"] == 2
                    && status["phase"] == 4
                {
                    assert_eq!(status["ready"], revision != 4, "{status}");
                    assert_eq!(updates.applied_epoch(), revision);
                    for worker in &workers {
                        assert_eq!(worker.node.servers.len(), usize::from(revision == 2));
                    }
                    if complete_at.get_or_insert_with(Instant::now).elapsed()
                        >= Duration::from_millis(600)
                    {
                        let Source::Http { address, host, .. } = &source else {
                            unreachable!()
                        };
                        let mut socket = credentials.connect(*address).unwrap();
                        socket
                            .set_read_timeout(Some(Duration::from_secs(2)))
                            .unwrap();
                        write!(
                            socket,
                            "GET /advance HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n"
                        )
                        .unwrap();
                        let mut response = String::new();
                        socket.read_to_string(&mut response).unwrap();
                        if response.starts_with("HTTP/1.1 200") {
                            eprintln!("idle lifecycle revision {revision}: {status}");
                            break;
                        }
                        assert!(response.starts_with("HTTP/1.1 409"), "{response}");
                    }
                }
                assert!(Instant::now() < end, "idle lifecycle stalled: {status}");
                std::thread::sleep(Duration::from_millis(5));
            }
        }
        drop(subscriber);
        for worker in workers {
            worker.finish();
        }
    }

    #[test]
    #[ignore = "launched by Go TestB15ProductionForward"]
    fn production_forward_child() {
        use std::io::{Read, Write};
        let source = Source::from_env().unwrap();
        let backend = matches!(
            std::env::var("RACER_FORWARD_MODE").unwrap().as_str(),
            "backend" | "replay"
        );
        let survivor = std::env::var("RACER_FORWARD_MODE").unwrap() == "survivor";
        let trust = Arc::new(Trust::from_env().unwrap());
        let updates = Arc::new(Updates::default());
        let credentials =
            crate::control::credentials::Provider::fixture_from_env(2, &updates).unwrap();
        let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
        // Permanent worker-local bind failure, retained through successful correction.
        let blockers: Vec<_> = workers
            .iter()
            .filter(|_| !backend && !survivor)
            .map(|w| {
                let _scope = w.world.enter();
                w.world.listen("0.0.0.0:10000".parse().unwrap()).unwrap()
            })
            .collect();
        let subscriber = Subscriber::start(source.clone(), trust, updates.clone()).unwrap();
        let end = Instant::now() + Duration::from_secs(12);
        let mut failed = false;
        let mut done = None;
        loop {
            for w in &mut workers {
                let _scope = w.world.enter();
                w.ring.progress().unwrap();
                w.world.run_tasks();
                w.world.advance(Duration::from_secs(1));
                w.poll();
            }
            let s = updates.status();
            if (s["rejected"] == true
                || (backend && s["lastError"].is_string())
                || (survivor && s["receiveReadyWorkers"] == 2))
                && !failed
            {
                assert_eq!(s["candidateRevision"], if backend { 0 } else { 1 });
                if !survivor {
                    assert_eq!(s["activeRevision"], 0);
                }
                let Source::Http { address, host, .. } = &source else {
                    unreachable!()
                };
                let mut socket = credentials.connect(*address).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                write!(
                    socket,
                    "GET /failed HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n"
                )
                .unwrap();
                let mut response = String::new();
                socket.read_to_string(&mut response).unwrap();
                assert!(response.starts_with("HTTP/1.1 200"));
                failed = true;
            }
            if s["activeRevision"] == 2 && s["phase"] == 4 && s["retiredWorkers"] == 2 {
                assert!(failed);
                if done.get_or_insert_with(Instant::now).elapsed() > Duration::from_millis(600) {
                    break;
                }
            }
            assert!(
                Instant::now() < end,
                "B15 corrective desired state blocked: {s}"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        eprintln!("B15 recovered: {}", updates.status());
        drop(subscriber);
        drop(blockers);
        for w in workers {
            w.finish();
        }
    }

    // Worker-local strict replay complements the real-thread/network hybrid above.
    // Commands here are prepared fixture snapshots, not a claim to replay Go HTTP.
    #[test]
    fn b13_phase2_terminal_catchup_strict_replay() {
        use crate::simulation::Choice;
        fn run(replay: Option<Vec<Choice>>, phase: u32) -> ([u8; 32], Vec<Choice>) {
            let world = World::new(513);
            world.enable_scheduler();
            if let Some(choices) = replay {
                world.replay(choices);
            }
            let _scope = world.enter();
            let mut ring = crate::conformance::ring(8, Default::default());
            let updates = Arc::new(Updates::default());
            let mut node = volumes(&ring, &updates, 0);
            let (trust, config) = fixture();
            updates
                .command(prepare_snapshot(&trust, config.clone()), 4)
                .unwrap();
            node.poll(&mut ring, 16).unwrap();
            let mut empty = config.clone();
            empty.revision = 2;
            empty.epoch = 2;
            empty.volumes.clear();
            empty.peers.clear();
            updates
                .command(prepare_snapshot(&trust, empty.clone()), phase)
                .unwrap();
            node.poll(&mut ring, 16).unwrap();
            let mut latest = config;
            latest.revision = 5;
            latest.epoch = 5;
            latest.volumes[0].topology.as_mut().unwrap().epoch = 5;
            if phase == 2 {
                assert_eq!(updates.status()["activeRevision"], 1);
                assert!(
                    updates
                        .command(prepare_snapshot(&trust, latest.clone()), 1)
                        .is_err()
                );
                assert!(
                    updates
                        .command(prepare_snapshot(&trust, empty.clone()), 5)
                        .is_err()
                );
                updates.command(prepare_snapshot(&trust, empty), 4).unwrap();
            }
            for _ in 0..64 {
                ring.progress().unwrap();
                world.advance(Duration::from_secs(1));
                world.run_tasks();
                node.poll(&mut ring, 16).unwrap();
            }
            assert_eq!(updates.status()["retiredWorkers"], 1);
            assert_eq!(updates.status()["activeRevision"], 2);
            // The phase-3 control never received phase 4 before local retirement.
            assert_eq!(updates.status()["phase"], if phase == 2 { 4 } else { 3 });
            updates
                .command(prepare_snapshot(&trust, latest), 4)
                .unwrap();
            node.poll(&mut ring, 16).unwrap();
            assert_eq!(updates.status()["activeRevision"], 5);
            node.shutdown(&mut ring).unwrap();
            drop((node, ring));
            world.assert_clean();
            world.assert_replay_consumed();
            (world.digest(), world.choices())
        }
        for phase in [2, 3] {
            let (digest, choices) = run(None, phase);
            assert!(!choices.is_empty());
            assert_eq!(run(Some(choices.clone()), phase), (digest, choices));
        }
    }

    #[test]
    #[ignore = "launched by Go TestB13ProductionCatchup"]
    fn production_catchup_child() {
        let trust = Arc::new(Trust::from_env().unwrap());
        let updates = Arc::new(Updates::default());
        crate::control::credentials::Provider::fixture_from_env(2, &updates).unwrap();
        let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
        let subscriber =
            Subscriber::start(Source::from_env().unwrap(), trust, updates.clone()).unwrap();
        let end = Instant::now() + Duration::from_secs(14);
        let trap: u64 = std::env::var("RACER_CATCHUP_TRAP")
            .unwrap()
            .parse()
            .unwrap();
        let mut trapped = false;
        let mut done = None;
        loop {
            for w in &mut workers {
                let _scope = w.world.enter();
                w.ring.progress().unwrap();
                w.world.run_tasks();
                w.world.advance(Duration::from_secs(1));
                w.poll();
            }
            let s = updates.status();
            if s["candidateRevision"] == 2 && s["phase"] == trap {
                if trap == 2 && s["receiveReadyWorkers"] == 2 {
                    assert_eq!(s["activeRevision"], 1);
                    assert_eq!(s["retiredWorkers"], 0);
                    trapped = true;
                }
                if trap == 3 && s["retiredWorkers"] == 2 {
                    assert_eq!(s["activeRevision"], 2);
                    assert_eq!(s["ready"], false);
                    trapped = true;
                }
            }
            if s["activeRevision"] == 5 && s["phase"] == 4 && s["retiredWorkers"] == 2 {
                assert!(trapped, "exact trap/control never observed");
                assert_eq!(s["ready"], true);
                assert_eq!(updates.applied_epoch(), 5);
                if done.get_or_insert_with(Instant::now).elapsed() > Duration::from_millis(600) {
                    break;
                }
            }
            assert!(
                Instant::now() < end,
                "B13 phase-{trap} catchup stalled: {s}"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        eprintln!("B13 phase-{trap}: {}", updates.status());
        drop(subscriber);
        for w in workers {
            w.finish();
        }
    }

    #[test]
    #[ignore = "launched by Go TestB14ProductionCoordination with mTLS controller"]
    fn production_coordination_child() {
        use std::io::{Read, Write};
        let mode = std::env::var("RACER_COORDINATION_MODE").unwrap();
        let source = Source::from_env().unwrap();
        let trust = Arc::new(Trust::from_env().unwrap());
        let updates = Arc::new(Updates::default());
        let credentials =
            crate::control::credentials::Provider::fixture_from_env(2, &updates).unwrap();
        let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
        let subscriber = Subscriber::start(source.clone(), trust, updates.clone()).unwrap();
        let heartbeat = mode == "heartbeat";
        let end = Instant::now() + Duration::from_secs(if heartbeat { 25 } else { 9 });
        let negative = !matches!(
            mode.as_str(),
            "lost" | "lost-read" | "create-lost" | "no-commit" | "conflict" | "heartbeat"
        );
        let mut released = false;
        let mut complete_at = None;
        loop {
            for worker in &mut workers {
                worker.poll();
            }
            let status = updates.status();
            if negative && !released && status["lastError"].is_string() {
                let expected = match mode.as_str() {
                    "universe" | "node" | "boot" => "control command identity/profile mismatch",
                    "revision" => "candidate revision mismatch",
                    "digest" => "candidate digest mismatch",
                    "status-revision" => "command revision mismatch",
                    "status-digest" => "missing candidate",
                    _ => unreachable!(),
                };
                assert_eq!(status["lastError"], expected);
                assert_eq!(
                    status["activeRevision"], 0,
                    "bad command activated: {status}"
                );
                assert_eq!(status["receiveReadyWorkers"], 0, "bad receive: {status}");
                if mode.starts_with("status-") {
                    assert_eq!(status["candidateRevision"], 1);
                    assert_eq!(status["preparedWorkers"], 2);
                    assert_eq!(status["phase"], 1);
                } else {
                    assert_eq!(status["candidateRevision"], 0, "bad candidate: {status}");
                }
                eprintln!("rejected {mode}: {}", status["lastError"]);
                let Source::Http { address, host, .. } = &source else {
                    unreachable!()
                };
                let mut socket = credentials.connect(*address).unwrap();
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                write!(
                    socket,
                    "GET /release HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n"
                )
                .unwrap();
                let mut response = String::new();
                socket.read_to_string(&mut response).unwrap();
                assert!(response.starts_with("HTTP/1.1 200"));
                released = true;
            }
            if status["activeRevision"] == 1
                && status["retiredWorkers"] == 2
                && status["phase"] == 4
            {
                assert_eq!(status["phase"], 4);
                assert_eq!(status["ready"], true);
                assert_eq!(status["activatedWorkers"], 2);
                assert_eq!(updates.applied_epoch(), 1);
                for worker in &workers {
                    assert_eq!(worker.node.servers.len(), 1);
                    assert!(
                        worker
                            .node
                            .servers
                            .values()
                            .all(|s| s.handler().current.active.get())
                    );
                }
                // Let the actual Subscriber send its final worker-derived phase-4 ack.
                let at = complete_at.get_or_insert_with(Instant::now);
                if at.elapsed() >= Duration::from_millis(if heartbeat { 17000 } else { 600 }) {
                    break;
                }
            }
            assert!(
                Instant::now() < end,
                "coordination {mode} stalled: {status}"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
        assert!(!negative || released);
        eprintln!("coordinated {mode}: {}", updates.status());
        drop(subscriber);
        for worker in workers {
            worker.finish();
        }
    }
}

mod forward_multi_tests {
    //! Concurrent recipients driven by the Go mTLS control server. Test endpoints
    //! schedule faults only; every protocol acknowledgment comes from Volumes.
    use super::*;
    use crate::control::{Source, Subscriber, Trust};

    fn schedule(
        source: &Source,
        credentials: &Arc<crate::control::credentials::Provider>,
        path: &str,
    ) -> bool {
        use std::io::{Read, Write};
        let Source::Http { address, host, .. } = source else {
            unreachable!()
        };
        let mut stream = credentials.connect(*address).unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        write!(
            stream,
            "GET {path} HTTP/1.1\r\nHost: {host}\r\nConnection: close\r\n\r\n"
        )
        .unwrap();
        let mut reply = String::new();
        stream.read_to_string(&mut reply).unwrap();
        reply.starts_with("HTTP/1.1 200")
    }

    #[test]
    #[ignore = "launched by Go TestB15ProductionMultiRecipient"]
    fn production_multi_forward_child() {
        let role = std::env::var("RACER_MULTI_ROLE").unwrap();
        let trap: u64 = std::env::var("RACER_MULTI_PHASE").unwrap().parse().unwrap();
        let source = Source::from_env().unwrap();
        let trust = Arc::new(Trust::from_env().unwrap());
        let updates = Arc::new(Updates::default());
        let credentials =
            crate::control::credentials::Provider::fixture_from_env(2, &updates).unwrap();
        let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
        let blockers: Vec<_> = workers
            .iter()
            .filter(|_| role == "failed")
            .map(|w| {
                let _scope = w.world.enter();
                w.world.listen("0.0.0.0:10000".parse().unwrap()).unwrap()
            })
            .collect();
        let subscriber = Subscriber::start(source.clone(), trust, updates.clone()).unwrap();
        let end = Instant::now() + Duration::from_secs(25);
        let mut observed_old = false;
        let mut old_retired = false;
        let mut failure_reported = false;
        let mut complete = None;
        loop {
            for w in &mut workers {
                let _scope = w.world.enter();
                w.ring.progress().unwrap();
                w.world.run_tasks();
                w.world.advance(Duration::from_millis(100));
                w.poll();
            }
            let s = updates.status();
            if s["candidateRevision"] == 1 {
                if role == "failed" {
                    assert_eq!(s["activeRevision"], 0);
                    assert_eq!(s["receiveReadyWorkers"], 0);
                    if s["rejected"] == true && !failure_reported {
                        assert!(workers.iter().all(|w| w.node.preparing.is_some()));
                        assert!(schedule(&source, &credentials, "/multi/failed"));
                        failure_reported = true;
                    }
                } else if s["phase"] == trap
                    && s["receiveReadyWorkers"] == 2
                    && (trap == 2 || s["activatedWorkers"] == 2)
                {
                    observed_old = true;
                    assert_eq!(s["activeRevision"], if trap == 2 { 0 } else { 1 });
                    for w in &workers {
                        assert_eq!(w.node.servers.len(), 2);
                        assert!(
                            w.node.servers.values().all(|server| server
                                .handler()
                                .current
                                .active
                                .get()
                                == (trap >= 3))
                        );
                    }
                    if role == "initial" && schedule(&source, &credentials, "/multi/checkpoint") {
                        break;
                    }
                }
                old_retired |= s["phase"] == 4 && s["retiredWorkers"] == 2;
            }
            if s["candidateRevision"] == 2 && role == "survivor" {
                assert!(
                    observed_old && old_retired,
                    "survivor skipped old receive obligation: {s}"
                );
            }
            if s["activeRevision"] == 2 && s["phase"] == 4 && s["retiredWorkers"] == 2 {
                assert_eq!(s["preparedWorkers"], 2);
                assert_eq!(s["receiveReadyWorkers"], 2);
                assert_eq!(s["activatedWorkers"], 2);
                assert_eq!(s["ready"], true);
                assert_eq!(updates.applied_epoch(), 2);
                assert!(role != "failed" || failure_reported);
                for w in &workers {
                    assert_eq!(w.node.servers.len(), 1);
                    assert!(
                        w.node
                            .servers
                            .values()
                            .all(|server| server.handler().current.active.get())
                    );
                }
                if complete.get_or_insert_with(Instant::now).elapsed() > Duration::from_millis(800)
                {
                    break;
                }
            }
            assert!(Instant::now() < end, "multi-recipient {role} stalled: {s}");
            std::thread::sleep(Duration::from_millis(10));
        }
        eprintln!(
            "B15 multi {role} old={observed_old} retired={old_retired}: {}",
            updates.status()
        );
        drop(subscriber);
        drop(blockers);
        for w in workers {
            w.finish();
        }
    }
}

mod storage_workers {
    //! Actual worker-group failure fanout, production Volumes/HTTP/cache, virtual IO.
    use super::*;
    use crate::{
        allocator, buffers::Key, http_client as client, http_server::scenario_origin::Origin,
        simulation::Disk,
    };
    use std::sync::{
        Mutex,
        atomic::{AtomicUsize, Ordering},
    };

    struct Wake;
    impl crate::workers::Wake for Wake {
        fn wake(&self) {}
    }
    struct Probe {
        world: World,
        ring: uring::Ring,
        node: Volumes,
        origin: http::Server<Origin>,
        disk: Disk,
        commands: Arc<Mutex<Vec<(usize, u8)>>>,
        replies: std::sync::mpsc::Sender<(usize, u16)>,
        id: usize,
        polls: Arc<Vec<AtomicUsize>>,
    }
    impl Probe {
        fn poll(&mut self) {
            self.polls[self.id].fetch_add(1, Ordering::Relaxed);
            self.world.advance(Duration::from_millis(1));
            self.world.run_tasks();
            self.ring.progress().unwrap();
            self.node.poll(&mut self.ring, 32).unwrap();
            self.origin.poll(&mut self.ring, 32).unwrap();
        }
        fn get(&mut self, address: SocketAddr, target: &str) -> u16 {
            let fill = self.ring.pool().stage(Key::new([240; 32])).unwrap();
            let start = self.world.now();
            let end = start + Duration::from_secs(5);
            let mut request = client::Connection::new(address, "localhost")
                .unwrap()
                .get(client::Request::new(target, &[]).unwrap(), fill, end)
                .unwrap();
            loop {
                self.poll();
                if let Progress::Ready(mut response) = request.poll(&mut self.ring, 32).unwrap() {
                    if response.status() == 200 {
                        assert_eq!(response.body(), b"abc");
                    } else {
                        assert!(
                            self.world.now() - start < Duration::from_millis(800),
                            "storage rejection exceeded bounded admission window"
                        );
                    }
                    return response.status();
                }
                assert!(self.world.now() < end);
            }
        }
    }
    impl crate::workers::Driver for Probe {
        type Wake = Wake;
        fn wake_handle(&self) -> Arc<Wake> {
            Arc::new(Wake)
        }
        fn turn(&mut self) -> io::Result<()> {
            let world = self.world.clone();
            let _scope = world.enter();
            self.poll();
            let command = {
                let mut commands = self.commands.lock().unwrap();
                commands
                    .iter()
                    .position(|(id, _)| *id == self.id)
                    .map(|i| commands.remove(i).1)
            };
            if let Some(command) = command {
                let a = "127.0.0.1:18080".parse().unwrap();
                let b = "127.0.0.1:18081".parse().unwrap();
                let status = match command {
                    0 => self.get(a, "/warm"),
                    1 => {
                        self.disk.set_available_bytes(0);
                        self.get(a, "/pressure")
                    }
                    2 => {
                        self.disk.set_available_bytes(u64::MAX);
                        self.get(a, "/recovered")
                    }
                    3 => {
                        // Fail actual WRITE_FIXED completion after a cold backend
                        // metadata admission. It must reach quarantine, not fail().
                        self.world.fail_next_errno(5, libc::ENOSPC);
                        let status = self.get(a, "/poison");
                        for _ in 0..100 {
                            self.poll();
                        }
                        assert!(self.world.fault_fired());
                        assert_eq!(self.ring.metrics().values()[20], 1);
                        status
                    }
                    4 => self.get(b, "/unrelated-volume"),
                    5 => self.get(a, "/warm"),
                    _ => unreachable!(),
                };
                self.replies.send((self.id, status)).unwrap();
            }
            std::thread::sleep(Duration::from_millis(1));
            Ok(())
        }
        fn shutdown(&mut self) -> io::Result<()> {
            let _scope = self.world.enter();
            self.node.shutdown(&mut self.ring)?;
            self.origin.shutdown(&mut self.ring)?;
            self.ring.shutdown()?;
            assert!(
                self.ring
                    .pool()
                    .invariant_snapshot()
                    .refs
                    .iter()
                    .all(|n| *n == 0)
            );
            Ok(())
        }
    }

    #[test]
    fn b17_actual_worker_group_survives_storage_pressure_and_poison() {
        let updates = Arc::new(Updates::default());
        let (trust, mut config) = fixture();
        config.peers.clear();
        let volume = &mut config.volumes[0];
        volume.listen = "127.0.0.1:18080".into();
        volume.origin_address = "127.0.0.1:19080".into();
        volume.peers.clear();
        let topology = volume.topology.as_mut().unwrap();
        topology.local_slots = vec![0, 1];
        topology.neighbors.clear();
        let mut b = volume.clone();
        b.id = "unrelated".into();
        b.listen = "127.0.0.1:18081".into();
        config.volumes.push(b);
        updates.publish(prepare_snapshot(&trust, config)).unwrap();
        let commands = Arc::new(Mutex::new(Vec::new()));
        let queue = commands.clone();
        let (tx, rx) = std::sync::mpsc::channel();
        let polls = Arc::new((0..2).map(|_| AtomicUsize::new(0)).collect::<Vec<_>>());
        let observed = polls.clone();
        let workers = crate::workers::Workers::start(
            crate::workers::Config {
                shard_count: std::num::NonZeroUsize::new(2).unwrap(),
            },
            move |placement| {
                let world = World::new(719 + placement.worker.0 as u64);
                let _scope = world.enter();
                let ring = crate::conformance::ring(12, Default::default());
                let disk = Disk::new(32 * 1024 * 1024);
                let mut slab =
                    allocator::Slab::simulated(disk.clone(), 32 * 1024 * 1024, 1, true).unwrap();
                let mut cache =
                    crate::cache::tests::cache_from_slab(&mut slab, 1, Default::default());
                cache.set_metrics(ring.metrics().clone());
                updates.subscribe(ring.wake_handle());
                let node = Volumes::new(
                    cache,
                    updates.clone(),
                    Arc::new(crate::crypto::Pool::test_pool(ring.pool())),
                    placement.worker.0,
                );
                let origin = http::Server::new(
                    http::Listener::bind(
                        "127.0.0.1:19080".parse().unwrap(),
                        NonZeroU32::new(16).unwrap(),
                    )?,
                    Origin {
                        hits: Rc::new(RefCell::new(Vec::new())),
                        node: 0,
                    },
                    http::Config::default(),
                );
                Ok(Probe {
                    world,
                    ring,
                    node,
                    origin,
                    disk,
                    commands: queue.clone(),
                    replies: tx.clone(),
                    id: placement.worker.0,
                    polls: observed.clone(),
                })
            },
        )
        .unwrap();
        assert_eq!(workers.placements().len(), 2);
        let ask = |id, cmd, expected| {
            commands.lock().unwrap().push((id, cmd));
            assert_eq!(
                rx.recv_timeout(Duration::from_secs(10)).unwrap(),
                (id, expected)
            );
        };
        for id in 0..2 {
            ask(id, 0, 200);
        }
        ask(0, 1, 503); // reversible admission, bounded by the existing cache retry cap
        ask(1, 4, 200); // unrelated volume on another worker continues cold fills
        ask(0, 2, 200); // capacity alone recovers pre-write pressure without restart
        ask(0, 3, 503); // ambiguous IO instead latches quarantine
        ask(0, 5, 503); // even warm lookups cannot reuse the poisoned shard
        ask(1, 5, 200);
        ask(1, 2, 200);
        ask(1, 4, 200);
        // All volumes share the worker's slab: the same poisoned shard on another
        // volume is deliberately rejected; containment is shard-, not volume-local.
        ask(0, 4, 503);
        assert!(polls.iter().all(|p| p.load(Ordering::Relaxed) > 100));
        workers.stop_handle().request_stop();
        workers.join().unwrap();
    }
}

pub(super) fn address() -> SocketAddr {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
}

#[test]
fn coordinated_candidate_is_receive_addressable_before_ingress() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let (trust, mut config) = fixture();
    let address = address();
    config.volumes[0].listen = address.to_string();
    let updates = Arc::new(Updates::default());
    let mut volumes = volumes(&ring, &updates, 0);
    let prepare = |s| prepare_snapshot(&trust, s);
    updates.command(prepare(config.clone()), 1).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers.is_empty());
    assert!(!updates.status()["ready"].as_bool().unwrap());
    updates.command(prepare(config.clone()), 2).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    let candidate = volumes.servers[&address].handler().current.clone();
    assert!(!candidate.active.get());
    assert!(!updates.status()["ready"].as_bool().unwrap());
    updates.command(prepare(config.clone()), 4).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(candidate.active.get());
    assert!(updates.status()["ready"].as_bool().unwrap());
    config.revision = 2;
    config.volumes[0].topology.as_mut().unwrap().epoch = 2;
    updates.command(prepare(config.clone()), 1).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers[&address].handler().draining.is_empty());
    updates.command(prepare(config.clone()), 2).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    let handler = volumes.servers[&address].handler();
    assert!(Rc::ptr_eq(&handler.current, &candidate));
    assert!(handler.current.active.get());
    assert_eq!(handler.draining.len(), 1);
    assert!(!handler.draining[0].active.get());
    assert_ne!(handler.current.identity, handler.draining[0].identity);
    updates.command(prepare(config), 3).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(!candidate.active.get());
    assert!(volumes.servers[&address].handler().current.active.get());
    assert_eq!(volumes.servers[&address].handler().draining.len(), 1);
    volumes.shutdown(&mut ring).unwrap();
}

pub(crate) fn volumes(ring: &uring::Ring, updates: &Arc<Updates>, worker: usize) -> Volumes {
    updates.subscribe(ring.wake_handle());
    let cache = if crate::simulation::current().is_some() {
        let size = 32 * 1024 * 1024;
        let mut slab =
            crate::allocator::Slab::simulated(crate::simulation::Disk::new(size), size, 1, true)
                .unwrap();
        crate::cache::tests::cache_from_slab(&mut slab, 1, Default::default())
    } else {
        crate::cache::tests::cache(1)
    };
    Volumes::new(
        cache,
        updates.clone(),
        Arc::new(crate::crypto::Pool::test_pool(ring.pool())),
        worker,
    )
}

pub(crate) fn pending_cleanup(node: &mut Volumes, ring: &uring::Ring) -> rdma::Transport {
    let transport = rdma::test_transport_config(ring.pool(), 1, 1);
    let pending = transport.prepare([25; 16], 0, 1).unwrap();
    transport.test_block_destroy(true);
    node.rdma_sources
        .push((0, RdmaSource::new(transport.test_source())));
    drop(pending);
    crate::cache::tests::pending_admission(&mut node.cache.borrow_mut(), ring.pool());
    transport
}

pub(crate) fn assert_cleanup_complete(node: &Volumes) {
    assert!(node.rdma_sources.is_empty());
    assert!(crate::cache::tests::idle(&node.cache.borrow()));
}

#[test]
fn final_shutdown_maximum_startup_capacity_has_constant_batch_delays() {
    for delay in [Duration::ZERO, Duration::from_millis(30)] {
        for qps in [0, 32] {
            let world = World::new(2602);
            let _scope = world.enter();
            let mut ring = crate::conformance::ring(8, Default::default());
            let mut node = volumes(&ring, &Arc::new(Updates::default()), 0);
            let transport = rdma::test_transport_config(ring.pool(), 32, 16);
            let pending: Vec<_> = (0..qps)
                .map(|_| transport.prepare([26; 16], 0, 1).unwrap())
                .collect();
            assert_eq!(transport.test_observe().qps, qps);
            transport.test_cleanup_batches(delay);
            node.rdma_sources
                .push((0, RdmaSource::new(transport.test_source())));
            crate::cache::tests::pending_admission(&mut node.cache.borrow_mut(), ring.pool());
            let start = world.now();
            node.shutdown(&mut ring).unwrap();
            assert_cleanup_complete(&node);
            // 4 stages (QP batch, 2048 MWs, CQ/MRs, context), each requiring
            // admission + one completion observation. Exact virtual time only.
            let stages = if qps == 0 { 3 } else { 4 };
            let expected = delay.max(Duration::from_millis(10)) * stages;
            assert!(world.now() - start >= expected);
            // Accepted cache I/O can advance this virtual clock between turns.
            assert!(world.now() - start <= expected + Duration::from_millis(10));
            assert!(world.now() - start < SHUTDOWN_TIMEOUT);
            assert_eq!(transport.test_invariants(), (0, 0, 0));
            drop((pending, transport, node));
            ring.shutdown().unwrap();
            drop(ring);
            world.assert_clean();
        }
    }
}

#[test]
fn final_shutdown_pending_is_bounded_and_completes_cache_before_provider() {
    for failed in [false, true] {
        let world = World::new(2601);
        let _scope = world.enter();
        let mut ring = crate::conformance::ring(8, Default::default());
        let mut node = volumes(&ring, &Arc::new(Updates::default()), 0);
        let transport = pending_cleanup(&mut node, &ring);
        transport.test_faults(0, failed, false, false);
        let freed = transport.test_observe().freed;
        let other = rdma::test_transport_config(ring.pool(), 1, 1);
        node.rdma_sources
            .push((1, RdmaSource::new(other.test_source())));
        let start = world.now();
        let end = start + Duration::from_millis(200);
        assert_eq!(
            node.shutdown_until(&mut ring, end).unwrap_err().kind(),
            io::ErrorKind::TimedOut
        );
        assert!(world.now() >= end && world.now() < end + Duration::from_millis(20));
        assert_eq!(node.rdma_sources.len(), 2);
        assert!(
            node.rdma_sources[1].1.quiesced,
            "blocked rail must not starve another"
        );
        assert_eq!(other.test_invariants(), (0, 0, 0));
        assert_eq!(transport.test_observe().qps, 1);
        assert_eq!(transport.test_observe().freed, freed);
        assert!(crate::cache::tests::idle(&node.cache.borrow()));
        // The retained owner can still finish safely, without reopening admission.
        transport.test_faults(0, false, false, false);
        transport.test_block_destroy(false);
        node.shutdown(&mut ring).unwrap();
        assert_cleanup_complete(&node);
        assert_eq!(transport.test_invariants(), (0, 0, 0));
        drop((transport, node));
        ring.shutdown().unwrap();
        drop(ring);
        world.assert_clean();
    }
}

#[test]
fn lifecycle_drain_fences_later_configuration_activation() {
    let world = World::new(622);
    let _scope = world.enter();
    let mut ring = crate::conformance::ring(8, Default::default());
    let updates = Arc::new(Updates::default());
    let mut node = volumes(&ring, &updates, 0);
    let (trust, mut config) = fixture();
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let address = config.volumes[0].listen.parse().unwrap();
    let generation = node.servers[&address].handler().current.clone();
    node.begin_drain();
    assert!(!generation.active.get());
    assert!(node.drained());
    config.revision = 2;
    config.volumes[0].listen = "127.0.0.1:18082".into();
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    for _ in 0..4 {
        node.poll(&mut ring, 16).unwrap();
    }
    assert_eq!(node.revision, 1);
    assert!(node.staged.is_none() && node.preparing.is_none());
    assert!(Rc::ptr_eq(
        &generation,
        &node.servers[&address].handler().current
    ));
    node.shutdown(&mut ring).unwrap();
    drop((generation, node, ring));
    world.assert_clean();
}

// Separate virtual hosts stand in for worker-local SO_REUSEPORT sockets. Each
// worker runs production staging on its own ring/cache, with one shared Updates.
pub(super) struct Worker {
    pub(super) world: World,
    pub(super) ring: uring::Ring,
    pub(super) node: Volumes,
}
impl Worker {
    pub(super) fn new(updates: &Arc<Updates>, id: usize) -> Self {
        let world = World::new(449 + id as u64);
        let _scope = world.enter();
        let ring = crate::conformance::ring(8, Default::default());
        let node = volumes(&ring, updates, id);
        Self { world, ring, node }
    }
    pub(super) fn poll(&mut self) -> uring::Work {
        let _scope = self.world.enter();
        self.node.poll(&mut self.ring, 16).unwrap()
    }
    pub(super) fn finish(mut self) {
        let _scope = self.world.enter();
        self.node.shutdown(&mut self.ring).unwrap();
        drop((self.node, self.ring));
        self.world.assert_clean();
    }
}

/// Model worker acknowledgments independently of Updates. Poll order, the
/// failed worker, startup/reload and supersession vary; assertions run after
/// every poll, including repeated polls before the retry deadline.
#[test]
fn generated_activation_retry_and_supersession() {
    use crate::simulation::corpus::Random;
    for seed in 0..16 {
        let mut random = Random(seed);
        let count = 1 + random.index(3);
        let initial = seed & 1 == 0;
        let supersede = seed & 2 != 0;
        let updates = Arc::new(Updates::default());
        let mut workers: Vec<_> = (0..count).map(|i| Worker::new(&updates, i)).collect();
        let (trust, mut config) = fixture();
        let a = config.volumes[0].listen.parse().unwrap();
        let b: std::net::SocketAddr = "127.0.0.1:18081".parse().unwrap();
        if !initial {
            updates
                .publish(prepare_snapshot(&trust, config.clone()))
                .unwrap();
            for i in (0..count).cycle().take(2 * count) {
                workers[i].poll();
            }
            assert_eq!(updates.status()["ready"], true);
            config.revision += 1;
        }
        let old: Vec<_> = workers
            .iter()
            .map(|w| w.node.servers.get(&a).map(|s| s.handler().current.clone()))
            .collect();
        let mut extra = config.volumes[0].clone();
        extra.id = "B".into();
        extra.listen = b.to_string();
        config.volumes.push(extra);
        config.epoch = 42;
        let failed = random.index(count);
        let blocker = workers[failed].world.listen(b).unwrap();
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        let candidate = updates.latest(0).unwrap();
        let mut prepared = vec![false; count];
        let mut retained = vec![None; count];
        // Guaranteed visitation followed by generated repeats prevents vacuous
        // seeds while varying which successful stages precede the failure.
        let order: Vec<_> = (0..count)
            .map(|i| (i + failed) % count)
            .chain((0..24).map(|_| random.index(count)))
            .collect();
        for i in order {
            let work = workers[i].poll();
            if i == failed {
                let retry = &workers[i].node.preparing.as_ref().unwrap().1;
                assert_eq!(retry.failures, 1);
                assert_eq!(
                    retry.after,
                    workers[i].world.now() + Duration::from_millis(250)
                );
                assert!(!work.runnable);
                assert_eq!(work.deadline, Some(retry.after));
            } else {
                prepared[i] = true;
                let stage = &workers[i].node.staged.as_ref().unwrap().generations[&b];
                if let Some(previous) = &retained[i] {
                    assert!(Rc::ptr_eq(previous, stage));
                }
                retained[i] = Some(stage.clone());
                updates.staged(config.revision, i, false); // duplicate cannot revoke success
                assert!(workers[i].world.listen(b).is_err());
            }
            let status = updates.status();
            assert_eq!(
                status["preparedWorkers"],
                prepared.iter().filter(|p| **p).count()
            );
            assert_eq!(status["activeRevision"], u64::from(!initial));
            assert_eq!(status["ready"], false);
            for (w, old) in workers.iter().zip(&old) {
                assert_eq!(w.node.servers.len(), usize::from(!initial));
                if let Some(old) = old {
                    assert!(old.active.get());
                    assert!(Rc::ptr_eq(old, &w.node.servers[&a].handler().current));
                }
            }
        }
        assert_eq!(updates.status()["rejected"], true);
        if supersede {
            config.revision += 1;
            config.epoch += 1;
            config.volumes.pop();
            updates
                .publish(prepare_snapshot(&trust, config.clone()))
                .unwrap();
            for i in 0..count {
                updates.staged(config.revision - 1, i, true);
                updates.received(config.revision - 1, i);
                updates.activated(config.revision - 1, i);
            }
            assert_eq!(updates.status()["preparedWorkers"], 0);
            assert_eq!(updates.status()["receiveReadyWorkers"], 0);
            prepared.fill(false);
        }
        drop(blocker);
        if !supersede {
            workers[failed].world.advance(Duration::from_millis(249));
            workers[failed].poll();
            assert_eq!(updates.status()["activeRevision"], u64::from(!initial));
            workers[failed].world.advance(Duration::from_millis(1));
        }
        let mut active = vec![false; count];
        let offset = random.index(count);
        for i in (0..count)
            .cycle()
            .take(3 * count)
            .map(|i| (i + offset) % count)
        {
            workers[i].poll();
            prepared[i] = true;
            if prepared.iter().all(|p| *p) {
                active[i] = true;
            }
            let all = active.iter().all(|a| *a);
            assert_eq!(
                updates.status()["activatedWorkers"],
                active.iter().filter(|a| **a).count()
            );
            assert_eq!(
                updates.status()["activeRevision"],
                if all {
                    config.revision
                } else {
                    u64::from(!initial)
                }
            );
        }
        assert_eq!(updates.applied_epoch(), config.epoch);
        assert_eq!(updates.status()["ready"], true);
        for (i, w) in workers.iter().enumerate() {
            assert!(w.node.staged.is_none() && w.node.preparing.is_none());
            if supersede {
                drop(w.world.listen(b).unwrap());
            } else {
                let current = &w.node.servers[&b].handler().current;
                assert!(Arc::ptr_eq(&candidate, &current._config));
                if let Some(stage) = &retained[i] {
                    assert!(Rc::ptr_eq(stage, current));
                }
            }
        }
        drop((old, retained));
        for w in workers {
            w.finish();
        }
    }
}

#[test]
fn b04_publication_retry_lock_gap_preserves_worker_authority() {
    let updates = Arc::new(Updates::default());
    let mut workers = [Worker::new(&updates, 0), Worker::new(&updates, 1)];
    let (trust, mut config) = fixture();
    let a = config.volumes[0].listen.parse().unwrap();
    config.epoch = 71;
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    for i in [0, 1, 0] {
        workers[i].poll();
    }
    let old: Vec<_> = workers
        .iter()
        .map(|w| w.node.servers[&a].handler().current.clone())
        .collect();
    let b: SocketAddr = "127.0.0.1:18083".parse().unwrap();
    let c: SocketAddr = "127.0.0.1:18084".parse().unwrap();
    let mut extra = config.volumes[0].clone();
    extra.id = "B".into();
    extra.listen = b.to_string();
    config.volumes.push(extra);
    config.revision = 2;
    config.epoch = 72;
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    let blocker = workers[1].world.listen(b).unwrap();
    workers[0].poll();
    workers[1].poll();
    assert_eq!(updates.status()["preparedWorkers"], 1);
    assert_eq!(updates.status()["rejected"], true);
    drop(blocker);
    // Retry preparation has completed, but its synchronous ready report has
    // not yet run. Use the exact production prepare/staged/decision/commit calls
    // separately so the blocked acknowledgment cannot deadlock the test thread.
    {
        let w = &mut workers[1];
        let _scope = w.world.enter();
        let candidate = w.node.preparing.take().unwrap().0;
        w.node.prepare(candidate, &mut w.ring).unwrap();
    }
    let blockers: Vec<_> = workers.iter().map(|w| w.world.listen(c).unwrap()).collect();
    config.revision = 3;
    config.epoch = 73;
    config.volumes[1].listen = c.to_string();
    let next = prepare_snapshot(&trust, config);
    let pause = updates.pause_candidate_replacement();
    let publishing = updates.clone();
    let mut publisher = Some(std::thread::spawn(move || {
        publishing.publish(next).unwrap()
    }));
    pause.entered.wait(); // R+1 has passed eligibility, holding current.
    let (current_locked, activation_locked) = updates.publication_locks();
    assert!(current_locked);
    if activation_locked {
        // Fixed ordering: R+1 wins. The retry cannot report until replacement.
        pause.resume.wait();
        publisher.take().unwrap().join().unwrap();
    }
    updates.staged(2, 1, true);
    let decision = updates.decision(2);
    let ack = if decision == Some(true) {
        let w = &mut workers[1];
        let _scope = w.world.enter();
        let staged = w.node.staged.take().unwrap();
        w.node.commit(staged);
        assert_eq!(
            w.node.servers[&b].handler().current._config.config.revision,
            2
        );
        let acknowledging = updates.clone();
        let (tx, rx) = std::sync::mpsc::channel();
        let ack = std::thread::spawn(move || {
            tx.send(acknowledging.publication_locks().0).unwrap();
            acknowledging.activated(2, 1);
        });
        assert!(
            rx.recv().unwrap(),
            "R acknowledgment must be blocked by publisher's current lock"
        );
        Some(ack)
    } else {
        None
    };
    if !activation_locked {
        pause.resume.wait();
        publisher.take().unwrap().join().unwrap();
    }
    if let Some(ack) = ack {
        ack.join().unwrap();
    }
    // R+1 now fails on both workers: any leaked R authority must remain visible.
    for w in &mut workers {
        w.poll();
    }
    let status = updates.status();
    eprintln!(
        "B04 publication race locks=({current_locked},{activation_locked}) decisionR={decision:?} worker revisions={:?} status={status} epoch={}",
        workers
            .iter()
            .map(|w| w.node.servers[&a].handler().current._config.config.revision)
            .collect::<Vec<_>>(),
        updates.applied_epoch()
    );
    assert_eq!(status["candidateRevision"], 3);
    assert_eq!(status["activeRevision"], 1);
    assert_eq!(status["rejected"], true);
    assert_eq!(status["activatedWorkers"], 0);
    assert_eq!(updates.applied_epoch(), 71);
    for (w, old) in workers.iter().zip(&old) {
        updates.assert_candidate_and_active(3, &old._config);
        assert!(
            Rc::ptr_eq(old, &w.node.servers[&a].handler().current),
            "B04 publication gap committed R behind last-good status"
        );
        assert!(old.active.get());
        assert_eq!(w.node.servers.len(), 1);
        assert!(w.node.staged.is_none());
        assert_eq!(w.node.preparing.as_ref().unwrap().0.config.revision, 3);
        drop(w.world.listen(b).unwrap()); // superseded stage released its listener
        assert!(w.world.listen(a).is_err()); // last-good listener still owned
        assert!(w.world.listen(c).is_err()); // external blocker still owns C
    }
    assert_eq!(decision, Some(false));
    assert!(
        activation_locked,
        "eligibility and replacement must hold activation continuously"
    );
    drop((old, blockers));
    for w in workers {
        w.finish();
    }
}

#[test]
fn b04_retry_wins_before_publication_and_receive_arm_is_committed() {
    for coordinated in [false, true] {
        let updates = Arc::new(Updates::default());
        let mut worker = Worker::new(&updates, 0);
        let (trust, mut config) = fixture();
        let a = config.volumes[0].listen.parse().unwrap();
        config.epoch = 81;
        let blocker = worker.world.listen(a).unwrap();
        let candidate = prepare_snapshot(&trust, config.clone());
        if coordinated {
            updates.command(candidate, 1).unwrap();
        } else {
            updates.publish(candidate).unwrap();
        }
        worker.poll();
        assert_eq!(updates.status()["rejected"], true);
        drop(blocker);
        {
            let _scope = worker.world.enter();
            let candidate = worker.node.preparing.take().unwrap().0;
            worker.node.prepare(candidate, &mut worker.ring).unwrap();
        }
        updates.staged(1, 0, true);
        if coordinated {
            updates
                .command(prepare_snapshot(&trust, config.clone()), 2)
                .unwrap();
            assert!(updates.receive_decision(1));
        } else {
            assert_eq!(updates.decision(1), Some(true));
        }
        let mut next = config.clone();
        next.revision = 2;
        next.epoch = 82;
        // Interpose a real publisher AFTER authorization but BEFORE local arm or
        // commit. All-ready (ordinary) / phase-2 (coordinated) makes it ineligible.
        let publish = updates.clone();
        let prepared = prepare_snapshot(&trust, next.clone());
        std::thread::spawn(move || {
            let result = if coordinated {
                publish.command(prepared, 1)
            } else {
                publish.publish(prepared)
            };
            assert_eq!(result.unwrap_err().kind(), io::ErrorKind::WouldBlock);
        })
        .join()
        .unwrap();
        assert_eq!(updates.status()["candidateRevision"], 1);
        assert_eq!(updates.status()["activeRevision"], 0);
        assert_eq!(updates.applied_epoch(), 0);
        let mut staged = worker.node.staged.take().unwrap();
        let generation = staged.generations[&a].clone();
        if coordinated {
            assert!(
                updates
                    .command(prepare_snapshot(&trust, config.clone()), 5)
                    .is_err()
            );
            let _scope = worker.world.enter();
            worker.node.arm(&mut staged);
            assert!(staged.armed);
            assert!(Rc::ptr_eq(
                &generation,
                &worker.node.servers[&a].handler().current
            ));
            assert!(!generation.active.get());
            assert!(worker.world.listen(a).is_err());
            assert_eq!(updates.status()["receiveReadyWorkers"], 1);
            assert_eq!(updates.decision(1), None);
            updates
                .command(prepare_snapshot(&trust, config.clone()), 3)
                .unwrap();
        }
        assert_eq!(updates.decision(1), Some(true));
        {
            let _scope = worker.world.enter();
            worker.node.commit(staged);
        }
        assert!(generation.active.get());
        // Publication must still wait for the acknowledgment after local commit.
        assert_eq!(
            updates
                .publish(prepare_snapshot(&trust, next.clone()))
                .unwrap_err()
                .kind(),
            io::ErrorKind::WouldBlock
        );
        updates.activated(1, 0);
        assert_eq!(updates.status()["activeRevision"], 1);
        assert_eq!(updates.applied_epoch(), 81);
        updates.assert_candidate_and_active(1, &generation._config);
        assert!(Rc::ptr_eq(
            &generation,
            &worker.node.servers[&a].handler().current
        ));
        if coordinated {
            updates
                .command(prepare_snapshot(&trust, config.clone()), 4)
                .unwrap();
            worker.poll();
            assert_eq!(updates.status()["retiredWorkers"], 1);
        }
        // Once the matching acknowledgment (and coordinated retirement) lands,
        // publication is allowed but does not itself replace serving authority.
        updates.publish(prepare_snapshot(&trust, next)).unwrap();
        assert_eq!(updates.status()["activeRevision"], 1);
        assert_eq!(updates.applied_epoch(), 81);
        updates.assert_candidate_and_active(2, &generation._config);
        assert!(generation.active.get());
        drop(generation);
        worker.finish();
    }
}

#[test]
fn b04_coordinated_retry_phases_and_terminal_abort() {
    for abort in [false, true] {
        let updates = Arc::new(Updates::default());
        let mut worker = Worker::new(&updates, 0);
        let (trust, mut config) = fixture();
        config.epoch = 61;
        let a = config.volumes[0].listen.parse().unwrap();
        let blocker = worker.world.listen(a).unwrap();
        updates
            .command(prepare_snapshot(&trust, config.clone()), 1)
            .unwrap();
        worker.poll();
        assert_eq!(updates.status()["preparedWorkers"], 0);
        // Identical publication is deliberately still a no-op, not a retry hook.
        updates
            .publish(prepare_snapshot(&trust, config.clone()))
            .unwrap();
        if abort {
            updates
                .command(prepare_snapshot(&trust, config.clone()), 5)
                .unwrap();
        }
        drop(blocker);
        worker.world.advance(Duration::from_millis(250));
        worker.poll();
        assert_eq!(updates.status()["activeRevision"], 0);
        assert!(worker.node.servers.is_empty());
        if abort {
            assert!(worker.node.preparing.is_none());
            assert!(worker.node.staged.is_none());
            updates.staged(1, 0, true);
            assert_eq!(updates.status()["preparedWorkers"], 0);
            assert_eq!(updates.decision(1), Some(false));
            assert!(
                updates
                    .command(prepare_snapshot(&trust, config.clone()), 1)
                    .is_err()
            );
            worker.world.advance(Duration::from_secs(120));
            worker.poll();
            drop(worker.world.listen(a).unwrap());
            config.revision = 2;
            updates
                .command(prepare_snapshot(&trust, config.clone()), 1)
                .unwrap();
            worker.poll();
        }
        assert_eq!(updates.status()["preparedWorkers"], 1);
        assert_eq!(updates.status()["rejected"], false);
        updates
            .command(prepare_snapshot(&trust, config.clone()), 2)
            .unwrap();
        worker.poll();
        assert_eq!(updates.status()["receiveReadyWorkers"], 1);
        assert!(!worker.node.servers[&a].handler().current.active.get());
        assert!(
            updates
                .command(prepare_snapshot(&trust, config.clone()), 5)
                .is_err()
        );
        updates
            .command(prepare_snapshot(&trust, config.clone()), 3)
            .unwrap();
        worker.poll();
        assert_eq!(updates.status()["activeRevision"], config.revision);
        assert!(worker.node.servers[&a].handler().current.active.get());
        updates
            .command(prepare_snapshot(&trust, config), 4)
            .unwrap();
        worker.poll();
        assert_eq!(updates.status()["retiredWorkers"], 1);
        worker.finish();
    }
}

#[test]
fn b04_retry_backoff_is_bounded_and_abort_drops_successful_stage() {
    let updates = Arc::new(Updates::default());
    let mut worker = Worker::new(&updates, 0);
    let (trust, config) = fixture();
    let a = config.volumes[0].listen.parse().unwrap();
    let blocker = worker.world.listen(a).unwrap();
    updates
        .command(prepare_snapshot(&trust, config.clone()), 1)
        .unwrap();
    for millis in [250, 500, 1000, 2000, 4000, 8000, 16000, 32000, 32000] {
        let work = worker.poll();
        assert!(!work.runnable);
        let retry_at = worker.node.preparing.as_ref().unwrap().1.after;
        assert_eq!(retry_at, worker.world.now() + Duration::from_millis(millis));
        assert!(work.deadline.is_some_and(|d| d <= retry_at));
        assert!(worker.node.crypto_sources.is_empty());
        worker.world.advance(Duration::from_millis(millis));
    }
    drop(blocker);
    worker.poll();
    assert!(worker.node.staged.is_some());
    assert!(worker.world.listen(a).is_err());
    updates
        .command(prepare_snapshot(&trust, config), 5)
        .unwrap();
    worker.poll();
    assert!(worker.node.staged.is_none());
    assert!(worker.node.preparing.is_none());
    drop(worker.world.listen(a).unwrap());
    worker.finish();
}

#[test]
fn b04_kernel_failed_bind_same_revision() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let updates = Arc::new(Updates::default());
    let mut node = volumes(&ring, &updates, 0);
    let (trust, mut config) = fixture();
    let a = address();
    config.volumes[0].listen = a.to_string();
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let old = node.servers[&a].handler().current.clone();
    let blocker = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let b = blocker.local_addr().unwrap();
    let mut extra = config.volumes[0].clone();
    extra.id = "B".into();
    extra.listen = b.to_string();
    config.volumes.push(extra);
    config.revision = 2;
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    node.poll(&mut ring, 16).unwrap();
    assert_eq!(updates.status()["rejected"], true);
    assert!(old.active.get());
    assert!(Rc::ptr_eq(&old, &node.servers[&a].handler().current));
    drop(blocker);
    let end = Instant::now() + Duration::from_secs(3);
    while updates.status()["activeRevision"] != 2 {
        ring.progress().unwrap();
        let work = node.poll(&mut ring, 16).unwrap();
        assert!(Instant::now() < end, "kernel same-revision retry stalled");
        if !work.runnable && updates.status()["activeRevision"] != 2 {
            ring.wait(Some(work.deadline.unwrap_or(end).min(end)))
                .unwrap();
        }
    }
    assert_eq!(node.servers.len(), 2);
    assert!(!old.active.get());
    std::net::TcpStream::connect(b).unwrap();
    node.shutdown(&mut ring).unwrap();
}

#[test]
fn b04_subscription_304_does_not_gate_runtime_retry() {
    use crate::control::{Source, Subscriber, proto};
    use prost::Message;
    use std::io::{Read, Write};
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let updates = Arc::new(Updates::default());
    let mut node = volumes(&ring, &updates, 0);
    let (trust, mut config) = fixture();
    let peer = "03".repeat(32);
    config.peers[0].id = peer.clone();
    config.volumes[0].peers = vec![peer.clone()];
    config.volumes[0].peer_endpoints.as_mut().unwrap().peers[0].peer = peer.clone();
    config.volumes[0].topology.as_mut().unwrap().neighbors[0].peer = peer;
    let a = address();
    config.volumes[0].listen = a.to_string();
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let blocker = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let b = blocker.local_addr().unwrap();
    let mut extra = config.volumes[0].clone();
    extra.id = "B".into();
    extra.listen = b.to_string();
    config.volumes.push(extra);
    config.revision = 2;
    let command_config = config.clone();
    let body = proto::Configuration {
        contents: Some(proto::configuration::Contents::Snapshot(config)),
    }
    .encode_to_vec();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let source = Source::parse(&format!(
        "https://{}/configuration",
        listener.local_addr().unwrap()
    ))
    .unwrap();
    let tls = crate::control::credentials::tests::Fixture::new();
    updates.set_credentials(tls.provider(1));
    let context = tls.context("spiffe://racer/controlplane", Some("localhost"));
    let done = Arc::new(AtomicBool::new(false));
    let not_modified = Arc::new(AtomicUsize::new(0));
    let stop = done.clone();
    let count = not_modified.clone();
    let server = std::thread::spawn(move || {
        let end = Instant::now() + Duration::from_secs(8);
        let mut first = true;
        while !stop.load(Ordering::Acquire) {
            assert!(Instant::now() < end, "subscription fixture watchdog");
            let (socket, _) = match listener.accept() {
                Ok(socket) => socket,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(1));
                    continue;
                }
                Err(e) => panic!("{e}"),
            };
            let mut socket = crate::control::credentials::tests::server(socket, &context);
            socket
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            let mut request = Vec::new();
            while !request.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                request.push(byte[0]);
                assert!(request.len() < 16384);
            }
            if first {
                first = false;
                use sha2::Digest;
                let request = String::from_utf8(request).unwrap();
                let boot = request
                    .lines()
                    .find_map(|line| line.strip_prefix("X-Racer-Boot: "))
                    .unwrap();
                let body = proto::ControlCommand {
                    universe: command_config.universe.clone(),
                    node: command_config.node.clone(),
                    revision: 2,
                    incarnation: (0..boot.len())
                        .step_by(2)
                        .map(|i| u8::from_str_radix(&boot[i..i + 2], 16).unwrap())
                        .collect(),
                    profile: 1,
                    phase: 3,
                    pod_uid: "test-pod".into(),
                    snapshot_digest: sha2::Sha256::digest(command_config.encode_to_vec()).to_vec(),
                    configuration: Some(proto::Configuration::decode(body.as_slice()).unwrap()),
                    ..Default::default()
                }
                .encode_to_vec();
                write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: \"R2\"\r\nConnection: close\r\n\r\n", body.len()).unwrap();
                socket.write_all(&body).unwrap();
            } else {
                assert!(
                    String::from_utf8(request)
                        .unwrap()
                        .to_ascii_lowercase()
                        .contains("if-none-match: \"r2\"")
                );
                socket.write_all(b"HTTP/1.1 304 Not Modified\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                count.fetch_add(1, Ordering::Release);
            }
        }
    });
    let subscriber = Subscriber::start(source, Arc::new(trust), updates.clone()).unwrap();
    let end = Instant::now() + Duration::from_secs(5);
    let mut blocker = Some(blocker);
    while updates.status()["activeRevision"] != 2 {
        ring.progress().unwrap();
        let work = node.poll(&mut ring, 16).unwrap();
        if blocker.is_some() && not_modified.load(Ordering::Acquire) > 0 {
            assert_eq!(updates.status()["rejected"], true);
            assert_eq!(updates.status()["activeRevision"], 1);
            blocker.take();
        }
        assert!(
            Instant::now() < end,
            "same revision stalled under production 304 subscription"
        );
        if !work.runnable && updates.status()["activeRevision"] != 2 {
            // Also observe the fixture's 304 counter; retry itself uses Work.deadline.
            let observe = Instant::now() + Duration::from_millis(20);
            ring.wait(Some(work.deadline.unwrap_or(observe).min(observe)))
                .unwrap();
        }
    }
    assert!(not_modified.load(Ordering::Acquire) > 0);
    assert_eq!(
        node.servers[&b].handler().current._config.config.revision,
        2
    );
    drop(subscriber);
    done.store(true, Ordering::Release);
    server.join().unwrap();
    node.shutdown(&mut ring).unwrap();
}

mod rdma_startup {
    use super::*;

    #[test]
    fn readiness24_deferred_registration_once_and_failed_holes_survive_reload() {
        let world = crate::simulation::World::new(2401);
        let _scope = world.enter();
        let mut ring = crate::conformance::ring(8, Default::default());
        let size = 32 * 1024 * 1024;
        let mut slab =
            crate::allocator::Slab::simulated(crate::simulation::Disk::new(size), size, 1, true)
                .unwrap();
        let cache = crate::cache::tests::cache_from_slab(&mut slab, 1, Default::default());
        let policy = rdma::StartupPolicy::parse(|name| match name {
            "RACER_RDMA_MODE" => Ok("enabled".into()),
            "RACER_RDMA_RAILS" => Ok("a:1:0,b:2:3,c:1:0".into()),
            _ => Err(std::env::VarError::NotPresent),
        })
        .unwrap();
        let mut volumes = Volumes::new(
            cache,
            Arc::new(Updates::default()),
            Arc::new(crate::crypto::Pool::test_pool(ring.pool())),
            0,
        )
        .with_rdma_startup(
            policy,
            vec![
                Some(rdma::Rail::test_candidate("a", 1, 0)),
                None,
                Some(rdma::Rail::test_candidate("c", 1, 0)),
            ],
        );
        let (trust, config) = crate::control::tests::rdma_fixture();
        let prepared = crate::control::tests::prepare_snapshot(&trust, config.clone());
        let mut http = config;
        http.fabric.clear();
        let http = crate::control::tests::prepare_snapshot(&trust, http);
        volumes
            .provision_rdma_with(&http, &ring, |_, _| panic!("no fabric must not register"))
            .unwrap();
        assert!(volumes.rdma_startup.is_some());
        assert!(volumes.rails.is_none());
        let mut calls = 0;
        volumes
            .provision_rdma_with(&prepared, &ring, |rail, config| {
                calls += 1;
                assert!(rail.name == "a" || rail.name == "c");
                assert_eq!((config.connections, config.depth), (8, 2));
                if rail.name == "a" {
                    Err(io::Error::other("mock registration failure"))
                } else {
                    let transport =
                        rdma::test_transport_config(ring.pool(), config.connections, config.depth);
                    let source = transport.test_source();
                    Ok((transport, source))
                }
            })
            .unwrap();
        assert_eq!(calls, 2);
        assert_eq!(volumes.rails.as_ref().unwrap().total(), 3);
        assert_eq!(volumes.rdma_sources.len(), 1);
        assert_eq!(volumes.rdma_sources[0].0, 2);
        for prepared in [&http, &prepared] {
            volumes
                .provision_rdma_with(prepared, &ring, |_, _| panic!("retry requires restart"))
                .unwrap();
        }
        // Exercise deferred source polling/arming and stopped-source HTTP containment.
        volumes.poll(&mut ring, 16).unwrap();
        let transport = volumes.rdma_sources[0].1.source.test_transport();
        let pending = transport.prepare([25; 16], 2, 3).unwrap();
        transport.test_block_destroy(true);
        drop(pending);
        volumes.rdma_sources[0]
            .1
            .failed(io::Error::other("mock device failure"));
        volumes.poll(&mut ring, 16).unwrap();
        assert!(volumes.rdma_sources[0].1.failed);
        assert!(!volumes.rdma_sources[0].1.quiesced);
        for _ in 0..100 {
            world.advance(Duration::from_millis(10));
            let work = volumes.poll(&mut ring, 16).unwrap();
            assert!(work.deadline.is_some());
            assert_eq!(transport.test_observe().qps, 1);
        }
        assert!(volumes.shutdown(&mut ring).is_err());
        transport.test_block_destroy(false);
        world.advance(Duration::from_millis(100));
        volumes.shutdown(&mut ring).unwrap();
        drop((volumes, ring));
        world.assert_clean();
    }
}
