// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local HTTP/1.1 GET/HEAD over filesystem Unix sockets and peer TLS.
//!
//! Poll exchanges from [`crate::uring::Application::poll`], merge their pending
//! [`Work`] (OR `runnable`, earliest deadline), and let the ring driver do the I/O
//! and sleeping. A deadline covers connecting, queue pressure, and all transfers.
//! Responses expose all final status codes; routing, retries, validation and cache
//! publication belong to the caller. Only Content-Length framing is supported.
//!
//! Each connection reuses one 8 KiB allocation. Only a body prefix received with
//! headers is copied; subsequent receives go directly into the pool's [`Fill`]
//! or restricted [`Destination`]. GET returns the same capability it receives.
//! Responses must be consumed to recover a reusable connection. Dropping an
//! unfinished exchange shuts down its socket; the ring retains in-flight storage
//! until the real terminal completion, including after cancellation.
//! Header storage is bounded to 8 KiB total (including informational responses),
//! 64 fields per response, and eight informational responses. GET bodies are
//! limited to one 64 MiB pool buffer; HEAD/304 metadata may describe larger objects.
//!
//! ```no_run
//! use racer_dataplane::{buffers::Fill, http_client::*, uring::{Ring, Work}};
//! use std::time::Instant;
//! fn start(c: Connection, fill: Fill, deadline: Instant) -> std::io::Result<GetExchange> {
//!     c.get(Request::new("/object", &[("Range", "bytes=0-4194303")])?, fill, deadline)
//! }
//! // Within Application::poll; retain the exchange between turns.
//! fn turn(exchange: &mut GetExchange, ring: &mut Ring) -> std::io::Result<Work> {
//!     match exchange.poll(ring, 32)? {
//!         Progress::Pending(work) => Ok(work),
//!         Progress::Ready(response) => {
//!             let (connection, fill, len) = response.recycle();
//!             let buffer = fill.publish(len)?; // after application validation
//!             // Store connection for reuse, deliver buffer, remove exchange.
//!             Ok(Work::default())
//!         }
//!     }
//! }
//! ```
//!
//! A connection cannot issue overlapping requests:
//! ```compile_fail
//! use racer_dataplane::http_client::{Connection, Request};
//! use std::time::Instant;
//! fn overlap(c: Connection, r: Request<'_>, deadline: Instant) {
//!     let first = c.head(r, deadline);
//!     let second = c.head(r, deadline);
//! }
//! ```
//! GET requires exclusive pool storage:
//! ```compile_fail
//! use racer_dataplane::http_client::{Connection, Request};
//! fn missing(c: Connection, r: Request<'_>) {
//!     c.get(r, std::time::Instant::now());
//! }
//! ```
//! Connections are worker-local:
//! ```compile_fail
//! use racer_dataplane::http_client::Connection;
//! fn transfer(c: Connection) { std::thread::spawn(move || drop(c)); }
//! ```
//! Header views cannot survive scratch recycling:
//! ```compile_fail
//! use racer_dataplane::http_client::HeadResponse;
//! fn recycle(r: HeadResponse) {
//!     let value = r.headers().get("etag");
//!     let connection = r.recycle();
//!     println!("{value:?}");
//! }
//! ```

use crate::buffers::{BUFFER_SIZE, Destination, Fill, Writable};
use crate::http::{
    Header, MAX_HEADERS, SCRATCH_SIZE, authority, connection_close, decimal, field, header_end,
    invalid, line, protocol, token, transfer, value,
};
pub use crate::http::{Headers, Progress};
use crate::socket::Address;
use crate::uring::{
    BufferRange, Bytes, Control, File, FixedFile, Identity, Read, Ring, Ticket, Work,
};
use std::io;
use std::net::SocketAddr;
#[cfg(test)]
use std::os::fd::AsFd;
use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};
use std::rc::Rc;
use std::time::Instant;

const MAX_INFORMATIONAL: usize = 8;
const ADMISSION_TIMEOUT: std::time::Duration = std::time::Duration::from_millis(320);

pub use crate::tls::TlsChannel;

pub(crate) mod endpoint {
    //! Numeric destinations only. Parsing never performs name resolution.
    use crate::http::protocol;
    use crate::socket::Address;
    use std::{io, net::SocketAddr};

    #[derive(Clone)]
    pub struct Endpoint {
        pub(crate) address: Address,
        pub(crate) host: String,
    }

    impl Endpoint {
        pub fn parse(raw: &str) -> io::Result<Self> {
            if raw.len() > 64 || raw.contains('%') {
                return Err(protocol("invalid numeric endpoint"));
            }
            let address: SocketAddr = raw
                .parse()
                .map_err(|_| protocol("expected IPv4:port or [IPv6]:port"))?;
            if address.port() == 0 {
                return Err(protocol("endpoint port must be nonzero"));
            }
            let host = address.to_string();
            drop(super::Connection::new(address, &host)?);
            Ok(Self {
                address: address.into(),
                host,
            })
        }
        pub fn unix(path: &str) -> io::Result<Self> {
            Ok(Self {
                address: Address::unix(path)?,
                host: "localhost".into(),
            })
        }
        pub fn address(&self) -> Address {
            self.address
        }
        pub fn host(&self) -> &str {
            &self.host
        }
    }
}
pub use endpoint::Endpoint;

/// Bounded idle connections and admission for one setup-resolved HTTP endpoint.
pub(crate) struct Origin {
    tls: Option<(
        std::sync::Arc<crate::control::credentials::Provider>,
        crate::tls::PeerIdentity,
    )>,
    peer: bool,
    pub(crate) endpoint: Endpoint,
    idle: Vec<Connection>,
    max_idle_age: Option<std::time::Duration>,
    pub(crate) breaker: crate::breaker::CircuitBreaker,
    pub(crate) limit: usize,
}
impl Origin {
    pub(crate) fn error(permit: crate::breaker::Permit, error: &crate::cache::Error, peer: bool) {
        if matches!(
            error.evidence().reason(),
            crate::outcome::PeerReason::Unauthorized | crate::outcome::PeerReason::Forbidden
        ) {
            permit.success();
            return;
        }
        if error.evidence().neutral_for_health() {
            drop(permit);
            return;
        }
        let healthy_status = error.healthy_http_status();

        if !peer && healthy_status {
            permit.success();
        } else {
            permit.failure();
        }
    }
    pub(crate) fn new(endpoint: Endpoint) -> Self {
        Self {
            tls: None,
            peer: false,
            endpoint,
            idle: Vec::new(),
            max_idle_age: None,
            breaker: crate::breaker::CircuitBreaker::new(std::time::Duration::from_secs(1)),
            limit: 16,
        }
    }
    pub(crate) fn peer(endpoint: Endpoint) -> Self {
        Self {
            peer: true,
            // Peer servers close idle connections at 30s. Age from the start of
            // the last exchange, not recycling: response/validation time must
            // not make an old server-side idle socket appear young locally.
            max_idle_age: Some(std::time::Duration::from_secs(20)),
            ..Self::new(endpoint)
        }
    }
    pub(crate) fn connection(
        &mut self,
    ) -> crate::cache::Result<(Connection, crate::breaker::Permit)> {
        let rejected = |cause, message: &str| {
            crate::cache::Error::from(crate::outcome::Failure {
                endpoint: self.endpoint.address,
                transport: crate::outcome::Transport::Http,
                phase: crate::outcome::Phase::LocalAdmission,
                cause,
                initiated: false,
                kind: io::ErrorKind::WouldBlock,
                message: message.into(),
            })
        };
        if self.breaker.active() >= self.limit {
            return Err(rejected(
                crate::outcome::Cause::LocalPressure,
                "direct endpoint exchange limit",
            ));
        }
        // Breaker rejection is not fresh evidence of a failed connection.
        let permit = self.breaker.try_acquire().map_err(|_| {
            rejected(
                crate::outcome::Cause::BreakerRejected,
                "direct endpoint breaker rejected",
            )
        })?;
        if let Some(max_age) = self.max_idle_age {
            let now = crate::environment::now();
            self.idle
                .retain(|c| now.saturating_duration_since(c.socket.started) < max_age);
        }
        self.maintain();
        let connection = self.idle.pop().map(Ok).unwrap_or_else(|| {
            if let Some((provider, identity)) = &self.tls {
                let snapshot = provider.current();
                let mut connection = Connection::new_tls(
                    self.endpoint
                        .address
                        .tcp()
                        .ok_or_else(|| invalid("peer TLS requires TCP"))?,
                    &self.endpoint.host,
                    &snapshot.context,
                    crate::tls::ExpectedPeer::Identity(identity.clone()),
                )?;
                connection.set_tls_revision(snapshot.revision, snapshot.expires_unix);
                return Ok(connection);
            }
            if self.peer {
                return Err(io::Error::new(
                    io::ErrorKind::PermissionDenied,
                    "peer TLS credentials required",
                ));
            }
            Connection::new_address(self.endpoint.address, &self.endpoint.host)
        })?;
        Ok((connection, permit))
    }
    pub(crate) fn set_tls(
        &mut self,
        provider: std::sync::Arc<crate::control::credentials::Provider>,
        identity: crate::tls::PeerIdentity,
    ) {
        self.tls = Some((provider, identity));
    }
    pub(crate) fn maintain(&mut self) {
        self.idle.retain(|c| !c.socket.tls_expired());
        if let Some((provider, _)) = &self.tls {
            let revision = provider.current().revision;
            self.idle
                .retain(|c| c.socket.credential_revision == revision);
        }
    }
    pub(crate) fn recycle(&mut self, connection: Option<Connection>) {
        if self.idle.len() < 16 {
            self.idle.extend(connection);
        }
    }
    pub(crate) fn retire_idle(&mut self) {
        self.idle.clear();
    }
}

/// Sparse destination health, distinct from the immediate transport breaker.
/// Live permits fence eviction and stale completions. Saturation fails locally.
pub(crate) mod owner_health {
    use std::{
        cell::RefCell,
        collections::BTreeMap,
        io,
        rc::Rc,
        time::{Duration, Instant},
    };

    const COOLDOWN: Duration = Duration::from_secs(1);
    #[derive(Default)]
    pub(crate) struct Owners(BTreeMap<([u8; 32], Key), OwnerHealth>);
    #[derive(Clone, Eq, PartialEq, Ord, PartialOrd)]
    enum Key {
        Slot(u32),
        Physical(String),
    }
    struct OwnerHealth {
        breaker: crate::breaker::CircuitBreaker,
        observed: Rc<RefCell<Option<Observation>>>,
    }
    #[derive(Clone, Copy)]
    struct Observation {
        at: Instant,
        indirect: bool,
    }
    impl OwnerHealth {
        fn new() -> Self {
            Self {
                breaker: crate::breaker::CircuitBreaker::new(COOLDOWN),
                observed: Rc::new(RefCell::new(None)),
            }
        }
        fn expired_indirect(&self) -> bool {
            self.observed.borrow().is_some_and(|observation| {
                observation.indirect && observation.at + COOLDOWN <= crate::environment::now()
            })
        }
    }
    pub(crate) struct OwnerPermit {
        permit: crate::breaker::Permit,
        observed: Rc<RefCell<Option<Observation>>>,
        final_hop: bool,
        physical: Option<Box<OwnerPermit>>,
    }
    impl OwnerPermit {
        pub(crate) fn success(self) {
            if let Some(physical) = self.physical {
                physical.success();
            }
            if self.permit.current() {
                self.observed.replace(None);
            }
            self.permit.success();
        }
        pub(crate) fn failure(self, observed: Instant) {
            if self.permit.current() {
                self.observed.replace(Some(Observation {
                    at: observed,
                    indirect: !self.final_hop,
                }));
            }
            self.permit.failure();
        }
        /// Only an initiated qualifying failure of a proven physical-final
        /// transport may publish cross-slot evidence. Semantic reports remain
        /// slot scoped, even when a final peer reports downstream unavailability.
        pub(crate) fn transport_failure(mut self, observed: Instant) {
            if let Some(physical) = self.physical.take() {
                physical.failure(observed);
            }
            self.failure(observed);
        }
    }
    impl Owners {
        pub(crate) fn blocked(&self, identity: [u8; 32], slot: u32) -> bool {
            self.0
                .get(&(identity, Key::Slot(slot)))
                .is_some_and(|b| !b.expired_indirect() && !b.breaker.available())
        }
        pub(crate) fn evidence(&self, identity: [u8; 32], slot: u32) -> Option<Instant> {
            self.0
                .get(&(identity, Key::Slot(slot)))?
                .observed
                .borrow()
                .map(|observation| observation.at)
                .filter(|at| *at + COOLDOWN > crate::environment::now())
        }
        #[cfg(test)]
        pub(crate) fn is_empty(&self) -> bool {
            self.0.is_empty()
        }

        pub(crate) fn physical_evidence(&self, identity: [u8; 32], peer: &str) -> Option<Instant> {
            self.0
                .get(&(identity, Key::Physical(peer.into())))?
                .observed
                .borrow()
                .map(|observation| observation.at)
                .filter(|at| *at + COOLDOWN > crate::environment::now())
        }
        pub(crate) fn acquire_final(
            &mut self,
            identity: [u8; 32],
            slot: u32,
            peer: Option<&str>,
        ) -> io::Result<OwnerPermit> {
            let mut owner = self.acquire_key(identity, Key::Slot(slot), peer.is_some())?;
            if let Some(peer) = peer {
                owner.physical = Some(Box::new(self.acquire_key(
                    identity,
                    Key::Physical(peer.into()),
                    true,
                )?));
            }
            Ok(owner)
        }
        fn acquire_key(
            &mut self,
            identity: [u8; 32],
            owner: Key,
            final_hop: bool,
        ) -> io::Result<OwnerPermit> {
            let key = (identity, owner);
            if let Some(health) = self.0.get_mut(&key)
                && health.expired_indirect()
            {
                // A relay hit cannot complete an owner probe. Indirect reports
                // suppress only for their evidence lifetime, then permit recovery.
                // Detach both state objects so old permits cannot alter the new
                // generation. This expiry does not assert owner reachability.
                *health = OwnerHealth::new();
            }
            if !self.0.contains_key(&key) {
                if self.0.len() >= 4096 {
                    let victim = self
                        .0
                        .iter()
                        .find(|(_, b)| b.breaker.evictable())
                        .map(|(k, _)| k.clone());
                    if let Some(victim) = victim {
                        self.0.remove(&victim);
                    } else {
                        return Err(io::ErrorKind::WouldBlock.into());
                    }
                }
                self.0.insert(key.clone(), OwnerHealth::new());
            }
            let health = &self.0[&key];
            Ok(OwnerPermit {
                permit: health
                    .breaker
                    .try_acquire()
                    .map_err(|_| io::Error::from(io::ErrorKind::WouldBlock))?,
                observed: health.observed.clone(),
                final_hop,
                physical: None,
            })
        }
    }

    #[cfg(test)]
    mod tests {
        include!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/http/owner_health.rs"
        ));
    }
}

/// Compatibility path for transport-neutral attempt outcomes.
pub use crate::outcome as attempt;

/// Validated origin-form target and additional headers, borrowed only until start.
/// Host and all request framing/connection-control headers are transport-owned.
#[derive(Clone, Copy)]
pub struct Request<'a> {
    target: &'a str,
    headers: &'a [(&'a str, &'a str)],
    authorization: Option<&'a str>,
}
impl<'a> Request<'a> {
    pub fn new(target: &'a str, headers: &'a [(&'a str, &'a str)]) -> io::Result<Self> {
        if !crate::http::target(target.as_bytes()) {
            return Err(invalid("invalid origin-form request target"));
        }
        let mut authorization = None;
        for &(name, v) in headers {
            if name.eq_ignore_ascii_case("authorization") {
                if authorization.replace(v).is_some() {
                    return Err(invalid("duplicate Authorization"));
                }
                crate::authorization::Authorization::new(v)?;
            }
            if name.is_empty() || !name.bytes().all(token) || !value(v.as_bytes()) {
                return Err(invalid("invalid request header"));
            }
            if [
                "host",
                "content-length",
                "transfer-encoding",
                "connection",
                "proxy-connection",
                "keep-alive",
                "upgrade",
                "expect",
                "trailer",
                "te",
            ]
            .iter()
            .any(|n| name.eq_ignore_ascii_case(n))
            {
                return Err(invalid("transport-owned request header"));
            }
        }
        Ok(Self {
            target,
            headers,
            authorization: None,
        })
    }

    pub(crate) fn with_authorization(
        mut self,
        auth: &'a crate::authorization::Authorization,
    ) -> Self {
        self.authorization = auth.as_str();
        self
    }

    fn capacity(&self, head: bool, host: &str) -> io::Result<usize> {
        let mut normal = (if head { 5 } else { 4 }) + self.target.len() + 17 + host.len() + 4;
        let mut auth = self.authorization;
        for &(name, value) in self.headers {
            if name.eq_ignore_ascii_case("authorization") {
                if auth.replace(value).is_some() {
                    return Err(invalid("duplicate Authorization"));
                }
            } else {
                normal += name.len() + value.len() + 4;
            }
        }
        if normal > SCRATCH_SIZE {
            return Err(invalid("normal request headers exceed 8 KiB"));
        }
        Ok(
            (normal + auth.map_or(0, |v| v.len() + crate::authorization::HTTP_AUTH_OVERHEAD))
                .max(SCRATCH_SIZE),
        )
    }

    fn encode(self, head: bool, host: &str, mut out: &mut [u8]) -> io::Result<usize> {
        use std::io::Write;
        let capacity = out.len();
        let result = (|| {
            out.write_all(if head { b"HEAD " } else { b"GET " })?;
            out.write_all(self.target.as_bytes())?;
            out.write_all(b" HTTP/1.1\r\nHost: ")?;
            out.write_all(host.as_bytes())?;
            out.write_all(b"\r\n")?;
            if let Some(auth) = self.authorization {
                out.write_all(b"Authorization: ")?;
                out.write_all(auth.as_bytes())?;
                out.write_all(b"\r\n")?;
            }
            for &(name, value) in self.headers {
                out.write_all(name.as_bytes())?;
                out.write_all(b": ")?;
                out.write_all(value.as_bytes())?;
                out.write_all(b"\r\n")?;
            }
            out.write_all(b"\r\n")
        })();
        result.map_err(|_| invalid("request exceeds 8 KiB"))?;
        Ok(capacity - out.len())
    }
}

enum Transport {
    New(Address),
    // TCP remains connected while idle; registration is an active-exchange lease.
    Idle(Rc<Identity>),
    Connected(FixedFile, Rc<Identity>),
}
struct PendingTls {
    context: crate::tls::TlsContext,
    expected: crate::tls::ExpectedPeer,
    expires_unix: u64,
}
struct Socket {
    credential_revision: u64,
    transferred: bool,
    tls: Option<TlsChannel>,
    pending_tls: Option<PendingTls>,
    endpoint: Address,
    started: Instant,
    file: File,
    transport: Transport,
    host: Box<str>,
}
impl Socket {
    fn tls_expired(&self) -> bool {
        self.tls.as_ref().is_some_and(TlsChannel::expired)
    }
}
impl Drop for Socket {
    fn drop(&mut self) {
        if !self.transferred {
            self.file.shutdown_socket();
        }
    }
}

/// Idle, uniquely owned connection. Construction does no network I/O; the first
/// exchange connects. Reuse only on the same worker and ring.
#[must_use]
pub struct Connection {
    socket: Socket,
    scratch: Box<[u8]>,
}
impl Connection {
    pub fn new(address: SocketAddr, host: &str) -> io::Result<Self> {
        Self::new_address(address.into(), host)
    }

    pub fn new_address(address: Address, host: &str) -> io::Result<Self> {
        if !authority(host.as_bytes()) {
            return Err(invalid("invalid Host authority"));
        }

        // SAFETY: socket creates a new descriptor; no borrowed pointers.
        let fd =
            unsafe { libc::socket(address.domain(), libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        // SAFETY: successful socket result is uniquely owned.
        let fd = unsafe { OwnedFd::from_raw_fd(fd) };
        let one: libc::c_int = 1;
        // SAFETY: correctly sized and aligned option and live socket.
        if address.tcp().is_some()
            && unsafe {
                libc::setsockopt(
                    fd.as_raw_fd(),
                    libc::IPPROTO_TCP,
                    libc::TCP_NODELAY,
                    (&one as *const libc::c_int).cast(),
                    size_of_val(&one) as libc::socklen_t,
                )
            } < 0
        {
            return Err(io::Error::last_os_error());
        }
        Ok(Self {
            socket: Socket {
                credential_revision: 0,
                transferred: false,
                tls: None,
                pending_tls: None,
                endpoint: address,
                started: crate::environment::now(),
                file: File::new(fd),
                transport: Transport::New(address),
                host: host.into(),
            },
            scratch: vec![0; SCRATCH_SIZE].into_boxed_slice(),
        })
    }

    pub fn new_tls(
        address: SocketAddr,
        host: &str,
        context: &crate::tls::TlsContext,
        expected: crate::tls::ExpectedPeer,
    ) -> io::Result<Self> {
        let mut connection = Self::new(address, host)?;
        // OpenSSL determines socket BIO kTLS eligibility when attaching the FD.
        // Capture the authority now, but attach only after TCP connect completes.
        connection.socket.pending_tls = Some(PendingTls {
            context: context.clone(),
            expected,
            expires_unix: u64::MAX,
        });
        Ok(connection)
    }
    pub fn peer_identity(&self) -> Option<crate::tls::PeerIdentity> {
        self.socket.tls.as_ref()?.peer_identity().cloned()
    }
    pub fn into_tls_channel(mut self) -> io::Result<TlsChannel> {
        let channel = self
            .socket
            .tls
            .take()
            .ok_or_else(|| invalid("connection is not TLS"))?;
        // Socket drop normally shuts down the shared descriptor. Transfer ownership.
        self.socket.transferred = true;
        Ok(channel)
    }
    pub fn set_tls_revision(&mut self, revision: u64, expires_unix: u64) {
        self.socket.credential_revision = revision;

        if let Some(tls) = &mut self.socket.pending_tls {
            tls.expires_unix = expires_unix;
        }
        if let Some(tls) = &mut self.socket.tls {
            tls.set_revision(revision);
            tls.set_expiry(expires_unix);
        }
    }
    /// Start GET with raw Fill or a restricted Destination. The exchange and
    /// response preserve B, including on completion; no publication is performed.
    pub fn get<B: Writable>(
        self,
        request: Request<'_>,
        fill: B,
        deadline: Instant,
    ) -> io::Result<GetExchange<B>> {
        Ok(GetExchange(Exchange::new(
            self,
            request,
            Body::Get(fill),
            deadline,
        )?))
    }
    pub fn head(self, request: Request<'_>, deadline: Instant) -> io::Result<HeadExchange> {
        Ok(HeadExchange(Exchange::new(
            self,
            request,
            Body::Head,
            deadline,
        )?))
    }
    /// Bounded control-plane-sized GET body, independent of the payload pool.
    pub fn get_small(
        self,
        request: Request<'_>,
        capacity: usize,
        deadline: Instant,
    ) -> io::Result<SmallExchange> {
        if capacity == 0 || capacity > SCRATCH_SIZE {
            return Err(protocol("invalid small body capacity"));
        }
        Ok(SmallExchange(Exchange::new(
            self,
            request,
            Body::Small(vec![0; capacity].into_boxed_slice()),
            deadline,
        )?))
    }
}

struct Metadata {
    status: u16,
    length: Option<u64>,
    close: bool,
    headers: [Header; MAX_HEADERS],
    count: usize,
}

struct Response {
    socket: Option<Socket>,
    scratch: Box<[u8]>,
    metadata: Metadata,
}
impl Response {
    fn recycle(self) -> Option<Connection> {
        self.socket.map(|mut socket| {
            if let Transport::Connected(_, identity) = &socket.transport {
                // Complete responses have consumed every client IO ticket. Any
                // remaining ring-owned operation independently pins its handle.
                socket.transport = Transport::Idle(identity.clone());
            }
            Connection {
                socket,
                scratch: self.scratch,
            }
        })
    }
    fn headers(&self) -> Headers<'_> {
        Headers {
            bytes: &self.scratch,
            headers: &self.metadata.headers[..self.metadata.count],
        }
    }
}

/// Completed GET; storage retains exactly the submitted capability.
///
/// ```compile_fail
/// use racer_dataplane::{buffers::Destination, http_client::GetResponse};
/// fn publish(response: GetResponse<Destination>) {
///     let (_, destination, len) = response.recycle();
///     destination.publish(len);
/// }
/// ```
#[must_use]
pub struct GetResponse<B: Writable = Fill> {
    response: Response,
    fill: B,
    len: usize,
}
impl<B: Writable> GetResponse<B> {
    pub fn status(&self) -> u16 {
        self.response.metadata.status
    }
    pub fn content_length(&self) -> Option<u64> {
        self.response.metadata.length
    }
    pub fn headers(&self) -> Headers<'_> {
        self.response.headers()
    }
    pub fn body(&mut self) -> &[u8] {
        &self.fill.as_mut_slice()[..self.len]
    }
    /// Returns the reusable connection (if allowed), unpublished storage and body length.
    pub fn recycle(self) -> (Option<Connection>, B, usize) {
        (self.response.recycle(), self.fill, self.len)
    }
}
#[must_use]
pub struct HeadResponse {
    response: Response,
}
impl HeadResponse {
    pub fn status(&self) -> u16 {
        self.response.metadata.status
    }
    pub fn content_length(&self) -> Option<u64> {
        self.response.metadata.length
    }
    pub fn headers(&self) -> Headers<'_> {
        self.response.headers()
    }
    pub fn recycle(self) -> Option<Connection> {
        self.response.recycle()
    }
}

enum Body<B: Writable = Fill> {
    Head,
    Get(B),
    Small(Box<[u8]>),
}
struct Payload<B: Writable = Fill> {
    scratch: Box<[u8]>,
    body: Body<B>,
}
#[derive(Default)]
struct Cursor {
    used: usize,
    scan: usize,
    start: usize,
    informational: usize,
}
enum State<B: Writable = Fill> {
    Connect(Payload<B>),
    Connecting(Payload<B>, Ticket<Control>),
    Register(Payload<B>),
    Handshake(Payload<B>),
    Send(Payload<B>, usize),
    Sending(Body<B>, Ticket<Bytes>, usize),
    Headers(Payload<B>, Cursor),
    ReceivingHeaders(Body<B>, Ticket<Bytes>, Cursor),
    Body(Box<[u8]>, Metadata, B, usize, usize),
    ReceivingBody(Box<[u8]>, Metadata, Ticket<Read<B>>, usize, usize),
    SmallBody(Box<[u8]>, Metadata, Box<[u8]>, usize, usize),
    ReceivingSmall(Box<[u8]>, Metadata, Ticket<Bytes>, usize, usize),
    Complete(Payload<B>, Metadata, usize),
    Finished,
}
struct Exchange<B: Writable = Fill> {
    reused: bool,
    stale_payload: Option<Payload<B>>,
    socket: Option<Socket>,
    state: State<B>,
    request_len: usize,
    deadline: Instant,
    ring: Option<Rc<Identity>>,
    phase: crate::outcome::Phase,
    initiated: bool,
    local_pressure: bool,
    pressure_since: Option<Instant>,
    service_deadline: bool,
    connect_end: Option<Instant>,
    retry_request: Option<Box<[u8]>>,
    retry_metrics: Option<crate::metrics::Local>,
    retry_peer: Option<(
        std::sync::Arc<crate::control::credentials::Provider>,
        crate::tls::PeerIdentity,
    )>,
}
struct Completed<B: Writable = Fill> {
    response: Response,
    body: Body<B>,
    len: usize,
}

#[must_use]
pub struct GetExchange<B: Writable = Fill>(Exchange<B>);
/// GET exchange carrying writable storage without cache publication authority.
pub type DestinationGetExchange = GetExchange<Destination>;
/// Completed restricted GET, whose recycle method returns Destination.
pub type DestinationGetResponse = GetResponse<Destination>;
#[must_use]
pub struct HeadExchange(Exchange);
pub struct SmallExchange(Exchange);
pub struct SmallResponse {
    response: Response,
    bytes: Box<[u8]>,
    len: usize,
}
impl SmallResponse {
    pub fn status(&self) -> u16 {
        self.response.metadata.status
    }
    pub fn content_length(&self) -> Option<u64> {
        self.response.metadata.length
    }
    pub fn headers(&self) -> Headers<'_> {
        self.response.headers()
    }
    pub fn body(&self) -> &[u8] {
        &self.bytes[..self.len]
    }
    pub fn recycle(self) -> Option<Connection> {
        self.response.recycle()
    }
}
impl SmallExchange {
    pub(crate) fn service_deadline(mut self, service: bool) -> Self {
        self.0.service_deadline = service;
        self
    }
    pub(crate) fn connect_cap(mut self, duration: std::time::Duration) -> Self {
        self.0.connect_end = Some((crate::environment::now() + duration).min(self.0.deadline));
        self
    }
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<SmallResponse>> {
        self.poll_typed(ring, budget)
            .map_err(crate::cache::Error::into_io)
    }
    pub(crate) fn poll_typed(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> crate::cache::Result<Progress<SmallResponse>> {
        Ok(match self.0.poll(ring, budget)? {
            Progress::Pending(work) => Progress::Pending(work),
            Progress::Ready(c) => {
                let Body::Small(bytes) = c.body else {
                    unreachable!()
                };
                Progress::Ready(SmallResponse {
                    response: c.response,
                    bytes,
                    len: c.len,
                })
            }
        })
    }
}
impl<B: Writable> GetExchange<B> {
    #[cfg(test)]
    pub(crate) fn deadlines_for_test(&self) -> (Instant, Option<Instant>) {
        (self.0.deadline, self.0.connect_end)
    }
    /// True only when this deadline is a transport service window strictly inside
    /// the caller's deadline. The default deadline belongs to the caller.
    pub fn service_deadline(mut self, service: bool) -> Self {
        self.0.service_deadline = service;
        self
    }
    /// A connect cap starts with the exchange, not with each submission retry.
    /// Header/body progress retains the original whole-exchange deadline.
    pub(crate) fn connect_cap(mut self, duration: std::time::Duration) -> Self {
        self.0.connect_end = Some((crate::environment::now() + duration).min(self.0.deadline));
        self
    }
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<GetResponse<B>>> {
        self.poll_typed(ring, budget)
            .map_err(crate::cache::Error::into_io)
    }
    pub(crate) fn poll_typed(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> crate::cache::Result<Progress<GetResponse<B>>> {
        Ok(match self.0.poll(ring, budget)? {
            Progress::Pending(work) => Progress::Pending(work),
            Progress::Ready(c) => {
                let Body::Get(fill) = c.body else {
                    unreachable!()
                };
                Progress::Ready(GetResponse {
                    response: c.response,
                    fill,
                    len: c.len,
                })
            }
        })
    }
    /// Retires the exchange immediately. Storage remains ring-owned until terminal CQEs.
    pub fn cancel(self, ring: &mut Ring) -> io::Result<()> {
        self.0.cancel(ring)
    }
}
impl HeadExchange {
    /// Marks a transport service window strictly inside the caller's deadline.
    pub(crate) fn service_deadline(mut self, service: bool) -> Self {
        self.0.service_deadline = service;
        self
    }
    pub fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<HeadResponse>> {
        self.poll_typed(ring, budget)
            .map_err(crate::cache::Error::into_io)
    }
    pub(crate) fn poll_typed(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> crate::cache::Result<Progress<HeadResponse>> {
        Ok(match self.0.poll(ring, budget)? {
            Progress::Pending(work) => Progress::Pending(work),
            Progress::Ready(c) => Progress::Ready(HeadResponse {
                response: c.response,
            }),
        })
    }
    pub fn cancel(self, ring: &mut Ring) -> io::Result<()> {
        self.0.cancel(ring)
    }
}

impl<B: Writable> Exchange<B> {
    fn deadline(&self) -> Instant {
        if matches!(
            self.state,
            State::Connect(_) | State::Connecting(..) | State::Handshake(_)
        ) {
            self.connect_end.unwrap_or(self.deadline).min(self.deadline)
        } else {
            self.deadline
        }
    }
    fn record_phase(&mut self) {
        use crate::outcome::Phase;
        self.phase = match &self.state {
            State::Connect(_) | State::Connecting(..) | State::Handshake(_) => Phase::Connect,
            State::Send(..) | State::Sending(..) => Phase::Send,
            State::Headers(..) | State::ReceivingHeaders(..) => Phase::Headers,
            State::Body(..)
            | State::ReceivingBody(..)
            | State::SmallBody(..)
            | State::ReceivingSmall(..)
            | State::Complete(..) => Phase::Body,
            _ => Phase::LocalAdmission,
        };
        self.initiated = matches!(
            self.state,
            State::Connecting(..)
                | State::Sending(..)
                | State::ReceivingHeaders(..)
                | State::ReceivingBody(..)
                | State::ReceivingSmall(..)
        ) || (self
            .socket
            .as_ref()
            .is_some_and(|socket| socket.tls.is_some())
            && matches!(
                self.state,
                State::Handshake(_)
                    | State::Send(..)
                    | State::Headers(..)
                    | State::Body(..)
                    | State::SmallBody(..)
            ));
    }
    fn new(
        mut connection: Connection,
        request: Request<'_>,
        body: Body<B>,
        deadline: Instant,
    ) -> io::Result<Self> {
        if connection
            .socket
            .tls
            .as_ref()
            .is_some_and(TlsChannel::expired)
        {
            return Err(io::ErrorKind::ConnectionAborted.into());
        }

        let capacity = request.capacity(matches!(body, Body::Head), &connection.socket.host)?;
        if connection.scratch.len() != capacity {
            connection.scratch = vec![0; capacity].into_boxed_slice();
        }
        let request_len = request.encode(
            matches!(body, Body::Head),
            &connection.socket.host,
            &mut connection.scratch,
        )?;
        let payload = Payload {
            scratch: connection.scratch,
            body,
        };
        let state = match connection.socket.transport {
            Transport::New(_) => State::Connect(payload),
            Transport::Idle(_) => State::Register(payload),
            Transport::Connected(..) => State::Send(payload, 0),
        };
        connection.socket.started = crate::environment::now();
        Ok(Self {
            reused: matches!(connection.socket.transport, Transport::Idle(_)),
            stale_payload: None,
            socket: Some(connection.socket),
            state,
            request_len,
            deadline,
            ring: None,
            phase: crate::outcome::Phase::LocalAdmission,
            initiated: false,
            local_pressure: false,
            pressure_since: None,
            service_deadline: false,
            connect_end: None,
            retry_request: None,
            retry_metrics: None,
            retry_peer: None,
        })
    }
    fn cancel(self, ring: &mut Ring) -> io::Result<()> {
        self.cancel_pending(ring)
    }
    fn cancel_pending(&self, ring: &mut Ring) -> io::Result<()> {
        let result = match &self.state {
            State::Connecting(_, t) => ring.cancel(t).map(drop),
            State::Sending(_, t, _) | State::ReceivingHeaders(_, t, _) => ring.cancel(t).map(drop),
            State::ReceivingBody(_, _, t, _, _) => ring.cancel(t).map(drop),
            State::ReceivingSmall(_, _, t, _, _) => ring.cancel(t).map(drop),
            _ => Ok(()),
        };
        // Connect tickets request deferred cancellation on abandonment; shutdown
        // terminates connected I/O even when the cancellation SQ is full. Never
        // interpret a cancel acknowledgment as completion.
        match result {
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => Ok(()),
            other => other,
        }
    }
    fn poll(
        &mut self,
        ring: &mut Ring,
        budget: usize,
    ) -> crate::cache::Result<Progress<Completed<B>>> {
        let result = self.poll_inner(ring, budget).map_err(|error| {
            use crate::outcome::{Cause, Failure, Transport};
            let cause = match error.kind() {
                io::ErrorKind::TimedOut if self.local_pressure => Cause::LocalPressure,
                io::ErrorKind::TimedOut
                    if crate::environment::now() >= self.deadline && !self.service_deadline =>
                {
                    Cause::CallerDeadline
                }
                io::ErrorKind::TimedOut => Cause::ServiceTimeout,
                io::ErrorKind::Interrupted => Cause::Cancelled,
                io::ErrorKind::InvalidData | io::ErrorKind::InvalidInput => Cause::Protocol,
                io::ErrorKind::ConnectionRefused
                | io::ErrorKind::ConnectionReset
                | io::ErrorKind::ConnectionAborted
                | io::ErrorKind::NotConnected
                | io::ErrorKind::BrokenPipe
                | io::ErrorKind::UnexpectedEof => Cause::Connection,
                _ => Cause::Other,
            };
            let Some(socket) = &self.socket else {
                return error.into();
            };
            static DIAGNOSTICS: std::sync::OnceLock<bool> = std::sync::OnceLock::new();
            if *DIAGNOSTICS.get_or_init(|| std::env::var("RACER_HTTP_DIAGNOSTICS").as_deref() == Ok("1")) {
                eprintln!("HTTP exchange failure at {:?}: endpoint={:?} phase={:?} cause={:?} initiated={} kind={:?} error={error}", std::time::SystemTime::now(), socket.endpoint, self.phase, cause, self.initiated, error.kind());
            }
            crate::cache::Error::from(Failure {
                endpoint: socket.endpoint,
                transport: Transport::Http,
                phase: self.phase,
                cause,
                initiated: self.initiated,
                kind: error.kind(),
                message: error.to_string(),
            })
        });
        if result.is_err() {
            let _ = self.cancel_pending(ring);
            self.socket.take();
            self.state = State::Finished;
        }
        result
    }
    fn poll_inner(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Completed<B>>> {
        // A completed connect can leave us awaiting local file registration at
        // the next poll's deadline. Attribute that wait to admission, not connect.
        self.record_phase();
        if self.local_pressure {
            let since = *self
                .pressure_since
                .get_or_insert_with(crate::environment::now);
            if crate::environment::now().saturating_duration_since(since) >= ADMISSION_TIMEOUT {
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "HTTP local admission deadline",
                ));
            }
        } else {
            self.pressure_since = None;
        }
        if matches!(self.state, State::Finished) {
            return Err(invalid("exchange already finished"));
        }
        if crate::environment::now() >= self.deadline() {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "HTTP exchange deadline",
            ));
        }
        if let Some(identity) = &self.ring {
            if !Rc::ptr_eq(identity, ring.identity()) {
                return Err(invalid("exchange belongs to another ring"));
            }
        } else {
            match &self.socket.as_ref().unwrap().transport {
                Transport::Idle(identity) | Transport::Connected(_, identity)
                    if !Rc::ptr_eq(identity, ring.identity()) =>
                {
                    return Err(invalid("connection belongs to another ring"));
                }
                _ => {}
            }
            if let State::Connect(Payload {
                body: Body::Get(fill),
                ..
            })
            | State::Register(Payload {
                body: Body::Get(fill),
                ..
            })
            | State::Send(
                Payload {
                    body: Body::Get(fill),
                    ..
                },
                _,
            ) = &self.state
            {
                ring.validate_fill(fill)?;
            }
            self.ring = Some(ring.identity().clone());
        }
        for _ in 0..budget {
            self.record_phase();
            self.local_pressure = false;

            let state = std::mem::replace(&mut self.state, State::Finished);
            let socket = self.socket.as_mut().expect("active socket");
            let mut waiting = false;
            self.state = match state {
                State::Connect(p) => {
                    let Transport::New(address) = socket.transport else {
                        unreachable!()
                    };
                    match ring.connect_address(socket.file.clone().into(), address) {
                        Ok(t) => {
                            self.initiated = true;
                            State::Connecting(p, t.cancel_on_drop())
                        }
                        Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                            self.local_pressure = true;
                            self.state = State::Connect(p);
                            break;
                        }
                        Err(e) => return Err(e),
                    }
                }
                State::Connecting(p, mut t) => match ring.take_control(&mut t)? {
                    None => {
                        waiting = true;
                        State::Connecting(p, t)
                    }
                    Some(result) => {
                        result.result?;
                        #[cfg(test)]
                        if socket.tls_expired() {
                            return Err(io::ErrorKind::ConnectionAborted.into());
                        }
                        if let Some(pending) = socket.pending_tls.take() {
                            let mut tls = TlsChannel::new(
                                socket.file.clone(),
                                &pending.context,
                                pending.expected,
                                false,
                            )?;
                            tls.set_revision(socket.credential_revision);
                            tls.set_expiry(pending.expires_unix);
                            socket.tls = Some(tls);
                        }
                        State::Register(p)
                    }
                },
                State::Register(p) => match ring.register_file(socket.file.clone()) {
                    Ok(fixed) => {
                        socket.transport = Transport::Connected(fixed, ring.identity().clone());
                        if socket.tls.is_some() {
                            State::Handshake(p)
                        } else {
                            State::Send(p, 0)
                        }
                    }
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        self.local_pressure = true;
                        self.state = State::Register(p);
                        break;
                    }
                    Err(e) => return Err(e),
                },
                State::Handshake(p) => {
                    let deadline = self.connect_end.unwrap_or(self.deadline).min(self.deadline);
                    self.initiated = true;
                    match socket.tls.as_mut().unwrap().handshake(ring, deadline)? {
                        Progress::Ready(()) => State::Send(p, 0),
                        Progress::Pending(work) => {
                            self.state = State::Handshake(p);
                            return Ok(Progress::Pending(work));
                        }
                    }
                }
                State::Send(p, sent) => {
                    if let Some(tls) = &mut socket.tls {
                        self.initiated = true;
                        match tls.poll_write(
                            ring,
                            &p.scratch[sent..self.request_len],
                            self.deadline,
                        ) {
                            Err(e) => {
                                self.state = self.reconnect_idle(p, e)?;
                                continue;
                            }
                            Ok(Progress::Ready(n)) => {
                                let sent = sent + transfer(n, self.request_len - sent)?;
                                self.state = if sent == self.request_len {
                                    State::Headers(p, Cursor::default())
                                } else {
                                    State::Send(p, sent)
                                };
                                continue;
                            }
                            Ok(Progress::Pending(work)) => {
                                self.state = State::Send(p, sent);
                                return Ok(Progress::Pending(work));
                            }
                        }
                    }
                    let Transport::Connected(fd, _) = &socket.transport else {
                        unreachable!()
                    };
                    match ring.send_bytes_range(
                        fd.clone().into(),
                        p.scratch,
                        sent..self.request_len,
                    ) {
                        Ok(t) => {
                            self.initiated = true;
                            State::Sending(p.body, t, sent)
                        }
                        Err(r) if r.error.kind() == io::ErrorKind::WouldBlock => {
                            self.local_pressure = true;
                            self.state = State::Send(
                                Payload {
                                    scratch: r.resource,
                                    body: p.body,
                                },
                                sent,
                            );
                            break;
                        }
                        Err(r) => return Err(r.error),
                    }
                }
                State::Sending(body, mut t, sent) => match ring.take_bytes(&mut t)? {
                    None => {
                        waiting = true;
                        State::Sending(body, t, sent)
                    }
                    Some(c) => {
                        let p = Payload {
                            scratch: c.resource,
                            body,
                        };
                        let n = match c.result.and_then(|n| transfer(n, self.request_len - sent)) {
                            Ok(n) => n,
                            Err(e) => {
                                self.state = self.reconnect_idle(p, e)?;
                                continue;
                            }
                        };
                        if sent + n == self.request_len {
                            State::Headers(p, Cursor::default())
                        } else {
                            State::Send(p, sent + n)
                        }
                    }
                },
                State::Headers(mut p, mut cursor) => {
                    // Retire sent credentials before response parsing/pooling.
                    if cursor.used == 0 {
                        p.scratch.fill(0);
                        if p.scratch.len() != SCRATCH_SIZE {
                            p.scratch = vec![0; SCRATCH_SIZE].into_boxed_slice();
                        }
                    }
                    if let Some(end) = header_end(&p.scratch[..cursor.used], &mut cursor.scan) {
                        let mut metadata = parse(&p.scratch, cursor.start, end)?;
                        // Authentication is a terminal header outcome. Origins
                        // may send large error bodies; never buffer them in page
                        // storage or reuse this socket.
                        if matches!(metadata.status, 401 | 403) {
                            metadata.close = true;
                            cursor.used = end;
                        }
                        if metadata.status < 200 {
                            if metadata.status == 101 || metadata.length.is_some() || metadata.close
                            {
                                return Err(protocol("unsupported informational response"));
                            }
                            cursor.informational += 1;
                            if cursor.informational > MAX_INFORMATIONAL {
                                return Err(protocol("too many informational responses"));
                            }
                            cursor.start = end;
                            cursor.scan = end;
                            State::Headers(p, cursor)
                        } else {
                            let len = if matches!(metadata.status, 401 | 403) {
                                0
                            } else {
                                body_length(&metadata, matches!(p.body, Body::Head))?
                            };
                            let prefix = cursor.used - end;
                            if prefix > len {
                                return Err(protocol("bytes beyond response body"));
                            }
                            if let Body::Get(fill) = &mut p.body {
                                fill.as_mut_slice()[..prefix]
                                    .copy_from_slice(&p.scratch[end..cursor.used]);
                            }
                            if let Body::Small(bytes) = &mut p.body {
                                if len > bytes.len() {
                                    return Err(protocol("small response body too large"));
                                }
                                bytes[..prefix].copy_from_slice(&p.scratch[end..cursor.used]);
                            }
                            if prefix == len {
                                State::Complete(p, metadata, len)
                            } else if let Body::Small(bytes) = p.body {
                                State::SmallBody(p.scratch, metadata, bytes, prefix, len)
                            } else {
                                let Body::Get(fill) = p.body else {
                                    unreachable!()
                                };
                                State::Body(p.scratch, metadata, fill, prefix, len)
                            }
                        }
                    } else {
                        if cursor.used == SCRATCH_SIZE {
                            return Err(protocol("response headers exceed 8 KiB"));
                        }
                        if let Some(tls) = &mut socket.tls {
                            self.initiated = true;
                            match tls.poll_read(ring, &mut p.scratch[cursor.used..], self.deadline)
                            {
                                Err(e) => {
                                    self.state = self.reconnect_idle(p, e)?;
                                    continue;
                                }
                                Ok(Progress::Ready(n)) => {
                                    let n = match transfer(n, SCRATCH_SIZE - cursor.used) {
                                        Ok(n) => n,
                                        Err(e) => {
                                            self.state = self.reconnect_idle(p, e)?;
                                            continue;
                                        }
                                    };
                                    self.reused = false;
                                    self.retry_request = None;
                                    self.retry_metrics = None;
                                    self.retry_peer = None;
                                    cursor.used += n;
                                    self.state = State::Headers(p, cursor);
                                    continue;
                                }
                                Ok(Progress::Pending(work)) => {
                                    self.state = State::Headers(p, cursor);
                                    return Ok(Progress::Pending(work));
                                }
                            }
                        }
                        let Transport::Connected(fd, _) = &socket.transport else {
                            unreachable!()
                        };
                        match ring.recv_bytes_range(
                            fd.clone().into(),
                            p.scratch,
                            cursor.used..SCRATCH_SIZE,
                        ) {
                            Ok(t) => {
                                self.initiated = true;
                                State::ReceivingHeaders(p.body, t, cursor)
                            }
                            Err(r) if r.error.kind() == io::ErrorKind::WouldBlock => {
                                self.local_pressure = true;
                                self.state = State::Headers(
                                    Payload {
                                        scratch: r.resource,
                                        body: p.body,
                                    },
                                    cursor,
                                );
                                break;
                            }
                            Err(r) => return Err(r.error),
                        }
                    }
                }
                State::ReceivingHeaders(body, mut t, mut cursor) => {
                    match ring.take_bytes(&mut t)? {
                        None => {
                            waiting = true;
                            State::ReceivingHeaders(body, t, cursor)
                        }
                        Some(c) => {
                            let p = Payload {
                                scratch: c.resource,
                                body,
                            };
                            match c
                                .result
                                .and_then(|n| transfer(n, SCRATCH_SIZE - cursor.used))
                            {
                                Ok(n) => {
                                    self.reused = false;
                                    self.retry_request = None;
                                    self.retry_metrics = None;
                                    self.retry_peer = None;
                                    cursor.used += n;
                                    State::Headers(p, cursor)
                                }
                                Err(e) => self.reconnect_idle(p, e)?,
                            }
                        }
                    }
                }
                State::Body(scratch, metadata, mut fill, received, len) => {
                    if let Some(tls) = &mut socket.tls {
                        match tls.poll_read(
                            ring,
                            &mut fill.as_mut_slice()[received..len.min(received + 64 * 1024)],
                            self.deadline,
                        )? {
                            Progress::Ready(n) => {
                                let received = received + transfer(n, len - received)?;
                                self.state = if received == len {
                                    State::Complete(
                                        Payload {
                                            scratch,
                                            body: Body::Get(fill),
                                        },
                                        metadata,
                                        len,
                                    )
                                } else {
                                    State::Body(scratch, metadata, fill, received, len)
                                };
                                continue;
                            }
                            Progress::Pending(work) => {
                                self.state = State::Body(scratch, metadata, fill, received, len);
                                return Ok(Progress::Pending(work));
                            }
                        }
                    }
                    let Transport::Connected(fd, _) = &socket.transport else {
                        unreachable!()
                    };
                    match ring.recv(fd.clone().into(), fill, BufferRange::new(received..len)?) {
                        Ok(t) => {
                            self.initiated = true;
                            State::ReceivingBody(scratch, metadata, t, received, len)
                        }
                        Err(r) if r.error.kind() == io::ErrorKind::WouldBlock => {
                            self.local_pressure = true;
                            self.state = State::Body(scratch, metadata, r.resource, received, len);
                            break;
                        }
                        Err(r) => return Err(r.error),
                    }
                }
                State::ReceivingBody(scratch, metadata, mut t, received, len) => {
                    match ring.take_read(&mut t)? {
                        None => {
                            waiting = true;
                            State::ReceivingBody(scratch, metadata, t, received, len)
                        }
                        Some(c) => {
                            let received = received + transfer(c.result?, len - received)?;
                            if received == len {
                                State::Complete(
                                    Payload {
                                        scratch,
                                        body: Body::Get(c.resource),
                                    },
                                    metadata,
                                    len,
                                )
                            } else {
                                State::Body(scratch, metadata, c.resource, received, len)
                            }
                        }
                    }
                }
                State::SmallBody(scratch, metadata, mut bytes, received, len) => {
                    if let Some(tls) = &mut socket.tls {
                        match tls.poll_read(ring, &mut bytes[received..len], self.deadline)? {
                            Progress::Ready(n) => {
                                let received = received + transfer(n, len - received)?;
                                self.state = if received == len {
                                    State::Complete(
                                        Payload {
                                            scratch,
                                            body: Body::Small(bytes),
                                        },
                                        metadata,
                                        len,
                                    )
                                } else {
                                    State::SmallBody(scratch, metadata, bytes, received, len)
                                };
                                continue;
                            }
                            Progress::Pending(work) => {
                                self.state =
                                    State::SmallBody(scratch, metadata, bytes, received, len);
                                return Ok(Progress::Pending(work));
                            }
                        }
                    }
                    let Transport::Connected(fd, _) = &socket.transport else {
                        unreachable!()
                    };
                    match ring.recv_bytes_range(fd.clone().into(), bytes, received..len) {
                        Ok(t) => {
                            self.initiated = true;
                            State::ReceivingSmall(scratch, metadata, t, received, len)
                        }
                        Err(r) if r.error.kind() == io::ErrorKind::WouldBlock => {
                            self.local_pressure = true;
                            self.state =
                                State::SmallBody(scratch, metadata, r.resource, received, len);
                            break;
                        }
                        Err(r) => return Err(r.error),
                    }
                }
                State::ReceivingSmall(scratch, metadata, mut t, received, len) => {
                    match ring.take_bytes(&mut t)? {
                        None => {
                            waiting = true;
                            State::ReceivingSmall(scratch, metadata, t, received, len)
                        }
                        Some(c) => {
                            let received = received + transfer(c.result?, len - received)?;
                            if received == len {
                                State::Complete(
                                    Payload {
                                        scratch,
                                        body: Body::Small(c.resource),
                                    },
                                    metadata,
                                    len,
                                )
                            } else {
                                State::SmallBody(scratch, metadata, c.resource, received, len)
                            }
                        }
                    }
                }
                State::Complete(p, metadata, len) => {
                    let socket = self.socket.take().filter(|_| !metadata.close);
                    return Ok(Progress::Ready(Completed {
                        response: Response {
                            socket,
                            scratch: p.scratch,
                            metadata,
                        },
                        body: p.body,
                        len,
                    }));
                }
                State::Finished => unreachable!(),
            };
            if waiting {
                self.pressure_since = None;
                return Ok(Progress::Pending(Work {
                    runnable: false,
                    deadline: Some(self.deadline()),
                }));
            }
        }
        let deadline = if self.local_pressure {
            let since = *self
                .pressure_since
                .get_or_insert_with(crate::environment::now);
            self.deadline().min(since + ADMISSION_TIMEOUT)
        } else {
            self.pressure_since = None;
            self.deadline()
        };
        Ok(Progress::Pending(Work {
            runnable: true,
            deadline: Some(deadline),
        }))
    }
}

fn parse(bytes: &[u8], start: usize, end: usize) -> io::Result<Metadata> {
    let status_end = line(bytes, start, end)?;
    let status = &bytes[start..status_end];
    if status.len() < 13
        || &status[..9] != b"HTTP/1.1 "
        || status[12] != b' '
        || !status[9..12].iter().all(u8::is_ascii_digit)
        || !value(&status[13..])
    {
        return Err(protocol("invalid HTTP/1.1 status line"));
    }
    let code = (status[9] - b'0') as u16 * 100
        + (status[10] - b'0') as u16 * 10
        + (status[11] - b'0') as u16;
    if !(100..=599).contains(&code) {
        return Err(protocol("invalid response status"));
    }
    let mut m = Metadata {
        status: code,
        length: None,
        close: false,
        headers: [Header::default(); MAX_HEADERS],
        count: 0,
    };
    let mut pos = status_end + 2;
    while pos < end - 2 {
        if m.count == MAX_HEADERS {
            return Err(protocol("too many response headers"));
        }
        let stop = line(bytes, pos, end)?;
        let h = field(bytes, pos, stop)?;
        let name = h.name.slice(bytes);
        let v = h.value.slice(bytes);
        if name.eq_ignore_ascii_case(b"content-length") {
            if m.length.is_some() || v.is_empty() {
                return Err(protocol("ambiguous Content-Length"));
            }
            m.length = Some(decimal(v)?);
        } else if name.eq_ignore_ascii_case(b"transfer-encoding") && !matches!(code, 401 | 403) {
            return Err(protocol("Transfer-Encoding is unsupported"));
        } else if name.eq_ignore_ascii_case(b"connection") {
            m.close |= connection_close(v)?;
        } else if name.eq_ignore_ascii_case(b"upgrade") {
            return Err(protocol("protocol upgrade unsupported"));
        }
        m.headers[m.count] = h;
        m.count += 1;
        pos = stop + 2;
    }
    Ok(m)
}
fn body_length(m: &Metadata, head: bool) -> io::Result<usize> {
    if m.status == 204 && m.length.is_some() {
        return Err(protocol("Content-Length on 204"));
    }
    if head || matches!(m.status, 204 | 304) {
        return Ok(0);
    }
    if m.status == 205 && m.length != Some(0) {
        return Err(protocol("205 requires zero Content-Length"));
    }
    let len = m.length.ok_or_else(|| protocol("missing Content-Length"))?;
    if len > BUFFER_SIZE as u64 {
        return Err(protocol("response exceeds one pool buffer"));
    }
    Ok(len as usize)
}

mod retry {
    //! Opt-in recovery before the first response byte. The exchange keeps
    //! its deadline, ring identity, writable capability and caller-owned health permit.
    use super::*;

    impl<B: Writable> GetExchange<B> {
        pub(crate) fn take_stale(&mut self) -> Option<(B, Instant, Option<Instant>, bool)> {
            let payload = self.0.stale_payload.take()?;
            let Body::Get(body) = payload.body else {
                unreachable!()
            };
            Some((
                body,
                self.0.deadline,
                self.0.connect_end,
                self.0.service_deadline,
            ))
        }
        pub(crate) fn retain_connect_end(&mut self, end: Option<Instant>, service: bool) {
            self.0.connect_end = end;
            self.0.service_deadline = service;
        }
        // Production peer retries must return to Provider to spend chain budget.
        #[cfg(test)]
        pub(crate) fn retry_idle_peer(
            mut self,
            origin: &Origin,
            metrics: &crate::metrics::Local,
        ) -> Self {
            self.0.enable_peer_retry(origin, metrics);
            self
        }
        pub(crate) fn retry_idle_backend(mut self, metrics: &crate::metrics::Local) -> Self {
            self.0.enable_idle_retry(metrics);
            self
        }
    }
    impl SmallExchange {
        pub(crate) fn take_stale(&mut self) -> Option<(Instant, Option<Instant>, bool)> {
            self.0.stale_payload.take()?;
            Some((self.0.deadline, self.0.connect_end, self.0.service_deadline))
        }
        pub(crate) fn retain_connect_end(&mut self, end: Option<Instant>, service: bool) {
            self.0.connect_end = end;
            self.0.service_deadline = service;
        }
        #[cfg(test)]
        pub(crate) fn retry_idle_peer(
            mut self,
            origin: &Origin,
            metrics: &crate::metrics::Local,
        ) -> Self {
            self.0.enable_peer_retry(origin, metrics);
            self
        }
    }
    impl HeadExchange {
        pub(crate) fn retry_idle_backend(mut self, metrics: &crate::metrics::Local) -> Self {
            self.0.enable_idle_retry(metrics);
            self
        }
    }

    impl<B: Writable> Exchange<B> {
        #[cfg(test)]
        fn enable_peer_retry(&mut self, origin: &Origin, metrics: &crate::metrics::Local) {
            if let Some(tls) = &origin.tls {
                self.enable_idle_retry(metrics);
                if self.retry_request.is_some() {
                    self.retry_peer = Some(tls.clone());
                }
            }
        }
        pub(super) fn enable_idle_retry(&mut self, metrics: &crate::metrics::Local) {
            // Only a previously completed/recycled connection is eligible. Never
            // retry initial connect failures or partially received responses.
            if let State::Register(p) = &self.state {
                self.retry_request = Some(p.scratch[..self.request_len].into());
                self.retry_metrics = Some(metrics.clone());
            }
        }

        pub(super) fn reconnect_idle(
            &mut self,
            mut payload: Payload<B>,
            error: io::Error,
        ) -> io::Result<State<B>> {
            if !matches!(
                error.kind(),
                io::ErrorKind::UnexpectedEof
                    | io::ErrorKind::BrokenPipe
                    | io::ErrorKind::ConnectionReset
                    | io::ErrorKind::ConnectionAborted
            ) || crate::environment::now() >= self.deadline()
            {
                return Err(error);
            }
            let Some(request) = self.retry_request.take() else {
                // Return unpublished storage to Provider for a budgeted fresh
                // attempt. Eligibility ends at the very first response byte.
                if self.reused {
                    self.reused = false;
                    self.stale_payload = Some(payload);
                }
                return Err(error);
            };
            // Plain IO has a terminal completion; TLS IO is synchronous and its
            // readiness tickets retain only the socket, never payload storage.
            // No body IO has been submitted and no response byte has been observed.
            let old = self.socket.as_ref().expect("active socket");
            let peer = self.retry_peer.take();
            let connection = if let Some((provider, expected)) = &peer {
                let snapshot = provider.current();
                let connect = || {
                    Connection::new_tls(
                        old.endpoint
                            .tcp()
                            .ok_or_else(|| invalid("peer TLS requires TCP"))?,
                        &old.host,
                        &snapshot.context,
                        crate::tls::ExpectedPeer::Identity(expected.clone()),
                    )
                };

                let mut connection = connect()?;
                connection.set_tls_revision(snapshot.revision, snapshot.expires_unix);
                connection
            } else {
                // A TLS connection must never reconnect as plaintext.
                if old.tls.is_some() {
                    return Err(error);
                }
                Connection::new_address(old.endpoint, &old.host)?
            };

            self.socket = Some(connection.socket); // shuts down and drops stale TCP
            if payload.scratch.len() < request.len() {
                payload.scratch = vec![0; request.len()].into_boxed_slice();
            }
            payload.scratch[..request.len()].copy_from_slice(&request);
            if let Some(metrics) = self.retry_metrics.take() {
                metrics.upstream(
                    if peer.is_some() {
                        crate::metrics::Upstream::PeerHttp
                    } else {
                        crate::metrics::Upstream::BackendHttp
                    },
                    match payload.body {
                        Body::Head | Body::Small(_) => crate::metrics::Kind::Metadata,
                        Body::Get(_) => crate::metrics::Kind::Page,
                    },
                );
            }
            // ring/deadline/connect cap and pressure history are deliberately retained.
            Ok(State::Connect(payload))
        }
    }
}

#[cfg(test)]
#[path = "../tests/http/client.rs"]
mod tests;

pub use crate::http;

pub mod breaker {
    //! Compatibility path; shared health implementation lives in crate::breaker.
    #[cfg(test)]
    use crate::breaker::Status;
    pub use crate::breaker::{CircuitBreaker, Permit, Rejected};
    #[cfg(test)]
    use std::{
        rc::Rc,
        time::{Duration, Instant},
    };

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/http/breaker.rs"
    ));
}
