// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::time::Duration;

use anyhow::{Context, Result, bail, ensure};
use futures::{StreamExt, TryStreamExt};
use k8s_openapi::{
    ByteString, api::core::v1::ConfigMap, apimachinery::pkg::apis::meta::v1::ObjectMeta,
};
use kube::core::SelectorExt;
use kube::{
    Api, Client, ResourceExt,
    api::{
        ApiResource, DeleteParams, DynamicObject, ListParams, Patch, PatchParams, PostParams,
        Preconditions,
    },
    core::GroupVersionKind,
    runtime::watcher,
};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::sync::{Mutex, OwnedMutexGuard, mpsc};
use tokio_util::sync::CancellationToken;

use crate::{
    model::{self, Cache, Generation, Inventory, Node, Pod, identity, universe_for_site},
    publication::{CommitOutcome, Durable, Publication},
    security::CaState,
    status,
    storage::StoragePolicy,
    subscription::{SecurityGetter, Subscriptions},
    topology::compile,
};

const PREFIX: &str = "racer.unbounded-cloud.io/";
const RECORD_LABEL: &str = "racer.unbounded-cloud.io/rust-state";
const CHUNK_BYTES: usize = 512 * 1024;
const MAX_RECORD_BYTES: usize = 512 * 1024 * 1024;
const STORE_GATE: &str = "racer-v4-store-gate";
const GATE_TIMEOUT: Duration = Duration::from_secs(30);
const GATE_RETRY: Duration = Duration::from_millis(100);
const PAGE_SIZE: u32 = 64;

#[derive(Clone)]
pub struct SecurityContext {
    pub fence: String,
    pub state: Arc<CaState>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Selection {
    pub node_name: String,
    pub pod_namespace: String,
    pub pod_name: String,
    pub pod_uid: String,
    pub universe: String,
}

#[derive(Clone)]
pub struct RuntimeOptions {
    pub namespace: String,
    pub socket_root: String,
    pub snapshot_cache_bytes: usize,
    pub long_poll: Duration,
    pub retry_interval: Duration,
}

impl RuntimeOptions {
    pub fn new(namespace: impl Into<String>) -> Self {
        Self {
            namespace: namespace.into(),
            socket_root: model::SOCKET_ROOT.into(),
            snapshot_cache_bytes: 64 * 1024 * 1024,
            long_poll: Duration::from_secs(28),
            retry_interval: Duration::from_secs(5),
        }
    }
}

#[derive(Clone)]
pub struct Runtime {
    options: RuntimeOptions,
    pub subscriptions: Arc<Subscriptions>,
    ready: Arc<AtomicBool>,
}

impl Runtime {
    pub fn new(options: RuntimeOptions, security: SecurityGetter) -> Self {
        Self {
            subscriptions: Subscriptions::new(
                security,
                options.snapshot_cache_bytes,
                options.long_poll,
            ),
            options,
            ready: Arc::new(AtomicBool::new(false)),
        }
    }
    pub fn router(&self) -> axum::Router {
        self.subscriptions.router()
    }
    pub fn ready(&self) -> bool {
        self.ready.load(Ordering::Acquire)
            && (self.subscriptions.security)().is_some_and(|s| {
                self.subscriptions.fence.read().unwrap().as_deref() == Some(s.fence.as_str())
            })
    }
    pub fn selection(&self, universe: &str, node: &str) -> Option<Selection> {
        self.ready()
            .then(|| self.subscriptions.selection(universe, node))
            .flatten()
    }

    /// Watches start on followers too. Each watch atomically replaces its indexed
    /// inventory on relist. Dirty universe names are a deterministic latest set.
    pub async fn run(self, client: Client, shutdown: CancellationToken) -> Result<()> {
        let (send, mut receive) = mpsc::channel(1024);
        let mut tasks = tokio::task::JoinSet::new();
        for kind in Kind::ALL {
            let api = dynamic_api(client.clone(), kind, &self.options.namespace);
            let send = send.clone();
            let stop = shutdown.clone();
            tasks.spawn(async move {
                let mut stream = watcher::watcher(api, watcher::Config::default()).boxed();
                loop {
                    tokio::select! {
                        _ = stop.cancelled() => break,
                        event = stream.try_next() => match event {
                            Ok(Some(event)) => if send.send((kind, event)).await.is_err() { break; },
                            Ok(None) => break,
                            Err(error) => { tracing::warn!(?kind, %error, "inventory watch retry"); tokio::time::sleep(Duration::from_secs(1)).await; }
                        }
                    }
                }
            });
        }
        drop(send);
        let mut index = InventoryIndex::default();
        let mut fence = String::new();
        let mut dirty = BTreeSet::new();
        let mut storage_dirty = BTreeSet::new();
        let mut tick = tokio::time::interval(self.options.retry_interval);
        let mut last_gc = std::time::Instant::now();
        let store = RecordStore::new(
            client.clone(),
            &self.options.namespace,
            self.subscriptions.security.clone(),
        );
        loop {
            tokio::select! {
                _ = shutdown.cancelled() => break,
                event = receive.recv() => {
                    let Some((kind, event)) = event else { break; };
                    index.event(kind, event, &mut dirty, &mut storage_dirty)?;
                    // Drain only a bounded batch; continuous events cannot starve commits.
                    for _ in 0..1024 { match receive.try_recv() { Ok((kind, event)) => index.event(kind, event, &mut dirty, &mut storage_dirty)?, Err(_) => break } }
                },
                _ = tick.tick() => {
                    // Storage/status retry uses indexed current Nodes, not request scans.
                    storage_dirty.extend(index.objects[&Kind::Node].keys().cloned());
                }
            }
            let context = (self.subscriptions.security)();
            let Some(context) = context else {
                if !fence.is_empty() {
                    fence.clear();
                    self.subscriptions.set_fence(None);
                    self.ready.store(false, Ordering::Release);
                }
                continue;
            };
            if index.initialized.len() != 4 {
                continue;
            }
            if fence != context.fence {
                self.ready.store(false, Ordering::Release);
                self.subscriptions.set_fence(None);
                let load = self.reload(&store, &context.fence).await;
                if let Err(error) = load {
                    tracing::error!(%error, "authoritative runtime load failed");
                    continue;
                }
                fence = context.fence;
                dirty.extend(index.universes());
                dirty.extend(
                    self.subscriptions
                        .universes
                        .read()
                        .unwrap()
                        .values()
                        .map(|p| p.topology.generation().universe.clone()),
                );
                storage_dirty.extend(index.objects[&Kind::Node].keys().cloned());
                self.subscriptions.set_fence(Some(fence.clone()));
            }
            let work = std::mem::take(&mut dirty);
            for universe in work {
                if let Err(error) = self.reconcile(&store, &index, &universe, &fence).await {
                    tracing::warn!(%universe, %error, "topology reconcile retained last good state");
                    dirty.insert(universe);
                }
            }
            // Invalid intent keeps last-good publications usable. A universe
            // without a committed publication still fails its own selection.
            self.ready.store(
                !self.subscriptions.universes.read().unwrap().is_empty()
                    || index.universes().is_empty(),
                Ordering::Release,
            );
            let nodes = std::mem::take(&mut storage_dirty);
            let results: Vec<_> = futures::stream::iter(nodes.into_iter().map(|node| {
                let (runtime, store, index, fence) = (&self, &store, &index, &fence);
                async move {
                    let result = runtime.reconcile_storage(store, index, &node, fence).await;
                    (node, result)
                }
            }))
            .buffer_unordered(16)
            .collect()
            .await;
            for (node, result) in results {
                if let Err(error) = result {
                    tracing::warn!(%node, %error, "storage reconcile retry");
                    storage_dirty.insert(node);
                }
            }
            if let Err(error) = self.publish_cache_status(&client, &index, &fence).await {
                tracing::warn!(%error, "cache status retry");
            }
            if last_gc.elapsed() >= Duration::from_secs(300) {
                if let Err(error) = store.collect_garbage(&fence).await {
                    tracing::warn!(%error, "chunk collection retry");
                }
                last_gc = std::time::Instant::now();
            }
        }
        self.ready.store(false, Ordering::Release);
        self.subscriptions.set_fence(None);
        shutdown.cancel();
        tasks.abort_all();
        while tasks.join_next().await.is_some() {}
        Ok(())
    }

    async fn reload(&self, store: &RecordStore, fence: &str) -> Result<()> {
        let mut continuation = String::new();
        loop {
            let records = store
                .api
                .list(
                    &ListParams::default()
                        .labels(&format!("{RECORD_LABEL}=pointer"))
                        .limit(PAGE_SIZE)
                        .continue_token(&continuation),
                )
                .await?;
            continuation = records.metadata.continue_.clone().unwrap_or_default();
            for pointer in records {
                let name = pointer.name_any();
                let Some(record) = store.load(&name).await? else {
                    continue;
                };
                let record = store.claim(&name, record, fence).await?;
                match record.pointer.kind.as_str() {
                    "topology" => {
                        let generation: Generation = serde_json::from_slice(&record.bytes)?;
                        generation.validate()?;
                        self.subscriptions.install(Arc::new(generation))?;
                    }
                    "storage" => {
                        let policy: StoragePolicy = serde_json::from_slice(&record.bytes)?;
                        policy.validate()?;
                        self.subscriptions.install_policy(Arc::new(policy));
                    }
                    _ => bail!("unknown runtime record kind"),
                }
            }
            if continuation.is_empty() {
                break;
            }
        }
        store.check(fence)?;
        Ok(())
    }

    async fn reconcile(
        &self,
        store: &RecordStore,
        index: &InventoryIndex,
        universe: &str,
        fence: &str,
    ) -> Result<()> {
        let key = format!("racer-v4-topology-{}", identity("universe", universe));
        let old = store.load(&key).await?;
        ensure!(
            old.is_some()
                || !self
                    .subscriptions
                    .universes
                    .read()
                    .unwrap()
                    .contains_key(&identity("universe", universe)),
            "committed topology disappeared"
        );
        let old = match old {
            Some(old) => Some(store.claim(&key, old, fence).await?),
            None => None,
        };
        let previous: Option<Generation> = old
            .as_ref()
            .map(|r| serde_json::from_slice(&r.bytes))
            .transpose()?;
        let mut input = index.inventory(universe, &self.options.socket_root)?;
        // Enrollment already durably binds known Pods to Node UIDs. Reuse that
        // authority after historical topology rows are removed.
        let security = (self.subscriptions.security)().context("leadership lost")?;
        let bindings: BTreeMap<_, _> = security
            .state
            .members()
            .filter(|p| p.identity.kind == crate::security::IdentityKind::Node)
            .map(|p| (p.identity.pod_uid.as_str(), p.identity.node.as_str()))
            .collect();
        for node in &mut input.nodes {
            let id = identity("node", &node.uid);
            for pod in &mut node.pods {
                if bindings.get(pod.uid.as_str()).is_some_and(|old| *old != id) {
                    pod.available = false;
                }
            }
        }
        let (mut desired, previous) = tokio::task::spawn_blocking(move || {
            compile(&input, previous.as_ref()).map(|desired| (desired, previous))
        })
        .await??;
        // Deconfiguration is synthesized from durable enrollment and the current
        // universe revision. It requires no historical recipient ledger.
        desired.nodes.retain(|_, member| member.ip.is_some());
        desired.withdrawn.clear();
        desired
            .slot_history
            .retain(|id, _| desired.volumes.iter().any(|v| &v.id == id));
        let next = persist(
            store,
            &key,
            "topology",
            old.as_ref(),
            previous,
            desired,
            fence,
        )
        .await?;
        let hex = identity("universe", universe);
        let current = self
            .subscriptions
            .universes
            .read()
            .unwrap()
            .get(&hex)
            .map(|p| p.topology.generation().digest());
        if current != Some(next.digest()) {
            store.check(fence)?;
            self.subscriptions.install(Arc::new(next))?;
        }
        Ok(())
    }

    async fn reconcile_storage(
        &self,
        store: &RecordStore,
        index: &InventoryIndex,
        name: &str,
        fence: &str,
    ) -> Result<()> {
        let Some(node) = index.objects[&Kind::Node].get(name) else {
            return Ok(());
        };
        let uid = node.uid().context("Node lacks UID")?;
        let node_id = identity("node", &uid);
        let site_name = site_for_node(node);
        let site = index.objects[&Kind::Site].get(&site_name);
        let override_value = node
            .annotations()
            .get(&format!("{PREFIX}cache-size"))
            .map(String::as_str);
        let site_value = site
            .and_then(|s| s.data.pointer("/spec/components/racer/cacheSize"))
            .and_then(Value::as_str);
        let universe = universe_for_site(&site_name);
        let cached = self
            .subscriptions
            .policies
            .read()
            .unwrap()
            .get(&node_id)
            .cloned();
        let unchanged = cached.as_ref().filter(|p| {
            p.resolve(&universe, override_value, site_value)
                .is_ok_and(|next| &next == p.as_ref())
        });
        let next = if let Some(policy) = unchanged {
            policy.as_ref().clone()
        } else {
            let key = format!("racer-v4-storage-{node_id}");
            let old = store.load(&key).await?;
            ensure!(
                old.is_some() || cached.is_none(),
                "committed storage policy disappeared"
            );
            let old = match old {
                Some(old) => Some(store.claim(&key, old, fence).await?),
                None => None,
            };
            let previous: Option<StoragePolicy> = old
                .as_ref()
                .map(|r| serde_json::from_slice(&r.bytes))
                .transpose()?;
            let base = previous
                .clone()
                .unwrap_or_else(|| StoragePolicy::new(node_id.clone(), rand::random()));
            let desired = base.resolve(&universe, override_value, site_value)?;
            persist(
                store,
                &key,
                "storage",
                old.as_ref(),
                previous,
                desired,
                fence,
            )
            .await?
        };
        let changed = self
            .subscriptions
            .policies
            .read()
            .unwrap()
            .get(&node_id)
            .is_none_or(|p| p.as_ref() != &next);
        if changed {
            store.check(fence)?;
            self.subscriptions.install_policy(Arc::new(next.clone()));
        }
        let universe_id = identity("universe", &universe);
        let selection = self.subscriptions.selection(&universe_id, &node_id);
        let report = self
            .subscriptions
            .reports
            .lock()
            .unwrap()
            .get(&(universe_id, node_id))
            .filter(|r| selection.as_ref().is_some_and(|s| s.pod_uid == r.pod_uid))
            .cloned();
        let (source, requested) = match (override_value, site_value) {
            (Some(v), _) => ("node", v),
            (_, Some(v)) => ("site", v),
            _ => ("default", "10Gi"),
        };
        let status = status::cache_status(
            &next,
            report.as_ref(),
            source,
            requested,
            std::time::Instant::now(),
        )
        .to_string();
        let annotation = format!("{PREFIX}cache-status");
        if node.annotations().get(&annotation) != Some(&status) {
            store.check(fence)?;
            dynamic_api(store.client.clone(), Kind::Node, &self.options.namespace).patch(name, &PatchParams::default(), &Patch::Merge(json!({
                "metadata": { "uid": uid, "resourceVersion": node.resource_version(), "annotations": { annotation: status } }
            }))).await?;
        }
        Ok(())
    }

    async fn publish_cache_status(
        &self,
        client: &Client,
        index: &InventoryIndex,
        fence: &str,
    ) -> Result<()> {
        for cache in index.objects[&Kind::Cache].values() {
            if cache.metadata.deletion_timestamp.is_some() {
                continue;
            }
            let uid = cache.uid().context("cache lacks UID")?;
            let mut desired = 0;
            let mut ready = 0;
            let mut accepted = true;
            for universe in index.selected_universes(cache)? {
                let input = index.inventory(&universe, &self.options.socket_root)?;
                desired += input.nodes.iter().filter(|n| n.eligible).count();
                let hex = identity("universe", &universe);
                let published = self
                    .subscriptions
                    .universes
                    .read()
                    .unwrap()
                    .get(&hex)
                    .cloned();
                if let Some(published) = published {
                    let generation = published.topology.generation();
                    accepted &= generation.volumes.iter().any(|v| {
                        v.id == uid
                            && v.resource_generation == cache.metadata.generation.unwrap_or(0)
                    });
                    for (node, selection) in &published.selections {
                        let pod_ready = index.objects[&Kind::Pod]
                            .get(&format!(
                                "{}/{}",
                                selection.pod_namespace, selection.pod_name
                            ))
                            .is_some_and(|p| condition(p, "Ready"));
                        let digest = self.subscriptions.expected_digest(&hex, node);
                        let reports = self.subscriptions.reports.lock().unwrap();
                        if pod_ready
                            && digest.as_ref().is_some_and(|(revision, digest)| {
                                reports.get(&(hex.clone(), node.clone())).is_some_and(|r| {
                                    r.converged(
                                        &selection.pod_uid,
                                        *revision,
                                        digest,
                                        std::time::Instant::now(),
                                    )
                                })
                            })
                        {
                            ready += 1;
                        }
                    }
                } else {
                    accepted = false;
                }
            }
            let generation = cache.metadata.generation.unwrap_or(0);
            let all_ready = accepted && desired > 0 && ready == desired;
            let mut conditions = Vec::new();
            for (kind, value, reason) in [
                (
                    "Accepted",
                    accepted,
                    if accepted {
                        "Compiled"
                    } else {
                        "InvalidConfiguration"
                    },
                ),
                (
                    "Ready",
                    all_ready,
                    if all_ready {
                        "Converged"
                    } else {
                        "Progressing"
                    },
                ),
            ] {
                let state = if value { "True" } else { "False" };
                let prior = cache
                    .data
                    .pointer("/status/conditions")
                    .and_then(Value::as_array)
                    .and_then(|conditions| {
                        conditions
                            .iter()
                            .find(|c| c["type"] == kind && c["status"] == state)
                    });
                let transition = prior
                    .and_then(|c| c.get("lastTransitionTime"))
                    .cloned()
                    .unwrap_or_else(|| {
                        json!(
                            time::OffsetDateTime::now_utc()
                                .format(&time::format_description::well_known::Rfc3339)
                                .unwrap()
                        )
                    });
                conditions.push(json!({ "type": kind, "status": state, "reason": reason, "message": reason, "observedGeneration": generation, "lastTransitionTime": transition }));
            }
            let status = json!({ "observedGeneration": generation, "participants": { "desired": desired, "ready": ready }, "conditions": conditions });
            if cache.data.get("status") != Some(&status) {
                ensure!(
                    (self.subscriptions.security)().is_some_and(|s| s.fence == fence),
                    "leadership lost"
                );
                dynamic_api(client.clone(), Kind::Cache, &self.options.namespace).patch_status(&cache.name_any(), &PatchParams::default(), &Patch::Merge(json!({
                    "metadata": { "uid": cache.uid(), "resourceVersion": cache.resource_version() }, "status": status
                }))).await?;
            }
        }
        Ok(())
    }
}

async fn persist<T: Durable + DeserializeOwned>(
    store: &RecordStore,
    key: &str,
    kind: &str,
    old: Option<&Record>,
    previous: Option<T>,
    desired: T,
    fence: &str,
) -> Result<T> {
    let mut publication = Publication::default();
    let ticket = publication.acquire_leadership();
    publication.loaded(ticket, previous)?;
    let Some(commit) = publication.prepare(desired)? else {
        return Ok(publication.published().unwrap().as_ref().clone());
    };
    let bytes = serde_json::to_vec(commit.value.as_ref())?;
    match store.commit(key, kind, old, &bytes, fence).await {
        Ok(_) => {
            publication.complete(commit.ticket, CommitOutcome::Committed);
        }
        Err(error) => {
            publication.complete(commit.ticket, CommitOutcome::ReloadRequired);
            let readback = store.load(key).await?;
            store.check(fence)?;
            if let Some(record) = readback.filter(|r| r.pointer.fence == fence && r.bytes == bytes)
            {
                let ticket = publication.begin_reload()?;
                publication.loaded(ticket, Some(serde_json::from_slice(&record.bytes)?))?;
            } else {
                return Err(error);
            }
        }
    }
    store.check(fence)?;
    Ok(publication.published().unwrap().as_ref().clone())
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Pointer {
    pub format: u32,
    pub kind: String,
    pub fence: String,
    pub digest: String,
    pub chunks: Vec<String>,
    pub bytes: usize,
    /// Reserved before staging. Replacing this pointer fences abandoned writers;
    /// GC protects both committed and proposed chunks across leader takeover.
    #[serde(default)]
    pub proposed: Vec<String>,
}

#[derive(Clone)]
pub struct Record {
    pub resource_version: String,
    pub pointer: Pointer,
    pub bytes: Vec<u8>,
}

/// Direct Kubernetes I/O seam. Immutable content is durable before the sole
/// pointer CAS. Conflicts and transport errors always require authoritative load.
#[derive(Clone)]
pub struct RecordStore {
    client: Client,
    api: Api<ConfigMap>,
    security: SecurityGetter,
    gate_state: Arc<Mutex<Option<GateAttempt>>>,
}

struct GateAttempt {
    fence: String,
    operation: String,
    previous_version: Option<String>,
}

/// Register ownership before sending acquisition I/O. Cancellation transfers the
/// local lock to bounded cleanup; failed cleanup remains for the next store call.
/// Only a new leadership fence can recover state lost in a process crash.
struct StoreGuard {
    api: Api<ConfigMap>,
    state: Option<OwnedMutexGuard<Option<GateAttempt>>>,
}
impl Drop for StoreGuard {
    fn drop(&mut self) {
        if let Some(mut state) = self.state.take().filter(|state| state.is_some()) {
            let api = self.api.clone();
            tokio::spawn(async move {
                if tokio::time::timeout(GATE_TIMEOUT, recover_gate(&api, &mut state))
                    .await
                    .is_err()
                {
                    tracing::warn!("store gate cleanup timed out; next store call will retry");
                }
            });
        }
    }
}

async fn recover_gate(api: &Api<ConfigMap>, pending: &mut Option<GateAttempt>) {
    let Some(attempt) = pending.as_ref() else {
        return;
    };
    loop {
        match release_gate_attempt(api, attempt).await {
            Ok(()) => {
                *pending = None;
                return;
            }
            Err(error) => {
                tracing::debug!(%error, "store gate cleanup retry");
                tokio::time::sleep(GATE_RETRY).await;
            }
        }
    }
}

async fn release_gate_attempt(api: &Api<ConfigMap>, attempt: &GateAttempt) -> Result<()> {
    let current = api.get_opt(STORE_GATE).await?;
    if let Some(mut object) = current {
        ensure!(
            object.labels().get(RECORD_LABEL).map(String::as_str) == Some("gate"),
            "store gate collision during cleanup"
        );
        let data = object.data.as_mut().context("missing gate data")?;
        if data.get("fence") == Some(&attempt.fence)
            && data.get("operation") == Some(&attempt.operation)
        {
            data.insert("operation".into(), String::new());
        } else if object.resource_version() != attempt.previous_version {
            // A successor (or an already completed release) invalidated our CAS.
            return Ok(());
        }
        // Even an unchanged predecessor must be CAS-touched: an acquisition with
        // a lost response may still be in flight. Never clear a different owner.
        api.replace(STORE_GATE, &PostParams::default(), &object)
            .await?;
    } else {
        // Fence a delayed first-create by occupying the name. Keep this empty
        // object as a CAS target; deleting it would allow that create to succeed.
        api.create(
            &PostParams::default(),
            &ConfigMap {
                metadata: ObjectMeta {
                    name: Some(STORE_GATE.into()),
                    labels: Some(BTreeMap::from([(RECORD_LABEL.into(), "gate".into())])),
                    ..Default::default()
                },
                data: Some(BTreeMap::from([
                    ("fence".into(), attempt.fence.clone()),
                    ("operation".into(), String::new()),
                ])),
                ..Default::default()
            },
        )
        .await?;
    }
    Ok(())
}

impl RecordStore {
    pub fn new(client: Client, namespace: &str, security: SecurityGetter) -> Self {
        Self {
            api: Api::namespaced(client.clone(), namespace),
            client,
            security,
            gate_state: Arc::new(Mutex::new(None)),
        }
    }
    fn check(&self, fence: &str) -> Result<()> {
        ensure!(
            (self.security)().is_some_and(|s| s.fence == fence && s.state.fence() == fence),
            "leadership lost"
        );
        Ok(())
    }
    async fn authoritative_fence(&self, fence: &str) -> Result<()> {
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

    async fn gate(&self, fence: &str) -> Result<StoreGuard> {
        let operation = uuid::Uuid::new_v4().to_string();
        tokio::time::timeout(GATE_TIMEOUT, async {
            let mut guard = StoreGuard {
                api: self.api.clone(),
                state: Some(self.gate_state.clone().lock_owned().await),
            };
            let state = guard.state.as_mut().unwrap();
            recover_gate(&self.api, state).await;
            loop {
                let old = self.api.get_opt(STORE_GATE).await?;
                self.authoritative_fence(fence).await?;
                if let Some(object) = &old {
                    ensure!(
                        object.labels().get(RECORD_LABEL).map(String::as_str) == Some("gate"),
                        "store gate collision"
                    );
                    let data = object.data.as_ref().context("missing gate data")?;
                    if data.get("fence").map(String::as_str) == Some(fence)
                        && data.get("operation").is_some_and(|s| !s.is_empty())
                    {
                        tokio::time::sleep(Duration::from_millis(10)).await;
                        continue;
                    }
                }
                let object = ConfigMap {
                    metadata: ObjectMeta {
                        name: Some(STORE_GATE.into()),
                        resource_version: old.as_ref().and_then(ResourceExt::resource_version),
                        labels: Some(BTreeMap::from([(RECORD_LABEL.into(), "gate".into())])),
                        ..Default::default()
                    },
                    data: Some(BTreeMap::from([
                        ("fence".into(), fence.into()),
                        ("operation".into(), operation.clone()),
                    ])),
                    ..Default::default()
                };
                **state = Some(GateAttempt {
                    fence: fence.into(),
                    operation: operation.clone(),
                    previous_version: object.resource_version(),
                });
                let result = if old.is_some() {
                    self.api
                        .replace(STORE_GATE, &PostParams::default(), &object)
                        .await
                } else {
                    self.api.create(&PostParams::default(), &object).await
                };
                match result {
                    Ok(_) => {}
                    Err(kube::Error::Api(e)) if e.code == 409 => {
                        **state = None;
                        continue;
                    }
                    Err(error) => {
                        let readback = self.api.get_opt(STORE_GATE).await?;
                        if !readback.is_some_and(|o| {
                            o.data.as_ref().is_some_and(|d| {
                                d.get("operation") == Some(&operation)
                                    && d.get("fence").map(String::as_str) == Some(fence)
                            })
                        }) {
                            return Err(error.into());
                        }
                    }
                }
                self.check(fence)?;
                return Ok(guard);
            }
        })
        .await
        .context("store gate busy")?
    }

    /// Collection holds the same gate as staging and pointer commit. References
    /// are read directly in bounded pages; partial scans never delete anything.
    /// Deletes use the candidate UID/RV, protecting chunks reused after takeover.
    pub async fn collect_garbage(&self, fence: &str) -> Result<usize> {
        let _guard = self.gate(fence).await?;
        let mut referenced = BTreeSet::new();
        let mut continuation = String::new();
        loop {
            let page = self
                .api
                .list(
                    &ListParams::default()
                        .labels(&format!("{RECORD_LABEL}=pointer"))
                        .limit(PAGE_SIZE)
                        .continue_token(&continuation),
                )
                .await?;
            continuation = page.metadata.continue_.clone().unwrap_or_default();
            for mut object in page {
                let mut pointer: Pointer = serde_json::from_str(
                    object
                        .data
                        .as_ref()
                        .and_then(|d| d.get("pointer"))
                        .context("missing pointer")?,
                )?;
                ensure!(
                    pointer.format == 1
                        && pointer.chunks.len() <= MAX_RECORD_BYTES / CHUNK_BYTES + 1,
                    "invalid pointer during collection"
                );
                if !pointer.proposed.is_empty() {
                    // The gate proves no current writer is staging. Clear an
                    // abandoned reservation by CAS before considering its chunks.
                    // A delayed old commit still has the pre-clear target RV.
                    pointer.proposed.clear();
                    pointer.fence = fence.into();
                    object
                        .data
                        .as_mut()
                        .unwrap()
                        .insert("pointer".into(), serde_json::to_string(&pointer)?);
                    self.authoritative_fence(fence).await?;
                    self.api
                        .replace(&object.name_any(), &PostParams::default(), &object)
                        .await?;
                }
                referenced.extend(pointer.chunks);
                ensure!(
                    referenced.len() <= 1_000_000,
                    "GC reference capacity exceeded"
                );
            }
            if continuation.is_empty() {
                break;
            }
        }
        let mut deleted = 0;
        loop {
            // Metadata-only list avoids materializing pages of 512KiB payloads.
            let page = self
                .api
                .list_metadata(
                    &ListParams::default()
                        .labels(&format!("{RECORD_LABEL}=chunk"))
                        .limit(PAGE_SIZE)
                        .continue_token(&continuation),
                )
                .await?;
            continuation = page.metadata.continue_.clone().unwrap_or_default();
            for object in page {
                let name = object.name_any();
                let Some(digest) = name.strip_prefix("racer-v4-chunk-") else {
                    continue;
                };
                if referenced.contains(digest) {
                    continue;
                }
                self.authoritative_fence(fence).await?;
                let params = DeleteParams {
                    preconditions: Some(Preconditions {
                        uid: object.uid(),
                        resource_version: object.resource_version(),
                    }),
                    ..Default::default()
                };
                match self.api.delete(&name, &params).await {
                    Ok(_) => deleted += 1,
                    Err(kube::Error::Api(e)) if matches!(e.code, 404 | 409) => {}
                    Err(error) => return Err(error.into()),
                }
            }
            if continuation.is_empty() {
                break;
            }
        }
        Ok(deleted)
    }
    pub async fn load(&self, name: &str) -> Result<Option<Record>> {
        let fence = (self.security)().context("leadership unavailable")?.fence;
        let _guard = self.gate(&fence).await?;
        let Some(cm) = self.api.get_opt(name).await? else {
            return Ok(None);
        };
        ensure!(
            cm.labels().get(RECORD_LABEL).map(String::as_str) == Some("pointer"),
            "state name collision"
        );
        let pointer: Pointer = serde_json::from_str(
            cm.data
                .as_ref()
                .and_then(|d| d.get("pointer"))
                .context("missing pointer")?,
        )?;
        ensure!(
            pointer.format == 1
                && pointer.bytes <= MAX_RECORD_BYTES
                && pointer.chunks.len() <= MAX_RECORD_BYTES / CHUNK_BYTES + 1,
            "invalid record bounds"
        );
        if pointer.bytes == 0 && pointer.chunks.is_empty() {
            return Ok(None);
        }
        let mut bytes = Vec::with_capacity(pointer.bytes);
        for digest in &pointer.chunks {
            ensure!(
                digest.len() == 64 && digest.bytes().all(|b| b.is_ascii_hexdigit()),
                "invalid chunk identity"
            );
            let chunk = self.api.get(&format!("racer-v4-chunk-{digest}")).await?;
            let data = &chunk
                .binary_data
                .as_ref()
                .and_then(|d| d.get("content"))
                .context("missing chunk content")?
                .0;
            ensure!(
                chunk.immutable == Some(true)
                    && data.len() <= CHUNK_BYTES
                    && hex::encode(Sha256::digest(data)) == *digest,
                "corrupt immutable chunk"
            );
            ensure!(
                bytes.len() + data.len() <= pointer.bytes,
                "chunk data exceeds record bounds"
            );
            bytes.extend_from_slice(data);
        }
        ensure!(
            bytes.len() == pointer.bytes && hex::encode(Sha256::digest(&bytes)) == pointer.digest,
            "record digest mismatch"
        );
        Ok(Some(Record {
            resource_version: cm.resource_version().context("missing resourceVersion")?,
            pointer,
            bytes,
        }))
    }
    pub async fn claim(&self, name: &str, old: Record, fence: &str) -> Result<Record> {
        self.check(fence)?;
        if old.pointer.fence == fence {
            return Ok(old);
        }
        self.commit(name, &old.pointer.kind, Some(&old), &old.bytes, fence)
            .await
    }
    pub async fn commit(
        &self,
        name: &str,
        kind: &str,
        old: Option<&Record>,
        bytes: &[u8],
        fence: &str,
    ) -> Result<Record> {
        ensure!(bytes.len() <= MAX_RECORD_BYTES, "record capacity exceeded");
        let _guard = self.gate(fence).await?;
        self.check(fence)?;
        let proposed: Vec<_> = bytes
            .chunks(CHUNK_BYTES)
            .map(|b| hex::encode(Sha256::digest(b)))
            .collect();
        let mut reserved = old.map(|r| r.pointer.clone()).unwrap_or_else(|| Pointer {
            format: 1,
            kind: kind.into(),
            fence: fence.into(),
            digest: hex::encode(Sha256::digest([])),
            chunks: vec![],
            bytes: 0,
            proposed: vec![],
        });
        reserved.fence = fence.into();
        reserved.proposed = proposed;
        // Reserve the target RV before any chunk is created. Even a first write
        // has a CAS target; abandoned reservations are repaired on reconciliation.
        self.authoritative_fence(fence).await?;
        let reservation = ConfigMap {
            metadata: ObjectMeta {
                name: Some(name.into()),
                resource_version: old.map(|r| r.resource_version.clone()),
                labels: Some(BTreeMap::from([(RECORD_LABEL.into(), "pointer".into())])),
                ..Default::default()
            },
            data: Some(BTreeMap::from([(
                "pointer".into(),
                serde_json::to_string(&reserved)?,
            )])),
            ..Default::default()
        };
        let reservation = if old.is_some() {
            self.api
                .replace(name, &PostParams::default(), &reservation)
                .await?
        } else {
            match self.api.create(&PostParams::default(), &reservation).await {
                Ok(value) => value,
                Err(kube::Error::Api(e)) if e.code == 409 => {
                    let existing = self.api.get(name).await?;
                    let pointer: Pointer = serde_json::from_str(
                        existing
                            .data
                            .as_ref()
                            .and_then(|d| d.get("pointer"))
                            .context("missing reservation")?,
                    )?;
                    ensure!(
                        pointer.bytes == 0 && pointer.chunks.is_empty(),
                        "record already committed"
                    );
                    let mut reservation = reservation;
                    reservation.metadata.resource_version = existing.resource_version();
                    self.authoritative_fence(fence).await?;
                    self.api
                        .replace(name, &PostParams::default(), &reservation)
                        .await?
                }
                Err(error) => return Err(error.into()),
            }
        };
        let mut chunks = Vec::new();
        for content in bytes.chunks(CHUNK_BYTES) {
            let digest = hex::encode(Sha256::digest(content));
            let name = format!("racer-v4-chunk-{digest}");
            let cm = ConfigMap {
                metadata: ObjectMeta {
                    name: Some(name.clone()),
                    labels: Some(BTreeMap::from([(RECORD_LABEL.into(), "chunk".into())])),
                    ..Default::default()
                },
                immutable: Some(true),
                binary_data: Some(BTreeMap::from([(
                    "content".into(),
                    ByteString(content.to_vec()),
                )])),
                ..Default::default()
            };
            match self.api.create(&PostParams::default(), &cm).await {
                Ok(_) => {}
                Err(kube::Error::Api(e)) if e.code == 409 => {
                    let mut existing = self.api.get(&name).await?;
                    ensure!(
                        existing.immutable == Some(true) && existing.binary_data == cm.binary_data,
                        "immutable chunk name collision"
                    );
                    // Metadata can change on immutable ConfigMaps. Touch on every
                    // reuse so a paused predecessor's conditional delete cannot
                    // remove a chunk this writer is about to reference.
                    existing
                        .metadata
                        .annotations
                        .get_or_insert_default()
                        .insert(
                            "racer.unbounded-cloud.io/chunk-use".into(),
                            uuid::Uuid::new_v4().to_string(),
                        );
                    self.api
                        .replace(&name, &PostParams::default(), &existing)
                        .await?;
                }
                Err(error) => return Err(error.into()),
            }
            chunks.push(digest);
        }
        // The target RV was captured before this authoritative fence read. A
        // takeover claims every pointer before serving, invalidating paused CASes.
        self.authoritative_fence(fence).await?;
        self.check(fence)?;
        let pointer = Pointer {
            format: 1,
            kind: kind.into(),
            fence: fence.into(),
            digest: hex::encode(Sha256::digest(bytes)),
            chunks,
            bytes: bytes.len(),
            proposed: Vec::new(),
        };
        let cm = ConfigMap {
            metadata: ObjectMeta {
                name: Some(name.into()),
                resource_version: reservation.resource_version(),
                labels: Some(BTreeMap::from([(RECORD_LABEL.into(), "pointer".into())])),
                ..Default::default()
            },
            data: Some(BTreeMap::from([(
                "pointer".into(),
                serde_json::to_string(&pointer)?,
            )])),
            ..Default::default()
        };
        let stored = self.api.replace(name, &PostParams::default(), &cm).await?;
        self.check(fence)?;
        Ok(Record {
            resource_version: stored
                .resource_version()
                .context("missing stored resourceVersion")?,
            pointer,
            bytes: bytes.into(),
        })
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Kind {
    Node,
    Pod,
    Site,
    Cache,
}
impl Kind {
    const ALL: [Kind; 4] = [Self::Node, Self::Pod, Self::Site, Self::Cache];
}

fn dynamic_api(client: Client, kind: Kind, namespace: &str) -> Api<DynamicObject> {
    let (group, version, resource, object) = match kind {
        Kind::Node => ("", "v1", "nodes", "Node"),
        Kind::Pod => ("", "v1", "pods", "Pod"),
        Kind::Site => ("unbounded-cloud.io", "v1alpha3", "sites", "Site"),
        Kind::Cache => (
            "racer.unbounded-cloud.io",
            "v1alpha1",
            "p2pcaches",
            "P2PCache",
        ),
    };
    let ar =
        ApiResource::from_gvk_with_plural(&GroupVersionKind::gvk(group, version, object), resource);
    if kind == Kind::Pod {
        Api::namespaced_with(client, namespace, &ar)
    } else {
        Api::all_with(client, &ar)
    }
}

fn key(kind: Kind, object: &DynamicObject) -> String {
    if kind == Kind::Pod {
        format!(
            "{}/{}",
            object.namespace().unwrap_or_default(),
            object.name_any()
        )
    } else {
        object.name_any()
    }
}
fn site_for_node(node: &DynamicObject) -> String {
    model::node_site(
        node.labels()
            .get("unbounded-cloud.io/site")
            .map(String::as_str),
        node.labels()
            .get("net.unbounded-cloud.io/site")
            .map(String::as_str),
    )
}
fn condition(object: &DynamicObject, kind: &str) -> bool {
    object
        .data
        .pointer("/status/conditions")
        .and_then(Value::as_array)
        .is_some_and(|conditions| {
            conditions
                .iter()
                .any(|c| c["type"] == kind && c["status"] == "True")
        })
}
fn site_enabled(site: &DynamicObject) -> bool {
    site.metadata.deletion_timestamp.is_none()
        && site
            .data
            .pointer("/spec/components/racer")
            .is_some_and(|r| !r.is_null() && r.get("enabled") != Some(&Value::Bool(false)))
}

/// Watch-owned old/new indexes. Relist swaps are atomic at InitDone, avoiding
/// transient deconfiguration while the API is streaming the replacement list.
pub struct InventoryIndex {
    objects: BTreeMap<Kind, BTreeMap<String, DynamicObject>>,
    staging: BTreeMap<Kind, BTreeMap<String, DynamicObject>>,
    initialized: BTreeSet<Kind>,
    nodes_by_universe: BTreeMap<String, BTreeSet<String>>,
    pods_by_node: BTreeMap<String, BTreeSet<String>>,
}

impl Default for InventoryIndex {
    fn default() -> Self {
        Self {
            objects: Kind::ALL
                .into_iter()
                .map(|k| (k, BTreeMap::new()))
                .collect(),
            staging: BTreeMap::new(),
            initialized: BTreeSet::new(),
            nodes_by_universe: BTreeMap::new(),
            pods_by_node: BTreeMap::new(),
        }
    }
}

impl InventoryIndex {
    pub fn event(
        &mut self,
        kind: Kind,
        event: watcher::Event<DynamicObject>,
        dirty: &mut BTreeSet<String>,
        storage: &mut BTreeSet<String>,
    ) -> Result<()> {
        match event {
            watcher::Event::Init => {
                self.staging.insert(kind, BTreeMap::new());
            }
            watcher::Event::InitApply(object) => {
                self.staging
                    .entry(kind)
                    .or_default()
                    .insert(key(kind, &object), object);
            }
            watcher::Event::InitDone => {
                let next = self.staging.remove(&kind).unwrap_or_default();
                let old_keys: Vec<_> = self.objects[&kind]
                    .keys()
                    .filter(|k| !next.contains_key(*k))
                    .cloned()
                    .collect();
                for name in old_keys {
                    self.update(kind, &name, None, dirty, storage)?;
                }
                for (name, object) in next {
                    self.update(kind, &name, Some(object), dirty, storage)?;
                }
                self.initialized.insert(kind);
            }
            watcher::Event::Apply(object) => {
                let name = key(kind, &object);
                self.update(kind, &name, Some(object), dirty, storage)?;
            }
            watcher::Event::Delete(object) => {
                let name = key(kind, &object);
                if self.objects[&kind]
                    .get(&name)
                    .is_some_and(|old| old.uid() == object.uid())
                {
                    self.update(kind, &name, None, dirty, storage)?;
                }
            }
        }
        Ok(())
    }

    fn scopes(&self, kind: Kind, object: &DynamicObject) -> Result<BTreeSet<String>> {
        Ok(match kind {
            Kind::Node => BTreeSet::from([universe_for_site(&site_for_node(object))]),
            Kind::Pod => self.objects[&Kind::Node]
                .get(
                    object
                        .data
                        .pointer("/spec/nodeName")
                        .and_then(Value::as_str)
                        .unwrap_or_default(),
                )
                .map(|n| BTreeSet::from([universe_for_site(&site_for_node(n))]))
                .unwrap_or_default(),
            Kind::Site => BTreeSet::from([universe_for_site(&object.name_any())]),
            Kind::Cache => self.selected_universes(object)?,
        })
    }

    fn update(
        &mut self,
        kind: Kind,
        name: &str,
        next: Option<DynamicObject>,
        dirty: &mut BTreeSet<String>,
        storage: &mut BTreeSet<String>,
    ) -> Result<()> {
        let old = self.objects.get_mut(&kind).unwrap().remove(name);
        // Controller-owned metadata/status changes do not recompile topology.
        let relevant = |o: &DynamicObject| {
            let annotations: BTreeMap<_, _> = o
                .annotations()
                .iter()
                .filter(|(k, _)| k.as_str() != format!("{PREFIX}cache-status"))
                .collect();
            json!([
                o.uid(),
                o.labels(),
                annotations,
                o.metadata.deletion_timestamp,
                o.data.get("spec"),
                if matches!(kind, Kind::Node | Kind::Pod) {
                    o.data.get("status")
                } else {
                    None
                }
            ])
        };
        let changed = old.as_ref().map(relevant) != next.as_ref().map(relevant);
        for object in old.iter().chain(next.iter()) {
            if changed {
                // Invalid new selectors must retain last-good publications and
                // surface rejection through reconciliation, not stop all watches.
                let scopes = self
                    .scopes(kind, object)
                    .unwrap_or_else(|_| self.universes());
                for scope in scopes {
                    if !scope.is_empty() {
                        if kind == Kind::Site {
                            storage.extend(
                                self.nodes_by_universe
                                    .get(&scope)
                                    .into_iter()
                                    .flatten()
                                    .cloned(),
                            );
                        }
                        dirty.insert(scope);
                    }
                }
                if kind == Kind::Node {
                    storage.insert(name.into());
                }
            }
        }
        if let Some(old) = &old {
            match kind {
                Kind::Node => {
                    if let Some(set) = self
                        .nodes_by_universe
                        .get_mut(&universe_for_site(&site_for_node(old)))
                    {
                        set.remove(name);
                    }
                }
                Kind::Pod => {
                    if let Some(set) = self.pods_by_node.get_mut(
                        old.data
                            .pointer("/spec/nodeName")
                            .and_then(Value::as_str)
                            .unwrap_or_default(),
                    ) {
                        set.remove(name);
                    }
                }
                _ => {}
            }
        }
        if let Some(next) = next {
            match kind {
                Kind::Node => {
                    self.nodes_by_universe
                        .entry(universe_for_site(&site_for_node(&next)))
                        .or_default()
                        .insert(name.into());
                }
                Kind::Pod => {
                    self.pods_by_node
                        .entry(
                            next.data
                                .pointer("/spec/nodeName")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .into(),
                        )
                        .or_default()
                        .insert(name.into());
                }
                _ => {}
            }
            self.objects
                .get_mut(&kind)
                .unwrap()
                .insert(name.into(), next);
        }
        Ok(())
    }

    pub fn universes(&self) -> BTreeSet<String> {
        self.objects[&Kind::Site]
            .keys()
            .map(|s| universe_for_site(s))
            .chain(self.nodes_by_universe.keys().cloned())
            .filter(|s| !s.is_empty())
            .collect()
    }

    fn selected_universes(&self, cache: &DynamicObject) -> Result<BTreeSet<String>> {
        if cache.metadata.deletion_timestamp.is_some() {
            return Ok(BTreeSet::new());
        }
        let selector: k8s_openapi::apimachinery::pkg::apis::meta::v1::LabelSelector =
            serde_json::from_value(
                cache
                    .data
                    .pointer("/spec/siteSelector")
                    .cloned()
                    .unwrap_or_else(|| json!({})),
            )?;
        let selector = kube::core::Selector::try_from(selector)?;
        Ok(self.objects[&Kind::Site]
            .values()
            .filter(|s| site_enabled(s) && selector.matches(s.labels()))
            .map(|s| universe_for_site(&s.name_any()))
            .collect())
    }

    pub fn inventory(&self, universe: &str, socket_root: &str) -> Result<Inventory> {
        let enabled = self.objects[&Kind::Site]
            .values()
            .any(|s| universe_for_site(&s.name_any()) == universe && site_enabled(s));
        let mut nodes = Vec::new();
        for name in self.nodes_by_universe.get(universe).into_iter().flatten() {
            let n = &self.objects[&Kind::Node][name];
            let eligible = enabled
                && n.labels()
                    .get("kubernetes.io/os")
                    .is_some_and(|v| v == "linux")
                && n.labels()
                    .get(&format!("{PREFIX}exclude"))
                    .is_none_or(|v| v != "true")
                && n.metadata.deletion_timestamp.is_none();
            let mut pods = Vec::new();
            for key in self.pods_by_node.get(name).into_iter().flatten() {
                let p = &self.objects[&Kind::Pod][key];
                let managed = p.metadata.owner_references.as_ref().is_some_and(|owners| {
                    owners.iter().any(|o| {
                        o.api_version == "apps/v1"
                            && o.kind == "DaemonSet"
                            && o.controller == Some(true)
                    })
                });
                let available = p.metadata.deletion_timestamp.is_none()
                    && p.data.pointer("/status/phase").and_then(Value::as_str) == Some("Running")
                    && managed
                    && p.labels()
                        .get(&format!("{PREFIX}dataplane"))
                        .is_some_and(|v| v == "true")
                    && p.labels()
                        .get(&format!("{PREFIX}universe"))
                        .is_some_and(|v| v == universe)
                    && p.data
                        .pointer("/spec/serviceAccountName")
                        .and_then(Value::as_str)
                        == Some("racer-dataplane");
                pods.push(Pod {
                    uid: p.uid().unwrap_or_default(),
                    namespace: p.namespace().unwrap_or_default(),
                    name: p.name_any(),
                    ip: p
                        .data
                        .pointer("/status/podIP")
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .into(),
                    available,
                    ready: condition(p, "Ready"),
                    created_at: p
                        .metadata
                        .creation_timestamp
                        .as_ref()
                        .and_then(|t| t.0.timestamp_nanos_opt())
                        .unwrap_or(0),
                });
            }
            nodes.push(Node {
                name: name.clone(),
                uid: n.uid().unwrap_or_default(),
                universe: universe.into(),
                eligible,
                ready: condition(n, "Ready"),
                fabric: n
                    .annotations()
                    .get(&format!("{PREFIX}fabric"))
                    .cloned()
                    .unwrap_or_default(),
                pods,
            });
        }
        let mut caches = Vec::new();
        if enabled {
            for c in self.objects[&Kind::Cache].values() {
                if self.selected_universes(c)?.contains(universe) {
                    caches.push(Cache {
                        name: c.name_any(),
                        uid: c.uid().unwrap_or_default(),
                        resource_generation: c.metadata.generation.unwrap_or(0),
                        cache_generation: c
                            .data
                            .pointer("/spec/cacheGeneration")
                            .and_then(Value::as_i64)
                            .unwrap_or(1),
                        max_candidate_attempts: c
                            .data
                            .pointer("/spec/maxCandidateAttempts")
                            .and_then(Value::as_u64)
                            .unwrap_or(3)
                            .try_into()?,
                    });
                }
            }
        }
        Ok(Inventory {
            universe: universe.into(),
            nodes,
            caches,
            socket_root: socket_root.into(),
        })
    }
}
