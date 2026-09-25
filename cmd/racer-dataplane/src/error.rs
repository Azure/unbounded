//! Typed boundary failures. Error values contain no headers, tokens, or body bytes.

use std::{fmt, future::Future, pin::Pin};

pub type Result<T> = std::result::Result<T, Error>;

/// Worker-local future: deliberately not `Send`, and never drives a hidden executor.
pub type Operation<'a, T> = Pin<Box<dyn Future<Output = Result<T>> + 'a>>;

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
