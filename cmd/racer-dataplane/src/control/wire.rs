//! HTTPS/JSON v1 and projected bundle DTOs. Encoding/decoding is not implemented.
//! See CONTROL_API.md for field encoding, bounds, authentication, and retry policy.
use super::caches::CacheDefinition;
use crate::{
    model::{
        envelope::KeyId,
        identity::{CacheId, ClusterId, MembershipVersion, NodeId},
    },
    topology::membership::Member,
};
use std::time::Duration;

pub const SCHEMA_VERSION: u32 = 1;
pub const ENROLL_PATH: &str = "/v1/enroll";
pub const SNAPSHOT_PATH: &str = "/v1/snapshot";
pub const TOKEN_AUDIENCE: &str = "racer-control";
pub const MAX_ENROLLMENT_BYTES: usize = 64 * 1024;
// Leave room below Kubernetes' Secret size limit for projection metadata.
pub const MAX_BUNDLE_BYTES: usize = 512 * 1024;
pub const MAX_PUBLICATION_BYTES: usize = 64 * 1024 * 1024;
pub const MAX_MEMBERS: usize = 100_000;
pub const POLL_WAIT: Duration = Duration::from_secs(30);
pub const RETRY_MIN: Duration = Duration::from_secs(1);
pub const RETRY_MAX: Duration = Duration::from_secs(30);
pub const CERTIFICATE_LIFETIME: Duration = Duration::from_secs(24 * 60 * 60);
pub const RENEW_AFTER: Duration = Duration::from_secs(16 * 60 * 60);

pub const SHARES_ANNOTATION: &str = "racer.unbounded-cloud.io/shares";
pub const RAILS_ANNOTATION: &str = "racer.unbounded-cloud.io/rails";
pub const ALIGNMENT_ANNOTATION: &str = "racer.unbounded-cloud.io/aligned-rails";
pub const EXCLUSION_LABEL: &str = "racer.unbounded-cloud.io/exclude";
pub const DEFAULT_SHARES: u32 = 4;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct PublicationSequence(pub u64);
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct BundleGeneration(pub u64);
/// Canonical UUID text on the wire; opaque idempotency identity, not a key ID.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EnrollmentId(pub String);

/// No cursor means immediate bootstrap; otherwise serialize as the `after` query.
pub struct SnapshotRequest {
    pub after: Option<PublicationSequence>,
}
pub enum SnapshotResponse {
    Updated(Publication),
    Unchanged,
}
pub struct Publication {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership_version: MembershipVersion,
    pub members: Vec<Member>,
    pub caches: Vec<CacheDefinition>,
}

/// Bearer token is supplied by the transport from the projected token path,
/// never embedded in a DTO or retained in diagnostics.
pub struct EnrollmentRequest {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub node: NodeId,
    pub enrollment: EnrollmentId,
    pub csr_der: Vec<u8>,
}
pub struct EnrollmentReceipt {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub node: NodeId,
    pub enrollment: EnrollmentId,
}

/// Receipt does not deliver a certificate: only the mounted bundle activates it.
pub struct NodeCertificate {
    pub enrollment: EnrollmentId,
    pub state: CertificateState,
    pub certificate_chain: Vec<Vec<u8>>,
}
pub enum CertificateState {
    Pending,
    Active,
    Retiring,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CacheKeyPurpose {
    Page,
    OriginCredentials,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CacheKeyState {
    Prepared,
    Active,
    Retiring,
}
/// Cache-scoped reference prevents retirement of an unrelated key or purpose.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheKeyRef {
    pub cache: CacheId,
    pub id: KeyId,
    pub purpose: CacheKeyPurpose,
}
/// Deliberately non-Debug. Decode only into a bounded, staged credential bundle.
pub struct CacheEncryptionKey {
    pub key: CacheKeyRef,
    pub state: CacheKeyState,
    pub(crate) material: [u8; 32],
}
/// One node-specific bundle.json from one coherent projected directory generation.
/// No signing private keys; missing old encryption keys request local retirement.
pub struct CredentialBundle {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub node: NodeId,
    pub generation: BundleGeneration,
    pub certificates: Vec<NodeCertificate>,
    pub peer_trust_roots: Vec<Vec<u8>>,
    pub cache_keys: Vec<CacheEncryptionKey>,
}

/// Wire errors use snake_case codes and the HTTP statuses specified in CONTROL_API.md.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProtocolFailure {
    InvalidRequest,
    Unauthenticated,
    Forbidden,
    Conflict,
    TooLarge,
    UnsupportedVersion,
    Overloaded,
    Unavailable,
}
pub struct ErrorResponse {
    pub code: ProtocolFailure,
}
#[cfg(test)]
mod tests { /* Unknown versions, excessive bounds, and out-of-order publications. */
}
