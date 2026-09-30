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
    topology::{membership::MembershipLease, paths::Paths, rails::Rails},
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
    rails: Rc<Rails>,
    forwarding: Rc<Forwarding>,
    transfers: Rc<Transfers>,
    network: Option<Rc<super::PeerNetwork>>,
}
impl Requester {
    pub fn new(
        paths: Rc<Paths>,
        rails: Rc<Rails>,
        forwarding: Rc<Forwarding>,
        transfers: Rc<Transfers>,
    ) -> Self {
        Self {
            observer: Observer::default(),
            health: paths.link_health(),
            paths,
            rails,
            forwarding,
            transfers,
            network: None,
        }
    }
    pub fn with_network(mut self, network: Rc<super::PeerNetwork>) -> Self {
        self.network = Some(network);
        self
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
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
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
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
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
                self.rails.select(&route, &page)?
            } else {
                crate::topology::rails::TransportPlan::Http
            };
            let _probe = self.health.acquire(&next)?;
            let response = self
                .transfers
                .exchange_inner(endpoint, request, plan, relay, &scope)
                .await;
            // A signed application response (including miss, 401 or 403) proves
            // the immediate transport works. It is verified by the logical owner.
            use crate::topology::health::LinkOutcome;
            let outcome = match &response {
                Ok(_) => Some(LinkOutcome::Success),
                Err(Error::Io | Error::Unavailable) => Some(LinkOutcome::Refused),
                Err(Error::DeadlineExceeded) => Some(LinkOutcome::Timeout),
                Err(Error::CorruptRecord | Error::InvalidRequest) => {
                    Some(LinkOutcome::ProtocolFailure)
                }
                _ => None,
            };
            if let Some(outcome) = outcome {
                self.health.observe(&next, outcome)?;
            }
            drop(membership);
            response
        })
    }
}
#[cfg(test)]
mod tests { /* Late attempts, connection ordering, retry budgets, cancellation. */
}
