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
        let state = this.reactor.state.borrow();
        let done = match &mut this.target {
            Target::Failed(error) => {
                return Poll::Ready(Err(error.take().expect("fence polled after completion")));
            }
            Target::All => state.in_flight == 0,
            Target::Keys(keys) => {
                keys.retain(|key| state.table.get(*key).is_some_and(Entry::in_flight));
                keys.is_empty()
            }
        };
        if done {
            return Poll::Ready(Ok(()));
        }
        if let Some(error) = &state.failure {
            return Poll::Ready(Err(error.clone()));
        }
        if state.fence_waiters.iter().any(|w| w.will_wake(cx.waker())) {
            return Poll::Pending;
        }
        drop(state);
        // Clone outside the borrow: a waker's clone may re-enter the reactor.
        let waker = cx.waker().clone();
        this.reactor.state.borrow_mut().fence_waiters.push(waker);
        Poll::Pending
    }
}

impl<B: Backend> fmt::Debug for Fence<'_, B> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Fence")
            .field("remaining", &self.remaining())
            .finish()
    }
}
