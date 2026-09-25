//! Trust-chain, node-binding, and validity checks before peer work admission.
use crate::{
    error::{Result, pending},
    model::identity::NodeId,
};
use std::path::PathBuf;
pub struct Certificates {
    trust_bundle: PathBuf,
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
    pub fn new(trust_bundle: PathBuf) -> Self {
        Self { trust_bundle }
    }
    pub fn verify(&self, _chain: &[Vec<u8>], _expected: &NodeId) -> Result<VerifiedPeer> {
        pending("certificates.verify")
    }
}
#[cfg(test)]
mod tests { /* Wrong node, expired trust, revoked credentials, and chain bounds. */
}
