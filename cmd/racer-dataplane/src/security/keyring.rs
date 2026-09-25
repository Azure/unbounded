//! Active/retiring key epochs and lifetime barriers across memory, disk, and I/O.
use crate::{
    error::{Result, pending},
    model::{envelope::KeyId, identity::CacheId},
};
/// Node-wide epoch state, published by the sole control owner. Secret material
/// implementation must support immutable leased epochs and coordinated retirement.
pub struct KeyEpochs;
pub struct Keyring {
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
    pub fn new(epochs: std::sync::Arc<KeyEpochs>) -> Self {
        Self { epochs }
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
    pub fn retire(&self, _id: KeyId) -> Result<()> {
        pending("keyring.retire")
    }
}
#[cfg(test)]
mod tests { /* In-flight rotation, dirty/checkpoint retirement, late write rejection. */
}
