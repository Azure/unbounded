//! Generate node-private Ed25519 keys locally and enroll/rotate with node-bound SA identity.
use super::wire::{EnrollmentId, EnrollmentRequest, EnrollmentResponse};
use crate::{
    error::{Operation, Result, deferred, pending},
    model::identity::{ClusterId, NodeId},
    runtime::deadline::RequestScope,
};
use std::path::PathBuf;
pub struct Enrollment {
    cluster: ClusterId,
    token_path: PathBuf,
    identity_directory: PathBuf,
}
/// Non-exportable signing identity. Do not share private keys through cluster Secrets.
pub struct LocalSigningIdentity {
    node: NodeId,
    enrollment: EnrollmentId,
    private_material: Vec<u8>,
    certificate_chain: Vec<Vec<u8>>,
}
impl Enrollment {
    pub fn new(cluster: ClusterId, token_path: PathBuf, identity_directory: PathBuf) -> Self {
        Self {
            cluster,
            token_path,
            identity_directory,
        }
    }
    /// Persist a fresh private key and retry-stable request before submission.
    /// All issuance uses the projected token, including renewal at 16 hours.
    pub fn prepare<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, EnrollmentRequest> {
        deferred("enrollment.prepare")
    }
    /// Verify server-authenticated response correlation, chain/SAN/validity, and
    /// local key pairing, then persist the node identity. Never trust CSR SANs.
    pub fn accept_response(&self, _response: EnrollmentResponse) -> Result<LocalSigningIdentity> {
        pending("enrollment.accept_response")
    }
}
impl LocalSigningIdentity {
    pub fn node(&self) -> &NodeId {
        &self.node
    }
}
#[cfg(test)]
mod tests { /* Node binding, wrong certificate pairing, renewal failure, and rotation. */
}
