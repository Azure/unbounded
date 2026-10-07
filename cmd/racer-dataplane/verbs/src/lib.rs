//! RDMA writes for a two-thread runtime: an I/O thread and a native thread.
//!
//! All RDMA (libibverbs) calls run on the native thread. The I/O thread never
//! touches RDMA. It talks to the native thread through a fixed pool of slots.
//!
//! - A slot is one queue pair (QP) plus one registered buffer.
//! - The I/O thread leases a slot, sends it one command at a time, and reads
//!   the result.
//! - The native thread runs the command and owns every RDMA resource.
//!
//! This crate spawns no threads and runs no executor. Time and randomness come
//! from the current `uring-runtime` environment.
//!
//! # Basics
//!
//! 1. Call [`pair`] to get an [`IoPort`] and a [`NativePort`].
//! 2. On the native thread, build a [`NativeService`] and poll it.
//! 3. On the I/O thread, call [`IoPort::activate`] with a [`Configuration`].
//!    The native thread finds RDMA ports, your selector picks some, and every
//!    slot gets a QP and a buffer.
//! 4. Get a [`DeviceHandle`] for a selected port, then lease a
//!    [`QueuePairHandle`] from it.
//! 5. Swap [`Endpoint`]s with the peer over your own channel, then connect.
//! 6. Either:
//!    - Receive: bind a [`Window`] over a [`Region`], send its address and key
//!      to the peer, and let the peer write into it.
//!    - Send: fill a [`Region`] and write it to the peer's window.
//! 7. Stop the QP and wait for [`QueuePairHandle::poll_stopped`]. Only then can
//!    you read received bytes. Dropping the handle returns the slot.
//!
//! # Stopping is the only fence
//!
//! A timeout or an invalidate does not stop the NIC from writing memory. Only a
//! finished stop does. Reads of a [`Region`] wait for it.
//!
//! If the native side cannot free something, it leaks it on purpose instead of
//! freeing memory the NIC might still use. Your [`Guard`]s leak with it, so the
//! leak counts against your limits.
//!
//! # Shutdown and reuse
//!
//! [`IoPort::close`] cancels everything. The native side then stops and frees
//! each slot. Once it has drained, [`IoPort::reopen`] can start a new round.
//! [`WithNative`] adds a [`NativeService`] to a `uring-runtime` service so
//! drain and fence also finish the native side.
//!
//! # Backends
//!
//! - `native`: loads `librdma_verbs.so.1`, built from `native/verbs.c`.
//! - `simulation`: an in-process fake fabric for tests.
//! - Neither: discovery returns [`Error::Unavailable`].
//!
//! # Limits
//!
//! - 1 to 256 slots. One region and one pending command per slot.
//! - At most 64 ports. Uses GID index 0.
//! - Poll budgets count steps, not time. A slow NIC call blocks the thread.
//! - Mailbox locks are only tried, never waited on. Poll again on a timer as
//!   well as on wakes.
//! - The caller owns auth, topology, admission sizing, retries, and timeouts.
//!
//! Hardware-free tests: `cargo test -p rdma-verbs --features simulation`.
mod ffi;

pub use ffi::Endpoint;

#[cfg(any(test, feature = "simulation"))]
pub use ffi::simulation;

/// Clean up and sort a list of RDMA ports using sysfs.
pub mod discovery {
    use crate::PortInfo;

    use std::path::Path;

    /// True if `device` is a safe sysfs name: 1 to 63 bytes, no path tricks.
    pub fn valid_device(device: &str) -> bool {
        !device.is_empty()
            && device.len() <= 63
            && !device.contains(['/', '\0', '\r', '\n'])
            && device != "."
            && device != ".."
    }

    /// Filter, sort, and dedup ports, reading PCI and NUMA info under `root`.
    ///
    /// - Drops bad names, port 0, and all-zero GIDs.
    /// - Sets `numa_node` from sysfs, or `None` if unreadable.
    /// - Sorts by PCI address (known first), then port, then name.
    /// - Keeps the first entry for each (device, port).
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

/// List RDMA ports on this host. Opens and closes devices on this thread.
///
/// Returns [`Error::Unavailable`] with no RDMA backend. Use
/// [`discovery::inventory_at`] to fill in NUMA info.
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

/// Any value you want kept alive as long as a slot's RDMA resources live.
///
/// Use it to count admission. If a resource is leaked, its guard leaks too.
pub type Guard = Arc<dyn Send + Sync>;

/// Result type for this crate.
pub type Result<T> = std::result::Result<T, Error>;

/// Errors from this crate.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// Bad pool or selector setup.
    InvalidConfiguration,

    /// Bad arguments, or the call is not allowed right now.
    InvalidRequest,

    /// Bad length or memory range.
    InvalidRange,

    /// No RDMA, pool closed, or slot canceled.
    Unavailable,

    /// No free slot, region, or command space.
    Overloaded,

    /// The deadline passed.
    DeadlineExceeded,

    /// Canceled before it finished.
    Cancelled,

    /// An RDMA call failed, or a lock was poisoned.
    Io,
}

impl std::fmt::Display for Error {
    /// Print the variant name.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}

/// One RDMA port, as shown to the selector.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PortInfo {
    /// Device name, like `mlx5_0`.
    pub device: String,

    /// Port number on the device.
    pub port: u8,

    /// Port GID at index 0.
    pub gid: [u8; 16],

    /// NUMA node, if known.
    pub numa_node: Option<usize>,
}

/// A port the selector picked, with your tag for it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Selection {
    /// Your name for this port. Pass it to [`IoPort::device`].
    pub tag: u32,

    /// Position in the discovered port list.
    pub index: usize,

    /// The picked port.
    pub port: PortInfo,
}

/// What [`IoPort::configure`] asks the native side to build.
pub struct Configuration {
    /// Set false to skip discovery for an empty plan. The selector then sees
    /// no ports and must pick none.
    pub discover: bool,

    /// Runs on the native thread. Gets the port list, returns `(tag, index)`
    /// pairs. Tags and indices must be unique.
    pub selector: Box<PortSelector>,

    /// One guard per slot. The length sets the slot count.
    pub guards: Vec<Guard>,

    /// Buffer size per slot.
    pub bytes: usize,
}

/// The I/O thread's end of the pair. Created by [`pair`].
pub struct IoPort {
    pub(crate) shared: Arc<Shared>,
}

/// The native thread's end of the pair. Move it there and pass it to
/// [`NativeService::new`].
pub struct NativePort {
    shared: Arc<Shared>,
}

/// Runs on the native thread and owns all RDMA resources.
///
/// Poll it from that thread's runtime. It is not `Send`.
pub struct NativeService {
    environment: uring_runtime::environment::Environment,

    port: NativePort,

    resources: Vec<Option<Resource>>,

    /// Selected devices keep their charges even if activation fails before slot creation.
    device_quotas: Vec<Arc<GuardOwner>>,

    activation: Option<Activation>,

    /// Completed activation retained until its mailbox can be locked.
    activation_result: Option<Result<Vec<Selection>>>,

    cursor: usize,

    #[cfg(any(test, feature = "simulation"))]
    simulation: Option<simulation::Simulation>,
}

/// A `uring-runtime` service with a [`NativeService`] attached.
///
/// - Poll: polls both with the same budget.
/// - Stop admission: only the inner service. Native work keeps going.
/// - Drain and fence: close and fully drain the native side first, even if
///   the scope has expired. Then run the inner one.
/// - Close: closes both, native first.
pub struct WithNative<T> {
    inner: T,

    native: NativeService,
}

/// A port tag on the I/O thread. Lease QPs from it.
///
/// Goes stale after [`IoPort::reopen`].
pub struct DeviceHandle {
    pub(crate) port: Rc<IoPort>,

    pub(crate) rail: u32,

    pub(crate) generation: u64,
}

/// A leased slot's QP, seen from the I/O thread.
///
/// Each call queues a command for the native thread. Dropping it stops the QP.
pub struct QueuePairHandle {
    lease: Rc<Lease>,

    /// This QP's address. Send it to the peer.
    pub endpoint: Endpoint,

    connecting: RefCell<Option<Ticket>>,

    connected: Cell<bool>,

    pending: RefCell<Option<Ticket>>,

    failure: Cell<Option<Error>>,

    expires: Cell<Option<std::time::Instant>>,
}

/// The slot's buffer, seen from the I/O thread.
///
/// CPU writes fill staging memory only. The native thread copies staging into
/// registered memory when executing a write, not when binding a receive window.
/// Read received bytes only after the QP has stopped and readback is published.
pub struct Region {
    lease: Rc<Lease>,

    length: usize,
}

/// Lets a peer write into a [`Region`]. Send its address and key to the peer.
///
/// Address and key are set once the bind [`Ticket`] succeeds. Do not send them
/// before that.
pub struct Window {
    pub(crate) key: Cell<u32>,

    pub(crate) address: Cell<u64>,

    _region: Rc<Region>,
}

/// The result of one command, once the native side has finished it.
#[derive(Clone)]
pub struct Ticket(Rc<TicketState>);

impl Endpoint {
    /// Size of the wire encoding.
    pub const ENCODED_LEN: usize = 32;

    /// Encode as 32 big-endian bytes. Does not validate.
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

    /// Decode 32 bytes and validate the fields.
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
    drivers::poll_scoped,
    group::{FailureReporter, Service},
};

/// The I/O side's hold on a slot. Shared by the QP handle, region, and tickets.
struct Lease {
    slot: Arc<Slot>,

    shared: Arc<Shared>,
}

impl Drop for Lease {
    /// Last holder gone: tell the native side to stop and recycle the slot.
    fn drop(&mut self) {
        self.slot.cancel.store(true, Ordering::Release);
        self.slot.released.store(true, Ordering::Release);
        self.shared.engine.wake();
    }
}

impl Region {
    /// Take the slot's buffer for `length` bytes. One region per slot.
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

    /// Bytes in use, not the full buffer size.
    pub fn length(&self) -> usize {
        self.length
    }

    /// Fill the buffer. `bytes` must be exactly [`Region::length`] long.
    /// Fails while a command is queued.
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

    /// Wake `cx` when the native side makes progress on this slot.
    pub fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }

    /// Read the buffer. Fails until the QP has stopped.
    pub fn poll_copy_to(&self, cx: &mut Context<'_>) -> Poll<Result<Vec<u8>>> {
        self.lease.slot.waiter.register(cx.waker());
        if !self.lease.slot.fenced.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        let mailbox = ready!(try_mailbox(&self.lease.slot.mailbox))?;
        Poll::Ready(self.copy_bytes(&mailbox))
    }

    /// Copy out `length` bytes.
    fn copy_bytes(&self, mailbox: &Mailbox) -> Result<Vec<u8>> {
        let mut bytes = Vec::new();
        bytes
            .try_reserve_exact(self.length)
            .map_err(|_| Error::Overloaded)?;
        bytes.extend_from_slice(&mailbox.bytes[..self.length]);
        Ok(bytes)
    }
}

/// A ticket's slot hold, its saved result, and the window it binds, if any.
struct TicketState {
    lease: Rc<Lease>,

    result: Cell<Option<Result<()>>>,

    window: Option<Rc<Window>>,
}

impl Ticket {
    /// The result, or `None` if not done yet.
    pub fn result(&self) -> Option<Result<()>> {
        match self.poll_result() {
            Poll::Ready(result) => result,
            Poll::Pending => None,
        }
    }

    /// Take the result from the mailbox and save it. On a good bind, fill in
    /// the window's address and key.
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

    /// Wait for the result. Fails if the pool closes first.
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
    /// Lease a free slot on `device`'s port.
    ///
    /// `permit` is held until the QP has stopped. Returns
    /// [`Error::Overloaded`] if no slot is free.
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

    /// Queue a command. The previous one must have finished.
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

    /// Start connecting to `remote`. Wait with [`Self::poll_connected`].
    pub fn poll_connect(&self, remote: Endpoint) -> Poll<Result<()>> {
        remote.validate()?;
        if self.connecting.borrow().is_some() || remote.link_layer != self.endpoint.link_layer {
            return Poll::Ready(Err(Error::InvalidRequest));
        }
        *self.connecting.borrow_mut() =
            Some(ready!(self.poll_submit(Command::Connect(remote), None))?);
        Poll::Ready(Ok(()))
    }

    /// True if connected and not failed, stopping, or closed.
    pub fn ready(&self) -> bool {
        self.connected.get()
            && self.failure.get().is_none()
            && !self.lease.shared.closed.load(Ordering::Acquire)
            && !self.lease.slot.cancel.load(Ordering::Acquire)
    }

    /// True once the QP has fully stopped.
    pub fn stopped(&self) -> bool {
        self.lease.slot.fenced.load(Ordering::Acquire)
    }

    /// Set a deadline. [`Self::progress`] fails and stops the QP after it.
    pub fn expire_at(&self, deadline: std::time::Instant) {
        self.expires.set(Some(deadline));
    }

    /// Open `region` to remote writes. Send the window to the peer only after
    /// the ticket succeeds.
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

    /// Revoke the window. This does not make the region safe to read; stop the
    /// QP for that.
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

    /// Write `region` to the peer's window at `address` with `key`.
    pub fn poll_write(&self, region: Rc<Region>, address: u64, key: u32) -> Poll<Result<Ticket>> {
        if !self.ready() || !Rc::ptr_eq(&region.lease, &self.lease) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        self.poll_submit(Command::Write { address, key }, None)
    }

    /// Wake `cx` when the native side makes progress on this slot.
    pub fn register_waiter(&self, cx: &Context<'_>) {
        self.lease.slot.waiter.register(cx.waker());
    }

    /// Check state. Returns 1 if the last command finished, else 0.
    ///
    /// On failure, close, or deadline, stops the QP and returns the error.
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

    /// Ask the native side to stop the QP. Returns at once; wait with
    /// [`Self::poll_stopped`].
    pub fn stop(&self) {
        self.lease.slot.cancel.store(true, Ordering::Release);
        self.lease.shared.engine.wake();
    }

    /// Stop the QP and wait until it has stopped. Fails if the pool closes
    /// first.
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

    /// Wait for [`Self::poll_connect`] to finish.
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
    /// Ask the native side to stop the QP. Does not wait.
    fn drop(&mut self) {
        self.stop();
    }
}

// Slot states.

/// Not built yet.
pub(crate) const IDLE: u8 = 0;

/// Built and free to lease.
pub(crate) const READY: u8 = 1;

/// Leased by the I/O thread.
pub(crate) const OWNED: u8 = 2;

/// Out of use, waiting to be released or rebuilt.
pub(crate) const RETIRED: u8 = 3;

/// A request from the I/O thread for the native thread to run.
pub(crate) enum Command {
    /// Connect the QP to the peer.
    Connect(Endpoint),

    /// Open the region to remote writes.
    Bind,

    /// Write the region to the peer's window.
    Write { address: u64, key: u32 },

    /// Revoke the window.
    Invalidate,
}

/// Data passed between the two threads for one slot, behind a mutex.
pub(crate) struct Mailbox {
    /// Lease permit, held until the QP stops.
    pub peer_admission: Option<Guard>,

    /// This slot's QP address.
    pub endpoint: Option<Endpoint>,

    /// Tag of the port this slot is on.
    pub rail: u32,

    /// Region length in use, or 0 if no region.
    pub length: usize,

    /// Copy of the region, readable by the I/O thread.
    pub bytes: Vec<u8>,

    /// Command waiting for the native side.
    pub command: Option<Command>,

    /// Result of the last command.
    pub result: Option<Result<()>>,

    /// Window address and key after a bind.
    pub descriptor: Option<(u64, u32)>,

    /// The slot's guard from [`Configuration::guards`].
    pub quota: Option<Arc<GuardOwner>>,
}

/// One pool slot, shared by both threads.
pub(crate) struct Slot {
    /// IDLE, READY, OWNED, or RETIRED.
    pub state: AtomicU8,

    /// I/O side wants the QP stopped.
    pub cancel: AtomicBool,

    /// I/O side has dropped its lease.
    pub released: AtomicBool,

    /// Native side has stopped the QP. Safe to read the buffer.
    pub fenced: AtomicBool,

    /// Native teardown stopped DMA, even if readback publication was contended.
    stopped: Arc<AtomicBool>,

    pub mailbox: Mutex<Mailbox>,

    /// Wakes the I/O task waiting on this slot.
    pub waiter: AtomicWaker,
}

impl Drop for Slot {
    /// If the QP never stopped, leak the lease permit along with it.
    fn drop(&mut self) {
        // The NIC may still own this slot's memory, so its permit must stay
        // counted.
        if !self.fenced.load(Ordering::Acquire) && !self.stopped.load(Ordering::Acquire) {
            let mailbox = self.mailbox.get_mut().unwrap_or_else(|e| e.into_inner());
            if let Some(permit) = mailbox.peer_admission.take() {
                std::mem::forget(permit);
            }
        }
    }
}

/// State shared by [`IoPort`] and [`NativePort`].
pub(crate) struct Shared {
    pub slots: Vec<Arc<Slot>>,

    /// Wakes the native service.
    pub engine: AtomicWaker,

    /// Wakes the I/O driver.
    pub io: AtomicWaker,

    /// No new work. Native side is shutting slots down.
    pub closed: AtomicBool,

    /// Bumped by each reopen. Stale [`DeviceHandle`]s are rejected.
    pub generation: AtomicU64,

    /// Closed and every slot freed. Reopen is allowed.
    drained: AtomicBool,

    /// False once the [`NativePort`] is dropped.
    alive: AtomicBool,

    /// A configuration was submitted this round.
    configured: AtomicBool,

    /// Configuration waiting for the native side.
    config: Mutex<Option<Configuration>>,

    /// Activation result waiting for the I/O side.
    activation: Mutex<Option<Result<Vec<Selection>>>>,
}

/// Picks ports: takes the port list, returns `(tag, index)` pairs.
type PortSelector = dyn FnOnce(&[PortInfo]) -> Result<Vec<(u32, usize)>> + Send;

impl Drop for NativePort {
    /// Native side is gone: close the pool and wake every waiter.
    fn drop(&mut self) {
        self.shared.alive.store(false, Ordering::Release);
        self.shared.closed.store(true, Ordering::Release);
        for slot in &self.shared.slots {
            slot.waiter.wake();
        }
        self.shared.io.wake();
    }
}

/// Create the two ends of a pool with `slots` slots (1 to 256).
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
                    stopped: Arc::new(AtomicBool::new(false)),
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
    /// Submit `configuration` and wait for the native side to build the pool.
    ///
    /// Returns the picked ports. If the wait is dropped or the scope ends
    /// first, the pool is closed.
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

        /// Closes the pool if dropped before activation finishes.
        struct ActivationGuard<'a> {
            port: &'a IoPort,

            completed: bool,
        }

        impl Drop for ActivationGuard<'_> {
            /// Close unless activation finished.
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

    /// Get a handle for the port you tagged `tag` in the selector.
    pub fn device(self: &Rc<Self>, tag: u32) -> Rc<DeviceHandle> {
        Rc::new(DeviceHandle {
            port: self.clone(),
            rail: tag,
            generation: self.shared.generation.load(Ordering::Acquire),
        })
    }

    /// True if the pool is closed.
    pub fn closed(&self) -> bool {
        self.shared.closed.load(Ordering::Acquire)
    }

    /// Number of slots.
    pub fn capacity(&self) -> usize {
        self.shared.slots.len()
    }

    /// Wake `waker` on activation results and slot changes.
    pub fn register_driver(&self, waker: &Waker) {
        self.shared.io.register(waker);
    }

    /// Start a new round after [`Self::close`].
    ///
    /// Fails until the native side has drained. Does nothing if not closed.
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
            slot.stopped.store(false, Ordering::Release);
            slot.state.store(IDLE, Ordering::Release);
        }
        self.shared.generation.store(generation, Ordering::Release);
        self.shared.configured.store(false, Ordering::Release);
        // Open before clearing the acknowledgment. Native progress that acquires
        // the cleared drained flag must also see this pool as open.
        self.shared.closed.store(false, Ordering::Release);
        #[cfg(test)]
        tests::poll_during_reopen();
        self.shared.drained.store(false, Ordering::Release);
        self.shared.engine.wake();
        Ok(())
    }

    /// Hand `configuration` to the native side. Once per round.
    ///
    /// Does not wait for the pool to be built; use [`Self::activation`] for
    /// that. To give up after this, call [`Self::close`].
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

    /// Take the activation result, or `None` if not ready yet.
    pub fn activation(&self) -> Option<Result<Vec<Selection>>> {
        match self.shared.activation.try_lock() {
            Ok(mut activation) => activation.take(),
            Err(std::sync::TryLockError::WouldBlock) => None,
            Err(std::sync::TryLockError::Poisoned(_)) => Some(Err(Error::Io)),
        }
    }

    /// Close the pool and cancel every slot. Does not wait.
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
    /// Close the pool.
    fn drop(&mut self) {
        self.close();
    }
}

/// Native RDMA objects for one slot.
struct Resource {
    device: Rc<ffi::NativeDevice>,

    region: Rc<ffi::NativeRegion>,

    qp: Option<Rc<ffi::NativeQueuePair>>,

    window: Option<Rc<ffi::Window>>,

    /// The command in flight, if any.
    pending: Option<ffi::Ticket>,

    /// Shutting this slot's QP down.
    stopping: bool,

    /// Wait until this time before retrying a failed stop or rebuild.
    next_retry: Option<std::time::Instant>,
}

/// Activation in progress. Slots are built one per step.
struct Activation {
    devices: Vec<Rc<ffi::NativeDevice>>,

    selected: Vec<Selection>,

    quotas: std::vec::IntoIter<Guard>,

    bytes: usize,

    /// Next slot to build.
    next: usize,
}

/// Outcome of one activation step.
enum ActivationStep {
    /// More to do.
    Pending,

    /// All slots built. Send this to the I/O side.
    Complete(Vec<Selection>),
}

impl NativeService {
    /// Build the service. Call on the native thread; it captures that thread's
    /// runtime (and simulation, in tests).
    pub fn new(port: NativePort) -> Self {
        let resources = (0..port.shared.slots.len()).map(|_| None).collect();
        Self {
            environment: uring_runtime::environment::Environment::current(),
            port,
            resources,
            device_quotas: Vec::new(),
            activation: None,
            activation_result: None,
            cursor: 0,
            #[cfg(any(test, feature = "simulation"))]
            simulation: simulation::current(),
        }
    }

    /// Wake `waker` when the I/O side has work for the native side.
    pub fn register_driver(&self, waker: &Waker) {
        self.port.shared.engine.register(waker);
    }

    /// Find ports and run the selector. Slots are built later, one per step.
    fn begin_activation(&mut self, config: Configuration) -> Result<ActivationStep> {
        #[cfg(any(test, feature = "simulation"))]
        let _environment = self.simulation.as_ref().map(simulation::Simulation::enter);
        if self.port.shared.closed.load(Ordering::Acquire) {
            return Err(Error::Unavailable);
        }
        if config.bytes == 0 || config.bytes > u32::MAX as usize {
            return Err(Error::InvalidRange);
        }
        let mut discovered = if config.discover {
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
        for (selection, guard) in selected.iter().zip(&config.guards) {
            let quota = GuardOwner::new(guard.clone());
            discovered[selection.index].quota = Some(quota.clone());
            self.device_quotas.push(quota);
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

    /// Build the next slot: buffer, QP, and a test window. Slots are spread
    /// across the picked ports in turn. None go READY until all are built.
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
        // Store the guard first, so it is tracked even if a step below fails.
        // A failed activation is cleaned up by the normal close path.
        mailbox.quota = Some(quota.clone());
        mailbox
            .bytes
            .try_reserve_exact(activation.bytes)
            .map_err(|_| Error::Overloaded)?;
        mailbox.bytes.resize(activation.bytes, 0);
        let region = ffi::NativeRegion::new(device.clone(), activation.bytes, quota.clone())?;
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
        resource.qp = Some(ffi::NativeQueuePair::new_charged(
            device,
            quota,
            slot.stopped.clone(),
        )?);
        let qp = resource.qp.as_ref().unwrap();
        qp.probe_window()?;
        mailbox.rail = selected.tag;
        mailbox.endpoint = Some(qp.endpoint);
        activation.next += 1;
        if activation.next != self.resources.len() {
            return Ok(ActivationStep::Pending);
        }
        // All slots built. Make them leasable at once.
        for slot in &self.port.shared.slots {
            slot.state.store(READY, Ordering::Release);
        }
        Ok(ActivationStep::Complete(activation.selected.to_vec()))
    }

    /// Do up to `budget` steps of work, then check whether shutdown is done.
    ///
    /// A step is one of: find ports, build one slot, or service one slot
    /// (round-robin). Budget counts steps, not time; one slow RDMA call
    /// blocks this thread.
    pub fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
        let _environment = self.environment.enter();
        if budget == 0 {
            return Ok(());
        }
        for _ in 0..budget.min(self.resources.len()) {
            if self.activation_result.is_some() {
                self.publish_activation()?;
                if self.activation_result.is_some() {
                    // Keep driving teardown even while result publication is busy.
                    let index = self.cursor;
                    self.cursor = (self.cursor + 1) % self.resources.len();
                    self.drive(index);
                    continue;
                }
            }
            let result = if self.activation.is_some() {
                Some(self.activate_slot())
            } else {
                let config = match try_mailbox(&self.port.shared.config) {
                    Poll::Ready(result) => result?.take(),
                    Poll::Pending => None,
                };
                config.map(|config| self.begin_activation(config))
            };
            if let Some(result) = result {
                let completed = match result {
                    Ok(ActivationStep::Pending) => continue,
                    Ok(ActivationStep::Complete(selected)) => Ok(selected),
                    Err(error) => Err(error),
                };
                self.activation = None;
                self.activation_result = Some(completed);
                self.publish_activation()?;
                continue;
            }
            let index = self.cursor;
            self.cursor = (self.cursor + 1) % self.resources.len();
            self.drive(index);
        }
        // Drained once closed and all native objects are gone. Then clear
        // the mailboxes so reopen starts fresh.
        // Read drained first: acquiring reopen's reset also publishes closed=false.
        if !self.port.shared.drained.load(Ordering::Acquire)
            && self.port.shared.closed.load(Ordering::Acquire)
            && self.activation.is_none()
            && self.activation_result.is_none()
            && self
                .port
                .shared
                .config
                .try_lock()
                .is_ok_and(|c| c.is_none())
            && self.resources.iter().all(Option::is_none)
        {
            self.device_quotas.retain(|q| q.quarantined());
            let mut drained = self.device_quotas.is_empty();
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
                // Someone else still holds the guard: a native object was leaked.
                if mailbox
                    .quota
                    .as_ref()
                    .is_some_and(|q| q.quarantined() || Arc::strong_count(q) != 1)
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

    /// Publish exactly once, retaining the result while the I/O side holds its lock.
    fn publish_activation(&mut self) -> Result<()> {
        match try_mailbox(&self.port.shared.activation) {
            Poll::Ready(result) => {
                *result? = self.activation_result.take();
                self.port.shared.io.wake();
            }
            Poll::Pending => {}
        }
        Ok(())
    }

    /// Service one slot.
    ///
    /// - Stopping: stop the QP, copy the buffer out, mark it fenced. Then free
    ///   the slot (if closed) or build a fresh QP and make it READY again.
    /// - Leased: check the command in flight, or start the next one.
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
                return; // Keep everything; retry later.
            }
            // Peers may have written into the region; copy it out for reads.
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
            resource.qp = None; // Destroyed here, on the native thread.
            slot.fenced.store(true, Ordering::Release);
            mailbox.peer_admission = None;
            mailbox.command = None;
            if mailbox.result.is_none() {
                mailbox.result = Some(Err(Error::Cancelled));
            }
            slot.waiter.wake();
            self.port.shared.io.wake();
            if closed {
                // Free the native objects. The mailbox copy stays for any
                // I/O holder that still wants to read it.
                self.resources[index] = None;
                if state != OWNED || slot.released.load(Ordering::Acquire) {
                    slot.released.store(true, Ordering::Release);
                }
                slot.state.store(RETIRED, Ordering::Release);
                return;
            }
            if !slot.released.load(Ordering::Acquire) {
                return;
            }
            let quota = mailbox.quota.as_ref().unwrap().clone();
            if quota.quarantined() {
                slot.state.store(RETIRED, Ordering::Release);
                return;
            }
            if resource.region.clear().is_err() {
                return;
            }
            mailbox.bytes.fill(0);
            // Lease returned: build a fresh QP. Never reuse a QP. The region
            // stays registered for the life of the pool.
            match ffi::NativeQueuePair::new_charged(
                resource.device.clone(),
                quota,
                slot.stopped.clone(),
            ) {
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
            slot.stopped.store(false, Ordering::Release);
            slot.state.store(READY, Ordering::Release);
            self.port.shared.io.wake();
            return;
        }
        if state != OWNED {
            return;
        }
        let qp = resource.qp.as_ref().unwrap();
        // A command is in flight: check for its completion.
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
        // Start the next command. Connect finishes at once; the rest post
        // work to the NIC and finish later.
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

    /// Close the pool from the native side.
    pub fn close(&self) {
        self.port.shared.closed.store(true, Ordering::Release);
        self.port.shared.io.wake();
    }

    /// True if no activation is pending and service-owned teardown is complete.
    /// Failed native frees may still leave quarantined, charged resources.
    /// This is not permission to reopen: [`IoPort::reopen`] separately checks
    /// whether I/O holders and quarantined resources have released their charges.
    pub fn drained(&self) -> bool {
        self.activation.is_none()
            && self.activation_result.is_none()
            && self
                .port
                .shared
                .config
                .try_lock()
                .is_ok_and(|config| config.is_none())
            && self.resources.iter().all(Option::is_none)
    }
}

impl Drop for NativeService {
    /// Close, then drop QPs before the regions they use.
    fn drop(&mut self) {
        self.close();
        // If a QP fails to stop, it leaks its region and guard on purpose.
        for (slot, resource) in self.port.shared.slots.iter().zip(&mut self.resources) {
            if let Some(resource) = resource {
                let stopped = resource.qp.as_ref().is_none_or(|qp| qp.stop().is_ok());
                if stopped {
                    // Publish the same safe readback as normal shutdown. If the
                    // mailbox is busy, final Slot drop can release its permit
                    // using `stopped`, without claiming readback was published.
                    if let Ok(mut mailbox) = slot.mailbox.try_lock() {
                        let copied = resource.window.is_none()
                            || resource
                                .region
                                .copy_into(&mut mailbox.bytes[..resource.region.length()])
                                .is_ok();
                        if copied {
                            slot.fenced.store(true, Ordering::Release);
                            mailbox.peer_admission = None;
                        }
                    }
                    slot.stopped.store(true, Ordering::Release);
                }
                resource.qp.take();
                resource.window.take();
                resource.pending.take();
            }
        }
        for slot in &self.port.shared.slots {
            slot.waiter.wake();
        }
    }
}

impl Window {
    /// Remote key. Set once the bind ticket succeeds.
    pub fn key(&self) -> u32 {
        self.key.get()
    }

    /// Remote address. Set once the bind ticket succeeds.
    pub fn address(&self) -> u64 {
        self.address.get()
    }
}

impl<T> WithNative<T> {
    /// Wrap `inner`. Call on the native thread.
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
    /// Pass through to the inner service.
    fn set_failure_reporter(&mut self, reporter: FailureReporter<S::Error>) {
        self.inner.set_failure_reporter(reporter);
    }

    /// Pass through to the inner service.
    fn waker(&self) -> std::result::Result<Waker, S::Error> {
        self.inner.waker()
    }

    /// Register `waker` with both.
    fn register_driver(&self, waker: &Waker) {
        self.inner.register_driver(waker);
        self.native.register_driver(waker);
    }

    /// Start the inner service. The native side starts via [`IoPort::activate`].
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.start(scope)
    }

    /// Poll the inner service, then the native side, each with `budget`.
    fn poll_budgeted(
        &mut self,
        cx: &mut Context<'_>,
        budget: usize,
    ) -> std::result::Result<(), S::Error> {
        self.inner.poll_budgeted(cx, budget)?;
        self.native.poll_budgeted(budget).map_err(Into::into)
    }

    /// Pass through to the inner service.
    fn wait_timeout(&self, maximum: std::time::Duration) -> std::time::Duration {
        self.inner.wait_timeout(maximum)
    }

    /// Stop the inner service only. Native work keeps going.
    fn stop_admission(&mut self) -> std::result::Result<(), S::Error> {
        self.inner.stop_admission()
    }

    /// Drain the native side fully, then the inner service.
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.native.close_and_fence().await?;
            self.inner.drain(scope).await
        })
    }

    /// Close the native side, then the inner service.
    fn close(&mut self) -> std::result::Result<(), S::Error> {
        self.native.close();
        self.inner.close()
    }

    /// Drain the native side fully, then fence the inner service.
    fn fence<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            self.native.close_and_fence().await?;
            self.inner.fence(scope).await
        })
    }

    /// Pass through to the inner service.
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error> {
        self.inner.shutdown(scope)
    }
}

impl NativeService {
    /// Close and poll until drained. Ignores scope deadlines: the NIC must be
    /// stopped before memory is freed.
    async fn close_and_fence(&mut self) -> Result<()> {
        self.close();
        std::future::poll_fn(|cx| {
            self.register_driver(cx.waker());
            self.poll_budgeted(1)?;
            if self.drained() {
                Poll::Ready(Ok(()))
            } else {
                // Relies on the runtime's periodic ticks to retry.
                Poll::Pending
            }
        })
        .await
    }
}

/// Our own `Arc` around a caller's [`Guard`].
///
/// Its strong count shows whether anything leaked, no matter how many clones
/// the caller keeps.
struct GuardOwner {
    _guard: Guard,

    /// A native allocation escaped destruction; this slot must never replenish it.
    quarantined: AtomicBool,
}

impl GuardOwner {
    /// Wrap `guard`.
    fn new(guard: Guard) -> Arc<Self> {
        Arc::new(Self {
            _guard: guard,
            quarantined: AtomicBool::new(false),
        })
    }

    /// Permanently retire the charged slot after a failed native free.
    fn quarantine(&self) {
        self.quarantined.store(true, Ordering::Release);
    }

    /// Whether a native allocation has been leaked against this charge.
    fn quarantined(&self) -> bool {
        self.quarantined.load(Ordering::Acquire)
    }
}

/// Try to lock a mailbox without blocking.
///
/// Busy returns `Pending` with no wake registered, so callers must also be
/// polled on a timer. Poisoned returns [`Error::Io`].
fn try_mailbox<T>(mailbox: &Mutex<T>) -> Poll<Result<MutexGuard<'_, T>>> {
    match mailbox.try_lock() {
        Ok(guard) => Poll::Ready(Ok(guard)),
        Err(TryLockError::WouldBlock) => Poll::Pending,
        Err(TryLockError::Poisoned(_)) => Poll::Ready(Err(Error::Io)),
    }
}

/// Test hooks: read slot state and hold locks on purpose.
///
/// Nothing here exposes a mailbox, native object, or lock guard.
#[cfg(feature = "simulation")]
pub mod testing {
    use super::*;

    /// A slot's state.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum State {
        /// Not built yet.
        Idle,

        /// Built and free to lease.
        Ready,

        /// Leased.
        Owned,

        /// Out of use, waiting to be released or rebuilt.
        Retired,
    }

    /// A copy of one slot's flags.
    #[derive(Clone, Copy, Debug)]
    pub struct Snapshot {
        /// Slot state.
        pub state: State,

        /// Stop was requested.
        pub cancelled: bool,

        /// The QP has stopped.
        pub fenced: bool,
    }

    /// Which lock [`IoPort::with_contention`] holds.
    pub enum Contention {
        /// One slot's mailbox.
        Slot(usize),

        /// The configuration mailbox.
        Configuration,

        /// The activation result mailbox.
        Activation,
    }

    impl IoPort {
        /// Run `f` while holding the chosen lock, to test the busy path.
        /// `f` must not block on that same lock.
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

        /// Read slot `i`'s flags.
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

        /// True if a configuration was submitted this round.
        pub fn configuration_submitted(&self) -> bool {
            self.shared.configured.load(Ordering::Acquire)
        }

        /// True if the pool is drained and can be reopened.
        pub fn pool_drained(&self) -> bool {
            self.shared.drained.load(Ordering::Acquire)
        }

        /// Current round number. Bumped by reopen.
        pub fn generation(&self) -> u64 {
            self.shared.generation.load(Ordering::Acquire)
        }

        /// True if slot `i` has a command waiting for the native side.
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
        /// Number of slots that still have native objects.
        pub fn resource_count(&self) -> usize {
            self.resources.iter().flatten().count()
        }

        /// True if slot `i` still has native objects.
        pub fn resource_present(&self, i: usize) -> bool {
            self.resources[i].is_some()
        }

        /// Skip slot `i`'s retry wait.
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

    thread_local! {
        static REOPEN_STEP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    }

    /// Let the native thread poll between reopen's two publication stores.
    pub(super) fn poll_during_reopen() {
        REOPEN_STEP.with(|step| {
            if let Some(step) = step.borrow_mut().take() {
                step();
            }
        });
    }

    /// An old drain acknowledgment cannot authorize resetting a new live lease.
    #[test]
    fn reopen_interleaved_native_poll_requires_a_fresh_drain() {
        let (io, port) = pair(1).unwrap();
        let io = Rc::new(io);
        type Step = Box<dyn FnOnce(&mut NativeService) + Send>;
        let (step_tx, step_rx) = std::sync::mpsc::channel::<Step>();
        let (done_tx, done_rx) = std::sync::mpsc::channel();
        let thread = std::thread::spawn(move || {
            let sim = simulation::Simulation::new()
                .with_devices(vec![simulation::Device::new("sim0", [1; 16])])
                .unwrap();
            let _environment = sim.enter();
            let mut native = NativeService::new(port);
            while let Ok(step) = step_rx.recv_timeout(std::time::Duration::from_secs(5)) {
                step(&mut native);
                done_tx.send(()).unwrap();
            }
            assert!(native.drained());
            assert_eq!(sim.live_resources(), 0);
        });
        let step = Rc::new(move |f: Step| {
            step_tx.send(f).unwrap();
            done_rx
                .recv_timeout(std::time::Duration::from_secs(5))
                .unwrap();
        });
        io.close();
        step(Box::new(|native| native.poll_budgeted(1).unwrap()));
        assert!(io.shared.drained.load(Ordering::Acquire));
        let interleave = step.clone();
        REOPEN_STEP.with(|hook| {
            *hook.borrow_mut() = Some(Box::new(move || {
                interleave(Box::new(|native| native.poll_budgeted(1).unwrap()));
            }));
        });
        io.reopen().unwrap();
        assert!(!io.closed());
        assert!(!io.shared.drained.load(Ordering::Acquire));
        // Also poll after the reset, when the native side must see the pool open.
        step(Box::new(|native| native.poll_budgeted(1).unwrap()));
        assert!(!io.shared.drained.load(Ordering::Acquire));

        let (guard, charged) = test_guard::guard();
        futures::executor::block_on(io.configure(Configuration {
            discover: true,
            bytes: 32,
            guards: vec![guard],
            selector: Box::new(|_| Ok(vec![(0, 0)])),
        }))
        .unwrap();
        step(Box::new(|native| {
            native.poll_budgeted(1).unwrap();
            native.poll_budgeted(1).unwrap();
            assert!(native.resources[0].is_some());
        }));
        io.activation().unwrap().unwrap();
        let qp = claim(&io);
        step(Box::new(|native| native.close()));
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        assert_eq!(io.shared.generation.load(Ordering::Acquire), 2);
        assert_eq!(io.shared.slots[0].state.load(Ordering::Acquire), OWNED);
        assert!(!qp.stopped());
        assert_eq!(charged.get(), 1);

        step(Box::new(|native| native.poll_budgeted(1).unwrap()));
        assert!(qp.stopped());
        assert_eq!(io.reopen(), Err(Error::Overloaded));
        drop(qp);
        step(Box::new(|native| native.poll_budgeted(1).unwrap()));
        assert_eq!(charged.get(), 0);
        io.reopen().unwrap();
        assert_eq!(io.shared.generation.load(Ordering::Acquire), 3);
        drop(step);
        thread.join().unwrap();
    }

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
