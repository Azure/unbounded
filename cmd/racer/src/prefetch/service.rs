//! Prefetch driver: client-socket handlers, the dispatch pump, and the owner
//! side of the `prefetch` peer operation.

use super::api::{self, Coverage, Reason, RefError};
use super::metrics::{Counter, PrefetchMetrics};
use super::registry::{
    Batch, PutCheck, Registry, Resolution, RetryKind, UnitContext, UnitKey, UnitOutcome,
};
use super::{Config, Priority};
use crate::error::{Error, Result};
use crate::model::{
    MetadataSelector, ObjectId, ObjectVersion, PAGE_BYTES, PageId, PageNumber, RequestId,
    VersionMetadata,
};
use crate::peer::protocol::{Operation as PeerOperation, PeerResponse, PrefetchAction};
use crate::read::Coordinator;
use crate::runtime::RequestScope;
use crate::security::{Authorization, OpaqueMetadata, OriginContext};
use crate::topology::Membership;
use futures::StreamExt;
use http1::MessageHead;
use racer_core::prefetch::{Deferral, FailCode, OwnerBudget, OwnerLease, PageResult};
use racer_wire::control::{CacheId, NodeId};
use racer_wire::object::prefetch::{ErrorCode, PrefetchError, PrefetchRequest, StatusQuery};
use std::collections::{BTreeMap, HashMap};
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::task::{Poll, Waker};
use std::time::{Duration, Instant};
use zeroize::Zeroizing;

/// Concurrent batches the pump keeps in flight.
const MAX_BATCHES: usize = 8;

/// Bound on one batch's dispatch, including the owner's fills.
const DISPATCH_TIMEOUT: Duration = Duration::from_secs(120);

/// Bound on version resolution for one PUT.
const RESOLVE_TIMEOUT: Duration = Duration::from_secs(60);

/// Concurrent version resolutions per PUT.
const RESOLVE_CONCURRENCY: usize = 16;

/// Status long-poll wakers, one per waiting request: a poll replaces its own
/// entry instead of adding another, and a finished wait removes it.
#[derive(Default)]
struct Waiters {
    next: u64,
    wakers: HashMap<u64, Waker>,
}

/// One status long-poll's place in [`Prefetch::waiters`]; removed on drop.
struct Waiter<'a> {
    prefetch: &'a Prefetch,
    id: u64,
}

impl Waiter<'_> {
    fn register(&self, waker: &Waker) {
        let mut waiters = self.prefetch.waiters.lock().unwrap();
        match waiters.wakers.get_mut(&self.id) {
            Some(current) if current.will_wake(waker) => {}
            Some(current) => current.clone_from(waker),
            None => {
                waiters.wakers.insert(self.id, waker.clone());
            }
        }
    }
}

impl Drop for Waiter<'_> {
    fn drop(&mut self) {
        self.prefetch
            .waiters
            .lock()
            .unwrap()
            .wakers
            .remove(&self.id);
    }
}

/// One owner fill admission. Dropping it, including with a cancelled fill
/// future, returns the reference so the slot and its bookkeeping are released.
pub(crate) struct OwnerFill<'a> {
    prefetch: &'a Prefetch,
    page: PageId,
    lease: Option<OwnerLease>,
}

impl Drop for OwnerFill<'_> {
    fn drop(&mut self) {
        if let Some(lease) = self.lease.take() {
            self.prefetch.owner_finish(&self.page, lease);
        }
    }
}

#[derive(Default)]
struct Pump {
    running: bool,
    waker: Option<Waker>,
}

/// Node-wide prefetch state shared by every worker.
pub struct Prefetch {
    registry: Mutex<Registry>,
    owner: Mutex<OwnerBudget<PageId>>,
    /// Promoted prefetch fills: priority and when demand joined.
    promoted: Mutex<HashMap<PageId, (Priority, Instant)>>,
    /// Priority of each active owner fill.
    filling: Mutex<HashMap<PageId, Priority>>,
    draining: AtomicBool,
    pump: Mutex<Pump>,
    kicked: AtomicBool,
    waiters: Mutex<Waiters>,
    next_wake: Mutex<Option<Instant>>,
    metrics: OnceLock<Arc<PrefetchMetrics>>,
    /// Node-wide time and entropy.
    ctx: racer_io::WorkerCtx,
}

impl Prefetch {
    pub fn new(ctx: racer_io::WorkerCtx) -> Self {
        let config = Config::default();
        // A failed draw keeps a fixed scheduler seed; it only orders ties.
        let seed = ctx.rng.next_u64().unwrap_or_default();
        Self {
            registry: Mutex::new(Registry::new(config, seed, ctx.clock.clone())),
            owner: Mutex::new(OwnerBudget::new(
                config.concurrent_fills as usize,
                config.origin_bytes_per_sec,
            )),
            promoted: Mutex::new(HashMap::new()),
            filling: Mutex::new(HashMap::new()),
            draining: AtomicBool::new(false),
            pump: Mutex::new(Pump::default()),
            kicked: AtomicBool::new(false),
            waiters: Mutex::new(Waiters::default()),
            next_wake: Mutex::new(None),
            metrics: OnceLock::new(),
            ctx,
        }
    }

    /// Apply `RACER_PREFETCH_*` settings.
    fn now(&self) -> Instant {
        self.ctx.now()
    }

    /// A random request identifier; a failed draw yields the zero identifier.
    fn request_id(&self) -> RequestId {
        let mut bytes = [0; 16];
        let _ = self.ctx.rng.fill(&mut bytes);
        RequestId(bytes)
    }

    /// A fresh scope on this node's clock.
    fn scope(&self, deadline: Instant) -> Result<RequestScope> {
        RequestScope::with_clock(self.request_id(), deadline, self.ctx.clock.clone())
    }

    pub fn configure(&self, config: Config) -> Result<()> {
        config.validate()?;
        self.registry.lock().unwrap().configure(config);
        *self.owner.lock().unwrap() = OwnerBudget::new(
            config.concurrent_fills as usize,
            config.origin_bytes_per_sec,
        );
        Ok(())
    }

    pub fn config(&self) -> Config {
        *self.registry.lock().unwrap().config()
    }

    /// Share the node's prefetch metrics. The first attachment wins.
    pub(crate) fn attach_metrics(&self, metrics: &crate::telemetry::Metrics) {
        let _ = self.metrics.set(metrics.prefetch().clone());
    }

    fn metrics(&self) -> Option<&Arc<PrefetchMetrics>> {
        self.metrics.get()
    }

    /// Stop admitting tickets and dispatching. Requests then get 503.
    pub fn stop(&self) {
        self.draining.store(true, Ordering::Release);
        self.kick();
        self.wake_waiters();
    }

    pub fn draining(&self) -> bool {
        self.draining.load(Ordering::Acquire)
    }

    /// Wake the pump to re-plan.
    fn kick(&self) {
        self.kicked.store(true, Ordering::Release);
        if let Some(waker) = self.pump.lock().unwrap().waker.take() {
            waker.wake();
        }
    }

    fn wake_waiters(&self) {
        let wakers = std::mem::take(&mut self.waiters.lock().unwrap().wakers);
        for waker in wakers.into_values() {
            waker.wake();
        }
    }

    fn waiter(&self) -> Waiter<'_> {
        let mut waiters = self.waiters.lock().unwrap();
        waiters.next += 1;
        Waiter {
            prefetch: self,
            id: waiters.next,
        }
    }

    /// Publish metric deltas and gauges, refresh the wake deadline, and wake
    /// status waiters after a registry change.
    fn changed(&self, registry: &mut Registry) {
        let (events, finished) = registry.take_events();
        if let Some(metrics) = self.metrics() {
            for (counter, priority, value) in events {
                metrics.add(counter, priority, value);
            }
            for (priority, outcome) in finished {
                metrics.finished(priority, outcome);
            }
            let (queued, tickets) = registry.gauges();
            metrics.set_gauges(queued, tickets);
        }
        // Keep a pending status-wait wake from `wake_at`: the registry does not
        // know it, and replacing it would leave that waiter parked past `until`.
        let now = self.now();
        let mut next = self.next_wake.lock().unwrap();
        *next = registry
            .next_deadline()
            .into_iter()
            .chain(next.filter(|at| *at > now))
            .min();
    }

    /// Called from every worker's poll: expire tickets and wake the pump and
    /// waiters when a deadline passed.
    pub fn tick(&self) {
        let now = self.now();
        let due = self
            .next_wake
            .try_lock()
            .is_ok_and(|next| next.is_some_and(|at| at <= now));
        if !due {
            return;
        }
        {
            let mut registry = self.registry.lock().unwrap();
            registry.expire(now);
            self.changed(&mut registry);
        }
        self.kick();
        self.wake_waiters();
    }

    /// When the next [`Self::tick`] has expiries to run. A contended lock
    /// reports none; its holder publishes the next deadline itself.
    pub fn next_wake(&self) -> Option<Instant> {
        self.next_wake.try_lock().ok().and_then(|next| *next)
    }

    /// Ask the next tick at or after `at` to wake waiters.
    pub(super) fn wake_at(&self, at: Instant) {
        let mut next = self.next_wake.lock().unwrap();
        if next.is_none_or(|current| at < current) {
            *next = Some(at);
        }
    }

    /// Cancel tickets of caches that switched to passthrough.
    pub fn passthrough_caches(&self, caches: &std::collections::HashSet<CacheId>) {
        if caches.is_empty() {
            return;
        }
        let mut registry = self.registry.lock().unwrap();
        registry.cancel_caches(caches, Reason::Passthrough, self.now());
        self.changed(&mut registry);
        drop(registry);
        self.wake_waiters();
    }

    /// Cancel tickets of caches that are no longer published.
    pub fn retain_caches(&self, keep: impl Fn(&CacheId) -> bool) {
        let mut registry = self.registry.lock().unwrap();
        registry.retain_caches(keep, self.now());
        self.changed(&mut registry);
        drop(registry);
        self.wake_waiters();
    }

    /// A demand read for `page` is about to join its flight. If an owner
    /// prefetch fill holds a slot for it, release that slot and its tokens.
    pub fn promote(&self, page: &PageId) -> bool {
        let promoted = self.owner.lock().unwrap().promote(page);
        if promoted {
            let priority = self
                .filling
                .lock()
                .unwrap()
                .get(page)
                .copied()
                .unwrap_or_default();
            if let Some(metrics) = self.metrics() {
                metrics.add(Counter::DemandJoins, priority, 1);
            }
            self.promoted
                .lock()
                .unwrap()
                .insert(page.clone(), (priority, self.now()));
        }
        promoted
    }

    /// Owner admission for one page fill.
    fn owner_admit(
        &self,
        page: &PageId,
        bytes: u64,
        priority: Priority,
    ) -> std::result::Result<OwnerFill<'_>, PageResult> {
        let now = self.now();
        let mut owner = self.owner.lock().unwrap();
        let expired = owner.expire(now);
        let admitted = owner.admit(page.clone(), bytes, now);
        drop(owner);
        if !expired.is_empty() {
            let mut filling = self.filling.lock().unwrap();
            let mut promoted = self.promoted.lock().unwrap();
            for page in &expired {
                filling.remove(page);
                promoted.remove(page);
            }
        }
        match admitted {
            Ok(lease) => {
                self.filling
                    .lock()
                    .unwrap()
                    .entry(page.clone())
                    .or_insert(priority);
                Ok(OwnerFill {
                    prefetch: self,
                    page: page.clone(),
                    lease: Some(lease),
                })
            }
            Err((why, wait)) => {
                if let Some(metrics) = self.metrics()
                    && why == Deferral::Throttle
                {
                    metrics.throttled(priority, wait);
                }
                Err(PageResult::Deferred {
                    retry_after_ms: wait.as_millis().clamp(1, u128::from(u32::MAX)) as u32,
                })
            }
        }
    }

    fn owner_finish(&self, page: &PageId, lease: OwnerLease) {
        let mut owner = self.owner.lock().unwrap();
        owner.finish(page, lease);
        let held = owner.holds(page);
        drop(owner);
        // Another admission still references this fill.
        if held {
            return;
        }
        self.filling.lock().unwrap().remove(page);
        if let Some((priority, at)) = self.promoted.lock().unwrap().remove(page)
            && let Some(metrics) = self.metrics()
        {
            metrics.demand_wait(priority, self.now().saturating_duration_since(at));
        }
    }

    /// Owner prefetch fills holding a slot.
    pub fn active_fills(&self) -> usize {
        self.owner.lock().unwrap().active()
    }

    /// Start the pump on this worker unless one is running.
    fn ensure_pump(self: &Arc<Self>, local: Rc<Coordinator>) {
        let mut pump = self.pump.lock().unwrap();
        if pump.running {
            drop(pump);
            self.kick();
            return;
        }
        let Ok(permit) = crate::rt::drivers::reserve() else {
            return;
        };
        pump.running = true;
        drop(pump);
        let prefetch = self.clone();
        permit.submit_detached(Box::pin(async move { run_pump(prefetch, local).await }));
    }

    /// Run `f` against the registry and publish the change.
    fn with_registry<T>(&self, f: impl FnOnce(&mut Registry) -> T) -> T {
        let mut registry = self.registry.lock().unwrap();
        let value = f(&mut registry);
        self.changed(&mut registry);
        value
    }

    #[cfg(test)]
    pub(crate) fn owner_admit_for_test(
        &self,
        page: &PageId,
        priority: Priority,
    ) -> std::result::Result<OwnerFill<'_>, PageResult> {
        self.owner_admit(page, PAGE_BYTES, priority)
    }

    #[cfg(test)]
    pub(crate) fn filling_for_test(&self) -> usize {
        self.filling.lock().unwrap().len() + self.promoted.lock().unwrap().len()
    }

    #[cfg(test)]
    pub(crate) fn wait_for_test(&self, waker: &Waker, polls: usize) -> usize {
        let waiter = self.waiter();
        for _ in 0..polls {
            waiter.register(waker);
        }
        let during = self.waiters.lock().unwrap().wakers.len();
        drop(waiter);
        during + self.waiters.lock().unwrap().wakers.len()
    }
}

/// Classify a whole-operation or whole-page error.
pub(crate) fn classify(error: Error) -> UnitOutcome {
    match error {
        Error::Overloaded | Error::FlightFull => UnitOutcome::Retry(RetryKind::Overloaded, None),
        Error::VersionUnavailable => UnitOutcome::Failed(RefError::VersionChanged),
        Error::NotFound => UnitOutcome::Failed(RefError::NotFound),
        Error::OriginRejected | Error::OriginForbidden | Error::Forbidden => {
            UnitOutcome::Failed(RefError::Forbidden)
        }
        Error::InvalidRequest | Error::CorruptRecord | Error::Unauthorized => {
            UnitOutcome::Retry(RetryKind::Unsupported, None)
        }
        _ => UnitOutcome::Retry(RetryKind::Unavailable, None),
    }
}

/// Map one owner record.
pub(crate) fn outcome_of(result: PageResult) -> UnitOutcome {
    match result {
        PageResult::Admitted => UnitOutcome::Admitted,
        PageResult::Resident => UnitOutcome::Resident,
        PageResult::Deferred { retry_after_ms } => UnitOutcome::Retry(
            RetryKind::Deferred,
            Some(Duration::from_millis(u64::from(retry_after_ms))),
        ),
        PageResult::Failed(code) => match code {
            FailCode::VersionChanged => UnitOutcome::Failed(RefError::VersionChanged),
            FailCode::NotFound => UnitOutcome::Failed(RefError::NotFound),
            FailCode::Forbidden => UnitOutcome::Failed(RefError::Forbidden),
            FailCode::OriginError => UnitOutcome::Failed(RefError::OriginError),
            FailCode::Overloaded => UnitOutcome::Retry(RetryKind::Overloaded, None),
            FailCode::Unavailable => UnitOutcome::Retry(RetryKind::Unavailable, None),
            FailCode::NotResident => UnitOutcome::Retry(RetryKind::Unsupported, None),
        },
    }
}

/// Owner-side failure code for a page error.
fn fail_code(error: Error) -> FailCode {
    match error {
        Error::VersionUnavailable => FailCode::VersionChanged,
        Error::NotFound => FailCode::NotFound,
        Error::OriginRejected | Error::OriginForbidden | Error::Forbidden => FailCode::Forbidden,
        Error::Overloaded | Error::FlightFull => FailCode::Overloaded,
        Error::CorruptRecord | Error::BadGateway | Error::Internal => FailCode::OriginError,
        _ => FailCode::Unavailable,
    }
}

fn origin_context(object: &ObjectId, context: &UnitContext) -> Result<OriginContext> {
    Ok(OriginContext {
        object: object.clone(),
        metadata: context
            .metadata
            .as_ref()
            .map(|m| OpaqueMetadata::from_header(m.as_bytes()))
            .transpose()?,
        authorization: context
            .authorization
            .as_ref()
            .map(|a| Authorization::from_header(a))
            .transpose()?,
    })
}

fn page_length(length: u64, page: u64) -> u64 {
    length.saturating_sub(page * PAGE_BYTES).min(PAGE_BYTES)
}

/// Owner side of `prefetch fill`: one result per page, in order. Pages are
/// filled concurrently up to the owner budget; the rest are deferred.
#[allow(clippy::too_many_arguments)]
pub(crate) async fn serve_fill(
    local: &Coordinator,
    version: &ObjectVersion,
    pages: &[u64],
    metadata: &VersionMetadata,
    priority: Priority,
    membership: Arc<Membership>,
    context: &OriginContext,
    scope: &RequestScope,
) -> Vec<PageResult> {
    let directory = local.prefetch_directory();
    let prefetch = &directory.prefetch;
    if local.cache_passthrough(&version.object.cache) {
        return vec![PageResult::Failed(FailCode::Unavailable); pages.len()];
    }
    let fills = pages.iter().map(|&number| {
        let page = PageId {
            version: version.clone(),
            number: PageNumber(number),
        };
        let membership = membership.clone();
        async move {
            match directory.probe_page(page.clone(), scope).await {
                Ok(true) => return PageResult::Resident,
                Ok(false) => {}
                Err(error) => return PageResult::Failed(fail_code(error)),
            }
            // Held across the fill: dropping this future releases the slot too.
            let _fill =
                match prefetch.owner_admit(&page, page_length(metadata.length, number), priority) {
                    Ok(fill) => fill,
                    Err(deferred) => return deferred,
                };
            let result = directory
                .prefetch_page(page.clone(), metadata.clone(), membership, context, scope)
                .await;
            result.unwrap_or_else(|error| PageResult::Failed(fail_code(error)))
        }
    });
    futures::future::join_all(fills).await
}

/// Owner side of `prefetch probe`.
async fn serve_probe(
    local: &Coordinator,
    version: &ObjectVersion,
    pages: &[u64],
    scope: &RequestScope,
) -> Vec<PageResult> {
    let directory = local.prefetch_directory();
    let probes = pages.iter().map(|&number| {
        let page = PageId {
            version: version.clone(),
            number: PageNumber(number),
        };
        async move {
            match directory.probe_page(page, scope).await {
                Ok(true) => PageResult::Resident,
                _ => PageResult::Failed(FailCode::NotResident),
            }
        }
    });
    futures::future::join_all(probes).await
}

/// Serve an authenticated `prefetch` peer operation.
pub(crate) async fn serve(
    local: &Coordinator,
    version: ObjectVersion,
    action: PrefetchAction,
    membership: Arc<Membership>,
    context: &OriginContext,
    scope: &RequestScope,
) -> Result<PeerResponse> {
    if local.prefetch_directory().prefetch.draining() {
        return Ok(PeerResponse::Unavailable);
    }
    // A node outside the cache's pool never owns its pages. The sender ranked
    // on a membership where it did, so it re-plans on a newer one. Check this
    // node's current membership too: a lagging sender may still carry a
    // retained version from before this node left the pool.
    let me = local.local_node();
    let departed = local
        .current_membership()
        .is_some_and(|current| !current.serves(&version.object.cache, &me));
    if departed || !membership.serves(&version.object.cache, &me) {
        return Ok(PeerResponse::StaleMembership);
    }
    Ok(PeerResponse::Prefetch(match action {
        PrefetchAction::Probe { pages } => serve_probe(local, &version, &pages, scope).await,
        PrefetchAction::Fill {
            pages,
            length,
            content_type,
            priority,
        } => {
            let metadata = VersionMetadata {
                content_type,
                version: version.clone(),
                length,
            };
            serve_fill(
                local, &version, &pages, &metadata, priority, membership, context, scope,
            )
            .await
        }
    }))
}

/// Dispatch one batch to the pages' rank-0 owners.
async fn dispatch(
    local: &Coordinator,
    batch: Batch,
) -> Vec<(UnitKey, UnitOutcome, Option<NodeId>)> {
    let version = batch.metadata.version.clone();
    let unit = |page| UnitKey {
        version: version.clone(),
        page,
    };
    let all = |outcome: UnitOutcome| {
        batch
            .pages
            .iter()
            .map(|&page| (unit(page), outcome, None))
            .collect::<Vec<_>>()
    };
    let prefetch = &local.prefetch_directory().prefetch;
    let Ok(scope) = prefetch.scope(prefetch.now() + DISPATCH_TIMEOUT) else {
        return all(UnitOutcome::Retry(RetryKind::Unavailable, None));
    };
    let membership = match local.cache_membership(&version.object.cache) {
        Ok(membership) => membership,
        Err(error) => return all(classify(error)),
    };
    let context = match origin_context(&version.object, &batch.context) {
        Ok(context) => context,
        Err(_) => return all(UnitOutcome::Failed(RefError::OriginError)),
    };
    let me = local.local_node();
    let mut groups: BTreeMap<NodeId, Vec<u64>> = BTreeMap::new();
    let mut results = Vec::new();
    for &page in &batch.pages {
        match local
            .rank_owner(membership.clone(), &version.object, page, &scope)
            .await
        {
            Ok(Some(owner)) => groups.entry(owner).or_default().push(page),
            // An empty pool has no owner; retry once the pool has members.
            Ok(None) => results.push((
                unit(page),
                UnitOutcome::Retry(RetryKind::Unavailable, None),
                None,
            )),
            Err(error) => results.push((unit(page), classify(error), None)),
        }
    }
    let groups = groups.into_iter().map(|(owner, pages)| {
        let membership = membership.clone();
        let context = &context;
        let scope = &scope;
        let me = &me;
        let batch = &batch;
        let unit = &unit;
        async move {
            let outcomes: Vec<UnitOutcome> = if &owner == me {
                serve_fill(
                    local,
                    &batch.metadata.version,
                    &pages,
                    &batch.metadata,
                    batch.priority,
                    membership,
                    context,
                    scope,
                )
                .await
                .into_iter()
                .map(outcome_of)
                .collect()
            } else {
                let operation = PeerOperation::Prefetch {
                    version: batch.metadata.version.clone(),
                    action: PrefetchAction::Fill {
                        pages: pages.clone(),
                        length: batch.metadata.length,
                        content_type: batch.metadata.content_type.clone(),
                        priority: batch.priority,
                    },
                };
                let answer = local
                    .peer_prefetch(&membership, &owner, context, &operation, scope)
                    .await;
                match answer {
                    Ok(PeerResponse::Prefetch(records)) if records.len() == pages.len() => {
                        records.into_iter().map(outcome_of).collect()
                    }
                    Ok(response) => {
                        vec![response_outcome(&response); pages.len()]
                    }
                    Err(error) => vec![classify(error); pages.len()],
                }
            };
            pages
                .iter()
                .zip(outcomes)
                .map(|(&page, outcome)| (unit(page), outcome, Some(owner.clone())))
                .collect::<Vec<_>>()
        }
    });
    for group in futures::future::join_all(groups).await {
        results.extend(group);
    }
    results
}

/// Classify a whole-operation peer response.
fn response_outcome(response: &PeerResponse) -> UnitOutcome {
    match response {
        PeerResponse::Overloaded => UnitOutcome::Retry(RetryKind::Overloaded, None),
        PeerResponse::Unavailable | PeerResponse::Miss | PeerResponse::StaleMembership => {
            UnitOutcome::Retry(RetryKind::Unavailable, None)
        }
        PeerResponse::VersionUnavailable => UnitOutcome::Failed(RefError::VersionChanged),
        PeerResponse::NotFound => UnitOutcome::Failed(RefError::NotFound),
        PeerResponse::OriginRejected | PeerResponse::OriginForbidden => {
            UnitOutcome::Failed(RefError::Forbidden)
        }
        _ => UnitOutcome::Retry(RetryKind::Unsupported, None),
    }
}

/// The dispatch loop. Runs on one worker while any unit is open.
async fn run_pump(prefetch: Arc<Prefetch>, local: Rc<Coordinator>) {
    let mut inflight = futures::stream::FuturesUnordered::new();
    let mut membership_version = None;
    let max_pages = (prefetch.config().concurrent_fills as usize)
        .clamp(1, racer_core::prefetch::MAX_OPERATION_PAGES);
    loop {
        let now = prefetch.now();
        let draining = prefetch.draining();
        if let Some(membership) = local.current_membership() {
            if membership_version.is_some_and(|v| v != membership.version) {
                prefetch.with_registry(|registry| {
                    registry.requeue_departed(|cache, node| membership.serves(cache, node))
                });
            }
            membership_version = Some(membership.version);
        }
        if !draining {
            while inflight.len() < MAX_BATCHES {
                let Some(batch) =
                    prefetch.with_registry(|registry| registry.next_batch(now, max_pages))
                else {
                    break;
                };
                let local = local.clone();
                inflight.push(Box::pin(async move { dispatch(&local, batch).await }));
            }
        }
        if inflight.is_empty() {
            let mut pump = prefetch.pump.lock().unwrap();
            if draining || !prefetch.registry.lock().unwrap().has_open() {
                pump.running = false;
                pump.waker = None;
                return;
            }
        }
        let next = std::future::poll_fn(|cx| {
            prefetch.pump.lock().unwrap().waker = Some(cx.waker().clone());
            if prefetch.kicked.swap(false, Ordering::AcqRel) {
                return Poll::Ready(None);
            }
            match inflight.poll_next_unpin(cx) {
                Poll::Ready(Some(results)) => Poll::Ready(Some(results)),
                _ => Poll::Pending,
            }
        })
        .await;
        if let Some(results) = next {
            let now = prefetch.now();
            let membership = local.current_membership();
            prefetch.with_registry(|registry| {
                let mut completed = Vec::with_capacity(results.len());
                for (unit, outcome, owner) in results {
                    registry.complete(&unit, outcome, owner, now);
                    completed.push(unit);
                }
                // A pool change seen while these fills were in flight skipped
                // them; recheck their owners now that they have settled.
                if let Some(membership) = &membership {
                    registry.requeue_departed_among(&completed, |cache, node| {
                        membership.serves(cache, node)
                    });
                }
            });
            prefetch.wake_waiters();
        }
    }
}

/// Resolve each distinct ref version with the ticket's credentials.
async fn resolve(
    local: &Coordinator,
    cache: &CacheId,
    membership: &Arc<Membership>,
    spec: &api::Spec,
    authorization: Option<&Arc<Zeroizing<Vec<u8>>>>,
) -> Vec<Resolution> {
    type Distinct = (crate::model::CacheKey, Option<String>, Option<String>);
    let mut distinct: Vec<Distinct> = Vec::new();
    let mut index: HashMap<Distinct, usize> = HashMap::new();
    let positions: Vec<usize> = spec
        .refs
        .iter()
        .map(|r| {
            let key = (
                r.key,
                r.etag.as_ref().map(|e| e.as_str().to_owned()),
                r.metadata.clone(),
            );
            *index.entry(key.clone()).or_insert_with(|| {
                distinct.push(key);
                distinct.len() - 1
            })
        })
        .collect();
    let prefetch = &local.prefetch_directory().prefetch;
    let scope = prefetch.scope(prefetch.now() + RESOLVE_TIMEOUT);
    let mut resolved: Vec<Option<Resolution>> = vec![None; distinct.len()];
    if let Ok(scope) = &scope {
        let mut work = futures::stream::iter(distinct.iter().enumerate().map(
            |(i, (key, etag, metadata))| {
                let membership = membership.clone();
                async move {
                    let context = UnitContext {
                        metadata: metadata.clone(),
                        authorization: authorization.cloned(),
                    };
                    let object = super::registry::object(cache, *key);
                    let selector = match etag {
                        Some(etag) => match crate::model::StrongEtag::parse(etag.as_bytes()) {
                            Ok(etag) => MetadataSelector::Pinned(etag),
                            Err(_) => return (i, Resolution::Failed(RefError::VersionChanged)),
                        },
                        None => MetadataSelector::Fresh,
                    };
                    let result = match origin_context(&object, &context) {
                        Ok(origin) => {
                            local
                                .resolve_version(selector, membership, &origin, scope)
                                .await
                        }
                        Err(error) => Err(error),
                    };
                    (
                        i,
                        match result {
                            Ok(metadata) => Resolution::Version(metadata),
                            Err(error) => Resolution::Failed(match classify(error) {
                                UnitOutcome::Failed(error) => error,
                                _ => RefError::OriginError,
                            }),
                        },
                    )
                }
            },
        ))
        .buffer_unordered(RESOLVE_CONCURRENCY);
        while let Some((i, resolution)) = work.next().await {
            resolved[i] = Some(resolution);
        }
    }
    positions
        .into_iter()
        .map(|i| {
            resolved[i]
                .clone()
                .unwrap_or(Resolution::Failed(RefError::OriginError))
        })
        .collect()
}

/// Share of the remaining client request budget that coverage probes may use.
/// The rest is reserved so the status response is still written before the
/// client request deadline, even when an owner is slow or unresponsive.
const COVERAGE_BUDGET_NUMERATOR: u32 = 3;
const COVERAGE_BUDGET_DENOMINATOR: u32 = 4;

/// Deadline for coverage probes made on behalf of a request expiring at
/// `request_deadline`: a share of the remaining budget, capped at
/// [`DISPATCH_TIMEOUT`]. `None` when no budget remains.
pub(super) fn coverage_deadline(at: Instant, request_deadline: Instant) -> Option<Instant> {
    let remaining = request_deadline.checked_duration_since(at)?;
    let share =
        (remaining * COVERAGE_BUDGET_NUMERATOR / COVERAGE_BUDGET_DENOMINATOR).min(DISPATCH_TIMEOUT);
    (!share.is_zero()).then(|| at + share)
}

/// Probe the owners of a ticket's desired pages and count resident bytes.
/// Probing stops at `deadline`; the count is then a lower bound covering only
/// the owners that answered in time.
async fn coverage(
    local: &Coordinator,
    membership: &Arc<Membership>,
    units: Vec<UnitKey>,
    deadline: Instant,
) -> u64 {
    let Ok(scope) = local.prefetch_directory().prefetch.scope(deadline) else {
        return 0;
    };
    let me = local.local_node();
    let mut groups: BTreeMap<(NodeId, ObjectVersion), Vec<u64>> = BTreeMap::new();
    for unit in units {
        let Some(owner) = local
            .rank_owner(membership.clone(), &unit.version.object, unit.page, &scope)
            .await
            .ok()
            .flatten()
        else {
            continue;
        };
        groups
            .entry((owner, unit.version))
            .or_default()
            .push(unit.page);
    }
    let mut resident = 0u64;
    for ((owner, version), pages) in groups {
        for chunk in pages.chunks(racer_core::prefetch::MAX_OPERATION_PAGES) {
            if scope.check().is_err() {
                return resident;
            }
            let results = if owner == me {
                serve_probe(local, &version, chunk, &scope).await
            } else {
                let context = OriginContext {
                    object: version.object.clone(),
                    metadata: None,
                    authorization: None,
                };
                let operation = PeerOperation::Prefetch {
                    version: version.clone(),
                    action: PrefetchAction::Probe {
                        pages: chunk.to_vec(),
                    },
                };
                match local
                    .peer_prefetch(membership, &owner, &context, &operation, &scope)
                    .await
                {
                    Ok(PeerResponse::Prefetch(results)) => results,
                    _ => Vec::new(),
                }
            };
            resident += results
                .iter()
                .filter(|r| **r == PageResult::Resident)
                .count() as u64
                * PAGE_BYTES;
        }
    }
    resident
}

/// A complete prefetch response.
pub struct Response {
    pub head: MessageHead,
    pub body: Vec<u8>,
}

fn json(status: u16, value: &impl serde::Serialize) -> Response {
    match serde_json::to_vec(value) {
        Ok(body) => Response {
            head: racer_wire::object::prefetch::json_head(status, body.len()),
            body,
        },
        Err(_) => error(PrefetchError::new(
            ErrorCode::Unavailable,
            "encoding failed",
        )),
    }
}

pub fn error(error: PrefetchError) -> Response {
    Response {
        head: error.head(),
        body: error.body(),
    }
}

fn unavailable(message: &str) -> PrefetchError {
    PrefetchError::new(ErrorCode::Unavailable, message)
}

/// Handle one parsed prefetch request. `body` is the complete PUT body.
pub async fn handle(
    local: &Coordinator,
    cache: &CacheId,
    request: PrefetchRequest<'_>,
    body: &[u8],
    scope: &RequestScope,
) -> Response {
    match handle_inner(local, cache, request, body, scope).await {
        Ok(response) => response,
        Err(e) => error(e),
    }
}

async fn handle_inner(
    local: &Coordinator,
    cache: &CacheId,
    request: PrefetchRequest<'_>,
    body: &[u8],
    scope: &RequestScope,
) -> std::result::Result<Response, PrefetchError> {
    let directory = local.prefetch_directory().clone();
    let prefetch = &directory.prefetch;
    if prefetch.draining() {
        return Err(unavailable("node is draining"));
    }
    match request {
        PrefetchRequest::List => {
            let prefetches = prefetch.with_registry(|r| r.list(cache));
            Ok(json(200, &api::List { prefetches }))
        }
        PrefetchRequest::Delete { id } => {
            let status = prefetch.with_registry(|r| r.delete(cache, &id, prefetch.now()))?;
            prefetch.kick();
            prefetch.wake_waiters();
            Ok(json(200, &status))
        }
        PrefetchRequest::Get { id, query } => get(local, prefetch, cache, &id, query, scope).await,
        PrefetchRequest::Put {
            id, authorization, ..
        } => {
            let membership = local
                .cache_membership(cache)
                .map_err(|_| unavailable("cache is not ready"))?;
            if local.cache_passthrough(cache) {
                return Err(PrefetchError::new(
                    ErrorCode::Passthrough,
                    "prefetch is not available for a passthrough cache",
                ));
            }
            let config = prefetch.config();
            let spec = api::parse_spec(body, authorization.is_some(), &config)?;
            let authorization = authorization.map(|a| Arc::new(Zeroizing::new(a.to_vec())));
            if let PutCheck::Replay(status) = prefetch
                .with_registry(|r| r.check_put(cache, &id, &spec, &authorization, prefetch.now()))?
            {
                return Ok(json(200, &*status));
            }
            let resolved = resolve(local, cache, &membership, &spec, authorization.as_ref()).await;
            scope.check().map_err(|_| unavailable("request expired"))?;
            let (status, body) = prefetch.with_registry(|r| {
                r.apply_put(cache, &id, spec, resolved, authorization, prefetch.now())
            })?;
            if let Ok(local) = directory.local_coordinator() {
                prefetch.ensure_pump(local);
            }
            prefetch.wake_waiters();
            Ok(json(status, &body))
        }
    }
}

async fn get(
    local: &Coordinator,
    prefetch: &Prefetch,
    cache: &CacheId,
    id: &str,
    query: StatusQuery,
    scope: &RequestScope,
) -> std::result::Result<Response, PrefetchError> {
    // Validate the id and cursor before waiting.
    prefetch.with_registry(|r| r.status(cache, id, query.refs, query.cursor.as_deref()))?;
    if let Some(wait) = query.wait.filter(|w| !w.is_zero()) {
        let until = prefetch.now() + wait;
        prefetch.wake_at(until);
        let cancellation = scope.cancellation.subscribe().ok();
        let waiter = prefetch.waiter();
        std::future::poll_fn(|cx| {
            if let Some(cancellation) = &cancellation {
                cancellation.register(cx.waker());
            }
            waiter.register(cx.waker());
            let settled = prefetch
                .registry
                .lock()
                .unwrap()
                .settled(cache, id)
                .unwrap_or(true);
            if settled || prefetch.now() >= until || scope.check().is_err() || prefetch.draining() {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
    }
    let mut status =
        prefetch.with_registry(|r| r.status(cache, id, query.refs, query.cursor.as_deref()))?;
    if query.coverage {
        let units = prefetch.with_registry(|r| r.desired(cache, id))?;
        // Bound probing by the client request deadline so a slow or
        // unresponsive owner cannot consume the budget needed to answer.
        let deadline = coverage_deadline(prefetch.now(), scope.deadline.0);
        let resident = match (local.cache_membership(cache), deadline) {
            (Ok(membership), Some(deadline)) => coverage(local, &membership, units, deadline).await,
            _ => 0,
        };
        status.coverage = Some(Coverage {
            resident_bytes: resident,
            observed_at_ms: super::registry::wall_ms(&prefetch.ctx.clock),
        });
    }
    Ok(json(200, &status))
}
