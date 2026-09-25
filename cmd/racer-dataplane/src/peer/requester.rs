//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
use super::{
    handshake::Handshake,
    transfer::Transfers,
    wire::{PeerRequest, SignedRequest, SignedResponse, VerifiedResponse},
};
use crate::{
    error::{Operation, deferred},
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
        }
    }
}
impl PeerClient for Requester {
    /// Sign a fresh attempt, exchange the full envelope, then verify the response
    /// using the binding retained from signing. Logical callers retain the proof.
    fn request<'a>(
        &'a self,
        _request: PeerRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedResponse> {
        deferred("peer.request")
    }
}
impl PeerTransport for Requester {
    fn exchange<'a>(
        &'a self,
        _request: SignedRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        deferred("peer.exchange")
    }
}
#[cfg(test)]
mod tests { /* Late attempts, fresh replay nonce per send, retry budgets, cancellation. */
}
