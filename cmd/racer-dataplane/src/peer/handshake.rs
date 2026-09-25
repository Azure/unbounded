//! Certificate-authenticated capabilities and signed RDMA setup over HTTP.
use crate::{
    error::{Operation, deferred},
    model::identity::NodeId,
    rdma::session::Sessions,
    security::signing::Signatures,
};
use std::rc::Rc;
pub struct Handshake {
    signatures: Rc<Signatures>,
    rdma: Option<Rc<Sessions>>,
}
pub struct Capabilities {
    pub rdma: bool,
    pub scoped_grants: bool,
}
impl Handshake {
    pub fn new(signatures: Rc<Signatures>, rdma: Option<Rc<Sessions>>) -> Self {
        Self { signatures, rdma }
    }
    pub fn negotiate<'a>(&'a self, _peer: &'a NodeId) -> Operation<'a, Capabilities> {
        deferred("peer.handshake")
    }
}
#[cfg(test)]
mod tests { /* Capability tampering, QP/rail binding, expired certs, downgrade policy. */
}
