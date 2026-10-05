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
#[cfg(test)]
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::task::Context;
use std::task::Poll;
use std::task::Waker;
use std::time::Duration;
use std::time::Instant;
use uring_runtime::deadline::Deadline;

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
    hedge: Option<Arc<Hedges>>,
    attempt_timeout: std::time::Duration,
    observer: Observer,
    pub(super) node: NodeId,
    placement: Rc<Placement>,
    peers: Rc<Requester>,
    credentials: Rc<CredentialCrypto>,
    published: Arc<crate::control::PublishedState>,
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
    pub(crate) fn hedge_owner(&self) -> Option<&Arc<Hedges>> {
        self.hedge.as_ref()
    }
    /// Only Fill's plaintext fixed-page path calls this. Direct HTTP destinations
    /// prove independent first hops. Drain both attempts before releasing escrow.
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
        scheduler: &super::range_stream::Scheduler,
        membership: std::sync::Arc<crate::topology::Membership>,
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
        peers: Rc<Requester>,
        credentials: Rc<CredentialCrypto>,
        published: Arc<crate::control::PublishedState>,
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
    pub(crate) fn with_hedges(mut self, hedges: Arc<Hedges>) -> Self {
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
        membership: std::sync::Arc<crate::topology::Membership>,
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
    pub fn maintain(&self, membership: &std::sync::Arc<crate::topology::Membership>) -> Result<()> {
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
                let next = self
                    .refresh_candidates(&candidates, object, page, scope, retried)
                    .await?;
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
                            bounded_routes: true,
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
                        trail.site = CandidateSite::Funding;
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
                    .request_traced(
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
                        Some((trail, index as u8)),
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
                                        ..Default::default()
                                    },
                                    trail,
                                )
                                .await;
                        }
                        let validated =
                            validated_copy(response, &operation, rank.is_none(), &mut validate)
                                .await;
                        note_validation(trail, &validated);
                        match validated? {
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
                    diagnostic: trail.clone(),
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
                if !matches!(trail.site, CandidateSite::Funding) {
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
                let mut transient = false;
                for (index, destination) in candidates.ordered.iter().enumerate().skip(rank + 1) {
                    match self
                        .request_traced(
                            &candidates.membership,
                            destination,
                            context,
                            operation,
                            FetchMode::CopyOnly,
                            scope,
                            budget,
                            (candidates.ordered.len() - index) as u32,
                            RequestMode::SharedRoute,
                            Some((&mut trail, index as u8)),
                        )
                        .await
                    {
                        Ok(response) => {
                            let validated =
                                validated_copy(response, operation, false, &mut validate).await;
                            note_validation(&mut trail, &validated);
                            match validated? {
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
        )
        .await
    }
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
    ) -> Result<VerifiedResponse> {
        let mut remote = [b'?'; 36];
        if trail.is_some() && racer_identity::canonical_uuid(&destination.0) {
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
            if trail.is_some() {
                facts.overall = crate::telemetry::timestamp(overall);
                facts.share = crate::telemetry::timestamp(deadline);
                facts.cap = crate::telemetry::timestamp(attempt_end);
            }
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
                && budget.remaining_links() >= crate::topology::FAILURE_LINKS
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
            let mut exchange = if matches!(request_mode, RequestMode::DirectHedge) {
                self.peers
                    .request_direct(request, membership.clone(), &attempt_scope)
            } else {
                self.peers
                    .request(request, membership.clone(), &attempt_scope)
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
                Ok(response) => match response.response() {
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
            let stop = stopped.or_else(|| attempt_scope.check().err());
            facts.stop = stop;
            match stop {
                Some(Error::DeadlineExceeded) => Err(Error::Unavailable),
                Some(error) => Err(error),
                None => match response {
                    Err(Error::DeadlineExceeded) => Err(Error::Unavailable),
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
struct Alarm {
    due: Instant,
    wake: Option<Waker>,
}
struct State {
    next: u64,
    alarms: BTreeMap<u64, Alarm>,
}
pub(crate) struct Hedges {
    config: HedgeConfig,
    state: Mutex<State>,
    metrics: Metrics,
}
pub(crate) struct Permit {
    owner: Arc<Hedges>,
    id: u64,
}
impl Hedges {
    pub(crate) fn new(config: HedgeConfig, metrics: Metrics) -> Result<Arc<Self>> {
        config.validate()?;
        Ok(Arc::new(Self {
            config,
            metrics,
            state: Mutex::new(State {
                next: 0,
                alarms: BTreeMap::new(),
            }),
        }))
    }
    pub(crate) fn enabled(&self) -> bool {
        self.config.slots > 0
    }
    pub(crate) fn suppressed(&self) {
        self.metrics.record(Event::PageHedgeSuppressed, 1);
    }
    pub(crate) fn acquire(self: &Arc<Self>) -> Result<Permit> {
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.alarms.len() >= self.config.slots
            || (state.alarms.len() + 1) * DUPLICATE_BYTES > self.config.bytes
        {
            return Err(Error::Overloaded);
        }
        state.next = state.next.checked_add(1).ok_or(Error::Unavailable)?;
        let id = state.next;
        state.alarms.insert(
            id,
            Alarm {
                due: uring_runtime::environment::now() + self.config.delay,
                wake: None,
            },
        );
        Ok(Permit {
            owner: self.clone(),
            id,
        })
    }
    pub(crate) fn poll(&self) {
        let now = uring_runtime::environment::now();
        let wakes: Vec<_> = self
            .state
            .lock()
            .map(|mut state| {
                state
                    .alarms
                    .values_mut()
                    .filter(|a| now >= a.due)
                    .filter_map(|a| a.wake.take())
                    .collect()
            })
            .unwrap_or_default();
        for wake in wakes {
            wake.wake();
        }
    }
}
impl Permit {
    pub(crate) fn delay(&self, cx: &mut Context<'_>) -> Poll<()> {
        let mut state = self.owner.state.lock().expect("hedge alarm lock");
        let alarm = state.alarms.get_mut(&self.id).expect("live hedge alarm");
        if uring_runtime::environment::now() >= alarm.due {
            Poll::Ready(())
        } else {
            alarm.wake = Some(cx.waker().clone());
            Poll::Pending
        }
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
impl Drop for Permit {
    fn drop(&mut self) {
        if let Ok(mut state) = self.owner.state.lock() {
            state.alarms.remove(&self.id);
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
    parent.check()?;
    let mut primary = Box::pin(primary);
    let mut secondary = Box::pin(secondary);
    let registration = parent.cancellation.subscribe()?;
    let mut a_done = false;
    let mut b_done = false;
    let mut launched = false;
    let mut winner = None;
    let mut fatal = None;
    std::future::poll_fn(|cx| {
        registration.register(cx.waker());
        if let Err(error) = parent.check() {
            fatal = Some(error);
        }
        if fatal.is_some() || winner.is_some() {
            if !a_done {
                let _ = primary_scope.cancel();
            }
            if !b_done {
                let _ = secondary_scope.cancel();
            }
        }
        if !a_done {
            if let Poll::Ready(result) = primary.as_mut().poll(cx) {
                a_done = true;
                match result {
                    Ok(value) if winner.is_none() && fatal.is_none() => winner = Some(value),
                    Err(error) if !recoverable(error) && winner.is_none() && fatal.is_none() => {
                        fatal = Some(error)
                    }
                    _ => {}
                }
                if !launched {
                    b_done = true;
                }
            }
        }
        if !launched
            && !b_done
            && fatal.is_none()
            && winner.is_none()
            && permit.delay(cx).is_ready()
        {
            launched = true;
        }
        if launched && !b_done {
            if fatal.is_some() || winner.is_some() {
                let _ = secondary_scope.cancel();
            }
            if let Poll::Ready(result) = secondary.as_mut().poll(cx) {
                b_done = true;
                match result {
                    Ok(value) if winner.is_none() && fatal.is_none() => {
                        permit.won();
                        winner = Some(value);
                    }
                    Err(error) if !recoverable(error) && winner.is_none() && fatal.is_none() => {
                        fatal = Some(error)
                    }
                    _ => {}
                }
            }
        }
        if winner.is_some() || fatal.is_some() {
            if !a_done {
                let _ = primary_scope.cancel();
            }
            if !b_done {
                let _ = secondary_scope.cancel();
            }
            if !launched {
                b_done = true;
            }
        }
        if a_done && b_done {
            Poll::Ready(if let Some(error) = fatal {
                Err(error)
            } else {
                winner.take().ok_or(Error::Unavailable)
            })
        } else {
            Poll::Pending
        }
    })
    .await
}
fn recoverable(error: Error) -> bool {
    matches!(
        error,
        Error::Unavailable | Error::Io | Error::Overloaded | Error::CorruptRecord | Error::MissingKey | Error::Cancelled
    // Malformed framing is local to that contender, not authority to
    // cancel another independently authenticated usable page.
    | Error::InvalidRequest | Error::HeaderTooLarge
    )
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
        use racer_identity::KeyEpochs;
        use racer_identity::Keyring;
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
            Requester::scripted(
                peer.clone(),
                ProbePeer::direct_hedge_available,
                ProbePeer::request,
                ProbePeer::request_direct,
            ),
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
            Requester::scripted(
                peer.clone(),
                ProbePeer::direct_hedge_available,
                ProbePeer::request,
                ProbePeer::request_direct,
            ),
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
            Requester::scripted(
                peer.clone(),
                ProbePeer::direct_hedge_available,
                ProbePeer::request,
                ProbePeer::request_direct,
            ),
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
        use racer_identity::KeyEpochs;
        use racer_identity::Keyring;
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(flow_control::Quotas::new(
            crate::admission::AdmissionPolicy::new(config.limits),
        ));
        let keys = Rc::new(Keyring::new(
            config.cluster,
            node.clone(),
            std::sync::Arc::new(KeyEpochs::default()),
        ));
        let policy = CandidatePolicy::new(
            node,
            Rc::new(Placement::new(8)),
            Requester::scripted(
                peers,
                RecordedPeer::direct_hedge_available,
                RecordedPeer::request,
                RecordedPeer::request_direct,
            ),
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
}
