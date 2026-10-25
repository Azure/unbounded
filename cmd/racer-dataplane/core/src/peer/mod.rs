//! Peer request decisions without transport, signatures, or clocks.
//!
//! - [`hop`]: what a node does with a verified peer request (serve, relay, or
//!   answer stale membership) and the error tables that turn failures into
//!   signed responses or link and admission evidence.
//! - [`validity`]: typed response-validity rules that the header-level checks
//!   in the dataplane (`src/peer/forwarding.rs`) call.
//! - [`subscriptions`]: the token-based subscription scheduler.
//! - [`silence`]: next hops that accept requests and never answer them.

pub mod hop;
pub mod silence;
pub mod subscriptions;
pub mod validity;

/// Failure classes the peer decisions distinguish.
///
/// The dataplane maps its `Error` onto this enum. The simulator uses it
/// directly. `Other` stands for every error that the tables propagate unchanged.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Fault {
    Overloaded,
    Unavailable,
    Io,
    Os,
    DeadlineExceeded,
    BadGateway,
    CorruptRecord,
    Cancelled,
    HopBudgetExhausted,
    IncompatibleMembership,
    NotFound,
    VersionUnavailable,
    Unauthorized,
    OriginForbidden,
    MissingKey,
    Other,
}
