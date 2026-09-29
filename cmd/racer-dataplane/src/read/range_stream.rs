//! Shared compact subscription demand with independently leased page slices.
//! A stream pins its version and length once. A late error terminates that stream;
//! it cannot replace headers or reopen against a newer version.
use super::{
    dispatch::WorkerDirectory,
    fill::{Fill, PageResult},
    flight::AcquisitionBudget,
};
use crate::telemetry::failures::{Detail, Failure, Observer, Stage};
use crate::{
    error::{Error, Operation, Result},
    memory::delivery::{Delivery, ReaderLease},
    model::{
        context::OriginContext,
        identity::{PageId, PageNumber},
        metadata::ObjectMetadata,
        range::ResolvedRange,
    },
    runtime::deadline::RequestScope,
    topology::membership::MembershipLease,
};
use std::{
    collections::VecDeque,
    future::poll_fn,
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
    time::{Duration, Instant},
};

/// Only client range progress creates a new allowance. An explicit aggregate
/// budget is conserved across pages, just like budgets passed to peer acquisition.
enum RangeBudget {
    ClientPages { deadline: Instant },
    ProgressingPages { timeout: Duration },
    Shared(AcquisitionBudget),
}
impl RangeBudget {
    fn next_page(&mut self, pending: bool) -> Result<Option<AcquisitionBudget>> {
        match self {
            Self::ProgressingPages { timeout } => Ok(Some(AcquisitionBudget::new(
                crate::runtime::environment::now() + *timeout,
                8,
                16,
            ))),
            Self::ClientPages { deadline } => {
                if crate::runtime::environment::now() >= *deadline {
                    return Err(Error::DeadlineExceeded);
                }
                Ok(Some(AcquisitionBudget::new(*deadline, 8, 16)))
            }
            Self::Shared(budget) => {
                let attempts = budget.remaining_attempts().min(8);
                let links = budget.remaining_links().min(16);
                if pending && (attempts == 0 || links < 4) {
                    return Ok(None);
                }
                budget.partition(attempts, links).map(Some)
            }
        }
    }
    fn complete(&mut self, remaining: AcquisitionBudget) -> Result<()> {
        match self {
            Self::ClientPages { .. } | Self::ProgressingPages { .. } => Ok(()),
            Self::Shared(budget) => budget.reunite(remaining),
        }
    }
}

enum WindowPage {
    Waiting(Operation<'static, (Result<PageResult>, AcquisitionBudget)>),
    Ready(Result<PageResult>),
}

pub struct RangeStreams {
    observer: Observer,
    // Retains the same worker-local Fill graph as the coordinator. Actual page
    // acquisition always goes through the stable directory owner.
    _fill: Option<Rc<Fill>>,
    directory: Arc<WorkerDirectory>,
    delivery: Rc<Delivery>,
    window_pages: usize,
}
/// The body stays on the ingress reactor, including its delivery pipe leases.
/// ```compile_fail
/// use racer_dataplane::read::range_stream::RangeStream;
/// fn send<T: Send>() {}
/// send::<RangeStream>();
/// ```
pub struct RangeStream {
    prefetch_error: Option<Error>,
    selected_ready: Option<PageResult>,
    selection: Option<Operation<'static, (Result<PageResult>, AcquisitionBudget)>>,
    retained: std::collections::BTreeMap<PageNumber, crate::memory::pool::VerifiedPage>,
    subscription: Option<super::subscription::DemandLease>,
    observer: Observer,
    metadata: ObjectMetadata,
    range: ResolvedRange,
    context: OriginContext,
    membership: MembershipLease,
    scope: RequestScope,
    directory: Arc<WorkerDirectory>,
    delivery: Rc<Delivery>,
    window_pages: usize,
    budget: RangeBudget,
    next_page: Option<PageNumber>,
    ready: VecDeque<(PageNumber, WindowPage)>,
    terminated: bool,
}
impl RangeStreams {
    pub fn new(
        fill: Rc<Fill>,
        directory: Arc<WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
    ) -> Self {
        Self {
            observer: Observer::default(),
            _fill: Some(fill),
            directory,
            delivery,
            window_pages,
        }
    }
    /// Construct delivery against an installed directory without retaining an
    /// additional Fill handle. Seeded pages undergo the same full validation;
    /// every unseeded page still goes through its stable directory owner.
    pub fn from_directory(
        directory: Arc<WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
    ) -> Self {
        Self {
            observer: Observer::default(),
            _fill: None,
            directory,
            delivery,
            window_pages,
        }
    }
    pub fn directory(&self) -> &Arc<WorkerDirectory> {
        &self.directory
    }
    pub(crate) fn with_observer(mut self, observer: Observer) -> Self {
        self.observer = observer;
        self
    }
    /// Open a client range with a bounded allowance for each distinct page and
    /// the original request deadline. Use open_with_budget for an aggregate cap.
    pub fn open(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
    ) -> Result<RangeStream> {
        let budget = RangeBudget::ClientPages {
            deadline: scope.deadline.0,
        };
        self.open_budget(metadata, range, context, membership, scope, budget, None)
    }
    /// Bootstrap supplies its already acquired page zero. The same original
    /// budget then belongs to the stream, with no retry/fanout reset.
    #[allow(clippy::too_many_arguments)]
    pub fn open_with_budget(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
        budget: AcquisitionBudget,
        seed: Option<PageResult>,
    ) -> Result<RangeStream> {
        self.open_budget(
            metadata,
            range,
            context,
            membership,
            scope,
            RangeBudget::Shared(budget),
            seed,
        )
    }
    #[allow(clippy::too_many_arguments)]
    fn open_budget(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: MembershipLease,
        scope: RequestScope,
        budget: RangeBudget,
        seed: Option<PageResult>,
    ) -> Result<RangeStream> {
        scope.check()?;
        if self.window_pages == 0 {
            return Err(Error::InvalidConfiguration);
        }
        if context.object != metadata.version.object
            || range.end() > metadata.length
            || range.start() >= range.end()
        {
            return Err(Error::InvalidRange);
        }
        let first = range.first_page();
        let mut stream = RangeStream {
            prefetch_error: None,
            selected_ready: None,
            selection: None,
            retained: std::collections::BTreeMap::new(),
            subscription: None,
            observer: self.observer.clone(),
            metadata,
            range,
            context,
            membership,
            scope,
            directory: self.directory.clone(),
            delivery: self.delivery.clone(),
            window_pages: self.window_pages,
            budget,
            next_page: Some(first),
            ready: VecDeque::new(),
            terminated: false,
        };
        if let Some(seed) = seed {
            stream.validate(&seed, first)?;
            stream.ready.push_back((first, WindowPage::Ready(Ok(seed))));
            stream.advance(first);
        }
        Ok(stream)
    }
}
impl RangeStream {
    pub fn configure_subscription(
        &mut self,
        pages: usize,
        bytes: u64,
        ordered: bool,
    ) -> Result<()> {
        if self.subscription.is_some() {
            return Err(Error::InvalidRequest);
        }
        let mut demand = self.directory.subscriptions.register(
            self.metadata.version.clone(),
            self.range,
            pages,
            bytes,
            ordered,
        )?;
        // Seeded streams are used by fixtures; production resolves metadata without bootstrap.
        for (number, _) in &self.ready {
            if demand.select() != Some(*number) {
                return Err(Error::InvalidRequest);
            }
        }
        // Ordered concurrency also respects the configured acquisition window:
        // a large client lease allowance must not create an I/O admission burst.
        // Unordered selection retains its existing credit-driven behavior.
        self.window_pages = if ordered {
            self.window_pages.min(pages)
        } else {
            pages
        };
        self.subscription = Some(demand);
        Ok(())
    }
    pub fn release_page(&mut self, number: PageNumber, length: u32) -> Result<()> {
        self.subscription
            .as_mut()
            .ok_or(Error::InvalidRequest)?
            .release(number, length)?;
        self.retained.remove(&number);
        Ok(())
    }
    /// Drive admitted acquisitions while the current slice encounters client
    /// backpressure. Completed pages leave their acquisition scopes promptly;
    /// new subscription work requires credit and ordered work also requires room
    /// in the configured window, including delivered but unreleased pages.
    pub(crate) fn poll_prefetch(&mut self, cx: &mut Context<'_>) {
        if self.terminated || self.prefetch_error.is_some() {
            return;
        }
        let result = if self.subscription.as_ref().is_some_and(|d| d.ordered()) {
            self.poll_ordered(cx)
        } else if self.subscription.is_some() && self.ready.is_empty() {
            self.poll_selection(cx)
        } else {
            poll_window(&mut self.ready, &mut self.budget, cx).map(|()| Ok(()))
        };
        if let Poll::Ready(Err(error)) = result {
            self.prefetch_error = Some(error);
        }
    }
    fn poll_ordered(&mut self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        let scope = self.operation_scope();
        scope.check()?;
        let _ = poll_window(&mut self.ready, &mut self.budget, cx);
        if self.subscription.as_ref().unwrap().exhausted() {
            self.next_page = None;
        }
        while self.ready.len() + self.retained.len() < self.window_pages {
            if self
                .ready
                .iter()
                .any(|(_, page)| matches!(page, WindowPage::Ready(Err(_))))
            {
                break;
            }
            let child = match self.budget.next_page(!self.ready.is_empty())? {
                Some(child) => child,
                None => break,
            };
            let assignment = self.subscription.as_mut().unwrap().poll_ordered(cx);
            let (number, guard) = match assignment {
                Poll::Ready(Ok(Some(assignment))) => assignment,
                other => {
                    self.budget.complete(child)?;
                    match other {
                        Poll::Ready(Err(error)) => return Poll::Ready(Err(error)),
                        Poll::Ready(Ok(None)) => self.next_page = None,
                        _ => {}
                    }
                    break;
                }
            };
            let mut child_scope = scope.clone();
            child_scope.deadline.0 = child.deadline();
            let future = self.directory.start_ordered_page(
                PageId {
                    version: self.metadata.version.clone(),
                    number,
                },
                guard,
                self.membership.clone(),
                &self.context,
                &child_scope,
                child,
            );
            let failed = future.is_err();
            self.ready.push_back((
                number,
                match future {
                    Ok(future) => WindowPage::Waiting(future),
                    Err(error) => WindowPage::Ready(Err(error)),
                },
            ));
            if failed {
                break;
            }
        }
        poll_window(&mut self.ready, &mut self.budget, cx).map(|()| Ok(()))
    }
    fn poll_selection(&mut self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        use super::subscription::Next;
        let scope = self.operation_scope();
        scope.check()?;
        loop {
            if self.selected_ready.is_some() {
                return Poll::Ready(Ok(()));
            }
            if let Some(future) = self.selection.as_mut() {
                let (result, remaining) = std::task::ready!(future.as_mut().poll(cx))?;
                self.budget.complete(remaining)?;
                self.selection = None;
                result?;
            }
            match std::task::ready!(self.subscription.as_mut().unwrap().poll_next(cx))? {
                Next::End => {
                    self.next_page = None;
                    return Poll::Ready(Ok(()));
                }
                Next::Page(result) => self.selected_ready = Some(result),
                Next::Select(selection) => {
                    let child = self.budget.next_page(false)?.ok_or(Error::Overloaded)?;
                    let mut child_scope = scope.clone();
                    child_scope.deadline.0 = child.deadline();
                    self.selection = Some(self.directory.start_selection(
                        self.metadata.version.clone(),
                        selection,
                        self.membership.clone(),
                        &self.context,
                        &child_scope,
                        child,
                    )?);
                }
            }
        }
    }
    /// Only client HTTP delivery may release the initial operation deadline after
    /// acquiring the first slice. Already admitted pages retain their original deadlines
    /// and retry budgets; only newly admitted, distinct pages get child scopes.
    pub(crate) fn enable_progress(&mut self, timeout: Duration) {
        if matches!(self.budget, RangeBudget::ClientPages { .. }) {
            self.budget = RangeBudget::ProgressingPages { timeout };
        }
    }
    fn operation_scope(&self) -> RequestScope {
        let mut scope = self.scope.clone();
        if let RangeBudget::ProgressingPages { timeout } = self.budget {
            scope.deadline.0 = crate::runtime::environment::now() + timeout;
        }
        scope
    }
    fn validate(&self, result: &PageResult, number: PageNumber) -> Result<()> {
        validate_pin(&self.metadata, &result.metadata)?;
        result.validate_for(&PageId {
            version: self.metadata.version.clone(),
            number,
        })
    }
    fn advance(&mut self, page: PageNumber) {
        self.next_page = (page != self.range.last_page()).then(|| PageNumber(page.0 + 1));
    }
    /// Admit each distinct page once into a bounded sliding window. Client page
    /// progress gets its own fixed child allowance after headers; explicit
    /// aggregate budgets partition credits. Pending futures live in the stream,
    /// so dropping next_slice cannot restart a page or refill its retries.
    pub fn next_slice(&mut self) -> Operation<'_, Option<ReaderLease>> {
        Box::pin(async move {
            if self.terminated {
                return Ok(None);
            }
            if let Some(error) = self.prefetch_error.take() {
                self.terminate();
                return Err(error);
            }
            if self.subscription.as_ref().is_some_and(|d| d.ordered()) {
                let result = self.next_ordered_slice().await;
                if result.is_err() {
                    self.terminate();
                }
                return result;
            }
            if self.subscription.is_some() && self.ready.is_empty() {
                return self.next_subscription_slice().await;
            }
            if self.ready.is_empty() && self.next_page.is_none() {
                self.terminated = true;
                return Ok(None);
            }
            let operation_scope = self.operation_scope();
            if let Err(error) = operation_scope.check() {
                self.observer
                    .record(Failure::new(Stage::RangeScope, error).request(&operation_scope));
                self.terminate();
                return Err(error);
            }
            if self.ready.is_empty() {
                if let Some(demand) = &self.subscription {
                    if demand.exhausted() {
                        self.terminated = true;
                        self.next_page = None;
                        return Ok(None);
                    }
                    if !demand.ready_to_select() {
                        // A credit-starved subscriber must not hold the shared
                        // delivery pipe while waiting for its own release frame.
                        std::future::pending::<()>().await;
                    }
                }
            }
            // Schedule delivery before starting more page work. Waiting requests
            // cannot pin newly acquired pages merely to discover pipe exhaustion.
            let pipe = match self.delivery.admit(&operation_scope).await {
                Ok(pipe) => pipe,
                Err(error) => {
                    self.observer
                        .record(Failure::new(Stage::RangePipe, error).request(&operation_scope));
                    self.terminate();
                    return Err(error);
                }
            };
            while self.ready.len() < self.window_pages {
                if self
                    .subscription
                    .as_ref()
                    .is_some_and(|demand| demand.exhausted())
                {
                    self.next_page = None;
                    break;
                }
                if self.subscription.is_none() && self.next_page.is_none() {
                    break;
                }
                let child = match self.budget.next_page(!self.ready.is_empty()) {
                    Ok(Some(child)) => child,
                    Ok(None) => break,
                    Err(error) => {
                        self.observer.record(
                            Failure::new(Stage::RangeBudget, error).request(&operation_scope),
                        );
                        self.terminate();
                        return Err(error);
                    }
                };
                let number = if let Some(demand) = self.subscription.as_mut() {
                    let Some(number) = demand.select() else {
                        if demand.exhausted() {
                            self.next_page = None;
                        }
                        self.budget.complete(child)?;
                        break;
                    };
                    number
                } else {
                    let Some(number) = self.next_page else {
                        self.budget.complete(child)?;
                        break;
                    };
                    number
                };
                let page = PageId {
                    version: self.metadata.version.clone(),
                    number,
                };
                let mut page_scope = operation_scope.clone();
                page_scope.deadline.0 = child.deadline();
                let result = self.directory.start_page(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &page_scope,
                    child,
                );
                let failed = result.is_err();
                let entry = match result {
                    Ok(future) => WindowPage::Waiting(future),
                    Err(error) => {
                        self.observer.record(
                            Failure::new(Stage::PageDispatch, error)
                                .request(&page_scope)
                                .detail(Detail::Page(number.0)),
                        );
                        WindowPage::Ready(Err(error))
                    }
                };
                if failed {
                    self.next_page = None;
                } else if self.subscription.is_none() {
                    self.advance(number);
                }
                self.ready.push_back((number, entry));
            }
            if self.ready.is_empty() && self.next_page.is_some() {
                // Only an exact client release can make credit available again.
                std::future::pending::<()>().await;
            }
            let unordered = self
                .subscription
                .as_ref()
                .is_some_and(|demand| !demand.ordered());
            poll_fn(|cx| {
                let result = poll_window(&mut self.ready, &mut self.budget, cx);
                if unordered {
                    if let Some(index) = self
                        .ready
                        .iter()
                        .position(|(_, page)| matches!(page, WindowPage::Ready(_)))
                    {
                        let page = self.ready.remove(index).unwrap();
                        self.ready.push_front(page);
                        return Poll::Ready(());
                    }
                }
                result
            })
            .await;
            if let Err(error) = operation_scope.check() {
                self.terminate();
                return Err(error);
            }
            let Some((number, WindowPage::Ready(result))) = self.ready.pop_front() else {
                self.terminated = true;
                return Ok(None);
            };
            if let Err(error) = &result {
                self.observer.record(
                    Failure::new(Stage::PageAcquire, *error)
                        .request(&operation_scope)
                        .detail(Detail::Page(number.0)),
                );
            }
            let lease = result.and_then(|result| {
                let attach = (|| {
                    self.validate(&result, number)?;
                    if let Some(demand) = self.subscription.as_mut() {
                        demand.completed(number);
                        demand.issued(number)?;
                        self.retained.insert(number, result.plaintext.clone());
                    }
                    let slice = self
                        .range
                        .slice_at(result.plaintext.page().number)?
                        .ok_or(Error::CorruptRecord)?;
                    self.delivery.attach_reserved(result.plaintext, slice, pipe)
                })();
                self.observer
                    .result(Stage::PageAttach, &operation_scope, attach)
            });
            match lease {
                Ok(lease) => Ok(Some(lease)),
                Err(error) => {
                    self.terminate();
                    Err(error)
                }
            }
        })
    }
    pub fn cancel(&mut self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.terminate();
            self.scope.cancel()
        })
    }
    fn terminate(&mut self) {
        self.terminated = true;
        self.next_page = None;
        self.ready.clear();
        self.retained.clear();
        self.subscription = None;
        self.selection = None;
        self.selected_ready = None;
    }

    async fn next_ordered_slice(&mut self) -> Result<Option<ReaderLease>> {
        let scope = self.operation_scope();
        let cancellation = scope.cancellation.subscribe()?;
        poll_fn(|cx| {
            cancellation.register(cx.waker());
            std::task::ready!(self.poll_ordered(cx))?;
            if self.ready.is_empty() && self.next_page.is_some() {
                Poll::Pending
            } else {
                Poll::Ready(Ok(()))
            }
        })
        .await?;
        if self.ready.is_empty() {
            self.terminated = true;
            return Ok(None);
        }
        // No delivery pipe is held while waiting for credit or the ordered head.
        let pipe = self.delivery.admit(&scope).await?;
        let Some((number, WindowPage::Ready(result))) = self.ready.pop_front() else {
            return Err(Error::StaleFlight);
        };
        let result = result?;
        self.validate(&result, number)?;
        self.subscription.as_mut().unwrap().issued(number)?;
        self.retained.insert(number, result.plaintext.clone());
        let slice = self.range.slice_at(number)?.ok_or(Error::CorruptRecord)?;
        self.delivery
            .attach_reserved(result.plaintext, slice, pipe)
            .map(Some)
    }
    async fn next_subscription_slice(&mut self) -> Result<Option<ReaderLease>> {
        let scope = self.operation_scope();
        let cancellation = scope.cancellation.subscribe()?;
        let result = async {
            loop {
                scope.check()?;
                if let Some(result) = self.selected_ready.as_ref() {
                    let number = result.plaintext.page().number;
                    self.validate(result, number)?;
                    let pipe = self.delivery.admit(&scope).await?;
                    let result = self.selected_ready.take().ok_or(Error::StaleFlight)?;
                    self.subscription.as_mut().unwrap().issued(number)?;
                    self.retained.insert(number, result.plaintext.clone());
                    let slice = self.range.slice_at(number)?.ok_or(Error::CorruptRecord)?;
                    return self
                        .delivery
                        .attach_reserved(result.plaintext, slice, pipe)
                        .map(Some);
                }
                poll_fn(|cx| {
                    cancellation.register(cx.waker());
                    self.poll_selection(cx)
                })
                .await?;
                if self.selected_ready.is_none() && self.next_page.is_none() {
                    self.terminated = true;
                    return Ok(None);
                }
            }
        }
        .await;
        if result.is_err() {
            self.terminate();
        }
        result
    }
    pub fn buffered_pages(&self) -> usize {
        self.ready.len()
    }
}
fn poll_window(
    window: &mut VecDeque<(PageNumber, WindowPage)>,
    budget: &mut RangeBudget,
    cx: &mut Context<'_>,
) -> Poll<()> {
    for (_, entry) in window.iter_mut() {
        if let WindowPage::Waiting(future) = entry {
            if let Poll::Ready(completion) = future.as_mut().poll(cx) {
                let result = match completion {
                    Ok((result, remaining)) => budget.complete(remaining).and(result),
                    Err(error) => Err(error),
                };
                *entry = WindowPage::Ready(result);
            }
        }
    }
    if window
        .front()
        .is_none_or(|(_, entry)| matches!(entry, WindowPage::Ready(_)))
    {
        Poll::Ready(())
    } else {
        Poll::Pending
    }
}
impl Drop for RangeStream {
    fn drop(&mut self) {
        // ReaderLease and runtime completion owners retain already submitted I/O.
        // This cancels acquisition only; it cannot revoke another reader's lease.
        if !self.terminated {
            let _ = self.scope.cancel();
        }
    }
}
fn validate_pin(expected: &ObjectMetadata, actual: &ObjectMetadata) -> Result<()> {
    if expected.version != actual.version {
        return Err(Error::VersionUnavailable);
    }
    if !expected.immutable().compatible(&actual.immutable()) {
        return Err(Error::CorruptRecord);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{
        identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag},
        metadata::ExpiresAt,
        range::{ByteRange, PAGE_BYTES},
    };
    fn remaining_credits(budget: &RangeBudget) -> (u32, u8) {
        let RangeBudget::Shared(budget) = budget else {
            panic!("expected aggregate budget")
        };
        (budget.remaining_attempts(), budget.remaining_links())
    }
    #[test]
    fn credit_starved_stream_never_holds_pipe_and_cancel_detaches_demand() {
        use crate::{
            memory::pipe::PipePool,
            model::identity::{MembershipVersion, RequestId, WorkerId},
            runtime::{admission::Admission, reactor::Reactor, worker::WorkerMap},
            topology::membership::Membership,
        };
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let pipes = Rc::new(PipePool::new(admission, reactor));
        let directory = Arc::new(
            WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                vec![WorkerId(0)],
                1,
            )
            .unwrap(),
        );
        let streams = RangeStreams::from_directory(
            directory.clone(),
            Rc::new(Delivery::new(pipes.clone(), Duration::from_secs(30))),
            1,
        );
        let metadata = metadata();
        let range = ByteRange::From(0).resolve(metadata.length).unwrap();
        let scope = RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(60))
            .unwrap();
        let mut stream = streams
            .open(
                metadata.clone(),
                range,
                OriginContext {
                    object: metadata.version.object.clone(),
                    metadata: None,
                    authorization: None,
                },
                Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap()),
                scope.clone(),
            )
            .unwrap();
        stream.configure_subscription(1, PAGE_BYTES, false).unwrap();
        // Model a delivered page still owned by a slow caller.
        let demand = stream.subscription.as_mut().unwrap();
        assert_eq!(demand.select(), Some(PageNumber(0)));
        demand.completed(PageNumber(0));
        demand.issued(PageNumber(0)).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert_eq!(pipes.idle_count(), 0, "no pipe was even allocated");
        assert!(
            directory
                .subscriptions
                .register(metadata.version.clone(), range, 1, PAGE_BYTES, false)
                .is_err()
        );
        futures::executor::block_on(stream.cancel()).unwrap();
        assert!(scope.cancellation.is_cancelled());
        assert!(stream.subscription.is_none());
        assert!(stream.retained.is_empty());
        assert!(
            directory
                .subscriptions
                .register(metadata.version, range, 1, PAGE_BYTES, false)
                .is_ok()
        );
    }
    #[test]
    fn client_page_progress_outlives_attempt_and_link_totals_without_refilling_retries() {
        let deadline = Instant::now() + std::time::Duration::from_secs(60);
        let mut budget = RangeBudget::ClientPages { deadline };
        // Model a healthy remote page that spends a four-link route and transfers
        // acquisition credits to its destination. Neither spend can accumulate
        // into a range-length limit, and unused credits cannot grow later pages.
        for _ in 0..100 {
            let mut page = budget.next_page(false).unwrap().unwrap();
            assert_eq!(page.remaining_attempts(), 8);
            assert_eq!(page.remaining_links(), 16);
            assert_eq!(page.begin_attempt(Instant::now(), deadline), Ok(deadline));
            page.charge_links(4).unwrap();
            let mut remote = page.partition(4, 4).unwrap();
            for _ in 0..4 {
                remote.begin_attempt(Instant::now(), deadline).unwrap();
            }
            assert_eq!(
                remote.begin_attempt(Instant::now(), deadline),
                Err(Error::Unavailable)
            );
            budget.complete(page).unwrap();
        }
        let mut page = budget.next_page(false).unwrap().unwrap();
        for _ in 0..8 {
            page.begin_attempt(Instant::now(), deadline).unwrap();
        }
        assert_eq!(
            page.begin_attempt(Instant::now(), deadline),
            Err(Error::Unavailable)
        );
        page.charge_links(16).unwrap();
        assert_eq!(page.charge_links(1), Err(Error::HopBudgetExhausted));
        assert_eq!(
            page.begin_attempt(deadline, deadline),
            Err(Error::DeadlineExceeded)
        );
        let mut expired = RangeBudget::ClientPages {
            deadline: Instant::now(),
        };
        assert!(matches!(
            expired.next_page(false),
            Err(Error::DeadlineExceeded)
        ));
    }
    #[test]
    fn explicit_aggregate_range_budget_never_refills_spent_pages() {
        let deadline = Instant::now() + std::time::Duration::from_secs(60);
        let mut budget = RangeBudget::Shared(AcquisitionBudget::new(deadline, 2, 4));
        let mut page = budget.next_page(false).unwrap().unwrap();
        assert!(budget.next_page(true).unwrap().is_none());
        page.begin_attempt(Instant::now(), deadline).unwrap();
        page.begin_attempt(Instant::now(), deadline).unwrap();
        page.charge_links(4).unwrap();
        budget.complete(page).unwrap();
        let mut next = budget.next_page(false).unwrap().unwrap();
        assert_eq!(next.deadline(), deadline);
        assert_eq!(
            next.begin_attempt(Instant::now(), deadline),
            Err(Error::Unavailable)
        );
        assert_eq!(next.charge_links(1), Err(Error::HopBudgetExhausted));
    }
    #[test]
    fn pending_client_window_never_restarts_failed_pages_and_honors_original_deadline() {
        use crate::{
            memory::pipe::PipePool,
            model::identity::{MembershipVersion, RequestId, WorkerId},
            runtime::{admission::Admission, reactor::Reactor, worker::WorkerMap},
            topology::membership::Membership,
        };
        use std::{cell::Cell, time::Duration};
        for expire in [false, true] {
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let streams = RangeStreams::from_directory(
                Arc::new(
                    WorkerDirectory::new(
                        Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                        vec![WorkerId(0)],
                        2,
                    )
                    .unwrap(),
                ),
                Rc::new(Delivery::new(
                    Rc::new(PipePool::new(admission, reactor)),
                    Duration::from_secs(30),
                )),
                2,
            );
            let mut metadata = metadata();
            metadata.length = 100 * PAGE_BYTES;
            let scope =
                RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = streams
                .open(
                    metadata.clone(),
                    ByteRange::From(0).resolve(metadata.length).unwrap(),
                    OriginContext {
                        object: metadata.version.object,
                        metadata: None,
                        authorization: None,
                    },
                    Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap()),
                    scope.clone(),
                )
                .unwrap();
            let attempts = Rc::new(Cell::new(0));
            let gate = Rc::new(Cell::new(false));
            // Script both admitted acquisitions, avoiding full-page allocation.
            // A full pending window must prevent admission of page two onward.
            for number in 0..2 {
                let mut budget = stream.budget.next_page(number != 0).unwrap().unwrap();
                let attempts = attempts.clone();
                let gate = gate.clone();
                stream.ready.push_back((
                    PageNumber(number),
                    WindowPage::Waiting(Box::pin(async move {
                        for _ in 0..8 {
                            budget.begin_attempt(Instant::now(), budget.deadline())?;
                            attempts.set(attempts.get() + 1);
                        }
                        poll_fn(|_| {
                            if gate.get() {
                                Poll::Ready(())
                            } else {
                                Poll::Pending
                            }
                        })
                        .await;
                        let result = budget
                            .begin_attempt(Instant::now(), budget.deadline())
                            .map(|_| unreachable!("same page received fresh retry credits"));
                        Ok((result, budget))
                    })),
                ));
                stream.advance(PageNumber(number));
            }
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            if !expire {
                stream.enable_progress(Duration::from_secs(60));
                // Releasing the client lifetime must not replace these already
                // admitted page futures or their spent credits.
                stream.scope.deadline.0 = Instant::now();
            }
            for _ in 0..10 {
                // Each temporary next_slice future is dropped while pending.
                assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
                assert_eq!(attempts.get(), 16);
                assert_eq!(stream.buffered_pages(), 2);
                assert_eq!(stream.next_page, Some(PageNumber(2)));
            }
            let expected = if expire {
                stream.scope.deadline.0 = Instant::now();
                Error::DeadlineExceeded
            } else {
                gate.set(true);
                Error::Unavailable
            };
            assert!(matches!(
                stream.next_slice().as_mut().poll(&mut cx),
                Poll::Ready(Err(error)) if error == expected
            ));
            assert_eq!(attempts.get(), 16);
            assert_eq!(stream.buffered_pages(), 0);
            assert!(matches!(
                stream.next_slice().as_mut().poll(&mut cx),
                Poll::Ready(Ok(None))
            ));
        }
    }
    fn metadata() -> ObjectMetadata {
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: PAGE_BYTES + 7,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        }
    }
    #[test]
    fn out_of_order_completion_waits_for_front_and_returns_only_unused_credits() {
        use std::{
            cell::Cell,
            time::{Duration, Instant},
        };
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut budget = AcquisitionBudget::new(deadline, 6, 8);
        let mut first_budget = budget.partition(3, 4).unwrap();
        let mut last_budget = budget.partition(3, 4).unwrap();
        first_budget
            .begin_attempt(Instant::now(), deadline)
            .unwrap();
        last_budget.begin_attempt(Instant::now(), deadline).unwrap();
        last_budget.charge_links(3).unwrap();
        let gate = Rc::new(Cell::new(false));
        let opened = gate.clone();
        let first = Box::pin(async move {
            poll_fn(|_| {
                if opened.get() {
                    Poll::Ready(())
                } else {
                    Poll::Pending
                }
            })
            .await;
            Ok((Err(Error::VersionUnavailable), first_budget))
        });
        let last = Box::pin(async move { Ok((Err(Error::Unavailable), last_budget)) });
        let mut budget = RangeBudget::Shared(budget);
        let mut window = VecDeque::from([
            (PageNumber(0), WindowPage::Waiting(first)),
            (PageNumber(1), WindowPage::Waiting(last)),
        ]);
        let waker = futures::task::noop_waker();
        let mut cx = Context::from_waker(&waker);
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_pending());
        assert!(matches!(
            window[1].1,
            WindowPage::Ready(Err(Error::Unavailable))
        ));
        assert_eq!(remaining_credits(&budget), (2, 1));
        // Re-polling a completed page must not return its credits twice.
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_pending());
        assert_eq!(remaining_credits(&budget), (2, 1));
        gate.set(true);
        assert!(poll_window(&mut window, &mut budget, &mut cx).is_ready());
        assert!(matches!(
            window.pop_front(),
            Some((
                PageNumber(0),
                WindowPage::Ready(Err(Error::VersionUnavailable))
            ))
        ));
        let RangeBudget::Shared(budget) = budget else {
            unreachable!()
        };
        assert_eq!(
            (
                budget.remaining_attempts(),
                budget.remaining_links(),
                budget.deadline()
            ),
            (4, 5, deadline)
        );
    }
    #[test]
    fn abandoning_window_drops_waiters_without_refunding_outstanding_spends() {
        use std::time::{Duration, Instant};
        struct Dropped(Rc<std::cell::Cell<bool>>);
        impl Drop for Dropped {
            fn drop(&mut self) {
                self.0.set(true);
            }
        }
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut budget = AcquisitionBudget::new(deadline, 3, 4);
        let child = budget.partition(3, 4).unwrap();
        let mut budget = RangeBudget::Shared(budget);
        let dropped = Rc::new(std::cell::Cell::new(false));
        let guard = Dropped(dropped.clone());
        let future = Box::pin(async move {
            let _guard = guard;
            std::future::pending::<()>().await;
            Ok((Err(Error::Cancelled), child))
        });
        let mut window = VecDeque::from([(PageNumber(0), WindowPage::Waiting(future))]);
        let waker = futures::task::noop_waker();
        assert!(
            poll_window(&mut window, &mut budget, &mut Context::from_waker(&waker)).is_pending()
        );
        window.clear();
        assert!(dropped.get());
        assert_eq!(remaining_credits(&budget), (0, 0));
    }
    #[test]
    fn stream_pin_rejects_version_or_length_changes_but_not_expiration() {
        let expected = metadata();
        let mut actual = expected.clone();
        actual.expires_at = ExpiresAt(std::time::SystemTime::now());
        assert_eq!(validate_pin(&expected, &actual), Ok(()));
        actual.length += 1;
        assert_eq!(validate_pin(&expected, &actual), Err(Error::CorruptRecord));
        actual.version.etag = StrongEtag::test_value("v2");
        assert_eq!(
            validate_pin(&expected, &actual),
            Err(Error::VersionUnavailable)
        );
    }
    #[test]
    fn ordered_slices_cross_page_boundary_without_object_sized_plan() {
        let range = ByteRange::Closed {
            first: PAGE_BYTES - 2,
            last: PAGE_BYTES + 6,
        }
        .resolve(PAGE_BYTES + 7)
        .unwrap();
        assert_eq!(range.first_page(), PageNumber(0));
        assert_eq!(range.last_page(), PageNumber(1));
        let first = range.slice_at(PageNumber(0)).unwrap().unwrap();
        let last = range.slice_at(PageNumber(1)).unwrap().unwrap();
        assert_eq!((first.offset, first.length), ((PAGE_BYTES - 2) as u32, 2));
        assert_eq!((last.offset, last.length), (0, 7));
        assert!(range.slice_at(PageNumber(2)).unwrap().is_none());
    }

    #[test]
    fn responses_stream_more_than_three_pages_only_with_client_sized_http_framing() {
        use crate::{
            client::response::Responses,
            http::{codec::Codec, io::HttpIo, pool::ConnectionLease},
            memory::{
                pipe::PipePool,
                pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
            },
            model::{
                envelope::{KeyId, Nonce, PageEnvelope},
                identity::{RequestId, WorkerId},
                limits::ResourceClass,
            },
            read::serve::ReadResponse,
            runtime::{admission::Admission, reactor::Reactor, worker::WorkerMap},
            topology::membership::Membership,
        };
        use std::{
            io::{Read, Write},
            os::unix::net::UnixStream,
            time::Duration,
        };
        let total = 4 * PAGE_BYTES + 17;
        let range = ByteRange::From(PAGE_BYTES).resolve(total).unwrap();
        for (capped, progressing, subscription) in [
            (true, false, false),
            (false, false, false),
            (false, true, false),
            (false, false, true),
        ] {
            let clock = crate::runtime::environment::SimulationClock::new(55);
            let environment = clock.environment(0);
            let _clock = environment.enter();
            let admission = Rc::new(Admission::new(
                crate::test_support::cluster::config(false).limits,
            ));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(
                    32768,
                    if capped {
                        PAGE_BYTES + 16
                    } else {
                        i64::MAX as u64
                    },
                ),
                admission.clone(),
            ));
            let delivery = Rc::new(Delivery::new(
                Rc::new(PipePool::new(admission.clone(), reactor.clone())),
                Duration::from_secs(30),
            ));
            let directory = Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    8,
                )
                .unwrap(),
            );
            let mut metadata = metadata();
            metadata.length = total;
            let scope = RequestScope::new(
                RequestId([7; 16]),
                crate::runtime::environment::now()
                    + if progressing {
                        Duration::from_millis(30)
                    } else {
                        Duration::from_secs(60)
                    },
            )
            .unwrap();
            let membership = Arc::new(
                Membership::validate(crate::model::identity::MembershipVersion(1), vec![]).unwrap(),
            );
            let mut ready = VecDeque::new();
            for number in 1..=4 {
                let length = if number == 4 { 17 } else { PAGE_BYTES as usize };
                let page = PageId {
                    version: metadata.version.clone(),
                    number: PageNumber(number),
                };
                let plaintext = VerifiedPage {
                    inner: Arc::new(VerifiedBytes {
                        page: page.clone(),
                        bytes: vec![number as u8; length],
                        reservation: admission
                            .reserve(
                                Some(&metadata.version.object.cache),
                                ResourceClass::Plaintext,
                                length,
                            )
                            .unwrap(),
                    }),
                };
                let ciphertext = CiphertextPage {
                    inner: Arc::new(CiphertextBytes {
                        checksum: std::sync::OnceLock::new(),
                        envelope: PageEnvelope {
                            page,
                            key_id: KeyId([1; 16]),
                            nonce: Nonce([2; 24]),
                            plaintext_length: length as u32,
                            ciphertext_length: length as u32 + 16,
                        },
                        bytes: vec![0; length + 16],
                        reservation: admission
                            .reserve(
                                Some(&metadata.version.object.cache),
                                ResourceClass::Ciphertext,
                                length + 16,
                            )
                            .unwrap(),
                    }),
                };
                ready.push_back((
                    PageNumber(number),
                    WindowPage::Ready(Ok(PageResult {
                        metadata: metadata.clone(),
                        plaintext,
                        ciphertext,
                    })),
                ));
            }
            let mut stream = RangeStream {
                prefetch_error: None,
                selected_ready: None,
                selection: None,
                retained: std::collections::BTreeMap::new(),
                subscription: None,
                observer: Observer::default(),
                metadata: metadata.clone(),
                range,
                context: OriginContext {
                    object: metadata.version.object.clone(),
                    metadata: None,
                    authorization: None,
                },
                membership,
                scope: scope.clone(),
                directory,
                delivery: delivery.clone(),
                window_pages: 4,
                budget: if progressing {
                    RangeBudget::ClientPages {
                        deadline: scope.deadline.0,
                    }
                } else {
                    RangeBudget::Shared(AcquisitionBudget::new(scope.deadline.0, 0, 0))
                },
                next_page: None,
                ready,
                terminated: false,
            };
            if subscription {
                stream
                    .configure_subscription(4, 4 * PAGE_BYTES, false)
                    .unwrap();
            }
            let response = ReadResponse {
                metadata,
                range: Some(range),
                body: Some(stream),
            };
            let responses = Responses::new(io.clone(), delivery);
            let (server, mut client) = UnixStream::pair().unwrap();
            let reader = std::thread::spawn(move || {
                client
                    .set_read_timeout(Some(Duration::from_secs(60)))
                    .unwrap();
                client
                    .write_all(b"GET / HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n\r\n")
                    .unwrap();
                let mut head = Vec::new();
                let mut byte = [0; 1];
                while !head.ends_with(b"\r\n\r\n") {
                    if client.read(&mut byte).unwrap() == 0 {
                        assert!(capped);
                        assert!(head.is_empty());
                        return;
                    }
                    head.push(byte[0]);
                }
                assert!(!capped);
                let head = String::from_utf8(head).unwrap();
                assert!(head.starts_with(if subscription {
                    "HTTP/1.1 200"
                } else {
                    "HTTP/1.1 206"
                }));
                assert!(head.contains(&format!(
                    "Content-Length: {}\r\n",
                    3 * PAGE_BYTES + 17 + if subscription { 5 * 21 } else { 0 }
                )));
                let mut scratch = [0; 65536];
                for number in 1..=4 {
                    let mut left = if number == 4 { 17 } else { PAGE_BYTES as usize };
                    if subscription {
                        let mut frame = [0; 21];
                        client.read_exact(&mut frame).unwrap();
                        assert_eq!(frame[0], 1);
                        assert_eq!(
                            u64::from_be_bytes(frame[1..9].try_into().unwrap()),
                            number as u64
                        );
                        assert_eq!(
                            u64::from_be_bytes(frame[9..17].try_into().unwrap()),
                            number as u64 * PAGE_BYTES
                        );
                        assert_eq!(
                            u32::from_be_bytes(frame[17..].try_into().unwrap()),
                            left as u32
                        );
                    }
                    while left != 0 {
                        let count = left.min(scratch.len());
                        client.read_exact(&mut scratch[..count]).unwrap();
                        assert!(scratch[..count].iter().all(|value| *value == number));
                        left -= count;
                    }
                }
                if subscription {
                    let mut frame = [0; 21];
                    client.read_exact(&mut frame).unwrap();
                    assert_eq!(frame[0], 2);
                    assert_eq!(u64::from_be_bytes(frame[1..9].try_into().unwrap()), 4);
                    assert_eq!(
                        u64::from_be_bytes(frame[9..17].try_into().unwrap()),
                        3 * PAGE_BYTES + 17
                    );
                    assert_eq!(&frame[17..], &[0; 4]);
                    // No final release is required before the terminal frame.
                    assert_eq!(client.read(&mut frame).unwrap(), 0);
                }
            });
            let work = async {
                let connection = ConnectionLease::from_accepted(server.into(), &admission)?;
                let head = io.receive_head(connection, &scope).await?;
                if subscription {
                    let metrics = crate::telemetry::metrics::Metrics::default();
                    let mut observation = metrics.request()?;
                    responses
                        .send_subscription(
                            head.connection,
                            response,
                            &scope,
                            &mut observation,
                            Duration::from_secs(30),
                        )
                        .await
                        .map(drop)
                } else if progressing {
                    let metrics = crate::telemetry::metrics::Metrics::default();
                    let mut observation = metrics.request()?;
                    responses
                        .send_observed(
                            head.connection,
                            response,
                            &scope,
                            &mut observation,
                            Duration::from_millis(30),
                        )
                        .await
                        .map(drop)
                } else {
                    responses
                        .send(head.connection, response, &scope)
                        .await
                        .map(drop)
                }
            };
            let mut work = std::pin::pin!(work);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let result = loop {
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    break result;
                }
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
                if progressing {
                    clock.advance(Duration::from_millis(1));
                }
            };
            if capped {
                assert_eq!(result, Err(Error::InvalidRequest));
            } else {
                result.unwrap();
            }
            reader.join().unwrap();
            admission.reclaim_buffers();
            if progressing {
                assert!(
                    crate::runtime::environment::now() > scope.deadline.0,
                    "stream must cross the scaled old absolute deadline"
                );
            }
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn progressing_page_children_keep_deadlines_and_retry_limits_across_long_streams() {
        let clock = crate::runtime::environment::SimulationClock::new(81);
        let _environment = clock.environment(0).enter();
        let timeout = Duration::from_millis(30);
        let old_deadline = crate::runtime::environment::now() + timeout;
        let mut budget = RangeBudget::ProgressingPages { timeout };
        for _ in 0..12 {
            let mut page = budget.next_page(false).unwrap().unwrap();
            let deadline = page.deadline();
            assert_eq!(deadline, crate::runtime::environment::now() + timeout);
            for _ in 0..8 {
                page.begin_attempt(crate::runtime::environment::now(), deadline)
                    .unwrap();
            }
            assert_eq!(
                page.begin_attempt(crate::runtime::environment::now(), deadline),
                Err(Error::Unavailable)
            );
            clock.advance(Duration::from_millis(20));
            assert_eq!(page.deadline(), deadline);
            budget.complete(page).unwrap();
        }
        assert!(crate::runtime::environment::now() > old_deadline);
        let mut stalled = budget.next_page(false).unwrap().unwrap();
        clock.advance(timeout);
        assert_eq!(
            stalled.begin_attempt(crate::runtime::environment::now(), stalled.deadline()),
            Err(Error::DeadlineExceeded)
        );
    }
}
