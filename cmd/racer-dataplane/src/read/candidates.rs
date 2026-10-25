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
    use crate::model::PAGE_BYTES;
    let plaintext = admission.reserve(
        Some(cache),
        ResourceClass::Plaintext,
        PAGE_BYTES as usize * pages,
    )?;
    let ciphertext = admission.reserve(
        Some(cache),
        ResourceClass::Ciphertext,
        (PAGE_BYTES as usize + 16) * pages,
    )?;
    Ok((plaintext, ciphertext))
}

pub struct OriginAuthority {
    diagnostic: CandidateTrail,
    membership: std::sync::Arc<crate::topology::Membership>,
    node: NodeId,
    object: ObjectId,
    page: PageNumber,
    predecessor_evidence: Vec<ProbeOutcome>,
    rank: usize,
}
pub use racer_core::read::candidates::{ProbeOutcome, ReplicaSelection};
pub(crate) use racer_core::read::candidates::{VisitOrder, replica_score, visit_order};
use racer_core::read::walk::{
    CandidateWalk, Continuation, End as WalkEnd, Mode as WalkMode, Outcome as WalkOutcome,
    RemainingEnd, RemainingWalk, RouteMode, Step, WalkConfig,
};

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
    /// Timer for waiting on a destination that is behind (Known issue 26).
    /// Without one, the walk skips it as before.
    reactor: Option<Rc<crate::runtime::Reactor>>,
    /// Test only: wait without a timer, returning at once.
    #[cfg(test)]
    pub(crate) instant_stale_wait: std::cell::Cell<bool>,
    /// Owners this node answered a page probe without a copy, by page
    /// (Known issue 21; see `racer_core::read::successor`). The coordinator
    /// notes and defers on them; an owner walk asks them before origin.
    pub(crate) successors: RefCell<Successors>,
    /// Pages this node fetched from origin as an owner after rank 0, to offer
    /// to rank 0 later (Known issue 37; see `racer_core::read::notice`).
    pub(crate) notices: RefCell<Notices>,
    /// Copy offers in flight ([`MAX_OFFERS_IN_FLIGHT`]).
    offers: Rc<std::cell::Cell<usize>>,
    /// Silence pings in flight ([`MAX_PINGS_IN_FLIGHT`]).
    pub(crate) pings: Rc<std::cell::Cell<usize>>,
    /// Pages whose rank 0 this node, as an owner above it, found fetching
    /// (a probe timed out) or overdue (it missed that request's deadline);
    /// see `racer_core::read::rank0` (Known issue 39).
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

pub(crate) type Notices = racer_core::read::notice::OrphanNotices<crate::model::PageId, NodeId>;

/// Copy offers a node sends at once; more due offers wait for a later turn.
pub(crate) const MAX_OFFERS_IN_FLIGHT: usize = 4;

/// Silence pings a worker sends at once (Known issues 41 and 43); more due
/// pings wait for a later turn.
pub(crate) const MAX_PINGS_IN_FLIGHT: usize = 16;

/// When each of the page's owners arrived in this node's membership chain
/// (`Membership::since`), for lineage targets (Known issue 25).
fn owner_arrivals(candidates: &Candidates) -> Vec<Option<u32>> {
    candidates
        .ordered
        .iter()
        .map(|node| candidates.membership.since(node))
        .collect()
}

pub(crate) type Rank0Watch = racer_core::read::rank0::Rank0Watch<crate::model::PageId, NodeId>;

pub(crate) type Successors =
    racer_core::read::successor::SuccessorHints<crate::model::PageId, NodeId>;

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
/// What an owner's walk uses from the previous membership
/// (`CandidatePolicy::previous_owners`).
#[derive(Default)]
struct PreviousOwners {
    /// Previous owners to ask copy-only before origin (`WalkConfig::handoff`).
    handoff: Vec<NodeId>,
    /// Predecessors to ask under the previous membership
    /// (`WalkConfig::previous`).
    same_rank: u8,
    /// The previous membership, when `same_rank` is not empty.
    membership: Option<std::sync::Arc<crate::topology::Membership>>,
}

#[derive(Default, Clone, Copy)]
pub(crate) struct HedgeContinuation {
    // This is local acquisition progress, never placement/origin authority.
    pub primary_consumed: bool,
    pub primary_outcome: Option<ProbeOutcome>,
    pub stale: bool,
    pub bounded_routes: bool,
    /// Waits this read already made for destinations that are behind
    /// (`racer_core::read::stale::stale_wait`, Known issues 26 and 36).
    pub waits: u8,
}
impl CandidatePolicy {
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&Arc<Hedges>> {
        self.hedge.as_ref()
    }
    /// Only Fill's plaintext fixed-page path calls this. Direct HTTP destinations
    /// prove independent first hops. Drain both attempts before releasing escrow.
    /// Hedge a page request while sharing its acquisition budget and validation.
    #[allow(clippy::too_many_arguments)] // The hedge borrows the caller's distinct policy inputs.
    pub(crate) async fn hedge_page<'a>(
        &'a self,
        candidates: &Candidates,
        context: &'a OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        admission: &Rc<flow_control::Quotas<crate::admission::AdmissionPolicy>>,
        continuation: &mut HedgeContinuation,
        validate: impl Fn(VerifiedResponse, RequestScope) -> Operation<'a, crate::memory::PageResult>,
    ) -> Result<Option<crate::memory::PageResult>> {
        let Some(hedges) = self.hedge.as_ref().filter(|h| h.enabled()) else {
            return Ok(None);
        };
        // The hedge primary and duplicate are the first two owners in the same
        // visiting order the serial fallback uses, so `primary_consumed` skips
        // exactly the owner the hedge already asked.
        let (object, page) = operation_identity(operation);
        let order = self.visit_order(candidates, object, page);
        let (first, second) = (order.get(0).unwrap_or(0), order.get(1).unwrap_or(0));
        // A pinned direct exchange carries one link, so its receiver cannot
        // probe predecessors; racer-core keeps one cold fallback fundable.
        let eligible = racer_core::read::walk::hedge_eligible(racer_core::read::walk::HedgeFacts {
            page_operation: matches!(operation, PeerOperation::Page { .. }),
            is_candidate: self.is_candidate(candidates),
            owners: candidates.ordered.len(),
            first,
            second,
            distinct: candidates.ordered.len() >= 2
                && candidates.ordered[first] != candidates.ordered[second],
            remaining_attempts: budget.remaining_attempts(),
            remaining_links: budget.remaining_links(),
        }) && [first, second].iter().all(|rank| {
            self.peers
                .direct_hedge_available(&candidates.membership, &candidates.ordered[*rank])
        });
        if !eligible {
            hedges.suppressed();
            return Ok(None);
        }
        check_budget(scope, budget)?;
        let reservation = (|| {
            let slot = hedges.acquire()?;
            let escrow = reserve_hedge_pages(admission, &context.object.cache, 1)?;
            // The caller already holds its serial plaintext page. Escrow is
            // additional accounting, not a buffer consumed by either validator.
            // Fund BOTH contenders before spending credits or changing routing.
            let working = reserve_hedge_pages(admission, &context.object.cache, 2)?;
            drop(working);
            Ok::<_, Error>((slot, escrow))
        })();
        let Ok((slot, _escrow)) = reservation else {
            hedges.suppressed();
            return Ok(None);
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
        continuation.primary_consumed = true;
        continuation.bounded_routes = true;
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
        continuation.stale = stale.get();
        continuation.primary_outcome = primary_outcome.get().or(Some(ProbeOutcome::Unreachable));
        check_budget(scope, budget)?;
        match result {
            Ok(result) => Ok(Some(result)),
            Err(error)
                if racer_core::read::Error::try_from(error)
                    .is_ok_and(racer_core::read::walk::hedge_fallback) =>
            {
                Ok(None)
            }
            Err(error) => Err(error),
        }
    }
    /// One admitted selection, one transfer grant. Routing chooses the primary of
    /// the oldest demanded page; the provider can choose any demanded page for
    /// which it is also primary. Backups are not speculatively contacted.
    /// Subscribe to candidate copies under the caller's demand and route budget.
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

    /// Request one selection from the provider of `first`, asking it to allow
    /// up to `flights` selections pending at once on the caller's contract.
    ///
    /// `contract(provider, flights)` builds the Subscribe for the chosen
    /// provider; `flights` is what to put in `Subscription::flights` (`None`
    /// when `flights <= 1` or the provider is known to be single-flight). It
    /// returns the subscription and the deadline to sign.
    ///
    /// Sequencing contract for concurrent selections. The provider keys one
    /// contract by (membership, receiver node, subscription id) and allows only
    /// one live contract per (membership, receiver node, version). Callers must:
    ///
    /// 1. Use one subscription id per (version, membership, provider) and run
    ///    every concurrent selection on that contract through this method.
    /// 2. Allocate each request's sequence and budgets atomically inside
    ///    `contract`: the sequence strictly greater than every sequence sent
    ///    on the contract, never reused even if the request fails, and the
    ///    page and byte budgets debited by one page (`PAGE_BYTES + 16`) per
    ///    allocation, never raised. The deadline never increases either.
    /// 3. Keep at most `granted` requests outstanding on the contract, where
    ///    `granted` starts at 1 and becomes the `flights` of the last
    ///    `Selected` result. With more outstanding, the provider supersedes
    ///    the oldest unselected request (a skipped selection) or refuses with
    ///    Overloaded. Sequences may reach the provider out of order; it accepts
    ///    a lower one once while it is within `granted` of the highest.
    /// 4. Send the full current demand on every request. Each request selects
    ///    only from its own demand, and the provider never selects a page the
    ///    contract already has in flight or completed, so concurrent requests
    ///    return distinct pages.
    /// 5. On `Refused`, drop back to one outstanding request on that provider
    ///    and retry with a new sequence. The provider may keep the old
    ///    contract's budgets, so continuing the same contract is valid.
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
            // A relay signs `Unavailable` when the next hop drops the
            // exchange, which is how an older provider behind a relay refuses
            // the field. Downgrade exactly as for a direct transport failure.
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
                racer_core::read::successor::HINT_CAPACITY,
                racer_core::read::successor::HINT_TTL,
            )),
            notices: RefCell::new(Notices::new(racer_core::read::notice::NOTICE_CAPACITY)),
            offers: Rc::default(),
            pings: Rc::default(),
            rank0_watch: RefCell::new(Rank0Watch::new(
                racer_core::read::rank0::WATCH_CAPACITY,
                racer_core::read::rank0::WATCH_TTL,
                racer_core::read::rank0::HANG_AFTER,
            )),
        }
    }

    /// Update the rank 0 watch from the outcome of this owner's page probe
    /// of rank 0 (`CandidateWalk::pending_rank0`, Known issue 39):
    /// a timeout notes the page with the request's deadline, an answer
    /// signed by rank 0 itself forgets it.
    fn note_rank0(
        &self,
        rank0: &NodeId,
        operation: &PeerOperation,
        outcome: &WalkOutcome,
        deadline: std::time::Instant,
    ) {
        let PeerOperation::Page { page, .. } = operation else {
            return;
        };
        let mut watch = self.rank0_watch.borrow_mut();
        match outcome {
            WalkOutcome::TimedOut => watch.timed_out(
                page.clone(),
                rank0.clone(),
                uring_runtime::environment::now(),
                deadline,
            ),
            WalkOutcome::Copy
            | WalkOutcome::Probe(
                ProbeOutcome::CopyMiss
                | ProbeOutcome::VersionUnavailable
                | ProbeOutcome::Unreachable,
            ) => watch.answered(page, rank0),
            _ => {}
        }
    }

    /// Whether this policy can wait for a destination that is behind.
    fn can_wait(&self) -> bool {
        #[cfg(test)]
        if self.instant_stale_wait.get() {
            return true;
        }
        self.reactor.is_some()
    }

    /// Let readers wait for a destination that is behind this node's
    /// membership instead of skipping it (Known issue 26).
    pub(crate) fn with_reactor(mut self, reactor: Rc<crate::runtime::Reactor>) -> Self {
        self.reactor = Some(reactor);
        self
    }

    /// Sleep until `until` under the request's scope. `false` without a
    /// timer.
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
        // Visit an older later owner before the newer owner that displaced
        // it, which would fetch the page again (Known issue 25).
        racer_core::read::candidates::lineage_first(order, &owner_arrivals(candidates), |rank| {
            candidates
                .membership
                .is_neighbor(&self.node, &ordered[rank])
        })
    }

    /// Bound each local peer exchange independently of the signed operation ceiling.
    pub(crate) fn with_attempt_timeout(mut self, timeout: std::time::Duration) -> Self {
        self.attempt_timeout = timeout;
        self
    }

    /// How long rank 0 may stay silent on a page after an owner's probe of
    /// it timed out before the owner presumes it hung (half the client
    /// request timeout; see `racer_core::read::rank0`, Known issue 39).
    pub(crate) fn with_rank0_hang_after(self, hang_after: std::time::Duration) -> Self {
        *self.rank0_watch.borrow_mut() = Rank0Watch::new(
            racer_core::read::rank0::WATCH_CAPACITY,
            racer_core::read::rank0::WATCH_TTL,
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
    /// ranking; never ranks (successor hints, Known issue 21).
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
    /// Walks one indexed resident at a time (including recovered idle pages),
    /// with at most 256 member scores and 64 ranking-cache CLOCK visits per turn.
    /// A completed pass returns Idle; schedule another pass on a bounded timer.
    /// Publication is reloaded every turn, so obsolete partial results cannot
    /// classify the new topology. Foreground scoped ranking warms the same cache.
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
        self.resolve_epoch(
            candidates,
            context,
            operation,
            scope,
            budget,
            |response| Box::pin(async move { Ok(response) }),
            false,
            HedgeContinuation::default(),
        )
    }

    /// Continue candidate resolution using the completed hedge's evidence.
    #[allow(clippy::too_many_arguments)] // Preserve the shared budget and continuation boundary.
    pub(crate) fn resolve_after_hedge<'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        validate: impl FnMut(VerifiedResponse) -> Operation<'a, crate::memory::AcquiredPage> + 'a,
        continuation: HedgeContinuation,
    ) -> Operation<'a, CandidateResolution<crate::memory::AcquiredPage>> {
        self.resolve_epoch(
            candidates,
            context,
            operation,
            scope,
            budget,
            validate,
            false,
            continuation,
        )
    }

    /// Resolve candidates within one membership epoch.
    #[allow(clippy::too_many_arguments)] // Forward explicit acquisition inputs without repackaging.
    fn resolve_epoch<'a, T: 'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        validate: impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a,
        retried: bool,
        continuation: HedgeContinuation,
    ) -> Operation<'a, CandidateResolution<T>> {
        Box::pin(async move {
            let mut trail = CandidateTrail {
                hedge_history_unavailable: continuation.primary_consumed || continuation.stale,
                ..Default::default()
            };
            let page = operation_identity(&operation).1.0;
            let kind = match &operation {
                PeerOperation::Page { .. } => CandidateOperation::Page,
                PeerOperation::Bootstrap { .. } => CandidateOperation::Bootstrap,
                PeerOperation::Metadata { .. } => CandidateOperation::Metadata,
                PeerOperation::Subscribe { .. } => CandidateOperation::Subscribe,
                PeerOperation::Warm { .. } => CandidateOperation::Page,
            };
            let membership = candidates.membership.version.0;
            let result = self
                .resolve_epoch_traced(
                    candidates,
                    context,
                    operation,
                    scope,
                    budget,
                    validate,
                    retried,
                    continuation,
                    &mut trail,
                )
                .await;
            if let Err(error) = &result {
                self.observer.candidate_final(CandidateFinal {
                    failure: Failure::new(Stage::CandidateExhausted, *error)
                        .request(scope)
                        .detail(Detail::Budget {
                            attempts: budget.remaining_attempts(),
                            links: budget.remaining_links(),
                        }),
                    operation: kind,
                    page,
                    membership,
                    trail,
                });
            }
            result
        })
    }

    /// Resolve one membership epoch while recording candidate evidence.
    #[allow(clippy::too_many_arguments)] // Keep the borrowed trace separate from acquisition state.
    fn resolve_epoch_traced<'a, 't, T: 'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'t mut AcquisitionBudget,
        mut validate: impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a,
        retried: bool,
        continuation: HedgeContinuation,
        trail: &'t mut CandidateTrail,
    ) -> Operation<'t, CandidateResolution<T>>
    where
        'a: 't,
    {
        Box::pin(async move {
            check_budget(scope, budget)?;
            let (object, page) = operation_identity(&operation);
            if object != &context.object {
                return Err(Error::InvalidRequest);
            }
            if continuation.stale {
                // A stale hedge refreshes to a newer membership when one is
                // available. Otherwise the stale contender is behind this node
                // (Known issue 3): walk this epoch's owners with bounded routes.
                let (next, retried) = match self.refresh_membership(&candidates, retried)? {
                    Some(latest) => (
                        self.candidates_scoped(latest, object, page, scope).await?,
                        true,
                    ),
                    None => (candidates, retried),
                };
                return self
                    .resolve_epoch_traced(
                        next,
                        context,
                        operation,
                        scope,
                        budget,
                        validate,
                        retried,
                        HedgeContinuation {
                            bounded_routes: true,
                            waits: continuation.waits,
                            ..Default::default()
                        },
                        trail,
                    )
                    .await;
            }
            // Public Candidates values are not authority. Recompute before origin.
            let expected = self
                .candidates_scoped(candidates.membership.clone(), object, page, scope)
                .await?;
            if expected.ordered != candidates.ordered
                || candidates.ordered.is_empty()
                || candidates.ordered.len() > 3
            {
                return Err(Error::IncompatibleMembership);
            }
            let rank = candidates
                .ordered
                .iter()
                .position(|node| node == &self.node);
            let page_operation = matches!(operation, PeerOperation::Page { .. });
            let previous = match rank {
                Some(rank) if page_operation => self
                    .previous_owners(&candidates, object, page, rank, scope)
                    .await
                    .unwrap_or_default(),
                _ => PreviousOwners::default(),
            };
            // Arrival order of the page's owners in this node's membership
            // chain: an owner may ask later owners that arrived before it,
            // which it may have displaced however many versions ago (Known
            // issue 25), and readers and delegations fund those probes.
            let since = owner_arrivals(&candidates);
            let lineage = if page_operation {
                racer_core::read::walk::lineage_counts(&since)
            } else {
                [0; racer_core::read::walk::MAX_HANDOFF]
            };
            // Lineage targets: later owners that arrived before this owner,
            // then members ranked just below the owners that were owners
            // when they arrived (Known issue 34, from the cached ranking
            // only). All are asked on their own grant, so a rank 0 reached
            // by a delegation with one credit asks them too (Known issue 35).
            let lineage_nodes = match rank {
                Some(rank) if page_operation => {
                    let mut targets =
                        racer_core::read::walk::lineage_targets(&candidates.ordered, &since, rank);
                    let demoted = self.placement.demoted(&candidates.membership, object, page);
                    let demoted_since: Vec<_> = demoted
                        .iter()
                        .map(|node| candidates.membership.since(node))
                        .collect();
                    // Arrivals at generation 0 are unknown: present since
                    // this node's chain began (Known issue 46).
                    for node in racer_core::read::walk::demoted_targets_from(
                        &since,
                        &demoted,
                        &demoted_since,
                        rank,
                        Some(0),
                    ) {
                        if targets.len() < racer_core::read::walk::MAX_LINEAGE
                            && !targets.contains(&node)
                        {
                            targets.push(node);
                        }
                    }
                    targets
                }
                _ => Vec::new(),
            };
            let mut handoff = match rank {
                Some(_) if page_operation => racer_core::read::walk::merge_handoff(
                    previous.handoff.clone(),
                    lineage_nodes.clone(),
                ),
                Some(_) => previous.handoff.clone(),
                None => Vec::new(),
            };
            // Later owners that asked this node for the page and got no copy
            // likely fetched it themselves: ask them copy-only before origin,
            // each on its own grant (Known issue 21, round 4 orphan2).
            let mut successors = 0;
            if let (Some(rank), PeerOperation::Page { page: id, .. }) = (rank, &operation) {
                let hints = self
                    .successors
                    .borrow_mut()
                    .probe(id, uring_runtime::environment::now());
                let targets = racer_core::read::successor::successor_targets(
                    &hints,
                    &candidates.ordered,
                    rank,
                );
                if !targets.is_empty() {
                    (handoff, successors) =
                        racer_core::read::successor::with_successors(targets, &handoff);
                }
            }
            // Successor targets first, then lineage targets, each on its own
            // grant; version - 1 targets after them spend the walk's credits.
            let granted = successors
                + racer_core::read::walk::granted_prefix(&handoff[successors..], &lineage_nodes);
            // A non-owner page walk may ask the page's overflow helper (Known
            // issue 16). It is chosen only when the walk asks for it.
            let helper = rank.is_none()
                && page_operation
                && candidates.membership.members().len() > candidates.ordered.len() + 1;
            // racer-core owns the visit order, funding, and evidence rules;
            // this loop performs the exchanges it plans.
            let mut walk = CandidateWalk::new(WalkConfig {
                rank,
                owners: candidates.ordered.len(),
                order: if rank.is_some() {
                    VisitOrder::canonical(0)
                } else {
                    self.visit_order(&candidates, object, page)
                },
                page_operation: matches!(operation, PeerOperation::Page { .. }),
                continuation: Continuation {
                    primary_consumed: continuation.primary_consumed,
                    primary_outcome: continuation.primary_outcome,
                    bounded_routes: continuation.bounded_routes,
                },
                handoff: handoff.len(),
                previous: previous.same_rank,
                helper,
                lineage,
                granted,
                // Granted probes go out at once (Known issue 45).
                parallel_granted: true,
                rank0: match (rank, &operation) {
                    (Some(rank), PeerOperation::Page { page: id, .. }) if rank > 0 => {
                        self.rank0_watch.borrow().state(
                            id,
                            &candidates.ordered[0],
                            uring_runtime::environment::now(),
                        )
                    }
                    _ => racer_core::read::rank0::Rank0State::Unknown,
                },
            });
            // This node owns the page under version - 1, so the stale
            // fallback (Known issue 19) is not available to this walk.
            let mut fallback_denied = false;
            // Waits this walk made for destinations that are behind (Known
            // issues 26 and 36).
            let mut waits = 0u8;
            let end = loop {
                let probe = match walk.next(budget)? {
                    Step::Probe(probe) => probe,
                    Step::Granted(probes) => {
                        // Every granted target at once, copy-only, each on
                        // its own grant and sharing one time share (Known
                        // issue 45). Only a validated copy matters; every
                        // other answer is a miss.
                        match self
                            .granted_batch(
                                &candidates,
                                &handoff,
                                &probes,
                                context,
                                &operation,
                                scope,
                                budget.deadline(),
                                &mut validate,
                                trail,
                            )
                            .await
                        {
                            Ok(Some(copy)) => {
                                walk.complete_granted(true);
                                return Ok(CandidateResolution::Copy(copy));
                            }
                            Ok(None) => {
                                walk.complete_granted(false);
                                continue;
                            }
                            Err(error) => {
                                walk.abandon(budget)?;
                                return Err(error);
                            }
                        }
                    }
                    Step::End(end) => break end,
                };
                let copy_only = probe.mode == WalkMode::CopyOnly;
                let membership = match (&previous.membership, probe.previous) {
                    (Some(previous), true) => previous,
                    _ => &candidates.membership,
                };
                // Canonical ranks past the owners address handoff targets
                // (`WalkConfig::handoff`) or, for a non-owner, the overflow
                // helper (`WalkConfig::helper`).
                let chosen;
                let destination = match probe.canonical.checked_sub(candidates.ordered.len()) {
                    Some(i) if rank.is_some() => &handoff[i],
                    Some(_) => match self.fanout_helper(&candidates, object, page) {
                        Some(helper) => {
                            chosen = helper;
                            &chosen
                        }
                        None => {
                            // Every candidate helper is excluded: a miss.
                            walk.complete(budget, WalkOutcome::Probe(ProbeOutcome::CopyMiss))?;
                            continue;
                        }
                    },
                    None => &candidates.ordered[probe.canonical],
                };
                let mut timed_out = false;
                // A relay, not the destination, signed the answer.
                let mut relayed = false;
                let sent = uring_runtime::environment::now();
                let response = self
                    .request_traced(
                        membership,
                        destination,
                        context,
                        &operation,
                        if copy_only {
                            FetchMode::CopyOnly
                        } else {
                            FetchMode::Acquire
                        },
                        scope,
                        walk.request_budget(budget),
                        probe.remaining_opportunities,
                        if probe.route == RouteMode::FundedRoute {
                            RequestMode::FundedRoute
                        } else {
                            RequestMode::SharedRoute
                        },
                        Some((trail, probe.canonical as u8)),
                        &mut timed_out,
                    )
                    .await;
                let outcome = match response {
                    Ok(response)
                        if matches!(response.response(), PeerResponse::Overloaded)
                            && response.origin().node() == destination =>
                    {
                        // The destination itself, not a relay, shed the
                        // request (for example a hot-page owner): the walk
                        // may ask the page's helper (Known issue 16).
                        WalkOutcome::Shed
                    }
                    Ok(response) => {
                        relayed = response.origin().node() != destination;
                        if matches!(response.response(), PeerResponse::StaleMembership) {
                            if walk.pending_handoff() || walk.pending_previous() {
                                // A stale handoff target is only a miss, and
                                // a predecessor asked under the previous
                                // membership no longer resolves it: neither
                                // refreshes this walk's membership.
                                walk.complete(budget, WalkOutcome::StaleMembership)?;
                                continue;
                            }
                            let refresh = match self.refresh_membership(&candidates, retried) {
                                Ok(refresh) => refresh,
                                Err(error) => {
                                    walk.abandon(budget)?;
                                    return Err(error);
                                }
                            };
                            let Some(refresh) = refresh else {
                                // The destination is behind this node and no
                                // newer membership exists locally: try the
                                // next owner (Known issue 3). A non-owner
                                // that recently replaced the previous
                                // membership walks the page's owners under it
                                // instead, once (Known issue 19).
                                let fallback = self
                                    .stale_fallback(&candidates, rank.is_some(), retried)
                                    .filter(|_| !fallback_denied);
                                // Rank the page under version - 1 before
                                // deciding: a node that owns it there may not
                                // walk it (that would give it origin
                                // authority), and must not lose the wait it
                                // would otherwise have had for this
                                // destination (Known issue 36).
                                let mut ranked = None;
                                if let Some(previous) = fallback
                                    && let Ok(previous) =
                                        self.candidates_scoped(previous, object, page, scope).await
                                {
                                    fallback_denied =
                                        !racer_core::read::stale::fallback_rank_allowed(
                                            previous
                                                .ordered
                                                .iter()
                                                .position(|node| node == &self.node),
                                        );
                                    if !fallback_denied {
                                        ranked = Some(previous);
                                    }
                                }
                                // One that cannot (it joined in this
                                // membership, or owns the page under version
                                // - 1) waits for the destination to adopt it
                                // and asks again, at most `MAX_STALE_WAITS`
                                // times (Known issues 26 and 36).
                                let now = uring_runtime::environment::now();
                                if let Some(wait) = racer_core::read::stale::stale_wait(
                                    racer_core::read::stale::StaleWaitFacts {
                                        owner: rank.is_some(),
                                        fallback: ranked.is_some(),
                                        waits: continuation.waits.saturating_add(waits),
                                        affordable: walk.retry_affordable(budget),
                                        remaining: budget
                                            .deadline()
                                            .min(scope.deadline.0)
                                            .saturating_duration_since(now),
                                    },
                                ) && self.can_wait()
                                {
                                    walk.retry_pending(budget)?;
                                    waits = waits.saturating_add(1);
                                    self.stale_sleep(now + wait, scope).await?;
                                    continue;
                                }
                                walk.complete(budget, WalkOutcome::StaleDestination)?;
                                if let Some(previous) = ranked {
                                    walk.abandon(budget)?;
                                    return self
                                        .resolve_epoch_traced(
                                            previous,
                                            context,
                                            operation,
                                            scope,
                                            budget,
                                            validate,
                                            true,
                                            HedgeContinuation {
                                                waits: continuation.waits.saturating_add(waits),
                                                ..Default::default()
                                            },
                                            trail,
                                        )
                                        .await;
                                }
                                continue;
                            };
                            walk.complete(budget, WalkOutcome::StaleMembership)?;
                            let next = self.candidates_scoped(refresh, object, page, scope).await?;
                            // Preserve the original operation (including ETag),
                            // cancellation/deadline and already spent link/attempt credits.
                            return self
                                .resolve_epoch_traced(
                                    next,
                                    context,
                                    operation,
                                    scope,
                                    budget,
                                    validate,
                                    true,
                                    HedgeContinuation {
                                        bounded_routes: continuation.bounded_routes,
                                        waits: continuation.waits.saturating_add(waits),
                                        ..Default::default()
                                    },
                                    trail,
                                )
                                .await;
                        }
                        // Validation never spends credits, so returning the
                        // child budget afterward is equivalent to before.
                        let validated =
                            validated_copy(response, &operation, !copy_only, &mut validate).await;
                        note_validation(trail, &validated);
                        match validated {
                            Ok(Ok(copy)) => {
                                if walk.pending_rank0() {
                                    self.note_rank0(
                                        destination,
                                        &operation,
                                        &WalkOutcome::Copy,
                                        budget.deadline().min(scope.deadline.0),
                                    );
                                }
                                walk.complete(budget, WalkOutcome::Copy)?;
                                return Ok(CandidateResolution::Copy(copy));
                            }
                            Ok(Err(outcome)) => WalkOutcome::Probe(outcome),
                            Err(error) => {
                                walk.abandon(budget)?;
                                return Err(error);
                            }
                        }
                    }
                    // Sent but unanswered within the attempt: an owner does
                    // not take origin authority from rank 0 for this.
                    Err(_) if timed_out => WalkOutcome::TimedOut,
                    Err(error) => match racer_core::read::Error::try_from(error)
                        .ok()
                        .and_then(racer_core::read::walk::transport_outcome)
                    {
                        Some(outcome) => WalkOutcome::Transport(outcome),
                        // A successor probe is opportunistic and runs on its
                        // own grant: any failure of it is only a miss, unless
                        // this request itself is over.
                        None if walk.pending_successor() && scope.check().is_ok() => {
                            WalkOutcome::Transport(ProbeOutcome::Unreachable)
                        }
                        None => {
                            walk.abandon(budget)?;
                            return Err(error);
                        }
                    },
                };
                if walk.pending_rank0() && !relayed {
                    // Only rank 0's own answer (or its silence) says anything
                    // about rank 0.
                    self.note_rank0(
                        destination,
                        &operation,
                        &outcome,
                        budget.deadline().min(scope.deadline.0),
                    );
                }
                if walk.pending_rank0()
                    && outcome == WalkOutcome::TimedOut
                    && let PeerOperation::Page { page: id, .. } = &operation
                {
                    let health = self.peers.link_health();
                    // Ask whether rank 0 answers at all (Known issue 43):
                    // `poll_pings` sends the ping, and a silent rank 0 then
                    // fails this owner's next probe at once, which gives it
                    // origin authority without another wait.
                    health.silence_timed_out(
                        destination,
                        id.clone(),
                        sent,
                        racer_core::peer::silence::RANK0_TIMEOUTS,
                    );
                }
                walk.complete(budget, outcome)?;
            };
            if walk.funding_stopped() {
                trail.site = CandidateSite::Funding;
            }
            check_budget(scope, budget)?;
            match end {
                WalkEnd::Origin { rank, evidence } => {
                    Ok(CandidateResolution::Origin(OriginAuthority {
                        diagnostic: trail.clone(),
                        membership: candidates.membership,
                        node: self.node.clone(),
                        object: object.clone(),
                        page,
                        predecessor_evidence: evidence,
                        rank,
                    }))
                }
                WalkEnd::VersionUnavailable => Err(Error::VersionUnavailable),
                WalkEnd::Exhausted { funding } => {
                    if !funding {
                        trail.site = CandidateSite::Exhausted;
                    }
                    self.observer.record(
                        Failure::new(Stage::CandidateExhausted, Error::Unavailable)
                            .request(scope)
                            .detail(Detail::Budget {
                                attempts: budget.remaining_attempts(),
                                links: budget.remaining_links(),
                            }),
                    );
                    Err(Error::Unavailable)
                }
                WalkEnd::Copy | WalkEnd::StaleMembership => {
                    unreachable!("handled while probing")
                }
            }
        })
    }

    /// Send a run of granted probes at once (`Step::Granted`, Known issue
    /// 45): copy-only, each on its own grant
    /// (`racer_core::read::walk::granted_budget`), all sharing the first
    /// probe's time share, so the run costs one round trip instead of one per
    /// target. Waits for every answer before validating, so no exchange is
    /// left running; returns the first validated copy in probe order. Any
    /// other answer, and any transport failure while `scope` still runs, is
    /// a miss, as for a sequential granted probe.
    #[allow(clippy::too_many_arguments)] // The batch borrows the walk's distinct inputs.
    async fn granted_batch<'a, T>(
        &'a self,
        candidates: &Candidates,
        handoff: &[NodeId],
        probes: &[racer_core::read::walk::Probe],
        context: &'a OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
        deadline: std::time::Instant,
        validate: &mut (impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a),
        trail: &mut CandidateTrail,
    ) -> Result<Option<T>> {
        let mut grants: Vec<(AcquisitionBudget, bool)> = probes
            .iter()
            .map(|_| (racer_core::read::walk::granted_budget(deadline), false))
            .collect();
        let owners = candidates.ordered.len();
        let responses = futures::future::join_all(probes.iter().zip(grants.iter_mut()).map(
            |(probe, (grant, timed_out))| {
                self.request_traced(
                    &candidates.membership,
                    &handoff[probe.canonical - owners],
                    context,
                    operation,
                    FetchMode::CopyOnly,
                    scope,
                    grant,
                    probe.remaining_opportunities,
                    RequestMode::FundedRoute,
                    None,
                    timed_out,
                )
            },
        ))
        .await;
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
    /// sheds the request (`racer_core::read::fanout`): one of the page's
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
        racer_core::read::fanout::fanout_helper(
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

    /// What an owner at `rank` uses from the membership published just
    /// before `candidates.membership`, if still retained:
    ///
    /// - handoff targets it asks copy-only before origin (Known issue 6):
    ///   previous owners that are current members, without this node and its
    ///   current predecessors; empty when the owner set did not change;
    /// - predecessors that held the same rank there, which it asks under that
    ///   membership so one that has not adopted the current version yet still
    ///   serves (Known issue 12), when this node replaced it at most
    ///   `PREVIOUS_MEMBERSHIP_WITHIN` ago and is a member of it.
    ///
    /// Ranking failures are not errors; the walk just goes without.
    fn previous_owners<'a>(
        &'a self,
        candidates: &'a Candidates,
        object: &'a ObjectId,
        page: PageNumber,
        rank: usize,
        scope: &'a RequestScope,
    ) -> Operation<'a, PreviousOwners> {
        Box::pin(async move {
            let Some(version) = candidates.membership.version.0.checked_sub(1) else {
                return Ok(PreviousOwners::default());
            };
            let now = uring_runtime::environment::now();
            let Ok(Some(previous)) = self.published.resolve(version, now) else {
                return Ok(PreviousOwners::default());
            };
            // Ask predecessors under the previous membership only while
            // peers that adopted the current one still hold it in grace
            // (`PREVIOUS_MEMBERSHIP_WITHIN`); a lease held only here says
            // nothing about a peer.
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
            let Ok(previous) = self.candidates_scoped(previous, object, page, scope).await else {
                return Ok(PreviousOwners::default());
            };
            let handoff = racer_core::read::walk::handoff_targets(
                &previous.ordered,
                &candidates.ordered,
                rank,
            )
            .into_iter()
            .filter(|node| candidates.membership.member(node).is_ok())
            .collect();
            let same_rank = if recent.is_some() {
                racer_core::read::walk::same_rank_previous(
                    &previous.ordered,
                    &candidates.ordered,
                    rank,
                )
            } else {
                0
            };
            Ok(PreviousOwners {
                handoff,
                same_rank,
                membership: recent.filter(|_| same_rank != 0),
            })
        })
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

    /// After a `StaleMembership` answer, decide with
    /// `racer_core::read::walk::stale_action` whether to refresh. `Some` is the
    /// strictly newer membership to rank again; `None` means the destination
    /// is behind and the caller moves to the next owner.
    /// After a destination under `candidates` was behind this node, the
    /// previous membership to walk the page's owners under instead, when
    /// `racer_core::read::stale` allows it: this node does not own the page,
    /// has not refreshed or fallen back yet, and holds version - 1, replaced
    /// within `PREVIOUS_MEMBERSHIP_WITHIN` (peers still resolve it) and
    /// listing this node (peers reject non-members).
    fn stale_fallback(
        &self,
        candidates: &Candidates,
        owner: bool,
        retried: bool,
    ) -> Option<std::sync::Arc<crate::topology::Membership>> {
        let mut facts = racer_core::read::stale::StaleSkip {
            owner,
            retried,
            previous_recent_member: true,
        };
        if !racer_core::read::stale::previous_fallback(facts) {
            return None;
        }
        let version = candidates.membership.version.0.checked_sub(1)?;
        let previous = self
            .published
            .resolve_recent(
                version,
                uring_runtime::environment::now(),
                racer_core::read::walk::PREVIOUS_MEMBERSHIP_WITHIN,
            )
            .ok()
            .flatten()
            .filter(|previous| previous.member(&self.node).is_ok());
        facts.previous_recent_member = previous.is_some();
        racer_core::read::stale::previous_fallback(facts)
            .then_some(previous)
            .flatten()
    }

    fn refresh_membership(
        &self,
        previous: &Candidates,
        retried: bool,
    ) -> Result<Option<std::sync::Arc<crate::topology::Membership>>> {
        let latest = self
            .published
            .current()?
            .map(|published| published.membership.clone());
        let action = racer_core::read::walk::stale_action(
            retried,
            previous.membership.version.0,
            latest.as_ref().map(|latest| latest.version.0),
        )?;
        Ok(match action {
            racer_core::read::walk::StaleAction::Refresh => latest,
            racer_core::read::walk::StaleAction::Skip => None,
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

    /// One CopyOnly page request for a `gpu-read` on GPU `ordinal`: the page
    /// may land in a GPU landing buffer (see [`Requester::request_landing`]).
    /// The caller sends it to one provider only.
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
            // A destination this node holds silent fails at once, before any
            // credit is spent (Known issues 41 and 43); its own pings go out.
            if !matches!(mode, FetchMode::Ping) && self.peers.link_health().silent(destination) {
                return Err(Error::Unavailable);
            }
            // racer-core reserves the complete route and remote credits before
            // sending and derives the share, local cap, and body budget. Lost
            // responses cannot refund forwarded links; no retry gets fresh credits.
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
            // A verified `StaleMembership` comes from a node that could not
            // resolve this request's membership, so the remote credits it
            // carried were never used: return them (Known issue 26).
            if let Ok(Delivered::Response(response)) = &response
                && matches!(response.response(), PeerResponse::StaleMembership)
            {
                budget.refund_remote(attempts)?;
            }
            check_budget(scope, budget)?;
            let stop = stopped.or_else(|| attempt_scope.check().err());
            facts.stop = stop;
            // The caller's scope and budget still hold here, so a deadline is
            // the attempt's own bound: the request was sent and is unanswered
            // (`WalkOutcome::TimedOut`, Known issue 31).
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

    /// This node fetched `page` from origin at `rank` among `owners`: when
    /// that is after rank 0, remember to offer rank 0 the copy later
    /// (Known issue 37, `racer_core::read::notice`). Returns whether a notice
    /// was noted.
    pub(crate) fn note_orphan(
        &self,
        page: &crate::model::PageId,
        owners: &[NodeId],
        rank: usize,
    ) -> bool {
        use racer_core::read::notice::should_notice;
        let Some(rank0) = owners.first() else {
            return false;
        };
        if !should_notice(Some(rank)) || owners.get(rank) != Some(&self.node) || rank0 == &self.node
        {
            return false;
        }
        self.notices.borrow_mut().note(
            page.clone(),
            rank0.clone(),
            uring_runtime::environment::now(),
        );
        true
    }

    /// Send due copy offers (Known issue 37), at most
    /// [`MAX_OFFERS_IN_FLIGHT`] at a time, each on its own detached driver
    /// task and fixed route grant (never a reader's budget). `holds` reports
    /// whether this node still holds a page; a page it lost is dropped. Best
    /// effort: an offer that is not answered `Miss` by its destination is
    /// tried again on the notice's schedule or expires, and never reaches a
    /// reader. Call once per worker service turn.
    pub(crate) fn poll_offers(
        self: &Rc<Self>,
        now: std::time::Instant,
        holds: impl Fn(&crate::model::PageId) -> bool,
        metrics: &Metrics,
    ) {
        while self.offers.get() < MAX_OFFERS_IN_FLIGHT {
            let Some(due) = self.notices.borrow_mut().pop_due(now) else {
                return;
            };
            if !holds(&due.key) {
                continue;
            }
            let Ok(permit) = uring_runtime::drivers::reserve() else {
                // No driver slot this turn: count it as a failed offer.
                let _ = self.notices.borrow_mut().failed(due, now);
                return;
            };
            let flight = OfferFlight::new(self.offers.clone());
            let policy = self.clone();
            let metrics = metrics.clone();
            permit.submit_detached(Box::pin(async move {
                let _flight = flight;
                metrics.record(Event::OrphanOfferSent, 1);
                match policy.offer(&due).await {
                    Ok(None) => {}
                    Ok(Some(true)) => metrics.record(Event::OrphanOfferDelivered, 1),
                    Ok(Some(false)) | Err(_) => {
                        metrics.record(Event::OrphanOfferFailed, 1);
                        let now = uring_runtime::environment::now();
                        let _ = policy.notices.borrow_mut().failed(due, now);
                    }
                }
                Ok::<(), Error>(())
            }));
        }
    }

    /// This worker's link health (tests set its silence timing).
    #[cfg(test)]
    pub(crate) fn link_health(&self) -> Rc<crate::topology::LinkHealth> {
        self.peers.link_health()
    }

    /// Send due silence pings (Known issues 41 and 43,
    /// `racer_core::peer::silence`), at most [`MAX_PINGS_IN_FLIGHT`] at a
    /// time, each on its own detached driver task and fixed route grant
    /// (never a reader's budget). Call once per worker service turn.
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

    /// Ping `peer` about `page` (mode `ping`, answered `Miss` at once).
    /// `Some(true)`: `peer` signed an answer. `Some(false)`: unanswered
    /// within the attempt, or a relay answered `Unavailable` (it holds `peer`
    /// silent, or cannot reach it). `None`: anything else (a relay's
    /// overload, a local failure) proves nothing.
    async fn ping(&self, peer: &NodeId, page: &crate::model::PageId) -> Option<bool> {
        use racer_core::read::notice::NOTICE_LINKS;
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
        // A fresh grant: one local exchange, no remote attempts, and
        // `NOTICE_LINKS` route links.
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
        match response {
            Ok(response) if response.origin().node() == peer => Some(true),
            Ok(response) if matches!(response.response(), PeerResponse::Unavailable) => Some(false),
            Ok(_) => None,
            Err(_) if timed_out => Some(false),
            Err(Error::DeadlineExceeded) => Some(false),
            Err(_) => None,
        }
    }

    /// Send one copy offer for `due` to the page's current rank 0. `None`
    /// when this node is no longer an owner after rank 0 (the notice is
    /// dropped); otherwise whether rank 0 itself answered `Miss`.
    async fn offer(
        &self,
        due: &racer_core::read::notice::Due<crate::model::PageId, NodeId>,
    ) -> Result<Option<bool>> {
        use racer_core::read::notice::{NOTICE_LINKS, notice_target};
        let page = &due.key;
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
        let Some(target) = notice_target(&self.node, &owners, true) else {
            return Ok(None);
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
        // A fresh grant: one local exchange, no remote attempts, and
        // `NOTICE_LINKS` route links.
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
        Ok(Some(response.is_ok_and(|response| {
            matches!(response.response(), PeerResponse::Miss) && response.origin().node() == &target
        })))
    }

    pub fn origin_miss_error(&self, authority: &OriginAuthority) -> Error {
        racer_core::read::candidates::origin_miss_error(&authority.predecessor_evidence).into()
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
                racer_core::read::candidates::unusable_copy(error, operation_kind(operation))
                    .is_some()
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

    pub fn validate(&self, object: &ObjectId, page: PageNumber) -> Result<()> {
        racer_core::read::candidates::validate_origin_authority(
            racer_core::read::candidates::AuthorityFacts {
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
pub(crate) fn operation_kind(
    operation: &PeerOperation,
) -> racer_core::read::candidates::OperationKind {
    use racer_core::read::candidates::OperationKind;
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

// `acquire` describes the actual probe, which may be CopyOnly even when the
// caller's operation requests acquisition (predecessors and remaining copies).
// racer-core owns the outcome table; this supplies payload identity checks.
fn classify(
    response: &PeerResponse,
    operation: &PeerOperation,
    acquire: bool,
) -> Result<Option<ProbeOutcome>> {
    racer_core::read::candidates::classify(
        response.kind(),
        operation_kind(operation),
        acquire,
        || validate_payload(response, operation),
    )
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

// Opt-in, node-shared speculative page capacity. No background task or byte bypass.
//
// RACER_PAGE_HEDGE_SLOTS=0 disables hedging. RACER_PAGE_HEDGE_DELAY_MS defaults
// to 100; RACER_PAGE_HEDGE_BYTES defaults to one maximum duplicate
// plaintext+ciphertext pair (32 MiB + 16).
// Only noncandidate plaintext fixed-page reads with two direct healthy neighbors
// qualify. Their distinct destinations are pinned first hops over HTTP. Metadata,
// subscription selection, ciphertext relays, native paths and full GETs do not race.
// One CopyOnly secondary may start; normal acquisition credits are partitioned,
// never replenished. Duplicate escrow is held in addition to actual buffer quota,
// intentionally over-accounting rather than requiring a transport-wide reservation
// handoff. Admission also preflights two full contender buffers before credits:
// the existing serial page plus escrow plus both contenders require 64 MiB of
// plaintext capacity in an otherwise empty worker. Low quotas fall back serially.
// Pair exchanges have a local one-third-remaining cap; signed authority stays
// unchanged. Serial continuation skips the consumed primary, keeps the CopyOnly
// secondary eligible for Acquire, and preserves credits for later candidates.
// One authorized cold fallback requires six original attempts/ten links; the
// standard eight/sixteen allowance can hedge without budget inflation. A second
// cold fallback needs additional original credits and is skipped if underfunded.
// A valid winner waits for the losing exchange/crypto fence before return:
// this can limit the latency benefit and is not an early-publication implementation.

pub const DUPLICATE_BYTES: usize = crate::model::PAGE_BYTES as usize * 2 + 16;
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
    racer_core::read::Error::try_from(error).is_ok_and(racer_core::read::policy::recoverable)
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
    fn visit(
        selection: ReplicaSelection,
        len: usize,
        neighbors: &[usize],
        scores: [u64; 3],
    ) -> Vec<usize> {
        super::visit_order(selection, len, |r| neighbors.contains(&r), |r| scores[r])
            .ranks()
            .collect()
    }

    #[test]
    fn visit_order_primary_is_canonical_regardless_of_neighbors_or_scores() {
        for len in 0..=4 {
            let expected: Vec<_> = (0..len.min(3)).collect();
            assert_eq!(
                visit(ReplicaSelection::Primary, len, &[2], [1, 2, 3]),
                expected
            );
            assert_eq!(
                visit(ReplicaSelection::Primary, len, &[], [9, 0, 5]),
                expected
            );
        }
    }

    #[test]
    fn visit_order_spread_prefers_neighbors_then_highest_score() {
        use ReplicaSelection::Spread;
        // All neighbors: pure rendezvous order by descending score.
        assert_eq!(visit(Spread, 3, &[0, 1, 2], [1, 3, 2]), vec![1, 2, 0]);
        // Neighbors first even with lower scores; non-neighbors last.
        assert_eq!(visit(Spread, 3, &[2], [9, 8, 1]), vec![2, 0, 1]);
        assert_eq!(visit(Spread, 3, &[0, 2], [1, 9, 2]), vec![2, 0, 1]);
        // No neighbors: rendezvous order over all owners.
        assert_eq!(visit(Spread, 3, &[], [1, 3, 2]), vec![1, 2, 0]);
        // Equal scores keep canonical order within a group.
        assert_eq!(visit(Spread, 3, &[0, 1, 2], [5, 5, 5]), vec![0, 1, 2]);
        // Two candidates and fewer.
        assert_eq!(visit(Spread, 2, &[1], [9, 1, 0]), vec![1, 0]);
        assert_eq!(visit(Spread, 2, &[0, 1], [1, 9, 0]), vec![1, 0]);
        assert_eq!(visit(Spread, 1, &[], [0, 9, 9]), vec![0]);
        assert!(visit(Spread, 0, &[0], [0, 0, 0]).is_empty());
        // Never more than three ranks.
        assert_eq!(visit(Spread, 5, &[0, 1, 2], [1, 2, 3]), vec![2, 1, 0]);
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

    /// Known issue 25: a reader whose first choice arrived in the membership
    /// chain after a later owner visits that older owner first, which may
    /// hold the copy it fetched while it ranked higher. Primary keeps the
    /// canonical order; with no arrivals in the chain nothing changes.
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
        calls: RefCell<Vec<(NodeId, bool, u32)>>,
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

    /// Known issue 16: a non-owner whose owner itself sheds it (a signed
    /// `Overloaded` from the destination) asks the page's overflow helper
    /// once, with an Acquire funded from what is left (too little for the
    /// helper to ask another helper), then the remaining owners. A transport
    /// `Overloaded` (no signed answer, as from a congested route) does not.
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
                    racer_core::read::range::PAGE_ATTEMPTS,
                    racer_core::read::range::PAGE_LINKS,
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
                let (_, copy, attempts) = &calls[1];
                assert!(!copy);
                assert!(*attempts >= 1);
                assert!(*attempts < racer_core::read::fanout::HELPER_MIN_ATTEMPTS);
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
