// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local RC SEND/RECV envelopes and immutable plaintext READs. Attach one
//! [`Source`] per worker/RNIC to uring; authenticate offers over HTTP before use.
//! Only `Session::take_offer` authorizes activation; transport failure selects HTTP.
//! Type-2B windows are QP-bound/read-only with private backing MR keys. BIND CQE
//! precedes advertisement, READ CQE generates ACK, and ACK + INV CQE releases Buffer.
//! Ambiguous failures destroy the QP before releasing DMA; failed destruction leaks
//! the retained owner. Forgotten tickets still expire. `Connected` has no data API
//! until its exact session is installed. RCR4 controls bind sequence, correlation,
//! direction and Ed25519; grants/ACKs carry CRC64, verified before cache publication.
//! Mutual v2 adds descriptor-bound negative replies without advertising memory.
//! Linux needs libibverbs development files, a C compiler and ar; tests need no RNIC.

use crate::{
    buffers::{
        BUFFER_SIZE, Buffer, Destination, Fill, Key, MemoryLease, WorkerPool, Writable,
        WritableStorage,
    },
    crypto, uring,
};
use std::{
    cell::{Cell, RefCell, UnsafeCell},
    ffi::{CStr, CString, c_char, c_void},
    io,
    marker::PhantomData,
    os::fd::{FromRawFd, OwnedFd},
    ptr,
    rc::Rc,
    time::{Duration, Instant},
};

const CONTROL: usize = 4096;
use crate::negotiation::control_wire::{Frame, HEADER};
/// Maximum opaque RPC metadata (for example, encoded HTTP headers).
const AUTH_OVERHEAD: usize = 112;
pub const MAX_METADATA: usize = CONTROL - HEADER - AUTH_OVERHEAD;

mod ffi {
    use super::*;
    #[repr(C)]
    #[derive(Clone, Copy)]
    pub struct Rail {
        pub name: [c_char; 64],
        pub gid: [u8; 16],
        pub max_read: u32,
        pub lid: u16,
        pub port: u8,
        pub gid_index: u8,
        pub mtu: u8,
        pub ethernet: u8,
        pub windows: u8,
        pub pad: u8,
    }
    pub(crate) use crate::negotiation::Endpoint;
    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    pub struct Wc {
        pub id: u64,
        pub status: u32,
        pub opcode: u32,
        pub len: u32,
        pub qpn: u32,
    }
    unsafe extern "C" {
        pub fn racer_discover(out: *mut Rail, capacity: i32) -> i32;
        pub fn racer_open(
            name: *const c_char,
            pool: *mut u8,
            pool_len: usize,
            control: *mut u8,
            control_len: usize,
            cqe: i32,
            out: *mut *mut c_void,
        ) -> i32;
        pub fn racer_close(d: *mut c_void) -> i32;
        pub fn racer_fd(d: *mut c_void, asynchronous: i32) -> i32;
        pub fn racer_notify(d: *mut c_void) -> i32;
        pub fn racer_event(d: *mut c_void, asynchronous: i32, qpn: *mut u32) -> i32;
        pub fn racer_poll(d: *mut c_void, out: *mut Wc, capacity: i32) -> i32;
        pub fn racer_qp(
            d: *mut c_void,
            depth: u32,
            rail: *const Rail,
            psn: u32,
            out: *mut Endpoint,
        ) -> *mut c_void;
        pub fn racer_init(qp: *mut c_void, port: u8) -> i32;
        pub fn racer_connect(
            qp: *mut c_void,
            rail: *const Rail,
            peer: *const Endpoint,
            psn: u32,
            reads: u8,
        ) -> i32;
        pub fn racer_destroy_qp(d: *mut c_void, qp: *mut c_void) -> i32;
        pub fn racer_destroy_qps(d: *mut c_void, qps: *mut *mut c_void, count: usize) -> i32;
        pub fn racer_window(d: *mut c_void, key: *mut u32) -> *mut c_void;
        pub fn racer_free_windows(d: *mut c_void, windows: *mut *mut c_void, count: usize) -> i32;
        pub fn racer_post(
            d: *mut c_void,
            qp: *mut c_void,
            op: u32,
            id: u64,
            address: *mut u8,
            len: u32,
            remote: u64,
            key: u32,
            mw: *mut c_void,
        ) -> i32;
    }
}

fn error(kind: io::ErrorKind, message: &'static str) -> io::Error {
    io::Error::new(kind, message)
}
use crate::negotiation::control_wire::{
    argument_error as invalid, capacity_error as full, protocol_error as protocol,
};
fn check(code: i32) -> io::Result<()> {
    if code == 0 {
        Ok(())
    } else {
        Err(io::Error::from_raw_os_error(code.abs()))
    }
}
use crate::negotiation::transport_nonce as random;

// Process-start policy. Physical catalogs never refresh in a live process:
// authenticated offers and draining DMA owners retain their original indices.
use std::env;

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
struct Selector {
    device: String,
    port: u8,
    gid: u8,
}
impl Selector {
    fn parse(value: &str) -> io::Result<Self> {
        let parts: Vec<_> = value.split(':').collect();
        if parts.len() != 3
            || parts[0].is_empty()
            || parts[0].len() >= 64
            || !parts[0]
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"_-.".contains(&b))
        {
            return Err(bad(
                "RACER_RDMA_RAILS requires device:port:gid_index entries",
            ));
        }
        let port = parts[1]
            .parse::<u8>()
            .map_err(|_| bad("invalid RDMA port"))?;
        let gid = parts[2]
            .parse::<u8>()
            .map_err(|_| bad("invalid RDMA GID index"))?;
        if port == 0 {
            return Err(bad("RDMA port must be positive"));
        }
        Ok(Self {
            device: parts[0].into(),
            port,
            gid,
        })
    }
}
fn bad(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}

/// Explicit opt-in, immutable selectors and bounded per-worker/rail capacity.
/// Disabled policy never invokes discovery or registration. Opt-in registration
/// waits for a configuration with both fabric and mutual authentication enabled.
#[derive(Clone, Debug)]
pub struct StartupPolicy {
    selectors: Vec<Selector>,
    pub(crate) transport: Config,
}
impl StartupPolicy {
    pub fn from_env() -> io::Result<Self> {
        Self::parse(|name| env::var(name))
    }
    pub(crate) fn parse(
        mut lookup: impl FnMut(&str) -> Result<String, env::VarError>,
    ) -> io::Result<Self> {
        let mut get = |name| match lookup(name) {
            Ok(v) => Ok(Some(v)),
            Err(env::VarError::NotPresent) => Ok(None),
            Err(e) => Err(bad(&format!("invalid {name}: {e}"))),
        };
        let mode = get("RACER_RDMA_MODE")?.unwrap_or_else(|| "disabled".into());
        let rails = get("RACER_RDMA_RAILS")?;
        let connections = get("RACER_RDMA_CONNECTIONS")?;
        let depth = get("RACER_RDMA_DEPTH")?;
        if mode == "disabled" {
            if rails.is_some() || connections.is_some() || depth.is_some() {
                return Err(bad(
                    "RDMA settings require RACER_RDMA_MODE=enabled; unset them for HTTP-only",
                ));
            }
            return Ok(Self {
                selectors: Vec::new(),
                transport: Config::default(),
            });
        }
        if mode != "enabled" {
            return Err(bad("RACER_RDMA_MODE must be disabled or enabled"));
        }
        let mut selectors = rails
            .ok_or_else(|| bad("enabled RDMA requires RACER_RDMA_RAILS"))?
            .split(',')
            .map(Selector::parse)
            .collect::<io::Result<Vec<_>>>()?;
        selectors.sort();
        if selectors.len() > crate::peer_identity::MAX_RAILS as usize
            || selectors.windows(2).any(|s| s[0] == s[1])
        {
            return Err(bad("duplicate or excessive RDMA selectors"));
        }
        let count = |value: Option<String>, default, max, name| -> io::Result<usize> {
            let n = value.map_or(Ok(default), |v| v.parse().map_err(|_| bad(name)))?;
            if !(1..=max).contains(&n) {
                return Err(bad(name));
            }
            Ok(n)
        };
        let transport = Config {
            connections: count(connections, 8, 32, "RACER_RDMA_CONNECTIONS must be 1..=32")?,
            depth: count(depth, 2, 16, "RACER_RDMA_DEPTH must be 1..=16")?,
            ..Config::default()
        };
        Ok(Self {
            selectors,
            transport,
        })
    }
    /// Conservative process-wide envelope, counting missing rails as provisioned.
    /// 65,536 windows / 256 MiB control storage maximum; payload registrations
    /// additionally cover the worker's entire NUMA payload pool on each rail.
    pub fn validate_workers(&self, workers: usize) -> io::Result<usize> {
        let slots = workers
            .checked_mul(self.selectors.len())
            .and_then(|n| n.checked_mul(self.transport.connections))
            .and_then(|n| n.checked_mul(self.transport.depth * 4))
            .ok_or_else(|| bad("RDMA provisioning budget overflow"))?;
        if slots > 65536 {
            return Err(bad(
                "RDMA exceeds 65536 process-wide windows / 256 MiB control budget; reduce workers, rails, connections or depth",
            ));
        }
        Ok(slots)
    }
    pub fn catalog(&self) -> Vec<Option<Rail>> {
        self.catalog_with(discover)
    }
    fn catalog_with(&self, discover: impl FnOnce() -> io::Result<Vec<Rail>>) -> Vec<Option<Rail>> {
        if self.selectors.is_empty() {
            eprintln!(
                "RDMA disabled: HTTP-only; set RACER_RDMA_MODE=enabled and RACER_RDMA_RAILS then restart to enable"
            );
            return Vec::new();
        }
        let candidates = discover().unwrap_or_else(|e| {
            eprintln!("RDMA discovery failed: {e}; using HTTP; correct device access and restart");
            Vec::new()
        });
        self.selectors.iter().enumerate().map(|(index, selector)| {
            let rail = candidates.iter().find(|r| r.name == selector.device
                && r.raw.port == selector.port && r.raw.gid_index == selector.gid).cloned();
            eprintln!("RDMA rail {index} {}:{}:{}: {}; recovery=restart (no live rediscovery)",
                selector.device, selector.port, selector.gid,
                if rail.is_some() { "selected; registration deferred until fabric and authentication enabled" }
                else { "missing/unsupported; HTTP fallback; correct port/GID/device access then restart" });
            rail
        }).collect()
    }
    pub(crate) fn eligible(fabric: bool, authentication: bool) -> bool {
        fabric && authentication
    }
    pub fn control_bytes(&self, workers: usize) -> io::Result<usize> {
        Ok(self.validate_workers(workers)? * CONTROL)
    }
}

/// Active device/port/GID candidates, ordered by that tuple. Ethernet requires
/// RoCEv2. Discovery is a snapshot, not proof of reachability or future health.
#[derive(Clone)]
pub struct Rail {
    raw: ffi::Rail,
    pub name: String,
    pub numa_node: Option<usize>,
}
pub fn discover() -> io::Result<Vec<Rail>> {
    let mut raw = vec![unsafe { std::mem::zeroed::<ffi::Rail>() }; 65536];
    // SAFETY: output capacity matches allocation; shim copies POD descriptors.
    let n = unsafe { ffi::racer_discover(raw.as_mut_ptr(), raw.len() as i32) };
    if n < 0 {
        return Err(io::Error::from_raw_os_error(-n));
    }
    let mut rails = Vec::new();
    for raw in raw
        .into_iter()
        .take(n as usize)
        .filter(|r| r.windows != 0 && r.max_read != 0)
    {
        // SAFETY: shim zero-initializes and terminates the name.
        let name = unsafe { CStr::from_ptr(raw.name.as_ptr()) }
            .to_string_lossy()
            .into_owned();
        let numa_node =
            std::fs::read_to_string(format!("/sys/class/infiniband/{name}/device/numa_node"))
                .ok()
                .and_then(|s| s.trim().parse::<usize>().ok());
        rails.push(Rail {
            raw,
            name,
            numa_node,
        });
    }
    rails.sort_by(|a, b| {
        (&a.name, a.raw.port, a.raw.gid_index).cmp(&(&b.name, b.raw.port, b.raw.gid_index))
    });
    Ok(rails)
}

pub use crate::crypto::auth::AuthenticatedOffer;
pub use crate::negotiation::{Offer, TransportConfig as Config, rails_for_shard};

struct Book {
    slots: Vec<Cell<(u64, bool)>>,
}
/// A ticket never owns a DMA buffer. Dropping an unknown/granted outcome asks the
/// driver to fail the session. A known negative waits for SEND retirement only.
/// Forgetting outstanding work is bounded by the request deadline.
/// ```compile_fail
/// use racer_dataplane::rdma::{Ticket, Read};
/// fn clone(t: Ticket<Read>) { let _ = t.clone(); }
/// ```
pub struct Ticket<T> {
    book: Rc<Book>,
    index: usize,
    generation: u64,
    active: bool,
    _kind: PhantomData<T>,
}
pub enum Grant {}
pub use crate::http_client::attempt::PeerFailure;
pub enum GrantReply {
    Grant(RemoteGrant),
    Failure(PeerFailure),
}
pub struct Read<B: Writable = Fill>(PhantomData<B>);
/// Restricted RDMA read ticket; it cannot be collected as a raw Fill or Buffer.
pub type DestinationRead = Read<Destination>;

/// Sealed by Writable. Raw Fill reads validate CRC before publication;
/// restricted reads return only the destination and the completed byte length.
pub trait ReadBuffer: Writable {
    type Completed;
    #[doc(hidden)]
    fn complete(self, len: usize, checksum: u64) -> io::Result<Self::Completed>;
}
impl ReadBuffer for Fill {
    type Completed = Buffer;
    fn complete(mut self, len: usize, checksum: u64) -> io::Result<Buffer> {
        if len > self.as_mut_slice().len()
            || crate::allocator::crc64(&self.as_mut_slice()[..len]) != checksum
        {
            return Err(invalid());
        }
        self.publish_checked(len, checksum)
    }
}
impl ReadBuffer for Destination {
    type Completed = (Destination, usize);
    fn complete(self, len: usize, _checksum: u64) -> io::Result<Self::Completed> {
        Ok((self, len))
    }
}
impl<T> Drop for Ticket<T> {
    fn drop(&mut self) {
        let cell = &self.book.slots[self.index];
        if self.active && cell.get().0 == self.generation {
            cell.set((self.generation, true));
        }
    }
}
/// Single-use, connection-bound capability, created only from a validated RPC
/// response. No raw rkey/address constructor exists.
/// ```compile_fail
/// use racer_dataplane::rdma::RemoteGrant;
/// fn duplicate(g: RemoteGrant) { let _ = g.clone(); }
/// ```
pub struct RemoteGrant {
    ticket: Ticket<Grant>,
    checksum: Option<u64>,
}
impl RemoteGrant {
    /// Authenticated original CRC, available before READ. Received plaintext must
    /// still match this CRC and the cache's expected identity before publication.
    pub fn checksum(&self) -> Option<u64> {
        self.checksum
    }
}
/// One incoming authenticated request. The value identity includes namespace,
/// version and page; it must name the Buffer supplied to `respond`.
pub struct Request {
    ticket: Ticket<Grant>,
    pub value: [u8; 32],
    pub len: usize,
    pub metadata: Vec<u8>,
}

/// Unique pending QP owner. Dropping it cancels negotiation and initiates
/// quiescence. Successful `connect` transfers that ownership to `Connected`.
/// ```compile_fail
/// use racer_dataplane::rdma::{Connecting, AuthenticatedOffer};
/// fn reuse(c: Connecting, a: AuthenticatedOffer, b: AuthenticatedOffer) {
///     let _ = c.connect(a, 0);
///     let _ = c.connect(b, 0);
/// }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connecting;
/// fn send(c: Connecting) { std::thread::spawn(move || drop(c)); }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connecting;
/// fn sync<T: Sync>() {}
/// sync::<Connecting>();
/// ```
pub struct Connecting {
    transport: Transport,
    index: usize,
    serial: u64,
    offer: Offer,
    cancelled: Rc<Cell<bool>>,
    armed: bool,
}
/// Connected QP awaiting control authentication. No data-plane methods are
/// available until consuming `authenticate_session` succeeds. Drop or failed
/// activation initiates quiescence and retains DMA on destruction failure.
/// ```compile_fail
/// use racer_dataplane::rdma::Connected;
/// fn premature(c: &Connected) { let _ = c.request([0; 32], 4, &[]); }
/// ```
/// ```compile_fail
/// use racer_dataplane::{buffers::Fill, rdma::{Connected, RemoteGrant}};
/// fn premature(c: &Connected, g: RemoteGrant, f: Fill) { let _ = c.read(g, f); }
/// ```
/// ```compile_fail
/// use racer_dataplane::{buffers::Buffer, rdma::{Connected, Request}};
/// fn premature(c: &Connected, r: Request, b: Buffer) { let _ = c.respond(r, b); }
/// ```
/// ```compile_fail
/// use racer_dataplane::{crypto::{Snapshot, auth::Session}, rdma::Connected};
/// fn reuse(c: Connected, a: Session, b: Session, s: Snapshot) {
///     let _ = c.authenticate_session(a, s.clone());
///     let _ = c.authenticate_session(b, s);
/// }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connected;
/// fn duplicate(c: Connected) { let _ = c.clone(); }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connected;
/// fn send(c: Connected) { std::thread::spawn(move || drop(c)); }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connected;
/// fn sync<T: Sync>() {}
/// sync::<Connected>();
/// ```
#[must_use]
pub struct Connected {
    transport: Transport,
    index: usize,
    serial: u64,
    cancelled: Rc<Cell<bool>>,
    armed: bool,
}

/// Authenticated live QP owner; share locally with `Rc<Connection>`. Last-owner drop
/// initiates quiescence; failed destruction retains DMA in the transport owner.
/// ```compile_fail
/// use racer_dataplane::rdma::Connection;
/// fn duplicate(c: Connection) { let _ = c.clone(); }
/// ```
/// ```compile_fail
/// use racer_dataplane::{crypto::{Snapshot, auth::Session}, rdma::Connection};
/// fn reinstall(c: Connection, s: Session, policy: Snapshot) {
///     let _ = c.authenticate_session(s, policy);
/// }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connection;
/// fn send(c: Connection) { std::thread::spawn(move || drop(c)); }
/// ```
/// ```compile_fail
/// use racer_dataplane::rdma::Connection;
/// fn sync<T: Sync>() {}
/// sync::<Connection>();
/// ```
pub struct Connection {
    transport: Transport,
    index: usize,
    serial: u64,
    cancelled: Rc<Cell<bool>>,
}
impl Connecting {
    pub fn offer(&self) -> &Offer {
        &self.offer
    }
    /// Cancel through a shared reference, including an `Rc` runtime owner.
    /// A `WouldBlock` result still records cancellation for driver progress.
    pub fn cancel(&self) -> io::Result<()> {
        self.transport
            .disconnect(self.index, self.serial, &self.cancelled)
    }
    pub fn close(self) -> io::Result<()> {
        self.cancel()
    }
    pub fn connect(mut self, peer: AuthenticatedOffer, shard: u64) -> io::Result<Connected> {
        let (peer, binding) = peer.into_parts();
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let c = core.connection(self.index, self.serial)?;
        if core.stopped
            || c.failed
            || c.cancelled.get()
            || c.ready
            || c.qp.is_null()
            || crate::environment::now() >= c.deadline
        {
            return Err(error(
                io::ErrorKind::NotConnected,
                "RDMA handshake expired or closed",
            ));
        }
        if peer.fabric != self.offer.fabric
            || peer.challenge != self.offer.challenge
            || peer.nonce == self.offer.nonce
            || peer.endpoint.ethernet != self.offer.endpoint.ethernet
            || rails_for_shard(shard, self.offer.rails as usize, peer.rails as usize)
                != Some((self.offer.rail as usize, peer.rail as usize))
        {
            return Err(invalid());
        }
        let qp = c.qp;
        // SAFETY: QP is owned and INIT; endpoint was structurally parsed and authenticated.
        let result = core.connect_qp(
            qp,
            &peer.endpoint,
            self.offer.endpoint.psn,
            peer.reads.min(self.offer.reads),
        );
        if result != 0 {
            core.fail(self.index, io::ErrorKind::ConnectionAborted)?;
            return Err(io::Error::from_raw_os_error(result));
        }
        let c = &mut core.connections[self.index];
        c.peer = peer.nonce;
        c.binding = Some(binding);
        #[cfg(test)]
        {
            c.remote_endpoint = Some(peer.endpoint);
        }
        c.ready = true;
        self.armed = false;
        Ok(Connected {
            transport: self.transport.clone(),
            index: self.index,
            serial: self.serial,
            cancelled: self.cancelled.clone(),
            armed: true,
        })
    }
    /// Activate only the offer from this session and install control protection
    /// before returning a connection usable by the application.
    pub fn connect_authenticated(
        self,
        mut session: crypto::auth::Session,
        snapshot: crypto::Snapshot,
        shard: u64,
    ) -> io::Result<Connection> {
        let offer = session.take_offer(&snapshot)?.ok_or_else(invalid)?;
        self.connect(offer, shard)?
            .authenticate_session(session, snapshot)
    }
}
impl Drop for Connecting {
    fn drop(&mut self) {
        if self.armed {
            self.transport
                .drop_connection(self.index, self.serial, &self.cancelled);
        }
    }
}
impl Drop for Connection {
    fn drop(&mut self) {
        self.transport
            .drop_connection(self.index, self.serial, &self.cancelled);
    }
}
impl Drop for Connected {
    fn drop(&mut self) {
        if self.armed {
            self.transport
                .drop_connection(self.index, self.serial, &self.cancelled);
        }
    }
}

#[derive(Clone)]
/// Worker-local submission handle; it cannot cross worker threads.
/// ```compile_fail
/// use racer_dataplane::rdma::Transport;
/// fn send(t: Transport) { std::thread::spawn(move || drop(t)); }
/// ```
pub struct Transport {
    owner: Rc<RefCell<Owner>>,
}
pub struct Source {
    transport: Transport,
    files: Option<[uring::File; 2]>,
    polls: [Option<uring::Ticket<uring::Control>>; 2],
}

struct Session {
    #[cfg(test)]
    endpoint: ffi::Endpoint,
    #[cfg(test)]
    remote_endpoint: Option<ffi::Endpoint>,
    local_renewal: bool,
    binding: Option<[u8; 32]>,
    auth: Option<(crypto::auth::Session, crypto::Snapshot)>,
    authenticated_received: bool,
    confirmation: Confirmation,
    qp: *mut c_void,
    serial: u64,
    local: [u8; 16],
    peer: [u8; 16],
    qpn: u32,
    ready: bool,
    failed: bool,
    cleanup_after: Option<Instant>,
    cancelled: Rc<Cell<bool>>,
    deadline: Instant,
    next_request: u64,
    last_request: u64,
    next_grant: u64,
}
impl Session {
    fn new(
        qp: *mut c_void,
        serial: u64,
        local: [u8; 16],
        endpoint: ffi::Endpoint,
        deadline: Instant,
    ) -> Self {
        Self {
            #[cfg(test)]
            endpoint,
            #[cfg(test)]
            remote_endpoint: None,
            local_renewal: false,
            binding: None,
            auth: None,
            authenticated_received: false,
            confirmation: Confirmation::None,
            qp,
            serial,
            local,
            peer: [0; 16],
            qpn: endpoint.qpn,
            ready: false,
            failed: false,
            cleanup_after: None,
            cancelled: Rc::new(Cell::new(false)),
            deadline,
            next_request: 0,
            last_request: 0,
            next_grant: 0,
        }
    }
}
// HTTP negotiation starts confirmation before exposing the connection. None
// denotes a transport session whose HTTP confirmation has not been started.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Confirmation {
    None,
    AwaitConfirm,
    AwaitAck,
    Complete,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum Phase {
    Free,
    Receive,
    ConfirmSend,
    RequestSend,
    AwaitGrant,
    GrantReady,
    FailureReady,
    FailureSend,
    Incoming,
    Claimed,
    Bind,
    Advertise,
    AwaitAck,
    Invalidate,
    Reading,
    Ack,
    Done,
    Failed,
}
impl Phase {
    fn outgoing(self) -> bool {
        matches!(
            self,
            Self::RequestSend
                | Self::AwaitGrant
                | Self::GrantReady
                | Self::FailureReady
                | Self::Reading
                | Self::Ack
                | Self::Done
                | Self::Failed
        )
    }
    fn incoming(self) -> bool {
        matches!(
            self,
            Self::Incoming
                | Self::Claimed
                | Self::Bind
                | Self::Advertise
                | Self::AwaitAck
                | Self::Invalidate
                | Self::FailureSend
        )
    }
}
struct Slot {
    send_pending: bool,
    signed: bool,
    descriptor: [u8; 32],
    negative: Option<PeerFailure>,
    checksum: Option<u64>,
    wire_len: usize,
    phase: Phase,
    conn: usize,
    generation: u64,
    wr: u64,
    opcode: u32,
    deadline: Instant,
    frame: Frame,
    fill: Option<WritableStorage>,
    buffer: Option<Buffer>,
    mw: *mut c_void,
    key: u32,
    uses: u16,
    #[cfg(test)]
    binding: Option<(u64, u32)>,
    failure: io::ErrorKind,
    early: Option<Frame>,
    tracked: bool,
}
impl Slot {
    fn new(now: Instant) -> Self {
        Self {
            send_pending: false,
            signed: false,
            descriptor: [0; 32],
            negative: None,
            checksum: None,
            wire_len: 0,
            phase: Phase::Free,
            conn: 0,
            generation: 0,
            wr: 0,
            opcode: 0,
            deadline: now,
            frame: Frame::default(),
            fill: None,
            buffer: None,
            mw: ptr::null_mut(),
            key: 0,
            uses: 0,
            #[cfg(test)]
            binding: None,
            failure: io::ErrorKind::ConnectionAborted,
            early: None,
            tracked: false,
        }
    }
    #[cfg(test)]
    fn owns_dma(&self) -> bool {
        self.fill.is_some() || self.buffer.is_some()
    }
}
// No Rust reference to NIC-owned bytes exists between post and completion.
// UnsafeCell also permits shared ownership of the allocation during DMA.
struct ControlArena(Box<[UnsafeCell<[u8; CONTROL]>]>);
impl ControlArena {
    fn new(count: usize) -> Self {
        Self((0..count).map(|_| UnsafeCell::new([0; CONTROL])).collect())
    }
    fn pointer(&self, i: usize) -> *mut u8 {
        self.0[i].get().cast()
    }
    /// Caller must have observed completion (or never posted) for this slot.
    unsafe fn bytes(&self, i: usize) -> &[u8; CONTROL] {
        unsafe { &*self.0[i].get() }
    }
    /// Caller must hold this slot exclusively, with no outstanding DMA.
    unsafe fn bytes_mut(&mut self, i: usize) -> &mut [u8; CONTROL] {
        unsafe { &mut *self.0[i].get() }
    }
}
struct Core {
    metrics: crate::metrics::Local,
    device: *mut c_void,
    lease: MemoryLease,
    control: ControlArena,
    rail: Rail,
    config: Config,
    connections: Vec<Session>,
    slots: Vec<Slot>,
    free: Vec<usize>,
    book: Rc<Book>,
    serial: u64,
    wr: u64,
    stopped: bool,
    renewing: bool,
    renew_after: Option<Instant>,
    // Helpers exclusively access these stable tables until successful join.
    // Owner's leak-on-failure retains them along with all provider/DMA owners.
    retiring_qps: Option<Box<[UnsafeCell<*mut c_void>]>>,
    retiring_windows: Option<Box<[UnsafeCell<*mut c_void>]>>,
    #[cfg(test)]
    simulation: Option<tests::Simulation>,
}
// Core is always behind this leak-on-failed-cleanup owner. Never put raw DMA
// owners in a local temporary whose destructor can run before quiescence.
struct Owner {
    core: Option<Box<Core>>,
}
impl Owner {
    fn core(&mut self) -> io::Result<&mut Core> {
        self.core
            .as_deref_mut()
            .ok_or_else(|| error(io::ErrorKind::NotConnected, "RDMA source closed"))
    }
    fn shutdown(&mut self) -> io::Result<()> {
        if let Some(core) = self.core.as_mut() {
            core.shutdown()?;
        }
        self.core.take();
        Ok(())
    }
}
impl Drop for Owner {
    fn drop(&mut self) {
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| self.shutdown()));
        if let Some(core) = self.core.take() {
            std::mem::forget(core);
        }
        if let Err(panic) = result {
            std::mem::forget(panic);
        }
    }
}

impl Transport {
    pub fn set_metrics(&self, metrics: crate::metrics::Local) -> io::Result<()> {
        self.owner.borrow_mut().core()?.metrics = metrics;
        Ok(())
    }
    pub fn new(pool: &WorkerPool, rail: Rail, config: Config) -> io::Result<(Self, Source)> {
        config.validate()?;
        if rail.raw.windows == 0 || rail.raw.max_read == 0 {
            return Err(error(
                io::ErrorKind::Unsupported,
                "type-2B memory windows required",
            ));
        }
        // 2*depth RPC owners + 2*depth receive credits per QP. Every RPC slot
        // reserves its own SEND/READ/BIND/INV WR and control storage for cleanup.
        let count = config.connections * config.depth * 4;
        let mut owner = Owner {
            core: Some(Box::new(Core::new(pool, rail, config))),
        };
        let core = owner.core()?;
        core.connections.reserve_exact(core.config.connections);
        let name = CString::new(core.rail.name.as_str()).map_err(|_| invalid())?;
        let region = core.lease.region();
        // SAFETY: Owner retains both stable allocations through deregistration,
        // including partial initialization and unwinding.
        check(unsafe {
            ffi::racer_open(
                name.as_ptr(),
                region.address,
                region.len,
                core.control.pointer(0),
                count * CONTROL,
                count as i32,
                &mut core.device,
            )
        })?;
        // Allocate provider resources during setup, never on the RPC hot path.
        for s in &mut core.slots {
            s.mw = unsafe { ffi::racer_window(core.device, &mut s.key) };
            if s.mw.is_null() {
                return Err(io::Error::last_os_error());
            }
        }
        let duplicate = |which| -> io::Result<uring::File> {
            // SAFETY: initialized device owns live fds; duplication is independent.
            let fd =
                unsafe { libc::fcntl(ffi::racer_fd(core.device, which), libc::F_DUPFD_CLOEXEC, 0) };
            if fd < 0 {
                return Err(io::Error::last_os_error());
            }
            Ok(uring::File::new(unsafe { OwnedFd::from_raw_fd(fd) }))
        };
        let files = [duplicate(0)?, duplicate(1)?];
        let transport = Self {
            owner: Rc::new(RefCell::new(owner)),
        };
        let source = Source {
            transport: transport.clone(),
            files: Some(files),
            polls: [None, None],
        };
        Ok((transport, source))
    }

    /// The HTTP layer supplies a fresh shared challenge (authenticated in both
    /// directions), plus this RNIC's index in the advertised discovery list.
    pub fn prepare(
        &self,
        challenge: [u8; 16],
        rail: usize,
        rails: usize,
    ) -> io::Result<Connecting> {
        let fabric = self.owner.borrow_mut().core()?.config.fabric.clone();
        self.prepare_for_fabric(&fabric, challenge, rail, rails)
    }

    /// Prepare on the current fabric without changing prior offers/defaults.
    /// Caller checks eligibility; activation checks exact authenticated matching.
    pub fn prepare_for_fabric(
        &self,
        fabric: &str,
        challenge: [u8; 16],
        rail: usize,
        rails: usize,
    ) -> io::Result<Connecting> {
        if fabric.is_empty()
            || fabric.len() > u16::MAX as usize
            || challenge == [0; 16]
            || rail >= rails
            || rails > u32::MAX as usize
        {
            return Err(invalid());
        }
        let mut owner = self.owner.borrow_mut();
        let core = owner.core()?;
        // Quiescent teardown can leave retired MWs. Renew immediately when the
        // rail is idle, or when a reconnect cannot reserve its complete share.
        // Otherwise leave unrelated healthy sessions serving until renewal is
        // actually needed.
        if core.free.len() < core.config.depth * 4
            && core.slots.iter().filter(|s| s.phase == Phase::Free).count() > core.free.len()
        {
            core.renewing = true;
        }
        core.renew_windows(crate::environment::now());
        if core.renewing {
            return Err(error(
                io::ErrorKind::NotConnected,
                "RDMA window renewal pending; use HTTP",
            ));
        }
        if core.stopped {
            return Err(error(io::ErrorKind::NotConnected, "RDMA stopped"));
        }
        let index = core
            .connections
            .iter()
            .enumerate()
            .position(|(i, c)| {
                c.qp.is_null()
                    && !core
                        .slots
                        .iter()
                        .any(|s| s.phase != Phase::Free && s.conn == i)
            })
            .unwrap_or(core.connections.len());
        if index == core.config.connections {
            return Err(full());
        }
        let nonce = random()?;
        let psn = u32::from_be_bytes(nonce[..4].try_into().unwrap()) & 0xffffff;
        core.serial = core.serial.checked_add(1).ok_or_else(full)?;
        let serial = core.serial;
        let mut endpoint = ffi::Endpoint::default();
        // SAFETY: device/CQ/PD live; QP is immediately installed in retained owner.
        let qp = core.create_qp(psn, &mut endpoint);
        if qp.is_null() {
            return Err(io::Error::last_os_error());
        }
        let session = Session::new(
            qp,
            serial,
            nonce,
            endpoint,
            crate::environment::now() + core.config.timeout,
        );
        let cancelled = session.cancelled.clone();
        if index == core.connections.len() {
            core.connections.push(session);
        } else {
            core.connections[index] = session;
        }
        let setup = (|| {
            check(core.init_qp(qp))?;
            for _ in 0..core.config.depth * 2 {
                let i = core.allocate(index, Phase::Receive)?;
                core.post(i, 2)?;
            }
            Ok::<_, io::Error>(())
        })();
        if let Err(e) = setup {
            core.fail(index, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        let offer = Offer {
            version: 2,
            fabric: fabric.to_owned(),
            nonce,
            challenge,
            endpoint,
            rail: rail as u32,
            rails: rails as u32,
            reads: core
                .rail
                .raw
                .max_read
                .min(core.config.depth as u32)
                .min(255) as u8,
        };
        Ok(Connecting {
            transport: self.clone(),
            index,
            serial,
            offer,
            cancelled,
            armed: true,
        })
    }

    fn disconnect(&self, index: usize, serial: u64, cancelled: &Cell<bool>) -> io::Result<()> {
        // Independent of the RefCell: reentrant buffer/waker callbacks cannot
        // lose cancellation or panic on a borrow conflict.
        cancelled.set(true);
        let mut owner = self.owner.try_borrow_mut().map_err(|_| full())?;
        let Some(core) = owner.core.as_mut() else {
            return Ok(());
        };
        if core.connection(index, serial).is_err() {
            return Ok(()); // This generation already quiesced and was replaced.
        }
        core.fail(index, io::ErrorKind::ConnectionAborted)
    }

    fn drop_connection(&self, index: usize, serial: u64, cancelled: &Cell<bool>) {
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _ = self.disconnect(index, serial, cancelled);
        }));
        if let Err(panic) = result {
            std::mem::forget(panic);
        }
    }

    pub fn shutdown(&self) -> io::Result<()> {
        self.owner.borrow_mut().shutdown()
    }
}

impl Connected {
    /// Install the exact handshake/direction once, preserving Ready sequences.
    /// Errors retire the QP. Responders activate before sending Ready; initiators
    /// verify Ready before activation.
    pub fn authenticate_session(
        mut self,
        session: crypto::auth::Session,
        snapshot: crypto::Snapshot,
    ) -> io::Result<Connection> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let c = core.connection(self.index, self.serial)?;
        if core.stopped
            || !c.ready
            || c.failed
            || c.qp.is_null()
            || c.cancelled.get()
            || c.auth.is_some()
            || c.next_request != 0
            || c.last_request != 0
        {
            return Err(invalid());
        }
        session.matches_transport(&snapshot, c.binding.as_ref().ok_or_else(invalid)?)?;
        core.connections[self.index].auth = Some((session, snapshot));
        self.armed = false;
        Ok(Connection {
            transport: self.transport.clone(),
            index: self.index,
            serial: self.serial,
            cancelled: self.cancelled.clone(),
        })
    }

    /// Cancel pending activation through a shared runtime owner. A borrow
    /// conflict records cancellation for progress and returns `WouldBlock`.
    pub fn cancel(&self) -> io::Result<()> {
        self.transport
            .disconnect(self.index, self.serial, &self.cancelled)
    }
    pub fn close(self) -> io::Result<()> {
        self.cancel()
    }
}

impl Connection {
    /// Reserve one bounded control slot after Ready, independent of application
    /// requests. Both controls use request zero and the existing signed sequence.
    pub(crate) fn begin_confirmation(&self, initiator: bool, deadline: Instant) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        core.ready(self.index, self.serial)?;
        let c = &mut core.connections[self.index];
        if c.auth.is_none() || c.confirmation != Confirmation::None {
            return Err(protocol());
        }
        c.deadline = c.deadline.min(deadline);
        if crate::environment::now() >= c.deadline {
            return Err(error(io::ErrorKind::TimedOut, "RDMA confirmation expired"));
        }
        c.confirmation = if initiator {
            Confirmation::AwaitAck
        } else {
            Confirmation::AwaitConfirm
        };
        if initiator {
            core.confirmation_send(self.index, 5)?;
        }
        Ok(())
    }

    /// ACK receipt can precede the local SEND CQE. Admission waits for both so
    /// the confirmation arena is retired before application capacity is used.
    pub(crate) fn is_confirmed(&self) -> bool {
        self.inspect(|core, c| {
            c.confirmation == Confirmation::Complete
                && core.ready(self.index, self.serial).is_ok()
                && !core
                    .slots
                    .iter()
                    .any(|s| s.conn == self.index && s.phase == Phase::ConfirmSend)
        })
    }

    fn collect_slot<T>(&self, core: &mut Core, ticket: &mut Ticket<T>) -> io::Result<usize> {
        let i = core.validate(self.index, self.serial, ticket)?;
        if core.renewing {
            core.connections[self.index].local_renewal |= !core.connections[self.index].failed;
        }
        if core.renewing
            || core.stopped
            || core.connections[self.index].failed
            || self.cancelled.get()
        {
            core.fail(self.index, io::ErrorKind::ConnectionAborted)?;
            if !core.connections[self.index].qp.is_null() {
                // Report failure immediately, but leave the ticket/slot and DMA
                // owner in Core until destruction actually completes.
                return Err(error(
                    io::ErrorKind::ConnectionAborted,
                    "RDMA teardown pending; use HTTP",
                ));
            }
        }
        if core.slots[i].phase == Phase::Failed {
            let e = core.slots[i].failure;
            ticket.active = false;
            core.release(i);
            return Err(error(e, "RDMA operation failed; use HTTP"));
        }
        Ok(i)
    }
    fn inspect(&self, f: impl FnOnce(&Core, &Session) -> bool) -> bool {
        self.transport.owner.try_borrow().is_ok_and(|owner| {
            owner.core.as_ref().is_some_and(|core| {
                core.connection(self.index, self.serial)
                    .is_ok_and(|session| f(core, session))
            })
        })
    }
    /// Local maintenance is recovery-only, not peer failure evidence.
    pub(crate) fn needs_http_recovery(&self) -> bool {
        if self.key_draining() {
            return true;
        }
        self.transport
            .owner
            .borrow()
            .core
            .as_ref()
            .is_some_and(|core| {
                core.connection(self.index, self.serial)
                    .map_or(true, |c| c.local_renewal || (core.renewing && !c.failed))
            })
    }
    pub fn is_authenticated(&self) -> bool {
        self.inspect(|_, s| s.auth.is_some())
    }
    /// True only after a valid authenticated RDMA control was received. HTTP
    /// Ready does not count. This includes transport confirmation controls.
    pub fn authenticated_received(&self) -> bool {
        self.inspect(|_, s| s.authenticated_received)
    }
    /// Local transport health, not a peer liveness probe. False after deferred
    /// cancellation, source shutdown, or failure (also during reentrant polling).
    pub fn is_healthy(&self) -> bool {
        !self.cancelled.get()
            && self.inspect(|core, s| {
                core.ready(self.index, self.serial).is_ok()
                    && s.auth
                        .as_ref()
                        .is_none_or(|(auth, snapshot)| auth.healthy(snapshot))
            })
    }

    pub(crate) fn key_draining(&self) -> bool {
        self.inspect(|_, s| {
            s.auth
                .as_ref()
                .is_some_and(|(auth, snapshot)| !auth.admitting(snapshot))
        })
    }

    /// Idempotently stop admission/destroy QP; errors retain DMA and cancellation.
    pub fn disconnect(&self) -> io::Result<()> {
        self.transport
            .disconnect(self.index, self.serial, &self.cancelled)
    }
    /// Queue one RPC. `value` must include the complete storage identity,
    /// including version and page offset. `len` is the expected stored length.
    pub fn request(
        &self,
        value: [u8; 32],
        len: usize,
        metadata: &[u8],
    ) -> io::Result<Ticket<Grant>> {
        if self.key_draining() {
            return Err(error(
                io::ErrorKind::ConnectionAborted,
                "signing key rotated; use HTTP",
            ));
        }
        if len == 0 || len > BUFFER_SIZE || metadata.len() > MAX_METADATA {
            return Err(invalid());
        }
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        core.ready(self.index, self.serial)?;
        if core
            .slots
            .iter()
            .any(|s| s.conn == self.index && s.send_pending)
        {
            return Err(full());
        }
        if matches!(
            core.connections[self.index].confirmation,
            Confirmation::AwaitConfirm | Confirmation::AwaitAck
        ) || core
            .slots
            .iter()
            .any(|s| s.conn == self.index && s.phase == Phase::ConfirmSend)
        {
            return Err(full());
        }
        if core
            .slots
            .iter()
            .filter(|s| s.conn == self.index && s.phase.outgoing())
            .count()
            >= core.config.depth
        {
            return Err(full());
        }
        let request = core.connections[self.index]
            .next_request
            .checked_add(1)
            .ok_or_else(full)?;
        let i = core.allocate(self.index, Phase::RequestSend)?;
        core.connections[self.index].next_request = request;
        core.slots[i].descriptor = *blake3::hash(metadata).as_bytes();
        core.slots[i].frame = Frame {
            kind: 1,
            session: core.connections[self.index].local,
            request,
            value,
            len: len as u32,
            metadata: metadata.len() as u16,
            ..Frame::default()
        };
        if let Err(e) = core.encode(i, metadata).and_then(|_| core.post(i, 1)) {
            core.release(i);
            core.fail(self.index, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        Ok(core.ticket(i))
    }

    pub fn take_grant(&self, ticket: &mut Ticket<Grant>) -> io::Result<Option<RemoteGrant>> {
        match self.take_reply(ticket)? {
            Some(GrantReply::Grant(grant)) => Ok(Some(grant)),
            Some(GrantReply::Failure(failure)) => Err(io::Error::other(failure)),
            None => Ok(None),
        }
    }

    pub fn take_reply(&self, ticket: &mut Ticket<Grant>) -> io::Result<Option<GrantReply>> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let i = self.collect_slot(core, ticket)?;
        if core.slots[i].phase == Phase::FailureReady {
            let failure = core.slots[i].negative.take().ok_or_else(protocol)?;
            ticket.active = false;
            core.release(i);
            return Ok(Some(GrantReply::Failure(failure)));
        }
        if crate::environment::now() >= core.slots[i].deadline {
            core.fail(self.index, io::ErrorKind::TimedOut)?;
            return Err(error(io::ErrorKind::TimedOut, "RDMA request expired"));
        }
        if core.slots[i].phase != Phase::GrantReady {
            return Ok(None);
        }
        ticket.active = false;
        Ok(Some(GrantReply::Grant(RemoteGrant {
            ticket: core.ticket(i),
            checksum: core.slots[i].checksum,
        })))
    }

    /// Transfer destination before READ; rejection returns the same capability.
    /// Accepted DMA stays owned until quiescence; HTTP must use separate storage.
    pub fn read<B: Writable>(
        &self,
        mut grant: RemoteGrant,
        fill: B,
    ) -> Result<Ticket<Read<B>>, uring::Rejected<B>> {
        let mut owner = self.transport.owner.borrow_mut();
        let setup = (|| {
            let core = owner.core()?;
            let i = core.validate(self.index, self.serial, &grant.ticket)?;
            core.ready(self.index, self.serial)?;
            if core.slots[i].phase != Phase::GrantReady
                || crate::environment::now() >= core.slots[i].deadline
                || !fill.matches_key(Key::new(core.slots[i].frame.value))
            {
                return Err(invalid());
            }
            core.region(fill.region().region.address, fill.region().region.len)?;
            // At most the negotiated READ depth is in flight. RPC admission is
            // bounded by depth too; extra initiator atomics are queued by RC.
            Ok(i)
        })();
        let i = match setup {
            Ok(i) => i,
            Err(error) => {
                return Err(uring::Rejected {
                    error,
                    resource: fill,
                });
            }
        };
        let core = owner.core().unwrap();
        core.slots[i].fill = Some(fill.into_storage());
        core.slots[i].phase = Phase::Reading;
        if let Err(error) = core.post(i, 3) {
            // A single rejected WR never acquired the Fill.
            let resource = B::from_storage(core.slots[i].fill.take().unwrap())
                .unwrap_or_else(|_| unreachable!("sealed writable pairing"));
            core.slots[i].phase = Phase::GrantReady;
            return Err(uring::Rejected { error, resource });
        }
        grant.ticket.active = false;
        Ok(core.ticket(i))
    }

    /// Collect before deadline, after READ + ACK CQEs; READ byte_len is undefined
    /// and ignored. Fill validates CRC/publishes; Destination stays unpublished.
    /// Cache uses `take_read_unpublished` for bounded checksum execution.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{buffers::Fill, rdma::{Connection, DestinationRead, Ticket}};
    /// fn escape(c: &Connection, t: &mut Ticket<DestinationRead>) {
    ///     let _: Option<(Fill, usize)> = c.take_read_unpublished(t).unwrap();
    /// }
    /// ```
    pub fn take_read<B: ReadBuffer>(
        &self,
        ticket: &mut Ticket<Read<B>>,
    ) -> io::Result<Option<B::Completed>> {
        // Cache consumers use unpublished collection and bounded checksum work.
        let checksum = {
            let mut owner = self.transport.owner.borrow_mut();
            let core = owner.core()?;
            let i = core.validate(self.index, self.serial, ticket)?;
            core.slots[i].checksum.ok_or_else(invalid)?
        };
        self.take_read_unpublished(ticket)?
            .map(|(fill, len)| fill.complete(len, checksum))
            .transpose()
    }

    /// Complete READ/ACK without publishing into single-flight. Applications that
    /// validate stored values must use this boundary before exposing any bytes.
    pub fn take_read_unpublished<B: Writable>(
        &self,
        ticket: &mut Ticket<Read<B>>,
    ) -> io::Result<Option<(B, usize)>> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let i = self.collect_slot(core, ticket)?;
        if crate::environment::now() >= core.slots[i].deadline {
            core.fail(self.index, io::ErrorKind::TimedOut)?;
            return Err(error(io::ErrorKind::TimedOut, "RDMA read expired"));
        }
        if core.slots[i].phase != Phase::Done {
            return Ok(None);
        }
        let len = core.slots[i].frame.len as usize;
        let fill = B::from_storage(core.slots[i].fill.take().unwrap())
            .unwrap_or_else(|_| unreachable!("sealed writable pairing"));
        ticket.active = false;
        core.release(i);
        Ok(Some((fill, len)))
    }

    pub fn next_request(&self) -> io::Result<Option<Request>> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        core.ready(self.index, self.serial)?;
        let Some(i) = core
            .slots
            .iter()
            .position(|s| s.conn == self.index && s.phase == Phase::Incoming)
        else {
            return Ok(None);
        };
        core.slots[i].phase = Phase::Claimed;
        let f = core.slots[i].frame;
        Ok(Some(Request {
            ticket: core.ticket(i),
            value: f.value,
            len: f.len as usize,
            metadata: unsafe { core.control.bytes(i) }[HEADER..HEADER + f.metadata as usize]
                .to_vec(),
        }))
    }

    /// An inbound application must relinquish its cache consumer when the
    /// authenticated request/session is canceled, expired or retired.
    pub(crate) fn request_live(&self, request: &Request) -> bool {
        let mut owner = self.transport.owner.borrow_mut();
        let Ok(core) = owner.core() else {
            return false;
        };
        let Ok(i) = core.validate(self.index, self.serial, &request.ticket) else {
            return false;
        };
        core.ready(self.index, self.serial).is_ok()
            && core.slots[i].phase == Phase::Claimed
            && crate::environment::now() < core.slots[i].deadline
    }

    /// Terminal pre-grant response. No memory window is bound. The driver owns
    /// the bounded control until SEND retirement, including local queue pressure.
    pub fn respond_error(&self, mut request: Request, failure: PeerFailure) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let i = core.validate(self.index, self.serial, &request.ticket)?;
        core.ready(self.index, self.serial)?;
        let c = &core.connections[self.index];
        if c.auth.is_none() || core.slots[i].phase != Phase::Claimed {
            return Err(invalid());
        }
        let s = &mut core.slots[i];
        s.frame.kind = 4;
        s.frame.session = c.local;
        let mut metadata = s.descriptor.to_vec();
        metadata.extend(failure.encode());
        core.encode(i, &metadata)?;
        core.slots[i].phase = Phase::FailureSend;
        core.slots[i].tracked = false;
        request.ticket.active = false;
        if let Err(e) = core.post(i, 1) {
            core.fail(self.index, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        Ok(())
    }

    /// Publish the exact immutable stored value requested. Both the buffer-pool
    /// identity and the published length must match the original wire request.
    pub fn respond(
        &self,
        mut request: Request,
        buffer: Buffer,
    ) -> Result<(), uring::Rejected<Buffer>> {
        let mut owner = self.transport.owner.borrow_mut();
        let setup = (|| {
            let core = owner.core()?;
            let i = core.validate(self.index, self.serial, &request.ticket)?;
            core.ready(self.index, self.serial)?;
            if buffer.checksum().is_none() {
                return Err(invalid());
            }
            if core.slots[i].phase != Phase::Claimed
                || buffer.as_slice().len() != core.slots[i].frame.len as usize
                || crate::environment::now() >= core.slots[i].deadline
                || !buffer.matches_key(Key::new(core.slots[i].frame.value))
            {
                return Err(invalid());
            }
            core.region(buffer.region().region.address, buffer.region().region.len)?;
            if core.slots[i].uses == 255 {
                return Err(error(
                    io::ErrorKind::ConnectionAborted,
                    "memory window key exhausted; reconnect on a new transport",
                ));
            }
            let generation = core.connections[self.index]
                .next_grant
                .checked_add(1)
                .ok_or_else(full)?;
            core.connections[self.index].next_grant = generation;
            let s = &mut core.slots[i];
            s.uses += 1;
            s.key = (s.key & !255) | (s.key.wrapping_add(1) & 255);
            s.frame.kind = 2;
            s.frame.grant = generation;
            s.frame.address = buffer.region().region.address as u64;
            #[cfg(test)]
            if crate::simulation::current().is_some() {
                // Virtual MR address, not an ASLR-dependent host pointer. The
                // simulated NIC resolves it only against the live grant below.
                s.frame.address = 0x10000000 + buffer.region().index as u64 * BUFFER_SIZE as u64;
            }
            s.frame.key = s.key;
            s.frame.metadata = 0;
            s.checksum = buffer.checksum();
            Ok(i)
        })();
        let i = match setup {
            Ok(i) => i,
            Err(error) => {
                return Err(uring::Rejected {
                    error,
                    resource: buffer,
                });
            }
        };
        let core = owner.core().unwrap();
        core.slots[i].buffer = Some(buffer);
        core.slots[i].phase = Phase::Bind;
        if let Err(error) = core.post(i, 4) {
            let resource = core.slots[i].buffer.take().unwrap();
            core.slots[i].phase = Phase::Claimed;
            return Err(uring::Rejected { error, resource });
        }
        request.ticket.active = false;
        core.slots[i].tracked = false;
        Ok(())
    }

    /// Unknown/granted RPC cancellation breaks its QP, quiescing incoming READs
    /// whose completion is invisible to the server. Known negatives only retire SEND.
    pub fn cancel<T>(&self, ticket: &Ticket<T>) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let i = core.validate(self.index, self.serial, ticket)?;
        if core.slots[i].phase == Phase::FailureReady {
            core.release(i);
            return Ok(());
        }
        if core.slots[i].negative.is_some() && core.slots[i].phase == Phase::RequestSend {
            core.book.slots[i].set((core.slots[i].generation, true));
            return Ok(());
        }
        core.fail(self.index, io::ErrorKind::Interrupted)
    }
    pub fn close(self) -> io::Result<()> {
        self.disconnect()
    }
}

impl Core {
    fn confirmation_send(&mut self, conn: usize, kind: u8) -> io::Result<()> {
        let i = self.allocate(conn, Phase::ConfirmSend)?;
        self.slots[i].frame = Frame {
            kind,
            session: self.connections[conn].local,
            ..Frame::default()
        };
        // Share the handshake deadline, including queue pressure. Never create
        // an unbounded or application-owned confirmation ticket.
        self.slots[i].deadline = self.connections[conn].deadline;
        if let Err(e) = self.encode(i, &[]).and_then(|_| self.post(i, 1)) {
            self.release(i);
            self.fail(conn, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        Ok(())
    }

    fn accept_reply(&mut self, i: usize, frame: Frame, ready: Phase) {
        let s = &mut self.slots[i];
        if s.phase == Phase::RequestSend {
            s.early = Some(frame);
        } else {
            if ready == Phase::GrantReady {
                s.frame = frame;
            }
            s.phase = ready;
        }
    }
    #[cfg(test)]
    fn receive_bytes(&mut self, conn: usize, bytes: &[u8]) -> io::Result<()> {
        assert!(bytes.len() <= CONTROL);
        let i = self
            .slots
            .iter()
            .position(|s| s.conn == conn && s.phase == Phase::Receive && s.wr != 0)
            .unwrap();
        unsafe {
            ptr::copy_nonoverlapping(bytes.as_ptr(), self.control.pointer(i), bytes.len());
        }
        let mut completion = tests::wc(self, i);
        completion.len = bytes.len() as u32;
        self.completed(completion, crate::environment::now())
    }
    fn find_slot(&self, conn: usize, request: u64, phases: &[Phase]) -> io::Result<usize> {
        self.slots
            .iter()
            .position(|s| s.conn == conn && s.frame.request == request && phases.contains(&s.phase))
            .ok_or_else(protocol)
    }
    #[cfg(test)]
    fn assert_invariants(&self, indices: &[usize]) {
        let mut free = vec![0u64; self.slots.len().div_ceil(64)];
        for &i in indices {
            assert!(i < self.slots.len(), "foreign free index {i}");
            let bit = 1u64 << (i % 64);
            assert_eq!(free[i / 64] & bit, 0, "duplicate free index {i}");
            free[i / 64] |= bit;
        }
        for (i, s) in self.slots.iter().enumerate() {
            assert!(s.uses <= 255);
            if free[i / 64] & (1u64 << (i % 64)) != 0 {
                assert!(s.phase == Phase::Free && s.wr == 0 && s.uses < 255);
            }
            if s.wr != 0 {
                assert_eq!(s.wr as u32 as usize, i);
                assert!(!self.connections[s.conn].qp.is_null());
                assert!(s.opcode != 3 || s.fill.is_some());
                assert!(!matches!(s.opcode, 4 | 5) || s.buffer.is_some());
            }
        }
    }
    fn new(pool: &WorkerPool, rail: Rail, config: Config) -> Self {
        let count = config.connections * config.depth * 4;
        let now = crate::environment::now();
        Self {
            metrics: crate::metrics::Local::default(),
            device: ptr::null_mut(),
            lease: pool.memory_lease(),
            control: ControlArena::new(count),
            rail,
            config,
            connections: Vec::new(),
            slots: (0..count).map(|_| Slot::new(now)).collect(),
            free: (0..count).rev().collect(),
            book: Rc::new(Book {
                slots: (0..count).map(|_| Cell::new((0, false))).collect(),
            }),
            serial: 0,
            wr: 0,
            stopped: false,
            renewing: false,
            renew_after: None,
            retiring_qps: None,
            retiring_windows: None,
            #[cfg(test)]
            simulation: None,
        }
    }
    fn create_qp(&mut self, psn: u32, endpoint: &mut ffi::Endpoint) -> *mut c_void {
        #[cfg(test)]
        if self.simulation.is_some() {
            *endpoint = ffi::Endpoint {
                qpn: if self.config.connections > 1 {
                    self.serial as u32 + 7
                } else {
                    7
                },
                psn,
                mtu: 1,
                ..ffi::Endpoint::default()
            };
            return std::ptr::dangling_mut::<u8>().cast();
        }
        // SAFETY: device/CQ/PD are retained by Owner; caller installs the QP
        // in that owner before any fallible setup or DMA posting.
        unsafe {
            ffi::racer_qp(
                self.device,
                (self.config.depth * 4) as u32,
                &self.rail.raw,
                psn,
                endpoint,
            )
        }
    }
    fn init_qp(&self, qp: *mut c_void) -> i32 {
        #[cfg(test)]
        if self.simulation.is_some() {
            return 0;
        }
        // SAFETY: QP belongs to this core and is newly created.
        unsafe { ffi::racer_init(qp, self.rail.raw.port) }
    }
    fn connect_qp(&self, qp: *mut c_void, peer: &ffi::Endpoint, psn: u32, reads: u8) -> i32 {
        #[cfg(test)]
        if let Some(sim) = &self.simulation {
            return sim.connect_error;
        }
        // SAFETY: caller validated the INIT QP and authenticated peer endpoint.
        unsafe { ffi::racer_connect(qp, &self.rail.raw, peer, psn, reads) }
    }
    fn connection(&self, index: usize, serial: u64) -> io::Result<&Session> {
        self.connections
            .get(index)
            .filter(|c| c.serial == serial)
            .ok_or_else(invalid)
    }
    fn ready(&self, index: usize, serial: u64) -> io::Result<()> {
        let c = self.connection(index, serial)?;
        if self.renewing
            || self.stopped
            || !c.ready
            || c.failed
            || c.cancelled.get()
            || c.qp.is_null()
            || (c.binding.is_some() && c.auth.is_none())
        {
            return Err(error(
                io::ErrorKind::NotConnected,
                "RDMA connection unavailable; use HTTP",
            ));
        }
        Ok(())
    }
    fn region(&self, address: *mut u8, len: usize) -> io::Result<()> {
        let pool = self.lease.region();
        let offset = (address as usize)
            .checked_sub(pool.address as usize)
            .ok_or_else(invalid)?;
        if len != BUFFER_SIZE
            || !offset.is_multiple_of(BUFFER_SIZE)
            || offset.checked_add(len).is_none_or(|end| end > pool.len)
        {
            return Err(invalid());
        }
        Ok(())
    }
    fn allocate(&mut self, conn: usize, phase: Phase) -> io::Result<usize> {
        if self.free.is_empty() && self.slots.iter().any(|s| s.phase == Phase::Free) {
            self.renewing = true;
            return Err(error(
                io::ErrorKind::NotConnected,
                "RDMA retired capacity requires renewal; use HTTP",
            ));
        }
        let i = self.free.pop().ok_or_else(full)?;
        let s = &mut self.slots[i];
        let Some(generation) = s.generation.checked_add(1) else {
            return Err(full());
        };
        s.generation = generation;
        s.conn = conn;
        s.phase = phase;
        s.wr = 0;
        s.opcode = 0;
        s.frame = Frame::default();
        s.checksum = None;
        s.negative = None;
        s.descriptor = [0; 32];
        s.wire_len = 0;
        s.send_pending = false;
        s.signed = false;
        s.early = None;
        s.tracked = false;
        s.deadline = crate::environment::now() + self.config.timeout;
        self.book.slots[i].set((generation, false));
        Ok(i)
    }
    fn ticket<T>(&mut self, i: usize) -> Ticket<T> {
        self.slots[i].tracked = true;
        Ticket {
            book: self.book.clone(),
            index: i,
            generation: self.slots[i].generation,
            active: true,
            _kind: PhantomData,
        }
    }
    fn validate<T>(&self, conn: usize, serial: u64, ticket: &Ticket<T>) -> io::Result<usize> {
        self.connection(conn, serial)?;
        if !ticket.active || !Rc::ptr_eq(&self.book, &ticket.book) {
            return Err(invalid());
        }
        let s = &self.slots[ticket.index];
        if s.conn != conn || s.generation != ticket.generation || s.phase == Phase::Free {
            return Err(invalid());
        }
        Ok(ticket.index)
    }
    fn release(&mut self, i: usize) {
        let s = &mut self.slots[i];
        debug_assert_eq!(s.wr, 0);
        s.phase = Phase::Free;
        s.tracked = false;
        s.early = None;
        s.send_pending = false;
        self.book.slots[i].set((s.generation, false));
        // Never wrap an rkey on a live QP. The entire rail must quiesce before
        // the provider may recycle any window index (including on another QP).
        if s.uses == 255 {
            self.renewing = true;
        }
        // After successful QP destruction, a never-bound MW has no exported
        // capability to retire. Reuse its slot for reconnect without forcing
        // unrelated confirmed sessions through rail-wide MW renewal. Bound MWs
        // still require the existing all-QP quiescence/renewal discipline.
        if s.uses < 255
            && (!self.connections[s.conn].failed
                || (s.uses == 0 && self.connections[s.conn].qp.is_null()))
        {
            self.free.push(i);
        }
        s.buffer.take();
        s.fill.take();
    }
    fn encode(&mut self, i: usize, metadata: &[u8]) -> io::Result<()> {
        debug_assert_eq!(self.slots[i].wr, 0);
        let s = &mut self.slots[i];
        s.frame.metadata = metadata.len() as u16;
        let mut body = vec![0; HEADER + metadata.len()];
        s.frame.encode(&mut body);
        body[HEADER..].copy_from_slice(metadata);
        let wire = body;
        if wire.len() > CONTROL {
            return Err(invalid());
        }
        s.wire_len = wire.len();
        s.signed = false;
        let bytes = unsafe { self.control.bytes_mut(i) };
        bytes[..wire.len()].copy_from_slice(&wire);
        Ok(())
    }
    fn post(&mut self, i: usize, op: u32) -> io::Result<()> {
        if self.slots[i].wr != 0 {
            return Err(protocol());
        }
        if op == 1 {
            // A rejected SEND retains its signed sequence. Later controls wait
            // unsigned in their own reserved slots; no extra unbounded queue.
            let conn = self.slots[i].conn;
            if self
                .slots
                .iter()
                .enumerate()
                .any(|(j, s)| j != i && s.conn == conn && s.send_pending && s.signed)
            {
                self.slots[i].send_pending = true;
                return Ok(());
            }
            if !self.slots[i].signed {
                let mut body = unsafe { self.control.bytes(i) }[..self.slots[i].wire_len].to_vec();
                #[cfg(test)]
                if let Some(edit) = self.simulation.as_mut().and_then(|s| s.control_edit.take()) {
                    edit(&mut body);
                }
                if let Some((auth, snapshot)) = &mut self.connections[conn].auth {
                    body[..4].copy_from_slice(b"RCR4");
                    body = auth
                        .sign(
                            snapshot,
                            crypto::auth::Control::new(self.slots[i].frame.request, body)?,
                        )?
                        .encode()
                        .to_vec();
                } else if self.connections[conn].binding.is_some() {
                    return Err(protocol());
                }
                if body.len() > CONTROL {
                    return Err(invalid());
                }
                self.slots[i].wire_len = body.len();
                (unsafe { self.control.bytes_mut(i) })[..body.len()].copy_from_slice(&body);
                self.slots[i].signed = true;
            }
        }
        let s = &self.slots[i];
        let sequence = self
            .wr
            .checked_add(1)
            .filter(|s| *s <= u32::MAX as u64)
            .ok_or_else(full)?;
        self.wr = sequence;
        let id = (sequence << 32) | i as u64;
        let (address, len) = match op {
            2 => (self.control.pointer(i), CONTROL as u32),
            3 => (
                s.fill.as_ref().unwrap().region().region.address,
                s.frame.len,
            ),
            4 => (
                s.buffer.as_ref().unwrap().region().region.address,
                s.frame.len,
            ),
            _ => (self.control.pointer(i), s.wire_len as u32),
        };
        // SAFETY: all pointers refer to stable registered allocations held in
        // this owner, lengths are validated, each slot has at most one WR.
        #[cfg(test)]
        if let Some(sim) = &mut self.simulation {
            if sim.reject == op {
                if op == 1 {
                    self.slots[i].send_pending = true;
                    return Ok(());
                }
                return Err(full());
            }
            if sim.posts.len() == self.slots.len() {
                sim.posts.remove(0);
            }
            sim.posts.push((i, op));
            sim.effected.remove(&id);
            self.slots[i].wr = id;
            self.slots[i].opcode = op;
            self.slots[i].send_pending = false;
            return Ok(());
        }
        let result = check(unsafe {
            ffi::racer_post(
                self.device,
                self.connections[s.conn].qp,
                op,
                id,
                address,
                len,
                s.frame.address,
                s.frame.key,
                s.mw,
            )
        });
        if op == 1
            && result
                .as_ref()
                .is_err_and(|e| matches!(e.raw_os_error(), Some(libc::ENOMEM | libc::EAGAIN)))
        {
            self.slots[i].send_pending = true;
            return Ok(());
        }
        result?;
        self.slots[i].wr = id;
        self.slots[i].opcode = op;
        self.slots[i].send_pending = false;
        Ok(())
    }

    fn fail(&mut self, conn: usize, reason: io::ErrorKind) -> io::Result<()> {
        let c = &mut self.connections[conn];
        let had_qp = !c.qp.is_null();
        c.failed = true;
        c.ready = false;
        if self.retiring_qps.is_some() {
            return Ok(()); // The batch owns destruction; even fatal events only ACK.
        }
        let now = crate::environment::now();
        if had_qp && c.cleanup_after.is_some_and(|d| now < d) {
            return Ok(());
        }
        #[cfg(test)]
        if let Some(sim) = &self.simulation {
            if sim.destroy_fails {
                return Err(error(
                    io::ErrorKind::Other,
                    "injected QP destruction failure",
                ));
            }
            if had_qp && (sim.destroy_blocked || sim.destroy_after.is_some_and(|after| now < after))
            {
                c.cleanup_after = Some(now + Duration::from_millis(10));
                return Ok(());
            }
            c.qp = ptr::null_mut();
        }
        if !c.qp.is_null() {
            // ERR alone and local invalidate are NOT quiescence proofs. Successful
            // provider QP destruction stops both outgoing DMA and incoming READs.
            // The C job owns ERR + destroy. EAGAIN includes both an outstanding
            // job and bounded helper admission pressure; neither is quiescence.
            let result = unsafe { ffi::racer_destroy_qp(self.device, c.qp) };
            c.cleanup_after = Some(now + Duration::from_millis(100));
            if result == libc::EAGAIN {
                c.cleanup_after = Some(now + Duration::from_millis(10));
                return Ok(());
            }
            check(result)?;
            c.qp = ptr::null_mut();
        }
        c.cleanup_after = None;
        // The MWs remain allocated and are retired (no rkey-index recycling).
        // No future remote access can use the destroyed QP's type-2B bindings.
        for i in 0..self.slots.len() {
            let s = &mut self.slots[i];
            if s.conn != conn || s.phase == Phase::Free {
                continue;
            }
            if s.phase == Phase::Failed {
                if self.book.slots[i].get().1 {
                    self.release(i);
                }
                continue;
            }
            s.wr = 0;
            s.send_pending = false;
            s.failure = reason;
            s.phase = Phase::Failed;
            if !s.tracked || self.book.slots[i].get().1 {
                self.release(i);
            } else {
                self.slots[i].buffer.take();
                self.slots[i].fill.take();
            }
        }
        if had_qp && !self.stopped && self.connections.iter().all(|c| c.qp.is_null()) {
            self.renewing = true;
        }
        #[cfg(test)]
        if let Some(sim) = &mut self.simulation {
            sim.effected
                .retain(|id| self.slots.iter().any(|s| s.wr == *id));
            sim.queued
                .retain(|id| self.slots.iter().any(|s| s.wr == *id));
            sim.receives
                .retain(|id, _| self.slots.iter().any(|s| s.wr == *id));
        }
        Ok(())
    }

    /// Destroy ALL QPs before freeing ANY MW, permitting provider key recycling.
    /// MR/CQ stay registered; monotonic WR/ticket/session generations fence old CQEs.
    fn renew_windows(&mut self, now: Instant) {
        if !self.renewing || self.stopped || self.renew_after.is_some_and(|d| now < d) {
            return;
        }
        self.renew_after = Some(now + Duration::from_millis(100));
        for c in &mut self.connections {
            c.local_renewal |= !c.failed;
        }
        for c in 0..self.connections.len() {
            if self.fail(c, io::ErrorKind::ConnectionAborted).is_err() {
                return; // Keep QPs, windows, control and DMA owners; no new allocation.
            }
        }
        if self.stopped || self.connections.iter().any(|c| !c.qp.is_null()) {
            return;
        }
        // All DMA is quiescent, including forgotten tracked tickets. Retire them
        // without waiting for application polling, and rebuild admission once.
        self.free.clear();
        for s in &mut self.slots {
            s.phase = Phase::Free;
            s.tracked = false;
            s.wr = 0;
            s.send_pending = false;
            s.buffer.take();
            s.fill.take();
        }
        // Finish all deallocations before allocating any replacement. On a
        // partial failure, null handles record progress; retries stay bounded.
        if self.free_windows().is_err() {
            return;
        }
        for i in 0..self.slots.len() {
            if self.allocate_window(i).is_err() {
                return;
            }
        }
        self.free.extend((0..self.slots.len()).rev());
        self.renewing = false;
        self.renew_after = None;
    }

    fn free_windows(&mut self) -> io::Result<()> {
        if self.retiring_windows.is_none() && self.slots.iter().all(|s| s.mw.is_null()) {
            return Ok(());
        }
        #[cfg(test)]
        if let Some(sim) = &mut self.simulation {
            assert!(self.connections.iter().all(|c| c.qp.is_null()));
            if sim.window_free_fails {
                return Err(error(
                    io::ErrorKind::Other,
                    "injected MW deallocation failure",
                ));
            }
            sim.cleanup_turn(1)?;
            for s in &mut self.slots {
                sim.windows_freed += usize::from(!s.mw.is_null());
                s.mw = ptr::null_mut();
                s.binding = None;
            }
            return Ok(());
        }
        let batch = self
            .retiring_windows
            .get_or_insert_with(|| self.slots.iter().map(|s| UnsafeCell::new(s.mw)).collect());
        // SAFETY: all QPs are gone. The fixed table and MW owners remain alive
        // and inaccessible to Rust until the helper has successfully joined.
        check(unsafe {
            ffi::racer_free_windows(self.device, batch.as_ptr().cast_mut().cast(), batch.len())
        })?;
        for s in &mut self.slots {
            s.mw = ptr::null_mut();
        }
        self.retiring_windows = None;
        Ok(())
    }

    fn allocate_window(&mut self, i: usize) -> io::Result<()> {
        let s = &mut self.slots[i];
        debug_assert!(s.mw.is_null());
        #[cfg(test)]
        if let Some(sim) = &mut self.simulation {
            assert!(self.connections.iter().all(|c| c.qp.is_null()));
            if sim.window_alloc_fails {
                return Err(error(
                    io::ErrorKind::Other,
                    "injected MW allocation failure",
                ));
            }
            sim.windows_allocated += 1;
            // Deliberately recycle provider key indexes. Safety must come from
            // QP quiescence, never a simulation-only unbounded key namespace.
            s.mw = std::ptr::dangling_mut::<u8>().cast();
            s.key = 0x123400 + (i as u32 * 256);
            s.uses = 0;
            return Ok(());
        }
        s.mw = unsafe { ffi::racer_window(self.device, &mut s.key) };
        if s.mw.is_null() {
            return Err(io::Error::last_os_error());
        }
        s.uses = 0;
        Ok(())
    }

    fn received(&mut self, conn: usize, frame: Frame, receive: usize) -> io::Result<()> {
        if frame.session != self.connections[conn].peer {
            return Err(protocol());
        }
        if matches!(frame.kind, 5 | 6) {
            let c = &mut self.connections[conn];
            let expected = if frame.kind == 5 {
                Confirmation::AwaitConfirm
            } else {
                Confirmation::AwaitAck
            };
            if c.auth.is_none()
                || c.confirmation != expected
                || crate::environment::now() >= c.deadline
            {
                return Err(protocol());
            }
            c.confirmation = Confirmation::Complete;
            if frame.kind == 5 {
                self.confirmation_send(conn, 6)?;
            }
            return Ok(());
        }
        if matches!(
            self.connections[conn].confirmation,
            Confirmation::AwaitConfirm | Confirmation::AwaitAck
        ) {
            return Err(protocol());
        }
        match frame.kind {
            1 => {
                if self.connections[conn]
                    .auth
                    .as_ref()
                    .is_some_and(|(auth, snapshot)| !auth.admitting(snapshot))
                {
                    self.connections[conn].local_renewal = true;
                    return Err(error(
                        io::ErrorKind::ConnectionAborted,
                        "signing key rotated; use HTTP",
                    ));
                }
                if !frame.request_valid(self.connections[conn].last_request) {
                    return Err(protocol());
                }
                let incoming = self
                    .slots
                    .iter()
                    .filter(|s| s.conn == conn && s.phase.incoming())
                    .count();
                if incoming >= self.config.depth {
                    return Err(full());
                }
                self.connections[conn].last_request = frame.request;
                let i = self.allocate(conn, Phase::Incoming)?;
                self.slots[i].frame = frame;
                self.slots[i].descriptor = *blake3::hash(
                    &unsafe { self.control.bytes(receive) }
                        [HEADER..HEADER + usize::from(frame.metadata)],
                )
                .as_bytes();
                // Only the bounded RPC envelope is copied; payloads never are.
                let len = HEADER + usize::from(frame.metadata);
                // Receive has completed; destination is a distinct fresh slot.
                unsafe {
                    ptr::copy_nonoverlapping(
                        self.control.pointer(receive),
                        self.control.pointer(i),
                        len,
                    );
                }
            }
            2 => {
                if !frame.grant_valid() {
                    return Err(protocol());
                }
                let checksum = u64::from_be_bytes(
                    unsafe { self.control.bytes(receive) }[HEADER..HEADER + 8]
                        .try_into()
                        .unwrap(),
                );
                let i = self.find_slot(
                    conn,
                    frame.request,
                    &[Phase::RequestSend, Phase::AwaitGrant],
                )?;
                let s = &mut self.slots[i];
                if !frame.same_value(&s.frame) || s.early.is_some() {
                    return Err(protocol());
                }
                // Sidecar lives outside the SEND arena. An early grant must
                // never overwrite the request bytes still owned by the NIC.
                s.checksum = Some(checksum);
                self.accept_reply(i, frame, Phase::GrantReady);
            }
            4 => {
                if self.connections[conn].auth.is_none() || !frame.negative_valid() {
                    return Err(protocol());
                }
                let metadata = &unsafe { self.control.bytes(receive) }
                    [HEADER..HEADER + usize::from(frame.metadata)];
                let failure = PeerFailure::decode(&metadata[32..])?;
                let i = self.find_slot(
                    conn,
                    frame.request,
                    &[Phase::RequestSend, Phase::AwaitGrant],
                )?;
                let s = &mut self.slots[i];
                if metadata[..32] != s.descriptor
                    || !frame.same_value(&s.frame)
                    || s.early.is_some()
                {
                    return Err(protocol());
                }
                s.negative = Some(failure);
                self.accept_reply(i, frame, Phase::FailureReady);
            }
            3 => {
                let i =
                    self.find_slot(conn, frame.request, &[Phase::Advertise, Phase::AwaitAck])?;
                let s = &mut self.slots[i];
                if !frame.acknowledges(&s.frame) || s.early.is_some() {
                    return Err(protocol());
                }
                if unsafe { self.control.bytes(receive) }[HEADER..HEADER + 8]
                    != s.checksum.ok_or_else(protocol)?.to_be_bytes()
                {
                    return Err(protocol());
                }
                self.metrics
                    .bytes(crate::metrics::Traffic::PeerRdma, u64::from(frame.len));
                if s.phase == Phase::Advertise {
                    s.early = Some(frame);
                } else {
                    s.phase = Phase::Invalidate;
                    self.post(i, 5)?;
                }
            }
            _ => return Err(protocol()),
        }
        Ok(())
    }

    fn completed(&mut self, wc: ffi::Wc, now: Instant) -> io::Result<()> {
        // IDs never wrap. CQEs queued before destroyed QPs are harmless even
        // when a new session receives the same provider QPN or slot index.
        let i = (wc.id & u32::MAX as u64) as usize;
        if self.slots.get(i).is_none_or(|s| s.wr != wc.id || s.wr == 0) {
            return Ok(());
        }
        let conn = self.slots[i].conn;
        if self.connections[conn].failed {
            // A late CQE is not proof that incoming READs or other WRs stopped.
            // Do not advance/repost/collect any slot on a destroying QP.
            return Ok(());
        }
        if wc.status != 0
            || wc.qpn != self.connections[conn].qpn
            || wc.opcode != self.slots[i].opcode
        {
            return self.fail(conn, io::ErrorKind::ConnectionAborted);
        }
        #[cfg(test)]
        if self
            .simulation
            .as_ref()
            .is_some_and(|sim| !sim.effected.contains(&wc.id))
        {
            let s = &mut self.slots[i];
            match wc.opcode {
                4 => {
                    assert!(!s.mw.is_null());
                    assert!(
                        s.binding.is_none(),
                        "bind requires invalidation or MW renewal"
                    );
                    s.binding = Some((self.connections[conn].serial, s.frame.key));
                }
                5 => {
                    assert_eq!(
                        s.binding.take(),
                        Some((self.connections[conn].serial, s.frame.key))
                    );
                }
                _ => (),
            }
        }
        self.slots[i].wr = 0;
        #[cfg(test)]
        if let Some(sim) = &mut self.simulation {
            sim.effected.remove(&wc.id);
            sim.queued.remove(&wc.id);
            sim.receives.remove(&wc.id);
        }
        if self.stopped || self.connections[conn].failed || self.connections[conn].cancelled.get() {
            return self.fail(conn, io::ErrorKind::ConnectionAborted);
        }
        if self.slots[i].phase != Phase::Receive
            && (now >= self.slots[i].deadline
                || (self.book.slots[i].get().1 && self.slots[i].negative.is_none()))
        {
            return self.fail(conn, io::ErrorKind::TimedOut);
        }
        let result = (|| {
            match self.slots[i].phase {
                Phase::Receive => {
                    if wc.len as usize > CONTROL {
                        return Err(protocol());
                    }
                    let wire = &unsafe { self.control.bytes(i) }[..wc.len as usize];
                    let c = &mut self.connections[conn];
                    let (frame, body) = crate::negotiation::control_wire::verify(
                        wire,
                        c.auth.as_mut(),
                        c.binding.is_some(),
                    )?;
                    // This RECV has completed; normalize only its own arena.
                    (unsafe { self.control.bytes_mut(i) })[..body.len()].copy_from_slice(&body);
                    self.received(conn, frame, i)?;
                    if self.connections[conn].auth.is_some() {
                        self.connections[conn].authenticated_received = true;
                    }
                    self.post(i, 2)?;
                }
                Phase::RequestSend => {
                    if let Some(frame) = self.slots[i].early.take() {
                        self.slots[i].frame = frame;
                        self.slots[i].phase = if frame.kind == 4 {
                            Phase::FailureReady
                        } else {
                            Phase::GrantReady
                        };
                    } else {
                        self.slots[i].phase = Phase::AwaitGrant;
                    }
                }
                Phase::Bind => {
                    self.slots[i].frame.session = self.connections[conn].local;
                    let checksum = self.slots[i].checksum.ok_or_else(protocol)?.to_be_bytes();
                    self.encode(i, &checksum)?;
                    self.slots[i].phase = Phase::Advertise;
                    self.post(i, 1)?;
                }
                Phase::Advertise => {
                    if self.slots[i].early.take().is_some() {
                        self.slots[i].phase = Phase::Invalidate;
                        self.post(i, 5)?;
                    } else {
                        self.slots[i].phase = Phase::AwaitAck;
                    }
                }
                Phase::Invalidate | Phase::FailureSend | Phase::ConfirmSend => self.release(i),
                Phase::Reading => {
                    self.slots[i].frame.kind = 3;
                    self.slots[i].frame.session = self.connections[conn].local;
                    let checksum = self.slots[i].checksum.ok_or_else(protocol)?.to_be_bytes();
                    self.encode(i, &checksum)?;
                    self.slots[i].phase = Phase::Ack;
                    self.post(i, 1)?;
                }
                Phase::Ack => self.slots[i].phase = Phase::Done,
                _ => return Err(protocol()),
            }
            Ok(())
        })();
        if result.is_err() {
            self.fail(conn, io::ErrorKind::ConnectionAborted)?;
        }
        Ok(())
    }

    fn events(&mut self, budget: usize) -> io::Result<bool> {
        // Initialization can fail before the shim allocates a device owner.
        if self.device.is_null() {
            return Ok(false);
        }
        #[cfg(test)]
        if self.simulation.is_some() {
            return Ok(false);
        }
        let mut exhausted = false;
        for asynchronous in [0, 1] {
            for n in 0..budget {
                let mut qpn = 0;
                // SAFETY: single owner of nonblocking channels, shim always ACKs.
                let event = unsafe { ffi::racer_event(self.device, asynchronous, &mut qpn) };
                if event == -libc::EAGAIN {
                    break;
                }
                if event < 0 {
                    return Err(io::Error::from_raw_os_error(-event));
                }
                if event == 2 {
                    self.stopped |= qpn == 0;
                    for c in 0..self.connections.len() {
                        if qpn == 0 || self.connections[c].qpn == qpn {
                            self.fail(c, io::ErrorKind::ConnectionAborted)?;
                        }
                    }
                }
                if n + 1 == budget {
                    exhausted = true;
                }
            }
        }
        Ok(exhausted)
    }
    fn progress(&mut self, budget: usize) -> io::Result<uring::Work> {
        let budget = budget.max(1);
        let now = crate::environment::now();
        // ACK before every cleanup pass, including rail renewal. Providers may
        // wait for these ACKs inside destroy; never park behind renewal first.
        let mut runnable = self.events(budget)?;
        self.renew_windows(now);
        if self.renewing && !self.stopped {
            for c in 0..self.connections.len() {
                let _ = self.fail(c, io::ErrorKind::ConnectionAborted);
            }
            return Ok(uring::Work {
                runnable,
                deadline: self
                    .cleanup_deadline()
                    .into_iter()
                    .chain(self.renew_after)
                    .min(),
            });
        }
        for c in 0..self.connections.len() {
            if self.stopped
                || ((!self.connections[c].qp.is_null())
                    && (self.connections[c].failed || self.connections[c].cancelled.get()))
            {
                self.fail(c, io::ErrorKind::ConnectionAborted)?;
            } else if (!self.connections[c].ready
                || (self.connections[c].binding.is_some() && self.connections[c].auth.is_none())
                || matches!(
                    self.connections[c].confirmation,
                    Confirmation::AwaitConfirm | Confirmation::AwaitAck
                ))
                && now >= self.connections[c].deadline
            {
                self.fail(c, io::ErrorKind::TimedOut)?;
            }
        }
        if self.stopped {
            return Err(error(io::ErrorKind::NotConnected, "RDMA source stopped"));
        }
        for i in 0..self.slots.len() {
            let s = &self.slots[i];
            if matches!(s.phase, Phase::Free | Phase::Receive | Phase::Failed) {
                continue;
            }
            if self.connections[s.conn].failed {
                continue;
            }
            if s.phase == Phase::FailureReady && (now >= s.deadline || self.book.slots[i].get().1) {
                self.release(i);
                continue;
            }
            if now >= s.deadline || (self.book.slots[i].get().1 && s.negative.is_none()) {
                self.fail(s.conn, io::ErrorKind::TimedOut)?;
            } else if s.send_pending && self.post(i, 1).is_err() {
                self.fail(self.slots[i].conn, io::ErrorKind::ConnectionAborted)?;
            }
        }
        let mut batch = [ffi::Wc::default(); 32];
        let mut remaining = budget;
        while remaining != 0 {
            let count = remaining.min(batch.len());
            #[cfg(test)]
            let n = if let Some(sim) = &mut self.simulation {
                let n = count.min(sim.completions.len());
                for out in &mut batch[..n] {
                    *out = sim.completions.pop_front().unwrap();
                }
                n as i32
            } else {
                unsafe { ffi::racer_poll(self.device, batch.as_mut_ptr(), count as i32) }
            };
            #[cfg(not(test))]
            let n = unsafe { ffi::racer_poll(self.device, batch.as_mut_ptr(), count as i32) };
            if n < 0 {
                self.stopped = true;
                for c in 0..self.connections.len() {
                    self.fail(c, io::ErrorKind::ConnectionAborted)?;
                }
                return Err(error(io::ErrorKind::ConnectionAborted, "RDMA CQ failed"));
            }
            for wc in &batch[..n as usize] {
                // Runtime polls managers before its embedded Sources. Revisit
                // application state after a live CQE, but do not spin on stale
                // completions while provider destruction is pending.
                runnable |= self.slots.get(wc.id as u32 as usize).is_some_and(|s| {
                    s.wr != 0 && s.wr == wc.id && !self.connections[s.conn].failed
                });
                self.completed(*wc, crate::environment::now())?;
            }
            remaining -= n as usize;
            if (n as usize) < count {
                break;
            }
        }
        runnable |= remaining == 0
            || self
                .slots
                .iter()
                .any(|s| s.send_pending && !self.connections[s.conn].failed);
        // Buffer cleanup callbacks may cancel an earlier connection after this
        // poll's initial scan. Revisit it before the driver goes to sleep.
        runnable |= self
            .connections
            .iter()
            .any(|c| c.cancelled.get() && !c.failed && !c.qp.is_null());
        // Include requests received in this very batch before the driver sleeps.
        let deadline = self
            .slots
            .iter()
            .filter(|s| {
                !matches!(s.phase, Phase::Free | Phase::Receive | Phase::Failed)
                    && !self.connections[s.conn].failed
            })
            .map(|s| s.deadline)
            .chain(
                self.connections
                    .iter()
                    .filter(|c| {
                        !c.failed
                            && (!c.ready
                                || (c.binding.is_some() && c.auth.is_none())
                                || matches!(
                                    c.confirmation,
                                    Confirmation::AwaitConfirm | Confirmation::AwaitAck
                                ))
                    })
                    .map(|c| c.deadline),
            )
            .chain(self.cleanup_deadline())
            .chain(self.renew_after)
            .min();
        Ok(uring::Work { runnable, deadline })
    }
    fn cleanup_deadline(&self) -> Option<Instant> {
        self.connections
            .iter()
            .filter(|c| !c.qp.is_null())
            .filter_map(|c| c.cleanup_after)
            .min()
    }
    fn shutdown(&mut self) -> io::Result<()> {
        self.stopped = true;
        // Nonblocking, bounded event drain also services pending QP/CQ destroy.
        // Even an event-channel error must not prevent reaping completed jobs.
        let _ = self.events(32);
        if self.connections.iter().any(|c| !c.qp.is_null()) {
            #[cfg(test)]
            if let Some(sim) = &mut self.simulation {
                sim.cleanup_turn(0)?;
            }
            #[cfg(test)]
            let native = self.simulation.is_none();
            #[cfg(not(test))]
            let native = true;
            if native {
                let batch = self.retiring_qps.get_or_insert_with(|| {
                    self.connections
                        .iter()
                        .map(|c| UnsafeCell::new(c.qp))
                        .collect()
                });
                // SAFETY: stopped admission; fail() cannot touch batch-owned QPs.
                // C adopts any earlier single-QP job; all owners survive pending
                // and partial errors. Only successful join releases the table.
                check(unsafe {
                    ffi::racer_destroy_qps(
                        self.device,
                        batch.as_ptr().cast_mut().cast(),
                        batch.len(),
                    )
                })?;
                for c in &mut self.connections {
                    c.qp = ptr::null_mut();
                    c.cleanup_after = None;
                }
                self.retiring_qps = None;
            }
        }
        for c in 0..self.connections.len() {
            self.fail(c, io::ErrorKind::ConnectionAborted)?;
        }
        if self.connections.iter().any(|c| !c.qp.is_null()) {
            return Err(full());
        }
        self.free_windows()?;
        #[cfg(test)]
        if let Some(sim) = &mut self.simulation {
            sim.cleanup_turn(2)?;
            sim.cleanup_turn(3)?;
        }
        // With all QPs gone no new completion-channel events can be generated.
        if !self.device.is_null() {
            check(unsafe { ffi::racer_close(self.device) })?;
            self.device = ptr::null_mut();
        }
        Ok(())
    }
}

impl uring::CompletionSource for Source {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        for ticket in &mut self.polls {
            if let Some(t) = ticket
                && let Some(c) = ring.take_control(t)?
            {
                c.result?;
                *ticket = None;
            }
        }
        let mut owner = self.transport.owner.borrow_mut();
        if owner.core.is_none() {
            return Ok(uring::Work::default());
        }
        owner.core()?.progress(budget)
    }
    fn arm(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        if owner.core.is_none() {
            return Ok(());
        }
        let core = owner.core()?;
        if core.stopped {
            return Err(error(io::ErrorKind::NotConnected, "RDMA source stopped"));
        }
        #[cfg(test)]
        if let Some(sim) = &mut core.simulation {
            sim.armed = true;
            sim.wake = Some(ring.wake_handle());
            return Ok(());
        }
        check(unsafe { ffi::racer_notify(core.device) })?;
        for i in 0..2 {
            if self.polls[i].is_none() {
                self.polls[i] = Some(ring.poll_fd(
                    self.files.as_ref().unwrap()[i].clone().into(),
                    uring::Readiness::Readable,
                )?);
            }
        }
        // uring::Driver rechecks CQ/application state after arming, before wait.
        Ok(())
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        for ticket in &mut self.polls {
            if let Some(t) = ticket.take() {
                drop(ring.cancel(&t)?);
            }
        }
        self.transport.shutdown()
    }
}
impl Drop for Source {
    fn drop(&mut self) {
        let result =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| self.transport.shutdown()));
        if let Err(panic) = result {
            std::mem::forget(panic);
        }
    }
}

#[cfg(test)]
#[path = "../tests/rdma/transport.rs"]
pub(crate) mod tests;
#[cfg(test)]
pub(crate) use tests::{
    TestPost, TestQp, test_connection, test_qps, test_transport, test_transport_config,
    test_transport_multi,
};
