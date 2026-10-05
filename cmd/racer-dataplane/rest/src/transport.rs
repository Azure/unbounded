//! Nonblocking rustls over reactor-owned readiness. No executor or helper thread.
use crate::{Config, Error, Identity, Io, Method, Operation, Request, Result, Scope};
use std::{
    cell::RefCell,
    io::{Read, Write},
    net::{SocketAddr, TcpStream},
    os::fd::FromRawFd,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime},
};
use uring_runtime::{Scope as _, reactor::Descriptor};

pub struct Transport<I: Io + ?Sized> {
    config: Config,
    io: RefCell<Option<Rc<I>>>,
    idle: Rc<RefCell<Option<Connection<I>>>>,
}
pub struct Connection<I: Io + ?Sized> {
    charge: Option<Rc<I::Lease>>,
    stream: Stream,
    fd: Rc<Descriptor>,
    tls: rustls::ClientConnection,
    io: Rc<I>,
    host: String,
    expires: Option<SystemTime>,
    epoch: [u8; 32],
    idle: std::rc::Weak<RefCell<Option<Connection<I>>>>,
    max_error_body: usize,
    idle_since: Instant,
    // Redistribution is checked only between requests, never during a long poll.
    retire_at: Option<Instant>,
}
const AUTHENTICATED_AGE_MIN: Duration = Duration::from_secs(240);
const AUTHENTICATED_AGE_JITTER_MS: u64 = 60_000;

fn authenticated_age(random: u64) -> Duration {
    AUTHENTICATED_AGE_MIN + Duration::from_millis(random % (AUTHENTICATED_AGE_JITTER_MS + 1))
}
enum Stream {
    Real(TcpStream),
    #[cfg(feature = "simulation")]
    Sim(Rc<Descriptor>),
}
impl Read for Stream {
    fn read(&mut self, bytes: &mut [u8]) -> std::io::Result<usize> {
        match self {
            Self::Real(stream) => stream.read(bytes),
            #[cfg(feature = "simulation")]
            Self::Sim(fd) => fd.try_recv(bytes),
        }
    }
}
impl Write for Stream {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        match self {
            Self::Real(stream) => stream.write(bytes),
            #[cfg(feature = "simulation")]
            Self::Sim(fd) => fd.try_send(bytes),
        }
    }
    fn flush(&mut self) -> std::io::Result<()> {
        match self {
            Self::Real(stream) => stream.flush(),
            #[cfg(feature = "simulation")]
            Self::Sim(_) => Ok(()),
        }
    }
}
pub struct Response {
    pub status: u16,
    pub body: Vec<u8>,
    pub retry_after: Option<Duration>,
}
impl Drop for Response {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.body.zeroize();
    }
}
struct Endpoint {
    host: String,
    authority: String,
    port: u16,
}
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
    pub fn new(config: Config) -> Self {
        Self {
            config,
            io: RefCell::new(None),
            idle: Rc::new(RefCell::new(None)),
        }
    }
    pub fn attach_io(&self, io: Rc<I>) {
        self.idle.borrow_mut().take();
        *self.io.borrow_mut() = Some(io);
    }
    pub fn io(&self) -> Result<Rc<I>, I::Error> {
        self.io
            .borrow()
            .clone()
            .ok_or_else(|| Error::InvalidConfiguration.into())
    }
    pub fn close_idle(&self) {
        self.idle.borrow_mut().take();
    }
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
            if let Some(connection) = self.idle.borrow_mut().take() {
                if identity.is_some()
                    && connection.epoch == epoch
                    && connection.within_max_age()
                    && uring_runtime::environment::now()
                        .saturating_duration_since(connection.idle_since)
                        < Duration::from_secs(20)
                    && connection.check(scope).is_ok()
                {
                    return Ok(connection);
                }
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
            let retire_at = if identity.is_some() {
                let mut random = [0; 8];
                uring_runtime::environment::fill_random(&mut random).map_err(|_| Error::Io)?;
                Some(
                    uring_runtime::environment::now()
                        + authenticated_age(u64::from_ne_bytes(random)),
                )
            } else {
                None
            };
            let mut connection = Connection {
                charge,
                stream,
                fd,
                tls,
                io,
                host: endpoint.authority,
                expires: identity.map(|i| i.expires),
                max_error_body: self.config.max_error_body,
                epoch,
                idle: if identity.is_some() {
                    Rc::downgrade(&self.idle)
                } else {
                    std::rc::Weak::new()
                },
                idle_since: uring_runtime::environment::now(),
                retire_at,
            };
            while connection.tls.is_handshaking() {
                connection.step(scope).await?;
            }
            Ok(connection)
        })
    }
}
type Connected<I> = (Stream, Rc<Descriptor>, Option<Rc<<I as Io>::Lease>>);

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
    fn within_max_age(&self) -> bool {
        self.retire_at
            .is_none_or(|at| uring_runtime::environment::now() < at)
    }
    fn check(&self, scope: &I::Scope) -> Result<(), I::Error> {
        scope.check()?;
        if self
            .expires
            .is_some_and(|e| uring_runtime::environment::wall_now() >= e)
        {
            return Err(Error::Unauthorized.into());
        }
        Ok(())
    }
    async fn step(&mut self, scope: &I::Scope) -> Result<(), I::Error> {
        self.check(scope)?;
        // Even a continuously readable peer must yield to other owner work.
        uring_runtime::yield_now().await;
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
            if let Some(expiry) = self.expires {
                let remaining = expiry
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
            let (status, header_len, framing, retry_after) = loop {
                if let Some(head) = parse_head(&received, limit, self.max_error_body)? {
                    break head;
                }
                if received.len() >= 16384 {
                    return Err(Error::Overloaded.into());
                }
                self.receive(&mut received, &mut scratch[..], scope).await?;
            };
            let close = std::str::from_utf8(&received[..header_len])
                .map_err(|_| Error::InvalidRequest)?
                .lines()
                .any(|line| {
                    line.split_once(':').is_some_and(|(name, value)| {
                        name.eq_ignore_ascii_case("connection")
                            && value
                                .split(',')
                                .any(|token| token.trim().eq_ignore_ascii_case("close"))
                    })
                });
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
            if reusable && !close && matches!(status, 200 | 204) && self.within_max_age() {
                if let Some(idle) = self.idle.upgrade() {
                    self.idle_since = uring_runtime::environment::now();
                    *idle.borrow_mut() = Some(self);
                }
            }
            Ok(Response {
                status,
                body: body.to_vec(),
                retry_after,
            })
        })
    }
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

// Grow without freeing an allocation containing plaintext key material.
fn append_sensitive(into: &mut zeroize::Zeroizing<Vec<u8>>, bytes: &[u8]) {
    if into.capacity() - into.len() < bytes.len() {
        let capacity = (into.len() + bytes.len()).max(into.capacity().saturating_mul(2));
        let mut next = zeroize::Zeroizing::new(Vec::with_capacity(capacity));
        next.extend_from_slice(into);
        std::mem::swap(into, &mut next);
    }
    into.extend_from_slice(bytes);
}
// Go's TLS server sends internal_error when bounded ClientHello admission is
// saturated. Retry that transport failure without accepting an unauthenticated
// connection; certificate and protocol failures remain terminal.
fn tls_failure(error: rustls::Error) -> Error {
    match error {
        rustls::Error::AlertReceived(rustls::AlertDescription::InternalError) => Error::Unavailable,
        _ => Error::Unauthorized,
    }
}

enum Framing {
    Length(usize),
    Chunked,
}
fn parse_head(
    bytes: &[u8],
    limit: usize,
    max_error_body: usize,
) -> Result<Option<(u16, usize, Framing, Option<Duration>)>> {
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
    Ok(Some((
        status,
        length,
        if chunked {
            Framing::Chunked
        } else {
            Framing::Length(size)
        },
        retry,
    )))
}
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
    if time.len() != 3 || year < 1970 || year > 9999 {
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

#[cfg(test)]
mod scenarios;

#[cfg(test)]
mod tests {
    use super::*;

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
