//! The shared simulated operating system.
//!
//! A [`World`] holds every simulated resource: the filesystem namespace and
//! its durable image, sockets, listeners, datagram endpoints, pipes, network
//! partitions, the fault queue and the trace. It is a cheap, single-threaded
//! `Rc` handle; clones refer to the same world. Several
//! [`SimBackend`](crate::SimBackend)s (one per simulated worker or node) can
//! share one world, so nodes talk to each other through it.
//!
//! Open resources are referred to by [`SimFd`] handles. A resource is
//! identified by a small positive integer id, like a file descriptor number;
//! ids are allocated in increasing order and never reused, so a stale handle
//! can never alias a newer resource.

use std::cell::{RefCell, RefMut};
use std::collections::{BTreeMap, BTreeSet};
use std::fmt;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::rc::Rc;

use uring_runtime::{Error, Result, SocketAddress};

use crate::fault::{Fault, FaultQueue, FaultTarget, SyncOp};
use crate::fs::{File, Namespace};
use crate::net::{Datagram, Listener, NetTables, PipeEnd, Stream};
use crate::trace::{Event, EventKind, Trace};

/// Upper bound for one fixture allocation, a stream or pipe buffer, and the
/// allocated pages of one sparse file (64 MiB).
pub const MAX_ALLOCATION: usize = 64 * 1024 * 1024;

/// The default per-stream receive buffer capacity (64 KiB).
pub const DEFAULT_STREAM_CAPACITY: usize = 64 * 1024;

/// The maximum value accepted by [`World::reject_submissions`].
pub const MAX_REJECTIONS: usize = 16384;

pub(crate) fn errno(n: i32) -> Error {
    Error::from_errno(n)
}

/// Allocate `length` zeroed bytes, failing with `EFBIG` above
/// [`MAX_ALLOCATION`] or `ENOMEM` if the allocation fails.
pub(crate) fn bounded_bytes(length: usize) -> Result<Vec<u8>> {
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

/// Handle to a simulated operating system. See the [module docs](self).
#[derive(Clone)]
pub struct World(Rc<Shared>);

struct Shared {
    state: RefCell<State>,
    /// Resources whose last [`SimFd`] was dropped while `state` was borrowed.
    deferred_close: RefCell<Vec<u64>>,
}

/// Tunable behavior shared by every backend on a world.
#[derive(Debug)]
pub(crate) struct Knobs {
    pub(crate) stream_capacity: usize,
    pub(crate) max_chunk: usize,
    pub(crate) reject_submissions: usize,
    pub(crate) cancel_first: bool,
}

/// The kernel-side state of an open resource.
#[derive(Debug)]
pub(crate) enum Resource {
    Stream(Stream),
    Listener(Listener),
    Datagram(Datagram),
    File(File),
    Pipe(PipeEnd),
}

/// Everything behind a [`World`] handle.
#[derive(Debug)]
pub(crate) struct State {
    pub(crate) resources: BTreeMap<u64, Resource>,
    /// Resources returned as raw ids (accept, openat2) not yet adopted.
    pub(crate) unowned: BTreeSet<u64>,
    next_resource: u64,
    pub(crate) fs: Namespace,
    pub(crate) net: NetTables,
    pub(crate) faults: FaultQueue,
    pub(crate) trace: Trace,
    pub(crate) knobs: Knobs,
}

impl State {
    /// Register a resource and return its id.
    pub(crate) fn insert(&mut self, resource: Resource) -> Result<u64> {
        let id = self.next_resource;
        if id > i32::MAX as u64 {
            return Err(errno(libc::ENFILE));
        }
        self.next_resource += 1;
        self.resources.insert(id, resource);
        self.trace.record(EventKind::Create, id, 0);
        Ok(id)
    }

    pub(crate) fn record(&mut self, kind: EventKind, resource: u64, result: i64) {
        self.trace.record(kind, resource, result);
    }

    /// Consume the first fault matching `site`, recording it.
    pub(crate) fn fault(&mut self, site: FaultTarget) -> Option<Fault> {
        let fault = self.faults.take(site)?;
        let value = match fault {
            Fault::Errno(n) => -i64::from(n),
            Fault::Short(n) | Fault::Delay(n) | Fault::HoldCompletion(n) => n as i64,
        };
        self.trace.record(EventKind::Fault(site), 0, value);
        Some(fault)
    }

    /// Consume a synchronous fault: `Err` for [`Fault::Errno`], otherwise the
    /// byte limit ([`Fault::Short`] or unlimited).
    pub(crate) fn sync_limit(&mut self, op: SyncOp) -> Result<usize> {
        match self.fault(FaultTarget::Sync(op)) {
            Some(Fault::Errno(n)) => Err(errno(n)),
            Some(Fault::Short(n)) => Ok(n),
            _ => Ok(usize::MAX),
        }
    }

    /// Close resource `id`, releasing its locks, names and queued connections.
    pub(crate) fn close(&mut self, id: u64) {
        self.unowned.remove(&id);
        let Some(resource) = self.resources.remove(&id) else {
            return;
        };
        match resource {
            Resource::File(file) => file.release(),
            Resource::Listener(listener) => {
                self.net.unbind_listener(id, &listener);
                for pending in listener.pending {
                    if self.resources.remove(&pending).is_some() {
                        self.trace.record(EventKind::Close, pending, 0);
                    }
                }
            }
            Resource::Datagram(_) => self.net.unbind_datagram(id),
            Resource::Stream(stream) => self.net.unbind_stream(&stream),
            Resource::Pipe(end) => end.close(),
        }
        self.trace.record(EventKind::Close, id, 0);
    }
}

impl World {
    /// Create an empty world whose filesystem holds a durable root directory.
    pub fn new() -> Self {
        let world = Self(Rc::new(Shared {
            state: RefCell::new(State {
                resources: BTreeMap::new(),
                unowned: BTreeSet::new(),
                next_resource: 1,
                fs: Namespace::new(),
                net: NetTables::default(),
                faults: FaultQueue::default(),
                trace: Trace::default(),
                knobs: Knobs {
                    stream_capacity: DEFAULT_STREAM_CAPACITY,
                    max_chunk: usize::MAX,
                    reject_submissions: 0,
                    cancel_first: true,
                },
            }),
            deferred_close: RefCell::new(Vec::new()),
        }));
        world
            .disk()
            .sync_all()
            .expect("syncing an empty root cannot fail");
        world
    }

    /// Whether `self` and `other` are handles to the same world.
    pub fn ptr_eq(&self, other: &World) -> bool {
        Rc::ptr_eq(&self.0, &other.0)
    }

    /// Borrow the world state, first closing any resources whose handles
    /// were dropped while it was borrowed.
    pub(crate) fn state(&self) -> RefMut<'_, State> {
        let mut state = self.0.state.borrow_mut();
        let deferred = std::mem::take(&mut *self.0.deferred_close.borrow_mut());
        for id in deferred {
            state.close(id);
        }
        state
    }

    /// Wrap resource `id` (already registered and owned by nobody) in a handle.
    pub(crate) fn handle(&self, id: u64) -> SimFd {
        SimFd(Rc::new(Owner {
            world: self.clone(),
            id,
        }))
    }

    /// Check that `fd` belongs to this world, returning its id; `EXDEV`
    /// otherwise.
    pub(crate) fn id_of(&self, fd: &SimFd) -> Result<u64> {
        if fd.0.world.ptr_eq(self) {
            Ok(fd.0.id)
        } else {
            Err(errno(libc::EXDEV))
        }
    }

    /// Take ownership of a resource returned as a completion result by an
    /// `Accept` or `OpenAt2` operation (or by a synchronous raw call).
    ///
    /// Fails with `EBADF` unless `raw` names a live resource that has not
    /// been adopted yet. A result that is never adopted stays open (and
    /// counted by [`live_handles`](Self::live_handles)) until
    /// [`close_raw`](Self::close_raw) is called or the world is dropped.
    pub fn adopt(&self, raw: i32) -> Result<SimFd> {
        let id = u64::try_from(raw).map_err(|_| errno(libc::EBADF))?;
        if !self.state().unowned.remove(&id) {
            return Err(errno(libc::EBADF));
        }
        Ok(self.handle(id))
    }

    /// Close a resource returned as a raw completion result without adopting
    /// it. Fails with `EBADF` if `raw` is not an unadopted resource.
    pub fn close_raw(&self, raw: i32) -> Result<()> {
        drop(self.adopt(raw)?);
        Ok(())
    }

    /// Queue `fault` for the next operation matching `target`.
    ///
    /// Fails with `EINVAL` for an errno outside `1..=4095`, or `ENOSPC` when
    /// [`MAX_FAULTS`](crate::fault::MAX_FAULTS) faults are already queued.
    pub fn inject(&self, target: FaultTarget, fault: Fault) -> Result<()> {
        self.state().faults.push(target, fault)
    }

    /// The number of queued, unconsumed faults.
    pub fn pending_faults(&self) -> usize {
        self.state().faults.len()
    }

    /// Discard every queued fault.
    pub fn clear_faults(&self) {
        self.state().faults.clear();
    }

    /// Reject the next `count` calls to [`Backend::push`](uring_runtime::Backend::push)
    /// on any backend of this world with [`PushError::Full`](uring_runtime::backend::PushError::Full),
    /// before the backend takes the operation. Replaces any previous count.
    ///
    /// Fails with `EINVAL` above [`MAX_REJECTIONS`].
    pub fn reject_submissions(&self, count: usize) -> Result<()> {
        if count > MAX_REJECTIONS {
            return Err(errno(libc::EINVAL));
        }
        self.state().knobs.reject_submissions = count;
        Ok(())
    }

    /// Set the receive buffer capacity of every stream, in bytes
    /// (default [`DEFAULT_STREAM_CAPACITY`]). Fails with `EINVAL` for 0 or
    /// above [`MAX_ALLOCATION`].
    pub fn set_stream_capacity(&self, capacity: usize) -> Result<()> {
        if capacity == 0 || capacity > MAX_ALLOCATION {
            return Err(errno(libc::EINVAL));
        }
        self.state().knobs.stream_capacity = capacity;
        Ok(())
    }

    /// Cap every single file or stream transfer at `bytes` (default
    /// unlimited), producing short reads and writes. Fails with `EINVAL`
    /// for 0.
    pub fn set_max_chunk(&self, bytes: usize) -> Result<()> {
        if bytes == 0 {
            return Err(errno(libc::EINVAL));
        }
        self.state().knobs.max_chunk = bytes;
        Ok(())
    }

    /// Choose the completion order when a cancel removes its target: the
    /// cancel's own completion first (`true`, the default) or the target's
    /// `-ECANCELED` completion first. io_uring allows both.
    pub fn set_cancel_first(&self, cancel_first: bool) {
        self.state().knobs.cancel_first = cancel_first;
    }

    /// A copy of the retained trace.
    pub fn trace(&self) -> Vec<Event> {
        self.state().trace.snapshot()
    }

    /// Remove and return the retained trace. Sequence numbers continue.
    pub fn take_trace(&self) -> Vec<Event> {
        self.state().trace.take()
    }

    /// The number of open resources, including connections waiting to be
    /// accepted and unadopted completion results.
    pub fn live_handles(&self) -> usize {
        self.state().resources.len()
    }
}

impl Default for World {
    fn default() -> Self {
        Self::new()
    }
}

impl fmt::Debug for World {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.0.state.try_borrow() {
            Ok(state) => f
                .debug_struct("World")
                .field("live_handles", &state.resources.len())
                .field("paths", &state.fs.len())
                .finish_non_exhaustive(),
            Err(_) => f.write_str("World { <borrowed> }"),
        }
    }
}

/// A handle to an open simulated resource: the simulator's file descriptor.
///
/// `SimFd` is a cheap, reference-counted clone (like
/// [`uring_runtime::Fd`]); the resource is closed when the last clone is
/// dropped. Dropping never panics: if the world is busy at that moment, the
/// close is deferred to the world's next use.
///
/// Besides being the [`Backend::Fd`](uring_runtime::Backend::Fd) of
/// [`SimBackend`](crate::SimBackend), a `SimFd` offers synchronous fixture
/// methods (file, stream, pipe and datagram I/O) for test setup and
/// inspection; see the `fs` and `net` method groups.
#[derive(Clone)]
pub struct SimFd(Rc<Owner>);

struct Owner {
    world: World,
    id: u64,
}

impl Drop for Owner {
    fn drop(&mut self) {
        match self.world.0.state.try_borrow_mut() {
            Ok(mut state) => state.close(self.id),
            Err(_) => self.world.0.deferred_close.borrow_mut().push(self.id),
        }
    }
}

impl SimFd {
    /// The resource id: the simulated descriptor number.
    pub fn id(&self) -> u64 {
        self.0.id
    }

    /// The world this resource lives in.
    pub fn world(&self) -> &World {
        &self.0.world
    }

    /// Whether two handles are clones of each other.
    pub fn ptr_eq(&self, other: &SimFd) -> bool {
        Rc::ptr_eq(&self.0, &other.0)
    }

    /// The number of clones of this handle.
    pub fn handle_count(&self) -> usize {
        Rc::strong_count(&self.0)
    }

    pub(crate) fn state(&self) -> RefMut<'_, State> {
        self.0.world.state()
    }
}

impl fmt::Debug for SimFd {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_tuple("SimFd").field(&self.0.id).finish()
    }
}

/// A totally ordered key for a [`SocketAddress`], used for deterministic
/// tables.
#[derive(Clone, Debug, Eq, PartialEq, Ord, PartialOrd, Hash)]
pub(crate) enum AddressKey {
    Inet(SocketAddr),
    Unix(PathBuf),
}

impl AddressKey {
    pub(crate) fn new(address: &SocketAddress) -> Self {
        match address {
            SocketAddress::Unix(path) => Self::Unix(path.clone()),
            other => Self::Inet(other.as_inet().expect("Inet or Unix")),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn knobs_validate_atomically() {
        let world = World::new();
        world.set_stream_capacity(MAX_ALLOCATION).unwrap();
        for invalid in [0, MAX_ALLOCATION + 1, usize::MAX] {
            let error = world.set_stream_capacity(invalid).unwrap_err();
            assert_eq!(error.raw_os_error(), Some(libc::EINVAL));
            assert_eq!(world.state().knobs.stream_capacity, MAX_ALLOCATION);
        }
        world.set_max_chunk(usize::MAX).unwrap();
        assert_eq!(
            world.set_max_chunk(0).unwrap_err().raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(world.state().knobs.max_chunk, usize::MAX);
        world.reject_submissions(MAX_REJECTIONS).unwrap();
        assert_eq!(
            world
                .reject_submissions(MAX_REJECTIONS + 1)
                .unwrap_err()
                .raw_os_error(),
            Some(libc::EINVAL)
        );
        assert_eq!(world.state().knobs.reject_submissions, MAX_REJECTIONS);
        world.set_cancel_first(false);
        assert!(!world.state().knobs.cancel_first);
        world.set_cancel_first(true);
        assert!(world.state().knobs.cancel_first);
        assert!(format!("{world:?}").contains("live_handles: 0"));
    }

    #[test]
    fn handles_close_on_last_drop_and_defer_while_borrowed() {
        let world = World::new();
        let (a, b) = world.socket_pair().unwrap();
        assert_eq!(world.live_handles(), 2);
        let clone = a.clone();
        assert_eq!(a.handle_count(), 2);
        assert!(a.ptr_eq(&clone));
        drop(a);
        assert_eq!(world.live_handles(), 2);
        {
            let _busy = world.0.state.borrow_mut();
            drop(clone);
            drop(b);
        }
        assert_eq!(world.live_handles(), 0);
        let events = world.trace();
        assert_eq!(
            events
                .iter()
                .filter(|event| event.kind == EventKind::Close)
                .count(),
            2
        );
    }

    #[test]
    fn foreign_handles_are_rejected_and_raw_results_adopt_once() {
        let world = World::new();
        let other = World::new();
        let (local, _peer) = world.socket_pair().unwrap();
        let (foreign, _foreign_peer) = other.socket_pair().unwrap();
        assert_eq!(local.id(), foreign.id());
        assert!(!local.world().ptr_eq(foreign.world()));
        assert_eq!(
            world.id_of(&foreign).unwrap_err().raw_os_error(),
            Some(libc::EXDEV)
        );
        assert_eq!(
            world.disconnect(&foreign).unwrap_err().raw_os_error(),
            Some(libc::EXDEV)
        );
        assert_eq!(
            world.adopt(-1).unwrap_err().raw_os_error(),
            Some(libc::EBADF)
        );
        assert_eq!(
            world.adopt(local.id() as i32).unwrap_err().raw_os_error(),
            Some(libc::EBADF),
            "owned resources cannot be adopted twice"
        );
        assert_eq!(format!("{local:?}"), format!("SimFd({})", local.id()));
    }
}
