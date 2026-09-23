// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local HTTP/1.1 GET/HEAD transport over TCP or filesystem Unix sockets.
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
//! limited to one 4 MiB pool buffer; HEAD/304 metadata may describe larger objects.
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
    pub(crate) endpoint: Endpoint,
    idle: Vec<Connection>,
    max_idle_age: Option<std::time::Duration>,
    pub(crate) breaker: crate::breaker::CircuitBreaker,
    pub(crate) limit: usize,
}
impl Origin {
    pub(crate) fn new(endpoint: Endpoint) -> Self {
        Self {
            endpoint,
            idle: Vec::new(),
            max_idle_age: None,
            breaker: crate::breaker::CircuitBreaker::new(std::time::Duration::from_secs(1)),
            limit: 16,
        }
    }
    pub(crate) fn peer(endpoint: Endpoint) -> Self {
        Self {
            // Peer servers close idle connections at 30s. Age from the start of
            // the last exchange, not recycling: response/validation time must
            // not make an old server-side idle socket appear young locally.
            max_idle_age: Some(std::time::Duration::from_secs(20)),
            ..Self::new(endpoint)
        }
    }
    pub(crate) fn connection(&mut self) -> io::Result<(Connection, crate::breaker::Permit)> {
        let rejected = |cause, message: &str| {
            io::Error::new(
                io::ErrorKind::WouldBlock,
                attempt::Failure {
                    endpoint: self.endpoint.address,
                    transport: attempt::Transport::Http,
                    phase: attempt::Phase::LocalAdmission,
                    cause,
                    initiated: false,
                    kind: io::ErrorKind::WouldBlock,
                    message: message.into(),
                },
            )
        };
        if self.breaker.active() >= self.limit {
            return Err(rejected(
                attempt::Cause::LocalPressure,
                "direct endpoint exchange limit",
            ));
        }
        // Breaker rejection is not fresh evidence of a failed connection.
        let permit = self.breaker.try_acquire().map_err(|_| {
            rejected(
                attempt::Cause::BreakerRejected,
                "direct endpoint breaker rejected",
            )
        })?;
        if let Some(max_age) = self.max_idle_age {
            let now = crate::environment::now();
            self.idle
                .retain(|c| now.saturating_duration_since(c.socket.started) < max_age);
        }
        let connection = self.idle.pop().map(Ok).unwrap_or_else(|| {
            Connection::new_address(self.endpoint.address, &self.endpoint.host)
        })?;
        Ok((connection, permit))
    }
    pub(crate) fn recycle(&mut self, connection: Option<Connection>) {
        if self.idle.len() < 16 {
            self.idle.extend(connection);
        }
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
        observed: Rc<RefCell<Option<Instant>>>,
    }
    pub(crate) struct OwnerPermit {
        permit: crate::breaker::Permit,
        observed: Rc<RefCell<Option<Instant>>>,
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
                self.observed.replace(Some(observed));
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
        #[cfg(test)]
        pub(crate) fn needs_probe(&self, identity: [u8; 32], slot: u32) -> bool {
            self.0
                .get(&(identity, Key::Slot(slot)))
                .is_some_and(|h| h.observed.borrow().is_some())
        }
        #[cfg(test)]
        pub(crate) fn physical_needs_probe(&self, identity: [u8; 32], peer: &str) -> bool {
            self.0
                .get(&(identity, Key::Physical(peer.into())))
                .is_some_and(|h| h.observed.borrow().is_some())
        }
        pub(crate) fn blocked(&self, identity: [u8; 32], slot: u32) -> bool {
            self.0
                .get(&(identity, Key::Slot(slot)))
                .is_some_and(|b| !b.breaker.available())
        }
        pub(crate) fn evidence(&self, identity: [u8; 32], slot: u32) -> Option<Instant> {
            self.0
                .get(&(identity, Key::Slot(slot)))?
                .observed
                .borrow()
                .filter(|at| *at + COOLDOWN > crate::environment::now())
        }
        #[cfg(test)]
        pub(crate) fn is_empty(&self) -> bool {
            self.0.is_empty()
        }
        #[cfg(test)]
        pub(crate) fn has_evidence(&self) -> bool {
            self.0.values().any(|h| h.observed.borrow().is_some())
        }
        pub(crate) fn acquire(&mut self, identity: [u8; 32], slot: u32) -> io::Result<OwnerPermit> {
            self.acquire_key(identity, Key::Slot(slot))
        }
        pub(crate) fn physical_evidence(&self, identity: [u8; 32], peer: &str) -> Option<Instant> {
            self.0
                .get(&(identity, Key::Physical(peer.into())))?
                .observed
                .borrow()
                .filter(|at| *at + COOLDOWN > crate::environment::now())
        }
        pub(crate) fn acquire_final(
            &mut self,
            identity: [u8; 32],
            slot: u32,
            peer: Option<&str>,
        ) -> io::Result<OwnerPermit> {
            let mut owner = self.acquire(identity, slot)?;
            if let Some(peer) = peer {
                owner.physical = Some(Box::new(
                    self.acquire_key(identity, Key::Physical(peer.into()))?,
                ));
            }
            Ok(owner)
        }
        fn acquire_key(&mut self, identity: [u8; 32], owner: Key) -> io::Result<OwnerPermit> {
            let key = (identity, owner);
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
                self.0.insert(
                    key.clone(),
                    OwnerHealth {
                        breaker: crate::breaker::CircuitBreaker::new(COOLDOWN),
                        observed: Rc::new(RefCell::new(None)),
                    },
                );
            }
            let health = &self.0[&key];
            Ok(OwnerPermit {
                permit: health
                    .breaker
                    .try_acquire()
                    .map_err(|_| io::Error::from(io::ErrorKind::WouldBlock))?,
                observed: health.observed.clone(),
                physical: None,
            })
        }
    }
    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/http/owner_health.rs"
    ));
}

/// Evidence captured before an HTTP state is retired or an io::Error is converted.
/// Semantic peer reports preserve downstream attribution independently of the
/// healthy reporting transport.
pub mod attempt {
    use std::{fmt, io, net::SocketAddr};
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Phase {
        LocalAdmission,
        Connect,
        Send,
        Headers,
        Body,
        Grant,
        Read,
    }
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Transport {
        Http,
        Rdma,
    }
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Cause {
        Connection,
        ServiceTimeout,
        CallerDeadline,
        LocalPressure,
        Cancelled,
        Protocol,
        BreakerRejected,
        Other,
    }
    /// Bounded semantic outcome, distinct from failure of the reporting transport.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    #[repr(u8)]
    pub enum PeerReason {
        OwnerUnavailable = 1,
        Busy,
        Unavailable,
        Protocol,
        Service,
        Deadline,
        Cancelled,
        NotFound,
        Gone,
        Precondition,
    }
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct PeerFailure {
        pub identity: [u8; 32],
        pub candidate: u32,
        pub reason: PeerReason,
        /// Original downstream attempt, never evidence about the reporting hop.
        pub evidence: Option<PeerEvidence>,
    }
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct PeerEvidence {
        pub endpoint: SocketAddr,
        pub transport: Transport,
        pub phase: Phase,
        pub cause: Cause,
        pub initiated: bool,
    }
    impl PeerEvidence {
        pub fn from_failure(f: &Failure) -> Option<Self> {
            Some(Self {
                endpoint: f.endpoint.tcp()?,
                transport: f.transport,
                phase: f.phase,
                cause: f.cause,
                initiated: f.initiated,
            })
        }
    }
    impl PeerFailure {
        pub const LEN: usize = 61;
        pub fn encode(self) -> [u8; Self::LEN] {
            let mut out = [0; Self::LEN];
            out[..32].copy_from_slice(&self.identity);
            out[32..36].copy_from_slice(&self.candidate.to_be_bytes());
            out[36] = self.reason as u8;
            if let Some(e) = self.evidence {
                out[37] = match e.endpoint.ip() {
                    std::net::IpAddr::V4(ip) => {
                        out[38..42].copy_from_slice(&ip.octets());
                        4
                    }
                    std::net::IpAddr::V6(ip) => {
                        out[38..54].copy_from_slice(&ip.octets());
                        6
                    }
                };
                out[54..56].copy_from_slice(&e.endpoint.port().to_be_bytes());
                out[56] = match e.transport {
                    Transport::Http => 1,
                    Transport::Rdma => 2,
                };
                out[57] = match e.phase {
                    Phase::LocalAdmission => 1,
                    Phase::Connect => 2,
                    Phase::Send => 3,
                    Phase::Headers => 4,
                    Phase::Body => 5,
                    Phase::Grant => 6,
                    Phase::Read => 7,
                };
                out[58] = match e.cause {
                    Cause::Connection => 1,
                    Cause::ServiceTimeout => 2,
                    Cause::CallerDeadline => 3,
                    Cause::LocalPressure => 4,
                    Cause::Cancelled => 5,
                    Cause::Protocol => 6,
                    Cause::BreakerRejected => 7,
                    Cause::Other => 8,
                };
                out[59] = u8::from(e.initiated);
            }
            out
        }
        pub fn decode(bytes: &[u8]) -> io::Result<Self> {
            if bytes.len() != Self::LEN {
                return Err(io::ErrorKind::InvalidData.into());
            }
            let reason = match bytes[36] {
                1 => PeerReason::OwnerUnavailable,
                2 => PeerReason::Busy,
                3 => PeerReason::Unavailable,
                4 => PeerReason::Protocol,
                5 => PeerReason::Service,
                6 => PeerReason::Deadline,
                7 => PeerReason::Cancelled,
                8 => PeerReason::NotFound,
                9 => PeerReason::Gone,
                10 => PeerReason::Precondition,
                _ => return Err(io::ErrorKind::InvalidData.into()),
            };
            let bad = || io::Error::from(io::ErrorKind::InvalidData);
            let evidence = if bytes[37] == 0 {
                if bytes[38..].iter().any(|b| *b != 0) {
                    return Err(bad());
                }
                None
            } else {
                let ip = match bytes[37] {
                    4 if bytes[42..54].iter().all(|b| *b == 0) => std::net::IpAddr::V4(
                        std::net::Ipv4Addr::from(<[u8; 4]>::try_from(&bytes[38..42]).unwrap()),
                    ),
                    6 => std::net::IpAddr::V6(std::net::Ipv6Addr::from(
                        <[u8; 16]>::try_from(&bytes[38..54]).unwrap(),
                    )),
                    _ => return Err(bad()),
                };
                if bytes[59] > 1 || bytes[60] != 0 {
                    return Err(bad());
                }
                Some(PeerEvidence {
                    endpoint: SocketAddr::new(
                        ip,
                        u16::from_be_bytes(bytes[54..56].try_into().unwrap()),
                    ),
                    transport: match bytes[56] {
                        1 => Transport::Http,
                        2 => Transport::Rdma,
                        _ => return Err(bad()),
                    },
                    phase: match bytes[57] {
                        1 => Phase::LocalAdmission,
                        2 => Phase::Connect,
                        3 => Phase::Send,
                        4 => Phase::Headers,
                        5 => Phase::Body,
                        6 => Phase::Grant,
                        7 => Phase::Read,
                        _ => return Err(bad()),
                    },
                    cause: match bytes[58] {
                        1 => Cause::Connection,
                        2 => Cause::ServiceTimeout,
                        3 => Cause::CallerDeadline,
                        4 => Cause::LocalPressure,
                        5 => Cause::Cancelled,
                        6 => Cause::Protocol,
                        7 => Cause::BreakerRejected,
                        8 => Cause::Other,
                        _ => return Err(bad()),
                    },
                    initiated: bytes[59] == 1,
                })
            };
            Ok(Self {
                identity: bytes[..32].try_into().unwrap(),
                candidate: u32::from_be_bytes(bytes[32..36].try_into().unwrap()),
                reason,
                evidence,
            })
        }
    }
    impl fmt::Display for PeerFailure {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            write!(f, "{self:?}")
        }
    }
    impl std::error::Error for PeerFailure {}
    #[derive(Clone, Debug)]
    pub struct Failure {
        pub endpoint: crate::socket::Address,
        pub transport: Transport,
        pub phase: Phase,
        pub cause: Cause,
        pub initiated: bool,
        pub kind: io::ErrorKind,
        pub message: String,
    }
    impl Failure {
        pub fn owner_evidence(&self) -> bool {
            self.endpoint.tcp().is_some()
                && self.initiated
                && matches!(self.cause, Cause::Connection | Cause::ServiceTimeout)
        }
    }
    impl fmt::Display for Failure {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.write_str(&self.message)
        }
    }
    impl std::error::Error for Failure {}
}

/// Validated origin-form target and additional headers, borrowed only until start.
/// Host and all request framing/connection-control headers are transport-owned.
#[derive(Clone, Copy)]
pub struct Request<'a> {
    target: &'a str,
    headers: &'a [(&'a str, &'a str)],
    #[cfg(test)]
    backend: bool,
}
impl<'a> Request<'a> {
    pub fn new(target: &'a str, headers: &'a [(&'a str, &'a str)]) -> io::Result<Self> {
        if !crate::http::target(target.as_bytes()) {
            return Err(invalid("invalid origin-form request target"));
        }
        for &(name, v) in headers {
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
            #[cfg(test)]
            backend: false,
        })
    }

    pub(crate) fn backend(target: &'a str, headers: &'a [(&'a str, &'a str)]) -> io::Result<Self> {
        let request = Self::new(target, headers)?;
        #[cfg(test)]
        let request = Self {
            backend: true,
            ..request
        };
        Ok(request)
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
struct Socket {
    endpoint: Address,
    started: Instant,
    file: File,
    transport: Transport,
    host: Box<str>,
}
impl Drop for Socket {
    fn drop(&mut self) {
        self.file.shutdown_socket();
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
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            return Ok(Self {
                socket: Socket {
                    endpoint: address,
                    started: crate::environment::now(),
                    file: File::simulated(world.socket()),
                    transport: Transport::New(address),
                    host: host.into(),
                },
                scratch: vec![0; SCRATCH_SIZE].into_boxed_slice(),
            });
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
                endpoint: address,
                started: crate::environment::now(),
                file: File::new(fd),
                transport: Transport::New(address),
                host: host.into(),
            },
            scratch: vec![0; SCRATCH_SIZE].into_boxed_slice(),
        })
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
        #[cfg(test)]
        if self.response.metadata.status == 200
            && crate::simulation::current().is_some_and(|world| {
                world.activate_mutant(crate::simulation::history::Mutant::SuccessfulGetStatus)
            })
        {
            return 201;
        }
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
    socket: Option<Socket>,
    state: State<B>,
    request_len: usize,
    deadline: Instant,
    ring: Option<Rc<Identity>>,
    phase: attempt::Phase,
    initiated: bool,
    local_pressure: bool,
    pressure_since: Option<Instant>,
    service_deadline: bool,
    connect_end: Option<Instant>,
    retry_request: Option<Box<[u8]>>,
    retry_metrics: Option<crate::metrics::Local>,
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
        if matches!(self.state, State::Connect(_) | State::Connecting(..)) {
            self.connect_end.unwrap_or(self.deadline).min(self.deadline)
        } else {
            self.deadline
        }
    }
    fn record_phase(&mut self) {
        use attempt::Phase;
        self.phase = match &self.state {
            State::Connect(_) | State::Connecting(..) => Phase::Connect,
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
        );
    }
    fn new(
        mut connection: Connection,
        request: Request<'_>,
        body: Body<B>,
        deadline: Instant,
    ) -> io::Result<Self> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            let target = request
                .headers
                .iter()
                .find(|(n, _)| n.eq_ignore_ascii_case("x-racer-fault"))
                .map_or_else(
                    || {
                        if request.backend {
                            request.target.to_owned()
                        } else {
                            format!("external:{}", request.target)
                        }
                    },
                    |(_, wire)| crate::handlers::simulation_target(wire),
                );
            world.tag_socket(
                connection.socket.file.simulation_id().unwrap(),
                connection
                    .socket
                    .endpoint
                    .tcp()
                    .expect("simulation TCP endpoint"),
                target,
            );
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
            socket: Some(connection.socket),
            state,
            request_len,
            deadline,
            ring: None,
            phase: attempt::Phase::LocalAdmission,
            initiated: false,
            local_pressure: false,
            pressure_since: None,
            service_deadline: false,
            connect_end: None,
            retry_request: None,
            retry_metrics: None,
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
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Progress<Completed<B>>> {
        let result = self.poll_inner(ring, budget).map_err(|error| {
            use attempt::{Cause, Failure, Transport};
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
                return error;
            };
            io::Error::new(
                error.kind(),
                Failure {
                    endpoint: socket.endpoint,
                    transport: Transport::Http,
                    phase: self.phase,
                    cause,
                    initiated: self.initiated,
                    kind: error.kind(),
                    message: error.to_string(),
                },
            )
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
            #[cfg(test)]
            if let Some(world) = crate::simulation::current() {
                world.socket_timeout(self.socket.as_ref().unwrap().file.simulation_id().unwrap());
            }
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
            #[cfg(test)]
            if let Some(world) = crate::simulation::current() {
                use crate::simulation::Phase;
                let phase = match &self.state {
                    State::Connect(_) | State::Connecting(..) => Some(Phase::Connect),
                    State::Send(..) | State::Sending(..) => Some(Phase::Request),
                    State::Headers(..) | State::ReceivingHeaders(..) => Some(Phase::Headers),
                    State::Body(_, _, _, n, _)
                    | State::ReceivingBody(_, _, _, n, _)
                    | State::SmallBody(_, _, _, n, _)
                    | State::ReceivingSmall(_, _, _, n, _)
                        if *n > 0 =>
                    {
                        Some(Phase::PartialBody)
                    }
                    _ => None,
                };
                if let Some(phase) = phase {
                    world.socket_phase(
                        self.socket.as_ref().unwrap().file.simulation_id().unwrap(),
                        phase,
                    );
                }
            }
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
                        State::Register(p)
                    }
                },
                State::Register(p) => match ring.register_file(socket.file.clone()) {
                    Ok(fixed) => {
                        socket.transport = Transport::Connected(fixed, ring.identity().clone());
                        State::Send(p, 0)
                    }
                    Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                        self.local_pressure = true;
                        self.state = State::Register(p);
                        break;
                    }
                    Err(e) => return Err(e),
                },
                State::Send(p, sent) => {
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
                    if let Some(end) = header_end(&p.scratch[..cursor.used], &mut cursor.scan) {
                        let metadata = parse(&p.scratch, cursor.start, end)?;
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
                            let len = body_length(&metadata, matches!(p.body, Body::Head))?;
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
                                    self.retry_request = None;
                                    self.retry_metrics = None;
                                    cursor.used += n;
                                    State::Headers(p, cursor)
                                }
                                Err(e) => self.reconnect_idle(p, e)?,
                            }
                        }
                    }
                }
                State::Body(scratch, metadata, fill, received, len) => {
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
                State::SmallBody(scratch, metadata, bytes, received, len) => {
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
        } else if name.eq_ignore_ascii_case(b"transfer-encoding") {
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
    //! Backend opt-in recovery before the first response byte. The exchange keeps
    //! its deadline, ring identity, writable capability and caller-owned health permit.
    use super::*;

    impl<B: Writable> GetExchange<B> {
        pub(crate) fn retry_idle_backend(mut self, metrics: &crate::metrics::Local) -> Self {
            self.0.enable_idle_retry(metrics);
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
        pub(super) fn enable_idle_retry(&mut self, metrics: &crate::metrics::Local) {
            // Only a previously completed/recycled connection is eligible. Never
            // retry initial connect failures, nor turn this into a general peer retry.
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
                return Err(error);
            };
            // Called only after take_bytes returned a terminal SEND/RECV completion.
            // No body IO has been submitted and no response byte has been observed.
            let old = self.socket.as_ref().expect("active socket");
            let connection = Connection::new_address(old.endpoint, &old.host)?;
            #[cfg(test)]
            if let Some(w) = crate::simulation::current() {
                w.copy_socket_tag(
                    old.file.simulation_id().unwrap(),
                    connection.socket.file.simulation_id().unwrap(),
                );
            }
            self.socket = Some(connection.socket); // shuts down and drops stale TCP
            payload.scratch[..request.len()].copy_from_slice(&request);
            if let Some(metrics) = self.retry_metrics.take() {
                metrics.upstream(
                    crate::metrics::Upstream::BackendHttp,
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

pub mod http {
    //! Small shared HTTP/1.1 wire primitives. Payload policy belongs to callers.

    pub use crate::uring::Progress;
    use std::io;

    pub(crate) const SCRATCH_SIZE: usize = 8192;
    pub(crate) const MAX_HEADERS: usize = 64;

    pub(crate) fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidInput, message)
    }
    pub(crate) fn protocol(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidData, message)
    }
    pub(crate) fn token(byte: u8) -> bool {
        byte.is_ascii_alphanumeric() || b"!#$%&'*+-.^_`|~".contains(&byte)
    }
    pub(crate) fn value(bytes: &[u8]) -> bool {
        bytes.iter().all(|&b| b == b'\t' || (b >= 32 && b != 127))
    }
    pub(crate) fn target(bytes: &[u8]) -> bool {
        bytes.starts_with(b"/") && bytes.iter().all(|b| (33..=126).contains(b) && *b != b'#')
    }
    pub(crate) fn authority(bytes: &[u8]) -> bool {
        !bytes.is_empty()
            && bytes
                .iter()
                .all(|b| (33..=126).contains(b) && !b"/\\?#@,".contains(b))
    }
    pub(crate) fn trim(mut bytes: &[u8]) -> &[u8] {
        while matches!(bytes.first(), Some(b' ' | b'\t')) {
            bytes = &bytes[1..];
        }
        while matches!(bytes.last(), Some(b' ' | b'\t')) {
            bytes = &bytes[..bytes.len() - 1];
        }
        bytes
    }
    pub(crate) fn decimal(bytes: &[u8]) -> io::Result<u64> {
        if bytes.is_empty() {
            return Err(protocol("empty decimal"));
        }
        bytes.iter().try_fold(0u64, |n, &b| {
            if !b.is_ascii_digit() {
                return Err(protocol("invalid decimal"));
            }
            n.checked_mul(10)
                .and_then(|n| n.checked_add((b - b'0') as u64))
                .ok_or_else(|| protocol("decimal overflow"))
        })
    }

    #[derive(Clone, Copy, Default)]
    pub(crate) struct Span {
        pub(crate) start: u16,
        pub(crate) end: u16,
    }
    impl Span {
        pub(crate) fn slice(self, bytes: &[u8]) -> &[u8] {
            &bytes[self.start as usize..self.end as usize]
        }
    }
    #[derive(Clone, Copy, Default)]
    pub(crate) struct Header {
        pub(crate) name: Span,
        pub(crate) value: Span,
    }

    /// Borrowed header views. Names compare ASCII-insensitively; values are bytes.
    /// Duplicate fields remain visible through `iter`.
    #[derive(Clone, Copy)]
    pub struct Headers<'a> {
        pub(crate) bytes: &'a [u8],
        pub(crate) headers: &'a [Header],
    }
    impl<'a> Headers<'a> {
        pub fn iter(self) -> impl Iterator<Item = (&'a str, &'a [u8])> {
            self.headers.iter().map(move |h| {
                (
                    std::str::from_utf8(h.name.slice(self.bytes)).expect("validated ASCII name"),
                    h.value.slice(self.bytes),
                )
            })
        }
        pub fn get(self, name: &str) -> Option<&'a [u8]> {
            self.iter()
                .find(|(n, _)| n.eq_ignore_ascii_case(name))
                .map(|(_, v)| v)
        }
    }

    pub(crate) fn transfer(n: usize, remaining: usize) -> io::Result<usize> {
        if n == 0 {
            Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "incomplete HTTP exchange",
            ))
        } else if n > remaining {
            Err(protocol("I/O exceeded requested range"))
        } else {
            Ok(n)
        }
    }

    // Scan each byte at most once, retaining three boundary bytes across receives.
    pub(crate) fn header_end(bytes: &[u8], scan: &mut usize) -> Option<usize> {
        while *scan + 4 <= bytes.len() {
            let i = *scan;
            *scan += 1;
            if bytes[i..i + 4] == *b"\r\n\r\n" {
                return Some(i + 4);
            }
        }
        None
    }
    pub(crate) fn line(bytes: &[u8], start: usize, end: usize) -> io::Result<usize> {
        bytes[start..end]
            .windows(2)
            .position(|s| s == b"\r\n")
            .map(|n| start + n)
            .ok_or_else(|| protocol("unterminated HTTP line"))
    }

    pub(crate) fn field(bytes: &[u8], start: usize, stop: usize) -> io::Result<Header> {
        let colon = bytes[start..stop]
            .iter()
            .position(|&b| b == b':')
            .map(|n| n + start)
            .ok_or_else(|| protocol("missing header colon"))?;
        let name = &bytes[start..colon];
        let raw = &bytes[colon + 1..stop];
        if name.is_empty() || !name.iter().copied().all(token) || !value(raw) {
            return Err(protocol("invalid HTTP header"));
        }
        let v = trim(raw);
        let vstart = v.as_ptr() as usize - bytes.as_ptr() as usize;
        Ok(Header {
            name: Span {
                start: start as u16,
                end: colon as u16,
            },
            value: Span {
                start: vstart as u16,
                end: (vstart + v.len()) as u16,
            },
        })
    }

    pub(crate) fn connection_close(bytes: &[u8]) -> io::Result<bool> {
        let mut close = false;
        for part in bytes.split(|&b| b == b',') {
            let part = trim(part);
            if part.eq_ignore_ascii_case(b"close") {
                close = true;
            } else if !part.eq_ignore_ascii_case(b"keep-alive") {
                return Err(protocol("unsupported Connection option"));
            }
        }
        Ok(close)
    }
}

pub mod breaker {
    //! Worker-local endpoint/connection health. Owned permits allow healthy requests
    //! to overlap without borrowing the breaker across asynchronous operations.
    use std::{
        cell::RefCell,
        rc::Rc,
        time::{Duration, Instant},
    };

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Rejected;
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub(crate) enum Status {
        Closed,
        Open,
        HalfOpen,
    }
    enum Phase {
        Closed,
        Open(Option<Instant>),
        Probe,
    }
    struct State {
        phase: Phase,
        active: usize,
        generation: Rc<()>,
        cooldown: Duration,
    }
    struct Inner {
        state: RefCell<State>,
        clock: Box<dyn Fn() -> Instant>,
    }

    /// Clones share one failure scope. Separate origins and RDMA connections get
    /// separate breakers; logical shards using one transport share its breaker.
    #[derive(Clone)]
    pub struct CircuitBreaker {
        inner: Rc<Inner>,
    }
    impl CircuitBreaker {
        /// Observing a cooldown never starts a probe or changes admission.
        pub(crate) fn status(&self) -> Status {
            match self.inner.state.borrow().phase {
                Phase::Closed => Status::Closed,
                Phase::Open(_) => Status::Open,
                Phase::Probe => Status::HalfOpen,
            }
        }
        pub(crate) fn active(&self) -> usize {
            self.inner.state.borrow().active
        }
        pub(crate) fn available(&self) -> bool {
            matches!(self.inner.state.borrow().phase, Phase::Closed)
                || matches!(self.inner.state.borrow().phase, Phase::Open(Some(at)) if (self.inner.clock)() >= at)
        }
        pub(crate) fn evictable(&self) -> bool {
            Rc::strong_count(&self.inner) == 1 && self.available()
        }
        pub fn new(cooldown: Duration) -> Self {
            Self::with_clock(cooldown, crate::environment::now)
        }
        pub fn with_clock(cooldown: Duration, clock: impl Fn() -> Instant + 'static) -> Self {
            Self {
                inner: Rc::new(Inner {
                    state: RefCell::new(State {
                        phase: Phase::Closed,
                        active: 0,
                        generation: Rc::new(()),
                        cooldown,
                    }),
                    clock: Box::new(clock),
                }),
            }
        }
        pub fn try_acquire(&self) -> Result<Permit, Rejected> {
            let now = (self.inner.clock)();
            let mut state = self.inner.state.borrow_mut();
            let probe = match state.phase {
                Phase::Closed => false,
                Phase::Open(Some(at)) if now >= at => {
                    state.phase = Phase::Probe;
                    true
                }
                _ => return Err(Rejected),
            };
            state.active += 1;
            Ok(Permit {
                inner: self.inner.clone(),
                generation: state.generation.clone(),
                probe,
                completed: false,
            })
        }
    }

    /// Non-clone completion authority tied to one breaker generation. Old completions
    /// cannot reset a newer failure. Dropping a probe returns it to cooldown; dropping
    /// an ordinary healthy request leaves health unchanged.
    /// ```compile_fail
    /// use racer_dataplane::breaker::Permit;
    /// fn twice(p: Permit) { p.success(); p.failure(); }
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::breaker::Permit;
    /// fn duplicate(p: Permit) { let _ = p.clone(); }
    /// ```
    #[must_use = "complete the request with success/failure, or drop to cancel"]
    pub struct Permit {
        inner: Rc<Inner>,
        generation: Rc<()>,
        probe: bool,
        completed: bool,
    }
    impl Permit {
        pub(crate) fn current(&self) -> bool {
            Rc::ptr_eq(&self.inner.state.borrow().generation, &self.generation)
        }
        pub fn success(mut self) {
            self.finish(true);
        }
        pub fn failure(mut self) {
            self.finish(false);
        }
        fn finish(&mut self, success: bool) {
            self.completed = true;
            let mut state = self.inner.state.borrow_mut();
            if !Rc::ptr_eq(&state.generation, &self.generation) {
                return;
            }
            if !success {
                state.phase = Phase::Open((self.inner.clock)().checked_add(state.cooldown));
                state.generation = Rc::new(());
            } else if self.probe {
                state.phase = Phase::Closed;
                state.generation = Rc::new(());
            }
        }
    }
    impl Drop for Permit {
        fn drop(&mut self) {
            if !self.completed && self.probe {
                self.finish(false);
            }
            self.inner.state.borrow_mut().active -= 1;
        }
    }

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/http/breaker.rs"
    ));
}
