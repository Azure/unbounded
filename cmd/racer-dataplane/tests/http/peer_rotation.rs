// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn peer_rotation_preserves_head_and_object_reads() {
    let status = std::process::Command::new("timeout")
        .args(["--signal=KILL", "90s"])
        .arg(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "handlers::tests::mixed_version::peer_rotation_child",
            "--ignored",
            "--nocapture",
        ])
        .env("RACER_PEER_ROTATION_CHILD", "1")
        .status()
        .unwrap();
    assert!(status.success());
}

#[test]
#[ignore = "bounded real TLS rotation subprocess"]
fn peer_rotation_child() {
    if std::env::var_os("RACER_PEER_ROTATION_CHILD").is_none() {
        return;
    }
    let Some(mut ring) = crate::conformance::kernel_ring(16, uring::Config::default()) else {
        return;
    };
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    let backend =
        Backend::new(&origin.local_addr().unwrap().to_string(), "rotation-origin").unwrap();
    let body = b"rotation object bytes";
    let etag = crate::conformance::etag(body);
    let origin_thread = thread::spawn(move || {
        // Five distinct metadata keys and one page miss.
        for _ in 0..6 {
            let (mut socket, _) = origin.accept().unwrap();
            let headers = request(&mut socket);
            if headers.starts_with("HEAD ") {
                write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: {etag}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", body.len()).unwrap();
            } else {
                write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes 0-{}/{}\r\nETag: {etag}\r\nConnection: close\r\n\r\n", body.len(), body.len() - 1, body.len()).unwrap();
                socket.write_all(body).unwrap();
            }
        }
    });
    let bind = || {
        http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(64).unwrap(),
        )
        .unwrap()
    };
    let mut listener = bind();
    let ca = crate::tls::tests::Authority::new();
    let aid = peer_identity(2);
    let bid = peer_identity(3);
    let provider = crate::control::credentials::Provider::for_test(
        aid.clone(),
        Arc::new(ca.context(&aid, false)),
    );
    listener.set_tls(ca.context(&bid, false), ExpectedPeer::Identity(aid.clone()));
    listener.set_tls_revision(1);
    let (_, mut config) = crate::control::tests::fixture();
    config.volumes[0].peers = vec![bid.node.clone()];
    config.volumes[0].topology.as_mut().unwrap().neighbors[0].peer = bid.node.clone();
    let mut ingress = Handler::new(cache(&backend, 4), backend.clone());
    ingress.set_attempt_policy(1).unwrap();
    ingress.set_routing(
        Arc::new(crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap()),
        BTreeMap::from([(
            bid.node.clone(),
            Peer::new(&listener.local_addr().unwrap().to_string(), None).unwrap(),
        )]),
    );
    ingress.set_peer_tls(
        "v1",
        provider,
        &BTreeMap::from([(bid.node.clone(), bid.clone())]),
    );
    let objects: Vec<_> = (0..)
        .map(|i| format!("/rotation-{i}"))
        .filter(|t| {
            let key = cache::PeerDescriptor::metadata(t)
                .key(ingress.namespace)
                .unwrap();
            ingress
                .upstream
                .routing
                .as_ref()
                .unwrap()
                .start_key(&key)
                .owner
                == 1
        })
        .take(5)
        .collect();
    let mut owner = Handler::new(cache(&backend, 4), backend);
    owner.set_authentication(peer_policy(3, 2));
    config.volumes[0].peers = vec![aid.node.clone()];
    let topology = config.volumes[0].topology.as_mut().unwrap();
    topology.local_slots = vec![1];
    topology.neighbors[0].slot = 0;
    topology.neighbors[0].peer = aid.node.clone();
    owner.set_routing(
        Arc::new(crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap()),
        BTreeMap::from([(aid.node.clone(), Peer::new("127.0.0.1:1", None).unwrap())]),
    );
    let local = bind();
    let address = local.local_addr().unwrap();
    let mut ingress = http::Server::new(local, ingress, http::Config::default());
    let mut owner = http::Server::new(listener, owner, http::Config::default());
    let read = |ring: &mut Ring,
                ingress: &mut http::Server<Handler>,
                owner: &mut http::Server<Handler>,
                object: &str,
                head: bool| {
        let connection = client::Connection::new(address, "localhost").unwrap();
        let end = Instant::now() + Duration::from_secs(5);
        let req = client::Request::new(object, &[]).unwrap();
        let mut head_request = if head {
            Some(connection.head(req, end).unwrap())
        } else {
            None
        };
        let mut get_request = if head {
            None
        } else {
            Some(
                client::Connection::new(address, "localhost")
                    .unwrap()
                    .get_small(req, 128, end)
                    .unwrap(),
            )
        };
        loop {
            assert!(Instant::now() < end);
            ring.progress().unwrap();
            for server in [&mut *ingress, &mut *owner] {
                server.handler_mut().poll_background(ring, 64).unwrap();
                server.poll(ring, 64).unwrap();
            }
            if let Some(request) = &mut head_request {
                if let Progress::Ready(response) = request.poll(ring, 64).unwrap() {
                    assert_eq!(response.status(), 200, "HEAD after remote credential drain");
                    assert_eq!(response.content_length(), Some(body.len() as u64));
                    break;
                }
            } else if let Progress::Ready(response) =
                get_request.as_mut().unwrap().poll(ring, 64).unwrap()
            {
                assert_eq!(response.status(), 200, "GET after remote credential drain");
                assert_eq!(response.body(), body);
                break;
            }
        }
    };
    read(&mut ring, &mut ingress, &mut owner, &objects[0], true);
    for (revision, index, head) in [(2, 1, true), (3, 3, false)] {
        for _ in 0..128 {
            ring.progress().unwrap();
            ingress
                .handler_mut()
                .poll_background(&mut ring, 64)
                .unwrap();
            ingress.poll(&mut ring, 64).unwrap();
            owner.handler_mut().poll_background(&mut ring, 64).unwrap();
            owner.poll(&mut ring, 64).unwrap();
        }
        assert_eq!(owner.connections(), 1);
        let handshakes = crate::tls::global_counters().handshakes;
        owner.install_tls(
            ca.context(&bid, false),
            ExpectedPeer::Identity(aid.clone()),
            revision,
            u64::MAX,
        );
        // Keep the client pool young while only the remote credential changes.
        // Refresh via a real peer request halfway through the production 30s grace.
        thread::sleep(Duration::from_secs(15));
        read(&mut ring, &mut ingress, &mut owner, &objects[index], true);
        assert_eq!(
            crate::tls::global_counters().handshakes,
            handshakes,
            "must reuse actual peer TLS connection"
        );
        thread::sleep(Duration::from_millis(15100));
        for _ in 0..32 {
            ring.progress().unwrap();
            owner.poll(&mut ring, 64).unwrap();
        }
        assert_eq!(
            owner.connections(),
            0,
            "old credential connection was retired"
        );
        // A unique key forces metadata recovery; the final GET also verifies page bytes.
        read(
            &mut ring,
            &mut ingress,
            &mut owner,
            &objects[index + 1],
            head,
        );
    }
    for server in [&mut ingress, &mut owner] {
        server.shutdown(&mut ring).unwrap();
        server.handler_mut().shutdown(&mut ring).unwrap();
    }
    ring.shutdown().unwrap();
    origin_thread.join().unwrap();
}
