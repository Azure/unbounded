//! Bounded node-local handoffs. Only owned commands and immutable results cross
//! threads; the coordinator, futures, delivery leases, and streams stay local.
//! Stable local page assignment is independent of cluster placement. Drain flights
//! before changing the worker map; no live remapping is implied.

use super::Coordinator;
use super::flight::AcquisitionBudget;
use super::warm::WarmWait;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::PageResult;
use crate::model::AttemptId;
use crate::model::MetadataSelector;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::peer::forwarding::VerifiedRequest;
use crate::peer::protocol::PeerResponse;
use crate::peer::server::LocalPageService;
use crate::runtime::Cancellation;
use crate::runtime::HashMap;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use crate::security::PeerOriginContext;
use crate::store::holds::{HoldEpoch, HoldPages, HolderKind};
use sha2::Digest;
use sha2::Sha256;
use std::cell::RefCell;
#[cfg(test)]
use std::future::Future;
#[cfg(test)]
use std::pin::Pin;
use std::rc::Rc;
use std::rc::Weak;
use std::sync::Arc;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
use std::task::Context;
#[cfg(test)]
use std::task::Poll;
#[cfg(test)]
use std::task::Waker;
use uring_runtime::mailbox;

#[cfg(test)]
type Completion = mailbox::Completion<Result<Value>, AcquisitionBudget>;
#[cfg(test)]
type Command = mailbox::Command<Work, Result<Value>, AcquisitionBudget, RequestScope>;
type Receipt = mailbox::Receipt<Work, Result<Value>, AcquisitionBudget, RequestScope>;
type Handoff = mailbox::Mailbox<Work, Result<Value>, AcquisitionBudget, RequestScope>;

thread_local! {
    // Installed only on the owning I/O thread. No Rc enters the shared directory.
    static LOCALS: RefCell<HashMap<usize, (WorkerId, Weak<Coordinator>)>> = RefCell::new(HashMap::default());
}

struct Mailbox {
    worker: WorkerId,
    inner: Arc<Handoff>,
}
impl std::ops::Deref for Mailbox {
    type Target = Handoff;
    fn deref(&self) -> &Handoff {
        &self.inner
    }
}

/// The map is immutable for the lifetime of this directory. Construct a replacement
/// only after all endpoints and completion receipts have drained.
pub struct WorkerDirectory {
    pub warm: super::warm::WarmRegistry,
    pub(crate) subscriptions: Arc<super::range_stream::Scheduler>,
    map: Arc<WorkerMap>,
    mailboxes: Vec<Arc<Mailbox>>,
    sequence: AtomicU64,
}

enum Work {
    Hold(ObjectVersion, HoldPages, std::time::SystemTime, HolderKind),
    /// Drop one holder from each page of this worker's hold for the version.
    Release(ObjectVersion, HoldPages, HoldEpoch),
    Warm(
        PageId,
        crate::model::VersionMetadata,
        Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    WarmLocal(PageId),
    Select(
        ObjectVersion,
        super::range_stream::Selection,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    Selected(crate::memory::CiphertextCopy),
    Cached(PageId),
    Observed(PageResult),
    Resolve(
        MetadataSelector,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    Acquire(
        PageId,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    AcquireUnobserved(PageId, Arc<crate::topology::Membership>, PeerOriginContext),
    /// One read window page: the warm hold first, then ordinary Fill.
    Fetch(
        PageId,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    Publish(VersionMetadata),
    Retained(ObjectVersion),
    Peer(VerifiedRequest, std::sync::Arc<crate::topology::Membership>),
}
enum Value {
    Held(bool, std::time::SystemTime, HoldEpoch),
    Metadata(ObjectMetadata),
    Page(PageResult),
    Cached(Option<PageResult>),
    Published,
    Retained(Option<VersionMetadata>),
    Peer(PeerResponse),
}
/// Runtime executor specialized to application work results and acquisition credits.
type Executor = mailbox::executor::Executor<Value, AcquisitionBudget, RequestScope>;

impl mailbox::executor::ExecutionScope for RequestScope {
    /// Keep the caller's deadline and identity without sharing cancellation authority.
    fn detached(&self) -> Result<Self> {
        let mut scope = self.clone();
        scope.cancellation = Cancellation::new()?;
        Ok(scope)
    }

    /// Notify accepted work without replacing its completion fence.
    fn cancel(&self) -> Result<()> {
        self.cancel()
    }

    /// Expose the original acquisition ceiling to the local scheduling loop.
    fn deadline(&self) -> std::time::Instant {
        self.deadline.0
    }

    /// Preserve the application's generation-mismatch classification.
    fn stale_completion() -> Error {
        Error::StaleFlight
    }
}

/// Worker-owned command driver. Poll it even after ingress requests disconnect.
/// Active futures are retained until completion; cancel is never a completion fence.
pub struct WorkerEndpoint {
    directory: Arc<WorkerDirectory>,

    mailbox: Arc<Mailbox>,

    local: Rc<Coordinator>,

    active: Executor,
}

/// Outcome of waiting for an active warm job to reach one page.
pub(super) enum Warmed {
    /// No warm job covers the version, or the job was abandoned.
    Unwarmed,
    /// Warm has published the page on a remote primary owner. Only this page
    /// may be filled; warm completes pages in any order, so later pages may
    /// still be pending.
    Remote,
    /// Warm has published the page locally and this is its held copy.
    Local(Box<PageResult>),
}

impl WorkerDirectory {
    #[cfg(test)]
    pub(crate) fn test_warm_command(
        &self,
        worker: WorkerId,
        local: Rc<Coordinator>,
    ) -> Option<Operation<'static, ()>> {
        let command = self.mailbox(worker).unwrap().pop()?;
        Some(Box::pin(async move {
            let Command {
                generation,
                work,
                scope,
                mut budget,
                reply,
                permit,
            } = command;
            let value = execute(&local, work, &scope, budget.as_mut()).await;
            reply
                .complete(generation, Completion { value, budget })
                .unwrap();
            drop(permit);
            Ok(())
        }))
    }
    #[cfg(test)]
    pub(super) fn test_observation_mailbox(&self) -> usize {
        for mailbox in &self.mailboxes {
            if !mailbox.has_queued() && mailbox.outstanding() == 0 {
                let _ = mailbox.install();
            }
        }
        self.mailboxes
            .iter()
            .map(|mailbox| mailbox.outstanding())
            .sum()
    }
    /// Scheduler fanout bypasses Fill flights. Count each consuming subscription
    /// on the stable page owner, retaining its original verified ciphertext.
    /// A full mailbox only loses an optional retention hint, never a page read.
    pub(crate) fn observe_verified(&self, page: PageResult, scope: &RequestScope) {
        let Ok(owner) = self.page_owner(page.plaintext.page()) else {
            return;
        };
        // Reserve bounded worker-owned progress BEFORE retaining a mailbox
        // receipt. Dropping or delaying this optional task cannot hold delivery.
        let Ok(permit) = uring_runtime::drivers::reserve() else {
            return;
        };
        if self.is_local(owner) {
            if let Ok(local) = self.local() {
                let fill = local.fill.clone();
                let scope = scope.clone();
                permit.submit_detached(Box::pin(async move {
                    fill.observe_verified_scoped(&page, &scope).await;
                    Ok::<(), Error>(())
                }));
            }
        } else if let Ok(receipt) = self.submit(owner, Work::Observed(page), scope, None) {
            permit.submit_detached(Box::pin(async move {
                let _ = receipt.await;
                Ok::<(), Error>(())
            }));
        }
    }
    pub(crate) async fn cached_page(
        &self,
        page: PageId,
        scope: &RequestScope,
    ) -> Result<Option<PageResult>> {
        let owner = self.page_owner(&page)?;
        if self.is_local(owner) {
            return self.local()?.fill.cached_page(&page, scope);
        }
        match self
            .submit(owner, Work::Cached(page), scope, None)?
            .await?
            .value?
        {
            Value::Cached(result) => Ok(result),
            _ => Err(Error::StaleFlight),
        }
    }
    pub(crate) fn start_selection(
        &self,
        version: ObjectVersion,
        selection: super::range_stream::Selection,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: AcquisitionBudget,
    ) -> Result<Operation<'static, (Result<PageResult>, AcquisitionBudget)>> {
        // Always use the owned worker driver, even for local ingress. Detaching the
        // stream must not free the provider contract before accepted I/O fences.
        let owner = self.metadata_owner(&version.object)?;
        let receipt = self.submit(
            owner,
            Work::Select(version, selection, membership, self.seal(context, scope)?),
            scope,
            Some(budget),
        )?;
        Ok(Box::pin(async move {
            let completion = receipt.await?;
            let budget = completion.budget.ok_or(Error::StaleFlight)?;
            let result = completion.value.and_then(|value| match value {
                Value::Page(result) => Ok(result),
                _ => Err(Error::StaleFlight),
            });
            Ok((result, budget))
        }))
    }
    pub(crate) async fn accept_selected(
        &self,
        copy: crate::memory::CiphertextCopy,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        let owner = self.page_owner(&copy.ciphertext.envelope().page)?;
        if self.is_local(owner) {
            return self
                .local()?
                .fill
                .accept_selected_unobserved(copy, scope)
                .await;
        }
        let receipt = self.submit(owner, Work::Selected(copy), scope, None)?;
        // Cancellation notifies the owner, but the selection keeps its provider
        // contract until its accepted crypto and publication work completes.
        let completion = std::future::poll_fn(|cx| receipt.poll_completion(cx)).await?;
        scope.check()?;
        match completion.value? {
            Value::Page(result) => Ok(result),
            _ => Err(Error::StaleFlight),
        }
    }
    /// A simulated process loss discards queued messages and their producer permits.
    /// Remaining receipts observe producer loss, not successful completion fences.
    /// Do not execute commands or mark kernel/NIC operations complete here.
    #[cfg(test)]
    pub(crate) fn simulation_crash(&self) {
        for mailbox in &self.mailboxes {
            mailbox.stop_admission();
            drop(mailbox.take_queued());
        }
    }
    /// Select a logical worker on the single-threaded simulation scheduler.
    /// Production gets this isolation from its owning OS thread.
    #[cfg(test)]
    pub(crate) fn simulation_scope(
        &self,
        local: Option<(WorkerId, Rc<Coordinator>)>,
    ) -> SimulationLocal {
        let key = self as *const Self as usize;
        let previous = LOCALS.with(|locals| {
            let mut locals = locals.borrow_mut();
            let previous = locals.remove(&key);
            if let Some((worker, local)) = local {
                locals.insert(key, (worker, Rc::downgrade(&local)));
            }
            previous
        });
        SimulationLocal { key, previous }
    }
    pub fn new(map: Arc<WorkerMap>, workers: Vec<WorkerId>, capacity: usize) -> Result<Self> {
        if workers.is_empty()
            || capacity == 0
            || workers
                .iter()
                .enumerate()
                .any(|(i, w)| workers[..i].contains(w))
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            warm: super::warm::WarmRegistry::new(capacity.saturating_mul(workers.len())),
            subscriptions: super::range_stream::Scheduler::new(
                capacity.saturating_mul(workers.len()),
            ),
            map,
            mailboxes: workers
                .into_iter()
                .map(|worker| {
                    Arc::new(Mailbox {
                        worker,
                        inner: Arc::new(Handoff::new(capacity).expect("validated capacity")),
                    })
                })
                .collect(),
            sequence: AtomicU64::new(1),
        })
    }

    pub fn install(
        self: &Arc<Self>,
        worker: WorkerId,
        local: Rc<Coordinator>,
    ) -> Result<WorkerEndpoint> {
        local
            .fill
            .dependencies
            .writer
            .index()
            .set_hold_capacity_percent(self.warm.config().retained_percent)?;
        let mailbox = self.mailbox(worker)?.clone();
        let key = Arc::as_ptr(self) as usize;
        LOCALS.with(|locals| {
            let mut locals = locals.borrow_mut();
            if locals
                .get(&key)
                .and_then(|(_, local)| local.upgrade())
                .is_some()
            {
                return Err(Error::InvalidConfiguration);
            }
            mailbox.install()?;
            locals.insert(key, (worker, Rc::downgrade(&local)));
            Ok(())
        })?;
        Ok(WorkerEndpoint {
            directory: self.clone(),
            mailbox,
            local,
            active: Executor::default(),
        })
    }

    fn mailbox(&self, worker: WorkerId) -> Result<&Arc<Mailbox>> {
        self.mailboxes
            .iter()
            .find(|m| m.worker == worker)
            .ok_or(Error::InvalidConfiguration)
    }
    pub(super) fn local(&self) -> Result<Rc<Coordinator>> {
        LOCALS
            .with(|locals| {
                locals
                    .borrow()
                    .get(&(self as *const Self as usize))
                    .and_then(|(_, local)| local.upgrade())
            })
            .ok_or(Error::Unavailable)
    }
    fn is_local(&self, owner: WorkerId) -> bool {
        LOCALS.with(|locals| {
            locals
                .borrow()
                .get(&(self as *const Self as usize))
                .is_some_and(|(worker, _)| *worker == owner)
        })
    }
    fn seal(&self, context: &OriginContext, scope: &RequestScope) -> Result<PeerOriginContext> {
        let sequence = self
            .sequence
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |n| n.checked_add(1))
            .map_err(|_| Error::Unavailable)?;
        let mut attempt = [0; 16];
        attempt[8..].copy_from_slice(&sequence.to_be_bytes());
        self.local()?
            .credentials
            .seal(context, AttemptId(attempt), scope)
    }
    pub fn metadata_owner(&self, object: &ObjectId) -> Result<WorkerId> {
        self.map.metadata_owner(object)
    }
    pub fn page_owner(&self, page: &PageId) -> Result<WorkerId> {
        self.map.owner(page)
    }

    /// Largest object page count this node admits for warm planning or holds.
    pub(super) fn max_warm_pages(&self) -> u64 {
        1_048_576u64.saturating_mul(self.mailboxes.len() as u64)
    }

    pub(super) fn workers(&self) -> Vec<WorkerId> {
        self.mailboxes
            .iter()
            .map(|mailbox| mailbox.worker)
            .collect()
    }

    /// Hold exactly `pages[worker]` on every local worker. Workers absent from
    /// `pages` still renew any hold they already have for the version.
    pub(super) async fn reserve_warm(
        &self,
        version: &ObjectVersion,
        pages: &std::collections::BTreeMap<u16, HoldPages>,
        until: std::time::SystemTime,
        scope: &RequestScope,
        fresh: &mut Vec<WorkerId>,
    ) -> Result<std::time::SystemTime> {
        scope.check()?;
        let mut accepted = until;
        for mailbox in &self.mailboxes {
            let held = pages.get(&mailbox.worker.0).cloned().unwrap_or_default();
            let receipt = self.submit(
                mailbox.worker,
                Work::Hold(version.clone(), held, until, HolderKind::Warm),
                scope,
                None,
            )?;
            match std::future::poll_fn(|cx| receipt.poll_completion(cx))
                .await?
                .value?
            {
                Value::Held(_created, expiry, _epoch) => {
                    fresh.push(mailbox.worker);
                    accepted = accepted.max(expiry);
                }
                _ => return Err(Error::StaleFlight),
            }
        }
        // All workers must honor the same expiry, including recovered longer holds.
        if accepted > until {
            for mailbox in &self.mailboxes {
                let held = pages.get(&mailbox.worker.0).cloned().unwrap_or_default();
                let receipt = self.submit(
                    mailbox.worker,
                    Work::Hold(version.clone(), held, accepted, HolderKind::Warm),
                    scope,
                    None,
                )?;
                std::future::poll_fn(|cx| receipt.poll_completion(cx))
                    .await?
                    .value?;
            }
        }
        Ok(accepted)
    }

    /// Drop one holder from `pages[worker]` on each listed local worker, under
    /// the epoch that worker's `Work::Hold` returned. Pages whose last holder
    /// leaves become evictable. Workers absent from `pages` are untouched.
    /// Pair with one `Work::Hold` per page set.
    #[cfg_attr(
        not(test),
        expect(dead_code, reason = "used by the per-read fetch job in a later change")
    )]
    pub(super) async fn release_hold_pages(
        &self,
        version: &ObjectVersion,
        pages: &std::collections::BTreeMap<u16, (HoldPages, HoldEpoch)>,
        scope: &RequestScope,
    ) -> Result<()> {
        scope.check()?;
        for (worker, (held, epoch)) in pages {
            if held.is_empty() {
                continue;
            }
            let receipt = self.submit(
                WorkerId(*worker),
                Work::Release(version.clone(), held.clone(), *epoch),
                scope,
                None,
            )?;
            match std::future::poll_fn(|cx| receipt.poll_completion(cx))
                .await?
                .value?
            {
                Value::Published => {}
                _ => return Err(Error::StaleFlight),
            }
        }
        Ok(())
    }

    /// Add one holder to `page` on its owning worker only, until `until`.
    /// Pair every success with one `release_read_pages` of the same page and
    /// the returned epoch.
    pub(super) async fn hold_read_page(
        &self,
        page: &PageId,
        until: std::time::SystemTime,
        scope: &RequestScope,
    ) -> Result<HoldEpoch> {
        let owner = self.page_owner(page)?;
        let pages = std::iter::once(page.number).collect::<HoldPages>();
        let receipt = self.submit(
            owner,
            Work::Hold(page.version.clone(), pages, until, HolderKind::Reader),
            scope,
            None,
        )?;
        match std::future::poll_fn(|cx| receipt.poll_completion(cx))
            .await?
            .value?
        {
            Value::Held(_, _, epoch) => Ok(epoch),
            _ => Err(Error::StaleFlight),
        }
    }

    /// Drop one holder from each page without waiting. Releases run on their
    /// owning workers under a fresh short scope, so a canceled read still
    /// returns its holds; hold expiry is the backstop if this cannot submit.
    pub(super) fn release_read_pages(
        &self,
        version: &ObjectVersion,
        pages: impl IntoIterator<Item = (PageNumber, HoldEpoch)>,
        request: crate::model::RequestId,
    ) {
        let mut owners: std::collections::BTreeMap<(WorkerId, HoldEpoch), HoldPages> =
            std::collections::BTreeMap::new();
        for (number, epoch) in pages {
            let page = PageId {
                version: version.clone(),
                number,
            };
            if let Ok(owner) = self.page_owner(&page) {
                owners.entry((owner, epoch)).or_default().insert(number);
            }
        }
        for ((owner, epoch), held) in owners {
            if self.is_local(owner) {
                if let Ok(local) = self.local() {
                    local
                        .fill
                        .dependencies
                        .writer
                        .index()
                        .release_hold_pages(version, &held, epoch);
                }
                continue;
            }
            let Ok(permit) = uring_runtime::drivers::reserve() else {
                continue;
            };
            let Ok(scope) = RequestScope::new(
                request,
                uring_runtime::environment::now() + std::time::Duration::from_secs(30),
            ) else {
                continue;
            };
            if let Ok(receipt) = self.submit(
                owner,
                Work::Release(version.clone(), held, epoch),
                &scope,
                None,
            ) {
                permit.submit_detached(Box::pin(async move {
                    let _ = receipt.await;
                    Ok::<(), Error>(())
                }));
            }
        }
    }

    /// Start a warm-path fill of a page a read holds: the owner fills it and
    /// persists it under the hold. The context is sealed before returning.
    pub(super) fn start_warm_page(
        &self,
        page: PageId,
        metadata: crate::model::VersionMetadata,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<Operation<'static, ()>> {
        let owner = self.page_owner(&page)?;
        let work = Work::Warm(page, metadata, membership, self.seal(context, scope)?);
        let receipt = self.submit(owner, work, scope, Some(super::default_budget(scope)))?;
        Ok(Box::pin(async move {
            match std::future::poll_fn(|cx| receipt.poll_completion(cx))
                .await?
                .value?
            {
                Value::Published => Ok(()),
                _ => Err(Error::StaleFlight),
            }
        }))
    }

    /// Read a page this node holds: memory first, then the held disk copy.
    pub(super) async fn held_page(&self, page: PageId, scope: &RequestScope) -> Result<PageResult> {
        let owner = self.page_owner(&page)?;
        if self.is_local(owner) {
            return self.local()?.fill.warm_local(&page, scope).await;
        }
        match self
            .submit(owner, Work::WarmLocal(page), scope, None)?
            .await?
            .value?
        {
            Value::Page(page) => Ok(page),
            _ => Err(Error::StaleFlight),
        }
    }

    pub(super) async fn warm_page(
        &self,
        page: PageId,
        metadata: crate::model::VersionMetadata,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<()> {
        let owner = self.page_owner(&page)?;
        let work = Work::Warm(page, metadata, membership, self.seal(context, scope)?);
        let receipt = self.submit(owner, work, scope, Some(super::default_budget(scope)))?;
        match std::future::poll_fn(|cx| receipt.poll_completion(cx))
            .await?
            .value?
        {
            Value::Published => Ok(()),
            _ => Err(Error::StaleFlight),
        }
    }

    /// Waits for any active warm job to publish `page` before it is acquired.
    pub(super) async fn warmed_page(&self, page: &PageId, scope: &RequestScope) -> Result<Warmed> {
        match self.warm.wait(page, scope).await? {
            WarmWait::Unwarmed => return Ok(Warmed::Unwarmed),
            // Pages held by a remote primary owner are read through normal Fill,
            // which routes to that owner's held copy under the usual topology.
            // Only this page is known to be complete; warm finishes pages in any
            // order, so callers must not widen the fill beyond it.
            WarmWait::Remote => return Ok(Warmed::Remote),
            WarmWait::Local => {}
        }
        let owner = self.page_owner(page)?;
        let result = if self.is_local(owner) {
            self.local()?.fill.warm_local(page, scope).await
        } else {
            match self
                .submit(owner, Work::WarmLocal(page.clone()), scope, None)?
                .await?
                .value?
            {
                Value::Page(page) => Ok(page),
                _ => Err(Error::StaleFlight),
            }
        };
        // A lost held copy is an error, not permission to race the active warm job.
        result.map(|page| Warmed::Local(Box::new(page)))
    }

    fn submit(
        &self,
        worker: WorkerId,
        work: Work,
        scope: &RequestScope,
        budget: Option<AcquisitionBudget>,
    ) -> Result<Receipt> {
        scope.check()?;
        let mailbox = self.mailbox(worker)?;
        let generation = self
            .sequence
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |n| n.checked_add(1))
            .map_err(|_| Error::Unavailable)?;
        mailbox.inner.submit(generation, work, scope, budget)
    }

    async fn budgeted(
        &self,
        owner: WorkerId,
        work: Work,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<Value> {
        // Exclusive transfer, not a clone or reset. Cancellation leaves zero credits
        // with the caller while accepted work retains the original until fenced.
        let owned = budget.transfer();
        let completion = self.submit(owner, work, scope, Some(owned))?.await?;
        *budget = completion.budget.ok_or(Error::StaleFlight)?;
        completion.value
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
            let owner = self.metadata_owner(&context.object)?;
            if self.is_local(owner) {
                return self
                    .local()?
                    .metadata
                    .resolve_with_budget(selector, membership, context, scope, budget)
                    .await;
            }
            let work = Work::Resolve(selector, membership, self.seal(context, scope)?);
            match self.budgeted(owner, work, scope, budget).await? {
                Value::Metadata(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        })
    }
    pub fn acquire<'a>(
        &'a self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, PageResult> {
        Box::pin(async move {
            if page.version.object != context.object {
                return Err(Error::InvalidRequest);
            }
            let owner = self.page_owner(&page)?;
            if self.is_local(owner) {
                return self
                    .local()?
                    .fill
                    .acquire(page, membership, context, scope, budget)
                    .await;
            }
            let work = Work::Acquire(page.clone(), membership, self.seal(context, scope)?);
            match self.budgeted(owner, work, scope, budget).await? {
                Value::Page(value) => {
                    value.validate_for(&page)?;
                    Ok(value)
                }
                _ => Err(Error::StaleFlight),
            }
        })
    }
    pub(crate) async fn acquire_unobserved(
        &self,
        page: PageId,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PageResult> {
        let owner = self.page_owner(&page)?;
        if self.is_local(owner) {
            return self
                .local()?
                .fill
                .acquire_unobserved(page, membership, context, scope, budget)
                .await;
        }
        let work = Work::AcquireUnobserved(page, membership, self.seal(context, scope)?);
        match self.budgeted(owner, work, scope, budget).await? {
            Value::Page(page) => Ok(page),
            _ => Err(Error::StaleFlight),
        }
    }
    /// Fetch one read window page on its owner. Always enqueue, including local
    /// ingress, so the owner's worker keeps accepted work fenced after the
    /// caller detaches.
    pub(crate) fn start_page(
        &self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: AcquisitionBudget,
    ) -> Result<Operation<'static, (Result<PageResult>, AcquisitionBudget)>> {
        if page.version.object != context.object {
            return Err(Error::InvalidRequest);
        }
        let owner = self.page_owner(&page)?;
        let receipt = self.submit(
            owner,
            Work::Fetch(page, membership, self.seal(context, scope)?),
            scope,
            Some(budget),
        )?;
        Ok(Box::pin(async move {
            let completion = receipt.await?;
            let budget = completion.budget.ok_or(Error::StaleFlight)?;
            let value = completion.value.and_then(|value| match value {
                Value::Page(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            });
            Ok((value, budget))
        }))
    }
    pub fn publish_metadata<'a>(
        &'a self,
        metadata: VersionMetadata,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let owner = self.metadata_owner(&metadata.version.object)?;
            if self.is_local(owner) {
                return self.local()?.metadata.publish_version(metadata);
            }
            match self
                .submit(owner, Work::Publish(metadata), scope, None)?
                .await?
                .value?
            {
                Value::Published => Ok(()),
                _ => Err(Error::StaleFlight),
            }
        })
    }
    pub fn retained_metadata<'a>(
        &'a self,
        version: &'a ObjectVersion,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<VersionMetadata>> {
        Box::pin(async move {
            let mut found: Option<VersionMetadata> = None;
            for mailbox in &self.mailboxes {
                let value = if self.is_local(mailbox.worker) {
                    Value::Retained(self.local()?.fill.retained_metadata(version, scope).await?)
                } else {
                    self.submit(mailbox.worker, Work::Retained(version.clone()), scope, None)?
                        .await?
                        .value?
                };
                match value {
                    Value::Retained(Some(value)) => {
                        super::merge_metadata(&mut found, value, version)?;
                    }
                    Value::Retained(None) => {}
                    _ => return Err(Error::StaleFlight),
                }
            }
            Ok(found)
        })
    }
    fn peer<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            let owner = match &request.request().operation {
                // PeerServer must authenticate and project a provider selection.
                crate::peer::protocol::Operation::Subscribe { .. } => {
                    return Err(Error::InvalidRequest);
                }
                crate::peer::protocol::Operation::Bootstrap { object, .. } => {
                    self.metadata_owner(object)?
                }
                crate::peer::protocol::Operation::Page { page, .. } => self.page_owner(page)?,
                crate::peer::protocol::Operation::Metadata { object, .. } => {
                    self.metadata_owner(object)?
                }
                // Fill runs on the page's local worker; hold and release fan out
                // from the object's metadata worker to every local worker.
                crate::peer::protocol::Operation::Warm {
                    version,
                    action: crate::peer::protocol::WarmAction::Fill { page, .. },
                    ..
                } => self.page_owner(&PageId {
                    version: version.clone(),
                    number: *page,
                })?,
                crate::peer::protocol::Operation::Warm { version, .. } => {
                    self.metadata_owner(&version.object)?
                }
            };
            if self.is_local(owner) {
                return self.local()?.serve_peer(request, membership, scope).await;
            }
            match self
                .submit(owner, Work::Peer(request, membership), scope, None)?
                .await?
                .value?
            {
                Value::Peer(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        })
    }
}

#[cfg(test)]
pub(crate) struct SimulationLocal {
    key: usize,
    previous: Option<(WorkerId, std::rc::Weak<Coordinator>)>,
}
#[cfg(test)]
impl Drop for SimulationLocal {
    fn drop(&mut self) {
        LOCALS.with(|locals| {
            let mut locals = locals.borrow_mut();
            locals.remove(&self.key);
            if let Some(previous) = self.previous.take() {
                locals.insert(self.key, previous);
            }
        });
    }
}

impl WorkerEndpoint {
    /// Simulated process loss occurs after the OS crash cut. Drop accepted tasks
    /// without polling them; the simulated reactor still fences owned buffers.
    #[cfg(test)]
    pub(crate) fn simulation_crash(&mut self) {
        assert!(uring_runtime::reactor::simulation::Simulation::current().is_some());
        self.directory.simulation_crash();
        self.active.simulation_crash();
    }
    pub fn stop_admission(&self) {
        self.directory.warm.stop();
        self.mailbox.stop_admission();
    }
    pub fn is_drained(&self) -> bool {
        self.mailbox.outstanding() == 0 && self.directory.warm.drained(self.mailbox.worker)
    }
    /// Close admission and continue reaping every accepted command. An expired
    /// shutdown scope requests cancellation but never substitutes for completion.
    pub fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.stop_admission();
        Box::pin(std::future::poll_fn(move |cx| {
            self.directory.warm.poll_releases(
                self.mailbox.worker,
                self.local.fill.dependencies.writer.index(),
                cx,
            )?;
            match self
                .active
                .poll_drain(&self.mailbox, scope, cx, self.local.clone())
            {
                std::task::Poll::Ready(Ok(()))
                    if !self.directory.warm.drained(self.mailbox.worker) =>
                {
                    std::task::Poll::Pending
                }
                other => other,
            }
        }))
    }
    /// Register a reactor waker and execute at most `work_budget` command/poll steps.
    pub fn poll(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        self.directory.warm.poll_deadlines();
        self.directory.warm.poll_releases(
            self.mailbox.worker,
            self.local.fill.dependencies.writer.index(),
            cx,
        )?;
        self.active
            .poll(&self.mailbox, cx, work_budget, self.local.clone())
    }
    pub fn poll_budgeted(&mut self, work_budget: usize) -> Result<()> {
        let waker = futures::task::noop_waker();
        self.poll(&mut Context::from_waker(&waker), work_budget)
    }
    /// Earliest caller deadline. The I/O loop currently uses its bounded 1 ms tick
    /// to check deadlines rather than scheduling this value directly.
    pub fn next_deadline(&self) -> Option<std::time::Instant> {
        self.active.next_deadline(&self.mailbox)
    }
    pub fn uninstall(&mut self) -> Result<()> {
        self.stop_admission();
        if !self.is_drained() || !self.active.is_empty() {
            return Err(Error::Unavailable);
        }
        self.mailbox.uninstall()?;
        LOCALS.with(|locals| {
            locals
                .borrow_mut()
                .remove(&(Arc::as_ptr(&self.directory) as usize))
        });
        Ok(())
    }
}
impl Drop for WorkerEndpoint {
    fn drop(&mut self) {
        self.stop_admission();
        LOCALS.with(|locals| {
            locals
                .borrow_mut()
                .remove(&(Arc::as_ptr(&self.directory) as usize));
        });
        // Queued work has not started and can be fenced as non-submission. Drop
        // outside the mailbox lock because its permit releases against that lock.
        Executor::reject_queued(&self.mailbox, Error::Unavailable);
    }
}

impl mailbox::executor::Handler<Work, Value, AcquisitionBudget, RequestScope> for Coordinator {
    /// Dispatch application work without owning the runtime reply or producer credit.
    fn execute<'a>(
        &'a self,
        work: Work,
        scope: &'a RequestScope,
        budget: Option<&'a mut AcquisitionBudget>,
    ) -> Operation<'a, Value> {
        Box::pin(execute(self, work, scope, budget))
    }
}

async fn execute(
    local: &Coordinator,
    work: Work,
    scope: &RequestScope,
    budget: Option<&mut AcquisitionBudget>,
) -> Result<Value> {
    scope.check()?;
    match work {
        Work::Hold(version, pages, until, kind) => {
            let previous = local.fill.dependencies.writer.index().hold_expiry(&version);
            let epoch = local
                .fill
                .dependencies
                .writer
                .index()
                .reserve_hold_kind(&version, &pages, until, kind)?;
            Ok(Value::Held(
                previous.is_none(),
                previous.map_or(until, |old| old.max(until)),
                epoch,
            ))
        }
        Work::Release(version, pages, epoch) => {
            local
                .fill
                .dependencies
                .writer
                .index()
                .release_hold_pages(&version, &pages, epoch);
            Ok(Value::Published)
        }
        Work::WarmLocal(page) => local.fill.warm_local(&page, scope).await.map(Value::Page),
        Work::Warm(page, metadata, membership, envelope) => {
            let context = local.open_context(envelope)?;
            let result = local
                .fill
                .acquire_warm(
                    page,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await?;
            if !metadata.compatible(&result.metadata.immutable()) {
                return Err(Error::CorruptRecord);
            }
            local
                .fill
                .dependencies
                .writer
                .persist_held(result.copy(), scope)
                .await?;
            Ok(Value::Published)
        }
        Work::Observed(page) => {
            local.fill.observe_verified_scoped(&page, scope).await;
            Ok(Value::Published)
        }
        Work::Select(version, selection, membership, envelope) => {
            if selection.version != version {
                return Err(Error::InvalidRequest);
            }
            let context = local.open_context(envelope)?;
            let page = local
                .fill
                .select_subscription(
                    &selection,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await?;
            selection.complete(page.clone())?;
            Ok(Value::Page(page))
        }
        Work::Selected(copy) => local
            .fill
            .accept_selected_unobserved(copy, scope)
            .await
            .map(Value::Page),
        Work::Cached(page) => local.fill.cached_page(&page, scope).map(Value::Cached),
        Work::Resolve(selector, membership, envelope) => {
            let context = local.open_context(envelope)?;
            local
                .metadata
                .resolve_with_budget(
                    selector,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await
                .map(Value::Metadata)
        }
        Work::Acquire(page, membership, envelope) => {
            let context = local.open_context(envelope)?;
            local
                .fill
                .acquire(
                    page,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await
                .map(Value::Page)
        }
        Work::AcquireUnobserved(page, membership, envelope) => {
            let context = local.open_context(envelope)?;
            local
                .fill
                .acquire_unobserved(
                    page,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await
                .map(Value::Page)
        }
        Work::Fetch(page, membership, envelope) => {
            if let Warmed::Local(result) =
                local.streams.directory().warmed_page(&page, scope).await?
            {
                return Ok(Value::Page(*result));
            }
            let context = local.open_context(envelope)?;
            local
                .fill
                .acquire(
                    page,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await
                .map(Value::Page)
        }
        Work::Publish(metadata) => {
            local.metadata.publish_version(metadata)?;
            Ok(Value::Published)
        }
        Work::Retained(version) => local
            .fill
            .retained_metadata(&version, scope)
            .await
            .map(Value::Retained),
        Work::Peer(request, membership) => local
            .serve_peer(request, membership, scope)
            .await
            .map(Value::Peer),
    }
}

// PeerServer owns local services through Rc; retain the node-wide directory Arc.
impl LocalPageService for Arc<WorkerDirectory> {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        self.peer(request, membership, scope)
    }
}

pub struct WorkerMap {
    workers: Vec<WorkerId>,
}
impl WorkerMap {
    /// Canonical worker order makes assignment independent of discovery order.
    /// Changing this set requires draining all flights first.
    pub fn new(mut workers: Vec<WorkerId>) -> Result<Self> {
        workers.sort_unstable_by_key(|worker| worker.0);
        if workers.is_empty() || workers.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { workers })
    }

    /// Same owner as this object's page zero, without needing an ETag first.
    pub fn metadata_owner(&self, object: &ObjectId) -> Result<WorkerId> {
        self.select(object, 0)
    }
    pub fn owner(&self, page: &PageId) -> Result<WorkerId> {
        self.select(&page.version.object, page.number.0)
    }

    fn select(&self, object: &ObjectId, page: u64) -> Result<WorkerId> {
        let mut hash = Sha256::new();
        hash.update(b"racer.local-worker.v1\0");
        let length = u64::try_from(object.cache.0.len()).map_err(|_| Error::InvalidRequest)?;
        hash.update(length.to_be_bytes());
        hash.update(object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.to_be_bytes());
        let digest = hash.finalize();
        Ok(self.select_digest(&digest.into()))
    }

    fn select_digest(&self, digest: &[u8; 32]) -> WorkerId {
        let index = u64::from_be_bytes(digest[..8].try_into().expect("eight digest bytes"))
            % u64::try_from(self.workers.len()).expect("worker count fits u64");
        self.workers[index as usize]
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use racer_control_wire::CacheId;
    use std::time::Duration;
    use std::time::Instant;
    #[test]
    fn head_admission_returns_before_body_and_survives_caller_cancel() {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(64));
        let _queue = queue.enter();
        let directory = Arc::new(directory(4));
        let (local, _fixture) = crate::read::tests::admission_coordinator();
        let _local = directory.simulation_scope(Some((WorkerId(0), local.clone())));
        let caller = scope();
        let mut version = version();
        version.object.cache =
            racer_control_wire::CacheId(crate::test_support::security::CACHE.into());
        let origin = OriginContext {
            object: version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let context = local.credentials.local_context(&origin, &caller).unwrap();
        let metadata = ObjectMetadata {
            version,
            length: 3,
            content_type: None,
            expires_at: crate::model::ExpiresAt::test_time(
                std::time::SystemTime::now() + Duration::from_secs(60),
            ),
        };
        let membership = Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        let mut head = Box::pin(super::super::warm::admit(
            directory.clone(),
            metadata,
            membership,
            context,
            Duration::from_secs(60),
            &caller,
        ));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(head.as_mut().poll(&mut cx).is_pending());
        for mailbox in &directory.mailboxes {
            queue.poll(&mut cx, 64);
            let command = mailbox.pop().expect("hold command");
            assert!(matches!(command.work, Work::Hold(..)));
            let Work::Hold(_, _, until, _) = &command.work else {
                unreachable!()
            };
            command
                .reply
                .complete(
                    command.generation,
                    Completion {
                        value: Ok(Value::Held(true, *until, HoldEpoch::default())),
                        budget: None,
                    },
                )
                .unwrap();
        }
        queue.poll(&mut cx, 64);
        assert!(matches!(head.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        drop(head);
        caller.cancel().unwrap();
        assert_eq!(queue.pending(), 1, "body driver survives HEAD");
        let command = directory
            .mailboxes
            .iter()
            .find_map(|mailbox| mailbox.pop())
            .expect("warm body command");
        assert!(matches!(command.work, Work::Warm(..)));
        assert!(
            command.scope.check().is_ok(),
            "fresh warm cancellation domain"
        );
        command
            .reply
            .complete(
                command.generation,
                Completion {
                    value: Err(Error::Unavailable),
                    budget: None,
                },
            )
            .unwrap();
        drop(command);
        queue.poll(&mut cx, 64);
        assert_eq!(queue.pending(), 0);
        assert_eq!(directory.warm.snapshot(), (0, 0));
    }
    #[test]
    fn fetch_and_select_commands_wait_behind_unstarted_warm_pages() {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(64));
        let _queue = queue.enter();
        let (local, _fixture) = crate::read::tests::admission_coordinator();
        let directory = local.streams.directory();
        let caller = scope();
        let mut version = version();
        version.object.cache =
            racer_control_wire::CacheId(crate::test_support::security::CACHE.into());
        let (job, _) = directory.warm.begin(&version).unwrap();
        let context = OriginContext {
            object: version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let membership = Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        for fetch in [true, false] {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let envelope = local
                .credentials
                .seal(&context, AttemptId([3; 16]), &caller)
                .unwrap();
            let work = if fetch {
                Work::Fetch(
                    PageId {
                        version: version.clone(),
                        number: crate::model::PageNumber(8),
                    },
                    membership.clone(),
                    envelope,
                )
            } else {
                let demand = crate::peer::subscriptions::Demand::new(vec![
                    crate::peer::subscriptions::PageInterval { start: 8, end: 10 },
                ])
                .unwrap();
                Work::Select(
                    version.clone(),
                    super::super::range_stream::Selection::for_test(
                        &directory.subscriptions,
                        version.clone(),
                        demand,
                    ),
                    membership.clone(),
                    envelope,
                )
            };
            let mut budget = super::super::default_budget(&caller);
            let mut command = Box::pin(execute(&local, work, &caller, Some(&mut budget)));
            for _ in 0..3 {
                assert!(command.as_mut().poll(&mut cx).is_pending());
            }
            assert_eq!(directory.warm.snapshot().1, 1);
            assert_eq!(
                queue.pending(),
                0,
                "no Fill driver or outbound acquisition may start"
            );
            drop(command);
            assert_eq!(directory.warm.snapshot().1, 0);
        }
        job.stop();
    }
    #[test]
    fn canonical_order_rejects_empty_and_duplicate_identities() {
        assert!(matches!(
            WorkerMap::new(vec![]),
            Err(Error::InvalidConfiguration)
        ));
        assert!(matches!(
            WorkerMap::new(vec![WorkerId(1), WorkerId(1)]),
            Err(Error::InvalidConfiguration)
        ));
        let map = WorkerMap::new(vec![WorkerId(9), WorkerId(1), WorkerId(3)]).unwrap();
        let ordered = WorkerMap::new(vec![WorkerId(1), WorkerId(3), WorkerId(9)]).unwrap();
        for value in 0..100_u64 {
            let mut digest = [0; 32];
            digest[..8].copy_from_slice(&value.to_be_bytes());
            assert_eq!(map.select_digest(&digest), ordered.select_digest(&digest));
            assert_eq!(
                map.select_digest(&digest),
                WorkerId([1, 3, 9][(value % 3) as usize])
            );
        }
    }

    #[test]
    fn selection_uses_big_endian_prefix_only_and_handles_singletons() {
        let map = WorkerMap::new((0..7).rev().map(WorkerId).collect()).unwrap();
        let mut digest = [255; 32];
        digest[..8].copy_from_slice(&256_u64.to_be_bytes());
        assert_eq!(map.select_digest(&digest), WorkerId(4));
        digest[8..].fill(0);
        assert_eq!(map.select_digest(&digest), WorkerId(4));
        assert_eq!(map.select_digest(&[255; 32]), WorkerId(1));
        let singleton = WorkerMap::new(vec![WorkerId(42)]).unwrap();
        assert_eq!(singleton.select_digest(&[255; 32]), WorkerId(42));
        assert_eq!(singleton.select_digest(&[0; 32]), WorkerId(42));
    }

    #[test]
    fn worker_map_preserves_configuration_errors_and_single_worker_assignment() {
        assert!(matches!(
            WorkerMap::new(vec![]),
            Err(Error::InvalidConfiguration)
        ));
        assert!(matches!(
            WorkerMap::new(vec![WorkerId(9), WorkerId(1), WorkerId(9)]),
            Err(Error::InvalidConfiguration)
        ));
        let map = WorkerMap::new(vec![WorkerId(u16::MAX)]).unwrap();
        let page = PageId {
            version: version(),
            number: crate::model::PageNumber(u64::MAX),
        };
        assert_eq!(map.owner(&page), Ok(WorkerId(u16::MAX)));
        assert_eq!(
            map.metadata_owner(&page.version.object),
            Ok(WorkerId(u16::MAX))
        );
    }
    #[test]
    fn stable_assignment_ignores_etag_and_worker_input_order() {
        use crate::model::CacheKey;
        use crate::model::ObjectVersion;
        use crate::model::PageNumber;
        use crate::model::StrongEtag;
        use racer_control_wire::CacheId;
        let map = WorkerMap::new(vec![WorkerId(9), WorkerId(3), WorkerId(1)]).unwrap();
        let ordered = WorkerMap::new(vec![WorkerId(1), WorkerId(3), WorkerId(9)]).unwrap();
        let object = ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([7; 32]),
        };
        let mut page = PageId {
            version: ObjectVersion {
                object: object.clone(),
                etag: StrongEtag::test_value("a"),
            },
            number: PageNumber(0),
        };
        assert_eq!(map.owner(&page), map.metadata_owner(&object));
        for number in 0..100 {
            page.number = PageNumber(number);
            let owner = map.owner(&page);
            assert_eq!(owner, ordered.owner(&page));
            page.version.etag = StrongEtag::test_value("different");
            assert_eq!(owner, map.owner(&page));
        }
        // SHA-256 encoding vector, independent of std's randomized hasher.
        assert_eq!(map.metadata_owner(&object).unwrap(), WorkerId(1));
    }

    fn directory(capacity: usize) -> WorkerDirectory {
        let workers = vec![WorkerId(0), WorkerId(1)];
        let directory = WorkerDirectory::new(
            Arc::new(WorkerMap::new(workers.clone()).unwrap()),
            workers,
            capacity,
        )
        .unwrap();
        for mailbox in &directory.mailboxes {
            mailbox.install().unwrap();
        }
        directory
    }
    fn scope() -> RequestScope {
        RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(60)).unwrap()
    }
    fn version() -> ObjectVersion {
        ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([7; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        }
    }

    #[test]
    fn selected_owner_cancellation_waits_for_actual_completion() {
        let directory = directory(1);
        let scope = scope();
        let admission = Rc::new(flow_control::Quotas::new(
            crate::admission::AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ),
        ));
        let page = crate::memory::tests::bundle_for(
            &admission,
            VersionMetadata {
                version: version(),
                length: 3,
                content_type: None,
            },
        );
        let owner = directory.page_owner(page.plaintext.page()).unwrap();
        let mut selected = Box::pin(directory.accept_selected(page.copy(), &scope));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(selected.as_mut().poll(&mut cx).is_pending());
        let command = directory.mailbox(owner).unwrap().pop().unwrap();
        assert!(
            matches!(&command.work, Work::Selected(copy) if copy.metadata.version == version())
        );
        scope.cancel().unwrap();
        assert!(selected.as_mut().poll(&mut cx).is_pending());
        assert_eq!(directory.mailbox(owner).unwrap().outstanding(), 1);
        command
            .reply
            .complete(
                command.generation,
                Completion {
                    value: Err(Error::Cancelled),
                    budget: None,
                },
            )
            .unwrap();
        assert!(matches!(
            selected.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop((selected, command));
        assert_eq!(directory.mailbox(owner).unwrap().outstanding(), 0);
    }

    #[test]
    fn expired_drain_completes_queued_selected_waiters_and_reclaims_ciphertext() {
        use crate::admission::ResourceClass;
        for cancel_caller in [false, true] {
            let directory = Arc::new(directory(1));
            let caller = scope();
            let admission = Rc::new(flow_control::Quotas::new(
                crate::admission::AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ),
            ));
            let page = crate::memory::tests::bundle_for(
                &admission,
                VersionMetadata {
                    version: version(),
                    length: 3,
                    content_type: None,
                },
            );
            let owner = directory.page_owner(page.plaintext.page()).unwrap();
            let mailbox = directory.mailbox(owner).unwrap().clone();
            let mut endpoint = WorkerEndpoint {
                mailbox: mailbox.clone(),
                directory: directory.clone(),
                local: crate::test_support::wake_test_coordinator(),
                active: Executor::default(),
            };
            // Model ingress on the other worker so the real selected handoff is used.
            let _ingress =
                directory.simulation_scope(Some((WorkerId(1 - owner.0), endpoint.local.clone())));
            let mut selected = Box::pin(directory.accept_selected(page.copy(), &caller));
            drop(page);
            let count = Arc::new(crate::test_support::WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            assert!(selected.as_mut().poll(&mut cx).is_pending());
            assert!(mailbox.has_queued());
            assert_eq!(mailbox.outstanding(), 1);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
            if cancel_caller {
                caller.cancel().unwrap();
                assert!(selected.as_mut().poll(&mut cx).is_pending());
            }
            let shutdown = RequestScope::new(RequestId([2; 16]), Instant::now()).unwrap();
            assert_eq!(shutdown.check(), Err(Error::DeadlineExceeded));
            let mut drain = endpoint.drain(&shutdown);
            let before = count.count();
            assert!(drain.as_mut().poll(&mut cx).is_pending());
            assert!(
                count.count() > before,
                "queued completion must wake its waiter"
            );
            assert!(!mailbox.has_queued());
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            assert_eq!(mailbox.outstanding(), 1, "unread receipt retains its slot");
            assert!(matches!(
                selected.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Cancelled))
            ));
            assert_eq!(mailbox.outstanding(), 0);
            assert_eq!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            drop(drain);
            assert!(
                endpoint.active.is_empty(),
                "queued work was never submitted"
            );
            endpoint.uninstall().unwrap();
        }
    }

    #[test]
    fn expired_drain_returns_queued_budget_and_reclaims_detached_receipts() {
        for detach in [false, true] {
            let directory = Arc::new(directory(1));
            let mut endpoint = WorkerEndpoint {
                mailbox: directory.mailboxes[0].clone(),
                directory: directory.clone(),
                local: crate::test_support::wake_test_coordinator(),
                active: Executor::default(),
            };
            let caller = scope();
            let mut budget = AcquisitionBudget::new(caller.deadline.0, 4, 8);
            budget
                .begin_attempt(Instant::now(), caller.deadline.0)
                .unwrap();
            budget.charge_links(3).unwrap();
            let mut receipt = directory
                .submit(
                    WorkerId(0),
                    Work::Retained(version()),
                    &caller,
                    Some(budget),
                )
                .unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
            let shutdown = RequestScope::new(RequestId([2; 16]), Instant::now()).unwrap();
            let mut drain = endpoint.drain(&shutdown);
            if detach {
                drop(receipt);
                assert_eq!(directory.mailboxes[0].outstanding(), 1);
                assert_eq!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            } else {
                assert!(drain.as_mut().poll(&mut cx).is_pending());
                let Poll::Ready(Ok(completion)) = Pin::new(&mut receipt).poll(&mut cx) else {
                    panic!("queued non-submission must return a normal completion");
                };
                assert!(matches!(completion.value, Err(Error::Cancelled)));
                let budget = completion.budget.unwrap();
                assert_eq!(budget.remaining_attempts(), 3);
                assert_eq!(budget.remaining_links(), 5);
                assert_eq!(budget.deadline(), caller.deadline.0);
                assert_eq!(directory.mailboxes[0].outstanding(), 1);
                drop(receipt);
                assert_eq!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
            }
            drop(drain);
            assert_eq!(directory.mailboxes[0].outstanding(), 0);
            assert!(endpoint.active.is_empty());
            endpoint.uninstall().unwrap();
        }
    }

    #[test]
    fn cached_page_handoff_is_lookup_only_and_propagates_misses_and_errors() {
        for error in [None, Some(Error::Cancelled), Some(Error::CorruptRecord)] {
            let directory = directory(1);
            let scope = scope();
            let page = PageId {
                version: version(),
                number: crate::model::PageNumber(9),
            };
            let owner = directory.page_owner(&page).unwrap();
            let mut lookup = Box::pin(directory.cached_page(page.clone(), &scope));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(lookup.as_mut().poll(&mut cx).is_pending());
            let command = directory.mailbox(owner).unwrap().pop().unwrap();
            assert!(matches!(&command.work, Work::Cached(requested) if requested == &page));
            assert!(command.budget.is_none(), "cache lookup cannot acquire");
            command
                .reply
                .complete(
                    command.generation,
                    Completion {
                        value: error.map_or(Ok(Value::Cached(None)), Err),
                        budget: None,
                    },
                )
                .unwrap();
            match lookup.as_mut().poll(&mut cx) {
                Poll::Ready(Ok(None)) => assert!(error.is_none()),
                Poll::Ready(Err(actual)) => assert_eq!(Some(actual), error),
                _ => panic!("unexpected lookup completion"),
            }
            drop((lookup, command));
            assert_eq!(directory.mailbox(owner).unwrap().outstanding(), 0);
        }
    }

    #[test]
    fn retained_metadata_requires_exact_mime_in_both_orders() {
        let version = version();
        let untyped = VersionMetadata {
            version: version.clone(),
            length: 3,
            content_type: None,
        };
        let mut typed = untyped.clone();
        typed.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
        let mut conflict = typed.clone();
        conflict.content_type = Some(crate::model::ContentType::parse(b"text/html").unwrap());
        let mut wrong_length = untyped.clone();
        wrong_length.length += 1;
        let mut wrong_version = untyped.clone();
        wrong_version.version.etag = StrongEtag::test_value("other");
        for (values, expected) in [
            (
                [untyped.clone(), untyped.clone()],
                Ok(Some(untyped.clone())),
            ),
            ([typed.clone(), typed.clone()], Ok(Some(typed.clone()))),
            ([untyped.clone(), typed.clone()], Err(Error::CorruptRecord)),
            ([typed.clone(), untyped.clone()], Err(Error::CorruptRecord)),
            ([typed.clone(), conflict.clone()], Err(Error::CorruptRecord)),
            ([conflict, typed.clone()], Err(Error::CorruptRecord)),
            ([untyped.clone(), wrong_length], Err(Error::CorruptRecord)),
            ([untyped, wrong_version], Err(Error::CorruptRecord)),
        ] {
            let directory = directory(1);
            let scope = scope();
            let mut lookup = directory.retained_metadata(&version, &scope);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for (mailbox, value) in directory.mailboxes.iter().zip(values) {
                assert!(lookup.as_mut().poll(&mut cx).is_pending());
                let command = mailbox.pop().unwrap();
                assert!(
                    matches!(&command.work, Work::Retained(requested) if requested == &version)
                );
                command
                    .reply
                    .complete(
                        command.generation,
                        Completion {
                            value: Ok(Value::Retained(Some(value))),
                            budget: None,
                        },
                    )
                    .unwrap();
                drop(command);
            }
            assert_eq!(lookup.as_mut().poll(&mut cx), Poll::Ready(expected));
            drop(lookup);
            for mailbox in &directory.mailboxes {
                assert_eq!(mailbox.outstanding(), 0);
            }
        }
    }

    #[test]
    fn mailbox_and_reply_notifications_cover_both_registration_orders() {
        for submit_before_poll in [false, true] {
            for complete_before_poll in [false, true] {
                let directory = Arc::new(directory(4));
                let mut endpoint = WorkerEndpoint {
                    mailbox: directory.mailboxes[0].clone(),
                    directory: directory.clone(),
                    local: crate::test_support::wake_test_coordinator(),
                    active: Executor::default(),
                };
                let count = Arc::new(crate::test_support::WakeCounter::default());
                let waker = Waker::from(count.clone());
                let mut cx = Context::from_waker(&waker);
                if !submit_before_poll {
                    endpoint.poll(&mut cx, 0).unwrap();
                }
                let sender = directory.clone();
                let mut receipt = std::thread::spawn(move || {
                    sender
                        .submit(
                            WorkerId(0),
                            Work::Publish(crate::model::VersionMetadata {
                                content_type: None,
                                version: version(),
                                length: 0,
                            }),
                            &scope(),
                            None,
                        )
                        .unwrap()
                })
                .join()
                .unwrap();
                assert_eq!(count.count(), usize::from(!submit_before_poll));
                if !complete_before_poll {
                    assert!(Pin::new(&mut receipt).poll(&mut cx).is_pending());
                }
                let before = count.count();
                endpoint.poll(&mut cx, 1).unwrap();
                assert_eq!(count.count(), before + usize::from(!complete_before_poll));
                let Poll::Ready(Ok(completion)) = Pin::new(&mut receipt).poll(&mut cx) else {
                    panic!("completion was lost across registration");
                };
                assert!(matches!(completion.value, Ok(Value::Published)));
                drop(receipt);
                assert!(endpoint.is_drained());
            }
        }
    }

    /// A parked real acquisition cannot prevent an unrelated mailbox completion.
    #[test]
    fn bounded_admission_mailbox_keeps_other_work_progressing() {
        let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(1024));
        let _owner = queue.enter();
        let directory = Arc::new(directory(4));
        let (local, _fixture) = crate::read::tests::admission_coordinator();
        let mut endpoint = WorkerEndpoint {
            mailbox: directory.mailboxes[0].clone(),
            directory: directory.clone(),
            local: local.clone(),
            active: Executor::default(),
        };
        let admission = &local.fill.dependencies.admission;
        let held = admission
            .reserve(
                None,
                crate::admission::ResourceClass::Flight,
                admission.limit(crate::admission::ResourceClass::Flight),
            )
            .unwrap();
        let caller = scope();
        let mut page = PageId {
            version: version(),
            number: crate::model::PageNumber(0),
        };
        page.version.object.cache =
            racer_control_wire::CacheId(crate::test_support::security::CACHE.into());
        let context = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let envelope = local
            .credentials
            .seal(&context, AttemptId([3; 16]), &caller)
            .unwrap();
        let membership = Arc::new(
            crate::topology::Membership::validate(racer_control_wire::MembershipVersion(1), vec![])
                .unwrap(),
        );
        let mut waiting = directory
            .submit(
                WorkerId(0),
                Work::Acquire(page.clone(), membership, envelope),
                &caller,
                Some(AcquisitionBudget::new(caller.deadline.0, 4, 8)),
            )
            .unwrap();
        let mut cx = Context::from_waker(Waker::noop());
        for _ in 0..8 {
            endpoint.poll(&mut cx, 8).unwrap();
        }
        if local.fill.dependencies.flights.admission_state().0 != 1 {
            let result = Pin::new(&mut waiting).poll(&mut cx);
            panic!(
                "acquisition did not park: {:?}",
                result.map(|r| r.map(|c| c.value.map(|_| ())))
            );
        }
        assert!(Pin::new(&mut waiting).poll(&mut cx).is_pending());
        let mut other = directory
            .submit(WorkerId(0), Work::Cached(page), &caller, None)
            .unwrap();
        for _ in 0..8 {
            endpoint.poll(&mut cx, 8).unwrap();
        }
        let Poll::Ready(Ok(done)) = Pin::new(&mut other).poll(&mut cx) else {
            panic!("lookup completion starved behind flight admission");
        };
        assert!(matches!(done.value, Ok(Value::Cached(None))));
        assert!(Pin::new(&mut waiting).poll(&mut cx).is_pending());
        drop(other);
        caller.cancel().unwrap();
        for _ in 0..8 {
            endpoint.poll(&mut cx, 8).unwrap();
        }
        drop(waiting);
        drop(held);
        assert_eq!(
            local.fill.dependencies.flights.admission_state(),
            (0, 0, false)
        );
        assert!(endpoint.is_drained());
        endpoint.uninstall().unwrap();
    }
    #[test]
    fn cancellation_does_not_reopen_a_full_mailbox_before_completion() {
        let directory = directory(1);
        let scope = scope();
        let receipt = directory
            .submit(WorkerId(0), Work::Retained(version()), &scope, None)
            .unwrap();
        assert!(matches!(
            directory.submit(WorkerId(0), Work::Retained(version()), &scope, None),
            Err(Error::Overloaded)
        ));
        let command = directory.mailboxes[0].pop().unwrap();
        drop(receipt);
        assert!(command.reply.is_abandoned());
        assert!(matches!(
            directory.submit(WorkerId(0), Work::Retained(version()), &scope, None),
            Err(Error::Overloaded)
        ));
        command
            .reply
            .complete(
                command.generation,
                Completion {
                    value: Ok(Value::Retained(None)),
                    budget: None,
                },
            )
            .unwrap();
        assert!(command.reply.is_abandoned());
        drop(command);
        let next = directory
            .submit(WorkerId(0), Work::Retained(version()), &scope, None)
            .unwrap();
        let command = directory.mailboxes[0].pop().unwrap();
        drop(next);
        assert_eq!(directory.mailboxes[0].outstanding(), 1);
        drop(command);
        assert_eq!(directory.mailboxes[0].outstanding(), 0);
    }
    #[test]
    fn completion_fence_rejects_late_and_duplicate_results_and_returns_spent_budget() {
        let directory = directory(1);
        let scope = scope();
        let mut receipt = Box::pin(
            directory
                .submit(
                    WorkerId(0),
                    Work::Retained(version()),
                    &scope,
                    Some(AcquisitionBudget::new(scope.deadline.0, 4, 8)),
                )
                .unwrap(),
        );
        let mut command = directory.mailboxes[0].pop().unwrap();
        let waker = futures::task::noop_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(receipt.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            command.reply.complete(
                command.generation + 1,
                Completion {
                    value: Err(Error::Unavailable),
                    budget: None
                }
            ),
            Err(mailbox::StaleCompletion)
        );
        let mut budget = command.budget.take().unwrap();
        budget
            .begin_attempt(Instant::now(), scope.deadline.0)
            .unwrap();
        budget.charge_links(3).unwrap();
        command
            .reply
            .complete(
                command.generation,
                Completion {
                    value: Ok(Value::Retained(None)),
                    budget: Some(budget),
                },
            )
            .unwrap();
        assert_eq!(
            command.reply.complete(
                command.generation,
                Completion {
                    value: Err(Error::Unavailable),
                    budget: None
                }
            ),
            Err(mailbox::StaleCompletion)
        );
        drop(command);
        // Completed but unread replies still consume their bounded slot.
        assert!(matches!(
            directory.submit(WorkerId(0), Work::Retained(version()), &scope, None),
            Err(Error::Overloaded)
        ));
        let Poll::Ready(Ok(completion)) = receipt.as_mut().poll(&mut cx) else {
            panic!("completion not ready");
        };
        let budget = completion.budget.unwrap();
        assert_eq!(
            (
                budget.remaining_attempts(),
                budget.remaining_links(),
                budget.deadline()
            ),
            (3, 5, scope.deadline.0)
        );
        assert!(matches!(completion.value, Ok(Value::Retained(None))));
        drop(receipt);
        assert_eq!(directory.mailboxes[0].outstanding(), 0);
    }
    #[test]
    fn metadata_owner_matches_page_zero_across_versions_and_closed_mailboxes_reject() {
        let directory = directory(2);
        let mut page = PageId {
            version: version(),
            number: crate::model::PageNumber(0),
        };
        let owner = directory.metadata_owner(&page.version.object).unwrap();
        assert_eq!(directory.page_owner(&page).unwrap(), owner);
        page.version.etag = StrongEtag::test_value("another-version");
        assert_eq!(directory.page_owner(&page).unwrap(), owner);
        directory.mailbox(owner).unwrap().stop_admission();
        assert!(matches!(
            directory.submit(owner, Work::Retained(version()), &scope(), None),
            Err(Error::Unavailable)
        ));
    }
    #[test]
    fn handoffs_are_owned_and_send_safe() {
        fn send<T: Send + 'static>() {}
        fn sync<T: Sync>() {}
        send::<Command>();
        send::<Completion>();
        send::<WorkerDirectory>();
        sync::<WorkerDirectory>();
    }
}
