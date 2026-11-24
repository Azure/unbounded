//! One local executor per lane, the lane loop, and the [`Role`] abstraction.
//!
//! [`LocalExecutor`] owns a slab of `!Send` tasks and a ready queue. Wakers
//! push the task index: a wake on the executor's own thread appends to a
//! thread-local queue without a lock, and a wake from another thread appends to
//! a small shared queue and also wakes the lane through its [`WakeHandle`] (an
//! eventfd polled on the ring), so a parked lane has exactly one wait primitive
//! and never polls on a fixed interval.
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
    collections::VecDeque,
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

/// Unique identity of one executor, distinct on every construction.
fn executor_key() -> u64 {
    static NEXT: AtomicU64 = AtomicU64::new(1);
    NEXT.fetch_add(1, Ordering::Relaxed)
}

// Ready ids pushed by wakers running on the executor's own thread, keyed by
// executor because one thread can run several executors (the simulated world
// steps every lane on one thread).
thread_local! {
    static LOCAL_READY: RefCell<VecDeque<(u64, u64)>> =
        const { RefCell::new(VecDeque::new()) };
}

/// Ready queue shared with wakers.
struct Shared {
    /// Ids woken from another thread. The owner pulls them after its local
    /// queue, so a wake on the owner thread never takes this lock.
    remote: Mutex<VecDeque<u64>>,

    /// Identity of this executor in the thread-local ready queue.
    key: u64,

    owner: u64,

    notify: Option<Arc<WakeHandle>>,
}

/// Queue `id` on the calling thread when it owns `shared`'s executor, and on
/// the shared queue (notifying the lane) otherwise.
fn enqueue(shared: &Shared, id: u64) {
    if thread_token() == shared.owner {
        LOCAL_READY.with(|queue| queue.borrow_mut().push_back((shared.key, id)));
        return;
    }
    shared
        .remote
        .lock()
        .expect("ready queue lock is never poisoned")
        .push_back(id);
    if let Some(notify) = &shared.notify {
        notify.wake();
    }
}

/// Move up to `limit` ids of executor `key` from this thread's queue into
/// `batch`, in queue order, leaving other executors' ids queued.
fn take_local(key: u64, batch: &mut Vec<u64>, limit: usize) {
    LOCAL_READY.with(|queue| {
        let mut queue = queue.borrow_mut();
        // Rotate the deque once: pop the front, keep this executor's ids in
        // `batch` and push every other executor's id back. Popping `remaining`
        // times visits exactly the entries queued on entry and preserves the
        // order of both the taken ids and the ones left behind.
        let mut remaining = queue.len();
        let mut taken = 0;
        while remaining > 0 && taken < limit {
            let (owner, id) = queue.pop_front().expect("local queue is not empty");
            remaining -= 1;
            if owner == key {
                batch.push(id);
                taken += 1;
            } else {
                queue.push_back((owner, id));
            }
        }
    });
}

/// Whether executor `key` has a queued id on this thread.
fn local_ready(key: u64) -> bool {
    LOCAL_READY.with(|queue| queue.borrow().iter().any(|&(owner, _)| owner == key))
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
        enqueue(&self.shared, self.id);
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

    /// Reused drain buffer for the ready ids run in one turn.
    batch: RefCell<Vec<u64>>,
}

impl LocalExecutor {
    /// An executor whose cross-thread wakes notify `wake` (pass the lane
    /// reactor's [`Reactor::wake_handle`]); `None` for single-thread use.
    pub fn new(wake: Option<Arc<WakeHandle>>) -> Self {
        Self {
            shared: Arc::new(Shared {
                remote: Mutex::new(VecDeque::new()),
                key: executor_key(),
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
        // Spawning runs on the executor's thread, so this is a local push.
        enqueue(&self.shared, id);
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
        local_ready(self.shared.key)
            || !self
                .shared
                .remote
                .lock()
                .expect("ready queue lock is never poisoned")
                .is_empty()
    }

    /// Poll up to `budget` queued tasks; returns how many were polled.
    pub fn run_ready(&self, budget: usize) -> usize {
        let mut batch = std::mem::take(&mut *self.batch.borrow_mut());
        let mut remote = self
            .shared
            .remote
            .lock()
            .expect("ready queue lock is never poisoned");
        // Local ids run first. While remote ids wait, the local queue takes at
        // most half the budget, so a lane whose own tasks re-wake themselves on
        // every poll cannot starve wakers on other threads (any budget of two
        // or more polls at least one of them).
        let local_budget = if remote.is_empty() {
            budget
        } else {
            budget.div_ceil(2)
        };
        take_local(self.shared.key, &mut batch, local_budget);
        let take = remote.len().min(budget.saturating_sub(batch.len()));
        batch.extend(remote.drain(..take));
        drop(remote);
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

    /// Register the polling owner notified by the next wake.
    #[inline]
    pub fn register_owner(&self, waker: &Waker) {
        self.owner.register(waker);
    }

    /// Whether a wake is pending since the last poll.
    #[inline]
    pub fn is_ready(&self) -> bool {
        self.ready.load(Ordering::Acquire)
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
    fn budgeted_run_ready_polls_in_fifo_order() {
        let executor = LocalExecutor::new(None);
        let order = Rc::new(RefCell::new(Vec::new()));
        for index in 0..5 {
            let order = order.clone();
            executor.spawn_detached(async move { order.borrow_mut().push(index) });
        }
        assert_eq!(executor.run_ready(2), 2);
        assert_eq!(*order.borrow(), [0, 1]);
        assert_eq!(executor.run_ready(2), 2);
        assert_eq!(executor.run_ready(16), 1);
        assert_eq!(*order.borrow(), [0, 1, 2, 3, 4]);
        assert!(!executor.has_ready());
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

    /// A wake on the executor's own thread stays in the thread-local queue:
    /// the shared queue and its lock are never touched.
    #[test]
    fn same_thread_wakes_use_the_local_queue() {
        let executor = LocalExecutor::new(None);
        let slot: Rc<RefCell<Option<Waker>>> = Rc::default();
        let task_slot = slot.clone();
        let handle = executor.spawn(std::future::poll_fn(move |cx| -> Poll<()> {
            *task_slot.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }));
        assert_eq!(executor.run_ready(16), 1);
        let waker = slot.borrow_mut().take().unwrap();
        waker.wake_by_ref();
        assert!(
            executor
                .shared
                .remote
                .lock()
                .expect("ready queue lock is never poisoned")
                .is_empty()
        );
        assert!(executor.has_ready());
        assert_eq!(executor.run_ready(16), 1);
        assert!(!handle.is_finished());
    }

    /// Executors sharing a thread keep their queues apart: one neither sees nor
    /// polls the other's local wakes.
    #[test]
    fn executors_on_one_thread_drain_only_their_own_wakes() {
        let first = LocalExecutor::new(None);
        let second = LocalExecutor::new(None);
        let slot: Rc<RefCell<Option<Waker>>> = Rc::default();
        let polls = Rc::new(Cell::new(0));
        let (task_slot, task_polls) = (slot.clone(), polls.clone());
        let handle = first.spawn(std::future::poll_fn(move |cx| -> Poll<()> {
            task_polls.set(task_polls.get() + 1);
            *task_slot.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }));
        assert_eq!(first.run_ready(16), 1);
        slot.borrow_mut().as_ref().unwrap().wake_by_ref();
        assert!(!second.has_ready());
        assert_eq!(second.run_ready(16), 0);
        assert!(first.has_ready());
        assert_eq!(first.run_ready(16), 1);
        assert_eq!(polls.get(), 2);
        assert!(!handle.is_finished());
    }

    /// A simulated reactor for the cross-thread wake handle; no kernel needed.
    fn simulated_reactor() -> Reactor {
        let os = crate::sim::SimOs::new();
        let clock = crate::ctx::VirtualClock::new();
        Reactor::new(
            crate::ring::Driver::Sim(os),
            crate::ring::Config::new(64),
            crate::ctx::WorkerCtx::simulated(&clock, 1, 1),
        )
        .expect("simulated reactor")
    }

    /// A wake from another thread that lands after the local drain still
    /// notifies the lane through its wake handle, so a lane about to park sees
    /// it instead of sleeping through it.
    #[test]
    fn remote_wake_after_the_drain_notifies_the_lane() {
        let reactor = simulated_reactor();
        let executor = LocalExecutor::new(Some(reactor.wake_handle()));
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
        assert_eq!(executor.run_ready(16), 1);
        assert!(!executor.has_ready());
        let waker = slot.borrow_mut().take().unwrap();
        let remote = flag.clone();
        std::thread::spawn(move || {
            remote.store(true, Ordering::Release);
            waker.wake();
        })
        .join()
        .unwrap();
        assert!(
            !executor
                .shared
                .remote
                .lock()
                .expect("ready queue lock is never poisoned")
                .is_empty()
        );
        assert!(executor.has_ready());
        assert!(reactor.has_ready(), "the lane's wake handle is pending");
        assert_eq!(executor.run_ready(16), 1);
        assert!(handle.is_finished());
    }

    /// Remote ids share the turn budget with self-waking local tasks, so a
    /// saturated local queue cannot hold them off forever.
    #[test]
    fn remote_wakes_are_polled_alongside_self_waking_local_tasks() {
        let executor = LocalExecutor::new(None);
        let local_polls = Rc::new(Cell::new(0));
        for _ in 0..4 {
            let local_polls = local_polls.clone();
            executor.spawn_detached(std::future::poll_fn(move |cx| -> Poll<()> {
                local_polls.set(local_polls.get() + 1);
                cx.waker().wake_by_ref();
                Poll::Pending
            }));
        }
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
        // The first turn polls every task; the busy ones queue themselves again.
        assert_eq!(executor.run_ready(8), 5);
        let waker = slot.borrow_mut().take().unwrap();
        let remote = flag.clone();
        std::thread::spawn(move || {
            remote.store(true, Ordering::Release);
            waker.wake();
        })
        .join()
        .unwrap();
        // A two-poll turn serves one local id and the remote one.
        assert_eq!(executor.run_ready(2), 2);
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
