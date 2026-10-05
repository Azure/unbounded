//! Explicitly polled worker tasks, cooperative turns, retries, and draining races.
//!
//! The caller supplies capacity and polling budgets, handles task results, and owns
//! shutdown fencing. Selection is thread-local; queues and permits retain their
//! original owner across nested scopes. No executor or application policy is hidden
//! here. For example, a local proxy can drive detached cache writes alongside I/O.

use crate::{
    Error, Operation, Result, Scope, environment, group::FailureReporter, reactor::ReactorWake,
};
use std::{
    cell::{Cell, RefCell},
    collections::VecDeque,
    future::Future,
    pin::Pin,
    rc::Rc,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Wake, Waker},
    thread,
    time::Duration,
};

/// A local task. The caller must handle its outcome before returning unit.
type Task = Pin<Box<dyn Future<Output = ()> + 'static>>;

/// Child scopes must support cancellation requests independently of the parent.
pub trait HedgeScope: crate::Scope {
    /// Request cancellation without claiming that the child's work has finished.
    fn cancel(&self);
}

/// Identifies the contender whose successful value won the race.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Contender {
    /// The first operation, which wins ties in readiness.
    Primary,
    /// The delayed operation, polled only after the delay expires.
    Secondary,
}

/// Application classification and observation, without application result types.
pub trait HedgePolicy<E> {
    /// Poll the caller's alarm for launching the secondary operation.
    fn delay(&mut self, cx: &mut Context<'_>) -> Poll<()>;

    /// Whether another contender may still succeed after this failure.
    fn recoverable(&self, error: E) -> bool;

    /// Produce the result when neither contender succeeds or fails terminally.
    fn failure(&mut self) -> E;

    /// Record the first successful contender, before draining the other one.
    fn won(&mut self, contender: Contender);
}

/// Poll an optional local task once and clear its slot before returning a result.
/// Pending tasks retain their owner; absent tasks and pending tasks return None.
/// No scope, result-handling, scheduling, or shutdown policy is added.
pub fn poll_task<F: Future + ?Sized>(
    task: &mut Option<Pin<Box<F>>>,
    cx: &mut Context<'_>,
) -> Option<F::Output> {
    match task.as_mut()?.as_mut().poll(cx) {
        Poll::Pending => None,
        Poll::Ready(result) => {
            task.take();
            Some(result)
        }
    }
}

thread_local! {
    static CURRENT: RefCell<Option<Rc<DriverQueue>>> = const { RefCell::new(None) };
}

/// A bounded queue, including reservations not yet submitted. Zero disables admission.
///
/// Queues cannot move between workers:
/// ```compile_fail
/// use uring_runtime::drivers::DriverQueue;
/// fn require_send<T: Send>() {}
/// require_send::<DriverQueue>();
/// ```
pub struct DriverQueue {
    capacity: usize,

    drivers: RefCell<VecDeque<Driver>>,

    new: RefCell<Vec<Task>>,

    count: Cell<usize>,

    owner: RefCell<Option<Rc<Waker>>>,

    polling: Cell<bool>,
}

/// A submitted task and its independent wake-gated readiness bit.
struct Driver {
    operation: Task,

    wake: Arc<Runnable>,
}

/// Per-future readiness with a thread-safe wake path to the polling owner.
pub struct Runnable {
    ready: AtomicBool,

    owner: futures::task::AtomicWaker,
}

impl Runnable {
    /// Create a runnable task that is initially ready for its first poll.
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            ready: AtomicBool::new(true),
            owner: futures::task::AtomicWaker::new(),
        })
    }

    /// Poll on first use or after a wake, unless the caller explicitly forces it.
    pub fn poll<T>(
        self: &Arc<Self>,
        future: Pin<&mut (impl Future<Output = T> + ?Sized)>,
        cx: &mut Context<'_>,
        force: bool,
    ) -> Poll<T> {
        self.owner.register(cx.waker());
        if !self.ready.swap(false, Ordering::AcqRel) && !force {
            return Poll::Pending;
        }
        let waker = Waker::from(self.clone());
        future.poll(&mut Context::from_waker(&waker))
    }
}

impl std::task::Wake for Runnable {
    /// Forward an owned wake through the shared readiness path.
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    /// Publish readiness before notifying the latest polling owner.
    fn wake_by_ref(self: &Arc<Self>) {
        self.ready.store(true, Ordering::Release);
        self.owner.wake();
    }
}

/// Restores the previous queue. Keep guards stack-nested, never across suspension.
/// ```compile_fail
/// use uring_runtime::drivers::QueueGuard;
/// fn require_send<T: Send>() {}
/// require_send::<QueueGuard>();
/// ```
pub struct QueueGuard {
    previous: Option<Rc<DriverQueue>>,
}

impl Drop for QueueGuard {
    /// Restore the queue selected before entering this stack-nested guard.
    fn drop(&mut self) {
        CURRENT.with(|current| current.replace(self.previous.take()));
    }
}

impl DriverQueue {
    /// Create an empty queue with a bound covering both tasks and reservations.
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            drivers: RefCell::new(VecDeque::new()),
            new: RefCell::new(Vec::new()),
            count: Cell::new(0),
            owner: RefCell::new(None),
            polling: Cell::new(false),
        }
    }

    /// Discard simulated process tasks without polling them. Outstanding permits
    /// retain their reservations. Call only outside an active polling turn.
    #[cfg(any(test, feature = "simulation"))]
    pub fn simulation_crash(&self) {
        let drivers = std::mem::take(&mut *self.drivers.borrow_mut());
        let new = std::mem::take(&mut *self.new.borrow_mut());
        self.count.set(self.count.get() - drivers.len() - new.len());
        drop((drivers, new));
    }

    /// Select this queue only during poll/drop, never across async suspension.
    pub fn scope<F: Future>(self: &Rc<Self>, future: F) -> Queued<F> {
        Queued {
            queue: self.clone(),
            future: Some(Box::pin(future)),
        }
    }

    /// Select this queue until the returned stack-nested guard is dropped.
    pub fn enter(self: &Rc<Self>) -> QueueGuard {
        QueueGuard {
            previous: CURRENT.with(|current| current.replace(Some(self.clone()))),
        }
    }

    /// Count submitted tasks and unused reservations, including sleeping tasks.
    pub fn pending(&self) -> usize {
        self.count.get()
    }

    /// Give at most budget queued tasks a turn, deferring children to a later turn.
    /// Recursive calls are ignored and remaining runnable work wakes the owner.
    pub fn poll(self: &Rc<Self>, cx: &mut Context<'_>, budget: usize) {
        // Reject nested turns before touching the outer owner's wake registration.
        let Ok(_turn) = Busy::try_enter(&self.polling) else {
            return;
        };
        let _queue = self.enter();
        let owner = Rc::new(cx.waker().clone());
        let old = self.owner.replace(Some(owner));
        drop(old);
        let new = self.new.take();
        self.drivers
            .borrow_mut()
            .extend(new.into_iter().map(|operation| Driver {
                operation,
                wake: Runnable::new(),
            }));
        let turns = budget.min(self.drivers.borrow().len());
        for _ in 0..turns {
            let Some(driver) = self.drivers.borrow_mut().pop_front() else {
                break;
            };
            // Release capacity before destroying a completed or panicking task.
            let mut active = ActiveDriver {
                driver: Some(driver),
                count: &self.count,
            };
            let driver = active.driver.as_mut().unwrap();
            match driver.wake.poll(Pin::new(&mut driver.operation), cx, false) {
                Poll::Ready(()) => {}
                Poll::Pending => self
                    .drivers
                    .borrow_mut()
                    .push_back(active.driver.take().unwrap()),
            }
        }
        let ready = !self.new.borrow().is_empty()
            || self
                .drivers
                .borrow()
                .iter()
                .any(|driver| driver.wake.ready.load(Ordering::Acquire));
        if budget != 0 && ready {
            cx.waker().wake_by_ref();
        }
    }
}

/// Releases admission before a completed or panicking future is destroyed.
struct ActiveDriver<'a> {
    driver: Option<Driver>,

    count: &'a Cell<usize>,
}

impl Drop for ActiveDriver<'_> {
    /// Return admission before the retained future's destructor can reenter the queue.
    fn drop(&mut self) {
        if self.driver.is_some() {
            self.count.set(self.count.get() - 1);
        }
    }
}

/// A future that selects its queue while polling and destroying its inner future.
/// Even a Send inner future remains worker-local:
/// ```compile_fail
/// use uring_runtime::drivers::Queued;
/// fn require_send<T: Send>() {}
/// require_send::<Queued<std::future::Ready<()>>>();
/// ```
pub struct Queued<F> {
    queue: Rc<DriverQueue>,

    future: Option<Pin<Box<F>>>,
}

impl<F: Future> Future for Queued<F> {
    /// Preserve the inner future's output without changing its ownership policy.
    type Output = F::Output;

    /// Select the captured queue for one poll, then restore the caller's selection.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        let _queue = this.queue.enter();
        this.future
            .as_mut()
            .expect("queued future")
            .as_mut()
            .poll(cx)
    }
}

impl<F> Drop for Queued<F> {
    /// Destroy the inner future while its original queue is selected.
    fn drop(&mut self) {
        let _queue = self.queue.enter();
        self.future.take();
    }
}

/// Capture the selected queue without retaining its thread-local borrow.
fn current() -> Option<Rc<DriverQueue>> {
    CURRENT.with(|current| current.borrow().clone())
}

/// A reservation bound to its original worker, even if another queue is selected.
/// ```compile_fail
/// use uring_runtime::drivers::Permit;
/// fn require_send<T: Send>() {}
/// require_send::<Permit>();
/// ```
pub struct Permit {
    queue: Rc<DriverQueue>,

    reserved: bool,
}

impl Permit {
    /// Discard the outcome, but retain the completed future until queue capacity
    /// has been released. An async wrapper would destroy it during its final poll.
    pub fn submit_detached<F: Future + 'static>(self, driver: F) {
        self.submit(Box::pin(Detached(driver)));
    }

    /// Transfer this reservation to a task and notify the queue's polling owner.
    fn submit(mut self, driver: Task) {
        self.queue.new.borrow_mut().push(driver);
        self.reserved = false;
        let waker = self.queue.owner.borrow().clone();
        if let Some(waker) = waker {
            waker.wake_by_ref();
        }
    }
}

/// Discards a result without dropping the completed future during its final poll.
struct Detached<F>(F);

impl<F: Future> Future for Detached<F> {
    /// Detached tasks report results through caller-owned side effects.
    type Output = ();

    /// Discard the output while retaining the pinned future until capacity is returned.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        // SAFETY: the inner future stays pinned until Detached is destroyed.
        unsafe { self.map_unchecked_mut(|this| &mut this.0) }
            .poll(cx)
            .map(|_| ())
    }
}

impl Drop for Permit {
    /// Return only unused reservations; submitted tasks retain their queue capacity.
    fn drop(&mut self) {
        if self.reserved {
            self.queue.count.set(self.queue.count.get() - 1);
        }
    }
}

/// Reserve capacity in the selected queue. Missing selection is InvalidConfiguration;
/// exhausted capacity is Overloaded. Dropping an unused permit returns capacity.
pub fn reserve() -> Result<Permit> {
    let queue = current().ok_or(Error::InvalidConfiguration)?;
    if queue.count.get() >= queue.capacity {
        return Err(Error::Overloaded);
    }
    queue.count.set(queue.count.get() + 1);
    Ok(Permit {
        queue,
        reserved: true,
    })
}

/// Reserve capacity and submit a task to the currently selected queue.
#[cfg(test)]
fn spawn(driver: Task) -> Result<()> {
    reserve()?.submit(driver);
    Ok(())
}

/// Poll the selected queue, doing nothing when no worker queue is installed.
pub fn poll(cx: &mut Context<'_>, budget: usize) {
    if let Some(queue) = current() {
        queue.poll(cx, budget);
    }
}

/// Count tasks and reservations in the selected queue, or zero without a queue.
pub fn pending() -> usize {
    current().map_or(0, |queue| queue.pending())
}

/// Worker-local exclusion flag. Dropping the guard makes the flag available.
#[must_use = "dropping the guard releases the busy flag"]
pub struct Busy<'a>(&'a Cell<bool>);

impl<'a> Busy<'a> {
    /// Enter an idle flag, returning overload without disturbing an active owner.
    pub fn try_enter(flag: &'a Cell<bool>) -> Result<Self> {
        if flag.replace(true) {
            Err(Error::Overloaded)
        } else {
            Ok(Self(flag))
        }
    }
}

impl Drop for Busy<'_> {
    /// Release exclusion on normal return and panic unwinding alike.
    fn drop(&mut self) {
        self.0.set(false);
    }
}

/// Yield exactly one cooperative turn, waking the current task for its next poll.
pub async fn yield_now() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    })
    .await
}

/// Poll after registering cancellation and checking caller-owned scope policy.
///
/// The caller must arrange deadline polling; this does not create a timer or
/// self-wake. Do not use it to truncate an operation's required completion fence.
pub async fn poll_scoped<S: Scope, T, E: Into<S::Error>>(
    scope: &S,
    mut poll: impl FnMut(&mut Context<'_>) -> Poll<Result<T, E>>,
) -> Result<T, S::Error> {
    let cancellation = scope.cancellation().map(|c| c.subscribe()).transpose()?;
    std::future::poll_fn(|cx| {
        if let Some(cancellation) = &cancellation {
            cancellation.register(cx.waker());
        }
        scope.check()?;
        poll(cx).map_err(Into::into)
    })
    .await
}

/// Wake this thread and, optionally, interrupt its reactor wait. No thread is spawned.
pub fn thread_waker(reactor: Option<ReactorWake>) -> Waker {
    Waker::from(Arc::new(ThreadWake(thread::current(), reactor)))
}

/// Drive resource completions while a local operation borrows its service graph.
///
/// Backend failures are reported immediately but do not abandon the operation.
/// Its completion fence runs to completion, then the first backend error wins.
/// `wait` must be bounded. After it returns, wake the polling task so a surrounding
/// driver schedules another turn before sleeping, including non-thread executors.
pub fn drive_local_with<'a, T: 'a, E: Copy + 'a>(
    mut operation: Operation<'a, T, E>,
    reporter: Option<&'a FailureReporter<E>>,
    mut poll: impl FnMut(&Waker) -> Result<(), E> + 'a,
    mut wait: impl FnMut() -> Result<(), E> + 'a,
) -> Operation<'a, T, E> {
    let mut error = None;
    Box::pin(std::future::poll_fn(move |cx| {
        // Report every failure while keeping the first as the eventual result.
        let mut record = |result| {
            if let Err(failure) = result {
                if let Some(reporter) = reporter {
                    reporter.report(failure);
                }
                error.get_or_insert(failure);
            }
        };
        record(poll(cx.waker()));
        if let Poll::Ready(result) = operation.as_mut().poll(cx) {
            return Poll::Ready(error.map_or(result, Err));
        }
        record(wait());
        cx.waker().wake_by_ref();
        Poll::Pending
    }))
}

/// Submission retry policy for the explicitly polled listener helper.
#[derive(Clone, Copy, Debug)]
struct RetryOptions {
    /// Minimum delay between overload attempts; the owner supplies polling turns.
    interval: Duration,

    /// Retries after the initial attempt; None leaves the scope as the only bound.
    max_retries: Option<usize>,
}

impl Default for RetryOptions {
    /// Use scope-bounded retries at the listener owner's ten-millisecond cadence.
    fn default() -> Self {
        Self {
            interval: Duration::from_millis(10),
            max_retries: None,
        }
    }
}

/// Retry listener submission on queue pressure without self-waking or submitting
/// a timer to the saturated ring. The owner must poll at least every 10ms.
/// Submitted operations retain their original completion fence on cancellation.
pub fn retry_listener<'a, S: Scope, T: 'a>(
    scope: &'a S,
    submit: impl FnMut() -> Operation<'a, T, S::Error> + 'a,
) -> Operation<'a, T, S::Error>
where
    S::Error: PartialEq,
{
    retry_listener_with(scope, RetryOptions::default(), submit)
}

/// Retry overload using caller-selected limits without allocating a backend timer.
fn retry_listener_with<'a, S: Scope, T: 'a>(
    scope: &'a S,
    options: RetryOptions,
    mut submit: impl FnMut() -> Operation<'a, T, S::Error> + 'a,
) -> Operation<'a, T, S::Error>
where
    S::Error: PartialEq,
{
    Box::pin(async move {
        if options.interval.is_zero() {
            return Err(Error::InvalidConfiguration.into());
        }
        let cancellation = scope.cancellation().map(|c| c.subscribe()).transpose()?;
        let mut operation = None;
        let mut retries = 0usize;
        let mut retry_at = environment::now();
        std::future::poll_fn(|cx| {
            if operation.is_none() {
                scope.check()?;
                if let Some(cancellation) = &cancellation {
                    cancellation.register(cx.waker());
                }
                if environment::now() < retry_at {
                    return Poll::Pending;
                }
                operation = Some(submit());
            }
            match operation.as_mut().unwrap().as_mut().poll(cx) {
                Poll::Ready(Err(error)) if error == Error::Overloaded.into() => {
                    operation = None;
                    if options.max_retries.is_some_and(|limit| retries >= limit) {
                        return Poll::Ready(Err(error));
                    }
                    retries = retries.checked_add(1).ok_or(Error::Overloaded)?;
                    retry_at = environment::now()
                        .checked_add(options.interval)
                        .ok_or(Error::InvalidInput)?;
                    Poll::Pending
                }
                result => result,
            }
        })
        .await
    })
}

/// Validation belongs inside each contender. A ready primary (even a recoverable
/// failure) never launches the secondary. Cancellation requests are not fences.
///
/// The owner must keep polling this future after caller detachment and retain
/// permits/escrow until it returns. Dropping a Rust future cannot drain its work.
/// The caller also drives deadlines and the policy's delay alarm.
pub async fn race<S: HedgeScope, T>(
    primary: impl Future<Output = Result<T, S::Error>>,
    secondary: impl Future<Output = Result<T, S::Error>>,
    primary_scope: &S,
    secondary_scope: &S,
    parent: &S,
    mut policy: impl HedgePolicy<S::Error>,
) -> Result<T, S::Error> {
    parent.check()?;
    let mut primary = std::pin::pin!(primary);
    let mut secondary = std::pin::pin!(secondary);
    let mut registration = parent.cancellation().map(|c| c.subscribe()).transpose()?;
    let mut primary_state = Child::new(primary_scope);
    let mut secondary_state = Child::new(secondary_scope);
    let mut launched = false;
    let mut winner = None;
    let mut fatal = None;
    std::future::poll_fn(|cx| {
        if let Err(error) = parent.check() {
            fatal = Some(error);
            registration.take();
        }
        if let Some(registration) = &registration {
            registration.register(cx.waker());
        }
        if fatal.is_some() || winner.is_some() {
            primary_state.cancel();
            secondary_state.cancel();
        }
        if !primary_state.is_done()
            && let Poll::Ready(result) = primary.as_mut().poll(cx)
        {
            primary_state.phase = ChildPhase::Done;
            record_result(
                result,
                Contender::Primary,
                &mut winner,
                &mut fatal,
                &mut policy,
            );
            if !launched {
                secondary_state.phase = ChildPhase::Done;
            }
        }
        if !launched
            && !secondary_state.is_done()
            && fatal.is_none()
            && winner.is_none()
            && policy.delay(cx).is_ready()
        {
            launched = true;
        }
        if launched && !secondary_state.is_done() {
            if fatal.is_some() || winner.is_some() {
                secondary_state.cancel();
            }
            if let Poll::Ready(result) = secondary.as_mut().poll(cx) {
                secondary_state.phase = ChildPhase::Done;
                record_result(
                    result,
                    Contender::Secondary,
                    &mut winner,
                    &mut fatal,
                    &mut policy,
                );
            }
        }
        if winner.is_some() || fatal.is_some() {
            primary_state.cancel();
            secondary_state.cancel();
            if !launched {
                secondary_state.phase = ChildPhase::Done;
            }
        }
        if primary_state.is_done() && secondary_state.is_done() {
            Poll::Ready(if let Some(error) = fatal {
                Err(error)
            } else {
                winner.take().ok_or_else(|| policy.failure())
            })
        } else {
            Poll::Pending
        }
    })
    .await
}

/// Tracks one child's fence separately from its one-shot cancellation request.
struct Child<'a, S> {
    scope: &'a S,

    phase: ChildPhase,
}

/// Cancellation starts draining; only observing completion ends the child's fence.
#[derive(Clone, Copy, Eq, PartialEq)]
enum ChildPhase {
    /// The child is incomplete and this race has not requested cancellation.
    Running,
    /// Cancellation was requested once, but completion is still required.
    Draining,
    /// The child completed or was skipped before launching.
    Done,
}

impl<'a, S: HedgeScope> Child<'a, S> {
    /// Start with an incomplete child whose scope has not been canceled here.
    fn new(scope: &'a S) -> Self {
        Self {
            scope,
            phase: ChildPhase::Running,
        }
    }

    /// Whether this child completed or was skipped before launching.
    fn is_done(&self) -> bool {
        self.phase == ChildPhase::Done
    }

    /// Request cancellation once; an already completed child needs no request.
    fn cancel(&mut self) {
        if self.phase == ChildPhase::Running {
            self.phase = ChildPhase::Draining;
            self.scope.cancel();
        }
    }
}

/// Classify every error, but retain only the first winner or terminal failure.
fn record_result<T, E: Copy>(
    result: Result<T, E>,
    contender: Contender,
    winner: &mut Option<T>,
    fatal: &mut Option<E>,
    policy: &mut impl HedgePolicy<E>,
) {
    match result {
        Ok(value) if winner.is_none() && fatal.is_none() => {
            policy.won(contender);
            *winner = Some(value);
        }
        Err(error) if !policy.recoverable(error) && winner.is_none() && fatal.is_none() => {
            *fatal = Some(error);
        }
        _ => {}
    }
}

/// Routes a task wake to both a parked thread and its optional reactor wait.
struct ThreadWake(thread::Thread, Option<ReactorWake>);

impl Wake for ThreadWake {
    /// Forward an owned wake through the shared thread and reactor notification path.
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    /// Unpark the owner thread and interrupt its optional reactor wait.
    fn wake_by_ref(self: &Arc<Self>) {
        self.0.unpark();
        if let Some(reactor) = &self.1 {
            let _ = reactor.wake();
        }
    }
}

/// Pure queue ownership, capacity, wake gating, and callback reentry regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use futures::{channel::oneshot, task::noop_waker};

    /// A polling panic returns admission before the task destructor submits a child.
    #[test]
    fn panic_releases_capacity_before_task_drop_and_queue_remains_usable() {
        /// Panics while polling, then reenters its queue during destruction.
        struct Panics;
        impl Future for Panics {
            /// This task has no application result.
            type Output = ();

            /// Inject a panic after the queue has transferred execution ownership.
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
                panic!("injected poll panic");
            }
        }
        impl Drop for Panics {
            /// Verify admission is available before submitting replacement work.
            fn drop(&mut self) {
                assert_eq!(pending(), 0);
                reserve().unwrap().submit(Box::pin(async {}));
            }
        }
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        reserve().unwrap().submit_detached(Panics);
        let mut cx = Context::from_waker(Waker::noop());
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| queue.poll(&mut cx, 1)))
                .is_err()
        );
        assert_eq!(pending(), 1);
        queue.poll(&mut cx, 1);
        assert_eq!(pending(), 0);
    }

    /// Nested polling cannot steal the outer owner's notifications for ready backlog.
    #[test]
    fn budget_backlog_wakes_outer_owner_and_nested_poll_cannot_replace_it() {
        let queue = Rc::new(DriverQueue::new(2));
        let _owner = queue.enter();
        let nested = Arc::new(WakeCount::default());
        let nested_waker = Waker::from(nested.clone());
        spawn(Box::pin(async move {
            poll(&mut Context::from_waker(&nested_waker), 1);
        }))
        .unwrap();
        spawn(Box::pin(async {})).unwrap();
        let outer = Arc::new(WakeCount::default());
        let waker = Waker::from(outer.clone());
        let mut cx = Context::from_waker(&waker);
        queue.poll(&mut cx, 1);
        assert_eq!(outer.count(), 1);
        spawn(Box::pin(async {})).unwrap();
        assert_eq!(outer.count(), 2);
        assert_eq!(nested.count(), 0);
        queue.poll(&mut cx, 2);
        assert_eq!(pending(), 0);
        assert_eq!(outer.count(), 2);
    }

    /// Retired owner wakers can reenter submission and polling without borrow conflicts.
    #[test]
    fn replacing_owner_drops_waker_outside_queue_borrows() {
        /// Submits replacement work when the queue discards its previous owner.
        struct OnDrop;
        impl std::task::Wake for OnDrop {
            /// Reject a wake when replacement should only dispose of the owner.
            fn wake(self: Arc<Self>) {
                panic!("idle queue must not wake its retired owner");
            }
        }
        impl Drop for OnDrop {
            /// Reenter submission and attempt a nested polling turn.
            fn drop(&mut self) {
                // Submission reads the owner; nested polling must be rejected
                // without any owner or driver-table borrow held by its caller.
                spawn(Box::pin(async {})).unwrap();
                poll(&mut Context::from_waker(Waker::noop()), 1);
            }
        }
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        queue.poll(&mut Context::from_waker(&Waker::from(Arc::new(OnDrop))), 0);
        queue.poll(&mut Context::from_waker(Waker::noop()), 1);
        assert_eq!(pending(), 0);
    }

    /// A completed future's panicking destructor cannot retain queue admission.
    #[test]
    fn panicking_completed_task_destructor_does_not_leak_capacity() {
        /// Completes normally, then panics while releasing its owned resources.
        struct PanicsOnDrop;
        impl Future for PanicsOnDrop {
            /// Completion carries no application value.
            type Output = ();

            /// Finish immediately so destruction happens through the completion path.
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<()> {
                Poll::Ready(())
            }
        }
        impl Drop for PanicsOnDrop {
            /// Check released admission before injecting the destructor panic.
            fn drop(&mut self) {
                assert_eq!(pending(), 0);
                panic!("injected destructor panic");
            }
        }
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        reserve().unwrap().submit_detached(PanicsOnDrop);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                queue.poll(&mut Context::from_waker(Waker::noop()), 1);
            }))
            .is_err()
        );
        assert_eq!(pending(), 0);
        assert!(reserve().is_ok());
    }

    /// Detached results do not destroy their future before queue capacity is returned.
    #[test]
    fn completed_operation_is_dropped_after_releasing_capacity() {
        /// Completes with an ignored failure and checks admission when destroyed.
        struct Complete;
        impl Future for Complete {
            /// A deliberately ignored application error exercises detached results.
            type Output = std::result::Result<(), &'static str>;

            /// Return a failure without releasing the future's ownership yet.
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Self::Output> {
                Poll::Ready(Err("detached failure"))
            }
        }
        impl Drop for Complete {
            /// Verify the completed task's slot is available despite other reservations.
            fn drop(&mut self) {
                assert_eq!(pending(), 1023);
                assert!(reserve().is_ok());
            }
        }
        let queue = Rc::new(DriverQueue::new(1024));
        let _owner = queue.enter();
        let permits: Vec<_> = (0..1023).map(|_| reserve().unwrap()).collect();
        reserve().unwrap().submit_detached(Complete);
        poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 1);
        drop(permits);
        assert_eq!(pending(), 0);
    }

    /// Sleeping tasks receive no extra polls until their own readiness waker fires.
    #[test]
    fn blocked_driver_is_not_repolled_until_its_own_wake() {
        let queue = Rc::new(DriverQueue::new(4));
        let _owner = queue.enter();
        let polls = Rc::new(Cell::new(0));
        let observed = polls.clone();
        let wake = Rc::new(RefCell::new(None::<Waker>));
        let saved = wake.clone();
        spawn(Box::pin(std::future::poll_fn(move |cx| {
            observed.set(observed.get() + 1);
            *saved.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        })))
        .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..100 {
            queue.poll(&mut cx, 64);
        }
        assert_eq!(polls.get(), 1);
        wake.borrow().as_ref().unwrap().wake_by_ref();
        queue.poll(&mut cx, 64);
        assert_eq!(polls.get(), 2);
        queue.simulation_crash();
        assert_eq!(queue.pending(), 0);
    }

    /// Permits keep their original worker and children wait for its next polling turn.
    #[test]
    fn workers_isolate_permits_children_and_capacity_on_one_thread() {
        let a = Rc::new(DriverQueue::new(2));
        let b = Rc::new(DriverQueue::new(3));
        let _a = a.enter();
        let permit = reserve().unwrap();
        let completed = Rc::new(Cell::new(false));
        let child = completed.clone();
        {
            let _b = b.enter();
            permit.submit(Box::pin(async move {
                spawn(Box::pin(async move {
                    child.set(true);
                }))
                .unwrap();
            }));
            let permits: Vec<_> = (0..3).map(|_| reserve().unwrap()).collect();
            assert!(matches!(reserve(), Err(Error::Overloaded)));
            assert_eq!(a.pending(), 1);
            drop(permits);
        }
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        b.poll(&mut cx, 8);
        assert!(!completed.get());
        assert_eq!(b.pending(), 0);
        a.poll(&mut cx, 8);
        assert!(!completed.get());
        assert_eq!(a.pending(), 1);
        a.poll(&mut cx, 8);
        assert!(completed.get());
        assert_eq!(a.pending(), 0);
    }

    /// Losing a request receiver does not release or stop its accepted operation.
    #[test]
    fn owned_operation_progresses_after_request_receiver_disappears() {
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        let complete = Rc::new(Cell::new(false));
        let observed = complete.clone();
        let (completion, fence) = oneshot::channel::<()>();
        let (reply, reader) = oneshot::channel::<()>();
        spawn(Box::pin(async move {
            fence.await.unwrap();
            observed.set(true);
            let _ = reply.send(());
        }))
        .unwrap();
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        poll(&mut cx, 8);
        drop(reader);
        assert!(!complete.get());
        assert_eq!(pending(), 1);
        completion.send(()).unwrap();
        poll(&mut cx, 8);
        assert!(complete.get());
        assert_eq!(pending(), 0);
    }

    /// Child submission during polling progresses without recursively borrowing the queue.
    #[test]
    fn nested_bootstrap_driver_is_polled_without_recursive_table_borrow() {
        let queue = Rc::new(DriverQueue::new(2));
        let _owner = queue.enter();
        let (send, mut result) = oneshot::channel();
        spawn(Box::pin(async move {
            let (child, receive) = oneshot::channel();
            spawn(Box::pin(async move {
                child.send(7).unwrap();
            }))
            .unwrap();
            let value = receive.await.unwrap();
            let _ = send.send(value);
        }))
        .unwrap();
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        for _ in 0..4 {
            poll(&mut cx, 8);
        }
        assert_eq!(result.try_recv().unwrap(), Some(7));
        assert_eq!(pending(), 0);
    }

    /// Admission requires a selected queue and unused permits restore its capacity.
    #[test]
    fn acquisition_requires_an_installed_worker_queue() {
        assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
        let queue = Rc::new(DriverQueue::new(1));
        {
            let _owner = queue.enter();
            let permit = reserve().unwrap();
            assert_eq!(queue.pending(), 1);
            drop(permit);
            assert_eq!(queue.pending(), 0);
        }
        assert!(matches!(reserve(), Err(Error::InvalidConfiguration)));
    }

    /// Zero capacity disables admission, and polling budgets preserve reservations.
    #[test]
    fn zero_capacity_budget_and_completion_accounting() {
        let disabled = Rc::new(DriverQueue::new(0));
        {
            let _owner = disabled.enter();
            assert!(matches!(reserve(), Err(Error::Overloaded)));
            assert_eq!(pending(), 0);
        }
        let queue = Rc::new(DriverQueue::new(2));
        let _owner = queue.enter();
        spawn(Box::pin(async {})).unwrap();
        spawn(Box::pin(async {})).unwrap();
        assert!(matches!(reserve(), Err(Error::Overloaded)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        poll(&mut cx, 0);
        assert_eq!(pending(), 2);
        poll(&mut cx, 1);
        assert_eq!(pending(), 1);
        let permit = reserve().unwrap();
        poll(&mut cx, usize::MAX);
        assert_eq!(pending(), 1);
        drop(permit);
        assert_eq!(pending(), 0);
    }

    /// Scoped polling and destruction restore the previously selected worker queue.
    #[test]
    fn scoped_poll_and_drop_restore_previous_selection() {
        /// Submits work while verifying the queue selected for future destruction.
        struct OnDrop(Rc<DriverQueue>);
        impl Drop for OnDrop {
            /// Check the captured queue and submit a replacement task into it.
            fn drop(&mut self) {
                assert!(Rc::ptr_eq(&current().unwrap(), &self.0));
                spawn(Box::pin(async {})).unwrap();
            }
        }
        let a = Rc::new(DriverQueue::new(2));
        let b = Rc::new(DriverQueue::new(2));
        let dropper = OnDrop(a.clone());
        let expected = a.clone();
        let mut scoped = Box::pin(a.scope(async move {
            let _dropper = dropper;
            assert!(Rc::ptr_eq(&current().unwrap(), &expected));
            std::future::pending::<()>().await;
        }));
        let _owner = b.enter();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(scoped.as_mut().poll(&mut cx).is_pending());
        assert!(Rc::ptr_eq(&current().unwrap(), &b));
        drop(scoped);
        assert_eq!(a.pending(), 1);
        assert!(Rc::ptr_eq(&current().unwrap(), &b));
        a.poll(&mut cx, 1);
        assert_eq!(a.pending(), 0);
    }

    /// Recursive turns defer children and simulated process loss retains unused permits.
    #[test]
    fn recursive_poll_defers_children_and_crash_preserves_unused_permits() {
        let queue = Rc::new(DriverQueue::new(3));
        let _owner = queue.enter();
        let permit = reserve().unwrap();
        spawn(Box::pin(async {
            spawn(Box::pin(std::future::pending())).unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            poll(&mut cx, 8);
        }))
        .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        poll(&mut cx, 8);
        assert_eq!(pending(), 2);
        queue.simulation_crash();
        assert_eq!(pending(), 1);
        drop(permit);
        assert_eq!(pending(), 0);
    }

    use crate::test_util::WakeCounter as WakeCount;

    /// Runnable ownership follows the latest poll without losing in-poll wakes or force.
    #[test]
    fn runnable_replaces_owner_preserves_in_poll_wake_and_force() {
        let first = Arc::new(WakeCount::default());
        let second = Arc::new(WakeCount::default());
        let w1 = Waker::from(first.clone());
        let w2 = Waker::from(second.clone());
        let runnable = Runnable::new();
        let polls = Cell::new(0);
        let saved = RefCell::new(None);
        let mut work = Box::pin(std::future::poll_fn(|cx| {
            polls.set(polls.get() + 1);
            *saved.borrow_mut() = Some(cx.waker().clone());
            if polls.get() == 1 {
                cx.waker().wake_by_ref();
            }
            Poll::<()>::Pending
        }));
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w1), false)
                .is_pending()
        );
        assert_eq!(first.count(), 1);
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), false)
                .is_pending()
        );
        assert_eq!(polls.get(), 2);
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), false)
                .is_pending()
        );
        assert_eq!(polls.get(), 2);
        saved.borrow().as_ref().unwrap().wake_by_ref();
        assert_eq!(second.count(), 1);
        assert_eq!(first.count(), 1);
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), false)
                .is_pending()
        );
        assert!(
            runnable
                .poll(work.as_mut(), &mut Context::from_waker(&w2), true)
                .is_pending()
        );
        assert_eq!(polls.get(), 4);
    }

    /// Submission notifies the queue's latest polling owner.
    #[test]
    fn submission_wakes_last_polling_owner() {
        let queue = Rc::new(DriverQueue::new(1));
        let _owner = queue.enter();
        let wake = Arc::new(WakeCount::default());
        let waker = Waker::from(wake.clone());
        queue.poll(&mut Context::from_waker(&waker), 0);
        spawn(Box::pin(async {})).unwrap();
        assert_eq!(wake.count(), 1);
    }

    /// Optional task slots retain pending work and dispose of ready work before delivery.
    #[test]
    fn task_slot_retains_pending_and_drops_ready_before_returning_result() {
        /// Controlled future that records destruction independently of its result.
        struct Local {
            ready: Rc<Cell<bool>>,

            dropped: Rc<Cell<bool>>,

            result: Result<usize>,
        }
        impl Future for Local {
            /// Return the fixture's chosen success or failure value.
            type Output = Result<usize>;

            /// Stay pending until the fixture explicitly permits completion.
            fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Self::Output> {
                if self.ready.get() {
                    Poll::Ready(self.result)
                } else {
                    Poll::Pending
                }
            }
        }
        impl Drop for Local {
            /// Record disposal so delivery can verify its ordering.
            fn drop(&mut self) {
                self.dropped.set(true);
            }
        }
        for result in [Ok(7), Err(Error::Io)] {
            let ready = Rc::new(Cell::new(false));
            let dropped = Rc::new(Cell::new(false));
            let mut task = Some(Box::pin(Local {
                ready: ready.clone(),
                dropped: dropped.clone(),
                result,
            }));
            let mut cx = Context::from_waker(Waker::noop());
            assert_eq!(poll_task(&mut task, &mut cx), None);
            assert!(task.is_some());
            assert!(!dropped.get());
            ready.set(true);
            assert_eq!(poll_task(&mut task, &mut cx), Some(result));
            assert!(task.is_none());
            assert!(dropped.get());
            assert_eq!(poll_task(&mut task, &mut cx), None);
        }
    }
}

/// Cooperative scheduling, scope checks, and resource-driving fence regressions.
#[cfg(test)]
mod scheduler_tests {
    use super::*;
    use crate::environment::Cancellation;
    use crate::test_util::WakeCounter;
    use std::{future::Future, pin::pin, rc::Rc};

    /// A bounded wait wakes a surrounding wake-gated scheduler for the next turn.
    #[test]
    fn driver_composes_with_wake_gated_local_scheduler() {
        let ready = Cell::new(false);
        let mut driver = drive_local_with(
            Box::pin(std::future::poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Ok::<_, Error>(7))
                } else {
                    Poll::Pending
                }
            })),
            None,
            |_| Ok(()),
            || {
                ready.set(true);
                Ok(())
            },
        );
        let runnable = crate::drivers::Runnable::new();
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(runnable.poll(driver.as_mut(), &mut cx, false).is_pending());
        assert_eq!(count.count(), 1);
        assert_eq!(
            runnable.poll(driver.as_mut(), &mut cx, false),
            Poll::Ready(Ok(7))
        );
    }

    /// Shares cancellation and a caller-controlled admission error across polls.
    #[derive(Clone)]
    struct TestScope {
        cancellation: Option<Cancellation>,

        error: Rc<Cell<Option<Error>>>,
    }

    impl Scope for TestScope {
        /// Scheduler fixtures use the runtime's portable failure classifications.
        type Error = Error;

        /// Prefer cancellation over the caller-controlled policy error.
        fn check(&self) -> Result<()> {
            if self
                .cancellation
                .as_ref()
                .is_some_and(Cancellation::is_cancelled)
            {
                return Err(Error::Cancelled);
            }
            self.error.get().map_or(Ok(()), Err)
        }

        /// Supply notification only for fixtures that enable cancellation.
        fn cancellation(&self) -> Option<&Cancellation> {
            self.cancellation.as_ref()
        }
    }

    /// Cooperative yielding suspends and notifies exactly once.
    #[test]
    fn yield_is_pending_once_and_wakes_once() {
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut future = pin!(yield_now());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(count.count(), 1);
        assert!(future.as_mut().poll(&mut cx).is_ready());
        assert_eq!(count.count(), 1);
    }

    /// Caller policy is checked before invoking work regardless of notification support.
    #[test]
    fn scoped_poll_checks_policy_before_callback_with_or_without_cancellation() {
        for cancellation in [None, Some(Cancellation::new().unwrap())] {
            let scope = TestScope {
                cancellation,
                error: Rc::new(Cell::new(None)),
            };
            let calls = Cell::new(0);
            let mut future = pin!(poll_scoped(&scope, |_| -> Poll<Result<()>> {
                calls.set(calls.get() + 1);
                Poll::Pending
            }));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            scope.error.set(Some(Error::DeadlineExceeded));
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::DeadlineExceeded))
            );
            assert_eq!(calls.get(), 1);
        }
    }

    /// Scoped polling retains a cancellation subscription only for its own lifetime.
    #[test]
    fn scoped_poll_wakes_on_cancel_and_releases_registration_on_drop() {
        let cancellation = Cancellation::new().unwrap();
        let scope = TestScope {
            cancellation: Some(cancellation.clone()),
            error: Rc::new(Cell::new(None)),
        };
        let count = Arc::new(WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let mut future = Box::pin(poll_scoped(&scope, |_| -> Poll<Result<()>> {
            Poll::Pending
        }));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        cancellation.cancel().unwrap();
        assert_eq!(count.count(), 1);
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        drop(future);
        assert_eq!(Arc::strong_count(&count), 2);
    }

    /// Ready results pass through and failed subscription prevents polling application work.
    #[test]
    fn scoped_poll_preserves_ready_results_and_subscription_failure() {
        let cancellation = Cancellation::new().unwrap();
        let scope = TestScope {
            cancellation: Some(cancellation.clone()),
            error: Rc::new(Cell::new(None)),
        };
        assert_eq!(
            futures::executor::block_on(poll_scoped(&scope, |_| Poll::Ready(Ok::<_, Error>(7)))),
            Ok(7)
        );
        assert_eq!(
            futures::executor::block_on(poll_scoped(&scope, |_| Poll::Ready(Err::<(), _>(
                Error::Io
            )))),
            Err(Error::Io)
        );
        let _registrations: Vec<_> = (0..1024)
            .map(|_| cancellation.subscribe().unwrap())
            .collect();
        assert_eq!(
            futures::executor::block_on(poll_scoped(&scope, |_| -> Poll<Result<()>> {
                panic!("subscription must fail first")
            })),
            Err(Error::Overloaded)
        );
    }

    /// Busy guards reject reentry and release exclusion during normal and panic paths.
    #[test]
    fn busy_excludes_reentry_and_releases_on_drop_and_unwind() {
        let flag = Cell::new(false);
        let guard = Busy::try_enter(&flag).unwrap();
        assert!(matches!(Busy::try_enter(&flag), Err(Error::Overloaded)));
        assert!(flag.get());
        drop(guard);
        assert!(!flag.get());
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let _guard = Busy::try_enter(&flag).unwrap();
            panic!("unwind");
        }));
        assert!(result.is_err());
        assert!(!flag.get());
        drop(Busy::try_enter(&flag).unwrap());
    }

    /// Backend failures retain their first error without truncating operation completion.
    #[test]
    fn driver_retains_operation_and_first_backend_error_until_completion() {
        for fail_poll in [false, true] {
            let ready = Cell::new(false);
            let waits = Cell::new(0);
            let operation = Box::pin(std::future::poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Err::<(), _>(Error::Unavailable))
                } else {
                    Poll::Pending
                }
            }));
            let mut future = drive_local_with(
                operation,
                None,
                |_| if fail_poll { Err(Error::Io) } else { Ok(()) },
                || {
                    waits.set(waits.get() + 1);
                    Err(Error::Overloaded)
                },
            );
            let mut cx = Context::from_waker(Waker::noop());
            assert!(future.as_mut().poll(&mut cx).is_pending());
            ready.set(true);
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(Err(if fail_poll {
                    Error::Io
                } else {
                    Error::Overloaded
                }))
            );
            assert_eq!(waits.get(), 1);
        }
    }

    /// An already completed operation preserves its result and never invokes waiting.
    #[test]
    fn driver_preserves_ready_value_and_does_not_wait() {
        for result in [Ok(7), Err(Error::Unavailable)] {
            let mut future = drive_local_with(
                Box::pin(async move { result }),
                None,
                |_| Ok(()),
                || panic!("ready operation must not wait"),
            );
            assert_eq!(
                future
                    .as_mut()
                    .poll(&mut Context::from_waker(Waker::noop())),
                Poll::Ready(result)
            );
        }
    }

    /// Cross-thread notification unparks the thread that created the waker.
    #[test]
    fn thread_waker_targets_creator_from_another_thread() {
        let (send, receive) = std::sync::mpsc::channel();
        let target = thread::spawn(move || {
            send.send(thread_waker(None)).unwrap();
            thread::park_timeout(std::time::Duration::from_secs(2));
            send.send(thread_waker(None)).unwrap();
        });
        let waker = receive
            .recv_timeout(std::time::Duration::from_secs(2))
            .unwrap();
        waker.wake_by_ref();
        // The second message confirms the target resumed, not the waking thread.
        receive
            .recv_timeout(std::time::Duration::from_secs(1))
            .unwrap();
        target.join().unwrap();
    }
}

/// Deterministic listener retry timing, exhaustion, and accepted-work fencing.
#[cfg(all(test, feature = "simulation"))]
mod retry_tests {
    use super::*;
    use crate::{Result, test_util::WakeCounter};
    use std::{
        cell::Cell,
        rc::Rc,
        sync::Arc,
        task::{Context, Waker},
    };

    /// Exposes a shared caller-controlled scope failure without automatic wakeups.
    #[derive(Clone, Default)]
    struct TestScope(Rc<Cell<Option<Error>>>);
    impl Scope for TestScope {
        /// Retry fixtures use the runtime error type directly.
        type Error = Error;

        /// Read the failure selected by the fixture without changing its state.
        fn check(&self) -> Result<()> {
            self.0.get().map_or(Ok(()), Err)
        }
    }

    /// Retry bounds and invalid intervals fail without unbounded or immediate resubmission.
    #[test]
    fn configured_retry_limit_interval_zero_and_overflow() {
        let clock = environment::SimulationClock::new(4);
        let _role = clock.environment(0).enter();
        let scope = TestScope::default();
        let attempts = Cell::new(0);
        let mut future = retry_listener_with(
            &scope,
            RetryOptions {
                interval: Duration::from_millis(3),
                max_retries: Some(1),
            },
            || {
                attempts.set(attempts.get() + 1);
                Box::pin(async { Err::<(), _>(Error::Overloaded) })
            },
        );
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        clock.advance(Duration::from_millis(2));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        assert_eq!(attempts.get(), 1);
        clock.advance(Duration::from_millis(1));
        assert_eq!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        );
        assert_eq!(attempts.get(), 2);
        for (interval, error) in [
            (Duration::ZERO, Error::InvalidConfiguration),
            (Duration::MAX, Error::InvalidInput),
        ] {
            let mut future = retry_listener_with(
                &scope,
                RetryOptions {
                    interval,
                    max_retries: None,
                },
                || Box::pin(async { Err::<(), _>(Error::Overloaded) }),
            );
            assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Err(error)));
        }
    }

    /// Default retry timing requires owner polling and rechecks policy before resubmission.
    #[test]
    fn retries_only_after_ten_ms_without_self_wakes_and_checks_scope() {
        let clock = environment::SimulationClock::new(45);
        let _environment = clock.environment(0).enter();
        for cancel in [false, true] {
            let scope = TestScope::default();
            let attempts = Cell::new(0);
            let wake = Arc::new(WakeCounter::default());
            let waker = Waker::from(wake.clone());
            let mut cx = Context::from_waker(&waker);
            let mut future = retry_listener(&scope, || {
                let attempt = attempts.get();
                attempts.set(attempt + 1);
                Box::pin(async move {
                    if attempt == 0 {
                        Err(Error::Overloaded)
                    } else {
                        Ok(7)
                    }
                })
            });
            assert!(future.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(9));
            assert!(future.as_mut().poll(&mut cx).is_pending());
            assert_eq!(attempts.get(), 1);
            assert_eq!(wake.count(), 0);
            if cancel {
                scope.0.set(Some(Error::Cancelled));
            }
            clock.advance(Duration::from_millis(1));
            assert_eq!(
                future.as_mut().poll(&mut cx),
                Poll::Ready(if cancel { Err(Error::Cancelled) } else { Ok(7) })
            );
            assert_eq!(attempts.get(), if cancel { 1 } else { 2 });
        }
    }

    /// Scope failure cannot truncate an accepted operation's completion fence.
    #[test]
    fn accepted_pending_operation_is_not_truncated_by_scope_failure() {
        let scope = TestScope::default();
        let completed = Cell::new(false);
        let attempts = Cell::new(0);
        let mut future = retry_listener(&scope, || {
            attempts.set(attempts.get() + 1);
            Box::pin(std::future::poll_fn(|_| {
                if completed.get() {
                    Poll::Ready(Err::<(), _>(Error::Io))
                } else {
                    Poll::Pending
                }
            }))
        });
        let mut cx = Context::from_waker(Waker::noop());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.0.set(Some(Error::Cancelled));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        completed.set(true);
        assert_eq!(future.as_mut().poll(&mut cx), Poll::Ready(Err(Error::Io)));
        assert_eq!(attempts.get(), 1);
    }
}

/// Pure hedge winner precedence, cancellation requests, and separate child fences.
#[cfg(test)]
mod hedge_tests {
    use super::*;
    use crate::{Error, environment::Cancellation};
    use std::{cell::Cell, rc::Rc, task::Waker};

    /// Cancellation-only scope shared by a contender and its controlling fixture.
    #[derive(Clone)]
    struct TestScope(Cancellation);
    impl TestScope {
        /// Create an independently cancelable child or parent scope.
        fn new() -> Self {
            Self(Cancellation::new().unwrap())
        }
    }
    impl crate::Scope for TestScope {
        /// Hedge fixtures use portable runtime failures.
        type Error = Error;

        /// Admit the contender unless its scope has been canceled.
        fn check(&self) -> Result<(), Error> {
            if self.0.is_cancelled() {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }

        /// Supply cancellation wakeups while the parent still owns admission.
        fn cancellation(&self) -> Option<&Cancellation> {
            Some(&self.0)
        }
    }
    impl HedgeScope for TestScope {
        /// Notify the child without claiming it has reached completion.
        fn cancel(&self) {
            self.0.cancel().unwrap();
        }
    }

    /// Borrowed delay control and outcome observations for a single hedge race.
    struct Hooks<'a> {
        due: &'a Cell<bool>,

        won: &'a Cell<Option<Contender>>,

        failures: &'a Cell<usize>,
    }
    impl HedgePolicy<Error> for Hooks<'_> {
        /// Launch the delayed contender only when the fixture marks the alarm due.
        fn delay(&mut self, _: &mut Context<'_>) -> Poll<()> {
            if self.due.get() {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        }

        /// Permit another contender to recover only from the fixture's I/O failure.
        fn recoverable(&self, error: Error) -> bool {
            error == Error::Io
        }

        /// Count and report exhaustion when neither contender produced a winner.
        fn failure(&mut self) -> Error {
            self.failures.set(self.failures.get() + 1);
            Error::NotFound
        }

        /// Record the first successful contender before the loser is drained.
        fn won(&mut self, contender: Contender) {
            self.won.set(Some(contender));
        }
    }
    /// Owns hedge delay state and policy observations without an executor.
    #[derive(Default)]
    struct Fixture {
        due: Cell<bool>,

        won: Cell<Option<Contender>>,

        failures: Cell<usize>,
    }
    impl Fixture {
        /// Borrow policy controls for one race while retaining external observations.
        fn hooks(&self) -> Hooks<'_> {
            Hooks {
                due: &self.due,
                won: &self.won,
                failures: &self.failures,
            }
        }
    }

    /// Parent cancellation requests each child once while still classifying drained errors.
    #[test]
    fn hedge_requests_each_cancel_once_and_classifies_drained_errors() {
        /// Counts cancellation requests independently of the shared cancellation bit.
        #[derive(Clone)]
        struct CountedScope {
            inner: TestScope,

            requests: Rc<Cell<usize>>,
        }
        impl crate::Scope for CountedScope {
            /// Preserve the wrapped scope's runtime failure type.
            type Error = Error;

            /// Delegate caller policy to the underlying cancellation scope.
            fn check(&self) -> Result<()> {
                self.inner.check()
            }

            /// Share the underlying scope's cancellation subscription source.
            fn cancellation(&self) -> Option<&Cancellation> {
                self.inner.cancellation()
            }
        }
        impl HedgeScope for CountedScope {
            /// Count every request before forwarding cancellation to the child.
            fn cancel(&self) {
                self.requests.set(self.requests.get() + 1);
                self.inner.cancel();
            }
        }
        /// Counts classification callbacks while rejecting impossible winning paths.
        struct CountedPolicy<'a>(&'a Cell<usize>);
        impl HedgePolicy<Error> for CountedPolicy<'_> {
            /// Launch the secondary immediately after the first pending primary poll.
            fn delay(&mut self, _: &mut Context<'_>) -> Poll<()> {
                Poll::Ready(())
            }

            /// Count classification even while the race drains a prior fatal failure.
            fn recoverable(&self, _: Error) -> bool {
                self.0.set(self.0.get() + 1);
                false
            }

            /// Reject exhaustion because parent cancellation must determine the result.
            fn failure(&mut self) -> Error {
                panic!("parent cancellation must win")
            }

            /// Reject winners because both fixture children complete with failures.
            fn won(&mut self, _: Contender) {
                panic!("drained errors cannot win")
            }
        }
        let scope = || CountedScope {
            inner: TestScope::new(),
            requests: Rc::new(Cell::new(0)),
        };
        let a = scope();
        let b = scope();
        let parent = scope();
        let fenced = Cell::new(false);
        let classified = Cell::new(0);
        let child = || {
            std::future::poll_fn(|_| {
                if fenced.get() {
                    Poll::Ready(Err::<(), _>(Error::Io))
                } else {
                    Poll::Pending
                }
            })
        };
        let mut work = Box::pin(race(
            child(),
            child(),
            &a,
            &b,
            &parent,
            CountedPolicy(&classified),
        ));
        let mut cx = Context::from_waker(Waker::noop());
        assert!(work.as_mut().poll(&mut cx).is_pending());
        parent.cancel();
        for _ in 0..3 {
            assert!(work.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!((a.requests.get(), b.requests.get()), (1, 1));
        fenced.set(true);
        assert_eq!(
            work.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        assert_eq!(classified.get(), 2);
        assert_eq!((a.requests.get(), b.requests.get()), (1, 1));
    }

    /// Any immediately ready primary prevents the delayed contender from starting.
    #[test]
    fn hedge_fast_primary_success_or_failure_never_launches_secondary() {
        for result in [
            Ok(Rc::new(String::from("primary"))),
            Err(Error::Io),
            Err(Error::InvalidInput),
        ] {
            let fixture = Fixture::default();
            fixture.due.set(true);
            let a = TestScope::new();
            let b = TestScope::new();
            let parent = TestScope::new();
            let expected = if result == Err(Error::Io) {
                Err(Error::NotFound)
            } else {
                result.clone()
            };
            let output = futures::executor::block_on(race(
                std::future::ready(result),
                async { panic!("secondary launched after primary completed") },
                &a,
                &b,
                &parent,
                fixture.hooks(),
            ));
            assert_eq!(output, expected);
            assert_eq!(
                fixture.won.get(),
                expected.is_ok().then_some(Contender::Primary)
            );
            assert_eq!(
                fixture.failures.get(),
                usize::from(expected == Err(Error::NotFound))
            );
        }
    }

    /// Cancellation before admission prevents either child from being polled.
    #[test]
    fn hedge_canceled_parent_never_polls_either_future() {
        let fixture = Fixture::default();
        let a = TestScope::new();
        let b = TestScope::new();
        let parent = TestScope::new();
        parent.cancel();
        let never = || {
            std::future::poll_fn(|_| -> Poll<Result<(), Error>> {
                panic!("submitted after cancellation")
            })
        };
        assert_eq!(
            futures::executor::block_on(race(never(), never(), &a, &b, &parent, fixture.hooks())),
            Err(Error::Cancelled)
        );
        assert_eq!(fixture.won.get(), None);
        assert_eq!(fixture.failures.get(), 0);
    }

    /// Cancellation before the delay drains only the primary and never launches new work.
    #[test]
    fn hedge_cancellation_before_delay_drains_primary_without_launching_secondary() {
        let fixture = Fixture::default();
        let primary_scope = TestScope::new();
        let secondary_scope = TestScope::new();
        let parent = TestScope::new();
        let fenced = Cell::new(false);
        let primary = std::future::poll_fn(|_| {
            if fenced.get() {
                Poll::Ready(Ok(7))
            } else {
                Poll::Pending
            }
        });
        let secondary = async { panic!("canceled race launched delayed work") };
        let mut work = Box::pin(race(
            primary,
            secondary,
            &primary_scope,
            &secondary_scope,
            &parent,
            fixture.hooks(),
        ));
        let mut cx = Context::from_waker(Waker::noop());
        assert!(work.as_mut().poll(&mut cx).is_pending());
        parent.cancel();
        fixture.due.set(true);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert!(primary_scope.0.is_cancelled());
        assert!(secondary_scope.0.is_cancelled());
        assert_eq!(fixture.won.get(), None);
        fenced.set(true);
        assert_eq!(
            work.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        assert_eq!(fixture.failures.get(), 0);
    }

    /// A secondary winner waits for the primary fence and may lose to parent cancellation.
    #[test]
    fn hedge_secondary_winner_waits_for_fence_and_parent_can_override() {
        for cancel_parent in [false, true] {
            let fixture = Fixture::default();
            let a = TestScope::new();
            let b = TestScope::new();
            let parent = TestScope::new();
            let fenced = Cell::new(false);
            let primary = std::future::poll_fn(|_| {
                if fenced.get() {
                    Poll::Ready(Err(Error::Cancelled))
                } else {
                    Poll::Pending
                }
            });
            let mut work = Box::pin(race(
                primary,
                async { Ok(String::from("secondary")) },
                &a,
                &b,
                &parent,
                fixture.hooks(),
            ));
            let mut cx = Context::from_waker(Waker::noop());
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(fixture.won.get(), None);
            fixture.due.set(true);
            assert!(work.as_mut().poll(&mut cx).is_pending());
            assert_eq!(fixture.won.get(), Some(Contender::Secondary));
            assert!(a.0.is_cancelled());
            assert!(!parent.0.is_cancelled());
            if cancel_parent {
                parent.cancel();
            }
            assert!(work.as_mut().poll(&mut cx).is_pending());
            fenced.set(true);
            let expected = if cancel_parent {
                Err(Error::Cancelled)
            } else {
                Ok(String::from("secondary"))
            };
            assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(expected));
            assert_eq!(fixture.failures.get(), 0);
        }
    }

    /// Recoverable and terminal errors select different cancellation and failure paths.
    #[test]
    fn hedge_classification_controls_cancellation_and_empty_failure_hook() {
        for secondary_error in [Error::Io, Error::InvalidInput] {
            for primary_result in [Ok(7), Err(Error::Io)] {
                let fixture = Fixture::default();
                fixture.due.set(true);
                let a = TestScope::new();
                let b = TestScope::new();
                let parent = TestScope::new();
                let fenced = Cell::new(false);
                let primary = std::future::poll_fn(|_| {
                    if fenced.get() {
                        Poll::Ready(primary_result)
                    } else {
                        Poll::Pending
                    }
                });
                let mut work = Box::pin(race(
                    primary,
                    std::future::ready(Err(secondary_error)),
                    &a,
                    &b,
                    &parent,
                    fixture.hooks(),
                ));
                let mut cx = Context::from_waker(Waker::noop());
                assert!(work.as_mut().poll(&mut cx).is_pending());
                assert_eq!(a.0.is_cancelled(), secondary_error == Error::InvalidInput);
                fenced.set(true);
                let expected = if secondary_error == Error::InvalidInput {
                    Err(secondary_error)
                } else {
                    primary_result.map_err(|_| Error::NotFound)
                };
                assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(expected));
                assert_eq!(
                    fixture.won.get(),
                    expected.is_ok().then_some(Contender::Primary)
                );
                assert_eq!(
                    fixture.failures.get(),
                    usize::from(expected == Err(Error::NotFound))
                );
            }
        }
    }

    /// Parent cancellation wakes once and waits for both independently completed fences.
    #[test]
    fn hedge_parent_cancellation_wakes_and_drains_both_separate_fences() {
        let fixture = Fixture::default();
        fixture.due.set(true);
        let a = TestScope::new();
        let b = TestScope::new();
        let parent = TestScope::new();
        let a_fence = Cell::new(false);
        let b_fence = Cell::new(false);
        let child = |fence: &Cell<bool>| {
            let ready = fence.get();
            if ready {
                Poll::Ready(Ok(9))
            } else {
                Poll::Pending
            }
        };
        let mut work = Box::pin(race(
            std::future::poll_fn(|_| child(&a_fence)),
            std::future::poll_fn(|_| child(&b_fence)),
            &a,
            &b,
            &parent,
            fixture.hooks(),
        ));
        let wake = std::sync::Arc::new(crate::test_util::WakeCounter::default());
        let waker = Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        parent.cancel();
        assert_eq!(wake.count(), 1);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        assert!(a.0.is_cancelled() && b.0.is_cancelled());
        for _ in 0..100 {
            assert!(work.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(
            wake.count(),
            1,
            "draining canceled children must not busy-wake"
        );
        a_fence.set(true);
        assert!(work.as_mut().poll(&mut cx).is_pending());
        b_fence.set(true);
        assert_eq!(
            work.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        );
        assert_eq!(fixture.won.get(), None);
        assert_eq!(fixture.failures.get(), 0);
    }

    /// The primary wins readiness ties without abandoning an already launched secondary.
    #[test]
    fn hedge_simultaneous_readiness_prefers_primary_but_drains_secondary() {
        let fixture = Fixture::default();
        fixture.due.set(true);
        let a = TestScope::new();
        let b = TestScope::new();
        let parent = TestScope::new();
        let ready = Cell::new(false);
        let secondary_drained = Cell::new(false);
        let primary = std::future::poll_fn(|_| {
            if ready.get() {
                Poll::Ready(Ok(1))
            } else {
                Poll::Pending
            }
        });
        let secondary = std::future::poll_fn(|_| {
            if ready.get() {
                assert!(b.0.is_cancelled());
                secondary_drained.set(true);
                Poll::Ready(Ok(2))
            } else {
                Poll::Pending
            }
        });
        let mut work = Box::pin(race(primary, secondary, &a, &b, &parent, fixture.hooks()));
        let mut cx = Context::from_waker(Waker::noop());
        assert!(work.as_mut().poll(&mut cx).is_pending());
        ready.set(true);
        assert_eq!(work.as_mut().poll(&mut cx), Poll::Ready(Ok(1)));
        assert!(secondary_drained.get());
        assert_eq!(fixture.won.get(), Some(Contender::Primary));
    }
}
