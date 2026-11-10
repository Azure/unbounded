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
//! It opens a fresh connection with `Connection: close` through a single-use
//! http1 pool (the same TLS path as pooled origins) and accepts fixed-length,
//! chunked, or close-delimited response bodies up to a cap.

use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use http1::tls::TlsClient;
use std::io::Write as _;
use std::net::IpAddr;
use std::net::SocketAddr;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::time::Duration;
use std::time::Instant;
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
        crate::control::rest::Io::resolve(&self.0, host, port, scope)
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

/// The http1 caller context for one small exchange: control-class
/// connection admission and request-context buffer charges.
struct SmallContext(Rc<Reactor>);

impl http1::connection::Context for SmallContext {
    type Error = Error;

    type Scope = RequestScope;

    type Reactor = Reactor;

    type Charge = flow_control::Charge<crate::admission::AdmissionPolicy>;

    type Slot = crate::admission::ConnectionReservation;

    type Opaque = ();

    type State = ();

    type Endpoint = crate::http::Endpoint;

    fn charge(&self, bytes: usize) -> Result<Self::Charge> {
        self.0
            .admission()
            .reserve(None, ResourceClass::RequestContext, bytes)
            .map_err(Into::into)
    }

    fn outbound_slot(&self) -> Result<Self::Slot> {
        self.0.reserve_connection(ResourceClass::ControlConnection)
    }

    fn stopped(&self) -> bool {
        self.0.admission().is_stopped()
    }
}

/// Send one request on a fresh connection and read the complete response.
///
/// The connection comes from a single-use http1 pool, so HTTPS uses the
/// shared TLS path (kernel TLS when available). Bodies above `limit` are
/// rejected.
pub async fn exchange_small(
    reactor: &Rc<Reactor>,
    resolver: &dyn Resolver,
    endpoint: &RemoteEndpoint,
    request: &SmallRequest<'_>,
    limit: usize,
    scope: &RequestScope,
) -> Result<SmallResponse> {
    if request.method.is_empty()
        || !request.method.bytes().all(http1::is_token)
        || request.target.is_empty()
        || request.target.bytes().any(|b| b <= 0x20 || b == 0x7f)
    {
        return Err(Error::InvalidRequest);
    }
    endpoint.resolve(resolver, scope).await?;
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
    let context = Rc::new(SmallContext(reactor.clone()));
    let pool = http1::connection::HttpPool::new(
        reactor.clone(),
        context.clone(),
        http1::connection::PoolConfig {
            per_endpoint: 1,
            secondary_cap: 1,
            max_endpoints: 1,
            waiter_cap: 1,
            idle_timeout: Duration::from_secs(1),
            tcp_nodelay: true,
        },
    );
    let io = http1::connection::HttpIo::new(
        reactor.clone(),
        http1::Codec::new(MAX_SMALL_HEAD),
        context,
        u64::MAX,
        u64::MAX,
    );
    let key = crate::http::Endpoint::Remote(endpoint.clone());
    let lease = pool.checkout(&key, scope).await?;
    let mut parse = |bytes: &[u8]| small_plan(bytes, limit);
    let exchanged = io
        .exchange_small(lease, &wire, MAX_SMALL_HEAD, &mut parse, scope)
        .await
        .map_err(|error| match error {
            // Framing violations, including pipelined bytes, are the peer's.
            Error::InvalidRequest | Error::HeaderTooLarge => Error::BadGateway,
            error => error,
        })?;
    let (status, headers) = exchanged.value;
    Ok(SmallResponse {
        status,
        headers,
        body: exchanged.body,
    })
}

/// Status and header fields of a small response.
type SmallHead = (u16, Vec<(String, String)>);

/// Plan a small response once its head is complete. Supports
/// `Content-Length`, `Transfer-Encoding: chunked`, and close-delimited bodies
/// through the shared head codec and small-body decoder.
fn small_plan(
    bytes: &[u8],
    limit: usize,
) -> Result<Option<http1::small_exchange::Plan<Error, SmallHead>>> {
    use racer_wire::http::{Codec, StartLine, small};
    let codec = Codec::<()>::new(MAX_SMALL_HEAD)
        .trim_ows()
        .transfer_coding();
    let (head, end) = match codec.decode_head(bytes) {
        Ok(Some(head)) => head,
        Ok(None) => return Ok(None),
        Err(_) => return Err(Error::BadGateway),
    };
    let StartLine::Response { status } = head.start else {
        return Err(Error::BadGateway);
    };
    if (100..200).contains(&status) {
        return Err(Error::BadGateway);
    }
    let framing = small::Framing::from_response(&head).map_err(|_| Error::BadGateway)?;
    // 204 and 304 never carry a body unless one is explicitly framed.
    let framing = match framing {
        small::Framing::Close if status == 204 || status == 304 => small::Framing::Length(0),
        framing => framing,
    };
    let close = head.closes_connection().map_err(|_| Error::BadGateway)?;
    let headers = head
        .headers
        .iter()
        .map(|h| {
            Ok((
                h.name.clone(),
                std::str::from_utf8(&h.value)
                    .map_err(|_| Error::BadGateway)?
                    .trim()
                    .to_owned(),
            ))
        })
        .collect::<Result<_>>()?;
    Ok(Some(http1::small_exchange::Plan {
        head_length: end,
        framing,
        limits: small::Limits {
            body: limit,
            ..small::Limits::default()
        },
        close,
        body_error: |_| Error::BadGateway,
        value: (status, headers),
    }))
}

/// Parse a complete buffered response, `None` while more bytes are needed.
#[cfg(test)]
pub(crate) fn parse_small_response(
    bytes: &[u8],
    eof: bool,
    limit: usize,
) -> Result<Option<SmallResponse>> {
    use racer_wire::http::small;
    let plan = match small_plan(bytes, limit)? {
        Some(plan) => plan,
        None if !eof => return Ok(None),
        None => return Err(Error::BadGateway),
    };
    let mut decoder = small::Decoder::new(plan.framing, plan.limits).map_err(plan.body_error)?;
    match decoder
        .feed(&bytes[plan.head_length..])
        .map_err(plan.body_error)?
    {
        small::Status::Done(_) => {}
        small::Status::Need if eof => decoder.finish().map_err(plan.body_error)?,
        small::Status::Need => return Ok(None),
    }
    let (status, headers) = plan.value;
    Ok(Some(SmallResponse {
        status,
        headers,
        body: Zeroizing::new(decoder.into_body()),
    }))
}

#[cfg(test)]
pub(crate) mod tests;
