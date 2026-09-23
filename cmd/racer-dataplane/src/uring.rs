// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Ownership-safe, worker-local io_uring for Linux 6.1+, using only libc.
//!
//! Construct [`Ring::new`] in the pinned [`crate::workers::Workers::start`]
//! factory with its placement and NUMA-local [`WorkerPool`]. [`Driver`] combines
//! the ring with application and external completion sources in one sleep loop.
//! Pool registration requires sufficient RLIMIT_MEMLOCK. No SQPOLL thread is used.
//!
//! Submissions consume their storage; tickets never own in-flight storage.
//! Dropping or forgetting a ticket cannot recycle a kernel-accessible buffer.
//! Short transfers are returned unchanged. Reads return a Fill: the application
//! must finish/validate the value before publishing it. SEND_ZC returns ownership
//! only after its notification (when MORE was set), even on failure. Its early
//! byte-count result is available through [`Ring::send_result`]. RECV on Linux
//! 6.1 copies into pool memory; it does not implement NIC receive zero-copy.
//!
//! Cancellation acknowledgments are not target completions. Shutdown drains real
//! terminal CQEs before unregistering memory. On failed or panicking teardown,
//! unresolved resources are deliberately leaked: close(uring_fd) is asynchronous
//! and does not prove quiescence. The ring must not be used across fork.

use crate::buffers::{
    BUFFER_SIZE, Buffer, BufferRegion, Fill, MemoryLease, WorkerPool, Writable, WritableStorage,
};
#[cfg(test)]
use crate::simulation::SimRing;
use crate::uring_sys::KernelRing;
pub(crate) use crate::uring_sys::abi;
use crate::workers::{self, WorkerContext};
use std::cell::RefCell;
use std::collections::VecDeque;
use std::io;
use std::marker::PhantomData;
use std::net::SocketAddr;
use std::ops::Range;
use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd};
use std::rc::Rc;
use std::sync::Arc;
#[cfg(test)]
use std::sync::atomic::Ordering;
use std::time::{Duration, Instant};

// One inbound budget per worker ring, shared by every HTTP listener/generation.
use std::cell::Cell;

// Always preserve outbound registration capacity, even when the SQ reserve is
// disabled. Tiny tables reserve a quarter rounded up; a one-file table cannot
// support inbound plus cold upstream and therefore admits no inbound work.
fn inbound_reserve(files: u32) -> usize {
    files.div_ceil(4).min(8) as usize
}

pub(crate) struct Inbound(Rc<Cell<usize>>);
impl Drop for Inbound {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}

impl Ring {
    pub(crate) fn admit_inbound(&self) -> Option<Rc<Inbound>> {
        let limit = (self.config.fixed_files as usize - inbound_reserve(self.config.fixed_files))
            .min(self.config.requests as usize / 2);
        if self.stopping || self.inbound.get() >= limit {
            return None;
        }
        self.inbound.set(self.inbound.get() + 1);
        Some(Rc::new(Inbound(self.inbound.clone())))
    }

    pub(crate) fn register_inbound(
        &mut self,
        file: File,
        admission: Rc<Inbound>,
    ) -> io::Result<FixedFile> {
        self.register_file_inner(file, Some(admission))
    }
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}
// Raw submission backend, below the production
// ownership table, validation, abandonment and CQE lifecycle state machine.
enum RawRing {
    Kernel(KernelRing),
    #[cfg(test)]
    Sim(SimRing),
}
impl RawRing {
    fn new(entries: u32) -> io::Result<Self> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            return Ok(Self::Sim(SimRing::new(world, entries)));
        }
        Ok(Self::Kernel(KernelRing::new(entries)?))
    }
    fn register(&self, op: u32, arg: *const libc::c_void, count: u32) -> io::Result<()> {
        match self {
            Self::Kernel(k) => k.register(op, arg, count),
            #[cfg(test)]
            // SAFETY: private callers supply the matching live ABI argument.
            Self::Sim(s) => unsafe { s.register(op, arg, count) },
        }
    }
    fn space(&self) -> u32 {
        match self {
            Self::Kernel(k) => k.space(),
            #[cfg(test)]
            Self::Sim(s) => s.entries.saturating_sub(s.submissions() as u32),
        }
    }
    fn pending(&self) -> u32 {
        match self {
            Self::Kernel(k) => k.pending(),
            #[cfg(test)]
            Self::Sim(s) => s.submissions() as u32,
        }
    }
    fn push(&mut self, sqe: abi::Sqe) {
        match self {
            Self::Kernel(k) => k.push(sqe),
            #[cfg(test)]
            Self::Sim(s) => s.push(sqe),
        }
    }
    fn discard_unsubmitted(&mut self, id: u64) -> bool {
        match self {
            Self::Kernel(k) => k.discard_unsubmitted(id),
            #[cfg(test)]
            Self::Sim(s) => s.discard_unsubmitted(id),
        }
    }
    fn ready(&self) -> bool {
        match self {
            Self::Kernel(k) => k.ready(),
            #[cfg(test)]
            Self::Sim(s) => !s.cq.is_empty(),
        }
    }
    fn needs_enter(&self) -> bool {
        match self {
            Self::Kernel(k) => k.needs_enter(),
            #[cfg(test)]
            Self::Sim(s) => s.submissions() != 0,
        }
    }
    fn enter(&mut self, wait: bool, timeout: Option<Duration>) -> io::Result<()> {
        match self {
            Self::Kernel(k) => k.enter(wait, timeout),
            #[cfg(test)]
            Self::Sim(s) => s.submit(wait),
        }
    }
    fn reap(&mut self, output: &mut Vec<abi::Cqe>, budget: usize) -> io::Result<()> {
        match self {
            Self::Kernel(k) => k.reap(output, budget),
            #[cfg(test)]
            Self::Sim(s) => s.reap(output, budget),
        }
    }
}

#[cfg(test)]
impl Ring {
    /// Process death, not graceful shutdown: the virtual kernel stops all memory
    /// accesses before the request table releases affine resources. No flush.
    pub(crate) fn simulated_crash(&mut self) {
        let core = self.core.as_mut().unwrap();
        let RawRing::Sim(sim) = &mut core.raw else {
            panic!("cannot simulate crash on a live ring")
        };
        sim.staged.clear();
        sim.pending.clear();
        sim.completions.clear();
        sim.cq.clear();
        sim.accepted.clear();
        self.stopping = true;
        drop(self.core.take());
    }
}

/// One bounded polling turn. Merge pending work into the worker's sleep decision.
#[must_use]
pub enum Progress<T> {
    Pending(Work),
    Ready(T),
}

/// Queue/request capacities are allocated once at setup. One SQ entry is reserved
/// for the software wake poll. Completed, uncollected tickets consume capacity.
#[derive(Clone, Copy, Debug)]
pub struct Config {
    /// Request slots and SQ entries kept free of ACCEPT/CONNECT/RECV work for replies
    /// and continuations. Clamped to a quarter of small rings/tables.
    pub progress_reserve: u32,
    pub entries: u32,
    pub requests: u32,
    pub fixed_files: u32,
    pub completion_budget: usize,
    pub shutdown_timeout: Duration,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            progress_reserve: 8,
            entries: 256,
            requests: 1024,
            fixed_files: 256,
            completion_budget: 128,
            shutdown_timeout: Duration::from_secs(5),
        }
    }
}

/// Shared local ownership of an ordinary descriptor; cloning makes no syscall.
#[derive(Clone)]
pub struct File(Rc<FileHandle>, crate::slab_io::Io);
enum FileHandle {
    Os(OwnedFd),
    #[cfg(test)]
    Sim(crate::simulation::Handle),
}
impl File {
    pub(crate) fn pipe() -> io::Result<(Self, Self)> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            let (read, write) = world.pipe();
            return Ok((Self::simulated(read), Self::simulated(write)));
        }
        let mut fds = [-1; 2];
        // SAFETY: two writable descriptors; ownership transfers on success.
        if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC | libc::O_NONBLOCK) } < 0 {
            return Err(io::Error::last_os_error());
        }
        // Best effort: kernels may impose a smaller per-user pipe-page budget.
        // Partial splice completions work with either capacity.
        unsafe {
            libc::fcntl(fds[1], libc::F_SETPIPE_SZ, 1024 * 1024);
        }
        Ok(unsafe {
            (
                Self::new(OwnedFd::from_raw_fd(fds[0])),
                Self::new(OwnedFd::from_raw_fd(fds[1])),
            )
        })
    }
    pub fn new(fd: OwnedFd) -> Self {
        Self(Rc::new(FileHandle::Os(fd)), crate::slab_io::Io::default())
    }
    pub(crate) fn with_slab_io(mut self, io: crate::slab_io::Io) -> Self {
        self.1 = io;
        self
    }
    pub(crate) fn slab_io(&self) -> &crate::slab_io::Io {
        &self.1
    }
    #[cfg(test)]
    pub(crate) fn simulated(handle: crate::simulation::Handle) -> Self {
        Self(
            Rc::new(FileHandle::Sim(handle)),
            crate::slab_io::Io::default(),
        )
    }
    #[cfg(test)]
    pub(crate) fn simulation_id(&self) -> Option<i32> {
        match &*self.0 {
            FileHandle::Sim(h) => Some(h.id),
            _ => None,
        }
    }
    fn raw_id(&self) -> i32 {
        match &*self.0 {
            FileHandle::Os(fd) => fd.as_raw_fd(),
            #[cfg(test)]
            FileHandle::Sim(handle) => handle.id,
        }
    }
    pub(crate) fn shutdown_socket(&self) {
        #[cfg(test)]
        if let FileHandle::Sim(handle) = &*self.0 {
            handle.shutdown();
            return;
        }
        // SAFETY: owned live OS descriptor.
        unsafe {
            libc::shutdown(self.as_fd().as_raw_fd(), libc::SHUT_RDWR);
        }
    }
}
impl AsFd for File {
    fn as_fd(&self) -> BorrowedFd<'_> {
        match &*self.0 {
            FileHandle::Os(fd) => fd.as_fd(),
            #[cfg(test)]
            FileHandle::Sim(_) => panic!("simulated descriptor escaped to OS"),
        }
    }
}

pub(crate) struct Identity;
/// Ring-scoped registered descriptor. The ring retains a slot until every handle
/// and request releases it. This capability cannot be constructed from an index.
/// Unused registrations are closed by [`Ring::progress`] or [`Ring::wait`].
#[derive(Clone)]
pub struct FixedFile(Rc<FixedRegistration>);
struct FixedRegistration {
    identity: Rc<Identity>,
    index: u32,
    _file: File,
    unused: Rc<RefCell<VecDeque<u32>>>,
    _inbound: Option<Rc<Inbound>>,
}
impl Drop for FixedFile {
    fn drop(&mut self) {
        // The table owns one reference; requests own ordinary FixedFile handles.
        if Rc::strong_count(&self.0) == 2 {
            self.0.unused.borrow_mut().push_back(self.0.index);
        }
    }
}

#[derive(Clone)]
pub enum Descriptor {
    File(File),
    Fixed(FixedFile),
}
impl From<File> for Descriptor {
    fn from(file: File) -> Self {
        Self::File(file)
    }
}
impl From<FixedFile> for Descriptor {
    fn from(file: FixedFile) -> Self {
        Self::Fixed(file)
    }
}

/// Absolute byte position; excludes Linux's special implicit-position sentinel.
#[derive(Clone, Copy, Debug)]
pub struct FileOffset(u64);
impl FileOffset {
    pub fn new(offset: u64) -> io::Result<Self> {
        if offset > i64::MAX as u64 {
            Err(invalid("file offset exceeds off_t"))
        } else {
            Ok(Self(offset))
        }
    }
}

/// Validated nonempty subrange of a 4 MiB slot. Published length is additionally
/// checked when submitting a Buffer. O_DIRECT alignment is device-specific and
/// remains the application's responsibility.
#[derive(Clone, Copy, Debug)]
pub struct BufferRange {
    start: usize,
    len: usize,
}
impl BufferRange {
    pub fn new(range: Range<usize>) -> io::Result<Self> {
        if range.start >= range.end || range.end > BUFFER_SIZE {
            Err(invalid("invalid buffer range"))
        } else {
            Ok(Self {
                start: range.start,
                len: range.end - range.start,
            })
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub enum Readiness {
    Readable,
    Writable,
}
impl Readiness {
    fn mask(self) -> u32 {
        let mask = match self {
            Self::Readable => libc::POLLIN,
            Self::Writable => libc::POLLOUT,
        } as u32;
        // poll32_events is word-reversed on big-endian Linux.
        if cfg!(target_endian = "big") {
            mask.rotate_left(16)
        } else {
            mask
        }
    }
}

/// A submission rejected before reaching the kernel returns its storage.
pub struct Rejected<T> {
    pub error: io::Error,
    pub resource: T,
}
impl<T> std::fmt::Debug for Rejected<T> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Rejected")
            .field("error", &self.error)
            .finish_non_exhaustive()
    }
}
pub struct Completion<T> {
    pub resource: T,
    pub result: io::Result<usize>,
}

mod sealed {
    pub trait Sealed {}
}
/// Sealed operation marker: users cannot forge a resource/completion pairing.
pub trait Operation: sealed::Sealed {
    type Resource;
}
pub struct Read<B: Writable = Fill>(PhantomData<B>);
pub enum Write {}
pub enum SendZc {}
pub enum Bytes {}
pub enum PageIo {}
pub enum Accept {}
pub enum Control {}
pub enum PunchHole {}
pub enum Splice {}
pub enum Cancel {}
macro_rules! operation {
    ($op:ty, $resource:ty) => {
        impl sealed::Sealed for $op {}
        impl Operation for $op {
            type Resource = $resource;
        }
    };
}
impl<B: Writable> sealed::Sealed for Read<B> {}
impl<B: Writable> Operation for Read<B> {
    type Resource = B;
}
operation!(Write, Buffer);
operation!(SendZc, Buffer);
operation!(Bytes, Box<[u8]>);
operation!(PageIo, Box<Page>);

/// Owned, sector-aligned storage for slab metadata I/O.
#[repr(C, align(4096))]
pub struct Page(pub [u8; 4096]);
operation!(Accept, Option<File>);
operation!(Control, ());
operation!(PunchHole, ());
operation!(Splice, ());
operation!(Cancel, ());

struct Book {
    identity: Rc<Identity>,
    abandoned: RefCell<VecDeque<(u64, bool)>>,
}
/// An affine, typed request identifier. Dropping abandons observation, not I/O.
/// Forgetting a ticket may exhaust capacity, but cannot release its resource.
///
/// ```compile_fail
/// use racer_dataplane::uring::{Ticket, Read};
/// fn send<T: Send>() {}
/// send::<Ticket<Read>>();
/// ```
/// ```compile_fail
/// use racer_dataplane::uring::{Ring, Ticket, Read};
/// fn wrong_kind(ring: &mut Ring, ticket: &mut Ticket<Read>) {
///     let _ = ring.take_write(ticket);
/// }
/// ```
/// ```compile_fail
/// use racer_dataplane::uring::{Ticket, Read};
/// fn duplicate(ticket: Ticket<Read>) { let _copy = ticket.clone(); }
/// ```
#[must_use]
pub struct Ticket<O: Operation> {
    id: u64,
    book: Rc<Book>,
    collected: bool,
    cancel_on_drop: bool,
    _op: PhantomData<O>,
}
impl<O: Operation> Ticket<O> {
    /// Request cancellation on abandonment, retried by the ring under SQ pressure.
    /// The resource remains retained until the target's terminal completion.
    pub(crate) fn cancel_on_drop(mut self) -> Self {
        self.cancel_on_drop = true;
        self
    }
}
impl<O: Operation> std::fmt::Debug for Ticket<O> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Ticket")
            .field("id", &self.id)
            .field("collected", &self.collected)
            .finish()
    }
}
impl<O: Operation> Drop for Ticket<O> {
    fn drop(&mut self) {
        if !self.collected {
            self.book
                .abandoned
                .borrow_mut()
                .push_back((self.id, self.cancel_on_drop));
        }
    }
}

enum Resource {
    Page(Box<Page>),
    Writable(WritableStorage),
    Buffer(Buffer),
    Bytes(Box<[u8]>),
    Address {
        _storage: Box<libc::sockaddr_storage>,
    },
    Accepted(Option<File>),
    None,
}
enum State {
    InFlight,
    Notification(i32),
    Complete(i32),
}
struct Request {
    slab_pending: Option<Box<SlabPending>>,
    slab_charge: Option<crate::slab_io::Charge>,
    metric_traffic: Option<crate::metrics::Traffic>,
    // Optional application ownership, retained even when its ticket is abandoned.
    _keepalive: Option<Rc<dyn std::any::Any>>,
    resource: Resource,
    _fd: Option<Descriptor>,
    opcode: u8,
    state: State,
    abandoned: bool,
}
struct SlabPending {
    sqe: abi::Sqe,
    io: crate::slab_io::Io,
    bytes: usize,
    since: Instant,
    waited: bool,
}
impl Request {
    // Apply a completion without accessing the kernel. Returns true only on
    // proven terminal CQEs.
    fn complete(&mut self, res: i32, flags: u32) -> io::Result<bool> {
        let res = match (&self.state, flags & (abi::MORE | abi::NOTIF)) {
            (State::InFlight, 0) => res,
            (State::InFlight, abi::MORE) if self.opcode == abi::SEND_ZC => {
                #[cfg(test)]
                if crate::simulation::current().is_some_and(|world| {
                    world.activate_mutant(crate::simulation::history::Mutant::PrematureZcRetirement)
                }) {
                    self.state = State::Complete(res);
                    return Ok(true);
                }
                self.state = State::Notification(res);
                return Ok(false);
            }
            (State::Notification(res), abi::NOTIF) if self.opcode == abi::SEND_ZC => *res,
            _ => return Err(io::Error::other("unexpected CQE lifecycle flags")),
        };
        if self.opcode == abi::ACCEPT
            && res >= 0
            && !matches!(self.resource, Resource::Accepted(Some(_)))
        {
            // SAFETY: caller supplies a single-shot accept CQE with a fresh fd.
            self.resource =
                Resource::Accepted(Some(File::new(unsafe { OwnedFd::from_raw_fd(res) })));
        }
        if let Some(charge) = self.slab_charge.take() {
            charge.finish(res.max(0) as usize);
        }
        self.state = State::Complete(res);
        Ok(true)
    }
    fn cancel_queued(&mut self) -> bool {
        let Some(pending) = self.slab_pending.take() else {
            return false;
        };
        if pending.waited {
            pending.io.waited(pending.since);
        }
        self.state = State::Complete(-libc::ECANCELED);
        true
    }
}
struct Slot {
    generation: u32,
    request: Option<Request>,
}

struct Core {
    raw: RawRing,
    lease: MemoryLease,
    slots: Vec<Slot>,
    free: Vec<u32>,
    fixed: Vec<Option<Rc<FixedRegistration>>>,
    unused_fixed: Rc<RefCell<VecDeque<u32>>>,
    cqes: Vec<abi::Cqe>,
    cqe_next: usize,
    wake: Arc<Wake>,
    wake_armed: bool,
    cancel_pending: bool,
    abandoned_cancel_pending: bool,
    registered: bool,
}

/// A nonblocking, coalescing eventfd capability, valid after driver destruction.
pub struct Wake {
    fd: Option<OwnedFd>,
    #[cfg(test)]
    pending: std::sync::atomic::AtomicBool,
}
impl Wake {
    pub(crate) fn new() -> io::Result<Self> {
        #[cfg(test)]
        if crate::simulation::current().is_some() {
            return Ok(Self {
                fd: None,
                pending: false.into(),
            });
        }
        // SAFETY: no pointer arguments; fresh descriptor on success.
        let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
        if fd < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(Self {
                fd: Some(unsafe { OwnedFd::from_raw_fd(fd) }),
                #[cfg(test)]
                pending: false.into(),
            })
        }
    }
    fn drain(&self) -> io::Result<()> {
        let Some(fd) = &self.fd else {
            #[cfg(test)]
            self.pending.store(false, Ordering::Release);
            return Ok(());
        };
        let mut value = 0u64;
        loop {
            // SAFETY: writable eight-byte eventfd output, synchronous access.
            let result = unsafe { libc::read(fd.as_raw_fd(), (&mut value as *mut u64).cast(), 8) };
            if result == 8 {
                return Ok(());
            }
            let error = io::Error::last_os_error();
            match error.raw_os_error() {
                Some(libc::EINTR) => continue,
                Some(libc::EAGAIN) => return Ok(()),
                _ => return Err(error),
            }
        }
    }
}
impl workers::Wake for Wake {
    fn wake(&self) {
        let Some(fd) = &self.fd else {
            #[cfg(test)]
            self.pending.store(true, Ordering::Release);
            return;
        };
        let value = 1u64;
        loop {
            // SAFETY: live owned eventfd, readable eight-byte value. Saturation
            // means a wake is already pending. There is no close/reuse race.
            let result = unsafe { libc::write(fd.as_raw_fd(), (&value as *const u64).cast(), 8) };
            if result == 8 || io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
                return;
            }
        }
    }
}
impl std::task::Wake for Wake {
    fn wake(self: Arc<Self>) {
        workers::Wake::wake(&*self);
    }
    fn wake_by_ref(self: &Arc<Self>) {
        workers::Wake::wake(&**self);
    }
}

/// Worker-local owner of registrations, requests and completion storage.
///
/// ```compile_fail
/// use racer_dataplane::uring::Ring;
/// fn send<T: Send>() {}
/// send::<Ring>();
/// ```
pub struct Ring {
    slab_queue: VecDeque<u64>,
    slab_deadline: Option<Instant>,
    inbound: Rc<std::cell::Cell<usize>>,
    metrics: crate::metrics::Local,
    // Take-and-forget guard: Rust must never automatically drop live requests
    // after a failed cleanup or a panic. Core has no public escape hatch.
    core: Option<Box<Core>>,
    book: Rc<Book>,
    pool: WorkerPool,
    config: Config,
    stopping: bool,
    completion_epoch: u64,
}

impl Ring {
    pub fn new(placement: &WorkerContext, pool: WorkerPool, config: Config) -> io::Result<Self> {
        placement.bind_pool(&pool)?;
        // SAFETY: sched_getcpu has no pointer arguments.
        let cpu = unsafe { libc::sched_getcpu() };
        if cpu < 0 {
            return Err(io::Error::last_os_error());
        }
        if cpu as usize != placement.cpu_id().0 || pool.numa_node_id() != placement.numa_node_id() {
            return Err(invalid("ring placement does not match worker pool/CPU"));
        }
        Self::create(pool, config)
    }

    fn create(pool: WorkerPool, config: Config) -> io::Result<Self> {
        if !(2..=32768).contains(&config.entries)
            || !config.entries.is_power_of_two()
            || config.requests == 0
            || config.requests > 1_048_576
            || config.fixed_files > 1_048_576
            || config.completion_budget == 0
            || config.completion_budget > 65536
        {
            return Err(invalid("invalid io_uring capacities"));
        }
        let lease = pool.memory_lease();
        if lease.buffers().len() > 65536 {
            return Err(invalid("too many fixed buffers"));
        }
        let raw = RawRing::new(config.entries)?;
        let wake = Arc::new(Wake::new()?);
        let iovecs: Vec<_> = lease
            .buffers()
            .map(|b| libc::iovec {
                iov_base: b.region.address.cast(),
                iov_len: b.region.len,
            })
            .collect();
        // Registration pins pages but cannot access payload. No request has yet
        // been published, so initialization rollback may safely release the lease.
        {
            // Concurrent long-term pins of one NUMA mapping can race kernel
            // page migration and fail with EFAULT during many-worker startup.
            // Serialize registration per mapping, without locking steady-state I/O.
            let _registration = lease.registration_lock();
            raw.register(0, iovecs.as_ptr().cast(), iovecs.len() as u32)?;
        }
        if config.fixed_files > 0 {
            let fds = vec![-1i32; config.fixed_files as usize];
            raw.register(2, fds.as_ptr().cast(), config.fixed_files)?;
        }
        let book = Rc::new(Book {
            identity: Rc::new(Identity),
            abandoned: RefCell::new(VecDeque::with_capacity(config.requests as usize)),
        });
        Ok(Self {
            inbound: Rc::new(std::cell::Cell::new(0)),
            core: Some(Box::new(Core {
                raw,
                lease,
                wake,
                wake_armed: false,
                cancel_pending: false,
                abandoned_cancel_pending: false,
                registered: true,
                slots: (0..config.requests)
                    .map(|_| Slot {
                        generation: 0,
                        request: None,
                    })
                    .collect(),
                free: (0..config.requests).rev().collect(),
                fixed: (0..config.fixed_files).map(|_| None).collect(),
                unused_fixed: Rc::new(RefCell::new(VecDeque::with_capacity(
                    config.fixed_files as usize,
                ))),
                cqes: Vec::with_capacity(config.completion_budget),
                cqe_next: 0,
            })),
            book,
            metrics: crate::metrics::Local::default(),
            pool,
            config,
            stopping: false,
            completion_epoch: 0,
            slab_queue: VecDeque::new(),
            slab_deadline: None,
        })
    }

    pub fn pool(&self) -> &WorkerPool {
        &self.pool
    }
    pub fn metrics(&self) -> &crate::metrics::Local {
        &self.metrics
    }
    /// Attach only to admitted HTTP body sends, before their first submission.
    pub(crate) fn measure_send<K: Operation>(
        &mut self,
        ticket: &Ticket<K>,
        traffic: Option<crate::metrics::Traffic>,
    ) {
        self.core.as_mut().unwrap().slots[ticket.id as u32 as usize]
            .request
            .as_mut()
            .unwrap()
            .metric_traffic = traffic;
    }
    pub(crate) fn identity(&self) -> &Rc<Identity> {
        &self.book.identity
    }
    pub fn wake_handle(&self) -> Arc<Wake> {
        self.core.as_ref().expect("ring closed").wake.clone()
    }

    pub fn register_file(&mut self, file: File) -> io::Result<FixedFile> {
        self.register_file_inner(file, None)
    }

    fn register_file_inner(
        &mut self,
        file: File,
        inbound: Option<Rc<Inbound>>,
    ) -> io::Result<FixedFile> {
        self.validate_file(&file)?;
        #[cfg(test)]
        if let Some(world) = crate::simulation::current()
            && world.admission(file.raw_id(), crate::simulation::Phase::Registration)
        {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        if self.stopping {
            return Err(io::Error::new(io::ErrorKind::BrokenPipe, "ring stopping"));
        }
        let core = self.core.as_mut().unwrap();
        if inbound.is_some()
            && core
                .fixed
                .iter()
                .filter(|s| s.as_ref().is_none_or(|r| Rc::strong_count(r) == 1))
                .count()
                <= inbound_reserve(self.config.fixed_files)
        {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        let index = core
            .fixed
            .iter()
            .position(|slot| slot.as_ref().is_none_or(|r| Rc::strong_count(r) == 1))
            .ok_or_else(|| io::Error::from(io::ErrorKind::WouldBlock))?;
        let fd = file.raw_id();
        let update = abi::FilesUpdate {
            offset: index as u32,
            reserved: 0,
            fds: &fd as *const _ as u64,
        };
        core.raw
            .register(6, (&update as *const abi::FilesUpdate).cast(), 1)?;
        let registration = Rc::new(FixedRegistration {
            identity: self.book.identity.clone(),
            index: index as u32,
            _file: file,
            unused: core.unused_fixed.clone(),
            _inbound: inbound,
        });
        core.fixed[index] = Some(registration.clone());
        Ok(FixedFile(registration))
    }

    fn descriptor(&self, descriptor: &Descriptor, sqe: &mut abi::Sqe) -> io::Result<()> {
        match descriptor {
            Descriptor::File(file) => {
                self.validate_file(file)?;
                sqe.fd = file.raw_id();
            }
            Descriptor::Fixed(file) => {
                if !Rc::ptr_eq(&file.0.identity, &self.book.identity) {
                    return Err(invalid("foreign fixed file"));
                }
                sqe.fd = file.0.index as i32;
                sqe.flags = 1; // IOSQE_FIXED_FILE
            }
        }
        Ok(())
    }
    fn validate_file(&self, _file: &File) -> io::Result<()> {
        #[cfg(test)]
        match (
            &self
                .core
                .as_ref()
                .ok_or_else(|| invalid("ring closed"))?
                .raw,
            &*_file.0,
        ) {
            (RawRing::Kernel(_), FileHandle::Os(_)) => {}
            (RawRing::Sim(s), FileHandle::Sim(h)) if h.belongs_to(&s.world) => {}
            _ => return Err(invalid("foreign IO backend or simulation world")),
        }
        Ok(())
    }

    pub(crate) fn validate_fill<B: Writable>(&self, fill: &B) -> io::Result<()> {
        self.buffer(
            fill.region(),
            BufferRange::new(0..BUFFER_SIZE)?,
            BUFFER_SIZE,
            &mut abi::Sqe::default(),
        )
    }

    #[cfg(test)]
    pub(crate) fn http_test_ring(pool: WorkerPool, config: Config) -> io::Result<Self> {
        Self::create(pool, config)
    }

    #[cfg(test)]
    pub(crate) fn http_test_request_count(&self) -> usize {
        self.core
            .as_ref()
            .unwrap()
            .slots
            .iter()
            .filter(|s| s.request.is_some())
            .count()
    }

    #[cfg(test)]
    pub(crate) fn http_test_idle(&self) -> bool {
        let core = self.core.as_ref().unwrap();
        core.slots.iter().all(|s| s.request.is_none())
            && core.fixed.iter().all(Option::is_none)
            && !core.abandoned_cancel_pending
            && self.book.abandoned.borrow().is_empty()
    }

    /// Changes whenever progress observes completions (including software wakes).
    /// A scheduler spanning several turns must recheck earlier sleepers if this
    /// changes during its sweep, before allowing the driver to sleep.
    pub(crate) fn completion_epoch(&self) -> u64 {
        self.completion_epoch
    }

    fn buffer(
        &self,
        region: BufferRegion<'_>,
        range: BufferRange,
        limit: usize,
        sqe: &mut abi::Sqe,
    ) -> io::Result<()> {
        let core = self.core.as_ref().ok_or_else(|| invalid("ring closed"))?;
        let mapping = core.lease.region();
        let offset = region
            .index
            .checked_mul(BUFFER_SIZE)
            .ok_or_else(|| invalid("invalid buffer index"))?;
        if offset >= mapping.len
            || region.region.address as usize != mapping.address as usize + offset
            || range.start + range.len > limit
        {
            return Err(invalid("foreign buffer or range beyond published bytes"));
        }
        sqe.addr = region.region.address as u64 + range.start as u64;
        sqe.len = range.len as u32;
        sqe.buf_index = region.index as u16;
        Ok(())
    }

    fn enqueue<O: Operation>(
        &mut self,
        sqe: abi::Sqe,
        resource: Resource,
        fd: Option<Descriptor>,
    ) -> Result<Ticket<O>, (io::Error, Resource)> {
        let io = if matches!(
            sqe.opcode,
            abi::READ_FIXED | abi::WRITE_FIXED | 22 | 23 | 3 | 17
        ) {
            fd.as_ref().map(|fd| match fd {
                Descriptor::File(file) => file.1.clone(),
                Descriptor::Fixed(file) => file.0._file.1.clone(),
            })
        } else {
            None
        };
        self.enqueue_slab(sqe, resource, fd, io)
    }

    fn enqueue_slab<O: Operation>(
        &mut self,
        mut sqe: abi::Sqe,
        resource: Resource,
        fd: Option<Descriptor>,
        io: Option<crate::slab_io::Io>,
    ) -> Result<Ticket<O>, (io::Error, Resource)> {
        let io = io.filter(|io| io.limited());
        let reserve =
            if io.is_some() || matches!(sqe.opcode, abi::ACCEPT | abi::RECV | abi::CONNECT) {
                self.config
                    .progress_reserve
                    .min(self.config.entries / 4)
                    .min(self.config.requests / 4) as usize
            } else {
                0
            };
        let error = if self.stopping {
            Some(io::Error::new(io::ErrorKind::BrokenPipe, "ring stopping"))
        } else if self.core.as_ref().is_none_or(|c| {
            c.free.len() <= reserve || (io.is_none() && c.raw.space() <= 1 + reserve as u32)
        }) {
            Some(io::Error::from(io::ErrorKind::WouldBlock))
        } else {
            None
        };
        if let Some(error) = error {
            return Err((error, resource));
        }
        let core = self.core.as_mut().unwrap();
        let index = core.free.pop().unwrap();
        let slot = &mut core.slots[index as usize];
        // Slots with an exhausted generation are retired by release().
        slot.generation += 1;
        let id = (u64::from(slot.generation) << 32) | u64::from(index);
        sqe.user_data = id;
        let slab_pending = io.map(|io| {
            Box::new(SlabPending {
                bytes: if matches!(sqe.opcode, 3 | 17) {
                    0
                } else {
                    sqe.len as usize
                },
                sqe,
                io,
                since: crate::environment::now(),
                waited: false,
            })
        });
        let queued = slab_pending.is_some();
        slot.request = Some(Request {
            slab_pending,
            slab_charge: None,
            metric_traffic: None,
            _keepalive: None,
            resource,
            _fd: fd,
            opcode: sqe.opcode,
            state: State::InFlight,
            abandoned: false,
        });
        if queued {
            self.slab_queue.push_back(id);
            self.slab_deadline = Some(crate::environment::now());
        } else {
            core.raw.push(sqe);
        }
        Ok(Ticket {
            id,
            book: self.book.clone(),
            collected: false,
            cancel_on_drop: false,
            _op: PhantomData,
        })
    }

    fn fill_op<B: Writable>(
        &mut self,
        fd: Descriptor,
        fill: B,
        range: BufferRange,
        opcode: u8,
        off: u64,
    ) -> Result<Ticket<Read<B>>, Rejected<B>> {
        let mut sqe = abi::Sqe {
            opcode,
            off,
            ..Default::default()
        };
        if let Err(error) = self
            .descriptor(&fd, &mut sqe)
            .and_then(|_| self.buffer(fill.region(), range, BUFFER_SIZE, &mut sqe))
        {
            return Err(Rejected {
                error,
                resource: fill,
            });
        }
        if opcode == abi::RECV {
            sqe.buf_index = 0;
        }
        self.enqueue(sqe, Resource::Writable(fill.into_storage()), Some(fd))
            .map_err(|(error, r)| {
                let Resource::Writable(storage) = r else {
                    unreachable!()
                };
                let resource = B::from_storage(storage)
                    .unwrap_or_else(|_| unreachable!("sealed writable pairing"));
                Rejected { error, resource }
            })
    }

    /// Consumes exclusive storage until completion; no publication is automatic.
    /// ```compile_fail
    /// use racer_dataplane::{buffers::Fill, uring::*};
    /// fn submit(ring: &mut Ring, fd: Descriptor, fill: Fill, range: BufferRange) {
    ///     let _ = ring.recv(fd, fill, range);
    ///     let _ = fill.publish(1);
    /// }
    /// ```
    pub fn read<B: Writable>(
        &mut self,
        fd: Descriptor,
        fill: B,
        range: BufferRange,
        offset: FileOffset,
    ) -> Result<Ticket<Read<B>>, Rejected<B>> {
        self.fill_op(fd, fill, range, abi::READ_FIXED, offset.0)
    }
    pub fn recv<B: Writable>(
        &mut self,
        fd: Descriptor,
        fill: B,
        range: BufferRange,
    ) -> Result<Ticket<Read<B>>, Rejected<B>> {
        self.fill_op(fd, fill, range, abi::RECV, 0)
    }

    fn buffer_op<O: Operation>(
        &mut self,
        fd: Descriptor,
        buffer: Buffer,
        range: BufferRange,
        opcode: u8,
        off: u64,
    ) -> Result<Ticket<O>, Rejected<Buffer>> {
        let mut sqe = abi::Sqe {
            opcode,
            off,
            ..Default::default()
        };
        if let Err(error) = self
            .descriptor(&fd, &mut sqe)
            .and_then(|_| self.buffer(buffer.region(), range, buffer.as_slice().len(), &mut sqe))
        {
            return Err(Rejected {
                error,
                resource: buffer,
            });
        }
        if opcode == abi::SEND_ZC {
            sqe.ioprio = 4;
        } // RECVSEND_FIXED_BUF
        if opcode == abi::SEND {
            sqe.buf_index = 0;
        }
        if opcode != abi::WRITE_FIXED {
            sqe.op_flags = libc::MSG_NOSIGNAL as u32;
        }
        self.enqueue(sqe, Resource::Buffer(buffer), Some(fd))
            .map_err(|(error, r)| {
                let Resource::Buffer(resource) = r else {
                    unreachable!()
                };
                Rejected { error, resource }
            })
    }
    pub fn write(
        &mut self,
        fd: Descriptor,
        buffer: Buffer,
        range: BufferRange,
        offset: FileOffset,
    ) -> Result<Ticket<Write>, Rejected<Buffer>> {
        self.buffer_op(fd, buffer, range, abi::WRITE_FIXED, offset.0)
    }
    pub fn send(
        &mut self,
        fd: Descriptor,
        buffer: Buffer,
        range: BufferRange,
    ) -> Result<Ticket<Write>, Rejected<Buffer>> {
        self.buffer_op(fd, buffer, range, abi::SEND, 0)
    }
    pub fn send_zc(
        &mut self,
        fd: Descriptor,
        buffer: Buffer,
        range: BufferRange,
    ) -> Result<Ticket<SendZc>, Rejected<Buffer>> {
        self.buffer_op(fd, buffer, range, abi::SEND_ZC, 0)
    }

    fn bytes_op(
        &mut self,
        fd: Descriptor,
        mut bytes: Box<[u8]>,
        range: Range<usize>,
        opcode: u8,
    ) -> Result<Ticket<Bytes>, Rejected<Box<[u8]>>> {
        let mut sqe = abi::Sqe {
            opcode,
            ..Default::default()
        };
        let validation = if range.start >= range.end
            || range.end > bytes.len()
            || range.end - range.start > BUFFER_SIZE
        {
            Err(invalid("invalid small I/O range"))
        } else {
            // SAFETY: validated nonempty subrange of the owned allocation.
            sqe.addr = unsafe { bytes.as_mut_ptr().add(range.start) } as u64;
            sqe.len = (range.end - range.start) as u32;
            self.descriptor(&fd, &mut sqe)
        };
        if let Err(error) = validation {
            return Err(Rejected {
                error,
                resource: bytes,
            });
        }
        if opcode == abi::SEND {
            sqe.op_flags = libc::MSG_NOSIGNAL as u32;
        }
        self.enqueue(sqe, Resource::Bytes(bytes), Some(fd))
            .map_err(|(error, r)| {
                let Resource::Bytes(resource) = r else {
                    unreachable!()
                };
                Rejected { error, resource }
            })
    }
    pub fn recv_bytes(
        &mut self,
        fd: Descriptor,
        bytes: Box<[u8]>,
    ) -> Result<Ticket<Bytes>, Rejected<Box<[u8]>>> {
        let len = bytes.len();
        self.recv_bytes_range(fd, bytes, 0..len)
    }
    /// Buffered file read with owned storage retained through terminal completion.
    pub(crate) fn read_bytes(
        &mut self,
        fd: Descriptor,
        bytes: Box<[u8]>,
        offset: u64,
    ) -> Result<Ticket<Bytes>, Rejected<Box<[u8]>>> {
        if bytes.is_empty() || bytes.len() > BUFFER_SIZE || offset > i64::MAX as u64 {
            return Err(Rejected {
                error: invalid("invalid buffered read range"),
                resource: bytes,
            });
        }
        let mut sqe = abi::Sqe {
            opcode: 22,
            off: offset,
            addr: bytes.as_ptr() as u64,
            len: bytes.len() as u32,
            ..Default::default()
        };
        if let Err(error) = self.descriptor(&fd, &mut sqe) {
            return Err(Rejected {
                error,
                resource: bytes,
            });
        }
        self.enqueue(sqe, Resource::Bytes(bytes), Some(fd))
            .map_err(|(error, resource)| {
                let Resource::Bytes(resource) = resource else {
                    unreachable!()
                };
                Rejected { error, resource }
            })
    }
    pub fn send_bytes(
        &mut self,
        fd: Descriptor,
        bytes: Box<[u8]>,
    ) -> Result<Ticket<Bytes>, Rejected<Box<[u8]>>> {
        let len = bytes.len();
        self.send_bytes_range(fd, bytes, 0..len)
    }

    /// Receives into a nonempty subrange, returning the entire allocation on
    /// completion or rejection. Bytes outside the range are preserved.
    pub fn recv_bytes_range(
        &mut self,
        fd: Descriptor,
        bytes: Box<[u8]>,
        range: Range<usize>,
    ) -> Result<Ticket<Bytes>, Rejected<Box<[u8]>>> {
        self.bytes_op(fd, bytes, range, abi::RECV)
    }

    /// Sends a nonempty subrange without copying or reallocating the storage.
    pub fn send_bytes_range(
        &mut self,
        fd: Descriptor,
        bytes: Box<[u8]>,
        range: Range<usize>,
    ) -> Result<Ticket<Bytes>, Rejected<Box<[u8]>>> {
        self.bytes_op(fd, bytes, range, abi::SEND)
    }

    pub fn accept(&mut self, fd: Descriptor) -> io::Result<Ticket<Accept>> {
        let mut sqe = abi::Sqe {
            opcode: abi::ACCEPT,
            op_flags: (libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK) as u32,
            ..Default::default()
        };
        self.descriptor(&fd, &mut sqe)?;
        self.enqueue(sqe, Resource::Accepted(None), Some(fd))
            .map_err(|(e, _)| e)
    }

    /// Retain application-owned resources until a real terminal completion has
    /// been collected or abandoned. In particular, slab extents must outlive I/O.
    pub(crate) fn retain<O: Operation>(
        &mut self,
        ticket: &Ticket<O>,
        owner: Rc<dyn std::any::Any>,
    ) {
        assert!(Rc::ptr_eq(&ticket.book.identity, &self.book.identity));
        let slot = &mut self.core.as_mut().unwrap().slots[ticket.id as u32 as usize];
        assert_eq!(slot.generation, (ticket.id >> 32) as u32);
        let request = slot.request.as_mut().unwrap();
        assert!(request._keepalive.is_none());
        request._keepalive = Some(owner);
    }

    pub fn write_page(
        &mut self,
        fd: Descriptor,
        page: Box<Page>,
        offset: FileOffset,
    ) -> Result<Ticket<PageIo>, Rejected<Box<Page>>> {
        let mut sqe = abi::Sqe {
            opcode: 23, // IORING_OP_WRITE
            off: offset.0,
            addr: page.0.as_ptr() as u64,
            len: 4096,
            ..Default::default()
        };
        if !offset.0.is_multiple_of(4096) {
            return Err(Rejected {
                error: invalid("unaligned page offset"),
                resource: page,
            });
        }
        if let Err(error) = self.descriptor(&fd, &mut sqe) {
            return Err(Rejected {
                error,
                resource: page,
            });
        }
        self.enqueue(sqe, Resource::Page(page), Some(fd))
            .map_err(|(error, r)| {
                let Resource::Page(resource) = r else {
                    unreachable!()
                };
                Rejected { error, resource }
            })
    }

    /// Submit only after observing completion of every write in the barrier.
    pub fn sync_data(&mut self, fd: Descriptor) -> io::Result<Ticket<Control>> {
        let mut sqe = abi::Sqe {
            opcode: 3,   // IORING_OP_FSYNC
            op_flags: 1, // IORING_FSYNC_DATASYNC
            ..Default::default()
        };
        self.descriptor(&fd, &mut sqe)?;
        self.enqueue(sqe, Resource::None, Some(fd))
            .map_err(|(error, _)| error)
    }

    /// Whole-page detachment before allocator-owned slab reuse. The caller must
    /// hold exclusive allocation authority until the target completion.
    pub(crate) fn punch_hole(
        &mut self,
        fd: Descriptor,
        offset: FileOffset,
        len: u64,
        owner: Rc<dyn std::any::Any>,
    ) -> io::Result<Ticket<PunchHole>> {
        if len == 0
            || offset.0 % 4096 != 0
            || len % 4096 != 0
            || offset
                .0
                .checked_add(len)
                .is_none_or(|end| end > i64::MAX as u64)
        {
            return Err(invalid("unaligned hole punch"));
        }
        let mut sqe = abi::Sqe {
            opcode: 17, // IORING_OP_FALLOCATE: len holds mode, addr holds length.
            off: offset.0,
            addr: len,
            len: (libc::FALLOC_FL_PUNCH_HOLE | libc::FALLOC_FL_KEEP_SIZE) as u32,
            ..Default::default()
        };
        self.descriptor(&fd, &mut sqe)?;
        let ticket = self
            .enqueue(sqe, Resource::None, Some(fd))
            .map_err(|(e, _)| e)?;
        self.retain(&ticket, owner);
        Ok(ticket)
    }

    pub(crate) fn take_punch(
        &mut self,
        ticket: &mut Ticket<PunchHole>,
    ) -> io::Result<Option<io::Result<()>>> {
        Ok(self.take(ticket)?.map(|(_, res)| {
            result(res).and_then(|n| {
                if n == 0 {
                    Ok(())
                } else {
                    Err(invalid("invalid punch completion"))
                }
            })
        }))
    }

    // Keep the general API for callers without a reusable ownership bundle.
    #[allow(dead_code)]
    pub(crate) fn splice(
        &mut self,
        input: File,
        output: Descriptor,
        offset: Option<FileOffset>,
        len: usize,
        owner: Rc<dyn std::any::Any>,
    ) -> io::Result<Ticket<Splice>> {
        self.splice_owned(
            Rc::new((input, owner)),
            |owner| &owner.0,
            output,
            offset,
            len,
        )
    }

    /// Reuse an ownership allocation across splice submissions. The selector
    /// borrows the input from the retained owner, so callers cannot accidentally
    /// omit input retention. Output is retained separately by the request.
    /// As with `retain`, cancellation acknowledgments do not release the owner.
    pub(crate) fn splice_owned<T: std::any::Any>(
        &mut self,
        owner: Rc<T>,
        input: impl FnOnce(&T) -> &File,
        output: Descriptor,
        offset: Option<FileOffset>,
        len: usize,
    ) -> io::Result<Ticket<Splice>> {
        let input = input(&owner);
        let io = input.1.clone();
        if io.limited() && len > BUFFER_SIZE {
            return Err(invalid("slab splice exceeds maximum operation size"));
        }
        let mut sqe = abi::Sqe {
            opcode: 30,
            off: u64::MAX,
            addr: offset.map_or(u64::MAX, |offset| offset.0),
            len: u32::try_from(len).map_err(|_| invalid("splice length"))?,
            file_index: input.raw_id() as u32,
            op_flags: libc::SPLICE_F_NONBLOCK,
            ..Default::default()
        };
        self.descriptor(&output, &mut sqe)?;
        let ticket = self
            .enqueue_slab(sqe, Resource::None, Some(output), Some(io))
            .map_err(|(e, _)| e)?;
        self.retain(&ticket, owner);
        Ok(ticket)
    }
    pub(crate) fn take_splice(
        &mut self,
        ticket: &mut Ticket<Splice>,
    ) -> io::Result<Option<io::Result<usize>>> {
        Ok(self.take(ticket)?.map(|(_, res)| result(res)))
    }

    pub fn connect(&mut self, fd: Descriptor, address: SocketAddr) -> io::Result<Ticket<Control>> {
        self.connect_address(fd, address.into())
    }

    pub fn connect_address(
        &mut self,
        fd: Descriptor,
        address: crate::socket::Address,
    ) -> io::Result<Ticket<Control>> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current()
            && let Descriptor::File(file) = &fd
            && world.admission(file.raw_id(), crate::simulation::Phase::ConnectAdmission)
        {
            return Err(io::ErrorKind::WouldBlock.into());
        }
        // SAFETY: zero is valid storage and padding for either sockaddr variant.
        let mut storage: Box<libc::sockaddr_storage> = Box::new(unsafe { std::mem::zeroed() });
        let len = match address {
            crate::socket::Address::Tcp(SocketAddr::V4(a)) => {
                let addr = libc::sockaddr_in {
                    sin_family: libc::AF_INET as _,
                    sin_port: a.port().to_be(),
                    sin_addr: libc::in_addr {
                        s_addr: u32::from_ne_bytes(a.ip().octets()),
                    },
                    sin_zero: [0; 8],
                };
                // SAFETY: sockaddr_storage has adequate size/alignment.
                unsafe {
                    (storage.as_mut() as *mut libc::sockaddr_storage)
                        .cast::<libc::sockaddr_in>()
                        .write(addr);
                }
                size_of::<libc::sockaddr_in>()
            }
            crate::socket::Address::Tcp(SocketAddr::V6(a)) => {
                let addr = libc::sockaddr_in6 {
                    sin6_family: libc::AF_INET6 as _,
                    sin6_port: a.port().to_be(),
                    sin6_flowinfo: a.flowinfo().to_be(),
                    sin6_addr: libc::in6_addr {
                        s6_addr: a.ip().octets(),
                    },
                    sin6_scope_id: a.scope_id(),
                };
                // SAFETY: sockaddr_storage has adequate size/alignment.
                unsafe {
                    (storage.as_mut() as *mut libc::sockaddr_storage)
                        .cast::<libc::sockaddr_in6>()
                        .write(addr);
                }
                size_of::<libc::sockaddr_in6>()
            }
            crate::socket::Address::Unix(path) => {
                // SAFETY: sockaddr_storage has sufficient size and alignment for
                // sockaddr_un. The owned storage remains live through the CQE.
                unsafe {
                    (storage.as_mut() as *mut libc::sockaddr_storage)
                        .cast::<libc::sockaddr_un>()
                        .write(path.sockaddr());
                }
                path.sockaddr_len()
            }
        };
        let mut sqe = abi::Sqe {
            opcode: abi::CONNECT,
            addr: storage.as_ref() as *const _ as u64,
            off: len as u64,
            ..Default::default()
        };
        self.descriptor(&fd, &mut sqe)?;
        self.enqueue(sqe, Resource::Address { _storage: storage }, Some(fd))
            .map_err(|(e, _)| e)
    }

    /// One-shot readiness; rearm after draining the source. POLLHUP/POLLERR are
    /// returned as readiness bits. For rdma-core this is not a CQ completion.
    pub fn poll_fd(&mut self, fd: Descriptor, readiness: Readiness) -> io::Result<Ticket<Control>> {
        let mut sqe = abi::Sqe {
            opcode: abi::POLL,
            op_flags: readiness.mask(),
            ..Default::default()
        };
        self.descriptor(&fd, &mut sqe)?;
        self.enqueue(sqe, Resource::None, Some(fd))
            .map_err(|(e, _)| e)
    }

    pub fn cancel<O: Operation>(&mut self, ticket: &Ticket<O>) -> io::Result<Ticket<Cancel>> {
        if self.request(ticket)?.slab_pending.is_some() {
            // A NOP supplies the ordinary cancellation acknowledgment. The target
            // never reached the kernel and retains resources until collection.
            let ack = self
                .enqueue(abi::Sqe::default(), Resource::None, None)
                .map_err(|(e, _)| e)?;
            self.core.as_mut().unwrap().slots[ticket.id as u32 as usize]
                .request
                .as_mut()
                .unwrap()
                .cancel_queued();
            self.slab_queue.retain(|id| *id != ticket.id);
            return Ok(ack);
        }
        self.enqueue(
            abi::Sqe {
                opcode: abi::CANCEL,
                addr: ticket.id,
                ..Default::default()
            },
            Resource::None,
            None,
        )
        .map_err(|(e, _)| e)
    }

    fn request<O: Operation>(&self, ticket: &Ticket<O>) -> io::Result<&Request> {
        if !Rc::ptr_eq(&ticket.book, &self.book) || ticket.collected {
            return Err(invalid("foreign or collected ticket"));
        }
        let slot = self
            .core
            .as_ref()
            .and_then(|c| c.slots.get(ticket.id as u32 as usize))
            .ok_or_else(|| invalid("stale ticket"))?;
        if slot.generation != (ticket.id >> 32) as u32 {
            return Err(invalid("stale ticket generation"));
        }
        slot.request
            .as_ref()
            .ok_or_else(|| invalid("released ticket"))
    }

    /// Early result observation never transfers buffer ownership.
    pub fn send_result(&self, ticket: &Ticket<SendZc>) -> io::Result<Option<io::Result<usize>>> {
        Ok(match self.request(ticket)?.state {
            State::InFlight => None,
            State::Notification(r) | State::Complete(r) => Some(result(r)),
        })
    }

    fn take<O: Operation>(
        &mut self,
        ticket: &mut Ticket<O>,
    ) -> io::Result<Option<(Resource, i32)>> {
        let State::Complete(res) = self.request(ticket)?.state else {
            return Ok(None);
        };
        ticket.collected = true;
        let request = self
            .core
            .as_mut()
            .unwrap()
            .release(ticket.id as u32 as usize);
        Ok(Some((request.resource, res)))
    }

    /// Progress a bounded CQ/abandonment batch without sleeping. Returns true if
    /// completions were processed, more work is known or a budget was exhausted;
    /// a composite driver must stay awake and let its caller observe stop flags.
    pub fn progress(&mut self) -> io::Result<bool> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current()
            && !world.managed()
        {
            world.advance(Duration::from_millis(1));
            world.run_tasks();
        }
        self.abandoned();
        let slab_runnable = self.submit_slab();
        let core = self.core.as_mut().ok_or_else(|| invalid("ring closed"))?;
        #[cfg(test)]
        if self.stopping
            && let RawRing::Sim(sim) = &mut core.raw
        {
            sim.quiesce();
        }
        if !self.stopping {
            core.arm_wake();
        }
        #[cfg(test)]
        let simulated = matches!(core.raw, RawRing::Sim(_));
        #[cfg(not(test))]
        let simulated = false;
        if simulated || core.raw.needs_enter() {
            core.raw.enter(false, None)?;
        }
        if core.cqe_next == core.cqes.len() {
            core.cqes.clear();
            core.cqe_next = 0;
            core.raw
                .reap(&mut core.cqes, self.config.completion_budget)?;
        }
        let count = core.cqes.len() - core.cqe_next;
        if count != 0 {
            self.completion_epoch = self.completion_epoch.wrapping_add(1);
        }
        // No callbacks while reading CQ memory; release its head in one batch.
        while core.cqe_next < core.cqes.len() {
            let cqe = core.cqes[core.cqe_next];
            // complete records terminal state/releases the slot before dropping
            // resources that can invoke wakers. Never replay that CQE on unwind,
            // but retain the rest of this already-reaped batch for the next call.
            core.cqe_next += 1;
            core.complete(cqe, &self.metrics)?;
        }
        core.reclaim_fixed(self.config.completion_budget)?;
        // Return to the worker runtime after every wake, including one with no
        // application I/O, so a concurrent stop cannot be consumed then slept on.
        #[cfg(test)]
        let woke = core.wake.fd.is_none() && core.wake.pending.swap(false, Ordering::AcqRel);
        #[cfg(not(test))]
        let woke = false;
        Ok(slab_runnable
            || woke
            || count != 0
            || core.raw.ready()
            || core.raw.needs_enter()
            || !core.unused_fixed.borrow().is_empty()
            || !self.book.abandoned.borrow().is_empty())
    }

    fn submit_slab(&mut self) -> bool {
        self.slab_deadline = None;
        let Some(core) = &mut self.core else {
            return false;
        };
        let now = crate::environment::now();
        for _ in 0..self.config.completion_budget {
            let Some(&id) = self.slab_queue.front() else {
                return false;
            };
            let slot = &mut core.slots[id as u32 as usize];
            let Some(request) = slot
                .request
                .as_mut()
                .filter(|_| slot.generation == (id >> 32) as u32)
            else {
                self.slab_queue.pop_front();
                continue;
            };
            if self.stopping {
                request.cancel_queued();
            }
            let Some(pending) = &mut request.slab_pending else {
                self.slab_queue.pop_front();
                if request.abandoned {
                    drop(core.release(id as u32 as usize));
                }
                continue;
            };
            if core.raw.space() <= 1 {
                self.slab_deadline = Some(now);
                return true;
            }
            match pending.io.reserve(pending.bytes, now) {
                Ok(charge) => {
                    if pending.waited {
                        pending.io.waited(pending.since);
                    }
                    core.raw.push(pending.sqe);
                    request.slab_charge = Some(charge);
                    request.slab_pending = None;
                    self.slab_queue.pop_front();
                }
                Err(deadline) => {
                    pending.waited = true;
                    self.slab_deadline = Some(deadline);
                    return false;
                }
            }
        }
        if !self.slab_queue.is_empty() {
            self.slab_deadline = Some(now);
            return true;
        }
        false
    }

    pub(crate) fn slab_deadline(&self) -> Option<Instant> {
        self.slab_deadline
    }

    fn abandoned(&mut self) {
        let Some(core) = &mut self.core else {
            return;
        };
        // Suppress every unpublished abandoned connect before publishing any SQ
        // tail, including tickets beyond this call's completion budget. Both
        // queues are bounded by ring configuration.
        for &(id, cancel) in self.book.abandoned.borrow().iter() {
            if cancel {
                if core.raw.discard_unsubmitted(id) {
                    let slot = &mut core.slots[id as u32 as usize];
                    if slot.generation == (id >> 32) as u32 {
                        if let Some(request) = &mut slot.request {
                            // Completion now belongs to a NOP, not to the
                            // original operation (notably ACCEPT's returned fd).
                            request.opcode = 0;
                        }
                    }
                }
            }
        }
        for _ in 0..self.config.completion_budget {
            let next = self.book.abandoned.borrow().front().copied();
            let Some((id, cancel)) = next else {
                break;
            };
            let index = id as u32 as usize;
            let slot = &mut core.slots[index];
            if slot.generation != (id >> 32) as u32 {
                self.book.abandoned.borrow_mut().pop_front();
                continue;
            }
            if let Some(request) = &mut slot.request {
                if cancel {
                    if request.cancel_queued() {
                        self.slab_queue.retain(|queued| *queued != id);
                    }
                }
                if cancel
                    && !matches!(request.state, State::Complete(_))
                    && !core.raw.discard_unsubmitted(id)
                {
                    if core.abandoned_cancel_pending || core.raw.space() == 0 {
                        break;
                    }
                    // This internal cancellation needs no request-table slot.
                    core.raw.push(abi::Sqe {
                        opcode: abi::CANCEL,
                        addr: id,
                        user_data: abi::CANCEL_ABANDONED,
                        ..Default::default()
                    });
                    core.abandoned_cancel_pending = true;
                }
                request.abandoned = true;
                if matches!(request.state, State::Complete(_)) {
                    self.book.abandoned.borrow_mut().pop_front();
                    drop(core.release(index));
                    continue;
                }
            }
            self.book.abandoned.borrow_mut().pop_front();
        }
    }

    /// Call only after all external sources have been armed and rechecked.
    /// Returns on software wakes, timeout or interruption, including empty wakes.
    pub fn wait(&mut self, deadline: Option<Instant>) -> io::Result<()> {
        self.abandoned();
        if self.submit_slab() {
            return Ok(());
        }
        let deadline = deadline.into_iter().chain(self.slab_deadline).min();
        let core = self.core.as_mut().ok_or_else(|| invalid("ring closed"))?;
        if self.stopping {
            return Err(invalid("ring stopping"));
        }
        core.reclaim_fixed(self.config.completion_budget)?;
        if !core.unused_fixed.borrow().is_empty() {
            return Ok(());
        }
        core.arm_wake();
        if core.cqe_next < core.cqes.len() || core.raw.ready() {
            return Ok(());
        }
        if !self.book.abandoned.borrow().is_empty() {
            // Submit to free SQ space, then let progress retry cancellation.
            // Sleeping here could wait on precisely the I/O we need to cancel.
            return core.raw.enter(false, None);
        }
        core.raw.enter(
            true,
            deadline.map(|d| d.saturating_duration_since(crate::environment::now())),
        )
    }

    /// Idempotent bounded shutdown. Failure leaves resources retained for a
    /// subsequent retry or the conservative Drop fallback.
    pub fn shutdown(&mut self) -> io::Result<()> {
        self.stopping = true;
        let Some(_) = &self.core else {
            return Ok(());
        };
        #[cfg(test)]
        if let RawRing::Sim(sim) = &mut self.core.as_mut().unwrap().raw {
            sim.quiesce();
        }
        let deadline = crate::environment::now()
            .checked_add(self.config.shutdown_timeout)
            .ok_or_else(|| invalid("shutdown timeout too large"))?;
        loop {
            self.progress()?;
            let core = self.core.as_mut().unwrap();
            let active = core.slots.iter().any(|s| {
                s.request
                    .as_ref()
                    .is_some_and(|r| !matches!(r.state, State::Complete(_)))
            });
            if !active
                && !core.wake_armed
                && !core.cancel_pending
                && !core.abandoned_cancel_pending
                && core.raw.pending() == 0
            {
                break;
            }
            if crate::environment::now() >= deadline {
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "io_uring shutdown did not quiesce",
                ));
            }
            if !core.cancel_pending && core.raw.space() > 0 {
                core.raw.push(abi::Sqe {
                    opcode: abi::CANCEL,
                    addr: 0,
                    op_flags: 1 | 4,
                    user_data: abi::CANCEL_ALL,
                    ..Default::default()
                });
                core.cancel_pending = true;
            }
            core.raw.enter(
                true,
                Some(
                    deadline
                        .saturating_duration_since(crate::environment::now())
                        .min(Duration::from_millis(10)),
                ),
            )?;
        }
        let core = self.core.as_mut().unwrap();
        if core.registered {
            core.raw.register(1, std::ptr::null(), 0)?;
            core.registered = false;
        }
        // No kernel payload access remains. Drop core even if a user's waker
        // panics while a completed, unpublished Fill is being dropped.
        drop(self.core.take());
        Ok(())
    }
}

fn result(res: i32) -> io::Result<usize> {
    if res < 0 {
        Err(io::Error::from_raw_os_error(res.saturating_neg()))
    } else {
        Ok(res as usize)
    }
}

macro_rules! collect {
    ($method:ident, $op:ty, $variant:ident) => {
        impl Ring {
            pub fn $method(
                &mut self,
                ticket: &mut Ticket<$op>,
            ) -> io::Result<Option<Completion<<$op as Operation>::Resource>>> {
                self.take(ticket).map(|done| {
                    done.map(|(r, res)| {
                        let Resource::$variant(resource) = r else {
                            unreachable!("sealed ticket/resource pairing")
                        };
                        Completion {
                            resource,
                            result: result(res),
                        }
                    })
                })
            }
        }
    };
}
impl Ring {
    /// Returns the exact submitted writable capability, never a stronger one.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{buffers::{Destination, Fill}, uring::*};
    /// fn escape(ring: &mut Ring, ticket: &mut Ticket<Read<Destination>>) {
    ///     let _: Option<Completion<Fill>> = ring.take_read(ticket).unwrap();
    /// }
    /// ```
    pub fn take_read<B: Writable>(
        &mut self,
        ticket: &mut Ticket<Read<B>>,
    ) -> io::Result<Option<Completion<B>>> {
        self.take(ticket).map(|done| {
            done.map(|(resource, res)| {
                let Resource::Writable(storage) = resource else {
                    unreachable!("sealed read resource")
                };
                Completion {
                    resource: B::from_storage(storage)
                        .unwrap_or_else(|_| unreachable!("sealed writable pairing")),
                    result: result(res),
                }
            })
        })
    }
}
collect!(take_write, Write, Buffer);
collect!(take_send_zc, SendZc, Buffer);
collect!(take_bytes, Bytes, Bytes);
collect!(take_page, PageIo, Page);
collect!(take_accept, Accept, Accepted);
impl Ring {
    pub fn take_control(
        &mut self,
        ticket: &mut Ticket<Control>,
    ) -> io::Result<Option<Completion<()>>> {
        self.take(ticket).map(|done| {
            done.map(|(_, res)| Completion {
                resource: (),
                result: result(res),
            })
        })
    }
    pub fn take_cancel(
        &mut self,
        ticket: &mut Ticket<Cancel>,
    ) -> io::Result<Option<Completion<()>>> {
        self.take(ticket).map(|done| {
            done.map(|(_, res)| Completion {
                resource: (),
                result: result(res),
            })
        })
    }
}

impl Core {
    fn reclaim_fixed(&mut self, budget: usize) -> io::Result<()> {
        for _ in 0..budget {
            let Some(index) = self.unused_fixed.borrow().front().copied() else {
                break;
            };
            let index = index as usize;
            if self.fixed[index]
                .as_ref()
                .is_some_and(|r| Rc::strong_count(r) == 1)
            {
                let fd = -1i32;
                let update = abi::FilesUpdate {
                    offset: index as u32,
                    reserved: 0,
                    fds: &fd as *const _ as u64,
                };
                self.raw
                    .register(6, (&update as *const abi::FilesUpdate).cast(), 1)?;
                self.fixed[index] = None;
            }
            self.unused_fixed.borrow_mut().pop_front();
        }
        Ok(())
    }

    fn release(&mut self, index: usize) -> Request {
        let slot = &mut self.slots[index];
        let request = slot.request.take().unwrap();
        if slot.generation < u32::MAX {
            self.free.push(index as u32);
        }
        request
    }
    fn arm_wake(&mut self) {
        let Some(fd) = &self.wake.fd else {
            return;
        };
        if !self.wake_armed && self.raw.space() > 0 {
            self.raw.push(abi::Sqe {
                opcode: abi::POLL,
                fd: fd.as_raw_fd(),
                op_flags: Readiness::Readable.mask(),
                user_data: abi::WAKE,
                ..Default::default()
            });
            self.wake_armed = true;
        }
    }
    fn complete(&mut self, cqe: abi::Cqe, metrics: &crate::metrics::Local) -> io::Result<()> {
        if cqe.user_data == abi::WAKE {
            self.wake_armed = false;
            if cqe.res >= 0 {
                self.wake.drain()?;
            } else if cqe.res != -libc::ECANCELED {
                return Err(io::Error::from_raw_os_error(-cqe.res));
            }
            return Ok(());
        }
        if cqe.user_data == abi::CANCEL_ALL {
            self.cancel_pending = false;
            if cqe.res < 0 && cqe.res != -libc::ENOENT && cqe.res != -libc::EALREADY {
                return Err(io::Error::from_raw_os_error(-cqe.res));
            }
            return Ok(());
        }
        if cqe.user_data == abi::CANCEL_ABANDONED {
            self.abandoned_cancel_pending = false;
            if cqe.res < 0 && cqe.res != -libc::ENOENT && cqe.res != -libc::EALREADY {
                return Err(io::Error::from_raw_os_error(-cqe.res));
            }
            return Ok(());
        }
        let index = cqe.user_data as u32 as usize;
        let slot = self
            .slots
            .get_mut(index)
            .ok_or_else(|| io::Error::other("invalid CQE identity"))?;
        if slot.generation != (cqe.user_data >> 32) as u32 {
            return Err(io::Error::other("stale CQE generation"));
        }
        let request = slot
            .request
            .as_mut()
            .ok_or_else(|| io::Error::other("CQE for vacant slot"))?;
        #[cfg(test)]
        if let RawRing::Sim(sim) = &mut self.raw {
            if let Some(file) = sim.accepted.remove(&cqe.user_data) {
                request.resource = Resource::Accepted(Some(file));
            }
        }
        let terminal = request.complete(cqe.res, cqe.flags)?;
        if cqe.flags & abi::NOTIF == 0 && cqe.res > 0 {
            if let Some(traffic) = request.metric_traffic.take() {
                metrics.bytes(traffic, cqe.res as u64);
            }
        }
        if terminal && request.abandoned {
            drop(self.release(index));
        }
        Ok(())
    }
}

impl Drop for Ring {
    fn drop(&mut self) {
        // A panic in a completed Fill's waker must not unwind through other
        // still-in-flight resources. Core itself is never automatically dropped
        // until shutdown has proved that all payload users are gone.
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| self.shutdown()));
        if let Some(core) = self.core.take() {
            std::mem::forget(core);
        }
        if let Err(payload) = outcome {
            std::mem::forget(payload);
        }
    }
}

/// Outcome of a bounded application/source batch. `runnable` includes budget
/// exhaustion; deadlines use the monotonic clock and are combined by minimum.
#[derive(Clone, Copy, Debug, Default)]
pub struct Work {
    pub runnable: bool,
    pub deadline: Option<Instant>,
}
impl Work {
    pub fn merge(&mut self, other: Self) {
        self.runnable |= other.runnable;
        self.deadline = match (self.deadline, other.deadline) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        };
    }
}

/// Application scheduler on the pinned worker. Poll ready tasks, inspect typed
/// tickets, and queue I/O up to `budget`; report runnable on budget exhaustion.
/// Task wakers can be made with `std::task::Waker::from(ring.wake_handle())`.
pub trait Application {
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work>;
    fn begin_drain(&mut self) {}
    fn drained(&self) -> bool {
        true
    }
    /// Stop admission and drop/cancel application tickets. The driver subsequently
    /// drains the ring; application-owned raw I/O needs its own safe teardown.
    fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()>;
}

/// An independently owned external completion source, e.g. an RDMA RNIC.
///
/// For rdma-core, `poll` must fairly poll all owned CQs, drain nonblocking
/// completion-channel events and acknowledge each event, and service async
/// events. `arm` calls ibv_req_notify_cq and installs a one-shot [`Ring::poll_fd`]
/// on an owned/duplicated channel descriptor. The driver then polls again before
/// sleeping. FD readiness is only a prompt to poll the CQ; it is not a work
/// completion. Keep CQ notification arming separate from FD poll rearming.
/// Unsignaled remote writes need an explicit protocol notification to wake a CPU.
///
/// One owner per CQ/channel; multiple sources/RNICs per worker are supported.
/// This is a safe scheduling trait, not a memory-safety contract. Implementations
/// must retain each MR's MemoryLease and each in-flight Fill/Buffer until the NIC
/// is proven quiescent, even if shutdown is skipped, fails, or panics. The ring's
/// cancellation and deregistration cannot prove NIC quiescence.
pub trait CompletionSource {
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work>;
    fn arm(&mut self, ring: &mut Ring) -> io::Result<()>;
    fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()>;
}

/// Composite worker driver: bounded round-robin sources, application scheduling,
/// notification arm/recheck, then exactly one io_uring sleep decision.
///
/// ```no_run
/// use racer_dataplane::{buffers, uring, workers};
/// use std::{io, num::NonZeroUsize, sync::Arc};
/// struct App;
/// impl uring::Application for App {
///     fn poll(&mut self, _: &mut uring::Ring, _: usize) -> io::Result<uring::Work> {
///         Ok(uring::Work::default())
///     }
///     fn shutdown(&mut self, _: &mut uring::Ring) -> io::Result<()> { Ok(()) }
/// }
/// # fn start() -> io::Result<()> {
/// let pools = Arc::new(buffers::Pools::new(buffers::Config::new(
///     NonZeroUsize::new(32).unwrap(),
/// )));
/// let workers = workers::Workers::start(workers::Config::default(), move |placement| {
///     let pool = pools.for_worker(placement)?;
///     let ring = uring::Ring::new(placement, pool, uring::Config::default())?;
///     uring::Driver::new(ring, App, 128)
/// })?;
/// # drop(workers);
/// # Ok(()) }
/// ```
pub struct Driver<A: Application> {
    lifecycle: Option<(Arc<crate::lifecycle::Lifecycle>, usize)>,
    metrics_deadline: Option<Instant>,
    ring: Ring,
    application: A,
    sources: Vec<Box<dyn CompletionSource>>,
    first: usize,
    budget: usize,
    stopped: bool,
    quiesced: bool,
    #[cfg(test)]
    parked: bool,
    #[cfg(test)]
    deadline: Option<Instant>,
}
impl<A: Application> Driver<A> {
    pub fn new(ring: Ring, application: A, budget: usize) -> io::Result<Self> {
        if budget == 0 {
            return Err(invalid("driver budget must be nonzero"));
        }
        Ok(Self {
            lifecycle: None,
            ring,
            metrics_deadline: None,
            application,
            sources: Vec::new(),
            first: 0,
            budget,
            stopped: false,
            quiesced: false,
            #[cfg(test)]
            parked: false,
            #[cfg(test)]
            deadline: None,
        })
    }
    /// Setup-time registration on the owning worker.
    pub fn with_lifecycle(mut self, life: Arc<crate::lifecycle::Lifecycle>, worker: usize) -> Self {
        self.lifecycle = Some((life, worker));
        self
    }
    pub fn add_source(&mut self, source: impl CompletionSource + 'static) {
        self.sources.push(Box::new(source));
    }
    fn poll(&mut self) -> io::Result<Work> {
        let mut work = Work {
            runnable: self.ring.progress()?,
            deadline: None,
        };
        for offset in 0..self.sources.len() {
            let index = (self.first + offset) % self.sources.len();
            work.merge(self.sources[index].poll(&mut self.ring, self.budget)?);
        }
        work.merge(self.application.poll(&mut self.ring, self.budget)?);
        work.merge(Work {
            runnable: false,
            deadline: self.ring.slab_deadline(),
        });
        work.merge(self.ring.metrics.poll(&mut self.metrics_deadline));
        if let Some((life, worker)) = &self.lifecycle {
            life.progress(*worker);
            work.merge(Work {
                runnable: false,
                deadline: Some(crate::environment::now() + crate::lifecycle::HEARTBEAT),
            });
        }
        Ok(work)
    }
}
impl<A: Application> workers::Driver for Driver<A> {
    type Wake = Wake;
    fn begin_drain(&mut self) {
        self.application.begin_drain();
    }
    fn drained(&self) -> bool {
        self.application.drained()
    }
    fn wake_handle(&self) -> Arc<Wake> {
        self.ring.wake_handle()
    }
    fn turn(&mut self) -> io::Result<()> {
        if self.stopped {
            return Err(invalid("driver stopped"));
        }
        #[cfg(test)]
        let _process = match &self
            .ring
            .core
            .as_ref()
            .ok_or_else(|| invalid("ring closed"))?
            .raw
        {
            RawRing::Sim(sim) if sim.world.managed() => {
                if !sim.world.is_current(sim.process) {
                    return Err(invalid("retired simulated driver"));
                }
                Some((sim.world.enter(), sim.world.scoped_process(sim.process)))
            }
            _ => None,
        };
        #[cfg(test)]
        {
            self.parked = false;
            self.deadline = None;
        }
        let mut work = self.poll()?;
        if !self.sources.is_empty() {
            self.first = (self.first + 1) % self.sources.len();
        }
        if work.runnable {
            return Ok(());
        }
        for source in &mut self.sources {
            source.arm(&mut self.ring)?;
        }
        work.merge(self.poll()?);
        if !work.runnable {
            #[cfg(test)]
            if let RawRing::Sim(sim) = &self.ring.core.as_ref().unwrap().raw
                && sim.world.managed()
            {
                self.deadline = work.deadline;
                self.parked = !self.ring.wake_handle().pending.load(Ordering::Acquire);
                return Ok(());
            }
            self.ring.wait(work.deadline)?;
        }
        Ok(())
    }
    fn shutdown(&mut self) -> io::Result<()> {
        if self.quiesced {
            return Ok(());
        }
        self.stopped = true;
        #[cfg(test)]
        if let Some(core) = &mut self.ring.core
            && let RawRing::Sim(sim) = &mut core.raw
            && sim.world.managed()
        {
            sim.draining = true;
            sim.quiesce();
        }
        #[cfg(test)]
        let _process = self.ring.core.as_ref().and_then(|core| match &core.raw {
            RawRing::Sim(sim) => Some((sim.world.enter(), sim.world.scoped_process(sim.process))),
            _ => None,
        });
        let mut error = self.application.shutdown(&mut self.ring).err();
        for source in &mut self.sources {
            if let Err(e) = source.shutdown(&mut self.ring) {
                error.get_or_insert(e);
            }
        }
        #[cfg(test)]
        if let Some(core) = &self.ring.core
            && let RawRing::Sim(sim) = &core.raw
            && sim.world.managed()
        {
            sim.world.finish_process_tasks(sim.process);
        }
        if let Err(e) = self.ring.shutdown() {
            error.get_or_insert(e);
        }
        self.ring.metrics.publish();
        match error {
            Some(e) => Err(e),
            None => {
                self.quiesced = true;
                Ok(())
            }
        }
    }
}

#[cfg(test)]
impl<A: Application> Driver<A> {
    pub(crate) fn application(&self) -> &A {
        &self.application
    }
    pub(crate) fn application_mut(&mut self) -> &mut A {
        &mut self.application
    }
    pub(crate) fn ring_mut(&mut self) -> &mut Ring {
        &mut self.ring
    }
    pub(crate) fn parts_mut(&mut self) -> (&mut A, &mut Ring) {
        (&mut self.application, &mut self.ring)
    }
    pub(crate) fn parked(&self) -> bool {
        self.parked
    }
    pub(crate) fn deadline(&self) -> Option<Instant> {
        self.deadline
    }
    /// Observes readiness without consuming wakes. Stale incarnations are fenced.
    pub(crate) fn ready(&self) -> bool {
        if self.stopped {
            return false;
        }
        let Some(core) = &self.ring.core else {
            return false;
        };
        let RawRing::Sim(sim) = &core.raw else {
            return true;
        };
        if !sim.world.is_current(sim.process) {
            return false;
        }
        !self.parked
            || !self.ring.book.abandoned.borrow().is_empty()
            || !core.unused_fixed.borrow().is_empty()
            || core.wake.pending.load(Ordering::Acquire)
            || core.cqe_next < core.cqes.len()
            || core.raw.ready()
            || self.deadline.is_some_and(|d| d <= sim.world.now())
            || sim.next_tick().is_some_and(|t| t <= sim.world.tick())
    }
    pub(crate) fn simulated_crash(&mut self) {
        self.stopped = true;
        self.quiesced = true;
        self.ring.simulated_crash();
    }
}

#[cfg(test)]
#[path = "../tests/execution/uring.rs"]
mod tests;

#[cfg(test)]
pub(crate) fn test_zc_retirement() {
    tests::zc_retirement();
}

/// Raw Linux queue ABI and mapping ownership. Only the typed uring owner calls
/// this backend; submitted pointer lifetimes remain its responsibility.
pub(crate) mod sys {
    use std::{
        io,
        marker::PhantomData,
        os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd},
        ptr::NonNull,
        rc::Rc,
        sync::atomic::{AtomicU32, Ordering},
        time::Duration,
    };
    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidInput, message)
    }
    pub(crate) mod abi {
        #[repr(C)]
        #[derive(Clone, Copy, Default)]
        pub(crate) struct Sqe {
            pub opcode: u8,
            pub flags: u8,
            pub ioprio: u16,
            pub fd: i32,
            pub off: u64,
            pub addr: u64,
            pub len: u32,
            pub op_flags: u32,
            pub user_data: u64,
            pub buf_index: u16,
            pub personality: u16,
            pub file_index: u32,
            pub addr3: u64,
            pub pad: u64,
        }
        #[repr(C)]
        #[derive(Clone, Copy, Default)]
        pub(crate) struct Cqe {
            pub user_data: u64,
            pub res: i32,
            pub flags: u32,
        }
        #[repr(C)]
        #[derive(Default)]
        pub(super) struct Offsets {
            pub head: u32,
            pub tail: u32,
            pub mask: u32,
            pub entries: u32,
            pub flags_or_overflow: u32,
            pub dropped_or_cqes: u32,
            pub array_or_flags: u32,
            pub reserved: u32,
            pub user_addr: u64,
        }
        #[repr(C)]
        #[derive(Default)]
        pub(super) struct Params {
            pub sq_entries: u32,
            pub cq_entries: u32,
            pub flags: u32,
            pub sq_thread_cpu: u32,
            pub sq_thread_idle: u32,
            pub features: u32,
            pub wq_fd: u32,
            pub reserved: [u32; 3],
            pub sq: Offsets,
            pub cq: Offsets,
        }
        #[repr(C)]
        #[derive(Default)]
        pub(super) struct Timespec {
            pub sec: i64,
            pub nsec: i64,
        }
        #[repr(C)]
        #[derive(Default)]
        pub(super) struct GetEvents {
            pub sigmask: u64,
            pub sigmask_sz: u32,
            pub pad: u32,
            pub ts: u64,
        }
        #[repr(C)]
        #[derive(Default)]
        pub(super) struct ProbeOp {
            pub op: u8,
            pub reserved: u8,
            pub flags: u16,
            pub reserved2: u32,
        }
        #[repr(C)]
        pub(super) struct Probe {
            pub last_op: u8,
            pub len: u8,
            pub reserved: u16,
            pub reserved2: [u32; 3],
            pub ops: [ProbeOp; 64],
        }
        #[repr(C)]
        pub(crate) struct FilesUpdate {
            pub offset: u32,
            pub reserved: u32,
            pub fds: u64,
        }
        pub(crate) const READ_FIXED: u8 = 4;
        pub(crate) const WRITE_FIXED: u8 = 5;
        pub(crate) const POLL: u8 = 6;
        pub(crate) const ACCEPT: u8 = 13;
        pub(crate) const CANCEL: u8 = 14;
        pub(crate) const CONNECT: u8 = 16;
        pub(crate) const READ: u8 = 22;
        pub(crate) const SEND: u8 = 26;
        pub(crate) const RECV: u8 = 27;
        pub(crate) const SEND_ZC: u8 = 47;
        pub(crate) const MORE: u32 = 2;
        pub(crate) const NOTIF: u32 = 8;
        pub(crate) const WAKE: u64 = 0;
        pub(crate) const CANCEL_ALL: u64 = u64::MAX;
        pub(crate) const CANCEL_ABANDONED: u64 = 1;
        const _: () = {
            assert!(size_of::<Sqe>() == 64);
            assert!(align_of::<Sqe>() == 8);
            assert!(std::mem::offset_of!(Sqe, off) == 8);
            assert!(std::mem::offset_of!(Sqe, addr) == 16);
            assert!(std::mem::offset_of!(Sqe, len) == 24);
            assert!(std::mem::offset_of!(Sqe, user_data) == 32);
            assert!(std::mem::offset_of!(Sqe, buf_index) == 40);
            assert!(std::mem::offset_of!(Sqe, addr3) == 48);
            assert!(size_of::<Cqe>() == 16);
            assert!(size_of::<Offsets>() == 40);
            assert!(size_of::<Params>() == 120);
            assert!(std::mem::offset_of!(Params, sq) == 40);
            assert!(std::mem::offset_of!(Params, cq) == 80);
            assert!(size_of::<GetEvents>() == 24);
            assert!(size_of::<Timespec>() == 16);
            assert!(std::mem::offset_of!(Probe, ops) == 16);
            assert!(size_of::<FilesUpdate>() == 16);
        };
    }
    struct Mapping {
        ptr: NonNull<u8>,
        len: usize,
    }
    impl Mapping {
        fn new(fd: RawFd, len: usize, offset: libc::off_t) -> io::Result<Self> {
            if len == 0 || len > isize::MAX as usize {
                return Err(invalid("invalid ring mapping length"));
            }
            // SAFETY: new shared mapping of the ring's kernel-provided region.
            let ptr = unsafe {
                libc::mmap(
                    std::ptr::null_mut(),
                    len,
                    libc::PROT_READ | libc::PROT_WRITE,
                    libc::MAP_SHARED | libc::MAP_POPULATE,
                    fd,
                    offset,
                )
            };
            if ptr == libc::MAP_FAILED {
                return Err(io::Error::last_os_error());
            }
            let Some(ptr) = NonNull::new(ptr.cast()) else {
                // SAFETY: release the successful but unusable null mapping.
                unsafe {
                    libc::munmap(ptr, len);
                }
                return Err(io::Error::other("null ring mapping"));
            };
            Ok(Self { ptr, len })
        }
        fn at<T>(&self, offset: u32, count: u32) -> io::Result<NonNull<T>> {
            let offset = offset as usize;
            let len = (count as usize)
                .checked_mul(size_of::<T>())
                .and_then(|len| offset.checked_add(len))
                .ok_or_else(|| invalid("ring layout overflow"))?;
            if len > self.len || !offset.is_multiple_of(align_of::<T>()) {
                return Err(invalid("invalid kernel ring layout"));
            }
            // SAFETY: checked range and alignment, mmap base is page aligned.
            Ok(unsafe { NonNull::new_unchecked(self.ptr.as_ptr().add(offset).cast()) })
        }
    }
    impl Drop for Mapping {
        fn drop(&mut self) {
            // SAFETY: exclusive mapping owner; kernel has its own mapping references.
            unsafe {
                libc::munmap(self.ptr.as_ptr().cast(), self.len);
            }
        }
    }
    pub(crate) struct KernelRing {
        fd: OwnedFd,
        _rings: Mapping,
        _sqes: Mapping,
        sq_head: NonNull<AtomicU32>,
        sq_tail: NonNull<AtomicU32>,
        sq_flags: NonNull<AtomicU32>,
        sq_dropped: NonNull<AtomicU32>,
        cq_head: NonNull<AtomicU32>,
        cq_tail: NonNull<AtomicU32>,
        cq_overflow: NonNull<AtomicU32>,
        array: NonNull<u32>,
        sqes: NonNull<abi::Sqe>,
        cqes: NonNull<abi::Cqe>,
        sq_size: u32,
        cq_size: u32,
        tail: u32,
        head: u32,
        _local: PhantomData<Rc<()>>,
    }
    fn load(ptr: NonNull<AtomicU32>) -> u32 {
        // SAFETY: callers use validated, live, aligned shared ring fields.
        unsafe { ptr.as_ref().load(Ordering::Acquire) }
    }
    fn store(ptr: NonNull<AtomicU32>, value: u32) {
        // SAFETY: only this worker writes the userspace-owned index.
        unsafe {
            ptr.as_ref().store(value, Ordering::Release);
        }
    }
    impl KernelRing {
        pub(crate) fn new(entries: u32) -> io::Result<Self> {
            let mut p = abi::Params {
                // CQSIZE | SUBMIT_ALL | TASKRUN_FLAG | SINGLE_ISSUER | DEFER_TASKRUN
                flags: (1 << 3) | (1 << 7) | (1 << 9) | (1 << 12) | (1 << 13),
                cq_entries: entries
                    .checked_mul(2)
                    .ok_or_else(|| invalid("ring too large"))?,
                ..Default::default()
            };
            // SAFETY: writable ABI-sized params; no borrowed memory survives setup.
            let fd = unsafe { libc::syscall(libc::SYS_io_uring_setup, entries, &mut p) };
            if fd < 0 {
                return Err(io::Error::last_os_error());
            }
            // SAFETY: setup returned a fresh owned descriptor.
            let fd = unsafe { OwnedFd::from_raw_fd(fd as RawFd) };
            // SINGLE_MMAP | NODROP | FAST_POLL | EXT_ARG
            let required = 1 | 2 | (1 << 5) | (1 << 8);
            if p.features & required != required {
                return Err(io::Error::new(
                    io::ErrorKind::Unsupported,
                    "io_uring requires SINGLE_MMAP, NODROP, FAST_POLL and EXT_ARG",
                ));
            }
            if !p.sq_entries.is_power_of_two()
                || !p.cq_entries.is_power_of_two()
                || p.sq_entries > 32768
                || p.cq_entries > 65536
            {
                return Err(invalid("invalid kernel queue sizes"));
            }
            let sq_len = p.sq.array_or_flags as usize + p.sq_entries as usize * 4;
            let cq_len =
                p.cq.dropped_or_cqes as usize + p.cq_entries as usize * size_of::<abi::Cqe>();
            let rings = Mapping::new(fd.as_raw_fd(), sq_len.max(cq_len), 0)?;
            let sqes = Mapping::new(
                fd.as_raw_fd(),
                p.sq_entries as usize * size_of::<abi::Sqe>(),
                0x10000000,
            )?;
            let raw = Self {
                sq_head: rings.at(p.sq.head, 1)?,
                sq_tail: rings.at(p.sq.tail, 1)?,
                sq_flags: rings.at(p.sq.flags_or_overflow, 1)?,
                sq_dropped: rings.at(p.sq.dropped_or_cqes, 1)?,
                cq_head: rings.at(p.cq.head, 1)?,
                cq_tail: rings.at(p.cq.tail, 1)?,
                cq_overflow: rings.at(p.cq.flags_or_overflow, 1)?,
                array: rings.at(p.sq.array_or_flags, p.sq_entries)?,
                cqes: rings.at(p.cq.dropped_or_cqes, p.cq_entries)?,
                sqes: sqes.at(0, p.sq_entries)?,
                sq_size: p.sq_entries,
                cq_size: p.cq_entries,
                tail: 0,
                head: 0,
                fd,
                _rings: rings,
                _sqes: sqes,
                _local: PhantomData,
            };
            // SAFETY: all zero is a valid probe; kernel writes at most 64 entries.
            let mut probe: abi::Probe = unsafe { std::mem::zeroed() };
            raw.register(8, &mut probe as *mut _ as *const libc::c_void, 64)?;
            for op in [
                abi::READ_FIXED,
                abi::WRITE_FIXED,
                abi::POLL,
                abi::ACCEPT,
                abi::CANCEL,
                abi::CONNECT,
                abi::READ,
                abi::SEND,
                abi::RECV,
                abi::SEND_ZC,
            ] {
                if !probe.ops[..usize::from(probe.len).min(64)]
                    .iter()
                    .any(|entry| entry.op == op && entry.flags & 1 != 0)
                {
                    return Err(io::Error::new(
                        io::ErrorKind::Unsupported,
                        format!("missing io_uring opcode {op}"),
                    ));
                }
            }
            Ok(raw)
        }
        pub(crate) fn register(
            &self,
            op: u32,
            arg: *const libc::c_void,
            count: u32,
        ) -> io::Result<()> {
            // SAFETY: private callers provide the corresponding live ABI argument.
            let result = unsafe {
                libc::syscall(
                    libc::SYS_io_uring_register,
                    self.fd.as_raw_fd(),
                    op,
                    arg,
                    count,
                )
            };
            if result < 0 {
                Err(io::Error::last_os_error())
            } else {
                Ok(())
            }
        }
        pub(crate) fn space(&self) -> u32 {
            self.sq_size - self.tail.wrapping_sub(load(self.sq_head))
        }
        pub(crate) fn push(&mut self, sqe: abi::Sqe) {
            assert!(self.space() > 0);
            let index = (self.tail & (self.sq_size - 1)) as usize;
            // SAFETY: free SQ slot, only this worker writes it; tail publication follows.
            unsafe {
                self.sqes.as_ptr().add(index).write(sqe);
                self.array.as_ptr().add(index).write(index as u32);
            }
            self.tail = self.tail.wrapping_add(1);
        }
        pub(crate) fn pending(&self) -> u32 {
            self.tail.wrapping_sub(load(self.sq_head))
        }
        pub(crate) fn discard_unsubmitted(&mut self, id: u64) -> bool {
            // Only touch SQEs whose tail has never been published to the kernel.
            let mut cursor = load(self.sq_tail);
            while cursor != self.tail {
                let index = (cursor & (self.sq_size - 1)) as usize;
                // SAFETY: unpublished slots are exclusively owned by this worker.
                let sqe = unsafe { &mut *self.sqes.as_ptr().add(index) };
                if sqe.user_data == id {
                    // NOP still produces a terminal CQE, preserving resource lifetime.
                    *sqe = abi::Sqe {
                        user_data: id,
                        ..Default::default()
                    };
                    return true;
                }
                cursor = cursor.wrapping_add(1);
            }
            false
        }
        pub(crate) fn ready(&self) -> bool {
            self.head != load(self.cq_tail)
        }
        pub(crate) fn needs_enter(&self) -> bool {
            self.pending() != 0 || load(self.sq_flags) & (2 | 4) != 0
        }
        // Always GETEVENTS, including nonblocking calls, to run deferred task work.
        pub(crate) fn enter(&mut self, wait: bool, timeout: Option<Duration>) -> io::Result<()> {
            store(self.sq_tail, self.tail);
            let ts = timeout.map(|d| abi::Timespec {
                sec: d.as_secs().min(i64::MAX as u64) as i64,
                nsec: d.subsec_nanos() as i64,
            });
            let args = abi::GetEvents {
                ts: ts.as_ref().map_or(0, |ts| ts as *const _ as u64),
                ..Default::default()
            };
            // SAFETY: all arguments live through this synchronous syscall; EXT_ARG
            // timeouts are copied during enter, never retained in an asynchronous SQE.
            let result = unsafe {
                libc::syscall(
                    libc::SYS_io_uring_enter,
                    self.fd.as_raw_fd(),
                    self.pending(),
                    u32::from(wait),
                    1u32 | 8u32,
                    &args,
                    size_of::<abi::GetEvents>(),
                )
            };
            if result < 0 {
                let error = io::Error::last_os_error();
                match error.raw_os_error() {
                    Some(libc::EINTR | libc::ETIME | libc::EBUSY | libc::EAGAIN) => return Ok(()),
                    _ => return Err(error),
                }
            }
            self.check_loss()
        }
        fn check_loss(&self) -> io::Result<()> {
            if load(self.sq_dropped) != 0 || load(self.cq_overflow) != 0 {
                Err(io::Error::other(
                    "io_uring lost an SQE/CQE; resources must remain pinned",
                ))
            } else {
                Ok(())
            }
        }
        pub(crate) fn reap(&mut self, output: &mut Vec<abi::Cqe>, budget: usize) -> io::Result<()> {
            self.check_loss()?;
            let count = load(self.cq_tail).wrapping_sub(self.head) as usize;
            if count > self.cq_size as usize {
                return Err(io::Error::other("invalid CQ occupancy"));
            }
            for _ in 0..count.min(budget) {
                let index = (self.head & (self.cq_size - 1)) as usize;
                // SAFETY: acquire observed kernel publication; copying before releasing head.
                output.push(unsafe { self.cqes.as_ptr().add(index).read() });
                self.head = self.head.wrapping_add(1);
            }
            store(self.cq_head, self.head);
            Ok(())
        }
    }
}
