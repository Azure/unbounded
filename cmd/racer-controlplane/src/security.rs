// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Native PKI and TLS security. Kubernetes adapters supply authoritative reads,
//! an externally elected term, and the fenced store below. No HTTP header can
//! construct a `TlsProof`. Private CA state belongs in a Secret, never a log.
//!
//! Durable state is a fresh-deployment format (version 4); public bundle.json
//! remains byte-compatible with the Go PKI helpers (version 1).
//!
//! Wiring order:
//! 1. Start replica CSR publication and proof/production listeners on followers.
//!    Keep LocalKey private to the process; publish only CSR, boot and Pod owner.
//! 2. After election, acquire CaManager with a unique Leadership and publish trust.
//!    Implement CaStore with direct reads, immutable shards and Secret/public CAS.
//! 3. Direct-list every eligible replica and admit unknown boots as `pending`.
//!    Authorize replica ownership, issue production/probe leaves, CAS responses
//!    against the exact request boot/CSR, and atomically install both snapshots.
//! 4. Enrollment uses TokenReview, authorize_renewal/authorize_enrollment and
//!    committed topology selection before issue. Return chain_pem and issuer.
//! 5. Acquire HotTls::production_connection before each handshake and retain its
//!    guard through transport close. Put VerifiedPeer in trusted request context;
//!    check node_pod and verify_member per request. Followers reject leader-only
//!    handlers immediately; keep replica proof serving for warm standby checks.
//! 6. Feed actual proof exchanges to record_proof. Complete direct admission and
//!    absence sweeps before advance_rotation. Cancel Leadership on election loss.
//!
//! `crate::service` supplies deadlines/concurrency admission, automatic rotation,
//! direct inventory, response CAS, old-connection closure and readiness. It
//! replaces managed Pods with multiple actual boots and waits for UID absence.
//! Retirement tombstones remain conservative and are not collected.

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
pub const MAX_OBJECT_BYTES: usize = 900 * 1024;

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

    pub fn uri(&self) -> Result<String> {
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
                Ok(format!(
                    "spiffe://racer/universe/{}/node/{}/pod/{}",
                    self.universe, self.node, self.pod_uid
                ))
            }
            IdentityKind::ControlPlane => {
                ensure!(
                    self.universe.is_empty() && self.node.is_empty(),
                    "invalid control-plane identity"
                );
                Ok("spiffe://racer/controlplane".into())
            }
        }
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
