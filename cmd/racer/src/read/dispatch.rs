//! Bounded node-local handoffs. Only owned commands and immutable results cross
//! threads; the coordinator, futures, delivery leases, and streams stay local.
//! Stable local page assignment is independent of cluster placement. Drain flights
//! before changing the worker map; no live remapping is implied.

use super::Coordinator;
use super::flight::AcquisitionBudget;
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
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use crate::security::PeerOriginContext;
use crate::store::holds::{HoldEpoch, HoldPages};
use racer_io::xthread::command as mailbox;
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

#[cfg(test)]
type Completion = mailbox::Completion<Result<Value>, AcquisitionBudget>;
#[cfg(test)]
type Command = mailbox::Command<Work, Result<Value>, AcquisitionBudget, RequestScope>;
type Receipt = mailbox::Receipt<Work, Result<Value>, AcquisitionBudget, RequestScope>;
type Handoff = mailbox::Mailbox<Work, Result<Value>, AcquisitionBudget, RequestScope>;

struct Mailbox {
    worker: WorkerId,
    inner: Arc<Handoff>,
    /// Failed GPU verdicts the mailbox could not carry. The owning endpoint
    /// applies them on its next poll so a corrupt copy is never left eligible.
    failed_verdicts: std::sync::Mutex<Vec<super::sealed::SealedReport>>,
}

/// Bound on retained failed verdicts per worker. Each entry is one failed GPU
/// verification; beyond this the owner is far behind and the oldest are kept.
const FAILED_VERDICT_LIMIT: usize = 4096;

impl Mailbox {
    /// Retain a failed verdict for the owner. Returns false only when full.
    fn retain_failed_verdict(&self, report: super::sealed::SealedReport) -> bool {
        let mut pending = self
            .failed_verdicts
            .lock()
            .unwrap_or_else(|poison| poison.into_inner());
        if pending.len() >= FAILED_VERDICT_LIMIT {
            return false;
        }
        pending.push(report);
        true
    }

    fn take_failed_verdicts(&self) -> Vec<super::sealed::SealedReport> {
        std::mem::take(
            &mut *self
                .failed_verdicts
                .lock()
                .unwrap_or_else(|poison| poison.into_inner()),
        )
    }
}
impl std::ops::Deref for Mailbox {
    type Target = Handoff;
    fn deref(&self) -> &Handoff {
        &self.inner
    }
}

/// One worker's view of the [`NodeDirectory`]: the shared map and mailboxes
/// plus this worker's own coordinator, bound by [`WorkerDirectory::install`].
/// Each worker thread builds its own view from the node graph, so no Rc
/// enters the shared directory and no thread-local lookup is needed. Tests that
/// model several workers on one thread may share one view and select the
/// current worker with `simulation_scope`.
pub struct WorkerDirectory {
    shared: Arc<NodeDirectory>,
    local: RefCell<Option<(WorkerId, Weak<Coordinator>)>>,
}

impl std::ops::Deref for WorkerDirectory {
    type Target = NodeDirectory;
    fn deref(&self) -> &NodeDirectory {
        &self.shared
    }
}

/// The map is immutable for the lifetime of this directory. Construct a replacement
/// only after all endpoints and completion receipts have drained.
pub struct NodeDirectory {
    /// Node-wide prefetch tickets, scheduler, and owner budget.
    pub prefetch: Arc<crate::prefetch::service::Prefetch>,
    spill: std::sync::Mutex<super::spill::Config>,
    pub(crate) subscriptions: Arc<super::range_stream::Scheduler>,
    map: Arc<WorkerMap>,
    /// Node-wide time and entropy.
    ctx: racer_io::WorkerCtx,
    mailboxes: Vec<Arc<Mailbox>>,
    sequence: AtomicU64,
}

enum Work {
    Hold(ObjectVersion, HoldPages, std::time::SystemTime),
    /// Drop one holder from each page of this worker's hold for the version.
    Release(ObjectVersion, HoldPages, HoldEpoch),
    /// Fill a spilled page a read holds and persist it under that hold.
    SpillFill(
        PageId,
        crate::model::VersionMetadata,
        Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    WarmLocal(PageId),
    /// Fill one prefetch page on its owning worker and persist it normally.
    PrefetchFill(
        PageId,
        crate::model::VersionMetadata,
        Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    /// Whether this worker's disk index holds the page.
    Probe(PageId),
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
    /// One read window page through ordinary Fill.
    Fetch(
        PageId,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
    ),
    /// One read window page for direct GPU delivery: like `Fetch`, but
    /// ciphertext the GPU can verify is returned sealed.
    FetchSealed(
        PageId,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
        super::sealed::SealedTarget,
    ),
    /// The GPU's verdict on a sealed page this worker supplied.
    SealedVerdict(super::sealed::SealedReport, bool),
    Publish(VersionMetadata),
    Retained(ObjectVersion),
    Peer(VerifiedRequest, std::sync::Arc<crate::topology::Membership>),
}
enum Value {
    Held(HoldEpoch),
    Metadata(ObjectMetadata),
    Page(PageResult),
    Fetched(super::sealed::Fetched),
    Cached(Option<PageResult>),
    Published,
    Retained(Option<VersionMetadata>),
    Peer(PeerResponse),
    Prefetched(racer_core::prefetch::PageResult),
    Resident(bool),
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
    directory: std::rc::Rc<WorkerDirectory>,

    mailbox: Arc<Mailbox>,

    local: Rc<Coordinator>,

    active: Executor,
}

impl WorkerDirectory {
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
    pub(crate) fn observe_verified(
        &self,
        drivers: &std::rc::Rc<crate::drivers::DriverQueue>,
        page: PageResult,
        scope: &RequestScope,
    ) {
        let Ok(owner) = self.page_owner(page.plaintext.page()) else {
            return;
        };
        // Reserve bounded worker-owned progress BEFORE retaining a mailbox
        // receipt. Dropping or delaying this optional task cannot hold delivery.
        let Ok(permit) = drivers.reserve() else {
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
    ) -> Result<Operation<'static, PageResult>> {
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
            match receipt.await?.value? {
                Value::Page(result) => Ok(result),
                _ => Err(Error::StaleFlight),
            }
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
    #[cfg(any(test, feature = "dst"))]
    pub(crate) fn simulation_crash(&self) {
        for mailbox in &self.mailboxes {
            mailbox.stop_admission();
            drop(mailbox.take_queued());
        }
    }
    /// Select a logical worker on the single-threaded simulation scheduler.
    /// Production gets this isolation from its owning OS thread.
    #[cfg(any(test, feature = "dst"))]
    pub(crate) fn simulation_scope(
        self: &Rc<Self>,
        local: Option<(WorkerId, Rc<Coordinator>)>,
    ) -> SimulationLocal {
        let previous = self
            .local
            .replace(local.map(|(worker, local)| (worker, Rc::downgrade(&local))));
        SimulationLocal {
            directory: self.clone(),
            previous,
        }
    }
    /// A directory with a fresh node-wide state and one unbound view of it.
    pub fn new(
        map: Arc<WorkerMap>,
        workers: Vec<WorkerId>,
        capacity: usize,
        ctx: racer_io::WorkerCtx,
    ) -> Result<Self> {
        Ok(Arc::new(NodeDirectory::new(map, workers, capacity, ctx)?).view())
    }

    /// The node-wide state shared by every worker's view.
    pub fn node(&self) -> &Arc<NodeDirectory> {
        &self.shared
    }

    pub fn install(
        self: &Rc<Self>,
        worker: WorkerId,
        local: Rc<Coordinator>,
    ) -> Result<WorkerEndpoint> {
        local
            .fill
            .dependencies
            .writer
            .index()
            .set_hold_capacity_percent(self.spill_config().retained_percent)?;
        let mailbox = self.mailbox(worker)?.clone();
        {
            let mut bound = self.local.borrow_mut();
            if bound
                .as_ref()
                .and_then(|(_, local)| local.upgrade())
                .is_some()
            {
                return Err(Error::InvalidConfiguration);
            }
            mailbox.install()?;
            *bound = Some((worker, Rc::downgrade(&local)));
        }
        Ok(WorkerEndpoint {
            directory: self.clone(),
            mailbox,
            local,
            active: Executor::default(),
        })
    }

    /// This worker's read coordinator.
    pub(crate) fn local_coordinator(&self) -> Result<Rc<Coordinator>> {
        self.local()
    }
    pub(super) fn local(&self) -> Result<Rc<Coordinator>> {
        self.local
            .borrow()
            .as_ref()
            .and_then(|(_, local)| local.upgrade())
            .ok_or(Error::Unavailable)
    }
    fn is_local(&self, owner: WorkerId) -> bool {
        self.local
            .borrow()
            .as_ref()
            .is_some_and(|(worker, _)| *worker == owner)
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
            Work::Hold(page.version.clone(), pages, until),
            scope,
            None,
        )?;
        match std::future::poll_fn(|cx| receipt.poll_completion(cx))
            .await?
            .value?
        {
            Value::Held(epoch) => Ok(epoch),
            _ => Err(Error::StaleFlight),
        }
    }

    /// Drop one holder from each page without waiting. Releases run on their
    /// owning workers under a fresh short scope, so a canceled read still
    /// returns its holds; hold expiry is the backstop if this cannot submit.
    pub(super) fn release_read_pages(
        &self,
        drivers: &std::rc::Rc<crate::drivers::DriverQueue>,
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
            let Ok(permit) = drivers.reserve() else {
                continue;
            };
            let clock = self.ctx.clock.clone();
            let Ok(scope) = RequestScope::with_clock(
                request,
                clock.now() + std::time::Duration::from_secs(30),
                clock,
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

    /// Start a fill of a page a read holds: the owner fills it and persists it
    /// under the hold. The context is sealed before returning.
    pub(super) fn start_spill_page(
        &self,
        page: PageId,
        metadata: crate::model::VersionMetadata,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<Operation<'static, ()>> {
        let owner = self.page_owner(&page)?;
        let work = Work::SpillFill(page, metadata, membership, self.seal(context, scope)?);
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

    /// Read a page this node holds without any network transfer: live
    /// plaintext first, then one coalesced decrypt of pending, cached, or disk
    /// ciphertext. The memory cache retains only ciphertext, so a plaintext
    /// miss alone must not send the page to a peer or origin transfer.
    /// Returns `None` when no local copy exists.
    pub(crate) async fn local_page(
        &self,
        page: PageId,
        scope: &RequestScope,
    ) -> Result<Option<PageResult>> {
        if let Some(result) = self.cached_page(page.clone(), scope).await? {
            return Ok(Some(result));
        }
        match self.held_page(page, scope).await {
            Ok(page) => Ok(Some(page)),
            Err(Error::Unavailable) => Ok(None),
            Err(error) => Err(error),
        }
    }

    /// Fill one prefetch page on its owning worker. Errors are whole-page
    /// failures for the caller to classify.
    pub(crate) async fn prefetch_page(
        &self,
        page: PageId,
        metadata: crate::model::VersionMetadata,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<racer_core::prefetch::PageResult> {
        let owner = self.page_owner(&page)?;
        let work = Work::PrefetchFill(page, metadata, membership, self.seal(context, scope)?);
        let receipt = self.submit(owner, work, scope, Some(super::default_budget(scope)))?;
        match std::future::poll_fn(|cx| receipt.poll_completion(cx))
            .await?
            .value?
        {
            Value::Prefetched(result) => Ok(result),
            _ => Err(Error::StaleFlight),
        }
    }

    /// Whether the page's owning worker has it on disk.
    pub(crate) async fn probe_page(&self, page: PageId, scope: &RequestScope) -> Result<bool> {
        let owner = self.page_owner(&page)?;
        if self.is_local(owner) {
            return probe(&*self.local()?, &page);
        }
        match self
            .submit(owner, Work::Probe(page), scope, None)?
            .await?
            .value?
        {
            Value::Resident(resident) => Ok(resident),
            _ => Err(Error::StaleFlight),
        }
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
    ) -> Result<Operation<'static, PageResult>> {
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
            match receipt.await?.value? {
                Value::Page(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        }))
    }
    /// Like [`Self::start_page`], but ciphertext the GPU behind `target`
    /// can verify comes back sealed instead of decrypted on the CPU.
    pub(crate) fn start_page_sealed(
        &self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: AcquisitionBudget,
        target: super::sealed::SealedTarget,
    ) -> Result<Operation<'static, super::sealed::Fetched>> {
        if page.version.object != context.object {
            return Err(Error::InvalidRequest);
        }
        let owner = self.page_owner(&page)?;
        let receipt = self.submit(
            owner,
            Work::FetchSealed(page, membership, self.seal(context, scope)?, target),
            scope,
            Some(budget),
        )?;
        Ok(Box::pin(async move {
            match receipt.await?.value? {
                Value::Fetched(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        }))
    }

    /// Tell a sealed page's owner what the GPU decided, in the background.
    pub(crate) fn report_sealed(
        &self,
        drivers: &std::rc::Rc<crate::drivers::DriverQueue>,
        report: super::sealed::SealedReport,
        passed: bool,
        scope: &RequestScope,
    ) {
        let Ok(owner) = self.page_owner(&report.page) else {
            return;
        };
        // A passed landed page has nothing for its owner to act on, so its
        // verdict is settled here and never takes a retained slot. A failed
        // one goes to the owner, which may be sharing the landed copy.
        if self.is_local(owner) || !report.has_owner_state(passed) {
            if let Ok(local) = self.local() {
                local.fill.sealed_verdict(report, passed);
            }
            return;
        }
        // A failed verdict must reach the owner, or the corrupt copy stays
        // eligible. When the driver queue or mailbox cannot take it, retain
        // it on the owner's mailbox for the owner's next poll.
        let retain = |report: super::sealed::SealedReport| {
            if !passed && let Ok(mailbox) = self.mailbox(owner) {
                mailbox.retain_failed_verdict(report);
            }
        };
        let Ok(permit) = drivers.reserve() else {
            retain(report);
            return;
        };
        // The request may already be ending; the verdict must still arrive.
        let deadline = scope.clock().now() + std::time::Duration::from_secs(5);
        let Ok(detached) = scope.child(scope.request, deadline) else {
            retain(report);
            return;
        };
        let kept = (!passed).then(|| report.clone());
        match self.submit(owner, Work::SealedVerdict(report, passed), &detached, None) {
            Ok(receipt) => permit.submit_detached(Box::pin(async move {
                let _ = receipt.await;
                Ok::<(), Error>(())
            })),
            Err(_) => {
                if let Some(report) = kept {
                    retain(report);
                }
            }
        }
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
    /// Run `f` as if this thread did not own its installed worker, so local
    /// work is queued on the mailbox as another worker's request would be.
    #[cfg(test)]
    pub(crate) fn as_remote<T>(&self, f: impl FnOnce() -> T) -> T {
        let entry = self.local.borrow_mut().take();
        let output = f();
        if let Some(entry) = entry {
            self.local.replace(Some(entry));
        }
        output
    }
    /// Queue a publication on the metadata owner's mailbox, as a fill on
    /// another worker would, even when the owner is this thread.
    #[cfg(test)]
    pub(crate) fn queue_publication<'a>(
        &'a self,
        metadata: VersionMetadata,
        scope: &'a RequestScope,
    ) -> Result<Operation<'a, ()>> {
        let owner = self.metadata_owner(&metadata.version.object)?;
        let receipt = self.submit(owner, Work::Publish(metadata), scope, None)?;
        Ok(Box::pin(async move {
            match receipt.await?.value? {
                Value::Published => Ok(()),
                _ => Err(Error::StaleFlight),
            }
        }))
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
                // Prefetch fans out from the object's metadata worker to each
                // page's owning worker.
                crate::peer::protocol::Operation::Prefetch { version, .. } => {
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

#[cfg(any(test, feature = "dst"))]
pub(crate) struct SimulationLocal {
    directory: std::rc::Rc<WorkerDirectory>,
    previous: Option<(WorkerId, std::rc::Weak<Coordinator>)>,
}
#[cfg(any(test, feature = "dst"))]
impl Drop for SimulationLocal {
    fn drop(&mut self) {
        self.directory.local.replace(self.previous.take());
    }
}

impl WorkerEndpoint {
    /// The local worker's detached driver queue.
    #[cfg(test)]
    pub(crate) fn drivers(&self) -> &std::rc::Rc<crate::drivers::DriverQueue> {
        self.local.drivers()
    }

    /// Simulated process loss occurs after the OS crash cut. Drop accepted tasks
    /// without polling them; the simulated reactor still fences owned buffers.
    #[cfg(any(test, feature = "dst"))]
    pub(crate) fn simulation_crash(&mut self) {
        self.directory.simulation_crash();
        self.active.simulation_crash();
    }
    /// Apply failed GPU verdicts other workers could not send through the mailbox.
    fn apply_failed_verdicts(&self) {
        for report in self.mailbox.take_failed_verdicts() {
            self.local.fill.sealed_verdict(report, false);
        }
    }
    pub fn stop_admission(&self) {
        self.directory.prefetch.stop();
        self.mailbox.stop_admission();
    }
    pub fn is_drained(&self) -> bool {
        self.mailbox.outstanding() == 0
    }
    /// Close admission and continue reaping every accepted command. An expired
    /// shutdown scope requests cancellation but never substitutes for completion.
    pub fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.stop_admission();
        Box::pin(std::future::poll_fn(move |cx| {
            self.apply_failed_verdicts();
            self.active
                .poll_drain(&self.mailbox, scope, cx, self.local.clone())
        }))
    }
    /// Register a reactor waker and execute at most `work_budget` command/poll steps.
    pub fn poll(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        self.apply_failed_verdicts();
        self.directory.prefetch.tick();
        self.active
            .poll(&self.mailbox, cx, work_budget, self.local.clone())
    }
    pub fn poll_budgeted(&mut self, work_budget: usize) -> Result<()> {
        let waker = futures::task::noop_waker();
        self.poll(&mut Context::from_waker(&waker), work_budget)
    }
    /// Earliest instant [`Self::poll`] has timed work: a caller deadline or a
    /// node-wide prefetch expiry. The owning lane parks no later than this.
    pub fn next_deadline(&self) -> Option<std::time::Instant> {
        self.active
            .next_deadline(&self.mailbox)
            .into_iter()
            .chain(self.directory.prefetch.next_wake())
            .min()
    }
    pub fn uninstall(&mut self) -> Result<()> {
        self.stop_admission();
        if !self.is_drained() || !self.active.is_empty() {
            return Err(Error::Unavailable);
        }
        self.mailbox.uninstall()?;
        self.directory.local.borrow_mut().take();
        Ok(())
    }
}
impl Drop for WorkerEndpoint {
    fn drop(&mut self) {
        self.stop_admission();
        self.directory.local.borrow_mut().take();
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

/// Whether this worker's disk index holds `page`.
fn probe(local: &Coordinator, page: &PageId) -> Result<bool> {
    Ok(local
        .fill
        .dependencies
        .writer
        .index()
        .lookup(page)?
        .is_some())
}

async fn execute(
    local: &Coordinator,
    work: Work,
    scope: &RequestScope,
    budget: Option<&mut AcquisitionBudget>,
) -> Result<Value> {
    scope.check()?;
    match work {
        // Holds and spill fills retain pages, which a passthrough cache never does.
        Work::Hold(version, ..) if local.fill.passthrough(&version.object.cache) => {
            Err(Error::Unavailable)
        }
        Work::SpillFill(page, ..) if local.fill.passthrough(&page.version.object.cache) => {
            Err(Error::Unavailable)
        }
        Work::PrefetchFill(page, ..) if local.fill.passthrough(&page.version.object.cache) => {
            Ok(Value::Prefetched(racer_core::prefetch::PageResult::Failed(
                racer_core::prefetch::FailCode::Unavailable,
            )))
        }
        Work::Hold(version, pages, until) => local
            .fill
            .dependencies
            .writer
            .index()
            .reserve_hold_pages(&version, &pages, until)
            .map(Value::Held),
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
        Work::Probe(page) => probe(local, &page).map(Value::Resident),
        Work::PrefetchFill(page, metadata, membership, envelope) => {
            if probe(local, &page)? {
                return Ok(Value::Prefetched(
                    racer_core::prefetch::PageResult::Resident,
                ));
            }
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
            // Never persist or acknowledge a same-ETag copy whose immutable
            // metadata disagrees with the requester's admitted metadata.
            if !metadata.compatible(&result.metadata.immutable()) {
                return Ok(Value::Prefetched(racer_core::prefetch::PageResult::Failed(
                    racer_core::prefetch::FailCode::VersionChanged,
                )));
            }
            local
                .fill
                .dependencies
                .writer
                .persist_prefetched(result.copy(), scope)
                .await?;
            Ok(Value::Prefetched(
                racer_core::prefetch::PageResult::Admitted,
            ))
        }
        Work::SpillFill(page, metadata, membership, envelope) => {
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
            // A demand read joining a prefetch flight promotes it.
            let _promoted = local.streams.directory().prefetch.promote(&page);
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
        Work::FetchSealed(page, membership, envelope, target) => {
            // A demand read joining a prefetch flight promotes it.
            let _promoted = local.streams.directory().prefetch.promote(&page);
            let context = local.open_context(envelope)?;
            local
                .fill
                .acquire_sealed(
                    page,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                    &target,
                )
                .await
                .map(Value::Fetched)
        }
        Work::SealedVerdict(report, passed) => {
            local.fill.sealed_verdict(report, passed);
            Ok(Value::Published)
        }
        Work::Publish(metadata) => {
            // A fill queued while the cache was cached may arrive after the
            // switch to passthrough and its one-time purge. Drop it so the
            // catalog never retains passthrough metadata.
            if !local.fill.passthrough(&metadata.version.object.cache) {
                local.metadata.publish_version(metadata)?;
            }
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

impl NodeDirectory {
    pub fn new(
        map: Arc<WorkerMap>,
        workers: Vec<WorkerId>,
        capacity: usize,
        ctx: racer_io::WorkerCtx,
    ) -> Result<Self> {
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
            prefetch: Arc::new(crate::prefetch::service::Prefetch::new(ctx.clone())),
            spill: std::sync::Mutex::new(super::spill::Config::default()),
            subscriptions: super::range_stream::Scheduler::new(
                capacity.saturating_mul(workers.len()),
                ctx.clone(),
            ),
            map,
            ctx,
            mailboxes: workers
                .into_iter()
                .map(|worker| {
                    Arc::new(Mailbox {
                        worker,
                        inner: Arc::new(Handoff::new(capacity).expect("validated capacity")),
                        failed_verdicts: std::sync::Mutex::new(Vec::new()),
                    })
                })
                .collect(),
            sequence: AtomicU64::new(1),
        })
    }

    pub fn metadata_owner(&self, object: &ObjectId) -> Result<WorkerId> {
        self.map.metadata_owner(object)
    }
    pub fn page_owner(&self, page: &PageId) -> Result<WorkerId> {
        self.map.owner(page)
    }

    /// Apply read spill settings. Call before installing workers.
    pub fn configure_spill(&self, config: super::spill::Config) -> Result<()> {
        config.validate()?;
        *self.spill.lock().unwrap() = config;
        Ok(())
    }

    pub fn spill_config(&self) -> super::spill::Config {
        *self.spill.lock().unwrap()
    }

    fn mailbox(&self, worker: WorkerId) -> Result<&Arc<Mailbox>> {
        self.mailboxes
            .iter()
            .find(|m| m.worker == worker)
            .ok_or(Error::InvalidConfiguration)
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

    /// A new view for one worker, unbound until [`WorkerDirectory::install`].
    pub fn view(self: &Arc<Self>) -> WorkerDirectory {
        WorkerDirectory {
            shared: self.clone(),
            local: RefCell::new(None),
        }
    }
}

// PeerServer owns local services through Rc; retain this worker's directory view.
impl LocalPageService for std::rc::Rc<WorkerDirectory> {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        self.peer(request, membership, scope)
    }
}

/// Local worker assignment; the rule lives in racer-core.
pub struct WorkerMap(racer_core::read::WorkerMap);
impl WorkerMap {
    /// Canonical worker order makes assignment independent of discovery order.
    /// Changing this set requires draining all flights first.
    pub fn new(workers: Vec<WorkerId>) -> Result<Self> {
        Ok(Self(racer_core::read::WorkerMap::new(workers)?))
    }

    /// Same owner as this object's page zero, without needing an ETag first.
    pub fn metadata_owner(&self, object: &ObjectId) -> Result<WorkerId> {
        self.select(object, 0)
    }
    pub fn owner(&self, page: &PageId) -> Result<WorkerId> {
        self.select(&page.version.object, page.number.0)
    }

    fn select(&self, object: &ObjectId, page: u64) -> Result<WorkerId> {
        Ok(self.0.select(object, page)?)
    }

    #[cfg(test)]
    fn select_digest(&self, digest: &[u8; 32]) -> WorkerId {
        self.0.select_digest(digest)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use racer_wire::control::CacheId;
    use std::time::Duration;
    use std::time::Instant;
    /// A page owned by `worker` in the two-worker test map.
    fn page_owned_by(directory: &WorkerDirectory, worker: WorkerId) -> PageId {
        (0..1024)
            .map(|number| PageId {
                version: version(),
                number: crate::model::PageNumber(number),
            })
            .find(|page| directory.page_owner(page).unwrap() == worker)
            .unwrap()
    }
    fn failed_report(page: PageId) -> super::super::sealed::SealedReport {
        super::super::sealed::SealedReport {
            page: page.clone(),
            copy: None,
            token: Some(crate::store::ReadToken::for_test(page)),
            landed: false,
        }
    }
    /// A landed page's report: nothing for the owner to persist or drop,
    /// but a failure must reach the owner's shared GPU copies.
    fn landed_report(page: PageId) -> super::super::sealed::SealedReport {
        super::super::sealed::SealedReport {
            page,
            copy: None,
            token: None,
            landed: true,
        }
    }
    /// Deliver retained verdicts through the owner's endpoint and count them.
    fn applied_by_owner(env: &racer_io::Env, directory: &std::rc::Rc<WorkerDirectory>) -> u64 {
        let (owner, _fixture) = crate::read::tests::admission_coordinator(env);
        let before = owner
            .fill
            .metrics
            .count(crate::telemetry::Event::CorruptMiss);
        let mut endpoint = directory.install(WorkerId(1), owner.clone()).unwrap();
        endpoint.poll_budgeted(0).unwrap();
        let applied = owner
            .fill
            .metrics
            .count(crate::telemetry::Event::CorruptMiss)
            - before;
        drop(endpoint);
        applied
    }
    #[test]
    fn failed_sealed_verdict_survives_full_driver_queue() {
        let env = racer_io::Env::host();
        let queue = Rc::new(crate::drivers::DriverQueue::new(1));
        let workers = vec![WorkerId(0), WorkerId(1)];
        let directory = std::rc::Rc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(workers.clone()).unwrap()),
                workers,
                4,
                racer_io::WorkerCtx::host(),
            )
            .unwrap(),
        );
        let page = page_owned_by(&directory, WorkerId(1));
        let full = queue.reserve().unwrap();
        {
            let (local, _fixture) = crate::read::tests::admission_coordinator(&env);
            let _local = directory.simulation_scope(Some((WorkerId(0), local)));
            directory.report_sealed(&queue, failed_report(page.clone()), false, &scope());
            // A passing verdict is only a persistence hint and is not retained.
            directory.report_sealed(&queue, failed_report(page), true, &scope());
        }
        drop(full);
        assert_eq!(applied_by_owner(&env, &directory), 1);
        assert!(directory.mailboxes[1].take_failed_verdicts().is_empty());
    }
    #[test]
    fn failed_sealed_verdict_survives_full_mailbox() {
        let env = racer_io::Env::host();
        let queue = Rc::new(crate::drivers::DriverQueue::new(64));
        let workers = vec![WorkerId(0), WorkerId(1)];
        let directory = std::rc::Rc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(workers.clone()).unwrap()),
                workers,
                1,
                racer_io::WorkerCtx::host(),
            )
            .unwrap(),
        );
        directory.mailboxes[1].install().unwrap();
        let page = page_owned_by(&directory, WorkerId(1));
        {
            let (local, _fixture) = crate::read::tests::admission_coordinator(&env);
            let _local = directory.simulation_scope(Some((WorkerId(0), local)));
            // The first verdict takes the only mailbox slot; the second is retained.
            directory.report_sealed(&queue, failed_report(page.clone()), false, &scope());
            assert_eq!(directory.mailboxes[1].outstanding(), 1);
            directory.report_sealed(&queue, failed_report(page), false, &scope());
        }
        assert_eq!(directory.mailboxes[1].take_failed_verdicts().len(), 1);
    }
    /// Run `report_sealed` for a landed page owned by worker 1 from worker 0,
    /// and return worker 0's new CorruptMiss count and worker 1's mailbox.
    fn landed_verdict(env: &racer_io::Env, passed: bool) -> (u64, usize) {
        let queue = Rc::new(crate::drivers::DriverQueue::new(64));
        let workers = vec![WorkerId(0), WorkerId(1)];
        let directory = std::rc::Rc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(workers.clone()).unwrap()),
                workers,
                4,
                racer_io::WorkerCtx::host(),
            )
            .unwrap(),
        );
        directory.mailboxes[1].install().unwrap();
        let page = page_owned_by(&directory, WorkerId(1));
        let (local, _fixture) = crate::read::tests::admission_coordinator(env);
        let before = local
            .fill
            .metrics
            .count(crate::telemetry::Event::CorruptMiss);
        {
            let _local = directory.simulation_scope(Some((WorkerId(0), local.clone())));
            directory.report_sealed(&queue, landed_report(page), passed, &scope());
        }
        let local_misses = local
            .fill
            .metrics
            .count(crate::telemetry::Event::CorruptMiss)
            - before;
        let outstanding = directory.mailboxes[1].outstanding();
        assert!(directory.mailboxes[1].take_failed_verdicts().is_empty());
        (local_misses, outstanding)
    }
    #[test]
    /// The owner may share a landed copy with other sealed fetches, so a
    /// failed verdict goes to it rather than settling on the reporter.
    fn failed_landed_verdict_goes_to_the_owner() {
        let env = racer_io::Env::host();
        assert_eq!(landed_verdict(&env, false), (0, 1));
    }
    #[test]
    /// A passed landed page leaves the owner nothing to do.
    fn passed_landed_verdict_settles_locally() {
        let env = racer_io::Env::host();
        assert_eq!(landed_verdict(&env, true), (0, 0));
    }
    #[test]
    fn failed_sealed_verdicts_are_bounded() {
        let directory = directory(1);
        let page = page_owned_by(&directory, WorkerId(1));
        for _ in 0..FAILED_VERDICT_LIMIT {
            assert!(directory.mailboxes[1].retain_failed_verdict(failed_report(page.clone())));
        }
        assert!(!directory.mailboxes[1].retain_failed_verdict(failed_report(page)));
        assert_eq!(
            directory.mailboxes[1].take_failed_verdicts().len(),
            FAILED_VERDICT_LIMIT
        );
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
        use racer_wire::control::CacheId;
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
            racer_io::WorkerCtx::host(),
        )
        .unwrap();
        for mailbox in &directory.mailboxes {
            mailbox.install().unwrap();
        }
        directory
    }
    fn scope() -> RequestScope {
        RequestScope::host(RequestId([1; 16]), Instant::now() + Duration::from_secs(60)).unwrap()
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
                    value: Err(Error::Canceled),
                    budget: None,
                },
            )
            .unwrap();
        assert!(matches!(
            selected.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Canceled))
        ));
        drop((selected, command));
        assert_eq!(directory.mailbox(owner).unwrap().outstanding(), 0);
    }

    #[test]
    fn expired_drain_completes_queued_selected_waiters_and_reclaims_ciphertext() {
        use crate::admission::ResourceClass;
        let env = racer_io::Env::host();
        for cancel_caller in [false, true] {
            let directory = Rc::new(directory(1));
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
                local: crate::test_support::wake_test_coordinator(&env),
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
            assert_eq!(
                admission.used(ResourceClass::Ciphertext),
                page_alloc::buf::GRANULE
            );
            if cancel_caller {
                caller.cancel().unwrap();
                assert!(selected.as_mut().poll(&mut cx).is_pending());
            }
            let shutdown = RequestScope::host(RequestId([2; 16]), Instant::now()).unwrap();
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
                Poll::Ready(Err(Error::Canceled))
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
        let env = racer_io::Env::host();
        for detach in [false, true] {
            let directory = Rc::new(directory(1));
            let mut endpoint = WorkerEndpoint {
                mailbox: directory.mailboxes[0].clone(),
                directory: directory.clone(),
                local: crate::test_support::wake_test_coordinator(&env),
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
            let shutdown = RequestScope::host(RequestId([2; 16]), Instant::now()).unwrap();
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
                assert!(matches!(completion.value, Err(Error::Canceled)));
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
        for error in [None, Some(Error::Canceled), Some(Error::CorruptRecord)] {
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
        let env = racer_io::Env::host();
        for submit_before_poll in [false, true] {
            for complete_before_poll in [false, true] {
                let directory = Rc::new(directory(4));
                let mut endpoint = WorkerEndpoint {
                    mailbox: directory.mailboxes[0].clone(),
                    directory: directory.clone(),
                    local: crate::test_support::wake_test_coordinator(&env),
                    active: Executor::default(),
                };
                let count = Arc::new(crate::test_support::WakeCounter::default());
                let waker = Waker::from(count.clone());
                let mut cx = Context::from_waker(&waker);
                if !submit_before_poll {
                    endpoint.poll(&mut cx, 0).unwrap();
                }
                let sender = directory.node().clone();
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
        let env = racer_io::Env::host();
        let directory = Rc::new(directory(4));
        let (local, _fixture) = crate::read::tests::admission_coordinator(&env);
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
            racer_wire::control::CacheId(crate::test_support::security::CACHE.into());
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
            crate::topology::Membership::validate(
                racer_wire::control::MembershipVersion(1),
                vec![],
            )
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
        // Only the node-wide state crosses threads; each worker owns its view.
        send::<Arc<NodeDirectory>>();
        sync::<NodeDirectory>();
    }
}
