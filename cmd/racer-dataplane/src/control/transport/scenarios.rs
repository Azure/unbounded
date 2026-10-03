use super::*;
use crate::control::{enrollment::Enrollment, testing};
use std::{
    cell::{Cell, RefCell},
    io::{Read, Write},
    sync::Arc,
    time::{Duration, SystemTime},
};
pub(in crate::control) use testing::FixtureIo;

fn server_config(
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    optional_client: bool,
) -> Arc<rustls::ServerConfig> {
    let mut params =
        rcgen::CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()]).unwrap();
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let cert = params.signed_by(&key, ca, ca_key).unwrap();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        provider.clone(),
    );
    let verifier = if optional_client {
        verifier.allow_unauthenticated()
    } else {
        verifier
    }
    .build()
    .unwrap();
    Arc::new(
        rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(verifier)
            .with_single_cert(
                vec![cert.der().clone()],
                rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
            )
            .unwrap(),
    )
}

fn accept(listener: &std::net::TcpListener) -> std::net::TcpStream {
    listener.set_nonblocking(true).unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let socket = loop {
        match listener.accept() {
            Ok((socket, _)) => break socket,
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                assert!(Instant::now() < deadline, "missing backend connection");
                std::thread::sleep(Duration::from_millis(1));
            }
            Err(e) => panic!("accept: {e}"),
        }
    };
    socket
        .set_read_timeout(Some(Duration::from_secs(10)))
        .unwrap();
    socket
        .set_write_timeout(Some(Duration::from_secs(10)))
        .unwrap();
    socket
}
fn read_head(stream: &mut impl Read) -> String {
    let mut request = Vec::new();
    while !request.ends_with(b"\r\n\r\n") {
        assert!(request.len() < 16384);
        let mut byte = [0];
        stream.read_exact(&mut byte).unwrap();
        request.push(byte[0]);
    }
    String::from_utf8(request).unwrap()
}
fn assert_disconnected(stream: &mut impl Read) {
    match stream.read(&mut [0]) {
        Ok(0) => (),
        Err(e)
            if matches!(
                e.kind(),
                std::io::ErrorKind::UnexpectedEof | std::io::ErrorKind::ConnectionReset
            ) =>
        {
            ()
        }
        result => panic!("connection not discarded: {result:?}"),
    }
}

/// Each group uses exactly one TLS connection; EOF proves actual retirement.
pub(in crate::control) fn scripted_server(
    d: &testing::Directory,
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    groups: Vec<Vec<(String, u16, Vec<u8>)>>,
) -> (ControlEndpoint, std::thread::JoinHandle<()>) {
    let config = server_config(ca, ca_key, false);
    let trust = d.0.join("trust");
    std::fs::write(&trust, ca.pem()).unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = ControlEndpoint {
        url: format!("https://{}", listener.local_addr().unwrap()),
        trust_bundle: trust,
    };
    let server = std::thread::spawn(move || {
        for group in groups {
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(config.clone()).unwrap(),
                accept(&listener),
            );
            let mut disconnected = false;
            for (path, status, body) in group {
                assert!(read_head(&mut stream).starts_with(&format!("GET {path} HTTP/1.1\r\n")));
                if status == 0 {
                    disconnected = true;
                    break;
                }
                let head = format!(
                    "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\nRetry-After: 1\r\n\r\n",
                    body.len()
                );
                stream.write_all(head.as_bytes()).unwrap();
                stream.write_all(&body).unwrap();
                stream.flush().unwrap();
            }
            if !disconnected {
                assert_disconnected(&mut stream);
            }
        }
    });
    (endpoint, server)
}

#[test]
fn real_connect_readiness_blackhole_yields_to_next_address_and_cleans_fds() {
    use std::task::{Context, Poll};
    struct ConnectIo {
        driver: ReactorControlIo,
        calls: Cell<usize>,
        held: RefCell<Option<std::rc::Weak<Descriptor>>>,
        leases: RefCell<Vec<std::rc::Weak<crate::runtime::admission::ConnectionReservation>>>,
        addresses: [SocketAddr; 2],
        mode: &'static str,
        parent_deadline: Instant,
    }
    impl ControlIo for ConnectIo {
        fn reactor(&self) -> Option<Rc<crate::runtime::reactor::Reactor>> {
            self.driver.reactor()
        }
        fn read_file<'a>(
            &'a self,
            path: &'a std::path::Path,
            limit: usize,
            scope: &'a RequestScope,
        ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
            FixtureIo.read_file(path, limit, scope)
        }
        fn resolve<'a>(
            &'a self,
            _: &'a str,
            _: u16,
            _: &'a RequestScope,
        ) -> Operation<'a, Vec<SocketAddr>> {
            Box::pin(async { Ok(self.addresses.to_vec()) })
        }
        fn ready_charged<'a>(
            &'a self,
            fd: Rc<Descriptor>,
            read: bool,
            write: bool,
            lease: Option<Rc<crate::runtime::admission::ConnectionReservation>>,
            scope: &'a RequestScope,
        ) -> Operation<'a, ()> {
            Box::pin(async move {
                self.leases
                    .borrow_mut()
                    .push(Rc::downgrade(lease.as_ref().expect("admission lease")));
                if !read && write {
                    let call = self.calls.get();
                    self.calls.set(call + 1);
                    if call == 0 {
                        *self.held.borrow_mut() = Some(Rc::downgrade(&fd));
                        assert!(scope.deadline.0 < self.parent_deadline);
                        if self.mode == "cancel" {
                            scope.cancel()?;
                        }
                        // Suppress a real socket's writable notification, not Simulation::connect.
                        let mut wait = scope.clone();
                        if self.mode == "parent" {
                            wait.deadline.0 = self.parent_deadline;
                        }
                        return self
                            .driver
                            .sleep(wait.deadline.0 + Duration::from_millis(1), &wait)
                            .await;
                    }
                    // Inspect the actual OS socket at the public readiness boundary.
                    let raw = unsafe { libc::dup(fd.as_raw_fd()) };
                    assert!(raw >= 0);
                    let socket = unsafe { std::net::TcpStream::from_raw_fd(raw) };
                    assert_eq!(socket.peer_addr().unwrap(), self.addresses[1]);
                }
                self.driver
                    .ready_charged(fd, read, write, lease, scope)
                    .await
            })
        }
        fn ready<'a>(
            &'a self,
            fd: Rc<Descriptor>,
            read: bool,
            write: bool,
            scope: &'a RequestScope,
        ) -> Operation<'a, ()> {
            self.driver.ready(fd, read, write, scope)
        }
        fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()> {
            self.driver.sleep(until, scope)
        }
    }
    for mode in ["local", "cancel", "parent"] {
        let Some(reactor) = testing::reactor() else {
            return;
        };
        let d = testing::Directory::new();
        let (ca, key) = testing::ca();
        let config = server_config(&ca, &key, true);
        let trust = d.0.join("trust");
        std::fs::write(&trust, ca.pem()).unwrap();
        let first = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let healthy = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addresses = [first.local_addr().unwrap(), healthy.local_addr().unwrap()];
        let server = (mode == "local").then(|| {
            std::thread::spawn(move || {
                let mut socket = accept(&healthy);
                let mut tls = rustls::ServerConnection::new(config).unwrap();
                while tls.is_handshaking() {
                    match tls.complete_io(&mut socket) {
                        Ok(_) => (),
                        // Bootstrap can finish client-side before its final flight
                        // is flushed. This test drops at checkout, before a request.
                        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => break,
                        Err(e) => panic!("blackhole fixture handshake: {e}"),
                    }
                }
                assert_eq!(
                    tls.protocol_version(),
                    Some(rustls::ProtocolVersion::TLSv1_3)
                );
                assert_disconnected(&mut socket);
            })
        });
        let scope = RequestScope::new(
            crate::model::RequestId([7; 16]),
            Instant::now() + Duration::from_millis(300),
        )
        .unwrap();
        let io = Rc::new(ConnectIo {
            driver: ReactorControlIo::new(reactor.clone()),
            calls: Cell::new(0),
            held: RefCell::new(None),
            leases: RefCell::new(Vec::new()),
            addresses,
            mode,
            parent_deadline: scope.deadline.0,
        });
        let transport = ControlTransport::new(ControlEndpoint {
            url: "https://localhost".into(),
            trust_bundle: trust,
        });
        transport.attach_io(io.clone());
        let mut connect = transport.bootstrap(&scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let watchdog = Instant::now() + Duration::from_secs(2);
        let result = loop {
            if let Poll::Ready(result) = connect.as_mut().poll(&mut cx) {
                break result;
            }
            assert!(Instant::now() < watchdog);
            reactor.poll_budgeted(64).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        };
        drop(connect);
        match mode {
            "local" => {
                let connection = result.unwrap();
                assert_eq!(io.calls.get(), 2);
                assert!(
                    Instant::now() < scope.deadline.0,
                    "TLS retains overall budget"
                );
                drop(connection);
            }
            "cancel" => assert!(matches!(result, Err(Error::Cancelled))),
            _ => assert!(matches!(result, Err(Error::DeadlineExceeded))),
        }
        if mode != "local" {
            assert_eq!(io.calls.get(), 1);
        }
        assert!(io.held.borrow().as_ref().unwrap().upgrade().is_none());
        let mut closed = accept(&first);
        closed
            .set_read_timeout(Some(Duration::from_secs(1)))
            .unwrap();
        assert_eq!(
            closed.read(&mut [0; 1]).unwrap(),
            0,
            "both failed socket FDs closed"
        );
        assert_eq!(reactor.in_flight(), 0);
        assert!(
            io.leases
                .borrow()
                .iter()
                .all(|lease| lease.upgrade().is_none())
        );
        if let Some(server) = server {
            server.join().unwrap();
        }
    }
}

fn enrolled(
    r: &Rc<crate::runtime::reactor::Reactor>,
    d: &testing::Directory,
    ca: &rcgen::Certificate,
    key: &rcgen::KeyPair,
) -> LocalSigningIdentity {
    let scope = testing::scope();
    let enrollment = Enrollment::new(
        crate::model::ClusterId("11111111-1111-4111-8111-111111111111".into()),
        d.0.join("token"),
        d.0.join("identity"),
    );
    enrollment
        .set_peer_trust_roots(vec![ca.der().to_vec()])
        .unwrap();
    enrollment.attach_reactor(r.clone());
    let request = testing::drive(r, enrollment.prepare(&scope)).unwrap();
    testing::drive(
        r,
        enrollment.accept_response_async(
            testing::issue(&request, ca, key, "22222222-2222-4222-8222-222222222222"),
            &scope,
        ),
    )
    .unwrap()
}

#[test]
fn real_ring_server_auth_and_mutual_tls() {
    let Some(r) = testing::reactor() else { return };
    let d = testing::Directory::new();
    let (ca, ca_key) = testing::ca();
    let identity = enrolled(&r, &d, &ca, &ca_key);
    let config = server_config(&ca, &ca_key, true);
    let trust = d.0.join("trust.pem");
    std::fs::write(&trust, ca.pem()).unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    let server = std::thread::spawn(move || {
        for mutual in [false, true] {
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(config.clone()).unwrap(),
                accept(&listener),
            );
            let request = read_head(&mut stream);
            assert_eq!(stream.conn.peer_certificates().is_some(), mutual);
            assert_eq!(
                stream.conn.protocol_version(),
                Some(rustls::ProtocolVersion::TLSv1_3)
            );
            if !mutual {
                assert!(request.contains("Authorization: Bearer fixture.token"));
            }
            stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\n{}\r\n0\r\n\r\n").unwrap();
            stream.flush().unwrap();
        }
    });
    let transport = ControlTransport::new(ControlEndpoint {
        url: format!("https://127.0.0.1:{port}"),
        trust_bundle: trust,
    });
    transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
    let scope = testing::scope();
    let first = testing::drive(
        &r,
        Box::pin(async {
            transport
                .bootstrap(&scope)
                .await?
                .request(
                    "POST",
                    wire::BOOTSTRAP_PATH,
                    Some("fixture.token"),
                    &[],
                    65536,
                    &scope,
                )
                .await
        }),
    )
    .unwrap();
    assert_eq!(first.body, b"{}");
    let second = testing::drive(
        &r,
        Box::pin(async {
            transport
                .authenticated(&identity, &scope)
                .await?
                .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
                .await
        }),
    )
    .unwrap();
    assert_eq!(second.status, 200);
    assert_eq!(second.body, b"{}");
    server.join().unwrap();
}

#[test]
fn backend_disconnect_recovers_after_health_retry_boundary() {
    let Some(r) = testing::reactor() else { return };
    let d = testing::Directory::new();
    let (ca, key) = testing::ca();
    let identity = enrolled(&r, &d, &ca, &key);
    let response = |status| {
        (
            wire::SNAPSHOT_PATH.to_owned(),
            status,
            if status == 204 {
                vec![]
            } else {
                b"{}".to_vec()
            },
        )
    };
    let (endpoint, server) = scripted_server(
        &d,
        &ca,
        &key,
        vec![vec![response(200), response(0)], vec![response(204)]],
    );
    let transport = ControlTransport::new(endpoint);
    transport.attach_io(Rc::new(FixtureIo));
    let scope = testing::scope();
    let get = || {
        futures::executor::block_on(async {
            transport
                .authenticated(&identity, &scope)
                .await?
                .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
                .await
        })
    };
    assert_eq!(get().unwrap().status, 200);
    assert!(matches!(get(), Err(Error::Io)));
    // Keep Racer's link circuit integration separate from generic pool policy.
    transport
        .health
        .observe_at(
            &transport.endpoint,
            crate::topology::health::LinkOutcome::Refused,
            Instant::now() - Duration::from_secs(60),
        )
        .unwrap();
    assert_eq!(get().unwrap().status, 204);
    transport.close_idle();
    server.join().unwrap();
}

#[test]
fn malformed_delta_base_and_method_are_rejected_before_writing() {
    let d = testing::Directory::new();
    let (ca, key) = testing::ca();
    let config = server_config(&ca, &key, true);
    let trust = d.0.join("trust");
    std::fs::write(&trust, ca.pem()).unwrap();
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = ControlEndpoint {
        url: format!("https://{}", listener.local_addr().unwrap()),
        trust_bundle: trust,
    };
    let cases = [
        ("DELETE", None),
        ("GET", Some("bad".to_owned())),
        ("GET", Some("g".repeat(64))),
        ("GET", Some("a".repeat(63))),
        ("GET", Some("a".repeat(65))),
        ("GET", Some(format!("{}\r\n", "a".repeat(62)))),
    ];
    let count = cases.len();
    let server = std::thread::spawn(move || {
        for _ in 0..count {
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(config.clone()).unwrap(),
                accept(&listener),
            );
            assert_disconnected(&mut stream);
        }
        for base in ["a".repeat(64), "ABCDEF0123456789".repeat(4)] {
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(config.clone()).unwrap(),
                accept(&listener),
            );
            assert!(
                read_head(&mut stream).ends_with(&format!("X-Racer-Delta-Base: {base}\r\n\r\n"))
            );
            stream
                .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                .unwrap();
            stream.flush().unwrap();
        }
    });
    let transport = ControlTransport::new(endpoint);
    transport.attach_io(Rc::new(FixtureIo));
    let scope = testing::scope();
    for (method, base) in cases {
        let connection = futures::executor::block_on(transport.bootstrap(&scope)).unwrap();
        assert!(matches!(
            futures::executor::block_on(connection.request_delta(
                method,
                "/enroll",
                None,
                &[],
                1024,
                base.as_deref(),
                &scope
            )),
            Err(Error::InvalidRequest)
        ));
    }
    for base in ["a".repeat(64), "ABCDEF0123456789".repeat(4)] {
        let connection = futures::executor::block_on(transport.bootstrap(&scope)).unwrap();
        assert_eq!(
            futures::executor::block_on(connection.request_delta(
                "GET",
                "/enroll",
                None,
                &[],
                1024,
                Some(&base),
                &scope
            ))
            .unwrap()
            .status,
            204
        );
    }
    server.join().unwrap();
}

#[test]
fn not_yet_valid_identity_is_rejected_before_transport_io() {
    let Some(r) = testing::reactor() else { return };
    let d = testing::Directory::new();
    let (ca, key) = testing::ca();
    let identity = enrolled(&r, &d, &ca, &key);
    assert!(identity.valid_now());
    let clock = uring_runtime::environment::SimulationClock::new(17);
    let environment = clock.environment(1);
    // Roll only the scoped wall clock back after real enrollment. The generic
    // Identity has expiry only; Racer must enforce its not-before policy itself.
    clock.set_wall_time(SystemTime::now() - Duration::from_secs(3600));
    let _guard = environment.enter();
    assert!(!identity.valid_now());
    let transport = ControlTransport::new(ControlEndpoint {
        url: "https://localhost".into(),
        trust_bundle: d.0.join("missing"),
    });
    // No Io attached: reaching generic transport would yield InvalidConfiguration.
    let scope = RequestScope::new(
        crate::model::RequestId([7; 16]),
        uring_runtime::environment::now() + Duration::from_secs(10),
    )
    .unwrap();
    assert!(matches!(
        futures::executor::block_on(transport.authenticated(&identity, &scope)),
        Err(Error::Unauthorized)
    ));
}
