// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use axum::{
    Extension, Router,
    body::Body,
    extract::State,
    http::{HeaderMap, Response, StatusCode},
    routing::get,
};
use prost::Message;
use sha2::{Digest, Sha256};
use tokio::sync::{Semaphore, watch};

use crate::kubernetes::{SecurityContext, Selection};
use crate::model::{Generation, identity, identity_bytes};
use crate::proto;
use crate::security::{VerifiedPeer, unix_now};
use crate::status::{Observation, Reports, header};
use crate::storage::StoragePolicy;
use crate::topology::Topology;

pub type SecurityGetter = Arc<dyn Fn() -> Option<SecurityContext> + Send + Sync>;

pub(crate) struct Published {
    pub topology: Arc<Topology>,
    pub selections: BTreeMap<String, Selection>,
    pub changes: watch::Sender<u64>,
}

#[derive(Clone)]
struct Snapshot {
    value: proto::Snapshot,
    digest: String,
    bytes: usize,
}

type CacheKey = (String, String, u64, bool);

struct Cache {
    values: BTreeMap<CacheKey, Arc<Snapshot>>,
    order: VecDeque<CacheKey>,
    bytes: usize,
    budget: usize,
    // Digest metadata survives payload eviction but never a generation change.
    digests: BTreeMap<(String, String), (u64, String)>,
}

/// Shared indexed committed state. No persistent subscriber or idle-process rows.
pub struct Subscriptions {
    pub(crate) universes: RwLock<BTreeMap<String, Arc<Published>>>,
    pub(crate) policies: RwLock<BTreeMap<String, Arc<StoragePolicy>>>,
    live_selections: RwLock<Option<BTreeMap<(String, String), Selection>>>,
    pub(crate) reports: Mutex<Reports>,
    cache: Mutex<Cache>,
    builders: Arc<Semaphore>,
    requests: Semaphore,
    responses: Arc<Semaphore>,
    pub(crate) security: SecurityGetter,
    pub(crate) fence: RwLock<Option<String>>,
    // Runtime enables a deadline before starting. It expires synchronously on
    // request/readiness checks even if the reconcile future or API I/O stalls.
    authority_deadline: RwLock<Option<Instant>>,
    wait: Duration,
}

impl Subscriptions {
    pub fn new(security: SecurityGetter, cache_bytes: usize, wait: Duration) -> Arc<Self> {
        Arc::new(Self {
            universes: RwLock::new(BTreeMap::new()),
            policies: RwLock::new(BTreeMap::new()),
            live_selections: RwLock::new(None),
            reports: Mutex::new(BTreeMap::new()),
            cache: Mutex::new(Cache {
                values: BTreeMap::new(),
                order: VecDeque::new(),
                bytes: 0,
                budget: cache_bytes,
                digests: BTreeMap::new(),
            }),
            builders: Arc::new(Semaphore::new(4)),
            requests: Semaphore::new(16_384),
            responses: Arc::new(Semaphore::new(16)),
            security,
            fence: RwLock::new(None),
            authority_deadline: RwLock::new(None),
            wait,
        })
    }

    pub fn router(self: &Arc<Self>) -> Router {
        Router::new()
            .route("/v1/config", get(config))
            .with_state(self.clone())
    }

    pub fn selection(&self, universe: &str, node: &str) -> Option<Selection> {
        let selected = self
            .universes
            .read()
            .unwrap()
            .get(universe)?
            .selections
            .get(node)
            .cloned()?;
        self.live_selections
            .read()
            .unwrap()
            .as_ref()
            .is_none_or(|live| live.get(&(universe.into(), node.into())) == Some(&selected))
            .then_some(selected)
    }

    pub(crate) fn set_live_selections(&self, next: BTreeMap<(String, String), Selection>) {
        let mut live = self.live_selections.write().unwrap();
        if live.as_ref() == Some(&next) {
            return;
        }
        let old = live.replace(next);
        let mut cache = self.cache.lock().unwrap();
        cache.digests.retain(|key, _| {
            old.as_ref().and_then(|prior| prior.get(key))
                == live.as_ref().and_then(|next| next.get(key))
        });
        drop(cache);
        drop(live);
        for publication in self.universes.read().unwrap().values() {
            publication.changes.send_modify(|n| *n = n.wrapping_add(1));
        }
    }

    pub(crate) fn authority_until(&self, deadline: Instant) {
        *self.authority_deadline.write().unwrap() = Some(deadline);
    }

    pub(crate) fn authority_current(&self) -> bool {
        self.authority_deadline
            .read()
            .unwrap()
            .is_none_or(|deadline| Instant::now() < deadline)
    }

    pub(crate) fn set_fence(&self, fence: Option<String>) {
        let mut current = self.fence.write().unwrap();
        if *current == fence {
            return;
        }
        *current = fence;
        for publication in self.universes.read().unwrap().values() {
            publication.changes.send_modify(|n| *n = n.wrapping_add(1));
        }
    }

    pub(crate) fn clear(&self) {
        self.universes.write().unwrap().clear();
        self.policies.write().unwrap().clear();
        self.reports.lock().unwrap().clear();
        let mut cache = self.cache.lock().unwrap();
        cache.values.clear();
        cache.order.clear();
        cache.digests.clear();
        cache.bytes = 0;
    }

    pub(crate) fn install(&self, generation: Arc<Generation>) -> anyhow::Result<()> {
        let universe = identity("universe", &generation.universe);
        let mut selections = BTreeMap::new();
        for (name, member) in &generation.nodes {
            if member.ip.is_some() {
                selections.insert(
                    member.id.clone(),
                    Selection {
                        node_name: name.clone(),
                        pod_namespace: member.pod_namespace.clone(),
                        pod_name: member.pod_name.clone(),
                        pod_uid: member.pod_uid.clone(),
                        universe: generation.universe.clone(),
                    },
                );
            }
        }
        let topology = Arc::new(Topology::from_generation(generation)?);
        let mut universes = self.universes.write().unwrap();
        let changes = universes
            .get(&universe)
            .map(|p| p.changes.clone())
            .unwrap_or_else(|| watch::channel(0).0);
        {
            let mut cache = self.cache.lock().unwrap();
            cache.values.retain(|k, _| k.0 != universe);
            cache.order.retain(|k| k.0 != universe);
            cache.bytes = cache.values.values().map(|s| s.bytes).sum();
            cache.digests.retain(|k, _| k.0 != universe);
        }
        self.reports.lock().unwrap().retain(|(u, n), r| {
            u != &universe || selections.get(n).is_some_and(|s| s.pod_uid == r.pod_uid)
        });
        universes.insert(
            universe,
            Arc::new(Published {
                topology,
                selections,
                changes: changes.clone(),
            }),
        );
        changes.send_modify(|n| *n = n.wrapping_add(1));
        Ok(())
    }

    pub(crate) fn install_policy(&self, policy: Arc<StoragePolicy>) {
        let universe = identity("universe", &policy.universe);
        let old_universe = self
            .policies
            .write()
            .unwrap()
            .insert(policy.node.clone(), policy)
            .map(|p| identity("universe", &p.universe));
        let universes = self.universes.read().unwrap();
        for key in [Some(universe), old_universe].into_iter().flatten() {
            if let Some(p) = universes.get(&key) {
                p.changes.send_modify(|n| *n = n.wrapping_add(1));
            }
        }
    }

    pub(crate) fn expected_digest(&self, universe: &str, node: &str) -> Option<(u64, String)> {
        self.cache
            .lock()
            .unwrap()
            .digests
            .get(&(universe.into(), node.into()))
            .cloned()
    }

    pub fn cache_usage(&self) -> (usize, usize) {
        let cache = self.cache.lock().unwrap();
        (cache.bytes, cache.digests.len())
    }

    pub fn waiter_count(&self) -> usize {
        self.universes
            .read()
            .unwrap()
            .values()
            .map(|p| p.changes.receiver_count())
            .sum()
    }

    pub fn response_count(&self) -> usize {
        16 - self.responses.available_permits()
    }

    fn authorize(
        &self,
        peer: &VerifiedPeer,
        boot: &str,
    ) -> Result<(String, String, String), StatusCode> {
        let context = (self.security)().ok_or(StatusCode::SERVICE_UNAVAILABLE)?;
        if self.fence.read().unwrap().as_deref() != Some(context.fence.as_str())
            || context.state.fence() != context.fence
            || !self.authority_current()
        {
            return Err(StatusCode::SERVICE_UNAVAILABLE);
        }
        let claims = context
            .state
            .verify_peer(peer, unix_now())
            .map_err(|_| StatusCode::FORBIDDEN)?;
        if claims.kind != crate::security::IdentityKind::Node || claims.boot_id != boot {
            return Err(StatusCode::FORBIDDEN);
        }
        let (universe, node) = (claims.universe.as_str(), claims.node.as_str());
        let pod = peer
            .node_pod(universe, node, unix_now())
            .map_err(|_| StatusCode::FORBIDDEN)?;
        if pod != claims.pod_uid {
            return Err(StatusCode::FORBIDDEN);
        }
        // A rejected cache configuration can retain old topology, but cannot
        // authorize a Pod whose live Node binding or selection has disappeared.
        let previously_selected =
            self.universes
                .read()
                .unwrap()
                .get(universe)
                .is_some_and(|published| {
                    published
                        .selections
                        .get(node)
                        .is_some_and(|s| s.pod_uid == pod)
                });
        if previously_selected && self.selection(universe, node).is_none() {
            return Err(StatusCode::FORBIDDEN);
        }
        if self
            .selection(universe, node)
            .is_some_and(|selected| selected.pod_uid == pod && selected.pod_name != claims.pod_name)
        {
            return Err(StatusCode::FORBIDDEN);
        }
        Ok((universe.into(), node.into(), pod.into()))
    }

    async fn snapshot(
        self: &Arc<Self>,
        universe: &str,
        node: &str,
        pod: &str,
    ) -> Result<(Arc<Snapshot>, Arc<Published>, bool), StatusCode> {
        // Queue only small request identities, never a stale topology Arc.
        let permit = self
            .builders
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| StatusCode::SERVICE_UNAVAILABLE)?;
        let publication = self
            .universes
            .read()
            .unwrap()
            .get(universe)
            .cloned()
            .ok_or(StatusCode::SERVICE_UNAVAILABLE)?;
        let selected = self
            .selection(universe, node)
            .is_some_and(|s| s.pod_uid == pod);
        let key = (
            universe.to_owned(),
            node.to_owned(),
            publication.topology.generation().revision,
            selected,
        );
        {
            // Match install's lock order and never resurrect a replaced
            // generation's digest. Live reselection may keep the payload cached
            // while invalidating its convergence metadata.
            let universes = self.universes.read().unwrap();
            let mut cache = self.cache.lock().unwrap();
            if let Some(cached) = cache.values.get(&key).cloned() {
                if selected
                    && universes
                        .get(universe)
                        .is_some_and(|p| Arc::ptr_eq(p, &publication))
                    && (cache.digests.len() < 100_000
                        || cache.digests.contains_key(&(universe.into(), node.into())))
                {
                    cache.digests.insert(
                        (universe.into(), node.into()),
                        (key.2, cached.digest.clone()),
                    );
                }
                return Ok((cached, publication, selected));
            }
        }
        let topology = publication.topology.clone();
        let node = node.to_string();
        let node_copy = node.clone();
        let snapshot = tokio::task::spawn_blocking(move || {
            let _permit = permit;
            let g = topology.generation();
            let value = if selected {
                topology
                    .snapshot(&node_copy)
                    .expect("selected member is indexed")
            } else {
                proto::Snapshot {
                    universe: identity_bytes("universe", &g.universe).to_vec(),
                    node: hex::decode(&node_copy).unwrap_or_default(),
                    revision: g.revision,
                    epoch: g.revision,
                    ..Default::default()
                }
            };
            let bytes = value.encode_to_vec();
            Snapshot {
                digest: hex::encode(Sha256::digest(&bytes)),
                // Include allocation overhead and duplicate endpoint strings, not
                // just protobuf wire length. Conservatively bound cached heap.
                bytes: bytes.len() * 4 + 1024,
                value,
            }
        })
        .await
        .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;
        let snapshot = Arc::new(snapshot);
        // Lock order matches install. Do not resurrect metadata for an old generation.
        let universes = self.universes.read().unwrap();
        if universes
            .get(universe)
            .is_some_and(|p| Arc::ptr_eq(p, &publication))
        {
            let mut cache = self.cache.lock().unwrap();
            if selected
                && (cache.digests.len() < 100_000
                    || cache.digests.contains_key(&(universe.into(), node.clone())))
            {
                cache
                    .digests
                    .insert((universe.into(), node), (key.2, snapshot.digest.clone()));
            }
            if snapshot.bytes <= cache.budget && !cache.values.contains_key(&key) {
                while cache.bytes + snapshot.bytes > cache.budget {
                    if let Some(old) = cache.order.pop_front() {
                        if let Some(value) = cache.values.remove(&old) {
                            cache.bytes -= value.bytes;
                        }
                    } else {
                        break;
                    }
                }
                cache.bytes += snapshot.bytes;
                cache.order.push_back(key.clone());
                cache.values.insert(key, snapshot.clone());
            }
        }
        Ok((snapshot, publication, selected))
    }
}

fn response(status: StatusCode, body: Vec<u8>) -> Response<Body> {
    Response::builder()
        .status(status)
        .header("content-length", body.len())
        .header("content-type", "application/x-protobuf")
        .body(Body::from(body))
        .unwrap()
}

async fn config(
    State(state): State<Arc<Subscriptions>>,
    peer: Option<Extension<VerifiedPeer>>,
    headers: HeaderMap,
) -> Response<Body> {
    match config_inner(state, peer.map(|p| p.0), headers).await {
        Ok(response) => response,
        Err(status) => response(status, Vec::new()),
    }
}

async fn config_inner(
    state: Arc<Subscriptions>,
    peer: Option<VerifiedPeer>,
    headers: HeaderMap,
) -> Result<Response<Body>, StatusCode> {
    let _request = state
        .requests
        .try_acquire()
        .map_err(|_| StatusCode::SERVICE_UNAVAILABLE)?;
    let peer = peer.ok_or(StatusCode::UNAUTHORIZED)?;
    let boot = header(&headers, "x-racer-boot");
    let cursor = header(&headers, "x-racer-cursor");
    if boot.len() != 64
        || !boot
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
        || header(&headers, "x-racer-profile") != "1"
        || cursor.len() > 1024
        || !cursor.bytes().all(|b| (0x21..=0x7e).contains(&b))
        || headers.get("content-length").is_some_and(|v| v != "0")
        || headers.contains_key("transfer-encoding")
    {
        return Err(StatusCode::BAD_REQUEST);
    }
    let (universe, node, pod) = state.authorize(&peer, boot)?;
    let deadline = tokio::time::Instant::now() + state.wait;
    let mut observed = false;
    let mut response_permit = None;
    loop {
        // Subscribe before reading either topology or policy to close the lost-wakeup race.
        let mut changes = state
            .universes
            .read()
            .unwrap()
            .get(&universe)
            .ok_or(StatusCode::SERVICE_UNAVAILABLE)?
            .changes
            .subscribe();
        let revision = {
            let universes = state.universes.read().unwrap();
            let publication = universes
                .get(&universe)
                .ok_or(StatusCode::SERVICE_UNAVAILABLE)?;
            publication.topology.generation().revision
        };
        let selected = state
            .selection(&universe, &node)
            .is_some_and(|s| s.pod_uid == pod);
        let policy = state
            .policies
            .read()
            .unwrap()
            .get(&node)
            .filter(|p| selected && identity("universe", &p.universe) == universe)
            .cloned();
        if selected {
            let mut reports = state.reports.lock().unwrap();
            let key = (universe.clone(), node.clone());
            if !observed {
                let observation =
                    Observation::observe(reports.get(&key), &pod, boot, &headers, Instant::now());
                reports.insert(key.clone(), observation);
                observed = true;
            }
            if let Some(report) = reports.get_mut(&key) {
                report.offer(policy.as_deref());
            }
        }
        let storage = policy
            .filter(|_| header(&headers, "x-racer-storage-policy") == "1")
            .and_then(|p| p.wire());
        let cursor_for = |digest: &str| {
            hex::encode(Sha256::digest(
                serde_json::to_vec(&(&universe, &node, &pod, boot, digest, &storage)).unwrap(),
            ))
        };
        // Digests survive payload eviction. Unchanged reconnects do not rebuild
        // or copy the per-recipient snapshot and its universe-wide member catalog.
        let unchanged = selected
            && state
                .expected_digest(&universe, &node)
                .is_some_and(|(r, digest)| r == revision && cursor_for(&digest) == cursor);
        state.authorize(&peer, boot)?;
        if !unchanged {
            // Wait with small identities only. The permit remains in the Body
            // until it is consumed or dropped, bounding slow-reader responses.
            if response_permit.is_none() {
                response_permit = Some(
                    state
                        .responses
                        .clone()
                        .acquire_owned()
                        .await
                        .map_err(|_| StatusCode::SERVICE_UNAVAILABLE)?,
                );
                // Admission can span many publications. Refresh revision,
                // selection, policy, and authorization before building anything.
                // Retain admission across publication races so current work does
                // not repeatedly return to the tail of a fleet-sized queue.
                continue;
            }
            let (snapshot, publication, snapshot_selected) =
                state.snapshot(&universe, &node, &pod).await?;
            if snapshot.value.revision != revision
                || snapshot_selected != selected
                || state
                    .selection(&universe, &node)
                    .is_some_and(|s| s.pod_uid == pod)
                    != selected
                || !state
                    .universes
                    .read()
                    .unwrap()
                    .get(&universe)
                    .is_some_and(|p| Arc::ptr_eq(p, &publication))
            {
                continue;
            }
            drop(publication);
            let next_cursor = cursor_for(&snapshot.digest);
            state.authorize(&peer, boot)?;
            if next_cursor == cursor {
                drop(snapshot);
            } else {
                let desired = proto::DesiredState {
                    universe: snapshot.value.universe.clone(),
                    node: snapshot.value.node.clone(),
                    incarnation: hex::decode(boot).map_err(|_| StatusCode::BAD_REQUEST)?,
                    snapshot_digest: hex::decode(&snapshot.digest).unwrap(),
                    revision: snapshot.value.revision,
                    configuration: Some(proto::Configuration {
                        contents: Some(proto::configuration::Contents::Snapshot(
                            snapshot.value.clone(),
                        )),
                    }),
                    profile: 1,
                    pod_uid: pod.clone(),
                    storage_policy: storage,
                    cursor: next_cursor,
                };
                let bytes = desired.encode_to_vec();
                let length = bytes.len();
                let permit = response_permit.take().expect("response admitted");
                let body =
                    futures::stream::unfold((Some(bytes), permit), |(bytes, permit)| async move {
                        bytes
                            .map(|bytes| (Ok::<_, std::convert::Infallible>(bytes), (None, permit)))
                    });
                return Ok(Response::builder()
                    .status(StatusCode::OK)
                    .header("content-length", length)
                    .header("content-type", "application/x-protobuf")
                    .body(Body::from_stream(body))
                    .unwrap());
            }
        }
        // An unchanged long poll must not occupy a response slot.
        drop(response_permit.take());
        // A waiter retains no snapshot payload or publication. Payload eviction
        // therefore bounds aggregate snapshot memory even with 10,000 subscribers.
        if tokio::time::Instant::now() >= deadline {
            return Ok(response(StatusCode::NO_CONTENT, Vec::new()));
        }
        // Periodic fence checks release requests promptly even if service has only
        // cleared its getter. Client disconnect drops this future and watch receiver.
        tokio::select! {
            _ = changes.changed() => {},
            _ = tokio::time::sleep_until(deadline) => {},
            _ = tokio::time::sleep(Duration::from_millis(250)) => {
                state.authorize(&peer, boot)?;
                // Avoid rebuilding a large envelope on an unchanged security tick.
                while !changes.has_changed().unwrap_or(true) && tokio::time::Instant::now() < deadline {
                    tokio::select! { _ = changes.changed() => { break; }, _ = tokio::time::sleep(Duration::from_millis(250)) => { state.authorize(&peer, boot)?; } }
                }
            }
        }
    }
}

#[cfg(test)]
#[path = "../tests/scale/distinct.rs"]
mod distinct_scale;

#[cfg(test)]
#[path = "../tests/runtime/subscription.rs"]
mod runtime_tests;
