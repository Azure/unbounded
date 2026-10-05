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
//!
//! # Membership and compatibility
//!
//! Membership freezes arbitrary binary IDs and positive weights, rejects duplicate
//! IDs, IDs longer than `u32::MAX`, and NUL-containing domains, then sorts by ID.
//! Positions belong to that snapshot, not a durable cluster-wide index. Empty
//! memberships are valid but have no valid route endpoints.
//!
//! The overlay joins 32 independently hashed rings over stable IDs. Edges are
//! symmetric, unique, sorted, self-free, and bounded by [`MAX_DEGREE`]. Every
//! nonempty membership is connected, but this does not guarantee four-hop routes.
//! A single join or leave changes only predecessor/successor edges in each ring.
//! Construction is synchronous: O(32 N log N) for bounded-size IDs, with up to
//! 64 N adjacency slots, N vector headers, and one reused N-entry sorting buffer.
//! At 100,000 members, adjacency slots alone occupy about 48.8 MiB on 64-bit targets.
//!
//! Placement retains the `/slot/v1`, `/hrw/v1`, and `/placement-identity/v1`
//! contracts: 2^20 slots, integer weighted rendezvous, and up to three replicas.
//! Ring ordering uses its own v1 domain and u64 ID lengths. Routing's
//! `/next-hop/v5` schema independently prefixes seed, source ID, and destination
//! ID with big-endian u32 lengths. Do not merge these distinct encodings.
//! Schema changes require coordinated cluster transitions: matching membership
//! versions do not establish mixed-routing safety or automatic negotiation.
//!
//! # Cache ownership and budgets
//!
//! Placement's bounded CLOCK cache coalesces resident work and pins active
//! requests. Zero capacity computes uncached. Cold async scoring handles at most
//! 256 members per poll plus a bounded admission delta; CLOCK admission may inspect
//! the resident cache. Sync ranking falls back to uncached scoring under pressure.
//! Maintenance migrates demanded predecessor slots, not the entire slot space.
//!
//! Paths have independent entry, accounted-byte, and distinct cold-search limits.
//! Disabling retention does not disable searches; disabling search admission still
//! permits validated self routes and cache hits. Searches yield after at most 32
//! vertex expansions, each with at most 64 edges. Equivalent canonical queries
//! share search admission across seeds and weight-only snapshots. The last dropped
//! waiter releases unfinished search state. Admission does not bound waiter count
//! or scratch bytes. Oversized results do not flush useful smaller cached routes.
//! [`Paths::new`] defaults to 8 MiB of accounted cache bytes and eight active
//! searches. Search retention excludes active scratch, returned results, graphs,
//! allocator overhead, and the inline cache object.
//!
//! Routing retains one canonical shortest-path witness per eligible first hop,
//! not every full path. Selection uses frozen next-hop weights and bounded integer
//! rejection sampling, reselecting on cache hits for each caller's seed and weights.
//! Visited positions exclude nodes throughout the path; blocked positions remove
//! only edges from the source. Queries allow at most 255 visited positions plus
//! remaining links, 64 blocked inputs, and a 65,536-byte seed. Visited positions
//! must be distinct and exclude endpoints; blocked duplicates are canonicalized.
//! Self routes validate inputs before bypassing admission, and zero links reach
//! only self. Applications may impose tighter forwarding limits.
//!
//! Placement CLOCK and path LRU intentionally remain separate: their pinning,
//! migration, admission, and variable-byte retention requirements differ.
//! [`Placement::ENTRY_BYTES`] models resident structures, not allocator metadata
//! or fragmentation. Membership estimates exclude nested application allocations
//! and count a shared graph in full for each snapshot. Unique allocation accounting
//! must deduplicate graphs; conservative publication budgets may count each copy.
//! Neither estimate is an RSS cap, and leased snapshots need separate headroom.
//!
//! # Application boundary
//!
//! Racer owns publication, leases, authenticated forwarding, deadlines,
//! cancellation, health, transport, and application key encoding. Its control
//! publication owner prepares memberships off the I/O thread, while installation
//! separately checks accepted state. Canceling a waiter does not cancel CPU work
//! already started; the application owns job admission and shutdown cleanup.
//! Racer admits one unfinished preparation job per store without a backlog and
//! retains its admission after waiter cancellation. Dropping the store joins that
//! owned job without an independent timeout. Input-size bounds do not guarantee
//! elapsed time. Unchanged membership version/content can reuse a whole snapshot;
//! new versions can share graphs through [`Membership::new_with_predecessor`].
//! Cache budgets are partitioned among I/O workers, while active path-search
//! limits are per worker. These policies do not belong in the algorithm crate.
//!
//! Public constants describe caller contracts: replica/slot geometry, degree,
//! incremental-delta limit, and placement entry accounting. Work quanta and hash
//! domains remain implementation details. The non-exhaustive error enum keeps
//! invalid input, overload, unreachability, and exhausted sampling distinct.
#![forbid(unsafe_code)]
#![deny(missing_docs)]

/// Cooperative equal-cost routing over the immutable overlay.
mod paths;

/// Weighted rendezvous placement and worker-local ranking maintenance.
mod placement;

pub use paths::{PathQuery, Paths};

pub use placement::{Maintenance, Placement, REPLICAS, SLOT_BITS, SLOT_COUNT, slot};

use sha2::Digest;

use std::{mem::size_of, num::NonZeroU32, sync::Arc};

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
    /// Describe the failure without exposing application-owned records.
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

/// Maximum changed IDs retained for incremental computation.
pub const MAX_INCREMENTAL_CHANGES: usize = 64;

/// Immutable ID-sorted members and bounded predecessor hints.
///
/// IDs and weights are snapshotted at construction. Original records remain
/// accessible for metadata, but interior mutation cannot change algorithm inputs.
/// Membership is `Send` and `Sync` when `M` is; caches remain worker-local.
#[derive(Debug)]
pub struct Membership<M: Member> {
    /// Application records kept in frozen ID order, not read during algorithms.
    members: Vec<M>,

    /// Owned identity bytes that interior application mutation cannot change.
    ids: Vec<Box<[u8]>>,

    /// Positive placement and next-hop weights frozen alongside the IDs.
    weights: Vec<NonZeroU32>,

    /// Placement schema digest including weights.
    identity: [u8; 32],

    /// Overlay schema digest excluding weights.
    topology_identity: [u8; 32],

    /// Immutable adjacency shared by searches and ID-identical successors.
    graph: Arc<Vec<Vec<usize>>>,

    /// Construction-time allocation estimate, excluding the replaceable delta.
    owned_bytes: usize,

    /// Optional bounded hint for exactly one predecessor generation.
    delta: Option<MembershipDelta>,
}

/// One changed ID with positions in its old and new snapshots.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum MemberChange {
    /// An ID present only in the predecessor.
    Removed { old: usize },

    /// An ID present only in the replacement.
    Added { new: usize },

    /// An unchanged ID whose frozen weight changed.
    Reweighted {
        /// Position of the ID in the predecessor.
        old: usize,

        /// Position of that same ID in the replacement.
        new: usize,
    },
}

impl MemberChange {
    /// Return the predecessor position, if this ID existed there.
    fn old_position(self) -> Option<usize> {
        match self {
            Self::Removed { old } | Self::Reweighted { old, .. } => Some(old),
            Self::Added { .. } => None,
        }
    }

    /// Return the replacement position, if this ID still exists.
    fn new_position(self) -> Option<usize> {
        match self {
            Self::Added { new } | Self::Reweighted { new, .. } => Some(new),
            Self::Removed { .. } => None,
        }
    }
}

/// Bounded, ID-ordered changes relative to one predecessor generation.
#[derive(Debug)]
struct MembershipDelta {
    /// Placement identity of the predecessor, including its frozen weights.
    base: [u8; 32],

    /// Number of scores needed before predecessor winners can be reused.
    old_count: usize,

    /// Changes ordered by frozen ID for exact old-to-new index translation.
    changes: Vec<MemberChange>,
}

impl<M: Member> Membership<M> {
    /// Freeze member IDs and weights, sort by ID, and construct ring adjacency.
    ///
    /// Each accessor is read once. Construction hashes each ID 32 times and sorts
    /// 32 rings, with O(64 N) adjacency and O(N) temporary ring sorting storage.
    ///
    /// # Errors
    /// Returns [`Error::InvalidDomain`] for NUL-containing domains,
    /// [`Error::DuplicateMember`] for duplicate frozen IDs, or
    /// [`Error::InvalidMember`] for an ID longer than `u32::MAX` bytes.
    ///
    /// # Panics
    /// Propagates panics from application-provided [`Member`] accessors.
    pub fn new(members: Vec<M>) -> Result<Self, Error> {
        Self::build(members, None)
    }

    /// Freeze members and prepare hints, sharing the predecessor's immutable
    /// graph when frozen IDs match. Weight and metadata changes do not rebuild
    /// rings. Changed IDs use the same construction as [`Self::new`].
    /// Does not retain the predecessor membership itself.
    ///
    /// # Errors
    /// Returns the same validation errors as [`Self::new`].
    ///
    /// # Panics
    /// Propagates panics from application-provided [`Member`] accessors.
    pub fn new_with_predecessor(members: Vec<M>, old: &Self) -> Result<Self, Error> {
        Ok(Self::build(members, Some(old))?.with_predecessor(old))
    }

    /// Validate and snapshot records, optionally reusing ID-identical adjacency.
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

    /// Prepare bounded incremental hints outside request processing.
    /// Larger changes use exact cooperative cold computation on demand.
    /// Replaces previous hints, retaining at most [`MAX_INCREMENTAL_CHANGES`]
    /// changed IDs. Does not retain the predecessor or replace this graph.
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

    /// Original records ordered by frozen ID; mutation cannot alter snapshots.
    #[must_use]
    pub fn members(&self) -> &[M] {
        &self.members
    }

    /// Find a frozen ID's position, or `None` if it is absent.
    #[must_use]
    pub fn position(&self, id: &[u8]) -> Option<usize> {
        self.ids.binary_search_by(|m| m.as_ref().cmp(id)).ok()
    }

    /// Placement identity v1 over domain, frozen IDs, and frozen weights only.
    #[must_use]
    pub fn identity(&self) -> [u8; 32] {
        self.identity
    }

    /// Frozen ID at a valid position.
    ///
    /// # Panics
    /// Panics if the position is outside the member slice.
    fn id(&self, position: usize) -> &[u8] {
        &self.ids[position]
    }

    /// Frozen weight at a valid position.
    ///
    /// # Panics
    /// Panics if the position is outside the member slice.
    fn weight(&self, position: usize) -> NonZeroU32 {
        self.weights[position]
    }

    /// Domain, frozen IDs, and overlay algorithm, independent of weights.
    fn topology_identity(&self) -> [u8; 32] {
        self.topology_identity
    }

    /// Sorted, unique, symmetric ring neighbors; invalid positions return empty.
    #[must_use]
    pub fn neighbors(&self, position: usize) -> Vec<usize> {
        self.neighbor_slice(position).to_vec()
    }

    /// Borrow sorted adjacency without allocating; invalid positions return empty.
    fn neighbor_slice(&self, position: usize) -> &[usize] {
        self.graph.get(position).map_or(&[], Vec::as_slice)
    }

    /// Share immutable adjacency with cooperative searches without copying it.
    fn graph(&self) -> Arc<Vec<Vec<usize>>> {
        Arc::clone(&self.graph)
    }

    /// Estimated allocated storage using retained capacities rather than lengths.
    /// Includes frozen inputs, member buffer, delta, and the full shared graph.
    /// Excludes this inline object, allocator overhead, and nested allocations in
    /// application records. Count shared graphs once for unique-allocation totals.
    /// Saturates at `usize::MAX`. O(1): immutable storage is measured once and only
    /// the bounded delta capacity is inspected on each call.
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

    /// Measure immutable capacities once, charging the entire shared graph.
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

/// Reject IDs that cannot be represented by the placement hash schema.
fn validate_id_length(length: usize) -> Result<(), Error> {
    u32::try_from(length)
        .map(|_| ())
        .map_err(|_| Error::InvalidMember)
}

/// Stable SHA-256 helpers; callers own schema domains and field order.
mod hash {
    use crate::Member;

    use sha2::{Digest, Sha256};

    /// Start a hash with the member domain and exact algorithm suffix.
    pub(super) fn domain<M: Member>(suffix: &[u8]) -> Sha256 {
        let mut hash = named_domain(M::DOMAIN.as_bytes());
        hash.update(suffix);
        hash
    }

    /// Start a hash with the exact domain, including any schema terminator.
    fn named_domain(name: &[u8]) -> Sha256 {
        let mut hash = Sha256::new();
        hash.update(name);
        hash
    }

    /// Append a u32-length-prefixed field; callers must bound its length.
    pub(super) fn bytes(hash: &mut Sha256, value: &[u8]) {
        hash.update(field_length(value.len()).to_be_bytes());
        hash.update(value);
    }

    /// Check length representability without silently truncating schema bytes.
    fn field_length(length: usize) -> u32 {
        u32::try_from(length).expect("topology hash fields must fit the u32 length schema")
    }

    /// Finalize the key without changing the underlying digest.
    pub(super) fn finish(hash: Sha256) -> [u8; 32] {
        hash.finalize().into()
    }

    /// Exact encoding and boundary checks for shared hash primitives.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Length conversion accepts schema endpoints and rejects overflow.
        #[test]
        fn length_prefix_checks_without_allocating_gigabytes() {
            assert_eq!(field_length(0), 0);
            assert_eq!(field_length(u32::MAX as usize), u32::MAX);
            if let Some(overflow) = (u32::MAX as usize).checked_add(1) {
                assert!(std::panic::catch_unwind(|| field_length(overflow)).is_err());
            }
        }

        /// Helper composition preserves exact application domain and field bytes.
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

        /// Length framing and domains distinguish otherwise ambiguous inputs.
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

/// Symmetric union of 32 domain-separated SHA-256 rings over frozen IDs.
/// Weights never affect edges. Construction retains at most 64 N adjacency
/// slots and reuses one N-entry digest/position array as sorting scratch.
mod overlay {
    use crate::{Member, hash};

    use sha2::Digest;

    /// Number of independent rings in this compatibility version.
    pub(super) const RINGS: u32 = 32;

    /// Exact compatibility prefix for each ring's ordering digest.
    const RING_DOMAIN: &[u8] = b"/overlay/sha256-rings-32/v1\0";

    /// Exact compatibility prefix for graph cache identities.
    const IDENTITY_DOMAIN: &[u8] = b"/topology-identity/sha256-rings-32/v1\0";

    /// Hash the domain, algorithm version, and sorted IDs without weights.
    pub(super) fn identity<M: Member>(ids: &[Box<[u8]>]) -> [u8; 32] {
        let mut digest = hash::domain::<M>(IDENTITY_DOMAIN);
        for id in ids {
            digest.update((id.len() as u64).to_be_bytes());
            digest.update(id);
        }
        hash::finish(digest)
    }

    /// Join consecutive sorted ring entries, then canonicalize adjacency rows.
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

    /// Overlay invariants and ID-local churn checks.
    #[cfg(test)]
    mod tests {
        use super::*;

        use crate::Membership;

        use std::{collections::BTreeSet, num::NonZeroU32};

        /// Fixed-width ID and positive weight for ring tests.
        #[derive(Clone, Debug)]
        struct Node([u8; 4], NonZeroU32);

        impl Member for Node {
            const DOMAIN: &'static str = "overlay-tests";

            /// Return the stable binary ID.
            fn id(&self) -> &[u8] {
                &self.0
            }

            /// Return the supplied positive weight.
            fn weight(&self) -> NonZeroU32 {
                self.1
            }
        }

        /// Build one equally weighted test node.
        fn node(id: u32) -> Node {
            Node(id.to_be_bytes(), NonZeroU32::new(1).unwrap())
        }

        /// Leave odd ID gaps to exercise ordered insertions.
        fn membership(count: u32) -> Membership<Node> {
            Membership::new((0..count).map(|id| node(2 * id)).collect()).unwrap()
        }

        /// Express undirected edges in stable IDs rather than shifted positions.
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

        /// Validate graph invariants across empty, small, and larger memberships.
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

        /// Joins and leaves alter only predecessor/successor edges in each ring.
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

/// Membership snapshots, validation, bounded deltas, and allocation accounting.
#[cfg(test)]
mod tests {
    use super::*;

    use std::{num::NonZeroU32, rc::Rc};

    /// Arbitrary binary ID with a positive test weight.
    #[derive(Clone, Debug)]
    struct BinaryMember(Vec<u8>, NonZeroU32);

    impl Member for BinaryMember {
        const DOMAIN: &'static str = "binary-store";

        /// Return the binary identity.
        fn id(&self) -> &[u8] {
            &self.0
        }

        /// Return the positive placement weight.
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }

    /// Build a binary test member with a checked positive weight.
    fn member(id: &[u8], weight: u32) -> BinaryMember {
        BinaryMember(id.to_vec(), NonZeroU32::new(weight).unwrap())
    }

    /// Define a test member using a chosen application hash domain.
    macro_rules! domain_member {
        ($name:ident, $domain:expr) => {
            /// Binary test record with a distinct compile-time domain.
            #[derive(Debug)]
            struct $name(BinaryMember);

            impl Member for $name {
                const DOMAIN: &'static str = $domain;

                /// Return the wrapped binary identity.
                fn id(&self) -> &[u8] {
                    self.0.id()
                }

                /// Return the wrapped positive weight.
                fn weight(&self) -> NonZeroU32 {
                    self.0.weight()
                }
            }
        };
    }

    /// Reject domains that could absorb placement field framing.
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

    /// Domain validation applies before empty/nonempty membership processing.
    #[test]
    fn nul_domains_rejected_for_empty_and_nonempty_memberships() {
        /// Check both empty and populated records for the invalid domain.
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

    /// Empty, Unicode, and non-NUL control bytes remain valid domain inputs.
    #[test]
    fn domains_without_nul_remain_valid() {
        /// Check the valid domain with empty and binary-ID records.
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

    /// Frozen binary ordering is deterministic and duplicates are invalid.
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

    /// Ordered changes retain old/new positions and fall back beyond 64 IDs.
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

    /// Typed changes expose only positions that exist in their snapshot.
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

    /// Interior record changes cannot affect frozen identity, weight, or graph.
    #[test]
    fn interior_mutation_cannot_change_frozen_inputs() {
        use std::cell::Cell;

        /// Mutable application metadata used to challenge snapshot isolation.
        #[derive(Debug)]
        struct Mutable {
            id: Rc<Cell<&'static [u8]>>,

            weight: Rc<Cell<NonZeroU32>>,
        }

        impl Member for Mutable {
            const DOMAIN: &'static str = "mutable";

            /// Read the current application identity.
            fn id(&self) -> &[u8] {
                self.id.get()
            }

            /// Read the current application weight.
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

    /// Placement bytes remain v1 while graph identities exclude weights.
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

    /// Replacing hints with identical or over-budget predecessors clears them.
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

    /// Compare bounded merge diffs with an independently ordered ID-map oracle.
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

    /// Account for actual retained capacities including the full shared graph.
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

    /// Plain records permit snapshots and their shared graph to cross threads.
    #[test]
    fn membership_is_send_and_sync_for_plain_members() {
        /// Require both auto traits at compile time.
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

    /// Validate length boundaries without constructing gigabyte-sized records.
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

    /// Graph sharing depends only on frozen ID equality, not weight equality.
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

    /// A sentinel proves retained accounting uses cached immutable storage.
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
