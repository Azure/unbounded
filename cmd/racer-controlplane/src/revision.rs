// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! The only durable runtime object is a fixed-size revision checkpoint. Fresh
//! trust-domain bootstrap must call `initialize` before making leadership usable.
//! Runtime acquisition never creates a missing checkpoint, including on restart.

use std::collections::BTreeMap;
use std::sync::Arc;

use anyhow::{Context, Result, ensure};
use k8s_openapi::{api::core::v1::ConfigMap, apimachinery::pkg::apis::meta::v1::ObjectMeta};
use kube::{Api, Client, ResourceExt, api::PostParams};
use tokio::sync::Mutex;

use crate::subscription::SecurityGetter;

pub const CHECKPOINT: &str = "racer-runtime-revisions";
pub const RANGE_SIZE: u64 = 1 << 20;

#[derive(Clone, Debug, PartialEq, Eq)]
struct Checkpoint {
    high_water: u64,
    fence: String,
}

impl Checkpoint {
    fn read(object: &ConfigMap) -> Result<Self> {
        let data = object
            .data
            .as_ref()
            .context("missing revision checkpoint data")?;
        ensure!(
            data.len() == 3
                && data.get("format").map(String::as_str) == Some("1")
                && object.binary_data.as_ref().is_none_or(BTreeMap::is_empty),
            "invalid revision checkpoint schema"
        );
        let high_water = data
            .get("high-water")
            .context("missing revision high-water")?;
        let parsed: u64 = high_water.parse()?;
        ensure!(
            parsed.to_string() == *high_water,
            "invalid revision high-water"
        );
        let fence = data.get("fence").context("missing revision fence")?.clone();
        ensure!(
            fence.len() <= 320 && (parsed == 0 || !fence.is_empty()),
            "invalid revision fence"
        );
        Ok(Self {
            high_water: parsed,
            fence,
        })
    }

    fn data(&self) -> BTreeMap<String, String> {
        BTreeMap::from([
            ("format".into(), "1".into()),
            ("high-water".into(), self.high_water.to_string()),
            ("fence".into(), self.fence.clone()),
        ])
    }
}

#[derive(Debug)]
pub struct RevisionRange {
    next: u64,
    end: u64,
}

impl RevisionRange {
    pub fn take(&mut self) -> Result<u64> {
        ensure!(
            self.next <= self.end && self.next != 0,
            "revision range exhausted"
        );
        let value = self.next;
        self.next = self.next.checked_add(1).unwrap_or(0);
        Ok(value)
    }

    pub fn exhausted(&self) -> bool {
        self.next == 0 || self.next > self.end
    }
}

#[derive(Clone)]
pub struct RevisionStore {
    pub(crate) client: Client,
    api: Api<ConfigMap>,
    security: SecurityGetter,
    // Serializes reservations by this process and detects deletion/recreation or
    // rollback after a checkpoint has been observed. No runtime payload is stored.
    known: Arc<Mutex<Option<Known>>>,
}

struct Known {
    uid: String,
    high_water: u64,
    reserved: Option<(u64, String)>,
}

impl RevisionStore {
    pub fn new(client: Client, namespace: &str, security: SecurityGetter) -> Self {
        Self {
            api: Api::namespaced(client.clone(), namespace),
            client,
            security,
            known: Default::default(),
        }
    }

    /// Only fresh CA bootstrap may invoke this. A conflict is an error: never
    /// reset an existing checkpoint, and never infer freshness from its absence.
    pub async fn initialize(client: Client, namespace: &str) -> Result<()> {
        Api::<ConfigMap>::namespaced(client, namespace)
            .create(
                &PostParams::default(),
                &ConfigMap {
                    metadata: ObjectMeta {
                        name: Some(CHECKPOINT.into()),
                        ..Default::default()
                    },
                    data: Some(
                        Checkpoint {
                            high_water: 0,
                            fence: String::new(),
                        }
                        .data(),
                    ),
                    ..Default::default()
                },
            )
            .await?;
        Ok(())
    }

    pub(crate) fn check(&self, fence: &str) -> Result<()> {
        ensure!(
            (self.security)().is_some_and(|s| s.fence == fence && s.state.fence() == fence),
            "leadership lost"
        );
        Ok(())
    }

    pub async fn authoritative_fence(&self, fence: &str) -> Result<()> {
        self.check(fence)?;
        crate::security::kubernetes::KubernetesCaStore::new(
            self.client.clone(),
            self.api.namespace().unwrap_or_default().into(),
            crate::security::Leadership::new(fence.into())?,
        )
        .check_fence()
        .await?;
        self.check(fence)
    }

    pub async fn verify(&self, fence: &str) -> Result<()> {
        let known = self.known.lock().await;
        let known = known.as_ref().context("no observed revision checkpoint")?;
        let (high_water, reservation) = known
            .reserved
            .as_ref()
            .context("no reserved revision range")?;
        let object = self.api.get(CHECKPOINT).await?;
        let value = Checkpoint::read(&object)?;
        ensure!(
            object.uid().as_deref() == Some(known.uid.as_str())
                && value.high_water == *high_water
                && value.fence == *reservation,
            "revision checkpoint lost or superseded"
        );
        self.authoritative_fence(fence).await
    }

    /// Each call reserves a fresh disjoint range, even with the same fence after
    /// restart. Read the target RV before the authority check, so a takeover CAS
    /// invalidates delayed predecessor writes. Uncertain responses are resolved
    /// by a direct read; only the exact reserved checkpoint can authorize use.
    pub async fn reserve(&self, fence: &str) -> Result<RevisionRange> {
        ensure!(
            !fence.is_empty() && fence.len() <= 256,
            "invalid leadership fence"
        );
        let mut known = self.known.lock().await;
        // A unique reservation token disambiguates unknown outcomes even when
        // two restarted processes happen to carry the same leadership fence.
        let reservation = format!("{fence}/{}", uuid::Uuid::new_v4());
        for _ in 0..16 {
            let mut object = self
                .api
                .get(CHECKPOINT)
                .await
                .context("revision checkpoint unavailable; refusing reset")?;
            let old = Checkpoint::read(&object)?;
            let uid = object.uid().context("revision checkpoint lacks UID")?;
            ensure!(
                object.resource_version().is_some(),
                "revision checkpoint lacks resourceVersion"
            );
            if let Some(known) = known.as_ref() {
                ensure!(
                    known.uid == uid && old.high_water >= known.high_water,
                    "revision checkpoint replaced or regressed"
                );
            }
            // Any attempted acquisition invalidates this store's prior range,
            // including cancellation or an unresolved response.
            *known = Some(Known {
                uid: uid.clone(),
                high_water: old.high_water,
                reserved: None,
            });
            let end = old
                .high_water
                .checked_add(RANGE_SIZE)
                .context("revision space exhausted")?;
            let next = Checkpoint {
                high_water: end,
                fence: reservation.clone(),
            };
            object.data = Some(next.data());
            self.authoritative_fence(fence).await?;
            let result = self
                .api
                .replace(CHECKPOINT, &PostParams::default(), &object)
                .await;
            // Even a successful response is rechecked against authority before
            // exposing the range. A former leader may burn numbers, never serve.
            let readback = self
                .api
                .get(CHECKPOINT)
                .await
                .context("reservation outcome unknown")?;
            let observed = Checkpoint::read(&readback)?;
            ensure!(
                readback.uid().as_deref() == Some(&uid) && observed.high_water >= old.high_water,
                "revision checkpoint replaced or regressed"
            );
            *known = Some(Known {
                uid,
                high_water: observed.high_water,
                reserved: None,
            });
            self.authoritative_fence(fence).await?;
            if observed == next {
                known.as_mut().unwrap().reserved = Some((end, reservation.clone()));
                return Ok(RevisionRange {
                    next: old.high_water + 1,
                    end,
                });
            }
            match result {
                Err(kube::Error::Api(e)) if e.code == 409 => continue,
                Err(error) => return Err(error.into()),
                Ok(_) => anyhow::bail!("revision reservation superseded"),
            }
        }
        anyhow::bail!("revision checkpoint contention")
    }
}
