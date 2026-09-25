//! Authenticate, replay-check, authorize, and admit before local dispatch or relay.
use super::{
    relay::Relay,
    wire::{PeerRequest, PeerResponse},
};
use crate::{
    error::{Operation, deferred},
    runtime::{admission::Admission, deadline::RequestScope},
    security::signing::{Signatures, SignedHead},
};
use std::rc::Rc;
/// Implemented by the existing read coordinator, never a second acquisition graph.
pub trait LocalPageService {
    fn serve_peer<'a>(
        &'a self,
        request: PeerRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse>;
}
pub struct PeerServer {
    io: Rc<crate::http::io::HttpIo>,
    signatures: Rc<Signatures>,
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
        signatures: Rc<Signatures>,
        admission: Rc<Admission>,
        local: Rc<dyn LocalPageService>,
        relay: Rc<Relay>,
    ) -> Self {
        Self {
            io,
            signatures,
            admission,
            local,
            relay,
        }
    }
    pub fn dispatch<'a>(
        &'a self,
        _head: SignedHead,
        _request: PeerRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        deferred("peer.dispatch")
    }
}
#[cfg(test)]
mod tests { /* Authentication before work, copy-only isolation, signed error replies. */
}
