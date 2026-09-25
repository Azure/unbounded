//! Correlated logical requests with monotonic budgets, attempts, and cancellation.
use super::{
    handshake::Handshake,
    transfer::Transfers,
    wire::{PeerRequest, PeerResponse},
};
use crate::{
    error::{Operation, deferred},
    runtime::deadline::RequestScope,
    security::signing::Signatures,
    topology::{paths::Paths, rails::Rails},
};
use std::rc::Rc;
pub trait PeerClient {
    fn request<'a>(
        &'a self,
        request: PeerRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse>;
}
pub struct Requester {
    paths: Rc<Paths>,
    rails: Rc<Rails>,
    signatures: Rc<Signatures>,
    handshake: Rc<Handshake>,
    transfers: Rc<Transfers>,
}
impl Requester {
    pub fn new(
        paths: Rc<Paths>,
        rails: Rc<Rails>,
        signatures: Rc<Signatures>,
        handshake: Rc<Handshake>,
        transfers: Rc<Transfers>,
    ) -> Self {
        Self {
            paths,
            rails,
            signatures,
            handshake,
            transfers,
        }
    }
}
impl PeerClient for Requester {
    fn request<'a>(
        &'a self,
        _request: PeerRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        deferred("peer.request")
    }
}
#[cfg(test)]
mod tests { /* Late attempts, fresh replay nonce per send, retry budgets, cancellation. */
}
