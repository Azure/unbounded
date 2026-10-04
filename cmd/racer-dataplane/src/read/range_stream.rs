//! Shared compact subscription demand with independently leased page slices.
//! A stream pins its version and length once. A late error terminates that stream;
//! it cannot replace headers or reopen against a newer version.

use super::dispatch::WorkerDirectory;
use super::flight::AcquisitionBudget;
use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::Delivery;
use crate::http::ReaderLease;
use crate::memory::PageResult;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PAGE_BYTES;
use crate::model::PageId;
use crate::model::PageNumber;
use crate::model::ResolvedRange;
#[cfg(test)]
use crate::read::dispatch::WorkerMap;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use flow_control::Window;
use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::collections::VecDeque;
use std::future::poll_fn;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;

// Compact, bounded node-wide subscription demand and page-credit accounting.
struct Demand {
    results: VecDeque<PageResult>,
    waker: Option<std::task::Waker>,
    version: ObjectVersion,
    range: ResolvedRange,
    next: u64,
    end: u64,
    selected: BTreeSet<u64>,
    credits: Window<PageNumber>,
    ordered: bool,
    turn: bool,
    gate_ticket: Option<u64>,
}
struct State {
    contracts: BTreeMap<
        (ObjectVersion, u64, racer_control_wire::NodeId),
        (crate::peer::subscriptions::Subscription, Instant),
    >,
    selecting: BTreeSet<ObjectVersion>,
    fixed: BTreeMap<ObjectVersion, usize>,
    sequence: u64,
    demands: BTreeMap<u64, Demand>,
}
pub(crate) struct Scheduler {
    capacity: usize,
    state: Mutex<State>,
}
impl Scheduler {
    pub(crate) fn contract(
        &self,
        version: ObjectVersion,
        membership: racer_control_wire::MembershipVersion,
        provider: racer_control_wire::NodeId,
        demand: crate::peer::subscriptions::Demand,
        deadline: Instant,
    ) -> Result<(crate::peer::subscriptions::Subscription, Instant)> {
        let mut state = self.state.lock().unwrap();
        let now = uring_runtime::environment::now();
        state.contracts.retain(|_, (_, deadline)| *deadline > now);
        let key = (version.clone(), membership.0, provider);
        if !state.contracts.contains_key(&key) {
            if state.contracts.len() >= self.capacity.saturating_mul(3) {
                return Err(Error::Overloaded);
            }
            let mut id = [0; 16];
            uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Unavailable)?;
            state.contracts.insert(
                key.clone(),
                (
                    crate::peer::subscriptions::Subscription {
                        id,
                        version,
                        demand: Default::default(),
                        sequence: 0,
                        page_budget: u32::MAX,
                        byte_budget: u64::from(u32::MAX) * (PAGE_BYTES + 16),
                    },
                    deadline,
                ),
            );
        }
        let (contract, expires) = state.contracts.get_mut(&key).unwrap();
        *expires = (*expires).min(deadline);
        contract.sequence = contract.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        contract.demand = demand;
        let outgoing = contract.clone();
        contract.page_budget = contract
            .page_budget
            .checked_sub(1)
            .ok_or(Error::Unavailable)?;
        contract.byte_budget = contract
            .byte_budget
            .checked_sub(PAGE_BYTES + 16)
            .ok_or(Error::Unavailable)?;
        Ok((outgoing, *expires))
    }
    pub(crate) fn new(capacity: usize) -> Arc<Self> {
        Arc::new(Self {
            capacity,
            state: Mutex::new(State {
                contracts: BTreeMap::new(),
                selecting: BTreeSet::new(),
                fixed: BTreeMap::new(),
                sequence: 0,
                demands: BTreeMap::new(),
            }),
        })
    }
    pub(crate) fn register(
        self: &Arc<Self>,
        version: ObjectVersion,
        range: ResolvedRange,
        pages: usize,
        bytes: u64,
        ordered: bool,
    ) -> Result<DemandLease> {
        if !(1..=64).contains(&pages) || !(PAGE_BYTES..=64 * PAGE_BYTES).contains(&bytes) {
            return Err(Error::InvalidRequest);
        }
        let credits = Window::new(pages, bytes, PAGE_BYTES).map_err(window_error)?;
        let mut state = self.state.lock().unwrap();
        if state.demands.len() >= self.capacity {
            return Err(Error::Overloaded);
        }
        state.sequence = state.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        let id = state.sequence;
        state.demands.insert(
            id,
            Demand {
                results: VecDeque::new(),
                waker: None,
                version,
                range,
                next: range.first_page().0,
                end: range.last_page().0 + 1,
                selected: BTreeSet::new(),
                credits,
                ordered,
                turn: false,
                gate_ticket: None,
            },
        );
        Ok(DemandLease {
            scheduler: self.clone(),
            id,
        })
    }
}
pub(crate) struct DemandLease {
    scheduler: Arc<Scheduler>,
    id: u64,
}
pub(crate) enum Next {
    Page(PageResult),
    Select(Selection),
    End,
}
/// Node-wide exclusive ownership of the next receiving transfer for this version.
/// Dropping a stream detaches this selection and wakes other subscribers; all
/// accepted I/O still retains its ordinary runtime completion fences.
pub(crate) struct Selection {
    scheduler: Arc<Scheduler>,
    version: ObjectVersion,
    pub(crate) demand: crate::peer::subscriptions::Demand,
}
impl Selection {
    pub(crate) fn complete(self, page: PageResult) -> Result<()> {
        let number = page.plaintext.page().number;
        if page.metadata.version != self.version || !self.demand.contains(number.0) {
            return Err(Error::CorruptRecord);
        }
        page.validate_for(&PageId {
            version: self.version.clone(),
            number,
        })?;
        let mut state = self.scheduler.state.lock().unwrap();
        for demand in state.demands.values_mut() {
            if demand.ordered || demand.version != self.version || !demand.eligible(number.0) {
                continue;
            }
            let length = demand
                .range
                .slice_at(number)?
                .ok_or(Error::InvalidRange)?
                .length;
            demand
                .credits
                .reserve(number, u64::from(length))
                .map_err(window_error)?;
            demand.turn = !demand.turn;
            demand.selected.insert(number.0);
            while demand.selected.remove(&demand.next) {
                demand.next += 1;
            }
            demand.results.push_back(page.clone());
            demand.gate_ticket = None;
        }
        drop(state);
        Ok(())
    }
}
impl Drop for Selection {
    fn drop(&mut self) {
        let wake = {
            let mut state = self.scheduler.state.lock().unwrap();
            state.selecting.remove(&self.version);
            state
                .demands
                .values_mut()
                .filter(|d| d.version == self.version)
                .filter_map(|d| d.waker.take())
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }
}
impl DemandLease {
    /// Reserve this reader's exact slice before dispatch. Fixed acquisitions may
    /// overlap each other (the stable owner singleflights each page), but not an
    /// unknown provider selection, which bypasses that singleflight.
    pub(crate) fn poll_ordered(
        &mut self,
        cx: &mut Context<'_>,
    ) -> Poll<Result<Option<(PageNumber, FixedAcquisition)>>> {
        let mut state = self.scheduler.state.lock().unwrap();
        let demand = state.demands.get_mut(&self.id).ok_or(Error::Cancelled)?;
        demand.waker = Some(cx.waker().clone());
        if demand.next == demand.end {
            return Poll::Ready(Ok(None));
        }
        let version = demand.version.clone();
        if !demand.eligible(demand.next) {
            return Poll::Pending;
        }
        let ticket = state.gate_ticket(self.id)?;
        if state.selecting.contains(&version) || state.earlier_turn(&version, ticket, true) {
            return Poll::Pending;
        }
        let demand = state.demands.get_mut(&self.id).unwrap();
        let number = PageNumber(demand.next);
        let length = demand
            .range
            .slice_at(number)?
            .ok_or(Error::InvalidRange)?
            .length;
        demand
            .credits
            .reserve(number, u64::from(length))
            .map_err(window_error)?;
        demand.gate_ticket = None;
        demand.next += 1;
        *state.fixed.entry(version.clone()).or_default() += 1;
        Poll::Ready(Ok(Some((
            number,
            FixedAcquisition {
                scheduler: self.scheduler.clone(),
                version,
            },
        ))))
    }
    pub(crate) fn poll_next(&mut self, cx: &mut Context<'_>) -> Poll<Result<Next>> {
        use crate::peer::subscriptions::Demand as WireDemand;
        use crate::peer::subscriptions::PageInterval;
        let mut state = self.scheduler.state.lock().unwrap();
        let demand = state.demands.get_mut(&self.id).ok_or(Error::Cancelled)?;
        if let Some(page) = demand.results.pop_front() {
            return Poll::Ready(Ok(Next::Page(page)));
        }
        if demand.next == demand.end {
            return Poll::Ready(Ok(Next::End));
        }
        demand.waker = Some(cx.waker().clone());
        if !demand.eligible(demand.next) {
            return Poll::Pending;
        }
        let version = demand.version.clone();
        let ticket = state.gate_ticket(self.id)?;
        if state.fixed.contains_key(&version) || state.earlier_turn(&version, ticket, false) {
            return Poll::Pending;
        }
        let mut intervals = Vec::new();
        // The ticket belongs to this reader. Including disjoint lower demand
        // lets a provider spend every turn on somebody else's oldest page.
        // Completion still fans out to all eligible overlapping readers.
        {
            let demand = &state.demands[&self.id];
            if !demand.turn
                || demand.selected.len() >= 63
                || !demand.credits.can_reserve(PAGE_BYTES)
            {
                intervals.push(PageInterval {
                    start: demand.next,
                    end: demand.next + 1,
                });
                // With less than a full page of byte credit, only boundary
                // slices can fit. Do not advertise ineligible interior pages.
                let tail = demand.end - 1;
                if demand.turn && tail != demand.next && demand.eligible(tail) {
                    intervals.push(PageInterval {
                        start: tail,
                        end: tail + 1,
                    });
                }
            } else {
                let mut start = demand.next;
                for hole in &demand.selected {
                    if start < *hole {
                        intervals.push(PageInterval { start, end: *hole });
                    }
                    start = *hole + 1;
                }
                if start < demand.end {
                    intervals.push(PageInterval {
                        start,
                        end: demand.end,
                    });
                }
            }
        }
        intervals.sort_unstable_by_key(|interval| interval.start);
        let mut merged: Vec<PageInterval> = Vec::new();
        for interval in intervals {
            if let Some(last) = merged.last_mut()
                && interval.start <= last.end
            {
                last.end = last.end.max(interval.end);
            } else {
                merged.push(interval);
            }
        }
        // Fragmented demand fails explicitly instead of dropping distant pages.
        let demand = WireDemand::new(merged).map_err(|_| Error::Overloaded)?;
        if !state.selecting.insert(version.clone()) {
            return Poll::Pending;
        }
        state.demands.get_mut(&self.id).unwrap().gate_ticket = None;
        Poll::Ready(Ok(Next::Select(Selection {
            scheduler: self.scheduler.clone(),
            version,
            demand,
        })))
    }
    pub(crate) fn issued(&mut self, number: PageNumber) -> Result<()> {
        self.scheduler
            .state
            .lock()
            .unwrap()
            .demands
            .get_mut(&self.id)
            .ok_or(Error::Cancelled)?
            .credits
            .issued(number)
            .map_err(window_error)
    }
    pub(crate) fn release(&mut self, number: PageNumber, length: u32) -> Result<()> {
        self.scheduler
            .state
            .lock()
            .unwrap()
            .demands
            .get_mut(&self.id)
            .ok_or(Error::Cancelled)?
            .credits
            .release(number, u64::from(length))
            .map_err(window_error)
    }
    pub(crate) fn ordered(&self) -> bool {
        self.scheduler.state.lock().unwrap().demands[&self.id].ordered
    }
    pub(crate) fn exhausted(&self) -> bool {
        let state = self.scheduler.state.lock().unwrap();
        let demand = &state.demands[&self.id];
        demand.next == demand.end
    }
}
impl State {
    /// One ticket per capacity-bearing polling demand, not per page or poll.
    /// Ordered work batches across earlier ordered tickets, but cannot extend
    /// its batch past a queued selection. Selection yields to all earlier tickets.
    /// Thus either mode can drain the other mode's finite accepted work without
    /// weakening the actual-completion exclusion held by the worker guards.
    fn gate_ticket(&mut self, id: u64) -> Result<u64> {
        let demand = self.demands.get_mut(&id).ok_or(Error::Cancelled)?;
        if let Some(ticket) = demand.gate_ticket {
            return Ok(ticket);
        }
        self.sequence = self.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        demand.gate_ticket = Some(self.sequence);
        Ok(self.sequence)
    }
    fn earlier_turn(&self, version: &ObjectVersion, ticket: u64, ordered: bool) -> bool {
        self.demands.values().any(|demand| {
            &demand.version == version
                && (!ordered || !demand.ordered)
                && demand.gate_ticket.is_some_and(|other| other < ticket)
                && demand.results.is_empty()
                && demand.eligible(demand.next)
        })
    }
}
/// Kept by the worker command, not the client future. Detachment cannot permit
/// provider selection to race accepted fixed-page I/O before actual completion.
pub(crate) struct FixedAcquisition {
    scheduler: Arc<Scheduler>,
    version: ObjectVersion,
}
impl Drop for FixedAcquisition {
    fn drop(&mut self) {
        let wake = {
            let mut state = self.scheduler.state.lock().unwrap();
            let count = state.fixed.get_mut(&self.version).unwrap();
            *count -= 1;
            if *count != 0 {
                return;
            }
            state.fixed.remove(&self.version);
            state
                .demands
                .values_mut()
                .filter(|d| d.version == self.version)
                .filter_map(|d| d.waker.take())
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }
}
impl Demand {
    fn eligible(&self, number: u64) -> bool {
        if number < self.next
            || number >= self.end
            || self.selected.contains(&number)
            || (number != self.next && (self.ordered || !self.turn || self.selected.len() >= 64))
        {
            return false;
        }
        self.range
            .slice_at(PageNumber(number))
            .ok()
            .flatten()
            .is_some_and(|slice| self.credits.can_reserve(u64::from(slice.length)))
    }
}
impl Drop for DemandLease {
    fn drop(&mut self) {
        let mut state = self.scheduler.state.lock().unwrap();
        if let Some(demand) = state.demands.remove(&self.id) {
            // A canceled waiter must not strand the next ticket. Wake outside
            // the lock; accepted work still owns its separate completion guard.
            let wake = state
                .demands
                .values_mut()
                .filter(|other| other.version == demand.version)
                .filter_map(|other| other.waker.take())
                .collect::<Vec<_>>();
            drop(state);
            for waker in wake {
                waker.wake();
            }
        }
    }
}
// Invalid credit transitions are client request errors, not configuration errors.
fn window_error(error: flow_control::Error) -> Error {
    match error {
        flow_control::Error::InvalidInput => Error::InvalidRequest,
        other => other.into(),
    }
}

/// Client range progress admits independent pages under bounded child allowances.
enum RangeBudget {
    ClientPages {
        deadline: Instant,
    },
    ProgressingPages {
        timeout: Duration,
    },
    #[cfg(test)]
    Shared(AcquisitionBudget),
}
#[cfg(test)]
pub(crate) fn client_page_budget_for_test(deadline: Instant) -> AcquisitionBudget {
    RangeBudget::ClientPages { deadline }
        .next_page(false)
        .unwrap()
        .unwrap()
}
impl RangeBudget {
    fn next_page(&mut self, _pending: bool) -> Result<Option<AcquisitionBudget>> {
        match self {
            Self::ProgressingPages { timeout } => Ok(Some(AcquisitionBudget::new(
                uring_runtime::environment::now() + *timeout,
                8,
                16,
            ))),
            Self::ClientPages { deadline } => {
                if uring_runtime::environment::now() >= *deadline {
                    return Err(Error::DeadlineExceeded);
                }
                Ok(Some(AcquisitionBudget::new(*deadline, 8, 16)))
            }
            #[cfg(test)]
            Self::Shared(budget) => {
                let attempts = budget.remaining_attempts().min(8);
                let links = budget.remaining_links().min(16);
                if _pending && (attempts == 0 || links < 4) {
                    return Ok(None);
                }
                budget.partition(attempts, links).map(Some)
            }
        }
    }
    fn complete(&mut self, _remaining: AcquisitionBudget) -> Result<()> {
        match self {
            Self::ClientPages { .. } | Self::ProgressingPages { .. } => Ok(()),
            #[cfg(test)]
            Self::Shared(budget) => budget.reunite(_remaining),
        }
    }
}

enum WindowPage {
    Waiting(Operation<'static, (Result<PageResult>, AcquisitionBudget)>),
    Ready(Result<PageResult>),
}

pub struct RangeStreams {
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
    pipe_admission: Option<Operation<'static, flow_control::pipe::PipeLease<AdmissionPolicy>>>,
    prefetch_error: Option<Error>,
    selected_ready: Option<PageResult>,
    selection: Option<Operation<'static, (Result<PageResult>, AcquisitionBudget)>>,
    retained: std::collections::BTreeMap<PageNumber, crate::memory::VerifiedPage>,
    subscription: Option<DemandLease>,
    metadata: ObjectMetadata,
    range: ResolvedRange,
    context: OriginContext,
    membership: std::sync::Arc<crate::topology::Membership>,
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
        directory: Arc<WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
    ) -> Self {
        Self {
            directory,
            delivery,
            window_pages,
        }
    }
    pub fn directory(&self) -> &Arc<WorkerDirectory> {
        &self.directory
    }
    /// Open a client range with a bounded allowance for each distinct page and
    /// the original request deadline.
    pub fn open(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: RequestScope,
    ) -> Result<RangeStream> {
        let budget = RangeBudget::ClientPages {
            deadline: scope.deadline.0,
        };
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
        Ok(RangeStream {
            pipe_admission: None,
            prefetch_error: None,
            selected_ready: None,
            selection: None,
            retained: std::collections::BTreeMap::new(),
            subscription: None,
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
        })
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
        let demand = self.directory.subscriptions.register(
            self.metadata.version.clone(),
            self.range,
            pages,
            bytes,
            ordered,
        )?;
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
    /// in the acquisition window. Delivered leases consume client credit, not
    /// acquisition slots; their full plaintext allocations remain admitted.
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
        // Credit is reserved before dispatch and covers ready plus retained
        // pages. Retained VerifiedPage clones keep their original full allocation
        // charges, even when byte credit accounts only a partial boundary slice.
        while self.ready.len() < self.window_pages {
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
            scope.deadline.0 = uring_runtime::environment::now() + timeout;
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
            if self.subscription.is_none() {
                self.terminate();
                return Err(Error::InvalidRequest);
            }
            self.next_subscription_slice().await
        })
    }
    pub fn cancel(&mut self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.terminate();
            self.scope.cancel()
        })
    }
    fn terminate(&mut self) {
        self.pipe_admission = None;
        self.terminated = true;
        self.next_page = None;
        self.ready.clear();
        self.retained.clear();
        self.subscription = None;
        self.selection = None;
        self.selected_ready = None;
    }

    // Duplex delivery drops next_slice between polls to process release_page.
    // Keep the FIFO guard, cancellation registration, and original admission
    // deadline in the stream instead of in that temporary borrowing future.
    async fn admit_pipe(
        &mut self,
        scope: &RequestScope,
    ) -> Result<flow_control::pipe::PipeLease<AdmissionPolicy>> {
        let admission = self.pipe_admission.get_or_insert_with(|| {
            let delivery = self.delivery.clone();
            let scope = scope.clone();
            Box::pin(async move { delivery.admit(&scope).await })
        });
        let result = poll_fn(|cx| admission.as_mut().poll(cx)).await;
        self.pipe_admission = None;
        result
    }

    async fn next_ordered_slice(&mut self) -> Result<Option<ReaderLease>> {
        let scope = self.operation_scope();
        let cancellation = scope.cancellation.subscribe()?;
        poll_fn(|cx| -> Poll<Result<()>> {
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
        let pipe = self.admit_pipe(&scope).await?;
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
                    let pipe = self.admit_pipe(&scope).await?;
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
pub(super) mod tests {
    use super::*;
    use crate::admission::AdmissionPolicy;
    use crate::admission::ResourceClass;
    use crate::http::new_pipe_pool;
    use crate::model::ByteRange;
    use crate::model::CacheKey;
    use crate::model::ExpiresAt;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::PAGE_BYTES;
    use crate::model::RequestId;
    use crate::model::StrongEtag;
    use crate::model::WorkerId;
    use crate::runtime::Reactor;
    use crate::topology::Membership;
    use racer_control_wire::CacheId;
    use racer_control_wire::MembershipVersion;

    struct Fixture {
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        pipes: Rc<flow_control::pipe::PipePool<AdmissionPolicy>>,
        streams: RangeStreams,
    }

    impl Fixture {
        fn new(limits: crate::config::Limits, capacity: usize) -> Self {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let pipes = Rc::new(new_pipe_pool(admission.clone()));
            let directory = Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    capacity,
                )
                .unwrap(),
            );
            let streams = RangeStreams::new(
                directory,
                Rc::new(Delivery::new(
                    pipes.clone(),
                    reactor,
                    Duration::from_secs(30),
                )),
                capacity,
            );
            Self {
                admission,
                pipes,
                streams,
            }
        }

        fn open(
            &self,
            metadata: &ObjectMetadata,
            range: ResolvedRange,
            scope: RequestScope,
        ) -> RangeStream {
            self.streams
                .open(
                    metadata.clone(),
                    range,
                    OriginContext {
                        object: metadata.version.object.clone(),
                        metadata: None,
                        authorization: None,
                    },
                    Arc::new(Membership::validate(MembershipVersion(1), vec![]).unwrap()),
                    scope,
                )
                .unwrap()
        }
    }
    fn remaining_credits(budget: &RangeBudget) -> (u32, u8) {
        let RangeBudget::Shared(budget) = budget else {
            panic!("expected aggregate budget")
        };
        (budget.remaining_attempts(), budget.remaining_links())
    }
    pub(crate) fn page_result(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        metadata: &ObjectMetadata,
        number: u64,
    ) -> PageResult {
        use crate::admission::ResourceClass;
        use crate::memory::CiphertextBytes;
        use crate::memory::CiphertextPage;
        use crate::memory::VerifiedBytes;
        use crate::memory::VerifiedPage;
        use crate::model::Nonce;
        use crate::model::PageEnvelope;
        let length = (metadata.length - number * PAGE_BYTES).min(PAGE_BYTES) as usize;
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(number),
        };
        let cache = Some(&metadata.version.object.cache);
        PageResult {
            metadata: metadata.clone(),
            plaintext: VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page: page.clone(),
                    bytes: vec![number as u8; length],
                    reservation: admission
                        .reserve(cache, ResourceClass::Plaintext, length)
                        .unwrap(),
                }),
            },
            ciphertext: CiphertextPage {
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: Default::default(),
                    envelope: PageEnvelope {
                        page,
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: length as u32 + 16,
                    },
                    bytes: vec![0; length + 16],
                    reservation: admission
                        .reserve(cache, ResourceClass::Ciphertext, length + 16)
                        .unwrap(),
                }),
            },
        }
    }

    #[test]
    fn retained_boundary_slices_keep_full_plaintext_charged_until_exact_release() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let admission = &f.admission;
        let mut metadata = metadata();
        metadata.length = 2 * PAGE_BYTES;
        let range = ByteRange::Closed {
            first: PAGE_BYTES - 1,
            last: PAGE_BYTES,
        }
        .resolve(metadata.length)
        .unwrap();
        let scope = RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
            .unwrap();
        let mut stream = f.open(&metadata, range, scope);
        for number in 0..2 {
            stream.ready.push_back((
                PageNumber(number),
                WindowPage::Ready(Ok(page_result(&admission, &metadata, number))),
            ));
        }
        stream.configure_subscription(4, PAGE_BYTES, true).unwrap();
        // Injected ready pages still need the scheduler's exact credit reservations.
        let demand = stream.subscription.as_mut().unwrap();
        for number in 0..2 {
            assert_eq!(subscriptions::ordered(demand), Some(PageNumber(number)));
        }
        for number in 0..2 {
            let reader = futures::executor::block_on(stream.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(reader.slice().page, PageNumber(number));
            assert_eq!(reader.slice().length, 1);
            drop(reader);
        }
        assert_eq!(stream.retained.len(), 2);
        // Idle recycled buffers are still charged; reclaim only those to isolate
        // the unreleased leases, which must remain charged despite reclamation.
        admission.reclaim_buffers();
        assert_eq!(
            admission.used(ResourceClass::Plaintext),
            2 * PAGE_BYTES as usize
        );
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(
            stream.release_page(PageNumber(0), 2),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            admission.used(ResourceClass::Plaintext),
            2 * PAGE_BYTES as usize
        );
        stream.release_page(PageNumber(0), 1).unwrap();
        admission.reclaim_buffers();
        assert_eq!(
            admission.used(ResourceClass::Plaintext),
            PAGE_BYTES as usize
        );
        futures::executor::block_on(stream.cancel()).unwrap();
        admission.reclaim_buffers();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert!(stream.retained.is_empty());
    }

    #[test]
    fn later_page_pipe_wait_survives_temporary_polls_and_cleans_up() {
        use crate::test_support::WakeCounter;
        for ordered in [false, true] {
            for mode in ["release", "cancel", "drop"] {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.pipes = std::num::NonZeroUsize::new(1).unwrap();
                let f = Fixture::new(limits, 2);
                let admission = &f.admission;
                let pipes = &f.pipes;
                let metadata = metadata();
                let scope =
                    RequestScope::new(RequestId([8; 16]), Instant::now() + Duration::from_secs(60))
                        .unwrap();
                let range = ByteRange::From(PAGE_BYTES - 1)
                    .resolve(metadata.length)
                    .unwrap();
                let mut stream = f.open(&metadata, range, scope.clone());
                for number in 0..2 {
                    stream.ready.push_back((
                        PageNumber(number),
                        WindowPage::Ready(Ok(page_result(&admission, &metadata, number))),
                    ));
                }
                stream
                    .configure_subscription(2, PAGE_BYTES, ordered)
                    .unwrap();
                stream.next_page = None;
                let demand = stream.subscription.as_mut().unwrap();
                for number in 0..2 {
                    if ordered {
                        assert_eq!(subscriptions::ordered(demand), Some(PageNumber(number)));
                    } else {
                        subscriptions::selection(demand)
                            .complete(page_result(&admission, &metadata, number))
                            .unwrap();
                        assert_eq!(subscriptions::selected(demand), Some(PageNumber(number)));
                    }
                }
                if !ordered {
                    let (_, WindowPage::Ready(Ok(result))) = stream.ready.pop_front().unwrap()
                    else {
                        panic!()
                    };
                    stream.selected_ready = Some(result);
                }
                let first = futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .unwrap();
                assert_eq!(first.slice().page, PageNumber(0));
                drop(first);
                // Exercise the production unordered selection path as well as ordered delivery.
                if !ordered {
                    let (_, WindowPage::Ready(Ok(result))) = stream.ready.pop_front().unwrap()
                    else {
                        panic!()
                    };
                    stream.selected_ready = Some(result);
                }
                let held = pipes.acquire().unwrap();
                let baseline = admission.used(ResourceClass::RequestContext);
                let wakes = Arc::new(WakeCounter::default());
                let waker = std::task::Waker::from(wakes.clone());
                let mut cx = Context::from_waker(&waker);
                assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
                let waiting = admission.used(ResourceClass::RequestContext);
                assert!(
                    waiting > baseline,
                    "temporary future must retain FIFO admission"
                );
                stream.release_page(PageNumber(0), 1).unwrap();
                for _ in 0..3 {
                    assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
                    assert_eq!(admission.used(ResourceClass::RequestContext), waiting);
                }
                let before = wakes.count();
                match mode {
                    "release" => {
                        drop(held);
                        assert!(
                            wakes.count() > before,
                            "pipe release alone must wake later-page delivery"
                        );
                        let Poll::Ready(Ok(Some(reader))) =
                            stream.next_slice().as_mut().poll(&mut cx)
                        else {
                            panic!("woken waiter did not progress")
                        };
                        assert_eq!(reader.slice().page, PageNumber(1));
                        drop(reader);
                        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
                        drop(stream);
                    }
                    "cancel" => {
                        scope.cancel().unwrap();
                        assert!(wakes.count() > before);
                        assert!(matches!(
                            stream.next_slice().as_mut().poll(&mut cx),
                            Poll::Ready(Err(Error::Cancelled))
                        ));
                        assert!(stream.pipe_admission.is_none());
                        drop((stream, held));
                    }
                    _ => drop((stream, held)),
                }
                assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
                admission.reclaim_buffers();
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            }
        }
    }
    #[test]
    fn credit_starved_stream_never_holds_pipe_and_cancel_detaches_demand() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 1);
        let admission = &f.admission;
        let pipes = &f.pipes;
        let directory = &f.streams.directory;
        let metadata = metadata();
        let range = ByteRange::From(0).resolve(metadata.length).unwrap();
        let scope = RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(60))
            .unwrap();
        let mut stream = f.open(&metadata, range, scope.clone());
        stream.configure_subscription(1, PAGE_BYTES, false).unwrap();
        // Model a delivered page still owned by a slow caller.
        let demand = stream.subscription.as_mut().unwrap();
        subscriptions::selection(demand)
            .complete(page_result(&admission, &metadata, 0))
            .unwrap();
        assert_eq!(subscriptions::selected(demand), Some(PageNumber(0)));
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
        use std::cell::Cell;
        use std::time::Duration;
        for expire in [false, true] {
            let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
            let mut metadata = metadata();
            metadata.length = 100 * PAGE_BYTES;
            let scope =
                RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(metadata.length).unwrap(),
                scope.clone(),
            );
            stream
                .configure_subscription(2, 2 * PAGE_BYTES, true)
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
                stream.next_page = Some(PageNumber(number + 1));
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
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        }
    }
    #[test]
    fn out_of_order_completion_waits_for_front_and_returns_only_unused_credits() {
        use std::cell::Cell;
        use std::time::Duration;
        use std::time::Instant;
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
        use std::time::Duration;
        use std::time::Instant;
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
        actual.expires_at = ExpiresAt::test_time(std::time::SystemTime::now());
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
        use crate::admission::ResourceClass;
        use crate::client::Responses;
        use crate::http::Codec;
        use crate::http::new_pipe_pool;
        use crate::model::RequestId;
        use crate::read::ReadResponse;
        use crate::runtime::Reactor;
        use std::io::Read;
        use std::io::Write;
        use std::os::unix::net::UnixStream;
        use std::time::Duration;
        let total = 4 * PAGE_BYTES + 17;
        let range = ByteRange::From(PAGE_BYTES).resolve(total).unwrap();
        for (capped, progressing) in [(true, false), (false, true), (false, false)] {
            let clock = uring_runtime::environment::SimulationClock::new(55);
            let environment = clock.environment(0);
            let _clock = environment.enter();
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = Rc::new(crate::http::new_io(
                reactor.clone(),
                Codec::new(32768),
                admission.clone(),
                if capped {
                    PAGE_BYTES + 16
                } else {
                    i64::MAX as u64
                },
            ));
            let delivery = Rc::new(Delivery::new(
                Rc::new(new_pipe_pool(admission.clone())),
                reactor.clone(),
                Duration::from_secs(30),
            ));
            let mut metadata = metadata();
            metadata.length = total;
            metadata.version.object.cache = CacheId(crate::test_support::security::CACHE.into());
            let (client_socket, origin_socket) =
                racer_control_wire::canonical_socket_paths("framing").unwrap();
            let worker = crate::test_support::ReadWorker::new(
                racer_control_wire::CacheDefinition {
                    id: metadata.version.object.cache.clone(),
                    name: "framing".into(),
                    client_socket,
                    origin_socket,
                },
                metadata.clone(),
                admission.clone(),
                reactor.clone(),
                delivery.clone(),
                4,
            );
            let _queue = worker.drivers.enter();
            let scope = RequestScope::new(
                RequestId([7; 16]),
                uring_runtime::environment::now()
                    + if progressing {
                        Duration::from_millis(30)
                    } else {
                        Duration::from_secs(60)
                    },
            )
            .unwrap();
            let membership = worker.membership.clone();
            let mut stream = worker
                .streams
                .open(
                    metadata.clone(),
                    range,
                    OriginContext {
                        object: metadata.version.object.clone(),
                        metadata: None,
                        authorization: None,
                    },
                    membership,
                    scope.clone(),
                )
                .unwrap();
            stream
                .configure_subscription(4, 4 * PAGE_BYTES, false)
                .unwrap();
            let response = ReadResponse {
                metadata,
                range: Some(range),
                body: Some(stream),
            };
            let responses = Responses::new(io.clone(), delivery);
            let headers_sent = Arc::new(std::sync::atomic::AtomicBool::new(false));
            let reader_headers = headers_sent.clone();
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
                reader_headers.store(true, std::sync::atomic::Ordering::Release);
                let head = String::from_utf8(head).unwrap();
                assert!(
                    head.starts_with("HTTP/1.1 200"),
                    "progressing={progressing}: {head}"
                );
                assert!(head.contains(&format!(
                    "Content-Length: {}\r\n",
                    3 * PAGE_BYTES + 17 + 5 * 21
                )));
                let mut scratch = [0; 65536];
                for number in 1..=4 {
                    let mut left = if number == 4 { 17 } else { PAGE_BYTES as usize };
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
                    while left != 0 {
                        let count = left.min(scratch.len());
                        client.read_exact(&mut scratch[..count]).unwrap();
                        assert!(scratch[..count].iter().all(|value| *value == number));
                        left -= count;
                    }
                }
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
            });
            let work = async {
                let connection = crate::http::from_accepted(server.into(), &admission)?;
                let head = io.receive_head(connection, &scope).await?;
                let metrics = crate::telemetry::Metrics::default();
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
            };
            let mut work = std::pin::pin!(work);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let result = loop {
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    break result;
                }
                reactor.poll_budgeted(128).unwrap();
                worker.poll(&mut cx);
                reactor.wait(Duration::from_millis(1)).unwrap();
                if progressing && headers_sent.load(std::sync::atomic::Ordering::Acquire) {
                    clock.advance(Duration::from_millis(1));
                }
            };
            if capped {
                assert_eq!(result, Err(Error::InvalidRequest));
            } else {
                result.unwrap();
            }
            reader.join().unwrap();
            // Dropping a capped response detaches waiters, but accepted worker
            // acquisitions and crypto completions still need their owner polled.
            let drain_deadline = Instant::now() + Duration::from_secs(5);
            while worker.drivers.pending() != 0 || reactor.in_flight() != 0 {
                worker.poll(&mut cx);
                reactor.poll_budgeted(128).unwrap();
                reactor.wait(Duration::from_millis(1)).unwrap();
                assert!(Instant::now() < drain_deadline, "read worker did not drain");
            }
            worker.poll(&mut cx);
            admission.reclaim_buffers();
            if progressing {
                assert!(
                    uring_runtime::environment::now() > scope.deadline.0,
                    "stream must cross the scaled old absolute deadline"
                );
            }
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn progressing_page_children_keep_deadlines_and_retry_limits_across_long_streams() {
        let clock = uring_runtime::environment::SimulationClock::new(81);
        let _environment = clock.environment(0).enter();
        let timeout = Duration::from_millis(30);
        let old_deadline = uring_runtime::environment::now() + timeout;
        let mut budget = RangeBudget::ProgressingPages { timeout };
        for _ in 0..12 {
            let mut page = budget.next_page(false).unwrap().unwrap();
            let deadline = page.deadline();
            assert_eq!(deadline, uring_runtime::environment::now() + timeout);
            for _ in 0..8 {
                page.begin_attempt(uring_runtime::environment::now(), deadline)
                    .unwrap();
            }
            assert_eq!(
                page.begin_attempt(uring_runtime::environment::now(), deadline),
                Err(Error::Unavailable)
            );
            clock.advance(Duration::from_millis(20));
            assert_eq!(page.deadline(), deadline);
            budget.complete(page).unwrap();
        }
        assert!(uring_runtime::environment::now() > old_deadline);
        let mut stalled = budget.next_page(false).unwrap().unwrap();
        clock.advance(timeout);
        assert_eq!(
            stalled.begin_attempt(uring_runtime::environment::now(), stalled.deadline()),
            Err(Error::DeadlineExceeded)
        );
    }

    mod subscriptions {
        use super::*;
        use crate::admission::AdmissionPolicy;
        use crate::model::ByteRange;
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::StrongEtag;
        use flow_control::Quotas;
        use racer_control_wire::CacheId;
        use std::task::Context;
        use std::task::Poll;
        fn version() -> ObjectVersion {
            ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            }
        }
        pub(crate) fn ordered(reader: &mut DemandLease) -> Option<PageNumber> {
            match reader.poll_ordered(&mut Context::from_waker(futures::task::noop_waker_ref())) {
                Poll::Ready(Ok(Some((page, guard)))) => {
                    drop(guard);
                    Some(page)
                }
                Poll::Ready(Ok(None)) | Poll::Pending => None,
                Poll::Ready(Err(error)) => panic!("ordered assignment: {error:?}"),
            }
        }
        pub(crate) fn selection(reader: &mut DemandLease) -> Selection {
            match reader.poll_next(&mut Context::from_waker(futures::task::noop_waker_ref())) {
                Poll::Ready(Ok(Next::Select(selection))) => selection,
                _ => panic!("expected provider selection"),
            }
        }
        pub(crate) fn selected(reader: &mut DemandLease) -> Option<PageNumber> {
            match reader.poll_next(&mut Context::from_waker(futures::task::noop_waker_ref())) {
                Poll::Ready(Ok(Next::Page(page))) => Some(page.plaintext.page().number),
                Poll::Pending | Poll::Ready(Ok(Next::End)) => None,
                _ => panic!("expected completed provider page"),
            }
        }
        fn complete(selection: Selection, number: u64, length: u64) {
            let metadata = crate::model::ObjectMetadata {
                version: selection.version.clone(),
                length,
                content_type: None,
                expires_at: crate::model::ExpiresAt::from_system_time(std::time::UNIX_EPOCH)
                    .unwrap(),
            };
            let admission: Quotas<AdmissionPolicy> = Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            selection
                .complete(super::tests::page_result(&admission, &metadata, number))
                .unwrap();
        }
        #[test]
        fn mixed_gate_tickets_bound_turns_under_sustained_ordered_demand() {
            use std::task::Poll;
            let scheduler = Scheduler::new(4);
            let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
            let mut a = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, true)
                .unwrap();
            let mut b = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, true)
                .unwrap();
            let mut unordered = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..32 {
                let Poll::Ready(Ok(Some((pa, ga)))) = a.poll_ordered(&mut cx) else {
                    panic!("ordered resumes")
                };
                let Poll::Ready(Ok(Some((pb, gb)))) = b.poll_ordered(&mut cx) else {
                    panic!("concurrent ordered resumes")
                };
                assert!(unordered.poll_next(&mut cx).is_pending());
                let mut newcomer = scheduler
                    .register(version(), range, 1, PAGE_BYTES, true)
                    .unwrap();
                assert!(
                    newcomer.poll_ordered(&mut cx).is_pending(),
                    "new readers cannot bypass a waiting selection"
                );
                drop(newcomer);
                // Keep both ordered readers capacity-bearing, and poll them first
                // at every boundary. Neither can refill ahead of the waiting ticket.
                assert!(a.poll_ordered(&mut cx).is_pending());
                assert!(b.poll_ordered(&mut cx).is_pending());
                drop(ga);
                a.issued(pa).unwrap();
                a.release(pa, PAGE_BYTES as u32).unwrap();
                assert!(a.poll_ordered(&mut cx).is_pending());
                assert!(
                    unordered.poll_next(&mut cx).is_pending(),
                    "last fence still live"
                );
                drop(gb);
                b.issued(pb).unwrap();
                b.release(pb, PAGE_BYTES as u32).unwrap();
                for _ in 0..8 {
                    assert!(a.poll_ordered(&mut cx).is_pending());
                    assert!(b.poll_ordered(&mut cx).is_pending());
                }
                let Poll::Ready(Ok(Next::Select(selection))) = unordered.poll_next(&mut cx) else {
                    panic!("selection gets the very next turn after two fences")
                };
                assert!(a.poll_ordered(&mut cx).is_pending());
                // Simulate an unsuccessful selection. Even immediate retries cannot
                // starve the ordered tickets queued during the preceding batch.
                drop(selection);
                assert!(unordered.poll_next(&mut cx).is_pending());
            }
            drop((a, b, unordered));
            let state = scheduler.state.lock().unwrap();
            assert!(state.fixed.is_empty());
            assert!(state.selecting.is_empty());
            assert!(state.demands.is_empty());
        }
        #[test]
        fn canceled_gate_waiter_wakes_successor_without_releasing_live_fences() {
            use std::sync::atomic::AtomicUsize;
            use std::sync::atomic::Ordering;
            use std::task::Poll;
            #[derive(Default)]
            struct Wakes(AtomicUsize);
            impl futures::task::ArcWake for Wakes {
                fn wake_by_ref(this: &Arc<Self>) {
                    this.0.fetch_add(1, Ordering::Relaxed);
                }
            }
            let wakes = Arc::new(Wakes::default());
            let waker = futures::task::waker(wakes.clone());
            let mut cx = std::task::Context::from_waker(&waker);
            let scheduler = Scheduler::new(3);
            let range = ByteRange::From(0).resolve(100 * PAGE_BYTES).unwrap();
            let mut ordered = scheduler
                .register(version(), range, 4, 4 * PAGE_BYTES, true)
                .unwrap();
            let mut canceled = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            let Poll::Ready(Ok(Some((_, first)))) = ordered.poll_ordered(&mut cx) else {
                panic!()
            };
            assert!(canceled.poll_next(&mut cx).is_pending());
            assert!(ordered.poll_ordered(&mut cx).is_pending());
            let before = wakes.0.load(Ordering::Relaxed);
            drop(canceled);
            assert!(wakes.0.load(Ordering::Relaxed) > before);
            let Poll::Ready(Ok(Some((_, second)))) = ordered.poll_ordered(&mut cx) else {
                panic!("canceled selection no longer blocks refill")
            };
            let mut successor = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            assert!(successor.poll_next(&mut cx).is_pending());
            assert!(ordered.poll_ordered(&mut cx).is_pending());
            drop(ordered);
            assert!(successor.poll_next(&mut cx).is_pending());
            drop(first);
            assert!(successor.poll_next(&mut cx).is_pending());
            let before = wakes.0.load(Ordering::Relaxed);
            drop(second);
            assert!(wakes.0.load(Ordering::Relaxed) > before);
            assert!(matches!(
                successor.poll_next(&mut cx),
                Poll::Ready(Ok(Next::Select(_)))
            ));
            drop(successor);
            let state = scheduler.state.lock().unwrap();
            assert!(state.demands.is_empty());
            assert!(state.fixed.is_empty());
            assert!(state.selecting.is_empty());
        }
        #[test]
        fn ordered_reservations_are_exact_and_mixed_exclusion_outlives_demand() {
            let scheduler = Scheduler::new(3);
            let range = ByteRange::From(PAGE_BYTES - 3)
                .resolve(2 * PAGE_BYTES + 5)
                .unwrap();
            let mut ordered = scheduler
                .register(version(), range, 2, PAGE_BYTES, true)
                .unwrap();
            let mut unordered = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let std::task::Poll::Ready(Ok(Next::Select(selection))) = unordered.poll_next(&mut cx)
            else {
                panic!()
            };
            assert!(ordered.poll_ordered(&mut cx).is_pending());
            drop(selection);
            let std::task::Poll::Ready(Ok(Some((number, guard)))) = ordered.poll_ordered(&mut cx)
            else {
                panic!()
            };
            assert_eq!(number, PageNumber(0));
            {
                let state = scheduler.state.lock().unwrap();
                let credits = &state.demands[&ordered.id].credits;
                assert!(credits.can_reserve(PAGE_BYTES - 3));
                assert!(!credits.can_reserve(PAGE_BYTES - 2));
            }
            assert!(
                ordered.poll_ordered(&mut cx).is_pending(),
                "byte credit reserved before work"
            );
            assert!(unordered.poll_next(&mut cx).is_pending());
            drop(ordered);
            assert!(
                unordered.poll_next(&mut cx).is_pending(),
                "worker still owns exclusion"
            );
            drop(guard);
            assert!(matches!(
                unordered.poll_next(&mut cx),
                std::task::Poll::Ready(Ok(Next::Select(_)))
            ));
            drop(unordered);
            let state = scheduler.state.lock().unwrap();
            assert!(state.fixed.is_empty());
            assert!(state.selecting.is_empty());
            assert!(state.demands.is_empty());
        }
        #[test]
        fn production_aggregate_is_compact_exclusive_and_contracts_never_refill() {
            use crate::peer::subscriptions::PageInterval;
            use racer_control_wire::MembershipVersion;
            use racer_control_wire::NodeId;
            let scheduler = Scheduler::new(4);
            let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
            let mut first = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            let mut second = scheduler
                .register(
                    version(),
                    ByteRange::From(900_000 * PAGE_BYTES)
                        .resolve(range.end())
                        .unwrap(),
                    1,
                    PAGE_BYTES,
                    false,
                )
                .unwrap();
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let std::task::Poll::Ready(Ok(Next::Select(selection))) = first.poll_next(&mut cx)
            else {
                panic!()
            };
            assert_eq!(
                selection.demand.intervals(),
                &[PageInterval { start: 0, end: 1 }]
            );
            assert!(second.poll_next(&mut cx).is_pending());
            let deadline = uring_runtime::environment::now() + std::time::Duration::from_secs(30);
            let (one, _) = scheduler
                .contract(
                    version(),
                    MembershipVersion(1),
                    NodeId("provider".into()),
                    selection.demand.clone(),
                    deadline,
                )
                .unwrap();
            let (two, expires) = scheduler
                .contract(
                    version(),
                    MembershipVersion(1),
                    NodeId("provider".into()),
                    selection.demand.clone(),
                    deadline + std::time::Duration::from_secs(30),
                )
                .unwrap();
            assert_eq!(one.id, two.id);
            assert_eq!(two.sequence, one.sequence + 1);
            assert_eq!(two.page_budget + 1, one.page_budget);
            assert_eq!(two.byte_budget + PAGE_BYTES + 16, one.byte_budget);
            assert_eq!(expires, deadline);
            drop(first);
            assert!(
                second.poll_next(&mut cx).is_pending(),
                "detached consumer cannot release owned work"
            );
            drop(selection);
            assert!(matches!(
                second.poll_next(&mut cx),
                std::task::Poll::Ready(Ok(Next::Select(_)))
            ));
        }
        #[test]
        fn production_ticket_cannot_be_spent_on_rotating_disjoint_low_readers() {
            use std::task::Poll;
            let scheduler = Scheduler::new(3);
            let length = 1000 * PAGE_BYTES;
            let mut high = scheduler
                .register(
                    version(),
                    ByteRange::From(900 * PAGE_BYTES).resolve(length).unwrap(),
                    1,
                    PAGE_BYTES,
                    false,
                )
                .unwrap();
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for low_page in 0..32 {
                let mut low = scheduler
                    .register(
                        version(),
                        ByteRange::Closed {
                            first: low_page * PAGE_BYTES,
                            last: (low_page + 1) * PAGE_BYTES - 1,
                        }
                        .resolve(length)
                        .unwrap(),
                        1,
                        PAGE_BYTES,
                        false,
                    )
                    .unwrap();
                let Poll::Ready(Ok(Next::Select(selection))) = low.poll_next(&mut cx) else {
                    panic!()
                };
                assert!(high.poll_next(&mut cx).is_pending());
                drop(selection);
                assert!(low.poll_next(&mut cx).is_pending());
                let Poll::Ready(Ok(Next::Select(selection))) = high.poll_next(&mut cx) else {
                    panic!("oldest ticket must run")
                };
                assert_eq!(
                    selection.demand.intervals(),
                    &[crate::peer::subscriptions::PageInterval {
                        start: 900,
                        end: 901
                    }]
                );
                assert!(!selection.demand.contains(low_page));
                drop((selection, low));
            }
        }
        #[test]
        fn production_selection_advertises_only_credit_eligible_slices() {
            let scheduler = Scheduler::new(1);
            let range = ByteRange::From(PAGE_BYTES - 1)
                .resolve(3 * PAGE_BYTES + 2)
                .unwrap();
            let mut reader = scheduler
                .register(version(), range, 2, PAGE_BYTES, false)
                .unwrap();
            {
                let mut state = scheduler.state.lock().unwrap();
                let demand = state.demands.get_mut(&reader.id).unwrap();
                demand.turn = true;
                demand
                    .credits
                    .reserve(PageNumber(99), PAGE_BYTES - 2)
                    .unwrap();
            }
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            let std::task::Poll::Ready(Ok(Next::Select(selection))) = reader.poll_next(&mut cx)
            else {
                panic!()
            };
            assert!(selection.demand.contains(0));
            assert!(selection.demand.contains(3));
            assert!(!selection.demand.contains(1));
            assert!(!selection.demand.contains(2));
        }
        #[test]
        fn pending_and_delivered_share_exact_once_credit() {
            let mut credits = Window::new(2, 2 * PAGE_BYTES, PAGE_BYTES).unwrap();
            credits.reserve(PageNumber(0), PAGE_BYTES).unwrap();
            assert_eq!(
                credits.release(PageNumber(0), PAGE_BYTES),
                Err(flow_control::Error::InvalidInput)
            );
            credits.reserve(PageNumber(1), PAGE_BYTES).unwrap();
            assert!(!credits.can_reserve(PAGE_BYTES));
            assert_eq!(
                credits.reserve(PageNumber(2), 1),
                Err(flow_control::Error::Overloaded)
            );
            credits.issued(PageNumber(0)).unwrap();
            assert_eq!(
                credits.release(PageNumber(0), 1),
                Err(flow_control::Error::InvalidInput)
            );
            credits.release(PageNumber(0), PAGE_BYTES).unwrap();
            assert_eq!(
                credits.release(PageNumber(0), PAGE_BYTES),
                Err(flow_control::Error::InvalidInput)
            );
            assert!(credits.can_reserve(PAGE_BYTES));
        }
        #[test]
        fn scheduler_is_compact_bounded_and_preserves_ordered_head() {
            let scheduler = Scheduler::new(2);
            let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
            let mut first = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, true)
                .unwrap();
            let mut second = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, true)
                .unwrap();
            assert!(matches!(
                scheduler.register(version(), range, 2, 2 * PAGE_BYTES, true),
                Err(Error::Overloaded)
            ));
            assert_eq!(ordered(&mut first), Some(PageNumber(0)));
            assert_eq!(ordered(&mut first), Some(PageNumber(1)));
            assert_eq!(ordered(&mut second), Some(PageNumber(0)));
            assert_eq!(ordered(&mut second), Some(PageNumber(1)));
            let state = scheduler.state.lock().unwrap();
            assert_eq!(state.demands.len(), 2);
            assert!(
                state
                    .demands
                    .values()
                    .all(|demand| demand.selected.is_empty())
            );
            drop(state);
            drop(first);
            drop(second);
            let state = scheduler.state.lock().unwrap();
            assert!(state.demands.is_empty());
            assert!(state.fixed.is_empty());
        }
        #[test]
        fn unordered_provider_can_select_distant_pages_but_alternates_head_progress() {
            let scheduler = Scheduler::new(2);
            let range = ByteRange::From(0).resolve(100 * PAGE_BYTES).unwrap();
            let mut reader = scheduler
                .register(version(), range, 3, 3 * PAGE_BYTES, false)
                .unwrap();
            for number in [0, 5, 1] {
                let choice = selection(&mut reader);
                assert!(choice.demand.contains(number));
                if number != 5 {
                    assert_eq!(choice.demand.page_count(), 1);
                }
                complete(choice, number, range.end());
                assert_eq!(selected(&mut reader), Some(PageNumber(number)));
            }
        }
        #[test]
        fn whole_demand_hot_assignment_fans_out_without_slow_reader_backpressure() {
            let scheduler = Scheduler::new(4);
            let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
            let mut first = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, false)
                .unwrap();
            let mut second = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, false)
                .unwrap();
            let mut slow = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            let mut other_version = version();
            other_version.etag = StrongEtag::test_value("v2");
            let other = scheduler
                .register(other_version, range, 2, 2 * PAGE_BYTES, false)
                .unwrap();
            complete(selection(&mut first), 0, range.end());
            assert_eq!(selected(&mut first), Some(PageNumber(0)));
            complete(selection(&mut first), 900_000, range.end());
            assert_eq!(selected(&mut first), Some(PageNumber(900_000)));
            assert_eq!(selected(&mut second), Some(PageNumber(0)));
            assert_eq!(selected(&mut second), Some(PageNumber(900_000)));
            assert_eq!(selected(&mut slow), Some(PageNumber(0)));
            assert_eq!(selected(&mut slow), None);
            assert!(!slow.exhausted());
            assert!(
                scheduler.state.lock().unwrap().demands[&other.id]
                    .results
                    .is_empty()
            );
            first.issued(PageNumber(0)).unwrap();
            first.release(PageNumber(0), PAGE_BYTES as u32).unwrap();
            let choice = selection(&mut first);
            assert_eq!(
                choice.demand.intervals(),
                &[crate::peer::subscriptions::PageInterval { start: 1, end: 2 }]
            );
            complete(choice, 1, range.end());
            assert_eq!(selected(&mut first), Some(PageNumber(1)));
            assert_eq!(
                selected(&mut slow),
                None,
                "slow subscriber reserves no additional work"
            );
            drop((first, second, slow, other));
            assert!(scheduler.state.lock().unwrap().demands.is_empty());
        }
        #[test]
        fn bounded_holes_and_credit_state_survive_adversarial_hot_pages() {
            let scheduler = Scheduler::new(1);
            let range = ByteRange::From(0).resolve(1_000_000 * PAGE_BYTES).unwrap();
            let mut reader = scheduler
                .register(version(), range, 1, PAGE_BYTES, false)
                .unwrap();
            let mut seen = BTreeSet::new();
            for turn in 0..1000 {
                let choice = selection(&mut reader);
                let hot = 900_000 + turn;
                let number = if choice.demand.contains(hot) {
                    hot
                } else {
                    choice.demand.intervals()[0].start
                };
                complete(choice, number, range.end());
                let number = selected(&mut reader).unwrap();
                assert!(seen.insert(number));
                reader.issued(number).unwrap();
                reader.release(number, PAGE_BYTES as u32).unwrap();
                let state = scheduler.state.lock().unwrap();
                let demand = &state.demands[&reader.id];
                assert!(demand.selected.len() <= 64);
                assert!(demand.results.is_empty());
                assert!(demand.credits.is_empty());
                assert!(state.selecting.is_empty());
            }
            assert!(scheduler.state.lock().unwrap().demands[&reader.id].next >= 500);
        }
        #[test]
        fn partial_final_page_uses_exact_bytes_and_completion_does_not_require_release() {
            let scheduler = Scheduler::new(1);
            let range = ByteRange::From(PAGE_BYTES - 3)
                .resolve(PAGE_BYTES + 5)
                .unwrap();
            let mut reader = scheduler
                .register(version(), range, 2, PAGE_BYTES, true)
                .unwrap();
            assert_eq!(ordered(&mut reader), Some(PageNumber(0)));
            assert_eq!(ordered(&mut reader), Some(PageNumber(1)));
            assert!(reader.exhausted());
            assert_eq!(ordered(&mut reader), None);
            assert!(
                !scheduler.state.lock().unwrap().demands[&reader.id]
                    .credits
                    .is_empty()
            );
            reader.issued(PageNumber(0)).unwrap();
            assert_eq!(reader.release(PageNumber(0), 5), Err(Error::InvalidRequest));
            reader.release(PageNumber(0), 3).unwrap();
            assert_eq!(reader.release(PageNumber(0), 3), Err(Error::InvalidRequest));
            reader.issued(PageNumber(1)).unwrap();
            assert_eq!(reader.release(PageNumber(1), 3), Err(Error::InvalidRequest));
            reader.release(PageNumber(1), 5).unwrap();
            assert!(
                scheduler.state.lock().unwrap().demands[&reader.id]
                    .credits
                    .is_empty()
            );
            drop(reader);
            assert!(scheduler.state.lock().unwrap().demands.is_empty());
        }
        #[test]
        fn inflight_page_outside_prefix_is_shared_and_drop_preserves_other_subscriber() {
            let scheduler = Scheduler::new(2);
            let length = 1_000_000 * PAGE_BYTES;
            let range = ByteRange::From(0).resolve(length).unwrap();
            let mut supplier = scheduler
                .register(
                    version(),
                    ByteRange::From(800_000 * PAGE_BYTES)
                        .resolve(length)
                        .unwrap(),
                    1,
                    PAGE_BYTES,
                    false,
                )
                .unwrap();
            let pending = selection(&mut supplier);
            let mut reader = scheduler
                .register(version(), range, 2, 2 * PAGE_BYTES, false)
                .unwrap();
            assert_eq!(selected(&mut reader), None);
            complete(pending, 800_000, length);
            assert_eq!(selected(&mut supplier), Some(PageNumber(800_000)));
            complete(selection(&mut reader), 0, length);
            assert_eq!(selected(&mut reader), Some(PageNumber(0)));
            complete(selection(&mut reader), 800_000, length);
            assert_eq!(selected(&mut reader), Some(PageNumber(800_000)));
            let page = PageId {
                version: version(),
                number: PageNumber(800_000),
            };
            assert_eq!(scheduler.state.lock().unwrap().demands.len(), 2);
            drop(supplier);
            assert_eq!(scheduler.state.lock().unwrap().demands.len(), 1);
            reader.issued(page.number).unwrap();
            reader.release(page.number, PAGE_BYTES as u32).unwrap();
            complete(selection(&mut reader), 1, length);
            assert_eq!(selected(&mut reader), Some(PageNumber(1)));
        }
        #[test]
        fn credit_boundaries_and_invalid_transitions_preserve_request_errors() {
            let scheduler = Scheduler::new(1);
            let range = ByteRange::From(0).resolve(PAGE_BYTES).unwrap();
            for (pages, bytes) in [(1, PAGE_BYTES), (64, 64 * PAGE_BYTES)] {
                let mut reader = scheduler
                    .register(version(), range, pages, bytes, true)
                    .unwrap();
                assert!(reader.ordered());
                assert_eq!(reader.issued(PageNumber(0)), Err(Error::InvalidRequest));
                assert_eq!(ordered(&mut reader), Some(PageNumber(0)));
                assert_eq!(
                    reader.release(PageNumber(0), PAGE_BYTES as u32),
                    Err(Error::InvalidRequest)
                );
                reader.issued(PageNumber(0)).unwrap();
                assert_eq!(reader.issued(PageNumber(0)), Err(Error::InvalidRequest));
                reader.release(PageNumber(0), PAGE_BYTES as u32).unwrap();
                assert_eq!(
                    reader.release(PageNumber(0), PAGE_BYTES as u32),
                    Err(Error::InvalidRequest)
                );
            }
        }
        #[test]
        fn invalid_credit_contracts_never_admit_demand() {
            let scheduler = Scheduler::new(1);
            let range = ByteRange::From(0).resolve(1).unwrap();
            for (pages, bytes) in [
                (0, PAGE_BYTES),
                (65, PAGE_BYTES),
                (1, 0),
                (1, PAGE_BYTES - 1),
                (1, 64 * PAGE_BYTES + 1),
                (1, 65 * PAGE_BYTES),
            ] {
                assert!(matches!(
                    scheduler.register(version(), range, pages, bytes, false),
                    Err(Error::InvalidRequest)
                ));
            }
            assert!(scheduler.state.lock().unwrap().demands.is_empty());
            let mut credits = Window::new(1, PAGE_BYTES, PAGE_BYTES).unwrap();
            assert_eq!(
                credits.reserve(PageNumber(0), 0),
                Err(flow_control::Error::InvalidInput)
            );
            assert_eq!(
                credits.reserve(PageNumber(0), PAGE_BYTES + 1),
                Err(flow_control::Error::InvalidInput)
            );
            assert_eq!(
                credits.issued(PageNumber(0)),
                Err(flow_control::Error::InvalidInput)
            );
        }
    }
}
