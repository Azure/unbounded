//! Preserve original signatures and bind each signed hop to consumed route state.
//!
//! The owned API supports a complete relay round trip without degrading signed
//! envelopes to logical operations. This is a compile-only contract example;
//! authentication and transport remain fail-closed stubs.
//!
//! ```no_run
//! use racer_dataplane::{
//!     error::Result,
//!     model::identity::NodeId,
//!     peer::{server::LocalPageService, wire::{PeerRequest, VerifiedResponse}},
//!     runtime::deadline::RequestScope,
//!     security::forwarding::Forwarding,
//!     topology::paths::RouteBudget,
//! };
//!
//! async fn round_trip(
//!     requester: &Forwarding,
//!     relay: &Forwarding,
//!     destination: &Forwarding,
//!     local: &dyn LocalPageService,
//!     request: PeerRequest,
//!     next: &NodeId,
//!     previous: &NodeId,
//!     budget: RouteBudget,
//!     scope: &RequestScope,
//! ) -> Result<VerifiedResponse> {
//!     let (outbound, outstanding) = requester.sign_request(request)?;
//!     let ingress = relay.verify_request(outbound)?;
//!     let reverse_binding = ingress.binding().clone();
//!     let forwarded = relay.append_request(ingress, next, budget)?;
//!     let admitted = destination.verify_request(forwarded)?;
//!     let reply_binding = admitted.binding().clone();
//!     let unsigned = local.serve_peer(admitted, scope).await?;
//!     let response = destination.sign_response(&reply_binding, unsigned)?;
//!     let verified = relay.verify_response(response, &reverse_binding)?;
//!     let reverse = relay.append_response(verified, previous)?;
//!     requester.verify_response(reverse, &outstanding)
//! }
//! ```
use super::{
    certificates::VerifiedPeer,
    signing::{Signatures, SignedHead},
};
use crate::{
    error::{Result, pending},
    model::identity::NodeId,
    peer::wire::{PeerRequest, PeerResponse, SignedRequest, SignedResponse},
    topology::paths::RouteBudget,
};
use std::rc::Rc;
pub struct Forwarding {
    signatures: Rc<Signatures>,
}
pub struct ForwardedHead {
    /// Shared ownership lets an outstanding attempt retain its exact original
    /// head while the transport owns the envelope. Never replace it at a relay.
    pub original: Rc<SignedHead>,
    pub hops: Vec<SignedHead>,
}

/// Opaque outstanding-request context, minted only by request signing or ingress
/// verification. Retains the exact original head/signature, not just a request ID.
/// Cloning retains ownership for response signing after local service consumes the
/// request. No credentials are decoded or decrypted here.
///
/// ```compile_fail
/// use racer_dataplane::security::forwarding::RequestBinding;
/// fn fabricate_binding() -> RequestBinding {
///     RequestBinding { original: todo!() }
/// }
/// ```
#[derive(Clone)]
pub struct RequestBinding {
    original: Rc<SignedHead>,
}

/// Admitted ingress with its original signature and entire forwarding chain.
/// Only this module's verifier can construct it. Shared access prevents callers
/// from modifying signed fields after verification.
///
/// ```compile_fail
/// use racer_dataplane::peer::wire::{SignedRequest, VerifiedRequest};
/// fn bypass_verification(request: SignedRequest) -> VerifiedRequest {
///     request.into()
/// }
/// ```
///
/// ```compile_fail
/// use racer_dataplane::peer::wire::VerifiedRequest;
/// fn change_verified_route(request: VerifiedRequest) {
///     request.request().route.remaining_links = 255;
/// }
/// ```
pub struct VerifiedRequest {
    signed: SignedRequest,
    binding: RequestBinding,
    origin: VerifiedPeer,
    forwarders: Vec<VerifiedPeer>,
}
impl VerifiedRequest {
    pub fn request(&self) -> &PeerRequest {
        &self.signed.request
    }
    pub fn signed(&self) -> &SignedRequest {
        &self.signed
    }
    pub fn binding(&self) -> &RequestBinding {
        &self.binding
    }
    pub fn origin(&self) -> &VerifiedPeer {
        &self.origin
    }
    pub fn forwarders(&self) -> &[VerifiedPeer] {
        &self.forwarders
    }
    pub fn into_signed(self) -> SignedRequest {
        self.signed
    }
}

/// Verified response bound to one original request, retaining every signature.
/// This is distinct from both an unsigned local result and unverified wire input.
///
/// ```compile_fail
/// use racer_dataplane::peer::wire::{SignedResponse, VerifiedResponse};
/// fn bypass_verification(response: SignedResponse) -> VerifiedResponse {
///     response.into()
/// }
/// ```
///
/// ```compile_fail
/// use racer_dataplane::peer::wire::{PeerResponse, VerifiedResponse};
/// fn replace_verified_result(mut response: VerifiedResponse) {
///     *response.response() = PeerResponse::Miss;
/// }
/// ```
pub struct VerifiedResponse {
    signed: SignedResponse,
    binding: RequestBinding,
    origin: VerifiedPeer,
    forwarders: Vec<VerifiedPeer>,
}
impl VerifiedResponse {
    pub fn response(&self) -> &PeerResponse {
        &self.signed.response
    }
    pub fn signed(&self) -> &SignedResponse {
        &self.signed
    }
    pub fn binding(&self) -> &RequestBinding {
        &self.binding
    }
    pub fn origin(&self) -> &VerifiedPeer {
        &self.origin
    }
    pub fn forwarders(&self) -> &[VerifiedPeer] {
        &self.forwarders
    }
    pub fn into_signed(self) -> SignedResponse {
        self.signed
    }
}
impl Forwarding {
    pub fn new(signatures: Rc<Signatures>) -> Self {
        Self { signatures }
    }
    /// Encode and sign a fresh local attempt, retaining request-binding context
    /// for its eventual response. Canonical encoding and signing remain pending.
    pub fn sign_request(&self, _request: PeerRequest) -> Result<(SignedRequest, RequestBinding)> {
        pending("forwarding.sign_request")
    }
    /// Verify original and every hop, identity, replay, logical-field agreement,
    /// and monotonic routing limits before returning service/relay admission.
    pub fn verify_request(&self, _request: SignedRequest) -> Result<VerifiedRequest> {
        pending("forwarding.verify_request")
    }
    /// Sign all local outcomes, including misses/errors, against the exact request.
    pub fn sign_response(
        &self,
        _request: &RequestBinding,
        _response: PeerResponse,
    ) -> Result<SignedResponse> {
        pending("forwarding.sign_response")
    }
    /// Verify original and every hop, replay/identity and signed logical fields,
    /// then check response correlation against the supplied outstanding attempt.
    /// A request ID alone is not sufficient; the original signature binds identity,
    /// operation, membership, attempt, and freshness without hashing page bytes.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{peer::wire::SignedResponse, security::forwarding::Forwarding};
    /// fn unbound_response(auth: &Forwarding, response: SignedResponse) {
    ///     auth.verify_response(response);
    /// }
    /// ```
    pub fn verify_response(
        &self,
        _response: SignedResponse,
        _request: &RequestBinding,
    ) -> Result<VerifiedResponse> {
        pending("forwarding.verify_response")
    }
    /// Preserve the original and existing hops; append a separately signed header
    /// bound to that original signature, prior chain, next hop, and consumed route.
    /// Only the effective route may change, without extending deadline or budget.
    pub fn append_request(
        &self,
        _request: VerifiedRequest,
        _next_hop: &NodeId,
        _budget: RouteBudget,
    ) -> Result<SignedRequest> {
        pending("forwarding.append_request")
    }
    /// Preserve the responder's original signature, prior chain, request binding,
    /// and ciphertext. Append a separate signed hop for the recorded reverse path.
    pub fn append_response(
        &self,
        _response: VerifiedResponse,
        _previous_hop: &NodeId,
    ) -> Result<SignedResponse> {
        pending("forwarding.append_response")
    }
}
#[cfg(test)]
mod tests { /* Budget/deadline extension, original signature substitution, hop binding. */
}
