// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Operational observations, never restored from status annotations after restart.

use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use http::HeaderMap;
use serde_json::{Value, json};

use crate::storage::{ALIGNMENT, MAX_BYTES, MIN_BYTES, StoragePolicy};

pub const FRESHNESS: Duration = Duration::from_secs(75);

pub(crate) fn header<'a>(headers: &'a HeaderMap, name: &str) -> &'a str {
    headers
        .get(name)
        .and_then(|v| v.to_str().ok())
        .unwrap_or_default()
}

fn number(headers: &HeaderMap, name: &str) -> Option<u64> {
    let value = header(headers, name);
    (!value.is_empty() && value.bytes().all(|b| b.is_ascii_digit()))
        .then(|| value.parse().ok())
        .flatten()
}

#[derive(Clone, Debug)]
pub struct Observation {
    pub pod_uid: String,
    pub boot: String,
    pub seen: Instant,
    pub revision: u64,
    pub digest: String,
    pub healthy: bool,
    pub local_state: String,
    pub rejected_revision: u64,
    pub offered: Option<([u8; 32], u64)>,
    pub storage_state: String,
    pub applied_bytes: u64,
    pub applied_version: u64,
    pub shards: u64,
    pub error: String,
}

impl Observation {
    pub fn observe(
        previous: Option<&Self>,
        pod: &str,
        boot: &str,
        headers: &HeaderMap,
        now: Instant,
    ) -> Self {
        let previous = previous.filter(|p| p.pod_uid == pod && p.boot == boot);
        let mut next = previous.cloned().unwrap_or_else(|| Self {
            pod_uid: pod.into(),
            boot: boot.into(),
            seen: now,
            revision: 0,
            digest: String::new(),
            healthy: false,
            local_state: String::new(),
            rejected_revision: 0,
            offered: None,
            storage_state: "pending".into(),
            applied_bytes: 0,
            applied_version: 0,
            shards: 0,
            error: String::new(),
        });
        next.seen = now;
        next.revision = number(headers, "x-racer-applied-revision").unwrap_or(0);
        next.digest = header(headers, "x-racer-applied-digest")
            .chars()
            .take(64)
            .collect();
        next.healthy = header(headers, "x-racer-worker-healthy") == "1";
        next.local_state = header(headers, "x-racer-local-state")
            .chars()
            .take(16)
            .collect();
        next.rejected_revision = number(headers, "x-racer-rejected-revision").unwrap_or(0);
        let identity = header(headers, "x-racer-storage-identity");
        let version = number(headers, "x-racer-storage-version");
        let bytes = number(headers, "x-racer-storage-applied-bytes");
        let state = header(headers, "x-racer-storage-state");
        if let (Some((offered_id, offered_version)), Some(version), Some(bytes)) =
            (next.offered, version, bytes)
            && identity == hex::encode(offered_id)
            && version == offered_version
            && matches!(state, "pending" | "failed" | "applied")
            && (bytes == 0
                || ((MIN_BYTES..=MAX_BYTES).contains(&bytes) && bytes.is_multiple_of(ALIGNMENT)))
        {
            next.storage_state = state.into();
            if next.applied_bytes != bytes {
                next.applied_version = 0;
            }
            next.applied_bytes = bytes;
            if state == "applied" {
                next.applied_version = version;
            }
            next.shards = number(headers, "x-racer-storage-shards")
                .filter(|s| *s <= bytes / MIN_BYTES)
                .unwrap_or(0);
            next.error = if state == "failed" {
                let raw = header(headers, "x-racer-storage-error");
                if raw.len() <= 2048 {
                    hex::decode(raw)
                        .ok()
                        .map(|b| String::from_utf8_lossy(&b).into_owned())
                        .unwrap_or_default()
                } else {
                    String::new()
                }
            } else {
                String::new()
            };
        }
        next
    }

    pub fn offer(&mut self, policy: Option<&StoragePolicy>) {
        if let Some(policy) = policy.filter(|p| p.wire().is_some()) {
            let offered = Some((policy.identity, policy.version));
            if self.offered != offered {
                self.storage_state = "pending".into();
                self.error.clear();
            }
            // "applied" must acknowledge exactly the offered desired size.
            if self.storage_state == "applied" && self.applied_bytes != policy.desired_bytes {
                self.storage_state = "pending".into();
                self.applied_version = 0;
            }
            self.offered = offered;
        } else {
            self.offered = None;
        }
    }

    pub fn converged(&self, pod: &str, revision: u64, digest: &str, now: Instant) -> bool {
        self.pod_uid == pod
            && now.duration_since(self.seen) < FRESHNESS
            && self.healthy
            && self.local_state == "applied"
            && self.revision == revision
            && self.digest == digest
    }
}

pub fn cache_status(
    policy: &StoragePolicy,
    report: Option<&Observation>,
    source: &str,
    requested: &str,
    now: Instant,
) -> Value {
    let fresh = report.is_some_and(|r| now.duration_since(r.seen) < FRESHNESS);
    let phase = if !fresh {
        "stale"
    } else {
        report.map_or("pending", |r| r.storage_state.as_str())
    };
    json!({ "source": source, "requested": requested, "effectiveBytes": policy.desired_bytes,
        "policyIdentity": hex::encode(policy.identity), "policyVersion": policy.version,
        "phase": if policy.validation_error.is_some() { "invalid" } else { phase },
        "policyPhase": phase, "validationError": policy.validation_error,
        "appliedBytes": report.map_or(0, |r| r.applied_bytes), "appliedVersion": report.map_or(0, |r| r.applied_version),
        "shards": report.map_or(0, |r| r.shards), "error": report.map_or("", |r| r.error.as_str()),
        "selectedPodUID": report.map_or("", |r| r.pod_uid.as_str()), "boot": report.map_or("", |r| r.boot.as_str()), "fresh": fresh })
}

pub(crate) type Reports = BTreeMap<(String, String), Observation>;
