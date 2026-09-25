//! Worker-local io_uring ownership and completion fences.
//!
//! Before submission, the reactor must own the buffer, FD, and associated leases
//! in its in-flight table, independently of the waiting future. Dropping a future
//! only abandons its result; it cannot release submitted resources. Cancellation
//! requests do not release them: original and cancellation completion accounting
//! must both finish. Shutdown must fence kernel access before dropping the table.
//! The worker drives CQEs explicitly. Operation futures never run an executor.
//!
//! Degraded shutdown: a fatal driver error during Drop cannot establish a kernel
//! fence. In that case the bounded ring and its resource owners are deliberately
//! leaked for memory safety; shutdown is not successfully fenced. Explicit worker
//! shutdown must keep driving its fence and report failures rather than rely on Drop.

use super::{
    admission::{Admission, Reservation},
    deadline::RequestScope,
};
use crate::{
    error::{Error, Operation, Result},
    model::limits::ResourceClass,
};
use io_uring::{IoUring, opcode, squeue, types};
// Control-owned filesystem extension; shares this reactor's completion fences.
#[path = "filesystem.rs"]
pub mod filesystem;
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    future::Future,
    net::SocketAddr,
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd},
        unix::ffi::OsStrExt,
    },
    path::PathBuf,
    pin::Pin,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll, Waker},
    time::Duration,
};

pub struct Reactor {
    admission: Rc<Admission>,
    state: RefCell<State>,
}
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct IoId(pub u64);

/// An owned address; the encoded sockaddr remains pinned in the in-flight owner.
#[derive(Clone, Debug)]
pub enum SocketAddress {
    Inet(SocketAddr),
    Unix(PathBuf),
}

const CANCEL_BIT: u64 = 1 << 63;
const MAX_WAIT: Duration = Duration::from_millis(10);

struct State {
    ring: Option<IoUring>,
    wake: Option<Arc<OwnedFd>>,
    ring_reservation: Option<Reservation>,
    entries: BTreeMap<IoId, Entry>,
    next: u64,
    scan_after: IoId,
    stopped: bool,
    fence_waiters: BTreeMap<(Option<IoId>, u64), FenceWaiter>,
    next_waiter: u64,
}

struct FenceWaiter {
    waker: Waker,
    _reservation: Reservation,
}

struct Fence<'a> {
    reactor: &'a Reactor,
    target: Option<IoId>,
    registration: Option<u64>,
}

impl Future for Fence<'_> {
    type Output = Result<()>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let mut state = this.reactor.state.borrow_mut();
        let pending = match this.target {
            Some(id) => match state.entries.get_mut(&id) {
                Some(entry) => {
                    entry.cancel_reason.get_or_insert(Error::Cancelled);
                    true
                }
                None => false,
            },
            None => {
                state.stopped = true;
                !state.entries.is_empty()
            }
        };
        if !pending {
            if let Some(id) = this.registration.take() {
                state.fence_waiters.remove(&(this.target, id));
            }
            return Poll::Ready(Ok(()));
        }
        if let Some(id) = this.registration {
            let waiter = state
                .fence_waiters
                .get_mut(&(this.target, id))
                .expect("pending fence registration");
            waiter.waker.clone_from(cx.waker());
        } else {
            // Separate bounded control registrations support independent callers,
            // including callers using the same executor waker. Drop removes only
            // its own registration. No quota is needed to submit cancellation.
            if state.fence_waiters.len() >= this.reactor.admission.limits().queue_entries.get() {
                return Poll::Ready(Err(Error::Overloaded));
            }
            let Some(next) = state.next_waiter.checked_add(1) else {
                return Poll::Ready(Err(Error::Overloaded));
            };
            let reservation = this.reactor.admission.reserve_completion(
                None,
                ResourceClass::RequestContext,
                std::mem::size_of::<FenceWaiter>(),
            )?;
            let id = state.next_waiter;
            state.next_waiter = next;
            state.fence_waiters.insert(
                (this.target, id),
                FenceWaiter {
                    waker: cx.waker().clone(),
                    _reservation: reservation,
                },
            );
            this.registration = Some(id);
        }
        Poll::Pending
    }
}

impl Drop for Fence<'_> {
    fn drop(&mut self) {
        if let Some(id) = self.registration {
            self.reactor
                .state
                .borrow_mut()
                .fence_waiters
                .remove(&(self.target, id));
        }
    }
}

/// Cloneable cross-thread wake endpoint for worker command/crypto producers.
#[derive(Clone)]
pub struct ReactorWake {
    fd: Arc<OwnedFd>,
}

impl ReactorWake {
    pub fn wake(&self) -> Result<()> {
        let value = 1u64;
        loop {
            // SAFETY: eventfd consumes this initialized, stack-local u64 synchronously.
            let result =
                unsafe { libc::write(self.fd.as_raw_fd(), (&value as *const u64).cast(), 8) };
            if result == 8 {
                return Ok(());
            }
            match std::io::Error::last_os_error().raw_os_error() {
                Some(libc::EINTR) => continue,
                Some(libc::EAGAIN) => return Ok(()), // Already readable.
                _ => return Err(Error::Io),
            }
        }
    }
}

struct Signal {
    abandoned: Cell<bool>,
    waker: RefCell<Option<Waker>>,
}

struct Reply<T> {
    result: Option<Result<T>>,
    // Charge completed-but-unconsumed results as well as submitted work.
    _reservation: Rc<Reservation>,
}

struct Waiting<T> {
    reply: Rc<RefCell<Reply<T>>>,
    signal: Rc<Signal>,
}

impl<T> Future for Waiting<T> {
    type Output = Result<T>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        if let Some(result) = self.reply.borrow_mut().result.take() {
            Poll::Ready(result)
        } else {
            *self.signal.waker.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }
    }
}

impl<T> Drop for Waiting<T> {
    fn drop(&mut self) {
        self.signal.abandoned.set(true);
        self.signal.waker.borrow_mut().take();
    }
}

enum KernelResult {
    Value(i32),
    Accepted(OwnedFd),
}

impl KernelResult {
    fn value(self) -> Result<i32> {
        match self {
            Self::Value(value) if value >= 0 => Ok(value),
            Self::Value(value) if value == -libc::ECANCELED => Err(Error::Cancelled),
            _ => Err(Error::Io),
        }
    }
}

struct Entry {
    // The finish closure owns every FD, buffer, lease and sockaddr backing.
    finish: Box<dyn FnOnce(Result<KernelResult>) -> Option<Waker>>,
    signal: Rc<Signal>,
    scope: RequestScope,
    original: Option<KernelResult>,
    accept: bool,
    cancel_reason: Option<Error>,
    cancel_sent: bool,
    cancel_done: bool,
}

impl Entry {
    fn fenced(&self) -> bool {
        self.original.is_some() && (!self.cancel_sent || self.cancel_done)
    }
    fn finish(mut self) -> Option<Waker> {
        let original = self.original.take().expect("original CQE fenced");
        let result = match self.cancel_reason {
            Some(error) => {
                drop(original);
                Err(error)
            }
            None => Ok(original),
        };
        (self.finish)(result)
    }
}

pub(crate) mod sealed {
    pub trait Sealed {}
}

/// An exclusively owned, stable backing allocation with an independent lifetime.
///
/// Only audited crate types may implement this trait. Moving the owner or calling
/// either accessor must not relocate or resize its backing allocation. Accessors
/// expose the same initialized region; there must be no independently accessible
/// mutable aliases. Ownership includes the allocation's quota reservation. Neither
/// the allocation nor that reservation may be freed/recycled before the final fence.
/// `'static` excludes request-scoped borrows; it does not require leaking memory.
///
/// Inline storage is address-unstable when its owner moves and cannot opt in:
/// ```compile_fail
/// use racer_dataplane::{error::Result, runtime::reactor::IoBuffer};
/// struct Inline([u8; 16]);
/// impl IoBuffer for Inline {
///     fn bytes(&self) -> Result<&[u8]> { Ok(&self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(&mut self.0) }
/// }
/// ```
/// Borrowed storage cannot opt in either:
/// ```compile_fail
/// use racer_dataplane::{error::Result, runtime::reactor::IoBuffer};
/// struct Borrowed<'a>(&'a mut [u8]);
/// impl IoBuffer for Borrowed<'_> {
///     fn bytes(&self) -> Result<&[u8]> { Ok(self.0) }
///     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(self.0) }
/// }
/// ```
/// Both supported buffer types have independently owned lifetimes:
/// ```
/// use racer_dataplane::{memory::pool::PlaintextBuffer,
///     runtime::reactor::IoBuffer, store::direct::AlignedBuffer};
/// fn independent<T: 'static>() {}
/// fn completion_safe<B: IoBuffer>() { independent::<B>(); }
/// completion_safe::<PlaintextBuffer>();
/// completion_safe::<AlignedBuffer>();
/// ```
pub trait IoBuffer: sealed::Sealed + 'static {
    fn bytes(&self) -> Result<&[u8]>;
    fn bytes_mut(&mut self) -> Result<&mut [u8]>;
}

/// Reactor-owned submission state. `L` retains connection/segment/other leases.
/// Stored before the kernel can see any pointer, including across partial I/O.
struct InFlight<B: IoBuffer, L: 'static> {
    file: Rc<OwnedFd>,
    buffer: B,
    lease: L,
}

/// Resources return to the caller only after all applicable completion fences.
/// On error or an abandoned future, the reactor releases them after those fences.
pub struct Completion<B: IoBuffer, L: 'static = ()> {
    pub buffer: B,
    pub bytes: usize,
    pub lease: L,
}

impl Reactor {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self {
            admission,
            state: RefCell::new(State {
                ring: None,
                wake: None,
                ring_reservation: None,
                entries: BTreeMap::new(),
                next: 1,
                scan_after: IoId(0),
                stopped: false,
                fence_waiters: BTreeMap::new(),
                next_waiter: 0,
            }),
        }
    }

    /// Explicit, idempotent kernel resource acquisition. Also called lazily on I/O.
    pub fn init(&self) -> Result<()> {
        let mut state = self.state.borrow_mut();
        if state.stopped {
            return Err(Error::Unavailable);
        }
        if state.ring.is_some() {
            return Ok(());
        }
        let capacity = self.admission.limits().queue_entries.get();
        // One original and one cancel per admitted operation, with no multishot
        // CQEs. Dedicated SQ/CQ headroom cannot be consumed by new data work.
        let sq = u32::try_from(capacity.checked_mul(2).ok_or(Error::InvalidConfiguration)?)
            .map_err(|_| Error::InvalidConfiguration)?;
        let cq = sq.checked_mul(2).ok_or(Error::InvalidConfiguration)?;
        // Ring infrastructure is memory, not a permanently occupied control
        // message. Cancellation headroom is structural and requires no new quota,
        // even after ordinary admission stops or all request-context bytes fill.
        let ring_bytes = (sq as usize)
            .checked_next_power_of_two()
            .and_then(|sq| sq.checked_mul(128))
            .and_then(|bytes| bytes.checked_add(8192))
            .ok_or(Error::InvalidConfiguration)?;
        let ring_reservation =
            self.admission
                .reserve_completion(None, ResourceClass::RequestContext, ring_bytes)?;
        let ring = IoUring::builder()
            .setup_cqsize(cq)
            .build(sq)
            .map_err(|_| Error::Io)?;
        // SAFETY: no borrowed pointers; ownership of the returned descriptor is unique.
        let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        if fd < 0 {
            return Err(Error::Io);
        }
        state.wake = Some(Arc::new(unsafe { OwnedFd::from_raw_fd(fd) }));
        state.ring_reservation = Some(ring_reservation);
        state.ring = Some(ring);
        Ok(())
    }

    /// Obtain after init, and attach to external worker queues before starting them.
    pub fn waker(&self) -> Result<ReactorWake> {
        self.init()?;
        Ok(ReactorWake {
            fd: self.state.borrow().wake.as_ref().unwrap().clone(),
        })
    }

    pub fn in_flight(&self) -> usize {
        self.state.borrow().entries.len()
    }

    fn submit<T: 'static>(
        &self,
        sqe: squeue::Entry,
        scope: &RequestScope,
        accept: bool,
        finish: impl FnOnce(Result<KernelResult>) -> Result<T> + 'static,
    ) -> Result<Waiting<T>> {
        scope.check()?;
        self.init()?;
        let mut state = self.state.borrow_mut();
        if state.stopped {
            return Err(Error::Unavailable);
        }
        if state.entries.len() >= self.admission.limits().queue_entries.get() {
            return Err(Error::Overloaded);
        }
        let reservation = Rc::new(self.admission.reserve_completion(
            None,
            ResourceClass::RequestContext,
            std::mem::size_of::<Entry>()
                + std::mem::size_of::<Reply<T>>()
                + std::mem::size_of_val(&finish)
                + std::mem::size_of::<Signal>()
                + std::mem::size_of::<libc::sockaddr_storage>(),
        )?);
        let id = IoId(state.next);
        if id.0 >= CANCEL_BIT {
            return Err(Error::Overloaded);
        }
        state.next += 1;
        let reply = Rc::new(RefCell::new(Reply {
            result: None,
            _reservation: reservation,
        }));
        let signal = Rc::new(Signal {
            abandoned: Cell::new(false),
            waker: RefCell::new(None),
        });
        let output = reply.clone();
        let notify = signal.clone();
        // Insert ownership BEFORE publishing the SQE. No fallible action may drop
        // this entry after publication; even submission errors leave it retained.
        state.entries.insert(
            id,
            Entry {
                finish: Box::new(move |result| {
                    let result = finish(result);
                    if !notify.abandoned.get() {
                        output.borrow_mut().result = Some(result);
                    }
                    let waker = notify.waker.borrow_mut().take();
                    waker
                }),
                signal: signal.clone(),
                scope: scope.clone(),
                original: None,
                accept,
                cancel_reason: None,
                cancel_sent: false,
                cancel_done: false,
            },
        );
        // SAFETY: the inserted entry owns all SQE backing until both CQEs arrive.
        if unsafe {
            state
                .ring
                .as_mut()
                .unwrap()
                .submission()
                .push(&sqe.user_data(id.0))
        }
        .is_err()
        {
            state.entries.remove(&id);
            return Err(Error::Overloaded);
        }
        // Submission itself is driven by poll_budgeted, so polling a future never
        // executes potentially blocking file/socket operations on the worker.
        Ok(Waiting { reply, signal })
    }

    fn buffer_io<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<OwnedFd>,
        buffer: B,
        lease: L,
        scope: &'a RequestScope,
        operation: BufferOperation,
    ) -> Operation<'a, Completion<B, L>> {
        Box::pin(async move {
            scope.check()?;
            let mut owned = InFlight {
                file,
                buffer,
                lease,
            };
            let fd = types::Fd(owned.file.as_raw_fd());
            // File issue may block on filesystem work; force it off the worker.
            // Socket opcodes use io_uring's native nonblocking issue/poll path.
            let sqe = match operation {
                BufferOperation::Read(_) | BufferOperation::Recv => {
                    let bytes = owned.buffer.bytes_mut()?;
                    let len = u32::try_from(bytes.len()).map_err(|_| Error::InvalidRequest)?;
                    match operation {
                        BufferOperation::Read(_) => opcode::Read::new(fd, bytes.as_mut_ptr(), len)
                            .offset(offset_or_zero(operation))
                            .build()
                            .flags(squeue::Flags::ASYNC),
                        _ => opcode::Recv::new(fd, bytes.as_mut_ptr(), len).build(),
                    }
                }
                BufferOperation::Write(_) | BufferOperation::Send => {
                    let bytes = owned.buffer.bytes()?;
                    let len = u32::try_from(bytes.len()).map_err(|_| Error::InvalidRequest)?;
                    match operation {
                        BufferOperation::Write(_) => opcode::Write::new(fd, bytes.as_ptr(), len)
                            .offset(offset_or_zero(operation))
                            .build()
                            .flags(squeue::Flags::ASYNC),
                        _ => opcode::Send::new(fd, bytes.as_ptr(), len)
                            .flags(libc::MSG_NOSIGNAL)
                            .build(),
                    }
                }
            };
            self.submit(sqe, scope, false, move |result| {
                // Destructure inside the closure to retain the FD even when a
                // caller drops its own last reference while this I/O is pending.
                let InFlight {
                    file,
                    buffer,
                    lease,
                } = owned;
                drop(file);
                Ok(Completion {
                    buffer,
                    bytes: result?.value()? as usize,
                    lease,
                })
            })?
            .await
        })
    }
    /// Transfer the FD, buffer, and any reuse-preventing lease (`()` if none).
    /// Owned resources can move from one completed operation to the next:
    /// ```no_run
    /// use std::{os::fd::OwnedFd, rc::Rc};
    /// use racer_dataplane::{error::Result,
    ///     runtime::{deadline::RequestScope, reactor::{Completion, Reactor}},
    ///     store::{direct::AlignedBuffer, segment::SegmentLease}};
    /// async fn copy(reactor: &Reactor, fd: Rc<OwnedFd>, buffer: AlignedBuffer,
    ///     lease: SegmentLease, scope: &RequestScope)
    ///     -> Result<Completion<AlignedBuffer, SegmentLease>> {
    ///     let read = reactor.read_at(fd.clone(), 0, buffer, lease, scope).await?;
    ///     reactor.write_at(fd, 0, read.buffer, read.lease, scope).await
    /// }
    /// ```
    /// The retained lease cannot borrow from the waiting future's caller:
    /// ```compile_fail
    /// use std::{os::fd::OwnedFd, rc::Rc};
    /// use racer_dataplane::{memory::pool::PlaintextBuffer,
    ///     runtime::{deadline::RequestScope, reactor::Reactor},
    ///     store::segment::SegmentLease};
    /// fn borrowed(reactor: &Reactor, fd: Rc<OwnedFd>, buffer: PlaintextBuffer,
    ///     lease: &SegmentLease, scope: &RequestScope) {
    ///     let _future = reactor.read_at(fd, 0, buffer, lease, scope);
    /// }
    /// ```
    pub fn read_at<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<OwnedFd>,
        offset: u64,
        buffer: B,
        lease: L,
        scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, L>> {
        self.buffer_io(file, buffer, lease, scope, BufferOperation::Read(offset))
    }
    pub fn write_at<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<OwnedFd>,
        offset: u64,
        buffer: B,
        lease: L,
        scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, L>> {
        self.buffer_io(file, buffer, lease, scope, BufferOperation::Write(offset))
    }

    pub fn recv<'a, B: IoBuffer, L: 'static>(
        &'a self,
        fd: Rc<OwnedFd>,
        buffer: B,
        lease: L,
        scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, L>> {
        self.buffer_io(fd, buffer, lease, scope, BufferOperation::Recv)
    }

    pub fn send<'a, B: IoBuffer, L: 'static>(
        &'a self,
        fd: Rc<OwnedFd>,
        buffer: B,
        lease: L,
        scope: &'a RequestScope,
    ) -> Operation<'a, Completion<B, L>> {
        self.buffer_io(fd, buffer, lease, scope, BufferOperation::Send)
    }

    pub fn readiness<'a>(
        &'a self,
        fd: Rc<OwnedFd>,
        interest: u32,
        scope: &'a RequestScope,
    ) -> Operation<'a, u32> {
        Box::pin(async move {
            if interest == 0 || interest & !((libc::POLLIN | libc::POLLOUT) as u32) != 0 {
                return Err(Error::InvalidRequest);
            }
            let sqe = opcode::PollAdd::new(types::Fd(fd.as_raw_fd()), interest).build();
            self.submit(sqe, scope, false, move |result| {
                drop(fd);
                Ok(result?.value()? as u32)
            })?
            .await
        })
    }

    pub fn accept<'a>(
        &'a self,
        fd: Rc<OwnedFd>,
        scope: &'a RequestScope,
    ) -> Operation<'a, OwnedFd> {
        Box::pin(async move {
            let sqe = opcode::Accept::new(
                types::Fd(fd.as_raw_fd()),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
            )
            .flags(libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK)
            .build();
            self.submit(sqe, scope, true, move |result| {
                drop(fd);
                match result? {
                    KernelResult::Accepted(fd) => Ok(fd),
                    value => {
                        value.value()?;
                        Err(Error::Io)
                    }
                }
            })?
            .await
        })
    }

    pub fn connect<'a>(
        &'a self,
        fd: Rc<OwnedFd>,
        address: SocketAddress,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        self.connect_with_lease(fd, address, (), scope)
    }

    /// Transfer the complete connection/admission owner before submission. Return
    /// it only after the original and any cancellation CQE are fenced. A dropped
    /// connect future cannot return its endpoint slot or quota prematurely.
    pub fn connect_with_lease<'a, L: 'static>(
        &'a self,
        fd: Rc<OwnedFd>,
        address: SocketAddress,
        lease: L,
        scope: &'a RequestScope,
    ) -> Operation<'a, L> {
        Box::pin(async move {
            let (address, len) = encode_address(address)?;
            let sqe = opcode::Connect::new(
                types::Fd(fd.as_raw_fd()),
                (&*address as *const libc::sockaddr_storage).cast(),
                len,
            )
            .build();
            self.submit(sqe, scope, false, move |result| {
                drop((fd, address));
                result?.value()?;
                Ok(lease)
            })?
            .await
        })
    }

    /// Process at most `budget` CQEs and inspect at most `budget` cancellation
    /// candidates, round-robin. Returns CQEs consumed, including cancel CQEs.
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        if budget == 0 {
            return Ok(0);
        }
        let mut state = self.state.borrow_mut();
        if state.ring.is_none() {
            return Ok(0);
        }
        let mut finished = Vec::new();
        let mut fence_wakes = Vec::new();
        let mut completed = 0;
        while completed < budget {
            let cqe = state.ring.as_mut().unwrap().completion().next();
            let Some(cqe) = cqe else {
                break;
            };
            completed += 1;
            if let Some(entry) = state.complete(cqe.user_data(), cqe.result())? {
                finished.push(entry);
                fence_wakes
                    .extend(state.take_fence_wakers(Some(IoId(cqe.user_data() & !CANCEL_BIT))));
            }
        }
        if state.entries.is_empty() {
            fence_wakes.extend(state.take_fence_wakers(None));
        }
        let count = budget.min(state.entries.len());
        for _ in 0..count {
            let id = state
                .entries
                .range((
                    std::ops::Bound::Excluded(state.scan_after),
                    std::ops::Bound::Unbounded,
                ))
                .next()
                .or_else(|| state.entries.first_key_value())
                .map(|(id, _)| *id);
            let Some(id) = id else {
                break;
            };
            state.scan_after = id;
            let stopped = state.stopped;
            let entry = state.entries.get_mut(&id).unwrap();
            if entry.cancel_reason.is_none() {
                entry.cancel_reason = if stopped || entry.signal.abandoned.get() {
                    Some(Error::Cancelled)
                } else {
                    entry.scope.check().err()
                };
            }
            if entry.original.is_none() && entry.cancel_reason.is_some() && !entry.cancel_sent {
                let sqe = opcode::AsyncCancel::new(id.0)
                    .build()
                    .user_data(id.0 | CANCEL_BIT);
                // SAFETY: cancel uses only an ID, and its target entry is retained.
                if unsafe { state.ring.as_mut().unwrap().submission().push(&sqe) }.is_ok() {
                    state.entries.get_mut(&id).unwrap().cancel_sent = true;
                }
            }
        }
        let submitted = state.ring.as_ref().unwrap().submit();
        drop(state);
        // Wakers may reenter the worker; never invoke under the reactor RefCell.
        for entry in finished {
            if let Some(waker) = entry.finish() {
                waker.wake();
            }
        }
        for waker in fence_wakes {
            waker.wake();
        }
        match submitted {
            Ok(_) => Ok(completed),
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::EINTR | libc::EAGAIN | libc::EBUSY)
                ) =>
            {
                Ok(completed)
            }
            Err(_) => Err(Error::Io),
        }
    }

    /// Sleep until a CQE/external wake, bounded by both duration and a 10ms timer
    /// fallback for producers that cannot yet attach the eventfd wake endpoint.
    pub fn wait(&self, duration: Duration) -> Result<()> {
        let state = self.state.borrow();
        let mut fds = [
            libc::pollfd {
                fd: state.ring.as_ref().map_or(-1, AsRawFd::as_raw_fd),
                events: libc::POLLIN,
                revents: 0,
            },
            libc::pollfd {
                fd: state.wake.as_ref().map_or(-1, |fd| fd.as_raw_fd()),
                events: libc::POLLIN,
                revents: 0,
            },
        ];
        let duration = duration.min(MAX_WAIT);
        let timeout = libc::timespec {
            tv_sec: 0,
            tv_nsec: duration.as_nanos() as libc::c_long,
        };
        // SAFETY: poll only borrows initialized descriptor/timeout arrays for this call.
        let result = unsafe {
            libc::ppoll(
                fds.as_mut_ptr(),
                fds.len() as libc::nfds_t,
                &timeout,
                std::ptr::null(),
            )
        };
        if result < 0 && std::io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
            return Err(Error::Io);
        }
        if fds[1].revents & libc::POLLIN != 0 {
            let mut value = 0u64;
            // SAFETY: nonblocking eventfd read into an initialized local u64.
            unsafe {
                libc::read(fds[1].fd, (&mut value as *mut u64).cast(), 8);
            }
        }
        if fds
            .iter()
            .any(|fd| fd.revents & (libc::POLLERR | libc::POLLNVAL) != 0)
        {
            return Err(Error::Io);
        }
        Ok(())
    }

    /// Request cancellation and sleep until the original and any cancel CQEs arrive.
    /// The worker must drive poll_budgeted. Notification registrations share a
    /// queue_entries bound with drain waiters and charge completion metadata;
    /// admission failure still leaves cancellation requested, but is not a fence.
    pub fn cancel_and_fence(&self, id: IoId) -> Operation<'_, ()> {
        Box::pin(Fence {
            reactor: self,
            target: Some(id),
            registration: None,
        })
    }

    /// Close admission and cancel outstanding work. The worker must keep driving
    /// poll_budgeted while awaiting this fence, including after request deadlines.
    /// Notification admission can fail as for cancel_and_fence; only Ok confirms
    /// the fence. Failure leaves admission closed and cancellation requested.
    pub fn drain(&self) -> Operation<'_, ()> {
        Box::pin(Fence {
            reactor: self,
            target: None,
            registration: None,
        })
    }
}

#[derive(Clone, Copy)]
enum BufferOperation {
    Read(u64),
    Write(u64),
    Recv,
    Send,
}

fn offset_or_zero(operation: BufferOperation) -> u64 {
    match operation {
        BufferOperation::Read(offset) | BufferOperation::Write(offset) => offset,
        _ => 0,
    }
}

impl State {
    fn take_fence_wakers(&mut self, target: Option<IoId>) -> Vec<Waker> {
        let keys: Vec<_> = self
            .fence_waiters
            .range((target, 0)..=(target, u64::MAX))
            .map(|(key, _)| *key)
            .collect();
        keys.into_iter()
            .map(|key| self.fence_waiters.remove(&key).unwrap().waker)
            .collect()
    }

    fn complete(&mut self, tag: u64, result: i32) -> Result<Option<Entry>> {
        let id = IoId(tag & !CANCEL_BIT);
        let entry = self.entries.get_mut(&id).ok_or(Error::Io)?;
        if tag & CANCEL_BIT != 0 {
            if !entry.cancel_sent || entry.cancel_done {
                return Err(Error::Io);
            }
            entry.cancel_done = true; // ENOENT/EALREADY are also cancellation fences.
        } else {
            if entry.original.is_some() {
                return Err(Error::Io);
            }
            entry.original = Some(if entry.accept && result >= 0 {
                // SAFETY: successful single-shot accept CQE transfers a fresh FD.
                KernelResult::Accepted(unsafe { OwnedFd::from_raw_fd(result) })
            } else {
                KernelResult::Value(result)
            });
            // Cancellation/deadline may precede this CQE without a cancel SQE.
            if entry.cancel_reason.is_none() {
                entry.cancel_reason = entry.scope.check().err();
            }
        }
        if entry.fenced() {
            Ok(self.entries.remove(&id))
        } else {
            Ok(None)
        }
    }
}

impl Drop for Reactor {
    fn drop(&mut self) {
        self.state.get_mut().stopped = true;
        while self.in_flight() != 0 {
            if self.poll_budgeted(256).is_err() || self.wait(MAX_WAIT).is_err() {
                // Closing an io_uring FD alone is NOT a synchronous memory fence.
                // On an unrecoverable driver failure retain the bounded owners and
                // ring forever rather than free memory the kernel may still use.
                let state = self.state.get_mut();
                std::mem::forget(std::mem::take(&mut state.entries));
                std::mem::forget(state.ring.take());
                std::mem::forget(state.ring_reservation.take());
                return;
            }
        }
        // Every SQE (including cancellation SQEs) has now produced its CQE.
        self.state.get_mut().ring.take();
    }
}

impl Drop for State {
    fn drop(&mut self) {
        // Also protect kernel ownership if an application lease destructor or a
        // custom waker panics while Reactor::drop is driving its final fence.
        if !self.entries.is_empty() {
            std::mem::forget(std::mem::take(&mut self.entries));
            std::mem::forget(self.ring.take());
            std::mem::forget(self.ring_reservation.take());
        }
    }
}

fn encode_address(
    address: SocketAddress,
) -> Result<(Box<libc::sockaddr_storage>, libc::socklen_t)> {
    // SAFETY: all-zero sockaddr storage is valid and sufficiently aligned for
    // every supported sockaddr variant. Box keeps its address stable across moves.
    let mut storage: Box<libc::sockaddr_storage> = Box::new(unsafe { std::mem::zeroed() });
    let len = match address {
        SocketAddress::Inet(SocketAddr::V4(address)) => {
            let value = libc::sockaddr_in {
                sin_family: libc::AF_INET as _,
                sin_port: address.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from_ne_bytes(address.ip().octets()),
                },
                sin_zero: [0; 8],
            };
            unsafe {
                std::ptr::write(
                    (&mut *storage as *mut libc::sockaddr_storage).cast::<libc::sockaddr_in>(),
                    value,
                );
            }
            std::mem::size_of::<libc::sockaddr_in>()
        }
        SocketAddress::Inet(SocketAddr::V6(address)) => {
            let value = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as _,
                sin6_port: address.port().to_be(),
                sin6_flowinfo: address.flowinfo().to_be(),
                sin6_addr: libc::in6_addr {
                    s6_addr: address.ip().octets(),
                },
                sin6_scope_id: address.scope_id(),
            };
            unsafe {
                std::ptr::write(
                    (&mut *storage as *mut libc::sockaddr_storage).cast::<libc::sockaddr_in6>(),
                    value,
                );
            }
            std::mem::size_of::<libc::sockaddr_in6>()
        }
        SocketAddress::Unix(path) => {
            let bytes = path.as_os_str().as_bytes();
            let mut value: libc::sockaddr_un = unsafe { std::mem::zeroed() };
            if bytes.is_empty() || bytes.contains(&0) || bytes.len() >= value.sun_path.len() {
                return Err(Error::InvalidRequest);
            }
            value.sun_family = libc::AF_UNIX as _;
            for (dst, src) in value.sun_path.iter_mut().zip(bytes) {
                *dst = *src as libc::c_char;
            }
            unsafe {
                std::ptr::write(
                    (&mut *storage as *mut libc::sockaddr_storage).cast::<libc::sockaddr_un>(),
                    value,
                );
            }
            std::mem::offset_of!(libc::sockaddr_un, sun_path) + bytes.len() + 1
        }
    };
    Ok((storage, len as libc::socklen_t))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::{identity::RequestId, limits::Limits},
        runtime::deadline::{Cancellation, Deadline},
    };
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::{num::NonZeroUsize, os::unix::net::UnixStream, time::Instant};

    #[derive(Default)]
    struct Count(AtomicUsize);
    impl std::task::Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    struct Buffer(Box<[u8]>, Rc<Cell<usize>>);
    impl sealed::Sealed for Buffer {}
    impl IoBuffer for Buffer {
        fn bytes(&self) -> Result<&[u8]> {
            Ok(&self.0)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8]> {
            Ok(&mut self.0)
        }
    }
    impl Drop for Buffer {
        fn drop(&mut self) {
            self.1.set(self.1.get() + 1);
        }
    }
    struct Lease(Rc<Cell<usize>>);
    impl Drop for Lease {
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }
    fn buffer(bytes: &[u8]) -> Buffer {
        Buffer(bytes.into(), Rc::new(Cell::new(0)))
    }
    fn limits(capacity: usize) -> Limits {
        let n = NonZeroUsize::new(1024 * 1024).unwrap();
        Limits {
            plaintext_bytes: n,
            ciphertext_bytes: n,
            dirty_bytes: n,
            registered_bytes: n,
            request_context_bytes: n,
            flights: n,
            waiters_per_flight: n,
            queue_entries: NonZeroUsize::new(capacity).unwrap(),
            connections_per_neighbor: n,
            client_connections: n,
            pipes: n,
            range_window_pages: n,
            replay_entries: n,
            header_bytes: n,
            route_search_work: n,
            cached_rankings: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        }
    }
    fn scope() -> RequestScope {
        RequestScope {
            request: RequestId([0; 16]),
            deadline: Deadline(Instant::now() + Duration::from_secs(5)),
            cancellation: Cancellation::new().unwrap(),
        }
    }
    fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = poll(&mut future) {
                return result;
            }
            assert!(Instant::now() < deadline, "reactor failed to make progress");
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }
    fn kernel_reactor(capacity: usize) -> Option<Reactor> {
        // Skip only when the kernel lacks io_uring or policy denies ring creation.
        // Configuration, quota, opcode, and ordinary I/O errors must fail tests.
        match IoUring::new(2) {
            Ok(ring) => drop(ring),
            Err(error)
                if matches!(
                    error.raw_os_error(),
                    Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
                ) =>
            {
                eprintln!("io_uring kernel test unavailable: {error}");
                return None;
            }
            Err(error) => panic!("unexpected io_uring setup failure: {error}"),
        }
        let reactor = Reactor::new(Rc::new(Admission::new(limits(capacity))));
        reactor.init().unwrap();
        Some(reactor)
    }

    #[test]
    fn constructor_does_not_open_kernel_resources() {
        let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
        assert!(reactor.state.borrow().ring.is_none());
        assert!(reactor.state.borrow().wake.is_none());
        assert_eq!(reactor.in_flight(), 0);
        assert_eq!(reactor.poll_budgeted(1).unwrap(), 0);
    }

    #[test]
    fn connecting_lease_survives_abandonment_until_kernel_fence() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let fd = unsafe {
            libc::socket(
                libc::AF_INET,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(fd >= 0);
        let fd = Rc::new(unsafe { OwnedFd::from_raw_fd(fd) });
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let quota = reactor
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let scope = scope();
        let mut operation = reactor.connect_with_lease(
            fd,
            SocketAddress::Inet(listener.local_addr().unwrap()),
            (Lease(drops.clone()), quota),
            &scope,
        );
        assert!(poll(&mut operation).is_pending());
        drop(operation);
        assert_eq!(
            drops.get(),
            0,
            "dropped future cannot release connecting admission"
        );
        assert!(weak.upgrade().is_some());
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(drops.get(), 1);
        assert!(weak.upgrade().is_none());
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 0);
        assert_eq!(reactor.in_flight(), 0);
    }

    #[test]
    fn fence_waiters_sleep_until_both_cqes_and_unregister_on_drop() {
        for cancel_first in [false, true] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(4))));
            reactor.state.borrow_mut().entries.insert(
                IoId(1),
                Entry {
                    finish: Box::new(|_| None),
                    signal: Rc::new(Signal {
                        abandoned: Cell::new(false),
                        waker: RefCell::new(None),
                    }),
                    scope: scope(),
                    original: None,
                    accept: false,
                    cancel_reason: None,
                    cancel_sent: true,
                    cancel_done: false,
                },
            );
            let counter = Arc::new(Count(AtomicUsize::new(0)));
            let waker = Waker::from(counter.clone());
            let mut cx = Context::from_waker(&waker);
            let mut cancel = reactor.cancel_and_fence(IoId(1));
            let mut drain = reactor.drain();
            let mut abandoned = reactor.cancel_and_fence(IoId(1));
            for future in [&mut cancel, &mut drain, &mut abandoned] {
                for _ in 0..3 {
                    assert!(future.as_mut().poll(&mut cx).is_pending());
                }
            }
            assert_eq!(counter.0.load(Ordering::Relaxed), 0);
            assert_eq!(reactor.state.borrow().fence_waiters.len(), 3);
            drop(abandoned);
            assert_eq!(reactor.state.borrow().fence_waiters.len(), 2);
            let first = if cancel_first { 1 | CANCEL_BIT } else { 1 };
            let second = if cancel_first { 1 } else { 1 | CANCEL_BIT };
            assert!(
                reactor
                    .state
                    .borrow_mut()
                    .complete(first, -libc::ECANCELED)
                    .unwrap()
                    .is_none()
            );
            assert!(cancel.as_mut().poll(&mut cx).is_pending());
            assert!(drain.as_mut().poll(&mut cx).is_pending());
            assert_eq!(counter.0.load(Ordering::Relaxed), 0);
            let (entry, wakes) = {
                let mut state = reactor.state.borrow_mut();
                let entry = state.complete(second, -libc::ENOENT).unwrap().unwrap();
                let mut wakes = state.take_fence_wakers(Some(IoId(1)));
                wakes.extend(state.take_fence_wakers(None));
                (entry, wakes)
            };
            entry.finish();
            for waker in wakes {
                waker.wake();
            }
            assert_eq!(counter.0.load(Ordering::Relaxed), 2);
            assert!(matches!(cancel.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
            assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
            assert!(reactor.state.borrow().fence_waiters.is_empty());
            assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 0);
        }
    }

    #[test]
    fn fence_waiters_are_bounded_and_refresh_executor_wakers() {
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        reactor.state.borrow_mut().entries.insert(
            IoId(1),
            Entry {
                finish: Box::new(|_| None),
                signal: Rc::new(Signal {
                    abandoned: Cell::new(false),
                    waker: RefCell::new(None),
                }),
                scope: scope(),
                original: None,
                accept: false,
                cancel_reason: None,
                cancel_sent: false,
                cancel_done: false,
            },
        );
        let mut first = reactor.cancel_and_fence(IoId(1));
        assert!(poll(&mut first).is_pending());
        let refreshed = Waker::from(Arc::new(Count::default()));
        assert!(
            first
                .as_mut()
                .poll(&mut Context::from_waker(&refreshed))
                .is_pending()
        );
        assert!(
            reactor
                .state
                .borrow()
                .fence_waiters
                .first_key_value()
                .unwrap()
                .1
                .waker
                .will_wake(&refreshed)
        );
        let mut overflow = reactor.drain();
        assert!(matches!(
            poll(&mut overflow),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(first);
        let mut replacement = reactor.drain();
        assert!(poll(&mut replacement).is_pending());
        drop(replacement);
        assert!(reactor.state.borrow().fence_waiters.is_empty());
        reactor
            .state
            .borrow_mut()
            .complete(1, -libc::ECANCELED)
            .unwrap()
            .unwrap()
            .finish();
    }

    #[test]
    fn kernel_cancellation_wakes_registered_fences_without_self_waking() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let (socket, _peer) = UnixStream::pair().unwrap();
        let request = scope();
        let mut recv = reactor.recv(
            Rc::new(OwnedFd::from(socket)),
            buffer(&[0; 8]),
            (),
            &request,
        );
        assert!(poll(&mut recv).is_pending());
        let id = *reactor.state.borrow().entries.first_key_value().unwrap().0;
        let counter = Arc::new(Count::default());
        let waker = Waker::from(counter.clone());
        let mut cx = Context::from_waker(&waker);
        let mut cancel = reactor.cancel_and_fence(id);
        let mut drain = reactor.drain();
        assert!(cancel.as_mut().poll(&mut cx).is_pending());
        assert!(drain.as_mut().poll(&mut cx).is_pending());
        assert_eq!(counter.0.load(Ordering::Relaxed), 0);
        let deadline = Instant::now() + Duration::from_secs(5);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(1).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        assert_eq!(counter.0.load(Ordering::Relaxed), 2);
        assert!(matches!(cancel.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
        assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
        assert!(matches!(
            poll(&mut recv),
            Poll::Ready(Err(Error::Cancelled))
        ));
    }

    #[test]
    fn delayed_and_reordered_cancel_cqes_retain_every_owner() {
        for cancel_first in [false, true] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
            let (socket, _peer) = UnixStream::pair().unwrap();
            let fd = Rc::new(OwnedFd::from(socket));
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let owned = InFlight {
                file: fd,
                buffer: Buffer(vec![0; 32].into(), drops.clone()),
                lease: Lease(drops.clone()),
            };
            let signal = Rc::new(Signal {
                abandoned: Cell::new(false),
                waker: RefCell::new(None),
            });
            let reservation = Rc::new(
                reactor
                    .admission
                    .reserve(None, ResourceClass::RequestContext, 64)
                    .unwrap(),
            );
            let reply = Rc::new(RefCell::new(Reply::<()> {
                result: None,
                _reservation: reservation.clone(),
            }));
            let waiting = Waiting {
                reply: reply.clone(),
                signal: signal.clone(),
            };
            reactor.state.borrow_mut().entries.insert(
                IoId(1),
                Entry {
                    finish: Box::new(move |result| {
                        drop((owned, reservation));
                        assert!(matches!(result, Err(Error::Cancelled)));
                        None
                    }),
                    scope: scope(),
                    signal,
                    original: None,
                    accept: false,
                    cancel_reason: Some(Error::Cancelled),
                    cancel_sent: true,
                    cancel_done: false,
                },
            );
            drop(waiting);
            drop(reply);
            assert_eq!(drops.get(), 0);
            let (first, second) = if cancel_first {
                ((1 | CANCEL_BIT, 0), (1, -libc::ECANCELED))
            } else {
                ((1, 7), (1 | CANCEL_BIT, -libc::ENOENT))
            };
            assert!(
                reactor
                    .state
                    .borrow_mut()
                    .complete(first.0, first.1)
                    .unwrap()
                    .is_none()
            );
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 64);
            let done = reactor
                .state
                .borrow_mut()
                .complete(second.0, second.1)
                .unwrap()
                .unwrap();
            done.finish();
            assert_eq!(drops.get(), 2);
            assert!(weak.upgrade().is_none());
            assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 0);
        }
    }

    #[test]
    fn sockaddr_encoding_is_owned_and_validated() {
        let (address, len) =
            encode_address(SocketAddress::Inet("127.0.0.1:1234".parse().unwrap())).unwrap();
        assert_eq!(len as usize, std::mem::size_of::<libc::sockaddr_in>());
        let value =
            unsafe { &*(&*address as *const libc::sockaddr_storage).cast::<libc::sockaddr_in>() };
        assert_eq!(value.sin_port, 1234u16.to_be());
        assert_eq!(value.sin_addr.s_addr.to_ne_bytes(), [127, 0, 0, 1]);
        let (address, _) =
            encode_address(SocketAddress::Inet("[::1]:4321".parse().unwrap())).unwrap();
        assert_eq!(address.ss_family, libc::AF_INET6 as libc::sa_family_t);
        let (address, len) = encode_address(SocketAddress::Unix("/a/b".into())).unwrap();
        assert_eq!(address.ss_family, libc::AF_UNIX as libc::sa_family_t);
        assert_eq!(
            len as usize,
            std::mem::offset_of!(libc::sockaddr_un, sun_path) + 5
        );
        for path in [
            PathBuf::new(),
            PathBuf::from("a\0b"),
            PathBuf::from("x".repeat(108)),
        ] {
            assert!(matches!(
                encode_address(SocketAddress::Unix(path)),
                Err(Error::InvalidRequest)
            ));
        }
    }

    #[test]
    fn accepted_descriptor_is_retained_until_cancel_fence() {
        use std::os::fd::IntoRawFd;
        let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = socket.into_raw_fd();
        reactor.state.borrow_mut().entries.insert(
            IoId(1),
            Entry {
                finish: Box::new(|result| {
                    assert!(matches!(result, Err(Error::Cancelled)));
                    None
                }),
                scope: scope(),
                signal: Rc::new(Signal {
                    abandoned: Cell::new(true),
                    waker: RefCell::new(None),
                }),
                original: None,
                accept: true,
                cancel_reason: Some(Error::Cancelled),
                cancel_sent: true,
                cancel_done: false,
            },
        );
        assert!(
            reactor
                .state
                .borrow_mut()
                .complete(1, fd)
                .unwrap()
                .is_none()
        );
        assert!(unsafe { libc::fcntl(fd, libc::F_GETFD) } >= 0);
        reactor
            .state
            .borrow_mut()
            .complete(1 | CANCEL_BIT, -libc::ENOENT)
            .unwrap()
            .unwrap()
            .finish();
        assert_eq!(unsafe { libc::fcntl(fd, libc::F_GETFD) }, -1);
        assert_eq!(
            std::io::Error::last_os_error().raw_os_error(),
            Some(libc::EBADF)
        );
    }

    #[test]
    fn delayed_short_success_and_error_return_only_on_original_cqe() {
        for result in [3, -libc::EIO] {
            let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
            let delivered = Rc::new(Cell::new(None));
            let output = delivered.clone();
            let drops = Rc::new(Cell::new(0));
            let lease = Lease(drops.clone());
            reactor.state.borrow_mut().entries.insert(
                IoId(1),
                Entry {
                    finish: Box::new(move |result| {
                        output.set(Some(result.and_then(KernelResult::value)));
                        drop(lease);
                        None
                    }),
                    scope: scope(),
                    signal: Rc::new(Signal {
                        abandoned: Cell::new(false),
                        waker: RefCell::new(None),
                    }),
                    original: None,
                    accept: false,
                    cancel_reason: None,
                    cancel_sent: false,
                    cancel_done: false,
                },
            );
            assert_eq!(delivered.get(), None);
            assert_eq!(drops.get(), 0);
            reactor
                .state
                .borrow_mut()
                .complete(1, result)
                .unwrap()
                .unwrap()
                .finish();
            assert_eq!(
                delivered.get(),
                Some(if result >= 0 { Ok(3) } else { Err(Error::Io) })
            );
            assert_eq!(drops.get(), 1);
        }
    }

    #[test]
    fn real_socket_short_io_readiness_eof_and_broken_pipe() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        let (left, right) = UnixStream::pair().unwrap();
        left.set_nonblocking(true).unwrap();
        right.set_nonblocking(true).unwrap();
        let left = Rc::new(OwnedFd::from(left));
        let right = Rc::new(OwnedFd::from(right));
        let sent = drive(
            &reactor,
            reactor.send(left.clone(), buffer(b"hello"), (), &scope),
        )
        .unwrap();
        assert_eq!(sent.bytes, 5);
        let ready = drive(
            &reactor,
            reactor.readiness(right.clone(), libc::POLLIN as u32, &scope),
        )
        .unwrap();
        assert_ne!(ready & libc::POLLIN as u32, 0);
        let received = drive(
            &reactor,
            reactor.recv(right.clone(), buffer(&[0; 64]), (), &scope),
        )
        .unwrap();
        assert_eq!(received.bytes, 5);
        assert_eq!(&received.buffer.bytes().unwrap()[..5], b"hello");
        drop(left);
        assert_eq!(
            drive(
                &reactor,
                reactor.recv(right.clone(), buffer(&[0; 64]), (), &scope)
            )
            .unwrap()
            .bytes,
            0
        );
        assert!(matches!(
            drive(&reactor, reactor.send(right, buffer(b"x"), (), &scope)),
            Err(Error::Io)
        ));
    }

    #[test]
    fn real_connect_lease_survives_cqes_until_result_is_consumed() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let request = scope();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let raw = unsafe {
            libc::socket(
                libc::AF_INET,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(raw >= 0);
        let fd = Rc::new(unsafe { OwnedFd::from_raw_fd(raw) });
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let quota = reactor
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let mut connect = reactor.connect_with_lease(
            fd,
            SocketAddress::Inet(listener.local_addr().unwrap()),
            (Lease(drops.clone()), quota),
            &request,
        );
        assert!(poll(&mut connect).is_pending());
        assert!(weak.upgrade().is_some());
        let deadline = Instant::now() + Duration::from_secs(5);
        while reactor.in_flight() != 0 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(1).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        // A completed but unconsumed reply must still quarantine the endpoint slot.
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
        let Poll::Ready(Ok(lease)) = poll(&mut connect) else {
            panic!("connect did not return its lease");
        };
        drop(connect);
        assert_eq!(drops.get(), 0);
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
        drop(lease);
        assert_eq!(drops.get(), 1);
        assert_eq!(reactor.admission.used(ResourceClass::Connection), 0);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn real_connect_lease_is_quarantined_after_cancel_and_error() {
        for (cancel, invalid_family) in [(true, false), (false, true)] {
            let Some(reactor) = kernel_reactor(2) else {
                return;
            };
            let request = scope();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            // An AF_UNIX socket with an Inet address deterministically fails in
            // the kernel, without racing another listener for an unused TCP port.
            let raw = unsafe {
                libc::socket(
                    if invalid_family {
                        libc::AF_UNIX
                    } else {
                        libc::AF_INET
                    },
                    libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                    0,
                )
            };
            assert!(raw >= 0);
            let fd = Rc::new(unsafe { OwnedFd::from_raw_fd(raw) });
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let quota = reactor
                .admission
                .reserve(None, ResourceClass::Connection, 1)
                .unwrap();
            let mut connect = reactor.connect_with_lease(
                fd,
                SocketAddress::Inet(listener.local_addr().unwrap()),
                (Lease(drops.clone()), quota),
                &request,
            );
            assert!(poll(&mut connect).is_pending());
            if cancel {
                request.cancel().unwrap();
            }
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
            let deadline = Instant::now() + Duration::from_secs(5);
            while reactor.in_flight() != 0 {
                assert!(Instant::now() < deadline);
                reactor.poll_budgeted(1).unwrap();
                if reactor.in_flight() != 0 {
                    // Includes the interval between original and cancel CQEs
                    // when a cancel SQE wins the race against connect completion.
                    assert_eq!(drops.get(), 0);
                    assert!(weak.upgrade().is_some());
                    assert_eq!(reactor.admission.used(ResourceClass::Connection), 1);
                }
                reactor.wait(Duration::from_millis(1)).unwrap();
            }
            let expected = if cancel { Error::Cancelled } else { Error::Io };
            assert!(matches!(poll(&mut connect), Poll::Ready(Err(error)) if error == expected));
            assert_eq!(drops.get(), 1);
            assert!(weak.upgrade().is_none());
            assert_eq!(reactor.admission.used(ResourceClass::Connection), 0);
        }
    }

    #[test]
    fn real_tcp_accept_connect() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let address = listener.local_addr().unwrap();
        let listener = Rc::new(OwnedFd::from(listener));
        let raw = unsafe {
            libc::socket(
                libc::AF_INET,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(raw >= 0);
        let client = Rc::new(unsafe { OwnedFd::from_raw_fd(raw) });
        let mut accept = reactor.accept(listener, &scope);
        assert!(poll(&mut accept).is_pending());
        drive(
            &reactor,
            reactor.connect(client.clone(), SocketAddress::Inet(address), &scope),
        )
        .unwrap();
        let server = Rc::new(drive(&reactor, accept).unwrap());
        drive(
            &reactor,
            reactor.send(client, buffer(b"connected"), (), &scope),
        )
        .unwrap();
        let received = drive(&reactor, reactor.recv(server, buffer(&[0; 32]), (), &scope)).unwrap();
        assert_eq!(
            &received.buffer.bytes().unwrap()[..received.bytes],
            b"connected"
        );
    }

    #[test]
    fn real_unix_connect_keeps_sockaddr_alive() {
        let Some(reactor) = kernel_reactor(4) else {
            return;
        };
        let scope = scope();
        // Linux exposes an unnamed Unix listener's autobound abstract name via
        // getsockname, but SocketAddress::Unix intentionally means a filesystem
        // path. Use a process-unique socket in the existing build directory.
        let path =
            PathBuf::from("target").join(format!("reactor-unix-{}.sock", std::process::id()));
        struct RemoveSocket(PathBuf);
        impl Drop for RemoveSocket {
            fn drop(&mut self) {
                let _ = std::fs::remove_file(&self.0);
            }
        }
        let listener = std::os::unix::net::UnixListener::bind(&path).unwrap();
        let _cleanup = RemoveSocket(path.clone());
        listener.set_nonblocking(true).unwrap();
        let raw = unsafe {
            libc::socket(
                libc::AF_UNIX,
                libc::SOCK_STREAM | libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK,
                0,
            )
        };
        assert!(raw >= 0);
        let client = Rc::new(unsafe { OwnedFd::from_raw_fd(raw) });
        let mut accept = reactor.accept(Rc::new(listener.into()), &scope);
        assert!(poll(&mut accept).is_pending());
        drive(
            &reactor,
            reactor.connect(client.clone(), SocketAddress::Unix(path), &scope),
        )
        .unwrap();
        let server = Rc::new(drive(&reactor, accept).unwrap());
        drive(&reactor, reactor.send(client, buffer(b"unix"), (), &scope)).unwrap();
        let received = drive(&reactor, reactor.recv(server, buffer(&[0; 32]), (), &scope)).unwrap();
        assert_eq!(&received.buffer.bytes().unwrap()[..received.bytes], b"unix");
    }

    #[test]
    fn real_cancellation_abandonment_limits_and_drop_fence() {
        let Some(reactor) = kernel_reactor(1) else {
            return;
        };
        let scope = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(OwnedFd::from(socket));
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let mut receive = reactor.recv(
            fd.clone(),
            Buffer(vec![0; 32].into(), drops.clone()),
            Lease(drops.clone()),
            &scope,
        );
        assert!(poll(&mut receive).is_pending());
        reactor.poll_budgeted(1).unwrap();
        assert_eq!(drops.get(), 0);
        let mut overflow = reactor.recv(fd.clone(), buffer(&[0; 1]), (), &scope);
        assert!(matches!(
            poll(&mut overflow),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(overflow);
        assert_eq!(reactor.poll_budgeted(0).unwrap(), 0);
        drop(receive);
        drop(fd);
        assert_eq!(drops.get(), 0);
        assert!(weak.upgrade().is_some());
        // Drop must submit cancellation and consume both CQEs before ownership ends.
        drop(reactor);
        assert_eq!(drops.get(), 2);
        assert!(weak.upgrade().is_none());
    }

    #[test]
    fn real_deadline_and_explicit_cancel_fences() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let scope = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(OwnedFd::from(socket));
        let mut ready = reactor.readiness(fd.clone(), libc::POLLIN as u32, &scope);
        assert!(poll(&mut ready).is_pending());
        let id = *reactor.state.borrow().entries.first_key_value().unwrap().0;
        drive(&reactor, reactor.cancel_and_fence(id)).unwrap();
        assert!(matches!(
            poll(&mut ready),
            Poll::Ready(Err(Error::Cancelled))
        ));
        let short = RequestScope {
            deadline: Deadline(Instant::now() + Duration::from_millis(20)),
            ..scope.clone()
        };
        assert!(matches!(
            drive(
                &reactor,
                reactor.readiness(fd.clone(), libc::POLLIN as u32, &short)
            ),
            Err(Error::DeadlineExceeded)
        ));
        scope.cancel().unwrap();
        assert!(matches!(
            drive(&reactor, reactor.recv(fd, buffer(&[0; 1]), (), &scope)),
            Err(Error::Cancelled)
        ));
        assert_eq!(reactor.in_flight(), 0);
    }

    #[test]
    fn real_offset_file_io_and_quota_release() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        let scope = scope();
        let baseline = reactor.admission.used(ResourceClass::RequestContext);
        // Anonymous memory-backed regular file avoids filesystem fixture side effects.
        let raw = unsafe { libc::memfd_create(c"reactor-test".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(raw >= 0);
        let fd = Rc::new(unsafe { OwnedFd::from_raw_fd(raw) });
        let written = drive(
            &reactor,
            reactor.write_at(fd.clone(), 7, buffer(b"payload"), (), &scope),
        )
        .unwrap();
        assert_eq!(written.bytes, 7);
        let read = drive(
            &reactor,
            reactor.read_at(fd, 7, buffer(&[0; 32]), (), &scope),
        )
        .unwrap();
        assert_eq!(read.bytes, 7);
        assert_eq!(&read.buffer.bytes().unwrap()[..7], b"payload");
        assert_eq!(
            reactor.admission.used(ResourceClass::RequestContext),
            baseline
        );
    }

    #[test]
    fn real_external_wake_is_persistent_and_bounded() {
        let Some(reactor) = kernel_reactor(1) else {
            return;
        };
        let wake = reactor.waker().unwrap();
        std::thread::spawn(move || wake.wake().unwrap())
            .join()
            .unwrap();
        let fd = reactor.state.borrow().wake.as_ref().unwrap().as_raw_fd();
        let mut descriptor = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        assert_eq!(unsafe { libc::poll(&mut descriptor, 1, 0) }, 1);
        reactor.wait(Duration::from_secs(60)).unwrap();
        assert_eq!(unsafe { libc::poll(&mut descriptor, 1, 0) }, 0);
    }

    #[test]
    fn real_drain_io_preserves_control_capacity_after_admission_stop() {
        let Some(reactor) = kernel_reactor(2) else {
            return;
        };
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
        let control = reactor
            .admission
            .reserve(None, ResourceClass::ControlProgress, 2)
            .unwrap();
        reactor.admission.stop();
        let scope = scope();
        let (left, right) = UnixStream::pair().unwrap();
        let left = Rc::new(OwnedFd::from(left));
        let right = Rc::new(OwnedFd::from(right));
        drive(&reactor, reactor.send(left, buffer(b"drain"), (), &scope)).unwrap();
        let read = drive(
            &reactor,
            reactor.recv(right.clone(), buffer(&[0; 8]), (), &scope),
        )
        .unwrap();
        assert_eq!(&read.buffer.bytes().unwrap()[..read.bytes], b"drain");
        let mut pending = reactor.readiness(right, libc::POLLOUT as u32, &scope);
        assert!(poll(&mut pending).is_pending());
        drive(&reactor, reactor.drain()).unwrap();
        assert_eq!(reactor.in_flight(), 0);
        assert!(matches!(
            poll(&mut pending),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(reactor.init(), Err(Error::Unavailable));
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 2);
        drop(control);
    }
}
