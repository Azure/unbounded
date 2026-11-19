//! ClusterCacheFill node controller.
//!
//! One controller runs per I/O worker. It reads the process-wide fill view,
//! fetches the pages this worker owns through the normal prefetch path, and
//! pins them until the fill changes or disappears. Progress is aggregated per
//! node and reported through the Pod report annotation.
//!
//! Pins are desired state, not durable state: a restart drops them and the
//! controller establishes them again from the manifest. A membership change
//! drops the local plan because replica placement changed.

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
use std::collections::{BTreeMap, BTreeSet};
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
/// Backoff after a failed page fetch.
const RETRY: Duration = Duration::from_secs(10);
/// Bound on one fill's page fetch.
const FETCH_TIMEOUT: Duration = Duration::from_secs(120);
/// Manifest pages one fill may examine in one service turn.
///
/// Pages this worker does not own count: a node outside the cache pool walks
/// every page without fetching any, so the manifest object cap does not bound a
/// turn. Keeping the walk small leaves the I/O reactor to foreground reads and
/// control work. The cursor stays in [`LocalFill`], and a spent budget requests
/// an immediate wakeup, so the scan continues on the next turn.
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
    page: u64,
    accounted: u64,
    version: ObjectVersion,
    future: Pin<Box<dyn Future<Output = Result<PageResult>>>>,
}

struct LocalFill {
    generation: u64,
    digest: String,
    membership: u64,
    manifest: Arc<Manifest>,
    cache: CacheId,
    replicas: u8,
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
    retry_at: Instant,
}

impl LocalFill {
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
    /// This worker owns the page, and the cursor stayed on it so a failed fetch
    /// retries it.
    Owned(FillObject, u64),
}

/// Visit at most [`SCAN_BUDGET`] manifest pages from `state`'s cursor,
/// including pages `owned` rejects. Returns the first owned page without
/// advancing past it; `finish` advances once the fetch succeeds.
fn scan_fill(
    cx: &mut Context<'_>,
    state: &mut LocalFill,
    page_size: u64,
    mut owned: impl FnMut(&FillObject, u64) -> Result<bool>,
) -> Result<ScanStep> {
    let mut visited = 0;
    while let Some(object) = state.manifest.objects.get(state.object).cloned() {
        let pages = object.length.div_ceil(page_size);
        if state.page >= pages {
            state.object += 1;
            state.page = 0;
            continue;
        }
        if visited == SCAN_BUDGET {
            cx.waker().wake_by_ref();
            return Ok(ScanStep::Budget);
        }
        visited += 1;
        let number = state.page;
        if owned(&object, number)? {
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
    inflight: Vec<Inflight>,
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
            inflight: Vec::new(),
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
        // A forgotten fill can be replaced in place: a membership change, a
        // manifest resync, or a renewal reset installs a new plan without
        // changing the spec generation, so `finish` cannot tell a completion
        // launched for the old plan from one for the new plan. Drop this
        // fill's receivers so an obsolete completion never credits a page or
        // advances the replacement plan's cursor.
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

    /// Adopt new fills and drop fills that disappeared or changed.
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
            let local = self
                .coordinator
                .current_membership()
                .map(|m| m.version.0)
                .unwrap_or(0);
            if local != membership {
                // The authority planned against a newer membership than this
                // node has adopted. Wait instead of pinning a stale ranking.
                self.forget(&view.uid);
                continue;
            }
            seen.insert(view.uid.clone());
            let stale = self.local.get(&view.uid).is_none_or(|state| {
                state.generation != view.generation
                    || state.digest != manifest.digest
                    || state.membership != membership
            });
            if stale {
                self.forget(&view.uid);
                self.local.insert(
                    view.uid.clone(),
                    LocalFill {
                        generation: view.generation,
                        digest: manifest.digest.clone(),
                        membership,
                        manifest,
                        cache: CacheId(view.cache_uid.clone()),
                        replicas: view.replicas.max(1),
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
                        retry_at: now,
                    },
                );
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
    /// bounded per fill so a turn stays short; [`scan_fill`] wakes when more
    /// manifest remains.
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
        let fills: Vec<String> = self.local.keys().cloned().collect();
        for uid in fills {
            if self.inflight.iter().any(|work| work.fill == uid) {
                continue;
            }
            let state = self.local.get_mut(&uid).expect("listed locally");
            if now < state.retry_at {
                continue;
            }
            let cache = state.cache.clone();
            let replicas = state.replicas;
            let step = scan_fill(cx, state, page_size, |object, number| {
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
                if !ranked {
                    return Ok(false);
                }
                Ok(directory.page_owner(&page)? == worker)
            })?;
            let ScanStep::Owned(object, page_number) = step else {
                continue;
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
                    // No pinned headroom. Count the bytes as refused and
                    // retry after the next reservation frees room.
                    if state.refused.insert(page.clone()) {
                        state.refused_bytes = state.refused_bytes.saturating_add(page_size);
                    }
                    state.retry_at = now + RETRY;
                    continue;
                }
                Err(Error::MissingKey) => {
                    // The cache has no page key yet. Nothing is pinned and
                    // no capacity was refused; try again later.
                    state.retry_at = now + RETRY;
                    continue;
                }
                Err(error) => return Err(error),
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
            self.inflight.push(Inflight {
                fill: uid,
                generation: state.generation,
                page: page_number,
                accounted: page_size,
                version,
                future,
            });
        }
        Ok(())
    }

    /// Record finished fetches. Success advances the fill; failure releases the
    /// page reservation and retries it later.
    fn finish(&mut self, cx: &mut Context<'_>) {
        let mut index = 0;
        while index < self.inflight.len() {
            let ready = Pin::new(&mut self.inflight[index].future).poll(cx);
            match ready {
                Poll::Pending => index += 1,
                Poll::Ready(result) => {
                    let work = self.inflight.swap_remove(index);
                    let Some(state) = self.local.get_mut(&work.fill) else {
                        continue;
                    };
                    if state.generation != work.generation {
                        continue;
                    }
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
                            state.page = work.page + 1;
                            state.retry_at = self.clock.now();
                        }
                        Ok(PageResult::Deferred { .. }) | Ok(PageResult::Failed(_)) | Err(_) => {
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
                            state.retry_at = self.clock.now() + RETRY;
                        }
                    }
                }
            }
        }
    }

    /// Extend every live pin. A pin whose page is no longer readable (its page
    /// key was pruned) invalidates the fill's plan, so the next refresh adopts
    /// the fill again and fetches every page under the current key.
    fn renew(&mut self, _now: Instant) {
        let mut reset = Vec::new();
        for (uid, state) in &self.local {
            let id = fill_id(uid);
            for version in &state.versions {
                let _ = self
                    .index
                    .renew_pin(id, version, self.clock.wall_now() + PIN_TTL);
            }
            let unreadable = state.resident.iter().any(|page| {
                matches!(self.index.lookup(page), Ok(None)) || !self.index.is_pinned(page)
            });
            if unreadable {
                reset.push(uid.clone());
            }
        }
        for uid in reset {
            self.forget(&uid);
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
fn forget_inflight(inflight: &mut Vec<Inflight>, fill: &str) {
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
            retry_at: Instant::now(),
        }
    }

    #[test]
    fn scan_stops_at_the_page_budget_and_requests_a_wakeup() {
        let pages = (SCAN_BUDGET + 3) as u64;
        let mut state = local(vec![fill_object(1, pages)]);
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut visits = 0;
        let step = scan_fill(&mut cx, &mut state, 1, |_, _| {
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
            let step = scan_fill(&mut cx, &mut state, 1, |object, number| {
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
        // The launch path fetches from this cursor and finish advances it, so a
        // failed fetch retries the same page.
        assert_eq!(state.page, owned_page);
        assert!(!wake.0.load(std::sync::atomic::Ordering::SeqCst));
    }

    #[test]
    fn scan_exhausting_the_manifest_does_not_wake() {
        let mut state = local(vec![fill_object(3, 2)]);
        let wake = Arc::new(CountWake(std::sync::atomic::AtomicBool::new(false)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let step = scan_fill(&mut cx, &mut state, 1, |_, _| Ok(false)).unwrap();
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
            page,
            accounted: 1024,
            version: version.clone(),
            future: pending(),
        };
        let mut inflight = vec![work("a", 0), work("b", 0), work("a", 1)];
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
