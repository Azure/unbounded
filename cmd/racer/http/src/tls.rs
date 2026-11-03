//! Outbound TLS for pooled connections, preferring kernel TLS.
//!
//! A fresh TCP connection completes a rustls handshake on the caller's reactor
//! (nonblocking sends and receives driven by readiness). Then, by default:
//!
//! - Kernel TLS: the socket switches to the `tls` upper-layer protocol and the
//!   negotiated traffic secrets are installed with `TLS_TX` and `TLS_RX`. The
//!   descriptor then carries plaintext like any TCP socket, so body receives
//!   decrypt directly into the caller's buffer. Receives use `recvmsg` with a
//!   control buffer so TLS 1.3 post-handshake records (NewSessionTicket) are
//!   reported instead of failing with `EIO`; tickets are discarded and any other
//!   handshake message (KeyUpdate) or alert closes the connection.
//! - Userspace fallback: if the kernel `tls` module is unavailable, a relay
//!   thread owns the TCP socket and a rustls session and exchanges plaintext
//!   with the pooled connection over a Unix socket pair. This costs extra
//!   copies and a thread per connection, so it exists for correctness only.
//!   In Auto mode, if the kernel rejects the negotiated keys after the socket
//!   switched to the `tls` ULP, the socket is closed and the connection is
//!   redone on a fresh socket with userspace TLS; the endpoint then skips
//!   kernel TLS for that cipher suite. Kernel mode fails instead.
//!
//! The handshake reads one TLS record at a time so no bytes beyond the final
//! handshake record are buffered in userspace before the secrets move to the
//! kernel. Only cipher suites supported by Linux kernel TLS are offered:
//! AES-128-GCM, AES-256-GCM, and ChaCha20-Poly1305 for TLS 1.2 and 1.3.
//! Session resumption is disabled.
//!
//! [`stats`] reports how many connections used each path, and the first use of
//! each path is logged once to standard error.

use crate::connection::{ConnectionLease, Context};
use rustls::{
    ClientConfig, ClientConnection, ConnectionTrafficSecrets, ProtocolVersion, RootCertStore,
    pki_types::{CertificateDer, ServerName, pem::PemObject},
};
use std::{
    io::{Read, Write},
    os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd},
    path::{Path, PathBuf},
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
};
use uring_runtime::{Scope as _, reactor::descriptor::Descriptor};
use zeroize::Zeroizing;

pub use rustls;

/// Environment variable naming the PEM trust bundle for origin HTTPS.
pub const TRUST_BUNDLE_ENV: &str = "RACER_ORIGIN_TRUST_BUNDLE";

/// Default PEM trust bundle (Debian and Ubuntu `ca-certificates`).
pub const DEFAULT_TRUST_BUNDLE: &str = "/etc/ssl/certs/ca-certificates.crt";

/// Largest trust bundle accepted.
pub const MAX_TRUST_BUNDLE: u64 = 8 << 20;

/// Largest post-handshake handshake message accepted under kernel TLS.
const MAX_CONTROL_BYTES: usize = 64 * 1024;

const SOL_TCP: libc::c_int = 6;
const TCP_ULP: libc::c_int = 31;
const SOL_TLS: libc::c_int = 282;
const TLS_TX: libc::c_int = 1;
const TLS_RX: libc::c_int = 2;
const TLS_RX_EXPECT_NO_PAD: libc::c_int = 4;
const TLS_1_2_VERSION: u16 = 0x0303;
const TLS_1_3_VERSION: u16 = 0x0304;
const TLS_CIPHER_AES_GCM_128: u16 = 51;
const TLS_CIPHER_AES_GCM_256: u16 = 52;
const TLS_CIPHER_CHACHA20_POLY1305: u16 = 54;

/// How a connection's record layer is placed.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum TlsMode {
    /// Kernel TLS when available, otherwise the userspace relay.
    #[default]
    Auto,

    /// Kernel TLS only; connections fail if it cannot be enabled.
    Kernel,

    /// Always use the userspace relay.
    Userspace,
}

/// Server identity and client policy for one HTTPS endpoint.
#[derive(Clone)]
pub struct TlsClient {
    config: Arc<ClientConfig>,

    name: ServerName<'static>,

    mode: TlsMode,

    /// Bit `i` is set once the kernel rejected the traffic keys for the `i`th
    /// configured cipher suite; Auto mode then uses userspace TLS directly.
    /// Shared by clones so one endpoint learns once.
    rejected_suites: Arc<AtomicU64>,
}

impl std::fmt::Debug for TlsClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TlsClient")
            .field("name", &self.name)
            .field("mode", &self.mode)
            .finish()
    }
}

impl TlsClient {
    /// Verify `host` (a DNS name or IP literal, without brackets) as SNI and
    /// certificate identity, using a configuration from [`client_config`].
    pub fn new(config: Arc<ClientConfig>, host: &str) -> std::io::Result<Self> {
        let name = ServerName::try_from(host.to_owned())
            .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidInput, "server name"))?;
        Ok(Self {
            config,
            name,
            mode: TlsMode::Auto,
            rejected_suites: Arc::default(),
        })
    }

    /// Select the record-layer placement.
    pub fn with_mode(mut self, mode: TlsMode) -> Self {
        self.mode = mode;
        self
    }

    /// The configured record-layer placement.
    pub fn mode(&self) -> TlsMode {
        self.mode
    }

    /// The client configuration.
    pub fn config(&self) -> &Arc<ClientConfig> {
        &self.config
    }

    /// The verified server name.
    pub fn server_name(&self) -> &ServerName<'static> {
        &self.name
    }
}

/// The trust bundle path from [`TRUST_BUNDLE_ENV`], else [`DEFAULT_TRUST_BUNDLE`].
pub fn trust_bundle_path() -> PathBuf {
    std::env::var_os(TRUST_BUNDLE_ENV)
        .filter(|v| !v.is_empty())
        .map_or_else(|| PathBuf::from(DEFAULT_TRUST_BUNDLE), PathBuf::from)
}

/// Parse every certificate in a PEM bundle into a root store. Fails if the file
/// is unreadable, larger than [`MAX_TRUST_BUNDLE`], or yields no usable root.
pub fn load_trust_bundle(path: &Path) -> std::io::Result<RootCertStore> {
    let file = std::fs::File::open(path)?;
    let mut bytes = Vec::new();
    file.take(MAX_TRUST_BUNDLE + 1).read_to_end(&mut bytes)?;
    if bytes.len() as u64 > MAX_TRUST_BUNDLE {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "trust bundle too large",
        ));
    }
    trust_store_from_pem(&bytes)
}

/// Parse a PEM bundle into a root store, skipping certificates rustls rejects.
pub fn trust_store_from_pem(pem: &[u8]) -> std::io::Result<RootCertStore> {
    let mut roots = RootCertStore::empty();
    let certificates = CertificateDer::pem_slice_iter(pem)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|_| std::io::Error::new(std::io::ErrorKind::InvalidData, "trust bundle PEM"))?;
    let (added, _) = roots.add_parsable_certificates(certificates);
    if added == 0 {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidData,
            "trust bundle has no usable certificates",
        ));
    }
    Ok(roots)
}

/// Client configuration restricted to kernel-TLS-capable suites, with secret
/// extraction enabled, resumption disabled, and ALPN `http/1.1`.
pub fn client_config(roots: RootCertStore) -> Arc<ClientConfig> {
    let mut provider = rustls::crypto::ring::default_provider();
    provider.cipher_suites.retain(|suite| {
        use rustls::CipherSuite::*;
        matches!(
            suite.suite(),
            TLS13_AES_128_GCM_SHA256
                | TLS13_AES_256_GCM_SHA384
                | TLS13_CHACHA20_POLY1305_SHA256
                | TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
                | TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
                | TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
                | TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
                | TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
                | TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
        )
    });
    let mut config = ClientConfig::builder_with_provider(Arc::new(provider))
        .with_protocol_versions(&[&rustls::version::TLS13, &rustls::version::TLS12])
        .expect("ring supports TLS 1.2 and 1.3")
        .with_root_certificates(roots)
        .with_no_client_auth();
    config.enable_secret_extraction = true;
    config.resumption = rustls::client::Resumption::disabled();
    config.alpn_protocols = vec![b"http/1.1".to_vec()];
    Arc::new(config)
}

/// Process-wide record-layer placement counters.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct TlsStats {
    /// Connections established with kernel TLS.
    pub kernel: u64,

    /// Connections established with the userspace relay.
    pub userspace: u64,

    /// Connections whose kernel TLS setup failed.
    pub kernel_failures: u64,

    /// TLS 1.3 session tickets received and discarded under kernel TLS.
    pub discarded_tickets: u64,
}

static KERNEL: AtomicU64 = AtomicU64::new(0);
static USERSPACE: AtomicU64 = AtomicU64::new(0);
static KERNEL_FAILURES: AtomicU64 = AtomicU64::new(0);
static TICKETS: AtomicU64 = AtomicU64::new(0);
static KERNEL_LOGGED: AtomicBool = AtomicBool::new(false);
static USERSPACE_LOGGED: AtomicBool = AtomicBool::new(false);
/// Set once kernel TLS failed in a way that will not change (module absent).
static KERNEL_UNAVAILABLE: AtomicBool = AtomicBool::new(false);

/// Snapshot the process-wide record-layer counters.
pub fn stats() -> TlsStats {
    TlsStats {
        kernel: KERNEL.load(Ordering::Relaxed),
        userspace: USERSPACE.load(Ordering::Relaxed),
        kernel_failures: KERNEL_FAILURES.load(Ordering::Relaxed),
        discarded_tickets: TICKETS.load(Ordering::Relaxed),
    }
}

/// Whether `/proc/sys/net/ipv4/tcp_available_ulp` lists `tls` (module loaded).
/// A missing module may still autoload on first use.
pub fn kernel_tls_loaded() -> bool {
    std::fs::read_to_string("/proc/sys/net/ipv4/tcp_available_ulp")
        .is_ok_and(|ulps| ulps.split_whitespace().any(|ulp| ulp == "tls"))
}

/// Outcome of [`establish`].
pub(crate) enum Established<C: Context> {
    /// The lease carries a usable record layer.
    Ready(ConnectionLease<C>),

    /// Auto mode attached the kernel `tls` ULP but the kernel rejected the
    /// traffic keys. The socket cannot revert to plain TCP and the session's
    /// secrets are consumed, so the caller must reconnect on a fresh socket and
    /// call [`establish`] again with `userspace` set.
    Retry(ConnectionLease<C>),
}

#[cfg(any(test, feature = "test-util"))]
thread_local! {
    static FAIL_KEY_INSTALL: std::cell::Cell<u32> = const { std::cell::Cell::new(0) };
}

/// Make the next `count` kernel TLS key installations on this thread fail with
/// `EOPNOTSUPP` after the `tls` ULP attaches, as an unsupported cipher would.
#[cfg(feature = "test-util")]
pub fn fail_next_key_installs(count: u32) {
    FAIL_KEY_INSTALL.with(|c| c.set(count));
}

/// Index of the negotiated suite within the client's provider, if trackable.
fn suite_bit(tls: &TlsClient, session: &ClientConnection) -> Option<u64> {
    let suite = session.negotiated_cipher_suite()?.suite();
    let index = tls
        .config
        .crypto_provider()
        .cipher_suites
        .iter()
        .position(|s| s.suite() == suite)?;
    (index < 64).then(|| 1u64 << index)
}

/// Complete the handshake on a freshly connected lease and place its record
/// layer. `userspace` forces the userspace relay regardless of mode.
pub(crate) async fn establish<C: Context>(
    reactor: &C::Reactor,
    context: &C,
    mut connection: ConnectionLease<C>,
    tls: &TlsClient,
    userspace: bool,
    scope: &C::Scope,
) -> crate::connection::Result<C, Established<C>> {
    let mut session = ClientConnection::new(tls.config.clone(), tls.name.clone())
        .map_err(|_| uring_runtime::Error::InvalidConfiguration)?;
    let fd = connection.socket();
    let mut reader = RecordReader::default();
    loop {
        while session.wants_write() {
            match session.write_tls(&mut Socket(&fd)) {
                Ok(_) => (),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    wait::<C>(reactor, &connection, libc::POLLOUT, scope).await?;
                }
                Err(_) => return Err(uring_runtime::Error::Io.into()),
            }
        }
        if !session.is_handshaking() {
            break;
        }
        match session.read_tls(&mut reader.over(&fd)) {
            Ok(0) => return Err(uring_runtime::Error::Io.into()),
            Ok(_) => {
                if session.process_new_packets().is_err() {
                    // Send any alert rustls queued, best effort.
                    let _ = session.write_tls(&mut Socket(&fd));
                    return Err(uring_runtime::Error::Os(libc::EPROTO).into());
                }
            }
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                wait::<C>(reactor, &connection, libc::POLLIN, scope).await?;
            }
            Err(_) => return Err(uring_runtime::Error::Io.into()),
        }
    }
    scope.check()?;
    drop(fd);
    let suite = suite_bit(tls, &session);
    let kernel = match tls.mode {
        _ if userspace => false,
        TlsMode::Userspace => false,
        TlsMode::Kernel => true,
        TlsMode::Auto => {
            !KERNEL_UNAVAILABLE.load(Ordering::Relaxed)
                && suite.is_none_or(|bit| tls.rejected_suites.load(Ordering::Relaxed) & bit == 0)
        }
    };
    if kernel {
        let fd = connection.socket();
        let raw = fd.as_raw_fd();
        if let Err(error) = set_ulp(raw) {
            KERNEL_FAILURES.fetch_add(1, Ordering::Relaxed);
            if tls.mode == TlsMode::Kernel {
                return Err(uring_runtime::Error::Unavailable.into());
            }
            if matches!(
                error.raw_os_error(),
                Some(libc::ENOENT | libc::EOPNOTSUPP | libc::ENOPROTOOPT | libc::EPERM)
            ) {
                KERNEL_UNAVAILABLE.store(true, Ordering::Relaxed);
            }
            log_once(
                &USERSPACE_LOGGED,
                &format!("racer: kernel TLS unavailable ({error}); using userspace TLS"),
            );
        } else {
            let version = session.protocol_version();
            let secrets = session
                .dangerous_extract_secrets()
                .map_err(|_| uring_runtime::Error::Os(libc::EPROTO))?;
            #[cfg(any(test, feature = "test-util"))]
            let injected = FAIL_KEY_INSTALL.with(|c| {
                let count = c.get();
                c.set(count.saturating_sub(1));
                count > 0
            });
            #[cfg(not(any(test, feature = "test-util")))]
            let injected = false;
            let installed = if injected {
                Err(std::io::Error::from_raw_os_error(libc::EOPNOTSUPP))
            } else {
                install(raw, version, secrets.tx, TLS_TX)
                    .and_then(|()| install(raw, version, secrets.rx, TLS_RX))
            };
            if let Err(error) = installed {
                // The socket is half switched and unusable. Auto mode remembers
                // the rejected suite for this endpoint and asks for a fresh
                // connection with userspace TLS; Kernel mode fails.
                KERNEL_FAILURES.fetch_add(1, Ordering::Relaxed);
                if tls.mode == TlsMode::Kernel {
                    log_once(
                        &USERSPACE_LOGGED,
                        &format!("racer: kernel TLS key install failed ({error})"),
                    );
                    return Err(uring_runtime::Error::Unavailable.into());
                }
                if let Some(bit) = suite {
                    tls.rejected_suites.fetch_or(bit, Ordering::Relaxed);
                }
                log_once(
                    &USERSPACE_LOGGED,
                    &format!(
                        "racer: kernel TLS key install failed ({error}); reconnecting with userspace TLS"
                    ),
                );
                return Ok(Established::Retry(connection));
            }
            if version == Some(ProtocolVersion::TLSv1_3) {
                // Lets the kernel decrypt in place; it retries if a record is padded.
                let one: libc::c_int = 1;
                // SAFETY: setsockopt reads one live integer.
                unsafe {
                    libc::setsockopt(
                        raw,
                        SOL_TLS,
                        TLS_RX_EXPECT_NO_PAD,
                        (&one as *const libc::c_int).cast(),
                        std::mem::size_of_val(&one) as libc::socklen_t,
                    )
                };
            }
            KERNEL.fetch_add(1, Ordering::Relaxed);
            log_once(&KERNEL_LOGGED, "racer: origin HTTPS uses kernel TLS");
            connection.enable_kernel_tls();
            return Ok(Established::Ready(connection));
        }
    }
    // The relay's queue and scratch are payload copies outside `HttpIo`
    // buffers; admit them before spawning so they count against the budget.
    let charge = context.charge(RELAY_ALLOCATION)?;
    let (app, relay) = spawn_relay(session, &connection.socket(), charge)
        .map_err(uring_runtime::Error::from_io)?;
    connection.replace_socket(Rc::new(app));
    connection.attach_relay(relay);
    USERSPACE.fetch_add(1, Ordering::Relaxed);
    log_once(&USERSPACE_LOGGED, "racer: origin HTTPS uses userspace TLS");
    Ok(Established::Ready(connection))
}

/// Wait for socket readiness, retaining the connection's admission slot.
async fn wait<C: Context>(
    reactor: &C::Reactor,
    connection: &ConnectionLease<C>,
    interest: libc::c_short,
    scope: &C::Scope,
) -> crate::connection::Result<C, ()> {
    reactor
        .readiness_with_lease(
            connection.socket(),
            interest as u32,
            connection.slot().cloned(),
            scope,
        )
        .await?;
    scope.check()
}

fn log_once(flag: &AtomicBool, message: &str) {
    if !flag.swap(true, Ordering::Relaxed) {
        eprintln!("{message}");
    }
}

fn set_ulp(fd: RawFd) -> std::io::Result<()> {
    // SAFETY: setsockopt reads the three-byte name.
    let result = unsafe { libc::setsockopt(fd, SOL_TCP, TCP_ULP, b"tls".as_ptr().cast(), 3) };
    if result < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

/// Install one direction's traffic secret as a Linux `tls12_crypto_info_*`.
fn install(
    fd: RawFd,
    version: Option<ProtocolVersion>,
    (sequence, secrets): (u64, ConnectionTrafficSecrets),
    direction: libc::c_int,
) -> std::io::Result<()> {
    let version = match version {
        Some(ProtocolVersion::TLSv1_2) => TLS_1_2_VERSION,
        Some(ProtocolVersion::TLSv1_3) => TLS_1_3_VERSION,
        _ => return Err(std::io::Error::from_raw_os_error(libc::EPROTONOSUPPORT)),
    };
    let info = crypto_info(version, sequence, &secrets)?;
    // SAFETY: setsockopt reads the complete initialized structure.
    let result = unsafe {
        libc::setsockopt(
            fd,
            SOL_TLS,
            direction,
            info.as_ptr().cast(),
            info.len() as libc::socklen_t,
        )
    };
    if result < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

/// Serialize a packed Linux crypto info structure. Key bytes are zeroized on drop.
fn crypto_info(
    version: u16,
    sequence: u64,
    secrets: &ConnectionTrafficSecrets,
) -> std::io::Result<Zeroizing<Vec<u8>>> {
    let (cipher, key, iv): (u16, &[u8], &[u8]) = match secrets {
        ConnectionTrafficSecrets::Aes128Gcm { key, iv } => {
            (TLS_CIPHER_AES_GCM_128, key.as_ref(), iv.as_ref())
        }
        ConnectionTrafficSecrets::Aes256Gcm { key, iv } => {
            (TLS_CIPHER_AES_GCM_256, key.as_ref(), iv.as_ref())
        }
        ConnectionTrafficSecrets::Chacha20Poly1305 { key, iv } => {
            (TLS_CIPHER_CHACHA20_POLY1305, key.as_ref(), iv.as_ref())
        }
        _ => return Err(std::io::Error::from_raw_os_error(libc::EPROTONOSUPPORT)),
    };
    let key_length = if cipher == TLS_CIPHER_AES_GCM_128 {
        16
    } else {
        32
    };
    if iv.len() != 12 || key.len() != key_length {
        return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
    }
    let mut info = Zeroizing::new(Vec::with_capacity(56));
    info.extend_from_slice(&version.to_ne_bytes());
    info.extend_from_slice(&cipher.to_ne_bytes());
    if cipher == TLS_CIPHER_CHACHA20_POLY1305 {
        // iv[12], key[32], salt[0], rec_seq[8]
        info.extend_from_slice(iv);
        info.extend_from_slice(key);
    } else {
        // iv[8], key[16|32], salt[4], rec_seq[8]
        info.extend_from_slice(&iv[4..]);
        info.extend_from_slice(key);
        info.extend_from_slice(&iv[..4]);
    }
    info.extend_from_slice(&sequence.to_be_bytes());
    Ok(info)
}

/// Nonblocking `Read`/`Write` over a borrowed descriptor.
struct Socket<'a>(&'a Descriptor);

impl Read for Socket<'_> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        self.0.try_recv(buf)
    }
}

impl Write for Socket<'_> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.try_send(buf)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Reads never cross a TLS record boundary, so nothing past the final
/// handshake record is consumed from the socket.
#[derive(Default)]
struct RecordReader {
    header: [u8; 5],

    have: usize,

    body: usize,
}

struct BoundedRead<'a, 'b> {
    state: &'a mut RecordReader,

    fd: &'b Descriptor,
}

impl RecordReader {
    fn over<'a, 'b>(&'a mut self, fd: &'b Descriptor) -> BoundedRead<'a, 'b> {
        BoundedRead { state: self, fd }
    }

    /// Account for bytes delivered, returning how many may be read next.
    fn advance(&mut self, bytes: &[u8]) {
        if self.have < 5 {
            self.header[self.have..self.have + bytes.len()].copy_from_slice(bytes);
            self.have += bytes.len();
            if self.have == 5 {
                self.body = usize::from(u16::from_be_bytes([self.header[3], self.header[4]]));
                if self.body == 0 {
                    self.have = 0;
                }
            }
        } else {
            self.body -= bytes.len();
            if self.body == 0 {
                self.have = 0;
            }
        }
    }

    fn next(&self) -> usize {
        if self.have < 5 {
            5 - self.have
        } else {
            self.body
        }
    }
}

impl Read for BoundedRead<'_, '_> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        let limit = self.state.next().min(buf.len());
        let n = self.fd.try_recv(&mut buf[..limit])?;
        self.state.advance(&buf[..n]);
        Ok(n)
    }
}

/// Post-handshake records received on a kernel TLS socket.
///
/// Ticket bodies are skipped rather than buffered, so a pooled connection
/// holds only a fixed few bytes of parser state and needs no admission charge.
#[derive(Default)]
pub(crate) struct ControlRecords {
    /// Partial four-byte handshake message header.
    header: [u8; 4],

    /// Header bytes collected so far.
    have: usize,

    /// Body bytes of the current ticket still to skip.
    skip: usize,
}

impl ControlRecords {
    /// Accept a non-application record fragment. Complete NewSessionTicket
    /// messages are discarded; anything else ends the connection.
    pub(crate) fn absorb(&mut self, kind: u8, mut bytes: &[u8]) -> std::result::Result<(), ()> {
        const HANDSHAKE: u8 = 22;
        const NEW_SESSION_TICKET: u8 = 4;
        if kind != HANDSHAKE {
            // Alerts (including close_notify), KeyUpdate handling, and unknown
            // record types are not supported on a pooled connection.
            return Err(());
        }
        while !bytes.is_empty() {
            if self.skip > 0 {
                let n = self.skip.min(bytes.len());
                self.skip -= n;
                bytes = &bytes[n..];
                if self.skip == 0 {
                    TICKETS.fetch_add(1, Ordering::Relaxed);
                }
                continue;
            }
            let n = (4 - self.have).min(bytes.len());
            self.header[self.have..self.have + n].copy_from_slice(&bytes[..n]);
            self.have += n;
            bytes = &bytes[n..];
            if self.have < 4 {
                break;
            }
            self.have = 0;
            if self.header[0] != NEW_SESSION_TICKET {
                return Err(());
            }
            let length = (usize::from(self.header[1]) << 16)
                | (usize::from(self.header[2]) << 8)
                | usize::from(self.header[3]);
            if 4 + length > MAX_CONTROL_BYTES {
                return Err(());
            }
            if length == 0 {
                TICKETS.fetch_add(1, Ordering::Relaxed);
            }
            self.skip = length;
        }
        Ok(())
    }

    /// Whether no partial message is outstanding.
    #[cfg(test)]
    fn idle(&self) -> bool {
        self.have == 0 && self.skip == 0
    }
}

/// Decrypted plaintext queued toward the pool end of the relay.
const RELAY_QUEUE: usize = 256 * 1024;

/// Scratch for one read from either side of the relay.
const RELAY_SCRATCH: usize = 64 * 1024;

/// Bytes charged per userspace relay for its queue and scratch buffers.
pub const RELAY_ALLOCATION: usize = RELAY_QUEUE + RELAY_SCRATCH;

/// A running userspace relay and the charge for its buffers.
///
/// Dropping it shuts down the plaintext socket so the relay observes a
/// hangup, joins the relay thread, and only then releases the charge. The
/// relay never blocks without polling the plaintext socket, so the join is
/// bounded by one nonblocking pass of its loop.
pub(crate) struct Relay<K> {
    app: OwnedFd,

    thread: Option<std::thread::JoinHandle<()>>,

    charge: Option<K>,
}

impl<K> Drop for Relay<K> {
    fn drop(&mut self) {
        // SAFETY: shutdown on a live owned descriptor.
        unsafe { libc::shutdown(self.app.as_raw_fd(), libc::SHUT_RDWR) };
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
        drop(self.charge.take());
    }
}

/// Start the userspace relay, returning the plaintext end for the pool and
/// the handle that retains `charge` until the relay thread exits.
fn spawn_relay<K>(
    session: ClientConnection,
    tcp: &Descriptor,
    charge: K,
) -> std::io::Result<(Descriptor, Relay<K>)> {
    // SAFETY: dup of a live descriptor; the result is immediately owned.
    let net = unsafe { libc::fcntl(tcp.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
    if net < 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: fcntl returned a fresh descriptor.
    let net = unsafe { OwnedFd::from_raw_fd(net) };
    let mut pair = [0; 2];
    // SAFETY: socketpair writes two descriptors on success.
    if unsafe {
        libc::socketpair(
            libc::AF_UNIX,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
            pair.as_mut_ptr(),
        )
    } < 0
    {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: both descriptors are fresh and owned here.
    let (pool_end, relay_end) =
        unsafe { (OwnedFd::from_raw_fd(pair[0]), OwnedFd::from_raw_fd(pair[1])) };
    // SAFETY: dup of a live descriptor; the result is immediately owned.
    let control = unsafe { libc::fcntl(pool_end.as_raw_fd(), libc::F_DUPFD_CLOEXEC, 0) };
    if control < 0 {
        return Err(std::io::Error::last_os_error());
    }
    // SAFETY: fcntl returned a fresh descriptor.
    let control = unsafe { OwnedFd::from_raw_fd(control) };
    let thread = std::thread::Builder::new()
        .name("racer-tls-relay".into())
        .spawn(move || relay_loop(session, net, relay_end))?;
    Ok((
        Descriptor::from(pool_end),
        Relay {
            app: control,
            thread: Some(thread),
            charge: Some(charge),
        },
    ))
}

/// Shuttle bytes until either side closes or fails.
fn relay_loop(mut session: ClientConnection, net: OwnedFd, app: OwnedFd) {
    const CAP: usize = RELAY_QUEUE;
    let net = Descriptor::from(net);
    let app = Descriptor::from(app);
    let mut to_app = Vec::with_capacity(CAP);
    let mut sent = 0;
    let mut scratch = vec![0; RELAY_SCRATCH];
    let mut net_eof = false;
    loop {
        // Move decrypted plaintext out of rustls while there is room.
        while to_app.len() < CAP {
            let room = (CAP - to_app.len()).min(scratch.len());
            match session.reader().read(&mut scratch[..room]) {
                Ok(0) => {
                    net_eof = true;
                    break;
                }
                Ok(n) => to_app.extend_from_slice(&scratch[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(_) => return,
            }
        }
        while sent < to_app.len() {
            match app.try_send(&to_app[sent..]) {
                Ok(n) => sent += n,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(_) => return,
            }
        }
        if sent == to_app.len() {
            to_app.clear();
            sent = 0;
            if net_eof {
                let _ = app.shutdown(libc::SHUT_WR);
                // Wait for the pool to close its end, then exit.
                let mut fds = [libc::pollfd {
                    fd: app.as_raw_fd(),
                    events: libc::POLLHUP,
                    revents: 0,
                }];
                // SAFETY: one live pollfd.
                unsafe { libc::poll(fds.as_mut_ptr(), 1, -1) };
                return;
            }
        }
        while session.wants_write() {
            match session.write_tls(&mut Socket(&net)) {
                Ok(_) => (),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                Err(_) => return,
            }
        }
        let backlog = session.wants_write();
        let mut fds = [
            libc::pollfd {
                fd: net.as_raw_fd(),
                events: (if !net_eof && to_app.len() < CAP && session.wants_read() {
                    libc::POLLIN
                } else {
                    0
                }) | (if backlog { libc::POLLOUT } else { 0 }),
                revents: 0,
            },
            libc::pollfd {
                fd: app.as_raw_fd(),
                events: (if backlog { 0 } else { libc::POLLIN })
                    | (if to_app.is_empty() { 0 } else { libc::POLLOUT }),
                revents: 0,
            },
        ];
        // SAFETY: two live pollfds.
        let ready = unsafe { libc::poll(fds.as_mut_ptr(), 2, -1) };
        if ready < 0 {
            if std::io::Error::last_os_error().kind() == std::io::ErrorKind::Interrupted {
                continue;
            }
            return;
        }
        if fds[0].revents & (libc::POLLIN | libc::POLLHUP | libc::POLLERR) != 0 {
            match session.read_tls(&mut Socket(&net)) {
                Ok(0) => net_eof = true,
                Ok(_) => {
                    if session.process_new_packets().is_err() {
                        let _ = session.write_tls(&mut Socket(&net));
                        return;
                    }
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(_) => return,
            }
        }
        if fds[1].revents & libc::POLLNVAL != 0 {
            return;
        }
        if fds[1].revents & (libc::POLLIN | libc::POLLHUP | libc::POLLERR) != 0 {
            match app.try_recv(&mut scratch) {
                Ok(0) => {
                    session.send_close_notify();
                    let _ = session.write_tls(&mut Socket(&net));
                    return;
                }
                Ok(n) => {
                    if session.writer().write_all(&scratch[..n]).is_err() {
                        return;
                    }
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(_) => return,
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn record_reader_never_crosses_record_boundaries() {
        let mut reader = RecordReader::default();
        assert_eq!(reader.next(), 5);
        reader.advance(&[22, 3, 3]);
        assert_eq!(reader.next(), 2);
        reader.advance(&[0, 3]);
        assert_eq!(reader.next(), 3);
        reader.advance(&[1, 2]);
        assert_eq!(reader.next(), 1);
        reader.advance(&[3]);
        assert_eq!(reader.next(), 5);
        reader.advance(&[23, 3, 3, 0, 0]);
        assert_eq!(reader.next(), 5);
    }

    #[test]
    fn control_records_discard_tickets_and_reject_others() {
        let mut control = ControlRecords::default();
        // A ticket split across two fragments, followed by a second ticket.
        assert!(control.absorb(22, &[4, 0, 0, 3, 1]).is_ok());
        assert!(!control.idle());
        assert!(control.absorb(22, &[2, 3, 4, 0, 0, 0]).is_ok());
        assert!(control.idle());
        // KeyUpdate.
        assert!(control.absorb(22, &[24, 0, 0, 1, 0]).is_err());
        // Alert.
        assert!(ControlRecords::default().absorb(21, &[1, 0]).is_err());
        // Oversized, rejected from the header alone.
        let length = MAX_CONTROL_BYTES - 3;
        let header = [4, (length >> 16) as u8, (length >> 8) as u8, length as u8];
        assert!(ControlRecords::default().absorb(22, &header).is_err());
    }

    #[test]
    fn control_records_hold_no_ticket_bytes() {
        // The largest accepted ticket, delivered one byte at a time, with the
        // header split across fragments; state stays fixed size throughout.
        let length = MAX_CONTROL_BYTES - 4;
        let mut control = ControlRecords::default();
        let header = [4, (length >> 16) as u8, (length >> 8) as u8, length as u8];
        assert!(control.absorb(22, &header[..1]).is_ok());
        assert!(control.absorb(22, &header[1..]).is_ok());
        for _ in 0..length {
            assert!(!control.idle());
            assert!(control.absorb(22, &[0xaa]).is_ok());
        }
        assert!(control.idle());
        // Empty ticket bodies and empty fragments are accepted.
        assert!(control.absorb(22, &[4, 0, 0, 0]).is_ok());
        assert!(control.absorb(22, &[]).is_ok());
        assert!(control.idle());
        // A non-ticket header after a ticket in the same fragment fails.
        assert!(control.absorb(22, &[4, 0, 0, 1, 9, 2, 0, 0, 0]).is_err());
    }

    #[test]
    fn crypto_info_layouts_match_linux() {
        use rustls::crypto::cipher::{AeadKey, Iv};
        let iv: [u8; 12] = std::array::from_fn(|i| i as u8);
        // AeadKey::from always holds 32 bytes, which the 128-bit layout rejects.
        let short = ConnectionTrafficSecrets::Aes128Gcm {
            key: AeadKey::from([7u8; 32]),
            iv: Iv::new(iv),
        };
        assert!(crypto_info(TLS_1_3_VERSION, 5, &short).is_err());
        let aes = ConnectionTrafficSecrets::Aes256Gcm {
            key: AeadKey::from([7u8; 32]),
            iv: Iv::new(iv),
        };
        let info = crypto_info(TLS_1_3_VERSION, 5, &aes).unwrap();
        assert_eq!(info.len(), 4 + 8 + 32 + 4 + 8);
        assert_eq!(&info[..4], &[0x04, 0x03, 52, 0][..]);
        assert_eq!(&info[4..12], &iv[4..]);
        assert_eq!(&info[info.len() - 12..info.len() - 8], &iv[..4]);
        assert_eq!(&info[info.len() - 8..], &5u64.to_be_bytes());
        let chacha = ConnectionTrafficSecrets::Chacha20Poly1305 {
            key: AeadKey::from([9u8; 32]),
            iv: Iv::new(iv),
        };
        let info = crypto_info(TLS_1_2_VERSION, 1, &chacha).unwrap();
        assert_eq!(info.len(), 4 + 12 + 32 + 8);
        assert_eq!(&info[4..16], &iv);
    }
}
