//! A bounded, single-thread executor for `!Send` tasks.
//!
//! [`LocalExecutor`] owns a fixed-capacity set of tasks and a wake-driven
//! ready queue. A task is polled only after its waker fired (or once, right
//! after it was spawned), so idle tasks cost nothing per turn. Nothing runs in
//! the background: the application decides when to run tasks and how many.
//!
//! # Model
//!
//! - **Bounded admission.** Every task and every unused [`Permit`] occupies one
//!   unit of capacity. [`spawn`](LocalExecutor::spawn) fails with
//!   [`Overloaded`](crate::ErrorKind::Overloaded) when the executor
//!   is full. Use [`reserve`](LocalExecutor::reserve) to claim capacity before
//!   building an expensive future. Capacity is released *before* a finished,
//!   aborted or panicked task's future is dropped, so its destructor can spawn
//!   a replacement. (An `async` block drops its own locals when it returns or
//!   unwinds, inside its final poll; only state the future still owns
//!   afterwards, such as fields of a hand-written future, observes this
//!   ordering.)
//! - **Ready queue.** A woken task is queued once, however often it is woken
//!   before it runs. One [`run_ready`](LocalExecutor::run_ready) turn polls at
//!   most `budget` tasks, and only tasks that were already queued when the turn
//!   started: tasks woken or spawned during the turn (including a task that
//!   [yields](yield_now)) run in the next turn, so a turn always terminates.
//! - **Thread-safe wakers.** Task wakers are `Send + Sync` and carry only a task
//!   id. Waking from another thread queues the task and wakes the *owner*: the
//!   waker most recently passed to [`poll`](LocalExecutor::poll). Tasks
//!   themselves never leave the executor's thread.
//! - **Re-entrancy.** Tasks, wakers and destructors may spawn, abort, wake and
//!   inspect the executor freely; no internal borrow is held while user code
//!   runs. A nested [`poll`](LocalExecutor::poll) or
//!   [`run_ready`](LocalExecutor::run_ready) from inside a task is a no-op that
//!   returns `0` and leaves the owner registration untouched.
//! - **Panics.** If a task panics while being polled, the executor removes it,
//!   releases its capacity, drops it, and then resumes the panic in the caller
//!   of `run_ready`/`poll`. Its [`JoinHandle`] resolves to an
//!   [`Unavailable`](crate::ErrorKind::Unavailable) error. The executor stays
//!   usable; the remaining tasks are untouched. A panic from a task's
//!   destructor also propagates after capacity has been released.
//!
//! # Driving the executor with a reactor
//!
//! An application thread typically loops over three explicit steps: reap I/O
//! completions and expire timers (which wake tasks), run ready tasks, then
//! block in the reactor until something happens. Make the owner waker
//! interrupt that block (for example by wrapping the reactor's remote waker in
//! a [`Waker`](std::task::Waker)), so a wake from another thread is not missed:
//!
//! ```text
//! let owner = Waker::from(Arc::new(RemoteWake(reactor.remote_waker()?)));
//! let mut cx = Context::from_waker(&owner);
//! loop {
//!     reactor.poll(256)?;                       // completions wake tasks
//!     timers.poll(clock.now(), 256);            // expired sleeps wake tasks
//!     executor.poll(&mut cx, 64);               // run ready tasks, register owner
//!     if !executor.has_ready() {
//!         let idle = timers.next_deadline().map_or(MAX_WAIT, |d| d - clock.now());
//!         reactor.wait(idle.min(MAX_WAIT))?;
//!     }
//! }
//! ```
//!
//! # Example
//!
//! ```
//! use std::task::{Context, Waker};
//! use uring_runtime::task::{LocalExecutor, yield_now};
//!
//! let executor = LocalExecutor::new(16);
//! let spawner = executor.spawner();
//! let mut handle = executor
//!     .spawn(async move {
//!         // Tasks can spawn more tasks through a `Spawner`.
//!         let child = spawner.spawn(async { 20 }).unwrap();
//!         yield_now().await;
//!         child.await.unwrap() + 1
//!     })
//!     .unwrap();
//!
//! let mut cx = Context::from_waker(Waker::noop());
//! while !handle.is_finished() {
//!     executor.poll(&mut cx, 8);
//! }
//! let output = std::pin::Pin::new(&mut handle).poll(&mut cx);
//! assert_eq!(output.map(Result::unwrap), std::task::Poll::Ready(21));
//! assert!(executor.is_empty());
//! # use std::future::Future;
//! ```

mod join;
mod raw;
#[cfg(test)]
mod tests;

use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::rc::{Rc, Weak};
use std::task::{Context, Poll};

pub use join::JoinHandle;
use join::JoinState;
use raw::{Inner, TaskCell};

use crate::{Error, ErrorKind, Result};

/// A bounded, single-thread executor for `!Send` futures.
///
/// See the [module documentation](self) for the scheduling, re-entrancy and
/// panic rules. Dropping the executor aborts every task (their handles resolve
/// to [`Canceled`](crate::ErrorKind::Canceled)) and makes every [`Spawner`]
/// fail with [`Unavailable`](crate::ErrorKind::Unavailable).
///
/// The executor cannot move between threads:
///
/// ```compile_fail
/// fn require_send<T: Send>() {}
/// require_send::<uring_runtime::task::LocalExecutor>();
/// ```
pub struct LocalExecutor {
    inner: Rc<Inner>,
}

impl LocalExecutor {
    /// Create an executor that admits at most `capacity` tasks and permits at
    /// a time. A capacity of zero rejects every spawn.
    pub fn new(capacity: usize) -> Self {
        Self {
            inner: Inner::new(capacity),
        }
    }

    /// A cheap, cloneable handle for spawning onto this executor, for example
    /// from inside a task.
    pub fn spawner(&self) -> Spawner {
        Spawner {
            inner: Rc::downgrade(&self.inner),
        }
    }

    /// Spawn `future`, returning a handle to its output.
    ///
    /// The task is queued for its first poll. Fails with
    /// [`Overloaded`](crate::ErrorKind::Overloaded) at capacity, dropping
    /// `future`; use [`reserve`](Self::reserve) to check admission before
    /// building the future.
    pub fn spawn<F>(&self, future: F) -> Result<JoinHandle<F::Output>>
    where
        F: Future + 'static,
    {
        self.inner.admit()?;
        Ok(spawn_admitted(&self.inner, future))
    }

    /// Spawn `future` without a handle; its output is dropped as soon as it is
    /// produced. Fails like [`spawn`](Self::spawn).
    pub fn spawn_detached<F>(&self, future: F) -> Result<()>
    where
        F: Future + 'static,
    {
        self.inner.admit()?;
        spawn_detached_admitted(&self.inner, future);
        Ok(())
    }

    /// Claim one unit of capacity for a task spawned later through
    /// [`Permit::spawn`]. Dropping an unused permit returns the capacity.
    pub fn reserve(&self) -> Result<Permit> {
        self.inner.admit()?;
        Ok(Permit {
            inner: Some(Rc::downgrade(&self.inner)),
        })
    }

    /// Run one turn of at most `budget` ready tasks and return how many were
    /// polled.
    ///
    /// Only tasks queued before the turn started are eligible. Does not
    /// register an owner waker; use [`poll`](Self::poll) when another thread
    /// may wake tasks. Returns `0` without doing anything when called from
    /// inside a task of this executor.
    ///
    /// # Panics
    ///
    /// Resumes a task's panic after the task has been removed and dropped.
    pub fn run_ready(&self, budget: usize) -> usize {
        self.inner.run_ready(budget)
    }

    /// Register `cx`'s waker as the owner waker, then run one turn like
    /// [`run_ready`](Self::run_ready).
    ///
    /// The owner waker is woken (once) when a task becomes ready, including
    /// from another thread. If ready tasks remain after the turn and `budget`
    /// is not zero, it is woken before returning, so a caller that sleeps until
    /// woken never strands ready work. Nested calls from inside a task return
    /// `0` without replacing the owner.
    ///
    /// # Panics
    ///
    /// Resumes a task's panic, like [`run_ready`](Self::run_ready).
    pub fn poll(&self, cx: &mut Context<'_>, budget: usize) -> usize {
        if self.inner.is_polling() {
            return 0;
        }
        self.inner.shared().set_owner(cx.waker());
        let polled = self.inner.run_ready(budget);
        if budget != 0 && self.inner.has_ready() {
            cx.waker().wake_by_ref();
        }
        polled
    }

    /// Abort every task without polling it. Capacity is released before each
    /// task is dropped; tasks are dropped outside every internal borrow, so
    /// their destructors may spawn new tasks (which survive). Unused permits
    /// keep their capacity. A task that calls this from inside its own poll
    /// is dropped when that poll returns.
    pub fn abort_all(&self) {
        self.inner.abort_all();
    }

    /// The number of tasks that have not finished (excluding unused permits).
    pub fn len(&self) -> usize {
        self.inner.len()
    }

    /// Whether there are no tasks.
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// The number of unused permits.
    pub fn reserved(&self) -> usize {
        self.inner.reserved()
    }

    /// The maximum number of tasks plus unused permits.
    pub fn capacity(&self) -> usize {
        self.inner.capacity()
    }

    /// Whether a task is queued to run. This may briefly report `true` for a
    /// task that was aborted after being woken; the next turn discards it.
    pub fn has_ready(&self) -> bool {
        self.inner.has_ready()
    }
}

impl Drop for LocalExecutor {
    fn drop(&mut self) {
        self.inner.close();
    }
}

impl fmt::Debug for LocalExecutor {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("LocalExecutor")
            .field("capacity", &self.capacity())
            .field("len", &self.len())
            .field("reserved", &self.reserved())
            .finish_non_exhaustive()
    }
}

fn spawn_admitted<F>(inner: &Rc<Inner>, future: F) -> JoinHandle<F::Output>
where
    F: Future + 'static,
{
    let state = JoinState::new();
    let key = inner.insert(Box::pin(TaskCell::new(future, Some(state.clone()))));
    JoinHandle::new(state, Rc::downgrade(inner), key)
}

fn spawn_detached_admitted<F>(inner: &Rc<Inner>, future: F)
where
    F: Future + 'static,
{
    inner.insert(Box::pin(TaskCell::new(future, None)));
}

fn shut_down() -> Error {
    Error::new(ErrorKind::Unavailable).with_context("executor shut down")
}

/// A handle for spawning onto a [`LocalExecutor`] without owning it.
///
/// Spawners are cheap to clone and do not keep the executor alive: once it is
/// dropped, every method fails with
/// [`Unavailable`](crate::ErrorKind::Unavailable). Like the executor, a
/// spawner stays on its thread.
#[derive(Clone)]
pub struct Spawner {
    inner: Weak<Inner>,
}

impl Spawner {
    fn inner(&self) -> Result<Rc<Inner>> {
        self.inner.upgrade().ok_or_else(shut_down)
    }

    /// Same as [`LocalExecutor::spawn`].
    pub fn spawn<F>(&self, future: F) -> Result<JoinHandle<F::Output>>
    where
        F: Future + 'static,
    {
        let inner = self.inner()?;
        inner.admit()?;
        Ok(spawn_admitted(&inner, future))
    }

    /// Same as [`LocalExecutor::spawn_detached`].
    pub fn spawn_detached<F>(&self, future: F) -> Result<()>
    where
        F: Future + 'static,
    {
        let inner = self.inner()?;
        inner.admit()?;
        spawn_detached_admitted(&inner, future);
        Ok(())
    }

    /// Same as [`LocalExecutor::reserve`].
    pub fn reserve(&self) -> Result<Permit> {
        let inner = self.inner()?;
        inner.admit()?;
        Ok(Permit {
            inner: Some(self.inner.clone()),
        })
    }

    /// Whether the executor still exists.
    pub fn is_alive(&self) -> bool {
        self.inner.strong_count() != 0
    }
}

impl fmt::Debug for Spawner {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Spawner")
            .field("alive", &self.is_alive())
            .finish()
    }
}

/// One unit of executor capacity, claimed by [`LocalExecutor::reserve`] or
/// [`Spawner::reserve`].
///
/// Spawning through a permit cannot fail for lack of capacity. Dropping an
/// unused permit returns its capacity. Permits survive
/// [`abort_all`](LocalExecutor::abort_all).
#[must_use = "an unused permit holds executor capacity until dropped"]
pub struct Permit {
    /// `None` once the capacity has been handed to a task.
    inner: Option<Weak<Inner>>,
}

impl Permit {
    /// Spawn `future` using this permit's capacity.
    ///
    /// If the executor has been dropped, `future` is dropped and the handle
    /// resolves to an [`Unavailable`](crate::ErrorKind::Unavailable) error.
    pub fn spawn<F>(self, future: F) -> JoinHandle<F::Output>
    where
        F: Future + 'static,
    {
        match self.consume() {
            Some(inner) => spawn_admitted(&inner, future),
            None => {
                drop(future);
                JoinHandle::failed(shut_down())
            }
        }
    }

    /// Spawn `future` without a handle using this permit's capacity. If the
    /// executor has been dropped, `future` is dropped.
    pub fn spawn_detached<F>(self, future: F)
    where
        F: Future + 'static,
    {
        if let Some(inner) = self.consume() {
            spawn_detached_admitted(&inner, future);
        }
    }

    /// Hand the capacity over to a task, so `Drop` no longer releases it.
    fn consume(mut self) -> Option<Rc<Inner>> {
        let inner = self.inner.take()?.upgrade()?;
        if inner.is_closed() {
            inner.release_permit();
            return None;
        }
        Some(inner)
    }
}

impl Drop for Permit {
    fn drop(&mut self) {
        if let Some(inner) = self.inner.take().and_then(|weak| weak.upgrade()) {
            inner.release_permit();
        }
    }
}

impl fmt::Debug for Permit {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Permit").finish_non_exhaustive()
    }
}

/// Yield to the executor once.
///
/// The returned future is pending exactly once and wakes its task before
/// returning `Pending`, so the task is queued behind every task that was
/// already ready.
pub fn yield_now() -> YieldNow {
    YieldNow { yielded: false }
}

/// The future returned by [`yield_now`].
#[derive(Debug)]
#[must_use = "futures do nothing unless polled"]
pub struct YieldNow {
    yielded: bool,
}

impl Future for YieldNow {
    type Output = ();

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.yielded {
            return Poll::Ready(());
        }
        self.yielded = true;
        cx.waker().wake_by_ref();
        Poll::Pending
    }
}
