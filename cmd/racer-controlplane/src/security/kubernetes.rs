// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! One bounded CA Secret and one public trust ConfigMap, fenced by the Lease.
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
    api::{ListParams, PostParams},
};

pub const CA_SECRET: &str = "racer-ca";
pub const TRUST_MAP: &str = "racer-trust";
pub const STATE_KEY: &str = "state.json";
pub const BUNDLE_KEY: &str = "bundle.json";
pub const LEASE: &str = "racer-controlplane";
const PUBLIC_FENCE: &str = "racer.unbounded.cloud/pki-fence";
const BOOTSTRAP_PENDING: &str = "racer.unbounded.cloud/pki-bootstrap-pending";
const REVISION_CHECKPOINT: &str = "racer-runtime-revisions";

#[derive(Clone)]
pub struct KubernetesCaStore {
    client: Client,
    namespace: String,
    term: Leadership,
}
impl KubernetesCaStore {
    pub fn new(client: Client, namespace: String, term: Leadership) -> Self {
        Self {
            client,
            namespace,
            term,
        }
    }
    fn maps(&self) -> Api<ConfigMap> {
        Api::namespaced(self.client.clone(), &self.namespace)
    }
    fn secrets(&self) -> Api<Secret> {
        Api::namespaced(self.client.clone(), &self.namespace)
    }
    /// Capture the target object's resourceVersion before invoking this check.
    pub async fn check_fence(&self) -> Result<()> {
        self.check_lease().await?;
        let secret = self.secrets().get(CA_SECRET).await?;
        let state = CaState::from_image(&self.load_image(&secret)?)?;
        ensure!(state.fence() == self.term.token(), "CA fence lost");
        self.term.check()
    }
    pub async fn check_lease(&self) -> Result<()> {
        self.term.check()?;
        let lease = Api::<Lease>::namespaced(self.client.clone(), &self.namespace)
            .get(LEASE)
            .await?;
        let spec = lease.spec.context("Lease missing spec")?;
        ensure!(
            spec.holder_identity.as_deref() == Some(self.term.token()),
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
    async fn artifacts(&self) -> Result<bool> {
        let maps = self.maps().list(&ListParams::default()).await?;
        Ok(maps.items.iter().any(|cm| {
            let name = cm.metadata.name.as_deref().unwrap_or_default();
            name == TRUST_MAP || name == REVISION_CHECKPOINT
        }))
    }
    fn load_image(&self, secret: &Secret) -> Result<StateImage> {
        let image = StateImage {
            metadata: secret
                .data
                .as_ref()
                .and_then(|d| d.get(STATE_KEY))
                .context("CA Secret missing state.json")?
                .0
                .clone(),
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
                Some(self.load_image(&secret)?),
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
        ensure!(state.fence() == self.term.token(), "commit fence mismatch");
        let mut secret = if let Some(rv) = &expected.resource_version {
            let secret = self.secrets().get(CA_SECRET).await?;
            if secret.metadata.resource_version.as_ref() != Some(rv) {
                return Ok(CommitOutcome::Conflict);
            }
            ensure!(
                Some(self.load_image(&secret)?) == expected.image,
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
                    annotations: Some([(BOOTSTRAP_PENDING.into(), "1".into())].into()),
                    ..Default::default()
                },
                type_: Some("Opaque".into()),
                ..Default::default()
            }
        };
        if expected.image.as_ref() == Some(next) {
            self.check_fence().await?;
            return Ok(CommitOutcome::Committed);
        }
        secret.data = Some([(STATE_KEY.into(), ByteString(next.metadata.clone()))].into());
        self.check_lease().await?;
        if expected.resource_version.is_some() {
            outcome(
                self.secrets()
                    .replace(CA_SECRET, &PostParams::default(), &secret)
                    .await,
            )
        } else {
            outcome(self.secrets().create(&PostParams::default(), &secret).await)
        }
    }
    async fn publish(
        &self,
        expected: &StoreSnapshot,
        fence: &str,
        bytes: &[u8],
    ) -> Result<CommitOutcome> {
        ensure!(fence == self.term.token(), "publication term mismatch");
        let bundle = TrustBundle::parse(bytes)?;
        let old = self.maps().get_opt(TRUST_MAP).await?;
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
        ensure!(
            CaState::from_image(&self.load_image(&secret)?)?.bundle() == bundle,
            "publication differs from committed CA"
        );
        let pending = secret
            .metadata
            .annotations
            .as_ref()
            .and_then(|a| a.get(BOOTSTRAP_PENDING));
        ensure!(
            pending.is_none_or(|value| value == "1"),
            "unsupported bootstrap marker"
        );
        let pending = pending.is_some();
        if pending {
            // Only a durable fresh-domain marker authorizes checkpoint creation.
            // Interrupted bootstrap can observe its exact empty checkpoint again.
            let checkpoint = ConfigMap {
                metadata: ObjectMeta {
                    name: Some(REVISION_CHECKPOINT.into()),
                    namespace: Some(self.namespace.clone()),
                    ..Default::default()
                },
                data: Some(
                    [
                        ("format".into(), "1".into()),
                        ("high-water".into(), "0".into()),
                        ("fence".into(), String::new()),
                    ]
                    .into(),
                ),
                ..Default::default()
            };
            if self.maps().get_opt(REVISION_CHECKPOINT).await?.is_none() {
                self.check_fence().await?;
                match self
                    .maps()
                    .create(&PostParams::default(), &checkpoint)
                    .await
                {
                    Ok(_) => (),
                    Err(kube::Error::Api(e)) if e.code == 409 => (),
                    Err(_) => (), // Authoritative readback resolves uncertain creation.
                }
            }
            let observed = self.maps().get(REVISION_CHECKPOINT).await?;
            ensure!(
                observed.data == checkpoint.data
                    && observed.binary_data.as_ref().is_none_or(|d| d.is_empty()),
                "bootstrap revision checkpoint differs"
            );
        }
        if let Some(old) = &old {
            let previous = TrustBundle::parse(
                old.data
                    .as_ref()
                    .and_then(|d| d.get(BUNDLE_KEY))
                    .context("invalid trust tombstone")?
                    .as_bytes(),
            )?;
            ensure!(
                previous.generation <= bundle.generation
                    && (previous.generation != bundle.generation || previous == bundle),
                "trust rollback or equivocation"
            );
            if !pending
                && previous == bundle
                && old
                    .metadata
                    .annotations
                    .as_ref()
                    .and_then(|a| a.get(PUBLIC_FENCE))
                    .map(String::as_str)
                    == Some(fence)
            {
                return Ok(CommitOutcome::Committed);
            }
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
            .get_or_insert_with(Default::default)
            .insert(PUBLIC_FENCE.into(), fence.into());
        cm.data = Some([(BUNDLE_KEY.into(), String::from_utf8(bytes.to_vec())?)].into());
        self.check_lease().await?;
        let result = if exists {
            outcome(
                self.maps()
                    .replace(TRUST_MAP, &PostParams::default(), &cm)
                    .await,
            )?
        } else {
            outcome(self.maps().create(&PostParams::default(), &cm).await)?
        };
        if pending {
            let observed = self.maps().get(TRUST_MAP).await?;
            ensure!(
                observed.data == cm.data
                    && observed
                        .metadata
                        .annotations
                        .as_ref()
                        .and_then(|a| a.get(PUBLIC_FENCE))
                        .map(String::as_str)
                        == Some(fence),
                "bootstrap trust publication unresolved"
            );
            secret
                .metadata
                .annotations
                .as_mut()
                .unwrap()
                .remove(BOOTSTRAP_PENDING);
            self.check_fence().await?;
            let result = outcome(
                self.secrets()
                    .replace(CA_SECRET, &PostParams::default(), &secret)
                    .await,
            )?;
            let observed = self.secrets().get(CA_SECRET).await?;
            ensure!(
                !observed
                    .metadata
                    .annotations
                    .as_ref()
                    .is_some_and(|a| a.contains_key(BOOTSTRAP_PENDING)),
                "bootstrap completion unresolved"
            );
            self.check_fence().await?;
            return Ok(result);
        }
        Ok(result)
    }
}
