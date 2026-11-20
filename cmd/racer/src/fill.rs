//! ClusterCacheFill node controller.
//!
//! One controller runs per I/O worker. It reads the process-wide fill view,
//! fetches the pages this worker owns through the normal prefetch path, and
//! pins them while they remain desired. Progress is aggregated per
//! node and reported through the separate Pod fills annotation.
//!
//! Pins are desired state, not durable state: a restart drops them and the
//! controller establishes them again from the manifest. Plan changes retain
//! readable pins this worker still owns and release only obsolete pages.

use crate::control::Managed;
use crate::error::{Error, Result};
use crate::model::{
    ContentType, ObjectId, ObjectVersion, PageId, PageNumber, RequestId, VersionMetadata, WorkerId,
};
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::security::{OpaqueMetadata, OriginContext};
use crate::store::catalog::Index;
use crate::store::holds::HoldPages;
use crate::store::pins::{FillId, PinEpoch};
use racer_authority::{FillObject, FillProgress, Manifest};
use racer_core::prefetch::PageResult;
use racer_wire::control::CacheId;
use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{Duration, Instant};

/// How often a worker looks for new fills or decisions.
const REFRESH: Duration = Duration::from_secs(2);
/// How often the control worker refreshes the Pod report.
const REPORT: Duration = Duration::from_secs(5);
/// How often live pins are extended.
const RENEW: Duration = Duration::from_secs(5 * 60);
/// Pin lifetime. Longer than [`RENEW`] so a late turn does not expire one.
const PIN_TTL: Duration = Duration::from_secs(60 * 60);
/// Initial retry ceiling, doubled per failed attempt up to [`RETRY_MAX`].
const RETRY_MIN: Duration = Duration::from_secs(10);
const RETRY_MAX: Duration = Duration::from_secs(5 * 60);
/// Bound on one fill's page fetch.
const FETCH_TIMEOUT: Duration = Duration::from_secs(120);
/// Scheduling work one worker may perform in one service turn, per phase.
///
/// Pages this worker does not own count: a node outside the cache pool walks
/// every page without fetching any, so the manifest object cap does not bound a
/// turn. Keeping the walk small leaves the I/O reactor to foreground reads and
/// control work. Fill visits, manifest pages, and object transitions share the
/// launch budget. Completion polling has the same cap and rotates separately.
const SCAN_BUDGET: usize = 256;

/// One fill's progress as one worker sees it.
#[derive(Clone, Debug, Default)]
struct WorkerProgress {
    generation: u64,
    membership: u64,
    digest: String,
    owned_pages: u64,
    pinned_pages: u64,
    owned_bytes: u64,
    pinned_bytes: u64,
    refused_bytes: u64,
}

/// Node-wide progress, written by every worker and read by the control worker.
#[derive(Default)]
pub struct FillShare {
    fills: Mutex<BTreeMap<(u16, String), WorkerProgress>>,
    capacities: Mutex<BTreeMap<u16, u64>>,
}

impl FillShare {
    pub fn new() -> Self {
        Self::default()
    }

    fn set(&self, worker: WorkerId, fill: &str, progress: WorkerProgress) {
        self.fills
            .lock()
            .unwrap()
            .insert((worker.0, fill.to_owned()), progress);
    }

    /// Record this worker's pinnable bytes even when it holds no fill.
    fn set_capacity(&self, worker: WorkerId, capacity: u64) {
        self.capacities.lock().unwrap().insert(worker.0, capacity);
    }

    fn forget(&self, worker: WorkerId, keep: &BTreeSet<String>) {
        let mut fills = self.fills.lock().unwrap();
        fills.retain(|(id, fill), _| *id != worker.0 || keep.contains(fill));
    }

    /// Aggregate this node's reports. A fill is reported only when every
    /// contributing worker agrees on generation, membership, and digest.
    fn report(&self) -> (Vec<FillProgress>, u64) {
        let fills = self.fills.lock().unwrap();
        let capacities = self.capacities.lock().unwrap();
        let mut by_fill: BTreeMap<&str, FillProgress> = BTreeMap::new();
        let mut consistent: BTreeMap<&str, bool> = BTreeMap::new();
        for ((_worker, fill), progress) in fills.iter() {
            let entry = by_fill.entry(fill.as_str());
            match entry {
                std::collections::btree_map::Entry::Vacant(slot) => {
                    consistent.insert(fill.as_str(), true);
                    slot.insert(FillProgress {
                        fill: fill.clone(),
                        generation: progress.generation,
                        digest: progress.digest.clone(),
                        membership: progress.membership,
                        owned_pages: progress.owned_pages,
                        pinned_pages: progress.pinned_pages,
                        owned_bytes: progress.owned_bytes,
                        pinned_bytes: progress.pinned_bytes,
                        refused_bytes: progress.refused_bytes,
                    });
                }
                std::collections::btree_map::Entry::Occupied(mut slot) => {
                    let entry = slot.get_mut();
                    if entry.generation != progress.generation
                        || entry.membership != progress.membership
                        || entry.digest != progress.digest
                    {
                        consistent.insert(fill.as_str(), false);
                        continue;
                    }
                    entry.owned_pages = entry.owned_pages.saturating_add(progress.owned_pages);
                    entry.pinned_pages = entry.pinned_pages.saturating_add(progress.pinned_pages);
                    entry.owned_bytes = entry.owned_bytes.saturating_add(progress.owned_bytes);
                    entry.pinned_bytes = entry.pinned_bytes.saturating_add(progress.pinned_bytes);
                    entry.refused_bytes =
                        entry.refused_bytes.saturating_add(progress.refused_bytes);
                }
            }
        }
        let progress = by_fill
            .into_iter()
            .filter(|(fill, _)| consistent.get(fill).copied().unwrap_or(false))
            .map(|(_, progress)| progress)
            .collect();
        let capacity: u64 = capacities.values().copied().sum();
        let used: u64 = fills.values().map(|p| p.pinned_bytes).sum();
        (progress, capacity.saturating_sub(used))
    }
}

struct Inflight {
    fill: String,
    generation: u64,
    object: usize,
    page: u64,
    attempt: u32,
    epoch: PinEpoch,
    accounted: u64,
    version: ObjectVersion,
    future: Pin<Box<dyn Future<Output = Result<PageResult>>>>,
}

struct LocalFill {
    /// Authority and local membership must agree before launching more work.
    paused: bool,
    generation: u64,
    digest: String,
    membership: u64,
    manifest: Arc<Manifest>,
    cache: CacheId,
    replicas: u8,
    fetching: bool,
    object: usize,
    page: u64,
    pins: BTreeMap<PageId, PinEpoch>,
    resident: BTreeSet<PageId>,
    counted: BTreeSet<PageId>,
    refused: BTreeSet<PageId>,
    versions: BTreeSet<ObjectVersion>,
    owned_pages: u64,
    pinned_pages: u64,
    owned_bytes: u64,
    pinned_bytes: u64,
    refused_bytes: u64,
    /// Failed pages ordered by deadline, independent of the manifest cursor.
    retries: BTreeMap<(Instant, usize, u64), u32>,
}

impl LocalFill {
    fn retry(
        &mut self,
        seed: u64,
        now: Instant,
        object: usize,
        page: u64,
        attempt: u32,
        retry_after: Option<Duration>,
    ) {
        let attempt = attempt.saturating_add(1);
        let seed = seed ^ (object as u64).rotate_left(17) ^ page.rotate_left(31);
        let delay = retry_delay(seed, attempt).max(retry_after.unwrap_or_default());
        self.retries.insert((now + delay, object, page), attempt);
    }

    /// Restart the manifest walk without opening an eviction window for pages
    /// retained by the replacement plan. Canceled reservations are released:
    /// only completed pages can be skipped by the next walk.
    fn reconcile(
        &mut self,
        index: &Index,
        uid: &str,
        page_size: u64,
        _now: Instant,
        mut keep: impl FnMut(&PageId) -> bool,
    ) {
        self.pins.retain(|page, epoch| {
            if self.resident.contains(page) && keep(page) {
                return true;
            }
            let pages = std::iter::once(page.number).collect();
            index.release_pin_pages(fill_id(uid), &page.version, &pages, *epoch);
            false
        });
        self.resident.retain(|page| self.pins.contains_key(page));
        self.counted = self.resident.clone();
        self.versions = self.pins.keys().map(|page| page.version.clone()).collect();
        self.owned_pages = self.counted.len() as u64;
        self.owned_bytes = self.owned_pages.saturating_mul(page_size);
        self.pinned_pages = self.resident.len() as u64;
        self.pinned_bytes = self.pinned_pages.saturating_mul(page_size);
        self.refused.clear();
        self.refused_bytes = 0;
        self.object = 0;
        self.page = 0;
        // Retry object indexes belong to the canceled walk, not its replacement.
        self.retries.clear();
        self.fetching = false;
    }

    fn progress(&self) -> WorkerProgress {
        WorkerProgress {
            generation: self.generation,
            membership: self.membership,
            digest: self.digest.clone(),
            owned_pages: self.owned_pages,
            pinned_pages: self.pinned_pages,
            owned_bytes: self.owned_bytes,
            pinned_bytes: self.pinned_bytes,
            refused_bytes: self.refused_bytes,
        }
    }
}

/// What one bounded pass over a fill's manifest found.
enum ScanStep {
    /// No manifest pages remain.
    Exhausted,
    /// The visit budget ran out with pages unexamined; a wakeup was requested.
    Budget,
    /// This worker owns the page; the cursor has advanced past it.
    Owned(FillObject, u64),
}

/// Spend at most `budget` work units from `state`'s cursor,
/// including pages `owned` rejects. Returns the first owned page without
/// losing failed pages: launch and finish queue them separately for retry.
fn scan_fill(
    cx: &mut Context<'_>,
    state: &mut LocalFill,
    page_size: u64,
    budget: &mut usize,
    mut owned: impl FnMut(&FillObject, u64) -> Result<bool>,
) -> Result<ScanStep> {
    while let Some(object) = state.manifest.objects.get(state.object).cloned() {
        if *budget == 0 {
            cx.waker().wake_by_ref();
            return Ok(ScanStep::Budget);
        }
        *budget -= 1;
        let pages = object.length.div_ceil(page_size);
        if state.page >= pages {
            state.object += 1;
            state.page = 0;
            continue;
        }
        let number = state.page;
        if owned(&object, number)? {
            state.page += 1;
            return Ok(ScanStep::Owned(object, number));
        }
        state.page += 1;
    }
    Ok(ScanStep::Exhausted)
}

/// Per-worker fill controller.
pub struct FillController {
    worker: WorkerId,
    fills: crate::control::Fills,
    report: crate::control::FillReport,
    share: Arc<FillShare>,
    index: Rc<Index>,
    coordinator: Rc<Coordinator>,
    clock: racer_io::Clock,
    local: BTreeMap<String, LocalFill>,
    inflight: VecDeque<Inflight>,
    launch_after: Option<String>,
    launch_remaining: usize,
    finish_remaining: usize,
    next_refresh: Instant,
    next_report: Instant,
    next_renew: Instant,
}

impl FillController {
    pub fn new(
        worker: WorkerId,
        managed: &Managed,
        share: Arc<FillShare>,
        index: Rc<Index>,
        coordinator: Rc<Coordinator>,
        clock: racer_io::Clock,
    ) -> Self {
        let now = clock.now();
        Self {
            worker,
            fills: managed.fills.clone(),
            report: managed.fill_report.clone(),
            share,
            index,
            coordinator,
            clock,
            local: BTreeMap::new(),
            inflight: VecDeque::new(),
            launch_after: None,
            launch_remaining: 0,
            finish_remaining: 0,
            next_refresh: now,
            next_report: now + REPORT,
            next_renew: now + RENEW,
        }
    }

    /// Advance the controller. Called once per worker service turn.
    pub fn poll(&mut self, cx: &mut Context<'_>) -> Result<()> {
        self.finish(cx);
        let now = self.clock.now();
        if now >= self.next_refresh {
            self.refresh(now);
            self.next_refresh = now + REFRESH;
        }
        self.launch(cx, now)?;
        if now >= self.next_renew {
            self.renew(now);
            self.next_renew = now + RENEW;
        }
        self.publish(now);
        Ok(())
    }

    /// Drop a fill and everything this worker pinned for it.
    fn forget(&mut self, fill: &str) {
        // A rejected fill can become accepted again without a new generation.
        // Drop its receivers so an obsolete completion never credits a page
        // or advances the replacement plan's cursor.
        forget_inflight(&mut self.inflight, fill);
        if let Some(state) = self.local.remove(fill) {
            let id = fill_id(fill);
            for (page, epoch) in state.pins {
                let pages: HoldPages = std::iter::once(page.number).collect();
                self.index
                    .release_pin_pages(id, &page.version, &pages, epoch);
            }
        }
    }

    /// Adopt replacement plans without releasing pins they still require.
    fn refresh(&mut self, now: Instant) {
        let views = (self.fills)();
        let mut seen = BTreeSet::new();
        for view in views {
            if !view.accepted {
                self.forget(&view.uid);
                continue;
            }
            let (Some(manifest), Some(membership)) = (view.manifest.clone(), view.membership)
            else {
                self.forget(&view.uid);
                continue;
            };
            seen.insert(view.uid.clone());
            let Some(current) = self.coordinator.current_membership() else {
                continue;
            };
            if current.version.0 != membership {
                // The authority planned against a newer membership than this
                // node has adopted. Wait instead of pinning a stale ranking.
                if let Some(state) = self.local.get_mut(&view.uid) {
                    state.paused = true;
                }
                continue;
            }
            if let Some(state) = self.local.get_mut(&view.uid) {
                state.paused = false;
            }
            let Ok(page_size) = self.coordinator.page_size() else {
                continue;
            };
            let stale = self.local.get(&view.uid).is_none_or(|state| {
                state.generation != view.generation
                    || state.digest != manifest.digest
                    || state.membership != membership
                    || state.cache.0 != view.cache_uid
                    || state.replicas != view.replicas.max(1)
            });
            if stale {
                forget_inflight(&mut self.inflight, &view.uid);
                let old = self.local.remove(&view.uid);
                self.local.insert(
                    view.uid.clone(),
                    LocalFill {
                        paused: false,
                        generation: view.generation,
                        digest: manifest.digest.clone(),
                        membership,
                        manifest,
                        cache: CacheId(view.cache_uid.clone()),
                        replicas: view.replicas.max(1),
                        fetching: false,
                        object: 0,
                        page: 0,
                        pins: BTreeMap::new(),
                        resident: BTreeSet::new(),
                        counted: BTreeSet::new(),
                        refused: BTreeSet::new(),
                        versions: BTreeSet::new(),
                        owned_pages: 0,
                        pinned_pages: 0,
                        owned_bytes: 0,
                        pinned_bytes: 0,
                        refused_bytes: 0,
                        retries: BTreeMap::new(),
                    },
                );
                if let Some(mut old) = old {
                    let state = self.local.get_mut(&view.uid).expect("just inserted");
                    // is_pinned is page-wide and could be satisfied by another
                    // fill. Verify this fill's own reservations before reuse.
                    let live: BTreeSet<_> = old
                        .versions
                        .iter()
                        .filter(|version| {
                            !matches!(
                                self.index.renew_pin(
                                    fill_id(&view.uid),
                                    version,
                                    self.clock.wall_now() + PIN_TTL,
                                ),
                                Err(Error::Unavailable)
                            )
                        })
                        .cloned()
                        .collect();
                    let objects: BTreeMap<_, _> = state
                        .manifest
                        .objects
                        .iter()
                        .map(|object| {
                            (
                                ObjectVersion {
                                    object: ObjectId {
                                        cache: state.cache.clone(),
                                        key: object.key,
                                    },
                                    etag: object.etag.clone(),
                                },
                                object.length.div_ceil(page_size.get()),
                            )
                        })
                        .collect();
                    let directory = self.coordinator.prefetch_directory();
                    old.reconcile(&self.index, &view.uid, page_size.get(), now, |page| {
                        objects
                            .get(&page.version)
                            .is_some_and(|pages| page.number.0 < *pages)
                            && self
                                .coordinator
                                .rank_position(current.clone(), page)
                                .is_some_and(|rank| rank < usize::from(state.replicas))
                            && directory
                                .page_owner(page)
                                .is_ok_and(|owner| owner == self.worker)
                            && live.contains(&page.version)
                            && !matches!(self.index.lookup(page), Ok(None))
                    });
                    state.pins = old.pins;
                    state.resident = old.resident;
                    state.counted = old.counted;
                    state.versions = old.versions;
                    state.owned_pages = old.owned_pages;
                    state.owned_bytes = old.owned_bytes;
                    state.pinned_pages = old.pinned_pages;
                    state.pinned_bytes = old.pinned_bytes;
                }
            }
        }
        let gone: Vec<String> = self
            .local
            .keys()
            .filter(|uid| !seen.contains(*uid))
            .cloned()
            .collect();
        for uid in gone {
            self.forget(&uid);
        }
        self.share.forget(self.worker, &seen);
        let capacity = self.index.pin_capacity_bytes();
        self.share.set_capacity(self.worker, capacity);
        for (uid, state) in &self.local {
            self.share.set(self.worker, uid, state.progress());
        }
    }

    /// Start one page fetch per fill that is not already fetching. The scan is
    /// bounded across all fills and rotates so a long scan cannot starve the
    /// next fill. A sweep wakes until every fill gets a scheduling opportunity.
    fn launch(&mut self, cx: &mut Context<'_>, now: Instant) -> Result<()> {
        let Ok(page_size) = self.coordinator.page_size() else {
            return Ok(());
        };
        let Some(membership) = self.coordinator.current_membership() else {
            return Ok(());
        };
        let page_size = page_size.get();
        let directory = self.coordinator.prefetch_directory().clone();
        let coordinator = &self.coordinator;
        let worker = self.worker;
        let retry_seed = fill_id(&coordinator.local_node().0).0 ^ u64::from(worker.0);
        use std::ops::Bound::{Excluded, Unbounded};
        if self.launch_remaining == 0 {
            self.launch_remaining = self.local.len();
        }
        let fills: Vec<String> = match &self.launch_after {
            Some(after) => self
                .local
                .range((Excluded(after.clone()), Unbounded))
                .chain(self.local.range(..=after.clone()))
                .take(self.launch_remaining.min(SCAN_BUDGET))
                .map(|(uid, _)| uid.clone())
                .collect(),
            None => self
                .local
                .keys()
                .take(self.launch_remaining.min(SCAN_BUDGET))
                .cloned()
                .collect(),
        };
        let mut budget = SCAN_BUDGET;
        for uid in fills {
            if budget == 0 {
                break;
            }
            budget -= 1;
            self.launch_remaining = self.launch_remaining.saturating_sub(1);
            self.launch_after = Some(uid.clone());
            let state = self.local.get_mut(&uid).expect("listed locally");
            if state.fetching || state.paused || state.membership != membership.version.0 {
                continue;
            }
            let cache = state.cache.clone();
            let replicas = state.replicas;
            let retry = state
                .retries
                .first_key_value()
                .and_then(|(key, _)| (key.0 <= now).then_some(*key));
            let (object_index, object, page_number, attempt) = if let Some(key) = retry {
                let attempt = state.retries.remove(&key).expect("listed retry");
                (key.1, state.manifest.objects[key.1].clone(), key.2, attempt)
            } else {
                let resident = std::mem::take(&mut state.resident);
                let step = scan_fill(cx, state, page_size, &mut budget, |object, number| {
                    let page = PageId {
                        version: ObjectVersion {
                            object: ObjectId {
                                cache: cache.clone(),
                                key: object.key,
                            },
                            etag: object.etag.clone(),
                        },
                        number: PageNumber(number),
                    };
                    let ranked = coordinator
                        .rank_position(membership.clone(), &page)
                        .is_some_and(|position| position < usize::from(replicas));
                    if !ranked || resident.contains(&page) {
                        return Ok(false);
                    }
                    Ok(directory.page_owner(&page)? == worker)
                });
                state.resident = resident;
                let step = step?;
                let ScanStep::Owned(object, page_number) = step else {
                    continue;
                };
                (state.object, object, page_number, 0)
            };
            let id = fill_id(&uid);
            let version = ObjectVersion {
                object: ObjectId {
                    cache: state.cache.clone(),
                    key: object.key,
                },
                etag: object.etag,
            };
            let page = PageId {
                version: version.clone(),
                number: PageNumber(page_number),
            };
            if state.counted.insert(page.clone()) {
                state.owned_pages += 1;
                state.owned_bytes = state.owned_bytes.saturating_add(page_size);
            }
            let pages: HoldPages = std::iter::once(page.number).collect();
            let epoch = match self.index.reserve_pin_pages(
                id,
                &version,
                &pages,
                self.clock.wall_now() + PIN_TTL,
            ) {
                Ok(epoch) => epoch,
                Err(Error::Overloaded) => {
                    // The reservation API signals capacity pressure with
                    // Overloaded. Count it once per page while retrying.
                    if state.refused.insert(page.clone()) {
                        state.refused_bytes = state.refused_bytes.saturating_add(page_size);
                    }
                    state.retry(
                        fill_id(&uid).0 ^ retry_seed,
                        now,
                        object_index,
                        page_number,
                        attempt,
                        None,
                    );
                    continue;
                }
                Err(_) => {
                    // This is background work. An unavailable page key or an
                    // index/configuration failure must not stop the worker's
                    // foreground service or lose this page from the plan.
                    // Only reservation Overloaded contributes refused bytes;
                    // another error clears an earlier refusal.
                    if state.refused.remove(&page) {
                        state.refused_bytes = state.refused_bytes.saturating_sub(page_size);
                    }
                    state.retry(
                        fill_id(&uid).0 ^ retry_seed,
                        now,
                        object_index,
                        page_number,
                        attempt,
                        None,
                    );
                    continue;
                }
            };
            state.pins.insert(page.clone(), epoch);
            state.versions.insert(version.clone());
            if state.refused.remove(&page) {
                state.refused_bytes = state.refused_bytes.saturating_sub(page_size);
            }
            let request = fill_request(&uid, &version, page_number);
            let version_metadata =
                page_metadata(object.content_type, version.clone(), object.length);
            let directory = directory.clone();
            let clock = self.clock.clone();
            let work_version = version.clone();
            let work_membership = membership.clone();
            let metadata_bytes = object.metadata;
            let future = Box::pin(async move {
                let metadata = OpaqueMetadata::from_header(&metadata_bytes)?;
                let context = OriginContext {
                    object: work_version.object.clone(),
                    metadata: Some(metadata),
                    authorization: None,
                };
                let scope = RequestScope::with_clock(request, clock.now() + FETCH_TIMEOUT, clock)?;
                directory
                    .prefetch_page(
                        page.clone(),
                        version_metadata,
                        work_membership,
                        &context,
                        &scope,
                    )
                    .await
            });
            self.inflight.push_back(Inflight {
                fill: uid,
                generation: state.generation,
                object: object_index,
                page: page_number,
                attempt,
                epoch,
                accounted: page_size,
                version,
                future,
            });
            state.fetching = true;
            self.finish_remaining = self.inflight.len();
            cx.waker().wake_by_ref();
        }
        if self.launch_remaining > 0 {
            // A fill may have disappeared during a multi-turn sweep. Clamp to
            // the current map size rather than retaining impossible work.
            self.launch_remaining = self.launch_remaining.min(self.local.len());
            if self.launch_remaining > 0 {
                cx.waker().wake_by_ref();
            }
        }
        Ok(())
    }

    /// Record finished fetches. Failure releases the page reservation and queues
    /// it for retry without moving the manifest cursor backward.
    fn finish(&mut self, cx: &mut Context<'_>) {
        let retry_seed = fill_id(&self.coordinator.local_node().0).0 ^ u64::from(self.worker.0);
        if self.finish_remaining == 0 {
            self.finish_remaining = self.inflight.len();
        }
        self.finish_remaining = self.finish_remaining.min(self.inflight.len());
        let mut remaining = self.finish_remaining.min(SCAN_BUDGET);
        while remaining > 0 && !self.inflight.is_empty() {
            remaining -= 1;
            self.finish_remaining -= 1;
            let mut work = self.inflight.pop_front().expect("nonempty queue");
            let ready = Pin::new(&mut work.future).poll(cx);
            match ready {
                Poll::Pending => self.inflight.push_back(work),
                Poll::Ready(result) => {
                    let Some(state) = self.local.get_mut(&work.fill) else {
                        continue;
                    };
                    if state.generation != work.generation {
                        continue;
                    }
                    let page = PageId {
                        version: work.version.clone(),
                        number: PageNumber(work.page),
                    };
                    if state.pins.get(&page) != Some(&work.epoch) {
                        continue;
                    }
                    state.fetching = false;
                    match result {
                        Ok(PageResult::Resident | PageResult::Admitted) => {
                            let id = PageId {
                                version: work.version,
                                number: PageNumber(work.page),
                            };
                            if state.resident.insert(id) {
                                state.pinned_pages = state.pinned_pages.saturating_add(1);
                                state.pinned_bytes =
                                    state.pinned_bytes.saturating_add(work.accounted);
                            }
                        }
                        failure => {
                            let retry_after = match failure {
                                Ok(PageResult::Deferred { retry_after_ms }) => {
                                    Some(Duration::from_millis(u64::from(retry_after_ms)))
                                }
                                _ => None,
                            };
                            let id = PageId {
                                version: work.version.clone(),
                                number: PageNumber(work.page),
                            };
                            if state.resident.remove(&id) {
                                state.pinned_pages = state.pinned_pages.saturating_sub(1);
                                state.pinned_bytes =
                                    state.pinned_bytes.saturating_sub(work.accounted);
                            }
                            if let Some(epoch) = state.pins.remove(&id) {
                                let pages: HoldPages =
                                    std::iter::once(PageNumber(work.page)).collect();
                                self.index.release_pin_pages(
                                    fill_id(&work.fill),
                                    &work.version,
                                    &pages,
                                    epoch,
                                );
                            }
                            state.retry(
                                fill_id(&work.fill).0 ^ retry_seed,
                                self.clock.now(),
                                work.object,
                                work.page,
                                work.attempt,
                                retry_after,
                            );
                        }
                    }
                }
            }
        }
        if self.finish_remaining > 0 {
            cx.waker().wake_by_ref();
        }
    }

    /// Extend live pins and revisit only pages whose protection or readability
    /// was lost. Healthy pages keep their original reservation epochs.
    fn renew(&mut self, now: Instant) {
        let Ok(page_size) = self.coordinator.page_size() else {
            return;
        };
        for (uid, state) in &mut self.local {
            let id = fill_id(uid);
            let mut failed = BTreeSet::new();
            for version in &state.versions {
                // Unavailable means this fill's pin expired or disappeared.
                // A temporary capacity refusal must not release a live pin.
                if matches!(
                    self.index
                        .renew_pin(id, version, self.clock.wall_now() + PIN_TTL),
                    Err(Error::Unavailable)
                ) {
                    failed.insert(version.clone());
                }
            }
            let lost: BTreeSet<_> = state
                .resident
                .iter()
                .filter(|page| {
                    failed.contains(&page.version)
                        || matches!(self.index.lookup(page), Ok(None))
                        || !self.index.is_pinned(page)
                })
                .cloned()
                .collect();
            if !lost.is_empty() {
                forget_inflight(&mut self.inflight, uid);
                state.reconcile(&self.index, uid, page_size.get(), now, |page| {
                    !lost.contains(page)
                });
            }
        }
    }

    /// Publish this node's progress from the control worker.
    fn publish(&mut self, now: Instant) {
        if self.worker != WorkerId(0) || now < self.next_report {
            return;
        }
        self.next_report = now + REPORT;
        for (uid, state) in &self.local {
            self.share.set(self.worker, uid, state.progress());
        }
        let (progress, pinnable) = self.share.report();
        (self.report)(progress, Some(pinnable));
    }
}

impl Drop for FillController {
    fn drop(&mut self) {
        let fills: Vec<String> = self.local.keys().cloned().collect();
        for fill in fills {
            self.forget(&fill);
        }
    }
}

/// Drop every in-flight fetch launched for `fill`.
fn forget_inflight(inflight: &mut VecDeque<Inflight>, fill: &str) {
    inflight.retain(|work| work.fill != fill);
}

/// Immutable descriptor handed to the prefetch path for one page of an object.
///
/// The dispatcher compares this against the metadata the store fetched for the
/// whole object, so `length` is the object's full length. Fetching one page
/// does not narrow it: the page number selects the byte range.
fn page_metadata(
    content_type: Option<ContentType>,
    version: ObjectVersion,
    object_length: u64,
) -> VersionMetadata {
    VersionMetadata {
        content_type,
        version,
        length: object_length,
    }
}

/// Stable local identity for a fill UID.
fn fill_id(uid: &str) -> FillId {
    let mut hash = 0xcbf2_9ce4_8422_2325u64;
    for byte in uid.as_bytes() {
        hash ^= u64::from(*byte);
        hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
    }
    FillId(hash)
}

/// Equal jitter in [ceiling / 2, ceiling], with a bounded exponential ceiling.
/// Mix node, worker, fill, page, and attempt identities so replicas and unrelated
/// failures do not hammer an origin together. This jitter needs no secret or OS
/// entropy, and remains reproducible under simulated time.
fn retry_delay(seed: u64, attempt: u32) -> Duration {
    let ceiling = RETRY_MIN
        .saturating_mul(1u32 << attempt.saturating_sub(1).min(16))
        .min(RETRY_MAX);
    let mut random = seed.wrapping_add(u64::from(attempt).wrapping_mul(0x9e37_79b9_7f4a_7c15));
    random = (random ^ (random >> 30)).wrapping_mul(0xbf58_476d_1ce4_e5b9);
    random = (random ^ (random >> 27)).wrapping_mul(0x94d0_49bb_1331_11eb);
    random ^= random >> 31;
    let half = ceiling / 2;
    half + Duration::from_millis(random % (half.as_millis() as u64 + 1))
}

/// Request identity for one fill page. Deterministic so retries reuse it.
fn fill_request(uid: &str, version: &ObjectVersion, page: u64) -> RequestId {
    let mut hash = 0xcbf2_9ce4_8422_2325u64;
    let mut mix = |bytes: &[u8]| {
        for byte in bytes {
            hash ^= u64::from(*byte);
            hash = hash.wrapping_mul(0x0000_0100_0000_01b3);
        }
    };
    mix(uid.as_bytes());
    mix(version.object.cache.0.as_bytes());
    mix(&version.object.key.0);
    mix(version.etag.as_str().as_bytes());
    mix(&page.to_le_bytes());
    let mut id = [0u8; 16];
    id[..8].copy_from_slice(&fill_id(uid).0.to_le_bytes());
    id[8..].copy_from_slice(&hash.to_le_bytes());
    RequestId(id)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{CacheKey, StrongEtag};

    /// Waker that records whether the scan asked for another turn.
    struct CountWake(std::sync::atomic::AtomicBool);

    impl std::task::Wake for CountWake {
        fn wake(self: Arc<Self>) {
            self.0.store(true, std::sync::atomic::Ordering::SeqCst);
        }

        fn wake_by_ref(self: &Arc<Self>) {
            self.0.store(true, std::sync::atomic::Ordering::SeqCst);
        }
    }

    fn fill_object(byte: u8, length: u64) -> FillObject {
        FillObject {
            key: CacheKey([byte; 32]),
            etag: StrongEtag::test_value("v1"),
            length,
            content_type: None,
            metadata: Vec::new(),
        }
    }

    fn local(objects: Vec<FillObject>) -> LocalFill {
        let digest = "aa".repeat(32);
        LocalFill {
            paused: false,
            generation: 1,
            digest: digest.clone(),
            membership: 1,
            manifest: Arc::new(Manifest {
                digest,
                logical_bytes: objects.iter().map(|object| object.length).sum(),
                objects,
                listed_at: 0,
            }),
            cache: CacheId("cache".to_owned()),
            replicas: 1,
            fetching: false,
            object: 0,
            page: 0,
            pins: BTreeMap::new(),
            resident: BTreeSet::new(),
            counted: BTreeSet::new(),
            refused: BTreeSet::new(),
            versions: BTreeSet::new(),
            owned_pages: 0,
            pinned_pages: 0,
            owned_bytes: 0,
            pinned_bytes: 0,
            refused_bytes: 0,
            retries: BTreeMap::new(),
        }
    }

    /// Real placement, worker directory, and pin index; tests script only the
    /// prefetch completion so reservation and controller transitions stay real.
    fn controller(env: &racer_io::Env, pages: u64) -> (FillController, impl Sized) {
        let (coordinator, graph) = crate::read::tests::admission_coordinator(env);
        let cache = CacheId("33333333-3333-4333-8333-333333333333".into());
        let keys = Rc::new(crate::test_support::security::keys_for(
            env,
            std::slice::from_ref(&cache),
        ));
        let index = Rc::new(Index::new(
            WorkerId(0),
            16,
            crate::control::for_caches(env, keys, vec![cache.clone()]),
            env.ctx.clock.clone(),
        ));
        index.set_page_capacity(16).unwrap();
        let segments = Rc::new(page_alloc::Segments::new(1024));
        segments
            .configure(8192, 8, page_alloc::Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        let _clock = crate::store::catalog::SegmentClock::new(index.clone(), segments, 1);
        let now = env.ctx.clock.now();
        let mut state = local(vec![fill_object(1, pages * crate::model::PAGE_BYTES)]);
        state.cache = cache;
        let controller = FillController {
            worker: WorkerId(0),
            fills: Arc::new(Vec::new),
            report: Arc::new(|_, _| {}),
            share: Arc::new(FillShare::new()),
            index,
            coordinator,
            clock: env.ctx.clock.clone(),
            local: BTreeMap::from([("fill".into(), state)]),
            inflight: VecDeque::new(),
            launch_after: None,
            launch_remaining: 0,
            finish_remaining: 0,
            next_refresh: now + REFRESH,
            next_report: now + REPORT,
            next_renew: now + RENEW,
        };
        (controller, graph)
    }

    fn complete(controller: &mut FillController, result: Result<PageResult>) {
        assert_eq!(controller.inflight.len(), 1);
        controller.inflight[0].future = Box::pin(std::future::ready(result));
        controller.finish(&mut Context::from_waker(futures::task::noop_waker_ref()));
    }

    #[test]
    fn failed_page_releases_reservation_and_later_pages_fill_before_retry_recovers() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 2);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let now = controller.clock.now();
        controller.launch(&mut cx, now).unwrap();
        assert_eq!(controller.inflight[0].page, 0);
        let page = PageId {
            version: controller.inflight[0].version.clone(),
            number: PageNumber(0),
        };
        assert!(controller.index.is_pinned(&page), "reserve before fetching");
        complete(&mut controller, Err(Error::Io));
        assert!(!controller.index.is_pinned(&page));
        assert_eq!(controller.local["fill"].pinned_pages, 0);
        assert_eq!(controller.local["fill"].refused_bytes, 0);
        controller.launch(&mut cx, now).unwrap();
        assert_eq!(
            controller.inflight[0].page, 1,
            "failed page must not block scan"
        );
        complete(&mut controller, Ok(PageResult::Admitted));
        controller.launch(&mut cx, now).unwrap();
        assert!(controller.inflight.is_empty(), "retry must respect backoff");
        let retry = controller.local["fill"]
            .retries
            .first_key_value()
            .unwrap()
            .0
            .0;
        controller.launch(&mut cx, retry).unwrap();
        assert_eq!(controller.inflight[0].page, 0);
        complete(&mut controller, Ok(PageResult::Resident));
        let state = &controller.local["fill"];
        assert_eq!(state.owned_pages, 2, "retry cannot count ownership twice");
        assert_eq!(state.pinned_pages, 2);
        assert_eq!(state.pinned_bytes, 2 * crate::model::PAGE_BYTES);
        assert!(state.retries.is_empty());
        assert_eq!(
            (state.object, state.page),
            (1, 0),
            "retry completion cannot rewind an exhausted cursor"
        );
        controller.forget("fill");
        assert!(!controller.index.is_pinned(&page));
    }

    #[test]
    fn real_prefetch_failure_and_fill_cancellation_release_their_exact_reservations() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 2);
        let mut objects = controller.local["fill"].manifest.objects.clone();
        objects[0].metadata = b"invalid\r\nmetadata".to_vec();
        let cache = controller.local["fill"].cache.clone();
        let mut state = local(objects);
        state.cache = cache;
        controller.local.insert("fill".into(), state);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        controller.poll(&mut cx).unwrap();
        let first = PageId {
            version: controller.inflight[0].version.clone(),
            number: PageNumber(0),
        };
        assert!(controller.index.is_pinned(&first));
        // Poll the actual future. Invalid metadata fails before dispatch, and
        // the next manifest page launches without waiting for the failed one.
        controller.poll(&mut cx).unwrap();
        assert!(!controller.index.is_pinned(&first));
        assert_eq!(controller.inflight[0].page, 1);
        assert_eq!(controller.local["fill"].retries.len(), 1);
        assert_eq!(controller.local["fill"].pinned_pages, 0);
        let second = PageId {
            number: PageNumber(1),
            ..first
        };
        assert!(controller.index.is_pinned(&second));
        controller.forget("fill");
        assert!(controller.inflight.is_empty());
        assert!(!controller.index.is_pinned(&second));
        controller.finish(&mut cx);
        assert!(controller.local.is_empty());
    }

    #[test]
    fn reservation_refusals_are_deduplicated_and_clear_on_recovery() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 1);
        controller.index.set_pin_capacity_percent(0).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        controller.launch(&mut cx, controller.clock.now()).unwrap();
        for _ in 0..3 {
            assert!(controller.inflight.is_empty());
            let retry = controller.local["fill"]
                .retries
                .first_key_value()
                .unwrap()
                .0
                .0;
            controller.launch(&mut cx, retry).unwrap();
            assert_eq!(
                controller.local["fill"].refused_bytes,
                crate::model::PAGE_BYTES
            );
            assert_eq!(controller.local["fill"].owned_pages, 1);
        }
        controller.index.set_pin_capacity_percent(50).unwrap();
        let retry = controller.local["fill"]
            .retries
            .first_key_value()
            .unwrap()
            .0
            .0;
        controller.launch(&mut cx, retry).unwrap();
        assert_eq!(controller.local["fill"].refused_bytes, 0);
        complete(&mut controller, Ok(PageResult::Admitted));
        assert_eq!(controller.local["fill"].pinned_pages, 1);
    }

    #[test]
    fn unexpected_reservation_errors_do_not_fail_the_worker_or_lose_the_page() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 1);
        let cache = controller.local["fill"].cache.clone();
        let keys = Rc::new(crate::test_support::security::keys_for(
            &env,
            std::slice::from_ref(&cache),
        ));
        // A live cache with no segment binding produces InvalidConfiguration,
        // not Overloaded or MissingKey, in the real reservation implementation.
        controller.index = Rc::new(Index::new(
            WorkerId(0),
            16,
            crate::control::for_caches(&env, keys, vec![cache]),
            env.ctx.clock.clone(),
        ));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        controller.poll(&mut cx).unwrap();
        assert!(controller.inflight.is_empty());
        assert_eq!(controller.local["fill"].retries.len(), 1);
        assert_eq!(controller.local["fill"].refused_bytes, 0);
        assert!(controller.local["fill"].pins.is_empty());
        let segments = Rc::new(page_alloc::Segments::new(1024));
        segments
            .configure(8192, 8, page_alloc::Alignment::new(512, 512, 512).unwrap())
            .unwrap();
        let _clock =
            crate::store::catalog::SegmentClock::new(controller.index.clone(), segments, 1);
        let retry = controller.local["fill"]
            .retries
            .first_key_value()
            .unwrap()
            .0
            .0;
        controller.launch(&mut cx, retry).unwrap();
        complete(&mut controller, Ok(PageResult::Admitted));
        assert_eq!(controller.local["fill"].owned_pages, 1);
        assert_eq!(controller.local["fill"].pinned_pages, 1);
    }

    #[test]
    fn missing_key_clears_an_earlier_capacity_refusal_without_crediting_progress() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 1);
        controller.index.set_pin_capacity_percent(0).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        controller.launch(&mut cx, controller.clock.now()).unwrap();
        assert_eq!(
            controller.local["fill"].refused_bytes,
            crate::model::PAGE_BYTES
        );
        let keys = Rc::new(crate::test_support::security::keys_for(&env, &[]));
        controller.index = Rc::new(Index::new(
            WorkerId(0),
            16,
            crate::control::for_caches(&env, keys, vec![]),
            env.ctx.clock.clone(),
        ));
        let retry = controller.local["fill"]
            .retries
            .first_key_value()
            .unwrap()
            .0
            .0;
        controller.launch(&mut cx, retry).unwrap();
        assert!(controller.inflight.is_empty());
        assert_eq!(controller.local["fill"].refused_bytes, 0);
        assert_eq!(controller.local["fill"].pinned_pages, 0);
        assert_eq!(controller.local["fill"].retries.len(), 1);
    }

    #[test]
    fn repeated_page_failures_back_off_exponentially_with_a_cap_and_then_recover() {
        let clock = racer_io::SimClock::new(1187);
        let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
        let (mut controller, _graph) = controller(&env, 1);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        controller.launch(&mut cx, controller.clock.now()).unwrap();
        for attempt in 1..=12 {
            let now = controller.clock.now();
            let failure = if attempt % 2 == 0 {
                Ok(PageResult::Failed(
                    racer_core::prefetch::FailCode::OriginError,
                ))
            } else {
                Err(Error::Io)
            };
            complete(&mut controller, failure);
            let state = &controller.local["fill"];
            let (key, failures) = state.retries.first_key_value().unwrap();
            assert_eq!(*failures, attempt);
            let delay = key.0.duration_since(now);
            let ceiling = RETRY_MIN.saturating_mul(1 << (attempt - 1)).min(RETRY_MAX);
            assert!(
                (ceiling / 2..=ceiling).contains(&delay),
                "attempt {attempt}: {delay:?}"
            );
            assert_eq!(state.pinned_pages, 0);
            assert_eq!(state.refused_bytes, 0);
            assert!(state.pins.is_empty());
            controller.launch(&mut cx, now).unwrap();
            assert!(controller.inflight.is_empty());
            clock.advance(delay - Duration::from_millis(1));
            controller.launch(&mut cx, controller.clock.now()).unwrap();
            assert!(controller.inflight.is_empty());
            clock.advance(Duration::from_millis(1));
            controller.launch(&mut cx, controller.clock.now()).unwrap();
            assert_eq!(controller.inflight.len(), 1);
        }
        complete(&mut controller, Ok(PageResult::Admitted));
        assert_eq!(controller.local["fill"].owned_pages, 1);
        assert_eq!(controller.local["fill"].pinned_pages, 1);
        assert!(controller.local["fill"].retries.is_empty());
    }

    #[test]
    fn deferred_owner_hint_is_honored_and_pages_have_distinct_retry_deadlines() {
        let clock = racer_io::SimClock::new(1188);
        let env = racer_io::Env::new(racer_io::Driver::Uring, clock.ctx(0));
        let (mut controller, _graph) = controller(&env, 3);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let now = controller.clock.now();
        for _ in 0..2 {
            controller.launch(&mut cx, now).unwrap();
            complete(&mut controller, Err(Error::Io));
        }
        let deadlines: Vec<_> = controller.local["fill"]
            .retries
            .keys()
            .map(|key| key.0)
            .collect();
        assert_eq!(deadlines.len(), 2);
        assert_ne!(
            deadlines[0], deadlines[1],
            "page jitter must spread retries"
        );
        controller.launch(&mut cx, now).unwrap();
        complete(
            &mut controller,
            Ok(PageResult::Deferred {
                retry_after_ms: 600_000,
            }),
        );
        let (key, _) = controller.local["fill"].retries.last_key_value().unwrap();
        assert_eq!(key.0.duration_since(now), Duration::from_secs(600));
        assert_eq!(key.2, 2);
        assert_eq!(
            controller.local["fill"].refused_bytes, 0,
            "owner deferral is not a pin-capacity refusal"
        );
        assert!(controller.local["fill"].pins.is_empty());
    }

    #[test]
    fn stale_completion_cannot_credit_or_release_a_replacement_reservation() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 1);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let now = controller.clock.now();
        controller.launch(&mut cx, now).unwrap();
        let mut stale = controller.inflight.pop_back().unwrap();
        let replacement = controller.local["fill"].manifest.objects.clone();
        let cache = controller.local["fill"].cache.clone();
        controller.forget("fill");
        let mut state = local(replacement);
        state.cache = cache;
        controller.local.insert("fill".into(), state);
        controller.launch(&mut cx, now).unwrap();
        let page = PageId {
            version: stale.version.clone(),
            number: PageNumber(0),
        };
        assert_ne!(stale.epoch, controller.inflight[0].epoch);
        controller.inflight[0].future = Box::pin(std::future::pending());
        stale.future = Box::pin(std::future::ready(Ok(PageResult::Admitted)));
        controller.inflight.push_back(stale);
        controller.finish_remaining = controller.inflight.len();
        controller.finish(&mut cx);
        assert_eq!(controller.local["fill"].pinned_pages, 0);
        assert!(controller.index.is_pinned(&page));
        assert_eq!(controller.inflight.len(), 1);
        complete(&mut controller, Ok(PageResult::Admitted));
        assert_eq!(controller.local["fill"].pinned_pages, 1);
    }

    #[test]
    fn controller_scan_budget_is_shared_and_rotates_between_fills() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, (2 * SCAN_BUDGET) as u64);
        // The real directory assigns every page to worker 0. A nonowning
        // worker must still charge the manifest walk against its shared budget.
        controller.worker = WorkerId(1);
        let first = controller.local.remove("fill").unwrap();
        let mut second = local(first.manifest.objects.clone());
        second.cache = first.cache.clone();
        controller.local.insert("a".into(), first);
        controller.local.insert("b".into(), second);
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let now = controller.clock.now();
        controller.launch(&mut cx, now).unwrap();
        assert_eq!(controller.local["a"].page, (SCAN_BUDGET - 1) as u64);
        assert_eq!(controller.local["b"].page, 0);
        assert!(wake.0.load(std::sync::atomic::Ordering::SeqCst));
        controller.launch(&mut cx, now).unwrap();
        assert_eq!(controller.local["a"].page, (SCAN_BUDGET - 1) as u64);
        assert_eq!(controller.local["b"].page, (SCAN_BUDGET - 1) as u64);
        assert!(controller.inflight.is_empty());
    }

    #[test]
    fn controller_fill_visits_are_bounded_without_an_endless_self_wake() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 0);
        controller.forget("fill");
        for number in 0..2 * SCAN_BUDGET {
            controller
                .local
                .insert(format!("{number:06}"), local(Vec::new()));
        }
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let now = controller.clock.now();
        controller.launch(&mut cx, now).unwrap();
        assert_eq!(controller.launch_remaining, SCAN_BUDGET);
        assert!(wake.0.swap(false, std::sync::atomic::Ordering::SeqCst));
        controller.launch(&mut cx, now).unwrap();
        assert_eq!(controller.launch_remaining, 0);
        assert!(!wake.0.load(std::sync::atomic::Ordering::SeqCst));
    }

    #[test]
    fn controller_completion_polling_is_bounded_and_fair() {
        let env = racer_io::Env::host();
        let (mut controller, _graph) = controller(&env, 1);
        let calls = Rc::new(std::cell::RefCell::new(Vec::new()));
        let version = ObjectVersion {
            object: ObjectId {
                cache: controller.local["fill"].cache.clone(),
                key: CacheKey([1; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        };
        for number in 0..2 * SCAN_BUDGET + 3 {
            let calls = calls.clone();
            controller.inflight.push_back(Inflight {
                fill: number.to_string(),
                generation: 1,
                object: 0,
                page: 0,
                attempt: 0,
                epoch: PinEpoch::default(),
                accounted: crate::model::PAGE_BYTES,
                version: version.clone(),
                future: Box::pin(futures::future::poll_fn(move |_| {
                    calls.borrow_mut().push(number);
                    Poll::Pending
                })),
            });
        }
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        for turn in 1..=3 {
            let before = calls.borrow().len();
            controller.finish(&mut cx);
            assert!(calls.borrow().len() - before <= SCAN_BUDGET);
            assert_eq!(
                wake.0.swap(false, std::sync::atomic::Ordering::SeqCst),
                turn < 3
            );
        }
        assert_eq!(
            *calls.borrow(),
            (0..2 * SCAN_BUDGET + 3).collect::<Vec<_>>()
        );
    }

    struct Fixture {
        controller: FillController,
        views: Arc<Mutex<Vec<racer_authority::FillView>>>,
        segments: Rc<page_alloc::Segments>,
        _worker: crate::test_support::ReadWorker,
    }

    fn fixture(env: &racer_io::Env) -> Fixture {
        let mut state = local(vec![fill_object(1, 2 * crate::model::PAGE_BYTES)]);
        state.cache = CacheId(crate::test_support::security::CACHE.into());
        let metadata = page_metadata(
            None,
            ObjectVersion {
                object: ObjectId {
                    cache: state.cache.clone(),
                    key: state.manifest.objects[0].key,
                },
                etag: state.manifest.objects[0].etag.clone(),
            },
            2 * crate::model::PAGE_BYTES,
        )
        .for_pin();
        let admission = Rc::new(flow_control::Quotas::new(
            crate::admission::AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ),
        ));
        let reactor = Rc::new(crate::runtime::Reactor::new(admission.clone(), env));
        let delivery = Rc::new(crate::http::Delivery::new(
            reactor.clone(),
            Duration::from_secs(2),
        ));
        let worker = crate::test_support::ReadWorker::new(
            env,
            racer_wire::control::CacheDefinition {
                id: state.cache.clone(),
                name: "cache".into(),
                client_socket: "/run/racer/cache/client/socket".into(),
                origin_socket: "/run/racer/cache/origin/socket".into(),
                priority: 1,
                mode: racer_wire::control::CacheMode::Cached,
                origin: None,
            },
            metadata,
            admission,
            reactor,
            delivery,
            1,
        );
        let index = Rc::new(Index::new(
            WorkerId(0),
            16,
            crate::test_support::availability(env),
            env.ctx.clock.clone(),
        ));
        index.set_page_capacity(16).unwrap();
        index.set_pin_capacity_percent(100).unwrap();
        let segments = Rc::new(page_alloc::Segments::new(4096));
        segments
            .configure(
                4096 * 8,
                8,
                page_alloc::Alignment::new(512, 512, 512).unwrap(),
            )
            .unwrap();
        let _clock = crate::store::catalog::SegmentClock::new(index.clone(), segments.clone(), 1);
        let mut view = racer_authority::FillView::from_json(&serde_json::json!({
            "metadata": {"uid": "11111111-1111-4111-8111-111111111111", "name": "fill", "generation": 1},
            "spec": {"cacheName": "cache", "prefixes": ["/"], "maxFootprint": 100000000}
        })).unwrap();
        view.cache_uid = state.cache.0.clone();
        view.replicas = 1;
        view.manifest = Some(state.manifest);
        view.membership = Some(worker.membership.version.0);
        view.accepted = true;
        let views = Arc::new(Mutex::new(vec![view]));
        let controller = FillController {
            worker: WorkerId(0),
            fills: Arc::new({
                let views = views.clone();
                move || views.lock().unwrap().clone()
            }),
            report: Arc::new(|_, _| {}),
            share: Arc::new(FillShare::new()),
            index,
            coordinator: worker.coordinator.clone(),
            clock: env.ctx.clock.clone(),
            local: BTreeMap::new(),
            inflight: VecDeque::new(),
            launch_after: None,
            launch_remaining: 0,
            finish_remaining: 0,
            next_refresh: env.ctx.clock.now(),
            next_report: env.ctx.clock.now() + REPORT,
            next_renew: env.ctx.clock.now() + RENEW,
        };
        Fixture {
            controller,
            views,
            segments,
            _worker: worker,
        }
    }

    impl Fixture {
        fn uid(&self) -> String {
            self.views.lock().unwrap()[0].uid.clone()
        }

        fn adopt(&mut self) {
            self.controller.refresh(self.controller.clock.now());
        }

        /// Install a real readable disk-index entry and one completed fill pin.
        fn complete(&mut self, number: u64) -> (PageId, PinEpoch) {
            let uid = self.uid();
            let state = self.controller.local.get_mut(&uid).unwrap();
            let object = &state.manifest.objects[0];
            let page = PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: state.cache.clone(),
                        key: object.key,
                    },
                    etag: object.etag.clone(),
                },
                number: PageNumber(number),
            };
            let epoch = self
                .controller
                .index
                .reserve_pin_pages(
                    fill_id(&uid),
                    &page.version,
                    &std::iter::once(page.number).collect(),
                    self.controller.clock.wall_now() + PIN_TTL,
                )
                .unwrap();
            let (lease, extent) = self.segments.append_in(512, true).unwrap();
            let location = crate::store::catalog::RecordLocation {
                segment: lease.id(),
                generation: lease.generation(),
                extent,
            };
            self.controller
                .index
                .publish(
                    page.clone(),
                    crate::store::catalog::IndexedPage {
                        page_size: racer_wire::object::PageSize::DEFAULT,
                        metadata: page_metadata(None, page.version.clone(), object.length),
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        location,
                    },
                )
                .unwrap();
            drop(lease);
            state.pins.insert(page.clone(), epoch);
            state.resident.insert(page.clone());
            state.counted.insert(page.clone());
            state.versions.insert(page.version.clone());
            state.pinned_pages += 1;
            state.owned_pages += 1;
            state.pinned_bytes += crate::model::PAGE_BYTES;
            state.owned_bytes += crate::model::PAGE_BYTES;
            (page, epoch)
        }
    }

    #[test]
    fn controller_plan_changes_preserve_owned_pin_epochs() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        f.adopt();
        let uid = f.uid();
        let (page, epoch) = f.complete(0);
        let (removed, _) = f.complete(1);
        // A max-footprint-only edit changes generation, not desired pages.
        {
            let mut views = f.views.lock().unwrap();
            views[0].generation += 1;
            views[0].max_footprint += 1;
        }
        f.adopt();
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
        assert!(f.controller.index.is_pinned(&removed));
        // A new manifest retains one page and removes the other.
        {
            let mut views = f.views.lock().unwrap();
            let manifest = Arc::make_mut(views[0].manifest.as_mut().unwrap());
            manifest.digest = "bb".repeat(32);
            manifest.objects[0].length = crate::model::PAGE_BYTES;
        }
        f.adopt();
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
        assert!(!f.controller.index.is_pinned(&removed));
        // A cluster-membership revision with unchanged placement keeps pins.
        f.controller.local.get_mut(&uid).unwrap().membership = 0;
        f.adopt();
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
        assert_eq!(f.controller.local[&uid].pinned_pages, 1);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        f.controller.launch(&mut cx, env.ctx.clock.now()).unwrap();
        assert!(
            f.controller.inflight.is_empty(),
            "retained pages must not reserve another holder"
        );
        f.controller.forget(&uid);
        assert!(
            !f.controller.index.is_pinned(&page),
            "one release must remove the sole holder"
        );
    }

    #[test]
    fn controller_replacement_cancels_same_generation_inflight() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        {
            let mut views = f.views.lock().unwrap();
            Arc::make_mut(views[0].manifest.as_mut().unwrap())
                .objects
                .push(fill_object(2, crate::model::PAGE_BYTES));
        }
        f.adopt();
        let uid = f.uid();
        let (page, epoch) = f.complete(0);
        let abandoned = PageId {
            version: page.version.clone(),
            number: PageNumber(1),
        };
        let reservation = f
            .controller
            .index
            .reserve_pin_pages(
                fill_id(&uid),
                &abandoned.version,
                &std::iter::once(abandoned.number).collect(),
                env.ctx.clock.wall_now() + PIN_TTL,
            )
            .unwrap();
        f.controller
            .local
            .get_mut(&uid)
            .unwrap()
            .pins
            .insert(abandoned.clone(), reservation);
        let state = f.controller.local.get_mut(&uid).unwrap();
        state.fetching = true;
        state.retries.insert((env.ctx.clock.now(), 1, 0), 2);
        f.controller.inflight.push_back(Inflight {
            fill: uid.clone(),
            generation: 1,
            object: 0,
            page: 1,
            attempt: 0,
            epoch: reservation,
            accounted: crate::model::PAGE_BYTES,
            version: page.version.clone(),
            future: Box::pin(async { Ok(PageResult::Admitted) }),
        });
        {
            let mut views = f.views.lock().unwrap();
            let manifest = Arc::make_mut(views[0].manifest.as_mut().unwrap());
            manifest.digest = "cc".repeat(32);
            manifest.objects.truncate(1);
        }
        f.adopt();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        f.controller.finish(&mut cx);
        assert!(f.controller.inflight.is_empty());
        assert_eq!(f.controller.local[&uid].page, 0);
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
        assert_eq!(f.controller.local[&uid].pinned_pages, 1);
        assert!(
            f.controller.local[&uid].retries.is_empty(),
            "replacement must discard old object indexes"
        );
        assert!(!f.controller.local[&uid].fetching);
        assert!(
            !f.controller.index.is_pinned(&abandoned),
            "canceled fetch reservations must be swept"
        );
        f.controller.launch(&mut cx, env.ctx.clock.now()).unwrap();
        assert_eq!(f.controller.inflight.len(), 1);
        assert_eq!(f.controller.inflight[0].object, 0);
        assert_eq!(f.controller.inflight[0].page, 1);
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
    }

    #[test]
    fn renewal_repair_clears_retries_and_restarts_canceled_fetch_without_unpinning_healthy_pages() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        {
            let mut views = f.views.lock().unwrap();
            Arc::make_mut(views[0].manifest.as_mut().unwrap())
                .objects
                .push(fill_object(2, crate::model::PAGE_BYTES));
        }
        f.adopt();
        let uid = f.uid();
        let (healthy, epoch) = f.complete(0);
        let (missing, _) = f.complete(1);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        f.controller.launch(&mut cx, env.ctx.clock.now()).unwrap();
        assert_eq!(f.controller.inflight.len(), 1);
        let work = &f.controller.inflight[0];
        assert_eq!(work.object, 1);
        let canceled = PageId {
            version: work.version.clone(),
            number: PageNumber(work.page),
        };
        f.controller
            .local
            .get_mut(&uid)
            .unwrap()
            .retries
            .insert((env.ctx.clock.now(), 0, 1), 3);
        let location = f
            .controller
            .index
            .lookup(&missing)
            .unwrap()
            .unwrap()
            .location;
        f.controller.index.remove_if_matches(&missing, &location);
        f.controller.renew(env.ctx.clock.now());
        let state = &f.controller.local[&uid];
        assert!(!state.fetching);
        assert!(state.retries.is_empty());
        assert_eq!((state.object, state.page), (0, 0));
        assert_eq!(state.pins[&healthy], epoch);
        assert!(f.controller.inflight.is_empty());
        assert!(!f.controller.index.is_pinned(&missing));
        assert!(!f.controller.index.is_pinned(&canceled));
        f.controller.launch(&mut cx, env.ctx.clock.now()).unwrap();
        assert_eq!(f.controller.inflight.len(), 1);
        assert_eq!(f.controller.inflight[0].object, 0);
        assert_eq!(f.controller.inflight[0].page, 1);
        assert_eq!(f.controller.local[&uid].pins[&healthy], epoch);
        assert!(f.controller.local[&uid].fetching);
    }

    #[test]
    fn controller_waits_for_membership_without_unpinning_then_releases_rejected_or_deleted() {
        for rejected in [false, true] {
            let env = racer_io::Env::host();
            let mut f = fixture(&env);
            f.adopt();
            let uid = f.uid();
            let (page, _) = f.complete(0);
            f.views.lock().unwrap()[0].membership = Some(2);
            f.adopt();
            assert!(f.controller.index.is_pinned(&page));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            f.controller.launch(&mut cx, env.ctx.clock.now()).unwrap();
            assert!(
                f.controller.inflight.is_empty(),
                "wait for the authority's membership before fetching"
            );
            if rejected {
                f.views.lock().unwrap()[0].accepted = false;
            } else {
                f.views.lock().unwrap().clear();
            }
            f.adopt();
            assert!(f.controller.local.is_empty());
            assert!(!f.controller.index.is_pinned(&page));
            assert!(f.controller.share.report().0.is_empty());
            assert!(!f.controller.local.contains_key(&uid));
        }
    }

    #[test]
    fn controller_renew_repairs_one_missing_page_without_releasing_healthy_pages() {
        let sim = racer_io::SimClock::new(1187);
        let env = racer_io::Env::new(racer_io::Driver::Uring, sim.ctx(0));
        let mut f = fixture(&env);
        f.adopt();
        let uid = f.uid();
        let (healthy, epoch) = f.complete(0);
        let (missing, _) = f.complete(1);
        let location = f
            .controller
            .index
            .lookup(&missing)
            .unwrap()
            .unwrap()
            .location;
        f.controller.index.remove_if_matches(&missing, &location);
        sim.advance(RENEW);
        f.controller.renew(env.ctx.clock.now());
        assert_eq!(f.controller.local[&uid].pins[&healthy], epoch);
        assert!(f.controller.index.is_pinned(&healthy));
        assert!(!f.controller.index.is_pinned(&missing));
        assert_eq!(f.controller.local[&uid].pinned_pages, 1);
        assert_eq!(
            f.controller.local[&uid].pinned_bytes,
            crate::model::PAGE_BYTES
        );
        assert_eq!(
            f.controller.index.pin_expiry(&healthy),
            Some(env.ctx.clock.wall_now() + PIN_TTL)
        );
        sim.advance(PIN_TTL);
        f.controller.renew(env.ctx.clock.now());
        assert!(f.controller.local[&uid].pins.is_empty());
        assert_eq!(f.controller.local[&uid].pinned_pages, 0);
        assert!(!f.controller.index.is_pinned(&healthy));
    }

    #[test]
    fn reconcile_releases_only_pages_rejected_by_replacement_ownership() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        f.adopt();
        let uid = f.uid();
        let (kept, epoch) = f.complete(0);
        let (moved, _) = f.complete(1);
        f.controller.local.get_mut(&uid).unwrap().reconcile(
            &f.controller.index,
            &uid,
            crate::model::PAGE_BYTES,
            env.ctx.clock.now(),
            |page| page == &kept,
        );
        assert_eq!(f.controller.local[&uid].pins[&kept], epoch);
        assert!(f.controller.index.is_pinned(&kept));
        assert!(!f.controller.index.is_pinned(&moved));
    }

    #[test]
    fn controller_new_object_version_releases_old_pins() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        f.adopt();
        let uid = f.uid();
        let (page, _) = f.complete(0);
        {
            let mut views = f.views.lock().unwrap();
            let manifest = Arc::make_mut(views[0].manifest.as_mut().unwrap());
            manifest.digest = "dd".repeat(32);
            manifest.objects[0].etag = StrongEtag::test_value("v2");
        }
        f.adopt();
        assert!(f.controller.local[&uid].pins.is_empty());
        assert_eq!(f.controller.local[&uid].owned_pages, 0);
        assert!(!f.controller.index.is_pinned(&page));
        assert!(
            f.controller.index.lookup(&page).unwrap().is_some(),
            "release must not delete cached data"
        );
    }

    #[test]
    fn dropping_controller_releases_retained_pins() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        f.adopt();
        let (page, _) = f.complete(0);
        f.views.lock().unwrap()[0].generation += 1;
        f.adopt();
        let index = f.controller.index.clone();
        assert!(index.is_pinned(&page));
        drop(f);
        assert!(!index.is_pinned(&page));
    }

    #[test]
    fn controller_capacity_refusal_does_not_discard_a_live_readable_pin() {
        let env = racer_io::Env::host();
        let mut f = fixture(&env);
        f.controller.index.set_pin_capacity_percent(50).unwrap();
        f.adopt();
        let uid = f.uid();
        let (page, epoch) = f.complete(0);
        // A refused reservation must not invalidate an existing completed pin.
        let more = (1..=8).map(PageNumber).collect();
        assert!(matches!(
            f.controller.index.reserve_pin_pages(
                fill_id(&uid),
                &page.version,
                &more,
                env.ctx.clock.wall_now() + PIN_TTL
            ),
            Err(Error::Overloaded)
        ));
        f.controller.renew(env.ctx.clock.now());
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
        f.views.lock().unwrap()[0].generation += 1;
        f.adopt();
        assert_eq!(f.controller.local[&uid].pins[&page], epoch);
        assert!(f.controller.index.is_pinned(&page));
    }

    #[test]
    fn controller_expired_pin_is_not_hidden_by_another_fills_live_pin() {
        let sim = racer_io::SimClock::new(1188);
        let env = racer_io::Env::new(racer_io::Driver::Uring, sim.ctx(0));
        let mut f = fixture(&env);
        f.adopt();
        let uid = f.uid();
        let (page, _) = f.complete(0);
        sim.advance(PIN_TTL);
        let pages = std::iter::once(page.number).collect();
        let other = f
            .controller
            .index
            .reserve_pin_pages(
                fill_id("other"),
                &page.version,
                &pages,
                env.ctx.clock.wall_now() + PIN_TTL,
            )
            .unwrap();
        assert!(f.controller.index.is_pinned(&page));
        f.views.lock().unwrap()[0].generation += 1;
        f.adopt();
        assert!(f.controller.local[&uid].pins.is_empty());
        assert_eq!(f.controller.local[&uid].pinned_pages, 0);
        assert!(
            f.controller.index.is_pinned(&page),
            "reconciliation must not release another fill"
        );
        f.controller
            .index
            .release_pin_pages(fill_id("other"), &page.version, &pages, other);
        assert!(!f.controller.index.is_pinned(&page));
    }

    #[test]
    fn scan_stops_at_the_page_budget_and_requests_a_wakeup() {
        let pages = (SCAN_BUDGET + 3) as u64;
        let mut state = local(vec![fill_object(1, pages)]);
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut visits = 0;
        let mut budget = SCAN_BUDGET;
        let step = scan_fill(&mut cx, &mut state, 1, &mut budget, |_, _| {
            visits += 1;
            Ok(false)
        })
        .unwrap();
        assert!(matches!(step, ScanStep::Budget));
        assert_eq!(visits, SCAN_BUDGET);
        assert_eq!(state.object, 0);
        assert_eq!(state.page, SCAN_BUDGET as u64);
        assert!(wake.0.load(std::sync::atomic::Ordering::SeqCst));
    }

    #[test]
    fn scan_reaches_an_owned_page_after_budgeted_turns() {
        let owned_page = (SCAN_BUDGET + 4) as u64;
        let mut state = local(vec![fill_object(2, owned_page + 1)]);
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut turns = 0;
        let selected = loop {
            turns += 1;
            assert!(turns < 4, "scan did not converge");
            let mut budget = SCAN_BUDGET;
            let step = scan_fill(&mut cx, &mut state, 1, &mut budget, |object, number| {
                Ok(object.key == CacheKey([2; 32]) && number == owned_page)
            })
            .unwrap();
            match step {
                ScanStep::Budget => assert!(wake.0.load(std::sync::atomic::Ordering::SeqCst)),
                ScanStep::Owned(object, number) => break (object, number),
                ScanStep::Exhausted => panic!("scan passed the owned page"),
            }
            wake.0.store(false, std::sync::atomic::Ordering::SeqCst);
        };
        assert_eq!(turns, 2);
        assert_eq!(selected.0.key, CacheKey([2; 32]));
        assert_eq!(selected.1, owned_page);
        // A failed fetch is queued separately and cannot stall this cursor.
        assert_eq!(state.page, owned_page + 1);
        assert!(!wake.0.load(std::sync::atomic::Ordering::SeqCst));
    }

    #[test]
    fn scan_exhausting_the_manifest_does_not_wake() {
        let mut state = local(vec![fill_object(3, 2)]);
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut budget = SCAN_BUDGET;
        let step = scan_fill(&mut cx, &mut state, 1, &mut budget, |_, _| Ok(false)).unwrap();
        assert!(matches!(step, ScanStep::Exhausted));
        assert_eq!(state.object, 1);
        assert_eq!(state.page, 0);
        assert!(!wake.0.load(std::sync::atomic::Ordering::SeqCst));
    }

    #[test]
    fn forgetting_a_fill_drops_its_inflight_fetches() {
        fn pending() -> Pin<Box<dyn Future<Output = Result<PageResult>>>> {
            Box::pin(std::future::pending())
        }

        let version = ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".to_owned()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        };
        let work = |fill: &str, page: u64| Inflight {
            fill: fill.to_owned(),
            generation: 1,
            object: 0,
            page,
            attempt: 0,
            epoch: PinEpoch::default(),
            accounted: 1024,
            version: version.clone(),
            future: pending(),
        };
        let mut inflight = VecDeque::from([work("a", 0), work("b", 0), work("a", 1)]);
        forget_inflight(&mut inflight, "a");
        assert_eq!(inflight.len(), 1);
        assert_eq!(inflight[0].fill, "b");
    }

    #[test]
    fn page_metadata_carries_the_whole_object_length() {
        let version = ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".to_owned()),
                key: CacheKey([1; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        };
        let page_size = crate::model::PAGE_BYTES;
        let object_length = 3 * page_size;
        let stored = VersionMetadata {
            content_type: None,
            version: version.clone(),
            length: object_length,
        };
        for page in 0..3 {
            let metadata = page_metadata(None, version.clone(), object_length);
            assert!(metadata.compatible(&stored), "page {page}");
        }
        let page_payload = VersionMetadata {
            content_type: None,
            version,
            length: page_size,
        };
        assert!(!page_payload.compatible(&stored));
    }

    #[test]
    fn fill_ids_are_stable_and_distinct() {
        assert_eq!(fill_id("a"), fill_id("a"));
        assert_ne!(fill_id("a"), fill_id("b"));
    }

    #[test]
    fn share_sums_only_matching_workers() {
        let share = FillShare::new();
        let progress = WorkerProgress {
            generation: 1,
            membership: 2,
            digest: "aa".repeat(32),
            owned_pages: 3,
            pinned_pages: 2,
            owned_bytes: 300,
            pinned_bytes: 200,
            refused_bytes: 0,
        };
        share.set(WorkerId(0), "fill", progress.clone());
        share.set(WorkerId(1), "fill", progress);
        share.set_capacity(WorkerId(0), 1000);
        share.set_capacity(WorkerId(1), 1000);
        let (reports, pinnable) = share.report();
        assert_eq!(pinnable, 1600);
        assert_eq!(reports.len(), 1);
        assert_eq!(reports[0].pinned_bytes, 400);
    }

    #[test]
    fn share_drops_inconsistent_workers() {
        let share = FillShare::new();
        let mut progress = WorkerProgress {
            generation: 1,
            membership: 2,
            digest: "aa".repeat(32),
            owned_pages: 1,
            pinned_pages: 1,
            owned_bytes: 100,
            pinned_bytes: 100,
            refused_bytes: 0,
        };
        share.set(WorkerId(0), "fill", progress.clone());
        progress.generation = 2;
        share.set(WorkerId(1), "fill", progress);
        assert!(share.report().0.is_empty());
    }
}
