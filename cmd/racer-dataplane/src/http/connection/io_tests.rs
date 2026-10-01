//! Real head/body exchanges, parser boundaries, zeroization, and ownership fences.
use super::*;
use crate::{
    http::{Codec, Header},
    model::RequestId,
};
use std::{
    future::Future,
    io::{Read, Write},
    net::TcpListener,
    os::unix::net::UnixStream,
    task::{Context, Poll},
    time::{Duration, Instant},
};

fn setup() -> (Rc<Admission>, Rc<Reactor>, HttpIo, RequestScope) {
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = HttpIo::with_admission(
        reactor.clone(),
        Codec::new(4096, 8 * 1024 * 1024),
        admission.clone(),
    );
    let scope =
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(10)).unwrap();
    (admission, reactor, io, scope)
}
pub(crate) fn drive<T>(reactor: &Reactor, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(12);
    loop {
        if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
            return result;
        }
        assert!(
            Instant::now() < deadline,
            "HTTP operation failed to make progress"
        );
        // Repoll completed work before sleeping: a drained CQ has no event left.
        if reactor.poll_budgeted(128).unwrap() == 0 {
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
}
fn request(method: &str) -> MessageHead {
    MessageHead {
        start: StartLine::Request {
            method: method.into(),
            target: "/test".into(),
        },
        headers: vec![Header {
            name: "Host".into(),
            value: b"localhost".to_vec(),
        }],
    }
}
fn response(length: usize) -> MessageHead {
    MessageHead {
        start: StartLine::Response { status: 200 },
        headers: vec![Header {
            name: "Content-Length".into(),
            value: length.to_string().into_bytes(),
        }],
    }
}
fn drain(reactor: &Reactor) {
    drive(reactor, reactor.drain()).unwrap();
    assert_eq!(reactor.in_flight(), 0);
}

#[test]
fn small_peer_send_stages_actual_head_under_context_pressure() {
    let (admission, reactor, _, scope) = setup();
    reactor.init().unwrap();
    let limit = crate::peer::protocol::MAX_ENVELOPE_HEAD;
    let io = HttpIo::with_admission(reactor.clone(), Codec::new(limit, 16), admission.clone());
    let baseline = admission.used(ResourceClass::RequestContext);
    let held = admission
        .reserve(
            None,
            ResourceClass::RequestContext,
            admission.limit(ResourceClass::RequestContext) - baseline - limit - 4096,
        )
        .unwrap();
    let head = response(0);
    let expected = io.codec.encode_head(&head).unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let result = drive(&reactor, io.send_head(connection, head, &scope)).unwrap();
    let mut received = vec![0; expected.len()];
    peer.read_exact(&mut received).unwrap();
    assert_eq!(received, expected);
    drop(result);
    drop(held);
    io.reclaim_buffer();
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn staging_recycles_zeroed_backing_with_retained_admission() {
    let (admission, _, io, _) = setup();
    let baseline = admission.used(ResourceClass::RequestContext);
    let mut buffer = io.buffer(4096).unwrap();
    let pointer = buffer.bytes().unwrap().as_ptr();
    buffer.bytes_mut().unwrap().fill(91);
    drop(buffer);
    assert_eq!(io.retained_buffer_bytes(), 4096);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + 4096
    );
    let buffer = io.buffer(4096).unwrap();
    assert_eq!(buffer.bytes().unwrap().as_ptr(), pointer);
    assert!(buffer.bytes().unwrap().iter().all(|b| *b == 0));
    drop(buffer);
    io.reclaim_buffer();
    assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
}
#[test]
fn decoded_field_storage_is_admitted_before_parser_allocations() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let held = admission
        .reserve(
            None,
            ResourceClass::RequestContext,
            admission.limit(ResourceClass::RequestContext) - baseline - 8192,
        )
        .unwrap();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let mut head = b"GET / HTTP/1.1\r\n".to_vec();
    for _ in 0..500 {
        head.extend_from_slice(b"X:\r\n");
    }
    head.extend_from_slice(b"\r\n");
    peer.write_all(&head).unwrap();
    assert!(matches!(
        drive(&reactor, io.receive_head(connection, &scope)),
        Err(Error::Overloaded)
    ));
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    drop(held);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
}
#[test]
fn client_constructor_streams_beyond_page_cap_with_bounded_staging() {
    let (admission, reactor, _, scope) = setup();
    let io = HttpIo::for_clients(reactor.clone(), admission.clone()).capped(4096);
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"GET /test HTTP/1.1\r\nHost: localhost\r\n\r\n")
        .unwrap();
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    let length = 2 * crate::model::PAGE_BYTES as usize + 113;
    let thread = std::thread::spawn(move || {
        peer.set_read_timeout(Some(Duration::from_secs(10)))
            .unwrap();
        let mut head = Vec::new();
        while !head.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            peer.read_exact(&mut byte).unwrap();
            head.push(byte[0]);
        }
        assert_eq!(
            head,
            format!("HTTP/1.1 200 \r\nContent-Length: {length}\r\n\r\n").as_bytes()
        );
        let mut chunk = [0; 8192];
        let mut remaining = length;
        while remaining != 0 {
            let count = remaining.min(chunk.len());
            peer.read_exact(&mut chunk[..count]).unwrap();
            assert!(chunk[..count].iter().all(|byte| *byte == 91));
            remaining -= count;
        }
    });
    let mut connection = drive(
        &reactor,
        io.send_head(received.connection, response(length), &scope),
    )
    .unwrap()
    .connection;
    drop(received.value);
    drop(received._decoded);
    assert_eq!(connection.finish_exchange(), Err(Error::InvalidRequest));
    let mut buffer = io.buffer(8192).unwrap();
    buffer.bytes_mut().unwrap().fill(91);
    let mut remaining = length;
    while remaining != 0 {
        let count = remaining.min(8192);
        let completed = drive(
            &reactor,
            io.write_body_range(connection, buffer, 0..count, &scope),
        )
        .unwrap();
        assert_eq!(completed.bytes, count);
        buffer = completed.buffer;
        connection = completed.lease;
        remaining -= count;
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + 8192
        );
    }
    connection.finish_exchange().unwrap();
    assert!(connection.is_reusable());
    assert!(matches!(
        drive(
            &reactor,
            io.write_body_range(connection, buffer, 0..1, &scope)
        ),
        Err(Error::InvalidRequest)
    ));
    thread.join().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn client_send_limit_does_not_relax_receive_or_page_transport_limits() {
    let (admission, reactor, _, scope) = setup();
    let page_limit = crate::model::PAGE_BYTES + 16;
    let page_io = HttpIo::with_admission(
        reactor.clone(),
        Codec::new(4096, page_limit),
        admission.clone(),
    );
    let client_io = HttpIo::for_clients(reactor.clone(), admission.clone()).capped(4096);
    for io in [&page_io, &client_io] {
        assert_eq!(
            io.framing(&response(page_limit as usize), false),
            Ok(page_limit)
        );
        for start in ["HTTP/1.1 200 OK", "GET /test HTTP/1.1"] {
            let (socket, mut peer) = UnixStream::pair().unwrap();
            let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
            write!(
                peer,
                "{start}\r\nContent-Length: {}\r\n\r\n",
                page_limit + 1
            )
            .unwrap();
            assert!(matches!(
                drive(&reactor, io.receive_head(connection, &scope)),
                Err(Error::InvalidRequest)
            ));
        }
    }
    for (io, length) in [
        (&page_io, page_limit + 1),
        (&client_io, i64::MAX as u64 + 1),
    ] {
        let (socket, _peer) = UnixStream::pair().unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        assert!(matches!(
            drive(
                &reactor,
                io.send_head(connection, response(length as usize), &scope)
            ),
            Err(Error::InvalidRequest)
        ));
    }
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn endpoint_caps_accept_exact_boundary_and_reject_one_extra_byte() {
    let (admission, reactor, _, scope) = setup();
    let peer = HttpIo::with_admission(
        reactor.clone(),
        Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD, 16),
        admission.clone(),
    );
    let client = HttpIo::for_clients(reactor.clone(), admission.clone());
    let origin = peer.capped(crate::http::MAX_HEAD_BYTES);
    for (io, limit) in [
        (&client, admission.limits().header_bytes.get()),
        (&origin, crate::http::MAX_HEAD_BYTES),
        (&peer, crate::peer::protocol::MAX_ENVELOPE_HEAD),
    ] {
        for extra in [0, 1] {
            let mut head = request("GET");
            let overhead = Codec::new(usize::MAX, 16).encode_head(&head).unwrap().len();
            let value_length = head.headers[0].value.len() + limit - overhead + extra;
            head.headers[0].value.resize(value_length, b'x');
            let raw = Codec::new(limit + 1, 16).encode_head(&head).unwrap();
            assert_eq!(raw.len(), limit + extra);
            let (socket, mut other) = UnixStream::pair().unwrap();
            let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
            let writer = std::thread::spawn(move || {
                let _ = other.write_all(&raw);
            });
            let result = drive(&reactor, io.receive_head(connection, &scope));
            if extra == 0 {
                assert!(result.is_ok());
            } else {
                assert!(matches!(result, Err(Error::HeaderTooLarge)));
            }
            drop(result);
            writer.join().unwrap();
            let (socket, mut other) = UnixStream::pair().unwrap();
            let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
            let reader = std::thread::spawn(move || {
                let mut received = Vec::new();
                other.read_to_end(&mut received).unwrap();
                received.len()
            });
            let result = drive(&reactor, io.send_head(connection, head, &scope));
            if extra == 0 {
                assert!(result.is_ok());
            } else {
                assert!(matches!(result, Err(Error::HeaderTooLarge)));
            }
            drop(result);
            assert_eq!(reader.join().unwrap(), if extra == 0 { limit } else { 0 });
        }
    }
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn real_socket_fragmentation_read_ahead_and_owned_ranges() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let thread = std::thread::spawn(move || {
        peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Len").unwrap();
        std::thread::sleep(Duration::from_millis(5));
        peer.write_all(b"gth: 5\r\n\r\nhello").unwrap();
    });
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    let mut bytes = io.buffer(9).unwrap();
    bytes.bytes_mut().unwrap().fill(b'_');
    let mut connection = received.connection;
    let mut offset = 2;
    while offset < 7 {
        let completed = drive(
            &reactor,
            io.read_body_range(connection, bytes, offset..7, &scope),
        )
        .unwrap();
        offset += completed.bytes;
        bytes = completed.buffer;
        connection = completed.lease;
    }
    assert_eq!(bytes.bytes().unwrap(), b"__hello__");
    assert_eq!(connection.remaining_body(), Some(0));
    drop(connection);
    drop(bytes);
    drop(received.value);
    drop(received._decoded);
    thread.join().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn real_partial_sends_preserve_owned_subrange() {
    let (admission, reactor, io, scope) = setup();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    use std::os::fd::AsRawFd;
    let size: libc::c_int = 4096;
    // SAFETY: setsockopt synchronously reads this correctly sized integer.
    assert_eq!(
        unsafe {
            libc::setsockopt(
                socket.as_raw_fd(),
                libc::SOL_SOCKET,
                libc::SO_SNDBUF,
                (&size as *const libc::c_int).cast(),
                std::mem::size_of_val(&size) as _,
            )
        },
        0
    );
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let length = 1024 * 1024;
    let thread = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(10));
        let mut received = Vec::new();
        peer.read_to_end(&mut received).unwrap();
        let start = received.windows(4).position(|w| w == b"\r\n\r\n").unwrap() + 4;
        assert_eq!(received.len() - start, length);
        assert!(received[start..].iter().all(|b| *b == 91));
    });
    let sent = drive(&reactor, io.send_head(connection, response(length), &scope)).unwrap();
    let mut bytes = io.buffer(length + 4).unwrap();
    bytes.bytes_mut().unwrap()[2..length + 2].fill(91);
    let completed = drive(
        &reactor,
        io.write_body_range(sent.connection, bytes, 2..length + 2, &scope),
    )
    .unwrap();
    assert_eq!(completed.bytes, length);
    drop(completed);
    drain(&reactor);
    thread.join().unwrap();
}
#[test]
fn tcp_pool_reuses_only_finished_exchanges_and_enforces_capacity() {
    let (admission, reactor, io, scope) = setup();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
    let thread = std::thread::spawn(move || {
        let (mut socket, _) = listener.accept().unwrap();
        for _ in 0..2 {
            let mut bytes = Vec::new();
            while !bytes.ends_with(b"\r\n\r\n") {
                let mut byte = [0];
                socket.read_exact(&mut byte).unwrap();
                bytes.push(byte[0]);
            }
            socket
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
                .unwrap();
        }
    });
    for _ in 0..2 {
        let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
        assert!(matches!(
            drive(&reactor, pool.checkout(&endpoint, &scope)),
            Err(Error::Overloaded)
        ));
        let received = drive(
            &reactor,
            io.exchange_head(connection, request("GET"), &scope),
        )
        .unwrap();
        let mut completed =
            drive(&reactor, io.collect_body(received.connection, 2, &scope)).unwrap();
        assert_eq!(completed.buffer.bytes().unwrap(), b"ok");
        completed.lease.finish_exchange().unwrap();
        assert!(completed.lease.is_reusable());
        drop(completed);
    }
    thread.join().unwrap();
    pool.close();
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn head_has_no_body_even_with_large_representation_length() {
    let (admission, reactor, io, scope) = setup();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let thread = std::thread::spawn(move || {
        let mut request = Vec::new();
        while !request.ends_with(b"\r\n\r\n") {
            let mut byte = [0];
            peer.read_exact(&mut byte).unwrap();
            request.push(byte[0]);
        }
        peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 999999999\r\n\r\n")
            .unwrap();
    });
    let mut received = drive(
        &reactor,
        io.exchange_head(connection, request("HEAD"), &scope),
    )
    .unwrap();
    assert_eq!(received.connection.remaining_body(), Some(0));
    received.connection.finish_exchange().unwrap();
    thread.join().unwrap();
}
#[test]
fn truncation_and_future_drop_do_not_recycle_live_buffers() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 9\r\n\r\nshort")
        .unwrap();
    drop(peer);
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    assert!(matches!(
        drive(&reactor, io.collect_body(received.connection, 9, &scope)),
        Err(Error::Io)
    ));
    drop(received.value);
    drop(received._decoded);
    let (socket, _peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let mut future = io.receive_head(connection, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    scope.cancel().unwrap();
    drain(&reactor);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn deadline_expires_without_peer_traffic() {
    let (admission, reactor, io, _) = setup();
    let scope = RequestScope::new(
        RequestId([2; 16]),
        Instant::now() + Duration::from_millis(30),
    )
    .unwrap();
    let (socket, _peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    assert!(matches!(
        drive(&reactor, io.receive_head(connection, &scope)),
        Err(Error::DeadlineExceeded)
    ));
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn unix_pool_reconnects_after_unread_response_and_bounds_endpoints() {
    use std::os::unix::net::UnixListener;
    let (admission, reactor, io, scope) = setup();
    let mut nonce = [0; 8];
    getrandom::getrandom(&mut nonce).unwrap();
    let path = std::path::PathBuf::from(format!(
        "racer-http-{}-{}.sock",
        std::process::id(),
        u64::from_ne_bytes(nonce)
    ));
    let listener = UnixListener::bind(&path).unwrap();
    let endpoint = Endpoint::Unix(path.clone());
    let pool = HttpPool::with_limits(reactor.clone(), admission.clone(), 1, 1, Duration::ZERO);
    let thread = std::thread::spawn(move || {
        for _ in 0..2 {
            let (mut socket, _) = listener.accept().unwrap();
            let _ = socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\ndata");
            let mut sink = Vec::new();
            let _ = socket.read_to_end(&mut sink);
        }
    });
    let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
    let mut received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    assert_eq!(
        received.connection.finish_exchange(),
        Err(Error::InvalidRequest)
    );
    drop(received);
    let connection = drive(&reactor, pool.checkout(&endpoint, &scope)).unwrap();
    let other = Endpoint::Peer("127.0.0.1:1".into());
    assert!(matches!(
        drive(&reactor, pool.checkout(&other, &scope)),
        Err(Error::Overloaded)
    ));
    pool.invalidate(&endpoint);
    drop(connection);
    pool.expire_idle();
    pool.close();
    thread.join().unwrap();
    std::fs::remove_file(path).unwrap();
    assert!(matches!(
        drive(&reactor, pool.checkout(&endpoint, &scope)),
        Err(Error::Unavailable)
    ));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn read_ahead_erases_credentials_and_endpoint_limit_is_enforced() {
    let (admission, reactor, io, scope) = setup();
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"POST / HTTP/1.1\r\nAuthorization: secret\r\nContent-Length: 4\r\n\r\nbody")
        .unwrap();
    let received = drive(&reactor, io.receive_head(connection, &scope)).unwrap();
    let (ahead, range) = received
        .connection
        .read_ahead
        .as_ref()
        .expect("socket supplied head and body together");
    assert!(ahead.bytes[..range.start].iter().all(|b| *b == 0));
    assert_eq!(&ahead.bytes[range.clone()], b"body");
    drop(received);
    let (socket, mut peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    peer.write_all(b"GET / HTTP/1.1\r\nAuthorization: too-large\r\n\r\n")
        .unwrap();
    assert!(matches!(
        drive(&reactor, io.receive_head_limited(connection, &scope, 20)),
        Err(Error::HeaderTooLarge)
    ));
}
#[test]
fn bodyless_and_malformed_response_framing_is_explicit() {
    let (_, _, io, _) = setup();
    assert_eq!(io.framing(&response(usize::MAX), true), Ok(0));
    assert!(io.framing(&response(usize::MAX), false).is_err());
    assert_eq!(
        io.framing(
            &MessageHead {
                start: StartLine::Response { status: 204 },
                headers: vec![]
            },
            false
        ),
        Ok(0)
    );
    assert!(
        io.framing(
            &MessageHead {
                start: StartLine::Response { status: 200 },
                headers: vec![]
            },
            false
        )
        .is_err()
    );
    assert!(
        io.framing(
            &MessageHead {
                start: StartLine::Response { status: 101 },
                headers: vec![]
            },
            false
        )
        .is_err()
    );
}
#[test]
fn dropped_connect_retains_slot_until_fd_fence_then_releases_quota() {
    let (admission, reactor, _, scope) = setup();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
    let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
    let mut future = pool.checkout(&endpoint, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    drop(future);
    assert_eq!(admission.used(ResourceClass::Connection), 1);
    assert!(matches!(
        drive(&reactor, pool.checkout(&endpoint, &scope)),
        Err(Error::Overloaded)
    ));
    drain(&reactor);
    assert_eq!(admission.used(ResourceClass::Connection), 0);
}
#[test]
fn dropped_checkout_and_destroyed_pool_release_quota_only_after_connect_fence() {
    for (cancel_before_drop, submit_before_drop) in [(false, false), (true, false), (true, true)] {
        let (admission, reactor, _, scope) = setup();
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let endpoint = Endpoint::Peer(listener.local_addr().unwrap().to_string());
        let pool = HttpPool::new(reactor.clone(), admission.clone(), 1);
        let mut future = pool.checkout(&endpoint, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 1);
        if submit_before_drop {
            reactor.poll_budgeted(32).unwrap();
        }
        if cancel_before_drop {
            scope.cancel().unwrap();
        }
        drop(future);
        drop(pool);
        assert_eq!(admission.used(ResourceClass::Connection), 1);
        assert_eq!(reactor.in_flight(), 1);
        drain(&reactor);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
        // No pool sweep exists: prove admission regained the charge rather than leaking it.
        let charge = admission
            .reserve(
                None,
                ResourceClass::Connection,
                admission.limits().client_connections.get(),
            )
            .unwrap();
        drop(charge);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}
#[test]
fn canceled_receive_retains_resources_until_completion_and_reports_canceled() {
    let (admission, reactor, io, scope) = setup();
    reactor.init().unwrap();
    let baseline = admission.used(ResourceClass::RequestContext);
    let (socket, _peer) = UnixStream::pair().unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let mut future = io.receive_head(connection, &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(future.as_mut().poll(&mut cx).is_pending());
    scope.cancel().unwrap();
    assert_eq!(admission.used(ResourceClass::Connection), 1);
    assert!(matches!(drive(&reactor, future), Err(Error::Cancelled)));
    assert_eq!(admission.used(ResourceClass::Connection), 0);
    assert_eq!(
        admission.used(ResourceClass::RequestContext),
        baseline + io.retained_buffer_bytes()
    );
}
#[test]
fn raw_request_head_errors_return_fenced_socket_for_empty_400_and_431() {
    for oversized in [false, true] {
        let (admission, reactor, _, scope) = setup();
        let io = HttpIo::with_admission(
            reactor.clone(),
            Codec::new(32 * 1024, 1024),
            admission.clone(),
        );
        reactor.init().unwrap();
        let baseline = admission.used(ResourceClass::RequestContext);
        let (socket, mut peer) = UnixStream::pair().unwrap();
        peer.set_read_timeout(Some(Duration::from_secs(2))).unwrap();
        let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
        let raw = if oversized {
            let mut raw = b"GET / HTTP/1.1\r\nAuthorization: ".to_vec();
            raw.resize(crate::http::MAX_HEAD_BYTES, b'x');
            raw
        } else {
            b"GET / HTTP/1.1\r\nAuthorization:secret\r\nContent-Length: 4\r\n\r\nbody".to_vec()
        };
        peer.write_all(&raw).unwrap();
        let expected = if oversized {
            Error::HeaderTooLarge
        } else {
            Error::InvalidRequest
        };
        let mut outcome = drive(
            &reactor,
            io.receive_request_head_limited(connection, &scope, 32 * 1024),
        )
        .unwrap();
        assert!(matches!(outcome.value, Err(error) if error == expected));
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(
            admission.used(ResourceClass::RequestContext),
            baseline + io.retained_buffer_bytes()
        );
        assert!(outcome.connection.read_ahead.is_none());
        assert!(!outcome.connection.is_reusable());
        assert_eq!(
            outcome.connection.finish_exchange(),
            Err(Error::InvalidRequest)
        );
        let status = if oversized { 431 } else { 400 };
        let mut head = response(0);
        head.start = StartLine::Response { status };
        head.headers.push(Header {
            name: "Connection".into(),
            value: b"close".to_vec(),
        });
        let mut sent = drive(&reactor, io.send_head(outcome.connection, head, &scope)).unwrap();
        assert!(!sent.connection.is_reusable());
        assert_eq!(
            sent.connection.finish_exchange(),
            Err(Error::InvalidRequest)
        );
        let expected =
            format!("HTTP/1.1 {status} \r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
        let mut wire = vec![0; expected.len()];
        peer.read_exact(&mut wire).unwrap();
        assert_eq!(wire, expected.as_bytes());
        drop(sent);
        assert_eq!(admission.used(ResourceClass::Connection), 0);
    }
}
