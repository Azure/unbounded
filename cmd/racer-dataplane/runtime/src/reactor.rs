//! Worker-local io_uring ownership and completion fences.
//!
//! Before submission, the reactor must own the buffer, FD, and associated leases
//! in its in-flight table, independently of the waiting future. Dropping a future
//! only abandons its result; it cannot release submitted resources. Cancellation
//! requests do not release them: original and cancellation completion accounting
//! must both finish. Shutdown must fence kernel access before dropping the table.
//! The worker drives CQEs explicitly. Operation futures never run an executor.
//!
//! Degraded shutdown: a fatal driver error or expired Drop wait cannot establish a
//! kernel fence. In that case the bounded ring and its resource owners are deliberately
//! leaked for memory safety; shutdown is not successfully fenced. Explicit worker
//! shutdown must keep driving its fence and report failures rather than rely on Drop.

use crate::{Budget, Error, Operation, Result, Scope};
use descriptor::Descriptor;
use io_uring::{IoUring, opcode, squeue, types};

#[cfg(feature = "simulation")]
/// Deterministic resource and completion backend for simulation runs.
pub mod simulation;

/// Construct only the selected backend's submission, keeping simulated handles
/// out of host SQEs and preventing invented raw descriptor numbers.
macro_rules! submission {
    ($reactor:expr, $sim:expr, $real:expr) => {{
        #[cfg(feature = "simulation")]
        {
            let simulated = $reactor.state.borrow().simulation.is_some();
            if simulated {
                Submission::Sim($sim)
            } else {
                Submission::Real($real)
            }
        }
        #[cfg(not(feature = "simulation"))]
        {
            Submission::Real($real)
        }
    }};
}

/// Control-owned filesystem operations sharing this reactor's completion fences.
pub mod filesystem;
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    future::Future,
    net::SocketAddr,
    os::{
        fd::{AsRawFd, FromRawFd, OwnedFd as HostFd},
        unix::ffi::OsStrExt,
    },
    path::PathBuf,
    pin::Pin,
    rc::{Rc, Weak},
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    time::Duration,
};

/// Worker-local I/O owner, explicitly driven by [`Self::poll_budgeted`].
/// Dropping a waiting operation abandons delivery, never its kernel-owned resources.
pub struct Reactor<S: Scope, B: Budget> {
    rejection_observer: Option<Box<RejectionObserver<S>>>,

    environment: super::environment::Environment,

    budget: B,

    queue_entries: usize,

    state: RefCell<State<S>>,

    ordinary: Rc<Cell<usize>>,

    reserved: RefCell<Weak<SubmissionCapacity>>,
}

/// Caller-owned event sink, invoked only when admission fails.
type RejectionObserver<S> = dyn Fn(&S, SubmissionRejection);

/// Submission events describe rejected operations, not final client outcomes.
#[derive(Clone, Copy, Debug)]
pub enum RejectionReason {
    Initialization,
    ReservedFull,
    OrdinaryFull,
    Bookkeeping,
    EntriesFull,
    PartitionFull,
    IdExhausted,
    SubmissionQueueFull,
}

#[derive(Clone, Copy, Debug)]
pub enum SubmissionKind {
    OtherIo,
    Readiness,
    Timeout,
}

#[derive(Clone, Copy, Debug)]
pub struct SubmissionRejection {
    pub reason: RejectionReason,

    pub kind: SubmissionKind,

    pub reserved: bool,

    pub used: Option<usize>,

    pub limit: Option<usize>,

    pub requested: usize,
}

/// Startup-owned partition of the existing entry and bookkeeping ceilings.
/// The caller supplies the retained charge; this capability grants no data authorization.
pub struct SubmissionCapacity {
    memory: Rc<Charge>,

    active: Rc<Cell<usize>>,

    capacity: usize,
}

/// Maximum completion bookkeeping charged to one prepaid submission slot.
pub const SUBMISSION_BYTES: usize = 4096;

/// An owned address; the encoded sockaddr remains pinned in the in-flight owner.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum SocketAddress {
    /// IPv4 or IPv6 address, including port and any IPv6 scope identifier.
    Inet(SocketAddr),
    /// Filesystem pathname only. Abstract Unix addresses are unsupported; empty
    /// paths and embedded NUL bytes are rejected by both backends.
    Unix(PathBuf),
}

/// Cloneable cross-thread wake endpoint for worker command/crypto producers.
#[derive(Clone)]
pub struct ReactorWake {
    fd: Option<Arc<WakeState>>,
}

struct WakeState {
    fd: HostFd,
    owner: std::thread::ThreadId,
    local: AtomicBool,
    remote: AtomicBool,
}

impl AsRawFd for WakeState {
    fn as_raw_fd(&self) -> std::os::fd::RawFd {
        self.fd.as_raw_fd()
    }
}

/// Resources return to the caller only after all applicable completion fences.
/// On error or an abandoned future, the reactor releases them after those fences.
/// Data I/O retries up to eight zero-progress EINTR/EAGAIN results internally,
/// retaining owners between attempts. EAGAIN waits for readiness; each attempt
/// checks scope. Exhaustion returns the last errno and releases the owners.
/// Positive partial progress returns immediately, never repeats completed bytes.
pub struct Completion<B: 'static, L: 'static = ()> {
    /// Original allocation, returned only after all applicable CQEs.
    pub buffer: B,

    /// Bytes transferred by this operation, possibly less than the buffer length.
    pub bytes: usize,

    /// Caller-owned admission or reuse guard retained through completion.
    pub lease: L,
}

/// An exclusively owned, stable backing allocation with an independent lifetime.
///
/// # Safety
/// Moving the owner or calling either accessor must not relocate or resize its
/// backing allocation. Accessors expose the same initialized region, with no
/// independently accessible mutable aliases. Ownership includes the allocation's
/// quota reservation; neither may be freed or recycled before the final fence.
/// `'static` excludes request-scoped borrows; it does not require leaking memory.
/// Derived raw pointers must remain valid across owner moves into completion
/// closures. Stable addresses alone are insufficient: Box backing is retagged on
/// moves in Miri's aliasing models. Use private, non-resizing Vec storage or an
/// explicitly managed allocation; never reborrow its bytes while I/O is pending.
/// Inline arrays cannot implement this contract: moving them relocates the bytes.
pub unsafe trait IoBuffer: 'static {
    /// Application error returned when access to this allocation is rejected.
    type Error;

    /// Borrow the initialized region after its previous operation has fenced.
    fn bytes(&self) -> Result<&[u8], Self::Error>;

    /// Exclusively borrow the same region before deriving a receive pointer.
    fn bytes_mut(&mut self) -> Result<&mut [u8], Self::Error>;
}

/// Completion-owned immutable send storage. Shared aliases are permitted only
/// when none can mutate or relocate the initialized bytes. The owner retains
/// allocation admission through the final original/cancel completion fence.
/// This trait deliberately grants no receive or mutable-buffer capability.
///
/// # Safety
/// Accessors must expose the same initialized allocation across owner moves.
/// No shared alias may mutate, resize, or free the bytes before completion.
pub unsafe trait SendBuffer: 'static {
    /// Application error returned when access to this allocation is rejected.
    type Error;

    /// Borrow initialized immutable bytes for one send operation.
    fn send_bytes(&self) -> Result<&[u8], Self::Error>;
}

/// Completion-owned immutable storage for the body of a vectored file write.
///
/// This is the file-write counterpart of [`SendBuffer`] and carries the same
/// contract. It is blanket-implemented for every [`SendBuffer`] (and therefore
/// every [`IoBuffer`]), so shared, reference-counted bodies can be written
/// without copying them into exclusive storage.
///
/// # Safety
/// Accessors must expose the same initialized allocation across owner moves.
/// No shared alias may mutate, resize, or free the bytes before completion.
pub unsafe trait SharedWriteBuffer: 'static {
    /// Application error returned when access to this allocation is rejected.
    type Error;

    /// Borrow initialized immutable bytes for one write operation.
    fn write_bytes(&self) -> Result<&[u8], Self::Error>;
}

/// Owners returned by [`Reactor::write_vectored_at`] after all applicable
/// completion fences.
pub struct VectoredCompletion<H: 'static, B: 'static, L: 'static = ()> {
    /// Header allocation, written first.
    pub header: H,

    /// Body allocation, written immediately after the header.
    pub body: B,

    /// Bytes written across both segments, possibly less than their sum.
    pub bytes: usize,

    /// Caller-owned admission or reuse guard retained through completion.
    pub lease: L,
}

/// Erased admission guard retained for the lifetime of its accounted owner.
type Charge = Box<dyn std::any::Any>;

/// An operation prepared only for the selected backend.
enum Submission {
    Real(squeue::Entry),
    #[cfg(feature = "simulation")]
    Sim(simulation::Op),
}

/// One active slot in either the ordinary or prepaid submission partition.
struct SubmissionSlot {
    active: Rc<Cell<usize>>,

    _capacity: Option<Rc<SubmissionCapacity>>,
}

impl Drop for SubmissionSlot {
    /// Release exactly one admitted slot when its owning fence or reply retires.
    fn drop(&mut self) {
        self.active.set(self.active.get() - 1);
    }
}

/// Monotonic operation identifier; its high bit is reserved for cancel CQEs.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
struct IoId(u64);

const CANCEL_BIT: u64 = 1 << 63;

const MAX_WAIT: Duration = Duration::from_millis(10);

const DROP_WAIT: Duration = Duration::from_millis(100);

// At most nine data attempts, each separated by a worker-driven CQE. A positive
// partial result is never replayed. EAGAIN additionally requires a readiness CQE.
const DATA_RETRIES: usize = 8;

// Linux UAPI setup flags. Unknown modes still fail closed for suppression.
const SETUP_CQSIZE: u32 = 1 << 3;
const SETUP_SUBMIT_ALL: u32 = 1 << 7;
const SETUP_SINGLE_ISSUER: u32 = 1 << 12;
const SETUP_NORMAL: u32 = SETUP_CQSIZE | SETUP_SUBMIT_ALL | SETUP_SINGLE_ISSUER;

/// Backend state and owners that must survive until both applicable CQEs arrive.
struct State<S: Scope> {
    ring: Option<IoUring>,

    wake: Option<Arc<WakeState>>,

    #[cfg(feature = "simulation")]
    simulation: Option<simulation::Driver>,

    #[cfg(test)]
    submit_attempts: usize,

    #[cfg(test)]
    wait_attempts: usize,

    #[cfg(test)]
    submit_result: Option<std::io::Result<usize>>,

    #[cfg(test)]
    completions: std::collections::VecDeque<(u64, KernelResult)>,

    ring_reservation: Option<Charge>,

    entries: Table<Entry<S>>,

    next: u64,

    scan_after: IoId,

    stopped: bool,

    fence_waiters: BTreeMap<(Option<IoId>, u64), FenceWaiter>,

    next_waiter: u64,

    /// Reusable batch storage for poll_budgeted, empty between calls.
    finished: Vec<FencedEntry<S>>,

    /// Reusable cancellation scan snapshot, empty between calls.
    scan_batch: Vec<ScanItem<S>>,
}

/// One scanned entry: its identifier, a retained view, and its policy result.
type ScanItem<S> = (IoId, Rc<dyn Pending<S>>, Option<<S as Scope>::Error>);

/// Independently charged wake registration for one fence caller.
struct FenceWaiter {
    waker: Waker,

    _reservation: Charge,
}

impl FenceWaiter {
    /// Consume this registration and notify its executor outside reactor borrows.
    fn wake(self) {
        self.waker.wake();
    }
}

/// Cancellation request plus an independently removable completion notification.
struct Fence<'a, S: Scope, B: Budget> {
    reactor: &'a Reactor<S, B>,

    target: Option<IoId>,

    registration: Option<u64>,
}

impl<S: Scope, B: Budget> Future for Fence<'_, S, B> {
    /// Successful fencing or a scope-specific notification-admission error.
    type Output = Result<(), S::Error>;

    /// Request cancellation and register an independently removable fence waiter.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let cancelled = Error::Cancelled.into();
        let waker = cx.waker().clone();
        let mut state = this.reactor.state.borrow_mut();
        let pending = match this.target {
            Some(id) => match state.entries.get_mut(&id) {
                Some(entry) => {
                    entry.cancel_reason.get_or_insert(cancelled);
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
            let removed = this
                .registration
                .take()
                .and_then(|id| state.fence_waiters.remove(&(this.target, id)));
            drop(state);
            drop(removed);
            return Poll::Ready(Ok(()));
        }
        if let Some(id) = this.registration {
            let waiter = state
                .fence_waiters
                .get_mut(&(this.target, id))
                .expect("pending fence registration");
            let old = std::mem::replace(&mut waiter.waker, waker);
            drop(state);
            drop(old);
        } else {
            // Separate bounded control registrations support independent callers,
            // including callers using the same executor waker. Drop removes only
            // its own registration. No quota is needed to submit cancellation.
            if state.fence_waiters.len() >= this.reactor.queue_entries {
                drop(state);
                return Poll::Ready(Err(Error::Overloaded.into()));
            }
            let Some(next) = state.next_waiter.checked_add(1) else {
                drop(state);
                return Poll::Ready(Err(Error::Overloaded.into()));
            };
            let id = state.next_waiter;
            state.next_waiter = next;
            drop(state);
            let reservation = this.reactor.charge(std::mem::size_of::<FenceWaiter>())?;
            let mut state = this.reactor.state.borrow_mut();
            if state.fence_waiters.len() >= this.reactor.queue_entries {
                drop(state);
                return Poll::Ready(Err(Error::Overloaded.into()));
            }
            let pending = this.target.map_or(!state.entries.is_empty(), |id| {
                state.entries.contains_key(&id)
            });
            if !pending {
                drop(state);
                return Poll::Ready(Ok(()));
            }
            state.fence_waiters.insert(
                (this.target, id),
                FenceWaiter {
                    waker,
                    _reservation: reservation,
                },
            );
            this.registration = Some(id);
        }
        Poll::Pending
    }
}

impl<S: Scope, B: Budget> Drop for Fence<'_, S, B> {
    /// Remove only this caller's notification without undoing cancellation.
    fn drop(&mut self) {
        if let Some(id) = self.registration {
            let removed = self
                .reactor
                .state
                .borrow_mut()
                .fence_waiters
                .remove(&(self.target, id));
            drop(removed);
        }
    }
}

impl ReactorWake {
    /// Notify the worker without blocking; an already readable eventfd is enough.
    pub fn wake(&self) -> Result<()> {
        let Some(fd) = &self.fd else {
            return Ok(());
        };
        if std::thread::current().id() == fd.owner {
            // Preserve the next-turn notification without waking our own eventfd.
            fd.local.store(true, Ordering::Release);
            return Ok(());
        }
        if fd.remote.swap(true, Ordering::AcqRel) {
            return Ok(());
        }
        let value = 1u64;
        loop {
            // SAFETY: eventfd consumes this initialized, stack-local u64 synchronously.
            let result = unsafe { libc::write(fd.as_raw_fd(), (&value as *const u64).cast(), 8) };
            if result == 8 {
                return Ok(());
            }
            match std::io::Error::last_os_error().raw_os_error() {
                Some(libc::EINTR) => continue,
                Some(libc::EAGAIN) => return Ok(()), // Already readable.
                _ => {
                    fd.remote.store(false, Ordering::Release);
                    return Err(Error::Io);
                }
            }
        }
    }
}

/// Delivery state shared by the waiting future and completion owner.
struct Signal {
    abandoned: Cell<bool>,

    waker: RefCell<Option<Waker>>,
}

/// Bookkeeping charge owned by one reply, or shared with a prepaid partition.
enum Reservation {
    /// An ordinary submission's own charge, released with its reply.
    Owned(#[allow(dead_code)] Charge),

    /// The prepaid partition's charge, released with its last reply.
    Shared(#[allow(dead_code)] Rc<Charge>),
}

/// Fenced result and bookkeeping retained until consumed or abandoned.
struct Reply<T, E> {
    // A reserved slot includes completion bookkeeping, not just the SQE. Keep it
    // until the result is consumed/dropped as well as the kernel being fenced.
    _slot: Option<SubmissionSlot>,

    result: Option<Result<T, E>>,

    // Charge completed-but-unconsumed results as well as submitted work.
    _reservation: Reservation,
}

/// One submission's scope, delivery state, and completion owners, sharing one
/// allocation between the reactor table and the waiting future.
struct Submitted<S: Scope, T, F> {
    scope: S,

    signal: Signal,

    reply: RefCell<Reply<T, S::Error>>,

    // An ordinary slot is released at the kernel fence, before the callback.
    slot: Cell<Option<SubmissionSlot>>,

    // The callback owns every FD, buffer, lease and sockaddr backing.
    finish: Cell<Option<F>>,
}

/// The reactor table's type-erased view of one submission.
trait Pending<S: Scope> {
    /// Borrow the scope that decides cancellation.
    fn scope(&self) -> &S;

    /// Borrow the delivery signal shared with the waiting future.
    fn signal(&self) -> &Signal;

    /// Release the owners through the callback once and return the waker to notify.
    fn finish(&self, result: Result<KernelResult, S::Error>) -> Option<Waker>;
}

impl<S, T, F> Pending<S> for Submitted<S, T, F>
where
    S: Scope,
    F: FnOnce(Result<KernelResult, S::Error>) -> Result<T, S::Error>,
{
    /// Borrow the submission's own scope.
    fn scope(&self) -> &S {
        &self.scope
    }

    /// Borrow the signal stored beside the reply.
    fn signal(&self) -> &Signal {
        &self.signal
    }

    /// Deliver unless abandoned; an abandoned result drops its owners here.
    fn finish(&self, result: Result<KernelResult, S::Error>) -> Option<Waker> {
        drop(self.slot.take());
        let finish = self.finish.take().expect("submission finishes once");
        let result = finish(result);
        if !self.signal.abandoned.get() {
            self.reply.borrow_mut().result = Some(result);
        }
        let mut pending_waker = self.signal.waker.borrow_mut();
        let waker = pending_waker.take();
        drop(pending_waker);
        waker
    }
}

/// The waiting future's type-erased view of one submission.
trait Delivery<T, E> {
    /// Borrow the delivery signal.
    fn signal(&self) -> &Signal;

    /// Borrow the fenced reply.
    fn reply(&self) -> &RefCell<Reply<T, E>>;
}

impl<S: Scope, T, F> Delivery<T, S::Error> for Submitted<S, T, F> {
    /// Borrow the signal stored beside the reply.
    fn signal(&self) -> &Signal {
        &self.signal
    }

    /// Borrow the reply stored beside the signal.
    fn reply(&self) -> &RefCell<Reply<T, S::Error>> {
        &self.reply
    }
}

/// Result-only future; dropping it never releases submitted kernel resources.
struct Waiting<T, E> {
    delivery: Rc<dyn Delivery<T, E>>,
}

impl<T, E> Future for Waiting<T, E> {
    /// Fenced operation result with the caller's error classification.
    type Output = Result<T, E>;

    /// Consume a fenced reply or refresh delivery notification without driving I/O.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let result = self.delivery.reply().borrow_mut().result.take();
        if let Some(result) = result {
            Poll::Ready(result)
        } else {
            let waker = cx.waker().clone();
            let old = self.delivery.signal().waker.borrow_mut().replace(waker);
            drop(old);
            // Clone/drop callbacks can drive completions before registration.
            let result = self.delivery.reply().borrow_mut().result.take();
            match result {
                Some(result) => Poll::Ready(result),
                None => Poll::Pending,
            }
        }
    }
}

impl<T, E> Drop for Waiting<T, E> {
    /// Abandon delivery while leaving submitted owners in the reactor table.
    fn drop(&mut self) {
        self.delivery.signal().abandoned.set(true);
        let old = self.delivery.signal().waker.borrow_mut().take();
        drop(old);
    }
}

/// Scalar completion or an already owned descriptor from open/accept.
enum KernelResult {
    Value(i32),
    Accepted(Descriptor),
}

impl KernelResult {
    /// Preserve errno through zero-progress retries and application error mapping.
    fn io_result(self) -> std::io::Result<i32> {
        match self {
            Self::Value(value) if value >= 0 => Ok(value),
            Self::Value(value) => Err(std::io::Error::from_raw_os_error(
                value.checked_neg().unwrap_or(libc::EIO),
            )),
            Self::Accepted(_) => Err(std::io::Error::from_raw_os_error(libc::EIO)),
        }
    }

    /// Record failure attribution without consuming an accepted resource owner.
    fn observe_errno(&self, errno: &std::cell::Cell<Option<i32>>) {
        if let Self::Value(value) = self
            && *value < 0
        {
            errno.set(value.checked_neg());
        }
    }

    /// Translate a scalar completion into the runtime's error classification.
    fn value(self) -> Result<i32> {
        self.io_result().map_err(Error::from_io)
    }
}

/// One published operation with independent original and cancellation accounting.
struct Entry<S: Scope> {
    // The submission owns every FD, buffer, lease and sockaddr backing.
    pending: Rc<dyn Pending<S>>,

    completion: CompletionState,

    cancel_reason: Option<S::Error>,
}

/// Whether a successful original CQE transfers a descriptor or a scalar value.
#[derive(Clone, Copy)]
enum CompletionKind {
    /// A nonnegative completion is a byte count or syscall status.
    Value,

    /// A nonnegative host completion transfers one uniquely owned descriptor.
    Descriptor,
}

/// Cancellation accounting is independent of the reason delivery was canceled.
#[derive(Clone, Copy)]
enum CancellationFence {
    /// No cancellation SQE was published for this original operation.
    NotSubmitted,

    /// The cancellation SQE is published but its CQE has not arrived.
    Pending,

    /// The cancellation CQE arrived before the original CQE.
    Complete,
}

/// Own an original result only while its submitted cancellation still needs fencing.
enum CompletionState {
    /// The original CQE has not arrived; cancellation can finish independently.
    Pending {
        kind: CompletionKind,

        cancellation: CancellationFence,
    },

    /// The original result is owned, but the published cancellation is still pending.
    AwaitingCancel(KernelResult),

    /// The original owner was transferred to a fenced retirement capability.
    Retired,
}

impl CompletionState {
    /// Start original completion accounting with its precise ownership interpretation.
    fn new(kind: CompletionKind) -> Self {
        Self::Pending {
            kind,
            cancellation: CancellationFence::NotSubmitted,
        }
    }

    /// Cancellation can be published only before the original CQE and only once.
    fn needs_cancel(&self) -> bool {
        matches!(
            self,
            Self::Pending {
                cancellation: CancellationFence::NotSubmitted,
                ..
            }
        )
    }

    /// Record successful cancellation publication without claiming its completion.
    fn cancel_submitted(&mut self) {
        let Self::Pending { cancellation, .. } = self else {
            unreachable!("cancellation requires a pending original")
        };
        assert!(matches!(cancellation, CancellationFence::NotSubmitted));
        *cancellation = CancellationFence::Pending;
    }

    /// Account one CQE, transferring the original only when every required CQE arrived.
    fn complete(&mut self, cancel: bool, result: KernelResult) -> Result<Option<KernelResult>> {
        match self {
            Self::Pending { cancellation, .. }
                if cancel && matches!(cancellation, CancellationFence::Pending) =>
            {
                // Even ENOENT/EALREADY prove that the cancellation request finished.
                *cancellation = CancellationFence::Complete;
                Ok(None)
            }
            Self::AwaitingCancel(_) if cancel => {
                let Self::AwaitingCancel(original) = std::mem::replace(self, Self::Retired) else {
                    unreachable!("matched original owner")
                };
                Ok(Some(original))
            }
            Self::Pending { kind, cancellation } if !cancel => {
                let original = match result {
                    KernelResult::Value(fd)
                        if matches!(kind, CompletionKind::Descriptor) && fd >= 0 =>
                    {
                        // SAFETY: this original CQE transfers unique descriptor ownership.
                        KernelResult::Accepted(unsafe { Descriptor::from_raw_fd(fd) })
                    }
                    result => result,
                };
                if matches!(cancellation, CancellationFence::Pending) {
                    *self = Self::AwaitingCancel(original);
                    Ok(None)
                } else {
                    *self = Self::Retired;
                    Ok(Some(original))
                }
            }
            _ => Err(Error::Io),
        }
    }
}

/// Retirement authority carrying the original result after both applicable fences.
struct FencedEntry<S: Scope> {
    entry: Entry<S>,

    original: KernelResult,
}

impl<S: Scope> FencedEntry<S> {
    /// Deliver a fenced result with cancellation precedence outside reactor borrows.
    fn finish(self) -> Option<Waker> {
        let Self {
            mut entry,
            original,
        } = self;
        if entry.cancel_reason.is_none() {
            entry.cancel_reason = entry.pending.scope().check().err();
        }
        let result = match entry.cancel_reason {
            Some(error) => {
                drop(original);
                Err(error)
            }
            None => Ok(original),
        };
        entry.pending.finish(result)
    }
}

// SAFETY: exclusive stable storage also meets immutable send requirements.
unsafe impl<B: IoBuffer> SendBuffer for B {
    /// Preserve the exclusive allocation's application error type.
    type Error = B::Error;

    /// Borrow exclusive storage immutably under the stronger IoBuffer contract.
    fn send_bytes(&self) -> Result<&[u8], Self::Error> {
        self.bytes()
    }
}

// SAFETY: the SendBuffer contract is the SharedWriteBuffer contract.
unsafe impl<B: SendBuffer> SharedWriteBuffer for B {
    /// Preserve the send allocation's application error type.
    type Error = B::Error;

    /// Borrow the same immutable bytes a send would publish.
    fn write_bytes(&self) -> Result<&[u8], Self::Error> {
        self.send_bytes()
    }
}

/// Reactor-owned vectored write state. The iovec array lives in stable
/// syscall backing so its address survives moves into completion closures.
struct VectoredInFlight<H: 'static, B: 'static, L: 'static> {
    file: Rc<Descriptor>,

    header: H,

    body: B,

    lease: L,

    iovecs: SyscallArg<[libc::iovec; 2]>,
}

impl<H: 'static, B: 'static, L: 'static> VectoredInFlight<H, B, L> {
    /// Return both owners and admission only after the retry loop's final fence.
    fn complete(self, bytes: usize) -> VectoredCompletion<H, B, L> {
        let Self {
            file,
            header,
            body,
            lease,
            iovecs,
        } = self;
        drop((file, iovecs));
        VectoredCompletion {
            header,
            body,
            bytes,
            lease,
        }
    }
}

/// Reactor-owned submission state. `L` retains connection/segment/other leases.
/// Stored before the kernel can see any pointer, including across partial I/O.
struct InFlight<B: 'static, L: 'static> {
    file: Rc<Descriptor>,

    buffer: B,

    lease: L,
}

impl<B: 'static, L: 'static> InFlight<B, L> {
    /// Return storage and admission only after the retry loop's final fence.
    fn complete(self, bytes: usize) -> Completion<B, L> {
        let Self {
            file,
            buffer,
            lease,
        } = self;
        drop(file);
        Completion {
            buffer,
            bytes,
            lease,
        }
    }
}

impl<S: Scope, Q: Budget> Reactor<S, Q> {
    /// Create an uninitialized reactor; kernel resources are acquired by [`Self::init`].
    pub fn new(queue_entries: usize, budget: Q) -> Self {
        Self {
            rejection_observer: None,
            environment: super::environment::Environment::current(),
            budget,
            queue_entries,
            ordinary: Rc::new(Cell::new(0)),
            reserved: RefCell::default(),
            state: RefCell::new(State {
                ring: None,
                wake: None,
                #[cfg(feature = "simulation")]
                simulation: simulation::Simulation::current().map(simulation::Driver::new),
                #[cfg(test)]
                submit_attempts: 0,
                #[cfg(test)]
                wait_attempts: 0,
                #[cfg(test)]
                submit_result: None,
                #[cfg(test)]
                completions: Default::default(),
                ring_reservation: None,
                entries: Table::default(),
                next: 1,
                scan_after: IoId(0),
                stopped: false,
                fence_waiters: BTreeMap::new(),
                next_waiter: 0,
                finished: Vec::new(),
                scan_batch: Vec::new(),
            }),
        }
    }

    /// Observe rejected submissions only, without changing the returned error.
    pub fn with_rejection_observer(
        mut self,
        observer: impl Fn(&S, SubmissionRejection) + 'static,
    ) -> Self {
        self.rejection_observer = Some(Box::new(observer));
        self
    }

    fn rejected(&self, scope: &S, facts: SubmissionRejection) {
        if let Some(observer) = &self.rejection_observer {
            observer(scope, facts);
        }
    }

    /// Initialize the selected backend once.
    pub fn init(&self) -> Result<()> {
        let state = self.state.borrow();
        if state.stopped {
            return Err(Error::Unavailable);
        }
        if state.ring_reservation.is_some() {
            return Ok(());
        }
        drop(state);
        let capacity = self.queue_entries;
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
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
        let ring_reservation = self.charge(ring_bytes)?;
        let mut state = self.state.borrow_mut();
        // The accounting hook may reenter and initialize or stop the reactor.
        if state.stopped || state.ring_reservation.is_some() {
            let stopped = state.stopped;
            drop(state);
            return if stopped {
                Err(Error::Unavailable)
            } else {
                Ok(())
            };
        }
        #[cfg(feature = "simulation")]
        if state.simulation.is_some() {
            state.ring_reservation = Some(ring_reservation);
            return Ok(());
        }
        let ring = probe_ring(|flags| {
            let mut builder = IoUring::builder();
            builder.setup_cqsize(cq);
            if flags & SETUP_SUBMIT_ALL != 0 {
                builder.setup_submit_all();
            }
            if flags & SETUP_SINGLE_ISSUER != 0 {
                builder.setup_single_issuer();
            }
            builder.build(sq)
        })
        .map_err(|_| Error::Io)?;
        // SAFETY: no borrowed pointers; ownership of the returned descriptor is unique.
        let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        if fd < 0 {
            return Err(Error::Io);
        }
        state.wake = Some(Arc::new(WakeState {
            fd: unsafe { HostFd::from_raw_fd(fd) },
            owner: std::thread::current().id(),
            local: AtomicBool::new(false),
            remote: AtomicBool::new(false),
        }));
        state.ring_reservation = Some(ring_reservation);
        state.ring = Some(ring);
        Ok(())
    }

    /// Obtain after init, and attach to external worker queues before starting them.
    pub fn waker(&self) -> Result<ReactorWake> {
        self.init()?;
        Ok(ReactorWake {
            fd: self.state.borrow().wake.clone(),
        })
    }

    /// Count operations still awaiting their original or cancellation CQE.
    pub fn in_flight(&self) -> usize {
        self.state.borrow().entries.len()
    }

    /// Partition existing queue capacity using a caller-owned prepaid charge.
    /// Only one live partition is allowed. Its slots include unread completions;
    /// ordinary slots instead become reusable at the kernel fence.
    pub fn reserve_submissions<C: 'static>(
        &self,
        capacity: usize,
        charge: C,
    ) -> Result<Rc<SubmissionCapacity>> {
        if self.reserved.borrow().upgrade().is_some()
            || self.ordinary.get() > self.queue_entries.saturating_sub(capacity)
            || capacity > self.queue_entries
        {
            return Err(Error::InvalidConfiguration);
        }
        self.init()?;
        let reserved = Rc::new(SubmissionCapacity {
            memory: Rc::new(Box::new(charge)),
            active: Rc::new(Cell::new(0)),
            capacity,
        });
        *self.reserved.borrow_mut() = Rc::downgrade(&reserved);
        Ok(reserved)
    }

    /// Retain a budget guard independently of the concrete accounting implementation.
    fn charge(&self, bytes: usize) -> Result<Charge> {
        self.budget
            .charge(bytes)
            .map(|charge| Box::new(charge) as Charge)
    }

    /// Admit ordinary work, transferring all backing into a completion closure.
    fn submit<T: 'static>(
        &self,
        sqe: Submission,
        scope: &S,
        accept: bool,
        finish: impl FnOnce(Result<KernelResult, S::Error>) -> Result<T, S::Error> + 'static,
    ) -> Result<Waiting<T, S::Error>, S::Error> {
        self.submit_reserved(sqe, scope, accept, None, finish)
    }

    /// Publish only after retaining owners, using ordinary or prepaid bookkeeping.
    fn submit_reserved<T: 'static>(
        &self,
        sqe: Submission,
        scope: &S,
        accept: bool,
        capacity: Option<Rc<SubmissionCapacity>>,
        finish: impl FnOnce(Result<KernelResult, S::Error>) -> Result<T, S::Error> + 'static,
    ) -> Result<Waiting<T, S::Error>, S::Error> {
        self.submit_reserved_kind(
            sqe,
            scope,
            accept,
            capacity,
            SubmissionKind::OtherIo,
            finish,
        )
    }

    fn submit_reserved_kind<T: 'static>(
        &self,
        sqe: Submission,
        scope: &S,
        accept: bool,
        capacity: Option<Rc<SubmissionCapacity>>,
        kind: SubmissionKind,
        finish: impl FnOnce(Result<KernelResult, S::Error>) -> Result<T, S::Error> + 'static,
    ) -> Result<Waiting<T, S::Error>, S::Error> {
        scope.check()?;
        let reserved_partition = capacity.is_some();
        let reject = |reason, used, limit, requested| {
            self.rejected(
                scope,
                SubmissionRejection {
                    reason,
                    kind,
                    reserved: reserved_partition,
                    used,
                    limit,
                    requested,
                },
            );
        };
        self.init().inspect_err(|error| {
            if *error == Error::Overloaded {
                reject(RejectionReason::Initialization, None, None, 1);
            }
        })?;
        // Clone before borrowing reactor state: a scope's Clone may reenter.
        let scope = scope.clone();
        let bytes = std::mem::size_of::<Entry<S>>()
            + std::mem::size_of::<S>()
            + 2 * std::mem::size_of::<usize>()
            + std::mem::size_of::<Reply<T, S::Error>>()
            + std::mem::size_of_val(&finish)
            + std::mem::size_of::<Signal>()
            + std::mem::size_of::<libc::sockaddr_storage>();
        let (reservation, active) = if let Some(pool) = &capacity {
            if !self
                .reserved
                .borrow()
                .upgrade()
                .is_some_and(|own| Rc::ptr_eq(&own, pool))
                || bytes > SUBMISSION_BYTES
            {
                return Err(Error::InvalidConfiguration.into());
            }
            if pool.active.get() >= pool.capacity {
                reject(
                    RejectionReason::ReservedFull,
                    Some(pool.active.get()),
                    Some(pool.capacity),
                    1,
                );
                return Err(Error::Overloaded.into());
            }
            (
                Reservation::Shared(pool.memory.clone()),
                pool.active.clone(),
            )
        } else {
            let reserved = self
                .reserved
                .borrow()
                .upgrade()
                .map_or(0, |pool| pool.capacity);
            if self.ordinary.get() >= self.queue_entries - reserved {
                reject(
                    RejectionReason::OrdinaryFull,
                    Some(self.ordinary.get()),
                    Some(self.queue_entries - reserved),
                    1,
                );
                return Err(Error::Overloaded.into());
            }
            let charge = self.charge(bytes).inspect_err(|error| {
                if *error == Error::Overloaded {
                    reject(RejectionReason::Bookkeeping, None, None, bytes);
                }
            })?;
            (Reservation::Owned(charge), self.ordinary.clone())
        };
        let mut state = self.state.borrow_mut();
        if state.stopped {
            drop(state);
            return Err(Error::Unavailable.into());
        }
        let available = capacity.as_ref().map_or_else(
            || self.queue_entries - self.reserved.borrow().upgrade().map_or(0, |p| p.capacity),
            |pool| pool.capacity,
        );
        if state.entries.len() >= self.queue_entries || active.get() >= available {
            let (reason, used, limit) = if state.entries.len() >= self.queue_entries {
                (
                    RejectionReason::EntriesFull,
                    state.entries.len(),
                    self.queue_entries,
                )
            } else {
                (RejectionReason::PartitionFull, active.get(), available)
            };
            drop(state);
            reject(reason, Some(used), Some(limit), 1);
            return Err(Error::Overloaded.into());
        }
        let id = IoId(state.next);
        if id.0 >= CANCEL_BIT {
            drop(state);
            reject(RejectionReason::IdExhausted, None, None, 1);
            return Err(Error::Overloaded.into());
        }
        state.next += 1;
        active.set(active.get() + 1);
        let slot = SubmissionSlot {
            active,
            _capacity: capacity,
        };
        // Ordinary entries release their queue slot at the kernel fence, as
        // before. Only the prepaid partition must also bound unconsumed replies.
        let (ordinary_slot, reserved_slot) = if slot._capacity.is_some() {
            (None, Some(slot))
        } else {
            (Some(slot), None)
        };
        let submitted = Rc::new(Submitted {
            scope,
            signal: Signal {
                abandoned: Cell::new(false),
                waker: RefCell::new(None),
            },
            reply: RefCell::new(Reply {
                _slot: reserved_slot,
                result: None,
                _reservation: reservation,
            }),
            slot: Cell::new(ordinary_slot),
            finish: Cell::new(Some(finish)),
        });
        // Insert ownership BEFORE publishing the SQE. No fallible action may drop
        // this entry after publication; even submission errors leave it retained.
        state.entries.insert(
            id,
            Entry {
                pending: submitted.clone(),
                completion: CompletionState::new(if accept {
                    CompletionKind::Descriptor
                } else {
                    CompletionKind::Value
                }),
                cancel_reason: None,
            },
        );
        // SAFETY: the inserted entry owns all SQE backing until both CQEs arrive.
        let published = match sqe {
            #[cfg(feature = "simulation")]
            Submission::Sim(op) => state
                .simulation
                .as_mut()
                .expect("simulation selected")
                .push(id.0, op),
            Submission::Real(sqe) => unsafe {
                state
                    .ring
                    .as_mut()
                    .unwrap()
                    .submission()
                    .push(&sqe.user_data(id.0))
                    .map_err(|_| ())
            },
        };
        if published.is_err() {
            let entry = state.entries.remove(&id);
            drop(state);
            drop(entry);
            reject(RejectionReason::SubmissionQueueFull, None, None, 1);
            return Err(Error::Overloaded.into());
        }
        // Kernel submission is driven by poll_budgeted and by wait before sleeping,
        // including SQEs queued in the intervening service turn. Polling a future
        // only queues work; potentially blocking file operations use ASYNC.
        Ok(Waiting {
            delivery: submitted,
        })
    }

    /// Only read/write/recv/send may use this helper. Connect and filesystem
    /// mutations have distinct side effects and must not be automatically replayed.
    async fn retry_data<O: 'static>(
        &self,
        mut owned: O,
        fd: Rc<Descriptor>,
        scope: &S,
        capacity: Option<Rc<SubmissionCapacity>>,
        interest: u32,
        mut prepare: impl FnMut(&mut O) -> Result<Submission, S::Error>,
    ) -> Result<(O, usize), S::Error> {
        for attempt in 0..=DATA_RETRIES {
            scope.check()?;
            let sqe = prepare(&mut owned)?;
            let (returned, result) = self
                .submit_reserved(sqe, scope, false, capacity.clone(), move |result| {
                    // Even a failed data CQE returns its owners internally. Scope
                    // cancellation still wins, but only after both CQE fences.
                    Ok((owned, result?.value()))
                })?
                .await?;
            owned = returned;
            let error = match result {
                Ok(bytes) => return Ok((owned, bytes as usize)),
                Err(error) => error,
            };
            if attempt == DATA_RETRIES || !matches!(error, Error::Os(libc::EINTR | libc::EAGAIN)) {
                return Err(error.into());
            }
            scope.check()?;
            if error == Error::Os(libc::EAGAIN) {
                let sqe = self.readiness_submission(&fd, interest);
                // Do not leave the buffer/lease in the waiting future while a
                // readiness SQE is pending. Abandonment must retain them too.
                let retained_fd = fd.clone();
                owned = self
                    .submit_reserved_kind(
                        sqe,
                        scope,
                        false,
                        capacity.clone(),
                        SubmissionKind::Readiness,
                        move |result| {
                            drop(retained_fd);
                            result?.value()?;
                            Ok(owned)
                        },
                    )?
                    .await?;
            }
        }
        unreachable!("bounded retries return on the final attempt")
    }

    /// Derive one bounded region, borrowing exclusively only for kernel output.
    fn buffer_region<B: IoBuffer>(
        buffer: &mut B,
        operation: BufferOperation,
    ) -> Result<(*mut u8, u32), S::Error>
    where
        S::Error: From<B::Error>,
    {
        let (ptr, length) = match operation {
            BufferOperation::Read(_) | BufferOperation::Recv => {
                let bytes = buffer.bytes_mut()?;
                (bytes.as_mut_ptr(), bytes.len())
            }
            BufferOperation::Write(_) | BufferOperation::Send => {
                let bytes = buffer.bytes()?;
                (bytes.as_ptr().cast_mut(), bytes.len())
            }
        };
        let len = u32::try_from(length).map_err(|_| Error::InvalidInput)?;
        Ok((ptr, len))
    }

    /// Prepare owned positioned or socket I/O using only the selected backend.
    fn buffer_submission<B: IoBuffer, L: 'static>(
        &self,
        owned: &mut InFlight<B, L>,
        operation: BufferOperation,
        policy: IssuePolicy,
    ) -> Result<Submission, S::Error>
    where
        S::Error: From<B::Error>,
    {
        Ok(submission!(
            self,
            {
                let (ptr, len) = Self::buffer_region(&mut owned.buffer, operation)?;
                simulation::Op::Buffer {
                    fd: owned.file.clone(),
                    operation,
                    ptr,
                    len: len as usize,
                }
            },
            {
                let fd = types::Fd(owned.file.as_raw_fd());
                let (ptr, len) = Self::buffer_region(&mut owned.buffer, operation)?;
                match operation {
                    BufferOperation::Read(offset) => opcode::Read::new(fd, ptr, len)
                        .offset(offset)
                        .build()
                        .flags(policy.flags()),
                    BufferOperation::Write(offset) => opcode::Write::new(fd, ptr, len)
                        .offset(offset)
                        .build()
                        .flags(policy.flags()),
                    BufferOperation::Recv => opcode::Recv::new(fd, ptr, len).build(),
                    BufferOperation::Send => opcode::Send::new(fd, ptr, len)
                        .flags(libc::MSG_NOSIGNAL)
                        .build(),
                }
            }
        ))
    }

    /// Retain exclusive storage and admission through retries and return fenced progress.
    #[allow(clippy::too_many_arguments)]
    fn buffer_io<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
        operation: BufferOperation,
        policy: IssuePolicy,
        capacity: Option<Rc<SubmissionCapacity>>,
    ) -> Operation<'a, Completion<B, L>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        Box::pin(async move {
            scope.check()?;
            if matches!(operation, BufferOperation::Read(offset) | BufferOperation::Write(offset) if offset > i64::MAX as u64)
            {
                return Err(Error::InvalidInput.into());
            }
            let owned = InFlight {
                file,
                buffer,
                lease,
            };
            // Positioned device I/O tries inline nonblocking issue; the kernel
            // offloads if needed. Cold buffered filesystem callers force ASYNC.
            // Socket opcodes always keep native nonblocking issue/poll behavior.
            let fd = owned.file.clone();
            let interest = match operation {
                BufferOperation::Read(_) | BufferOperation::Recv => libc::POLLIN,
                BufferOperation::Write(_) | BufferOperation::Send => libc::POLLOUT,
            } as u32;
            let (owned, bytes) = self
                .retry_data(owned, fd, scope, capacity, interest, |owned| {
                    self.buffer_submission(owned, operation, policy)
                })
                .await?;
            Ok(owned.complete(bytes))
        })
    }

    /// Transfer the FD, buffer, and any reuse-preventing lease (`()` if none).
    /// Owned resources can move from one completed operation to the next:
    /// ```no_run
    /// use std::rc::Rc;
    /// use uring_runtime::{Budget, Scope, Result, reactor::{descriptor::Descriptor, IoBuffer, Completion, Reactor}};
    /// async fn copy<S: Scope, Q: Budget, B: IoBuffer, L: 'static>(
    ///     reactor: &Reactor<S,Q>, fd: Rc<Descriptor>, buffer: B,
    ///     lease: L, scope: &S) -> Result<Completion<B,L>, S::Error>
    /// where S::Error: From<B::Error> {
    ///     let read = reactor.read_at(fd.clone(), 0, buffer, lease, scope).await?;
    ///     reactor.write_at(fd, 0, read.buffer, read.lease, scope).await
    /// }
    /// ```
    /// The retained lease cannot borrow from the waiting future's caller:
    /// ```compile_fail
    /// use std::rc::Rc;
    /// use uring_runtime::{Budget, Scope, reactor::{descriptor::Descriptor, IoBuffer, Reactor}};
    /// fn borrowed<S: Scope, Q: Budget, B: IoBuffer>(
    ///     reactor: &Reactor<S,Q>, fd: Rc<Descriptor>, buffer: B,
    ///     lease: &u8, scope: &S) where S::Error: From<B::Error> {
    ///     let _future = reactor.read_at(fd, 0, buffer, lease, scope);
    /// }
    /// ```
    pub fn read_at<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        self.buffer_io(
            file,
            buffer,
            lease,
            scope,
            BufferOperation::Read(offset),
            IssuePolicy::Inline,
            None,
        )
    }

    /// Write at an explicit offset, returning short progress without replaying bytes.
    pub fn write_at<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        self.buffer_io(
            file,
            buffer,
            lease,
            scope,
            BufferOperation::Write(offset),
            IssuePolicy::Inline,
            None,
        )
    }

    /// Write `header` then `body` contiguously at an explicit offset with one
    /// vectored write, without copying either into a staging buffer.
    ///
    /// Both owners and the lease are retained until every completion fence.
    /// Short progress is returned as is and may end inside either segment;
    /// zero-progress `EINTR`/`EAGAIN` is retried like [`Self::write_at`]. A
    /// combined length above `u32::MAX`, or an offset above `i64::MAX`, fails
    /// with [`Error::InvalidInput`].
    pub fn write_vectored_at<'a, H: IoBuffer, T: SharedWriteBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        header: H,
        body: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, VectoredCompletion<H, T, L>, S::Error>
    where
        S::Error: From<H::Error> + From<T::Error>,
    {
        Box::pin(async move {
            scope.check()?;
            if offset > i64::MAX as u64 {
                return Err(Error::InvalidInput.into());
            }
            let empty = libc::iovec {
                iov_base: std::ptr::null_mut(),
                iov_len: 0,
            };
            let owned = VectoredInFlight {
                file,
                header,
                body,
                lease,
                iovecs: SyscallArg::new([empty; 2]),
            };
            let fd = owned.file.clone();
            let (owned, bytes) = self
                .retry_data(owned, fd, scope, None, libc::POLLOUT as u32, |owned| {
                    self.vectored_submission(owned, offset)
                })
                .await?;
            Ok(owned.complete(bytes))
        })
    }

    /// Point the retained iovecs at both segments and prepare one positioned writev.
    fn vectored_submission<H: IoBuffer, T: SharedWriteBuffer, L: 'static>(
        &self,
        owned: &mut VectoredInFlight<H, T, L>,
        offset: u64,
    ) -> Result<Submission, S::Error>
    where
        S::Error: From<H::Error> + From<T::Error>,
    {
        let header = owned.header.bytes()?;
        let body = owned.body.write_bytes()?;
        if header
            .len()
            .checked_add(body.len())
            .is_none_or(|total| u32::try_from(total).is_err())
        {
            return Err(Error::InvalidInput.into());
        }
        let segments = [
            libc::iovec {
                iov_base: header.as_ptr().cast_mut().cast(),
                iov_len: header.len(),
            },
            libc::iovec {
                iov_base: body.as_ptr().cast_mut().cast(),
                iov_len: body.len(),
            },
        ];
        // SAFETY: the one-element backing is initialized and exclusively owned;
        // the kernel only reads it after publication below.
        unsafe { owned.iovecs.as_mut_ptr().write(segments) };
        let iovecs = owned.iovecs.as_ptr().cast::<libc::iovec>();
        Ok(submission!(
            self,
            simulation::Op::Writev {
                fd: owned.file.clone(),
                offset,
                iovecs,
                count: 2,
            },
            opcode::Writev::new(types::Fd(owned.file.as_raw_fd()), iovecs, 2)
                .offset(offset)
                .build()
                .flags(IssuePolicy::Inline.flags())
        ))
    }

    /// Receive into owned storage while retaining the caller's lease through fencing.
    pub fn recv<'a, B: IoBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        self.buffer_io(
            fd,
            buffer,
            lease,
            scope,
            BufferOperation::Recv,
            IssuePolicy::Inline,
            None,
        )
    }

    /// Receive using prepaid capacity held until the fenced reply is consumed or dropped.
    pub fn recv_reserved<'a, B: IoBuffer>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, Completion<B>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        self.buffer_io(
            fd,
            buffer,
            (),
            scope,
            BufferOperation::Recv,
            IssuePolicy::Inline,
            Some(capacity),
        )
    }

    /// Send using prepaid capacity held until the fenced reply is consumed or dropped.
    pub fn send_reserved<'a, B: IoBuffer>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, Completion<B>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        self.buffer_io(
            fd,
            buffer,
            (),
            scope,
            BufferOperation::Send,
            IssuePolicy::Inline,
            Some(capacity),
        )
    }

    /// Send owned immutable storage, retaining its lease and suppressing SIGPIPE.
    pub fn send<'a, B: SendBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error>
    where
        S::Error: From<B::Error>,
    {
        Box::pin(async move {
            let retained_fd = fd.clone();
            let (owned, bytes) = self
                .retry_data(
                    InFlight {
                        file: fd,
                        buffer,
                        lease,
                    },
                    retained_fd,
                    scope,
                    None,
                    libc::POLLOUT as u32,
                    |owned| {
                        let bytes = owned.buffer.send_bytes()?;
                        let len = u32::try_from(bytes.len()).map_err(|_| Error::InvalidInput)?;
                        let sqe = submission!(
                            self,
                            simulation::Op::Buffer {
                                fd: owned.file.clone(),
                                operation: BufferOperation::Send,
                                // Simulation creates a shared slice for Send, never a mutable one.
                                ptr: bytes.as_ptr().cast_mut(),
                                len: len as usize,
                            },
                            opcode::Send::new(
                                types::Fd(owned.file.as_raw_fd()),
                                bytes.as_ptr(),
                                len
                            )
                            .flags(libc::MSG_NOSIGNAL)
                            .build()
                        );
                        Ok(sqe)
                    },
                )
                .await?;
            Ok(owned.complete(bytes))
        })
    }

    /// Select a readiness opcode without converting simulated owners to host FDs.
    fn readiness_submission(&self, fd: &Rc<Descriptor>, interest: u32) -> Submission {
        submission!(
            self,
            simulation::Op::Poll {
                fd: fd.clone(),
                interest
            },
            opcode::PollAdd::new(types::Fd(fd.as_raw_fd()), interest).build()
        )
    }

    /// Wait for a supported readiness mask while retaining the descriptor owner.
    pub fn readiness<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        interest: u32,
        scope: &'a S,
    ) -> Operation<'a, u32, S::Error> {
        self.readiness_with_lease(fd, interest, (), scope)
    }

    /// Retain progress admission alongside the descriptor until all CQE fences.
    pub fn readiness_with_lease<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        interest: u32,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, u32, S::Error> {
        self.readiness_capacity(fd, interest, lease, None, scope)
    }

    /// Wait with prepaid capacity and retain the descriptor and lease until fenced.
    pub fn readiness_reserved<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        interest: u32,
        lease: L,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, u32, S::Error> {
        self.readiness_capacity(fd, interest, lease, Some(capacity), scope)
    }

    fn readiness_capacity<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        interest: u32,
        lease: L,
        capacity: Option<Rc<SubmissionCapacity>>,
        scope: &'a S,
    ) -> Operation<'a, u32, S::Error> {
        Box::pin(async move {
            if interest == 0
                || interest
                    & !((libc::POLLIN
                        | libc::POLLOUT
                        | libc::POLLRDHUP
                        | libc::POLLHUP
                        | libc::POLLERR) as u32)
                    != 0
            {
                return Err(Error::InvalidInput.into());
            }
            let sqe = self.readiness_submission(&fd, interest);
            self.submit_reserved_kind(
                sqe,
                scope,
                false,
                capacity,
                SubmissionKind::Readiness,
                move |result| {
                    drop(fd);
                    drop(lease);
                    Ok(result?.value()? as u32)
                },
            )?
            .await
        })
    }

    /// Accept one nonblocking, close-on-exec socket into a typed owner.
    pub fn accept<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, Descriptor, S::Error> {
        self.accept_reserved(fd, None, scope)
    }

    /// Accept with optional prepaid capacity, retaining unread accepted descriptors.
    pub fn accept_reserved<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        capacity: Option<Rc<SubmissionCapacity>>,
        scope: &'a S,
    ) -> Operation<'a, Descriptor, S::Error> {
        Box::pin(async move {
            let sqe = submission!(
                self,
                simulation::Op::Accept(fd.clone()),
                opcode::Accept::new(
                    types::Fd(fd.as_raw_fd()),
                    std::ptr::null_mut(),
                    std::ptr::null_mut(),
                )
                .flags(libc::SOCK_CLOEXEC | libc::SOCK_NONBLOCK)
                .build()
            );
            self.submit_reserved(sqe, scope, true, capacity, move |result| {
                drop(fd);
                match result? {
                    KernelResult::Accepted(fd) => Ok(fd),
                    value => {
                        value.value()?;
                        Err(Error::Io.into())
                    }
                }
            })?
            .await
        })
    }

    /// Connect once to an owned address; connection side effects are never retried.
    pub fn connect<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        address: SocketAddress,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        self.connect_with_lease(fd, address, (), scope)
    }

    /// Transfer the complete connection/admission owner before submission. Return
    /// it only after the original and any cancellation CQE are fenced. A dropped
    /// connect future cannot return its endpoint slot or quota prematurely.
    pub fn connect_with_lease<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        address: SocketAddress,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, L, S::Error> {
        self.connect_with_observation(fd, address, lease, None, scope)
    }

    /// Preserve connect errno locally for attribution, without changing boundary errors.
    pub fn connect_with_observation<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        address: SocketAddress,
        lease: L,
        errno: Option<Rc<std::cell::Cell<Option<i32>>>>,
        scope: &'a S,
    ) -> Operation<'a, L, S::Error> {
        Box::pin(async move {
            #[cfg(feature = "simulation")]
            let destination = address.clone();
            let (address, len) = encode_address(address)?;
            let sqe = submission!(
                self,
                simulation::Op::Connect {
                    fd: fd.clone(),
                    address: destination
                },
                opcode::Connect::new(types::Fd(fd.as_raw_fd()), address.as_ptr().cast(), len,)
                    .build()
            );
            self.submit(sqe, scope, false, move |result| {
                drop((fd, address));
                if let (Some(errno), Ok(result)) = (&errno, &result) {
                    result.observe_errno(errno);
                }
                result?.value()?;
                Ok(lease)
            })?
            .await
        })
    }

    /// Process at most `budget` CQEs and inspect at most `budget` cancellation
    /// candidates, round-robin. Returns CQEs consumed, including cancel CQEs.
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        let _environment = self.environment.enter();
        if budget == 0 {
            return Ok(0);
        }
        let mut state = self.state.borrow_mut();
        if state.ring_reservation.is_none() {
            return Ok(0);
        }
        // Reuse batch storage; a reentrant call finds it taken and allocates.
        let mut finished = std::mem::take(&mut state.finished);
        let mut fence_wakes = Vec::new();
        let mut failure = None;
        let mut completed = 0;
        while completed < budget {
            let cqe = state.next_completion();
            let Some((tag, result)) = cqe else {
                break;
            };
            completed += 1;
            match state.complete(tag, result) {
                Ok(Some(entry)) => {
                    finished.push(entry);
                    fence_wakes.extend(state.take_fence_wakers(Some(IoId(tag & !CANCEL_BIT))));
                }
                Ok(None) => (),
                Err(error) => {
                    state.stopped = true;
                    failure = Some(error);
                    break;
                }
            }
        }
        if state.entries.is_empty() {
            fence_wakes.extend(state.take_fence_wakers(None));
        }
        drop(state);
        // One panicking callback must not discard unrelated completed replies or
        // notifications. Finish the batch outside all reactor borrows, then unwind.
        let mut panic = None;
        for entry in finished.drain(..) {
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                if let Some(waker) = entry.finish() {
                    waker.wake();
                }
            }));
            if let Err(payload) = result {
                panic.get_or_insert(payload);
            }
        }
        {
            let mut state = self.state.borrow_mut();
            if state.finished.capacity() == 0 {
                state.finished = finished;
            }
        }
        for waiter in fence_wakes {
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| waiter.wake()));
            if let Err(payload) = result {
                panic.get_or_insert(payload);
            }
        }
        if let Some(payload) = panic {
            std::panic::resume_unwind(payload);
        }
        if let Some(error) = failure {
            return Err(error);
        }
        self.queue_cancellations(budget);
        let mut state = self.state.borrow_mut();
        let submitted = state.submit_pending();
        if submitted.is_err() {
            state.stopped = true;
        }
        submitted.map(|()| completed)
    }

    /// Queue ASYNC_CANCEL for up to `budget` in-flight operations whose scope was
    /// cancelled, resuming the round-robin scan where the previous call stopped.
    fn queue_cancellations(&self, budget: usize) {
        // Snapshot the next round-robin slice in one borrow, reusing the scratch
        // storage; a reentrant scan finds it taken and allocates its own.
        let mut batch = {
            let mut state = self.state.borrow_mut();
            let count = budget.min(state.entries.len());
            if count == 0 {
                return;
            }
            let mut batch = std::mem::take(&mut state.scan_batch);
            let after = state.scan_after;
            let next = state
                .entries
                .after(after)
                .take(count)
                .map(|(id, entry)| (*id, entry.pending.clone(), None));
            batch.extend(next);
            if let Some((last, _, _)) = batch.last() {
                state.scan_after = *last;
            }
            batch
        };
        // Policy checks run outside reactor borrows and may reenter.
        for (_, pending, checked) in &mut batch {
            *checked = pending.scope().check().err();
        }
        {
            let mut state = self.state.borrow_mut();
            let stopped = state.stopped;
            for (id, _, checked) in &mut batch {
                let id = *id;
                let Some(entry) = state.entries.get_mut(&id) else {
                    continue;
                };
                if entry.cancel_reason.is_none() {
                    entry.cancel_reason = if stopped || entry.pending.signal().abandoned.get() {
                        Some(Error::Cancelled.into())
                    } else {
                        checked.take()
                    };
                }
                if entry.cancel_reason.is_some() && entry.completion.needs_cancel() {
                    #[cfg(feature = "simulation")]
                    if let Some(driver) = &mut state.simulation {
                        driver.cancel(id.0);
                        state
                            .entries
                            .get_mut(&id)
                            .unwrap()
                            .completion
                            .cancel_submitted();
                        continue;
                    }
                    let sqe = opcode::AsyncCancel::new(id.0)
                        .build()
                        .user_data(id.0 | CANCEL_BIT);
                    // SAFETY: cancel uses only an ID, and its target entry is retained.
                    if unsafe { state.ring.as_mut().unwrap().submission().push(&sqe) }.is_ok() {
                        state
                            .entries
                            .get_mut(&id)
                            .unwrap()
                            .completion
                            .cancel_submitted();
                    }
                }
            }
        }
        // Release snapshot owners outside reactor borrows, then keep the storage.
        batch.clear();
        let mut state = self.state.borrow_mut();
        if state.scan_batch.capacity() == 0 {
            state.scan_batch = batch;
        }
    }

    /// Sleep until a CQE/external wake, bounded by both duration and a 10ms timer
    /// fallback for producers that cannot yet attach the eventfd wake endpoint.
    pub fn wait(&self, duration: Duration) -> Result<()> {
        // A scope cancelled during the service turn wakes nothing, and its
        // fenced operation completes only after ASYNC_CANCEL. Queue those
        // cancels now so their CQEs end the sleep instead of the timeout. A
        // pending local wake skips the sleep, so the next poll_budgeted scans.
        let local_pending = || {
            self.state
                .borrow()
                .wake
                .as_ref()
                .is_some_and(|wake| wake.local.load(Ordering::Acquire))
        };
        if !duration.is_zero() && !local_pending() {
            // Scope checks read the ambient clock, so scan in the reactor's
            // captured environment exactly as poll_budgeted does.
            let _environment = self.environment.enter();
            let pending = self.state.borrow().entries.len();
            self.queue_cancellations(pending);
        }
        let mut state = self.state.borrow_mut();
        // Service polling can queue SQEs after poll_budgeted. Submit once before
        // sleeping so that work can produce the CQEs we wait for. Transient errors
        // defer progress to the next worker turn; completion/cancel budgets stay
        // in poll_budgeted, and an absent ring remains uninitialized.
        if let Err(error) = state.submit_pending() {
            state.stopped = true;
            return Err(error);
        }
        let local = state
            .wake
            .as_ref()
            .is_some_and(|wake| wake.local.swap(false, Ordering::AcqRel));
        if duration.is_zero() || local {
            return Ok(());
        }
        #[cfg(feature = "simulation")]
        if state.simulation.is_some() {
            return Ok(());
        }
        // Completions already posted, for example by inline issue during the
        // submit above, end the wait without a ppoll; the next poll_budgeted
        // reaps them. A pending remote token stays for the next sleeping wait.
        if state
            .ring
            .as_mut()
            .is_some_and(|ring| !ring.completion().is_empty())
        {
            return Ok(());
        }
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
        #[cfg(test)]
        {
            state.wait_attempts += 1;
        }
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
            // Clear after draining, never before: a racing coalesced producer is
            // covered by the service turn we are about to run. A later producer
            // writes a fresh token. Zero/local waits leave remote tokens intact.
            state
                .wake
                .as_ref()
                .unwrap()
                .remote
                .store(false, Ordering::Release);
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
    fn cancel_and_fence(&self, id: IoId) -> Operation<'_, (), S::Error> {
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
    pub fn drain(&self) -> Operation<'_, (), S::Error> {
        Box::pin(Fence {
            reactor: self,
            target: None,
            registration: None,
        })
    }

    /// Close admission and drive cancellation for at most `timeout` (host time).
    /// Callbacks must terminate promptly. An error, including DeadlineExceeded,
    /// is NEVER a fence: retained owners remain quarantined and this may be retried.
    /// Drop makes one bounded attempt, then leaks the ring and unfenced owners.
    pub fn shutdown(&self, timeout: Duration) -> Result<()> {
        self.state.borrow_mut().stopped = true;
        let started = std::time::Instant::now();
        while self.in_flight() != 0 {
            if started.elapsed() >= timeout {
                return Err(Error::DeadlineExceeded);
            }
            self.poll_budgeted(256)?;
            if self.in_flight() != 0 {
                self.wait(timeout.saturating_sub(started.elapsed()).min(MAX_WAIT))?;
            }
        }
        Ok(())
    }

    /// Cancel all current matching operations before awaiting any fence. New work
    /// submitted by the predicate or after the snapshot is not part of this fence.
    /// The worker must drive completions; only Ok confirms ownership is released.
    pub fn fence_matching<'a>(
        &'a self,
        matches: impl Fn(&S) -> bool + 'a,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let scopes: Vec<_> = self
                .state
                .borrow()
                .entries
                .iter()
                .map(|(id, entry)| (*id, entry.pending.clone()))
                .collect();
            let ids: Vec<_> = scopes
                .into_iter()
                .filter(|(_, pending)| matches(pending.scope()))
                .map(|(id, _)| id)
                .collect();
            let cancelled = Error::Cancelled.into();
            {
                let mut state = self.state.borrow_mut();
                for id in &ids {
                    if let Some(entry) = state.entries.get_mut(id) {
                        entry.cancel_reason.get_or_insert(cancelled);
                    }
                }
            }
            for id in ids {
                self.cancel_and_fence(id).await?;
            }
            Ok(())
        })
    }
}

/// Data operation and, for positioned file I/O, its explicit byte offset.
#[derive(Clone, Copy)]
enum BufferOperation {
    Read(u64),
    Write(u64),
    Recv,
    Send,
}

#[derive(Clone, Copy)]
enum IssuePolicy {
    Inline,
    Offload,
}

impl IssuePolicy {
    fn flags(self) -> squeue::Flags {
        match self {
            Self::Inline => squeue::Flags::empty(),
            Self::Offload => squeue::Flags::ASYNC,
        }
    }
}

impl<S: Scope> State<S> {
    /// Retain every kernel-visible owner when no completion fence was established.
    /// Closing the ring descriptor alone does not make submitted pointers safe.
    fn quarantine(&mut self) {
        std::mem::forget(std::mem::take(&mut self.entries));
        std::mem::forget(self.ring.take());
        std::mem::forget(self.ring_reservation.take());
    }

    /// Consume one backend CQE without releasing its entry's resources.
    fn next_completion(&mut self) -> Option<(u64, KernelResult)> {
        #[cfg(test)]
        if let Some(cqe) = self.completions.pop_front() {
            return Some(cqe);
        }
        #[cfg(feature = "simulation")]
        if let Some(driver) = &mut self.simulation {
            return driver.pop();
        }
        let cqe = self.ring.as_mut()?.completion().next()?;
        Some((cqe.user_data(), KernelResult::Value(cqe.result())))
    }

    /// Publish queued SQEs when queue state or kernel flags require an enter call.
    fn submit_pending(&mut self) -> Result<()> {
        #[cfg(test)]
        if self.ring.is_none()
            && let Some(result) = self.submit_result.take()
        {
            self.submit_attempts += 1;
            return submission_result(result);
        }
        #[cfg(feature = "simulation")]
        if let Some(driver) = &self.simulation {
            // This advances simulated kernel execution, not just SQ consumption.
            // Guard tests below use real rings rather than this simulation path.
            driver.submit();
            return Ok(());
        }
        let Some(ring) = &mut self.ring else {
            return Ok(());
        };
        let flags = ring_setup_flags(ring.params());
        let required = {
            // A fresh safe view observes the shared head, including consumption
            // after partial submissions or transient errors. No dirty bit can
            // substitute for the actual remaining SQ occupancy.
            let sq = ring.submission();
            submission_required(flags, sq.is_empty(), sq.cq_overflow(), sq.taskrun())
        }; // Drop the SQ view before submitting.
        if !required {
            return Ok(());
        }
        #[cfg(test)]
        {
            self.submit_attempts += 1;
            if let Some(result) = self.submit_result.take() {
                return submission_result(result);
            }
        }
        submission_result(ring.submit())
    }

    /// Remove matching registrations for notification outside the state borrow.
    fn take_fence_wakers(&mut self, target: Option<IoId>) -> Vec<FenceWaiter> {
        if self.fence_waiters.is_empty() {
            return Vec::new();
        }
        let keys: Vec<_> = self
            .fence_waiters
            .range((target, 0)..=(target, u64::MAX))
            .map(|(key, _)| *key)
            .collect();
        keys.into_iter()
            .map(|key| self.fence_waiters.remove(&key).unwrap())
            .collect()
    }

    /// Account one CQE and return its owner only after all applicable fences.
    fn complete(
        &mut self,
        tag: u64,
        result: impl Into<KernelResult>,
    ) -> Result<Option<FencedEntry<S>>> {
        let id = IoId(tag & !CANCEL_BIT);
        // One lookup serves both the fence update and the owner's removal.
        let index = self.entries.index_of(id).ok_or(Error::Io)?;
        let Some(original) = self
            .entries
            .at_mut(index)
            .completion
            .complete(tag & CANCEL_BIT != 0, result.into())?
        else {
            return Ok(None);
        };
        let entry = self.entries.remove_at(index);
        Ok(Some(FencedEntry { entry, original }))
    }
}

/// In-flight entries ordered by their monotonic identifiers.
///
/// Identifiers are allocated in increasing order, so inserts append and the
/// slots stay sorted for binary search. Removal leaves a tombstone that is
/// trimmed from either end or compacted once tombstones outnumber live
/// entries, which bounds the storage by the live count even when one old
/// entry outlives many newer ones.
struct Table<V> {
    slots: std::collections::VecDeque<(IoId, Option<V>)>,

    live: usize,
}

impl<V> Default for Table<V> {
    /// Create an empty table.
    fn default() -> Self {
        Self {
            slots: std::collections::VecDeque::new(),
            live: 0,
        }
    }
}

impl<V> Table<V> {
    /// Count live entries.
    fn len(&self) -> usize {
        self.live
    }

    /// Whether no entry is live.
    fn is_empty(&self) -> bool {
        self.live == 0
    }

    /// Find the slot holding a live entry for `id`.
    fn index_of(&self, id: IoId) -> Option<usize> {
        let index = self.slots.binary_search_by_key(&id, |(key, _)| *key).ok()?;
        self.slots[index].1.is_some().then_some(index)
    }

    /// Borrow the live entry at a slot returned by [`Self::index_of`].
    fn at_mut(&mut self, index: usize) -> &mut V {
        self.slots[index].1.as_mut().expect("live table slot")
    }

    /// Borrow a live entry.
    fn get(&self, id: &IoId) -> Option<&V> {
        self.index_of(*id)
            .and_then(|index| self.slots[index].1.as_ref())
    }

    /// Mutably borrow a live entry.
    fn get_mut(&mut self, id: &IoId) -> Option<&mut V> {
        let index = self.index_of(*id)?;
        self.slots[index].1.as_mut()
    }

    /// Whether `id` is live.
    fn contains_key(&self, id: &IoId) -> bool {
        self.index_of(*id).is_some()
    }

    /// Insert or replace an entry, returning any replaced value.
    fn insert(&mut self, id: IoId, value: V) -> Option<V> {
        if self.slots.back().is_none_or(|(last, _)| *last < id) {
            self.slots.push_back((id, Some(value)));
            self.live += 1;
            return None;
        }
        match self.slots.binary_search_by_key(&id, |(key, _)| *key) {
            Ok(index) => {
                let old = self.slots[index].1.replace(value);
                if old.is_none() {
                    self.live += 1;
                }
                old
            }
            Err(index) => {
                self.slots.insert(index, (id, Some(value)));
                self.live += 1;
                None
            }
        }
    }

    /// Remove a live entry.
    fn remove(&mut self, id: &IoId) -> Option<V> {
        let index = self.index_of(*id)?;
        Some(self.remove_at(index))
    }

    /// Remove the live entry at a slot returned by [`Self::index_of`].
    fn remove_at(&mut self, index: usize) -> V {
        let value = self.slots[index].1.take().expect("live table slot");
        self.live -= 1;
        while self.slots.front().is_some_and(|(_, value)| value.is_none()) {
            self.slots.pop_front();
        }
        while self.slots.back().is_some_and(|(_, value)| value.is_none()) {
            self.slots.pop_back();
        }
        if self.slots.len() > 2 * self.live + 16 {
            self.slots.retain(|(_, value)| value.is_some());
        }
        value
    }

    /// Iterate live entries in identifier order.
    fn iter(&self) -> impl Iterator<Item = (&IoId, &V)> {
        self.slots
            .iter()
            .filter_map(|(id, value)| value.as_ref().map(|value| (id, value)))
    }

    /// Iterate live identifiers in order.
    #[cfg(all(test, feature = "simulation"))]
    fn keys(&self) -> impl Iterator<Item = &IoId> {
        self.iter().map(|(id, _)| id)
    }

    /// Iterate live entries in identifier order.
    #[cfg(test)]
    fn values(&self) -> impl Iterator<Item = &V> {
        self.iter().map(|(_, value)| value)
    }

    /// The live entry with the smallest identifier.
    #[cfg(test)]
    fn first_key_value(&self) -> Option<(&IoId, &V)> {
        self.iter().next()
    }

    /// Iterate live entries after `id`, then wrap around to those at or before it.
    fn after(&self, id: IoId) -> impl Iterator<Item = (&IoId, &V)> {
        let split = self.slots.partition_point(|(key, _)| *key <= id);
        self.slots
            .range(split..)
            .chain(self.slots.range(..split))
            .filter_map(|(id, value)| value.as_ref().map(|value| (id, value)))
    }
}

impl<V> std::ops::Index<&IoId> for Table<V> {
    type Output = V;

    /// Borrow a live entry, panicking if it is absent.
    fn index(&self, id: &IoId) -> &V {
        self.get(id).expect("live table entry")
    }
}

impl From<i32> for KernelResult {
    /// Preserve a raw scalar CQE until its entry determines ownership semantics.
    fn from(value: i32) -> Self {
        Self::Value(value)
    }
}

/// Read setup flags from the documented transparent Linux parameter layout.
fn ring_setup_flags(params: &io_uring::Parameters) -> u32 {
    // io-uring 0.7 documents Parameters as repr(transparent) over Linux
    // io_uring_params. Its third u32 is flags, after sq_entries and cq_entries.
    // There are no public getters for COOP_TASKRUN, TASKRUN_FLAG or DEFER_TASKRUN.
    // SAFETY: this reads an initialized, aligned u32 within that documented ABI;
    // it neither accesses the mapped queues nor aliases a mutable reference.
    unsafe {
        (params as *const io_uring::Parameters)
            .cast::<u32>()
            .add(2)
            .read()
    }
}

/// Probe setup combinations, keeping the first ring the kernel accepts.
fn probe_ring<T>(mut build: impl FnMut(u32) -> std::io::Result<T>) -> std::io::Result<T> {
    for flags in [SETUP_NORMAL, SETUP_CQSIZE | SETUP_SUBMIT_ALL, SETUP_CQSIZE] {
        match build(flags) {
            Err(error) if error.raw_os_error() == Some(libc::EINVAL) && flags != SETUP_CQSIZE => (),
            result => return result,
        }
    }
    unreachable!("the baseline setup returns its result")
}

/// Skip empty submissions only for known interrupt-driven modes.
fn submission_required(setup_flags: u32, empty: bool, cq_overflow: bool, taskrun: bool) -> bool {
    // SINGLE_ISSUER and SUBMIT_ALL do not change completion delivery. Deferred
    // task work is not enabled: it needs a separate completion/fence audit.
    // All task-run modes, polling modes, and unknown flags remain fail-closed.
    setup_flags & !SETUP_NORMAL != 0 || !empty || cq_overflow || taskrun
}

/// Defer transient submit failures to the next worker turn; report fatal failures.
fn submission_result(result: std::io::Result<usize>) -> Result<()> {
    match result {
        Ok(_) => Ok(()),
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::EINTR | libc::EAGAIN | libc::EBUSY)
            ) =>
        {
            Ok(())
        }
        Err(_) => Err(Error::Io),
    }
}

impl<S: Scope, B: Budget> Drop for Reactor<S, B> {
    /// Attempt a bounded fence, quarantining all kernel owners if it fails.
    fn drop(&mut self) {
        if std::thread::panicking() || self.shutdown(DROP_WAIT).is_err() {
            // Closing an io_uring FD alone is NOT a synchronous memory fence.
            // On an unrecoverable driver failure retain the bounded owners and
            // ring forever rather than free memory the kernel may still use.
            self.state.get_mut().quarantine();
            return;
        }
        // Every SQE (including cancellation SQEs) has now produced its CQE.
        self.state.get_mut().ring.take();
    }
}

impl<S: Scope> Drop for State<S> {
    /// Quarantine unfinished owners even if a callback interrupted reactor shutdown.
    fn drop(&mut self) {
        // Also protect kernel ownership if an application lease destructor or a
        // custom waker panics while Reactor::drop is driving its final fence.
        if !self.entries.is_empty() {
            self.quarantine();
        }
    }
}

/// Stable syscall backing whose pointer provenance survives owner moves.
/// Do not replace the private one-element Vec with a move-retagged Box.
struct SyscallArg<T>(Vec<T>);

impl<T> SyscallArg<T> {
    /// Allocate initialized backing before publishing any derived pointer.
    fn new(value: T) -> Self {
        Self(vec![value])
    }

    /// Borrow the stable input pointer retained through the completion fence.
    fn as_ptr(&self) -> *const T {
        self.0.as_ptr()
    }

    /// Borrow the stable output pointer before transferring ownership to the kernel.
    fn as_mut_ptr(&mut self) -> *mut T {
        self.0.as_mut_ptr()
    }

    /// Recover output only after the caller has established its completion fence.
    fn into_inner(mut self) -> T {
        self.0.pop().expect("one syscall argument")
    }
}

/// Encode an owned Internet or filesystem Unix address into stable syscall storage.
fn encode_address(
    address: SocketAddress,
) -> Result<(SyscallArg<libc::sockaddr_storage>, libc::socklen_t)> {
    // SAFETY: all-zero sockaddr storage is valid and sufficiently aligned for
    // every supported sockaddr variant. The owner preserves pointer provenance
    // as well as the allocation address across completion-closure moves.
    let mut storage = SyscallArg::<libc::sockaddr_storage>::new(unsafe { std::mem::zeroed() });
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
                std::ptr::write(storage.as_mut_ptr().cast::<libc::sockaddr_in>(), value);
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
                std::ptr::write(storage.as_mut_ptr().cast::<libc::sockaddr_in6>(), value);
            }
            std::mem::size_of::<libc::sockaddr_in6>()
        }
        SocketAddress::Unix(path) => {
            let bytes = path.as_os_str().as_bytes();
            let mut value: libc::sockaddr_un = unsafe { std::mem::zeroed() };
            if bytes.is_empty() || bytes.contains(&0) || bytes.len() >= value.sun_path.len() {
                return Err(Error::InvalidInput);
            }
            value.sun_family = libc::AF_UNIX as _;
            for (dst, src) in value.sun_path.iter_mut().zip(bytes) {
                *dst = *src as libc::c_char;
            }
            unsafe {
                std::ptr::write(storage.as_mut_ptr().cast::<libc::sockaddr_un>(), value);
            }
            std::mem::offset_of!(libc::sockaddr_un, sun_path) + bytes.len() + 1
        }
    };
    Ok((storage, len as libc::socklen_t))
}

pub mod timer {
    //! Explicit timer backend selection, sharing the reactor's completion fences.
    use super::*;

    /// Choose kernel-owned waiting or cooperative observation of a simulated clock.
    #[derive(Clone, Copy, Debug)]
    pub enum SleepMode {
        /// Submit an owned io_uring timeout on a host clock and driver.
        Kernel,
        /// Poll the scoped clock cooperatively. The owner advances that clock.
        #[cfg(feature = "simulation")]
        ClockPoll,
    }

    impl<S: Scope, B: Budget> Reactor<S, B> {
        /// Sleep under caller-owned scope policy with an owned io_uring timeout.
        /// `until` must belong to the currently entered clock domain. Instant itself
        /// carries no domain ID, so an instant copied from another clock is unsupported.
        /// Kernel mode rejects simulated clocks/drivers, including expired deadlines.
        /// ClockPoll requires a simulated clock and cooperatively wakes its executor.
        pub fn sleep_until<'a>(
            &'a self,
            until: std::time::Instant,
            mode: SleepMode,
            scope: &'a S,
        ) -> Operation<'a, (), S::Error> {
            Box::pin(async move {
                scope.check()?;
                #[cfg(feature = "simulation")]
                match mode {
                    SleepMode::Kernel
                        if crate::environment::simulation_seed().is_some()
                            || self.state.borrow().simulation.is_some() =>
                    {
                        return Err(Error::InvalidConfiguration.into());
                    }
                    SleepMode::ClockPoll if crate::environment::simulation_seed().is_none() => {
                        return Err(Error::InvalidConfiguration.into());
                    }
                    _ => (),
                }
                let duration = until.saturating_duration_since(crate::environment::now());
                if duration.is_zero() {
                    return Ok(());
                }
                match mode {
                    #[cfg(feature = "simulation")]
                    SleepMode::ClockPoll => {
                        return std::future::poll_fn(|cx| {
                            scope.check()?;
                            if crate::environment::now() >= until {
                                Poll::Ready(Ok(()))
                            } else {
                                cx.waker().wake_by_ref();
                                Poll::Pending
                            }
                        })
                        .await;
                    }
                    SleepMode::Kernel => (),
                }
                if duration.as_secs() > i64::MAX as u64 {
                    return Err(Error::InvalidInput.into());
                }
                let timeout = SyscallArg::new(types::Timespec::from(duration));
                let sqe = opcode::Timeout::new(timeout.as_ptr()).build();
                self.submit_reserved_kind(
                    Submission::Real(sqe),
                    scope,
                    false,
                    None,
                    SubmissionKind::Timeout,
                    move |result| {
                        drop(timeout);
                        match result? {
                            KernelResult::Value(value) if value == -libc::ETIME => Ok(()),
                            other => {
                                other.value()?;
                                Ok(())
                            }
                        }
                    },
                )?
                .await?;
                scope.check()
            })
        }
    }

    #[cfg(test)]
    mod tests {
        //! Kernel and cooperative timer selection, cancellation, and deadline contracts.
        use super::*;
        use crate::reactor::tests::{drive, kernel_reactor, poll, scope};

        #[test]
        /// Check timer completion and cancellation through the actual kernel fence.
        fn kernel_timer_completes_and_cancel_retains_fence() {
            let Some(reactor) = kernel_reactor(4) else {
                return;
            };
            let scope = scope();
            drive(
                &reactor,
                reactor.sleep_until(crate::environment::now(), SleepMode::Kernel, &scope),
            )
            .unwrap();
            let until = crate::environment::now() + Duration::from_millis(2);
            drive(
                &reactor,
                reactor.sleep_until(until, SleepMode::Kernel, &scope),
            )
            .unwrap();
            assert!(crate::environment::now() >= until);
            let mut future = reactor.sleep_until(
                crate::environment::now() + Duration::from_secs(10),
                SleepMode::Kernel,
                &scope,
            );
            assert!(poll(&mut future).is_pending());
            assert_eq!(reactor.in_flight(), 1);
            scope.cancel().unwrap();
            assert_eq!(drive(&reactor, future), Err(Error::Cancelled));
            assert_eq!(reactor.in_flight(), 0);
        }

        #[cfg(feature = "simulation")]
        #[test]
        /// Verify simulated clock polling wakes cooperatively without submissions.
        fn clock_poll_never_submits_and_preserves_scope_precedence() {
            use crate::reactor::tests::fixtures::{Admission, Limits, Reactor};
            use crate::test_util::WakeCounter;
            let clock = crate::environment::SimulationClock::new(33);
            let _clock = clock.environment(0).enter();
            let reactor = Reactor::new(Rc::new(Admission::new(Limits {
                queue_entries: std::num::NonZeroUsize::new(4).unwrap(),
            })));
            let scope = scope();
            let until = crate::environment::now() + Duration::from_millis(5);
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut future = reactor.sleep_until(until, SleepMode::ClockPoll, &scope);
            assert!(future.as_mut().poll(&mut cx).is_pending());
            assert_eq!(count.count(), 1);
            assert_eq!(reactor.in_flight(), 0);
            clock.advance(Duration::from_millis(5));
            assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            scope.cancel().unwrap();
            assert_eq!(
                poll(&mut reactor.sleep_until(until, SleepMode::ClockPoll, &scope)),
                Poll::Ready(Err(Error::Cancelled))
            );
        }

        #[cfg(feature = "simulation")]
        #[test]
        /// Reject mixed clock domains before even accepting an expired timer.
        fn timer_rejects_mixed_clock_and_backend_even_for_expired_deadlines() {
            let request = scope();
            let reactor = Reactor::<crate::reactor::tests::fixtures::RequestScope, ()>::new(2, ());
            let expired = crate::environment::now();
            assert_eq!(
                poll(&mut reactor.sleep_until(expired, SleepMode::ClockPoll, &request)),
                Poll::Ready(Err(Error::InvalidConfiguration))
            );
            let clock = crate::environment::SimulationClock::new(4);
            let _clock = clock.environment(0).enter();
            assert_eq!(
                poll(&mut reactor.sleep_until(expired, SleepMode::Kernel, &request)),
                Poll::Ready(Err(Error::InvalidConfiguration))
            );
            assert_eq!(reactor.in_flight(), 0);
        }
    }
}

pub mod descriptor {
    //! Concrete OS resource owner. Simulated resources never have a raw descriptor.
    use crate::{Error, Result};
    use std::os::fd::{AsFd, AsRawFd, BorrowedFd, FromRawFd, OwnedFd, RawFd};

    /// Owned host or simulation resource. `AsRawFd` and `AsFd` panic for simulated
    /// handles: use `as_sim` or fallible `into_host` at explicit backend boundaries.
    #[derive(Debug)]
    pub struct Descriptor(Kind);

    /// Backend-specific ownership, never a fabricated raw descriptor.
    #[derive(Debug)]
    enum Kind {
        Real(OwnedFd),
        #[cfg(feature = "simulation")]
        Sim(super::simulation::Handle),
    }

    impl Descriptor {
        /// Borrow a simulated resource without exposing an invented host descriptor.
        #[cfg(feature = "simulation")]
        pub fn as_sim(&self) -> Option<&super::simulation::Handle> {
            match &self.0 {
                Kind::Sim(handle) => Some(handle),
                Kind::Real(_) => None,
            }
        }

        /// Extract a simulated resource, returning the unchanged host owner on mismatch.
        #[cfg(feature = "simulation")]
        pub fn into_sim(self) -> std::result::Result<super::simulation::Handle, Self> {
            match self.0 {
                Kind::Sim(handle) => Ok(handle),
                other => Err(Self(other)),
            }
        }

        /// Set TCP_NODELAY for TCP callers, including disabling it when false.
        pub fn enable_tcp_nodelay(&self, enabled: bool) -> Result<()> {
            #[cfg(feature = "simulation")]
            if self.as_sim().is_some() {
                return Ok(());
            }
            let value: libc::c_int = enabled.into();
            // SAFETY: the descriptor is owned and setsockopt reads one live integer.
            let result = unsafe {
                libc::setsockopt(
                    self.as_raw_fd(),
                    libc::IPPROTO_TCP,
                    libc::TCP_NODELAY,
                    (&value as *const libc::c_int).cast(),
                    std::mem::size_of_val(&value) as libc::socklen_t,
                )
            };
            if result < 0 {
                return Err(Error::Io);
            }
            Ok(())
        }

        /// Numeric socket identity only, sampled on failure while the owner is live.
        pub fn tcp_tuple(&self) -> Option<(std::net::SocketAddr, std::net::SocketAddr)> {
            #[cfg(feature = "simulation")]
            if self.as_sim().is_some() {
                return None;
            }
            // SAFETY: ManuallyDrop borrows this live descriptor without closing it.
            let socket = std::mem::ManuallyDrop::new(unsafe {
                std::net::TcpStream::from_raw_fd(self.as_raw_fd())
            });
            Some((socket.local_addr().ok()?, socket.peer_addr().ok()?))
        }

        /// The listener must already be nonblocking; accept4 flags apply only to the
        /// accepted socket. EINTR is retried a bounded number of times; other errno
        /// values, including EAGAIN, remain available to caller retry policy.
        pub fn try_accept(&self) -> std::io::Result<Self> {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.accept();
            }
            // SAFETY: live listener; successful descriptor is immediately owned.
            let fd = retry_interrupted(|| {
                count(unsafe {
                    libc::accept4(
                        self.as_raw_fd(),
                        std::ptr::null_mut(),
                        std::ptr::null_mut(),
                        libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                    ) as isize
                })
            })?;
            Ok(unsafe { Self::from_raw_fd(fd as RawFd) })
        }

        /// Explicit extraction for host-only adapters. A simulated handle is rejected.
        pub fn into_host(self) -> std::result::Result<OwnedFd, Self> {
            match self.0 {
                Kind::Real(fd) => Ok(fd),
                #[cfg(feature = "simulation")]
                other => Err(Self(other)),
            }
        }

        /// Bind a nonblocking TCP listener in the currently selected environment.
        pub fn tcp_listener(address: std::net::SocketAddr) -> Result<Self> {
            #[cfg(feature = "simulation")]
            if let Some(sim) = super::simulation::Simulation::current() {
                return sim
                    .listen(super::SocketAddress::Inet(address))
                    .map_err(|_| Error::Io);
            }
            let listener = std::net::TcpListener::bind(address).map_err(|_| Error::Io)?;
            listener.set_nonblocking(true).map_err(|_| Error::Io)?;
            Ok(listener.into())
        }

        /// Create a nonblocking, close-on-exec stream socket for the requested family.
        pub fn socket(domain: i32) -> Result<Self> {
            #[cfg(feature = "simulation")]
            if let Some(sim) = super::simulation::Simulation::current() {
                return sim.socket(domain).map_err(|_| Error::Io);
            }
            let raw = unsafe {
                libc::socket(
                    domain,
                    libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
                    0,
                )
            };
            if raw < 0 {
                return Err(Error::Io);
            }
            Ok(unsafe { Self::from_raw_fd(raw) })
        }

        /// Enable O_NONBLOCK and FD_CLOEXEC, preserving other status/descriptor flags.
        /// O_NONBLOCK affects duplicated descriptors sharing the open file description;
        /// FD_CLOEXEC is local to this descriptor. Failure may leave partial changes.
        pub fn set_nonblocking(&self) -> Result<()> {
            #[cfg(feature = "simulation")]
            if self.as_sim().is_some() {
                return Ok(());
            }
            let fd = self.as_raw_fd();
            unsafe {
                let flags = libc::fcntl(fd, libc::F_GETFL);
                let descriptor_flags = libc::fcntl(fd, libc::F_GETFD);
                if flags < 0
                    || descriptor_flags < 0
                    || libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0
                    || libc::fcntl(fd, libc::F_SETFD, descriptor_flags | libc::FD_CLOEXEC) < 0
                {
                    return Err(Error::Io);
                }
            }
            Ok(())
        }

        /// Send without waiting or raising SIGPIPE; preserve short progress and errno.
        /// Only zero-progress EINTR is retried, for at most four attempts.
        pub fn try_send(&self, bytes: &[u8]) -> std::io::Result<usize> {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.send(bytes);
            }
            retry_interrupted(|| {
                count(unsafe {
                    libc::send(
                        self.as_raw_fd(),
                        bytes.as_ptr().cast(),
                        bytes.len(),
                        libc::MSG_DONTWAIT | libc::MSG_NOSIGNAL,
                    )
                })
            })
        }

        /// Receive without waiting; preserve EOF, short progress, and errno.
        /// Only zero-progress EINTR is retried, for at most four attempts.
        pub fn try_recv(&self, bytes: &mut [u8]) -> std::io::Result<usize> {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.recv(bytes);
            }
            retry_interrupted(|| {
                count(unsafe {
                    libc::recv(
                        self.as_raw_fd(),
                        bytes.as_mut_ptr().cast(),
                        bytes.len(),
                        libc::MSG_DONTWAIT,
                    )
                })
            })
        }

        /// Check for an idle connected socket without consuming any queued byte.
        /// Unexpected data, EOF, and errors all make a pooled socket unsuitable.
        pub fn idle_healthy(&self) -> bool {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.idle_healthy();
            }
            let mut byte = 0u8;
            let result = unsafe {
                libc::recv(
                    self.as_raw_fd(),
                    (&mut byte as *mut u8).cast(),
                    1,
                    libc::MSG_PEEK | libc::MSG_DONTWAIT,
                )
            };
            result < 0 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EAGAIN)
        }

        /// Full peer close, not a write-half shutdown. Consumes no bytes and never
        /// competes with parsing, even during acquisition or response writes.
        pub fn peer_disconnected(&self) -> bool {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.peer_disconnected();
            }
            self.host_closed(0)
        }

        /// Unlike POLLHUP, POLLRDHUP notices a peer FIN while our send side is open.
        pub fn peer_read_closed(&self) -> bool {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.peer_read_closed();
            }
            self.host_closed(libc::POLLRDHUP)
        }

        /// Shut down one or both socket directions without releasing ownership.
        pub fn shutdown(&self, how: i32) -> Result<()> {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.shutdown(how).map_err(Error::from_io);
            }
            // SAFETY: shutdown only changes this live owned descriptor's state.
            if unsafe { libc::shutdown(self.as_raw_fd(), how) } < 0 {
                return Err(Error::from_io(std::io::Error::last_os_error()));
            }
            Ok(())
        }

        /// Require a stream socket, rejecting files and other socket kinds.
        pub fn validate_socket(&self) -> Result<()> {
            #[cfg(feature = "simulation")]
            if let Some(handle) = self.as_sim() {
                return handle.validate_socket().map_err(|_| Error::InvalidInput);
            }
            let mut kind: libc::c_int = 0;
            let mut length = std::mem::size_of_val(&kind) as libc::socklen_t;
            let result = unsafe {
                libc::getsockopt(
                    self.as_raw_fd(),
                    libc::SOL_SOCKET,
                    libc::SO_TYPE,
                    (&mut kind as *mut libc::c_int).cast(),
                    &mut length,
                )
            };
            if result < 0 || kind != libc::SOCK_STREAM {
                return Err(Error::InvalidInput);
            }
            Ok(())
        }

        /// Inspect host closure flags without waiting or taking descriptor ownership.
        fn host_closed(&self, interest: i16) -> bool {
            let mut fd = libc::pollfd {
                fd: self.as_raw_fd(),
                events: interest,
                revents: 0,
            };
            // SAFETY: poll only inspects one live descriptor and never waits.
            (unsafe { libc::poll(&mut fd, 1, 0) > 0 })
                && fd.revents & (interest | libc::POLLHUP | libc::POLLERR | libc::POLLNVAL) != 0
        }
    }

    impl AsRawFd for Descriptor {
        /// Borrow a host descriptor number, rejecting simulated resources.
        fn as_raw_fd(&self) -> RawFd {
            match &self.0 {
                Kind::Real(fd) => fd.as_raw_fd(),
                #[cfg(feature = "simulation")]
                Kind::Sim(_) => panic!("simulated descriptor reached a host syscall"),
            }
        }
    }

    impl AsFd for Descriptor {
        /// Borrow host ownership for a syscall, rejecting simulated resources.
        fn as_fd(&self) -> BorrowedFd<'_> {
            match &self.0 {
                Kind::Real(fd) => fd.as_fd(),
                #[cfg(feature = "simulation")]
                Kind::Sim(_) => panic!("simulated descriptor reached a host syscall"),
            }
        }
    }

    impl FromRawFd for Descriptor {
        /// Assume sole closing ownership of a live host descriptor.
        unsafe fn from_raw_fd(fd: RawFd) -> Self {
            Self(Kind::Real(unsafe { OwnedFd::from_raw_fd(fd) }))
        }
    }

    /// Implement closing-ownership transfer for supported host resource adapters.
    macro_rules! from_host {
        ($($ty:ty),*) => { $(impl From<$ty> for Descriptor {
            /// Transfer a host resource's closing ownership into the reactor adapter.
            fn from(value: $ty) -> Self { Self(Kind::Real(value.into())) }
        })* };
    }

    from_host!(
        OwnedFd,
        std::fs::File,
        std::net::TcpStream,
        std::net::TcpListener,
        std::net::UdpSocket,
        std::os::unix::net::UnixStream,
        std::os::unix::net::UnixListener,
        std::os::unix::net::UnixDatagram
    );

    #[cfg(feature = "simulation")]
    impl From<super::simulation::Handle> for Descriptor {
        /// Retain a simulated owner without assigning it a host descriptor number.
        fn from(handle: super::simulation::Handle) -> Self {
            Self(Kind::Sim(handle))
        }
    }

    /// Convert a syscall byte count without losing errno on failure.
    fn count(value: isize) -> std::io::Result<usize> {
        if value < 0 {
            Err(std::io::Error::last_os_error())
        } else {
            Ok(value as usize)
        }
    }

    /// Bound zero-progress EINTR retries, returning short success and other errors.
    fn retry_interrupted<T>(mut syscall: impl FnMut() -> std::io::Result<T>) -> std::io::Result<T> {
        for _ in 0..3 {
            match syscall() {
                Err(error) if error.raw_os_error() == Some(libc::EINTR) => (),
                result => return result,
            }
        }
        syscall()
    }

    #[cfg(all(test, feature = "simulation"))]
    #[test]
    /// Compare descriptor half-close behavior across host and simulated streams.
    fn descriptor_half_close_matches_simulated_and_host_sockets() {
        let sim = super::simulation::Simulation::new();
        let simulated = sim.socket_pair();
        let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
        for (left, right) in [simulated, (left.into(), right.into())] {
            left.try_send(b"x").unwrap();
            left.shutdown(libc::SHUT_WR).unwrap();
            assert!(right.peer_read_closed());
            assert!(!right.peer_disconnected());
            assert_eq!(right.try_recv(&mut [0; 1]).unwrap(), 1);
            assert_eq!(right.try_recv(&mut [0; 1]).unwrap(), 0);
            assert_eq!(right.try_send(b"r").unwrap(), 1);
            assert_eq!(left.try_recv(&mut [0; 1]).unwrap(), 1);
            assert_eq!(left.shutdown(123), Err(Error::Os(libc::EINVAL)));
            drop(left);
            assert!(tests::eventually_disconnected(&right));
        }
    }

    #[cfg(test)]
    mod tests {
        //! Host descriptor flags, bounded retries, and backend separation contracts.
        use super::*;

        #[test]
        /// Preserve short success and errno while bounding interrupted syscall retries.
        fn interrupted_syscalls_retry_boundedly_without_hiding_errno_or_short_success() {
            let mut calls = 0;
            let result: std::io::Result<()> = retry_interrupted(|| {
                calls += 1;
                Err(std::io::Error::from_raw_os_error(libc::EINTR))
            });
            assert_eq!(calls, 4);
            assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EINTR));
            let mut calls = 0;
            assert_eq!(
                retry_interrupted(|| {
                    calls += 1;
                    if calls == 1 {
                        Err(std::io::Error::from_raw_os_error(libc::EINTR))
                    } else {
                        Ok(1)
                    }
                })
                .unwrap(),
                1
            );
            assert_eq!(calls, 2);
            let result: std::io::Result<()> =
                retry_interrupted(|| Err(std::io::Error::from_raw_os_error(libc::EAGAIN)));
            assert_eq!(result.unwrap_err().raw_os_error(), Some(libc::EAGAIN));
        }

        #[test]
        /// Check reversible TCP_NODELAY and nonblocking close-on-exec configuration.
        fn nodelay_can_be_disabled_and_nonblocking_sets_cloexec() {
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let client = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let fd = Descriptor::from(client.try_clone().unwrap());
            fd.enable_tcp_nodelay(true).unwrap();
            assert!(client.nodelay().unwrap());
            fd.enable_tcp_nodelay(false).unwrap();
            assert!(!client.nodelay().unwrap());
            fd.set_nonblocking().unwrap();
            assert_ne!(
                unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) } & libc::O_NONBLOCK,
                0
            );
            assert_ne!(
                unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) } & libc::FD_CLOEXEC,
                0
            );
        }

        #[test]
        /// Keep the response direction usable after a peer's write-half close.
        fn peer_write_half_close_does_not_imply_full_disconnect() {
            let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
            let fd = Descriptor::from(left);
            right.shutdown(std::net::Shutdown::Write).unwrap();
            assert!(fd.peer_read_closed());
            assert!(!fd.peer_disconnected());
            assert_eq!(fd.try_send(b"response").unwrap(), 8);
            drop(right);
            assert!(eventually_disconnected(&fd));
        }

        /// Wait up to 5s for a full peer close.
        ///
        /// Other tests spawn processes. A new child holds copies of the parent's
        /// descriptors until exec, which closes those marked close-on-exec. So
        /// dropping a test socket may not close it at once.
        pub(super) fn eventually_disconnected(fd: &Descriptor) -> bool {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
            loop {
                if fd.peer_disconnected() {
                    return true;
                }
                if std::time::Instant::now() >= deadline {
                    return false;
                }
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
        }

        #[test]
        /// Report disconnect only after the last copy of the peer closes.
        fn peer_disconnect_waits_for_every_peer_descriptor_copy() {
            let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
            let fd = Descriptor::from(left);
            let copy = right.try_clone().unwrap();
            drop(right);
            assert!(!fd.peer_disconnected());
            assert!(!fd.peer_read_closed());
            drop(copy);
            assert!(eventually_disconnected(&fd));
        }

        #[test]
        /// Wait through a peer copy that another thread closes later.
        fn eventually_disconnected_waits_for_late_peer_copy_close() {
            let (left, right) = std::os::unix::net::UnixStream::pair().unwrap();
            let fd = Descriptor::from(left);
            let copy = right.try_clone().unwrap();
            drop(right);
            assert!(!fd.peer_disconnected());
            let closer = std::thread::spawn(move || {
                std::thread::sleep(std::time::Duration::from_millis(20));
                drop(copy);
            });
            assert!(eventually_disconnected(&fd));
            closer.join().unwrap();
        }

        #[cfg(feature = "simulation")]
        #[test]
        /// Prevent simulated owners from crossing host syscall adapter boundaries.
        fn raw_simulated_descriptor_access_panics_and_host_extraction_is_fallible() {
            let sim = super::super::simulation::Simulation::new();
            let (fd, _peer) = sim.socket_pair();
            assert!(
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| fd.as_raw_fd())).is_err()
            );
            assert!(std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| fd.as_fd())).is_err());
            assert!(fd.into_host().is_err());
        }
    }
}

pub mod ready_set {
    //! Level-triggered host descriptor index with one completion-owned idle wait.
    //! Callers own registration lifetimes, generation changes, scope cancellation,
    //! and retry policy. This index never retains registered descriptor owners.
    use super::Descriptor;
    use crate::{Error, Operation, Result};
    use std::{
        collections::{BTreeMap, VecDeque},
        os::fd::{AsRawFd, FromRawFd, RawFd},
        rc::Rc,
        task::{Context, Poll},
    };

    /// A host-only, level-triggered readiness index with one owned idle wait.
    /// Registered descriptors remain caller-owned and must be removed before reuse.
    pub struct ReadySet<E = Error> {
        fd: Rc<Descriptor>,

        ready: VecDeque<usize>,

        wait: Option<Operation<'static, u32, E>>,

        registrations: BTreeMap<RawFd, usize>,
    }

    impl<E: From<Error>> ReadySet<E> {
        /// Create an empty, close-on-exec epoll index without registering handles.
        pub fn new() -> Result<Self, E> {
            // SAFETY: epoll_create1 returns a uniquely owned descriptor.
            let fd = unsafe { libc::epoll_create1(libc::EPOLL_CLOEXEC) };
            if fd < 0 {
                return Err(Error::Io.into());
            }
            Ok(Self {
                fd: Rc::new(unsafe { Descriptor::from_raw_fd(fd) }),
                ready: VecDeque::new(),
                wait: None,
                registrations: BTreeMap::new(),
            })
        }

        /// Register a host descriptor with a unique caller-chosen index. Remove it
        /// before closing or reusing its FD/index. Closing does not remove an epoll
        /// registration while duplicated aliases retain the open file description.
        /// This host-only index retains no registered descriptor owners.
        pub fn insert(&mut self, fd: RawFd, index: usize) -> Result<(), E> {
            if self
                .registrations
                .values()
                .any(|existing| *existing == index)
            {
                return Err(Error::AlreadyExists.into());
            }
            let mut event = libc::epoll_event {
                events: libc::EPOLLIN as u32,
                u64: index as u64,
            };
            // SAFETY: initialized event lives through ctl. Invalid FDs fail ctl.
            if unsafe { libc::epoll_ctl(self.fd.as_raw_fd(), libc::EPOLL_CTL_ADD, fd, &mut event) }
                < 0
            {
                return Err(Error::Io.into());
            }
            self.registrations.insert(fd, index);
            Ok(())
        }

        /// Deregister while `fd` still names the registered open file description,
        /// discarding any cached event. Unknown descriptors return NotFound.
        pub fn remove(&mut self, fd: RawFd) -> Result<(), E> {
            let index = *self.registrations.get(&fd).ok_or(Error::NotFound)?;
            // SAFETY: DEL ignores its event argument; fd must still be live.
            if unsafe {
                libc::epoll_ctl(
                    self.fd.as_raw_fd(),
                    libc::EPOLL_CTL_DEL,
                    fd,
                    std::ptr::null_mut(),
                )
            } < 0
            {
                return Err(Error::Io.into());
            }
            self.registrations.remove(&fd);
            self.ready.retain(|queued| *queued != index);
            Ok(())
        }

        /// Abandon an idle wait and discard cached indices, without deregistration.
        /// Cancel the caller-owned scope first; accepted I/O retains its CQE leases.
        pub fn clear(&mut self) {
            self.wait.take();
            self.ready.clear();
        }

        /// Drain at most 64 events per refill, using one idle wait when none are ready.
        /// `wait` must retain the epoll owner through its completion fence. Completed
        /// idle waits are hints to refill. Zero budget neither polls nor creates work.
        pub fn next(
            &mut self,
            cx: &mut Context<'_>,
            budget: usize,
            wait: impl FnOnce(Rc<Descriptor>) -> Operation<'static, u32, E>,
        ) -> Result<Option<usize>, E> {
            if budget == 0 {
                return Ok(None);
            }
            if let Some(index) = self.ready.pop_front() {
                return Ok(Some(index));
            }
            if self.poll_wait(cx)?.is_pending() {
                return Ok(None);
            }
            let mut events = [libc::epoll_event { events: 0, u64: 0 }; 64];
            // SAFETY: events is writable for the bounded requested event count.
            let count = unsafe {
                libc::epoll_wait(
                    self.fd.as_raw_fd(),
                    events.as_mut_ptr(),
                    budget.clamp(1, 64) as i32,
                    0,
                )
            };
            if count < 0 {
                if std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    cx.waker().wake_by_ref();
                    return Ok(None);
                }
                return Err(Error::Io.into());
            }
            self.ready.extend(
                events[..count as usize]
                    .iter()
                    .map(|event| event.u64 as usize),
            );
            if let Some(index) = self.ready.pop_front() {
                return Ok(Some(index));
            }
            self.wait = Some(wait(self.fd.clone()));
            if self.poll_wait(cx)?.is_ready() {
                cx.waker().wake_by_ref();
            }
            Ok(None)
        }

        /// Report whether an index is cached or a registered descriptor is ready,
        /// without waiting. Ready indices are cached for the next `next` call. An
        /// outstanding idle wait owns the next wake, so this reports false.
        pub fn has_ready(&mut self) -> Result<bool, E> {
            if !self.ready.is_empty() {
                return Ok(true);
            }
            if self.wait.is_some() {
                return Ok(false);
            }
            let mut events = [libc::epoll_event { events: 0, u64: 0 }; 64];
            // SAFETY: events is writable for the bounded requested event count.
            let count = unsafe {
                libc::epoll_wait(
                    self.fd.as_raw_fd(),
                    events.as_mut_ptr(),
                    events.len() as i32,
                    0,
                )
            };
            if count < 0 {
                if std::io::Error::last_os_error().raw_os_error() == Some(libc::EINTR) {
                    return Ok(true);
                }
                return Err(Error::Io.into());
            }
            self.ready.extend(
                events[..count as usize]
                    .iter()
                    .map(|event| event.u64 as usize),
            );
            Ok(!self.ready.is_empty())
        }

        /// Consume completed waits before propagating failure, preventing repolling.
        /// With no wait the caller can inspect the level-triggered index immediately.
        fn poll_wait(&mut self, cx: &mut Context<'_>) -> Result<Poll<()>, E> {
            let Some(wait) = &mut self.wait else {
                return Ok(Poll::Ready(()));
            };
            match wait.as_mut().poll(cx) {
                Poll::Pending => Ok(Poll::Pending),
                Poll::Ready(result) => {
                    self.wait.take();
                    result.map(|_| Poll::Ready(()))
                }
            }
        }
    }

    #[cfg(test)]
    mod tests {
        //! Level-triggered registration, cached-event, and idle-wait ownership contracts.
        use super::*;
        use std::{
            cell::Cell,
            io::{Read, Write},
            os::unix::net::UnixStream,
        };

        #[test]
        /// Consume an immediate idle completion before requesting another refill.
        fn immediately_completed_idle_wait_is_consumed_and_requests_a_refill() {
            let mut set = ReadySet::<Error>::new().unwrap();
            let counter = std::sync::Arc::new(crate::test_util::WakeCounter::default());
            let waker = std::task::Waker::from(counter.clone());
            let mut cx = Context::from_waker(&waker);
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(async { Ok(0) })),
                Ok(None)
            );
            assert!(set.wait.is_none());
            assert_eq!(counter.count(), 1);
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
                Ok(None)
            );
            assert!(set.wait.is_some());
            assert_eq!(counter.count(), 1);
        }

        #[test]
        /// Verify repeated level readiness without retaining registered socket owners.
        fn indices_are_level_triggered_and_registration_does_not_retain_owners() {
            let mut set = ReadySet::<Error>::new().unwrap();
            let (mut reader, mut writer) = UnixStream::pair().unwrap();
            reader.set_nonblocking(true).unwrap();
            set.insert(reader.as_raw_fd(), 17).unwrap();
            assert_eq!(set.insert(reader.as_raw_fd(), 18), Err(Error::Io));
            assert_eq!(set.insert(-1, 19), Err(Error::Io));
            writer.write_all(b"x").unwrap();
            let mut cx = Context::from_waker(std::task::Waker::noop());
            assert_eq!(set.next(&mut cx, 0, |_| panic!("zero budget")), Ok(None));
            for budget in [1, usize::MAX] {
                assert_eq!(set.next(&mut cx, budget, |_| panic!("ready")), Ok(Some(17)));
            }
            reader.read_exact(&mut [0]).unwrap();
            let calls = Cell::new(0);
            for _ in 0..3 {
                assert_eq!(
                    set.next(&mut cx, 1, |_| {
                        calls.set(calls.get() + 1);
                        Box::pin(std::future::pending())
                    }),
                    Ok(None)
                );
            }
            assert_eq!(calls.get(), 1);
            set.clear();
            drop(reader);
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(async { Err(Error::Cancelled) })),
                Err(Error::Cancelled)
            );
        }

        #[test]
        /// Drain cached readiness before refilling and discard it on clear.
        fn ready_batch_is_drained_before_refilling_and_clear_discards_it() {
            let mut set = ReadySet::<Error>::new().unwrap();
            let mut sockets = Vec::new();
            for index in 0..3 {
                let (reader, mut writer) = UnixStream::pair().unwrap();
                set.insert(reader.as_raw_fd(), index).unwrap();
                writer.write_all(b"x").unwrap();
                sockets.push((reader, writer));
            }
            let mut cx = Context::from_waker(std::task::Waker::noop());
            let mut indices = Vec::new();
            for _ in 0..3 {
                indices.push(set.next(&mut cx, 3, |_| panic!("ready")).unwrap().unwrap());
            }
            indices.sort();
            assert_eq!(indices, [0, 1, 2]);
            set.next(&mut cx, 3, |_| panic!("ready")).unwrap();
            assert_eq!(set.ready.len(), 2);
            set.clear();
            assert!(set.ready.is_empty());
        }

        #[test]
        /// Probe level readiness without waiting, caching indices for `next`.
        fn has_ready_caches_level_readiness_and_defers_to_an_idle_wait() {
            let mut set = ReadySet::<Error>::new().unwrap();
            let (mut reader, mut writer) = UnixStream::pair().unwrap();
            reader.set_nonblocking(true).unwrap();
            set.insert(reader.as_raw_fd(), 4).unwrap();
            assert_eq!(set.has_ready(), Ok(false));
            writer.write_all(b"x").unwrap();
            assert_eq!(set.has_ready(), Ok(true));
            assert_eq!(set.ready.len(), 1);
            assert_eq!(set.has_ready(), Ok(true), "cached index is not refilled");
            assert_eq!(set.ready.len(), 1);
            let mut cx = Context::from_waker(std::task::Waker::noop());
            assert_eq!(set.next(&mut cx, 1, |_| panic!("cached")), Ok(Some(4)));
            reader.read_exact(&mut [0]).unwrap();
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
                Ok(None)
            );
            writer.write_all(b"x").unwrap();
            assert_eq!(set.has_ready(), Ok(false), "the idle wait owns the wake");
            assert!(set.ready.is_empty());
        }

        #[test]
        /// Keep the epoll owner alive until an abandoned idle operation is fenced.
        fn abandoned_idle_wait_retains_epoll_until_completion_fence() {
            use crate::reactor::tests::{drive, kernel_reactor, scope};
            let Some(reactor) = kernel_reactor(8) else {
                return;
            };
            let reactor = Rc::new(reactor);
            let request = scope();
            let mut set = ReadySet::<Error>::new().unwrap();
            let weak = Rc::downgrade(&set.fd);
            let mut cx = Context::from_waker(std::task::Waker::noop());
            assert_eq!(
                set.next(&mut cx, 1, |fd| {
                    let reactor = reactor.clone();
                    let request = request.clone();
                    Box::pin(async move {
                        reactor
                            .readiness_with_lease(fd.clone(), libc::POLLIN as u32, fd, &request)
                            .await
                    })
                }),
                Ok(None)
            );
            assert_eq!(reactor.in_flight(), 1);
            request.cancel().unwrap();
            drop(set);
            assert!(weak.upgrade().is_some());
            drive(&reactor, reactor.file_fence(())).unwrap();
            assert_eq!(reactor.in_flight(), 0);
            assert!(weak.upgrade().is_none());
        }

        #[test]
        /// Remove cached events for each independently registered descriptor alias.
        fn remove_discards_cached_events_and_handles_duplicated_fds() {
            let mut set = ReadySet::<Error>::new().unwrap();
            let (reader, mut writer) = UnixStream::pair().unwrap();
            let duplicate = reader.try_clone().unwrap();
            set.insert(reader.as_raw_fd(), 5).unwrap();
            assert_eq!(
                set.insert(duplicate.as_raw_fd(), 5),
                Err(Error::AlreadyExists)
            );
            set.insert(duplicate.as_raw_fd(), 6).unwrap();
            writer.write_all(b"ready").unwrap();
            let mut cx = Context::from_waker(std::task::Waker::noop());
            set.next(&mut cx, 2, |_| panic!("ready")).unwrap();
            assert_eq!(set.ready.len(), 1);
            set.remove(reader.as_raw_fd()).unwrap();
            set.remove(duplicate.as_raw_fd()).unwrap();
            assert!(set.ready.is_empty());
            assert_eq!(set.remove(reader.as_raw_fd()), Err(Error::NotFound));
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
                Ok(None)
            );
        }

        #[test]
        /// Retire failed waits so later calls never poll completed futures again.
        fn failed_idle_wait_is_consumed_not_polled_again() {
            let mut set = ReadySet::<Error>::new().unwrap();
            let mut cx = Context::from_waker(std::task::Waker::noop());
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(async { Err(Error::Io) })),
                Err(Error::Io)
            );
            assert!(set.wait.is_none());
            assert_eq!(
                set.next(&mut cx, 1, |_| Box::pin(std::future::pending())),
                Ok(None)
            );
        }
    }
}

#[cfg(test)]
mod tests;
