use super::*;
use std::{
    os::fd::AsRawFd,
    path::PathBuf,
    sync::atomic::{AtomicU64, Ordering},
};

#[derive(Clone)]
pub struct TestScope(Instant);
impl uring_runtime::Scope for TestScope {
    type Error = Error;
    fn check(&self) -> Result<()> {
        if Instant::now() >= self.0 {
            Err(Error::Unavailable)
        } else {
            Ok(())
        }
    }
}
impl Scope for TestScope {
    fn deadline(&self) -> Instant {
        self.0
    }
    fn narrowed(&self, until: Instant) -> Self {
        Self(self.0.min(until))
    }
}
impl From<uring_runtime::Error> for Error {
    fn from(_: uring_runtime::Error) -> Self {
        Self::Io
    }
}
pub fn scope() -> TestScope {
    TestScope(Instant::now() + Duration::from_secs(10))
}

/// Blocking poll is confined to loopback tests; no io_uring or Racer dependency.
pub struct FixtureIo;
impl Io for FixtureIo {
    type Error = Error;
    type Scope = TestScope;
    type Lease = ();
    fn lease(&self) -> Result<Option<Rc<()>>> {
        Ok(None)
    }
    fn read_file<'a>(
        &'a self,
        path: &'a std::path::Path,
        limit: usize,
        scope: &'a TestScope,
    ) -> Operation<'a, zeroize::Zeroizing<Vec<u8>>, Error> {
        Box::pin(async move {
            scope.check()?;
            let file = std::fs::File::open(path).map_err(|_| Error::Io)?;
            let mut bytes = zeroize::Zeroizing::new(Vec::new());
            file.take(limit as u64 + 1)
                .read_to_end(&mut bytes)
                .map_err(|_| Error::Io)?;
            if bytes.len() > limit {
                return Err(Error::Overloaded);
            }
            Ok(bytes)
        })
    }
    fn resolve<'a>(
        &'a self,
        _: &'a str,
        port: u16,
        _: &'a TestScope,
    ) -> Operation<'a, Vec<SocketAddr>, Error> {
        Box::pin(async move { Ok(vec![SocketAddr::from(([127, 0, 0, 1], port))]) })
    }
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
                    n if n < 0 => return Err(Error::Io),
                    _ => (),
                }
            }
        })
    }
}
pub struct Directory(pub PathBuf);
impl Directory {
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
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}
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
pub struct TestIdentity {
    chain: Vec<Vec<u8>>,
    key: zeroize::Zeroizing<Vec<u8>>,
    expires: SystemTime,
}
impl TestIdentity {
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
    fn borrowed(&self) -> Identity<'_> {
        Identity {
            certificate_chain: &self.chain,
            private_key: &self.key,
            expires: self.expires,
        }
    }
}
pub fn config(url: String, trust_bundle: PathBuf) -> Config {
    Config {
        url,
        trust_bundle,
        max_trust_bundle: 1024 * 1024,
        max_error_body: 65536,
    }
}
impl Transport<FixtureIo> {
    pub fn authenticated<'a>(
        &'a self,
        identity: &'a TestIdentity,
        scope: &'a TestScope,
    ) -> Operation<'a, Connection<FixtureIo>, Error> {
        self.connect(Some(identity.borrowed()), scope)
    }
    pub fn bootstrap<'a>(
        &'a self,
        scope: &'a TestScope,
    ) -> Operation<'a, Connection<FixtureIo>, Error> {
        self.connect(None, scope)
    }
}
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
