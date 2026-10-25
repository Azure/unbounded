//! Candidate ordering, peer response classification, and origin authority rules.

use super::error::{Error, Result};
use racer_control_wire::NodeId;
use racer_object_wire::model::{ObjectId, PageNumber};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProbeOutcome {
    CopyMiss,
    Unreachable,
    Overloaded,
    VersionUnavailable,
    /// An authenticated response carried a copy that could not be used locally.
    UnusableCopy,
}
/// How a non-owner requester orders its remote attempts across a page's
/// ranked owners. Placement, rank, and origin authority are unchanged: this only
/// chooses which owner receives the first Acquire.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum ReplicaSelection {
    /// Visit owners in canonical rank order, so every remote read of a page
    /// lands on the primary owner first.
    Primary,
    /// Visit owners that are direct overlay neighbors before owners that need
    /// a relay hop. Among equally preferred owners, a rendezvous hash of the
    /// requester and page picks a stable order, so one requester keeps using
    /// the same replica for a page while different requesters spread the
    /// load across all replicas.
    #[default]
    Spread,
}

/// Canonical ranks in the order a non-owner requester visits them. At most
/// three owners exist; `ordered` itself stays the authoritative rank order.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct VisitOrder {
    ranks: [u8; 3],
    len: u8,
}

impl VisitOrder {
    /// The identity order over the first `len` (at most three) ranks.
    pub fn canonical(len: usize) -> Self {
        Self {
            ranks: [0, 1, 2],
            len: len.min(3) as u8,
        }
    }

    /// Canonical ranks in visiting order.
    pub fn ranks(&self) -> impl ExactSizeIterator<Item = usize> + '_ {
        self.ranks[..usize::from(self.len)]
            .iter()
            .map(|rank| usize::from(*rank))
    }

    /// The canonical rank visited at `index`, if any.
    pub fn get(&self, index: usize) -> Option<usize> {
        self.ranks[..usize::from(self.len)]
            .get(index)
            .map(|rank| usize::from(*rank))
    }
}

/// Order `len` canonical ranks for a non-owner requester. `Primary` returns the
/// canonical order. `Spread` visits ranks for which `neighbor` holds first, then
/// orders each group by descending `score` (ties keep canonical order).
pub fn visit_order(
    selection: ReplicaSelection,
    len: usize,
    neighbor: impl Fn(usize) -> bool,
    score: impl Fn(usize) -> u64,
) -> VisitOrder {
    let mut order = VisitOrder::canonical(len);
    if selection == ReplicaSelection::Primary || order.len < 2 {
        return order;
    }
    let mut keys = [(false, 0u64); 3];
    for (rank, key) in keys.iter_mut().enumerate().take(usize::from(order.len)) {
        *key = (!neighbor(rank), u64::MAX - score(rank));
    }
    order.ranks[..usize::from(order.len)].sort_by_key(|rank| (keys[usize::from(*rank)], *rank));
    order
}

/// Known issue 25: when the owner a non-owner would visit first arrived after
/// a later owner (`since`, see [`super::walk::lineage_targets`]), visit that
/// later owner first. Placement is rendezvous hashing, so the later owner
/// was above the first one before it arrived and may hold a copy fetched
/// then; asking the newer owner first made it fetch the page from origin
/// again, and its own copy-only probes of the older owner carry only the
/// credits and links the reader forwarded (too few to route to a
/// non-neighbor). The rest of the order is unchanged. Members with equal or
/// unknown arrival never move, so a cluster whose members arrived together
/// visits exactly as before.
///
/// A non-neighbor never moves ahead of a direct `neighbor` visited first: its
/// funded route spends most of the reader's links, so when it misses (for
/// example it defers the reader to a successor, Known issue 21) the reader can
/// no longer route to a non-neighbor owner after it (found by the DST oracle,
/// seed 16, at the wave 4 merge).
pub fn lineage_first(
    mut order: VisitOrder,
    since: &[Option<u32>],
    neighbor: impl Fn(usize) -> bool,
) -> VisitOrder {
    let Some(first) = order.get(0) else {
        return order;
    };
    let Some(Some(newest)) = since.get(first).copied() else {
        return order;
    };
    let first_neighbor = neighbor(first);
    let older = (1..usize::from(order.len)).find(|&i| {
        let rank = usize::from(order.ranks[i]);
        rank > first
            && (!first_neighbor || neighbor(rank))
            && since
                .get(rank)
                .copied()
                .flatten()
                .is_some_and(|s| s < newest)
    });
    if let Some(i) = older {
        order.ranks[..=i].rotate_right(1);
    }
    order
}

/// What this node last learned about an owner's answers (see
/// [`crate::peer::silence::Silence`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq, PartialOrd, Ord)]
pub enum Answering {
    /// Nothing against it.
    Answers,
    /// It left one of this node's requests unanswered and has answered
    /// nothing since ([`crate::peer::silence::Silence::unanswered`]).
    Unanswered,
    /// It left a ping unanswered: requests for it fail at once
    /// ([`crate::peer::silence::Silence::silent`]).
    Silent,
}

/// Visit owners that recently left one of this node's requests unanswered
/// after the others, and silent owners last, keeping the order within each
/// group (Known issue 41). A reader's out-of-order `Acquire` carries most of
/// its attempt's credits, so an owner that hangs costs the whole attempt;
/// without this, every retry of the read visited the same hung owner first
/// (`Spread` is deterministic per requester and page) and the read failed
/// with healthy owners left. A reader pings the page's rank 0 when its probe
/// of rank 0 goes unanswered, so a busy rank 0 (whose acquisition the probe
/// joined) answers the ping and keeps its place, while a hung one becomes
/// silent and goes last: the owners above it, which also find it silent,
/// fetch the page.
pub fn demote_unanswered(order: VisitOrder, answering: impl Fn(usize) -> Answering) -> VisitOrder {
    let len = usize::from(order.len);
    let mut ranks = [0u8; 3];
    let mut n = 0;
    for pass in [Answering::Answers, Answering::Unanswered, Answering::Silent] {
        for &rank in &order.ranks[..len] {
            if answering(usize::from(rank)) == pass {
                ranks[n] = rank;
                n += 1;
            }
        }
    }
    VisitOrder {
        ranks,
        len: order.len,
    }
}

/// Rendezvous weight of one owner for one requester and page. Stable within a
/// process; it only spreads load, so it never needs to agree across nodes.
pub fn replica_score(
    requester: &NodeId,
    object: &ObjectId,
    page: PageNumber,
    owner: &NodeId,
) -> u64 {
    use std::hash::Hash;
    use std::hash::Hasher;
    let mut hasher = std::hash::DefaultHasher::new();
    requester.0.hash(&mut hasher);
    object.cache.0.hash(&mut hasher);
    object.key.0.hash(&mut hasher);
    page.0.hash(&mut hasher);
    owner.0.hash(&mut hasher);
    hasher.finish()
}

/// Payload-free shape of a peer operation, as far as classification cares.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum OperationKind {
    Subscribe,
    Page,
    /// `fresh` is true for `MetadataSelector::Fresh`.
    Metadata {
        fresh: bool,
    },
    Bootstrap,
    Warm,
}

/// Payload-free shape of a peer response.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ResponseKind {
    Selected,
    Bootstrap,
    Page,
    Metadata,
    Miss,
    NotFound,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    OriginRejected,
    OriginForbidden,
    StaleMembership,
    Held,
    Warmed,
    Released,
}

impl ResponseKind {
    /// Whether this response carries a page or metadata payload.
    pub fn has_payload(self) -> bool {
        matches!(
            self,
            Self::Selected | Self::Bootstrap | Self::Page | Self::Metadata
        )
    }

    /// The operation a payload response must answer, if any.
    fn answers(self, operation: OperationKind) -> bool {
        matches!(
            (self, operation),
            (Self::Selected, OperationKind::Subscribe)
                | (Self::Bootstrap, OperationKind::Bootstrap)
                | (Self::Page, OperationKind::Page)
                | (Self::Metadata, OperationKind::Metadata { .. })
        )
    }
}

/// Classify one authenticated candidate response.
///
/// `acquire` describes the actual probe, which may be CopyOnly even when the
/// caller's operation requests acquisition (predecessors and remaining copies).
/// Payload responses that answer the operation's kind call `validate`, which
/// checks payload identity and content and returns `Ok(())` for a usable copy;
/// mismatched kinds are `CorruptRecord` without calling it. `Ok(None)` means a
/// usable payload; `Ok(Some(outcome))` is a recoverable probe outcome.
pub fn classify<E: From<Error>>(
    response: ResponseKind,
    operation: OperationKind,
    acquire: bool,
    validate: impl FnOnce() -> Result<(), E>,
) -> Result<Option<ProbeOutcome>, E> {
    let outcome = match response {
        kind if kind.has_payload() => {
            if !kind.answers(operation) {
                return Err(Error::CorruptRecord.into());
            }
            return validate().map(|()| None);
        }
        ResponseKind::Miss => ProbeOutcome::CopyMiss,
        ResponseKind::NotFound => {
            return Err(match operation {
                OperationKind::Bootstrap | OperationKind::Metadata { fresh: true } if acquire => {
                    Error::NotFound
                }
                _ => Error::Unauthorized,
            }
            .into());
        }
        ResponseKind::VersionUnavailable => ProbeOutcome::VersionUnavailable,
        ResponseKind::Unavailable => ProbeOutcome::Unreachable,
        ResponseKind::Overloaded => ProbeOutcome::Overloaded,
        ResponseKind::OriginRejected => return Err(Error::OriginRejected.into()),
        ResponseKind::OriginForbidden => return Err(Error::OriginForbidden.into()),
        ResponseKind::StaleMembership => return Err(Error::IncompatibleMembership.into()),
        // Warm acknowledgements never answer a candidate data probe.
        _ => return Err(Error::CorruptRecord.into()),
    };
    Ok(Some(outcome))
}

/// Map a payload validation failure to a recoverable probe outcome. Only page
/// content validation is recoverable; transport and security errors never reach
/// here, and `Unauthorized` remains terminal.
pub fn unusable_copy(error: Error, operation: OperationKind) -> Option<ProbeOutcome> {
    (matches!(error, Error::CorruptRecord | Error::MissingKey)
        && matches!(operation, OperationKind::Page | OperationKind::Bootstrap))
    .then_some(ProbeOutcome::UnusableCopy)
}

/// Error after every ranked owner missed: any unreachable, overloaded, or
/// unusable predecessor makes the miss transient rather than authoritative.
pub fn origin_miss_error(predecessor_evidence: &[ProbeOutcome]) -> Error {
    if predecessor_evidence.iter().any(|outcome| {
        matches!(
            outcome,
            ProbeOutcome::Unreachable | ProbeOutcome::Overloaded | ProbeOutcome::UnusableCopy
        )
    }) {
        Error::Unavailable
    } else {
        Error::VersionUnavailable
    }
}

/// Facts an origin authority was minted from.
#[derive(Clone, Copy, Debug)]
pub struct AuthorityFacts<'a> {
    pub object: &'a ObjectId,
    pub page: PageNumber,
    pub rank: usize,
    pub predecessor_evidence: usize,
    /// Whether the authority's node is still a member of its membership.
    pub member: bool,
}

/// Validate an origin authority for `object`/`page`. Private construction
/// follows a recomputed ranking under an immutable membership, so validation
/// need not rehash the entire cluster on each write.
pub fn validate_origin_authority(
    facts: AuthorityFacts<'_>,
    object: &ObjectId,
    page: PageNumber,
) -> Result<()> {
    if object != facts.object || page != facts.page {
        return Err(Error::Unauthorized);
    }
    if facts.rank >= 3 || facts.predecessor_evidence != facts.rank || !facts.member {
        return Err(Error::Unauthorized);
    }
    Ok(())
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use racer_control_wire::CacheId;
    use racer_object_wire::model::CacheKey;

    fn ranks(order: VisitOrder) -> Vec<usize> {
        order.ranks().collect()
    }

    /// Known issue 41: owners that left this node's request unanswered go
    /// after the others, silent owners last, each group in its previous
    /// order; nothing changes without them.
    #[test]
    fn demote_unanswered_moves_unanswered_then_silent_owners_last() {
        use Answering::*;
        let spread = visit_order(ReplicaSelection::Spread, 3, |r| r == 2, |r| 3 - r as u64);
        assert_eq!(ranks(spread), [2, 0, 1]);
        let with = |states: [Answering; 3]| ranks(demote_unanswered(spread, |r| states[r]));
        assert_eq!(with([Answers; 3]), [2, 0, 1]);
        assert_eq!(with([Answers, Answers, Unanswered]), [0, 1, 2]);
        assert_eq!(with([Unanswered, Answers, Unanswered]), [1, 2, 0]);
        assert_eq!(with([Unanswered; 3]), [2, 0, 1]);
        assert_eq!(with([Silent, Unanswered, Unanswered]), [2, 1, 0]);
        assert_eq!(with([Silent, Answers, Unanswered]), [1, 2, 0]);
        let short = VisitOrder::canonical(2);
        assert_eq!(
            ranks(demote_unanswered(short, |r| if r == 0 {
                Silent
            } else {
                Answers
            })),
            [1, 0]
        );
        assert_eq!(
            ranks(demote_unanswered(VisitOrder::canonical(0), |_| Silent)),
            [0usize; 0]
        );
    }

    #[test]
    fn lineage_first_visits_an_older_later_owner_first() {
        let order = |first: usize| {
            visit_order(
                ReplicaSelection::Spread,
                3,
                |r| r == first,
                |r| 3 - r as u64,
            )
        };
        // Every owner a neighbor: arrival alone decides.
        let all = |_: usize| true;
        assert_eq!(ranks(order(0)), [0, 1, 2]);
        // Rank 0 arrived after rank 2: visit rank 2 first, then as before.
        let since = [Some(4), Some(4), Some(1)];
        assert_eq!(ranks(lineage_first(order(0), &since, all)), [2, 0, 1]);
        // The first older one in visit order.
        let since = [Some(4), Some(2), Some(1)];
        assert_eq!(ranks(lineage_first(order(0), &since, all)), [1, 0, 2]);
        // Visiting rank 1 first: only ranks after it can be older.
        assert_eq!(ranks(order(1)), [1, 0, 2]);
        assert_eq!(
            ranks(lineage_first(order(1), &[Some(0), Some(3), Some(5)], all)),
            [1, 0, 2]
        );
        assert_eq!(
            ranks(lineage_first(order(1), &[Some(0), Some(3), Some(1)], all)),
            [2, 1, 0]
        );
        // Equal, unknown or newer arrivals keep the order.
        for since in [
            [Some(0); 3],
            [None, Some(0), Some(0)],
            [Some(1), Some(2), None],
        ] {
            assert_eq!(ranks(lineage_first(order(0), &since, all)), [0, 1, 2]);
        }
        // Short orders.
        assert_eq!(
            ranks(lineage_first(VisitOrder::canonical(1), &[Some(3)], all)),
            [0]
        );
        assert_eq!(
            ranks(lineage_first(VisitOrder::canonical(0), &[], all)),
            Vec::<usize>::new()
        );
        assert_eq!(
            ranks(lineage_first(
                VisitOrder::canonical(2),
                &[Some(3), Some(1)],
                all
            )),
            [1, 0]
        );
    }

    /// Wave 4 merge (DST oracle seed 16): an older later owner that is not a
    /// neighbor never moves ahead of a neighbor visited first; one that is a
    /// neighbor still does, and with a non-neighbor first any older one may.
    #[test]
    fn lineage_first_keeps_a_neighbor_ahead_of_a_non_neighbor() {
        let order = |first: usize| {
            visit_order(
                ReplicaSelection::Spread,
                3,
                |r| r == first,
                |r| 3 - r as u64,
            )
        };
        let since = [Some(4), Some(4), Some(1)];
        // Only rank 0 is a neighbor: rank 2 stays behind it.
        assert_eq!(
            ranks(lineage_first(order(0), &since, |r| r == 0)),
            [0, 1, 2]
        );
        // Rank 2 is a neighbor too: it moves first.
        assert_eq!(
            ranks(lineage_first(order(0), &since, |r| r != 1)),
            [2, 0, 1]
        );
        // The first older neighbor is chosen, skipping a non-neighbor.
        let since = [Some(4), Some(2), Some(1)];
        assert_eq!(
            ranks(lineage_first(order(0), &since, |r| r != 1)),
            [2, 0, 1]
        );
        // No neighbor first (all relayed): any older one may move.
        assert_eq!(ranks(lineage_first(order(0), &since, |_| false)), [1, 0, 2]);
    }

    fn object(key: u8) -> ObjectId {
        ObjectId {
            cache: CacheId("c".into()),
            key: CacheKey([key; 32]),
        }
    }

    #[test]
    fn classify_outcomes_and_errors() {
        use OperationKind as O;
        use ResponseKind as R;
        let ok = || Ok::<(), Error>(());
        assert_eq!(
            classify(R::Miss, O::Page, true, ok),
            Ok(Some(ProbeOutcome::CopyMiss))
        );
        assert_eq!(
            classify(R::Unavailable, O::Page, true, ok),
            Ok(Some(ProbeOutcome::Unreachable))
        );
        assert_eq!(
            classify(R::Overloaded, O::Page, true, ok),
            Ok(Some(ProbeOutcome::Overloaded))
        );
        assert_eq!(
            classify(R::VersionUnavailable, O::Page, true, ok),
            Ok(Some(ProbeOutcome::VersionUnavailable))
        );
        assert_eq!(classify(R::Page, O::Page, true, ok), Ok(None));
        assert_eq!(classify(R::Selected, O::Subscribe, false, ok), Ok(None));
        assert_eq!(
            classify(R::Metadata, O::Metadata { fresh: false }, false, ok),
            Ok(None)
        );
        assert_eq!(classify(R::Bootstrap, O::Bootstrap, false, ok), Ok(None));
        assert_eq!(
            classify(R::Page, O::Bootstrap, true, ok),
            Err(Error::CorruptRecord)
        );
        assert_eq!(
            classify(R::Page, O::Page, true, || Err(Error::MissingKey)),
            Err(Error::MissingKey)
        );
        assert_eq!(
            classify(R::NotFound, O::Bootstrap, true, ok),
            Err(Error::NotFound)
        );
        assert_eq!(
            classify(R::NotFound, O::Bootstrap, false, ok),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            classify(R::NotFound, O::Metadata { fresh: true }, true, ok),
            Err(Error::NotFound)
        );
        assert_eq!(
            classify(R::NotFound, O::Metadata { fresh: false }, true, ok),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            classify(R::NotFound, O::Page, true, ok),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            classify(R::OriginRejected, O::Page, true, ok),
            Err(Error::OriginRejected)
        );
        assert_eq!(
            classify(R::OriginForbidden, O::Page, true, ok),
            Err(Error::OriginForbidden)
        );
        assert_eq!(
            classify(R::StaleMembership, O::Page, true, ok),
            Err(Error::IncompatibleMembership)
        );
        for warm in [R::Held, R::Warmed, R::Released] {
            assert_eq!(classify(warm, O::Warm, true, ok), Err(Error::CorruptRecord));
        }
    }

    #[test]
    fn unusable_copy_only_for_page_content() {
        assert_eq!(
            unusable_copy(Error::CorruptRecord, OperationKind::Page),
            Some(ProbeOutcome::UnusableCopy)
        );
        assert_eq!(
            unusable_copy(Error::MissingKey, OperationKind::Bootstrap),
            Some(ProbeOutcome::UnusableCopy)
        );
        assert_eq!(
            unusable_copy(Error::Unauthorized, OperationKind::Page),
            None
        );
        assert_eq!(
            unusable_copy(Error::CorruptRecord, OperationKind::Subscribe),
            None
        );
    }

    #[test]
    fn origin_miss_error_requires_clean_misses() {
        assert_eq!(origin_miss_error(&[]), Error::VersionUnavailable);
        assert_eq!(
            origin_miss_error(&[ProbeOutcome::CopyMiss, ProbeOutcome::VersionUnavailable]),
            Error::VersionUnavailable
        );
        for transient in [
            ProbeOutcome::Unreachable,
            ProbeOutcome::Overloaded,
            ProbeOutcome::UnusableCopy,
        ] {
            assert_eq!(
                origin_miss_error(&[ProbeOutcome::CopyMiss, transient]),
                Error::Unavailable
            );
        }
    }

    #[test]
    fn authority_validation() {
        let a = object(1);
        let facts = |rank, evidence, member| AuthorityFacts {
            object: &a,
            page: PageNumber(3),
            rank,
            predecessor_evidence: evidence,
            member,
        };
        assert_eq!(
            validate_origin_authority(facts(1, 1, true), &a, PageNumber(3)),
            Ok(())
        );
        assert_eq!(
            validate_origin_authority(facts(1, 1, true), &object(2), PageNumber(3)),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            validate_origin_authority(facts(1, 1, true), &a, PageNumber(4)),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            validate_origin_authority(facts(3, 3, true), &a, PageNumber(3)),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            validate_origin_authority(facts(1, 0, true), &a, PageNumber(3)),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            validate_origin_authority(facts(0, 0, false), &a, PageNumber(3)),
            Err(Error::Unauthorized)
        );
    }

    #[test]
    fn visit_order_primary_and_spread() {
        assert_eq!(
            visit_order(ReplicaSelection::Primary, 3, |_| false, |r| r as u64)
                .ranks()
                .collect::<Vec<_>>(),
            [0, 1, 2]
        );
        let order = visit_order(ReplicaSelection::Spread, 3, |r| r == 2, |r| r as u64);
        assert_eq!(order.ranks().collect::<Vec<_>>(), [2, 1, 0]);
        assert_eq!(order.get(1), Some(1));
        assert_eq!(order.get(3), None);
        assert_eq!(VisitOrder::canonical(9).ranks().len(), 3);
        let requester = NodeId("r".into());
        let owner = NodeId("o".into());
        assert_eq!(
            replica_score(&requester, &object(1), PageNumber(0), &owner),
            replica_score(&requester, &object(1), PageNumber(0), &owner)
        );
    }
}
