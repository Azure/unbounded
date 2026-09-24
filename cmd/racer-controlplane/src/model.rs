// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::BTreeMap;
use std::net::IpAddr;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{Error, Result};

pub const SLOT_COUNT: u32 = 262_144;
/// Physical-owner HRW with independent Cartesian-product routing roles.
pub const PRODUCT_ROUTING_ALGORITHM: u32 = 1;
pub const GENERATION_FORMAT: u32 = 1;
pub const SOCKET_ROOT: &str = "/run/racer";

pub fn identity_bytes(domain: &str, value: &str) -> [u8; 32] {
    Sha256::digest(format!("racer/{domain}/v1\0{value}").as_bytes()).into()
}

pub fn identity(domain: &str, value: &str) -> String {
    hex::encode(identity_bytes(domain, value))
}

pub fn universe_for_site(site: &str) -> String {
    let alnum = |b: u8| b.is_ascii_alphanumeric();
    let valid = site.len() <= 63
        && site.as_bytes().first().is_some_and(|b| alnum(*b))
        && site.as_bytes().last().is_some_and(|b| alnum(*b))
        && site.bytes().all(|b| alnum(b) || b"-_.".contains(&b));
    if site.is_empty() || valid {
        site.into()
    } else {
        format!(
            "site_{}",
            data_encoding::BASE32_NOPAD
                .encode(&Sha256::digest(site.as_bytes()))
                .to_ascii_lowercase()
        )
    }
}

pub fn universe_id_for_site(site: &str) -> String {
    if site.is_empty() {
        String::new()
    } else {
        identity("universe", &universe_for_site(site))
    }
}

/// Only the canonical Site label assigns membership.
pub fn node_site(canonical: Option<&str>) -> String {
    canonical.unwrap_or_default().into()
}

/// Derive client and origin sockets from metadata.name. Names are
/// 1..=63 lowercase ASCII alphanumeric bytes with optional interior hyphens.
pub fn cache_sockets(root: &str, name: &str) -> Result<(String, String)> {
    let label_char = |b: u8| b.is_ascii_lowercase() || b.is_ascii_digit();
    if !root.starts_with('/')
        || root.contains('\0')
        || name.is_empty()
        || name.len() > 63
        || !name.as_bytes().first().is_some_and(|b| label_char(*b))
        || !name.as_bytes().last().is_some_and(|b| label_char(*b))
        || !name.bytes().all(|b| label_char(b) || b == b'-')
    {
        return Err(Error(
            "socket root must be absolute and cache name must be a lowercase DNS label of at most 63 bytes".into(),
        ));
    }
    // Match filepath.Clean without resolving symlinks or consulting the filesystem.
    let mut parts = Vec::new();
    for part in root.split('/') {
        match part {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            _ => parts.push(part),
        }
    }
    parts.push(name);
    let base = format!("/{}", parts.join("/"));
    let client = format!("{base}/client/socket");
    let origin = format!("{base}/origin/socket");
    if origin.len() > 107 {
        return Err(Error("derived socket path exceeds 107 bytes".into()));
    }
    Ok((client, origin))
}

/// Kubernetes adapters normalize membership/OS/deletion into `eligible` and
/// readiness into `ready`. Include eligible unready nodes to send removals.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Node {
    pub name: String,
    pub uid: String,
    pub universe: String,
    pub eligible: bool,
    pub ready: bool,
    #[serde(default)]
    pub fabric: String,
    #[serde(default)]
    pub pods: Vec<Pod>,
}

/// `available` means running, nondeleting, managed DaemonSet-owned, with the
/// expected labels/service account. The compiler additionally checks IP validity.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Pod {
    pub uid: String,
    pub namespace: String,
    pub name: String,
    pub ip: String,
    pub available: bool,
    pub ready: bool,
    /// Nanoseconds since the Unix epoch, used only for overlap preference.
    pub created_at: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Cache {
    pub name: String,
    pub uid: String,
    pub resource_generation: i64,
    pub cache_generation: i64,
    pub max_candidate_attempts: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Inventory {
    pub universe: String,
    pub nodes: Vec<Node>,
    /// Live caches already selected against Site labels by the adapter.
    pub caches: Vec<Cache>,
    pub socket_root: String,
}

#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Member {
    pub id: String,
    pub ip: Option<IpAddr>,
    pub fabric: String,
    pub pod_uid: String,
    pub pod_namespace: String,
    pub pod_name: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Volume {
    pub id: String,
    pub name: String,
    pub resource_generation: i64,
    pub client_socket: String,
    pub origin_socket: String,
    pub slots: u32,
    pub cache_generation: u64,
    pub routing_algorithm: u32,
    pub max_candidate_attempts: u32,
    /// Node names, in slot order. Empty only when no process is available.
    pub owners: Vec<String>,
}

/// Process-local desired state. Physical placement derives solely from inventory;
/// valid previous roles may be retained to reduce graph churn within a leadership.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Generation {
    pub format: u32,
    pub universe: String,
    pub revision: u64,
    pub nodes: BTreeMap<String, Member>,
    pub volumes: Vec<Volume>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub product: Option<ProductPlacement>,
}

/// Process-local role assignments and universe-wide physical owner rankings.
/// Roles may be reassigned after leadership restart; rankings never use roles.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ProductPlacement {
    pub left_factor: u32,
    pub right_factor: u32,
    pub members: Vec<String>,
    pub roles: Vec<u32>,
    pub candidate_width: u32,
    pub candidates: Vec<u32>,
}

impl Generation {
    pub fn empty(universe: impl Into<String>) -> Self {
        Self {
            format: GENERATION_FORMAT,
            universe: universe.into(),
            revision: 0,
            nodes: BTreeMap::new(),
            volumes: Vec::new(),
            product: None,
        }
    }

    pub fn canonical_bytes(&self) -> Vec<u8> {
        serde_json::to_vec(self).expect("generation contains only serializable values")
    }

    pub fn digest(&self) -> [u8; 32] {
        Sha256::digest(self.canonical_bytes()).into()
    }
}

pub(crate) fn available_ip(raw: &str) -> Option<IpAddr> {
    let ip: IpAddr = raw.parse().ok()?;
    if ip.is_unspecified() || ip.is_multicast() || ip.is_loopback() {
        return None;
    }
    match ip {
        IpAddr::V4(v4) if v4.is_broadcast() => None,
        IpAddr::V6(v6) if v6.to_ipv4_mapped().is_some() => {
            available_ip(&v6.to_ipv4_mapped()?.to_string())
        }
        _ => Some(ip),
    }
}
