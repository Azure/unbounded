//! Authenticate, replay-check, authorize, and admit before local dispatch or relay.
use super::{
    relay::Relay,
    wire::{PeerResponse, SignedRequest, SignedResponse, VerifiedRequest},
};
use crate::{
    error::{Operation, deferred},
    runtime::{admission::Admission, deadline::RequestScope},
    security::forwarding::Forwarding,
};
use std::rc::Rc;
/// Implemented by the existing read coordinator, never a second acquisition graph.
/// Ingress must be verified; the local result is unsigned until the server signs
/// it with a retained clone of the request binding.
///
/// ```compile_fail
/// use racer_dataplane::{peer::{server::LocalPageService, wire::PeerRequest},
///     runtime::deadline::RequestScope};
/// fn unverified(service: &dyn LocalPageService, request: PeerRequest, scope: &RequestScope) {
///     service.serve_peer(request, scope);
/// }
/// ```
pub trait LocalPageService {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse>;
}
pub struct PeerServer {
    io: Rc<crate::http::io::HttpIo>,
    forwarding: Rc<Forwarding>,
    admission: Rc<Admission>,
    local: Rc<dyn LocalPageService>,
    relay: Rc<Relay>,
}
impl PeerServer {
    /// Accept bounded neighbor HTTP connections on the owning reactor. The shared
    /// codec frames input before signatures are verified and operations decoded.
    pub fn listen<'a>(
        &'a self,
        _address: std::net::SocketAddr,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("peer.listen")
    }
    pub fn new(
        io: Rc<crate::http::io::HttpIo>,
        forwarding: Rc<Forwarding>,
        admission: Rc<Admission>,
        local: Rc<dyn LocalPageService>,
        relay: Rc<Relay>,
    ) -> Self {
        Self {
            io,
            forwarding,
            admission,
            local,
            relay,
        }
    }
    /// Verify the complete ingress envelope before service or relay. Sign a local
    /// result against its binding; return a relayed signed result without replacing
    /// the responder's original signature or discarding forwarding headers.
    pub fn dispatch<'a>(
        &'a self,
        _request: SignedRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, SignedResponse> {
        deferred("peer.dispatch")
    }
}
#[cfg(test)]
mod tests { /* Authentication before work, copy-only isolation, signed error replies. */
}
