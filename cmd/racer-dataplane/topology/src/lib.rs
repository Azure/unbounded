//! Runtime-independent algorithms over immutable, domain-separated membership.
//!
//! Implement [`Member`] for application-owned records, then freeze them in a
//! [`Membership`]. [`Placement`] ranks up to three members for an opaque key;
//! [`Paths`] selects a weighted next hop among equal-cost routes on the overlay.
//! Results are positions in the supplied membership's ID-sorted member slice.
//!
//! Caches are worker-local. Cooperative futures bound work per poll but leave
//! deadlines, cancellation, health, transport, and membership versions to callers.
//! Changing a member's domain changes placement and routing across the cluster.
pub mod hash;
mod membership;
mod paths;
mod placement;
mod workers;

pub use membership::Membership;
pub use paths::{PathQuery, Paths};
pub use placement::Placement;
use std::num::NonZeroU32;
pub use workers::StaticWorkerMap;

pub const MAX_DEGREE: usize = 64;
pub(crate) const RADIX: usize = 32;

/// Stable algorithm inputs. IDs and weights must not change within a membership.
/// Domains separate unrelated applications while preserving their hash contracts.
pub trait Member {
    /// Application hash prefix, which must not contain NUL (`\0`). Empty, ASCII,
    /// and UTF-8 prefixes are otherwise accepted by [`Membership::new`].
    /// Hashes concatenate this prefix with an algorithm suffix ending in NUL.
    /// Excluding NUL here keeps that terminator unambiguous, preventing domain
    /// bytes from absorbing member data without changing existing hash bytes.
    const DOMAIN: &'static str;
    fn id(&self) -> &[u8];
    fn weight(&self) -> NonZeroU32;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
pub enum Error {
    DuplicateMember,
    /// The member domain contains a NUL byte.
    InvalidDomain,
    InvalidQuery,
    Overloaded,
    Unreachable,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::DuplicateMember => "duplicate member identity",
            Self::InvalidDomain => "member domain must not contain NUL",
            Self::InvalidQuery => "invalid topology query",
            Self::Overloaded => "topology cache overloaded",
            Self::Unreachable => "destination unreachable",
        })
    }
}

impl std::error::Error for Error {}
