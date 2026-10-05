//! Deterministic OS boundary for the production reactor and synchronous syscalls.
//! Operations keep only borrowed pointers: the production Entry owns all backing
//! until this driver has emitted the original and cancellation completions.
use super::Descriptor;
use super::{BufferOperation, CANCEL_BIT, KernelResult, SocketAddress};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet, VecDeque},
    ffi::CString,
    io,
    path::{Path, PathBuf},
    rc::Rc,
};

thread_local! { static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) }; }
mod network;
#[cfg(test)]
use disk::DiskState;
use disk::{CrashDisk, check_direct};

pub mod disk;

/// Labels new outbound streams with their owning node's listening endpoint.
/// Enter this scope when polling that node; established sockets retain the label.
pub struct EndpointEnvironment {
    sim: Simulation,

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
pub struct Simulation(Rc<RefCell<World>>);
/// Restores the previous thread-local simulation when its scope ends.
pub struct Environment {
    previous: Option<Simulation>,
}
impl Drop for Environment {
    /// Restore the previous thread-local backend selection.
    fn drop(&mut self) {
        CURRENT.with(|s| *s.borrow_mut() = self.previous.take());
    }
}

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

    stream_capacity: usize,

    max_chunk: usize,

    executing: bool,

    endpoint: Option<SocketAddress>,

    partitions: BTreeSet<(SocketAddress, SocketAddress)>,

    disk: disk::State,
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

/// Owns a simulated open descriptor and closes it when dropped.
#[derive(Debug)]
pub struct Handle {
    sim: Simulation,

    id: u64,
}
impl Drop for Handle {
    /// Close this resource and pending accepts, releasing its lock and address bindings.
    fn drop(&mut self) {
        let mut w = self.sim.0.borrow_mut();
        match w.resources.remove(&self.id) {
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
        w.listeners.retain(|_, id| *id != self.id);
        w.datagrams.retain(|_, id| *id != self.id);
        w.record("close", self.id, 0);
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
impl Simulation {
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
            stream_capacity: 64 * 1024,
            max_chunk: usize::MAX,
            executing: false,
            endpoint: None,
            partitions: BTreeSet::new(),
            disk: disk::State::default(),
        })));
        sim.create_dir_all(Path::new("/")).unwrap();
        sim.disk().sync_all().unwrap();
        sim
    }
    /// Selection is worker-thread scoped. Reactors and descriptors retain their
    /// environment independently, including while a nested environment is active.
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|s| s.borrow_mut().replace(self.clone())),
        }
    }
    /// Returns the simulation selected for the current worker thread.
    pub fn current() -> Option<Self> {
        CURRENT.with(|s| s.borrow().clone())
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

impl Default for Simulation {
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

/// A submission borrowing memory pinned by the production reactor's entry.
pub(super) enum Op {
    Buffer {
        fd: Rc<Descriptor>,

        operation: BufferOperation,

        ptr: *mut u8,

        len: usize,
    },
    Poll {
        fd: Rc<Descriptor>,

        interest: u32,
    },
    Accept(Rc<Descriptor>),
    Connect {
        fd: Rc<Descriptor>,

        address: SocketAddress,
    },
    Open {
        dir: Option<Rc<Descriptor>>,

        path: CString,

        flags: i32,

        resolve: u64,
    },
    Stat {
        fd: Rc<Descriptor>,

        ptr: *mut libc::statx,
    },
    Sync(Rc<Descriptor>),
    Mkdir {
        dir: Rc<Descriptor>,

        name: CString,
    },
    Rename {
        dir: Rc<Descriptor>,

        from: CString,

        to: CString,
    },
    Unlink {
        dir: Rc<Descriptor>,

        name: CString,
    },
}

/// Executes submissions in identifier order and emits independent cancel fences.
pub(super) struct Driver {
    sim: Simulation,

    pending: RefCell<BTreeMap<u64, Pending>>,

    completed: RefCell<VecDeque<(u64, KernelResult)>>,
}

/// Scheduling state retained until an operation's original completion is emitted.
struct Pending {
    op: Op,

    delay: usize,

    limit: usize,

    error: Option<i32>,

    hold: usize,

    result: Option<KernelResult>,

    disk_crash: Rc<disk::PendingCrash>,
}

impl Driver {
    /// Creates an empty submission and completion queue for a world.
    pub(super) fn new(sim: Simulation) -> Self {
        Self {
            sim,
            pending: RefCell::default(),
            completed: RefCell::default(),
        }
    }

    /// Publishes an operation, capturing its fault and affected disk paths once.
    pub(super) fn push(&mut self, id: u64, op: Op) -> Result<(), ()> {
        {
            let mut w = self.sim.0.borrow_mut();
            if w.reject_submissions != 0 {
                w.reject_submissions -= 1;
                w.record("reject:sq", id, 0);
                return Err(());
            }
        }
        let name = op.name();
        let fault = self.sim.0.borrow_mut().fault(name);
        let disk_paths = op.disk_paths(&self.sim);
        let mut pending = Pending {
            disk_crash: self.sim.0.borrow_mut().disk.watch(disk_paths),
            op,
            delay: 0,
            limit: usize::MAX,
            error: None,
            hold: 0,
            result: None,
        };
        match fault {
            Some(Fault::Errno(n)) => pending.error = Some(n),
            Some(Fault::Short(n)) => pending.limit = n,
            Some(Fault::Delay(n)) => pending.delay = n,
            Some(Fault::HoldCompletion(n)) => pending.hold = n,
            None => (),
        }
        self.sim
            .0
            .borrow_mut()
            .record(&format!("submit:{name}"), id, 0);
        self.pending.borrow_mut().insert(id, pending);
        Ok(())
    }

    /// Retires borrowed pointers before supplying an arbitrary original CQE.
    /// Cancellation CQEs leave the original pending until its own fence.
    #[cfg(test)]
    pub(super) fn inject_completion(&mut self, id: u64, result: i32) -> io::Result<()> {
        if self.completed.borrow().len() >= 16384 {
            return Err(errno(libc::ENOSPC));
        }
        if id & CANCEL_BIT == 0 {
            self.pending.borrow_mut().remove(&id);
        }
        self.completed
            .borrow_mut()
            .push_back((id, KernelResult::Value(result)));
        Ok(())
    }

    /// Swaps queued completions to exercise arbitrary original/cancel orderings.
    #[cfg(test)]
    pub(super) fn reorder_completions(&mut self, first: usize, second: usize) -> io::Result<()> {
        let mut completed = self.completed.borrow_mut();
        if first >= completed.len() || second >= completed.len() {
            return Err(errno(libc::EINVAL));
        }
        completed.swap(first, second);
        Ok(())
    }

    /// Cancels only unexecuted work, preserving held results and both CQE fences.
    pub(super) fn cancel(&mut self, id: u64) {
        let mut pending = self.pending.borrow_mut();
        // An executed operation cannot be canceled retroactively, nor may its
        // held CQE be replaced with ECANCELED and release the owners early.
        let removed = pending.get(&id).is_some_and(|p| p.result.is_none());
        if removed {
            pending.remove(&id);
        }
        let mut completed = self.completed.borrow_mut();
        completed.push_back((
            id | CANCEL_BIT,
            KernelResult::Value(if removed { 0 } else { -libc::ENOENT }),
        ));
        if removed {
            completed.push_back((id, KernelResult::Value(-libc::ECANCELED)));
            if !self.sim.0.borrow().cancel_first {
                let len = completed.len();
                completed.swap(len - 2, len - 1);
            }
        }
        self.sim
            .0
            .borrow_mut()
            .record("cancel", id, i64::from(removed));
    }

    /// Advances every pending operation by one deterministic submission turn.
    pub(super) fn submit(&self) {
        let mut pending = self.pending.borrow_mut();
        let mut done = Vec::new();
        for (&id, operation) in pending.iter_mut() {
            if let Some(result) = operation.advance(&self.sim, id) {
                self.completed.borrow_mut().push_back((id, result));
                done.push(id);
            }
        }
        // Keep descriptor owners alive until every operation has had its turn.
        for id in done {
            pending.remove(&id);
        }
    }

    /// Removes the oldest completion without advancing simulated execution.
    pub(super) fn pop(&mut self) -> Option<(u64, KernelResult)> {
        self.completed.borrow_mut().pop_front()
    }
}

impl Pending {
    /// Executes once when ready, then holds the actual result for its full delay.
    fn advance(&mut self, sim: &Simulation, id: u64) -> Option<KernelResult> {
        if self.result.is_some() {
            if self.hold != 0 {
                self.hold -= 1;
                return None;
            }
            return self.result.take();
        }
        if self.delay != 0 {
            self.delay -= 1;
            return None;
        }
        sim.0.borrow_mut().executing = true;
        let injected = self.error.take();
        let result = match injected {
            Some(n) => Err(errno(n)),
            None if self.disk_crash.crashed.get() => Err(errno(libc::EIO)),
            None => self.op.execute(sim, self.limit),
        };
        sim.0.borrow_mut().executing = false;
        if injected.is_none()
            && self.op.waits_for_readiness()
            && result
                .as_ref()
                .is_err_and(|error| error.kind() == io::ErrorKind::WouldBlock)
        {
            return None;
        }
        let result = result.unwrap_or_else(|error| {
            KernelResult::Value(-error.raw_os_error().unwrap_or(libc::EIO))
        });
        let value = match &result {
            KernelResult::Value(n) => *n as i64,
            KernelResult::Accepted(fd) => fd.as_sim().expect("simulated completion").id as i64,
        };
        sim.0
            .borrow_mut()
            .record(&format!("complete:{}", self.op.name()), id, value);
        if self.hold != 0 {
            self.result = Some(result);
            None
        } else {
            Some(result)
        }
    }
}

impl Op {
    /// Resolves the storage subtrees whose crashes invalidate this submission.
    fn disk_paths(&self, sim: &Simulation) -> Vec<PathBuf> {
        let w = sim.0.borrow();
        let file_path = |fd: &Descriptor| {
            let handle = fd.as_sim()?;
            match w.resources.get(&handle.id) {
                Some(Resource::File { opened_path, .. }) => Some(opened_path.clone()),
                _ => None,
            }
        };
        let path = |dir: Option<&Descriptor>, name: &CString| w.path(dir, c_path(name)).ok();
        match self {
            Self::Buffer {
                fd,
                operation: BufferOperation::Read(_) | BufferOperation::Write(_),
                ..
            }
            | Self::Stat { fd, .. }
            | Self::Sync(fd) => file_path(fd).into_iter().collect(),
            Self::Open {
                dir,
                path: name,
                resolve,
                flags,
            } => w
                .open_path(dir.as_deref(), c_path(name), *resolve, *flags)
                .ok()
                .into_iter()
                .collect(),
            Self::Mkdir { dir, name } | Self::Unlink { dir, name } => {
                path(Some(dir), name).into_iter().collect()
            }
            Self::Rename { dir, from, to } => [path(Some(dir), from), path(Some(dir), to)]
                .into_iter()
                .flatten()
                .collect(),
            _ => Vec::new(),
        }
    }

    /// Names the operation for fault matching and deterministic event recording.
    fn name(&self) -> &'static str {
        match self {
            Self::Buffer { operation, .. } => match operation {
                BufferOperation::Read(_) => "read",
                BufferOperation::Write(_) => "write",
                BufferOperation::Recv => "recv",
                BufferOperation::Send => "send",
            },
            Self::Poll { .. } => "poll",
            Self::Accept(_) => "accept",
            Self::Connect { .. } => "connect",
            Self::Open { .. } => "open",
            Self::Stat { .. } => "stat",
            Self::Sync(_) => "fsync",
            Self::Mkdir { .. } => "mkdir",
            Self::Rename { .. } => "rename",
            Self::Unlink { .. } => "unlink",
        }
    }

    /// Identifies operations for which a real EAGAIN means retry next turn.
    fn waits_for_readiness(&self) -> bool {
        matches!(
            self,
            Self::Buffer {
                operation: BufferOperation::Recv | BufferOperation::Send,
                ..
            } | Self::Poll { .. }
                | Self::Accept(_)
                | Self::Connect { .. }
        )
    }

    /// Performs one syscall attempt using memory owned by the production entry.
    fn execute(&self, sim: &Simulation, limit: usize) -> io::Result<KernelResult> {
        match self {
            Self::Buffer {
                fd,
                operation,
                ptr,
                len,
            } => {
                let fd = sim.handle(fd)?;
                if let BufferOperation::Read(offset) | BufferOperation::Write(offset) = operation {
                    let (_, flags) = fd.node()?;
                    check_direct(flags, *offset, *ptr, *len)?;
                }
                let len = (*len).min(limit).min(i32::MAX as usize);
                let n = match operation {
                    // SAFETY: receive/read entries own exclusive IoBuffers through
                    // both fences; immutable sends may have shared aliases.
                    BufferOperation::Read(offset) => fd.file_read(*offset, unsafe {
                        std::slice::from_raw_parts_mut(*ptr, len)
                    }),
                    BufferOperation::Recv => {
                        fd.recv(unsafe { std::slice::from_raw_parts_mut(*ptr, len) })
                    }
                    BufferOperation::Write(offset) => {
                        fd.file_write(*offset, unsafe { std::slice::from_raw_parts(*ptr, len) })
                    }
                    BufferOperation::Send => {
                        fd.send(unsafe { std::slice::from_raw_parts(*ptr, len) })
                    }
                }?;
                Ok(KernelResult::Value(n as i32))
            }
            Self::Poll { fd, interest } => {
                sim.handle(fd)?.ready(*interest).map(KernelResult::Value)
            }
            Self::Accept(fd) => sim.handle(fd)?.accept().map(KernelResult::Accepted),
            Self::Connect { fd, address } => {
                sim.handle(fd)?.connect(address)?;
                Ok(KernelResult::Value(0))
            }
            Self::Open {
                dir,
                path,
                flags,
                resolve,
            } => sim
                .open_resolved(dir.as_deref(), c_path(path), *flags, *resolve)
                .map(KernelResult::Accepted),
            Self::Stat { fd, ptr } => {
                let stat = sim.handle(fd)?.stat()?;
                // SAFETY: the production entry pins the stat buffer until its CQE.
                unsafe {
                    **ptr = stat;
                }
                Ok(KernelResult::Value(0))
            }
            Self::Sync(fd) => {
                sim.handle(fd)?.sync()?;
                Ok(KernelResult::Value(0))
            }
            Self::Mkdir { dir, name } => {
                sim.handle(dir)?;
                let path = sim.0.borrow().path(Some(dir), c_path(name))?;
                if sim.0.borrow().paths.contains_key(&path) {
                    return Err(errno(libc::EEXIST));
                }
                sim.mkdir(&path)?;
                Ok(KernelResult::Value(0))
            }
            Self::Rename { dir, from, to } => {
                sim.handle(dir)?;
                let a = sim.0.borrow().path(Some(dir), c_path(from))?;
                let b = sim.0.borrow().path(Some(dir), c_path(to))?;
                sim.rename(&a, &b, 0)?;
                Ok(KernelResult::Value(0))
            }
            Self::Unlink { dir, name } => {
                sim.handle(dir)?;
                let path = sim.0.borrow().path(Some(dir), c_path(name))?;
                sim.unlink(&path)?;
                Ok(KernelResult::Value(0))
            }
        }
    }
}

impl Simulation {
    /// Borrows a descriptor only when it belongs to this world.
    fn handle<'a>(&self, fd: &'a Descriptor) -> io::Result<&'a Handle> {
        fd.as_sim()
            .filter(|handle| Rc::ptr_eq(&self.0, &handle.sim.0))
            .ok_or_else(|| errno(libc::EXDEV))
    }
}

/// Borrows a Unix pathname without allocating or interpreting its bytes.
fn c_path(name: &CString) -> &Path {
    use std::os::unix::ffi::OsStrExt;
    Path::new(std::ffi::OsStr::from_bytes(name.as_bytes()))
}

/// Cross-resource syscall and scheduling contracts exercised against Linux.
#[cfg(test)]
mod tests {
    use super::network::tests::{drive, poll, reactor, scope};
    use super::*;
    use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};

    /// Requires a syscall failure and extracts its Linux error number for comparison.
    fn code<T>(result: io::Result<T>) -> i32 {
        result
            .err()
            .expect("expected error")
            .raw_os_error()
            .unwrap()
    }

    /// Consume synchronous transfer faults once without stealing driver-selected faults.
    #[test]
    fn transfer_fault_policy_preserves_matching_order_and_execution_ownership() {
        let sim = Simulation::new();
        sim.inject("write", Fault::Short(3)).unwrap();
        sim.inject("recv", Fault::Errno(libc::EINTR)).unwrap();
        sim.inject("*", Fault::Short(0)).unwrap();
        {
            let mut world = sim.0.borrow_mut();
            world.executing = true;
            assert_eq!(world.transfer_limit("recv").unwrap(), usize::MAX);
            assert_eq!(world.faults.len(), 3);
            world.executing = false;
            assert_eq!(code(world.transfer_limit("recv")), libc::EINTR);
            assert_eq!(world.transfer_limit("send").unwrap(), 0);
            assert_eq!(world.transfer_limit("write").unwrap(), 3);
            assert_eq!(world.transfer_limit("read").unwrap(), usize::MAX);
        }
        for fault in [Fault::Delay(2), Fault::HoldCompletion(2)] {
            sim.inject("read", fault).unwrap();
            assert_eq!(
                sim.0.borrow_mut().transfer_limit("read").unwrap(),
                usize::MAX
            );
            assert!(sim.0.borrow().faults.is_empty());
        }
        let faults: Vec<_> = sim
            .take_trace()
            .into_iter()
            .filter(|event| event.operation.starts_with("fault:"))
            .map(|event| (event.operation, event.result))
            .collect();
        assert_eq!(
            faults,
            [
                ("fault:recv".into(), -i64::from(libc::EINTR)),
                ("fault:send".into(), 0),
                ("fault:write".into(), 3),
                ("fault:read".into(), 2),
                ("fault:read".into(), 2),
            ]
        );
    }

    /// Reject foreign and host descriptors even when their numeric resource IDs collide.
    #[test]
    fn stream_mutation_checks_world_provenance_before_resource_lookup() {
        let sim = Simulation::new();
        let other = Simulation::new();
        let (local, peer) = sim.socket_pair();
        let (foreign, _foreign_peer) = other.socket_pair();
        assert_eq!(local.as_sim().unwrap().id(), foreign.as_sim().unwrap().id());
        let (host, _host_peer) = std::os::unix::net::UnixStream::pair().unwrap();
        let host = Descriptor::from(host);
        let a = SocketAddress::Inet("127.0.0.1:101".parse().unwrap());
        let b = SocketAddress::Inet("127.0.0.1:102".parse().unwrap());
        for invalid in [&foreign, &host] {
            assert_eq!(
                code(sim.label_stream(invalid, a.clone(), b.clone())),
                libc::EXDEV
            );
            assert_eq!(code(sim.disconnect(invalid)), libc::EXDEV);
        }
        sim.label_stream(&local, a.clone(), b.clone()).unwrap();
        sim.partition(a.clone(), b.clone());
        assert_eq!(code(local.try_send(b"x")), libc::EAGAIN);
        sim.heal(a, b);
        assert_eq!(local.try_send(b"x").unwrap(), 1);
        sim.disconnect(&local).unwrap();
        assert_eq!(peer.try_recv(&mut [0; 1]).unwrap(), 1);
        assert_eq!(peer.try_recv(&mut [0; 1]).unwrap(), 0);
        assert_eq!(code(local.try_send(b"x")), libc::EPIPE);
    }

    #[test]
    /// Exercise short sparse I/O, append, locks, bounds, and invalid fault inputs.
    fn short_files_zero_writes_append_locks_and_bounds() {
        let sim = Simulation::new();
        let file = sim
            .open(None, Path::new("/f"), libc::O_CREAT | libc::O_RDWR)
            .unwrap()
            .into_sim()
            .unwrap();
        sim.set_max_chunk(2).unwrap();
        assert_eq!(file.file_write(0, b"abcd").unwrap(), 2);
        assert_eq!(file.file_write(100, b"").unwrap(), 0);
        assert_eq!(file.stat().unwrap().stx_size, 2);
        assert_eq!(file.file_write(2, b"cd").unwrap(), 2);
        let mut bytes = [0xcc; 4];
        assert_eq!(file.file_read(0, &mut bytes).unwrap(), 2);
        assert_eq!(bytes, [b'a', b'b', 0xcc, 0xcc]);
        let append = sim
            .open(None, Path::new("/f"), libc::O_RDWR | libc::O_APPEND)
            .unwrap()
            .into_sim()
            .unwrap();
        append.file_write(0, b"ef").unwrap();
        assert_eq!(sim.read_file(Path::new("/f")).unwrap(), b"abcdef");
        file.lock().unwrap();
        file.lock().unwrap();
        assert_eq!(code(append.lock()), libc::EWOULDBLOCK);
        drop(file);
        append.lock().unwrap();
        assert_eq!(code(append.file_write(u64::MAX, b"x")), libc::EINVAL);
        assert_eq!(code(append.file_read(u64::MAX, &mut bytes)), libc::EINVAL);
        assert_eq!(code(append.set_len(u64::MAX)), libc::EINVAL);
        append.set_len(1 << 40).unwrap();
        assert_eq!(code(sim.read_file(Path::new("/f"))), libc::EFBIG);
        assert_eq!(
            code(
                sim.disk()
                    .read(Path::new("/f"), 0, usize::MAX, DiskState::Volatile)
            ),
            libc::EFBIG
        );
        assert_eq!(
            sim.disk()
                .read(Path::new("/f"), 1 << 39, 4, DiskState::Volatile)
                .unwrap(),
            [0; 4]
        );
        assert_eq!(code(sim.set_max_chunk(0)), libc::EINVAL);
        assert_eq!(code(sim.set_stream_capacity(0)), libc::EINVAL);
        assert_eq!(code(sim.set_stream_capacity(usize::MAX)), libc::EINVAL);
        assert_eq!(code(sim.inject("read", Fault::Errno(0))), libc::EINVAL);
    }

    #[test]
    /// Enforce supported resolver boundaries and nonrecursive mkdir semantics.
    fn resolver_policies_and_single_component_mkdir() {
        let sim = Simulation::new();
        sim.write_file(Path::new("/root/child/f"), b"x").unwrap();
        sim.symlink(Path::new("/child/f"), Path::new("/root/absolute"))
            .unwrap();
        sim.symlink(Path::new("child/f"), Path::new("/root/relative"))
            .unwrap();
        let dir = sim
            .open(None, Path::new("/root"), libc::O_DIRECTORY)
            .unwrap();
        for policy in [1, 0x20, 0x40, 0x18] {
            assert_eq!(
                code(sim.open_resolved(Some(&dir), Path::new("child/f"), 0, policy)),
                libc::EINVAL
            );
        }
        assert!(
            sim.open_resolved(Some(&dir), Path::new("relative"), 0, 0x02 | 0x08)
                .is_ok()
        );
        assert_eq!(
            code(sim.open_resolved(Some(&dir), Path::new("relative"), 0, 0x04)),
            libc::ELOOP
        );
        assert_eq!(
            code(sim.open_resolved(Some(&dir), Path::new("absolute"), 0, 0x08)),
            libc::EXDEV
        );
        assert_eq!(
            code(sim.open_resolved(Some(&dir), Path::new("../root/child/f"), 0, 0x08)),
            libc::EXDEV
        );
        assert!(
            sim.open_resolved(Some(&dir), Path::new("child/../child/f"), 0, 0x08)
                .is_ok()
        );
        for name in ["absolute", "/child/f", "../../child/f"] {
            assert!(
                sim.open_resolved(Some(&dir), Path::new(name), 0, 0x10)
                    .is_ok(),
                "{name}"
            );
        }
        assert_eq!(code(sim.mkdir(Path::new("/missing/child"))), libc::ENOENT);
        assert!(sim.metadata(Path::new("/missing")).is_err());
        sim.mkdir(Path::new("/one")).unwrap();
        assert_eq!(code(sim.mkdir(Path::new("/one"))), libc::EEXIST);
        assert_eq!(
            code(sim.mkdir(Path::new("/root/child/f/dir"))),
            libc::ENOTDIR
        );
    }

    #[test]
    /// Attribute pending alias opens to their resolved disk rather than the alias path.
    fn delayed_alias_open_is_invalidated_by_target_disk_not_alias_disk() {
        for crash_root in ["/storage", "/alias"] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let scope = scope();
            sim.write_file(Path::new("/storage/f"), b"x").unwrap();
            sim.create_dir_all(Path::new("/alias")).unwrap();
            sim.symlink(Path::new("/storage/f"), Path::new("/alias/f"))
                .unwrap();
            sim.disk().sync_all().unwrap();
            sim.inject("open", Fault::Delay(2)).unwrap();
            let mut open = r.file_open(
                None,
                CString::new("/alias/f").unwrap(),
                libc::O_RDONLY,
                0,
                &scope,
            );
            assert!(poll(&mut open).is_pending());
            sim.disk().crash_under(Path::new(crash_root)).unwrap();
            let result = drive(&r, open);
            if crash_root == "/storage" {
                assert!(matches!(result, Err(crate::Error::Os(libc::EIO))));
            } else {
                assert!(result.is_ok());
            }
        }
    }

    #[test]
    /// Preserve socket families, half-close semantics, and renamed Unix listeners.
    fn socket_families_half_close_and_renamed_listener_descendants() {
        let sim = Simulation::new();
        let address = SocketAddress::Inet("127.0.0.1:8080".parse().unwrap());
        let listener = sim.listen(address.clone()).unwrap().into_sim().unwrap();
        let wrong = sim.socket(libc::AF_UNIX).unwrap().into_sim().unwrap();
        assert_eq!(code(wrong.connect(&address)), libc::EAFNOSUPPORT);
        let client = sim.connect(address).unwrap().into_sim().unwrap();
        let server = listener.accept().unwrap().into_sim().unwrap();
        client.send(b"hello").unwrap();
        client.shutdown(libc::SHUT_WR).unwrap();
        assert_eq!(code(client.send(b"x")), libc::EPIPE);
        assert_eq!(
            server.ready(libc::POLLRDHUP as u32).unwrap(),
            libc::POLLRDHUP as i32
        );
        let mut bytes = [0; 8];
        assert_eq!(server.recv(&mut bytes).unwrap(), 5);
        assert_eq!(server.recv(&mut bytes).unwrap(), 0);
        server.send(b"reply").unwrap();
        assert_eq!(client.recv(&mut bytes).unwrap(), 5);
        assert!(!server.idle_healthy());
        assert!(!server.peer_disconnected());
        assert!(server.peer_read_closed());
        assert_eq!(code(client.shutdown(123)), libc::EINVAL);
        sim.create_dir_all(Path::new("/old/child")).unwrap();
        let listener = sim
            .listen(SocketAddress::Unix("/old/child/socket".into()))
            .unwrap();
        sim.rename(Path::new("/old"), Path::new("/new"), 0).unwrap();
        assert!(
            sim.connect(SocketAddress::Unix("/old/child/socket".into()))
                .is_err()
        );
        assert!(
            sim.connect(SocketAddress::Unix("/new/child/socket".into()))
                .is_ok()
        );
        drop(listener);
    }

    #[test]
    /// Check pipe close errors, small-write atomicity, and empty splice behavior.
    fn pipe_close_atomicity_and_empty_splice() {
        let sim = Simulation::new();
        let (read, write) = sim.pipe(4096);
        let (read, write) = (read.into_sim().unwrap(), write.into_sim().unwrap());
        let (socket, _peer) = sim.socket_pair();
        let socket = socket.into_sim().unwrap();
        assert_eq!(code(read.splice(&socket, 1)), libc::EAGAIN);
        assert_eq!(read.splice(&socket, 0).unwrap(), 0);
        write.pipe_write(&[1; 4095]).unwrap();
        assert_eq!(code(write.pipe_write(&[2; 2])), libc::EAGAIN);
        assert_eq!(write.pipe_write(&[2; 4097]).unwrap(), 1);
        let mut bytes = [0; 4096];
        assert_eq!(read.pipe_read(&mut bytes).unwrap(), 4096);
        assert_eq!(bytes[4095], 2);
        drop(write);
        assert_eq!(read.pipe_read(&mut bytes).unwrap(), 0);
        assert_eq!(read.splice(&socket, 1).unwrap(), 0);
        let (read, write) = sim.pipe(4096);
        drop(read);
        let write = write.into_sim().unwrap();
        assert_eq!(code(write.pipe_write(b"x")), libc::EPIPE);
        assert_eq!(write.pipe_write(b"").unwrap(), 0);
    }

    #[test]
    /// Exercise rejected publication, explicit completion reordering, and bounded traces.
    fn scheduling_hooks_reject_sq_and_reorder_arbitrary_completions() {
        let sim = Simulation::new();
        let mut driver = Driver::new(sim.clone());
        let (fd, _peer) = sim.socket_pair();
        let fd = Rc::new(fd);
        sim.reject_submissions(1).unwrap();
        assert!(
            driver
                .push(
                    1,
                    Op::Poll {
                        fd: fd.clone(),
                        interest: libc::POLLIN as u32
                    }
                )
                .is_err()
        );
        assert!(driver.pending.borrow().is_empty());
        driver
            .push(
                2,
                Op::Poll {
                    fd,
                    interest: libc::POLLIN as u32,
                },
            )
            .unwrap();
        driver.inject_completion(2, -libc::EIO).unwrap();
        driver
            .inject_completion(2 | CANCEL_BIT, -libc::ENOENT)
            .unwrap();
        driver.reorder_completions(0, 1).unwrap();
        assert_eq!(driver.pop().unwrap().0, 2 | CANCEL_BIT);
        assert_eq!(driver.pop().unwrap().0, 2);
        driver.submit();
        assert!(driver.pop().is_none());
        assert_eq!(code(driver.reorder_completions(0, 1)), libc::EINVAL);
        for _ in 0..20000 {
            sim.0.borrow_mut().record("bounded", 0, 0);
        }
        assert!(sim.trace().len() <= 16384);
    }

    #[test]
    /// Compare sparse writes, append, locks, and pipe EOF directly against Linux.
    fn linux_file_and_pipe_differential() {
        let raw = unsafe { libc::memfd_create(c"sim-differential".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(raw >= 0);
        let real = unsafe { OwnedFd::from_raw_fd(raw) };
        let sim = Simulation::new();
        let file = sim
            .open(None, Path::new("/f"), libc::O_CREAT | libc::O_RDWR)
            .unwrap()
            .into_sim()
            .unwrap();
        for (offset, bytes) in [(0, b"abc".as_slice()), (100, b""), (2, b"XY")] {
            let actual = unsafe {
                libc::pwrite(real.as_raw_fd(), bytes.as_ptr().cast(), bytes.len(), offset)
            };
            assert_eq!(
                file.file_write(offset as u64, bytes).unwrap(),
                actual as usize
            );
            let mut stat: libc::stat = unsafe { std::mem::zeroed() };
            assert_eq!(unsafe { libc::fstat(real.as_raw_fd(), &mut stat) }, 0);
            assert_eq!(file.stat().unwrap().stx_size, stat.st_size as u64);
        }
        assert_eq!(
            unsafe { libc::flock(real.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
            0
        );
        file.lock().unwrap();
        assert_eq!(
            unsafe { libc::fcntl(real.as_raw_fd(), libc::F_SETFL, libc::O_APPEND) },
            0
        );
        let append = sim
            .open(None, Path::new("/f"), libc::O_RDWR | libc::O_APPEND)
            .unwrap()
            .into_sim()
            .unwrap();
        assert_eq!(
            unsafe { libc::pwrite(real.as_raw_fd(), b"Z".as_ptr().cast(), 1, 0) },
            append.file_write(0, b"Z").unwrap() as isize
        );
        let mut actual = [0; 8];
        let count = unsafe {
            libc::pread(
                real.as_raw_fd(),
                actual.as_mut_ptr().cast(),
                actual.len(),
                0,
            )
        };
        assert_eq!(
            &actual[..count as usize],
            sim.read_file(Path::new("/f")).unwrap()
        );
        assert_eq!(
            unsafe { libc::flock(real.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
            0
        );
        file.lock().unwrap();
        let mut fds = [-1; 2];
        assert_eq!(
            unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_NONBLOCK | libc::O_CLOEXEC) },
            0
        );
        let reader = unsafe { OwnedFd::from_raw_fd(fds[0]) };
        let writer = unsafe { OwnedFd::from_raw_fd(fds[1]) };
        let (sr, sw) = sim.pipe(4096);
        let sr = sr.into_sim().unwrap();
        let mut byte = [0];
        assert_eq!(
            unsafe { libc::read(reader.as_raw_fd(), byte.as_mut_ptr().cast(), 1) },
            -1
        );
        assert_eq!(
            code(sr.pipe_read(&mut byte)),
            io::Error::last_os_error().raw_os_error().unwrap()
        );
        drop((writer, sw));
        assert_eq!(
            unsafe { libc::read(reader.as_raw_fd(), byte.as_mut_ptr().cast(), 1) },
            sr.pipe_read(&mut byte).unwrap() as isize
        );
    }

    #[test]
    /// Compare openat2 traversal policy errors against host syscalls.
    fn linux_openat2_policy_differential() {
        let directory = std::fs::File::open(".").unwrap();
        let sim = Simulation::new();
        sim.mkdir(Path::new("/root")).unwrap();
        let dir = sim
            .open(None, Path::new("/root"), libc::O_DIRECTORY)
            .unwrap();
        /// Linux openat2 argument layout used to compare path-resolution policies.
        #[repr(C)]
        struct How {
            flags: u64,

            mode: u64,

            resolve: u64,
        }
        for (name, policy) in [
            (".", 0x02),
            (".", 0x04),
            (".", 0x08),
            ("/", 0x08),
            ("..", 0x08),
            ("..", 0x10),
            ("/", 0x10),
            (".", 0x18),
            (".", 0x40),
        ] {
            let how = How {
                flags: libc::O_DIRECTORY as u64,
                mode: 0,
                resolve: policy,
            };
            let name_c = CString::new(name).unwrap();
            let result = unsafe {
                libc::syscall(
                    libc::SYS_openat2,
                    directory.as_raw_fd(),
                    name_c.as_ptr(),
                    &how,
                    std::mem::size_of::<How>(),
                )
            };
            let expected = if result < 0 {
                io::Error::last_os_error().raw_os_error()
            } else {
                drop(unsafe { OwnedFd::from_raw_fd(result as i32) });
                None
            };
            let actual = sim
                .open_resolved(Some(&dir), Path::new(name), libc::O_DIRECTORY, policy)
                .err()
                .and_then(|error| error.raw_os_error());
            assert_eq!(actual, expected, "{name} {policy:#x}");
        }
    }

    #[test]
    /// Compare stream half-close bytes and readiness directly against Linux.
    fn linux_socket_half_close_differential() {
        use std::io::{Read, Write};
        let (mut a, mut b) = std::os::unix::net::UnixStream::pair().unwrap();
        let sim = Simulation::new();
        let (sa, sb) = sim.socket_pair();
        let (sa, sb) = (sa.into_sim().unwrap(), sb.into_sim().unwrap());
        a.write_all(b"x").unwrap();
        sa.send(b"x").unwrap();
        a.shutdown(std::net::Shutdown::Write).unwrap();
        sa.shutdown(libc::SHUT_WR).unwrap();
        let mut pfd = libc::pollfd {
            fd: b.as_raw_fd(),
            events: libc::POLLIN | libc::POLLRDHUP | libc::POLLOUT,
            revents: 0,
        };
        assert_eq!(unsafe { libc::poll(&mut pfd, 1, 0) }, 1);
        assert_eq!(sb.ready(pfd.events as u32).unwrap(), pfd.revents as i32);
        for _ in 0..2 {
            let mut actual = [0; 8];
            let mut expected = [0; 8];
            assert_eq!(
                sb.recv(&mut actual).unwrap(),
                b.read(&mut expected).unwrap()
            );
            assert_eq!(actual, expected);
        }
        b.write_all(b"y").unwrap();
        sb.send(b"y").unwrap();
        let mut expected = [0];
        let mut actual = [0];
        assert_eq!(
            sa.recv(&mut actual).unwrap(),
            a.read(&mut expected).unwrap()
        );
        assert_eq!(actual, expected);
    }

    #[test]
    /// Keep crash watches bounded and reclaim unreachable durable images.
    fn repeated_crashes_compact_history_and_reclaim_orphan_images() {
        let sim = Simulation::new();
        let affected = sim.0.borrow_mut().disk.watch(vec!["/unused/file".into()]);
        let unrelated = sim.0.borrow_mut().disk.watch(vec!["/file".into()]);
        let network = sim.0.borrow_mut().disk.watch(vec![]);
        for _ in 0..4100 {
            sim.disk().crash_under(Path::new("/unused")).unwrap();
        }
        assert!(affected.crashed.get());
        assert!(!unrelated.crashed.get());
        assert!(!network.crashed.get());
        assert_eq!(code(sim.try_pipe(0)), libc::EINVAL);
        assert_eq!(code(sim.try_pipe(usize::MAX)), libc::EINVAL);
        sim.write_file(Path::new("/file"), b"x").unwrap();
        sim.disk().sync_all().unwrap();
        sim.unlink(Path::new("/file")).unwrap();
        sim.disk().sync(Path::new("/")).unwrap();
        sim.disk().crash().unwrap();
        assert!(sim.read_file(Path::new("/file")).is_err());
    }

    #[test]
    /// Preserve unrelated pending disk operations across repeated subtree crashes.
    fn pending_reactor_operations_survive_unrelated_crashes_past_old_history_limit() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        for path in ["/a/file", "/b/file", "/ab/file"] {
            sim.write_file(Path::new(path), b"old").unwrap();
        }
        sim.disk().sync_all().unwrap();
        let mut writes = Vec::new();
        let mut opens = Vec::new();
        for root in ["/a", "/b", "/ab"] {
            let fd = Rc::new(
                sim.open(None, &Path::new(root).join("file"), libc::O_RDWR)
                    .unwrap(),
            );
            sim.inject("write", Fault::Delay(2)).unwrap();
            let mut write = r.write_at(fd, 0, r.file_bytes(b"new").unwrap(), (), &scope);
            assert!(poll(&mut write).is_pending());
            writes.push(write);
            sim.inject("open", Fault::Delay(2)).unwrap();
            let mut open = r.file_open(
                None,
                CString::new(format!("{root}/late")).unwrap(),
                libc::O_CREAT | libc::O_RDWR,
                0,
                &scope,
            );
            assert!(poll(&mut open).is_pending());
            opens.push(open);
        }
        for _ in 0..4100 {
            sim.disk().crash_under(Path::new("/a")).unwrap();
        }
        for (index, write) in writes.into_iter().enumerate() {
            let result = drive(&r, write);
            if index == 0 {
                assert!(matches!(result, Err(crate::Error::Os(libc::EIO))));
            } else {
                assert_eq!(result.unwrap().bytes, 3);
            }
        }
        for (index, open) in opens.into_iter().enumerate() {
            let result = drive(&r, open);
            if index == 0 {
                assert!(matches!(result, Err(crate::Error::Os(libc::EIO))));
            } else {
                assert!(result.is_ok());
            }
        }
        assert_eq!(sim.read_file(Path::new("/a/file")).unwrap(), b"old");
        assert_eq!(sim.read_file(Path::new("/b/file")).unwrap(), b"new");
        assert_eq!(sim.read_file(Path::new("/ab/file")).unwrap(), b"new");
        assert!(sim.metadata(Path::new("/a/late")).is_err());
        assert_eq!(r.in_flight(), 0);
    }

    /// Owns a worktree-local differential fixture directory, removed even on panic.
    struct Directory(PathBuf);
    impl Directory {
        /// Creates the host filesystem fixture beneath this worktree's target directory.
        fn new() -> Self {
            let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("../target")
                .join(format!("sim-open-differential-{}", std::process::id()));
            std::fs::create_dir_all(&root).unwrap();
            Self(root)
        }
    }
    impl Drop for Directory {
        /// Remove the host differential fixture even when assertions unwind.
        fn drop(&mut self) {
            std::fs::remove_dir_all(&self.0).unwrap();
        }
    }

    /// Compare access modes, directory suffixes, and ignored absolute-path dirfds.
    #[test]
    fn linux_open_access_suffix_and_absolute_dirfd_differential() {
        let directory = Directory::new();
        std::fs::create_dir(directory.0.join("dir")).unwrap();
        std::fs::write(directory.0.join("file"), b"data").unwrap();
        let sim = Simulation::new();
        sim.mkdir(Path::new("/dir")).unwrap();
        sim.write_file(Path::new("/file"), b"data").unwrap();
        for (target, link) in [
            ("file", "file-link"),
            ("dir", "dir-link"),
            ("file/", "file-slash"),
            ("file/.", "file-dot"),
            ("dir/", "dir-slash"),
            ("dir/.", "dir-dot"),
        ] {
            std::os::unix::fs::symlink(target, directory.0.join(link)).unwrap();
            sim.symlink(Path::new(target), &Path::new("/").join(link))
                .unwrap();
        }
        let real_dir = std::fs::File::open(&directory.0).unwrap();
        let real_file = std::fs::File::open(directory.0.join("file")).unwrap();
        let sim_dir = sim.open(None, Path::new("/"), libc::O_DIRECTORY).unwrap();
        let sim_file = sim.open(None, Path::new("/file"), libc::O_RDONLY).unwrap();
        /// Linux openat2 ABI used by the independent host comparison.
        #[repr(C)]
        struct How {
            flags: u64,

            mode: u64,

            resolve: u64,
        }
        for name in [
            "file",
            "file/",
            "file/.",
            "file/./",
            "dir",
            "dir/",
            "dir/.",
            "file-link/",
            "file-link/.",
            "dir-link/",
            "dir-link/.",
            "file-slash",
            "file-dot",
            "dir-slash",
            "dir-dot",
        ] {
            for flags in [
                libc::O_RDONLY,
                libc::O_WRONLY,
                libc::O_RDWR,
                libc::O_RDONLY | libc::O_TRUNC,
                libc::O_RDONLY | libc::O_NOFOLLOW,
                libc::O_PATH | libc::O_NOFOLLOW,
            ] {
                for resolve in [0, 0x08, 0x10] {
                    for (absolute, regular_dirfd) in [(false, false), (true, false), (true, true)] {
                        let (host_path, sim_path) = if absolute {
                            if resolve == 0x10 {
                                (Path::new("/").join(name), Path::new("/").join(name))
                            } else {
                                (directory.0.join(name), Path::new("/").join(name))
                            }
                        } else {
                            (PathBuf::from(name), PathBuf::from(name))
                        };
                        // Reset content so every successful truncation has an observable effect.
                        std::fs::write(directory.0.join("file"), b"data").unwrap();
                        sim.write_file(Path::new("/file"), b"data").unwrap();
                        let host_name =
                            CString::new(host_path.as_os_str().as_encoded_bytes()).unwrap();
                        let how = How {
                            flags: flags as u64,
                            mode: 0,
                            resolve,
                        };
                        let raw = unsafe {
                            libc::syscall(
                                libc::SYS_openat2,
                                if regular_dirfd {
                                    real_file.as_raw_fd()
                                } else {
                                    real_dir.as_raw_fd()
                                },
                                host_name.as_ptr(),
                                &how,
                                std::mem::size_of::<How>(),
                            )
                        };
                        let expected = if raw < 0 {
                            Err(io::Error::last_os_error().raw_os_error().unwrap())
                        } else {
                            Ok(unsafe { OwnedFd::from_raw_fd(raw as i32) })
                        };
                        let actual = sim
                            .open_resolved(
                                Some(if regular_dirfd { &sim_file } else { &sim_dir }),
                                &sim_path,
                                flags,
                                resolve,
                            )
                            .map_err(|e| e.raw_os_error().unwrap());
                        assert_eq!(
                            actual.as_ref().err(),
                            expected.as_ref().err(),
                            "{name} flags={flags:#x} resolve={resolve:#x} absolute={absolute} regular_dirfd={regular_dirfd}"
                        );
                        if let (Ok(actual), Ok(expected)) = (actual, expected) {
                            let mut stat: libc::stat = unsafe { std::mem::zeroed() };
                            assert_eq!(unsafe { libc::fstat(expected.as_raw_fd(), &mut stat) }, 0);
                            assert_eq!(
                                actual.as_sim().unwrap().stat().unwrap().stx_mode as u32
                                    & libc::S_IFMT,
                                stat.st_mode & libc::S_IFMT
                            );
                        }
                        assert_eq!(
                            sim.read_file(Path::new("/file")).unwrap(),
                            std::fs::read(directory.0.join("file")).unwrap()
                        );
                    }
                }
            }
        }
    }

    #[test]
    /// Compare final symlink and O_PATH behavior across supported open policies.
    fn linux_final_symlink_and_path_descriptor_differential() {
        let directory = Directory::new();
        std::fs::create_dir(directory.0.join("real")).unwrap();
        std::fs::write(directory.0.join("real/file"), b"data").unwrap();
        let sim = Simulation::new();
        sim.write_file(Path::new("/real/file"), b"data").unwrap();
        for (target, link) in [
            ("real", "parent"),
            ("file", "real/link"),
            ("missing", "real/dangling"),
            ("loop", "real/loop"),
        ] {
            std::os::unix::fs::symlink(target, directory.0.join(link)).unwrap();
            sim.symlink(Path::new(target), &Path::new("/").join(link))
                .unwrap();
        }
        let real_dir = std::fs::File::open(&directory.0).unwrap();
        let sim_dir = sim.open(None, Path::new("/"), libc::O_DIRECTORY).unwrap();
        /// Linux openat2 argument layout used to compare final-symlink handling.
        #[repr(C)]
        struct How {
            flags: u64,

            mode: u64,

            resolve: u64,
        }
        for path in [
            "real/file",
            "real/link",
            "real/dangling",
            "real/loop",
            "parent/link",
            "parent/dangling",
        ] {
            for flags in [
                libc::O_RDONLY,
                libc::O_RDONLY | libc::O_NOFOLLOW,
                libc::O_CREAT | libc::O_EXCL | libc::O_RDWR,
                libc::O_CREAT | libc::O_EXCL | libc::O_RDWR | libc::O_NOFOLLOW,
                libc::O_PATH,
                libc::O_PATH | libc::O_NOFOLLOW,
                libc::O_PATH | libc::O_NOFOLLOW | libc::O_DIRECTORY,
            ] {
                for resolve in [0, 0x04, 0x08] {
                    let how = How {
                        flags: flags as u64,
                        mode: if flags & libc::O_CREAT != 0 { 0o600 } else { 0 },
                        resolve,
                    };
                    let name = CString::new(path).unwrap();
                    let raw = unsafe {
                        libc::syscall(
                            libc::SYS_openat2,
                            real_dir.as_raw_fd(),
                            name.as_ptr(),
                            &how,
                            std::mem::size_of::<How>(),
                        )
                    };
                    let expected = if raw < 0 {
                        Err(io::Error::last_os_error().raw_os_error().unwrap())
                    } else {
                        Ok(unsafe { OwnedFd::from_raw_fd(raw as i32) })
                    };
                    let actual = sim
                        .open_resolved(Some(&sim_dir), Path::new(path), flags, resolve)
                        .map_err(|error| error.raw_os_error().unwrap());
                    assert_eq!(
                        actual.as_ref().err(),
                        expected.as_ref().err(),
                        "{path} flags={flags:#x} resolve={resolve:#x}"
                    );
                    if let (Ok(actual), Ok(expected)) = (actual, expected) {
                        let h = actual.as_sim().unwrap();
                        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
                        assert_eq!(unsafe { libc::fstat(expected.as_raw_fd(), &mut stat) }, 0);
                        assert_eq!(
                            h.stat().unwrap().stx_mode as u32 & libc::S_IFMT,
                            stat.st_mode & libc::S_IFMT
                        );
                        if flags & libc::O_PATH != 0 {
                            let mut bytes = [0; 4];
                            assert_eq!(
                                unsafe {
                                    libc::pread(
                                        expected.as_raw_fd(),
                                        bytes.as_mut_ptr().cast(),
                                        4,
                                        0,
                                    )
                                },
                                -1
                            );
                            assert_eq!(
                                h.file_read(0, &mut bytes).unwrap_err().raw_os_error(),
                                io::Error::last_os_error().raw_os_error()
                            );
                            assert_eq!(unsafe { libc::fsync(expected.as_raw_fd()) }, -1);
                            assert_eq!(
                                h.sync().unwrap_err().raw_os_error(),
                                io::Error::last_os_error().raw_os_error()
                            );
                            assert_eq!(
                                h.file_write(0, b"bad").unwrap_err().raw_os_error(),
                                Some(libc::EBADF)
                            );
                            assert_eq!(h.set_len(0).unwrap_err().raw_os_error(), Some(libc::EBADF));
                            assert_eq!(h.lock().unwrap_err().raw_os_error(), Some(libc::EBADF));
                        }
                    }
                }
            }
        }
        assert_eq!(sim.read_file(Path::new("/real/file")).unwrap(), b"data");
        assert!(sim.metadata(Path::new("/real/missing")).is_err());
    }

    #[test]
    /// Allow metadata but reject data I/O and sync through actual O_PATH submissions.
    fn actual_reactor_path_descriptors_stat_but_reject_io() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        sim.write_file(Path::new("/real/file"), b"data").unwrap();
        sim.symlink(Path::new("real"), Path::new("/parent"))
            .unwrap();
        sim.symlink(Path::new("file"), Path::new("/real/link"))
            .unwrap();
        for (name, flags, mode) in [
            ("/parent/file", libc::O_PATH, libc::S_IFREG),
            (
                "/parent/link",
                libc::O_PATH | libc::O_NOFOLLOW,
                libc::S_IFLNK,
            ),
        ] {
            let fd = drive(
                &r,
                r.file_open(None, CString::new(name).unwrap(), flags, 0, &scope),
            )
            .unwrap();
            assert_eq!(
                drive(&r, r.file_stat(fd.clone(), &scope)).unwrap().stx_mode as u32 & libc::S_IFMT,
                mode
            );
            assert!(matches!(
                drive(
                    &r,
                    r.read_at(fd.clone(), 0, r.file_buffer(4).unwrap(), (), &scope)
                ),
                Err(crate::Error::Os(libc::EBADF))
            ));
            assert!(matches!(
                drive(
                    &r,
                    r.write_at(fd.clone(), 0, r.file_bytes(b"bad").unwrap(), (), &scope)
                ),
                Err(crate::Error::Os(libc::EBADF))
            ));
            assert_eq!(
                drive(&r, r.file_sync(fd, &scope)),
                Err(crate::Error::Os(libc::EBADF))
            );
        }
        assert_eq!(r.in_flight(), 0);
    }

    /// Samples host poll readiness without waiting, including terminal event flags.
    fn linux_ready(fd: &impl AsRawFd, interest: i16) -> i32 {
        let mut pfd = libc::pollfd {
            fd: fd.as_raw_fd(),
            events: interest,
            revents: 0,
        };
        assert!(unsafe { libc::poll(&mut pfd, 1, 0) } >= 0);
        pfd.revents as i32
    }
    /// Converts simulated EAGAIN to poll's zero-event result for differential checks.
    fn simulated_ready(fd: &Descriptor, interest: i16) -> i32 {
        match fd.as_sim().unwrap().ready(interest as u32) {
            Ok(flags) => flags,
            Err(error) => {
                assert_eq!(error.raw_os_error(), Some(libc::EAGAIN));
                0
            }
        }
    }

    #[test]
    /// Compare regular-file readiness across host poll, io_uring, and simulation.
    fn regular_file_readiness_matches_linux_poll_and_uring() {
        let Some(host) = crate::reactor::tests::kernel_reactor(8) else {
            return;
        };
        let raw =
            unsafe { libc::memfd_create(c"sim-poll-differential".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(raw >= 0);
        let file = unsafe { OwnedFd::from_raw_fd(raw) };
        let path = CString::new(format!("/proc/self/fd/{}", file.as_raw_fd())).unwrap();
        let sim = Simulation::new();
        sim.write_file(Path::new("/poll-file"), b"").unwrap();
        let _environment = sim.enter();
        let simulated_reactor = reactor();
        let request = scope();
        for flags in [libc::O_RDONLY, libc::O_WRONLY, libc::O_RDWR, libc::O_PATH] {
            let raw = unsafe { libc::open(path.as_ptr(), flags | libc::O_CLOEXEC) };
            assert!(raw >= 0);
            let real = Rc::new(unsafe { Descriptor::from_raw_fd(raw) });
            let simulated = Rc::new(sim.open(None, Path::new("/poll-file"), flags).unwrap());
            for interest in [libc::POLLIN, libc::POLLOUT, libc::POLLIN | libc::POLLOUT] {
                let host_result = drive(
                    &host,
                    host.readiness(real.clone(), interest as u32, &request),
                );
                let sim_result = drive(
                    &simulated_reactor,
                    simulated_reactor.readiness(simulated.clone(), interest as u32, &request),
                );
                assert_eq!(
                    sim_result, host_result,
                    "flags={flags:#x} interest={interest:#x}"
                );
                if flags == libc::O_PATH {
                    assert_eq!(linux_ready(&*real, interest), libc::POLLNVAL as i32);
                    assert_eq!(host_result, Err(crate::Error::Os(libc::EBADF)));
                } else {
                    assert_eq!(host_result, Ok(linux_ready(&*real, interest) as u32));
                    assert_eq!(simulated_ready(&simulated, interest), interest as i32);
                }
            }
        }
    }

    #[test]
    /// Exhaust shutdown direction, fullness, endpoint, and readiness-interest combinations.
    fn linux_shutdown_matrix_and_outstanding_reactor_polls() {
        let all = libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP;
        for full in [false, true] {
            for how in [libc::SHUT_RD, libc::SHUT_WR, libc::SHUT_RDWR] {
                for endpoint in 0..2 {
                    for interest in [libc::POLLIN, libc::POLLOUT, libc::POLLRDHUP, all] {
                        let (a, b) = std::os::unix::net::UnixStream::pair().unwrap();
                        let real = [a, b];
                        for fd in &real {
                            fd.set_nonblocking(true).unwrap();
                        }
                        let sim = Simulation::new();
                        let _environment = sim.enter();
                        let r = reactor();
                        let scope = scope();
                        sim.set_stream_capacity(1).unwrap();
                        let (sa, sb) = sim.socket_pair();
                        let simulated = [Rc::new(sa), Rc::new(sb)];
                        if full {
                            for fd in &real {
                                let bytes = [0; 4096];
                                loop {
                                    let count = unsafe {
                                        libc::send(
                                            fd.as_raw_fd(),
                                            bytes.as_ptr().cast(),
                                            bytes.len(),
                                            libc::MSG_NOSIGNAL,
                                        )
                                    };
                                    if count < 0 {
                                        assert_eq!(
                                            io::Error::last_os_error().raw_os_error(),
                                            Some(libc::EAGAIN)
                                        );
                                        break;
                                    }
                                    assert!(count > 0);
                                }
                            }
                            for fd in &simulated {
                                fd.try_send(b"x").unwrap();
                            }
                        }
                        let before = linux_ready(&real[endpoint], interest);
                        assert_eq!(simulated_ready(&simulated[endpoint], interest), before);
                        let mut pending =
                            r.readiness(simulated[endpoint].clone(), interest as u32, &scope);
                        assert!(poll(&mut pending).is_pending());
                        // Submit before shutdown only when this interest is not ready.
                        if before == 0 {
                            r.poll_budgeted(8).unwrap();
                            assert!(poll(&mut pending).is_pending());
                        }
                        assert_eq!(unsafe { libc::shutdown(real[1].as_raw_fd(), how) }, 0);
                        simulated[1].as_sim().unwrap().shutdown(how).unwrap();
                        let expected = linux_ready(&real[endpoint], interest);
                        // Full sockets can remain non-writable even when send fails EPIPE.
                        let terminal = if endpoint == 0 {
                            how != libc::SHUT_WR
                        } else {
                            how != libc::SHUT_RD
                        };
                        assert_eq!(
                            simulated_ready(&simulated[endpoint], interest),
                            expected,
                            "full={full} how={how} endpoint={endpoint} interest={interest:#x}"
                        );
                        if expected == 0 {
                            r.poll_budgeted(8).unwrap();
                            assert!(poll(&mut pending).is_pending());
                            drop(pending);
                            drive(&r, r.drain()).unwrap();
                        } else {
                            assert_eq!(drive(&r, pending).unwrap(), expected as u32);
                        }
                        if terminal {
                            assert_eq!(
                                unsafe {
                                    libc::send(
                                        real[endpoint].as_raw_fd(),
                                        b"x".as_ptr().cast(),
                                        1,
                                        libc::MSG_NOSIGNAL,
                                    )
                                },
                                -1
                            );
                            assert_eq!(
                                io::Error::last_os_error().raw_os_error(),
                                Some(libc::EPIPE)
                            );
                            assert_eq!(
                                simulated[endpoint]
                                    .try_send(b"x")
                                    .unwrap_err()
                                    .raw_os_error(),
                                Some(libc::EPIPE)
                            );
                        }
                        assert_eq!(r.in_flight(), 0);
                    }
                }
            }
        }
    }

    #[test]
    /// Exhaust all two-operation CQE interleavings while observing retained owners.
    fn actual_reactor_arbitrary_completion_permutations_keep_both_fences() {
        use crate::reactor::tests::fixtures::ResourceClass;
        // Exhaust all 4! interleavings of two original/cancel pairs.
        for a in 0..4 {
            for b in 0..4 {
                for c in 0..4 {
                    for d in 0..4 {
                        let order = [a, b, c, d];
                        if order.iter().copied().collect::<BTreeSet<_>>().len() != 4 {
                            continue;
                        }
                        let sim = Simulation::new();
                        let _environment = sim.enter();
                        let r = reactor();
                        r.init().unwrap();
                        let scope = scope();
                        let baseline = r.admission.used(ResourceClass::RequestContext);
                        let mut weak_fds = Vec::new();
                        let mut weak_leases = Vec::new();
                        let mut peers = Vec::new();
                        for _ in 0..2 {
                            let (fd, peer) = sim.socket_pair();
                            peers.push(peer);
                            let fd = Rc::new(fd);
                            weak_fds.push(Rc::downgrade(&fd));
                            let lease = Rc::new(());
                            weak_leases.push(Rc::downgrade(&lease));
                            let mut recv = r.recv(fd, r.file_buffer(8).unwrap(), lease, &scope);
                            assert!(poll(&mut recv).is_pending());
                            drop(recv);
                        }
                        let ids: Vec<_> = r.state.borrow().entries.keys().map(|id| id.0).collect();
                        assert_eq!(r.poll_budgeted(2).unwrap(), 0);
                        let original;
                        {
                            let mut state = r.state.borrow_mut();
                            let driver = state.simulation.as_mut().unwrap();
                            original = driver
                                .completed
                                .borrow()
                                .iter()
                                .map(|(id, _)| *id)
                                .collect::<Vec<_>>();
                            assert_eq!(original.len(), 4);
                            let mut current = [0, 1, 2, 3];
                            for (index, wanted) in order.iter().enumerate() {
                                let other = current.iter().position(|id| id == wanted).unwrap();
                                driver.reorder_completions(index, other).unwrap();
                                current.swap(index, other);
                            }
                        }
                        let mut seen = BTreeSet::new();
                        for index in order {
                            assert_eq!(r.poll_budgeted(1).unwrap(), 1);
                            seen.insert(original[index]);
                            for (slot, id) in ids.iter().enumerate() {
                                let fenced = seen.contains(id) && seen.contains(&(id | CANCEL_BIT));
                                assert_eq!(
                                    weak_fds[slot].upgrade().is_none(),
                                    fenced,
                                    "order={order:?}"
                                );
                                assert_eq!(
                                    weak_leases[slot].upgrade().is_none(),
                                    fenced,
                                    "order={order:?}"
                                );
                            }
                        }
                        assert_eq!(r.in_flight(), 0);
                        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
                        assert_eq!(sim.live_handles(), 2);
                        drop(peers);
                        assert_eq!(sim.live_handles(), 0);
                    }
                }
            }
        }
    }

    #[test]
    /// Retire the driver's borrowed receive pointer before an injected original CQE.
    fn actual_reactor_injected_completion_retires_borrowed_pointer() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        let (fd, peer) = sim.socket_pair();
        let fd = Rc::new(fd);
        let weak = Rc::downgrade(&fd);
        let mut recv = r.recv(fd, r.file_buffer(8).unwrap(), (), &scope);
        assert!(poll(&mut recv).is_pending());
        let id = r.state.borrow().entries.keys().next().unwrap().0;
        r.state
            .borrow_mut()
            .simulation
            .as_mut()
            .unwrap()
            .inject_completion(id, -libc::EIO)
            .unwrap();
        assert!(weak.upgrade().is_some());
        assert!(matches!(drive(&r, recv), Err(crate::Error::Os(libc::EIO))));
        assert!(weak.upgrade().is_none());
        r.poll_budgeted(8).unwrap();
        assert!(
            r.state
                .borrow()
                .simulation
                .as_ref()
                .unwrap()
                .pending
                .borrow()
                .is_empty()
        );
        drop(peer);
        assert_eq!(sim.live_handles(), 0);
    }

    #[test]
    /// Reject invalid simulation bounds and fault plans without partial mutation.
    fn bounded_simulation_setters_validate_atomically() {
        let sim = Simulation::new();
        sim.set_stream_capacity(MAX_ALLOCATION).unwrap();
        for invalid in [0, MAX_ALLOCATION + 1, usize::MAX] {
            assert_eq!(
                sim.set_stream_capacity(invalid).unwrap_err().raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(sim.0.borrow().stream_capacity, MAX_ALLOCATION);
            assert_eq!(
                sim.try_pipe(invalid).unwrap_err().raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(sim.live_handles(), 0);
        }
        let (a, b) = sim.try_pipe(MAX_ALLOCATION).unwrap();
        drop((a, b));
        sim.set_max_chunk(usize::MAX).unwrap();
        assert_eq!(
            sim.set_max_chunk(0).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(sim.0.borrow().max_chunk, usize::MAX);
        sim.reject_submissions(16384).unwrap();
        assert_eq!(
            sim.reject_submissions(16385).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(sim.0.borrow().reject_submissions, 16384);
        sim.reject_submissions(0).unwrap();
        for invalid in [i32::MIN, -1, 0, 4096, i32::MAX] {
            assert_eq!(
                sim.inject("read", Fault::Errno(invalid))
                    .unwrap_err()
                    .raw_os_error(),
                Some(libc::EINVAL)
            );
            assert!(sim.0.borrow().faults.is_empty());
        }
        for _ in 0..16384 {
            sim.inject("read", Fault::Errno(4095)).unwrap();
        }
        assert_eq!(
            sim.inject("write", Fault::Short(1))
                .unwrap_err()
                .raw_os_error(),
            Some(libc::ENOSPC)
        );
        assert_eq!(sim.0.borrow().faults.len(), 16384);
        assert!(matches!(
            sim.0.borrow_mut().fault("read"),
            Some(Fault::Errno(4095))
        ));
        sim.inject("write", Fault::Short(1)).unwrap();
        sim.set_cancel_first(false);
        assert!(!sim.0.borrow().cancel_first);
        sim.set_cancel_first(true);
        assert!(sim.0.borrow().cancel_first);
    }

    #[test]
    /// Reject invalid Unix names without creating disk entries or submissions.
    fn unix_address_restrictions_match_shared_encoder_without_disk_side_effects() {
        use std::os::unix::ffi::OsStrExt;
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        let fd = Rc::new(sim.socket(libc::AF_UNIX).unwrap());
        for bytes in [
            b"".as_slice(),
            b"\0abstract",
            b"/embedded\0nul",
            &[b'x'; 108],
        ] {
            let path = PathBuf::from(std::ffi::OsStr::from_bytes(bytes));
            let address = SocketAddress::Unix(path.clone());
            assert!(crate::reactor::encode_address(address.clone()).is_err());
            assert_eq!(
                sim.listen(address.clone()).unwrap_err().raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(
                sim.connect(address.clone()).unwrap_err().raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(
                fd.as_sim()
                    .unwrap()
                    .connect(&address)
                    .unwrap_err()
                    .raw_os_error(),
                Some(libc::EINVAL)
            );
            assert_eq!(
                drive(&r, r.connect(fd.clone(), address, &scope)),
                Err(crate::Error::InvalidInput)
            );
            assert!(!sim.0.borrow().paths.contains_key(&path));
            assert!(sim.0.borrow().listeners.is_empty());
            assert_eq!(sim.live_handles(), 1);
            assert_eq!(r.in_flight(), 0);
        }
    }
}
