//! Read-path decision errors.
//!
//! A subset of the dataplane's boundary errors. The dataplane converts in both
//! directions (`From<racer_core::read::Error>` and a fallible reverse), so core
//! decisions never widen what the dataplane can observe.

/// Errors produced or interpreted by sans-IO read decisions.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidConfiguration,
    InvalidRequest,
    HeaderTooLarge,
    NotFound,
    Internal,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    DeadlineExceeded,
    Cancelled,
    Unauthorized,
    OriginRejected,
    OriginForbidden,
    IncompatibleMembership,
    HopBudgetExhausted,
    CorruptRecord,
    MissingKey,
    Io,
    Os(i32),
}

impl std::fmt::Display for Error {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "{self:?}")
    }
}

impl std::error::Error for Error {}

pub type Result<T, E = Error> = std::result::Result<T, E>;
