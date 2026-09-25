//! Server-authenticated HTTPS for issuance and mTLS for snapshot polling.
//! No control HTTP signatures, application challenge, or shared-key encryption.
use super::{client::ControlEndpoint, enrollment::LocalSigningIdentity};
use crate::{
    error::{Operation, deferred},
    runtime::deadline::RequestScope,
};

pub struct ControlTransport {
    endpoint: ControlEndpoint,
}
/// Owns a bounded TLS connection. Actual reactor/TLS integration remains deferred.
pub struct ControlConnection;
impl ControlTransport {
    pub fn new(endpoint: ControlEndpoint) -> Self {
        Self { endpoint }
    }
    /// No client certificate: also supports recovery from an expired identity.
    pub fn bootstrap<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ControlConnection> {
        deferred("control_transport.bootstrap")
    }
    /// Verify server name/trust and use the local certificate/key. Bound pooled
    /// connection lifetime by certificate validity and drain on identity rotation.
    pub fn authenticated<'a>(
        &'a self,
        _identity: &'a LocalSigningIdentity,
        _scope: &'a RequestScope,
    ) -> Operation<'a, ControlConnection> {
        deferred("control_transport.authenticated")
    }
}
