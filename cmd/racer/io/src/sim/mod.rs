//! Deterministic simulated operating system: files with crash persistence,
//! stream and datagram sockets, pipes, and the reactor driver that executes
//! ring operations against them.
//!
//! A world is an ordinary value ([`SimOs`]) selected explicitly through
//! `Driver::Sim`; nothing is thread-local. Operations borrow memory pinned by
//! the reactor slot until both of their completions are emitted.
use crate::ring::{Descriptor, SocketAddress, op::Op};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet, VecDeque},
    ffi::CStr,
    io,
    path::{Path, PathBuf},
    rc::Rc,
};

#[doc(hidden)]
mod driver;
mod network;
#[cfg(test)]
mod tests;

use disk::check_direct;
pub use disk::{CrashDisk, DiskState};
pub(crate) use driver::SimDriver;

pub mod disk;

/// Labels new outbound streams with their owning node's listening endpoint.
/// Enter this scope when polling that node; established sockets retain the label.
pub struct EndpointEnvironment {
    sim: SimOs,

    previous: Option<SocketAddress>,
}

impl Drop for EndpointEnvironment {
    /// Restore socket labeling without changing existing endpoint labels.
    fn drop(&mut self) {
        self.sim.0.borrow_mut().endpoint = self.previous.take();
    }
}

/// Shared deterministic OS state retained by reactors and simulated descriptors.
#[derive(Clone, Debug)]
pub struct SimOs(Rc<RefCell<World>>);

/// One ordered resource, fault, or completion event in the bounded trace.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Event {
    pub sequence: u64,

    pub operation: String,

    pub resource: u64,

    pub result: i64,
}

/// Queued faults match the next submitted operation with this name (or `*`).
/// `Delay` counts driver submission turns; `Short` caps a single byte transfer.
/// Synchronous syscalls support Errno/Short; use Delay on reactor submissions.
#[derive(Clone, Debug)]
pub enum Fault {
    Errno(i32),
    Short(usize),
    Delay(usize),
    /// Execute normally, then retain the actual result for this many driver turns.
    /// Bytes are visible to the peer while production CQE owners remain pinned.
    HoldCompletion(usize),
}

/// Resource tables and fault scheduling shared by this simulated OS.
#[derive(Debug)]
struct World {
    reject_submissions: usize,

    cancel_first: bool,

    next: u64,

    resources: BTreeMap<u64, Resource>,

    paths: BTreeMap<PathBuf, Rc<RefCell<Node>>>,

    listeners: BTreeMap<SocketAddress, u64>,

    datagrams: BTreeMap<std::net::SocketAddr, u64>,

    faults: VecDeque<(String, Fault)>,

    trace: Vec<Event>,

    next_event: u64,

    /// Events that changed world state. A blocked attempt (`blocked:*`) is
    /// traced but changes nothing, so it must not make another reactor's
    /// waiting operations runnable again, or two reactors sharing a world
    /// would wake each other forever.
    changes: u64,

    stream_capacity: usize,

    max_chunk: usize,

    executing: bool,

    endpoint: Option<SocketAddress>,

    partitions: BTreeSet<(SocketAddress, SocketAddress)>,

    disk: disk::State,

    /// Scheduling choices (submission delays, execution order, cancel CQE
    /// order); `None` keeps FIFO order.
    choices: Option<Box<dyn crate::ctx::ChoiceSource>>,

    /// Virtual latency charged to each submitted operation.
    cost: Option<OpCost>,
}

/// One submitted operation as the cost model sees it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct OpDesc {
    /// The operation name, as used by [`SimOs::inject`] (`read`, `write`,
    /// `fsync`, `send`, `recv`, `open`, ...).
    pub kind: &'static str,

    /// Bytes the operation asks to transfer (0 for metadata operations).
    pub bytes: u64,
}

/// A shared cost model.
#[derive(Clone)]
struct OpCost(Rc<dyn Fn(&OpDesc) -> std::time::Duration>);

impl std::fmt::Debug for OpCost {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("OpCost")
    }
}

/// The kernel-side state of an open simulated descriptor.
#[derive(Debug)]
enum Resource {
    Socket {
        domain: i32,

        read_shutdown: bool,

        write_shutdown: bool,

        peer: Option<u64>,

        bytes: VecDeque<u8>,

        connected: bool,

        local: Option<SocketAddress>,

        remote: Option<SocketAddress>,
    },
    Datagram {
        address: std::net::SocketAddr,

        peer: Option<std::net::SocketAddr>,

        packets: VecDeque<(std::net::SocketAddr, Vec<u8>)>,
    },
    Listener {
        pending: VecDeque<u64>,
    },
    File {
        node: Rc<RefCell<Node>>,

        flags: i32,

        lock_owner: bool,

        opened_path: PathBuf,
    },
    Pipe {
        bytes: Rc<RefCell<VecDeque<u8>>>,

        write: bool,

        capacity: usize,
    },
}

/// A sparse inode whose allocated pages are shared with durable snapshots.
#[derive(Debug)]
struct Node {
    inode: u64,

    mode: u16,

    length: u64,

    pages: BTreeMap<u64, Rc<[u8; 4096]>>,

    locked: bool,

    symlink: Option<PathBuf>,
}

/// A non-owning view of one simulated resource; [`Descriptor`] owns closing.
#[derive(Clone, Debug)]
pub struct Handle {
    sim: SimOs,

    id: u64,
}

impl SimOs {
    /// Close a resource and its pending accepts, releasing its lock and
    /// address bindings.
    pub(crate) fn close(&self, raw: i32) {
        let id = raw as u64;
        let Ok(mut w) = self.0.try_borrow_mut() else {
            return;
        };
        match w.resources.remove(&id) {
            Some(Resource::File {
                node,
                lock_owner: true,
                ..
            }) => node.borrow_mut().locked = false,
            Some(Resource::Listener { pending }) => {
                for id in pending {
                    w.resources.remove(&id);
                    w.record("close", id, 0);
                }
            }
            _ => (),
        }
        w.listeners.retain(|_, listener| *listener != id);
        w.datagrams.retain(|_, datagram| *datagram != id);
        w.record("close", id, 0);
    }
}

/// Preserves a Linux error number when reporting a simulated syscall failure.
fn errno(n: i32) -> io::Error {
    io::Error::from_raw_os_error(n)
}

/// Memory ceiling for materialized fixtures and each sparse inode's allocated pages.
const MAX_ALLOCATION: usize = 64 * 1024 * 1024;

/// Allocates zeroed fixture bytes, reporting oversized requests and allocation failure.
fn bounded_bytes(length: usize) -> io::Result<Vec<u8>> {
    if length > MAX_ALLOCATION {
        return Err(errno(libc::EFBIG));
    }
    let mut bytes = Vec::new();
    bytes
        .try_reserve_exact(length)
        .map_err(|_| errno(libc::ENOMEM))?;
    bytes.resize(length, 0);
    Ok(bytes)
}

impl World {
    /// Checks a symmetric endpoint partition regardless of argument order.
    fn partitioned(&self, a: &SocketAddress, b: &SocketAddress) -> bool {
        self.partitions
            .contains(&endpoint_pair(a.clone(), b.clone()))
    }

    /// Checks a stream's endpoint labels; unlabeled streams are never partitioned.
    fn stream_partitioned(&self, id: u64) -> bool {
        matches!(self.resources.get(&id), Some(Resource::Socket { local: Some(a), remote: Some(b), .. }) if self.partitioned(a,b))
    }

    /// Reserves the next identifier in this world's deterministic resource sequence.
    fn id(&mut self) -> u64 {
        let id = self.next;
        self.next += 1;
        id
    }

    /// Appends a sequenced event, discarding the oldest half when the trace fills.
    fn record(&mut self, op: &str, resource: u64, result: i64) {
        if self.trace.len() == 16384 {
            self.trace.drain(..8192);
        }
        self.trace.push(Event {
            sequence: self.next_event,
            operation: op.into(),
            resource,
            result,
        });
        self.next_event += 1;
        if !op.starts_with("blocked:") {
            self.changes += 1;
        }
    }

    /// Consumes the first matching fault unless the driver already selected one.
    fn fault(&mut self, op: &str) -> Option<Fault> {
        if self.executing {
            return None;
        }
        let index = self
            .faults
            .iter()
            .position(|(name, _)| name == op || name == "*")?;
        let (_, fault) = self.faults.remove(index)?;
        self.record(
            &format!("fault:{op}"),
            0,
            match fault {
                Fault::Errno(n) => -(n as i64),
                Fault::Short(n) | Fault::Delay(n) | Fault::HoldCompletion(n) => n as i64,
            },
        );
        Some(fault)
    }

    /// Consume a synchronous transfer fault without applying driver-only delays.
    fn transfer_limit(&mut self, operation: &str) -> io::Result<usize> {
        match self.fault(operation) {
            Some(Fault::Errno(n)) => Err(errno(n)),
            Some(Fault::Short(n)) => Ok(n),
            _ => Ok(usize::MAX),
        }
    }
}

/// Orders two endpoints so both directions share one partition-table key.
fn endpoint_pair(a: SocketAddress, b: SocketAddress) -> (SocketAddress, SocketAddress) {
    if a <= b { (a, b) } else { (b, a) }
}

impl SimOs {
    /// Creates an empty world with a durable root directory.
    pub fn new() -> Self {
        let sim = Self(Rc::new(RefCell::new(World {
            reject_submissions: 0,
            cancel_first: true,
            next: 1,
            resources: BTreeMap::new(),
            paths: BTreeMap::new(),
            listeners: BTreeMap::new(),
            datagrams: BTreeMap::new(),
            faults: VecDeque::new(),
            trace: Vec::new(),
            next_event: 0,
            changes: 0,
            stream_capacity: 64 * 1024,
            max_chunk: usize::MAX,
            executing: false,
            endpoint: None,
            partitions: BTreeSet::new(),
            disk: disk::State::default(),
            choices: None,
            cost: None,
        })));
        sim.create_dir_all(Path::new("/")).unwrap();
        sim.disk().sync_all().unwrap();
        sim
    }

    /// Test support: a world whose scheduling choices (operation delays,
    /// execution order, cancel CQE order) come from `seed`. The same seed and
    /// the same workload always produce the same trace.
    pub fn seeded(seed: u64) -> Self {
        Self::with_choices(crate::ctx::Xoshiro::new(seed, 0x51_u64))
    }

    /// A world whose scheduling choices (operation delays, execution order,
    /// cancel CQE order) are drawn from `source`, for example a simulation
    /// harness's choice stream. A source that only yields `0` behaves like
    /// [`SimOs::new`]: no added delay, submission order, cancel CQE first.
    pub fn with_choices(source: impl crate::ctx::ChoiceSource + 'static) -> Self {
        let os = Self::new();
        os.0.borrow_mut().choices = Some(Box::new(source));
        os
    }

    /// Charge virtual time to every operation submitted from now on: an
    /// operation becomes eligible to execute `cost(desc)` after submission,
    /// on the submitting reactor's clock. Without a cost model (the default)
    /// operations cost nothing. Delays from faults and choices still count
    /// driver turns on top of this.
    pub fn set_op_cost(&self, cost: impl Fn(&OpDesc) -> std::time::Duration + 'static) {
        self.0.borrow_mut().cost = Some(OpCost(Rc::new(cost)));
    }

    /// The virtual time charged to `desc` (zero without a cost model).
    pub fn op_cost(&self, desc: &OpDesc) -> std::time::Duration {
        let cost = self.0.borrow().cost.clone();
        cost.map_or(std::time::Duration::ZERO, |cost| (cost.0)(desc))
    }

    /// Accesses this world's volatile and durable storage images.
    pub fn disk(&self) -> CrashDisk {
        CrashDisk(self.clone())
    }

    /// Labels sockets created in this scope with a node's listening endpoint.
    pub fn enter_endpoint(&self, endpoint: SocketAddress) -> EndpointEnvironment {
        let previous = self.0.borrow_mut().endpoint.replace(endpoint);
        EndpointEnvironment {
            sim: self.clone(),
            previous,
        }
    }

    /// Symmetric blackhole: existing streams and future connects stall until heal
    /// or their normal production deadlines/cancellation. Received bytes remain
    /// readable; a partition does not manufacture EOF or discard accepted bytes.
    pub fn partition(&self, a: SocketAddress, b: SocketAddress) {
        let mut w = self.0.borrow_mut();
        w.partitions.insert(endpoint_pair(a, b));
        w.record("partition", 0, 0);
    }

    /// Removes a symmetric partition without discarding queued bytes.
    pub fn heal(&self, a: SocketAddress, b: SocketAddress) {
        let mut w = self.0.borrow_mut();
        w.partitions.remove(&endpoint_pair(a, b));
        w.record("heal", 0, 0);
    }

    /// Assign labels to a preexisting socket pair or a socket created outside a
    /// node scope. Updates the opposite endpoint too, including pending accepts.
    pub fn label_stream(
        &self,
        fd: &Descriptor,
        local: SocketAddress,
        remote: SocketAddress,
    ) -> io::Result<()> {
        let h = self.handle(fd)?;
        let mut w = self.0.borrow_mut();
        let Some(Resource::Socket {
            peer,
            local: a,
            remote: b,
            ..
        }) = w.resources.get_mut(&h.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        *a = Some(local.clone());
        *b = Some(remote.clone());
        let peer = *peer;
        if let Some(Resource::Socket {
            local: a,
            remote: b,
            ..
        }) = peer.and_then(|id| w.resources.get_mut(&id))
        {
            *a = Some(remote);
            *b = Some(local);
        }
        Ok(())
    }

    /// Queues a fault for the next matching operation, or any operation for `*`.
    pub fn inject(&self, operation: &str, fault: Fault) -> io::Result<()> {
        if let Fault::Errno(errno) = &fault
            && (*errno <= 0 || *errno > 4095)
        {
            return Err(self::errno(libc::EINVAL));
        }
        if self.0.borrow().faults.len() >= 16384 {
            return Err(errno(libc::ENOSPC));
        }
        self.0
            .borrow_mut()
            .faults
            .push_back((operation.into(), fault));
        Ok(())
    }

    /// Copies the retained trace without resetting event sequence numbers.
    pub fn trace(&self) -> Vec<Event> {
        self.0.borrow().trace.clone()
    }

    /// Drains the retained trace without resetting event sequence numbers.
    pub fn take_trace(&self) -> Vec<Event> {
        std::mem::take(&mut self.0.borrow_mut().trace)
    }

    /// Counts open resources, including connections waiting to be accepted.
    pub fn live_handles(&self) -> usize {
        self.0.borrow().resources.len()
    }

    /// Reject the next SQ publications before the driver takes pointer ownership.
    /// The reactor must propagate Driver::push's error through its publication path.
    pub fn reject_submissions(&self, count: usize) -> io::Result<()> {
        if count > 16384 {
            return Err(errno(libc::EINVAL));
        }
        self.0.borrow_mut().reject_submissions = count;
        Ok(())
    }

    /// Allocates a deterministic identifier shared with resource creation.
    pub fn next_sequence(&self) -> u64 {
        self.0.borrow_mut().id()
    }

    /// Disconnect an established stream even while production owns both handles.
    /// Already received bytes remain readable, then reads return EOF.
    pub fn disconnect(&self, descriptor: &Descriptor) -> io::Result<()> {
        let handle = self.handle(descriptor)?;
        let mut w = self.0.borrow_mut();
        let Some(Resource::Socket { peer, .. }) = w.resources.get_mut(&handle.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let other = peer.take();
        if let Some(Resource::Socket { peer, .. }) = other.and_then(|id| w.resources.get_mut(&id)) {
            *peer = None;
        }
        w.record("disconnect", handle.id, 0);
        Ok(())
    }

    /// Sets the bounded receive queue capacity used by every stream.
    pub fn set_stream_capacity(&self, capacity: usize) -> io::Result<()> {
        if capacity == 0 || capacity > MAX_ALLOCATION {
            return Err(errno(libc::EINVAL));
        }
        self.0.borrow_mut().stream_capacity = capacity;
        Ok(())
    }

    /// Select cancellation CQE ordering in the simulated kernel.
    pub fn set_cancel_first(&self, cancel_first: bool) {
        self.0.borrow_mut().cancel_first = cancel_first;
    }

    /// Caps each stream or file transfer without allocating a buffer.
    pub fn set_max_chunk(&self, bytes: usize) -> io::Result<()> {
        if bytes == 0 {
            return Err(errno(libc::EINVAL));
        }
        self.0.borrow_mut().max_chunk = bytes;
        Ok(())
    }

    /// Registers a resource and returns its sole closing owner.
    fn insert(&self, resource: Resource) -> Descriptor {
        let mut w = self.0.borrow_mut();
        let id = w.id();
        w.resources.insert(id, resource);
        w.record("create", id, 0);
        Descriptor::from(Handle {
            sim: self.clone(),
            id,
        })
    }
}

impl Default for SimOs {
    /// Create an isolated world with a durable root directory.
    fn default() -> Self {
        Self::new()
    }
}

impl Handle {
    /// Returns the resource's stable identifier within its simulation.
    pub fn id(&self) -> u64 {
        self.id
    }
}

impl SimOs {
    /// Borrows a descriptor only when it belongs to this world.
    fn handle(&self, fd: &Descriptor) -> io::Result<Handle> {
        fd.as_sim()
            .filter(|handle| Rc::ptr_eq(&self.0, &handle.sim.0))
            .ok_or_else(|| errno(libc::EXDEV))
    }

    /// A view of descriptor `raw` in this world.
    fn view(&self, raw: i32) -> Handle {
        Handle {
            sim: self.clone(),
            id: raw as u64,
        }
    }

    /// Nonblocking accept on a simulated listener.
    pub(crate) fn try_accept(&self, raw: i32) -> io::Result<Descriptor> {
        self.view(raw).accept()
    }

    /// Nonblocking send on a simulated socket.
    pub(crate) fn try_send(&self, raw: i32, bytes: &[u8]) -> io::Result<usize> {
        self.view(raw).send(bytes)
    }

    /// Nonblocking receive on a simulated socket.
    pub(crate) fn try_recv(&self, raw: i32, bytes: &mut [u8]) -> io::Result<usize> {
        self.view(raw).recv(bytes)
    }

    /// Whether an idle simulated socket is reusable.
    pub(crate) fn idle_healthy(&self, raw: i32) -> bool {
        self.view(raw).idle_healthy()
    }

    /// Whether the simulated peer fully closed.
    pub(crate) fn peer_disconnected(&self, raw: i32) -> bool {
        self.view(raw).peer_disconnected()
    }

    /// Whether the simulated peer shut down its write half.
    pub(crate) fn peer_read_closed(&self, raw: i32) -> bool {
        self.view(raw).peer_read_closed()
    }

    /// Shut down a simulated socket.
    pub(crate) fn shutdown(&self, raw: i32, how: i32) -> io::Result<()> {
        self.view(raw).shutdown(how)
    }

    /// Require a simulated stream socket.
    pub(crate) fn validate_socket(&self, raw: i32) -> io::Result<()> {
        self.view(raw).validate_socket()
    }
}

impl Descriptor {
    /// A non-owning view of the simulated resource, if this descriptor is
    /// simulated. The descriptor keeps closing ownership.
    pub fn as_sim(&self) -> Option<Handle> {
        self.sim_os().map(|os| os.view(self.raw()))
    }
}

impl From<Handle> for Descriptor {
    /// Take closing ownership of a simulated resource.
    fn from(handle: Handle) -> Self {
        Descriptor::sim(handle.sim, handle.id as i32)
    }
}

/// Borrows a Unix pathname without allocating or interpreting its bytes.
fn c_path(name: &CStr) -> &Path {
    use std::os::unix::ffi::OsStrExt;
    Path::new(std::ffi::OsStr::from_bytes(name.to_bytes()))
}
