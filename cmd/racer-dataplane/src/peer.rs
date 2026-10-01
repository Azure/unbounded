//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
pub mod adaptive;
mod native;
pub mod protocol;
pub mod server;
pub mod subscriptions;
#[cfg(test)]
mod tests;
pub mod transport;

use self::{
    protocol::{PeerRequest, SignedRequest, SignedResponse, VerifiedRequest, VerifiedResponse},
    transport::Transfers,
};
use crate::telemetry::failures::{Observer, Stage};
use crate::{
    error::{Error, Operation, Result},
    model::{MembershipVersion, NodeId, ResourceClass},
    runtime::admission::Admission,
    runtime::deadline::RequestScope,
    security::forwarding::Forwarding,
    topology::{membership::MembershipLease, paths::Paths, rails},
};
use std::{rc::Rc, sync::Arc};

/// Worker-local identity and a handle to the sole node-wide incoming registry.
/// Outbound operations route directly from their retained membership lease.
pub struct PeerNetwork {
    pub local: NodeId,
    published: Arc<crate::control::snapshot::PublishedState>,
    algorithm: crate::topology::RoutingAlgorithm,
}

impl PeerNetwork {
    pub fn new(
        local: NodeId,
        published: Arc<crate::control::snapshot::PublishedState>,
    ) -> Result<Self> {
        Self::with_algorithm(
            local,
            published,
            crate::topology::RoutingAlgorithm::default(),
        )
    }

    pub fn with_algorithm(
        local: NodeId,
        published: Arc<crate::control::snapshot::PublishedState>,
        algorithm: crate::topology::RoutingAlgorithm,
    ) -> Result<Self> {
        if local.0.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            local,
            published,
            algorithm,
        })
    }

    pub fn membership(&self, version: MembershipVersion) -> Result<MembershipLease> {
        self.published.membership(version)
    }

    pub fn endpoint(
        &self,
        membership: &MembershipLease,
        node: &NodeId,
    ) -> Result<crate::http::pool::Endpoint> {
        if !crate::topology::Graph::with_algorithm(membership.clone(), self.algorithm)
            .neighbors(&self.local)?
            .contains(node)
        {
            return Err(Error::InvalidRequest);
        }
        let member = membership.member(node)?;
        Ok(crate::http::pool::Endpoint::Peer(
            member.peer_endpoint.clone(),
        ))
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
    route: &crate::topology::paths::RouteBudget,
    local: &NodeId,
) -> Result<crate::topology::paths::RouteBudget> {
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
    transport: Rc<dyn PeerTransport>,
    admission: Rc<Admission>,
    network: Rc<PeerNetwork>,
}

impl Relay {
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transport: Rc<dyn PeerTransport>,
        admission: Rc<Admission>,
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
        relay: Option<Rc<crate::runtime::admission::Reservation>>,
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
            let outbound_budget = crate::topology::paths::RouteBudget {
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

pub trait PeerClient {
    /// Conservative hedge capability: the destination must itself be the first hop.
    fn direct_hedge_available(
        &self,
        _membership: &MembershipLease,
        _destination: &crate::model::NodeId,
    ) -> bool {
        false
    }
    fn request_direct<'a>(
        &'a self,
        _request: PeerRequest,
        _membership: MembershipLease,
        _scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async { Err(Error::Unavailable) })
    }
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
///
/// ```no_run
/// use racer_dataplane::{error::Result, peer::{PeerClient, PeerTransport,
///     Relay, server::PeerServer,
///     protocol::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse}},
///     runtime::deadline::RequestScope, security::forwarding::Forwarding,
///     topology::membership::MembershipLease};
/// async fn interfaces(
///     client: &dyn PeerClient, transport: &dyn PeerTransport,
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
pub trait PeerTransport {
    fn exchange_relay<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        reservation: Rc<crate::runtime::admission::Reservation>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        Box::pin(async move {
            let response = self.exchange(request, membership, scope).await;
            drop(reservation);
            response.map(transport::RelayResponse::Complete)
        })
    }
    /// Use this exact lease for the signed route; never resolve its version again.
    fn exchange<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse>;
}
pub struct Requester {
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
}
impl PeerClient for Requester {
    fn direct_hedge_available(
        &self,
        membership: &MembershipLease,
        destination: &crate::model::NodeId,
    ) -> bool {
        self.network.endpoint(membership, destination).is_ok()
            && self.health.available(destination).unwrap_or(false)
            && self.paths.peer_admission.as_ref().is_some_and(|a| a.hedge_available(destination))
    }
    fn request_direct<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            if !matches!(request.operation, wire::Operation::Page { .. }) {
                return Err(Error::InvalidRequest);
            }
            let scope = request_scope(&request, scope)?;
            let next = request.route.destination.clone();
            if !self.direct_hedge_available(&membership, &next) {
                return Err(Error::Overloaded);
            }
            let (signed, binding) = self.forwarding.sign_request_to(request, &next)?;
            let response = self.exchange_inner_mode(signed, membership, None, &scope, true).await?;
            let transfer::RelayResponse::Complete(response) = response else {
                return Err(Error::Internal);
            };
            scope.check()?;
            self.forwarding.verify_response(response, &binding)
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
            let response = self.exchange(signed, membership, &scope).await?;
            scope.check()?;
            self.observer.result(
                Stage::PeerVerify,
                &scope,
                self.forwarding.verify_response(response, &binding),
            )
        })
    }
}
impl PeerTransport for Requester {
    fn exchange_relay<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        reservation: Rc<crate::runtime::admission::Reservation>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner(request, membership, Some(reservation), scope)
    }
    fn exchange<'a>(
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
        relay: Option<Rc<crate::runtime::admission::Reservation>>,
        scope: &'a RequestScope,
    ) -> Operation<'a, transport::RelayResponse> {
        self.exchange_inner_mode(request, membership, relay, scope, false)
    }
    fn exchange_inner_mode<'a>(
        &'a self,
        request: SignedRequest,
        membership: MembershipLease,
        relay: Option<Rc<crate::runtime::admission::Reservation>>,
        scope: &'a RequestScope,
        direct_http: bool,
    ) -> Operation<'a, transport::RelayResponse> {
        Box::pin(async move {
            let scope = request_scope(&request.request, scope)?;
            check_membership(&request.request, &membership)?;
            let network = &self.network;
            let budget = &request.request.route;
            let signed_head = request
                .authentication
                .hops
                .last()
                .unwrap_or(&request.authentication.original);
            let next = crate::security::signing::receiver(&signed_head.head)?;
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
                rails::select(&route, &page)?
            } else {
                crate::topology::rails::TransportPlan::Http
            };
            let _probe = self.health.acquire(&next)?;
            let permit = self.paths.peer_admission.as_ref().map(|a| a.acquire(&next)).transpose()?;
            let binding = self.forwarding.outbound_binding(&request)?;
            let socket_failure = Rc::new(std::cell::Cell::new(false));
            let response = self
                .transfers
                .exchange_inner(endpoint, request, plan, relay, permit.clone(), socket_failure.clone(), &scope)
                .await;
            // Receiving a signed envelope is not proof. Recover only after the
            // complete reverse chain and original request binding are verified.
            let response = response.and_then(|response| self.verify_exchange(response, &binding));
            use crate::topology::health::LinkOutcome;
            let outcome = match &response {
                Ok(transfer::RelayResponse::Complete(_)) => Some(LinkOutcome::Success),
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
                    Ok(transfer::RelayResponse::Complete(response))
                        if matches!(response.response, wire::PeerResponse::Overloaded) => adaptive::Outcome::Neutral,
                    Ok(transfer::RelayResponse::Complete(_)) => adaptive::Outcome::Verified,
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
        response: transfer::RelayResponse,
        binding: &crate::security::forwarding::RequestBinding,
    ) -> Result<transfer::RelayResponse> {
        match response {
            transfer::RelayResponse::Complete(response) => self.forwarding.verify_response(response, binding)
                .map(|verified| transfer::RelayResponse::Complete(verified.into_signed())),
            transfer::RelayResponse::Http { authentication, mut connection, length } => {
                self.forwarding.verify_opaque(&authentication, length, binding)?;
                // All Racer outcomes use HTTP 200. Inspect the authenticated
                // outcome, exactly as the materialized path does, not HTTP status.
                connection.peer_response_verified = crate::security::protocol::field(
                    &authentication.original.head, "racer-outcome",
                )? != "overloaded";
                Ok(transfer::RelayResponse::Http { authentication, connection, length })
            }
        }
    }
}

#[cfg(test)]
#[path = "peer/requester_safety_tests.rs"]
mod safety_tests;

#[cfg(test)]
mod requester_tests {
    use super::*;
    #[test]
    fn invalid_response_never_recovers_half_open_peer() {
        use crate::{model::NodeId, peer::wire::PeerResponse, telemetry::metrics::{Event, Metrics}, topology::health::LinkHealth};
        let clock = crate::runtime::environment::SimulationClock::new(782);
        let _env = clock.environment(0).enter();
        let signers = crate::peer::tests::signers();
        let admission = Rc::new(crate::runtime::admission::Admission::new(crate::test_support::cluster::config(false).limits));
        let reactor = Rc::new(crate::runtime::reactor::Reactor::new(admission.clone()));
        let io = Rc::new(crate::http::io::HttpIo::with_admission(reactor.clone(), crate::http::codec::Codec::new(crate::peer::wire::MAX_ENVELOPE_HEAD, 0), admission.clone()));
        let pool = Rc::new(crate::http::pool::HttpPool::new(reactor, admission.clone(), 2));
        let forwarding = Rc::new(Forwarding::new(signers[0].clone()));
        let requester = Requester::new(Rc::new(Paths::new(Rc::new(LinkHealth), 4)), forwarding.clone(), Rc::new(Transfers::new(pool, io, None)), Rc::new(PeerNetwork::new(signers[0].node().clone(), Default::default()).unwrap()));
        let metrics = Metrics::default();
        let adaptive = adaptive::AdaptivePeers::new(Default::default(), metrics.clone()).unwrap();
        let node: NodeId = signers[2].node().clone();
        let permit = adaptive.acquire(&node).unwrap();
        permit.observe(adaptive::Outcome::PeerFailure);
        drop(permit);
        clock.advance(std::time::Duration::from_secs(1));
        let probe = adaptive.acquire(&node).unwrap();
        let request = crate::peer::tests::request(&admission, 81);
        let (signed, binding) = forwarding.sign_request(request).unwrap();
        let destination = Forwarding::new(signers[2].clone());
        let admitted = destination.verify_request(signed).unwrap();
        let mut response = destination.sign_response(admitted.binding(), PeerResponse::Miss).unwrap();
        response.response = PeerResponse::Overloaded;
        let result = requester.verify_exchange(transfer::RelayResponse::Complete(response), &binding);
        assert!(result.is_err());
        assert_eq!(metrics.count(Event::PeerVerified), 0);
        drop(probe);
        assert!(!adaptive.available(&node));
    }
}
