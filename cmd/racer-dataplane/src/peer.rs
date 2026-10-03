//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
pub mod adaptive;
pub mod protocol;
pub mod server;
pub mod subscriptions;
#[cfg(test)]
pub(crate) mod tests;
mod timing {
    use super::protocol::{Operation, PeerRequest, PeerResponse, VerifiedResponse};
    use crate::{
        telemetry::metrics::{Event, Metrics},
        topology::rails::TransportPlan,
    };
    use std::time::Instant;
    use uring_runtime::environment::now;

    pub(super) const STAGES: [(Event, Event); 4] = [
        (Event::PeerPageCheckoutCount, Event::PeerPageCheckoutNs),
        (Event::PeerPageAuthCount, Event::PeerPageAuthNs),
        (Event::PeerPageHeadCount, Event::PeerPageHeadNs),
        (Event::PeerPageBodyCount, Event::PeerPageBodyNs),
    ];

    /// Publish all HTTP Page stages only after verification; censor other outcomes.
    pub(super) struct PageTiming<'a> {
        metrics: &'a Metrics,
        active: bool,
        start: Option<Instant>,
        durations: [u64; 4],
        completed: u8,
    }
    impl<'a> PageTiming<'a> {
        pub(super) fn new(metrics: &'a Metrics) -> Self {
            Self {
                metrics,
                active: false,
                start: None,
                durations: [0; 4],
                completed: 0,
            }
        }
        pub(super) fn enable(&mut self, request: &PeerRequest, plan: TransportPlan, opaque: bool) {
            self.active = !opaque
                && matches!(plan, TransportPlan::Http)
                && request.route.visited.len() == 1
                && matches!(request.operation, Operation::Page { .. });
            self.begin();
        }
        pub(super) fn begin(&mut self) {
            if self.active {
                self.start = Some(now());
            }
        }
        pub(super) fn end(&mut self, stage: usize) {
            if let Some(start) = self.start.take() {
                self.durations[stage] = now()
                    .saturating_duration_since(start)
                    .as_nanos()
                    .min(u64::MAX as u128) as u64;
                self.completed |= 1 << stage;
            }
        }
        pub(super) fn success(&mut self, response: &VerifiedResponse) {
            if self.active
                && self.completed == 15
                && matches!(response.response(), PeerResponse::Page { .. })
            {
                // Concurrent scrapes are not atomic snapshots.
                for ((count, sum), duration) in STAGES.into_iter().zip(self.durations) {
                    let _ = self.metrics.record(sum, duration);
                    let _ = self.metrics.record(count, 1);
                }
                self.active = false;
            }
        }
    }
    impl Drop for PageTiming<'_> {
        fn drop(&mut self) {
            if self.active {
                let _ = self.metrics.record(Event::PeerPageCensored, 1);
            }
        }
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        #[test]
        fn page_timing_duration_conversion_saturates_and_reversed_clock_is_zero() {
            use std::time::Duration;
            use uring_runtime::environment::SimulationClock;
            let metrics = Metrics::default();
            let clock = SimulationClock::new(9);
            let _environment = clock.environment(1).enter();
            let mut timing = PageTiming::new(&metrics);
            timing.active = true;
            timing.begin();
            clock.advance(Duration::from_secs(u64::MAX / 1_000_000_000 + 1));
            timing.end(0);
            assert_eq!(timing.durations[0], u64::MAX);
            timing.start = Some(now() + Duration::from_secs(1));
            timing.end(1);
            assert_eq!(timing.durations[1], 0);
            drop(timing);
            assert_eq!(metrics.count(Event::PeerPageCensored), 1);
            assert_eq!(metrics.count(Event::PeerPageCheckoutNs), 0);
        }
    }
}
pub mod transport;

use self::{
    protocol::{PeerRequest, SignedRequest, SignedResponse, VerifiedRequest, VerifiedResponse},
    transport::Transfers,
};
use crate::telemetry::failures::{Observer, Stage};
use crate::{
    error::{Error, Operation, Result},
    model::{MembershipVersion, NodeId, ResourceClass},
    runtime::admission::AdmissionPolicy,
    runtime::deadline::RequestScope,
    security::forwarding::Forwarding,
    topology::{membership::MembershipLease, rails, routing::Paths},
};
use std::{rc::Rc, sync::Arc};

/// Worker-local identity and a handle to the sole node-wide incoming registry.
/// Outbound operations route directly from their retained membership lease.
pub struct PeerNetwork {
    pub local: NodeId,
    published: Arc<crate::control::state::PublishedState>,
}

impl PeerNetwork {
    pub fn new(
        local: NodeId,
        published: Arc<crate::control::state::PublishedState>,
    ) -> Result<Self> {
        if local.0.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { local, published })
    }

    pub fn membership(&self, version: MembershipVersion) -> Result<MembershipLease> {
        self.published.membership(version)
    }

    pub fn endpoint(
        &self,
        membership: &MembershipLease,
        node: &NodeId,
    ) -> Result<crate::http::Endpoint> {
        if !membership.neighbors(&self.local)?.contains(node) {
            return Err(Error::InvalidRequest);
        }
        let member = membership.member(node)?;
        Ok(crate::http::Endpoint::Peer(member.peer_endpoint.clone()))
    }
}

/// Narrow a caller's scope to the signed route without creating a new cancellation
/// domain or extending the original deadline.
pub(crate) fn request_scope(
    request: &protocol::PeerRequest,
    scope: &RequestScope,
) -> Result<RequestScope> {
    scope.check()?;
    if request.route.request != scope.request
        || request.origin.request != scope.request
        || request.route.attempt != request.origin.attempt
    {
        return Err(Error::InvalidRequest);
    }
    let mut narrowed = scope.clone();
    narrowed.deadline.0 = narrowed
        .deadline
        .0
        .min(request.route.deadline.0)
        .min(request.origin.scope().deadline.0);
    narrowed.check()?;
    Ok(narrowed)
}

pub(crate) fn check_membership(
    request: &protocol::PeerRequest,
    membership: &MembershipLease,
) -> Result<()> {
    if request.route.membership != membership.version {
        return Err(Error::IncompatibleMembership);
    }
    Ok(())
}

pub(crate) fn search_budget(
    route: &crate::topology::routing::RouteBudget,
    local: &NodeId,
) -> Result<crate::topology::routing::RouteBudget> {
    let mut budget = route.clone();
    if budget.visited.last() == Some(local) {
        budget.visited.pop();
    } else {
        budget.remaining_links = budget
            .remaining_links
            .checked_sub(1)
            .ok_or(Error::HopBudgetExhausted)?;
    }
    Ok(budget)
}
/// Opaque bounded transit with recorded reverse-path responses, never a page cache.
/// No decryption service is injected. Reverse-link failure terminates the attempt;
/// responses are not independently rerouted. Preserve encrypted credentials.
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transport: Rc<Requester>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    network: Rc<PeerNetwork>,
}

impl Relay {
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transport: Rc<Requester>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        network: Rc<PeerNetwork>,
    ) -> Self {
        Self {
            paths,
            forwarding,
            transport,
            admission,
            network,
        }
    }

    /// Retain ingress binding and reverse path, append a signed request hop, and
    /// exchange complete envelopes. Verify the downstream response against that
    /// binding before appending a reverse hop. Never re-sign the original response.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{peer::{Relay, protocol::SignedRequest},
    ///     runtime::deadline::RequestScope, topology::membership::MembershipLease};
    /// fn unverified(relay: &Relay, request: SignedRequest,
    ///     membership: MembershipLease, scope: &RequestScope) {
    ///     relay.forward(request, membership, scope);
    /// }
    /// ```
    pub fn forward<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            match self.forward_inner(request, membership, None, scope).await? {
                transport::RelayResponse::Complete(response) => Ok(response),
                _ => Err(Error::Internal),
            }
        })
    }

    pub(crate) fn forward_inner<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: MembershipLease,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        Box::pin(async move {
            let scope = request_scope(request.request(), scope)?;
            check_membership(request.request(), &membership)?;
            let network = &self.network;
            let budget = &request.request().route;
            if budget.destination == network.local || budget.visited.contains(&network.local) {
                return Err(Error::InvalidRequest);
            }
            let reservation = match relay.as_ref() {
                Some(reservation) => reservation.clone(),
                None => Rc::new(self.admission.reserve(None, ResourceClass::Relay, 1)?),
            };
            let search_budget = search_budget(budget, &network.local)?;
            let route = self
                .paths
                .shortest_async(membership.clone(), &network.local, &search_budget, &scope)
                .await?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            let previous = request
                .forwarders()
                .last()
                .unwrap_or(request.origin())
                .node()
                .clone();
            let binding = request.binding().clone();
            let mut visited = budget.visited.clone();
            visited.push(network.local.clone());
            let remaining_links = budget
                .remaining_links
                .checked_sub(1)
                .ok_or(Error::HopBudgetExhausted)?;
            let outbound_budget = crate::topology::routing::RouteBudget {
                membership: budget.membership,
                request: budget.request,
                attempt: budget.attempt,
                destination: budget.destination.clone(),
                visited,
                remaining_links,
                remaining_attempts: budget.remaining_attempts,
                deadline: budget.deadline,
            };
            let outbound = self
                .forwarding
                .append_request(request, next, outbound_budget)?;
            let response = if relay.is_some() {
                self.transport
                    .exchange_relay(outbound, membership, reservation, &scope)
                    .await?
            } else {
                transport::RelayResponse::Complete(
                    self.transport
                        .exchange(outbound, membership, &scope)
                        .await?,
                )
            };
            scope.check()?;
            match response {
                transport::RelayResponse::Complete(response) => {
                    let response = self.forwarding.verify_response(response, &binding)?;
                    self.forwarding
                        .append_response(response, &previous)
                        .map(transport::RelayResponse::Complete)
                }
                transport::RelayResponse::Http {
                    authentication,
                    connection,
                    length,
                } => {
                    let authentication = self.forwarding.forward_opaque(
                        authentication,
                        length,
                        &binding,
                        &previous,
                    )?;
                    Ok(transport::RelayResponse::Http {
                        authentication,
                        connection,
                        length,
                    })
                }
            }
        })
    }
}

/// Candidate-policy boundary with one shipping implementation, [`Requester`].
/// Precise-fence fixtures hold completion after cancellation to prove fallback
/// cannot reuse accepted work early; real socket timing cannot prescribe that
/// boundary. Ordinary transport behavior is covered by signed socket exchanges.
/// Implementations must explicitly declare and implement direct hedge behavior.
pub trait PeerClient {
    /// Conservative hedge capability: the destination must itself be the first hop.
    fn direct_hedge_available(
        &self,
        membership: &MembershipLease,
        destination: &crate::model::NodeId,
    ) -> bool;
    fn request_direct<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse>;
    /// Carry the originating operation's lease through routing and completion.
    /// Cancellation does not release accepted transport work before its fence.
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse>;
}
/// Owned signed-envelope exchange shared by requesters and opaque relays.
/// Receiving a signed response does not authenticate it: callers must verify it
/// against their retained request binding before use or reverse forwarding.
/// Requesters handle both complete and opaque I/O.
///
/// ```no_run
/// use racer_dataplane::{error::Result, peer::{PeerClient, Requester,
///     Relay, server::PeerServer,
///     protocol::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse}},
///     runtime::deadline::RequestScope, security::forwarding::Forwarding,
///     topology::membership::MembershipLease};
/// async fn interfaces(
///     client: &dyn PeerClient, transport: &Requester,
///     server: &PeerServer, relay: &Relay, auth: &Forwarding,
///     local: PeerRequest, wire: SignedRequest, outbound: SignedRequest,
///     inbound: SignedRequest, membership: MembershipLease, scope: &RequestScope,
/// ) -> Result<()> {
///     let verified: VerifiedResponse = client.request(local, membership.clone(), scope).await?;
///     let _wire_response: SignedResponse = verified.into_signed();
///     let admitted = auth.verify_request(wire)?;
///     let reply = relay.forward(admitted, membership.clone(), scope).await?;
///     let _retained_chain = reply.authentication;
///     // Both network boundaries preserve envelopes in each direction.
///     let _exchange: SignedResponse = transport.exchange(outbound, membership, scope).await?;
///     let _dispatch: SignedResponse = server.dispatch(inbound, scope).await?;
///     Ok(())
/// }
/// ```
pub struct Requester {
    #[cfg(test)]
    outbound_requests: std::cell::Cell<usize>,
    metrics: crate::telemetry::metrics::Metrics,
    observer: Observer,
    health: Rc<crate::topology::health::LinkHealth>,
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transfers: Rc<Transfers>,
    network: Rc<PeerNetwork>,
}
impl Requester {
    #[cfg(test)]
    pub(crate) fn admission(&self) -> &std::sync::Arc<adaptive::AdaptivePeers> {
        self.paths.peer_admission.as_ref().unwrap()
    }
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transfers: Rc<Transfers>,
        network: Rc<PeerNetwork>,
    ) -> Self {
        Self {
            #[cfg(test)]
            outbound_requests: std::cell::Cell::new(0),
            metrics: crate::telemetry::metrics::Metrics::default(),
            observer: Observer::default(),
            health: paths.link_health(),
            paths,
            forwarding,
            transfers,
            network,
        }
    }
    pub(crate) fn with_observer(mut self, observer: Observer) -> Self {
        self.observer = observer;
        self
    }
    pub(crate) fn with_metrics(mut self, metrics: crate::telemetry::metrics::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
}
impl PeerClient for Requester {
    fn direct_hedge_available(
        &self,
        membership: &MembershipLease,
        destination: &crate::model::NodeId,
    ) -> bool {
        self.network.endpoint(membership, destination).is_ok()
            && self.health.available(destination).unwrap_or(false)
            && self
                .paths
                .peer_admission
                .as_ref()
                .is_some_and(|a| a.hedge_available(destination))
    }
    fn request_direct<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            if !matches!(request.operation, protocol::Operation::Page { .. }) {
                return Err(Error::InvalidRequest);
            }
            let scope = request_scope(&request, scope)?;
            let next = request.route.destination.clone();
            if !self.direct_hedge_available(&membership, &next) {
                return Err(Error::Overloaded);
            }
            let (signed, binding) = self.forwarding.sign_request_to(request, &next)?;
            let mut timing = timing::PageTiming::new(&self.metrics);
            let response = self
                .exchange_inner_mode(signed, membership, None, &scope, true, Some(&mut timing))
                .await?;
            let transport::RelayResponse::Complete(response) = response else {
                return Err(Error::Internal);
            };
            scope.check()?;
            let response = self.forwarding.verify_response(response, &binding)?;
            timing.success(&response);
            Ok(response)
        })
    }
    /// Sign a fresh attempt, exchange the full envelope, then verify the response
    /// using the binding retained from signing. Logical callers retain the proof.
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let scope = request_scope(&request, scope)?;
            check_membership(&request, &membership)?;
            let network = &self.network;
            let search_budget = search_budget(&request.route, &network.local)?;
            let route = self.observer.result(
                Stage::PeerRoute,
                &scope,
                self.paths
                    .shortest_async(membership.clone(), &network.local, &search_budget, &scope)
                    .await,
            )?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            let (signed, binding) = self.forwarding.sign_request_to(request, next)?;
            let mut timing = timing::PageTiming::new(&self.metrics);
            let response = self
                .exchange_inner_mode(signed, membership, None, &scope, false, Some(&mut timing))
                .await?;
            let transport::RelayResponse::Complete(response) = response else {
                return Err(Error::Internal);
            };
            scope.check()?;
            let response = self.observer.result(
                Stage::PeerVerify,
                &scope,
                self.forwarding.verify_response(response, &binding),
            )?;
            timing.success(&response);
            Ok(response)
        })
    }
}
impl Requester {
    pub fn exchange_relay<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        reservation: Rc<flow_control::Charge<AdmissionPolicy>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner(request, membership, Some(reservation), scope)
    }
    /// Use this exact lease for the signed route; never resolve its version again.
    pub fn exchange<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            match self
                .exchange_inner(request, membership, None, scope)
                .await?
            {
                transport::RelayResponse::Complete(response) => Ok(response),
                _ => Err(Error::Internal),
            }
        })
    }
}
impl Requester {
    fn exchange_inner<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner_mode(request, membership, relay, scope, false, None)
    }
    fn exchange_inner_mode<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        relay: Option<Rc<flow_control::Charge<AdmissionPolicy>>>,
        scope: &'a RequestScope,
        direct_http: bool,
        timing: Option<&'a mut timing::PageTiming<'_>>,
    ) -> Operation<'a, transport::RelayResponse> {
        Box::pin(async move {
            #[cfg(test)]
            self.outbound_requests.set(self.outbound_requests.get() + 1);
            let scope = request_scope(&request.request, scope)?;
            check_membership(&request.request, &membership)?;
            let network = &self.network;
            let budget = &request.request.route;
            let signed_head = request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original);
            let next = crate::security::connection::receiver(&signed_head.head)?;
            // A signature selects the next receiver. Never reroute this envelope
            // independently after signing, even if link health changes.
            let endpoint = network.endpoint(&membership, &next)?;
            // The hint chooses a candidate rail, never the provider's page order.
            // The sender validates the actual selected page against that rail and
            // falls back to HTTP before exporting a window when they disagree.
            let rail_hint = match &request.request.operation {
                protocol::Operation::Page { page, .. } => Some(page.clone()),
                protocol::Operation::Subscribe { subscription, .. } => subscription
                    .demand
                    .intervals()
                    .first()
                    .map(|interval| crate::model::PageId {
                        version: subscription.version.clone(),
                        number: crate::model::PageNumber(interval.start),
                    }),
                _ => None,
            };
            let plan = if direct_http {
                if next != budget.destination {
                    return Err(Error::InvalidRequest);
                }
                crate::topology::rails::TransportPlan::Http
            } else if let Some(page) = rail_hint {
                let search = search_budget(budget, &network.local)?;
                let route = self
                    .paths
                    .shortest_async(membership.clone(), &network.local, &search, &scope)
                    .await?;
                if route.nodes.get(1) != Some(&next) {
                    return Err(Error::Unavailable);
                }
                rails::select_hop(&route, &page, &network.local, &next)?
            } else {
                crate::topology::rails::TransportPlan::Http
            };
            let _probe = self.health.acquire(&next)?;
            let permit = self
                .paths
                .peer_admission
                .as_ref()
                .map(|a| a.acquire(&next))
                .transpose()?;
            let binding = self.forwarding.outbound_binding(&request)?;
            let socket_failure = Rc::new(std::cell::Cell::new(false));
            let response = self
                .transfers
                .exchange_timed(
                    endpoint,
                    request,
                    plan,
                    Some(membership.clone()),
                    relay,
                    permit.clone(),
                    socket_failure.clone(),
                    timing,
                    &scope,
                )
                .await;
            // Receiving a signed envelope is not proof. Recover only after the
            // complete reverse chain and original request binding are verified.
            let response = response.and_then(|response| self.verify_exchange(response, &binding));
            use crate::topology::health::LinkOutcome;
            let outcome = match &response {
                Ok(transport::RelayResponse::Complete(_)) => Some(LinkOutcome::Success),
                Err(_) if permit.is_none() && socket_failure.get() => Some(LinkOutcome::Refused),
                // Unavailable/deadline/protocol errors can arise locally or at a
                // downstream node. Do not blame an immediate peer without evidence.
                _ => None,
            };
            if let Some(outcome) = outcome {
                self.health.observe(&next, outcome)?;
            }
            if let Some(permit) = permit {
                permit.observe(match &response {
                    // A downstream overload is not attributable to the immediate
                    // peer. It also must not increase its admission or recover a probe.
                    Ok(transport::RelayResponse::Complete(response))
                        if matches!(response.response, protocol::PeerResponse::Overloaded) =>
                    {
                        adaptive::Outcome::Neutral
                    }
                    Ok(transport::RelayResponse::Complete(_)) => adaptive::Outcome::Verified,
                    Err(Error::Overloaded) => adaptive::Outcome::LocalPressure,
                    _ => adaptive::Outcome::Neutral,
                });
            }
            drop(membership);
            response
        })
    }

    fn verify_exchange(
        &self,
        response: transport::RelayResponse,
        binding: &crate::security::forwarding::RequestBinding,
    ) -> Result<transport::RelayResponse> {
        match response {
            transport::RelayResponse::Complete(response) => self
                .forwarding
                .verify_response(response, binding)
                .map(|verified| transport::RelayResponse::Complete(verified.into_signed())),
            transport::RelayResponse::Http {
                authentication,
                mut connection,
                length,
            } => {
                self.forwarding
                    .verify_opaque(&authentication, length, binding)?;
                // All Racer outcomes use HTTP 200. Inspect the authenticated
                // outcome, exactly as the materialized path does, not HTTP status.
                connection.state_mut().peer_response_verified =
                    crate::peer::protocol::field(&authentication.original.head, "racer-outcome")?
                        != "overloaded";
                Ok(transport::RelayResponse::Http {
                    authentication,
                    connection,
                    length,
                })
            }
        }
    }
}
