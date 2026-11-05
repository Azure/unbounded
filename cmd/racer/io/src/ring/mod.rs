//! The concrete worker-local reactor.
//!
//! Before submission the reactor owns the buffer, descriptor, and lease of an
//! operation in a preallocated slot, independently of the waiting future.
//! Dropping a future only abandons its result: the owners stay in the slot
//! until the original completion and any cancel completion have both arrived
//! (the two-fence model). Shutdown must fence kernel access before the slots
//! are freed; a reactor dropped with work in flight leaks its slots and ring
//! instead of freeing memory the kernel may still use.
//!
//! The reactor is driven explicitly by its lane: [`Reactor::reap`] processes
//! completions and expired deadlines, and [`Reactor::park`] submits and waits
//! for the next completion, cross-thread wake, or deadline. Data operations
//! retry zero-progress `EINTR`/`EAGAIN` at most eight times inside the reactor
//! (waiting for readiness on `EAGAIN`); positive partial progress is returned
//! as is and never replayed.

mod buffer;
pub(crate) mod fd;
#[doc(hidden)]
pub mod op;
pub mod ops;
pub(crate) mod owner;
pub mod ready_set;
pub(crate) mod uring;

pub use buffer::{Completion, IoBuffer, SendBuffer, SharedWriteBuffer, VectoredCompletion};
pub use fd::{Descriptor, SocketAddress};
pub use owner::INLINE_BYTES;
pub use uring::EnterStats;

pub(crate) use fd::encode_address;

use crate::{CancelToken, Deadline, Error, Result, WorkerCtx, sim::SimDriver};
use op::{Op, Scratch};
use owner::{InlineBytes, OwnerCell};
use std::{
    cell::{RefCell, RefMut},
    cmp::Reverse,
    collections::BinaryHeap,
    future::Future,
    os::fd::{AsRawFd, FromRawFd, OwnedFd, RawFd},
    pin::Pin,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    time::Instant,
};
use uring::UringDriver;

/// High bit of `user_data` marks cancel CQEs.
const CANCEL_BIT: u64 = 1 << 63;

/// `user_data` of the multishot poll on the lane eventfd.
const WAKE_TAG: u64 = u64::MAX;

/// `user_data` of the one-shot poll on the process stop eventfd.
const STOP_TAG: u64 = u64::MAX - 1;

/// Zero-progress `EINTR`/`EAGAIN` retries per data operation.
const DATA_RETRIES: u8 = 8;

/// Slots per lazily allocated slab chunk.
const CHUNK: usize = 16;

/// A tag carried by every operation, used to fence one class of work
/// (for example all file I/O of a request) without scanning scopes.
#[derive(Clone, Copy, Debug, Default, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct IoClass(pub u8);

/// Per-operation cancellation, deadline, and class.
#[derive(Clone, Debug, Default)]
pub struct OpCtx {
    /// Cancels the operation when canceled.
    pub cancel: Option<CancelToken>,

    /// Cancels the operation with [`Error::DeadlineExceeded`] when reached.
    pub deadline: Deadline,

    /// Class used by [`Reactor::fence_class`].
    pub class: IoClass,

    /// Draw from the reserved partition ([`Config::reserved_entries`]) so
    /// control traffic is never starved by data operations.
    pub reserved: bool,

    /// Issue positioned file reads and writes with `IOSQE_ASYNC` so cold
    /// buffered filesystem I/O runs on a kernel worker instead of blocking
    /// the lane inline. Device `O_DIRECT` and socket I/O leave this unset;
    /// filesystem metadata operations are always offloaded.
    pub offload: bool,
}

impl OpCtx {
    /// No cancellation, no deadline, class 0.
    pub fn none() -> Self {
        Self::default()
    }

    /// A context canceled by `token`.
    pub fn with_cancel(token: &CancelToken) -> Self {
        Self {
            cancel: Some(token.clone()),
            ..Self::default()
        }
    }

    /// Replace the deadline.
    pub fn deadline(mut self, deadline: Deadline) -> Self {
        self.deadline = deadline;
        self
    }

    /// Replace the class.
    pub fn class(mut self, class: IoClass) -> Self {
        self.class = class;
        self
    }

    /// Use the reserved partition.
    pub fn reserved(mut self) -> Self {
        self.reserved = true;
        self
    }

    /// Offload positioned file I/O (see [`OpCtx::offload`]).
    pub fn offload(mut self) -> Self {
        self.offload = true;
        self
    }

    /// The first reason this context forbids starting or delivering work.
    pub fn check(&self, now: Instant) -> Result<()> {
        if let Some(token) = &self.cancel {
            token.check()?;
        }
        self.deadline.check(now)
    }
}

/// Backend selected at construction.
#[derive(Clone, Debug)]
pub enum Driver {
    /// The Linux kernel through io_uring.
    Uring,
    /// A deterministic simulated operating system.
    Sim(crate::sim::SimOs),
}

impl Driver {
    /// The simulated OS this driver selects, or `None` for the kernel.
    pub fn sim_os(&self) -> Option<&crate::sim::SimOs> {
        match self {
            Self::Uring => None,
            Self::Sim(os) => Some(os),
        }
    }
}

/// The backend and worker context one worker role runs against, passed
/// explicitly to every owner that builds a reactor or a descriptor.
#[derive(Clone, Debug)]
pub struct Env {
    /// Kernel or simulated operating system.
    pub driver: Driver,

    /// Time and entropy.
    pub ctx: WorkerCtx,
}

impl Env {
    /// Production: the kernel with host time and entropy.
    pub fn host() -> Self {
        Self {
            driver: Driver::Uring,
            ctx: WorkerCtx::host(),
        }
    }

    /// A role with an explicit backend and context.
    pub fn new(driver: Driver, ctx: WorkerCtx) -> Self {
        Self { driver, ctx }
    }

    /// A role on the simulated OS `os` with context `ctx`.
    pub fn sim(os: &crate::sim::SimOs, ctx: WorkerCtx) -> Self {
        Self::new(Driver::Sim(os.clone()), ctx)
    }

    /// The simulated OS, or `None` for the kernel.
    pub fn sim_os(&self) -> Option<&crate::sim::SimOs> {
        self.driver.sim_os()
    }
}

/// Ring sizing.
#[derive(Clone, Copy, Debug)]
pub struct Config {
    /// Maximum operations in flight. The SQ holds twice this many entries so
    /// every operation can also have a cancel in flight; the CQ twice the SQ.
    pub queue_entries: u32,

    /// Slots usable only by operations with [`OpCtx::reserved`]; ordinary
    /// operations are limited to `queue_entries - reserved_entries`.
    pub reserved_entries: u32,
}

impl Config {
    /// `queue_entries` operations, none reserved.
    pub fn new(queue_entries: u32) -> Self {
        Self {
            queue_entries,
            reserved_entries: 0,
        }
    }
}

impl Default for Config {
    /// 256 operations in flight.
    fn default() -> Self {
        Self::new(256)
    }
}

/// Cross-thread wake endpoint of one lane: coalesced eventfd writes for the
/// kernel driver, a flag for the simulated one.
#[derive(Debug)]
pub struct WakeHandle {
    eventfd: Option<OwnedFd>,

    pending: AtomicBool,
}

impl WakeHandle {
    fn new(eventfd: Option<OwnedFd>) -> Self {
        Self {
            eventfd,
            pending: AtomicBool::new(false),
        }
    }

    /// Wake the lane. Coalesced: at most one eventfd write per lane turn.
    pub fn wake(&self) {
        if self.pending.swap(true, Ordering::AcqRel) {
            return;
        }
        let Some(fd) = &self.eventfd else {
            return;
        };
        let value = 1u64;
        loop {
            // SAFETY: eventfd consumes this initialized u64 synchronously.
            let written = unsafe { libc::write(fd.as_raw_fd(), (&value as *const u64).cast(), 8) };
            if written == 8 || std::io::Error::last_os_error().raw_os_error() != Some(libc::EINTR) {
                // EAGAIN means the counter is already readable.
                return;
            }
        }
    }

    /// Whether a wake is pending (not yet observed by the lane).
    pub fn is_pending(&self) -> bool {
        self.pending.load(Ordering::Acquire)
    }

    /// Consume the pending wake.
    fn clear(&self) -> bool {
        self.pending.swap(false, Ordering::AcqRel)
    }

    /// Drain the eventfd counter.
    fn drain(&self) {
        if let Some(fd) = &self.eventfd {
            let mut value = 0u64;
            // SAFETY: nonblocking read into a local u64.
            unsafe { libc::read(fd.as_raw_fd(), (&mut value as *mut u64).cast(), 8) };
        }
    }
}

/// Operation identity: slot index (low 32 bits) and generation.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct IoId(u64);

impl IoId {
    fn new(index: u32, generation: u32) -> Self {
        Self((u64::from(generation) << 32) | u64::from(index))
    }

    fn index(self) -> usize {
        (self.0 & 0xFFFF_FFFF) as usize
    }

    fn generation(self) -> u32 {
        ((self.0 >> 32) & 0x7FFF_FFFF) as u32
    }
}

/// Cancel accounting, independent of why cancellation was requested.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum CancelFence {
    NotSubmitted,
    Pending,
    Complete,
}

/// Two-fence completion accounting for one attempt.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Fence {
    /// The original CQE has not arrived.
    Pending(CancelFence),
    /// The original arrived; its cancel CQE is still outstanding.
    AwaitingCancel(i32),
    /// Every applicable CQE arrived.
    Retired,
}

impl Fence {
    fn needs_cancel(self) -> bool {
        self == Self::Pending(CancelFence::NotSubmitted)
    }

    /// Account one CQE, returning the original result once fully fenced.
    fn complete(&mut self, cancel: bool, result: i32) -> Result<Option<i32>> {
        match *self {
            Self::Pending(CancelFence::Pending) if cancel => {
                // Even ENOENT/EALREADY prove the cancel request finished.
                *self = Self::Pending(CancelFence::Complete);
                Ok(None)
            }
            Self::AwaitingCancel(original) if cancel => {
                *self = Self::Retired;
                Ok(Some(original))
            }
            Self::Pending(fence) if !cancel => {
                if fence == CancelFence::Pending {
                    *self = Self::AwaitingCancel(result);
                    Ok(None)
                } else {
                    *self = Self::Retired;
                    Ok(Some(result))
                }
            }
            _ => Err(Error::Io),
        }
    }
}

/// Erased re-preparation for retries.
type Prepare = unsafe fn(*mut InlineBytes, *mut Scratch) -> Result<Op>;

/// Erased [`sealed::OpOwner::discard`]; empties the owner cell and returns
/// the release to run once the state borrow is gone.
type Discard = unsafe fn(&mut OwnerCell, i32) -> Deferred;

/// An owner release deferred until the reactor state is not borrowed, so
/// owner destructors may re-enter the reactor.
type Deferred = Box<dyn FnOnce()>;

/// One preallocated operation slot.
struct Slot {
    generation: u32,

    live: bool,

    fence: Fence,

    /// Final result once delivered and not yet taken.
    done: Option<i32>,

    cancel: Option<Error>,

    abandoned: bool,

    waker: Option<Waker>,

    class: IoClass,

    deadline: Option<Instant>,

    returns_fd: bool,

    reserved: bool,

    retry_interest: u32,

    offload: bool,

    retry_fd: RawFd,

    attempts: u8,

    polling: bool,

    prepare: Option<Prepare>,

    discard: Option<Discard>,

    scratch: Scratch,

    owner: OwnerCell,
}

impl Slot {
    fn new() -> Self {
        Self {
            generation: 0,
            live: false,
            fence: Fence::Retired,
            done: None,
            cancel: None,
            abandoned: false,
            waker: None,
            class: IoClass(0),
            deadline: None,
            returns_fd: false,
            reserved: false,
            retry_interest: 0,
            offload: false,
            retry_fd: -1,
            attempts: 0,
            polling: false,
            prepare: None,
            discard: None,
            scratch: Scratch::new(),
            owner: OwnerCell::new(),
        }
    }
}

/// Chunked slot storage: addresses never move once a chunk is allocated.
struct Slab {
    chunks: Vec<Box<[Slot]>>,

    free: Vec<u32>,

    capacity: usize,

    live: usize,
}

impl Slab {
    fn new(capacity: usize) -> Self {
        Self {
            chunks: Vec::new(),
            free: Vec::new(),
            capacity,
            live: 0,
        }
    }

    fn get(&mut self, index: usize) -> Option<&mut Slot> {
        self.chunks
            .get_mut(index / CHUNK)
            .map(|chunk| &mut chunk[index % CHUNK])
    }

    /// The live slot named by `id`, checking its generation.
    fn lookup(&mut self, id: IoId) -> Option<&mut Slot> {
        self.get(id.index())
            .filter(|slot| slot.live && slot.generation == id.generation())
    }

    /// Allocate a free slot, bumping its generation.
    fn alloc(&mut self) -> Option<IoId> {
        if self.live >= self.capacity {
            return None;
        }
        let index = match self.free.pop() {
            Some(index) => index,
            None => {
                let index = self.chunks.len() * CHUNK;
                if index >= self.capacity {
                    return None;
                }
                let chunk: Box<[Slot]> = (0..CHUNK).map(|_| Slot::new()).collect();
                self.chunks.push(chunk);
                self.free
                    .extend((index as u32 + 1..(index + CHUNK) as u32).rev());
                index as u32
            }
        };
        let slot = self.get(index as usize).expect("allocated slot");
        slot.generation = (slot.generation + 1) & 0x7FFF_FFFF;
        slot.live = true;
        slot.fence = Fence::Pending(CancelFence::NotSubmitted);
        slot.done = None;
        slot.cancel = None;
        slot.abandoned = false;
        slot.waker = None;
        slot.attempts = 0;
        slot.polling = false;
        let generation = slot.generation;
        self.live += 1;
        Some(IoId::new(index, generation))
    }

    /// Return a slot whose owner was already taken or cleared.
    fn release(&mut self, id: IoId) {
        let slot = self.get(id.index()).expect("released slot");
        debug_assert!(!slot.owner.is_occupied());
        slot.live = false;
        slot.prepare = None;
        slot.discard = None;
        slot.waker = None;
        self.live -= 1;
        self.free.push(id.index() as u32);
    }
}

/// One backend per reactor, so the size difference is irrelevant.
#[allow(clippy::large_enum_variant)]
enum Backend {
    Uring(UringDriver),
    Sim(SimDriver),
}

/// Reactor counters.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Stats {
    /// Kernel entry counters (zero for the simulated driver).
    pub enters: EnterStats,

    /// Original CQEs that triggered an internal retry.
    pub retries: u64,

    /// Submissions rejected for lack of slots or SQ space.
    pub rejected: u64,

    /// Cancels requested because a deadline expired.
    pub expired: u64,
}

struct State {
    backend: Backend,

    slab: Slab,

    /// In-flight operation deadlines (lazily invalidated).
    deadlines: BinaryHeap<Reverse<(Instant, IoId)>>,

    timers: Timers,

    /// Cancels that did not fit in the SQ; retried every turn.
    cancel_backlog: Vec<IoId>,

    /// Live operations per class.
    classes: Box<[u32; 256]>,

    fence_waiters: Vec<(Option<IoClass>, Waker)>,

    /// Waiters of [`Reactor::fence_ids`], keyed by one unfenced operation.
    id_waiters: Vec<(IoId, Waker)>,

    stopped: bool,

    /// Operations whose kernel fences have not all arrived.
    unfenced: usize,

    wake_armed: bool,

    retries: u64,

    rejected: u64,

    expired: u64,

    /// Slot limits: ordinary and reserved partitions.
    ordinary_limit: usize,

    reserved_limit: usize,

    ordinary_live: usize,

    reserved_live: usize,

    /// Reused batch of wakers notified outside the state borrow.
    wakers: Vec<Waker>,

    /// Owner releases run outside the state borrow.
    graveyard: Vec<Deferred>,
}

impl State {
    /// Return a delivered or abandoned slot to its partition.
    fn release_slot(&mut self, id: IoId) {
        let reserved = self.slab.lookup(id).is_some_and(|slot| slot.reserved);
        if reserved {
            self.reserved_live -= 1;
        } else {
            self.ordinary_live -= 1;
        }
        self.slab.release(id);
    }
}

struct Inner {
    ctx: WorkerCtx,

    wake: Arc<WakeHandle>,

    state: std::mem::ManuallyDrop<RefCell<State>>,
}

/// The worker-local reactor. Cloning shares the same reactor.
#[derive(Clone)]
pub struct Reactor(Rc<Inner>);

impl std::fmt::Debug for Reactor {
    /// Show only in-flight counts.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Reactor")
            .field("in_flight", &self.in_flight())
            .finish()
    }
}

impl Reactor {
    /// Build a reactor over `driver`.
    ///
    /// Guard: [`Driver::Uring`] rejects a simulated `ctx` with
    /// [`Error::InvalidConfiguration`], so seeded entropy and virtual time can
    /// never back a kernel reactor.
    pub fn new(driver: Driver, config: Config, ctx: WorkerCtx) -> Result<Self> {
        let entries = config.queue_entries;
        if entries == 0 || entries > (1 << 20) || config.reserved_entries > entries {
            return Err(Error::InvalidConfiguration);
        }
        let (backend, wake) = match driver {
            Driver::Uring => {
                if ctx.is_simulated() {
                    return Err(Error::InvalidConfiguration);
                }
                let sq = entries.checked_mul(2).ok_or(Error::InvalidConfiguration)?;
                let ring = UringDriver::new(sq)?;
                // SAFETY: eventfd returns a fresh descriptor or fails.
                let fd = unsafe { libc::eventfd(0, libc::EFD_CLOEXEC | libc::EFD_NONBLOCK) };
                if fd < 0 {
                    return Err(Error::last_os_error());
                }
                // SAFETY: the descriptor was just created and is uniquely owned.
                let fd = unsafe { OwnedFd::from_raw_fd(fd) };
                (Backend::Uring(ring), WakeHandle::new(Some(fd)))
            }
            Driver::Sim(os) => (
                Backend::Sim(SimDriver::new(os, entries as usize * 2, ctx.clock.clone())),
                WakeHandle::new(None),
            ),
        };
        let reactor = Self(Rc::new(Inner {
            ctx,
            wake: Arc::new(wake),
            state: std::mem::ManuallyDrop::new(RefCell::new(State {
                backend,
                slab: Slab::new(entries as usize),
                deadlines: BinaryHeap::new(),
                timers: Timers::default(),
                cancel_backlog: Vec::new(),
                classes: Box::new([0; 256]),
                fence_waiters: Vec::new(),
                id_waiters: Vec::new(),
                stopped: false,
                unfenced: 0,
                wake_armed: false,
                retries: 0,
                rejected: 0,
                expired: 0,
                ordinary_limit: (entries - config.reserved_entries) as usize,
                reserved_limit: config.reserved_entries as usize,
                ordinary_live: 0,
                reserved_live: 0,
                wakers: Vec::new(),
                graveyard: Vec::new(),
            })),
        }));
        reactor.arm_wake()?;
        reactor.arm_stop()?;
        Ok(reactor)
    }

    /// Poll the process stop eventfd once, so a raised stop wakes this lane.
    /// One-shot: a lane that observed the stop never needs another wake.
    fn arm_stop(&self) -> Result<()> {
        let Some(fd) = crate::signal::existing_stop_fd() else {
            return Ok(());
        };
        let op = Op::Poll {
            fd,
            mask: libc::POLLIN as u32,
            multi: false,
        };
        if let Backend::Uring(ring) = &mut self.state().backend {
            ring.push(STOP_TAG, &op, false)
                .map_err(|_| Error::Overloaded)?;
        }
        Ok(())
    }

    /// This reactor's worker context.
    pub fn ctx(&self) -> &WorkerCtx {
        &self.0.ctx
    }

    /// Current monotonic time from the worker clock.
    pub fn now(&self) -> Instant {
        self.0.ctx.clock.now()
    }

    /// The lane's cross-thread wake endpoint.
    pub fn wake_handle(&self) -> Arc<WakeHandle> {
        self.0.wake.clone()
    }

    /// Whether this reactor uses the simulated driver.
    pub fn is_sim(&self) -> bool {
        matches!(self.state().backend, Backend::Sim(_))
    }

    /// The simulated OS, if any.
    pub fn sim_os(&self) -> Option<crate::sim::SimOs> {
        match &self.state().backend {
            Backend::Sim(driver) => Some(driver.os().clone()),
            Backend::Uring(_) => None,
        }
    }

    /// Operations awaiting their original or cancel CQE.
    pub fn in_flight(&self) -> usize {
        self.state().unfenced
    }

    /// Counters.
    pub fn stats(&self) -> Stats {
        let state = self.state();
        Stats {
            enters: match &state.backend {
                Backend::Uring(ring) => ring.stats,
                Backend::Sim(_) => EnterStats::default(),
            },
            retries: state.retries,
            rejected: state.rejected,
            expired: state.expired,
        }
    }

    /// Kernel setup flags in use (zero for the simulated driver).
    pub fn setup_flags(&self) -> u32 {
        match &self.state().backend {
            Backend::Uring(ring) => ring.setup_flags(),
            Backend::Sim(_) => 0,
        }
    }

    fn state(&self) -> RefMut<'_, State> {
        self.0.state.borrow_mut()
    }

    /// Arm the multishot poll on the lane eventfd.
    fn arm_wake(&self) -> Result<()> {
        let mut state = self.state();
        let Some(fd) = &self.0.wake.eventfd else {
            return Ok(());
        };
        let op = Op::Poll {
            fd: fd.as_raw_fd(),
            mask: libc::POLLIN as u32,
            multi: true,
        };
        if let Backend::Uring(ring) = &mut state.backend {
            ring.push(WAKE_TAG, &op, false)
                .map_err(|_| Error::Overloaded)?;
            state.wake_armed = true;
        }
        Ok(())
    }

    /// Adopt a descriptor number produced by this reactor's backend.
    pub(crate) fn adopt(&self, raw: RawFd) -> Descriptor {
        match &self.state().backend {
            // SAFETY: the kernel transferred ownership of this descriptor.
            Backend::Uring(_) => unsafe { Descriptor::from_raw_fd(raw) },
            Backend::Sim(driver) => Descriptor::sim(driver.os().clone(), raw),
        }
    }

    /// Close an orphaned descriptor result.
    fn close_raw(state: &mut State, raw: RawFd) {
        match &mut state.backend {
            // SAFETY: an unclaimed accepted/opened descriptor is uniquely ours.
            Backend::Uring(_) => unsafe {
                libc::close(raw);
            },
            Backend::Sim(driver) => driver.os().close(raw),
        }
    }

    /// Submit an operation owning `owner`.
    pub(crate) fn submit<O: sealed::OpOwner>(&self, owner: O, ctx: &OpCtx) -> Result<IoId> {
        ctx.check(self.now())?;
        let mut state = self.state();
        if state.stopped {
            return Err(Error::Unavailable);
        }
        let full = if ctx.reserved {
            state.reserved_live >= state.reserved_limit
        } else {
            state.ordinary_live >= state.ordinary_limit
        };
        let Some(id) = (!full).then(|| state.slab.alloc()).flatten() else {
            state.rejected += 1;
            return Err(Error::Overloaded);
        };
        let slot = state.slab.lookup(id).expect("fresh slot");
        slot.reserved = ctx.reserved;
        slot.offload = ctx.offload;
        slot.owner.put(owner);
        slot.class = ctx.class;
        slot.deadline = ctx.deadline.instant();
        slot.returns_fd = O::RETURNS_FD;
        slot.retry_interest = O::RETRY_INTEREST;
        slot.prepare = Some(prepare_erased::<O>);
        slot.discard = Some(discard_erased::<O>);
        // SAFETY: the cell holds the O stored above; scratch belongs to this slot.
        let prepared = unsafe { prepare_erased::<O>(slot.owner.raw(), &mut slot.scratch) };
        let op = match prepared {
            Ok(op) => op,
            Err(error) => {
                // SAFETY: the cell holds the O stored above.
                let owner = unsafe { slot.owner.take::<O>() };
                state.slab.release(id);
                drop(state);
                drop(owner);
                return Err(error);
            }
        };
        slot_retry_fd(slot, &op);
        if let Err(error) = Self::push(&mut state, id.0, &op, ctx.offload) {
            let slot = state.slab.lookup(id).expect("fresh slot");
            // SAFETY: the cell holds the O stored above.
            let owner = unsafe { slot.owner.take::<O>() };
            state.slab.release(id);
            state.rejected += 1;
            drop(state);
            drop(owner);
            return Err(error);
        }
        if ctx.reserved {
            state.reserved_live += 1;
        } else {
            state.ordinary_live += 1;
        }
        state.classes[ctx.class.0 as usize] += 1;
        state.unfenced += 1;
        if let Some(deadline) = ctx.deadline.instant() {
            state.deadlines.push(Reverse((deadline, id)));
        }
        Ok(id)
    }

    /// Queue one SQE, flushing the SQ once if it is full.
    fn push(state: &mut State, user_data: u64, op: &Op, offload: bool) -> Result<()> {
        match &mut state.backend {
            Backend::Uring(ring) => {
                if ring.push(user_data, op, offload).is_ok() {
                    return Ok(());
                }
                ring.submit()?;
                ring.push(user_data, op, offload)
                    .map_err(|_| Error::Overloaded)
            }
            Backend::Sim(driver) => driver.push(user_data, op).map_err(|_| Error::Overloaded),
        }
    }

    /// Poll an operation's result for its future.
    pub(crate) fn poll_op<O: sealed::OpOwner>(
        &self,
        id: IoId,
        cx: &mut Context<'_>,
        ctx: &OpCtx,
    ) -> Poll<Result<O::Value>> {
        let mut state = self.state();
        let slot = state.slab.lookup(id).expect("polled operation is live");
        let Some(result) = slot.done else {
            match &mut slot.waker {
                Some(old) if old.will_wake(cx.waker()) => (),
                waker => *waker = Some(cx.waker().clone()),
            }
            return Poll::Pending;
        };
        // SAFETY: the slot holds the O submitted under this id.
        let owner = unsafe { slot.owner.take::<O>() };
        let returns_fd = slot.returns_fd;
        let cancel = slot.cancel;
        state.release_slot(id);
        drop(state);
        // Cancellation and deadline take precedence over a late success.
        let reason = cancel.or_else(|| ctx.check(self.now()).err());
        if let Some(reason) = reason {
            if returns_fd && result >= 0 {
                Self::close_raw(&mut self.state(), result);
            }
            owner.discard(result);
            return Poll::Ready(Err(reason));
        }
        Poll::Ready(owner.finish(result, self))
    }

    /// Request cancellation of an in-flight operation.
    pub(crate) fn cancel(&self, id: IoId, reason: Error) {
        let mut state = self.state();
        Self::cancel_locked(&mut state, id, reason);
    }

    fn cancel_locked(state: &mut State, id: IoId, reason: Error) {
        let Some(slot) = state.slab.lookup(id) else {
            return;
        };
        if slot.done.is_some() {
            return;
        }
        slot.cancel.get_or_insert(reason);
        if !slot.fence.needs_cancel() {
            return;
        }
        let op = Op::Cancel { target: id.0 };
        if Self::push(state, id.0 | CANCEL_BIT, &op, false).is_ok() {
            if let Some(slot) = state.slab.lookup(id) {
                slot.fence = Fence::Pending(CancelFence::Pending);
            }
        } else {
            state.cancel_backlog.push(id);
        }
    }

    /// Abandon an operation whose future was dropped.
    pub(crate) fn abandon(&self, id: IoId) {
        let mut state = self.state();
        let Some(slot) = state.slab.lookup(id) else {
            return;
        };
        if let Some(result) = slot.done {
            let returns_fd = slot.returns_fd;
            let release = discard_slot(slot, result);
            state.release_slot(id);
            if returns_fd && result >= 0 {
                Self::close_raw(&mut state, result);
            }
            drop(state);
            if let Some(release) = release {
                release();
            }
            return;
        }
        slot.abandoned = true;
        slot.waker = None;
        Self::cancel_locked(&mut state, id, Error::Cancelled);
    }

    /// Process expired deadlines and timers, then up to `budget` CQEs, then
    /// submit queued SQEs (skipping the syscall when the SQ is empty).
    /// Returns the number of CQEs processed.
    pub fn reap(&self, budget: usize) -> Result<usize> {
        let now = self.now();
        let mut wakers = std::mem::take(&mut self.state().wakers);
        let mut state = self.state();
        // Expire in-flight deadlines: each becomes a cancel, never a scan.
        while let Some(Reverse((deadline, id))) = state.deadlines.peek().copied() {
            if deadline > now {
                break;
            }
            state.deadlines.pop();
            let current = state
                .slab
                .lookup(id)
                .is_some_and(|slot| slot.deadline == Some(deadline) && slot.done.is_none());
            if current {
                state.expired += 1;
                Self::cancel_locked(&mut state, id, Error::DeadlineExceeded);
            }
        }
        state.timers.expire(now, &mut wakers);
        if self.0.wake.is_pending() && matches!(state.backend, Backend::Sim(_)) {
            self.0.wake.clear();
        }
        let backlog = std::mem::take(&mut state.cancel_backlog);
        for id in backlog {
            Self::cancel_locked(&mut state, id, Error::Cancelled);
        }
        if let Backend::Sim(driver) = &mut state.backend {
            driver.run_turn(now);
        }
        let mut completed = 0;
        let mut failure = None;
        while completed < budget {
            let next = match &mut state.backend {
                Backend::Uring(ring) => ring.next_cqe(),
                Backend::Sim(driver) => driver.next_cqe(),
            };
            let Some((user_data, result, flags)) = next else {
                break;
            };
            completed += 1;
            if user_data == STOP_TAG {
                continue;
            }
            if user_data == WAKE_TAG {
                self.0.wake.drain();
                self.0.wake.clear();
                if !uring::more(flags) {
                    state.wake_armed = false;
                }
                continue;
            }
            if let Err(error) = Self::complete(&mut state, user_data, result, &mut wakers) {
                state.stopped = true;
                failure = Some(error);
                break;
            }
        }
        if !state.wake_armed && !state.stopped {
            drop(state);
            self.arm_wake()?;
            state = self.state();
        }
        let submitted = match &mut state.backend {
            Backend::Uring(ring) => ring.submit(),
            Backend::Sim(_) => Ok(()),
        };
        if submitted.is_err() {
            state.stopped = true;
        }
        if !state.fence_waiters.is_empty() {
            let classes = state.classes.clone();
            let live = state.unfenced;
            let mut index = 0;
            while index < state.fence_waiters.len() {
                let done = match state.fence_waiters[index].0 {
                    None => live == 0,
                    Some(class) => classes[class.0 as usize] == 0,
                };
                if done {
                    wakers.push(state.fence_waiters.swap_remove(index).1);
                } else {
                    index += 1;
                }
            }
        }
        if !state.id_waiters.is_empty() {
            let mut index = 0;
            while index < state.id_waiters.len() {
                let id = state.id_waiters[index].0;
                if Self::fenced(&mut state.slab, id) {
                    wakers.push(state.id_waiters.swap_remove(index).1);
                } else {
                    index += 1;
                }
            }
        }
        let graveyard = std::mem::take(&mut state.graveyard);
        drop(state);
        for release in graveyard {
            release();
        }
        for waker in wakers.drain(..) {
            waker.wake();
        }
        let mut state = self.state();
        if state.wakers.capacity() == 0 {
            state.wakers = wakers;
        }
        drop(state);
        if let Some(error) = failure {
            return Err(error);
        }
        submitted.map(|()| completed)
    }

    /// Account one CQE.
    fn complete(
        state: &mut State,
        user_data: u64,
        result: i32,
        wakers: &mut Vec<Waker>,
    ) -> Result<()> {
        let cancel = user_data & CANCEL_BIT != 0;
        let id = IoId(user_data & !CANCEL_BIT);
        let slot = state.slab.lookup(id).ok_or(Error::Io)?;
        let Some(original) = slot.fence.complete(cancel, result)? else {
            return Ok(());
        };
        // Zero-progress retry inside the reactor, never after cancellation.
        if slot.cancel.is_none() && !slot.abandoned {
            let retry = if slot.polling {
                slot.polling = false;
                (original >= 0).then_some(false)
            } else if slot.retry_interest != 0
                && (original == -libc::EINTR || original == -libc::EAGAIN)
                && slot.attempts < DATA_RETRIES
            {
                slot.attempts += 1;
                Some(original == -libc::EAGAIN)
            } else {
                None
            };
            if let Some(poll_first) = retry {
                state.retries += 1;
                let op = if poll_first {
                    slot.polling = true;
                    Ok(Op::Poll {
                        fd: slot.retry_fd,
                        mask: slot.retry_interest,
                        multi: false,
                    })
                } else {
                    let prepare = slot.prepare.expect("retryable operation");
                    // SAFETY: the slot holds the owner this function was built for.
                    unsafe { prepare(slot.owner.raw(), &mut slot.scratch) }
                };
                slot.fence = Fence::Pending(CancelFence::NotSubmitted);
                let offload = slot.offload;
                let pushed = op.and_then(|op| Self::push(state, id.0, &op, offload));
                match pushed {
                    Ok(()) => return Ok(()),
                    Err(error) => {
                        let slot = state.slab.lookup(id).expect("retried slot");
                        slot.fence = Fence::Retired;
                        return Self::finish(
                            state,
                            id,
                            -error.errno().unwrap_or(libc::EBUSY),
                            wakers,
                        );
                    }
                }
            }
        }
        Self::finish(state, id, original, wakers)
    }

    /// Deliver a fully fenced result or release an abandoned slot.
    fn finish(state: &mut State, id: IoId, result: i32, wakers: &mut Vec<Waker>) -> Result<()> {
        let slot = state.slab.lookup(id).expect("finished slot");
        let class = slot.class;
        state.classes[class.0 as usize] -= 1;
        state.unfenced -= 1;
        let slot = state.slab.lookup(id).expect("finished slot");
        if slot.abandoned {
            let returns_fd = slot.returns_fd;
            if let Some(release) = discard_slot(slot, result) {
                state.graveyard.push(release);
            }
            state.release_slot(id);
            if returns_fd && result >= 0 {
                Self::close_raw(state, result);
            }
            return Ok(());
        }
        slot.done = Some(result);
        if let Some(waker) = slot.waker.take() {
            wakers.push(waker);
        }
        Ok(())
    }

    /// Earliest instant at which [`Self::reap`] has timed work: an operation
    /// deadline, a timer, or (simulated driver) the end of an operation's
    /// cost-model latency.
    pub fn next_deadline(&self) -> Option<Instant> {
        let mut state = self.state();
        let state = &mut *state;
        while let Some(Reverse((deadline, id))) = state.deadlines.peek().copied() {
            let current = state
                .slab
                .lookup(id)
                .is_some_and(|slot| slot.deadline == Some(deadline) && slot.done.is_none());
            if current {
                break;
            }
            state.deadlines.pop();
        }
        let deadline = state.deadlines.peek().map(|Reverse((at, _))| *at);
        let timer = state.timers.next();
        let latency = match &state.backend {
            Backend::Sim(driver) => driver.next_ready_at(),
            Backend::Uring(_) => None,
        };
        [deadline, timer, latency].into_iter().flatten().min()
    }

    /// Whether completions or a cross-thread wake are ready without waiting.
    pub fn has_ready(&self) -> bool {
        if self.0.wake.is_pending() {
            return true;
        }
        let now = self.now();
        match &mut self.state().backend {
            Backend::Uring(ring) => ring.has_cqes(),
            Backend::Sim(driver) => driver.has_cqes() || driver.runnable(now),
        }
    }

    /// Submit and wait until a completion, a cross-thread wake, or `until`.
    ///
    /// The kernel driver blocks in one `io_uring_enter` (the eventfd is a
    /// multishot poll on the ring, so cross-thread wakes are completions).
    /// The simulated driver never blocks: with a virtual clock and nothing
    /// runnable it advances the clock to `until`.
    pub fn park(&self, until: Option<Instant>) -> Result<()> {
        let now = self.now();
        let mut state = self.state();
        match &mut state.backend {
            Backend::Uring(ring) => {
                let timeout = until.map(|until| until.saturating_duration_since(now));
                if timeout.is_some_and(|timeout| timeout.is_zero()) {
                    return ring.submit();
                }
                ring.wait(timeout)
            }
            Backend::Sim(driver) => {
                if driver.has_cqes() || driver.runnable(now) || self.0.wake.is_pending() {
                    return Ok(());
                }
                if let (Some(until), Some(clock)) = (until, self.0.ctx.clock.as_virtual()) {
                    clock.advance_to(until)?;
                }
                Ok(())
            }
        }
    }

    /// Sleep until `until` on this reactor's clock.
    pub fn sleep_until(&self, until: Instant) -> Sleep {
        Sleep {
            reactor: self.clone(),
            until,
            key: None,
        }
    }

    /// Stop admission and cancel every in-flight operation. Await
    /// [`Self::fence_all`] (while the lane keeps reaping) to wait for fences.
    pub fn stop(&self) {
        let mut state = self.state();
        state.stopped = true;
        let ids: Vec<IoId> = live_ids(&mut state.slab);
        for id in ids {
            Self::cancel_locked(&mut state, id, Error::Cancelled);
        }
    }

    /// Whether admission is stopped.
    pub fn is_stopped(&self) -> bool {
        self.state().stopped
    }

    /// Cancel every in-flight operation of `class` and wait until all of them
    /// have fenced. Work submitted later in the same class extends the wait.
    pub fn fence_class(&self, class: IoClass) -> FenceWait {
        {
            let mut state = self.state();
            let ids: Vec<IoId> = live_ids(&mut state.slab)
                .into_iter()
                .filter(|id| {
                    state
                        .slab
                        .lookup(*id)
                        .is_some_and(|slot| slot.class == class && slot.done.is_none())
                })
                .collect();
            for id in ids {
                Self::cancel_locked(&mut state, id, Error::Cancelled);
            }
        }
        FenceWait {
            reactor: self.clone(),
            class: Some(class),
            registered: None,
        }
    }

    /// Whether every kernel completion of `id` has arrived (or `id` is no
    /// longer live). A delivered but untaken result counts as fenced.
    pub fn is_fenced(&self, id: IoId) -> bool {
        Self::fenced(&mut self.state().slab, id)
    }

    fn fenced(slab: &mut Slab, id: IoId) -> bool {
        slab.lookup(id)
            .is_none_or(|slot| slot.fence == Fence::Retired)
    }

    /// Cancel one in-flight operation with `reason`, as if its context had
    /// been canceled. Unknown or fenced ids are ignored.
    pub fn cancel_op(&self, id: IoId, reason: Error) {
        self.cancel(id, reason);
    }

    /// Wait until every operation in `ids` has fenced, without canceling
    /// them. Ids that are already fenced or unknown are skipped.
    pub fn fence_ids(&self, ids: Vec<IoId>) -> FenceIds {
        FenceIds {
            reactor: self.clone(),
            ids,
            registered: None,
        }
    }

    /// Change the reserved partition ([`Config::reserved_entries`]) of a
    /// running reactor. Fails with [`Error::InvalidConfiguration`] when
    /// `entries` exceeds the queue, or when live ordinary operations would
    /// exceed the shrunken ordinary partition.
    pub fn set_reserved_entries(&self, entries: u32) -> Result<()> {
        let mut state = self.state();
        let total = state.ordinary_limit + state.reserved_limit;
        let entries = entries as usize;
        if entries > total || state.ordinary_live > total - entries {
            return Err(Error::InvalidConfiguration);
        }
        state.ordinary_limit = total - entries;
        state.reserved_limit = entries;
        Ok(())
    }

    /// Stop admission, cancel everything, and wait until nothing is in flight.
    pub fn fence_all(&self) -> FenceWait {
        self.stop();
        FenceWait {
            reactor: self.clone(),
            class: None,
            registered: None,
        }
    }

    /// Drive [`Self::stop`] to completion on this thread without an executor,
    /// for shutdown paths: reap and park until nothing is in flight or
    /// `deadline` passes. An error is never a fence.
    pub fn shutdown(&self, deadline: Instant) -> Result<()> {
        self.stop();
        while self.in_flight() != 0 {
            if self.now() >= deadline {
                return Err(Error::DeadlineExceeded);
            }
            self.reap(256)?;
            if self.in_flight() != 0 {
                self.park(Some(deadline))?;
            }
        }
        Ok(())
    }
}

/// Every live slot id.
fn live_ids(slab: &mut Slab) -> Vec<IoId> {
    let mut ids = Vec::with_capacity(slab.live);
    for (chunk_index, chunk) in slab.chunks.iter().enumerate() {
        for (offset, slot) in chunk.iter().enumerate() {
            if slot.live {
                ids.push(IoId::new(
                    (chunk_index * CHUNK + offset) as u32,
                    slot.generation,
                ));
            }
        }
    }
    ids
}

/// Remember the descriptor a data retry polls on `EAGAIN`.
fn slot_retry_fd(slot: &mut Slot, op: &Op) {
    slot.retry_fd = match *op {
        Op::Read { fd, .. }
        | Op::Write { fd, .. }
        | Op::Readv { fd, .. }
        | Op::Writev { fd, .. }
        | Op::Recv { fd, .. }
        | Op::Send { fd, .. }
        | Op::RecvMsg { fd, .. }
        | Op::SendMsg { fd, .. } => fd,
        _ => -1,
    };
}

/// Erased [`sealed::OpOwner::prepare`].
/// Erased [`sealed::OpOwner::discard`].
unsafe fn discard_erased<O: sealed::OpOwner>(cell: &mut OwnerCell, result: i32) -> Deferred {
    // SAFETY: the caller passes the cell holding an O.
    let owner = unsafe { cell.take::<O>() };
    Box::new(move || owner.discard(result))
}

/// Take a fenced, undelivered slot's owners for release through their
/// discard hook once the state borrow is gone.
fn discard_slot(slot: &mut Slot, result: i32) -> Option<Deferred> {
    let discard = slot.discard.take()?;
    // SAFETY: discard was built for the owner stored in this cell.
    Some(unsafe { discard(&mut slot.owner, result) })
}

unsafe fn prepare_erased<O: sealed::OpOwner>(
    owner: *mut InlineBytes,
    scratch: *mut Scratch,
) -> Result<Op> {
    // SAFETY: the caller passes the cell holding an O and its slot's scratch.
    let (owner, scratch) = unsafe { (owner::erased_mut::<O>(owner), &mut *scratch) };
    owner.prepare(scratch)
}

impl Drop for Inner {
    /// Leak the ring and slots if work is still in flight: closing the ring
    /// descriptor is not a memory fence.
    fn drop(&mut self) {
        let state = self.state.get_mut();
        let leak = state.unfenced != 0 && matches!(state.backend, Backend::Uring(_));
        if !leak {
            // SAFETY: the state is dropped exactly once, here.
            unsafe { std::mem::ManuallyDrop::drop(&mut self.state) };
        }
    }
}

pub mod sealed {
    //! Operation owner protocol, implemented only inside this crate.
    use super::{Reactor, op::Op, op::Scratch};
    use crate::Result;

    /// The owners of one operation and how to issue and finish it.
    pub trait OpOwner: 'static {
        /// The successful result.
        type Value;

        /// Poll mask for bounded zero-progress retries; zero disables them.
        const RETRY_INTEREST: u32 = 0;

        /// Whether a nonnegative result is a new descriptor.
        const RETURNS_FD: bool = false;

        /// Build the operation. Called with the owner at its final address.
        #[doc(hidden)]
        fn prepare(&mut self, scratch: &mut Scratch) -> Result<Op>;

        /// Map the raw result (negative errno on failure) after every fence.
        #[doc(hidden)]
        fn finish(self, result: i32, reactor: &Reactor) -> Result<Self::Value>;

        /// Release the owners of a fenced operation whose result is not
        /// delivered (canceled, expired, or abandoned). `result` is the raw
        /// kernel result, so owners can account for side effects that did
        /// happen (for example a close that ran).
        #[doc(hidden)]
        fn discard(self, result: i32)
        where
            Self: Sized,
        {
            let _ = result;
        }
    }
}

/// The future of one submitted operation. Dropping it abandons the result;
/// the owners stay in the reactor until every fence.
#[must_use = "operations do nothing until polled"]
pub struct IoFuture<O: sealed::OpOwner> {
    reactor: Reactor,

    ctx: OpCtx,

    state: FutureState<O>,

    key: Option<crate::cancel::WaitKey>,
}

enum FutureState<O> {
    Init(O),
    Waiting(IoId),
    Done,
}

impl<O: sealed::OpOwner> IoFuture<O> {
    pub(crate) fn new(reactor: &Reactor, owner: O, ctx: &OpCtx) -> Self {
        Self {
            reactor: reactor.clone(),
            ctx: ctx.clone(),
            state: FutureState::Init(owner),
            key: None,
        }
    }

    /// The operation identity once submitted.
    pub fn id(&self) -> Option<IoId> {
        match self.state {
            FutureState::Waiting(id) => Some(id),
            _ => None,
        }
    }
}

impl<O: sealed::OpOwner> Unpin for IoFuture<O> {}

impl<O: sealed::OpOwner> Future for IoFuture<O> {
    type Output = Result<O::Value>;

    /// Submit on first poll, then deliver the fenced result.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let id = match std::mem::replace(&mut this.state, FutureState::Done) {
            FutureState::Init(owner) => match this.reactor.submit(owner, &this.ctx) {
                Ok(id) => id,
                Err(error) => return Poll::Ready(Err(error)),
            },
            FutureState::Waiting(id) => id,
            FutureState::Done => panic!("IoFuture polled after completion"),
        };
        match this.reactor.poll_op::<O>(id, cx, &this.ctx) {
            Poll::Ready(result) => {
                if let Some(token) = &this.ctx.cancel {
                    token.deregister(&mut this.key);
                }
                Poll::Ready(result)
            }
            Poll::Pending => {
                this.state = FutureState::Waiting(id);
                if let Some(token) = &this.ctx.cancel
                    && let Some(reason) = token.register(&mut this.key, cx.waker())
                {
                    this.reactor.cancel(id, reason);
                }
                Poll::Pending
            }
        }
    }
}

impl<O: sealed::OpOwner> Drop for IoFuture<O> {
    /// Abandon a submitted operation without releasing its owners early.
    fn drop(&mut self) {
        if let Some(token) = &self.ctx.cancel {
            token.deregister(&mut self.key);
        }
        if let FutureState::Waiting(id) = self.state {
            self.reactor.abandon(id);
        }
    }
}

/// Waits for a fence requested by [`Reactor::fence_class`] or
/// [`Reactor::fence_all`].
#[must_use = "fences do nothing until polled"]
pub struct FenceWait {
    reactor: Reactor,

    class: Option<IoClass>,

    /// Waker last registered in `fence_waiters`, replaced on each poll and
    /// removed on drop so repeated polls and abandoned waits stay bounded.
    registered: Option<Waker>,
}

impl Future for FenceWait {
    type Output = ();

    /// Ready once no matching operation is in flight.
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let this = &mut *self;
        let mut state = this.reactor.state();
        if let Some(old) = this.registered.take() {
            unregister(&mut state.fence_waiters, &this.class, &old);
        }
        let done = match this.class {
            None => state.unfenced == 0,
            Some(class) => state.classes[class.0 as usize] == 0,
        };
        if done {
            return Poll::Ready(());
        }
        let waker = cx.waker().clone();
        state.fence_waiters.push((this.class, waker.clone()));
        this.registered = Some(waker);
        Poll::Pending
    }
}

impl Drop for FenceWait {
    /// Remove a still-registered waker.
    fn drop(&mut self) {
        if let Some(old) = self.registered.take() {
            unregister(&mut self.reactor.state().fence_waiters, &self.class, &old);
        }
    }
}

/// Remove one waiter registered under `key` with `waker`, if still present
/// (a completed fence already removed it when waking).
fn unregister<K: PartialEq>(waiters: &mut Vec<(K, Waker)>, key: &K, waker: &Waker) {
    if let Some(index) = waiters
        .iter()
        .position(|(k, w)| k == key && w.will_wake(waker))
    {
        waiters.swap_remove(index);
    }
}

/// Waits for the operations named by [`Reactor::fence_ids`].
#[must_use = "fences do nothing until polled"]
pub struct FenceIds {
    reactor: Reactor,

    ids: Vec<IoId>,

    /// Registration in `id_waiters`, replaced on each poll and removed on drop.
    registered: Option<(IoId, Waker)>,
}

impl Future for FenceIds {
    type Output = ();

    /// Ready once every named operation has fenced.
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let this = &mut *self;
        let mut state = this.reactor.state();
        if let Some((id, old)) = this.registered.take() {
            unregister(&mut state.id_waiters, &id, &old);
        }
        this.ids.retain(|id| !Reactor::fenced(&mut state.slab, *id));
        let Some(id) = this.ids.last().copied() else {
            return Poll::Ready(());
        };
        let waker = cx.waker().clone();
        state.id_waiters.push((id, waker.clone()));
        this.registered = Some((id, waker));
        Poll::Pending
    }
}

impl Drop for FenceIds {
    /// Remove a still-registered waker.
    fn drop(&mut self) {
        if let Some((id, old)) = self.registered.take() {
            unregister(&mut self.reactor.state().id_waiters, &id, &old);
        }
    }
}

/// Reactor-local timers on the worker clock.
#[derive(Default)]
struct Timers {
    heap: BinaryHeap<Reverse<(Instant, u64)>>,

    entries: std::collections::BTreeMap<u64, Option<Waker>>,

    next: u64,
}

impl Timers {
    fn insert(&mut self, until: Instant) -> u64 {
        self.next += 1;
        self.heap.push(Reverse((until, self.next)));
        self.entries.insert(self.next, None);
        self.next
    }

    fn expire(&mut self, now: Instant, wakers: &mut Vec<Waker>) {
        while let Some(Reverse((until, key))) = self.heap.peek().copied() {
            if until > now {
                break;
            }
            self.heap.pop();
            if let Some(Some(waker)) = self.entries.remove(&key) {
                wakers.push(waker);
            }
        }
    }

    fn next(&mut self) -> Option<Instant> {
        while let Some(Reverse((until, key))) = self.heap.peek().copied() {
            if self.entries.contains_key(&key) {
                return Some(until);
            }
            self.heap.pop();
        }
        None
    }
}

/// A reactor timer future.
#[must_use = "timers do nothing until polled"]
pub struct Sleep {
    reactor: Reactor,

    until: Instant,

    key: Option<u64>,
}

impl Sleep {
    /// The wake instant.
    pub fn deadline(&self) -> Instant {
        self.until
    }
}

impl Future for Sleep {
    type Output = ();

    /// Ready once the reactor clock reaches the deadline.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let this = self.get_mut();
        if this.reactor.now() >= this.until {
            if let Some(key) = this.key.take() {
                this.reactor.state().timers.entries.remove(&key);
            }
            return Poll::Ready(());
        }
        let mut state = this.reactor.state();
        let key = *this
            .key
            .get_or_insert_with(|| state.timers.insert(this.until));
        match state.timers.entries.get_mut(&key) {
            Some(slot) => {
                if !slot.as_ref().is_some_and(|w| w.will_wake(cx.waker())) {
                    *slot = Some(cx.waker().clone());
                }
            }
            None => {
                // Expired by reap but our clock read raced; re-arm.
                let key = state.timers.insert(this.until);
                state.timers.entries.insert(key, Some(cx.waker().clone()));
                this.key = Some(key);
            }
        }
        Poll::Pending
    }
}

impl Drop for Sleep {
    /// Remove the timer entry.
    fn drop(&mut self) {
        if let Some(key) = self.key {
            self.reactor.state().timers.entries.remove(&key);
        }
    }
}

#[cfg(test)]
pub(crate) mod tests;
