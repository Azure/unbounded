// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local TLS control channels and immutable plaintext RDMA READs. Attach one
//! [`Source`] per worker/RNIC to uring; negotiation authenticates offers over TLS.
//! Only authenticated channel-bound offers authorize activation; failures select HTTP.
//! Type-2B windows are QP-bound/read-only with private backing MR keys. BIND CQE
//! precedes advertisement, READ CQE generates ACK, and ACK + INV CQE releases Buffer.
//! Ambiguous failures destroy the QP before releasing DMA; failed destruction leaks
//! the retained owner. Forgotten tickets still expire. `Connected` has no data API
//! until its exact channel is installed. TLS protects control ordering and integrity;
//! grants/ACKs carry CRC64, verified before cache publication. Descriptor-bound
//! negative replies never advertise memory.
//! Linux needs libibverbs development files, a C compiler and ar; tests need no RNIC.

use crate::{
    buffers::{
        BUFFER_SIZE, Buffer, Destination, Fill, Key, MemoryLease, WorkerPool, Writable,
        WritableStorage,
    },
    negotiation::ControlChannel,
    uring,
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
mod connection;
mod control;
mod ffi;
mod framing;
mod lifecycle;
mod provider;
mod retirement;
mod slot_state;
mod slots;
mod source;
use crate::negotiation::control_wire::{Frame, HEADER};
use framing::ControlArena;
/// Maximum opaque RPC metadata (for example, encoded HTTP headers).
pub const MAX_METADATA: usize = CONTROL - HEADER;

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
impl Rail {
    /// Stable device:port:GID selector used by process-start policy.
    #[cfg(feature = "dev-bench")]
    pub(crate) fn benchmark_selector(&self) -> String {
        format!("{}:{}:{}", self.name, self.raw.port, self.raw.gid_index)
    }
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

pub use crate::negotiation::AuthenticatedOffer;
pub use crate::negotiation::{Offer, TransportConfig as Config, rails_for_shard};

struct Book {
    slots: Vec<Cell<(u64, bool)>>,
}
/// A ticket never owns a DMA buffer. Dropping an unknown/granted outcome asks the
/// driver to fail the session. A known negative waits for channel write retirement.
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
pub use crate::outcome::PeerFailure;
pub enum GrantReply {
    Grant(RemoteGrant),
    Failure(PeerFailure),
    /// Validated, descriptor-bound admission refusal; retry the same request
    /// over fresh HTTP without treating local maintenance as peer evidence.
    Retry(PeerFailure),
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
impl Drop for Request {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.metadata.zeroize();
    }
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
/// available until consuming `authenticate_channel` succeeds. Drop or failed
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
/// use racer_dataplane::{negotiation::ControlChannel, rdma::Connected};
/// fn reuse(c: Connected, a: ControlChannel, b: ControlChannel) {
///     let _ = c.authenticate_channel(a);
///     let _ = c.authenticate_channel(b);
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
/// use racer_dataplane::{negotiation::ControlChannel, rdma::Connection};
/// fn reinstall(c: Connection, channel: ControlChannel) {
///     let _ = c.authenticate_channel(channel);
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

struct ConnectionState {
    local_renewal: bool,
    binding: Option<[u8; 32]>,
    channel: Option<ControlChannel>,
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
impl ConnectionState {
    fn new(
        qp: *mut c_void,
        serial: u64,
        local: [u8; 16],
        endpoint: ffi::Endpoint,
        deadline: Instant,
    ) -> Self {
        Self {
            local_renewal: false,
            binding: None,
            channel: None,
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
    send_id: u64,
    control_tag: u64,
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

    failure: io::ErrorKind,
    early: Option<Frame>,
    tracked: bool,
}
struct Core {
    metrics: crate::metrics::Local,
    device: *mut c_void,
    lease: MemoryLease,
    control: ControlArena,
    rail: Rail,
    config: Config,
    connections: Vec<ConnectionState>,
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
}
// Core is always behind this leak-on-failed-cleanup owner. Never put raw DMA
// owners in a local temporary whose destructor can run before quiescence.
struct Owner {
    core: Option<Box<Core>>,
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
        // Bounded RPC/control owners per QP. Only READ/BIND/INV use verbs WRs.
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
        let session = ConnectionState::new(
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
        let setup = check(core.init_qp(qp));
        if let Err(e) = setup {
            core.fail(index, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        let offer = Offer {
            version: 1,
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

impl Connection {
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
    /// Queue one RPC. `value` must include the complete storage identity,
    /// including version and page offset. `len` is the expected stored length.
    pub fn request(
        &self,
        value: [u8; 32],
        len: usize,
        metadata: &[u8],
    ) -> io::Result<Ticket<Grant>> {
        if metadata.len() > MAX_METADATA {
            return Err(invalid());
        }
        self.request_with_metadata(value, len, || Ok(metadata))
    }
    /// Invoke the metadata builder only after local request admission. Callers
    /// can spend affine forwarding authority here without charging capacity
    /// retries. Once invoked, an error may have followed submission.
    pub(crate) fn request_with_metadata<M: AsRef<[u8]>>(
        &self,
        value: [u8; 32],
        len: usize,
        metadata: impl FnOnce() -> io::Result<M>,
    ) -> io::Result<Ticket<Grant>> {
        if self.key_draining() {
            return Err(error(
                io::ErrorKind::ConnectionAborted,
                "TLS credential generation retired; use HTTP",
            ));
        }
        if len == 0 || len > BUFFER_SIZE {
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
        let awaiting_confirmation = matches!(
            core.connections[self.index].confirmation,
            Confirmation::AwaitConfirm | Confirmation::AwaitAck
        );

        if awaiting_confirmation
            || core
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
        let metadata = match metadata() {
            Ok(metadata) if metadata.as_ref().len() <= MAX_METADATA => metadata,
            result => {
                core.release(i);
                return Err(result.err().unwrap_or_else(invalid));
            }
        };
        let metadata = metadata.as_ref();
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
        if let Err(e) = core.encode(i, metadata).and_then(|_| core.send_control(i)) {
            core.release(i);
            core.fail(self.index, io::ErrorKind::ConnectionAborted)?;
            return Err(e);
        }
        Ok(core.ticket(i))
    }

    pub fn take_grant(&self, ticket: &mut Ticket<Grant>) -> io::Result<Option<RemoteGrant>> {
        match self.take_reply(ticket)? {
            Some(GrantReply::Grant(grant)) => Ok(Some(grant)),
            Some(GrantReply::Failure(failure) | GrantReply::Retry(failure)) => {
                Err(io::Error::other(failure))
            }
            None => Ok(None),
        }
    }

    pub fn take_reply(&self, ticket: &mut Ticket<Grant>) -> io::Result<Option<GrantReply>> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let i = self.collect_slot(core, ticket)?;
        if core.slots[i].phase == Phase::FailureReady {
            let failure = core.slots[i].negative.take().ok_or_else(protocol)?;
            let retry = core.slots[i].frame.kind == 7;
            ticket.active = false;
            core.release(i);
            return Ok(Some(if retry {
                GrantReply::Retry(failure)
            } else {
                GrantReply::Failure(failure)
            }));
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

    /// Collect before deadline, after READ CQE + TLS ACK write; READ byte_len is undefined
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
            metadata: core.control.bytes(i)[HEADER..HEADER + f.metadata as usize].to_vec(),
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
    /// the bounded control until TLS write retirement, including queue pressure.
    pub fn respond_error(&self, mut request: Request, failure: PeerFailure) -> io::Result<()> {
        let mut owner = self.transport.owner.borrow_mut();
        let core = owner.core()?;
        let i = core.validate(self.index, self.serial, &request.ticket)?;
        core.ready(self.index, self.serial)?;
        let c = &core.connections[self.index];
        if c.channel.is_none() || core.slots[i].phase != Phase::Claimed {
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
        if let Err(e) = core.send_control(i) {
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
    /// whose completion is invisible to the server. Known negatives retire control writes.
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
        }
    }
}

impl Core {
    fn connection(&self, index: usize, serial: u64) -> io::Result<&ConnectionState> {
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
            || c.channel.as_ref().is_none_or(|channel| !channel.healthy())
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
}

impl Core {
    fn encode(&mut self, i: usize, metadata: &[u8]) -> io::Result<()> {
        debug_assert_eq!(self.slots[i].wr, 0);
        debug_assert_eq!(self.slots[i].send_id, 0);
        let s = &mut self.slots[i];
        if metadata.len() > MAX_METADATA {
            return Err(invalid());
        }
        s.control_tag = 0;
        s.wire_len = self.control.encode(i, &mut s.frame, metadata)?;
        Ok(())
    }
    fn post(&mut self, i: usize, op: u32) -> io::Result<()> {
        if self.slots[i].wr != 0 || self.slots[i].send_id != 0 {
            return Err(protocol());
        }
        if !matches!(op, 3..=5) {
            return Err(invalid());
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
            3 => (
                s.fill.as_ref().unwrap().region().region.address,
                s.frame.len,
            ),
            4 => (
                s.buffer.as_ref().unwrap().region().region.address,
                s.frame.len,
            ),
            _ => (ptr::null_mut(), 0),
        };
        // SAFETY: all pointers refer to stable registered allocations held in
        // this owner, lengths are validated, each slot has at most one WR.

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
        result?;
        self.slots[i].wr = id;
        self.slots[i].opcode = op;
        self.slots[i].send_pending = false;
        Ok(())
    }

    fn send_control(&mut self, i: usize) -> io::Result<()> {
        if self.slots[i].wr != 0 || self.slots[i].send_id != 0 {
            return Err(protocol());
        }
        let conn = self.slots[i].conn;
        if self.slots[i].control_tag == 0 {
            self.wr = self
                .wr
                .checked_add(1)
                .filter(|n| *n <= u32::MAX as u64)
                .ok_or_else(full)?;
            self.slots[i].control_tag = (self.wr << 32) | i as u64;
        }
        let id = self.slots[i].control_tag;
        if self
            .slots
            .iter()
            .any(|s| s.conn == conn && s.send_pending && s.control_tag < id)
        {
            self.slots[i].send_pending = true;
            return Ok(());
        }

        let channel = self.connections[conn]
            .channel
            .as_mut()
            .ok_or_else(protocol)?;
        match channel.enqueue(id, &self.control.bytes(i)[..self.slots[i].wire_len]) {
            Ok(()) => {
                self.slots[i].send_id = id;
                self.slots[i].send_pending = false;
                Ok(())
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                self.slots[i].send_pending = true;
                Ok(())
            }
            Err(e) => Err(e),
        }
    }

    fn sent(&mut self, conn: usize, id: u64) -> io::Result<()> {
        let i = id as u32 as usize;
        if self
            .slots
            .get(i)
            .is_none_or(|s| s.conn != conn || s.send_id != id || id == 0)
            || self.connections[conn].failed
        {
            return Ok(());
        }
        if crate::environment::now() >= self.slots[i].deadline
            || (self.book.slots[i].get().1 && self.slots[i].negative.is_none())
        {
            return self.fail(conn, io::ErrorKind::TimedOut);
        }
        self.slots[i].send_id = 0;
        match self.slots[i].phase {
            Phase::RequestSend => self.slots[i].request_sent(),
            Phase::Advertise => {
                if self.slots[i].early.take().is_some() {
                    self.slots[i].phase = Phase::Invalidate;
                    self.post(i, 5)?;
                } else {
                    self.slots[i].phase = Phase::AwaitAck;
                }
            }
            Phase::FailureSend | Phase::ConfirmSend => self.release(i),
            Phase::Ack => self.slots[i].phase = Phase::Done,
            _ => return Err(protocol()),
        }
        Ok(())
    }

    fn poll_channels(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = uring::Work::default();
        for conn in 0..self.connections.len() {
            if self.connections[conn].failed {
                continue;
            }
            let Some(channel) = self.connections[conn].channel.as_mut() else {
                continue;
            };
            let result = if channel.healthy() {
                channel.poll(ring, budget.max(1))
            } else {
                Err(error(
                    io::ErrorKind::PermissionDenied,
                    "TLS peer no longer authorized",
                ))
            };
            match result {
                Ok(progress) => {
                    work.runnable |= progress.runnable;
                    work.deadline = work.deadline.into_iter().chain(progress.deadline).min();
                }
                Err(_) => {
                    self.fail(conn, io::ErrorKind::ConnectionAborted)?;
                    continue;
                }
            }
            for _ in 0..budget.max(1) {
                let id = self.connections[conn].channel.as_mut().unwrap().take_sent();
                let Some(id) = id else {
                    break;
                };
                work.runnable = true;
                if self.sent(conn, id).is_err() {
                    self.fail(conn, io::ErrorKind::ConnectionAborted)?;
                    break;
                }
            }
            for _ in 0..budget.max(1) {
                if self.connections[conn].failed {
                    break;
                }
                let bytes = self.connections[conn]
                    .channel
                    .as_mut()
                    .unwrap()
                    .take_received();
                let Some(bytes) = bytes else {
                    break;
                };
                work.runnable = true;
                if self.receive_bytes(conn, &bytes).is_err() {
                    self.fail(conn, io::ErrorKind::ConnectionAborted)?;
                    break;
                }
            }
        }
        Ok(work)
    }
}

impl Core {
    fn received(&mut self, conn: usize, frame: Frame, metadata: &[u8]) -> io::Result<()> {
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
            if c.channel.is_none()
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
                self.slots[i].descriptor = *blake3::hash(metadata).as_bytes();
                if let Some(channel) = self.connections[conn].channel.as_ref()
                    && !channel.admitting()
                {
                    // A peer can race our local rotation. Reject only this RPC;
                    // existing READs and advertised windows keep their deadlines.
                    let failure = channel.rejection(metadata);
                    self.slots[i].frame.kind =
                        if failure.reason == crate::outcome::PeerReason::Unavailable {
                            7
                        } else {
                            4
                        };
                    self.slots[i].frame.session = self.connections[conn].local;
                    let mut negative = self.slots[i].descriptor.to_vec();
                    negative.extend(failure.encode());
                    self.encode(i, &negative)?;
                    self.slots[i].phase = Phase::FailureSend;
                    self.send_control(i)?;
                } else {
                    self.encode(i, metadata)?;
                }
            }
            2 => {
                if !frame.grant_valid() {
                    return Err(protocol());
                }
                let checksum = u64::from_be_bytes(metadata[..8].try_into().unwrap());
                let i = self.find_slot(
                    conn,
                    frame.request,
                    &[Phase::RequestSend, Phase::AwaitGrant],
                )?;
                let s = &mut self.slots[i];
                if !frame.same_value(&s.frame) || s.early.is_some() {
                    return Err(protocol());
                }
                // Keep an early reply separate until the request's TLS write
                // retires; queue pressure cannot change its retained bytes.
                s.checksum = Some(checksum);
                self.accept_reply(i, frame, Phase::GrantReady);
            }
            4 | 7 => {
                if self.connections[conn].channel.is_none() || !frame.negative_valid() {
                    return Err(protocol());
                }
                let failure = PeerFailure::decode(&metadata[32..])?;
                if frame.kind == 7
                    && (failure.reason != crate::outcome::PeerReason::Unavailable
                        || failure.evidence.is_some())
                {
                    return Err(protocol());
                }
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
                if metadata[..8] != s.checksum.ok_or_else(protocol)?.to_be_bytes() {
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

        self.slots[i].wr = 0;

        if self.stopped || self.connections[conn].failed || self.connections[conn].cancelled.get() {
            return self.fail(conn, io::ErrorKind::ConnectionAborted);
        }
        if now >= self.slots[i].deadline
            || (self.book.slots[i].get().1 && self.slots[i].negative.is_none())
        {
            return self.fail(conn, io::ErrorKind::TimedOut);
        }
        let result = (|| {
            match self.slots[i].phase {
                Phase::Bind => {
                    self.slots[i].frame.session = self.connections[conn].local;
                    let checksum = self.slots[i].checksum.ok_or_else(protocol)?.to_be_bytes();
                    self.encode(i, &checksum)?;
                    self.slots[i].phase = Phase::Advertise;
                    self.send_control(i)?;
                }
                Phase::Invalidate => self.release(i),
                Phase::Reading => {
                    self.slots[i].frame.kind = 3;
                    self.slots[i].frame.session = self.connections[conn].local;
                    let checksum = self.slots[i].checksum.ok_or_else(protocol)?.to_be_bytes();
                    self.encode(i, &checksum)?;
                    self.slots[i].phase = Phase::Ack;
                    self.send_control(i)?;
                }
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
                || self.connections[c].channel.is_none()
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
            if matches!(s.phase, Phase::Free | Phase::Failed) {
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
            } else if s.send_pending {
                if self.send_control(i).is_err() {
                    self.fail(self.slots[i].conn, io::ErrorKind::ConnectionAborted)?;
                } else {
                    runnable |= self.slots[i].send_id != 0;
                }
            }
        }
        let mut batch = [ffi::Wc::default(); 32];
        let mut remaining = budget;
        while remaining != 0 {
            let count = remaining.min(batch.len());

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
                    (s.wr == wc.id || s.send_id == wc.id)
                        && wc.id != 0
                        && !self.connections[s.conn].failed
                });

                self.completed(*wc, crate::environment::now())?;
            }
            remaining -= n as usize;
            if (n as usize) < count {
                break;
            }
        }
        runnable |= remaining == 0;
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
                !matches!(s.phase, Phase::Free | Phase::Failed) && !self.connections[s.conn].failed
            })
            .map(|s| s.deadline)
            .chain(
                self.connections
                    .iter()
                    .filter(|c| {
                        !c.failed
                            && (!c.ready
                                || c.channel.is_none()
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
}

#[cfg(test)]
#[path = "../tests/rdma/transport.rs"]
pub(crate) mod tests;
