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
type Charge = Box<dyn std::any::Any>;
use io_uring::{IoUring, opcode, squeue, types};
pub mod descriptor;
pub use descriptor::Descriptor;
#[cfg(feature = "simulation")]
pub mod simulation;

// Only the selected backend constructs a submission, so simulated handles cannot
// accidentally enter an SQE or be converted to invented raw descriptor numbers.
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
enum Submission {
    Real(squeue::Entry),
    #[cfg(feature = "simulation")]
    Sim(simulation::Op),
}
// Control-owned filesystem extension; shares this reactor's completion fences.
pub mod filesystem;
pub mod ready_set;
pub mod timer;
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
    sync::Arc,
    task::{Context, Poll, Waker},
    time::Duration,
};
// A private one-element Vec keeps syscall backing stable across owner moves.
// Do not replace with Box: its move retagging invalidates derived pointers.
struct SyscallArg<T>(Vec<T>);
impl<T> SyscallArg<T> {
    fn new(value: T) -> Self {
        Self(vec![value])
    }
    fn as_ptr(&self) -> *const T {
        self.0.as_ptr()
    }
    fn as_mut_ptr(&mut self) -> *mut T {
        self.0.as_mut_ptr()
    }
    fn into_inner(mut self) -> T {
        self.0.pop().expect("one syscall argument")
    }
}

pub struct Reactor<S: Scope, B: Budget> {
    environment: super::environment::Environment,
    budget: B,
    queue_entries: usize,
    state: RefCell<State<S>>,
    ordinary: Rc<Cell<usize>>,
    reserved: RefCell<Weak<SubmissionCapacity>>,
}

/// Startup-owned partition of the existing entry and bookkeeping ceilings.
/// The caller supplies the retained charge; this capability grants no data authorization.
pub struct SubmissionCapacity {
    memory: Rc<Charge>,
    active: Rc<Cell<usize>>,
    capacity: usize,
}
pub const SUBMISSION_BYTES: usize = 4096;

struct SubmissionSlot {
    active: Rc<Cell<usize>>,
    _capacity: Option<Rc<SubmissionCapacity>>,
}
impl Drop for SubmissionSlot {
    fn drop(&mut self) {
        self.active.set(self.active.get() - 1);
    }
}
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
struct IoId(u64);

/// An owned address; the encoded sockaddr remains pinned in the in-flight owner.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum SocketAddress {
    Inet(SocketAddr),
    /// Filesystem pathname only. Abstract Unix addresses are unsupported; empty
    /// paths and embedded NUL bytes are rejected by both backends.
    Unix(PathBuf),
}

const CANCEL_BIT: u64 = 1 << 63;
const MAX_WAIT: Duration = Duration::from_millis(10);
const DROP_WAIT: Duration = Duration::from_millis(100);
// At most nine data attempts, each separated by a worker-driven CQE. A positive
// partial result is never replayed. EAGAIN additionally requires a readiness CQE.
const DATA_RETRIES: usize = 8;
// Linux UAPI: the only setup flag enabled by init. Unknown flags fail closed.
const SETUP_CQSIZE: u32 = 1 << 3;

struct State<S: Scope> {
    ring: Option<IoUring>,
    wake: Option<Arc<HostFd>>,
    #[cfg(feature = "simulation")]
    simulation: Option<simulation::Driver>,
    #[cfg(test)]
    submit_attempts: usize,
    #[cfg(test)]
    submit_result: Option<std::io::Result<usize>>,
    #[cfg(test)]
    completions: std::collections::VecDeque<(u64, KernelResult)>,
    ring_reservation: Option<Charge>,
    entries: BTreeMap<IoId, Entry<S>>,
    next: u64,
    scan_after: IoId,
    stopped: bool,
    fence_waiters: BTreeMap<(Option<IoId>, u64), FenceWaiter>,
    next_waiter: u64,
}

struct FenceWaiter {
    waker: Waker,
    _reservation: Charge,
}
impl FenceWaiter {
    fn wake(self) {
        self.waker.wake();
    }
}

struct Fence<'a, S: Scope, B: Budget> {
    reactor: &'a Reactor<S, B>,
    target: Option<IoId>,
    registration: Option<u64>,
}

impl<S: Scope, B: Budget> Future for Fence<'_, S, B> {
    type Output = Result<(), S::Error>;

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

/// Cloneable cross-thread wake endpoint for worker command/crypto producers.
#[derive(Clone)]
pub struct ReactorWake {
    fd: Option<Arc<HostFd>>,
}

impl ReactorWake {
    pub fn wake(&self) -> Result<()> {
        let Some(fd) = &self.fd else {
            return Ok(());
        };
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
                _ => return Err(Error::Io),
            }
        }
    }
}

struct Signal {
    abandoned: Cell<bool>,
    waker: RefCell<Option<Waker>>,
}

struct Reply<T, E> {
    // A reserved slot includes completion bookkeeping, not just the SQE. Keep it
    // until the result is consumed/dropped as well as the kernel being fenced.
    _slot: Option<SubmissionSlot>,
    result: Option<Result<T, E>>,
    // Charge completed-but-unconsumed results as well as submitted work.
    _reservation: Rc<Charge>,
}

struct Waiting<T, E> {
    reply: Rc<RefCell<Reply<T, E>>>,
    signal: Rc<Signal>,
}

impl<T, E> Future for Waiting<T, E> {
    type Output = Result<T, E>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let result = self.reply.borrow_mut().result.take();
        if let Some(result) = result {
            Poll::Ready(result)
        } else {
            let waker = cx.waker().clone();
            let old = self.signal.waker.borrow_mut().replace(waker);
            drop(old);
            // Clone/drop callbacks can drive completions before registration.
            let result = self.reply.borrow_mut().result.take();
            match result {
                Some(result) => Poll::Ready(result),
                None => Poll::Pending,
            }
        }
    }
}

impl<T, E> Drop for Waiting<T, E> {
    fn drop(&mut self) {
        self.signal.abandoned.set(true);
        let old = self.signal.waker.borrow_mut().take();
        drop(old);
    }
}

enum KernelResult {
    Value(i32),
    Accepted(Descriptor),
}

impl KernelResult {
    // Keep errno intact through internal zero-progress retries and the application
    // error boundary. Never infer retryability from the generic Error::Io.
    fn io_result(self) -> std::io::Result<i32> {
        match self {
            Self::Value(value) if value >= 0 => Ok(value),
            Self::Value(value) => Err(std::io::Error::from_raw_os_error(
                value.checked_neg().unwrap_or(libc::EIO),
            )),
            Self::Accepted(_) => Err(std::io::Error::from_raw_os_error(libc::EIO)),
        }
    }
    fn observe_errno(&self, errno: &std::cell::Cell<Option<i32>>) {
        if let Self::Value(value) = self
            && *value < 0
        {
            errno.set(value.checked_neg());
        }
    }
    fn value(self) -> Result<i32> {
        self.io_result().map_err(Error::from_io)
    }
}

type Finish<E> = Box<dyn FnOnce(Result<KernelResult, E>) -> Option<Waker>>;

struct Entry<S: Scope> {
    // The finish closure owns every FD, buffer, lease and sockaddr backing.
    finish: Finish<S::Error>,
    signal: Rc<Signal>,
    scope: Rc<S>,
    original: Option<KernelResult>,
    accept: bool,
    cancel_reason: Option<S::Error>,
    cancel_sent: bool,
    cancel_done: bool,
}

impl<S: Scope> Entry<S> {
    fn fenced(&self) -> bool {
        self.original.is_some() && (!self.cancel_sent || self.cancel_done)
    }
    fn finish(mut self) -> Option<Waker> {
        if self.cancel_reason.is_none() {
            self.cancel_reason = self.scope.check().err();
        }
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

/// An exclusively owned, stable backing allocation with an independent lifetime.
///
/// # Safety
/// Moving the owner or calling
/// either accessor must not relocate or resize its backing allocation. Accessors
/// expose the same initialized region; there must be no independently accessible
/// mutable aliases. Ownership includes the allocation's quota reservation. Neither
/// the allocation nor that reservation may be freed/recycled before the final fence.
/// `'static` excludes request-scoped borrows; it does not require leaking memory.
/// Derived raw pointers must also remain valid across owner moves into completion
/// closures. Stable addresses alone are insufficient: Box backing is retagged on
/// moves in Miri's aliasing models. Use private, non-resizing Vec storage or an
/// explicitly managed allocation; never reborrow its bytes while I/O is pending.
///
/// Inline arrays cannot implement this contract: moving them relocates the bytes.
pub unsafe trait IoBuffer: 'static {
    type Error;
    fn bytes(&self) -> Result<&[u8], Self::Error>;
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
    type Error;
    fn send_bytes(&self) -> Result<&[u8], Self::Error>;
}
// SAFETY: exclusive stable storage also meets immutable send requirements.
unsafe impl<B: IoBuffer> SendBuffer for B {
    type Error = B::Error;
    fn send_bytes(&self) -> Result<&[u8], Self::Error> {
        self.bytes()
    }
}

/// Reactor-owned submission state. `L` retains connection/segment/other leases.
/// Stored before the kernel can see any pointer, including across partial I/O.
struct InFlight<B: IoBuffer, L: 'static> {
    file: Rc<Descriptor>,
    buffer: B,
    lease: L,
}

/// Resources return to the caller only after all applicable completion fences.
/// On error or an abandoned future, the reactor releases them after those fences.
/// Data I/O retries up to eight zero-progress EINTR/EAGAIN results internally,
/// retaining owners between attempts. EAGAIN waits for readiness; each attempt
/// checks scope. Exhaustion returns the last errno and releases the owners.
/// Positive partial progress returns immediately, never repeats completed bytes.
pub struct Completion<B: 'static, L: 'static = ()> {
    pub buffer: B,
    pub bytes: usize,
    pub lease: L,
}

impl<S: Scope, Q: Budget> Reactor<S, Q> {
    fn charge(&self, bytes: usize) -> Result<Charge> {
        self.budget
            .charge(bytes)
            .map(|charge| Box::new(charge) as Charge)
    }
    pub fn new(queue_entries: usize, budget: Q) -> Self {
        Self {
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
                submit_result: None,
                #[cfg(test)]
                completions: Default::default(),
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
        let ring = IoUring::builder()
            .setup_cqsize(cq)
            .build(sq)
            .map_err(|_| Error::Io)?;
        // SAFETY: no borrowed pointers; ownership of the returned descriptor is unique.
        let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
        if fd < 0 {
            return Err(Error::Io);
        }
        state.wake = Some(Arc::new(unsafe { HostFd::from_raw_fd(fd) }));
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

    pub fn in_flight(&self) -> usize {
        self.state.borrow().entries.len()
    }

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

    fn submit<T: 'static>(
        &self,
        sqe: Submission,
        scope: &S,
        accept: bool,
        finish: impl FnOnce(Result<KernelResult, S::Error>) -> Result<T, S::Error> + 'static,
    ) -> Result<Waiting<T, S::Error>, S::Error> {
        self.submit_reserved(sqe, scope, accept, None, finish)
    }

    fn submit_reserved<T: 'static>(
        &self,
        sqe: Submission,
        scope: &S,
        accept: bool,
        capacity: Option<Rc<SubmissionCapacity>>,
        finish: impl FnOnce(Result<KernelResult, S::Error>) -> Result<T, S::Error> + 'static,
    ) -> Result<Waiting<T, S::Error>, S::Error> {
        scope.check()?;
        self.init()?;
        let scope = Rc::new(scope.clone());
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
                return Err(Error::Overloaded.into());
            }
            (pool.memory.clone(), pool.active.clone())
        } else {
            let reserved = self
                .reserved
                .borrow()
                .upgrade()
                .map_or(0, |pool| pool.capacity);
            if self.ordinary.get() >= self.queue_entries - reserved {
                return Err(Error::Overloaded.into());
            }
            (Rc::new(self.charge(bytes)?), self.ordinary.clone())
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
            drop(state);
            return Err(Error::Overloaded.into());
        }
        let id = IoId(state.next);
        if id.0 >= CANCEL_BIT {
            drop(state);
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
        let reply = Rc::new(RefCell::new(Reply {
            _slot: reserved_slot,
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
                    drop(ordinary_slot);
                    let result = finish(result);
                    if !notify.abandoned.get() {
                        output.borrow_mut().result = Some(result);
                    }
                    let mut pending_waker = notify.waker.borrow_mut();
                    let waker = pending_waker.take();
                    drop(pending_waker);
                    waker
                }),
                signal: signal.clone(),
                scope,
                original: None,
                accept,
                cancel_reason: None,
                cancel_sent: false,
                cancel_done: false,
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
            return Err(Error::Overloaded.into());
        }
        // Kernel submission is driven by poll_budgeted and by wait before sleeping,
        // including SQEs queued in the intervening service turn. Polling a future
        // only queues work; potentially blocking file operations use ASYNC.
        Ok(Waiting { reply, signal })
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
                let sqe = submission!(
                    self,
                    simulation::Op::Poll {
                        fd: fd.clone(),
                        interest
                    },
                    opcode::PollAdd::new(types::Fd(fd.as_raw_fd()), interest).build()
                );
                // Do not leave the buffer/lease in the waiting future while a
                // readiness SQE is pending. Abandonment must retain them too.
                let retained_fd = fd.clone();
                owned = self
                    .submit_reserved(sqe, scope, false, capacity.clone(), move |result| {
                        drop(retained_fd);
                        result?.value()?;
                        Ok(owned)
                    })?
                    .await?;
            }
        }
        unreachable!("bounded retries return on the final attempt")
    }

    fn buffer_io<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
        operation: BufferOperation,
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
            // File issue may block on filesystem work; force it off the worker.
            // Socket opcodes use io_uring's native nonblocking issue/poll path.
            let fd = owned.file.clone();
            let interest = match operation {
                BufferOperation::Read(_) | BufferOperation::Recv => libc::POLLIN,
                BufferOperation::Write(_) | BufferOperation::Send => libc::POLLOUT,
            } as u32;
            let (owned, bytes) = self
                .retry_data(owned, fd, scope, capacity, interest, |owned| {
                    Ok(submission!(
                        self,
                        {
                            let (ptr, len) = match operation {
                                BufferOperation::Read(_) | BufferOperation::Recv => {
                                    let bytes = owned.buffer.bytes_mut()?;
                                    (bytes.as_mut_ptr(), bytes.len())
                                }
                                BufferOperation::Write(_) | BufferOperation::Send => {
                                    let bytes = owned.buffer.bytes()?;
                                    (bytes.as_ptr().cast_mut(), bytes.len())
                                }
                            };
                            u32::try_from(len).map_err(|_| Error::InvalidInput)?;
                            simulation::Op::Buffer {
                                fd: owned.file.clone(),
                                operation,
                                ptr,
                                len,
                            }
                        },
                        {
                            let fd = types::Fd(owned.file.as_raw_fd());
                            match operation {
                                BufferOperation::Read(_) | BufferOperation::Recv => {
                                    let bytes = owned.buffer.bytes_mut()?;
                                    let len = u32::try_from(bytes.len())
                                        .map_err(|_| Error::InvalidInput)?;
                                    match operation {
                                        BufferOperation::Read(_) => {
                                            opcode::Read::new(fd, bytes.as_mut_ptr(), len)
                                                .offset(offset_or_zero(operation))
                                                .build()
                                                .flags(squeue::Flags::ASYNC)
                                        }
                                        _ => opcode::Recv::new(fd, bytes.as_mut_ptr(), len).build(),
                                    }
                                }
                                BufferOperation::Write(_) | BufferOperation::Send => {
                                    let bytes = owned.buffer.bytes()?;
                                    let len = u32::try_from(bytes.len())
                                        .map_err(|_| Error::InvalidInput)?;
                                    match operation {
                                        BufferOperation::Write(_) => {
                                            opcode::Write::new(fd, bytes.as_ptr(), len)
                                                .offset(offset_or_zero(operation))
                                                .build()
                                                .flags(squeue::Flags::ASYNC)
                                        }
                                        _ => opcode::Send::new(fd, bytes.as_ptr(), len)
                                            .flags(libc::MSG_NOSIGNAL)
                                            .build(),
                                    }
                                }
                            }
                        }
                    ))
                })
                .await?;
            let InFlight {
                file,
                buffer,
                lease,
            } = owned;
            drop(file);
            Ok(Completion {
                buffer,
                bytes,
                lease,
            })
        })
    }
    /// Transfer the FD, buffer, and any reuse-preventing lease (`()` if none).
    /// Owned resources can move from one completed operation to the next:
    /// ```no_run
    /// use std::rc::Rc;
    /// use uring_runtime::{Budget, Scope, Result, reactor::{Descriptor, IoBuffer, Completion, Reactor}};
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
    /// use uring_runtime::{Budget, Scope, reactor::{Descriptor, IoBuffer, Reactor}};
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
            None,
        )
    }
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
            None,
        )
    }

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
        self.buffer_io(fd, buffer, lease, scope, BufferOperation::Recv, None)
    }

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
        self.buffer_io(fd, buffer, (), scope, BufferOperation::Recv, Some(capacity))
    }

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
        self.buffer_io(fd, buffer, (), scope, BufferOperation::Send, Some(capacity))
    }

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
            let ((fd, buffer, lease), bytes) = self
                .retry_data(
                    (fd, buffer, lease),
                    retained_fd,
                    scope,
                    None,
                    libc::POLLOUT as u32,
                    |(fd, buffer, _)| {
                        let bytes = buffer.send_bytes()?;
                        let len = u32::try_from(bytes.len()).map_err(|_| Error::InvalidInput)?;
                        let sqe = submission!(
                            self,
                            simulation::Op::Buffer {
                                fd: fd.clone(),
                                operation: BufferOperation::Send,
                                // Simulation creates a shared slice for Send, never a mutable one.
                                ptr: bytes.as_ptr().cast_mut(),
                                len: len as usize,
                            },
                            opcode::Send::new(types::Fd(fd.as_raw_fd()), bytes.as_ptr(), len)
                                .flags(libc::MSG_NOSIGNAL)
                                .build()
                        );
                        Ok(sqe)
                    },
                )
                .await?;
            drop(fd);
            Ok(Completion {
                buffer,
                bytes,
                lease,
            })
        })
    }

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
            let sqe = submission!(
                self,
                simulation::Op::Poll {
                    fd: fd.clone(),
                    interest
                },
                opcode::PollAdd::new(types::Fd(fd.as_raw_fd()), interest).build()
            );
            self.submit(sqe, scope, false, move |result| {
                drop(fd);
                drop(lease);
                Ok(result?.value()? as u32)
            })?
            .await
        })
    }

    pub fn accept<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, Descriptor, S::Error> {
        self.accept_reserved(fd, None, scope)
    }

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
        let mut finished = Vec::new();
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
        for entry in finished {
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                if let Some(waker) = entry.finish() {
                    waker.wake();
                }
            }));
            if let Err(payload) = result {
                panic.get_or_insert(payload);
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
        let mut state = self.state.borrow_mut();
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
            let scope = state.entries[&id].scope.clone();
            drop(state);
            let cancelled = Error::Cancelled.into();
            let checked = scope.check().err();
            drop(scope);
            state = self.state.borrow_mut();
            let stopped = state.stopped;
            let Some(entry) = state.entries.get_mut(&id) else {
                continue;
            };
            if entry.cancel_reason.is_none() {
                entry.cancel_reason = if stopped || entry.signal.abandoned.get() {
                    Some(cancelled)
                } else {
                    checked
                };
            }
            if entry.original.is_none() && entry.cancel_reason.is_some() && !entry.cancel_sent {
                #[cfg(feature = "simulation")]
                if let Some(driver) = &mut state.simulation {
                    driver.cancel(id.0);
                    state.entries.get_mut(&id).unwrap().cancel_sent = true;
                    continue;
                }
                let sqe = opcode::AsyncCancel::new(id.0)
                    .build()
                    .user_data(id.0 | CANCEL_BIT);
                // SAFETY: cancel uses only an ID, and its target entry is retained.
                if unsafe { state.ring.as_mut().unwrap().submission().push(&sqe) }.is_ok() {
                    state.entries.get_mut(&id).unwrap().cancel_sent = true;
                }
            }
        }
        let submitted = state.submit_pending();
        if submitted.is_err() {
            state.stopped = true;
        }
        submitted.map(|()| completed)
    }

    /// Sleep until a CQE/external wake, bounded by both duration and a 10ms timer
    /// fallback for producers that cannot yet attach the eventfd wake endpoint.
    pub fn wait(&self, duration: Duration) -> Result<()> {
        let mut state = self.state.borrow_mut();
        // Service polling can queue SQEs after poll_budgeted. Submit once before
        // sleeping so that work can produce the CQEs we wait for. Transient errors
        // defer progress to the next worker turn; completion/cancel budgets stay
        // in poll_budgeted, and an absent ring remains uninitialized.
        if let Err(error) = state.submit_pending() {
            state.stopped = true;
            return Err(error);
        }
        #[cfg(feature = "simulation")]
        if state.simulation.is_some() {
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
                .map(|(id, entry)| (*id, entry.scope.clone()))
                .collect();
            let ids: Vec<_> = scopes
                .into_iter()
                .filter(|(_, scope)| matches(scope))
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

impl<S: Scope> State<S> {
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

    fn take_fence_wakers(&mut self, target: Option<IoId>) -> Vec<FenceWaiter> {
        let keys: Vec<_> = self
            .fence_waiters
            .range((target, 0)..=(target, u64::MAX))
            .map(|(key, _)| *key)
            .collect();
        keys.into_iter()
            .map(|key| self.fence_waiters.remove(&key).unwrap())
            .collect()
    }

    fn complete(&mut self, tag: u64, result: impl Into<KernelResult>) -> Result<Option<Entry<S>>> {
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
            entry.original = Some(match result.into() {
                // Raw CQEs supplied by the real backend (and fence tests) transfer
                // ownership here. Simulated opens already carry a typed owner.
                KernelResult::Value(fd) if entry.accept && fd >= 0 => {
                    KernelResult::Accepted(unsafe { Descriptor::from_raw_fd(fd) })
                }
                result => result,
            });
        }
        if entry.fenced() {
            Ok(self.entries.remove(&id))
        } else {
            Ok(None)
        }
    }
}

impl From<i32> for KernelResult {
    fn from(value: i32) -> Self {
        Self::Value(value)
    }
}

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

fn submission_required(setup_flags: u32, empty: bool, cq_overflow: bool, taskrun: bool) -> bool {
    // Only the normal interrupt-driven mode is supported for suppression.
    // SQPOLL, IOPOLL, all task-run modes and future flags retain ring.submit().
    setup_flags & !SETUP_CQSIZE != 0 || !empty || cq_overflow || taskrun
}

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
    fn drop(&mut self) {
        if std::thread::panicking() || self.shutdown(DROP_WAIT).is_err() {
            // Closing an io_uring FD alone is NOT a synchronous memory fence.
            // On an unrecoverable driver failure retain the bounded owners and
            // ring forever rather than free memory the kernel may still use.
            let state = self.state.get_mut();
            std::mem::forget(std::mem::take(&mut state.entries));
            std::mem::forget(state.ring.take());
            std::mem::forget(state.ring_reservation.take());
            return;
        }
        // Every SQE (including cancellation SQEs) has now produced its CQE.
        self.state.get_mut().ring.take();
    }
}

impl<S: Scope> Drop for State<S> {
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

#[cfg(test)]
mod tests;
