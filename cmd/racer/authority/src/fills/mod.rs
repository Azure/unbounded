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
                    // The CRD bounds a prefix by characters, so count the same
                    // way. A byte limit would reject multibyte prefixes the API
                    // accepted.
                    if p.chars().count() > 1024 {
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
    // Admission follows the publication, not the origin: a node drops its pins
    // as soon as the membership version moves and cannot adopt a current plan
    // until admission is recomputed. A listing batch can take arbitrarily
    // long, so admission gets its own task instead of sharing origin I/O.
    let admission = shared.clone();
    let _ = rt::spawn(async move { watch_fills(fills).await });
    let _ = rt::spawn(async move { watch_caches(caches).await });
    let _ = rt::spawn(async move { list_fills(listing).await });
    let _ = rt::spawn(async move { admit_fills(admission).await });
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
                            install_manifest(&shared, view, manifest, generation, cache_uid, now)
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
                // Installing a manifest recomputes this view's admission.
                // Reapply the node-wide report budget before exposing it.
                enforce_report_limit(
                    &Admission::read(&shared),
                    &mut fills,
                    &shared.fills_caches.borrow(),
                );
            }
        }
        rt::sleep(TICK).await;
    }
}

/// Refresh admission every [`TICK`] independently of origin listing.
///
/// Admission is driven by the cache catalog and the publication's membership
/// version, so it must not queue behind a listing batch: the node controller
/// releases pins when the membership version moves and cannot adopt a current
/// plan until admission is refreshed.
async fn admit_fills(shared: Rc<Shared>) {
    while !shared.stopping.get() {
        recompute(&shared);
        rt::sleep(TICK).await;
    }
}

/// Admission inputs that do not change per fill.
struct Admission {
    page_size: u64,
    membership: Option<u64>,
    pool_members: BTreeMap<racer_wire::control::CacheId, Arc<[u32]>>,
}

impl Admission {
    /// Read the page size and publication admission is computed against.
    fn read(shared: &Shared) -> Self {
        let (page_size, membership, pool_members) = {
            let serving = shared.serving.borrow();
            match serving.image.as_ref() {
                Some(image) => {
                    let publication = &image.publication;
                    let mut pools = BTreeMap::new();
                    for pool in &publication.pools {
                        let members: Arc<[u32]> = pool
                            .members
                            .clone()
                            .unwrap_or_else(|| (0..publication.members.len() as u32).collect())
                            .into();
                        for cache in &pool.caches {
                            pools.insert(cache.clone(), members.clone());
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
        Self {
            page_size,
            membership,
            pool_members,
        }
    }

    /// Resolve the fill's cache and recompute admission and its footprint.
    fn admit(&self, view: &mut FillView, caches: &BTreeMap<String, CacheInfo>) {
        view.membership = self.membership;
        let origin = catalog::fill_cache_named(&view.cache_name, caches);
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
                view.available_nodes = self
                    .pool_members
                    .get(&racer_wire::control::CacheId(view.cache_uid.clone()))
                    .map_or(view.replicas, |members| {
                        u8::try_from(members.len()).unwrap_or(u8::MAX)
                    });
            }
            FillCache::CacheNotFound => {
                view.cache_uid.clear();
                view.accepted = false;
                view.message = "CacheNotFound".into();
                view.planned_footprint_bytes = None;
                return;
            }
            FillCache::UnsupportedCache => {
                view.cache_uid.clear();
                view.accepted = false;
                view.message = "UnsupportedCache".into();
                view.planned_footprint_bytes = None;
                return;
            }
            FillCache::UnsupportedOrigin => {
                view.cache_uid.clear();
                view.accepted = false;
                view.message = "UnsupportedOrigin".into();
                view.planned_footprint_bytes = None;
                return;
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
            let planned = manifest::planned_footprint(manifest, self.page_size, view.replicas);
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

/// Recompute admission and the footprint plan for every fill from the current
/// cache catalog and publication.
pub fn recompute(shared: &Shared) {
    let admission = Admission::read(shared);
    let caches = shared.fills_caches.borrow();
    let mut fills = shared.fills.borrow_mut();
    enforce_report_limit(&admission, &mut fills, &caches);
}

/// Reserve slots in UID order on every node in the fill's published cache pool.
/// Pending listings reserve slots too. A fill is rejected if any participating
/// node is full, without spending slots on its other nodes. Disjoint pools do
/// not compete, and structurally rejected fills do not spend report slots.
fn enforce_report_limit(
    admission: &Admission,
    fills: &mut BTreeMap<String, FillView>,
    caches: &BTreeMap<String, CacheInfo>,
) {
    let mut slots = BTreeMap::<u32, usize>::new();
    for view in fills.values_mut() {
        // Recompute previous TooManyFills decisions as well, so freed slots
        // and newly rejected manifests are accounted for in the same pass.
        admission.admit(view, caches);
        if !view.accepted {
            continue;
        }
        let Some(members) = admission
            .pool_members
            .get(&racer_wire::control::CacheId(view.cache_uid.clone()))
        else {
            // Until the cache is published there is no membership plan to pin
            // or report. Admission will reserve slots when a pool appears.
            continue;
        };
        if members
            .iter()
            .any(|node| slots.get(node).copied().unwrap_or(0) >= crate::limits::MAX_REPORT_FILLS)
        {
            view.accepted = false;
            view.message = "TooManyFills".into();
            continue;
        }
        for node in members.iter() {
            *slots.entry(*node).or_default() += 1;
        }
    }
}

/// Recompute one fill from the current cache catalog and publication.
fn recompute_view(shared: &Shared, view: &mut FillView) {
    let admission = Admission::read(shared);
    let caches = shared.fills_caches.borrow();
    admission.admit(view, &caches);
}

/// Replace a view's manifest from a successful listing and recompute the
/// admission fields the new manifest implies before any other await can
/// expose the view.
fn install_manifest(
    shared: &Shared,
    view: &mut FillView,
    manifest: Manifest,
    generation: u64,
    cache_uid: String,
    now: Instant,
) {
    view.manifest = Some(Arc::new(manifest));
    view.listed_generation = Some(generation);
    view.listed_cache_uid = cache_uid;
    view.error = None;
    view.failures = 0;
    view.next_list = Some(now + Duration::from_secs(view.resync_secs));
    recompute_view(shared, view);
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::rules::catalog::CacheInfo;

    fn add_cache(shared: &Shared, i: usize) {
        let uid = format!("22222222-2222-4222-8222-{i:012}");
        shared.fills_caches.borrow_mut().insert(
            uid.clone(),
            CacheInfo::from_json(&serde_json::json!({
                "metadata": {"uid": uid, "name": format!("models-{i}")},
                "spec": {"origin": {"azure": {"containerURL":
                    "https://acct.blob.core.windows.net/models"}}},
            })),
        );
    }

    fn add_pending_fill(shared: &Shared, i: usize, cache: usize) -> String {
        let mut value = fill_json("");
        let uid = format!("11111111-1111-4111-8111-{i:012}");
        value["metadata"]["uid"] = serde_json::json!(uid);
        value["spec"]["cacheName"] = serde_json::json!(format!("models-{cache}"));
        let view = FillView::from_json(&value).unwrap();
        shared.fills.borrow_mut().insert(uid.clone(), view);
        uid
    }

    fn install_pools(shared: &Shared, nodes: usize, pools: Vec<(Option<Vec<u32>>, Vec<usize>)>) {
        use crate::rules::publication::Image;
        use racer_wire::control as wire;
        let caches = shared
            .fills_caches
            .borrow()
            .values()
            .cloned()
            .collect::<Vec<_>>();
        let publication = wire::Publication {
            schema_version: wire::SCHEMA_VERSION,
            cluster: wire::ClusterId("33333333-3333-4333-8333-333333333333".into()),
            sequence: wire::PublicationSequence(1),
            membership_version: wire::MembershipVersion(1),
            members: (0..nodes)
                .map(|i| wire::Member {
                    node: wire::NodeId(format!("44444444-4444-4444-8444-{i:012}")),
                    shares: std::num::NonZeroU32::MIN,
                    peer_endpoint: format!("192.0.2.{}:8082", i + 1),
                    rails: vec![],
                    site: String::new(),
                })
                .collect(),
            caches: catalog::build(&caches).unwrap().caches,
            pools: pools
                .into_iter()
                .enumerate()
                .map(|(i, (members, caches))| wire::Pool {
                    id: if members.is_none() {
                        wire::PoolId::default_pool()
                    } else {
                        wire::PoolId(format!("sel-{i:032x}"))
                    },
                    rank: if members.is_none() { 0 } else { 1 << 32 },
                    members,
                    caches: caches
                        .into_iter()
                        .map(|i| wire::CacheId(format!("22222222-2222-4222-8222-{i:012}")))
                        .collect(),
                })
                .collect(),
            page_size: shared.config.page_size,
        };
        let encoded = wire::encode_publication(&publication).unwrap();
        shared.serving.borrow_mut().image =
            Some(Image::new(Default::default(), publication, encoded, None));
    }

    #[test]
    fn replacing_a_manifest_recomputes_admission_before_it_is_visible() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let cache_uid = "22222222-2222-4222-8222-222222222222";
            let cache = CacheInfo::from_json(&serde_json::json!({
                "metadata": {"uid": cache_uid, "name": "models"},
                "spec": {"origin": {"azure": {"containerURL":
                    "https://acct.blob.core.windows.net/models"}}},
            }));
            shared
                .fills_caches
                .borrow_mut()
                .insert(cache_uid.to_owned(), cache);
            let mut view = FillView::from_json(&serde_json::json!({
                "metadata": {
                    "uid": "11111111-1111-4111-8111-111111111111",
                    "name": "fill",
                    "generation": 2,
                },
                "spec": {
                    "cacheName": "models",
                    "replicas": 2,
                    "maxFootprint": 1_000_000_000,
                    "prefixes": [""],
                },
            }))
            .unwrap();
            let page = shared.config.page_size.get();
            let old = manifest::build(
                "models",
                &[manifest::ListedObject {
                    name: "old".into(),
                    etag: "\"e\"".into(),
                    length: page,
                    content_type: None,
                }],
                &["".into()],
                1,
            )
            .unwrap();
            view.manifest = Some(Arc::new(old));
            view.listed_generation = Some(view.generation);
            view.listed_cache_uid = cache_uid.into();
            // Deliberately stale, as left by the previous manifest.
            view.planned_footprint_bytes = Some(1);

            let new = manifest::build(
                "models",
                &[manifest::ListedObject {
                    name: "new".into(),
                    etag: "\"e2\"".into(),
                    length: page + 1,
                    content_type: None,
                }],
                &["".into()],
                2,
            )
            .unwrap();
            let expected = manifest::planned_footprint(&new, page, view.replicas);
            assert_ne!(Some(expected), view.planned_footprint_bytes);

            let generation = view.generation;
            install_manifest(
                &shared,
                &mut view,
                new,
                generation,
                cache_uid.into(),
                rt::now(),
            );

            // The new manifest and the footprint it implies become visible
            // together; no snapshot sees the old plan with the new manifest.
            assert_eq!(view.planned_footprint_bytes, Some(expected));
            assert_eq!(view.listed_generation, Some(generation));
            assert_eq!(view.listed_cache_uid, cache_uid);
            assert!(view.error.is_none());
            assert!(view.accepted);
        });
    }

    fn fill_json(prefix: &str) -> serde_json::Value {
        serde_json::json!({
            "metadata": {
                "uid": "11111111-1111-4111-8111-111111111111",
                "name": "fill",
            },
            "spec": {
                "cacheName": "models",
                "prefixes": [prefix],
                "maxFootprint": 1_000_000_000,
            },
        })
    }

    #[test]
    fn report_slots_bound_admission_across_caches_and_recover_after_deletion() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            for i in 0..2 {
                let uid = format!("22222222-2222-4222-8222-{i:012}");
                shared.fills_caches.borrow_mut().insert(
                    uid.clone(),
                    CacheInfo::from_json(&serde_json::json!({
                        "metadata": {"uid": uid, "name": format!("models-{i}")},
                        "spec": {"origin": {"azure": {"containerURL":
                            "https://acct.blob.core.windows.net/models"}}},
                    })),
                );
            }
            install_pools(&shared, 1, vec![(None, vec![0, 1])]);
            // Reverse insertion order and alternate caches: the bound is on
            // this node's entire inventory, with stable UID ordering.
            for i in (0..20).rev() {
                let mut value = fill_json("");
                value["metadata"]["uid"] =
                    serde_json::json!(format!("11111111-1111-4111-8111-{i:012}"));
                value["spec"]["cacheName"] = serde_json::json!(format!("models-{}", i % 2));
                let view = FillView::from_json(&value).unwrap();
                shared.fills.borrow_mut().insert(view.uid.clone(), view);
            }
            recompute(&shared);
            let check = || {
                let fills = shared.fills.borrow();
                assert_eq!(fills.values().filter(|v| v.accepted).count(), 16);
                for (i, view) in fills.values().enumerate() {
                    assert_eq!(view.accepted, i < crate::limits::MAX_REPORT_FILLS);
                    if !view.accepted {
                        assert_eq!(view.message, "TooManyFills");
                        let status = status::status(view, &BTreeMap::new(), None);
                        let accepted = status["conditions"]
                            .as_array()
                            .unwrap()
                            .iter()
                            .find(|condition| condition["type"] == "Accepted")
                            .unwrap();
                        assert_eq!(accepted["status"], "False");
                        assert_eq!(accepted["reason"], "TooManyFills");
                    }
                }
            };
            check();
            // A successful listing must not bypass admission until the next
            // periodic recompute.
            {
                let mut fills = shared.fills.borrow_mut();
                let view = fills.last_entry().unwrap().into_mut();
                let generation = view.generation;
                let cache_uid = view.cache_uid.clone();
                install_manifest(
                    &shared,
                    view,
                    manifest::build("models", &[], &["".into()], 1).unwrap(),
                    generation,
                    cache_uid,
                    rt::now(),
                );
                enforce_report_limit(
                    &Admission::read(&shared),
                    &mut fills,
                    &shared.fills_caches.borrow(),
                );
            }
            check();
            shared.fills.borrow_mut().pop_first();
            recompute(&shared);
            check();
        });
    }

    #[test]
    fn disjoint_pools_do_not_compete_for_report_slots() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            for i in (0..17).rev() {
                add_cache(&shared, i);
                add_pending_fill(&shared, i, i);
            }
            install_pools(
                &shared,
                17,
                (0..17).map(|i| (Some(vec![i as u32]), vec![i])).collect(),
            );
            recompute(&shared);
            let fills = shared.fills.borrow();
            assert_eq!(fills.len(), 17);
            assert!(
                fills
                    .values()
                    .all(|view| view.accepted && view.manifest.is_none())
            );
            assert!(fills.values().all(|view| view.available_nodes == 1));
        });
    }

    #[test]
    fn overlapping_pools_share_slots_and_rejections_do_not_spend_them() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            for i in 0..3 {
                add_cache(&shared, i);
            }
            install_pools(
                &shared,
                2,
                vec![
                    (Some(vec![0]), vec![0]),
                    (Some(vec![1]), vec![1]),
                    (Some(vec![0, 1]), vec![2]),
                ],
            );
            // Pending listings reserve slots: node 1 has 15, node 0 has 16.
            for i in (0..31).rev() {
                add_pending_fill(&shared, i, usize::from(i < 15));
            }
            let overlap = add_pending_fill(&shared, 31, 2);
            let last = add_pending_fill(&shared, 32, 1);
            recompute(&shared);
            {
                let fills = shared.fills.borrow();
                assert_eq!(fills.values().filter(|view| view.accepted).count(), 32);
                assert_eq!(fills[&overlap].message, "TooManyFills");
                assert!(!fills[&overlap].accepted);
                // Rejecting the overlap at node 0 must not consume node 1's
                // final slot. A per-cache limit would admit all 33 fills.
                assert!(fills[&last].accepted);
            }
            // A structurally rejected earlier fill frees node 0's slot. The
            // overlap is admitted, and node 1's later fill is now rejected.
            let rejected = format!("11111111-1111-4111-8111-{:012}", 15);
            shared.fills.borrow_mut().get_mut(&rejected).unwrap().error =
                Some("TooManyObjects".into());
            recompute(&shared);
            let fills = shared.fills.borrow();
            assert_eq!(fills[&rejected].message, "TooManyObjects");
            assert!(fills[&overlap].accepted);
            assert_eq!(fills[&last].message, "TooManyFills");
            assert!(!fills[&last].accepted);
        });
    }

    #[test]
    fn unpublished_pools_do_not_reserve_report_slots() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            add_cache(&shared, 0);
            for i in 0..17 {
                add_pending_fill(&shared, i, 0);
            }
            recompute(&shared);
            assert!(
                shared
                    .fills
                    .borrow()
                    .values()
                    .all(|view| view.accepted && view.membership.is_none())
            );
            // Once publication supplies an applicable node, slots are bounded.
            install_pools(&shared, 1, vec![(None, vec![0])]);
            recompute(&shared);
            assert_eq!(
                shared
                    .fills
                    .borrow()
                    .values()
                    .filter(|view| view.accepted)
                    .count(),
                16
            );
        });
    }

    #[test]
    fn multibyte_prefix_within_the_character_limit_is_accepted() {
        // 400 characters, 1200 UTF-8 bytes: the CRD accepts it, so the parser
        // must not drop the fill.
        let prefix = "雪".repeat(400);
        let view = FillView::from_json(&fill_json(&prefix)).expect("within the API limit");
        assert_eq!(view.prefixes, vec![prefix]);
    }

    #[test]
    fn prefix_over_the_character_limit_is_rejected() {
        let prefix = "雪".repeat(1025);
        assert!(FillView::from_json(&fill_json(&prefix)).is_none());
    }

    #[test]
    fn admission_refreshes_on_its_own_task() {
        crate::kube::fake::run(|_api, kube| async move {
            let shared = crate::shared::tests::shared(kube);
            let cache_uid = "22222222-2222-4222-8222-222222222222";
            let cache = CacheInfo::from_json(&serde_json::json!({
                "metadata": {"uid": cache_uid, "name": "models"},
                "spec": {"origin": {"azure": {"containerURL":
                    "https://acct.blob.core.windows.net/models"}}},
            }));
            shared
                .fills_caches
                .borrow_mut()
                .insert(cache_uid.to_owned(), cache);
            let view = FillView::from_json(&serde_json::json!({
                "metadata": {
                    "uid": "11111111-1111-4111-8111-111111111111",
                    "name": "fill",
                    "generation": 1,
                },
                "spec": {
                    "cacheName": "models",
                    "maxFootprint": 1_000_000_000,
                    "prefixes": [""],
                },
            }))
            .unwrap();
            assert!(!view.accepted);
            let uid = view.uid.clone();
            shared.fills.borrow_mut().insert(uid.clone(), view);

            // No listing runs here: admission alone must resolve the cache,
            // as it does when the publication changes during a slow listing.
            rt::spawn(admit_fills(shared.clone())).unwrap();
            rt::sleep(Duration::from_millis(10)).await;

            let fills = shared.fills.borrow();
            let view = fills.get(&uid).unwrap();
            assert!(view.accepted);
            assert_eq!(view.cache_uid, cache_uid);
            shared.stopping.set(true);
        });
    }
}
