//! Racer's portable, `Copy` runtime error.
//!
//! The library error ([`uring::Error`]) is deliberately not `Copy` so it can
//! grow. Racer's state machines store and compare errors by value, so they use
//! this small `Copy` enum instead and convert at the library boundary.

use std::{future::Future, pin::Pin};

/// Portable runtime failures, preserving terminal operating-system diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Error {
    /// The operation's owner requested cancellation.
    Canceled,
    /// The caller's deadline expired.
    DeadlineExceeded,
    /// A bounded queue or resource has no available capacity.
    Overloaded,
    /// A required resource cannot currently be used.
    Unavailable,
    /// An input value is invalid or cannot be represented.
    InvalidInput,
    /// The runtime was configured inconsistently or a required scope is absent.
    InvalidConfiguration,
    /// The requested resource does not exist.
    NotFound,
    /// The requested resource already exists.
    AlreadyExists,
    /// An I/O failure without a usable operating-system error code.
    Io,
    /// A terminal OS failure. This is diagnostic, not an instruction to retry.
    Os(i32),
}

impl Error {
    /// Classify an errno the way the old runtime did: `ENOENT` and `EEXIST`
    /// have dedicated variants, every other positive errno is kept in
    /// [`Error::Os`]. Kernel `ECANCELED` is `Os(ECANCELED)`, never
    /// [`Error::Canceled`], which is reserved for owner-requested cancellation.
    pub fn from_errno(errno: i32) -> Self {
        match errno.wrapping_abs() {
            libc::ENOENT => Self::NotFound,
            libc::EEXIST => Self::AlreadyExists,
            errno if errno > 0 => Self::Os(errno),
            _ => Self::Io,
        }
    }

    /// Preserve positive errno values while classifying common resource failures.
    ///
    /// Errors without an errno become [`Error::Io`].
    pub fn from_io(error: std::io::Error) -> Self {
        error.raw_os_error().map_or(Self::Io, Self::from_errno)
    }
}

impl std::fmt::Display for Error {
    /// Describe the failure without losing raw operating-system diagnostics.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Canceled => "operation canceled",
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

impl From<uring::Error> for Error {
    /// Convert a library error.
    ///
    /// An errno, when present, takes precedence and is classified with
    /// [`Error::from_errno`], so `EINVAL`, `ETIMEDOUT` and `ECANCELED` stay
    /// [`Error::Os`] exactly as the old runtime reported them. Without an
    /// errno the kind is mapped one to one; `Unsupported` becomes
    /// [`Error::Unavailable`] and an errno-less `Os` becomes [`Error::Io`].
    /// The library error's context string is dropped.
    fn from(error: uring::Error) -> Self {
        use uring::ErrorKind;

        if let Some(errno) = error.raw_os_error() {
            return Self::from_errno(errno);
        }
        match error.kind() {
            ErrorKind::Canceled => Self::Canceled,
            ErrorKind::DeadlineExceeded => Self::DeadlineExceeded,
            ErrorKind::Overloaded => Self::Overloaded,
            ErrorKind::Unavailable | ErrorKind::Unsupported => Self::Unavailable,
            ErrorKind::InvalidInput => Self::InvalidInput,
            ErrorKind::InvalidConfiguration => Self::InvalidConfiguration,
            ErrorKind::NotFound => Self::NotFound,
            ErrorKind::AlreadyExists => Self::AlreadyExists,
            _ => Self::Io,
        }
    }
}

impl From<Error> for uring::Error {
    /// Convert into the library error. [`Error::Os`] keeps its errno through
    /// [`uring::Error::from_errno`]; [`Error::Io`] becomes an errno-less `Os`.
    fn from(error: Error) -> Self {
        use uring::ErrorKind;

        let kind = match error {
            Error::Canceled => ErrorKind::Canceled,
            Error::DeadlineExceeded => ErrorKind::DeadlineExceeded,
            Error::Overloaded => ErrorKind::Overloaded,
            Error::Unavailable => ErrorKind::Unavailable,
            Error::InvalidInput => ErrorKind::InvalidInput,
            Error::InvalidConfiguration => ErrorKind::InvalidConfiguration,
            Error::NotFound => ErrorKind::NotFound,
            Error::AlreadyExists => ErrorKind::AlreadyExists,
            Error::Io => ErrorKind::Os,
            Error::Os(errno) => return Self::from_errno(errno),
        };
        Self::new(kind)
    }
}

/// A runtime result, optionally using an application-owned error type.
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Worker-local future; no hidden executor and no Send requirement.
pub type Operation<'a, T, E = Error> = Pin<Box<dyn Future<Output = Result<T, E>> + 'a>>;

#[cfg(test)]
mod tests {
    use super::*;

    /// Common errno values retain classifications while other positive diagnostics survive.
    #[test]
    fn os_errors_preserve_errno_and_existing_classifications() {
        for (errno, expected) in [
            (libc::ENOENT, Error::NotFound),
            (libc::EEXIST, Error::AlreadyExists),
            (libc::ECANCELED, Error::Os(libc::ECANCELED)),
            (libc::EIO, Error::Os(libc::EIO)),
            (libc::EINTR, Error::Os(libc::EINTR)),
        ] {
            assert_eq!(
                Error::from_io(std::io::Error::from_raw_os_error(errno)),
                expected
            );
        }
        assert_eq!(Error::from_io(std::io::Error::other("opaque")), Error::Io);
        assert_eq!(Error::from_errno(-libc::EPIPE), Error::Os(libc::EPIPE));
        assert_eq!(Error::from_errno(0), Error::Io);
    }

    /// Portable failures describe themselves without fabricating nested error sources.
    #[test]
    fn classified_errors_implement_standard_error_without_fabricated_sources() {
        for error in [
            Error::Canceled,
            Error::DeadlineExceeded,
            Error::Overloaded,
            Error::Unavailable,
            Error::InvalidInput,
            Error::InvalidConfiguration,
            Error::NotFound,
            Error::AlreadyExists,
            Error::Io,
            Error::Os(libc::EIO),
        ] {
            let standard: &dyn std::error::Error = &error;
            assert!(!standard.to_string().is_empty());
            assert!(standard.source().is_none());
        }
    }

    /// Library errors map by kind, and an errno always wins, including ECANCELED.
    #[test]
    fn library_errors_map_by_kind_and_keep_errno() {
        use uring::ErrorKind;

        for (kind, expected) in [
            (ErrorKind::Canceled, Error::Canceled),
            (ErrorKind::DeadlineExceeded, Error::DeadlineExceeded),
            (ErrorKind::Overloaded, Error::Overloaded),
            (ErrorKind::Unavailable, Error::Unavailable),
            (ErrorKind::Unsupported, Error::Unavailable),
            (ErrorKind::InvalidInput, Error::InvalidInput),
            (ErrorKind::InvalidConfiguration, Error::InvalidConfiguration),
            (ErrorKind::NotFound, Error::NotFound),
            (ErrorKind::AlreadyExists, Error::AlreadyExists),
            (ErrorKind::Os, Error::Io),
        ] {
            assert_eq!(
                Error::from(uring::Error::new(kind).with_context("x")),
                expected
            );
        }
        for (errno, expected) in [
            (libc::ECANCELED, Error::Os(libc::ECANCELED)),
            (libc::EINVAL, Error::Os(libc::EINVAL)),
            (libc::ETIMEDOUT, Error::Os(libc::ETIMEDOUT)),
            (libc::ENOENT, Error::NotFound),
            (libc::ECONNREFUSED, Error::Os(libc::ECONNREFUSED)),
        ] {
            assert_eq!(Error::from(uring::Error::from_errno(-errno)), expected);
        }
    }

    /// Converting to the library and back is lossless for every variant.
    #[test]
    fn library_round_trip() {
        for error in [
            Error::Canceled,
            Error::DeadlineExceeded,
            Error::Overloaded,
            Error::Unavailable,
            Error::InvalidInput,
            Error::InvalidConfiguration,
            Error::NotFound,
            Error::AlreadyExists,
            Error::Io,
            Error::Os(libc::EINVAL),
            Error::Os(libc::ECANCELED),
        ] {
            let library = uring::Error::from(error);
            assert_eq!(Error::from(library), error);
        }
        let library = uring::Error::from(Error::Os(libc::EPIPE));
        assert_eq!(library.raw_os_error(), Some(libc::EPIPE));
        assert!(uring::Error::from(Error::Canceled).is_canceled());
    }
}
