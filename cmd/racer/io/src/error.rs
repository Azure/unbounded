//! Portable I/O failures that preserve terminal operating-system diagnostics.

/// Portable runtime failures, preserving terminal operating-system diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Error {
    /// The operation's owner requested cancellation.
    Canceled,
    /// The operation's deadline expired.
    DeadlineExceeded,
    /// A bounded queue or resource has no available capacity.
    Overloaded,
    /// A required resource cannot currently be used, for example after stop.
    Unavailable,
    /// An input value is invalid or cannot be represented.
    InvalidInput,
    /// The runtime was configured inconsistently.
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
    /// Classify an [`std::io::Error`], keeping positive errno values.
    pub fn from_io(error: std::io::Error) -> Self {
        match error.raw_os_error() {
            Some(errno) => Self::from_errno(errno),
            None => Self::Io,
        }
    }

    /// Classify a positive errno value.
    pub fn from_errno(errno: i32) -> Self {
        match errno {
            libc::ENOENT => Self::NotFound,
            libc::EEXIST => Self::AlreadyExists,
            libc::ECANCELED => Self::Canceled,
            errno if errno > 0 => Self::Os(errno),
            _ => Self::Io,
        }
    }

    /// Read `errno` after a failed libc call.
    pub fn last_os_error() -> Self {
        Self::from_io(std::io::Error::last_os_error())
    }

    /// The errno this error was classified from, if any.
    pub fn errno(self) -> Option<i32> {
        match self {
            Self::NotFound => Some(libc::ENOENT),
            Self::AlreadyExists => Some(libc::EEXIST),
            Self::Canceled => Some(libc::ECANCELED),
            Self::Os(errno) => Some(errno),
            _ => None,
        }
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

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    /// Classify a standard I/O error.
    fn from(error: std::io::Error) -> Self {
        Self::from_io(error)
    }
}

/// A runtime result.
pub type Result<T, E = Error> = std::result::Result<T, E>;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn errno_classification_round_trips() {
        assert_eq!(Error::from_errno(libc::ENOENT), Error::NotFound);
        assert_eq!(Error::from_errno(libc::EEXIST), Error::AlreadyExists);
        assert_eq!(Error::from_errno(libc::ECANCELED), Error::Canceled);
        assert_eq!(Error::from_errno(libc::EIO), Error::Os(libc::EIO));
        assert_eq!(Error::from_errno(0), Error::Io);
        assert_eq!(Error::from_errno(-3), Error::Io);
        for errno in [libc::ENOENT, libc::EEXIST, libc::ECANCELED, libc::EPIPE] {
            assert_eq!(Error::from_errno(errno).errno(), Some(errno));
        }
        assert_eq!(Error::Io.errno(), None);
        assert_eq!(Error::Os(5).to_string(), "OS error 5");
    }
}

/// Errno classification and standard error contracts.
#[cfg(test)]
mod classification_tests {
    use super::*;

    /// Common errno values retain classifications while other positive diagnostics survive.
    #[test]
    fn os_errors_preserve_errno_and_existing_classifications() {
        for (errno, expected) in [
            (libc::ENOENT, Error::NotFound),
            (libc::EEXIST, Error::AlreadyExists),
            (libc::ECANCELED, Error::Canceled),
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
            Error::Canceled,
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
