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
//!
//! [`Membership::new_with_predecessor`] shares immutable graph storage when
//! frozen IDs match, including weight-only and metadata-only updates. It does
//! not retain the predecessor membership. [`Membership::with_predecessor`]
//! alone only adds placement hints after construction; it does not share graphs.
//! Membership can cross threads when its record type permits, but caches remain
//! worker-local. Construction is synchronous; applications decide where to run it.
//!
//! [`Membership::retained_bytes`] uses a cached immutable-storage estimate plus
//! the current bounded delta capacity, so reading it is O(1). Each estimate
//! includes the full graph even when multiple memberships share it; summing them
//! is conservative rather than unique-allocation accounting.
//!
//! # Example
//!
//! This crate serves the Racer dataplane without owning its wire records or
//! runtime policy. A neutral model is sufficient for the algorithm API:
//!
//! ```
//! use std::num::NonZeroU32;
//! use topology::{Error, Member, Membership, PathQuery, Paths, Placement};
//!
//! struct Node {
//!     id: &'static [u8],
//!     weight: NonZeroU32,
//! }
//!
//! impl Member for Node {
//!     const DOMAIN: &'static str = "example-store";
//!     fn id(&self) -> &[u8] { self.id }
//!     fn weight(&self) -> NonZeroU32 { self.weight }
//! }
//!
//! # fn main() -> Result<(), Error> {
//! let members = Membership::new(vec![
//!     Node { id: b"b", weight: NonZeroU32::new(2).unwrap() },
//!     Node { id: b"a", weight: NonZeroU32::new(1).unwrap() },
//! ])?;
//! let a = members.position(b"a").unwrap();
//! let b = members.position(b"b").unwrap();
//! assert_eq!(a, 0); // Positions follow frozen ID order, not input order.
//!
//! let placement = Placement::new(16);
//! let owners = placement.rank(&members, b"opaque application key")?;
//! assert_eq!(owners.len(), 2); // At most three, with no duplicate owners.
//! assert!(owners.contains(&a) && owners.contains(&b));
//!
//! // Independent entry, accounted-byte, and distinct active-search limits.
//! let paths = Paths::with_limits(16, 1024 * 1024, 2);
//! futures::executor::block_on(async {
//!     let query = PathQuery {
//!         from: a,
//!         to: b,
//!         links: 1,
//!         visited: &[],
//!         blocked: &[],
//!         seed: b"request seed",
//!     };
//!     assert_eq!(paths.route(&members, query).await?, vec![a, b]);
//!     assert_eq!(
//!         paths.route(&members, PathQuery { links: 0, ..query }).await,
//!         Err(Error::Unreachable),
//!     );
//!
//!     // A validated self route needs neither links nor search admission.
//!     let no_searches = Paths::with_limits(0, 0, 0);
//!     let self_query = PathQuery { to: a, links: 0, ..query };
//!     assert_eq!(no_searches.route(&members, self_query).await?, vec![a]);
//!     assert_eq!(
//!         no_searches.route(&members, PathQuery { visited: &[a], ..self_query }).await,
//!         Err(Error::InvalidQuery),
//!     );
//!     Ok::<(), Error>(())
//! })?;
//! # Ok(())
//! # }
//! ```
//!
//! The example executor is only a test/dev dependency. Production callers poll
//! these worker-local futures on their own executor and impose deadlines and
//! total request limits. Byte accounting is not an allocator or RSS hard bound.
#![forbid(unsafe_code)]
#![deny(missing_docs)]

mod hash;
mod membership;
mod overlay;
mod paths;
mod placement;

pub use membership::{MAX_INCREMENTAL_CHANGES, Membership};
pub use paths::{PathQuery, Paths};
pub use placement::{Maintenance, Placement, REPLICAS, SLOT_BITS, SLOT_COUNT, slot};
use std::num::NonZeroU32;

/// Maximum neighbors in the symmetric union of independent hash rings.
pub const MAX_DEGREE: usize = 2 * overlay::RINGS as usize;

/// Algorithm inputs snapshotted when constructing a membership.
/// Domains separate unrelated applications while preserving their hash contracts.
pub trait Member {
    /// Application hash prefix, which must not contain NUL (`\0`). Empty, ASCII,
    /// and UTF-8 prefixes are otherwise accepted by [`Membership::new`].
    /// Hashes concatenate this prefix with an algorithm suffix ending in NUL.
    /// Excluding NUL here keeps that terminator unambiguous, preventing domain
    /// bytes from absorbing member data without changing existing hash bytes.
    const DOMAIN: &'static str;
    /// Stable identity, sorted lexicographically within the membership.
    fn id(&self) -> &[u8];
    /// Positive relative placement and next-hop selection weight.
    fn weight(&self) -> NonZeroU32;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
/// Membership validation, routing, and admission failures.
pub enum Error {
    /// Two records have the same frozen identity.
    DuplicateMember,
    /// The member domain contains a NUL byte.
    InvalidDomain,
    /// An identity exceeds the placement hash schema's u32 length limit.
    InvalidMember,
    /// Positions or hop constraints are invalid.
    InvalidQuery,
    /// The configured concurrent work limit is reached.
    Overloaded,
    /// No eligible route exists within the hop budget.
    Unreachable,
    /// Bounded unbiased random selection exhausted its retry budget.
    SamplingExhausted,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::DuplicateMember => "duplicate member identity",
            Self::InvalidDomain => "member domain must not contain NUL",
            Self::InvalidMember => "member identity exceeds the hash length limit",
            Self::InvalidQuery => "invalid topology query",
            Self::Overloaded => "topology cache overloaded",
            Self::Unreachable => "destination unreachable",
            Self::SamplingExhausted => "weighted selection retry budget exhausted",
        })
    }
}

impl std::error::Error for Error {}
