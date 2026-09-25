//! Preserve original signatures and bind each signed hop to consumed route state.
//!
//! The owned API supports a complete relay round trip without degrading signed
//! envelopes to logical operations. Each receiver admits only its final envelope;
//! historical originals and hops are cryptographically verified without readmission.
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
    protocol::{self, field, number, push, push_binary},
    signing::{Signatures, SignedHead},
    signing::{node_field, receiver, signed_digest},
};
use crate::{
    error::{Error, Result},
    http::codec::{MessageHead, StartLine},
    model::identity::NodeId,
    peer::wire::{PeerRequest, PeerResponse, SignedRequest, SignedResponse},
    topology::paths::RouteBudget,
};
use std::{rc::Rc, sync::Arc};
pub struct Forwarding {
    signatures: Rc<Signatures>,
}
pub struct ForwardedHead {
    /// Shared ownership lets an outstanding attempt retain its exact original
    /// head while the transport owns the envelope. Never replace it at a relay.
    pub original: Arc<SignedHead>,
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
    original: Arc<SignedHead>,
    path: Vec<NodeId>,
    deadline: u64,
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
    /// for its eventual response. Direct sends target the route destination.
    pub fn sign_request(&self, request: PeerRequest) -> Result<(SignedRequest, RequestBinding)> {
        let next = request.route.destination.clone();
        self.sign_request_to(request, &next)
    }
    /// Sign an initial request to an explicitly selected first relay. The route
    /// starts with exactly the local node in `visited`; links include this send.
    pub fn sign_request_to(
        &self,
        request: PeerRequest,
        next: &NodeId,
    ) -> Result<(SignedRequest, RequestBinding)> {
        let route = RouteState::from_budget(&request.route)?;
        if route.deadline <= protocol::millis(std::time::SystemTime::now())? {
            return Err(Error::DeadlineExceeded);
        }
        if route.visited != [self.signatures.node().clone()]
            || next == self.signatures.node()
            || route.links == 0
        {
            return Err(Error::HopBudgetExhausted);
        }
        let mut head = protocol::request_head(&request)?;
        push(&mut head, "racer-receiver", &next.0);
        let original = Arc::new(self.signatures.sign(head)?);
        let binding = RequestBinding {
            original: original.clone(),
            path: route.visited,
            deadline: route.deadline,
        };
        Ok((
            SignedRequest {
                authentication: ForwardedHead {
                    original,
                    hops: Vec::new(),
                },
                request,
            },
            binding,
        ))
    }
    /// Verify original and every hop, identity, replay, logical-field agreement,
    /// and monotonic routing limits before returning service/relay admission.
    pub fn verify_request(&self, request: SignedRequest) -> Result<VerifiedRequest> {
        let auth = &request.authentication;
        if auth.hops.len() >= protocol::MAX_HOPS {
            return Err(Error::HopBudgetExhausted);
        }
        let origin = self.signatures.verify_historical(&auth.original)?;
        protocol::agrees(
            &auth.original.head,
            &protocol::request_head(&request.request)?,
            true,
        )?;
        let mut route = RouteState::from_head(&auth.original.head)?;
        if route.visited != [origin.node().clone()] {
            return Err(Error::Unauthorized);
        }
        let mut previous = auth.original.as_ref();
        let mut forwarders = Vec::new();
        for hop in &auth.hops {
            let peer = self.signatures.verify_historical(hop)?;
            if peer.node() != &receiver(&previous.head)? {
                return Err(Error::Unauthorized);
            }
            let next = RouteState::from_head(&hop.head)?;
            route.transition(&next, peer.node())?;
            check_hop(hop, "request-hop", &auth.original, previous, &next)?;
            route = next;
            previous = hop;
            forwarders.push(peer);
        }
        if route != RouteState::from_budget(&request.request.route)?
            || route.visited.contains(&receiver(&previous.head)?)
        {
            return Err(Error::Unauthorized);
        }
        if route.deadline <= protocol::millis(std::time::SystemTime::now())? {
            return Err(Error::DeadlineExceeded);
        }
        let last_peer = forwarders.last().unwrap_or(&origin);
        self.signatures.admit(previous, last_peer)?;
        let mut path = route.visited;
        path.push(self.signatures.node().clone());
        let binding = RequestBinding {
            original: auth.original.clone(),
            path,
            deadline: route.deadline,
        };
        Ok(VerifiedRequest {
            signed: request,
            binding,
            origin,
            forwarders,
        })
    }
    /// Sign all local outcomes, including misses/errors, against the exact request.
    pub fn sign_response(
        &self,
        request: &RequestBinding,
        response: PeerResponse,
    ) -> Result<SignedResponse> {
        check_request_deadline(request)?;
        if request.path.last() != Some(self.signatures.node()) || request.path.len() < 2 {
            return Err(Error::Unauthorized);
        }
        let mut head =
            protocol::response_head(&response, &signed_digest(&request.original)?, &request.path)?;
        response_matches(&head, &request.original.head)?;
        response_authority(&head, &request.original.head, self.signatures.node())?;
        push(
            &mut head,
            "racer-receiver",
            &request.path[request.path.len() - 2].0,
        );
        let original = Arc::new(self.signatures.sign(head)?);
        Ok(SignedResponse {
            authentication: ForwardedHead {
                original,
                hops: Vec::new(),
            },
            response,
        })
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
        response: SignedResponse,
        request: &RequestBinding,
    ) -> Result<VerifiedResponse> {
        check_request_deadline(request)?;
        let auth = &response.authentication;
        if auth.hops.len() >= protocol::MAX_HOPS {
            return Err(Error::HopBudgetExhausted);
        }
        let origin = self.signatures.verify_historical(&auth.original)?;
        let path =
            protocol::decode_nodes(field(&auth.original.head, "racer-response-path")?.as_bytes())?;
        if path.len() < 2
            || !path.starts_with(&request.path)
            || path.last() != Some(origin.node())
            || path[1] != receiver(&request.original.head)?
            || path.len() > RouteState::from_head(&request.original.head)?.links as usize + 1
        {
            return Err(Error::Unauthorized);
        }
        protocol::agrees(
            &auth.original.head,
            &protocol::response_head(
                &response.response,
                &signed_digest(&request.original)?,
                &path,
            )?,
            false,
        )?;
        response_matches(&auth.original.head, &request.original.head)?;
        response_authority(&auth.original.head, &request.original.head, origin.node())?;
        if receiver(&auth.original.head)? != path[path.len() - 2] {
            return Err(Error::Unauthorized);
        }
        let mut previous = auth.original.as_ref();
        let mut forwarders = Vec::new();
        for (i, hop) in auth.hops.iter().enumerate() {
            let index = path
                .len()
                .checked_sub(i + 2)
                .filter(|n| *n > 0)
                .ok_or(Error::Unauthorized)?;
            let peer = self.signatures.verify_historical(hop)?;
            if peer.node() != &path[index] || receiver(&hop.head)? != path[index - 1] {
                return Err(Error::Unauthorized);
            }
            let expected = response_hop_head(
                &auth.original,
                previous,
                &request.original,
                &path,
                index - 1,
            )?;
            protocol::agrees(&hop.head, &expected, false)?;
            previous = hop;
            forwarders.push(peer);
        }
        let index = path
            .len()
            .checked_sub(auth.hops.len() + 2)
            .ok_or(Error::Unauthorized)?;
        if index + 1 != request.path.len() || request.path.last() != Some(self.signatures.node()) {
            return Err(Error::Unauthorized);
        }
        self.signatures
            .admit(previous, forwarders.last().unwrap_or(&origin))?;
        Ok(VerifiedResponse {
            signed: response,
            binding: request.clone(),
            origin,
            forwarders,
        })
    }
    /// Preserve the original and existing hops; append a separately signed header
    /// bound to that original signature, prior chain, next hop, and consumed route.
    /// Only the effective route may change, without extending deadline or budget.
    pub fn append_request(
        &self,
        mut request: VerifiedRequest,
        next_hop: &NodeId,
        budget: RouteBudget,
    ) -> Result<SignedRequest> {
        check_request_deadline(&request.binding)?;
        if request.binding.path.last() != Some(self.signatures.node()) {
            return Err(Error::Unauthorized);
        }
        let old = RouteState::from_budget(&request.signed.request.route)?;
        let next = RouteState::from_budget(&budget)?;
        if next.deadline <= protocol::millis(std::time::SystemTime::now())? {
            return Err(Error::DeadlineExceeded);
        }
        old.transition(&next, self.signatures.node())?;
        if next.visited.contains(next_hop) {
            return Err(Error::HopBudgetExhausted);
        }
        let auth = &mut request.signed.authentication;
        let previous = auth.hops.last().unwrap_or(&auth.original);
        let mut head = hop_head("request-hop", &auth.original, previous)?;
        protocol::route_headers(&mut head, &budget)?;
        push(&mut head, "racer-receiver", &next_hop.0);
        auth.hops.push(self.signatures.sign(head)?);
        request.signed.request.route = budget;
        Ok(request.signed)
    }
    /// Preserve the responder's original signature, prior chain, request binding,
    /// and ciphertext. Append a separate signed hop for the recorded reverse path.
    pub fn append_response(
        &self,
        mut response: VerifiedResponse,
        previous_hop: &NodeId,
    ) -> Result<SignedResponse> {
        check_request_deadline(&response.binding)?;
        let auth = &mut response.signed.authentication;
        let path =
            protocol::decode_nodes(field(&auth.original.head, "racer-response-path")?.as_bytes())?;
        let index = path
            .len()
            .checked_sub(auth.hops.len() + 2)
            .filter(|n| *n > 0)
            .ok_or(Error::Unauthorized)?;
        if path[index] != *self.signatures.node() || path[index - 1] != *previous_hop {
            return Err(Error::Unauthorized);
        }
        let previous = auth.hops.last().unwrap_or(&auth.original);
        let mut head = response_hop_head(
            &auth.original,
            previous,
            &response.binding.original,
            &path,
            index - 1,
        )?;
        push(&mut head, "racer-receiver", &previous_hop.0);
        auth.hops.push(self.signatures.sign(head)?);
        Ok(response.signed)
    }
}
#[derive(PartialEq, Eq)]
struct RouteState {
    membership: u64,
    request: String,
    attempt: String,
    destination: NodeId,
    visited: Vec<NodeId>,
    links: u64,
    deadline: u64,
}
impl RouteState {
    fn from_budget(budget: &RouteBudget) -> Result<Self> {
        let mut head = MessageHead {
            start: StartLine::Response { status: 200 },
            headers: Vec::new(),
        };
        protocol::route_headers(&mut head, budget)?;
        Self::from_head(&head)
    }
    fn from_head(head: &MessageHead) -> Result<Self> {
        let state = Self {
            membership: number(head, "racer-route-membership")?,
            request: field(head, "racer-route-request")?,
            attempt: field(head, "racer-route-attempt")?,
            destination: node_field(head, "racer-route-destination")?,
            visited: protocol::decode_nodes(field(head, "racer-route-visited")?.as_bytes())?,
            links: number(head, "racer-route-links")?,
            deadline: number(head, "racer-route-deadline")?,
        };
        if state.membership == 0
            || protocol::decode_binary(state.request.as_bytes())?.len() != 16
            || protocol::decode_binary(state.attempt.as_bytes())?.len() != 16
        {
            return Err(Error::Unauthorized);
        }
        if state.links == 0
            || state.links > protocol::MAX_HOPS as u64
            || state.visited.is_empty()
            || state.visited.len() + state.links as usize > protocol::MAX_HOPS + 1
        {
            return Err(Error::HopBudgetExhausted);
        }
        Ok(state)
    }
    fn transition(&self, next: &Self, signer: &NodeId) -> Result<()> {
        let mut visited = self.visited.clone();
        visited.push(signer.clone());
        if self.links <= 1
            || next.links != self.links - 1
            || next.deadline > self.deadline
            || next.membership != self.membership
            || next.request != self.request
            || next.attempt != self.attempt
            || next.destination != self.destination
            || self.visited.contains(signer)
            || signer == &self.destination
            || next.visited != visited
        {
            return Err(Error::HopBudgetExhausted);
        }
        Ok(())
    }
}
fn hop_head(kind: &str, original: &SignedHead, previous: &SignedHead) -> Result<MessageHead> {
    let mut head = MessageHead {
        start: StartLine::Request {
            method: "POST".into(),
            target: "/racer/peer/v1/hop".into(),
        },
        headers: Vec::new(),
    };
    push(&mut head, "racer-kind", kind);
    push(&mut head, "content-length", 0);
    push_binary(&mut head, "racer-original", &signed_digest(original)?);
    push_binary(&mut head, "racer-previous", &signed_digest(previous)?);
    Ok(head)
}
fn check_hop(
    hop: &SignedHead,
    kind: &str,
    original: &SignedHead,
    previous: &SignedHead,
    route: &RouteState,
) -> Result<()> {
    let mut expected = hop_head(kind, original, previous)?;
    // The route parser has already validated monotonic state. Copy only its fixed
    // schema to the comparison head; unexpected fields still fail agreement.
    for name in [
        "racer-route-membership",
        "racer-route-request",
        "racer-route-attempt",
        "racer-route-destination",
        "racer-route-visited",
        "racer-route-links",
        "racer-route-deadline",
    ] {
        push(&mut expected, name, field(&hop.head, name)?);
    }
    if route.visited.contains(&receiver(&hop.head)?) {
        return Err(Error::Unauthorized);
    }
    protocol::agrees(&hop.head, &expected, false)
}
fn response_hop_head(
    original: &SignedHead,
    previous: &SignedHead,
    request: &SignedHead,
    path: &[NodeId],
    index: usize,
) -> Result<MessageHead> {
    let mut head = hop_head("response-hop", original, previous)?;
    push_binary(&mut head, "racer-request-binding", &signed_digest(request)?);
    push(&mut head, "racer-response-path", protocol::nodes(path)?);
    push(&mut head, "racer-reverse-index", index);
    Ok(head)
}
fn response_matches(response: &MessageHead, request: &MessageHead) -> Result<()> {
    let outcome = field(response, "racer-outcome")?;
    if outcome != "page" && outcome != "metadata" {
        return Ok(());
    }
    if field(request, "racer-operation")? != outcome {
        return Err(Error::Unauthorized);
    }
    for name in ["racer-cache", "racer-key"] {
        if field(response, name)? != field(request, name)? {
            return Err(Error::Unauthorized);
        }
    }
    if let Some(etag) = request.unique("racer-etag")? {
        if response.unique("racer-etag")? != Some(etag) {
            return Err(Error::Unauthorized);
        }
    }
    if outcome == "page" && field(response, "racer-page")? != field(request, "racer-page")? {
        return Err(Error::Unauthorized);
    }
    Ok(())
}
/// A relay may report its own failure, but only the requested destination may
/// assert successful metadata or page data, even when a shorter path is valid.
fn response_authority(
    response: &MessageHead,
    request: &MessageHead,
    signer: &NodeId,
) -> Result<()> {
    if matches!(
        field(response, "racer-outcome")?.as_str(),
        "page" | "metadata"
    ) && signer != &node_field(request, "racer-route-destination")?
    {
        return Err(Error::Unauthorized);
    }
    Ok(())
}
fn check_request_deadline(request: &RequestBinding) -> Result<()> {
    if request.deadline <= protocol::millis(std::time::SystemTime::now())? {
        return Err(Error::DeadlineExceeded);
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::{
            context::{EncryptedAuthorization, OpaqueMetadata, PeerOriginContext},
            envelope::{KeyId, Nonce},
            identity::*,
            limits::{Limits, ResourceClass},
            metadata::MetadataSelector,
        },
        peer::wire::{FetchMode, Operation},
        runtime::{admission::Admission, deadline::RequestScope},
        security::signing::tests::{clone_head, network, node},
    };
    use std::time::{Duration, Instant};
    fn request(id: u8) -> PeerRequest {
        let n = std::num::NonZeroUsize::new(1024 * 1024).unwrap();
        let admission = Admission::new(Limits {
            plaintext_bytes: n,
            ciphertext_bytes: n,
            dirty_bytes: n,
            registered_bytes: n,
            request_context_bytes: n,
            flights: n,
            waiters_per_flight: n,
            queue_entries: n,
            connections_per_neighbor: n,
            client_connections: n,
            pipes: n,
            range_window_pages: n,
            replay_entries: n,
            header_bytes: n,
            route_search_work: n,
            cached_rankings: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        });
        let object = ObjectId {
            cache: CacheId(node(88).0),
            key: CacheKey([7; 32]),
        };
        let scope = RequestScope::new(
            RequestId([id; 16]),
            Instant::now() + Duration::from_secs(30),
        )
        .unwrap();
        PeerRequest {
            operation: Operation::Metadata {
                object: object.clone(),
                selector: MetadataSelector::Pinned(StrongEtag::parse(b"\"version\"").unwrap()),
                mode: FetchMode::CopyOnly,
            },
            route: RouteBudget {
                membership: MembershipVersion(1),
                request: scope.request,
                attempt: AttemptId([2; 16]),
                destination: node(2),
                visited: vec![node(0)],
                remaining_links: 4,
                deadline: scope.deadline,
            },
            origin: PeerOriginContext {
                object,
                request: scope.request,
                attempt: AttemptId([2; 16]),
                metadata: Some(OpaqueMetadata::from_header(b"opaque\xff").unwrap()),
                authorization: Some(EncryptedAuthorization {
                    key_id: KeyId([3; 16]),
                    nonce: Nonce([4; 24]),
                    ciphertext: vec![5; 32],
                }),
                reservation: admission
                    .reserve(None, ResourceClass::RequestContext, 1024)
                    .unwrap(),
                scope,
            },
        }
    }
    fn budget(request: &VerifiedRequest) -> RouteBudget {
        let old = &request.request().route;
        let mut visited = old.visited.clone();
        visited.push(node(1));
        RouteBudget {
            membership: old.membership,
            request: old.request,
            attempt: old.attempt,
            destination: old.destination.clone(),
            visited,
            remaining_links: old.remaining_links - 1,
            deadline: old.deadline,
        }
    }
    fn copy_response(response: &SignedResponse) -> SignedResponse {
        SignedResponse {
            authentication: ForwardedHead {
                original: Arc::new(clone_head(&response.authentication.original)),
                hops: response
                    .authentication
                    .hops
                    .iter()
                    .map(clone_head)
                    .collect(),
            },
            response: PeerResponse::Miss,
        }
    }
    #[test]
    fn every_signed_request_field_must_agree_with_the_logical_request() {
        let signatures = network(3);
        let receiver = Forwarding::new(signatures[1].clone());
        let fields = protocol::request_head(&request(1)).unwrap().headers;
        for field in fields {
            let logical = request(1);
            let mut head = protocol::request_head(&logical).unwrap();
            head.headers
                .iter_mut()
                .find(|h| h.name == field.name)
                .unwrap()
                .value
                .push(b'x');
            push(&mut head, "racer-receiver", node(1).0);
            // Malformed transport fields can be rejected before signing; every
            // otherwise well-formed, validly signed disagreement must fail ingress.
            if let Ok(original) = signatures[0].sign(head) {
                assert!(
                    receiver
                        .verify_request(SignedRequest {
                            authentication: ForwardedHead {
                                original: Arc::new(original),
                                hops: Vec::new()
                            },
                            request: logical,
                        })
                        .is_err(),
                    "accepted signed logical disagreement {}",
                    field.name
                );
            }
        }
    }
    #[test]
    fn route_membership_and_identifier_sizes_are_validated_before_admission() {
        let signatures = network(3);
        let sender = Forwarding::new(signatures[0].clone());
        let receiver = Forwarding::new(signatures[1].clone());
        let mut zero = request(1);
        zero.route.membership.0 = 0;
        assert!(sender.sign_request_to(zero, &node(1)).is_err());
        for (name, value) in [
            ("racer-route-membership", "0".to_owned()),
            ("racer-route-request", protocol::binary(&[1; 15])),
            ("racer-route-attempt", protocol::binary(&[2; 17])),
        ] {
            let logical = request(1);
            let mut head = protocol::request_head(&logical).unwrap();
            head.headers
                .iter_mut()
                .find(|h| h.name == name)
                .unwrap()
                .value = value.into_bytes();
            push(&mut head, "racer-receiver", node(1).0);
            let original = signatures[0].sign(head).unwrap();
            assert!(
                receiver
                    .verify_request(SignedRequest {
                        authentication: ForwardedHead {
                            original: Arc::new(original),
                            hops: Vec::new()
                        },
                        request: logical,
                    })
                    .is_err(),
                "accepted malformed {name}"
            );
        }
    }
    #[test]
    fn every_negative_outcome_is_signed_and_bound_to_the_exact_attempt() {
        let signatures = network(3);
        let sender = Forwarding::new(signatures[0].clone());
        let receiver = Forwarding::new(signatures[2].clone());
        let (signed, binding) = sender.sign_request(request(1)).unwrap();
        // Same request/attempt IDs, but a distinct original signature and nonce.
        let (_, other) = sender.sign_request(request(1)).unwrap();
        let admitted = receiver.verify_request(signed).unwrap();
        for outcome in [
            PeerResponse::Miss,
            PeerResponse::VersionUnavailable,
            PeerResponse::Unavailable,
            PeerResponse::Overloaded,
            PeerResponse::OriginRejected,
        ] {
            let signed = receiver.sign_response(admitted.binding(), outcome).unwrap();
            // An authentic response cannot substitute for another signed attempt.
            let substituted = SignedResponse {
                authentication: ForwardedHead {
                    original: signed.authentication.original.clone(),
                    hops: Vec::new(),
                },
                response: match &signed.response {
                    PeerResponse::Miss => PeerResponse::Miss,
                    PeerResponse::VersionUnavailable => PeerResponse::VersionUnavailable,
                    PeerResponse::Unavailable => PeerResponse::Unavailable,
                    PeerResponse::Overloaded => PeerResponse::Overloaded,
                    _ => PeerResponse::OriginRejected,
                },
            };
            assert!(sender.verify_response(substituted, &other).is_err());
            sender.verify_response(signed, &binding).unwrap();
        }
    }
    #[test]
    fn signed_round_trip_reverse_path_response_substitution_and_replay() {
        let signatures = network(3);
        let f: Vec<_> = signatures
            .iter()
            .map(|s| Forwarding::new(s.clone()))
            .collect();
        let (outbound, binding) = f[0].sign_request_to(request(1), &node(1)).unwrap();
        let admitted = f[1].verify_request(outbound).unwrap();
        let reverse = admitted.binding().clone();
        let route = budget(&admitted);
        let forwarded = f[1].append_request(admitted, &node(2), route).unwrap();
        let destination = f[2].verify_request(forwarded).unwrap();
        let response = f[2]
            .sign_response(destination.binding(), PeerResponse::Miss)
            .unwrap();
        let mut substituted = copy_response(&response);
        substituted.response = PeerResponse::Overloaded;
        assert!(f[1].verify_response(substituted, &reverse).is_err());
        let (_, other) = f[0].sign_request_to(request(9), &node(1)).unwrap();
        assert!(
            f[1].verify_response(copy_response(&response), &other)
                .is_err()
        );
        let replay = copy_response(&response);
        let verified = f[1].verify_response(response, &reverse).unwrap();
        assert!(matches!(
            f[1].verify_response(replay, &reverse),
            Err(Error::Replay)
        ));
        assert!(f[1].append_response(verified, &node(2)).is_err());
        // Fresh response nonce allows a legitimate distinct signed outcome.
        let response = f[2]
            .sign_response(destination.binding(), PeerResponse::Miss)
            .unwrap();
        let verified = f[1].verify_response(response, &reverse).unwrap();
        let reverse_response = f[1].append_response(verified, &node(0)).unwrap();
        for h in &reverse_response.authentication.hops[0].head.headers {
            let mut tampered = copy_response(&reverse_response);
            tampered.authentication.hops[0]
                .head
                .headers
                .iter_mut()
                .find(|v| v.name == h.name)
                .unwrap()
                .value
                .push(b'x');
            assert!(
                f[0].verify_response(tampered, &binding).is_err(),
                "accepted reverse-hop mutation {}",
                h.name
            );
        }
        let mut stripped = copy_response(&reverse_response);
        stripped.authentication.hops.clear();
        assert!(f[0].verify_response(stripped, &binding).is_err());
        assert!(
            f[0].verify_response(copy_response(&reverse_response), &other)
                .is_err()
        );
        f[0].verify_response(reverse_response, &binding).unwrap();
    }
    #[test]
    fn page_descriptor_fields_and_successful_response_are_bound_exactly() {
        use crate::{
            memory::pool::{CiphertextBytes, CiphertextPage},
            model::{
                envelope::PageEnvelope,
                metadata::{ExpiresAt, ObjectMetadata},
            },
        };
        let signatures = network(3);
        let a = Forwarding::new(signatures[0].clone());
        let b = Forwarding::new(signatures[2].clone());
        let mut request = request(3);
        let page = PageId {
            version: ObjectVersion {
                object: request.origin.object.clone(),
                etag: StrongEtag::parse(b"\"version\"").unwrap(),
            },
            number: PageNumber(0),
        };
        request.operation = Operation::Page {
            page: page.clone(),
            mode: FetchMode::Acquire,
        };
        let (signed, binding) = a.sign_request(request).unwrap();
        let admitted = b.verify_request(signed).unwrap();
        let metadata = ObjectMetadata {
            version: page.version.clone(),
            length: 7,
            expires_at: ExpiresAt(std::time::SystemTime::now()),
        };
        let ciphertext = CiphertextPage {
            inner: Arc::new(CiphertextBytes {
                envelope: PageEnvelope {
                    page,
                    key_id: KeyId([6; 16]),
                    nonce: Nonce([7; 24]),
                    plaintext_length: 7,
                    ciphertext_length: 23,
                },
                bytes: vec![0; 23],
                reservation: self::request(5).origin.reservation,
            }),
        };
        let response = b
            .sign_response(
                admitted.binding(),
                PeerResponse::Page {
                    metadata: metadata.clone(),
                    ciphertext: ciphertext.clone(),
                },
            )
            .unwrap();
        for change in 0..6 {
            let mut m = metadata.clone();
            let mut envelope = ciphertext.envelope().clone();
            match change {
                0 => envelope.key_id.0[0] ^= 1,
                1 => envelope.nonce.0[0] ^= 1,
                2 => envelope.plaintext_length += 1,
                3 => envelope.ciphertext_length += 1,
                4 => m.length += 1,
                _ => m.expires_at.0 += Duration::from_millis(1),
            }
            let body = CiphertextPage {
                inner: Arc::new(CiphertextBytes {
                    envelope,
                    bytes: vec![0; 23],
                    reservation: self::request(5).origin.reservation,
                }),
            };
            let tampered = SignedResponse {
                authentication: ForwardedHead {
                    original: response.authentication.original.clone(),
                    hops: Vec::new(),
                },
                response: PeerResponse::Page {
                    metadata: m,
                    ciphertext: body,
                },
            };
            assert!(
                a.verify_response(tampered, &binding).is_err(),
                "accepted page field {change}"
            );
        }
        a.verify_response(response, &binding).unwrap();
    }
    #[test]
    fn validly_signed_page_must_match_the_requested_version_and_page() {
        use crate::{
            memory::pool::{CiphertextBytes, CiphertextPage},
            model::{
                envelope::PageEnvelope,
                metadata::{ExpiresAt, ObjectMetadata},
                range::PAGE_BYTES,
            },
        };
        let signatures = network(3);
        let sender = Forwarding::new(signatures[0].clone());
        let mut local = request(1);
        let requested = PageId {
            version: ObjectVersion {
                object: local.origin.object.clone(),
                etag: StrongEtag::parse(b"\"version\"").unwrap(),
            },
            number: PageNumber(0),
        };
        local.operation = Operation::Page {
            page: requested.clone(),
            mode: FetchMode::CopyOnly,
        };
        let (_, binding) = sender.sign_request(local).unwrap();
        for change in 0..2 {
            let mut page = requested.clone();
            if change == 0 {
                page.version.etag = StrongEtag::parse(b"\"another-version\"").unwrap();
            } else {
                page.number = PageNumber(1);
            }
            let metadata = ObjectMetadata {
                version: page.version.clone(),
                length: page.number.0 * PAGE_BYTES + 3,
                expires_at: ExpiresAt(std::time::SystemTime::now()),
            };
            let ciphertext = CiphertextPage {
                inner: Arc::new(CiphertextBytes {
                    envelope: PageEnvelope {
                        page,
                        key_id: KeyId([3; 16]),
                        nonce: Nonce([4; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    bytes: vec![0; 19],
                    reservation: request(1).origin.reservation,
                }),
            };
            let response = PeerResponse::Page {
                metadata,
                ciphertext,
            };
            let mut head = protocol::response_head(
                &response,
                &signed_digest(&binding.original).unwrap(),
                &[node(0), node(2)],
            )
            .unwrap();
            push(&mut head, "racer-receiver", node(0).0);
            let signed = SignedResponse {
                authentication: ForwardedHead {
                    original: Arc::new(signatures[2].sign(head).unwrap()),
                    hops: Vec::new(),
                },
                response,
            };
            assert!(sender.verify_response(signed, &binding).is_err());
        }
    }
    #[test]
    fn exact_request_agreement_credentials_mode_identity_and_route() {
        let signatures = network(3);
        let f: Vec<_> = signatures
            .iter()
            .map(|s| Forwarding::new(s.clone()))
            .collect();
        for change in 0..10 {
            let (mut signed, _) = f[0].sign_request_to(request(1), &node(1)).unwrap();
            match change {
                0 => {
                    if let Operation::Metadata { mode, .. } = &mut signed.request.operation {
                        *mode = FetchMode::Acquire;
                    }
                }
                1 => signed.request.origin.metadata = None,
                2 => {
                    signed
                        .request
                        .origin
                        .authorization
                        .as_mut()
                        .unwrap()
                        .ciphertext[0] ^= 1
                }
                3 => {
                    signed
                        .request
                        .origin
                        .authorization
                        .as_mut()
                        .unwrap()
                        .nonce
                        .0[0] ^= 1
                }
                4 => {
                    signed
                        .request
                        .origin
                        .authorization
                        .as_mut()
                        .unwrap()
                        .key_id
                        .0[0] ^= 1
                }
                5 => signed.request.route.membership.0 += 1,
                6 => signed.request.route.remaining_links += 1,
                7 => signed.request.route.deadline.0 += Duration::from_secs(1),
                8 => {
                    if let Operation::Metadata { selector, .. } = &mut signed.request.operation {
                        *selector = MetadataSelector::Fresh;
                    }
                }
                _ => signed.request.origin.object.key.0[0] ^= 1,
            }
            assert!(
                f[1].verify_request(signed).is_err(),
                "accepted logical mutation {change}"
            );
        }
    }
    #[test]
    fn route_budget_extensions_loops_and_hop_chain_substitution_fail() {
        let signatures = network(3);
        let f: Vec<_> = signatures
            .iter()
            .map(|s| Forwarding::new(s.clone()))
            .collect();
        for change in 0..5 {
            let (signed, _) = f[0].sign_request_to(request(1), &node(1)).unwrap();
            let admitted = f[1].verify_request(signed).unwrap();
            let mut route = budget(&admitted);
            match change {
                0 => route.remaining_links += 1,
                1 => route.deadline.0 += Duration::from_secs(1),
                2 => route.visited.clear(),
                3 => route.destination = node(0),
                _ => route.attempt.0[0] ^= 1,
            }
            assert!(f[1].append_request(admitted, &node(2), route).is_err());
        }
        let (first, _) = f[0].sign_request_to(request(1), &node(1)).unwrap();
        let (second, _) = f[0].sign_request_to(request(1), &node(1)).unwrap();
        let admitted = f[1].verify_request(first).unwrap();
        let route = budget(&admitted);
        let mut forwarded = f[1].append_request(admitted, &node(2), route).unwrap();
        for h in &forwarded.authentication.hops[0].head.headers {
            let mut logical = request(1);
            logical.route = RouteBudget {
                membership: forwarded.request.route.membership,
                request: forwarded.request.route.request,
                attempt: forwarded.request.route.attempt,
                destination: forwarded.request.route.destination.clone(),
                visited: forwarded.request.route.visited.clone(),
                remaining_links: forwarded.request.route.remaining_links,
                deadline: forwarded.request.route.deadline,
            };
            let mut hop = clone_head(&forwarded.authentication.hops[0]);
            hop.head
                .headers
                .iter_mut()
                .find(|v| v.name == h.name)
                .unwrap()
                .value
                .push(b'x');
            assert!(
                f[2].verify_request(SignedRequest {
                    authentication: ForwardedHead {
                        original: forwarded.authentication.original.clone(),
                        hops: vec![hop]
                    },
                    request: logical,
                })
                .is_err(),
                "accepted forward-hop mutation {}",
                h.name
            );
        }
        forwarded.authentication.original = second.authentication.original;
        assert!(f[2].verify_request(forwarded).is_err());
        let mut exhausted = request(1);
        exhausted.route.remaining_links = 1;
        let (signed, _) = f[0].sign_request_to(exhausted, &node(1)).unwrap();
        let admitted = f[1].verify_request(signed).unwrap();
        let route = budget(&admitted);
        assert!(f[1].append_request(admitted, &node(2), route).is_err());
    }
    #[test]
    fn successful_metadata_must_match_original_object_pin_and_operation() {
        use crate::model::metadata::{ExpiresAt, ObjectMetadata};
        let signatures = network(3);
        let f: Vec<_> = signatures
            .iter()
            .map(|s| Forwarding::new(s.clone()))
            .collect();
        let (signed, _) = f[0].sign_request(request(1)).unwrap();
        let admitted = f[2].verify_request(signed).unwrap();
        let good = ObjectMetadata {
            version: ObjectVersion {
                object: admitted.request().origin.object.clone(),
                etag: StrongEtag::parse(b"\"version\"").unwrap(),
            },
            length: 20,
            expires_at: ExpiresAt(std::time::SystemTime::now()),
        };
        f[2].sign_response(admitted.binding(), PeerResponse::Metadata(good.clone()))
            .unwrap();
        let mut bad = good.clone();
        bad.version.etag = StrongEtag::parse(b"\"other\"").unwrap();
        assert!(
            f[2].sign_response(admitted.binding(), PeerResponse::Metadata(bad))
                .is_err()
        );
        let mut bad = good;
        bad.version.object.key.0[0] ^= 1;
        assert!(
            f[2].sign_response(admitted.binding(), PeerResponse::Metadata(bad))
                .is_err()
        );
    }
    #[test]
    fn relay_cannot_assert_success_but_can_return_request_bound_errors() {
        use crate::model::metadata::{ExpiresAt, ObjectMetadata};
        let signatures = network(3);
        let requester = Forwarding::new(signatures[0].clone());
        let relay = Forwarding::new(signatures[1].clone());
        let (signed, binding) = requester.sign_request_to(request(1), &node(1)).unwrap();
        let admitted = relay.verify_request(signed).unwrap();
        let metadata = ObjectMetadata {
            version: ObjectVersion {
                object: admitted.request().origin.object.clone(),
                etag: StrongEtag::parse(b"\"version\"").unwrap(),
            },
            length: 20,
            expires_at: ExpiresAt(std::time::SystemTime::now()),
        };
        assert!(
            relay
                .sign_response(admitted.binding(), PeerResponse::Metadata(metadata.clone()))
                .is_err()
        );
        // Bypass the honest signer facade to simulate a malicious certified relay.
        let response = PeerResponse::Metadata(metadata);
        let mut head = protocol::response_head(
            &response,
            &signed_digest(&binding.original).unwrap(),
            &[node(0), node(1)],
        )
        .unwrap();
        push(&mut head, "racer-receiver", node(0).0);
        let forged_success = SignedResponse {
            authentication: ForwardedHead {
                original: Arc::new(signatures[1].sign(head).unwrap()),
                hops: Vec::new(),
            },
            response,
        };
        assert!(requester.verify_response(forged_success, &binding).is_err());
        let error = relay
            .sign_response(admitted.binding(), PeerResponse::Unavailable)
            .unwrap();
        requester.verify_response(error, &binding).unwrap();
    }
    #[test]
    fn page_descriptor_range_nonce_and_exact_request_binding() {
        use crate::{
            memory::pool::{CiphertextBytes, CiphertextPage},
            model::{
                envelope::PageEnvelope,
                metadata::{ExpiresAt, ObjectMetadata},
            },
        };
        let signatures = network(3);
        let requester = Forwarding::new(signatures[0].clone());
        let server = Forwarding::new(signatures[2].clone());
        let mut local = request(1);
        let page = PageId {
            version: ObjectVersion {
                object: local.origin.object.clone(),
                etag: StrongEtag::parse(b"\"version\"").unwrap(),
            },
            number: PageNumber(0),
        };
        local.operation = Operation::Page {
            page: page.clone(),
            mode: FetchMode::Acquire,
        };
        let (signed, binding) = requester.sign_request(local).unwrap();
        let admitted = server.verify_request(signed).unwrap();
        let metadata = ObjectMetadata {
            version: page.version.clone(),
            length: 3,
            expires_at: ExpiresAt(std::time::SystemTime::now()),
        };
        let envelope = PageEnvelope {
            page: page.clone(),
            key_id: KeyId([6; 16]),
            nonce: Nonce([8; 24]),
            plaintext_length: 3,
            ciphertext_length: 19,
        };
        let pool_request = request(2);
        let reservation = pool_request.origin.reservation;
        // A charged wire ciphertext is unverified page data; signing does not
        // authenticate its body. Only the signed descriptor is consumed here.
        let ciphertext = CiphertextPage {
            inner: Arc::new(CiphertextBytes {
                envelope: envelope.clone(),
                bytes: vec![0; 19],
                reservation,
            }),
        };
        let response = server
            .sign_response(
                admitted.binding(),
                PeerResponse::Page {
                    metadata: metadata.clone(),
                    ciphertext: ciphertext.clone(),
                },
            )
            .unwrap();
        assert_eq!(
            field(&response.authentication.original.head, "content-range").unwrap(),
            "bytes 0-2/3"
        );
        let auth = response.authentication;
        for change in 0..5 {
            let mut e = envelope.clone();
            let mut m = metadata.clone();
            match change {
                0 => e.nonce.0[0] ^= 1,
                1 => e.key_id.0[0] ^= 1,
                2 => m.expires_at.0 += Duration::from_secs(1),
                3 => m.length += 1,
                _ => e.page.number.0 += 1,
            }
            let reservation = request(3).origin.reservation;
            let bad = CiphertextPage {
                inner: Arc::new(CiphertextBytes {
                    envelope: e,
                    bytes: vec![0; 19],
                    reservation,
                }),
            };
            let response = SignedResponse {
                authentication: ForwardedHead {
                    original: auth.original.clone(),
                    hops: Vec::new(),
                },
                response: PeerResponse::Page {
                    metadata: m,
                    ciphertext: bad,
                },
            };
            assert!(requester.verify_response(response, &binding).is_err());
        }
        requester
            .verify_response(
                SignedResponse {
                    authentication: auth,
                    response: PeerResponse::Page {
                        metadata,
                        ciphertext,
                    },
                },
                &binding,
            )
            .unwrap();
    }
    #[test]
    fn deadline_roundtrip_and_verified_mailbox_ownership() {
        let deadline = crate::runtime::deadline::Deadline(Instant::now() + Duration::from_secs(20));
        let value = protocol::encode_deadline(deadline).unwrap();
        let decoded = protocol::decode_deadline(value).unwrap();
        assert_eq!(protocol::encode_deadline(decoded).unwrap(), value);
        assert!(decoded.0 <= deadline.0);
        fn send<T: Send>() {}
        send::<VerifiedRequest>();
        send::<VerifiedResponse>();
        send::<RequestBinding>();
    }
}
