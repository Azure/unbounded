use super::*;
use crate::model::RequestId;
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
fn good_resources() -> health::Resources {
    health::Resources {
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
fn resource_and_lifecycle_changes_are_observable_at_both_health_endpoints() {
    let (_, reactor, io) = setup();
    let telemetry = Telemetry::default();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
    let mut observe = |ready: bool, live: bool| {
        for (path, healthy, body) in [
            (
                "/readyz",
                ready,
                if ready { "ready\n" } else { "not ready\n" },
            ),
            ("/healthz", live, if live { "ok\n" } else { "not live\n" }),
        ] {
            let response = exchange_raw(
                address,
                format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes(),
                &mut server,
                &reactor,
            );
            assert_response(
                &response,
                if healthy {
                    "200 OK"
                } else {
                    "503 Service Unavailable"
                },
                Some(body),
            );
        }
    };
    observe(false, true);
    assert_eq!(
        telemetry.health.transition(health::State::Ready),
        Err(Error::Unavailable)
    );
    let good = good_resources();
    telemetry.health.observe(good).unwrap();
    telemetry.health.transition(health::State::Ready).unwrap();
    observe(true, true);
    for bad in [
        health::Resources {
            workers_usable: false,
            ..good
        },
        health::Resources {
            storage_usable: false,
            ..good
        },
        health::Resources {
            listeners_usable: false,
            ..good
        },
        health::Resources {
            membership_usable: false,
            ..good
        },
        health::Resources {
            admission_usable: false,
            ..good
        },
        health::Resources {
            credentials_valid_until: Some(Instant::now()),
            ..good
        },
        health::Resources {
            credentials_valid_until: None,
            ..good
        },
        health::Resources {
            observed_until: Some(Instant::now()),
            ..good
        },
        health::Resources {
            observed_until: None,
            ..good
        },
    ] {
        telemetry.health.observe(bad).unwrap();
        observe(false, true);
        telemetry.health.observe(good).unwrap();
        observe(true, true);
    }
    telemetry
        .health
        .transition(health::State::Draining)
        .unwrap();
    observe(false, true);
    assert_eq!(
        telemetry.health.transition(health::State::Ready),
        Err(Error::Unavailable)
    );
    telemetry.health.transition(health::State::Stopped).unwrap();
    observe(false, false);
    assert_eq!(
        telemetry.health.transition(health::State::Starting),
        Err(Error::Unavailable)
    );
    finish(server, &scope, &reactor);
}

#[test]
fn diagnostic_probe_and_monitors_progress_under_sustained_queue_pressure() {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.queue_entries = std::num::NonZeroUsize::new(8).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(DiagnosticIo::attach(reactor.clone(), admission.clone()).unwrap());
    let telemetry = Telemetry::default();
    telemetry.health.observe(good_resources()).unwrap();
    telemetry.health.transition(health::State::Ready).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
    let mut sockets: Vec<_> = ["/readyz", "/metrics", "/debug/failures", "/healthz"]
        .into_iter()
        .map(|path| {
            let mut socket = TcpStream::connect(address).unwrap();
            socket
                .write_all(format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes())
                .unwrap();
            socket.set_nonblocking(true).unwrap();
            (socket, Vec::new(), false)
        })
        .collect();
    // Accept one connection first, then let ordinary work occupy every available
    // slot before its receive is submitted. Before isolation this resets the probe.
    let deadline = Instant::now() + Duration::from_secs(1);
    while telemetry.metrics.count(Event::DiagnosticAccepted) == 0 {
        assert!(Instant::now() < deadline);
        poll_server(&mut server, &reactor);
    }
    let (reader, _writer) = std::os::unix::net::UnixStream::pair().unwrap();
    let reader = Rc::new(Descriptor::from(reader));
    let mut pressure = Vec::new();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    for _ in 0..=8 {
        let mut wait = reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope);
        match wait.as_mut().poll(&mut cx) {
            Poll::Pending => pressure.push(wait),
            Poll::Ready(Err(Error::Overloaded)) => break,
            _ => panic!("unexpected pressure result"),
        }
    }
    assert!(!pressure.is_empty());
    assert_eq!(pressure.len(), 8 - CONTROL_SLOTS);
    // Payload work also consumes all remaining bookkeeping memory. Diagnostics
    // must use their existing startup charge, not acquire shared bytes per SQE.
    let _memory_pressure = admission
        .reserve(
            None,
            ResourceClass::RequestContext,
            admission.limit(ResourceClass::RequestContext)
                - admission.used(ResourceClass::RequestContext),
        )
        .unwrap();
    let deadline = Instant::now() + Duration::from_secs(1);
    while sockets.iter().any(|(_, _, done)| !done) {
        assert!(
            Instant::now() < deadline,
            "diagnostics starved by ordinary queue entries"
        );
        poll_server(&mut server, &reactor);
        assert!(reactor.in_flight() <= 8);
        assert!(telemetry.metrics.gauge(Gauge::DiagnosticConnections) <= MAX_CONNECTIONS as u64);
        for (socket, response, done) in &mut sockets {
            if *done {
                continue;
            }
            let mut bytes = [0; 4096];
            match socket.read(&mut bytes) {
                Ok(0) => {
                    assert_response(response, "200 OK", None);
                    *done = true;
                }
                Ok(count) => response.extend_from_slice(&bytes[..count]),
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(error) => panic!("diagnostic reset under ordinary queue pressure: {error}"),
            }
        }
    }
    assert_eq!(telemetry.metrics.count(Event::DiagnosticIoError), 0);
    assert_eq!(telemetry.metrics.count(Event::DiagnosticTimeout), 0);
    // Isolation must not turn genuinely stale/unusable health into a ready result.
    telemetry
        .health
        .observe(health::Resources {
            observed_until: Some(Instant::now()),
            ..good_resources()
        })
        .unwrap();
    let response = exchange_raw(
        address,
        b"GET /readyz HTTP/1.1\r\nHost: local\r\n\r\n",
        &mut server,
        &reactor,
    );
    assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
    admission.stop();
    let response = exchange_raw(
        address,
        b"GET /readyz HTTP/1.1\r\nHost: local\r\n\r\n",
        &mut server,
        &reactor,
    );
    assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
    drop(_memory_pressure);
    drop(pressure);
    finish(server, &scope, &reactor);
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
}

#[test]
fn diagnostic_accept_recovers_after_full_entry_table() {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.queue_entries = std::num::NonZeroUsize::new(8).unwrap();
    let admission = Rc::new(Admission::new(limits));
    let reactor = Rc::new(Reactor::new(admission.clone()));
    let io = Rc::new(DiagnosticIo::attach(reactor.clone(), admission.clone()).unwrap());
    let telemetry = Telemetry::default();
    telemetry.health.observe(good_resources()).unwrap();
    telemetry.health.transition(health::State::Ready).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let scope = scope();
    let mut server = telemetry.serve_listener_with_io(listener, io, &scope);
    let (reader, _writer) = std::os::unix::net::UnixStream::pair().unwrap();
    let reader = Rc::new(Descriptor::from(reader));
    let mut pressure = Vec::new();
    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
    // The ordinary partition can fill, but cannot steal the listener's slots.
    for _ in 0..8 - CONTROL_SLOTS {
        let mut wait = reactor.readiness(reader.clone(), libc::POLLIN as u32, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        pressure.push(wait);
    }
    assert_eq!(reactor.in_flight(), 8 - CONTROL_SLOTS);
    for _ in 0..32 {
        assert!(server.as_mut().poll(&mut cx).is_pending());
        assert_eq!(reactor.in_flight(), 8 - CONTROL_SLOTS + 1);
    }
    drop(pressure);
    for path in ["/readyz", "/metrics"] {
        let response = exchange_raw(
            address,
            format!("GET {path} HTTP/1.1\r\nHost: local\r\n\r\n").as_bytes(),
            &mut server,
            &reactor,
        );
        assert_response(&response, "200 OK", None);
    }
    finish(server, &scope, &reactor);
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(admission.used(ResourceClass::ControlProgress), 0);
}

#[test]
fn failure_endpoint_exports_full_ring_with_maximum_numeric_fields() {
    use crate::{
        model::{AttemptId, WorkerId},
        telemetry::failures::{CAPACITY, Detail, Failure, Stage},
    };
    let telemetry = Telemetry::default();
    let observer = telemetry.failures.observer(WorkerId(u16::MAX));
    let scope = RequestScope::new(
        RequestId([255; 16]),
        Instant::now() + Duration::from_secs(10),
    )
    .unwrap();
    for _ in 0..CAPACITY + 1 {
        observer.record(
            Failure::new(
                Stage::PeerReceiveAdmission,
                Error::UnsatisfiableRangeWithLength(u64::MAX),
            )
            .request(&scope)
            .attempt(AttemptId([255; 16]))
            .detail(Detail::Resource {
                class: ResourceClass::OutboundConnection,
                used: usize::MAX,
                limit: usize::MAX,
                requested: usize::MAX,
                cache_used: Some(usize::MAX),
                cache_limit: Some(usize::MAX),
            }),
        );
    }
    let mut bytes = vec![0; MAX_RESPONSE_BYTES];
    let length = respond(&telemetry, parse(b"GET /debug/failures HTTP/1.1\r\nHost: local\r\nAuthorization: synthetic-secret\r\n\r\n"), true, &mut bytes).unwrap();
    assert_response(&bytes[..length], "200 OK", None);
    let text = std::str::from_utf8(&bytes[..length]).unwrap();
    assert_eq!(text.matches("stage=PeerReceiveAdmission").count(), CAPACITY);
    assert!(text.contains("sequence=2 worker=65535"));
    assert!(!text.contains("synthetic"));
}

#[test]
fn aead_endpoint_exports_full_ring_with_maximum_fields() {
    use crate::{
        model::WorkerId,
        runtime::crypto::CryptoId,
        telemetry::failures::{AEAD_CAPACITY, test_aead_failure},
    };
    let telemetry = Telemetry::default();
    let observer = telemetry.failures.observer(WorkerId(u16::MAX));
    for _ in 0..AEAD_CAPACITY + 1 {
        observer.record_aead(
            CryptoId {
                worker: WorkerId(u16::MAX),
                generation: u64::MAX,
                sequence: u64::MAX,
            },
            test_aead_failure(),
        );
    }
    let mut bytes = vec![0; MAX_RESPONSE_BYTES];
    let length = respond(
        &telemetry,
        parse(
            b"GET /debug/aead HTTP/1.1\r\nHost: local\r\nAuthorization: synthetic-secret\r\n\r\n",
        ),
        true,
        &mut bytes,
    )
    .unwrap();
    assert_response(&bytes[..length], "200 OK", None);
    let text = std::str::from_utf8(&bytes[..length]).unwrap();
    assert!(length < MAX_RESPONSE_BYTES);
    assert_eq!(text.matches(" supplier=").count(), AEAD_CAPACITY);
    assert!(text.contains("total=65 retained=64 overwritten=1 capacity=64"));
    assert!(!text.contains("synthetic"));
}

#[test]
fn send_crc_endpoint_is_empty_when_disabled_and_does_not_echo_headers() {
    let telemetry = Telemetry::default();
    let mut bytes = vec![0; MAX_RESPONSE_BYTES];
    let length = respond(&telemetry, parse(b"GET /debug/send-crc HTTP/1.1\r\nHost: local\r\nAuthorization: synthetic-secret\r\n\r\n"), true, &mut bytes).unwrap();
    assert_response(&bytes[..length], "200 OK", None);
    let text = std::str::from_utf8(&bytes[..length]).unwrap();
    assert!(text.contains("sampled=0"));
    assert!(!text.contains("synthetic"));
    telemetry.send_crc.fill_test_ring();
    let length = respond(
        &telemetry,
        parse(b"GET /debug/send-crc HTTP/1.1\r\nHost: local\r\n\r\n"),
        true,
        &mut bytes,
    )
    .unwrap();
    assert_response(&bytes[..length], "200 OK", None);
    assert!(length < MAX_RESPONSE_BYTES);
    assert_eq!(
        std::str::from_utf8(&bytes[..length])
            .unwrap()
            .matches("status=computed")
            .count(),
        64
    );
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
    telemetry.health.transition(health::State::Ready).unwrap();
    let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "200 OK", Some("ready\n"));
    // Stopping local admission overrides even a still-valid ready observation.
    admission.stop();
    let response = exchange_raw(address, get("/readyz").as_bytes(), &mut server, &reactor);
    assert_response(&response, "503 Service Unavailable", Some("not ready\n"));
    telemetry
        .health
        .observe(health::Resources {
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
    assert!(text.contains("racer_requests_total 0\n"));
    assert!(text.contains("racer_request_errors_total 0\n"));
    assert!(text.contains("racer_active_requests 0\n"));
    assert!(text.contains("racer_ready 0\n"));
    assert!(!text.contains("synthetic"));
    assert!(!text.contains("Authorization"));
    assert_eq!(telemetry.tracing.snapshot(&mut [None; 1]).unwrap(), 0);
    telemetry
        .failures
        .observer(crate::model::WorkerId(1))
        .record(
            crate::telemetry::failures::Failure::new(
                crate::telemetry::failures::Stage::NextSlice,
                Error::Unavailable,
            )
            .request(&scope),
        );
    let response = exchange_raw(
        address,
        get("/debug/failures").as_bytes(),
        &mut server,
        &reactor,
    );
    assert_response(&response, "200 OK", None);
    let text = std::str::from_utf8(&response).unwrap();
    assert!(text.contains("worker=1 stage=NextSlice error=Unavailable"));
    assert!(!text.contains("synthetic"));
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
    for event in metrics::EVENTS {
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
fn metrics_http_response_exports_worker_quotas_with_bounded_output() {
    use crate::{model::WorkerId, telemetry::metrics::Metrics};
    let workers = Metrics::for_workers(64).unwrap();
    let admissions: Vec<_> = workers
        .iter()
        .enumerate()
        .map(|(index, metrics)| {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.relay_transfers = std::num::NonZeroUsize::new(usize::MAX).unwrap();
            limits.ciphertext_bytes = std::num::NonZeroUsize::new(usize::MAX).unwrap();
            let admission = Admission::new(limits);
            metrics
                .observe_admission(WorkerId(u16::MAX - index as u16), admission.usage())
                .unwrap();
            admission
        })
        .collect();
    let charges: Vec<_> = admissions
        .iter()
        .map(|admission| {
            (
                admission
                    .reserve(None, ResourceClass::Relay, usize::MAX)
                    .unwrap(),
                admission
                    .reserve(None, ResourceClass::Ciphertext, usize::MAX)
                    .unwrap(),
            )
        })
        .collect();
    let mut telemetry = Telemetry::default();
    telemetry.metrics = workers[0].clone();
    for event in crate::telemetry::metrics::EVENTS {
        telemetry.metrics.record(event, u64::MAX).unwrap();
    }
    let mut bytes = [0; MAX_RESPONSE_BYTES];
    let length = respond(&telemetry, Route::Metrics, true, &mut bytes).unwrap();
    assert_response(&bytes[..length], "200 OK", None);
    let text = std::str::from_utf8(&bytes[..length]).unwrap();
    assert!(length < MAX_RESPONSE_BYTES);
    for name in [
        "racer_opaque_relay_body_completed_total",
        "racer_opaque_relay_body_completed_bytes_total",
        "racer_opaque_relay_body_failed_total",
    ] {
        assert!(text.contains(&format!("# TYPE {name} counter\n{name} {}\n", u64::MAX)));
        assert_eq!(
            text.lines().filter(|line| line.starts_with(name)).count(),
            1
        );
        assert!(!text.contains(&format!("{name}{{")));
    }
    for name in [
        "relay_used",
        "relay_limit",
        "ciphertext_used_bytes",
        "ciphertext_limit_bytes",
    ] {
        assert!(text.contains(&format!("# TYPE racer_worker_{name} gauge\n")));
        assert!(text.contains(&format!(
            "racer_worker_{name}{{worker=\"65535\"}} {}\n",
            usize::MAX
        )));
    }
    assert_eq!(
        text.lines()
            .filter(|line| line.contains("{worker="))
            .count(),
        4 * 64
    );
    let mut small = [0; 512];
    assert!(matches!(
        respond(&telemetry, Route::Metrics, true, &mut small),
        Err(Error::Internal)
    ));
    drop(charges);
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
