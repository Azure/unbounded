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
use crate::security::OriginContext;
use crate::model::PageId;
use crate::security::PeerOriginContext;
use crate::model::VersionMetadata;
use crate::model::WorkerId;
use crate::peer::forwarding::VerifiedRequest;
use crate::peer::protocol::PeerResponse;
use crate::peer::server::LocalPageService;
use crate::runtime::Cancellation;
use crate::runtime::HashMap;
use crate::runtime::RequestScope;

use sha2::Digest;
use sha2::Sha256;
use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::rc::Weak;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
use std::task::Context;
use std::task::Poll;
use std::task::Waker;

thread_local! {
    // Installed only on the owning I/O thread. No Rc enters the shared directory.
    static LOCALS: RefCell<HashMap<usize, (WorkerId, Weak<Coordinator>)>> = RefCell::new(HashMap::default());
}

struct Mailbox {
    worker: WorkerId,
    state: Mutex<MailboxState>,
}
struct MailboxState {
    installed: bool,
    closed: bool,
    outstanding: usize,
    queue: VecDeque<Command>,
    waker: Option<Waker>,
}

/// The map is immutable for the lifetime of this directory. Construct a replacement
/// only after all endpoints and completion receipts have drained.
pub struct WorkerDirectory {
    pub(crate) subscriptions: Arc<super::range_stream::Scheduler>,
    map: Arc<WorkerMap>,
    mailboxes: Vec<Arc<Mailbox>>,
    capacity: usize,
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
struct Completion {
    value: Result<Value>,
    budget: Option<AcquisitionBudget>,
}
struct Reply {
    generation: u64,
    abandoned: AtomicBool,
    state: Mutex<ReplyState>,
}
struct ReplyState {
    completion: Option<Completion>,
    waker: Option<Waker>,
    finished: bool,
}
impl Reply {
    fn complete(&self, generation: u64, completion: Completion) -> Result<()> {
        let mut state = self.state.lock().unwrap();
        if generation != self.generation || state.finished {
            return Err(Error::StaleFlight);
        }
        state.finished = true;
        if !self.abandoned.load(Ordering::Acquire) {
            state.completion = Some(completion);
        }
        let waker = state.waker.take();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
        Ok(())
    }
}
struct Permit(Arc<Mailbox>);
impl Drop for Permit {
    fn drop(&mut self) {
        let mut state = self.0.state.lock().unwrap();
        state.outstanding -= 1;
        let waker = state.waker.clone();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}
struct Command {
    generation: u64,
    work: Work,
    scope: RequestScope,
    budget: Option<AcquisitionBudget>,
    reply: Arc<Reply>,
    permit: Arc<Permit>,
}
struct Receipt {
    reply: Arc<Reply>,
    scope: RequestScope,
    cancellation: uring_runtime::deadline::CancellationRegistration,
    _permit: Arc<Permit>,
}
impl Future for Receipt {
    type Output = Result<Completion>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        self.cancellation.register(cx.waker());
        if let Err(error) = self.scope.check() {
            return Poll::Ready(Err(error));
        }
        if self.reply.abandoned.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Cancelled));
        }
        let mut state = self.reply.state.lock().unwrap();
        if let Some(completion) = state.completion.take() {
            return Poll::Ready(Ok(completion));
        }
        state.waker = Some(cx.waker().clone());
        Poll::Pending
    }
}
impl Drop for Receipt {
    fn drop(&mut self) {
        self.reply.abandoned.store(true, Ordering::Release);
        // Cancellation is a notification, not release of the accepted permit.
        let waker = self._permit.0.state.lock().unwrap().waker.clone();
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}
struct Active {
    cancellation: Result<uring_runtime::deadline::CancellationRegistration>,
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
            return self.local()?.fill.accept_selected(copy, scope).await;
        }
        let receipt = self.submit(owner, Work::Selected(copy), scope, None)?;
        // Cancellation notifies the owner, but selection exclusivity must survive
        // until its accepted crypto and publication work actually completes.
        let completion = std::future::poll_fn(|cx| {
            receipt.cancellation.register(cx.waker());
            let mut state = receipt.reply.state.lock().unwrap();
            if let Some(completion) = state.completion.take() {
                return Poll::Ready(completion);
            }
            state.waker = Some(cx.waker().clone());
            Poll::Pending
        })
        .await;
        scope.check()?;
        match completion.value? {
            Value::Page(result) => Ok(result),
            _ => Err(Error::StaleFlight),
        }
    }
    /// A simulated process loss discards queued messages, including the permit
    /// whose Arc otherwise forms a mailbox -> command -> mailbox ownership cycle.
    /// Do not execute commands or mark kernel/NIC operations complete here.
    #[cfg(test)]
    pub(crate) fn simulation_crash(&self) {
        for mailbox in &self.mailboxes {
            let commands = {
                let mut state = mailbox.state.lock().unwrap();
                state.closed = true;
                std::mem::take(&mut state.queue)
            };
            drop(commands);
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
                        state: Mutex::new(MailboxState {
                            installed: false,
                            closed: false,
                            outstanding: 0,
                            queue: VecDeque::new(),
                            waker: None,
                        }),
                    })
                })
                .collect(),
            capacity,
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
            let mut state = mailbox.state.lock().unwrap();
            if state.installed || state.closed {
                return Err(Error::InvalidConfiguration);
            }
            state.installed = true;
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
        // A receipt has a task-specific waker, not the worker's stable waker.
        // Release its notification slot when the receipt completes or detaches.
        let cancellation = scope.cancellation.subscribe()?;
        let mailbox = self.mailbox(worker)?;
        let generation = self
            .sequence
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |n| n.checked_add(1))
            .map_err(|_| Error::Unavailable)?;
        let mut state = mailbox.state.lock().unwrap();
        if state.closed || !state.installed {
            return Err(Error::Unavailable);
        }
        if state.outstanding >= self.capacity {
            return Err(Error::Overloaded);
        }
        state.outstanding += 1;
        let permit = Arc::new(Permit(mailbox.clone()));
        let reply = Arc::new(Reply {
            generation,
            abandoned: AtomicBool::new(false),
            state: Mutex::new(ReplyState {
                completion: None,
                waker: None,
                finished: false,
            }),
        });
        state.queue.push_back(Command {
            generation,
            work,
            scope: scope.clone(),
            budget,
            reply: reply.clone(),
            permit: permit.clone(),
        });
        let waker = state.waker.take();
        drop(state);
        if let Some(waker) = waker {
            waker.wake();
        }
        Ok(Receipt {
            reply,
            scope: scope.clone(),
            cancellation,
            _permit: permit,
        })
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
        self.mailbox.state.lock().unwrap().closed = true;
    }
    pub fn is_drained(&self) -> bool {
        self.mailbox.state.lock().unwrap().outstanding == 0
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
                let state = self.mailbox.state.lock().unwrap();
                for command in &state.queue {
                    command.reply.abandoned.store(true, Ordering::Release);
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
        self.mailbox.state.lock().unwrap().waker = Some(cx.waker().clone());
        let mut remaining_polls = self.active.len();
        for _ in 0..work_budget {
            let command = self.mailbox.state.lock().unwrap().queue.pop_front();
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
                        let value = if reply.abandoned.load(Ordering::Acquire) {
                            Err(Error::Cancelled)
                        } else {
                            execute(&local, work, &scope, budget.as_mut()).await
                        };
                        // Only actual completion releases command resources. The
                        // receipt retains the slot until its result is consumed.
                        let completed = reply.complete(generation, Completion { value, budget });
                        drop(permit);
                        completed
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
                    || active.reply.abandoned.load(Ordering::Acquire)
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
        if work_budget != 0 && !self.mailbox.state.lock().unwrap().queue.is_empty() {
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
        let state = self.mailbox.state.lock().unwrap();
        self.active
            .iter()
            .map(|active| active.caller.deadline.0)
            .chain(state.queue.iter().map(|command| command.scope.deadline.0))
            .min()
    }
    pub fn uninstall(&mut self) -> Result<()> {
        self.stop_admission();
        if !self.is_drained() || !self.active.is_empty() {
            return Err(Error::Unavailable);
        }
        self.mailbox.state.lock().unwrap().installed = false;
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
        let queued = std::mem::take(&mut self.mailbox.state.lock().unwrap().queue);
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
            .accept_selected(copy, scope)
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
        workers.sort_by_key(|worker| worker.0);
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
        if self.workers.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let mut hash = Sha256::new();
        hash.update(b"racer.local-worker.v1\0");
        hash.update((object.cache.0.len() as u64).to_be_bytes());
        hash.update(object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.to_be_bytes());
        let digest = hash.finalize();
        let index = u64::from_be_bytes(digest[..8].try_into().expect("eight digest bytes"))
            % self.workers.len() as u64;
        Ok(self.workers[index as usize])
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_control_wire::CacheId;
    use crate::model::CacheKey;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use std::time::Duration;
    use std::time::Instant;
    #[test]
    fn stable_assignment_ignores_etag_and_worker_input_order() {
        use racer_control_wire::CacheId;
        use crate::model::CacheKey;
        use crate::model::ObjectVersion;
        use crate::model::PageNumber;
        use crate::model::StrongEtag;
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
            mailbox.state.lock().unwrap().installed = true;
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
        let command = directory
            .mailbox(owner)
            .unwrap()
            .state
            .lock()
            .unwrap()
            .queue
            .pop_front()
            .unwrap();
        assert!(
            matches!(&command.work, Work::Selected(copy) if copy.metadata.version == version())
        );
        scope.cancel().unwrap();
        assert!(selected.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            directory
                .mailbox(owner)
                .unwrap()
                .state
                .lock()
                .unwrap()
                .outstanding,
            1
        );
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
        assert_eq!(
            directory
                .mailbox(owner)
                .unwrap()
                .state
                .lock()
                .unwrap()
                .outstanding,
            0
        );
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
            let command = directory
                .mailbox(owner)
                .unwrap()
                .state
                .lock()
                .unwrap()
                .queue
                .pop_front()
                .unwrap();
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
            assert_eq!(
                directory
                    .mailbox(owner)
                    .unwrap()
                    .state
                    .lock()
                    .unwrap()
                    .outstanding,
                0
            );
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
                let command = mailbox.state.lock().unwrap().queue.pop_front().unwrap();
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
                assert_eq!(mailbox.state.lock().unwrap().outstanding, 0);
            }
        }
    }

    #[test]
    fn blocked_endpoint_is_quiet_and_round_robin_is_budgeted() {
        let directory = Arc::new(directory(4));
        let mut endpoint = WorkerEndpoint {
            mailbox: directory.mailboxes[0].clone(),
            directory,
            local: crate::app::tests::wake_test_coordinator(),
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
                reply: Arc::new(Reply {
                    generation: id,
                    abandoned: AtomicBool::new(false),
                    state: Mutex::new(ReplyState {
                        completion: None,
                        waker: None,
                        finished: false,
                    }),
                }),
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
                    local: crate::app::tests::wake_test_coordinator(),
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
        let command = directory.mailboxes[0]
            .state
            .lock()
            .unwrap()
            .queue
            .pop_front()
            .unwrap();
        drop(receipt);
        assert!(command.reply.abandoned.load(Ordering::Acquire));
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
        assert!(command.reply.state.lock().unwrap().completion.is_none());
        drop(command);
        let next = directory
            .submit(WorkerId(0), Work::Retained(version()), &scope, None)
            .unwrap();
        let command = directory.mailboxes[0]
            .state
            .lock()
            .unwrap()
            .queue
            .pop_front()
            .unwrap();
        drop(next);
        drop(command);
        assert_eq!(directory.mailboxes[0].state.lock().unwrap().outstanding, 0);
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
        let mut command = directory.mailboxes[0]
            .state
            .lock()
            .unwrap()
            .queue
            .pop_front()
            .unwrap();
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
            Err(Error::StaleFlight)
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
            Err(Error::StaleFlight)
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
        assert_eq!(directory.mailboxes[0].state.lock().unwrap().outstanding, 0);
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
        directory
            .mailbox(owner)
            .unwrap()
            .state
            .lock()
            .unwrap()
            .closed = true;
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
        let mailbox = Arc::new(Mailbox {
            worker: WorkerId(0),
            state: Mutex::new(MailboxState {
                installed: true,
                closed: false,
                outstanding: 1,
                queue: VecDeque::new(),
                waker: None,
            }),
        });
        let accepted = Arc::new(Permit(mailbox.clone()));
        let receipt = accepted.clone();
        drop(receipt);
        assert_eq!(mailbox.state.lock().unwrap().outstanding, 1);
        drop(accepted);
        assert_eq!(mailbox.state.lock().unwrap().outstanding, 0);
    }
}
