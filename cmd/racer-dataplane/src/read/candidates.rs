//! Ranked acquisition and copy-only predecessor probes. Only a validated local
//! candidate can mint origin authority; request headers never change placement.

use super::flight::AcquisitionBudget;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::AttemptId;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::PageNumber;
use crate::peer::Delivered;
use crate::peer::Requester;
use crate::peer::forwarding::VerifiedResponse;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerRequest;
use crate::peer::protocol::PeerResponse;
use crate::runtime::RequestScope;
use crate::security::CredentialCrypto;
use crate::security::OriginContext;
use crate::telemetry::Detail;
use crate::telemetry::Event;
use crate::telemetry::Failure;
use crate::telemetry::Metrics;
use crate::telemetry::Observer;
use crate::telemetry::Stage;
use crate::telemetry::{
    CandidateAttempt, CandidateFinal, CandidateOperation, CandidateOutcome, CandidateSite,
    CandidateTrail,
};
use crate::topology::Candidates;
use crate::topology::Placement;
use crate::topology::RouteBudget;
use racer_control_wire::NodeId;
use racer_core::read::hints::OfferEnd;
use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
#[cfg(test)]
use std::time::Instant;
use uring_runtime::environment::Deadline;

fn reserve_hedge_pages(
    admission: &flow_control::Quotas<crate::admission::AdmissionPolicy>,
    cache: &racer_control_wire::CacheId,
    pages: usize,
) -> Result<(
    flow_control::Charge<crate::admission::AdmissionPolicy>,
    flow_control::Charge<crate::admission::AdmissionPolicy>,
)> {
    use crate::admission::ResourceClass;
    let (plaintext, ciphertext) =
        racer_core::read::hedge_page_charges(crate::model::PAGE_BYTES as usize, pages);
    let plaintext = admission.reserve(Some(cache), ResourceClass::Plaintext, plaintext)?;
    let ciphertext = admission.reserve(Some(cache), ResourceClass::Ciphertext, ciphertext)?;
    Ok((plaintext, ciphertext))
}

pub struct OriginAuthority {
    diagnostic: CandidateTrail,
    membership: std::sync::Arc<crate::topology::Membership>,
    node: NodeId,
    object: ObjectId,
    /// The page's ranked owners under `membership`, which `rank` indexes. A walk that refreshed its
    /// membership grants authority under the newer ranking, not the caller's.
    owners: Vec<NodeId>,
    page: PageNumber,
    predecessor_evidence: Vec<ProbeOutcome>,
    rank: usize,
}
use racer_core::read::walk::{
    Action, Event as FillEvent, Evidence, Features, Fill, FillConfig, HedgeEnd, List, MAX_HANDOFF,
    Mode as WalkMode, NOT_MEMBER, Notes, Outcome as WalkOutcome, Probe, Rank0Note, Ranked, Ranking,
    RemainingEnd, RemainingWalk, Reply as FillReply, RouteMode, SilenceNote,
};
pub use racer_core::read::{ProbeOutcome, ReplicaSelection};
pub(crate) use racer_core::read::{VisitOrder, replica_score, visit_order};

/// One read's acquisition (`CandidatePolicy::drive`): the racer-core
/// [`Fill`] and the rankings its member positions refer to.
pub(crate) struct Acquisition {
    fill: Fill,
    /// The caller's membership version (diagnostics).
    membership: u64,
    /// The caller's ranking, for the first `Ranking::Current`.
    initial: Option<Candidates>,
    /// The ranking the walk probes.
    current: Option<Candidates>,
    /// The previous membership's ranking a stale fallback walks, until the walk takes it.
    fallback: Option<Candidates>,
    /// The membership a refresh adopts.
    latest: Option<std::sync::Arc<crate::topology::Membership>>,
    /// Version - 1, which predecessors are asked under.
    previous: Option<std::sync::Arc<crate::topology::Membership>>,
    /// Version - 1 for a stale fallback, when peers still resolve it.
    stale_previous: Option<std::sync::Arc<crate::topology::Membership>>,
    /// The answer awaiting validation, and the outstanding probe's
    /// destination, mode, and send time.
    answer: Option<VerifiedResponse>,
    dest: Option<ProbeDest>,
    acquire: bool,
    sent: std::time::Instant,
    /// A dataplane error racer-core cannot represent, for its failure.
    error: Option<Error>,
    phase: Phase,
    authority: Option<OriginAuthority>,
    pub(crate) trail: CandidateTrail,
}

impl Acquisition {
    /// `error` as racer-core sees it; the dataplane error is kept for the read's failure.
    fn core_error(&mut self, error: Error) -> racer_core::read::Error {
        racer_core::read::Error::try_from(error).unwrap_or_else(|_| {
            self.error = Some(error);
            racer_core::read::Error::Internal
        })
    }

    /// The ranking the walk probes.
    pub(crate) fn candidates(&self) -> &Candidates {
        self.current.as_ref().expect("ranked")
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Phase {
    Walk,
    Origin,
    Remaining,
}

enum ProbeDest {
    Member(u32),
    Helper(NodeId),
}

/// The owners of `candidates` as member positions.
fn positions(candidates: &Candidates) -> List<u32, MAX_HANDOFF> {
    candidates
        .ordered
        .iter()
        .map(|node| {
            candidates
                .membership
                .position(node)
                .map_or(NOT_MEMBER, |p| p as u32)
        })
        .collect()
}

pub struct CandidatePolicy {
    hedge: Option<Arc<Hedges>>,
    replica_selection: ReplicaSelection,
    attempt_timeout: std::time::Duration,
    observer: Observer,
    pub(super) node: NodeId,
    placement: Rc<Placement>,
    peers: Rc<Requester>,
    credentials: Rc<CredentialCrypto>,
    published: Arc<controlplane::Published<crate::control::Snapshot>>,
    ownership_refresh: RefCell<OwnershipRefresh>,
    single_flight: RefCell<SingleFlightPeers>,
    /// Timer for waiting on a destination that is behind. Without one, the walk skips it as before.
    reactor: Option<Rc<crate::runtime::Reactor>>,
    /// Test only: wait without a timer, returning at once.
    #[cfg(test)]
    pub(crate) instant_stale_wait: std::cell::Cell<bool>,
    /// Owners this node answered a page probe without a copy, by page (see
    /// `racer_core::read::hints`).
    pub(crate) successors: RefCell<Successors>,
    /// Pages this node fetched from origin as an owner after rank 0, to offer to rank 0 later (see
    /// `racer_core::read::hints`).
    pub(crate) notices: RefCell<Notices>,
    /// Copy offers in flight ([`MAX_OFFERS_IN_FLIGHT`]).
    offers: Rc<std::cell::Cell<usize>>,
    /// Silence pings in flight ([`MAX_PINGS_IN_FLIGHT`]).
    pub(crate) pings: Rc<std::cell::Cell<usize>>,
    /// Pages whose rank 0 this node, as an owner above it, found fetching (a probe timed out) or
    /// overdue (it missed that request's deadline); see `racer_core::liveness`.
    pub(crate) rank0_watch: RefCell<Rank0Watch>,
}

/// Counts one copy offer in flight until its task ends or is dropped.
struct OfferFlight(Rc<std::cell::Cell<usize>>);

impl OfferFlight {
    fn new(count: Rc<std::cell::Cell<usize>>) -> Self {
        count.set(count.get() + 1);
        Self(count)
    }
}

impl Drop for OfferFlight {
    fn drop(&mut self) {
        self.0.set(self.0.get().saturating_sub(1));
    }
}

/// Orphan notices grouped by their target's site. Keyed by page and the target's owner rank.
pub(crate) type Notices =
    racer_core::read::hints::OrphanNotices<(crate::model::PageId, u8), NodeId, String>;

pub(crate) use racer_core::liveness::{MAX_OFFERS_IN_FLIGHT, MAX_PINGS_IN_FLIGHT};

/// When each of the page's owners arrived in this node's membership chain (`Membership::since`),
/// for lineage targets.
pub(crate) type Rank0Watch = racer_core::liveness::Rank0Watch<crate::model::PageId, NodeId>;

pub(crate) type Successors = racer_core::read::hints::SuccessorHints<crate::model::PageId, NodeId>;

/// How long a provider stays single-flight after a refused multi-flight
/// Subscribe. An upgraded provider is asked again after this.
const SINGLE_FLIGHT_TTL: Duration = Duration::from_secs(300);

/// Most providers remembered as single-flight. The oldest is forgotten first.
const MAX_SINGLE_FLIGHT_PEERS: usize = 1024;

/// Providers that refused `Subscription::flights`, presumably because they
/// predate it, with when to try the field again.
#[derive(Default)]
struct SingleFlightPeers(std::collections::HashMap<NodeId, std::time::Instant>);

impl SingleFlightPeers {
    fn contains(&mut self, peer: &NodeId, now: std::time::Instant) -> bool {
        match self.0.get(peer) {
            Some(until) if *until > now => true,
            Some(_) => {
                self.0.remove(peer);
                false
            }
            None => false,
        }
    }

    fn insert(&mut self, peer: NodeId, now: std::time::Instant) {
        if !self.0.contains_key(&peer) && self.0.len() >= MAX_SINGLE_FLIGHT_PEERS {
            self.0.retain(|_, until| *until > now);
            if self.0.len() >= MAX_SINGLE_FLIGHT_PEERS
                && let Some(oldest) = self
                    .0
                    .iter()
                    .min_by_key(|(_, until)| **until)
                    .map(|(peer, _)| peer.clone())
            {
                self.0.remove(&oldest);
            }
        }
        self.0.insert(peer, now + SINGLE_FLIGHT_TTL);
    }
}

/// Result of one provider selection request.
pub(crate) enum ProviderSelection {
    /// This node is the provider for the first demanded page; fetch locally.
    Local,
    /// No page this time: the provider missed, was overloaded or unreachable.
    /// The caller may retry with a new sequence.
    Skipped,
    /// The provider refused a multi-flight request without a signed response,
    /// as a provider predating `Subscription::flights` does. It is now
    /// single-flight for `SINGLE_FLIGHT_TTL`, so retrying with a new sequence
    /// omits the field. A transport failure on a multi-flight request looks the
    /// same and is treated the same; it only costs concurrency, never pages.
    Refused { provider: NodeId },
    /// One verified page. `flights` is how many selections the caller may keep
    /// pending on this contract from now on (the provider's grant, or 1).
    Selected {
        provider: NodeId,
        response: Box<VerifiedResponse>,
        flights: u16,
    },
}

#[derive(Default)]
struct OwnershipRefresh {
    cursor: u64,
    visited: usize,
    active: Option<crate::model::PageId>,
}
#[derive(Clone, Copy)]
enum RequestMode {
    SharedRoute,
    FundedRoute,
    DirectHedge,
}
impl CandidatePolicy {
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&Arc<Hedges>> {
        self.hedge.as_ref()
    }
    /// Only Fill's plaintext fixed-page path calls this. Direct HTTP destinations prove independent
    /// first hops. Drain both attempts before releasing escrow.
    #[allow(clippy::too_many_arguments)] // The hedge borrows the caller's distinct policy inputs.
    pub(crate) async fn hedge_page<'a>(
        &'a self,
        candidates: &Candidates,
        (first, second, eligible): (usize, usize, bool),
        context: &'a OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        admission: &Rc<flow_control::Quotas<crate::admission::AdmissionPolicy>>,
        validate: impl Fn(VerifiedResponse, RequestScope) -> Operation<'a, crate::memory::PageResult>,
    ) -> Result<(Option<crate::memory::PageResult>, HedgeEnd)> {
        let Some(hedges) = self.hedge.as_ref().filter(|h| h.enabled()) else {
            return Ok((None, HedgeEnd::NotStarted));
        };
        // The hedge primary and duplicate are the first two owners in the visit order the walk uses
        // (`HedgeEnd::Lost` skips the primary).
        let eligible = eligible
            && [first, second].iter().all(|rank| {
                self.peers
                    .direct_hedge_available(&candidates.membership, &candidates.ordered[*rank])
            });
        if !eligible {
            hedges.suppressed();
            return Ok((None, HedgeEnd::NotStarted));
        }
        check_budget(scope, budget)?;
        let reservation = (|| {
            let slot = hedges.acquire()?;
            let escrow = reserve_hedge_pages(admission, &context.object.cache, 1)?;
            // The caller already holds its serial plaintext page. Escrow is additional accounting,
            // not a buffer consumed by either validator.
            let working = reserve_hedge_pages(admission, &context.object.cache, 2)?;
            drop(working);
            Ok::<_, Error>((slot, escrow))
        })();
        let Ok((slot, _escrow)) = reservation else {
            hedges.suppressed();
            return Ok((None, HedgeEnd::NotStarted));
        };
        // Pinned direct exchanges consume exactly one link each. Reserve the
        // remaining original links for serial candidates or one epoch refresh.
        let (mut secondary_budget, mut primary_budget) =
            racer_core::read::walk::hedge_budgets(budget)?;
        let primary_scope =
            RequestScope::new(scope.request, scope.deadline.0.min(budget.deadline()))?;
        let secondary_scope = RequestScope::new(scope.request, primary_scope.deadline.0)?;
        let stale = std::cell::Cell::new(false);
        let primary_outcome = std::cell::Cell::new(None);
        let primary = async {
            let response = self
                .request_mode(
                    &candidates.membership,
                    &candidates.ordered[first],
                    context,
                    operation,
                    FetchMode::Acquire,
                    &primary_scope,
                    &mut primary_budget,
                    3,
                    RequestMode::DirectHedge,
                )
                .await?;
            if matches!(response.response(), PeerResponse::StaleMembership) {
                stale.set(true);
                return Err(Error::Unavailable);
            }
            match classify(response.response(), operation, true)? {
                Some(outcome) => {
                    primary_outcome.set(Some(outcome));
                    Err(Error::Unavailable)
                }
                None => {
                    let result = validate(response, primary_scope.clone()).await;
                    if matches!(result, Err(Error::CorruptRecord | Error::MissingKey)) {
                        primary_outcome.set(Some(ProbeOutcome::UnusableCopy));
                    }
                    result
                }
            }
        };
        let secondary = async {
            if !self
                .peers
                .direct_hedge_available(&candidates.membership, &candidates.ordered[second])
            {
                hedges.suppressed();
                return Err(Error::Overloaded);
            }
            // Recheck local headroom after the delay. The escrow remains held;
            // these trial reservations do not replace actual receive/crypto quota.
            let capacity = reserve_hedge_pages(admission, &context.object.cache, 1);
            let Ok(capacity) = capacity else {
                hedges.suppressed();
                return Err(Error::Overloaded);
            };
            drop(capacity);
            slot.started();
            let response = self
                .request_mode(
                    &candidates.membership,
                    &candidates.ordered[second],
                    context,
                    operation,
                    FetchMode::CopyOnly,
                    &secondary_scope,
                    &mut secondary_budget,
                    3,
                    RequestMode::DirectHedge,
                )
                .await?;
            if matches!(response.response(), PeerResponse::StaleMembership) {
                stale.set(true);
                return Err(Error::Unavailable);
            }
            match classify(response.response(), operation, false)? {
                Some(_) => Err(Error::Unavailable),
                None => validate(response, secondary_scope.clone()).await,
            }
        };
        let result = race(
            primary,
            secondary,
            &primary_scope,
            &secondary_scope,
            scope,
            &slot,
        )
        .await;
        budget.reunite(primary_budget)?;
        budget.reunite(secondary_budget)?;
        check_budget(scope, budget)?;
        let lost = HedgeEnd::Lost {
            stale: stale.get(),
            primary: primary_outcome.get(),
            error: None,
        };
        match result {
            Ok(result) => Ok((Some(result), HedgeEnd::Won)),
            Err(error)
                if racer_core::read::Error::try_from(error)
                    .is_ok_and(racer_core::read::walk::hedge_fallback) =>
            {
                Ok((None, lost))
            }
            Err(error) => Err(error),
        }
    }
    /// One admitted selection, one transfer grant. Routing chooses the primary of the oldest
    /// demanded page; the provider can choose any demanded page for which it is also primary.
    #[allow(clippy::too_many_arguments)] // Keep demand, validation, and budget inputs explicit.
    pub(crate) async fn subscribe(
        &self,
        selection: &super::range_stream::Selection,
        flights: u16,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<Option<VerifiedResponse>> {
        let first = selection
            .demand
            .intervals()
            .first()
            .ok_or(Error::InvalidRequest)?
            .start;
        let deadline = scope.deadline.0.min(budget.deadline());
        let scheduler = &selection.scheduler;
        let result = self
            .subscribe_provider(
                &selection.version.object,
                PageNumber(first),
                flights,
                |provider, requested| {
                    scheduler.contract(
                        selection,
                        membership.version,
                        provider.clone(),
                        requested,
                        deadline,
                    )
                },
                membership.clone(),
                context,
                scope,
                budget,
            )
            .await?;
        Ok(match result {
            ProviderSelection::Selected {
                provider,
                response,
                flights,
            } => {
                scheduler.granted(selection, &provider, flights);
                Some(*response)
            }
            ProviderSelection::Refused { provider } => {
                scheduler.refused(selection, &provider);
                None
            }
            ProviderSelection::Local | ProviderSelection::Skipped => None,
        })
    }

    /// Request one selection from the provider of `first`, asking it to allow up to `flights`
    /// selections pending at once on the caller's contract.
    #[allow(clippy::too_many_arguments)] // Keep demand, validation, and budget inputs explicit.
    pub(crate) async fn subscribe_provider(
        &self,
        object: &ObjectId,
        first: PageNumber,
        flights: u16,
        contract: impl FnOnce(
            &NodeId,
            Option<u16>,
        )
            -> Result<(crate::peer::subscriptions::Subscription, std::time::Instant)>,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<ProviderSelection> {
        let rank = self
            .candidates_scoped(membership.clone(), object, first, scope)
            .await?;
        let destination = rank.ordered.first().ok_or(Error::Unavailable)?;
        if destination == &self.node {
            return Ok(ProviderSelection::Local);
        }
        let requested = self.requested_flights(destination, flights);
        let (mut subscription, deadline) = contract(destination, requested)?;
        subscription.flights = requested;
        let mut selected_scope = scope.clone();
        selected_scope.deadline.0 = deadline;
        let operation = PeerOperation::Subscribe {
            subscription,
            mode: FetchMode::Acquire,
        };
        let skipped = || ProviderSelection::Skipped;
        let response = match self
            .request(
                &membership,
                destination,
                context,
                &operation,
                FetchMode::Acquire,
                &selected_scope,
                budget,
                2,
            )
            .await
        {
            Ok(response) => response,
            Err(error @ (Error::Unavailable | Error::Io | Error::Os(_) | Error::Overloaded)) => {
                budget.note_route_failure();
                // A provider predating the field drops the exchange without a
                // signed response; it cannot answer in a way we can tell apart.
                if requested.is_some() && !matches!(error, Error::Overloaded) {
                    self.single_flight
                        .borrow_mut()
                        .insert(destination.clone(), uring_runtime::environment::now());
                    return Ok(ProviderSelection::Refused {
                        provider: destination.clone(),
                    });
                }
                return Ok(skipped());
            }
            Err(error) => return Err(error),
        };
        match classify(response.response(), &operation, true)? {
            None => {
                let PeerResponse::Selected { grant, .. } = response.response() else {
                    return Err(Error::CorruptRecord);
                };
                let actual = self
                    .candidates_scoped(
                        membership,
                        &grant.page.version.object,
                        grant.page.number,
                        scope,
                    )
                    .await?;
                if actual.ordered.first() != Some(destination) {
                    return Err(Error::Unauthorized);
                }
                let flights = grant.flights.unwrap_or(1);
                Ok(ProviderSelection::Selected {
                    provider: destination.clone(),
                    response: Box::new(response),
                    flights,
                })
            }
            // A relay signs `Unavailable` when the next hop drops the exchange, which is how an
            // older provider behind a relay refuses the field.
            Some(_)
                if requested.is_some()
                    && matches!(response.response(), PeerResponse::Unavailable) =>
            {
                self.single_flight
                    .borrow_mut()
                    .insert(destination.clone(), uring_runtime::environment::now());
                Ok(ProviderSelection::Refused {
                    provider: destination.clone(),
                })
            }
            Some(_) => Ok(skipped()),
        }
    }

    /// The flight count to request from `provider`, or `None` to send the
    /// original single-flight Subscribe.
    fn requested_flights(&self, provider: &NodeId, flights: u16) -> Option<u16> {
        if flights <= 1
            || self
                .single_flight
                .borrow_mut()
                .contains(provider, uring_runtime::environment::now())
        {
            return None;
        }
        Some(flights.min(crate::peer::subscriptions::MAX_SUBSCRIPTION_FLIGHTS))
    }

    pub fn new(
        node: NodeId,
        placement: Rc<Placement>,
        peers: Rc<Requester>,
        credentials: Rc<CredentialCrypto>,
        published: Arc<controlplane::Published<crate::control::Snapshot>>,
    ) -> Self {
        Self {
            hedge: None,
            // Low-level construction keeps canonical order; the service reads
            // its selection from configuration (default `Spread`).
            replica_selection: ReplicaSelection::Primary,
            attempt_timeout: std::time::Duration::from_secs(30),
            observer: Observer::default(),
            node,
            placement,
            peers,
            credentials,
            published,
            ownership_refresh: RefCell::default(),
            single_flight: RefCell::default(),
            reactor: None,
            #[cfg(test)]
            instant_stale_wait: std::cell::Cell::new(false),
            successors: RefCell::new(Successors::new(
                racer_core::read::hints::HINT_CAPACITY,
                racer_core::read::hints::HINT_TTL,
            )),
            notices: RefCell::new(Notices::new(racer_core::read::hints::NOTICE_CAPACITY)),
            offers: Rc::default(),
            pings: Rc::default(),
            rank0_watch: RefCell::new(Rank0Watch::new(
                racer_core::liveness::WATCH_CAPACITY,
                racer_core::liveness::WATCH_TTL,
                racer_core::liveness::HANG_AFTER,
            )),
        }
    }

    /// Feed a walk probe's route evidence to the orphan notices' cut groups: a transport failure or
    /// timeout toward `destination` holds offers to its site, an answer signed by it releases them.
    fn observe_route(
        &self,
        membership: &crate::topology::Membership,
        destination: &NodeId,
        outcome: &WalkOutcome,
    ) {
        let cut = match outcome {
            WalkOutcome::Transport(_) | WalkOutcome::TimedOut => true,
            WalkOutcome::Copy
            | WalkOutcome::Shed
            | WalkOutcome::Failed
            | WalkOutcome::Probe(ProbeOutcome::CopyMiss | ProbeOutcome::VersionUnavailable) => {
                false
            }
            _ => return,
        };
        let site = membership
            .member(destination)
            .map(|m| m.site.clone())
            .unwrap_or_default();
        self.notices
            .borrow_mut()
            .observe(site, cut, uring_runtime::environment::now());
    }

    /// Whether this policy can wait for a destination that is behind.
    fn can_wait(&self) -> bool {
        #[cfg(test)]
        if self.instant_stale_wait.get() {
            return true;
        }
        self.reactor.is_some()
    }

    /// Let readers wait for a destination that is behind this node's membership instead of skipping
    /// it.
    pub(crate) fn with_reactor(mut self, reactor: Rc<crate::runtime::Reactor>) -> Self {
        self.reactor = Some(reactor);
        self
    }

    /// Sleep until `until` under the request's scope. `false` without a timer.
    async fn stale_sleep(&self, until: std::time::Instant, scope: &RequestScope) -> Result<bool> {
        use uring_runtime::reactor::timer::SleepMode;
        #[cfg(test)]
        if self.instant_stale_wait.get() {
            return Ok(true);
        }
        let Some(reactor) = &self.reactor else {
            return Ok(false);
        };
        #[cfg(test)]
        let mode = if uring_runtime::environment::simulation_seed().is_some() {
            SleepMode::ClockPoll
        } else {
            SleepMode::Kernel
        };
        #[cfg(not(test))]
        let mode = SleepMode::Kernel;
        reactor.sleep_until(until, mode, scope).await?;
        Ok(true)
    }

    pub(crate) fn with_observer(mut self, observer: Observer) -> Self {
        self.observer = observer;
        self
    }
    pub(crate) fn with_hedges(mut self, hedges: Arc<Hedges>) -> Self {
        self.hedge = Some(hedges);
        self
    }

    /// Choose how a non-owner requester orders its attempts across owners.
    pub(crate) fn with_replica_selection(mut self, selection: ReplicaSelection) -> Self {
        self.replica_selection = selection;
        self
    }

    /// The order in which this node, as a non-owner, visits `candidates`.
    /// Owners never use this: they probe predecessors in canonical order.
    /// Allocation-free: neighbor checks binary-search the overlay graph.
    pub(crate) fn visit_order(
        &self,
        candidates: &Candidates,
        object: &ObjectId,
        page: PageNumber,
    ) -> VisitOrder {
        let ordered = &candidates.ordered;
        let order = visit_order(
            self.replica_selection,
            ordered.len(),
            |rank| {
                candidates
                    .membership
                    .is_neighbor(&self.node, &ordered[rank])
            },
            |rank| replica_score(&self.node, object, page, &ordered[rank]),
        );
        if self.replica_selection != ReplicaSelection::Spread {
            return order;
        }
        // Visit an older later owner before the newer owner that displaced it, which would fetch
        // the page again.
        let order = racer_core::read::lineage_first(
            order,
            &candidates
                .ordered
                .iter()
                .map(|node| candidates.membership.since(node))
                .collect::<Vec<_>>(),
            |rank| {
                candidates
                    .membership
                    .is_neighbor(&self.node, &ordered[rank])
            },
        );
        // Owners that left this node's request unanswered go after the others, silent ones last.
        let health = self.peers.link_health();
        racer_core::read::demote_unanswered(order, |rank| health.answering(&ordered[rank]))
    }

    /// Bound each local peer exchange independently of the signed operation ceiling.
    pub(crate) fn with_attempt_timeout(mut self, timeout: std::time::Duration) -> Self {
        self.attempt_timeout = timeout;
        self
    }

    /// How long rank 0 may stay silent on a page after an owner's probe of it timed out before the
    /// owner presumes it hung (half the client request timeout; see `racer_core::liveness`).
    pub(crate) fn with_rank0_hang_after(self, hang_after: std::time::Duration) -> Self {
        *self.rank0_watch.borrow_mut() = Rank0Watch::new(
            racer_core::liveness::WATCH_CAPACITY,
            racer_core::liveness::WATCH_TTL,
            hang_after,
        );
        self
    }

    pub fn candidates(
        &self,
        membership: std::sync::Arc<crate::topology::Membership>,
        object: &ObjectId,
        page: PageNumber,
    ) -> Candidates {
        self.placement.rank(membership, object, page)
    }

    pub fn is_candidate(&self, candidates: &Candidates) -> bool {
        candidates
            .ordered
            .iter()
            .take(3)
            .any(|node| node == &self.node)
    }
    /// Retention must not own this policy: its requester can retain the writer
    /// through transport reclamation, closing a cycle back to retention.
    pub(crate) fn ownership_callback(self: &Rc<Self>) -> Rc<dyn Fn(&crate::model::PageId) -> bool> {
        let policy = Rc::downgrade(self);
        Rc::new(move |page| {
            policy
                .upgrade()
                .is_some_and(|policy| policy.owns_current(page))
        })
    }
    pub(crate) fn ownership_pass(self: &Rc<Self>) -> crate::retention::OwnershipPass {
        let policy = Rc::downgrade(self);
        Rc::new(move || {
            let policy = policy.upgrade();
            let snapshot = policy
                .as_ref()
                .and_then(|p| p.published.current().ok().flatten());
            Rc::new(move |page| {
                policy
                    .as_ref()
                    .zip(snapshot.as_ref())
                    .is_some_and(|(policy, snapshot)| {
                        policy.placement.is_owner(
                            &snapshot.membership,
                            &page.version.object,
                            page.number,
                            &policy.node,
                        )
                    })
            })
        })
    }
    /// Whether `node` owns `page` under `membership` by a completed cached
    /// ranking; never ranks (successor hints).
    pub(crate) fn owns_cached(
        &self,
        membership: &crate::topology::Membership,
        page: &crate::model::PageId,
        node: &NodeId,
    ) -> bool {
        self.placement
            .is_owner(membership, &page.version.object, page.number, node)
    }

    /// Retention is a hint from CURRENT publication, not the read's leased route.
    /// This callback is lookup-only: no cold scoring or cache eviction. Missing
    /// and obsolete results contribute no bonus, never a storage pin.
    pub(super) fn owns_current(&self, page: &crate::model::PageId) -> bool {
        self.published
            .current()
            .ok()
            .flatten()
            .is_some_and(|snapshot| {
                self.placement.is_owner(
                    &snapshot.membership,
                    &page.version.object,
                    page.number,
                    &self.node,
                )
            })
    }

    /// Call once per worker service turn, also when no reads are arriving.
    pub fn refresh_ownership(
        &self,
        index: &crate::store::catalog::Index,
    ) -> Result<crate::topology::Maintenance> {
        let snapshot = self.published.current()?.ok_or(Error::Unavailable)?;
        let mut work = self.ownership_refresh.borrow_mut();
        if work.active.is_none() {
            // Continuous appends must not keep a pass chasing the moving tail
            // forever while earlier residents retain missing topology hints.
            if work.visited >= index.page_capacity() {
                work.cursor = 0;
                work.visited = 0;
                return Ok(crate::topology::Maintenance::Idle);
            }
            let (cursor, mut pages) = index.snapshot_pages(work.cursor, 1);
            work.cursor = cursor;
            work.active = pages.pop().map(|(page, _)| page);
            work.visited += usize::from(work.active.is_some());
        }
        let Some(page) = &work.active else {
            work.cursor = 0;
            work.visited = 0;
            return Ok(crate::topology::Maintenance::Idle);
        };
        if self
            .placement
            .refresh(
                snapshot.membership.clone(),
                &page.version.object,
                page.number,
            )
            .is_some()
        {
            work.active = None;
        }
        Ok(crate::topology::Maintenance::Progress)
    }

    /// Refresh accepted-current ownership before latching a read observation.
    /// This does not replace the read's leased placement or mint origin authority.
    pub(crate) fn refresh_ownership_scoped<'a>(
        &'a self,
        page: &crate::model::PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        let membership = self
            .published
            .current()
            .map_err(Error::from)
            .and_then(|s| s.map(|s| s.membership.clone()).ok_or(Error::Unavailable));
        let object = page.version.object.clone();
        let number = page.number;
        Box::pin(async move {
            self.candidates_scoped(membership?, &object, number, scope)
                .await?;
            Ok(())
        })
    }
    pub fn maintain(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
    ) -> crate::topology::Maintenance {
        self.placement.maintain(membership)
    }
    pub fn candidates_scoped<'a>(
        &'a self,
        membership: std::sync::Arc<crate::topology::Membership>,
        object: &ObjectId,
        page: PageNumber,
        scope: &'a RequestScope,
    ) -> Operation<'a, Candidates> {
        self.placement
            .rank_scoped(membership, object, page, Some(scope))
    }

    pub fn resolve<'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
    ) -> Operation<'a, CandidateResolution> {
        Box::pin(async move {
            let mut budget = super::default_budget(scope);
            self.resolve_with_budget(candidates, context, operation, scope, &mut budget)
                .await
        })
    }

    pub fn resolve_with_budget<'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, CandidateResolution> {
        self.resolve_epoch(candidates, context, operation, scope, budget, |response| {
            Box::pin(async move { Ok(response) })
        })
    }

    /// Resolve candidates with the walk alone.
    fn resolve_epoch<'a, T: 'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        mut validate: impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a,
    ) -> Operation<'a, CandidateResolution<T>> {
        Box::pin(async move {
            let mut a = self.acquisition(candidates, &operation, scope, false, true);
            let mut copy = None;
            let action = self
                .drive(
                    &mut a,
                    FillEvent::Start { local_hit: false },
                    context,
                    &operation,
                    scope,
                    budget,
                    &mut validate,
                    &mut copy,
                )
                .await?;
            match action {
                Action::Done(Ok(_)) => {
                    Ok(CandidateResolution::Copy(copy.expect("a validated copy")))
                }
                Action::Origin { rank } => {
                    self.authority(&mut a, rank, &operation);
                    Ok(CandidateResolution::Origin(
                        a.authority.take().expect("origin authority"),
                    ))
                }
                action => unreachable!("a walk-only acquisition asked for {action:?}"),
            }
        })
    }

    /// A read's acquisition over the caller's ranking `candidates`: the walk
    /// alone (`walk_only`), or the whole fill with a hedge when `hedge`.
    pub(crate) fn acquisition(
        &self,
        candidates: Candidates,
        operation: &PeerOperation,
        scope: &RequestScope,
        hedge: bool,
        walk_only: bool,
    ) -> Acquisition {
        // Without a timer, the walk skips a destination that is behind instead of waiting for it.
        let features = Features::PRODUCTION.with(Features::STALE_WAIT, self.can_wait());
        Acquisition {
            fill: Fill::new(FillConfig {
                operation: operation_kind(operation),
                hedge,
                walk_only,
                scope: scope.deadline.0,
                features,
            }),
            membership: candidates.membership.version.0,
            initial: Some(candidates),
            current: None,
            fallback: None,
            latest: None,
            previous: None,
            stale_previous: None,
            answer: None,
            dest: None,
            acquire: false,
            sent: uring_runtime::environment::now(),
            error: None,
            phase: Phase::Walk,
            authority: None,
            trail: CandidateTrail::default(),
        }
    }

    /// Run `acquisition` from `event` until it needs what only the caller performs: a hedge, an
    /// origin fetch, a publication, or the end. A validated copy lands in `copy`.
    #[allow(clippy::too_many_arguments)] // The driver borrows the read's distinct inputs.
    pub(crate) async fn drive<'a, T: 'a>(
        &'a self,
        a: &mut Acquisition,
        event: FillEvent<'_>,
        context: &'a OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        validate: &mut (impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a),
        copy: &mut Option<T>,
    ) -> Result<Action> {
        let (object, page) = operation_identity(operation);
        let now = uring_runtime::environment::now;
        let mut next = a.fill.on(event, budget, now());
        loop {
            let (action, evidence) = next;
            if !matches!(action, Action::Done(Err(_))) {
                a.error = None;
            }
            // The walk took the previous membership's ranking.
            if a.fallback
                .as_ref()
                .is_some_and(|c| c.membership.version.0 == a.fill.version)
            {
                a.current = a.fallback.take();
            }
            if evidence.notes & Notes::REMAINING != 0 {
                a.phase = Phase::Remaining;
                a.trail = CandidateTrail {
                    site: CandidateSite::RemainingCopy,
                    ..Default::default()
                };
            }
            self.apply(a, &evidence, operation, scope, budget);
            let event = match action {
                Action::Rank { ranking, version } => {
                    let ranked = self
                        .rank_for(
                            a, ranking, version, context, object, page, operation, scope, budget,
                        )
                        .await;
                    FillEvent::Ranked(ranked.map_err(|error| a.core_error(error)))
                }
                Action::Hints => {
                    let PeerOperation::Page { page: id, .. } = operation else {
                        unreachable!("only page owners ask for hints");
                    };
                    let current = a.current.as_ref().expect("ranked");
                    let membership = &current.membership;
                    let hints: Vec<u32> = self
                        .successors
                        .borrow_mut()
                        .probe(id, now())
                        .iter()
                        .filter_map(|node| membership.position(node).ok())
                        .map(|p| p as u32)
                        .collect();
                    let rank0 = match a.fill.rank {
                        Some(rank) if rank > 0 => {
                            self.rank0_watch
                                .borrow()
                                .state(id, &current.ordered[0], now())
                        }
                        _ => racer_core::liveness::Rank0State::Unknown,
                    };
                    next = a.fill.on(
                        FillEvent::Hints {
                            hints: &hints,
                            rank0,
                        },
                        budget,
                        now(),
                    );
                    continue;
                }
                Action::Refresh => {
                    let latest = match self.published.current() {
                        Ok(latest) => latest.map(|published| published.membership.clone()),
                        Err(error) => {
                            let error = a.core_error(Error::from(error));
                            next = a.fill.on(FillEvent::Abort(error), budget, now());
                            continue;
                        }
                    };
                    let version = latest.as_ref().map(|latest| latest.version.0);
                    a.latest = latest;
                    // Version - 1, when still resolvable by peers that adopted the current one
                    // (replaced within `PREVIOUS_MEMBERSHIP_WITHIN`) and listing this node.
                    let current = a.current.as_ref().expect("ranked");
                    a.stale_previous = current
                        .membership
                        .version
                        .0
                        .checked_sub(1)
                        .and_then(|previous| {
                            self.published
                                .resolve_recent(
                                    previous,
                                    now(),
                                    racer_core::read::walk::PREVIOUS_MEMBERSHIP_WITHIN,
                                )
                                .ok()
                                .flatten()
                        })
                        .filter(|previous| previous.member(&self.node).is_ok());
                    FillEvent::Membership {
                        latest: version,
                        previous_recent_member: a.stale_previous.is_some(),
                    }
                }
                Action::Probe(probe) => {
                    self.probe(a, probe, context, object, page, operation, scope, budget)
                        .await
                }
                Action::Validate => {
                    let response = a.answer.take().expect("an answer to validate");
                    let validated = validated_copy(response, operation, a.acquire, validate).await;
                    note_validation(&mut a.trail, &validated);
                    FillEvent::Validated(match validated {
                        Ok(Ok(validated)) => {
                            *copy = Some(validated);
                            Ok(None)
                        }
                        Ok(Err(outcome)) => Ok(Some(outcome)),
                        Err(error) => Err(a.core_error(error)),
                    })
                }
                Action::Batch {
                    first,
                    count,
                    remaining_opportunities,
                } => {
                    let current = a.current.as_ref().expect("ranked");
                    let membership = current.membership.clone();
                    let targets = &a.fill.handoff[first..first + count];
                    match self
                        .granted_batch(
                            &membership,
                            targets,
                            remaining_opportunities,
                            context,
                            operation,
                            scope,
                            budget.deadline(),
                            validate,
                            &mut a.trail,
                        )
                        .await
                    {
                        Ok(Some(validated)) => {
                            *copy = Some(validated);
                            FillEvent::BatchDone { copy: true }
                        }
                        Ok(None) => FillEvent::BatchDone { copy: false },
                        Err(error) => FillEvent::Abort(a.core_error(error)),
                    }
                }
                Action::Wait(wait) => {
                    if let Err(error) = self.stale_sleep(now() + wait, scope).await {
                        return Err(self.failed(a, error, &evidence, operation, scope, budget));
                    }
                    FillEvent::Timer
                }
                Action::Done(Err(error)) => {
                    let error = match (a.error.take(), error) {
                        (Some(error), racer_core::read::Error::Internal) => error,
                        (_, error) => Error::from(error),
                    };
                    return Err(self.failed(a, error, &evidence, operation, scope, budget));
                }
                Action::Origin { .. } => {
                    a.phase = Phase::Origin;
                    return Ok(action);
                }
                Action::Idle => unreachable!("one probe is outstanding at a time"),
                action => return Ok(action),
            };
            next = a.fill.on(event, budget, now());
        }
    }

    /// A ranking the walk asked for, as member positions of its membership.
    #[allow(clippy::too_many_arguments)] // The ranking borrows the read's distinct inputs.
    async fn rank_for(
        &self,
        a: &mut Acquisition,
        ranking: Ranking,
        version: Option<u64>,
        context: &OriginContext,
        object: &ObjectId,
        page: PageNumber,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &AcquisitionBudget,
    ) -> Result<Ranked> {
        let page_operation = matches!(operation, PeerOperation::Page { .. });
        match ranking {
            Ranking::Current => {
                check_budget(scope, budget)?;
                let candidates = match (version, a.initial.take()) {
                    (None, Some(candidates)) => {
                        if object != &context.object {
                            return Err(Error::InvalidRequest);
                        }
                        // Public Candidates values are not authority. Recompute before origin.
                        let expected = self
                            .candidates_scoped(candidates.membership.clone(), object, page, scope)
                            .await?;
                        if expected.ordered != candidates.ordered {
                            return Err(Error::IncompatibleMembership);
                        }
                        candidates
                    }
                    _ => {
                        let latest = a.latest.take().ok_or(Error::Unavailable)?;
                        self.candidates_scoped(latest, object, page, scope).await?
                    }
                };
                if candidates.ordered.is_empty() || candidates.ordered.len() > 3 {
                    return Err(Error::IncompatibleMembership);
                }
                let ranked = self.ranked(&candidates, object, page, page_operation);
                a.current = Some(candidates);
                a.previous = None;
                Ok(ranked)
            }
            Ranking::Previous => {
                // Ranking failures are not errors; the walk just goes without.
                let version = version.expect("a previous version");
                let current = a.current.as_ref().expect("ranked");
                let now = uring_runtime::environment::now();
                let previous = self
                    .published
                    .resolve(version, now)
                    .ok()
                    .flatten()
                    .ok_or(Error::Unavailable)?;
                // Ask predecessors under the previous membership only while peers that adopted the
                // current one still hold it in grace (`PREVIOUS_MEMBERSHIP_WITHIN`); a lease held
                // only here says nothing about a peer.
                let recent = self
                    .published
                    .resolve_recent(
                        version,
                        now,
                        racer_core::read::walk::PREVIOUS_MEMBERSHIP_WITHIN,
                    )
                    .ok()
                    .flatten()
                    .filter(|recent| recent.member(&self.node).is_ok());
                let previous = self
                    .candidates_scoped(previous, object, page, scope)
                    .await?;
                // Previous owners that are no longer members are not asked.
                let owners: List<u32, MAX_HANDOFF> = previous
                    .ordered
                    .iter()
                    .map(|node| {
                        current
                            .membership
                            .position(node)
                            .map_or(NOT_MEMBER, |p| p as u32)
                    })
                    .collect();
                let recent_member = recent.is_some();
                a.previous = recent;
                Ok(Ranked {
                    recent_member,
                    ..Ranked::new(version, &owners, None)
                })
            }
            Ranking::Fallback => {
                let previous = a.stale_previous.take().ok_or(Error::Unavailable)?;
                let previous = self
                    .candidates_scoped(previous, object, page, scope)
                    .await?;
                if previous.ordered.is_empty() || previous.ordered.len() > 3 {
                    return Err(Error::IncompatibleMembership);
                }
                let ranked = self.ranked(&previous, object, page, page_operation);
                a.fallback = Some(previous);
                Ok(ranked)
            }
            Ranking::Authority => {
                check_budget(scope, budget)?;
                if a.fill.funding {
                    a.trail.site = CandidateSite::Funding;
                }
                let current = a.current.as_ref().expect("ranked");
                let owners = positions(current);
                let rank = current.ordered.iter().position(|node| node == &self.node);
                Ok(Ranked::new(current.membership.version.0, &owners, rank))
            }
        }
    }

    /// What the walk needs from `candidates`: the owners as member
    /// positions, arrivals, the visit order, and the demoted members.
    fn ranked(
        &self,
        candidates: &Candidates,
        object: &ObjectId,
        page: PageNumber,
        page_operation: bool,
    ) -> Ranked {
        let membership = &candidates.membership;
        let rank = candidates
            .ordered
            .iter()
            .position(|node| node == &self.node);
        let mut ranked = Ranked::new(membership.version.0, &positions(candidates), rank);
        ranked.members = membership.members().len();
        // Arrival order of the page's owners in this node's membership chain.
        for (since, node) in ranked.since.iter_mut().zip(&candidates.ordered) {
            *since = membership.since(node);
        }
        match rank {
            None => ranked.order = self.visit_order(candidates, object, page),
            Some(_) if page_operation => {
                // Members ranked just below the owners that were owners when they arrived (from the
                // cached ranking only).
                let demoted = self.placement.demoted(membership, object, page);
                for node in demoted.iter() {
                    let Ok(p) = membership.position(node) else {
                        continue;
                    };
                    let index = ranked.demoted.len();
                    if ranked.demoted.push(p as u32) {
                        ranked.demoted_since[index] = membership.since(node);
                    }
                }
                ranked.oldest = Some(0);
            }
            Some(_) => {}
        }
        ranked
    }

    /// Send one probe the walk planned and report how it ended.
    #[allow(clippy::too_many_arguments)] // The probe borrows the read's distinct inputs.
    async fn probe(
        &self,
        a: &mut Acquisition,
        probe: Probe,
        context: &OriginContext,
        object: &ObjectId,
        page: PageNumber,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> FillEvent<'static> {
        let current = a.current.as_ref().expect("ranked");
        let membership = current.membership.clone();
        // Canonical ranks past the owners address handoff targets or, for a
        // non-owner, the overflow helper.
        let helper;
        let destination = match a.fill.dest(&probe) {
            Some(p) => {
                a.dest = Some(ProbeDest::Member(p));
                &membership.members()[p as usize].node
            }
            None => match self.fanout_helper(current, object, page) {
                Some(node) => {
                    helper = node;
                    a.dest = Some(ProbeDest::Helper(helper.clone()));
                    &helper
                }
                // Every candidate helper is excluded: a miss.
                None => return FillEvent::NoHelper,
            },
        };
        let request_membership = match (&a.previous, probe.previous) {
            (Some(previous), true) => previous.clone(),
            _ => membership.clone(),
        };
        let remaining = a.phase == Phase::Remaining;
        a.acquire = probe.mode == WalkMode::Acquire;
        a.sent = uring_runtime::environment::now();
        let mut timed_out = false;
        let response = self
            .request_traced(
                &request_membership,
                destination,
                context,
                operation,
                if a.acquire {
                    FetchMode::Acquire
                } else {
                    FetchMode::CopyOnly
                },
                scope,
                a.fill.request_budget(budget),
                probe.remaining_opportunities,
                if probe.route == RouteMode::FundedRoute {
                    RequestMode::FundedRoute
                } else {
                    RequestMode::SharedRoute
                },
                Some((&mut a.trail, probe.canonical as u8)),
                &mut timed_out,
            )
            .await;
        FillEvent::Reply(match response {
            Ok(response) => {
                let signed = response.origin().node() == destination;
                let kind = response.response().kind();
                if kind.has_payload() {
                    a.answer = Some(response);
                }
                FillReply::Answer { kind, signed }
            }
            // Sent but unanswered within the attempt's own bound.
            Err(_) if timed_out && !remaining => FillReply::TimedOut,
            Err(error) => FillReply::Failed {
                error: a.core_error(error),
                sent: true,
                scope_live: scope.check().is_ok(),
            },
        })
    }

    /// Apply what one event showed: silence, rank 0 and route evidence.
    fn apply(
        &self,
        a: &Acquisition,
        evidence: &Evidence,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &AcquisitionBudget,
    ) {
        // The remaining-copy walk records no evidence.
        if a.phase == Phase::Remaining {
            return;
        }
        let (Some(dest), Some(current)) = (&a.dest, &a.current) else {
            return;
        };
        let destination = match dest {
            ProbeDest::Member(p) => &current.membership.members()[*p as usize].node,
            ProbeDest::Helper(node) => node,
        };
        let health = self.peers.link_health();
        let page = match operation {
            PeerOperation::Page { page, .. } => Some(page),
            _ => None,
        };
        match (evidence.silence, page) {
            // The destination answered this node: it is not hung.
            (Some(SilenceNote::Answered), _) => health.silence_answered(destination),
            // It shed the request: busy, not hung. A reader's later walks visit it after the page's
            // other owners for a while.
            (Some(SilenceNote::Shed), Some(id)) => health.silence_shed(destination, id.clone()),
            // A probe of the page's rank 0 asks whether rank 0 answers at all: `poll_pings` sends
            // the ping. Any other timeout only demotes the owner in this node's visit order.
            (Some(SilenceNote::TimedOut { needed }), Some(id)) => {
                health.silence_timed_out(destination, id.clone(), a.sent, needed);
            }
            _ => {}
        }
        // Update the rank 0 watch from this owner's page probe of rank 0: a timeout notes the page
        // with the request's deadline, an answer signed by rank 0 itself forgets it.
        if let (Some(note), Some(id)) = (evidence.rank0, page) {
            let mut watch = self.rank0_watch.borrow_mut();
            match note {
                Rank0Note::TimedOut => watch.timed_out(
                    id.clone(),
                    destination.clone(),
                    uring_runtime::environment::now(),
                    budget.deadline().min(scope.deadline.0),
                ),
                Rank0Note::Failed => watch.answered_failure(id, destination),
                Rank0Note::Answered => watch.answered(id, destination),
            }
        }
        if let Some(Ok(outcome)) = &evidence.outcome {
            self.observe_route(&current.membership, destination, outcome);
        }
    }

    /// Record why the read failed and return the dataplane error.
    fn failed(
        &self,
        a: &mut Acquisition,
        mut error: Error,
        evidence: &Evidence,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &AcquisitionBudget,
    ) -> Error {
        if a.phase == Phase::Walk && a.fill.funding {
            a.trail.site = CandidateSite::Funding;
        }
        if evidence.notes & (Notes::EXHAUSTED | Notes::EXHAUSTED_FUNDING) != 0
            || (a.phase == Phase::Walk && error == Error::VersionUnavailable)
        {
            if let Err(stop) = check_budget(scope, budget) {
                error = stop;
            } else if error == Error::Unavailable {
                if evidence.notes & Notes::EXHAUSTED != 0 {
                    a.trail.site = CandidateSite::Exhausted;
                }
                self.observer.record(
                    Failure::new(Stage::CandidateExhausted, Error::Unavailable)
                        .request(scope)
                        .detail(Detail::Budget {
                            attempts: budget.remaining_attempts(),
                            links: budget.remaining_links(),
                        }),
                );
            }
        }
        match a.phase {
            Phase::Walk => self.record_final(
                operation,
                a.membership,
                scope,
                budget,
                error,
                std::mem::take(&mut a.trail),
            ),
            Phase::Remaining => match (a.fill.remaining_end, &a.authority) {
                (Some(RemainingEnd::None), Some(authority)) => {
                    // Record the miss diagnostic; the walk classifies it.
                    self.final_origin_miss(authority, operation, scope, budget);
                }
                _ => {
                    let membership = a
                        .current
                        .as_ref()
                        .map_or(a.membership, |c| c.membership.version.0);
                    self.record_final(
                        operation,
                        membership,
                        scope,
                        budget,
                        error,
                        std::mem::take(&mut a.trail),
                    );
                }
            },
            Phase::Origin => {}
        }
        error
    }

    /// The origin authority the walk ended with at `rank`.
    pub(crate) fn authority<'a>(
        &self,
        a: &'a mut Acquisition,
        rank: usize,
        operation: &PeerOperation,
    ) -> &'a OriginAuthority {
        let (object, page) = operation_identity(operation);
        let current = a.current.as_ref().expect("ranked");
        a.authority.insert(OriginAuthority {
            diagnostic: a.trail.clone(),
            membership: current.membership.clone(),
            node: self.node.clone(),
            object: object.clone(),
            owners: current.ordered.clone(),
            page,
            predecessor_evidence: a.fill.evidence.to_vec(),
            rank,
        })
    }

    /// Send a run of granted probes at once (`Step::Granted`): copy-only, each on its own grant
    /// (`racer_core::read::walk::granted_budget`), all sharing the first probe's time share, so the
    /// run costs one round trip instead of one per target.
    #[allow(clippy::too_many_arguments)] // The batch borrows the walk's distinct inputs.
    async fn granted_batch<'a, T>(
        &'a self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        targets: &[u32],
        remaining_opportunities: u32,
        context: &'a OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
        deadline: std::time::Instant,
        validate: &mut (impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a),
        trail: &mut CandidateTrail,
    ) -> Result<Option<T>> {
        let mut grants: Vec<(AcquisitionBudget, bool)> = targets
            .iter()
            .map(|_| (racer_core::read::walk::granted_budget(deadline), false))
            .collect();
        let node = |p: &u32| &membership.members()[*p as usize].node;
        let sent = uring_runtime::environment::now();
        let responses = futures::future::join_all(targets.iter().zip(grants.iter_mut()).map(
            |(target, (grant, timed_out))| {
                self.request_traced(
                    membership,
                    node(target),
                    context,
                    operation,
                    FetchMode::CopyOnly,
                    scope,
                    grant,
                    remaining_opportunities,
                    RequestMode::FundedRoute,
                    None,
                    timed_out,
                )
            },
        ))
        .await;
        // Silence evidence as for a sequential granted probe: the destination's own answer clears
        // it, a timeout only demotes (a granted target is never rank 0).
        if let PeerOperation::Page { page: id, .. } = operation {
            for ((target, (_, timed_out)), response) in targets.iter().zip(&grants).zip(&responses)
            {
                let destination = node(target);
                match response {
                    Ok(response) if response.origin().node() == destination => {
                        self.peers.link_health().silence_answered(destination);
                    }
                    Err(_) if *timed_out => {
                        let _ = self.peers.link_health().silence_timed_out(
                            destination,
                            id.clone(),
                            sent,
                            racer_core::liveness::NO_PING,
                        );
                    }
                    _ => {}
                }
            }
        }
        for response in responses {
            match response {
                Ok(response)
                    if matches!(
                        response.response(),
                        PeerResponse::StaleMembership | PeerResponse::Overloaded
                    ) => {}
                Ok(response) => {
                    let validated = validated_copy(response, operation, false, validate).await;
                    note_validation(trail, &validated);
                    match validated {
                        Ok(Ok(copy)) => return Ok(Some(copy)),
                        Ok(Err(_)) => {}
                        Err(error) => return Err(error),
                    }
                }
                Err(error)
                    if racer_core::read::Error::try_from(error)
                        .ok()
                        .and_then(racer_core::read::walk::transport_outcome)
                        .is_some()
                        || scope.check().is_ok() => {}
                Err(error) => return Err(error),
            }
        }
        Ok(None)
    }

    /// The member this non-owner asks to acquire `page` for it when an owner
    /// sheds the request (`racer_core::read::budget`): one of the page's
    /// hash-chosen helpers, never an owner and never this node, preferring a
    /// direct overlay neighbor so the exchange needs no relay.
    pub(crate) fn fanout_helper(
        &self,
        candidates: &Candidates,
        object: &ObjectId,
        page: PageNumber,
    ) -> Option<NodeId> {
        let membership = &candidates.membership;
        let members = membership.members();
        racer_core::read::budget::fanout_helper(
            object,
            page,
            &self.node,
            members.len(),
            |p| {
                let node = &members[p].node;
                node == &self.node || candidates.ordered.contains(node)
            },
            |p| membership.is_neighbor(&self.node, &members[p].node),
        )
        .map(|p| members[p].node.clone())
    }

    /// After an origin version miss, old copies on later candidates remain useful.
    /// This path never asks another node to start acquisition.
    pub fn remaining_copy<'a>(
        &'a self,
        candidates: &'a Candidates,
        context: &'a OriginContext,
        operation: &'a PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, Option<VerifiedResponse>> {
        self.remaining_validated_copy(candidates, context, operation, scope, budget, |response| {
            Box::pin(async move { Ok(response) })
        })
    }

    pub(crate) fn remaining_validated_copy<'a, T: 'a>(
        &'a self,
        candidates: &'a Candidates,
        context: &'a OriginContext,
        operation: &'a PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        mut validate: impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a,
    ) -> Operation<'a, Option<T>> {
        Box::pin(async move {
            let mut trail = CandidateTrail {
                site: CandidateSite::RemainingCopy,
                ..Default::default()
            };
            let result = async {
                check_budget(scope, budget)?;
                let rank = candidates
                    .ordered
                    .iter()
                    .position(|node| node == &self.node)
                    .ok_or(Error::Unauthorized)?;
                let mut walk = RemainingWalk::new(rank, candidates.ordered.len());
                let end = loop {
                    let probe = match walk.next_probe() {
                        Ok(probe) => probe,
                        Err(end) => break end,
                    };
                    let outcome = match self
                        .request_traced(
                            &candidates.membership,
                            &candidates.ordered[probe.canonical],
                            context,
                            operation,
                            FetchMode::CopyOnly,
                            scope,
                            budget,
                            probe.remaining_opportunities,
                            RequestMode::SharedRoute,
                            Some((&mut trail, probe.index as u8)),
                            &mut false,
                        )
                        .await
                    {
                        Ok(response) => {
                            let validated =
                                validated_copy(response, operation, false, &mut validate).await;
                            note_validation(&mut trail, &validated);
                            match validated? {
                                Ok(copy) => return Ok(Some(copy)),
                                Err(outcome) => WalkOutcome::Probe(outcome),
                            }
                        }
                        Err(error) => match racer_core::read::Error::try_from(error)
                            .ok()
                            .and_then(racer_core::read::walk::transport_outcome)
                        {
                            Some(outcome) => WalkOutcome::Transport(outcome),
                            None => return Err(error),
                        },
                    };
                    walk.complete(budget, outcome);
                };
                check_budget(scope, budget)?;
                match end {
                    RemainingEnd::Unavailable => Err(Error::Unavailable),
                    RemainingEnd::None | RemainingEnd::Copy => Ok(None),
                }
            }
            .await;
            if let Err(error) = &result {
                trail.site = CandidateSite::RemainingCopy;
                self.record_final(
                    operation,
                    candidates.membership.version.0,
                    scope,
                    budget,
                    *error,
                    trail,
                );
            }
            result
        })
    }

    /// Request a peer copy using the accepted membership and remaining budget.
    #[allow(clippy::too_many_arguments)] // Preserve explicit route and request authorization inputs.
    pub(super) async fn request(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        destination: &NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        mode: FetchMode,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        remaining_opportunities: u32,
    ) -> Result<VerifiedResponse> {
        self.request_mode(
            membership,
            destination,
            context,
            operation,
            mode,
            scope,
            budget,
            remaining_opportunities,
            RequestMode::SharedRoute,
        )
        .await
    }
    /// Request a peer copy using the selected delivery mode.
    #[allow(clippy::too_many_arguments)] // Forward route policy without introducing a parameter shim.
    async fn request_mode(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        destination: &NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        mode: FetchMode,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        remaining_opportunities: u32,
        request_mode: RequestMode,
    ) -> Result<VerifiedResponse> {
        self.request_traced(
            membership,
            destination,
            context,
            operation,
            mode,
            scope,
            budget,
            remaining_opportunities,
            request_mode,
            None,
            &mut false,
        )
        .await
    }
    /// Request a peer copy and record its route evidence when tracing is enabled.
    #[allow(clippy::too_many_arguments)] // Keep the optional borrowed trace separate from route state.
    async fn request_traced(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        destination: &NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        mode: FetchMode,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        remaining_opportunities: u32,
        request_mode: RequestMode,
        trail: Option<(&mut CandidateTrail, u8)>,
        timed_out: &mut bool,
    ) -> Result<VerifiedResponse> {
        match self
            .request_delivered(
                membership,
                destination,
                context,
                operation,
                mode,
                scope,
                budget,
                remaining_opportunities,
                request_mode,
                trail,
                None,
                timed_out,
            )
            .await?
        {
            Delivered::Response(response) => Ok(response),
            Delivered::Landed(_) => Err(Error::Internal),
        }
    }

    /// One CopyOnly page request for a `gpu-read` on GPU `ordinal`: the page may land in a GPU
    /// landing buffer (see [`Requester::request_landing`]).
    #[allow(clippy::too_many_arguments)] // Same route inputs as a plain request.
    pub(crate) async fn request_landing(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        destination: &NodeId,
        context: &OriginContext,
        page: &crate::model::PageId,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        ordinal: i32,
    ) -> Result<Delivered> {
        let operation = PeerOperation::Page {
            page: page.clone(),
            mode: FetchMode::CopyOnly,
        };
        self.request_delivered(
            membership,
            destination,
            context,
            &operation,
            FetchMode::CopyOnly,
            scope,
            budget,
            2,
            RequestMode::SharedRoute,
            None,
            Some(ordinal),
            &mut false,
        )
        .await
    }

    /// Whether this node can land peer pages on GPU `ordinal`.
    pub(crate) fn landing_ready(&self, ordinal: i32) -> bool {
        self.peers.landing_ready(ordinal)
    }

    /// Request a peer copy, optionally into a GPU landing buffer.
    #[allow(clippy::too_many_arguments)] // Keep the optional borrowed trace separate from route state.
    async fn request_delivered(
        &self,
        membership: &std::sync::Arc<crate::topology::Membership>,
        destination: &NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        mode: FetchMode,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        remaining_opportunities: u32,
        request_mode: RequestMode,
        trail: Option<(&mut CandidateTrail, u8)>,
        landing: Option<i32>,
        timed_out: &mut bool,
    ) -> Result<Delivered> {
        *timed_out = false;
        let mut remote = [b'?'; 36];
        if trail.is_some() && racer_crypto::identity::canonical_uuid(&destination.0) {
            remote.copy_from_slice(destination.0.as_bytes());
        }
        let mut facts = CandidateAttempt {
            destination: remote,
            membership: membership.version.0,
            rank: trail.as_ref().map_or(u8::MAX, |(_, rank)| *rank),
            acquire: matches!(mode, FetchMode::Acquire),
            attempt: None,
            site: CandidateSite::Prepare,
            raw: None,
            effective: None,
            stop: None,
            attempts: budget.remaining_attempts(),
            links: budget.remaining_links(),
            overall: if trail.is_some() {
                crate::telemetry::timestamp(budget.deadline())
            } else {
                0
            },
            share: 0,
            cap: 0,
        };
        let result = async {
            check_budget(scope, budget)?;
            // A destination this node holds silent fails at once, before any credit is spent; its
            // own pings go out.
            if !matches!(mode, FetchMode::Ping) && self.peers.link_health().silent(destination) {
                return Err(Error::Unavailable);
            }
            // racer-core reserves the complete route and remote credits before sending and derives
            // the share, local cap, and body budget.
            let now = uring_runtime::environment::now();
            let plan = racer_core::read::walk::plan_attempt(
                budget,
                racer_core::read::walk::AttemptInputs {
                    now,
                    caller_deadline: scope.deadline.0,
                    remaining_opportunities,
                    route: match request_mode {
                        RequestMode::SharedRoute => RouteMode::SharedRoute,
                        RequestMode::FundedRoute => RouteMode::FundedRoute,
                        RequestMode::DirectHedge => RouteMode::DirectHedge,
                    },
                    mode: if matches!(mode, FetchMode::Acquire) {
                        WalkMode::Acquire
                    } else {
                        WalkMode::CopyOnly
                    },
                    attempt_timeout: self.attempt_timeout,
                    failure_links: crate::topology::FAILURE_LINKS,
                },
            )?;
            let links = plan.links;
            let overall = plan.overall;
            let attempt_end = plan.attempt_end;
            let attempts = plan.remote_attempts;
            // The local exchange may time out before the signed contract. Shortening
            // the latter per attempt would make a later update renew provider authority.
            let signed_deadline = overall;
            if trail.is_some() {
                facts.overall = crate::telemetry::timestamp(overall);
                facts.share = crate::telemetry::timestamp(plan.share);
                facts.cap = crate::telemetry::timestamp(attempt_end);
            }
            // A clone shares cancellation: timing it out would cancel the caller too.
            let mut attempt_scope = RequestScope::new(scope.request, overall)?;
            attempt_scope.set_candidate_idle(plan.idle)?;
            attempt_scope.set_candidate_total(attempt_end)?;
            attempt_scope.body_deadlines = Some((overall, plan.share));
            attempt_scope.set_candidate_body_budget(plan.observation, plan.complete_by)?;
            let mut bytes = [0; 16];
            uring_runtime::environment::fill_random(&mut bytes).map_err(|_| Error::Unavailable)?;
            let attempt = AttemptId(bytes);
            facts.attempt = Some(attempt);
            let credentials = &self.credentials;
            // Sealing is synchronous, but can still use up a very short time share.
            let mut signed_scope = attempt_scope.clone();
            signed_scope.deadline.0 = signed_deadline;
            let origin = credentials.seal(context, attempt, &signed_scope);
            check_budget(scope, budget)?;
            let origin = origin.map_err(|error| match error {
                Error::DeadlineExceeded => Error::Unavailable,
                error => error,
            })?;
            let request = PeerRequest {
                operation: copy_operation(operation, mode),
                origin,
                route: RouteBudget {
                    membership: membership.version,
                    request: scope.request,
                    attempt,
                    destination: destination.clone(),
                    visited: vec![self.node.clone()],
                    remaining_links: links,
                    remaining_attempts: attempts,
                    deadline: Deadline(signed_deadline),
                },
            };
            let registration = scope.cancellation.subscribe()?;
            let mut exchange: Operation<'_, Delivered> = if let Some(ordinal) = landing {
                self.peers
                    .request_landing(request, membership.clone(), ordinal, &attempt_scope)
            } else {
                let exchange = if matches!(request_mode, RequestMode::DirectHedge) {
                    self.peers
                        .request_direct(request, membership.clone(), &attempt_scope)
                } else {
                    self.peers
                        .request(request, membership.clone(), &attempt_scope)
                };
                Box::pin(async move { exchange.await.map(Delivered::Response) })
            };
            let mut stopped = None;
            facts.site = CandidateSite::Exchange;
            let response = std::future::poll_fn(|cx| {
                registration.register(cx.waker());
                if stopped.is_none() {
                    stopped = check_budget(scope, budget)
                        .and_then(|()| attempt_scope.check())
                        .err();
                    if stopped.is_some() {
                        let _ = attempt_scope.cancel();
                    }
                }
                // Keep polling after cancellation: a timeout is not a completion
                // fence and must not release accepted I/O before the peer returns.
                exchange.as_mut().poll(cx)
            })
            .await;
            facts.raw = Some(match &response {
                Err(error) => CandidateOutcome::Error(*error),
                Ok(Delivered::Landed(_)) => CandidateOutcome::Success,
                Ok(Delivered::Response(response)) => match response.response() {
                    PeerResponse::Miss => CandidateOutcome::Miss,
                    PeerResponse::Unavailable => {
                        CandidateOutcome::ResponseError(Error::Unavailable)
                    }
                    PeerResponse::Overloaded => CandidateOutcome::ResponseError(Error::Overloaded),
                    PeerResponse::VersionUnavailable => {
                        CandidateOutcome::ResponseError(Error::VersionUnavailable)
                    }
                    PeerResponse::StaleMembership => {
                        CandidateOutcome::ResponseError(Error::IncompatibleMembership)
                    }
                    PeerResponse::OriginRejected => {
                        CandidateOutcome::ResponseError(Error::OriginRejected)
                    }
                    PeerResponse::OriginForbidden => {
                        CandidateOutcome::ResponseError(Error::OriginForbidden)
                    }
                    PeerResponse::NotFound => CandidateOutcome::ResponseError(Error::NotFound),
                    _ => CandidateOutcome::Success,
                },
            });
            match &response {
                Err(error) => self.observer.record(
                    Failure::new(Stage::CandidateExchange, *error)
                        .request(scope)
                        .attempt(attempt)
                        .detail(Detail::Budget {
                            attempts: budget.remaining_attempts(),
                            links: budget.remaining_links(),
                        }),
                ),
                Ok(Delivered::Landed(_)) => {}
                Ok(Delivered::Response(response)) => {
                    let error = match response.response() {
                        PeerResponse::Overloaded => Some(Error::Overloaded),
                        PeerResponse::Unavailable => Some(Error::Unavailable),
                        PeerResponse::VersionUnavailable => Some(Error::VersionUnavailable),
                        PeerResponse::StaleMembership => Some(Error::IncompatibleMembership),
                        _ => None,
                    };
                    if let Some(error) = error {
                        self.observer.record(
                            Failure::new(Stage::CandidateResponse, error)
                                .request(scope)
                                .attempt(attempt)
                                .detail(Detail::Budget {
                                    attempts: budget.remaining_attempts(),
                                    links: budget.remaining_links(),
                                }),
                        );
                    }
                }
            }
            // A verified `StaleMembership` comes from a node that could not resolve this request's
            // membership, so the remote credits it carried were never used: return them, and the
            // links past the hops that carried it to that node.
            if let Ok(Delivered::Response(response)) = &response
                && matches!(response.response(), PeerResponse::StaleMembership)
            {
                budget.refund_remote(attempts)?;
                budget.refund_links(racer_core::read::budget::stale_unused_links(
                    links,
                    response.forwarders().len() + 1,
                ))?;
            }
            check_budget(scope, budget)?;
            let stop = stopped.or_else(|| attempt_scope.check().err());
            facts.stop = stop;
            // The caller's scope and budget still hold here, so a deadline is the attempt's own
            // bound: the request was sent and is unanswered (`WalkOutcome::TimedOut`).
            match stop {
                Some(Error::DeadlineExceeded) => {
                    *timed_out = true;
                    Err(Error::Unavailable)
                }
                Some(error) => Err(error),
                None => match response {
                    Err(Error::DeadlineExceeded) => {
                        *timed_out = true;
                        Err(Error::Unavailable)
                    }
                    result => result,
                },
            }
        }
        .await;
        facts.effective = result.as_ref().err().copied();
        facts.attempts = budget.remaining_attempts();
        facts.links = budget.remaining_links();
        if let Some((trail, _)) = trail {
            trail.site = facts.site;
            trail.recent.push(facts);
        }
        result
    }

    /// This node fetched `page` from origin at `rank` among `owners`: when that is after rank 0,
    /// remember to offer rank 0 the copy later (`racer_core::read::hints`).
    pub(crate) fn note_orphan(
        &self,
        page: &crate::model::PageId,
        owners: &[NodeId],
        rank: usize,
        evidence: &[ProbeOutcome],
    ) -> bool {
        use racer_core::read::hints::{notice_indexes, should_notice};
        if !should_notice(Some(rank)) || owners.get(rank) != Some(&self.node) {
            return false;
        }
        let now = uring_runtime::environment::now();
        let mut noted = false;
        // Rank 0 and, for a holder at rank 2, rank 1 unless it answered this owner's walk.
        for index in notice_indexes(Some(rank), evidence) {
            let Some(target) = owners.get(usize::from(index)) else {
                continue;
            };
            if target == &self.node {
                continue;
            }
            self.notices
                .borrow_mut()
                .note((page.clone(), index), target.clone(), now);
            noted = true;
        }
        noted
    }

    /// This node's own origin request for `page`, made with authority at `rank` among `owners`,
    /// failed with `error`: above rank 0, leave retries after rank 0's own failures to rank 0 for a
    /// while (`racer_core::liveness`).
    pub(crate) fn note_origin_failure(
        &self,
        page: &crate::model::PageId,
        owners: &[NodeId],
        rank: usize,
        error: &Error,
    ) {
        if rank == 0
            || owners.get(rank) != Some(&self.node)
            || matches!(
                error,
                Error::VersionUnavailable
                    | Error::OriginRejected
                    | Error::OriginForbidden
                    | Error::NotFound
            )
        {
            return;
        }
        let Some(rank0) = owners.first() else {
            return;
        };
        self.rank0_watch.borrow_mut().failed(
            page.clone(),
            rank0.clone(),
            uring_runtime::environment::now(),
        );
    }

    /// Send due copy offers, at most [`MAX_OFFERS_IN_FLIGHT`] at a time, each on its own detached
    /// driver task and fixed route grant (never a reader's budget). `holds` reports whether this
    /// node still holds a page; a page it lost is dropped. Best effort: an offer that is not
    /// answered `Miss` by its destination is tried again on the notice's schedule or expires, and
    /// never reaches a reader. Call once per worker service turn.
    pub(crate) fn poll_offers(
        self: &Rc<Self>,
        now: std::time::Instant,
        holds: impl Fn(&crate::model::PageId) -> bool,
        metrics: &Metrics,
    ) {
        let membership = self
            .published
            .current()
            .ok()
            .flatten()
            .map(|published| published.membership.clone());
        // Offers are held per target site while a cut lasts; members without a site, or unknown,
        // share one group.
        let site = move |node: &NodeId| -> String {
            membership
                .as_ref()
                .and_then(|m| m.member(node).ok())
                .map(|m| m.site.clone())
                .unwrap_or_default()
        };
        while self.offers.get() < MAX_OFFERS_IN_FLIGHT {
            let Some(due) = self.notices.borrow_mut().pop_open(now, &site) else {
                return;
            };
            let group = site(&due.target);
            if !holds(&due.key.0) {
                self.notices.borrow_mut().release(&group);
                continue;
            }
            let Ok(permit) = uring_runtime::drivers::reserve() else {
                // No driver slot this turn: count it as a failed offer (local, so not a cut).
                let _ = self
                    .notices
                    .borrow_mut()
                    .offer_result(due, OfferEnd::Refused, group, now);
                return;
            };
            let flight = OfferFlight::new(self.offers.clone());
            let policy = self.clone();
            let metrics = metrics.clone();
            permit.submit_detached(Box::pin(async move {
                let _flight = flight;
                metrics.record(Event::OrphanOfferSent, 1);
                let end = policy.offer(&due).await.unwrap_or(OfferEnd::Cut);
                let now = uring_runtime::environment::now();
                match end {
                    OfferEnd::Dropped => {}
                    OfferEnd::Delivered => metrics.record(Event::OrphanOfferDelivered, 1),
                    OfferEnd::Refused | OfferEnd::Cut => {
                        metrics.record(Event::OrphanOfferFailed, 1);
                    }
                }
                let _ = policy
                    .notices
                    .borrow_mut()
                    .offer_result(due, end, group, now);
                Ok::<(), Error>(())
            }));
        }
    }

    /// This worker's link health (tests set its silence timing).
    #[cfg(test)]
    pub(crate) fn link_health(&self) -> Rc<crate::topology::LinkHealth> {
        self.peers.link_health()
    }

    /// Send due silence pings (`racer_core::liveness`), at most [`MAX_PINGS_IN_FLIGHT`] at a time,
    /// each on its own detached driver task and fixed route grant (never a reader's budget). Call
    /// once per worker service turn.
    pub(crate) fn poll_pings(self: &Rc<Self>, now: std::time::Instant, metrics: &Metrics) {
        let health = self.peers.link_health();
        let room = MAX_PINGS_IN_FLIGHT.saturating_sub(self.pings.get());
        if room == 0 {
            return;
        }
        for (peer, page) in health.due_pings(now, room) {
            let Ok(permit) = uring_runtime::drivers::reserve() else {
                // No driver slot this turn: the ping's slot goes stale and
                // the next timeout asks again.
                return;
            };
            let flight = OfferFlight::new(self.pings.clone());
            let policy = self.clone();
            let metrics = metrics.clone();
            let health = health.clone();
            permit.submit_detached(Box::pin(async move {
                let _flight = flight;
                metrics.record(Event::SilencePingSent, 1);
                let answered = policy.ping(&peer, &page).await;
                let now = uring_runtime::environment::now();
                if health.ping_outcome(&peer, answered, now) {
                    metrics.record(Event::SilencePeerSilent, 1);
                }
                Ok::<(), Error>(())
            }));
        }
    }

    /// Ping `peer` about `page` (mode `ping`, answered `Miss` at once), and
    /// classify the result (`racer_core::liveness::ping_verdict`).
    async fn ping(&self, peer: &NodeId, page: &crate::model::PageId) -> Option<bool> {
        use racer_core::read::hints::NOTICE_LINKS;
        let deadline = uring_runtime::environment::now() + self.attempt_timeout;
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).ok()?;
        let scope = RequestScope::new(crate::model::RequestId(id), deadline).ok()?;
        let membership = self.published.current().ok()??.membership.clone();
        let context = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let operation = PeerOperation::Page {
            page: page.clone(),
            mode: FetchMode::Ping,
        };
        // A fresh grant: one local exchange, no remote attempts, and `NOTICE_LINKS` route links.
        let mut budget = AcquisitionBudget::new(deadline, 1, NOTICE_LINKS);
        let mut timed_out = false;
        let response = self
            .request_traced(
                &membership,
                peer,
                &context,
                &operation,
                FetchMode::Ping,
                &scope,
                &mut budget,
                1,
                RequestMode::FundedRoute,
                None,
                &mut timed_out,
            )
            .await;
        racer_core::liveness::ping_verdict(match response {
            Ok(response) => Ok((
                response.origin().node() == peer,
                matches!(response.response(), PeerResponse::Unavailable),
            )),
            Err(error) => Err(timed_out || matches!(error, Error::DeadlineExceeded)),
        })
    }

    /// Send one copy offer for `due` to the page's current rank 0, and classify how it ended.
    /// Errors before sending are cut evidence too (membership or route lookup).
    async fn offer(
        &self,
        due: &racer_core::read::hints::Due<(crate::model::PageId, u8), NodeId>,
    ) -> Result<OfferEnd> {
        use racer_core::read::hints::{NOTICE_LINKS, notice_target};
        let (page, index) = (&due.key.0, due.key.1);
        let deadline = uring_runtime::environment::now() + self.attempt_timeout;
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Unavailable)?;
        let scope = RequestScope::new(crate::model::RequestId(id), deadline)?;
        let membership = self
            .published
            .current()?
            .ok_or(Error::Unavailable)?
            .membership
            .clone();
        let mut owners = self
            .candidates_scoped(
                membership.clone(),
                &page.version.object,
                page.number,
                &scope,
            )
            .await?
            .ordered;
        owners.truncate(3);
        let Some(target) = notice_target(&self.node, &owners, true, index) else {
            return Ok(OfferEnd::Dropped);
        };
        let context = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let operation = PeerOperation::Page {
            page: page.clone(),
            mode: FetchMode::Offer,
        };
        // A fresh grant: one local exchange, no remote attempts, and `NOTICE_LINKS` route links.
        let mut budget = AcquisitionBudget::new(deadline, 1, NOTICE_LINKS);
        let response = self
            .request_mode(
                &membership,
                &target,
                &context,
                &operation,
                FetchMode::Offer,
                &scope,
                &mut budget,
                1,
                RequestMode::FundedRoute,
            )
            .await;
        Ok(OfferEnd::of(response.ok().and_then(|response| {
            (response.origin().node() == &target)
                .then(|| matches!(response.response(), PeerResponse::Miss))
        })))
    }

    pub fn origin_miss_error(&self, authority: &OriginAuthority) -> Error {
        racer_core::read::origin_miss_error(&authority.predecessor_evidence).into()
    }
    pub(super) fn final_origin_miss(
        &self,
        authority: &OriginAuthority,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &AcquisitionBudget,
    ) -> Error {
        let error = self.origin_miss_error(authority);
        let mut trail = authority.diagnostic.clone();
        trail.site = CandidateSite::OriginMiss;
        self.record_final(
            operation,
            authority.membership.version.0,
            scope,
            budget,
            error,
            trail,
        );
        error
    }
    fn record_final(
        &self,
        operation: &PeerOperation,
        membership: u64,
        scope: &RequestScope,
        budget: &AcquisitionBudget,
        error: Error,
        trail: CandidateTrail,
    ) {
        let kind = match operation {
            PeerOperation::Page { .. } => CandidateOperation::Page,
            PeerOperation::Bootstrap { .. } => CandidateOperation::Bootstrap,
            PeerOperation::Metadata { .. } => CandidateOperation::Metadata,
            PeerOperation::Subscribe { .. } => CandidateOperation::Subscribe,
            PeerOperation::Warm { .. } => CandidateOperation::Page,
        };
        self.observer.candidate_final(CandidateFinal {
            failure: Failure::new(Stage::CandidateExhausted, error)
                .request(scope)
                .detail(Detail::Budget {
                    attempts: budget.remaining_attempts(),
                    links: budget.remaining_links(),
                }),
            operation: kind,
            page: operation_identity(operation).1.0,
            membership,
            trail,
        });
    }
}

fn note_validation<T>(
    trail: &mut CandidateTrail,
    result: &Result<std::result::Result<T, ProbeOutcome>>,
) {
    let outcome = match result {
        Err(error) => CandidateOutcome::Error(*error),
        Ok(Err(ProbeOutcome::UnusableCopy)) => CandidateOutcome::UnusableCopy,
        _ => return,
    };
    if let Some((_, mut facts)) = trail.recent.iter().last() {
        facts.site = CandidateSite::Validation;
        facts.raw = Some(outcome);
        facts.effective = result.as_ref().err().copied();
        trail.recent.push(facts);
    }
    trail.site = CandidateSite::Validation;
}

#[cfg(test)]
#[test]
fn candidate_final_validation_tail_preserves_identity_and_omissions() {
    let mut trail = CandidateTrail::default();
    let facts = CandidateAttempt {
        destination: [b'f'; 36],
        membership: 9,
        rank: 2,
        acquire: true,
        attempt: Some(AttemptId([7; 16])),
        site: CandidateSite::Exchange,
        raw: Some(CandidateOutcome::Success),
        effective: None,
        stop: None,
        attempts: 3,
        links: 4,
        overall: 100,
        share: 50,
        cap: 75,
    };
    trail.recent.push(facts);
    note_validation::<()>(&mut trail, &Ok(Ok(())));
    assert_eq!(trail.recent.total(), 1);
    note_validation::<()>(&mut trail, &Ok(Err(ProbeOutcome::UnusableCopy)));
    let (_, unusable) = trail.recent.iter().last().unwrap();
    assert!(matches!(unusable.raw, Some(CandidateOutcome::UnusableCopy)));
    assert!(unusable.effective.is_none());
    assert_eq!(unusable.attempt, facts.attempt);
    assert_eq!(unusable.destination, facts.destination);
    note_validation::<()>(&mut trail, &Err(Error::Unauthorized));
    let (_, rejected) = trail.recent.iter().last().unwrap();
    assert!(matches!(
        rejected.raw,
        Some(CandidateOutcome::Error(Error::Unauthorized))
    ));
    assert_eq!(rejected.effective, Some(Error::Unauthorized));
    assert_eq!((rejected.attempts, rejected.links), (3, 4));
    for _ in 0..3 {
        trail.recent.push(facts);
    }
    assert_eq!(trail.recent.len(), 4);
    assert_eq!(trail.recent.total() - trail.recent.len() as u64, 2);
}

fn check_budget(scope: &RequestScope, budget: &AcquisitionBudget) -> Result<()> {
    scope.check()?;
    if uring_runtime::environment::now() >= budget.deadline() {
        Err(Error::DeadlineExceeded)
    } else {
        Ok(())
    }
}

/// A validated peer copy or the evidence authorizing an origin request.
#[allow(clippy::large_enum_variant)] // Keep origin evidence inline without allocating per resolution.
pub enum CandidateResolution<T = VerifiedResponse> {
    Copy(T),

    Origin(OriginAuthority),
}

async fn validated_copy<'a, T>(
    response: VerifiedResponse,
    operation: &PeerOperation,
    acquire: bool,
    validate: &mut (impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a),
) -> Result<std::result::Result<T, ProbeOutcome>> {
    let result = match classify(response.response(), operation, acquire) {
        Ok(Some(outcome)) => return Ok(Err(outcome)),
        Ok(None) => validate(response).await,
        Err(error) => Err(error),
    };
    match result {
        Ok(copy) => Ok(Ok(copy)),
        // Only page content validation is recoverable here. Transport/security
        // errors never enter this helper, and Unauthorized remains terminal.
        Err(error)
            if racer_core::read::Error::try_from(error).is_ok_and(|error| {
                racer_core::read::unusable_copy(error, operation_kind(operation)).is_some()
            }) =>
        {
            Ok(Err(ProbeOutcome::UnusableCopy))
        }
        Err(error) => Err(error),
    }
}
impl OriginAuthority {
    /// The canonical rank this authority was granted at.
    pub(crate) fn rank(&self) -> usize {
        self.rank
    }

    /// The page's ranked owners `rank` indexes: those of the membership the
    /// walk ended under, which may be newer than the caller's.
    pub(crate) fn owners(&self) -> &[NodeId] {
        &self.owners
    }

    /// The predecessors' answers this authority rests on, in rank order.
    pub(crate) fn predecessor_evidence(&self) -> &[ProbeOutcome] {
        &self.predecessor_evidence
    }

    pub fn validate(&self, object: &ObjectId, page: PageNumber) -> Result<()> {
        racer_core::read::validate_origin_authority(
            racer_core::read::AuthorityFacts {
                object: &self.object,
                page: self.page,
                rank: self.rank,
                predecessor_evidence: self.predecessor_evidence.len(),
                member: self.membership.member(&self.node).is_ok(),
            },
            object,
            page,
        )
        .map_err(Error::from)
    }
}

fn operation_identity(operation: &PeerOperation) -> (&ObjectId, PageNumber) {
    match operation {
        PeerOperation::Subscribe { subscription, .. } => (
            &subscription.version.object,
            PageNumber(
                subscription
                    .demand
                    .intervals()
                    .first()
                    .map_or(0, |i| i.start),
            ),
        ),
        PeerOperation::Page { page, .. } => (&page.version.object, page.number),
        PeerOperation::Metadata { object, .. } => (object, PageNumber(0)),
        PeerOperation::Bootstrap { object, .. } => (object, PageNumber(0)),
        PeerOperation::Warm {
            version, action, ..
        } => (
            &version.object,
            match action {
                crate::peer::protocol::WarmAction::Fill { page, .. } => *page,
                _ => PageNumber(0),
            },
        ),
    }
}
fn copy_operation(operation: &PeerOperation, mode: FetchMode) -> PeerOperation {
    match operation {
        PeerOperation::Subscribe { subscription, .. } => PeerOperation::Subscribe {
            subscription: subscription.clone(),
            mode,
        },
        PeerOperation::Bootstrap { object, .. } => PeerOperation::Bootstrap {
            object: object.clone(),
            mode,
        },
        PeerOperation::Page { page, .. } => PeerOperation::Page {
            page: page.clone(),
            mode,
        },
        PeerOperation::Metadata {
            object, selector, ..
        } => PeerOperation::Metadata {
            object: object.clone(),
            selector: selector.clone(),
            mode,
        },
        // Warm actions are always acquire-mode; the probe mode is not applied.
        PeerOperation::Warm {
            version,
            lease,
            action,
        } => PeerOperation::Warm {
            version: version.clone(),
            lease: *lease,
            action: action.clone(),
        },
    }
}
/// Payload-free shape of `operation` for racer-core classification.
pub(crate) fn operation_kind(operation: &PeerOperation) -> racer_core::read::OperationKind {
    use racer_core::read::OperationKind;
    match operation {
        PeerOperation::Subscribe { .. } => OperationKind::Subscribe,
        PeerOperation::Page { .. } => OperationKind::Page,
        PeerOperation::Metadata { selector, .. } => OperationKind::Metadata {
            fresh: matches!(selector, MetadataSelector::Fresh),
        },
        PeerOperation::Bootstrap { .. } => OperationKind::Bootstrap,
        PeerOperation::Warm { .. } => OperationKind::Warm,
    }
}

// `acquire` describes the actual probe, which may be CopyOnly even when the caller's operation
// requests acquisition (predecessors and remaining copies).
fn classify(
    response: &PeerResponse,
    operation: &PeerOperation,
    acquire: bool,
) -> Result<Option<ProbeOutcome>> {
    racer_core::read::classify(response.kind(), operation_kind(operation), acquire, || {
        validate_payload(response, operation)
    })
}

/// Payload identity and content checks for a response whose kind answers the
/// operation's kind. Mismatches are corrupt records.
fn validate_payload(response: &PeerResponse, operation: &PeerOperation) -> Result<()> {
    match (response, operation) {
        (
            PeerResponse::Selected {
                metadata,
                ciphertext,
                grant,
            },
            PeerOperation::Subscribe { subscription, .. },
        ) if grant.page.version == subscription.version
            && subscription.demand.contains(grant.page.number.0)
            && ciphertext.envelope().page == grant.page
            && metadata.version == subscription.version =>
        {
            metadata
                .immutable()
                .validate_page(ciphertext.envelope())
                .map_err(Error::from)
        }
        (
            PeerResponse::Bootstrap {
                metadata,
                page_zero,
            },
            PeerOperation::Bootstrap { object, .. },
        ) if &metadata.version.object == object => {
            if let Some(page) = page_zero {
                metadata.immutable().validate_page(page.envelope())?;
                if page.envelope().page.number.0 != 0 {
                    return Err(Error::CorruptRecord);
                }
            }
            if page_zero.is_some() != (metadata.length != 0) {
                return Err(Error::CorruptRecord);
            }
            Ok(())
        }
        (
            PeerResponse::Page {
                metadata,
                ciphertext,
            },
            PeerOperation::Page { page, .. },
        ) if &ciphertext.envelope().page == page && metadata.version == page.version => metadata
            .immutable()
            .validate_page(ciphertext.envelope())
            .map_err(Error::from),
        (
            PeerResponse::Metadata(metadata),
            PeerOperation::Metadata {
                object, selector, ..
            },
        ) if &metadata.version.object == object => {
            if let MetadataSelector::Pinned(etag) = selector
                && &metadata.version.etag != etag
            {
                return Err(Error::CorruptRecord);
            }
            Ok(())
        }
        _ => Err(Error::CorruptRecord),
    }
}

// Opt-in, node-shared speculative page hedging (`RACER_PAGE_HEDGE_SLOTS=0` disables it;
// `RACER_PAGE_HEDGE_DELAY_MS` defaults to 100, `RACER_PAGE_HEDGE_BYTES` to one duplicate
// plaintext+ciphertext pair).

pub const DUPLICATE_BYTES: usize =
    racer_core::read::duplicate_bytes(crate::model::PAGE_BYTES as usize);
#[derive(Clone, Copy)]
pub struct HedgeConfig {
    pub delay: Duration,
    pub slots: usize,
    pub bytes: usize,
}
impl Default for HedgeConfig {
    fn default() -> Self {
        Self {
            delay: Duration::from_millis(100),
            slots: 0,
            bytes: DUPLICATE_BYTES,
        }
    }
}
impl HedgeConfig {
    pub fn validate(self) -> Result<()> {
        if self.slots > 32
            || self.delay.is_zero()
            || self.delay > Duration::from_secs(30)
            || self.bytes > DUPLICATE_BYTES * 32
            || (self.slots > 0 && self.bytes < DUPLICATE_BYTES)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
pub(crate) struct Hedges {
    config: HedgeConfig,
    capacity: Arc<flow_control::Hedges>,
    metrics: Metrics,
}
pub(crate) struct Permit {
    owner: Arc<Hedges>,
    capacity: flow_control::HedgePermit,
}
impl Hedges {
    pub(crate) fn for_worker(&self, metrics: Metrics) -> Arc<Self> {
        Arc::new(Self {
            config: self.config,
            capacity: self.capacity.clone(),
            metrics,
        })
    }
    pub(crate) fn new(config: HedgeConfig, metrics: Metrics) -> Result<Arc<Self>> {
        config.validate()?;
        Ok(Arc::new(Self {
            config,
            metrics,
            capacity: flow_control::Hedges::new(config.slots, config.bytes),
        }))
    }
    pub(crate) fn enabled(&self) -> bool {
        self.config.slots > 0
    }
    pub(crate) fn suppressed(&self) {
        self.metrics.record(Event::PageHedgeSuppressed, 1);
    }
    pub(crate) fn acquire(self: &Arc<Self>) -> Result<Permit> {
        let capacity = self.capacity.acquire(
            DUPLICATE_BYTES,
            uring_runtime::environment::now() + self.config.delay,
        )?;
        Ok(Permit {
            owner: self.clone(),
            capacity,
        })
    }
    pub(crate) fn poll(&self) {
        self.capacity.poll(uring_runtime::environment::now());
    }
}
impl Permit {
    pub(crate) fn delay(&self, cx: &mut Context<'_>) -> Poll<()> {
        self.capacity.delay(uring_runtime::environment::now(), cx)
    }
    pub(crate) fn started(&self) {
        self.owner.metrics.record(Event::PageHedgeStarted, 1);
        self.owner
            .metrics
            .record(Event::PageHedgeDuplicateBytes, DUPLICATE_BYTES as u64);
    }
    pub(crate) fn won(&self) {
        self.owner.metrics.record(Event::PageHedgeWon, 1);
    }
}
impl uring_runtime::drivers::HedgeScope for RequestScope {
    fn cancel(&self) {
        let _ = RequestScope::cancel(self);
    }
}
impl uring_runtime::drivers::HedgePolicy<Error> for &Permit {
    fn delay(&mut self, cx: &mut Context<'_>) -> Poll<()> {
        Permit::delay(self, cx)
    }
    fn recoverable(&self, error: Error) -> bool {
        recoverable(error)
    }
    fn failure(&mut self) -> Error {
        Error::Unavailable
    }
    fn won(&mut self, contender: uring_runtime::drivers::Contender) {
        if contender == uring_runtime::drivers::Contender::Secondary {
            Permit::won(self);
        }
    }
}

/// Validation is inside each future. Cancellation is requested, never mistaken
/// for completion: even a validated winner waits for the losing exchange fence.
pub(crate) async fn race(
    primary: impl std::future::Future<Output = Result<crate::memory::PageResult>>,
    secondary: impl std::future::Future<Output = Result<crate::memory::PageResult>>,
    primary_scope: &crate::runtime::RequestScope,
    secondary_scope: &crate::runtime::RequestScope,
    parent: &crate::runtime::RequestScope,
    permit: &Permit,
) -> Result<crate::memory::PageResult> {
    uring_runtime::drivers::race(
        primary,
        secondary,
        primary_scope,
        secondary_scope,
        parent,
        permit,
    )
    .await
}
fn recoverable(error: Error) -> bool {
    racer_core::read::Error::try_from(error).is_ok_and(racer_core::read::recoverable)
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::ExpiresAt;
    use crate::model::ObjectMetadata;
    use crate::model::ObjectVersion;
    use crate::model::StrongEtag;
    use racer_control_wire::CacheId;
    #[test]
    fn candidate_failure_routes_spend_initial_allowance_with_four_then_eight_link_ceiling() {
        struct Routes(RefCell<Vec<(u8, u32)>>);
        impl Routes {
            fn direct_hedge_available(
                &self,
                _: &std::sync::Arc<crate::topology::Membership>,
                _: &NodeId,
            ) -> bool {
                false
            }
            fn request_direct<'a>(
                &'a self,
                _: PeerRequest,
                _: std::sync::Arc<crate::topology::Membership>,
                _: &'a RequestScope,
            ) -> Operation<'a, VerifiedResponse> {
                panic!("route-budget fixture does not admit direct hedges")
            }
            fn request<'a>(
                &'a self,
                request: PeerRequest,
                _: std::sync::Arc<crate::topology::Membership>,
                _: &'a RequestScope,
            ) -> Operation<'a, VerifiedResponse> {
                self.0.borrow_mut().push((
                    request.route.remaining_links,
                    request.route.remaining_attempts,
                ));
                Box::pin(async { Err(Error::Unavailable) })
            }
        }
        let (membership, placement, context, scope, credentials) = fixture();
        let peers = Rc::new(Routes(RefCell::new(Vec::new())));
        let policy = CandidatePolicy::new(
            NodeId("outside".into()),
            placement,
            Requester::scripted(
                peers.clone(),
                Routes::direct_hedge_available,
                Routes::request,
                Routes::request_direct,
            ),
            credentials,
            Arc::new(controlplane::Published::new(
                crate::control::Snapshot::retention(2),
            )),
        );
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let result = futures::executor::block_on(policy.resolve_with_budget(
            policy.candidates(membership, &context.object, PageNumber(0)),
            &context,
            PeerOperation::Metadata {
                object: context.object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            },
            &scope,
            &mut budget,
        ));
        assert!(matches!(result, Err(Error::Unavailable)));
        let routes = peers.0.borrow();
        assert_eq!(
            routes.iter().map(|(links, _)| *links).collect::<Vec<_>>(),
            vec![4, 8, 8]
        );
        assert_eq!(
            routes.iter().map(|(_, credits)| *credits).sum::<u32>()
                + routes.len() as u32
                + budget.remaining_attempts(),
            16
        );
        assert_eq!(budget.remaining_links(), 4);
    }
    fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("33333333-3333-4333-8333-333333333333".into()),
            key: CacheKey([0; 32]),
        }
    }
    struct ProbePeer {
        calls: RefCell<Vec<(NodeId, bool)>>,
        error: Error,
    }
    impl ProbePeer {
        fn direct_hedge_available(
            &self,
            _: &std::sync::Arc<crate::topology::Membership>,
            _: &NodeId,
        ) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("probe fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            let copy = matches!(
                request.operation,
                PeerOperation::Metadata {
                    mode: FetchMode::CopyOnly,
                    ..
                } | PeerOperation::Page {
                    mode: FetchMode::CopyOnly,
                    ..
                }
            );
            self.calls
                .borrow_mut()
                .push((request.route.destination, copy));
            Box::pin(async move { Err(self.error) })
        }
    }
    pub(crate) fn fixture() -> (
        std::sync::Arc<crate::topology::Membership>,
        Rc<Placement>,
        OriginContext,
        RequestScope,
        Rc<CredentialCrypto>,
    ) {
        use crate::admission::AdmissionPolicy;
        use crate::model::RequestId;
        use crate::topology::Member;
        use crate::topology::Membership;
        use racer_control_wire::ClusterId;
        use racer_control_wire::MembershipVersion;
        use racer_crypto::identity::KeyEpochs;
        use racer_crypto::identity::Keyring;
        let membership = std::sync::Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..4)
                    .map(|n| Member {
                        node: NodeId(format!("22222222-2222-4222-8222-{n:012}")),
                        shares: std::num::NonZeroU32::new(4).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 8000 + n),
                        rails: vec![],
                        site: "site1".into(),
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let context = OriginContext {
            object: object(),
            metadata: None,
            authorization: None,
        };
        let scope = RequestScope::new(
            RequestId([0; 16]),
            Instant::now() + std::time::Duration::from_secs(60),
        )
        .unwrap();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let keys = Rc::new(Keyring::new(
            ClusterId("cluster".into()),
            NodeId("local".into()),
            std::sync::Arc::new(KeyEpochs::default()),
        ));
        (
            membership,
            Rc::new(Placement::new(16)),
            context,
            scope,
            Rc::new(CredentialCrypto::new(keys, admission)),
        )
    }
    fn spread_membership(count: usize) -> std::sync::Arc<crate::topology::Membership> {
        use crate::topology::Member;
        use crate::topology::Membership;
        std::sync::Arc::new(
            Membership::validate(
                racer_control_wire::MembershipVersion(1),
                (0..count)
                    .map(|i| Member {
                        node: NodeId(format!("33333333-3333-4333-8333-{i:012}")),
                        shares: std::num::NonZeroU32::new(4).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 9000 + i),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        )
    }

    fn spread_policy(node: NodeId, selection: ReplicaSelection) -> CandidatePolicy {
        let peers = Rc::new(RecordedPeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        policy(node, peers).with_replica_selection(selection)
    }

    #[test]
    fn policy_visit_order_is_deterministic_and_neighbor_first_on_large_membership() {
        let membership = spread_membership(100);
        let placement = Placement::new(64);
        let mut neighbor_first = 0;
        let mut relayed_first_primary = 0;
        let mut relayed_first_spread = 0;
        for requester in membership.members().iter().take(20) {
            let spread = spread_policy(requester.node.clone(), ReplicaSelection::Spread);
            let primary = spread_policy(requester.node.clone(), ReplicaSelection::Primary);
            for page in 0..64 {
                let candidates = placement.rank(membership.clone(), &object(), PageNumber(page));
                if candidates.ordered.contains(&requester.node) {
                    continue;
                }
                let order = spread.visit_order(&candidates, &object(), PageNumber(page));
                assert_eq!(
                    order,
                    spread.visit_order(&candidates, &object(), PageNumber(page)),
                    "same requester and page must keep the same replica"
                );
                assert_eq!(
                    primary
                        .visit_order(&candidates, &object(), PageNumber(page))
                        .ranks()
                        .collect::<Vec<_>>(),
                    vec![0, 1, 2]
                );
                let near: Vec<bool> = order
                    .ranks()
                    .map(|rank| membership.is_neighbor(&requester.node, &candidates.ordered[rank]))
                    .collect();
                // Neighbors strictly precede non-neighbors.
                assert!(near.windows(2).all(|w| w[0] || !w[1]), "{near:?}");
                let mut ranks: Vec<_> = order.ranks().collect();
                ranks.sort_unstable();
                assert_eq!(ranks, vec![0, 1, 2]);
                neighbor_first += usize::from(near[0]);
                relayed_first_spread += usize::from(!near[0]);
                relayed_first_primary +=
                    usize::from(!membership.is_neighbor(&requester.node, &candidates.ordered[0]));
            }
        }
        assert!(neighbor_first > 0);
        assert!(
            relayed_first_spread < relayed_first_primary,
            "spread {relayed_first_spread} must relay less than primary {relayed_first_primary}"
        );
    }

    /// A reader whose first choice arrived in the membership chain after a later owner visits that
    /// older owner first, which may hold the copy it fetched while it ranked higher.
    #[test]
    fn spread_visits_an_older_later_owner_before_a_newer_owner() {
        use crate::topology::Membership;
        let all = spread_membership(12);
        let placement = Placement::new(64);
        let mut moved = 0;
        for page in 0..32 {
            let ranked = placement.rank(all.clone(), &object(), PageNumber(page));
            let newest = ranked.ordered[0].clone();
            // `newest` arrives in version 2; the others were in version 1.
            let before = Membership::validate(
                racer_control_wire::MembershipVersion(1),
                all.members()
                    .iter()
                    .filter(|m| m.node != newest)
                    .cloned()
                    .collect(),
            )
            .unwrap();
            let chain = std::sync::Arc::new(
                Membership::validate_with_predecessor(
                    racer_control_wire::MembershipVersion(2),
                    all.members().to_vec(),
                    Some(&before),
                )
                .unwrap(),
            );
            let candidates = placement.rank(chain.clone(), &object(), PageNumber(page));
            assert_eq!(candidates.ordered, ranked.ordered);
            for requester in all.members() {
                if candidates.ordered.contains(&requester.node) {
                    continue;
                }
                let spread = spread_policy(requester.node.clone(), ReplicaSelection::Spread);
                let plain = spread
                    .visit_order(&ranked, &object(), PageNumber(page))
                    .ranks()
                    .collect::<Vec<_>>();
                let lineage = spread
                    .visit_order(&candidates, &object(), PageNumber(page))
                    .ranks()
                    .collect::<Vec<_>>();
                if plain[0] == 0 {
                    // The newer rank 0 goes after the first older owner.
                    let mut expected = plain.clone();
                    expected[..2].rotate_left(1);
                    assert_eq!(lineage, expected, "page {page}");
                    moved += 1;
                } else {
                    assert_eq!(lineage, plain, "page {page}");
                }
                let primary = spread_policy(requester.node.clone(), ReplicaSelection::Primary);
                assert_eq!(
                    primary
                        .visit_order(&candidates, &object(), PageNumber(page))
                        .ranks()
                        .collect::<Vec<_>>(),
                    vec![0, 1, 2]
                );
            }
        }
        assert!(moved > 0);
    }

    #[test]
    fn spread_splits_first_choice_evenly_across_neighbor_replicas() {
        // Four members form a complete graph, so every owner is a neighbor.
        let membership = membership();
        let placement = Placement::new(64);
        let mut counts = [0usize; 3];
        let mut total = 0;
        for requester in membership.members() {
            let policy = spread_policy(requester.node.clone(), ReplicaSelection::Spread);
            for page in 0..3000 {
                let candidates = placement.rank(membership.clone(), &object(), PageNumber(page));
                if candidates.ordered.contains(&requester.node) {
                    continue;
                }
                assert!(
                    candidates
                        .ordered
                        .iter()
                        .all(|owner| membership.is_neighbor(&requester.node, owner))
                );
                let first = policy
                    .visit_order(&candidates, &object(), PageNumber(page))
                    .get(0)
                    .unwrap();
                counts[first] += 1;
                total += 1;
            }
        }
        assert!(total > 2000, "{total}");
        for count in counts {
            let share = count as f64 / total as f64;
            assert!((0.28..0.39).contains(&share), "{counts:?}");
        }
    }

    #[test]
    fn spread_noncandidate_funds_out_of_order_owner_by_canonical_rank() {
        futures::executor::block_on(async {
            let membership = membership();
            let placement = Placement::new(8);
            let local = membership
                .members()
                .iter()
                .find(|member| {
                    !placement
                        .rank(membership.clone(), &object(), PageNumber(0))
                        .ordered
                        .contains(&member.node)
                })
                .unwrap()
                .node
                .clone();
            // Find a page whose spread order visits a non-primary owner first.
            let probe = spread_policy(local.clone(), ReplicaSelection::Spread);
            let (page, candidates, order) = (0..256)
                .map(PageNumber)
                .find_map(|page| {
                    let candidates = placement.rank(membership.clone(), &object(), page);
                    if candidates.ordered.contains(&local) {
                        return None;
                    }
                    let order = probe.visit_order(&candidates, &object(), page);
                    (order.get(0) == Some(2)).then_some((page, candidates, order))
                })
                .expect("some page visits rank 2 first");
            for selection in [ReplicaSelection::Primary, ReplicaSelection::Spread] {
                let peers = Rc::new(FundedPeer::default());
                let policy = CandidatePolicy::new(
                    local.clone(),
                    Rc::new(Placement::new(8)),
                    Requester::scripted(
                        peers.clone(),
                        FundedPeer::direct_hedge_available,
                        FundedPeer::request,
                        FundedPeer::request_direct,
                    ),
                    probe.credentials.clone(),
                    probe.published.clone(),
                )
                .with_replica_selection(selection);
                let context = OriginContext {
                    object: object(),
                    metadata: None,
                    authorization: None,
                };
                let scope = scope();
                let mut budget = AcquisitionBudget::new(scope.deadline.0, 32, 96);
                let operation = PeerOperation::Page {
                    page: crate::model::PageId {
                        version: crate::model::ObjectVersion {
                            object: object(),
                            etag: crate::model::StrongEtag::test_value("v1"),
                        },
                        number: page,
                    },
                    mode: FetchMode::Acquire,
                };
                assert!(matches!(
                    policy
                        .resolve_with_budget(
                            candidates.clone(),
                            &context,
                            operation,
                            &scope,
                            &mut budget
                        )
                        .await,
                    Err(Error::Unavailable)
                ));
                let calls = peers.calls.borrow();
                let visited: Vec<_> = calls.iter().map(|(to, ..)| to.clone()).collect();
                let expected: Vec<usize> = match selection {
                    ReplicaSelection::Primary => vec![0, 1, 2],
                    ReplicaSelection::Spread => order.ranks().collect(),
                };
                assert_eq!(
                    visited,
                    expected
                        .iter()
                        .map(|rank| candidates.ordered[*rank].clone())
                        .collect::<Vec<_>>()
                );
                assert!(calls.iter().all(|(_, copy, ..)| !copy));
                if selection == ReplicaSelection::Spread {
                    // Rank 2 visited first: it must be able to ask rank 0 to
                    // acquire, probe rank 1, and then reach origin itself, over
                    // failure routes (`owner_funding` less the exchange).
                    let (_, _, attempts, links) = &calls[0];
                    assert_eq!(*attempts, racer_core::read::walk::owner_funding(2) - 1);
                    assert_eq!(*links, 8);
                } else {
                    // Primary first: shared route, shared credits.
                    assert_eq!(calls[0].3, 4);
                }
            }
        });
    }

    /// Signs every reply as its destination, answering `Overloaded` (owners
    /// shedding a hot page) or `Miss` (the helper); records each request.
    struct ShedPeer {
        nodes: Vec<NodeId>,
        signing: Vec<crate::peer::forwarding::Forwarding>,
        local: usize,
        owners: Vec<NodeId>,
        /// Owners fail at the transport (no signed reply) instead.
        transport: bool,
        #[allow(clippy::type_complexity)]
        calls: RefCell<Vec<(NodeId, bool, u32, u8)>>,
    }
    impl ShedPeer {
        fn direct_hedge_available(
            &self,
            _: &std::sync::Arc<crate::topology::Membership>,
            _: &NodeId,
        ) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("shed fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            Box::pin(async move {
                let destination = request.route.destination.clone();
                let copy = matches!(
                    request.operation,
                    PeerOperation::Page {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                );
                self.calls.borrow_mut().push((
                    destination.clone(),
                    copy,
                    request.route.remaining_attempts,
                    request.route.remaining_links,
                ));
                let owner = self.owners.contains(&destination);
                if owner && self.transport {
                    return Err(Error::Overloaded);
                }
                let remote = self.nodes.iter().position(|n| n == &destination).unwrap();
                let (signed, binding) = self.signing[self.local].sign_request(request)?;
                let admitted = self.signing[remote].verify_request(signed)?;
                let response = if owner {
                    PeerResponse::Overloaded
                } else {
                    PeerResponse::Miss
                };
                let response = self.signing[remote].sign_response(admitted.binding(), response)?;
                self.signing[self.local].verify_response(response, &binding)
            })
        }
    }

    /// A non-owner whose owner itself sheds it (a signed `Overloaded` from the destination) asks
    /// the page's overflow helper once, with an Acquire funded from what is left (too little for
    /// the helper to ask another helper), then the remaining owners.
    #[test]
    fn shed_reader_asks_the_page_helper_once_then_the_other_owners() {
        use crate::test_support::security::{network, node};
        futures::executor::block_on(async {
            let count = 16;
            let nodes: Vec<NodeId> = (0..count).map(node).collect();
            let membership = std::sync::Arc::new(
                crate::topology::Membership::validate(
                    racer_control_wire::MembershipVersion(1),
                    nodes
                        .iter()
                        .enumerate()
                        .map(|(i, n)| crate::topology::Member {
                            node: n.clone(),
                            shares: std::num::NonZeroU32::new(4).unwrap(),
                            peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                            rails: vec![],
                            site: String::new(),
                        })
                        .collect(),
                )
                .unwrap(),
            );
            let placement = Placement::new(8);
            let local = 0;
            let probe = spread_policy(nodes[local].clone(), ReplicaSelection::Primary);
            let (page, candidates, helper) = (0..256)
                .map(PageNumber)
                .find_map(|page| {
                    let candidates = placement.rank(membership.clone(), &object(), page);
                    if candidates.ordered.contains(&nodes[local]) {
                        return None;
                    }
                    let helper = probe.fanout_helper(&candidates, &object(), page)?;
                    Some((page, candidates, helper))
                })
                .expect("some page has a helper");
            assert!(!candidates.ordered.contains(&helper) && helper != nodes[local]);
            for transport in [false, true] {
                let peers = Rc::new(ShedPeer {
                    nodes: nodes.clone(),
                    signing: network(count)
                        .into_iter()
                        .map(crate::peer::forwarding::Forwarding::new)
                        .collect(),
                    local,
                    owners: candidates.ordered.clone(),
                    transport,
                    calls: RefCell::default(),
                });
                let policy = CandidatePolicy::new(
                    nodes[local].clone(),
                    Rc::new(Placement::new(8)),
                    Requester::scripted(
                        peers.clone(),
                        ShedPeer::direct_hedge_available,
                        ShedPeer::request,
                        ShedPeer::request_direct,
                    ),
                    probe.credentials.clone(),
                    probe.published.clone(),
                );
                let context = OriginContext {
                    object: object(),
                    metadata: None,
                    authorization: None,
                };
                let scope = scope();
                let mut budget = AcquisitionBudget::new(
                    scope.deadline.0,
                    racer_core::read::PAGE_ATTEMPTS,
                    racer_core::read::PAGE_LINKS,
                );
                let operation = PeerOperation::Page {
                    page: crate::model::PageId {
                        version: crate::model::ObjectVersion {
                            object: object(),
                            etag: crate::model::StrongEtag::test_value("v1"),
                        },
                        number: page,
                    },
                    mode: FetchMode::Acquire,
                };
                let result = policy
                    .resolve_with_budget(
                        candidates.clone(),
                        &context,
                        operation,
                        &scope,
                        &mut budget,
                    )
                    .await;
                assert!(
                    matches!(result, Err(Error::Unavailable)),
                    "{:?}",
                    result.err()
                );
                let calls = peers.calls.borrow();
                let visited: Vec<_> = calls.iter().map(|(to, ..)| to.clone()).collect();
                if transport {
                    assert!(!visited.contains(&helper), "{visited:?}");
                    continue;
                }
                assert_eq!(
                    visited[..2],
                    [candidates.ordered[0].clone(), helper.clone()]
                );
                assert_eq!(visited.iter().filter(|n| **n == helper).count(), 1);
                assert!(visited[2..].iter().all(|n| candidates.ordered.contains(n)));
                // The helper probe is an Acquire carrying fewer credits than a
                // walk needs to ask a helper itself.
                let (_, copy, attempts, links) = &calls[1];
                assert!(!copy);
                assert!(*attempts >= 1);
                assert!(*attempts < racer_core::read::budget::HELPER_MIN_ATTEMPTS);
                // It carries its whole funded envelope, not a normal four-link route, so the helper
                // can still reach rank 0 through a relay.
                assert!(
                    *links > racer_core::read::budget::NORMAL_ROUTE_LINKS,
                    "{links}"
                );
            }
        });
    }

    /// Records each request's destination, mode, and route funding, then fails.
    #[derive(Default)]
    struct FundedPeer {
        #[allow(clippy::type_complexity)]
        calls: RefCell<Vec<(NodeId, bool, u32, u8)>>,
    }
    impl FundedPeer {
        fn direct_hedge_available(
            &self,
            _: &std::sync::Arc<crate::topology::Membership>,
            _: &NodeId,
        ) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("funding fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            Box::pin(async move {
                let copy = matches!(
                    request.operation,
                    PeerOperation::Page {
                        mode: FetchMode::CopyOnly,
                        ..
                    }
                );
                self.calls.borrow_mut().push((
                    request.route.destination,
                    copy,
                    request.route.remaining_attempts,
                    request.route.remaining_links,
                ));
                Err(Error::Unavailable)
            })
        }
    }

    #[test]
    fn only_candidates_mint_authority_after_ordered_copy_probes() {
        ordered_predecessor_probes(4);
    }
    fn ordered_predecessor_probes(attempts: u32) {
        let (membership, placement, context, scope, credentials) = fixture();
        let ordered = placement
            .rank(membership.clone(), &context.object, PageNumber(0))
            .ordered;
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = CandidatePolicy::new(
            ordered[2].clone(),
            placement,
            Requester::scripted(
                peer.clone(),
                ProbePeer::direct_hedge_available,
                ProbePeer::request,
                ProbePeer::request_direct,
            ),
            credentials,
            Arc::new(controlplane::Published::new(
                crate::control::Snapshot::retention(2),
            )),
        );
        let candidates = policy.candidates(membership, &context.object, PageNumber(0));
        let mut budget = AcquisitionBudget::new(scope.deadline.0, attempts, 8);
        let result = futures::executor::block_on(policy.resolve_with_budget(
            candidates,
            &context,
            PeerOperation::Metadata {
                object: context.object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            },
            &scope,
            &mut budget,
        ))
        .unwrap();
        let CandidateResolution::Origin(authority) = result else {
            panic!("must grant third candidate after probes")
        };
        authority.validate(&context.object, PageNumber(0)).unwrap();
        assert_eq!(
            *peer.calls.borrow(),
            vec![(ordered[0].clone(), true), (ordered[1].clone(), true)]
        );
        assert_eq!(budget.remaining_attempts(), attempts - 2);
        assert_eq!(budget.remaining_links(), 0);
        assert_eq!(
            authority.validate(&context.object, PageNumber(1)),
            Err(Error::Unauthorized)
        );
    }
    #[test]
    fn noncandidate_never_gets_origin_and_auth_failure_is_not_miss_evidence() {
        let (membership, placement, context, scope, credentials) = fixture();
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unauthorized,
        });
        let ordered = placement
            .rank(membership.clone(), &context.object, PageNumber(0))
            .ordered;
        let policy = CandidatePolicy::new(
            ordered[1].clone(),
            placement.clone(),
            Requester::scripted(
                peer.clone(),
                ProbePeer::direct_hedge_available,
                ProbePeer::request,
                ProbePeer::request_direct,
            ),
            credentials.clone(),
            Arc::new(controlplane::Published::new(
                crate::control::Snapshot::retention(2),
            )),
        );
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 4, 8);
        let result = futures::executor::block_on(policy.resolve_with_budget(
            policy.candidates(membership.clone(), &context.object, PageNumber(0)),
            &context,
            PeerOperation::Metadata {
                object: context.object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            },
            &scope,
            &mut budget,
        ));
        assert!(matches!(result, Err(Error::Unauthorized)));
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = CandidatePolicy::new(
            NodeId("outside".into()),
            placement,
            Requester::scripted(
                peer.clone(),
                ProbePeer::direct_hedge_available,
                ProbePeer::request,
                ProbePeer::request_direct,
            ),
            credentials,
            Arc::new(controlplane::Published::new(
                crate::control::Snapshot::retention(2),
            )),
        );
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let result = futures::executor::block_on(policy.resolve_with_budget(
            policy.candidates(membership, &context.object, PageNumber(0)),
            &context,
            PeerOperation::Metadata {
                object: context.object.clone(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            },
            &scope,
            &mut budget,
        ));
        assert!(matches!(result, Err(Error::Unavailable)));
        assert_eq!(peer.calls.borrow().len(), 3);
        assert!(peer.calls.borrow().iter().all(|(_, copy)| !copy));
    }
    #[test]
    fn signed_failure_classification_preserves_auth_and_version_distinctions() {
        let operation = PeerOperation::Metadata {
            object: object(),
            selector: MetadataSelector::Fresh,
            mode: FetchMode::CopyOnly,
        };
        assert_eq!(
            classify(&PeerResponse::OriginRejected, &operation, false),
            Err(Error::OriginRejected)
        );
        assert_eq!(
            classify(&PeerResponse::VersionUnavailable, &operation, false),
            Ok(Some(ProbeOutcome::VersionUnavailable))
        );
        assert_eq!(
            classify(&PeerResponse::Overloaded, &operation, false),
            Ok(Some(ProbeOutcome::Overloaded))
        );
    }
    #[test]
    fn authoritative_absence_is_terminal_only_for_actual_fresh_acquire() {
        let mut operation = PeerOperation::Metadata {
            object: object(),
            selector: MetadataSelector::Fresh,
            mode: FetchMode::Acquire,
        };
        assert_eq!(
            classify(&PeerResponse::NotFound, &operation, true),
            Err(Error::NotFound)
        );
        assert_eq!(
            classify(&PeerResponse::NotFound, &operation, false),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            classify(&PeerResponse::Miss, &operation, true),
            Ok(Some(ProbeOutcome::CopyMiss))
        );
        if let PeerOperation::Metadata { selector, .. } = &mut operation {
            *selector = MetadataSelector::Pinned(StrongEtag::test_value("old"));
        }
        assert_eq!(
            classify(&PeerResponse::NotFound, &operation, true),
            Err(Error::Unauthorized)
        );
    }
    #[test]
    fn metadata_copy_must_match_object_and_explicit_pin() {
        let operation = PeerOperation::Metadata {
            object: object(),
            selector: MetadataSelector::Pinned(StrongEtag::test_value("v1")),
            mode: FetchMode::CopyOnly,
        };
        let mut metadata = ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: object(),
                etag: StrongEtag::test_value("v1"),
            },
            length: 0,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        };
        assert_eq!(
            classify(&PeerResponse::Metadata(metadata.clone()), &operation, false),
            Ok(None)
        );
        metadata.version.etag = StrongEtag::test_value("v2");
        assert_eq!(
            classify(&PeerResponse::Metadata(metadata.clone()), &operation, false),
            Err(Error::CorruptRecord)
        );
        metadata.version.etag = StrongEtag::test_value("v1");
        metadata.version.object.key = CacheKey([1; 32]);
        assert_eq!(
            classify(&PeerResponse::Metadata(metadata), &operation, false),
            Err(Error::CorruptRecord)
        );
    }

    struct RecordedPeer {
        calls: RefCell<Vec<(NodeId, bool)>>,
        error: Error,
    }
    impl RecordedPeer {
        fn direct_hedge_available(
            &self,
            _: &std::sync::Arc<crate::topology::Membership>,
            _: &NodeId,
        ) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("candidate-recording fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: std::sync::Arc<crate::topology::Membership>,
            _scope: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            Box::pin(async move {
                let copy = match request.operation {
                    PeerOperation::Subscribe { mode, .. }
                    | PeerOperation::Bootstrap { mode, .. }
                    | PeerOperation::Page { mode, .. }
                    | PeerOperation::Metadata { mode, .. } => {
                        matches!(mode, FetchMode::CopyOnly)
                    }
                    PeerOperation::Warm { .. } => false,
                };
                self.calls
                    .borrow_mut()
                    .push((request.route.destination, copy));
                Err(self.error)
            })
        }
    }
    fn membership() -> std::sync::Arc<crate::topology::Membership> {
        use crate::topology::Member;
        use crate::topology::Membership;
        use racer_control_wire::MembershipVersion;
        std::sync::Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..4)
                    .map(|i| Member {
                        node: NodeId(format!("22222222-2222-4222-8222-{i:012}")),
                        shares: std::num::NonZeroU32::new(4).unwrap(),
                        peer_endpoint: format!("127.0.0.1:{}", 8000 + i),
                        rails: vec![],
                        site: String::new(),
                    })
                    .collect(),
            )
            .unwrap(),
        )
    }
    fn policy(node: NodeId, peers: Rc<RecordedPeer>) -> CandidatePolicy {
        use racer_crypto::identity::KeyEpochs;
        use racer_crypto::identity::Keyring;
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(flow_control::Quotas::new(
            crate::admission::AdmissionPolicy::new(config.limits),
        ));
        let keys = Rc::new(Keyring::new(
            config.cluster,
            node.clone(),
            std::sync::Arc::new(KeyEpochs::default()),
        ));
        CandidatePolicy::new(
            node,
            Rc::new(Placement::new(8)),
            Requester::scripted(
                peers,
                RecordedPeer::direct_hedge_available,
                RecordedPeer::request,
                RecordedPeer::request_direct,
            ),
            Rc::new(CredentialCrypto::new(keys, admission)),
            Arc::new(controlplane::Published::new(
                crate::control::Snapshot::retention(2),
            )),
        )
    }
    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::RequestId([7; 16]),
            Instant::now() + std::time::Duration::from_secs(60),
        )
        .unwrap()
    }
    #[test]
    fn ownership_refresh_recovers_idle_pages_and_rejects_obsolete_results() {
        use crate::model::{PageId, VersionMetadata, WorkerId};
        use crate::store::catalog::{Index, IndexSnapshot, IndexedPage, RecordLocation};
        use crate::topology::tests::fixtures;
        let members = fixtures::membership(1000);
        let page = PageId {
            version: ObjectVersion {
                object: object(),
                etag: StrongEtag::test_value("recovered"),
            },
            number: PageNumber(0),
        };
        let owner = Placement::new(0)
            .rank(members.clone(), &page.version.object, page.number)
            .ordered[0]
            .clone();
        let peers = Rc::new(RecordedPeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let mut policy = policy(owner.clone(), peers.clone());
        let index = Index::new(WorkerId(0), 8, crate::test_support::availability());
        index.set_page_capacity(1).unwrap();
        index
            .restore(IndexSnapshot {
                entries: vec![(
                    page.clone(),
                    IndexedPage {
                        location: RecordLocation {
                            segment: page_alloc::SegmentId(0),
                            generation: page_alloc::Generation(1),
                            extent: page_alloc::Extent::new(0, 512).unwrap(),
                        },
                        metadata: VersionMetadata {
                            content_type: None,
                            version: page.version.clone(),
                            length: 1,
                        },
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    },
                )],
                metadata: vec![],
            })
            .unwrap();
        assert_eq!(index.snapshot_pages(0, 1).1.len(), 1);
        assert!(!policy.owns_current(&page));
        assert_eq!(policy.refresh_ownership(&index), Err(Error::Unavailable));
        policy.published = crate::test_support::published_membership(members.clone());
        for _ in 0..3 {
            assert_eq!(
                policy.refresh_ownership(&index),
                Ok(crate::topology::Maintenance::Progress)
            );
            assert!(!policy.owns_current(&page));
        }
        policy.refresh_ownership(&index).unwrap();
        assert!(policy.owns_current(&page));
        assert_eq!(
            policy.refresh_ownership(&index),
            Ok(crate::topology::Maintenance::Idle)
        );
        let next = Arc::new(
            crate::topology::Membership::validate(
                racer_control_wire::MembershipVersion(2),
                members
                    .members()
                    .iter()
                    .filter(|m| m.node != owner)
                    .cloned()
                    .collect(),
            )
            .unwrap(),
        );
        policy.published = crate::test_support::published_membership(next);
        assert!(!policy.owns_current(&page));
        for _ in 0..4 {
            policy.refresh_ownership(&index).unwrap();
        }
        assert!(!policy.owns_current(&page));
        // Completing a leased obsolete foreground rank never restores ownership.
        futures::executor::block_on(policy.candidates_scoped(
            members,
            &page.version.object,
            page.number,
            &scope(),
        ))
        .unwrap();
        assert!(!policy.owns_current(&page));
        assert!(peers.calls.borrow().is_empty());
    }

    #[test]
    fn ownership_scoped_refresh_warms_current_only_and_honors_cancellation() {
        let members = membership();
        let page = crate::model::PageId {
            version: ObjectVersion {
                object: object(),
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        };
        let owner = Placement::new(0)
            .rank(members.clone(), &object(), page.number)
            .ordered[0]
            .clone();
        let peers = Rc::new(RecordedPeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let mut policy = policy(owner, peers);
        policy.published = crate::test_support::published_membership(members);
        let cancelled = scope();
        cancelled.cancel().unwrap();
        assert_eq!(
            futures::executor::block_on(policy.refresh_ownership_scoped(&page, &cancelled)),
            Err(Error::Cancelled)
        );
        assert!(!policy.owns_current(&page));
        futures::executor::block_on(policy.refresh_ownership_scoped(&page, &scope())).unwrap();
        assert!(policy.owns_current(&page));
        let mut version = page.clone();
        version.version.etag = StrongEtag::test_value("v2");
        assert!(policy.owns_current(&version));
    }

    #[test]
    fn ownership_weak_callback_releases_network_reclamation_graph_and_is_neutral_after_drop() {
        use crate::memory::{BufferPool, MemoryCache};
        use crate::peer::forwarding::Forwarding;
        use crate::peer::protocol::{SecurityCodec, Signatures};
        use crate::peer::transport::Transfers;
        use crate::store::catalog::Index;
        use racer_crypto::identity::{Certificates, KeyEpochs, Keyring};

        for _ in 0..3 {
            let config = crate::test_support::cluster::config(false);
            let admission = Rc::new(flow_control::Quotas::new(
                crate::admission::AdmissionPolicy::new(config.limits.clone()),
            ));
            let reactor = Rc::new(crate::runtime::Reactor::new(admission.clone()));
            let keys = Rc::new(Keyring::new(
                config.cluster.clone(),
                config.node.clone(),
                Arc::new(KeyEpochs::default()),
            ));
            let published = crate::test_support::published_membership(Arc::new(
                crate::topology::Membership::validate(
                    racer_control_wire::MembershipVersion(1),
                    vec![crate::topology::Member {
                        node: config.node.clone(),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: "127.0.0.1:7443".into(),
                        rails: vec![],
                        site: String::new(),
                    }],
                )
                .unwrap(),
            ));
            let availability = Rc::new(crate::control::Availability::new(
                published.clone(),
                keys.clone(),
            ));
            let buffers = BufferPool::new(admission.clone());
            let memory = Rc::new(MemoryCache::new(buffers.clone(), availability.clone()));
            let index = Rc::new(Index::new(
                crate::model::WorkerId(0),
                1,
                availability.clone(),
            ));
            let writer = Rc::new(crate::store::StoreWriter::new(
                index,
                Rc::new(page_alloc::Segments::new(config.segment_bytes)),
                Rc::new(page_alloc::Slab::new(
                    std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                        .join("target/ownership-unopened-slab"),
                    config.slab_bytes,
                    config.segment_bytes,
                    crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
                )),
                admission.clone(),
                reactor.clone(),
                availability,
            ));
            let retention = writer.retention();
            let weak_writer = Rc::downgrade(&writer);
            let weak_memory = Rc::downgrade(&memory);
            let weak_retention = Rc::downgrade(&retention);
            let http = Rc::new(crate::http::new_pool(reactor.clone(), admission.clone(), 1));
            let io = Rc::new(crate::http::new_io(
                reactor,
                crate::http::Codec::new(crate::peer::protocol::MAX_ENVELOPE_HEAD),
                admission.clone(),
                crate::model::PAGE_BYTES + 16,
            ));
            let signatures = Rc::new(Signatures::new(
                keys.clone(),
                Rc::new(Certificates::new(config.cluster, keys.clone())),
            ));
            // Real network requester/transport plus the same strong reclamation
            // captures as production. No I/O or slab file is opened in this test.
            let transfers = Rc::new(
                Transfers::new(
                    http,
                    io,
                    admission.clone(),
                    Rc::new(SecurityCodec::new(admission.clone(), buffers)),
                    signatures.clone(),
                )
                .with_reclamation(move |cache, bytes| {
                    memory.reclaim_idle(
                        crate::admission::ResourceClass::Ciphertext,
                        Some(cache),
                        bytes,
                        |page, ciphertext| writer.discard_idle_copy(page, ciphertext),
                    );
                    writer.reclaim_ciphertext(Some(cache), bytes);
                }),
            );
            let requester = Rc::new(Requester::new(
                Rc::new(crate::topology::Paths::new(
                    Rc::new(crate::topology::LinkHealth::default()),
                    1,
                )),
                Rc::new(Forwarding::new(signatures)),
                transfers,
                Rc::new(
                    crate::peer::PeerNetwork::new(config.node.clone(), published.clone()).unwrap(),
                ),
            ));
            let policy = Rc::new(CandidatePolicy::new(
                config.node,
                Rc::new(Placement::new(1)),
                requester,
                Rc::new(CredentialCrypto::new(keys, admission)),
                published,
            ));
            let weak_policy = Rc::downgrade(&policy);
            let callback = policy.ownership_callback();
            retention.set_ownership(callback.clone());
            let prepare = policy.ownership_pass();
            retention.set_ownership_pass(prepare.clone());
            let page = crate::model::PageId {
                version: ObjectVersion {
                    object: object(),
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            };
            assert!(!callback(&page));
            futures::executor::block_on(policy.refresh_ownership_scoped(&page, &scope())).unwrap();
            assert!(callback(&page));
            let snapshot = policy.published.current().unwrap().unwrap();
            let snapshot_refs = Arc::strong_count(&snapshot);
            let membership_refs = Arc::strong_count(&snapshot.membership);
            let pinned = prepare();
            assert_eq!(Arc::strong_count(&snapshot), snapshot_refs + 1);
            for _ in 0..256 {
                assert!(pinned(&page));
            }
            assert_eq!(Arc::strong_count(&snapshot), snapshot_refs + 1);
            assert_eq!(Arc::strong_count(&snapshot.membership), membership_refs);
            drop(pinned);
            assert_eq!(Arc::strong_count(&snapshot), snapshot_refs);
            assert_eq!(Rc::strong_count(&policy), 1);
            drop(retention);
            assert!(weak_retention.upgrade().is_some());
            drop(policy);
            assert!(weak_policy.upgrade().is_none());
            assert!(weak_writer.upgrade().is_none());
            assert!(weak_memory.upgrade().is_none());
            assert!(weak_retention.upgrade().is_none());
            assert!(!callback(&page));
            assert!(!prepare()(&page));
        }
    }
    #[test]
    fn backup_probes_only_predecessors_in_order_before_scoped_origin_authority() {
        ordered_predecessor_probes(3);
    }
    #[test]
    fn noncandidate_acquires_from_candidates_and_never_mints_origin_authority() {
        futures::executor::block_on(async {
            let membership = membership();
            let candidates = Placement::new(8).rank(membership.clone(), &object(), PageNumber(0));
            let local = membership
                .members()
                .iter()
                .find(|member| !candidates.ordered.contains(&member.node))
                .unwrap()
                .node
                .clone();
            let peers = Rc::new(RecordedPeer {
                calls: RefCell::new(vec![]),
                error: Error::Unavailable,
            });
            let policy = policy(local, peers.clone());
            let context = OriginContext {
                object: object(),
                metadata: None,
                authorization: None,
            };
            let scope = scope();
            let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
            let operation = PeerOperation::Metadata {
                object: object(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            };
            assert!(matches!(
                policy
                    .resolve_with_budget(candidates, &context, operation, &scope, &mut budget)
                    .await,
                Err(Error::Unavailable)
            ));
            assert_eq!(peers.calls.borrow().len(), 3);
            assert!(peers.calls.borrow().iter().all(|(_, copy)| !copy));
        });
    }
    #[test]
    fn peer_auth_failure_is_not_predecessor_miss_evidence() {
        futures::executor::block_on(async {
            let candidates = Placement::new(8).rank(membership(), &object(), PageNumber(0));
            let peers = Rc::new(RecordedPeer {
                calls: RefCell::new(vec![]),
                error: Error::Unauthorized,
            });
            let policy = policy(candidates.ordered[1].clone(), peers.clone());
            let context = OriginContext {
                object: object(),
                metadata: None,
                authorization: None,
            };
            let scope = scope();
            let mut budget = AcquisitionBudget::new(scope.deadline.0, 3, 8);
            let operation = PeerOperation::Metadata {
                object: object(),
                selector: MetadataSelector::Fresh,
                mode: FetchMode::Acquire,
            };
            assert!(matches!(
                policy
                    .resolve_with_budget(candidates, &context, operation, &scope, &mut budget)
                    .await,
                Err(Error::Unauthorized)
            ));
            assert_eq!(peers.calls.borrow().len(), 1);
        });
    }
    #[test]
    fn origin_version_miss_cannot_claim_absence_when_predecessor_was_unreachable() {
        let membership = membership();
        let ranked = Placement::new(8).rank(membership.clone(), &object(), PageNumber(0));
        let peers = Rc::new(RecordedPeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = policy(ranked.ordered[1].clone(), peers);
        let mut authority = OriginAuthority {
            diagnostic: CandidateTrail::default(),
            membership,
            node: ranked.ordered[1].clone(),
            object: object(),
            owners: ranked.ordered.clone(),
            page: PageNumber(0),
            predecessor_evidence: vec![ProbeOutcome::Unreachable],
            rank: 1,
        };
        assert_eq!(policy.origin_miss_error(&authority), Error::Unavailable);
        authority.predecessor_evidence = vec![ProbeOutcome::CopyMiss];
        assert_eq!(
            policy.origin_miss_error(&authority),
            Error::VersionUnavailable
        );
    }

    mod hedging {
        use super::*;
        #[test]
        fn worker_handles_share_capacity_but_not_counter_shards() {
            let metrics = Metrics::for_workers(2).unwrap();
            let first = Hedges::new(
                HedgeConfig {
                    slots: 1,
                    bytes: DUPLICATE_BYTES,
                    delay: Duration::from_millis(1),
                },
                metrics[0].clone(),
            )
            .unwrap();
            let second = first.for_worker(metrics[1].clone());
            let permit = second.acquire().unwrap();
            assert!(matches!(first.acquire(), Err(Error::Overloaded)));
            first.suppressed();
            permit.started();
            permit.won();
            for event in [
                Event::PageHedgeStarted,
                Event::PageHedgeWon,
                Event::PageHedgeDuplicateBytes,
            ] {
                assert_eq!(metrics[0].writer_count(event), 0);
                assert!(metrics[1].writer_count(event) > 0);
            }
            assert_eq!(metrics[0].writer_count(Event::PageHedgeSuppressed), 1);
            assert_eq!(metrics[1].writer_count(Event::PageHedgeSuppressed), 0);
            drop(permit);
            assert!(first.acquire().is_ok());
        }
        use crate::model::RequestId;
        use crate::read::tests::page;
        use crate::runtime::RequestScope;
        use std::cell::Cell;
        use std::future::Future;
        use std::rc::Rc;
        use uring_runtime::environment::SimulationClock;
        use uring_runtime::environment::now;
        fn scope() -> RequestScope {
            RequestScope::new(RequestId([1; 16]), now() + Duration::from_secs(10)).unwrap()
        }
        fn controller(slots: usize, bytes: usize) -> Arc<Hedges> {
            Hedges::new(
                HedgeConfig {
                    slots,
                    bytes,
                    delay: Duration::from_millis(10),
                },
                Metrics::default(),
            )
            .unwrap()
        }
        #[test]
        fn malformed_speculative_http_and_wire_heads_do_not_veto_valid_primary() {
            for kind in ["invalid", "large", "wire"] {
                for same_poll in [false, true] {
                    let clock = SimulationClock::new(933);
                    let _env = clock.environment(0).enter();
                    let owner = controller(1, DUPLICATE_BYTES);
                    let permit = owner.acquire().unwrap();
                    let a = scope();
                    let b = scope();
                    let parent = scope();
                    let ready = Cell::new(false);
                    let malformed_ready = Cell::new(false);
                    let primary = std::future::poll_fn(|_| {
                        if ready.get() {
                            Poll::Ready(Ok(page(42)))
                        } else {
                            Poll::Pending
                        }
                    });
                    let secondary = async {
                        std::future::poll_fn(|_| {
                            if malformed_ready.get() {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        })
                        .await;
                        let codec = crate::http::Codec::new(if kind == "large" { 8 } else { 4096 });
                        let bytes: &[u8] = if kind == "invalid" {
                            b"not-http\r\n\r\n"
                        } else {
                            b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
                        };
                        let parsed = codec.decode_head(bytes);
                        let error = match parsed {
                            Err(e) => Error::from(e),
                            Ok(Some((head, _))) => {
                                crate::peer::protocol::decode_envelope(head, true)
                                    .err()
                                    .expect("missing signed wire envelope")
                            }
                            _ => panic!("complete malformed frame"),
                        };
                        assert!(matches!(
                            error,
                            Error::InvalidRequest | Error::HeaderTooLarge
                        ));
                        Err(error)
                    };
                    let mut work = Box::pin(race(primary, secondary, &a, &b, &parent, &permit));
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    assert!(work.as_mut().poll(&mut cx).is_pending());
                    clock.advance(Duration::from_millis(10));
                    // Start both contenders and park them before the readiness turn.
                    assert!(work.as_mut().poll(&mut cx).is_pending());
                    malformed_ready.set(true);
                    if same_poll {
                        ready.set(true);
                    }
                    let result = work.as_mut().poll(&mut cx);
                    if same_poll {
                        assert!(
                            matches!(result, Poll::Ready(Ok(page)) if page.plaintext.bytes() == [42])
                        );
                    } else {
                        assert!(result.is_pending());
                        assert!(!a.cancellation.is_cancelled());
                        ready.set(true);
                        assert!(
                            matches!(work.as_mut().poll(&mut cx), Poll::Ready(Ok(page)) if page.plaintext.bytes() == [42])
                        );
                    }
                }
            }
        }
        #[test]
        fn fast_primary_never_launches_secondary_or_records_duplicate_bytes() {
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let result = futures::executor::block_on(race(
                async { Ok(page(1)) },
                async {
                    panic!("unexpected speculative launch");
                    #[allow(unreachable_code)]
                    Ok(page(2))
                },
                &a,
                &b,
                &parent,
                &permit,
            ));
            assert_eq!(result.unwrap().plaintext.bytes(), &[1]);
            assert_eq!(owner.metrics.count(Event::PageHedgeStarted), 0);
            assert_eq!(owner.metrics.count(Event::PageHedgeDuplicateBytes), 0);
        }
        #[test]
        fn canceled_parent_never_polls_unlaunched_attempts() {
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            parent.cancel().unwrap();
            let never = || async {
                panic!("canceled parent submitted work");
                #[allow(unreachable_code)]
                Ok::<_, Error>(page(0))
            };
            assert_eq!(
                futures::executor::block_on(race(never(), never(), &a, &b, &parent, &permit)).err(),
                Some(Error::Cancelled)
            );
        }
        #[test]
        fn worker_owned_race_keeps_permit_after_caller_detaches_until_loser_fence() {
            let clock = SimulationClock::new(906);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
            let _guard = queue.enter();
            let (send, receive) = futures::channel::oneshot::channel();
            let fence = Rc::new(Cell::new(false));
            let primary_fence = fence.clone();
            let completion = Rc::new(Cell::new(false));
            let done = completion.clone();
            uring_runtime::drivers::reserve()
                .unwrap()
                .submit_detached(Box::pin(async move {
                    let a = scope();
                    let b = scope();
                    let parent = scope();
                    let primary = std::future::poll_fn(|cx| {
                        if primary_fence.get() {
                            Poll::Ready(Err(Error::Cancelled))
                        } else {
                            cx.waker().wake_by_ref();
                            Poll::Pending
                        }
                    });
                    let result =
                        race(primary, async { Ok(page(3)) }, &a, &b, &parent, &permit).await;
                    drop(permit);
                    done.set(true);
                    let _ = send.send(result);
                    Ok::<_, Error>(())
                }));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            queue.poll(&mut cx, 4);
            clock.advance(Duration::from_millis(10));
            queue.poll(&mut cx, 4);
            drop(receive);
            assert!(!completion.get());
            assert!(matches!(owner.acquire(), Err(Error::Overloaded)));
            fence.set(true);
            queue.poll(&mut cx, 4);
            assert!(completion.get());
            assert!(owner.acquire().is_ok());
            assert_eq!(queue.pending(), 0);
        }
        #[test]
        fn shared_slots_bytes_and_alarm_wake_are_bounded() {
            let clock = SimulationClock::new(901);
            let _env = clock.environment(0).enter();
            let owner = controller(2, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            assert!(matches!(owner.clone().acquire(), Err(Error::Overloaded)));
            let wake = std::sync::Arc::new(crate::test_support::WakeCounter::default());
            let waker = std::task::Waker::from(wake.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(permit.delay(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            owner.poll();
            assert_eq!(wake.count(), 1);
            owner.poll();
            assert_eq!(wake.count(), 1);
            assert!(permit.delay(&mut cx).is_ready());
            drop(permit);
            assert!(owner.acquire().is_ok());
            assert!(
                !Hedges::new(HedgeConfig::default(), Metrics::default())
                    .unwrap()
                    .enabled()
            );
        }
        #[test]
        fn validated_secondary_waits_for_primary_fence_and_keeps_slot() {
            let clock = SimulationClock::new(902);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let fence = Rc::new(Cell::new(false));
            let primary = std::future::poll_fn(|_| {
                if fence.get() {
                    Poll::Ready(Err(Error::Cancelled))
                } else {
                    Poll::Pending
                }
            });
            let mut race = Box::pin(race(
                primary,
                async { Ok(page(42)) },
                &a,
                &b,
                &parent,
                &permit,
            ));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(race.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            assert!(race.as_mut().poll(&mut cx).is_pending());
            assert!(a.cancellation.is_cancelled());
            assert!(!parent.cancellation.is_cancelled());
            assert!(matches!(owner.acquire(), Err(Error::Overloaded)));
            fence.set(true);
            assert!(
                matches!(race.as_mut().poll(&mut cx), Poll::Ready(Ok(page)) if page.plaintext.bytes() == [42])
            );
            drop(race);
            drop(permit);
            assert!(owner.acquire().is_ok());
        }
        #[test]
        fn invalid_fast_copy_does_not_displace_valid_slow_primary() {
            let clock = SimulationClock::new(903);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let ready = Cell::new(false);
            let primary = std::future::poll_fn(|_| {
                if ready.get() {
                    Poll::Ready(Ok(page(7)))
                } else {
                    Poll::Pending
                }
            });
            let mut race = Box::pin(race(
                primary,
                async { Err(Error::CorruptRecord) },
                &a,
                &b,
                &parent,
                &permit,
            ));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(race.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            assert!(race.as_mut().poll(&mut cx).is_pending());
            assert!(!a.cancellation.is_cancelled());
            ready.set(true);
            assert!(
                matches!(race.as_mut().poll(&mut cx), Poll::Ready(Ok(page)) if page.plaintext.bytes() == [7])
            );
            assert_eq!(owner.metrics.count(Event::PageHedgeWon), 0);
        }
        #[test]
        fn parent_cancellation_drains_both_and_never_returns_a_winner() {
            let clock = SimulationClock::new(904);
            let _env = clock.environment(0).enter();
            let owner = controller(1, DUPLICATE_BYTES);
            let permit = owner.acquire().unwrap();
            let a = scope();
            let b = scope();
            let parent = scope();
            let fence = Cell::new(false);
            let child = || {
                std::future::poll_fn(|_| {
                    if fence.get() {
                        Poll::Ready(Ok(page(9)))
                    } else {
                        Poll::Pending
                    }
                })
            };
            let mut race = Box::pin(race(child(), child(), &a, &b, &parent, &permit));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(race.as_mut().poll(&mut cx).is_pending());
            clock.advance(Duration::from_millis(10));
            assert!(race.as_mut().poll(&mut cx).is_pending());
            parent.cancel().unwrap();
            assert!(race.as_mut().poll(&mut cx).is_pending());
            assert!(a.cancellation.is_cancelled() && b.cancellation.is_cancelled());
            assert!(matches!(owner.acquire(), Err(Error::Overloaded)));
            fence.set(true);
            assert!(matches!(
                race.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
        }
    }

    #[test]
    fn single_flight_peers_expire_and_stay_bounded() {
        let mut peers = super::SingleFlightPeers::default();
        let now = std::time::Instant::now();
        let a = NodeId("a".into());
        assert!(!peers.contains(&a, now));
        peers.insert(a.clone(), now);
        assert!(peers.contains(&a, now + super::SINGLE_FLIGHT_TTL / 2));
        assert!(!peers.contains(&a, now + super::SINGLE_FLIGHT_TTL));
        assert!(peers.0.is_empty(), "expired entries are forgotten");
        for i in 0..super::MAX_SINGLE_FLIGHT_PEERS + 10 {
            let at = now + std::time::Duration::from_millis(i as u64);
            peers.insert(NodeId(i.to_string()), at);
        }
        assert_eq!(peers.0.len(), super::MAX_SINGLE_FLIGHT_PEERS);
        assert!(!peers.0.contains_key(&NodeId("0".into())), "oldest evicted");
        assert!(peers.contains(&NodeId("1033".into()), now));
    }
}
