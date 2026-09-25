// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[test]
fn cold_nonowner_streams_multiple_pages_with_independent_page_resolution_budgets() {
    use std::io::Read;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    let Some(mut ring) = crate::conformance::kernel_ring(32, uring::Config::default()) else {
        return;
    };
    let body: Arc<Vec<u8>> = Arc::new((0..BUFFER_SIZE + 123).map(|i| (i % 251) as u8).collect());
    let etag = crate::conformance::etag(&body);
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    origin.set_nonblocking(true).unwrap();
    let backend = Backend::new(
        &origin.local_addr().unwrap().to_string(),
        "large-peer-origin",
    )
    .unwrap();
    let stop = Arc::new(AtomicBool::new(false));
    let stopping = stop.clone();
    let gets = Arc::new(AtomicUsize::new(0));
    let served = gets.clone();
    let origin_body = body.clone();
    let origin_etag = etag.clone();
    let origin_thread = thread::spawn(move || {
        let mut tasks = Vec::new();
        while !stopping.load(Ordering::Acquire) {
            let (mut socket, _) = match origin.accept() {
                Ok(socket) => socket,
                Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                    thread::sleep(Duration::from_millis(1));
                    continue;
                }
                Err(error) => panic!("{error}"),
            };
            let body = origin_body.clone();
            let etag = origin_etag.clone();
            let served = served.clone();
            tasks.push(thread::spawn(move || {
                socket.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
                socket.set_write_timeout(Some(Duration::from_secs(10))).unwrap();
                let headers = request(&mut socket);
                if headers.starts_with("HEAD ") {
                    write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: {etag}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", body.len()).unwrap();
                } else {
                    assert!(headers.starts_with("GET "));
                    let range = headers.lines().find_map(|l| l.strip_prefix("Range: bytes=")).unwrap();
                    let (start, end) = range.split_once('-').unwrap();
                    let start: usize = start.parse().unwrap();
                    let end: usize = end.parse().unwrap();
                    assert!(headers.contains(&format!("If-Match: {etag}\r\n")));
                    assert!(end < body.len());
                    served.fetch_add(1, Ordering::Relaxed);
                    write!(socket, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes {start}-{end}/{}\r\nETag: {etag}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", end - start + 1, body.len()).unwrap();
                    socket.write_all(&body[start..=end]).unwrap();
                }
            }));
        }
        for task in tasks {
            task.join().unwrap();
        }
    });
    let bind = || {
        http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(64).unwrap(),
        )
        .unwrap()
    };
    let mut peer_listener = bind();
    let peer_address = peer_listener.local_addr().unwrap();
    let ca = crate::tls::tests::Authority::new();
    let aid = peer_identity(2);
    let bid = peer_identity(3);
    let ap =
        crate::control::credentials::Provider::for_test(aid.clone(), Arc::new(ca.context(&aid)));
    peer_listener.set_tls(ca.context(&bid), ExpectedPeer::Identity(aid));
    let (_, mut config) = crate::control::tests::fixture();
    config.volumes[0].peers = vec![bid.node.clone()];
    config.volumes[0]
        .topology
        .as_mut()
        .unwrap()
        .product
        .as_mut()
        .unwrap()
        .members[1] = bid.node.clone();
    let routing =
        Arc::new(crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap());
    let mut ingress = Handler::new(cache(&backend, 4), backend.clone());
    ingress.set_routing(
        routing,
        BTreeMap::from([(
            bid.node.clone(),
            Peer::new(&peer_address.to_string(), None).unwrap(),
        )]),
    );
    ingress.set_peer_tls("v1", ap, &BTreeMap::from([(bid.node.clone(), bid)]));
    let object = target(&ingress);
    let mut owner = Handler::new(cache(&backend, 4), backend);
    owner.set_authentication(peer_policy(3, 2));
    // Slot one is local at the owner; the request must cross HTTP from slot zero.
    config.volumes[0].peers = vec!["02".repeat(32)];
    let topology = config.volumes[0].topology.as_mut().unwrap();
    topology.local_slots = vec![1];
    topology.product.as_mut().unwrap().local_member = 1;
    owner.set_routing(
        Arc::new(crate::routing::Routing::new(&config.universe, &config.volumes[0]).unwrap()),
        BTreeMap::from([("02".repeat(32), Peer::new("127.0.0.1:1", None).unwrap())]),
    );
    let ingress_listener = bind();
    let address = ingress_listener.local_addr().unwrap();
    let mut ingress = http::Server::new(ingress_listener, ingress, http::Config::default());
    let mut owner = http::Server::new(peer_listener, owner, http::Config::default());
    let expected = body.clone();
    let client = thread::spawn(move || {
        let mut socket = std::net::TcpStream::connect(address).unwrap();
        socket
            .set_read_timeout(Some(Duration::from_secs(25)))
            .unwrap();
        write!(
            socket,
            "GET {object} HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"
        )
        .unwrap();
        let headers = request(&mut socket);
        assert!(headers.starts_with("HTTP/1.1 200"), "{headers}");
        assert!(headers.contains(&format!("Content-Length: {}\r\n", expected.len())));
        let mut received = Vec::new();
        socket.read_to_end(&mut received).unwrap();
        assert_eq!(
            received.len(),
            expected.len(),
            "cold nonowner stream truncated"
        );
        assert_eq!(blake3::hash(&received), blake3::hash(&expected));
    });
    let end = Instant::now() + Duration::from_secs(30);
    while !client.is_finished() {
        assert!(Instant::now() < end, "large peer stream stalled");
        ring.progress().unwrap();
        for server in [&mut ingress, &mut owner] {
            server.handler_mut().poll_background(&mut ring, 64).unwrap();
            server.poll(&mut ring, 64).unwrap();
        }
        thread::yield_now();
    }
    let result = client.join();
    stop.store(true, Ordering::Release);
    origin_thread.join().unwrap();
    for server in [&mut ingress, &mut owner] {
        server.shutdown(&mut ring).unwrap();
        server.handler_mut().shutdown(&mut ring).unwrap();
    }
    ring.shutdown().unwrap();
    result.unwrap();
    assert_eq!(
        gets.load(Ordering::Relaxed),
        body.len().div_ceil(BUFFER_SIZE)
    );
}
