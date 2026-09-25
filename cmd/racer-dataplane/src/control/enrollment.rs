//! Generate node-private Ed25519 keys locally and enroll/rotate with node-bound SA identity.
use super::wire::{EnrollmentId, EnrollmentReceipt, EnrollmentRequest, NodeCertificate};
use crate::{
    error::{Operation, Result, deferred, pending},
    model::identity::{ClusterId, NodeId},
    runtime::deadline::RequestScope,
};
use std::path::PathBuf;
pub struct Enrollment {
    cluster: ClusterId,
    node: NodeId,
    token_path: PathBuf,
    identity_directory: PathBuf,
}
/// Non-exportable signing identity. Do not share private keys through cluster Secrets.
pub struct LocalSigningIdentity {
    enrollment: EnrollmentId,
    private_material: Vec<u8>,
}
impl Enrollment {
    pub fn new(
        cluster: ClusterId,
        node: NodeId,
        token_path: PathBuf,
        identity_directory: PathBuf,
    ) -> Self {
        Self {
            cluster,
            node,
            token_path,
            identity_directory,
        }
    }
    /// Persist a fresh private key and retry-stable request before submission.
    /// Begin renewal at 16 hours; bootstrap/recovery also use this operation.
    pub fn prepare<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, EnrollmentRequest> {
        deferred("enrollment.prepare")
    }
    /// Validate receipt identity/idempotency; this never activates a certificate.
    pub fn accept_receipt(&self, _receipt: EnrollmentReceipt) -> Result<()> {
        pending("enrollment.accept_receipt")
    }
    /// Pair a staged mounted certificate with its persisted node-private key.
    pub fn identity_for(&self, _certificate: &NodeCertificate) -> Result<LocalSigningIdentity> {
        pending("enrollment.identity_for")
    }
}
#[cfg(test)]
mod tests { /* Node binding, wrong certificate pairing, renewal failure, and rotation. */
}
