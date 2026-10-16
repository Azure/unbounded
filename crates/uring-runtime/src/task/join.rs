//! [`JoinHandle`] and the state it shares with its task.

use std::cell::{Cell, RefCell};
use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::rc::{Rc, Weak};
use std::task::{Context, Poll, Waker};

use super::raw::{Inner, TaskKey};
use crate::{Error, ErrorKind, Result};

enum Slot<T> {
    /// The task has not finished.
    Running,
    /// The task finished; the output is held back until its future is dropped.
    Stashed(T),
    /// The outcome is visible to the handle.
    Ready(Result<T>),
    /// The handle already returned the outcome.
    Taken,
}

/// State shared between a task and its [`JoinHandle`].
pub(super) struct JoinState<T> {
    slot: RefCell<Slot<T>>,
    waker: RefCell<Option<Waker>>,
    panicked: Cell<bool>,
}

fn aborted() -> Error {
    Error::new(ErrorKind::Canceled).with_context("task aborted")
}

impl<T> JoinState<T> {
    pub(super) fn new() -> Rc<Self> {
        Rc::new(Self {
            slot: RefCell::new(Slot::Running),
            waker: RefCell::new(None),
            panicked: Cell::new(false),
        })
    }

    /// Hold the task's output until [`publish`](Self::publish). If the task
    /// was already aborted the output is dropped, outside the borrow.
    pub(super) fn stash(&self, output: T) {
        let mut slot = self.slot.borrow_mut();
        let rejected = if matches!(*slot, Slot::Running) {
            *slot = Slot::Stashed(output);
            None
        } else {
            Some(output)
        };
        drop(slot);
        drop(rejected);
    }

    pub(super) fn mark_panicked(&self) {
        self.panicked.set(true);
    }

    /// Make the outcome visible: the stashed output, or an error if the task
    /// was dropped before finishing. An outcome already set by an abort is
    /// kept.
    pub(super) fn publish(&self) {
        let mut slot = self.slot.borrow_mut();
        let next = match std::mem::replace(&mut *slot, Slot::Taken) {
            Slot::Running if self.panicked.get() => Slot::Ready(Err(Error::new(
                ErrorKind::Unavailable,
            )
            .with_context("task panicked"))),
            Slot::Running => Slot::Ready(Err(aborted())),
            Slot::Stashed(output) => Slot::Ready(Ok(output)),
            settled => {
                *slot = settled;
                return;
            }
        };
        *slot = next;
        drop(slot);
        self.wake();
    }

    /// Resolve a task that has not finished as aborted.
    fn abort(&self) {
        let mut slot = self.slot.borrow_mut();
        if !matches!(*slot, Slot::Running) {
            return;
        }
        *slot = Slot::Ready(Err(aborted()));
        drop(slot);
        self.wake();
    }

    /// Wake the handle, outside every borrow.
    fn wake(&self) {
        let waker = self.waker.borrow_mut().take();
        if let Some(waker) = waker {
            waker.wake();
        }
    }

    fn is_finished(&self) -> bool {
        matches!(*self.slot.borrow(), Slot::Ready(_) | Slot::Taken)
    }
}

/// An owned handle to a spawned task's output.
///
/// `JoinHandle<T>` is a future that resolves to:
///
/// - `Ok(output)` once the task finished and its future has been dropped;
/// - an [`ErrorKind::Canceled`] error if the task was aborted (through
///   [`abort`](Self::abort), [`LocalExecutor::abort_all`] or dropping the
///   executor) before it finished;
/// - an [`ErrorKind::Unavailable`] error (context `"task panicked"`) if the
///   task panicked. The panic itself propagates to whoever was running the
///   executor.
///
/// Dropping the handle detaches the task: it keeps running and its output is
/// dropped when it finishes. Polling the handle again after it returned its
/// outcome panics.
///
/// Like the executor, a handle stays on its thread:
///
/// ```compile_fail
/// fn require_send<T: Send>() {}
/// require_send::<uring_runtime::task::JoinHandle<()>>();
/// ```
///
/// [`LocalExecutor::abort_all`]: super::LocalExecutor::abort_all
#[must_use = "dropping a JoinHandle detaches the task; use `spawn_detached` to make that explicit"]
pub struct JoinHandle<T> {
    state: Rc<JoinState<T>>,
    executor: Weak<Inner>,
    key: Option<TaskKey>,
}

impl<T> JoinHandle<T> {
    pub(super) fn new(state: Rc<JoinState<T>>, executor: Weak<Inner>, key: TaskKey) -> Self {
        Self {
            state,
            executor,
            key: Some(key),
        }
    }

    /// A handle for a task that could not be started (its executor is gone).
    pub(super) fn failed(error: Error) -> Self {
        let state = JoinState::new();
        *state.slot.borrow_mut() = Slot::Ready(Err(error));
        Self {
            state,
            executor: Weak::new(),
            key: None,
        }
    }

    /// Abort the task.
    ///
    /// If the task has not finished, the handle resolves to a
    /// [`Canceled`](ErrorKind::Canceled) error and the task's future is
    /// dropped now, after its capacity has been released. A task that aborts
    /// itself from inside its own poll is dropped as soon as that poll
    /// returns. Aborting a finished task does nothing.
    pub fn abort(&self) {
        self.state.abort();
        if let (Some(key), Some(executor)) = (self.key, self.executor.upgrade()) {
            executor.abort(key);
        }
    }

    /// Whether the outcome is available, that is, polling would return
    /// `Ready`.
    pub fn is_finished(&self) -> bool {
        self.state.is_finished()
    }
}

impl<T> Future for JoinHandle<T> {
    type Output = Result<T>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<T>> {
        // Clone before borrowing: a waker clone may run arbitrary code,
        // including code that completes this task.
        let waker = cx.waker().clone();
        {
            let mut slot = self.state.slot.borrow_mut();
            match std::mem::replace(&mut *slot, Slot::Taken) {
                Slot::Ready(result) => return Poll::Ready(result),
                Slot::Taken => panic!("JoinHandle polled after returning its output"),
                pending => *slot = pending,
            }
        }
        let old = self.state.waker.borrow_mut().replace(waker);
        drop(old);
        Poll::Pending
    }
}

impl<T> fmt::Debug for JoinHandle<T> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("JoinHandle")
            .field("finished", &self.is_finished())
            .finish_non_exhaustive()
    }
}
