// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0
use super::*;
use std::{
    io::{Read, Write},
    os::unix::net::UnixStream,
    sync::Barrier,
};

#[test]
fn authorization_uds_multihop_kernel() {
    crate::conformance::kernel_child(
        "handlers::tests::authorization::authorization_uds_multihop_child",
        "RACER_AUTH_CHILD",
    );
}

#[test]
#[ignore = "bounded real UDS, TLS and io_uring subprocess"]
fn authorization_uds_multihop_child() {
    if std::env::var_os("RACER_AUTH_CHILD").is_none() {
        return;
    }
    let mut rings = Vec::new();
    for _ in 0..3 {
        let pool = crate::buffers::io_test_pool_config(crate::buffers::Config {
            network_flights: std::num::NonZeroUsize::new(32).unwrap(),
            ..crate::buffers::Config::new(std::num::NonZeroUsize::new(4).unwrap())
        });
        let ring = Ring::http_test_ring(pool, Default::default()).unwrap();
        rings.push(ring);
    }
    let origin = TcpListener::bind("127.0.0.1:0").unwrap();
    let backend = Backend::new(&origin.local_addr().unwrap().to_string(), "auth-origin").unwrap();
    let concurrent = Arc::new(Barrier::new(2));
    let origin_thread = thread::spawn(move || {
        let mut workers = Vec::new();
        for _ in 0..6 {
            let mut socket = accept(&origin);
            let concurrent = concurrent.clone();
            workers.push(thread::spawn(move || {
                let wire = request(&mut socket);
                let auth = wire.lines().find_map(|l| l.strip_prefix("Authorization: ")).unwrap();
                let method = wire.split(' ').next().unwrap();
                let target = wire.split(' ').nth(1).unwrap();
                let status = match auth {
                    "Bearer deny-a" => { concurrent.wait(); 401 },
                    "Bearer deny-b" => { concurrent.wait(); 403 },
                    "Bearer page-denied" => if method == "HEAD" { 200 } else { 403 },
                    value => { assert_eq!(value, "x".repeat(65536)); 200 },
                };
                assert!(!target.contains("Bearer"));
                if status != 200 {
                    let framing = if status == 401 { "Content-Length: 999999" } else { "Transfer-Encoding: chunked" };
                    write!(socket, "HTTP/1.1 {status} Denied\r\n{framing}\r\nWWW-Authenticate: Bearer realm=\"registry\",scope=\"{auth}\"\r\nRetry-After: 7\r\nConnection: close\r\n\r\n5\r\nerror\r\n0\r\n\r\n").unwrap();
                } else {
                    write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nContent-Type: application/vnd.oci.image.manifest.v1+json\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"abc")).unwrap();
                    if method == "GET" {
                        assert!(wire.contains("Range: bytes=0-2\r\n"));
                        assert!(wire.contains("If-Match: "));
                        socket.write_all(b"abc").unwrap();
                    }
                }
            }));
        }
        for worker in workers {
            worker.join().unwrap();
        }
    });
    let ca = crate::tls::tests::Authority::new();
    let contexts: Vec<_> = (1..=3)
        .map(|n| ca.context(&peer_identity(n), false))
        .collect();
    let mut servers = Vec::new();
    let mut next = None;
    let path = std::env::temp_dir().join(format!("racer-auth-{}.sock", std::process::id()));
    for node in (1..=3).rev() {
        let mut handler = Handler::new(cache(&backend, 1), backend.clone());
        if let Some(address) = next {
            let mut peer = Peer::new(&format!("{address}"), None).unwrap();
            peer.http.set_tls(
                crate::control::credentials::Provider::for_test(
                    peer_identity(node),
                    contexts[node as usize - 1].clone(),
                ),
                peer_identity(node + 1),
            );
            handler.set_peer(peer);
        }
        let listener = if node == 1 {
            http::Listener::bind_unix(crate::socket::UnixPath::new(path.to_str().unwrap()).unwrap())
                .unwrap()
        } else {
            handler.set_authentication(peer_policy(node, node - 1));
            let mut listener = http::Listener::bind(
                "127.0.0.1:0".parse().unwrap(),
                std::num::NonZeroU32::new(16).unwrap(),
            )
            .unwrap();
            next = Some(listener.local_addr().unwrap());
            listener.set_tls(
                contexts[node as usize - 1].clone(),
                ExpectedPeer::Identity(peer_identity(node - 1)),
            );
            listener
        };
        servers.push(http::Server::new(
            listener,
            handler,
            http::Config::default(),
        ));
    }
    let clients = thread::spawn(move || {
        let mut clients = Vec::new();
        for (auth, target, code) in [
            ("Bearer deny-a".to_owned(), "/same", 401),
            ("Bearer deny-b".to_owned(), "/same", 403),
            ("x".repeat(65536), "/success", 200),
            ("Bearer page-denied".to_owned(), "/page-failure", 403),
        ] {
            let path = path.clone();
            clients.push(thread::spawn(move || {
                let mut socket = UnixStream::connect(path).unwrap();
                socket.set_read_timeout(Some(Duration::from_secs(10))).unwrap();
                write!(socket, "GET {target} HTTP/1.1\r\nHost: cache\r\nAuthorization: {auth}\r\nConnection: close\r\n\r\n").unwrap();
                let headers = request(&mut socket);
                assert!(headers.starts_with(&format!("HTTP/1.1 {code} ")), "{headers}");
                if code == 200 {
                    assert!(headers.contains("Content-Type: application/vnd.oci.image.manifest.v1+json\r\n"));
                    let mut body = [0;3];
                    socket.read_exact(&mut body).unwrap();
                    assert_eq!(&body, b"abc");
                } else {
                    assert!(headers.contains(&format!("scope=\"{auth}\"")), "{headers}");
                    assert!(headers.contains("Retry-After: 7\r\n"));
                }
            }));
        }
        for client in clients {
            client.join().unwrap();
        }
    });
    let end = Instant::now() + Duration::from_secs(15);
    while !clients.is_finished() {
        assert!(Instant::now() < end, "authorization multihop stalled");
        for (server, ring) in servers.iter_mut().zip(&mut rings) {
            ring.progress().unwrap();
            server.handler_mut().poll_background(ring, 16).unwrap();
            server.poll(ring, 16).unwrap();
        }
        thread::yield_now();
    }
    clients.join().unwrap();
    origin_thread.join().unwrap();
    for (server, ring) in servers.iter_mut().zip(&mut rings) {
        assert!(
            server
                .handler_mut()
                .upstream
                .backend
                .borrow()
                .breaker
                .available()
        );
        if let Some(peer) = &server.handler_mut().upstream.peer {
            assert!(peer.borrow().http.breaker.available());
        }
        server.shutdown(ring).unwrap();
        server.handler_mut().shutdown(ring).unwrap();
        ring.shutdown().unwrap();
    }
}
