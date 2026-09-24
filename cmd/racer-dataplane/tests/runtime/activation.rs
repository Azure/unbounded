// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::control::tests::{fixture, prepare_snapshot};

mod management_tests {
    use super::*;

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
    fn b06_real_exporter_socket_default_custom_and_family_independence() {
        // Exercise default/custom bind IPs on kernel-assigned ports in the shared
        // test namespace. tests/bin/dataplane.rs asserts the default address;
        // e2e/racer/deployment_test.go's TestDeployment probes real daemons on 9090
        // in isolated Pod network namespaces, without a metrics-address override.
        for management in ["0.0.0.0:0", "127.0.0.1:0", "[::1]:0", "[::]:0"] {
            let management: SocketAddr = management.parse().unwrap();
            for initial in [false, true] {
                for occupied_tcp in [false, true] {
                    let updates = Arc::new(Updates::default());
                    let registry = Arc::new(crate::metrics::Registry::new(1, updates.clone()));
                    let exporter = crate::metrics::Exporter::start(management, registry).unwrap();
                    assert_eq!(exporter.address().ip(), management.ip());
                    assert_ne!(exporter.address().port(), 0);
                    let Some(mut ring) = crate::control::tests::ring() else {
                        return;
                    };
                    let mut node = volumes(&ring, &updates, 0);
                    let (trust, mut config) = fixture();
                    // Retain ownership from allocation through activation; reserving
                    // a port with address() and rebinding it would introduce a race.
                    let tcp =
                        occupied_tcp.then(|| std::net::TcpListener::bind("127.0.0.1:0").unwrap());
                    let a = tcp
                        .as_ref()
                        .map_or_else(address, |listener| listener.local_addr().unwrap());
                    config.volumes[0].client_socket =
                        crate::control::tests::test_socket(a, "client");
                    let a = Address::unix(&config.volumes[0].client_socket).unwrap();
                    config.epoch = 111;
                    if !initial {
                        updates
                            .publish(prepare_snapshot(&trust, config.clone()))
                            .unwrap();
                        node.poll(&mut ring, 16).unwrap();
                        check_http(&exporter, &updates, true);
                        config.revision = 2;
                    }
                    let old = node
                        .servers
                        .get(&a.into())
                        .map(|s| s.handler().current.clone());
                    let mut b = config.volumes[0].clone();
                    b.id = "B".into();
                    let unique = address();
                    b.client_socket = crate::control::tests::test_socket(unique, "client-b");
                    b.origin_socket = crate::control::tests::test_socket(unique, "origin-b");
                    let blocker = std::os::unix::net::UnixListener::bind(&b.client_socket).unwrap();
                    config.volumes.push(b);
                    config.epoch = 112;
                    updates
                        .publish(prepare_snapshot(&trust, config.clone()))
                        .unwrap();
                    let candidate = updates.latest(0).unwrap();
                    check_http(&exporter, &updates, !initial);
                    node.poll(&mut ring, 16).unwrap();
                    let status = updates.status();
                    eprintln!(
                        "B06 real management={} B={} initial={initial}: {status}",
                        exporter.address(),
                        config.volumes[1].client_socket
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
                        assert!(Rc::ptr_eq(old, &node.servers[&a.into()].handler().current));
                    }
                    check_http(&exporter, &updates, !initial);
                    let (code, metrics) = get(&exporter, "/metrics");
                    assert_eq!(code, 200);
                    assert!(metrics.contains(&format!(
                        "racer_dataplane_config_epoch {}\n",
                        if initial { 0 } else { 111 }
                    )));
                    // Releasing the local socket permits activation while management
                    // and the unrelated TCP owner remain live.
                    config.revision += 1;
                    drop(blocker);
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

pub(super) fn address() -> SocketAddr {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
}

#[test]
fn desired_state_keeps_working_listener_after_failure_and_converges_independently() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let updates = Arc::new(Updates::default());
    let mut node = volumes(&ring, &updates, 0);
    let (trust, mut config) = fixture();
    config.volumes[0].client_socket =
        crate::control::tests::test_socket(address(), "desired-client");
    let original = Address::unix(&config.volumes[0].client_socket).unwrap();
    updates
        .apply_desired(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let working = node.servers[&original].handler().current.clone();
    assert_eq!(updates.active().unwrap().config_snapshot().revision, 1);
    let blocked = crate::control::tests::test_socket(address(), "desired-blocked");
    let blocker = std::os::unix::net::UnixListener::bind(&blocked).unwrap();
    config.revision = 3;
    config.volumes[0].client_socket = blocked;
    updates
        .apply_desired(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    assert_eq!(updates.active().unwrap().config_snapshot().revision, 1);
    assert!(Rc::ptr_eq(
        &working,
        &node.servers[&original].handler().current
    ));
    assert!(working.active.get());
    assert_eq!(updates.status()["ready"], true);
    config.revision = 20;
    config.volumes[0].client_socket =
        crate::control::tests::test_socket(address(), "desired-recovered");
    updates
        .apply_desired(prepare_snapshot(&trust, config))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    assert_eq!(updates.active().unwrap().config_snapshot().revision, 20);
    assert_eq!(updates.status()["localState"], "applied");
    assert!(
        !node.retired.is_empty(),
        "old work drains after independent commit"
    );
    drop(blocker);
    node.shutdown(&mut ring).unwrap();
}

#[test]
fn independently_prepared_generation_has_no_serving_authority_until_commit() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let (trust, mut config) = fixture();
    let address = address();
    config.volumes[0].client_socket = crate::control::tests::test_socket(address, "client");
    let address = Address::unix(&config.volumes[0].client_socket).unwrap();
    let updates = Arc::new(Updates::default());
    let mut volumes = volumes(&ring, &updates, 0);
    let prepare = |s| prepare_snapshot(&trust, s);
    // A second worker delays the local commit, without any remote phase.
    updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers.is_empty());
    assert!(!updates.status()["ready"].as_bool().unwrap());
    let candidate = volumes.staged.as_ref().unwrap().generations[&address].clone();
    assert!(!candidate.active.get());
    assert!(!updates.status()["ready"].as_bool().unwrap());
    updates.staged(1, 1, true);
    volumes.poll(&mut ring, 16).unwrap();
    assert!(candidate.active.get());
    updates.activated(1, 1);
    assert!(updates.status()["ready"].as_bool().unwrap());
    config.revision = 2;
    config.volumes[0].topology.as_mut().unwrap().epoch = 2;
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(
        volumes.servers[&address.into()]
            .handler()
            .draining
            .is_empty()
    );
    let handler = volumes.servers[&address.into()].handler();
    assert!(Rc::ptr_eq(&handler.current, &candidate));
    assert!(handler.current.active.get());
    assert!(handler.draining.is_empty());
    updates.staged(2, 1, true);
    volumes.poll(&mut ring, 16).unwrap();
    updates.activated(2, 1);
    assert!(!candidate.active.get());
    assert!(
        volumes.servers[&address.into()]
            .handler()
            .current
            .active
            .get()
    );
    assert_eq!(volumes.servers[&address.into()].handler().draining.len(), 1);
    volumes.shutdown(&mut ring).unwrap();
}

#[test]
fn storage_fence_preserves_preparation_until_local_commit() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let (trust, mut config) = fixture();
    config.volumes[0].client_socket = crate::control::tests::test_socket(address(), "client");
    let updates = Arc::new(Updates::default());
    let mut node = volumes(&ring, &updates, 0);
    updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    node.poll(&mut ring, 16).unwrap();
    let staged = node
        .staged
        .as_ref()
        .unwrap()
        .generations
        .values()
        .next()
        .unwrap()
        .clone();
    node.storage_maintenance(true);
    updates.staged(1, 1, true);
    node.poll(&mut ring, 16).unwrap();
    assert!(node.servers.is_empty());
    assert!(!staged.active.get());
    assert!(updates.active().is_none());
    node.storage_maintenance(false);
    node.poll(&mut ring, 16).unwrap();
    updates.activated(1, 1);
    assert!(staged.active.get());
    assert!(Rc::ptr_eq(
        &node.servers.values().next().unwrap().handler().current,
        &staged
    ));
    assert_eq!(updates.status()["activeRevision"], 1);
    node.shutdown(&mut ring).unwrap();
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
    config.volumes[0].client_socket = crate::control::tests::test_socket(a, "client");
    let a = Address::unix(&config.volumes[0].client_socket).unwrap();
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let old = node.servers[&a.into()].handler().current.clone();
    let b = address();
    let path = crate::control::tests::test_socket(b, "client");
    let blocker = std::os::unix::net::UnixListener::bind(&path).unwrap();
    let mut extra = config.volumes[0].clone();
    extra.id = "B".into();
    extra.client_socket = crate::control::tests::test_socket(b, "client");
    extra.origin_socket = crate::control::tests::test_socket(b, "origin");
    config.volumes.push(extra);
    config.revision = 2;
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    node.poll(&mut ring, 16).unwrap();
    assert_eq!(updates.status()["rejected"], true);
    assert!(old.active.get());
    assert!(Rc::ptr_eq(&old, &node.servers[&a.into()].handler().current));
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
    std::os::unix::net::UnixStream::connect(path).unwrap();
    node.shutdown(&mut ring).unwrap();
}

#[test]
fn superseded_local_preparation_never_commits_or_leaks_listener_authority() {
    let Some(mut ring) = crate::control::tests::ring() else {
        return;
    };
    let updates = Arc::new(Updates::default());
    let mut node = volumes(&ring, &updates, 0);
    updates.subscribe(Arc::new(uring::Wake::new().unwrap()));
    let (trust, mut config) = fixture();
    config.volumes[0].client_socket = crate::control::tests::test_socket(address(), "stale-stage");
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let stale = node
        .staged
        .as_ref()
        .unwrap()
        .generations
        .values()
        .next()
        .unwrap()
        .clone();
    assert!(!stale.active.get());
    config.revision = 9;
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    // Simulate completion of work that started before desired replacement.
    updates.staged(1, 1, true);
    updates.activated(1, 1);
    node.poll(&mut ring, 16).unwrap();
    assert!(node.servers.is_empty());
    assert_eq!(node.staged.as_ref().unwrap().revision, 9);
    updates.staged(9, 1, true);
    node.poll(&mut ring, 16).unwrap();
    updates.activated(9, 1);
    assert!(!stale.active.get());
    assert_eq!(updates.active().unwrap().config_snapshot().revision, 9);
    assert_eq!(node.servers.len(), 1);
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
    // This fixture installs TLS after initial activation. Use a distinct local
    // peer IP so its fixed port does not overlap concurrent listener fixtures.
    let mut node = volumes(&ring, &updates, 0).with_peer_ip("127.254.0.4".parse().unwrap());
    let (trust, mut config) = fixture();
    let peer = "03".repeat(32);
    config.peers[0].id = peer.clone();
    config.volumes[0].peers = vec![peer.clone()];
    config.volumes[0].peer_endpoints.as_mut().unwrap().peers[0].peer = peer.clone();
    config.volumes[0]
        .topology
        .as_mut()
        .unwrap()
        .product
        .as_mut()
        .unwrap()
        .members[1] = peer;
    let a = address();
    config.volumes[0].client_socket = crate::control::tests::test_socket(a, "client");
    updates
        .publish(prepare_snapshot(&trust, config.clone()))
        .unwrap();
    node.poll(&mut ring, 16).unwrap();
    let b = address();
    let path = crate::control::tests::test_socket(b, "client");
    let blocker = std::os::unix::net::UnixListener::bind(&path).unwrap();
    let mut extra = config.volumes[0].clone();
    extra.id = "B".into();
    extra.client_socket = crate::control::tests::test_socket(b, "client");
    extra.origin_socket = crate::control::tests::test_socket(b, "origin");
    let b = Address::unix(&extra.client_socket).unwrap();
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
        "https://{}/v1/config",
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
                if socket.read_exact(&mut byte).is_err() {
                    break;
                }
                request.push(byte[0]);
                assert!(request.len() < 16384);
            }
            if !request.ends_with(b"\r\n\r\n") {
                continue;
            }
            if first {
                first = false;
                use sha2::Digest;
                let request = String::from_utf8(request).unwrap();
                let boot = request
                    .lines()
                    .find_map(|line| line.strip_prefix("X-Racer-Boot: "))
                    .unwrap();
                let body = proto::DesiredState {
                    universe: command_config.universe.clone(),
                    node: command_config.node.clone(),
                    revision: 2,
                    incarnation: (0..boot.len())
                        .step_by(2)
                        .map(|i| u8::from_str_radix(&boot[i..i + 2], 16).unwrap())
                        .collect(),
                    profile: 1,
                    cursor: "revision-2".into(),
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
        node.servers[&b.into()]
            .handler()
            .current
            ._config
            .config_snapshot()
            .revision,
        2
    );
    drop(subscriber);
    done.store(true, Ordering::Release);
    server.join().unwrap();
    node.shutdown(&mut ring).unwrap();
}

pub(crate) fn volumes(ring: &uring::Ring, updates: &Arc<Updates>, worker: usize) -> Volumes {
    updates.subscribe(ring.wake_handle());
    Volumes::new(
        crate::cache::tests::cache(1),
        updates.clone(),
        Arc::new(crate::crypto::Pool::test_pool(ring.pool())),
        worker,
    )
}
