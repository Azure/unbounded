//! Typed boundary failures. Error values contain no headers, tokens, or body bytes.

use std::{fmt, future::Future, pin::Pin};

pub type Result<T> = std::result::Result<T, Error>;

/// Worker-local future: deliberately not `Send`, and never drives a hidden executor.
pub type Operation<'a, T> = Pin<Box<dyn Future<Output = Result<T>> + 'a>>;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Unimplemented(&'static str),
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
        match self {
            Self::Unimplemented(operation) => write!(formatter, "unimplemented: {operation}"),
            other => write!(formatter, "{other:?}"),
        }
    }
}

impl std::error::Error for Error {}

/// Explicit fail-closed placeholder for synchronous operations.
pub(crate) fn pending<T>(operation: &'static str) -> Result<T> {
    Err(Error::Unimplemented(operation))
}

/// Explicit placeholder for reactor-driven operations; never pretends work succeeded.
pub(crate) fn deferred<'a, T: 'a>(operation: &'static str) -> Operation<'a, T> {
    Box::pin(async move { pending(operation) })
}
