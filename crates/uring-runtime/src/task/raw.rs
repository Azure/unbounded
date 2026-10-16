//! Executor internals: the task table, type-erased task cells, and the
//! thread-safe ready queue that task wakers push into.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::task::{Context, Poll, Wake, Waker};

use super::join::JoinState;
use crate::{Error, ErrorKind, Result};

/// Identifies one task: a table index plus the generation of that slot, so a
/// stale key (from a late wake) never reaches a task that reused the slot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct TaskKey {
    index: usize,
    generation: u64,
}

/// A type-erased task as stored in the table.
pub(super) type Task = Pin<Box<dyn ErasedTask>>;

/// The operations the executor needs on a task, independent of its output type.
pub(super) trait ErasedTask {
    /// Poll the task's future; on completion the output is stashed (not yet
    /// published) so the future can be dropped first.
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()>;

    /// Record that the task panicked, so its handle reports that instead of a
    /// cancellation when the task is dropped.
    fn mark_panicked(&self);
}

/// A future together with the handle state it reports to.
///
/// Field order matters: fields drop in declaration order, so `future` is
/// destroyed before `join` publishes the result and wakes the handle. This
/// holds during unwinding too: if the future's destructor panics, `join` is
/// still dropped and resolves the handle.
pub(super) struct TaskCell<F: Future> {
    future: F,
    join: Option<Completion<F::Output>>,
}

impl<F: Future> TaskCell<F> {
    pub(super) fn new(future: F, join: Option<Rc<JoinState<F::Output>>>) -> Self {
        Self {
            future,
            join: join.map(Completion),
        }
    }
}

impl<F: Future> ErasedTask for TaskCell<F> {
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        // SAFETY: `future` is structurally pinned. `TaskCell` has no `Drop`
        // impl, is never moved out of its pinned box, and no other code hands
        // out `&mut F`. `join` is only accessed through shared references.
        let this = unsafe { self.get_unchecked_mut() };
        // SAFETY: see above; `this.future` is never moved.
        let future = unsafe { Pin::new_unchecked(&mut this.future) };
        match future.poll(cx) {
            Poll::Pending => Poll::Pending,
            Poll::Ready(output) => {
                // Detached tasks drop their output here; joined tasks keep it
                // until the future itself has been dropped.
                if let Some(join) = &this.join {
                    join.0.stash(output);
                }
                Poll::Ready(())
            }
        }
    }

    fn mark_panicked(&self) {
        if let Some(join) = &self.join {
            join.0.mark_panicked();
        }
    }
}

/// Publishes a task's outcome to its handle when dropped.
struct Completion<T>(Rc<JoinState<T>>);

impl<T> Drop for Completion<T> {
    fn drop(&mut self) {
        self.0.publish();
    }
}

/// State shared with task wakers. This is the only part of the executor that
/// other threads can reach, so it holds no task data, only keys.
pub(super) struct Shared {
    ready: Mutex<VecDeque<TaskKey>>,
    owner: Mutex<Option<Waker>>,
    closed: AtomicBool,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    // No user code runs while these locks are held, so poisoning can only
    // come from an allocation failure; the data is still consistent.
    mutex.lock().unwrap_or_else(|e| e.into_inner())
}

impl Shared {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            ready: Mutex::new(VecDeque::new()),
            owner: Mutex::new(None),
            closed: AtomicBool::new(false),
        })
    }

    /// Queue `key` and notify the owner.
    fn push(&self, key: TaskKey) {
        if self.closed.load(Ordering::Acquire) {
            return;
        }
        lock(&self.ready).push_back(key);
        self.wake_owner();
    }

    /// Wake the registered owner waker, if any. The waker is taken (like an
    /// `AtomicWaker`): the owner re-registers on its next
    /// [`LocalExecutor::poll`](super::LocalExecutor::poll). It is woken
    /// outside the lock, so the callback may re-enter the executor.
    fn wake_owner(&self) {
        let owner = lock(&self.owner).take();
        if let Some(owner) = owner {
            owner.wake();
        }
    }

    /// Register `waker` as the owner. The caller clones it beforehand, because
    /// cloning can run arbitrary code; the replaced waker is dropped after the
    /// lock is released for the same reason.
    pub(super) fn set_owner(&self, waker: &Waker) {
        let mut owner = lock(&self.owner);
        if owner
            .as_ref()
            .is_some_and(|current| current.will_wake(waker))
        {
            return;
        }
        drop(owner);
        let new = waker.clone();
        owner = lock(&self.owner);
        let old = owner.replace(new);
        drop(owner);
        drop(old);
    }

    fn pop(&self) -> Option<TaskKey> {
        lock(&self.ready).pop_front()
    }

    fn ready_len(&self) -> usize {
        lock(&self.ready).len()
    }
}

/// The waker of one task. It carries only the task's key, so it is `Send +
/// Sync` even though the task itself never leaves the owner thread.
struct TaskWake {
    key: TaskKey,
    /// Set while the key is in the ready queue, so repeated wakes enqueue once.
    queued: AtomicBool,
    shared: Arc<Shared>,
}

impl Wake for TaskWake {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        if !self.queued.swap(true, Ordering::AcqRel) {
            self.shared.push(self.key);
        }
    }
}

enum SlotState {
    Vacant {
        next_free: Option<usize>,
    },
    Idle {
        task: Task,
        wake: Arc<TaskWake>,
    },
    /// The task has been moved out for polling; `abort` records a request to
    /// drop it once the poll returns.
    Running {
        abort: bool,
        wake: Arc<TaskWake>,
    },
}

struct Slot {
    generation: u64,
    state: SlotState,
}

/// What [`Inner::take`] found for a key.
enum Taken {
    Gone,
    Task(Task),
    Running,
}

/// The executor proper, shared by `LocalExecutor`, `Spawner`, `Permit` and
/// `JoinHandle` (the latter three through `Weak`).
pub(super) struct Inner {
    capacity: usize,
    /// Live tasks plus unused permits; never exceeds `capacity`.
    admitted: Cell<usize>,
    tasks: Cell<usize>,
    slots: RefCell<Vec<Slot>>,
    free: Cell<Option<usize>>,
    shared: Arc<Shared>,
    polling: Cell<bool>,
    closed: Cell<bool>,
}

impl Inner {
    pub(super) fn new(capacity: usize) -> Rc<Self> {
        Rc::new(Self {
            capacity,
            admitted: Cell::new(0),
            tasks: Cell::new(0),
            slots: RefCell::new(Vec::new()),
            free: Cell::new(None),
            shared: Shared::new(),
            polling: Cell::new(false),
            closed: Cell::new(false),
        })
    }

    pub(super) fn shared(&self) -> &Shared {
        &self.shared
    }

    pub(super) fn capacity(&self) -> usize {
        self.capacity
    }

    pub(super) fn len(&self) -> usize {
        self.tasks.get()
    }

    pub(super) fn reserved(&self) -> usize {
        self.admitted.get() - self.tasks.get()
    }

    pub(super) fn has_ready(&self) -> bool {
        self.shared.ready_len() != 0
    }

    /// Claim one unit of capacity.
    pub(super) fn admit(&self) -> Result<()> {
        if self.closed.get() {
            return Err(Error::new(ErrorKind::Unavailable).with_context("executor shut down"));
        }
        if self.admitted.get() >= self.capacity {
            return Err(Error::new(ErrorKind::Overloaded).with_context("executor at capacity"));
        }
        self.admitted.set(self.admitted.get() + 1);
        Ok(())
    }

    /// Return one unit of capacity that is not attached to a task.
    pub(super) fn release_permit(&self) {
        self.admitted.set(self.admitted.get() - 1);
    }

    /// Store an admitted task and queue it for its first poll.
    pub(super) fn insert(&self, task: Task) -> TaskKey {
        let mut slots = self.slots.borrow_mut();
        let index = match self.free.get() {
            Some(index) => {
                let SlotState::Vacant { next_free } = slots[index].state else {
                    unreachable!("free list points at an occupied slot");
                };
                self.free.set(next_free);
                index
            }
            None => {
                slots.push(Slot {
                    generation: 0,
                    state: SlotState::Vacant { next_free: None },
                });
                slots.len() - 1
            }
        };
        let slot = &mut slots[index];
        slot.generation = slot.generation.wrapping_add(1);
        let key = TaskKey {
            index,
            generation: slot.generation,
        };
        let wake = Arc::new(TaskWake {
            key,
            queued: AtomicBool::new(true),
            shared: self.shared.clone(),
        });
        slot.state = SlotState::Idle { task, wake };
        drop(slots);
        self.tasks.set(self.tasks.get() + 1);
        self.shared.push(key);
        key
    }

    /// Mark `index` vacant and release the task's capacity. Called with the
    /// table borrowed; the caller drops the task afterwards.
    fn vacate(&self, slots: &mut [Slot], index: usize) {
        slots[index].state = SlotState::Vacant {
            next_free: self.free.get(),
        };
        self.free.set(Some(index));
        self.tasks.set(self.tasks.get() - 1);
        self.admitted.set(self.admitted.get() - 1);
    }

    /// Remove the task for `key` unless it is being polled, in which case it
    /// is flagged for removal when its poll returns.
    fn take(&self, key: TaskKey) -> Taken {
        let mut slots = self.slots.borrow_mut();
        let Some(slot) = slots.get_mut(key.index) else {
            return Taken::Gone;
        };
        if slot.generation != key.generation {
            return Taken::Gone;
        }
        match &mut slot.state {
            SlotState::Vacant { .. } => Taken::Gone,
            SlotState::Running { abort, .. } => {
                *abort = true;
                Taken::Running
            }
            SlotState::Idle { .. } => {
                let state =
                    std::mem::replace(&mut slot.state, SlotState::Vacant { next_free: None });
                self.vacate(&mut slots, key.index);
                let SlotState::Idle { task, .. } = state else {
                    unreachable!()
                };
                Taken::Task(task)
            }
        }
    }

    /// Abort one task. Capacity is released before the task is dropped, and
    /// the drop happens outside every internal borrow.
    pub(super) fn abort(&self, key: TaskKey) {
        if let Taken::Task(task) = self.take(key) {
            drop(task);
        }
    }

    /// Abort every task. Tasks spawned by the destructors of aborted tasks
    /// survive; a task currently being polled is dropped when its poll returns.
    pub(super) fn abort_all(&self) {
        let mut drained = Vec::new();
        {
            let mut slots = self.slots.borrow_mut();
            for index in 0..slots.len() {
                match &mut slots[index].state {
                    SlotState::Vacant { .. } => {}
                    SlotState::Running { abort, .. } => *abort = true,
                    SlotState::Idle { .. } => {
                        let state = std::mem::replace(
                            &mut slots[index].state,
                            SlotState::Vacant { next_free: None },
                        );
                        self.vacate(&mut slots, index);
                        if let SlotState::Idle { task, .. } = state {
                            drained.push(task);
                        }
                    }
                }
            }
        }
        // Stale keys would otherwise make `has_ready` report work.
        let stale = std::mem::take(&mut *lock(&self.shared.ready));
        drop(stale);
        drop(drained);
    }

    /// Stop admitting tasks, stop accepting wakes, and abort all tasks.
    pub(super) fn close(&self) {
        self.closed.set(true);
        self.shared.closed.store(true, Ordering::Release);
        self.abort_all();
        let owner = lock(&self.shared.owner).take();
        drop(owner);
    }

    /// Poll at most `budget` tasks that were ready when the call started.
    pub(super) fn run_ready(&self, budget: usize) -> usize {
        let Some(_turn) = TurnGuard::enter(&self.polling) else {
            return 0;
        };
        let snapshot = self.shared.ready_len();
        let mut polled = 0;
        for _ in 0..snapshot {
            if polled == budget {
                break;
            }
            let Some(key) = self.shared.pop() else {
                break;
            };
            let Some((mut task, wake)) = self.start(key) else {
                continue;
            };
            polled += 1;
            // Clear before polling so a wake during the poll queues it again.
            wake.queued.store(false, Ordering::Release);
            let waker = Waker::from(wake);
            let mut cx = Context::from_waker(&waker);
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                task.as_mut().poll(&mut cx)
            }));
            match result {
                Ok(Poll::Pending) => {
                    if let Some(task) = self.finish_pending(key, task) {
                        drop(task);
                    }
                }
                Ok(Poll::Ready(())) => {
                    self.finish(key);
                    drop(task);
                }
                Err(payload) => {
                    task.mark_panicked();
                    self.finish(key);
                    drop(task);
                    std::panic::resume_unwind(payload);
                }
            }
        }
        polled
    }

    /// Move an idle task out of the table for polling.
    fn start(&self, key: TaskKey) -> Option<(Task, Arc<TaskWake>)> {
        let mut slots = self.slots.borrow_mut();
        let slot = slots.get_mut(key.index)?;
        if slot.generation != key.generation || !matches!(slot.state, SlotState::Idle { .. }) {
            return None;
        }
        let SlotState::Idle { task, wake } =
            std::mem::replace(&mut slot.state, SlotState::Vacant { next_free: None })
        else {
            unreachable!()
        };
        slot.state = SlotState::Running {
            abort: false,
            wake: wake.clone(),
        };
        Some((task, wake))
    }

    /// Put a pending task back, or hand it back for dropping if it was aborted
    /// while running.
    fn finish_pending(&self, key: TaskKey, task: Task) -> Option<Task> {
        let mut slots = self.slots.borrow_mut();
        let slot = &mut slots[key.index];
        let SlotState::Running { abort, wake } =
            std::mem::replace(&mut slot.state, SlotState::Vacant { next_free: None })
        else {
            unreachable!("polled task slot changed state");
        };
        if abort {
            self.vacate(&mut slots, key.index);
            Some(task)
        } else {
            slot.state = SlotState::Idle { task, wake };
            None
        }
    }

    /// Release a completed (or panicked) task's slot and capacity.
    fn finish(&self, key: TaskKey) {
        let mut slots = self.slots.borrow_mut();
        self.vacate(&mut slots, key.index);
    }
}

/// Rejects nested executor turns; resets on return and on unwind.
struct TurnGuard<'a>(&'a Cell<bool>);

impl<'a> TurnGuard<'a> {
    fn enter(flag: &'a Cell<bool>) -> Option<Self> {
        if flag.replace(true) {
            None
        } else {
            Some(Self(flag))
        }
    }
}

impl Drop for TurnGuard<'_> {
    fn drop(&mut self) {
        self.0.set(false);
    }
}

impl Inner {
    pub(super) fn is_polling(&self) -> bool {
        self.polling.get()
    }

    pub(super) fn is_closed(&self) -> bool {
        self.closed.get()
    }
}
