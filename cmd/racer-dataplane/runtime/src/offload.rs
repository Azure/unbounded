//! Bounded owned offload, with admission reserved through completion consumption.
//!
//! The caller owns execution, payloads, scope policy and result classification.
//! Dropping a waiter abandons delivery, not the accepted job or its credit.

use crate::channel::{self, Receiver, Sender};
use futures::task::AtomicWaker;
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque, btree_map::Entry},
    num::NonZeroUsize,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
};

/// Caller-defined ticket identity. Sequences must increase within a generation.
pub trait Identity: Copy + Ord {
    /// Application owner that may submit work to this pair.
    type Owner: Eq;

    /// Return the application owner associated with this ticket.
    fn owner(self) -> Self::Owner;

    /// Return the pair generation, changed when its sequence space is replaced.
    fn generation(self) -> u64;

    /// Return the monotonically increasing ticket sequence.
    fn sequence(self) -> u64;
}

/// Admission or delivery failure without relinquishing a caller-owned payload.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The owner, generation, sequence, or pair does not match.
    Stale,
    /// The runtime rejected work because of capacity or availability.
    Runtime(crate::Error),
}
impl From<crate::Error> for Error {
    /// Preserve the runtime failure as an offload admission or delivery error.
    fn from(error: crate::Error) -> Self {
        Self::Runtime(error)
    }
}
/// Rejected publication with the original payload still owned by the caller.
pub struct SendFailure<T> {
    /// The payload whose ownership was not transferred.
    pub command: T,

    /// Why publication failed.
    pub error: Error,
}
impl<T> From<channel::SendFailure<T>> for SendFailure<T> {
    /// Translate the error without dropping or replacing the rejected payload.
    fn from(failure: channel::SendFailure<T>) -> Self {
        Self {
            command: failure.command,
            error: failure.error.into(),
        }
    }
}

/// Non-cloneable reservation of submission AND completion space.
/// Keep this after payload fields so their destructors run before credit release.
pub struct Permit<I> {
    handoff: Arc<Handoff<I>>,

    id: I,
}
impl<I: Copy> Permit<I> {
    /// Return the accepted ticket identity without transferring its reservation.
    pub fn id(&self) -> I {
        self.id
    }
}
impl<I> Drop for Permit<I> {
    /// Release one admission credit and notify the sole capacity waiter.
    fn drop(&mut self) {
        self.handoff.outstanding.fetch_sub(1, Ordering::AcqRel);
        self.handoff.capacity_waker.wake();
    }
}

/// Jobs and completions retain their permit until execution and consumption fence
/// their payloads. A completion must inherit the accepted job's exact permit.
pub trait Reserved<I> {
    /// Borrow the exact permit retained by this job or completion.
    fn permit(&self) -> &Permit<I>;
}

/// Unique producer/consumer endpoints, movable to their respective threads.
pub struct ClientPort<I, J, C> {
    handoff: Arc<Handoff<I>>,

    jobs: Sender<J>,

    completions: Receiver<C>,

    last_sequence: Cell<Option<u64>>,
}
/// Unique worker endpoint that receives jobs and publishes their completions.
pub struct WorkerPort<I, J, C> {
    handoff: Arc<Handoff<I>>,

    jobs: Option<Receiver<J>>,

    completions: Sender<C>,
}

/// Paired client and worker endpoints sharing fixed admission capacity.
type PortPair<I, J, C> = (ClientPort<I, J, C>, WorkerPort<I, J, C>);

/// Allocate both fixed-capacity queues without starting a service. The sequence
/// in `identity` is ignored; the first reservation may use any sequence.
pub fn try_pair<I: Identity, J: Reserved<I>, C: Reserved<I>>(
    identity: I,
    capacity: NonZeroUsize,
) -> crate::Result<PortPair<I, J, C>> {
    let (jobs_tx, jobs_rx) = channel::bounded(capacity.get())?;
    let (results_tx, results_rx) = channel::bounded(capacity.get())?;
    let handoff = Arc::new(Handoff {
        identity,
        capacity,
        outstanding: AtomicUsize::new(0),
        closed: AtomicBool::new(false),
        capacity_waker: AtomicWaker::new(),
        worker_waker: AtomicWaker::new(),
        client_waker: AtomicWaker::new(),
    });
    Ok((
        ClientPort {
            handoff: handoff.clone(),
            jobs: jobs_tx,
            completions: results_rx,
            last_sequence: Cell::new(None),
        },
        WorkerPort {
            handoff,
            jobs: Some(jobs_rx),
            completions: results_tx,
        },
    ))
}

impl<I: Identity, J: Reserved<I>, C: Reserved<I>> ClientPort<I, J, C> {
    /// Return the pair identity; its initial sequence is not a reservation.
    pub fn identity(&self) -> I {
        self.handoff.identity
    }
    /// Return the joint limit on jobs and unconsumed completions.
    pub fn capacity(&self) -> usize {
        self.handoff.capacity.get()
    }
    /// Count permits still retained by callers, accepted jobs, or completions.
    pub fn outstanding(&self) -> usize {
        self.handoff.outstanding.load(Ordering::Acquire)
    }
    /// Whether reservations and submissions have permanently closed.
    pub fn submissions_closed(&self) -> bool {
        self.handoff.closed.load(Ordering::Acquire)
    }
    /// Whether the worker closed completion publication, not an ownership fence.
    pub fn completions_closed(&self) -> bool {
        self.completions.is_closed()
    }
    /// Replace the local driver's completion and worker-loss notification waker.
    pub fn register_driver(&self, waker: &Waker) {
        self.handoff.client_waker.register(waker);
    }
    /// Replace the sole capacity waiter's notification waker before checking it.
    pub fn register_capacity(&self, waker: &Waker) {
        self.handoff.capacity_waker.register(waker);
    }
    /// Register before checking capacity. Pending does not consume the sequence.
    pub fn poll_reserve(&self, cx: &mut Context<'_>, id: I) -> Poll<Result<Permit<I>, Error>> {
        self.register_capacity(cx.waker());
        if self.submissions_closed() {
            return Poll::Ready(Err(crate::Error::Unavailable.into()));
        }
        if id.owner() != self.handoff.identity.owner()
            || id.generation() != self.handoff.identity.generation()
            || self
                .last_sequence
                .get()
                .is_some_and(|last| id.sequence() <= last)
        {
            return Poll::Ready(Err(Error::Stale));
        }
        if self
            .handoff
            .outstanding
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |n| {
                (n < self.capacity()).then_some(n + 1)
            })
            .is_err()
        {
            return Poll::Pending;
        }
        self.last_sequence.set(Some(id.sequence()));
        Poll::Ready(Ok(Permit {
            handoff: self.handoff.clone(),
            id,
        }))
    }
    /// The hook runs only after pair/close validation, immediately before the
    /// publication attempt, so caller timing excludes reservation waits.
    pub fn try_submit(
        &self,
        mut job: J,
        before_publish: impl FnOnce(&mut J),
    ) -> Result<(), SendFailure<J>> {
        if !Arc::ptr_eq(&self.handoff, &job.permit().handoff) || self.submissions_closed() {
            return Err(SendFailure {
                command: job,
                error: crate::Error::Unavailable.into(),
            });
        }
        before_publish(&mut job);
        self.jobs.try_send(job)?;
        self.handoff.worker_waker.wake();
        Ok(())
    }
    /// Poll the sole completion waiter; `None` means publication closed and drained.
    pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<crate::Result<Option<C>>> {
        self.completions.poll_receive_shared(cx)
    }
    /// Take one completion without waiting; `None` may mean temporarily empty.
    pub fn receive(&self) -> crate::Result<Option<C>> {
        self.completions.receive_shared()
    }
    /// Discard orphaned jobs only after the unique worker receiver is destroyed.
    pub fn discard_closed(&self) {
        if self.completions_closed() {
            self.jobs.discard_closed();
        }
    }
    /// Permanently reject new work while leaving accepted completions readable.
    pub fn close_submissions(&self) {
        self.handoff.closed.store(true, Ordering::Release);
        self.jobs.close();
        self.handoff.worker_waker.wake();
        self.handoff.capacity_waker.wake();
    }
}
impl<I: Identity, J: Reserved<I>, C: Reserved<I>> WorkerPort<I, J, C> {
    /// Replace the worker driver's submission and closure notification waker.
    pub fn register_driver(&self, waker: &Waker) {
        self.handoff.worker_waker.register(waker);
    }
    /// None means closed and all accepted jobs consumed, not temporarily empty.
    pub fn poll_job(&mut self, cx: &mut Context<'_>) -> Poll<crate::Result<Option<J>>> {
        self.jobs
            .as_mut()
            .expect("live worker endpoint")
            .poll_receive(cx)
    }
    /// Publish a completion retaining its original permit, or return it unchanged.
    pub fn complete(&mut self, completion: C) -> Result<(), SendFailure<C>> {
        if !Arc::ptr_eq(&self.handoff, &completion.permit().handoff) {
            return Err(SendFailure {
                command: completion,
                error: Error::Stale,
            });
        }
        self.completions.try_send(completion)?;
        self.handoff.client_waker.wake();
        Ok(())
    }
}
impl<I, J, C> Drop for WorkerPort<I, J, C> {
    /// Close admission and reclaim queued jobs before announcing completion EOF.
    fn drop(&mut self) {
        self.handoff.closed.store(true, Ordering::Release);
        // Destroy queued payloads before announcing completion EOF.
        if let Some(mut jobs) = self.jobs.take() {
            while let Ok(Some(job)) = jobs.receive() {
                drop(job);
            }
        }
        self.completions.close();
        self.handoff.client_waker.wake();
        self.handoff.capacity_waker.wake();
    }
}

/// Worker-local bounded FIFO before ownership is accepted by an offload port.
/// A separate bound on pending admissions prevents unbounded parked futures.
/// Scopes are opaque: callers decide which ones need cancellation/expiry wakes.
pub struct AdmissionQueue<S> {
    capacity: NonZeroUsize,

    sequence: Cell<u64>,

    pending: RefCell<VecDeque<Pending<S>>>,

    cursor: Cell<usize>,
}
impl<S> AdmissionQueue<S> {
    /// Bound the number of waiting futures independently of accepted work.
    pub fn new(capacity: NonZeroUsize) -> Self {
        Self {
            capacity,
            sequence: Cell::new(0),
            pending: RefCell::new(VecDeque::new()),
            cursor: Cell::new(0),
        }
    }
    /// Count futures waiting for admission, excluding accepted work.
    pub fn len(&self) -> usize {
        self.pending.borrow().len()
    }
    /// Whether no futures are waiting for admission.
    pub fn is_empty(&self) -> bool {
        self.pending.borrow().is_empty()
    }
    /// Admission consumes a sequence even if the guard is subsequently dropped.
    /// Saturation consumes neither a queue slot nor a sequence. Overflow requires
    /// draining and a new pair generation, never sequence reuse.
    pub fn enter(&self, scope: S, cx: &Context<'_>) -> crate::Result<CapacityWaiter<'_, S>> {
        // RawWaker clone/drop are caller code, just like wake. Snapshot before
        // borrowing, then validate capacity/sequence after any reentrant clone.
        let waker = Rc::new(cx.waker().clone());
        let scope = Rc::new(scope);
        let mut pending = self.pending.borrow_mut();
        if pending.len() >= self.capacity.get() {
            return Err(crate::Error::Overloaded);
        }
        let sequence = self
            .sequence
            .get()
            .checked_add(1)
            .ok_or(crate::Error::Unavailable)?;
        self.sequence.set(sequence);
        pending.push_back(Pending {
            sequence,
            waker,
            scope,
        });
        Ok(CapacityWaiter {
            queue: self,
            sequence,
        })
    }
    /// Snapshot at most budget entries, rotating across turns. Predicates and all
    /// waker callbacks run outside the borrow and may reenter this queue.
    pub fn wake_if(&self, budget: usize, mut predicate: impl FnMut(&S) -> bool) {
        let candidates = {
            let pending = self.pending.borrow();
            let mut wakes = Vec::new();
            for _ in 0..budget.min(pending.len()) {
                let index = self.cursor.get() % pending.len();
                self.cursor.set(index + 1);
                let entry = &pending[index];
                wakes.push((entry.scope.clone(), entry.waker.clone()));
            }
            wakes
        };
        for (scope, wake) in candidates {
            if predicate(&scope) {
                wake.wake_by_ref();
            }
        }
    }
}

/// Drop removes only pre-admission waiting state, never an accepted job/permit.
/// Drop this guard after successful reservation, before publishing the job.
pub struct CapacityWaiter<'a, S> {
    queue: &'a AdmissionQueue<S>,

    sequence: u64,
}
impl<S> CapacityWaiter<'_, S> {
    /// Return this admission attempt's unique sequence, never reused on drop.
    pub fn sequence(&self) -> u64 {
        self.sequence
    }
    /// Only the head may register for port capacity; later waiters cannot steal
    /// its wake or overtake it. Check caller cancellation before invoking this.
    pub fn poll<T>(
        &self,
        cx: &mut Context<'_>,
        reserve: impl FnOnce(&mut Context<'_>) -> Poll<T>,
    ) -> Poll<T> {
        let waker = Rc::new(cx.waker().clone());
        let mut pending = self.queue.pending.borrow_mut();
        let entry = pending
            .iter_mut()
            .find(|entry| entry.sequence == self.sequence)
            .expect("live capacity waiter");
        let old = std::mem::replace(&mut entry.waker, waker);
        let head = pending
            .front()
            .is_none_or(|entry| entry.sequence == self.sequence);
        drop(pending);
        drop(old);
        if !head {
            return Poll::Pending;
        }
        reserve(cx)
    }
}
impl<S> Drop for CapacityWaiter<'_, S> {
    /// Remove this FIFO entry and wake its successor outside the queue borrow.
    fn drop(&mut self) {
        let (removed, wake) = {
            let mut pending = self.queue.pending.borrow_mut();
            let removed = pending
                .iter()
                .position(|entry| entry.sequence == self.sequence)
                .and_then(|index| pending.remove(index));
            (removed, pending.front().map(|entry| entry.waker.clone()))
        };
        drop(removed);
        if let Some(wake) = wake {
            wake.wake_by_ref();
        }
    }
}

/// Worker-local delivery registry. Callers register only after admission; the
/// retained completion's permit continues charging capacity until consumption.
pub struct Waiters<I, C> {
    entries: RefCell<BTreeMap<I, Waiter<C>>>,

    cursor: Cell<Option<I>>,

    generation: Cell<u64>,
}
/// Dropping the waiting future abandons delivery; accepted owners stay fenced by
/// their permits until completion reap or worker loss, independently of this guard.
pub struct Registration<'a, I: Copy + Ord, C> {
    waiters: &'a Waiters<I, C>,

    id: I,

    generation: u64,
}
impl<I: Copy + Ord, C> Drop for Registration<'_, I, C> {
    /// Abandon only the registered generation, leaving any replacement untouched.
    fn drop(&mut self) {
        self.waiters
            .abandon_generation(self.id, Some(self.generation));
    }
}
impl<I: Copy + Ord, C> Default for Waiters<I, C> {
    /// Initialize an empty local registry with an unused generation sequence.
    fn default() -> Self {
        Self {
            entries: RefCell::new(BTreeMap::new()),
            cursor: Cell::new(None),
            generation: Cell::new(0),
        }
    }
}
impl<I: Copy + Ord, C> Waiters<I, C> {
    /// Count registered accepted jobs, including abandoned but unfenced jobs.
    pub fn len(&self) -> usize {
        self.entries.borrow().len()
    }
    /// Whether there are no retained delivery registrations.
    pub fn is_empty(&self) -> bool {
        self.entries.borrow().is_empty()
    }
    /// Insert a unique accepted identity and return its exact registration generation.
    /// Duplicate live registrations are rejected, never replaced; exhausted
    /// generations fail before inserting any entry.
    fn try_register(&self, id: I, waker: &Waker) -> crate::Result<u64> {
        let waker = Rc::new(waker.clone());
        let mut entries = self.entries.borrow_mut();
        let Entry::Vacant(entry) = entries.entry(id) else {
            return Err(crate::Error::AlreadyExists);
        };
        let generation = self
            .generation
            .get()
            .checked_add(1)
            .ok_or(crate::Error::Unavailable)?;
        self.generation.set(generation);
        entry.insert(Waiter {
            generation,
            waker,
            abandoned: false,
            result: None,
        });
        Ok(generation)
    }
    /// Register a unique identity, treating reuse as caller misuse.
    #[cfg(test)]
    fn register(&self, id: I, waker: &Waker) {
        self.try_register(id, waker)
            .expect("unique waiter registration");
    }
    /// Register accepted work and return a guard that abandons only this generation.
    /// Panics on a duplicate live identity or exhausted registration generations.
    pub fn register_guard(&self, id: I, waker: &Waker) -> Registration<'_, I, C> {
        let generation = self
            .try_register(id, waker)
            .expect("unique waiter registration");
        Registration {
            waiters: self,
            id,
            generation,
        }
    }
    /// Remove a registration after rejected submission, before any acceptance.
    pub fn remove(&self, id: I) {
        let removed = self.entries.borrow_mut().remove(&id);
        drop(removed);
    }
    /// Abandon an identity directly when no generation guard is involved.
    #[cfg(test)]
    fn abandon(&self, id: I) {
        self.abandon_generation(id, None);
    }
    /// Stop delivery only for the matching generation, retaining unfenced owners.
    fn abandon_generation(&self, id: I, generation: Option<u64>) {
        let mut entries = self.entries.borrow_mut();
        let mut removed = None;
        if let Some(waiter) = entries.get_mut(&id) {
            if generation.is_some_and(|generation| generation != waiter.generation) {
                return;
            }
            if waiter.result.is_some() {
                removed = entries.remove(&id);
            } else {
                waiter.abandoned = true;
            }
        }
        drop(entries);
        drop(removed);
    }
    /// A missing registration or abandoned completed result is canceled. Scope
    /// checks belong after Ready: cancellation cannot bypass the completion fence.
    pub fn poll_result(&self, id: I, cx: &mut Context<'_>) -> Poll<crate::Result<C>> {
        let waker = Rc::new(cx.waker().clone());
        let mut entries = self.entries.borrow_mut();
        let Some(waiter) = entries.get_mut(&id) else {
            return Poll::Ready(Err(crate::Error::Cancelled));
        };
        let old = std::mem::replace(&mut waiter.waker, waker);
        if let Some(result) = waiter.result.take() {
            let abandoned = waiter.abandoned;
            let removed = entries.remove(&id);
            drop(entries);
            drop((old, removed));
            if abandoned {
                Poll::Ready(Err(crate::Error::Cancelled))
            } else {
                Poll::Ready(Ok(result))
            }
        } else {
            drop(entries);
            drop(old);
            Poll::Pending
        }
    }
    /// Unknown or abandoned completions are dropped, never delivered to a new ID.
    pub fn deliver(&self, id: I, completion: C) {
        let mut completion = Some(completion);
        let mut removed = None;
        let wake = {
            let mut entries = self.entries.borrow_mut();
            if let Some(waiter) = entries.get_mut(&id) {
                if waiter.abandoned {
                    removed = entries.remove(&id);
                    None
                } else if waiter.result.is_some() {
                    // First completion wins. Never overwrite an unread owner.
                    None
                } else {
                    let wake = waiter.waker.clone();
                    waiter.result = completion.take();
                    Some(wake)
                }
            } else {
                None
            }
        };
        let wake = wake.or_else(|| removed.as_ref().map(|waiter| waiter.waker.clone()));
        drop((removed, completion));
        if let Some(wake) = wake {
            wake.wake_by_ref();
        }
    }
    /// Only call with fenced=true after worker loss AND zero outstanding owners.
    /// Bounded round-robin wakes let live futures observe worker loss themselves.
    pub fn worker_closed(&self, budget: usize, fenced: bool) {
        use std::ops::Bound::{Excluded, Unbounded};
        let mut abandoned = Vec::new();
        let wakes: Vec<_> = {
            let entries = self.entries.borrow();
            let start = self.cursor.get().map_or(Unbounded, Excluded);
            let mut wakes = Vec::new();
            for (id, waiter) in entries
                .range((start, Unbounded))
                .chain(entries.iter())
                .take(budget.min(entries.len()))
            {
                self.cursor.set(Some(*id));
                if !waiter.abandoned {
                    wakes.push(waiter.waker.clone());
                } else if fenced {
                    abandoned.push(*id);
                }
            }
            wakes
        };
        for id in abandoned {
            self.remove(id);
        }
        for wake in wakes {
            wake.wake_by_ref();
        }
    }
    /// Cancel delivery, releasing only completions already fenced by execution.
    pub fn abandon_all(&self) {
        // Extract in one ordered pass, but keep all payloads alive until the
        // borrow ends. Their destructors may reenter and mutate this registry.
        let removed: Vec<_> = self
            .entries
            .borrow_mut()
            .extract_if(.., |_, waiter| {
                waiter.abandoned = true;
                waiter.result.is_some()
            })
            .collect();
        let wakes: Vec<_> = removed
            .iter()
            .map(|(_, waiter)| waiter.waker.clone())
            .collect();
        drop(removed);
        for wake in wakes {
            wake.wake_by_ref();
        }
    }
}

/// Pair-wide accounting retained independently by both endpoints and every permit.
struct Handoff<I> {
    identity: I,

    capacity: NonZeroUsize,

    outstanding: AtomicUsize,

    closed: AtomicBool,

    capacity_waker: AtomicWaker,

    worker_waker: AtomicWaker,

    client_waker: AtomicWaker,
}

/// A pre-admission waiter whose callbacks are snapshotted outside queue borrows.
struct Pending<S> {
    sequence: u64,

    waker: Rc<Waker>,

    scope: Rc<S>,
}

/// Delivery state for one accepted identity, independent of its waiting future.
struct Waiter<C> {
    generation: u64,

    waker: Rc<Waker>,

    abandoned: bool,

    result: Option<C>,
}

/// Admission, delivery, reentry, and owner-lifetime state-space regressions.
#[cfg(test)]
mod tests {
    use super::*;

    /// Bulk abandonment detaches every completed owner before any destructor runs.
    #[test]
    fn abandon_all_detaches_completed_entries_before_reentrant_destruction() {
        /// Completion whose destructor verifies batch removal before mutating reentry.
        struct Reenter {
            id: u64,

            waiters: std::rc::Weak<Waiters<u64, Reenter>>,

            dropped: Rc<RefCell<Vec<u64>>>,
        }
        impl Drop for Reenter {
            /// Check retained unfenced work and reenter the now-unborrowed registry.
            fn drop(&mut self) {
                let waiters = self.waiters.upgrade().unwrap();
                assert_eq!(waiters.len(), 1, "only unfenced work may remain");
                assert!(waiters.entries.borrow().get(&3).unwrap().abandoned);
                self.dropped.borrow_mut().push(self.id);
                waiters.register(99, Waker::noop());
                waiters.remove(99);
            }
        }
        let waiters = Rc::new(Waiters::default());
        let dropped = Rc::new(RefCell::new(Vec::new()));
        for id in [3, 2, 1] {
            waiters.register(id, Waker::noop());
            if id != 3 {
                waiters.deliver(
                    id,
                    Reenter {
                        id,
                        waiters: Rc::downgrade(&waiters),
                        dropped: dropped.clone(),
                    },
                );
            }
        }
        waiters.abandon_all();
        assert_eq!(*dropped.borrow(), [1, 2]);
        waiters.abandon_all();
        assert_eq!(*dropped.borrow(), [1, 2], "repeated abandonment is inert");
        assert_eq!(waiters.len(), 1, "unfenced work remains registered");
    }

    thread_local! {
        static CALLBACK: RefCell<Option<Box<dyn Fn()>>> = RefCell::new(None);
    }
    /// Run the current thread's raw-waker callback, if installed.
    fn callback() {
        CALLBACK.with(|callback| {
            if let Some(callback) = &*callback.borrow() {
                callback();
            }
        });
    }
    /// Build a stateless raw waker that inspects only the executing thread's hook.
    fn callback_waker() -> Waker {
        use std::task::{RawWaker, RawWakerVTable};
        /// Invoke the clone hook and return another stateless raw waker.
        unsafe fn clone(_: *const ()) -> RawWaker {
            callback();
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        /// Invoke the hook for owned wakes, borrowed wakes, or destruction.
        unsafe fn wake(_: *const ()) {
            callback();
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, wake, wake, wake);
        unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) }
    }
    /// Clear the installed hook when its test scope ends.
    struct CallbackGuard;
    impl Drop for CallbackGuard {
        /// Destroy the hook after releasing the thread-local mutable borrow.
        fn drop(&mut self) {
            CALLBACK.with(|hook| {
                let old = hook.borrow_mut().take();
                drop(old);
            });
        }
    }
    /// Install a thread-local callback for the duration of the returned guard.
    fn on_callback(hook: impl Fn() + 'static) -> CallbackGuard {
        CALLBACK.with(|callback| *callback.borrow_mut() = Some(Box::new(hook)));
        CallbackGuard
    }

    /// Queue callbacks and rejected scopes are destroyed outside admission borrows.
    #[test]
    fn admission_raw_waker_and_scope_destructors_reenter_without_borrows() {
        /// Scope that invokes the callback and counts its destruction.
        struct ScopeHook(Rc<Cell<usize>>);
        impl Drop for ScopeHook {
            /// Reenter the queue before recording scope release.
            fn drop(&mut self) {
                callback();
                self.0.set(self.0.get() + 1);
            }
        }
        let queue = Rc::new(AdmissionQueue::new(NonZeroUsize::new(2).unwrap()));
        let reenter = queue.clone();
        let calls = Rc::new(Cell::new(0));
        let count = calls.clone();
        let _hook = on_callback(move || {
            assert!(reenter.pending.try_borrow_mut().is_ok());
            reenter.wake_if(0, |_| false);
            count.set(count.get() + 1);
        });
        let drops = Rc::new(Cell::new(0));
        let waker = callback_waker();
        let mut cx = Context::from_waker(&waker);
        let first = queue.enter(ScopeHook(drops.clone()), &cx).unwrap();
        let second = queue.enter(ScopeHook(drops.clone()), &cx).unwrap();
        assert!(matches!(
            queue.enter(ScopeHook(drops.clone()), &cx),
            Err(crate::Error::Overloaded)
        ));
        assert!(first.poll(&mut cx, |_| Poll::<()>::Pending).is_pending());
        assert!(
            second
                .poll(&mut cx, |_| -> Poll<()> { panic!("tail reserved") })
                .is_pending()
        );
        queue.wake_if(2, |_| {
            callback();
            true
        });
        drop(first);
        drop(second);
        assert!(queue.is_empty());
        assert_eq!(drops.get(), 3);
        assert!(calls.get() >= 10, "clone/drop/wake hooks must all execute");
    }

    /// Registry operations release their borrow before raw-waker callbacks run.
    #[test]
    fn registry_raw_waker_clone_drop_and_wake_allow_mutating_reentry() {
        let waiters = Rc::new(Waiters::<u64, ()>::default());
        let reenter = waiters.clone();
        let calls = Rc::new(Cell::new(0));
        let count = calls.clone();
        let _hook = on_callback(move || {
            reenter.register(99, futures::task::noop_waker_ref());
            reenter.remove(99);
            count.set(count.get() + 1);
        });
        let waker = callback_waker();
        let mut cx = Context::from_waker(&waker);
        waiters.register(1, &waker);
        assert_eq!(
            waiters.try_register(1, &waker),
            Err(crate::Error::AlreadyExists)
        );
        assert!(waiters.poll_result(1, &mut cx).is_pending());
        waiters.deliver(1, ());
        assert_eq!(waiters.poll_result(1, &mut cx), Poll::Ready(Ok(())));
        waiters.register(2, &waker);
        waiters.worker_closed(1, false);
        waiters.abandon(2);
        waiters.deliver(2, ());
        waiters.register(3, &waker);
        waiters.deliver(3, ());
        waiters.abandon_all();
        assert!(waiters.is_empty());
        assert!(calls.get() >= 10);
    }

    /// Completion polling and consumption remain ownership-safe under callback reentry.
    #[test]
    fn client_completion_callbacks_can_receive_and_poll_reentrantly() {
        let (client, mut worker) = pair(7, 2);
        let client = Rc::new(client);
        let reenter = client.clone();
        let calls = Rc::new(Cell::new(0));
        let count = calls.clone();
        let _hook = on_callback(move || {
            // Nested receive may consume a different completion; the outer receive
            // has already advanced its cursor before waking the producer.
            drop(reenter.receive().unwrap());
            let _ =
                reenter.poll_completion(&mut Context::from_waker(futures::task::noop_waker_ref()));
            count.set(count.get() + 1);
        });
        let waker = callback_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(client.poll_completion(&mut cx).is_pending());
        let drops = Arc::new(AtomicUsize::new(0));
        for sequence in 1..=2 {
            assert!(worker.complete(message(&client, sequence, &drops)).is_ok());
        }
        // Register a producer capacity callback directly to cover receive's wake.
        let _ = worker.completions.poll_ready(&mut cx);
        drop(client.receive().unwrap());
        assert!(calls.get() > 0);
        assert_eq!(client.outstanding(), 0);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    /// Duplicate work preserves the first owner and old guards cannot abandon replacements.
    #[test]
    fn duplicate_registration_and_delivery_preserve_first_owner_and_stale_guard_is_inert() {
        let waiters = Waiters::<u64, usize>::default();
        let waker = futures::task::noop_waker_ref();
        let old = waiters.register_guard(1, waker);
        assert_eq!(
            waiters.try_register(1, waker),
            Err(crate::Error::AlreadyExists)
        );
        waiters.deliver(1, 7);
        waiters.deliver(1, 9);
        let mut cx = Context::from_waker(waker);
        assert_eq!(waiters.poll_result(1, &mut cx), Poll::Ready(Ok(7)));
        let current = waiters.register_guard(1, waker);
        drop(old);
        waiters.deliver(1, 11);
        assert_eq!(waiters.poll_result(1, &mut cx), Poll::Ready(Ok(11)));
        drop(current);
    }

    /// Registration returns the inserted generation and exhaustion leaves no new entry.
    #[test]
    fn registration_generation_is_exact_and_exhaustion_is_atomic() {
        let waiters = Waiters::<u64, ()>::default();
        waiters.generation.set(u64::MAX - 2);
        let first = waiters.register_guard(1, Waker::noop());
        assert_eq!(first.generation, u64::MAX - 1);
        assert_eq!(
            waiters.try_register(1, Waker::noop()),
            Err(crate::Error::AlreadyExists)
        );
        assert_eq!(waiters.generation.get(), first.generation);
        let last = waiters.register_guard(2, Waker::noop());
        assert_eq!(last.generation, u64::MAX);
        for _ in 0..2 {
            assert_eq!(
                waiters.try_register(3, Waker::noop()),
                Err(crate::Error::Unavailable)
            );
            assert_eq!(waiters.generation.get(), u64::MAX);
            assert_eq!(waiters.len(), 2);
        }
        drop((first, last));
        assert_eq!(
            waiters.len(),
            2,
            "abandonment retains unfenced registrations"
        );
        waiters.deliver(1, ());
        waiters.deliver(2, ());
        assert!(waiters.is_empty());
    }

    /// Worker-loss inspection spends its budget on every visited entry, not only matches.
    #[test]
    fn worker_loss_bounds_inspection_not_just_matching_removals() {
        let waiters = Waiters::<u64, ()>::default();
        let (count, waker) = wake_counter();
        for id in 0..100 {
            waiters.register(id, &waker);
        }
        waiters.abandon(99);
        waiters.worker_closed(0, true);
        assert_eq!(waiters.cursor.get(), None);
        for id in 0..99 {
            waiters.worker_closed(1, true);
            assert_eq!(waiters.cursor.get(), Some(id));
            assert_eq!(waiters.len(), 100);
        }
        assert_eq!(count.0.load(Ordering::SeqCst), 99);
        waiters.worker_closed(1, true);
        assert_eq!(waiters.len(), 99);
    }

    /// Discarded completion destructors may safely mutate the delivery registry.
    #[test]
    fn result_destructors_can_reenter_delivery_registry() {
        use std::rc::{Rc, Weak};
        /// Completion that removes an unrelated registry entry on destruction.
        struct Reenter(Weak<Waiters<u64, Reenter>>);
        impl Drop for Reenter {
            /// Reenter the registry after the outer operation releases its borrow.
            fn drop(&mut self) {
                self.0.upgrade().unwrap().remove(99);
            }
        }
        let waiters = Rc::new(Waiters::default());
        let waker = futures::task::noop_waker_ref();
        waiters.register(1, waker);
        waiters.deliver(1, Reenter(Rc::downgrade(&waiters)));
        waiters.deliver(1, Reenter(Rc::downgrade(&waiters)));
        waiters.abandon_all();
        assert!(waiters.is_empty());
    }

    /// Atomic wake counter whose value tests may reset between lifecycle transitions.
    struct WakeCount(AtomicUsize);
    impl std::task::Wake for WakeCount {
        /// Record an owned wake notification.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
    /// Return shared wake instrumentation and a waker using that counter.
    fn wake_counter() -> (Arc<WakeCount>, Waker) {
        let count = Arc::new(WakeCount(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        (count, waker)
    }

    /// Only the FIFO head reserves capacity, and dropping its guard leaves credit owned.
    #[test]
    fn admission_fifo_refreshes_head_wake_and_drop_unblocks_next_without_releasing_credit() {
        let (client, _worker) = pair(7, 1);
        let blocker = reserve(&client, 0);
        let pending = AdmissionQueue::new(NonZeroUsize::new(2).unwrap());
        let (old, old_wake) = wake_counter();
        let (head, head_wake) = wake_counter();
        let (next, next_wake) = wake_counter();
        let mut old_cx = Context::from_waker(&old_wake);
        let mut head_cx = Context::from_waker(&head_wake);
        let mut next_cx = Context::from_waker(&next_wake);
        let first = pending.enter((), &old_cx).unwrap();
        let second = pending.enter((), &next_cx).unwrap();
        assert!(matches!(
            pending.enter((), &next_cx),
            Err(crate::Error::Overloaded)
        ));
        assert_eq!(pending.sequence.get(), 2);
        assert!(
            first
                .poll(&mut old_cx, |cx| client
                    .poll_reserve(cx, Id(1, 7, first.sequence())))
                .is_pending()
        );
        assert!(
            first
                .poll(&mut head_cx, |cx| client
                    .poll_reserve(cx, Id(1, 7, first.sequence())))
                .is_pending()
        );
        assert!(
            second
                .poll(&mut next_cx, |_| -> Poll<()> { panic!("FIFO overtaken") })
                .is_pending()
        );
        drop(blocker);
        assert_eq!(old.0.load(Ordering::SeqCst), 0);
        assert_eq!(head.0.swap(0, Ordering::SeqCst), 1);
        assert_eq!(next.0.load(Ordering::SeqCst), 0);
        let permit = match first.poll(&mut head_cx, |cx| {
            client.poll_reserve(cx, Id(1, 7, first.sequence()))
        }) {
            Poll::Ready(Ok(permit)) => permit,
            _ => panic!("head must reserve"),
        };
        drop(first);
        assert_eq!(next.0.swap(0, Ordering::SeqCst), 1);
        assert_eq!(
            client.outstanding(),
            1,
            "FIFO guard never owns accepted credit"
        );
        assert!(
            second
                .poll(&mut next_cx, |cx| client
                    .poll_reserve(cx, Id(1, 7, second.sequence())))
                .is_pending()
        );
        drop(permit);
        assert_eq!(next.0.swap(0, Ordering::SeqCst), 1);
        assert!(matches!(
            second.poll(&mut next_cx, |cx| client
                .poll_reserve(cx, Id(1, 7, second.sequence()))),
            Poll::Ready(Ok(_))
        ));
        drop(second);
        assert!(pending.is_empty());
    }

    /// Cancellation and drop remove only their own pending entry without reusing sequences.
    #[test]
    fn pending_future_cancellation_and_drop_remove_only_their_fifo_entry() {
        use std::{future::Future, rc::Rc};
        let pending = AdmissionQueue::new(NonZeroUsize::new(2).unwrap());
        let canceled = Rc::new(Cell::new(false));
        let (first_count, first_wake) = wake_counter();
        let (next_count, next_wake) = wake_counter();
        let mut first_cx = Context::from_waker(&first_wake);
        let mut next_cx = Context::from_waker(&next_wake);
        let wait = |scope: Rc<Cell<bool>>| {
            let pending = &pending;
            async move {
                let guard =
                    futures::future::poll_fn(|cx| Poll::Ready(pending.enter(scope.clone(), cx)))
                        .await?;
                futures::future::poll_fn(|cx| {
                    if scope.get() {
                        return Poll::Ready(Err(crate::Error::Cancelled));
                    }
                    guard.poll(cx, |_| Poll::<crate::Result<()>>::Pending)
                })
                .await
            }
        };
        let mut first = Box::pin(wait(canceled.clone()));
        let mut second = Box::pin(wait(Rc::new(Cell::new(false))));
        assert!(first.as_mut().poll(&mut first_cx).is_pending());
        assert!(second.as_mut().poll(&mut next_cx).is_pending());
        canceled.set(true);
        pending.wake_if(2, |scope| scope.get());
        assert_eq!(first_count.0.load(Ordering::SeqCst), 1);
        assert_eq!(next_count.0.load(Ordering::SeqCst), 0);
        assert!(matches!(
            first.as_mut().poll(&mut first_cx),
            Poll::Ready(Err(crate::Error::Cancelled))
        ));
        assert_eq!(pending.len(), 1);
        assert_eq!(next_count.0.load(Ordering::SeqCst), 1);
        let third = pending.enter(Rc::new(Cell::new(false)), &first_cx).unwrap();
        assert_eq!(third.sequence(), 3, "canceled sequence is not reused");
        drop(third); // Tail drop retains the head and wakes it.
        assert_eq!(pending.len(), 1);
        assert_eq!(next_count.0.load(Ordering::SeqCst), 2);
        drop(second);
        assert!(pending.is_empty());
        assert_eq!(pending.sequence.get(), 3);
    }

    /// Saturation and exhausted sequence space do not insert or wrap admission identities.
    #[test]
    fn admission_sequence_exhaustion_never_wraps_or_inserts() {
        let pending = AdmissionQueue::new(NonZeroUsize::new(1).unwrap());
        let cx = Context::from_waker(futures::task::noop_waker_ref());
        pending.sequence.set(u64::MAX - 1);
        let last = pending.enter((), &cx).unwrap();
        assert_eq!(last.sequence(), u64::MAX);
        assert!(matches!(
            pending.enter((), &cx),
            Err(crate::Error::Overloaded)
        ));
        drop(last);
        for _ in 0..2 {
            assert!(matches!(
                pending.enter((), &cx),
                Err(crate::Error::Unavailable)
            ));
            assert_eq!(pending.sequence.get(), u64::MAX);
            assert!(pending.is_empty());
        }
    }

    /// Closure wakes rotate within budget and cannot turn a closed pair into a reservation.
    #[test]
    fn pending_close_wakes_are_bounded_round_robin_and_cannot_reserve() {
        let (client, _worker) = pair(7, 1);
        let pending = AdmissionQueue::new(NonZeroUsize::new(2).unwrap());
        let (first_count, first_wake) = wake_counter();
        let (next_count, next_wake) = wake_counter();
        let mut first_cx = Context::from_waker(&first_wake);
        let next_cx = Context::from_waker(&next_wake);
        let first = pending.enter((), &first_cx).unwrap();
        let second = pending.enter((), &next_cx).unwrap();
        client.close_submissions();
        pending.wake_if(0, |_| panic!("zero budget inspected scope"));
        pending.wake_if(1, |_| client.submissions_closed());
        assert_eq!(first_count.0.load(Ordering::SeqCst), 1);
        assert_eq!(next_count.0.load(Ordering::SeqCst), 0);
        pending.wake_if(1, |_| client.submissions_closed());
        assert_eq!(next_count.0.load(Ordering::SeqCst), 1);
        assert!(matches!(
            first.poll(&mut first_cx, |cx| client
                .poll_reserve(cx, Id(1, 7, first.sequence()))),
            Poll::Ready(Err(Error::Runtime(crate::Error::Unavailable)))
        ));
        assert_eq!(client.outstanding(), 0);
        drop((first, second));
        pending.wake_if(usize::MAX, |_| panic!("empty queue inspected scope"));
    }

    /// Abandonment retains accepted credit until execution supplies a fenced completion.
    #[test]
    fn accepted_registration_drop_fences_until_reap_and_completed_drop_reclaims() {
        let (client, _worker) = pair(7, 1);
        let drops = Arc::new(AtomicUsize::new(0));
        let waiters = Waiters::default();
        let waker = futures::task::noop_waker_ref();
        let id = Id(1, 7, 1);
        let registration = waiters.register_guard(id, waker);
        let completion = message(&client, 1, &drops);
        drop(registration);
        assert_eq!(waiters.len(), 1);
        assert_eq!(client.outstanding(), 1);
        waiters.deliver(id, completion);
        assert!(waiters.is_empty());
        assert_eq!(drops.load(Ordering::SeqCst), 1);
        let id = Id(1, 7, 2);
        let registration = waiters.register_guard(id, waker);
        waiters.deliver(id, message(&client, 2, &drops));
        assert_eq!(client.outstanding(), 1);
        drop(registration);
        assert_eq!(client.outstanding(), 0);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
        assert!(waiters.is_empty());
    }

    /// Test ticket containing owner, generation, and monotonically increasing sequence.
    #[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
    struct Id(u8, u64, u64);
    impl Identity for Id {
        /// Small application-owner identity used by the test pair.
        type Owner = u8;
        /// Return the test application owner.
        fn owner(self) -> u8 {
            self.0
        }
        /// Return the test pair generation.
        fn generation(self) -> u64 {
            self.1
        }
        /// Return this ticket's admission sequence.
        fn sequence(self) -> u64 {
            self.2
        }
    }
    /// Owned payload with an observable final release count.
    struct Payload(Arc<AtomicUsize>);
    impl Drop for Payload {
        /// Record payload release before the surrounding message releases its permit.
        fn drop(&mut self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
    /// Job and completion fixture that destroys payloads before admission credit.
    struct Message {
        payload: Payload,

        result: Result<usize, &'static str>,

        permit: Permit<Id>,
    }
    impl Reserved<Id> for Message {
        /// Borrow the permit that accompanies this message through execution.
        fn permit(&self) -> &Permit<Id> {
            &self.permit
        }
    }
    /// Client endpoint specialized for test messages.
    type Client = ClientPort<Id, Message, Message>;
    /// Worker endpoint specialized for test messages.
    type Worker = WorkerPort<Id, Message, Message>;
    /// Allocate a bounded test pair with a chosen generation.
    fn pair(generation: u64, capacity: usize) -> (Client, Worker) {
        try_pair(Id(1, generation, 0), NonZeroUsize::new(capacity).unwrap()).unwrap()
    }
    /// Reserve a test ticket immediately, failing if capacity is unexpectedly absent.
    fn reserve(client: &Client, sequence: u64) -> Permit<Id> {
        let id = Id(1, client.identity().1, sequence);
        match client.poll_reserve(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            id,
        ) {
            Poll::Ready(Ok(permit)) => permit,
            _ => panic!("expected reservation"),
        }
    }
    /// Construct a successful test message with its exact accepted permit.
    fn message(client: &Client, sequence: u64, drops: &Arc<AtomicUsize>) -> Message {
        Message {
            payload: Payload(drops.clone()),
            result: Ok(42),
            permit: reserve(client, sequence),
        }
    }
    /// Take an already-published job from the test worker.
    fn dequeue(worker: &mut Worker) -> Message {
        match worker.poll_job(&mut Context::from_waker(futures::task::noop_waker_ref())) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("expected accepted job"),
        }
    }

    /// Both successful and failed execution retain admission credit through result drop.
    #[test]
    fn success_and_failure_keep_credit_through_consumption() {
        for outcome in [Ok(42), Err("execution failed")] {
            let (client, mut worker) = pair(7, 1);
            let drops = Arc::new(AtomicUsize::new(0));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let waiters = Waiters::default();
            let job = message(&client, 1, &drops);
            let id = job.permit.id();
            waiters.register(id, cx.waker());
            assert!(client.try_submit(job, |_| {}).is_ok());
            let mut job = dequeue(&mut worker);
            job.result = outcome;
            assert!(client.poll_reserve(&mut cx, Id(1, 7, 2)).is_pending());
            assert!(worker.complete(job).is_ok());
            client.close_submissions();
            waiters.deliver(id, client.receive().unwrap().unwrap());
            assert_eq!(client.outstanding(), 1);
            let completion = match waiters.poll_result(id, &mut cx) {
                Poll::Ready(Ok(result)) => result,
                _ => panic!("expected completion"),
            };
            assert_eq!(completion.result, outcome);
            assert_eq!(completion.payload.0.load(Ordering::SeqCst), 0);
            assert_eq!(client.outstanding(), 1);
            drop(completion);
            assert_eq!(drops.load(Ordering::SeqCst), 1);
            assert_eq!(client.outstanding(), 0);
        }
    }

    /// Invalid ticket fields fail before charging any admission capacity.
    #[test]
    fn stale_owner_generation_and_sequence_never_debit_capacity() {
        let (client, _worker) = pair(7, 1);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for id in [Id(2, 7, 0), Id(1, 6, 0), Id(1, 8, 0)] {
            assert!(matches!(
                client.poll_reserve(&mut cx, id),
                Poll::Ready(Err(Error::Stale))
            ));
            assert_eq!(client.outstanding(), 0);
        }
        drop(reserve(&client, u64::MAX));
        for sequence in [0, u64::MAX] {
            assert!(matches!(
                client.poll_reserve(&mut cx, Id(1, 7, sequence)),
                Poll::Ready(Err(Error::Stale))
            ));
        }
        assert_eq!(client.outstanding(), 0);
    }

    /// Wrong-pair and closed submissions preserve the payload and skip publication hooks.
    #[test]
    fn wrong_pair_and_closed_submission_return_exact_owner_without_hook() {
        let (client, mut worker) = pair(7, 1);
        // Equal ticket fields still do not authorize crossing pair instances.
        let (other, mut other_worker) = pair(7, 1);
        let drops = Arc::new(AtomicUsize::new(0));
        let job = message(&client, 1, &drops);
        let failure = other
            .try_submit(job, |_| panic!("invalid pair hook"))
            .err()
            .unwrap();
        assert_eq!(failure.error, Error::Runtime(crate::Error::Unavailable));
        assert_eq!(failure.command.permit.id(), Id(1, 7, 1));
        assert!(client.try_submit(failure.command, |_| {}).is_ok());
        let failure = other_worker.complete(dequeue(&mut worker)).err().unwrap();
        assert_eq!(failure.error, Error::Stale);
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        assert!(worker.complete(failure.command).is_ok());
        drop(client.receive().unwrap());
        let job = message(&other, 1, &drops);
        other.close_submissions();
        let failure = other
            .try_submit(job, |_| panic!("closed pair hook"))
            .err()
            .unwrap();
        assert_eq!(failure.error, Error::Runtime(crate::Error::Unavailable));
        assert_eq!(other.outstanding(), 1);
        drop(failure);
        assert_eq!(other.outstanding(), 0);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    /// Delivery abandonment cannot release accepted work or overwrite a newer identity.
    #[test]
    fn abandonment_waits_for_reap_and_stale_delivery_cannot_replace_waiter() {
        let (client, mut worker) = pair(7, 1);
        let drops = Arc::new(AtomicUsize::new(0));
        let waiters = Waiters::default();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let id = Id(1, 7, 1);
        let new_id = Id(1, 8, 1);
        waiters.register(id, cx.waker());
        assert!(
            client
                .try_submit(message(&client, 1, &drops), |_| {})
                .is_ok()
        );
        waiters.abandon(id);
        assert_eq!(waiters.len(), 1);
        assert_eq!(client.outstanding(), 1);
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        let completion = dequeue(&mut worker);
        assert!(worker.complete(completion).is_ok());
        waiters.deliver(id, client.receive().unwrap().unwrap());
        assert!(waiters.is_empty());
        assert_eq!(client.outstanding(), 0);
        waiters.register(new_id, cx.waker());
        waiters.deliver(id, message(&client, 2, &drops));
        assert!(waiters.poll_result(new_id, &mut cx).is_pending());
        assert_eq!(client.outstanding(), 0);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    /// Worker destruction releases queued jobs while an executing owner retains its permit.
    #[test]
    fn worker_loss_fences_queued_but_not_executing_owners() {
        let (client, mut worker) = pair(7, 2);
        let drops = Arc::new(AtomicUsize::new(0));
        for sequence in 1..=2 {
            assert!(
                client
                    .try_submit(message(&client, sequence, &drops), |_| {})
                    .is_ok()
            );
        }
        let executing = dequeue(&mut worker);
        drop(worker);
        assert!(client.completions_closed());
        client.discard_closed();
        assert_eq!(drops.load(Ordering::SeqCst), 1);
        assert_eq!(client.outstanding(), 1);
        drop(executing);
        assert_eq!(client.outstanding(), 0);
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    /// A completion rejected after client destruction returns its exact payload owner.
    #[test]
    fn completion_after_client_drop_returns_owner() {
        let (client, mut worker) = pair(7, 1);
        let drops = Arc::new(AtomicUsize::new(0));
        assert!(
            client
                .try_submit(message(&client, 1, &drops), |_| {})
                .is_ok()
        );
        let job = dequeue(&mut worker);
        drop(client);
        let failure = worker.complete(job).err().unwrap();
        assert_eq!(failure.error, Error::Runtime(crate::Error::Unavailable));
        assert_eq!(drops.load(Ordering::SeqCst), 0);
        drop(failure);
        assert_eq!(drops.load(Ordering::SeqCst), 1);
    }

    /// Bulk abandonment releases completed owners but requires a fence for unfinished jobs.
    #[test]
    fn cancel_all_releases_only_completed_owners_and_cleanup_is_bounded() {
        let (client, _worker) = pair(7, 2);
        let drops = Arc::new(AtomicUsize::new(0));
        let waiters = Waiters::default();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for sequence in 1..=3 {
            waiters.register(Id(1, 7, sequence), cx.waker());
        }
        waiters.deliver(Id(1, 7, 1), message(&client, 1, &drops));
        let executing = message(&client, 2, &drops);
        waiters.abandon_all();
        assert_eq!(waiters.len(), 2);
        assert_eq!(client.outstanding(), 1);
        assert!(matches!(
            waiters.poll_result(Id(1, 7, 1), &mut cx),
            Poll::Ready(Err(crate::Error::Cancelled))
        ));
        waiters.worker_closed(1, false);
        assert_eq!(waiters.len(), 2);
        drop(executing);
        waiters.worker_closed(1, true);
        assert_eq!(waiters.len(), 1);
        waiters.worker_closed(1, true);
        assert!(waiters.is_empty());
        assert_eq!(drops.load(Ordering::SeqCst), 2);
    }

    /// Submission, completion, consumption, and closure notify their registered drivers.
    #[test]
    fn capacity_and_driver_wakes_cover_publication_consumption_and_close() {
        let (client, mut worker) = pair(7, 1);
        let (count, waker) = wake_counter();
        let mut cx = Context::from_waker(&waker);
        let drops = Arc::new(AtomicUsize::new(0));
        worker.register_driver(&waker);
        client.register_driver(&waker);
        assert!(
            client
                .try_submit(message(&client, 1, &drops), |_| {})
                .is_ok()
        );
        assert!(count.0.swap(0, Ordering::SeqCst) > 0);
        let job = dequeue(&mut worker);
        assert!(client.poll_reserve(&mut cx, Id(1, 7, 2)).is_pending());
        assert!(worker.complete(job).is_ok());
        assert!(count.0.swap(0, Ordering::SeqCst) > 0);
        drop(client.receive().unwrap());
        assert!(count.0.swap(0, Ordering::SeqCst) > 0);
        drop(client.poll_reserve(&mut cx, Id(1, 7, 2)));
        worker.register_driver(&waker);
        client.close_submissions();
        assert!(count.0.load(Ordering::SeqCst) > 0);
        assert!(matches!(worker.poll_job(&mut cx), Poll::Ready(Ok(None))));
    }
}
