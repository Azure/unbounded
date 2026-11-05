//! Outbound origin endpoints addressed by DNS name or IP literal, over plain
//! HTTP or HTTPS, plus a bounded one-shot exchange for small control requests.
//!
//! [`RemoteEndpoint`] is a pool key ([`crate::http::Endpoint::Remote`]). The pool
//! never resolves names itself: callers run [`RemoteEndpoint::resolve`] before a
//! checkout, which is a no-op for IP literals and for addresses resolved within
//! [`DNS_TTL`]. HTTPS endpoints complete TLS inside the pool's connect path
//! (kernel TLS when available, see [`http1::tls`]), so leases behave like plain
//! sockets and bodies can be received directly into caller buffers.
//!
//! [`exchange_small`] is for infrequent, small requests such as token endpoints.
//! It opens a fresh connection with `Connection: close`, uses userspace TLS, and
//! accepts fixed-length, chunked, or close-delimited response bodies up to a cap.

use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use http1::tls::TlsClient;
use http1::tls::rustls;
use std::io::Read as _;
use std::io::Write as _;
use std::net::IpAddr;
use std::net::SocketAddr;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::reactor::SocketAddress;
use uring_runtime::reactor::descriptor::Descriptor;
use zeroize::Zeroizing;

/// How long resolved addresses are reused before [`RemoteEndpoint::resolve`]
/// queries again. Stale addresses are kept if a refresh fails.
pub const DNS_TTL: Duration = Duration::from_secs(30);

/// Largest response head accepted by [`exchange_small`].
pub const MAX_SMALL_HEAD: usize = 16 * 1024;

/// Default body cap for [`exchange_small`].
pub const MAX_SMALL_BODY: usize = 64 * 1024;

/// Owner-local name resolution.
pub trait Resolver {
    /// Resolve `host` to socket addresses with `port`.
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>>;
}

/// Bounded UDP DNS through `/etc/resolv.conf` on the worker's reactor.
pub struct DnsResolver(crate::control::ReactorControlIo);

impl DnsResolver {
    /// Resolve on `reactor`.
    pub fn new(reactor: Rc<Reactor>) -> Self {
        Self(crate::control::ReactorControlIo::new(reactor))
    }
}

impl Resolver for DnsResolver {
    fn resolve<'a>(
        &'a self,
        host: &'a str,
        port: u16,
        scope: &'a RequestScope,
    ) -> Operation<'a, Vec<SocketAddr>> {
        wire_codec::rest::Io::resolve(&self.0, host, port, scope)
    }
}

/// Host, port, and optional TLS policy; equality and order ignore resolved state.
///
/// A clone made by [`Self::address_sequence`] shares the resolved addresses but
/// walks them with its own cursor; it still compares equal to the original.
#[derive(Clone)]
pub struct RemoteEndpoint(Arc<Inner>, Option<Arc<Mutex<Option<Sequence>>>>);

/// Per-exchange walk over the resolved addresses: the shared rotation picks
/// `start` on the first connect, later connects take `start + 1`, `start + 2`, ...
#[derive(Clone, Copy)]
struct Sequence {
    start: usize,

    taken: usize,
}

struct Inner {
    host: String,

    port: u16,

    literal: Option<IpAddr>,

    tls: Option<TlsClient>,

    cache: Mutex<Addresses>,
}

#[derive(Default)]
struct Addresses {
    addresses: Vec<SocketAddr>,

    next: usize,

    refreshed: Option<Instant>,
}

impl RemoteEndpoint {
    /// Plain HTTP to `host` (DNS name, IPv4, or IPv6 with or without brackets).
    pub fn http(host: &str, port: u16) -> Result<Self> {
        Self::new(host, port, None)
    }

    /// HTTPS to `host`, verifying it as SNI and certificate identity.
    pub fn https(host: &str, port: u16, config: Arc<rustls::ClientConfig>) -> Result<Self> {
        let host = normalize_host(host)?;
        let tls = TlsClient::new(config, host).map_err(|_| Error::InvalidConfiguration)?;
        Self::new(host, port, Some(tls))
    }

    /// Use an explicit TLS policy (for example a forced [`http1::tls::TlsMode`]).
    pub fn with_tls(host: &str, port: u16, tls: TlsClient) -> Result<Self> {
        Self::new(host, port, Some(tls))
    }

    fn new(host: &str, port: u16, tls: Option<TlsClient>) -> Result<Self> {
        let host = normalize_host(host)?;
        if port == 0 {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self(
            Arc::new(Inner {
                literal: host.parse().ok(),
                host: host.to_ascii_lowercase(),
                port,
                tls,
                cache: Mutex::default(),
            }),
            None,
        ))
    }

    /// Host without IPv6 brackets.
    pub fn host(&self) -> &str {
        &self.0.host
    }

    /// TCP port.
    pub fn port(&self) -> u16 {
        self.0.port
    }

    /// Whether connections use TLS.
    pub fn is_https(&self) -> bool {
        self.0.tls.is_some()
    }

    /// TLS policy, if HTTPS.
    pub fn tls(&self) -> Option<&TlsClient> {
        self.0.tls.as_ref()
    }

    /// `Host` header value: brackets IPv6 and omits the scheme's default port.
    pub fn authority(&self) -> String {
        let host = match self.0.literal {
            Some(IpAddr::V6(_)) => format!("[{}]", self.0.host),
            _ => self.0.host.clone(),
        };
        let default = if self.is_https() { 443 } else { 80 };
        if self.0.port == default {
            host
        } else {
            format!("{host}:{}", self.0.port)
        }
    }

    /// Whether [`Self::resolve`] would query the resolver at `now`.
    pub fn needs_resolve(&self, now: std::time::Instant) -> bool {
        if self.0.literal.is_some() {
            return false;
        }
        let cache = self.0.cache.lock().unwrap_or_else(|e| e.into_inner());
        cache.addresses.is_empty() || cache.refreshed.is_none_or(|at| now >= at + DNS_TTL)
    }

    /// Resolve unless the endpoint is an IP literal or its addresses are fresh.
    /// A failed refresh keeps previously resolved addresses usable.
    pub async fn resolve(&self, resolver: &dyn Resolver, scope: &RequestScope) -> Result<()> {
        scope.check()?;
        if !self.needs_resolve(scope.clock().now()) {
            return Ok(());
        }
        match resolver.resolve(&self.0.host, self.0.port, scope).await {
            Ok(addresses) if !addresses.is_empty() => {
                let mut cache = self.0.cache.lock().unwrap_or_else(|e| e.into_inner());
                // Keep the rotation cursor: resetting it would send the
                // attempt after a connect-failure re-resolve back to the
                // first (possibly dead) address every time.
                cache.addresses = addresses;
                cache.refreshed = Some(scope.clock().now());
                Ok(())
            }
            result => {
                let cache = self.0.cache.lock().unwrap_or_else(|e| e.into_inner());
                if cache.addresses.is_empty() {
                    result.and(Err(Error::Unavailable))
                } else {
                    scope.check()
                }
            }
        }
    }

    /// Force the next [`Self::resolve`] to query (for example after connect failures).
    pub fn expire_addresses(&self) {
        let mut cache = self.0.cache.lock().unwrap_or_else(|e| e.into_inner());
        cache.refreshed = None;
    }

    /// Next address, rotating through resolved addresses. Fails until resolved.
    pub fn address(&self) -> Result<SocketAddr> {
        if let Some(ip) = self.0.literal {
            return Ok(SocketAddr::new(ip, self.0.port));
        }
        let mut cache = self.0.cache.lock().unwrap_or_else(|e| e.into_inner());
        if cache.addresses.is_empty() {
            return Err(Error::Unavailable);
        }
        let index = match &self.1 {
            None => Self::rotate(&mut cache),
            Some(sequence) => {
                let mut sequence = sequence.lock().unwrap_or_else(|e| e.into_inner());
                let current = sequence.get_or_insert_with(|| Sequence {
                    start: Self::rotate(&mut cache),
                    taken: 0,
                });
                let index = current.start.wrapping_add(current.taken);
                current.taken = current.taken.wrapping_add(1);
                index
            }
        };
        Ok(cache.addresses[index % cache.addresses.len()])
    }

    fn rotate(cache: &mut Addresses) -> usize {
        let index = cache.next % cache.addresses.len();
        cache.next = cache.next.wrapping_add(1);
        index
    }

    /// Clone for one exchange's connect attempts. Its first connect takes the
    /// shared rotation's next address; each retry takes the following resolved
    /// address, so [`Self::address_count`] attempts try every address once even
    /// while other exchanges rotate the shared cursor concurrently. Reused idle
    /// connections take no address and leave the shared rotation untouched.
    pub fn address_sequence(&self) -> Self {
        Self(self.0.clone(), Some(Arc::default()))
    }

    /// Addresses this [`Self::address_sequence`] clone has handed to connects.
    /// Always 0 for IP literals and for endpoints that are not sequences.
    pub fn addresses_taken(&self) -> usize {
        self.1.as_ref().map_or(0, |sequence| {
            sequence
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .map_or(0, |current| current.taken)
        })
    }

    /// Number of distinct addresses a connect may try: 1 for IP literals,
    /// otherwise the resolved count (0 until resolved).
    pub fn address_count(&self) -> usize {
        if self.0.literal.is_some() {
            return 1;
        }
        let cache = self.0.cache.lock().unwrap_or_else(|e| e.into_inner());
        cache.addresses.len()
    }

    fn key(&self) -> (bool, &str, u16) {
        (self.is_https(), &self.0.host, self.0.port)
    }
}

/// Strip IPv6 brackets and reject empty or obviously invalid host text.
fn normalize_host(host: &str) -> Result<&str> {
    let host = host
        .strip_prefix('[')
        .and_then(|h| h.strip_suffix(']'))
        .unwrap_or(host);
    if host.is_empty()
        || host.len() > 253
        || !host
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b':' | b'_'))
    {
        return Err(Error::InvalidConfiguration);
    }
    Ok(host)
}

impl PartialEq for RemoteEndpoint {
    fn eq(&self, other: &Self) -> bool {
        self.key() == other.key()
    }
}
impl Eq for RemoteEndpoint {}
impl PartialOrd for RemoteEndpoint {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}
impl Ord for RemoteEndpoint {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        self.key().cmp(&other.key())
    }
}
impl std::hash::Hash for RemoteEndpoint {
    fn hash<H: std::hash::Hasher>(&self, state: &mut H) {
        self.key().hash(state);
    }
}
impl std::fmt::Debug for RemoteEndpoint {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "{}://{}",
            if self.is_https() { "https" } else { "http" },
            self.authority()
        )
    }
}

/// One small request. `Host`, `Connection`, and `Content-Length` are added.
pub struct SmallRequest<'a> {
    /// Method token.
    pub method: &'a str,

    /// Origin-form target (path and query).
    pub target: &'a str,

    /// Extra header fields.
    pub headers: &'a [(&'a str, &'a str)],

    /// Request body; may hold credentials and is copied only into zeroized storage.
    pub body: &'a [u8],
}

/// A complete small response. The body is zeroized on drop.
pub struct SmallResponse {
    /// Status code.
    pub status: u16,

    headers: Vec<(String, String)>,

    /// Decoded body.
    pub body: Zeroizing<Vec<u8>>,
}

impl SmallResponse {
    /// First header value with `name` (case-insensitive).
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers
            .iter()
            .find(|(n, _)| n.eq_ignore_ascii_case(name))
            .map(|(_, v)| v.as_str())
    }
}

/// Send one request on a fresh connection and read the complete response.
/// HTTPS uses userspace rustls; bodies above `limit` are rejected.
pub async fn exchange_small(
    reactor: &Rc<Reactor>,
    resolver: &dyn Resolver,
    endpoint: &RemoteEndpoint,
    request: &SmallRequest<'_>,
    limit: usize,
    scope: &RequestScope,
) -> Result<SmallResponse> {
    endpoint.resolve(resolver, scope).await?;
    let address = endpoint.address()?;
    let lease = Rc::new(reactor.reserve_connection(ResourceClass::ControlConnection)?);
    let fd = Rc::new(uring_runtime::reactor::descriptor::socket(
        reactor.sim_os().as_ref(),
        if address.is_ipv4() {
            libc::AF_INET
        } else {
            libc::AF_INET6
        },
    )?);
    reactor
        .connect_with_lease(
            fd.clone(),
            SocketAddress::Inet(address),
            lease.clone(),
            scope,
        )
        .await?;
    let mut wire = Zeroizing::new(Vec::with_capacity(256 + request.body.len()));
    write!(
        wire,
        "{} {} HTTP/1.1\r\nHost: {}\r\nConnection: close\r\nContent-Length: {}\r\n",
        request.method,
        request.target,
        endpoint.authority(),
        request.body.len()
    )
    .map_err(|_| Error::Internal)?;
    for (name, value) in request.headers {
        if !name.bytes().all(http1::is_token)
            || value.bytes().any(|b| b == b'\r' || b == b'\n' || b == 0)
        {
            return Err(Error::InvalidRequest);
        }
        wire.extend_from_slice(name.as_bytes());
        wire.extend_from_slice(b": ");
        wire.extend_from_slice(value.as_bytes());
        wire.extend_from_slice(b"\r\n");
    }
    wire.extend_from_slice(b"\r\n");
    wire.extend_from_slice(request.body);
    let mut session = match endpoint.tls() {
        Some(tls) => Some(
            rustls::ClientConnection::new(tls.config().clone(), tls.server_name().clone())
                .map_err(|_| Error::InvalidConfiguration)?,
        ),
        None => None,
    };
    let wait = |interest: libc::c_short| {
        reactor.readiness_with_lease(fd.clone(), interest as u32, lease.clone(), scope)
    };
    // Send everything.
    match &mut session {
        Some(session) => {
            session
                .writer()
                .write_all(&wire)
                .map_err(|_| Error::Internal)?;
            while session.wants_write() || session.is_handshaking() {
                if session.wants_write() {
                    match session.write_tls(&mut Plain(&fd)) {
                        Ok(_) => continue,
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                            wait(libc::POLLOUT).await?;
                            continue;
                        }
                        Err(_) => return Err(Error::Io),
                    }
                }
                match session.read_tls(&mut Plain(&fd)) {
                    Ok(0) => return Err(Error::Io),
                    Ok(_) => {
                        session
                            .process_new_packets()
                            .map_err(|_| Error::Os(libc::EPROTO))?;
                    }
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        wait(libc::POLLIN).await?;
                    }
                    Err(_) => return Err(Error::Io),
                }
            }
        }
        None => {
            let mut sent = 0;
            while sent < wire.len() {
                match fd.try_send(&wire[sent..]) {
                    Ok(n) => sent += n,
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        wait(libc::POLLOUT).await?;
                    }
                    Err(_) => return Err(Error::Io),
                }
            }
        }
    }
    drop(wire);
    // Receive until the response is complete or the peer closes.
    let cap = MAX_SMALL_HEAD + limit.saturating_mul(2).saturating_add(4096);
    let mut received = Zeroizing::new(Vec::new());
    let mut scratch = Zeroizing::new(vec![0u8; 16 * 1024]);
    loop {
        scope.check()?;
        let (read, eof) = match &mut session {
            Some(session) => match session.reader().read(&mut scratch) {
                Ok(0) => (0, true),
                Ok(n) => (n, false),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    match session.read_tls(&mut Plain(&fd)) {
                        // Truncation without close_notify still ends the body;
                        // framing checks below reject incomplete messages.
                        Ok(0) => (0, true),
                        Ok(_) => {
                            session
                                .process_new_packets()
                                .map_err(|_| Error::Os(libc::EPROTO))?;
                            continue;
                        }
                        Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                            wait(libc::POLLIN).await?;
                            continue;
                        }
                        Err(_) => return Err(Error::Io),
                    }
                }
                Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => (0, true),
                Err(_) => return Err(Error::Io),
            },
            None => match fd.try_recv(&mut scratch) {
                Ok(0) => (0, true),
                Ok(n) => (n, false),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    wait(libc::POLLIN).await?;
                    continue;
                }
                Err(_) => return Err(Error::Io),
            },
        };
        received.extend_from_slice(&scratch[..read]);
        if received.len() > cap {
            return Err(Error::BadGateway);
        }
        if let Some(response) = parse_small_response(&received, eof, limit)? {
            return Ok(response);
        }
        if eof {
            return Err(Error::BadGateway);
        }
    }
}

/// Nonblocking `Read`/`Write` over a borrowed descriptor.
struct Plain<'a>(&'a Descriptor);

impl std::io::Read for Plain<'_> {
    fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
        self.0.try_recv(buf)
    }
}

impl std::io::Write for Plain<'_> {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        self.0.try_send(buf)
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Parse a complete response, `None` while more bytes are needed. Supports
/// `Content-Length`, `Transfer-Encoding: chunked`, and close-delimited bodies.
pub(crate) fn parse_small_response(
    bytes: &[u8],
    eof: bool,
    limit: usize,
) -> Result<Option<SmallResponse>> {
    let mut fields = [httparse::EMPTY_HEADER; 64];
    let mut response = httparse::Response::new(&mut fields);
    let end = match response.parse(bytes) {
        Ok(httparse::Status::Complete(end)) => end,
        Ok(httparse::Status::Partial) if !eof && bytes.len() <= MAX_SMALL_HEAD => {
            return Ok(None);
        }
        _ => return Err(Error::BadGateway),
    };
    if end > MAX_SMALL_HEAD {
        return Err(Error::BadGateway);
    }
    let status = response.code.ok_or(Error::BadGateway)?;
    if (100..200).contains(&status) {
        return Err(Error::BadGateway);
    }
    let headers: Vec<(String, String)> = response
        .headers
        .iter()
        .map(|h| {
            Ok((
                h.name.to_owned(),
                std::str::from_utf8(h.value)
                    .map_err(|_| Error::BadGateway)?
                    .trim()
                    .to_owned(),
            ))
        })
        .collect::<Result<_>>()?;
    let field = |name: &str| {
        headers
            .iter()
            .find(|(n, _)| n.eq_ignore_ascii_case(name))
            .map(|(_, v)| v.as_str())
    };
    let rest = &bytes[end..];
    let body = if let Some(coding) = field("transfer-encoding") {
        if !coding.eq_ignore_ascii_case("chunked") {
            return Err(Error::BadGateway);
        }
        match decode_chunked(rest, limit)? {
            Some(body) => body,
            None if eof => return Err(Error::BadGateway),
            None => return Ok(None),
        }
    } else if let Some(length) = field("content-length") {
        let length: usize = length.parse().map_err(|_| Error::BadGateway)?;
        if length > limit {
            return Err(Error::BadGateway);
        }
        if rest.len() < length {
            return if eof {
                Err(Error::BadGateway)
            } else {
                Ok(None)
            };
        }
        Zeroizing::new(rest[..length].to_vec())
    } else if status == 204 || status == 304 {
        Zeroizing::new(Vec::new())
    } else {
        if rest.len() > limit {
            return Err(Error::BadGateway);
        }
        if !eof {
            return Ok(None);
        }
        Zeroizing::new(rest.to_vec())
    };
    Ok(Some(SmallResponse {
        status,
        headers,
        body,
    }))
}

/// Decode a complete chunked body (trailers ignored), `None` if incomplete.
fn decode_chunked(mut bytes: &[u8], limit: usize) -> Result<Option<Zeroizing<Vec<u8>>>> {
    let mut body = Zeroizing::new(Vec::new());
    loop {
        let Some(line_end) = bytes.windows(2).position(|w| w == b"\r\n") else {
            return if bytes.len() > 1024 {
                Err(Error::BadGateway)
            } else {
                Ok(None)
            };
        };
        let line = std::str::from_utf8(&bytes[..line_end]).map_err(|_| Error::BadGateway)?;
        let size = line.split(';').next().unwrap_or("").trim();
        if size.is_empty() || size.len() > 8 {
            return Err(Error::BadGateway);
        }
        let size = usize::from_str_radix(size, 16).map_err(|_| Error::BadGateway)?;
        bytes = &bytes[line_end + 2..];
        if size == 0 {
            // Trailer fields end with an empty line.
            loop {
                let Some(end) = bytes.windows(2).position(|w| w == b"\r\n") else {
                    return Ok(None);
                };
                bytes = &bytes[end + 2..];
                if end == 0 {
                    return Ok(Some(body));
                }
            }
        }
        if body.len() + size > limit {
            return Err(Error::BadGateway);
        }
        if bytes.len() < size + 2 {
            return Ok(None);
        }
        if &bytes[size..size + 2] != b"\r\n" {
            return Err(Error::BadGateway);
        }
        body.extend_from_slice(&bytes[..size]);
        bytes = &bytes[size + 2..];
    }
}

#[cfg(test)]
pub(crate) mod tests;
