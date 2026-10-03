//! Racer policy and health around the generic worker-local REST transport.
use super::{ControlEndpoint, enrollment::LocalSigningIdentity};
use crate::{
    error::{Error, Operation, Result},
    runtime::deadline::RequestScope,
};
use racer_control_wire as wire;
use std::{
    net::SocketAddr,
    os::fd::{AsRawFd, FromRawFd},
    rc::Rc,
    time::Instant,
};

pub use rest_client::Response as HttpResponse;

/// Racer's owner-local filesystem, timer, admission, and readiness adapter.
pub struct ReactorControlIo {
    reactor: Rc<crate::runtime::reactor::Reactor>,
    #[cfg(test)]
    connect_probe: Option<Rc<scenarios::ConnectProbe>>,
}
impl rest_client::Io for ReactorControlIo {
    type Error = Error;
    type Scope = RequestScope;
    type Lease = crate::runtime::admission::ConnectionReservation;

    fn lease(&self) -> Result<Option<Rc<Self::Lease>>> {
        self.reactor
            .reserve_connection(crate::model::ResourceClass::ControlConnection)
            .map(|lease| Some(Rc::new(lease)))
    }
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        lease: Option<Rc<Self::Lease>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            #[cfg(test)]
            if let Some(probe) = &self.connect_probe {
                if probe.suppress(&fd, read, write, &lease, scope)? {
                    let mut wait = scope.clone();
                    if probe.mode == "parent" {
                        wait.deadline.0 = probe.parent_deadline;
                    }
                    return self
                        .sleep(wait.deadline.0 + std::time::Duration::from_millis(1), &wait)
                        .await;
                }
            }
            let interest =
                if read { libc::POLLIN } else { 0 } | if write { libc::POLLOUT } else { 0 };
            self.reactor
                .readiness_with_lease(fd, interest as u32, lease, scope)
                .await?;
            scope.check()
        })
    }
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>> {
        Box::pin(async move {
            #[cfg(test)]
            if let Some(probe) = &self.connect_probe {
                return Ok(probe.addresses.to_vec());
            }
            rest_client::dns::resolve(self, host, port, scope).await
        })
    }
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>> {
        Box::pin(
            async move { super::async_files::read_path(&self.reactor, path, limit, scope).await },
        )
    }
}

impl ReactorControlIo {
    pub fn new(reactor: Rc<crate::runtime::reactor::Reactor>) -> Self {
        Self {
            reactor,
            #[cfg(test)]
            connect_probe: None,
        }
    }
    pub(crate) fn reactor(&self) -> Rc<crate::runtime::reactor::Reactor> {
        self.reactor.clone()
    }
    pub fn sleep<'a>(&'a self, until: Instant, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let duration = until.saturating_duration_since(uring_runtime::environment::now());
            if duration.is_zero() {
                return Ok(());
            }
            #[cfg(test)]
            if uring_runtime::reactor::simulation::Simulation::current().is_some() {
                return std::future::poll_fn(|cx| {
                    scope.check()?;
                    if uring_runtime::environment::now() >= until {
                        std::task::Poll::Ready(Ok(()))
                    } else {
                        cx.waker().wake_by_ref();
                        std::task::Poll::Pending
                    }
                })
                .await;
            }
            let raw = unsafe {
                libc::timerfd_create(
                    libc::CLOCK_MONOTONIC,
                    libc::TFD_CLOEXEC | libc::TFD_NONBLOCK,
                )
            };
            if raw < 0 {
                return Err(Error::Io);
            }
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
            let interval = libc::itimerspec {
                it_interval: libc::timespec {
                    tv_sec: 0,
                    tv_nsec: 0,
                },
                it_value: libc::timespec {
                    tv_sec: duration
                        .as_secs()
                        .try_into()
                        .map_err(|_| Error::InvalidRequest)?,
                    tv_nsec: duration.subsec_nanos() as _,
                },
            };
            if unsafe { libc::timerfd_settime(fd.as_raw_fd(), 0, &interval, std::ptr::null_mut()) }
                != 0
            {
                return Err(Error::Io);
            }
            self.reactor
                .readiness(fd, libc::POLLIN as u32, scope)
                .await?;
            scope.check()
        })
    }
}

pub struct ControlTransport {
    health: Rc<crate::topology::health::LinkHealth>,
    endpoint: crate::model::NodeId,
    inner: rest_client::Transport<ReactorControlIo>,
}
pub struct ControlConnection {
    health: Rc<crate::topology::health::LinkHealth>,
    endpoint: crate::model::NodeId,
    inner: rest_client::Connection<ReactorControlIo>,
}
impl ControlTransport {
    pub fn new(endpoint: ControlEndpoint) -> Self {
        Self {
            health: Rc::new(crate::topology::health::LinkHealth::new(1)),
            endpoint: crate::model::NodeId(endpoint.url.clone()),
            inner: rest_client::Transport::new(rest_client::Config {
                url: endpoint.url,
                trust_bundle: endpoint.trust_bundle,
                max_trust_bundle: wire::MAX_BUNDLE_BYTES,
                max_error_body: wire::MAX_ENROLLMENT_BYTES,
            }),
        }
    }
    pub fn attach_io(&self, io: Rc<ReactorControlIo>) {
        self.inner.attach_io(io);
    }
    pub fn io(&self) -> Result<Rc<ReactorControlIo>> {
        self.inner.io()
    }
    pub fn close_idle(&self) {
        self.inner.close_idle();
    }
    pub fn bootstrap<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ControlConnection> {
        self.connect(None, scope)
    }
    pub fn authenticated<'a>(
        &'a self,
        identity: &'a LocalSigningIdentity,
        scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        self.connect(Some(identity), scope)
    }
    fn connect<'a>(
        &'a self,
        identity: Option<&'a LocalSigningIdentity>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        Box::pin(async move {
            self.health
                .run(
                    &self.endpoint,
                    Box::pin(async move {
                        scope.check()?;
                        if identity.is_some_and(|i| !i.valid_now()) {
                            return Err(Error::Unauthorized);
                        }
                        let identity = identity.map(|i| rest_client::Identity {
                            certificate_chain: i.certificate_chain(),
                            private_key: i.private_key_der(),
                            expires: i.expires_at(),
                        });
                        let inner = self.inner.connect(identity, scope).await?;
                        Ok(ControlConnection {
                            health: self.health.clone(),
                            endpoint: self.endpoint.clone(),
                            inner,
                        })
                    }),
                )
                .await
        })
    }
}
impl ControlConnection {
    pub fn request<'a>(
        self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, HttpResponse> {
        self.request_delta(method, path, token, body, limit, None, scope)
    }
    pub fn request_delta<'a>(
        self,
        method: &'a str,
        path: &'a str,
        token: Option<&'a str>,
        body: &'a [u8],
        limit: usize,
        base: Option<&'a str>,
        scope: &'a RequestScope,
    ) -> Operation<'a, HttpResponse> {
        Box::pin(async move {
            self.health
                .run(
                    &self.endpoint,
                    Box::pin(async move {
                        let method = match method {
                            "GET" => rest_client::Method::Get,
                            "POST" => rest_client::Method::Post,
                            _ => return Err(Error::InvalidRequest),
                        };
                        if base.is_some_and(|b| {
                            b.len() != 64 || !b.bytes().all(|b| b.is_ascii_hexdigit())
                        }) {
                            return Err(Error::InvalidRequest);
                        }
                        self.inner
                            .request(
                                rest_client::Request {
                                    method,
                                    path,
                                    bearer: token,
                                    header: base.map(|base| ("X-Racer-Delta-Base", base)),
                                    body,
                                    limit,
                                },
                                scope,
                            )
                            .await
                    }),
                )
                .await
        })
    }
}

#[cfg(test)]
pub(super) mod scenarios {
    use super::*;
    use crate::control::{enrollment::Enrollment, testing};
    use std::{
        cell::{Cell, RefCell},
        io::{Read, Write},
        sync::Arc,
        time::{Duration, SystemTime},
    };

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
                    assert!(
                        read_head(&mut stream).starts_with(&format!("GET {path} HTTP/1.1\r\n"))
                    );
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

    // Test-only observation and first-writable suppression on the real reactor adapter.
    // No alternative transport, filesystem, admission, or readiness implementation.
    pub(super) struct ConnectProbe {
        calls: Cell<usize>,
        held: RefCell<Option<std::rc::Weak<Descriptor>>>,
        leases: RefCell<Vec<std::rc::Weak<crate::runtime::admission::ConnectionReservation>>>,
        pub(super) addresses: [SocketAddr; 2],
        pub(super) mode: &'static str,
        pub(super) parent_deadline: Instant,
    }
    impl ConnectProbe {
        pub(super) fn suppress(
            &self,
            fd: &Rc<Descriptor>,
            read: bool,
            write: bool,
            lease: &Option<Rc<crate::runtime::admission::ConnectionReservation>>,
            scope: &RequestScope,
        ) -> Result<bool> {
            self.leases
                .borrow_mut()
                .push(Rc::downgrade(lease.as_ref().expect("admission lease")));
            if !read && write {
                let call = self.calls.get();
                self.calls.set(call + 1);
                if call == 0 {
                    *self.held.borrow_mut() = Some(Rc::downgrade(fd));
                    assert!(scope.deadline.0 < self.parent_deadline);
                    if self.mode == "cancel" {
                        scope.cancel()?;
                    }
                    // Suppress a real socket's writable notification, not Simulation::connect.
                    return Ok(true);
                }
                // Inspect the actual OS socket at the public readiness boundary.
                let raw = unsafe { libc::dup(fd.as_raw_fd()) };
                assert!(raw >= 0);
                let socket = unsafe { std::net::TcpStream::from_raw_fd(raw) };
                assert_eq!(socket.peer_addr().unwrap(), self.addresses[1]);
            }
            Ok(false)
        }
    }
    #[test]
    fn real_connect_readiness_blackhole_yields_to_next_address_and_cleans_fds() {
        use std::task::{Context, Poll};
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
            let io = Rc::new(ConnectProbe {
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
            let mut driver = ReactorControlIo::new(reactor.clone());
            driver.connect_probe = Some(io.clone());
            transport.attach_io(Rc::new(driver));
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
        transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
        let scope = testing::scope();
        let get = || {
            testing::drive(
                &r,
                Box::pin(async {
                    transport
                        .authenticated(&identity, &scope)
                        .await?
                        .request("GET", wire::SNAPSHOT_PATH, None, &[], 65536, &scope)
                        .await
                }),
            )
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
        let Some(r) = testing::reactor() else { return };
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
                    read_head(&mut stream)
                        .ends_with(&format!("X-Racer-Delta-Base: {base}\r\n\r\n"))
                );
                stream
                    .write_all(b"HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
                    .unwrap();
                stream.flush().unwrap();
            }
        });
        let transport = ControlTransport::new(endpoint);
        transport.attach_io(Rc::new(ReactorControlIo::new(r.clone())));
        let scope = testing::scope();
        for (method, base) in cases {
            let connection = testing::drive(&r, transport.bootstrap(&scope)).unwrap();
            assert!(matches!(
                testing::drive(
                    &r,
                    connection.request_delta(
                        method,
                        "/enroll",
                        None,
                        &[],
                        1024,
                        base.as_deref(),
                        &scope,
                    )
                ),
                Err(Error::InvalidRequest)
            ));
        }
        for base in ["a".repeat(64), "ABCDEF0123456789".repeat(4)] {
            let connection = testing::drive(&r, transport.bootstrap(&scope)).unwrap();
            assert_eq!(
                testing::drive(
                    &r,
                    connection.request_delta(
                        "GET",
                        "/enroll",
                        None,
                        &[],
                        1024,
                        Some(&base),
                        &scope,
                    )
                )
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
}
use uring_runtime::reactor::Descriptor;
