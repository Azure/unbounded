//! Explicitly driven worker-local I/O and pinned thread groups.
//!
//! Scheduling and resource policy belong to the caller. Simulation is opt-in.
//! Generic mechanisms accept caller-owned scope, budget, result, and scheduling
//! hooks. Request models, admission classes, retry policy, entropy domain choices,
//! and service-graph ownership belong in application adapters, not this crate.
#![deny(unsafe_op_in_unsafe_fn)]

pub mod affinity;
pub mod channel;
pub mod clock_observer;
mod cooperative;
pub use cooperative::{Busy, drive_local_with, poll_scoped, thread_waker, yield_now};
pub mod deadline;
pub mod deadline_registry;
pub mod drivers;
pub mod environment;
pub mod group;
pub mod hedge;
pub mod mailbox;
pub mod offload;
pub mod reactor;
mod retry;
pub use retry::{RetryOptions, retry_listener, retry_listener_with};
#[cfg(any(test, feature = "test-util"))]
pub mod test_util;

use std::{future::Future, pin::Pin};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Cancelled,
    DeadlineExceeded,
    Overloaded,
    Unavailable,
    InvalidInput,
    InvalidConfiguration,
    NotFound,
    AlreadyExists,
    Io,
    /// A terminal OS failure. This is diagnostic, not an instruction to retry.
    Os(i32),
}

impl Error {
    pub fn from_io(error: std::io::Error) -> Self {
        match error.raw_os_error() {
            Some(libc::ENOENT) => Self::NotFound,
            Some(libc::EEXIST) => Self::AlreadyExists,
            Some(libc::ECANCELED) => Self::Cancelled,
            Some(errno) if errno > 0 => Self::Os(errno),
            _ => Self::Io,
        }
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Cancelled => "operation canceled",
            Self::DeadlineExceeded => "deadline exceeded",
            Self::Overloaded => "capacity exhausted",
            Self::Unavailable => "resource unavailable",
            Self::InvalidInput => "invalid input",
            Self::InvalidConfiguration => "invalid configuration",
            Self::NotFound => "resource not found",
            Self::AlreadyExists => "resource already exists",
            Self::Io => "I/O failure",
            Self::Os(errno) => return write!(f, "OS error {errno}"),
        })
    }
}

// Preserve Copy errors, including raw errno, without fabricating an error source.
impl std::error::Error for Error {}

pub type Result<T, E = Error> = std::result::Result<T, E>;
/// Worker-local future; no hidden executor and no Send requirement.
pub type Operation<'a, T, E = Error> = Pin<Box<dyn Future<Output = Result<T, E>> + 'a>>;

/// Caller-owned operation deadline and cancellation policy.
pub trait Scope: Clone + 'static {
    type Error: Copy + Send + 'static + From<Error>;

    fn check(&self) -> Result<(), Self::Error>;

    fn cancellation(&self) -> Option<&deadline::Cancellation> {
        None
    }
}

/// Accounting hook. Charges stay alive until their resources are fenced.
pub trait Budget: 'static {
    type Charge: 'static;

    fn charge(&self, bytes: usize) -> Result<Self::Charge>;
}

impl Budget for () {
    type Charge = ();

    fn charge(&self, _bytes: usize) -> Result<()> {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
