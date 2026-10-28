//! Sans-IO acquisition of one page: [`Fill`] drives one [`CandidateWalk`]
//! per membership epoch, and the caller performs each [`Action`].

pub use crate::liveness::Rank0State;
use crate::read::budget::{AcquisitionBudget, MAX_ROUTE_LINKS};
use crate::read::budget::{
    HELPER_ATTEMPTS, HELPER_FLOOR_ATTEMPTS, HELPER_FLOOR_LINKS, HELPER_LINKS, HELPER_MIN_ATTEMPTS,
};
use crate::read::{
    Error, OperationKind, ProbeOutcome, ResponseKind, Result, Source, VisitOrder, classify,
    origin_miss_error, unusable_copy,
};
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
pub(crate) struct WalkConfig {
    /// This node's canonical rank among the owners, if it is one.
    pub rank: Option<usize>,
    /// Number of ranked owners (1..=3).
    pub owners: usize,
    /// Visit order for a non-owner; owners probe predecessors canonically.
    pub order: VisitOrder,
    /// The operation is a fixed page fetch (`Operation::Page`).
    pub page_operation: bool,
    pub continuation: Continuation,
    /// Previous-membership owners asked copy-only before origin; a probe of
    /// target `i` reports `canonical = owners + i`.
    pub handoff: usize,
    /// Predecessors (bit `c` for rank `c`) asked under version - 1, where
    /// they held the same rank, so one that has not adopted the current
    /// version still serves (Known issue 12).
    pub previous: u8,
    /// A non-owner page walk may ask an overflow helper (`canonical =
    /// owners`) once, right after an owner itself sheds the probe. A relay's
    /// `Overloaded` or a timeout says the route is congested, not the page.
    pub helper: bool,
    /// Lineage targets per owner rank (Known issue 25), funded from slack
    /// only: without slack the walk funds exactly as before.
    pub lineage: [u8; MAX_HANDOFF],
    /// The first `granted` handoff targets (successors, then lineage) are
    /// asked copy-only on their own grant, never the walk's budget, so a
    /// delegated rank 0 keeps its origin credit (Known issues 21, 34, 35).
    pub granted: usize,
    /// Send a run of granted probes at once ([`Step::Granted`], Known issue
    /// 45). Only a copy changes the walk, so their order cannot matter.
    pub parallel_granted: bool,
    /// What this owner knows about rank 0's acquisition (Known issues 39,
    /// 52): `Overdue` makes a rank 0 timeout grant authority, `Failed` makes
    /// [`Outcome::Failed`] withhold it.
    pub rank0: Rank0State,
}

/// Attempts granted to one successor probe.
pub const SUCCESSOR_GRANT_ATTEMPTS: u32 = 1;

/// Divides a successor probe's time share so it takes at most a fifth.
pub(crate) const SUCCESSOR_SHARE_DIVISOR: u32 = 4;

/// Links granted to one successor probe, as a reader funds an owner.
pub const SUCCESSOR_GRANT_LINKS: u8 = 8;

/// Most previous-membership owners one walk asks (the replica count).
pub const MAX_HANDOFF: usize = 3;

/// Most lineage targets one owner walk asks (Known issue 46). Granted probes
/// go out at once, so more targets add exchanges, not latency.
pub const MAX_LINEAGE: usize = 6;

/// Most handoff targets: successor targets plus lineage targets.
pub(crate) const MAX_TARGETS: usize = REPLICAS_BELOW + MAX_LINEAGE;

/// Later owners an owner can have (successor targets).
const REPLICAS_BELOW: usize = MAX_HANDOFF - 1;

/// The fresh grant a granted probe spends.
pub fn granted_budget(deadline: Instant) -> AcquisitionBudget {
    AcquisitionBudget::new(deadline, SUCCESSOR_GRANT_ATTEMPTS, SUCCESSOR_GRANT_LINKS)
}

/// Fewest attempts a non-owner funds an out-of-order Acquire to `canonical`
/// with: the exchange, one probe per predecessor, and the owner's origin.
pub fn owner_funding_min(canonical: usize) -> u32 {
    canonical as u32 + 2
}

/// [`owner_funding_min`] plus, above rank 0, the credit for the owner's
/// delegation to rank 0.
pub fn owner_funding(canonical: usize) -> u32 {
    owner_funding_min(canonical) + u32::from(canonical > 0)
}

/// Fewest links of a funded Acquire: the owner keeps one to reach rank 0.
pub(crate) const FUNDED_MIN_LINKS: u8 = 2;

/// Links a non-owner funds an out-of-order Acquire with: a full route when
/// available, else what a shared route would charge, if that leaves the
/// owner a link. Requiring a full route left forwarded owners with one
/// credit (Known issue 57).
pub fn funded_links(budget: &AcquisitionBudget) -> Option<u8> {
    if budget.remaining_links() >= MAX_ROUTE_LINKS {
        Some(MAX_ROUTE_LINKS)
    } else {
        let links = budget.route_links();
        (links >= FUNDED_MIN_LINKS).then_some(links)
    }
}

/// Whether a shared route would give the owner at `canonical` fewer remote
/// credits than it needs to probe its predecessors and still fetch (Known
/// issue 57). The walk then funds the owner instead, unless an earlier owner
/// was busy: it is likely fetching, and funding the next one would only
/// overlap its origin request (Known issue 31). The cost is bounded: a
/// funded owner sends at most one origin request (Known issue 59).
pub fn shared_share_short(
    budget: &AcquisitionBudget,
    canonical: usize,
    opportunities: usize,
) -> bool {
    canonical > 0
        && budget
            .remaining_attempts()
            .saturating_sub(1)
            .div_ceil(opportunities.max(1) as u32)
            < owner_funding_min(canonical) - 1
}

/// How long after replacing version - 1 an owner may still ask predecessors
/// under it. Peers keep a superseded membership for 30 s.
pub const PREVIOUS_MEMBERSHIP_WITHIN: Duration = Duration::from_secs(10);

/// A short fixed-capacity list, so target lists never allocate.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct List<T, const N: usize> {
    items: [T; N],
    len: u8,
}

impl<T: Copy + Default, const N: usize> Default for List<T, N> {
    fn default() -> Self {
        Self {
            items: [T::default(); N],
            len: 0,
        }
    }
}

impl<T: Copy + Default, const N: usize> List<T, N> {
    /// The first `N` of `items`.
    pub fn from_slice(items: &[T]) -> Self {
        items.iter().copied().collect()
    }

    /// Append `item`; `false` when the list is full.
    pub fn push(&mut self, item: T) -> bool {
        let len = usize::from(self.len);
        if len >= N {
            return false;
        }
        self.items[len] = item;
        self.len += 1;
        true
    }
}

impl<T, const N: usize> std::ops::Deref for List<T, N> {
    type Target = [T];

    fn deref(&self) -> &[T] {
        &self.items[..usize::from(self.len)]
    }
}

impl<T: Copy + Default, const N: usize> FromIterator<T> for List<T, N> {
    fn from_iter<I: IntoIterator<Item = T>>(iter: I) -> Self {
        let mut list = Self::default();
        for item in iter {
            if !list.push(item) {
                break;
            }
        }
        list
    }
}

impl<T: PartialEq, const N: usize, const K: usize> PartialEq<[T; K]> for List<T, N> {
    fn eq(&self, other: &[T; K]) -> bool {
        **self == other[..]
    }
}

impl<T: PartialEq, const N: usize> PartialEq<Vec<T>> for List<T, N> {
    fn eq(&self, other: &Vec<T>) -> bool {
        **self == other[..]
    }
}

/// Predecessors an owner at `rank` may ask under the previous membership:
/// bit `c` is set when rank `c` names the same node in both (Known issue 12).
fn same_rank_previous<N: PartialEq>(previous: &[N], current: &[N], rank: usize) -> u8 {
    (0..rank.min(current.len()).min(8))
        .filter(|c| previous.get(*c).is_some_and(|p| *p == current[*c]))
        .fold(0, |bits, c| bits | (1 << c))
}

/// Previous owners a page owner asks before origin, in previous rank order,
/// without this node and its current predecessors. Empty when the owner set
/// did not change, so a steady cold miss pays no extra probe.
fn handoff_targets<N: PartialEq + Copy + Default>(
    previous: &[N],
    current: &[N],
    rank: usize,
) -> List<N, MAX_HANDOFF> {
    let same = previous.len() == current.len() && previous.iter().all(|n| current.contains(n));
    if same {
        return List::default();
    }
    let me = current.get(rank);
    previous
        .iter()
        .filter(|n| Some(*n) != me && !current[..rank.min(current.len())].contains(n))
        .copied()
        .collect()
}

/// Later owners that arrived before the owner at `rank` (Known issue 25).
/// Under rendezvous hashing they could only have outranked it, and fetched
/// the page, before it arrived. Equal or unknown arrivals never qualify.
fn lineage_targets<N: Copy + Default>(
    owners: &[N],
    since: &[Option<u32>],
    rank: usize,
) -> List<N, MAX_HANDOFF> {
    let Some(Some(mine)) = since.get(rank).copied() else {
        return List::default();
    };
    owners
        .iter()
        .zip(since)
        .skip(rank + 1)
        .filter(|(_, s)| s.is_some_and(|s| s < mine))
        .map(|(n, _)| *n)
        .collect()
}

/// Demoted members (ranked just below the owners) that arrived before the
/// owner at `rank` and were owners then (Known issue 34): fewer than
/// [`MAX_HANDOFF`] members above them had arrived. Unknown arrivals count as
/// present, so they never add a target. Members whose arrival is `oldest`
/// (the start of this node's membership chain) always qualify, since no rule
/// can tell (Known issue 46).
fn demoted_targets<N: Copy + Default>(
    since: &[Option<u32>],
    demoted: &[N],
    demoted_since: &[Option<u32>],
    rank: usize,
    oldest: Option<u32>,
) -> List<N, MAX_LINEAGE> {
    let Some(Some(mine)) = since.get(rank).copied() else {
        return List::default();
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
            (arrival < mine && (unknown || above(k, arrival) < MAX_HANDOFF)).then_some(*n)
        })
        .collect()
}

/// How many leading `handoff` targets are in `granted`.
fn granted_prefix<N: PartialEq>(handoff: &[N], granted: &[N]) -> usize {
    handoff.iter().take_while(|n| granted.contains(n)).count()
}

/// [`lineage_targets`] counts for every owner rank.
fn lineage_counts(since: &[Option<u32>]) -> [u8; MAX_HANDOFF] {
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

/// Lineage targets first, then version - 1 targets while fewer than
/// [`MAX_HANDOFF`] are listed (those spend the walk's budget). A lineage
/// target outranked this owner before it arrived, so it is the likelier
/// holder.
fn merge_handoff<N: PartialEq + Copy + Default>(
    previous: &[N],
    lineage: &[N],
) -> List<N, MAX_TARGETS> {
    let mut targets: List<N, MAX_TARGETS> = lineage.iter().take(MAX_LINEAGE).copied().collect();
    for n in previous {
        if targets.len() >= MAX_HANDOFF {
            break;
        }
        if !targets.contains(n) {
            targets.push(*n);
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
    /// The destination is behind and there is nothing newer to refresh to:
    /// transient evidence, not a route failure.
    StaleDestination,
    /// The destination itself (not a relay) answered `Overloaded`. Like
    /// `Probe(Overloaded)` without a route failure; asks a reader's helper.
    Shed,
    /// Unanswered within the attempt's time bound. Like
    /// `Transport(Unreachable)`, except that an owner's probe of rank 0
    /// withholds origin authority: rank 0 is most likely still fetching
    /// (Known issue 31), unless it is `Overdue` (Known issue 39).
    TimedOut,
    /// Rank 0 itself answered an owner's probe `Unavailable`. Like
    /// `Probe(Unreachable)`, except that it withholds authority when this
    /// owner's own origin request failed recently (Known issue 52).
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
pub(crate) enum Step {
    Probe(Probe),
    /// Granted probes to send at once, reported with
    /// [`CandidateWalk::complete_granted`].
    Granted(Vec<Probe>),
    /// No probes remain. The caller still checks its scope before acting.
    End(End),
}

/// Sans-IO candidate walk state.
pub(crate) struct CandidateWalk {
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
    /// An owner's probe of rank 0 went unanswered: no origin authority.
    rank0_unanswered: bool,
    /// An earlier owner was busy ([`Outcome::Shed`], [`Outcome::TimedOut`],
    /// `Overloaded`), so it is likely fetching ([`shared_share_short`]).
    saw_busy: bool,
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
    /// A previous-membership owner: not predecessor evidence.
    handoff: bool,
    /// An owner's `Acquire` to rank 0.
    delegated: bool,
    /// An owner's page probe of rank 0, not a handoff probe.
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
            saw_busy: false,
        }
    }

    /// Decide whether this walk may ask a helper. A non-owner that cannot
    /// fund its first out-of-order owner visits the owners canonically.
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

    /// Ask the page's helper, funded from what is left.
    fn helper_probe(&mut self, budget: &mut AcquisitionBudget) -> Result<Step> {
        let attempts = budget.remaining_attempts().min(HELPER_ATTEMPTS);
        let links = budget.remaining_links().min(HELPER_LINKS);
        let mut child = budget.partition(attempts, links)?;
        // Forward every funded link: fewer cannot reach rank 0 through a
        // relay (Known issue 60).
        child.note_route_failure();
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

    /// Whether the pending probe is a granted successor probe.
    pub fn pending_successor(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.granted)
    }

    /// Whether the pending probe asks the overflow helper.
    pub fn pending_helper(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.helper)
    }

    /// Minimum credits the funded visits after `index` need.
    fn later_minimum(&self, index: usize) -> u32 {
        (index + 1..self.count)
            .filter_map(|later| self.target(later).map(|c| (later, c)))
            .filter(|(later, c)| *c > *later || self.config.continuation.bounded_routes)
            .map(|(_, c)| owner_funding_min(c))
            .sum()
    }

    /// What every later visit needs to still reach origin; lineage credits
    /// are slack beyond this (Known issue 25).
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

    /// The pending probe asks a handoff target: a `StaleMembership` answer
    /// is only a miss.
    pub fn pending_handoff(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.handoff)
    }

    /// The pending probe is sent under the previous membership: a
    /// `StaleMembership` answer is transient evidence, not a refresh.
    pub fn pending_previous(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.previous)
    }

    /// Rank 0 timed out or failed this owner's probe: no origin authority.
    pub fn rank0_unanswered(&self) -> bool {
        self.rank0_unanswered
    }

    /// The pending probe is this owner's page probe of rank 0.
    pub fn pending_rank0(&self) -> bool {
        self.pending.as_ref().is_some_and(|p| p.rank0)
    }

    /// Whether the walk stopped because a probe could not be funded.
    pub fn funding_stopped(&self) -> bool {
        self.funding
    }

    /// Evidence collected so far, in probe order.
    #[cfg(test)]
    fn evidence(&self) -> &[ProbeOutcome] {
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
                // Every granted target from here on, at once (Known issue 45).
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
                // A successor probe spends its own grant, never the walk's.
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
                    // A short time share: the requester waits on this.
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
                    && (canonical > index
                        || (shared_share_short(budget, canonical, count - index)
                            && !self.saw_busy))
                    && budget.remaining_attempts() >= owner_funding_min(canonical)
                    && funded_links(budget).is_some())
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
                let links = if rank.is_some() {
                    4
                } else if continuation.bounded_routes {
                    MAX_ROUTE_LINKS
                } else {
                    funded_links(budget).unwrap_or(MAX_ROUTE_LINKS)
                };
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

    /// Report the last [`Step::Granted`] batch: whether any returned a copy.
    pub fn complete_granted(&mut self, copy: bool) -> Option<End> {
        assert!(self.batch > 0, "no granted probes outstanding");
        self.batch = 0;
        copy.then(|| {
            self.done = true;
            End::Copy
        })
    }

    /// Return the pending probe's child credits without an outcome.
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

    /// The pending destination is behind and the caller waits to ask it
    /// again: return its credits and plan it once more.
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

    /// Whether asking the pending destination again still leaves every later
    /// visit funded.
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

    /// Record the pending probe's outcome; returns an end when the walk stops.
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
                self.saw_busy |= !pending.helper && !pending.handoff;
                self.rank0_unanswered |= pending.rank0 && self.config.rank0 != Rank0State::Overdue;
                Outcome::Transport(ProbeOutcome::Unreachable)
            }
            Outcome::Failed => {
                self.rank0_unanswered |= pending.rank0 && self.config.rank0 == Rank0State::Failed;
                Outcome::Probe(ProbeOutcome::Unreachable)
            }
            outcome => outcome,
        };
        if !pending.helper
            && !pending.handoff
            && matches!(
                outcome,
                Outcome::Probe(ProbeOutcome::Overloaded)
                    | Outcome::Transport(ProbeOutcome::Overloaded)
            )
        {
            // Overloaded on the route to the owner or by the owner: load,
            // not a missing owner ([`shared_share_short`]).
            self.saw_busy = true;
        }
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
                self.saw_busy = true;
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

/// After an origin version miss, old copies on later owners remain useful.
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

/// Map a transport error to walk evidence; `None` is terminal.
/// `HopBudgetExhausted` is local: the owner is unreachable within budget.
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
enum StaleAction {
    /// Refresh to the strictly newer local membership.
    Refresh,
    /// Nothing newer, or already refreshed: the destination is behind.
    Skip,
}

/// Each epoch refreshes at most once, and only to a strictly newer
/// membership.
fn stale_action(retried: bool, previous: u64, latest: Option<u64>) -> Result<StaleAction> {
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

/// Reserve the route and remote credits before sending.
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
struct HedgeFacts {
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

/// Whether to hedge before direct-link availability is checked.
fn hedge_eligible(facts: HedgeFacts) -> bool {
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

// Stale destinations (Known issues 19, 26, 36). A non-owner whose destination
// is behind walks the page's owners under N - 1 instead, once
// ([`previous_fallback`]), unless it owns the page there. A reader that cannot
// fall back waits for the destination to adopt N ([`stale_wait`]).

/// Most time one wait for a destination that is behind may take.
pub const MAX_STALE_WAIT: Duration = Duration::from_secs(4);

/// Most waits one read makes: a membership reaches every client within about
/// 5 s (Known issue 36), and two waits of a third cover that.
pub const MAX_STALE_WAITS: u8 = 2;

/// Shortest wait worth making.
pub const MIN_STALE_WAIT: Duration = Duration::from_millis(250);

/// A destination behind this node, for a reader that cannot fall back.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
struct StaleWaitFacts {
    /// This node owns the page (owners go on to their own fetch).
    pub owner: bool,
    /// [`previous_fallback`] walks the previous membership instead.
    pub fallback: bool,
    /// Waits this read already made for destinations that are behind.
    pub waits: u8,
    /// Asking again leaves later visits funded (required after the first).
    pub affordable: bool,
    /// Time left until the read's deadline.
    pub remaining: Duration,
}

/// How long a non-owner waits before asking a destination that is behind
/// again: a third of the time left, within [`MIN_STALE_WAIT`] and
/// [`MAX_STALE_WAIT`], at most [`MAX_STALE_WAITS`] times. The verified
/// `StaleMembership` refunds the probe's credits, so budgets never grow.
fn stale_wait(facts: StaleWaitFacts) -> Option<Duration> {
    if facts.owner
        || facts.fallback
        || facts.waits >= MAX_STALE_WAITS
        || (facts.waits > 0 && !facts.affordable)
    {
        return None;
    }
    let wait = (facts.remaining / 3).min(MAX_STALE_WAIT);
    (wait >= MIN_STALE_WAIT).then_some(wait)
}

/// Facts about a walk under membership N whose destination was behind.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
struct StaleSkip {
    /// This node owns the page under N.
    pub owner: bool,
    /// This read already refreshed its membership or fell back once.
    pub retried: bool,
    /// N - 1 is resolvable here, recent, and lists this node.
    pub previous_recent_member: bool,
}

/// Whether to walk the page's owners under N - 1 instead of skipping.
fn previous_fallback(s: StaleSkip) -> bool {
    !s.owner && !s.retried && s.previous_recent_member
}

/// A page owner under N - 1 would gain origin authority it no longer has.
fn fallback_rank_allowed(rank: Option<usize>) -> bool {
    rank.is_none()
}

#[cfg(test)]
mod stale_tests {
    use super::*;

    fn skip() -> StaleSkip {
        StaleSkip {
            owner: false,
            retried: false,
            previous_recent_member: true,
        }
    }

    #[test]
    fn non_owner_falls_back_once() {
        assert!(previous_fallback(skip()));
    }

    #[test]
    fn no_fallback_without_a_usable_previous_membership() {
        // Already retried (refreshed or fell back): at most one more walk.
        assert!(!previous_fallback(StaleSkip {
            retried: true,
            ..skip()
        }));
        // Peers may no longer resolve N - 1, or would reject this node.
        assert!(!previous_fallback(StaleSkip {
            previous_recent_member: false,
            ..skip()
        }));
        // Owners keep their walk under N.
        assert!(!previous_fallback(StaleSkip {
            owner: true,
            ..skip()
        }));
        assert!(!previous_fallback(StaleSkip::default()));
    }

    fn waiting() -> StaleWaitFacts {
        StaleWaitFacts {
            remaining: Duration::from_secs(9),
            affordable: true,
            ..StaleWaitFacts::default()
        }
    }

    #[test]
    fn a_reader_that_cannot_fall_back_waits_twice() {
        assert_eq!(stale_wait(waiting()), Some(Duration::from_secs(3)));
        // The first wait does not depend on funding (Known issue 26).
        assert_eq!(
            stale_wait(StaleWaitFacts {
                affordable: false,
                ..waiting()
            }),
            Some(Duration::from_secs(3))
        );
        // Known issue 36: a second wait, a third of what is left, covers
        // the 50k propagation window (about 5 s) within a 10 s deadline.
        let deadline = Duration::from_secs(10);
        let first = stale_wait(StaleWaitFacts {
            remaining: deadline,
            ..waiting()
        })
        .unwrap();
        let second = stale_wait(StaleWaitFacts {
            remaining: deadline - first,
            waits: 1,
            ..waiting()
        })
        .unwrap();
        assert!(
            first + second >= Duration::from_millis(5500),
            "{first:?} {second:?}"
        );
        assert_eq!(
            stale_wait(StaleWaitFacts {
                remaining: deadline - first - second,
                waits: 2,
                ..waiting()
            }),
            None
        );
        // Capped, and a third of what is left.
        let long = StaleWaitFacts {
            remaining: Duration::from_secs(60),
            ..waiting()
        };
        assert_eq!(stale_wait(long), Some(MAX_STALE_WAIT));
        let short = StaleWaitFacts {
            remaining: Duration::from_millis(900),
            ..waiting()
        };
        assert_eq!(stale_wait(short), Some(Duration::from_millis(300)));
    }

    #[test]
    fn no_wait_for_owners_fallbacks_repeats_or_short_deadlines() {
        for facts in [
            StaleWaitFacts {
                owner: true,
                ..waiting()
            },
            StaleWaitFacts {
                fallback: true,
                ..waiting()
            },
            StaleWaitFacts {
                waits: MAX_STALE_WAITS,
                ..waiting()
            },
            // A further wait must leave the later visits funded.
            StaleWaitFacts {
                waits: 1,
                affordable: false,
                ..waiting()
            },
            StaleWaitFacts {
                remaining: Duration::from_millis(749),
                ..waiting()
            },
            StaleWaitFacts::default(),
        ] {
            assert_eq!(stale_wait(facts), None, "{facts:?}");
        }
        let edge = StaleWaitFacts {
            remaining: Duration::from_millis(750),
            ..waiting()
        };
        assert_eq!(stale_wait(edge), Some(MIN_STALE_WAIT));
    }

    #[test]
    fn fallback_never_gives_origin_authority() {
        assert!(fallback_rank_allowed(None));
        for rank in 0..3 {
            assert!(!fallback_rank_allowed(Some(rank)));
        }
    }
}

// [`Fill`]: one read's acquisition as a sans-IO state machine. Production
// drives it from `src/read/candidates.rs`, the simulator from
// `sim/src/node/world.rs`. Node identities are caller-chosen `u32`s.

/// Previous owners that are not current members ([`Ranking::Previous`]).
pub const NOT_MEMBER: u32 = u32::MAX;

/// Members ranked just below the owners ([`Ranked::demoted`]).
pub const DEMOTED: usize = topology::RETAINED_DEPTH - topology::REPLICAS;

/// Optional rules. Production runs [`Features::PRODUCTION`]; the simulator
/// switches rules off to measure them, and keeps a few temporary `SIM_*`
/// bits where it still differs from production.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Features {
    bits: u32,
    /// Most lineage targets per owner walk (at most [`MAX_LINEAGE`]).
    lineage_cap: u8,
}

impl Features {
    /// Lineage targets are asked on their own grant (Known issue 35).
    pub const LINEAGE_GRANTS: u32 = 1;
    /// With [`Self::LINEAGE_GRANTS`], readers still fund lineage probes from
    /// slack ([`WalkConfig::lineage`]).
    pub const LINEAGE_SLACK: u32 = 1 << 1;
    /// Granted probes go out at once ([`WalkConfig::parallel_granted`]).
    pub const PARALLEL_GRANTED: u32 = 1 << 2;
    /// A non-owner page walk may ask an overflow helper
    /// ([`WalkConfig::helper`]).
    pub const HELPER: u32 = 1 << 3;
    /// A non-owner walks the previous membership at a destination that is
    /// behind (Known issue 19).
    pub const STALE_FALLBACK: u32 = 1 << 4;
    /// A non-owner waits for a destination that is behind (Known issue 26).
    pub const STALE_WAIT: u32 = 1 << 5;
    /// Rank 0's own `Unavailable` is [`Outcome::Failed`] (Known issue 52).
    pub const RANK0_FAILED: u32 = 1 << 6;
    /// Simulator only: a stale wait measures the time left from the
    /// budget's deadline alone, not the earlier of it and the scope's.
    pub const SIM_WAIT_BUDGET_DEADLINE: u32 = 1 << 16;
    /// Simulator only: a membership refresh drops the epoch's bounded routes.
    pub const SIM_REFRESH_DROPS_BOUNDED: u32 = 1 << 17;
    /// Simulator only: a failed or empty ranking under the previous
    /// membership denies the stale fallback for the rest of the read.
    pub const SIM_FALLBACK_RANK_DENIES: u32 = 1 << 18;
    /// Simulator only: a remaining-copy probe answered `StaleMembership` is
    /// a refresh or a skip, not a terminal error.
    pub const SIM_REMAINING_STALE: u32 = 1 << 19;

    /// Every rule production runs.
    pub const PRODUCTION: Self = Self {
        bits: 0x7f,
        lineage_cap: MAX_LINEAGE as u8,
    };

    /// `bits` with at most `lineage_cap` lineage targets per owner walk.
    pub const fn new(bits: u32, lineage_cap: usize) -> Self {
        Self {
            bits,
            lineage_cap: if lineage_cap < MAX_LINEAGE {
                lineage_cap as u8
            } else {
                MAX_LINEAGE as u8
            },
        }
    }

    /// These features with `bit` set or cleared.
    pub const fn with(self, bit: u32, on: bool) -> Self {
        Self {
            bits: if on {
                self.bits | bit
            } else {
                self.bits & !bit
            },
            ..self
        }
    }

    #[inline]
    fn has(self, bit: u32) -> bool {
        self.bits & bit != 0
    }
}

/// Inputs fixed for one [`Fill`].
#[derive(Clone, Copy, Debug)]
pub struct FillConfig {
    pub operation: OperationKind,
    /// The fill races a hedge before its first walk (plaintext page fills).
    pub hedge: bool,
    /// Stop after the walk: a copy is [`Action::Done`], origin authority is
    /// [`Action::Origin`], with no origin, remaining-copy or publish steps.
    pub walk_only: bool,
    /// The request's deadline (a stale wait never outlasts it).
    pub scope: Instant,
    pub features: Features,
}

/// Which ranking of the page [`Action::Rank`] asks for.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Ranking {
    /// The membership this read walks: the caller's own (`version: None`),
    /// or the newer one a refresh adopted.
    Current,
    /// An owner's version - 1 ranking, for handoff targets and predecessors
    /// asked under it. Owners that are not current members are
    /// [`NOT_MEMBER`]; [`Ranked::recent_member`] says whether the caller
    /// replaced that membership within [`PREVIOUS_MEMBERSHIP_WITHIN`] and is
    /// a member of it.
    Previous,
    /// The previous membership a non-owner walks instead when the current
    /// one's destination is behind (Known issue 19).
    Fallback,
    /// The walk's own ranking again, before origin: only a ranked owner may
    /// fetch.
    Authority,
}

/// One ranking of the page under membership `version` ([`Event::Ranked`]).
/// The caller computes what needs its membership: arrivals, the visit
/// order, and the members ranked just below the owners.
#[derive(Clone, Copy, Debug)]
pub struct Ranked {
    pub version: u64,
    pub owners: List<u32, MAX_HANDOFF>,
    /// This node's canonical rank among `owners`, if it is one.
    pub rank: Option<usize>,
    /// When each owner arrived in this node's membership chain.
    pub since: [Option<u32>; MAX_HANDOFF],
    /// A non-owner's visit order (`visit_order`, `lineage_first`,
    /// `demote_unanswered`); ignored for owners.
    pub order: VisitOrder,
    /// For an owner: members ranked just below the owners, and their
    /// arrivals (empty without [`demoted_targets`]).
    pub demoted: List<u32, DEMOTED>,
    pub demoted_since: [Option<u32>; DEMOTED],
    /// The generation this node's membership chain began at
    /// ([`demoted_targets`]).
    pub oldest: Option<u32>,
    /// Members of `version`.
    pub members: usize,
    /// [`Ranking::Previous`]: this node may ask predecessors under it.
    pub recent_member: bool,
}

impl Ranked {
    /// A ranking with only its owners known.
    pub fn new(version: u64, owners: &[u32], rank: Option<usize>) -> Self {
        Self {
            version,
            owners: List::from_slice(owners),
            rank,
            since: [None; MAX_HANDOFF],
            order: VisitOrder::canonical(owners.len()),
            demoted: List::default(),
            demoted_since: [None; DEMOTED],
            oldest: None,
            members: 0,
            recent_member: false,
        }
    }
}

/// How a speculative hedge ended ([`Event::Hedge`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HedgeEnd {
    /// Not raced (disabled, ineligible, or not admitted).
    NotStarted,
    /// A contender returned a validated page.
    Won,
    /// Neither won. `primary` is the primary's own outcome, `stale` whether
    /// a contender answered `StaleMembership`, `error` the race's terminal
    /// error, if any.
    Lost {
        stale: bool,
        primary: Option<ProbeOutcome>,
        error: Option<Error>,
    },
}

/// A peer's answer to [`Action::Probe`], before payload validation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Reply {
    /// An authenticated response; `signed` when the destination itself, not
    /// a relay in front of it, signed it.
    Answer { kind: ResponseKind, signed: bool },
    /// Sent, but unanswered within the attempt's own time bound while the
    /// request's scope still runs.
    TimedOut,
    /// The exchange failed without a response. `sent` is false when it
    /// never left this node (the probe could not be planned); `scope_live`
    /// when the request's own scope still runs.
    Failed {
        error: Error,
        sent: bool,
        scope_live: bool,
    },
}

/// What the caller learned, for [`Fill::on`].
#[derive(Clone, Copy, Debug)]
pub enum Event<'a> {
    /// The local tiers were checked.
    Start {
        local_hit: bool,
    },
    /// The ranking [`Action::Rank`] asked for.
    Ranked(Result<Ranked, Error>),
    /// [`Action::Hints`]: successor hints for the page (oldest first) and
    /// what this owner knows about rank 0's acquisition of it.
    Hints {
        hints: &'a [u32],
        rank0: Rank0State,
    },
    Hedge(HedgeEnd),
    Reply(Reply),
    /// [`Action::Validate`]: `Ok(None)` for a usable copy.
    Validated(Result<Option<ProbeOutcome>, Error>),
    /// [`Action::Probe`] asked the helper, but every candidate helper is
    /// excluded.
    NoHelper,
    /// Every probe of [`Action::Batch`] answered; `copy` when one returned a
    /// validated copy.
    BatchDone {
        copy: bool,
    },
    /// [`Action::Wait`] elapsed.
    Timer,
    /// [`Action::Refresh`]: the newest membership this node adopted, and
    /// whether its predecessor is still resolvable by peers, was replaced
    /// within [`PREVIOUS_MEMBERSHIP_WITHIN`], and lists this node.
    Membership {
        latest: Option<u64>,
        previous_recent_member: bool,
    },
    Origin(Result<(), Error>),
    Published(Result<(), Error>),
    /// The caller failed the read (for example an origin attempt it could
    /// not fund).
    Abort(Error),
}

/// What the caller does next.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Action {
    /// Rank the page; answer [`Event::Ranked`].
    Rank {
        ranking: Ranking,
        version: Option<u64>,
    },
    /// Answer [`Event::Hints`].
    Hints,
    /// Race a hedge if enabled, `eligible`, and admitted: the primary is
    /// canonical owner `first`, the duplicate `second`; answer
    /// [`Event::Hedge`].
    Hedge {
        eligible: bool,
        first: usize,
        second: usize,
    },
    /// Report the newest adopted membership; answer [`Event::Membership`].
    Refresh,
    /// Send this probe to [`Fill::dest`] (the helper when `None`) with
    /// [`Fill::request_budget`]; answer [`Event::Reply`] or
    /// [`Event::NoHelper`].
    Probe(Probe),
    /// Validate the payload of the last answer; answer [`Event::Validated`].
    Validate,
    /// Send copy-only probes to handoff targets `first..first + count` at
    /// once, each on its own [`granted_budget`]; answer
    /// [`Event::BatchDone`].
    Batch {
        first: usize,
        count: usize,
        remaining_opportunities: u32,
    },
    /// Wait this long for a destination that is behind; answer
    /// [`Event::Timer`].
    Wait(Duration),
    /// This owner at `rank` fetches from origin; answer [`Event::Origin`].
    Origin {
        rank: usize,
    },
    /// Publish the page; answer [`Event::Published`].
    Publish(Source),
    Done(Result<Source, Error>),
    /// Nothing to do until the next event.
    Idle,
}

/// What a destination's answer, or its silence, says about it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SilenceNote {
    Answered,
    /// It shed the request: busy, not hung.
    Shed,
    /// Unanswered; `needed` timeouts ask a ping
    /// ([`crate::liveness::RANK0_TIMEOUTS`], [`crate::liveness::NO_PING`]).
    TimedOut {
        needed: u32,
    },
}

/// What a probe of rank 0 says about its acquisition
/// ([`crate::liveness::Rank0Watch`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Rank0Note {
    Answered,
    Failed,
    TimedOut,
}

/// Facts from one [`Fill::on`] call for the caller's own state and counters.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Evidence {
    /// How the probe the event answered ended, when it was sent.
    pub outcome: Option<Result<Outcome, Error>>,
    /// That probe asked the overflow helper, a granted target, or rank 0
    /// (an owner's page probe), or belonged to the remaining-copy walk.
    pub helper: bool,
    pub granted: bool,
    pub rank0_probe: bool,
    pub remaining: bool,
    pub silence: Option<SilenceNote>,
    pub rank0: Option<Rank0Note>,
    /// [`Notes`] bits.
    pub notes: u16,
}

/// Decisions [`Evidence::notes`] reports, for counters.
pub struct Notes;

impl Notes {
    /// A destination behind this node was skipped.
    pub const STALE_SKIP: u16 = 1;
    /// ... and this node owns the page.
    pub const OWNER_STALE_SKIP: u16 = 1 << 1;
    /// The read refreshed to a newer membership.
    pub const REFRESH: u16 = 1 << 2;
    /// The read walks the previous membership instead.
    pub const FALLBACK: u16 = 1 << 3;
    /// This node owns the page under the previous membership: no fallback.
    pub const FALLBACK_DENIED: u16 = 1 << 4;
    /// ... and it waits for the destination instead.
    pub const WAIT_DENIED: u16 = 1 << 5;
    /// A non-owner's first skip found no previous membership to walk.
    pub const FALLBACK_BLOCKED: u16 = 1 << 6;
    /// Rank 0 left this owner's probe unanswered, so it does not fetch.
    pub const RANK0_LEFT: u16 = 1 << 7;
    pub const WALK_COPY: u16 = 1 << 8;
    pub const EXHAUSTED: u16 = 1 << 9;
    pub const EXHAUSTED_FUNDING: u16 = 1 << 10;
    /// The exhausted walk skipped a destination that was behind.
    pub const EXHAUSTED_STALE: u16 = 1 << 11;
    /// An origin version miss starts the remaining-copy walk.
    pub const REMAINING: u16 = 1 << 12;
}

/// A handoff target's kind ([`Fill::target_kind`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TargetKind {
    Successor,
    Lineage,
    Demoted,
    Handoff,
}

enum Walking {
    None,
    Candidates(CandidateWalk),
    Remaining(RemainingWalk),
}

/// Why [`Action::Refresh`] was asked.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Refresh {
    Hedge,
    Probe,
    Remaining,
}

/// Sans-IO state of one read's acquisition ([`Action`], [`Event`]).
pub struct Fill {
    config: FillConfig,
    /// The plan still races a hedge before the first walk.
    hedge: bool,
    ranking: Ranking,
    refresh: Refresh,
    /// The walked ranking. Public fields are read-only to callers.
    pub version: u64,
    pub owners: List<u32, MAX_HANDOFF>,
    pub rank: Option<usize>,
    order: VisitOrder,
    members: usize,
    lineage: [u8; MAX_HANDOFF],
    lineage_targets: List<u32, MAX_LINEAGE>,
    demoted: List<u32, MAX_LINEAGE>,
    pub handoff: List<u32, MAX_TARGETS>,
    successors: usize,
    granted: usize,
    previous: u8,
    rank0: Rank0State,
    /// This epoch's walk funds every probe on a bounded route.
    bounded: bool,
    continuation: Continuation,
    walk: Walking,
    probe: Option<Probe>,
    /// The last answer was signed by its destination.
    signed: bool,
    /// Across epochs. `funding`: the last walk could not fund a probe.
    pub retried: bool,
    pub fell_back: bool,
    pub fallback_denied: bool,
    pub waits: u8,
    waiting: bool,
    pub stale: u32,
    latest: u64,
    pub evidence: Vec<ProbeOutcome>,
    origin_rank: usize,
    /// [`Action::Origin`] awaits [`Event::Origin`].
    origin_pending: bool,
    source: Source,
    pub funding: bool,
    pub remaining_end: Option<RemainingEnd>,
    out: Evidence,
}

impl Fill {
    pub fn new(config: FillConfig) -> Self {
        Self {
            config,
            hedge: config.hedge && !config.walk_only,
            ranking: Ranking::Current,
            refresh: Refresh::Probe,
            version: 0,
            owners: List::default(),
            rank: None,
            order: VisitOrder::canonical(0),
            members: 0,
            lineage: [0; MAX_HANDOFF],
            lineage_targets: List::default(),
            demoted: List::default(),
            handoff: List::default(),
            successors: 0,
            granted: 0,
            previous: 0,
            rank0: Rank0State::Unknown,
            bounded: false,
            continuation: Continuation::default(),
            walk: Walking::None,
            probe: None,
            signed: false,
            retried: false,
            fell_back: false,
            fallback_denied: false,
            waits: 0,
            waiting: false,
            stale: 0,
            latest: 0,
            evidence: Vec::new(),
            origin_rank: 0,
            origin_pending: false,
            source: Source::Peer,
            funding: false,
            remaining_end: None,
            out: Evidence::default(),
        }
    }

    /// Feed one event; returns the next action and what the event showed.
    #[inline]
    pub fn on(
        &mut self,
        event: Event<'_>,
        budget: &mut AcquisitionBudget,
        now: Instant,
    ) -> (Action, Evidence) {
        let action = match self.handle(event, budget, now) {
            Ok(action) => action,
            Err(error) => {
                if let Walking::Candidates(walk) = &mut self.walk {
                    let _ = walk.abandon(budget);
                }
                self.walk = Walking::None;
                Action::Done(Err(error))
            }
        };
        (action, std::mem::take(&mut self.out))
    }

    /// Budget the outstanding probe spends: its bounded child, or the read's.
    pub fn request_budget<'a>(
        &'a mut self,
        budget: &'a mut AcquisitionBudget,
    ) -> &'a mut AcquisitionBudget {
        match &mut self.walk {
            Walking::Candidates(walk) => walk.request_budget(budget),
            _ => budget,
        }
    }

    /// Where `probe` goes: an owner or handoff target, or `None` for the
    /// overflow helper the caller chooses ([`crate::read::budget::fanout_helper`]).
    pub fn dest(&self, probe: &Probe) -> Option<u32> {
        match probe.canonical.checked_sub(self.owners.len()) {
            Some(i) if self.rank.is_some() => Some(self.handoff[i]),
            Some(_) => None,
            None => Some(self.owners[probe.canonical]),
        }
    }

    /// The kind of handoff target `i`.
    pub fn target_kind(&self, i: usize) -> TargetKind {
        if i < self.successors {
            TargetKind::Successor
        } else if i < self.granted && self.demoted.contains(&self.handoff[i]) {
            TargetKind::Demoted
        } else if i < self.granted {
            TargetKind::Lineage
        } else {
            TargetKind::Handoff
        }
    }

    /// The remaining-copy walk runs.
    pub fn remaining(&self) -> bool {
        matches!(self.walk, Walking::Remaining(_))
    }

    fn page(&self) -> bool {
        self.config.operation == OperationKind::Page
    }

    fn has(&self, bit: u32) -> bool {
        self.config.features.has(bit)
    }

    fn note(&mut self, note: u16) {
        self.out.notes |= note;
    }

    fn rank_next(&mut self, ranking: Ranking, version: Option<u64>) -> Action {
        self.ranking = ranking;
        Action::Rank { ranking, version }
    }

    fn handle(
        &mut self,
        event: Event<'_>,
        budget: &mut AcquisitionBudget,
        now: Instant,
    ) -> Result<Action> {
        match event {
            Event::Start { local_hit: true } => Ok(Action::Done(Ok(Source::Local))),
            Event::Start { .. } => Ok(self.rank_next(Ranking::Current, None)),
            Event::Ranked(ranked) => match self.ranking {
                Ranking::Current => self.ranked(ranked?, budget),
                Ranking::Previous => {
                    if let (Ok(previous), Some(rank)) = (ranked, self.rank) {
                        let targets = handoff_targets(&previous.owners, &self.owners, rank);
                        let targets: List<u32, MAX_HANDOFF> = targets
                            .iter()
                            .copied()
                            .filter(|n| *n != NOT_MEMBER)
                            .collect();
                        if previous.recent_member {
                            self.previous =
                                same_rank_previous(&previous.owners, &self.owners, rank);
                        }
                        self.handoff = merge_handoff(&targets, &self.lineage_targets);
                    }
                    self.targets(budget)
                }
                Ranking::Fallback => self.fallback_ranked(ranked, budget, now),
                Ranking::Authority => {
                    if *ranked?.owners != *self.owners {
                        return Err(Error::IncompatibleMembership);
                    }
                    self.origin_pending = !self.config.walk_only;
                    Ok(Action::Origin {
                        rank: self.origin_rank,
                    })
                }
            },
            Event::Hints { hints, rank0 } => {
                if let Some(rank) = self.rank {
                    let targets = crate::read::hints::successor_targets(hints, &self.owners, rank);
                    if !targets.is_empty() {
                        (self.handoff, self.successors) =
                            crate::read::hints::with_successors(&targets, &self.handoff);
                    }
                }
                self.rank0 = rank0;
                self.walk_or_hedge(budget)
            }
            Event::Hedge(end) => {
                self.hedge = false;
                match end {
                    HedgeEnd::NotStarted => self.start_walk(budget),
                    HedgeEnd::Won => Ok(self.walked(Source::Hedge)),
                    HedgeEnd::Lost {
                        stale,
                        primary,
                        error,
                    } => {
                        if let Some(error) = error
                            && !hedge_fallback(error)
                        {
                            return Err(error);
                        }
                        self.continuation = Continuation {
                            primary_consumed: !stale,
                            primary_outcome: (!stale)
                                .then(|| primary.unwrap_or(ProbeOutcome::Unreachable)),
                            bounded_routes: true,
                        };
                        if stale {
                            // A stale hedge refreshes to a newer membership when
                            // one is available. Otherwise the stale contender is
                            // behind this node (Known issue 3): walk this epoch's
                            // owners with bounded routes.
                            self.refresh = Refresh::Hedge;
                            return Ok(Action::Refresh);
                        }
                        self.start_walk(budget)
                    }
                }
            }
            Event::Reply(reply) => self.reply(reply, budget),
            Event::Validated(result) => match result {
                Ok(None) => self.complete(Outcome::Copy, budget),
                Ok(Some(outcome)) => self.complete(Outcome::Probe(outcome), budget),
                Err(error) => {
                    self.pending();
                    self.out.outcome = Some(Err(error));
                    Err(error)
                }
            },
            Event::NoHelper => {
                let Walking::Candidates(walk) = &mut self.walk else {
                    return Ok(Action::Idle);
                };
                let end = walk.complete(budget, Outcome::Probe(ProbeOutcome::CopyMiss))?;
                self.after(end, false, budget)
            }
            Event::BatchDone { copy } => {
                let Walking::Candidates(walk) = &mut self.walk else {
                    return Ok(Action::Idle);
                };
                match walk.complete_granted(copy) {
                    Some(end) => self.end(end),
                    None => self.next(budget),
                }
            }
            Event::Timer if self.waiting => {
                self.waiting = false;
                self.next(budget)
            }
            Event::Timer => Ok(Action::Idle),
            Event::Membership {
                latest,
                previous_recent_member,
            } => self.membership(latest, previous_recent_member, budget, now),
            Event::Origin(_) if !std::mem::take(&mut self.origin_pending) => {
                panic!("fill out of order: no origin request")
            }
            Event::Origin(Ok(())) => Ok(self.publish(Source::Origin)),
            Event::Origin(Err(Error::VersionUnavailable)) => {
                // After an origin version miss, old copies on later owners
                // remain useful.
                self.note(Notes::REMAINING);
                self.walk = Walking::Remaining(RemainingWalk::new(
                    self.rank.unwrap_or(0),
                    self.owners.len(),
                ));
                self.next(budget)
            }
            Event::Origin(Err(error)) | Event::Published(Err(error)) | Event::Abort(error) => {
                Err(error)
            }
            Event::Published(Ok(())) => Ok(Action::Done(Ok(self.source))),
        }
    }

    /// The page is ranked under the membership this read walks: build the
    /// owner's targets, then walk.
    fn ranked(&mut self, ranked: Ranked, budget: &mut AcquisitionBudget) -> Result<Action> {
        if ranked.owners.is_empty() {
            return Err(Error::Unavailable);
        }
        let page = self.page();
        let since = &ranked.since[..ranked.owners.len()];
        self.version = ranked.version;
        self.owners = ranked.owners;
        self.rank = ranked.rank;
        self.members = ranked.members;
        self.order = if ranked.rank.is_some() {
            VisitOrder::canonical(0)
        } else {
            ranked.order
        };
        // Arrival order of the page's owners in this node's membership
        // chain: an owner may ask later owners that arrived before it, which
        // it may have displaced however many versions ago (Known issue 25),
        // and readers and delegations fund those probes.
        self.lineage = if !page
            || (self.has(Features::LINEAGE_GRANTS) && !self.has(Features::LINEAGE_SLACK))
        {
            [0; MAX_HANDOFF]
        } else {
            lineage_counts(since)
        };
        // Lineage targets: later owners that arrived before this owner, then
        // members ranked just below the owners that were owners when they
        // arrived (Known issue 34). All are asked on their own grant, so a
        // rank 0 reached by a delegation with one credit asks them too
        // (Known issue 35).
        self.lineage_targets = List::default();
        self.demoted = List::default();
        if let (Some(rank), true) = (ranked.rank, page) {
            self.lineage_targets = List::from_slice(&lineage_targets(&ranked.owners, since, rank));
            self.demoted = demoted_targets(
                since,
                &ranked.demoted,
                &ranked.demoted_since[..ranked.demoted.len()],
                rank,
                ranked.oldest,
            );
            for n in self.demoted.iter() {
                if !self.lineage_targets.contains(n)
                    && self.lineage_targets.len() < usize::from(self.config.features.lineage_cap)
                {
                    self.lineage_targets.push(*n);
                }
            }
        }
        self.handoff = List::from_slice(&self.lineage_targets);
        self.successors = 0;
        self.previous = 0;
        self.stale = 0;
        self.rank0 = Rank0State::Unknown;
        // An owner also ranks the page under membership version - 1.
        if let (Some(_), true, Some(previous)) = (ranked.rank, page, ranked.version.checked_sub(1))
        {
            return Ok(self.rank_next(Ranking::Previous, Some(previous)));
        }
        self.targets(budget)
    }

    /// The handoff targets are known: an owner adds its successor targets.
    fn targets(&mut self, budget: &mut AcquisitionBudget) -> Result<Action> {
        if self.rank.is_some() && self.page() {
            return Ok(Action::Hints);
        }
        self.walk_or_hedge(budget)
    }

    fn walk_or_hedge(&mut self, budget: &mut AcquisitionBudget) -> Result<Action> {
        if !self.hedge {
            return self.start_walk(budget);
        }
        // The hedge primary and duplicate are the first two owners in the
        // visit order the walk uses, so `primary_consumed` skips exactly the
        // owner the hedge already asked.
        let (first, second) = (
            self.order.get(0).unwrap_or(0),
            self.order.get(1).unwrap_or(0),
        );
        let owners = self.owners.len();
        Ok(Action::Hedge {
            eligible: hedge_eligible(HedgeFacts {
                page_operation: self.page(),
                is_candidate: self.rank.is_some(),
                owners,
                first,
                second,
                distinct: owners >= 2 && self.owners[first] != self.owners[second],
                remaining_attempts: budget.remaining_attempts(),
                remaining_links: budget.remaining_links(),
            }),
            first,
            second,
        })
    }

    fn start_walk(&mut self, budget: &mut AcquisitionBudget) -> Result<Action> {
        let page = self.page();
        // Successor targets come first, then lineage targets, each on its own
        // grant; version - 1 targets after them spend the walk's credits.
        self.granted = self.successors
            + if self.has(Features::LINEAGE_GRANTS) && self.rank.is_some() {
                granted_prefix(&self.handoff[self.successors..], &self.lineage_targets)
            } else {
                0
            };
        let continuation = std::mem::take(&mut self.continuation);
        self.bounded = continuation.bounded_routes;
        self.walk = Walking::Candidates(CandidateWalk::new(WalkConfig {
            rank: self.rank,
            owners: self.owners.len(),
            order: self.order,
            page_operation: page,
            continuation,
            handoff: self.handoff.len(),
            previous: self.previous,
            // A non-owner page walk may ask the page's overflow helper
            // (Known issue 16), chosen when the walk asks for it.
            helper: self.rank.is_none()
                && page
                && self.has(Features::HELPER)
                && self.members > self.owners.len() + 1,
            lineage: self.lineage,
            granted: self.granted,
            parallel_granted: self.has(Features::PARALLEL_GRANTED),
            rank0: self.rank0,
        }));
        self.next(budget)
    }

    /// Plan the next probe of the current walk.
    fn next(&mut self, budget: &mut AcquisitionBudget) -> Result<Action> {
        match &mut self.walk {
            Walking::Candidates(walk) => match walk.next(budget)? {
                Step::Probe(probe) => {
                    self.probe = Some(probe);
                    Ok(Action::Probe(probe))
                }
                Step::Granted(probes) => Ok(Action::Batch {
                    first: probes[0].canonical - self.owners.len(),
                    count: probes.len(),
                    remaining_opportunities: probes[0].remaining_opportunities,
                }),
                Step::End(end) => self.end(end),
            },
            Walking::Remaining(walk) => match walk.next_probe() {
                Ok(probe) => {
                    self.probe = Some(probe);
                    Ok(Action::Probe(probe))
                }
                Err(end) => self.remaining_ended(end),
            },
            Walking::None => Ok(Action::Idle),
        }
    }

    /// Map a peer's answer to the walk's outcome.
    fn reply(&mut self, reply: Reply, budget: &mut AcquisitionBudget) -> Result<Action> {
        let probe = self.probe.expect("no probe outstanding");
        let page = self.page();
        let no_refresh = self.pending();
        let remaining = self.out.remaining;
        self.signed = false;
        let outcome = match reply {
            Reply::Answer { kind, signed } => {
                self.signed = signed;
                if signed {
                    // The destination answered this node: it is not hung
                    // (Known issue 41). One that shed the request is busy
                    // (Known issue 54).
                    self.out.silence = Some(if kind == ResponseKind::Overloaded && page {
                        SilenceNote::Shed
                    } else {
                        SilenceNote::Answered
                    });
                }
                match kind {
                    // The destination itself, not a relay, shed the request
                    // (for example a hot-page owner): the walk may ask the
                    // page's helper (Known issue 16).
                    ResponseKind::Overloaded if signed => Outcome::Shed,
                    ResponseKind::StaleMembership
                        if remaining && !self.has(Features::SIM_REMAINING_STALE) =>
                    {
                        self.out.outcome = Some(Err(Error::IncompatibleMembership));
                        return Err(Error::IncompatibleMembership);
                    }
                    // A stale handoff target is only a miss, and a
                    // predecessor asked under the previous membership no
                    // longer resolves it: neither refreshes this walk's
                    // membership.
                    ResponseKind::StaleMembership if no_refresh => Outcome::StaleMembership,
                    ResponseKind::StaleMembership => {
                        self.refresh = if remaining {
                            Refresh::Remaining
                        } else {
                            Refresh::Probe
                        };
                        return Ok(Action::Refresh);
                    }
                    // Rank 0 itself answered that its own acquisition failed:
                    // leave the next attempt to it (Known issue 52).
                    ResponseKind::Unavailable
                        if signed && self.out.rank0_probe && self.has(Features::RANK0_FAILED) =>
                    {
                        Outcome::Failed
                    }
                    kind if kind.has_payload() => return Ok(Action::Validate),
                    kind => match classify(
                        kind,
                        self.config.operation,
                        probe.mode == Mode::Acquire,
                        || Ok::<(), Error>(()),
                    ) {
                        Ok(outcome) => {
                            Outcome::Probe(outcome.unwrap_or(ProbeOutcome::UnusableCopy))
                        }
                        Err(error) => match unusable_copy(error, self.config.operation) {
                            Some(outcome) => Outcome::Probe(outcome),
                            None => {
                                self.out.outcome = Some(Err(error));
                                return Err(error);
                            }
                        },
                    },
                }
            }
            // Sent but unanswered within the attempt: an owner does not take
            // origin authority from rank 0 for this. A probe of the page's
            // rank 0 (an owner's, or a reader's) asks whether rank 0 answers
            // at all (Known issue 43); any other timeout only demotes the
            // destination in this node's visit order (Known issue 41).
            Reply::TimedOut => {
                if page {
                    let rank0 = self.out.rank0_probe
                        || (self.rank.is_none() && probe.canonical == 0 && !self.out.helper);
                    self.out.silence = Some(SilenceNote::TimedOut {
                        needed: if rank0 {
                            crate::liveness::RANK0_TIMEOUTS
                        } else {
                            crate::liveness::NO_PING
                        },
                    });
                }
                Outcome::TimedOut
            }
            Reply::Failed {
                error,
                sent,
                scope_live,
            } => {
                let outcome = match transport_outcome(error) {
                    Some(outcome) => outcome,
                    // A successor probe is opportunistic and runs on its own
                    // grant: any failure of it is only a miss, unless this
                    // request itself is over.
                    None if self.out.granted && scope_live => ProbeOutcome::Unreachable,
                    None => {
                        if sent {
                            self.out.outcome = Some(Err(error));
                        }
                        return Err(error);
                    }
                };
                if !sent {
                    // Never sent: no evidence about the destination.
                    self.out = Evidence {
                        notes: self.out.notes,
                        ..Evidence::default()
                    };
                    return self.walk_complete(Outcome::Transport(outcome), budget);
                }
                Outcome::Transport(outcome)
            }
        };
        self.complete(outcome, budget)
    }

    /// Report what the outstanding probe asked; whether a `StaleMembership`
    /// answer to it must not refresh membership (a handoff target, or a
    /// predecessor asked under the previous membership).
    fn pending(&mut self) -> bool {
        match &self.walk {
            Walking::Candidates(walk) => {
                self.out.helper = walk.pending_helper();
                self.out.granted = walk.pending_successor();
                self.out.rank0_probe = walk.pending_rank0();
                walk.pending_handoff() || walk.pending_previous()
            }
            _ => {
                self.out.remaining = true;
                false
            }
        }
    }

    /// Record the outstanding probe's outcome and its evidence.
    fn complete(&mut self, outcome: Outcome, budget: &mut AcquisitionBudget) -> Result<Action> {
        self.pending();
        self.out.outcome = Some(Ok(outcome));
        // Only rank 0's own answer (or its silence) says anything about
        // rank 0 (Known issue 39).
        if self.out.rank0_probe && (self.signed || outcome == Outcome::TimedOut) {
            self.out.rank0 = match outcome {
                Outcome::TimedOut => Some(Rank0Note::TimedOut),
                Outcome::Failed => Some(Rank0Note::Failed),
                Outcome::Copy
                | Outcome::Probe(
                    ProbeOutcome::CopyMiss
                    | ProbeOutcome::VersionUnavailable
                    | ProbeOutcome::Unreachable,
                ) => Some(Rank0Note::Answered),
                _ => None,
            };
        }
        self.walk_complete(outcome, budget)
    }

    fn walk_complete(
        &mut self,
        outcome: Outcome,
        budget: &mut AcquisitionBudget,
    ) -> Result<Action> {
        match &mut self.walk {
            Walking::Candidates(walk) => {
                let end = walk.complete(budget, outcome)?;
                self.after(end, false, budget)
            }
            Walking::Remaining(walk) => match walk.complete(budget, outcome) {
                Some(end) => self.remaining_ended(end),
                None => self.next(budget),
            },
            Walking::None => Ok(Action::Idle),
        }
    }

    /// After a probe completed: end the walk, or plan the next probe.
    fn after(
        &mut self,
        end: Option<End>,
        skipped: bool,
        budget: &mut AcquisitionBudget,
    ) -> Result<Action> {
        if let Some(end) = end {
            return self.end(end);
        }
        if skipped && self.rank.is_none() && !self.retried && !self.fell_back {
            self.note(Notes::FALLBACK_BLOCKED);
        }
        self.next(budget)
    }

    /// The newest adopted membership after a `StaleMembership` answer.
    fn membership(
        &mut self,
        latest: Option<u64>,
        previous_recent_member: bool,
        budget: &mut AcquisitionBudget,
        now: Instant,
    ) -> Result<Action> {
        // Each epoch walk refreshes at most once, and only to a strictly
        // newer membership; otherwise the stale destination is skipped like
        // an unreachable one, so a requester that adopted a publication
        // before its destination still reaches the other owners.
        let action = match stale_action(self.retried, self.version, latest) {
            Ok(action) => action,
            Err(error) => {
                if self.refresh != Refresh::Hedge {
                    self.pending();
                    self.out.outcome = Some(Err(error));
                }
                return Err(error);
            }
        };
        let latest = latest.unwrap_or_default();
        match (self.refresh, action) {
            (Refresh::Hedge, StaleAction::Refresh) => {
                self.note(Notes::REFRESH);
                self.retried = true;
                Ok(self.rank_next(Ranking::Current, Some(latest)))
            }
            (Refresh::Hedge, StaleAction::Skip) => {
                self.note(Notes::STALE_SKIP);
                self.start_walk(budget)
            }
            (Refresh::Probe, StaleAction::Refresh) => {
                self.latest = latest;
                self.complete(Outcome::StaleMembership, budget)
            }
            (Refresh::Probe, StaleAction::Skip) => {
                let owner = self.rank.is_some();
                // A non-owner that recently replaced the previous membership
                // walks the page's owners under it instead, once (Known issue
                // 19). It ranks the page there before deciding: a node that
                // owns it there may not walk it (that would give it origin
                // authority), and must not lose the wait it would otherwise
                // have had for this destination (Known issue 36).
                if self.has(Features::STALE_FALLBACK)
                    && !self.fallback_denied
                    && previous_fallback(StaleSkip {
                        owner,
                        retried: self.retried || self.fell_back,
                        previous_recent_member,
                    })
                {
                    self.fell_back = true;
                    return Ok(self.rank_next(Ranking::Fallback, Some(self.version - 1)));
                }
                if !owner && let Some(wait) = self.wait(budget, now)? {
                    return Ok(Action::Wait(wait));
                }
                self.skip(budget)
            }
            (Refresh::Remaining, StaleAction::Refresh) => {
                self.complete(Outcome::StaleMembership, budget)
            }
            (Refresh::Remaining, StaleAction::Skip) => {
                self.stale += 1;
                self.note(Notes::STALE_SKIP | Notes::OWNER_STALE_SKIP);
                self.complete(Outcome::StaleDestination, budget)
            }
        }
    }

    /// The destination is behind this node and no newer membership exists
    /// locally: try the next owner (Known issue 3).
    fn skip(&mut self, budget: &mut AcquisitionBudget) -> Result<Action> {
        self.stale += 1;
        self.note(Notes::STALE_SKIP);
        if self.rank.is_some() {
            // An owner's predecessor is behind (Known issue 12).
            self.note(Notes::OWNER_STALE_SKIP);
        }
        self.pending();
        self.out.outcome = Some(Ok(Outcome::StaleDestination));
        let Walking::Candidates(walk) = &mut self.walk else {
            return Ok(Action::Idle);
        };
        let end = walk.complete(budget, Outcome::StaleDestination)?;
        self.after(end, true, budget)
    }

    /// A non-owner that cannot walk the previous membership waits for a
    /// destination that is behind, then asks it again (Known issues 26 and
    /// 36). The outstanding probe's credits return to the read's budget.
    fn wait(&mut self, budget: &mut AcquisitionBudget, now: Instant) -> Result<Option<Duration>> {
        if !self.has(Features::STALE_WAIT) {
            return Ok(None);
        }
        let deadline = if self.has(Features::SIM_WAIT_BUDGET_DEADLINE) {
            budget.deadline()
        } else {
            budget.deadline().min(self.config.scope)
        };
        let Walking::Candidates(walk) = &mut self.walk else {
            return Ok(None);
        };
        let Some(wait) = stale_wait(StaleWaitFacts {
            owner: self.rank.is_some(),
            fallback: false,
            waits: self.waits,
            affordable: walk.retry_affordable(budget),
            remaining: deadline.saturating_duration_since(now),
        }) else {
            return Ok(None);
        };
        walk.retry_pending(budget)?;
        self.waits = self.waits.saturating_add(1);
        self.waiting = true;
        Ok(Some(wait))
    }

    /// The page is ranked under the previous membership: walk it there, or,
    /// when this node owns it there, keep walking the current membership.
    fn fallback_ranked(
        &mut self,
        ranked: Result<Ranked>,
        budget: &mut AcquisitionBudget,
        now: Instant,
    ) -> Result<Action> {
        let allowed = match &ranked {
            Ok(r) if !r.owners.is_empty() => fallback_rank_allowed(r.rank),
            _ => false,
        };
        if allowed {
            let ranked = ranked.expect("allowed");
            self.stale += 1;
            self.note(Notes::STALE_SKIP | Notes::FALLBACK);
            self.pending();
            self.out.outcome = Some(Ok(Outcome::StaleDestination));
            if let Walking::Candidates(walk) = &mut self.walk {
                walk.complete(budget, Outcome::StaleDestination)?;
                walk.abandon(budget)?;
            }
            // A fresh walk: the previous membership's owners were not asked.
            self.walk = Walking::None;
            self.retried = true;
            self.continuation = Continuation::default();
            return self.ranked(ranked, budget);
        }
        let denied = ranked.is_ok() || self.has(Features::SIM_FALLBACK_RANK_DENIES);
        if denied {
            self.note(Notes::FALLBACK_DENIED);
            self.fallback_denied = true;
        }
        self.fell_back = false;
        if let Some(wait) = self.wait(budget, now)? {
            if denied {
                self.note(Notes::WAIT_DENIED);
            }
            return Ok(Action::Wait(wait));
        }
        self.skip(budget)
    }

    /// The candidate walk ended.
    fn end(&mut self, end: End) -> Result<Action> {
        if let Walking::Candidates(walk) = &self.walk {
            self.funding = walk.funding_stopped();
            if walk.rank0_unanswered() {
                // Known issue 31: rank 0 has not answered this owner's probe
                // within the attempt; the owner does not fetch origin too.
                self.out.notes |= Notes::RANK0_LEFT;
            }
        }
        self.walk = Walking::None;
        match end {
            End::Copy => {
                self.note(Notes::WALK_COPY);
                Ok(self.walked(Source::Peer))
            }
            End::StaleMembership => {
                // Preserve the original operation, deadline, and spent
                // credits; walk the newer membership.
                self.note(Notes::REFRESH);
                self.retried = true;
                self.continuation = Continuation {
                    bounded_routes: self.bounded && !self.has(Features::SIM_REFRESH_DROPS_BOUNDED),
                    ..Continuation::default()
                };
                Ok(self.rank_next(Ranking::Current, Some(self.latest)))
            }
            End::Origin { rank, evidence } => {
                // Only a ranked owner may fetch: rank again before origin.
                self.evidence = evidence;
                self.origin_rank = rank;
                Ok(self.rank_next(Ranking::Authority, Some(self.version)))
            }
            End::VersionUnavailable => Err(Error::VersionUnavailable),
            End::Exhausted { funding } => {
                self.note(if funding {
                    Notes::EXHAUSTED_FUNDING
                } else {
                    Notes::EXHAUSTED
                });
                if self.stale > 0 {
                    self.note(Notes::EXHAUSTED_STALE);
                }
                Err(Error::Unavailable)
            }
        }
    }

    fn remaining_ended(&mut self, end: RemainingEnd) -> Result<Action> {
        self.walk = Walking::None;
        self.remaining_end = Some(end);
        match end {
            RemainingEnd::Copy => Ok(self.publish(Source::Peer)),
            RemainingEnd::None => Err(origin_miss_error(&self.evidence)),
            RemainingEnd::Unavailable => Err(Error::Unavailable),
        }
    }

    fn walked(&mut self, source: Source) -> Action {
        if self.config.walk_only {
            Action::Done(Ok(source))
        } else {
            self.publish(source)
        }
    }

    fn publish(&mut self, source: Source) -> Action {
        self.source = source;
        Action::Publish(source)
    }
}
