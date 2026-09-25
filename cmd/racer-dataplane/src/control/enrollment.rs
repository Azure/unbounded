//! Generate node-private Ed25519 keys locally and enroll/rotate with node-bound SA identity.
use super::wire::EnrollmentResponse;
use crate::{
    error::{Operation, deferred},
    model::identity::NodeId,
};
use std::path::PathBuf;
pub struct Enrollment {
    node: NodeId,
    token_path: PathBuf,
}
/// Non-exportable signing identity. Do not share private keys through cluster Secrets.
pub struct LocalSigningIdentity {
    private_material: Vec<u8>,
}
impl Enrollment {
    pub fn new(node: NodeId, token_path: PathBuf) -> Self {
        Self { node, token_path }
    }
    /// Renew before 24-hour expiry; rotation generates a new private key and CSR.
    pub fn renew(&self) -> Operation<'_, (LocalSigningIdentity, EnrollmentResponse)> {
        deferred("enrollment.renew")
    }
}
#[cfg(test)]
mod tests { /* Node binding, wrong certificate pairing, renewal failure, and rotation. */
}
