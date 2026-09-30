//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
use super::{
    transfer::Transfers,
    wire::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse},
};
use crate::telemetry::failures::{Observer, Stage};
use crate::{
    error::{Error, Operation},
    runtime::deadline::RequestScope,
    security::forwarding::Forwarding,
    topology::{membership::MembershipLease, paths::Paths, rails},
};
use std::rc::Rc;
pub trait PeerClient {
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
/// use racer_dataplane::{error::Result, peer::{requester::{PeerClient, PeerTransport},
///     relay::Relay, server::PeerServer,
///     wire::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse}},
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
    ) -> Operation<'a, super::transfer::RelayResponse> {
        Box::pin(async move {
            let response = self.exchange(request, membership, scope).await;
            drop(reservation);
            response.map(super::transfer::RelayResponse::Complete)
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
    network: Rc<super::PeerNetwork>,
}
impl Requester {
    #[cfg(test)]
    pub(crate) fn admission(&self) -> &std::sync::Arc<super::adaptive::AdaptivePeers> {
        self.paths.peer_admission.as_ref().unwrap()
    }
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transfers: Rc<Transfers>,
        network: Rc<super::PeerNetwork>,
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
    /// Sign a fresh attempt, exchange the full envelope, then verify the response
    /// using the binding retained from signing. Logical callers retain the proof.
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        membership: MembershipLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let scope = super::request_scope(&request, scope)?;
            super::check_membership(&request, &membership)?;
            let network = &self.network;
            let search_budget = super::search_budget(&request.route, &network.local)?;
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
    ) -> Operation<'a, super::transfer::RelayResponse> {
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
                super::transfer::RelayResponse::Complete(response) => Ok(response),
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
    ) -> Operation<'a, super::transfer::RelayResponse> {
        Box::pin(async move {
            let scope = super::request_scope(&request.request, scope)?;
            super::check_membership(&request.request, &membership)?;
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
                super::wire::Operation::Page { page, .. } => Some(page.clone()),
                super::wire::Operation::Subscribe { subscription, .. } => subscription
                    .demand
                    .intervals()
                    .first()
                    .map(|interval| crate::model::PageId {
                        version: subscription.version.clone(),
                        number: crate::model::PageNumber(interval.start),
                    }),
                _ => None,
            };
            let plan = if let Some(page) = rail_hint {
                let search = super::search_budget(budget, &network.local)?;
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
                .exchange_inner(
                    endpoint,
                    request,
                    plan,
                    relay,
                    permit.clone(),
                    socket_failure.clone(),
                    &scope,
                )
                .await;
            // Receiving a signed envelope is not proof. Recover only after the
            // complete reverse chain and original request binding are verified.
            let response = response.and_then(|response| self.verify_exchange(response, &binding));
            use crate::topology::health::LinkOutcome;
            let outcome = match &response {
                Ok(super::transfer::RelayResponse::Complete(_)) => Some(LinkOutcome::Success),
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
                    Ok(super::transfer::RelayResponse::Complete(response))
                        if matches!(response.response, super::wire::PeerResponse::Overloaded) =>
                    {
                        super::adaptive::Outcome::Neutral
                    }
                    Ok(super::transfer::RelayResponse::Complete(_)) => {
                        super::adaptive::Outcome::Verified
                    }
                    Err(Error::Overloaded) => super::adaptive::Outcome::LocalPressure,
                    _ => super::adaptive::Outcome::Neutral,
                });
            }
            drop(membership);
            response
        })
    }

    fn verify_exchange(
        &self,
        response: super::transfer::RelayResponse,
        binding: &crate::security::forwarding::RequestBinding,
    ) -> crate::error::Result<super::transfer::RelayResponse> {
        match response {
            super::transfer::RelayResponse::Complete(response) => self
                .forwarding
                .verify_response(response, binding)
                .map(|verified| super::transfer::RelayResponse::Complete(verified.into_signed())),
            super::transfer::RelayResponse::Http {
                authentication,
                mut connection,
                length,
            } => {
                self.forwarding
                    .verify_opaque(&authentication, length, binding)?;
                // All Racer outcomes use HTTP 200. Inspect the authenticated
                // outcome, exactly as the materialized path does, not HTTP status.
                connection.peer_response_verified = crate::security::protocol::field(
                    &authentication.original.head,
                    "racer-outcome",
                )? != "overloaded";
                Ok(super::transfer::RelayResponse::Http {
                    authentication,
                    connection,
                    length,
                })
            }
        }
    }
}
#[cfg(test)]
#[path = "requester_safety_tests.rs"]
mod safety_tests;
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn invalid_response_never_recovers_half_open_peer() {
        use crate::{
            model::NodeId,
            peer::wire::PeerResponse,
            telemetry::metrics::{Event, Metrics},
            topology::health::LinkHealth,
        };
        let clock = crate::runtime::environment::SimulationClock::new(782);
        let _env = clock.environment(0).enter();
        let signers = crate::peer::tests::signers();
        let admission = Rc::new(crate::runtime::admission::Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(crate::runtime::reactor::Reactor::new(admission.clone()));
        let io = Rc::new(crate::http::io::HttpIo::with_admission(
            reactor.clone(),
            crate::http::codec::Codec::new(crate::peer::wire::MAX_ENVELOPE_HEAD, 0),
            admission.clone(),
        ));
        let pool = Rc::new(crate::http::pool::HttpPool::new(
            reactor,
            admission.clone(),
            2,
        ));
        let forwarding = Rc::new(Forwarding::new(signers[0].clone()));
        let requester = Requester::new(
            Rc::new(Paths::new(Rc::new(LinkHealth), 4)),
            forwarding.clone(),
            Rc::new(Transfers::new(pool, io, None)),
            Rc::new(
                super::super::PeerNetwork::new(signers[0].node().clone(), Default::default())
                    .unwrap(),
            ),
        );
        let metrics = Metrics::default();
        let adaptive =
            super::super::adaptive::AdaptivePeers::new(Default::default(), metrics.clone())
                .unwrap();
        let node: NodeId = signers[2].node().clone();
        let permit = adaptive.acquire(&node).unwrap();
        permit.observe(super::super::adaptive::Outcome::PeerFailure);
        drop(permit);
        clock.advance(std::time::Duration::from_secs(1));
        let probe = adaptive.acquire(&node).unwrap();
        let request = crate::peer::tests::request(&admission, 81);
        let (signed, binding) = forwarding.sign_request(request).unwrap();
        let destination = Forwarding::new(signers[2].clone());
        let admitted = destination.verify_request(signed).unwrap();
        let mut response = destination
            .sign_response(admitted.binding(), PeerResponse::Miss)
            .unwrap();
        response.response = PeerResponse::Overloaded;
        let result = requester.verify_exchange(
            super::super::transfer::RelayResponse::Complete(response),
            &binding,
        );
        assert!(result.is_err());
        assert_eq!(metrics.count(Event::PeerVerified), 0);
        drop(probe);
        assert!(!adaptive.available(&node));
    }
}
