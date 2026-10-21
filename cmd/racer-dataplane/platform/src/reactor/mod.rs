//! Racer's reactor surface over [`uring::Reactor`].
//!
//! [`Reactor<S, B>`] recreates the API of the old internal runtime's reactor
//! (`uring_runtime::reactor::Reactor`) on top of the library, so Racer crates
//! migrate mostly by changing import paths. It owns Racer policy the library
//! deliberately does not:
//!
//! - Lazy initialization: [`Reactor::new`] opens no kernel resources;
//!   [`Reactor::init`] (or the first operation) creates the ring and charges
//!   its memory to the [`Budget`].
//! - Admission accounting: every operation charges its bookkeeping bytes to
//!   the [`Budget`] (or to a prepaid [`SubmissionCapacity`]) and holds the
//!   charge, as a library lease, until the operation is fenced.
//! - One optional reserved partition carved out of `queue_entries`.
//! - The data retry policy: zero-progress `EINTR`/`EAGAIN` from read, write,
//!   recv and send is retried up to [`DATA_RETRIES`] times, waiting for
//!   readiness after `EAGAIN`. Positive partial progress returns at once.
//! - The old rejection report shape ([`SubmissionRejection`]).
//!
//! # Differences from the old reactor
//!
//! - The kernel result wins over cancellation (library default): an operation
//!   that completed successfully is returned as `Ok` even if its scope failed
//!   or a fence canceled it meanwhile. A failed completion of a canceled
//!   operation reports the scope's error (or [`Error::Canceled`]).
//!   Previously cancellation always won and the scope was checked again when
//!   the result was delivered. [`Reactor::sleep_until`] still checks the scope
//!   after the timer fires.
//! - Operation futures resolve to `Result<_, E>`; on error the buffer and
//!   lease are released (after the fence), as before. The library returns
//!   them, but the old result shape has no place for them.
//! - Kernel errors keep their errno through [`Error::from`] (ring setup
//!   failures used to be reported as [`Error::Io`]).
//! - The cancellation scan inspects at most 64 operations per
//!   [`Reactor::poll_budgeted`] (it was `budget`); canceled
//!   [`Cancellation`](crate::Cancellation) tokens wake the reactor directly.
//! - Fence registrations are no longer bounded or charged.
//! - A reserved slot is released when the operation is fenced and its result
//!   collected (as before), but there is no separate bound on unread replies.

mod backend;
pub mod descriptor;
pub mod ready_set;
#[cfg(test)]
mod scripted;
#[cfg(all(test, feature = "simulation"))]
mod sim_tests;
#[cfg(test)]
pub(crate) mod tests;
pub mod timer;

pub use backend::{RacerBackend, RacerClock, RacerEntropy};
pub use uring::SocketAddress;

use crate::{
    Budget, Error, Operation, Result, Scope, environment::Environment, scope::LibraryScope,
};
use descriptor::Descriptor;
use std::{
    any::Any,
    cell::{Cell, OnceCell, RefCell},
    marker::PhantomData,
    rc::{Rc, Weak},
    time::{Duration, Instant},
};
use uring::{BufResult, CancelToken, IoBuf, IoBufMut, ReactorConfig};

/// The library reactor a Racer reactor drives.
pub type Core = uring::Reactor<RacerBackend>;

/// Maximum completion bookkeeping charged to one prepaid submission slot.
pub const SUBMISSION_BYTES: usize = 4096;

/// Zero-progress `EINTR`/`EAGAIN` retries of one data operation.
pub const DATA_RETRIES: usize = 8;

/// Upper bound of one [`Reactor::wait`].
const MAX_WAIT: Duration = Duration::from_millis(10);

/// How long dropping a reactor may block to fence work in flight.
const DROP_WAIT: Duration = Duration::from_millis(100);

/// Estimated per-operation reactor bookkeeping (table entry, waker, node
/// header), excluding the scope, lease and inputs.
const ENTRY_BYTES: usize = 256;

/// Submission events describe rejected operations, not final client outcomes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RejectionReason {
    /// Charging the ring memory failed with [`Error::Overloaded`].
    Initialization,
    /// The reserved partition is full.
    ReservedFull,
    /// The ordinary partition is full.
    OrdinaryFull,
    /// Charging the operation's bookkeeping failed with [`Error::Overloaded`].
    Bookkeeping,
    /// The reactor's in-flight table is full.
    EntriesFull,
    /// The partition filled while the operation was being charged.
    PartitionFull,
    /// Operation identifiers are exhausted (no longer produced).
    IdExhausted,
    /// The submission queue stayed full after a submit.
    SubmissionQueueFull,
    /// The backend refused or does not support the operation.
    Backend,
}

/// Which family of operation was rejected.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SubmissionKind {
    /// Data, socket and filesystem operations.
    OtherIo,
    /// A readiness poll.
    Readiness,
    /// A kernel timer.
    Timeout,
}

/// Facts about one rejected submission.
#[derive(Clone, Copy, Debug)]
pub struct SubmissionRejection {
    /// Why it was rejected.
    pub reason: RejectionReason,
    /// The operation family.
    pub kind: SubmissionKind,
    /// Whether it asked for the reserved partition.
    pub reserved: bool,
    /// Slots or bytes in use, when known.
    pub used: Option<usize>,
    /// The applicable limit, when known.
    pub limit: Option<usize>,
    /// Slots or bytes requested.
    pub requested: usize,
}

type Observer<S> = Rc<dyn Fn(&S, SubmissionRejection)>;

/// Erased admission guard retained for the lifetime of its accounted owner.
type Charge = Box<dyn Any>;

/// Startup-owned partition of the reactor's `queue_entries`.
/// The caller supplies the retained charge; this capability grants no data
/// authorization.
pub struct SubmissionCapacity {
    memory: Rc<dyn Any>,
    active: Rc<Cell<usize>>,
    capacity: usize,
}

impl SubmissionCapacity {
    /// Slots in the partition.
    pub fn capacity(&self) -> usize {
        self.capacity
    }

    /// Slots held by unfenced or uncollected operations.
    pub fn active(&self) -> usize {
        self.active.get()
    }
}

/// Cloneable cross-thread wake endpoint for worker command/crypto producers.
#[derive(Clone, Debug)]
pub struct ReactorWake {
    waker: Option<uring::backend::RemoteWaker>,
}

impl ReactorWake {
    /// The backend wake endpoint, if the backend has one.
    pub(crate) fn into_remote(self) -> Option<uring::backend::RemoteWaker> {
        self.waker
    }

    /// Wrap a backend wake endpoint (tests).
    #[cfg(test)]
    pub(crate) fn from_remote(waker: uring::backend::RemoteWaker) -> Self {
        Self { waker: Some(waker) }
    }

    /// Notify the worker without blocking. Wakes from the reactor's own thread
    /// only mark the next wait as non-blocking.
    pub fn wake(&self) -> Result<()> {
        if let Some(waker) = &self.waker {
            waker.wake();
        }
        Ok(())
    }
}

/// Resources returned to the caller after every applicable completion fence.
/// Data I/O retries up to [`DATA_RETRIES`] zero-progress `EINTR`/`EAGAIN`
/// results, retaining owners between attempts. Positive partial progress
/// returns immediately, never repeating completed bytes.
pub struct Completion<B: 'static, L: 'static = ()> {
    /// Original allocation, returned only after all applicable completions.
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
/// Derived raw pointers must remain valid across owner moves. Box backing is
/// retagged on moves in Miri's aliasing models: use private, non-resizing Vec
/// storage or an explicitly managed allocation, and never reborrow its bytes
/// while I/O is pending. Inline arrays cannot implement this contract.
pub unsafe trait IoBuffer: 'static {
    /// Application error returned when access to this allocation is rejected.
    type Error;

    /// Borrow the initialized region after its previous operation has fenced.
    fn bytes(&self) -> Result<&[u8], Self::Error>;

    /// Exclusively borrow the same region before deriving a receive pointer.
    fn bytes_mut(&mut self) -> Result<&mut [u8], Self::Error>;
}

/// Completion-owned immutable send storage. Shared aliases are permitted only
/// when none can mutate or relocate the initialized bytes.
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

// SAFETY: exclusive stable storage also meets immutable send requirements.
unsafe impl<B: IoBuffer> SendBuffer for B {
    type Error = B::Error;

    fn send_bytes(&self) -> Result<&[u8], Self::Error> {
        self.bytes()
    }
}

/// Completion-owned immutable storage for the body of a vectored file write.
///
/// This is the file-write counterpart of [`SendBuffer`] and carries the same
/// contract. It is blanket-implemented for every [`SendBuffer`] (and therefore
/// every [`IoBuffer`]), so shared, reference-counted bodies can be written
/// without copying them into exclusive storage.
///
/// # Safety
/// The slice returned by [`write_bytes`](Self::write_bytes) must keep the same
/// address and length, and its bytes must stay initialized and immutable, for
/// as long as the owner is alive, including across moves of the owner. No
/// shared alias may mutate, resize, or free the bytes while the owner lives.
pub unsafe trait SharedWriteBuffer: 'static {
    /// Application error returned when access to this allocation is rejected.
    type Error;

    /// Borrow initialized immutable bytes for one write operation.
    fn write_bytes(&self) -> Result<&[u8], Self::Error>;
}

// SAFETY: the SendBuffer contract is the SharedWriteBuffer contract.
unsafe impl<B: SendBuffer> SharedWriteBuffer for B {
    type Error = B::Error;

    fn write_bytes(&self) -> Result<&[u8], Self::Error> {
        self.send_bytes()
    }
}

/// Owners returned by [`Reactor::write_vectored_at`] after every applicable
/// completion fence.
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

/// A Racer buffer viewed as a library buffer through a pointer derived once.
struct Region<T> {
    owner: T,
    ptr: *mut u8,
    len: usize,
}

impl<T> Region<T> {
    fn new(owner: T, ptr: *mut u8, len: usize) -> Result<Self> {
        if u32::try_from(len).is_err() {
            return Err(Error::InvalidInput);
        }
        Ok(Self { owner, ptr, len })
    }
}

/// Mutable view for read/recv.
struct Exclusive<T>(Region<T>);
/// Shared view for write/send.
struct Shared<T>(Region<T>);

// SAFETY: the pointer was derived from the owner's stable allocation, which the
// IoBuffer/SendBuffer contracts keep valid and unmoved while the owner moves.
unsafe impl<T: 'static> IoBuf for Exclusive<T> {
    fn stable_ptr(&self) -> *const u8 {
        self.0.ptr
    }

    fn bytes_init(&self) -> usize {
        self.0.len
    }
}

// SAFETY: derived from `IoBuffer::bytes_mut`, so exclusively writable for
// `len` bytes; the whole region is already initialized.
unsafe impl<T: 'static> IoBufMut for Exclusive<T> {
    fn stable_mut_ptr(&mut self) -> *mut u8 {
        self.0.ptr
    }

    fn bytes_total(&mut self) -> usize {
        self.0.len
    }

    unsafe fn set_init(&mut self, _: usize) {}
}

// SAFETY: as for `Exclusive`; only read access is exposed.
unsafe impl<T: 'static> IoBuf for Shared<T> {
    fn stable_ptr(&self) -> *const u8 {
        self.0.ptr
    }

    fn bytes_init(&self) -> usize {
        self.0.len
    }
}

/// The scope type submitted to the library: the caller's scope, with an error
/// that keeps kernel failures (and their errno) apart from scope failures.
#[repr(transparent)]
#[derive(Clone)]
pub(crate) struct OpScope<S>(pub(crate) S);

impl<S> OpScope<S> {
    pub(crate) fn wrap(scope: &S) -> &Self {
        // SAFETY: `OpScope<S>` is `repr(transparent)` over `S`.
        unsafe { &*(scope as *const S).cast::<Self>() }
    }
}

/// Why a library operation failed.
pub(crate) enum Failure<E> {
    /// The caller's scope failed.
    Scope(E),
    /// The kernel, the reactor or admission failed.
    Runtime(uring::Error),
}

impl<E> From<uring::Error> for Failure<E> {
    fn from(error: uring::Error) -> Self {
        Self::Runtime(error)
    }
}

impl<E: From<Error>> Failure<E> {
    pub(crate) fn into_scope(self) -> E {
        match self {
            Self::Scope(error) => error,
            Self::Runtime(error) => Error::from(error).into(),
        }
    }

    fn errno(&self) -> Option<i32> {
        match self {
            Self::Runtime(error) => error.raw_os_error(),
            Self::Scope(_) => None,
        }
    }
}

impl<S: Scope> LibraryScope for OpScope<S> {
    type Error = Failure<S::Error>;

    fn check(&self) -> Result<(), Self::Error> {
        self.0.check().map_err(Failure::Scope)
    }

    fn cancellation(&self) -> Option<&CancelToken> {
        self.0.cancellation()
    }

    fn deadline(&self) -> Option<Instant> {
        self.0.deadline()
    }
}

/// One admitted slot in either partition.
struct Slot {
    active: Rc<Cell<usize>>,
    _capacity: Option<Rc<SubmissionCapacity>>,
}

impl Drop for Slot {
    fn drop(&mut self) {
        self.active.set(self.active.get() - 1);
    }
}

/// Accounting retained for one operation.
enum Accounting {
    Charged(#[allow(dead_code)] Charge),
    Prepaid(#[allow(dead_code)] Rc<dyn Any>),
}

/// The library lease of every operation: the slot, its charge and the
/// caller's lease, released together at the fence.
pub(crate) struct Held<L> {
    pub(crate) user: L,
    _slot: Slot,
    _accounting: Accounting,
}

impl<L> Held<L> {
    /// Release the slot and accounting now (the operation is fenced) and
    /// return the caller's value. A partial move out of `user` would keep
    /// the slot until the end of the enclosing scope.
    pub(crate) fn into_user(self) -> L {
        self.user
    }
}

/// Worker-local I/O owner, explicitly driven by [`Self::poll_budgeted`].
/// Dropping a waiting operation abandons delivery, never its kernel-owned
/// resources.
pub struct Reactor<S: Scope, B: Budget> {
    observer: Option<Observer<S>>,
    environment: Environment,
    /// The simulation current at construction, as the old reactor selected it.
    #[cfg(feature = "simulation")]
    simulation: Option<crate::simulation::Simulation>,
    budget: B,
    queue_entries: usize,
    core: OnceCell<Core>,
    ring_charge: RefCell<Option<Charge>>,
    stopped: Cell<bool>,
    ordinary: Rc<Cell<usize>>,
    reserved: RefCell<Weak<SubmissionCapacity>>,
    #[cfg(test)]
    backend: Cell<Option<RacerBackend>>,
    _scope: PhantomData<fn(S)>,
}

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Create an uninitialized reactor; kernel resources are acquired by
    /// [`Self::init`].
    pub fn new(queue_entries: usize, budget: B) -> Self {
        Self {
            observer: None,
            environment: Environment::current(),
            #[cfg(feature = "simulation")]
            simulation: crate::simulation::Simulation::current(),
            budget,
            queue_entries,
            core: OnceCell::new(),
            ring_charge: RefCell::new(None),
            stopped: Cell::new(false),
            ordinary: Rc::new(Cell::new(0)),
            reserved: RefCell::default(),
            #[cfg(test)]
            backend: Cell::new(None),
            _scope: PhantomData,
        }
    }

    /// Use `backend` instead of [`RacerBackend::select`] at initialization.
    #[cfg(test)]
    pub(crate) fn with_backend(queue_entries: usize, budget: B, backend: RacerBackend) -> Self {
        let reactor = Self::new(queue_entries, budget);
        reactor.backend.set(Some(backend));
        reactor
    }

    /// Observe rejected submissions only, without changing the returned error.
    pub fn with_rejection_observer(
        mut self,
        observer: impl Fn(&S, SubmissionRejection) + 'static,
    ) -> Self {
        self.observer = Some(Rc::new(observer));
        self
    }

    fn rejected(&self, scope: &S, facts: SubmissionRejection) {
        if let Some(observer) = &self.observer {
            observer(scope, facts);
        }
    }

    /// The backend for the simulation and environment captured at
    /// construction: simulated when a [`Simulation`](crate::simulation::Simulation)
    /// was current, io_uring otherwise.
    fn select_backend(&self) -> RacerBackend {
        #[cfg(feature = "simulation")]
        if let Some(simulation) = &self.simulation {
            return RacerBackend::simulated(simulation, &self.environment);
        }
        RacerBackend::Uring(uring::Uring::new())
    }

    /// Whether this reactor runs (or will run) on the simulated backend.
    pub fn is_simulated(&self) -> bool {
        #[cfg(feature = "simulation")]
        if self.simulation.is_some() {
            return true;
        }
        self.core
            .get()
            .is_some_and(|core| core.backend().is_simulated())
    }

    /// The library reactor, if initialized.
    pub fn core(&self) -> Option<&Core> {
        self.core.get()
    }

    /// Initialize the selected backend once.
    pub fn init(&self) -> Result<()> {
        self.initialized().map(drop)
    }

    fn initialized(&self) -> Result<&Core> {
        if self.stopped.get() {
            return Err(Error::Unavailable);
        }
        if let Some(core) = self.core.get() {
            return Ok(core);
        }
        let capacity = self.queue_entries;
        if capacity == 0 {
            return Err(Error::InvalidConfiguration);
        }
        // One original and one cancel per admitted operation.
        let sq = u32::try_from(capacity.checked_mul(2).ok_or(Error::InvalidConfiguration)?)
            .map_err(|_| Error::InvalidConfiguration)?;
        // Ring infrastructure is memory, charged once.
        let ring_bytes = (sq as usize)
            .checked_next_power_of_two()
            .and_then(|sq| sq.checked_mul(128))
            .and_then(|bytes| bytes.checked_add(8192))
            .ok_or(Error::InvalidConfiguration)?;
        let charge = self.charge(ring_bytes)?;
        // The accounting hook may reenter and initialize or stop the reactor.
        if self.stopped.get() {
            return Err(Error::Unavailable);
        }
        if let Some(core) = self.core.get() {
            return Ok(core);
        }
        #[cfg(test)]
        let backend = self.backend.take().unwrap_or_else(|| self.select_backend());
        #[cfg(not(test))]
        let backend = self.select_backend();
        let config = ReactorConfig::default()
            .with_max_in_flight(capacity)
            .with_max_wait(MAX_WAIT)
            .with_cancel_scan_budget(64);
        let core = Core::new(backend, config).map_err(Error::from)?;
        if let Some(observer) = self.observer.clone() {
            let limit = capacity;
            core.set_rejection_observer(move |rejection| {
                use uring::reactor::RejectionReason as Library;
                let Some(OpScope(scope)) = rejection.scope.downcast_ref::<OpScope<S>>() else {
                    return;
                };
                let (reason, used, limit) = match rejection.reason {
                    Library::Capacity => (RejectionReason::EntriesFull, Some(limit), Some(limit)),
                    Library::QueueFull => (RejectionReason::SubmissionQueueFull, None, None),
                    // The old reactor did not report closed admission.
                    Library::Closed | Library::Failed => return,
                    _ => (RejectionReason::Backend, None, None),
                };
                observer(
                    scope,
                    SubmissionRejection {
                        reason,
                        kind: submission_kind(rejection.kind),
                        reserved: rejection.reserved,
                        used,
                        limit,
                        requested: 1,
                    },
                );
            });
        }
        *self.ring_charge.borrow_mut() = Some(charge);
        Ok(self.core.get_or_init(|| core))
    }

    /// Obtain after init, and attach to external worker queues before starting them.
    pub fn waker(&self) -> Result<ReactorWake> {
        let core = self.initialized()?;
        Ok(ReactorWake {
            waker: Some(core.remote_waker()),
        })
    }

    /// Count operations still awaiting their original or cancellation completion.
    pub fn in_flight(&self) -> usize {
        self.core.get().map_or(0, Core::in_flight)
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
            memory: Rc::new(charge),
            active: Rc::new(Cell::new(0)),
            capacity,
        });
        *self.reserved.borrow_mut() = Rc::downgrade(&reserved);
        Ok(reserved)
    }

    /// Retain a budget guard independently of the concrete accounting implementation.
    pub(crate) fn charge(&self, bytes: usize) -> Result<Charge> {
        self.budget
            .charge(bytes)
            .map(|charge| Box::new(charge) as Charge)
    }

    /// Admit one operation: check the scope, initialize, take a slot in the
    /// requested partition and charge its bookkeeping.
    pub(crate) fn admit<L: 'static>(
        &self,
        scope: &S,
        capacity: Option<Rc<SubmissionCapacity>>,
        kind: SubmissionKind,
        bytes: usize,
        user: L,
    ) -> Result<(&Core, Held<L>), S::Error> {
        scope.check()?;
        let reserved = capacity.is_some();
        let reject = |reason, used, limit, requested| {
            self.rejected(
                scope,
                SubmissionRejection {
                    reason,
                    kind,
                    reserved,
                    used,
                    limit,
                    requested,
                },
            );
        };
        let core = self.initialized().inspect_err(|error| {
            if *error == Error::Overloaded {
                reject(RejectionReason::Initialization, None, None, 1);
            }
        })?;
        let (accounting, active) = match &capacity {
            Some(pool) => {
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
                    Accounting::Prepaid(pool.memory.clone()),
                    pool.active.clone(),
                )
            }
            None => {
                let limit = self.ordinary_limit();
                if self.ordinary.get() >= limit {
                    reject(
                        RejectionReason::OrdinaryFull,
                        Some(self.ordinary.get()),
                        Some(limit),
                        1,
                    );
                    return Err(Error::Overloaded.into());
                }
                let charge = self.charge(bytes).inspect_err(|error| {
                    if *error == Error::Overloaded {
                        reject(RejectionReason::Bookkeeping, None, None, bytes);
                    }
                })?;
                (Accounting::Charged(charge), self.ordinary.clone())
            }
        };
        if self.stopped.get() {
            return Err(Error::Unavailable.into());
        }
        // The charge hook may have reentered and used the last slot.
        let limit = capacity
            .as_ref()
            .map_or_else(|| self.ordinary_limit(), |pool| pool.capacity);
        if active.get() >= limit {
            reject(
                RejectionReason::PartitionFull,
                Some(active.get()),
                Some(limit),
                1,
            );
            return Err(Error::Overloaded.into());
        }
        active.set(active.get() + 1);
        Ok((
            core,
            Held {
                user,
                _slot: Slot {
                    active,
                    _capacity: capacity,
                },
                _accounting: accounting,
            },
        ))
    }

    fn ordinary_limit(&self) -> usize {
        let reserved = self
            .reserved
            .borrow()
            .upgrade()
            .map_or(0, |pool| pool.capacity);
        self.queue_entries - reserved
    }

    /// Bookkeeping bytes charged for one operation carrying `X` and lease `L`.
    pub(crate) fn bookkeeping<L, X>() -> usize {
        ENTRY_BYTES
            + std::mem::size_of::<S>()
            + std::mem::size_of::<Held<L>>()
            + std::mem::size_of::<X>()
            + std::mem::size_of::<libc::sockaddr_storage>()
    }

    /// One data operation with the retry policy.
    #[allow(clippy::too_many_arguments)]
    async fn data<T: 'static, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        mut buffer: T,
        mut lease: L,
        scope: &S,
        operation: Data,
        offload: bool,
        capacity: Option<Rc<SubmissionCapacity>>,
        region: impl Fn(&mut T) -> Result<(*mut u8, usize), S::Error>,
    ) -> Result<Completion<T, L>, S::Error> {
        if matches!(operation, Data::Read(offset) | Data::Write(offset) if offset > i64::MAX as u64)
        {
            return Err(Error::InvalidInput.into());
        }
        let op_scope = OpScope::wrap(scope);
        for attempt in 0..=DATA_RETRIES {
            scope.check()?;
            let (ptr, len) = region(&mut buffer)?;
            let view = Region::new(buffer, ptr, len)?;
            let bytes = Self::bookkeeping::<L, Region<T>>();
            let (core, held) = self.admit(
                scope,
                capacity.clone(),
                SubmissionKind::OtherIo,
                bytes,
                lease,
            )?;
            let (result, held) = match operation {
                Data::Read(offset) => {
                    let op = core
                        .read_at(&fd, offset, Exclusive(view), op_scope)
                        .lease(held);
                    let (BufResult { result, buf }, held) = if offload {
                        op.offload().await
                    } else {
                        op.await
                    };
                    buffer = buf.0.owner;
                    (result, held)
                }
                Data::Recv => {
                    let (BufResult { result, buf }, held) =
                        core.recv(&fd, Exclusive(view), op_scope).lease(held).await;
                    buffer = buf.0.owner;
                    (result, held)
                }
                Data::Write(offset) => {
                    let op = core
                        .write_at(&fd, offset, Shared(view), op_scope)
                        .lease(held);
                    let (BufResult { result, buf }, held) = if offload {
                        op.offload().await
                    } else {
                        op.await
                    };
                    buffer = buf.0.owner;
                    (result, held)
                }
                Data::Send => {
                    let (BufResult { result, buf }, held) =
                        core.send(&fd, Shared(view), op_scope).lease(held).await;
                    buffer = buf.0.owner;
                    (result, held)
                }
            };
            lease = held.into_user();
            let failure = match result {
                Ok(bytes) => {
                    return Ok(Completion {
                        buffer,
                        bytes,
                        lease,
                    });
                }
                Err(failure) => failure,
            };
            let errno = failure.errno();
            if attempt == DATA_RETRIES || !matches!(errno, Some(libc::EINTR | libc::EAGAIN)) {
                return Err(failure.into_scope());
            }
            scope.check()?;
            if errno == Some(libc::EAGAIN) {
                // Keep the buffer and lease in the readiness operation so that
                // abandonment retains them until its fence too.
                let bytes = Self::bookkeeping::<(T, L), ()>();
                let (core, held) = self.admit(
                    scope,
                    capacity.clone(),
                    SubmissionKind::Readiness,
                    bytes,
                    (buffer, lease),
                )?;
                let (result, held) = core
                    .readiness(&fd, operation.interest(), op_scope)
                    .lease(held)
                    .await;
                (buffer, lease) = held.into_user();
                result.map_err(Failure::into_scope)?;
            }
        }
        unreachable!("bounded retries return on the final attempt")
    }

    /// Exclusive region of an [`IoBuffer`].
    fn exclusive<T: IoBuffer>(buffer: &mut T) -> Result<(*mut u8, usize), S::Error>
    where
        S::Error: From<T::Error>,
    {
        let bytes = buffer.bytes_mut()?;
        Ok((bytes.as_mut_ptr(), bytes.len()))
    }

    /// Shared region of a [`SendBuffer`].
    fn shared<T: SendBuffer>(buffer: &mut T) -> Result<(*mut u8, usize), S::Error>
    where
        S::Error: From<T::Error>,
    {
        let bytes = buffer.send_bytes()?;
        Ok((bytes.as_ptr().cast_mut(), bytes.len()))
    }

    /// Read into owned storage at an explicit offset, retaining the lease
    /// through fencing.
    pub fn read_at<'a, T: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<T, L>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            file,
            buffer,
            lease,
            scope,
            Data::Read(offset),
            false,
            None,
            Self::exclusive,
        ))
    }

    /// Positioned read executed in a kernel worker (`IOSQE_ASYNC`), for cold
    /// buffered file I/O.
    pub(crate) fn read_at_offloaded<'a, T: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<T, L>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            file,
            buffer,
            lease,
            scope,
            Data::Read(offset),
            true,
            None,
            Self::exclusive,
        ))
    }

    /// Write at an explicit offset, returning short progress without replaying bytes.
    pub fn write_at<'a, T: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<T, L>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            file,
            buffer,
            lease,
            scope,
            Data::Write(offset),
            false,
            None,
            |buffer| Self::shared(buffer),
        ))
    }

    /// Positioned write executed in a kernel worker (`IOSQE_ASYNC`).
    pub(crate) fn write_at_offloaded<'a, T: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<T, L>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            file,
            buffer,
            lease,
            scope,
            Data::Write(offset),
            true,
            None,
            |buffer| Self::shared(buffer),
        ))
    }

    /// Write `header` then `body` contiguously at an explicit offset with one
    /// vectored write, without copying either into a staging buffer.
    ///
    /// Both owners and the lease are retained until every completion fence.
    /// Short progress is returned as is and may end inside either segment;
    /// zero-progress `EINTR`/`EAGAIN` is retried like [`write_at`](Self::write_at).
    /// A segment or total length above `u32::MAX`, or an offset above
    /// `i64::MAX`, fails with [`Error::InvalidInput`].
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
        Box::pin(self.vectored(file, offset, header, body, lease, scope, false))
    }

    /// [`write_vectored_at`](Self::write_vectored_at) executed in a kernel
    /// worker (`IOSQE_ASYNC`), for cold buffered file I/O.
    pub fn write_vectored_at_offloaded<'a, H: IoBuffer, T: SharedWriteBuffer, L: 'static>(
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
        Box::pin(self.vectored(file, offset, header, body, lease, scope, true))
    }

    /// One vectored positioned write with the data retry policy.
    #[allow(clippy::too_many_arguments)]
    async fn vectored<H: IoBuffer, T: SharedWriteBuffer, L: 'static>(
        &self,
        fd: Rc<Descriptor>,
        offset: u64,
        mut header: H,
        mut body: T,
        mut lease: L,
        scope: &S,
        offload: bool,
    ) -> Result<VectoredCompletion<H, T, L>, S::Error>
    where
        S::Error: From<H::Error> + From<T::Error>,
    {
        if offset > i64::MAX as u64 {
            return Err(Error::InvalidInput.into());
        }
        let op_scope = OpScope::wrap(scope);
        for attempt in 0..=DATA_RETRIES {
            scope.check()?;
            let (header_ptr, header_len) = Self::shared(&mut header)?;
            let body_bytes = body.write_bytes()?;
            let (body_ptr, body_len) = (body_bytes.as_ptr().cast_mut(), body_bytes.len());
            let total = header_len
                .checked_add(body_len)
                .filter(|total| u32::try_from(*total).is_ok());
            if total.is_none() {
                return Err(Error::InvalidInput.into());
            }
            let segments = (
                Shared(Region::new(header, header_ptr, header_len)?),
                Shared(Region::new(body, body_ptr, body_len)?),
            );
            let bytes = Self::bookkeeping::<L, (Shared<H>, Shared<T>)>()
                + 2 * std::mem::size_of::<libc::iovec>();
            let (core, held) = self.admit(scope, None, SubmissionKind::OtherIo, bytes, lease)?;
            let op = core.writev_at(&fd, offset, segments, op_scope).lease(held);
            let (BufResult { result, buf }, held) = if offload {
                op.offload().await
            } else {
                op.await
            };
            header = buf.0.0.owner;
            body = buf.1.0.owner;
            lease = held.into_user();
            let failure = match result {
                Ok(bytes) => {
                    return Ok(VectoredCompletion {
                        header,
                        body,
                        bytes,
                        lease,
                    });
                }
                Err(failure) => failure,
            };
            let errno = failure.errno();
            if attempt == DATA_RETRIES || !matches!(errno, Some(libc::EINTR | libc::EAGAIN)) {
                return Err(failure.into_scope());
            }
            scope.check()?;
            if errno == Some(libc::EAGAIN) {
                // Keep the owners and lease in the readiness operation so that
                // abandonment retains them until its fence too.
                let bytes = Self::bookkeeping::<((H, T), L), ()>();
                let (core, held) = self.admit(
                    scope,
                    None,
                    SubmissionKind::Readiness,
                    bytes,
                    ((header, body), lease),
                )?;
                let (result, held) = core
                    .readiness(&fd, Data::Write(offset).interest(), op_scope)
                    .lease(held)
                    .await;
                ((header, body), lease) = held.into_user();
                result.map_err(Failure::into_scope)?;
            }
        }
        unreachable!("bounded retries return on the final attempt")
    }

    /// Receive into owned storage while retaining the caller's lease through fencing.
    pub fn recv<'a, T: IoBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<T, L>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            fd,
            buffer,
            lease,
            scope,
            Data::Recv,
            false,
            None,
            Self::exclusive,
        ))
    }

    /// Receive using prepaid capacity held until the fenced reply is consumed or dropped.
    pub fn recv_reserved<'a, T: IoBuffer>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: T,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, Completion<T>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            fd,
            buffer,
            (),
            scope,
            Data::Recv,
            false,
            Some(capacity),
            Self::exclusive,
        ))
    }

    /// Send using prepaid capacity held until the fenced reply is consumed or dropped.
    pub fn send_reserved<'a, T: IoBuffer>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: T,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, Completion<T>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            fd,
            buffer,
            (),
            scope,
            Data::Send,
            false,
            Some(capacity),
            |buffer| Self::shared(buffer),
        ))
    }

    /// Send owned immutable storage, retaining its lease and suppressing SIGPIPE.
    pub fn send<'a, T: SendBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<T, L>, S::Error>
    where
        S::Error: From<T::Error>,
    {
        Box::pin(self.data(
            fd,
            buffer,
            lease,
            scope,
            Data::Send,
            false,
            None,
            Self::shared,
        ))
    }

    /// Wait for a supported readiness mask while retaining the descriptor owner.
    pub fn readiness<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        interest: u32,
        scope: &'a S,
    ) -> Operation<'a, u32, S::Error> {
        self.readiness_capacity(fd, interest, (), None, scope)
    }

    /// Retain progress admission alongside the descriptor until all completion fences.
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
            let supported =
                (libc::POLLIN | libc::POLLOUT | libc::POLLRDHUP | libc::POLLHUP | libc::POLLERR)
                    as u32;
            if interest == 0 || interest & !supported != 0 {
                return Err(Error::InvalidInput.into());
            }
            let bytes = Self::bookkeeping::<L, ()>();
            let (core, held) =
                self.admit(scope, capacity, SubmissionKind::Readiness, bytes, lease)?;
            let (result, held) = core
                .readiness(&fd, interest, OpScope::wrap(scope))
                .lease(held)
                .await;
            drop(held);
            result.map_err(Failure::into_scope)
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

    /// Accept with optional prepaid capacity. An accepted descriptor that is
    /// never collected is closed after the fence.
    pub fn accept_reserved<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        capacity: Option<Rc<SubmissionCapacity>>,
        scope: &'a S,
    ) -> Operation<'a, Descriptor, S::Error> {
        Box::pin(async move {
            let bytes = Self::bookkeeping::<(), uring::SockAddr>();
            let (core, held) = self.admit(scope, capacity, SubmissionKind::OtherIo, bytes, ())?;
            let (result, held) = core.accept(&fd, OpScope::wrap(scope)).lease(held).await;
            drop(held);
            let (accepted, _peer) = result.map_err(Failure::into_scope)?;
            Rc::try_unwrap(accepted).map_err(|_| Error::Io.into())
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

    /// Transfer the complete connection/admission owner before submission and
    /// return it only after the original and any cancellation completions are
    /// fenced. A dropped connect future cannot return its slot prematurely.
    pub fn connect_with_lease<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        address: SocketAddress,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, L, S::Error> {
        self.connect_with_observation(fd, address, lease, None, scope)
    }

    /// Preserve the connect errno locally for attribution, without changing
    /// boundary errors.
    pub fn connect_with_observation<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        address: SocketAddress,
        lease: L,
        errno: Option<Rc<Cell<Option<i32>>>>,
        scope: &'a S,
    ) -> Operation<'a, L, S::Error> {
        Box::pin(async move {
            // The old reactor rejected empty (unnamed) Unix paths.
            if address
                .as_unix()
                .is_some_and(|path| path.as_os_str().is_empty())
            {
                return Err(Error::InvalidInput.into());
            }
            uring::SockAddr::encode(&address).map_err(|_| Error::InvalidInput)?;
            let bytes = Self::bookkeeping::<L, uring::SockAddr>();
            let (core, held) = self.admit(scope, None, SubmissionKind::OtherIo, bytes, lease)?;
            let (result, held) = core
                .connect(&fd, &address, OpScope::wrap(scope))
                .lease(held)
                .await;
            let lease = held.user;
            match result {
                Ok(()) => Ok(lease),
                Err(failure) => {
                    if let (Some(cell), Some(value)) = (&errno, failure.errno()) {
                        cell.set(Some(value));
                    }
                    Err(failure.into_scope())
                }
            }
        })
    }

    /// Process at most `budget` completions, then scan for canceled scopes and
    /// submit queued work. Returns completions consumed, including cancel
    /// completions.
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        let _environment = self.environment.enter();
        if budget == 0 {
            return Ok(0);
        }
        match self.core.get() {
            Some(core) => core.poll(budget).map_err(Error::from),
            None => Ok(0),
        }
    }

    /// Sleep until a completion or external wake, bounded by `duration` and a
    /// 10 ms fallback. Submits queued work first.
    pub fn wait(&self, duration: Duration) -> Result<()> {
        match self.core.get() {
            Some(core) => core.wait(duration).map_err(Error::from),
            None => {
                // Nothing can complete; behave like the old empty ppoll.
                let duration = duration.min(MAX_WAIT);
                if !duration.is_zero() && !self.environment.is_simulated() && !self.is_simulated() {
                    std::thread::sleep(duration);
                }
                Ok(())
            }
        }
    }

    /// Close admission and cancel outstanding work. The worker must keep
    /// driving [`Self::poll_budgeted`] while awaiting this fence. Admission
    /// closes when the returned future is first polled.
    pub fn drain(&self) -> Operation<'_, (), S::Error> {
        Box::pin(async move {
            self.stopped.set(true);
            match self.core.get() {
                Some(core) => core.drain().await.map_err(|e| Error::from(e).into()),
                None => Ok(()),
            }
        })
    }

    /// Close admission and drive cancellation for at most `timeout` (host time).
    /// An error, including `DeadlineExceeded`, is never a fence: owners stay
    /// with the reactor and this may be retried.
    pub fn shutdown(&self, timeout: Duration) -> Result<()> {
        self.stopped.set(true);
        match self.core.get() {
            Some(core) => core.shutdown(timeout).map_err(Error::from),
            None => Ok(()),
        }
    }

    /// Cancel every current operation whose scope matches before awaiting
    /// their fence. Work submitted later is not covered. The snapshot is
    /// taken when the returned future is first polled.
    pub fn fence_matching<'a>(
        &'a self,
        matches: impl Fn(&S) -> bool + 'a,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            match self.core.get() {
                Some(core) => core
                    .fence_matching(|scope: &OpScope<S>| matches(&scope.0))
                    .await
                    .map_err(|e| Error::from(e).into()),
                None => Ok(()),
            }
        })
    }
}

impl<S: Scope, B: Budget> Drop for Reactor<S, B> {
    /// Attempt a bounded fence. If it fails the library leaks the backend and
    /// unfenced owners; the ring's charge is leaked with them.
    fn drop(&mut self) {
        let Some(core) = self.core.take() else {
            return;
        };
        if core.in_flight() != 0 && !std::thread::panicking() {
            let _environment = self.environment.enter();
            let _ =
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| core.shutdown(DROP_WAIT)));
        }
        if core.in_flight() != 0 {
            std::mem::forget(self.ring_charge.get_mut().take());
        }
        drop(core);
    }
}

/// Data operation and, for positioned file I/O, its explicit byte offset.
#[derive(Clone, Copy)]
enum Data {
    Read(u64),
    Write(u64),
    Recv,
    Send,
}

impl Data {
    fn interest(self) -> u32 {
        (match self {
            Self::Read(_) | Self::Recv => libc::POLLIN,
            Self::Write(_) | Self::Send => libc::POLLOUT,
        }) as u32
    }
}

fn submission_kind(kind: uring::backend::OpKind) -> SubmissionKind {
    match kind {
        uring::backend::OpKind::PollAdd => SubmissionKind::Readiness,
        uring::backend::OpKind::Timeout => SubmissionKind::Timeout,
        _ => SubmissionKind::OtherIo,
    }
}
