//! Active/retiring key epochs and lifetime barriers across memory, disk, and I/O.
use crate::{
    control::{
        enrollment::LocalSigningIdentity,
        wire::{BundleGeneration, CacheKeyRef, KeyringBundle},
    },
    error::{Operation, Result, deferred, pending},
    model::{
        envelope::KeyId,
        identity::{CacheId, ClusterId, NodeId},
    },
    runtime::deadline::RequestScope,
};
/// Node-wide epoch state, published by the sole control owner. Secret material
/// implementation must support immutable leased epochs and coordinated retirement.
pub struct KeyEpochs;
pub struct Keyring {
    cluster: ClusterId,
    node: NodeId,
    epochs: std::sync::Arc<KeyEpochs>,
}
/// Secret material is private, non-Debug, and only accessed through crypto adapters.
pub struct KeyLease {
    id: KeyId,
    material: Vec<u8>,
}
pub enum KeyPurpose {
    Page,
    OriginCredentials,
    NodeSigning,
    PeerVerification,
}
impl Keyring {
    pub fn new(cluster: ClusterId, node: NodeId, epochs: std::sync::Arc<KeyEpochs>) -> Self {
        Self {
            cluster,
            node,
            epochs,
        }
    }
    /// Validate shared bundle cluster/generation before atomic activation.
    /// Missing prior keys request retirement, never immediate deletion. Prepared
    /// keys may decrypt received ciphertext but must not encrypt new fills.
    pub fn install(&self, _bundle: KeyringBundle) -> Result<BundleGeneration> {
        pending("keyring.install")
    }
    /// Activate locally issued signing identity independently of shared-key epochs.
    /// Validate the resolved node and preserve leases on retiring local identities.
    pub fn install_identity(&self, _identity: LocalSigningIdentity) -> Result<()> {
        pending("keyring.install_identity")
    }
    pub fn lease(
        &self,
        _cache: Option<&CacheId>,
        _id: KeyId,
        _purpose: KeyPurpose,
    ) -> Result<KeyLease> {
        pending("keyring.lease")
    }
    pub fn active(&self, _cache: &CacheId, _purpose: KeyPurpose) -> Result<KeyLease> {
        pending("keyring.active")
    }
    /// Coordinate eviction of every dependent record/buffer/checkpoint and fence
    /// late writes before removing an epoch. Request credential leases also drain.
    /// Local completion only. Cancellation retains material until barriers finish;
    /// no control-plane acknowledgment or cluster-wide barrier is involved.
    pub fn retire<'a>(
        &'a self,
        _key: &'a CacheKeyRef,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        deferred("keyring.retire")
    }
}
#[cfg(test)]
mod tests { /* In-flight rotation, dirty/checkpoint retirement, late write rejection. */
}
