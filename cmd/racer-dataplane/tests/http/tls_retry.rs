// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::tls::{ExpectedPeer, PeerIdentity, TlsProgress, TlsSession, tests::Authority};

fn identity(node: &str) -> PeerIdentity {
    PeerIdentity::new(&"01".repeat(32), &node.repeat(32), "retry-pod").unwrap()
}
fn complete<T>(mut operation: impl FnMut() -> io::Result<TlsProgress<T>>) -> T {
    let end = Instant::now() + Duration::from_secs(4);
    loop {
        match operation().unwrap() {
            TlsProgress::Complete(value) => return value,
            TlsProgress::Eof => panic!("unexpected close_notify"),
            _ => assert!(Instant::now() < end, "TLS fixture stalled"),
        }
        thread::sleep(Duration::from_millis(1));
    }
}
fn accept(listener: &TcpListener, context: &crate::tls::TlsContext) -> TlsSession {
    let socket = listener.accept().unwrap().0;
    socket.set_nonblocking(true).unwrap();
    let mut session = TlsSession::server(
        context,
        socket.into(),
        ExpectedPeer::Identity(identity("02")),
    )
    .unwrap();
    complete(|| session.handshake());
    session
}
fn request(session: &mut TlsSession) -> Vec<u8> {
    let mut bytes = Vec::new();
    while !bytes.ends_with(b"\r\n\r\n") {
        let mut byte = [0];
        assert_eq!(complete(|| session.read(&mut byte)), 1);
        bytes.push(byte[0]);
        assert!(bytes.len() <= SCRATCH_SIZE + 65536 + 17);
    }
    bytes
}
fn send(session: &mut TlsSession, mut bytes: &[u8]) {
    while !bytes.is_empty() {
        let n = complete(|| session.write(bytes));
        bytes = &bytes[n..];
    }
}
fn poll<T>(
    ring: &mut Ring,
    mut turn: impl FnMut(&mut Ring) -> io::Result<Progress<T>>,
) -> io::Result<T> {
    let end = Instant::now() + Duration::from_secs(5);
    loop {
        ring.progress()?;
        match turn(ring)? {
            Progress::Ready(value) => return Ok(value),
            Progress::Pending(_) => assert!(Instant::now() < end, "HTTP fixture stalled"),
        }
        thread::sleep(Duration::from_millis(1));
    }
}

#[test]
fn cached_peer_tls_abrupt_close_kernel() {
    crate::conformance::kernel_child(
        "http_client::tests::tls_retry::cached_peer_tls_abrupt_close_child",
        "RACER_TLS_RETRY_CHILD",
    );
}

#[test]
#[ignore = "bounded real socket/io_uring subprocess"]
fn cached_peer_tls_abrupt_close_child() {
    if std::env::var_os("RACER_TLS_RETRY_CHILD").is_none() {
        return;
    }
    let Some(mut ring) = crate::conformance::kernel_ring(4, Config::default()) else {
        return;
    };
    // Each cut is tested on small metadata and writable payload responses. A
    // partial HTTP byte (including informational headers) forbids all replay.
    for ktls in [false, true] {
        for payload in [false, true] {
            for cut in [
                "idle-fin",
                "idle-rst",
                "refused",
                "first-close",
                "partial",
                "informational",
                "body",
                "bad-record",
                "wrong-pod",
            ] {
                // Raw send on TX kTLS encrypts application bytes, so corruption
                // injection is meaningful only on the forced software record path.
                if ktls && cut == "bad-record" {
                    continue;
                }
                let ca = Authority::new();
                let client_context = ca.context(&identity("02"), ktls);
                let server_context = ca.context(&identity("03"), ktls);
                let bad_context = ca.context(
                    &PeerIdentity::new(&"01".repeat(32), &"03".repeat(32), "wrong-pod").unwrap(),
                    ktls,
                );
                let listener = TcpListener::bind("127.0.0.1:0").unwrap();
                let address = listener.local_addr().unwrap();
                let (closed_tx, closed_rx) = std::sync::mpsc::channel();
                let (stop_tx, stop_rx) = std::sync::mpsc::channel();
                let server = thread::spawn(move || {
                    let mut session = accept(&listener, &server_context);
                    assert!(request(&mut session).starts_with(b"GET /warm "));
                    send(
                        &mut session,
                        b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc",
                    );
                    if matches!(cut, "partial" | "informational" | "body" | "bad-record") {
                        assert!(request(&mut session).starts_with(b"GET /exact?x=%2f "));
                        if cut == "bad-record" {
                            let invalid = b"not a TLS record";
                            // SAFETY: live socket and readable bytes; deliberately
                            // bypass TLS to prove protocol failures never replay.
                            assert_eq!(
                                unsafe {
                                    libc::send(
                                        session.as_raw_fd(),
                                        invalid.as_ptr().cast(),
                                        invalid.len(),
                                        libc::MSG_NOSIGNAL,
                                    )
                                },
                                invalid.len() as isize
                            );
                        } else {
                            send(
                                &mut session,
                                match cut {
                                    "partial" => b"H",
                                    "informational" => b"HTTP/1.1 103 Early Hints\r\n\r\n",
                                    _ => b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\na",
                                },
                            );
                        }
                    }
                    if cut == "idle-rst" {
                        let linger = libc::linger {
                            l_onoff: 1,
                            l_linger: 0,
                        };
                        // SAFETY: owned live socket and valid linger option.
                        assert_eq!(
                            unsafe {
                                libc::setsockopt(
                                    session.as_raw_fd(),
                                    libc::SOL_SOCKET,
                                    libc::SO_LINGER,
                                    (&linger as *const libc::linger).cast(),
                                    size_of_val(&linger) as _,
                                )
                            },
                            0
                        );
                    }
                    drop(session); // No TLS close_notify: genuine transport loss.
                    if cut == "refused" {
                        drop(listener);
                        closed_tx.send(()).unwrap();
                        return;
                    }
                    closed_tx.send(()).unwrap();
                    if matches!(cut, "partial" | "informational" | "body" | "bad-record") {
                        stop_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                        listener.set_nonblocking(true).unwrap();
                        assert_eq!(
                            listener.accept().unwrap_err().kind(),
                            io::ErrorKind::WouldBlock,
                            "partial response was replayed"
                        );
                        return;
                    }
                    let mut session = accept(
                        &listener,
                        if cut == "wrong-pod" {
                            &bad_context
                        } else {
                            &server_context
                        },
                    );
                    if cut != "wrong-pod" {
                        let actual = request(&mut session);
                        assert_eq!(actual, format!("GET /exact?x=%2f HTTP/1.1\r\nHost: {address}\r\nRacer-Origin-Data: {}\r\nX-Racer-Volume: v1\r\n\r\n", openssl::base64::encode_block(&vec![b'x'; 65536])).as_bytes());
                        if cut != "first-close" {
                            send(
                                &mut session,
                                b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc",
                            );
                        }
                    }
                    drop(session);
                    stop_rx.recv_timeout(Duration::from_secs(5)).unwrap();
                    listener.set_nonblocking(true).unwrap();
                    assert_eq!(
                        listener.accept().unwrap_err().kind(),
                        io::ErrorKind::WouldBlock,
                        "more than one retry"
                    );
                });
                let provider =
                    crate::control::credentials::Provider::for_test(identity("02"), client_context);
                let mut origin = Origin::peer(Endpoint::parse(&address.to_string()).unwrap());
                origin.set_tls(provider, identity("03"));
                let (connection, permit) = origin.connection().unwrap();
                let mut warm = connection
                    .get_small(
                        Request::new("/warm", &[]).unwrap(),
                        3,
                        Instant::now() + Duration::from_secs(4),
                    )
                    .unwrap();
                let response = poll(&mut ring, |r| warm.poll(r, 16)).unwrap();
                assert_eq!(response.body(), b"abc");
                origin.recycle(response.recycle());
                permit.success();
                if !matches!(cut, "partial" | "informational" | "body" | "bad-record") {
                    closed_rx.recv_timeout(Duration::from_secs(4)).unwrap();
                }
                let (connection, permit) = origin.connection().unwrap();
                let deadline = Instant::now() + Duration::from_secs(3);
                let auth = crate::origin_data::OriginData::new(&vec![b'x'; 65536]).unwrap();
                let request = Request::new("/exact?x=%2f", &[("X-Racer-Volume", "v1")])
                    .unwrap()
                    .with_origin_data(&auth);
                let metrics = crate::metrics::Local::default();
                let result = if payload {
                    let fill = ring.pool().private_fill().unwrap();
                    let mut exchange = connection
                        .get(request, fill, deadline)
                        .unwrap()
                        .retry_idle_peer(&origin, &metrics);
                    let result = poll(&mut ring, |r| exchange.poll(r, 16))
                        .map(|mut response| response.body().to_vec());
                    assert_eq!(exchange.0.deadline, deadline);
                    result
                } else {
                    let mut exchange = connection
                        .get_small(request, 3, deadline)
                        .unwrap()
                        .retry_idle_peer(&origin, &metrics);
                    let result = poll(&mut ring, |r| exchange.poll(r, 16))
                        .map(|response| response.body().to_vec());
                    assert_eq!(exchange.0.deadline, deadline);
                    result
                };
                if matches!(cut, "idle-fin" | "idle-rst") {
                    assert_eq!(result.unwrap(), b"abc");
                    permit.success();
                    assert!(origin.breaker.try_acquire().is_ok());
                } else {
                    let error = result.unwrap_err();
                    let failure = error
                        .get_ref()
                        .unwrap()
                        .downcast_ref::<crate::outcome::Failure>()
                        .unwrap();
                    if cut == "refused" {
                        assert!(failure.owner_evidence());
                    }
                    if matches!(cut, "wrong-pod" | "bad-record") {
                        assert!(
                            !failure.owner_evidence(),
                            "cut={cut} ktls={ktls}: {failure:?}"
                        );
                    }
                    drop(permit);
                }
                let _ = stop_tx.send(());
                server.join().unwrap();
            }
        }
    }
    ring.shutdown().unwrap();
}
