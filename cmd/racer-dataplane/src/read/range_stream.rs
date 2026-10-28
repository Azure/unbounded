//! Per-read fetch window: pages are fetched out of order and delivered in order.

use super::dispatch::WorkerDirectory;
use super::flight::AcquisitionBudget;
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
use crate::read::sealed::Fetched;
use crate::read::sealed::SealedPage;
use crate::read::sealed::SealedReport;
use crate::read::sealed::SealedTarget;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use crate::store::holds::HoldEpoch;
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
use std::task::Waker;
use std::time::Duration;
use std::time::Instant;

#[cfg(test)]
use racer_core::read::SELECTIONS;
use racer_core::read::{self, DIRECT_AHEAD, SlotKind};

/// Flights requested per provider contract. A provider grants at most this
/// many pending selections per contract, or one if it predates multi-flight.
pub(crate) const REQUESTED_FLIGHTS: u16 = 4;
const _: () = assert!(REQUESTED_FLIGHTS <= racer_core::subscriptions::MAX_SUBSCRIPTION_FLIGHTS);

/// Spill fills (hold plus warm fill) a read keeps in flight.
const SPILL_FILLS: usize = 2;

/// Lifetime of a read's spill hold. Holds are renewed by nothing; a page that expires before
/// delivery is fetched again.
const SPILL_HOLD: Duration = Duration::from_secs(60);

/// One open read window registered for selection fan-out.
struct Reader {
    version: ObjectVersion,
    /// Window pages that still accept a page from any selection.
    wants: BTreeSet<u64>,
    /// Window pages covered by this reader's outstanding selections or direct
    /// fills. Other windows leave these pages to this reader's fan-out.
    busy: BTreeSet<u64>,
    /// Selected pages offered to this reader, bounded by its window.
    offered: VecDeque<PageResult>,
    waker: Option<Waker>,
}

/// One provider contract: (version, membership, provider).
type ContractKey = (ObjectVersion, u64, racer_control_wire::NodeId);

/// A peer subscription contract and the selections it has in flight.
struct Contract {
    subscription: crate::peer::subscriptions::Subscription,
    expires: Instant,
    /// Selections currently using the contract.
    inflight: u16,
    /// Flights asked of the provider by the latest request (1 when the field was omitted).
    requested: u16,
    /// Selections the provider allows pending at once: 1 until a `Selected`
    /// grant raises it, back to 1 after a refusal.
    granted: u16,
}

struct State {
    contracts: BTreeMap<ContractKey, Contract>,
    sequence: u64,
    readers: BTreeMap<u64, Reader>,
}

/// Node-wide registry of open read windows and peer subscription contracts.
pub(crate) struct Scheduler {
    capacity: usize,
    state: Mutex<State>,
}

impl Scheduler {
    /// Issue the next sequence of the node's contract with `provider` for `selection`, asking for
    /// `requested` flights.
    pub(crate) fn contract(
        &self,
        selection: &Selection,
        membership: racer_control_wire::MembershipVersion,
        provider: racer_control_wire::NodeId,
        requested: Option<u16>,
        deadline: Instant,
    ) -> Result<(crate::peer::subscriptions::Subscription, Instant)> {
        let mut used = selection.contract.lock().unwrap();
        if used.is_some() {
            return Err(Error::InvalidRequest);
        }
        let version = selection.version.clone();
        let demand = selection.demand.clone();
        let mut state = self.state.lock().unwrap();
        let now = uring_runtime::environment::now();
        // Expired contracts are dropped only once idle, so a late drop never
        // releases a flight of the contract that replaced it.
        state
            .contracts
            .retain(|_, contract| contract.expires > now || contract.inflight > 0);
        let key = (version.clone(), membership.0, provider);
        if !state.contracts.contains_key(&key) {
            if state.contracts.len() >= self.capacity.saturating_mul(3) {
                return Err(Error::Overloaded);
            }
            let mut id = [0; 16];
            uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Unavailable)?;
            state.contracts.insert(
                key.clone(),
                Contract {
                    subscription: crate::peer::subscriptions::Subscription {
                        id,
                        version,
                        demand: Default::default(),
                        sequence: 0,
                        page_budget: u32::MAX,
                        byte_budget: u64::from(u32::MAX) * (PAGE_BYTES + 16),
                        flights: None,
                    },
                    expires: deadline,
                    inflight: 0,
                    requested: 1,
                    granted: 1,
                },
            );
        }
        let contract = state.contracts.get_mut(&key).unwrap();
        // An expired contract still draining its selections takes no more;
        // once idle it is replaced by a fresh one.
        if contract.expires <= now && contract.inflight > 0 {
            return Err(Error::Overloaded);
        }
        let requested = requested.unwrap_or(1).max(1);
        // A single-flight request never has more than one pending selection.
        let granted = contract.granted.min(requested);
        if contract.inflight >= granted {
            return Err(Error::Overloaded);
        }
        contract.expires = contract.expires.min(deadline);
        let sequence = contract
            .subscription
            .sequence
            .checked_add(1)
            .ok_or(Error::Overloaded)?;
        let page_budget = contract
            .subscription
            .page_budget
            .checked_sub(1)
            .ok_or(Error::Unavailable)?;
        let byte_budget = contract
            .subscription
            .byte_budget
            .checked_sub(PAGE_BYTES + 16)
            .ok_or(Error::Unavailable)?;
        contract.subscription.sequence = sequence;
        contract.subscription.demand = demand;
        let outgoing = contract.subscription.clone();
        contract.subscription.page_budget = page_budget;
        contract.subscription.byte_budget = byte_budget;
        contract.requested = requested;
        contract.granted = granted;
        contract.inflight += 1;
        *used = Some(key);
        Ok((outgoing, contract.expires))
    }

    /// Record a provider's grant for `selection`'s contract. The grant never
    /// exceeds the flights requested.
    pub(crate) fn granted(
        &self,
        selection: &Selection,
        provider: &racer_control_wire::NodeId,
        flights: u16,
    ) {
        let Some(key) = selection.contract.lock().unwrap().clone() else {
            return;
        };
        if &key.2 != provider {
            return;
        }
        if let Some(contract) = self.state.lock().unwrap().contracts.get_mut(&key) {
            contract.granted = flights.clamp(1, contract.requested);
        }
    }

    /// The provider refused a multi-flight request: one flight from now on.
    pub(crate) fn refused(&self, selection: &Selection, provider: &racer_control_wire::NodeId) {
        let Some(key) = selection.contract.lock().unwrap().clone() else {
            return;
        };
        if &key.2 != provider {
            return;
        }
        if let Some(contract) = self.state.lock().unwrap().contracts.get_mut(&key) {
            contract.granted = 1;
        }
    }

    /// Unused granted flights across the live contracts of `version`.
    fn spare(&self, version: &ObjectVersion) -> usize {
        let now = uring_runtime::environment::now();
        self.state
            .lock()
            .unwrap()
            .contracts
            .iter()
            .filter(|((contracted, _, _), contract)| {
                contracted == version && contract.expires > now
            })
            .map(|(_, contract)| usize::from(contract.granted.saturating_sub(contract.inflight)))
            .sum()
    }

    pub(crate) fn new(capacity: usize) -> Arc<Self> {
        Arc::new(Self {
            capacity,
            state: Mutex::new(State {
                contracts: BTreeMap::new(),
                sequence: 0,
                readers: BTreeMap::new(),
            }),
        })
    }

    /// Register one read window of `version`. At capacity the read is refused.
    fn open(self: &Arc<Self>, version: ObjectVersion) -> Result<WindowLease> {
        let mut state = self.state.lock().unwrap();
        if state.readers.len() >= self.capacity {
            return Err(Error::Overloaded);
        }
        state.sequence = state.sequence.checked_add(1).ok_or(Error::Overloaded)?;
        let id = state.sequence;
        state.readers.insert(
            id,
            Reader {
                version,
                wants: BTreeSet::new(),
                busy: BTreeSet::new(),
                offered: VecDeque::new(),
                waker: None,
            },
        );
        Ok(WindowLease {
            scheduler: self.clone(),
            id,
        })
    }

    /// Give a selected page to every open window of its version that wants it.
    fn offer(&self, version: &ObjectVersion, page: &PageResult) {
        let number = page.plaintext.page().number.0;
        let wake = {
            let mut state = self.state.lock().unwrap();
            state
                .readers
                .values_mut()
                .filter_map(|reader| {
                    if &reader.version != version || !reader.wants.remove(&number) {
                        return None;
                    }
                    reader.busy.remove(&number);
                    reader.offered.push_back(page.clone());
                    reader.waker.take()
                })
                .collect::<Vec<_>>()
        };
        for waker in wake {
            waker.wake();
        }
    }

    /// Release the flight a dropped selection held on its contract.
    fn release(&self, key: &ContractKey) {
        if let Some(contract) = self.state.lock().unwrap().contracts.get_mut(key) {
            contract.inflight = contract.inflight.saturating_sub(1);
        }
    }
}

/// Registration of one stream's window. Dropping it stops fan-out to the stream.
struct WindowLease {
    scheduler: Arc<Scheduler>,
    id: u64,
}

impl WindowLease {
    /// Publish the pages this window still wants and those it is fetching.
    fn sync(&self, wants: BTreeSet<u64>, busy: BTreeSet<u64>, waker: &Waker) {
        let mut state = self.scheduler.state.lock().unwrap();
        if let Some(reader) = state.readers.get_mut(&self.id) {
            reader.wants = wants;
            reader.busy = busy;
            reader.waker = Some(waker.clone());
        }
    }

    /// Selected pages offered since the last call.
    fn take_offered(&self) -> VecDeque<PageResult> {
        let mut state = self.scheduler.state.lock().unwrap();
        state
            .readers
            .get_mut(&self.id)
            .map(|reader| std::mem::take(&mut reader.offered))
            .unwrap_or_default()
    }

    /// Give a directly filled page to other windows of the version.
    fn offer(&self, page: &PageResult) {
        self.scheduler.offer(&page.metadata.version, page);
    }

    /// Pages that other windows of the same version are already selecting or filling directly.
    fn busy_elsewhere(&self) -> BTreeSet<u64> {
        let state = self.scheduler.state.lock().unwrap();
        let Some(version) = state.readers.get(&self.id).map(|r| &r.version) else {
            return BTreeSet::new();
        };
        state
            .readers
            .iter()
            .filter(|(id, reader)| **id != self.id && &reader.version == version)
            .flat_map(|(_, reader)| reader.busy.iter().copied())
            .collect()
    }
}

impl Drop for WindowLease {
    fn drop(&mut self) {
        self.scheduler
            .state
            .lock()
            .unwrap()
            .readers
            .remove(&self.id);
    }
}

/// One provider selection over some window pages.
pub(crate) struct Selection {
    pub(crate) scheduler: Arc<Scheduler>,
    pub(crate) version: ObjectVersion,
    pub(crate) demand: racer_core::subscriptions::Demand,
    /// The contract this selection holds a flight on, once issued.
    contract: Mutex<Option<ContractKey>>,
}

impl Selection {
    pub(crate) fn complete(&self, page: PageResult) -> Result<()> {
        let number = page.plaintext.page().number;
        if page.metadata.version != self.version || !self.demand.contains(number.0) {
            return Err(Error::CorruptRecord);
        }
        page.validate_for(&PageId {
            version: self.version.clone(),
            number,
        })?;
        self.scheduler.offer(&self.version, &page);
        Ok(())
    }

    #[cfg(test)]
    pub(crate) fn for_test(
        scheduler: &Arc<Scheduler>,
        version: ObjectVersion,
        demand: racer_core::subscriptions::Demand,
    ) -> Self {
        Self {
            scheduler: scheduler.clone(),
            version,
            demand,
            contract: Mutex::new(None),
        }
    }
}

impl Drop for Selection {
    fn drop(&mut self) {
        if let Some(key) = self.contract.get_mut().unwrap().take() {
            self.scheduler.release(&key);
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
                budget
                    .partition(attempts, links)
                    .map(Some)
                    .map_err(Into::into)
            }
        }
    }
    fn complete(&mut self, _remaining: AcquisitionBudget) -> Result<()> {
        match self {
            Self::ClientPages { .. } | Self::ProgressingPages { .. } => Ok(()),
            #[cfg(test)]
            Self::Shared(budget) => Ok(budget.reunite(_remaining)?),
        }
    }
}

type Fetch = Operation<'static, (Result<PageResult>, AcquisitionBudget)>;

/// A direct fill in sealed mode, which may return ciphertext for the GPU.
type SealedFetch = Operation<'static, (Result<Fetched>, AcquisitionBudget)>;

/// One page for a direct read: plaintext, or ciphertext the sink verifies.
pub(crate) enum DirectItem {
    Plaintext(ReaderLease),
    /// A sealed page and the slice of it the range covers.
    Sealed(Box<SealedPage>, crate::model::PageSlice),
}

/// One page of the read window, `slots[i]` is page `next + i`.
#[allow(clippy::large_enum_variant)] // Avoid allocating again when an in-window page becomes ready.
enum Slot {
    /// Looking the page up in local memory on its owning worker.
    Probing(Operation<'static, Option<PageResult>>),
    /// Not in memory and not requested. `select` is false once a selection
    /// covering the page failed; it is then only filled directly.
    Missing { select: bool },
    /// A direct fill through the owning worker.
    Fetching(Fetch),
    /// A direct fill in sealed mode.
    FetchingSealed(SealedFetch),
    /// Fetched as ciphertext for the GPU to verify.
    Sealed(Box<SealedPage>),
    /// Covered by an outstanding provider selection.
    Selecting,
    /// Fetched, or failed. An error waits here until the page reaches the head.
    Ready(Result<PageResult>),
}

/// A page fetched ahead of the in-memory window onto local disk.
enum Spilling {
    /// Reserving this read's hold on the page's owning worker.
    Holding(Operation<'static, HoldEpoch>),
    /// Filling the held page through the warm path, which persists it.
    Warming(Operation<'static, ()>),
    /// Published under this read's hold.
    Spilled,
}

/// Read spill state: pages in `[next + window, next + window + limit)` may be
/// parked on disk under per-page holds until the window reaches them.
struct Spill {
    limit: usize,
    /// Set once a hold is refused; the read then keeps only its memory window.
    refused: bool,
    pages: BTreeMap<u64, Spilling>,
    /// Pages this read holds and the hold epoch each was taken under; each owes
    /// exactly one release under that epoch.
    held: BTreeMap<u64, HoldEpoch>,
}

/// One outstanding provider selection and the window pages it covers.
struct Pending {
    operation: Fetch,
    pages: BTreeSet<u64>,
}

/// Open pinned streams on the ingress reactor using the shared worker directory.
pub struct RangeStreams {
    directory: Arc<WorkerDirectory>,

    delivery: Rc<Delivery>,

    window_pages: usize,
}

/// The body stays on the ingress reactor, including its delivery pipe leases.
pub struct RangeStream {
    error: Option<Error>,

    lease: Option<WindowLease>,

    credits: Option<Window<PageNumber>>,

    retained: BTreeMap<PageNumber, crate::memory::VerifiedPage>,

    metadata: ObjectMetadata,

    range: ResolvedRange,

    context: OriginContext,

    membership: Arc<crate::topology::Membership>,

    scope: RequestScope,

    directory: Arc<WorkerDirectory>,

    delivery: Rc<Delivery>,

    window_pages: usize,

    budget: RangeBudget,

    /// Delivery cursor: the next page handed to the client.
    next: u64,

    /// One past the last page of the range.
    end: u64,

    slots: VecDeque<Slot>,

    selections: Vec<Pending>,

    spill: Option<Spill>,

    /// Set for direct GPU reads: misses are fetched sealed for this GPU.
    sealed: Option<SealedTarget>,

    terminated: bool,
}

impl RangeStreams {
    /// Bind stream creation to this worker's delivery resources and read window.
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

    /// The configured `range_window_pages` of each stream.
    pub(crate) fn window_pages(&self) -> usize {
        self.window_pages
    }

    /// Share the same metadata and page owners used by newly opened streams.
    pub(crate) fn directory(&self) -> &Arc<WorkerDirectory> {
        &self.directory
    }

    /// Open a client range with a bounded allowance for each distinct page and
    /// the original request deadline.
    pub fn open(
        &self,
        metadata: ObjectMetadata,
        range: ResolvedRange,
        context: OriginContext,
        membership: Arc<crate::topology::Membership>,
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
        Ok(RangeStream {
            error: None,
            lease: None,
            credits: None,
            retained: BTreeMap::new(),
            metadata,
            next: range.first_page().0,
            end: range.last_page().0 + 1,
            range,
            context,
            membership,
            scope,
            directory: self.directory.clone(),
            delivery: self.delivery.clone(),
            window_pages: self.window_pages,
            budget,
            slots: VecDeque::new(),
            selections: Vec::new(),
            spill: None,
            sealed: None,
            terminated: false,
        })
    }
}

impl RangeStream {
    /// Return the immutable range admitted when this stream was opened.
    pub(crate) fn range(&self) -> ResolvedRange {
        self.range
    }

    /// Install exact page/byte release credits once, before delivery. The fetch window is the
    /// configured `range_window_pages`, further limited by the client's unreleased credits.
    pub fn configure_subscription(&mut self, pages: usize, bytes: u64) -> Result<()> {
        if self.lease.is_some() {
            return Err(Error::InvalidRequest);
        }
        if !(1..=64).contains(&pages) || !(PAGE_BYTES..=64 * PAGE_BYTES).contains(&bytes) {
            return Err(Error::InvalidRequest);
        }
        let credits = Window::new(pages, bytes, PAGE_BYTES).map_err(window_error)?;
        let lease = self
            .directory
            .subscriptions
            .open(self.metadata.version.clone())?;
        self.credits = Some(credits);
        self.lease = Some(lease);
        let limit = self.directory.warm.config().range_spill_pages;
        self.spill = (limit != 0).then(|| Spill {
            limit,
            refused: false,
            pages: BTreeMap::new(),
            held: BTreeMap::new(),
        });
        Ok(())
    }

    /// Fetch pages that are not verified plaintext in memory as sealed pages
    /// for `target`'s GPU. Call before the first delivery.
    pub(crate) fn set_sealed(&mut self, target: SealedTarget) -> Result<()> {
        if self.terminated || !self.slots.is_empty() {
            return Err(Error::InvalidRequest);
        }
        self.sealed = Some(target);
        Ok(())
    }

    /// Tell a sealed page's owner what the GPU decided.
    pub(crate) fn report_sealed(&self, report: SealedReport, passed: bool) {
        self.directory.report_sealed(report, passed, &self.scope);
    }

    /// Release exactly one delivered page and its retained plaintext charge.
    pub fn release_page(&mut self, number: PageNumber, length: u32) -> Result<()> {
        self.credits
            .as_mut()
            .ok_or(Error::InvalidRequest)?
            .release(number, u64::from(length))
            .map_err(window_error)?;
        self.retained.remove(&number);
        Ok(())
    }

    /// Drive the fetch window while the current slice encounters client backpressure. Completed
    /// pages leave their acquisition scopes promptly.
    pub(crate) fn poll_prefetch(&mut self, cx: &mut Context<'_>) {
        if self.terminated || self.error.is_some() || self.lease.is_none() {
            return;
        }
        if let Err(error) = self.step(cx) {
            self.error = Some(error);
        }
    }

    /// One step of the fetch job: collect completions and offers, then admit
    /// new probes, direct fills and selections until nothing changes.
    fn step(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let scope = self.operation_scope();
        scope.check()?;
        loop {
            self.refill(&scope);
            let mut changed = self.poll_slots(cx)?;
            changed |= self.take_offered(&scope);
            changed |= self.poll_selections(cx)?;
            changed |= self.admit(&scope)?;
            changed |= self.poll_spill(cx);
            changed |= self.admit_spill(&scope);
            if !changed {
                break;
            }
        }
        self.sync(cx);
        Ok(())
    }

    /// Keep the window full: each page entering it takes its exact client
    /// credit, then probes memory. Window pages plus delivered, unreleased pages
    /// never exceed the client's credits, so fetching ahead pins no more
    /// plaintext than the client may hold.
    fn refill(&mut self, scope: &RequestScope) {
        while self.slots.len() < self.window_pages && self.page_at(self.slots.len()) < self.end {
            let number = PageNumber(self.page_at(self.slots.len()));
            if !self.reserve(number) {
                break;
            }
            let page = PageId {
                version: self.metadata.version.clone(),
                number,
            };
            let directory = self.directory.clone();
            let scope = scope.clone();
            let sealed = self.sealed.is_some();
            let spilled = self
                .spill
                .as_mut()
                .and_then(|spill| spill.pages.remove(&number.0));
            self.slots.push_back(Slot::Probing(match spilled {
                // A sealed read probes live plaintext only. Cached or disk ciphertext is then
                // fetched sealed, so the GPU rather than the CPU decrypts it.
                None if sealed => {
                    Box::pin(async move { directory.cached_page(page, &scope).await })
                }
                // Holding pages stay with the spill state until the hold
                // completes, so the hold is released exactly once.
                None => Box::pin(async move { directory.local_page(page, &scope).await }),
                Some(Spilling::Holding(holding)) => {
                    self.spill
                        .as_mut()
                        .unwrap()
                        .pages
                        .insert(number.0, Spilling::Holding(holding));
                    if sealed {
                        Box::pin(async move { directory.cached_page(page, &scope).await })
                    } else {
                        Box::pin(async move { directory.local_page(page, &scope).await })
                    }
                }
                // A sealed read probes memory only: a spilled page on disk is
                // then fetched sealed, so the GPU decrypts it.
                Some(Spilling::Spilled) if sealed => {
                    Box::pin(async move { directory.cached_page(page, &scope).await })
                }
                Some(Spilling::Warming(warming)) if sealed => Box::pin(async move {
                    let _ = warming.await;
                    directory.cached_page(page, &scope).await
                }),
                // A spilled page is read back from memory or its held disk copy.
                // If that fails the page is fetched normally.
                Some(Spilling::Spilled) => {
                    Box::pin(async move { directory.held_page(page, &scope).await.map(Some) })
                }
                Some(Spilling::Warming(warming)) => Box::pin(async move {
                    warming.await?;
                    directory.held_page(page, &scope).await.map(Some)
                }),
            }));
        }
    }

    /// Reserve the client credit of one window page, if it fits.
    fn reserve(&mut self, number: PageNumber) -> bool {
        let Some(length) = self
            .range
            .slice_at(number)
            .ok()
            .flatten()
            .map(|slice| u64::from(slice.length))
        else {
            return false;
        };
        self.credits.as_mut().is_some_and(|credits| {
            credits.can_reserve(length) && credits.reserve(number, length).is_ok()
        })
    }

    /// Place a page in the window with its credit, as `refill` would.
    #[cfg(test)]
    fn inject(&mut self, slot: Slot) {
        assert!(self.reserve(PageNumber(self.page_at(self.slots.len()))));
        self.slots.push_back(slot);
    }

    fn page_at(&self, index: usize) -> u64 {
        self.next + index as u64
    }

    fn slot_mut(&mut self, number: u64) -> Option<&mut Slot> {
        let index = usize::try_from(number.checked_sub(self.next)?).ok()?;
        self.slots.get_mut(index)
    }

    fn poll_slots(&mut self, cx: &mut Context<'_>) -> Result<bool> {
        let mut changed = false;
        for index in 0..self.slots.len() {
            let number = PageNumber(self.page_at(index));
            let next = match &mut self.slots[index] {
                Slot::Probing(probe) => match probe.as_mut().poll(cx) {
                    Poll::Pending => continue,
                    Poll::Ready(Ok(Some(page))) => {
                        if self.validate(&page, number).is_ok() {
                            Slot::Ready(Ok(page))
                        } else {
                            Slot::Missing { select: true }
                        }
                    }
                    // A miss or a failed probe is fetched normally. Stream
                    // cancellation and deadlines are checked by `step` itself.
                    Poll::Ready(_) => Slot::Missing { select: true },
                },
                Slot::Fetching(fetch) => match fetch.as_mut().poll(cx) {
                    Poll::Pending => continue,
                    Poll::Ready(Ok((result, remaining))) => {
                        if let (Ok(page), Some(lease)) = (&result, &self.lease) {
                            lease.offer(page);
                        }
                        Slot::Ready(self.budget.complete(remaining).and(result))
                    }
                    Poll::Ready(Err(error)) => Slot::Ready(Err(error)),
                },
                Slot::FetchingSealed(fetch) => match fetch.as_mut().poll(cx) {
                    Poll::Pending => continue,
                    Poll::Ready(Ok((result, remaining))) => {
                        let completed = self.budget.complete(remaining);
                        match result {
                            Ok(Fetched::Page(page)) => {
                                if let Some(lease) = &self.lease {
                                    lease.offer(&page);
                                }
                                Slot::Ready(completed.map(|()| page))
                            }
                            Ok(Fetched::Sealed(sealed)) => match completed
                                .and_then(|()| self.validate_sealed(&sealed, number))
                            {
                                Ok(()) => Slot::Sealed(sealed),
                                Err(error) => Slot::Ready(Err(error)),
                            },
                            Err(error) => Slot::Ready(completed.and(Err(error))),
                        }
                    }
                    Poll::Ready(Err(error)) => Slot::Ready(Err(error)),
                },
                _ => continue,
            };
            self.slots[index] = next;
            changed = true;
        }
        Ok(changed)
    }

    /// Accept pages that any selection of this version offered to this window.
    fn take_offered(&mut self, scope: &RequestScope) -> bool {
        let Some(lease) = self.lease.as_ref() else {
            return false;
        };
        let mut changed = false;
        for page in lease.take_offered() {
            let number = page.plaintext.page().number;
            if self.validate(&page, number).is_err() {
                continue;
            }
            let Some(slot) = self.slot_mut(number.0) else {
                continue;
            };
            if !matches!(
                slot,
                Slot::Probing(_) | Slot::Missing { .. } | Slot::Selecting
            ) {
                continue;
            }
            *slot = Slot::Ready(Ok(page.clone()));
            // Selected pages are accepted unobserved on their owner; record
            // this reader's interest once it takes the page.
            self.directory.observe_verified(page, scope);
            changed = true;
        }
        changed
    }

    fn poll_selections(&mut self, cx: &mut Context<'_>) -> Result<bool> {
        let mut changed = false;
        let mut index = 0;
        while index < self.selections.len() {
            let Poll::Ready(completion) = self.selections[index].operation.as_mut().poll(cx) else {
                index += 1;
                continue;
            };
            let pending = self.selections.swap_remove(index);
            let result = match completion {
                Ok((result, remaining)) => {
                    self.budget.complete(remaining)?;
                    result
                }
                Err(error) => Err(error),
            };
            // The selected page was already offered.
            let select = result.is_ok();
            for number in pending.pages {
                if let Some(slot) = self.slot_mut(number)
                    && matches!(slot, Slot::Selecting)
                {
                    *slot = Slot::Missing { select };
                }
            }
            changed = true;
        }
        Ok(changed)
    }

    fn pending(&self) -> bool {
        !self.selections.is_empty()
            || self
                .slots
                .iter()
                .any(|slot| matches!(slot, Slot::Fetching(_) | Slot::FetchingSealed(_)))
    }

    /// Start a direct fill for the head page, up to `direct_ahead` more holes,
    /// and provider selections for the remaining holes.
    fn admit(&mut self, scope: &RequestScope) -> Result<bool> {
        // A failed page stops admission at and after it until it reaches the
        // client. Pages before it are still filled, or the stream could never
        // deliver them and so never surface the error.
        let kinds = self.kinds();
        let failed = read::first_failure(&kinds);
        // Pages another window is selecting or filling arrive through its
        // fan-out. Only the head is always filled directly, so it never waits
        // on another window; a concurrent fill of the same page joins the
        // owner's flight instead of transferring it again.
        let elsewhere = self
            .lease
            .as_ref()
            .map(WindowLease::busy_elsewhere)
            .unwrap_or_default();
        let plan = read::direct_fills(&kinds, self.direct_ahead(), |index| {
            elsewhere.contains(&self.page_at(index))
        });
        let mut changed = false;
        for index in plan {
            if !self.start_fetch(index, scope)? {
                return Ok(changed);
            }
            changed = true;
        }
        // Earlier holes behind a failure are filled directly: each becomes the
        // head in turn, so no new selection is needed to reach the error.
        while failed.is_none() && self.may_select() && self.start_selection(scope)? {
            changed = true;
        }
        Ok(changed)
    }

    /// Direct fills to keep in flight beyond the head page. Provider selections deliver verified
    /// plaintext, so a sealed read fills its whole window directly.
    fn direct_ahead(&self) -> usize {
        if self.sealed.is_some() {
            self.window_pages.saturating_sub(1).max(DIRECT_AHEAD)
        } else {
            DIRECT_AHEAD
        }
    }

    /// Returns false when the range budget admits no more work for now.
    fn start_fetch(&mut self, index: usize, scope: &RequestScope) -> Result<bool> {
        let Some(child) = self.budget.next_page(self.pending())? else {
            return Ok(false);
        };
        let mut child_scope = scope.clone();
        child_scope.deadline.0 = child.deadline();
        let page = PageId {
            version: self.metadata.version.clone(),
            number: PageNumber(self.page_at(index)),
        };
        let fetch = match &self.sealed {
            Some(target) => self
                .directory
                .start_page_sealed(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &child_scope,
                    child,
                    target.clone(),
                )
                .map(Slot::FetchingSealed),
            None => self
                .directory
                .start_page(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &child_scope,
                    child,
                )
                .map(Slot::Fetching),
        };
        self.slots[index] = fetch.unwrap_or_else(|error| Slot::Ready(Err(error)));
        Ok(true)
    }

    /// Whether the read may start another selection: its first always, more
    /// only while a contract of the version has an unused granted flight.
    fn may_select(&self) -> bool {
        read::may_select(
            self.selections.len(),
            self.directory.subscriptions.spare(&self.metadata.version),
        )
    }

    /// Scheduling state of each window slot, for the core range planner.
    fn kinds(&self) -> Vec<SlotKind> {
        self.slots
            .iter()
            .map(|slot| match slot {
                Slot::Probing(_) => SlotKind::Probing,
                Slot::Missing { select } => SlotKind::Missing { select: *select },
                Slot::Fetching(_) | Slot::FetchingSealed(_) => SlotKind::Fetching,
                Slot::Selecting => SlotKind::Selecting,
                Slot::Ready(Err(_)) => SlotKind::Failed,
                Slot::Ready(Ok(_)) | Slot::Sealed(_) => SlotKind::Ready,
            })
            .collect()
    }

    /// The demand and pages of the next selection, if any. Pages that another window of this
    /// version is selecting are left to that selection's fan-out.
    fn plan_selection(&self) -> Result<Option<(racer_core::subscriptions::Demand, BTreeSet<u64>)>> {
        if self.directory.warm.contains(&self.metadata.version) {
            return Ok(None);
        }
        let Some(lease) = self.lease.as_ref() else {
            return Ok(None);
        };
        self.selection_demand(&lease.busy_elsewhere(), self.share())
    }

    /// Select the next share of the remaining holes from a peer provider. Pages that another window
    /// of this version is selecting are left to that selection's fan-out.
    fn start_selection(&mut self, scope: &RequestScope) -> Result<bool> {
        let Some((demand, pages)) = self.plan_selection()? else {
            return Ok(false);
        };
        let Some(child) = self.budget.next_page(self.pending())? else {
            return Ok(false);
        };
        let selection = Selection {
            scheduler: self.directory.subscriptions.clone(),
            version: self.metadata.version.clone(),
            demand,
            contract: Mutex::new(None),
        };
        let mut child_scope = scope.clone();
        child_scope.deadline.0 = child.deadline();
        let operation = match self.directory.start_selection(
            self.metadata.version.clone(),
            selection,
            self.membership.clone(),
            &self.context,
            &child_scope,
            child,
        ) {
            Ok(operation) => operation,
            Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => return Err(error),
            // A selection that cannot start leaves its pages to direct fills.
            Err(_) => {
                for number in &pages {
                    if let Some(slot) = self.slot_mut(*number) {
                        *slot = Slot::Missing { select: false };
                    }
                }
                return Ok(true);
            }
        };
        for number in &pages {
            if let Some(slot) = self.slot_mut(*number) {
                *slot = Slot::Selecting;
            }
        }
        self.selections.push(Pending { operation, pages });
        Ok(true)
    }

    /// Pages the next selection may demand: an even share of the selectable holes over the
    /// selections the read can start now (its free selection slots, bounded by the version's spare
    /// granted flights), so concurrent selections cover disjoint parts of the window.
    fn share(&self) -> usize {
        let holes = self
            .slots
            .iter()
            .filter(|slot| matches!(slot, Slot::Missing { select: true }))
            .count();
        let spare = self.directory.subscriptions.spare(&self.metadata.version);
        read::share(holes, self.selections.len(), spare)
    }

    /// Up to `limit` selectable holes of the window, excluding pages that another window is
    /// selecting or filling, as a canonical demand of at most the wire interval limit.
    fn selection_demand(
        &self,
        elsewhere: &BTreeSet<u64>,
        limit: usize,
    ) -> Result<Option<(racer_core::subscriptions::Demand, BTreeSet<u64>)>> {
        use flow_control::subscriptions::Interval;
        use racer_core::subscriptions::{Demand, MAX_DEMAND_INTERVALS};
        let Some((intervals, pages)) = read::selection_demand(
            &self.kinds(),
            self.next,
            |number| elsewhere.contains(&number),
            limit,
            MAX_DEMAND_INTERVALS,
        ) else {
            return Ok(None);
        };
        let intervals = intervals
            .into_iter()
            .map(|r| Interval {
                start: r.start,
                end: r.end,
            })
            .collect();
        Ok(Some((
            Demand::new(intervals).ok_or(Error::InvalidRequest)?,
            pages,
        )))
    }

    /// Publish this window's wanted and in-flight pages for fan-out.
    fn sync(&self, cx: &Context<'_>) {
        let Some(lease) = self.lease.as_ref() else {
            return;
        };
        let mut wants = BTreeSet::new();
        let mut busy = BTreeSet::new();
        for (index, slot) in self.slots.iter().enumerate() {
            let number = self.page_at(index);
            match slot {
                Slot::Selecting => {
                    wants.insert(number);
                    busy.insert(number);
                }
                Slot::Fetching(_) | Slot::FetchingSealed(_) => {
                    busy.insert(number);
                }
                Slot::Probing(_) | Slot::Missing { .. } => {
                    wants.insert(number);
                }
                _ => {}
            }
        }
        lease.sync(wants, busy, cx.waker());
    }

    /// Return every spill hold this read still owns.
    fn release_spill(&mut self) {
        let Some(spill) = self.spill.take() else {
            return;
        };
        // A pending hold may still be applied; its completion fence says so.
        for (number, state) in spill.pages {
            let Spilling::Holding(holding) = state else {
                continue;
            };
            let Ok(permit) = uring_runtime::drivers::reserve() else {
                continue;
            };
            let directory = self.directory.clone();
            let version = self.metadata.version.clone();
            let request = self.scope.request;
            permit.submit_detached(Box::pin(async move {
                if let Ok(epoch) = holding.await {
                    directory.release_read_pages(&version, [(PageNumber(number), epoch)], request);
                }
                Ok::<(), Error>(())
            }));
        }
        if !spill.held.is_empty() {
            self.directory.release_read_pages(
                &self.metadata.version,
                spill
                    .held
                    .into_iter()
                    .map(|(number, epoch)| (PageNumber(number), epoch)),
                self.scope.request,
            );
        }
    }

    fn poll_spill(&mut self, cx: &mut Context<'_>) -> bool {
        let horizon = self.page_at(self.slots.len());
        let Some(spill) = self.spill.as_mut() else {
            return false;
        };
        let mut changed = false;
        let mut release = Vec::new();
        let mut warm = Vec::new();
        spill.pages.retain(|number, state| {
            match state {
                Spilling::Holding(op) => {
                    let Poll::Ready(result) = op.as_mut().poll(cx) else {
                        return true;
                    };
                    changed = true;
                    let Ok(epoch) = result else {
                        // Hold limits, a missing key or no disk: keep the memory window.
                        spill.refused = true;
                        return false;
                    };
                    // The window reached the page while its hold was pending.
                    if *number < horizon {
                        release.push((PageNumber(*number), epoch));
                        return false;
                    }
                    spill.held.insert(*number, epoch);
                    warm.push(*number);
                    true
                }
                Spilling::Warming(op) => {
                    let Poll::Ready(result) = op.as_mut().poll(cx) else {
                        return true;
                    };
                    changed = true;
                    if result.is_ok() {
                        *state = Spilling::Spilled;
                        return true;
                    }
                    if let Some(epoch) = spill.held.remove(number) {
                        release.push((PageNumber(*number), epoch));
                    }
                    // A failed spill fill (origin refusal, version change, no disk) would likely
                    // fail again, and each retry takes a fresh warm budget.
                    spill.refused = true;
                    false
                }
                Spilling::Spilled => true,
            }
        });
        for number in warm {
            let operation = self.start_spill_fill(PageNumber(number));
            if let Some(spill) = self.spill.as_mut() {
                spill.pages.insert(number, Spilling::Warming(operation));
            }
        }
        if !release.is_empty() {
            self.directory
                .release_read_pages(&self.metadata.version, release, self.scope.request);
        }
        changed
    }

    fn start_spill_fill(&self, number: PageNumber) -> Operation<'static, ()> {
        let page = PageId {
            version: self.metadata.version.clone(),
            number,
        };
        match self.directory.start_warm_page(
            page,
            self.metadata.immutable(),
            self.membership.clone(),
            &self.context,
            &self.operation_scope(),
        ) {
            Ok(operation) => operation,
            Err(error) => Box::pin(async move { Err(error) }),
        }
    }

    /// Start holds for pages past the window, while no warm job covers the
    /// version (its own holds already cover those pages).
    fn admit_spill(&mut self, scope: &RequestScope) -> bool {
        let start = self.page_at(self.slots.len());
        let warming = self.directory.warm.contains(&self.metadata.version);
        let Some(spill) = self.spill.as_mut() else {
            return false;
        };
        if spill.refused || warming {
            return false;
        }
        let remaining = scope
            .deadline
            .0
            .saturating_duration_since(uring_runtime::environment::now());
        let Some(until) =
            uring_runtime::environment::wall_now().checked_add(SPILL_HOLD.min(remaining))
        else {
            return false;
        };
        let mut active = spill
            .pages
            .values()
            .filter(|state| !matches!(state, Spilling::Spilled))
            .count();
        let end = start.saturating_add(spill.limit as u64).min(self.end);
        let mut changed = false;
        for number in start..end {
            if active >= SPILL_FILLS {
                break;
            }
            if spill.pages.contains_key(&number) {
                continue;
            }
            let directory = self.directory.clone();
            let page = PageId {
                version: self.metadata.version.clone(),
                number: PageNumber(number),
            };
            let scope = scope.clone();
            spill.pages.insert(
                number,
                Spilling::Holding(Box::pin(async move {
                    directory.hold_read_page(&page, until, &scope).await
                })),
            );
            active += 1;
            changed = true;
        }
        changed
    }

    /// Only client HTTP delivery may release the initial operation deadline after acquiring the
    /// first slice.
    pub(crate) fn enable_progress(&mut self, timeout: Duration) {
        if self.directory.warm.contains(&self.metadata.version) {
            return;
        }
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
    fn validate_sealed(&self, sealed: &SealedPage, number: PageNumber) -> Result<()> {
        validate_pin(&self.metadata, &sealed.metadata)?;
        sealed.validate_for(&PageId {
            version: self.metadata.version.clone(),
            number,
        })
    }
    fn validate(&self, result: &PageResult, number: PageNumber) -> Result<()> {
        validate_pin(&self.metadata, &result.metadata)?;
        result.validate_for(&PageId {
            version: self.metadata.version.clone(),
            number,
        })
    }
    /// Deliver the next page in order. Pending futures live in the stream, so
    /// dropping next_slice cannot restart a page or refill its retries.
    pub fn next_slice(&mut self) -> Operation<'_, Option<ReaderLease>> {
        Box::pin(async move {
            match self.next_direct().await? {
                None => Ok(None),
                Some(DirectItem::Plaintext(reader)) => Ok(Some(reader)),
                // Only direct GPU reads set a sealed target.
                Some(DirectItem::Sealed(..)) => {
                    self.terminate();
                    Err(Error::Internal)
                }
            }
        })
    }

    /// Deliver the next page in order for a direct read, sealed if the
    /// stream has a sealed target and the page was fetched as ciphertext.
    pub(crate) fn next_direct(&mut self) -> Operation<'_, Option<DirectItem>> {
        Box::pin(async move {
            if self.terminated {
                return Ok(None);
            }
            if let Some(error) = self.error.take() {
                self.terminate();
                return Err(error);
            }
            if self.lease.is_none() {
                self.terminate();
                return Err(Error::InvalidRequest);
            }
            let result = self.deliver().await;
            if result.is_err() {
                self.terminate();
            }
            result
        })
    }
    pub fn cancel(&mut self) -> Operation<'_, ()> {
        Box::pin(async move {
            self.terminate();
            self.scope.cancel()
        })
    }
    fn terminate(&mut self) {
        self.release_spill();
        self.terminated = true;
        self.slots.clear();
        self.selections.clear();
        self.retained.clear();
        self.lease = None;
        self.credits = None;
    }

    /// Whether the head page can be handed to the client now.
    fn deliverable(&self) -> bool {
        matches!(self.slots.front(), Some(Slot::Ready(_) | Slot::Sealed(_)))
    }

    async fn deliver(&mut self) -> Result<Option<DirectItem>> {
        let scope = self.operation_scope();
        let cancellation = scope.cancellation.subscribe()?;
        poll_fn(|cx| -> Poll<Result<()>> {
            cancellation.register(cx.waker());
            self.poll_prefetch(cx);
            if let Some(error) = self.error.take() {
                return Poll::Ready(Err(error));
            }
            if self.next == self.end || self.deliverable() {
                Poll::Ready(Ok(()))
            } else {
                Poll::Pending
            }
        })
        .await?;
        if self.next == self.end {
            self.terminated = true;
            return Ok(None);
        }
        scope.check()?;
        let number = PageNumber(self.next);
        let result = match self.slots.pop_front() {
            Some(Slot::Ready(result)) => {
                self.next += 1;
                result?
            }
            Some(Slot::Sealed(sealed)) => {
                self.next += 1;
                self.validate_sealed(&sealed, number)?;
                let slice = self.range.slice_at(number)?.ok_or(Error::CorruptRecord)?;
                self.issue(number)?;
                return Ok(Some(DirectItem::Sealed(sealed, slice)));
            }
            _ => return Err(Error::StaleFlight),
        };
        self.validate(&result, number)?;
        let slice = self.range.slice_at(number)?.ok_or(Error::CorruptRecord)?;
        self.issue(number)?;
        self.retained.insert(number, result.plaintext.clone());
        self.delivery
            .attach(result.plaintext, slice)
            .map(|reader| Some(DirectItem::Plaintext(reader)))
    }

    /// Account one page as handed to the client, and drop its spill hold.
    fn issue(&mut self, number: PageNumber) -> Result<()> {
        self.credits
            .as_mut()
            .ok_or(Error::InvalidRequest)?
            .issued(number)
            .map_err(window_error)?;
        if let Some(spill) = self.spill.as_mut()
            && let Some(epoch) = spill.held.remove(&number.0)
        {
            self.directory.release_read_pages(
                &self.metadata.version,
                [(number, epoch)],
                self.scope.request,
            );
        }
        Ok(())
    }

    /// Pages admitted into the window: fetched, being fetched or selected.
    pub fn buffered_pages(&self) -> usize {
        self.slots
            .iter()
            .filter(|slot| {
                matches!(
                    slot,
                    Slot::Fetching(_)
                        | Slot::FetchingSealed(_)
                        | Slot::Selecting
                        | Slot::Ready(_)
                        | Slot::Sealed(_)
                )
            })
            .count()
    }
}

impl Drop for RangeStream {
    fn drop(&mut self) {
        self.release_spill();
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
        pipes: Rc<flow_control::PipePool<AdmissionPolicy>>,
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
                Rc::new(Delivery::new(reactor, Duration::from_secs(30))),
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
                    storage: flow_control::ChargedBytes {
                        bytes: vec![number as u8; length],
                        reservation: admission
                            .reserve(cache, ResourceClass::Plaintext, length)
                            .unwrap(),
                    },
                }),
            },
            ciphertext: CiphertextPage {
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: Default::default(),
                    envelope: PageEnvelope {
                        cipher: crate::model::PageCipher::Xaes256Gcm,
                        page,
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: length as u32 + 16,
                    },
                    storage: flow_control::ChargedBytes {
                        bytes: vec![0; length + 16],
                        reservation: admission
                            .reserve(cache, ResourceClass::Ciphertext, length + 16)
                            .unwrap(),
                    }
                    .into(),
                }),
            },
        }
    }

    #[test]
    fn sealed_reads_fill_their_whole_window_directly() {
        for (window, sealed) in [(1, 1), (2, 1), (8, 7)] {
            let f = Fixture::new(crate::test_support::cluster::config(false).limits, window);
            let mut metadata = metadata();
            metadata.length = 16 * PAGE_BYTES;
            let range = ByteRange::From(0).resolve(metadata.length).unwrap();
            let scope =
                RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = f.open(&metadata, range, scope);
            assert_eq!(stream.direct_ahead(), DIRECT_AHEAD);
            stream
                .set_sealed(SealedTarget {
                    helper: crate::gpu::testing::helper(&[None]),
                    ordinal: 0,
                })
                .unwrap();
            assert_eq!(stream.direct_ahead(), sealed, "window {window}");
        }
    }

    #[test]
    fn sealed_pages_are_delivered_in_order_only_to_direct_readers() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let admission = &f.admission;
        let mut metadata = metadata();
        metadata.length = 2 * PAGE_BYTES;
        let range = ByteRange::Closed {
            first: 5,
            last: PAGE_BYTES + 9,
        }
        .resolve(metadata.length)
        .unwrap();
        let scope = || {
            RequestScope::new(RequestId([9; 16]), Instant::now() + Duration::from_secs(60)).unwrap()
        };
        let mut stream = f.open(&metadata, range, scope());
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        let first = crate::read::sealed::tests::sealed(&page_result(admission, &metadata, 0));
        stream.inject(Slot::Sealed(Box::new(first)));
        stream.inject(Slot::Ready(Ok(page_result(admission, &metadata, 1))));
        match futures::executor::block_on(stream.next_direct()).unwrap() {
            Some(DirectItem::Sealed(page, slice)) => {
                assert_eq!(page.page().number, PageNumber(0));
                assert_eq!(
                    (slice.page, slice.offset, slice.length),
                    (PageNumber(0), 5, PAGE_BYTES as u32 - 5)
                );
            }
            _ => panic!("expected a sealed page"),
        }
        stream
            .release_page(PageNumber(0), PAGE_BYTES as u32 - 5)
            .unwrap();
        match futures::executor::block_on(stream.next_direct()).unwrap() {
            Some(DirectItem::Plaintext(reader)) => {
                assert_eq!(reader.slice().page, PageNumber(1));
                assert_eq!(reader.slice().length, 10);
            }
            _ => panic!("expected plaintext"),
        }
        assert!(
            futures::executor::block_on(stream.next_direct())
                .unwrap()
                .is_none()
        );

        // A sealed page for another page number fails that page.
        let mut stream = f.open(&metadata, range, scope());
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        let other = crate::read::sealed::tests::sealed(&page_result(admission, &metadata, 1));
        stream.inject(Slot::Sealed(Box::new(other)));
        assert!(futures::executor::block_on(stream.next_direct()).is_err());

        // A Get stream never yields sealed pages.
        let mut stream = f.open(&metadata, range, scope());
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        let first = crate::read::sealed::tests::sealed(&page_result(admission, &metadata, 0));
        stream.inject(Slot::Sealed(Box::new(first)));
        assert_eq!(
            futures::executor::block_on(stream.next_slice()).err(),
            Some(Error::Internal)
        );
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
        stream.configure_subscription(4, PAGE_BYTES).unwrap();
        for number in 0..2 {
            stream.inject(Slot::Ready(Ok(page_result(admission, &metadata, number))));
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
    fn optional_remote_observation_never_blocks_ready_delivery_or_duplicates_on_repoll() {
        for repoll in [false, true] {
            let queue = Rc::new(uring_runtime::drivers::DriverQueue::new(8));
            let _queue = queue.enter();
            let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
            let directory = f.streams.directory();
            assert_eq!(directory.test_observation_mailbox(), 0);
            let mut metadata = metadata();
            metadata.length = 3;
            let scope = RequestScope::new(
                RequestId([12; 16]),
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(3).unwrap(),
                scope.clone(),
            );
            stream.configure_subscription(1, PAGE_BYTES).unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            stream.inject(Slot::Selecting);
            stream.sync(&cx);
            let demand = racer_core::subscriptions::Demand::new(vec![
                flow_control::subscriptions::Interval { start: 0, end: 1 },
            ])
            .unwrap();
            Selection::for_test(&directory.subscriptions, metadata.version.clone(), demand)
                .complete(page_result(&f.admission, &metadata, 0))
                .unwrap();
            if repoll {
                stream.poll_prefetch(&mut cx);
                stream.poll_prefetch(&mut cx);
                assert!(matches!(stream.slots.front(), Some(Slot::Ready(Ok(_)))));
            }
            // No endpoint is ever polled. A continuous next_slice future must
            // still deliver immediately with the accepted hint unanswered.
            let mut next = stream.next_slice();
            let reader = match next.as_mut().poll(&mut cx) {
                Poll::Ready(Ok(Some(reader))) => reader,
                _ => panic!("optional owner receipt blocked ready page"),
            };
            assert_eq!(reader.slice().length, 3);
            drop(next);
            assert_eq!(directory.test_observation_mailbox(), 1);
            assert_eq!(queue.pending(), 1);
            scope.cancel().unwrap();
            queue.poll(&mut cx, 8);
            assert_eq!(queue.pending(), 0);
            assert_eq!(reader.slice().length, 3);
            drop(reader);
            directory.simulation_crash();
        }
    }

    #[test]
    fn later_page_pipe_wait_survives_temporary_polls_and_cleans_up() {
        use crate::test_support::WakeCounter;
        {
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
                stream.configure_subscription(2, PAGE_BYTES).unwrap();
                for number in 0..2 {
                    stream.inject(Slot::Ready(Ok(page_result(admission, &metadata, number))));
                }
                let first = futures::executor::block_on(stream.next_slice())
                    .unwrap()
                    .unwrap();
                assert_eq!(first.slice().page, PageNumber(0));
                drop(first);
                let held = pipes.acquire().unwrap();
                let baseline = admission.used(ResourceClass::RequestContext);
                let wakes = Arc::new(WakeCounter::default());
                let waker = std::task::Waker::from(wakes.clone());
                let mut cx = Context::from_waker(&waker);
                let mut next = stream.next_slice();
                let Poll::Ready(Ok(Some(second))) = next.as_mut().poll(&mut cx) else {
                    panic!("pipe exhaustion must not block page delivery")
                };
                assert_eq!(second.slice().page, PageNumber(1));
                drop(next);
                stream
                    .release_page(PageNumber(1), second.slice().length)
                    .unwrap();
                let waiting = admission.used(ResourceClass::RequestContext);
                assert!(
                    waiting == baseline,
                    "direct delivery needs no FIFO pipe admission"
                );
                stream.release_page(PageNumber(0), 1).unwrap();
                for _ in 0..3 {
                    assert!(matches!(
                        stream.next_slice().as_mut().poll(&mut cx),
                        Poll::Ready(Ok(None))
                    ));
                    assert_eq!(admission.used(ResourceClass::RequestContext), waiting);
                }
                let before = wakes.count();
                match mode {
                    "release" => {
                        drop(held);
                        assert!(
                            wakes.count() == before,
                            "direct delivery does not register a pipe waiter"
                        );
                        let reader = second;
                        assert_eq!(reader.slice().page, PageNumber(1));
                        drop(reader);
                        assert_eq!(admission.used(ResourceClass::RequestContext), baseline);
                        drop(stream);
                    }
                    "cancel" => {
                        scope.cancel().unwrap();
                        assert!(wakes.count() == before);
                        assert!(matches!(
                            stream.next_slice().as_mut().poll(&mut cx),
                            Poll::Ready(Ok(None))
                        ));
                        assert!(stream.terminated);
                        drop(second);
                        drop((stream, held));
                    }
                    _ => drop((second, stream, held)),
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
        stream.configure_subscription(1, PAGE_BYTES).unwrap();
        stream.inject(Slot::Ready(Ok(page_result(admission, &metadata, 0))));
        // Model a delivered page still owned by a slow caller.
        let first = futures::executor::block_on(stream.next_slice())
            .unwrap()
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(stream.slots.is_empty(), "no credit admits no window page");
        drop(first);
        assert_eq!(pipes.idle_count(), 0, "no pipe was even allocated");
        assert!(matches!(
            directory.subscriptions.open(metadata.version.clone()),
            Err(Error::Overloaded)
        ));
        futures::executor::block_on(stream.cancel()).unwrap();
        assert!(scope.cancellation.is_cancelled());
        assert!(stream.lease.is_none());
        assert!(stream.retained.is_empty());
        assert!(directory.subscriptions.open(metadata.version).is_ok());
    }
    #[test]
    fn client_page_progress_outlives_attempt_and_link_totals_without_refilling_retries() {
        let deadline = Instant::now() + std::time::Duration::from_secs(60);
        let mut budget = RangeBudget::ClientPages { deadline };
        // Model a healthy remote page that spends a four-link route and transfers acquisition
        // credits to its destination.
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
                Err(racer_core::read::Error::Unavailable)
            );
            budget.complete(page).unwrap();
        }
        let mut page = budget.next_page(false).unwrap().unwrap();
        for _ in 0..8 {
            page.begin_attempt(Instant::now(), deadline).unwrap();
        }
        assert_eq!(
            page.begin_attempt(Instant::now(), deadline),
            Err(racer_core::read::Error::Unavailable)
        );
        page.charge_links(16).unwrap();
        assert_eq!(
            page.charge_links(1),
            Err(racer_core::read::Error::HopBudgetExhausted)
        );
        assert_eq!(
            page.begin_attempt(deadline, deadline),
            Err(racer_core::read::Error::DeadlineExceeded)
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
            Err(racer_core::read::Error::Unavailable)
        );
        assert_eq!(
            next.charge_links(1),
            Err(racer_core::read::Error::HopBudgetExhausted)
        );
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
            stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
            let attempts = Rc::new(Cell::new(0));
            let gate = Rc::new(Cell::new(false));
            // Script both admitted acquisitions, avoiding full-page allocation.
            // A full pending window must prevent admission of page two onward.
            for number in 0..2 {
                let mut budget = stream.budget.next_page(number != 0).unwrap().unwrap();
                let attempts = attempts.clone();
                let gate = gate.clone();
                stream.inject(Slot::Fetching(Box::pin(async move {
                    for _ in 0..8 {
                        budget
                            .begin_attempt(Instant::now(), budget.deadline())
                            .map_err(Error::from)?;
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
                        .map(|_| unreachable!("same page received fresh retry credits"))
                        .map_err(Error::from);
                    Ok((result, budget))
                })));
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
                assert_eq!((stream.next, stream.slots.len()), (0, 2));
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
    /// A gate future that completes once `open` is set.
    fn gated(open: Rc<std::cell::Cell<bool>>) -> impl std::future::Future<Output = ()> {
        poll_fn(move |_| {
            if open.get() {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
    }
    fn stream_with_budget(
        f: &Fixture,
        metadata: &ObjectMetadata,
        budget: RangeBudget,
    ) -> RangeStream {
        let scope = RequestScope::new(RequestId([5; 16]), Instant::now() + Duration::from_secs(60))
            .unwrap();
        let mut stream = f.open(
            metadata,
            ByteRange::From(0).resolve(metadata.length).unwrap(),
            scope,
        );
        stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
        stream.budget = budget;
        stream
    }
    #[test]
    fn out_of_order_completion_waits_for_front_and_returns_only_unused_credits() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut budget = AcquisitionBudget::new(deadline, 6, 8);
        let mut first_budget = budget.partition(3, 4).unwrap();
        let mut last_budget = budget.partition(3, 4).unwrap();
        first_budget
            .begin_attempt(Instant::now(), deadline)
            .unwrap();
        last_budget.begin_attempt(Instant::now(), deadline).unwrap();
        last_budget.charge_links(3).unwrap();
        let gate = Rc::new(std::cell::Cell::new(false));
        let opened = gated(gate.clone());
        let mut stream = stream_with_budget(&f, &metadata(), RangeBudget::Shared(budget));
        stream.inject(Slot::Fetching(Box::pin(async move {
            opened.await;
            Ok((Err(Error::VersionUnavailable), first_budget))
        })));
        stream.inject(Slot::Fetching(Box::pin(async move {
            Ok((Err(Error::Unavailable), last_budget))
        })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        stream.poll_prefetch(&mut cx);
        assert!(matches!(
            stream.slots[1],
            Slot::Ready(Err(Error::Unavailable))
        ));
        assert!(matches!(stream.slots[0], Slot::Fetching(_)));
        assert_eq!(remaining_credits(&stream.budget), (2, 1));
        // Re-polling a completed page must not return its credits twice.
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert_eq!(remaining_credits(&stream.budget), (2, 1));
        gate.set(true);
        stream.poll_prefetch(&mut cx);
        assert!(matches!(
            stream.slots.front(),
            Some(Slot::Ready(Err(Error::VersionUnavailable)))
        ));
        let RangeBudget::Shared(budget) = &stream.budget else {
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
        // The head error is delivered first; the later error never is.
        assert!(matches!(
            stream.next_slice().as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::VersionUnavailable))
        ));
        assert!(matches!(
            stream.next_slice().as_mut().poll(&mut cx),
            Poll::Ready(Ok(None))
        ));
    }
    #[test]
    fn abandoning_window_drops_waiters_without_refunding_outstanding_spends() {
        struct Dropped(Rc<std::cell::Cell<bool>>);
        impl Drop for Dropped {
            fn drop(&mut self) {
                self.0.set(true);
            }
        }
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut budget = AcquisitionBudget::new(deadline, 3, 4);
        let child = budget.partition(3, 4).unwrap();
        let mut metadata = metadata();
        metadata.length = 3;
        let mut stream = stream_with_budget(&f, &metadata, RangeBudget::Shared(budget));
        let dropped = Rc::new(std::cell::Cell::new(false));
        let guard = Dropped(dropped.clone());
        stream.inject(Slot::Fetching(Box::pin(async move {
            let _guard = guard;
            std::future::pending::<()>().await;
            Ok((Err(Error::Cancelled), child))
        })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        futures::executor::block_on(stream.cancel()).unwrap();
        assert!(dropped.get());
        assert_eq!(remaining_credits(&stream.budget), (0, 0));
    }
    #[test]
    fn later_failure_still_fills_earlier_pages_and_surfaces_in_order() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(&f, &metadata(), RangeBudget::ClientPages { deadline });
        // The head probe misses only after the later page has already failed.
        let gate = Rc::new(std::cell::Cell::new(false));
        let opened = gated(gate.clone());
        stream.inject(Slot::Probing(Box::pin(async move {
            opened.await;
            Ok(None)
        })));
        let budget = stream.budget.next_page(false).unwrap().unwrap();
        stream.inject(Slot::Fetching(Box::pin(async move {
            Ok((Err(Error::VersionUnavailable), budget))
        })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(matches!(
            stream.slots[1],
            Slot::Ready(Err(Error::VersionUnavailable))
        ));
        gate.set(true);
        stream.poll_prefetch(&mut cx);
        // The missed head gets a direct fill despite the later failure (this fixture has no worker,
        // so starting it may fail at once), and nothing is admitted at or past the failed page.
        assert!(
            matches!(stream.slots[0], Slot::Fetching(_) | Slot::Ready(Err(_))),
            "the head must not be left without work"
        );
        assert!(matches!(
            stream.slots[1],
            Slot::Ready(Err(Error::VersionUnavailable))
        ));
        assert!(stream.selections.is_empty());
        // Once the head arrives it is delivered, then the failure ends the stream.
        stream.slots[0] = Slot::Ready(Ok(page_result(&f.admission, &metadata(), 0)));
        let Poll::Ready(Ok(Some(reader))) = stream.next_slice().as_mut().poll(&mut cx) else {
            panic!("head deliverable");
        };
        assert_eq!(reader.slice().page, PageNumber(0));
        assert!(matches!(
            stream.next_slice().as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::VersionUnavailable))
        ));
    }
    #[test]
    fn memory_hits_in_window_deliver_without_fetching_earlier_pages() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let metadata = metadata();
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(
            &f,
            &metadata,
            RangeBudget::Shared(AcquisitionBudget::new(deadline, 8, 16)),
        );
        let gate = Rc::new(std::cell::Cell::new(false));
        let opened = gated(gate.clone());
        let head = page_result(&f.admission, &metadata, 0);
        let budget = stream.budget.next_page(false).unwrap().unwrap();
        stream.inject(Slot::Fetching(Box::pin(async move {
            opened.await;
            Ok((Ok(head), budget))
        })));
        let hit = page_result(&f.admission, &metadata, 1);
        stream.inject(Slot::Probing(Box::pin(async move { Ok(Some(hit)) })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        // The memory hit is ready ahead of the head and started no fill.
        assert!(matches!(stream.slots[1], Slot::Ready(Ok(_))));
        assert!(stream.selections.is_empty());
        gate.set(true);
        for number in 0..2 {
            let Poll::Ready(Ok(Some(reader))) = stream.next_slice().as_mut().poll(&mut cx) else {
                panic!("page {number} deliverable");
            };
            assert_eq!(reader.slice().page, PageNumber(number));
        }
    }
    #[test]
    fn out_of_order_direct_fill_is_buffered_and_delivered_in_order() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let metadata = metadata();
        let scope = RequestScope::new(RequestId([6; 16]), Instant::now() + Duration::from_secs(60))
            .unwrap();
        let mut stream = f.open(
            &metadata,
            ByteRange::From(0).resolve(metadata.length).unwrap(),
            scope,
        );
        stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
        let gate = Rc::new(std::cell::Cell::new(false));
        let opened = gated(gate.clone());
        let head = page_result(&f.admission, &metadata, 0);
        let later = page_result(&f.admission, &metadata, 1);
        let first = stream.budget.next_page(false).unwrap().unwrap();
        let second = stream.budget.next_page(true).unwrap().unwrap();
        stream.inject(Slot::Fetching(Box::pin(async move {
            opened.await;
            Ok((Ok(head), first))
        })));
        stream.inject(Slot::Fetching(Box::pin(
            async move { Ok((Ok(later), second)) },
        )));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(matches!(stream.slots[1], Slot::Ready(Ok(_))));
        gate.set(true);
        let Poll::Ready(Ok(Some(reader))) = stream.next_slice().as_mut().poll(&mut cx) else {
            panic!("head deliverable");
        };
        assert_eq!(reader.slice().page, PageNumber(0));
        let Poll::Ready(Ok(Some(reader))) = stream.next_slice().as_mut().poll(&mut cx) else {
            panic!("the buffered page follows without another fetch");
        };
        assert_eq!(reader.slice().page, PageNumber(1));
    }
    #[test]
    fn selection_fanout_reaches_every_window_that_wants_the_page() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 4);
        let metadata = metadata();
        let mut streams = (0..2)
            .map(|_| {
                let mut stream = f.open(
                    &metadata,
                    ByteRange::From(0).resolve(metadata.length).unwrap(),
                    RequestScope::new(RequestId([4; 16]), Instant::now() + Duration::from_secs(60))
                        .unwrap(),
                );
                stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
                stream.inject(Slot::Ready(Ok(page_result(&f.admission, &metadata, 0))));
                stream.inject(Slot::Selecting);
                stream
            })
            .collect::<Vec<_>>();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for stream in &mut streams {
            stream.poll_prefetch(&mut cx);
            assert!(matches!(stream.slots[1], Slot::Selecting));
        }
        assert_eq!(
            streams[0].lease.as_ref().unwrap().busy_elsewhere(),
            BTreeSet::from([1])
        );
        let demand =
            racer_core::subscriptions::Demand::new(vec![flow_control::subscriptions::Interval {
                start: 1,
                end: 2,
            }])
            .unwrap();
        let selection = Selection::for_test(
            &f.streams.directory.subscriptions,
            metadata.version.clone(),
            demand,
        );
        // Pages outside the demand are refused.
        assert_eq!(
            selection.complete(page_result(&f.admission, &metadata, 0)),
            Err(Error::CorruptRecord)
        );
        selection
            .complete(page_result(&f.admission, &metadata, 1))
            .unwrap();
        for stream in &mut streams {
            stream.poll_prefetch(&mut cx);
            assert!(matches!(stream.slots[1], Slot::Ready(Ok(_))));
            for number in 0..2 {
                let Poll::Ready(Ok(Some(reader))) = stream.next_slice().as_mut().poll(&mut cx)
                else {
                    panic!("page {number} deliverable");
                };
                assert_eq!(reader.slice().page, PageNumber(number));
            }
        }
    }
    #[test]
    fn selection_failure_returns_pages_to_direct_fill_and_cancellation_is_fatal() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 2);
        let metadata = metadata();
        for error in [
            Error::Unavailable,
            Error::Overloaded,
            Error::Cancelled,
            Error::DeadlineExceeded,
        ] {
            let scope =
                RequestScope::new(RequestId([3; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(metadata.length).unwrap(),
                scope.clone(),
            );
            stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
            stream.inject(Slot::Ready(Ok(page_result(&f.admission, &metadata, 0))));
            stream.inject(Slot::Selecting);
            let budget = stream.budget.next_page(false).unwrap().unwrap();
            stream.selections.push(Pending {
                operation: Box::pin(async move { Ok((Err(error), budget)) }),
                pages: BTreeSet::from([1]),
            });
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            // A selection's own failure, even of its child deadline, is not the stream's.
            assert_eq!(stream.poll_selections(&mut cx), Ok(true));
            assert!(stream.selections.is_empty());
            assert!(matches!(stream.slots[1], Slot::Missing { select: false }));
            // The page is no longer offered for selection, only for a direct fill.
            assert!(
                stream
                    .selection_demand(&BTreeSet::new(), usize::MAX)
                    .unwrap()
                    .is_none()
            );
            // Canceling the stream itself is fatal.
            scope.cancel().unwrap();
            stream.poll_prefetch(&mut cx);
            assert_eq!(stream.error, Some(Error::Cancelled));
        }
    }
    #[test]
    fn active_warm_job_issues_no_selection_and_spill_stays_off_by_default() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 4);
        let mut metadata = metadata();
        metadata.length = 4 * PAGE_BYTES;
        let mut stream = f.open(
            &metadata,
            ByteRange::From(0).resolve(metadata.length).unwrap(),
            RequestScope::new(RequestId([2; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap(),
        );
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        assert!(stream.spill.is_none());
        for _ in 0..4 {
            stream.inject(Slot::Missing { select: true });
        }
        let (job, _) = f.streams.directory.warm.begin(&metadata.version).unwrap();
        let scope = stream.operation_scope();
        // Each page waits for the warm job through its own direct fill.
        assert_eq!(stream.start_selection(&scope), Ok(false));
        assert!(stream.selections.is_empty());
        assert!(
            stream
                .slots
                .iter()
                .all(|slot| matches!(slot, Slot::Missing { select: true }))
        );
        job.stop();
    }
    #[test]
    fn read_keeps_disjoint_selections_in_flight_up_to_spare_granted_flights() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 8);
        let mut metadata = metadata();
        metadata.length = 8 * PAGE_BYTES;
        let open = || {
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(metadata.length).unwrap(),
                RequestScope::new(RequestId([5; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap(),
            );
            stream.configure_subscription(8, 8 * PAGE_BYTES).unwrap();
            for _ in 0..8 {
                stream.inject(Slot::Missing { select: true });
            }
            stream
        };
        // Admit selections as `admit` does, with operations that never finish
        // in place of worker submissions.
        let admit = |stream: &mut RangeStream| {
            while stream.may_select() {
                let Some((_, pages)) = stream.plan_selection().unwrap() else {
                    break;
                };
                for number in &pages {
                    *stream.slot_mut(*number).unwrap() = Slot::Selecting;
                }
                stream.selections.push(Pending {
                    operation: Box::pin(std::future::pending()),
                    pages,
                });
            }
        };
        let selected = |stream: &RangeStream| {
            stream
                .selections
                .iter()
                .map(|pending| pending.pages.clone())
                .collect::<Vec<_>>()
        };
        // Before any provider grants more than one flight, a read runs one
        // selection over every selectable hole beyond its direct fills.
        let mut stream = open();
        admit(&mut stream);
        assert_eq!(selected(&stream), vec![BTreeSet::from_iter(0..8)]);
        drop(stream);
        // A grant of several flights on a contract of the version lets the
        // read split its holes over several concurrent selections.
        let scheduler = &f.streams.directory.subscriptions;
        let provider = racer_control_wire::NodeId("provider".into());
        let granted = Selection::for_test(
            scheduler,
            metadata.version.clone(),
            racer_core::subscriptions::Demand::new(vec![flow_control::subscriptions::Interval {
                start: 0,
                end: 1,
            }])
            .unwrap(),
        );
        scheduler
            .contract(
                &granted,
                MembershipVersion(1),
                provider.clone(),
                Some(REQUESTED_FLIGHTS),
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
        scheduler.granted(&granted, &provider, REQUESTED_FLIGHTS);
        drop(granted);
        assert_eq!(scheduler.spare(&metadata.version), 4);
        let mut stream = open();
        admit(&mut stream);
        let pages = selected(&stream);
        assert_eq!(pages.len(), SELECTIONS);
        assert_eq!(
            pages,
            vec![
                BTreeSet::from([0, 1]),
                BTreeSet::from([2, 3]),
                BTreeSet::from([4, 5]),
                BTreeSet::from([6, 7]),
            ]
        );
        assert!(
            stream
                .slots
                .iter()
                .all(|slot| matches!(slot, Slot::Selecting))
        );
        drop(stream);
        // Once the granted flights are in use, a read starts only its first
        // selection, over every hole.
        let held = (0..REQUESTED_FLIGHTS)
            .map(|_| {
                let held = Selection::for_test(
                    scheduler,
                    metadata.version.clone(),
                    racer_core::subscriptions::Demand::new(vec![
                        flow_control::subscriptions::Interval { start: 0, end: 1 },
                    ])
                    .unwrap(),
                );
                scheduler
                    .contract(
                        &held,
                        MembershipVersion(1),
                        provider.clone(),
                        Some(REQUESTED_FLIGHTS),
                        Instant::now() + Duration::from_secs(60),
                    )
                    .unwrap();
                held
            })
            .collect::<Vec<_>>();
        assert_eq!(scheduler.spare(&metadata.version), 0);
        let mut stream = open();
        admit(&mut stream);
        assert_eq!(selected(&stream), vec![BTreeSet::from_iter(0..8)]);
        drop(held);
    }
    #[test]
    fn selection_demand_covers_only_selectable_holes_not_selected_elsewhere() {
        let f = Fixture::new(crate::test_support::cluster::config(false).limits, 8);
        let mut metadata = metadata();
        metadata.length = 8 * PAGE_BYTES;
        let mut stream = f.open(
            &metadata,
            ByteRange::From(0).resolve(metadata.length).unwrap(),
            RequestScope::new(RequestId([2; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap(),
        );
        stream.configure_subscription(8, 8 * PAGE_BYTES).unwrap();
        stream.inject(Slot::Fetching(Box::pin(std::future::pending())));
        stream.inject(Slot::Ready(Ok(page_result(&f.admission, &metadata, 1))));
        stream.inject(Slot::Missing { select: true });
        stream.inject(Slot::Missing { select: false });
        for _ in 4..8 {
            stream.inject(Slot::Missing { select: true });
        }
        let (demand, pages) = stream
            .selection_demand(&BTreeSet::from([5]), usize::MAX)
            .unwrap()
            .unwrap();
        use flow_control::subscriptions::Interval;
        assert_eq!(
            demand.intervals(),
            &[
                Interval { start: 2, end: 3 },
                Interval { start: 4, end: 5 },
                Interval { start: 6, end: 8 },
            ]
        );
        assert_eq!(pages, BTreeSet::from([2, 4, 6, 7]));
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
    fn slices_cross_page_boundary_without_object_sized_plan() {
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
            let delivery = Rc::new(Delivery::new(reactor.clone(), Duration::from_secs(30)));
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
                    priority: racer_control_wire::DEFAULT_PRIORITY,
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
            stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
            let response = ReadResponse {
                retain_until: None,
                metadata,
                payload: crate::read::ReadPayload::Stream(stream),
            };
            let responses = Responses::new(io.clone(), delivery);
            let headers_sent = Arc::new(std::sync::atomic::AtomicBool::new(false));
            let reader_headers = headers_sent.clone();
            // In the progressing case the reader holds the stream after its first
            // page until the clock has crossed the original deadline, so the
            // remaining pages are delivered after it.
            let crossed = Arc::new(std::sync::atomic::AtomicBool::new(!progressing));
            let reader_crossed = crossed.clone();
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
                    if number == 2 {
                        let until = Instant::now() + Duration::from_secs(60);
                        while !reader_crossed.load(std::sync::atomic::Ordering::Acquire) {
                            assert!(Instant::now() < until, "clock never crossed the deadline");
                            std::thread::sleep(Duration::from_millis(1));
                        }
                    }
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
            let mut fetched = false;
            let result = loop {
                if let Poll::Ready(result) = work.as_mut().poll(&mut cx) {
                    break result;
                }
                reactor.poll_budgeted(128).unwrap();
                worker.poll(&mut cx);
                reactor.wait(Duration::from_millis(1)).unwrap();
                // Pages admitted before the headers keep the original deadline, and with four
                // credits that is every page of this range.
                if !crossed.load(std::sync::atomic::Ordering::Acquire)
                    && headers_sent.load(std::sync::atomic::Ordering::Acquire)
                    && worker
                        .origin
                        .completed(racer_object_wire::test_util::RequestKind::PinnedGet)
                        == 4
                    && worker.is_drained()
                {
                    if fetched {
                        clock.advance(Duration::from_millis(31));
                        crossed.store(true, std::sync::atomic::Ordering::Release);
                    }
                    fetched = true;
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
                Err(racer_core::read::Error::Unavailable)
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
            Err(racer_core::read::Error::DeadlineExceeded)
        );
    }

    mod subscriptions {
        use super::*;
        use crate::admission::AdmissionPolicy;
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::StrongEtag;
        use flow_control::Quotas;
        use flow_control::subscriptions::Interval;
        use racer_control_wire::CacheId;
        use racer_control_wire::MembershipVersion;
        use racer_control_wire::NodeId;
        use racer_core::subscriptions::Demand as WireDemand;
        fn version() -> ObjectVersion {
            ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            }
        }
        fn demand(start: u64, end: u64) -> WireDemand {
            WireDemand::new(vec![Interval { start, end }]).unwrap()
        }
        fn page(version: &ObjectVersion, number: u64) -> PageResult {
            let metadata = crate::model::ObjectMetadata {
                version: version.clone(),
                length: 1_000_000 * PAGE_BYTES,
                content_type: None,
                expires_at: crate::model::ExpiresAt::from_system_time(std::time::UNIX_EPOCH)
                    .unwrap(),
            };
            let admission: Quotas<AdmissionPolicy> = Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            super::tests::page_result(&admission, &metadata, number)
        }
        fn want(lease: &WindowLease, wants: &[u64]) {
            lease.sync(
                wants.iter().copied().collect(),
                BTreeSet::new(),
                futures::task::noop_waker_ref(),
            );
        }
        fn offered(lease: &WindowLease) -> Vec<u64> {
            lease
                .take_offered()
                .into_iter()
                .map(|page| page.plaintext.page().number.0)
                .collect()
        }
        fn issue(
            scheduler: &Arc<Scheduler>,
            start: u64,
            provider: &str,
            requested: Option<u16>,
            deadline: Instant,
        ) -> (
            Selection,
            Result<(crate::peer::subscriptions::Subscription, Instant)>,
        ) {
            let selection = Selection::for_test(scheduler, version(), demand(start, start + 1));
            let result = scheduler.contract(
                &selection,
                MembershipVersion(1),
                NodeId(provider.into()),
                requested,
                deadline,
            );
            (selection, result)
        }
        #[test]
        fn contracts_never_refill_and_start_with_one_selection_per_provider() {
            let scheduler = Scheduler::new(4);
            let deadline = uring_runtime::environment::now() + std::time::Duration::from_secs(30);
            let (first, one) = issue(&scheduler, 0, "provider", Some(4), deadline);
            let (one, _) = one.unwrap();
            // Until the provider grants more, a second concurrent selection from the same provider
            // waits for the first; another provider is independent.
            let (_, second) = issue(&scheduler, 5, "provider", Some(4), deadline);
            assert_eq!(second, Err(Error::Overloaded));
            let (_other, other) = issue(&scheduler, 5, "other", Some(4), deadline);
            other.unwrap();
            // A selection holds at most one contract.
            assert_eq!(
                scheduler.contract(
                    &first,
                    MembershipVersion(1),
                    NodeId("other".into()),
                    None,
                    deadline,
                ),
                Err(Error::InvalidRequest)
            );
            drop(first);
            let (_second, two) = issue(
                &scheduler,
                0,
                "provider",
                Some(4),
                deadline + std::time::Duration::from_secs(30),
            );
            let (two, expires) = two.unwrap();
            assert_eq!(one.id, two.id);
            // The refused attempt above still consumed no sequence.
            assert_eq!(two.sequence, one.sequence + 1);
            assert_eq!(two.page_budget + 1, one.page_budget);
            assert_eq!(two.byte_budget + PAGE_BYTES + 16, one.byte_budget);
            assert_eq!(expires, deadline);
        }
        #[test]
        fn contract_serves_up_to_granted_flights_and_refusal_resets_to_one() {
            let scheduler = Scheduler::new(4);
            let deadline = uring_runtime::environment::now() + std::time::Duration::from_secs(30);
            let provider = NodeId("provider".into());
            let (first, result) = issue(&scheduler, 0, "provider", Some(3), deadline);
            result.unwrap();
            assert_eq!(scheduler.spare(&version()), 0);
            // A grant above the request, or from another provider, is clamped or ignored.
            scheduler.granted(&first, &NodeId("other".into()), 3);
            assert_eq!(scheduler.spare(&version()), 0);
            scheduler.granted(&first, &provider, 9);
            assert_eq!(scheduler.spare(&version()), 2);
            let mut held = vec![first];
            let mut sequences = Vec::new();
            for start in 1..3 {
                let (selection, result) = issue(&scheduler, start, "provider", Some(3), deadline);
                sequences.push(result.unwrap().0.sequence);
                held.push(selection);
            }
            assert!(sequences.windows(2).all(|pair| pair[0] < pair[1]));
            assert_eq!(scheduler.spare(&version()), 0);
            let (_, fourth) = issue(&scheduler, 3, "provider", Some(3), deadline);
            assert_eq!(fourth, Err(Error::Overloaded));
            // A single-flight request is never one of several pending.
            held.pop();
            let (_, single) = issue(&scheduler, 3, "provider", None, deadline);
            assert_eq!(single, Err(Error::Overloaded));
            // A refusal drops the contract to one flight: two still pending
            // block new ones until both finish.
            let (selection, result) = issue(&scheduler, 3, "provider", Some(3), deadline);
            result.unwrap();
            scheduler.refused(&selection, &provider);
            held.push(selection);
            while let Some(selection) = held.pop() {
                let (_, next) = issue(&scheduler, 4, "provider", Some(3), deadline);
                assert_eq!(next, Err(Error::Overloaded));
                drop(selection);
            }
            let (_last, last) = issue(&scheduler, 4, "provider", Some(3), deadline);
            last.unwrap();
        }
        #[test]
        fn expired_contract_drains_before_a_fresh_one_replaces_it() {
            let scheduler = Scheduler::new(4);
            let deadline = uring_runtime::environment::now() + std::time::Duration::from_millis(20);
            let (first, result) = issue(&scheduler, 0, "provider", Some(2), deadline);
            let (old, _) = result.unwrap();
            scheduler.granted(&first, &NodeId("provider".into()), 2);
            std::thread::sleep(std::time::Duration::from_millis(30));
            let later = uring_runtime::environment::now() + std::time::Duration::from_secs(30);
            // The expired contract never signs another request.
            let (_, draining) = issue(&scheduler, 1, "provider", Some(2), later);
            assert_eq!(draining, Err(Error::Overloaded));
            assert_eq!(scheduler.spare(&version()), 0);
            drop(first);
            let (_fresh, fresh) = issue(&scheduler, 1, "provider", Some(2), later);
            let (fresh, expires) = fresh.unwrap();
            assert_ne!(fresh.id, old.id);
            assert_eq!(fresh.sequence, 1);
            assert_eq!(expires, later);
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
        fn scheduler_windows_are_bounded_and_drop_frees_capacity() {
            let scheduler = Scheduler::new(2);
            let first = scheduler.open(version()).unwrap();
            let second = scheduler.open(version()).unwrap();
            assert!(matches!(scheduler.open(version()), Err(Error::Overloaded)));
            drop(first);
            let third = scheduler.open(version()).unwrap();
            drop((second, third));
            assert!(scheduler.state.lock().unwrap().readers.is_empty());
        }
        #[test]
        fn offer_fans_out_to_every_window_that_wants_the_page() {
            let scheduler = Scheduler::new(4);
            let first = scheduler.open(version()).unwrap();
            let second = scheduler.open(version()).unwrap();
            let slow = scheduler.open(version()).unwrap();
            let mut other_version = version();
            other_version.etag = StrongEtag::test_value("v2");
            let other = scheduler.open(other_version).unwrap();
            want(&first, &[0, 900_000]);
            want(&second, &[0, 900_000]);
            want(&slow, &[0]);
            want(&other, &[0, 900_000]);
            let selection = Selection::for_test(&scheduler, version(), demand(0, 1_000_000));
            selection.complete(page(&version(), 0)).unwrap();
            selection.complete(page(&version(), 900_000)).unwrap();
            // A repeated page is not offered twice to the same window.
            selection.complete(page(&version(), 0)).unwrap();
            assert_eq!(offered(&first), vec![0, 900_000]);
            assert_eq!(offered(&second), vec![0, 900_000]);
            assert_eq!(offered(&slow), vec![0]);
            assert!(offered(&other).is_empty());
        }
        #[test]
        fn offers_outside_wanted_pages_are_dropped_and_inbox_stays_bounded() {
            let scheduler = Scheduler::new(1);
            let reader = scheduler.open(version()).unwrap();
            want(&reader, &[0, 1]);
            let selection = Selection::for_test(&scheduler, version(), demand(0, 1_000_000));
            for hot in 900_000..901_000 {
                selection.complete(page(&version(), hot)).unwrap();
            }
            selection.complete(page(&version(), 1)).unwrap();
            assert_eq!(offered(&reader), vec![1]);
            let state = scheduler.state.lock().unwrap();
            assert!(
                state
                    .readers
                    .values()
                    .all(|reader| reader.offered.is_empty())
            );
        }
        #[test]
        fn dropped_window_is_removed_from_fanout_and_busy_elsewhere() {
            let scheduler = Scheduler::new(2);
            let supplier = scheduler.open(version()).unwrap();
            let reader = scheduler.open(version()).unwrap();
            supplier.sync(
                BTreeSet::from([800_000]),
                BTreeSet::from([800_000]),
                futures::task::noop_waker_ref(),
            );
            want(&reader, &[0, 800_000]);
            assert_eq!(reader.busy_elsewhere(), BTreeSet::from([800_000]));
            assert!(supplier.busy_elsewhere().is_empty());
            let selection = Selection::for_test(&scheduler, version(), demand(800_000, 800_001));
            selection.complete(page(&version(), 800_000)).unwrap();
            assert_eq!(offered(&supplier), vec![800_000]);
            assert_eq!(offered(&reader), vec![800_000]);
            supplier.sync(
                BTreeSet::from([800_001]),
                BTreeSet::from([800_001]),
                futures::task::noop_waker_ref(),
            );
            drop(supplier);
            assert!(reader.busy_elsewhere().is_empty());
            assert_eq!(scheduler.state.lock().unwrap().readers.len(), 1);
            Selection::for_test(&scheduler, version(), demand(0, 1))
                .complete(page(&version(), 0))
                .unwrap();
            assert_eq!(offered(&reader), vec![0]);
        }
        fn stream(
            f: &super::Fixture,
            metadata: &ObjectMetadata,
            range: ResolvedRange,
        ) -> RangeStream {
            f.open(
                metadata,
                range,
                RequestScope::new(
                    crate::model::RequestId([1; 16]),
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap(),
            )
        }
        #[test]
        fn partial_final_page_uses_exact_bytes_and_completion_does_not_require_release() {
            let f = super::Fixture::new(crate::test_support::cluster::config(false).limits, 2);
            let mut metadata = super::metadata();
            metadata.length = PAGE_BYTES + 5;
            let range = crate::model::ByteRange::From(PAGE_BYTES - 3)
                .resolve(metadata.length)
                .unwrap();
            let mut reader = stream(&f, &metadata, range);
            reader.configure_subscription(2, PAGE_BYTES).unwrap();
            for number in 0..2 {
                reader.inject(Slot::Ready(Ok(super::page_result(
                    &f.admission,
                    &metadata,
                    number,
                ))));
            }
            let first = futures::executor::block_on(reader.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(first.slice().length, 3);
            let second = futures::executor::block_on(reader.next_slice())
                .unwrap()
                .unwrap();
            assert_eq!(second.slice().length, 5);
            assert!(
                futures::executor::block_on(reader.next_slice())
                    .unwrap()
                    .is_none()
            );
            assert!(!reader.credits.as_ref().unwrap().is_empty());
            assert_eq!(
                reader.release_page(PageNumber(0), 5),
                Err(Error::InvalidRequest)
            );
            reader.release_page(PageNumber(0), 3).unwrap();
            assert_eq!(
                reader.release_page(PageNumber(0), 3),
                Err(Error::InvalidRequest)
            );
            assert_eq!(
                reader.release_page(PageNumber(1), 3),
                Err(Error::InvalidRequest)
            );
            reader.release_page(PageNumber(1), 5).unwrap();
            assert!(reader.credits.as_ref().unwrap().is_empty());
        }
        #[test]
        fn credit_boundaries_and_invalid_transitions_preserve_request_errors() {
            let f = super::Fixture::new(crate::test_support::cluster::config(false).limits, 2);
            let mut metadata = super::metadata();
            metadata.length = PAGE_BYTES;
            let range = crate::model::ByteRange::From(0)
                .resolve(PAGE_BYTES)
                .unwrap();
            for (pages, bytes) in [(1, PAGE_BYTES), (64, 64 * PAGE_BYTES)] {
                let mut reader = stream(&f, &metadata, range);
                assert_eq!(
                    reader.release_page(PageNumber(0), PAGE_BYTES as u32),
                    Err(Error::InvalidRequest)
                );
                reader.configure_subscription(pages, bytes).unwrap();
                reader.inject(Slot::Ready(Ok(super::page_result(
                    &f.admission,
                    &metadata,
                    0,
                ))));
                assert_eq!(
                    reader.configure_subscription(pages, bytes),
                    Err(Error::InvalidRequest)
                );
                assert_eq!(
                    reader.release_page(PageNumber(0), PAGE_BYTES as u32),
                    Err(Error::InvalidRequest)
                );
                let lease = futures::executor::block_on(reader.next_slice())
                    .unwrap()
                    .unwrap();
                assert_eq!(lease.slice().page, PageNumber(0));
                reader
                    .release_page(PageNumber(0), PAGE_BYTES as u32)
                    .unwrap();
                assert_eq!(
                    reader.release_page(PageNumber(0), PAGE_BYTES as u32),
                    Err(Error::InvalidRequest)
                );
            }
        }
        #[test]
        fn invalid_credit_contracts_never_admit_demand() {
            let f = super::Fixture::new(crate::test_support::cluster::config(false).limits, 1);
            let metadata = super::metadata();
            let range = crate::model::ByteRange::From(0).resolve(1).unwrap();
            for (pages, bytes) in [
                (0, PAGE_BYTES),
                (65, PAGE_BYTES),
                (1, 0),
                (1, PAGE_BYTES - 1),
                (1, 64 * PAGE_BYTES + 1),
                (1, 65 * PAGE_BYTES),
            ] {
                let mut reader = stream(&f, &metadata, range);
                assert_eq!(
                    reader.configure_subscription(pages, bytes),
                    Err(Error::InvalidRequest)
                );
                assert!(reader.lease.is_none());
            }
            let scheduler = &f.streams.directory.subscriptions;
            assert!(scheduler.state.lock().unwrap().readers.is_empty());
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
