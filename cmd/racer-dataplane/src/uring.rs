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

mod completion;
mod driver;
mod files;
mod registration;
mod request;
/// Raw Linux queue ABI and mapping ownership. Only the typed uring owner calls
/// this backend; submitted pointer lifetimes remain its responsibility.
pub(crate) mod sys;

pub use driver::{Application, CompletionSource, Driver, Work};
pub use files::FixedFile;
use files::FixedRegistration;
pub(crate) use registration::Inbound;
use registration::inbound_reserve;

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
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
pub struct File(Rc<OwnedFd>, crate::slab_io::Io);
impl File {
    pub(crate) fn pipe() -> io::Result<(Self, Self)> {
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
        Self(Rc::new(fd), crate::slab_io::Io::default())
    }
    pub(crate) fn with_slab_io(mut self, io: crate::slab_io::Io) -> Self {
        self.1 = io;
        self
    }
    pub(crate) fn slab_io(&self) -> &crate::slab_io::Io {
        &self.1
    }
    fn raw_id(&self) -> i32 {
        self.0.as_raw_fd()
    }
    pub(crate) fn shutdown_socket(&self) {
        // SAFETY: owned live OS descriptor.
        unsafe {
            libc::shutdown(self.as_fd().as_raw_fd(), libc::SHUT_RDWR);
        }
    }
}
impl AsFd for File {
    fn as_fd(&self) -> BorrowedFd<'_> {
        self.0.as_fd()
    }
}

pub(crate) struct Identity;

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

/// Validated nonempty subrange of a 64 MiB slot. Published length is additionally
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
struct Slot {
    generation: u32,
    request: Option<Request>,
}

struct Core {
    raw: KernelRing,
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
    fd: OwnedFd,
}
impl Wake {
    pub(crate) fn new() -> io::Result<Self> {
        // SAFETY: no pointer arguments; fresh descriptor on success.
        let fd = unsafe { libc::eventfd(0, libc::EFD_NONBLOCK | libc::EFD_CLOEXEC) };
        if fd < 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(Self {
                fd: unsafe { OwnedFd::from_raw_fd(fd) },
            })
        }
    }
    fn drain(&self) -> io::Result<()> {
        let fd = &self.fd;
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
        let fd = &self.fd;
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
        let raw = KernelRing::new(config.entries)?;
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
}

impl Ring {
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
                Descriptor::Fixed(file) => file.slab_io(),
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

    #[cfg(test)]
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

    /// Progress a bounded CQ/abandonment batch without sleeping. Returns true if
    /// completions were processed, more work is known or a budget was exhausted;
    /// a composite driver must stay awake and let its caller observe stop flags.
    pub fn progress(&mut self) -> io::Result<bool> {
        self.abandoned();
        let slab_runnable = self.submit_slab();
        let core = self.core.as_mut().ok_or_else(|| invalid("ring closed"))?;

        if !self.stopping {
            core.arm_wake();
        }

        if core.raw.needs_enter() {
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

        Ok(slab_runnable
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
                    request.submit_admitted(&mut core.raw, charge);
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
    fn release(&mut self, index: usize) -> Request {
        let slot = &mut self.slots[index];
        let request = slot.request.take().unwrap();
        if slot.generation < u32::MAX {
            self.free.push(index as u32);
        }
        request
    }
    fn arm_wake(&mut self) {
        let fd = &self.wake.fd;
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

#[cfg(test)]
#[path = "../tests/execution/uring.rs"]
mod tests;
