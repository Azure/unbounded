// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Native PKI and TLS security. Kubernetes adapters supply authoritative reads,
//! an externally elected term, and the fenced store below. No HTTP header can
//! construct a `TlsProof`. Private CA state belongs in a Secret, never a log.
//!
//! Version 1 private state has at most two authorities and fixed-size rotation
//! metadata. Leaves carry versioned signed claims; TLS proves key ownership.
//! Enrollment always authorizes live Kubernetes resources. Request authentication
//! uses only signed claims, current trust, expiry, and local topology policy.
//! Rotation waits for publication overlap and durable issuer expiry watermarks,
//! never for fleet acknowledgments. Replica keys remain process-local; bounded
//! Pod annotations carry only the current CSR and certificate response.

#[path = "security/certificates.rs"]
mod certificates;
#[path = "security/enrollment.rs"]
mod enrollment;
#[path = "security/kubernetes.rs"]
pub mod kubernetes;
#[path = "security/state.rs"]
mod state;
#[path = "security/transport.rs"]
mod transport;

pub use certificates::*;
pub use enrollment::*;
pub use state::*;
pub use transport::*;

use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const CONTROL_AUDIENCE: &str = "racer-control";

pub fn digest(bytes: &[u8]) -> String {
    hex::encode(Sha256::digest(bytes))
}

fn hex_id(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

fn process_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value.as_bytes()[0].is_ascii_alphanumeric()
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b))
}

fn pod_name(value: &str) -> bool {
    !value.is_empty() && value.len() <= 253 && value.split('.').all(certificates::valid_namespace)
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum IdentityKind {
    Node,
    ControlPlane,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct Identity {
    pub kind: IdentityKind,
    pub universe: String,
    pub node: String,
    #[serde(rename = "podUID")]
    pub pod_uid: String,
    #[serde(rename = "bootID")]
    pub boot_id: String,
    pub pod_name: String,
    #[serde(rename = "containerID")]
    pub container_id: String,
}

impl Identity {
    pub fn key(&self) -> String {
        format!("{}/{}", self.pod_uid, self.boot_id)
    }

    pub fn validate(&self) -> Result<()> {
        ensure!(
            process_id(&self.pod_uid) && process_id(&self.boot_id),
            "invalid process identity"
        );
        match self.kind {
            IdentityKind::Node => {
                ensure!(
                    hex_id(&self.universe) && hex_id(&self.node),
                    "invalid node identity"
                );
            }
            IdentityKind::ControlPlane => {
                ensure!(
                    self.universe.is_empty() && self.node.is_empty(),
                    "invalid control-plane identity"
                );
            }
        }
        Ok(())
    }
}

/// Canonical URI SAN payload. Hex encoding keeps every claim unambiguous and
/// avoids URL normalization differences between TLS implementations.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SignedClaims {
    pub version: u32,
    pub namespace: String,
    pub identity: Identity,
}

impl SignedClaims {
    pub fn validate(&self) -> Result<()> {
        ensure!(self.version == 1, "unsupported certificate claims version");
        ensure!(
            certificates::valid_namespace(&self.namespace),
            "invalid claims namespace"
        );
        self.identity.validate()?;
        ensure!(hex_id(&self.identity.boot_id), "invalid signed boot nonce");
        ensure!(pod_name(&self.identity.pod_name), "invalid signed Pod name");
        ensure!(
            self.identity.container_id.len() <= 256,
            "container identity too long"
        );
        Ok(())
    }

    pub fn uri(&self) -> Result<String> {
        self.validate()?;
        let uri = format!(
            "spiffe://racer/v1/{}",
            hex::encode(serde_json::to_vec(self)?)
        );
        ensure!(uri.len() <= 4096, "certificate claims too large");
        Ok(uri)
    }

    pub fn parse(uri: &str) -> Result<Self> {
        ensure!(uri.len() <= 4096, "certificate claims too large");
        let payload = uri
            .strip_prefix("spiffe://racer/v1/")
            .ok_or_else(|| anyhow::anyhow!("missing versioned certificate claims"))?;
        let claims: Self = serde_json::from_slice(&hex::decode(payload)?)?;
        claims.validate()?;
        ensure!(claims.uri()? == uri, "noncanonical certificate claims");
        Ok(claims)
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, rename_all = "camelCase")]
pub struct Acknowledgment {
    pub generation: u64,
    pub digest: String,
    pub old_connections_drained: bool,
}

/// Seconds since Unix epoch. Proof creation uses the local clock, never wire time.
pub fn unix_now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .expect("clock before Unix epoch")
        .as_secs() as i64
}
