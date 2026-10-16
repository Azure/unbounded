//! Error type shared by every runtime operation.
//!
//! [`Error`] carries a coarse [`ErrorKind`], the originating `errno` when the
//! failure came from the operating system, and an optional static context
//! string describing what was being attempted.
//!
//! Kernel `ECANCELED` is reported as [`ErrorKind::Os`] with the errno kept.
//! Only cancellation requested through a [`Scope`](crate::Scope) or the
//! reactor itself is [`ErrorKind::Canceled`].

use std::fmt;
use std::io;

/// A specialized [`Result`](std::result::Result) whose error defaults to [`Error`].
pub type Result<T, E = Error> = std::result::Result<T, E>;

/// Coarse classification of an [`Error`].
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum ErrorKind {
    /// The caller's scope or the runtime requested cancellation.
    Canceled,
    /// The caller's deadline passed before the operation completed.
    DeadlineExceeded,
    /// A bounded resource (queue, table, ring) is full.
    Overloaded,
    /// The runtime or a resource is shutting down or otherwise unusable.
    Unavailable,
    /// An argument was invalid.
    InvalidInput,
    /// A configuration value was invalid.
    InvalidConfiguration,
    /// A named entity does not exist.
    NotFound,
    /// A named entity already exists.
    AlreadyExists,
    /// The operation is not supported by the kernel or backend.
    Unsupported,
    /// Any other operating system error; see [`Error::raw_os_error`].
    Os,
}

impl ErrorKind {
    /// A short, lowercase description of the kind.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Canceled => "canceled",
            Self::DeadlineExceeded => "deadline exceeded",
            Self::Overloaded => "overloaded",
            Self::Unavailable => "unavailable",
            Self::InvalidInput => "invalid input",
            Self::InvalidConfiguration => "invalid configuration",
            Self::NotFound => "not found",
            Self::AlreadyExists => "already exists",
            Self::Unsupported => "unsupported",
            Self::Os => "os error",
        }
    }
}

impl fmt::Display for ErrorKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The runtime error type.
///
/// `Error` is `Clone` but deliberately not `Copy`, so it can grow new fields
/// without a breaking change.
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct Error {
    kind: ErrorKind,
    errno: Option<i32>,
    context: Option<&'static str>,
}

impl Error {
    /// Create an error of the given kind without an errno or context.
    pub const fn new(kind: ErrorKind) -> Self {
        Self {
            kind,
            errno: None,
            context: None,
        }
    }

    /// Attach a static description of what was being attempted, replacing any
    /// previous context.
    #[must_use]
    pub const fn with_context(mut self, context: &'static str) -> Self {
        self.context = Some(context);
        self
    }

    /// Create an error from a positive `errno` value.
    ///
    /// `ENOENT`, `EEXIST`, `EINVAL`, `ENOSYS`/`EOPNOTSUPP` and `ETIME`/`ETIMEDOUT`
    /// map to dedicated kinds; everything else, including `ECANCELED`, is
    /// [`ErrorKind::Os`]. The errno is always retained. A negative value (as
    /// found in io_uring completion results) is negated first.
    pub const fn from_errno(errno: i32) -> Self {
        let errno = errno.wrapping_abs();
        let kind = match errno {
            libc::ENOENT => ErrorKind::NotFound,
            libc::EEXIST => ErrorKind::AlreadyExists,
            libc::EINVAL => ErrorKind::InvalidInput,
            libc::ENOSYS | libc::EOPNOTSUPP => ErrorKind::Unsupported,
            libc::ETIMEDOUT => ErrorKind::DeadlineExceeded,
            _ => ErrorKind::Os,
        };
        Self {
            kind,
            errno: Some(errno),
            context: None,
        }
    }

    /// Capture the calling thread's current `errno`.
    pub fn last_os_error() -> Self {
        io::Error::last_os_error().into()
    }

    /// The error's kind.
    pub const fn kind(&self) -> ErrorKind {
        self.kind
    }

    /// The operating system error number, when the error came from a syscall.
    pub const fn raw_os_error(&self) -> Option<i32> {
        self.errno
    }

    /// The static context attached with [`Error::with_context`].
    pub const fn context(&self) -> Option<&'static str> {
        self.context
    }

    /// Whether this is [`ErrorKind::Canceled`].
    pub fn is_canceled(&self) -> bool {
        self.kind == ErrorKind::Canceled
    }

    /// Whether this is [`ErrorKind::DeadlineExceeded`].
    pub fn is_deadline_exceeded(&self) -> bool {
        self.kind == ErrorKind::DeadlineExceeded
    }

    /// Whether the errno is `EAGAIN`/`EWOULDBLOCK`.
    pub fn is_would_block(&self) -> bool {
        self.errno == Some(libc::EAGAIN)
    }

    /// Whether the errno is `EINTR`.
    pub fn is_interrupted(&self) -> bool {
        self.errno == Some(libc::EINTR)
    }
}

impl From<ErrorKind> for Error {
    fn from(kind: ErrorKind) -> Self {
        Self::new(kind)
    }
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if let Some(context) = self.context {
            write!(f, "{context}: ")?;
        }
        match self.errno {
            Some(errno) => write!(f, "{} ({})", self.kind, io::Error::from_raw_os_error(errno)),
            None => f.write_str(self.kind.as_str()),
        }
    }
}

impl std::error::Error for Error {}

impl From<io::Error> for Error {
    fn from(error: io::Error) -> Self {
        if let Some(errno) = error.raw_os_error() {
            return Self::from_errno(errno);
        }
        if let Some(inner) = error.get_ref().and_then(|e| e.downcast_ref::<Error>()) {
            return inner.clone();
        }
        let kind = match error.kind() {
            io::ErrorKind::NotFound => ErrorKind::NotFound,
            io::ErrorKind::AlreadyExists => ErrorKind::AlreadyExists,
            io::ErrorKind::InvalidInput | io::ErrorKind::InvalidData => ErrorKind::InvalidInput,
            io::ErrorKind::TimedOut => ErrorKind::DeadlineExceeded,
            io::ErrorKind::Unsupported => ErrorKind::Unsupported,
            io::ErrorKind::OutOfMemory => ErrorKind::Overloaded,
            _ => ErrorKind::Os,
        };
        Self::new(kind)
    }
}

impl From<Error> for io::Error {
    fn from(error: Error) -> Self {
        if let Some(errno) = error.errno
            && error.context.is_none()
        {
            return io::Error::from_raw_os_error(errno);
        }
        let kind = match error.kind {
            ErrorKind::Canceled => io::ErrorKind::Interrupted,
            ErrorKind::DeadlineExceeded => io::ErrorKind::TimedOut,
            ErrorKind::Overloaded => io::ErrorKind::WouldBlock,
            ErrorKind::Unavailable => io::ErrorKind::NotConnected,
            ErrorKind::InvalidInput | ErrorKind::InvalidConfiguration => {
                io::ErrorKind::InvalidInput
            }
            ErrorKind::NotFound => io::ErrorKind::NotFound,
            ErrorKind::AlreadyExists => io::ErrorKind::AlreadyExists,
            ErrorKind::Unsupported => io::ErrorKind::Unsupported,
            ErrorKind::Os => match error.errno {
                Some(errno) => io::Error::from_raw_os_error(errno).kind(),
                None => io::ErrorKind::Other,
            },
        };
        io::Error::new(kind, error)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn errno_mapping_keeps_errno() {
        for (errno, kind) in [
            (libc::ENOENT, ErrorKind::NotFound),
            (libc::EEXIST, ErrorKind::AlreadyExists),
            (libc::EINVAL, ErrorKind::InvalidInput),
            (libc::ENOSYS, ErrorKind::Unsupported),
            (libc::EOPNOTSUPP, ErrorKind::Unsupported),
            (libc::ETIMEDOUT, ErrorKind::DeadlineExceeded),
            (libc::EPIPE, ErrorKind::Os),
        ] {
            let error = Error::from_errno(errno);
            assert_eq!(error.kind(), kind);
            assert_eq!(error.raw_os_error(), Some(errno));
        }
    }

    #[test]
    fn kernel_ecanceled_is_not_canceled() {
        let error = Error::from_errno(-libc::ECANCELED);
        assert_eq!(error.kind(), ErrorKind::Os);
        assert!(!error.is_canceled());
        assert_eq!(error.raw_os_error(), Some(libc::ECANCELED));
    }

    #[test]
    fn negative_errno_is_normalized() {
        assert_eq!(
            Error::from_errno(-libc::EAGAIN).raw_os_error(),
            Some(libc::EAGAIN)
        );
        assert!(Error::from_errno(libc::EAGAIN).is_would_block());
        assert!(Error::from_errno(libc::EINTR).is_interrupted());
        // i32::MIN cannot be negated; it must not panic.
        let _ = Error::from_errno(i32::MIN);
    }

    #[test]
    fn display_includes_context_and_errno() {
        let error = Error::from_errno(libc::ENOENT).with_context("open config");
        let text = error.to_string();
        assert!(text.starts_with("open config: not found ("), "{text}");
        assert_eq!(Error::new(ErrorKind::Canceled).to_string(), "canceled");
        assert_eq!(error.context(), Some("open config"));
    }

    #[test]
    fn io_round_trip() {
        let io_error: io::Error = Error::from_errno(libc::ECONNRESET).into();
        assert_eq!(io_error.raw_os_error(), Some(libc::ECONNRESET));
        let back: Error = io_error.into();
        assert_eq!(back.raw_os_error(), Some(libc::ECONNRESET));

        let original = Error::new(ErrorKind::Overloaded).with_context("queue");
        let io_error: io::Error = original.clone().into();
        assert_eq!(io_error.kind(), io::ErrorKind::WouldBlock);
        assert_eq!(Error::from(io_error), original);

        let contextual: io::Error = Error::from_errno(libc::EPIPE).with_context("send").into();
        assert_eq!(contextual.kind(), io::ErrorKind::BrokenPipe);
        assert_eq!(Error::from(contextual).context(), Some("send"));
    }

    #[test]
    fn io_without_errno_maps_kind() {
        let error = Error::from(io::Error::new(io::ErrorKind::NotFound, "x"));
        assert_eq!(error.kind(), ErrorKind::NotFound);
        assert_eq!(error.raw_os_error(), None);
        let error = Error::from(io::Error::other("x"));
        assert_eq!(error.kind(), ErrorKind::Os);
        let io_error: io::Error = Error::new(ErrorKind::Canceled).into();
        assert_eq!(io_error.kind(), io::ErrorKind::Interrupted);
    }
}
