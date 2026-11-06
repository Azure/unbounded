//! Worker-local I/O adapter over [`crate::Reactor`].
//!
//! Callers hold a [`Scope`] rather than an [`OpCtx`]: each operation is
//! submitted with the scope's context, every submission, completion fence,
//! deadline, and retry is owned by [`crate::Reactor`], and a scope's own error
//! takes precedence over the operation's once the operation is fenced. In-flight
//! operations are bounded by the reactor's statically sized operation slab and
//! reserved partition; request-level admission belongs to the caller.
//!
//! Dropping a waiting operation abandons its result only: `racer-io` keeps the
//! buffer, descriptor, and lease until the original and any cancel completion
//! have both arrived. A scope whose `check` fails while its operation is in
//! flight is canceled from [`Reactor::poll_budgeted`], so policies that are not
//! expressible as a cancel token or deadline still bound kernel work.

use crate::{Error, Operation, Result, Scope};
use crate::{
    OpCtx,
    ring::{Config, Driver, IoId, WakeHandle},
};
use descriptor::Descriptor;

use std::{
    cell::{Cell, OnceCell, RefCell},
    future::Future,
    pin::Pin,
    rc::{Rc, Weak},
    sync::Arc,
    task::{Context, Poll},
    time::{Duration, Instant},
};

pub use crate::ring::{
    Completion, IoBuffer, SendBuffer, SharedWriteBuffer, SocketAddress, VectoredCompletion,
    ops::MessageCompletion,
};

/// Worker-local I/O owner, explicitly driven by [`Self::poll_budgeted`].
/// Dropping a waiting operation abandons delivery, never its kernel-owned resources.
pub struct Reactor<S: Scope> {
    rejection_observer: Option<Box<RejectionObserver<S>>>,

    /// Backend and worker context for code driven by this reactor.
    env: crate::Env,

    queue_entries: usize,

    io: OnceCell<crate::Reactor>,

    /// The kernel reactor runs on host time while scopes use a simulated
    /// clock: scope deadlines are then enforced by the scope scan only.
    foreign_clock: Cell<bool>,

    stopped: Cell<bool>,

    records: RefCell<Vec<Rc<Record<S>>>>,

    /// Admissions of abandoned operations, released once `racer-io` reports
    /// their kernel fences.
    parked: RefCell<Vec<(IoId, Admission<S>)>>,

    reserved: RefCell<Weak<SubmissionCapacity>>,
}

/// One admitted operation: its scope (for scans and [`Reactor::fence_matching`])
/// and its `racer-io` identity once submitted.
struct Record<S> {
    scope: S,

    id: Cell<Option<IoId>>,
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

/// Startup-owned partition of the queue reserved for control traffic.
/// The caller supplies the retained charge; this capability grants no data authorization.
pub struct SubmissionCapacity {
    _memory: Charge,

    io: crate::Reactor,

    capacity: usize,
}

impl Drop for SubmissionCapacity {
    /// Return the partition to ordinary operations.
    fn drop(&mut self) {
        let _ = self.io.set_reserved_entries(0);
    }
}

impl SubmissionCapacity {
    /// Slots in the partition.
    pub fn capacity(&self) -> usize {
        self.capacity
    }
}

/// Maximum completion bookkeeping charged to one submission.
pub const SUBMISSION_BYTES: usize = 4096;

/// Longest single sleep in [`Reactor::wait`].
const MAX_WAIT: Duration = Duration::from_millis(10);

/// Bounded fence attempted when a reactor is dropped.
const DROP_WAIT: Duration = Duration::from_millis(100);

/// An owned guard an operation retains until its kernel fences.
pub type Charge = Box<dyn std::any::Any>;

/// Cloneable cross-thread wake endpoint for worker command/crypto producers.
#[derive(Clone)]
pub struct ReactorWake {
    handle: Option<Arc<WakeHandle>>,
}

impl ReactorWake {
    /// Wake the owning worker; coalesced until its next turn.
    pub fn wake(&self) -> Result<()> {
        if let Some(handle) = &self.handle {
            handle.wake();
        }
        Ok(())
    }
}

/// Most `SCM_RIGHTS` descriptors one [`Reactor::recv_msg`] accepts.
pub const MAX_RECEIVED_DESCRIPTORS: usize = 8;

/// Linux `SOL_TLS` socket level.
pub const SOL_TLS: i32 = 282;

/// `TLS_GET_RECORD_TYPE` control message type.
pub const TLS_GET_RECORD_TYPE: i32 = 2;

/// Present exclusive storage to `racer-io` writes, which only read it.
struct Exclusive<B>(B);

// SAFETY: the IoBuffer contract keeps the bytes stable and unaliased while the
// reactor owns the buffer; writes and sends only read them.
unsafe impl<B: IoBuffer> SendBuffer for Exclusive<B> {
    fn send_bytes(&self) -> crate::Result<&[u8]> {
        self.0.bytes()
    }
}

/// Admission retained by an operation until it is fenced and delivered.
struct Admission<S> {
    _held: Option<Charge>,

    _capacity: Option<Rc<SubmissionCapacity>>,

    record: Rc<Record<S>>,
}

impl<S: Scope> Reactor<S> {
    /// Worker clock and entropy for code driven by this reactor.
    pub fn ctx(&self) -> &crate::WorkerCtx {
        &self.env.ctx
    }

    /// The backend and worker context this reactor was built with.
    pub fn env(&self) -> &crate::Env {
        &self.env
    }

    /// The simulated OS this reactor drives, or `None` on the kernel.
    pub fn sim_os(&self) -> Option<crate::sim::SimOs> {
        self.env.sim_os().cloned()
    }

    /// Create an uninitialized reactor; resources are acquired by [`Self::init`].
    pub fn new(queue_entries: usize, env: &crate::Env) -> Self {
        Self {
            rejection_observer: None,
            env: env.clone(),
            queue_entries,
            io: OnceCell::new(),
            foreign_clock: Cell::new(false),
            stopped: Cell::new(false),
            records: RefCell::new(Vec::new()),
            parked: RefCell::new(Vec::new()),
            reserved: RefCell::default(),
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
        self.io().map(|_| ())
    }

    /// The `racer-io` reactor, built on first use.
    fn io(&self) -> Result<&crate::Reactor> {
        if self.stopped.get() {
            return Err(Error::Unavailable);
        }
        if let Some(io) = self.io.get() {
            return Ok(io);
        }
        let entries = u32::try_from(self.queue_entries).map_err(|_| Error::InvalidConfiguration)?;
        if entries == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let ctx = self.ctx().clone();
        let driver = self.env.driver.clone();
        let ctx = if matches!(driver, Driver::Uring) && ctx.is_simulated() {
            self.foreign_clock.set(true);
            crate::WorkerCtx::host()
        } else {
            ctx
        };
        let io = crate::Reactor::new(driver, Config::new(entries), ctx).map_err(
            |error| match error {
                Error::InvalidConfiguration | Error::Overloaded => error,
                _ => Error::Io,
            },
        )?;
        Ok(self.io.get_or_init(|| io))
    }

    /// Obtain after init, and attach to external worker queues before starting them.
    pub fn waker(&self) -> Result<ReactorWake> {
        Ok(ReactorWake {
            handle: Some(self.io()?.wake_handle()),
        })
    }

    /// Earliest instant, on [`Self::ctx`]'s clock, at which
    /// [`Self::poll_budgeted`] has timed work such as a timer or operation
    /// deadline. `None` when idle, uninitialized, or when the kernel reactor
    /// runs on host time under a simulated worker clock.
    pub fn next_deadline(&self) -> Option<Instant> {
        if self.foreign_clock.get() {
            return None;
        }
        self.io.get().and_then(crate::Reactor::next_deadline)
    }

    /// Count operations still awaiting their original or cancellation CQE.
    pub fn in_flight(&self) -> usize {
        self.io.get().map_or(0, crate::Reactor::in_flight)
    }

    /// Partition queue capacity for control traffic using a caller-owned
    /// prepaid charge. Only one live partition is allowed.
    pub fn reserve_submissions<C: 'static>(
        &self,
        capacity: usize,
        charge: C,
    ) -> Result<Rc<SubmissionCapacity>> {
        if self.reserved.borrow().upgrade().is_some() || capacity > self.queue_entries {
            return Err(Error::InvalidConfiguration);
        }
        let io = self.io()?;
        io.set_reserved_entries(u32::try_from(capacity).map_err(|_| Error::InvalidConfiguration)?)?;
        let reserved = Rc::new(SubmissionCapacity {
            _memory: Box::new(charge),
            io: io.clone(),
            capacity,
        });
        *self.reserved.borrow_mut() = Rc::downgrade(&reserved);
        Ok(reserved)
    }

    /// Admit one operation: initialize, check the partition (reserved), register the scope, and build its context.
    fn admit(
        &self,
        scope: &S,
        capacity: Option<Rc<SubmissionCapacity>>,
        kind: SubmissionKind,
        held: Option<Charge>,
    ) -> Result<(crate::Reactor, OpCtx, Admission<S>), S::Error> {
        scope.check()?;
        let reserved = capacity.is_some();
        let reject = |reason, requested| {
            self.rejected(
                scope,
                SubmissionRejection {
                    reason,
                    kind,
                    reserved,
                    used: None,
                    limit: None,
                    requested,
                },
            );
        };
        let io = self
            .io()
            .inspect_err(|error| {
                if *error == Error::Overloaded {
                    reject(RejectionReason::Initialization, 1);
                }
            })?
            .clone();
        if let Some(pool) = &capacity {
            let own = self.reserved.borrow().upgrade();
            if !own.is_some_and(|own| Rc::ptr_eq(&own, pool)) {
                return Err(Error::InvalidConfiguration.into());
            }
        }
        let mut ctx = scope.op_ctx();
        if self.foreign_clock.get() {
            ctx.deadline = crate::Deadline::default();
        }
        ctx.reserved = reserved;
        let record = Rc::new(Record {
            scope: scope.clone(),
            id: Cell::new(None),
        });
        self.records.borrow_mut().push(record.clone());
        Ok((
            io,
            ctx,
            Admission {
                _held: held,
                _capacity: capacity,
                record,
            },
        ))
    }

    /// Submit and await one `racer-io` operation under `scope`. A scope
    /// failure takes precedence over the operation's own error, but only after
    /// the operation is fenced.
    pub fn run<'a, T: 'static, F>(
        &'a self,
        scope: &'a S,
        capacity: Option<Rc<SubmissionCapacity>>,
        kind: SubmissionKind,
        make: impl FnOnce(&crate::Reactor, &OpCtx) -> F + 'a,
        id: fn(&F) -> Option<IoId>,
    ) -> Operation<'a, T, S::Error>
    where
        F: Future<Output = crate::Result<T>> + Unpin + 'a,
    {
        self.run_holding(scope, capacity, kind, None, make, id)
    }

    /// [`Self::run`], also retaining `held` until the operation is fenced,
    /// even when the waiting future is dropped first.
    pub fn run_holding<'a, T: 'static, F>(
        &'a self,
        scope: &'a S,
        capacity: Option<Rc<SubmissionCapacity>>,
        kind: SubmissionKind,
        held: Option<Charge>,
        make: impl FnOnce(&crate::Reactor, &OpCtx) -> F + 'a,
        id: fn(&F) -> Option<IoId>,
    ) -> Operation<'a, T, S::Error>
    where
        F: Future<Output = crate::Result<T>> + Unpin + 'a,
    {
        Box::pin(async move {
            let reserved = capacity.is_some();
            let (io, ctx, admission) = self.admit(scope, capacity, kind, held)?;
            let mut guard = Waiting {
                reactor: self,
                io,
                admission: Some(admission),
            };
            let mut future = make(&guard.io, &ctx);
            let result = std::future::poll_fn(|cx| {
                let result = Pin::new(&mut future).poll(cx);
                let record = &guard.admission.as_ref().expect("admitted").record;
                if record.id.get().is_none() {
                    record.id.set(id(&future));
                }
                result
            })
            .await;
            drop(future);
            guard.admission.take();
            match result {
                Ok(value) => Ok(value),
                Err(error) => {
                    if error == Error::Overloaded {
                        let partition = self.reserved.borrow().upgrade().map_or(0, |p| p.capacity);
                        let limit = if reserved {
                            partition
                        } else {
                            self.queue_entries - partition
                        };
                        self.rejected(
                            scope,
                            SubmissionRejection {
                                reason: if reserved {
                                    RejectionReason::ReservedFull
                                } else {
                                    RejectionReason::OrdinaryFull
                                },
                                kind,
                                reserved,
                                used: Some(limit),
                                limit: Some(limit),
                                requested: 1,
                            },
                        );
                    }
                    scope.check()?;
                    Err(error.into())
                }
            }
        })
    }

    /// Read at an explicit offset into owned storage, retaining `lease`
    /// (`()` if none) until every completion fence.
    /// ```no_run
    /// use std::rc::Rc;
    /// use racer_io::{Scope, Result, scoped::{descriptor::Descriptor, IoBuffer, Completion, Reactor}};
    /// async fn copy<S: Scope, B: IoBuffer, L: 'static>(
    ///     reactor: &Reactor<S>, fd: Rc<Descriptor>, buffer: B,
    ///     lease: L, scope: &S) -> Result<Completion<B,L>, S::Error>
    /// {
    ///     let read = reactor.read_at(fd.clone(), 0, buffer, lease, scope).await?;
    ///     reactor.write_at(fd, 0, read.buffer, read.lease, scope).await
    /// }
    /// ```
    /// The retained lease cannot borrow from the waiting future's caller:
    /// ```compile_fail
    /// use std::rc::Rc;
    /// use racer_io::{Scope, scoped::{descriptor::Descriptor, IoBuffer, Reactor}};
    /// fn borrowed<S: Scope, B: IoBuffer>(
    ///     reactor: &Reactor<S>, fd: Rc<Descriptor>, buffer: B,
    ///     lease: &u8, scope: &S) {
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
    ) -> Operation<'a, Completion<B, L>, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.read_at(file, offset, buffer, lease, ctx),
            |f| f.id(),
        )
    }

    /// Read at an explicit offset with `IOSQE_ASYNC`, for cold buffered
    /// filesystem reads that must not block the lane inline.
    pub fn read_at_offloaded<'a, B: IoBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.read_at(file, offset, buffer, lease, &ctx.clone().offload()),
            |f| f.id(),
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
    ) -> Operation<'a, Completion<B, L>, S::Error> {
        let write = self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.write_at(file, offset, Exclusive(buffer), lease, ctx),
            |f| f.id(),
        );
        Box::pin(async move {
            let done = write.await?;
            Ok(Completion {
                buffer: done.buffer.0,
                bytes: done.bytes,
                lease: done.lease,
            })
        })
    }

    /// Write immutable shared storage at an explicit file offset, returning
    /// short progress without replaying bytes.
    pub fn write_shared_at<'a, B: SendBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.write_at(file, offset, buffer, lease, ctx),
            |f| f.id(),
        )
    }

    /// Write `header` then `body` contiguously at an explicit offset with one
    /// vectored write, without copying either into a staging buffer. Short
    /// progress is returned as is and may end inside either segment.
    pub fn write_vectored_at<'a, H: IoBuffer, T: SharedWriteBuffer, L: 'static>(
        &'a self,
        file: Rc<Descriptor>,
        offset: u64,
        header: H,
        body: T,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, VectoredCompletion<H, T, L>, S::Error> {
        let write = self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.write_vectored_at(file, offset, Exclusive(header), body, lease, ctx),
            |f| f.id(),
        );
        Box::pin(async move {
            let done = write.await?;
            Ok(VectoredCompletion {
                header: done.header.0,
                body: done.body,
                bytes: done.bytes,
                lease: done.lease,
            })
        })
    }

    /// Receive into owned storage while retaining the caller's lease through fencing.
    pub fn recv<'a, B: IoBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.recv(fd, buffer, lease, ctx),
            |f| f.id(),
        )
    }

    /// Receive using the reserved partition.
    pub fn recv_reserved<'a, B: IoBuffer>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, Completion<B>, S::Error> {
        self.run(
            scope,
            Some(capacity),
            SubmissionKind::OtherIo,
            move |io, ctx| io.recv(fd, buffer, (), ctx),
            |f| f.id(),
        )
    }

    /// Send using the reserved partition.
    pub fn send_reserved<'a, B: IoBuffer>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, Completion<B>, S::Error> {
        let send = self.run(
            scope,
            Some(capacity),
            SubmissionKind::OtherIo,
            move |io, ctx| io.send(fd, Exclusive(buffer), (), ctx),
            |f| f.id(),
        );
        Box::pin(async move {
            let done = send.await?;
            Ok(Completion {
                buffer: done.buffer.0,
                bytes: done.bytes,
                lease: done.lease,
            })
        })
    }

    /// Send owned immutable storage, retaining its lease and suppressing SIGPIPE.
    pub fn send<'a, B: SendBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, Completion<B, L>, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.send(fd, buffer, lease, ctx),
            |f| f.id(),
        )
    }

    /// Receive like [`Self::recv`], also accepting `SCM_RIGHTS` descriptors with
    /// `MSG_CMSG_CLOEXEC` and reporting kernel TLS record types. Simulated
    /// sockets carry no ancillary data and always report none.
    pub fn recv_msg<'a, B: IoBuffer, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        buffer: B,
        lease: L,
        scope: &'a S,
    ) -> Operation<'a, MessageCompletion<B, L>, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| io.recv_msg(fd, buffer, lease, ctx),
            |f| f.id(),
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
        self.run(
            scope,
            None,
            SubmissionKind::Readiness,
            move |io, ctx| io.poll_ready(fd, interest, lease, ctx),
            |f| f.id(),
        )
    }

    /// Wait using the reserved partition, retaining the descriptor and lease until fenced.
    pub fn readiness_reserved<'a, L: 'static>(
        &'a self,
        fd: Rc<Descriptor>,
        interest: u32,
        lease: L,
        capacity: Rc<SubmissionCapacity>,
        scope: &'a S,
    ) -> Operation<'a, u32, S::Error> {
        self.run(
            scope,
            Some(capacity),
            SubmissionKind::Readiness,
            move |io, ctx| io.poll_ready(fd, interest, lease, ctx),
            |f| f.id(),
        )
    }

    /// Accept one nonblocking, close-on-exec socket into a typed owner.
    pub fn accept<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        scope: &'a S,
    ) -> Operation<'a, Descriptor, S::Error> {
        self.accept_reserved(fd, None, scope)
    }

    /// Accept with an optional reserved partition.
    pub fn accept_reserved<'a>(
        &'a self,
        fd: Rc<Descriptor>,
        capacity: Option<Rc<SubmissionCapacity>>,
        scope: &'a S,
    ) -> Operation<'a, Descriptor, S::Error> {
        self.run(
            scope,
            capacity,
            SubmissionKind::OtherIo,
            move |io, ctx| io.accept(fd, ctx),
            |f| f.id(),
        )
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

    /// Return the lease only after the original and any cancellation CQE are fenced.
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
        errno: Option<Rc<Cell<Option<i32>>>>,
        scope: &'a S,
    ) -> Operation<'a, L, S::Error> {
        self.run(
            scope,
            None,
            SubmissionKind::OtherIo,
            move |io, ctx| ObservedConnect {
                inner: io.connect(fd, address, lease, ctx),
                errno,
            },
            |f| f.inner.id(),
        )
    }

    /// Process at most `budget` completions and check at most `budget`
    /// in-flight scopes, canceling operations whose scope failed. Returns the
    /// completions consumed.
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        if budget == 0 {
            return Ok(0);
        }
        let Some(io) = self.io.get() else {
            return Ok(0);
        };
        // Completions first, then scope checks: a cancel requested by this
        // scan is reaped on a later turn, as with any other submission.
        let reaped = io.reap(budget)?;
        self.scan(io, budget);
        Ok(reaped)
    }

    /// Drop finished records and cancel up to `budget` operations whose
    /// scope check fails. Checks run outside the records borrow.
    fn scan(&self, io: &crate::Reactor, budget: usize) {
        let mut checks = Vec::new();
        self.records.borrow_mut().retain(|record| {
            let waiting = Rc::strong_count(record) > 1;
            match record.id.get() {
                None => waiting,
                Some(id) if io.is_fenced(id) => waiting,
                Some(id) => {
                    if checks.len() < budget {
                        checks.push((id, record.clone()));
                    }
                    true
                }
            }
        });
        self.release_parked(io);
        for (id, record) in checks {
            if record.scope.check().is_err() {
                io.cancel_op(id, Error::Canceled);
            }
        }
    }

    /// Release admissions of abandoned operations that have fenced. Guards
    /// drop outside the borrow, since budget hooks may reenter.
    fn release_parked(&self, io: &crate::Reactor) {
        let released: Vec<_> = {
            let mut parked = self.parked.borrow_mut();
            if parked.is_empty() {
                return;
            }
            let mut released = Vec::new();
            let mut index = 0;
            while index < parked.len() {
                if io.is_fenced(parked[index].0) {
                    released.push(parked.swap_remove(index));
                } else {
                    index += 1;
                }
            }
            released
        };
        drop(released);
    }

    /// Sleep until a completion or external wake, bounded by `duration` and a
    /// short fallback for producers that cannot attach the wake endpoint.
    pub fn wait(&self, duration: Duration) -> Result<()> {
        let Some(io) = self.io.get() else {
            return Ok(());
        };
        if !duration.is_zero() {
            self.scan(io, usize::MAX);
        }
        if duration.is_zero() || io.is_sim() {
            return io.park(Some(io.now()));
        }
        // In-flight scopes whose checks only a scan observes need a bounded
        // park; otherwise completions, wakes, and the ring's own deadlines
        // and timers end it.
        let duration = if self.needs_scan() {
            duration.min(MAX_WAIT)
        } else {
            duration
        };
        let until = [io.now().checked_add(duration), io.next_deadline()]
            .into_iter()
            .flatten()
            .min();
        io.park(until)
    }

    /// Whether some in-flight operation can fail its scope check without its
    /// operation context's cancel token or deadline firing.
    fn needs_scan(&self) -> bool {
        let foreign = self.foreign_clock.get();
        self.records
            .borrow()
            .iter()
            .any(|record| record.id.get().is_some() && (foreign || record.scope.polled_checks()))
    }

    /// Close admission and cancel outstanding work. The worker must keep
    /// driving [`Self::poll_budgeted`] while awaiting this fence.
    pub fn drain(&self) -> Operation<'_, (), S::Error> {
        Box::pin(async move {
            let fence = self.io.get().map(crate::Reactor::fence_all);
            self.stopped.set(true);
            if let Some(fence) = fence {
                fence.await;
            }
            if let Some(io) = self.io.get() {
                self.release_parked(io);
            }
            Ok(())
        })
    }

    /// Close admission and drive cancellation for at most `timeout` (host time).
    /// An error, including DeadlineExceeded, is never a fence.
    pub fn shutdown(&self, timeout: Duration) -> Result<()> {
        self.stopped.set(true);
        let Some(io) = self.io.get() else {
            return Ok(());
        };
        io.stop();
        let started = Instant::now();
        while io.in_flight() != 0 {
            if started.elapsed() >= timeout {
                return Err(Error::DeadlineExceeded);
            }
            io.reap(256)?;
            if io.in_flight() != 0 {
                if io.is_sim() {
                    io.park(None)?;
                } else {
                    let remaining = timeout.saturating_sub(started.elapsed()).min(MAX_WAIT);
                    io.park(Some(io.now() + remaining))?;
                }
            }
        }
        self.release_parked(io);
        Ok(())
    }

    /// Cancel all current matching operations, then wait for their fences.
    /// Work submitted after the snapshot is not part of this fence.
    pub fn fence_matching<'a>(
        &'a self,
        matches: impl Fn(&S) -> bool + 'a,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            let Some(io) = self.io.get() else {
                return Ok(());
            };
            let records: Vec<_> = self.records.borrow().clone();
            let ids: Vec<_> = records
                .iter()
                .filter(|record| matches(&record.scope))
                .filter_map(|record| record.id.get())
                .filter(|id| !io.is_fenced(*id))
                .collect();
            drop(records);
            for id in &ids {
                io.cancel_op(*id, Error::Canceled);
            }
            io.fence_ids(ids).await;
            self.release_parked(io);
            Ok(())
        })
    }
}

/// Parks an abandoned operation's admission until its kernel fences.
struct Waiting<'a, S: Scope> {
    reactor: &'a Reactor<S>,

    io: crate::Reactor,

    admission: Option<Admission<S>>,
}

impl<S: Scope> Drop for Waiting<'_, S> {
    fn drop(&mut self) {
        let Some(admission) = self.admission.take() else {
            return;
        };
        if let Some(id) = admission.record.id.get()
            && !self.io.is_fenced(id)
        {
            self.reactor.parked.borrow_mut().push((id, admission));
        }
    }
}

/// A connect that records its errno for attribution.
struct ObservedConnect<L: 'static> {
    inner: crate::ring::ops::Connect<L>,

    errno: Option<Rc<Cell<Option<i32>>>>,
}

impl<L: 'static> Future for ObservedConnect<L> {
    type Output = crate::Result<L>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let result = Pin::new(&mut self.inner).poll(cx);
        if let (Poll::Ready(Err(Error::Os(errno))), Some(cell)) = (&result, &self.errno) {
            cell.set(Some(*errno));
        }
        result
    }
}

impl<S: Scope> Drop for Reactor<S> {
    /// Attempt a bounded fence; `racer-io` leaks its slots and ring if work
    /// is still in flight afterwards.
    fn drop(&mut self) {
        if !std::thread::panicking() {
            let _ = self.shutdown(DROP_WAIT);
        }
        self.records.get_mut().clear();
        if self.in_flight() == 0 {
            self.parked.get_mut().clear();
        } else {
            // Unfenced kernel work: keep its owners and charges forever.
            std::mem::forget(std::mem::take(self.parked.get_mut()));
        }
    }
}

pub mod timer {
    //! Timers sharing the reactor's completion fences.
    use super::*;

    impl<S: Scope> Reactor<S> {
        /// Sleep until `until` under the caller's scope. On a simulated
        /// worker clock the sleep polls that clock cooperatively (its owner
        /// advances it); otherwise it waits on the reactor's timer heap.
        pub fn sleep_until<'a>(
            &'a self,
            until: Instant,
            scope: &'a S,
        ) -> Operation<'a, (), S::Error> {
            Box::pin(async move {
                scope.check()?;
                let clock = self.ctx().clock.clone();
                if until <= clock.now() {
                    return Ok(());
                }
                if clock.is_virtual() {
                    return std::future::poll_fn(|cx| {
                        scope.check()?;
                        if clock.now() >= until {
                            Poll::Ready(Ok(()))
                        } else {
                            cx.waker().wake_by_ref();
                            Poll::Pending
                        }
                    })
                    .await;
                }
                if self.env.sim_os().is_some() {
                    return Err(Error::InvalidConfiguration.into());
                }
                let io = self.io()?;
                let ctx = scope.op_ctx();
                let target = match ctx.deadline.instant() {
                    Some(deadline) if !self.foreign_clock.get() => until.min(deadline),
                    _ => until,
                };
                let mut sleep = io.sleep_until(target);
                let mut key = None;
                let result = std::future::poll_fn(|cx| {
                    if let Some(token) = &ctx.cancel
                        && token.register(&mut key, cx.waker()).is_some()
                    {
                        return Poll::Ready(scope.check().and(Err(Error::Canceled.into())));
                    }
                    if let Err(error) = scope.check() {
                        return Poll::Ready(Err(error));
                    }
                    Pin::new(&mut sleep).poll(cx).map(|()| scope.check())
                })
                .await;
                if let Some(token) = &ctx.cancel {
                    token.deregister(&mut key);
                }
                result
            })
        }
    }
}

pub mod descriptor {
    //! Owned descriptors are shared with `racer-io`. These constructors use the
    //! explicitly selected simulated OS, or the kernel.
    pub use crate::ring::Descriptor;
    use crate::sim::SimOs;
    use crate::{Error, Result};

    /// Bind a nonblocking TCP listener on `sim`, or on the kernel when `None`.
    pub fn tcp_listener(sim: Option<&SimOs>, address: std::net::SocketAddr) -> Result<Descriptor> {
        if let Some(sim) = sim {
            return sim
                .listen(super::SocketAddress::Inet(address))
                .map_err(|_| Error::Io);
        }
        Descriptor::tcp_listener(address).map_err(|_| Error::Io)
    }

    /// Create a nonblocking, close-on-exec stream socket on `sim` or the kernel.
    pub fn socket(sim: Option<&SimOs>, domain: i32) -> Result<Descriptor> {
        if let Some(sim) = sim {
            return sim.socket(domain).map_err(|_| Error::Io);
        }
        Descriptor::socket(domain).map_err(|_| Error::Io)
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
            let counter = std::sync::Arc::new(crate::xthread::tests::WakeCounter::default());
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
                set.next(&mut cx, 1, |_| Box::pin(async { Err(Error::Canceled) })),
                Err(Error::Canceled)
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
            use crate::scoped::tests::{drive, kernel_reactor, scope};
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
            request.cancel();
            drop(set);
            assert!(weak.upgrade().is_some());
            drive(&reactor, reactor.fence_matching(|_| true)).unwrap();
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

pub mod fs;

#[cfg(test)]
mod reserved_tests;

#[cfg(test)]
mod tests;

mod retry;
