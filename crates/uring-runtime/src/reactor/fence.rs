//! Fence futures returned by [`Reactor::drain`] and
//! [`Reactor::fence_matching`].

use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use crate::{Backend, Error, Result};

use super::Reactor;
use super::table::Entry;

/// Resolves once a set of operations has been fenced: every completion,
/// including cancel request completions, has arrived, so the kernel no
/// longer references their memory.
///
/// `Ok(())` is the only proof of a fence. If the reactor fails while
/// operations are still in flight, the fence resolves to the failure
/// instead. Dropping a `Fence` does not undo the cancellation it requested.
#[must_use = "a fence does nothing unless awaited"]
pub struct Fence<'r, B: Backend> {
    reactor: &'r Reactor<B>,
    target: Target,
}

enum Target {
    /// Every operation in flight.
    All,
    /// The listed keys; fenced keys are removed as they are observed.
    Keys(Vec<u64>),
    Failed(Option<Error>),
}

impl<'r, B: Backend> Fence<'r, B> {
    pub(super) fn all(reactor: &'r Reactor<B>) -> Self {
        Self {
            reactor,
            target: Target::All,
        }
    }

    pub(super) fn keys(reactor: &'r Reactor<B>, keys: Vec<u64>) -> Self {
        Self {
            reactor,
            target: Target::Keys(keys),
        }
    }

    pub(super) fn failed(reactor: &'r Reactor<B>, error: Error) -> Self {
        Self {
            reactor,
            target: Target::Failed(Some(error)),
        }
    }

    /// The number of operations this fence still waits for (for a drain
    /// fence: everything in flight).
    pub fn remaining(&self) -> usize {
        let state = self.reactor.state.borrow();
        match &self.target {
            Target::All => state.in_flight,
            Target::Keys(keys) => keys
                .iter()
                .filter(|key| state.table.get(**key).is_some_and(Entry::in_flight))
                .count(),
            Target::Failed(_) => 0,
        }
    }
}

impl<B: Backend> Future for Fence<'_, B> {
    type Output = Result<()>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<()>> {
        let this = &mut *self;
        if let Target::Failed(error) = &mut this.target {
            return Poll::Ready(Err(error.take().expect("fence polled after completion")));
        }
        {
            let state = this.reactor.state.borrow();
            if let Some(ready) = this.target.check(&state) {
                return Poll::Ready(ready);
            }
            if state.fence_waiters.iter().any(|w| w.will_wake(cx.waker())) {
                return Poll::Pending;
            }
        }
        // Clone outside the borrow: a waker's clone may re-enter the reactor
        // and reap the final completion, so check again before registering.
        let waker = cx.waker().clone();
        let mut state = this.reactor.state.borrow_mut();
        if let Some(ready) = this.target.check(&state) {
            return Poll::Ready(ready);
        }
        state.fence_waiters.push(waker);
        Poll::Pending
    }
}

impl Target {
    /// Returns the fence's result if it is resolved under `state`.
    fn check<B: Backend>(&mut self, state: &super::State<B>) -> Option<Result<()>> {
        let done = match self {
            Target::Failed(_) => unreachable!("failed fences resolve before checking"),
            Target::All => state.in_flight == 0,
            Target::Keys(keys) => {
                keys.retain(|key| state.table.get(*key).is_some_and(Entry::in_flight));
                keys.is_empty()
            }
        };
        if done {
            return Some(Ok(()));
        }
        state.failure.as_ref().map(|error| Err(error.clone()))
    }
}

impl<B: Backend> fmt::Debug for Fence<'_, B> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Fence")
            .field("remaining", &self.remaining())
            .finish()
    }
}
