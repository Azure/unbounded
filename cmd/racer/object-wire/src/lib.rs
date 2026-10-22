//! Object identities, metadata, ranges, and client/origin HTTP protocol boundaries.
//! Application admission, credential ownership, I/O, and encryption stay with callers.
//!
//! `model` defines canonical keys, quoted version tags, exact millisecond expiry,
//! and bounded page ranges. `client` parses borrowed request context and encodes
//! response heads; `origin` validates framing before interpreting adapter status.
//! These modules do not authorize requests or authenticate encrypted pages.
//!
//! HTTP framing must be validated before semantic parsing. In particular, opaque
//! Authorization and Racer-Metadata values require one separator space and must
//! not be trimmed. Returned context borrows the head rather than creating another
//! credential owner. Applications map [`Error`] explicitly into their own taxonomy.
//!
//! The optional `test-util` feature provides strong-tag/time fixture constructors
//! and a controllable Unix-socket origin. It does not enable application policy.

pub mod client;
pub mod model;
pub mod origin;

#[cfg(feature = "test-util")]
pub mod test_util;

pub use model::*;

/// Protocol failures without application lifecycle or transport state.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    InvalidRequest,
    MethodNotAllowed,
    HeaderTooLarge,
    InvalidRange,
    UnsatisfiableRange,
    UnsatisfiableRangeWithLength(u64),
    NotFound,
    BadGateway,
    Internal,
    VersionUnavailable,
    Unavailable,
    OriginRejected,
    OriginForbidden,
    CorruptRecord,
}

/// Result of validating or encoding an object protocol value.
pub type Result<T> = std::result::Result<T, Error>;

impl From<http1::Error> for Error {
    fn from(error: http1::Error) -> Self {
        match error {
            http1::Error::Malformed => Self::InvalidRequest,
            http1::Error::HeadTooLarge => Self::HeaderTooLarge,
        }
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}
