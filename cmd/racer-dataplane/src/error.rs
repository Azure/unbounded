//! Typed boundary failures. Error values contain no headers, tokens, or body bytes.

use std::{fmt, future::Future, pin::Pin};

pub type Result<T> = std::result::Result<T, Error>;

/// Worker-local future: deliberately not `Send`, and never drives a hidden executor.
pub type Operation<'a, T> = Pin<Box<dyn Future<Output = Result<T>> + 'a>>;

/// Yield one cooperative turn without retaining an executor or I/O owner.
pub(crate) async fn cooperative_turn() {
    let mut yielded = false;
    std::future::poll_fn(|cx| {
        if std::mem::replace(&mut yielded, true) {
            std::task::Poll::Ready(())
        } else {
            cx.waker().wake_by_ref();
            std::task::Poll::Pending
        }
    })
    .await
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidConfiguration,
    InvalidRequest,
    MethodNotAllowed,
    HeaderTooLarge,
    InvalidRange,
    UnsatisfiableRange,
    UnsatisfiableRangeWithLength(u64),
    NotFound,
    Forbidden,
    BadGateway,
    Internal,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    DeadlineExceeded,
    Cancelled,
    StaleFlight,
    Unauthorized,
    /// Authenticated Node binding changed; drain the entire graph and restart.
    NodeIdentityChanged,
    /// The origin rejected only the credentials of the supplying caller.
    OriginRejected,
    /// The origin forbids this caller; this is not evidence of page absence.
    OriginForbidden,
    Replay,
    IncompatibleMembership,
    HopBudgetExhausted,
    CorruptRecord,
    MissingKey,
    DirectIoUnsupported,
    Io,
}

impl fmt::Display for Error {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{self:?}")
    }
}

impl std::error::Error for Error {}

impl From<uring_runtime::Error> for Error {
    fn from(error: uring_runtime::Error) -> Self {
        match error {
            uring_runtime::Error::Cancelled => Self::Cancelled,
            uring_runtime::Error::DeadlineExceeded => Self::DeadlineExceeded,
            uring_runtime::Error::Overloaded => Self::Overloaded,
            uring_runtime::Error::Unavailable => Self::Unavailable,
            uring_runtime::Error::InvalidConfiguration => Self::InvalidConfiguration,
            uring_runtime::Error::InvalidInput => Self::InvalidRequest,
            uring_runtime::Error::NotFound => Self::MissingKey,
            uring_runtime::Error::AlreadyExists => Self::Replay,
            uring_runtime::Error::Io => Self::Io,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::Error;

    #[test]
    fn runtime_errors_keep_racer_boundary_meanings() {
        use uring_runtime::Error as RuntimeError;
        for (runtime, racer) in [
            (RuntimeError::Cancelled, Error::Cancelled),
            (RuntimeError::DeadlineExceeded, Error::DeadlineExceeded),
            (RuntimeError::Overloaded, Error::Overloaded),
            (RuntimeError::Unavailable, Error::Unavailable),
            (
                RuntimeError::InvalidConfiguration,
                Error::InvalidConfiguration,
            ),
            (RuntimeError::InvalidInput, Error::InvalidRequest),
            (RuntimeError::NotFound, Error::MissingKey),
            (RuntimeError::AlreadyExists, Error::Replay),
            (RuntimeError::Io, Error::Io),
        ] {
            assert_eq!(Error::from(runtime), racer);
        }
    }
}
