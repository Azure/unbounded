//! Sans-IO read-path decisions: acquisition budgets, candidate ordering and
//! response classification, fill/serve policy, and the candidate walk.
//!
//! The dataplane re-exports these from their historical paths under
//! `racer_dataplane::read`, so there is one implementation of each rule.

use crate::read::walk::RemainingEnd;
pub use budget::AcquisitionBudget;
use racer_control_wire::NodeId;
use racer_object_wire::model::{
    MetadataSelector, ObjectId, ObjectMetadata, PageNumber, StrongEtag, WorkerId,
};
use sha2::{Digest, Sha256};
use std::collections::BTreeSet;
use std::ops::Range;
use std::time::{Duration, Instant, SystemTime};

pub mod budget;
pub mod flight;
pub mod hints;
pub mod walk;

// Read-path decision errors.
//
// A subset of the dataplane's boundary errors. The dataplane converts in both
// directions (`From<racer_core::read::Error>` and a fallible reverse), so core
// decisions never widen what the dataplane can observe.

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

// Local worker selection for objects and pages.

/// Canonical local worker set. Assignment is independent of discovery order;
/// changing the set requires draining all flights first.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerMap {
    workers: Vec<WorkerId>,
}

impl WorkerMap {
    /// Reject empty and duplicate worker sets.
    pub fn new(mut workers: Vec<WorkerId>) -> Result<Self> {
        workers.sort_unstable_by_key(|worker| worker.0);
        if workers.is_empty() || workers.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { workers })
    }

    /// Canonical (sorted) workers.
    pub fn workers(&self) -> &[WorkerId] {
        &self.workers
    }

    /// Owner of `page` of `object`; page zero also owns metadata.
    pub fn select(&self, object: &ObjectId, page: u64) -> Result<WorkerId> {
        let mut hash = Sha256::new();
        hash.update(b"racer.local-worker.v1\0");
        let length = u64::try_from(object.cache.0.len()).map_err(|_| Error::InvalidRequest)?;
        hash.update(length.to_be_bytes());
        hash.update(object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.to_be_bytes());
        let digest = hash.finalize();
        Ok(self.select_digest(&digest.into()))
    }

    /// Map a precomputed digest onto a worker.
    pub fn select_digest(&self, digest: &[u8; 32]) -> WorkerId {
        let index = u64::from_be_bytes(digest[..8].try_into().expect("eight digest bytes"))
            % u64::try_from(self.workers.len()).expect("worker count fits u64");
        self.workers[index as usize]
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod worker_tests {
    use super::*;
    use racer_control_wire::CacheId;
    use racer_object_wire::model::CacheKey;

    #[test]
    fn rejects_empty_and_duplicates_and_is_order_independent() {
        assert_eq!(WorkerMap::new(vec![]), Err(Error::InvalidConfiguration));
        assert_eq!(
            WorkerMap::new(vec![WorkerId(1), WorkerId(1)]),
            Err(Error::InvalidConfiguration)
        );
        let a = WorkerMap::new(vec![WorkerId(2), WorkerId(1)]).unwrap();
        let b = WorkerMap::new(vec![WorkerId(1), WorkerId(2)]).unwrap();
        let object = ObjectId {
            cache: CacheId("c".into()),
            key: CacheKey([7; 32]),
        };
        let mut seen = [false; 2];
        for page in 0..64 {
            let worker = a.select(&object, page).unwrap();
            assert_eq!(worker, b.select(&object, page).unwrap());
            seen[usize::from(worker.0 - 1)] = true;
        }
        assert_eq!(seen, [true, true]);
    }
}

// Peer responses generic over their page payload.
//
// The dataplane instantiates [`PeerResponse`] with real ciphertext pages and
// transfer grants.

/// Unsigned local result. Transport must sign it against the admitted request.
pub enum PeerResponse<P, G> {
    Selected {
        metadata: ObjectMetadata,
        ciphertext: P,
        grant: G,
    },
    Bootstrap {
        metadata: ObjectMetadata,
        page_zero: Option<P>,
    },
    Page {
        metadata: ObjectMetadata,
        ciphertext: P,
    },
    Metadata(ObjectMetadata),
    Miss,
    /// Authoritative origin absence, only for fresh metadata Acquire.
    NotFound,
    VersionUnavailable,
    Unavailable,
    Overloaded,
    OriginRejected,
    OriginForbidden,
    /// Authenticated peer no longer retains the requested routing epoch.
    StaleMembership,
    /// Warm holds accepted until this wall-clock time (millisecond precision).
    Held {
        until: SystemTime,
    },
    /// The requested warm page is published on the owner's disk under its hold.
    Warmed,
    /// The warm lease is gone. Unused capacity is freed after the last lease.
    Released,
}

impl<P, G> PeerResponse<P, G> {
    /// Payload-free shape used by racer-core decisions.
    pub fn kind(&self) -> ResponseKind {
        match self {
            Self::Selected { .. } => ResponseKind::Selected,
            Self::Bootstrap { .. } => ResponseKind::Bootstrap,
            Self::Page { .. } => ResponseKind::Page,
            Self::Metadata(_) => ResponseKind::Metadata,
            Self::Miss => ResponseKind::Miss,
            Self::NotFound => ResponseKind::NotFound,
            Self::VersionUnavailable => ResponseKind::VersionUnavailable,
            Self::Unavailable => ResponseKind::Unavailable,
            Self::Overloaded => ResponseKind::Overloaded,
            Self::OriginRejected => ResponseKind::OriginRejected,
            Self::OriginForbidden => ResponseKind::OriginForbidden,
            Self::StaleMembership => ResponseKind::StaleMembership,
            Self::Held { .. } => ResponseKind::Held,
            Self::Warmed => ResponseKind::Warmed,
            Self::Released => ResponseKind::Released,
        }
    }
}

#[cfg(test)]
mod response_tests {
    use super::*;

    #[test]
    fn kind_matches_variant() {
        let miss: PeerResponse<(), ()> = PeerResponse::Miss;
        assert_eq!(miss.kind(), ResponseKind::Miss);
        let held: PeerResponse<(), ()> = PeerResponse::Held {
            until: SystemTime::UNIX_EPOCH,
        };
        assert_eq!(held.kind(), ResponseKind::Held);
    }
}

// Metadata refresh cohort bounds, purposes, and settlement.
//
// Concurrent metadata resolutions for one object and purpose share one
// refresh through `flow_control::coalesce::Table` in production
// `MetadataService`. Elections are bounded by [`MAX_REFRESH_ATTEMPTS`].
// [`refresh_settlement`] is the shared rule for a leader's outcome.

/// Waiters per refresh cohort.
pub const MAX_WAITERS: usize = 64;

/// Leader elections per cohort before broadcasting exhaustion.
pub const MAX_REFRESH_ATTEMPTS: usize = 8;

/// The work a refresh cohort may perform. A pin never bootstraps a current body.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub enum RefreshPurpose {
    /// Observe the current version without requesting its body (including HEAD).
    Current,
    /// Resolve immutable metadata for exactly this version, even after expiry.
    Pinned(StrongEtag),
    /// Observe the current version and allow acquisition of page zero.
    Bootstrap,
}

impl From<MetadataSelector> for RefreshPurpose {
    /// Metadata-only callers cannot opt into a body-fetching cohort.
    fn from(selector: MetadataSelector) -> Self {
        match selector {
            MetadataSelector::Fresh => Self::Current,
            MetadataSelector::Pinned(etag) => Self::Pinned(etag),
        }
    }
}

impl RefreshPurpose {
    /// Select metadata without changing whether the cohort may fetch a body.
    pub fn selector(&self) -> MetadataSelector {
        match self {
            Self::Current | Self::Bootstrap => MetadataSelector::Fresh,
            Self::Pinned(etag) => MetadataSelector::Pinned(etag.clone()),
        }
    }

    /// Whether this purpose may acquire page zero (a bootstrap operation).
    pub fn bootstrap(&self) -> bool {
        matches!(self, Self::Bootstrap)
    }
}

/// How a refresh leader settles its cohort.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Settlement {
    /// Broadcast the result to every waiter.
    Finish,
    /// Release leadership so another waiter may try with its own credentials
    /// or budget.
    Retry,
}

/// Settle a failed refresh. Credential-specific origin rejections and
/// failures of the leader's own budget let another waiter retry; other
/// failures finish the cohort.
pub fn refresh_settlement(error: Error, remaining_attempts: u32) -> Settlement {
    let rejected = matches!(error, Error::OriginRejected | Error::OriginForbidden);
    let caller_budget = matches!(
        error,
        Error::Cancelled | Error::DeadlineExceeded | Error::HopBudgetExhausted
    ) || (error == Error::Unavailable && remaining_attempts == 0);
    if rejected || caller_budget {
        Settlement::Retry
    } else {
        Settlement::Finish
    }
}

#[cfg(test)]
mod metadata_tests {
    use super::*;

    #[test]
    fn settlement_rules() {
        for error in [
            Error::OriginRejected,
            Error::OriginForbidden,
            Error::Cancelled,
            Error::DeadlineExceeded,
            Error::HopBudgetExhausted,
        ] {
            assert_eq!(refresh_settlement(error, 3), Settlement::Retry, "{error:?}");
        }
        assert_eq!(refresh_settlement(Error::Unavailable, 0), Settlement::Retry);
        assert_eq!(
            refresh_settlement(Error::Unavailable, 1),
            Settlement::Finish
        );
        assert_eq!(refresh_settlement(Error::NotFound, 0), Settlement::Finish);
    }

    #[test]
    fn purposes() {
        let etag = StrongEtag::test_value("v");
        assert_eq!(
            RefreshPurpose::from(MetadataSelector::Pinned(etag.clone())),
            RefreshPurpose::Pinned(etag.clone())
        );
        assert!(matches!(
            RefreshPurpose::Bootstrap.selector(),
            MetadataSelector::Fresh
        ));
        assert!(matches!(
            RefreshPurpose::Pinned(etag).selector(),
            MetadataSelector::Pinned(_)
        ));
        assert!(RefreshPurpose::Bootstrap.bootstrap());
        assert!(!RefreshPurpose::Current.bootstrap());
    }
}

// Sans-IO policy for serving an authenticated peer read request.
//
// Production `Coordinator::serve_peer` (src/read.rs) calls [`serve_action`]
// to dispatch, [`crate::read::budget::inherited_budget`] for acquisition credits,
// and [`crate::read::peer_error`] to translate failures.

/// Facts available after request authentication.
#[derive(Clone, Copy, Debug)]
pub struct ServeFacts {
    pub operation: OperationKind,
    /// The request's fetch mode is CopyOnly.
    pub copy_only: bool,
    /// The operation's object equals the signed origin context's object.
    pub object_matches: bool,
    /// The cache is configured locally and its metadata is available.
    pub cache_available: bool,
}

/// How to serve a peer request.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ServeAction {
    Reject(Error),
    /// Answer `Miss` without touching storage.
    Miss,
    /// Serve local copies only; never open credentials or acquire.
    CopyOnly,
    /// Open credentials and acquire with the inherited budget.
    Acquire,
}

/// Dispatch rule. Subscriptions never arrive here; warm actions always acquire.
pub fn serve_action(facts: ServeFacts) -> ServeAction {
    if facts.operation == OperationKind::Subscribe {
        return ServeAction::Reject(Error::InvalidRequest);
    }
    if !facts.object_matches {
        return ServeAction::Reject(Error::InvalidRequest);
    }
    if !facts.cache_available {
        return ServeAction::Miss;
    }
    if facts.copy_only && facts.operation != OperationKind::Warm {
        ServeAction::CopyOnly
    } else {
        ServeAction::Acquire
    }
}

/// Effective serve deadline: never later than the route's signed deadline.
pub fn serve_deadline(scope: Instant, route: Instant) -> Instant {
    scope.min(route)
}

/// Whether a copy-only page request from `requester` may wait on this
/// node's (`local`) running acquisition of the page, given the page's
/// `owners` (canonical order) under the request's membership.
///
/// Waiting is right when the requester is a reader, or an owner ranked
/// after this node asking a predecessor that is fetching for everyone (Known
/// issue 31). It is wrong when the requester is an owner ranked before this
/// node, or this node is not an owner: owners delegate a cold fetch to rank 0
/// through their predecessors and readers walk the owners, so this node's
/// acquisition most likely waits on the requester itself, which waits on
/// this answer until its time share runs out (Known issue 44). Owners ask
/// such nodes only as handoff, lineage or successor targets, which look for a
/// copy fetched earlier, so a local copy or a miss is the whole answer.
pub fn copy_join_allowed<N: PartialEq>(owners: &[N], local: &N, requester: &N) -> bool {
    let Some(theirs) = owners.iter().position(|n| n == requester) else {
        return true;
    };
    owners
        .iter()
        .position(|n| n == local)
        .is_some_and(|mine| mine < theirs)
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod serve_tests {
    use super::*;
    use std::time::Duration;

    fn facts(operation: OperationKind, copy_only: bool) -> ServeFacts {
        ServeFacts {
            operation,
            copy_only,
            object_matches: true,
            cache_available: true,
        }
    }

    #[test]
    fn dispatch_table() {
        assert_eq!(
            serve_action(facts(OperationKind::Subscribe, false)),
            ServeAction::Reject(Error::InvalidRequest)
        );
        assert_eq!(
            serve_action(facts(OperationKind::Page, true)),
            ServeAction::CopyOnly
        );
        assert_eq!(
            serve_action(facts(OperationKind::Page, false)),
            ServeAction::Acquire
        );
        assert_eq!(
            serve_action(facts(OperationKind::Bootstrap, true)),
            ServeAction::CopyOnly
        );
        assert_eq!(
            serve_action(facts(OperationKind::Metadata { fresh: true }, true)),
            ServeAction::CopyOnly
        );
        assert_eq!(
            serve_action(facts(OperationKind::Warm, true)),
            ServeAction::Acquire
        );
        assert_eq!(
            serve_action(ServeFacts {
                object_matches: false,
                ..facts(OperationKind::Page, true)
            }),
            ServeAction::Reject(Error::InvalidRequest)
        );
        assert_eq!(
            serve_action(ServeFacts {
                cache_available: false,
                ..facts(OperationKind::Page, false)
            }),
            ServeAction::Miss
        );
    }

    #[test]
    fn deadline_never_extends() {
        let now = Instant::now();
        let later = now + Duration::from_secs(1);
        assert_eq!(serve_deadline(now, later), now);
        assert_eq!(serve_deadline(later, now), now);
    }

    #[test]
    fn copy_only_joins_only_for_readers_and_later_owners() {
        let owners = ["r0", "r1", "r2"];
        // A reader may wait on any node's acquisition.
        assert!(copy_join_allowed(&owners, &"r0", &"reader"));
        assert!(copy_join_allowed(&owners, &"other", &"reader"));
        // A later owner asking a predecessor that is fetching (Known issue 31).
        assert!(copy_join_allowed(&owners, &"r0", &"r2"));
        assert!(copy_join_allowed(&owners, &"r1", &"r2"));
        // An earlier owner asking a later owner, or an owner asking a
        // non-owner: that acquisition most likely waits on the requester.
        assert!(!copy_join_allowed(&owners, &"r2", &"r0"));
        assert!(!copy_join_allowed(&owners, &"r1", &"r0"));
        assert!(!copy_join_allowed(&owners, &"demoted", &"r0"));
        assert!(!copy_join_allowed(&owners, &"r1", &"r1"));
        // Without a ranking nothing changes.
        assert!(copy_join_allowed::<&str>(&[], &"r1", &"r0"));
    }
}

// Small read-path policy rules shared by fill, serve, and race drivers.

/// Whether a contender's failure leaves the other contender of a race free to
/// win. Malformed framing is local to that contender, not authority to cancel
/// another independently authenticated usable page.
pub fn recoverable(error: Error) -> bool {
    matches!(
        error,
        Error::Unavailable
            | Error::Io
            | Error::Os(_)
            | Error::Overloaded
            | Error::CorruptRecord
            | Error::MissingKey
            | Error::Cancelled
            | Error::InvalidRequest
            | Error::HeaderTooLarge
    )
}

/// Translate an expected acquisition failure into the peer response that
/// reports it. `None` means the error is internal and must propagate.
pub fn peer_error(error: Error) -> Option<ResponseKind> {
    Some(match error {
        Error::NotFound => ResponseKind::NotFound,
        Error::VersionUnavailable => ResponseKind::VersionUnavailable,
        Error::Overloaded => ResponseKind::Overloaded,
        Error::OriginRejected => ResponseKind::OriginRejected,
        Error::OriginForbidden => ResponseKind::OriginForbidden,
        Error::Unavailable | Error::HopBudgetExhausted | Error::DeadlineExceeded => {
            ResponseKind::Unavailable
        }
        _ => return None,
    })
}

/// Optional writes may use half of the aggregate class budget, with a floor
/// sufficient for one valid page when the configured budget can hold it. Owned
/// usage counts against this conservative ceiling; quota admission remains the
/// authority for per-cache fairness and all completion-owned allocations.
pub fn optional_headroom(limit: usize, used: usize, amount: usize, one_page: usize) -> bool {
    let ceiling = optional_ceiling(limit, one_page);
    amount <= ceiling && used <= ceiling - amount
}

/// Ceiling for optional (non-owned) usage of one resource class.
pub fn optional_ceiling(limit: usize, one_page: usize) -> usize {
    limit.min((limit / 2).max(one_page))
}

/// Second-sight admission: owned pages always qualify; others only when
/// retention is enabled and the page was seen before.
pub fn retention_eligible(owned: bool, enabled: bool, seen: bool) -> bool {
    owned || (enabled && seen)
}

/// Resident retention score: exact heat plus one for owned pages, zero when
/// retention is disabled.
pub fn retention_score(heat: u8, owned: bool, enabled: bool) -> u8 {
    if enabled { heat + u8::from(owned) } else { 0 }
}

/// Why a verified page is or is not persisted to local disk.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Persist {
    /// Not eligible under second-sight, or metadata did not validate.
    Ineligible,
    /// Already resident in a copy-only slot or the disk index.
    Resident,
    /// A non-owned page lacked optional dirty headroom.
    NoHeadroom,
    /// Reserve and enqueue the write.
    Write,
}

/// Facts needed to decide persistence of one verified page.
#[derive(Clone, Copy, Debug)]
pub struct PersistFacts {
    pub eligible: bool,
    pub metadata_valid: bool,
    pub resident: bool,
    pub owned: bool,
    pub dirty_limit: usize,
    pub dirty_used: usize,
    pub amount: usize,
    pub one_page: usize,
}

/// Persist decision before reservation; reservation failures are separate.
pub fn persist(facts: PersistFacts) -> Persist {
    if !facts.eligible || !facts.metadata_valid {
        Persist::Ineligible
    } else if facts.resident {
        Persist::Resident
    } else if !facts.owned
        && !optional_headroom(
            facts.dirty_limit,
            facts.dirty_used,
            facts.amount,
            facts.one_page,
        )
    {
        Persist::NoHeadroom
    } else {
        Persist::Write
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod policy_tests {
    use super::*;

    #[test]
    fn recoverable_errors() {
        for error in [
            Error::Unavailable,
            Error::Io,
            Error::Os(5),
            Error::Overloaded,
            Error::CorruptRecord,
            Error::MissingKey,
            Error::Cancelled,
            Error::InvalidRequest,
            Error::HeaderTooLarge,
        ] {
            assert!(recoverable(error), "{error:?}");
        }
        for error in [
            Error::Unauthorized,
            Error::DeadlineExceeded,
            Error::NotFound,
            Error::OriginForbidden,
        ] {
            assert!(!recoverable(error), "{error:?}");
        }
    }

    #[test]
    fn peer_error_table() {
        assert_eq!(peer_error(Error::NotFound), Some(ResponseKind::NotFound));
        assert_eq!(
            peer_error(Error::DeadlineExceeded),
            Some(ResponseKind::Unavailable)
        );
        assert_eq!(
            peer_error(Error::HopBudgetExhausted),
            Some(ResponseKind::Unavailable)
        );
        assert_eq!(
            peer_error(Error::OriginRejected),
            Some(ResponseKind::OriginRejected)
        );
        assert_eq!(peer_error(Error::Unauthorized), None);
        assert_eq!(peer_error(Error::Internal), None);
    }

    #[test]
    fn optional_headroom_floor_and_half() {
        assert!(optional_headroom(100, 0, 50, 10));
        assert!(!optional_headroom(100, 1, 50, 10));
        assert!(optional_headroom(100, 0, 60, 60));
        assert!(!optional_headroom(50, 0, 60, 60));
        assert!(!optional_headroom(0, 0, 1, 1));
        assert_eq!(optional_ceiling(10, 100), 10);
    }

    #[test]
    fn retention_rules() {
        assert!(retention_eligible(true, false, false));
        assert!(!retention_eligible(false, false, true));
        assert!(retention_eligible(false, true, true));
        assert!(!retention_eligible(false, true, false));
        assert_eq!(retention_score(2, true, true), 3);
        assert_eq!(retention_score(2, true, false), 0);
    }

    #[test]
    fn persist_decisions() {
        let base = PersistFacts {
            eligible: true,
            metadata_valid: true,
            resident: false,
            owned: false,
            dirty_limit: 100,
            dirty_used: 0,
            amount: 10,
            one_page: 10,
        };
        assert_eq!(persist(base), Persist::Write);
        assert_eq!(
            persist(PersistFacts {
                eligible: false,
                ..base
            }),
            Persist::Ineligible
        );
        assert_eq!(
            persist(PersistFacts {
                metadata_valid: false,
                ..base
            }),
            Persist::Ineligible
        );
        assert_eq!(
            persist(PersistFacts {
                resident: true,
                ..base
            }),
            Persist::Resident
        );
        assert_eq!(
            persist(PersistFacts {
                dirty_used: 45,
                ..base
            }),
            Persist::NoHeadroom
        );
        assert_eq!(
            persist(PersistFacts {
                dirty_used: 45,
                owned: true,
                ..base
            }),
            Persist::Write
        );
    }
}

// Sans-IO range stream scheduling: which window slots to fill directly,
// whether to start another provider selection, how many holes it demands,
// and the per-page acquisition budget.
//
// Production `RangeStream` (src/read/range_stream.rs) calls these with its
// window state; the simulator calls them with its own slot model.

/// Direct fills a read keeps in flight beyond its head page.
pub const DIRECT_AHEAD: usize = 1;

/// Provider selections a read keeps in flight. Beyond the first, a read only
/// starts another while some contract of its version has an unused granted
/// flight. Each selection demands a disjoint share of the selectable holes.
pub const SELECTIONS: usize = 4;

/// Attempts and links each independently admitted page receives.
pub const PAGE_ATTEMPTS: u32 = 8;
pub const PAGE_LINKS: u8 = 16;

/// Scheduling-relevant state of one window slot.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SlotKind {
    Probing,
    /// Not in memory and not requested; `select` is false once a selection
    /// covering the page failed.
    Missing {
        select: bool,
    },
    Fetching,
    Selecting,
    Ready,
    Failed,
}

/// Slots to start direct fills for, in order: the head when it is a hole or
/// only selected, then up to `direct_ahead` (normally [`DIRECT_AHEAD`]) more
/// missing holes that no other window is busy with. Nothing at or after the
/// first failed slot is planned. The driver stops at the first fill the budget
/// refuses.
pub fn direct_fills(
    slots: &[SlotKind],
    direct_ahead: usize,
    elsewhere: impl Fn(usize) -> bool,
) -> Vec<usize> {
    let limit = first_failure(slots).unwrap_or(slots.len());
    let mut plan = Vec::new();
    if limit > 0 && matches!(slots[0], SlotKind::Missing { .. } | SlotKind::Selecting) {
        plan.push(0);
    }
    let mut ahead = slots
        .iter()
        .skip(1)
        .filter(|slot| **slot == SlotKind::Fetching)
        .count();
    let mut index = 1;
    while ahead < direct_ahead && index < limit {
        if matches!(slots[index], SlotKind::Missing { .. }) && !elsewhere(index) {
            plan.push(index);
            ahead += 1;
        }
        index += 1;
    }
    plan
}

/// Position of the first failed slot; admission stops there.
pub fn first_failure(slots: &[SlotKind]) -> Option<usize> {
    slots.iter().position(|slot| *slot == SlotKind::Failed)
}

/// Whether a read may start another selection: its first always, more only
/// while a contract of the version has an unused granted flight.
pub fn may_select(selections: usize, spare: usize) -> bool {
    selections < SELECTIONS && (selections == 0 || spare > 0)
}

/// Holes the next selection may demand: an even share of the selectable holes
/// over the selections the read can start now.
pub fn share(holes: usize, selections: usize, spare: usize) -> usize {
    let free = SELECTIONS.saturating_sub(selections);
    // The read's first selection may run before any spare flight exists.
    let startable = if selections == 0 {
        free.min(spare + 1)
    } else {
        free.min(spare)
    };
    holes.div_ceil(startable.max(1)).max(1)
}

/// Up to `limit` selectable holes starting at page `next`, excluding pages
/// another window is busy with, as at most `max_intervals - 1` contiguous
/// page intervals plus the page set.
pub fn selection_demand(
    slots: &[SlotKind],
    next: u64,
    elsewhere: impl Fn(u64) -> bool,
    limit: usize,
    max_intervals: usize,
) -> Option<(Vec<Range<u64>>, BTreeSet<u64>)> {
    let mut intervals: Vec<Range<u64>> = Vec::new();
    let mut pages = BTreeSet::new();
    for (index, slot) in slots.iter().enumerate() {
        if pages.len() >= limit {
            break;
        }
        let number = next + index as u64;
        if *slot != (SlotKind::Missing { select: true }) || elsewhere(number) {
            continue;
        }
        let full = intervals.len() == max_intervals - 1;
        match intervals.last_mut() {
            Some(last) if last.end == number => last.end += 1,
            _ if full => break,
            _ => intervals.push(number..number + 1),
        }
        pages.insert(number);
    }
    (!pages.is_empty()).then_some((intervals, pages))
}

/// How each admitted page of a range is budgeted.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PageBudget {
    /// Client range: every page shares the client's deadline.
    Client { deadline: Instant },
    /// Progress-bounded stream: each page gets `timeout` from its start.
    Progressing { timeout: Duration },
}

impl PageBudget {
    /// Budget for the next independently admitted page at `now`.
    pub fn next_page(&self, now: Instant) -> Result<AcquisitionBudget> {
        match *self {
            Self::Progressing { timeout } => Ok(AcquisitionBudget::new(
                now + timeout,
                PAGE_ATTEMPTS,
                PAGE_LINKS,
            )),
            Self::Client { deadline } => {
                if now >= deadline {
                    return Err(Error::DeadlineExceeded);
                }
                Ok(AcquisitionBudget::new(deadline, PAGE_ATTEMPTS, PAGE_LINKS))
            }
        }
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod range_tests {
    use super::*;
    use SlotKind::*;

    const HOLE: SlotKind = Missing { select: true };

    #[test]
    fn head_and_one_ahead() {
        assert_eq!(
            direct_fills(&[HOLE, HOLE, HOLE], DIRECT_AHEAD, |_| false),
            [0, 1]
        );
        assert_eq!(
            direct_fills(&[Selecting, HOLE], DIRECT_AHEAD, |_| false),
            [0, 1]
        );
        assert_eq!(
            direct_fills(&[Ready, HOLE, HOLE], DIRECT_AHEAD, |_| false),
            [1]
        );
        assert_eq!(
            direct_fills(&[HOLE, Fetching, HOLE], DIRECT_AHEAD, |_| false),
            [0]
        );
        assert_eq!(
            direct_fills(&[HOLE, HOLE, HOLE], DIRECT_AHEAD, |i| i == 1),
            [0, 2]
        );
        assert_eq!(
            direct_fills(&[Probing, Missing { select: false }], DIRECT_AHEAD, |_| {
                false
            }),
            [1]
        );
        assert!(direct_fills(&[], DIRECT_AHEAD, |_| false).is_empty());
        assert_eq!(direct_fills(&[HOLE, HOLE, HOLE], 2, |_| false), [0, 1, 2]);
    }

    #[test]
    fn failure_stops_admission() {
        assert_eq!(
            direct_fills(&[HOLE, Failed, HOLE], DIRECT_AHEAD, |_| false),
            [0]
        );
        assert!(direct_fills(&[Failed, HOLE], DIRECT_AHEAD, |_| false).is_empty());
        assert_eq!(first_failure(&[Ready, Failed]), Some(1));
    }

    #[test]
    fn selection_counts() {
        assert!(may_select(0, 0));
        assert!(!may_select(1, 0));
        assert!(may_select(1, 1));
        assert!(!may_select(SELECTIONS, 9));
        assert_eq!(share(10, 0, 0), 10);
        assert_eq!(share(10, 0, 9), 3);
        assert_eq!(share(10, 1, 1), 10);
        assert_eq!(share(10, 1, 3), 4);
        assert_eq!(share(0, 0, 0), 1);
        assert_eq!(share(5, 4, 9), 5);
    }

    #[test]
    fn demand_intervals() {
        let slots = [HOLE, HOLE, Fetching, HOLE, Missing { select: false }, HOLE];
        let (intervals, pages) = selection_demand(&slots, 10, |_| false, 99, 64).unwrap();
        assert_eq!(intervals, [10..12, 13..14, 15..16]);
        assert_eq!(pages.into_iter().collect::<Vec<_>>(), [10, 11, 13, 15]);
        let (intervals, _) = selection_demand(&slots, 10, |n| n == 11, 2, 64).unwrap();
        assert_eq!(intervals, [10..11, 13..14]);
        let (intervals, _) = selection_demand(&slots, 10, |_| false, 99, 2).unwrap();
        assert_eq!(intervals.len(), 1);
        assert_eq!(intervals[0], 10..12);
        assert!(selection_demand(&[Ready], 0, |_| false, 9, 64).is_none());
    }

    #[test]
    fn page_budgets() {
        let now = Instant::now();
        let budget = PageBudget::Progressing {
            timeout: Duration::from_secs(3),
        }
        .next_page(now)
        .unwrap();
        assert_eq!(budget.deadline(), now + Duration::from_secs(3));
        assert_eq!(
            (budget.remaining_attempts(), budget.remaining_links()),
            (8, 16)
        );
        let client = PageBudget::Client { deadline: now };
        assert!(matches!(
            client.next_page(now),
            Err(Error::DeadlineExceeded)
        ));
        let client = PageBudget::Client {
            deadline: now + Duration::from_secs(1),
        };
        assert_eq!(
            client.next_page(now).unwrap().deadline(),
            now + Duration::from_secs(1)
        );
    }
}

// Sans-IO page fill plan: local tiers, then the candidate walk, then origin,
// then publication.
//
// Production `Fill::acquire_once` (`src/read/fill.rs`) and the simulator both
// drive [`FillPlan`]; production also calls [`leader_source`],
// [`acquisition_failure`], and [`ciphertext_publish_tolerated`].

/// Which local source an elected leader tries before network acquisition.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum LeaderSource {
    /// Ciphertext retained by the flight from an earlier partial publication.
    Retained,
    /// Join the shared local plaintext decrypt job.
    SharedLocal,
    /// Look up cached ciphertext (memory or disk-backed).
    Cached,
}

/// Order of local sources for an elected leader. `retained` is the flight's
/// partial ciphertext; `local_job` is an existing shared plaintext job; and
/// `unverified` is cached ciphertext in memory.
pub fn leader_source(
    retained: bool,
    plaintext: bool,
    local_job: bool,
    unverified: bool,
) -> LeaderSource {
    if retained {
        LeaderSource::Retained
    } else if plaintext && (local_job || unverified) {
        LeaderSource::SharedLocal
    } else {
        LeaderSource::Cached
    }
}

/// How a failed leader settles its flight. `E` is the driver's error type
/// (production `AcquisitionFailure` is `FailureKind<racer_dataplane::Error>`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FailureKind<E = Error> {
    /// A validated credential-specific origin rejection.
    OriginRejected,
    /// Credential-specific origin 403; retry with independent credentials.
    OriginForbidden,
    /// Terminal only for this cohort; never negative-cache a page here.
    Terminal(E),
}

/// Map a leader's acquisition error to its flight failure.
pub fn acquisition_failure(error: Error) -> FailureKind {
    match error {
        Error::OriginRejected => FailureKind::OriginRejected,
        Error::OriginForbidden => FailureKind::OriginForbidden,
        error => FailureKind::Terminal(error),
    }
}

/// Errors from publishing an acquired ciphertext copy to memory that do not
/// fail the acquisition (the copy is still returned to waiters).
pub fn ciphertext_publish_tolerated(error: Error) -> bool {
    matches!(
        error,
        Error::Overloaded | Error::MissingKey | Error::Unavailable
    )
}

/// Where a filled page came from.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Source {
    Local,
    Hedge,
    Peer,
    Origin,
}

/// Next action the driver must perform.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum FillStep {
    /// Probe local memory and disk copies.
    LocalCopy,
    /// Optionally race a speculative hedge (plaintext page fills only).
    Hedge,
    /// Run the candidate walk.
    Walk,
    /// This owner contacts origin.
    Origin,
    /// After an origin version miss, probe later owners copy-only.
    RemainingCopy,
    /// Publish the page obtained from `Source`.
    Publish(Source),
    /// Finished.
    Done(Source),
    Failed(Error),
}

/// How the walk ended, as reported to the plan.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WalkResult {
    Copy,
    Origin { evidence: Vec<ProbeOutcome> },
    Failed(Error),
}

/// Sans-IO fill state machine for one elected acquisition.
#[derive(Clone, Debug)]
pub struct FillPlan {
    step: FillStep,
    plaintext: bool,
    evidence: Vec<ProbeOutcome>,
}

impl FillPlan {
    /// Start with the local tiers. `plaintext` selects hedging and whether a
    /// peer copy is decrypted before publication.
    pub fn new(plaintext: bool) -> Self {
        Self {
            step: FillStep::LocalCopy,
            plaintext,
            evidence: Vec::new(),
        }
    }

    pub fn step(&self) -> &FillStep {
        &self.step
    }

    /// Report the local tier result.
    pub fn local(&mut self, hit: bool) {
        self.expect(FillStep::LocalCopy);
        self.step = if hit {
            FillStep::Done(Source::Local)
        } else if self.plaintext {
            FillStep::Hedge
        } else {
            FillStep::Walk
        };
    }

    /// Report whether a hedge produced a page (`false` when not eligible).
    pub fn hedge(&mut self, won: bool) {
        self.expect(FillStep::Hedge);
        self.step = if won {
            FillStep::Publish(Source::Hedge)
        } else {
            FillStep::Walk
        };
    }

    /// Report how the candidate walk ended.
    pub fn walk(&mut self, result: WalkResult) {
        self.expect(FillStep::Walk);
        self.step = match result {
            WalkResult::Copy => FillStep::Publish(Source::Peer),
            WalkResult::Origin { evidence } => {
                self.evidence = evidence;
                FillStep::Origin
            }
            WalkResult::Failed(error) => FillStep::Failed(error),
        };
    }

    /// Report the origin result.
    pub fn origin(&mut self, result: Result<(), Error>) {
        self.expect(FillStep::Origin);
        self.step = match result {
            Ok(()) => FillStep::Publish(Source::Origin),
            Err(Error::VersionUnavailable) => FillStep::RemainingCopy,
            Err(error) => FillStep::Failed(error),
        };
    }

    /// Report how the remaining-copy walk ended.
    pub fn remaining(&mut self, result: Result<RemainingEnd, Error>) {
        self.expect(FillStep::RemainingCopy);
        self.step = match result {
            Ok(RemainingEnd::Copy) => FillStep::Publish(Source::Peer),
            Ok(RemainingEnd::None) => FillStep::Failed(origin_miss_error(&self.evidence)),
            Ok(RemainingEnd::Unavailable) => FillStep::Failed(Error::Unavailable),
            Err(error) => FillStep::Failed(error),
        };
    }

    /// Report publication.
    pub fn published(&mut self, result: Result<(), Error>) {
        let FillStep::Publish(source) = self.step else {
            panic!("not publishing: {:?}", self.step);
        };
        self.step = match result {
            Ok(()) => FillStep::Done(source),
            Err(error) => FillStep::Failed(error),
        };
    }

    fn expect(&self, step: FillStep) {
        assert_eq!(self.step, step, "fill plan out of order");
    }
}

#[cfg(test)]
mod fill_tests {
    use super::*;

    #[test]
    fn local_hit_finishes() {
        let mut plan = FillPlan::new(true);
        plan.local(true);
        assert_eq!(plan.step(), &FillStep::Done(Source::Local));
    }

    #[test]
    fn plaintext_hedges_before_walk_ciphertext_does_not() {
        let mut plan = FillPlan::new(true);
        plan.local(false);
        assert_eq!(plan.step(), &FillStep::Hedge);
        plan.hedge(true);
        assert_eq!(plan.step(), &FillStep::Publish(Source::Hedge));
        plan.published(Ok(()));
        assert_eq!(plan.step(), &FillStep::Done(Source::Hedge));
        let mut plan = FillPlan::new(false);
        plan.local(false);
        assert_eq!(plan.step(), &FillStep::Walk);
    }

    #[test]
    fn origin_then_publish() {
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Origin { evidence: vec![] });
        assert_eq!(plan.step(), &FillStep::Origin);
        plan.origin(Ok(()));
        plan.published(Err(Error::Overloaded));
        assert_eq!(plan.step(), &FillStep::Failed(Error::Overloaded));
    }

    #[test]
    fn version_miss_probes_remaining_copies_then_classifies() {
        for (end, step) in [
            (Ok(RemainingEnd::Copy), FillStep::Publish(Source::Peer)),
            (Ok(RemainingEnd::None), FillStep::Failed(Error::Unavailable)),
            (
                Ok(RemainingEnd::Unavailable),
                FillStep::Failed(Error::Unavailable),
            ),
            (Err(Error::Cancelled), FillStep::Failed(Error::Cancelled)),
        ] {
            let mut plan = FillPlan::new(false);
            plan.local(false);
            plan.walk(WalkResult::Origin {
                evidence: vec![ProbeOutcome::Unreachable],
            });
            plan.origin(Err(Error::VersionUnavailable));
            assert_eq!(plan.step(), &FillStep::RemainingCopy);
            plan.remaining(end);
            assert_eq!(plan.step(), &step);
        }
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Origin {
            evidence: vec![ProbeOutcome::CopyMiss],
        });
        plan.origin(Err(Error::VersionUnavailable));
        plan.remaining(Ok(RemainingEnd::None));
        assert_eq!(plan.step(), &FillStep::Failed(Error::VersionUnavailable));
    }

    #[test]
    fn walk_and_origin_failures() {
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Failed(Error::Unavailable));
        assert_eq!(plan.step(), &FillStep::Failed(Error::Unavailable));
        let mut plan = FillPlan::new(false);
        plan.local(false);
        plan.walk(WalkResult::Origin { evidence: vec![] });
        plan.origin(Err(Error::OriginForbidden));
        assert_eq!(plan.step(), &FillStep::Failed(Error::OriginForbidden));
    }

    #[test]
    #[should_panic(expected = "out of order")]
    fn out_of_order_report_panics() {
        FillPlan::new(false).origin(Ok(()));
    }

    #[test]
    fn leader_sources_and_failures() {
        assert_eq!(
            leader_source(true, true, true, true),
            LeaderSource::Retained
        );
        assert_eq!(
            leader_source(false, true, true, false),
            LeaderSource::SharedLocal
        );
        assert_eq!(
            leader_source(false, true, false, true),
            LeaderSource::SharedLocal
        );
        assert_eq!(
            leader_source(false, false, true, true),
            LeaderSource::Cached
        );
        assert_eq!(
            leader_source(false, true, false, false),
            LeaderSource::Cached
        );
        assert_eq!(
            acquisition_failure(Error::OriginRejected),
            FailureKind::OriginRejected
        );
        assert_eq!(
            acquisition_failure(Error::OriginForbidden),
            FailureKind::OriginForbidden
        );
        assert_eq!(
            acquisition_failure(Error::Unavailable),
            FailureKind::Terminal(Error::Unavailable)
        );
        assert!(ciphertext_publish_tolerated(Error::MissingKey));
        assert!(!ciphertext_publish_tolerated(Error::CorruptRecord));
    }
}

// Candidate ordering, peer response classification, and origin authority rules.

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
/// a later owner (`since`, see [`crate::read::walk::lineage_targets`]), visit that
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
/// [`crate::liveness::Silence`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq, PartialOrd, Ord)]
pub enum Answering {
    /// Nothing against it.
    Answers,
    /// It left one of this node's requests unanswered and has answered
    /// nothing since ([`crate::liveness::Silence::unanswered`]).
    Unanswered,
    /// It left a ping unanswered: requests for it fail at once
    /// ([`crate::liveness::Silence::silent`]).
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

/// Bytes one hedge duplicate holds: two pages plus a tag.
pub const fn duplicate_bytes(page_bytes: usize) -> usize {
    page_bytes * 2 + 16
}

/// Plaintext and ciphertext bytes a hedge reserves for `pages` pages.
pub const fn hedge_page_charges(page_bytes: usize, pages: usize) -> (usize, usize) {
    (page_bytes * pages, (page_bytes + 16) * pages)
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod candidates_tests {
    #[test]
    fn hedge_sizes() {
        assert_eq!(super::duplicate_bytes(1 << 20), (2 << 20) + 16);
        assert_eq!(super::hedge_page_charges(100, 2), (200, 232));
    }

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
