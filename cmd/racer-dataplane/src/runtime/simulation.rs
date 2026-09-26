//! Deterministic OS boundary for the production reactor and synchronous syscalls.
//! Operations keep only borrowed pointers: the production Entry owns all backing
//! until this driver has emitted the original and cancellation completions.
use super::{BufferOperation, CANCEL_BIT, Descriptor, KernelResult, SocketAddress};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet, VecDeque},
    ffi::CString,
    io,
    path::{Path, PathBuf},
    rc::Rc,
};

thread_local! { static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) }; }

#[path = "simulation_disk.rs"]
mod disk;
pub use disk::{CrashDisk, DiskState};
#[cfg(test)]
#[path = "simulation_disk_tests.rs"]
mod disk_tests;
#[cfg(test)]
#[path = "simulation_io_tests.rs"]
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
}

#[derive(Debug)]
struct World {
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
                Fault::Short(n) | Fault::Delay(n) => n as i64,
            },
        );
        Some(fault)
    }
    fn path(&self, dir: Option<&Descriptor>, name: &Path) -> io::Result<PathBuf> {
        if name.is_absolute() {
            return normalize(name);
        }
        let base = match dir {
            None => PathBuf::from("/"),
            Some(Descriptor::Sim(h)) => {
                let Some(Resource::File { node, .. }) = self.resources.get(&h.id) else {
                    return Err(errno(libc::ENOTDIR));
                };
                if node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
                self.paths
                    .iter()
                    .find(|(_, n)| Rc::ptr_eq(n, node))
                    .map(|(p, _)| p.clone())
                    .ok_or_else(|| errno(libc::ENOENT))?
            }
            _ => return Err(errno(libc::EBADF)),
        };
        normalize(&base.join(name))
    }
    fn node(&mut self, mode: u16) -> Rc<RefCell<Node>> {
        Rc::new(RefCell::new(Node {
            inode: self.id(),
            mode,
            length: 0,
            pages: BTreeMap::new(),
            locked: false,
            symlink: None,
        }))
    }
    fn resolve(
        &self,
        path: PathBuf,
        boundary: Option<&Path>,
        no_symlinks: bool,
    ) -> io::Result<PathBuf> {
        let mut path = path;
        for _ in 0..40 {
            let mut prefix = PathBuf::from("/");
            let parts: Vec<_> = path.components().collect();
            let mut replacement = None;
            for (index, component) in parts.iter().enumerate() {
                if let std::path::Component::Normal(name) = component {
                    prefix.push(name);
                }
                if let Some(target) = self
                    .paths
                    .get(&prefix)
                    .and_then(|n| n.borrow().symlink.clone())
                {
                    if no_symlinks {
                        return Err(errno(libc::ELOOP));
                    }
                    if target.is_absolute() && boundary.is_some() {
                        return Err(errno(libc::EXDEV));
                    }
                    let mut next = if target.is_absolute() {
                        target
                    } else {
                        prefix.parent().unwrap_or(Path::new("/")).join(target)
                    };
                    for component in &parts[index + 1..] {
                        next.push(component.as_os_str());
                    }
                    let next = normalize(&next)?;
                    if boundary.is_some_and(|b| !next.starts_with(b)) {
                        return Err(errno(libc::EXDEV));
                    }
                    replacement = Some(next);
                    break;
                }
            }
            match replacement {
                Some(next) => path = next,
                None => return Ok(path),
            }
        }
        Err(errno(libc::ELOOP))
    }
}
fn endpoint_pair(a: SocketAddress, b: SocketAddress) -> (SocketAddress, SocketAddress) {
    if a <= b { (a, b) } else { (b, a) }
}
fn normalize(path: &Path) -> io::Result<PathBuf> {
    let mut out = PathBuf::from("/");
    for component in path.components() {
        match component {
            std::path::Component::RootDir | std::path::Component::CurDir => (),
            std::path::Component::Normal(name) => out.push(name),
            _ => return Err(errno(libc::EXDEV)),
        }
    }
    Ok(out)
}

impl Simulation {
    pub fn new() -> Self {
        let sim = Self(Rc::new(RefCell::new(World {
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
        let Descriptor::Sim(h) = fd else {
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
    pub fn inject(&self, operation: &str, fault: Fault) {
        if let Fault::Errno(errno) = &fault {
            assert!(*errno > 0, "fault errno must be positive");
        }
        self.0
            .borrow_mut()
            .faults
            .push_back((operation.into(), fault));
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
    pub fn next_sequence(&self) -> u64 {
        self.0.borrow_mut().id()
    }
    /// Disconnect an established stream even while production owns both handles.
    /// Already received bytes remain readable, then reads return EOF.
    pub fn disconnect(&self, descriptor: &Descriptor) -> io::Result<()> {
        let Descriptor::Sim(handle) = descriptor else {
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
    pub fn set_stream_capacity(&self, capacity: usize) {
        assert!(capacity > 0);
        self.0.borrow_mut().stream_capacity = capacity;
    }
    pub fn set_max_chunk(&self, bytes: usize) {
        assert!(bytes > 0);
        self.0.borrow_mut().max_chunk = bytes;
    }
    fn insert(&self, resource: Resource) -> Descriptor {
        let mut w = self.0.borrow_mut();
        let id = w.id();
        w.resources.insert(id, resource);
        w.record("create", id, 0);
        Descriptor::Sim(Handle {
            sim: self.clone(),
            id,
        })
    }
    pub fn socket(&self, domain: i32) -> io::Result<Descriptor> {
        if ![libc::AF_INET, libc::AF_INET6, libc::AF_UNIX].contains(&domain) {
            return Err(errno(libc::EAFNOSUPPORT));
        }
        let local = self.0.borrow().endpoint.clone();
        Ok(self.insert(Resource::Socket {
            peer: None,
            bytes: VecDeque::new(),
            connected: false,
            local,
            remote: None,
        }))
    }
    pub fn bind_datagram(&self, mut address: std::net::SocketAddr) -> io::Result<Descriptor> {
        if address.port() == 0 {
            let w = self.0.borrow();
            let port = (20000..=65535)
                .find(|port| {
                    address.set_port(*port);
                    !w.datagrams.contains_key(&address)
                })
                .ok_or_else(|| errno(libc::EADDRINUSE))?;
            address.set_port(port);
        }
        if self.0.borrow().datagrams.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Datagram {
            address,
            peer: None,
            packets: VecDeque::new(),
        });
        let Descriptor::Sim(h) = &fd else {
            unreachable!()
        };
        self.0.borrow_mut().datagrams.insert(address, h.id);
        Ok(fd)
    }
    pub fn listen(&self, address: SocketAddress) -> io::Result<Descriptor> {
        if self.0.borrow().listeners.contains_key(&address) {
            return Err(errno(libc::EADDRINUSE));
        }
        let fd = self.insert(Resource::Listener {
            pending: VecDeque::new(),
        });
        let Descriptor::Sim(h) = &fd else {
            unreachable!()
        };
        self.0.borrow_mut().listeners.insert(address.clone(), h.id);
        if let SocketAddress::Unix(path) = address {
            let mut w = self.0.borrow_mut();
            if w.paths.contains_key(&path) {
                drop(w);
                drop(fd);
                return Err(errno(libc::EADDRINUSE));
            }
            let node = w.node(libc::S_IFSOCK as u16 | 0o660);
            w.paths.insert(path, node);
        }
        Ok(fd)
    }
    pub fn connect(&self, address: SocketAddress) -> io::Result<Descriptor> {
        let fd = self.socket(libc::AF_UNIX)?;
        let Descriptor::Sim(h) = &fd else {
            unreachable!()
        };
        h.connect(&address)?;
        Ok(fd)
    }
    pub fn socket_pair(&self) -> (Descriptor, Descriptor) {
        let a = self.socket(libc::AF_UNIX).unwrap();
        let b = self.socket(libc::AF_UNIX).unwrap();
        let (Descriptor::Sim(ah), Descriptor::Sim(bh)) = (&a, &b) else {
            unreachable!()
        };
        let mut w = self.0.borrow_mut();
        for (id, peer) in [(ah.id, bh.id), (bh.id, ah.id)] {
            if let Resource::Socket {
                peer: p, connected, ..
            } = w.resources.get_mut(&id).unwrap()
            {
                *p = Some(peer);
                *connected = true;
            }
        }
        (a, b)
    }
    pub fn pipe(&self, capacity: usize) -> (Descriptor, Descriptor) {
        let bytes = Rc::new(RefCell::new(VecDeque::new()));
        (
            self.insert(Resource::Pipe {
                bytes: bytes.clone(),
                write: false,
                capacity,
            }),
            self.insert(Resource::Pipe {
                bytes,
                write: true,
                capacity,
            }),
        )
    }
    pub fn create_dir_all(&self, path: &Path) -> io::Result<()> {
        let path = normalize(path)?;
        let mut w = self.0.borrow_mut();
        let mut paths: Vec<_> = path.ancestors().collect();
        paths.reverse();
        for path in paths {
            if let Some(node) = w.paths.get(path) {
                if node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
            } else {
                let node = w.node(libc::S_IFDIR as u16 | 0o755);
                w.paths.insert(path.into(), node);
            }
        }
        Ok(())
    }
    pub fn open(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
    ) -> io::Result<Descriptor> {
        self.open_resolved(dir, path, flags, 0)
    }
    pub fn symlink(&self, target: &Path, path: &Path) -> io::Result<()> {
        let path = normalize(path)?;
        let mut w = self.0.borrow_mut();
        if w.paths.contains_key(&path) {
            return Err(errno(libc::EEXIST));
        }
        if !w
            .paths
            .contains_key(path.parent().unwrap_or(Path::new("/")))
        {
            return Err(errno(libc::ENOENT));
        }
        let node = w.node(libc::S_IFLNK as u16 | 0o777);
        node.borrow_mut().symlink = Some(target.into());
        w.paths.insert(path, node);
        w.record("symlink", 0, 0);
        Ok(())
    }
    fn open_resolved(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
        resolve: u64,
    ) -> io::Result<Descriptor> {
        if let Some(dir) = dir {
            match dir {
                Descriptor::Sim(handle) if Rc::ptr_eq(&self.0, &handle.sim.0) => (),
                _ => return Err(errno(libc::EXDEV)),
            }
        }
        let (node, opened_path) = {
            let mut w = self.0.borrow_mut();
            if let Some(Fault::Errno(n)) = w.fault("open") {
                return Err(errno(n));
            }
            if resolve & 0x08 != 0 && path.is_absolute() {
                return Err(errno(libc::EXDEV));
            }
            let boundary = if resolve & 0x08 != 0 {
                Some(w.path(dir, Path::new("."))?)
            } else {
                None
            };
            let path = w.path(dir, path)?;
            if flags & libc::O_NOFOLLOW != 0
                && w.paths
                    .get(&path)
                    .is_some_and(|n| n.borrow().symlink.is_some())
            {
                return Err(errno(libc::ELOOP));
            }
            let path = w.resolve(path, boundary.as_deref(), resolve & 0x04 != 0)?;
            if w.paths.contains_key(&path)
                && flags & (libc::O_CREAT | libc::O_EXCL) == (libc::O_CREAT | libc::O_EXCL)
            {
                return Err(errno(libc::EEXIST));
            }
            if !w.paths.contains_key(&path) {
                if flags & libc::O_CREAT == 0 {
                    return Err(errno(libc::ENOENT));
                }
                let parent = w
                    .paths
                    .get(path.parent().ok_or_else(|| errno(libc::ENOENT))?)
                    .ok_or_else(|| errno(libc::ENOENT))?;
                if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
                    return Err(errno(libc::ENOTDIR));
                }
                let node = w.node(libc::S_IFREG as u16 | 0o600);
                w.paths.insert(path.clone(), node);
            }
            let node = w.paths[&path].clone();
            if flags & libc::O_DIRECTORY != 0
                && node.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR
            {
                return Err(errno(libc::ENOTDIR));
            }
            if flags & libc::O_TRUNC != 0 {
                let mut n = node.borrow_mut();
                if n.mode as u32 & libc::S_IFMT != libc::S_IFREG {
                    return Err(errno(libc::EISDIR));
                }
                if flags & libc::O_ACCMODE == libc::O_RDONLY {
                    return Err(errno(libc::EINVAL));
                }
                n.pages.clear();
                n.length = 0;
            }
            (node, path)
        };
        Ok(self.insert(Resource::File {
            node,
            flags,
            lock_owner: false,
            opened_path,
        }))
    }
    pub fn write_file(&self, path: &Path, bytes: &[u8]) -> io::Result<()> {
        if let Some(parent) = path.parent() {
            self.create_dir_all(parent)?;
        }
        let fd = self.open(None, path, libc::O_CREAT | libc::O_RDWR | libc::O_TRUNC)?;
        let Descriptor::Sim(h) = fd else {
            unreachable!()
        };
        let mut offset = 0;
        while offset < bytes.len() {
            let written = h.file_write(offset as u64, &bytes[offset..])?;
            if written == 0 {
                return Err(errno(libc::EIO));
            }
            offset += written;
        }
        Ok(())
    }
    pub fn read_file(&self, path: &Path) -> io::Result<Vec<u8>> {
        let fd = self.open(None, path, libc::O_RDONLY)?;
        let Descriptor::Sim(h) = fd else {
            unreachable!()
        };
        let mut bytes =
            vec![0; usize::try_from(h.stat()?.stx_size).map_err(|_| errno(libc::EFBIG))?];
        let mut offset = 0;
        while offset < bytes.len() {
            let count = h.file_read(offset as u64, &mut bytes[offset..])?;
            if count == 0 {
                bytes.truncate(offset);
                break;
            }
            offset += count;
        }
        Ok(bytes)
    }
    pub fn metadata(&self, path: &Path) -> io::Result<(u64, u16)> {
        let w = self.0.borrow();
        let node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow();
        Ok((node.inode, node.mode))
    }
    pub fn chmod(&self, path: &Path, mode: u32) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("chmod") {
            return Err(errno(n));
        }
        let mut node = w
            .paths
            .get(&normalize(path)?)
            .ok_or_else(|| errno(libc::ENOENT))?
            .borrow_mut();
        node.mode = (node.mode & libc::S_IFMT as u16) | (mode as u16 & 0o7777);
        Ok(())
    }
    pub fn rename(&self, from: &Path, to: &Path, flags: u32) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("rename") {
            return Err(errno(n));
        }
        let from = normalize(from)?;
        let to = normalize(to)?;
        if flags & !(libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE) != 0
            || flags == (libc::RENAME_NOREPLACE | libc::RENAME_EXCHANGE)
        {
            return Err(errno(libc::EINVAL));
        }
        if !w.paths.contains_key(&from) {
            return Err(errno(libc::ENOENT));
        }
        if from == to {
            return Ok(());
        }
        if from == Path::new("/") || to == Path::new("/") {
            return Err(errno(libc::EBUSY));
        }
        let parent = w
            .paths
            .get(to.parent().unwrap())
            .ok_or_else(|| errno(libc::ENOENT))?;
        if parent.borrow().mode as u32 & libc::S_IFMT != libc::S_IFDIR {
            return Err(errno(libc::ENOTDIR));
        }
        if to.starts_with(&from) || from.starts_with(&to) {
            return Err(errno(libc::EINVAL));
        }
        if flags & libc::RENAME_NOREPLACE != 0 && w.paths.contains_key(&to) {
            return Err(errno(libc::EEXIST));
        }
        if flags & libc::RENAME_EXCHANGE != 0 && !w.paths.contains_key(&to) {
            return Err(errno(libc::ENOENT));
        }
        if flags & libc::RENAME_EXCHANGE == 0
            && let Some(target) = w.paths.get(&to)
        {
            let source_dir = w.paths[&from].borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR;
            let target_dir = target.borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR;
            if source_dir != target_dir {
                return Err(errno(if source_dir {
                    libc::ENOTDIR
                } else {
                    libc::EISDIR
                }));
            }
            if target_dir && w.paths.keys().any(|p| p != &to && p.starts_with(&to)) {
                return Err(errno(libc::ENOTEMPTY));
            }
        }
        let descendants: Vec<_> = w
            .paths
            .iter()
            .filter_map(|(path, node)| {
                if path != &from && path.starts_with(&from) {
                    Some((
                        path.clone(),
                        to.join(path.strip_prefix(&from).unwrap()),
                        node.clone(),
                    ))
                } else if flags & libc::RENAME_EXCHANGE != 0 && path != &to && path.starts_with(&to)
                {
                    Some((
                        path.clone(),
                        from.join(path.strip_prefix(&to).unwrap()),
                        node.clone(),
                    ))
                } else {
                    None
                }
            })
            .collect();
        for (path, _, _) in &descendants {
            w.paths.remove(path);
        }
        for (_, path, node) in descendants {
            w.paths.insert(path, node);
        }
        let source = w.paths.remove(&from).unwrap();
        let target = w.paths.insert(to.clone(), source);
        if flags & libc::RENAME_EXCHANGE != 0 {
            w.paths.insert(from.clone(), target.unwrap());
        }
        // Keep open-description crash ownership attached to the moved inode.
        let locations: BTreeMap<_, _> = w
            .paths
            .iter()
            .map(|(path, node)| (node.borrow().inode, path.clone()))
            .collect();
        for resource in w.resources.values_mut() {
            if let Resource::File {
                node, opened_path, ..
            } = resource
                && let Some(path) = locations.get(&node.borrow().inode)
            {
                *opened_path = path.clone();
            }
        }
        let a = w.listeners.remove(&SocketAddress::Unix(from.clone()));
        let b = w.listeners.remove(&SocketAddress::Unix(to.clone()));
        if let Some(a) = a {
            w.listeners.insert(SocketAddress::Unix(to), a);
        }
        if flags & libc::RENAME_EXCHANGE != 0
            && let Some(b) = b
        {
            w.listeners.insert(SocketAddress::Unix(from), b);
        }
        w.record("rename", 0, 0);
        Ok(())
    }
    pub fn unlink(&self, path: &Path) -> io::Result<()> {
        let mut w = self.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("unlink") {
            return Err(errno(n));
        }
        let path = normalize(path)?;
        if w.paths
            .get(&path)
            .is_some_and(|n| n.borrow().mode as u32 & libc::S_IFMT == libc::S_IFDIR)
        {
            return Err(errno(libc::EISDIR));
        }
        w.paths.remove(&path).ok_or_else(|| errno(libc::ENOENT))?;
        w.listeners.remove(&SocketAddress::Unix(path));
        w.record("unlink", 0, 0);
        Ok(())
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
    pub fn connect_datagram(&self, peer: std::net::SocketAddr) -> io::Result<()> {
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Datagram { peer: target, .. }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        *target = Some(peer);
        Ok(())
    }
    pub fn send_to(&self, bytes: &[u8], target: std::net::SocketAddr) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("send_datagram") {
            return Err(errno(n));
        }
        let Some(Resource::Datagram { address, .. }) = w.resources.get(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let address = *address;
        let target_id = *w
            .datagrams
            .get(&target)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        let Some(Resource::Datagram { packets, peer, .. }) = w.resources.get_mut(&target_id) else {
            unreachable!()
        };
        if peer.is_none_or(|peer| peer == address) {
            if packets.len() >= 64 {
                return Err(errno(libc::EAGAIN));
            }
            packets.push_back((address, bytes.to_vec()));
        }
        w.record("send_datagram", self.id, bytes.len() as i64);
        Ok(bytes.len())
    }
    pub fn recv_from(&self, bytes: &mut [u8]) -> io::Result<(usize, std::net::SocketAddr)> {
        let mut w = self.sim.0.borrow_mut();
        if let Some(Fault::Errno(n)) = w.fault("recv_datagram") {
            return Err(errno(n));
        }
        let Some(Resource::Datagram { packets, .. }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::ENOTSOCK));
        };
        let (address, packet) = packets.pop_front().ok_or_else(|| errno(libc::EAGAIN))?;
        let count = bytes.len().min(packet.len());
        bytes[..count].copy_from_slice(&packet[..count]);
        w.record("recv_datagram", self.id, count as i64);
        Ok((count, address))
    }
    pub fn send_datagram(&self, bytes: &[u8]) -> io::Result<usize> {
        let peer = match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::Datagram {
                peer: Some(peer), ..
            }) => *peer,
            _ => return Err(errno(libc::ENOTCONN)),
        };
        self.send_to(bytes, peer)
    }
    pub fn validate_socket(&self) -> io::Result<()> {
        if matches!(
            self.sim.0.borrow().resources.get(&self.id),
            Some(Resource::Socket { .. })
        ) {
            Ok(())
        } else {
            Err(errno(libc::ENOTSOCK))
        }
    }
    pub fn idle_healthy(&self) -> bool {
        let w = self.sim.0.borrow();
        matches!(w.resources.get(&self.id), Some(Resource::Socket { peer: Some(peer), bytes, .. }) if bytes.is_empty() && w.resources.contains_key(peer))
    }
    pub fn connect(&self, address: &SocketAddress) -> io::Result<()> {
        let mut w = self.sim.0.borrow_mut();
        let local = match w.resources.get(&self.id) {
            Some(Resource::Socket { local, .. }) => local.clone(),
            _ => return Err(errno(libc::ENOTSOCK)),
        };
        if local.as_ref().is_some_and(|a| w.partitioned(a, address)) {
            w.record("blocked:connect", self.id, 0);
            return Err(errno(libc::EAGAIN));
        }
        let listener = *w
            .listeners
            .get(address)
            .ok_or_else(|| errno(libc::ECONNREFUSED))?;
        if !matches!(
            w.resources.get(&self.id),
            Some(Resource::Socket {
                connected: false,
                ..
            })
        ) {
            return Err(errno(libc::EISCONN));
        }
        let peer = w.id();
        w.resources.insert(
            peer,
            Resource::Socket {
                peer: Some(self.id),
                bytes: VecDeque::new(),
                connected: true,
                local: Some(address.clone()),
                remote: local,
            },
        );
        if let Resource::Socket {
            peer: p,
            connected,
            remote,
            ..
        } = w.resources.get_mut(&self.id).unwrap()
        {
            *p = Some(peer);
            *connected = true;
            *remote = Some(address.clone());
        }
        let Some(Resource::Listener { pending }) = w.resources.get_mut(&listener) else {
            return Err(errno(libc::ECONNREFUSED));
        };
        pending.push_back(peer);
        w.record("connect", self.id, 0);
        Ok(())
    }
    pub fn accept(&self) -> io::Result<Descriptor> {
        let mut w = self.sim.0.borrow_mut();
        let Some(Resource::Listener { pending }) = w.resources.get_mut(&self.id) else {
            return Err(errno(libc::EINVAL));
        };
        let id = pending.pop_front().ok_or_else(|| errno(libc::EAGAIN))?;
        w.record("accept", self.id, id as i64);
        Ok(Descriptor::Sim(Handle {
            sim: self.sim.clone(),
            id,
        }))
    }
    pub fn send(&self, bytes: &[u8]) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        let limit = match w.fault("send") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let capacity = w.stream_capacity;
        let max_chunk = w.max_chunk;
        let Some(Resource::Socket {
            peer, connected, ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::ENOTCONN));
        };
        let peer = peer.ok_or_else(|| {
            errno(if *connected {
                libc::EPIPE
            } else {
                libc::ENOTCONN
            })
        })?;
        if !bytes.is_empty() && w.stream_partitioned(self.id) {
            w.record("blocked:send", self.id, bytes.len() as i64);
            return Err(errno(libc::EAGAIN));
        }
        let Some(Resource::Socket { bytes: output, .. }) = w.resources.get_mut(&peer) else {
            return Err(errno(libc::EPIPE));
        };
        let count = bytes
            .len()
            .min(capacity.saturating_sub(output.len()))
            .min(max_chunk)
            .min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output.extend(&bytes[..count]);
        w.record("send", self.id, count as i64);
        Ok(count)
    }
    pub fn recv(&self, bytes: &mut [u8]) -> io::Result<usize> {
        let mut w = self.sim.0.borrow_mut();
        let limit = match w.fault("recv") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let max_chunk = w.max_chunk;
        let Some(Resource::Socket {
            peer, connected, ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::ENOTSOCK));
        };
        if !connected {
            return Err(errno(libc::ENOTCONN));
        }
        let closed = peer.is_none_or(|id| !w.resources.contains_key(&id));
        let Some(Resource::Socket { bytes: input, .. }) = w.resources.get_mut(&self.id) else {
            unreachable!()
        };
        if input.is_empty() && !closed && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        let count = bytes.len().min(input.len()).min(max_chunk).min(limit);
        for byte in &mut bytes[..count] {
            *byte = input.pop_front().unwrap();
        }
        w.record("recv", self.id, count as i64);
        Ok(count)
    }
    fn ready(&self, interest: u32) -> io::Result<i32> {
        let w = self.sim.0.borrow();
        let flags = match w.resources.get(&self.id) {
            Some(Resource::Datagram { packets, .. }) => {
                libc::POLLOUT | if packets.is_empty() { 0 } else { libc::POLLIN }
            }
            Some(Resource::Listener { pending }) => {
                if pending.is_empty() {
                    0
                } else {
                    libc::POLLIN
                }
            }
            Some(Resource::Socket { peer, bytes, .. }) => {
                let remote = peer.and_then(|id| w.resources.get(&id));
                let mut flags = if bytes.is_empty() { 0 } else { libc::POLLIN };
                if let Some(Resource::Socket { bytes, .. }) = remote {
                    if bytes.len() < w.stream_capacity && !w.stream_partitioned(self.id) {
                        flags |= libc::POLLOUT;
                    }
                } else {
                    flags |= libc::POLLHUP | libc::POLLIN | libc::POLLOUT;
                }
                flags
            }
            _ => return Err(errno(libc::EINVAL)),
        };
        let flags = flags as u32 & (interest | libc::POLLHUP as u32);
        if flags == 0 {
            Err(errno(libc::EAGAIN))
        } else {
            Ok(flags as i32)
        }
    }
    fn node(&self) -> io::Result<(Rc<RefCell<Node>>, i32)> {
        match self.sim.0.borrow().resources.get(&self.id) {
            Some(Resource::File { node, flags, .. }) => Ok((node.clone(), *flags)),
            _ => Err(errno(libc::EBADF)),
        }
    }
    pub fn stat(&self) -> io::Result<libc::statx> {
        let (node, _) = self.node()?;
        let node = node.borrow();
        let mut stat: libc::statx = unsafe { std::mem::zeroed() };
        stat.stx_mask = libc::STATX_BASIC_STATS | libc::STATX_DIOALIGN;
        stat.stx_mode = node.mode;
        stat.stx_ino = node.inode;
        stat.stx_size = node.length;
        stat.stx_nlink = 1;
        stat.stx_dio_mem_align = 4096;
        stat.stx_dio_offset_align = 4096;
        stat.stx_uid = 0;
        stat.stx_gid = 0;
        Ok(stat)
    }
    pub fn set_len(&self, length: u64) -> io::Result<()> {
        let (node, flags) = self.node()?;
        if flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let mut node = node.borrow_mut();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EINVAL));
        }
        node.pages.retain(|page, _| *page < length.div_ceil(4096));
        if !length.is_multiple_of(4096)
            && let Some(page) = node.pages.get_mut(&(length / 4096))
        {
            Rc::make_mut(page)[(length % 4096) as usize..].fill(0);
        }
        node.length = length;
        Ok(())
    }
    pub fn lock(&self) -> io::Result<()> {
        let (node, _) = self.node()?;
        let mut n = node.borrow_mut();
        if n.locked {
            return Err(errno(libc::EWOULDBLOCK));
        }
        n.locked = true;
        if let Some(Resource::File { lock_owner, .. }) =
            self.sim.0.borrow_mut().resources.get_mut(&self.id)
        {
            *lock_owner = true;
        }
        Ok(())
    }
    fn file_read(&self, offset: u64, bytes: &mut [u8]) -> io::Result<usize> {
        let limit = match self.sim.0.borrow_mut().fault("read") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let (node, flags) = self.node()?;
        if !self.sim.0.borrow().executing {
            check_direct(flags, offset, bytes.as_ptr(), bytes.len())?;
        }
        if flags & libc::O_ACCMODE == libc::O_WRONLY {
            return Err(errno(libc::EBADF));
        }
        let node = node.borrow();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        let len = bytes
            .len()
            .min(node.length.saturating_sub(offset) as usize)
            .min(limit);
        for (index, byte) in bytes[..len].iter_mut().enumerate() {
            let pos = offset + index as u64;
            *byte = node
                .pages
                .get(&(pos / 4096))
                .map_or(0, |p| p[(pos % 4096) as usize]);
        }
        Ok(len)
    }
    fn file_write(&self, offset: u64, bytes: &[u8]) -> io::Result<usize> {
        let (node, flags) = self.node()?;
        if !self.sim.0.borrow().executing {
            check_direct(flags, offset, bytes.as_ptr(), bytes.len())?;
        }
        let limit = match self.sim.0.borrow_mut().fault("write") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let bytes = &bytes[..bytes.len().min(limit)];
        if flags & libc::O_ACCMODE == libc::O_RDONLY {
            return Err(errno(libc::EBADF));
        }
        let end = offset
            .checked_add(bytes.len() as u64)
            .ok_or_else(|| errno(libc::EFBIG))?;
        let mut node = node.borrow_mut();
        if node.mode as u32 & libc::S_IFMT != libc::S_IFREG {
            return Err(errno(libc::EISDIR));
        }
        for (index, byte) in bytes.iter().enumerate() {
            let pos = offset + index as u64;
            Rc::make_mut(
                node.pages
                    .entry(pos / 4096)
                    .or_insert_with(|| Rc::new([0; 4096])),
            )[(pos % 4096) as usize] = *byte;
        }
        node.length = node.length.max(end);
        Ok(bytes.len())
    }
    pub fn pipe_write(&self, bytes: &[u8]) -> io::Result<usize> {
        let limit = match self.sim.0.borrow_mut().fault("pipe_write") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let w = self.sim.0.borrow();
        let Some(Resource::Pipe {
            bytes: output,
            write: true,
            capacity,
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::EBADF));
        };
        let mut output = output.borrow_mut();
        let count = bytes
            .len()
            .min(capacity.saturating_sub(output.len()))
            .min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        output.extend(&bytes[..count]);
        Ok(count)
    }
    pub fn pipe_read(&self, bytes: &mut [u8]) -> io::Result<usize> {
        let limit = match self.sim.0.borrow_mut().fault("pipe_read") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => n,
            _ => usize::MAX,
        };
        let w = self.sim.0.borrow();
        let Some(Resource::Pipe {
            bytes: input,
            write: false,
            ..
        }) = w.resources.get(&self.id)
        else {
            return Err(errno(libc::EBADF));
        };
        let mut input = input.borrow_mut();
        let count = bytes.len().min(input.len()).min(limit);
        if count == 0 && !bytes.is_empty() {
            return Err(errno(libc::EAGAIN));
        }
        for byte in &mut bytes[..count] {
            *byte = input.pop_front().unwrap();
        }
        Ok(count)
    }
    pub fn splice(&self, socket: &Handle, count: usize) -> io::Result<usize> {
        let count = match self.sim.0.borrow_mut().fault("splice") {
            Some(Fault::Errno(n)) => return Err(errno(n)),
            Some(Fault::Short(n)) => count.min(n),
            _ => count,
        };
        if !Rc::ptr_eq(&self.sim.0, &socket.sim.0) {
            return Err(errno(libc::EXDEV));
        }
        let bytes = {
            let w = self.sim.0.borrow();
            let Some(Resource::Pipe {
                bytes,
                write: false,
                ..
            }) = w.resources.get(&self.id)
            else {
                return Err(errno(libc::EBADF));
            };
            bytes.clone()
        };
        let input: Vec<_> = bytes.borrow().iter().take(count).copied().collect();
        let sent = socket.send(&input)?;
        bytes.borrow_mut().drain(..sent);
        Ok(sent)
    }
}

fn check_direct(flags: i32, offset: u64, ptr: *const u8, length: usize) -> io::Result<()> {
    if flags & libc::O_DIRECT != 0
        && (!offset.is_multiple_of(4096)
            || !(ptr as usize).is_multiple_of(4096)
            || !length.is_multiple_of(4096))
    {
        return Err(errno(libc::EINVAL));
    }
    Ok(())
}

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
impl Op {
    fn disk_paths(&self, sim: &Simulation) -> Vec<PathBuf> {
        let w = sim.0.borrow();
        let file_path = |fd: &Descriptor| match fd {
            Descriptor::Sim(h) => match w.resources.get(&h.id) {
                Some(Resource::File { opened_path, .. }) => Some(opened_path.clone()),
                _ => None,
            },
            _ => None,
        };
        let path = |dir: Option<&Descriptor>, name: &CString| {
            use std::os::unix::ffi::OsStrExt;
            w.path(dir, Path::new(std::ffi::OsStr::from_bytes(name.as_bytes())))
                .ok()
        };
        match self {
            Self::Buffer {
                fd,
                operation: BufferOperation::Read(_) | BufferOperation::Write(_),
                ..
            }
            | Self::Stat { fd, .. }
            | Self::Sync(fd) => file_path(fd).into_iter().collect(),
            Self::Open {
                dir, path: name, ..
            } => path(dir.as_deref(), name).into_iter().collect(),
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
    fn execute(&self, sim: &Simulation, limit: usize) -> io::Result<KernelResult> {
        use std::os::unix::ffi::OsStrExt;
        let path = |name: &CString| PathBuf::from(std::ffi::OsStr::from_bytes(name.as_bytes()));
        let handle = |fd: &Rc<Descriptor>| match &**fd {
            Descriptor::Sim(h) if Rc::ptr_eq(&h.sim.0, &sim.0) => Ok(h.id),
            _ => Err(errno(libc::EXDEV)),
        };
        // Temporary handles are not owners; only call through references below.
        let h = |fd: &Rc<Descriptor>| {
            handle(fd)?;
            Ok::<_, io::Error>(())
        };
        match self {
            Self::Buffer {
                fd,
                operation,
                ptr,
                len,
            } => {
                h(fd)?;
                let Descriptor::Sim(fd) = &**fd else {
                    unreachable!()
                };
                if let BufferOperation::Read(offset) | BufferOperation::Write(offset) = operation {
                    let (_, flags) = fd.node()?;
                    check_direct(flags, *offset, *ptr, *len)?;
                }
                let len = (*len).min(limit).min(i32::MAX as usize);
                // SAFETY: Entry owns the exclusive IoBuffer until both completions.
                let bytes = unsafe { std::slice::from_raw_parts_mut(*ptr, len) };
                let n = match operation {
                    BufferOperation::Read(offset) => fd.file_read(*offset, bytes),
                    BufferOperation::Write(offset) => fd.file_write(*offset, bytes),
                    BufferOperation::Recv => fd.recv(bytes),
                    BufferOperation::Send => fd.send(bytes),
                }?;
                Ok(KernelResult::Value(n as i32))
            }
            Self::Poll { fd, interest } => {
                h(fd)?;
                let Descriptor::Sim(fd) = &**fd else {
                    unreachable!()
                };
                fd.ready(*interest).map(KernelResult::Value)
            }
            Self::Accept(fd) => {
                h(fd)?;
                let Descriptor::Sim(fd) = &**fd else {
                    unreachable!()
                };
                fd.accept().map(KernelResult::Accepted)
            }
            Self::Connect { fd, address } => {
                h(fd)?;
                let Descriptor::Sim(fd) = &**fd else {
                    unreachable!()
                };
                fd.connect(address)?;
                Ok(KernelResult::Value(0))
            }
            Self::Open {
                dir,
                path: name,
                flags,
                resolve,
            } => sim
                .open_resolved(dir.as_deref(), &path(name), *flags, *resolve)
                .map(KernelResult::Accepted),
            Self::Stat { fd, ptr } => {
                h(fd)?;
                let Descriptor::Sim(fd) = &**fd else {
                    unreachable!()
                };
                unsafe {
                    **ptr = fd.stat()?;
                }
                Ok(KernelResult::Value(0))
            }
            Self::Sync(fd) => {
                h(fd)?;
                let Descriptor::Sim(fd) = &**fd else {
                    unreachable!()
                };
                fd.sync()?;
                Ok(KernelResult::Value(0))
            }
            Self::Mkdir { dir, name } => {
                h(dir)?;
                let path = sim.0.borrow().path(Some(dir), &path(name))?;
                if sim.0.borrow().paths.contains_key(&path) {
                    return Err(errno(libc::EEXIST));
                }
                sim.create_dir_all(&path)?;
                sim.chmod(&path, 0o700)?;
                Ok(KernelResult::Value(0))
            }
            Self::Rename { dir, from, to } => {
                h(dir)?;
                let a = sim.0.borrow().path(Some(dir), &path(from))?;
                let b = sim.0.borrow().path(Some(dir), &path(to))?;
                sim.rename(&a, &b, 0)?;
                Ok(KernelResult::Value(0))
            }
            Self::Unlink { dir, name } => {
                h(dir)?;
                let path = sim.0.borrow().path(Some(dir), &path(name))?;
                sim.unlink(&path)?;
                Ok(KernelResult::Value(0))
            }
        }
    }
}

struct Pending {
    op: Op,
    delay: usize,
    limit: usize,
    error: Option<i32>,
    disk_generation: u64,
    disk_paths: Vec<PathBuf>,
}
pub(super) struct Driver {
    sim: Simulation,
    pending: RefCell<BTreeMap<u64, Pending>>,
    completed: RefCell<VecDeque<(u64, KernelResult)>>,
}
impl Driver {
    pub fn new(sim: Simulation) -> Self {
        Self {
            sim,
            pending: RefCell::default(),
            completed: RefCell::default(),
        }
    }
    pub fn push(&mut self, id: u64, op: Op) {
        let name = op.name();
        let fault = self.sim.0.borrow_mut().fault(name);
        let mut pending = Pending {
            disk_generation: self.sim.0.borrow().disk.generation,
            disk_paths: op.disk_paths(&self.sim),
            op,
            delay: 0,
            limit: usize::MAX,
            error: None,
        };
        match fault {
            Some(Fault::Errno(n)) => pending.error = Some(n),
            Some(Fault::Short(n)) => pending.limit = n,
            Some(Fault::Delay(n)) => pending.delay = n,
            None => (),
        }
        self.sim
            .0
            .borrow_mut()
            .record(&format!("submit:{name}"), id, 0);
        self.pending.borrow_mut().insert(id, pending);
    }
    pub fn cancel(&mut self, id: u64) {
        let removed = self.pending.borrow_mut().remove(&id).is_some();
        let mut completed = self.completed.borrow_mut();
        // Deliberately emit cancel first, exercising the shared two-CQE fence.
        completed.push_back((
            id | CANCEL_BIT,
            KernelResult::Value(if removed { 0 } else { -libc::ENOENT }),
        ));
        if removed {
            completed.push_back((id, KernelResult::Value(-libc::ECANCELED)));
        }
        self.sim
            .0
            .borrow_mut()
            .record("cancel", id, i64::from(removed));
    }
    pub fn submit(&self) {
        let mut pending = self.pending.borrow_mut();
        let mut done = Vec::new();
        for (&id, operation) in pending.iter_mut() {
            if operation.delay != 0 {
                operation.delay -= 1;
                continue;
            }
            self.sim.0.borrow_mut().executing = true;
            let injected = operation.error.take();
            let result = match injected {
                Some(n) => Err(errno(n)),
                None if self
                    .sim
                    .0
                    .borrow()
                    .disk
                    .crashed_since(operation.disk_generation, &operation.disk_paths) =>
                {
                    Err(errno(libc::EIO))
                }
                None => operation.op.execute(&self.sim, operation.limit),
            };
            self.sim.0.borrow_mut().executing = false;
            match result {
                Err(error)
                    if injected.is_none()
                        && error.kind() == io::ErrorKind::WouldBlock
                        && matches!(
                            operation.op,
                            Op::Buffer {
                                operation: BufferOperation::Recv | BufferOperation::Send,
                                ..
                            } | Op::Poll { .. }
                                | Op::Accept(_)
                                | Op::Connect { .. }
                        ) =>
                {
                    ()
                }
                result => {
                    let result = result.unwrap_or_else(|error| {
                        KernelResult::Value(-error.raw_os_error().unwrap_or(libc::EIO))
                    });
                    let value = match &result {
                        KernelResult::Value(n) => *n as i64,
                        KernelResult::Accepted(Descriptor::Sim(h)) => h.id as i64,
                        _ => unreachable!(),
                    };
                    self.sim.0.borrow_mut().record(
                        &format!("complete:{}", operation.op.name()),
                        id,
                        value,
                    );
                    self.completed.borrow_mut().push_back((id, result));
                    done.push(id);
                }
            }
        }
        for id in done {
            pending.remove(&id);
        }
    }
    pub fn pop(&mut self) -> Option<(u64, KernelResult)> {
        self.completed.borrow_mut().pop_front()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        error::{Error, Operation, Result},
        model::{identity::RequestId, limits::ResourceClass},
        runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor},
    };
    use std::{
        task::{Context, Poll},
        time::{Duration, Instant},
    };

    pub(super) fn reactor() -> Reactor {
        Reactor::new(Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        )))
    }
    pub(super) fn scope() -> RequestScope {
        RequestScope::new(RequestId([4; 16]), Instant::now() + Duration::from_secs(30)).unwrap()
    }
    pub(super) fn poll<T>(op: &mut Operation<'_, T>) -> Poll<Result<T>> {
        op.as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    pub(super) fn drive<T>(r: &Reactor, mut op: Operation<'_, T>) -> Result<T> {
        for _ in 0..1000 {
            if let Poll::Ready(result) = poll(&mut op) {
                return result;
            }
            r.poll_budgeted(8)?;
            r.wait(Duration::ZERO)?;
        }
        panic!("simulation did not progress")
    }
    #[test]
    fn real_reactor_stream_backpressure_eof_and_completion_fences() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        assert!(r.state.borrow().ring.is_none());
        assert!(r.state.borrow().wake.is_none());
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let address = SocketAddress::Unix("/stream".into());
        let listener = Rc::new(sim.listen(address.clone()).unwrap());
        let client = Rc::new(Descriptor::socket(libc::AF_UNIX).unwrap());
        drive(&r, r.connect(client.clone(), address, &scope)).unwrap();
        let server = Rc::new(drive(&r, r.accept(listener.clone(), &scope)).unwrap());
        sim.set_stream_capacity(3);
        let sent = drive(
            &r,
            r.send(client.clone(), r.file_bytes(b"abcdef").unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 3);
        drop(sent);
        let mut writable = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
        assert!(poll(&mut writable).is_pending());
        r.poll_budgeted(8).unwrap();
        assert!(poll(&mut writable).is_pending());
        let read = drive(
            &r,
            r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 3);
        assert_eq!(read.buffer.prefix(3).unwrap(), b"abc");
        drop(read);
        assert_eq!(drive(&r, writable).unwrap(), libc::POLLOUT as u32);
        drop(client);
        assert_eq!(
            drive(
                &r,
                r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        drop((server, listener));
        assert_eq!(sim.live_handles(), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    }
    #[test]
    fn abandonment_retains_descriptor_buffer_and_lease_until_both_cqes() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        let scope = scope();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let (a, b) = sim.socket_pair();
        let a = Rc::new(a);
        let weak = Rc::downgrade(&a);
        let lease = r
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let mut recv = r.recv(a, r.file_buffer(17).unwrap(), lease, &scope);
        assert!(poll(&mut recv).is_pending());
        drop(recv);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        assert_eq!(r.in_flight(), 1);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_some());
        assert_eq!(r.admission.used(ResourceClass::Connection), 1);
        r.poll_budgeted(1).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::Connection), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        drop(b);
        assert_eq!(sim.live_handles(), 0);
    }
    #[test]
    fn sparse_files_partial_io_faults_rename_unlink_and_open_inode_ownership() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        sim.create_dir_all(Path::new("/data")).unwrap();
        let dir = drive(
            &r,
            r.file_open(
                None,
                CString::new("/data").unwrap(),
                libc::O_RDONLY | libc::O_DIRECTORY,
                0,
                &scope,
            ),
        )
        .unwrap();
        let file = drive(
            &r,
            r.file_open(
                Some(dir.clone()),
                CString::new("a").unwrap(),
                libc::O_CREAT | libc::O_RDWR | libc::O_EXCL,
                0,
                &scope,
            ),
        )
        .unwrap();
        sim.inject("write", Fault::Short(2));
        let write = drive(
            &r,
            r.write_at(
                file.clone(),
                1 << 30,
                r.file_bytes(b"abcdef").unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(write.bytes, 2);
        drop(write);
        let stat = drive(&r, r.file_stat(file.clone(), &scope)).unwrap();
        assert_eq!(stat.stx_size, (1 << 30) + 2);
        sim.inject("fsync", Fault::Errno(libc::EIO));
        assert_eq!(drive(&r, r.file_sync(file.clone(), &scope)), Err(Error::Io));
        drive(&r, r.file_sync(file.clone(), &scope)).unwrap();
        drive(
            &r,
            r.file_rename(
                dir.clone(),
                CString::new("a").unwrap(),
                CString::new("b").unwrap(),
                &scope,
            ),
        )
        .unwrap();
        drive(
            &r,
            r.file_unlink(dir.clone(), CString::new("b").unwrap(), &scope),
        )
        .unwrap();
        let read = drive(
            &r,
            r.read_at(
                file.clone(),
                (1 << 30) - 2,
                r.file_buffer(8).unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(read.bytes, 4);
        assert_eq!(read.buffer.prefix(4).unwrap(), b"\0\0ab");
        assert!(matches!(
            drive(
                &r,
                r.file_open(
                    Some(dir.clone()),
                    CString::new("b").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &scope
                )
            ),
            Err(Error::MissingKey)
        ));
        drop((read, file, dir));
        assert_eq!(sim.live_handles(), 0);
        assert!(
            sim.trace()
                .iter()
                .any(|e| e.operation == "complete:fsync" && e.result == -(libc::EIO as i64))
        );
    }
    #[test]
    fn scoped_selection_and_listener_pending_close_are_isolated() {
        let sim = Simulation::new();
        let other = Simulation::new();
        assert!(Simulation::current().is_none());
        {
            let _scope = sim.enter();
            {
                let _nested = other.enter();
                assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &other.0));
            }
            assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &sim.0));
        }
        assert!(Simulation::current().is_none());
        let address = SocketAddress::Inet("127.0.0.1:1234".parse().unwrap());
        let listener = sim.listen(address.clone()).unwrap();
        let client = sim.connect(address).unwrap();
        assert_eq!(sim.live_handles(), 3);
        drop(listener);
        assert_eq!(sim.live_handles(), 1);
        let Descriptor::Sim(client) = client else {
            unreachable!()
        };
        assert_eq!(
            client.send(b"x").unwrap_err().raw_os_error(),
            Some(libc::EPIPE)
        );
        drop(client);
        assert_eq!(sim.live_handles(), 0);
    }
    #[test]
    fn simulated_pipe_splice_preserves_suffix_under_backpressure() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let pool =
            crate::memory::pipe::PipePool::new(admission.clone(), Rc::new(Reactor::new(admission)));
        let mut pipe = pool.acquire().unwrap();
        let (a, b) = sim.socket_pair();
        sim.set_stream_capacity(2);
        pipe.try_write(b"abc").unwrap();
        assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 2);
        assert_eq!(pipe.buffered(), 1);
        assert_eq!(
            pipe.try_splice_descriptor(&a, 3).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        let Descriptor::Sim(b) = b else {
            unreachable!()
        };
        let mut bytes = [0; 2];
        b.recv(&mut bytes).unwrap();
        assert_eq!(&bytes, b"ab");
        assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 1);
        assert_eq!(b.recv(&mut bytes).unwrap(), 1);
        assert_eq!(bytes[0], b'c');
    }

    #[test]
    fn projected_directory_rotation_and_private_atomic_writes_use_real_filesystem_calls() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        sim.write_file(Path::new("/projected/epoch-a/bundle"), b"first")
            .unwrap();
        sim.symlink(Path::new("epoch-a"), Path::new("/projected/..data"))
            .unwrap();
        let bytes = drive(
            &r,
            Box::pin(crate::control::async_files::projected_file(
                &r,
                Path::new("/projected"),
                "bundle",
                64,
                &scope,
            )),
        )
        .unwrap();
        assert_eq!(&**bytes, b"first");
        let private = drive(
            &r,
            Box::pin(crate::control::async_files::directory(
                &r,
                Path::new("/private"),
                true,
                true,
                &scope,
            )),
        )
        .unwrap();
        sim.inject("write", Fault::Short(2));
        drive(
            &r,
            Box::pin(crate::control::async_files::atomic_write(
                &r, &private, "identity", b"secret", &scope,
            )),
        )
        .unwrap();
        assert_eq!(
            sim.read_file(Path::new("/private/identity")).unwrap(),
            b"secret"
        );
        sim.symlink(Path::new("/private"), Path::new("/projected/escape"))
            .unwrap();
        assert!(
            drive(
                &r,
                r.file_open(
                    None,
                    CString::new("/projected/escape").unwrap(),
                    libc::O_RDONLY,
                    4,
                    &scope
                )
            )
            .is_err()
        );
    }

    #[test]
    fn delayed_completion_and_fault_trace_replay_exactly() {
        fn run() -> Vec<Event> {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let r = reactor();
            let scope = scope();
            sim.write_file(Path::new("/file"), b"abc").unwrap();
            sim.inject("open", Fault::Delay(2));
            let fd = drive(
                &r,
                r.file_open(
                    None,
                    CString::new("/file").unwrap(),
                    libc::O_RDONLY,
                    0,
                    &scope,
                ),
            )
            .unwrap();
            sim.inject("read", Fault::Errno(libc::EIO));
            assert!(matches!(
                drive(&r, r.read_at(fd, 0, r.file_buffer(3).unwrap(), (), &scope)),
                Err(Error::Io)
            ));
            sim.trace()
        }
        assert_eq!(run(), run());
    }

    #[test]
    fn datagrams_preserve_packet_boundaries_and_source_addresses() {
        let sim = Simulation::new();
        let server_address = "127.0.0.1:53".parse().unwrap();
        let server = sim.bind_datagram(server_address).unwrap();
        let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
        let (Descriptor::Sim(server), Descriptor::Sim(client)) = (server, client) else {
            unreachable!()
        };
        client.connect_datagram(server_address).unwrap();
        client.send_datagram(b"query").unwrap();
        let mut bytes = [0; 32];
        let (count, source) = server.recv_from(&mut bytes).unwrap();
        assert_eq!(&bytes[..count], b"query");
        server.send_to(b"reply", source).unwrap();
        let (count, source) = client.recv_from(&mut bytes).unwrap();
        assert_eq!(source, server_address);
        assert_eq!(&bytes[..count], b"reply");
    }

    #[test]
    fn real_slab_open_is_sparse_exclusive_and_checks_direct_geometry() {
        use crate::{model::identity::WorkerId, store::slab::Slabs};
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = Rc::new(reactor());
        let slabs = Slabs::new(
            WorkerId(0),
            "/slabs".into(),
            r.clone(),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
        );
        assert!(slabs.open_now().is_ok());
        let other = Slabs::new(
            WorkerId(0),
            "/slabs".into(),
            r.clone(),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
        );
        assert_eq!(other.open_now(), Err(Error::Unavailable));
        // A failed second flock must not unlock the first file description.
        assert_eq!(other.open_now(), Err(Error::Unavailable));
        drop(slabs);
        assert!(other.open_now().is_ok());
        let path = Path::new("/slabs/worker-0-slab-0.dat");
        let file = Rc::new(sim.open(None, path, libc::O_RDWR | libc::O_DIRECT).unwrap());
        let scope = scope();
        assert!(matches!(
            drive(
                &r,
                r.write_at(file, 1, r.file_bytes(b"bad").unwrap(), (), &scope)
            ),
            Err(Error::Io)
        ));
        let w = sim.0.borrow();
        let node = w.paths[path].borrow();
        assert_eq!(node.length, 64 * 1024 * 1024);
        assert!(node.pages.is_empty());
    }
}
