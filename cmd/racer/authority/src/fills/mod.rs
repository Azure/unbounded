//! ClusterCacheFill control-plane state, listing, and status.
//!
//! Every Racer process watches ClusterCacheFills and ClusterCaches and lists
//! its fills independently. The primary additionally writes fill status. Fill
//! state is process-local: it is never in the publication, the committed
//! record, or a member record.

pub mod azure;
pub mod identity;
pub mod manifest;
pub mod status;

pub use manifest::{FillObject, MAX_FILL_OBJECTS, Manifest, ManifestError};

use crate::{
    kube::{self, KubeError, paths},
    rt,
    rules::catalog::{self, CacheInfo, FillCache},
    shared::Shared,
};
use serde_json::Value;
use std::{
    collections::BTreeMap,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

/// Longest wait before retrying a failed list or missing CRD.
const RETRY_MAX: Duration = Duration::from_secs(30);

/// How often the process-wide fill loops look for work.
const TICK: Duration = Duration::from_secs(5);

/// Backoff after a failed origin listing.
const LIST_BACKOFF: Duration = Duration::from_secs(10);

/// Default replicas and resync interval, matching the CRD defaults.
const DEFAULT_REPLICAS: u8 = 2;
const DEFAULT_RESYNC_SECS: u64 = 3600;
const MIN_RESYNC_SECS: u64 = 60;
const MAX_RESYNC_SECS: u64 = 24 * 60 * 60;

/// One fill as every process sees it. Listing fields are process-local; the
/// spec fields come from the API object.
#[derive(Clone, Debug)]
pub struct FillView {
    /// ClusterCacheFill UID, canonical UUID.
    pub uid: String,
    pub name: String,
    pub generation: u64,
    /// ClusterCache UID, empty until the cache is published or listed.
    pub cache_uid: String,
    pub cache_name: String,
    pub replicas: u8,
    /// Nodes in the cache's pool at the publication used for the plan. Zero
    /// until a publication lists the cache.
    pub available_nodes: u8,
    pub max_footprint: u64,
    pub resync_secs: u64,
    /// Current manifest, from this process's own listing.
    pub manifest: Option<Arc<Manifest>>,
    /// Publication membership version used for the footprint plan.
    pub membership: Option<u64>,
    /// Planned disk reservation for the current manifest.
    pub planned_footprint_bytes: Option<u64>,
    /// Admission result. False fills are never pinned.
    pub accepted: bool,
    /// Admission reason when `accepted` is false, or a short message.
    pub message: String,
    /// Last listing failure reason, `ListFailed` or `Forbidden`.
    pub error: Option<String>,
    /// Prefixes from the spec.
    pub(crate) prefixes: Vec<String>,
    /// Generation the current manifest was listed for.
    pub(crate) listed_generation: Option<u64>,
    /// Cache UID the current manifest was listed against.
    pub(crate) listed_cache_uid: String,
    /// Earliest time of the next listing attempt.
    pub(crate) next_list: Option<Instant>,
    /// Current listing failure streak.
    pub(crate) failures: u32,
}

impl FillView {
    /// Parse a ClusterCacheFill API object. `None` for objects outside the
    /// CRD bounds, which the API server should have rejected.
    pub fn from_json(v: &Value) -> Option<Self> {
        let uid = kube::str_at(v, &["metadata", "uid"]).to_owned();
        let name = kube::str_at(v, &["metadata", "name"]).to_owned();
        if !racer_wire::control::valid_uuid(&uid) || name.is_empty() {
            return None;
        }
        let generation = v
            .pointer("/metadata/generation")
            .and_then(Value::as_u64)
            .unwrap_or(0);
        let cache_name = kube::str_at(v, &["spec", "cacheName"]).to_owned();
        if cache_name.is_empty() {
            return None;
        }
        let mut prefixes = Vec::new();
        match v.pointer("/spec/prefixes") {
            Some(Value::Array(a)) if a.len() <= 16 => {
                for p in a {
                    let p = p.as_str()?;
                    if p.len() > 1024 {
                        return None;
                    }
                    prefixes.push(p.to_owned());
                }
            }
            None => prefixes.push(String::new()),
            Some(_) => return None,
        }
        if prefixes.is_empty() {
            prefixes.push(String::new());
        }
        let replicas = v
            .pointer("/spec/replicas")
            .and_then(Value::as_u64)
            .filter(|r| (1..=3).contains(r))
            .unwrap_or(u64::from(DEFAULT_REPLICAS)) as u8;
        let max_footprint = v
            .pointer("/spec/maxFootprint")
            .and_then(Value::as_i64)
            .filter(|f| *f > 0)? as u64;
        let resync_secs = v
            .pointer("/spec/resyncInterval")
            .and_then(Value::as_str)
            .and_then(catalog::go_duration)
            .map(|d| d.as_secs())
            .filter(|s| (MIN_RESYNC_SECS..=MAX_RESYNC_SECS).contains(s))
            .unwrap_or(DEFAULT_RESYNC_SECS);
        Some(Self {
            uid,
            name,
            generation,
            cache_uid: String::new(),
            cache_name,
            replicas,
            available_nodes: 0,
            max_footprint,
            resync_secs,
            manifest: None,
            membership: None,
            planned_footprint_bytes: None,
            accepted: false,
            message: "CacheNotFound".into(),
            error: None,
            prefixes,
            listed_generation: None,
            listed_cache_uid: String::new(),
            next_list: None,
            failures: 0,
        })
    }

    /// Whether this process needs to list the origin now. `next_list` is
    /// cleared when a spec or cache change invalidates the manifest and is set
    /// to the next resync or to a backoff deadline after a failure.
    fn needs_list(&self, now: Instant) -> bool {
        self.next_list.is_none_or(|at| now >= at)
    }

    /// Whether a listing is for the current spec and cache.
    fn listing_current(&self, generation: u64, cache_uid: &str) -> bool {
        self.generation == generation && self.cache_uid == cache_uid
    }
}

/// Snapshot of the current fills, sorted by name, for the dataplane.
pub fn snapshot(shared: &Shared) -> Vec<FillView> {
    let mut fills: Vec<_> = shared.fills.borrow().values().cloned().collect();
    fills.sort_by(|a, b| a.name.cmp(&b.name));
    fills
}

/// Watch ClusterCacheFills and ClusterCaches process-wide. Missing CRDs are an
/// empty, synced catalog.
pub async fn watch(shared: Rc<Shared>) {
    let fills = shared.clone();
    let caches = shared.clone();
    let listing = shared.clone();
    let _ = rt::spawn(async move { watch_fills(fills).await });
    let _ = rt::spawn(async move { watch_caches(caches).await });
    let _ = rt::spawn(async move { list_fills(listing).await });
}

/// Carry the process-local listing and admission state from an existing view
/// into a newly read spec. A changed generation or cache resets the manifest.
fn adopt(mut view: FillView, old: &FillView) -> FillView {
    view.cache_uid = old.cache_uid.clone();
    view.manifest = old.manifest.clone();
    view.membership = old.membership;
    view.available_nodes = old.available_nodes;
    view.planned_footprint_bytes = old.planned_footprint_bytes;
    view.accepted = old.accepted;
    view.message = old.message.clone();
    view.error = old.error.clone();
    view.listed_generation = old.listed_generation;
    view.listed_cache_uid = old.listed_cache_uid.clone();
    view.next_list = old.next_list;
    view.failures = old.failures;
    if old.generation != view.generation || old.cache_name != view.cache_name {
        view.manifest = None;
        view.error = None;
        view.listed_generation = None;
        view.listed_cache_uid.clear();
        view.next_list = None;
        view.failures = 0;
    }
    view
}

async fn watch_fills(shared: Rc<Shared>) {
    let path = paths::CLUSTER_CACHE_FILLS;
    let mut backoff = 1u64;
    while !shared.stopping.get() {
        let (items, mut rv) = match shared.api.list(path, "").await {
            Ok(r) => r,
            Err(KubeError::NotFound) => {
                shared.fills.borrow_mut().clear();
                shared.fills_synced.set(true);
                rt::sleep(RETRY_MAX).await;
                continue;
            }
            Err(_) => {
                rt::sleep(Duration::from_secs(backoff)).await;
                backoff = (backoff * 2).min(RETRY_MAX.as_secs());
                continue;
            }
        };
        {
            let mut fills = shared.fills.borrow_mut();
            let mut next = BTreeMap::new();
            for v in &items {
                if let Some(view) = FillView::from_json(v) {
                    next.insert(view.uid.clone(), view);
                }
            }
            for (uid, mut view) in next {
                if let Some(old) = fills.get(&uid) {
                    view = adopt(view, old);
                }
                fills.insert(uid, view);
            }
            fills.retain(|uid, _| {
                items
                    .iter()
                    .any(|v| kube::str_at(v, &["metadata", "uid"]) == uid.as_str())
            });
        }
        shared.fills_synced.set(true);
        backoff = 1;
        drop(items);
        loop {
            let mut w = match shared.api.watch(path, "", &rv).await {
                Ok(w) => w,
                Err(_) => {
                    rt::sleep(Duration::from_secs(backoff)).await;
                    backoff = (backoff * 2).min(RETRY_MAX.as_secs());
                    break;
                }
            };
            let mut relist = false;
            loop {
                if shared.stopping.get() {
                    return;
                }
                match w.next().await {
                    Ok(Some(e)) => {
                        backoff = 1;
                        rv = kube::str_at(&e.object, &["metadata", "resourceVersion"]).to_owned();
                        if e.kind != "BOOKMARK" {
                            apply_fill(&shared, &e.kind, &e.object);
                        }
                    }
                    Ok(None) => break,
                    Err(_) => {
                        relist = true;
                        break;
                    }
                }
            }
            if relist {
                break;
            }
        }
    }
}

fn apply_fill(shared: &Shared, event: &str, v: &Value) {
    let uid = kube::str_at(v, &["metadata", "uid"]).to_owned();
    if uid.is_empty() {
        return;
    }
    let mut fills = shared.fills.borrow_mut();
    if event == "DELETED" {
        fills.remove(&uid);
        return;
    }
    let Some(mut view) = FillView::from_json(v) else {
        return;
    };
    if let Some(old) = fills.get(&uid) {
        view = adopt(view, old);
    }
    fills.insert(uid, view);
}

async fn watch_caches(shared: Rc<Shared>) {
    let path = paths::CACHES;
    let mut backoff = 1u64;
    while !shared.stopping.get() {
        let (items, mut rv) = match shared.api.list(path, "").await {
            Ok(r) => r,
            Err(KubeError::NotFound) => {
                shared.fills_caches.borrow_mut().clear();
                shared.fills_caches_synced.set(true);
                rt::sleep(RETRY_MAX).await;
                continue;
            }
            Err(_) => {
                rt::sleep(Duration::from_secs(backoff)).await;
                backoff = (backoff * 2).min(RETRY_MAX.as_secs());
                continue;
            }
        };
        {
            let mut caches = shared.fills_caches.borrow_mut();
            caches.clear();
            for v in &items {
                let info = CacheInfo::from_json(v);
                caches.insert(info.uid.clone(), info);
            }
        }
        shared.fills_caches_synced.set(true);
        backoff = 1;
        drop(items);
        loop {
            let mut w = match shared.api.watch(path, "", &rv).await {
                Ok(w) => w,
                Err(_) => {
                    rt::sleep(Duration::from_secs(backoff)).await;
                    backoff = (backoff * 2).min(RETRY_MAX.as_secs());
                    break;
                }
            };
            let mut relist = false;
            loop {
                if shared.stopping.get() {
                    return;
                }
                match w.next().await {
                    Ok(Some(e)) => {
                        backoff = 1;
                        rv = kube::str_at(&e.object, &["metadata", "resourceVersion"]).to_owned();
                        if e.kind != "BOOKMARK" {
                            apply_cache(&shared, &e.kind, &e.object);
                        }
                    }
                    Ok(None) => break,
                    Err(_) => {
                        relist = true;
                        break;
                    }
                }
            }
            if relist {
                break;
            }
        }
    }
}

fn apply_cache(shared: &Shared, event: &str, v: &Value) {
    let info = CacheInfo::from_json(v);
    let mut caches = shared.fills_caches.borrow_mut();
    if event == "DELETED" {
        caches.remove(&info.uid);
    } else {
        caches.insert(info.uid.clone(), info);
    }
}

/// One process's listing loop. It never runs in a Kubernetes watch, so a slow
/// origin does not delay fill discovery or the primary Lease.
async fn list_fills(shared: Rc<Shared>) {
    let client = azure::Client::new(azure::Settings {
        trust_bundle: shared.config.trust_bundle.clone(),
        authority_host: shared.config.azure_authority_host.clone(),
        federated_token_file: shared.config.azure_federated_token_file.clone(),
    })
    .await;
    while !shared.stopping.get() {
        // Only list once the cache catalog is known: a missing cache is not
        // the same as an unsynced catalog.
        if shared.fills_caches_synced.get() {
            let candidates = {
                let now = rt::now();
                let caches = shared.fills_caches.borrow();
                shared
                    .fills
                    .borrow()
                    .values()
                    .filter_map(|view| {
                        let FillCache::Usable { uid, origin, .. } =
                            catalog::fill_cache_named(&view.cache_name, &caches)
                        else {
                            return None;
                        };
                        view.needs_list(now).then(|| {
                            (
                                view.uid.clone(),
                                view.generation,
                                uid,
                                origin.azure.container_url,
                                origin.azure.credential,
                                view.prefixes.clone(),
                            )
                        })
                    })
                    .collect::<Vec<_>>()
            };
            for (fill_uid, generation, cache_uid, url, credential, prefixes) in candidates {
                if shared.stopping.get() {
                    return;
                }
                let key = format!("{cache_uid}/{fill_uid}");
                let result = client.list(&url, &credential, &key, &prefixes).await;
                let now = rt::now();
                let mut fills = shared.fills.borrow_mut();
                let Some(view) = fills.get_mut(&fill_uid) else {
                    continue;
                };
                if !view.listing_current(generation, &cache_uid) {
                    continue;
                }
                match result {
                    Ok(listed) => match manifest::build(
                        url.container(),
                        &listed,
                        &view.prefixes,
                        rt::wall_now()
                            .duration_since(std::time::UNIX_EPOCH)
                            .map_or(0, |d| d.as_secs()),
                    ) {
                        Ok(manifest) => {
                            view.manifest = Some(Arc::new(manifest));
                            view.listed_generation = Some(generation);
                            view.listed_cache_uid = cache_uid;
                            view.error = None;
                            view.failures = 0;
                            view.next_list = Some(now + Duration::from_secs(view.resync_secs));
                        }
                        Err(ManifestError::TooManyObjects) => {
                            // Keep the previous manifest, as on a list failure.
                            view.error = Some("TooManyObjects".into());
                            view.failures = view.failures.saturating_add(1);
                            view.next_list = Some(now + LIST_BACKOFF);
                        }
                        Err(ManifestError::InvalidObject) => {
                            view.error = Some("ListFailed".into());
                            view.failures = view.failures.saturating_add(1);
                            view.next_list = Some(now + LIST_BACKOFF);
                        }
                    },
                    Err(e) => {
                        view.error = Some(e.reason().into());
                        view.failures = view.failures.saturating_add(1);
                        let backoff = LIST_BACKOFF
                            .saturating_mul(1 << view.failures.min(5))
                            .min(RETRY_MAX);
                        view.next_list = Some(now + backoff);
                    }
                }
            }
        }
        recompute(&shared);
        rt::sleep(TICK).await;
    }
}

/// Recompute admission and the footprint plan for every fill from the current
/// cache catalog and publication.
pub fn recompute(shared: &Shared) {
    let (page_size, membership, pool_sizes) = {
        let serving = shared.serving.borrow();
        match serving.image.as_ref() {
            Some(image) => {
                let publication = &image.publication;
                let mut pools = BTreeMap::new();
                for pool in &publication.pools {
                    let members = pool
                        .members
                        .as_ref()
                        .map_or(publication.members.len(), Vec::len);
                    for cache in &pool.caches {
                        pools.insert(cache.clone(), members);
                    }
                }
                (
                    publication.page_size.get(),
                    Some(publication.membership_version.0),
                    pools,
                )
            }
            None => (shared.config.page_size.get(), None, BTreeMap::new()),
        }
    };
    let caches = shared.fills_caches.borrow();
    let mut fills = shared.fills.borrow_mut();
    for view in fills.values_mut() {
        view.membership = membership;
        let origin = catalog::fill_cache_named(&view.cache_name, &caches);
        match &origin {
            FillCache::Usable { uid, .. } => {
                view.cache_uid.clone_from(uid);
                if !view.listed_cache_uid.is_empty() && view.listed_cache_uid != view.cache_uid {
                    // A recreated or replaced cache invalidates the manifest.
                    view.manifest = None;
                    view.error = None;
                    view.listed_generation = None;
                    view.listed_cache_uid.clear();
                    view.next_list = None;
                    view.failures = 0;
                }
                view.accepted = true;
                view.message.clear();
                view.available_nodes = pool_sizes
                    .get(&racer_wire::control::CacheId(view.cache_uid.clone()))
                    .map_or(view.replicas, |members| {
                        u8::try_from(*members).unwrap_or(u8::MAX)
                    });
            }
            FillCache::CacheNotFound => {
                view.cache_uid.clear();
                view.accepted = false;
                view.message = "CacheNotFound".into();
                view.planned_footprint_bytes = None;
                continue;
            }
            FillCache::UnsupportedCache => {
                view.cache_uid.clear();
                view.accepted = false;
                view.message = "UnsupportedCache".into();
                view.planned_footprint_bytes = None;
                continue;
            }
            FillCache::UnsupportedOrigin => {
                view.cache_uid.clear();
                view.accepted = false;
                view.message = "UnsupportedOrigin".into();
                view.planned_footprint_bytes = None;
                continue;
            }
        }
        if view.error.as_deref() == Some("TooManyObjects") {
            view.accepted = false;
            view.message = "TooManyObjects".into();
            view.planned_footprint_bytes = None;
        } else if let Some(manifest) = &view.manifest {
            // Plan every requested replica so admission checks the requested
            // footprint. Missing pool members are reported as shortfall, not
            // folded into a smaller target.
            let planned = manifest::planned_footprint(manifest, page_size, view.replicas);
            view.planned_footprint_bytes = Some(planned);
            if planned > view.max_footprint {
                view.accepted = false;
                view.message = "ExceedsMaxFootprint".into();
            }
        } else {
            view.planned_footprint_bytes = None;
        }
    }
}
