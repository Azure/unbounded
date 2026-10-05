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
mod driver;
#[cfg(test)]
mod followup_tests;
mod network;
#[cfg(test)]
mod review_tests;
use disk::check_direct;
pub(super) use driver::{Driver, Op};

mod disk;
pub use disk::{CrashDisk, DiskState};
#[cfg(test)]
mod disk_tests;
#[cfg(test)]
mod io_tests;

/// Labels new outbound streams with their owning node's listening endpoint.
/// Enter this scope when polling that node; established sockets retain the label.
pub struct EndpointEnvironment {
    sim: Simulation,
    previous: Option<SocketAddress>,
}
impl Drop for EndpointEnvironment {
    fn drop(&mut self) {
        self.sim.0.borrow_mut().endpoint = self.previous.take();
    }
}

#[derive(Clone, Debug)]
pub struct Simulation(Rc<RefCell<World>>);
pub struct Environment {
    previous: Option<Simulation>,
}
impl Drop for Environment {
    fn drop(&mut self) {
        CURRENT.with(|s| *s.borrow_mut() = self.previous.take());
    }
}

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
#[derive(Debug)]
struct Node {
    inode: u64,
    mode: u16,
    length: u64,
    pages: BTreeMap<u64, Rc<[u8; 4096]>>,
    locked: bool,
    symlink: Option<PathBuf>,
}

#[derive(Debug)]
pub struct Handle {
    sim: Simulation,
    id: u64,
}
impl Drop for Handle {
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

fn errno(n: i32) -> io::Error {
    io::Error::from_raw_os_error(n)
}
// Fixture materialization and each sparse inode have an explicit memory ceiling.
const MAX_ALLOCATION: usize = 64 * 1024 * 1024;
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
    fn partitioned(&self, a: &SocketAddress, b: &SocketAddress) -> bool {
        self.partitions
            .contains(&endpoint_pair(a.clone(), b.clone()))
    }
    fn stream_partitioned(&self, id: u64) -> bool {
        matches!(self.resources.get(&id), Some(Resource::Socket { local: Some(a), remote: Some(b), .. }) if self.partitioned(a,b))
    }
    fn id(&mut self) -> u64 {
        let id = self.next;
        self.next += 1;
        id
    }
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
}
fn endpoint_pair(a: SocketAddress, b: SocketAddress) -> (SocketAddress, SocketAddress) {
    if a <= b { (a, b) } else { (b, a) }
}
impl Simulation {
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
    pub fn current() -> Option<Self> {
        CURRENT.with(|s| s.borrow().clone())
    }
    pub fn disk(&self) -> CrashDisk {
        CrashDisk(self.clone())
    }
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
        let Some(h) = fd.as_sim() else {
            return Err(errno(libc::EXDEV));
        };
        if !Rc::ptr_eq(&self.0, &h.sim.0) {
            return Err(errno(libc::EXDEV));
        }
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
    pub fn trace(&self) -> Vec<Event> {
        self.0.borrow().trace.clone()
    }
    pub fn take_trace(&self) -> Vec<Event> {
        std::mem::take(&mut self.0.borrow_mut().trace)
    }
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
    pub fn next_sequence(&self) -> u64 {
        self.0.borrow_mut().id()
    }
    /// Disconnect an established stream even while production owns both handles.
    /// Already received bytes remain readable, then reads return EOF.
    pub fn disconnect(&self, descriptor: &Descriptor) -> io::Result<()> {
        let Some(handle) = descriptor.as_sim() else {
            return Err(errno(libc::EXDEV));
        };
        if !Rc::ptr_eq(&self.0, &handle.sim.0) {
            return Err(errno(libc::EXDEV));
        }
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
    pub fn set_max_chunk(&self, bytes: usize) -> io::Result<()> {
        if bytes == 0 {
            return Err(errno(libc::EINVAL));
        }
        self.0.borrow_mut().max_chunk = bytes;
        Ok(())
    }
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
    fn default() -> Self {
        Self::new()
    }
}
impl Handle {
    pub fn simulation(&self) -> Simulation {
        self.sim.clone()
    }
    pub fn id(&self) -> u64 {
        self.id
    }
}
