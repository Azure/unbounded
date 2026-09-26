//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
use super::{
    handshake::Handshake,
    transfer::Transfers,
    wire::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse},
};
use crate::{
    error::{Error, Operation},
    runtime::deadline::RequestScope,
    security::forwarding::Forwarding,
    topology::{paths::Paths, rails::Rails},
};
use std::rc::Rc;
pub trait PeerClient {
    fn request<'a>(
        &'a self,
        request: PeerRequest,
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
///     runtime::deadline::RequestScope, security::forwarding::Forwarding};
/// async fn interfaces(
///     client: &dyn PeerClient, transport: &dyn PeerTransport,
///     server: &PeerServer, relay: &Relay, auth: &Forwarding,
///     local: PeerRequest, wire: SignedRequest, outbound: SignedRequest,
///     inbound: SignedRequest, scope: &RequestScope,
/// ) -> Result<()> {
///     let verified: VerifiedResponse = client.request(local, scope).await?;
///     let _wire_response: SignedResponse = verified.into_signed();
///     let admitted = auth.verify_request(wire)?;
///     let reply = relay.forward(admitted, scope).await?;
///     let _retained_chain = reply.authentication;
///     // Both network boundaries preserve envelopes in each direction.
///     let _exchange: SignedResponse = transport.exchange(outbound, scope).await?;
///     let _dispatch: SignedResponse = server.dispatch(inbound, scope).await?;
///     Ok(())
/// }
/// ```
pub trait PeerTransport {
    fn exchange<'a>(
        &'a self,
        request: SignedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse>;
}
pub struct Requester {
    paths: Rc<Paths>,
    rails: Rc<Rails>,
    forwarding: Rc<Forwarding>,
    handshake: Rc<Handshake>,
    transfers: Rc<Transfers>,
    network: Option<Rc<super::PeerNetwork>>,
}
impl Requester {
    pub fn new(
        paths: Rc<Paths>,
        rails: Rc<Rails>,
        forwarding: Rc<Forwarding>,
        handshake: Rc<Handshake>,
        transfers: Rc<Transfers>,
    ) -> Self {
        Self {
            paths,
            rails,
            forwarding,
            handshake,
            transfers,
            network: None,
        }
    }
    pub fn with_network(mut self, network: Rc<super::PeerNetwork>) -> Self {
        self.network = Some(network);
        self
    }
}
impl PeerClient for Requester {
    /// Sign a fresh attempt, exchange the full envelope, then verify the response
    /// using the binding retained from signing. Logical callers retain the proof.
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        Box::pin(async move {
            let scope = super::request_scope(&request, scope)?;
            let network = self.network.as_ref().ok_or(Error::InvalidConfiguration)?;
            let search_budget = super::search_budget(&request.route, &network.local)?;
            let route = self
                .paths
                .shortest_async(
                    network.membership(request.route.membership)?,
                    &network.local,
                    &search_budget,
                )
                .await?;
            let next = route.nodes.get(1).ok_or(Error::Unavailable)?;
            let _capabilities = self
                .handshake
                .negotiate_at(next, request.route.membership, &scope)
                .await?;
            let (signed, binding) = self.forwarding.sign_request_to(request, next)?;
            let response = self.exchange(signed, &scope).await?;
            scope.check()?;
            self.forwarding.verify_response(response, &binding)
        })
    }
}
impl PeerTransport for Requester {
    fn exchange<'a>(
        &'a self,
        request: SignedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        Box::pin(async move {
            let scope = super::request_scope(&request.request, scope)?;
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
            let endpoint = network.endpoint(budget.membership, &next)?;
            let mut plan =
                if let super::wire::Operation::Page { page, .. } = &request.request.operation {
                    let search = super::search_budget(budget, &network.local)?;
                    let route = self
                        .paths
                        .shortest_async(
                            network.membership(budget.membership)?,
                            &network.local,
                            &search,
                        )
                        .await?;
                    if route.nodes.get(1) != Some(&next) {
                        return Err(Error::Unavailable);
                    }
                    self.rails.select(&route, page)?
                } else {
                    crate::topology::rails::TransportPlan::Http
                };
            if matches!(plan, crate::topology::rails::TransportPlan::Rdma { .. }) {
                let capabilities = self.handshake.negotiate(&next).await?;
                if !capabilities.rdma || !capabilities.scoped_grants {
                    plan = crate::topology::rails::TransportPlan::Http;
                }
            }
            self.transfers
                .exchange_planned(endpoint, request, plan, &scope)
                .await
                .inspect_err(|error| {
                    // A restarted peer has a new receiver challenge. Rediscover it
                    // on the next attempt instead of retaining a stale cache hit.
                    if matches!(error, Error::Io | Error::Unauthorized | Error::Replay) {
                        self.handshake.invalidate(&next);
                    }
                })
        })
    }
}
#[cfg(test)]
mod tests { /* Late attempts, fresh replay nonce per send, retry budgets, cancellation. */
}
