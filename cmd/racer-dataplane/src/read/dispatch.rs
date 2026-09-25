//! Bounded node-local handoffs. Only owned commands and immutable results cross
//! threads; the coordinator, futures, delivery leases, and streams stay local.
use super::{
    fill::PageResult,
    flight::AcquisitionBudget,
    metadata::BootstrapResult,
    serve::{Coordinator, ReadResponse, ReadService},
};
use crate::{
    client::request::ClientRequest,
    error::{Error, Operation, Result},
    model::{
        context::{OriginContext, PeerOriginContext},
        identity::{AttemptId, ObjectId, ObjectVersion, PageId, WorkerId},
        metadata::{MetadataSelector, ObjectMetadata, VersionMetadata},
    },
    peer::{
        server::LocalPageService,
        wire::{PeerResponse, VerifiedRequest},
    },
    runtime::{
        deadline::{Cancellation, RequestScope},
        worker::WorkerMap,
    },
    topology::membership::MembershipLease,
};
use std::{
    cell::RefCell,
    collections::{HashMap, VecDeque},
    future::Future,
    pin::Pin,
    rc::{Rc, Weak},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    task::{Context, Poll, Waker},
};

thread_local! {
    // Installed only on the owning I/O thread. No Rc enters the shared directory.
    static LOCALS: RefCell<HashMap<usize, (WorkerId, Weak<Coordinator>)>> = RefCell::new(HashMap::new());
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
    map: Arc<WorkerMap>,
    mailboxes: Vec<Arc<Mailbox>>,
    capacity: usize,
    sequence: AtomicU64,
}

enum Work {
    Resolve(MetadataSelector, MembershipLease, PeerOriginContext),
    Bootstrap(MetadataSelector, MembershipLease, PeerOriginContext),
    Acquire(PageId, MembershipLease, PeerOriginContext),
    Publish(VersionMetadata),
    Retained(ObjectVersion),
    Peer(VerifiedRequest),
}
enum Value {
    Metadata(ObjectMetadata),
    Bootstrap(BootstrapResult),
    Page(PageResult),
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
    _permit: Arc<Permit>,
}
impl Future for Receipt {
    type Output = Result<Completion>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        if let Err(error) = self.scope.cancellation.register(cx.waker()) {
            return Poll::Ready(Err(error));
        }
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
            .seal_context(context, AttemptId(attempt), scope)
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

    pub fn resolve_metadata<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
    ) -> Operation<'a, ObjectMetadata> {
        Box::pin(async move {
            let mut budget = super::serve::default_budget(scope);
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
            let owner = self.metadata_owner(&context.object)?;
            if self.is_local(owner) {
                return self
                    .local()?
                    .resolve_metadata(selector, membership, context, scope, budget)
                    .await;
            }
            let work = Work::Resolve(selector, membership, self.seal(context, scope)?);
            match self.budgeted(owner, work, scope, budget).await? {
                Value::Metadata(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        })
    }
    pub fn resolve_metadata_with_budget<'a>(
        &'a self,
        selector: MetadataSelector,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, ObjectMetadata> {
        self.resolve_with_budget(selector, membership, context, scope, budget)
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
            let owner = self.metadata_owner(&context.object)?;
            if self.is_local(owner) {
                return self
                    .local()?
                    .bootstrap(selector, membership, context, scope, budget)
                    .await;
            }
            let work = Work::Bootstrap(selector, membership, self.seal(context, scope)?);
            match self.budgeted(owner, work, scope, budget).await? {
                Value::Bootstrap(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        })
    }
    pub fn acquire<'a>(
        &'a self,
        page: PageId,
        membership: MembershipLease,
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
    /// Independently admitted envelope and partitioned credits for a sliding
    /// window. The returned future owns no borrow of the raw request context.
    pub(crate) fn start_page(
        &self,
        page: PageId,
        membership: MembershipLease,
        context: &OriginContext,
        scope: &RequestScope,
        budget: AcquisitionBudget,
    ) -> Result<Operation<'static, (Result<PageResult>, AcquisitionBudget)>> {
        if page.version.object != context.object {
            return Err(Error::InvalidRequest);
        }
        let owner = self.page_owner(&page)?;
        let work = Work::Acquire(page.clone(), membership, self.seal(context, scope)?);
        let receipt = self.submit(owner, work, scope, Some(budget))?;
        Ok(Box::pin(async move {
            let completion = receipt.await?;
            let budget = completion.budget.ok_or(Error::StaleFlight)?;
            let value = completion.value.and_then(|value| match value {
                Value::Page(value) => {
                    value.validate_for(&page)?;
                    Ok(value)
                }
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
                return self.local()?.publish_metadata(metadata);
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
                    Value::Retained(self.local()?.retained_metadata(version, scope).await?)
                } else {
                    self.submit(mailbox.worker, Work::Retained(version.clone()), scope, None)?
                        .await?
                        .value?
                };
                match value {
                    Value::Retained(Some(value)) => {
                        if value.version != *version
                            || found.as_ref().is_some_and(|old| old != &value)
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
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        Box::pin(async move {
            let owner = match &request.request().operation {
                crate::peer::wire::Operation::Page { page, .. } => self.page_owner(page)?,
                crate::peer::wire::Operation::Metadata { object, .. } => {
                    self.metadata_owner(object)?
                }
            };
            if self.is_local(owner) {
                return self.local()?.serve_peer(request, scope).await;
            }
            match self
                .submit(owner, Work::Peer(request), scope, None)?
                .await?
                .value?
            {
                Value::Peer(value) => Ok(value),
                _ => Err(Error::StaleFlight),
            }
        })
    }
}

impl WorkerEndpoint {
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
        for _ in 0..work_budget {
            let command = self.mailbox.state.lock().unwrap().queue.pop_front();
            if let Some(command) = command {
                let local = self.local.clone();
                let caller = command.scope.clone();
                let mut scope = caller.clone();
                scope.cancellation = Cancellation::new()?;
                let active_scope = scope.clone();
                let reply = command.reply.clone();
                self.active.push_back(Active {
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
            if let Some(mut active) = self.active.pop_front() {
                let registration = active.caller.cancellation.register(cx.waker());
                if registration.is_err()
                    || active.reply.abandoned.load(Ordering::Acquire)
                    || active.caller.check().is_err()
                {
                    let _ = active.scope.cancel();
                }
                match active.future.as_mut().poll(cx) {
                    Poll::Pending => self.active.push_back(active),
                    Poll::Ready(result) => result?,
                }
            }
        }
        if !self.mailbox.state.lock().unwrap().queue.is_empty() || self.active.len() > work_budget {
            cx.waker().wake_by_ref();
        }
        Ok(())
    }
    pub fn poll_budgeted(&mut self, work_budget: usize) -> Result<()> {
        let waker = futures::task::noop_waker();
        self.poll(&mut Context::from_waker(&waker), work_budget)
    }
    /// The I/O loop also schedules this deadline when no completion wakes it.
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
        Work::Resolve(selector, membership, envelope) => {
            let context = local.open_context(envelope)?;
            local
                .resolve_metadata(
                    selector,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await
                .map(Value::Metadata)
        }
        Work::Bootstrap(selector, membership, envelope) => {
            let context = local.open_context(envelope)?;
            local
                .bootstrap(
                    selector,
                    membership,
                    &context,
                    scope,
                    budget.ok_or(Error::StaleFlight)?,
                )
                .await
                .map(Value::Bootstrap)
        }
        Work::Acquire(page, membership, envelope) => {
            let context = local.open_context(envelope)?;
            local
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
            local.publish_metadata(metadata)?;
            Ok(Value::Published)
        }
        Work::Retained(version) => local
            .retained_metadata(&version, scope)
            .await
            .map(Value::Retained),
        Work::Peer(request) => local.serve_peer(request, scope).await.map(Value::Peer),
    }
}

pub struct Dispatcher {
    worker: WorkerId,
    directory: Arc<WorkerDirectory>,
    local: Rc<Coordinator>,
}
impl Dispatcher {
    pub fn new(worker: WorkerId, directory: Arc<WorkerDirectory>, local: Rc<Coordinator>) -> Self {
        Self {
            worker,
            directory,
            local,
        }
    }
    pub fn worker(&self) -> WorkerId {
        self.worker
    }
}
impl ReadService for Dispatcher {
    fn read<'a>(
        &'a self,
        request: ClientRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, ReadResponse> {
        self.local.read(request, scope)
    }
}
impl LocalPageService for Dispatcher {
    fn serve_peer<'a>(
        &'a self,
        request: VerifiedRequest,
        scope: &'a RequestScope,
    ) -> Operation<'a, PeerResponse> {
        self.directory.peer(request, scope)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::{CacheId, CacheKey, RequestId, StrongEtag};
    use std::time::{Duration, Instant};

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
            number: crate::model::identity::PageNumber(0),
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
