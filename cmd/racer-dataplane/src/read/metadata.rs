//! Versioned metadata cache and page-zero-worker refresh singleflight.
//!
//! Keep old lengths separately from the current-version pointer. TTL gates fresh
//! admission; explicit pins may use expired metadata. A zero-TTL refresh admits its
//! waiters once. Clock discontinuities invalidate uncertain freshness. Cache entries
//! contain no origin context, Authorization, or opaque adapter metadata header.

use super::candidates::CandidatePolicy;
use super::candidates::CandidateResolution;
use super::flight::AcquisitionBudget;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::StrongEtag;
use crate::origin::Origin;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerResponse;
use crate::runtime::RequestScope;
use crate::security::CredentialCrypto;
use crate::security::OriginContext;
use crate::store::catalog::Index;
use flow_control::coalesce;
use std::cell::RefCell;
use std::future::Future;
use std::future::poll_fn;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Poll;
use std::task::Waker;
use std::time::Duration;
use std::time::Instant;
use std::time::SystemTime;

const MAX_WAITERS: usize = 64;
const MAX_REFRESH_ATTEMPTS: usize = 8;

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct RefreshKey {
    object: ObjectId,
    pin: Option<StrongEtag>,
    bootstrap: bool,
}
impl RefreshKey {
    fn new(object: &ObjectId, selector: &MetadataSelector) -> Self {
        Self {
            object: object.clone(),
            bootstrap: false,
            pin: match selector {
                MetadataSelector::Fresh => None,
                MetadataSelector::Pinned(etag) => Some(etag.clone()),
            },
        }
    }
}

#[derive(Clone)]
struct RefreshOutput {
    metadata: ObjectMetadata,
    page: Option<crate::memory::AcquiredPage>,
}
// Match runtime::HashMap's cfg exactly: dependency simulation features must not
// switch production or integration-test builds to seeded hashing.
#[cfg(not(test))]
type RefreshHashState = std::collections::hash_map::RandomState;
#[cfg(test)]
type RefreshHashState = crate::runtime::HashState;
type Cohorts = coalesce::Table<RefreshKey, Result<RefreshOutput>, RefreshHashState>;
type Registration = coalesce::Registration<RefreshKey, Result<RefreshOutput>, RefreshHashState>;
type RefreshEvent = coalesce::Event<Result<RefreshOutput>>;

/// Only credential-free coordination lives here. Context, membership and credits
/// remain request-owned or in its admitted worker driver, never in this table.
struct RefreshTable {
    inner: Rc<Cohorts>,
}
impl Default for RefreshTable {
    fn default() -> Self {
        Self {
            inner: Rc::new(Cohorts::new(
                coalesce::Limits {
                    waiters_per_cohort: MAX_WAITERS,
                    attempts_per_cohort: MAX_REFRESH_ATTEMPTS,
                },
                Err(Error::Unavailable),
            )),
        }
    }
}
impl RefreshTable {
    fn join(&self, key: RefreshKey, capacity: usize) -> Result<Registration> {
        self.inner
            .join(key, capacity)
            .map_err(|_| Error::Overloaded)
    }
}

#[derive(Default)]
struct IngressDeadlines {
    inner: Rc<uring_runtime::environment::Registry>,
}
/// One entry per admitted resolve, shared across its follower and leader waits.
/// The refresh registration quota bounds these entries. Drivers never own this
/// guard: ingress expiry must not release their acquisition or completion fences.
struct IngressDeadline {
    inner: uring_runtime::environment::Registration,
}
impl IngressDeadlines {
    fn register(self: &Rc<Self>, deadline: Instant) -> Result<IngressDeadline> {
        Ok(IngressDeadline {
            inner: self.inner.register(deadline).map_err(Error::from)?,
        })
    }

    fn poll(&self, now: Instant, budget: usize) -> usize {
        self.inner.poll(now, budget)
    }
}
impl IngressDeadline {
    fn check(&self, scope: &RequestScope, waker: &Waker) -> Result<()> {
        scope.check()?;
        match self
            .inner
            .poll_expired(&mut std::task::Context::from_waker(waker))
        {
            Poll::Ready(()) => Err(Error::DeadlineExceeded),
            Poll::Pending => Ok(()),
        }
    }
}

#[derive(Default)]
struct Clock {
    inner: uring_runtime::environment::Observer,
}
impl Clock {
    fn observe(&mut self, wall: SystemTime, monotonic: Instant) -> bool {
        self.inner.observe(wall, monotonic, Duration::from_secs(1))
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
    deadlines: Rc<IngressDeadlines>,
    clock: Rc<RefCell<Clock>>,
}
impl MetadataService {
    /// Page completion handoff: immutable facts only, never fresh admission.
    pub fn publish_version(&self, metadata: crate::model::VersionMetadata) -> Result<()> {
        self.storage.index.publish_version(metadata)
    }

    pub fn new(
        candidates: Rc<CandidatePolicy>,
        origin: Rc<dyn Origin>,
        credentials: Rc<CredentialCrypto>,
        capacity: usize,
        storage: MetadataDependencies,
    ) -> Self {
        Self {
            candidates,
            origin,
            credentials,
            capacity,
            storage,
            refreshes: Rc::new(RefreshTable::default()),
            deadlines: Rc::new(IngressDeadlines::default()),
            clock: Rc::new(RefCell::new(Clock::default())),
        }
    }
    /// Wake actual ingress children, including those parked in FuturesUnordered.
    /// The worker tick calls this during normal polling and shutdown drain.
    pub(crate) fn poll_deadlines(&self, now: Instant, budget: usize) -> usize {
        self.deadlines.poll(now, budget)
    }

    pub(crate) fn next_deadline(&self) -> Option<Instant> {
        self.deadlines.inner.next_deadline()
    }

    /// Missing pinned metadata may require a conditional page-zero probe; never
    /// substitute current-version length to resolve a suffix or final-page range.
    pub fn resolve<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, ObjectMetadata> {
        Box::pin(async move {
            let mut budget = super::default_budget(scope);
            self.resolve_with_budget(selector, membership, context, scope, &mut budget)
                .await
        })
    }

    pub fn resolve_with_budget<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, ObjectMetadata> {
        Box::pin(async move {
            Ok(self
                .resolve_inner(selector, membership, context, scope, budget, false)
                .await?
                .metadata)
        })
    }

    fn observe_clock(&self) -> Result<SystemTime> {
        let now = uring_runtime::environment::wall_now();
        if self
            .clock
            .borrow_mut()
            .observe(now, uring_runtime::environment::now())
        {
            self.storage.index.invalidate_freshness()?;
        }
        Ok(now)
    }

    fn resolve_inner<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        bootstrap: bool,
    ) -> Operation<'a, RefreshOutput> {
        Box::pin(async move {
            let mut bounded_scope = scope.clone();
            bounded_scope.deadline.0 = scope.deadline.0.min(budget.deadline());
            let scope = &bounded_scope;
            scope.check()?;
            if let Some(metadata) = self.copy_only(selector.clone(), context, scope).await? {
                return Ok(RefreshOutput {
                    metadata,
                    page: None,
                });
            }
            let mut key = RefreshKey::new(&context.object, &selector);
            // HEAD cannot join a body-fetching bootstrap leader or elect one.
            key.bootstrap = bootstrap;
            let registration = self.refreshes.join(key, self.capacity)?;
            let deadline = self.deadlines.register(scope.deadline.0)?;
            let cancellation = scope.cancellation.subscribe()?;
            let event = poll_fn(|cx| {
                uring_runtime::drivers::poll(cx, 64);
                cancellation.register(cx.waker());
                if let Err(error) = deadline.check(scope, cx.waker()) {
                    return Poll::Ready(Err(error));
                }
                match registration.event(cx.waker()) {
                    Poll::Ready(event) => Poll::Ready(Ok(event)),
                    // Completion/re-election, cancellation, and the worker's
                    // metadata deadline hook notify this specific child.
                    Poll::Pending => Poll::Pending,
                }
            })
            .await?;
            match event {
                RefreshEvent::Complete(result) => result,
                RefreshEvent::Lead => {
                    if budget.remaining_attempts() == 0 {
                        return Err(Error::Unavailable);
                    }
                    let driver_permit = uring_runtime::drivers::reserve().map_err(Error::from)?;
                    let mut nonce = [0; 16];
                    uring_runtime::environment::fill_random(&mut nonce)
                        .map_err(|_| Error::Unavailable)?;
                    let attempt = crate::model::AttemptId(nonce);
                    let sealed = self.credentials.seal(context, attempt, scope)?;
                    let owned_context =
                        self.credentials
                            .open_charged(sealed, scope.request, attempt)?;
                    let service = self.clone();
                    let driver_registration = registration.clone();
                    // Detaching one refresh must not cancel the shared peer
                    // ingress scope. Parent cancellation still reaches its work.
                    let caller_scope = scope.clone();
                    let caller_cancellation = caller_scope.cancellation.subscribe()?;
                    let owned_scope =
                        RequestScope::new(caller_scope.request, caller_scope.deadline.0)?;
                    let mut owned_budget = budget.transfer();
                    let (send, mut receive) = futures::channel::oneshot::channel();
                    driver_permit.submit_detached(Box::pin(async move {
                        let mut work = Box::pin(service.refresh(
                            selector,
                            membership,
                            &owned_context,
                            &owned_scope,
                            &mut owned_budget,
                            bootstrap,
                        ));
                        let result = poll_fn(|cx| {
                            if !caller_scope.cancellation.is_cancelled() {
                                caller_cancellation.register(cx.waker());
                            }
                            if !owned_scope.cancellation.is_cancelled()
                                && (caller_scope.check().is_err()
                                    || driver_registration.is_only_handle())
                            {
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
                        Ok::<_, Error>(())
                    }));
                    let (result, remaining) = poll_fn(|cx| {
                        cancellation.register(cx.waker());
                        deadline.check(scope, cx.waker())?;
                        uring_runtime::drivers::poll(cx, 64);
                        match Pin::new(&mut receive).poll(cx) {
                            Poll::Ready(Ok(value)) => Poll::Ready(Ok(value)),
                            Poll::Ready(Err(_)) => Poll::Ready(Err(Error::Unavailable)),
                            Poll::Pending => {
                                if let Err(error) = scope.check() {
                                    return Poll::Ready(Err(error));
                                }
                                // The same ingress deadline guard spans both waits.
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
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        bootstrap: bool,
    ) -> std::result::Result<RefreshOutput, RefreshFailure> {
        scope.check()?;
        self.observe_clock()?;
        let clock_epoch = self.clock.borrow().inner.epoch();
        let candidates = self
            .candidates
            .candidates_scoped(membership.clone(), &context.object, PageNumber(0), scope)
            .await?;
        let operation = if bootstrap && matches!(selector, MetadataSelector::Fresh) {
            PeerOperation::Bootstrap {
                object: context.object.clone(),
                mode: FetchMode::Acquire,
            }
        } else {
            PeerOperation::Metadata {
                object: context.object.clone(),
                selector: selector.clone(),
                mode: FetchMode::Acquire,
            }
        };
        let mut bootstrap_page = None;
        let metadata = match self
            .candidates
            .resolve_with_budget(candidates, context, operation, scope, budget)
            .await?
        {
            CandidateResolution::Copy(response) => match response.response() {
                PeerResponse::Bootstrap {
                    metadata,
                    page_zero,
                } if bootstrap => {
                    if let Some(ciphertext) = page_zero {
                        bootstrap_page = Some(crate::memory::AcquiredPage::Ciphertext(
                            crate::memory::UnverifiedPage {
                                copy: crate::memory::CiphertextCopy {
                                    metadata: metadata.clone(),
                                    ciphertext: ciphertext.clone(),
                                },
                                disk_token: None,
                            },
                        ));
                    }
                    metadata.clone()
                }
                PeerResponse::Bootstrap { .. } => return Err(Error::CorruptRecord.into()),
                PeerResponse::Metadata(metadata) => metadata.clone(),
                PeerResponse::OriginRejected => {
                    return Err(RefreshFailure::Rejected(Error::OriginRejected));
                }
                PeerResponse::OriginForbidden => {
                    return Err(RefreshFailure::Rejected(Error::OriginForbidden));
                }
                PeerResponse::VersionUnavailable => return Err(Error::VersionUnavailable.into()),
                PeerResponse::NotFound => return Err(Error::NotFound.into()),
                PeerResponse::Overloaded => return Err(Error::Overloaded.into()),
                PeerResponse::Miss | PeerResponse::Unavailable => {
                    return Err(Error::Unavailable.into());
                }
                PeerResponse::Page { .. } | PeerResponse::Selected { .. } => {
                    return Err(Error::CorruptRecord.into());
                }
                PeerResponse::StaleMembership => return Err(Error::IncompatibleMembership.into()),
            },
            CandidateResolution::Origin(authority) => {
                let output = self
                    .refresh_origin(
                        &authority, &selector, membership, context, scope, budget, bootstrap,
                    )
                    .await?;
                bootstrap_page = output.page;
                output.metadata
            }
        };
        scope.check()?;
        super::validate_metadata(&metadata, &context.object, &selector)?;
        self.observe_clock()?;
        if matches!(selector, MetadataSelector::Fresh)
            && self.clock.borrow().inner.epoch() == clock_epoch
        {
            self.storage.index.publish_current(metadata.clone())?;
        } else {
            self.storage.index.publish_version(metadata.immutable())?;
        }
        Ok(RefreshOutput {
            metadata,
            page: bootstrap_page,
        })
    }

    /// Acquire and authenticate page zero before publishing any fresh observation.
    /// Refresh origin metadata under the selected authority and acquisition budget.
    #[allow(clippy::too_many_arguments)] // Keep bootstrap policy and borrowed request inputs explicit.
    async fn refresh_origin(
        &self,
        authority: &super::candidates::OriginAuthority,
        selector: &MetadataSelector,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        bootstrap: bool,
    ) -> std::result::Result<RefreshOutput, RefreshFailure> {
        authority.validate(&context.object, PageNumber(0))?;
        budget.begin_attempt(uring_runtime::environment::now(), scope.deadline.0)?;
        let acquisition = if bootstrap && matches!(selector, MetadataSelector::Fresh) {
            let reservation = self.storage.fill.reserve_bootstrap(&context.object.cache)?;
            self.origin
                .bootstrap_reserved(authority, context, reservation, scope)
                .await
        } else {
            self.origin
                .metadata(authority, context, selector.clone(), scope)
                .await
        };
        let mut reply = match acquisition {
            Ok(reply) => reply,
            Err(Error::VersionUnavailable) if matches!(selector, MetadataSelector::Pinned(_)) => {
                self.copy_after_origin_miss(
                    authority,
                    selector,
                    membership.clone(),
                    context,
                    scope,
                    budget,
                )
                .await?
            }
            Err(error) => return Err(error.into()),
        };
        if let Some(page) = &reply.page_zero {
            validate_bootstrap_metadata(&reply.metadata, &page.metadata)?;
            if reply.metadata.content_type.is_none() {
                reply.metadata.content_type = page.metadata.content_type.clone();
            }
            if reply.metadata.length == 0 {
                return Err(Error::CorruptRecord.into());
            }
        }
        super::validate_metadata(&reply.metadata, &context.object, selector)?;
        let page = if let Some(page) = reply.page_zero {
            let result = self
                .storage
                .fill
                .publish_bootstrap_with_context(page, membership, context, scope, budget)
                .await?;
            validate_bootstrap_metadata(&reply.metadata, &result.metadata)?;
            if reply.metadata.content_type.is_none() {
                reply.metadata.content_type = result.metadata.content_type.clone();
            }
            result.validate_for(&PageId {
                version: reply.metadata.version.clone(),
                number: PageNumber(0),
            })?;
            Some(result.into())
        } else {
            None
        };
        Ok(RefreshOutput {
            metadata: reply.metadata,
            page,
        })
    }

    /// A conditional origin miss says nothing about immutable copies on later
    /// candidates. Probe them copy-only before reporting the pin unavailable.
    async fn copy_after_origin_miss(
        &self,
        authority: &super::candidates::OriginAuthority,
        selector: &MetadataSelector,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<crate::origin::MetadataReply> {
        let candidates = self
            .candidates
            .candidates_scoped(membership, &context.object, PageNumber(0), scope)
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
            .ok_or_else(|| {
                self.candidates
                    .final_origin_miss(authority, &operation, scope, budget)
            })?;
        match response.response() {
            PeerResponse::Metadata(metadata) => Ok(crate::origin::MetadataReply {
                metadata: metadata.clone(),
                page_zero: None,
            }),
            _ => Err(Error::CorruptRecord),
        }
    }
    pub(crate) async fn bootstrap_copy(
        &self,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<PeerResponse> {
        let Some(metadata) = self
            .copy_only(MetadataSelector::Fresh, context, scope)
            .await?
        else {
            return Ok(PeerResponse::Miss);
        };
        if metadata.length == 0 {
            return Ok(PeerResponse::Bootstrap {
                metadata,
                page_zero: None,
            });
        }
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        };
        match self.storage.fill.copy_only(&page, scope).await? {
            Some((descriptor, ciphertext)) => {
                validate_bootstrap_metadata(&metadata, &descriptor)?;
                Ok(PeerResponse::Bootstrap {
                    metadata,
                    page_zero: Some(ciphertext),
                })
            }
            None => Ok(PeerResponse::Miss),
        }
    }

    pub(crate) async fn bootstrap_peer(
        &self,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PeerResponse> {
        if let response @ PeerResponse::Bootstrap { .. } =
            self.bootstrap_copy(context, scope).await?
        {
            return Ok(response);
        }
        let mut output = self
            .resolve_inner(
                MetadataSelector::Fresh,
                membership.clone(),
                context,
                scope,
                budget,
                true,
            )
            .await?;
        if output.metadata.length == 0 {
            return Ok(PeerResponse::Bootstrap {
                metadata: output.metadata,
                page_zero: None,
            });
        }
        let ciphertext = match output.page {
            Some(page) => page.copy().ciphertext,
            None => {
                let copy = self
                    .storage
                    .fill
                    .acquire_ciphertext(
                        PageId {
                            version: output.metadata.version.clone(),
                            number: PageNumber(0),
                        },
                        membership,
                        context,
                        scope,
                        budget,
                    )
                    .await?;
                validate_bootstrap_metadata(&output.metadata, &copy.metadata)?;
                if output.metadata.content_type.is_none() {
                    output.metadata.content_type = copy.metadata.content_type;
                }
                copy.ciphertext
            }
        };
        output
            .metadata
            .immutable()
            .validate_page(ciphertext.envelope())?;
        Ok(PeerResponse::Bootstrap {
            metadata: output.metadata,
            page_zero: Some(ciphertext),
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
                    current.resolve(&descriptor, now).map_err(Into::into)
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
}

fn validate_bootstrap_metadata(expected: &ObjectMetadata, actual: &ObjectMetadata) -> Result<()> {
    if expected.version.object != actual.version.object {
        return Err(Error::CorruptRecord);
    }
    if expected.version != actual.version {
        return Err(Error::VersionUnavailable);
    }
    if !expected.immutable().compatible(&actual.immutable()) {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::ExpiresAt;
    use crate::model::WorkerId;
    use futures::task::noop_waker;
    use racer_control_wire::CacheId;
    use std::time::UNIX_EPOCH;

    fn metadata(etag: &str, length: u64) -> ObjectMetadata {
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(crate::test_support::security::CACHE.into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
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

    pub(crate) fn deadline_probe(
        service: &MetadataService,
        due: Instant,
    ) -> Operation<'static, ()> {
        let deadline = service.deadlines.register(due).unwrap();
        let scope = RequestScope::new(
            crate::model::RequestId([0; 16]),
            Instant::now() + Duration::from_secs(600),
        )
        .unwrap();
        Box::pin(poll_fn(move |cx| {
            match deadline.check(&scope, cx.waker()) {
                Ok(()) => Poll::Pending,
                Err(error) => Poll::Ready(Err(error)),
            }
        }))
    }

    pub(crate) fn assert_ingress_counts(
        service: &MetadataService,
        deadlines: usize,
        refreshes: usize,
    ) {
        assert_eq!(service.deadlines.inner.len(), deadlines);
        assert_eq!(service.refreshes.inner.registration_count(), refreshes);
    }

    #[test]
    fn ingress_deadlines_are_ordered_budgeted_and_wake_latest_child_once() {
        let table = Rc::new(IngressDeadlines::default());
        let now = Instant::now();
        let due = now + Duration::from_secs(60);
        let scope = RequestScope::new(crate::model::RequestId([0; 16]), due).unwrap();
        let old = Arc::new(crate::test_support::WakeCounter::default());
        let latest = Arc::new(crate::test_support::WakeCounter::default());
        let later = table.register(due + Duration::from_secs(1)).unwrap();
        let first = table.register(due).unwrap();
        let second = table.register(due).unwrap();
        first.check(&scope, &Waker::from(old.clone())).unwrap();
        first.check(&scope, &Waker::from(latest.clone())).unwrap();
        second.check(&scope, &Waker::from(latest.clone())).unwrap();
        later.check(&scope, &Waker::from(latest.clone())).unwrap();
        assert_eq!(table.poll(due - Duration::from_nanos(1), 64), 0);
        assert_eq!(table.poll(due, 0), 0);
        assert_eq!(latest.count(), 0);
        assert_eq!(table.poll(due, 1), 1);
        assert_eq!(old.count(), 0);
        assert_eq!(latest.count(), 1);
        assert_eq!(
            first.check(&scope, &noop_waker()),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(second.check(&scope, &Waker::from(latest.clone())), Ok(()));
        assert_eq!(table.poll(due, 1), 1);
        assert_eq!(latest.count(), 2);
        assert_eq!(table.poll(due, 64), 0);
        assert_eq!(latest.count(), 2);
        assert_eq!(table.inner.len(), 1);
        drop(later);
        assert!(table.inner.is_empty());
        assert_eq!(table.poll(due + Duration::from_secs(1), 64), 0);
    }

    #[test]
    fn ingress_deadline_drop_reclaims_entries_and_refresh_capacity_without_stale_keys() {
        let table = Rc::new(IngressDeadlines::default());
        let refreshes = Rc::new(RefreshTable::default());
        let due = Instant::now() + Duration::from_secs(60);
        for _ in 0..2 * MAX_WAITERS {
            let registration = refreshes.join(key(), 1).unwrap();
            let deadline = table.register(due).unwrap();
            assert_eq!(table.inner.len(), 1);
            registration.finish(Ok(output()));
            drop(deadline);
            drop(registration);
            assert!(table.inner.is_empty());
            assert_eq!(refreshes.inner.registration_count(), 0);
        }
        let entries: Vec<_> = (0..MAX_WAITERS)
            .map(|_| {
                (
                    refreshes.join(key(), 1).unwrap(),
                    table.register(due).unwrap(),
                )
            })
            .collect();
        assert!(matches!(refreshes.join(key(), 1), Err(Error::Overloaded)));
        assert_eq!(table.inner.len(), MAX_WAITERS);
        drop(entries);
        assert!(table.inner.is_empty());
        assert_eq!(refreshes.inner.active_count(), 0);
        assert_eq!(refreshes.inner.registration_count(), 0);
        let table = Rc::new(IngressDeadlines {
            inner: Rc::new(uring_runtime::environment::Registry::with_next_id(u64::MAX)),
        });
        assert!(matches!(table.register(due), Err(Error::Overloaded)));
        assert!(table.inner.is_empty());
    }

    #[test]
    fn refresh_notifications_cover_registration_completion_and_reelection_order() {
        for complete_first in [false, true] {
            let table = Rc::new(RefreshTable::default());
            let leader = table.join(key(), 1).unwrap();
            let follower = table.join(key(), 1).unwrap();
            let count = Arc::new(crate::test_support::WakeCounter::default());
            let waker = Waker::from(count.clone());
            assert!(matches!(
                leader.event(&noop_waker()),
                Poll::Ready(RefreshEvent::Lead)
            ));
            if !complete_first {
                assert!(follower.event(&waker).is_pending());
                assert_eq!(count.count(), 0);
            }
            leader.finish(Ok(output()));
            assert_eq!(count.count(), usize::from(!complete_first));
            assert!(matches!(
                follower.event(&waker),
                Poll::Ready(RefreshEvent::Complete(Ok(_)))
            ));
        }
        let table = Rc::new(RefreshTable::default());
        let leader = table.join(key(), 1).unwrap();
        let follower = table.join(key(), 1).unwrap();
        let count = Arc::new(crate::test_support::WakeCounter::default());
        let waker = Waker::from(count.clone());
        assert!(matches!(
            leader.event(&noop_waker()),
            Poll::Ready(RefreshEvent::Lead)
        ));
        assert!(follower.event(&waker).is_pending());
        leader.retry();
        assert_eq!(count.count(), 1);
        assert!(matches!(
            follower.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
    }

    #[test]
    fn worker_driver_retains_leadership_after_request_drop_until_completion() {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _owner = queue.enter();
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
        uring_runtime::drivers::reserve()
            .unwrap()
            .submit_detached(Box::pin(async move {
                fence.await.map_err(|_| Error::Io)?;
                driver.retry();
                Ok::<_, Error>(())
            }));
        let mut cx = std::task::Context::from_waker(&waker);
        uring_runtime::drivers::poll(&mut cx, 64);
        drop(request);
        assert!(follower.event(&waker).is_pending());
        assert_eq!(table.inner.registration_count(), 2);
        complete.send(()).unwrap();
        uring_runtime::drivers::poll(&mut cx, 64);
        assert_eq!(table.inner.registration_count(), 1);
        assert!(matches!(
            follower.event(&waker),
            Poll::Ready(RefreshEvent::Lead)
        ));
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
        assert_eq!(clock.inner.epoch(), 2);
        let index = Index::new(WorkerId(0), 4, crate::test_support::availability());
        let old = metadata("old", 7);
        let mut new = metadata("new", 900);
        new.expires_at = ExpiresAt::test_time(SystemTime::now() + Duration::from_secs(60));
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
            crate::read::validate_metadata(&wrong, &expected.version.object, &selector),
            Err(Error::VersionUnavailable)
        );
        wrong.version.object.key = CacheKey([1; 32]);
        assert_eq!(
            crate::read::validate_metadata(
                &wrong,
                &expected.version.object,
                &MetadataSelector::Fresh
            ),
            Err(Error::CorruptRecord)
        );
        assert_eq!(
            validate_bootstrap_metadata(&expected, &wrong),
            Err(Error::CorruptRecord)
        );
        assert_ne!(RefreshKey::new(&expected.version.object, &selector), key());
    }
}
