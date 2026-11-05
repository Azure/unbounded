//! One local executor per lane, the lane loop, and the [`Role`] abstraction.
//!
//! [`LocalExecutor`] owns a slab of `!Send` tasks and a ready queue. Wakers
//! push the task index; a wake from another thread also wakes the lane through
//! its [`WakeHandle`] (an eventfd polled on the ring), so a parked lane has
//! exactly one wait primitive and never polls on a fixed interval.
//!
//! Every thread is a [`Role`]: [`Role::step`] does a bounded amount of
//! nonblocking work and reports when it next needs to run. Production runs a
//! role on its own pinned thread with [`run`]; a deterministic test steps many
//! roles on one thread and advances virtual time itself.
use crate::{
    Result,
    ring::{Reactor, WakeHandle},
};
use std::{
    cell::{Cell, RefCell},
    future::Future,
    pin::Pin,
    rc::Rc,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    task::{Context, Poll, Wake, Waker},
    time::Instant,
};

/// What a role needs after one step.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Next {
    /// More work is ready; step again without waiting.
    Ready,
    /// Nothing is ready before this instant (or an earlier wake source).
    WaitUntil(Instant),
    /// Nothing is ready until a wake source fires.
    Idle,
    /// The role finished.
    Done,
}

/// A schedulable unit of work bound to one thread.
pub trait Role {
    /// Do bounded nonblocking work and report what comes next.
    fn step(&mut self) -> Result<Next>;

    /// Block until the condition reported by `next` (production only).
    fn park(&mut self, next: Next) -> Result<()>;
}

/// Run `role` on the calling thread until it reports [`Next::Done`].
pub fn run(role: &mut impl Role) -> Result<()> {
    loop {
        match role.step()? {
            Next::Done => return Ok(()),
            next => role.park(next)?,
        }
    }
}

/// Unique token of the calling thread, cheaper than `thread::current().id()`.
fn thread_token() -> u64 {
    static NEXT: AtomicU64 = AtomicU64::new(1);
    thread_local! {
        static TOKEN: u64 = NEXT.fetch_add(1, Ordering::Relaxed);
    }
    TOKEN.with(|token| *token)
}

/// Ready queue shared with wakers.
struct Shared {
    queue: Mutex<Vec<u64>>,

    owner: u64,

    notify: Option<Arc<WakeHandle>>,
}

/// One task's waker.
struct TaskWaker {
    shared: Arc<Shared>,

    id: u64,

    queued: AtomicBool,
}

impl Wake for TaskWaker {
    /// Queue the task once until it is polled again.
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    /// Queue the task; wake the lane if called from another thread.
    fn wake_by_ref(self: &Arc<Self>) {
        if self.queued.swap(true, Ordering::AcqRel) {
            return;
        }
        self.shared
            .queue
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(self.id);
        if thread_token() != self.shared.owner
            && let Some(notify) = &self.shared.notify
        {
            notify.wake();
        }
    }
}

struct Task {
    generation: u32,

    future: Option<Pin<Box<dyn Future<Output = ()>>>>,

    waker: Option<Arc<TaskWaker>>,
}

/// A lane-local executor for `!Send` futures.
pub struct LocalExecutor {
    shared: Arc<Shared>,

    tasks: RefCell<Vec<Task>>,

    free: RefCell<Vec<u32>>,

    live: Cell<usize>,

    /// Spawns made while tasks run; queued after the batch.
    batch: RefCell<Vec<u64>>,
}

impl LocalExecutor {
    /// An executor whose cross-thread wakes notify `wake` (pass the lane
    /// reactor's [`Reactor::wake_handle`]); `None` for single-thread use.
    pub fn new(wake: Option<Arc<WakeHandle>>) -> Self {
        Self {
            shared: Arc::new(Shared {
                queue: Mutex::new(Vec::new()),
                owner: thread_token(),
                notify: wake,
            }),
            tasks: RefCell::new(Vec::new()),
            free: RefCell::new(Vec::new()),
            live: Cell::new(0),
            batch: RefCell::new(Vec::new()),
        }
    }

    /// Spawn a task; it is polled on the next [`Self::run_ready`].
    pub fn spawn<F: Future + 'static>(&self, future: F) -> JoinHandle<F::Output> {
        let state = Rc::new(RefCell::new(JoinState {
            result: None,
            waker: None,
            finished: false,
        }));
        let output = state.clone();
        let task = async move {
            let value = future.await;
            let mut state = output.borrow_mut();
            state.result = Some(value);
            state.finished = true;
            let waker = state.waker.take();
            drop(state);
            if let Some(waker) = waker {
                waker.wake();
            }
        };
        self.spawn_boxed(Box::pin(task));
        JoinHandle { state }
    }

    /// Spawn a task whose output is discarded.
    pub fn spawn_detached<F: Future<Output = ()> + 'static>(&self, future: F) {
        self.spawn_boxed(Box::pin(future));
    }

    fn spawn_boxed(&self, future: Pin<Box<dyn Future<Output = ()>>>) {
        let mut tasks = self.tasks.borrow_mut();
        let index = match self.free.borrow_mut().pop() {
            Some(index) => index,
            None => {
                tasks.push(Task {
                    generation: 0,
                    future: None,
                    waker: None,
                });
                (tasks.len() - 1) as u32
            }
        };
        let task = &mut tasks[index as usize];
        task.generation = task.generation.wrapping_add(1);
        let id = (u64::from(task.generation) << 32) | u64::from(index);
        task.future = Some(future);
        task.waker = Some(Arc::new(TaskWaker {
            shared: self.shared.clone(),
            id,
            queued: AtomicBool::new(true),
        }));
        self.live.set(self.live.get() + 1);
        self.shared
            .queue
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(id);
    }

    /// Live tasks.
    pub fn len(&self) -> usize {
        self.live.get()
    }

    /// Whether no task is live.
    pub fn is_empty(&self) -> bool {
        self.live.get() == 0
    }

    /// Whether some task is queued to run.
    pub fn has_ready(&self) -> bool {
        !self
            .shared
            .queue
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .is_empty()
    }

    /// Poll up to `budget` queued tasks; returns how many were polled.
    pub fn run_ready(&self, budget: usize) -> usize {
        let mut batch = std::mem::take(&mut *self.batch.borrow_mut());
        {
            let mut queue = self.shared.queue.lock().unwrap_or_else(|e| e.into_inner());
            let take = queue.len().min(budget);
            batch.extend(queue.drain(..take));
        }
        let mut polled = 0;
        for id in batch.drain(..) {
            let index = (id & 0xFFFF_FFFF) as usize;
            let generation = (id >> 32) as u32;
            let (mut future, waker) = {
                let mut tasks = self.tasks.borrow_mut();
                let Some(task) = tasks.get_mut(index) else {
                    continue;
                };
                if task.generation != generation {
                    continue;
                }
                let Some(future) = task.future.take() else {
                    continue;
                };
                let waker = task.waker.clone().expect("live task waker");
                (future, waker)
            };
            waker.queued.store(false, Ordering::Release);
            polled += 1;
            let std_waker = Waker::from(waker);
            let mut cx = Context::from_waker(&std_waker);
            let done = future.as_mut().poll(&mut cx).is_ready();
            let mut tasks = self.tasks.borrow_mut();
            let task = &mut tasks[index];
            if done {
                task.waker = None;
                drop(tasks);
                drop(future);
                self.free.borrow_mut().push(index as u32);
                self.live.set(self.live.get() - 1);
            } else {
                task.future = Some(future);
            }
        }
        let mut stored = self.batch.borrow_mut();
        if stored.capacity() == 0 {
            *stored = batch;
        }
        polled
    }

    /// Drive `future` to completion on `reactor` with this executor, for
    /// bootstrap and tests.
    pub fn block_on<F: Future + 'static>(&self, reactor: &Reactor, future: F) -> Result<F::Output> {
        let handle = self.spawn(future);
        loop {
            reactor.reap(256)?;
            self.run_ready(256);
            if let Some(value) = handle.try_take() {
                return Ok(value);
            }
            if self.has_ready() || reactor.has_ready() {
                continue;
            }
            reactor.park(reactor.next_deadline())?;
        }
    }
}

impl Drop for LocalExecutor {
    /// Drop task futures (abandoning their operations) before the queue.
    fn drop(&mut self) {
        let tasks = std::mem::take(&mut *self.tasks.borrow_mut());
        drop(tasks);
    }
}

struct JoinState<T> {
    result: Option<T>,

    waker: Option<Waker>,

    finished: bool,
}

/// Awaits a spawned task's output. Dropping it detaches the task.
pub struct JoinHandle<T> {
    state: Rc<RefCell<JoinState<T>>>,
}

impl<T> JoinHandle<T> {
    /// Take the output if the task finished.
    pub fn try_take(&self) -> Option<T> {
        self.state.borrow_mut().result.take()
    }

    /// Whether the task finished.
    pub fn is_finished(&self) -> bool {
        self.state.borrow().finished
    }
}

impl<T> Future for JoinHandle<T> {
    type Output = T;

    /// Ready with the task's output.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<T> {
        let mut state = self.state.borrow_mut();
        match state.result.take() {
            Some(value) => Poll::Ready(value),
            None => {
                assert!(!state.finished, "JoinHandle polled after taking the output");
                state.waker = Some(cx.waker().clone());
                Poll::Pending
            }
        }
    }
}

/// Lane hooks called once per turn.
pub trait Tick {
    /// Periodic application work; returns when it next needs to run.
    fn tick(&mut self, now: Instant) -> Option<Instant> {
        let _ = now;
        None
    }

    /// Whether the lane should exit.
    fn done(&self) -> bool {
        false
    }
}

impl Tick for () {}

/// An I/O lane: reactor, executor, and application tick, as a [`Role`].
///
/// Each step is `reap (budget) -> run ready tasks (budget) -> tick`; parking
/// is one `submit_and_wait` bounded by the next deadline.
pub struct Lane<T> {
    /// The lane reactor.
    pub reactor: Reactor,

    /// The lane executor.
    pub executor: LocalExecutor,

    /// Application hooks.
    pub tick: T,

    /// CQEs reaped per step.
    pub reap_budget: usize,

    /// Tasks polled per step.
    pub run_budget: usize,
}

impl<T: Tick> Lane<T> {
    /// A lane over `reactor` with default budgets (256 each).
    pub fn new(reactor: Reactor, tick: T) -> Self {
        let executor = LocalExecutor::new(Some(reactor.wake_handle()));
        Self {
            reactor,
            executor,
            tick,
            reap_budget: 256,
            run_budget: 256,
        }
    }
}

impl<T> Lane<T> {
    /// Whether a step could make progress without waiting: a task is ready,
    /// a completion (or runnable simulated operation) is ready, or a
    /// cross-thread wake is pending.
    pub fn has_pending_wake(&self) -> bool {
        self.executor.has_ready() || self.reactor.has_ready()
    }

    /// Whether the lane holds unfinished work: live tasks or operations
    /// awaiting their completion fences.
    pub fn has_pending_work(&self) -> bool {
        !self.executor.is_empty() || self.reactor.in_flight() > 0
    }
}

impl<T: Tick> Role for Lane<T> {
    /// One lane turn.
    fn step(&mut self) -> Result<Next> {
        self.reactor.reap(self.reap_budget)?;
        self.executor.run_ready(self.run_budget);
        let now = self.reactor.now();
        let tick = self.tick.tick(now);
        if self.tick.done() {
            return Ok(Next::Done);
        }
        if self.executor.has_ready() || self.reactor.has_ready() {
            return Ok(Next::Ready);
        }
        let deadline = match (self.reactor.next_deadline(), tick) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        };
        Ok(deadline.map_or(Next::Idle, Next::WaitUntil))
    }

    /// Submit and wait on the ring.
    fn park(&mut self, next: Next) -> Result<()> {
        match next {
            Next::Ready | Next::Done => Ok(()),
            Next::WaitUntil(until) => self.reactor.park(Some(until)),
            Next::Idle => self.reactor.park(None),
        }
    }
}

/// Worker-local exclusion flag. Dropping the guard makes the flag available.
#[must_use = "dropping the guard releases the busy flag"]
pub struct Busy<'a>(&'a Cell<bool>);

impl<'a> Busy<'a> {
    /// Enter an idle flag, returning overload without disturbing an active owner.
    pub fn try_enter(flag: &'a Cell<bool>) -> crate::Result<Self> {
        if flag.replace(true) {
            Err(crate::Error::Overloaded)
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lane_reports_pending_wake_and_work() {
        let os = crate::sim::SimOs::new();
        let clock = crate::ctx::VirtualClock::new();
        let reactor = Reactor::new(
            crate::Driver::Sim(os.clone()),
            crate::ring::Config::new(8),
            crate::WorkerCtx::simulated(&clock, 1, 1),
        )
        .unwrap();
        let mut lane = Lane::new(reactor, ());
        assert!(!lane.has_pending_wake() && !lane.has_pending_work());
        let (_left, right) = os.socket_pair();
        let recv = lane.reactor.recv(
            std::rc::Rc::new(right),
            vec![0; 4],
            (),
            &crate::OpCtx::none(),
        );
        lane.executor.spawn_detached(async move {
            let _ = recv.await;
        });
        assert!(lane.has_pending_wake() && lane.has_pending_work());
        while lane.step().unwrap() == Next::Ready {}
        // The receive waits for a peer: work is pending, nothing wakes it.
        assert!(!lane.has_pending_wake());
        assert!(lane.has_pending_work());
        assert_eq!(lane.reactor.in_flight(), 1);
    }

    #[test]
    fn tasks_run_once_per_wake_and_join() {
        let executor = LocalExecutor::new(None);
        let counter = Rc::new(Cell::new(0));
        let seen = counter.clone();
        let handle = executor.spawn(async move {
            seen.set(seen.get() + 1);
            7
        });
        assert!(executor.has_ready());
        assert_eq!(executor.run_ready(16), 1);
        assert_eq!(handle.try_take(), Some(7));
        assert_eq!(counter.get(), 1);
        assert!(executor.is_empty());
        assert_eq!(executor.run_ready(16), 0);
    }

    #[test]
    fn wakes_from_another_thread_queue_the_task() {
        let executor = LocalExecutor::new(None);
        let slot: Rc<RefCell<Option<Waker>>> = Rc::default();
        let flag = Arc::new(AtomicBool::new(false));
        let (task_slot, task_flag) = (slot.clone(), flag.clone());
        let handle = executor.spawn(std::future::poll_fn(move |cx| {
            if task_flag.load(Ordering::Acquire) {
                Poll::Ready(())
            } else {
                *task_slot.borrow_mut() = Some(cx.waker().clone());
                Poll::Pending
            }
        }));
        executor.run_ready(16);
        assert!(!executor.has_ready());
        let waker = slot.borrow_mut().take().unwrap();
        let remote = flag.clone();
        std::thread::spawn(move || {
            remote.store(true, Ordering::Release);
            waker.wake();
        })
        .join()
        .unwrap();
        assert!(executor.has_ready());
        executor.run_ready(16);
        assert!(handle.is_finished());
    }

    #[test]
    fn duplicate_wakes_coalesce_and_budget_bounds_polls() {
        let executor = LocalExecutor::new(None);
        let polls = Rc::new(Cell::new(0));
        for _ in 0..4 {
            let polls = polls.clone();
            executor.spawn_detached(std::future::poll_fn(move |cx| {
                polls.set(polls.get() + 1);
                cx.waker().wake_by_ref();
                cx.waker().wake_by_ref();
                if polls.get() > 8 {
                    Poll::Ready(())
                } else {
                    Poll::Pending
                }
            }));
        }
        assert_eq!(executor.run_ready(3), 3);
        assert_eq!(executor.run_ready(100), 4);
        assert_eq!(polls.get(), 7);
    }
}

/// Return `Pending` once after waking the current task, so other ready work
/// on the same executor runs before this task continues.
pub async fn yield_now() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            std::task::Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    })
    .await
}

/// A waker that unparks the calling thread. No thread is spawned.
pub fn thread_waker() -> std::task::Waker {
    std::task::Waker::from(std::sync::Arc::new(ThreadWake(std::thread::current())))
}

/// Unparks one owner thread.
struct ThreadWake(std::thread::Thread);

impl std::task::Wake for ThreadWake {
    fn wake(self: std::sync::Arc<Self>) {
        self.0.unpark();
    }

    fn wake_by_ref(self: &std::sync::Arc<Self>) {
        self.0.unpark();
    }
}

#[cfg(test)]
mod thread_waker_tests {
    /// A thread waker unparks its owner, so a later park returns at once.
    #[test]
    fn thread_waker_unparks_owner() {
        let waker = super::thread_waker();
        waker.wake_by_ref();
        let started = std::time::Instant::now();
        std::thread::park_timeout(std::time::Duration::from_secs(10));
        assert!(started.elapsed() < std::time::Duration::from_secs(5));
    }
}
