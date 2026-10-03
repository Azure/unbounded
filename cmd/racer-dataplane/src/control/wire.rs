//! Application adapters for the independent Racer control wire contract.
//!
//! Runtime membership and cache installation remain application-owned.
use super::state::CacheDefinition;
use crate::{
    error::{Error, Result},
    model::{ClusterId, MembershipVersion},
    topology::{membership::Member, rails::RailMapping},
};
use racer_control_wire as contract;
pub use racer_control_wire::{
    ALIGNMENT_ANNOTATION, BOOTSTRAP_PATH, BundleGeneration, CERTIFICATE_LIFETIME,
    CacheEncryptionKey, CacheKeyPurpose, CacheKeyRef, CacheKeyState, DEFAULT_SHARES,
    EXCLUSION_LABEL, EnrollmentId, EnrollmentResponse, ErrorResponse, KEYRING_PATH, KeyringBundle,
    MAX_BUNDLE_BYTES, MAX_ENROLLMENT_BYTES, MAX_MEMBERS, MAX_PUBLICATION_BYTES, POLL_WAIT,
    ProtocolFailure, PublicationSequence, RAILS_ANNOTATION, RENEW_AFTER, RETRY_MAX, RETRY_MIN,
    SCHEMA_VERSION, SHARES_ANNOTATION, SNAPSHOT_PATH, SnapshotRequest, TOKEN_AUDIENCE, valid_uuid,
};

pub enum SnapshotResponse {
    Updated(Publication),
    Unchanged,
}
#[derive(Clone)]
pub struct Publication {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership_version: MembershipVersion,
    pub members: Vec<Member>,
    pub caches: Vec<CacheDefinition>,
}
#[derive(Clone)]
pub struct EnrollmentRequest {
    pub shares: u32,
    pub rdma_nics: Vec<RailMapping>,
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub enrollment: EnrollmentId,
    pub csr_der: Vec<u8>,
}

impl From<contract::Error> for Error {
    fn from(error: contract::Error) -> Self {
        match error {
            contract::Error::InvalidRequest => Self::InvalidRequest,
            contract::Error::IncompatibleMembership => Self::IncompatibleMembership,
            contract::Error::Overloaded => Self::Overloaded,
            contract::Error::Replay => Self::Replay,
        }
    }
}

impl From<contract::RailMapping> for RailMapping {
    fn from(value: contract::RailMapping) -> Self {
        Self {
            rail: crate::topology::rails::RailId(value.rail.0),
            device: value.device,
            port: value.port,
            gid: value.gid,
            numa_node: value.numa_node,
        }
    }
}
impl From<RailMapping> for contract::RailMapping {
    fn from(value: RailMapping) -> Self {
        Self {
            rail: contract::RailId(value.rail.0),
            device: value.device,
            port: value.port,
            gid: value.gid,
            numa_node: value.numa_node,
        }
    }
}
impl From<contract::Member> for Member {
    fn from(value: contract::Member) -> Self {
        Self {
            node: value.node,
            shares: value.shares,
            peer_endpoint: value.peer_endpoint,
            rails: value.rails.into_iter().map(Into::into).collect(),
            site: value.site,
        }
    }
}
impl From<Member> for contract::Member {
    fn from(value: Member) -> Self {
        Self {
            node: value.node,
            shares: value.shares,
            peer_endpoint: value.peer_endpoint,
            rails: value.rails.into_iter().map(Into::into).collect(),
            site: value.site,
        }
    }
}
impl From<contract::CacheDefinition> for CacheDefinition {
    fn from(value: contract::CacheDefinition) -> Self {
        Self {
            id: value.id,
            name: value.name,
            client_socket: value.client_socket,
            origin_socket: value.origin_socket,
        }
    }
}
impl From<CacheDefinition> for contract::CacheDefinition {
    fn from(value: CacheDefinition) -> Self {
        Self {
            id: value.id,
            name: value.name,
            client_socket: value.client_socket,
            origin_socket: value.origin_socket,
        }
    }
}
impl From<contract::Publication> for Publication {
    fn from(value: contract::Publication) -> Self {
        Self {
            schema_version: value.schema_version,
            cluster: value.cluster,
            sequence: value.sequence,
            membership_version: value.membership_version,
            members: value.members.into_iter().map(Into::into).collect(),
            caches: value.caches.into_iter().map(Into::into).collect(),
        }
    }
}
impl From<Publication> for contract::Publication {
    fn from(value: Publication) -> Self {
        Self {
            schema_version: value.schema_version,
            cluster: value.cluster,
            sequence: value.sequence,
            membership_version: value.membership_version,
            members: value.members.into_iter().map(Into::into).collect(),
            caches: value.caches.into_iter().map(Into::into).collect(),
        }
    }
}
impl From<contract::EnrollmentRequest> for EnrollmentRequest {
    fn from(value: contract::EnrollmentRequest) -> Self {
        Self {
            shares: value.shares,
            rdma_nics: value.rdma_nics.into_iter().map(Into::into).collect(),
            schema_version: value.schema_version,
            cluster: value.cluster,
            enrollment: value.enrollment,
            csr_der: value.csr_der,
        }
    }
}
impl From<EnrollmentRequest> for contract::EnrollmentRequest {
    fn from(value: EnrollmentRequest) -> Self {
        Self {
            shares: value.shares,
            rdma_nics: value.rdma_nics.into_iter().map(Into::into).collect(),
            schema_version: value.schema_version,
            cluster: value.cluster,
            enrollment: value.enrollment,
            csr_der: value.csr_der,
        }
    }
}

pub fn decode_publication(bytes: &[u8]) -> Result<Publication> {
    Ok(contract::decode_publication(bytes)?.into())
}
pub fn encode_publication(value: &Publication) -> Result<Vec<u8>> {
    Ok(contract::encode_publication(&value.clone().into())?)
}
pub(crate) fn validate_publication(value: &Publication) -> Result<Publication> {
    Ok(contract::validate_publication(&value.clone().into())?.into())
}
pub fn canonical_content(value: &Publication) -> Result<(Vec<u8>, Vec<u8>)> {
    Ok(contract::canonical_content(&value.clone().into())?)
}
pub fn content_hash(value: &Publication) -> Result<String> {
    Ok(contract::content_hash(&value.clone().into())?)
}
pub fn apply_delta(base: &Publication, bytes: &[u8]) -> Result<Publication> {
    Ok(contract::apply_delta(&base.clone().into(), bytes)?.into())
}
pub fn decode_enrollment_request(bytes: &[u8]) -> Result<EnrollmentRequest> {
    Ok(contract::decode_enrollment_request(bytes)?.into())
}
pub fn encode_enrollment_request(value: &EnrollmentRequest) -> Result<Vec<u8>> {
    Ok(contract::encode_enrollment_request(&value.clone().into())?)
}
pub fn decode_enrollment_response(bytes: &[u8]) -> Result<EnrollmentResponse> {
    Ok(contract::decode_enrollment_response(bytes)?)
}
pub fn encode_enrollment_response(value: &EnrollmentResponse) -> Result<Vec<u8>> {
    Ok(contract::encode_enrollment_response(value)?)
}
pub fn decode_bundle(bytes: &[u8]) -> Result<KeyringBundle> {
    Ok(contract::decode_bundle(bytes)?)
}
pub fn encode_bundle(value: &KeyringBundle) -> Result<Vec<u8>> {
    Ok(contract::encode_bundle(value)?)
}
pub fn decode_error(bytes: &[u8]) -> Result<ErrorResponse> {
    Ok(contract::decode_error(bytes)?)
}
pub fn encode_error(value: &ErrorResponse) -> Result<Vec<u8>> {
    Ok(contract::encode_error(value)?)
}
pub(crate) fn strict_json(bytes: &[u8], limit: usize) -> Result<serde_json::Value> {
    Ok(contract::strict_json(bytes, limit)?)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn application_roundtrips_preserve_exact_wire_records() {
        const ROOT: &str = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../internal/racer/wire/testdata/"
        );
        for name in [
            "publication.json",
            "bootstrap-request.json",
            "bootstrap-response.json",
            "bundle.json",
        ] {
            let input = std::fs::read(format!("{ROOT}{name}")).unwrap();
            let bytes = input.strip_suffix(b"\n").unwrap_or(&input);
            let output = match name {
                "publication.json" => {
                    let publication = decode_publication(bytes).unwrap();
                    let roundtrip = encode_publication(&publication).unwrap();
                    assert_eq!(
                        content_hash(&publication).unwrap(),
                        contract::content_hash(&contract::decode_publication(bytes).unwrap())
                            .unwrap()
                    );
                    roundtrip
                }
                "bootstrap-request.json" => {
                    encode_enrollment_request(&decode_enrollment_request(bytes).unwrap()).unwrap()
                }
                "bootstrap-response.json" => {
                    encode_enrollment_response(&decode_enrollment_response(bytes).unwrap()).unwrap()
                }
                _ => encode_bundle(&decode_bundle(bytes).unwrap()).unwrap(),
            };
            // The publication fixture is intentionally not canonically sorted.
            if name == "publication.json" {
                assert_eq!(
                    output,
                    contract::encode_publication(&contract::decode_publication(bytes).unwrap())
                        .unwrap()
                );
            } else {
                assert_eq!(output, bytes, "{name}");
            }
        }
    }

    #[test]
    fn wire_failures_keep_application_error_meanings() {
        for (wire, app) in [
            (contract::Error::InvalidRequest, Error::InvalidRequest),
            (
                contract::Error::IncompatibleMembership,
                Error::IncompatibleMembership,
            ),
            (contract::Error::Overloaded, Error::Overloaded),
            (contract::Error::Replay, Error::Replay),
        ] {
            assert_eq!(Error::from(wire), app);
        }
        assert_eq!(
            strict_json(b"{\"x\":1,\"x\":2}", 64),
            Err(Error::InvalidRequest)
        );
        assert_eq!(strict_json(b"{}", 1), Err(Error::Overloaded));
    }
}
