//! Sans-IO candidate walk for one membership epoch.
//!
//! [`CandidateWalk`] owns the arithmetic and ordering of the dataplane's
//! candidate resolution: which ranked owner to probe next, whether the probe
//! may start acquisition, how credits are partitioned for it, how responses
//! accumulate predecessor evidence, and how the walk ends. The caller performs
//! each probe (network, simulation, or test script) and reports the outcome.
//!
//! Driving loop:
//!
//! ```text
//! let mut walk = CandidateWalk::new(config);
//! loop {
//!     match walk.next(&mut budget)? {
//!         Step::Probe(probe) => {
//!             let outcome = exchange(probe, walk.request_budget(&mut budget));
//!             if let Some(end) = walk.complete(&mut budget, outcome)? { break end }
//!         }
//!         Step::End(end) => break end,
//!     }
//! }
//! ```

use super::budget::AcquisitionBudget;
use super::candidates::{ProbeOutcome, VisitOrder};
use super::error::{Error, Result};
use super::fanout::{
    HELPER_ATTEMPTS, HELPER_FLOOR_ATTEMPTS, HELPER_FLOOR_LINKS, HELPER_LINKS, HELPER_MIN_ATTEMPTS,
};
pub use super::rank0::Rank0State;
use std::time::{Duration, Instant};

/// Whether a probe may start acquisition at its destination.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Mode {
    CopyOnly,
    Acquire,
}

/// How a probe's remote credits are funded.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RouteMode {
    /// Remote credits are a fair share of the caller's remaining attempts.
    SharedRoute,
    /// The probe owns a partitioned child budget and forwards all of it.
    FundedRoute,
    /// A pinned single-link direct exchange used by hedging.
    DirectHedge,
}

/// Carry-over from a completed hedge, or from a stale-membership retry.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Continuation {
    /// The first visited owner was already asked by the hedge primary.
    pub primary_consumed: bool,
    pub primary_outcome: Option<ProbeOutcome>,
    /// Every probe uses a bounded, partitioned route.
    pub bounded_routes: bool,
}

/// Inputs fixed for one epoch.
#[derive(Clone, Copy, Debug)]
pub struct WalkConfig {
    /// This node's canonical rank among the owners, if it is one.
    pub rank: Option<usize>,
    /// Number of ranked owners (1..=3).
    pub owners: usize,
    /// Visit order for a non-owner; ignored for owners, which probe their
    /// predecessors canonically.
    pub order: VisitOrder,
    /// The operation is a fixed page fetch (`Operation::Page`).
    pub page_operation: bool,
    pub continuation: Continuation,
    /// Previous-membership owners an owner may ask copy-only before origin
    /// (at most [`MAX_HANDOFF`]); see [`handoff_targets`]. A probe of handoff
    /// target `i` reports `canonical = owners + i`. Ignored for non-owners and
    /// for operations other than pages.
    pub handoff: usize,
    /// Predecessors (bit `c` for canonical rank `c`) to ask under membership
    /// version - 1 ([`Probe::previous`]): the caller replaced that version at
    /// most [`PREVIOUS_MEMBERSHIP_WITHIN`] ago, this node is a member of
    /// it, and the predecessor held the same rank
    /// there, so the request carries the same authority and a predecessor
    /// that has not adopted the current version yet still serves it (Known
    /// issue 12); see [`same_rank_previous`]. Ignored for non-owners, for
    /// handoff targets, and for non-page walks.
    pub previous: u8,
    /// A non-owner page walk has an overflow helper (see
    /// [`super::fanout`]); a probe of it reports `canonical = owners`.
    /// The walk is eligible only when it starts with at least
    /// [`HELPER_MIN_ATTEMPTS`] attempts (so a helper, which inherits fewer,
    /// never asks another helper). It asks the helper at most once, right
    /// after an owner itself sheds the probe ([`Outcome::Shed`]; an
    /// `Overloaded` signed by a relay or a timeout does not count, since
    /// those say the route, not the page, is congested), funded with up to
    /// [`HELPER_ATTEMPTS`] attempts and
    /// [`HELPER_LINKS`] links from what is left. Owner probes are planned
    /// exactly as without a helper. Ignored for owners and non-pages.
    pub helper: bool,
    /// Per canonical owner rank, how many lineage targets that owner has
    /// ([`lineage_targets`]): later owners that arrived before it and so may
    /// hold a copy fetched before it was an owner (Known issue 25). Funds
    /// their copy-only probes from slack: a non-owner's out-of-order
    /// `Acquire` to that owner carries one more credit per target (and per
    /// rank 0 target, for the owner's delegation to rank 0), and a
    /// delegation to rank 0 forwards spare credits for rank 0's. Never
    /// reserved: without slack the walk funds exactly as before.
    pub lineage: [u8; MAX_HANDOFF],
    /// The first `granted` handoff targets are asked copy-only once, before
    /// origin, each with its own grant ([`SUCCESSOR_GRANT_ATTEMPTS`],
    /// [`SUCCESSOR_GRANT_LINKS`]) instead of the walk's budget, whose
    /// credits a delegated rank 0 needs for origin. They are successor
    /// targets ([`crate::read::successor::successor_targets`]: later owners
    /// this owner answered without a copy, which then likely fetched the
    /// page themselves, Known issue 21), then lineage targets
    /// ([`lineage_targets`], [`demoted_targets`]: members that ranked above
    /// this owner before it arrived, Known issues 34 and 35); see
    /// [`granted_prefix`]. The grant's unused credits are dropped, never
    /// added to the walk's budget, so the walk's credits never grow.
    pub granted: usize,
    /// Send the granted probes of a run of consecutive granted targets at
    /// once ([`Step::Granted`]) instead of one after the other (Known issue
    /// 45). Each still has its own grant and only a copy changes the walk,
    /// so the order of their outcomes cannot matter; the run costs one time
    /// share instead of one per target.
    pub parallel_granted: bool,
    /// What this owner last learned about rank 0's acquisition of the page
    /// ([`crate::read::rank0::Rank0Watch`], Known issue 39). `Overdue`: a
    /// timeout of rank 0 gives origin authority like a transport failure,
    /// since rank 0 stayed silent too long
    /// ([`crate::read::rank0::Rank0Watch::hang_after`]). `Fetching` and
    /// `Unknown` keep the [`Outcome::TimedOut`] rule. `Failed`: this
    /// owner's own origin request for the page failed recently, so
    /// [`Outcome::Failed`] withholds authority too (Known issue 52). Ignored
    /// unless this node is an owner above rank 0 on a page walk.
    pub rank0: Rank0State,
}

/// Attempts granted to one successor probe ([`WalkConfig::successors`]): the
/// copy-only exchange.
pub const SUCCESSOR_GRANT_ATTEMPTS: u32 = 1;

/// Extra opportunities a successor probe's time share is divided by, so it
/// takes at most a fifth of the time left ([`plan_attempt`]).
pub const SUCCESSOR_SHARE_DIVISOR: u32 = 4;

/// Links granted to one successor probe, as a reader funds an owner.
pub const SUCCESSOR_GRANT_LINKS: u8 = 8;

/// Most previous-membership owners one walk asks (the replica count).
pub const MAX_HANDOFF: usize = 3;

/// Most lineage targets (later owners and demoted members,
/// [`lineage_targets`], [`demoted_targets`]) one owner walk asks, each on its
/// own grant (Known issue 46). Larger than [`MAX_HANDOFF`] because granted
/// probes are sent at once ([`WalkConfig::parallel_granted`]): more targets
/// add copy-only exchanges, not latency, and never draw on the walk's
/// budget.
pub const MAX_LINEAGE: usize = 6;

/// Most handoff targets one owner walk lists ([`WalkConfig::handoff`]):
/// successor targets (later owners, at most two) and lineage targets.
pub const MAX_TARGETS: usize = REPLICAS_BELOW + MAX_LINEAGE;

/// Later owners an owner can have (successor targets).
const REPLICAS_BELOW: usize = MAX_HANDOFF - 1;

/// The grant a granted probe spends ([`Step::Granted`],
/// [`WalkConfig::granted`]): fresh credits that are never added to the
/// walk's budget.
pub fn granted_budget(deadline: Instant) -> AcquisitionBudget {
    AcquisitionBudget::new(deadline, SUCCESSOR_GRANT_ATTEMPTS, SUCCESSOR_GRANT_LINKS)
}

/// Fewest attempts a non-owner funds an out-of-order Acquire to the owner at
/// `canonical` rank with: the exchange itself, one copy-only probe per
/// predecessor, and the owner's own origin attempt.
pub fn owner_funding_min(canonical: usize) -> u32 {
    canonical as u32 + 2
}

/// Attempts a non-owner funds an out-of-order Acquire with when it can:
/// [`owner_funding_min`] plus, above rank 0, the remote credit the owner's
/// `Acquire` to rank 0 carries, so it can delegate the cold fetch.
pub fn owner_funding(canonical: usize) -> u32 {
    owner_funding_min(canonical) + u32::from(canonical > 0)
}

/// How long after this node replaced membership version - 1 its owners may
/// still ask predecessors under it ([`WalkConfig::previous`]). Peers keep a
/// superseded membership for a 30 s grace window
/// (`Snapshot::retention`), so this leaves 20 s for a peer that adopted the
/// current membership before this node; past it, a peer may answer
/// `StaleMembership` and the probe would be wasted.
pub const PREVIOUS_MEMBERSHIP_WITHIN: Duration = Duration::from_secs(10);

/// Predecessors an owner at `rank` may ask under the previous membership
/// ([`WalkConfig::previous`]): bit `c` is set when canonical rank `c` <
/// `rank` names the same node in `previous` and `current`. Such a request
/// carries the same authority under either version, and a predecessor that
/// has not adopted the current version yet still resolves the previous one,
/// so it serves (or, at rank 0, fetches for everyone) instead of answering
/// `StaleMembership` and leaving the page to this owner (Known issue 12).
/// The caller sets nothing unless it replaced the previous membership at
/// most [`PREVIOUS_MEMBERSHIP_WITHIN`] ago and is a member of it.
pub fn same_rank_previous<N: PartialEq>(previous: &[N], current: &[N], rank: usize) -> u8 {
    (0..rank.min(current.len()).min(8))
        .filter(|c| previous.get(*c).is_some_and(|p| *p == current[*c]))
        .fold(0, |bits, c| bits | (1 << c))
}

/// Which previous-membership owners a page owner should ask before origin.
///
/// `previous` and `current` are the page's ranked owners under the previous
/// and current membership; `rank` is this node's current rank. Returns the
/// previous owners in previous rank order, without this node and without the
/// current predecessors (the walk asks those anyway). Empty when the owner
/// set did not change, so a steady cold miss pays no extra probe.
pub fn handoff_targets<N: PartialEq + Clone>(previous: &[N], current: &[N], rank: usize) -> Vec<N> {
    let same = previous.len() == current.len() && previous.iter().all(|n| current.contains(n));
    if same {
        return Vec::new();
    }
    let me = current.get(rank);
    previous
        .iter()
        .filter(|n| Some(*n) != me && !current[..rank.min(current.len())].contains(n))
        .take(MAX_HANDOFF)
        .cloned()
        .collect()
}

/// Lineage handoff targets of the owner at `rank` (Known issue 25): later
/// owners (`k > rank`) that arrived in this node's membership chain before
/// that owner (`since[k] < since[rank]`, see `topology::Membership::since`).
///
/// Placement is rendezvous hashing, so a member ranks below the owner at
/// `rank` in every membership listing both. Such a later owner could have
/// held a higher rank, and fetched the page, only while that owner was not
/// a member yet: a page that moved to a member that joined is still held by
/// the owner it displaced, however many versions ago that was, while the
/// version - 1 [`handoff_targets`] see only the last move. Members with
/// equal or unknown arrival never qualify, so a cluster whose members
/// arrived together pays nothing. A target can only miss, so it costs one
/// copy-only probe at most, funded opportunistically like other handoff
/// probes.
pub fn lineage_targets<N: Clone>(owners: &[N], since: &[Option<u32>], rank: usize) -> Vec<N> {
    let Some(Some(mine)) = since.get(rank).copied() else {
        return Vec::new();
    };
    owners
        .iter()
        .zip(since)
        .skip(rank + 1)
        .filter(|(_, s)| s.is_some_and(|s| s < mine))
        .map(|(n, _)| n.clone())
        .take(MAX_HANDOFF)
        .collect()
}

/// Lineage targets ranked below the owners (Known issue 34): members of
/// `demoted` (the ranks just below the owners, best first, see
/// `topology::Placement::demoted`) that arrived before the owner at `rank`
/// and were owners when they arrived, so they may still hold a copy fetched
/// then. `since` covers the owners, `demoted_since` the demoted members.
///
/// Rendezvous scores never change, so a member ranked `k` is an owner
/// whenever fewer than [`MAX_HANDOFF`] of the members ranked above it are
/// present. It was one when it arrived (generation `s`) if fewer than
/// [`MAX_HANDOFF`] members above it have an arrival at or before `s`.
/// Members with an unknown arrival count as present, so an unknown never
/// adds a target. Members that left since are not seen, which can only
/// make a member look like it was an owner when it was not: the probe then
/// misses, costing one granted copy-only exchange.
pub fn demoted_targets<N: Clone>(
    since: &[Option<u32>],
    demoted: &[N],
    demoted_since: &[Option<u32>],
    rank: usize,
) -> Vec<N> {
    demoted_targets_from(since, demoted, demoted_since, rank, None)
}

/// [`demoted_targets`], plus members whose arrival is `oldest`, the
/// generation this node's membership chain began at (production
/// `Membership::since` 0): their arrival is unknown, only that they are at
/// least as old as the chain, so no arrival rule can tell whether one of
/// them was an owner before the chain began (Known issue 46). In 5k
/// `full-churn` most holders left unasked by [`demoted_targets`] were such
/// members, tied at the chain's start with the owners above them. When this
/// owner arrived after the chain began, they qualify too, in rank order:
/// a member ranked higher is an owner whenever fewer members above it are
/// present, so it is the likelier holder. At most [`MAX_LINEAGE`] in all,
/// each a granted copy-only probe that can only miss.
pub fn demoted_targets_from<N: Clone>(
    since: &[Option<u32>],
    demoted: &[N],
    demoted_since: &[Option<u32>],
    rank: usize,
    oldest: Option<u32>,
) -> Vec<N> {
    let Some(Some(mine)) = since.get(rank).copied() else {
        return Vec::new();
    };
    let above = |k: usize, arrival: u32| {
        since
            .iter()
            .chain(&demoted_since[..k])
            .filter(|s| s.is_none_or(|s| s <= arrival))
            .count()
    };
    demoted
        .iter()
        .zip(demoted_since)
        .enumerate()
        .filter_map(|(k, (n, s))| {
            let arrival = (*s)?;
            let unknown = oldest == Some(arrival);
            (arrival < mine && (unknown || above(k, arrival) < MAX_HANDOFF)).then(|| n.clone())
        })
        .take(MAX_LINEAGE)
        .collect()
}

/// How many leading `handoff` targets are in `granted` (successor and
/// lineage targets), for [`WalkConfig::granted`]. Callers order the
/// handoff targets so these come first ([`merge_handoff`],
/// [`crate::read::successor::with_successors`]); version - 1 targets after
/// them stay funded from the walk's budget.
pub fn granted_prefix<N: PartialEq>(handoff: &[N], granted: &[N]) -> usize {
    handoff.iter().take_while(|n| granted.contains(n)).count()
}

/// [`lineage_targets`] counts for every canonical owner rank, for
/// [`WalkConfig::lineage`].
pub fn lineage_counts(since: &[Option<u32>]) -> [u8; MAX_HANDOFF] {
    let mut counts = [0; MAX_HANDOFF];
    for (rank, count) in counts.iter_mut().enumerate() {
        if let Some(Some(mine)) = since.get(rank) {
            *count = since[rank + 1..]
                .iter()
                .filter(|s| s.is_some_and(|s| s < *mine))
                .count()
                .min(MAX_HANDOFF) as u8;
        }
    }
    counts
}

/// The handoff targets an owner asks: lineage targets ([`lineage_targets`],
/// [`demoted_targets`], at most [`MAX_LINEAGE`]) first, then the version - 1
/// targets ([`handoff_targets`]) not already listed while fewer than
/// [`MAX_HANDOFF`] targets are listed (the walk funds those from its own
/// budget, so their number does not grow with the lineage cap). Lineage targets come first: each was an
/// owner above this one before it arrived, so it fetched the page if anyone
/// read it then, while a version - 1 owner that was displaced since may
/// never have been asked for it.
pub fn merge_handoff<N: PartialEq + Clone>(previous: Vec<N>, lineage: Vec<N>) -> Vec<N> {
    let mut targets = lineage;
    targets.truncate(MAX_LINEAGE);
    for n in previous {
        if targets.len() >= MAX_HANDOFF {
            break;
        }
        if !targets.contains(&n) {
            targets.push(n);
        }
    }
    targets
}

/// One probe the caller must perform.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Probe {
    /// Position in the visit order.
    pub index: usize,
    /// Destination's canonical rank.
    pub canonical: usize,
    pub mode: Mode,
    /// Remaining opportunities in visit order including this one; owners also
    /// reserve a share for origin.
    pub remaining_opportunities: u32,
    pub route: RouteMode,
    /// Send under membership version - 1 ([`WalkConfig::previous`]). The
    /// caller routes and signs the request under that membership.
    pub previous: bool,
}

/// Outcome the caller reports for the current probe.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Outcome {
    /// A validated, usable copy.
    Copy,
    /// A classified recoverable response or payload-validation outcome.
    Probe(ProbeOutcome),
    /// A recoverable transport failure (see [`transport_outcome`]).
    Transport(ProbeOutcome),
    /// The peer reported a stale membership epoch and the caller will refresh
    /// (see [`stale_action`]).
    StaleMembership,
    /// The peer reported a stale membership epoch, but the caller holds no
    /// newer membership to refresh to (or already refreshed once): the
    /// destination is behind. The walk records transient evidence and moves
    /// on to the next owner without charging a route failure; the route
    /// itself worked.
    StaleDestination,
    /// The destination itself answered `Overloaded` (signed by the
    /// destination, not by a relay in front of it): for example an owner
    /// shedding readers of a hot page
    /// ([`super::fanout::shed_hot_page`]). Recorded like
    /// `Probe(Overloaded)` but without charging a route failure (the route
    /// worked); for a non-owner page walk it is also the one signal that asks
    /// the page's overflow helper.
    Shed,
    /// The request was sent but no answer arrived before the attempt's own
    /// time bound (its idle share or the attempt timeout) while the caller's
    /// scope still runs: the destination may still be working on it.
    /// Recorded like `Transport(Unreachable)`, except for an owner's page
    /// probe of rank 0: a delegated `Acquire` waits on rank 0's acquisition, and a
    /// copy-only probe answers at once unless it joined rank 0's running
    /// acquisition (production `Flights::join_copy`). Either way rank 0 is
    /// most likely still fetching (its origin request queued behind others),
    /// so the timeout is not origin authority for this owner, whose own
    /// request would only queue behind rank 0's at the same origin (Known
    /// issue 31). Such a walk ends [`End::Exhausted`] instead of
    /// [`End::Origin`]; a transport failure (rank 0 down or unreachable) or
    /// a miss still lets the owner fetch. [`WalkConfig::rank0`] bounds the
    /// rule: once rank 0 stayed silent too long (`Overdue`) a timeout gives
    /// authority again (Known issue 39).
    TimedOut,
    /// Rank 0 itself (the response is signed by the destination, not by a
    /// relay) answered an owner's page probe of it `Unavailable`: its own
    /// acquisition of the page failed. Callers report it only for
    /// [`CandidateWalk::pending_rank0`] probes; anywhere else it is
    /// recorded as `Probe(Unreachable)`.
    ///
    /// Recorded like `Probe(Unreachable)`, except that when this owner's
    /// own origin request for the page failed recently
    /// ([`WalkConfig::rank0`] is `Failed`, see
    /// [`crate::read::rank0::Rank0Watch::failed`]) it withholds origin
    /// authority like [`Outcome::TimedOut`] (Known issue 52): origin is
    /// failing for the page, this owner's request would go to it right
    /// after rank 0's failed, and the reader's retry sends rank 0 to origin
    /// again anyway.
    Failed,
}

/// How the epoch ended.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum End {
    /// The copy reported by the last probe wins.
    Copy,
    /// The caller must refresh membership and start a new walk.
    StaleMembership,
    /// This owner may contact origin with the collected evidence.
    Origin {
        rank: usize,
        evidence: Vec<ProbeOutcome>,
    },
    /// All visited owners authoritatively lacked the version.
    VersionUnavailable,
    /// Exhausted without authority; `funding` is true when a probe could not
    /// be funded.
    Exhausted { funding: bool },
}

/// One step of the walk.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Step {
    Probe(Probe),
    /// Granted probes to send at once ([`WalkConfig::parallel_granted`]),
    /// each copy-only on its own [`granted_budget`]. Report them together
    /// with [`CandidateWalk::complete_granted`] once all have answered.
    Granted(Vec<Probe>),
    /// No probes remain. The caller still checks its scope before acting.
    End(End),
}

/// Sans-IO candidate walk state.
pub struct CandidateWalk {
    config: WalkConfig,
    order: VisitOrder,
    /// Owner walks: predecessors then handoff targets (`owners + i`).
    targets: [u8; 2 + MAX_TARGETS],
    count: usize,
    /// Granted probes sent with [`Step::Granted`] and not yet reported.
    batch: usize,
    index: usize,
    evidence: Vec<ProbeOutcome>,
    saw_transient: bool,
    saw_version: bool,
    funding: bool,
    pending: Option<Pending>,
    done: bool,
    helper: Helper,
    /// An owner's probe of rank 0 timed out ([`Outcome::TimedOut`]): the
    /// walk does not end with origin authority.
    rank0_unanswered: bool,
}

/// Overflow helper state of a non-owner page walk.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Helper {
    /// Not decided yet (before the first `next`).
    Unset,
    /// No helper for this walk.
    Off,
    /// Eligible; no owner probe has failed transiently yet.
    Armed,
    /// An owner failed transiently: the next probe asks the helper.
    Due,
    /// Asked (or disarmed).
    Used,
}

struct Pending {
    bounded: Option<AcquisitionBudget>,
    /// The probe asks a previous-membership owner; its outcome is not
    /// predecessor evidence.
    handoff: bool,
    /// An owner's `Acquire` to rank 0.
    delegated: bool,
    /// An owner's page probe of rank 0 (delegated `Acquire` or copy-only,
    /// under either membership), not a handoff probe.
    rank0: bool,
    /// Sent under the previous membership ([`WalkConfig::previous`]).
    previous: bool,
    /// The probe asks the overflow helper.
    helper: bool,
    /// `bounded` is a successor grant: dropped, never reunited.
    granted: bool,
}

impl CandidateWalk {
    pub fn new(config: WalkConfig) -> Self {
        let mut targets = [0; 2 + MAX_TARGETS];
        let mut count = config.rank.unwrap_or(config.owners);
        let order = if let Some(rank) = config.rank {
            let rank = rank.min(2);
            count = rank;
            for (i, target) in targets.iter_mut().enumerate().take(rank) {
                *target = i as u8;
            }
            if config.page_operation {
                for i in 0..config.handoff.min(MAX_TARGETS) {
                    targets[count] = (config.owners + i) as u8;
                    count += 1;
                }
            }
            VisitOrder::canonical(rank)
        } else {
            config.order
        };
        Self {
            config,
            order,
            targets,
            count,
            batch: 0,
            index: 0,
            evidence: Vec::new(),
            saw_transient: false,
            saw_version: false,
            funding: false,
            pending: None,
            done: false,
            helper: Helper::Unset,
            rank0_unanswered: false,
        }
    }

    /// Decide once, from the starting budget, whether this walk may ask an
    /// overflow helper. A non-owner that cannot fund its first out-of-order
    /// owner at all (which would end the walk on funding before any probe)
    /// visits the owners canonically instead, starting at rank 0; a helper's
    /// small inherited budget can take this path.
    fn start(&mut self, budget: &AcquisitionBudget) {
        let eligible = self.config.helper
            && self.config.rank.is_none()
            && self.config.page_operation
            && budget.remaining_attempts() >= HELPER_MIN_ATTEMPTS;
        self.helper = if eligible { Helper::Armed } else { Helper::Off };
        if self.config.rank.is_none()
            && !self.config.continuation.bounded_routes
            && !self.config.continuation.primary_consumed
            && self.order.get(0).is_some_and(|first| {
                first > 0 && budget.remaining_attempts() < owner_funding_min(first)
            })
        {
            let len = self.order.ranks().len();
            self.order = VisitOrder::canonical(len);
        }
    }

    /// An owner (or a relay in front of it) shed this walk's request or timed
    /// out: ask the page's helper to acquire it, funded from what is left.
    fn helper_probe(&mut self, budget: &mut AcquisitionBudget) -> Result<Step> {
        let attempts = budget.remaining_attempts().min(HELPER_ATTEMPTS);
        let links = budget.remaining_links().min(HELPER_LINKS);
        let child = budget.partition(attempts, links)?;
        self.pending = Some(Pending {
            bounded: Some(child),
            handoff: true,
            delegated: false,
            rank0: false,
            helper: true,
            previous: false,
            granted: false,
        });
        Ok(Step::Probe(Probe {
            index: self.index,
            canonical: self.config.owners,
            mode: Mode::Acquire,
            remaining_opportunities: (self.count.saturating_sub(self.index) + 1) as u32,
            route: RouteMode::FundedRoute,
            previous: false,
        }))
    }

    /// Whether the pending probe is a granted successor probe
    /// ([`WalkConfig::successors`]).
    pub fn pending_successor(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.granted)
    }

    /// Whether the pending probe asks the overflow helper.
    pub fn pending_helper(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.helper)
    }

    /// Credits the funded visits after `index` need at least: minimum
    /// funding for each out-of-order owner (every owner on bounded routes).
    /// Shared visits take a share of whatever is left.
    fn later_minimum(&self, index: usize) -> u32 {
        (index + 1..self.count)
            .filter_map(|later| self.target(later).map(|c| (later, c)))
            .filter(|(later, c)| *c > *later || self.config.continuation.bounded_routes)
            .map(|(_, c)| owner_funding_min(c))
            .sum()
    }

    /// What every later visit needs to still reach the origin through its
    /// owner, in order or not ([`owner_funding_min`]): lineage credits
    /// (Known issue 25) are slack beyond this, so they never cost a later
    /// owner its visit.
    fn later_reserve(&self, index: usize) -> u32 {
        (index + 1..self.count)
            .filter_map(|later| self.target(later))
            .map(owner_funding_min)
            .sum()
    }

    /// Canonical rank (or `owners + i` for handoff target `i`) at `index`.
    fn target(&self, index: usize) -> Option<usize> {
        if self.config.rank.is_some() {
            (index < self.count).then(|| usize::from(self.targets[index]))
        } else {
            self.order.get(index)
        }
    }

    /// Whether the pending probe asks a previous-membership owner (handoff
    /// target). A `StaleMembership` answer from one is only a miss: callers
    /// must neither refresh membership nor skip it as a stale destination.
    pub fn pending_handoff(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.handoff)
    }

    /// Whether the pending probe is sent under the previous membership. A
    /// `StaleMembership` answer to it (the destination no longer resolves
    /// that version) must not refresh this walk's membership: report
    /// [`Outcome::StaleMembership`] and the walk records transient evidence,
    /// as for a destination that is behind.
    pub fn pending_previous(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.previous)
    }

    /// Whether rank 0 left this owner's probe unanswered
    /// ([`Outcome::TimedOut`]) or answered that its own acquisition failed
    /// ([`Outcome::Failed`]), so the walk will not end with origin
    /// authority.
    pub fn rank0_unanswered(&self) -> bool {
        self.rank0_unanswered
    }

    /// The pending probe is this owner's page probe of rank 0: the caller
    /// updates its [`crate::read::rank0::Rank0Watch`] from the outcome.
    pub fn pending_rank0(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.rank0)
    }

    /// Whether the walk stopped because a probe could not be funded.
    pub fn funding_stopped(&self) -> bool {
        self.funding
    }

    /// Evidence collected so far, in probe order.
    pub fn evidence(&self) -> &[ProbeOutcome] {
        &self.evidence
    }

    /// Plan the next probe, partitioning a bounded child budget when needed.
    pub fn next(&mut self, budget: &mut AcquisitionBudget) -> Result<Step> {
        assert!(self.pending.is_none(), "complete the pending probe first");
        assert!(self.batch == 0, "complete the granted probes first");
        assert!(!self.done, "walk already ended");
        if self.helper == Helper::Unset {
            self.start(budget);
        }
        if self.helper == Helper::Due {
            self.helper = Helper::Used;
            if budget.remaining_attempts() >= HELPER_FLOOR_ATTEMPTS
                && budget.remaining_links() >= HELPER_FLOOR_LINKS
            {
                return self.helper_probe(budget);
            }
        }
        let rank = self.config.rank;
        let continuation = self.config.continuation;
        while let Some(canonical) = self.target(self.index) {
            let index = self.index;
            let handoff = canonical >= self.config.owners;
            let granted = handoff && canonical - self.config.owners < self.config.granted;
            if granted && self.config.parallel_granted {
                // Every granted target from here on, at once (Known issue
                // 45): one time share for the run, each on its own grant.
                let remaining_opportunities =
                    (self.count - index + 1) as u32 + SUCCESSOR_SHARE_DIVISOR;
                let mut probes = Vec::new();
                while let Some(canonical) = self.target(self.index)
                    && canonical >= self.config.owners
                    && canonical - self.config.owners < self.config.granted
                {
                    probes.push(Probe {
                        index: self.index,
                        canonical,
                        mode: Mode::CopyOnly,
                        remaining_opportunities,
                        route: RouteMode::FundedRoute,
                        previous: false,
                    });
                    self.index += 1;
                }
                self.batch = probes.len();
                return Ok(Step::Granted(probes));
            }
            if granted {
                // A successor probe spends its own grant, never the walk's
                // credits.
                let child = AcquisitionBudget::new(
                    budget.deadline(),
                    SUCCESSOR_GRANT_ATTEMPTS,
                    SUCCESSOR_GRANT_LINKS,
                );
                self.pending = Some(Pending {
                    bounded: Some(child),
                    handoff: true,
                    delegated: false,
                    rank0: false,
                    previous: false,
                    helper: false,
                    granted: true,
                });
                return Ok(Step::Probe(Probe {
                    index,
                    canonical,
                    mode: Mode::CopyOnly,
                    // A short time share: the requester waiting on this
                    // owner must not time out while it asks a successor.
                    remaining_opportunities: (self.count - index + 1) as u32
                        + SUCCESSOR_SHARE_DIVISOR,
                    route: RouteMode::FundedRoute,
                    previous: false,
                }));
            }
            // Credits the delegation must leave: one per later predecessor
            // probe and one for this owner's own origin attempt. Handoff
            // probes are opportunistic and never reserve credits, so they
            // cannot turn a delegation into a local fetch (Known issue 13).
            let left = rank.map_or(0, |rank| rank.min(2).saturating_sub(index)) as u32;
            if handoff && (budget.remaining_attempts() < 2 || budget.remaining_links() == 0) {
                // Handoff probes are opportunistic: never spend the credit
                // the owner's own origin attempt needs.
                break;
            }
            // A predecessor that held the same rank under the previous
            // membership is asked under it (Known issue 12).
            let previous = !handoff
                && rank.is_some()
                && self.config.page_operation
                && canonical < 8
                && self.config.previous & (1 << canonical) != 0;
            // An owner above rank 0 asks rank 0 to acquire, so one owner per
            // membership fetches a cold page and concurrent misses coalesce in
            // rank 0's flight. The Acquire costs two credits (the exchange and
            // rank 0's origin credit); the owner keeps one per later probe
            // and one for its own origin attempt if rank 0 fails
            // ([`owner_funding`] provides them).
            let delegated = rank.is_some_and(|rank| rank > 0)
                && canonical == 0
                && self.config.page_operation
                && budget.remaining_attempts() >= left + 2
                && u32::from(budget.remaining_links()) >= left;
            if continuation.primary_consumed && index == 0 {
                let outcome = continuation
                    .primary_outcome
                    .unwrap_or(ProbeOutcome::Unreachable);
                self.saw_version |= outcome == ProbeOutcome::VersionUnavailable;
                self.saw_transient |= transient(outcome);
                self.index += 1;
                continue;
            }
            // With only the sender attempt left, a page Acquire would carry
            // zero remote credits and reject before checking disk/pending copies.
            // Spend the same last route on local copies, never new acquisition.
            let copy_only = (rank.is_some() && !delegated)
                || (!continuation.bounded_routes
                    && budget.remaining_attempts() == 1
                    && self.config.page_operation);
            let mode = if copy_only {
                Mode::CopyOnly
            } else {
                Mode::Acquire
            };
            let count = self.count;
            // When this owner sees an ownership change (it has handoff
            // targets), rank 0 has previous owners to ask too, but an
            // Acquire that carries only its origin credit leaves rank 0
            // none to ask them with (Known issue 13). Forward spare credits,
            // up to one per previous owner: they would otherwise fund this
            // owner's own handoff probes, which only run if rank 0 fails.
            // Rank 0's own lineage targets (Known issue 25) are funded the
            // same way.
            let spare_for = if self.config.handoff > 0 {
                MAX_HANDOFF as u32
            } else {
                u32::from(self.config.lineage[0])
            };
            let spare = if delegated && spare_for > 0 {
                (budget.remaining_attempts() - (left + 2)).min(spare_for)
            } else {
                0
            };
            let owner_attempts = 1 + u32::from(delegated) + spare;
            // Probes that share the owner's inherited links with this one:
            // the remaining predecessors, or the remaining handoff targets.
            // Opportunistic handoff probes never shrink a predecessor's
            // route; they get whatever links the predecessors left.
            let sharing = if handoff {
                1
            } else {
                rank.map_or(count, |rank| rank.min(2))
                    .saturating_sub(index)
                    .max(1)
            };
            let bounded = if delegated {
                // The exchange and rank 0's origin credit: a funded route
                // forwards all of it, and this owner keeps what later probes
                // and its own origin attempt need. The delegation decides
                // whether rank 0 fetches for everyone, so its route comes
                // first: later copy-only predecessors (which matter only if
                // rank 0 fails) keep one link each, not an equal share
                // (an equal share left rank 2 too few links to reach rank 0
                // and made it fetch itself; Known issue 13).
                let links = budget
                    .route_links()
                    .min(budget.remaining_links().saturating_sub(sharing as u8 - 1))
                    .max(1);
                Some(budget.partition(owner_attempts, links)?)
            } else if rank.is_some()
                && u16::from(budget.remaining_links())
                    < sharing as u16 * u16::from(budget.route_links())
            {
                // Receiver CopyOnly predecessor probes share the inherited route
                // pool; the first must not consume every link after an incoming
                // eight-link envelope selected failure routing.
                let remaining = sharing as u8;
                let links = if canonical == 0 && !handoff && self.config.page_operation {
                    // A copy-only probe of rank 0 decides, like a
                    // delegation, whether this owner leaves the fetch to
                    // rank 0 (which joins it to its running acquisition,
                    // Known issue 31): later predecessors keep one link each.
                    // An equal share left rank 2 too few links to route to
                    // rank 0, and a route failure gives origin authority
                    // (Known issue 40).
                    budget
                        .route_links()
                        .min(budget.remaining_links().saturating_sub(remaining - 1))
                        .max(1)
                } else {
                    (budget.remaining_links() / remaining).min(4)
                };
                Some(budget.partition(owner_attempts, links)?)
            } else if continuation.bounded_routes
                || (rank.is_none()
                    && !copy_only
                    && canonical > index
                    && budget.remaining_attempts() >= owner_funding_min(canonical)
                    && budget.remaining_links() >= 8)
            {
                // Fund by the destination's canonical rank: it probes all of its
                // predecessors, wherever it falls in this visit.
                // Prefer the funding that lets the owner delegate to rank 0,
                // but only from slack: later visits keep the credits they had
                // before delegation, so the walk's reach does not shrink.
                // One more credit per lineage target of the destination and,
                // above rank 0, of rank 0, which the destination delegates
                // to and forwards spare credits for (Known issue 25); only
                // from slack as well.
                let lineage = self
                    .config
                    .lineage
                    .get(canonical)
                    .map_or(0, |n| u32::from(*n))
                    + if canonical > 0 {
                        u32::from(self.config.lineage[0])
                    } else {
                        0
                    };
                let attempts = if rank.is_some() {
                    owner_attempts
                } else if lineage > 0
                    && budget.remaining_attempts()
                        >= owner_funding(canonical) + lineage + self.later_reserve(index)
                {
                    owner_funding(canonical) + lineage
                } else if budget.remaining_attempts()
                    >= owner_funding(canonical) + self.later_minimum(index)
                {
                    owner_funding(canonical)
                } else {
                    owner_funding_min(canonical)
                };
                let links = if rank.is_some() { 4 } else { 8 };
                if budget.remaining_attempts() < attempts || budget.remaining_links() < links {
                    // Never send an Acquire whose receiver cannot probe its
                    // predecessors and still exercise origin authority.
                    self.saw_transient = true;
                    self.funding = true;
                    break;
                }
                let mut child = budget.partition(attempts, links)?;
                if rank.is_none() {
                    child.note_route_failure();
                }
                Some(child)
            } else {
                None
            };
            let funded =
                continuation.bounded_routes || delegated || (rank.is_none() && bounded.is_some());
            self.pending = Some(Pending {
                bounded,
                handoff,
                delegated,
                rank0: rank.is_some() && canonical == 0 && !handoff && self.config.page_operation,
                previous,
                helper: false,
                granted: false,
            });
            return Ok(Step::Probe(Probe {
                index,
                canonical,
                mode,
                remaining_opportunities: (count - index + usize::from(rank.is_some())) as u32,
                route: if funded {
                    RouteMode::FundedRoute
                } else {
                    RouteMode::SharedRoute
                },
                previous,
            }));
        }
        self.done = true;
        Ok(Step::End(self.finish()))
    }

    /// Budget the pending probe spends: its bounded child, or the caller's.
    pub fn request_budget<'a>(
        &'a mut self,
        budget: &'a mut AcquisitionBudget,
    ) -> &'a mut AcquisitionBudget {
        match self.pending.as_mut().and_then(|p| p.bounded.as_mut()) {
            Some(bounded) => bounded,
            None => budget,
        }
    }

    /// Report the granted probes of the last [`Step::Granted`]: whether any
    /// of them returned a validated, usable copy. Every other outcome
    /// (miss, failure, timeout, stale or shed answer) is only a miss, and the
    /// grants are dropped, so the walk's budget and evidence are as before.
    pub fn complete_granted(&mut self, copy: bool) -> Option<End> {
        assert!(self.batch > 0, "no granted probes outstanding");
        self.batch = 0;
        copy.then(|| {
            self.done = true;
            End::Copy
        })
    }

    /// Return the pending probe's child credits without recording an outcome,
    /// for a probe that failed terminally.
    pub fn abandon(&mut self, budget: &mut AcquisitionBudget) -> Result<()> {
        self.done = true;
        self.batch = 0;
        if let Some(Pending {
            bounded: Some(bounded),
            granted: false,
            ..
        }) = self.pending.take()
        {
            budget.reunite(bounded)?;
        }
        Ok(())
    }

    /// The pending probe's destination is behind this node and the caller
    /// waits for it ([`super::stale::stale_wait`], Known issue 26): return
    /// the child credits and plan the same destination again on the next
    /// [`Self::next`], instead of recording an outcome and moving on.
    pub fn retry_pending(&mut self, budget: &mut AcquisitionBudget) -> Result<()> {
        if let Some(Pending {
            bounded: Some(bounded),
            ..
        }) = self.pending.take()
        {
            budget.reunite(bounded)?;
        }
        Ok(())
    }

    /// Whether asking the pending destination once more (one more exchange,
    /// after [`Self::retry_pending`]) still leaves the walk enough attempts
    /// to fund that visit and every later one at its minimum
    /// ([`owner_funding_min`]), and links for a full route share
    /// ([`AcquisitionBudget::route_links`]) to each. A read's further waits
    /// for a destination that is behind take this (Known issue 36), so
    /// waiting never starves a later owner that may already be current.
    pub fn retry_affordable(&self, budget: &AcquisitionBudget) -> bool {
        // The pending probe's child holds its credits and its route share.
        let (attempts, links, route) = match &self.pending {
            Some(Pending {
                bounded: Some(b),
                granted: false,
                ..
            }) => (b.remaining_attempts(), b.remaining_links(), b.route_links()),
            _ => (0, 0, 0),
        };
        let attempts = budget.remaining_attempts().saturating_add(attempts);
        let links = u32::from(budget.remaining_links()) + u32::from(links);
        let later = (self.index + 1..self.count)
            .filter(|i| self.target(*i).is_some())
            .count() as u32;
        let need = 1
            + self.target(self.index).map_or(0, owner_funding_min)
            + self.later_reserve(self.index);
        let route = u32::from(route.max(budget.route_links()).max(1));
        attempts >= need && links >= route * (1 + later)
    }

    /// Record the pending probe's outcome. Returns an end when the walk stops
    /// early (copy or stale membership).
    pub fn complete(
        &mut self,
        budget: &mut AcquisitionBudget,
        outcome: Outcome,
    ) -> Result<Option<End>> {
        let pending = self.pending.take().expect("no pending probe");
        if let Some(bounded) = pending.bounded
            && !pending.granted
        {
            budget.reunite(bounded)?;
        }
        let outcome = match outcome {
            Outcome::TimedOut => {
                self.rank0_unanswered |= pending.rank0 && self.config.rank0 != Rank0State::Overdue;
                Outcome::Transport(ProbeOutcome::Unreachable)
            }
            Outcome::Failed => {
                self.rank0_unanswered |= pending.rank0 && self.config.rank0 == Rank0State::Failed;
                Outcome::Probe(ProbeOutcome::Unreachable)
            }
            outcome => outcome,
        };
        if pending.helper {
            // The helper does not consume a visit-order position. Its
            // failure is only a miss (handled with handoff probes below).
            return match outcome {
                Outcome::Copy => {
                    self.done = true;
                    Ok(Some(End::Copy))
                }
                Outcome::Probe(o) | Outcome::Transport(o)
                    if matches!(o, ProbeOutcome::Unreachable | ProbeOutcome::Overloaded) =>
                {
                    budget.note_route_failure();
                    Ok(None)
                }
                _ => Ok(None),
            };
        }
        self.index += 1;
        if self.helper == Helper::Armed && outcome == Outcome::Shed {
            self.helper = Helper::Due;
        }
        match outcome {
            Outcome::Probe(ProbeOutcome::VersionUnavailable) if pending.delegated => {
                // Rank 0 asked origin for this version and was told it does
                // not exist; asking again would only repeat that.
                self.done = true;
                Ok(Some(End::VersionUnavailable))
            }
            // A previous owner that lags or leads this membership is only a
            // missing copy: never refresh the owner's walk for it.
            Outcome::StaleMembership | Outcome::StaleDestination | Outcome::Shed
                if pending.handoff =>
            {
                Ok(None)
            }
            Outcome::Probe(outcome) | Outcome::Transport(outcome) if pending.handoff => {
                if !pending.granted
                    && matches!(
                        outcome,
                        ProbeOutcome::Unreachable | ProbeOutcome::Overloaded
                    )
                {
                    budget.note_route_failure();
                }
                Ok(None)
            }
            Outcome::StaleDestination | Outcome::StaleMembership if pending.previous => {
                // The destination no longer resolves the previous
                // membership: transient evidence, like a destination that
                // is behind.
                self.saw_transient = true;
                self.evidence.push(ProbeOutcome::Unreachable);
                Ok(None)
            }
            Outcome::Copy => {
                self.done = true;
                Ok(Some(End::Copy))
            }
            Outcome::StaleMembership => {
                self.done = true;
                Ok(Some(End::StaleMembership))
            }
            Outcome::StaleDestination => {
                self.saw_transient = true;
                self.evidence.push(ProbeOutcome::Unreachable);
                Ok(None)
            }
            // The route worked; the destination is busy.
            Outcome::Shed => {
                self.saw_transient = true;
                self.evidence.push(ProbeOutcome::Overloaded);
                Ok(None)
            }
            // Normalized above.
            Outcome::TimedOut | Outcome::Failed => Ok(None),
            Outcome::Probe(outcome) | Outcome::Transport(outcome) => {
                self.saw_version |= outcome == ProbeOutcome::VersionUnavailable;
                self.saw_transient |= transient(outcome);
                self.evidence.push(outcome);
                if matches!(
                    outcome,
                    ProbeOutcome::Unreachable | ProbeOutcome::Overloaded
                ) {
                    budget.note_route_failure();
                }
                Ok(None)
            }
        }
    }

    fn finish(&mut self) -> End {
        if self.config.rank.is_some() && self.rank0_unanswered {
            // Rank 0 has not answered this owner's probe within the
            // attempt: leave the fetch to it (Known issue 31). Callers retry
            // and join rank 0's acquisition.
            End::Exhausted { funding: false }
        } else if let Some(rank) = self.config.rank {
            End::Origin {
                rank,
                evidence: std::mem::take(&mut self.evidence),
            }
        } else if self.saw_version && !self.saw_transient {
            End::VersionUnavailable
        } else {
            End::Exhausted {
                funding: self.funding,
            }
        }
    }
}

/// After an origin version miss, old copies on later candidates remain useful.
/// This walk probes the owners after `rank` copy-only and never asks another
/// node to start acquisition.
pub struct RemainingWalk {
    owners: usize,
    index: usize,
    transient: bool,
    pending: bool,
}

/// How a [`RemainingWalk`] ended.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RemainingEnd {
    Copy,
    /// No later owner had a copy.
    None,
    /// Some later owner was unreachable or unusable.
    Unavailable,
}

impl RemainingWalk {
    pub fn new(rank: usize, owners: usize) -> Self {
        Self {
            owners,
            index: rank + 1,
            transient: false,
            pending: false,
        }
    }

    /// The next copy-only probe, or the end.
    pub fn next_probe(&mut self) -> std::result::Result<Probe, RemainingEnd> {
        assert!(!self.pending, "complete the pending probe first");
        if self.index >= self.owners {
            return Err(if self.transient {
                RemainingEnd::Unavailable
            } else {
                RemainingEnd::None
            });
        }
        self.pending = true;
        Ok(Probe {
            index: self.index,
            canonical: self.index,
            mode: Mode::CopyOnly,
            remaining_opportunities: (self.owners - self.index) as u32,
            route: RouteMode::SharedRoute,
            previous: false,
        })
    }

    /// Record the pending probe; `Some(Copy)` ends the walk.
    pub fn complete(
        &mut self,
        budget: &mut AcquisitionBudget,
        outcome: Outcome,
    ) -> Option<RemainingEnd> {
        assert!(self.pending, "no pending probe");
        self.pending = false;
        self.index += 1;
        match outcome {
            Outcome::Copy => return Some(RemainingEnd::Copy),
            Outcome::Probe(outcome) => self.transient |= transient(outcome),
            Outcome::Shed | Outcome::Failed => self.transient = true,
            Outcome::Transport(_) | Outcome::TimedOut => {
                self.transient = true;
                budget.note_route_failure();
            }
            Outcome::StaleMembership | Outcome::StaleDestination => self.transient = true,
        }
        None
    }
}

fn transient(outcome: ProbeOutcome) -> bool {
    matches!(
        outcome,
        ProbeOutcome::Unreachable | ProbeOutcome::Overloaded | ProbeOutcome::UnusableCopy
    )
}

/// Map a probe transport error to walk evidence. `None` means the error is
/// terminal and must propagate.
///
/// `HopBudgetExhausted` is local: [`plan_attempt`] or the local route search
/// found too few links left for this probe, before anything was sent. It is
/// evidence that this owner is unreachable within the budget, not a reason to
/// abandon the walk: later probes that cannot be funded fail the same way
/// without I/O, and the walk ends as `Exhausted` (a recoverable `Unavailable`)
/// or, at an owner, with origin authority. Budgets never grow.
pub fn transport_outcome(error: Error) -> Option<ProbeOutcome> {
    match error {
        Error::Unavailable | Error::Io | Error::Os(_) | Error::HopBudgetExhausted => {
            Some(ProbeOutcome::Unreachable)
        }
        Error::Overloaded => Some(ProbeOutcome::Overloaded),
        _ => None,
    }
}

/// What to do after a peer answered `StaleMembership`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StaleAction {
    /// Refresh to the strictly newer local membership and start a new walk
    /// (complete the probe with [`Outcome::StaleMembership`]).
    Refresh,
    /// No newer membership is available locally, or this walk already
    /// refreshed once: the destination is behind this node. Complete the
    /// probe with [`Outcome::StaleDestination`] and try the next owner.
    Skip,
}

/// Decide how to handle a `StaleMembership` answer. Each epoch walk refreshes
/// at most once, and only to a strictly newer membership; otherwise the stale
/// destination is skipped like an unreachable one, so a requester that adopted
/// a publication before its destination still reaches the other owners.
pub fn stale_action(retried: bool, previous: u64, latest: Option<u64>) -> Result<StaleAction> {
    let latest = latest.ok_or(Error::Unavailable)?;
    Ok(if !retried && latest > previous {
        StaleAction::Refresh
    } else {
        StaleAction::Skip
    })
}

/// Times and credits for one peer exchange.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AttemptPlan {
    /// Links reserved for the whole route.
    pub links: u8,
    /// Signed hard ceiling; never renewed.
    pub overall: Instant,
    /// Fair-share idle deadline.
    pub share: Instant,
    /// Local nonrenewable exchange cap.
    pub attempt_end: Instant,
    /// Idle allowance for the attempt scope.
    pub idle: Duration,
    /// Remote acquisition credits granted to the destination.
    pub remote_attempts: u32,
    /// Known-body completion bound.
    pub complete_by: Instant,
    /// Known-body observation interval.
    pub observation: Duration,
}

/// Inputs to [`plan_attempt`] other than the budget.
#[derive(Clone, Copy, Debug)]
pub struct AttemptInputs {
    pub now: Instant,
    pub caller_deadline: Instant,
    pub remaining_opportunities: u32,
    pub route: RouteMode,
    pub mode: Mode,
    pub attempt_timeout: Duration,
    /// Links a failure route needs (`topology::FAILURE_LINKS`).
    pub failure_links: u8,
}

/// Reserve the complete permitted route and remote credits before sending.
/// Lost responses cannot refund an unknown number of forwarded links, and no
/// retry gets fresh credits. Remote credits stay spent unless a verified
/// `StaleMembership` shows they were never used (`AcquisitionBudget::refund_remote`).
pub fn plan_attempt(budget: &mut AcquisitionBudget, inputs: AttemptInputs) -> Result<AttemptPlan> {
    let links = budget.route_links();
    if links == 0 {
        return Err(Error::HopBudgetExhausted);
    }
    let now = inputs.now;
    let overall = budget.begin_peer_attempt(now, inputs.caller_deadline, links)?;
    let opportunities = inputs.remaining_opportunities.max(1);
    // Share bounds idle fallback. A separate nonrenewable local cap bounds every
    // exchange, even when no alternative is affordable.
    let share = now + (overall - now) / opportunities;
    let attempt_end = now
        + inputs
            .attempt_timeout
            .min(if inputs.route == RouteMode::DirectHedge {
                (overall - now) / 3
            } else {
                overall - now
            });
    let idle = (share - now).min(attempt_end - now);
    let remote_attempts = if inputs.mode == Mode::Acquire {
        let credits = if inputs.route == RouteMode::FundedRoute {
            budget.remaining_attempts()
        } else {
            budget.remaining_attempts().div_ceil(opportunities)
        };
        budget.send_remote(credits)?
    } else {
        0
    };
    let mut complete_by = attempt_end;
    if inputs.remaining_opportunities > 1
        && budget.remaining_attempts() > 0
        && budget.remaining_links() >= inputs.failure_links
    {
        // Reserve at most half the original post-share interval for fallback.
        let reserve = (share - now).min((overall - share) / 2);
        if !reserve.is_zero() {
            complete_by = complete_by.min(overall - reserve);
        }
    }
    let observation = (share - now)
        .min((attempt_end - now) / 3)
        .max(Duration::from_nanos(1));
    Ok(AttemptPlan {
        links,
        overall,
        share,
        attempt_end,
        idle,
        remote_attempts,
        complete_by,
        observation,
    })
}

/// Inputs to the speculative page hedge decision.
#[derive(Clone, Copy, Debug)]
pub struct HedgeFacts {
    pub page_operation: bool,
    pub is_candidate: bool,
    pub owners: usize,
    /// First two canonical ranks in visit order.
    pub first: usize,
    pub second: usize,
    /// The first two visited owners are distinct nodes.
    pub distinct: bool,
    pub remaining_attempts: u32,
    pub remaining_links: u8,
}

/// Whether to hedge before direct-link availability is checked. Only rank 0 can
/// serve as hedge primary, and one cold fallback must remain fundable:
/// primary(2) + duplicate(1) + the second visited owner at rank r
/// ([`owner_funding_min`]), with links 1 + 1 + 8.
pub fn hedge_eligible(facts: HedgeFacts) -> bool {
    facts.page_operation
        && !facts.is_candidate
        && facts.owners >= 2
        && facts.first == 0
        && facts.distinct
        && facts.remaining_attempts >= 3 + owner_funding_min(facts.second)
        && facts.remaining_links >= 10
}

/// Split the hedge's pinned direct budgets: (secondary, primary).
pub fn hedge_budgets(
    budget: &mut AcquisitionBudget,
) -> Result<(AcquisitionBudget, AcquisitionBudget)> {
    let secondary = budget.partition(1, 1)?;
    let primary = budget.partition(2, 1)?;
    Ok((secondary, primary))
}

/// Hedge race errors that fall back to the serial walk.
pub fn hedge_fallback(error: Error) -> bool {
    matches!(
        error,
        Error::Unavailable
            | Error::Io
            | Error::Os(_)
            | Error::Overloaded
            | Error::CorruptRecord
            | Error::MissingKey
            | Error::InvalidRequest
            | Error::HeaderTooLarge
    )
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests;
