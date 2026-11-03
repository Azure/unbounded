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
//! let owners = placement.rank(&members, b"opaque application key");
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
//! # Membership and pools
//!
//! Each member has a binary ID, a positive weight, and a Site name
//! ([`Member::site`], empty by default). IDs must be unique. An empty
//! membership is allowed, but has nothing to route between.
//!
//! Members are grouped into pools ([`PoolSpec`], [`Pool`]). Pools may overlap.
//! [`Membership::new`] makes one default pool of every member. Each pool has
//! its own mesh, and the membership's graph is the union of the pool meshes.
//!
//! A pool of up to [`FULL_MESH_MAX`] members is a full mesh. A larger pool is
//! split into columns, mostly by Site. Members of a column are all linked,
//! and each member links to one delegate in every other column. So any two
//! members of a pool are at most 2 links apart, with two disjoint 2-link
//! paths. A member of a pool of `k` members has about `2.83 * sqrt(k)`
//! neighbors in it. One join or leave changes O(sqrt(k)) edges. Weights and
//! pool IDs do not affect the mesh. See `designs/racer-mesh-v2.md`.
//!
//! Neighbor lists are not stored. Each pool keeps the column of each member,
//! the members of each column, one hash ring per column (16 points per
//! member), and its members sorted by delegate key: about 230 bytes per
//! member. Building takes O(k log k) time per pool. [`Membership::neighbors`]
//! computes a list on demand; [`Membership::is_neighbor`] is a few binary
//! searches per shared pool.
//!
//! When membership changes, build the new one with
//! [`Membership::new_with_predecessor`] or
//! [`Membership::with_pools_and_predecessor`]. They reuse old pool meshes
//! when IDs, Sites, and the pool's members are the same, and help
//! [`Placement`] update its cache cheaply.
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
//! limits. [`Membership::retained_bytes`] counts a shared mesh in full for
//! every membership that uses it, so adding them up overcounts.
//!
//! # Compatibility
//!
//! Every node in a cluster must agree on placement and routing. Placement
//! depends on [`Member::DOMAIN`], member IDs, weights, and this crate's hash
//! formats. The mesh also depends on Sites and pool members, and routes on
//! pool ranks.
//!
//! - Changing a domain changes the hashes; placement and routes may move.
//! - Hash formats are versioned (`/slot/v1`, `/hrw/v1`,
//!   `/placement-identity/v1`, `/next-hop/v1`, `/mesh/v2/column`,
//!   `/mesh/v2/delegate`, `/mesh/v2/point`, `/mesh/v2/edge`, and
//!   `/topology-identity/mesh/v2`). Changing one needs a planned
//!   cluster-wide rollout. Nothing here detects a mismatch.
#![forbid(unsafe_code)]
#![deny(missing_docs)]

/// The per-pool mesh.
mod mesh;

/// Routing between members.
mod paths;

/// Choosing which members store a key.
mod placement;

pub use mesh::{EdgeKind, FULL_MESH_MAX, POINTS, Pool, PoolSpec, RDMA_PEERS, jump, target_columns};

pub use paths::{
    Alternatives, DEFAULT_SEARCH_QUEUE, Exhaustive, MAX_BLOCKED, MeshSearch, PathKey, PathQuery,
    Paths, Searches,
};

pub use placement::{
    Maintenance, Placement, RANKED_DEPTH, REPLICAS, RETAINED_DEPTH, Ranking, RankingEntry,
    Rankings, SLOT_BITS, SLOT_COUNT, Scored, rank_of, slot,
};

use sha2::Digest;

use std::{mem::size_of, num::NonZeroU32, sync::Arc};

/// A cluster member, as seen by this crate.
///
/// [`Membership`] reads the ID, weight, and Site once, when it is built.
pub trait Member {
    /// A name that keeps this application's hashes apart from others.
    ///
    /// Must not contain a NUL byte (`\0`). Any other string, even an empty one,
    /// is fine. Changing it changes the hashes; placement and routes may move.
    const DOMAIN: &'static str;

    /// Unique ID for this member. Should not change over the member's life.
    fn id(&self) -> &[u8];

    /// How much this member is favored, compared to others. Higher is more.
    /// Used for both placement and route tie-breaks.
    fn weight(&self) -> NonZeroU32;

    /// Site name. Members of a large Site get columns of their own in each
    /// pool mesh. Affects only the mesh, not placement. Empty by default; the
    /// empty name is a Site like any other. At most `u32::MAX` bytes.
    fn site(&self) -> &[u8] {
        &[]
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[non_exhaustive]
/// Errors from this crate.
pub enum Error {
    /// Two members have the same ID.
    DuplicateMember,

    /// [`Member::DOMAIN`] contains a NUL byte.
    InvalidDomain,

    /// A member ID or Site is longer than `u32::MAX` bytes.
    InvalidMember,

    /// A pool names an unknown member or names one twice, or two pools share
    /// an ID.
    InvalidPool,

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
            Self::InvalidPool => "invalid pool",
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

/// Hash prefix for [`Membership::topology_identity`].
const TOPOLOGY_IDENTITY_DOMAIN: &[u8] = b"/topology-identity/mesh/v2\0";

/// A fixed set of members, sorted by ID, and their pools.
///
/// IDs, weights, and Sites are copied when it is built, so later changes to
/// the records do not affect it. It can be shared across threads if `M` can.
#[derive(Debug)]
pub struct Membership<M: Member> {
    /// The original records, in ID order.
    members: Vec<M>,

    /// Copied IDs, in order.
    ids: Vec<Box<[u8]>>,

    /// Copied weights, matching `ids`.
    weights: Vec<NonZeroU32>,

    /// Copied Sites, matching `ids`.
    sites: Vec<Box<[u8]>>,

    /// Hash of the domain, IDs, and weights.
    identity: [u8; 32],

    /// Hash of the domain, IDs, Sites, and pools. Used as the route cache key.
    topology_identity: [u8; 32],

    /// Pools sorted by ID. Meshes are shared with equal pools and
    /// predecessors.
    pools: Arc<[Pool]>,

    /// Estimated bytes, measured once at build time. Excludes `delta`.
    owned_bytes: usize,

    /// What changed since the previous membership, if known.
    delta: Option<MembershipDelta>,

    /// Position in the chain of memberships built with
    /// [`Self::with_predecessor`]: 0 for one built without a predecessor,
    /// otherwise the predecessor's plus one. Local: two processes that
    /// started at different memberships number them differently.
    generation: u32,

    /// Per member, matching `ids`: the [`Self::generation`] of the first
    /// membership in this chain that listed it without a break. Members of
    /// a membership built without a predecessor all have 0 (unknown, at
    /// least as old as the chain).
    since: Vec<u32>,
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
    /// Build a membership with one default pool of every member
    /// ([`PoolSpec::default_pool`]): copy IDs, weights, and Sites, sort by
    /// ID, and build the mesh.
    ///
    /// This runs synchronously and takes O(N log N) time, so choose where to
    /// run it with care for large memberships.
    ///
    /// # Errors
    /// - [`Error::InvalidDomain`] if [`Member::DOMAIN`] contains NUL.
    /// - [`Error::DuplicateMember`] if two members share an ID.
    /// - [`Error::InvalidMember`] if an ID or Site is longer than `u32::MAX`
    ///   bytes.
    ///
    /// # Panics
    /// Panics if a [`Member`] method panics.
    pub fn new(members: Vec<M>) -> Result<Self, Error> {
        Self::build(members, vec![PoolSpec::default_pool()], None)
    }

    /// Build a membership with these pools. With no pools there are no
    /// edges, and building does no mesh work (placement only).
    ///
    /// # Errors
    /// Same as [`Self::new`], plus [`Error::InvalidPool`] if a pool names an
    /// unknown member, names a member twice, or two pools share an ID.
    ///
    /// # Panics
    /// Panics if a [`Member`] method panics.
    pub fn with_pools(members: Vec<M>, pools: Vec<PoolSpec>) -> Result<Self, Error> {
        Self::build(members, pools, None)
    }

    /// Build a membership that replaces `old`, with one default pool.
    ///
    /// Same as [`Self::new`] followed by [`Self::with_predecessor`], but also
    /// reuses `old`'s meshes where possible (see
    /// [`Self::with_pools_and_predecessor`]). Does not keep `old` alive.
    ///
    /// # Errors
    /// Same as [`Self::new`].
    ///
    /// # Panics
    /// Panics if a [`Member`] method panics.
    pub fn new_with_predecessor(members: Vec<M>, old: &Self) -> Result<Self, Error> {
        Self::with_pools_and_predecessor(members, vec![PoolSpec::default_pool()], old)
    }

    /// Build a membership with these pools that replaces `old`.
    ///
    /// Same as [`Self::with_pools`] followed by [`Self::with_predecessor`].
    /// When the member IDs and Sites equal `old`'s, a pool whose members
    /// equal an old pool's reuses that pool's mesh. Does not keep `old` alive.
    ///
    /// # Errors
    /// Same as [`Self::with_pools`].
    ///
    /// # Panics
    /// Panics if a [`Member`] method panics.
    pub fn with_pools_and_predecessor(
        members: Vec<M>,
        pools: Vec<PoolSpec>,
        old: &Self,
    ) -> Result<Self, Error> {
        Ok(Self::build(members, pools, Some(old))?.with_predecessor(old))
    }

    /// Shared code for the constructors. Reuses `old`'s meshes if IDs and
    /// Sites match.
    fn build(members: Vec<M>, pools: Vec<PoolSpec>, old: Option<&Self>) -> Result<Self, Error> {
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
                let site = member.site();
                validate_id_length(site.len())?;
                let site: Box<[u8]> = site.into();
                let weight = member.weight();
                Ok((member, id, weight, site))
            })
            .collect::<Result<_, Error>>()?;
        frozen.sort_unstable_by(|a, b| a.1.cmp(&b.1));
        if frozen.windows(2).any(|pair| pair[0].1 == pair[1].1) {
            return Err(Error::DuplicateMember);
        }
        let mut members = Vec::with_capacity(frozen.len());
        let mut ids = Vec::with_capacity(frozen.len());
        let mut weights = Vec::with_capacity(frozen.len());
        let mut sites = Vec::with_capacity(frozen.len());
        let mut digest = hash::domain::<M>(b"/placement-identity/v1\0");
        for (member, id, weight, site) in frozen {
            hash::bytes(&mut digest, &id);
            digest.update(weight.get().to_be_bytes());
            members.push(member);
            ids.push(id);
            weights.push(weight);
            sites.push(site);
        }
        let pools = Self::build_pools(&ids, &sites, pools, old)?;
        let topology_identity = topology_identity::<M>(&ids, &sites, &pools);
        let mut membership = Self {
            members,
            ids,
            weights,
            sites,
            identity: hash::finish(digest),
            topology_identity,
            pools: pools.into(),
            owned_bytes: 0,
            delta: None,
            generation: 0,
            since: Vec::new(),
        };
        membership.since = vec![0; membership.ids.len()];
        membership.owned_bytes = membership.measure_owned_bytes();
        Ok(membership)
    }

    /// Resolve pool specs to positions and build (or reuse) their meshes.
    fn build_pools(
        ids: &[Box<[u8]>],
        sites: &[Box<[u8]>],
        specs: Vec<PoolSpec>,
        old: Option<&Self>,
    ) -> Result<Vec<Pool>, Error> {
        let mut resolved = Vec::with_capacity(specs.len());
        for spec in specs {
            validate_id_length(spec.id.len()).map_err(|_| Error::InvalidPool)?;
            let positions = match spec.members {
                None => (0..ids.len()).collect(),
                Some(list) => {
                    let mut positions = list
                        .iter()
                        .map(|id| {
                            ids.binary_search_by(|m| m.as_ref().cmp(id))
                                .map_err(|_| Error::InvalidPool)
                        })
                        .collect::<Result<Vec<_>, _>>()?;
                    positions.sort_unstable();
                    if positions.windows(2).any(|pair| pair[0] == pair[1]) {
                        return Err(Error::InvalidPool);
                    }
                    positions
                }
            };
            resolved.push((spec.id, spec.rank, positions));
        }
        resolved.sort_by(|a, b| a.0.cmp(&b.0));
        if resolved.windows(2).any(|pair| pair[0].0 == pair[1].0) {
            return Err(Error::InvalidPool);
        }
        let reusable = old.filter(|old| old.ids == ids && old.sites == sites);
        let mut pools: Vec<Pool> = Vec::with_capacity(resolved.len());
        for (id, rank, positions) in resolved {
            let shared = pools
                .iter()
                .chain(reusable.iter().flat_map(|old| old.pools.iter()))
                .find(|pool| pool.members() == positions.as_slice())
                .map(|pool| Arc::clone(pool.mesh()));
            let mesh =
                shared.unwrap_or_else(|| Arc::new(mesh::Mesh::build::<M>(positions, ids, sites)));
            pools.push(Pool::new(id, rank, mesh));
        }
        Ok(pools)
    }

    /// Record what changed since `old`, so [`Placement`] can update cheaply.
    ///
    /// Only up to [`MAX_INCREMENTAL_CHANGES`] changes are recorded. With more,
    /// nothing is recorded and placement recomputes from scratch. Replaces any
    /// earlier record. Does not keep `old` alive or change the mesh.
    ///
    /// Also continues `old`'s lineage ([`Self::since`]): members that `old`
    /// listed keep their arrival generation, the others arrive now.
    #[must_use]
    pub fn with_predecessor(mut self, old: &Self) -> Self {
        // Identical or over-budget replacements must not retain a stale hint.
        self.delta = None;
        self.inherit_lineage(old);
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

    /// Continue `old`'s lineage: one generation later, members `old` listed
    /// keep their arrival generation and new ones arrive in this one.
    fn inherit_lineage(&mut self, old: &Self) {
        self.generation = old.generation.saturating_add(1);
        let (mut a, mut b) = (0, 0);
        while b < self.ids.len() {
            match old.ids.get(a).map(|id| id.cmp(&self.ids[b])) {
                Some(std::cmp::Ordering::Less) => a += 1,
                Some(std::cmp::Ordering::Equal) => {
                    self.since[b] = old.since[a];
                    a += 1;
                    b += 1;
                }
                _ => {
                    self.since[b] = self.generation;
                    b += 1;
                }
            }
        }
    }

    /// This membership's position in its local chain (see
    /// [`Self::with_predecessor`]).
    #[must_use]
    pub fn generation(&self) -> u32 {
        self.generation
    }

    /// The generation in which the member at `position` arrived in this
    /// chain (0: unknown, present since the chain began). A member that left
    /// and came back arrives again. `None` if the position is out of range.
    #[must_use]
    pub fn since(&self, position: usize) -> Option<u32> {
        self.since.get(position).copied()
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

    /// Hash of the domain, IDs, Sites, and pools (ID, rank, members).
    /// Weights are left out.
    ///
    /// This is the graph identity that route cache keys use
    /// ([`PathKey::topology`]).
    #[must_use]
    pub fn topology_identity(&self) -> [u8; 32] {
        self.topology_identity
    }

    /// The pools, sorted by ID.
    #[must_use]
    pub fn pools(&self) -> &[Pool] {
        &self.pools
    }

    /// The pool with this ID.
    #[must_use]
    pub fn pool(&self, id: &[u8]) -> Option<&Pool> {
        self.pools
            .binary_search_by(|pool| pool.id().cmp(id))
            .ok()
            .map(|index| &self.pools[index])
    }

    /// A shared handle to the pools.
    pub(crate) fn shared_pools(&self) -> Arc<[Pool]> {
        Arc::clone(&self.pools)
    }

    /// Neighbors of a member: the sorted union over its pools. Empty if the
    /// position is out of range.
    #[must_use]
    pub fn neighbors(&self, position: usize) -> Vec<usize> {
        union_neighbors(&self.pools, position)
    }

    /// Number of neighbors of a member.
    #[must_use]
    pub fn degree(&self, position: usize) -> usize {
        self.neighbors(position).len()
    }

    /// Upper bound on any member's degree: the sum of per-pool bounds
    /// ([`Pool::max_degree`]), capped at `members - 1`. Use it to size
    /// per-peer capacity. It can be well above the real maximum when pools
    /// overlap; [`Self::degree`] is exact.
    #[must_use]
    pub fn max_degree(&self) -> usize {
        self.pools
            .iter()
            .map(Pool::max_degree)
            .fold(0usize, usize::saturating_add)
            .min(self.ids.len().saturating_sub(1))
    }

    /// Whether two members share a mesh edge in any pool. Does not allocate:
    /// a few binary searches per pool. False when either position is out of
    /// range or both positions are the same member.
    #[must_use]
    pub fn is_neighbor(&self, position: usize, other: usize) -> bool {
        position != other
            && self
                .pools
                .iter()
                .any(|pool| pool.edge(position, other).is_some())
    }

    /// Best `(pool rank, edge kind)` over pools that have the edge.
    fn edge_rank(&self, a: usize, b: usize) -> Option<(u64, EdgeKind)> {
        self.pools
            .iter()
            .filter_map(|pool| pool.edge(a, b).map(|kind| (pool.rank(), kind)))
            .max()
    }

    /// The member's RDMA peers: its top `budget` neighbors `q` with
    /// `eligible(position, q)`, best first.
    ///
    /// Edges rank by the highest rank of a pool that has the edge, then
    /// column edge before delegate edge, then the symmetric hash
    /// `H("/mesh/v2/edge\0", min id, max id)` (lower first). Both ends compute
    /// the same rank. `eligible` must be symmetric, for example "same
    /// nonempty Site and a shared rail". Production uses [`RDMA_PEERS`].
    #[must_use]
    pub fn rdma_peers(
        &self,
        position: usize,
        budget: usize,
        eligible: impl Fn(usize, usize) -> bool,
    ) -> Vec<usize> {
        let mut ranked: Vec<_> = self
            .neighbors(position)
            .into_iter()
            .filter(|&other| eligible(position, other))
            .filter_map(|other| {
                let (rank, kind) = self.edge_rank(position, other)?;
                let tie = mesh::edge_hash::<M>(self.id(position), self.id(other));
                Some((std::cmp::Reverse(rank), std::cmp::Reverse(kind), tie, other))
            })
            .collect();
        ranked.sort_unstable();
        ranked.truncate(budget);
        ranked.into_iter().map(|entry| entry.3).collect()
    }

    /// Whether the edge uses RDMA: both ends took it in
    /// [`Self::rdma_peers`] with the same `budget` and `eligible`.
    #[must_use]
    pub fn is_rdma_edge(
        &self,
        a: usize,
        b: usize,
        budget: usize,
        eligible: impl Fn(usize, usize) -> bool,
    ) -> bool {
        self.is_neighbor(a, b)
            && eligible(a, b)
            && self.rdma_peers(a, budget, &eligible).contains(&b)
            && self.rdma_peers(b, budget, &eligible).contains(&a)
    }

    /// Estimated heap bytes used by this membership. Fast to call.
    ///
    /// Counts every mesh in full, even when it is shared with another pool
    /// or membership. Does not count heap memory owned by your records.
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
        bytes = bytes.saturating_add(self.sites.capacity().saturating_mul(size_of::<Box<[u8]>>()));
        for (id, site) in self.ids.iter().zip(&self.sites) {
            bytes = bytes.saturating_add(id.len()).saturating_add(site.len());
        }
        bytes = bytes.saturating_add(self.since.capacity().saturating_mul(size_of::<u32>()));
        // Arc of a pool slice: two counters plus the pools.
        bytes = bytes.saturating_add(2 * size_of::<usize>() + self.pools.len() * size_of::<Pool>());
        for pool in self.pools.iter() {
            bytes = bytes
                .saturating_add(pool.id().len())
                .saturating_add(pool.mesh().bytes());
        }
        bytes
    }
}

/// Sorted union of a member's neighbors over `pools`.
pub(crate) fn union_neighbors(pools: &[Pool], position: usize) -> Vec<usize> {
    let mut out = Vec::new();
    for pool in pools {
        pool.extend_neighbors(position, &mut out);
    }
    out.sort_unstable();
    out.dedup();
    out
}

/// Hash the domain, IDs, Sites, and pools (ID, rank, member positions).
fn topology_identity<M: Member>(
    ids: &[Box<[u8]>],
    sites: &[Box<[u8]>],
    pools: &[Pool],
) -> [u8; 32] {
    let mut digest = hash::domain::<M>(TOPOLOGY_IDENTITY_DOMAIN);
    digest.update((ids.len() as u64).to_be_bytes());
    for (id, site) in ids.iter().zip(sites) {
        hash::bytes(&mut digest, id);
        hash::bytes(&mut digest, site);
    }
    digest.update((pools.len() as u64).to_be_bytes());
    for pool in pools {
        hash::bytes(&mut digest, pool.id());
        digest.update(pool.rank().to_be_bytes());
        digest.update((pool.members().len() as u64).to_be_bytes());
        for &position in pool.members() {
            digest.update((position as u64).to_be_bytes());
        }
    }
    hash::finish(digest)
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

    impl<M: Member> Membership<M> {
        /// The default pool's shared mesh.
        fn graph(&self) -> Arc<mesh::Mesh> {
            Arc::clone(self.pools()[0].mesh())
        }

        /// Every member's neighbor list.
        fn adjacency(&self) -> Vec<Vec<usize>> {
            (0..self.members().len())
                .map(|p| self.neighbors(p))
                .collect()
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
        assert_eq!(placement.rank(&members, b"key"), vec![0]);
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
    fn lineage_records_arrival_generations() {
        let since = |m: &Membership<BinaryMember>, id: &[u8]| m.since(m.position(id).unwrap());
        // Without a predecessor every member is as old as the chain.
        let first = Membership::new(vec![member(b"a", 1), member(b"c", 1)]).unwrap();
        assert_eq!(first.generation(), 0);
        assert_eq!(since(&first, b"a"), Some(0));
        assert_eq!(first.since(2), None);
        // `b` arrives in generation 1; `a` and `c` keep 0.
        let second = Membership::new_with_predecessor(
            vec![member(b"a", 1), member(b"b", 1), member(b"c", 1)],
            &first,
        )
        .unwrap();
        assert_eq!(second.generation(), 1);
        assert_eq!(since(&second, b"a"), Some(0));
        assert_eq!(since(&second, b"b"), Some(1));
        assert_eq!(since(&second, b"c"), Some(0));
        // A reweight keeps the arrival; leaving and coming back arrives again.
        let third =
            Membership::new_with_predecessor(vec![member(b"b", 1), member(b"c", 3)], &second)
                .unwrap();
        assert_eq!(since(&third, b"b"), Some(1));
        assert_eq!(since(&third, b"c"), Some(0));
        let fourth = Membership::new_with_predecessor(
            vec![member(b"a", 1), member(b"b", 1), member(b"c", 3)],
            &third,
        )
        .unwrap();
        assert_eq!(fourth.generation(), 3);
        assert_eq!(since(&fourth, b"a"), Some(3));
        // An identical replacement keeps every arrival and advances the chain.
        let same = Membership::new(fourth.members().to_vec())
            .unwrap()
            .with_predecessor(&fourth);
        assert_eq!(same.generation(), 4);
        assert_eq!(since(&same, b"a"), Some(3));
        assert_eq!(since(&same, b"c"), Some(0));
        // More changes than the incremental budget still continue the chain.
        let many: Vec<_> = (0..=MAX_INCREMENTAL_CHANGES as u32 + 1)
            .map(|i| member(&i.to_be_bytes(), 1))
            .chain([member(b"c", 3)])
            .collect();
        let big = Membership::new_with_predecessor(many, &same).unwrap();
        assert!(big.delta.is_none());
        assert_eq!(since(&big, b"c"), Some(0));
        assert_eq!(since(&big, &0u32.to_be_bytes()), Some(5));
    }

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

    /// Placement identity matches a fixed vector; graph hash ignores weights.
    #[test]
    fn placement_identity_matches_vector_and_topology_ignores_weights() {
        let old = Membership::new(vec![member(b"b", 9), member(b"a", 1)]).unwrap();
        let expected: [u8; 32] = sha2::Sha256::digest(
            b"binary-store/placement-identity/v1\0\0\0\0\x01a\0\0\0\x01\0\0\0\x01b\0\0\0\x09",
        )
        .into();
        assert_eq!(old.identity(), expected);
        let changed = Membership::new(vec![member(b"b", u32::MAX), member(b"a", 2)]).unwrap();
        assert_ne!(old.identity(), changed.identity());
        assert_eq!(old.topology_identity(), changed.topology_identity());
        assert_eq!(old.adjacency(), changed.adjacency());
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
            + new.sites.capacity() * size_of::<Box<[u8]>>()
            + new.since.capacity() * size_of::<u32>()
            + new.delta.as_ref().unwrap().changes.capacity() * size_of::<MemberChange>()
            + 2 * size_of::<usize>()
            + size_of::<Pool>()
            + b"default".len()
            + new.graph().bytes();
        assert_eq!(new.retained_bytes(), expected);
        let shared = new.graph();
        assert_eq!(new.retained_bytes(), expected);
        assert!(Arc::ptr_eq(&shared, &new.graph()));
        let empty = Membership::<BinaryMember>::with_pools(vec![], vec![]).unwrap();
        assert_eq!(empty.retained_bytes(), 2 * size_of::<usize>());
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
            changed.adjacency(),
            Membership::new(changed.members().to_vec())
                .unwrap()
                .adjacency()
        );
        assert!(
            Membership::new_with_predecessor(vec![member(b"a", 1), member(b"a", 2)], &old).is_err()
        );
    }

    /// Pool meshes are shared within a membership and with a predecessor
    /// only when IDs, Sites, and pool members match.
    #[test]
    fn pool_meshes_shared_only_for_equal_ids_sites_and_members() {
        /// Member with a Site.
        #[derive(Clone)]
        struct Sited(Vec<u8>, Vec<u8>);

        impl Member for Sited {
            const DOMAIN: &'static str = "sited";

            /// Return the ID.
            fn id(&self) -> &[u8] {
                &self.0
            }

            /// Weight 1.
            fn weight(&self) -> NonZeroU32 {
                NonZeroU32::new(1).unwrap()
            }

            /// Return the Site.
            fn site(&self) -> &[u8] {
                &self.1
            }
        }
        let input = |site: &[u8]| -> Vec<Sited> {
            (0..100u32)
                .map(|i| Sited(i.to_be_bytes().to_vec(), site.to_vec()))
                .collect()
        };
        let half: Vec<Box<[u8]>> = (0..50u32).map(|i| i.to_be_bytes().into()).collect();
        let pools = || {
            vec![
                PoolSpec::default_pool(),
                PoolSpec {
                    id: (*b"a").into(),
                    rank: 1,
                    members: Some(half.clone()),
                },
                PoolSpec {
                    id: (*b"b").into(),
                    rank: 2,
                    members: Some(half.clone()),
                },
            ]
        };
        let old = Membership::with_pools(input(b"x"), pools()).unwrap();
        let (a, b, default) = (&old.pools()[0], &old.pools()[1], &old.pools()[2]);
        assert_eq!(default.id(), b"default");
        assert!(Arc::ptr_eq(a.mesh(), b.mesh()));
        assert!(!Arc::ptr_eq(a.mesh(), default.mesh()));
        let same = Membership::with_pools_and_predecessor(input(b"x"), pools(), &old).unwrap();
        for (x, y) in old.pools().iter().zip(same.pools()) {
            assert!(Arc::ptr_eq(x.mesh(), y.mesh()));
        }
        assert_eq!(same.generation(), 1);
        let moved = Membership::with_pools_and_predecessor(input(b"y"), pools(), &old).unwrap();
        for (x, y) in old.pools().iter().zip(moved.pools()) {
            assert!(!Arc::ptr_eq(x.mesh(), y.mesh()));
        }
        assert_ne!(old.topology_identity(), moved.topology_identity());
        assert_eq!(old.identity(), moved.identity());
        assert_eq!(Error::InvalidPool.to_string(), "invalid pool");
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
