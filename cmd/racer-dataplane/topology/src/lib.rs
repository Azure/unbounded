//! Placement and routing for a set of cluster members.
//!
//! This crate answers two questions for the Racer dataplane:
//!
//! - **Which members store this key?** [`Placement`] picks up to three.
//! - **How do I get from one member to another?** [`Paths`] picks a short route.
//!
//! It does no I/O and has no timers. The caller owns networking, timeouts,
//! cancellation, health checks, and when to publish a new membership.
//!
//! # Basics
//!
//! 1. Implement [`Member`] for your member type.
//! 2. Build a [`Membership`] from a list of members. It sorts them by ID.
//! 3. Ask [`Placement`] or [`Paths`]. Answers are positions in that sorted list.
//!
//! A position only has meaning for the membership that produced it.
//!
//! # Example
//!
//! ```
//! use std::num::NonZeroU32;
//! use topology::{Error, Member, Membership, PathQuery, Paths, Placement};
//!
//! struct Node {
//!     id: &'static [u8],
//!
//!     weight: NonZeroU32,
//! }
//!
//! impl Member for Node {
//!     const DOMAIN: &'static str = "example-store";
//!
//!     fn id(&self) -> &[u8] { self.id }
//!
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
//! assert_eq!(a, 0); // Sorted by ID, not by input order.
//!
//! let placement = Placement::new(16);
//! let owners = placement.rank(&members, b"opaque application key")?;
//! assert_eq!(owners.len(), 2); // Up to three owners, never repeated.
//! assert!(owners.contains(&a) && owners.contains(&b));
//!
//! // Cache up to 16 routes or 1 MiB, and run up to 2 searches at once.
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
//!     // A route to yourself always works, even with no search slots.
//!     let no_searches = Paths::with_limits(0, 0, 0);
//!     let self_query = PathQuery { to: a, links: 0, ..query };
//!     assert_eq!(no_searches.route(&members, self_query).await?, vec![a]);
//!     // But the query is still checked.
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
//! The example uses a simple test executor. Real callers use their own.
//!
//! # Membership
//!
//! Each member has a binary ID and a positive weight. IDs must be unique.
//! An empty membership is allowed, but has nothing to route between.
//!
//! Members are linked into a graph built from 32 hash rings. Each member has
//! at most [`MAX_DEGREE`] neighbors and the graph is always connected. Adding
//! or removing one member only changes that member's ring neighbors. Weights
//! do not affect the graph.
//!
//! Building a membership takes O(N log N) time. At 100,000 members the graph
//! uses about 49 MiB.
//!
//! When membership changes, build the new one with
//! [`Membership::new_with_predecessor`]. It reuses the old graph when the IDs
//! are the same, and helps [`Placement`] update its cache cheaply.
//!
//! # Threads and caches
//!
//! A [`Membership`] can be shared across threads. [`Placement`] and [`Paths`]
//! cannot: make one per worker thread. Async placement scoring and route searches
//! advance incrementally across polls. Cache admission and other synchronous work
//! do not have a fixed per-poll latency bound: placement admission may scan the
//! whole cache before scoring starts.
//!
//! Every cache has a size limit. Byte counts are estimates, not hard memory
//! limits. [`Membership::retained_bytes`] counts a shared graph in full for
//! every membership that uses it, so adding them up overcounts.
//!
//! # Compatibility
//!
//! Every node in a cluster must agree on placement and routing. Both depend
//! on [`Member::DOMAIN`], member IDs, weights, and this crate's hash formats.
//!
//! - Changing a domain changes the hashes; placement and routes may move.
//! - Hash formats are versioned (`/slot/v1`, `/hrw/v1`,
//!   `/placement-identity/v1`, `/next-hop/v5`, and ring `v1`). Changing one
//!   needs a planned cluster-wide rollout. Nothing here detects a mismatch.
#![forbid(unsafe_code)]
#![deny(missing_docs)]

/// Routing between members.
mod paths;

/// Choosing which members store a key.
mod placement;

pub use paths::{PathQuery, Paths};

pub use placement::{Maintenance, Placement, REPLICAS, SLOT_BITS, SLOT_COUNT, slot};

use sha2::Digest;

use std::{mem::size_of, num::NonZeroU32, sync::Arc};

/// Most neighbors any member can have in the graph.
pub const MAX_DEGREE: usize = 2 * overlay::RINGS as usize;

/// A cluster member, as seen by this crate.
///
/// [`Membership`] reads the ID and weight once, when it is built.
pub trait Member {
    /// A name that keeps this application's hashes apart from others.
    ///
    /// Must not contain a NUL byte (`\0`). Any other string, even an empty one,
    /// is fine. Changing it changes the hashes; placement and routes may move.
    const DOMAIN: &'static str;

    /// Unique ID for this member. Should not change over the member's life.
    fn id(&self) -> &[u8];

    /// How much this member is favored, compared to others. Higher is more.
    /// Used for both placement and routing.
    fn weight(&self) -> NonZeroU32;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
/// Errors from this crate.
pub enum Error {
    /// Two members have the same ID.
    DuplicateMember,

    /// [`Member::DOMAIN`] contains a NUL byte.
    InvalidDomain,

    /// A member ID is longer than `u32::MAX` bytes.
    InvalidMember,

    /// A query has a bad position or breaks a limit.
    InvalidQuery,

    /// Too much work is already running.
    Overloaded,

    /// No route fits within the allowed hops.
    Unreachable,

    /// Weighted random picking ran out of tries. Very rare.
    SamplingExhausted,
}

impl std::fmt::Display for Error {
    /// Short description of the error.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::DuplicateMember => "duplicate member identity",
            Self::InvalidDomain => "member domain must not contain NUL",
            Self::InvalidMember => "member identity exceeds the hash length limit",
            Self::InvalidQuery => "invalid topology query",
            Self::Overloaded => "topology placement or route search overloaded",
            Self::Unreachable => "destination unreachable",
            Self::SamplingExhausted => "weighted selection retry budget exhausted",
        })
    }
}

impl std::error::Error for Error {}

/// Most member changes [`Membership::with_predecessor`] will track. Bigger
/// changes are handled by recomputing from scratch.
pub const MAX_INCREMENTAL_CHANGES: usize = 64;

/// A fixed set of members, sorted by ID.
///
/// IDs and weights are copied when it is built, so later changes to the
/// records do not affect it. It can be shared across threads if `M` can.
#[derive(Debug)]
pub struct Membership<M: Member> {
    /// The original records, in ID order.
    members: Vec<M>,

    /// Copied IDs, in order.
    ids: Vec<Box<[u8]>>,

    /// Copied weights, matching `ids`.
    weights: Vec<NonZeroU32>,

    /// Hash of the domain, IDs, and weights.
    identity: [u8; 32],

    /// Hash of the domain and IDs only. Used as the route cache key.
    topology_identity: [u8; 32],

    /// Neighbor lists. Shared with later memberships that have the same IDs.
    graph: Arc<Vec<Vec<usize>>>,

    /// Estimated bytes, measured once at build time. Excludes `delta`.
    owned_bytes: usize,

    /// What changed since the previous membership, if known.
    delta: Option<MembershipDelta>,
}

/// One member that changed, with its position in the old and new lists.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum MemberChange {
    /// The member left.
    Removed { old: usize },

    /// The member joined.
    Added { new: usize },

    /// The member stayed but its weight changed.
    Reweighted {
        /// Position in the old list.
        old: usize,

        /// Position in the new list.
        new: usize,
    },
}

impl MemberChange {
    /// Position in the old list, if the member was there.
    fn old_position(self) -> Option<usize> {
        match self {
            Self::Removed { old } | Self::Reweighted { old, .. } => Some(old),
            Self::Added { .. } => None,
        }
    }

    /// Position in the new list, if the member is there.
    fn new_position(self) -> Option<usize> {
        match self {
            Self::Added { new } | Self::Reweighted { new, .. } => Some(new),
            Self::Removed { .. } => None,
        }
    }
}

/// Changes from one previous membership, in ID order.
#[derive(Debug)]
struct MembershipDelta {
    /// [`Membership::identity`] of the previous membership.
    base: [u8; 32],

    /// Member count of the previous membership.
    old_count: usize,

    /// The changes, in ID order.
    changes: Vec<MemberChange>,
}

impl<M: Member> Membership<M> {
    /// Build a membership: copy IDs and weights, sort by ID, and build the graph.
    ///
    /// This runs synchronously and takes O(N log N) time, so choose where to
    /// run it with care for large memberships.
    ///
    /// # Errors
    /// - [`Error::InvalidDomain`] if [`Member::DOMAIN`] contains NUL.
    /// - [`Error::DuplicateMember`] if two members share an ID.
    /// - [`Error::InvalidMember`] if an ID is longer than `u32::MAX` bytes.
    ///
    /// # Panics
    /// Panics if a [`Member`] method panics.
    pub fn new(members: Vec<M>) -> Result<Self, Error> {
        Self::build(members, None)
    }

    /// Build a membership that replaces `old`.
    ///
    /// Same as [`Self::new`] followed by [`Self::with_predecessor`], but also
    /// reuses `old`'s graph when the IDs are the same. Does not keep `old` alive.
    ///
    /// # Errors
    /// Same as [`Self::new`].
    ///
    /// # Panics
    /// Panics if a [`Member`] method panics.
    pub fn new_with_predecessor(members: Vec<M>, old: &Self) -> Result<Self, Error> {
        Ok(Self::build(members, Some(old))?.with_predecessor(old))
    }

    /// Shared code for the constructors. Reuses `old`'s graph if IDs match.
    fn build(members: Vec<M>, old: Option<&Self>) -> Result<Self, Error> {
        if M::DOMAIN.as_bytes().contains(&0) {
            return Err(Error::InvalidDomain);
        }
        // Validate before allocating a snapshot or hashing a truncated length.
        let mut frozen: Vec<_> = members
            .into_iter()
            .map(|member| {
                let id = member.id();
                validate_id_length(id.len())?;
                let id: Box<[u8]> = id.into();
                let weight = member.weight();
                Ok((member, id, weight))
            })
            .collect::<Result<_, Error>>()?;
        frozen.sort_unstable_by(|a, b| a.1.cmp(&b.1));
        if frozen.windows(2).any(|pair| pair[0].1 == pair[1].1) {
            return Err(Error::DuplicateMember);
        }
        let mut members = Vec::with_capacity(frozen.len());
        let mut ids = Vec::with_capacity(frozen.len());
        let mut weights = Vec::with_capacity(frozen.len());
        let mut digest = hash::domain::<M>(b"/placement-identity/v1\0");
        for (member, id, weight) in frozen {
            hash::bytes(&mut digest, &id);
            digest.update(weight.get().to_be_bytes());
            members.push(member);
            ids.push(id);
            weights.push(weight);
        }
        let topology_identity = overlay::identity::<M>(&ids);
        let graph = match old.filter(|old| old.ids == ids) {
            Some(old) => Arc::clone(&old.graph),
            None => Arc::new(overlay::build::<M>(&ids)),
        };
        let mut membership = Self {
            members,
            ids,
            weights,
            identity: hash::finish(digest),
            topology_identity,
            graph,
            owned_bytes: 0,
            delta: None,
        };
        membership.owned_bytes = membership.measure_owned_bytes();
        Ok(membership)
    }

    /// Record what changed since `old`, so [`Placement`] can update cheaply.
    ///
    /// Only up to [`MAX_INCREMENTAL_CHANGES`] changes are recorded. With more,
    /// nothing is recorded and placement recomputes from scratch. Replaces any
    /// earlier record. Does not keep `old` alive or change the graph.
    #[must_use]
    pub fn with_predecessor(mut self, old: &Self) -> Self {
        // Identical or over-budget replacements must not retain a stale hint.
        self.delta = None;
        if self.identity == old.identity {
            return self;
        }
        let mut changes = Vec::new();
        let (mut a, mut b) = (0, 0);
        while a < old.members.len() || b < self.members.len() {
            let order = match (old.ids.get(a), self.ids.get(b)) {
                (Some(a), Some(b)) => a.cmp(b),
                (Some(_), None) => std::cmp::Ordering::Less,
                _ => std::cmp::Ordering::Greater,
            };
            match order {
                std::cmp::Ordering::Less => {
                    changes.push(MemberChange::Removed { old: a });
                    a += 1;
                }
                std::cmp::Ordering::Greater => {
                    changes.push(MemberChange::Added { new: b });
                    b += 1;
                }
                std::cmp::Ordering::Equal => {
                    if old.weight(a) != self.weight(b) {
                        changes.push(MemberChange::Reweighted { old: a, new: b });
                    }
                    a += 1;
                    b += 1;
                }
            }
            if changes.len() > MAX_INCREMENTAL_CHANGES {
                return self;
            }
        }
        self.delta = Some(MembershipDelta {
            base: old.identity,
            old_count: old.members.len(),
            changes,
        });
        self
    }

    /// The original records, sorted by ID.
    #[must_use]
    pub fn members(&self) -> &[M] {
        &self.members
    }

    /// Position of the member with this ID, or `None` if there is none.
    #[must_use]
    pub fn position(&self, id: &[u8]) -> Option<usize> {
        self.ids.binary_search_by(|m| m.as_ref().cmp(id)).ok()
    }

    /// Hash of the domain, IDs, and weights. Equal hashes mean equal placement.
    #[must_use]
    pub fn identity(&self) -> [u8; 32] {
        self.identity
    }

    /// ID at a position.
    ///
    /// # Panics
    /// Panics if the position is out of range.
    fn id(&self, position: usize) -> &[u8] {
        &self.ids[position]
    }

    /// Weight at a position.
    ///
    /// # Panics
    /// Panics if the position is out of range.
    fn weight(&self, position: usize) -> NonZeroU32 {
        self.weights[position]
    }

    /// Hash of the domain and IDs. Weights are left out.
    fn topology_identity(&self) -> [u8; 32] {
        self.topology_identity
    }

    /// Neighbors of a member, sorted. Empty if the position is out of range.
    #[must_use]
    pub fn neighbors(&self, position: usize) -> Vec<usize> {
        self.neighbor_slice(position).to_vec()
    }

    /// Like [`Self::neighbors`], but borrows instead of copying.
    fn neighbor_slice(&self, position: usize) -> &[usize] {
        self.graph.get(position).map_or(&[], Vec::as_slice)
    }

    /// A shared handle to the graph.
    fn graph(&self) -> Arc<Vec<Vec<usize>>> {
        Arc::clone(&self.graph)
    }

    /// Estimated heap bytes used by this membership. Fast to call.
    ///
    /// Counts the full graph even when it is shared with another membership.
    /// Does not count heap memory owned by your records.
    #[must_use]
    pub fn retained_bytes(&self) -> usize {
        self.owned_bytes
            .saturating_add(self.delta.as_ref().map_or(0, |delta| {
                delta
                    .changes
                    .capacity()
                    .saturating_mul(size_of::<MemberChange>())
            }))
    }

    /// Measure the fixed part of [`Self::retained_bytes`].
    fn measure_owned_bytes(&self) -> usize {
        let mut bytes = self.members.capacity().saturating_mul(size_of::<M>());
        bytes = bytes.saturating_add(self.ids.capacity().saturating_mul(size_of::<Box<[u8]>>()));
        bytes = bytes.saturating_add(
            self.weights
                .capacity()
                .saturating_mul(size_of::<NonZeroU32>()),
        );
        for id in &self.ids {
            bytes = bytes.saturating_add(id.len());
        }
        // Arc contains the Vec header and two atomic reference counters.
        bytes = bytes.saturating_add(size_of::<Vec<Vec<usize>>>() + 2 * size_of::<usize>());
        bytes = bytes.saturating_add(
            self.graph
                .capacity()
                .saturating_mul(size_of::<Vec<usize>>()),
        );
        for neighbors in self.graph.iter() {
            bytes = bytes.saturating_add(neighbors.capacity().saturating_mul(size_of::<usize>()));
        }
        bytes
    }
}

/// Reject IDs longer than `u32::MAX` bytes.
fn validate_id_length(length: usize) -> Result<(), Error> {
    u32::try_from(length)
        .map(|_| ())
        .map_err(|_| Error::InvalidMember)
}

/// SHA-256 helpers. Hash output must never change, so edit with care.
pub mod hash {
    use crate::Member;

    use sha2::{Digest, Sha256};

    /// Start a hash with the member domain and then `suffix`.
    pub(super) fn domain<M: Member>(suffix: &[u8]) -> Sha256 {
        let mut hash = named_domain(M::DOMAIN.as_bytes());
        hash.update(suffix);
        hash
    }

    /// Start a hash with exactly these bytes.
    pub fn named_domain(name: &[u8]) -> Sha256 {
        let mut hash = Sha256::new();
        hash.update(name);
        hash
    }

    /// Add a value, preceded by its length as a big-endian u32.
    pub(super) fn bytes(hash: &mut Sha256, value: &[u8]) {
        try_bytes(hash, value).expect("topology hash fields must fit the u32 length schema");
    }

    /// Append a u32-length-prefixed field, rejecting oversized input before mutation.
    pub fn try_bytes(hash: &mut Sha256, value: &[u8]) -> Result<(), crate::Error> {
        crate::validate_id_length(value.len())?;
        hash.update(field_length(value.len()).to_be_bytes());
        hash.update(value);
        Ok(())
    }

    /// Convert a length to u32. Panics if it does not fit.
    fn field_length(length: usize) -> u32 {
        u32::try_from(length).expect("topology hash fields must fit the u32 length schema")
    }

    /// Finish the hash.
    pub fn finish(hash: Sha256) -> [u8; 32] {
        hash.finalize().into()
    }

    /// Hash helper tests.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Lengths up to `u32::MAX` work; longer ones panic.
        #[test]
        fn length_prefix_checks_without_allocating_gigabytes() {
            assert_eq!(field_length(0), 0);
            assert_eq!(crate::validate_id_length(0), Ok(()));
            assert_eq!(field_length(u32::MAX as usize), u32::MAX);
            assert_eq!(crate::validate_id_length(u32::MAX as usize), Ok(()));
            if let Some(overflow) = (u32::MAX as usize).checked_add(1) {
                assert!(std::panic::catch_unwind(|| field_length(overflow)).is_err());
                assert_eq!(
                    crate::validate_id_length(overflow),
                    Err(crate::Error::InvalidMember)
                );
            }
        }

        /// Public checked fields preserve the internal schema, including empty values.
        #[test]
        fn checked_fields_preserve_exact_schema() {
            let mut checked = named_domain(b"racer/link-backoff/v1\0");
            try_bytes(&mut checked, b"node").unwrap();
            try_bytes(&mut checked, b"").unwrap();
            checked.update(3u32.to_be_bytes());
            let expected: [u8; 32] =
                Sha256::digest(b"racer/link-backoff/v1\0\0\0\0\x04node\0\0\0\0\0\0\0\x03").into();
            assert_eq!(finish(checked), expected);
        }

        /// The helpers produce exactly the expected bytes.
        #[test]
        fn exact_domain_and_length_prefix_bytes() {
            let mut hash = named_domain(b"app/key/v1\0");
            bytes(&mut hash, b"abc");
            bytes(&mut hash, b"");
            let expected: [u8; 32] = Sha256::digest(b"app/key/v1\0\0\0\0\x03abc\0\0\0\0").into();
            assert_eq!(finish(hash), expected);
            assert_eq!(
                finish(named_domain(b"")),
                <[u8; 32]>::from(Sha256::digest(b""))
            );
        }

        /// Different splits of the same bytes, or different domains, hash differently.
        #[test]
        fn field_boundaries_and_domains_are_distinct() {
            let key = |domain: &[u8], a: &[u8], b: &[u8]| {
                let mut hash = named_domain(domain);
                bytes(&mut hash, a);
                bytes(&mut hash, b);
                finish(hash)
            };
            assert_ne!(key(b"a\0", b"ab", b"c"), key(b"a\0", b"a", b"bc"));
            assert_ne!(key(b"a\0", b"ab", b"c"), key(b"b\0", b"ab", b"c"));
        }
    }
}

/// The member graph.
///
/// Each of 32 rings orders members by a different hash of their ID. Each
/// member links to its two neighbors in every ring. Weights play no part.
mod overlay {
    use crate::{Member, hash};

    use sha2::Digest;

    /// Number of rings.
    pub(super) const RINGS: u32 = 32;

    /// Hash prefix for ring order. Changing it changes every graph.
    const RING_DOMAIN: &[u8] = b"/overlay/sha256-rings-32/v1\0";

    /// Hash prefix for `identity`.
    const IDENTITY_DOMAIN: &[u8] = b"/topology-identity/sha256-rings-32/v1\0";

    /// Hash the domain and sorted IDs. Weights are left out.
    pub(super) fn identity<M: Member>(ids: &[Box<[u8]>]) -> [u8; 32] {
        let mut digest = hash::domain::<M>(IDENTITY_DOMAIN);
        for id in ids {
            digest.update((id.len() as u64).to_be_bytes());
            digest.update(id);
        }
        hash::finish(digest)
    }

    /// Build sorted, duplicate-free neighbor lists from all rings.
    pub(super) fn build<M: Member>(ids: &[Box<[u8]>]) -> Vec<Vec<usize>> {
        let mut graph: Vec<_> = (0..ids.len())
            .map(|_| Vec::with_capacity(if ids.len() > 1 { 2 * RINGS as usize } else { 0 }))
            .collect();
        if ids.len() < 2 {
            return graph;
        }
        let mut order = Vec::with_capacity(ids.len());
        for ring in 0..RINGS {
            order.clear();
            let mut prefix = hash::domain::<M>(RING_DOMAIN);
            prefix.update(ring.to_be_bytes());
            for (position, id) in ids.iter().enumerate() {
                let mut digest = prefix.clone();
                digest.update((id.len() as u64).to_be_bytes());
                digest.update(id);
                order.push((hash::finish(digest), position));
            }
            // Frozen-ID positions break digest ties without size-dependent edges.
            order.sort_unstable();
            for index in 0..order.len() {
                let a = order[index].1;
                let b = order[(index + 1) % order.len()].1;
                graph[a].push(b);
                graph[b].push(a);
            }
        }
        for neighbors in &mut graph {
            neighbors.sort_unstable();
            neighbors.dedup();
        }
        graph
    }

    /// Graph tests.
    #[cfg(test)]
    mod tests {
        use super::*;

        use crate::Membership;

        use std::{collections::BTreeSet, num::NonZeroU32};

        /// Test member with a 4-byte ID.
        #[derive(Clone, Debug)]
        struct Node([u8; 4], NonZeroU32);

        impl Member for Node {
            const DOMAIN: &'static str = "overlay-tests";

            /// Return the ID.
            fn id(&self) -> &[u8] {
                &self.0
            }

            /// Return the weight.
            fn weight(&self) -> NonZeroU32 {
                self.1
            }
        }

        /// Test member with weight 1.
        fn node(id: u32) -> Node {
            Node(id.to_be_bytes(), NonZeroU32::new(1).unwrap())
        }

        /// Members with even IDs, leaving gaps to insert odd ones.
        fn membership(count: u32) -> Membership<Node> {
            Membership::new((0..count).map(|id| node(2 * id)).collect()).unwrap()
        }

        /// All edges, as ID pairs, so they can be compared across memberships.
        fn edges(members: &Membership<Node>) -> BTreeSet<(Vec<u8>, Vec<u8>)> {
            let mut edges = BTreeSet::new();
            for position in 0..members.members().len() {
                for &other in members.neighbor_slice(position) {
                    if position < other {
                        edges.insert((members.id(position).to_vec(), members.id(other).to_vec()));
                    }
                }
            }
            edges
        }

        /// Graphs are symmetric, connected, size-limited, and order-independent.
        #[test]
        fn symmetric_connected_bounded_and_deterministic() {
            for count in [0, 1, 2, 3, 32, 65, 257, 1024] {
                let members = membership(count);
                let mut permuted = members.members().to_vec();
                permuted.reverse();
                for member in &mut permuted {
                    member.1 = NonZeroU32::new(u32::MAX).unwrap();
                }
                let reordered = Membership::new(permuted).unwrap();
                assert_eq!(members.graph(), reordered.graph());
                assert_eq!(members.topology_identity(), reordered.topology_identity());
                for position in 0..count as usize {
                    let neighbors = members.neighbor_slice(position);
                    assert_eq!(members.neighbors(position), neighbors);
                    assert!(neighbors.len() <= crate::MAX_DEGREE);
                    assert!(neighbors.windows(2).all(|pair| pair[0] < pair[1]));
                    assert!(!neighbors.contains(&position));
                    for &other in neighbors {
                        assert!(other < count as usize);
                        assert!(
                            members
                                .neighbor_slice(other)
                                .binary_search(&position)
                                .is_ok()
                        );
                    }
                }
                let mut seen = BTreeSet::new();
                let mut pending = if count == 0 { vec![] } else { vec![0] };
                while let Some(position) = pending.pop() {
                    if seen.insert(position) {
                        pending.extend(members.neighbor_slice(position));
                    }
                }
                assert_eq!(seen.len(), count as usize);
                assert!(members.neighbors(count as usize).is_empty());
                assert!(members.neighbor_slice(usize::MAX).is_empty());
                assert!(members.neighbors(usize::MAX).is_empty());
            }
        }

        /// A join or leave only changes edges next to that member.
        #[test]
        fn joins_and_leaves_change_only_local_id_edges() {
            let old = membership(512);
            let before = edges(&old);
            for id in [1, 511, 1023] {
                let mut values = old.members().to_vec();
                values.push(node(id));
                let joined = Membership::new(values).unwrap();
                let after = edges(&joined);
                assert!(before.symmetric_difference(&after).count() <= 3 * RINGS as usize);
                let removed: Vec<_> = before.difference(&after).collect();
                assert!(removed.len() <= RINGS as usize);
                let added: Vec<_> = after.difference(&before).collect();
                assert!(added.len() <= 2 * RINGS as usize);
                assert!(
                    added.iter().all(|(a, b)| a.as_slice() == id.to_be_bytes()
                        || b.as_slice() == id.to_be_bytes())
                );
                let remaining = Membership::new(
                    joined
                        .members()
                        .iter()
                        .filter(|member| member.id() != id.to_be_bytes())
                        .cloned()
                        .collect(),
                )
                .unwrap();
                assert_eq!(edges(&remaining), before);
            }
            for id in [0u32, 512, 1022] {
                let remaining = Membership::new(
                    old.members()
                        .iter()
                        .filter(|member| member.id() != id.to_be_bytes())
                        .cloned()
                        .collect(),
                )
                .unwrap();
                assert!(
                    before.symmetric_difference(&edges(&remaining)).count() <= 3 * RINGS as usize
                );
            }
        }
    }
}

/// Membership tests.
#[cfg(test)]
mod tests {
    use super::*;

    use std::{num::NonZeroU32, rc::Rc};

    /// Test member with a binary ID and a weight.
    #[derive(Clone, Debug)]
    struct BinaryMember(Vec<u8>, NonZeroU32);

    impl Member for BinaryMember {
        const DOMAIN: &'static str = "binary-store";

        /// Return the ID.
        fn id(&self) -> &[u8] {
            &self.0
        }

        /// Return the weight.
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }

    /// Build a test member. `weight` must not be zero.
    fn member(id: &[u8], weight: u32) -> BinaryMember {
        BinaryMember(id.to_vec(), NonZeroU32::new(weight).unwrap())
    }

    /// Define a test member type with the given domain.
    macro_rules! domain_member {
        ($name:ident, $domain:expr) => {
            /// Test member with its own domain.
            #[derive(Debug)]
            struct $name(BinaryMember);

            impl Member for $name {
                const DOMAIN: &'static str = $domain;

                /// Return the wrapped ID.
                fn id(&self) -> &[u8] {
                    self.0.id()
                }

                /// Return the wrapped weight.
                fn weight(&self) -> NonZeroU32 {
                    self.0.weight()
                }
            }
        };
    }

    /// A domain crafted to fake another hash input is rejected.
    #[test]
    fn domain_member_concatenation_collision_is_rejected() {
        domain_member!(Plain, "x");
        domain_member!(
            Adversarial,
            concat!("x", "/placement-identity/v1\0", "\0\0\0\x13")
        );
        let members = Membership::new(vec![Plain(member(
            b"/placement-identity",
            u32::from_be_bytes(*b"/v1\0"),
        ))])
        .unwrap();
        assert_eq!(
            members.identity(),
            hash::finish(hash::domain::<Adversarial>(b"/placement-identity/v1\0"))
        );
        let placement = crate::Placement::new(2);
        assert_eq!(placement.rank(&members, b"key").unwrap(), vec![0]);
        assert_eq!(
            Membership::<Adversarial>::new(vec![]).unwrap_err(),
            Error::InvalidDomain
        );
    }

    /// Domains with NUL are rejected, even for empty memberships.
    #[test]
    fn nul_domains_rejected_for_empty_and_nonempty_memberships() {
        /// Expect `InvalidDomain` with and without members.
        macro_rules! check {
            ($domain:expr) => {{
                domain_member!(Invalid, $domain);
                for members in [vec![], vec![Invalid(member(b"id", 1))]] {
                    assert_eq!(Membership::new(members).unwrap_err(), Error::InvalidDomain);
                }
            }};
        }
        check!("\0");
        check!("\0prefix");
        check!("pre\0fix");
        check!("prefix\0");
        assert_eq!(
            Error::InvalidDomain.to_string(),
            "member domain must not contain NUL"
        );
    }

    /// Empty, Unicode, and control-character domains are allowed.
    #[test]
    fn domains_without_nul_remain_valid() {
        /// Expect success with and without members.
        macro_rules! check {
            ($domain:expr) => {{
                domain_member!(Valid, $domain);
                let empty = Membership::<Valid>::new(vec![]).unwrap();
                assert!(empty.members().is_empty());
                let members = Membership::new(vec![Valid(member(b"\0", 1))]).unwrap();
                assert_eq!(members.position(b"\0"), Some(0));
                assert_ne!(empty.identity(), members.identity());
            }};
        }
        check!("");
        check!("racer");
        check!("x/placement-identity/v1");
        check!("prefix with spaces/and\ncontrols\x01");
        check!("存储/é");
    }

    /// Members sort by ID bytes, duplicates fail, and empty is fine.
    #[test]
    fn binary_ids_sorted_duplicate_rejected_and_empty_allowed() {
        let members = Membership::new(vec![
            member(&[255, 0], u32::MAX),
            member(&[], 1),
            member(&[0], 2),
        ])
        .unwrap();
        assert_eq!(members.position(&[]), Some(0));
        assert_eq!(members.position(&[0]), Some(1));
        assert_eq!(members.position(&[255, 0]), Some(2));
        assert_eq!(members.position(&[255]), None);
        assert_eq!(members.members()[2].weight().get(), u32::MAX);
        assert_eq!(
            Membership::new(vec![member(&[0], 1), member(&[0], 2)]).unwrap_err(),
            Error::DuplicateMember
        );
        let empty = Membership::<BinaryMember>::new(vec![]).unwrap();
        assert_eq!(empty.position(&[]), None);
        assert!(empty.members().is_empty());
        let reordered = Membership::new(vec![
            member(&[0], 2),
            member(&[255, 0], u32::MAX),
            member(&[], 1),
        ])
        .unwrap();
        assert_eq!(members.identity(), reordered.identity());
    }

    /// Changes keep both positions and stop being tracked above 64.
    #[test]
    fn predecessor_delta_preserves_indices_and_caps_changes() {
        let old = Membership::new(vec![member(b"a", 1), member(b"c", 1), member(b"d", 1)]).unwrap();
        let new = Membership::new(vec![member(b"b", 1), member(b"c", 2), member(b"d", 1)])
            .unwrap()
            .with_predecessor(&old);
        let delta = new.delta.unwrap();
        assert_eq!(delta.base, old.identity());
        assert_eq!(delta.old_count, 3);
        assert_eq!(
            delta.changes,
            vec![
                MemberChange::Removed { old: 0 },
                MemberChange::Added { new: 0 },
                MemberChange::Reweighted { old: 1, new: 1 }
            ]
        );
        let unchanged = Membership::new(old.members().to_vec())
            .unwrap()
            .with_predecessor(&old);
        assert!(unchanged.delta.is_none());
        let empty = Membership::new(vec![]).unwrap();
        for count in [64, 65] {
            let new = Membership::new((0..count).map(|i| member(&[i], 1)).collect())
                .unwrap()
                .with_predecessor(&empty);
            assert_eq!(
                new.delta.is_some(),
                usize::from(count) == MAX_INCREMENTAL_CHANGES
            );
        }
    }

    /// Each change kind reports only the positions it has.
    #[test]
    fn member_changes_expose_only_present_positions() {
        for (change, old, new) in [
            (MemberChange::Removed { old: 7 }, Some(7), None),
            (MemberChange::Added { new: 3 }, None, Some(3)),
            (
                MemberChange::Reweighted { old: 7, new: 3 },
                Some(7),
                Some(3),
            ),
        ] {
            assert_eq!(change.old_position(), old);
            assert_eq!(change.new_position(), new);
        }
    }

    /// Changing a record after build does not change the membership.
    #[test]
    fn interior_mutation_cannot_change_frozen_inputs() {
        use std::cell::Cell;

        /// Record whose ID and weight can change after use.
        #[derive(Debug)]
        struct Mutable {
            id: Rc<Cell<&'static [u8]>>,

            weight: Rc<Cell<NonZeroU32>>,
        }

        impl Member for Mutable {
            const DOMAIN: &'static str = "mutable";

            /// Return the current ID.
            fn id(&self) -> &[u8] {
                self.id.get()
            }

            /// Return the current weight.
            fn weight(&self) -> NonZeroU32 {
                self.weight.get()
            }
        }
        let id = Rc::new(Cell::new(b"a".as_slice()));
        let weight = Rc::new(Cell::new(NonZeroU32::new(7).unwrap()));
        let frozen = Membership::new(vec![Mutable {
            id: id.clone(),
            weight: weight.clone(),
        }])
        .unwrap();
        let placement_identity = frozen.identity();
        let topology_identity = frozen.topology_identity();
        let graph = frozen.graph();
        id.set(b"z");
        weight.set(NonZeroU32::new(99).unwrap());
        assert_eq!(frozen.members()[0].id(), b"z");
        assert_eq!(frozen.members()[0].weight().get(), 99);
        assert_eq!(frozen.id(0), b"a");
        assert_eq!(frozen.weight(0).get(), 7);
        assert_eq!(frozen.position(b"a"), Some(0));
        assert_eq!(frozen.position(b"z"), None);
        assert_eq!(frozen.identity(), placement_identity);
        assert_eq!(frozen.topology_identity(), topology_identity);
        assert!(Arc::ptr_eq(&graph, &frozen.graph()));
        let replacement = Membership::new(vec![Mutable { id, weight }])
            .unwrap()
            .with_predecessor(&frozen);
        assert_eq!(
            replacement.delta.unwrap().changes,
            vec![
                MemberChange::Removed { old: 0 },
                MemberChange::Added { new: 0 }
            ]
        );
    }

    /// Placement hash is unchanged from v1; graph hash ignores weights.
    #[test]
    fn placement_identity_preserves_v1_and_topology_ignores_weights() {
        let old = Membership::new(vec![member(b"b", 9), member(b"a", 1)]).unwrap();
        let expected: [u8; 32] = sha2::Sha256::digest(
            b"binary-store/placement-identity/v1\0\0\0\0\x01a\0\0\0\x01\0\0\0\x01b\0\0\0\x09",
        )
        .into();
        assert_eq!(old.identity(), expected);
        let changed = Membership::new(vec![member(b"b", u32::MAX), member(b"a", 2)]).unwrap();
        assert_ne!(old.identity(), changed.identity());
        assert_eq!(old.topology_identity(), changed.topology_identity());
        assert_eq!(old.graph(), changed.graph());
        let removed = Membership::new(vec![member(b"a", 1)]).unwrap();
        assert_ne!(old.topology_identity(), removed.topology_identity());
        domain_member!(OtherDomain, "another-domain");
        let other = Membership::new(vec![
            OtherDomain(member(b"a", 1)),
            OtherDomain(member(b"b", 9)),
        ])
        .unwrap();
        assert_ne!(old.topology_identity(), other.topology_identity());
    }

    /// An identical or too-different predecessor clears old change records.
    #[test]
    fn replacing_predecessor_clears_stale_delta() {
        let old = Membership::new(vec![member(b"a", 1)]).unwrap();
        let identical = Membership::new(vec![member(b"b", 1)]).unwrap();
        let new = Membership::new(vec![member(b"b", 1)])
            .unwrap()
            .with_predecessor(&old)
            .with_predecessor(&identical);
        assert!(new.delta.is_none());
        let large = Membership::new((0..100).map(|i| member(&[i], 1)).collect()).unwrap();
        let new = Membership::new(vec![member(b"b", 1)])
            .unwrap()
            .with_predecessor(&old)
            .with_predecessor(&large);
        assert!(new.delta.is_none());
    }

    /// Random change lists match a simple map-based reference.
    #[test]
    fn randomized_diff_matches_independent_id_map() {
        use std::collections::{BTreeMap, BTreeSet};
        let mut state = 0x3d89_720a_481c_652fu64;
        let mut draw = || {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            state
        };
        let mut incremental_cases = 0;
        let mut cold_cases = 0;
        for iteration in 0..64 {
            let mut make = || {
                let mut values = Vec::new();
                for id in 0..if iteration % 2 == 0 { 24u8 } else { 96u8 } {
                    let sample = draw();
                    if sample % 3 != 0 {
                        values.push(member(&[id], (sample >> 32) as u32 | 1));
                    }
                }
                Membership::new(values).unwrap()
            };
            let old = make();
            let new = make();
            let map =
                |members: &Membership<BinaryMember>| -> BTreeMap<Vec<u8>, (usize, NonZeroU32)> {
                    members
                        .members()
                        .iter()
                        .enumerate()
                        .map(|(position, value)| (value.id().to_vec(), (position, value.weight())))
                        .collect()
                };
            let before = map(&old);
            let after = map(&new);
            let ids: BTreeSet<_> = before.keys().chain(after.keys()).collect();
            let expected: Vec<_> = ids
                .into_iter()
                .filter_map(|id| {
                    let a = before.get(id);
                    let b = after.get(id);
                    if a.map(|v| v.1) == b.map(|v| v.1) {
                        None
                    } else {
                        Some((a.map(|v| v.0), b.map(|v| v.0)))
                    }
                })
                .collect();
            let new = new.with_predecessor(&old);
            if expected.is_empty() || expected.len() > MAX_INCREMENTAL_CHANGES {
                cold_cases += 1;
                assert!(new.delta.is_none());
            } else {
                incremental_cases += 1;
                let delta = new.delta.unwrap();
                assert_eq!(delta.base, old.identity());
                assert_eq!(delta.old_count, old.members().len());
                let actual: Vec<_> = delta
                    .changes
                    .iter()
                    .map(|change| (change.old_position(), change.new_position()))
                    .collect();
                assert_eq!(actual, expected);
            }
        }
        assert!(incremental_cases > 0);
        assert!(cold_cases > 0);
    }

    /// Byte estimate matches a hand count, including the shared graph.
    #[test]
    fn retained_storage_counts_capacities_and_shared_graph_once() {
        let old = Membership::new(vec![member(b"a", 1)]).unwrap();
        let new = Membership::new(vec![member(b"a", 2), member(b"longer-id", 3)])
            .unwrap()
            .with_predecessor(&old);
        let expected = new.members.capacity() * size_of::<BinaryMember>()
            + new.ids.capacity() * size_of::<Box<[u8]>>()
            + new.ids.iter().map(|id| id.len()).sum::<usize>()
            + new.weights.capacity() * size_of::<NonZeroU32>()
            + new.delta.as_ref().unwrap().changes.capacity() * size_of::<MemberChange>()
            + size_of::<Vec<Vec<usize>>>()
            + 2 * size_of::<usize>()
            + new.graph.capacity() * size_of::<Vec<usize>>()
            + new
                .graph
                .iter()
                .map(|row| row.capacity() * size_of::<usize>())
                .sum::<usize>();
        assert_eq!(new.retained_bytes(), expected);
        let shared = new.graph();
        assert_eq!(new.retained_bytes(), expected);
        assert!(Arc::ptr_eq(&shared, &new.graph()));
        let empty = Membership::<BinaryMember>::new(vec![]).unwrap();
        assert_eq!(
            empty.retained_bytes(),
            size_of::<Vec<Vec<usize>>>() + 2 * size_of::<usize>()
        );
    }

    /// A membership of plain records can move to another thread.
    #[test]
    fn membership_is_send_and_sync_for_plain_members() {
        /// Compile only if `T` is `Send` and `Sync`.
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<Membership<BinaryMember>>();
        let members = Membership::new(vec![member(b"a", 1), member(b"b", 2)]).unwrap();
        let graph = members.graph();
        std::thread::spawn(move || {
            assert_eq!(members.position(b"a"), Some(0));
            assert!(Arc::ptr_eq(&graph, &members.graph()));
            assert_eq!(members.neighbors(0), vec![1]);
        })
        .join()
        .unwrap();
    }

    /// ID length limit, checked without huge allocations.
    #[test]
    fn member_id_length_is_checked_without_large_allocations() {
        assert_eq!(validate_id_length(0), Ok(()));
        if let Ok(maximum) = usize::try_from(u32::MAX) {
            assert_eq!(validate_id_length(maximum), Ok(()));
            if let Some(oversized) = maximum.checked_add(1) {
                assert_eq!(validate_id_length(oversized), Err(Error::InvalidMember));
            }
        }
    }

    /// The graph is reused when IDs match, even if weights differ.
    #[test]
    fn predecessor_constructor_shares_graph_only_for_identical_frozen_ids() {
        let old = Membership::new(vec![member(b"a", 1), member(b"b", 2)]).unwrap();
        let same =
            Membership::new_with_predecessor(vec![member(b"b", 9), member(b"a", 7)], &old).unwrap();
        assert!(Arc::ptr_eq(&old.graph(), &same.graph()));
        assert_ne!(old.identity(), same.identity());
        assert_eq!(same.delta.as_ref().unwrap().changes.len(), 2);
        let changed =
            Membership::new_with_predecessor(vec![member(b"a", 1), member(b"c", 2)], &old).unwrap();
        assert!(!Arc::ptr_eq(&old.graph(), &changed.graph()));
        assert_eq!(
            changed.graph(),
            Membership::new(changed.members().to_vec()).unwrap().graph()
        );
        assert!(
            Membership::new_with_predecessor(vec![member(b"a", 1), member(b"a", 2)], &old).is_err()
        );
    }

    /// `retained_bytes` uses the stored measure plus the change list.
    #[test]
    fn retained_bytes_uses_cached_storage_and_bounded_delta_capacity() {
        let old = Membership::new(vec![member(b"a", 1)]).unwrap();
        let mut current = Membership::new_with_predecessor(vec![member(b"a", 2)], &old).unwrap();
        assert_eq!(current.owned_bytes, current.measure_owned_bytes());
        let delta_bytes =
            current.delta.as_ref().unwrap().changes.capacity() * size_of::<MemberChange>();
        assert_eq!(current.retained_bytes(), current.owned_bytes + delta_bytes);
        current.owned_bytes = 17;
        assert_eq!(current.retained_bytes(), 17 + delta_bytes);
        let identical = Membership::new(vec![member(b"a", 2)]).unwrap();
        current = current.with_predecessor(&identical);
        assert_eq!(current.retained_bytes(), 17);
    }
}
