// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::buffers::{self, Key};
use crate::uring::Config;
use std::io::{Read as _, Write};
use std::net::{TcpListener, TcpStream};
use std::process::{Command, Stdio};
use std::thread;
use std::time::Duration;

fn metadata(bytes: &[u8]) -> io::Result<Metadata> {
    let end = header_end(bytes, &mut 0).ok_or_else(|| protocol("missing terminator"))?;
    parse(bytes, 0, end)
}

#[test]
fn request_validation_and_encoding() {
    let r = Request::new(
        "/object?version=2",
        &[("Range", "bytes=0-4194303"), ("X-Test", "yes")],
    )
    .unwrap();
    let mut scratch = [0; SCRATCH_SIZE];
    let n = r.encode(false, "example:80", &mut scratch).unwrap();
    assert_eq!(&scratch[..n], b"GET /object?version=2 HTTP/1.1\r\nHost: example:80\r\nRange: bytes=0-4194303\r\nX-Test: yes\r\n\r\n");
    for target in [
        "",
        "http://host/x",
        "/x y",
        "/x\r\nY: z",
        "/x#frag",
        "/\u{7f}",
        "/é",
    ] {
        assert!(Request::new(target, &[]).is_err(), "{target:?}");
    }
    for (name, v) in [
        ("", "x"),
        ("Bad Name", "x"),
        ("X", "x\nY: z"),
        ("Host", "x"),
        ("CONTENT-LENGTH", "0"),
        ("Transfer-Encoding", "chunked"),
        ("Connection", "close"),
        ("Expect", "100-continue"),
    ] {
        assert!(Request::new("/", &[(name, v)]).is_err());
    }
    let target = format!("/{}", "x".repeat(SCRATCH_SIZE));
    assert!(
        Request::new(&target, &[])
            .unwrap()
            .encode(false, "host", &mut scratch)
            .is_err()
    );
    for host in ["", "x y", "x\r\nY: z", "x/path", "a@b"] {
        assert!(Connection::new("127.0.0.1:80".parse().unwrap(), host).is_err());
    }
}

#[test]
fn fragmented_scan_and_borrowed_headers() {
    let bytes = b"HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\nETag: \t\"tag\" \t\r\nX: \xff\r\nX: second\r\n\r\nabc";
    for chunk in 1..=bytes.len() {
        let mut cursor = Cursor::default();
        let mut statuses = Vec::new();
        for used in (chunk..bytes.len()).step_by(chunk).chain([bytes.len()]) {
            while let Some(end) = header_end(&bytes[..used], &mut cursor.scan) {
                let m = parse(bytes, cursor.start, end).unwrap();
                statuses.push(m.status);
                if m.status == 206 {
                    assert_eq!(body_length(&m, false).unwrap(), 3);
                    let headers = Headers {
                        bytes,
                        headers: &m.headers[..m.count],
                    };
                    assert_eq!(headers.get("etag"), Some(&b"\"tag\""[..]));
                    assert_eq!(headers.get("x"), Some(&b"\xff"[..]));
                    assert_eq!(headers.iter().filter(|(n, _)| *n == "X").count(), 2);
                }
                cursor.start = end;
                cursor.scan = end;
            }
        }
        assert_eq!(statuses, [103, 206]);
    }
}

#[test]
fn strict_framing() {
    for bytes in [
        &b"HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n"[..],
        b"HTTP/1.1 099 Bad\r\n\r\n",
        b"HTTP/1.1 600 Bad\r\n\r\n",
        b"HTTP/1.1 200\r\n\r\n",
        b"HTTP/1.1 200 O\nK\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nContent-Length: 1\r\nContent-Length: 1\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nContent-Length: 1, 1\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nContent-Length: +1\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nContent-Length: \r\n\r\n",
        b"HTTP/1.1 200 OK\r\nContent-Length: 18446744073709551616\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Length: 0\r\n\r\n",
        b"HTTP/1.1 200 OK\r\n Folded: x\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nX : x\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nX: a\0b\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nConnection: content-length\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nConnection: close,\r\n\r\n",
        b"HTTP/1.1 200 OK\r\nUpgrade: websocket\r\n\r\n",
    ] {
        assert!(metadata(bytes).is_err(), "{bytes:?}");
    }
    for (response, head, expected) in [
        (
            "HTTP/1.1 200 OK\r\nContent-Length: 4194304\r\n\r\n",
            false,
            Some(BUFFER_SIZE),
        ),
        (
            "HTTP/1.1 200 OK\r\nContent-Length: 4194305\r\n\r\n",
            false,
            None,
        ),
        (
            "HTTP/1.1 200 OK\r\nContent-Length: 18446744073709551615\r\n\r\n",
            true,
            Some(0),
        ),
        ("HTTP/1.1 200 OK\r\n\r\n", true, Some(0)),
        ("HTTP/1.1 200 OK\r\n\r\n", false, None),
        ("HTTP/1.1 204 No Content\r\n\r\n", false, Some(0)),
        (
            "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n",
            false,
            None,
        ),
        (
            "HTTP/1.1 304 Not Modified\r\nContent-Length: 9999999\r\n\r\n",
            false,
            Some(0),
        ),
        (
            "HTTP/1.1 205 Reset Content\r\nContent-Length: 0\r\n\r\n",
            false,
            Some(0),
        ),
        (
            "HTTP/1.1 205 Reset Content\r\nContent-Length: 1\r\n\r\n",
            false,
            None,
        ),
        (
            "HTTP/1.1 404 Not Found\r\nContent-Length: 3\r\n\r\n",
            false,
            Some(3),
        ),
    ] {
        assert_eq!(
            body_length(&metadata(response.as_bytes()).unwrap(), head).ok(),
            expected,
            "{response}"
        );
    }
    assert!(
        metadata(b"HTTP/1.1 200 OK\r\nConnection: Keep-Alive, CLOSE\r\n\r\n")
            .unwrap()
            .close
    );
    let response = format!(
        "HTTP/1.1 200 OK\r\n{}\r\n",
        "X: a\r\n".repeat(MAX_HEADERS + 1)
    );
    assert!(metadata(response.as_bytes()).is_err());
    assert_eq!(
        transfer(0, 1).unwrap_err().kind(),
        io::ErrorKind::UnexpectedEof
    );
    assert!(transfer(2, 1).is_err());
    assert_eq!(transfer(1, 10).unwrap(), 1);
}

fn fill(ring: &Ring, key: u8) -> Fill {
    ring.pool().stage(Key::new([key; 32])).unwrap()
}
fn deadline() -> Instant {
    Instant::now() + Duration::from_secs(3)
}
fn drive<T>(
    ring: &mut Ring,
    mut poll: impl FnMut(&mut Ring) -> io::Result<Progress<T>>,
) -> io::Result<T> {
    let end = deadline();
    loop {
        assert!(Instant::now() < end, "HTTP test stalled");
        ring.progress()?;
        match poll(ring)? {
            Progress::Ready(r) => return Ok(r),
            Progress::Pending(work) => {
                if !work.runnable {
                    ring.wait(work.deadline)?;
                }
            }
        }
    }
}
fn request(stream: &mut TcpStream) -> String {
    stream
        .set_read_timeout(Some(Duration::from_secs(3)))
        .unwrap();
    stream
        .set_write_timeout(Some(Duration::from_secs(3)))
        .unwrap();
    let mut bytes = Vec::new();
    loop {
        let mut byte = [0];
        stream.read_exact(&mut byte).unwrap();
        bytes.push(byte[0]);
        if bytes.ends_with(b"\r\n\r\n") {
            return String::from_utf8(bytes).unwrap();
        }
        assert!(bytes.len() <= SCRATCH_SIZE);
    }
}
fn server(f: impl FnOnce(TcpStream) + Send + 'static) -> (Connection, thread::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let c = Connection::new(listener.local_addr().unwrap(), "objects.test").unwrap();
    let thread = thread::spawn(move || f(listener.accept().unwrap().0));
    (c, thread)
}

fn until(ring: &mut Ring, mut ready: impl FnMut(&mut Ring) -> bool) {
    let end = deadline();
    while !ready(ring) {
        assert!(Instant::now() < end, "HTTP lifecycle test stalled");
        ring.progress().unwrap();
        thread::yield_now();
    }
}

fn drained(ring: &mut Ring) {
    until(ring, |r| r.http_test_idle());
    // Hold all four at once: one successful allocation cannot detect leaks.
    let fills: Vec<_> = (230..234).map(|k| fill(ring, k)).collect();
    assert!(ring.pool().stage(Key::new([234; 32])).is_err());
    drop(fills);
}

struct Pressure {
    fd: File,
    _peer: std::os::unix::net::UnixStream,
    tickets: Vec<Ticket<Bytes>>,
}
impl Pressure {
    fn new(ring: &mut Ring, submit: bool) -> Self {
        let (socket, peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let fd = File::new(socket.into());
        let mut tickets = Vec::new();
        loop {
            match ring.recv_bytes(fd.clone().into(), vec![0; 1].into_boxed_slice()) {
                Ok(t) => {
                    tickets.push(t);
                    if submit {
                        ring.progress().unwrap();
                    }
                }
                Err(r) => {
                    assert_eq!(r.error.kind(), io::ErrorKind::WouldBlock);
                    break;
                }
            }
        }
        assert!(!tickets.is_empty());
        Self {
            fd,
            _peer: peer,
            tickets,
        }
    }
    fn release(mut self, ring: &mut Ring) {
        // SAFETY: live socket, EOF retires all blocking receives.
        unsafe {
            libc::shutdown(self.fd.as_fd().as_raw_fd(), libc::SHUT_RDWR);
        }
        for ticket in &mut self.tickets {
            until(ring, |r| r.take_bytes(ticket).unwrap().is_some());
        }
    }
}

fn connected(ring: &mut Ring) -> (Connection, TcpStream) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let stream = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let peer = listener.accept().unwrap().0;
    let file = File::new(stream.into());
    let fixed = ring.register_file(file.clone()).unwrap();
    (
        Connection {
            socket: Socket {
                tls: None,
                transferred: false,
                credential_revision: 0,
                file,
                started: crate::environment::now(),
                endpoint: listener.local_addr().unwrap(),
                transport: Transport::Connected(fixed, ring.identity().clone()),
                host: "objects.test".into(),
            },
            scratch: vec![0; SCRATCH_SIZE].into_boxed_slice(),
        },
        peer,
    )
}

#[test]
fn kernel_integration() {
    crate::http_server::cache_responses::assert_child_selected("http_client::tests::kernel_child");
    let mut child = Command::new(std::env::current_exe().unwrap())
        .args([
            "--exact",
            "http_client::tests::kernel_child",
            "--ignored",
            "--nocapture",
        ])
        .env("RACER_HTTP_CHILD", "1")
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit())
        .spawn()
        .unwrap();
    let end = Instant::now() + Duration::from_secs(30);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            assert!(status.success());
            break;
        }
        if Instant::now() >= end {
            child.kill().unwrap();
            child.wait().unwrap();
            panic!("HTTP kernel child timed out");
        }
        thread::sleep(Duration::from_millis(10));
    }
}

#[test]
#[ignore = "run via bounded kernel_integration subprocess"]
fn kernel_child() {
    if std::env::var_os("RACER_HTTP_CHILD").is_none() {
        return;
    }
    let config = Config {
        progress_reserve: 0,
        entries: 8,
        requests: 16,
        fixed_files: 4,
        completion_budget: 2,
        shutdown_timeout: Duration::from_millis(500),
    };
    let mut ring = match Ring::http_test_ring(buffers::io_test_pool(4), config) {
        Ok(r) => r,
        Err(e)
            if matches!(
                e.raw_os_error(),
                Some(libc::EPERM | libc::ENOSYS | libc::ENOMEM)
            ) || e.kind() == io::ErrorKind::Unsupported =>
        {
            assert!(
                std::env::var_os("RACER_REQUIRE_URING").is_none(),
                "io_uring required: {e}"
            );
            eprintln!("SKIP HTTP io_uring kernel tests: {e}");
            return;
        }
        Err(e) => panic!("ring: {e}"),
    };
    reuse(&mut ring);
    destination_get(&mut ring);
    small_get_pinned_fragmented_and_bounded(&mut ring);
    destination_cancellation(&mut ring);
    short_send(&mut ring);
    concurrent_bulk(&mut ring);
    backpressure(&mut ring);
    admission_timeout_evidence(&mut ring);
    receive_backpressure(&mut ring);
    failures(&mut ring);
    excess_bytes(&mut ring);
    cancellation(&mut ring);
    connect_cancellation(&mut ring);
    byte_ranges(&mut ring);
    ring.shutdown().unwrap();
}

fn small_get_pinned_fragmented_and_bounded(ring: &mut Ring) {
    let pinned: Vec<_> = (0..4)
        .map(|_| ring.pool().private_fill().unwrap())
        .collect();
    for oversized in [false, true] {
        let (connection, mut peer) = connected(ring);
        let mut exchange = connection
            .get_small(Request::new("/metadata", &[]).unwrap(), 48, deadline())
            .unwrap();
        let server = thread::spawn(move || {
            let mut request = [0; 1024];
            peer.read(&mut request).unwrap();
            write!(
                peer,
                "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                if oversized { 49 } else { 48 }
            )
            .unwrap();
            if !oversized {
                for _ in 0..48 {
                    peer.write_all(&[7]).unwrap();
                    thread::sleep(Duration::from_millis(1));
                }
            }
        });
        let response = drive(ring, |ring| exchange.poll(ring, 1));
        if oversized {
            assert!(response.is_err());
        } else {
            assert_eq!(response.unwrap().body(), &[7; 48]);
        }
        server.join().unwrap();
    }
    drop(pinned);
    drained(ring);
}

fn destination_get(ring: &mut Ring) {
    let (c, peer) = server(|mut stream| {
        request(&mut stream);
        stream.write_all(b"HTTP/1.1 206 Partial Content\r\nContent-Length: 65536\r\nETag: restricted\r\n\r\n").unwrap();
        stream.write_all(&[0xab; 65536]).unwrap();
    });
    let (authority, destination) = fill(ring, 70).split_destination();
    let address = destination.region().region.address;
    let mut exchange: DestinationGetExchange = c
        .get(
            Request::new("/restricted", &[]).unwrap(),
            destination,
            deadline(),
        )
        .unwrap();
    let mut response: DestinationGetResponse = drive(ring, |r| exchange.poll(r, 1)).unwrap();
    assert_eq!(response.status(), 206);
    assert_eq!(
        response.headers().get("etag"),
        Some(b"restricted".as_slice())
    );
    assert_eq!(response.body(), &[0xab; 65536]);
    assert_eq!(ring.pool().invariant_snapshot().loading, 1);
    let (connection, destination, len) = response.recycle();
    assert_eq!(destination.region().region.address, address);
    let fill = authority
        .reunite(destination)
        .unwrap_or_else(|_| panic!("lost HTTP origin"));
    assert_eq!(fill.publish(len).unwrap().as_slice(), &[0xab; 65536]);
    drop(connection);
    peer.join().unwrap();
    drained(ring);
}

fn admission_timeout_evidence(ring: &mut Ring) {
    use attempt::{Cause, Failure, Phase};
    for service in [false, true] {
        let pressure = Pressure::new(ring, false);
        let mut exchange = Connection::new("127.0.0.1:9".parse().unwrap(), "test")
            .unwrap()
            .get(Request::new("/", &[]).unwrap(), fill(ring, 70), deadline())
            .unwrap()
            .service_deadline(service);
        assert!(matches!(
            exchange.poll(ring, 1).unwrap(),
            Progress::Pending(_)
        ));
        exchange.0.deadline = Instant::now();
        let error = exchange.poll(ring, 1).err().unwrap();
        let cache_error = crate::cache::Error::from(error);
        let crate::cache::Error::Io(error) = cache_error else {
            panic!("lost timeout provenance")
        };
        let evidence = error.get_ref().unwrap().downcast_ref::<Failure>().unwrap();
        assert_eq!(evidence.phase, Phase::Connect);
        assert_eq!(evidence.cause, Cause::LocalPressure);
        assert!(!evidence.initiated);
        assert!(!evidence.owner_evidence());
        drop(exchange);
        pressure.release(ring);
        drained(ring);
    }
}

fn destination_cancellation(ring: &mut Ring) {
    for mode in 0..3 {
        let (c, peer) = server(|mut stream| {
            request(&mut stream);
            stream
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nabc")
                .unwrap();
            let mut byte = [0];
            assert_eq!(stream.read(&mut byte).unwrap(), 0);
        });
        let (authority, destination) = fill(ring, 71).split_destination();
        let held: Vec<_> = (72..75).map(|k| fill(ring, k)).collect();
        let mut exchange = c
            .get(
                Request::new("/stall", &[]).unwrap(),
                destination,
                deadline(),
            )
            .unwrap();
        until(ring, |ring| {
            assert!(matches!(
                exchange.poll(ring, 1).unwrap(),
                Progress::Pending(_)
            ));
            matches!(exchange.0.state, State::ReceivingBody(..))
        });
        ring.progress().unwrap();
        drop(authority);
        retire(exchange, ring, mode);
        // No target CQE was processed by retirement, even if cancel was queued.
        assert!(ring.pool().stage(Key::new([75; 32])).is_err());
        peer.join().unwrap();
        until(ring, |r| r.http_test_idle());
        drop(fill(ring, 75));
        drop(held);
        drained(ring);
    }
}

#[test]
fn destination_exchange_drop_before_submission_releases_storage() {
    let pool = buffers::io_test_pool(1);
    let fill = pool.stage(Key::new([1; 32])).unwrap();
    let (authority, destination) = fill.split_destination();
    let c = Connection::new("127.0.0.1:80".parse().unwrap(), "test").unwrap();
    let exchange = c
        .get(Request::new("/", &[]).unwrap(), destination, deadline())
        .unwrap();
    drop(authority);
    assert!(pool.stage(Key::new([2; 32])).is_err());
    drop(exchange);
    assert!(pool.stage(Key::new([2; 32])).is_ok());
}

fn reuse(ring: &mut Ring) {
    let (c, peer) = server(|mut stream| {
        assert_eq!(
            request(&mut stream),
            "HEAD /object HTTP/1.1\r\nHost: objects.test\r\nX-Caller: yes\r\n\r\n"
        );
        for byte in b"HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 9999999\r\nETag: \"tag\"\r\n\r\n" {
            stream.write_all(&[*byte]).unwrap();
        }
        assert!(request(&mut stream).contains("Range: bytes=0-2\r\n"));
        stream
            .write_all(b"HTTP/1.1 206 Partial Content\r\nContent-Length: 3\r\n\r\nabc")
            .unwrap();
        assert!(request(&mut stream).starts_with("GET /missing "));
        stream
            .write_all(
                b"HTTP/1.1 404 Not Found\r\nContent-Length: 3\r\nConnection: close\r\n\r\nbad",
            )
            .unwrap();
    });
    let scratch = c.scratch.as_ptr();
    let mut head = c
        .head(
            Request::new("/object", &[("X-Caller", "yes")]).unwrap(),
            deadline(),
        )
        .unwrap();
    let response = drive(ring, |r| head.poll(r, 2)).unwrap();
    assert_eq!(response.status(), 200);
    assert_eq!(response.content_length(), Some(9999999));
    assert_eq!(response.headers().get("ETag"), Some(&b"\"tag\""[..]));
    let c = response.recycle().unwrap();
    assert_eq!(c.scratch.as_ptr(), scratch);
    let mut get = c
        .get(
            Request::new("/object", &[("Range", "bytes=0-2")]).unwrap(),
            fill(ring, 1),
            deadline(),
        )
        .unwrap();
    let mut response = drive(ring, |r| get.poll(r, 1)).unwrap();
    assert_eq!(response.status(), 206);
    assert_eq!(response.body(), b"abc");
    let (c, f, n) = response.recycle();
    assert_eq!(f.publish(n).unwrap().as_slice(), b"abc");
    let c = c.unwrap();
    assert_eq!(c.scratch.as_ptr(), scratch);
    let mut get = c
        .get(
            Request::new("/missing", &[]).unwrap(),
            fill(ring, 2),
            deadline(),
        )
        .unwrap();
    let mut response = drive(ring, |r| get.poll(r, 4)).unwrap();
    assert_eq!(response.status(), 404);
    assert_eq!(response.body(), b"bad");
    assert!(response.recycle().0.is_none());
    peer.join().unwrap();
}

fn concurrent_bulk(ring: &mut Ring) {
    let mut exchanges = Vec::new();
    let mut peers = Vec::new();
    for key in 10..14 {
        let (c, peer) = server(move |mut stream| {
            request(&mut stream);
            stream
                .write_all(
                    format!("HTTP/1.1 200 OK\r\nContent-Length: {BUFFER_SIZE}\r\n\r\n").as_bytes(),
                )
                .unwrap();
            let chunk = [key; 16384];
            for _ in 0..BUFFER_SIZE / chunk.len() {
                stream.write_all(&chunk).unwrap();
            }
        });
        exchanges.push(Some(
            c.get(
                Request::new("/bulk", &[]).unwrap(),
                fill(ring, key),
                deadline(),
            )
            .unwrap(),
        ));
        peers.push(peer);
    }
    let end = deadline();
    while exchanges.iter().any(Option::is_some) {
        assert!(Instant::now() < end);
        ring.progress().unwrap();
        let mut runnable = false;
        for (index, slot) in exchanges.iter_mut().enumerate() {
            if let Some(exchange) = slot {
                match exchange.poll(ring, 2).unwrap() {
                    Progress::Pending(work) => runnable |= work.runnable,
                    Progress::Ready(mut r) => {
                        assert_eq!(r.body().len(), BUFFER_SIZE);
                        assert!(r.body().iter().all(|&b| b == index as u8 + 10));
                        let (_, fill, len) = r.recycle();
                        assert_eq!(fill.publish(len).unwrap().as_slice().len(), BUFFER_SIZE);
                        *slot = None;
                    }
                }
            }
        }
        if !runnable && exchanges.iter().any(Option::is_some) {
            ring.wait(Some(end)).unwrap();
        }
    }
    for peer in peers {
        peer.join().unwrap();
    }
}

fn short_send(ring: &mut Ring) {
    let (c, peer) = server(|mut stream| {
        assert_eq!(
            request(&mut stream),
            "HEAD /short HTTP/1.1\r\nHost: objects.test\r\n\r\n"
        );
        stream
            .write_all(b"HTTP/1.1 204 No Content\r\n\r\n")
            .unwrap();
    });
    let mut head = c
        .head(Request::new("/short", &[]).unwrap(), deadline())
        .unwrap();
    let end = deadline();
    while !matches!(head.0.state, State::Send(..)) {
        assert!(Instant::now() < end);
        ring.progress().unwrap();
        assert!(matches!(head.poll(ring, 1).unwrap(), Progress::Pending(_)));
    }
    let State::Send(p, sent) = std::mem::replace(&mut head.0.state, State::Finished) else {
        unreachable!()
    };
    assert_eq!(sent, 0);
    let scratch = p.scratch.as_ptr();
    let Transport::Connected(fd, _) = &head.0.socket.as_ref().unwrap().transport else {
        unreachable!()
    };
    // Force a real one-byte completion, independent of TCP send-buffer sizing.
    // The normal Sending state must send the entire remaining request once.
    let t = ring
        .send_bytes_range(fd.clone().into(), p.scratch, 0..1)
        .unwrap();
    head.0.state = State::Sending(p.body, t, 0);
    until(ring, |r| {
        assert!(matches!(head.poll(r, 1).unwrap(), Progress::Pending(_)));
        matches!(head.0.state, State::Send(_, 1))
    });
    let pressure = Pressure::new(ring, true);
    for _ in 0..2 {
        assert!(matches!(
            head.poll(ring, 8).unwrap(),
            Progress::Pending(Work { runnable: true, .. })
        ));
        let State::Send(p, sent) = &head.0.state else {
            panic!("lost short-send state")
        };
        assert_eq!(*sent, 1);
        assert_eq!(p.scratch.as_ptr(), scratch);
    }
    pressure.release(ring);
    let response = drive(ring, |r| head.poll(r, 1)).unwrap();
    assert_eq!(response.status(), 204);
    assert_eq!(response.recycle().unwrap().scratch.as_ptr(), scratch);
    peer.join().unwrap();
}

fn backpressure(ring: &mut Ring) {
    use std::os::unix::net::UnixStream;
    let (socket, _peer) = UnixStream::pair().unwrap();
    let fd = File::new(socket.into());
    let occupy = |ring: &mut Ring| {
        let mut tickets = Vec::new();
        loop {
            match ring.recv_bytes(fd.clone().into(), vec![0; 1].into_boxed_slice()) {
                Ok(t) => {
                    tickets.push(t);
                    ring.progress().unwrap();
                }
                Err(r) => {
                    assert_eq!(r.error.kind(), io::ErrorKind::WouldBlock);
                    break;
                }
            }
        }
        tickets
    };
    let release = |ring: &mut Ring, mut tickets: Vec<Ticket<Bytes>>| {
        // SAFETY: live socket; EOF completes every outstanding receive.
        unsafe {
            libc::shutdown(fd.as_fd().as_raw_fd(), libc::SHUT_RDWR);
        }
        for t in &mut tickets {
            drive(ring, |r| {
                Ok(match r.take_bytes(t)? {
                    Some(c) => Progress::Ready(c),
                    None => Progress::Pending(Work {
                        runnable: false,
                        deadline: Some(deadline()),
                    }),
                })
            })
            .unwrap();
        }
    };
    let (c, peer) = server(|mut stream| {
        request(&mut stream);
        stream
            .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
            .unwrap();
    });
    let scratch = c.scratch.as_ptr();
    let mut head = c.head(Request::new("/", &[]).unwrap(), deadline()).unwrap();
    let tickets = occupy(ring);
    assert!(matches!(
        head.poll(ring, 8).unwrap(),
        Progress::Pending(Work { runnable: true, .. })
    ));
    let State::Connect(p) = &head.0.state else {
        panic!("lost connect state")
    };
    assert_eq!(p.scratch.as_ptr(), scratch);
    release(ring, tickets);
    // Hold the whole fixed-file table while connect completes.
    let fixed: Vec<_> = (0..4)
        .map(|_| ring.register_file(fd.clone()).unwrap())
        .collect();
    until(ring, |r| {
        assert!(matches!(head.poll(r, 1).unwrap(), Progress::Pending(_)));
        matches!(head.0.state, State::Register(_))
    });
    for _ in 0..2 {
        assert!(matches!(
            head.poll(ring, 8).unwrap(),
            Progress::Pending(Work { runnable: true, .. })
        ));
        let State::Register(p) = &head.0.state else {
            panic!("lost register state")
        };
        assert_eq!(p.scratch.as_ptr(), scratch);
    }
    drop(fixed);
    let response = drive(ring, |r| head.poll(r, 2)).unwrap();
    assert_eq!(response.recycle().unwrap().scratch.as_ptr(), scratch);
    peer.join().unwrap();
    drained(ring);
}

fn receive_backpressure(ring: &mut Ring) {
    for body_stage in [false, true] {
        let (c, mut peer) = connected(ring);
        let scratch = c.scratch.as_ptr();
        let f = fill(ring, 40);
        let index = f.region().index;
        let mut get = c
            .get(Request::new("/", &[]).unwrap(), f, deadline())
            .unwrap();
        let State::Send(mut p, _) = std::mem::replace(&mut get.0.state, State::Finished) else {
            unreachable!()
        };
        // Model a completed receive with a nonzero prefix; TCP packetization
        // cannot determine these state boundaries for us.
        let prefix = b"HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: ";
        if body_stage {
            let headers = b"HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\n";
            p.scratch[..headers.len()].copy_from_slice(headers);
            let Body::Get(mut f) = p.body else {
                unreachable!()
            };
            f.as_mut_slice()[..3].copy_from_slice(b"abc");
            get.0.state = State::Body(p.scratch, metadata(headers).unwrap(), f, 3, 6);
            peer.write_all(b"def").unwrap();
        } else {
            p.scratch[..prefix.len()].copy_from_slice(prefix);
            get.0.state = State::Headers(
                p,
                Cursor {
                    used: prefix.len(),
                    ..Default::default()
                },
            );
            peer.write_all(b"6\r\n\r\nabcdef").unwrap();
            // Consume the informational header before forcing recv pressure.
            assert!(matches!(get.poll(ring, 1).unwrap(), Progress::Pending(_)));
        }
        let pressure = Pressure::new(ring, true);
        for _ in 0..2 {
            assert!(matches!(
                get.poll(ring, 8).unwrap(),
                Progress::Pending(Work { runnable: true, .. })
            ));
            match &mut get.0.state {
                State::Headers(p, cursor) if !body_stage => {
                    assert_eq!(p.scratch.as_ptr(), scratch);
                    assert_eq!(&p.scratch[..prefix.len()], prefix);
                    assert_eq!(cursor.used, prefix.len());
                    assert_eq!(cursor.start, b"HTTP/1.1 103 Early Hints\r\n\r\n".len());
                    assert_eq!(cursor.informational, 1);
                    let Body::Get(f) = &p.body else {
                        unreachable!()
                    };
                    assert_eq!(f.region().index, index);
                }
                State::Body(s, _, f, received, len) if body_stage => {
                    assert_eq!(s.as_ptr(), scratch);
                    assert_eq!((*received, *len), (3, 6));
                    assert_eq!(f.region().index, index);
                    assert_eq!(&f.as_mut_slice()[..3], b"abc");
                }
                _ => panic!("lost receive state"),
            }
        }
        pressure.release(ring);
        let mut response = drive(ring, |r| get.poll(r, 1)).unwrap();
        assert_eq!(response.body(), b"abcdef");
        let (c, f, n) = response.recycle();
        assert_eq!(c.as_ref().unwrap().scratch.as_ptr(), scratch);
        assert_eq!(f.region().index, index);
        assert_eq!(n, 6);
        drop((c, f, peer));
        drained(ring);
    }
}

fn failures(ring: &mut Ring) {
    let mut cases = vec![
        b"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nx".to_vec(),
        b"HTTP/1.1 200 OK\r\nContent-Length: 4194305\r\n\r\n".to_vec(),
        b"HTTP/1.1 101 Switching Protocols\r\n\r\n".to_vec(),
        b"HTTP/1.1 100 Continue\r\nContent-Length: 0\r\n\r\n".to_vec(),
        b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n".to_vec(),
        b"HTTP/1.1 103 Early Hints\r\n\r\n".repeat(MAX_INFORMATIONAL + 1),
    ];
    let mut oversized = b"HTTP/1.1 200 OK\r\nX: ".to_vec();
    oversized.resize(SCRATCH_SIZE, b'x');
    cases.push(oversized);
    for bytes in cases {
        let (c, peer) = server(move |mut stream| {
            request(&mut stream);
            stream.write_all(&bytes).unwrap();
        });
        let mut get = c
            .get(Request::new("/", &[]).unwrap(), fill(ring, 20), deadline())
            .unwrap();
        assert!(drive(ring, |r| get.poll(r, 8)).is_err());
        assert!(get.0.socket.is_none());
        peer.join().unwrap();
        ring.progress().unwrap();
    }
    // Validate the pool even if the entire body would fit in scratch.
    let other = buffers::io_test_pool(1);
    let f = other.stage(Key::new([0; 32])).unwrap();
    let c = Connection::new("127.0.0.1:1".parse().unwrap(), "host").unwrap();
    let mut get = c
        .get(Request::new("/", &[]).unwrap(), f, deadline())
        .unwrap();
    assert!(get.poll(ring, 1).is_err());
}

fn excess_bytes(ring: &mut Ring) {
    // Inject a completed receive into Headers: a TCP write is not a recv
    // boundary, so a live server cannot reliably exercise excess prefix.
    for bytes in [
        &b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\nexcess"[..],
        &b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc!"[..],
    ] {
        let c = Connection::new("127.0.0.1:1".parse().unwrap(), "host").unwrap();
        let mut get = c
            .get(Request::new("/", &[]).unwrap(), fill(ring, 50), deadline())
            .unwrap();
        let State::Connect(mut p) = std::mem::replace(&mut get.0.state, State::Finished) else {
            unreachable!()
        };
        p.scratch[..bytes.len()].copy_from_slice(bytes);
        get.0.state = State::Headers(
            p,
            Cursor {
                used: bytes.len(),
                ..Default::default()
            },
        );
        let error = match get.poll(ring, 1) {
            Err(e) => e,
            _ => panic!("accepted excess prefix"),
        };
        assert_eq!(error.to_string(), "bytes beyond response body");
        assert!(get.0.socket.is_none());
    }
    // The peer sends excess only after the first response has been recycled.
    // It must poison the next response rather than be silently discarded.
    let (send, recv) = std::sync::mpsc::channel();
    let (c, peer) = server(move |mut stream| {
        request(&mut stream);
        stream
            .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
            .unwrap();
        recv.recv_timeout(Duration::from_secs(3)).unwrap();
        stream.write_all(b"excess").unwrap();
        request(&mut stream);
        stream
            .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
            .unwrap();
    });
    let mut get = c
        .get(
            Request::new("/first", &[]).unwrap(),
            fill(ring, 50),
            deadline(),
        )
        .unwrap();
    let response = drive(ring, |r| get.poll(r, 2)).unwrap();
    let (c, f, n) = response.recycle();
    assert_eq!(n, 0);
    drop(f);
    send.send(()).unwrap();
    let mut get = c
        .unwrap()
        .get(
            Request::new("/next", &[]).unwrap(),
            fill(ring, 51),
            deadline(),
        )
        .unwrap();
    assert!(drive(ring, |r| get.poll(r, 2)).is_err());
    assert!(get.0.socket.is_none());
    peer.join().unwrap();
    drained(ring);
}

fn retire<B: Writable>(mut get: GetExchange<B>, ring: &mut Ring, mode: usize) {
    match mode {
        0 => get.cancel(ring).unwrap(),
        1 => drop(get),
        _ => {
            get.0.deadline = Instant::now();
            let error = get.poll(ring, 1).err().unwrap();
            assert_eq!(error.kind(), io::ErrorKind::TimedOut);
            let evidence = error
                .get_ref()
                .unwrap()
                .downcast_ref::<attempt::Failure>()
                .unwrap();
            assert_eq!(evidence.cause, attempt::Cause::CallerDeadline);
            assert!(!evidence.owner_evidence());
            assert!(get.0.socket.is_none());
        }
    }
}

fn cancellation(ring: &mut Ring) {
    for (body, mode, full) in [false, true].into_iter().flat_map(|body| {
        (0..3).flat_map(move |mode| [false, true].map(move |full| (body, mode, full)))
    }) {
        let (c, peer) = server(move |mut stream| {
            request(&mut stream);
            if body {
                stream
                    .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nabc")
                    .unwrap();
            }
            let mut b = [0];
            assert_eq!(stream.read(&mut b).unwrap(), 0);
        });
        let f = fill(ring, 30);
        let index = f.region().index;
        let held: Vec<_> = (31..34).map(|k| fill(ring, k)).collect();
        let mut get = c
            .get(Request::new("/stall", &[]).unwrap(), f, deadline())
            .unwrap();
        let end = deadline();
        loop {
            assert!(Instant::now() < end);
            ring.progress().unwrap();
            assert!(matches!(get.poll(ring, 1).unwrap(), Progress::Pending(_)));
            if if body {
                matches!(get.0.state, State::ReceivingBody(..))
            } else {
                matches!(get.0.state, State::ReceivingHeaders(..))
            } {
                break;
            }
        }
        ring.progress().unwrap();
        let pressure = full.then(|| Pressure::new(ring, false));
        if full {
            let error = match &get.0.state {
                State::ReceivingBody(_, _, t, _, _) => ring.cancel(t).unwrap_err(),
                State::ReceivingHeaders(_, t, _) => ring.cancel(t).unwrap_err(),
                _ => unreachable!(),
            };
            assert_eq!(error.kind(), io::ErrorKind::WouldBlock);
        }
        retire(get, ring, mode);
        if body {
            // No CQE has been processed since retirement. The target's Fill
            // must still occupy its exact slot, even after socket shutdown.
            assert!(ring.pool().stage(Key::new([34; 32])).is_err());
        }
        peer.join().unwrap();
        if let Some(p) = pressure {
            p.release(ring);
        }
        until(ring, |r| r.http_test_idle());
        let recovered = fill(ring, 34);
        assert_eq!(recovered.region().index, index);
        drop((recovered, held));
        drained(ring);
    }
}

fn connect_cancellation(ring: &mut Ring) {
    for (submitted, mode, pressure_mode) in [false, true].into_iter().flat_map(|submitted| {
        (0..3).flat_map(move |mode| {
            (0..if submitted { 3 } else { 2 })
                .map(move |pressure_mode| (submitted, mode, pressure_mode))
        })
    }) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        // A full accept queue keeps the second connect genuinely pending in
        // the kernel, rather than merely leaving a successful CQE uncollected.
        let blocker = submitted.then(|| {
            // SAFETY: live listening socket; Linux permits a zero backlog.
            assert_eq!(unsafe { libc::listen(listener.as_raw_fd(), 0) }, 0);
            TcpStream::connect(listener.local_addr().unwrap()).unwrap()
        });
        let c = Connection::new(listener.local_addr().unwrap(), "host").unwrap();
        let mut get = c
            .get(Request::new("/", &[]).unwrap(), fill(ring, 60), deadline())
            .unwrap();
        assert!(matches!(get.poll(ring, 1).unwrap(), Progress::Pending(_)));
        assert!(matches!(get.0.state, State::Connecting(..)));
        if submitted {
            ring.progress().unwrap();
            let State::Connecting(_, t) = &mut get.0.state else {
                unreachable!()
            };
            assert!(ring.take_control(t).unwrap().is_none());
        }
        // Exercise both SQ exhaustion and an entirely occupied request table.
        let pressure = (pressure_mode != 0).then(|| Pressure::new(ring, pressure_mode == 2));
        if pressure_mode != 0 {
            let State::Connecting(_, t) = &get.0.state else {
                unreachable!()
            };
            assert_eq!(
                ring.cancel(t).unwrap_err().kind(),
                io::ErrorKind::WouldBlock
            );
        }
        retire(get, ring, mode);
        if pressure_mode == 2 {
            // Keep every unrelated request stalled while deferred cancel
            // retires the connect without allocating a request-table slot.
            let occupied = pressure.as_ref().unwrap().tickets.len();
            until(ring, |r| r.http_test_request_count() == occupied);
        }
        if let Some(p) = pressure {
            p.release(ring);
        }
        drained(ring);
        if let Some(blocker) = blocker {
            let (_, addr) = listener.accept().unwrap();
            assert_eq!(addr, blocker.local_addr().unwrap());
        }
        assert_eq!(
            listener.accept().unwrap_err().kind(),
            io::ErrorKind::WouldBlock,
            "abandoned CONNECT reached the listener"
        );
    }
    // More abandoned connects than the ring's completion budget must all
    // be suppressed before the next SQ publication (including wait()).
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    for k in 60..64 {
        let c = Connection::new(listener.local_addr().unwrap(), "host").unwrap();
        let mut get = c
            .get(Request::new("/", &[]).unwrap(), fill(ring, k), deadline())
            .unwrap();
        assert!(matches!(get.poll(ring, 1).unwrap(), Progress::Pending(_)));
        drop(get);
    }
    ring.wait(Some(deadline())).unwrap();
    drained(ring);
    assert_eq!(
        listener.accept().unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
}

fn byte_ranges(ring: &mut Ring) {
    use std::os::unix::net::UnixStream;
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let fd = File::new(socket.into());
    let rejected = ring
        .send_bytes_range(fd.clone().into(), vec![0; 8].into_boxed_slice(), 7..9)
        .unwrap_err();
    assert_eq!(rejected.resource.len(), 8);
    let mut send = ring
        .send_bytes_range(
            fd.clone().into(),
            b"xxabczz".to_vec().into_boxed_slice(),
            2..5,
        )
        .unwrap();
    let result = drive(ring, |r| {
        Ok(match r.take_bytes(&mut send)? {
            Some(c) => Progress::Ready(c),
            None => Progress::Pending(Work {
                runnable: false,
                deadline: Some(deadline()),
            }),
        })
    })
    .unwrap();
    assert_eq!(result.result.unwrap(), 3);
    assert_eq!(&*result.resource, b"xxabczz");
    let mut received = [0; 3];
    peer.read_exact(&mut received).unwrap();
    assert_eq!(&received, b"abc");
    peer.write_all(b"q").unwrap();
    let mut recv = ring
        .recv_bytes_range(fd.into(), b"1234567".to_vec().into_boxed_slice(), 2..5)
        .unwrap();
    let result = drive(ring, |r| {
        Ok(match r.take_bytes(&mut recv)? {
            Some(c) => Progress::Ready(c),
            None => Progress::Pending(Work {
                runnable: false,
                deadline: Some(deadline()),
            }),
        })
    })
    .unwrap();
    assert_eq!(result.result.unwrap(), 1);
    assert_eq!(&*result.resource, b"12q4567");
}

mod idle_pressure {
    use super::*;
    use crate::{
        http_server as server,
        simulation::{Choice, World},
        uring::Config,
    };
    use std::{cell::Cell, num::NonZeroU32, time::Duration};

    struct Head(Rc<Cell<usize>>);
    impl server::Handler for Head {
        type Task = server::SendingHeadHeaders;
        fn start(&mut self, request: server::Request) -> Self::Task {
            self.0.set(self.0.get() + 1);
            let server::Request::Head(request) = request else {
                panic!("HEAD only")
            };
            request
                .respond(server::ResponseHead::new(200, Some(3), &[]).unwrap())
                .unwrap()
        }
        fn poll(
            &mut self,
            task: &mut Self::Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<server::Completed>> {
            task.poll(ring, budget)
        }
    }
    struct Fixture {
        local: Ring,
        remote: Ring,
        backend: server::Server<Head>,
        ingress: server::Server<Head>,
        origins: Vec<Origin>,
        hits: Rc<Cell<usize>>,
        ingress_address: SocketAddr,
    }
    impl Fixture {
        fn new() -> Self {
            let simulated = crate::simulation::current().is_some();
            let bind = |port| {
                server::Listener::bind(
                    SocketAddr::from(([127, 0, 0, 1], if simulated { port } else { 0 })),
                    NonZeroU32::new(32).unwrap(),
                )
                .unwrap()
            };
            let backend = bind(18910);
            let endpoint = Endpoint {
                address: backend.local_addr().unwrap(),
                host: "origin".into(),
            };
            let ingress = bind(18911);
            let ingress_address = ingress.local_addr().unwrap();
            let hits = Rc::new(Cell::new(0));
            Self {
                local: crate::conformance::ring(
                    4,
                    Config {
                        fixed_files: 4,
                        ..Default::default()
                    },
                ),
                remote: crate::conformance::ring(4, Config::default()),
                backend: server::Server::new(backend, Head(hits.clone()), Default::default()),
                ingress: server::Server::new(
                    ingress,
                    Head(Rc::new(Cell::new(0))),
                    Default::default(),
                ),
                origins: (0..4).map(|_| Origin::new(endpoint.clone())).collect(),
                hits,
                ingress_address,
            }
        }
        fn tick(&mut self) {
            // Both rings can service IO; strict replay records the actual turn order.
            let remote_first = crate::simulation::current()
                .is_some_and(|w| w.choose_enabled("b10-ring-order", &[0, 1]) == 1);
            for remote in [remote_first, !remote_first] {
                if remote {
                    self.remote.progress().unwrap();
                    self.backend.poll(&mut self.remote, 64).unwrap();
                } else {
                    self.local.progress().unwrap();
                    self.ingress.poll(&mut self.local, 64).unwrap();
                }
            }
            if let Some(world) = crate::simulation::current() {
                world.service_tick();
            } else {
                std::thread::yield_now();
            }
        }
        fn head(&mut self, index: usize) -> io::Result<()> {
            let (connection, permit) = self.origins[index].connection()?;
            let response = self.response(connection)?;
            self.origins[index].recycle(response.recycle());
            permit.success();
            Ok(())
        }
        fn response(&mut self, connection: Connection) -> io::Result<HeadResponse> {
            let mut exchange = connection.head(
                Request::new("/", &[])?,
                crate::environment::now() + Duration::from_secs(2),
            )?;
            let end = Instant::now() + Duration::from_secs(3);
            loop {
                assert!(Instant::now() < end, "fixture watchdog");
                match exchange.poll(&mut self.local, 64)? {
                    Progress::Ready(response) => {
                        assert_eq!(response.status(), 200);
                        return Ok(response);
                    }
                    Progress::Pending(_) => self.tick(),
                }
            }
        }
        fn ingress(&mut self) -> Connection {
            let mut exchange = Connection::new(self.ingress_address, "ingress")
                .unwrap()
                .head(
                    Request::new("/", &[]).unwrap(),
                    crate::environment::now() + Duration::from_secs(2),
                )
                .unwrap();
            let end = Instant::now() + Duration::from_secs(3);
            loop {
                assert!(Instant::now() < end);
                match exchange.poll(&mut self.remote, 64).unwrap() {
                    Progress::Ready(response) => return response.recycle().unwrap(),
                    Progress::Pending(_) => self.tick(),
                }
            }
        }
        fn finish(mut self) {
            self.origins.clear();
            self.backend.shutdown(&mut self.remote).unwrap();
            self.ingress.shutdown(&mut self.local).unwrap();
            self.local.shutdown().unwrap();
            self.remote.shutdown().unwrap();
        }
    }
    fn pools() {
        let mut f = Fixture::new();
        // Distinct real Origin pools model separate volumes/retained generations.
        // Idle sockets must not reserve fixed-file slots needed by active traffic.
        for i in 0..3 {
            f.head(i).unwrap();
        }
        let ingress = f.ingress();
        f.head(0).expect("warm reuse with ingress");
        assert_eq!(f.hits.get(), 4);
        f.head(3)
            .expect("idle pools must not starve cold origin with ingress");
        assert_eq!(f.hits.get(), 5);
        let connects = crate::simulation::current()
            .map(|w| w.counts()[crate::uring_sys::abi::CONNECT as usize]);
        for i in 0..4 {
            f.head(i).expect("warm pooled TCP reuse");
        }
        assert_eq!(f.hits.get(), 9);
        if let Some(connects) = connects {
            assert_eq!(
                crate::simulation::current().unwrap().counts()
                    [crate::uring_sys::abi::CONNECT as usize],
                connects
            );
        }
        // Replace generations while retaining old pools, then retire both sets.
        let endpoint = f.origins[0].endpoint.clone();
        let retained = std::mem::replace(
            &mut f.origins,
            (0..4).map(|_| Origin::new(endpoint.clone())).collect(),
        );
        for i in 0..4 {
            f.head(i).unwrap();
        }
        assert!(retained.iter().all(|o| o.idle.len() == 1));
        drop(retained);
        drop(ingress);
        f.finish();
        eprintln!(
            "B10 pools: 13 successful HEADs, warm TCP reuse, cold progress with ingress, retained pools retired"
        );
    }
    fn pinned() {
        let mut f = Fixture::new();
        let mut responses = Vec::new();
        for i in 0..4 {
            let (connection, permit) = f.origins[i].connection().unwrap();
            responses.push(f.response(connection).unwrap());
            permit.success();
        }
        let hits = f.hits.get();
        let start = crate::environment::now();
        let error = f
            .head(0)
            .expect_err("unrecycled responses pin all four registrations");
        let failure = error
            .get_ref()
            .unwrap()
            .downcast_ref::<attempt::Failure>()
            .unwrap();
        assert_eq!(failure.cause, attempt::Cause::LocalPressure);
        assert_eq!(failure.phase, attempt::Phase::LocalAdmission);
        assert!(!failure.initiated && !failure.owner_evidence());
        assert!(crate::environment::now() - start < Duration::from_millis(500));
        assert_eq!(f.hits.get(), hits);
        assert!(f.origins[0].breaker.available());
        let response = responses.pop().unwrap();
        let Transport::Connected(fixed, _) = &response.response.socket.as_ref().unwrap().transport
        else {
            panic!("unrecycled response must retain registration")
        };
        // A ring-owned operation, not the idle pool, now pins the last slot. Recycling
        // must not authorize its reuse before the target CQE, even after cancellation.
        let mut pending = f
            .local
            .recv_bytes(fixed.clone().into(), vec![0; 1].into_boxed_slice())
            .unwrap();
        f.local.progress().unwrap();
        f.origins[0].recycle(response.recycle());
        assert!(
            f.head(1).is_err(),
            "outstanding IO must retain fixed capability"
        );
        let cancel = f.local.cancel(&pending).unwrap();
        let end = Instant::now() + Duration::from_secs(3);
        loop {
            assert!(Instant::now() < end);
            if let Some(completion) = f.local.take_bytes(&mut pending).unwrap() {
                assert_eq!(
                    completion.result.unwrap_err().raw_os_error(),
                    Some(libc::ECANCELED)
                );
                break;
            }
            f.tick();
        }
        drop(cancel);
        f.head(1)
            .expect("recycle releases capacity while other responses remain pinned");
        drop(responses);
        f.finish();
        eprintln!(
            "B10 pinned: bounded LocalPressure, no origin hit/owner evidence, target CQE required before capacity recovery"
        );
    }
    fn ownership_and_faults(world: &World) {
        use crate::simulation::{Gate, Phase};
        let mut f = Fixture::new();
        f.head(0).unwrap();
        let (connection, permit) = f.origins[0].connection().unwrap();
        let mut exchange = connection
            .head(
                Request::new("/", &[]).unwrap(),
                world.now() + Duration::from_secs(2),
            )
            .unwrap();
        let before = world.counts();
        assert_eq!(
            exchange.poll(&mut f.remote, 64).err().unwrap().kind(),
            io::ErrorKind::InvalidInput
        );
        assert_eq!(world.counts(), before, "foreign ring must not submit IO");
        drop((exchange, permit));
        f.head(0).unwrap();
        let (connection, permit) = f.origins[0].connection().unwrap();
        let foreign = crate::buffers::io_test_pool(1);
        let fill = foreign.stage(crate::buffers::Key::new([99; 32])).unwrap();
        let mut get = connection
            .get(
                Request::new("/", &[]).unwrap(),
                fill,
                world.now() + Duration::from_secs(2),
            )
            .unwrap();
        assert_eq!(
            get.poll(&mut f.local, 64).err().unwrap().kind(),
            io::ErrorKind::InvalidInput
        );
        drop((get, permit));
        f.head(0).unwrap();
        world.node(Some(0));
        let gate = world.gate(
            Gate::new(
                0,
                f.origins[0].endpoint.address,
                "external:/",
                Phase::Registration,
                None,
            )
            .persistent(),
        );
        let hits = f.hits.get();
        let error = f.head(0).expect_err("injected warm registration pressure");
        assert!(
            world.hits(gate) > 1,
            "fault must actually intercept recycled registration"
        );
        assert_eq!(
            error
                .get_ref()
                .unwrap()
                .downcast_ref::<attempt::Failure>()
                .unwrap()
                .cause,
            attempt::Cause::LocalPressure
        );
        assert_eq!(f.hits.get(), hits);
        world.release(gate);
        f.head(0).unwrap();
        // Cancel a warm exchange while it waits for registration; no CONNECT/SEND.
        let gate = world.gate(
            Gate::new(
                0,
                f.origins[0].endpoint.address,
                "external:/",
                Phase::Registration,
                None,
            )
            .persistent(),
        );
        let (connection, permit) = f.origins[0].connection().unwrap();
        let mut exchange = connection
            .head(
                Request::new("/", &[]).unwrap(),
                world.now() + Duration::from_secs(2),
            )
            .unwrap();
        assert!(matches!(
            exchange.poll(&mut f.local, 64).unwrap(),
            Progress::Pending(_)
        ));
        assert!(world.hits(gate) > 0);
        let before = world.counts();
        exchange.cancel(&mut f.local).unwrap();
        assert_eq!(world.counts(), before);
        drop(permit);
        world.release(gate);
        f.head(0).unwrap();
        assert!(f.origins[0].breaker.available());
        f.finish();
        world.node(None);
    }
    fn run(replay: Option<Vec<Choice>>) -> ([u8; 32], Vec<Choice>) {
        let world = World::new(510);
        let _scope = world.enter();
        world.enable_scheduler();
        if let Some(replay) = replay {
            world.replay(replay);
        }
        pools();
        pinned();
        ownership_and_faults(&world);
        world.assert_clean();
        world.assert_replay_consumed();
        (world.digest(), world.choices())
    }
    #[test]
    fn b10_dst_idle_pools_cold_progress_strict_replay() {
        let (digest, choices) = run(None);
        assert!(!choices.is_empty());
        let (replayed, replay_choices) = run(Some(choices.clone()));
        assert_eq!(digest, replayed);
        assert_eq!(choices, replay_choices);
        eprintln!(
            "B10 strict replay: seed=510 choices={} digest={digest:02x?}, registration fault hits required",
            choices.len()
        );
    }

    #[test]
    fn b10_kernel_small_table_pools() {
        crate::http_server::cache_responses::kernel_child(
            "http_client::tests::idle_pressure::b10_kernel_child",
            "RACER_B10_CHILD",
        );
    }
    #[test]
    #[ignore = "bounded subprocess helper"]
    fn b10_kernel_child() {
        if std::env::var_os("RACER_B10_CHILD").is_none() {
            return;
        }
        pools();
        pinned();
    }
}

mod idle_close {
    use super::*;
    use crate::{
        buffers::Key,
        simulation::{Choice, World},
        uring::{Accept, Config},
    };
    use std::time::Duration;

    // Raw origin transport lets the test close an actually idle accepted socket,
    // independently of HTTP's Connection: close signalling.
    struct Fixture {
        local: Ring,
        remote: Ring,
        listener: File,
        accept: Option<Ticket<Accept>>,
        peer: Option<File>,
        recv: Option<Ticket<Bytes>>,
        send: Option<Ticket<Bytes>>,
        input: Vec<u8>,
        requests: Vec<Vec<u8>>,
        origin: Origin,
        reply: Vec<u8>,
        close_after_reply: bool,
        accepts: usize,
        reading: bool,
        retire_on_gate: Option<usize>,
    }
    impl Fixture {
        fn new() -> Self {
            let (listener, address) = if let Some(w) = crate::simulation::current() {
                let address = "127.0.0.1:18912".parse().unwrap();
                (File::simulated(w.listen(address).unwrap()), address)
            } else {
                let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
                let address = l.local_addr().unwrap();
                (File::new(l.into()), address)
            };
            Self {
                local: crate::conformance::ring(4, Config::default()),
                remote: crate::conformance::ring(4, Config::default()),
                listener,
                accept: None,
                peer: None,
                recv: None,
                send: None,
                input: Vec::new(),
                requests: Vec::new(),
                origin: Origin::new(Endpoint {
                    address,
                    host: "origin".into(),
                }),
                reply: b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: \"v1\"\r\n\r\n".to_vec(),
                close_after_reply: false,
                accepts: 0,
                reading: false,
                retire_on_gate: None,
            }
        }
        fn tick(&mut self) {
            if let Some(gate) = self.retire_on_gate
                && crate::simulation::current().unwrap().hits(gate) > 0
            {
                self.retire_on_gate = None;
                if let Some(t) = self.recv.take() {
                    drop(self.remote.cancel(&t).unwrap());
                }
                self.send.take();
                self.close(false);
            }
            let first = crate::simulation::current()
                .is_some_and(|w| w.choose_enabled("b11-ring-order", &[0, 1]) == 1);
            for remote in [first, !first] {
                if remote {
                    self.remote.progress().unwrap();
                    self.serve();
                } else {
                    self.local.progress().unwrap();
                }
            }
            if let Some(w) = crate::simulation::current() {
                w.service_tick();
            } else {
                std::thread::yield_now();
            }
        }
        fn serve(&mut self) {
            if let Some(t) = &mut self.send {
                let Some(c) = self.remote.take_bytes(t).unwrap() else {
                    return;
                };
                let n = c.result.unwrap();
                self.send = None;
                if n < c.resource.len() {
                    self.send = Some(
                        self.remote
                            .send_bytes(
                                self.peer.as_ref().unwrap().clone().into(),
                                c.resource[n..].into(),
                            )
                            .unwrap(),
                    );
                    return;
                }
                if self.close_after_reply {
                    self.close(false);
                }
            }
            if self.peer.is_none() {
                if self.accept.is_none() {
                    self.accept = Some(self.remote.accept(self.listener.clone().into()).unwrap());
                }
                if let Some(c) = self
                    .remote
                    .take_accept(self.accept.as_mut().unwrap())
                    .unwrap()
                {
                    c.result.unwrap();
                    self.peer = c.resource;
                    self.accept = None;
                    self.accepts += 1;
                } else {
                    return;
                }
            }
            // Do not read ahead: after a response the socket is genuinely idle.
            if self.recv.is_none() {
                return;
            }
            if let Some(c) = self.remote.take_bytes(self.recv.as_mut().unwrap()).unwrap() {
                self.recv = None;
                match c.result {
                    Ok(0) | Err(_) => {
                        self.close(false);
                        return;
                    }
                    Ok(n) => self.input.extend_from_slice(&c.resource[..n]),
                }
                if self.input.ends_with(b"\r\n\r\n") {
                    let mut reply = self.reply.clone();
                    if self.input.starts_with(b"GET") && !self.close_after_reply {
                        reply.extend_from_slice(b"abc");
                    }
                    self.requests.push(std::mem::take(&mut self.input));
                    self.reading = false;
                    self.send = Some(
                        self.remote
                            .send_bytes(self.peer.as_ref().unwrap().clone().into(), reply.into())
                            .unwrap(),
                    );
                } else {
                    self.read_request();
                }
            }
        }
        fn read_request(&mut self) {
            if self.reading
                && self.recv.is_none()
                && self.send.is_none()
                && let Some(peer) = &self.peer
            {
                self.recv = Some(
                    self.remote
                        .recv_bytes(peer.clone().into(), vec![0; 8192].into())
                        .unwrap(),
                );
            }
        }
        fn close(&mut self, reset: bool) {
            assert!(self.recv.is_none() && self.send.is_none());
            if let Some(peer) = self.peer.take() {
                if reset && crate::simulation::current().is_none() {
                    let linger = libc::linger {
                        l_onoff: 1,
                        l_linger: 0,
                    };
                    // SAFETY: live TCP descriptor and correctly sized linger option.
                    assert_eq!(
                        unsafe {
                            libc::setsockopt(
                                peer.as_fd().as_raw_fd(),
                                libc::SOL_SOCKET,
                                libc::SO_LINGER,
                                (&linger as *const libc::linger).cast(),
                                size_of_val(&linger) as _,
                            )
                        },
                        0
                    );
                } else {
                    peer.shutdown_socket();
                }
            }
            self.input.clear();
            self.reading = true;
        }
        fn drive<T>(
            &mut self,
            mut poll: impl FnMut(&mut Ring) -> io::Result<Progress<T>>,
        ) -> io::Result<T> {
            self.reading = true;
            let end = Instant::now() + Duration::from_secs(4);
            loop {
                assert!(Instant::now() < end, "B11 watchdog");
                match poll(&mut self.local)? {
                    Progress::Ready(r) => return Ok(r),
                    Progress::Pending(_) => {
                        self.tick();
                        self.read_request();
                    }
                }
            }
        }
        fn head(&mut self) -> io::Result<()> {
            let (c, permit) = self.origin.connection()?;
            let mut e = c
                .head(
                    Request::backend("/value", &[])?,
                    crate::environment::now() + Duration::from_secs(2),
                )?
                .retry_idle_backend(&Default::default());
            match self.drive(|r| e.poll(r, 1)) {
                Ok(response) => {
                    assert_eq!(response.status(), 200);
                    assert_eq!(response.content_length(), Some(3));
                    self.origin.recycle(response.recycle());
                    permit.success();
                    Ok(())
                }
                Err(error) => {
                    let error = crate::cache::Error::Io(error);
                    Origin::error(permit, &error, false);
                    let crate::cache::Error::Io(error) = error else {
                        unreachable!()
                    };
                    Err(error)
                }
            }
        }
        fn idle(&mut self) {
            let end = Instant::now() + Duration::from_secs(3);
            while self.send.is_some() {
                assert!(Instant::now() < end);
                self.tick();
            }
            assert!(self.recv.is_none());
            assert_eq!(self.origin.idle.len(), 1);
            assert!(matches!(
                self.origin.idle[0].socket.transport,
                Transport::Idle(_)
            ));
        }
        fn get(&mut self) -> io::Result<()> {
            let (c, permit) = self.origin.connection()?;
            let fill = self.local.pool().private_fill().unwrap();
            let (authority, destination) = fill.split_destination();
            let address = destination.region().region.address;
            let mut e = c
                .get(
                    Request::backend("/value", &[("Range", "bytes=0-2"), ("If-Match", "\"v1\"")])?,
                    destination,
                    crate::environment::now() + Duration::from_secs(2),
                )?
                .retry_idle_backend(&Default::default());
            let mut response = self.drive(|r| e.poll(r, 1))?;
            assert_eq!(response.status(), 200);
            assert_eq!(response.body(), b"abc");
            let (connection, destination, len) = response.recycle();
            assert_eq!(destination.region().region.address, address);
            assert_eq!(
                authority
                    .reunite(destination)
                    .ok()
                    .unwrap()
                    .publish(len)
                    .unwrap()
                    .as_slice(),
                b"abc"
            );
            self.origin.recycle(connection);
            permit.success();
            Ok(())
        }
        fn finish(mut self) {
            self.origin.idle.clear();
            if let Some(t) = self.recv.take() {
                drop(self.remote.cancel(&t).unwrap());
            }
            self.send.take();
            self.close(false);
            self.accept.take();
            self.local.shutdown().unwrap();
            self.remote.shutdown().unwrap();
            // All four writable slots can be reacquired after actual IO retirement.
            let fills: Vec<_> = (200..204)
                .map(|n| self.local.pool().stage(Key::new([n; 32])).unwrap())
                .collect();
            assert_eq!(fills.len(), 4);
        }
    }
    fn lifecycle(seed: u64, reset: bool, world: Option<&World>) {
        use crate::simulation::{Gate, Phase, corpus::Random};
        let mut random = Random(seed);
        let mut f = Fixture::new();
        f.head().unwrap();
        let mut connections = 1;
        // The prefix guarantees both methods at each fault seam; the suffix
        // varies reuse/close transitions on the same pool and breaker.
        for step in 0..24 {
            f.idle();
            let action = if step < 12 { step / 2 } else { random.index(6) };
            let get = if step < 12 {
                step % 2 != 0
            } else {
                random.index(2) == 0
            };
            let gate = if action >= 2 && world.is_some() {
                let w = world.unwrap();
                w.node(Some(0));
                let (phase, errno) = [
                    (Phase::Request, libc::EPIPE),
                    (Phase::Request, libc::ECONNRESET),
                    (Phase::Headers, 0),
                    (Phase::Headers, libc::ECONNRESET),
                ][action - 2];
                let id = w.gate(Gate::new(
                    0,
                    f.origin.endpoint.address,
                    "/value",
                    phase,
                    Some(errno),
                ));
                f.retire_on_gate = Some(id);
                Some(id)
            } else {
                if action != 0 {
                    f.close(reset);
                }
                None
            };
            connections += usize::from(action != 0);
            let requests = f.requests.len();
            if get { f.get() } else { f.head() }.unwrap();
            if let Some(gate) = gate {
                assert_eq!(world.unwrap().hits(gate), 1);
            }
            assert_eq!(f.accepts, connections);
            let maximum = if gate.is_some() && action >= 4 { 2 } else { 1 };
            assert!((1..=maximum).contains(&(f.requests.len() - requests)));
            assert!(f.origin.breaker.available());
            assert_eq!(f.origin.breaker.active(), 0);
            for request in &f.requests[requests..] {
                if get {
                    assert_eq!(request, b"GET /value HTTP/1.1\r\nHost: origin\r\nRange: bytes=0-2\r\nIf-Match: \"v1\"\r\n\r\n");
                } else {
                    assert_eq!(request, &f.requests[0]);
                }
            }
        }
        f.finish();
        if let Some(w) = world {
            w.node(None);
        }
    }
    fn fault_cases(w: &World) {
        use crate::simulation::{Gate, Phase};
        // Persistent errors on a reused connection get one replacement only; a
        // never-used connection gets none. Assert actual CONNECT and fault counts.
        for warm in [false, true] {
            let mut f = Fixture::new();
            if warm {
                f.head().unwrap();
                f.idle();
            }
            w.node(Some(0));
            let gate = w.gate(
                Gate::new(
                    0,
                    f.origin.endpoint.address,
                    "/value",
                    Phase::Request,
                    Some(libc::EPIPE),
                )
                .persistent(),
            );
            f.retire_on_gate = Some(gate);
            let before = w.counts()[crate::uring_sys::abi::CONNECT as usize];
            assert_eq!(f.head().unwrap_err().kind(), io::ErrorKind::BrokenPipe);
            assert_eq!(w.hits(gate), if warm { 2 } else { 1 });
            assert_eq!(
                w.counts()[crate::uring_sys::abi::CONNECT as usize] - before,
                1
            );
            assert!(!f.origin.breaker.available());
            assert_eq!(f.origin.breaker.active(), 0);
            assert!(f.origin.idle.is_empty());
            w.release(gate);
            f.finish();
            w.node(None);
        }
    }
    fn exclusions() {
        for reply in [
            b"HTTP/1.1 200".as_slice(),
            b"HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\na",
            b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n",
            b"HTTP/1.1 503 Unavailable\r\nContent-Length: 0\r\n\r\n",
            b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n",
            b"HTTP/1.1 100 Continue\r\n\r\n",
        ] {
            let mut f = Fixture::new();
            f.head().unwrap();
            f.idle();
            f.reply = reply.to_vec();
            f.close_after_reply = true;
            let (c, permit) = f.origin.connection().unwrap();
            let fill = f.local.pool().stage(Key::new([91; 32])).unwrap();
            let mut e = c
                .get(
                    Request::new("/page", &[]).unwrap(),
                    fill,
                    crate::environment::now() + Duration::from_secs(2),
                )
                .unwrap()
                .retry_idle_backend(&Default::default());
            let result = f.drive(|r| e.poll(r, 1));
            if reply.starts_with(b"HTTP/1.1 503") || reply.starts_with(b"HTTP/1.1 401") {
                let mut response = result.unwrap();
                assert_eq!(response.status(), if reply[9] == b'5' { 503 } else { 401 });
                assert!(response.body().is_empty());
            } else {
                let error = result
                    .err()
                    .expect("partial/protocol response must fail without replay");
                assert_eq!(
                    error.kind(),
                    if reply.windows(17).any(|s| s == b"Transfer-Encoding") {
                        io::ErrorKind::InvalidData
                    } else {
                        io::ErrorKind::UnexpectedEof
                    }
                );
            }
            assert_eq!(f.accepts, 1);
            assert_eq!(f.requests.len(), 2);
            drop((e, permit));
            f.finish();
        }
    }
    fn deadlines_cancel(w: &World) {
        use crate::simulation::{Gate, Phase};
        for phase in [
            Phase::Connect,
            Phase::Registration,
            Phase::Request,
            Phase::Headers,
        ] {
            for cancel in [false, true] {
                let mut f = Fixture::new();
                f.head().unwrap();
                f.idle();
                f.close(false);
                w.node(Some(0));
                let (c, permit) = f.origin.connection().unwrap();
                let fill = f.local.pool().stage(Key::new([92; 32])).unwrap();
                let (authority, destination) = fill.split_destination();
                let end = w.now() + Duration::from_millis(80);
                let mut e = c
                    .get(Request::backend("/value", &[]).unwrap(), destination, end)
                    .unwrap()
                    .retry_idle_backend(&Default::default());
                // Consume some of the original budget before encountering stale TCP.
                w.advance(Duration::from_millis(50));
                let watchdog = Instant::now() + Duration::from_secs(3);
                while !matches!(e.0.state, State::Connect(_)) {
                    assert!(Instant::now() < watchdog);
                    assert!(matches!(
                        e.poll(&mut f.local, 1).unwrap(),
                        Progress::Pending(_)
                    ));
                    f.tick();
                }
                assert_eq!(e.0.deadline, end);
                let gate = w.gate(
                    Gate::new(0, f.origin.endpoint.address, "/value", phase, None).persistent(),
                );
                f.reading = true;
                while w.hits(gate) == 0 {
                    assert!(Instant::now() < watchdog);
                    assert!(matches!(
                        e.poll(&mut f.local, 1).unwrap(),
                        Progress::Pending(_)
                    ));
                    f.tick();
                    f.read_request();
                }
                if cancel {
                    e.cancel(&mut f.local).unwrap();
                } else {
                    let error = f.drive(|r| e.poll(r, 1)).err().unwrap();
                    assert_eq!(error.kind(), io::ErrorKind::TimedOut);
                    assert_eq!(w.now(), end, "replacement must not restart deadline");
                    drop(e);
                }
                drop((authority, permit));
                assert!(f.origin.breaker.available());
                assert_eq!(f.origin.breaker.active(), 0);
                w.release(gate);
                f.finish();
                w.node(None);
            }
        }
        // Peer requests/default public API never opt in, even for recycled sockets.
        let mut f = Fixture::new();
        f.head().unwrap();
        f.idle();
        f.close(false);
        let (c, permit) = f.origin.connection().unwrap();
        let before = w.counts()[crate::uring_sys::abi::CONNECT as usize];
        let mut e = c
            .head(
                Request::new("/peer", &[("X-Racer-Attempt", "never-replay")]).unwrap(),
                w.now() + Duration::from_secs(1),
            )
            .unwrap();
        assert!(f.drive(|r| e.poll(r, 1)).is_err());
        assert_eq!(w.counts()[crate::uring_sys::abi::CONNECT as usize], before);
        drop((e, permit));
        f.finish();
    }
    fn boundaries(w: &World) {
        use crate::simulation::{Gate, Phase};
        // Replacement CONNECT failure is terminal, without a third connection.
        let mut f = Fixture::new();
        f.head().unwrap();
        f.idle();
        f.close(false);
        w.node(Some(0));
        let gate = w.gate(
            Gate::new(
                0,
                f.origin.endpoint.address,
                "/value",
                Phase::Connect,
                Some(libc::ECONNREFUSED),
            )
            .persistent(),
        );
        let before = w.counts()[crate::uring_sys::abi::CONNECT as usize];
        assert_eq!(
            f.head().unwrap_err().kind(),
            io::ErrorKind::ConnectionRefused
        );
        assert_eq!(w.hits(gate), 1);
        assert_eq!(
            w.counts()[crate::uring_sys::abi::CONNECT as usize] - before,
            1
        );
        w.release(gate);
        f.finish();
        w.node(None);

        // Retry eligibility cannot bypass initial ring/storage validation.
        for foreign_storage in [false, true] {
            let mut f = Fixture::new();
            f.head().unwrap();
            f.idle();
            f.close(false);
            let (c, permit) = f.origin.connection().unwrap();
            let pool = if foreign_storage {
                f.remote.pool()
            } else {
                f.local.pool()
            };
            let fill = pool.stage(Key::new([93; 32])).unwrap();
            let mut e = c
                .get(
                    Request::new("/page", &[]).unwrap(),
                    fill,
                    w.now() + Duration::from_secs(1),
                )
                .unwrap()
                .retry_idle_backend(&Default::default());
            let before = w.counts();
            let ring = if foreign_storage {
                &mut f.local
            } else {
                &mut f.remote
            };
            assert_eq!(
                e.poll(ring, 64).err().unwrap().kind(),
                io::ErrorKind::InvalidInput
            );
            assert_eq!(w.counts(), before);
            drop((e, permit));
            f.finish();
        }
        // Expiration wins over an already available stale-socket completion.
        for service in [false, true] {
            let mut f = Fixture::new();
            f.head().unwrap();
            f.idle();
            f.close(false);
            let (c, permit) = f.origin.connection().unwrap();
            let end = w.now() + Duration::from_millis(10);
            let mut e = c
                .head(Request::new("/metadata", &[]).unwrap(), end)
                .unwrap()
                .service_deadline(service)
                .retry_idle_backend(&Default::default());
            assert!(matches!(
                e.poll(&mut f.local, 64).unwrap(),
                Progress::Pending(_)
            ));
            f.local.progress().unwrap();
            w.advance(Duration::from_millis(10));
            let before = w.counts()[crate::uring_sys::abi::CONNECT as usize];
            let error = e.poll(&mut f.local, 64).err().unwrap();
            let failure = error
                .get_ref()
                .unwrap()
                .downcast_ref::<attempt::Failure>()
                .unwrap();
            assert_eq!(
                failure.cause,
                if service {
                    attempt::Cause::ServiceTimeout
                } else {
                    attempt::Cause::CallerDeadline
                }
            );
            assert_eq!(w.counts()[crate::uring_sys::abi::CONNECT as usize], before);
            drop((e, permit));
            f.finish();
        }
        // Partial request SEND followed by reset must restart at byte zero on the
        // fresh socket, retaining exact range/precondition bytes and Destination.
        let mut f = Fixture::new();
        f.head().unwrap();
        f.idle();
        w.node(Some(0));
        w.short_transfers(7);
        let (c, permit) = f.origin.connection().unwrap();
        let fill = f.local.pool().stage(Key::new([94; 32])).unwrap();
        let mut e = c
            .get(
                Request::backend("/value", &[("Range", "bytes=0-2")]).unwrap(),
                fill,
                w.now() + Duration::from_secs(2),
            )
            .unwrap()
            .retry_idle_backend(&Default::default());
        let end = Instant::now() + Duration::from_secs(3);
        while !matches!(e.0.state, State::Send(_, n) if n > 0) {
            assert!(Instant::now() < end);
            assert!(matches!(
                e.poll(&mut f.local, 1).unwrap(),
                Progress::Pending(_)
            ));
            f.tick();
        }
        let gate = w.gate(Gate::new(
            0,
            f.origin.endpoint.address,
            "/value",
            Phase::Request,
            Some(libc::ECONNRESET),
        ));
        f.retire_on_gate = Some(gate);
        let mut response = f.drive(|r| e.poll(r, 1)).unwrap();
        assert_eq!(response.body(), b"abc");
        assert_eq!(w.hits(gate), 1);
        assert_eq!(
            f.requests[1],
            b"GET /value HTTP/1.1\r\nHost: origin\r\nRange: bytes=0-2\r\n\r\n"
        );
        assert_eq!(f.accepts, 2);
        drop((response, e));
        permit.success();
        f.finish();
        w.short_transfers(usize::MAX);
        w.node(None);
    }
    fn run(replay: Option<Vec<Choice>>) -> ([u8; 32], Vec<Choice>) {
        let w = World::new(511);
        let _scope = w.enter();
        w.enable_scheduler();
        if let Some(choices) = replay {
            w.replay(choices);
        }
        lifecycle(511, false, Some(&w));
        fault_cases(&w);
        exclusions();
        deadlines_cancel(&w);
        boundaries(&w);
        w.assert_clean();
        w.assert_replay_consumed();
        (w.digest(), w.choices())
    }
    #[test]
    fn b11_kernel_idle_fin_rst_get_head() {
        crate::http_server::cache_responses::kernel_child(
            "http_client::tests::idle_close::b11_kernel_child",
            "RACER_B11_CHILD",
        );
    }
    #[test]
    #[ignore = "bounded subprocess helper"]
    fn b11_kernel_child() {
        if std::env::var_os("RACER_B11_CHILD").is_none() {
            return;
        }
        for reset in [false, true] {
            lifecycle(511, reset, None);
        }
        exclusions();
    }
    #[test]
    fn b11_dst_idle_close_strict_replay() {
        let (digest, choices) = run(None);
        assert!(!choices.is_empty());
        let (again, replay) = run(Some(choices.clone()));
        assert_eq!(again, digest);
        assert_eq!(replay, choices);
        eprintln!(
            "B11 seed=511 choices={} digest={digest:02x?}",
            choices.len()
        );
    }
}
