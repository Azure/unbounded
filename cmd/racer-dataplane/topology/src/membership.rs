use crate::{Error, Member, hash};
use sha2::Digest;
use std::{mem::size_of, num::NonZeroU32, sync::Arc};

/// Maximum changed IDs retained for incremental computation.
pub const MAX_INCREMENTAL_CHANGES: usize = 64;

/// Immutable ID-sorted members and bounded predecessor hints.
///
/// IDs and weights are snapshotted at construction. The original records remain
/// accessible for application metadata, but subsequent interior mutation cannot
/// change placement or topology inputs. Membership is `Send` and `Sync` when `M`
/// is, even though routing and placement caches remain worker-local.
#[derive(Debug)]
pub struct Membership<M: Member> {
    members: Vec<M>,
    ids: Vec<Box<[u8]>>,
    weights: Vec<NonZeroU32>,
    identity: [u8; 32],
    topology_identity: [u8; 32],
    graph: Arc<Vec<Vec<usize>>>,
    owned_bytes: usize,
    pub(crate) delta: Option<MembershipDelta>,
}

#[cfg(test)]
mod tests;

#[derive(Debug)]
pub(crate) struct MembershipDelta {
    pub base: [u8; 32],
    pub old_count: usize,
    pub changes: Vec<(Option<usize>, Option<usize>)>,
}

fn validate_id_length(length: usize) -> Result<(), Error> {
    u32::try_from(length)
        .map(|_| ())
        .map_err(|_| Error::InvalidMember)
}

impl<M: Member> Membership<M> {
    /// Freeze member IDs and weights, sort by ID, and construct ring adjacency.
    ///
    /// Each member accessor is read once. Overlay construction hashes each ID
    /// 32 times and sorts 32 rings: O(32 * (total ID bytes + N log N)) time and
    /// O(64 N) adjacency storage, with O(N) temporary ring sorting storage.
    ///
    /// # Errors
    ///
    /// Returns [`Error::InvalidDomain`] for NUL-containing domains,
    /// [`Error::DuplicateMember`] for duplicate frozen IDs, or
    /// [`Error::InvalidMember`] for an ID longer than `u32::MAX` bytes, which
    /// cannot be encoded in the placement identity v1 hash schema.
    ///
    /// # Panics
    ///
    /// Propagates panics from application-provided [`Member`] accessors.
    pub fn new(members: Vec<M>) -> Result<Self, Error> {
        Self::build(members, None)
    }

    /// Freeze members and prepare incremental hints, reusing the predecessor's
    /// immutable graph when the frozen IDs are identical. Weight and metadata
    /// changes do not rebuild the rings. Changed IDs use the same construction
    /// as [`Self::new`]. Does not retain the predecessor membership itself.
    ///
    /// # Errors
    /// Returns the same validation errors as [`Self::new`].
    ///
    /// # Panics
    /// Propagates panics from application-provided [`Member`] accessors.
    pub fn new_with_predecessor(members: Vec<M>, old: &Self) -> Result<Self, Error> {
        Ok(Self::build(members, Some(old))?.with_predecessor(old))
    }

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
        let topology_identity = crate::overlay::identity::<M>(&ids);
        let graph = match old.filter(|old| old.ids == ids) {
            Some(old) => Arc::clone(&old.graph),
            None => Arc::new(crate::overlay::build::<M>(&ids)),
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

    /// Prepare bounded incremental ranking hints outside request processing.
    /// Larger changes use exact cooperative cold computation on demand.
    /// Replaces any previous hint, retaining at most [`MAX_INCREMENTAL_CHANGES`]
    /// changed IDs. Does not retain the predecessor itself.
    #[must_use]
    pub fn with_predecessor(mut self, old: &Self) -> Self {
        // Replacing the predecessor must not retain a stale hint, even when the
        // new predecessor is identical or exceeds the bounded diff budget.
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
                    changes.push((Some(a), None));
                    a += 1;
                }
                std::cmp::Ordering::Greater => {
                    changes.push((None, Some(b)));
                    b += 1;
                }
                std::cmp::Ordering::Equal => {
                    if old.weight(a) != self.weight(b) {
                        changes.push((Some(a), Some(b)));
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

    /// Original application records, ordered by their frozen IDs.
    /// Interior mutation of these records does not alter algorithm snapshots.
    #[must_use]
    pub fn members(&self) -> &[M] {
        &self.members
    }

    /// Find the position of a frozen ID, or `None` if it is absent.
    #[must_use]
    pub fn position(&self, id: &[u8]) -> Option<usize> {
        self.ids.binary_search_by(|m| m.as_ref().cmp(id)).ok()
    }

    /// Placement identity v1 over domain, frozen IDs, and frozen weights only,
    /// independent of application metadata and the overlay algorithm.
    #[must_use]
    pub fn identity(&self) -> [u8; 32] {
        self.identity
    }

    /// Frozen ID at a valid membership position.
    ///
    /// # Panics
    /// Panics if `position` is outside the member slice.
    #[must_use]
    pub(crate) fn id(&self, position: usize) -> &[u8] {
        &self.ids[position]
    }

    /// Frozen weight at a valid membership position.
    ///
    /// # Panics
    /// Panics if `position` is outside the member slice.
    #[must_use]
    pub(crate) fn weight(&self, position: usize) -> NonZeroU32 {
        self.weights[position]
    }

    /// Domain, frozen IDs, and overlay algorithm, independent of weights.
    #[must_use]
    pub(crate) fn topology_identity(&self) -> [u8; 32] {
        self.topology_identity
    }

    /// Sorted, unique, symmetric ID-based ring neighbors. Invalid positions,
    /// including every position in an empty membership, return an empty list.
    #[must_use]
    pub fn neighbors(&self, position: usize) -> Vec<usize> {
        self.neighbor_slice(position).to_vec()
    }

    /// Borrow sorted adjacency without allocating; invalid positions return empty.
    #[must_use]
    pub(crate) fn neighbor_slice(&self, position: usize) -> &[usize] {
        self.graph.get(position).map_or(&[], Vec::as_slice)
    }

    /// Share immutable adjacency with a cooperative search without copying it.
    #[must_use]
    pub(crate) fn graph(&self) -> Arc<Vec<Vec<usize>>> {
        Arc::clone(&self.graph)
    }

    /// Estimated allocated storage owned by this membership, using capacities
    /// rather than lengths. Includes the member buffer, frozen inputs, delta,
    /// and the full shared overlay allocation (count it only once if shared).
    /// Excludes this inline object, allocator bookkeeping/alignment overhead,
    /// and heap allocations inside application-owned `M` values. Applications
    /// should add only those nested allocations, not another member buffer.
    /// The estimate saturates at `usize::MAX` rather than overflowing. This is
    /// O(1): immutable storage is measured once at construction; only the bounded
    /// delta buffer's capacity is consulted on each call.
    #[must_use]
    pub fn retained_bytes(&self) -> usize {
        self.owned_bytes
            .saturating_add(self.delta.as_ref().map_or(0, |delta| {
                delta
                    .changes
                    .capacity()
                    .saturating_mul(size_of::<(Option<usize>, Option<usize>)>())
            }))
    }

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
        // Arc allocation contains the Vec header and two atomic reference counters.
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
