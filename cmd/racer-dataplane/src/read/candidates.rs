//! Ranked acquisition and copy-only predecessor probes. Only a validated local
//! candidate can mint origin authority; request headers never change placement.
use super::flight::AcquisitionBudget;
use crate::telemetry::failures::{Detail, Failure, Observer, Stage};
use crate::{
    error::{Error, Operation, Result},
    model::{AttemptId, MetadataSelector, NodeId, ObjectId, OriginContext, PageNumber},
    peer::{
        PeerClient,
        protocol::{
            FetchMode, Operation as PeerOperation, PeerRequest, PeerResponse, VerifiedResponse,
        },
    },
    runtime::deadline::{Deadline, RequestScope},
    security::credentials::CredentialCrypto,
    topology::{
        membership::MembershipLease,
        placement::{Candidates, Placement},
        routing::RouteBudget,
    },
};
#[cfg(test)]
use std::cell::RefCell;
#[cfg(test)]
use std::time::Instant;
use std::{rc::Rc, sync::Arc};

fn reserve_hedge_pages(
    admission: &flow_control::Quotas<crate::runtime::admission::AdmissionPolicy>,
    cache: &crate::model::CacheId,
    pages: usize,
) -> Result<(
    flow_control::Charge<crate::runtime::admission::AdmissionPolicy>,
    flow_control::Charge<crate::runtime::admission::AdmissionPolicy>,
)> {
    use crate::model::{PAGE_BYTES, ResourceClass};
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
    membership: MembershipLease,
    node: NodeId,
    object: ObjectId,
    page: PageNumber,
    predecessor_evidence: Vec<ProbeOutcome>,
    rank: usize,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProbeOutcome {
    CopyMiss,
    Unreachable,
    Overloaded,
    VersionUnavailable,
    /// An authenticated response carried a copy that could not be used locally.
    UnusableCopy,
}
pub struct CandidatePolicy {
    hedge: Option<std::sync::Arc<super::hedge::Hedges>>,
    attempt_timeout: std::time::Duration,
    observer: Observer,
    node: NodeId,
    placement: Rc<Placement>,
    peers: Rc<dyn PeerClient>,
    credentials: Rc<CredentialCrypto>,
    published: Arc<crate::control::state::PublishedState>,
}
#[derive(Clone, Copy)]
enum RequestMode {
    SharedRoute,
    FundedRoute,
    DirectHedge,
}
#[derive(Default, Clone, Copy)]
pub(crate) struct HedgeContinuation {
    // This is local acquisition progress, never placement/origin authority.
    pub primary_consumed: bool,
    pub primary_outcome: Option<ProbeOutcome>,
    pub stale: bool,
    pub bounded_routes: bool,
}
impl CandidatePolicy {
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&std::sync::Arc<super::hedge::Hedges>> {
        self.hedge.as_ref()
    }
    /// Only Fill's plaintext fixed-page path calls this. Direct HTTP destinations
    /// prove independent first hops. Drain both attempts before releasing escrow.
    pub(crate) async fn hedge_page<'a, T: 'a>(
        &'a self,
        candidates: &Candidates,
        context: &'a OriginContext,
        operation: &PeerOperation,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        admission: &Rc<flow_control::Quotas<crate::runtime::admission::AdmissionPolicy>>,
        continuation: &mut HedgeContinuation,
        validate: impl Fn(VerifiedResponse, RequestScope) -> Operation<'a, T>,
    ) -> Result<Option<T>> {
        let Some(hedges) = self.hedge.as_ref().filter(|h| h.enabled()) else {
            return Ok(None);
        };
        let eligible = matches!(operation, PeerOperation::Page { .. })
            && !self.is_candidate(candidates)
            && candidates.ordered.len() >= 2
            && candidates.ordered[0] != candidates.ordered[1]
            // Preserve ONE cold fallback: primary(2) + duplicate(1) +
            // rank1(sender+probe+origin=3), with links1+1+8. Additional
            // candidates are attempted only when the original remainder funds them.
            && budget.remaining_attempts() >= 6
            && budget.remaining_links() >= 10
            && candidates.ordered[..2]
                .iter()
                .all(|n| self.peers.direct_hedge_available(&candidates.membership, n));
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
        let mut secondary_budget = budget.partition(1, 1)?;
        let mut primary_budget = budget.partition(2, 1)?;
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
                    &candidates.ordered[0],
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
                .direct_hedge_available(&candidates.membership, &candidates.ordered[1])
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
                    &candidates.ordered[1],
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
        let result = super::hedge::race(
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
            Err(
                Error::Unavailable
                | Error::Io
                | Error::Overloaded
                | Error::CorruptRecord
                | Error::MissingKey
                | Error::InvalidRequest
                | Error::HeaderTooLarge,
            ) => Ok(None),
            Err(error) => Err(error),
        }
    }
    /// One admitted selection, one transfer grant. Routing chooses the primary of
    /// the oldest demanded page; the provider can choose any demanded page for
    /// which it is also primary. Backups are not speculatively contacted.
    pub(crate) async fn subscribe(
        &self,
        version: crate::model::ObjectVersion,
        demand: crate::peer::subscriptions::Demand,
        scheduler: &super::subscription::Scheduler,
        membership: MembershipLease,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<Option<VerifiedResponse>> {
        let first = demand
            .intervals()
            .first()
            .ok_or(Error::InvalidRequest)?
            .start;
        let rank = self
            .candidates_scoped(
                membership.clone(),
                &version.object,
                PageNumber(first),
                scope,
            )
            .await?;
        let destination = rank.ordered.first().ok_or(Error::Unavailable)?;
        if destination == &self.node {
            return Ok(None);
        }
        let (subscription, deadline) = scheduler.contract(
            version,
            membership.version,
            destination.clone(),
            demand,
            scope.deadline.0.min(budget.deadline()),
        )?;
        let mut selected_scope = scope.clone();
        selected_scope.deadline.0 = deadline;
        let operation = PeerOperation::Subscribe {
            subscription,
            mode: FetchMode::Acquire,
        };
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
            Err(Error::Unavailable | Error::Io | Error::Overloaded) => {
                budget.note_route_failure();
                return Ok(None);
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
                Ok(Some(response))
            }
            Some(_) => Ok(None),
        }
    }
    pub fn new(
        node: NodeId,
        placement: Rc<Placement>,
        peers: Rc<dyn PeerClient>,
        credentials: Rc<CredentialCrypto>,
        published: Arc<crate::control::state::PublishedState>,
    ) -> Self {
        Self {
            hedge: None,
            attempt_timeout: std::time::Duration::from_secs(30),
            observer: Observer::default(),
            node,
            placement,
            peers,
            credentials,
            published,
        }
    }

    pub(crate) fn with_observer(mut self, observer: Observer) -> Self {
        self.observer = observer;
        self
    }
    pub(crate) fn with_hedges(mut self, hedges: std::sync::Arc<super::hedge::Hedges>) -> Self {
        self.hedge = Some(hedges);
        self
    }

    /// Bound each local peer exchange independently of the signed operation ceiling.
    pub(crate) fn with_attempt_timeout(mut self, timeout: std::time::Duration) -> Self {
        self.attempt_timeout = timeout;
        self
    }

    pub fn candidates(
        &self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Result<Candidates> {
        self.placement.rank(membership, object, page)
    }

    pub fn is_candidate(&self, candidates: &Candidates) -> bool {
        candidates
            .ordered
            .iter()
            .take(3)
            .any(|node| node == &self.node)
    }
    pub fn maintain(&self, membership: &MembershipLease) -> Result<()> {
        self.placement.maintain(membership)
    }
    pub fn candidates_scoped<'a>(
        &'a self,
        membership: MembershipLease,
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

    pub(crate) fn resolve_after_hedge<'a, T: 'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        validate: impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a,
        continuation: HedgeContinuation,
    ) -> Operation<'a, CandidateResolution<T>> {
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

    fn resolve_epoch<'a, T: 'a>(
        &'a self,
        candidates: Candidates,
        context: &'a OriginContext,
        operation: PeerOperation,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        mut validate: impl FnMut(VerifiedResponse) -> Operation<'a, T> + 'a,
        retried: bool,
        continuation: HedgeContinuation,
    ) -> Operation<'a, CandidateResolution<T>> {
        Box::pin(async move {
            check_budget(scope, budget)?;
            let (object, page) = operation_identity(&operation);
            if object != &context.object {
                return Err(Error::InvalidRequest);
            }
            if continuation.stale {
                let next = self
                    .refresh_candidates(&candidates, object, page, scope, retried)
                    .await?;
                return self
                    .resolve_epoch(
                        next,
                        context,
                        operation,
                        scope,
                        budget,
                        validate,
                        true,
                        HedgeContinuation {
                            bounded_routes: true,
                            ..Default::default()
                        },
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
            let mut evidence = Vec::new();
            let mut saw_transient = false;
            let mut saw_version = false;
            let count = rank.unwrap_or(candidates.ordered.len());
            for (index, destination) in candidates.ordered[..count].iter().enumerate() {
                if continuation.primary_consumed && index == 0 {
                    let outcome = continuation
                        .primary_outcome
                        .unwrap_or(ProbeOutcome::Unreachable);
                    saw_version |= outcome == ProbeOutcome::VersionUnavailable;
                    saw_transient |= matches!(
                        outcome,
                        ProbeOutcome::Unreachable
                            | ProbeOutcome::Overloaded
                            | ProbeOutcome::UnusableCopy
                    );
                    continue;
                }
                let mode = if rank.is_some() {
                    FetchMode::CopyOnly
                } else {
                    FetchMode::Acquire
                };
                let mut bounded = if rank.is_some()
                    && budget.remaining_links() < (count - index) as u8 * budget.route_links()
                {
                    // Receiver CopyOnly predecessor probes share the inherited
                    // route pool; the first must not consume every link after
                    // an incoming eight-link envelope selected failure routing.
                    let remaining = (count - index) as u8;
                    Some(budget.partition(1, (budget.remaining_links() / remaining).min(4))?)
                } else if continuation.bounded_routes {
                    let attempts = if rank.is_some() { 1 } else { index as u32 + 2 };
                    let links = if rank.is_some() { 4 } else { 8 };
                    if budget.remaining_attempts() < attempts || budget.remaining_links() < links {
                        // Never send an Acquire whose receiving coordinator cannot
                        // probe its predecessors and still exercise origin authority.
                        saw_transient = true;
                        break;
                    }
                    let mut child = budget.partition(attempts, links)?;
                    // Receiver needs incoming link plus all predecessor routes.
                    // A rank2 cold fill cannot execute with a four-link envelope.
                    if rank.is_none() {
                        child.note_route_failure();
                    }
                    Some(child)
                } else {
                    None
                };
                let request_budget = bounded.as_mut().unwrap_or(&mut *budget);
                let response = self
                    .request_mode(
                        &candidates.membership,
                        destination,
                        context,
                        &operation,
                        mode,
                        scope,
                        request_budget,
                        (count - index + usize::from(rank.is_some())) as u32,
                        if continuation.bounded_routes {
                            RequestMode::FundedRoute
                        } else {
                            RequestMode::SharedRoute
                        },
                    )
                    .await;
                if let Some(bounded) = bounded {
                    budget.reunite(bounded)?;
                }
                match response {
                    Ok(response) => {
                        if matches!(response.response(), PeerResponse::StaleMembership) {
                            let next = self
                                .refresh_candidates(&candidates, object, page, scope, retried)
                                .await?;
                            // Preserve the original operation (including ETag),
                            // cancellation/deadline and already spent link/attempt credits.
                            return self
                                .resolve_epoch(
                                    next,
                                    context,
                                    operation,
                                    scope,
                                    budget,
                                    validate,
                                    true,
                                    HedgeContinuation {
                                        bounded_routes: continuation.bounded_routes,
                                        ..Default::default()
                                    },
                                )
                                .await;
                        }
                        match validated_copy(response, &operation, rank.is_none(), &mut validate)
                            .await?
                        {
                            Ok(copy) => return Ok(CandidateResolution::Copy(copy)),
                            Err(outcome) => {
                                saw_version |= outcome == ProbeOutcome::VersionUnavailable;
                                saw_transient |= matches!(
                                    outcome,
                                    ProbeOutcome::Unreachable
                                        | ProbeOutcome::Overloaded
                                        | ProbeOutcome::UnusableCopy
                                );
                                evidence.push(outcome);
                                if matches!(
                                    outcome,
                                    ProbeOutcome::Unreachable | ProbeOutcome::Overloaded
                                ) {
                                    budget.note_route_failure();
                                }
                            }
                        }
                    }
                    Err(Error::Unavailable | Error::Io) => {
                        evidence.push(ProbeOutcome::Unreachable);
                        saw_transient = true;
                        budget.note_route_failure();
                    }
                    Err(Error::Overloaded) => {
                        evidence.push(ProbeOutcome::Overloaded);
                        saw_transient = true;
                        budget.note_route_failure();
                    }
                    Err(error) => return Err(error),
                }
            }
            check_budget(scope, budget)?;
            if rank.is_some() {
                Ok(CandidateResolution::Origin(OriginAuthority {
                    membership: candidates.membership,
                    node: self.node.clone(),
                    object: object.clone(),
                    page,
                    predecessor_evidence: evidence,
                    rank: rank.expect("candidate checked"),
                }))
            } else if saw_version && !saw_transient {
                Err(Error::VersionUnavailable)
            } else {
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
            check_budget(scope, budget)?;
            let rank = candidates
                .ordered
                .iter()
                .position(|node| node == &self.node)
                .ok_or(Error::Unauthorized)?;
            let mut transient = false;
            for (index, destination) in candidates.ordered.iter().enumerate().skip(rank + 1) {
                match self
                    .request(
                        &candidates.membership,
                        destination,
                        context,
                        operation,
                        FetchMode::CopyOnly,
                        scope,
                        budget,
                        (candidates.ordered.len() - index) as u32,
                    )
                    .await
                {
                    Ok(response) => {
                        match validated_copy(response, operation, false, &mut validate).await? {
                            Ok(copy) => return Ok(Some(copy)),
                            Err(
                                ProbeOutcome::Unreachable
                                | ProbeOutcome::Overloaded
                                | ProbeOutcome::UnusableCopy,
                            ) => transient = true,
                            Err(_) => {}
                        }
                    }
                    Err(Error::Unavailable | Error::Overloaded | Error::Io) => {
                        transient = true;
                        budget.note_route_failure();
                    }
                    Err(error) => return Err(error),
                }
            }
            check_budget(scope, budget)?;
            if transient {
                Err(Error::Unavailable)
            } else {
                Ok(None)
            }
        })
    }

    async fn refresh_candidates(
        &self,
        previous: &Candidates,
        object: &ObjectId,
        page: PageNumber,
        scope: &RequestScope,
        retried: bool,
    ) -> Result<Candidates> {
        if retried {
            return Err(Error::IncompatibleMembership);
        }
        let latest = self.published.current()?.membership.clone();
        if latest.version.0 <= previous.membership.version.0 {
            return Err(Error::IncompatibleMembership);
        }
        self.candidates_scoped(latest, object, page, scope).await
    }

    async fn request(
        &self,
        membership: &MembershipLease,
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
    async fn request_mode(
        &self,
        membership: &MembershipLease,
        destination: &NodeId,
        context: &OriginContext,
        operation: &PeerOperation,
        mode: FetchMode,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        remaining_opportunities: u32,
        request_mode: RequestMode,
    ) -> Result<VerifiedResponse> {
        check_budget(scope, budget)?;
        // Reserve the complete permitted route before sending. Lost responses cannot
        // refund an unknown number of forwarded links. No retry gets fresh credits.
        let links = budget.route_links();
        if links == 0 {
            return Err(Error::HopBudgetExhausted);
        }
        let now = uring_runtime::environment::now();
        let overall = budget.begin_peer_attempt(now, scope.deadline.0, links)?;
        // Share bounds idle fallback. A separate nonrenewable local cap bounds
        // every exchange, even when no alternative is affordable.
        // Sign the original hard ceiling before sending; it never renews.
        let deadline = now + (overall - now) / remaining_opportunities.max(1);
        let attempt_end = now
            + self
                .attempt_timeout
                .min(if matches!(request_mode, RequestMode::DirectHedge) {
                    (overall - now) / 3
                } else {
                    overall - now
                });
        // The local exchange may time out before the signed contract. Shortening
        // the latter per attempt would make a later update renew provider authority.
        let signed_deadline = overall;
        // A clone shares cancellation: timing it out would cancel the caller too.
        let mut attempt_scope = RequestScope::new(scope.request, overall)?;
        attempt_scope.set_candidate_idle((deadline - now).min(attempt_end - now))?;
        attempt_scope.set_candidate_total(attempt_end)?;
        attempt_scope.body_deadlines = Some((overall, deadline));
        let attempts = if matches!(mode, FetchMode::Acquire) {
            // Reserve remote acquisition credits from the same original call.
            // Without a signed response receipt unused remote credits stay spent.
            let credits = if matches!(request_mode, RequestMode::FundedRoute) {
                budget.remaining_attempts()
            } else {
                budget
                    .remaining_attempts()
                    .div_ceil(remaining_opportunities.max(1))
            };
            budget.partition(credits, 0)?.remaining_attempts()
        } else {
            0
        };
        let mut complete_by = attempt_end;
        if remaining_opportunities > 1
            && budget.remaining_attempts() > 0
            && budget.remaining_links() >= crate::topology::routing::FAILURE_LINKS
        {
            // Reserve at most half the original post-share interval for fallback,
            // including subscription/fixed-page fallback. The local cap may be tighter.
            let reserve = (deadline - now).min((overall - deadline) / 2);
            if !reserve.is_zero() {
                complete_by = complete_by.min(overall - reserve);
            }
        }
        // Observe actual known-length body progress, not checkout/head latency.
        // Even a last candidate should not occupy the whole budget if its measured
        // rate cannot finish in time. Healthy bodies may exceed the idle share.
        let observation = (deadline - now)
            .min((attempt_end - now) / 3)
            .max(std::time::Duration::from_nanos(1));
        attempt_scope.set_candidate_body_budget(observation, complete_by)?;
        let mut bytes = [0; 16];
        uring_runtime::environment::fill_random(&mut bytes).map_err(|_| Error::Unavailable)?;
        let attempt = AttemptId(bytes);
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
        let mut exchange = if matches!(request_mode, RequestMode::DirectHedge) {
            self.peers
                .request_direct(request, membership.clone(), &attempt_scope)
        } else {
            self.peers
                .request(request, membership.clone(), &attempt_scope)
        };
        let mut stopped = None;
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
            Ok(response) => {
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
        check_budget(scope, budget)?;
        match stopped.or_else(|| attempt_scope.check().err()) {
            Some(Error::DeadlineExceeded) => Err(Error::Unavailable),
            Some(error) => Err(error),
            None => match response {
                Err(Error::DeadlineExceeded) => Err(Error::Unavailable),
                result => result,
            },
        }
    }

    pub fn origin_miss_error(&self, authority: &OriginAuthority) -> Error {
        if authority.predecessor_evidence.iter().any(|outcome| {
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
}

fn check_budget(scope: &RequestScope, budget: &AcquisitionBudget) -> Result<()> {
    scope.check()?;
    if uring_runtime::environment::now() >= budget.deadline() {
        Err(Error::DeadlineExceeded)
    } else {
        Ok(())
    }
}

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
        Err(Error::CorruptRecord | Error::MissingKey)
            if matches!(
                operation,
                PeerOperation::Page { .. } | PeerOperation::Bootstrap { .. }
            ) =>
        {
            Ok(Err(ProbeOutcome::UnusableCopy))
        }
        Err(error) => Err(error),
    }
}
impl OriginAuthority {
    pub fn validate(&self, object: &ObjectId, page: PageNumber) -> Result<()> {
        if object != &self.object || page != self.page {
            return Err(Error::Unauthorized);
        }
        // Private construction follows a recomputed ranking under this immutable
        // membership. Validation need not rehash the entire cluster on each write.
        if self.rank >= 3
            || self.predecessor_evidence.len() != self.rank
            || self.membership.member(&self.node).is_err()
        {
            return Err(Error::Unauthorized);
        }
        Ok(())
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
    }
}
// `acquire` describes the actual probe, which may be CopyOnly even when the
// caller's operation requests acquisition (predecessors and remaining copies).
fn classify(
    response: &PeerResponse,
    operation: &PeerOperation,
    acquire: bool,
) -> Result<Option<ProbeOutcome>> {
    match response {
        PeerResponse::Selected {
            metadata,
            ciphertext,
            grant,
        } => match operation {
            PeerOperation::Subscribe { subscription, .. }
                if grant.page.version == subscription.version
                    && subscription.demand.contains(grant.page.number.0)
                    && ciphertext.envelope().page == grant.page
                    && metadata.version == subscription.version =>
            {
                metadata.immutable().validate_page(ciphertext.envelope())?;
                Ok(None)
            }
            _ => Err(Error::CorruptRecord),
        },
        PeerResponse::Miss => Ok(Some(ProbeOutcome::CopyMiss)),
        PeerResponse::NotFound => match operation {
            PeerOperation::Bootstrap { .. } if acquire => Err(Error::NotFound),
            PeerOperation::Metadata {
                selector: MetadataSelector::Fresh,
                ..
            } if acquire => Err(Error::NotFound),
            _ => Err(Error::Unauthorized),
        },
        PeerResponse::VersionUnavailable => Ok(Some(ProbeOutcome::VersionUnavailable)),
        PeerResponse::Unavailable => Ok(Some(ProbeOutcome::Unreachable)),
        PeerResponse::Overloaded => Ok(Some(ProbeOutcome::Overloaded)),
        PeerResponse::OriginRejected => Err(Error::OriginRejected),
        PeerResponse::OriginForbidden => Err(Error::OriginForbidden),
        PeerResponse::StaleMembership => Err(Error::IncompatibleMembership),
        PeerResponse::Bootstrap {
            metadata,
            page_zero,
        } => match operation {
            PeerOperation::Bootstrap { object, .. } if &metadata.version.object == object => {
                if let Some(page) = page_zero {
                    metadata.immutable().validate_page(page.envelope())?;
                    if page.envelope().page.number.0 != 0 {
                        return Err(Error::CorruptRecord);
                    }
                }
                if page_zero.is_some() != (metadata.length != 0) {
                    return Err(Error::CorruptRecord);
                }
                Ok(None)
            }
            _ => Err(Error::CorruptRecord),
        },
        PeerResponse::Page {
            metadata,
            ciphertext,
        } => match operation {
            PeerOperation::Page { page, .. }
                if &ciphertext.envelope().page == page && metadata.version == page.version =>
            {
                metadata.immutable().validate_page(ciphertext.envelope())?;
                Ok(None)
            }
            _ => Err(Error::CorruptRecord),
        },
        PeerResponse::Metadata(metadata) => match operation {
            PeerOperation::Metadata {
                object, selector, ..
            } if &metadata.version.object == object => {
                if let MetadataSelector::Pinned(etag) = selector {
                    if &metadata.version.etag != etag {
                        return Err(Error::CorruptRecord);
                    }
                }
                Ok(None)
            }
            _ => Err(Error::CorruptRecord),
        },
    }
}

#[cfg(test)]
mod timeout_tests;

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn candidate_failure_routes_spend_initial_allowance_with_four_then_eight_link_ceiling() {
        struct Routes(RefCell<Vec<(u8, u32)>>);
        impl PeerClient for Routes {
            fn direct_hedge_available(&self, _: &MembershipLease, _: &NodeId) -> bool {
                false
            }
            fn request_direct<'a>(
                &'a self,
                _: PeerRequest,
                _: MembershipLease,
                _: &'a RequestScope,
            ) -> Operation<'a, VerifiedResponse> {
                panic!("route-budget fixture does not admit direct hedges")
            }
            fn request<'a>(
                &'a self,
                request: PeerRequest,
                _: MembershipLease,
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
            peers.clone(),
            credentials,
            Arc::new(Default::default()),
        );
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let result = futures::executor::block_on(
            policy.resolve_with_budget(
                policy
                    .candidates(membership, &context.object, PageNumber(0))
                    .unwrap(),
                &context,
                PeerOperation::Metadata {
                    object: context.object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::Acquire,
                },
                &scope,
                &mut budget,
            ),
        );
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
    use crate::model::{CacheId, CacheKey, ExpiresAt, ObjectMetadata, ObjectVersion, StrongEtag};
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
    impl PeerClient for ProbePeer {
        fn direct_hedge_available(&self, _: &MembershipLease, _: &NodeId) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: MembershipLease,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("probe fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: MembershipLease,
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
    pub(super) fn fixture() -> (
        MembershipLease,
        Rc<Placement>,
        OriginContext,
        RequestScope,
        Rc<CredentialCrypto>,
    ) {
        use crate::{
            model::ClusterId,
            model::{MembershipVersion, RequestId},
            runtime::admission::AdmissionPolicy,
            security::identity::{KeyEpochs, Keyring},
            topology::membership::{Member, Membership},
        };
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
    #[test]
    fn only_candidates_mint_authority_after_ordered_copy_probes() {
        ordered_predecessor_probes(4);
    }
    fn ordered_predecessor_probes(attempts: u32) {
        let (membership, placement, context, scope, credentials) = fixture();
        let ordered = placement
            .rank(membership.clone(), &context.object, PageNumber(0))
            .unwrap()
            .ordered;
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = CandidatePolicy::new(
            ordered[2].clone(),
            placement,
            peer.clone(),
            credentials,
            Arc::new(Default::default()),
        );
        let candidates = policy
            .candidates(membership, &context.object, PageNumber(0))
            .unwrap();
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
            .unwrap()
            .ordered;
        let policy = CandidatePolicy::new(
            ordered[1].clone(),
            placement.clone(),
            peer.clone(),
            credentials.clone(),
            Arc::new(Default::default()),
        );
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 4, 8);
        let result = futures::executor::block_on(
            policy.resolve_with_budget(
                policy
                    .candidates(membership.clone(), &context.object, PageNumber(0))
                    .unwrap(),
                &context,
                PeerOperation::Metadata {
                    object: context.object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::Acquire,
                },
                &scope,
                &mut budget,
            ),
        );
        assert!(matches!(result, Err(Error::Unauthorized)));
        let peer = Rc::new(ProbePeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = CandidatePolicy::new(
            NodeId("outside".into()),
            placement,
            peer.clone(),
            credentials,
            Arc::new(Default::default()),
        );
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 16, 24);
        let result = futures::executor::block_on(
            policy.resolve_with_budget(
                policy
                    .candidates(membership, &context.object, PageNumber(0))
                    .unwrap(),
                &context,
                PeerOperation::Metadata {
                    object: context.object.clone(),
                    selector: MetadataSelector::Fresh,
                    mode: FetchMode::Acquire,
                },
                &scope,
                &mut budget,
            ),
        );
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
    impl PeerClient for RecordedPeer {
        fn direct_hedge_available(&self, _: &MembershipLease, _: &NodeId) -> bool {
            false
        }
        fn request_direct<'a>(
            &'a self,
            _: PeerRequest,
            _: MembershipLease,
            _: &'a RequestScope,
        ) -> Operation<'a, VerifiedResponse> {
            panic!("candidate-recording fixture does not admit direct hedges")
        }
        fn request<'a>(
            &'a self,
            request: PeerRequest,
            _: MembershipLease,
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
                };
                self.calls
                    .borrow_mut()
                    .push((request.route.destination, copy));
                Err(self.error)
            })
        }
    }
    fn membership() -> MembershipLease {
        use crate::{
            model::MembershipVersion,
            topology::membership::{Member, Membership},
        };
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
        use crate::security::identity::{KeyEpochs, Keyring};
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(flow_control::Quotas::new(
            crate::runtime::admission::AdmissionPolicy::new(config.limits),
        ));
        let keys = Rc::new(Keyring::new(
            config.cluster,
            node.clone(),
            std::sync::Arc::new(KeyEpochs::default()),
        ));
        let policy = CandidatePolicy::new(
            node,
            Rc::new(Placement::new(8)),
            peers,
            Rc::new(CredentialCrypto::new(keys, admission)),
            Arc::new(Default::default()),
        );
        policy
    }
    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::RequestId([7; 16]),
            Instant::now() + std::time::Duration::from_secs(60),
        )
        .unwrap()
    }
    #[test]
    fn backup_probes_only_predecessors_in_order_before_scoped_origin_authority() {
        ordered_predecessor_probes(3);
    }
    #[test]
    fn noncandidate_acquires_from_candidates_and_never_mints_origin_authority() {
        futures::executor::block_on(async {
            let membership = membership();
            let candidates = Placement::new(8)
                .rank(membership.clone(), &object(), PageNumber(0))
                .unwrap();
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
            let candidates = Placement::new(8)
                .rank(membership(), &object(), PageNumber(0))
                .unwrap();
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
        let ranked = Placement::new(8)
            .rank(membership.clone(), &object(), PageNumber(0))
            .unwrap();
        let peers = Rc::new(RecordedPeer {
            calls: RefCell::new(vec![]),
            error: Error::Unavailable,
        });
        let policy = policy(ranked.ordered[1].clone(), peers);
        let mut authority = OriginAuthority {
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
}
