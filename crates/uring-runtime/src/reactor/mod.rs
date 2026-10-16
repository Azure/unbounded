//! The reactor: a single-threaded owner of in-flight operations.
//!
//! A [`Reactor`] submits typed operations ([`read_at`](Reactor::read_at),
//! [`recv`](Reactor::recv), [`accept`](Reactor::accept),
//! [`sleep_until`](Reactor::sleep_until), [`open_at2`](Reactor::open_at2),
//! ...) to a [`Backend`] and resolves their futures as completions arrive.
//! It never runs on its own: the application calls [`Reactor::poll`] to reap
//! completions and [`Reactor::wait`] to block, typically from the same loop
//! that polls its tasks.
//!
//! ```
//! # fn main() -> uring_runtime::Result<()> {
//! use std::os::fd::OwnedFd;
//! use std::pin::pin;
//! use std::task::{Context, Poll, Waker};
//! use std::time::Duration;
//! use uring_runtime::reactor::{Reactor, ReactorConfig};
//! use uring_runtime::uring::Uring;
//! use uring_runtime::{Fd, Unscoped};
//!
//! # if !Uring::is_available() { return Ok(()); }
//! let reactor = Reactor::new(Uring::new(), ReactorConfig::default())?;
//! let file: Fd = OwnedFd::from(std::fs::File::open("/proc/self/stat")?).into();
//!
//! // Drive one operation to completion with a no-op waker.
//! let mut read = pin!(reactor.read_at(&file, 0, Vec::with_capacity(64), &Unscoped));
//! let mut cx = Context::from_waker(Waker::noop());
//! let outcome = loop {
//!     if let Poll::Ready(outcome) = read.as_mut().poll(&mut cx) {
//!         break outcome;
//!     }
//!     reactor.poll(64)?;
//!     reactor.wait(Duration::from_millis(10))?;
//! };
//! let n = outcome.result?;
//! assert!(n > 0);
//! assert_eq!(outcome.buf.len(), n);
//! # Ok(())
//! # }
//! ```
//!
//! # Ownership model
//!
//! An operation is described by a value that owns every input: the buffer,
//! a clone of the descriptor, the encoded address, the path, the
//! `open_how`, the `statx` output, the timeout's timespec and the optional
//! [lease](Operation::lease). On its first poll, the [`Operation`] future
//! moves that value, with the scope, into a heap node and only then derives
//! the raw [`Op`](crate::backend::Op) description from it, so every pointer
//! the backend sees points into memory with a fixed address. The node is
//! referenced through raw pointers only (never moved or reborrowed as a
//! `Box`) and is freed exactly once, after the operation is *fenced*:
//!
//! - by the future, which returns every input to the caller with the result
//!   (see [`BufResult`](crate::BufResult)), or
//! - by the reactor, if the future was dropped first.
//!
//! If neither can happen (the reactor is dropped with operations the
//! backend still references), the node is leaked, never freed, and counted
//! by [`leaked_operations`].
//!
//! Descriptors produced by the kernel ([`accept`](Reactor::accept),
//! [`open_at2`](Reactor::open_at2)) are adopted into the node as soon as the
//! completion arrives, through [`Backend::adopt_fd`], so they are closed
//! rather than leaked when nobody collects them.
//!
//! # Fences
//!
//! Each operation is tagged with a `user_data` key from a generation-tagged
//! slab. Cancellation pushes an asynchronous cancel request with its own
//! completion. An operation is fenced when its original completion has
//! arrived and, if a cancel request was pushed, the cancel request's
//! completion has too, in either order:
//!
//! ```text
//!   InFlight(no cancel) ---- original ----------------------------> Done
//!   InFlight(queued)    ---- original (request never pushed) ------> Done
//!   InFlight(queued)    ---- request pushed ---> InFlight(pushed)
//!   InFlight(pushed)    ---- original ---> AwaitingCancel ---- cancel ---> Done
//!   InFlight(pushed)    ---- cancel ----> InFlight(completed) -- original --> Done
//! ```
//!
//! Any other completion (unknown key, stale generation, duplicate) stops the
//! reactor (see [`Reactor::poll`]) without touching the entry.
//!
//! # Cancellation
//!
//! Each operation runs under a [`Scope`]. Cancellation is
//! requested when:
//!
//! - the scope's [`cancellation`](crate::Scope::cancellation) token is
//!   canceled (the reactor registers with the token, so this wakes a blocked
//!   [`wait`](Reactor::wait) through the [`RemoteWaker`] and is handled by
//!   the next [`poll`](Reactor::poll));
//! - the scope's [`deadline`](crate::Scope::deadline) hint passes and its
//!   [`check`](crate::Scope::check) then fails (`wait` never sleeps past the
//!   earliest hint);
//! - the bounded round-robin scan in [`poll`](Reactor::poll) finds that
//!   `check` fails (at most
//!   [`cancel_scan_budget`](ReactorConfig::cancel_scan_budget) slots per
//!   poll);
//! - the future is dropped;
//! - [`drain`](Reactor::drain), [`fence_matching`](Reactor::fence_matching)
//!   or [`shutdown`](Reactor::shutdown) selects the operation.
//!
//! Cancellation is a request: the kernel may still complete the operation.
//! By default the kernel's result wins: a successful completion is returned
//! as `Ok` even though cancellation was requested, so received bytes or an
//! accepted connection are never silently lost (call
//! [`Scope::check`] to learn whether the scope has
//! since failed). A failed completion of a canceled operation (typically
//! `ECANCELED`) is reported as the scope's error, or
//! [`ErrorKind::Canceled`] when the scope itself has not failed. With
//! [`ReactorConfig::cancel_wins`], every canceled operation reports the
//! cancellation error and drops the kernel's result (closing any accepted
//! descriptor).
//!
//! The reactor never retries: short reads and writes, `EAGAIN` and `EINTR`
//! are returned to the caller.
//!
//! # Admission
//!
//! Ordinary operations share [`ReactorConfig::max_in_flight`] slots. A
//! [`Reservation`] ([`Reactor::reserve`]) sets aside dedicated slots from
//! [`ReactorConfig::max_reserved`]; attach it with
//! [`Operation::reserved`]. A slot is held from submission until the fence.
//! The backend is sized for twice the total, so a cancel request always
//! fits. A rejected operation resolves to an error
//! ([`Overloaded`](ErrorKind::Overloaded),
//! [`Unavailable`](ErrorKind::Unavailable), ...) with every input returned,
//! and is reported to the
//! [rejection observer](Reactor::set_rejection_observer).
//!
//! # Re-entrancy and panics
//!
//! No user code (scope checks, wakers, destructors of buffers, leases and
//! scopes, the rejection observer) runs while the reactor's internal state
//! is borrowed, so all of it may call back into the reactor. The one
//! exception: [`poll`](Reactor::poll) and
//! [`fence_matching`](Reactor::fence_matching) fail with
//! [`ErrorKind::Unavailable`] when called from a scope check or a fence
//! predicate. Panics in wakers, scope checks and released inputs are caught
//! so that every other completion is still delivered, and the first one is
//! resumed when `poll` returns.

mod admission;
mod fence;
mod ops;
mod table;
#[cfg(test)]
mod tests;

use std::cell::{Cell, Ref, RefCell, RefMut};
use std::cmp::Reverse;
use std::collections::{BinaryHeap, VecDeque};
use std::fmt;
use std::mem::ManuallyDrop;
use std::panic::{AssertUnwindSafe, catch_unwind, resume_unwind};
use std::ptr::NonNull;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::task::{Wake, Waker};
use std::time::{Duration, Instant};

use crate::backend::{BackendConfig, CompletionEntry, PushError, RemoteWaker};
use crate::{Backend, Clock, Error, ErrorKind, Result, Scope};

pub use admission::{Rejection, RejectionReason, Reservation};
pub use fence::Fence;
pub use ops::{
    Accept, Fsync, Leased, MkDirAt, Nop, OpSpec, OpenAt2, Operation, ReadAt, Readiness, RenameAt,
    SleepUntil, SocketRecv, SocketSend, Statx, UnlinkAt, WriteAt,
};

use admission::{Partition, ReservationShared};
use ops::NodeImpl;
use table::{CANCEL_BIT, CancelFence, Entry, Node, Phase, Slab, Transition};

/// Operations leaked by dropped reactors, process wide.
static LEAKED: AtomicUsize = AtomicUsize::new(0);

/// The number of operations whose memory was deliberately leaked because a
/// reactor was dropped before the backend finished with them.
///
/// The count is process wide and only grows. A non-zero value means that a
/// reactor was dropped with work in flight and
/// [`drop_wait`](ReactorConfig::drop_wait) did not cover it.
pub fn leaked_operations() -> usize {
    LEAKED.load(Ordering::Relaxed)
}

/// Settings for a [`Reactor`].
///
/// Start from [`ReactorConfig::default`] and adjust with the `with_*`
/// methods (or by assigning the public fields).
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReactorConfig {
    /// Ordinary operations admitted at once. Default 256; must be positive.
    pub max_in_flight: usize,
    /// Total slots that [`Reactor::reserve`] may hand out. Default 0.
    pub max_reserved: usize,
    /// Upper bound on a single [`Reactor::wait`]. Default 10 ms.
    pub max_wait: Duration,
    /// How long dropping a reactor with work in flight may block to fence
    /// it before leaking it. Default zero: drop never blocks.
    pub drop_wait: Duration,
    /// Slots inspected per [`Reactor::poll`] by the round-robin scope scan.
    /// Default 64. Zero disables the scan (token and deadline hints still
    /// work).
    pub cancel_scan_budget: usize,
    /// Report cancellation even when the kernel completed a canceled
    /// operation successfully. Default `false`: the kernel's result wins.
    pub cancel_wins: bool,
}

impl Default for ReactorConfig {
    fn default() -> Self {
        Self {
            max_in_flight: 256,
            max_reserved: 0,
            max_wait: Duration::from_millis(10),
            drop_wait: Duration::ZERO,
            cancel_scan_budget: 64,
            cancel_wins: false,
        }
    }
}

impl ReactorConfig {
    /// Set [`max_in_flight`](ReactorConfig::max_in_flight).
    #[must_use]
    pub fn with_max_in_flight(mut self, max_in_flight: usize) -> Self {
        self.max_in_flight = max_in_flight;
        self
    }

    /// Set [`max_reserved`](ReactorConfig::max_reserved).
    #[must_use]
    pub fn with_max_reserved(mut self, max_reserved: usize) -> Self {
        self.max_reserved = max_reserved;
        self
    }

    /// Set [`max_wait`](ReactorConfig::max_wait).
    #[must_use]
    pub fn with_max_wait(mut self, max_wait: Duration) -> Self {
        self.max_wait = max_wait;
        self
    }

    /// Set [`drop_wait`](ReactorConfig::drop_wait).
    #[must_use]
    pub fn with_drop_wait(mut self, drop_wait: Duration) -> Self {
        self.drop_wait = drop_wait;
        self
    }

    /// Set [`cancel_scan_budget`](ReactorConfig::cancel_scan_budget).
    #[must_use]
    pub fn with_cancel_scan_budget(mut self, budget: usize) -> Self {
        self.cancel_scan_budget = budget;
        self
    }

    /// Set [`cancel_wins`](ReactorConfig::cancel_wins).
    #[must_use]
    pub fn with_cancel_wins(mut self, cancel_wins: bool) -> Self {
        self.cancel_wins = cancel_wins;
        self
    }

    /// The capacity requested from the backend: one original and one cancel
    /// request per admissible operation.
    fn backend_capacity(&self) -> Result<u32> {
        let invalid = |context| Error::new(ErrorKind::InvalidConfiguration).with_context(context);
        if self.max_in_flight == 0 {
            return Err(invalid("max_in_flight must be positive"));
        }
        self.max_in_flight
            .checked_add(self.max_reserved)
            .and_then(|total| total.checked_mul(2))
            .and_then(|total| u32::try_from(total).ok())
            .ok_or_else(|| invalid("reactor capacity exceeds u32::MAX"))
    }
}

/// Wakes the reactor when a registered cancel token fires.
struct Signal {
    pending: AtomicBool,
    remote: RemoteWaker,
}

impl Wake for Signal {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        if !self.pending.swap(true, Ordering::AcqRel) {
            self.remote.wake();
        }
    }
}

type Observer = Rc<dyn Fn(&Rejection<'_>)>;

/// A single-threaded io_uring style reactor over a [`Backend`].
///
/// See the [module documentation](self) for the ownership, fence,
/// cancellation and admission rules. A reactor is `!Send`: create it on the
/// thread that drives it.
pub struct Reactor<B: Backend> {
    state: RefCell<State<B>>,
    config: ReactorConfig,
    clock: B::Clock,
    /// Set while user code runs inside `poll` or `fence_matching`.
    busy: Cell<bool>,
    observer: RefCell<Option<Observer>>,
    /// Free slots of `max_reserved`.
    reserved_pool: Rc<Cell<usize>>,
    signal: Arc<Signal>,
    signal_waker: Waker,
}

struct State<B: Backend> {
    /// Leaked rather than dropped while operations are in flight.
    backend: ManuallyDrop<B>,
    table: Slab<Entry<B>>,
    /// Entries whose cancel request has not been pushed yet.
    cancel_queue: VecDeque<u64>,
    /// Deadline hints, earliest first. Stale keys are skipped.
    deadlines: BinaryHeap<Reverse<(Instant, u64)>>,
    fence_waiters: Vec<Waker>,
    /// Ordinary slots in use.
    ordinary: usize,
    /// Entries not yet fenced.
    in_flight: usize,
    /// Admission is stopped (drain or shutdown).
    closed: bool,
    /// A backend failure or protocol violation stopped the reactor.
    failure: Option<Error>,
    /// Next slot for the round-robin scope scan.
    cursor: usize,
}

/// Values released by `poll`, handled after the state borrow ends.
struct Released<B: Backend> {
    wakers: Vec<Waker>,
    entries: Vec<Entry<B>>,
}

impl<B: Backend> Default for Released<B> {
    fn default() -> Self {
        Self {
            wakers: Vec::new(),
            entries: Vec::new(),
        }
    }
}

/// Resets the busy flag when dropped.
struct Enter<'a>(&'a Cell<bool>);

impl Drop for Enter<'_> {
    fn drop(&mut self) {
        self.0.set(false);
    }
}

impl<B: Backend> Reactor<B> {
    /// Initialize `backend` and create a reactor around it.
    ///
    /// Fails with [`ErrorKind::InvalidConfiguration`] for a zero or
    /// overflowing capacity, and with the backend's error if
    /// [`Backend::init`] or [`Backend::remote_waker`] fails.
    pub fn new(mut backend: B, config: ReactorConfig) -> Result<Self> {
        let capacity = config.backend_capacity()?;
        backend.init(&BackendConfig::new(capacity))?;
        let remote = backend.remote_waker()?;
        let signal = Arc::new(Signal {
            pending: AtomicBool::new(false),
            remote,
        });
        Ok(Self {
            clock: backend.clock().clone(),
            state: RefCell::new(State {
                backend: ManuallyDrop::new(backend),
                table: Slab::default(),
                cancel_queue: VecDeque::new(),
                deadlines: BinaryHeap::new(),
                fence_waiters: Vec::new(),
                ordinary: 0,
                in_flight: 0,
                closed: false,
                failure: None,
                cursor: 0,
            }),
            reserved_pool: Rc::new(Cell::new(config.max_reserved)),
            config,
            busy: Cell::new(false),
            observer: RefCell::new(None),
            signal_waker: Waker::from(signal.clone()),
            signal,
        })
    }

    /// The configuration the reactor was created with.
    pub fn config(&self) -> &ReactorConfig {
        &self.config
    }

    /// Borrow the backend.
    ///
    /// # Panics
    ///
    /// The reactor panics if it needs its state while the borrow is held, so
    /// keep it short.
    pub fn backend(&self) -> Ref<'_, B> {
        Ref::map(self.state.borrow(), |state| &*state.backend)
    }

    /// Borrow the backend mutably, with the same caveat as
    /// [`backend`](Reactor::backend). Do not use this to push or pop
    /// operations behind the reactor's back.
    pub fn backend_mut(&self) -> RefMut<'_, B> {
        RefMut::map(self.state.borrow_mut(), |state| &mut *state.backend)
    }

    /// The backend's clock.
    pub fn clock(&self) -> &B::Clock {
        &self.clock
    }

    /// The backend's entropy source (borrowed like
    /// [`backend`](Reactor::backend)).
    pub fn entropy(&self) -> Ref<'_, B::Entropy> {
        Ref::map(self.state.borrow(), |state| state.backend.entropy())
    }

    /// A handle that wakes a blocked [`wait`](Reactor::wait) from any
    /// thread.
    pub fn remote_waker(&self) -> RemoteWaker {
        self.signal.remote.clone()
    }

    /// Operations that have not been fenced yet.
    pub fn in_flight(&self) -> usize {
        self.state.borrow().in_flight
    }

    /// Whether admission is stopped ([`drain`](Reactor::drain),
    /// [`shutdown`](Reactor::shutdown) or a failure).
    pub fn is_closed(&self) -> bool {
        let state = self.state.borrow();
        state.closed || state.failure.is_some()
    }

    /// The failure that stopped the reactor, if any.
    pub fn failure(&self) -> Option<Error> {
        self.state.borrow().failure.clone()
    }

    /// Set aside `slots` of [`max_reserved`](ReactorConfig::max_reserved)
    /// for operations submitted with [`Operation::reserved`].
    ///
    /// Fails with [`ErrorKind::InvalidInput`] for zero slots and
    /// [`ErrorKind::Overloaded`] if fewer than `slots` reserved slots are
    /// free.
    pub fn reserve(&self, slots: usize) -> Result<Reservation> {
        if slots == 0 {
            return Err(Error::new(ErrorKind::InvalidInput).with_context("empty reservation"));
        }
        let free = self.reserved_pool.get();
        if slots > free {
            return Err(
                Error::new(ErrorKind::Overloaded).with_context("reserved capacity exhausted")
            );
        }
        self.reserved_pool.set(free - slots);
        Ok(Reservation(Rc::new(ReservationShared {
            slots,
            used: Cell::new(0),
            pool: self.reserved_pool.clone(),
        })))
    }

    /// Reserved slots not handed out by [`reserve`](Reactor::reserve).
    pub fn reserved_available(&self) -> usize {
        self.reserved_pool.get()
    }

    /// Call `observer` for every rejected submission (see [`Rejection`]),
    /// replacing any previous observer.
    ///
    /// The observer runs after the reactor state is released, so it may
    /// call back into the reactor. It does not change the returned error.
    pub fn set_rejection_observer(&self, observer: impl Fn(&Rejection<'_>) + 'static) {
        let previous = self.observer.replace(Some(Rc::new(observer)));
        drop(previous);
    }

    /// Remove the rejection observer.
    pub fn clear_rejection_observer(&self) {
        let previous = self.observer.replace(None);
        drop(previous);
    }

    fn enter(&self) -> Result<Enter<'_>> {
        if self.busy.replace(true) {
            return Err(Error::new(ErrorKind::Unavailable)
                .with_context("reactor re-entered from a scope check or fence predicate"));
        }
        Ok(Enter(&self.busy))
    }

    /// Admit and queue an operation. On failure the node is returned with
    /// the error.
    #[allow(clippy::type_complexity)]
    fn submit_node<O: OpSpec<B>, S: Scope, L: 'static>(
        &self,
        mut node: NodeImpl<O, S, L>,
        reservation: Option<&Reservation>,
        waker: &Waker,
    ) -> std::result::Result<(u64, NonNull<NodeImpl<O, S, L>>), (NodeImpl<O, S, L>, S::Error)> {
        if let Err(error) = node.scope.check() {
            return Err((node, error));
        }
        if let Err(error) = node.data.prepare() {
            return Err((node, error.into()));
        }
        let token = node.scope.cancellation().cloned();
        let deadline = node.scope.deadline();
        let waker = waker.clone();

        let mut state = self.state.borrow_mut();
        let partition = match self.admit(&state, reservation) {
            Ok(partition) => partition,
            Err(reason) => {
                drop(state);
                return Err(self.reject(node, reason, reservation.is_some(), None));
            }
        };
        let Some(key) = state.table.vacant_key() else {
            drop(state);
            return Err(self.reject(node, RejectionReason::Capacity, reservation.is_some(), None));
        };

        let ptr = NonNull::from(Box::leak(Box::new(node)));
        // SAFETY: `ptr` is a fresh, exclusively owned allocation. The `Op`
        // borrows from it only for this call. On success the allocation is
        // owned by the table entry inserted below, and is freed only after
        // the entry is fenced (every completion for `key` popped), which
        // satisfies `Backend::push`'s contract. It is never moved.
        let pushed = unsafe { push_node(&mut *state.backend, key, ptr) };
        if let Err(error) = pushed {
            drop(state);
            // SAFETY: the push failed, so the backend holds no reference into
            // the node, and nothing else owns it.
            let node = *unsafe { Box::from_raw(ptr.as_ptr()) };
            let (reason, error) = match error {
                PushError::Full => (RejectionReason::QueueFull, None),
                PushError::Unsupported => (RejectionReason::Unsupported, None),
                PushError::Failed(error) => (RejectionReason::Backend, Some(error)),
            };
            return Err(self.reject(node, reason, reservation.is_some(), error));
        }

        match &partition {
            Partition::Ordinary => state.ordinary += 1,
            Partition::Reserved(shared) => shared.used.set(shared.used.get() + 1),
            Partition::Released => unreachable!("admission returns a live partition"),
        }
        // CancelToken registration runs only runtime code (it may wake the
        // signal immediately, which only touches atomics and the backend's
        // remote waker).
        let registration = token
            .as_ref()
            .map(|token| token.register(&self.signal_waker));
        let erased: NonNull<dyn Node<B>> = ptr;
        let inserted = state.table.insert(Entry {
            node: erased,
            phase: Phase::InFlight(CancelFence::None),
            cancel_requested: false,
            abandoned: false,
            waker: Some(waker),
            partition,
            token,
            registration,
        });
        debug_assert_eq!(inserted, key);
        state.in_flight += 1;
        if let Some(deadline) = deadline {
            state.push_deadline(deadline, key);
        }
        Ok((key, ptr))
    }

    fn admit(
        &self,
        state: &State<B>,
        reservation: Option<&Reservation>,
    ) -> std::result::Result<Partition, RejectionReason> {
        if state.failure.is_some() {
            return Err(RejectionReason::Failed);
        }
        if state.closed {
            return Err(RejectionReason::Closed);
        }
        match reservation {
            Some(reservation) => {
                let shared = &reservation.0;
                if !Rc::ptr_eq(&shared.pool, &self.reserved_pool) {
                    Err(RejectionReason::ForeignReservation)
                } else if shared.used.get() >= shared.slots {
                    Err(RejectionReason::ReservationExhausted)
                } else {
                    Ok(Partition::Reserved(shared.clone()))
                }
            }
            None if state.ordinary >= self.config.max_in_flight => Err(RejectionReason::Capacity),
            None => Ok(Partition::Ordinary),
        }
    }

    /// Build the rejection error and notify the observer.
    fn reject<O: OpSpec<B>, S: Scope, L>(
        &self,
        node: NodeImpl<O, S, L>,
        reason: RejectionReason,
        reserved: bool,
        error: Option<Error>,
    ) -> (NodeImpl<O, S, L>, S::Error) {
        let error = error.unwrap_or_else(|| {
            let kind = match reason {
                RejectionReason::Capacity
                | RejectionReason::ReservationExhausted
                | RejectionReason::QueueFull => ErrorKind::Overloaded,
                RejectionReason::ForeignReservation => ErrorKind::InvalidInput,
                RejectionReason::Closed | RejectionReason::Failed => ErrorKind::Unavailable,
                RejectionReason::Unsupported | RejectionReason::Backend => ErrorKind::Unsupported,
            };
            Error::new(kind).with_context(reason.as_str())
        });
        let observer = self.observer.borrow().clone();
        if let Some(observer) = observer {
            observer(&Rejection {
                reason,
                kind: O::KIND,
                reserved,
                scope: &node.scope,
                error: &error,
            });
        }
        (node, error.into())
    }

    /// Collect a fenced result, or remember `waker` and return `None`.
    fn collect(&self, key: u64, waker: &Waker) -> Option<(i32, bool)> {
        let mut state = self.state.borrow_mut();
        let entry = state
            .table
            .get_mut(key)
            .expect("an operation's entry lives as long as its future");
        if !matches!(entry.phase, Phase::Done(_))
            && entry.waker.as_ref().is_some_and(|w| w.will_wake(waker))
        {
            return None;
        }
        drop(state);
        // Clone outside the borrow: a waker's clone may re-enter the reactor.
        let waker = waker.clone();
        let mut state = self.state.borrow_mut();
        let entry = state.table.get_mut(key).expect("entry still present");
        if let Phase::Done(result) = entry.phase {
            let entry = state.table.remove(key).expect("entry still present");
            drop(state);
            let canceled = entry.cancel_requested;
            drop(entry);
            return Some((result, canceled));
        }
        let previous = entry.waker.replace(waker);
        drop(state);
        drop(previous);
        None
    }

    /// The future of `key` was dropped. Returns `true` if the operation was
    /// already fenced and its entry removed: the caller then frees the node.
    fn abandon(&self, key: u64) -> bool {
        let mut state = self.state.borrow_mut();
        let Some(entry) = state.table.get_mut(key) else {
            return false;
        };
        if matches!(entry.phase, Phase::Done(_)) {
            let entry = state.table.remove(key);
            drop(state);
            drop(entry);
            return true;
        }
        entry.abandoned = true;
        let waker = entry.waker.take();
        state.request_cancel(key);
        drop(state);
        drop(waker);
        false
    }

    /// Reap up to `max_completions` completions, request cancellation of
    /// operations whose scopes failed, push cancel requests and submit
    /// queued work. Returns the number of completions reaped (including
    /// cancel request completions).
    ///
    /// Futures are woken, and the inputs of dropped futures released, after
    /// the reactor's state is released; a panic from any of them is resumed
    /// after all the others ran.
    ///
    /// A completion that does not match an in-flight operation, or a
    /// backend submit failure, stops the reactor: it is reported once as an
    /// error here, admission is closed, everything in flight is canceled,
    /// and later polls keep reaping so that fences can still complete.
    pub fn poll(&self, max_completions: usize) -> Result<usize> {
        let guard = self.enter()?;
        let mut released = Released::default();
        let mut failure = None;
        let mut reaped = 0;
        {
            let mut state = self.state.borrow_mut();
            let mut fenced = false;
            while reaped < max_completions {
                let Some(completion) = state.backend.pop() else {
                    break;
                };
                reaped += 1;
                match state.complete(completion, &mut released) {
                    Ok(done) => fenced |= done,
                    Err(error) => {
                        state.fail(error.clone());
                        failure = Some(error);
                        break;
                    }
                }
            }
            if fenced || failure.is_some() {
                released.wakers.append(&mut state.fence_waiters);
            }
        }

        let mut panic = self.scan();
        {
            let mut state = self.state.borrow_mut();
            if let Err(error) = state.push_cancels().and_then(|()| state.submit()) {
                if failure.is_none() {
                    failure = Some(error.clone());
                }
                state.fail(error);
                released.wakers.append(&mut state.fence_waiters);
            }
        }
        drop(guard);

        for entry in released.entries {
            let result = catch_unwind(AssertUnwindSafe(|| {
                // SAFETY: the entry was fenced and removed from the table
                // after its future was dropped, so nothing else references
                // the node, which was allocated as a Box.
                drop(unsafe { Box::from_raw(entry.node.as_ptr()) });
                drop(entry);
            }));
            if let Err(payload) = result {
                panic.get_or_insert(payload);
            }
        }
        for waker in released.wakers {
            if let Err(payload) = catch_unwind(AssertUnwindSafe(|| waker.wake())) {
                panic.get_or_insert(payload);
            }
        }
        if let Some(payload) = panic {
            resume_unwind(payload);
        }
        match failure {
            Some(error) => Err(error),
            None => Ok(reaped),
        }
    }

    /// Find operations to cancel: canceled tokens, expired deadline hints
    /// and a bounded round-robin scan of scope checks. Returns the first
    /// panic from a scope check (the operation is then canceled).
    fn scan(&self) -> Option<Box<dyn std::any::Any + std::marker::Send>> {
        let now = self.clock.now();
        let mut candidates = Vec::new();
        {
            let mut state = self.state.borrow_mut();
            if self.signal.pending.swap(false, Ordering::AcqRel) {
                state.cancel_canceled_tokens();
            }
            state.expired_deadlines(now, &mut candidates);
            let budget = self.config.cancel_scan_budget;
            state.round_robin(budget, &mut candidates);
        }
        let mut panic = None;
        let mut failed = Vec::new();
        for (key, node) in candidates {
            // SAFETY: the node belongs to an in-flight entry. Only `poll`
            // frees in-flight nodes, and `poll` cannot run until the busy
            // guard held by our caller is released.
            let node = unsafe { node.as_ref() };
            match catch_unwind(AssertUnwindSafe(|| node.scope_failed())) {
                Ok(false) => {}
                Ok(true) => failed.push(key),
                Err(payload) => {
                    panic.get_or_insert(payload);
                    failed.push(key);
                }
            }
        }
        if !failed.is_empty() {
            let mut state = self.state.borrow_mut();
            for key in failed {
                state.request_cancel(key);
            }
        }
        panic
    }

    /// Block until a completion arrives, the [`RemoteWaker`] fires, or
    /// `timeout` elapses, after submitting queued work.
    ///
    /// The timeout is capped by [`max_wait`](ReactorConfig::max_wait) and
    /// by the earliest scope deadline hint, and is zero when cancellation
    /// work is pending. Spurious returns are possible.
    pub fn wait(&self, timeout: Duration) -> Result<()> {
        let now = self.clock.now();
        let mut state = self.state.borrow_mut();
        let mut timeout = timeout.min(self.config.max_wait);
        if !state.cancel_queue.is_empty() || self.signal.pending.load(Ordering::Acquire) {
            timeout = Duration::ZERO;
        }
        if let Some(Reverse((deadline, _))) = state.deadlines.peek() {
            timeout = timeout.min(deadline.saturating_duration_since(now));
        }
        let result = state.submit().and_then(|()| state.backend.wait(timeout));
        if let Err(error) = &result {
            state.fail(error.clone());
            let waiters = std::mem::take(&mut state.fence_waiters);
            drop(state);
            waiters.into_iter().for_each(Waker::wake);
        }
        result
    }

    /// Stop admission and cancel every operation in flight. The returned
    /// [`Fence`] resolves once all of them are fenced.
    ///
    /// The application must keep driving [`poll`](Reactor::poll) and
    /// [`wait`](Reactor::wait) while it awaits the fence. Admission stays
    /// closed.
    pub fn drain(&self) -> Fence<'_, B> {
        let waiters = {
            let mut state = self.state.borrow_mut();
            state.closed = true;
            state.cancel_all();
            std::mem::take(&mut state.fence_waiters)
        };
        waiters.into_iter().for_each(Waker::wake);
        Fence::all(self)
    }

    /// Cancel every in-flight operation whose scope is an `S` matching
    /// `predicate`, and return a [`Fence`] for exactly those operations.
    ///
    /// The selection is a snapshot taken by this call; operations submitted
    /// later are not covered. Operations with a scope of a different type
    /// never match. A typical use is canceling everything belonging to one
    /// request: `reactor.fence_matching(|scope: &RequestScope| scope.id == id)`.
    ///
    /// The predicate must not call [`poll`](Reactor::poll) or
    /// `fence_matching`; if this is called from a scope check or another
    /// predicate, the fence resolves to an [`ErrorKind::Unavailable`] error.
    pub fn fence_matching<S: Scope>(&self, mut predicate: impl FnMut(&S) -> bool) -> Fence<'_, B> {
        let guard = match self.enter() {
            Ok(guard) => guard,
            Err(error) => return Fence::failed(self, error),
        };
        let candidates: Vec<(u64, NonNull<dyn Node<B>>)> = self
            .state
            .borrow()
            .table
            .iter()
            .filter(|(_, entry)| entry.in_flight())
            .map(|(key, entry)| (key, entry.node))
            .collect();
        let mut matched = Vec::new();
        for (key, node) in candidates {
            // SAFETY: as in `scan`: in-flight nodes are only freed by `poll`,
            // which the busy guard excludes.
            let scope = unsafe { node.as_ref() }.scope_any().downcast_ref::<S>();
            if scope.is_some_and(&mut predicate) {
                matched.push(key);
            }
        }
        drop(guard);
        {
            let mut state = self.state.borrow_mut();
            for &key in &matched {
                state.request_cancel(key);
            }
        }
        Fence::keys(self, matched)
    }

    /// Stop admission, cancel everything in flight and drive the reactor
    /// until every operation is fenced or `timeout` (measured with
    /// [`std::time::Instant`], not the backend clock) elapses.
    ///
    /// Fails with [`ErrorKind::DeadlineExceeded`] on timeout. An error is
    /// never a fence: operations stay owned by the reactor, and `shutdown`
    /// may be retried.
    pub fn shutdown(&self, timeout: Duration) -> Result<()> {
        drop(self.drain());
        let started = Instant::now();
        loop {
            if self.in_flight() == 0 {
                return Ok(());
            }
            let elapsed = started.elapsed();
            if elapsed >= timeout {
                return Err(Error::new(ErrorKind::DeadlineExceeded)
                    .with_context("operations still in flight at shutdown"));
            }
            self.poll(usize::MAX)?;
            if self.in_flight() != 0 {
                self.wait(timeout - elapsed)?;
            }
        }
    }
}

/// Describe the node's operation and push it.
///
/// # Safety
///
/// `node` must be a valid, exclusively owned allocation that is not moved,
/// accessed mutably or freed until a completion for `key` has been popped
/// (if the push succeeds).
unsafe fn push_node<B: Backend, O: OpSpec<B>, S: Scope, L>(
    backend: &mut B,
    key: u64,
    node: NonNull<NodeImpl<O, S, L>>,
) -> std::result::Result<(), PushError> {
    // SAFETY: the caller guarantees exclusive access to the node; this
    // borrow ends before we return.
    let data = unsafe { &mut (*node.as_ptr()).data };
    let op = data.op();
    // SAFETY: forwarded from our caller.
    match unsafe { backend.push(key, &op) } {
        Err(PushError::Full) => {
            // Make room by submitting what is queued, then retry once.
            backend.submit().map_err(PushError::Failed)?;
            // SAFETY: as above.
            unsafe { backend.push(key, &op) }
        }
        result => result,
    }
}

impl<B: Backend> State<B> {
    /// Account one completion. Returns whether an entry was fenced.
    fn complete(
        &mut self,
        completion: CompletionEntry,
        released: &mut Released<B>,
    ) -> Result<bool> {
        let unexpected = || {
            Error::new(ErrorKind::Unavailable).with_context("completion for an unknown operation")
        };
        let cancel = completion.user_data & CANCEL_BIT != 0;
        let key = completion.user_data & !CANCEL_BIT;
        let entry = self.table.get_mut(key).ok_or_else(unexpected)?;
        let transition = entry
            .phase
            .complete(cancel, completion.result)
            .map_err(|_| unexpected())?;
        if !cancel && completion.result >= 0 {
            // SAFETY: the original completion arrived, so the backend no
            // longer accesses the operation's memory; the node is alive
            // (owned by this entry) and not otherwise borrowed.
            let node = unsafe { entry.node.as_mut() };
            node.deliver(&mut self.backend, completion.result);
        }
        if transition == Transition::Partial {
            return Ok(false);
        }
        self.in_flight -= 1;
        match std::mem::replace(&mut entry.partition, Partition::Released) {
            Partition::Ordinary => self.ordinary -= 1,
            Partition::Reserved(shared) => shared.used.set(shared.used.get() - 1),
            Partition::Released => {}
        }
        entry.registration = None;
        entry.token = None;
        if entry.abandoned {
            released
                .entries
                .push(self.table.remove(key).expect("entry present"));
        } else if let Some(waker) = entry.waker.take() {
            released.wakers.push(waker);
        }
        Ok(true)
    }

    /// Mark `key` canceled and queue its cancel request if needed.
    fn request_cancel(&mut self, key: u64) {
        let Some(entry) = self.table.get_mut(key) else {
            return;
        };
        if !entry.in_flight() {
            return;
        }
        entry.cancel_requested = true;
        if entry.phase == Phase::InFlight(CancelFence::None) {
            entry.phase = Phase::InFlight(CancelFence::Queued);
            self.cancel_queue.push_back(key);
        }
    }

    fn cancel_all(&mut self) {
        let keys: Vec<u64> = self
            .table
            .iter()
            .filter(|(_, entry)| entry.in_flight() && !entry.cancel_requested)
            .map(|(key, _)| key)
            .collect();
        for key in keys {
            self.request_cancel(key);
        }
    }

    fn fail(&mut self, error: Error) {
        self.failure.get_or_insert(error);
        self.cancel_all();
    }

    fn cancel_canceled_tokens(&mut self) {
        let keys: Vec<u64> = self
            .table
            .iter()
            .filter(|(_, entry)| {
                entry.in_flight()
                    && !entry.cancel_requested
                    && entry
                        .token
                        .as_ref()
                        .is_some_and(|token| token.is_canceled())
            })
            .map(|(key, _)| key)
            .collect();
        for key in keys {
            self.request_cancel(key);
        }
    }

    fn push_deadline(&mut self, deadline: Instant, key: u64) {
        // Keep the heap proportional to the table: drop stale hints.
        if self.deadlines.len() > 2 * self.table.len() + 64 {
            let table = &self.table;
            self.deadlines
                .retain(|Reverse((_, key))| table.get(*key).is_some_and(Entry::in_flight));
        }
        self.deadlines.push(Reverse((deadline, key)));
    }

    fn expired_deadlines(&mut self, now: Instant, out: &mut Vec<(u64, NonNull<dyn Node<B>>)>) {
        while let Some(&Reverse((deadline, key))) = self.deadlines.peek() {
            if deadline > now {
                break;
            }
            self.deadlines.pop();
            if let Some(entry) = self.table.get(key)
                && entry.in_flight()
                && !entry.cancel_requested
            {
                out.push((key, entry.node));
            }
        }
    }

    fn round_robin(&mut self, budget: usize, out: &mut Vec<(u64, NonNull<dyn Node<B>>)>) {
        let slots = self.table.slot_count();
        for _ in 0..budget.min(slots) {
            let index = self.cursor % slots;
            self.cursor = index + 1;
            if let Some((key, entry)) = self.table.at(index)
                && entry.in_flight()
                && !entry.cancel_requested
            {
                out.push((key, entry.node));
            }
        }
    }

    /// Push queued cancel requests, stopping when the queue is full.
    fn push_cancels(&mut self) -> Result<()> {
        while let Some(key) = self.cancel_queue.pop_front() {
            let Some(entry) = self.table.get(key) else {
                continue;
            };
            if entry.phase != Phase::InFlight(CancelFence::Queued) {
                continue;
            }
            let mut result = self.backend.cancel(key | CANCEL_BIT, key);
            if result == Err(PushError::Full) {
                self.backend.submit()?;
                result = self.backend.cancel(key | CANCEL_BIT, key);
            }
            let entry = self.table.get_mut(key).expect("entry present");
            match result {
                Ok(()) => entry.phase = Phase::InFlight(CancelFence::Pushed),
                Err(PushError::Full) => {
                    self.cancel_queue.push_front(key);
                    break;
                }
                // The backend cannot cancel this operation: wait for its
                // natural completion.
                Err(_) => entry.phase = Phase::InFlight(CancelFence::None),
            }
        }
        Ok(())
    }

    fn submit(&mut self) -> Result<()> {
        self.backend.submit().map(drop)
    }
}

impl<B: Backend> Drop for Reactor<B> {
    fn drop(&mut self) {
        let in_flight = self.state.get_mut().in_flight;
        if in_flight > 0 && !self.config.drop_wait.is_zero() && !std::thread::panicking() {
            let drop_wait = self.config.drop_wait;
            let _ = catch_unwind(AssertUnwindSafe(|| self.shutdown(drop_wait)));
        }
        let state = self.state.get_mut();
        if state.in_flight == 0 {
            // SAFETY: nothing is in flight, so the backend references no
            // operation memory; it is dropped exactly once, here.
            unsafe { ManuallyDrop::drop(&mut state.backend) };
        } else {
            // The backend (for io_uring, the ring) may still write into the
            // nodes of in-flight entries. Closing a ring is not a memory
            // fence, so keep the backend and those nodes forever. Nodes are
            // raw pointers, so dropping the table does not free them.
            LEAKED.fetch_add(state.in_flight, Ordering::Relaxed);
        }
    }
}

impl<B: Backend> fmt::Debug for Reactor<B> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let mut debug = f.debug_struct("Reactor");
        debug.field("config", &self.config);
        if let Ok(state) = self.state.try_borrow() {
            debug
                .field("in_flight", &state.in_flight)
                .field("closed", &state.closed)
                .field("failure", &state.failure);
        }
        debug.finish_non_exhaustive()
    }
}
