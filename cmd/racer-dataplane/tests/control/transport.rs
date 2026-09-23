// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

fn persistent_request(socket: &mut credentials::Stream) -> String {
    let mut request = Vec::new();
    while !request.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        socket.read_exact(&mut byte).unwrap();
        request.push(byte[0]);
        assert!(request.len() < 32768);
    }
    let request = String::from_utf8(request).unwrap();
    assert!(!request.contains("Connection: close"));
    assert!(request.contains("Prefer: wait=28\r\n"));
    request
}

fn persistent_reply(socket: &mut credentials::Stream, body: &[u8], etag: &str) {
    let mut response = format!(
        "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nETag: {etag}\r\n\r\n",
        body.len()
    )
    .into_bytes();
    response.extend_from_slice(body);
    socket.write_all(&response).unwrap();
}

#[test]
fn persistent_subscriber_retains_identity_and_updates_headers() {
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    let (mut socket, request, _) = server.next();
    assert!(!request.contains("Connection: close"));
    persistent_reply(&mut socket, &signed(&trust, config.clone()), "\"one\"");
    for _ in 0..4 {
        let request = persistent_request(&mut socket);
        assert!(request.contains("If-None-Match: \"one\""));
        assert!(request.contains("X-Racer-Old-Connections: 0"));
        assert!(server.requests.try_recv().is_err(), "unnecessary handshake");
        socket
            .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
            .unwrap();
    }
    persistent_request(&mut socket);
    let mut wrong = proto::DesiredState::decode(signed(&trust, config).as_slice()).unwrap();
    wrong.pod_uid = "replacement-pod".into();
    persistent_reply(&mut socket, &wrong.encode_to_vec(), "\"wrong\"");
    let (mut fresh, request, _) = server.next();
    assert!(request.contains("If-None-Match: \"one\""));
    assert!(
        updates.status()["lastError"]
            .as_str()
            .unwrap()
            .contains("Pod identity")
    );
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    fresh.write_all(b"HTTP/1.1 204 No Content\r\n\r\n").unwrap();
    persistent_request(&mut fresh);
    assert!(updates.status()["lastError"].is_null());
    drop(subscriber);
}

#[test]
fn persistent_subscriber_reconnects_after_eof_and_truncated_body() {
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    let (mut socket, _, _) = server.next();
    persistent_reply(&mut socket, &signed(&trust, config), "\"one\"");
    // Close an idle connection without announcing Connection: close.
    drop(socket);
    let (mut socket, request, _) = server.next();
    assert!(request.contains("If-None-Match: \"one\""));
    socket
        .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 10\r\nETag: \"bad\"\r\n\r\nabc")
        .unwrap();
    drop(socket);
    let (mut socket, request, _) = server.next();
    assert!(request.contains("If-None-Match: \"one\""));
    assert_eq!(updates.latest(0).unwrap().config.revision, 1);
    assert!(updates.status()["lastError"].is_string());
    socket
        .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
        .unwrap();
    persistent_request(&mut socket);
    assert!(updates.status()["lastError"].is_null());
    drop(subscriber);
}

#[test]
fn persistent_subscriber_rotation_drains_before_next_trust_claim() {
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    let (mut socket, _, _) = server.next();
    persistent_reply(&mut socket, &signed(&trust, config), "\"one\"");
    persistent_request(&mut socket);
    credentials::tests::Fixture::advance(&server.provider);
    assert_eq!(server.provider.headers()[3].1, "1");
    // Credential changes cancel the held request without waiting for a response.
    let (mut socket, request, _) = server.next();
    assert!(request.contains("X-Racer-Trust-Generation: 2\r\n"));
    assert!(request.contains("X-Racer-Old-Connections: 0\r\n"));
    assert!(updates.status()["lastError"].is_null());
    socket
        .write_all(b"HTTP/1.1 204 No Content\r\n\r\n")
        .unwrap();
    persistent_request(&mut socket);
    drop(subscriber);
}

#[test]
fn persistent_subscriber_rejects_extra_bytes_and_times_out_reused_socket() {
    let server = Server::new();
    let (subscriber, updates, trust, config) = server.start();
    let (mut socket, _, _) = server.next();
    persistent_reply(&mut socket, &signed(&trust, config), "\"one\"");
    persistent_request(&mut socket);
    // Both frames are in one TLS record. The unsolicited second response must
    // never be accepted as the reply to a future heartbeat.
    socket
        .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\nHTTP/1.1 204 No Content\r\n\r\n")
        .unwrap();
    let (mut socket, request, _) = server.next();
    assert!(request.contains("If-None-Match: \"one\""));
    assert!(
        updates.status()["lastError"]
            .as_str()
            .unwrap()
            .contains("unsolicited")
    );
    socket
        .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
        .unwrap();
    persistent_request(&mut socket);
    let start = Instant::now();
    // A reused connection gets a fresh first-byte deadline, not its creation time.
    let (mut retry, request, _) = server
        .requests
        .recv_timeout(Duration::from_secs(38))
        .unwrap();
    assert!(start.elapsed() >= Duration::from_secs(35));
    assert!(start.elapsed() < Duration::from_secs(38));
    assert!(request.contains("If-None-Match: \"one\""));
    assert!(
        updates.status()["lastError"]
            .as_str()
            .unwrap()
            .contains("deadline")
    );
    retry
        .write_all(b"HTTP/1.1 304 Not Modified\r\n\r\n")
        .unwrap();
    persistent_request(&mut retry);
    assert!(updates.status()["lastError"].is_null());
    drop(subscriber);
}

#[test]
fn control_response_framing_contract() {
    for wire in [
        "HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nab",
        "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n",
        "HTTP/1.1 200 OK\r\nContent-Length: +0\r\n\r\n",
        "HTTP/1.1 200 OK\r\nContent-Length: 65\r\n\r\n",
        "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n",
        "HTTP/1.1 200 OK\r\n\r\n",
        "HTTP/1.1 204 No Content\r\nContent-Length: 1\r\n\r\n",
        "HTTP/2 200 OK\r\nContent-Length: 0\r\n\r\n",
        "HTTP/1.1 200 OK\r\nBad Header: x\r\nContent-Length: 0\r\n\r\n",
        "HTTP/1.1 200 OK\r\nETag: a\r\nETag: b\r\nContent-Length: 0\r\n\r\n",
        "HTTP/1.1 200 OK\r\nContent-Length:",
    ] {
        assert!(
            read_framed_response(&mut wire.as_bytes(), Some("\"one\""), 64).is_err(),
            "{wire}"
        );
    }
    for (wire, reusable) in [
        ("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc", true),
        ("HTTP/1.1 204 No Content\r\n\r\n", true),
        (
            "HTTP/1.1 304 Not Modified\r\nContent-Length: 42\r\n\r\n",
            true,
        ),
        ("HTTP/1.1 304 Not Modified\r\n\r\n", true),
        (
            "HTTP/1.1 200 OK\r\nConnection: keep-alive, ClOsE\r\nContent-Length: 0\r\n\r\n",
            false,
        ),
        (
            "HTTP/1.0 200 OK\r\nConnection: keep-alive\r\nContent-Length: 0\r\n\r\n",
            false,
        ),
    ] {
        assert_eq!(
            read_framed_response(&mut wire.as_bytes(), Some("\"one\""), 64)
                .unwrap()
                .reusable,
            reusable,
            "{wire}"
        );
    }
}

#[test]
fn control_transport_amortizes_1500_requests_over_one_handshake() {
    let fixture = credentials::tests::Fixture::new();
    let provider = fixture.provider(0);
    let context = fixture.context("spiffe://racer/controlplane", Some("localhost"));
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let server = std::thread::spawn(move || {
        let (socket, _) = listener.accept().unwrap();
        socket.set_nodelay(true).unwrap();
        let mut socket = credentials::tests::server(socket, &context);
        for _ in 0..1500 {
            persistent_request(&mut socket);
            socket
                .write_all(b"HTTP/1.1 204 No Content\r\n\r\n")
                .unwrap();
        }
    });
    let mut transport = ControlTransport {
        address,
        host: "localhost".into(),
        target: "/".into(),
        provider,
        idle: None,
        lifetime: Duration::from_secs(240),
    };
    for _ in 0..1500 {
        assert_eq!(
            transport.fetch(None, &[], &mut || Ok(())).unwrap(),
            (None, None)
        );
        assert!(transport.idle.is_some());
    }
    server.join().unwrap();
}
