use super::*;
mod testing;
use testing::{FixtureIo, TestIdentity};
type ControlTransport = Transport<FixtureIo>;
type LocalSigningIdentity = TestIdentity;
const SNAPSHOT_PATH: &str = "/snapshot";
const BOOTSTRAP_PATH: &str = "/enroll";

#[test]
fn request_heads_validate_and_bound_credentials_before_copying() {
    let base = "a".repeat(64);
    let head = request_head(
        "controller:443",
        Method::Post,
        "/enroll",
        Some("secret"),
        42,
        Some(("X-Delta-Base", &base)),
    )
    .unwrap();
    assert!(head.starts_with("POST /enroll HTTP/1.1\r\nHost: controller:443\r\n"));
    assert!(head.contains("Content-Length: 42\r\n"));
    assert!(head.contains("Authorization: Bearer secret\r\n"));
    assert!(head.ends_with(&format!("X-Delta-Base: {base}\r\n\r\n")));
    for (method, path, token, base) in [
        (Method::Get, "enroll", None, None),
        (Method::Get, "/bad\r\n", None, None),
        (Method::Get, "/enroll", Some("bad\r\n"), None),
        (
            Method::Get,
            "/enroll",
            None,
            Some(("X-Delta-Base", "bad\r\n")),
        ),
    ] {
        assert_eq!(
            request_head("controller", method, path, token, 0, base),
            Err(Error::InvalidRequest)
        );
    }
    assert_eq!(
        request_head("controller\r\n", Method::Get, "/", None, 0, None),
        Err(Error::InvalidRequest)
    );
    assert_eq!(
        request_head(
            "controller",
            Method::Get,
            "/",
            Some(&"a".repeat(16384)),
            0,
            None
        ),
        Err(Error::Overloaded)
    );
    assert_eq!(
        request_head(
            "controller",
            Method::Get,
            &format!("/{}", "a".repeat(16384)),
            None,
            0,
            None
        ),
        Err(Error::Overloaded)
    );
}

/// Each group must use exactly one TLS connection. EOF after each group
/// proves that failure/retirement closes the socket rather than just hiding it.
fn scripted_server(
    d: &testing::Directory,
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    groups: Vec<Vec<(String, u16, Vec<u8>)>>,
) -> (Config, std::thread::JoinHandle<()>) {
    let mut params = rcgen::CertificateParams::new(vec!["127.0.0.1".into()]).unwrap();
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let cert = params.signed_by(&key, ca, ca_key).unwrap();
    let trust = d.0.join("trust");
    std::fs::write(&trust, ca.pem()).unwrap();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        provider.clone(),
    )
    .build()
    .unwrap();
    let config = Arc::new(
        rustls::ServerConfig::builder_with_provider(provider)
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_client_cert_verifier(verifier)
            .with_single_cert(
                vec![cert.der().clone()],
                rustls::pki_types::PrivatePkcs8KeyDer::from(key.serialize_der()).into(),
            )
            .unwrap(),
    );
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let endpoint = testing::config(format!("https://{}", listener.local_addr().unwrap()), trust);
    let server = std::thread::spawn(move || {
        for group in groups {
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
            let mut stream = rustls::StreamOwned::new(
                rustls::ServerConnection::new(config.clone()).unwrap(),
                socket,
            );
            let mut disconnected = false;
            for (path, status, body) in group {
                let mut request = Vec::new();
                while !request.ends_with(b"\r\n\r\n") {
                    assert!(request.len() < 16384);
                    let mut byte = [0];
                    stream.read_exact(&mut byte).unwrap();
                    request.push(byte[0]);
                }
                assert!(
                    String::from_utf8(request)
                        .unwrap()
                        .starts_with(&format!("GET {path} HTTP/1.1\r\n"))
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
            if disconnected {
                continue;
            }
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
    });
    (endpoint, server)
}

#[test]
fn authenticated_age_jitter_is_bounded() {
    assert_eq!(authenticated_age(0), Duration::from_secs(240));
    assert_eq!(authenticated_age(60_000), Duration::from_secs(300));
    for random in [1, 30_000, 60_001, u64::MAX] {
        let age = authenticated_age(random);
        assert!((Duration::from_secs(240)..=Duration::from_secs(300)).contains(&age));
    }
    assert_ne!(authenticated_age(1), authenticated_age(30_000));
}

#[test]
fn pooled_connections_retire_between_requests_and_discard_failures() {
    let d = testing::Directory::new();
    let (ca, key) = testing::ca();
    let identity = TestIdentity::new(&ca, &key);
    let response = |status| {
        (
            SNAPSHOT_PATH.to_owned(),
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
        vec![
            vec![response(200), response(204)], // Reuse before age boundary.
            vec![response(204)],                // In-flight response survives retirement.
            vec![response(200)],                // Idle timeout still applies.
            vec![response(200)],                // Identity/trust epoch still applies.
            vec![response(200), response(429)],
            vec![response(200), response(503)],
            vec![response(200)], // Invalid local request discards the pool lease.
            vec![response(200), response(0)], // Backend disconnect during request.
            vec![response(204)],
        ],
    );
    let transport = ControlTransport::new(endpoint);
    transport.attach_io(Rc::new(FixtureIo));
    let scope = testing::scope();
    let get = || {
        futures::executor::block_on(async {
            transport
                .authenticated(&identity, &scope)
                .await
                .unwrap()
                .request(
                    testing::request(Method::Get, SNAPSHOT_PATH, None, 65536),
                    &scope,
                )
                .await
                .unwrap()
        })
    };
    assert_eq!(get().status, 200);
    let retire_at = transport.idle.borrow().as_ref().unwrap().retire_at.unwrap();
    assert!(retire_at > Instant::now() + Duration::from_secs(239));
    assert!(retire_at <= Instant::now() + Duration::from_secs(300));
    assert_eq!(get().status, 204);
    assert_eq!(
        transport.idle.borrow().as_ref().unwrap().retire_at,
        Some(retire_at)
    );
    transport.idle.borrow_mut().as_mut().unwrap().retire_at = Some(Instant::now());
    // Checkout retires the first connection. Move the next connection across
    // its age boundary after checkout: active I/O must not consult that age.
    let mut connection =
        futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
    connection.retire_at = Some(Instant::now());
    assert_eq!(connection.check(&scope), Ok(()));
    let expiry = connection.expires;
    connection.expires = Some(SystemTime::now() - Duration::from_secs(1));
    assert_eq!(connection.check(&scope), Err(Error::Unauthorized));
    connection.expires = expiry;
    let result = futures::executor::block_on(connection.request(
        testing::request(Method::Get, SNAPSHOT_PATH, None, 65536),
        &scope,
    ))
    .unwrap();
    assert_eq!(result.status, 204);
    assert!(transport.idle.borrow().is_none());
    assert_eq!(get().status, 200);
    transport.idle.borrow_mut().as_mut().unwrap().idle_since =
        Instant::now() - Duration::from_secs(20);
    assert_eq!(get().status, 200);
    transport.idle.borrow_mut().as_mut().unwrap().epoch = [0; 32];
    for status in [429, 503] {
        assert_eq!(get().status, 200);
        assert_eq!(get().status, status);
        assert!(transport.idle.borrow().is_none());
    }
    assert_eq!(get().status, 200);
    let connection =
        futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
    assert!(matches!(
        futures::executor::block_on(connection.request(
            testing::request(Method::Get, "invalid", None, 65536),
            &scope
        )),
        Err(Error::InvalidRequest)
    ));
    assert!(transport.idle.borrow().is_none());
    assert_eq!(get().status, 200);
    let connection =
        futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
    assert!(matches!(
        futures::executor::block_on(connection.request(
            testing::request(Method::Get, SNAPSHOT_PATH, None, 65536),
            &scope
        )),
        Err(Error::Io)
    ));
    assert!(transport.idle.borrow().is_none());
    assert_eq!(get().status, 204);
    transport.close_idle();
    server.join().unwrap();
}

#[test]
fn real_server_auth_and_mutual_tls_chunked_response() {
    tls_fixture(false);
}

// Match operator/components/racer/tls.go: fresh P-256 keys, distinct CA
// subjects, unconstrained cert-signing CAs and server-auth leaves. Keys live
// only in memory. These are rcgen equivalents, not Go-generated fixtures.
struct WeeklyCertificates {
    roots: Vec<rcgen::Certificate>,
    crosses: Vec<rcgen::Certificate>,
    servers: Vec<Arc<rustls::ServerConfig>>,
    leaf_only_servers: Vec<Arc<rustls::ServerConfig>>,
}
impl WeeklyCertificates {
    fn new() -> Self {
        let day = Duration::from_secs(86400);
        let now = SystemTime::now();
        let mut roots: Vec<rcgen::Certificate> = Vec::new();
        let mut keys = Vec::new();
        let mut crosses = Vec::new();
        let mut servers = Vec::new();
        let mut leaf_only_servers = Vec::new();
        for generation in 0..4 {
            let created = now - day * (7 * (3 - generation) as u32);
            let mut ca = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            ca.distinguished_name = rcgen::DistinguishedName::new();
            ca.distinguished_name.push(
                rcgen::DnType::CommonName,
                format!("racer-serving-ca-{generation}"),
            );
            ca.serial_number = Some((generation as u64 + 1).into());
            ca.not_before = (created - Duration::from_secs(3600)).into();
            ca.not_after = (created + day * 28).into();
            ca.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            ca.key_usages = vec![
                rcgen::KeyUsagePurpose::KeyCertSign,
                rcgen::KeyUsagePurpose::CrlSign,
            ];
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let root = ca.clone().self_signed(&key).unwrap();
            if generation > 0 {
                // crossSign preserves the child's subject/key/serial and
                // caps validity at the signing parent's expiration.
                ca.not_after = ca.not_after.min(roots[generation - 1].params().not_after);
                ca.use_authority_key_identifier_extension = true;
                crosses.push(
                    ca.signed_by(&key, &roots[generation - 1], &keys[generation - 1])
                        .unwrap(),
                );
            }
            let mut leaf = rcgen::CertificateParams::new(vec![
                "racer-controller.custom-system.svc".into(),
                "racer-controller.custom-system.svc.cluster.local".into(),
            ])
            .unwrap();
            leaf.distinguished_name = rcgen::DistinguishedName::new();
            leaf.distinguished_name.push(
                rcgen::DnType::CommonName,
                "racer-controller.custom-system.svc",
            );
            leaf.is_ca = rcgen::IsCa::ExplicitNoCa;
            // Reissue each snapshot's leaf now so all server generations
            // can be exercised without changing rustls's wall clock.
            leaf.not_before = (now - Duration::from_secs(3600)).into();
            leaf.not_after = root.params().not_after.min((now + day * 14).into());
            leaf.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            leaf.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
            leaf.use_authority_key_identifier_extension = true;
            let leaf_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let cert = leaf.signed_by(&leaf_key, &root, &key).unwrap();
            let mut chain = vec![cert.der().clone()];
            // servingChain omits the self-signed root and retains at most
            // two compatibility bridges, newest first.
            chain.extend(crosses.iter().rev().take(2).map(|c| c.der().clone()));
            let config = |chain| {
                Arc::new(
                    rustls::ServerConfig::builder_with_provider(Arc::new(
                        rustls::crypto::ring::default_provider(),
                    ))
                    .with_safe_default_protocol_versions()
                    .unwrap()
                    .with_no_client_auth()
                    .with_single_cert(
                        chain,
                        rustls::pki_types::PrivatePkcs8KeyDer::from(leaf_key.serialize_der())
                            .into(),
                    )
                    .unwrap(),
                )
            };
            leaf_only_servers.push(config(vec![cert.der().clone()]));
            servers.push(config(chain));
            roots.push(root);
            keys.push(key);
        }
        Self {
            roots,
            crosses,
            servers,
            leaf_only_servers,
        }
    }
}

// A join-on-drop server also cleans up after assertion failures. Accept is
// nonblocking, every socket operation has a timeout, and request headers
// have both a size bound and an absolute deadline.
struct RotationServer {
    port: u16,
    config: Arc<std::sync::Mutex<Arc<rustls::ServerConfig>>>,
    stop: Arc<std::sync::atomic::AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}
impl RotationServer {
    fn new(config: Arc<rustls::ServerConfig>) -> Self {
        use std::sync::atomic::Ordering;
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let port = listener.local_addr().unwrap().port();
        let config = Arc::new(std::sync::Mutex::new(config));
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let (selected, stopped) = (config.clone(), stop.clone());
        let thread = std::thread::spawn(move || {
            let mut accepted = 0;
            while !stopped.load(Ordering::SeqCst) {
                let socket = match listener.accept() {
                    Ok((socket, _)) => socket,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        std::thread::sleep(Duration::from_millis(1));
                        continue;
                    }
                    Err(e) => panic!("rotation fixture accept: {e}"),
                };
                accepted += 1;
                socket
                    .set_read_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                socket
                    .set_write_timeout(Some(Duration::from_secs(2)))
                    .unwrap();
                let tls = rustls::ServerConnection::new(selected.lock().unwrap().clone()).unwrap();
                let mut stream = rustls::StreamOwned::new(tls, socket);
                'connection: while !stopped.load(Ordering::SeqCst) {
                    let deadline = Instant::now() + Duration::from_secs(5);
                    let mut request = Vec::new();
                    while !request.ends_with(b"\r\n\r\n") {
                        if Instant::now() >= deadline || request.len() >= 16384 {
                            break 'connection;
                        }
                        let mut byte = [0];
                        if stream.read_exact(&mut byte).is_err() {
                            // Certificate rejection and discarded idle
                            // connections deliberately close the socket.
                            break 'connection;
                        }
                        request.push(byte[0]);
                    }
                    let body = format!("{{\"connection\":{accepted}}}");
                    let response = format!(
                        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{body}",
                        body.len()
                    );
                    if stream.write_all(response.as_bytes()).is_err() || stream.flush().is_err() {
                        break;
                    }
                }
            }
        });
        Self {
            port,
            config,
            stop,
            thread: Some(thread),
        }
    }
    fn rotate(&self, config: Arc<rustls::ServerConfig>) {
        *self.config.lock().unwrap() = config;
    }
    fn transport(&self, trust_bundle: std::path::PathBuf) -> ControlTransport {
        let transport = ControlTransport::new(testing::config(
            format!("https://racer-controller.custom-system.svc:{}", self.port),
            trust_bundle,
        ));
        transport.attach_io(Rc::new(FixtureIo));
        transport
    }
}
impl Drop for RotationServer {
    fn drop(&mut self) {
        self.stop.store(true, std::sync::atomic::Ordering::SeqCst);
        let result = self.thread.take().unwrap().join();
        if !std::thread::panicking() {
            result.unwrap();
        }
    }
}
fn rotation_request(
    transport: &ControlTransport,
    identity: Option<&LocalSigningIdentity>,
) -> Result<Vec<u8>> {
    let scope = testing::scope();
    futures::executor::block_on(async {
        let connection = if let Some(identity) = identity {
            transport.authenticated(identity, &scope).await?
        } else {
            transport.bootstrap(&scope).await?
        };
        let mut response = connection
            .request(
                testing::request(Method::Get, SNAPSHOT_PATH, None, 1024),
                &scope,
            )
            .await?;
        assert_eq!(response.status, 200);
        Ok(std::mem::take(&mut response.body))
    })
}

#[test]
fn weekly_cross_signed_chains_accept_retained_anchors_only() {
    let certs = WeeklyCertificates::new();
    let d = testing::Directory::new();
    let trust = d.0.join("trust.pem");
    let server = RotationServer::new(certs.servers[1].clone());
    let transport = server.transport(trust.clone());
    let (unrelated, _) = testing::ca();
    for generation in 1..4 {
        server.rotate(certs.servers[generation].clone());
        let first_retained = generation.saturating_sub(2);
        let overlap = certs.roots[first_retained..=generation]
            .iter()
            .rev()
            .map(|c| c.pem())
            .collect::<String>();
        for (label, bundle, accepted) in [
            ("old-only", certs.roots[generation - 1].pem(), true),
            ("current-only", certs.roots[generation].pem(), true),
            ("overlap", overlap, true),
            ("oldest-only", certs.roots[0].pem(), generation <= 2),
            (
                "new-cross-anchor",
                certs.crosses[generation - 1].pem(),
                true,
            ),
            ("unrelated", unrelated.pem(), false),
        ] {
            std::fs::write(&trust, bundle).unwrap();
            let result = rotation_request(&transport, None);
            if accepted {
                assert!(result.is_ok(), "week {generation}, {label}: {result:?}");
            } else {
                assert!(
                    matches!(result, Err(Error::Unauthorized)),
                    "week {generation}, {label}: {result:?}"
                );
            }
        }
    }
    // Week two: leaf -> CA2-by-CA1 -> CA1-by-CA0. A public
    // intermediate is also a valid anchor, without its self-signed root.
    server.rotate(certs.servers[2].clone());
    std::fs::write(&trust, certs.crosses[0].pem()).unwrap();
    assert!(rotation_request(&transport, None).is_ok());
    // Removing the compatibility bridges must break old-only trust, but
    // the exact same leaf still verifies directly under the current root.
    server.rotate(certs.leaf_only_servers[2].clone());
    std::fs::write(&trust, certs.roots[0].pem()).unwrap();
    assert!(matches!(
        rotation_request(&transport, None),
        Err(Error::Unauthorized)
    ));
    std::fs::write(&trust, certs.roots[2].pem()).unwrap();
    assert!(rotation_request(&transport, None).is_ok());
}

#[test]
fn weekly_projected_trust_reload_invalidates_idle_transport() {
    let certs = WeeklyCertificates::new();
    let d = testing::Directory::new();
    let trust = d.0.join("ca.crt");
    std::os::unix::fs::symlink("..data/ca.crt", &trust).unwrap();
    let project = |revision: usize, bundle: String| {
        let directory = format!("revision-{revision}");
        std::fs::create_dir(d.0.join(&directory)).unwrap();
        std::fs::write(d.0.join(&directory).join("ca.crt"), bundle).unwrap();
        std::os::unix::fs::symlink(directory, d.0.join("..data-next")).unwrap();
        std::fs::rename(d.0.join("..data-next"), d.0.join("..data")).unwrap();
    };
    let (peer_ca, peer_key) = testing::ca();
    let identity = TestIdentity::new(&peer_ca, &peer_key);
    let server = RotationServer::new(certs.servers[1].clone());
    let transport = server.transport(trust);
    project(0, certs.roots[0].pem());
    let old = rotation_request(&transport, Some(&identity)).unwrap();
    assert!(transport.idle.borrow().is_some());
    assert_eq!(rotation_request(&transport, Some(&identity)).unwrap(), old);
    // Projected overlap must still accept the previous live server. The
    // changed bytes must discard its cached TLS connection, not reuse it.
    project(
        1,
        certs.roots[2].pem() + &certs.roots[1].pem() + &certs.roots[0].pem(),
    );
    let overlap = rotation_request(&transport, Some(&identity)).unwrap();
    assert_ne!(overlap, old);
    assert_eq!(
        rotation_request(&transport, Some(&identity)).unwrap(),
        overlap
    );
    // Removing old trust while the old server is still live must fail,
    // rather than bypass verification by recycling its idle connection.
    project(2, certs.roots[2].pem());
    assert!(matches!(
        rotation_request(&transport, Some(&identity)),
        Err(Error::Unauthorized)
    ));
    assert!(transport.idle.borrow().is_none());
    server.rotate(certs.servers[2].clone());
    let current = rotation_request(&transport, Some(&identity)).unwrap();
    assert_ne!(current, overlap);
    assert_eq!(
        rotation_request(&transport, Some(&identity)).unwrap(),
        current
    );
    project(3, peer_ca.pem());
    assert!(matches!(
        rotation_request(&transport, Some(&identity)),
        Err(Error::Unauthorized)
    ));
    assert!(transport.idle.borrow().is_none());
    // Recover using only the oldest root through both cross certificates,
    // still without recreating or reattaching the transport.
    project(4, certs.roots[0].pem());
    assert!(rotation_request(&transport, Some(&identity)).is_ok());
    transport.close_idle();
}

#[test]
fn rejects_trailing_tls_plaintext_before_connection_reuse() {
    tls_fixture(true);
}
fn tls_fixture(trailing: bool) {
    let d = testing::Directory::new();
    let (ca, ca_key) = testing::ca();
    let mut params =
        rcgen::CertificateParams::new(vec!["localhost".into(), "127.0.0.1".into()]).unwrap();
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
    let server_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let cert = params.signed_by(&server_key, &ca, &ca_key).unwrap();
    let trust = d.0.join("trust.pem");
    std::fs::write(&trust, ca.pem()).unwrap();
    let identity = TestIdentity::new(&ca, &ca_key);
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let port = listener.local_addr().unwrap().port();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(ca.der().clone()).unwrap();
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(roots),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .allow_unauthenticated()
    .build()
    .unwrap();
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .unwrap()
    .with_client_cert_verifier(verifier)
    .with_single_cert(
        vec![cert.der().clone()],
        rustls::pki_types::PrivatePkcs8KeyDer::from(server_key.serialize_der()).into(),
    )
    .unwrap();
    let server = std::thread::spawn(move || {
        for mutual in [false, true] {
            let (socket, _) = listener.accept().unwrap();
            socket
                .set_read_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            socket
                .set_write_timeout(Some(Duration::from_secs(10)))
                .unwrap();
            let tls = rustls::ServerConnection::new(Arc::new(config.clone())).unwrap();
            let mut stream = rustls::StreamOwned::new(tls, socket);
            let mut request = Vec::new();
            let mut b = [0; 1];
            while !request.ends_with(b"\r\n\r\n") {
                stream.read_exact(&mut b).unwrap();
                request.push(b[0]);
            }
            assert_eq!(stream.conn.peer_certificates().is_some(), mutual);
            if !mutual {
                assert!(
                    String::from_utf8(request)
                        .unwrap()
                        .contains("Authorization: Bearer fixture.token")
                );
            }
            if trailing {
                // The framed response ends exactly at receive's 16 KiB
                // boundary, leaving the extra byte in rustls's reader.
                let head = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 16310\r\n\r\n";
                let mut response = head.to_vec();
                response.resize(head.len() + 16310, b' ');
                response.push(b'x');
                stream.write_all(&response).unwrap();
            } else {
                stream.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\n{}\r\n0\r\n\r\n").unwrap();
            }
            stream.flush().unwrap();
        }
    });
    let transport =
        ControlTransport::new(testing::config(format!("https://127.0.0.1:{port}"), trust));
    transport.attach_io(Rc::new(FixtureIo));
    let scope = testing::scope();
    let run = |future: Operation<'_, Response, Error>| futures::executor::block_on(future);
    let first = run(Box::pin(async {
        let connection = transport.bootstrap(&scope).await?;
        assert_eq!(
            connection.tls.protocol_version(),
            Some(rustls::ProtocolVersion::TLSv1_3)
        );
        connection
            .request(
                testing::request(Method::Post, BOOTSTRAP_PATH, Some("fixture.token"), 65536),
                &scope,
            )
            .await
    }));
    if trailing {
        assert!(matches!(first, Err(Error::InvalidRequest)));
    } else {
        assert_eq!(first.unwrap().body, b"{}");
    }
    let second = run(Box::pin(async {
        let connection = transport.authenticated(&identity, &scope).await?;
        assert_eq!(
            connection.tls.protocol_version(),
            Some(rustls::ProtocolVersion::TLSv1_3)
        );
        connection
            .request(
                testing::request(Method::Get, SNAPSHOT_PATH, None, 65536),
                &scope,
            )
            .await
    }));
    if trailing {
        assert!(matches!(second, Err(Error::InvalidRequest)));
    } else {
        let second = second.unwrap();
        assert_eq!(second.status, 200);
        assert_eq!(second.body, b"{}");
    }
    server.join().unwrap();
}

#[test]
fn rejects_ambiguous_framing_and_insecure_urls() {
    for url in [
        "http://localhost",
        "https://user@localhost",
        "https://localhost/path",
        "https://localhost:0",
    ] {
        assert!(endpoint(url).is_err());
    }
    for bytes in [
        &b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nContent-Length: 2\r\n\r\n"[..],
        &b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nTransfer-Encoding: chunked\r\n\r\n"[..],
        &b"HTTP/1.1 204 No Content\r\nContent-Length: 1\r\n\r\n"[..],
        &b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 999999\r\n\r\n"[..],
    ] { assert!(parse_head(bytes,10,65536).is_err()); }
}
