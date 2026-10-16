//! Racer's platform layer over the `uring-runtime` library.
//!
//! The library is policy free. This crate recreates the ergonomic surface the
//! Racer crates used from the old internal runtime (`cmd/racer-dataplane/runtime`)
//! and holds the Racer policy the library intentionally does not own: a `Copy`
//! error type, ambient clock and entropy, bounded channels and mailboxes,
//! offload queues, explicitly polled driver queues, retry and hedging, and CPU
//! placement. See `designs/uring-runtime-library.md` for the split.
//!
//! The library itself is re-exported as [`uring`].
#![deny(unsafe_op_in_unsafe_fn)]
#![warn(missing_docs)]

pub use uring;

pub mod channel;

pub mod drivers;

pub mod environment;

pub mod error;

pub mod group;

pub mod mailbox;

pub mod offload;

pub mod reactor;

pub mod scope;

pub use error::{Error, Operation, Result};
pub use scope::{Budget, Cancellation, CancellationExt, CancellationRegistration, Scope};

#[cfg(any(test, feature = "test-util"))]
/// Optional shared wake instrumentation for tests, disabled in production builds.
pub mod test_util {
    use std::sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    };

    /// Counts owned and borrowed wakes across threads.
    #[derive(Default)]
    pub struct WakeCounter(AtomicUsize);

    impl WakeCounter {
        /// Read the number of wake calls observed so far.
        pub fn count(&self) -> usize {
            self.0.load(Ordering::SeqCst)
        }
    }

    impl std::task::Wake for WakeCounter {
        /// Count an owned wake; borrowed wakes use the trait's default forwarding.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        /// Shared wake instrumentation counts owned and borrowed notifications equally.
        #[test]
        fn counts_owned_and_borrowed_wakes() {
            let counter = Arc::new(WakeCounter::default());
            let waker = std::task::Waker::from(counter.clone());
            waker.wake_by_ref();
            waker.wake();
            assert_eq!(counter.count(), 2);
        }
    }
}
