//! Two HTTPS operations: server-authenticated enrollment and mTLS snapshot polling.
//! One bounded long poll, durable cursor, jittered retry; no node/status reporting.
use super::{
    caches::CacheRegistry,
    enrollment::Enrollment,
    secrets::SecretWatcher,
    snapshot::SnapshotStore,
    transport::ControlTransport,
    wire::{EnrollmentRequest, EnrollmentResponse, SnapshotRequest, SnapshotResponse},
};
use crate::{
    error::{Operation, deferred},
    runtime::deadline::RequestScope,
    security::keyring::Keyring,
};
use std::{path::PathBuf, rc::Rc};
pub struct ControlEndpoint {
    pub url: String,
    /// Deployment-provided trust, independent of rotating peer trust roots.
    pub trust_bundle: PathBuf,
}
pub struct ControlClient {
    transport: ControlTransport,
    enrollment: Rc<Enrollment>,
    keys: Rc<Keyring>,
    secrets: SecretWatcher,
    snapshots: Rc<SnapshotStore>,
    caches: Rc<CacheRegistry>,
}
impl ControlClient {
    pub fn new(
        endpoint: ControlEndpoint,
        enrollment: Rc<Enrollment>,
        keys: Rc<Keyring>,
        secrets: SecretWatcher,
        snapshots: Rc<SnapshotStore>,
        caches: Rc<CacheRegistry>,
    ) -> Self {
        Self {
            transport: ControlTransport::new(endpoint),
            enrollment,
            keys,
            secrets,
            snapshots,
            caches,
        }
    }
    /// Reads the projected token at submission; retries reuse the same request ID.
    pub fn enroll<'a>(
        &'a self,
        _request: &'a EnrollmentRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, EnrollmentResponse> {
        deferred("control.enroll")
    }
    /// Requires the locally activated certificate and matching local signing key.
    /// New/pooled TLS connections must not outlive client certificate validity.
    pub fn poll<'a>(
        &'a self,
        _request: SnapshotRequest,
        _scope: &'a RequestScope,
    ) -> Operation<'a, SnapshotResponse> {
        deferred("control.poll")
    }
    pub fn run<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("control.run")
    }
}
#[cfg(test)]
mod tests { /* Token-authenticated renewal, mTLS polling, reconnect, expired state. */
}
