use super::*;
use crate::model::identity::RequestId;
use std::{
    io::{Read, Write as IoWrite},
    net::TcpStream,
    task::Context,
};

fn scope() -> RequestScope {
    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(15)).unwrap()
}
fn setup() -> (Rc<Admission>, Rc<Reactor>, Rc<DiagnosticIo>) {
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(
        DiagnosticIo::attach(reactor.clone(), admission.clone())
            .expect("real io_uring diagnostics attachment"),
    );
    (admission, reactor, io)
}
fn poll_server(server: &mut Operation<'_, ()>, reactor: &Reactor) {
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(server.as_mut().poll(&mut cx).is_pending());
    reactor.poll_budgeted(128).unwrap();
    reactor.wait(Duration::from_millis(1)).unwrap();
}
fn exchange_raw(
    address: std::net::SocketAddr,
    request: &[u8],
    server: &mut Operation<'_, ()>,
    reactor: &Reactor,
) -> Vec<u8> {
    let mut socket = TcpStream::connect(address).unwrap();
    socket.set_nonblocking(true).unwrap();
    let mut sent = 0;
    let mut response = Vec::new();
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        assert!(Instant::now() < deadline, "raw diagnostic exchange stalled");
        poll_server(server, reactor);
        if sent < request.len() {
            // Force fragmentation across worker polls, including CRLF boundaries.
            match socket.write(&request[sent..(sent + 7).min(request.len())]) {
                Ok(count) => sent += count,
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(error) => panic!("{error}"),
            }
        }
        let mut bytes = [0; 512];
        match socket.read(&mut bytes) {
            Ok(0) => break,
            Ok(count) => response.extend_from_slice(&bytes[..count]),
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
            Err(error) => panic!("{error}"),
        }
        assert!(response.len() <= MAX_RESPONSE_BYTES);
    }
    response
}
fn finish(mut server: Operation<'_, ()>, scope: &RequestScope, reactor: &Reactor) {
    scope.cancel().unwrap();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(matches!(
        server.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Cancelled))
    ));
    drop(server);
    let mut drain = reactor.drain();
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < deadline);
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}
fn assert_response(response: &[u8], status: &str, body: Option<&str>) {
    let text = std::str::from_utf8(response).unwrap();
    assert!(
        text.starts_with(&format!("HTTP/1.1 {status}\r\n")),
        "{text}"
    );
    let (head, actual_body) = text.split_once("\r\n\r\n").unwrap();
    let length: usize = head
        .lines()
        .find_map(|line| line.strip_prefix("Content-Length: "))
        .unwrap()
        .parse()
        .unwrap();
    assert_eq!(actual_body.len(), length);
    assert!(head.contains("Connection: close"));
    if let Some(body) = body {
        assert_eq!(actual_body, body);
    }
}
fn good_resources() -> super::super::health::Resources {
    super::super::health::Resources {
        workers_usable: true,
        storage_usable: true,
        listeners_usable: true,
        membership_usable: true,
        admission_usable: true,
        credentials_valid_until: Some(Instant::now() + Duration::from_secs(30)),
        observed_until: Some(Instant::now() + Duration::from_secs(30)),
    }
}

#[test]
fn raw_endpoints_fragmentation_readiness_redaction_and_data_admission_stop() {
    let (admission, reactor, io) = setup();
    let telemetry = Telemetry::default();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io.clone(), &scope);
    // Diagnostics consume neither ordinary connection nor payload quota.
    let _connections = admission
        .reserve(
            None,
            ResourceClass::Connection,
            admission.limit(ResourceClass::Connection),
        )
        .unwrap();
    let _payload = admission
        .reserve(
            None,
            ResourceClass::Plaintext,
            admission.limit(ResourceClass::Plaintext),
        )
        .unwrap();
    let get = |path: &str| {
        format!(
            "GET {path} HTTP/1.1\r\nHost: local\r\nAuthorization: synthetic-secret\r\nX-Key: synthetic-key\r\n\r\n"
        )
    };
    let response = exchange_raw(address, get("/healthz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "200 OK", Some("ok\n"));
    let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
    telemetry.health.observe(good_resources()).unwrap();
    telemetry
        .health
        .transition(super::super::health::State::Ready)
        .unwrap();
    let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "200 OK", Some("ready\n"));
    // Stopping local admission overrides even a still-valid ready observation.
    admission.stop();
    let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
    telemetry
        .health
        .observe(super::super::health::Resources {
            credentials_valid_until: Some(Instant::now()),
            ..good_resources()
        })
        .unwrap();
    let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
    let response = exchange_raw(address, get("/metrics").as_bytes(), &mut server, &reactor);
    assert_response(&response, "200 OK", None);
    let text = std::str::from_utf8(&response).unwrap();
    assert!(text.contains("racer_diagnostic_ready_total 4\n"));
    assert!(text.contains("racer_ready 0\n"));
    assert!(!text.contains("synthetic"));
    assert!(!text.contains("Authorization"));
    assert_eq!(telemetry.tracing.snapshot(&mut [None; 1]).unwrap(), 0);
    finish(server, &scope, &reactor);
    assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 0);
    drop(io);
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
}

#[test]
fn raw_rejections_are_fixed_and_never_echo_untrusted_input() {
    let (_, reactor, io) = setup();
    let telemetry = Telemetry::default();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
    for (request, status) in [
        (
            "GET /secret-key HTTP/1.1\r\nHost: local\r\n\r\n",
            "404 Not Found",
        ),
        (
            "POST /metrics HTTP/1.1\r\nHost: local\r\n\r\n",
            "405 Method Not Allowed",
        ),
        (
            "GET /metrics HTTP/1.1\r\nHost: local\r\nContent-Length: 9\r\n\r\n",
            "400 Bad Request",
        ),
        (
            "GET /metrics HTTP/1.1\r\nHost: local\r\nTransfer-Encoding: chunked\r\n\r\n",
            "400 Bad Request",
        ),
        (
            "GET /metrics HTTP/1.1\r\nHost: local\r\nHost: other\r\n\r\n",
            "400 Bad Request",
        ),
    ] {
        let response = exchange_raw(address, request.as_bytes(), &mut server, &reactor);
        assert_response(&response, status, None);
        assert!(
            !std::str::from_utf8(&response)
                .unwrap()
                .contains("secret-key")
        );
    }
    let mut huge = b"GET /metrics HTTP/1.1\r\nHost: local\r\nX: ".to_vec();
    huge.resize(MAX_REQUEST_BYTES, b'x');
    let response = exchange_raw(address, &huge, &mut server, &reactor);
    assert_response(
        &response,
        "431 Request Header Fields Too Large",
        Some("headers too large\n"),
    );
    assert_eq!(telemetry.metrics.count(Event::DiagnosticRejected), 6);
    finish(server, &scope, &reactor);
}

#[test]
fn slow_socket_does_not_block_probes_and_abandonment_keeps_quota_until_fenced() {
    let (admission, reactor, io) = setup();
    let telemetry = Telemetry::default();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io.clone(), &scope);
    let mut slow = TcpStream::connect(address).unwrap();
    slow.write_all(b"GET /healthz HTTP/1.1\r\n").unwrap();
    for _ in 0..10 {
        poll_server(&mut server, &reactor);
    }
    assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 1);
    let response = exchange_raw(
        address,
        b"GET /healthz HTTP/1.1\r\nHost: local\r\n\r\n",
        &mut server,
        &reactor,
    );
    assert_response(&response, "200 OK", Some("ok\n"));
    drop(server);
    drop(io);
    assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 1);
    assert_eq!(
        admission.used(ResourceClass::ControlProgress),
        CONTROL_SLOTS
    );
    let mut drain = reactor.drain();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        if let Poll::Ready(result) = drain.as_mut().poll(&mut cx) {
            result.unwrap();
            break;
        }
        assert!(Instant::now() < deadline);
        reactor.poll_budgeted(128).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 0);
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
}

#[test]
fn fixed_parser_and_worst_case_response_bounds() {
    let telemetry = Telemetry::default();
    for event in super::super::metrics::EVENTS {
        telemetry.metrics.record(event, u64::MAX).unwrap();
    }
    let mut bytes = [0; MAX_RESPONSE_BYTES];
    let length = respond(&telemetry, Route::Metrics, true, &mut bytes).unwrap();
    assert_response(&bytes[..length], "200 OK", None);
    assert!(length < MAX_RESPONSE_BYTES);
    for request in [
        b"GET /metrics HTTP/1.0\r\n\r\n".as_slice(),
        b"GET /metrics HTTP/1.1\r\n\r\n",
        b"GET /metrics HTTP/1.1\r\nHost: l\r\nContent-Length: 0\r\nContent-Length: 0\r\n\r\n",
    ] {
        assert_eq!(parse(request), Route::BadRequest);
    }
    assert_eq!(
        parse(b"GET /metrics?secret HTTP/1.1\r\nHost: l\r\n\r\n"),
        Route::NotFound
    );
}

#[test]
fn unattached_serving_fails_closed_and_attachment_capacity_rolls_back() {
    let telemetry = Telemetry::default();
    let scope = scope();
    let mut serving = telemetry.serve("127.0.0.1:0".parse().unwrap(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(matches!(
        serving.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::InvalidConfiguration))
    ));
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.request_context_bytes = std::num::NonZeroUsize::new(1).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    assert!(matches!(
        telemetry.attach_io(reactor, admission.clone()),
        Err(Error::Overloaded)
    ));
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
}

#[test]
fn raw_slow_clients_are_bounded_and_timeout_releases_capacity() {
    let (_, reactor, io) = setup();
    let telemetry = Telemetry::default();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
    let sockets: Vec<_> = (0..MAX_CONNECTIONS + 1)
        .map(|_| TcpStream::connect(address).unwrap())
        .collect();
    for _ in 0..40 {
        poll_server(&mut server, &reactor);
    }
    assert_eq!(
        telemetry.metrics.gauge(Gauge::DiagnosticConnections),
        MAX_CONNECTIONS as u64
    );
    assert_eq!(
        telemetry.metrics.count(Event::DiagnosticAccepted),
        MAX_CONNECTIONS as u64
    );
    let deadline = Instant::now() + CONNECTION_TIMEOUT + Duration::from_secs(2);
    while telemetry.metrics.count(Event::DiagnosticTimeout) < MAX_CONNECTIONS as u64 {
        assert!(Instant::now() < deadline);
        poll_server(&mut server, &reactor);
        assert!(telemetry.metrics.gauge(Gauge::DiagnosticConnections) <= MAX_CONNECTIONS as u64);
    }
    drop(sockets);
    let response = exchange_raw(
        address,
        b"GET /healthz HTTP/1.1\r\nHost: local\r\n\r\n",
        &mut server,
        &reactor,
    );
    assert_response(&response, "200 OK", Some("ok\n"));
    finish(server, &scope, &reactor);
    assert_eq!(telemetry.metrics.gauge(Gauge::DiagnosticConnections), 0);
}

#[test]
fn attach_is_explicit_and_bind_failure_is_reported_on_first_poll() {
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let telemetry = Telemetry::default();
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
    telemetry
        .attach_io(reactor.clone(), admission.clone())
        .unwrap();
    assert_eq!(
        admission.used(ResourceClass::ControlProgress),
        CONTROL_SLOTS
    );
    assert_eq!(
        telemetry.attach_io(reactor, admission.clone()),
        Err(Error::InvalidConfiguration)
    );
    let occupied = TcpListener::bind("127.0.0.1:0").unwrap();
    let scope = scope();
    let mut serving = telemetry.serve(occupied.local_addr().unwrap(), &scope);
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    assert!(matches!(
        serving.as_mut().poll(&mut cx),
        Poll::Ready(Err(Error::Io))
    ));
    drop(serving);
    drop(telemetry);
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
}
