//! Bounded owned offload, with admission reserved through completion consumption.
//!
//! The caller owns execution, payloads, scope policy and result classification.
//! Dropping a waiter abandons delivery, not the accepted job or its credit.

use crate::channel::{self, Receiver, Sender};
use futures::task::AtomicWaker;
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    num::NonZeroUsize,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
};

/// Caller-defined ticket identity. Sequences must increase within a generation.
pub trait Identity: Copy + Ord {
    type Owner: Eq;
    fn owner(self) -> Self::Owner;
    fn generation(self) -> u64;
    fn sequence(self) -> u64;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Stale,
    Runtime(crate::Error),
}
impl From<crate::Error> for Error {
    fn from(error: crate::Error) -> Self {
        Self::Runtime(error)
    }
}
pub struct SendFailure<T> {
    pub command: T,
    pub error: Error,
}
impl<T> From<channel::SendFailure<T>> for SendFailure<T> {
    fn from(failure: channel::SendFailure<T>) -> Self {
        Self {
            command: failure.command,
            error: failure.error.into(),
        }
    }
}

struct Handoff<I> {
    identity: I,
    capacity: NonZeroUsize,
    outstanding: AtomicUsize,
    closed: AtomicBool,
    capacity_waker: AtomicWaker,
    worker_waker: AtomicWaker,
    client_waker: AtomicWaker,
}

/// Non-cloneable reservation of submission AND completion space.
/// Keep this after payload fields so their destructors run before credit release.
pub struct Permit<I> {
    handoff: Arc<Handoff<I>>,
    id: I,
}
impl<I: Copy> Permit<I> {
    pub fn id(&self) -> I {
        self.id
    }
}
impl<I> Drop for Permit<I> {
    fn drop(&mut self) {
        self.handoff.outstanding.fetch_sub(1, Ordering::AcqRel);
        self.handoff.capacity_waker.wake();
    }
}

/// Jobs and completions retain their permit until execution and consumption fence
/// their payloads. A completion must inherit the accepted job's exact permit.
pub trait Reserved<I> {
    fn permit(&self) -> &Permit<I>;
}

/// Unique producer/consumer endpoints, movable to their respective threads.
pub struct ClientPort<I, J, C> {
    handoff: Arc<Handoff<I>>,
    jobs: Sender<J>,
    completions: RefCell<Receiver<C>>,
    last_sequence: Cell<Option<u64>>,
}
pub struct WorkerPort<I, J, C> {
    handoff: Arc<Handoff<I>>,
    jobs: Option<Receiver<J>>,
    completions: Sender<C>,
}

/// Allocate both fixed-capacity queues without starting a service. The sequence
/// in `identity` is ignored; the first reservation may use any sequence.
pub fn try_pair<I: Identity, J: Reserved<I>, C: Reserved<I>>(
    identity: I,
    capacity: NonZeroUsize,
) -> crate::Result<(ClientPort<I, J, C>, WorkerPort<I, J, C>)> {
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
            completions: RefCell::new(results_rx),
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
    pub fn identity(&self) -> I {
        self.handoff.identity
    }
    pub fn capacity(&self) -> usize {
        self.handoff.capacity.get()
    }
    pub fn outstanding(&self) -> usize {
        self.handoff.outstanding.load(Ordering::Acquire)
    }
    pub fn submissions_closed(&self) -> bool {
        self.handoff.closed.load(Ordering::Acquire)
    }
    pub fn completions_closed(&self) -> bool {
        self.completions.borrow().is_closed()
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.handoff.client_waker.register(waker);
    }
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
    pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<crate::Result<Option<C>>> {
        self.completions.borrow_mut().poll_receive(cx)
    }
    pub fn receive(&self) -> crate::Result<Option<C>> {
        self.completions.borrow_mut().receive()
    }
    /// Discard orphaned jobs only after the unique worker receiver is destroyed.
    pub fn discard_closed(&self) {
        if self.completions_closed() {
            self.jobs.discard_closed();
        }
    }
    pub fn close_submissions(&self) {
        self.handoff.closed.store(true, Ordering::Release);
        self.jobs.close();
        self.handoff.worker_waker.wake();
        self.handoff.capacity_waker.wake();
    }
}
impl<I: Identity, J: Reserved<I>, C: Reserved<I>> WorkerPort<I, J, C> {
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

struct Pending<S> {
    sequence: u64,
    waker: Waker,
    scope: S,
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
    pub fn new(capacity: NonZeroUsize) -> Self {
        Self {
            capacity,
            sequence: Cell::new(0),
            pending: RefCell::new(VecDeque::new()),
            cursor: Cell::new(0),
        }
    }
    pub fn len(&self) -> usize {
        self.pending.borrow().len()
    }
    pub fn is_empty(&self) -> bool {
        self.pending.borrow().is_empty()
    }
    /// Admission consumes a sequence even if the guard is subsequently dropped.
    /// Saturation consumes neither a queue slot nor a sequence. Overflow requires
    /// draining and a new pair generation, never sequence reuse.
    pub fn enter(&self, scope: S, cx: &Context<'_>) -> crate::Result<CapacityWaiter<'_, S>> {
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
            waker: cx.waker().clone(),
            scope,
        });
        Ok(CapacityWaiter {
            queue: self,
            sequence,
        })
    }
    /// Inspect at most budget entries, rotating across turns, then wake outside
    /// the queue borrow. The predicate must not mutate this queue.
    pub fn wake_if(&self, budget: usize, mut predicate: impl FnMut(&S) -> bool) {
        let wakes = {
            let pending = self.pending.borrow();
            let mut wakes = Vec::new();
            for _ in 0..budget.min(pending.len()) {
                let index = self.cursor.get() % pending.len();
                self.cursor.set(index + 1);
                let entry = &pending[index];
                if predicate(&entry.scope) {
                    wakes.push(entry.waker.clone());
                }
            }
            wakes
        };
        for wake in wakes {
            wake.wake();
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
        let mut pending = self.queue.pending.borrow_mut();
        let entry = pending
            .iter_mut()
            .find(|entry| entry.sequence == self.sequence)
            .expect("live capacity waiter");
        entry.waker = cx.waker().clone();
        if pending
            .front()
            .is_some_and(|entry| entry.sequence != self.sequence)
        {
            return Poll::Pending;
        }
        drop(pending);
        reserve(cx)
    }
}
impl<S> Drop for CapacityWaiter<'_, S> {
    fn drop(&mut self) {
        let wake = {
            let mut pending = self.queue.pending.borrow_mut();
            pending.retain(|entry| entry.sequence != self.sequence);
            pending.front().map(|entry| entry.waker.clone())
        };
        if let Some(wake) = wake {
            wake.wake();
        }
    }
}

struct Waiter<C> {
    waker: Waker,
    abandoned: bool,
    result: Option<C>,
}
/// Worker-local delivery registry. Callers register only after admission; the
/// retained completion's permit continues charging capacity until consumption.
pub struct Waiters<I, C> {
    entries: RefCell<BTreeMap<I, Waiter<C>>>,
    cursor: Cell<Option<I>>,
}
/// Dropping the waiting future abandons delivery; accepted owners stay fenced by
/// their permits until completion reap or worker loss, independently of this guard.
pub struct Registration<'a, I: Copy + Ord, C> {
    waiters: &'a Waiters<I, C>,
    id: I,
}
impl<I: Copy + Ord, C> Drop for Registration<'_, I, C> {
    fn drop(&mut self) {
        self.waiters.abandon(self.id);
    }
}
impl<I: Copy + Ord, C> Default for Waiters<I, C> {
    fn default() -> Self {
        Self {
            entries: RefCell::new(BTreeMap::new()),
            cursor: Cell::new(None),
        }
    }
}
impl<I: Copy + Ord, C> Waiters<I, C> {
    pub fn len(&self) -> usize {
        self.entries.borrow().len()
    }
    pub fn is_empty(&self) -> bool {
        self.entries.borrow().is_empty()
    }
    pub fn register(&self, id: I, waker: &Waker) {
        self.entries.borrow_mut().insert(
            id,
            Waiter {
                waker: waker.clone(),
                abandoned: false,
                result: None,
            },
        );
    }
    pub fn register_guard(&self, id: I, waker: &Waker) -> Registration<'_, I, C> {
        self.register(id, waker);
        Registration { waiters: self, id }
    }
    /// Remove a registration after rejected submission, before any acceptance.
    pub fn remove(&self, id: I) {
        self.entries.borrow_mut().remove(&id);
    }
    pub fn abandon(&self, id: I) {
        let mut entries = self.entries.borrow_mut();
        if let Some(waiter) = entries.get_mut(&id) {
            if waiter.result.is_some() {
                entries.remove(&id);
            } else {
                waiter.abandoned = true;
            }
        }
    }
    /// A missing registration or abandoned completed result is canceled. Scope
    /// checks belong after Ready: cancellation cannot bypass the completion fence.
    pub fn poll_result(&self, id: I, cx: &mut Context<'_>) -> Poll<crate::Result<C>> {
        let mut entries = self.entries.borrow_mut();
        let Some(waiter) = entries.get_mut(&id) else {
            return Poll::Ready(Err(crate::Error::Cancelled));
        };
        waiter.waker = cx.waker().clone();
        if let Some(result) = waiter.result.take() {
            let abandoned = waiter.abandoned;
            entries.remove(&id);
            if abandoned {
                Poll::Ready(Err(crate::Error::Cancelled))
            } else {
                Poll::Ready(Ok(result))
            }
        } else {
            Poll::Pending
        }
    }
    /// Unknown or abandoned completions are dropped, never delivered to a new ID.
    pub fn deliver(&self, id: I, completion: C) {
        let wake = {
            let mut entries = self.entries.borrow_mut();
            if let Some(waiter) = entries.get_mut(&id) {
                if waiter.abandoned {
                    entries.remove(&id).map(|waiter| waiter.waker)
                } else {
                    let wake = waiter.waker.clone();
                    waiter.result = Some(completion);
                    Some(wake)
                }
            } else {
                None
            }
        };
        if let Some(wake) = wake {
            wake.wake();
        }
    }
    /// Only call with fenced=true after worker loss AND zero outstanding owners.
    /// Bounded round-robin wakes let live futures observe worker loss themselves.
    pub fn worker_closed(&self, budget: usize, fenced: bool) {
        use std::ops::Bound::{Excluded, Unbounded};
        if fenced {
            let abandoned: Vec<_> = self
                .entries
                .borrow()
                .iter()
                .filter(|(_, waiter)| waiter.abandoned)
                .take(budget)
                .map(|(id, _)| *id)
                .collect();
            for id in abandoned {
                self.remove(id);
            }
        }
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
                }
            }
            wakes
        };
        for wake in wakes {
            wake.wake();
        }
    }
    /// Cancel delivery, releasing only completions already fenced by execution.
    pub fn abandon_all(&self) {
        let wakes: Vec<_> = self
            .entries
            .borrow()
            .values()
            .filter(|waiter| waiter.result.is_some())
            .map(|waiter| waiter.waker.clone())
            .collect();
        let mut entries = self.entries.borrow_mut();
        for waiter in entries.values_mut() {
            waiter.abandoned = true;
        }
        entries.retain(|_, waiter| waiter.result.is_none());
        drop(entries);
        for wake in wakes {
            wake.wake();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct WakeCount(AtomicUsize);
    impl std::task::Wake for WakeCount {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
    fn wake_counter() -> (Arc<WakeCount>, Waker) {
        let count = Arc::new(WakeCount(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        (count, waker)
    }

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

    #[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
    struct Id(u8, u64, u64);
    impl Identity for Id {
        type Owner = u8;
        fn owner(self) -> u8 {
            self.0
        }
        fn generation(self) -> u64 {
            self.1
        }
        fn sequence(self) -> u64 {
            self.2
        }
    }
    struct Payload(Arc<AtomicUsize>);
    impl Drop for Payload {
        fn drop(&mut self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
    struct Message {
        payload: Payload,
        result: Result<usize, &'static str>,
        permit: Permit<Id>,
    }
    impl Reserved<Id> for Message {
        fn permit(&self) -> &Permit<Id> {
            &self.permit
        }
    }
    type Client = ClientPort<Id, Message, Message>;
    type Worker = WorkerPort<Id, Message, Message>;
    fn pair(generation: u64, capacity: usize) -> (Client, Worker) {
        try_pair(Id(1, generation, 0), NonZeroUsize::new(capacity).unwrap()).unwrap()
    }
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
    fn message(client: &Client, sequence: u64, drops: &Arc<AtomicUsize>) -> Message {
        Message {
            payload: Payload(drops.clone()),
            result: Ok(42),
            permit: reserve(client, sequence),
        }
    }
    fn dequeue(worker: &mut Worker) -> Message {
        match worker.poll_job(&mut Context::from_waker(futures::task::noop_waker_ref())) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("expected accepted job"),
        }
    }

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

    #[test]
    fn capacity_and_driver_wakes_cover_publication_consumption_and_close() {
        struct Counter(AtomicUsize);
        impl std::task::Wake for Counter {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }
        let (client, mut worker) = pair(7, 1);
        let count = Arc::new(Counter(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
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
