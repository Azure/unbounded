// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use anyhow::Context;
use std::collections::BTreeMap;

const PREFIX: &str = "racer.unbounded-cloud.io/";
const DATAPLANE: &str = "racer-dataplane";
const CONTROLPLANE: &str = "racer-controlplane";

/// Data-only authorization inputs populated from successful direct Kubernetes
/// reads. They are not deserialized from enrollment requests. The adapter must
/// fail on read errors and preserve object UIDs, owner flags, and label presence.
#[derive(Clone, Debug, Default)]
pub struct ObjectMetadata {
    pub namespace: String,
    pub name: String,
    pub uid: String,
    pub deleting: bool,
    pub labels: BTreeMap<String, String>,
    pub owners: Vec<OwnerReference>,
}

#[derive(Clone, Debug, Default)]
pub struct OwnerReference {
    pub api_version: String,
    pub kind: String,
    pub name: String,
    pub uid: String,
    pub controller: bool,
}

#[derive(Clone, Debug, Default)]
pub struct PodData {
    pub metadata: ObjectMetadata,
    pub service_account: String,
    pub node_name: String,
    pub running_container_id: String,
}

#[derive(Clone, Debug, Default)]
pub struct WorkloadData {
    pub metadata: ObjectMetadata,
    pub template_service_account: String,
    pub template_labels: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Default)]
pub struct SiteData {
    pub metadata: ObjectMetadata,
    /// True only when spec.components.racer exists and ComponentEnabled is true.
    pub racer_enabled: bool,
}

/// Result of TokenReview sent with exactly `audiences: ["racer-control"]`.
/// Authentication failure and API failure remain distinct at the HTTP adapter.
#[derive(Clone, Debug, Default)]
pub struct TokenReviewResult {
    pub authenticated: bool,
    pub audiences: Vec<String>,
    pub error: String,
    pub pod_uids: Vec<String>,
}

pub fn token_review_request(token: &str) -> Result<serde_json::Value> {
    ensure!(
        !token.is_empty() && token.len() <= 16384,
        "invalid bearer token size"
    );
    Ok(
        serde_json::json!({ "apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview", "spec": { "token": token, "audiences": [CONTROL_AUDIENCE] } }),
    )
}

pub fn reviewed_pod_uid(review: &TokenReviewResult) -> Result<&str> {
    ensure!(review.error.is_empty(), "TokenReview unavailable");
    ensure!(
        review.authenticated && review.audiences.iter().any(|a| a == CONTROL_AUDIENCE),
        "Pod credential rejected"
    );
    ensure!(
        review.pod_uids.len() == 1 && process_id(&review.pod_uids[0]),
        "credential lacks one bound Pod UID"
    );
    Ok(&review.pod_uids[0])
}

fn label<'a>(metadata: &'a ObjectMetadata, suffix: &str) -> &'a str {
    metadata
        .labels
        .get(&format!("{PREFIX}{suffix}"))
        .map(String::as_str)
        .unwrap_or("")
}

fn owned_by(
    child: &ObjectMetadata,
    parent: &ObjectMetadata,
    api: &str,
    kind: &str,
    controller: bool,
) -> bool {
    !parent.uid.is_empty()
        && child.owners.iter().any(|o| {
            o.api_version == api
                && o.kind == kind
                && o.name == parent.name
                && o.uid == parent.uid
                && (!controller || o.controller)
        })
}

pub fn racer_identity(domain: &str, value: &str) -> String {
    digest(format!("racer/{domain}/v1\0{value}").as_bytes())
}

/// Exact Go UniverseForSite mapping, including long DNS-subdomain Site names.
pub fn universe_for_site(site: &str) -> String {
    let legal = site.is_empty()
        || (site.len() <= 63
            && site.as_bytes()[0].is_ascii_alphanumeric()
            && site.as_bytes()[site.len() - 1].is_ascii_alphanumeric()
            && site
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b"_.-".contains(&b)));
    if legal {
        return site.into();
    }
    const ALPHABET: &[u8] = b"abcdefghijklmnopqrstuvwxyz234567";
    let mut result = String::from("site_");
    let mut bits = 0u32;
    let mut count = 0;
    for byte in Sha256::digest(site.as_bytes()) {
        bits = (bits << 8) | u32::from(byte);
        count += 8;
        while count >= 5 {
            count -= 5;
            result.push(ALPHABET[((bits >> count) & 31) as usize] as char);
        }
    }
    if count > 0 {
        result.push(ALPHABET[((bits << (5 - count)) & 31) as usize] as char);
    }
    result
}

/// Canonical label presence wins even when its value is empty.
pub fn node_site(node: &ObjectMetadata) -> &str {
    node.labels
        .get("unbounded-cloud.io/site")
        .or_else(|| node.labels.get("net.unbounded-cloud.io/site"))
        .map(String::as_str)
        .unwrap_or("")
}

pub struct EnrollmentData<'a> {
    pub namespace: &'a str,
    pub pod_name: &'a str,
    pub boot: &'a str,
    pub review: &'a TokenReviewResult,
    pub pod: &'a PodData,
    pub daemon_set: &'a WorkloadData,
    pub node: &'a ObjectMetadata,
    pub site: &'a SiteData,
}

pub fn authorize_enrollment(data: &EnrollmentData<'_>) -> Result<Identity> {
    let uid = reviewed_pod_uid(data.review)?;
    let pod = data.pod;
    let daemon = data.daemon_set;
    let node = data.node;
    let site = data.site;
    ensure!(hex_id(data.boot), "boot must be a lowercase 32-byte nonce");
    ensure!(
        super::certificates::valid_namespace(data.namespace)
            && pod.metadata.namespace == data.namespace
            && pod.metadata.name == data.pod_name
            && pod.metadata.uid == uid,
        "Pod identity mismatch"
    );
    ensure!(
        !pod.metadata.deleting
            && pod.service_account == DATAPLANE
            && !pod.node_name.is_empty()
            && label(&pod.metadata, "dataplane") == "true",
        "Pod not eligible"
    );
    ensure!(
        daemon.metadata.namespace == data.namespace
            && !daemon.metadata.deleting
            && owned_by(
                &pod.metadata,
                &daemon.metadata,
                "apps/v1",
                "DaemonSet",
                true
            ),
        "DaemonSet ownership mismatch"
    );
    ensure!(
        label(&daemon.metadata, "component") == DATAPLANE
            && daemon.template_service_account == DATAPLANE,
        "unmanaged DaemonSet"
    );
    ensure!(
        node.name == pod.node_name
            && !node.uid.is_empty()
            && !node.deleting
            && label(node, "exclude") != "true",
        "Node not eligible"
    );
    let site_name = node_site(node);
    ensure!(
        !site_name.is_empty()
            && site_name == site.metadata.name
            && !site.metadata.deleting
            && site.racer_enabled,
        "Site not eligible"
    );
    let universe = universe_for_site(site_name);
    ensure!(
        label(&pod.metadata, "universe") == universe
            && daemon.template_labels.get(&format!("{PREFIX}universe")) == Some(&universe),
        "universe mismatch"
    );
    ensure!(
        owned_by(
            &daemon.metadata,
            &site.metadata,
            "unbounded-cloud.io/v1alpha3",
            "Site",
            false
        ),
        "DaemonSet not Site-owned"
    );
    let identity = Identity {
        kind: IdentityKind::Node,
        universe: racer_identity("universe", &universe),
        node: racer_identity("node", &node.uid),
        pod_uid: uid.into(),
        boot_id: data.boot.into(),
        pod_name: pod.metadata.name.clone(),
        container_id: pod.running_container_id.clone(),
    };
    identity.uri()?;
    Ok(identity)
}

/// Renewal keeps an already admitted boot's historical Node/Site identity, even
/// after exclusion or Node replacement. The adapter still checks committed
/// topology selection and authenticates a current Pod-bound TokenReview.
pub fn authorize_renewal(
    state: &CaState,
    namespace: &str,
    pod_name: &str,
    pod: &PodData,
    boot: &str,
    review: &TokenReviewResult,
) -> Result<Identity> {
    let uid = reviewed_pod_uid(review)?;
    ensure!(
        hex_id(boot)
            && pod.metadata.namespace == namespace
            && pod.metadata.name == pod_name
            && pod.metadata.uid == uid
            && pod.service_account == DATAPLANE,
        "renewal Pod mismatch"
    );
    let member = state
        .member(&format!("{uid}/{boot}"))
        .context("unadmitted boot")?;
    ensure!(
        member.identity.kind == IdentityKind::Node,
        "not a node enrollment"
    );
    let mut identity = member.identity.clone();
    if identity.pod_name.is_empty() {
        identity.pod_name.clone_from(&pod.metadata.name);
    }
    ensure!(
        identity.pod_name == pod.metadata.name,
        "renewal Pod name changed"
    );
    Ok(identity)
}

/// CP labels alone do not authorize keys. Validate the live Pod -> ReplicaSet ->
/// managed Deployment UID chain. Terminating replicas remain rotation members.
pub fn authorize_replica(
    namespace: &str,
    pod: &PodData,
    replica_set: &WorkloadData,
    deployment: &WorkloadData,
    boot: &str,
) -> Result<Identity> {
    ensure!(
        super::certificates::valid_namespace(namespace) && process_id(boot),
        "invalid replica identity"
    );
    ensure!(
        pod.metadata.namespace == namespace
            && replica_set.metadata.namespace == namespace
            && deployment.metadata.namespace == namespace,
        "replica namespace mismatch"
    );
    ensure!(
        !pod.metadata.uid.is_empty()
            && pod.service_account == CONTROLPLANE
            && label(&pod.metadata, "component") == CONTROLPLANE,
        "unmanaged CP Pod"
    );
    ensure!(
        owned_by(
            &pod.metadata,
            &replica_set.metadata,
            "apps/v1",
            "ReplicaSet",
            true
        ),
        "CP ReplicaSet mismatch"
    );
    ensure!(
        owned_by(
            &replica_set.metadata,
            &deployment.metadata,
            "apps/v1",
            "Deployment",
            true
        ),
        "CP Deployment mismatch"
    );
    ensure!(
        deployment.metadata.name == CONTROLPLANE
            && label(&deployment.metadata, "component") == CONTROLPLANE
            && deployment.template_service_account == CONTROLPLANE,
        "unmanaged CP Deployment"
    );
    let identity = Identity {
        kind: IdentityKind::ControlPlane,
        universe: String::new(),
        node: String::new(),
        pod_uid: pod.metadata.uid.clone(),
        boot_id: boot.into(),
        pod_name: pod.metadata.name.clone(),
        container_id: pod.running_container_id.clone(),
    };
    identity.uri()?;
    Ok(identity)
}

pub fn replica_request_owned(metadata: &ObjectMetadata, pod: &PodData) -> bool {
    metadata.namespace == pod.metadata.namespace
        && metadata.name == format!("racer-replica-{}", pod.metadata.uid)
        && owned_by(metadata, &pod.metadata, "v1", "Pod", true)
}

pub fn certificate_matches_csr(certificate: &[u8], csr: &[u8]) -> Result<bool> {
    let key = validate_csr(csr)?;
    let certs = super::certificates::parse_certificates(certificate)?;
    Ok(key.public_eq(certs[0].public_key()?.as_ref()))
}
