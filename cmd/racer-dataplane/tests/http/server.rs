// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod tests {
    use super::*;
    use crate::buffers::{BUFFER_SIZE, Key};
    use crate::http_client;
    use std::io::Read as _;
    use std::net::TcpStream;
    use std::thread;

    #[test]
    fn tcp_identity_is_stable_distinct_and_observes_close() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let client_a = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (a, _) = listener.accept().unwrap();
        let client_b = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (b, _) = listener.accept().unwrap();
        let a = ConnectionId(Rc::new(Control {
            unix: false,
            file: File::new(a.into()),
            closed: Cell::new(false),
        }));
        let b = ConnectionId(Rc::new(Control {
            unix: false,
            file: File::new(b.into()),
            closed: Cell::new(false),
        }));
        let retained = a.clone();
        let mut pending = std::collections::HashMap::new();
        pending.insert(a.clone(), "Hello");
        assert!(pending.remove(&b).is_none());
        assert_eq!(pending.remove(&retained), Some("Hello"));
        assert!(!retained.is_closed());
        a.0.close();
        assert!(retained.is_closed());
        assert!(!b.is_closed());
        drop((client_a, client_b));
    }

    fn parse_request(bytes: &[u8]) -> Result<Metadata, u16> {
        parse(bytes, header_end(bytes, &mut 0).ok_or(400u16)?)
    }

    #[test]
    fn fragmented_requests_and_exact_target() {
        let bytes = b"GET //any/%2f%FF?x=1&x=2 HTTP/1.1\r\nHost: example:80\r\nRange: bytes=1-2\r\nX: \xff\r\nX: two\r\nConnection: keep-alive, CLOSE\r\n\r\nHEAD /next HTTP/1.1\r\nHost: h\r\n\r\n";
        let end = header_end(bytes, &mut 0).unwrap();
        for chunk in 1..=bytes.len() {
            let mut scan = 0;
            for used in (chunk..bytes.len()).step_by(chunk).chain([bytes.len()]) {
                if let Some(found) = header_end(&bytes[..used], &mut scan) {
                    assert_eq!(found, end);
                    let m = parse(bytes, found).unwrap();
                    assert_eq!(m.target.slice(bytes), b"//any/%2f%FF?x=1&x=2");
                    assert!(m.close);
                    assert!(!m.head);
                    let headers = Headers {
                        bytes,
                        headers: &m.headers[..m.count],
                    };
                    assert_eq!(headers.get("x"), Some(b"\xff".as_slice()));
                    assert_eq!(headers.iter().filter(|(n, _)| *n == "X").count(), 2);
                    assert!(matches!(
                        resolve_range(headers, 5),
                        RangeSelection::Partial(_)
                    ));
                    break;
                }
            }
        }
        assert!(parse_request(&bytes[end..]).unwrap().head);
    }

    #[test]
    fn absolute_targets_preserve_object_identity() {
        for method in ["GET", "HEAD"] {
            for authority in ["http://h", "https://h:443", "HTTP://[::1]:80"] {
                for path in ["/", "/models/model.safetensors", "//any/%2f%FF?x=1&x=2"] {
                    let bytes = format!("{method} {authority}{path} HTTP/1.1\r\nHost: h\r\n\r\n");
                    let m = parse_request(bytes.as_bytes()).unwrap();
                    assert_eq!(m.target.slice(bytes.as_bytes()), path.as_bytes());
                    assert_eq!(m.head, method == "HEAD");
                }
            }
        }
    }

    #[test]
    fn reject_ambiguous_or_unsupported_requests() {
        for bytes in [
            &b"GET / HTTP/1.0\r\nHost: h\r\n\r\n"[..],
            b"GET / HTTP/1.1\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nHost: h\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: \r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h/path\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost : h\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\n Folded: x\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nX: a\0b\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: 1\r\n\r\nx",
            b"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: +0\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: 0, 0\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nContent-Length: 18446744073709551616\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nConnection: x\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nConnection: close,\r\n\r\n",
            b"GET / HTTP/1.1\r\nHost: h\r\nUpgrade: rdma\r\n\r\n",
            b"GET  / HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET / HTTP/1.1 extra\r\nHost: h\r\n\r\n",
            b"GET /#fragment HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET http:///path HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET http://user@h/path HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET http://h?x/path HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET http://h/#fragment HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET ftp://h/path HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET http://h HTTP/1.1\r\nHost: h\r\n\r\n",
            b"GET /\xff HTTP/1.1\r\nHost: h\r\n\r\n",
        ] {
            assert_eq!(parse_request(bytes).err(), Some(400), "{bytes:?}");
        }
        assert_eq!(
            parse_request(b"POST / HTTP/1.1\r\nHost: h\r\n\r\n").err(),
            Some(405)
        );
        assert_eq!(
            parse_request(b"GET / HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\n\r\n").err(),
            Some(417)
        );
        let bytes = format!(
            "GET / HTTP/1.1\r\nHost: h\r\n{}\r\n",
            "X: a\r\n".repeat(MAX_HEADERS)
        );
        assert_eq!(parse_request(bytes.as_bytes()).err(), Some(431));
        assert!(
            parse_request(b"HEAD / HTTP/1.1\r\nHost: [::1]:80\r\nContent-Length: 0\r\n\r\n")
                .is_ok()
        );
    }

    #[test]
    fn response_framing_and_bounds() {
        for (status, length, valid) in [
            (199, None, false),
            (600, Some(0), false),
            (200, None, false),
            (200, Some(u64::MAX), true),
            (204, None, true),
            (204, Some(0), false),
            (205, Some(0), true),
            (205, Some(1), false),
            (205, None, false),
            (304, None, true),
            (304, Some(u64::MAX), true),
            (404, Some(3), true),
        ] {
            assert_eq!(ResponseHead::new(status, length, &[]).is_ok(), valid);
        }
        for (name, v) in [
            ("Content-Length", b"0".as_slice()),
            ("Transfer-Encoding", b"chunked"),
            ("Connection", b"close"),
            ("Upgrade", b"rdma"),
            ("Bad Name", b"x"),
            ("X", b"x\r\nX: y"),
        ] {
            assert!(ResponseHead::new(200, Some(0), &[(name, v)]).is_err());
        }
        let mut bytes = [0; SCRATCH_SIZE];
        let h = ResponseHead::new(206, Some(2), &[("ETag", b"\"tag\""), ("X", b"\xff")])
            .unwrap()
            .close();
        let n = h.encode(&mut bytes).unwrap();
        assert_eq!(&bytes[..n], b"HTTP/1.1 206 \r\nContent-Length: 2\r\nConnection: close\r\nETag: \"tag\"\r\nX: \xff\r\n\r\n");
        let overhead = ResponseHead::new(200, Some(0), &[("X", b"")])
            .unwrap()
            .close()
            .encode(&mut bytes)
            .unwrap();
        let v = vec![b'x'; SCRATCH_SIZE - overhead];
        assert_eq!(
            ResponseHead::new(200, Some(0), &[("X", &v)])
                .unwrap()
                .close()
                .encode(&mut bytes)
                .unwrap(),
            SCRATCH_SIZE
        );
        let v = vec![b'x'; SCRATCH_SIZE - overhead + 1];
        assert!(ResponseHead::new(200, Some(0), &[("X", &v)]).is_err());
    }

    #[test]
    fn range_edges() {
        use RangeSelection::*;
        for (spec, total, expected) in [
            (
                "bytes=0-0",
                1,
                Partial(ResolvedRange {
                    start: 0,
                    end: 1,
                    total: 1,
                }),
            ),
            (
                "bytes=2-99",
                8,
                Partial(ResolvedRange {
                    start: 2,
                    end: 8,
                    total: 8,
                }),
            ),
            (
                "bytes=2-",
                8,
                Partial(ResolvedRange {
                    start: 2,
                    end: 8,
                    total: 8,
                }),
            ),
            (
                "bytes=-3",
                8,
                Partial(ResolvedRange {
                    start: 5,
                    end: 8,
                    total: 8,
                }),
            ),
            (
                "bytes=-99",
                8,
                Partial(ResolvedRange {
                    start: 0,
                    end: 8,
                    total: 8,
                }),
            ),
            (
                "BYTES=0-18446744073709551615",
                u64::MAX,
                Partial(ResolvedRange {
                    start: 0,
                    end: u64::MAX,
                    total: u64::MAX,
                }),
            ),
            ("bytes=8-", 8, Unsatisfiable),
            ("bytes=0-", 0, Unsatisfiable),
            ("bytes=-0", 8, Unsatisfiable),
            ("bytes=-1", 0, Unsatisfiable),
            ("bytes=4-2", 8, Full),
            ("bytes=0-1,3-4", 8, Full),
            ("bytes= 0-1", 8, Full),
            ("bytes=+1-2", 8, Full),
            ("items=0-1", 8, Full),
            ("bytes=-", 8, Full),
            ("bytes=0--1", 8, Full),
            ("bytes=18446744073709551616-", 8, Full),
        ] {
            let found = range(spec.as_bytes(), total);
            assert_eq!(found, expected, "{spec}");
            if let Partial(r) = found {
                assert!(r.start() < r.end() && r.end() <= total);
                assert_eq!(r.len(), r.end() - r.start());
                assert_eq!(
                    r.content_range().to_string(),
                    format!("bytes {}-{}/{total}", r.start(), r.end() - 1)
                );
            }
        }
        let bytes = b"GET / HTTP/1.1\r\nHost: h\r\nRange: bytes=0-1\r\nRange: bytes=3-4\r\n\r\n";
        let m = parse_request(bytes).unwrap();
        assert_eq!(
            resolve_range(
                Headers {
                    bytes,
                    headers: &m.headers[..m.count]
                },
                8
            ),
            Full
        );
    }

    fn deadline() -> Instant {
        Instant::now() + Duration::from_secs(5)
    }
    fn buffer(ring: &Ring, key: u8, len: usize) -> Buffer {
        let mut fill = ring.pool().stage(Key::new([key; 32])).unwrap();
        fill.as_mut_slice()[..len].fill(key);
        fill.publish(len).unwrap()
    }
    fn drive<T>(
        ring: &mut Ring,
        mut poll: impl FnMut(&mut Ring) -> io::Result<Progress<T>>,
    ) -> io::Result<T> {
        let end = deadline();
        loop {
            assert!(Instant::now() < end, "server test stalled");
            ring.progress()?;
            match poll(ring)? {
                Progress::Ready(t) => return Ok(t),
                Progress::Pending(w) if !w.runnable => ring.wait(w.deadline.or(Some(end)))?,
                _ => {}
            }
        }
    }
    fn until(ring: &mut Ring, mut ready: impl FnMut(&mut Ring) -> bool) {
        let end = deadline();
        while !ready(ring) {
            assert!(Instant::now() < end, "server lifecycle stalled");
            ring.progress().unwrap();
            thread::yield_now();
        }
    }
    fn drained(ring: &mut Ring) {
        until(ring, |r| r.http_test_idle());
        let held: Vec<_> = (220..224).map(|k| buffer(ring, k, 1)).collect();
        assert!(ring.pool().stage(Key::new([224; 32])).is_err());
        drop(held);
    }
    fn listener() -> Listener {
        Listener::bind("127.0.0.1:0".parse().unwrap(), NonZeroU32::new(16).unwrap()).unwrap()
    }
    fn connected(ring: &mut Ring) -> (Connection, TcpStream) {
        let mut l = listener();
        let peer = TcpStream::connect(l.local_addr().unwrap()).unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
        peer.set_write_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let c = drive(ring, |r| l.poll_accept(r, 1)).unwrap();
        (c, peer)
    }
    fn request(ring: &mut Ring, c: Connection) -> Request {
        let mut receiving = c.receive(deadline());
        drive(ring, |r| receiving.poll(r, 1)).unwrap()
    }
    fn response_headers(peer: &mut TcpStream) -> Vec<u8> {
        let mut bytes = Vec::new();
        while !bytes.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            peer.read_exact(&mut byte).unwrap();
            bytes.push(byte[0]);
            assert!(bytes.len() <= SCRATCH_SIZE);
        }
        bytes
    }
    fn body_writer(ring: &mut Ring, request: Request, len: u64) -> BodyWriter {
        let Request::Get(r) = request else {
            panic!("expected GET")
        };
        let mut send = r
            .respond(ResponseHead::new(200, Some(len), &[]).unwrap())
            .unwrap();
        let BodyProgress::More(w) = drive(ring, |r| send.poll(r, 1)).unwrap() else {
            panic!("expected writer")
        };
        w
    }
    fn send_chunk(w: BodyWriter, chunk: BodyChunk) -> SendingBody {
        match w.send(chunk) {
            Ok(s) => s,
            Err(e) => panic!("send: {}", e.error),
        }
    }

    #[test]
    fn body_diagnostic_distinguishes_body_admission_and_socket_send() {
        let Some(mut ring) = crate::conformance::kernel_ring(2, Default::default()) else {
            return;
        };
        let (connection, mut peer) = connected(&mut ring);
        peer.write_all(b"GET /redacted HTTP/1.1\r\nHost: cache\r\n\r\n")
            .unwrap();
        let request = request(&mut ring, connection);
        let writer = body_writer(&mut ring, request, 3);
        response_headers(&mut peer);
        let chunk = BodyChunk::new(buffer(&ring, 1, 3), 0..3).unwrap();
        let mut body = writer.send(chunk).unwrap();
        body.set_diagnostic(serde_json::json!({"key":"c".repeat(64)}));
        body.observe_diagnostic(&ring);
        let record = body.diagnostic.as_ref().unwrap().record("test", None);
        assert_eq!(record["stage"], "body_admission");
        assert!(record["io"].is_null());
        body.small = Some(
            ring.send_bytes(
                body.response
                    .as_ref()
                    .unwrap()
                    .connection
                    .control
                    .file
                    .clone()
                    .into(),
                vec![1; 1].into_boxed_slice(),
            )
            .unwrap(),
        );
        drive(&mut ring, |r| {
            Ok(
                if r.diagnostic(body.small.as_ref().unwrap()).unwrap().state == "complete" {
                    Progress::Ready(())
                } else {
                    pending(true, None)
                },
            )
        })
        .unwrap();
        body.observe_diagnostic(&ring);
        assert_eq!(body.diagnostic.as_ref().unwrap().stage, "socket_send");
        assert_eq!(
            body.diagnostic.as_ref().unwrap().io.as_ref().unwrap().state,
            "complete"
        );
        body.response.as_ref().unwrap().deadline.cap(Instant::now());
        assert_eq!(
            body.poll(&mut ring, 1).err().unwrap().kind(),
            io::ErrorKind::TimedOut
        );
        assert!(body.diagnostic.as_ref().unwrap().emitted);
        drop(body);
        ring.shutdown().unwrap();
        ring.pool().assert_recovered();
    }

    #[test]
    fn kernel_integration() {
        cache_responses::kernel_child(
            "http_server::tests::kernel_child",
            "RACER_HTTP_SERVER_CHILD",
        );
    }
    #[test]
    #[ignore = "run via bounded kernel_integration subprocess"]
    fn kernel_child() {
        if std::env::var_os("RACER_HTTP_SERVER_CHILD").is_none() {
            return;
        }
        let config = crate::uring::Config {
            progress_reserve: 0,
            entries: 8,
            requests: 16,
            fixed_files: 4,
            completion_budget: 2,
            shutdown_timeout: Duration::from_millis(500),
        };
        let Some(mut ring) = crate::conformance::kernel_ring(4, config) else {
            return;
        };
        pipelining(&mut ring);
        stream_pages(&mut ring);
        rejection(&mut ring);
        short_and_pressure(&mut ring);
        cancellation(&mut ring);
        accept_abandonment(&mut ring);
        server_and_client(&mut ring);
        unix_server_and_client(&mut ring);
        handler_deadlines(&mut ring);
        ring.shutdown().unwrap();
    }

    fn pipelining(ring: &mut Ring) {
        let before = ring.metrics().values()[3];
        let (mut c, mut peer) = connected(ring);
        let input = c.input.as_ref().unwrap().as_ptr();
        let output = c.output.as_ref().unwrap().as_ptr();
        // Inject completed read-ahead to make coalescing deterministic, rather
        // than assuming TCP writes imply receive boundaries.
        let bytes = b"HEAD /first?x=%2f HTTP/1.1\r\nHost: h\r\n\r\nGET /next HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n";
        c.input.as_mut().unwrap()[..bytes.len()].copy_from_slice(bytes);
        c.used = bytes.len();
        let Request::Head(mut r) = request(ring, c) else {
            panic!()
        };
        r.0.metric_traffic = Some(crate::metrics::Traffic::ClientHttp);
        assert_eq!(r.target(), "/first?x=%2f");
        let mut send = r
            .respond(ResponseHead::new(200, Some(u64::MAX), &[("ETag", b"\"v1\"")]).unwrap())
            .unwrap();
        let c = drive(ring, |r| send.poll(r, 1)).unwrap().recycle().unwrap();
        assert_eq!(c.input.as_ref().unwrap().as_ptr(), input);
        assert_eq!(c.output.as_ref().unwrap().as_ptr(), output);
        assert!(response_headers(&mut peer).ends_with(b"ETag: \"v1\"\r\n\r\n"));
        let Request::Get(r) = request(ring, c) else {
            panic!()
        };
        assert_eq!(r.target(), "/next");
        let mut send = r
            .respond(ResponseHead::new(204, None, &[]).unwrap())
            .unwrap();
        let BodyProgress::Done(c) = drive(ring, |r| send.poll(r, 1)).unwrap() else {
            panic!()
        };
        assert!(c.recycle().is_none());
        assert_eq!(
            response_headers(&mut peer),
            b"HTTP/1.1 204 \r\nConnection: close\r\n\r\n"
        );
        assert_eq!(peer.read(&mut [0]).unwrap(), 0);
        assert_eq!(ring.metrics().values()[3], before, "HEAD has no payload");
        drained(ring);
    }

    fn stream_pages(ring: &mut Ring) {
        let (c, mut peer) = connected(ring);
        peer.write_all(b"GET /large HTTP/1.1\r\nHost: h\r\n\r\n")
            .unwrap();
        let req = request(ring, c);
        let total = BUFFER_SIZE * 2 + 11;
        let peer = thread::spawn(move || {
            assert!(
                String::from_utf8(response_headers(&mut peer))
                    .unwrap()
                    .contains(&format!("Content-Length: {total}\r\n"))
            );
            let mut body = vec![0; total];
            peer.read_exact(&mut body).unwrap();
            assert!(body[..BUFFER_SIZE - 2].iter().all(|&b| b == 10));
            assert!(
                body[BUFFER_SIZE - 2..BUFFER_SIZE * 2 - 2]
                    .iter()
                    .all(|&b| b == 11)
            );
            assert!(body[BUFFER_SIZE * 2 - 2..].iter().all(|&b| b == 12));
        });
        let mut progress = BodyProgress::More(body_writer(ring, req, total as u64));
        for (key, range) in [(10, 2..BUFFER_SIZE), (11, 0..BUFFER_SIZE), (12, 5..18)] {
            let BodyProgress::More(w) = progress else {
                panic!()
            };
            let chunk = BodyChunk::new(buffer(ring, key, BUFFER_SIZE), range)
                .ok()
                .unwrap();
            let mut sending = send_chunk(w, chunk);
            progress = drive(ring, |r| sending.poll(r, 1)).unwrap();
        }
        let BodyProgress::Done(c) = progress else {
            panic!()
        };
        drop(c);
        peer.join().unwrap();
        drained(ring);
    }

    fn rejection(ring: &mut Ring) {
        for (bytes, status) in [
            (b"POST / HTTP/1.1\r\nHost: h\r\n\r\n".to_vec(), 405),
            (
                b"GET / HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\n\r\n".to_vec(),
                417,
            ),
            (
                b"GET / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n".to_vec(),
                400,
            ),
            (
                [
                    b"GET / HTTP/1.1\r\nHost: h\r\nX: ".as_slice(),
                    &vec![b'x'; SCRATCH_SIZE],
                ]
                .concat(),
                431,
            ),
        ] {
            let (c, mut peer) = connected(ring);
            peer.write_all(&bytes).unwrap();
            let mut recv = c.receive(deadline());
            assert!(drive(ring, |r| recv.poll(r, 1)).is_err());
            let head = String::from_utf8(response_headers(&mut peer)).unwrap();
            assert!(head.starts_with(&format!("HTTP/1.1 {status} \r\n")));
            assert!(head.contains("Connection: close\r\n"));
            if status == 405 {
                assert!(head.contains("Allow: GET, HEAD\r\n"));
            }
            drained(ring);
        }
    }

    struct Pressure {
        file: File,
        _peer: std::os::unix::net::UnixStream,
        tickets: Vec<Ticket<Bytes>>,
    }
    impl Pressure {
        fn new(ring: &mut Ring) -> Self {
            let (socket, peer) = std::os::unix::net::UnixStream::pair().unwrap();
            let file = File::new(socket.into());
            let mut tickets = Vec::new();
            loop {
                match ring.recv_bytes(file.clone().into(), vec![0; 1].into_boxed_slice()) {
                    Ok(t) => {
                        tickets.push(t);
                        ring.progress().unwrap();
                    }
                    Err(e) => {
                        assert_eq!(e.error.kind(), io::ErrorKind::WouldBlock);
                        break;
                    }
                }
            }
            Self {
                file,
                _peer: peer,
                tickets,
            }
        }
        fn release(self, ring: &mut Ring) {
            // SAFETY: owned socket; shutdown retires blocking receives.
            unsafe {
                libc::shutdown(self.file.as_fd().as_raw_fd(), libc::SHUT_RDWR);
            }
            drop(self.tickets);
            for _ in 0..32 {
                ring.progress().unwrap();
            }
        }
    }

    fn short_and_pressure(ring: &mut Ring) {
        let (c, mut peer) = connected(ring);
        let fixed: Vec<_> = (0..4)
            .map(|_| ring.register_file(c.control.file.clone()).unwrap())
            .collect();
        let mut receiving = c.receive(deadline());
        assert!(matches!(
            receiving.poll(ring, 2).unwrap(),
            Progress::Pending(Work {
                runnable: false,
                ..
            })
        ));
        assert!(receiving.connection.as_ref().unwrap().fixed.is_none());
        drop(fixed);
        ring.progress().unwrap();
        peer.write_all(b"GET / HTTP/1.1\r\nHost: h\r\n\r\n")
            .unwrap();
        let pressure = Pressure::new(ring);
        for _ in 0..2 {
            assert!(matches!(
                receiving.poll(ring, 8).unwrap(),
                Progress::Pending(Work {
                    runnable: false,
                    ..
                })
            ));
        }
        pressure.release(ring);
        let Request::Get(mut r) = drive(ring, |r| receiving.poll(r, 1)).unwrap() else {
            panic!()
        };
        let before = ring.metrics().values()[3];
        r.0.metric_traffic = Some(crate::metrics::Traffic::ClientHttp);
        let mut headers = r
            .respond(ResponseHead::new(200, Some(6), &[]).unwrap())
            .unwrap();
        // Force a one-byte header completion and then force queue rejection.
        let response = headers.0.response.as_mut().unwrap();
        let bytes = response.connection.output.take().unwrap();
        headers.0.ticket = Some(
            ring.send_bytes_range(
                response.connection.fixed.as_ref().unwrap().clone().into(),
                bytes,
                0..1,
            )
            .unwrap(),
        );
        until(ring, |r| {
            let _ = headers.poll(r, 1).unwrap();
            headers.0.sent == 1
        });
        let pressure = Pressure::new(ring);
        assert!(matches!(
            headers.poll(ring, 8).unwrap(),
            Progress::Pending(Work { runnable: true, .. })
        ));
        assert_eq!(headers.0.sent, 1);
        pressure.release(ring);
        let BodyProgress::More(w) = drive(ring, |r| headers.poll(r, 1)).unwrap() else {
            panic!()
        };
        assert_eq!(
            ring.metrics().values()[3],
            before,
            "headers are not payload"
        );
        let chunk = BodyChunk::new(buffer(ring, 30, 6), 0..6).ok().unwrap();
        let mut sending = send_chunk(w, chunk);
        let response = sending.response.as_ref().unwrap();
        let chunk = sending.chunk.as_ref().unwrap();
        sending.ticket = Some(
            ring.send_zc(
                response.connection.fixed.as_ref().unwrap().clone().into(),
                match &chunk.buffer {
                    crate::cache::CachedValue::Buffer(buffer) => buffer.clone(),
                    _ => unreachable!(),
                },
                BufferRange::new(0..2).unwrap(),
            )
            .unwrap(),
        );
        ring.measure_send(
            sending.ticket.as_ref().unwrap(),
            Some(crate::metrics::Traffic::ClientHttp),
        );
        until(ring, |r| {
            let _ = sending.poll(r, 1).unwrap();
            sending.chunk.as_ref().unwrap().range.start == 2
        });
        until(ring, |r| {
            let response = sending.response.as_mut().unwrap();
            response.reap(r).unwrap();
            response.is_drained()
        });
        assert_eq!(
            ring.metrics().values()[3],
            before + 2,
            "short send counted once, not on notification"
        );
        let pressure = Pressure::new(ring);
        assert!(matches!(
            sending.poll(ring, 8).unwrap(),
            Progress::Pending(Work { runnable: true, .. })
        ));
        assert_eq!(sending.chunk.as_ref().unwrap().range, 2..6);
        pressure.release(ring);
        let BodyProgress::Done(c) = drive(ring, |r| sending.poll(r, 1)).unwrap() else {
            panic!()
        };
        response_headers(&mut peer);
        let mut body = [0; 6];
        peer.read_exact(&mut body).unwrap();
        assert_eq!(body, [30; 6]);
        assert_eq!(ring.metrics().values()[3], before + 6);
        drop(c);
        drained(ring);
    }

    fn cancellation(ring: &mut Ring) {
        for mode in 0..3 {
            let before = ring.metrics().values()[3];
            let (c, mut peer) = connected(ring);
            peer.write_all(b"GET / HTTP/1.1\r\nHost: h\r\n\r\n")
                .unwrap();
            let mut req = request(ring, c);
            req.set_metric_traffic(crate::metrics::Traffic::ClientHttp);
            let w = body_writer(ring, req, BUFFER_SIZE as u64);
            let chunk = BodyChunk::new(buffer(ring, 40, BUFFER_SIZE), 0..BUFFER_SIZE)
                .ok()
                .unwrap();
            let mut sending = send_chunk(w, chunk);
            let held: Vec<_> = (41..44).map(|k| buffer(ring, k, 1)).collect();
            assert!(matches!(
                sending.poll(ring, 1).unwrap(),
                Progress::Pending(_)
            ));
            assert!(sending.ticket.is_some());
            // Complete the send in the ring without polling the HTTP task. A
            // following cancel/deadline must not lose its successful bytes.
            until(ring, |r| {
                r.send_result(sending.ticket.as_ref().unwrap())
                    .unwrap()
                    .is_some()
            });
            let sent = ring
                .send_result(sending.ticket.as_ref().unwrap())
                .unwrap()
                .unwrap()
                .unwrap();
            assert!(sent > 0);
            match mode {
                0 => sending.cancel(),
                1 => drop(sending),
                _ => {
                    sending
                        .response
                        .as_mut()
                        .unwrap()
                        .deadline
                        .cap(Instant::now());
                    assert!(
                        matches!(sending.poll(ring, 1), Err(e) if e.kind() == io::ErrorKind::TimedOut)
                    );
                }
            }
            assert!(ring.pool().stage(Key::new([44; 32])).is_err());
            drop(peer);
            until(ring, |r| r.http_test_idle());
            assert_eq!(ring.metrics().values()[3] - before, sent as u64);
            drop((held, buffer(ring, 44, 1)));
            drained(ring);
        }
        for mode in 0..2 {
            let (c, mut peer) = connected(ring);
            let mut receive = c.receive(deadline());
            let _ = receive.poll(ring, 2).unwrap();
            assert!(receive.ticket.is_some());
            let pressure = Pressure::new(ring);
            if mode == 0 {
                receive.cancel();
            } else {
                receive.deadline = Instant::now();
                assert!(receive.poll(ring, 1).is_err());
            }
            assert_eq!(peer.read(&mut [0]).unwrap(), 0);
            pressure.release(ring);
            drained(ring);
        }
    }

    fn accept_abandonment(ring: &mut Ring) {
        for submitted in [false, true] {
            let mut l = listener();
            let _ = l.poll_accept(ring, 1).unwrap();
            if submitted {
                ring.progress().unwrap();
            }
            let pressure = Pressure::new(ring);
            drop(l);
            pressure.release(ring);
            drained(ring);
        }
    }

    enum Task {
        Head(SendingHeadHeaders),
        Headers(SendingGetHeaders),
        Body(SendingBody),
        Done,
    }
    struct Echo {
        requests: usize,
    }
    impl Handler for Echo {
        type Task = Task;
        fn start(&mut self, r: Request) -> Task {
            self.requests += 1;
            assert_eq!(r.target(), "/object?version=1");
            match r {
                Request::Head(r) => Task::Head(
                    r.respond(
                        ResponseHead::new(200, Some(9999999), &[("ETag", b"\"tag\"")]).unwrap(),
                    )
                    .unwrap(),
                ),
                Request::Get(r) => Task::Headers(
                    r.respond(
                        ResponseHead::new(206, Some(3), &[("Content-Range", b"bytes 0-2/9999999")])
                            .unwrap(),
                    )
                    .unwrap(),
                ),
            }
        }
        fn poll(
            &mut self,
            task: &mut Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<Completed>> {
            let progress = match task {
                Task::Head(h) => return h.poll(ring, budget),
                Task::Headers(h) => h.poll(ring, budget)?,
                Task::Body(b) => b.poll(ring, budget)?,
                Task::Done => panic!(),
            };
            match progress {
                Progress::Pending(w) => Ok(Progress::Pending(w)),
                Progress::Ready(BodyProgress::More(w)) => {
                    *task = Task::Body(send_chunk(
                        w,
                        BodyChunk::new(buffer(ring, 50, 3), 0..3).ok().unwrap(),
                    ));
                    Ok(pending(true, None))
                }
                Progress::Ready(BodyProgress::Done(c)) => {
                    *task = Task::Done;
                    Ok(Progress::Ready(c))
                }
            }
        }
    }
    fn server_and_client(ring: &mut Ring) {
        let l = listener();
        let address = l.local_addr().unwrap();
        exercise_server_and_client(ring, l, address.into());
    }

    fn unix_server_and_client(ring: &mut Ring) {
        let directory = std::env::temp_dir().join(format!("uds-http-{}", std::process::id()));
        std::fs::create_dir(&directory).unwrap();
        let path =
            crate::socket::UnixPath::new(directory.join("client").to_str().unwrap()).unwrap();
        let mut other_worker = Listener::bind_unix(path).unwrap();
        let l = Listener::bind_unix(path).unwrap();
        // Cancel a ring-local accept without shutting down the shared endpoint.
        assert!(matches!(
            other_worker.poll_accept(ring, 1).unwrap(),
            Progress::Pending(_)
        ));
        ring.progress().unwrap();
        drop(other_worker);
        exercise_server_and_client(ring, l, crate::socket::Address::Unix(path));
        assert!(!std::path::Path::new(path.as_str()).exists());
        // Rebinding the pathname must be visible through the same parent directory.
        exercise_server_and_client(
            ring,
            Listener::bind_unix(path).unwrap(),
            crate::socket::Address::Unix(path),
        );
        std::fs::remove_dir_all(directory).unwrap();
    }

    fn exercise_server_and_client(ring: &mut Ring, l: Listener, address: crate::socket::Address) {
        let mut server = Server::new(
            l,
            Echo { requests: 0 },
            Config {
                max_connections: NonZeroUsize::new(2).unwrap(),
                ..Config::default()
            },
        );
        let client = http_client::Connection::new_address(address, "objects.test").unwrap();
        let mut head = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        let response = drive(ring, |r| {
            let work = server.poll(r, 1)?;
            Ok(match head.poll(r, 1)? {
                Progress::Pending(mut w) => {
                    merge(&mut w, work);
                    Progress::Pending(w)
                }
                p => p,
            })
        })
        .unwrap();
        assert_eq!(response.content_length(), Some(9999999));
        assert_eq!(response.headers().get("etag"), Some(b"\"tag\"".as_slice()));
        let client = response.recycle().unwrap();
        let fill = ring.pool().stage(Key::new([51; 32])).unwrap();
        let mut get = client
            .get(
                http_client::Request::new("/object?version=1", &[("Range", "bytes=0-2")]).unwrap(),
                fill,
                deadline(),
            )
            .unwrap();
        let mut response = drive(ring, |r| {
            let work = server.poll(r, 1)?;
            Ok(match get.poll(r, 1)? {
                Progress::Pending(mut w) => {
                    merge(&mut w, work);
                    Progress::Pending(w)
                }
                p => p,
            })
        })
        .unwrap();
        assert_eq!(response.status(), 206);
        assert_eq!(response.body(), [50; 3]);
        assert_eq!(server.handler().requests, 2);
        drop(response);
        server.shutdown(ring).unwrap();
        drained(ring);
    }

    struct Stalled {
        started: usize,
    }
    impl Handler for Stalled {
        type Task = Request;
        fn start(&mut self, r: Request) -> Request {
            self.started += 1;
            r
        }
        fn poll(
            &mut self,
            _: &mut Request,
            _: &mut Ring,
            _: usize,
        ) -> io::Result<Progress<Completed>> {
            Ok(pending(false, None))
        }
    }
    fn handler_deadlines(ring: &mut Ring) {
        let l = listener();
        let mut peers: Vec<_> = (0..3)
            .map(|_| {
                let mut p = TcpStream::connect(l.local_addr().unwrap()).unwrap();
                p.set_read_timeout(Some(Duration::from_secs(5))).unwrap();
                p.write_all(b"GET / HTTP/1.1\r\nHost: h\r\n\r\n").unwrap();
                p
            })
            .collect();
        let mut server = Server::new(
            l,
            Stalled { started: 0 },
            Config {
                max_connections: NonZeroUsize::new(2).unwrap(),
                ..Config::default()
            },
        );
        until(ring, |r| {
            server.poll(r, 1).unwrap();
            server.handler().started == 2
        });
        for _ in 0..50 {
            ring.progress().unwrap();
            server.poll(ring, 1).unwrap();
            assert_eq!(server.connections(), 2);
        }
        assert_eq!(server.handler().started, 2);
        // A wake can arrive for an already-polled task during a multi-turn
        // sweep. Consuming that wake must force another sweep before sleep.
        until(ring, |r| {
            let work = server.poll(r, 8).unwrap();
            !work.runnable && server.sweep_left == 0
        });
        assert!(server.poll(ring, 1).unwrap().runnable);
        assert!(server.poll(ring, 1).unwrap().runnable);
        assert_eq!(server.sweep_left, 1);
        let epoch = ring.completion_epoch();
        std::task::Wake::wake_by_ref(&ring.wake_handle());
        until(ring, |r| r.completion_epoch() != epoch);
        assert!(server.poll(ring, 1).unwrap().runnable);
        // Force one expiration; all deadlines remain enforced despite a handler
        // returning no deadline. This also opens admission for the queued peer.
        server
            .slots
            .front()
            .unwrap()
            .response_deadline
            .as_ref()
            .unwrap()
            .cap(Instant::now());
        until(ring, |r| {
            server.poll(r, 1).unwrap();
            server.handler().started == 3
        });
        server.shutdown(ring).unwrap();
        for p in &mut peers {
            assert_eq!(p.read(&mut [0]).unwrap(), 0);
        }
        drained(ring);
    }

    pub(crate) mod cache_responses {
        // libtest exits successfully for an unmatched --exact filter. Validate the
        // selector before spawning so a module move cannot silently skip TCP coverage.
        pub(crate) fn assert_child_selected(test: &str) {
            let output = std::process::Command::new(std::env::current_exe().unwrap())
                .args(["--exact", test, "--ignored", "--list"])
                .output()
                .unwrap();
            assert!(output.status.success());
            let listing = String::from_utf8(output.stdout).unwrap();
            assert_eq!(
                listing
                    .lines()
                    .filter(|line| line.ends_with(": test"))
                    .collect::<Vec<_>>(),
                [format!("{test}: test")],
                "child selector must select exactly one test: {listing}"
            );
        }

        pub(crate) fn kernel_child(test: &str, variable: &str) {
            assert_child_selected(test);
            crate::conformance::kernel_child(test, variable);
        }

        use crate::{
            buffers::BUFFER_SIZE,
            handlers::{Backend, Handler, Peer},
            http_server as http,
            uring::Ring,
        };
        use std::{
            io::{self, Read, Write},
            net::{TcpListener, TcpStream},
            num::NonZeroU32,
            sync::{
                Arc,
                atomic::{AtomicBool, AtomicU64, Ordering},
            },
            thread,
            time::{Duration, Instant},
        };

        fn cache(backend: &Backend, shards: usize) -> crate::cache::Cache {
            crate::cache::adapter_fixture::cache(backend.namespace(), shards)
        }
        pub(crate) fn accept(listener: &TcpListener) -> TcpStream {
            let (stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(5)))
                .unwrap();
            stream
        }
        pub(crate) fn request(stream: &mut TcpStream) -> String {
            let mut out = Vec::new();
            while !out.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                stream.read_exact(&mut byte).unwrap();
                out.push(byte[0]);
                assert!(out.len() < 16384);
            }
            String::from_utf8(out).unwrap()
        }
        fn drive_client(
            server: &mut http::Server<Handler>,
            ring: &mut Ring,
            client: thread::JoinHandle<()>,
            timeout: Duration,
        ) {
            let end = Instant::now() + timeout;
            while !client.is_finished() {
                assert!(
                    Instant::now() < end,
                    "HTTP stalled under two-buffer pressure"
                );
                ring.progress().unwrap();
                let mut work = server.handler_mut().poll_background(ring, 1).unwrap();
                work.merge(server.poll(ring, 1).unwrap());
                if !work.runnable {
                    ring.wait(Some(work.deadline.unwrap_or(end).min(end)))
                        .unwrap();
                }
            }
            client.join().unwrap();
        }

        pub(crate) fn http_streaming(ring: &mut Ring) {
            let before = ring.metrics().values();
            let backend = TcpListener::bind("127.0.0.1:0").unwrap();
            backend.set_nonblocking(true).unwrap();
            let url = backend.local_addr().unwrap().to_string();
            let stop = Arc::new(AtomicBool::new(false));
            let done = stop.clone();
            let heads = Arc::new(AtomicU64::new(0));
            let pages = Arc::new(AtomicU64::new(0));
            let (head_count, page_count) = (heads.clone(), pages.clone());
            let backend_thread = thread::spawn(move || {
                let mut connections = Vec::new();
                while !done.load(Ordering::Acquire) {
                    match backend.accept() {
                        Ok((mut stream, _)) => {
                            let (heads, pages) = (head_count.clone(), page_count.clone());
                            connections.push(thread::spawn(move || {
                            stream
                                .set_read_timeout(Some(Duration::from_secs(5)))
                                .unwrap();
                            let request = request(&mut stream);
                            assert!(request.contains(" //object%2f?x=1&x=2 HTTP/1.1\r\n"));
                            assert!(!request.contains("X-Racer-Target:"));
                            assert!(request.contains("Accept-Encoding: identity\r\n"));
                            let length = BUFFER_SIZE + 3;
                            let tag = crate::conformance::etag(&vec![b'x'; length]);
                            if request.starts_with("HEAD ") {
                                heads.fetch_add(1, Ordering::Relaxed);
                                write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: {length}\r\nETag: {tag}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n").unwrap();
                            } else {
                                pages.fetch_add(1, Ordering::Relaxed);
                                assert!(request.starts_with("GET "));
                                assert!(request.contains(&format!("If-Match: {tag}\r\n")));
                                assert!(request.contains("Accept-Encoding: identity\r\n"));
                                let range = request
                                    .lines()
                                    .find_map(|l| l.strip_prefix("Range: bytes="))
                                    .unwrap();
                                let (start, end) = range.split_once('-').unwrap();
                                let (start, end) = (
                                    start.parse::<usize>().unwrap(),
                                    end.parse::<usize>().unwrap(),
                                );
                                assert!(start.is_multiple_of(BUFFER_SIZE));
                                assert_eq!(end, (start + BUFFER_SIZE).min(length) - 1);
                                write!(stream, "HTTP/1.1 206 Partial Content\r\nContent-Length: {}\r\nContent-Range: bytes {start}-{end}/{length}\r\nETag: {tag}\r\nConnection: close\r\n\r\n", end - start + 1).unwrap();
                                stream.write_all(&vec![b'x'; end - start + 1]).unwrap();
                            }
                        }));
                        }
                        Err(error) if error.kind() == io::ErrorKind::WouldBlock => {
                            thread::sleep(Duration::from_millis(1))
                        }
                        Err(error) => panic!("accept: {error}"),
                    }
                }
                for connection in connections {
                    connection.join().unwrap();
                }
            });
            let backend = Backend::new(&url, "test-origin").unwrap();
            let mut cache = cache(&backend, 3);
            cache.set_metrics(ring.metrics().clone());
            let mut handler = Handler::new(cache, backend);
            let refused = TcpListener::bind("127.0.0.1:0").unwrap();
            let peer = refused.local_addr().unwrap().to_string();
            drop(refused);
            handler.test_authentication(2, &[3], Some(3));
            handler.set_peer(Peer::new(&peer, None).unwrap());
            let address = TcpListener::bind("127.0.0.1:0").unwrap();
            let port = address.local_addr().unwrap();
            drop(address);
            let listener = http::Listener::bind(port, NonZeroU32::new(32).unwrap()).unwrap();
            let mut server = http::Server::new(listener, handler, http::Config::default());
            let client = thread::spawn(move || {
                for (method, extra, status, size) in [
                    ("HEAD", "", 200, BUFFER_SIZE + 3),
                    ("GET", "", 200, BUFFER_SIZE + 3),
                    ("GET", "Range: bytes=67108863-67108865\r\n", 206, 3),
                    ("GET", "Range: bytes=99999999-\r\n", 416, 0),
                    (
                        "GET",
                        "Range: bytes=0-1\r\nIf-Range: \"old\"\r\n",
                        200,
                        BUFFER_SIZE + 3,
                    ),
                    ("GET", "Range: bytes=0-1\r\nIf-Range: \"v1\"\r\n", 206, 2),
                ] {
                    let mut stream = TcpStream::connect(port).unwrap();
                    stream
                        .set_read_timeout(Some(Duration::from_secs(10)))
                        .unwrap();
                    let tag = crate::conformance::etag(&vec![b'x'; BUFFER_SIZE + 3]);
                    let extra = extra.replace("\"v1\"", &tag);
                    write!(stream, "{method} //object%2f?x=1&x=2 HTTP/1.1\r\nHost: cache\r\n{extra}Connection: close\r\n\r\n").unwrap();
                    let headers = request(&mut stream).to_ascii_lowercase();
                    assert!(
                        headers.starts_with(&format!("http/1.1 {status} ")),
                        "{headers}"
                    );
                    assert!(
                        headers.contains(&format!("content-length: {size}\r\n")),
                        "{headers}"
                    );
                    assert!(headers.contains(&format!("etag: {tag}\r\n")));
                    if method == "HEAD" {
                        assert_eq!(stream.read(&mut [0]).unwrap(), 0);
                    } else {
                        let mut bytes = Vec::new();
                        stream.read_to_end(&mut bytes).unwrap();
                        assert_eq!(bytes, vec![b'x'; size]);
                    }
                }
            });
            drive_client(&mut server, ring, client, Duration::from_secs(25));
            assert_eq!(heads.load(Ordering::Relaxed), 1);
            assert!(pages.load(Ordering::Relaxed) >= 2);
            server.shutdown(ring).unwrap();
            server.handler_mut().shutdown(ring).unwrap();
            assert_eq!(ring.metrics().values()[0] - before[0], 6);
            assert_eq!(
                ring.metrics().values()[3] - before[3],
                (2 * (BUFFER_SIZE + 3) + 3 + 2) as u64,
                "file-to-pipe copies and HEAD must not count payload bytes"
            );
            stop.store(true, Ordering::Release);
            backend_thread.join().unwrap();
        }

        pub(crate) fn http_error_responses(ring: &mut Ring) {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            let backend =
                Backend::new(&listener.local_addr().unwrap().to_string(), "test-origin").unwrap();
            let backend_thread = thread::spawn(move || {
                for _ in 0..4 {
                    let mut stream = accept(&listener);
                    let request = request(&mut stream);
                    if request.contains(" /missing HTTP/1.1\r\n") {
                        stream.write_all(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                    } else if request.contains(" /gone HTTP/1.1\r\n") {
                        stream
                        .write_all(
                            b"HTTP/1.1 410 Gone\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                        )
                        .unwrap();
                    } else if request.starts_with("HEAD ") {
                        write!(stream, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"old")).unwrap();
                    } else {
                        assert!(request.starts_with("GET "));
                        assert!(request.contains(&format!(
                            "If-Match: {}\r\n",
                            crate::conformance::etag(b"old")
                        )));
                        stream.write_all(b"HTTP/1.1 412 Precondition Failed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n").unwrap();
                    }
                }
            });
            let address = TcpListener::bind("127.0.0.1:0").unwrap();
            let port = address.local_addr().unwrap();
            drop(address);
            let listener = http::Listener::bind(port, NonZeroU32::new(8).unwrap()).unwrap();
            let handler = Handler::new(cache(&backend, 1), backend);
            let mut server = http::Server::new(listener, handler, http::Config::default());
            let client = thread::spawn(move || {
                for (target, code) in [("/missing", 404), ("/gone", 410), ("/changed", 412)] {
                    let mut stream = TcpStream::connect(port).unwrap();
                    stream
                        .set_read_timeout(Some(Duration::from_secs(5)))
                        .unwrap();
                    write!(
                        stream,
                        "GET {target} HTTP/1.1\r\nHost: cache\r\nConnection: close\r\n\r\n"
                    )
                    .unwrap();
                    let headers = request(&mut stream);
                    // First-page precondition failure must precede 200 headers.
                    assert!(
                        headers.starts_with(&format!("HTTP/1.1 {code} ")),
                        "{headers}"
                    );
                    assert!(
                        headers
                            .to_ascii_lowercase()
                            .contains("content-length: 0\r\n")
                    );
                    assert_eq!(stream.read(&mut [0]).unwrap(), 0);
                }
            });
            drive_client(&mut server, ring, client, Duration::from_secs(10));
            backend_thread.join().unwrap();
            server.shutdown(ring).unwrap();
            server.handler_mut().shutdown(ring).unwrap();
        }
    }

    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/http/server_lifecycle.rs"
    ));
    include!(concat!(env!("CARGO_MANIFEST_DIR"), "/tests/http/tls.rs"));
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/http/uds_slab.rs"
    ));
}
#[test]
fn authorization_limit_duplicates_and_wide_spans() {
    let prefix = "GET / HTTP/1.1\r\nHost: cache\r\nX-Normal: ";
    let wire = format!(
        "{prefix}{}\r\nRacer-Origin-Data: {}\r\n\r\n",
        "x".repeat(8192 - prefix.len() - 4),
        openssl::base64::encode_block(&vec![0xff; 65536])
    );
    assert_eq!(
        wire.len(),
        8192 + crate::origin_data::MAX_ENCODED_ORIGIN_DATA + 21
    );
    assert!(parse(wire.as_bytes(), wire.len()).is_ok());
    for n in 8175..8220 {
        assert!(
            crate::http::request_budget(&wire.as_bytes()[..n]).is_ok(),
            "split at {n}"
        );
    }
    for size in [65535, 65536] {
        let raw = vec![0xff; size];
        let credential = openssl::base64::encode_block(&raw);
        let wire = format!(
            "GET / HTTP/1.1\r\nHost: cache\r\nRacer-Origin-Data: {credential}\r\nRange: bytes=0-2\r\n\r\n"
        );
        let metadata = parse(wire.as_bytes(), wire.len()).unwrap();
        let headers = Headers {
            bytes: wire.as_bytes(),
            headers: &metadata.headers[..metadata.count],
        };
        assert_eq!(
            headers.get("racer-origin-data").unwrap().len(),
            credential.len()
        );
        assert_eq!(headers.get("range"), Some(b"bytes=0-2".as_slice()));
        assert_eq!(
            crate::origin_data::OriginData::from_headers(headers)
                .unwrap()
                .as_bytes(),
            raw
        );
        // Every possible CRLF fragmentation near the exact limit is admitted.
        for n in wire.len() - 26..wire.len() {
            assert!(crate::http::request_budget(&wire.as_bytes()[..n]).is_ok());
        }
    }
    for (field, status) in [
        (
            format!(
                "Racer-Origin-Data: {}\r\n",
                "x".repeat(crate::origin_data::MAX_ENCODED_ORIGIN_DATA + 1)
            ),
            431,
        ),
        (
            "Racer-Origin-Data: YQ==\r\nracer-origin-data: Yg==\r\n".into(),
            400,
        ),
        ("Racer-Origin-Data: YR==\r\n".into(), 400),
        ("Racer-Origin-Data: a\tb\r\n".into(), 400),
        (format!("X-Normal: {}\r\n", "x".repeat(8192)), 431),
    ] {
        let wire = format!("GET / HTTP/1.1\r\nHost: cache\r\n{field}\r\n");
        assert_eq!(parse(wire.as_bytes(), wire.len()).err(), Some(status));
    }
}
