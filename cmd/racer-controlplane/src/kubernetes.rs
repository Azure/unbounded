// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, BTreeSet};
use std::ops::Bound::{Excluded, Unbounded};
use std::sync::{
    Arc,
    atomic::{AtomicBool, Ordering},
};
use std::time::{Duration, Instant};

use anyhow::{Context, Result, ensure};
use futures::{StreamExt, TryStreamExt};
use kube::core::SelectorExt;
use kube::{
    Api, Client, ResourceExt,
    api::{ApiResource, DynamicObject, Patch, PatchParams},
    core::GroupVersionKind,
    runtime::watcher,
};
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    model::{self, Cache, Inventory, Node, Pod, identity, universe_for_site},
    publication::Versioned,
    security::CaState,
    status,
    storage::StoragePolicy,
    subscription::{SecurityGetter, Subscriptions},
    topology::{PlacementCache, compile_cached},
};

const PREFIX: &str = "racer.unbounded-cloud.io/";

#[path = "revision.rs"]
mod revision;
pub use revision::{CHECKPOINT, RANGE_SIZE, RevisionRange, RevisionStore};

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
    /// Maximum age of a completed authority/reconciliation round. Must exceed
    /// the retry interval. Independent request checks enforce this during stalls.
    pub authority_timeout: Duration,
}

impl RuntimeOptions {
    pub fn new(namespace: impl Into<String>) -> Self {
        Self {
            namespace: namespace.into(),
            socket_root: model::SOCKET_ROOT.into(),
            snapshot_cache_bytes: 64 * 1024 * 1024,
            long_poll: Duration::from_secs(28),
            retry_interval: Duration::from_secs(5),
            authority_timeout: Duration::from_secs(15),
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
        let subscriptions =
            Subscriptions::new(security, options.snapshot_cache_bytes, options.long_poll);
        subscriptions.authority_until(Instant::now());
        Self {
            subscriptions,
            options,
            ready: Arc::new(AtomicBool::new(false)),
        }
    }
    pub fn router(&self) -> axum::Router {
        self.subscriptions.router()
    }
    pub fn ready(&self) -> bool {
        self.ready.load(Ordering::Acquire)
            && self.subscriptions.authority_current()
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
        ensure!(
            self.options.authority_timeout > self.options.retry_interval,
            "authority timeout must exceed retry interval"
        );
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
        let mut revisions = None;
        let mut dirty = BTreeSet::new();
        let mut storage_dirty = BTreeSet::new();
        let mut storage_cursor = String::new();
        let mut continue_storage = false;
        let mut placements = BTreeMap::<String, PlacementCache>::new();
        let mut tick = tokio::time::interval(self.options.retry_interval);
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let store = RevisionStore::new(
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
                },
                _ = tick.tick() => {
                    // Storage/status retry uses indexed current Nodes, not request scans.
                    storage_dirty.extend(index.objects[&Kind::Node].keys().cloned());
                },
                // Finish pending batches without waiting for another watch event
                // or retry tick. Failed rounds still wait for those retry signals.
                _ = std::future::ready(()), if continue_storage => {}
            }
            continue_storage = false;
            // Drain a bounded batch on continuation/timer rounds too, so watch updates
            // refresh CAS versions before the next bounded storage batch.
            for _ in 0..1024 {
                match receive.try_recv() {
                    Ok((kind, event)) => {
                        index.event(kind, event, &mut dirty, &mut storage_dirty)?
                    }
                    Err(_) => break,
                }
            }
            let context = (self.subscriptions.security)();
            let Some(context) = context else {
                if !fence.is_empty() {
                    fence.clear();
                    revisions = None;
                    self.subscriptions.set_fence(None);
                    self.ready.store(false, Ordering::Release);
                }
                continue;
            };
            if index.initialized.len() != 4 {
                continue;
            }
            // Authorization follows current Pod/Node bindings independently of
            // last-good topology retained for a rejected cache configuration.
            let live = index.live_selections();
            if fence != context.fence {
                self.ready.store(false, Ordering::Release);
                self.subscriptions.set_fence(None);
                match store.reserve(&context.fence).await {
                    Ok(range) => revisions = Some(range),
                    Err(error) => {
                        tracing::error!(%error, "revision reservation failed");
                        continue;
                    }
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
                // Each acquisition rebuilds from complete live inventory. No
                // prior leader's payload can become authoritative after restart.
                self.subscriptions.clear();
            }
            if revisions.as_ref().is_some_and(RevisionRange::exhausted) {
                self.subscriptions.set_fence(None);
                self.ready.store(false, Ordering::Release);
                match store.reserve(&fence).await {
                    Ok(range) => revisions = Some(range),
                    Err(error) => {
                        tracing::error!(%error, "revision range renewal failed");
                        continue;
                    }
                }
            }
            // Check authoritative lease/CA state and checkpoint existence before
            // each publication batch. Suspend serving on uncertain authority.
            let authority_deadline = Instant::now() + self.options.authority_timeout;
            let storage_deadline = Instant::now() + self.options.authority_timeout / 4;
            if let Err(error) = store.verify(&fence).await {
                tracing::warn!(%error, "runtime authority unavailable");
                self.subscriptions.set_fence(None);
                self.ready.store(false, Ordering::Release);
                continue;
            }
            let revisions = revisions.as_mut().expect("reserved leadership range");
            let live_nodes: BTreeSet<_> = index.objects[&Kind::Node]
                .values()
                .filter_map(ResourceExt::uid)
                .map(|uid| identity("node", &uid))
                .collect();
            self.subscriptions
                .policies
                .write()
                .unwrap()
                .retain(|id, _| live_nodes.contains(id));
            let work = std::mem::take(&mut dirty);
            for universe in work {
                if let Err(error) = self
                    .reconcile(
                        &store,
                        revisions,
                        &index,
                        &universe,
                        &fence,
                        placements.entry(universe.clone()).or_default(),
                    )
                    .await
                {
                    tracing::warn!(%universe, %error, "topology reconcile retained last good state");
                    dirty.insert(universe);
                }
            }
            let current_universes = index.universes();
            placements.retain(|universe, _| current_universes.contains(universe));
            self.subscriptions.set_live_selections(live);
            // Invalid intent keeps last-good publications usable. A universe
            // without a committed publication still fails its own selection.
            // A full-cluster serial pass can outlive authority and starve watch
            // consumption. Bound each round by count and elapsed time, and rotate
            // past the last attempted node so retries/churn cannot starve the tail.
            let nodes: Vec<_> = storage_dirty
                .range((Excluded(storage_cursor.clone()), Unbounded))
                .chain(storage_dirty.range(..=storage_cursor.clone()))
                .take(32)
                .cloned()
                .collect();
            for node in nodes {
                if Instant::now() >= storage_deadline {
                    break;
                }
                storage_dirty.remove(&node);
                storage_cursor = node.clone();
                if let Err(error) = self
                    .reconcile_storage(&store, revisions, &index, &node, &fence)
                    .await
                {
                    tracing::warn!(%node, %error, "storage reconcile retry");
                    // The periodic sweep retries failures with refreshed watch
                    // state, rather than immediately replaying the same stale CAS.
                }
            }
            if let Err(error) = self.publish_cache_status(&client, &index, &fence).await {
                tracing::warn!(%error, "cache status retry");
            }
            if store.verify(&fence).await.is_ok() {
                continue_storage = !storage_dirty.is_empty();
                // Use the round's start, not the I/O completion time: a delayed
                // response must not extend authority based on an old read.
                self.subscriptions.authority_until(authority_deadline);
                self.subscriptions.set_fence(Some(fence.clone()));
                self.ready.store(
                    !self.subscriptions.universes.read().unwrap().is_empty()
                        || index.universes().is_empty(),
                    Ordering::Release,
                );
            } else {
                self.subscriptions.set_fence(None);
                self.ready.store(false, Ordering::Release);
            }
        }
        self.ready.store(false, Ordering::Release);
        self.subscriptions.set_fence(None);
        shutdown.cancel();
        tasks.abort_all();
        while tasks.join_next().await.is_some() {}
        Ok(())
    }

    async fn reconcile(
        &self,
        store: &RevisionStore,
        revisions: &mut RevisionRange,
        index: &InventoryIndex,
        universe: &str,
        fence: &str,
        placement: &mut PlacementCache,
    ) -> Result<()> {
        let previous = self
            .subscriptions
            .universes
            .read()
            .unwrap()
            .get(&identity("universe", universe))
            .map(|p| p.topology.generation().clone());
        let input = index.inventory(universe, &self.options.socket_root)?;
        // Move the disposable per-universe cache into blocking compilation and
        // retain it even when validation rejects the desired configuration.
        let mut cached = std::mem::take(placement);
        let (result, cached) = tokio::task::spawn_blocking(move || {
            let result = compile_cached(&input, previous.as_deref(), &mut cached)
                .map(|desired| (desired, previous));
            (result, cached)
        })
        .await?;
        *placement = cached;
        let (mut desired, previous) = result?;
        if previous.as_deref() != Some(&desired) {
            desired.revision = next_revision(store, revisions, fence).await?;
            desired.validate()?;
            store.verify(fence).await?;
            self.subscriptions.install(Arc::new(desired))?;
        }
        Ok(())
    }

    async fn reconcile_storage(
        &self,
        store: &RevisionStore,
        revisions: &mut RevisionRange,
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
        let base = cached
            .as_deref()
            .cloned()
            .unwrap_or_else(|| StoragePolicy::for_node(&uid));
        let mut next = base.resolve(&universe, override_value, site_value)?;
        let changed = cached.as_deref() != Some(&next);
        if changed {
            next.revision = next_revision(store, revisions, fence).await?;
            if next.validation_error.is_none()
                && (base.desired_bytes != next.desired_bytes
                    || base.validation_error.is_some()
                    || base.version == 0)
            {
                next.version = next.revision;
            }
            next.validate()?;
            store.verify(fence).await?;
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

async fn next_revision(
    store: &RevisionStore,
    range: &mut RevisionRange,
    fence: &str,
) -> Result<u64> {
    if range.exhausted() {
        *range = store.reserve(fence).await?;
    }
    range.take()
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
        self.inventory_inner(universe, socket_root, true)
    }

    /// Membership authorization depends only on live identity, eligibility, and
    /// deterministic Pod selection. Invalid fabric/cache/placement intent cannot
    /// revoke unrelated healthy members of the retained last-good topology.
    pub fn live_selections(&self) -> BTreeMap<(String, String), Selection> {
        let mut live = BTreeMap::new();
        for universe in self.universes() {
            let Ok(input) = self.inventory_inner(&universe, "", false) else {
                continue;
            };
            for node in input.nodes {
                if !node.eligible || !node.ready || node.uid.is_empty() || node.name.is_empty() {
                    continue;
                }
                let pod = node
                    .pods
                    .iter()
                    .filter(|p| {
                        p.available && !p.uid.is_empty() && model::available_ip(&p.ip).is_some()
                    })
                    .min_by_key(|p| (!p.ready, p.created_at, &p.name, &p.uid));
                if let Some(pod) = pod {
                    live.insert(
                        (identity("universe", &universe), identity("node", &node.uid)),
                        Selection {
                            node_name: node.name,
                            pod_namespace: pod.namespace.clone(),
                            pod_name: pod.name.clone(),
                            pod_uid: pod.uid.clone(),
                            universe: universe.clone(),
                        },
                    );
                }
            }
        }
        live
    }

    fn inventory_inner(
        &self,
        universe: &str,
        socket_root: &str,
        include_caches: bool,
    ) -> Result<Inventory> {
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
                            && o.name == "racer-dataplane"
                            && !o.uid.is_empty()
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
                        .get(&format!("{PREFIX}component"))
                        .is_some_and(|v| v == "racer-dataplane")
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
        if enabled && include_caches {
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
