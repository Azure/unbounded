//! Opaque bounded transit with recorded reverse-path responses, never a page cache.
//!
//! No decryption service is injected here. Reverse-link failure terminates the
//! attempt; responses are not independently rerouted. Preserve encrypted credentials.
use super::{
    requester::PeerTransport,
    wire::{SignedResponse, VerifiedRequest},
};
use crate::{
    error::{Operation, deferred},
    runtime::{admission::Admission, deadline::RequestScope},
    security::forwarding::Forwarding,
    topology::paths::Paths,
};
use std::rc::Rc;
pub struct Relay {
    paths: Rc<Paths>,
    forwarding: Rc<Forwarding>,
    transport: Rc<dyn PeerTransport>,
    admission: Rc<Admission>,
}
impl Relay {
    pub fn new(
        paths: Rc<Paths>,
        forwarding: Rc<Forwarding>,
        transport: Rc<dyn PeerTransport>,
        admission: Rc<Admission>,
    ) -> Self {
        Self {
            paths,
            forwarding,
            transport,
            admission,
        }
    }
    /// Retain ingress binding and reverse path, append a signed request hop, and
    /// exchange complete envelopes. Verify the downstream response against that
    /// binding before appending a reverse hop. Never re-sign the original response.
    ///
    /// ```compile_fail
    /// use racer_dataplane::{peer::{relay::Relay, wire::SignedRequest},
    ///     runtime::deadline::RequestScope};
    /// fn unverified(relay: &Relay, request: SignedRequest, scope: &RequestScope) {
    ///     relay.forward(request, scope);
    /// }
    /// ```
    pub fn forward<'a>(
        &'a self,
        _request: VerifiedRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        deferred("peer.relay")
    }
}
#[cfg(test)]
mod tests { /* Transit-only buffers, reverse-link failure, budgets, opaque credentials. */
}
