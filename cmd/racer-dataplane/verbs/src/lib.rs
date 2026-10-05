//! Bounded RDMA ownership for paired I/O and native threads.
//!
//! This crate does not spawn threads or drive an executor. Create [`pair`] before
//! starting either role, move its ports to their threads, and construct
//! [`NativeService`] on the native thread. Native owners are not `Send`; provider
//! calls and destruction remain on that thread. Time and randomness come from
//! the current `uring-runtime` environment.
//!
//! # Integration
//!
//! Submit a [`Configuration`] with one opaque [`Guard`] per slot, a byte bound,
//! and a selector. The selector runs on the native thread after discovery and
//! returns caller tags paired with discovered indices. Duplicate tags, reused
//! indices, and out-of-range indices are rejected. An intentionally empty plan
//! can skip discovery and therefore does not require the native adapter.
//!
//! Await [`IoPort::activate`], or use [`IoPort::configure`] and
//! [`IoPort::activation`] with caller-managed cancellation. After submission,
//! abandoning activation requires closing the I/O port and continuing native
//! progress. Claim a [`QueuePairHandle`] through an I/O-local [`DeviceHandle`],
//! exchange authenticated [`Endpoint`] values using the caller's protocol, and
//! connect before binding a [`Window`] or writing a [`Region`]. A window must not
//! be published before its bind [`Ticket`] succeeds.
//!
//! # Fencing and shutdown
//!
//! Request [`QueuePairHandle::stop`] and await [`QueuePairHandle::poll_stopped`].
//! Neither a timeout nor an invalidation completion is a terminal DMA fence.
//! Readback requires the actual native QP fence. Dropping a QP requests
//! cancellation without blocking I/O. Native ownership and caller guards survive
//! failed destruction, including intentional bounded quarantine leaks.
//!
//! Reopening requires native drainage and release of all I/O leases. Caller
//! `Arc` clones do not affect the internal quarantine reference count. Use
//! `uring_runtime::poll_scoped` for caller-scoped operations, but never to truncate
//! a required native fence. [`WithNative`] preserves native progress after
//! admission stops and fences native resources before the inner drain/fence.
//!
//! # Backends and bounds
//!
//! The `native` feature loads `librdma_verbs.so.1` and checks ABI version 2.
//! Build `native/verbs.c` against installed libibverbs headers; Rust never
//! reproduces provider layouts. Without native support or an active simulated
//! fabric, discovery returns [`Error::Unavailable`]. The `simulation` feature
//! exposes a deterministic connected fabric and narrow contention/observation
//! controls. Enter a node's simulation scope before constructing its native
//! service; owners retain the fabric after the scope exits.
//!
//! Capacity is 1 through 256 slots, with one region and one pending command per
//! leased slot. Discovery uses GID index 0 and rejects inventories exceeding 64
//! ports rather than truncating them. Poll budgets bound steps, not provider wall
//! time. A stalled provider can stall siblings on the native thread. Mailbox
//! contention requires periodic driver ticks as well as completion wakes.
//! Authentication, topology, admission sizing, page-size accounting, retry policy,
//! and request scopes belong to callers.
//!
//! Run `cargo test -p rdma-verbs --locked --offline --features simulation` from
//! the workspace for hardware-free integration checks. Simulated DMA does not
//! validate the C adapter or a real provider; operator-selected native tests are
//! ignored by default.
mod ffi;
pub use ffi::Endpoint;
#[cfg(any(test, feature = "simulation"))]
pub use ffi::simulation;

/// Synchronous physical inventory normalization without application rail IDs.
pub mod discovery {
    use crate::PortInfo;
    use std::path::Path;

    /// Check the native name bound and reject invalid sysfs path components.
    pub fn valid_device(device: &str) -> bool {
        !device.is_empty()
            && device.len() <= 63
            && !device.contains(['/', '\0', '\r', '\n'])
            && device != "."
            && device != ".."
    }

    /// Normalize eligible ports using PCI and NUMA metadata below `root`.
    ///
    /// Known PCI paths sort first, then PCI path, port number, and device name.
    /// Equal entries retain provider order, including the first duplicate GID.
    /// Missing or invalid NUMA metadata replaces provider locality with unknown.
    /// The caller applies its own inventory bound and logical identities.
    pub fn inventory_at(mut ports: Vec<PortInfo>, root: &Path) -> Vec<PortInfo> {
        ports.retain(|p| valid_device(&p.device) && p.port != 0 && p.gid != [0; 16]);
        let mut ports: Vec<_> = ports
            .into_iter()
            .map(|mut port| {
                let device = root.join(&port.device).join("device");
                let bdf = std::fs::canonicalize(&device)
                    .ok()
                    .and_then(|p| p.file_name().map(|s| s.to_string_lossy().into_owned()));
                port.numa_node = std::fs::read_to_string(device.join("numa_node"))
                    .ok()
                    .and_then(|s| s.trim().parse::<u32>().ok())
                    .map(|n| n as usize);
                (bdf, port)
            })
            .collect();
        ports.sort_by(|(a, x), (b, y)| {
            (a.is_none(), a, x.port, &x.device).cmp(&(b.is_none(), b, y.port, &y.device))
        });
        let mut physical = std::collections::BTreeSet::new();
        ports.retain(|(_, p)| physical.insert((p.device.clone(), p.port)));
        ports.into_iter().map(|(_, p)| p).collect()
    }
}

/// Synchronous pre-enrollment inventory with no lasting native handles.
///
/// Handles are dropped on this thread. Only owned descriptions leave it;
/// missing providers return [`Error::Unavailable`]. NUMA normalization is separate.
pub fn inventory() -> Result<Vec<PortInfo>> {
    Ok(ffi::discover()?
        .iter()
        .map(|device| PortInfo {
            device: device.name.clone(),
            port: device.endpoint.port,
            gid: device.endpoint.gid,
            numa_node: None,
        })
        .collect())
}

/// Opaque caller-owned lifetime charge whose contents are never inspected.
pub type Guard = Arc<dyn Send + Sync>;

/// A verbs operation result.
pub type Result<T> = std::result::Result<T, Error>;

/// Portable admission, validation, and native-operation failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The pool or selector configuration is invalid.
    InvalidConfiguration,
    /// Operation parameters or state are invalid.
    InvalidRequest,
    /// The requested memory range is invalid.
    InvalidRange,
    /// The provider, connection, or required resource is unavailable.
    Unavailable,
    /// Bounded capacity cannot accept more work.
    Overloaded,
    /// The operation's deadline has expired.
    DeadlineExceeded,
    /// The operation was canceled before successful completion.
    Cancelled,
    /// A provider operation or synchronization primitive failed.
    Io,
}
impl std::fmt::Display for Error {
    /// Render the stable variant name used by the portable error type.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Error {}

/// Description available to the selector on the native owner thread.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PortInfo {
    /// Native device name.
    pub device: String,
    /// Physical port number.
    pub port: u8,
    /// Port GID at the adapter's selected index.
    pub gid: [u8; 16],
    /// Known local NUMA node, if available.
    pub numa_node: Option<usize>,
}

/// Caller tag associated with one discovered physical port.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Selection {
    /// Caller-defined tag used for later slot claims.
    pub tag: u32,
    /// Index in the selector's discovered inventory.
    pub index: usize,
    /// Owned description of the selected port.
    pub port: PortInfo,
}

/// Owned plan retained while configuration mailboxes are contended.
pub struct Configuration {
    /// Skip native discovery for an intentionally empty plan. The selector
    /// receives an empty slice and must return an empty selection.
    pub discover: bool,
    /// Native-thread selector returning unique caller tags and inventory indices.
    pub selector: Box<PortSelector>,
    /// One lifetime charge for each pool slot.
    pub guards: Vec<Guard>,
    /// Staging and registered allocation capacity per slot.
    pub bytes: usize,
}

/// Sendable I/O handoff created before either paired role starts.
pub struct IoPort {
    pub(crate) shared: Arc<Shared>,
}

/// Sendable handoff consumed on the thread that constructs the native service.
pub struct NativePort {
    shared: Arc<Shared>,
}

/// Bounded native progress constructed on the owning thread after port handoff.
/// `Rc` native owners never cross threads, even during cancellation or shutdown.
pub struct NativeService {
    environment: uring_runtime::environment::Environment,
    port: NativePort,
    resources: Vec<Option<Resource>>,
    activation: Option<Activation>,
    cursor: usize,
    #[cfg(any(test, feature = "simulation"))]
    simulation: Option<simulation::Simulation>,
}

/// Compose a local service with paired native progress and uncancelable fences.
///
/// Both services receive the budget. Admission stop leaves native progress
/// available for accepted work. Drain and fence close and fence native resources
/// one slot per turn before the inner hook, even if the scope has expired.
/// Close shuts native admission even if the inner close reports a failure.
pub struct WithNative<T> {
    inner: T,
    native: NativeService,
}

/// I/O-local caller tag tied to one pool generation.
pub struct DeviceHandle {
    pub(crate) port: Rc<IoPort>,
    pub(crate) rail: u32,
    pub(crate) generation: u64,
}

/// I/O-local queue pair proxy; native calls run only on the paired service.
pub struct QueuePairHandle {
    lease: Rc<Lease>,
    /// Local endpoint to exchange through the caller's authenticated protocol.
    pub endpoint: Endpoint,
    connecting: RefCell<Option<Ticket>>,
    connected: Cell<bool>,
    pending: RefCell<Option<Ticket>>,
    failure: Cell<Option<Error>>,
    expires: Cell<Option<std::time::Instant>>,
}

/// One leased slot's bounded staging region, readable only after its native fence.
pub struct Region {
    lease: Rc<Lease>,
    length: usize,
}

/// Remote write capability whose descriptor becomes visible after bind completion.
pub struct Window {
    pub(crate) key: Cell<u32>,
    pub(crate) address: Cell<u64>,
    _region: Rc<Region>,
}

/// Shared observation of one command's native completion, not mere submission.
#[derive(Clone)]
pub struct Ticket(Rc<TicketState>);

impl Endpoint {
    /// Fixed width of the network-order encoding, independent of native layout.
    pub const ENCODED_LEN: usize = 32;

    /// Encode fields without validation; callers may validate before encoding.
    pub fn to_bytes(&self) -> [u8; Self::ENCODED_LEN] {
        let mut bytes = [0; Self::ENCODED_LEN];
        bytes[..16].copy_from_slice(&self.gid);
        bytes[16..20].copy_from_slice(&self.qpn.to_be_bytes());
        bytes[20..24].copy_from_slice(&self.psn.to_be_bytes());
        bytes[24..28].copy_from_slice(&self.mtu.to_be_bytes());
        bytes[28..30].copy_from_slice(&self.lid.to_be_bytes());
        bytes[30] = self.port;
        bytes[31] = self.link_layer;
        bytes
    }

    /// Decode exactly one endpoint and validate its verbs field bounds.
    pub fn from_bytes(bytes: &[u8]) -> Result<Self> {
        if bytes.len() != Self::ENCODED_LEN {
            return Err(Error::InvalidRequest);
        }
        let endpoint = Self {
            gid: bytes[..16].try_into().unwrap(),
            qpn: u32::from_be_bytes(bytes[16..20].try_into().unwrap()),
            psn: u32::from_be_bytes(bytes[20..24].try_into().unwrap()),
            mtu: u32::from_be_bytes(bytes[24..28].try_into().unwrap()),
            lid: u16::from_be_bytes(bytes[28..30].try_into().unwrap()),
            port: bytes[30],
            link_layer: bytes[31],
        };
        endpoint.validate()?;
        Ok(endpoint)
    }
}

use futures::task::AtomicWaker;
use std::{
    cell::{Cell, RefCell},
    rc::Rc,
    sync::{
        Arc, Mutex, MutexGuard, TryLockError,
        atomic::{AtomicBool, AtomicU8, AtomicU64, Ordering},
    },
    task::{Context, Poll, Waker, ready},
};
use uring_runtime::{
    Operation, Scope,
    group::{FailureReporter, Service},
    poll_scoped,
};

/// Keep a slot and its staging alive until the last I/O owner releases it.
struct Lease {
    slot: Arc<Slot>,
    shared: Arc<Shared>,
}
impl Drop for Lease {
    /// Request cancellation and release staging admission on the last I/O lease.
    fn drop(&mut self) {
        self.slot.cancel.store(true, Ordering::Release);
        self.slot.released.store(true, Ordering::Release);
        self.shared.engine.wake();
    }
}
impl Region {
    /// Claim the slot's single region, yielding on mailbox contention.
    pub fn poll_acquire(qp: &QueuePairHandle, length: usize) -> Poll<Result<Rc<Self>>> {
        if length == 0 {
            return Poll::Ready(Err(Error::InvalidRange));
        }
        if qp.lease.slot.cancel.load(Ordering::Acquire)
            || qp.lease.shared.closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut mailbox = ready!(try_mailbox(&qp.lease.slot.mailbox))?;
        if length > mailbox.bytes.len() || mailbox.length != 0 {
            return Poll::Ready(Err(Error::Overloaded));
        }
        mailbox.length = length;
        Poll::Ready(Ok(Rc::new(Self {
            lease: qp.lease.clone(),
            length,
        })))
    }
    /// Return the transfer length, not the pool's registered allocation capacity.
    pub fn length(&self) -> usize {
        self.length
    }
    /// Copy an exact-length payload into staging when no command is queued.
    pub fn poll_copy_from(&self, bytes: &[u8]) -> Poll<Result<()>> {
        if bytes.len() != self.length {
            return Poll::Ready(Err(Error::InvalidRange));
        }
        if self.lease.slot.cancel.load(Ordering::Acquire)
            || self.lease.shared.closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        if mailbox.command.is_some() {
            return Poll::Ready(Err(Error::Unavailable));
        }
        mailbox.bytes[..self.length].copy_from_slice(bytes);
        Poll::Ready(Ok(()))
    }
    /// Register for the leased slot's native progress notifications.
    pub fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }
    /// Copy staging after the terminal native fence, yielding on contention.
    pub fn poll_copy_to(&self, cx: &mut Context<'_>) -> Poll<Result<Vec<u8>>> {
        self.lease.slot.waiter.register(cx.waker());
        if !self.lease.slot.fenced.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        Poll::Ready(self.copy_bytes(&mailbox))
    }
    /// Allocate an owned copy of the region's exact transfer length.
    fn copy_bytes(&self, mailbox: &Mailbox) -> Result<Vec<u8>> {
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(self.length)
            .map_err(|_| Error::Overloaded)?;
        bytes.extend_from_slice(&mailbox.bytes[..self.length]);
        Ok(bytes)
    }
}
/// Retain the slot and cache one completion independently of later commands.
struct TicketState {
    lease: Rc<Lease>,
    result: Cell<Option<Result<()>>>,
    window: Option<Rc<Window>>,
}
impl Ticket {
    /// Observe a cached completion, or `None` before completion or during contention.
    pub fn result(&self) -> Option<Result<()>> {
        match self.poll_result() {
            Poll::Ready(result) => result,
            Poll::Pending => None,
        }
    }
    /// Consume and cache the mailbox result, publishing successful bind descriptors.
    fn poll_result(&self) -> Poll<Option<Result<()>>> {
        if let Some(result) = self.0.result.get() {
            return Poll::Ready(Some(result));
        }
        let mut mailbox = match ready!(try_mailbox(&self.0.lease.slot.mailbox)) {
            Ok(mailbox) => mailbox,
            Err(error) => return Poll::Ready(Some(Err(error))),
        };
        let Some(result) = mailbox.result.take() else {
            return Poll::Ready(None);
        };
        if result.is_ok()
            && let Some(window) = &self.0.window
            && let Some((address, key)) = mailbox.descriptor
        {
            window.address.set(address);
            window.key.set(key);
        }
        self.0.result.set(Some(result));
        Poll::Ready(Some(result))
    }
    /// Register a waiter and await completion, rejecting an unavailable closed pool.
    pub fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.0.lease.slot.waiter.register(cx.waker());
        if let Some(result) = self.result() {
            return Poll::Ready(result);
        }
        if self.0.lease.shared.closed.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        Poll::Pending
    }
}
impl QueuePairHandle {
    /// Claim a ready slot and retain an optional guard until its native fence.
    pub fn poll_new(device: Rc<DeviceHandle>, permit: Option<Guard>) -> Poll<Result<Rc<Self>>> {
        if device.port.shared.closed.load(Ordering::Acquire)
            || device.generation != device.port.shared.generation.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mut contended = false;
        for slot in &device.port.shared.slots {
            if slot.state.load(Ordering::Acquire) != READY {
                continue;
            }
            let mut mailbox = match try_mailbox(&slot.mailbox) {
                Poll::Pending => {
                    contended = true;
                    continue;
                }
                Poll::Ready(result) => result?,
            };
            if mailbox.rail != device.rail {
                continue;
            }
            if slot
                .state
                .compare_exchange(READY, OWNED, Ordering::AcqRel, Ordering::Acquire)
                .is_err()
            {
                continue;
            }
            let endpoint = mailbox.endpoint.ok_or(Error::Unavailable)?;
            mailbox.peer_admission = permit;
            return Poll::Ready(Ok(Rc::new(Self {
                lease: Rc::new(Lease {
                    slot: slot.clone(),
                    shared: device.port.shared.clone(),
                }),
                endpoint,
                connecting: RefCell::new(None),
                connected: Cell::new(false),
                pending: RefCell::new(None),
                failure: Cell::new(None),
                expires: Cell::new(None),
            })));
        }
        if contended {
            Poll::Pending
        } else {
            Poll::Ready(Err(Error::Overloaded))
        }
    }
    /// Queue one command after observing the previous completion.
    fn poll_submit(&self, command: Command, window: Option<Rc<Window>>) -> Poll<Result<Ticket>> {
        if self.lease.slot.cancel.load(Ordering::Acquire)
            || self.lease.shared.closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
        if let Some(ticket) = self.pending.borrow().as_ref() {
            match ready!(ticket.poll_result()) {
                None => return Poll::Ready(Err(Error::Overloaded)),
                Some(result) => result?,
            }
        }
        let mut mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        if mailbox.command.is_some() {
            return Poll::Ready(Err(Error::Overloaded));
        }
        mailbox.result = None;
        mailbox.command = Some(command);
        let ticket = Ticket(Rc::new(TicketState {
            lease: self.lease.clone(),
            result: Cell::new(None),
            window,
        }));
        *self.pending.borrow_mut() = Some(ticket.clone());
        self.lease.shared.engine.wake();
        Poll::Ready(Ok(ticket))
    }
    /// Validate and submit connection setup; completion requires `poll_connected`.
    pub fn poll_connect(&self, remote: Endpoint) -> Poll<Result<()>> {
        remote.validate()?;
        if self.connecting.borrow().is_some() || remote.link_layer != self.endpoint.link_layer {
            return Poll::Ready(Err(Error::InvalidRequest));
        }
        *self.connecting.borrow_mut() =
            Some(ready!(self.poll_submit(Command::Connect(remote), None))?);
        Poll::Ready(Ok(()))
    }
    /// Report a connected, uncanceled queue pair without a recorded failure.
    pub fn ready(&self) -> bool {
        self.connected.get()
            && self.failure.get().is_none()
            && !self.lease.shared.closed.load(Ordering::Acquire)
            && !self.lease.slot.cancel.load(Ordering::Acquire)
    }
    /// Report whether the native terminal fence has completed.
    pub fn stopped(&self) -> bool {
        self.lease.slot.fenced.load(Ordering::Acquire)
    }
    /// Set the deadline checked by subsequent I/O progress calls.
    pub fn expire_at(&self, deadline: std::time::Instant) {
        self.expires.set(Some(deadline));
    }
    /// Bind this queue pair's region and return its unpublished window and ticket.
    pub fn poll_bind(&self, region: Rc<Region>) -> Poll<Result<(Rc<Window>, Ticket)>> {
        if !self.ready() || !Rc::ptr_eq(&region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let window = Rc::new(Window {
            key: Cell::new(0),
            address: Cell::new(0),
            _region: region,
        });
        let ticket = ready!(self.poll_submit(Command::Bind, Some(window.clone())))?;
        Poll::Ready(Ok((window, ticket)))
    }
    /// Submit local invalidation; its completion alone does not permit readback.
    pub fn poll_invalidate(
        &self,
        window: Rc<Window>,
        cx: &mut Context<'_>,
    ) -> Poll<Result<Ticket>> {
        self.lease.slot.waiter.register(cx.waker());
        if !self.ready() || !Rc::ptr_eq(&window._region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.poll_submit(Command::Invalidate, None)
    }
    /// Submit this queue pair's region to a caller-authenticated remote capability.
    pub fn poll_write(&self, region: Rc<Region>, address: u64, key: u32) -> Poll<Result<Ticket>> {
        if !self.ready() || !Rc::ptr_eq(&region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.poll_submit(Command::Write { address, key }, None)
    }
    /// Register for this queue pair's native progress notifications.
    pub fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }
    /// Observe connection/completion state and request stop on failure or expiry.
    pub fn progress(&self) -> Result<usize> {
        if self.lease.shared.closed.load(Ordering::Acquire) && !self.stopped() {
            self.failure.set(Some(Error::Unavailable));
        }
        if self
            .expires
            .get()
            .is_some_and(|d| uring_runtime::environment::now() >= d)
            && !self.stopped()
        {
            self.failure.set(Some(Error::DeadlineExceeded));
        }
        if let Some(ticket) = self.connecting.borrow().as_ref()
            && ticket.result() == Some(Ok(()))
        {
            self.connected.set(true);
        }
        let mut count = 0;
        if let Some(ticket) = self.pending.borrow().as_ref()
            && let Some(result) = ticket.result()
        {
            count = 1;
            if let Err(error) = result {
                self.failure.set(Some(error));
            }
        }
        if let Some(error) = self.failure.get() {
            self.stop();
            self.lease.slot.waiter.wake();
            return Err(error);
        }
        Ok(count)
    }
    /// Cancellation request only. poll_stopped is the actual asynchronous fence.
    pub fn stop(&self) {
        self.lease.slot.cancel.store(true, Ordering::Release);
        self.lease.shared.engine.wake();
    }
    /// Request stop and await its actual fence, or report pool unavailability.
    pub fn poll_stopped(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.lease.slot.waiter.register(cx.waker());
        self.stop();
        if self.stopped() {
            Poll::Ready(Ok(()))
        } else if self.lease.shared.closed.load(Ordering::Acquire) {
            Poll::Ready(Err(Error::Unavailable))
        } else {
            Poll::Pending
        }
    }
    /// Observe progress and await completion of the submitted connection setup.
    pub fn poll_connected(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.progress()?;
        let poll = self
            .connecting
            .borrow()
            .as_ref()
            .ok_or(Error::Unavailable)?
            .poll(cx);
        if poll == Poll::Ready(Ok(())) {
            self.connected.set(true);
        }
        poll
    }
}
impl Drop for QueuePairHandle {
    /// Request cancellation without performing native work on the I/O thread.
    fn drop(&mut self) {
        self.stop();
    }
}

/// A slot that has not been published for claims.
pub(crate) const IDLE: u8 = 0;
/// A provisioned slot available for an I/O claim.
pub(crate) const READY: u8 = 1;
/// A slot held by an I/O lease.
pub(crate) const OWNED: u8 = 2;
/// A slot awaiting release or successful replenishment.
pub(crate) const RETIRED: u8 = 3;

/// One safe operation to execute on the native owning thread.
pub(crate) enum Command {
    /// Connect to a validated remote endpoint.
    Connect(Endpoint),
    /// Bind the slot's region to a fresh memory window.
    Bind,
    /// Write the staged transfer using the caller's remote capability.
    Write { address: u64, key: u32 },
    /// Revoke the slot's current remote memory-window capability.
    Invalidate,
}
/// Bounded staging, command, and completion handoff for one slot.
pub(crate) struct Mailbox {
    pub peer_admission: Option<Guard>,
    pub endpoint: Option<Endpoint>,
    pub rail: u32,
    pub length: usize,
    pub bytes: Vec<u8>,
    pub command: Option<Command>,
    pub result: Option<Result<()>>,
    pub descriptor: Option<(u64, u32)>,
    pub quota: Option<Arc<GuardOwner>>,
}
/// Atomic lease lifecycle and protected I/O/native handoff state.
pub(crate) struct Slot {
    pub state: AtomicU8,
    pub cancel: AtomicBool,
    pub released: AtomicBool,
    pub fenced: AtomicBool,
    pub mailbox: Mutex<Mailbox>,
    pub waiter: AtomicWaker,
}
impl Drop for Slot {
    /// Quarantine an admission guard if native teardown never established a fence.
    fn drop(&mut self) {
        // Failed native teardown may intentionally outlive both paired roles.
        // The bounded admission slot must quarantine with that DMA ownership.
        if !self.fenced.load(Ordering::Acquire) {
            let mailbox = self.mailbox.get_mut().unwrap_or_else(|e| e.into_inner());
            if let Some(permit) = mailbox.peer_admission.take() {
                std::mem::forget(permit);
            }
        }
    }
}
/// Cross-thread pool admission, configuration, and wake state.
pub(crate) struct Shared {
    pub slots: Vec<Arc<Slot>>,
    pub engine: AtomicWaker,
    pub io: AtomicWaker,
    pub closed: AtomicBool,
    pub generation: AtomicU64,
    drained: AtomicBool,
    alive: AtomicBool,
    configured: AtomicBool,
    config: Mutex<Option<Configuration>>,
    activation: Mutex<Option<Result<Vec<Selection>>>>,
}
/// Native-thread selection callback mapping inventory indices to caller tags.
type PortSelector = dyn FnOnce(&[PortInfo]) -> Result<Vec<(u32, usize)>> + Send;
impl Drop for NativePort {
    /// Mark the native role gone and notify all remaining I/O waiters.
    fn drop(&mut self) {
        self.shared.alive.store(false, Ordering::Release);
        self.shared.closed.store(true, Ordering::Release);
        for slot in &self.shared.slots {
            slot.waiter.wake();
        }
        self.shared.io.wake();
    }
}

/// Create paired ports with a fixed capacity of 1 through 256 slots.
pub fn pair(slots: usize) -> Result<(IoPort, NativePort)> {
    if slots == 0 || slots > 256 {
        return Err(Error::InvalidConfiguration);
    }
    let shared = Arc::new(Shared {
        slots: (0..slots)
            .map(|_| {
                Arc::new(Slot {
                    state: AtomicU8::new(IDLE),
                    cancel: AtomicBool::new(false),
                    released: AtomicBool::new(false),
                    fenced: AtomicBool::new(false),
                    mailbox: Mutex::new(Mailbox {
                        peer_admission: None,
                        endpoint: None,
                        rail: 0,
                        length: 0,
                        bytes: Vec::new(),
                        command: None,
                        result: None,
                        descriptor: None,
                        quota: None,
                    }),
                    waiter: AtomicWaker::new(),
                })
            })
            .collect(),
        engine: AtomicWaker::new(),
        io: AtomicWaker::new(),
        closed: AtomicBool::new(false),
        generation: AtomicU64::new(1),
        drained: AtomicBool::new(false),
        alive: AtomicBool::new(true),
        configured: AtomicBool::new(false),
        config: Mutex::new(None),
        activation: Mutex::new(None),
    });
    Ok((
        IoPort {
            shared: shared.clone(),
        },
        NativePort { shared },
    ))
}
impl IoPort {
    /// Submit configuration and await activation under the caller's scope.
    /// Dropping the wait after submission closes admission but does not bypass fences.
    pub async fn activate<S: Scope>(
        &self,
        configuration: Configuration,
        scope: &S,
    ) -> std::result::Result<Vec<Selection>, S::Error>
    where
        S::Error: From<Error>,
    {
        let mut configure = std::pin::pin!(self.configure(configuration));
        poll_scoped(scope, |cx| {
            std::future::Future::poll(configure.as_mut(), cx)
        })
        .await?;
        /// Close admission if a submitted activation wait is abandoned.
        struct ActivationGuard<'a> {
            port: &'a IoPort,
            completed: bool,
        }
        impl Drop for ActivationGuard<'_> {
            /// Close admission unless activation completed successfully.
            fn drop(&mut self) {
                if !self.completed {
                    self.port.close();
                }
            }
        }
        let mut guard = ActivationGuard {
            port: self,
            completed: false,
        };
        let cancel = scope.cancellation().map(|c| c.subscribe()).transpose()?;
        let mappings = std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            if let Some(cancel) = &cancel {
                cancel.register(cx.waker());
            }
            if self.closed() {
                return Poll::Ready(Err(Error::Unavailable.into()));
            }
            if let Err(error) = scope.check() {
                self.close();
                return Poll::Ready(Err(error));
            }
            self.activation()
                .map(|r| r.map_err(Into::into))
                .map_or(Poll::Pending, Poll::Ready)
        })
        .await?;
        guard.completed = true;
        Ok(mappings)
    }

    /// Capture a caller tag and the current pool generation in an I/O handle.
    pub fn device(self: &Rc<Self>, tag: u32) -> Rc<DeviceHandle> {
        Rc::new(DeviceHandle {
            port: self.clone(),
            rail: tag,
            generation: self.shared.generation.load(Ordering::Acquire),
        })
    }
    /// Report whether I/O admission is closed.
    pub fn closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }
    /// Return the pool's fixed number of slots.
    pub fn capacity(&self) -> usize {
        self.shared.slots.len()
    }
    /// Register the I/O driver for activation and native progress notifications.
    pub fn register_driver(&self, waker: &Waker) {
        self.shared.io.register(waker);
    }
    /// Reopen only after the native role has destroyed every device owner and
    /// all I/O leases have released their fenced staging. No timer is a fence.
    pub fn reopen(&self) -> Result<()> {
        if !self.shared.alive.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if !self.shared.closed.load(Ordering::Acquire) {
            return Ok(());
        }
        if !self.shared.drained.load(Ordering::Acquire) {
            return Err(Error::Overloaded);
        }
        let generation = self
            .shared
            .generation
            .load(Ordering::Acquire)
            .checked_add(1)
            .ok_or(Error::Unavailable)?;
        for slot in &self.shared.slots {
            slot.cancel.store(false, Ordering::Release);
            slot.released.store(false, Ordering::Release);
            slot.fenced.store(false, Ordering::Release);
            slot.state.store(IDLE, Ordering::Release);
        }
        self.shared.generation.store(generation, Ordering::Release);
        self.shared.configured.store(false, Ordering::Release);
        self.shared.drained.store(false, Ordering::Release);
        self.shared.closed.store(false, Ordering::Release);
        self.shared.engine.wake();
        Ok(())
    }
    /// Submit an owned plan, retaining it across contention without scope checks.
    /// After submission, abandoned activation requires close and native progress.
    pub async fn configure(&self, configuration: Configuration) -> Result<()> {
        let mut configuration = Some(configuration);
        std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            if self.shared.closed.load(Ordering::Acquire)
                || configuration.as_ref().unwrap().guards.len() != self.capacity()
            {
                return std::task::Poll::Ready(Err(Error::Unavailable));
            }
            let mut config = std::task::ready!(try_mailbox(&self.shared.config))?;
            if config.is_some()
                || self
                    .shared
                    .slots
                    .iter()
                    .any(|s| s.state.load(Ordering::Acquire) != IDLE)
            {
                return std::task::Poll::Ready(Err(Error::InvalidConfiguration));
            }
            let mut activation = std::task::ready!(try_mailbox(&self.shared.activation))?;
            if self.shared.configured.swap(true, Ordering::AcqRel) {
                return std::task::Poll::Ready(Err(Error::InvalidConfiguration));
            }
            *activation = None;
            *config = configuration.take();
            self.shared.engine.wake();
            std::task::Poll::Ready(Ok(()))
        })
        .await
    }
    /// Consume an activation result, returning `None` before publication or on contention.
    pub fn activation(&self) -> Option<Result<Vec<Selection>>> {
        match self.shared.activation.try_lock() {
            Ok(mut activation) => activation.take(),
            Err(std::sync::TryLockError::WouldBlock) => None,
            Err(std::sync::TryLockError::Poisoned(_)) => Some(Err(Error::Io)),
        }
    }
    /// Close admission and request cancellation without waiting for native fences.
    pub fn close(&self) {
        self.shared.drained.store(false, Ordering::Release);
        self.shared.closed.store(true, Ordering::Release);
        for slot in &self.shared.slots {
            slot.cancel.store(true, Ordering::Release);
        }
        self.shared.engine.wake();
    }
}
impl Drop for IoPort {
    /// Close admission when the paired I/O role releases its final port owner.
    fn drop(&mut self) {
        self.close();
    }
}

/// Native owners and retry state for one provisioned slot.
struct Resource {
    device: Rc<ffi::NativeDevice>,
    region: Rc<ffi::NativeRegion>,
    qp: Option<Rc<ffi::NativeQueuePair>>,
    window: Option<Rc<ffi::Window>>,
    pending: Option<ffi::Ticket>,
    stopping: bool,
    next_retry: Option<std::time::Instant>,
}
/// Selected devices and guards retained during incremental provisioning.
struct Activation {
    devices: Vec<Rc<ffi::NativeDevice>>,
    selected: Vec<Selection>,
    quotas: std::vec::IntoIter<Guard>,
    bytes: usize,
    next: usize,
}
/// Result of one bounded discovery or slot-provisioning step.
enum ActivationStep {
    /// Keep provisioning, or retry this step after mailbox contention.
    Pending,
    /// Publish the complete selected-port mapping to the I/O role.
    Complete(Vec<Selection>),
}
impl NativeService {
    /// Capture the current runtime and simulation environments on the native thread.
    pub fn new(port: NativePort) -> Self {
        let resources = (0..port.shared.slots.len()).map(|_| None).collect();
        Self {
            environment: uring_runtime::environment::Environment::current(),
            port,
            resources,
            activation: None,
            cursor: 0,
            #[cfg(any(test, feature = "simulation"))]
            simulation: simulation::current(),
        }
    }
    /// Register the native driver for commands and cancellation requests.
    pub fn register_driver(&self, waker: &Waker) {
        self.port.shared.engine.register(waker);
    }
    /// Discover and select ports, retaining nonempty plans for provisioning.
    fn begin_activation(&mut self, config: Configuration) -> Result<ActivationStep> {
        #[cfg(any(test, feature = "simulation"))]
        let _environment = self.simulation.as_ref().map(simulation::Simulation::enter);
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if config.bytes == 0 || config.bytes > u32::MAX as usize {
            return Err(Error::InvalidRange);
        }
        let discovered = if config.discover {
            ffi::discover()?
        } else {
            Vec::new()
        };
        let descriptions = discovered
            .iter()
            .map(|d| PortInfo {
                device: d.name.clone(),
                port: d.endpoint.port,
                gid: d.endpoint.gid,
                numa_node: d.numa_node(),
            })
            .collect::<Vec<_>>();
        let plan = (config.selector)(&descriptions)?;
        let mut selected: Vec<Selection> = Vec::new();
        for (tag, index) in plan {
            if selected.iter().any(|s| s.tag == tag || s.index == index) {
                return Err(Error::InvalidConfiguration);
            }
            let port = descriptions
                .get(index)
                .ok_or(Error::InvalidConfiguration)?
                .clone();
            selected.push(Selection { tag, index, port });
        }
        if selected.is_empty() {
            return Ok(ActivationStep::Complete(Vec::new()));
        }
        if selected.len() > self.resources.len() {
            return Err(Error::Overloaded);
        }
        self.activation = Some(Activation {
            devices: discovered.into_iter().map(Rc::new).collect(),
            selected,
            quotas: config.guards.into_iter(),
            bytes: config.bytes,
            next: 0,
        });
        Ok(ActivationStep::Pending)
    }
    /// Provision one slot and publish readiness only after all slots succeed.
    fn activate_slot(&mut self) -> Result<ActivationStep> {
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        let activation = self.activation.as_mut().unwrap();
        let i = activation.next;
        let slot = &self.port.shared.slots[i];
        let mut mailbox = match slot.mailbox.try_lock() {
            Ok(mailbox) => mailbox,
            Err(std::sync::TryLockError::WouldBlock) => return Ok(ActivationStep::Pending),
            Err(std::sync::TryLockError::Poisoned(_)) => return Err(Error::Io),
        };
        let selected = &activation.selected[i % activation.selected.len()];
        let device = activation.devices[selected.index].clone();
        let quota = GuardOwner::new(
            activation
                .quotas
                .next()
                .ok_or(Error::InvalidConfiguration)?,
        );
        // Retain a quota observer even if registration/destruction fails. Partial
        // resources stay IDLE and are fenced by the normal close/retry path when
        // the I/O activation guard observes an error or is canceled/dropped.
        mailbox.quota = Some(quota.clone());
        mailbox
            .bytes
            .try_reserve_exact(activation.bytes)
            .map_err(|_| Error::Overloaded)?;
        mailbox.bytes.resize(activation.bytes, 0);
        let region = ffi::NativeRegion::new(device.clone(), activation.bytes, quota)?;
        self.resources[i] = Some(Resource {
            device: device.clone(),
            region,
            qp: None,
            window: None,
            pending: None,
            stopping: false,
            next_retry: None,
        });
        let resource = self.resources[i].as_mut().unwrap();
        resource.qp = Some(ffi::NativeQueuePair::new(device)?);
        let qp = resource.qp.as_ref().unwrap();
        qp.probe_window()?;
        mailbox.rail = selected.tag;
        mailbox.endpoint = Some(qp.endpoint);
        activation.next += 1;
        if activation.next != self.resources.len() {
            return Ok(ActivationStep::Pending);
        }
        // No native work in this publication pass. No slot is claimable until
        // every slot has successfully provisioned, and I/O awaits the result.
        for slot in &self.port.shared.slots {
            slot.state.store(READY, Ordering::Release);
        }
        Ok(ActivationStep::Complete(activation.selected.to_vec()))
    }
    /// Run at most budget steps: discovery, one slot's provisioning, or one slot's
    /// normal progress/fence. Native provider calls (including discovery's bounded
    /// port scan) cannot be preempted. This is a work bound, not a wall-time bound;
    /// a provider may stall all siblings on this crypto thread, but not their I/O.
    pub fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        let _environment = self.environment.enter();
        if budget == 0 {
            return Ok(());
        }
        for _ in 0..budget.min(self.resources.len()) {
            let result = if self.activation.is_some() {
                Some(self.activate_slot())
            } else {
                let config = self
                    .port
                    .shared
                    .config
                    .lock()
                    .map_err(|_| Error::Io)?
                    .take();
                config.map(|config| self.begin_activation(config))
            };
            if let Some(result) = result {
                let completed = match result {
                    Ok(ActivationStep::Pending) => continue,
                    Ok(ActivationStep::Complete(selected)) => Ok(selected),
                    Err(error) => Err(error),
                };
                self.activation = None;
                *self.port.shared.activation.lock().map_err(|_| Error::Io)? = Some(completed);
                self.port.shared.io.wake();
                continue;
            }
            let index = self.cursor;
            self.cursor = (self.cursor + 1) % self.resources.len();
            self.drive(index);
        }
        if !self.port.shared.drained.load(Ordering::Acquire)
            && self.port.shared.closed.load(Ordering::Acquire)
            && self.activation.is_none()
            && self.resources.iter().all(Option::is_none)
        {
            let mut drained = true;
            for slot in &self.port.shared.slots {
                let Ok(mut mailbox) = slot.mailbox.try_lock() else {
                    drained = false;
                    continue;
                };
                if slot.state.load(Ordering::Acquire) == RETIRED
                    && !slot.released.load(Ordering::Acquire)
                {
                    drained = false;
                    continue;
                }
                // Backend destruction failures retain another quota owner.
                if mailbox
                    .quota
                    .as_ref()
                    .is_some_and(|q| Arc::strong_count(q) != 1)
                {
                    drained = false;
                    continue;
                }
                mailbox.bytes.clear();
                mailbox.bytes.shrink_to_fit();
                mailbox.quota = None;
                mailbox.endpoint = None;
                mailbox.command = None;
                mailbox.result = None;
                mailbox.descriptor = None;
                mailbox.length = 0;
            }
            if drained {
                self.port.shared.drained.store(true, Ordering::Release);
                self.port.shared.io.wake();
            }
        }
        Ok(())
    }
    /// Progress a slot's command, terminal fence, or replenishment attempt.
    fn drive(&mut self, index: usize) {
        let Some(resource) = self.resources[index].as_mut() else {
            return;
        };
        let slot = &self.port.shared.slots[index];
        let closed = self.port.shared.closed.load(Ordering::Acquire);
        let Ok(mut mailbox) = slot.mailbox.try_lock() else {
            return;
        };
        let state = slot.state.load(Ordering::Acquire);
        if closed || slot.cancel.load(Ordering::Acquire) {
            resource.stopping = true;
        }
        if resource.stopping {
            if resource
                .next_retry
                .is_some_and(|at| uring_runtime::environment::now() < at)
            {
                return;
            }
            if let Some(qp) = &resource.qp
                && qp.stop().is_err()
            {
                resource.next_retry =
                    Some(uring_runtime::environment::now() + std::time::Duration::from_millis(10));
                return; // quarantine, retry on next service turn
            }
            if resource.window.is_some()
                && !slot.fenced.load(Ordering::Acquire)
                && resource
                    .region
                    .copy_into(&mut mailbox.bytes[..resource.region.length()])
                    .is_err()
            {
                return;
            }
            resource.pending = None;
            resource.window = None;
            resource.qp = None; // CQ destruction on crypto, never I/O
            slot.fenced.store(true, Ordering::Release);
            mailbox.peer_admission = None;
            mailbox.command = None;
            if mailbox.result.is_none() {
                mailbox.result = Some(Err(Error::Cancelled));
            }
            slot.waiter.wake();
            self.port.shared.io.wake();
            if closed {
                // Only the service owns native region/PD teardown.
                self.resources[index] = None;
                // A fenced receiver may still need its staging copy. That copy
                // and its quota survive with the slot's outstanding I/O leases.
                if state != OWNED || slot.released.load(Ordering::Acquire) {
                    slot.released.store(true, Ordering::Release);
                }
                slot.state.store(RETIRED, Ordering::Release);
                return;
            }
            if !slot.released.load(Ordering::Acquire) {
                return;
            }
            // Replenish the QP outside request turns. MR stays registered for the
            // entire bounded pool lifetime. Never reuse a QP/remote capability.
            match ffi::NativeQueuePair::new(resource.device.clone()) {
                Ok(qp) => {
                    mailbox.endpoint = Some(qp.endpoint);
                    resource.qp = Some(qp);
                }
                Err(_) => {
                    resource.next_retry = Some(
                        uring_runtime::environment::now() + std::time::Duration::from_millis(100),
                    );
                    slot.state.store(RETIRED, Ordering::Release);
                    return;
                }
            }
            resource.stopping = false;
            resource.next_retry = None;
            mailbox.result = None;
            mailbox.descriptor = None;
            mailbox.length = 0;
            slot.cancel.store(false, Ordering::Release);
            slot.released.store(false, Ordering::Release);
            slot.fenced.store(false, Ordering::Release);
            slot.state.store(READY, Ordering::Release);
            self.port.shared.io.wake();
            return;
        }
        if state != OWNED {
            return;
        }
        let qp = resource.qp.as_ref().unwrap();
        if let Some(ticket) = &resource.pending {
            if let Err(error) = qp.progress() {
                mailbox.result = Some(Err(error));
                resource.stopping = true;
            } else if let Some(result) = ticket.result() {
                mailbox.result = Some(result);
                resource.pending = None;
            }
            if mailbox.result.is_some() {
                slot.waiter.wake();
                self.port.shared.io.wake();
            }
            return;
        }
        let Some(command) = mailbox.command.take() else {
            return;
        };
        let result = (|| -> Result<()> {
            match command {
                Command::Connect(remote) => qp.connect(remote)?,
                Command::Bind => {
                    resource.region.resize(mailbox.length)?;
                    let (window, ticket) = qp.bind(resource.region.clone())?;
                    mailbox.descriptor = Some((resource.region.address(), window.key));
                    resource.window = Some(window);
                    resource.pending = Some(ticket);
                }
                Command::Write { address, key } => {
                    resource.region.resize(mailbox.length)?;
                    resource
                        .region
                        .copy_from(&mailbox.bytes[..mailbox.length])?;
                    resource.pending = Some(qp.write(resource.region.clone(), address, key)?);
                }
                Command::Invalidate => {
                    resource.pending =
                        Some(qp.invalidate(resource.window.clone().ok_or(Error::InvalidRequest)?)?)
                }
            }
            Ok(())
        })();
        if result.is_err() {
            resource.stopping = true;
        }
        if resource.pending.is_none() || result.is_err() {
            mailbox.result = Some(result);
            slot.waiter.wake();
            self.port.shared.io.wake();
        }
    }
    /// Close native admission and notify the paired I/O role.
    pub fn close(&self) {
        self.port.shared.closed.store(true, Ordering::Release);
        self.port.shared.io.wake();
    }
    /// Report that activation and service-owned resources have drained.
    /// Outstanding leases or quarantine can still prevent [`IoPort::reopen`].
    pub fn drained(&self) -> bool {
        self.activation.is_none()
            && self
                .port
                .shared
                .config
                .lock()
                .is_ok_and(|config| config.is_none())
            && self.resources.iter().all(Option::is_none)
    }
}
impl Drop for NativeService {
    /// Drop queue pairs before other native owners, preserving FFI quarantine rules.
    fn drop(&mut self) {
        self.close();
        // Drop native QPs before their region owners. On failure the FFI boundary
        // keeps its own DMA references and quota, even after this service dies.
        for resource in self.resources.iter_mut().flatten() {
            resource.qp.take();
            resource.window.take();
            resource.pending.take();
        }
        for slot in &self.port.shared.slots {
            slot.waiter.wake();
        }
    }
}

impl Window {
    /// Return the key, populated after a successful bind result is observed.
    pub fn key(&self) -> u32 {
        self.key.get()
    }
    /// Return the address, populated after a successful bind result is observed.
    pub fn address(&self) -> u64 {
        self.address.get()
    }
}

impl<T> WithNative<T> {
    /// Attach a port on the thread that will own native resources.
    pub fn new(inner: T, port: NativePort) -> Self {
        Self {
            inner,
            native: NativeService::new(port),
        }
    }
}
impl<S: Scope, T: Service<S>> Service<S> for WithNative<T>
where
    S::Error: From<Error>,
{
    /// Forward asynchronous failure reporting to the inner service.
    fn set_failure_reporter(&mut self, reporter: FailureReporter<S::Error>) {
        self.inner.set_failure_reporter(reporter);
    }
    /// Return the inner service's driver waker.
    fn waker(&self) -> std::result::Result<Waker, S::Error> {
        self.inner.waker()
    }
    /// Register both services for the same runtime driver notifications.
    fn register_driver(&self, waker: &Waker) {
        self.inner.register_driver(waker);
        self.native.register_driver(waker);
    }
    /// Start the inner service; native resources activate through their I/O port.
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.start(scope)
    }
    /// Drive the inner service first, then native progress with the same work budget.
    fn poll_budgeted(
        &mut self,
        cx: &mut Context<'_>,
        budget: usize,
    ) -> std::result::Result<(), S::Error> {
        self.inner.poll_budgeted(cx, budget)?;
        self.native.poll_budgeted(budget).map_err(Into::into)
    }
    /// Stop inner admission while keeping native progress available to accepted work.
    fn stop_admission(&mut self) -> std::result::Result<(), S::Error> {
        self.inner.stop_admission()
    }
    /// Fence native resources before invoking the inner drain hook.
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.native.close_and_fence().await?;
            self.inner.drain(scope).await
        })
    }
    /// Close native admission even when the subsequent inner close fails.
    fn close(&mut self) -> std::result::Result<(), S::Error> {
        self.native.close();
        self.inner.close()
    }
    /// Finish native fences before awaiting the inner ownership fence.
    fn fence<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.native.close_and_fence().await?;
            self.inner.fence(scope).await
        })
    }
    /// Forward shutdown to the inner service without changing its scope.
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.shutdown(scope)
    }
}
impl NativeService {
    /// Drive actual native fences without checking request cancellation.
    async fn close_and_fence(&mut self) -> Result<()> {
        self.close();
        std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            self.poll_budgeted(1)?;
            if self.drained() {
                Poll::Ready(Ok(()))
            } else {
                // Periodic worker ticks retry fences even without a completion wake.
                Poll::Pending
            }
        })
        .await
    }
}

/// Unique ownership counter, independent of any caller-retained guard clones.
struct GuardOwner {
    _guard: Guard,
}
impl GuardOwner {
    /// Wrap a guard so quarantine ownership can be counted independently.
    fn new(guard: Guard) -> Arc<Self> {
        Arc::new(Self { _guard: guard })
    }
}

/// Try a mailbox without blocking, distinguishing contention from poisoning.
/// Retain owners across Pending; periodic driver ticks cover unlocks without wakes.
fn try_mailbox<T>(mailbox: &Mutex<T>) -> Poll<Result<MutexGuard<'_, T>>> {
    match mailbox.try_lock() {
        Ok(guard) => Poll::Ready(Ok(guard)),
        Err(TryLockError::WouldBlock) => Poll::Pending,
        Err(TryLockError::Poisoned(_)) => Poll::Ready(Err(Error::Io)),
    }
}

/// Narrow observation and contention controls for deterministic integration tests.
/// These never expose a mailbox, native owner, pointer, or lock guard.
#[cfg(feature = "simulation")]
pub mod testing {
    use super::*;
    /// Observable lifecycle state of a bounded pool slot.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum State {
        /// Not yet published for claims.
        Idle,
        /// Provisioned and available for a claim.
        Ready,
        /// Held by an I/O lease.
        Owned,
        /// Awaiting release or successful replenishment.
        Retired,
    }
    /// Scalar slot observations without exposing mailbox or native ownership.
    #[derive(Clone, Copy, Debug)]
    pub struct Snapshot {
        /// Current claim/replenishment state.
        pub state: State,
        /// Whether cancellation has been requested.
        pub cancelled: bool,
        /// Whether the terminal native fence has completed.
        pub fenced: bool,
    }
    /// Mailbox to hold while exercising a nonblocking caller operation.
    pub enum Contention {
        /// Hold one slot's command and staging mailbox.
        Slot(usize),
        /// Hold the pool configuration mailbox.
        Configuration,
        /// Hold the activation result mailbox.
        Activation,
    }
    impl IoPort {
        /// Run a closure with the selected mailbox locked for deterministic contention.
        /// The closure must not invoke an operation that blocks on the same mailbox.
        pub fn with_contention<T>(&self, at: Contention, f: impl FnOnce() -> T) -> T {
            match at {
                Contention::Slot(i) => {
                    let _lock = self.shared.slots[i].mailbox.lock().unwrap();
                    f()
                }
                Contention::Configuration => {
                    let _lock = self.shared.config.lock().unwrap();
                    f()
                }
                Contention::Activation => {
                    let _lock = self.shared.activation.lock().unwrap();
                    f()
                }
            }
        }
        /// Read the selected slot's scalar lifecycle state.
        pub fn snapshot(&self, i: usize) -> Snapshot {
            let slot = &self.shared.slots[i];
            Snapshot {
                state: match slot.state.load(Ordering::Acquire) {
                    IDLE => State::Idle,
                    READY => State::Ready,
                    OWNED => State::Owned,
                    _ => State::Retired,
                },
                cancelled: slot.cancel.load(Ordering::Acquire),
                fenced: slot.fenced.load(Ordering::Acquire),
            }
        }
        /// Report whether this generation has accepted a configuration.
        pub fn configuration_submitted(&self) -> bool {
            self.shared.configured.load(Ordering::Acquire)
        }
        /// Report the stronger pool-reuse condition, including released I/O leases.
        pub fn pool_drained(&self) -> bool {
            self.shared.drained.load(Ordering::Acquire)
        }
        /// Return the generation used to reject stale device handles.
        pub fn generation(&self) -> u64 {
            self.shared.generation.load(Ordering::Acquire)
        }
        /// Inspect whether a slot has a command queued for native submission.
        pub fn command_pending(&self, i: usize) -> bool {
            self.shared.slots[i]
                .mailbox
                .lock()
                .unwrap()
                .command
                .is_some()
        }
    }
    impl NativeService {
        /// Count slots still holding service-owned native resources.
        pub fn resource_count(&self) -> usize {
            self.resources.iter().flatten().count()
        }
        /// Report whether a particular slot retains service-owned native resources.
        pub fn resource_present(&self, i: usize) -> bool {
            self.resources[i].is_some()
        }
        /// Clear a slot's backoff so its next native turn retries immediately.
        pub fn retry_now(&mut self, i: usize) {
            self.resources[i].as_mut().unwrap().next_retry = None;
        }
    }
}

#[cfg(test)]
mod mailbox_tests {
    //! Hold native mailboxes at each public handoff, independently of thread timing.
    use super::test_guard::ready;
    use super::tests::{claim, mark_connected, provision_test};
    use super::*;

    /// Contention yields, while an outstanding accepted command rejects another.
    #[test]
    fn command_poll_distinguishes_contended_completion_from_full_queue() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        let qp = claim(&io);
        mark_connected(&qp, &mut native);
        let region = ready(Region::poll_acquire(&qp, 32)).unwrap();
        assert!(matches!(
            Region::poll_acquire(&qp, 32),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(qp.poll_write(region.clone(), 4096, 7).is_pending());
        drop(guard);
        assert!(matches!(
            qp.poll_write(region.clone(), 4096, 7),
            Poll::Ready(Ok(_))
        ));
        assert!(matches!(
            qp.poll_write(region.clone(), 4096, 7),
            Poll::Ready(Err(Error::Overloaded))
        ));
        native.poll_budgeted(1).unwrap();
        ffi::lifetime_tests::complete(1, 0, 1);
        native.poll_budgeted(1).unwrap();
        let guard = io.shared.slots[0].mailbox.lock().unwrap();
        assert!(qp.poll_write(region.clone(), 4096, 7).is_pending());
        drop(guard);
        assert!(matches!(qp.poll_write(region, 4096, 7), Poll::Ready(Ok(_))));
    }

    /// Poisoning is terminal and must not look like temporary contention.
    #[test]
    fn poisoned_mailbox_is_terminal_io_error_not_contention() {
        let mutex = std::sync::Mutex::new(());
        let _ = std::panic::catch_unwind(|| {
            let _guard = mutex.lock().unwrap();
            panic!("poison test mailbox");
        });
        assert!(matches!(try_mailbox(&mutex), Poll::Ready(Err(Error::Io))));
    }
}

/// Test-only lifetime charges shared with the private native boundary tests.
#[cfg(test)]
mod test_guard {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    /// Unwrap an uncontended poll while retaining its success or failure result.
    pub(crate) fn ready<T>(poll: Poll<Result<T>>) -> Result<T> {
        match poll {
            Poll::Ready(result) => result,
            Poll::Pending => panic!("unexpected mailbox contention"),
        }
    }
    /// Observe how many lifetime charges are still retained.
    pub struct Observer(Arc<AtomicUsize>);
    impl Observer {
        /// Read the outstanding charge count.
        pub fn get(&self) -> usize {
            self.0.load(Ordering::Acquire)
        }
    }
    /// Decrement the observed count when the last guard owner disappears.
    struct Charge(Arc<AtomicUsize>);
    impl Drop for Charge {
        /// Observe final guard release without retaining the original charge.
        fn drop(&mut self) {
            self.0.fetch_sub(1, Ordering::AcqRel);
        }
    }
    /// Create one lifetime charge and its independent observer.
    pub fn guard() -> (Guard, Observer) {
        let count = Arc::new(AtomicUsize::new(1));
        (Arc::new(Charge(count.clone())), Observer(count))
    }
}

#[cfg(test)]
mod tests {
    //! Private ownership fixtures and operator-selected native regressions.
    use super::test_guard::ready;
    use super::*;
    use std::task::Context;

    /// Install a private ABI fixture while keeping its independent quota observer.
    pub(super) fn provision_test(
        service: &mut NativeService,
        index: usize,
    ) -> ffi::lifetime_tests::QuotaObserver {
        let (qp, region, charged) = ffi::lifetime_tests::fresh_fixture();
        let device = qp.device().clone();
        let slot = &service.port.shared.slots[index];
        let mut mailbox = slot.mailbox.lock().unwrap();
        mailbox.endpoint = Some(qp.endpoint);
        mailbox.rail = 0;
        mailbox.bytes = vec![0; 32];
        slot.state.store(READY, Ordering::Release);
        service.resources[index] = Some(Resource {
            device,
            region,
            qp: Some(qp),
            window: None,
            pending: None,
            stopping: false,
            next_retry: None,
        });
        charged
    }
    /// Claim tag zero in the current generation without native discovery.
    pub(super) fn claim(io: &Rc<IoPort>) -> Rc<QueuePairHandle> {
        ready(QueuePairHandle::poll_new(
            Rc::new(DeviceHandle {
                port: io.clone(),
                rail: 0,
                generation: io.shared.generation.load(Ordering::Acquire),
            }),
            None,
        ))
        .unwrap()
    }
    /// Assert that connection setup runs on native progress, never the I/O caller.
    pub(super) fn mark_connected(qp: &QueuePairHandle, service: &mut NativeService) {
        ready(qp.poll_connect(qp.endpoint)).unwrap();
        assert!(!qp.ready(), "connect is not executed on the I/O caller");
        service.poll_budgeted(8).unwrap();
        qp.progress().unwrap();
        assert!(qp.ready());
    }
    /// Reopen waits for native fencing and the last lease, then rejects stale handles.
    #[test]
    fn restart_requires_native_fence_and_last_lease_and_revokes_old_devices() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        let charged = provision_test(&mut native, 0);
        let old_device = Rc::new(DeviceHandle {
            port: io.clone(),
            rail: 0,
            generation: 1,
        });
        let qp = ready(QueuePairHandle::poll_new(old_device.clone(), None)).unwrap();
        mark_connected(&qp, &mut native);
        let region = ready(Region::poll_acquire(&qp, 16)).unwrap();
        io.close();
        ffi::lifetime_tests::fail_stop(true);
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(charged.get(), 1);
        ffi::lifetime_tests::fail_stop(false);
        native.resources[0].as_mut().unwrap().next_retry = None;
        native.poll_budgeted(1).unwrap();
        assert!(qp.stopped());
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(
            ready(region.poll_copy_to(&mut Context::from_waker(Waker::noop())))
                .unwrap()
                .len(),
            16
        );
        drop(qp);
        native.poll_budgeted(1).unwrap();
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        drop(region);
        native.poll_budgeted(1).unwrap();
        io.reopen().unwrap();
        assert_eq!(io.shared.generation.load(Ordering::Acquire), 2);
        provision_test(&mut native, 0);
        assert!(matches!(
            ready(QueuePairHandle::poll_new(old_device, None)),
            Err(Error::Unavailable)
        ));
        let fresh = claim(&io);
        mark_connected(&fresh, &mut native);
        drop(fresh);
        io.close();
        native.poll_budgeted(1).unwrap();
        assert!(native.drained());
    }
    /// I/O cancellation does not wait for native progress but fence completion does.
    #[test]
    fn io_cancel_and_poll_do_not_wait_for_native_thread_or_mailbox_lock() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        let (ready_tx, ready_rx) = std::sync::mpsc::channel();
        let (release_tx, release_rx) = std::sync::mpsc::channel();
        let thread = std::thread::spawn(move || {
            let mut service = NativeService::new(port);
            let charged = provision_test(&mut service, 0);
            ready_tx.send(()).unwrap();
            release_rx.recv().unwrap();
            service.poll_budgeted(1).unwrap();
            assert_eq!(charged.get(), 1);
            service.close();
            service.poll_budgeted(1).unwrap();
        });
        ready_rx.recv().unwrap();
        let qp = claim(&io);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        qp.stop();
        assert!(
            qp.poll_stopped(&mut cx).is_pending(),
            "must await actual native destroy"
        );
        assert!(!qp.stopped());
        // The service has not run at all while these I/O operations complete.
        release_tx.send(()).unwrap();
        thread.join().unwrap();
        assert!(qp.stopped());
        assert!(qp.poll_stopped(&mut cx).is_ready());
    }
    /// Contention on one slot does not block cancellation or a sibling session.
    #[test]
    fn mailbox_contention_never_blocks_cancel_or_other_session() {
        let (io, port) = pair(2).unwrap();
        let io = Rc::new(io);
        let mut native = NativeService::new(port);
        provision_test(&mut native, 0);
        provision_test(&mut native, 1);
        let one = claim(&io);
        let two = claim(&io);
        mark_connected(&one, &mut native);
        mark_connected(&two, &mut native);
        let _guard = io.shared.slots[0].mailbox.lock().unwrap();
        one.stop();
        assert!(!one.stopped());
        assert!(one.progress().is_ok());
        assert!(two.progress().is_ok());
        assert!(two.ready());
    }
    #[cfg(feature = "native")]
    #[test]
    #[ignore = "requires built native ABI v2 adapter and zero usable type-2B ports"]
    /// A real no-device adapter activates on the native role and releases charges.
    fn native_no_device_activation_runs_on_paired_role_and_releases_quota() {
        assert!(ffi::discover().expect("real adapter must load").is_empty());
        let (io, port) = pair(1).unwrap();
        let (guard, charged) = crate::test_guard::guard();
        futures::executor::block_on(io.configure(Configuration {
            discover: true,
            bytes: 4096,
            guards: vec![guard],
            selector: Box::new(|ports| {
                assert!(ports.is_empty());
                Err(Error::Unavailable)
            }),
        }))
        .unwrap();
        let mut activation = Box::pin(std::future::poll_fn(|_| {
            io.activation().map_or(Poll::Pending, Poll::Ready)
        }));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(charged.get(), 1);
        std::thread::spawn(move || {
            let mut service = NativeService::new(port);
            service.poll_budgeted(1).unwrap();
        })
        .join()
        .unwrap();
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Err(Error::Unavailable))
        ));
        drop(activation);
        assert_eq!(charged.get(), 0);
        assert!(io.closed());
    }
    #[cfg(feature = "native")]
    #[test]
    #[ignore = "requires operator-selected active type-2B provider; real pooled RC loopback"]
    /// Exercise pooled loopback DMA and exact-length staging on an explicit provider.
    fn native_available_provider_pooled_service_roundtrip() {
        let name = std::env::var("RDMA_VERBS_TEST_DEVICE")
            .expect("select native test provider explicitly");
        let ports = ffi::discover().expect("real native adapter");
        let selected = ports
            .iter()
            .find(|d| d.name == name)
            .expect("active type-2B provider");
        let expected = (
            selected.endpoint.port,
            selected.endpoint.gid,
            selected.numa_node(),
        );
        let (io, port) = pair(2).unwrap();
        let devices = Rc::new(io);
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(15);
        let mut native = NativeService::new(port);
        futures::executor::block_on(devices.configure(Configuration {
            discover: true,
            bytes: 4096,
            guards: vec![crate::test_guard::guard().0, crate::test_guard::guard().0],
            selector: Box::new(move |ports| {
                let index = ports
                    .iter()
                    .position(|p| p.device == name && (p.port, p.gid, p.numa_node) == expected)
                    .ok_or(Error::Unavailable)?;
                Ok(vec![(0, index)])
            }),
        }))
        .unwrap();
        let mut activate = Box::pin(std::future::poll_fn(|_| {
            devices.activation().map_or(Poll::Pending, Poll::Ready)
        }));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(activate.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(2).unwrap();
        assert!(activate.as_mut().poll(&mut cx).is_pending());
        native.poll_budgeted(1).unwrap();
        assert!(matches!(
            activate.as_mut().poll(&mut cx),
            std::task::Poll::Ready(Ok(_))
        ));
        drop(activate);
        let device = devices.device(0);
        let sender = ready(QueuePairHandle::poll_new(device.clone(), None)).unwrap();
        let receiver = ready(QueuePairHandle::poll_new(device, None)).unwrap();
        ready(sender.poll_connect(receiver.endpoint)).unwrap();
        ready(receiver.poll_connect(sender.endpoint)).unwrap();
        native.poll_budgeted(2).unwrap();
        sender.progress().unwrap();
        receiver.progress().unwrap();
        assert!(sender.ready());
        assert!(receiver.ready());
        let target = ready(Region::poll_acquire(&receiver, 17)).unwrap();
        let (window, bind) = ready(receiver.poll_bind(target.clone())).unwrap();
        /// Drive a real ticket to completion under the fixture's fixed deadline.
        fn wait(native: &mut NativeService, ticket: &Ticket, until: std::time::Instant) {
            while ticket.result().is_none() {
                assert!(std::time::Instant::now() < until);
                native.poll_budgeted(2).unwrap();
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
            ticket.result().unwrap().unwrap();
        }
        wait(&mut native, &bind, deadline);
        let source = ready(Region::poll_acquire(&sender, 17)).unwrap();
        ready(source.poll_copy_from(&[0xa5; 17])).unwrap();
        let write =
            ready(sender.poll_write(source, window.address.get(), window.key.get())).unwrap();
        wait(&mut native, &write, deadline);
        let invalidate = ready(receiver.poll_invalidate(window, &mut cx)).unwrap();
        wait(&mut native, &invalidate, deadline);
        assert_eq!(ready(target.poll_copy_to(&mut cx)), Err(Error::Unavailable));
        receiver.stop();
        sender.stop();
        native.poll_budgeted(2).unwrap();
        assert!(receiver.stopped());
        assert_eq!(ready(target.poll_copy_to(&mut cx)).unwrap(), [0xa5; 17]);
        // A short transfer used the existing 4KiB MR; it never exposed padding.
        assert_eq!(native.resources[0].as_ref().unwrap().region.length(), 17);
        native.close();
        native.poll_budgeted(2).unwrap();
        assert!(native.drained());
    }
}

/// Pure endpoint encoding and validation boundary tests.
#[cfg(test)]
mod endpoint_tests {
    use super::*;

    /// Construct valid fields at useful encoding boundaries.
    fn endpoint() -> Endpoint {
        Endpoint {
            gid: [1; 16],
            qpn: 0x123456,
            psn: 0xabcdef,
            mtu: 5,
            lid: 0x1234,
            port: 255,
            link_layer: 2,
        }
    }

    /// The wire format is fixed-width network order, not native ABI serialization.
    #[test]
    fn exact_network_byte_layout_and_roundtrip() {
        let endpoint = endpoint();
        let bytes = endpoint.to_bytes();
        assert_eq!(&bytes[..16], &[1; 16]);
        assert_eq!(
            &bytes[16..],
            &[
                0, 0x12, 0x34, 0x56, 0, 0xab, 0xcd, 0xef, 0, 0, 0, 5, 0x12, 0x34, 255, 2
            ]
        );
        assert_eq!(Endpoint::from_bytes(&bytes), Ok(endpoint));
        for len in 0..32 {
            assert_eq!(
                Endpoint::from_bytes(&bytes[..len]),
                Err(Error::InvalidRequest)
            );
        }
        assert_eq!(Endpoint::from_bytes(&[0; 33]), Err(Error::InvalidRequest));
    }

    /// Decode rejects each invalid field while preserving valid boundary values.
    #[test]
    fn decode_rejects_invalid_fields_and_accepts_boundaries() {
        let e = endpoint();
        for invalid in [
            Endpoint { gid: [0; 16], ..e },
            Endpoint { qpn: 0, ..e },
            Endpoint {
                qpn: 0x1000000,
                ..e
            },
            Endpoint {
                psn: 0x1000000,
                ..e
            },
            Endpoint { mtu: 0, ..e },
            Endpoint { mtu: 6, ..e },
            Endpoint { port: 0, ..e },
            Endpoint { link_layer: 0, ..e },
            Endpoint { link_layer: 3, ..e },
        ] {
            assert_eq!(
                Endpoint::from_bytes(&invalid.to_bytes()),
                Err(Error::InvalidRequest)
            );
        }
        for valid in [
            Endpoint {
                qpn: 1,
                psn: 0,
                mtu: 1,
                lid: 0,
                port: 1,
                link_layer: 1,
                ..e
            },
            Endpoint {
                qpn: 0xffffff,
                psn: 0xffffff,
                lid: u16::MAX,
                ..e
            },
        ] {
            assert_eq!(Endpoint::from_bytes(&valid.to_bytes()), Ok(valid));
        }
    }
}

/// Inventory filtering and metadata ordering tests with controlled filesystem input.
#[cfg(test)]
mod discovery_tests {
    use super::{PortInfo, discovery::*};
    use std::path::Path;

    /// Construct a port whose provider locality should be replaced by normalization.
    fn port(device: &str, port: u8, gid: u8) -> PortInfo {
        PortInfo {
            device: device.into(),
            port,
            gid: [gid; 16],
            numa_node: Some(99),
        }
    }

    /// Device names obey the native ABI and sysfs component limits.
    #[test]
    fn names_obey_native_and_path_bounds() {
        for name in [
            "",
            ".",
            "..",
            "a/b",
            "a\0b",
            "a\rb",
            "a\nb",
            &"x".repeat(64),
        ] {
            assert!(!valid_device(name));
        }
        for name in ["mlx5_0", "a b", "a\"b", &"x".repeat(63)] {
            assert!(valid_device(name));
        }
    }

    /// Missing metadata gives deterministic order and preserves the first duplicate.
    #[test]
    fn unknown_pci_sorts_by_port_then_name_and_keeps_first_duplicate() {
        let ports = inventory_at(
            vec![
                port("z", 2, 1),
                port("b", 1, 2),
                port("a", 1, 3),
                port("b", 1, 4),
                port("../bad", 1, 1),
                port("zero", 0, 1),
                port("gid", 1, 0),
            ],
            Path::new("/nonexistent-rdma-inventory"),
        );
        assert_eq!(
            ports
                .iter()
                .map(|p| (p.device.as_str(), p.port, p.gid[0], p.numa_node))
                .collect::<Vec<_>>(),
            [("a", 1, 3, None), ("b", 1, 2, None), ("z", 2, 1, None)]
        );
        assert!(inventory_at(vec![], Path::new(".")).is_empty());
    }

    /// PCI and NUMA normalization does not impose the caller's inventory cap.
    #[test]
    fn pci_order_and_numa_metadata_are_preserved_without_a_policy_cap() {
        /// Remove the project-local metadata fixture when the test finishes.
        struct Fixture(std::path::PathBuf);
        impl Drop for Fixture {
            /// Remove metadata files created inside the project's test directory.
            fn drop(&mut self) {
                std::fs::remove_dir_all(&self.0).unwrap();
            }
        }
        let root = Fixture(
            std::env::current_dir()
                .unwrap()
                .join(format!(".inventory-test-{}", std::process::id())),
        );
        std::fs::create_dir(&root.0).unwrap();
        for (name, bdf, numa) in [("z", "0000:01:00.0", " 7\n"), ("a", "0000:02:00.0", "-1\n")] {
            let physical = root.0.join(bdf);
            std::fs::create_dir(&physical).unwrap();
            std::fs::write(physical.join("numa_node"), numa).unwrap();
            std::fs::create_dir(root.0.join(name)).unwrap();
            std::os::unix::fs::symlink(&physical, root.0.join(name).join("device")).unwrap();
        }
        let mut input = vec![port("a", 1, 1), port("z", 2, 1), port("z", 1, 1)];
        input.extend((0..65).map(|i| port(&format!("unknown{i}"), 1, 1)));
        let ports = inventory_at(input, &root.0);
        assert_eq!(ports.len(), 68);
        assert_eq!(
            ports[..3]
                .iter()
                .map(|p| (p.device.as_str(), p.port, p.numa_node))
                .collect::<Vec<_>>(),
            [("z", 1, Some(7)), ("z", 2, Some(7)), ("a", 1, None)]
        );
    }
}
