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
    /// No probes remain. The caller still checks its scope before acting.
    End(End),
}

/// Sans-IO candidate walk state.
pub struct CandidateWalk {
    config: WalkConfig,
    order: VisitOrder,
    count: usize,
    index: usize,
    evidence: Vec<ProbeOutcome>,
    saw_transient: bool,
    saw_version: bool,
    funding: bool,
    pending: Option<Pending>,
    done: bool,
}

struct Pending {
    bounded: Option<AcquisitionBudget>,
}

impl CandidateWalk {
    pub fn new(config: WalkConfig) -> Self {
        let count = config.rank.unwrap_or(config.owners);
        let order = if config.rank.is_some() {
            VisitOrder::canonical(count)
        } else {
            config.order
        };
        Self {
            config,
            order,
            count,
            index: 0,
            evidence: Vec::new(),
            saw_transient: false,
            saw_version: false,
            funding: false,
            pending: None,
            done: false,
        }
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
        assert!(!self.done, "walk already ended");
        let rank = self.config.rank;
        let continuation = self.config.continuation;
        while let Some(canonical) = self.order.get(self.index) {
            let index = self.index;
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
            let copy_only = rank.is_some()
                || (!continuation.bounded_routes
                    && budget.remaining_attempts() == 1
                    && self.config.page_operation);
            let mode = if copy_only {
                Mode::CopyOnly
            } else {
                Mode::Acquire
            };
            let count = self.count;
            let bounded = if rank.is_some()
                && u16::from(budget.remaining_links())
                    < (count - index) as u16 * u16::from(budget.route_links())
            {
                // Receiver CopyOnly predecessor probes share the inherited route
                // pool; the first must not consume every link after an incoming
                // eight-link envelope selected failure routing.
                let remaining = (count - index) as u8;
                Some(budget.partition(1, (budget.remaining_links() / remaining).min(4))?)
            } else if continuation.bounded_routes
                || (rank.is_none()
                    && !copy_only
                    && canonical > index
                    && budget.remaining_attempts() >= canonical as u32 + 2
                    && budget.remaining_links() >= 8)
            {
                // Fund by the destination's canonical rank: it probes all of its
                // predecessors, wherever it falls in this visit.
                let attempts = if rank.is_some() {
                    1
                } else {
                    canonical as u32 + 2
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
            let funded = continuation.bounded_routes || (rank.is_none() && bounded.is_some());
            self.pending = Some(Pending { bounded });
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

    /// Return the pending probe's child credits without recording an outcome,
    /// for a probe that failed terminally.
    pub fn abandon(&mut self, budget: &mut AcquisitionBudget) -> Result<()> {
        self.done = true;
        if let Some(Pending {
            bounded: Some(bounded),
        }) = self.pending.take()
        {
            budget.reunite(bounded)?;
        }
        Ok(())
    }

    /// Record the pending probe's outcome. Returns an end when the walk stops
    /// early (copy or stale membership).
    pub fn complete(
        &mut self,
        budget: &mut AcquisitionBudget,
        outcome: Outcome,
    ) -> Result<Option<End>> {
        let pending = self.pending.take().expect("no pending probe");
        if let Some(bounded) = pending.bounded {
            budget.reunite(bounded)?;
        }
        self.index += 1;
        match outcome {
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
        if let Some(rank) = self.config.rank {
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
            Outcome::Transport(_) => {
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
/// retry gets fresh credits. Partitioned remote credits stay spent.
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
        budget.partition(credits, 0)?.remaining_attempts()
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
/// primary(2) + duplicate(1) + the second visited owner at rank r (r + 2), with
/// links 1 + 1 + 8.
pub fn hedge_eligible(facts: HedgeFacts) -> bool {
    facts.page_operation
        && !facts.is_candidate
        && facts.owners >= 2
        && facts.first == 0
        && facts.distinct
        && facts.remaining_attempts >= 3 + facts.second as u32 + 2
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
