//! Worker-local scheduling, ambient environment, offload, and filesystem
//! helpers formerly in the `uring-runtime` crate. Relocated here so the crate
//! can be deleted; I/O itself lives in `racer-io`. Simulation helpers exist
//! only in test builds.

pub mod drivers;

pub mod environment;

pub mod reactor;

#[cfg(test)]
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
}

/// Portable runtime failures from `racer-io`.
pub use racer_io::Error;

/// A runtime result, optionally using an application-owned error type.
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Worker-local future; no hidden executor and no Send requirement.
pub use racer_io::scope::Operation;

pub use racer_io::scope::{CancelWait, Scope};

/// Runtime error and shared wake instrumentation contracts.
#[cfg(test)]
mod tests {
    use super::*;

    /// Shared wake instrumentation counts owned and borrowed notifications equally.
    #[test]
    fn counts_owned_and_borrowed_wakes() {
        let counter = std::sync::Arc::new(test_util::WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        waker.wake_by_ref();
        waker.wake();
        assert_eq!(counter.count(), 2);
    }

    /// Common errno values retain classifications while other positive diagnostics survive.
    #[test]
    fn os_errors_preserve_errno_and_existing_classifications() {
        for (errno, expected) in [
            (libc::ENOENT, Error::NotFound),
            (libc::EEXIST, Error::AlreadyExists),
            (libc::ECANCELED, Error::Cancelled),
            (libc::EIO, Error::Os(libc::EIO)),
            (libc::EINTR, Error::Os(libc::EINTR)),
        ] {
            assert_eq!(
                Error::from_io(std::io::Error::from_raw_os_error(errno)),
                expected
            );
        }
        assert_eq!(Error::from_io(std::io::Error::other("opaque")), Error::Io);
    }

    /// Portable failures describe themselves without fabricating nested error sources.
    #[test]
    fn classified_errors_implement_standard_error_without_fabricated_sources() {
        for error in [
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::Overloaded,
            Error::Unavailable,
            Error::InvalidInput,
            Error::InvalidConfiguration,
            Error::NotFound,
            Error::AlreadyExists,
            Error::Io,
        ] {
            let standard: &dyn std::error::Error = &error;
            assert!(!standard.to_string().is_empty());
            assert!(standard.source().is_none());
        }
    }
}
