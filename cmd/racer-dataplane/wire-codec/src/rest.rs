//! Bounded TLS 1.3 JSON HTTP on caller-owned readiness, deadlines, and leases.
//!
//! Connections and the single idle slot stay on their I/O owner's thread. TLS
//! authenticates peers; endpoint health and identity admission belong to callers.
use std::{
    cell::RefCell,
    io::{Read, Write},
    net::{SocketAddr, TcpStream},
    os::fd::FromRawFd,
    path::{Path, PathBuf},
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime},
};
pub use uring_runtime::Operation;
use uring_runtime::{Scope as _, reactor::descriptor::Descriptor};

/// Transport failures contain no credentials, headers, or body bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The endpoint or local transport configuration is invalid.
    InvalidConfiguration,
    /// The request or response violates the supported HTTP contract.
    InvalidRequest,
    /// Trust, identity, or TLS authentication failed.
    Unauthorized,
    /// No endpoint is available, or TLS admission is temporarily saturated.
    Unavailable,
    /// A configured byte or count limit was exceeded.
    Overloaded,
    /// Socket or local I/O failed.
    Io,
    /// An internal encoding operation failed.
    Internal,
}

/// A transport result, optionally carrying the caller's richer error type.
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Narrowing must preserve cancellation and all caller policy, never extend time.
pub trait Scope: uring_runtime::Scope {
    /// Return the caller's absolute monotonic deadline.
    fn deadline(&self) -> Instant;

    /// Retain caller policy while limiting the deadline to `until`.
    fn narrowed(&self, until: Instant) -> Self;
}

/// Readiness retains the descriptor and optional lease until deregistration,
/// including cancellation cleanup. Futures run on the caller's sole I/O owner.
pub trait Io: 'static {
    /// Caller errors preserve cancellation and transport failure classification.
    type Error: Copy + Send + PartialEq + From<Error> + From<uring_runtime::Error> + 'static;

    /// Caller policy governing each I/O operation.
    type Scope: Scope<Error = Self::Error>;

    /// Admission charge retained through readiness cleanup.
    type Lease: 'static;

    /// Caller-owned sensitive bytes, retaining any read admission charge.
    type FileBytes: AsRef<[u8]>;

    /// Acquire an optional admission charge before opening a connection.
    fn lease(&self) -> Result<Option<Rc<Self::Lease>>, Self::Error>;

    /// Wait for requested readiness while retaining the descriptor and lease.
    fn ready<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        read: bool,
        write: bool,
        lease: Option<Rc<Self::Lease>>,
        scope: &'a Self::Scope,
    ) -> Operation<'a, (), Self::Error>;

    /// Resolve addresses using the caller's owner-local resolver.
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a Self::Scope,
    ) -> Operation<'a, Vec<SocketAddr>, Self::Error>;

    /// Read a fresh file under a byte limit and the caller's scope.
    fn read_file<'a>(
        &'a self,
        path: &'a Path,
        limit: usize,
        scope: &'a Self::Scope,
    ) -> Operation<'a, Self::FileBytes, Self::Error>;
}

/// HTTPS endpoint and per-operation input limits.
pub struct Config {
    /// HTTPS authority without a path, query, or credentials.
    pub url: String,

    /// PEM trust file reread on every connection checkout.
    pub trust_bundle: PathBuf,

    /// Maximum bytes admitted from the trust file.
    pub max_trust_bundle: usize,

    /// Maximum response body size for statuses other than 200.
    pub max_error_body: usize,
}

/// Borrowed DER certificates and PKCS#8 key. Caller validates identity policy.
#[derive(Clone, Copy)]
pub struct Identity<'a> {
    /// Client certificate followed by its issuing chain.
    pub certificate_chain: &'a [Vec<u8>],

    /// Borrowed PKCS#8 private key, copied only into TLS-owned storage.
    pub private_key: &'a [u8],

    /// Identity expiration checked before and during I/O.
    pub expires: SystemTime,
}

/// Methods supported by the generic transport; route policy belongs to callers.
#[derive(Clone, Copy)]
pub enum Method {
    /// Fetch a JSON resource.
    Get,
    /// Submit a JSON resource.
    Post,
}

/// Borrowed request data and the maximum successful response size.
pub struct Request<'a> {
    /// HTTP method selected by the caller.
    pub method: Method,

    /// Absolute path with no whitespace or control bytes.
    pub path: &'a str,

    /// Optional bearer credential; authentication policy belongs to the caller.
    pub bearer: Option<&'a str>,

    /// Optional non-reserved header; value semantics belong to the caller.
    pub header: Option<(&'a str, &'a str)>,

    /// JSON bytes sent without interpretation.
    pub body: &'a [u8],

    /// Maximum response body size for status 200.
    pub limit: usize,
}

/// Owner-local HTTPS transport with at most one idle authenticated connection.
pub struct Transport<I: Io + ?Sized> {
    config: Config,

    io: RefCell<Option<Rc<I>>>,

    idle: Rc<RefCell<Option<Connection<I>>>>,
}

/// An exclusive TLS connection, consumed by one request before possible reuse.
pub struct Connection<I: Io + ?Sized> {
    charge: Option<Rc<I::Lease>>,

    stream: Stream,

    fd: Rc<Descriptor>,

    tls: rustls::ClientConnection,

    io: Rc<I>,

    host: String,

    authentication: Authentication<I>,

    max_error_body: usize,
}

/// A framed JSON response whose body is erased when it is dropped.
pub struct Response {
    /// HTTP status, including retryable server failures.
    pub status: u16,

    /// Owned response bytes, zeroized on drop.
    pub body: Vec<u8>,

    /// Parsed Retry-After delay, relative to receipt for HTTP dates.
    pub retry_after: Option<Duration>,
}

/// Bootstrap connections cannot carry authenticated pooling metadata.
enum Authentication<I: Io + ?Sized> {
    Bootstrap,
    Authenticated(Authenticated<I>),
}

/// Identity lifetime and trust epoch required for authenticated reuse.
struct Authenticated<I: Io + ?Sized> {
    expires: SystemTime,

    epoch: [u8; 32],

    idle: std::rc::Weak<RefCell<Option<Connection<I>>>>,

    idle_since: Instant,

    /// Redistribution is checked only between requests, never during a long poll.
    retire_at: Instant,
}

const AUTHENTICATED_AGE_MIN: Duration = Duration::from_secs(240);
const AUTHENTICATED_AGE_JITTER_MS: u64 = 60_000;

/// Spread authenticated retirement over the inclusive four-to-five-minute range.
fn authenticated_age(random: u64) -> Duration {
    AUTHENTICATED_AGE_MIN + Duration::from_millis(random % (AUTHENTICATED_AGE_JITTER_MS + 1))
}
/// A real or simulated nonblocking byte stream owned by this I/O thread.
enum Stream {
    Real(TcpStream),
    #[cfg(feature = "simulation")]
    Sim(Rc<Descriptor>),
}
impl Read for Stream {
    /// Read without waiting; the reactor handles readiness separately.
    fn read(&mut self, bytes: &mut [u8]) -> std::io::Result<usize> {
        match self {
            Self::Real(stream) => stream.read(bytes),
            #[cfg(feature = "simulation")]
            Self::Sim(fd) => fd.try_recv(bytes),
        }
    }
}
impl Write for Stream {
    /// Write without waiting; the reactor handles readiness separately.
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        match self {
            Self::Real(stream) => stream.write(bytes),
            #[cfg(feature = "simulation")]
            Self::Sim(fd) => fd.try_send(bytes),
        }
    }

    /// Flush the underlying stream, which has no simulation-side buffer.
    fn flush(&mut self) -> std::io::Result<()> {
        match self {
            Self::Real(stream) => stream.flush(),
            #[cfg(feature = "simulation")]
            Self::Sim(_) => Ok(()),
        }
    }
}
impl Drop for Response {
    /// Erase response plaintext before releasing its allocation.
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.body.zeroize();
    }
}
/// Validated TLS server name, HTTP authority, and connection port.
struct Endpoint {
    host: String,

    authority: String,

    port: u16,
}
/// Accept only a TLS authority, preserving its spelling for the Host header.
fn endpoint(url: &str) -> Result<Endpoint> {
    let authority = url
        .strip_prefix("https://")
        .ok_or(Error::InvalidConfiguration)?
        .trim_end_matches('/');
    if authority.is_empty()
        || authority.contains(['/', '?', '#', '@', '\r', '\n', '\\'])
        || !authority.is_ascii()
    {
        return Err(Error::InvalidConfiguration);
    }
    let (host, port) = if let Some(s) = authority.strip_prefix('[') {
        let (host, rest) = s.split_once(']').ok_or(Error::InvalidConfiguration)?;
        host.parse::<std::net::Ipv6Addr>()
            .map_err(|_| Error::InvalidConfiguration)?;
        (
            host,
            if rest.is_empty() {
                443
            } else {
                rest.strip_prefix(':')
                    .ok_or(Error::InvalidConfiguration)?
                    .parse()
                    .map_err(|_| Error::InvalidConfiguration)?
            },
        )
    } else {
        match authority.split_once(':') {
            Some((host, port)) => (host, port.parse().map_err(|_| Error::InvalidConfiguration)?),
            None => (authority, 443),
        }
    };
    rustls::pki_types::ServerName::try_from(host.to_owned())
        .map_err(|_| Error::InvalidConfiguration)?;
    if port == 0 {
        return Err(Error::InvalidConfiguration);
    }
    Ok(Endpoint {
        host: host.into(),
        authority: authority.into(),
        port,
    })
}
impl<I: Io + ?Sized> Transport<I> {
    /// Create a detached transport; attach its owner before connecting.
    pub fn new(config: Config) -> Self {
        Self {
            config,
            io: RefCell::new(None),
            idle: Rc::new(RefCell::new(None)),
        }
    }
    /// Replace the I/O owner and discard the current idle connection.
    pub fn attach_io(&self, io: Rc<I>) {
        self.idle.borrow_mut().take();
        *self.io.borrow_mut() = Some(io);
    }
    /// Retrieve the attached owner or report missing configuration.
    pub fn io(&self) -> Result<Rc<I>, I::Error> {
        self.io
            .borrow()
            .clone()
            .ok_or_else(|| Error::InvalidConfiguration.into())
    }
    /// Drop the idle connection without interrupting checked-out requests.
    pub fn close_idle(&self) {
        self.idle.borrow_mut().take();
    }
    /// Reread trust and reuse eligible authenticated TLS, or establish fresh TLS.
    pub fn connect<'a>(
        &'a self,
        identity: Option<Identity<'a>>,
        scope: &'a I::Scope,
    ) -> Operation<'a, Connection<I>, I::Error> {
        Box::pin(async move {
            scope.check()?;
            if identity.is_some_and(|i| uring_runtime::environment::wall_now() >= i.expires) {
                return Err(Error::Unauthorized.into());
            }
            let endpoint = endpoint(&self.config.url)?;
            let io = self.io()?;
            let trust = io
                .read_file(
                    &self.config.trust_bundle,
                    self.config.max_trust_bundle,
                    scope,
                )
                .await?;
            use sha2::Digest;
            let mut epoch = sha2::Sha256::new();
            epoch.update(trust.as_ref());
            if let Some(identity) = identity {
                for certificate in identity.certificate_chain {
                    epoch.update((certificate.len() as u64).to_be_bytes());
                    epoch.update(certificate);
                }
            }
            let epoch: [u8; 32] = epoch.finalize().into();
            if let Some(connection) = self.idle.borrow_mut().take()
                && let Authentication::Authenticated(auth) = &connection.authentication
                && identity.is_some()
                && auth.epoch == epoch
                && connection.within_max_age()
                && uring_runtime::environment::now().saturating_duration_since(auth.idle_since)
                    < Duration::from_secs(20)
                && connection.check(scope).is_ok()
            {
                return Ok(connection);
            }
            let mut roots = rustls::RootCertStore::empty();
            for cert in rustls_pemfile::certs(&mut trust.as_ref()) {
                roots
                    .add(cert.map_err(|_| Error::Unauthorized)?)
                    .map_err(|_| Error::Unauthorized)?;
            }
            if roots.is_empty() {
                return Err(Error::Unauthorized.into());
            }
            let builder = rustls::ClientConfig::builder_with_provider(Arc::new(
                rustls::crypto::ring::default_provider(),
            ))
            .with_protocol_versions(&[&rustls::version::TLS13])
            .map_err(|_| Error::Unauthorized)?
            .with_root_certificates(roots);
            let mut config = if let Some(i) = identity {
                builder
                    .with_client_auth_cert(
                        i.certificate_chain
                            .iter()
                            .cloned()
                            .map(rustls::pki_types::CertificateDer::from)
                            .collect(),
                        rustls::pki_types::PrivatePkcs8KeyDer::from(i.private_key.to_vec()).into(),
                    )
                    .map_err(|_| Error::Unauthorized)?
            } else {
                builder.with_no_client_auth()
            };
            config.alpn_protocols = vec![b"http/1.1".to_vec()];
            config.resumption = rustls::client::Resumption::disabled();
            let addresses = if let Ok(ip) = endpoint.host.parse() {
                vec![SocketAddr::new(ip, endpoint.port)]
            } else {
                io.resolve(&endpoint.host, endpoint.port, scope).await?
            };
            if addresses.is_empty() || addresses.len() > 64 {
                return Err(Error::Unavailable.into());
            }
            let (stream, fd, charge) = connect_addresses(io.as_ref(), &addresses, scope).await?;
            let server = rustls::pki_types::ServerName::try_from(endpoint.host)
                .map_err(|_| Error::InvalidConfiguration)?;
            let mut tls = rustls::ClientConnection::new(Arc::new(config), server)
                .map_err(|_| Error::Unauthorized)?;
            tls.set_buffer_limit(Some(64 * 1024));
            let authentication = if let Some(identity) = identity {
                let mut random = [0; 8];
                uring_runtime::environment::fill_random(&mut random).map_err(|_| Error::Io)?;
                let retire_at = uring_runtime::environment::now()
                    + authenticated_age(u64::from_ne_bytes(random));
                Authentication::Authenticated(Authenticated {
                    expires: identity.expires,
                    epoch,
                    idle: Rc::downgrade(&self.idle),
                    idle_since: uring_runtime::environment::now(),
                    retire_at,
                })
            } else {
                Authentication::Bootstrap
            };
            let mut connection = Connection {
                charge,
                stream,
                fd,
                tls,
                io,
                host: endpoint.authority,
                authentication,
                max_error_body: self.config.max_error_body,
            };
            while connection.tls.is_handshaking() {
                connection.step(scope).await?;
            }
            Ok(connection)
        })
    }
}
/// Connected socket, readiness descriptor, and retained admission charge.
type Connected<I> = (Stream, Rc<Descriptor>, Option<Rc<<I as Io>::Lease>>);

/// Try addresses in order while reserving deadline shares for fallback and TLS.
async fn connect_addresses<I: Io + ?Sized>(
    io: &I,
    addresses: &[SocketAddr],
    scope: &I::Scope,
) -> Result<Connected<I>, I::Error> {
    for (index, address) in addresses.iter().enumerate() {
        scope.check()?;
        // Reserve a share for every remaining address and one for TLS. A local
        // blackhole must not consume the parent's entire connection deadline.
        let now = uring_runtime::environment::now();
        let share =
            scope.deadline().saturating_duration_since(now) / (addresses.len() - index + 1) as u32;
        let attempt = scope.narrowed(now + share.min(Duration::from_secs(5)));
        let charge = io.lease()?;
        #[cfg(feature = "simulation")]
        if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
            if let Ok(fd) = sim.connect(uring_runtime::reactor::SocketAddress::Inet(*address)) {
                let fd = Rc::new(fd);
                return Ok((Stream::Sim(fd.clone()), fd, charge));
            }
            continue;
        }
        if let Ok(stream) = connect_socket(*address) {
            let fd = Rc::new(Descriptor::from(stream.try_clone().map_err(|_| Error::Io)?));
            let ready = io
                .ready(fd.clone(), false, true, charge.clone(), &attempt)
                .await;
            // Cancellation and overall expiry always win over local retry.
            scope.check()?;
            match ready {
                Ok(()) => {}
                Err(error)
                    if error == uring_runtime::Error::DeadlineExceeded.into()
                        || error == Error::Io.into()
                        || error == Error::Unavailable.into() =>
                {
                    continue;
                }
                Err(error) => return Err(error),
            }
            if stream.take_error().map_err(|_| Error::Io)?.is_none() {
                return Ok((Stream::Real(stream), fd, charge));
            }
        }
    }
    scope.check()?;
    Err(Error::Unavailable.into())
}

/// Begin a close-on-exec, nonblocking TCP connection.
fn connect_socket(address: SocketAddr) -> Result<TcpStream> {
    let domain = if address.is_ipv4() {
        libc::AF_INET
    } else {
        libc::AF_INET6
    };
    let raw = unsafe {
        libc::socket(
            domain,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if raw < 0 {
        return Err(Error::Io);
    }
    let stream = unsafe { TcpStream::from_raw_fd(raw) };
    let result = match address {
        SocketAddr::V4(a) => {
            let addr = libc::sockaddr_in {
                sin_family: libc::AF_INET as _,
                sin_port: a.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from_ne_bytes(a.ip().octets()),
                },
                sin_zero: [0; 8],
            };
            unsafe {
                libc::connect(
                    raw,
                    (&addr as *const libc::sockaddr_in).cast(),
                    std::mem::size_of_val(&addr) as _,
                )
            }
        }
        SocketAddr::V6(a) => {
            let addr = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as _,
                sin6_port: a.port().to_be(),
                sin6_flowinfo: a.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: a.ip().octets(),
                },
                sin6_scope_id: a.scope_id(),
            };
            unsafe {
                libc::connect(
                    raw,
                    (&addr as *const libc::sockaddr_in6).cast(),
                    std::mem::size_of_val(&addr) as _,
                )
            }
        }
    };
    if result < 0 && std::io::Error::last_os_error().raw_os_error() != Some(libc::EINPROGRESS) {
        return Err(Error::Io);
    }
    Ok(stream)
}
impl<I: Io + ?Sized> Connection<I> {
    /// Check redistribution age only at request boundaries, not during I/O.
    fn within_max_age(&self) -> bool {
        match &self.authentication {
            Authentication::Bootstrap => true,
            Authentication::Authenticated(auth) => {
                uring_runtime::environment::now() < auth.retire_at
            }
        }
    }
    /// Check caller cancellation first, then the authenticated identity lifetime.
    fn check(&self, scope: &I::Scope) -> Result<(), I::Error> {
        scope.check()?;
        if let Authentication::Authenticated(auth) = &self.authentication
            && uring_runtime::environment::wall_now() >= auth.expires
        {
            return Err(Error::Unauthorized.into());
        }
        Ok(())
    }
    /// Make bounded TLS progress, yielding and waiting on owner-local readiness.
    async fn step(&mut self, scope: &I::Scope) -> Result<(), I::Error> {
        self.check(scope)?;
        // Even a continuously readable peer must yield to other owner work.
        uring_runtime::drivers::yield_now().await;
        let mut progress = false;
        if self.tls.wants_write() {
            match self.tls.write_tls(&mut self.stream) {
                Ok(0) => return Err(Error::Io.into()),
                Ok(_) => progress = true,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(_) => return Err(Error::Io.into()),
            }
        }
        if self.tls.wants_read() {
            match self.tls.read_tls(&mut self.stream) {
                Ok(0) => return Err(Error::Io.into()),
                Ok(_) => {
                    self.tls.process_new_packets().map_err(tls_failure)?;
                    progress = true;
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                Err(_) => return Err(Error::Io.into()),
            }
        }
        if !progress {
            let mut bounded = scope.clone();
            if let Authentication::Authenticated(auth) = &self.authentication {
                let remaining = auth
                    .expires
                    .duration_since(uring_runtime::environment::wall_now())
                    .map_err(|_| Error::Unauthorized)?;
                bounded = scope.narrowed(uring_runtime::environment::now() + remaining);
            }
            self.io
                .ready(
                    self.fd.clone(),
                    self.tls.wants_read(),
                    self.tls.wants_write(),
                    self.charge.clone(),
                    &bounded,
                )
                .await?;
        }
        Ok(())
    }
    /// Successful framed requests recycle one connection within its trust/identity epoch.
    pub fn request<'a>(
        mut self,
        request: Request<'a>,
        scope: &'a I::Scope,
    ) -> Operation<'a, Response, I::Error> {
        Box::pin(async move {
            let Request {
                method,
                path,
                bearer: token,
                header,
                body,
                limit,
            } = request;
            let request = request_head(&self.host, method, path, token, body.len(), header)?;
            for bytes in [request.as_bytes(), body] {
                let mut offset = 0;
                while offset < bytes.len() {
                    self.check(scope)?;
                    match self.tls.writer().write(&bytes[offset..]) {
                        Ok(n) if n != 0 => offset += n,
                        Ok(_) => (),
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => (),
                        Err(_) => return Err(Error::Io.into()),
                    }
                    self.step(scope).await?;
                }
            }
            while self.tls.wants_write() {
                self.step(scope).await?;
            }
            let mut received = zeroize::Zeroizing::new(Vec::new());
            let mut scratch = zeroize::Zeroizing::new([0; 16384]);
            let ResponseHead {
                status,
                header_len,
                framing,
                retry_after,
                close,
            } = loop {
                if let Some(head) = parse_head(&received, limit, self.max_error_body)? {
                    break head;
                }
                if received.len() >= 16384 {
                    return Err(Error::Overloaded.into());
                }
                self.receive(&mut received, &mut scratch[..], scope).await?;
            };
            let body = match framing {
                Framing::Length(length) => {
                    if received.len() > header_len + length {
                        return Err(Error::InvalidRequest.into());
                    }
                    while received.len() < header_len + length {
                        self.receive(&mut received, &mut scratch[..], scope).await?;
                        if received.len() > header_len + length {
                            return Err(Error::InvalidRequest.into());
                        }
                    }
                    zeroize::Zeroizing::new(received[header_len..].to_vec())
                }
                Framing::Chunked => {
                    let bound = if status == 200 {
                        limit
                    } else {
                        self.max_error_body
                    };
                    self.receive_chunked(&received[header_len..], bound, &mut scratch[..], scope)
                        .await?
                }
            };
            self.check(scope)?;
            // A TLS record can contain more plaintext than the last bounded
            // receive consumed. Never recycle a connection with trailing bytes.
            let reusable = match self.tls.reader().read(&mut scratch[..1]) {
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => true,
                Ok(0) => false,
                Ok(_) => return Err(Error::InvalidRequest.into()),
                Err(_) => false,
            };
            // Failed requests (including 429/503) must reselect a Service backend.
            // An aged connection finishes its in-flight response, then retires.
            if reusable
                && !close
                && matches!(status, 200 | 204)
                && self.within_max_age()
                && let Authentication::Authenticated(auth) = &mut self.authentication
                && let Some(idle) = auth.idle.upgrade()
            {
                auth.idle_since = uring_runtime::environment::now();
                *idle.borrow_mut() = Some(self);
            }
            Ok(Response {
                status,
                body: body.to_vec(),
                retry_after,
            })
        })
    }
    /// Decode bounded chunks, rejecting extensions, trailers, and trailing bytes.
    async fn receive_chunked(
        &mut self,
        initial: &[u8],
        bound: usize,
        scratch: &mut [u8],
        scope: &I::Scope,
    ) -> Result<zeroize::Zeroizing<Vec<u8>>, I::Error> {
        let mut raw = zeroize::Zeroizing::new(initial.to_vec());
        let mut body = zeroize::Zeroizing::new(Vec::new());
        loop {
            let line_end = loop {
                if let Some(n) = raw.windows(2).position(|w| w == b"\r\n") {
                    break n;
                }
                if raw.len() > 128 {
                    return Err(Error::InvalidRequest.into());
                }
                self.receive(&mut raw, scratch, scope).await?;
            };
            if line_end == 0 || line_end > 16 || !raw[..line_end].iter().all(u8::is_ascii_hexdigit)
            {
                return Err(Error::InvalidRequest.into());
            }
            let length = usize::from_str_radix(
                std::str::from_utf8(&raw[..line_end]).map_err(|_| Error::InvalidRequest)?,
                16,
            )
            .map_err(|_| Error::Overloaded)?;
            raw.drain(..line_end + 2);
            if length > bound - body.len() {
                return Err(Error::Overloaded.into());
            }
            let mut remaining = length;
            while remaining != 0 {
                if raw.is_empty() {
                    self.receive(&mut raw, scratch, scope).await?;
                }
                let n = remaining.min(raw.len());
                append_sensitive(&mut body, &raw[..n]);
                raw.drain(..n);
                remaining -= n;
            }
            while raw.len() < 2 {
                self.receive(&mut raw, scratch, scope).await?;
            }
            if &raw[..2] != b"\r\n" {
                return Err(Error::InvalidRequest.into());
            }
            raw.drain(..2);
            if length == 0 {
                if !raw.is_empty() {
                    return Err(Error::InvalidRequest.into());
                }
                return Ok(body);
            }
        }
    }

    /// Append one available plaintext fragment without freeing unerased storage.
    async fn receive(
        &mut self,
        into: &mut zeroize::Zeroizing<Vec<u8>>,
        scratch: &mut [u8],
        scope: &I::Scope,
    ) -> Result<(), I::Error> {
        loop {
            self.check(scope)?;
            match self.tls.reader().read(scratch) {
                Ok(0) => return Err(Error::Io.into()),
                Ok(n) => {
                    append_sensitive(into, &scratch[..n]);
                    return Ok(());
                }
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => self.step(scope).await?,
                Err(_) => return Err(Error::Io.into()),
            }
        }
    }
}
/// Validate and bound request headers before copying any bearer credential.
fn request_head(
    host: &str,
    method: Method,
    path: &str,
    token: Option<&str>,
    body_length: usize,
    header: Option<(&str, &str)>,
) -> Result<zeroize::Zeroizing<String>> {
    const MAX_HEAD: usize = 16384;
    if !path.starts_with('/')
        || path.bytes().any(|b| b <= 32 || b >= 127)
        || host.bytes().any(|b| b <= 32 || b >= 127)
        || token.is_some_and(|t| t.bytes().any(|b| b <= 32 || b >= 127))
        || header.is_some_and(|(name, value)| !valid_header(name, value))
    {
        return Err(Error::InvalidRequest);
    }
    // Bound allocation before copying bearer credentials. Reserve once, so a
    // String growth cannot free an allocation still containing a token.
    let capacity = [
        host.len(),
        path.len(),
        token.map_or(0, str::len),
        header.map_or(0, |(name, _)| name.len()),
        header.map_or(0, |(_, value)| value.len()),
    ]
    .into_iter()
    .try_fold(256usize, |total, len| total.checked_add(len))
    .filter(|size| *size <= MAX_HEAD)
    .ok_or(Error::Overloaded)?;
    let mut request = zeroize::Zeroizing::new(String::with_capacity(capacity));
    use std::fmt::Write;
    let method = match method {
        Method::Get => "GET",
        Method::Post => "POST",
    };
    write!(request, "{method} {path} HTTP/1.1\r\nHost: {host}\r\nAccept: application/json\r\nContent-Type: application/json\r\nContent-Length: {body_length}\r\nConnection: keep-alive\r\n")
        .map_err(|_| Error::Internal)?;
    if let Some(token) = token {
        request.push_str("Authorization: Bearer ");
        request.push_str(token);
        request.push_str("\r\n");
    }
    if let Some((name, value)) = header {
        request.push_str(name);
        request.push_str(": ");
        request.push_str(value);
        request.push_str("\r\n");
    }
    request.push_str("\r\n");
    Ok(request)
}

/// Grow without freeing an allocation containing plaintext key material.
fn append_sensitive(into: &mut zeroize::Zeroizing<Vec<u8>>, bytes: &[u8]) {
    if into.capacity() - into.len() < bytes.len() {
        let capacity = (into.len() + bytes.len()).max(into.capacity().saturating_mul(2));
        let mut next = zeroize::Zeroizing::new(Vec::with_capacity(capacity));
        next.extend_from_slice(into);
        std::mem::swap(into, &mut next);
    }
    into.extend_from_slice(bytes);
}
/// Retry Go server admission saturation, but keep certificate/protocol failures terminal.
fn tls_failure(error: rustls::Error) -> Error {
    match error {
        rustls::Error::AlertReceived(rustls::AlertDescription::InternalError) => Error::Unavailable,
        _ => Error::Unauthorized,
    }
}

/// Supported, unambiguous HTTP body boundaries.
#[derive(Debug, Eq, PartialEq)]
enum Framing {
    Length(usize),
    Chunked,
}
/// Validated response metadata, including the connection reuse directive.
#[derive(Debug, Eq, PartialEq)]
struct ResponseHead {
    status: u16,

    header_len: usize,

    framing: Framing,

    retry_after: Option<Duration>,

    close: bool,
}

/// Parse framing and reuse metadata together, preserving failure precedence.
fn parse_head(bytes: &[u8], limit: usize, max_error_body: usize) -> Result<Option<ResponseHead>> {
    let mut headers = [httparse::EMPTY_HEADER; 64];
    let mut response = httparse::Response::new(&mut headers);
    let length = match response.parse(bytes).map_err(|_| Error::InvalidRequest)? {
        httparse::Status::Partial => return Ok(None),
        httparse::Status::Complete(n) => n,
    };
    if length > 16384 || response.version != Some(1) {
        return Err(Error::InvalidRequest);
    }
    let status = response.code.ok_or(Error::InvalidRequest)?;
    let mut content_length = None;
    let mut retry = None;
    let mut content_type = false;
    let mut chunked = false;
    let mut close = false;
    for h in response.headers {
        if h.name.eq_ignore_ascii_case("content-encoding") {
            return Err(Error::InvalidRequest);
        }
        if h.name.eq_ignore_ascii_case("transfer-encoding") {
            if chunked || !h.value.eq_ignore_ascii_case(b"chunked") {
                return Err(Error::InvalidRequest);
            }
            chunked = true;
        }
        if h.name.eq_ignore_ascii_case("content-length") {
            if content_length.is_some() {
                return Err(Error::InvalidRequest);
            }
            let s = std::str::from_utf8(h.value).map_err(|_| Error::InvalidRequest)?;
            if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
                return Err(Error::InvalidRequest);
            }
            content_length = Some(s.parse::<usize>().map_err(|_| Error::Overloaded)?);
        }
        if h.name.eq_ignore_ascii_case("content-type") {
            if content_type
                || !std::str::from_utf8(h.value)
                    .map_err(|_| Error::InvalidRequest)?
                    .split(';')
                    .next()
                    .is_some_and(|s| s.trim().eq_ignore_ascii_case("application/json"))
            {
                return Err(Error::InvalidRequest);
            }
            content_type = true;
        }
        if h.name.eq_ignore_ascii_case("retry-after") {
            if retry.is_some() {
                return Err(Error::InvalidRequest);
            }
            retry = Some(retry_delay(h.value)?);
        }
        if h.name.eq_ignore_ascii_case("connection") {
            // Defer invalid UTF-8 until after framing validation, as before.
            if let Ok(value) = std::str::from_utf8(h.value) {
                close |= value
                    .split(',')
                    .any(|token| token.trim().eq_ignore_ascii_case("close"));
            }
        }
    }
    if chunked && (content_length.is_some() || status == 204) {
        return Err(Error::InvalidRequest);
    }
    let size = if status == 204 {
        if content_length.is_some_and(|n| n != 0) {
            return Err(Error::InvalidRequest);
        }
        0
    } else {
        if !content_type {
            return Err(Error::InvalidRequest);
        }
        if chunked {
            0
        } else {
            content_length.ok_or(Error::InvalidRequest)?
        }
    };
    let bound = if status == 200 { limit } else { max_error_body };
    if size > bound {
        return Err(Error::Overloaded);
    }
    // The old request path required UTF-8 even in ignored headers. Keep that
    // acceptance rule after size/framing checks, without reparsing Connection.
    std::str::from_utf8(&bytes[..length]).map_err(|_| Error::InvalidRequest)?;
    Ok(Some(ResponseHead {
        status,
        header_len: length,
        framing: if chunked {
            Framing::Chunked
        } else {
            Framing::Length(size)
        },
        retry_after: retry,
        close,
    }))
}
/// Decode delta seconds or IMF-fixdate without locale or time-zone globals.
fn retry_delay(bytes: &[u8]) -> Result<Duration> {
    let text = std::str::from_utf8(bytes).map_err(|_| Error::InvalidRequest)?;
    if !text.is_empty() && text.bytes().all(|b| b.is_ascii_digit()) {
        return text
            .parse::<u64>()
            .map(Duration::from_secs)
            .map_err(|_| Error::InvalidRequest);
    }
    // IMF-fixdate, the preferred HTTP-date form. No locale or time-zone globals.
    let parts: Vec<_> = text.split(' ').collect();
    if parts.len() != 6
        || !["Mon,", "Tue,", "Wed,", "Thu,", "Fri,", "Sat,", "Sun,"].contains(&parts[0])
        || parts[5] != "GMT"
    {
        return Err(Error::InvalidRequest);
    }
    let number = |s: &str| -> Result<i64> {
        if s.is_empty() || !s.bytes().all(|b| b.is_ascii_digit()) {
            return Err(Error::InvalidRequest);
        }
        s.parse().map_err(|_| Error::InvalidRequest)
    };
    let day = number(parts[1])?;
    let year = number(parts[3])?;
    let month = [
        "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
    ]
    .iter()
    .position(|s| *s == parts[2])
    .ok_or(Error::InvalidRequest)? as i64
        + 1;
    let time: Vec<_> = parts[4].split(':').collect();
    if time.len() != 3 || !(1970..=9999).contains(&year) {
        return Err(Error::InvalidRequest);
    }
    let hour = number(time[0])?;
    let minute = number(time[1])?;
    let second = number(time[2])?;
    let leap = year % 4 == 0 && (year % 100 != 0 || year % 400 == 0);
    let days = [
        31,
        if leap { 29 } else { 28 },
        31,
        30,
        31,
        30,
        31,
        31,
        30,
        31,
        30,
        31,
    ];
    if day < 1 || day > days[month as usize - 1] || hour > 23 || minute > 59 || second > 59 {
        return Err(Error::InvalidRequest);
    }
    let y = year - i64::from(month <= 2);
    let era = y / 400;
    let yoe = y - era * 400;
    let doy = (153 * (month + if month > 2 { -3 } else { 9 }) + 2) / 5 + day - 1;
    let days = era * 146097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719468;
    let seconds = days * 86400 + hour * 3600 + minute * 60 + second;
    let target = std::time::UNIX_EPOCH
        + Duration::from_secs(seconds.try_into().map_err(|_| Error::InvalidRequest)?);
    Ok(target
        .duration_since(uring_runtime::environment::wall_now())
        .unwrap_or_default())
}

/// Accept printable extension headers but never overrides of transport metadata.
fn valid_header(name: &str, value: &str) -> bool {
    !name.is_empty()
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&b))
        && value.bytes().all(|b| (32..127).contains(&b))
        && ![
            "host",
            "authorization",
            "proxy-authorization",
            "content-length",
            "transfer-encoding",
            "content-type",
            "content-encoding",
            "accept",
            "accept-encoding",
            "connection",
            "te",
            "trailer",
            "upgrade",
            "expect",
        ]
        .iter()
        .any(|reserved| name.eq_ignore_ascii_case(reserved))
}

/// Bounded UDP DNS on caller-owned readiness. TLS, never DNS, authenticates the host.
pub mod dns {
    use super::{Error, Io, Result, Scope};
    use std::{
        net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, UdpSocket},
        rc::Rc,
        time::Duration,
    };
    use uring_runtime::{Scope as _, reactor::descriptor::Descriptor};

    /// Resolve through bounded owner-local UDP queries and resolv.conf search rules.
    pub async fn resolve<I: Io + ?Sized>(
        io: &I,
        host: &str,
        port: u16,
        scope: &I::Scope,
    ) -> Result<Vec<SocketAddr>, I::Error> {
        scope.check()?;
        // Configuration and network I/O both use the owner-local reactor.
        let config = io
            .read_file(std::path::Path::new("/etc/resolv.conf"), 64 * 1024, scope)
            .await?;
        let text = std::str::from_utf8(config.as_ref()).map_err(|_| Error::InvalidConfiguration)?;
        let mut servers = Vec::new();
        let mut search = Vec::new();
        let mut ndots = 1;
        for line in text.lines() {
            let mut fields = line
                .split(['#', ';'])
                .next()
                .unwrap_or("")
                .split_whitespace();
            match fields.next() {
                Some("nameserver") => {
                    if let Some(ip) = fields.next().and_then(|s| s.parse::<IpAddr>().ok()) {
                        servers.push(SocketAddr::new(ip, 53));
                    }
                }
                Some("search" | "domain") => search = fields.take(6).map(str::to_owned).collect(),
                Some("options") => {
                    for option in fields {
                        if let Some(n) = option
                            .strip_prefix("ndots:")
                            .and_then(|s| s.parse::<usize>().ok())
                        {
                            ndots = n.min(15);
                        }
                    }
                }
                _ => (),
            }
        }
        servers.truncate(3);
        if servers.is_empty() {
            return Err(Error::Unavailable.into());
        }
        let mut names = Vec::new();
        let absolute_first =
            host.ends_with('.') || host.bytes().filter(|b| *b == b'.').count() >= ndots;
        if absolute_first {
            names.push(host.trim_end_matches('.').to_owned());
        }
        if !host.ends_with('.') {
            for suffix in search {
                names.push(format!("{host}.{suffix}"));
            }
        }
        if !absolute_first {
            names.push(host.to_owned());
        }
        for name in names {
            for server in &servers {
                let mut addresses = Vec::new();
                for kind in [1u16, 28] {
                    let attempt =
                        scope.narrowed(uring_runtime::environment::now() + Duration::from_secs(2));
                    match query(io, *server, &name, kind, &attempt).await {
                        Ok(ips) => {
                            addresses.extend(ips.into_iter().map(|ip| SocketAddr::new(ip, port)))
                        }
                        Err(error) if error == uring_runtime::Error::Cancelled.into() => {
                            return Err(error);
                        }
                        Err(_) => {
                            scope.check()?;
                        }
                    }
                }
                if !addresses.is_empty() {
                    addresses.truncate(64);
                    return Ok(addresses);
                }
            }
        }
        Err(Error::Unavailable.into())
    }

    /// A connected real or simulated DNS datagram socket.
    enum Datagram {
        Real(UdpSocket),
        #[cfg(feature = "simulation")]
        Sim(Rc<Descriptor>),
    }
    impl Datagram {
        /// Bind an ephemeral socket and restrict its peer to the chosen resolver.
        fn connect(server: SocketAddr) -> Result<(Self, Rc<Descriptor>)> {
            #[cfg(feature = "simulation")]
            if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
                let fd = sim
                    .bind_datagram(
                        if server.is_ipv4() {
                            "127.0.0.1:0"
                        } else {
                            "[::1]:0"
                        }
                        .parse()
                        .unwrap(),
                    )
                    .map_err(|_| Error::Io)?;
                let Some(handle) = fd.as_sim() else {
                    unreachable!()
                };
                handle.connect_datagram(server).map_err(|_| Error::Io)?;
                let fd = Rc::new(fd);
                return Ok((Self::Sim(fd.clone()), fd));
            }
            let socket = UdpSocket::bind(if server.is_ipv4() {
                "0.0.0.0:0"
            } else {
                "[::]:0"
            })
            .map_err(|_| Error::Io)?;
            socket.set_nonblocking(true).map_err(|_| Error::Io)?;
            socket.connect(server).map_err(|_| Error::Io)?;
            let fd = Rc::new(Descriptor::from(socket.try_clone().map_err(|_| Error::Io)?));
            Ok((Self::Real(socket), fd))
        }

        /// Send one query without waiting for readiness.
        fn send(&self, bytes: &[u8]) -> std::io::Result<usize> {
            match self {
                Self::Real(socket) => socket.send(bytes),
                #[cfg(feature = "simulation")]
                Self::Sim(fd) => {
                    let Some(h) = fd.as_sim() else { unreachable!() };
                    h.send_datagram(bytes)
                }
            }
        }

        /// Receive one answer from the connected resolver without waiting.
        fn recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
            match self {
                Self::Real(socket) => socket.recv(bytes),
                #[cfg(feature = "simulation")]
                Self::Sim(fd) => {
                    let Some(h) = fd.as_sim() else { unreachable!() };
                    h.recv_from(bytes).map(|(n, _)| n)
                }
            }
        }
    }

    /// Exchange one bounded DNS question with a random transaction identifier.
    async fn query<I: Io + ?Sized>(
        io: &I,
        server: SocketAddr,
        name: &str,
        kind: u16,
        scope: &I::Scope,
    ) -> Result<Vec<IpAddr>, I::Error> {
        let mut id = [0; 2];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
        let mut request = vec![id[0], id[1], 1, 0, 0, 1, 0, 0, 0, 0, 0, 0];
        if name.len() > 253 {
            return Err(Error::InvalidConfiguration.into());
        }
        for label in name.split('.') {
            if label.is_empty() || label.len() > 63 || !label.is_ascii() {
                return Err(Error::InvalidConfiguration.into());
            }
            request.push(label.len() as u8);
            request.extend_from_slice(label.as_bytes());
        }
        request.push(0);
        request.extend_from_slice(&kind.to_be_bytes());
        request.extend_from_slice(&[0, 1]);
        let (socket, fd) = Datagram::connect(server)?;
        loop {
            scope.check()?;
            match socket.send(&request) {
                Ok(n) if n == request.len() => break,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    io.ready(fd.clone(), false, true, None, scope).await?
                }
                _ => return Err(Error::Io.into()),
            }
        }
        let mut response = [0; 4096];
        loop {
            scope.check()?;
            match socket.recv(&mut response) {
                Ok(n) => return parse(&response[..n], &request, kind).map_err(Into::into),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    io.ready(fd.clone(), true, false, None, scope).await?
                }
                _ => return Err(Error::Io.into()),
            }
        }
    }

    /// Read a network-order integer without indexing beyond the packet.
    fn u16_at(b: &[u8], p: usize) -> Result<u16> {
        Ok(u16::from_be_bytes(
            b.get(p..p + 2)
                .ok_or(Error::InvalidRequest)?
                .try_into()
                .unwrap(),
        ))
    }

    /// Skip bounded labels or a compression pointer without following pointers.
    fn skip_name(b: &[u8], p: &mut usize) -> Result<()> {
        for _ in 0..128 {
            let n = *b.get(*p).ok_or(Error::InvalidRequest)?;
            *p += 1;
            if n == 0 {
                return Ok(());
            }
            if n & 0xc0 == 0xc0 {
                b.get(*p).ok_or(Error::InvalidRequest)?;
                *p += 1;
                return Ok(());
            }
            if n > 63 {
                return Err(Error::InvalidRequest);
            }
            *p += n as usize;
            if *p > b.len() {
                return Err(Error::InvalidRequest);
            }
        }
        Err(Error::InvalidRequest)
    }

    /// Validate the echoed question and collect bounded A or AAAA answers.
    fn parse(b: &[u8], request: &[u8], kind: u16) -> Result<Vec<IpAddr>> {
        if b.len() < request.len()
            || b[..2] != request[..2]
            || b[2] & 0xfa != 0x80
            || b[3] & 0x0f != 0
            || u16_at(b, 4)? != 1
            || b[12..request.len()] != request[12..]
        {
            return Err(Error::Unavailable);
        }
        let mut p = request.len();
        let count = u16_at(b, 6)? as usize;
        if count > 128 {
            return Err(Error::Overloaded);
        }
        let mut addresses = Vec::new();
        for _ in 0..count {
            skip_name(b, &mut p)?;
            let rr = u16_at(b, p)?;
            let class = u16_at(b, p + 2)?;
            let len = u16_at(b, p + 8)? as usize;
            p += 10;
            let data = b.get(p..p + len).ok_or(Error::InvalidRequest)?;
            p += len;
            if class == 1 && rr == kind {
                match (rr, len) {
                    (1, 4) => addresses.push(IpAddr::V4(Ipv4Addr::from(
                        <[u8; 4]>::try_from(data).unwrap(),
                    ))),
                    (28, 16) => addresses.push(IpAddr::V6(Ipv6Addr::from(
                        <[u8; 16]>::try_from(data).unwrap(),
                    ))),
                    _ => return Err(Error::InvalidRequest),
                }
            }
        }
        Ok(addresses)
    }

    /// DNS wire parser boundary coverage.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Reject truncated packets, changed questions, and incomplete answers.
        #[test]
        fn rejects_truncation_wrong_question_and_malformed_answers() {
            let request = [1, 2, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, b'a', 0, 0, 1, 0, 1];
            let mut response = request.to_vec();
            response[2] = 0x81;
            response[3] = 0x80;
            response[7] = 1;
            response.extend_from_slice(&[0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4, 192, 0, 2, 1]);
            assert_eq!(
                parse(&response, &request, 1).unwrap(),
                vec!["192.0.2.1".parse::<IpAddr>().unwrap()]
            );
            response[2] |= 2;
            assert!(parse(&response, &request, 1).is_err());
            response[2] &= !2;
            response[13] = b'b';
            assert!(parse(&response, &request, 1).is_err());
            response[13] = b'a';
            response.pop();
            assert!(parse(&response, &request, 1).is_err());
        }
    }
}

/// Loopback fixtures shared with public API integration tests.
#[cfg(test)]
#[path = "../tests/support/mod.rs"]
mod testing;

/// Pure parser tests and owner-local pool invariants.
#[cfg(test)]
mod tests {
    use super::*;
    use testing::{FixtureIo, TestIdentity, TestTransport as _};

    /// Access authenticated metadata only inside tests that mutate pool boundaries.
    fn authenticated(connection: &mut Connection<FixtureIo>) -> &mut Authenticated<FixtureIo> {
        let Authentication::Authenticated(auth) = &mut connection.authentication else {
            panic!("expected authenticated connection");
        };
        auth
    }

    /// Projected trust changes must invalidate the private idle slot before reuse.
    #[test]
    fn weekly_projected_trust_reload_invalidates_idle_transport() {
        use testing::{RotationServer, WeeklyCertificates, rotation_request};
        let certs = WeeklyCertificates::new();
        assert_eq!(certs.crosses.len(), 3);
        assert_eq!(certs.leaf_only_servers.len(), 4);
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
        // Projected overlap accepts the live old server but discards cached TLS.
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
        // Removed trust cannot be bypassed by recycling an idle connection.
        project(2, certs.roots[2].pem());
        assert!(matches!(
            rotation_request(&transport, Some(&identity)),
            Err(testing::Error::Transport(Error::Unauthorized))
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
            Err(testing::Error::Transport(Error::Unauthorized))
        ));
        assert!(transport.idle.borrow().is_none());
        // Recover through both cross certificates without reattaching transport.
        project(4, certs.roots[0].pem());
        assert!(rotation_request(&transport, Some(&identity)).is_ok());
        transport.close_idle();
    }

    /// Successful bootstrap requests always open fresh TLS and leave no idle slot.
    #[test]
    fn bootstrap_never_carries_authenticated_pool_state() {
        use testing::{RotationServer, WeeklyCertificates, rotation_request};
        let certs = WeeklyCertificates::new();
        let d = testing::Directory::new();
        let trust = d.0.join("trust.pem");
        std::fs::write(&trust, certs.roots[0].pem()).unwrap();
        let server = RotationServer::new(certs.servers[0].clone());
        let transport = server.transport(trust);
        let scope = testing::scope();
        let connection = futures::executor::block_on(transport.bootstrap(&scope)).unwrap();
        assert!(matches!(
            connection.authentication,
            Authentication::Bootstrap
        ));
        let first = futures::executor::block_on(connection.request(
            testing::request(Method::Get, "/snapshot", None, 1024),
            &scope,
        ))
        .unwrap();
        assert!(transport.idle.borrow().is_none());
        let second = rotation_request(&transport, None).unwrap();
        assert_ne!(first.body, second);
        assert!(transport.idle.borrow().is_none());
    }

    /// Retirement affects request boundaries, while expiry and failure discard TLS.
    #[test]
    fn pooled_connections_retire_between_requests_and_discard_failures() {
        let d = testing::Directory::new();
        let (ca, key) = testing::ca();
        let identity = TestIdentity::new(&ca, &key);
        let response = |status| {
            (
                "/snapshot".to_owned(),
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
        let transport = Transport::new(endpoint);
        transport.attach_io(Rc::new(FixtureIo));
        let scope = testing::scope();
        let get = || {
            futures::executor::block_on(async {
                transport
                    .authenticated(&identity, &scope)
                    .await
                    .unwrap()
                    .request(
                        testing::request(Method::Get, "/snapshot", None, 65536),
                        &scope,
                    )
                    .await
                    .unwrap()
            })
        };
        assert_eq!(get().status, 200);
        let retire_at = authenticated(transport.idle.borrow_mut().as_mut().unwrap()).retire_at;
        assert!(retire_at > Instant::now() + Duration::from_secs(239));
        assert!(retire_at <= Instant::now() + Duration::from_secs(300));
        assert_eq!(get().status, 204);
        assert_eq!(
            authenticated(transport.idle.borrow_mut().as_mut().unwrap()).retire_at,
            retire_at
        );
        authenticated(transport.idle.borrow_mut().as_mut().unwrap()).retire_at = Instant::now();
        // Checkout retires the first connection. Active I/O ignores its age.
        let mut connection =
            futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
        authenticated(&mut connection).retire_at = Instant::now();
        assert_eq!(connection.check(&scope), Ok(()));
        let expiry = authenticated(&mut connection).expires;
        authenticated(&mut connection).expires = SystemTime::now() - Duration::from_secs(1);
        assert_eq!(
            connection.check(&scope),
            Err(testing::Error::Transport(Error::Unauthorized))
        );
        authenticated(&mut connection).expires = expiry;
        let result = futures::executor::block_on(connection.request(
            testing::request(Method::Get, "/snapshot", None, 65536),
            &scope,
        ))
        .unwrap();
        assert_eq!(result.status, 204);
        assert!(transport.idle.borrow().is_none());
        assert_eq!(get().status, 200);
        authenticated(transport.idle.borrow_mut().as_mut().unwrap()).idle_since =
            Instant::now() - Duration::from_secs(20);
        assert_eq!(get().status, 200);
        authenticated(transport.idle.borrow_mut().as_mut().unwrap()).epoch = [0; 32];
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
            Err(testing::Error::Transport(Error::InvalidRequest))
        ));
        assert!(transport.idle.borrow().is_none());
        assert_eq!(get().status, 200);
        let connection =
            futures::executor::block_on(transport.authenticated(&identity, &scope)).unwrap();
        assert!(matches!(
            futures::executor::block_on(connection.request(
                testing::request(Method::Get, "/snapshot", None, 65536),
                &scope
            )),
            Err(testing::Error::Transport(Error::Io))
        ));
        assert!(transport.idle.borrow().is_none());
        assert_eq!(get().status, 204);
        transport.close_idle();
        server.join().unwrap();
    }

    /// Each group uses exactly one TLS connection, with EOF proving disposal.
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
        let endpoint =
            testing::config(format!("https://{}", listener.local_addr().unwrap()), trust);
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
                        ) => {}
                    result => panic!("connection not discarded: {result:?}"),
                }
            }
        });
        (endpoint, server)
    }

    /// Request syntax and credential allocation limits survive consolidation.
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

    /// Age jitter remains inclusive, bounded, and nonconstant.
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

    /// Reject insecure authorities and ambiguous or oversized response framing.
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
        ] { assert!(parse_head(bytes, 10, 65536).is_err()); }
    }

    /// Connection directives are comma-delimited, case-insensitive, and cumulative.
    #[test]
    fn response_head_collects_connection_close_once() {
        for (headers, close) in [
            ("", false),
            ("Connection: keep-alive\r\n", false),
            ("Connection: xclose, closex\r\n", false),
            ("cOnNeCtIoN: keep-alive,\tClOsE \r\n", true),
            ("Connection: close\r\nConnection: keep-alive\r\n", true),
            ("Connection: keep-alive\r\nConnection: close\r\n", true),
            ("Connection: \u{a0}close\u{a0}\r\n", true),
        ] {
            let bytes = format!("HTTP/1.1 204 No Content\r\n{headers}Retry-After: 2\r\n\r\n");
            assert_eq!(
                parse_head(bytes.as_bytes(), 10, 20).unwrap(),
                Some(ResponseHead {
                    status: 204,
                    header_len: bytes.len(),
                    framing: Framing::Length(0),
                    retry_after: Some(Duration::from_secs(2)),
                    close,
                })
            );
        }
        assert_eq!(parse_head(b"HTTP/1.1 204 No Content\r\n", 10, 20), Ok(None));
    }

    /// Ignored non-UTF-8 headers remain invalid, after framing and size errors.
    #[test]
    fn response_head_preserves_utf8_acceptance_and_error_precedence() {
        for name in ["X-Ignored", "Connection"] {
            let mut bytes = format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 12\r\n{name}: ").into_bytes();
            bytes.extend_from_slice(b"\xff\r\n\r\n");
            assert_eq!(parse_head(&bytes, 10, 20), Err(Error::Overloaded));
            assert_eq!(parse_head(&bytes, 12, 20), Err(Error::InvalidRequest));
        }
        let bytes = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n\xff\xff";
        assert!(parse_head(bytes, 2, 20).is_ok());
        assert_eq!(parse_head(b"HTTP/1.1 429 Busy\r\nContent-Type: application/json\r\nContent-Length: 12\r\n\r\n", 10, 20).unwrap().unwrap().framing, Framing::Length(12));
    }

    /// Extension headers retain printable values without changing framing.
    #[test]
    fn generic_header_accepts_tokens_and_printable_values() {
        let head = request_head(
            "example.test",
            Method::Post,
            "/json",
            Some("secret"),
            4,
            Some(("X-Client-Version", "v1 with spaces")),
        )
        .unwrap();
        assert!(head.starts_with("POST /json HTTP/1.1\r\n"));
        assert!(head.contains("Authorization: Bearer secret\r\n"));
        assert!(head.contains("Content-Length: 4\r\n"));
        assert!(head.ends_with("X-Client-Version: v1 with spaces\r\n\r\n"));
        assert!(valid_header("!#$%&'*+-.^_`|~09AZaz", ""));
        assert!(valid_header("X-Test", " ~"));
    }

    #[test]
    /// Invalid syntax and transport-owned headers cannot be injected.
    fn generic_header_rejects_injection_and_reserved_overrides() {
        for (name, value) in [
            ("", "ok"),
            ("X Bad", "ok"),
            ("X:Bad", "ok"),
            ("X-Test", "bad\r\nInjected: yes"),
            ("X-Test", "\t"),
            ("X-Test", "\x7f"),
            ("X-Test", "non-ASCII: é"),
            ("é", "ok"),
        ] {
            assert_eq!(
                request_head(
                    "example.test",
                    Method::Get,
                    "/",
                    None,
                    0,
                    Some((name, value))
                ),
                Err(Error::InvalidRequest)
            );
        }
        for name in [
            "Host",
            "AUTHORIZATION",
            "Proxy-Authorization",
            "Content-Length",
            "Transfer-Encoding",
            "Content-Type",
            "Content-Encoding",
            "Accept",
            "Accept-Encoding",
            "Connection",
            "TE",
            "Trailer",
            "Upgrade",
            "Expect",
        ] {
            assert_eq!(
                request_head(
                    "example.test",
                    Method::Get,
                    "/",
                    None,
                    0,
                    Some((name, "override"))
                ),
                Err(Error::InvalidRequest)
            );
        }
    }

    #[test]
    /// Header sizes are rejected before credentials enter an allocation.
    fn generic_header_bounds_name_and_value_before_copying() {
        let large = "a".repeat(16384);
        for header in [Some((large.as_str(), "")), Some(("X-Test", large.as_str()))] {
            assert_eq!(
                request_head("example.test", Method::Get, "/", None, 0, header),
                Err(Error::Overloaded)
            );
        }
    }
}
