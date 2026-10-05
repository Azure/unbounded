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
use sha2::Digest;
use sha2::Sha256;
use std::cell::RefCell;
use std::collections::VecDeque;
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
use std::task::Poll;
#[cfg(test)]
use std::task::Waker;
use uring_runtime::mailbox;

type Completion = mailbox::Completion<Result<Value>, AcquisitionBudget>;
type Reply = mailbox::Reply<Result<Value>, AcquisitionBudget>;
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
    pub(crate) subscriptions: Arc<super::range_stream::Scheduler>,
    map: Arc<WorkerMap>,
    mailboxes: Vec<Arc<Mailbox>>,
    sequence: AtomicU64,
}

enum Work {
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
    Ordered(
        PageId,
        std::sync::Arc<crate::topology::Membership>,
        PeerOriginContext,
        Arc<super::range_stream::FixedAcquisition>,
    ),
    Publish(VersionMetadata),
    Retained(ObjectVersion),
    Peer(VerifiedRequest, std::sync::Arc<crate::topology::Membership>),
}
enum Value {
    Metadata(ObjectMetadata),
    Page(PageResult),
    Cached(Option<PageResult>),
    Published,
    Retained(Option<VersionMetadata>),
    Peer(PeerResponse),
}
struct Active {
    cancellation: Result<uring_runtime::environment::CancellationRegistration>,
    runnable: Arc<uring_runtime::drivers::Runnable>,
    future: Operation<'static, ()>,
    scope: RequestScope,
    caller: RequestScope,
    reply: Arc<Reply>,
}

/// Worker-owned command driver. Poll it even after ingress requests disconnect.
/// Active futures are retained until completion; cancel is never a completion fence.
pub struct WorkerEndpoint {
    directory: Arc<WorkerDirectory>,
    mailbox: Arc<Mailbox>,
    local: Rc<Coordinator>,
    active: VecDeque<Active>,
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
        // stream must not release receiving exclusivity before accepted I/O fences.
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
        // Cancellation notifies the owner, but selection exclusivity must survive
        // until its accepted crypto and publication work actually completes.
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
            active: VecDeque::new(),
        })
    }

    fn mailbox(&self, worker: WorkerId) -> Result<&Arc<Mailbox>> {
        self.mailboxes
            .iter()
            .find(|m| m.worker == worker)
            .ok_or(Error::InvalidConfiguration)
    }
    fn local(&self) -> Result<Rc<Coordinator>> {
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
    /// Always enqueue, including local ingress. The command and elected Fill
    /// driver, rather than the detachable receipt, own mixed-mode exclusion.
    pub(crate) fn start_ordered_page(
        &self,
        page: PageId,
        guard: super::range_stream::FixedAcquisition,
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
            Work::Ordered(
                page,
                membership,
                self.seal(context, scope)?,
                Arc::new(guard),
            ),
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
                        if value.version != *version
                            || found.as_ref().is_some_and(|old| !old.compatible(&value))
                        {
                            return Err(Error::CorruptRecord);
                        }
                        found = Some(value);
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
        self.active.clear();
    }
    pub fn stop_admission(&self) {
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
            if scope.check().is_err() {
                for active in &self.active {
                    let _ = active.scope.cancel();
                }
                // Unstarted work is fenced by non-submission, not abandonment:
                // completion-only waiters must receive an explicit result. Take
                // ownership before releasing ciphertext, budgets, and permits;
                // permit release locks this same mailbox.
                let queued = self.mailbox.take_queued();
                for mut command in queued {
                    let _ = command.reply.complete(
                        command.generation,
                        Completion {
                            value: Err(Error::Cancelled),
                            budget: command.budget.take(),
                        },
                    );
                }
            }
            if let Err(error) = self.poll(cx, 64) {
                return Poll::Ready(Err(error));
            }
            if self.is_drained() {
                Poll::Ready(Ok(()))
            } else {
                Poll::Pending
            }
        }))
    }
    /// Register a reactor waker and execute at most `work_budget` command/poll steps.
    pub fn poll(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        self.mailbox.register(cx.waker());
        let mut remaining_polls = self.active.len();
        for _ in 0..work_budget {
            let command = self.mailbox.pop();
            if let Some(command) = command {
                remaining_polls += 1;
                let local = self.local.clone();
                let caller = command.scope.clone();
                let mut scope = caller.clone();
                scope.cancellation = Cancellation::new()?;
                let active_scope = scope.clone();
                let reply = command.reply.clone();
                self.active.push_back(Active {
                    cancellation: caller.cancellation.subscribe(),
                    runnable: uring_runtime::drivers::Runnable::new(),
                    scope: active_scope,
                    caller,
                    reply,
                    future: Box::pin(async move {
                        let Command {
                            generation,
                            work,
                            mut budget,
                            reply,
                            permit,
                            ..
                        } = command;
                        let value = if reply.is_abandoned() {
                            Err(Error::Cancelled)
                        } else {
                            execute(&local, work, &scope, budget.as_mut()).await
                        };
                        // Only actual completion releases command resources. The
                        // receipt retains the slot until its result is consumed.
                        let completed = reply.complete(generation, Completion { value, budget });
                        drop(permit);
                        completed.map_err(|_| Error::StaleFlight)
                    }),
                });
            }
            if remaining_polls != 0
                && let Some(mut active) = self.active.pop_front()
            {
                remaining_polls -= 1;
                if let Ok(cancellation) = &active.cancellation {
                    cancellation.register(cx.waker());
                }
                if active.cancellation.is_err()
                    || active.reply.is_abandoned()
                    || active.caller.check().is_err()
                {
                    let _ = active.scope.cancel();
                }
                let force = active.scope.check().is_err();
                match active
                    .runnable
                    .poll(std::pin::Pin::new(&mut active.future), cx, force)
                {
                    Poll::Pending => self.active.push_back(active),
                    Poll::Ready(result) => result?,
                }
            }
        }
        // Active length is not runnable work: every future may be blocked. Keep
        // round-robin order; the bounded worker tick reaches the rest of the set.
        if work_budget != 0 && self.mailbox.has_queued() {
            cx.waker().wake_by_ref();
        }
        Ok(())
    }
    pub fn poll_budgeted(&mut self, work_budget: usize) -> Result<()> {
        let waker = futures::task::noop_waker();
        self.poll(&mut Context::from_waker(&waker), work_budget)
    }
    /// Earliest caller deadline. The I/O loop currently uses its bounded 1 ms tick
    /// to check deadlines rather than scheduling this value directly.
    pub fn next_deadline(&self) -> Option<std::time::Instant> {
        self.active
            .iter()
            .map(|active| active.caller.deadline.0)
            .chain(self.mailbox.queued_min(|scope| scope.deadline.0))
            .min()
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
        let queued = self.mailbox.take_queued();
        for mut command in queued {
            let _ = command.reply.complete(
                command.generation,
                Completion {
                    value: Err(Error::Unavailable),
                    budget: command.budget.take(),
                },
            );
        }
        // Normal shutdown drains before dropping this owner. If integration
        // violates that contract, fail closed: never free futures/buffers that
        // could still be referenced by accepted I/O. No replacement can install.
        if !self.active.is_empty() {
            std::mem::forget(std::mem::take(&mut self.active));
        }
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
        Work::Observed(page) => {
            local.fill.observe_verified_scoped(&page, scope).await;
            Ok(Value::Published)
        }
        Work::Select(version, selection, membership, envelope) => {
            let context = local.open_context(envelope)?;
            let page = local
                .fill
                .select_subscription(
                    version,
                    selection.demand.clone(),
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
        Work::Ordered(page, membership, envelope, guard) => {
            let context = local.open_context(envelope)?;
            local
                .fill
                .acquire_ordered(
                    page,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                    guard,
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
        assert!(WorkerMap::new(vec![]).is_err());
        assert!(WorkerMap::new(vec![WorkerId(1), WorkerId(1)]).is_err());
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
                active: VecDeque::new(),
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
                active: VecDeque::new(),
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
    fn expired_drain_keeps_active_work_until_its_completion_fence() {
        let directory = Arc::new(directory(1));
        let mut endpoint = WorkerEndpoint {
            mailbox: directory.mailboxes[0].clone(),
            directory: directory.clone(),
            local: crate::test_support::wake_test_coordinator(),
            active: VecDeque::new(),
        };
        let caller = scope();
        let receipt = directory
            .submit(WorkerId(0), Work::Retained(version()), &caller, None)
            .unwrap();
        let command = endpoint.mailbox.pop().unwrap();
        let active_scope = scope();
        let (finish, fence) = futures::channel::oneshot::channel::<()>();
        endpoint.active.push_back(Active {
            cancellation: caller.cancellation.subscribe(),
            runnable: uring_runtime::drivers::Runnable::new(),
            scope: active_scope.clone(),
            caller,
            reply: command.reply.clone(),
            // Model accepted work whose cancellation is not a completion fence.
            future: Box::pin(async move {
                fence.await.map_err(|_| Error::Unavailable)?;
                command
                    .reply
                    .complete(
                        command.generation,
                        Completion {
                            value: Err(Error::Cancelled),
                            budget: None,
                        },
                    )
                    .map_err(|_| Error::StaleFlight)?;
                drop(command);
                Ok(())
            }),
        });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        endpoint.poll(&mut cx, 1).unwrap();
        let shutdown = RequestScope::new(RequestId([2; 16]), Instant::now()).unwrap();
        let mut drain = endpoint.drain(&shutdown);
        assert!(drain.as_mut().poll(&mut cx).is_pending());
        assert_eq!(active_scope.check(), Err(Error::Cancelled));
        assert!(receipt.poll_completion(&mut cx).is_pending());
        drop(receipt);
        assert_eq!(directory.mailboxes[0].outstanding(), 1);
        assert!(drain.as_mut().poll(&mut cx).is_pending());
        finish.send(()).unwrap();
        assert_eq!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        drop(drain);
        assert!(endpoint.active.is_empty());
        assert_eq!(directory.mailboxes[0].outstanding(), 0);
        endpoint.uninstall().unwrap();
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
        let legacy = VersionMetadata {
            version: version.clone(),
            length: 3,
            content_type: None,
        };
        let mut typed = legacy.clone();
        typed.content_type = Some(crate::model::ContentType::parse(b"text/plain").unwrap());
        let mut conflict = typed.clone();
        conflict.content_type = Some(crate::model::ContentType::parse(b"text/html").unwrap());
        let mut wrong_length = legacy.clone();
        wrong_length.length += 1;
        let mut wrong_version = legacy.clone();
        wrong_version.version.etag = StrongEtag::test_value("other");
        for (values, expected) in [
            ([legacy.clone(), legacy.clone()], Ok(Some(legacy.clone()))),
            ([typed.clone(), typed.clone()], Ok(Some(typed.clone()))),
            ([legacy.clone(), typed.clone()], Err(Error::CorruptRecord)),
            ([typed.clone(), legacy.clone()], Err(Error::CorruptRecord)),
            ([typed.clone(), conflict.clone()], Err(Error::CorruptRecord)),
            ([conflict, typed.clone()], Err(Error::CorruptRecord)),
            ([legacy.clone(), wrong_length], Err(Error::CorruptRecord)),
            ([legacy, wrong_version], Err(Error::CorruptRecord)),
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
    fn blocked_endpoint_is_quiet_and_round_robin_is_budgeted() {
        let directory = Arc::new(directory(4));
        let mut endpoint = WorkerEndpoint {
            mailbox: directory.mailboxes[0].clone(),
            directory,
            local: crate::test_support::wake_test_coordinator(),
            active: VecDeque::new(),
        };
        let count = Arc::new(crate::test_support::WakeCounter::default());
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);
        let order = Rc::new(RefCell::new(Vec::new()));
        for id in 0..3 {
            let order = order.clone();
            let caller = scope();
            endpoint.active.push_back(Active {
                cancellation: caller.cancellation.subscribe(),
                runnable: uring_runtime::drivers::Runnable::new(),
                scope: scope(),
                caller,
                reply: Arc::new(Reply::new(id)),
                future: Box::pin(std::future::poll_fn(move |_| {
                    order.borrow_mut().push(id);
                    Poll::Pending
                })),
            });
        }
        endpoint.poll(&mut cx, 0).unwrap();
        assert!(order.borrow().is_empty());
        for _ in 0..6 {
            endpoint.poll(&mut cx, 1).unwrap();
        }
        assert_eq!(&*order.borrow(), &[0, 1, 2]);
        assert_eq!(
            count.count(),
            0,
            "active length does not imply runnable work"
        );
        for active in &endpoint.active {
            std::task::Wake::wake_by_ref(&active.runnable);
        }
        endpoint.poll(&mut cx, 64).unwrap();
        assert_eq!(&*order.borrow(), &[0, 1, 2, 0, 1, 2]);
        endpoint.active.clear(); // Test futures have no accepted I/O to fence.
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
                    active: VecDeque::new(),
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
    #[test]
    fn slot_is_retained_until_both_completion_and_receipt_release() {
        let directory = directory(1);
        let mailbox = &directory.mailboxes[0];
        let receipt = directory
            .submit(WorkerId(0), Work::Retained(version()), &scope(), None)
            .unwrap();
        let accepted = mailbox.pop().unwrap();
        drop(receipt);
        assert_eq!(mailbox.outstanding(), 1);
        drop(accepted);
        assert_eq!(mailbox.outstanding(), 0);
    }
}
