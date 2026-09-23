// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

#[test]
fn production_peer_recovery_is_budgeted_and_never_replays_responses() {
    let Some(mut ring) = crate::conformance::kernel_ring(8, uring::Config::default()) else {
        return;
    };
    for ktls in [false, true] {
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
            let client_context = ca.context(&peer_identity(2), ktls);
            let server_context = ca.context(&peer_identity(3), ktls);
            let wrong = ca.context(
                &PeerIdentity::new(
                    &peer_identity(3).universe,
                    &peer_identity(3).node,
                    "wrong-pod",
                )
                .unwrap(),
                ktls,
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
