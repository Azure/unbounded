//! Racer's ambient deterministic simulation (feature `simulation`).
//!
//! [`Simulation`] wraps a [`uring_runtime_sim::World`] and keeps the shape of
//! the legacy `uring_runtime::reactor::simulation::Simulation`: a thread may
//! [`enter`](Simulation::enter) one, and reactors created while it is
//! current run on a [`SimBackend`] over its world, while
//! [`Descriptor::socket`] and [`Descriptor::tcp_listener`] create simulated
//! resources. Fixture helpers return Racer [`Descriptor`]s and
//! [`std::io::Result`]s like the legacy API.
//!
//! The backend's clock is the current environment's [`SimClock`] (from
//! [`SimulationClock::environment`](crate::environment::SimulationClock::environment))
//! when one is entered, otherwise this simulation's own
//! [`clock`](Simulation::clock), which nothing advances unless the caller
//! does.
//!
//! # Migration from the legacy runtime
//!
//! | Legacy (`uring_runtime::reactor::simulation`) | Here |
//! |---|---|
//! | `Simulation::{new, enter, current, disk}` | same |
//! | `Environment` (guard returned by `enter`) | [`Environment`] |
//! | `enter_endpoint(addr) -> EndpointEnvironment` | same; read when a socket is created, by fixtures, [`Descriptor::socket`] and the reactor backend alike |
//! | `partition(a, b)`, `heal(a, b)` | same (by value) |
//! | `label_stream(&Descriptor, local, remote)`, `disconnect(&Descriptor)` | same (`EXDEV` for foreign or host descriptors) |
//! | `inject(&str, Fault)` | same names, mapped by [`fault_target`]; unknown names now fail with `EINVAL`. Typed form: [`inject_at`](Simulation::inject_at) |
//! | `Fault::{Errno, Short, Delay, HoldCompletion}` | same ([`Fault`]) |
//! | `trace()`/`take_trace() -> Vec<Event { operation: String, .. }>` | `Vec<`[`Event`]`>` with a typed [`EventKind`] instead of `operation` |
//! | `live_handles`, `set_stream_capacity`, `set_cancel_first`, `set_max_chunk` | same |
//! | `reject_submissions(n)` | same meaning (the next `n` operations are rejected); doubled into backend push rejections because the library retries a full push once |
//! | `next_sequence` | same, but a separate per-world counter (no longer shared with resource ids) |
//! | `create_dir_all`, `mkdir`, `symlink`, `write_file`, `read_file`, `chmod`, `rename`, `unlink` | same |
//! | `metadata(path) -> (inode, mode)` | same; full [`Metadata`] through [`world`](Simulation::world) |
//! | `open(dir, path, flags)`, `open_resolved(dir, path, flags, resolve)` | same |
//! | `socket(domain)`, `listen(addr)`, `connect(addr)`, `bind_datagram`, `socket_pair()`, `pipe(cap)`, `try_pipe(cap)` | same |
//! | `CrashDisk` (`disk()`) | [`Disk`] (returns [`uring::Result`]) |
//! | `Descriptor::as_sim() -> Option<&Handle>`, `into_sim()` | [`Descriptor::as_sim`] `-> Option<&SimFd>`, [`Descriptor::into_sim`] |
//! | `Handle::{file_read, file_write}` | [`SimFd::read_at`], [`SimFd::write_at`] |
//! | `Handle::{recv, ready, shutdown(i32)}` | [`SimFd::recv`]`(buf, peek)`, [`SimFd::poll`], [`SimFd::shutdown`]`(Shutdown)` |
//! | `Handle::{send, accept, connect, stat, set_len, lock, sync, pipe_read, pipe_write, splice, send_to, recv_from}` | same names on [`SimFd`] (`accept` returns a [`SimFd`]) |
//! | `Handle::{idle_healthy, peer_disconnected, peer_read_closed, id}` | same on [`SimFd`] |
//! | `Handle::validate_socket` | [`SimFd::socket_type`] |
//!
//! Everything else is on the library [`World`] ([`Simulation::world`]):
//! `set_owner`, `set_link_count`, `set_umask`, `pending_faults`, ...
use crate::reactor::{SocketAddress, descriptor::Descriptor};
use std::{cell::RefCell, io, marker::PhantomData, path::Path, rc::Rc};
use uring::backend::OpKind;

pub use uring_runtime_sim as sim;
pub use uring_runtime_sim::{
    DEFAULT_ENTROPY_DOMAIN, Disk, DiskState, Event, EventKind, Fault, FaultTarget, Metadata,
    SimBackend, SimClock, SimEntropy, SimFd, SyncOp, World,
};

thread_local! {
    static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) };
}

/// Shared deterministic OS state retained by reactors and simulated descriptors.
/// Cheap to clone; clones share the world.
#[derive(Clone, Debug)]
pub struct Simulation(Rc<Shared>);

#[derive(Debug)]
struct Shared {
    world: World,

    clock: SimClock,

    endpoint: RefCell<Option<SocketAddress>>,

    sequence: std::cell::Cell<u64>,
}

/// Restores the previous thread-local simulation when dropped.
pub struct Environment {
    previous: Option<Simulation>,

    _local: PhantomData<Rc<()>>,
}

impl Drop for Environment {
    /// Restore the previous thread-local selection.
    fn drop(&mut self) {
        CURRENT.with(|current| *current.borrow_mut() = self.previous.take());
    }
}

/// Labels new sockets with a node's listening endpoint until dropped.
pub struct EndpointEnvironment {
    simulation: Simulation,

    previous: Option<SocketAddress>,
}

impl Drop for EndpointEnvironment {
    /// Restore the previous label without changing existing sockets.
    fn drop(&mut self) {
        *self.simulation.0.endpoint.borrow_mut() = self.previous.take();
    }
}

/// Map a legacy fault name to a typed target.
///
/// Names shared by reactor operations and synchronous calls (`read`,
/// `write`, `recv`, `send`, `accept`, `connect`, `open`, `fsync`, `mkdir`,
/// `rename`, `unlink`) match whichever comes first, as before. `poll`,
/// `stat`, `timeout` and `nop` are reactor-only; `chmod`, `pipe_read`,
/// `pipe_write`, `splice`, `send_datagram` and `recv_datagram` are
/// synchronous only; `*` matches anything.
pub fn fault_target(name: &str) -> io::Result<FaultTarget> {
    use FaultTarget::{Any, Either, Op, Sync};
    Ok(match name {
        "*" => Any,
        "read" => Either(OpKind::Read, SyncOp::Read),
        "write" => Either(OpKind::Write, SyncOp::Write),
        "recv" => Either(OpKind::Recv, SyncOp::Recv),
        "send" => Either(OpKind::Send, SyncOp::Send),
        "accept" => Either(OpKind::Accept, SyncOp::Accept),
        "connect" => Either(OpKind::Connect, SyncOp::Connect),
        "open" => Either(OpKind::OpenAt2, SyncOp::Open),
        "fsync" => Either(OpKind::Fsync, SyncOp::Fsync),
        "mkdir" => Either(OpKind::MkDirAt, SyncOp::Mkdir),
        "rename" => Either(OpKind::RenameAt, SyncOp::Rename),
        "unlink" => Either(OpKind::UnlinkAt, SyncOp::Unlink),
        "poll" => Op(OpKind::PollAdd),
        "stat" => Op(OpKind::Statx),
        "timeout" => Op(OpKind::Timeout),
        "nop" => Op(OpKind::Nop),
        "chmod" => Sync(SyncOp::Chmod),
        "pipe_read" => Sync(SyncOp::PipeRead),
        "pipe_write" => Sync(SyncOp::PipeWrite),
        "splice" => Sync(SyncOp::Splice),
        "send_datagram" => Sync(SyncOp::SendTo),
        "recv_datagram" => Sync(SyncOp::RecvFrom),
        _ => return Err(io::Error::from_raw_os_error(libc::EINVAL)),
    })
}

/// Convert like the legacy simulator: an errno always survives, even when
/// the library attached context.
fn io<T>(result: uring::Result<T>) -> io::Result<T> {
    result.map_err(io_error)
}

/// See [`io`].
pub(crate) fn io_error(error: uring::Error) -> io::Error {
    match error.raw_os_error() {
        Some(errno) => io::Error::from_raw_os_error(errno),
        None => io::Error::from(error),
    }
}

impl Simulation {
    /// Create an empty world with a durable root directory.
    pub fn new() -> Self {
        Self::with_clock(SimClock::new(0))
    }

    /// Create an empty world whose backends fall back to `clock` when no
    /// simulated environment is entered.
    ///
    /// The world reports the process's effective UID and GID as every
    /// inode's owner, so Racer's private-access checks behave as on the host
    /// (the legacy simulator reported owner 0 and checked against 0).
    pub fn with_clock(clock: SimClock) -> Self {
        let world = World::new();
        // SAFETY: geteuid and getegid have no preconditions.
        world.set_owner(unsafe { libc::geteuid() }, unsafe { libc::getegid() });
        Self(Rc::new(Shared {
            world,
            clock,
            endpoint: RefCell::new(None),
            sequence: std::cell::Cell::new(1),
        }))
    }

    /// Select this simulation for the current thread until the guard drops.
    /// Reactors and descriptors keep the simulation they were created in.
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|current| current.borrow_mut().replace(self.clone())),
            _local: PhantomData,
        }
    }

    /// The simulation selected for the current thread.
    pub fn current() -> Option<Self> {
        CURRENT.with(|current| current.borrow().clone())
    }

    /// The library world.
    pub fn world(&self) -> &World {
        &self.0.world
    }

    /// The fallback clock for backends built outside a simulated environment.
    pub fn clock(&self) -> &SimClock {
        &self.0.clock
    }

    /// Whether `self` and `other` share one world.
    pub fn ptr_eq(&self, other: &Simulation) -> bool {
        Rc::ptr_eq(&self.0, &other.0)
    }

    /// Allocate the next number of this world's deterministic sequence
    /// (starting at 1), for names that must replay identically.
    pub fn next_sequence(&self) -> u64 {
        let next = self.0.sequence.get();
        self.0.sequence.set(next + 1);
        next
    }

    /// The volatile and durable storage images.
    pub fn disk(&self) -> Disk {
        self.0.world.disk()
    }

    /// Label sockets created in this scope with a node's listening endpoint.
    pub fn enter_endpoint(&self, endpoint: SocketAddress) -> EndpointEnvironment {
        let previous = self.0.endpoint.borrow_mut().replace(endpoint);
        EndpointEnvironment {
            simulation: self.clone(),
            previous,
        }
    }

    /// The endpoint label for sockets created now.
    pub fn endpoint(&self) -> Option<SocketAddress> {
        self.0.endpoint.borrow().clone()
    }

    /// Symmetric blackhole between two endpoint labels until [`heal`](Self::heal).
    pub fn partition(&self, a: SocketAddress, b: SocketAddress) {
        self.0.world.partition(&a, &b);
    }

    /// Remove a symmetric partition without discarding queued bytes.
    pub fn heal(&self, a: SocketAddress, b: SocketAddress) {
        self.0.world.heal(&a, &b);
    }

    /// Borrow a descriptor's simulated handle if it belongs to this world.
    pub fn handle<'a>(&self, fd: &'a Descriptor) -> io::Result<&'a SimFd> {
        fd.as_sim()
            .filter(|handle| handle.world().ptr_eq(&self.0.world))
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EXDEV))
    }

    /// Label a stream and its peer (for streams created outside a node scope).
    pub fn label_stream(
        &self,
        fd: &Descriptor,
        local: SocketAddress,
        remote: SocketAddress,
    ) -> io::Result<()> {
        io(self.0.world.label_stream(self.handle(fd)?, local, remote))
    }

    /// Break an established stream while both handles stay open.
    pub fn disconnect(&self, fd: &Descriptor) -> io::Result<()> {
        io(self.0.world.disconnect(self.handle(fd)?))
    }

    /// Queue a fault for the next operation with this legacy name; see
    /// [`fault_target`].
    pub fn inject(&self, operation: &str, fault: Fault) -> io::Result<()> {
        self.inject_at(fault_target(operation)?, fault)
    }

    /// Queue a fault for the next operation matching `target`.
    pub fn inject_at(&self, target: FaultTarget, fault: Fault) -> io::Result<()> {
        io(self.0.world.inject(target, fault))
    }

    /// Faults not yet consumed.
    pub fn pending_faults(&self) -> usize {
        self.0.world.pending_faults()
    }

    /// Copy the retained trace.
    pub fn trace(&self) -> Vec<Event> {
        self.0.world.trace()
    }

    /// Drain the retained trace without resetting sequence numbers.
    pub fn take_trace(&self) -> Vec<Event> {
        self.0.world.take_trace()
    }

    /// Count open resources, including connections waiting to be accepted.
    pub fn live_handles(&self) -> usize {
        self.0.world.live_handles()
    }

    /// Reject the publication of the next `count` reactor operations.
    ///
    /// The library reactor retries a push that reports a full queue once
    /// after submitting, so each rejected operation consumes two backend
    /// rejections; this doubles `count` (at most `MAX_REJECTIONS / 2`). Use
    /// [`World::reject_submissions`] through [`world`](Self::world) to count
    /// raw pushes instead.
    pub fn reject_submissions(&self, count: usize) -> io::Result<()> {
        let pushes = count
            .checked_mul(2)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EINVAL))?;
        io(self.0.world.reject_submissions(pushes))
    }

    /// Set the receive queue capacity of every stream.
    pub fn set_stream_capacity(&self, capacity: usize) -> io::Result<()> {
        io(self.0.world.set_stream_capacity(capacity))
    }

    /// Select cancellation completion ordering.
    pub fn set_cancel_first(&self, cancel_first: bool) {
        self.0.world.set_cancel_first(cancel_first);
    }

    /// Cap each stream or file transfer.
    pub fn set_max_chunk(&self, bytes: usize) -> io::Result<()> {
        io(self.0.world.set_max_chunk(bytes))
    }

    /// Create a directory and its missing ancestors.
    pub fn create_dir_all(&self, path: &Path) -> io::Result<()> {
        io(self.0.world.create_dir_all(path))
    }

    /// Create one directory.
    pub fn mkdir(&self, path: &Path) -> io::Result<()> {
        io(self.0.world.mkdir(path))
    }

    /// Create a symbolic link at `path` pointing to `target`.
    pub fn symlink(&self, target: &Path, path: &Path) -> io::Result<()> {
        io(self.0.world.symlink(target, path))
    }

    /// Open with ordinary resolution.
    pub fn open(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
    ) -> io::Result<Descriptor> {
        self.open_resolved(dir, path, flags, 0)
    }

    /// `openat2` with `RESOLVE_*` flags.
    pub fn open_resolved(
        &self,
        dir: Option<&Descriptor>,
        path: &Path,
        flags: i32,
        resolve: u64,
    ) -> io::Result<Descriptor> {
        let dir = dir.map(|dir| self.handle(dir)).transpose()?;
        io(self.0.world.open_at(dir, path, flags, resolve)).map(Descriptor::from)
    }

    /// Replace a file's contents, creating missing ancestors. Not synced.
    pub fn write_file(&self, path: &Path, bytes: &[u8]) -> io::Result<()> {
        io(self.0.world.write_file(path, bytes))
    }

    /// Read a whole file.
    pub fn read_file(&self, path: &Path) -> io::Result<Vec<u8>> {
        io(self.0.world.read_file(path))
    }

    /// A named inode's `(inode, mode)`, without following a final symlink.
    pub fn metadata(&self, path: &Path) -> io::Result<(u64, u16)> {
        let metadata = io(self.0.world.metadata(path))?;
        Ok((metadata.inode, metadata.mode as u16))
    }

    /// Change permission bits.
    pub fn chmod(&self, path: &Path, mode: u32) -> io::Result<()> {
        io(self.0.world.chmod(path, mode))
    }

    /// `renameat2` with `RENAME_*` flags.
    pub fn rename(&self, from: &Path, to: &Path, flags: u32) -> io::Result<()> {
        io(self.0.world.rename(from, to, flags))
    }

    /// Remove a non-directory.
    pub fn unlink(&self, path: &Path) -> io::Result<()> {
        io(self.0.world.unlink(path))
    }

    /// Create a socket labeled with the current endpoint.
    pub fn socket(&self, domain: i32) -> io::Result<Descriptor> {
        let endpoint = self.endpoint();
        io(self
            .0
            .world
            .labeled_socket(domain, libc::SOCK_STREAM, endpoint.as_ref()))
        .map(Descriptor::from)
    }

    /// Bind an Internet datagram socket.
    pub fn bind_datagram(&self, address: std::net::SocketAddr) -> io::Result<Descriptor> {
        io(self.0.world.bind_datagram(address)).map(Descriptor::from)
    }

    /// Create a listener labeled with the current endpoint.
    pub fn listen(&self, address: SocketAddress) -> io::Result<Descriptor> {
        let endpoint = self.endpoint();
        io(self.0.world.labeled_listen(&address, endpoint.as_ref())).map(Descriptor::from)
    }

    /// Connect a stream labeled with the current endpoint, without waiting.
    pub fn connect(&self, address: SocketAddress) -> io::Result<Descriptor> {
        let endpoint = self.endpoint();
        io(self.0.world.labeled_connect(&address, endpoint.as_ref())).map(Descriptor::from)
    }

    /// Two connected, unlabeled Unix streams.
    ///
    /// # Panics
    ///
    /// Panics if a queued `Socket` fault fails the creation.
    pub fn socket_pair(&self) -> (Descriptor, Descriptor) {
        let (a, b) = self.0.world.socket_pair().expect("simulated socket pair");
        (a.into(), b.into())
    }

    /// A pipe, `(read end, write end)`.
    ///
    /// # Panics
    ///
    /// Panics for an invalid capacity; see [`try_pipe`](Self::try_pipe).
    pub fn pipe(&self, capacity: usize) -> (Descriptor, Descriptor) {
        self.try_pipe(capacity).expect("simulated pipe")
    }

    /// A pipe, `(read end, write end)`, or `EINVAL` for an invalid capacity.
    pub fn try_pipe(&self, capacity: usize) -> io::Result<(Descriptor, Descriptor)> {
        let (read, write) = io(self.0.world.pipe(capacity))?;
        Ok((read.into(), write.into()))
    }
}

impl Default for Simulation {
    /// Create an isolated world with a durable root directory.
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn code<T: std::fmt::Debug>(result: io::Result<T>) -> i32 {
        result.unwrap_err().raw_os_error().unwrap()
    }

    #[test]
    fn enter_nests_and_restores() {
        assert!(Simulation::current().is_none());
        let a = Simulation::new();
        let b = Simulation::new();
        {
            let _a = a.enter();
            {
                let _b = b.enter();
                assert!(Simulation::current().unwrap().ptr_eq(&b));
            }
            assert!(Simulation::current().unwrap().ptr_eq(&a));
        }
        assert!(Simulation::current().is_none());
    }

    #[test]
    fn named_faults_reach_reactor_and_synchronous_sites() {
        let sim = Simulation::new();
        assert_eq!(code(sim.inject("bogus", Fault::Short(1))), libc::EINVAL);
        assert_eq!(code(sim.inject("read", Fault::Errno(0))), libc::EINVAL);
        let (left, right) = sim.socket_pair();
        sim.inject("send", Fault::Short(1)).unwrap();
        assert_eq!(left.try_send(b"abc").unwrap(), 1);
        sim.inject("recv", Fault::Errno(libc::EINTR)).unwrap();
        assert_eq!(
            right.try_recv(&mut [0; 4]).unwrap_err().raw_os_error(),
            Some(libc::EINTR)
        );
        assert_eq!(sim.pending_faults(), 0);
        for name in [
            "*",
            "read",
            "write",
            "accept",
            "connect",
            "open",
            "fsync",
            "mkdir",
            "rename",
            "unlink",
            "poll",
            "stat",
            "timeout",
            "nop",
            "chmod",
            "pipe_read",
            "pipe_write",
            "splice",
            "send_datagram",
            "recv_datagram",
        ] {
            fault_target(name).unwrap();
        }
    }

    /// Reject foreign and host descriptors even when their resource ids collide.
    #[test]
    fn stream_mutation_checks_world_provenance() {
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

    /// Endpoint scopes label new sockets so partitions apply to them.
    #[test]
    fn endpoint_scope_labels_sockets_created_inside_it() {
        let sim = Simulation::new();
        let a = SocketAddress::Inet("127.0.0.1:201".parse().unwrap());
        let b = SocketAddress::Inet("127.0.0.1:202".parse().unwrap());
        let listener = {
            let _b = sim.enter_endpoint(b.clone());
            sim.listen(b.clone()).unwrap()
        };
        assert_eq!(sim.endpoint(), None);
        sim.partition(a.clone(), b.clone());
        {
            let _a = sim.enter_endpoint(a.clone());
            assert_eq!(code(sim.connect(b.clone())), libc::EAGAIN);
        }
        // Unlabeled sockets are never partitioned.
        let client = sim.connect(b.clone()).unwrap();
        assert!(listener.try_accept().is_ok());
        drop(client);
        sim.heal(a.clone(), b.clone());
        let _a = sim.enter_endpoint(a);
        let _guard = sim.enter();
        let socket = Descriptor::socket(libc::AF_INET).unwrap();
        assert!(socket.as_sim().is_some());
    }

    #[test]
    fn filesystem_fixtures_keep_legacy_shapes() {
        let sim = Simulation::new();
        sim.write_file(Path::new("/root/child/f"), b"abc").unwrap();
        let (inode, mode) = sim.metadata(Path::new("/root/child/f")).unwrap();
        assert!(inode > 0);
        assert_eq!(mode as u32 & libc::S_IFMT, libc::S_IFREG);
        sim.chmod(Path::new("/root/child/f"), 0o600).unwrap();
        assert_eq!(
            sim.metadata(Path::new("/root/child/f")).unwrap().1 & 0o777,
            0o600
        );
        let dir = sim
            .open(None, Path::new("/root"), libc::O_DIRECTORY)
            .unwrap();
        let file = sim
            .open_resolved(Some(&dir), Path::new("child/f"), libc::O_RDONLY, 0x08)
            .unwrap();
        let mut bytes = [0; 3];
        assert_eq!(file.as_sim().unwrap().read_at(0, &mut bytes).unwrap(), 3);
        sim.symlink(Path::new("/root/child/f"), Path::new("/link"))
            .unwrap();
        sim.rename(Path::new("/link"), Path::new("/link2"), 0)
            .unwrap();
        sim.unlink(Path::new("/link2")).unwrap();
        sim.mkdir(Path::new("/one")).unwrap();
        assert_eq!(code(sim.mkdir(Path::new("/one"))), libc::EEXIST);
        sim.create_dir_all(Path::new("/a/b")).unwrap();
        sim.disk().sync_all().unwrap();
        assert_eq!(sim.read_file(Path::new("/root/child/f")).unwrap(), b"abc");
        let (read, write) = sim.pipe(16);
        assert_eq!(write.as_sim().unwrap().pipe_write(b"x").unwrap(), 1);
        assert_eq!(read.as_sim().unwrap().pipe_read(&mut [0; 1]).unwrap(), 1);
        assert_eq!(code(sim.try_pipe(0)), libc::EINVAL);
        assert!(sim.bind_datagram("127.0.0.1:0".parse().unwrap()).is_ok());
        assert!(sim.live_handles() > 0);
        assert!(!sim.trace().is_empty());
        assert!(!sim.take_trace().is_empty());
        sim.reject_submissions(1).unwrap();
        sim.set_stream_capacity(8).unwrap();
        sim.set_max_chunk(4).unwrap();
        sim.set_cancel_first(false);
    }
}
