// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Direct Kubernetes PKI persistence. The Secret resourceVersion is the single
//! commit point. Immutable public participant shards are content verified.
use super::*;
use anyhow::Context;
use k8s_openapi::{
    ByteString,
    api::{
        coordination::v1::Lease,
        core::v1::{ConfigMap, Secret},
    },
    apimachinery::pkg::apis::meta::v1::ObjectMeta,
};
use kube::{
    Api, Client,
    api::{DeleteParams, ListParams, PostParams, Preconditions},
};
use std::collections::BTreeMap;

pub const CA_SECRET: &str = "racer-ca";
pub const TRUST_MAP: &str = "racer-trust";
pub const STATE_KEY: &str = "state.json";
pub const BUNDLE_KEY: &str = "bundle.json";
pub const LEASE: &str = "racer-controlplane";
const PUBLIC_FENCE: &str = "racer.unbounded.cloud/pki-fence";
const BOOTSTRAP_PENDING: &str = "racer.unbounded.cloud/pki-bootstrap-pending";

pub fn participant_object_name(fence: &str, id: &str) -> String {
    format!("racer-pki-{}-{id}", &digest(fence.as_bytes())[..16])
}

#[derive(Clone)]
pub struct KubernetesCaStore {
    client: Client,
    namespace: String,
    term: Leadership,
    shards: std::sync::Arc<tokio::sync::Mutex<BTreeMap<String, Vec<u8>>>>,
}

impl KubernetesCaStore {
    pub fn new(client: Client, namespace: String, term: Leadership) -> Self {
        Self {
            client,
            namespace,
            term,
            shards: Default::default(),
        }
    }

    /// This is also used before routing/replica response CAS operations. Capture
    /// the target object's resourceVersion before invoking this fence check.
    pub async fn check_fence(&self) -> Result<()> {
        self.check_lease().await?;
        let secret = Api::<Secret>::namespaced(self.client.clone(), &self.namespace)
            .get(CA_SECRET)
            .await?;
        let metadata = secret
            .data
            .as_ref()
            .and_then(|d| d.get(STATE_KEY))
            .context("missing CA state")?;
        let value: serde_json::Value = serde_json::from_slice(&metadata.0)?;
        ensure!(
            value["fence"].as_str() == Some(self.term.token.as_str()),
            "CA fence lost"
        );
        self.term.check()
    }

    pub async fn check_lease(&self) -> Result<()> {
        self.term.check()?;
        let lease = Api::<Lease>::namespaced(self.client.clone(), &self.namespace)
            .get(LEASE)
            .await?;
        let spec = lease.spec.context("Lease missing spec")?;
        ensure!(
            spec.holder_identity.as_deref() == Some(self.term.token.as_str()),
            "Lease owned by another process"
        );
        let renewed = spec
            .renew_time
            .context("Lease missing renewal")?
            .0
            .timestamp();
        let lifetime = spec
            .lease_duration_seconds
            .context("Lease missing duration")?;
        ensure!(
            lifetime > 0 && unix_now() < renewed.saturating_add(i64::from(lifetime)),
            "Lease expired"
        );
        Ok(())
    }

    fn maps(&self) -> Api<ConfigMap> {
        Api::namespaced(self.client.clone(), &self.namespace)
    }
    fn secrets(&self) -> Api<Secret> {
        Api::namespaced(self.client.clone(), &self.namespace)
    }

    async fn artifacts(&self) -> Result<bool> {
        // An unfiltered, complete API list is deliberate. Legacy artifacts and
        // even empty trust tombstones must prevent trust-domain regeneration.
        let maps = self.maps().list(&ListParams::default()).await?;
        Ok(maps.items.iter().any(|cm| {
            let name = cm.metadata.name.as_deref().unwrap_or("");
            let state = cm
                .metadata
                .labels
                .as_ref()
                .and_then(|m| m.get("racer.unbounded-cloud.io/state"));
            name == TRUST_MAP
                || state.is_some_and(|s| s == "commit")
                || name.starts_with("racer-desired-")
                || name.starts_with("racer-v4-")
                || (name.starts_with("racer-replica-")
                    && cm.data.as_ref().is_some_and(|d| {
                        d.contains_key("certificate") || d.contains_key("proof-certificate")
                    }))
        }))
    }

    async fn load_image(&self, secret: &Secret) -> Result<StateImage> {
        let metadata = secret
            .data
            .as_ref()
            .and_then(|d| d.get(STATE_KEY))
            .context("CA Secret missing state.json")?
            .0
            .clone();
        ensure!(
            metadata.len() <= MAX_OBJECT_BYTES,
            "CA metadata exceeds capacity"
        );
        let value: serde_json::Value = serde_json::from_slice(&metadata)?;
        let refs = value["shards"]
            .as_object()
            .context("invalid participant references")?;
        ensure!(refs.len() <= 1024, "too many shards");
        let maps = self.maps();
        let fence = value["fence"]
            .as_str()
            .context("missing shard fence")?
            .to_owned();
        let cache = self.shards.clone();
        use futures::{StreamExt, TryStreamExt};
        let ids: Vec<String> = refs
            .values()
            .map(|v| {
                v.as_str()
                    .context("invalid shard digest")
                    .map(str::to_owned)
            })
            .collect::<Result<_>>()?;
        let shards: Vec<(String, Vec<u8>)> = futures::stream::iter(ids.into_iter().map(|id| {
            let maps = maps.clone();
            let fence = fence.clone();
            let cache = cache.clone();
            async move {
                ensure!(hex_id(&id), "invalid shard digest");
                let name = participant_object_name(&fence, &id);
                if let Some(bytes) = cache.lock().await.get(&name).cloned() {
                    return Ok::<_, anyhow::Error>((id, bytes));
                }
                let cm = maps.get(&name).await?;
                ensure!(cm.immutable == Some(true), "mutable participant object");
                let bytes = cm
                    .data
                    .as_ref()
                    .and_then(|d| d.get(STATE_KEY))
                    .context("missing participant data")?
                    .as_bytes()
                    .to_vec();
                ensure!(
                    bytes.len() <= MAX_OBJECT_BYTES && digest(&bytes) == id,
                    "participant object integrity failure"
                );
                let mut cache = cache.lock().await;
                if cache.len() >= 2048 {
                    cache.clear();
                }
                cache.insert(name, bytes.clone());
                Ok::<_, anyhow::Error>((id.to_owned(), bytes))
            }
        }))
        .buffer_unordered(16)
        .try_collect()
        .await?;
        let image = StateImage {
            metadata,
            shards: shards.into_iter().collect(),
        };
        CaState::from_image(&image)?;
        Ok(image)
    }
}

fn outcome<T>(result: std::result::Result<T, kube::Error>) -> Result<CommitOutcome> {
    match result {
        Ok(_) => Ok(CommitOutcome::Committed),
        Err(kube::Error::Api(e)) if e.code == 409 => Ok(CommitOutcome::Conflict),
        Err(kube::Error::Api(e))
            if (400..500).contains(&e.code) && e.code != 408 && e.code != 429 =>
        {
            Err(kube::Error::Api(e).into())
        }
        Err(_) => Ok(CommitOutcome::Uncertain),
    }
}

impl CaStore for KubernetesCaStore {
    async fn collect(&self) -> Result<()> {
        let objects = self
            .maps()
            .list(&ListParams::default().labels("racer.unbounded-cloud.io/pki-participants=v4"))
            .await?;
        self.check_fence().await?;
        let secret = self.secrets().get(CA_SECRET).await?;
        let metadata: serde_json::Value = serde_json::from_slice(
            &secret
                .data
                .as_ref()
                .and_then(|d| d.get(STATE_KEY))
                .context("missing CA metadata")?
                .0,
        )?;
        ensure!(
            metadata["fence"].as_str() == Some(self.term.token.as_str()),
            "collector fence changed"
        );
        let live: std::collections::BTreeSet<_> = metadata["shards"]
            .as_object()
            .context("invalid shard references")?
            .values()
            .filter_map(|v| v.as_str())
            .map(|id| participant_object_name(&self.term.token, id))
            .collect();
        let listed_fence_time = metadata["fence_at"]
            .as_i64()
            .context("missing fence time")?;
        let own_prefix = format!("racer-pki-{}-", &digest(self.term.token.as_bytes())[..16]);
        for object in objects {
            let Some(name) = object.metadata.name.as_deref() else {
                continue;
            };
            let Some(created) = object
                .metadata
                .creation_timestamp
                .as_ref()
                .map(|t| t.0.timestamp())
            else {
                continue;
            };
            if live.contains(name)
                || unix_now() - created < 600
                || (!name.starts_with(&own_prefix) && created >= listed_fence_time)
            {
                continue;
            }
            self.check_fence().await?;
            match self
                .maps()
                .delete(
                    name,
                    &DeleteParams {
                        preconditions: Some(Preconditions {
                            uid: object.metadata.uid.clone(),
                            resource_version: object.metadata.resource_version.clone(),
                        }),
                        ..Default::default()
                    },
                )
                .await
            {
                Ok(_) => {
                    self.shards.lock().await.remove(name);
                }
                Err(kube::Error::Api(e)) if e.code == 404 => (),
                Err(error) => return Err(error.into()),
            }
        }
        Ok(())
    }
    async fn read(&self) -> Result<StoreSnapshot> {
        let secret = self.secrets().get_opt(CA_SECRET).await?;
        let publication = self
            .maps()
            .get_opt(TRUST_MAP)
            .await?
            .map(|cm| {
                Ok::<_, anyhow::Error>(Publication {
                    resource_version: cm
                        .metadata
                        .resource_version
                        .context("trust missing resourceVersion")?,
                    fence: cm
                        .metadata
                        .annotations
                        .as_ref()
                        .and_then(|a| a.get(PUBLIC_FENCE))
                        .cloned()
                        .unwrap_or_default(),
                    bytes: cm
                        .data
                        .as_ref()
                        .and_then(|d| d.get(BUNDLE_KEY))
                        .cloned()
                        .unwrap_or_default()
                        .into_bytes(),
                })
            })
            .transpose()?;
        let (resource_version, image, prior_artifacts) = if let Some(secret) = secret {
            (
                Some(
                    secret
                        .metadata
                        .resource_version
                        .clone()
                        .context("CA missing resourceVersion")?,
                ),
                Some(self.load_image(&secret).await?),
                false,
            )
        } else {
            (None, None, self.artifacts().await?)
        };
        Ok(StoreSnapshot {
            resource_version,
            image,
            publication,
            prior_artifacts,
        })
    }

    async fn commit(&self, expected: &StoreSnapshot, next: &StateImage) -> Result<CommitOutcome> {
        self.check_lease().await?;
        let state = CaState::from_image(next)?;
        ensure!(state.fence() == self.term.token, "commit fence mismatch");
        let secrets = self.secrets();
        let mut secret = if let Some(rv) = &expected.resource_version {
            let secret = secrets.get(CA_SECRET).await?;
            if secret.metadata.resource_version.as_ref() != Some(rv) {
                return Ok(CommitOutcome::Conflict);
            }
            // A takeover may change the fence, but never commit from a different
            // Secret snapshot than the one the manager validated.
            ensure!(
                secret
                    .data
                    .as_ref()
                    .and_then(|d| d.get(STATE_KEY))
                    .map(|v| &v.0)
                    == expected.image.as_ref().map(|i| &i.metadata),
                "CA base changed"
            );
            secret
        } else {
            ensure!(
                !self.artifacts().await?,
                "existing trust artifacts forbid CA bootstrap"
            );
            Secret {
                metadata: ObjectMeta {
                    name: Some(CA_SECRET.into()),
                    namespace: Some(self.namespace.clone()),
                    annotations: Some([(BOOTSTRAP_PENDING.into(), "v4".into())].into()),
                    ..Default::default()
                },
                type_: Some("Opaque".into()),
                ..Default::default()
            }
        };
        // Bootstrap marker survives until trust publication. When retirement
        // collection is added it must be committed with a live-Pod admission
        // requirement; no current path forgets retired process tombstones.
        if expected
            .image
            .as_ref()
            .is_some_and(|old| old.metadata == next.metadata)
        {
            self.check_fence().await?;
            return Ok(CommitOutcome::Committed);
        }
        for (id, bytes) in &next.shards {
            ensure!(
                hex_id(id) && digest(bytes) == *id && bytes.len() <= MAX_OBJECT_BYTES,
                "invalid shard write"
            );
            let name = participant_object_name(&self.term.token, id);
            if self.shards.lock().await.get(&name) == Some(bytes) {
                continue;
            }
            let cm = ConfigMap {
                metadata: ObjectMeta {
                    name: Some(name.clone()),
                    namespace: Some(self.namespace.clone()),
                    labels: Some(
                        [(
                            "racer.unbounded-cloud.io/pki-participants".into(),
                            "v4".into(),
                        )]
                        .into(),
                    ),
                    ..Default::default()
                },
                immutable: Some(true),
                data: Some([(STATE_KEY.into(), String::from_utf8(bytes.clone())?)].into()),
                ..Default::default()
            };
            match self.maps().create(&PostParams::default(), &cm).await {
                Ok(_) => (),
                Err(kube::Error::Api(e)) if e.code == 409 => {
                    let old = self.maps().get(&name).await?;
                    ensure!(
                        old.immutable == Some(true) && old.data == cm.data,
                        "immutable shard collision"
                    );
                }
                Err(error) => return Err(error.into()),
            }
            let mut cache = self.shards.lock().await;
            if cache.len() >= 2048 {
                cache.clear();
            }
            cache.insert(name, bytes.clone());
        }
        self.check_lease().await?;
        if expected.resource_version.is_none() {
            ensure!(
                !self.artifacts().await?,
                "bootstrap raced prior trust artifacts"
            );
        }
        secret.data = Some([(STATE_KEY.into(), ByteString(next.metadata.clone()))].into());
        if expected.resource_version.is_some() {
            outcome(
                secrets
                    .replace(CA_SECRET, &PostParams::default(), &secret)
                    .await,
            )
        } else {
            outcome(secrets.create(&PostParams::default(), &secret).await)
        }
    }

    async fn publish(
        &self,
        expected: &StoreSnapshot,
        fence: &str,
        bytes: &[u8],
    ) -> Result<CommitOutcome> {
        ensure!(fence == self.term.token, "publication term mismatch");
        let bundle = TrustBundle::parse(bytes)?;
        let maps = self.maps();
        // Capture target RV before checking the Secret. Never retry with a new
        // RV after loss of this fence. A successor claims the map before serving.
        let old = maps.get_opt(TRUST_MAP).await?;
        if old
            .as_ref()
            .and_then(|m| m.metadata.resource_version.as_ref())
            != expected.publication.as_ref().map(|p| &p.resource_version)
        {
            return Ok(CommitOutcome::Conflict);
        }
        self.check_fence().await?;
        let mut secret = self.secrets().get(CA_SECRET).await?;
        if secret.metadata.resource_version != expected.resource_version {
            return Ok(CommitOutcome::Conflict);
        }
        let image = self.load_image(&secret).await?;
        ensure!(
            CaState::from_image(&image)?.bundle() == bundle,
            "publication differs from committed CA"
        );
        let pending = secret
            .metadata
            .annotations
            .as_ref()
            .is_some_and(|a| a.contains_key(BOOTSTRAP_PENDING));
        if pending {
            if let Some(old) = &old {
                ensure!(
                    old.data
                        .as_ref()
                        .and_then(|d| d.get(BUNDLE_KEY))
                        .map(String::as_bytes)
                        == Some(bytes),
                    "bootstrap trust tombstone differs from private state"
                );
            } else {
                ensure!(
                    !self.artifacts().await?,
                    "bootstrap raced existing trust artifacts"
                );
            }
        }
        if !pending
            && old.as_ref().is_some_and(|cm| {
                cm.metadata
                    .annotations
                    .as_ref()
                    .and_then(|a| a.get(PUBLIC_FENCE))
                    .map(String::as_str)
                    == Some(fence)
                    && cm
                        .data
                        .as_ref()
                        .and_then(|d| d.get(BUNDLE_KEY))
                        .map(String::as_bytes)
                        == Some(bytes)
            })
        {
            return Ok(CommitOutcome::Committed);
        }
        let exists = old.is_some();
        let mut cm = old.unwrap_or_else(|| ConfigMap {
            metadata: ObjectMeta {
                name: Some(TRUST_MAP.into()),
                namespace: Some(self.namespace.clone()),
                ..Default::default()
            },
            ..Default::default()
        });
        cm.metadata
            .annotations
            .get_or_insert_with(BTreeMap::new)
            .insert(PUBLIC_FENCE.into(), fence.into());
        cm.data
            .get_or_insert_with(BTreeMap::new)
            .insert(BUNDLE_KEY.into(), String::from_utf8(bytes.to_vec())?);
        self.check_lease().await?;
        let result = if exists {
            maps.replace(TRUST_MAP, &PostParams::default(), &cm).await
        } else {
            maps.create(&PostParams::default(), &cm).await
        };
        let result = outcome(result)?;
        if result != CommitOutcome::Committed {
            return Ok(result);
        }
        if pending {
            secret
                .metadata
                .annotations
                .as_mut()
                .unwrap()
                .remove(BOOTSTRAP_PENDING);
            self.check_fence().await?;
            // A conflict leaves bootstrap pending; next publication recovers it.
            return outcome(
                self.secrets()
                    .replace(CA_SECRET, &PostParams::default(), &secret)
                    .await,
            );
        }
        Ok(CommitOutcome::Committed)
    }
}
