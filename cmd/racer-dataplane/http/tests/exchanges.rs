//! Consumer workflows using only public APIs and real, unprivileged socket pairs.

use http1::{
    Codec, Error, Header, MessageHead, StartLine,
    connection::{ConnectionLease, Context, Endpoint, HttpIo, OwnedBuffer},
};
use std::{
    future::Future,
    os::unix::net::UnixStream,
    rc::Rc,
    task::{Poll, Waker},
    time::{Duration, Instant},
};
use uring_runtime::{
    Scope,
    reactor::{IoBuffer, Reactor, SocketAddress},
};

/// Keeps HTTP syntax failures distinct from transport failures.
#[derive(Clone, Copy, Debug, PartialEq)]
enum Failure {
    Http(Error),

    Runtime(uring_runtime::Error),
}

impl From<Error> for Failure {
    /// Preserve an HTTP failure for assertions at the caller boundary.
    fn from(error: Error) -> Self {
        Self::Http(error)
    }
}

impl From<uring_runtime::Error> for Failure {
    /// Preserve the runtime failure without mapping it to HTTP syntax.
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

/// A live scope for exchanges bounded by the test driver's deadline.
#[derive(Clone)]
struct RequestScope;

impl Scope for RequestScope {
    type Error = Failure;

    /// Allow progress until the surrounding test driver stops polling.
    fn check(&self) -> Result<(), Failure> {
        Ok(())
    }
}

/// Wraps a socket address in the caller's endpoint policy.
#[derive(Clone, PartialEq, Eq, PartialOrd, Ord)]
struct Address(SocketAddress);

impl Endpoint<Failure> for Address {
    /// Return the endpoint unchanged without name resolution.
    fn address(&self) -> Result<SocketAddress, Failure> {
        Ok(self.0.clone())
    }
}

/// Supplies permissive resource policy without application dependencies.
struct Caller;

impl Context for Caller {
    type Error = Failure;

    type Scope = RequestScope;

    type Budget = ();

    type Reactor = Rc<Reactor<RequestScope, ()>>;

    type Charge = ();

    type Slot = ();

    type Opaque = ();

    type State = ();

    type Endpoint = Address;

    /// Admit the bounded storage used by each exchange fixture.
    fn charge(&self, _: usize) -> Result<(), Failure> {
        Ok(())
    }

    /// Admit an outbound connection without tracking application quotas.
    fn outbound_slot(&self) -> Result<(), Failure> {
        Ok(())
    }

    /// Keep admission open for the lifetime of the fixture.
    fn stopped(&self) -> bool {
        false
    }
}

/// Build a small reactor and explicit head and body limits.
fn io() -> HttpIo<Caller> {
    HttpIo::new(
        Rc::new(Rc::new(Reactor::new(16, ()))),
        Codec::new(256),
        Rc::new(Caller),
        16,
        16,
    )
}

/// Reserve both ends of a real local stream for HTTP exchanges.
fn pair() -> (ConnectionLease<Caller>, ConnectionLease<Caller>) {
    let (client, server) = UnixStream::pair().unwrap();
    (
        ConnectionLease::from_reserved(client.into(), (), ()).unwrap(),
        ConnectionLease::from_reserved(server.into(), (), ()).unwrap(),
    )
}

/// Poll one operation and its reactor under a fixed progress deadline.
fn drive<T>(io: &HttpIo<Caller>, future: impl Future<Output = T>) -> T {
    let mut future = std::pin::pin!(future);
    let mut cx = std::task::Context::from_waker(Waker::noop());
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
            return value;
        }
        assert!(Instant::now() < deadline, "HTTP exchange made no progress");
        io.reactor().poll_budgeted(32).unwrap();
        std::thread::yield_now();
    }
}

/// Construct a fixed-length request or response head.
fn head(start: StartLine, length: usize) -> MessageHead {
    MessageHead {
        start,
        headers: vec![Header {
            name: "Content-Length".into(),
            value: length.to_string().into_bytes(),
        }],
    }
}

/// Construct a request for the shared fixture resource.
fn request(method: &str, length: usize) -> MessageHead {
    head(
        StartLine::Request {
            method: method.into(),
            target: "/items".into(),
        },
        length,
    )
}

/// Exercise sequential upload and fetch exchanges and server-directed closure.
#[test]
fn upload_then_fetch_on_one_connection_and_honor_server_close() {
    let io = io();
    let (mut client, mut server) = pair();
    for (method, body, reply, close) in [
        ("POST", b"item".as_slice(), b"saved".as_slice(), false),
        ("GET", b"".as_slice(), b"item".as_slice(), true),
    ] {
        client = drive(
            &io,
            io.send_head(client, request(method, body.len()), &RequestScope),
        )
        .unwrap()
        .connection;
        let mut padded = b"__".to_vec();
        padded.extend_from_slice(body);
        padded.extend_from_slice(b"__");
        let sent = drive(
            &io,
            io.write_body_range(
                client,
                OwnedBuffer::copy_from(&Caller, &padded).unwrap(),
                2..2 + body.len(),
                &RequestScope,
            ),
        )
        .unwrap();
        assert_eq!(sent.bytes, body.len());
        assert_eq!(sent.buffer.bytes().unwrap(), padded);
        client = sent.lease;

        let received = drive(
            &io,
            io.receive_request_head_limited(server, &RequestScope, 128),
        )
        .unwrap();
        let request = received.value.unwrap();
        assert!(
            matches!(request.start, StartLine::Request { method: ref m, ref target }
            if m == method && target == "/items")
        );
        assert_eq!(request.content_length().unwrap(), Some(body.len() as u64));
        let uploaded = drive(
            &io,
            io.collect_body(received.connection, body.len(), &RequestScope),
        )
        .unwrap();
        assert_eq!(uploaded.bytes, body.len());
        assert_eq!(uploaded.buffer.bytes().unwrap(), body);
        server = uploaded.lease;
        assert_eq!(server.receive_remaining(), Some(0));
        assert_eq!(
            server.finish_exchange(),
            Err(Failure::Http(Error::Malformed))
        );

        let mut response = head(StartLine::Response { status: 200 }, reply.len());
        if close {
            response.headers.push(Header {
                name: "Connection".into(),
                value: b"close".to_vec(),
            });
        }
        server = drive(&io, io.send_head(server, response, &RequestScope))
            .unwrap()
            .connection;
        server = drive(
            &io,
            io.write_body(
                server,
                OwnedBuffer::copy_from(&Caller, reply).unwrap(),
                &RequestScope,
            ),
        )
        .unwrap()
        .lease;
        let received = drive(&io, io.receive_head(client, &RequestScope)).unwrap();
        assert!(matches!(
            received.value.start,
            StartLine::Response { status: 200 }
        ));
        client = received.connection;
        assert_eq!(
            client.finish_exchange(),
            Err(Failure::Http(Error::Malformed))
        );
        let downloaded = drive(&io, io.collect_body(client, reply.len(), &RequestScope)).unwrap();
        assert_eq!(downloaded.bytes, reply.len());
        assert_eq!(downloaded.buffer.bytes().unwrap(), reply);
        client = downloaded.lease;
        for connection in [&client, &server] {
            assert_eq!(connection.receive_remaining(), Some(0));
            assert_eq!(connection.send_remaining(), Some(0));
            assert_eq!(connection.closing(), close);
        }
        client.finish_exchange().unwrap();
        assert_eq!(client.is_reusable(), !close);
        if close {
            server.finish_exchange().unwrap();
        } else {
            // Advance the server's owned socket without returning it to a pool.
            server.next_round().unwrap();
        }
        assert!(!server.is_reusable());
        for connection in [&client, &server] {
            assert_eq!(connection.receive_remaining(), None);
            assert_eq!(connection.send_remaining(), None);
        }
    }
    assert_eq!(io.reactor().in_flight(), 0);
}

/// Reject invalid body operations before submitting transport work.
#[test]
fn upload_body_bounds_fail_without_waiting_for_more_peer_traffic() {
    for case in ["collect cap", "empty read", "oversized write"] {
        let io = io();
        let (client, server) = pair();
        let sent = drive(&io, io.send_head(client, request("POST", 4), &RequestScope)).unwrap();
        let received = drive(&io, io.receive_head(server, &RequestScope)).unwrap();
        let mut operation = match case {
            "collect cap" => io.collect_body(received.connection, 3, &RequestScope),
            "empty read" => io.read_body(received.connection, io.buffer(0).unwrap(), &RequestScope),
            _ => io.write_body(
                sent.connection,
                OwnedBuffer::copy_from(&Caller, b"extra").unwrap(),
                &RequestScope,
            ),
        };
        assert!(
            matches!(
                operation
                    .as_mut()
                    .poll(&mut std::task::Context::from_waker(Waker::noop())),
                Poll::Ready(Err(Failure::Http(Error::Malformed)))
            ),
            "{case}"
        );
        assert_eq!(io.reactor().in_flight(), 0, "{case} submitted body I/O");
    }
}

/// Keep a rejected request writable for an error response but forbid reuse.
#[test]
fn response_on_request_only_connection_can_be_rejected_but_never_reused() {
    let io = io();
    let (client, server) = pair();
    let sent = drive(
        &io,
        io.send_head(
            client,
            head(StartLine::Response { status: 200 }, 0),
            &RequestScope,
        ),
    )
    .unwrap();
    let mut rejected = drive(
        &io,
        io.receive_request_head_limited(server, &RequestScope, 128),
    )
    .unwrap();
    assert!(matches!(
        rejected.value,
        Err(Failure::Http(Error::Malformed))
    ));
    assert!(rejected.connection.closing());
    assert_eq!(rejected.connection.receive_remaining(), None);
    assert!(rejected.connection.take_read_ahead().is_none());
    let mut response = drive(
        &io,
        io.send_head(
            rejected.connection,
            head(StartLine::Response { status: 400 }, 0),
            &RequestScope,
        ),
    )
    .unwrap();
    let received = drive(&io, io.receive_head(sent.connection, &RequestScope)).unwrap();
    assert!(matches!(
        received.value.start,
        StartLine::Response { status: 400 }
    ));
    assert_eq!(
        response.connection.finish_exchange(),
        Err(Failure::Http(Error::Malformed))
    );
    assert!(!response.connection.is_reusable());
    assert_eq!(io.reactor().in_flight(), 0);
}
