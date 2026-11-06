//! Bounded TLS 1.3 JSON HTTP over the shared http1 connection pool.
//!
//! Connections and their pool stay on their I/O owner's thread. TLS
//! authenticates peers; endpoint health and identity admission belong to callers.

use std::{
    cell::RefCell,
    net::SocketAddr,
    path::{Path, PathBuf},
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime},
};

/// Owner-local future carrying the caller's transport error type.
pub use racer_io::Operation;

use http1::connection::{self as pool, HttpIo, HttpPool};
use http1::tls::TlsClient;
use racer_io::{Scope as _, ring::Descriptor};
use racer_wire::http::{self, small};
use zeroize::Zeroize as _;

/// Largest request or response head, including the final empty line.
const MAX_HEAD: usize = 16384;

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
pub trait Scope: racer_io::Scope {
    /// Return the caller's absolute monotonic deadline, or `None` when the
    /// scope is untimed and ends only by cancellation.
    fn deadline(&self) -> Option<Instant>;

    /// Retain caller policy while limiting the deadline to `until`.
    fn narrowed(&self, until: Instant) -> Self;
}

/// Readiness retains the descriptor and optional lease until deregistration,
/// including cancellation cleanup. Futures run on the caller's sole I/O owner.
pub trait Io: 'static {
    /// Caller errors preserve cancellation and transport failure classification.
    type Error: Copy
        + Send
        + PartialEq
        + From<Error>
        + From<racer_io::Error>
        + From<http1::Error>
        + 'static;

    /// Caller policy governing each I/O operation.
    type Scope: Scope<Error = Self::Error>;

    /// Admission charge retained through readiness cleanup.
    type Lease: 'static;

    /// Caller-owned sensitive bytes, retaining any read admission charge.
    type FileBytes: AsRef<[u8]>;

    /// Reactor that runs pooled connection I/O on this owner.
    type Reactor: std::ops::Deref<Target = racer_io::scoped::Reactor<Self::Scope>> + 'static;

    /// Admission charge for buffers owned by pooled connection I/O.
    type Charge: 'static;

    /// The owner's reactor.
    fn reactor(&self) -> Rc<Self::Reactor>;

    /// Admit `bytes` of connection buffer storage.
    fn charge(&self, bytes: usize) -> Result<Self::Charge, Self::Error>;

    /// Worker clock and entropy for every transport decision on this owner.
    fn ctx(&self) -> &racer_io::WorkerCtx;

    /// The simulated OS that owns this endpoint's sockets, or `None` for
    /// kernel sockets.
    fn sim(&self) -> Option<racer_io::sim::SimOs> {
        None
    }

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

pub use racer_wire::control::{DiscoveredEndpoint, DiscoveredEndpoints, MAX_DISCOVERED_ENDPOINTS};

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

/// Owner-local HTTPS transport over the shared http1 connection pool.
///
/// Authenticated connections idle in the pool between requests; this
/// transport tracks their identity epoch and lifetime and decides reuse.
pub struct Transport<I: Io + ?Sized> {
    /// Endpoint and byte limits retained across owner attachment.
    config: Config,

    /// Attached readiness owner and its pool; never shared across threads.
    attached: RefCell<Option<Attached<I>>>,

    /// Authenticated connections currently idle in the attached pool.
    idle: Rc<RefCell<Vec<Pooled>>>,

    /// Process-unique pool generation; replacing it fences every connection
    /// checked out or idle under the previous value.
    generation: Rc<std::cell::Cell<u64>>,

    /// Discovered endpoints replacing the static URL and trust file when set.
    discovered: RefCell<Option<DiscoveredEndpoints>>,

    /// Discovered seat ranking hints shared with checked-out connections.
    health: Rc<std::cell::Cell<Health>>,
}

/// One owner attachment: the owner, its pool, and its exchange I/O.
struct Attached<I: Io + ?Sized> {
    io: Rc<I>,

    pool: Rc<HttpPool<PoolContext<I>>>,

    http: Rc<HttpIo<PoolContext<I>>>,
}

impl<I: Io + ?Sized> Clone for Attached<I> {
    fn clone(&self) -> Self {
        Self {
            io: self.io.clone(),
            pool: self.pool.clone(),
            http: self.http.clone(),
        }
    }
}

/// Source of process-unique pool generations.
static NEXT_GENERATION: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(1);

/// Allocate a generation never used by any transport in this process.
fn next_generation() -> u64 {
    NEXT_GENERATION.fetch_add(1, std::sync::atomic::Ordering::Relaxed)
}

/// Active connections allowed per address and trust epoch.
const POOL_PER_ENDPOINT: usize = 4;

/// Idle connections are reused only within this window.
const IDLE_REUSE: Duration = Duration::from_secs(20);

/// The http1 caller context for one transport attachment.
pub struct PoolContext<I: Io + ?Sized>(Rc<I>);

impl<I: Io + ?Sized> pool::Context for PoolContext<I> {
    type Error = I::Error;

    type Scope = I::Scope;

    type Reactor = I::Reactor;

    type Charge = I::Charge;

    type Slot = Option<Rc<I::Lease>>;

    type Opaque = ();

    type State = ();

    type Endpoint = PoolKey;

    fn charge(&self, bytes: usize) -> Result<I::Charge, I::Error> {
        self.0.charge(bytes)
    }

    fn outbound_slot(&self) -> Result<Option<Rc<I::Lease>>, I::Error> {
        self.0.lease()
    }

    fn stopped(&self) -> bool {
        false
    }
}

/// Pool identity: address, TLS policy, trust epoch, and transport generation.
///
/// Connections are shared only between checkouts with the same address,
/// epoch, and generation; the TLS policy is derived from the epoch.
#[derive(Clone)]
pub struct PoolKey {
    address: SocketAddr,

    tls: TlsClient,

    epoch: [u8; 32],

    generation: u64,
}

impl PoolKey {
    fn order(&self) -> (u64, [u8; 32], SocketAddr) {
        (self.generation, self.epoch, self.address)
    }
}

impl PartialEq for PoolKey {
    fn eq(&self, other: &Self) -> bool {
        self.order() == other.order()
    }
}

impl Eq for PoolKey {}

impl PartialOrd for PoolKey {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for PoolKey {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        self.order().cmp(&other.order())
    }
}

impl<E> pool::Endpoint<E> for PoolKey {
    fn address(&self) -> Result<racer_io::ring::SocketAddress, E> {
        Ok(racer_io::ring::SocketAddress::Inet(self.address))
    }

    fn tls(&self) -> Option<&TlsClient> {
        Some(&self.tls)
    }
}

/// An authenticated connection idle in the pool, with its reuse policy.
struct Pooled {
    /// Socket identity of the idle lease; dead once the pool closes it.
    socket: std::rc::Weak<Descriptor>,

    /// Pool key that returns this connection.
    key: PoolKey,

    /// Identity lifetime and redistribution age.
    auth: Authenticated,

    /// Host header authority for requests on this connection.
    host: String,
}

/// Ranking hints for discovered seats. Both values are endpoint epochs.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
struct Health {
    /// Seat that last served a successful response; tried first.
    preferred: Option<[u8; 32]>,

    /// Seat that last failed after connecting; tried last.
    avoid: Option<[u8; 32]>,
}

impl Health {
    /// Move `epoch` from first to last place in the next ranking.
    fn demote(&mut self, epoch: [u8; 32]) {
        if self.preferred == Some(epoch) {
            self.preferred = None;
        }
        self.avoid = Some(epoch);
    }
}

/// The discovered seat a connection reached, so a bad response demotes it.
struct Route {
    /// Ranking hints of the transport that opened the connection.
    health: std::rc::Weak<std::cell::Cell<Health>>,

    /// Endpoint epoch of the seat.
    epoch: [u8; 32],
}

impl Route {
    fn demote(&self) {
        if let Some(health) = self.health.upgrade() {
            let mut value = health.get();
            value.demote(self.epoch);
            health.set(value);
        }
    }

    fn promote(&self) {
        if let Some(health) = self.health.upgrade() {
            let mut value = health.get();
            value.preferred = Some(self.epoch);
            if value.avoid == Some(self.epoch) {
                value.avoid = None;
            }
            health.set(value);
        }
    }
}

/// An exclusive TLS connection, consumed by one request before possible reuse.
pub struct Connection<I: Io + ?Sized> {
    /// Pooled socket, admission slot, and record layer.
    lease: pool::ConnectionLease<PoolContext<I>>,

    /// Owner and pool retained even if the transport attaches a new one.
    attached: Attached<I>,

    /// Readiness owner retained even if the transport attaches a new one.
    io: Rc<I>,

    /// Pool key used to return this connection.
    key: PoolKey,

    /// Original endpoint authority used for the HTTP Host header.
    host: String,

    /// Bootstrap or authenticated pooling and lifetime policy.
    authentication: Authentication,

    /// Maximum response body size for statuses other than 200.
    max_error_body: usize,

    /// Discovered seat, if any, ranked by this connection's outcome.
    route: Option<Route>,

    /// Non-owning destination for successful connection recycling.
    idle: std::rc::Weak<RefCell<Vec<Pooled>>>,

    /// Current generation of the transport that opened this connection.
    generation: std::rc::Weak<std::cell::Cell<u64>>,
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
enum Authentication {
    /// Server-authenticated TLS without a client identity; never reused.
    Bootstrap,

    /// Client-authenticated TLS with its identity lifetime.
    Authenticated(Authenticated),
}

/// Identity lifetime and trust epoch required for authenticated reuse.
#[derive(Clone, Copy)]
struct Authenticated {
    /// Wall-clock identity deadline checked during active I/O.
    expires: SystemTime,

    /// Trust and certificate-chain fingerprint used at checkout.
    epoch: [u8; 32],

    /// Monotonic start of the current idle interval.
    idle_since: Instant,

    /// Redistribution is checked only between requests, never during a long poll.
    retire_at: Instant,
}

/// Minimum lifetime before an authenticated connection is redistributed.
const AUTHENTICATED_AGE_MIN: Duration = Duration::from_secs(240);

/// Maximum extra lifetime used to spread reconnections across owners.
const AUTHENTICATED_AGE_JITTER_MS: u64 = 60_000;

/// Spread authenticated retirement over the inclusive four-to-five-minute range.
fn authenticated_age(random: u64) -> Duration {
    AUTHENTICATED_AGE_MIN + Duration::from_millis(random % (AUTHENTICATED_AGE_JITTER_MS + 1))
}

impl Drop for Response {
    /// Erase response plaintext before releasing its allocation.
    fn drop(&mut self) {
        self.body.zeroize();
    }
}

/// Validated TLS server name, HTTP authority, and connection port.
struct Endpoint {
    /// Unbracketed name or IP address used for TLS and resolution.
    host: String,

    /// Original authority, including brackets and any explicit port.
    authority: String,

    /// Validated nonzero TCP port.
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
            attached: RefCell::new(None),
            idle: Rc::new(RefCell::new(Vec::new())),
            generation: Rc::new(std::cell::Cell::new(next_generation())),
            discovered: RefCell::new(None),
            health: Rc::new(std::cell::Cell::new(Health::default())),
        }
    }

    /// Use a discovered, pinned endpoint set instead of the static URL and trust.
    ///
    /// Replaces the generation as well as idle connections, so a connection
    /// checked out under the previous set can never be recycled afterward.
    pub fn set_endpoints(&self, endpoints: DiscoveredEndpoints) {
        self.invalidate();
        self.health.set(Health::default());
        *self.discovered.borrow_mut() = Some(endpoints);
    }

    /// Move the last successful discovered seat to the end of the next ranking.
    ///
    /// For failures seen above HTTP, such as a stale cursor in a 200
    /// response. Drops idle connections so the next request reconnects.
    /// No effect on ranking without discovered endpoints.
    pub fn demote(&self) {
        self.close_idle();
        let mut health = self.health.get();
        if let Some(preferred) = health.preferred {
            health.demote(preferred);
        }
        self.health.set(health);
    }

    /// Replace the owner, discarding idle TLS and invalidating old recycling rights.
    pub fn attach_io(&self, io: Rc<I>) {
        self.invalidate();
        let context = Rc::new(PoolContext(io.clone()));
        let reactor = io.reactor();
        let pool = HttpPool::new(
            reactor.clone(),
            context.clone(),
            pool::PoolConfig {
                per_endpoint: POOL_PER_ENDPOINT,
                secondary_cap: POOL_PER_ENDPOINT,
                max_endpoints: 2 * MAX_DISCOVERED_ENDPOINTS + 64,
                waiter_cap: 1,
                idle_timeout: IDLE_REUSE,
                tcp_nodelay: true,
            },
        );
        let http = HttpIo::new(
            reactor,
            http1::Codec::new(MAX_HEAD),
            context,
            u64::MAX,
            u64::MAX,
        );
        if let Some(old) = self.attached.borrow_mut().replace(Attached {
            io,
            pool: Rc::new(pool),
            http: Rc::new(http),
        }) {
            old.pool.release_idle();
        }
    }

    /// Retrieve the attached owner or report missing configuration.
    pub fn io(&self) -> Result<Rc<I>, I::Error> {
        Ok(self.attached()?.io)
    }

    /// Retrieve the attachment or report missing configuration.
    fn attached(&self) -> Result<Attached<I>, I::Error> {
        self.attached
            .borrow()
            .clone()
            .ok_or_else(|| Error::InvalidConfiguration.into())
    }

    /// Drop idle connections without interrupting checked-out requests.
    pub fn close_idle(&self) {
        self.idle.borrow_mut().clear();
        if let Some(attached) = &*self.attached.borrow() {
            attached.pool.release_idle();
        }
    }

    /// Whether any live authenticated connection is idle under this generation.
    #[cfg(test)]
    fn has_idle(&self) -> bool {
        let generation = self.generation.get();
        let mut idle = self.idle.borrow_mut();
        idle.retain(|p| p.socket.strong_count() > 0 && p.key.generation == generation);
        !idle.is_empty()
    }

    /// Fence every connection opened so far and release idle ones.
    fn invalidate(&self) {
        self.generation.set(next_generation());
        self.close_idle();
    }

    /// Reread trust and reuse eligible authenticated TLS, or establish fresh TLS.
    pub fn connect<'a>(
        &'a self,
        identity: Option<Identity<'a>>,
        scope: &'a I::Scope,
    ) -> Operation<'a, Connection<I>, I::Error> {
        Box::pin(async move {
            scope.check()?;
            if let Some(identity) = identity
                && self.io()?.ctx().clock.wall_now() >= identity.expires
            {
                return Err(Error::Unauthorized.into());
            }
            let discovered = self.discovered.borrow().clone();
            if let Some(discovered) = discovered {
                return self.connect_discovered(&discovered, identity, scope).await;
            }
            let endpoint = endpoint(&self.config.url)?;
            let attached = self.attached()?;
            let generation = self.generation.get();
            let trust = attached
                .io
                .read_file(
                    &self.config.trust_bundle,
                    self.config.max_trust_bundle,
                    scope,
                )
                .await?;
            let epoch = trust_epoch(trust.as_ref(), identity);
            if let Some(connection) = self.checkout(&attached, identity, &[epoch], scope).await {
                return Ok(connection);
            }
            let config = tls_config(trust.as_ref(), identity)?;
            self.handshake(
                attached, endpoint, config, identity, epoch, generation, scope,
            )
            .await
        })
    }

    /// Reuse a connection to any current endpoint, else try each in ranked order.
    ///
    /// Every endpoint receives a share of the remaining deadline. Failures,
    /// including pin mismatches against a stale endpoint, move on to the next
    /// endpoint and finally report `Unavailable` so callers retry with backoff
    /// after discovery refreshes the set.
    async fn connect_discovered(
        &self,
        discovered: &DiscoveredEndpoints,
        identity: Option<Identity<'_>>,
        scope: &I::Scope,
    ) -> Result<Connection<I>, I::Error> {
        let endpoints = discovered
            .read()
            .map_err(|_| Error::Internal)?
            .iter()
            .take(MAX_DISCOVERED_ENDPOINTS)
            .cloned()
            .collect::<Vec<_>>();
        let attached = self.attached()?;
        let generation = self.generation.get();
        let epochs = endpoints
            .iter()
            .map(|e| discovered_epoch(e, identity))
            .collect::<Vec<_>>();
        if let Some(mut connection) = self.checkout(&attached, identity, &epochs, scope).await {
            connection.route = Some(Route {
                health: Rc::downgrade(&self.health),
                epoch: connection.key.epoch,
            });
            return Ok(connection);
        }
        let io = attached.io.clone();
        let random = io.ctx().rng.next_u64().map_err(|_| Error::Io)?;
        let health = self.health.get();
        let order = rank(&epochs, health.preferred, health.avoid, random);
        for (index, position) in order.iter().enumerate() {
            scope.check()?;
            let candidate = &endpoints[*position];
            let Ok(parsed) = endpoint(&candidate.url) else {
                continue;
            };
            let Ok(config) = pinned_tls_config(&candidate.certificate, identity) else {
                continue;
            };
            let now = io.ctx().clock.now();
            let narrowed;
            let attempt = match scope.deadline() {
                Some(_) if index + 1 == order.len() => scope,
                Some(deadline) => {
                    narrowed = scope.narrowed(
                        now + deadline.saturating_duration_since(now)
                            / (order.len() - index) as u32,
                    );
                    &narrowed
                }
                None => scope,
            };
            let result = self
                .handshake(
                    attached.clone(),
                    parsed,
                    config,
                    identity,
                    epochs[*position],
                    generation,
                    attempt,
                )
                .await;
            scope.check()?;
            if let Ok(mut connection) = result {
                // Ranking changes only after a response, so a seat that
                // accepts TLS but answers badly is not preferred.
                connection.route = Some(Route {
                    health: Rc::downgrade(&self.health),
                    epoch: epochs[*position],
                });
                return Ok(connection);
            }
        }
        let mut health = self.health.get();
        health.preferred = None;
        self.health.set(health);
        Err(Error::Unavailable.into())
    }

    /// Resolve, then connect and complete TLS through the pool, trying each
    /// address with a share of the remaining deadline.
    #[allow(clippy::too_many_arguments)]
    async fn handshake(
        &self,
        attached: Attached<I>,
        endpoint: Endpoint,
        config: rustls::ClientConfig,
        identity: Option<Identity<'_>>,
        epoch: [u8; 32],
        generation: u64,
        scope: &I::Scope,
    ) -> Result<Connection<I>, I::Error> {
        let io = attached.io.clone();
        let addresses = if let Ok(ip) = endpoint.host.parse() {
            vec![SocketAddr::new(ip, endpoint.port)]
        } else {
            io.resolve(&endpoint.host, endpoint.port, scope).await?
        };
        if addresses.is_empty() || addresses.len() > 64 {
            return Err(Error::Unavailable.into());
        }
        let tls = TlsClient::new(Arc::new(config), &endpoint.host)
            .map_err(|_| Error::InvalidConfiguration)?;
        let bounded = bounded_scope(io.as_ref(), identity.map(|i| i.expires), scope)?;
        for (index, address) in addresses.iter().enumerate() {
            bounded.check()?;
            let key = PoolKey {
                address: *address,
                tls: tls.clone(),
                epoch,
                generation,
            };
            // Idle connections are adopted only through `checkout`, which
            // applies this transport's reuse policy.
            attached.pool.discard_idle(&key);
            // Each remaining address gets an equal share, so one blackholed
            // address cannot consume the parent's entire connection deadline.
            let now = io.ctx().clock.now();
            let attempt = match bounded.deadline() {
                // The last address runs under the caller's own scope, so its
                // expiry reports the caller's deadline rather than a share.
                Some(_) if index + 1 == addresses.len() => bounded.clone(),
                Some(deadline) => bounded.narrowed(
                    now + deadline.saturating_duration_since(now)
                        / (addresses.len() - index) as u32,
                ),
                None => bounded.clone(),
            };
            let result = attached.pool.checkout(&key, &attempt).await;
            // Cancellation, identity expiry, and overall expiry always win.
            check_identity(io.as_ref(), identity.map(|i| i.expires), scope)?;
            bounded.check()?;
            match result {
                Ok(mut lease) => {
                    if lease.reused() {
                        // Idle connections were just discarded; never adopt
                        // one without this transport's reuse metadata.
                        lease.poison();
                        continue;
                    }
                    let authentication = self.authentication(io.ctx(), identity, epoch)?;
                    return Ok(Connection {
                        lease,
                        attached: attached.clone(),
                        io: io.clone(),
                        key,
                        host: endpoint.authority,
                        authentication,
                        max_error_body: self.config.max_error_body,
                        route: None,
                        idle: Rc::downgrade(&self.idle),
                        generation: Rc::downgrade(&self.generation),
                    });
                }
                Err(error) if error == racer_io::Error::Os(libc::EPROTO).into() => {
                    return Err(Error::Unauthorized.into());
                }
                Err(error)
                    if error == racer_io::Error::Unavailable.into()
                        || error == racer_io::Error::DeadlineExceeded.into()
                        || error == racer_io::Error::Io.into()
                        || error == Error::Io.into()
                        || error == Error::Unavailable.into()
                        || is_socket_error(error) =>
                {
                    continue;
                }
                Err(error) => return Err(error),
            }
        }
        scope.check()?;
        Err(Error::Unavailable.into())
    }

    /// Take an eligible idle connection for one of `epochs`, discarding any
    /// ineligible idle connections met on the way.
    async fn checkout(
        &self,
        attached: &Attached<I>,
        identity: Option<Identity<'_>>,
        epochs: &[[u8; 32]],
        scope: &I::Scope,
    ) -> Option<Connection<I>> {
        identity?;
        let generation = self.generation.get();
        loop {
            let now = attached.io.ctx().clock.now();
            let wall = attached.io.ctx().clock.wall_now();
            let (pooled, stale) = {
                let mut idle = self.idle.borrow_mut();
                idle.retain(|p| p.socket.strong_count() > 0 && p.key.generation == generation);
                // Connections under a trust epoch no longer current are
                // closed rather than left for a later checkout.
                let stale = idle
                    .extract_if(.., |p| !epochs.contains(&p.auth.epoch))
                    .collect::<Vec<_>>();
                (idle.pop(), stale)
            };
            for pooled in stale {
                attached.pool.discard_idle(&pooled.key);
            }
            let pooled = pooled?;
            let eligible = now < pooled.auth.retire_at
                && now.saturating_duration_since(pooled.auth.idle_since) < IDLE_REUSE
                && wall < pooled.auth.expires
                && scope.check().is_ok();
            if !eligible {
                attached.pool.discard_idle(&pooled.key);
                continue;
            }
            let Ok(mut lease) = attached.pool.checkout(&pooled.key, scope).await else {
                return None;
            };
            let same = pooled
                .socket
                .upgrade()
                .is_some_and(|socket| Rc::ptr_eq(&socket, &lease.socket()));
            if !lease.reused() || !same {
                // The pool closed or reordered this connection; never adopt
                // an unknown socket as authenticated reuse.
                lease.poison();
                attached.pool.discard_idle(&pooled.key);
                continue;
            }
            return Some(Connection {
                lease,
                attached: attached.clone(),
                io: attached.io.clone(),
                host: pooled.host,
                key: pooled.key,
                authentication: Authentication::Authenticated(pooled.auth),
                max_error_body: self.config.max_error_body,
                route: None,
                idle: Rc::downgrade(&self.idle),
                generation: Rc::downgrade(&self.generation),
            });
        }
    }

    /// Create pooling metadata only for authenticated connections.
    fn authentication(
        &self,
        ctx: &racer_io::WorkerCtx,
        identity: Option<Identity<'_>>,
        epoch: [u8; 32],
    ) -> Result<Authentication> {
        let Some(identity) = identity else {
            return Ok(Authentication::Bootstrap);
        };
        let random = ctx.rng.next_u64().map_err(|_| Error::Io)?;
        let now = ctx.clock.now();
        Ok(Authentication::Authenticated(Authenticated {
            expires: identity.expires,
            epoch,
            idle_since: now,
            retire_at: now + authenticated_age(random),
        }))
    }
}

/// Whether an error is an operating-system socket failure worth trying the
/// next address for.
fn is_socket_error<E: PartialEq + From<racer_io::Error>>(error: E) -> bool {
    [
        libc::ECONNREFUSED,
        libc::ECONNRESET,
        libc::ENETUNREACH,
        libc::EHOSTUNREACH,
        libc::ETIMEDOUT,
        libc::EADDRNOTAVAIL,
        libc::ECONNABORTED,
        libc::EPIPE,
    ]
    .into_iter()
    .any(|errno| error == racer_io::Error::Os(errno).into())
}

/// Fail with `Unauthorized` once the identity has expired.
fn check_identity<I: Io + ?Sized>(
    io: &I,
    expires: Option<SystemTime>,
    scope: &I::Scope,
) -> Result<(), I::Error> {
    scope.check()?;
    if expires.is_some_and(|expires| io.ctx().clock.wall_now() >= expires) {
        return Err(Error::Unauthorized.into());
    }
    Ok(())
}

/// Limit `scope` to the identity's remaining lifetime.
fn bounded_scope<I: Io + ?Sized>(
    io: &I,
    expires: Option<SystemTime>,
    scope: &I::Scope,
) -> Result<I::Scope, I::Error> {
    check_identity(io, expires, scope)?;
    let Some(expires) = expires else {
        return Ok(scope.clone());
    };
    let remaining = expires
        .duration_since(io.ctx().clock.wall_now())
        .map_err(|_| Error::Unauthorized)?;
    Ok(scope.narrowed(io.ctx().clock.now() + remaining))
}

impl<I: Io + ?Sized> Connection<I> {
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
            let head = request_head(&self.host, method, path, token, body.len(), header)?;
            let mut wire = zeroize::Zeroizing::new(Vec::new());
            wire.try_reserve_exact(head.len() + body.len())
                .map_err(|_| Error::Overloaded)?;
            wire.extend_from_slice(head.as_bytes());
            wire.extend_from_slice(body);
            drop(head);
            let route = self.route.take();
            let result = self.exchange(&wire, limit, scope).await;
            drop(wire);
            let (connection, head, body) = match result {
                Ok(exchanged) => exchanged,
                Err(error) => {
                    if let Some(route) = &route {
                        route.demote();
                    }
                    return Err(error);
                }
            };
            self = connection;
            // Any non-success status, including 401, 403, and 503 from a
            // live but stale seat, moves the next attempt to another seat.
            if let Some(route) = &route {
                if matches!(head.status, 200 | 204) {
                    route.promote();
                } else {
                    route.demote();
                }
            }
            self.recycle(&head);
            Ok(Response {
                status: head.status,
                body: body.to_vec(),
                retry_after: head.retry_after,
            })
        })
    }

    /// Send one request and receive its framed response on this connection.
    async fn exchange(
        self,
        wire: &[u8],
        limit: usize,
        scope: &I::Scope,
    ) -> Result<(Self, ResponseHead, zeroize::Zeroizing<Vec<u8>>), I::Error> {
        let Self {
            lease,
            attached,
            io,
            key,
            host,
            authentication,
            max_error_body,
            route,
            idle,
            generation,
        } = self;
        let expires = match &authentication {
            Authentication::Authenticated(auth) => Some(auth.expires),
            Authentication::Bootstrap => None,
        };
        let bounded = bounded_scope(io.as_ref(), expires, scope)?;
        let result = {
            let ctx = io.ctx();
            let mut parse = |bytes: &[u8]| -> Result<_, I::Error> {
                Ok(
                    parse_head(bytes, limit, max_error_body, ctx.clock.wall_now())?.map(|head| {
                        http1::small_exchange::Plan {
                            head_length: head.header_len,
                            framing: head.framing,
                            limits: small::Limits {
                                body: head.bound,
                                line: 128,
                                trailers: 4096,
                            },
                            close: head.close,
                            body_error: |error| body_error(error).into(),
                            value: head,
                        }
                    }),
                )
            };
            attached
                .http
                .exchange_small(lease, wire, MAX_HEAD, &mut parse, &bounded)
                .await
        };
        check_identity(io.as_ref(), expires, scope)?;
        let exchanged = result.map_err(|error| {
            if error == http1::Error::Malformed.into() {
                Error::InvalidRequest.into()
            } else if error == http1::Error::HeadTooLarge.into() {
                Error::Overloaded.into()
            } else if error == racer_io::Error::Io.into() || is_socket_error(error) {
                Error::Io.into()
            } else {
                error
            }
        })?;
        Ok((
            Self {
                lease: exchanged.connection,
                attached,
                io,
                key,
                host,
                authentication,
                max_error_body,
                route,
                idle,
                generation,
            },
            exchanged.value,
            exchanged.body,
        ))
    }

    /// Check redistribution age only at request boundaries, not during I/O.
    fn within_max_age(&self) -> bool {
        match &self.authentication {
            Authentication::Bootstrap => true,
            Authentication::Authenticated(auth) => self.io.ctx().clock.now() < auth.retire_at,
        }
    }

    /// Check caller cancellation first, then the authenticated identity lifetime.
    #[cfg(test)]
    fn check(&self, scope: &I::Scope) -> Result<(), I::Error> {
        let expires = match &self.authentication {
            Authentication::Authenticated(auth) => Some(auth.expires),
            Authentication::Bootstrap => None,
        };
        check_identity(self.io.as_ref(), expires, scope)
    }

    /// Recycle only completed successful responses; failures reselect a backend.
    fn recycle(mut self, head: &ResponseHead) {
        // An aged connection finishes its in-flight response, then retires.
        if self.lease.is_reusable()
            && !head.close
            && matches!(head.status, 200 | 204)
            && self.within_max_age()
            && self
                .generation
                .upgrade()
                .is_some_and(|current| current.get() == self.key.generation)
            && let Authentication::Authenticated(auth) = &mut self.authentication
            && let Some(idle) = self.idle.upgrade()
        {
            auth.idle_since = self.io.ctx().clock.now();
            idle.borrow_mut().push(Pooled {
                socket: Rc::downgrade(&self.lease.socket()),
                key: self.key.clone(),
                auth: *auth,
                host: self.host.clone(),
            });
            return;
        }
        self.lease.poison();
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

/// Hash trust bytes and length-delimited certificates without copying key material.
fn trust_epoch(trust: &[u8], identity: Option<Identity<'_>>) -> [u8; 32] {
    use sha2::Digest;
    let mut epoch = sha2::Sha256::new();
    epoch.update(trust);
    if let Some(identity) = identity {
        for certificate in identity.certificate_chain {
            epoch.update((certificate.len() as u64).to_be_bytes());
            epoch.update(certificate);
        }
    }
    epoch.finalize().into()
}

/// Bind pooled reuse to one endpoint's URL, pin, election generation, and identity.
fn discovered_epoch(endpoint: &DiscoveredEndpoint, identity: Option<Identity<'_>>) -> [u8; 32] {
    use sha2::Digest;
    let mut epoch = sha2::Sha256::new();
    epoch.update(b"racer-discovered-endpoint\0");
    for field in [
        endpoint.url.as_bytes(),
        &endpoint.certificate,
        endpoint.generation.as_bytes(),
    ] {
        epoch.update((field.len() as u64).to_be_bytes());
        epoch.update(field);
    }
    epoch.update(trust_epoch(&[], identity));
    epoch.finalize().into()
}

/// Order the last successful endpoint first, then the rest from a random rotation.
///
/// The random start spreads many nodes across live seats after a failure,
/// instead of every node retrying the first listed endpoint at once.
fn rank(
    epochs: &[[u8; 32]],
    preferred: Option<[u8; 32]>,
    avoid: Option<[u8; 32]>,
    random: u64,
) -> Vec<usize> {
    let count = epochs.len();
    if count == 0 {
        return Vec::new();
    }
    let start = (random % count as u64) as usize;
    let mut order = (0..count)
        .map(|offset| (start + offset) % count)
        .collect::<Vec<_>>();
    if let Some(avoid) = avoid
        && let Some(position) = order.iter().position(|i| epochs[*i] == avoid)
    {
        let last = order.remove(position);
        order.push(last);
    }
    if let Some(preferred) = preferred
        && Some(preferred) != avoid
        && let Some(position) = order.iter().position(|i| epochs[*i] == preferred)
    {
        let first = order.remove(position);
        order.insert(0, first);
    }
    order
}

/// Accept exactly the pinned certificate, after full WebPKI verification against it.
#[derive(Debug)]
struct PinnedVerifier {
    /// Exact DER end-entity certificate the server must present.
    pinned: Vec<u8>,

    /// Hostname, validity, usage, and signature checks rooted at the pin.
    inner: Arc<rustls::client::WebPkiServerVerifier>,
}

impl rustls::client::danger::ServerCertVerifier for PinnedVerifier {
    /// Require the exact pinned leaf with no chain, then delegate WebPKI checks.
    fn verify_server_cert(
        &self,
        end_entity: &rustls::pki_types::CertificateDer<'_>,
        intermediates: &[rustls::pki_types::CertificateDer<'_>],
        server_name: &rustls::pki_types::ServerName<'_>,
        ocsp: &[u8],
        now: rustls::pki_types::UnixTime,
    ) -> std::result::Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        if end_entity.as_ref() != self.pinned.as_slice() || !intermediates.is_empty() {
            return Err(rustls::Error::InvalidCertificate(
                rustls::CertificateError::UnknownIssuer,
            ));
        }
        self.inner
            .verify_server_cert(end_entity, intermediates, server_name, ocsp, now)
    }

    /// TLS 1.2 is never negotiated; delegate for completeness.
    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &rustls::pki_types::CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> std::result::Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    /// Verify the handshake signature with the pinned key.
    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &rustls::pki_types::CertificateDer<'_>,
        dss: &rustls::DigitallySignedStruct,
    ) -> std::result::Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    /// Offer the provider's supported signature schemes.
    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

/// Build TLS 1.3 configuration trusting only one pinned self-signed certificate.
fn pinned_tls_config(
    certificate: &[u8],
    identity: Option<Identity<'_>>,
) -> Result<rustls::ClientConfig> {
    let provider = Arc::new(http1::tls::kernel_provider());
    let mut roots = rustls::RootCertStore::empty();
    roots
        .add(rustls::pki_types::CertificateDer::from(
            certificate.to_vec(),
        ))
        .map_err(|_| Error::Unauthorized)?;
    let inner = rustls::client::WebPkiServerVerifier::builder_with_provider(
        Arc::new(roots),
        provider.clone(),
    )
    .build()
    .map_err(|_| Error::Unauthorized)?;
    let builder = rustls::ClientConfig::builder_with_provider(provider)
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| Error::Unauthorized)?
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(PinnedVerifier {
            pinned: certificate.to_vec(),
            inner,
        }));
    finish_tls_config(builder, identity)
}

/// Build fresh TLS 1.3 trust and optional client authentication without resumption.
fn tls_config(mut trust: &[u8], identity: Option<Identity<'_>>) -> Result<rustls::ClientConfig> {
    let mut roots = rustls::RootCertStore::empty();
    for cert in rustls_pemfile::certs(&mut trust) {
        roots
            .add(cert.map_err(|_| Error::Unauthorized)?)
            .map_err(|_| Error::Unauthorized)?;
    }
    if roots.is_empty() {
        return Err(Error::Unauthorized);
    }
    let builder =
        rustls::ClientConfig::builder_with_provider(Arc::new(http1::tls::kernel_provider()))
            .with_protocol_versions(&[&rustls::version::TLS13])
            .map_err(|_| Error::Unauthorized)?
            .with_root_certificates(roots);
    finish_tls_config(builder, identity)
}

/// Add optional client authentication and the pooled-connection TLS requirements.
fn finish_tls_config(
    builder: rustls::ConfigBuilder<rustls::ClientConfig, rustls::client::WantsClientCert>,
    identity: Option<Identity<'_>>,
) -> Result<rustls::ClientConfig> {
    let mut config = if let Some(identity) = identity {
        builder
            .with_client_auth_cert(
                identity
                    .certificate_chain
                    .iter()
                    .cloned()
                    .map(rustls::pki_types::CertificateDer::from)
                    .collect(),
                rustls::pki_types::PrivatePkcs8KeyDer::from(identity.private_key.to_vec()).into(),
            )
            .map_err(|_| Error::Unauthorized)?
    } else {
        builder.with_no_client_auth()
    };
    // Pooled connections may move their record layer into kernel TLS.
    http1::tls::prepare_client_config(&mut config);
    Ok(config)
}

#[cfg(test)]
mod discovery_tests {
    use super::*;

    /// Ranking is a permutation that leads with the preferred seat when present.
    #[test]
    fn rank_prefers_last_success_and_spreads_start() {
        assert!(rank(&[], None, None, 7).is_empty());
        let epochs = [[1; 32], [2; 32], [3; 32]];
        let mut starts = std::collections::BTreeSet::new();
        for random in 0..6 {
            let mut order = rank(&epochs, None, None, random);
            starts.insert(order[0]);
            order.sort();
            assert_eq!(order, [0, 1, 2]);
            assert_eq!(rank(&epochs, Some([3; 32]), None, random)[0], 2);
            assert_eq!(
                rank(&epochs, Some([9; 32]), None, random)[0],
                (random % 3) as usize
            );
        }
        assert_eq!(starts.len(), 3);
    }

    /// A failed seat goes last, even when it was preferred.
    #[test]
    fn rank_moves_failed_seat_last() {
        let epochs = [[1; 32], [2; 32], [3; 32]];
        for random in 0..6 {
            let order = rank(&epochs, None, Some([1; 32]), random);
            assert_eq!(order[2], 0);
            let order = rank(&epochs, Some([1; 32]), Some([1; 32]), random);
            assert_eq!(order[2], 0);
            let order = rank(&epochs, Some([3; 32]), Some([1; 32]), random);
            assert_eq!((order[0], order[2]), (2, 0));
            let mut sorted = order.clone();
            sorted.sort();
            assert_eq!(sorted, [0, 1, 2]);
        }
    }

    /// Health keeps one preferred and one avoided seat.
    #[test]
    fn health_demote_clears_matching_preference() {
        let mut health = Health {
            preferred: Some([1; 32]),
            avoid: None,
        };
        health.demote([2; 32]);
        assert_eq!(health.preferred, Some([1; 32]));
        assert_eq!(health.avoid, Some([2; 32]));
        health.demote([1; 32]);
        assert_eq!(health.preferred, None);
        assert_eq!(health.avoid, Some([1; 32]));
    }

    /// Endpoint epochs separate URL, pin, generation, and client identity.
    #[test]
    fn discovered_epoch_binds_every_field() {
        let base = DiscoveredEndpoint {
            url: "https://10.0.0.1:8443".into(),
            certificate: vec![1, 2],
            generation: "g".into(),
        };
        let mut url = base.clone();
        url.url = "https://10.0.0.2:8443".into();
        let mut pin = base.clone();
        pin.certificate = vec![1, 3];
        let mut generation = base.clone();
        generation.generation = "h".into();
        let chain = vec![vec![9]];
        let identity = Identity {
            certificate_chain: &chain,
            private_key: &[],
            expires: SystemTime::UNIX_EPOCH,
        };
        let epochs = [
            discovered_epoch(&base, None),
            discovered_epoch(&url, None),
            discovered_epoch(&pin, None),
            discovered_epoch(&generation, None),
            discovered_epoch(&base, Some(identity)),
        ];
        for (i, left) in epochs.iter().enumerate() {
            for right in &epochs[i + 1..] {
                assert_ne!(left, right);
            }
        }
        assert_eq!(discovered_epoch(&base, None), epochs[0]);
    }
}

/// Validated response metadata, including the connection reuse directive.
#[derive(Debug, Eq, PartialEq)]
struct ResponseHead {
    /// Parsed HTTP status used for body limits and pool policy.
    status: u16,

    /// Offset of the first body byte in the accumulated plaintext.
    header_len: usize,

    /// Fixed length or chunked framing, never close-delimited.
    framing: small::Framing,

    /// Status-specific decoded body limit.
    bound: usize,

    /// Server retry hint, already converted to a relative delay.
    retry_after: Option<Duration>,

    /// Whether any Connection header asks to close the socket.
    close: bool,
}

/// Map body framing failures onto transport errors: size is overload, the rest
/// are protocol violations.
fn body_error(error: small::Error) -> Error {
    match error {
        small::Error::TooLarge => Error::Overloaded,
        small::Error::Malformed | small::Error::Truncated => Error::InvalidRequest,
    }
}

/// Map head syntax failures; an oversized head is overload.
fn head_error(error: http::Error) -> Error {
    match error {
        http::Error::HeadTooLarge => Error::Overloaded,
        http::Error::Malformed => Error::InvalidRequest,
    }
}

/// Parse framing and reuse metadata together, preserving failure precedence.
fn parse_head(
    bytes: &[u8],
    limit: usize,
    max_error_body: usize,
    wall_now: SystemTime,
) -> Result<Option<ResponseHead>> {
    let codec = http::Codec::<()>::new(MAX_HEAD).transfer_coding();
    let Some((head, length)) = codec.decode_head(bytes).map_err(head_error)? else {
        return Ok(None);
    };
    let http::StartLine::Response { status } = head.start else {
        return Err(Error::InvalidRequest);
    };
    if head.values("content-encoding").next().is_some() {
        return Err(Error::InvalidRequest);
    }
    let framing = small::Framing::from_response(&head).map_err(body_error)?;
    let retry = head
        .unique("retry-after")
        .map_err(head_error)?
        .map(|value| retry_delay(value, wall_now))
        .transpose()?;
    let content_type = head.unique("content-type").map_err(head_error)?;
    let framing = if status == 204 {
        match framing {
            small::Framing::Length(0) | small::Framing::Close => small::Framing::Length(0),
            _ => return Err(Error::InvalidRequest),
        }
    } else {
        if !content_type.is_some_and(|value| {
            std::str::from_utf8(value)
                .ok()
                .and_then(|value| value.split(';').next())
                .is_some_and(|value| value.trim().eq_ignore_ascii_case("application/json"))
        }) {
            return Err(Error::InvalidRequest);
        }
        match framing {
            small::Framing::Close => return Err(Error::InvalidRequest),
            framing => framing,
        }
    };
    let bound = if status == 200 { limit } else { max_error_body };
    if let small::Framing::Length(size) = framing
        && usize::try_from(size).map_or(true, |size| size > bound)
    {
        return Err(Error::Overloaded);
    }
    let close = head.closes_connection().map_err(head_error)?;
    // Every header, including ignored ones, must be valid UTF-8.
    std::str::from_utf8(&bytes[..length]).map_err(|_| Error::InvalidRequest)?;
    Ok(Some(ResponseHead {
        status,
        header_len: length,
        framing,
        bound,
        retry_after: retry,
        close,
    }))
}

/// Decode delta seconds or IMF-fixdate without locale or time-zone globals.
fn retry_delay(bytes: &[u8], wall_now: SystemTime) -> Result<Duration> {
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
    Ok(target.duration_since(wall_now).unwrap_or_default())
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
    use racer_io::{Scope as _, ring::Descriptor};
    use std::{
        net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, UdpSocket},
        rc::Rc,
        time::Duration,
    };

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
                    let attempt = scope.narrowed(io.ctx().clock.now() + Duration::from_secs(2));
                    match query(io, *server, &name, kind, &attempt).await {
                        Ok(ips) => {
                            addresses.extend(ips.into_iter().map(|ip| SocketAddr::new(ip, port)))
                        }
                        Err(error) if error == racer_io::Error::Cancelled.into() => {
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

    /// Bind an ephemeral UDP socket restricted to the chosen resolver.
    ///
    /// The descriptor is driven by reactor `send`/`recv` operations, which the
    /// kernel and simulated drivers both complete, so a query never polls
    /// readiness by hand.
    fn connect(server: SocketAddr, sim: Option<racer_io::sim::SimOs>) -> Result<Rc<Descriptor>> {
        if let Some(sim) = sim {
            let local = if server.is_ipv4() {
                SocketAddr::from((Ipv4Addr::LOCALHOST, 0))
            } else {
                SocketAddr::from((Ipv6Addr::LOCALHOST, 0))
            };
            let fd = sim.bind_datagram(local).map_err(|_| Error::Io)?;
            let Some(handle) = fd.as_sim() else {
                unreachable!()
            };
            handle.connect_datagram(server).map_err(|_| Error::Io)?;
            return Ok(Rc::new(fd));
        }
        let socket = UdpSocket::bind(if server.is_ipv4() {
            "0.0.0.0:0"
        } else {
            "[::]:0"
        })
        .map_err(|_| Error::Io)?;
        socket.set_nonblocking(true).map_err(|_| Error::Io)?;
        socket.connect(server).map_err(|_| Error::Io)?;
        Ok(Rc::new(Descriptor::from(socket)))
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
        io.ctx().rng.fill(&mut id).map_err(|_| Error::Io)?;
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
        let fd = connect(server, io.sim())?;
        let reactor = io.reactor();
        let length = request.len();
        let sent = reactor.send(fd.clone(), request, (), scope).await?;
        if sent.bytes != length {
            return Err(Error::Io.into());
        }
        let request = sent.buffer;
        let received = reactor.recv(fd, vec![0; 4096], (), scope).await?;
        parse(&received.buffer[..received.bytes], &request, kind).map_err(Into::into)
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

        use super::super::{Io, Scope as RestScope, testing::Error as FixtureError};
        use racer_io::{Env, Operation, SimClock, sim::SimOs};
        use std::{
            cell::Cell,
            path::Path,
            pin::pin,
            task::{Context, Poll, Waker},
            time::Instant,
        };

        /// Untimed scope that fails only once the test cancels it.
        #[derive(Clone, Default)]
        struct SimScope(Rc<Cell<bool>>);

        impl racer_io::Scope for SimScope {
            type Error = FixtureError;

            fn check(&self) -> Result<(), FixtureError> {
                if self.0.get() {
                    Err(racer_io::Error::Cancelled.into())
                } else {
                    Ok(())
                }
            }
        }

        impl RestScope for SimScope {
            fn deadline(&self) -> Option<Instant> {
                None
            }

            fn narrowed(&self, _: Instant) -> Self {
                self.clone()
            }
        }

        /// Resolver I/O on a simulated reactor with a fixed resolv.conf.
        struct SimIo {
            reactor: Rc<Box<racer_io::scoped::Reactor<SimScope>>>,
            os: SimOs,
            resolv: &'static str,
        }

        impl Io for SimIo {
            type Error = FixtureError;
            type Scope = SimScope;
            type Lease = ();
            type FileBytes = Vec<u8>;
            type Reactor = Box<racer_io::scoped::Reactor<SimScope>>;
            type Charge = ();

            fn reactor(&self) -> Rc<Self::Reactor> {
                self.reactor.clone()
            }

            fn charge(&self, _: usize) -> Result<(), FixtureError> {
                Ok(())
            }

            fn ctx(&self) -> &racer_io::WorkerCtx {
                self.reactor.ctx()
            }

            fn sim(&self) -> Option<SimOs> {
                Some(self.os.clone())
            }

            fn lease(&self) -> Result<Option<Rc<()>>, FixtureError> {
                Ok(None)
            }

            /// DNS must not fall back to hand-polled readiness.
            fn ready<'a>(
                &'a self,
                _: Rc<Descriptor>,
                _: bool,
                _: bool,
                _: Option<Rc<()>>,
                _: &'a SimScope,
            ) -> Operation<'a, (), FixtureError> {
                panic!("DNS queries run on reactor send and recv operations")
            }

            fn resolve<'a>(
                &'a self,
                _: &'a str,
                _: u16,
                _: &'a SimScope,
            ) -> Operation<'a, Vec<SocketAddr>, FixtureError> {
                unreachable!()
            }

            fn read_file<'a>(
                &'a self,
                path: &'a Path,
                _: usize,
                _: &'a SimScope,
            ) -> Operation<'a, Vec<u8>, FixtureError> {
                assert_eq!(path, Path::new("/etc/resolv.conf"));
                Box::pin(async move { Ok(self.resolv.as_bytes().to_vec()) })
            }
        }

        /// Build a simulated resolver world with one UDP server socket.
        fn world(resolv: &'static str, server: SocketAddr) -> (SimIo, Descriptor) {
            let os = SimOs::new();
            let clock = SimClock::new(7);
            let reactor = racer_io::scoped::Reactor::new(8, &Env::sim(&os, clock.ctx(0)));
            let socket = os.bind_datagram(server).unwrap();
            let io = SimIo {
                reactor: Rc::new(Box::new(reactor)),
                os,
                resolv,
            };
            (io, socket)
        }

        /// Answer one query: `Some(ip)` sends a matching record for A
        /// questions and an empty answer otherwise; `None` corrupts the ID.
        fn answer(server: &Descriptor, ip: Option<Ipv4Addr>) -> Option<Vec<u8>> {
            let server = server.as_sim().unwrap();
            let mut query = [0; 512];
            let (n, source) = server.recv_from(&mut query).ok()?;
            let query = &query[..n];
            let mut reply = query.to_vec();
            reply[2] = 0x81;
            reply[3] = 0x80;
            let kind = u16_at(query, n - 4).unwrap();
            match ip {
                Some(ip) if kind == 1 => {
                    reply[7] = 1;
                    reply.extend_from_slice(&[0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 1, 0, 4]);
                    reply.extend_from_slice(&ip.octets());
                }
                Some(_) => (),
                None => reply[0] ^= 0xff,
            }
            server.send_to(&reply, source).unwrap();
            Some(query.to_vec())
        }

        /// Poll `resolve` on the sim reactor, answering queries as they land.
        fn run(
            io: &SimIo,
            server: &Descriptor,
            host: &str,
            ip: Option<Ipv4Addr>,
            scope: &SimScope,
        ) -> (Result<Vec<SocketAddr>, FixtureError>, Vec<Vec<u8>>) {
            let mut future = pin!(resolve(io, host, 443, scope));
            let mut cx = Context::from_waker(Waker::noop());
            let mut queries = Vec::new();
            for _ in 0..1000 {
                if let Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                    return (result, queries);
                }
                queries.extend(answer(server, ip));
                io.reactor.poll_budgeted(64).unwrap();
            }
            panic!("resolution never settled on the sim reactor");
        }

        /// Search-list resolution completes on the simulated driver without
        /// readiness polling, sending one A and one AAAA question.
        #[test]
        fn resolves_through_sim_reactor_send_and_recv() {
            let server: SocketAddr = "127.0.0.53:53".parse().unwrap();
            let (io, socket) = world("nameserver 127.0.0.53\nsearch svc\n", server);
            let scope = SimScope::default();
            let ip = Ipv4Addr::new(192, 0, 2, 7);
            let (result, queries) = run(&io, &socket, "api", Some(ip), &scope);
            assert_eq!(result.unwrap(), vec![SocketAddr::from((ip, 443))]);
            assert_eq!(queries.len(), 2);
            assert!(
                queries
                    .iter()
                    .all(|q| q.windows(5).any(|w| w == b"\x03api\x03"))
            );
        }

        /// A reply with the wrong transaction ID never yields addresses.
        #[test]
        fn rejects_mismatched_transaction_on_sim_reactor() {
            let server: SocketAddr = "127.0.0.53:53".parse().unwrap();
            let (io, socket) = world("nameserver 127.0.0.53\n", server);
            let scope = SimScope::default();
            let (result, queries) = run(&io, &socket, "api.example.", None, &scope);
            assert_eq!(result, Err(Error::Unavailable.into()));
            assert_eq!(queries.len(), 2);
        }

        /// Cancellation while a query waits on the reactor ends resolution.
        #[test]
        fn cancellation_ends_a_pending_sim_query() {
            let server: SocketAddr = "127.0.0.53:53".parse().unwrap();
            let (io, _socket) = world("nameserver 127.0.0.53\n", server);
            let scope = SimScope::default();
            let mut future = pin!(resolve(&io, "api.example.", 443, &scope));
            let mut cx = Context::from_waker(Waker::noop());
            for _ in 0..8 {
                assert!(future.as_mut().poll(&mut cx).is_pending());
                io.reactor.poll_budgeted(64).unwrap();
            }
            scope.0.set(true);
            let mut result = None;
            for _ in 0..64 {
                if let Poll::Ready(r) = future.as_mut().poll(&mut cx) {
                    result = Some(r);
                    break;
                }
                io.reactor.poll_budgeted(64).unwrap();
            }
            assert_eq!(result, Some(Err(racer_io::Error::Cancelled.into())));
        }
    }
}

/// Loopback fixtures shared with the transport integration suites.
#[cfg(test)]
#[path = "../../tests/support/rest.rs"]
mod testing;

/// Pure parser tests and owner-local pool invariants.
#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read as _, Write as _};
    use testing::{FixtureIo, TestIdentity, TestTransport as _};

    /// Access authenticated metadata only inside tests that mutate pool boundaries.
    fn authenticated(connection: &mut Connection<FixtureIo>) -> &mut Authenticated {
        let Authentication::Authenticated(auth) = &mut connection.authentication else {
            panic!("expected authenticated connection");
        };
        auth
    }

    /// Access the single idle connection's reuse metadata.
    fn idle_auth(transport: &Transport<FixtureIo>) -> std::cell::RefMut<'_, Authenticated> {
        assert!(transport.has_idle());
        std::cell::RefMut::map(transport.idle.borrow_mut(), |idle| {
            assert_eq!(idle.len(), 1);
            &mut idle[0].auth
        })
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
        assert!(transport.has_idle());
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
        assert!(!transport.has_idle());
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
        assert!(!transport.has_idle());
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
        let connection = testing::drive(transport.bootstrap(&scope)).unwrap();
        assert!(matches!(
            connection.authentication,
            Authentication::Bootstrap
        ));
        let first = testing::drive(connection.request(
            testing::request(Method::Get, "/snapshot", None, 1024),
            &scope,
        ))
        .unwrap();
        assert!(!transport.has_idle());
        let second = rotation_request(&transport, None).unwrap();
        assert_ne!(first.body, second);
        assert!(!transport.has_idle());
    }

    /// A checked-out request may finish on A but must never enter B's idle pool.
    #[test]
    fn replacing_owner_invalidates_checked_out_connection_recycling() {
        let d = testing::Directory::new();
        let (ca, key) = testing::ca();
        let identity = TestIdentity::new(&ca, &key);
        let response = || ("/snapshot".to_owned(), 200, b"{}".to_vec());
        let (endpoint, server) = scripted_server(
            &d,
            &ca,
            &key,
            vec![vec![response(), response()], vec![response(), response()]],
        );
        let transport = Transport::new(endpoint);
        let owner_a = Rc::new(FixtureIo::default());
        let owner_b = Rc::new(FixtureIo::default());
        transport.attach_io(owner_a.clone());
        let scope = testing::scope();
        let complete = |connection: Connection<FixtureIo>| {
            testing::drive(connection.request(
                testing::request(Method::Get, "/snapshot", None, 1024),
                &scope,
            ))
            .unwrap()
        };
        let connect = || testing::drive(transport.authenticated(&identity, &scope)).unwrap();
        complete(connect());
        let old = connect();
        assert!(Rc::ptr_eq(&old.io, &owner_a));
        transport.attach_io(owner_b.clone());
        assert_eq!(complete(old).status, 200);
        assert!(!transport.has_idle());
        for _ in 0..2 {
            let current = connect();
            assert!(Rc::ptr_eq(&current.io, &owner_b));
            assert_eq!(complete(current).status, 200);
            assert!(transport.has_idle());
        }
        transport.close_idle();
        server.join().unwrap();
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
        transport.attach_io(Rc::new(FixtureIo::default()));
        let scope = testing::scope();
        let get = || {
            testing::drive(async {
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
        let retire_at = idle_auth(&transport).retire_at;
        assert!(retire_at > Instant::now() + Duration::from_secs(239));
        assert!(retire_at <= Instant::now() + Duration::from_secs(300));
        assert_eq!(get().status, 204);
        assert_eq!(idle_auth(&transport).retire_at, retire_at);
        idle_auth(&transport).retire_at = Instant::now();
        // Checkout retires the first connection. Active I/O ignores its age.
        let mut connection = testing::drive(transport.authenticated(&identity, &scope)).unwrap();
        authenticated(&mut connection).retire_at = Instant::now();
        assert_eq!(connection.check(&scope), Ok(()));
        let expiry = authenticated(&mut connection).expires;
        authenticated(&mut connection).expires = SystemTime::now() - Duration::from_secs(1);
        assert_eq!(
            connection.check(&scope),
            Err(testing::Error::Transport(Error::Unauthorized))
        );
        authenticated(&mut connection).expires = expiry;
        let result = testing::drive(connection.request(
            testing::request(Method::Get, "/snapshot", None, 65536),
            &scope,
        ))
        .unwrap();
        assert_eq!(result.status, 204);
        assert!(!transport.has_idle());
        assert_eq!(get().status, 200);
        idle_auth(&transport).idle_since = Instant::now() - Duration::from_secs(20);
        assert_eq!(get().status, 200);
        idle_auth(&transport).epoch = [0; 32];
        for status in [429, 503] {
            assert_eq!(get().status, 200);
            assert_eq!(get().status, status);
            assert!(!transport.has_idle());
        }
        assert_eq!(get().status, 200);
        let connection = testing::drive(transport.authenticated(&identity, &scope)).unwrap();
        assert!(matches!(
            testing::drive(connection.request(
                testing::request(Method::Get, "invalid", None, 65536),
                &scope
            )),
            Err(testing::Error::Transport(Error::InvalidRequest))
        ));
        assert!(!transport.has_idle());
        assert_eq!(get().status, 200);
        let connection = testing::drive(transport.authenticated(&identity, &scope)).unwrap();
        assert!(matches!(
            testing::drive(connection.request(
                testing::request(Method::Get, "/snapshot", None, 65536),
                &scope
            )),
            Err(testing::Error::Transport(Error::Io))
        ));
        assert!(!transport.has_idle());
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
    ] { assert!(parse_head(bytes, 10, 65536, std::time::SystemTime::now()).is_err()); }
    }

    /// Maximum caller limits cannot permit overflowing frame sizes or body allocation.
    #[test]
    fn maximum_content_length_rejects_frame_overflow_without_body_allocation() {
        for status in [200, 429] {
            // Only a small header is allocated, never the attacker-specified body.
            let bytes = format!(
                "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n",
                usize::MAX
            );
            let head = parse_head(
                bytes.as_bytes(),
                usize::MAX,
                usize::MAX,
                std::time::SystemTime::now(),
            )
            .unwrap()
            .unwrap();
            assert_eq!(head.framing, small::Framing::Length(usize::MAX as u64));
            // The decoder accepts the declared length without reserving it.
            let decoder = small::Decoder::new(
                head.framing,
                small::Limits {
                    body: head.bound,
                    line: 128,
                    trailers: 4096,
                },
            )
            .unwrap();
            assert!(decoder.body().is_empty());
        }
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
        ] {
            let bytes = format!("HTTP/1.1 204 No Content\r\n{headers}Retry-After: 2\r\n\r\n");
            assert_eq!(
                parse_head(bytes.as_bytes(), 10, 20, std::time::SystemTime::now()).unwrap(),
                Some(ResponseHead {
                    status: 204,
                    header_len: bytes.len(),
                    framing: small::Framing::Length(0),
                    bound: 20,
                    retry_after: Some(Duration::from_secs(2)),
                    close,
                })
            );
        }
        assert_eq!(
            parse_head(
                b"HTTP/1.1 204 No Content\r\n",
                10,
                20,
                std::time::SystemTime::now()
            ),
            Ok(None)
        );
        // Connection options follow the shared token grammar.
        let bytes = "HTTP/1.1 204 No Content\r\nConnection: \u{a0}close\u{a0}\r\n\r\n";
        assert_eq!(
            parse_head(bytes.as_bytes(), 10, 20, std::time::SystemTime::now()),
            Err(Error::InvalidRequest)
        );
    }

    /// Ignored non-UTF-8 headers remain invalid; non-UTF-8 bodies are opaque.
    #[test]
    fn response_head_preserves_utf8_acceptance_and_error_precedence() {
        // Size errors precede the UTF-8 check for ignored fields; the shared
        // head grammar rejects an invalid Connection token list up front.
        for (name, oversized) in [
            ("X-Ignored", Error::Overloaded),
            ("Connection", Error::InvalidRequest),
        ] {
            let mut bytes = format!("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 12\r\n{name}: ").into_bytes();
            bytes.extend_from_slice(b"\xff\r\n\r\n");
            assert_eq!(
                parse_head(&bytes, 10, 20, std::time::SystemTime::now()),
                Err(oversized)
            );
            assert_eq!(
                parse_head(&bytes, 12, 20, std::time::SystemTime::now()),
                Err(Error::InvalidRequest)
            );
        }
        let bytes = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n\xff\xff";
        assert!(parse_head(bytes, 2, 20, std::time::SystemTime::now()).is_ok());
        assert_eq!(parse_head(b"HTTP/1.1 429 Busy\r\nContent-Type: application/json\r\nContent-Length: 12\r\n\r\n", 10, 20, std::time::SystemTime::now()).unwrap().unwrap().framing, small::Framing::Length(12));
    }

    /// Chunked framing retains its status-specific bound, including zero limits.
    #[test]
    fn chunked_framing_carries_the_validated_body_limit() {
        for (status, limit, max_error_body, bound) in [
            (200, 10, 20, 10),
            (429, 10, 20, 20),
            (503, 10, 0, 0),
            (200, 0, 20, 0),
        ] {
            let bytes = format!(
                "HTTP/1.1 {status} Result\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
            );
            assert_eq!(
                parse_head(
                    bytes.as_bytes(),
                    limit,
                    max_error_body,
                    std::time::SystemTime::now()
                )
                .unwrap()
                .map(|head| (head.framing, head.bound)),
                Some((small::Framing::Chunked, bound))
            );
        }
        assert_eq!(
            parse_head(
                b"HTTP/1.1 204 Empty\r\nTransfer-Encoding: chunked\r\n\r\n",
                10,
                20,
                std::time::SystemTime::now()
            ),
            Err(Error::InvalidRequest)
        );
    }

    /// Pool epochs bind trust and certificate boundaries, not borrowed key bytes.
    #[test]
    fn trust_epochs_preserve_certificate_boundaries() {
        let chain = vec![b"ab".to_vec(), b"c".to_vec()];
        let regrouped = vec![b"a".to_vec(), b"bc".to_vec()];
        let identity = Identity {
            certificate_chain: &chain,
            private_key: b"key",
            expires: std::time::UNIX_EPOCH,
        };
        let epoch = trust_epoch(b"trust", Some(identity));
        assert_eq!(epoch, trust_epoch(b"trust", Some(identity)));
        assert_ne!(epoch, trust_epoch(b"changed", Some(identity)));
        assert_ne!(epoch, trust_epoch(b"trust", None));
        assert_ne!(
            epoch,
            trust_epoch(
                b"trust",
                Some(Identity {
                    certificate_chain: &regrouped,
                    ..identity
                })
            )
        );
        assert_eq!(
            epoch,
            trust_epoch(
                b"trust",
                Some(Identity {
                    private_key: b"different key",
                    ..identity
                })
            )
        );
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

    /// Invalid syntax and transport-owned headers cannot be injected.
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

    /// Header sizes are rejected before credentials enter an allocation.
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
