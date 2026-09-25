//! Versioned metadata cache and page-zero-worker refresh singleflight.
//!
//! Keep old lengths separately from the current-version pointer. TTL gates fresh
//! admission; explicit pins may use expired metadata. A zero-TTL refresh admits its
//! waiters once. Clock discontinuities invalidate uncertain freshness. Cache entries
//! contain no origin context, Authorization, or opaque adapter metadata header.
use super::{
    candidates::{CandidatePolicy, CandidateResolution},
    flight::AcquisitionBudget,
};
use crate::{
    error::{Error, Operation, Result},
    model::{
        context::OriginContext,
        identity::{ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag},
        metadata::{MetadataSelector, ObjectMetadata},
    },
    origin::client::Origin,
    peer::{
        requester::PeerClient,
        wire::{FetchMode, Operation as PeerOperation, PeerResponse},
    },
    runtime::deadline::RequestScope,
    security::credentials::CredentialCrypto,
    store::index::Index,
    topology::membership::MembershipLease,
};
use std::{
    cell::{Cell, RefCell},
    collections::HashMap,
    future::{Future, poll_fn},
    pin::Pin,
    rc::Rc,
    sync::Arc,
    task::{Poll, Waker},
    time::{Duration, Instant, SystemTime},
};

const MAX_WAITERS: usize = 64;
const MAX_REFRESH_ATTEMPTS: usize = 8;
const MAX_BOOTSTRAP_ATTEMPTS: usize = 3;
const DEFAULT_ATTEMPTS: u32 = 32;
const DEFAULT_LINKS: u8 = 96;

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct RefreshKey {
    object: ObjectId,
    pin: Option<StrongEtag>,
}
impl RefreshKey {
    fn new(object: &ObjectId, selector: &MetadataSelector) -> Self {
        Self {
            object: object.clone(),
            pin: match selector {
                MetadataSelector::Fresh => None,
                MetadataSelector::Pinned(etag) => Some(etag.clone()),
            },
        }
    }
}

/// Only credential-free coordination lives here. Context, membership and credits
/// remain request-owned or in its admitted worker driver, never in this table.
#[derive(Default)]
struct Refresh {
    next: u64,
    leader: Option<u64>,
    attempts: usize,
    waiters: HashMap<u64, Option<Waker>>,
    result: Option<Result<RefreshOutput>>,
}
#[derive(Clone)]
struct RefreshOutput {
    metadata: ObjectMetadata,
    page: Option<super::fill::PageResult>,
}
#[derive(Default)]
struct RefreshTable {
    active: RefCell<HashMap<RefreshKey, Rc<RefCell<Refresh>>>>,
    // Includes completed cohorts still held by slow readers, not just active keys.
    registrations: Cell<usize>,
}
#[derive(Clone)]
struct Registration {
    table: Rc<RefreshTable>,
    key: RefreshKey,
    refresh: Rc<RefCell<Refresh>>,
    id: u64,
    lifetime: Rc<()>,
}
enum RefreshEvent {
    Lead,
    Complete(Result<RefreshOutput>),
}

impl RefreshTable {
    fn join(self: &Rc<Self>, key: RefreshKey, capacity: usize) -> Result<Registration> {
        if self.registrations.get() >= capacity.saturating_mul(MAX_WAITERS) {
            return Err(Error::Overloaded);
        }
        let mut active = self.active.borrow_mut();
        let refresh = if let Some(refresh) = active.get(&key) {
            refresh.clone()
        } else {
            if active.len() >= capacity {
                return Err(Error::Overloaded);
            }
            let refresh = Rc::new(RefCell::new(Refresh::default()));
            active.insert(key.clone(), refresh.clone());
            refresh
        };
        let id = {
            let mut state = refresh.borrow_mut();
            if state.waiters.len() >= MAX_WAITERS {
                return Err(Error::Overloaded);
            }
            let id = state.next;
            state.next = state.next.checked_add(1).ok_or(Error::Overloaded)?;
            state.waiters.insert(id, None);
            id
        };
        self.registrations.set(self.registrations.get() + 1);
        Ok(Registration {
            table: self.clone(),
            key,
            refresh,
            id,
            lifetime: Rc::new(()),
        })
    }
}
impl Registration {
    fn retry(&self) {
        let wakers = {
            let mut state = self.refresh.borrow_mut();
            if state.leader == Some(self.id) {
                state.leader = None;
            }
            take_wakers(&mut state)
        };
        for waker in wakers {
            waker.wake();
        }
    }
    fn event(&self, waker: &Waker) -> Poll<RefreshEvent> {
        let mut state = self.refresh.borrow_mut();
        if let Some(result) = &state.result {
            return Poll::Ready(RefreshEvent::Complete(result.clone()));
        }
        state.waiters.insert(self.id, Some(waker.clone()));
        if state.leader.is_none() {
            if state.attempts >= MAX_REFRESH_ATTEMPTS {
                drop(state);
                self.finish(Err(Error::Unavailable));
                return Poll::Ready(RefreshEvent::Complete(Err(Error::Unavailable)));
            }
            state.attempts += 1;
            state.leader = Some(self.id);
            return Poll::Ready(RefreshEvent::Lead);
        }
        Poll::Pending
    }

    fn remove_active(&self) {
        let mut active = self.table.active.borrow_mut();
        if active
            .get(&self.key)
            .is_some_and(|entry| Rc::ptr_eq(entry, &self.refresh))
        {
            active.remove(&self.key);
        }
    }

    fn finish(&self, result: Result<RefreshOutput>) {
        let wakers = {
            let mut state = self.refresh.borrow_mut();
            state.result = Some(result);
            state.leader = None;
            take_wakers(&mut state)
        };
        // Close admission before waking the cohort. In particular, a new caller
        // cannot consume a completed zero-TTL observation even if old waiters live.
        self.remove_active();
        for waker in wakers {
            waker.wake();
        }
    }
}
impl Drop for Registration {
    fn drop(&mut self) {
        // An accepted worker driver holds the same registration until its real
        // operation completes, fencing detach/re-election after ingress drops.
        if Rc::strong_count(&self.lifetime) != 1 {
            return;
        }
        let (empty, wakers) = {
            let mut state = self.refresh.borrow_mut();
            state.waiters.remove(&self.id);
            let wakers = if state.leader == Some(self.id) {
                state.leader = None;
                take_wakers(&mut state)
            } else {
                Vec::new()
            };
            (state.waiters.is_empty(), wakers)
        };
        self.table
            .registrations
            .set(self.table.registrations.get() - 1);
        if empty {
            self.remove_active();
        }
        for waker in wakers {
            waker.wake();
        }
    }
}
fn take_wakers(state: &mut Refresh) -> Vec<Waker> {
    state
        .waiters
        .values_mut()
        .filter_map(Option::take)
        .collect()
}

#[derive(Default)]
struct Clock {
    sample: Option<(SystemTime, Instant)>,
    epoch: u64,
}
impl Clock {
    fn observe(&mut self, wall: SystemTime, monotonic: Instant) -> bool {
        let uncertain = self.sample.is_some_and(|(previous_wall, previous_mono)| {
            match (
                wall.duration_since(previous_wall),
                monotonic.checked_duration_since(previous_mono),
            ) {
                (Ok(wall_elapsed), Some(elapsed)) => {
                    wall_elapsed.abs_diff(elapsed) > Duration::from_secs(1)
                }
                _ => true,
            }
        });
        self.sample = Some((wall, monotonic));
        if uncertain {
            self.epoch = self.epoch.saturating_add(1);
        }
        uncertain
    }
}

enum RefreshFailure {
    // Only a credential-specific rejection, never a generic peer auth error.
    Rejected(Error),
    Terminal(Error),
}
impl From<Error> for RefreshFailure {
    fn from(error: Error) -> Self {
        if matches!(error, Error::OriginRejected | Error::OriginForbidden) {
            Self::Rejected(error)
        } else {
            Self::Terminal(error)
        }
    }
}

fn caller_budget_failure(error: Error, budget: &AcquisitionBudget) -> bool {
    matches!(
        error,
        Error::Cancelled | Error::DeadlineExceeded | Error::HopBudgetExhausted
    ) || (error == Error::Unavailable && budget.remaining_attempts() == 0)
}
/// An empty bootstrap is metadata-only: it never constructs an encrypted page.
pub enum BootstrapResult {
    Empty(ObjectMetadata),
    Page(super::fill::PageResult),
}
#[derive(Clone)]
pub struct MetadataDependencies {
    pub index: Rc<Index>,
    pub fill: Rc<super::fill::Fill>,
    pub owners: Arc<super::dispatch::WorkerDirectory>,
}
#[derive(Clone)]
pub struct MetadataService {
    candidates: Rc<CandidatePolicy>,
    origin: Rc<dyn Origin>,
    credentials: Rc<CredentialCrypto>,
    capacity: usize,
    storage: MetadataDependencies,
    refreshes: Rc<RefreshTable>,
    clock: Rc<RefCell<Clock>>,
}
impl MetadataService {
    /// Page completion handoff: immutable facts only, never fresh admission.
    pub fn publish_version(&self, metadata: crate::model::metadata::VersionMetadata) -> Result<()> {
        self.storage.index.publish_version(metadata)
    }

    pub fn new(
        candidates: Rc<CandidatePolicy>,
        origin: Rc<dyn Origin>,
        _peers: Rc<dyn PeerClient>,
        credentials: Rc<CredentialCrypto>,
        capacity: usize,
        storage: MetadataDependencies,
    ) -> Self {
        candidates.set_credentials(credentials.clone());
        Self {
            candidates,
            origin,
            credentials,
            capacity,
            storage,
            refreshes: Rc::new(RefreshTable::default()),
            clock: Rc::new(RefCell::new(Clock::default())),
        }
    }
    /// Missing pinned metadata may require a conditional page-zero probe; never
    /// substitute current-version length to resolve a suffix or final-page range.
    pub fn resolve<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, ObjectMetadata> {
        Box::pin(async move {
            let mut budget =
                AcquisitionBudget::new(scope.deadline.0, DEFAULT_ATTEMPTS, DEFAULT_LINKS);
            self.resolve_with_budget(selector, membership, context, scope, &mut budget)
                .await
        })
    }

    pub fn resolve_with_budget<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, ObjectMetadata> {
        Box::pin(async move {
            Ok(self
                .resolve_inner(selector, membership, context, scope, budget, false, false)
                .await?
                .metadata)
        })
    }

    fn observe_clock(&self) -> Result<SystemTime> {
        let now = SystemTime::now();
        if self.clock.borrow_mut().observe(now, Instant::now()) {
            self.storage.index.invalidate_freshness()?;
        }
        Ok(now)
    }

    fn resolve_inner<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        force_refresh: bool,
        bootstrap: bool,
    ) -> Operation<'a, RefreshOutput> {
        Box::pin(async move {
            let mut bounded_scope = scope.clone();
            bounded_scope.deadline.0 = scope.deadline.0.min(budget.deadline());
            let scope = &bounded_scope;
            scope.check()?;
            if !force_refresh {
                if let Some(metadata) = self.copy_only(selector.clone(), context, scope).await? {
                    return Ok(RefreshOutput {
                        metadata,
                        page: None,
                    });
                }
            }
            let registration = self
                .refreshes
                .join(RefreshKey::new(&context.object, &selector), self.capacity)?;
            let event = poll_fn(|cx| {
                super::drivers::poll(cx, 64);
                if let Err(error) = scope.cancellation.register(cx.waker()) {
                    return Poll::Ready(Err(error));
                }
                if let Err(error) = scope.check() {
                    return Poll::Ready(Err(error));
                }
                match registration.event(cx.waker()) {
                    Poll::Ready(event) => Poll::Ready(Ok(event)),
                    Poll::Pending => {
                        // The scope has cancellation notification but no deadline
                        // timer registration. Yield and recheck its original deadline.
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                }
            })
            .await?;
            match event {
                RefreshEvent::Complete(result) => result,
                RefreshEvent::Lead => {
                    if budget.remaining_attempts() == 0 {
                        return Err(Error::Unavailable);
                    }
                    let driver_permit = super::drivers::reserve()?;
                    let mut nonce = [0; 16];
                    getrandom::getrandom(&mut nonce).map_err(|_| Error::Unavailable)?;
                    let attempt = crate::model::identity::AttemptId(nonce);
                    let sealed = self.credentials.seal(context, attempt, scope)?;
                    let owned_context =
                        self.credentials
                            .open_charged(sealed, scope.request, attempt)?;
                    let service = self.clone();
                    let driver_registration = registration.clone();
                    let owned_scope = scope.clone();
                    let mut owned_budget = budget.transfer();
                    let (send, mut receive) = futures::channel::oneshot::channel();
                    driver_permit.submit(Box::pin(async move {
                        let mut work = Box::pin(service.refresh(
                            selector,
                            membership,
                            &owned_context,
                            &owned_scope,
                            &mut owned_budget,
                            bootstrap,
                        ));
                        let result = poll_fn(|cx| {
                            if Rc::strong_count(&driver_registration.lifetime) == 1 {
                                let _ = owned_scope.cancel();
                            }
                            work.as_mut().poll(cx)
                        })
                        .await;
                        drop(work);
                        let result = match result {
                            Ok(output) => {
                                driver_registration.finish(Ok(output.clone()));
                                Ok(output)
                            }
                            Err(RefreshFailure::Rejected(error)) => {
                                driver_registration.retry();
                                Err(error)
                            }
                            Err(RefreshFailure::Terminal(error)) => {
                                if caller_budget_failure(error, &owned_budget) {
                                    driver_registration.retry();
                                } else {
                                    driver_registration.finish(Err(error));
                                }
                                Err(error)
                            }
                        };
                        // Completion, not receiver lifetime, releases this context
                        // and registration. The original budget is returned intact.
                        let _ = send.send((result, owned_budget));
                        Ok(())
                    }));
                    let (result, remaining) = poll_fn(|cx| {
                        super::drivers::poll(cx, 64);
                        match Pin::new(&mut receive).poll(cx) {
                            Poll::Ready(Ok(value)) => Poll::Ready(Ok(value)),
                            Poll::Ready(Err(_)) => Poll::Ready(Err(Error::Unavailable)),
                            Poll::Pending => {
                                if let Err(error) = scope.check() {
                                    return Poll::Ready(Err(error));
                                }
                                cx.waker().wake_by_ref();
                                Poll::Pending
                            }
                        }
                    })
                    .await?;
                    *budget = remaining;
                    scope.check()?;
                    result
                }
            }
        })
    }

    async fn refresh(
        &self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        bootstrap: bool,
    ) -> std::result::Result<RefreshOutput, RefreshFailure> {
        scope.check()?;
        self.observe_clock()?;
        let clock_epoch = self.clock.borrow().epoch;
        let candidates = self
            .candidates
            .candidates_async(membership.clone(), &context.object, PageNumber(0))
            .await?;
        let operation = PeerOperation::Metadata {
            object: context.object.clone(),
            selector: selector.clone(),
            mode: FetchMode::Acquire,
        };
        let mut bootstrap_page = None;
        let metadata = match self
            .candidates
            .resolve_with_budget(candidates, context, operation, scope, budget)
            .await?
        {
            CandidateResolution::Copy(response) => match response.response() {
                PeerResponse::Metadata(metadata) => metadata.clone(),
                PeerResponse::OriginRejected => {
                    return Err(RefreshFailure::Rejected(Error::OriginRejected));
                }
                PeerResponse::OriginForbidden => {
                    return Err(RefreshFailure::Rejected(Error::OriginForbidden));
                }
                PeerResponse::VersionUnavailable => return Err(Error::VersionUnavailable.into()),
                PeerResponse::Overloaded => return Err(Error::Overloaded.into()),
                PeerResponse::Miss | PeerResponse::Unavailable => {
                    return Err(Error::Unavailable.into());
                }
                PeerResponse::Page { .. } => return Err(Error::CorruptRecord.into()),
            },
            CandidateResolution::Origin(authority) => {
                authority.validate(&context.object, PageNumber(0))?;
                budget.begin_attempt(Instant::now(), scope.deadline.0)?;
                let acquisition = if bootstrap && matches!(selector, MetadataSelector::Fresh) {
                    self.origin.bootstrap(&authority, context, scope).await
                } else {
                    self.origin
                        .metadata(&authority, context, selector.clone(), scope)
                        .await
                };
                let reply = match acquisition {
                    Ok(reply) => reply,
                    Err(Error::VersionUnavailable)
                        if matches!(selector, MetadataSelector::Pinned(_)) =>
                    {
                        // A conditional origin miss says nothing about immutable
                        // copies on later candidates. Probe those copy-only first.
                        let candidates = self
                            .candidates
                            .candidates_async(membership.clone(), &context.object, PageNumber(0))
                            .await?;
                        let operation = PeerOperation::Metadata {
                            object: context.object.clone(),
                            selector: selector.clone(),
                            mode: FetchMode::CopyOnly,
                        };
                        let response = self
                            .candidates
                            .remaining_copy(&candidates, context, &operation, scope, budget)
                            .await?
                            .ok_or_else(|| self.candidates.origin_miss_error(&authority))?;
                        match response.response() {
                            PeerResponse::Metadata(metadata) => {
                                crate::origin::metadata::MetadataReply {
                                    metadata: metadata.clone(),
                                    page_zero: None,
                                }
                            }
                            _ => return Err(Error::CorruptRecord.into()),
                        }
                    }
                    Err(error) => return Err(error.into()),
                };
                if let Some(page) = &reply.page_zero {
                    validate_bootstrap_metadata(&reply.metadata, &page.metadata)?;
                    if reply.metadata.length == 0 {
                        return Err(Error::CorruptRecord.into());
                    }
                }
                validate_metadata(&context.object, &selector, &reply.metadata)?;
                if let Some(page) = reply.page_zero {
                    let result = self
                        .storage
                        .fill
                        .publish_bootstrap_with_context(page, membership, context, scope, budget)
                        .await?;
                    validate_bootstrap_metadata(&reply.metadata, &result.metadata)?;
                    result.validate_for(&PageId {
                        version: reply.metadata.version.clone(),
                        number: PageNumber(0),
                    })?;
                    bootstrap_page = Some(result);
                }
                reply.metadata
            }
        };
        scope.check()?;
        validate_metadata(&context.object, &selector, &metadata)?;
        self.observe_clock()?;
        if matches!(selector, MetadataSelector::Fresh) && self.clock.borrow().epoch == clock_epoch {
            self.storage.index.publish_current(metadata.clone())?;
        } else {
            self.storage.index.publish_version(metadata.immutable())?;
        }
        Ok(RefreshOutput {
            metadata,
            page: bootstrap_page,
        })
    }
    pub fn copy_only<'a>(
        &'a self,
        selector: MetadataSelector,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<ObjectMetadata>> {
        Box::pin(async move {
            scope.check()?;
            let now = self.observe_clock()?;
            match selector {
                MetadataSelector::Fresh => {
                    let Some(current) = self.storage.index.current(&context.object)? else {
                        return Ok(None);
                    };
                    let Some(descriptor) = self.storage.index.version(&current.version)? else {
                        return Ok(None);
                    };
                    current.resolve(&descriptor, now)
                }
                MetadataSelector::Pinned(etag) => {
                    let version = ObjectVersion {
                        object: context.object.clone(),
                        etag,
                    };
                    if let Some(metadata) = self.storage.index.version(&version)? {
                        return Ok(Some(metadata.for_pin()));
                    }
                    // The directory bounds local-shard fanout and never starts I/O
                    // acquisition. An old page can outlive the page-zero catalog.
                    let retained = self
                        .storage
                        .owners
                        .retained_metadata(&version, scope)
                        .await?;
                    scope.check()?;
                    if let Some(metadata) = retained {
                        if metadata.version != version {
                            return Err(Error::CorruptRecord);
                        }
                        self.storage.index.publish_version(metadata.clone())?;
                        Ok(Some(metadata.for_pin()))
                    } else {
                        Ok(None)
                    }
                }
            }
        })
    }
    /// Runs on the page-zero owner. Resolve metadata, then acquire page zero via
    /// the same Fill as pinned reads. Require identical version AND total length;
    /// bounded version-change retries happen before any response headers escape.
    /// Missing pinned descriptors may use owners.retained_metadata before probing
    /// peers/origin; fresh admission must still revalidate an absent/expired pointer.
    pub fn bootstrap<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, BootstrapResult> {
        Box::pin(async move {
            let mut budget =
                AcquisitionBudget::new(scope.deadline.0, DEFAULT_ATTEMPTS, DEFAULT_LINKS);
            self.bootstrap_with_budget(selector, membership, context, scope, &mut budget)
                .await
        })
    }

    pub fn bootstrap_with_budget<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, BootstrapResult> {
        Box::pin(async move {
            let mut bounded_scope = scope.clone();
            bounded_scope.deadline.0 = scope.deadline.0.min(budget.deadline());
            let scope = &bounded_scope;
            for attempt in 0..MAX_BOOTSTRAP_ATTEMPTS {
                scope.check()?;
                let resolved = match self
                    .resolve_inner(
                        selector.clone(),
                        membership.clone(),
                        context,
                        scope,
                        budget,
                        attempt != 0,
                        true,
                    )
                    .await
                {
                    Err(Error::VersionUnavailable)
                        if matches!(selector, MetadataSelector::Fresh) =>
                    {
                        continue;
                    }
                    result => result?,
                };
                let metadata = resolved.metadata;
                if metadata.length == 0 {
                    return Ok(BootstrapResult::Empty(metadata));
                }
                let page = PageId {
                    version: metadata.version.clone(),
                    number: PageNumber(0),
                };
                let result = match resolved.page {
                    Some(page) => Ok(page),
                    None => {
                        self.storage
                            .fill
                            .acquire(page.clone(), membership.clone(), context, scope, budget)
                            .await
                    }
                };
                scope.check()?;
                let result = result.and_then(|mut result| {
                    validate_bootstrap_metadata(&metadata, &result.metadata)?;
                    result.validate_for(&page)?;
                    // Preserve the fresh admission's expiry, not a page copy's
                    // historical expiry, alongside both full-page buffers.
                    result.metadata = metadata;
                    Ok(result)
                });
                match result {
                    Ok(page) => return Ok(BootstrapResult::Page(page)),
                    Err(Error::VersionUnavailable)
                        if matches!(selector, MetadataSelector::Fresh) =>
                    {
                        continue;
                    }
                    Err(error) => return Err(error),
                }
            }
            Err(Error::VersionUnavailable)
        })
    }
}

fn validate_metadata(
    object: &ObjectId,
    selector: &MetadataSelector,
    metadata: &ObjectMetadata,
) -> Result<()> {
    if &metadata.version.object != object {
        return Err(Error::CorruptRecord);
    }
    if let MetadataSelector::Pinned(etag) = selector {
        if &metadata.version.etag != etag {
            return Err(Error::VersionUnavailable);
        }
    }
    Ok(())
}

fn validate_bootstrap_metadata(expected: &ObjectMetadata, actual: &ObjectMetadata) -> Result<()> {
    if expected.version.object != actual.version.object {
        return Err(Error::CorruptRecord);
    }
    if expected.version != actual.version {
        return Err(Error::VersionUnavailable);
    }
    if expected.length != actual.length {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{
        identity::{CacheId, CacheKey, WorkerId},
        metadata::ExpiresAt,
    };
    use futures::task::noop_waker;
    use std::time::UNIX_EPOCH;

    fn metadata(etag: &str, length: u64) -> ObjectMetadata {
        ObjectMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
            expires_at: ExpiresAt(UNIX_EPOCH),
        }
    }
    fn key() -> RefreshKey {
        RefreshKey::new(&metadata("v1", 0).version.object, &MetadataSelector::Fresh)
    }
    fn output() -> RefreshOutput {
        RefreshOutput {
            metadata: metadata("v1", 0),
            page: None,
        }
    }

    #[test]
    fn worker_driver_retains_leadership_after_request_drop_until_completion() {
        let table = Rc::new(RefreshTable::default());
        let request = table.join(key(), 1).unwrap();
        let follower = table.join(key(), 1).unwrap();
        let waker = noop_waker();
        assert!(matches!(
            request.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
        let driver = request.clone();
        let (complete, fence) = futures::channel::oneshot::channel::<()>();
        super::super::drivers::spawn(Box::pin(async move {
            fence.await.map_err(|_| Error::Io)?;
            driver.retry();
            Ok(())
        }))
        .unwrap();
        let mut cx = std::task::Context::from_waker(&waker);
        super::super::drivers::poll(&mut cx, 64);
        drop(request);
        assert!(follower.event(&waker).is_pending());
        assert_eq!(table.registrations.get(), 2);
        complete.send(()).unwrap();
        super::super::drivers::poll(&mut cx, 64);
        assert_eq!(table.registrations.get(), 1);
        assert!(matches!(
            follower.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
    }

    #[test]
    fn coalesces_refresh_and_zero_ttl_admits_only_registered_cohort() {
        let table = Rc::new(RefreshTable::default());
        let first = table.join(key(), 1).unwrap();
        let second = table.join(key(), 1).unwrap();
        let waker = noop_waker();
        assert!(matches!(
            first.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
        assert!(second.event(&waker).is_pending());
        first.finish(Ok(output()));
        let Poll::Ready(RefreshEvent::Complete(Ok(result))) = second.event(&waker) else {
            panic!("cohort did not complete")
        };
        assert_eq!(result.metadata.length, 0);
        assert_eq!(result.metadata.expires_at.0, UNIX_EPOCH);
        let next = table.join(key(), 1).unwrap();
        assert!(!Rc::ptr_eq(&first.refresh, &next.refresh));
        assert!(matches!(
            next.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
        drop(first);
        drop(second);
        // A stale cohort's final detach must not remove the replacement.
        assert_eq!(table.active.borrow().len(), 1);
        drop(next);
        assert_eq!(table.registrations.get(), 0);
        assert!(table.active.borrow().is_empty());
    }

    #[test]
    fn rejected_or_canceled_leader_does_not_fail_other_callers() {
        let table = Rc::new(RefreshTable::default());
        let first = table.join(key(), 1).unwrap();
        let second = table.join(key(), 1).unwrap();
        let waker = noop_waker();
        assert!(matches!(
            first.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
        assert!(second.event(&waker).is_pending());
        drop(first);
        assert!(matches!(
            second.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
        second.finish(Ok(output()));
        assert!(matches!(
            second.event(&waker),
            Poll::Ready(RefreshEvent::Complete(Ok(_)))
        ));
        for error in [Error::OriginRejected, Error::OriginForbidden] {
            assert!(
                matches!(RefreshFailure::from(error), RefreshFailure::Rejected(actual) if actual == error)
            );
        }
        assert!(matches!(
            RefreshFailure::from(Error::Unauthorized),
            RefreshFailure::Terminal(Error::Unauthorized)
        ));
    }

    #[test]
    fn exhausted_supplier_credits_never_turn_protocol_failure_into_retry() {
        let now = Instant::now();
        let deadline = now + Duration::from_secs(10);
        let mut supplier = AcquisitionBudget::new(deadline, 1, 0);
        let mut next = AcquisitionBudget::new(deadline, 1, 1);
        supplier.begin_attempt(now, deadline).unwrap();
        assert!(caller_budget_failure(Error::Unavailable, &supplier));
        for error in [Error::Unauthorized, Error::CorruptRecord, Error::Replay] {
            assert!(!caller_budget_failure(error, &supplier));
        }
        assert!(caller_budget_failure(Error::DeadlineExceeded, &supplier));
        // Election never borrows the failed supplier's credits or extends either
        // request's deadline. The next caller spends only its own original budget.
        assert_eq!(next.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(
            supplier.begin_attempt(now, deadline),
            Err(Error::Unavailable)
        );
        assert_eq!(next.begin_attempt(now, deadline), Err(Error::Unavailable));
    }

    #[test]
    fn cohort_retries_entries_and_live_registrations_are_bounded() {
        let table = Rc::new(RefreshTable::default());
        assert!(matches!(table.join(key(), 0), Err(Error::Overloaded)));
        let observer = table.join(key(), 1).unwrap();
        let waker = noop_waker();
        for _ in 0..MAX_REFRESH_ATTEMPTS {
            let leader = table.join(key(), 1).unwrap();
            assert!(matches!(
                leader.event(&waker),
                Poll::Ready(RefreshEvent::Lead)
            ));
            drop(leader);
        }
        assert!(matches!(
            observer.event(&waker),
            Poll::Ready(RefreshEvent::Complete(Err(Error::Unavailable)))
        ));
        drop(observer);
        let waiters: Vec<_> = (0..MAX_WAITERS)
            .map(|_| table.join(key(), 1).unwrap())
            .collect();
        assert!(matches!(table.join(key(), 1), Err(Error::Overloaded)));
        waiters[0].finish(Ok(output()));
        // Completed readers also count against the global registration quota.
        assert!(matches!(table.join(key(), 1), Err(Error::Overloaded)));
        drop(waiters);
        let first = table.join(key(), 1).unwrap();
        let different = RefreshKey::new(
            &key().object,
            &MetadataSelector::Pinned(StrongEtag::test_value("old")),
        );
        assert!(matches!(table.join(different, 1), Err(Error::Overloaded)));
        drop(first);
    }

    #[test]
    fn terminal_failure_is_shared_but_never_negative_cached() {
        let table = Rc::new(RefreshTable::default());
        let first = table.join(key(), 1).unwrap();
        let second = table.join(key(), 1).unwrap();
        first.finish(Err(Error::CorruptRecord));
        assert!(matches!(
            second.event(&noop_waker()),
            Poll::Ready(RefreshEvent::Complete(Err(Error::CorruptRecord)))
        ));
        let next = table.join(key(), 1).unwrap();
        assert!(matches!(
            next.event(&noop_waker()),
            Poll::Ready(RefreshEvent::Lead)
        ));
    }

    #[test]
    fn clock_discontinuities_invalidate_freshness_but_preserve_old_descriptors() {
        let mut clock = Clock::default();
        let wall = UNIX_EPOCH + Duration::from_secs(100);
        let mono = Instant::now();
        assert!(!clock.observe(wall, mono));
        assert!(!clock.observe(wall + Duration::from_secs(2), mono + Duration::from_secs(2)));
        assert!(clock.observe(wall, mono + Duration::from_secs(3)));
        assert!(clock.observe(
            wall + Duration::from_secs(100),
            mono + Duration::from_secs(4)
        ));
        assert_eq!(clock.epoch, 2);
        let index = Index::new(WorkerId(0), 4);
        let old = metadata("old", 7);
        let mut new = metadata("new", 900);
        new.expires_at = ExpiresAt(SystemTime::now() + Duration::from_secs(60));
        index.publish_version(old.immutable()).unwrap();
        index.publish_current(new.clone()).unwrap();
        assert_eq!(
            index.current(&old.version.object).unwrap().unwrap().version,
            new.version
        );
        index.invalidate_freshness().unwrap();
        assert!(index.current(&old.version.object).unwrap().is_none());
        assert_eq!(index.version(&old.version).unwrap().unwrap().for_pin(), old);
        assert_eq!(index.version(&new.version).unwrap().unwrap().length, 900);
    }

    #[test]
    fn immutable_pin_and_bootstrap_require_exact_version_and_total_length() {
        let expected = metadata("old", 17);
        assert_eq!(validate_bootstrap_metadata(&expected, &expected), Ok(()));
        let mut wrong = expected.clone();
        wrong.length += 1;
        assert_eq!(
            validate_bootstrap_metadata(&expected, &wrong),
            Err(Error::CorruptRecord)
        );
        wrong = metadata("new", 17);
        assert_eq!(
            validate_bootstrap_metadata(&expected, &wrong),
            Err(Error::VersionUnavailable)
        );
        let selector = MetadataSelector::Pinned(expected.version.etag.clone());
        assert_eq!(
            validate_metadata(&expected.version.object, &selector, &wrong),
            Err(Error::VersionUnavailable)
        );
        wrong.version.object.key = CacheKey([1; 32]);
        assert_eq!(
            validate_metadata(&expected.version.object, &MetadataSelector::Fresh, &wrong),
            Err(Error::CorruptRecord)
        );
        assert_eq!(
            validate_bootstrap_metadata(&expected, &wrong),
            Err(Error::CorruptRecord)
        );
        assert_ne!(RefreshKey::new(&expected.version.object, &selector), key());
    }
}
