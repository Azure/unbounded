// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Process-local leaf keys, projected trust, and worker installation barriers.
use crate::tls::{
    self, ExpectedPeer, PeerIdentity, TlsContext, TlsProgress, TlsSession, TrustBundle,
};
use std::{
    collections::BTreeMap,
    io::{self, Read, Write},
    net::{SocketAddr, TcpStream},
    os::fd::AsRawFd,
    path::PathBuf,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

fn invalid(message: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn unix() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}
fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

pub struct Snapshot {
    pub revision: u64,
    pub generation: u64,
    pub digest: String,
    pub issuer: String,
    pub context: Arc<TlsContext>,
    pub expires_unix: u64,
}
struct State {
    current: Arc<Snapshot>,
    installed: BTreeMap<usize, (u64, usize)>,
    error: Option<String>,
    connections: BTreeMap<u64, usize>,
}
pub struct Provider {
    state: Mutex<State>,
    workers: usize,
    identity: PeerIdentity,
    server_name: String,
}
impl Provider {
    #[cfg(test)]
    pub(crate) fn for_test(
        identity: PeerIdentity,
        context: impl Into<Arc<TlsContext>>,
    ) -> Arc<Self> {
        Arc::new(Self {
            state: Mutex::new(State {
                current: Arc::new(Snapshot {
                    revision: 1,
                    generation: 1,
                    digest: "test-context".into(),
                    issuer: "test-issuer".into(),
                    context: context.into(),
                    expires_unix: crate::environment::wall()
                        .duration_since(UNIX_EPOCH)
                        .unwrap()
                        .as_secs()
                        + 3600,
                }),
                installed: BTreeMap::new(),
                error: None,
                connections: BTreeMap::new(),
            }),
            workers: 0,
            identity,
            server_name: "localhost".into(),
        })
    }
    #[cfg(test)]
    pub(crate) fn fixture_from_env(
        workers: usize,
        updates: &Arc<super::Updates>,
    ) -> io::Result<Arc<Self>> {
        let dir = PathBuf::from(
            std::env::var_os("RACER_TLS_DIR").ok_or_else(|| invalid("missing test TLS fixture"))?,
        );
        let trust = TrustBundle::load(&dir, None)?;
        let certificate = std::fs::read(dir.join("tls.crt"))?;
        let key = zeroize::Zeroizing::new(std::fs::read(dir.join("tls.key"))?);
        let identity = PeerIdentity::new(
            &std::env::var("RACER_UNIVERSE").map_err(invalid)?,
            &std::env::var("RACER_NODE").map_err(invalid)?,
            &std::env::var("RACER_POD_UID").map_err(invalid)?,
        )?;
        let info = tls::validate_leaf(&trust, &certificate, &key, &identity)?;
        let context = Arc::new(TlsContext::new(&trust, &certificate, &key)?);
        let provider = Arc::new(Self {
            state: Mutex::new(State {
                current: Arc::new(Snapshot {
                    revision: 1,
                    generation: trust.generation,
                    digest: hex(&trust.digest),
                    issuer: info.issuer,
                    expires_unix: info.expires_unix,
                    context,
                }),
                installed: BTreeMap::new(),
                error: None,
                connections: BTreeMap::new(),
            }),
            workers,
            identity,
            server_name: "localhost".into(),
        });
        updates.set_credentials(provider.clone());
        Ok(provider)
    }
    pub fn current(&self) -> Arc<Snapshot> {
        self.state.lock().unwrap().current.clone()
    }
    pub fn identity(&self) -> &PeerIdentity {
        &self.identity
    }
    pub fn installed(&self, worker: usize, revision: u64, old_connections: usize) {
        let mut state = self.state.lock().unwrap();
        if worker < self.workers && revision == state.current.revision {
            state.installed.insert(worker, (revision, old_connections));
        }
    }
    pub fn headers(&self) -> Vec<(&'static str, String)> {
        let state = self.state.lock().unwrap();
        let snapshot = &state.current;
        let ready = state.installed.len() == self.workers
            && state
                .installed
                .values()
                .all(|(r, _)| *r == snapshot.revision);
        let old = state.installed.values().map(|(_, n)| *n).sum::<usize>()
            + state
                .connections
                .iter()
                .filter(|(r, _)| **r != snapshot.revision)
                .map(|(_, n)| *n)
                .sum::<usize>();
        vec![
            (
                "X-Racer-Trust-Generation",
                if ready { snapshot.generation } else { 0 }.to_string(),
            ),
            (
                "X-Racer-Trust-Digest",
                if ready {
                    snapshot.digest.clone()
                } else {
                    String::new()
                },
            ),
            ("X-Racer-Certificate-Issuer", snapshot.issuer.clone()),
            (
                "X-Racer-Old-Connections",
                if ready { old } else { old.max(1) }.to_string(),
            ),
        ]
    }
    pub fn status(&self) -> serde_json::Value {
        let state = self.state.lock().unwrap();
        serde_json::json!({"generation":state.current.generation,"trustDigest":state.current.digest,
            "issuer":state.current.issuer,"expiresUnix":state.current.expires_unix,
            "installedWorkers":state.installed.values().filter(|(r,_)| *r == state.current.revision).count(),
            "workers":self.workers,"error":state.error})
    }
    pub(crate) fn connect(self: &Arc<Self>, address: SocketAddr) -> io::Result<Stream> {
        let snapshot = {
            let mut state = self.state.lock().unwrap();
            let snapshot = state.current.clone();
            if unix() >= snapshot.expires_unix {
                return Err(invalid("node certificate expired"));
            }
            *state.connections.entry(snapshot.revision).or_default() += 1;
            snapshot
        };
        let mut stream = match Stream::connect(address, &snapshot.context, &self.server_name) {
            Ok(stream) => stream,
            Err(error) => {
                self.release(snapshot.revision);
                return Err(error);
            }
        };
        stream.end = stream.end.min(
            Instant::now() + Duration::from_secs(snapshot.expires_unix.saturating_sub(unix())),
        );
        stream.owner = Some((self.clone(), snapshot.revision));
        Ok(stream)
    }
    fn release(&self, revision: u64) {
        let mut state = self.state.lock().unwrap();
        if let Some(count) = state.connections.get_mut(&revision) {
            *count -= 1;
            if *count == 0 {
                state.connections.remove(&revision);
            }
        }
    }
}

struct Settings {
    boot: String,
    proof: url::Url,
    trust_dir: PathBuf,
    enroll: url::Url,
    token: PathBuf,
    namespace: String,
    pod: String,
    server_name: String,
    identity: PeerIdentity,
}
impl Settings {
    fn from_env(boot: [u8; 32]) -> io::Result<Self> {
        let env = |key| std::env::var(key).map_err(invalid);
        let identity = PeerIdentity::new(
            &env("RACER_UNIVERSE")?,
            &env("RACER_NODE")?,
            &env("RACER_POD_UID")?,
        )?;
        let enroll = url::Url::parse(&env("RACER_ENROLL_URL")?).map_err(invalid)?;
        if enroll.scheme() != "https"
            || enroll.path() != "/v3/enroll"
            || enroll.query().is_some()
            || enroll.fragment().is_some()
            || !enroll.username().is_empty()
            || enroll.password().is_some()
        {
            return Err(invalid("expected HTTPS /v3/enroll enrollment URL"));
        }
        let mut proof = enroll.clone();
        proof
            .set_port(Some(8446))
            .map_err(|_| invalid("invalid proof port"))?;
        proof.set_path("/v3/proof");
        if let Ok(value) = std::env::var("RACER_TRUST_PROOF_URL") {
            proof = url::Url::parse(&value).map_err(invalid)?;
        }
        if proof.scheme() != "https"
            || proof.path() != "/v3/proof"
            || proof.query().is_some()
            || proof.fragment().is_some()
            || !proof.username().is_empty()
            || proof.password().is_some()
        {
            return Err(invalid("expected HTTPS /v3/proof URL"));
        }
        let result = Self {
            boot: hex(&boot),
            proof,
            trust_dir: std::env::var_os("RACER_TLS_TRUST_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|| "/var/run/racer-trust".into()),
            enroll,
            token: env("RACER_CONTROL_TOKEN_FILE")?.into(),
            namespace: env("RACER_POD_NAMESPACE")?,
            pod: env("RACER_POD_NAME")?,
            server_name: env("RACER_CONTROL_SERVER_NAME")?,
            identity,
        };
        if result.namespace.is_empty() || result.pod.is_empty() || result.server_name.is_empty() {
            return Err(invalid("empty enrollment identity"));
        }
        Ok(result)
    }
}
struct Leaf {
    key: zeroize::Zeroizing<Vec<u8>>,
    certificate: Vec<u8>,
    issuer: String,
    expires: u64,
    renew: u64,
}
impl Leaf {
    fn enroll(settings: &Settings, trust: &TrustBundle) -> io::Result<Self> {
        let request = tls::generate_key_and_csr(&settings.identity)?;
        let key = zeroize::Zeroizing::new(request.private_key_pem);
        let body = serde_json::to_vec(&serde_json::json!({
            "csr": std::str::from_utf8(&request.csr_pem).map_err(invalid)?,
            "pod_namespace":settings.namespace,"pod_name":settings.pod,
        }))
        .map_err(invalid)?;
        let token = zeroize::Zeroizing::new(read_bounded(&settings.token, 16384)?);
        let token = std::str::from_utf8(&token).map_err(invalid)?.trim();
        if token.is_empty() || !token.bytes().all(|b| b.is_ascii_graphic()) {
            return Err(invalid("invalid enrollment bearer token"));
        }
        let authority = &settings.enroll[url::Position::BeforeHost..url::Position::AfterPort];
        let context = TlsContext::bootstrap(trust)?;
        let address = *settings
            .enroll
            .socket_addrs(|| Some(8444))
            .map_err(invalid)?
            .first()
            .ok_or_else(|| invalid("enrollment DNS returned no addresses"))?;
        let mut stream = Stream::connect(address, &context, &settings.server_name)?;
        stream.set_read_timeout(Some(Duration::from_secs(5)))?;
        stream.set_write_timeout(Some(Duration::from_secs(5)))?;
        let wire = zeroize::Zeroizing::new(format!(
            "POST /v3/enroll HTTP/1.1\r\nHost: {authority}\r\nAuthorization: Bearer {token}\r\nX-Racer-Boot: {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            settings.boot,
            body.len()
        ));
        stream.write_all(wire.as_bytes())?;
        stream.write_all(&body)?;
        let mut reader = io::BufReader::new(stream);
        let (reply, _) = super::read_response(&mut reader, None, 1024 * 1024)?;
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Reply {
            certificate: String,
            generation: u64,
            issuer: String,
        }
        let reply: Reply =
            serde_json::from_slice(&reply.ok_or_else(|| invalid("empty enrollment reply"))?)
                .map_err(invalid)?;
        if reply.generation != trust.generation || reply.issuer != trust.active {
            return Err(invalid("enrollment trust generation/issuer mismatch"));
        }
        let info = tls::validate_leaf(
            trust,
            reply.certificate.as_bytes(),
            &key,
            &settings.identity,
        )?;
        if info.issuer != reply.issuer {
            return Err(invalid("enrollment issuer proof mismatch"));
        }
        let mut random = [0u8; 8];
        crate::environment::random(&mut random).map_err(|e| invalid(e.to_string()))?;
        let now = unix();
        if info.expires_unix <= now + 10 {
            return Err(invalid("enrolled certificate expires too soon"));
        }
        let lifetime = info.expires_unix.saturating_sub(info.issued_unix);
        let renew = info.issued_unix + lifetime * (45 + u64::from_le_bytes(random) % 11) / 100;
        Ok(Self {
            key,
            certificate: reply.certificate.into_bytes(),
            issuer: reply.issuer,
            expires: info.expires_unix,
            renew: renew.max(now + 1),
        })
    }
    fn snapshot(
        &self,
        trust: &TrustBundle,
        revision: u64,
        identity: &PeerIdentity,
    ) -> io::Result<Snapshot> {
        tls::validate_leaf(trust, &self.certificate, &self.key, identity)?;
        Ok(Snapshot {
            revision,
            generation: trust.generation,
            digest: hex(&trust.digest),
            issuer: self.issuer.clone(),
            context: Arc::new(TlsContext::new(trust, &self.certificate, &self.key)?),
            expires_unix: self.expires,
        })
    }
}
fn read_bounded(path: &std::path::Path, limit: usize) -> io::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)?
        .take((limit + 1) as u64)
        .read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(invalid("credential input exceeds budget"));
    }
    Ok(bytes)
}

/// Independent of control requests so failures cannot starve trust reload.
pub struct Manager {
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}
impl Manager {
    pub fn start(workers: usize, updates: &Arc<super::Updates>) -> io::Result<Self> {
        let settings = Settings::from_env(updates.boot()?)?;
        Self::start_with_settings(workers, updates, settings)
    }
    fn start_with_settings(
        workers: usize,
        updates: &Arc<super::Updates>,
        settings: Settings,
    ) -> io::Result<Self> {
        let mut trust = TrustBundle::load(&settings.trust_dir, None)?;
        let mut observed = trust.clone();
        let mut leaf = Leaf::enroll(&settings, &trust)?;
        let initial = Arc::new(leaf.snapshot(&trust, 1, &settings.identity)?);
        let provider = Arc::new(Provider {
            state: Mutex::new(State {
                current: initial,
                installed: BTreeMap::new(),
                error: None,
                connections: BTreeMap::new(),
            }),
            workers,
            identity: settings.identity.clone(),
            server_name: settings.server_name.clone(),
        });
        updates.set_credentials(provider.clone());
        let updates = Arc::downgrade(updates);
        let stop = Arc::new(AtomicBool::new(false));
        let stopping = stop.clone();
        let thread = std::thread::Builder::new()
            .name("racer-credentials".into())
            .spawn(move || {
                let mut next_attempt = Instant::now();
                let mut next_proof = Instant::now();
                while !stopping.load(Ordering::Acquire) {
                    let result = (|| {
                        let next = TrustBundle::load(&settings.trust_dir, Some(&observed));
                        // A valid projection advances the anti-rollback floor even when
                        // enrolling a compatible leaf is temporarily unavailable.
                        if let Ok(next) = &next {
                            observed = next.clone();
                        }
                        let changed = next.as_ref().is_ok_and(|next| next.digest != trust.digest);
                        let projection_error = next.as_ref().err().map(ToString::to_string);
                        // Install overlap roots immediately even if enrollment is unavailable.
                        if changed {
                            let next = next.unwrap();
                            let revision = provider.current().revision + 1;
                            let snapshot = leaf
                                .snapshot(&next, revision, &settings.identity)
                                .or_else(|_| {
                                    // Recovery after expiration or an offline root retirement
                                    // must be able to enroll against the new valid projection.
                                    let replacement = Leaf::enroll(&settings, &next)?;
                                    let snapshot = replacement.snapshot(
                                        &next,
                                        revision,
                                        &settings.identity,
                                    )?;
                                    leaf = replacement;
                                    Ok::<_, io::Error>(snapshot)
                                })?;
                            provider.state.lock().unwrap().current = Arc::new(snapshot);
                            trust = next;
                            next_attempt = Instant::now();
                            next_proof = Instant::now();
                            if let Some(updates) = updates.upgrade() {
                                updates.wake_all();
                            }
                        }
                        let renew = trust.active != leaf.issuer || unix() >= leaf.renew;
                        if renew && Instant::now() >= next_attempt {
                            next_attempt = Instant::now() + Duration::from_secs(5);
                            let next_leaf = Leaf::enroll(&settings, &trust)?;
                            let snapshot = next_leaf.snapshot(
                                &trust,
                                provider.current().revision + 1,
                                &settings.identity,
                            )?;
                            provider.state.lock().unwrap().current = Arc::new(snapshot);
                            leaf = next_leaf;
                            next_proof = Instant::now();
                            if let Some(updates) = updates.upgrade() {
                                updates.wake_all();
                            }
                        }
                        if let Some(error) = projection_error {
                            return Err(invalid(error));
                        }
                        if Instant::now() >= next_proof && provider.headers()[0].1 != "0" {
                            next_proof = Instant::now() + Duration::from_secs(5);
                            prove(&settings, &provider)?;
                        }
                        Ok::<_, io::Error>(())
                    })();
                    provider.state.lock().unwrap().error = result.err().map(|e| e.to_string());
                    std::thread::park_timeout(Duration::from_secs(1));
                }
            })?;
        Ok(Self {
            stop,
            thread: Some(thread),
        })
    }
}

fn prove(settings: &Settings, provider: &Arc<Provider>) -> io::Result<()> {
    let headers = provider.headers();
    if headers[0].1 == "0" {
        return Ok(());
    }
    let revision = provider.current().revision;
    let address = *settings
        .proof
        .socket_addrs(|| Some(8446))
        .map_err(invalid)?
        .first()
        .ok_or_else(|| invalid("proof DNS returned no addresses"))?;
    let mut stream = provider.connect(address)?;
    // No new bundle may be acknowledged using a connection from the old context.
    if provider.current().revision != revision {
        return Ok(());
    }
    let authority = &settings.proof[url::Position::BeforeHost..url::Position::AfterPort];
    write!(
        stream,
        "POST /v3/proof HTTP/1.1\r\nHost: {authority}\r\nX-Racer-Boot: {}\r\n",
        settings.boot
    )?;
    for (name, value) in headers {
        write!(stream, "{name}: {value}\r\n")?;
    }
    stream.write_all(b"Content-Length: 0\r\nConnection: close\r\n\r\n")?;
    super::read_response(&mut io::BufReader::new(stream), None, 8192)?;
    Ok(())
}
impl Drop for Manager {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            thread.thread().unpark();
            let _ = thread.join();
        }
    }
}

/// Synchronous control-only adapter over the native socket BIO.
pub(crate) struct Stream {
    owner: Option<(Arc<Provider>, u64)>,
    session: TlsSession,
    read_timeout: Duration,
    write_timeout: Duration,
    end: Instant,
}
impl Stream {
    fn connect(address: SocketAddr, context: &TlsContext, name: &str) -> io::Result<Self> {
        let socket = TcpStream::connect_timeout(&address, Duration::from_millis(250))?;
        socket.set_nonblocking(true)?;
        let session = TlsSession::client(
            context,
            socket.into(),
            ExpectedPeer::ControlPlane {
                dns_name: name.to_owned(),
            },
        )?;
        let mut stream = Self {
            owner: None,
            session,
            read_timeout: Duration::from_secs(2),
            write_timeout: Duration::from_secs(2),
            end: Instant::now() + Duration::from_secs(10),
        };
        let end = Instant::now() + Duration::from_secs(3);
        loop {
            match stream.session.handshake()? {
                TlsProgress::Complete(()) => {
                    let expires = stream
                        .session
                        .valid_until()
                        .ok_or_else(|| invalid("TLS session has no authenticated lifetime"))?;
                    if unix() >= expires {
                        return Err(invalid("TLS certificate expired during handshake"));
                    }
                    stream.end = stream
                        .end
                        .min(Instant::now() + Duration::from_secs(expires.saturating_sub(unix())));
                    return Ok(stream);
                }
                TlsProgress::WantRead => stream.wait(false, end)?,
                TlsProgress::WantWrite => stream.wait(true, end)?,
                TlsProgress::Eof => return Err(io::ErrorKind::UnexpectedEof.into()),
            }
        }
    }
    fn wait(&self, write: bool, end: Instant) -> io::Result<()> {
        loop {
            self.check_expiry()?;
            let remaining = end.min(self.end).saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(io::ErrorKind::TimedOut.into());
            }
            let mut fd = libc::pollfd {
                fd: self.session.as_raw_fd(),
                events: if write { libc::POLLOUT } else { libc::POLLIN },
                revents: 0,
            };
            // SAFETY: initialized pollfd referencing this live TLS socket.
            let n = unsafe { libc::poll(&mut fd, 1, remaining.as_millis().clamp(1, 100) as i32) };
            if n > 0 {
                return Ok(());
            }
            if n < 0 && io::Error::last_os_error().kind() != io::ErrorKind::Interrupted {
                return Err(io::Error::last_os_error());
            }
        }
    }
    fn check_expiry(&self) -> io::Result<()> {
        if self
            .session
            .valid_until()
            .is_some_and(|expires| unix() >= expires)
        {
            return Err(invalid("TLS certificate expired"));
        }
        Ok(())
    }
    pub fn set_read_timeout(&mut self, timeout: Option<Duration>) -> io::Result<()> {
        self.read_timeout = timeout.unwrap_or(Duration::from_secs(2));
        Ok(())
    }
    pub fn set_write_timeout(&mut self, timeout: Option<Duration>) -> io::Result<()> {
        self.write_timeout = timeout.unwrap_or(Duration::from_secs(2));
        Ok(())
    }
}
impl Drop for Stream {
    fn drop(&mut self) {
        if let Some((provider, revision)) = &self.owner {
            // Disable the socket before reporting it drained. The native session
            // closes its owned descriptor after this destructor returns.
            // SAFETY: the session still owns this live socket descriptor.
            unsafe { libc::shutdown(self.session.as_raw_fd(), libc::SHUT_RDWR) };
            provider.release(*revision);
        }
    }
}
impl Read for Stream {
    fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        let end = Instant::now() + self.read_timeout;
        loop {
            self.check_expiry()?;
            if Instant::now() >= end.min(self.end) {
                return Err(io::ErrorKind::TimedOut.into());
            }
            match self.session.read(bytes)? {
                TlsProgress::Complete(n) => return Ok(n),
                TlsProgress::Eof => return Ok(0),
                TlsProgress::WantRead => self.wait(false, end)?,
                TlsProgress::WantWrite => self.wait(true, end)?,
            }
        }
    }
}
impl Write for Stream {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        let end = Instant::now() + self.write_timeout;
        loop {
            self.check_expiry()?;
            if Instant::now() >= end.min(self.end) {
                return Err(io::ErrorKind::TimedOut.into());
            }
            match self.session.write(bytes)? {
                TlsProgress::Complete(n) => return Ok(n),
                TlsProgress::Eof => return Err(io::ErrorKind::WriteZero.into()),
                TlsProgress::WantRead => self.wait(false, end)?,
                TlsProgress::WantWrite => self.wait(true, end)?,
            }
        }
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[cfg(test)]
#[path = "../tests/control/credentials.rs"]
pub(crate) mod tests;
