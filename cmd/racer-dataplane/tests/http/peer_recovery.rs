// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn peer_crc_rejection_leaves_no_cached_value_and_allows_healthy_refetch() {
    let Some(mut ring) = crate::conformance::kernel_ring(4, uring::Config::default()) else {
        return;
    };
    let pool = crate::crypto::Pool::test_pool(ring.pool());
    let (worker, source) = pool.attach_local(ring.pool(), ring.wake_handle()).unwrap();
    let worker = Rc::new(RefCell::new(worker));
    for metadata in [false, true] {
        for bad in [
            "missing",
            "empty",
            "malformed",
            "duplicate",
            "metadata-flip",
        ] {
            if !metadata && bad == "metadata-flip" {
                continue; // Payload flips already exercise async CRC in cache_persistence.
            }
            let record = crate::metadata::Metadata {
                checksum: crate::metadata::Checksum(*blake3::hash(b"abc").as_bytes()),
                len: 3,
                expires: crate::environment::wall()
                    .duration_since(std::time::UNIX_EPOCH)
                    .unwrap()
                    .as_secs()
                    + 60,
                content_type: Default::default(),
            };
            let bytes = if metadata {
                record.to_bytes().to_vec()
            } else {
                b"abc".to_vec()
            };
            let crc = crate::allocator::crc64(&bytes);
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            let address = listener.local_addr().unwrap();
            let tls = PeerTls::new();
            let client_context = tls.client.clone();
            let server = thread::spawn(move || {
                for healthy in [false, true] {
                    let end = deadline();
                    let socket = loop {
                        match listener.accept() {
                            Ok((socket, _)) => break socket,
                            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                                assert!(Instant::now() < end, "missing refetch: {metadata}/{bad}");
                                thread::sleep(Duration::from_millis(1));
                            }
                            Err(e) => panic!("accept: {e}"),
                        }
                    };
                    socket.set_nonblocking(true).unwrap();
                    let mut stream = PeerStream::new(
                        TlsSession::server(
                            &tls.server,
                            socket.into(),
                            ExpectedPeer::Identity(peer_identity(2)),
                        )
                        .unwrap(),
                        peer_identity(2),
                    );
                    assert!(request(&mut stream).starts_with("GET / HTTP/1.1\r\n"));
                    let valid = format!("X-Racer-Crc64: {crc:016x}\r\n");
                    let header = match (healthy, bad) {
                        (false, "missing") => String::new(),
                        (false, "empty") => "X-Racer-Crc64: \r\n".into(),
                        (false, "malformed") => "X-Racer-Crc64: not-hex\r\n".into(),
                        (false, "duplicate") => valid.repeat(2),
                        _ => valid,
                    };
                    let mut body = bytes.clone();
                    if !healthy && bad == "metadata-flip" {
                        // Keep a decodable record and matching ETag, but change its length
                        // without updating the origin-admission CRC.
                        body[32] ^= 1;
                    }
                    write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: {}\r\n{header}Connection: close\r\n\r\n", body.len(), record.checksum.etag().as_str()).unwrap();
                    stream.write_all(&body).unwrap();
                }
            });
            let backend = Backend::new("127.0.0.1:1", "crc-origin").unwrap();
            let context =
                cache::Context::new(backend.namespace()).with_crypto(Some(worker.clone()));
            let mut handler = Handler::new(cache(&backend, 1), backend);
            handler.set_peer(Peer::new(&address.to_string(), None).unwrap());
            let (trust, config) = crate::control::tests::fixture();
            let prepared = trust
                .prepare(crate::control::proto::Configuration {
                    contents: Some(crate::control::proto::configuration::Contents::Snapshot(
                        config,
                    )),
                })
                .unwrap();
            let routing = prepared.volumes()[0].routing().clone();
            let target = (0..)
                .map(|n| format!("/crc-{n}"))
                .find(|t| routing.start(t).owner == 1)
                .unwrap();
            handler.upstream.active = Some(Rc::new(RefCell::new(RouteState {
                cursor: routing.start(&target),
                origin: true,
                exhausted: false,
            })));
            handler.upstream.routing = Some(routing);
            // One direct peer hop leaves room for the native TLS scratch buffers.
            handler.upstream.receive_rank = Some(1);
            handler
                .upstream
                .peer
                .as_ref()
                .unwrap()
                .borrow_mut()
                .http
                .set_tls(
                    crate::control::credentials::Provider::for_test(
                        peer_identity(2),
                        Arc::new(client_context),
                    ),
                    peer_identity(3),
                );
            for attempt in 0..3 {
                if attempt == 1 {
                    let peer = handler.upstream.peer.as_ref().unwrap();
                    assert!(!peer.borrow().http.breaker.available());
                    let end = deadline();
                    while !peer.borrow().http.breaker.available() {
                        assert!(Instant::now() < end, "CRC failure breaker never reopened");
                        thread::sleep(Duration::from_millis(1));
                    }
                }
                if attempt == 2 {
                    // A successful refetch must now be cached, even with no peer.
                    handler.upstream.peer = None;
                }
                let descriptor = if metadata {
                    cache::PeerDescriptor::metadata(&target)
                } else {
                    cache::PeerDescriptor::page(
                        &target,
                        cache::PeerPage::new(0, 3, record.checksum),
                    )
                };
                let end = deadline();
                let mut fault = handler
                    .cache
                    .borrow_mut()
                    .peer_fault_in::<Provider>(&context, descriptor, end)
                    .unwrap();
                let result = loop {
                    assert!(Instant::now() < end, "stalled: {metadata}/{bad}/{attempt}");
                    ring.progress().unwrap();
                    handler.cache.borrow_mut().poll(&mut ring, 16).unwrap();
                    match handler.cache.borrow_mut().poll_value(
                        fault,
                        &mut ring,
                        &mut handler.upstream,
                    ) {
                        Ok(cache::Progress::Pending { fault: next, .. }) => fault = next,
                        result => break result,
                    }
                    thread::yield_now();
                };
                if attempt == 0 {
                    assert!(
                        result.is_err(),
                        "published invalid peer CRC: {metadata}/{bad}"
                    );
                    let state = handler.upstream.active.as_ref().unwrap().borrow();
                    assert!(
                        !handler
                            .upstream
                            .owners
                            .borrow()
                            .blocked(state.cursor.identity, 1)
                    );
                    assert_eq!(state.cursor.attempt, 0);
                } else {
                    if let Err(error) = &result {
                        panic!(
                            "healthy refetch/cache hit failed: {metadata}/{bad}/{attempt}: {error}"
                        );
                    }
                    let Ok(cache::Progress::Ready(value)) = result else {
                        panic!("healthy refetch/cache hit failed: {metadata}/{bad}/{attempt}");
                    };
                    assert_eq!(value.checksum(), Some(crc));
                    if metadata {
                        let cache::CachedValue::Metadata(actual) = value else {
                            panic!("metadata expected")
                        };
                        assert_eq!(actual.to_bytes(), record.to_bytes());
                    } else {
                        assert!(matches!(value, cache::CachedValue::File(_)));
                    }
                }
            }
            server.join().unwrap();
            handler.shutdown(&mut ring).unwrap();
        }
    }
    drop((source, worker));
    pool.shutdown().unwrap();
    ring.shutdown().unwrap();
}

#[test]
fn production_peer_recovery_is_budgeted_and_never_replays_responses() {
    let Some(mut ring) = crate::conformance::kernel_ring(8, uring::Config::default()) else {
        return;
    };
    {
        for cut in [
            "close",
            "twice",
            "partial",
            "informational",
            "body",
            "401",
            "403",
            "exhausted",
            "wrong-pod",
            "deadline",
        ] {
            let ca = crate::tls::tests::Authority::new();
            let client_context = ca.context(&peer_identity(2));
            let server_context = ca.context(&peer_identity(3));
            let wrong = ca.context(
                &PeerIdentity::new(
                    &peer_identity(3).universe,
                    &peer_identity(3).node,
                    "wrong-pod",
                )
                .unwrap(),
            );
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let address = listener.local_addr().unwrap();
            let (stop_tx, stop_rx) = std::sync::mpsc::channel();
            let server = thread::spawn(move || {
                let accept = |context: &TlsContext| {
                    let socket = listener.accept().unwrap().0;
                    socket.set_nonblocking(true).unwrap();
                    PeerStream::new(
                        TlsSession::server(
                            context,
                            socket.into(),
                            ExpectedPeer::Identity(peer_identity(2)),
                        )
                        .unwrap(),
                        peer_identity(2),
                    )
                };
                let mut stream = accept(&server_context);
                assert!(request(&mut stream).starts_with("GET /warm "));
                stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                    .unwrap();
                let first = request(&mut stream);
                let chain = |headers: &str| {
                    let wire = headers
                        .lines()
                        .find_map(|l| l.strip_prefix("X-Racer-Fault: "))
                        .unwrap();
                    let bytes = unhex(wire).unwrap();
                    let (_, hops, work, _) = cache::peer_wire::chain(&bytes).unwrap().unwrap();
                    (hops, work)
                };
                let first_chain = chain(&first);
                match cut {
                    "partial" => stream.write_all(b"H").unwrap(),
                    "informational" => stream
                        .write_all(b"HTTP/1.1 103 Early Hints\r\n\r\n")
                        .unwrap(),
                    "body" => stream
                        .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\na")
                        .unwrap(),
                    "401" => stream
                        .write_all(b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n")
                        .unwrap(),
                    "403" => stream
                        .write_all(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
                        .unwrap(),
                    "deadline" => thread::sleep(Duration::from_millis(1100)),
                    _ => {}
                }
                drop(stream);
                if matches!(cut, "close" | "twice" | "wrong-pod") {
                    let mut stream = accept(if cut == "wrong-pod" {
                        &wrong
                    } else {
                        &server_context
                    });
                    if cut != "wrong-pod" {
                        let second = request(&mut stream);
                        let second_chain = chain(&second);
                        assert!(second_chain.0 < first_chain.0);
                        assert!(second_chain.1 < first_chain.1);
                        assert_ne!(
                            first, second,
                            "recovery must not copy remote work authority"
                        );
                        if cut == "close" {
                            write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nX-Racer-Crc64: {:016x}\r\n\r\nabc", crate::conformance::etag(b"abc"), crate::allocator::crc64(b"abc")).unwrap();
                        }
                    }
                    drop(stream);
                }
                stop_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                listener.set_nonblocking(true).unwrap();
                assert_eq!(
                    listener.accept().unwrap_err().kind(),
                    io::ErrorKind::WouldBlock,
                    "unexpected replay: {cut}"
                );
            });
            let backend = Backend::new("127.0.0.1:1", "recovery-origin").unwrap();
            let mut handler = Handler::new(cache(&backend, 1), backend);
            handler.set_peer(Peer::new(&address.to_string(), None).unwrap());
            let credentials = crate::control::credentials::Provider::for_test(
                peer_identity(2),
                Arc::new(client_context),
            );
            let peer = handler.upstream.peer.as_ref().unwrap().clone();
            peer.borrow_mut()
                .http
                .set_tls(credentials, peer_identity(3));
            let (connection, permit) = peer.borrow_mut().http.connection().unwrap();
            let mut warm = connection
                .get_small(client::Request::new("/warm", &[]).unwrap(), 1, deadline())
                .unwrap();
            loop {
                ring.progress().unwrap();
                if let Progress::Ready(response) = warm.poll(&mut ring, 32).unwrap() {
                    peer.borrow_mut().http.recycle(response.recycle());
                    permit.success();
                    break;
                }
            }
            let page = page_request(&mut handler.cache.borrow_mut(), &mut ring, "/recovery");
            let (authority, dest) = destination(&ring, *page.key());
            if cut == "exhausted" {
                *handler.upstream.chain.borrow_mut() = Chain { hops: 1, work: 1 };
            }
            let mut exchange = handler
                .upstream
                .start(UpstreamRequest::PeerPage(page), dest, deadline(), &mut ring)
                .unwrap();
            let original = match &exchange {
                Exchange::Get(p) => match &p.exchange {
                    HttpGet::Payload(e) => e.deadlines_for_test(),
                    _ => unreachable!(),
                },
                _ => unreachable!(),
            };
            let end = deadline();
            let result = loop {
                assert!(Instant::now() < end);
                ring.progress().unwrap();
                match handler.upstream.poll(exchange, &mut ring) {
                    Ok(ExchangeProgress::Pending { exchange: next, .. }) => {
                        if let Exchange::Get(p) = &next {
                            if let HttpGet::Payload(e) = &p.exchange {
                                let current = e.deadlines_for_test();
                                assert!(current.0 <= original.0);
                                assert_eq!(current.1, original.1);
                            }
                        }
                        exchange = next;
                    }
                    other => break other,
                }
            };
            if cut == "close" {
                if let Err(error) = &result {
                    panic!("recovery failed: {error}");
                }
                let Ok(ExchangeProgress::ReadyPeer {
                    result: UpstreamResult::PeerPage(received),
                    retry,
                }) = result
                else {
                    panic!("recovery failed: {cut}")
                };
                let mut fill = authority.reunite(received.destination).ok().unwrap();
                assert_eq!(&fill.as_mut_slice()[..received.len], b"abc");
                drop(retry);
                assert!(peer.borrow().http.breaker.available());
            } else {
                assert!(result.is_err(), "must reject {cut}");
            }
            stop_tx.send(()).unwrap();
            server.join().unwrap();
            handler.shutdown(&mut ring).unwrap();
        }
    }
    ring.shutdown().unwrap();
}
