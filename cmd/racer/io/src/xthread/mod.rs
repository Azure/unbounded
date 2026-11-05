//! Cross-thread queues: a lock-free bounded SPSC ring ([`spsc`]) and one
//! bounded MPSC mailbox with typed request/reply ([`mailbox`]).
//!
//! Producers on other threads can wake a consumer parked in its ring through
//! a [`Notify`] (the lane's [`crate::ring::WakeHandle`] implements it).
pub mod mailbox;
pub mod offload;
pub mod spsc;

use std::sync::Arc;

/// Wakes a consumer that is parked outside the queue.
pub trait Notify: Send + Sync {
    /// Wake the consumer; must be cheap and must not block.
    fn notify(&self);
}

impl Notify for crate::ring::WakeHandle {
    /// Wake the lane through its eventfd (coalesced).
    fn notify(&self) {
        self.wake();
    }
}

impl<N: Notify + ?Sized> Notify for Arc<N> {
    /// Forward to the shared notifier.
    fn notify(&self) {
        (**self).notify();
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    /// Counts owned and borrowed wakes across threads.
    #[derive(Default)]
    pub(crate) struct WakeCounter(AtomicUsize);

    impl WakeCounter {
        /// Wakes observed so far.
        pub(crate) fn count(&self) -> usize {
            self.0.load(Ordering::SeqCst)
        }
    }

    impl std::task::Wake for WakeCounter {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    impl super::Notify for WakeCounter {
        fn notify(&self) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }
}
