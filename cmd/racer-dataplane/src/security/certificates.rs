//! Trust-chain, node-binding, and validity checks before peer work admission.
use super::keyring::Keyring;
use crate::{
    error::{Result, pending},
    model::identity::{ClusterId, NodeId},
};
use std::rc::Rc;
pub struct Certificates {
    cluster: ClusterId,
    keys: Rc<Keyring>,
}
pub struct VerifiedPeer {
    node: NodeId,
}
impl VerifiedPeer {
    pub fn node(&self) -> &NodeId {
        &self.node
    }
}
impl Certificates {
    /// Peer trust follows coherent mounted epochs, not deployment bootstrap trust.
    pub fn new(cluster: ClusterId, keys: Rc<Keyring>) -> Self {
        Self { cluster, keys }
    }
    pub fn verify(&self, _chain: &[Vec<u8>], _expected: &NodeId) -> Result<VerifiedPeer> {
        pending("certificates.verify")
    }
}
#[cfg(test)]
mod tests { /* Wrong node, expired trust, revoked credentials, and chain bounds. */
}
