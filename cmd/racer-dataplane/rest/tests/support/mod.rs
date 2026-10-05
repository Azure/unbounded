//! Shared loopback I/O and certificate fixtures for public and private invariants.
use rest_client::{
    Config, Connection, Error as RestError, Identity, Io, Method, Operation, Request, Scope,
    Transport,
};
use std::{
    io::{Read, Write},
    net::SocketAddr,
    os::fd::AsRawFd,
    path::PathBuf,
    rc::Rc,
    sync::Arc,
    sync::atomic::{AtomicU64, Ordering},
    time::{Duration, Instant, SystemTime},
};
use uring_runtime::{Scope as _, reactor::Descriptor};

/// Fixture errors retain transport categories without an orphan conversion impl.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// A generic transport failure.
    Transport(RestError),
}
impl Error {
    /// Socket or filesystem failure in the blocking fixture.
    pub const IO: Self = Self::Transport(RestError::Io);
    /// Scope deadline or unavailable endpoint.
    pub const UNAVAILABLE: Self = Self::Transport(RestError::Unavailable);
    /// The fixture's file byte bound was exceeded.
    pub const OVERLOADED: Self = Self::Transport(RestError::Overloaded);
}
impl From<RestError> for Error {
    /// Retain the transport's exact error classification.
    fn from(error: RestError) -> Self {
        Self::Transport(error)
    }
}
/// Results returned by blocking fixture operations.
pub type Result<T> = std::result::Result<T, Error>;

/// A monotonic deadline used by loopback operations.
#[derive(Clone)]
pub struct TestScope(Instant);
impl uring_runtime::Scope for TestScope {
    type Error = Error;

    /// Expire operations after the fixture's absolute deadline.
    fn check(&self) -> Result<()> {
        if Instant::now() >= self.0 {
            Err(Error::UNAVAILABLE)
        } else {
            Ok(())
        }
    }
}
impl Scope for TestScope {
    /// Return the fixture's absolute deadline.
    fn deadline(&self) -> Instant {
        self.0
    }

    /// Never extend an existing deadline.
    fn narrowed(&self, until: Instant) -> Self {
        Self(self.0.min(until))
    }
}
impl From<uring_runtime::Error> for Error {
    /// Preserve the original fixture's runtime-to-I/O error mapping.
    fn from(_: uring_runtime::Error) -> Self {
        Self::IO
    }
}
/// Give one fixture operation a bounded loopback deadline.
pub fn scope() -> TestScope {
    TestScope(Instant::now() + Duration::from_secs(10))
}

/// Blocking poll is confined to loopback tests; no io_uring or Racer dependency.
pub struct FixtureIo;
impl Io for FixtureIo {
    type FileBytes = zeroize::Zeroizing<Vec<u8>>;

    type Error = Error;

    type Scope = TestScope;

    type Lease = ();

    /// Loopback fixtures need no admission charge.
    fn lease(&self) -> Result<Option<Rc<()>>> {
        Ok(None)
    }

    /// Read projected trust anew under the configured byte limit.
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a TestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>, Error> {
        Box::pin(async move {
            scope.check()?;
            let file = std::fs::File::open(path).map_err(|_| Error::IO)?;
            let mut bytes = zeroize::Zeroizing::new(Vec::new());
            file.take(limit as u64 + 1)
                .read_to_end(&mut bytes)
                .map_err(|_| Error::IO)?;
            if bytes.len() > limit {
                return Err(Error::OVERLOADED);
            }
            Ok(bytes)
        })
    }

    /// Route fixture names to the loopback server without changing TLS names.
    fn resolve<'a>(
        &'a self,
        _: &'a str,
        port: u16,
        _: &'a TestScope,
    ) -> Operation<'a, Vec<SocketAddr>, Error> {
        Box::pin(async move { Ok(vec![SocketAddr::from(([127, 0, 0, 1], port))]) })
    }

    /// Poll loopback readiness while enforcing the fixture deadline.
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        _: Option<Rc<()>>,
        scope: &'a TestScope,
    ) -> Operation<'a, (), Error> {
        Box::pin(async move {
            loop {
                scope.check()?;
                let mut poll = libc::pollfd {
                    fd: fd.as_raw_fd(),
                    events: (if read { libc::POLLIN } else { 0 })
                        | (if write { libc::POLLOUT } else { 0 }),
                    revents: 0,
                };
                // SAFETY: one initialized poll entry remains live for the call.
                match unsafe { libc::poll(&mut poll, 1, 10) } {
                    n if n > 0 => return Ok(()),
                    n if n < 0 => return Err(Error::IO),
                    _ => (),
                }
            }
        })
    }
}
/// Process-unique fixture directory removed on drop.
pub struct Directory(pub PathBuf);
impl Directory {
    /// Create a directory inside this worktree's target tree.
    pub fn new() -> Self {
        static NEXT: AtomicU64 = AtomicU64::new(0);
        let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../target/rest-tests")
            .join(format!(
                "{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
        std::fs::create_dir_all(&path).unwrap();
        Self(path)
    }
}
impl Drop for Directory {
    /// Clean up trust files, including projected symlinks, after the test.
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}
/// Generate an in-memory Ed25519 certificate authority.
pub fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    (params.self_signed(&key).unwrap(), key)
}
/// Client identity with zeroized private-key storage.
pub struct TestIdentity {
    chain: Vec<Vec<u8>>,

    key: zeroize::Zeroizing<Vec<u8>>,

    expires: SystemTime,
}
impl TestIdentity {
    /// Issue a client-auth certificate from the supplied fixture CA.
    pub fn new(ca: &rcgen::Certificate, ca_key: &rcgen::KeyPair) -> Self {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        let cert = params.signed_by(&key, ca, ca_key).unwrap();
        Self {
            chain: vec![cert.der().to_vec()],
            key: zeroize::Zeroizing::new(key.serialize_der()),
            expires: SystemTime::now() + Duration::from_secs(86400),
        }
    }
    /// Borrow the certificate and key without copying sensitive bytes.
    fn borrowed(&self) -> Identity<'_> {
        Identity {
            certificate_chain: &self.chain,
            private_key: &self.key,
            expires: self.expires,
        }
    }
}
/// Build standard bounded transport configuration for loopback tests.
pub fn config(url: String, trust_bundle: PathBuf) -> Config {
    Config {
        url,
        trust_bundle,
        max_trust_bundle: 1024 * 1024,
        max_error_body: 65536,
    }
}
/// Test-only convenience methods, not additions to the production API.
pub trait TestTransport {
    /// Connect with a borrowed fixture identity.
    fn authenticated<'a>(
        &'a self,
        identity: &'a TestIdentity,
        scope: &'a TestScope,
    ) -> Operation<'a, Connection<FixtureIo>, Error>;

    /// Connect without client authentication.
    fn bootstrap<'a>(&'a self, scope: &'a TestScope)
    -> Operation<'a, Connection<FixtureIo>, Error>;
}
impl TestTransport for Transport<FixtureIo> {
    /// Connect using the fixture's borrowed client certificate and key.
    fn authenticated<'a>(
        &'a self,
        identity: &'a TestIdentity,
        scope: &'a TestScope,
    ) -> Operation<'a, Connection<FixtureIo>, Error> {
        self.connect(Some(identity.borrowed()), scope)
    }

    /// Connect without client authentication or pooling.
    fn bootstrap<'a>(
        &'a self,
        scope: &'a TestScope,
    ) -> Operation<'a, Connection<FixtureIo>, Error> {
        self.connect(None, scope)
    }
}
/// Construct a bodyless fixture request with a caller-selected response limit.
pub fn request<'a>(
    method: Method,
    path: &'a str,
    bearer: Option<&'a str>,
    limit: usize,
) -> Request<'a> {
    Request {
        method,
        path,
        bearer,
        header: None,
        body: &[],
        limit,
    }
}

/// Match operator/components/racer/tls.go with fresh in-memory P-256 keys,
/// distinct CA subjects, unconstrained signing CAs, and server-auth leaves.
pub struct WeeklyCertificates {
    pub roots: Vec<rcgen::Certificate>,

    pub crosses: Vec<rcgen::Certificate>,

    pub servers: Vec<Arc<rustls::ServerConfig>>,

    pub leaf_only_servers: Vec<Arc<rustls::ServerConfig>>,
}
impl WeeklyCertificates {
    /// Build four generations retaining at most two compatibility bridges.
    pub fn new() -> Self {
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
                // crossSign preserves subject/key/serial and caps validity at the parent.
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
            // Reissue each snapshot's leaf now without changing rustls's wall clock.
            leaf.not_before = (now - Duration::from_secs(3600)).into();
            leaf.not_after = root.params().not_after.min((now + day * 14).into());
            leaf.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            leaf.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
            leaf.use_authority_key_identifier_extension = true;
            let leaf_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
            let cert = leaf.signed_by(&leaf_key, &root, &key).unwrap();
            let mut chain = vec![cert.der().clone()];
            // Omit the self-signed root and retain at most two bridges, newest first.
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

/// A join-on-drop server with bounded accept, socket I/O, and request headers.
pub struct RotationServer {
    port: u16,

    config: Arc<std::sync::Mutex<Arc<rustls::ServerConfig>>>,

    stop: Arc<std::sync::atomic::AtomicBool>,

    thread: Option<std::thread::JoinHandle<()>>,
}
impl RotationServer {
    /// Start serving connection identifiers using a replaceable TLS config.
    pub fn new(config: Arc<rustls::ServerConfig>) -> Self {
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
                            // Certificate rejection and idle disposal deliberately close TLS.
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

    /// Select certificates for subsequent connections.
    pub fn rotate(&self, config: Arc<rustls::ServerConfig>) {
        *self.config.lock().unwrap() = config;
    }

    /// Attach loopback I/O while retaining the controller DNS name for TLS.
    pub fn transport(&self, trust_bundle: PathBuf) -> Transport<FixtureIo> {
        let transport = Transport::new(config(
            format!("https://racer-controller.custom-system.svc:{}", self.port),
            trust_bundle,
        ));
        transport.attach_io(Rc::new(FixtureIo));
        transport
    }
}
impl Drop for RotationServer {
    /// Stop and join the server even when an assertion unwinds.
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        let result = self.thread.take().unwrap().join();
        if !std::thread::panicking() {
            result.unwrap();
        }
    }
}

/// Return the serving connection identifier from a bounded public request.
pub fn rotation_request(
    transport: &Transport<FixtureIo>,
    identity: Option<&TestIdentity>,
) -> Result<Vec<u8>> {
    let scope = scope();
    futures::executor::block_on(async {
        let connection = if let Some(identity) = identity {
            transport.authenticated(identity, &scope).await?
        } else {
            transport.bootstrap(&scope).await?
        };
        let mut response = connection
            .request(request(Method::Get, "/snapshot", None, 1024), &scope)
            .await?;
        assert_eq!(response.status, 200);
        Ok(std::mem::take(&mut response.body))
    })
}
