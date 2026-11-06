//! Per-read fetch window: pages are fetched out of order and delivered in order.
//!
//! Each stream owns a window of pages starting at its delivery cursor. Window
//! pages are probed in local memory, filled directly through their owning worker,
//! or selected from a peer provider in whatever order they complete. Delivery
//! stays strictly in page order. A stream pins its version and length once. A
//! late error terminates that stream; it cannot replace headers or reopen
//! against a newer version.

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

use racer_core::read::effect::{EffectId, EffectIds};
use racer_core::read::window as win;
use racer_core::read::{self, PageBudget};
#[cfg(test)]
use racer_core::read::{DIRECT_AHEAD, SELECTIONS, window::PAGE_RETRIES};

/// Flights requested per provider contract. A provider grants at most this
/// many pending selections per contract, or one if it predates multi-flight.
pub(crate) const REQUESTED_FLIGHTS: u16 = 4;
const _: () = assert!(REQUESTED_FLIGHTS <= racer_core::subscriptions::MAX_SUBSCRIPTION_FLIGHTS);

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
type ContractKey = (ObjectVersion, u64, racer_wire::control::NodeId);

/// A peer subscription contract and the selections it has in flight.
struct Contract {
    subscription: crate::peer::subscriptions::Subscription,
    expires: Instant,
    /// Selections currently using the contract.
    inflight: u16,
    /// Flights asked of the provider by the latest request (1 when the field
    /// was omitted).
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
    /// Contract expiry and identifiers.
    ctx: racer_io::WorkerCtx,
    state: Mutex<State>,
}

impl Scheduler {
    /// Issue the next sequence of the node's contract with `provider` for
    /// `selection`, asking for `requested` flights. A contract serves at most
    /// its granted flights at once; one more concurrent selection is refused
    /// with `Overloaded` until an earlier `Selection` is dropped.
    pub(crate) fn contract(
        &self,
        selection: &Selection,
        membership: racer_wire::control::MembershipVersion,
        provider: racer_wire::control::NodeId,
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
        let now = self.ctx.now();
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
            self.ctx.rng.fill(&mut id).map_err(|_| Error::Unavailable)?;
            state.contracts.insert(
                key.clone(),
                Contract {
                    subscription: crate::peer::subscriptions::Subscription {
                        id,
                        version,
                        demand: Default::default(),
                        sequence: 0,
                        page_budget: u32::MAX,
                        // Largest-page units on purpose: the provider debits the
                        // same `PAGE_BYTES + 16` per selection
                        // (`racer_core::subscriptions`), and `u32::MAX` of them
                        // never binds whatever the published page size.
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
        provider: &racer_wire::control::NodeId,
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
    pub(crate) fn refused(&self, selection: &Selection, provider: &racer_wire::control::NodeId) {
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
    #[cfg(test)]
    fn spare(&self, version: &ObjectVersion) -> usize {
        let now = self.ctx.now();
        spare(&self.state.lock().unwrap(), version, now)
    }

    pub(crate) fn new(capacity: usize, ctx: racer_io::WorkerCtx) -> Arc<Self> {
        Arc::new(Self {
            capacity,
            ctx,
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
        let mut wake = Vec::new();
        offer(&mut self.state.lock().unwrap(), version, page, &mut wake);
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

/// Unused granted flights across the live contracts of `version`.
fn spare(state: &State, version: &ObjectVersion, now: Instant) -> usize {
    state
        .contracts
        .iter()
        .filter(|((contracted, _, _), contract)| contracted == version && contract.expires > now)
        .map(|(_, contract)| usize::from(contract.granted.saturating_sub(contract.inflight)))
        .sum()
}

/// Queue `page` for every open window of `version` that wants it; their wakers
/// go to `wake`, to be woken once the lock is released.
fn offer(state: &mut State, version: &ObjectVersion, page: &PageResult, wake: &mut Vec<Waker>) {
    let number = page.plaintext.page().number.0;
    for reader in state.readers.values_mut() {
        if &reader.version != version || !reader.wants.remove(&number) {
            continue;
        }
        reader.busy.remove(&number);
        reader.offered.push_back(page.clone());
        wake.extend(reader.waker.take());
    }
}

/// Registration of one stream's window. Dropping it stops fan-out to the stream.
struct WindowLease {
    scheduler: Arc<Scheduler>,
    id: u64,
}

impl WindowLease {
    /// One step's exchange with the scheduler, under one lock: publish the
    /// pages `window` still wants and those it is busy with, take the pages
    /// offered to it, fan its directly `filled` pages out to other windows of
    /// `version`, and refresh `facts` (pages busy elsewhere, spare flights).
    fn exchange(
        &self,
        window: Option<&win::ReadWindow>,
        filled: &mut Vec<PageResult>,
        version: &ObjectVersion,
        waker: &Waker,
        offered: &mut VecDeque<PageResult>,
        facts: &mut win::Facts,
    ) {
        let now = self.scheduler.ctx.now();
        let mut wake = Vec::new();
        {
            let mut guard = self.scheduler.state.lock().unwrap();
            let state = &mut *guard;
            if let Some(reader) = state.readers.get_mut(&self.id) {
                match window {
                    Some(window) => window.interest_into(&mut reader.wants, &mut reader.busy),
                    None => {
                        reader.wants.clear();
                        reader.busy.clear();
                    }
                }
                if !reader
                    .waker
                    .as_ref()
                    .is_some_and(|known| known.will_wake(waker))
                {
                    reader.waker = Some(waker.clone());
                }
                offered.extend(reader.offered.drain(..));
            }
            for page in filled.drain(..) {
                offer(state, version, &page, &mut wake);
            }
            facts.elsewhere.clear();
            for (id, reader) in &state.readers {
                if *id != self.id && &reader.version == version {
                    facts.elsewhere.extend(reader.busy.iter().copied());
                }
            }
            facts.spare = spare(state, version, now);
        }
        for waker in wake {
            waker.wake();
        }
    }

    /// Publish the pages this window still wants and those it is fetching.
    #[cfg(test)]
    fn sync(&self, wants: BTreeSet<u64>, busy: BTreeSet<u64>, waker: &Waker) {
        let mut state = self.scheduler.state.lock().unwrap();
        if let Some(reader) = state.readers.get_mut(&self.id) {
            reader.wants = wants;
            reader.busy = busy;
            reader.waker = Some(waker.clone());
        }
    }

    /// Selected pages offered since the last call.
    #[cfg(test)]
    fn take_offered(&self) -> VecDeque<PageResult> {
        let mut state = self.scheduler.state.lock().unwrap();
        state
            .readers
            .get_mut(&self.id)
            .map(|reader| std::mem::take(&mut reader.offered))
            .unwrap_or_default()
    }

    /// Pages that other windows of the same version are already selecting or
    /// filling directly.
    #[cfg(test)]
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

/// One provider selection over some window pages. The worker running it
/// offers the selected page to every interested window, then drops it, which
/// frees its flight on the provider contract for the next selection.
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

#[cfg(test)]
pub(crate) fn client_page_budget_for_test(deadline: Instant) -> AcquisitionBudget {
    PageBudget::Client { deadline }
        .next_page(racer_io::Clock::host().now())
        .unwrap()
}

/// One page for a direct read: plaintext, or ciphertext the sink verifies.
pub(crate) enum DirectItem {
    Plaintext(ReaderLease),
    /// A sealed page and the slice of it the range covers.
    Sealed(Box<SealedPage>, crate::model::PageSlice),
}

/// One outstanding operation of the window, keyed by its [`EffectId`].
enum Io {
    /// Looking the page up in local memory (or its held disk copy).
    Probe(Operation<'static, Option<PageResult>>),
    /// A direct fill through the owning worker.
    Fetch(Operation<'static, PageResult>),
    /// A direct fill in sealed mode, which may return ciphertext for the GPU.
    FetchSealed(Operation<'static, Fetched>),
    /// A provider selection; its page arrives through the scheduler's fan-out.
    Select(Operation<'static, PageResult>),
    /// Reserving this read's spill hold on a page.
    Hold(PageNumber, Operation<'static, HoldEpoch>),
    /// Filling a held page, which persists it under the hold.
    SpillFill(Operation<'static, ()>),
}

/// A page the window holds under a handle until it is delivered or discarded.
#[allow(clippy::large_enum_variant)] // Plaintext pages are the common case; boxing them costs an allocation per page.
enum Held {
    Page(PageResult),
    Sealed(Box<SealedPage>),
}

/// Open pinned streams on the ingress reactor using the shared worker directory.
pub struct RangeStreams {
    directory: std::rc::Rc<WorkerDirectory>,

    delivery: Rc<Delivery>,

    window_pages: usize,

    /// This worker's detached driver queue for stream cleanup and hints.
    drivers: Rc<crate::drivers::DriverQueue>,
}

/// The body stays on the ingress reactor, including its delivery pipe leases.
/// ```compile_fail
/// use racer::read::range_stream::RangeStream;
/// fn send<T: Send>() {}
/// send::<RangeStream>();
/// ```
pub struct RangeStream {
    /// The opening worker's detached driver queue.
    drivers: Rc<crate::drivers::DriverQueue>,

    /// The cache retains nothing: no peer selections and no spill holds.
    passthrough: bool,

    error: Option<Error>,

    lease: Option<WindowLease>,

    credits: Option<Window<PageNumber>>,

    retained: BTreeMap<PageNumber, crate::memory::VerifiedPage>,

    metadata: ObjectMetadata,

    range: ResolvedRange,

    context: OriginContext,

    membership: Arc<crate::topology::Membership>,

    scope: RequestScope,

    directory: std::rc::Rc<WorkerDirectory>,

    delivery: Rc<Delivery>,

    window_pages: usize,

    budget: PageBudget,

    /// Spill pages past the window, 0 when the read does not spill.
    spill: usize,

    /// The window machine, built on first use from the settings above.
    window: Option<win::ReadWindow>,

    ids: EffectIds,

    /// Outstanding operations, polled each step.
    io: Vec<(EffectId, Io)>,

    /// Pages fetched, probed or offered, by handle.
    pages: BTreeMap<EffectId, Held>,

    /// Inputs produced without an operation (an operation that could not start).
    inputs: VecDeque<win::Input>,

    /// Effects to apply, reused across steps.
    effects: Vec<win::Effect>,

    /// Pages offered by the scheduler, not yet fed to the window.
    offered: VecDeque<PageResult>,

    /// Directly filled pages to fan out to other windows at the next exchange.
    filled: Vec<PageResult>,

    /// Scheduler facts from the latest exchange.
    facts: win::Facts,
    /// The cursor and per-slot interest classes the latest exchange published.
    published: (u64, Vec<u8>),

    /// Spill hold epochs by the window's epoch number.
    epochs: BTreeMap<win::Epoch, HoldEpoch>,

    next_epoch: win::Epoch,

    /// Failures without a core error, by page: the core sees them as `Internal`.
    failures: BTreeMap<u64, Error>,

    /// Set for direct GPU reads: misses are fetched sealed for this GPU.
    sealed: Option<SealedTarget>,

    terminated: bool,
}

impl RangeStreams {
    /// Bind stream creation to this worker's delivery resources and read window.
    pub fn new(
        directory: std::rc::Rc<WorkerDirectory>,
        delivery: Rc<Delivery>,
        window_pages: usize,
        drivers: Rc<crate::drivers::DriverQueue>,
    ) -> Self {
        Self {
            directory,
            delivery,
            window_pages,
            drivers,
        }
    }

    /// The configured `range_window_pages` of each stream.
    pub(crate) fn window_pages(&self) -> usize {
        self.window_pages
    }

    /// Share the same metadata and page owners used by newly opened streams.
    pub(crate) fn directory(&self) -> &std::rc::Rc<WorkerDirectory> {
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
        let budget = PageBudget::Client {
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
            drivers: self.drivers.clone(),
            passthrough: false,
            error: None,
            lease: None,
            credits: None,
            retained: BTreeMap::new(),
            metadata,
            range,
            context,
            membership,
            scope,
            directory: self.directory.clone(),
            delivery: self.delivery.clone(),
            window_pages: self.window_pages,
            budget,
            spill: 0,
            window: None,
            ids: EffectIds::new(),
            io: Vec::new(),
            pages: BTreeMap::new(),
            inputs: VecDeque::new(),
            effects: Vec::new(),
            offered: VecDeque::new(),
            filled: Vec::new(),
            facts: win::Facts::default(),
            published: (0, Vec::new()),
            epochs: BTreeMap::new(),
            next_epoch: 0,
            failures: BTreeMap::new(),
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

    /// Fetch every page directly through its owner's flight, for a cache that
    /// retains nothing. Selections would ask providers for retained copies and
    /// spill would hold pages on disk. Call before the subscription is configured.
    pub(crate) fn set_passthrough(&mut self) {
        self.passthrough = true;
        self.window = None;
    }

    /// Install exact page/byte release credits once, before delivery. The fetch
    /// window is the configured `range_window_pages`, further limited by the
    /// client's unreleased credits.
    pub fn configure_subscription(&mut self, pages: usize, bytes: u64) -> Result<()> {
        if self.lease.is_some() {
            return Err(Error::InvalidRequest);
        }
        // Credits cover at least one page of the cluster page size. The ceiling
        // stays at the largest page size so a client sized for 16 MiB pages is
        // still accepted; byte credits only bound, never size, a delivery.
        let size = self.range.page_size().get();
        if !(1..=64).contains(&pages) || !(size..=64 * PAGE_BYTES).contains(&bytes) {
            return Err(Error::InvalidRequest);
        }
        let credits = Window::new(pages, bytes, size).map_err(window_error)?;
        let lease = self
            .directory
            .subscriptions
            .open(self.metadata.version.clone())?;
        self.credits = Some(credits);
        self.lease = Some(lease);
        self.facts.leased = true;
        let limit = self.directory.spill_config().range_spill_pages;
        self.spill = if self.passthrough { 0 } else { limit };
        self.window = None;
        Ok(())
    }

    /// Fetch pages that are not verified plaintext in memory as sealed pages
    /// for `target`'s GPU. Call before the first delivery.
    pub(crate) fn set_sealed(&mut self, target: SealedTarget) -> Result<()> {
        if self.terminated
            || self
                .window
                .as_ref()
                .is_some_and(|window| window.slots().next().is_some() || window.terminated())
        {
            return Err(Error::InvalidRequest);
        }
        self.sealed = Some(target);
        self.window = None;
        Ok(())
    }

    /// The window machine, built from the stream's settings on first use.
    fn window(&mut self) -> &mut win::ReadWindow {
        let config = win::WindowConfig {
            start: self.range.first_page().0,
            end: self.range.last_page().0 + 1,
            window_pages: self.window_pages,
            sealed: self.sealed.is_some(),
            passthrough: self.passthrough,
            spill: self.spill,
            budget: self.budget,
        };
        self.window
            .get_or_insert_with(|| win::ReadWindow::new(config))
    }

    /// Tell a sealed page's owner what the GPU decided.
    pub(crate) fn report_sealed(&self, report: SealedReport, passed: bool) {
        self.directory
            .report_sealed(&self.drivers, report, passed, &self.scope);
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

    /// Drive the fetch window while the current slice encounters client
    /// backpressure. Completed pages leave their acquisition scopes promptly.
    /// Delivered leases consume client credit, not window slots; their full
    /// plaintext allocations remain admitted until released.
    pub(crate) fn poll_prefetch(&mut self, cx: &mut Context<'_>) {
        if self.terminated || self.error.is_some() || self.lease.is_none() {
            return;
        }
        if let Err(error) = self.step(cx) {
            self.error = Some(error);
        }
    }

    /// One step of the fetch job: feed completions and offers to the window and
    /// apply the work it plans until nothing changes, then exchange with the
    /// node-wide scheduler once (more only while it offers pages).
    fn step(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let scope = self.operation_scope();
        scope.check()?;
        // Offers that arrived since the last step go first, as they spare
        // direct fills of their pages.
        // Facts (pages busy elsewhere, spare flights) are refreshed once per
        // step so admission sees other windows' current work.
        self.exchange(cx);
        loop {
            loop {
                let mut changed = self.poll_io(cx, &scope);
                changed |= self.feed_offered(&scope);
                changed |= self.pump(&scope);
                if !changed {
                    break;
                }
            }
            // Exchange again only when other windows would see a difference.
            if self.filled.is_empty() && !self.interest_changed() {
                return Ok(());
            }
            if !self.exchange(cx) {
                return Ok(());
            }
        }
    }

    /// Whether the window's per-slot interest differs from what the latest
    /// exchange published.
    fn interest_changed(&mut self) -> bool {
        let Some(window) = self.window.as_ref() else {
            return false;
        };
        let classes = window.slots().map(interest_class);
        if window.cursor() != self.published.0
            || window.slots().len() != self.published.1.len()
            || !classes.eq(self.published.1.iter().copied())
        {
            return true;
        }
        false
    }

    /// Publish this window's interest and fan out its filled pages, take the
    /// pages other selections offered it and refresh the scheduler facts, under
    /// one scheduler lock. True when pages were offered.
    fn exchange(&mut self, cx: &Context<'_>) -> bool {
        let Some(lease) = self.lease.as_ref() else {
            return false;
        };
        if let Some(window) = self.window.as_ref() {
            self.published.0 = window.cursor();
            self.published.1.clear();
            self.published.1.extend(window.slots().map(interest_class));
        }
        lease.exchange(
            self.window.as_ref(),
            &mut self.filled,
            &self.metadata.version,
            cx.waker(),
            &mut self.offered,
            &mut self.facts,
        );
        !self.offered.is_empty()
    }

    /// Plan new work: refill the window within client credits, then direct
    /// fills, selections and spill holds.
    fn pump(&mut self, scope: &RequestScope) -> bool {
        let now = self.scope.clock().now();
        self.window();
        let Some(window) = self.window.as_mut() else {
            return false;
        };
        let range = self.range;
        let credits = &mut self.credits;
        let mut effects = std::mem::take(&mut self.effects);
        window.pump(
            now,
            &self.facts,
            |number| reserve(&range, credits, PageNumber(number)),
            &mut self.ids,
            &mut effects,
        );
        let changed = !effects.is_empty();
        self.apply(&mut effects, scope);
        self.effects = effects;
        changed
    }

    /// Feed one input to the window and apply what it emits.
    fn feed(&mut self, input: win::Input, scope: &RequestScope) {
        let live = self.scope.check().is_ok();
        self.window();
        let Some(window) = self.window.as_mut() else {
            return;
        };
        let mut effects = std::mem::take(&mut self.effects);
        if window.on(input, live, &mut self.ids, &mut effects).is_err() {
            // A completion the window no longer waits for: drop its page.
            if let win::Input::Probed { id, .. } | win::Input::Fetched { id, .. } = input {
                self.pages.remove(&id);
            }
        }
        self.apply(&mut effects, scope);
        self.effects = effects;
    }

    /// Start or perform each effect the window emitted.
    fn apply(&mut self, effects: &mut Vec<win::Effect>, scope: &RequestScope) {
        for effect in effects.drain(..) {
            match effect {
                win::Effect::Probe { id, page, from } => {
                    let probe = self.start_probe(PageNumber(page), from, scope);
                    self.io.push((id, Io::Probe(probe)));
                }
                win::Effect::Fetch {
                    id,
                    page,
                    budget,
                    sealed: _,
                } => self.start_fetch(id, PageNumber(page), budget.build(), scope),
                win::Effect::Select {
                    id,
                    intervals,
                    pages: _,
                    budget,
                } => self.start_selection(id, intervals, budget.build(), scope),
                win::Effect::Hold { id, page, hold } => self.start_hold(id, page, hold, scope),
                win::Effect::SpillFill { id, page } => {
                    let fill = self.start_spill_fill(PageNumber(page));
                    self.io.push((id, Io::SpillFill(fill)));
                }
                win::Effect::Release(pages) => {
                    let pages = pages
                        .into_iter()
                        .filter_map(|(number, epoch)| {
                            Some((PageNumber(number), self.epochs.remove(&epoch)?))
                        })
                        .collect::<Vec<_>>();
                    if !pages.is_empty() {
                        self.directory.release_read_pages(
                            &self.drivers,
                            &self.metadata.version,
                            pages,
                            self.scope.request,
                        );
                    }
                }
                win::Effect::Discard(handle) => {
                    self.pages.remove(&handle);
                }
            }
        }
    }

    fn page_id(&self, number: PageNumber) -> PageId {
        PageId {
            version: self.metadata.version.clone(),
            number,
        }
    }

    /// Probe a page entering the window. A sealed read probes live plaintext
    /// only: cached or disk ciphertext is then fetched sealed, so the GPU
    /// rather than the CPU decrypts it. A spilled page is read back from
    /// memory or its held disk copy, after its spill fill when one is running;
    /// if that fails the page is fetched normally.
    fn start_probe(
        &mut self,
        number: PageNumber,
        from: win::ProbeFrom,
        scope: &RequestScope,
    ) -> Operation<'static, Option<PageResult>> {
        let page = self.page_id(number);
        let directory = self.directory.clone();
        let scope = scope.clone();
        match from {
            win::ProbeFrom::Memory if self.sealed.is_some() => {
                Box::pin(async move { directory.cached_page(page, &scope).await })
            }
            win::ProbeFrom::Memory => {
                Box::pin(async move { directory.local_page(page, &scope).await })
            }
            win::ProbeFrom::Held { after } => {
                let warming = after.and_then(|fill| {
                    let index = self.io.iter().position(|(id, _)| *id == fill)?;
                    let (_, Io::SpillFill(warming)) = self.io.swap_remove(index) else {
                        unreachable!("a spill fill id names a spill fill");
                    };
                    // The window ignores the moved fill's completion; settle it now.
                    self.inputs
                        .push_back(win::Input::SpillFilled { id: fill, ok: true });
                    Some(warming)
                });
                Box::pin(async move {
                    if let Some(warming) = warming {
                        warming.await?;
                    }
                    directory.held_page(page, &scope).await.map(Some)
                })
            }
        }
    }

    fn start_fetch(
        &mut self,
        id: EffectId,
        number: PageNumber,
        budget: AcquisitionBudget,
        scope: &RequestScope,
    ) {
        let mut child_scope = scope.clone();
        child_scope.deadline.0 = budget.deadline();
        let page = self.page_id(number);
        let started = match &self.sealed {
            Some(target) => self
                .directory
                .start_page_sealed(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &child_scope,
                    budget,
                    target.clone(),
                )
                .map(Io::FetchSealed),
            None => self
                .directory
                .start_page(
                    page,
                    self.membership.clone(),
                    &self.context,
                    &child_scope,
                    budget,
                )
                .map(Io::Fetch),
        };
        match started {
            Ok(io) => self.io.push((id, io)),
            Err(error) => {
                let result = Err(self.core_error(number.0, error));
                self.inputs.push_back(win::Input::Fetched { id, result });
            }
        }
    }

    /// Select pages from a peer provider. A selection that cannot start leaves
    /// its pages to direct fills; the stream's own cancellation is fatal.
    fn start_selection(
        &mut self,
        id: EffectId,
        intervals: Vec<std::ops::Range<u64>>,
        budget: AcquisitionBudget,
        scope: &RequestScope,
    ) {
        use flow_control::subscriptions::Interval;
        let intervals = intervals
            .into_iter()
            .map(|r| Interval {
                start: r.start,
                end: r.end,
            })
            .collect();
        let Some(demand) = racer_core::subscriptions::Demand::new(intervals) else {
            self.inputs
                .push_back(win::Input::Cancel(read::Error::InvalidRequest));
            return;
        };
        let selection = Selection {
            scheduler: self.directory.subscriptions.clone(),
            version: self.metadata.version.clone(),
            demand,
            contract: Mutex::new(None),
        };
        let mut child_scope = scope.clone();
        child_scope.deadline.0 = budget.deadline();
        match self.directory.start_selection(
            self.metadata.version.clone(),
            selection,
            self.membership.clone(),
            &self.context,
            &child_scope,
            budget,
        ) {
            Ok(operation) => self.io.push((id, Io::Select(operation))),
            Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => {
                let error = read::Error::try_from(error).unwrap_or(read::Error::Cancelled);
                self.inputs.push_back(win::Input::Cancel(error));
            }
            Err(_) => self.inputs.push_back(win::Input::Selected {
                id,
                ok: false,
                started: false,
            }),
        }
    }

    /// Reserve this read's spill hold on a page past the window, for at most
    /// `hold` and never past the read's own deadline.
    fn start_hold(&mut self, id: EffectId, number: u64, hold: Duration, scope: &RequestScope) {
        let remaining = scope
            .deadline
            .0
            .saturating_duration_since(scope.clock().now());
        let Some(until) = scope.clock().wall_now().checked_add(hold.min(remaining)) else {
            self.inputs.push_back(win::Input::Held {
                id,
                result: Err(read::Error::InvalidRequest),
            });
            return;
        };
        let directory = self.directory.clone();
        let page = self.page_id(PageNumber(number));
        let scope = scope.clone();
        self.io.push((
            id,
            Io::Hold(
                PageNumber(number),
                Box::pin(async move { directory.hold_read_page(&page, until, &scope).await }),
            ),
        ));
    }

    fn start_spill_fill(&self, number: PageNumber) -> Operation<'static, ()> {
        match self.directory.start_spill_page(
            self.page_id(number),
            self.metadata.immutable(),
            self.membership.clone(),
            &self.context,
            &self.operation_scope(),
        ) {
            Ok(operation) => operation,
            Err(error) => Box::pin(async move { Err(error) }),
        }
    }

    /// The core error for a page failure. Errors without a core counterpart
    /// reach the window as `Internal` and are restored when the page fails the
    /// read.
    fn core_error(&mut self, number: u64, error: Error) -> read::Error {
        read::Error::try_from(error).unwrap_or_else(|error| {
            self.failures.insert(number, error);
            read::Error::Internal
        })
    }

    /// Poll every outstanding operation once and feed what finished. True when
    /// the window received any input.
    fn poll_io(&mut self, cx: &mut Context<'_>, scope: &RequestScope) -> bool {
        let mut changed = false;
        while let Some(input) = self.inputs.pop_front() {
            self.feed(input, scope);
            changed = true;
        }
        let mut index = 0;
        while index < self.io.len() {
            let (id, operation) = &mut self.io[index];
            let id = *id;
            let input = match operation {
                Io::Probe(probe) => match probe.as_mut().poll(cx) {
                    Poll::Pending => None,
                    Poll::Ready(result) => {
                        let number = self.number_of(id);
                        // A miss or a failed probe is fetched normally. Stream
                        // cancellation and deadlines are checked by `step`.
                        let found = match (result, number) {
                            (Ok(Some(page)), Some(number))
                                if self.validate(&page, number).is_ok() =>
                            {
                                self.pages.insert(id, Held::Page(page));
                                true
                            }
                            _ => false,
                        };
                        Some(win::Input::Probed { id, found })
                    }
                },
                Io::Fetch(fetch) => match fetch.as_mut().poll(cx) {
                    Poll::Pending => None,
                    Poll::Ready(result) => Some(self.fetched(id, result.map(Fetched::Page))),
                },
                Io::FetchSealed(fetch) => match fetch.as_mut().poll(cx) {
                    Poll::Pending => None,
                    Poll::Ready(result) => Some(self.fetched(id, result)),
                },
                Io::Select(select) => match select.as_mut().poll(cx) {
                    // The selected page was already offered. Pages the provider
                    // did not send may be selected again; after a failure,
                    // including the selection's own deadline, they are only
                    // filled directly. The stream's cancellation and deadline
                    // are checked by `step`.
                    Poll::Pending => None,
                    Poll::Ready(result) => Some(win::Input::Selected {
                        id,
                        ok: result.is_ok(),
                        started: true,
                    }),
                },
                Io::Hold(number, hold) => match hold.as_mut().poll(cx) {
                    Poll::Pending => None,
                    Poll::Ready(result) => {
                        let _ = number;
                        Some(win::Input::Held {
                            id,
                            result: result
                                .map(|epoch| {
                                    self.next_epoch += 1;
                                    self.epochs.insert(self.next_epoch, epoch);
                                    self.next_epoch
                                })
                                .map_err(|_| read::Error::Unavailable),
                        })
                    }
                },
                Io::SpillFill(fill) => match fill.as_mut().poll(cx) {
                    Poll::Pending => None,
                    Poll::Ready(result) => Some(win::Input::SpillFilled {
                        id,
                        ok: result.is_ok(),
                    }),
                },
            };
            let Some(input) = input else {
                index += 1;
                continue;
            };
            self.io.swap_remove(index);
            self.feed(input, scope);
            changed = true;
        }
        changed
    }

    /// The input for a finished direct fill. A filled plaintext page also goes
    /// to other windows of the version; a sealed page is checked against the
    /// pin now.
    fn fetched(&mut self, id: EffectId, result: Result<Fetched>) -> win::Input {
        let number = self.number_of(id);
        let result = match result {
            Ok(Fetched::Page(page)) => {
                if self.lease.is_some() {
                    self.filled.push(page.clone());
                }
                self.pages.insert(id, Held::Page(page));
                Ok(win::Fetched::Page)
            }
            Ok(Fetched::Sealed(sealed)) => {
                let checked = number
                    .ok_or(Error::StaleFlight)
                    .and_then(|number| self.validate_sealed(&sealed, number));
                match checked {
                    Ok(()) => {
                        self.pages.insert(id, Held::Sealed(sealed));
                        Ok(win::Fetched::Sealed)
                    }
                    Err(error) => Err(error),
                }
            }
            Err(error) => Err(error),
        };
        win::Input::Fetched {
            id,
            result: result.map_err(|error| {
                let page = number.map_or(u64::MAX, |number| number.0);
                self.core_error(page, error)
            }),
        }
    }

    /// The page of the window slot waiting on `id`.
    fn number_of(&self, id: EffectId) -> Option<PageNumber> {
        let window = self.window.as_ref()?;
        let index = window.slots().position(
            |slot| matches!(slot, win::Slot::Probing(i) | win::Slot::Fetching(i) if *i == id),
        )?;
        Some(PageNumber(window.cursor() + index as u64))
    }

    /// Accept pages that any selection of this version offered to this window.
    fn feed_offered(&mut self, scope: &RequestScope) -> bool {
        let mut changed = false;
        while let Some(page) = self.offered.pop_front() {
            let number = page.plaintext.page().number;
            if self.validate(&page, number).is_err() {
                continue;
            }
            let handle = self.ids.allocate();
            self.pages.insert(handle, Held::Page(page.clone()));
            let discarded = self.effects.len();
            self.window();
            let Some(window) = self.window.as_mut() else {
                continue;
            };
            let mut effects = std::mem::take(&mut self.effects);
            let _ = window.on(
                win::Input::Offered {
                    page: number.0,
                    handle,
                },
                true,
                &mut self.ids,
                &mut effects,
            );
            let accepted = !effects[discarded..].contains(&win::Effect::Discard(handle));
            self.apply(&mut effects, scope);
            self.effects = effects;
            if accepted {
                // Selected pages are accepted unobserved on their owner; record
                // this reader's interest once it takes the page.
                self.directory.observe_verified(&self.drivers, page, scope);
                changed = true;
            }
        }
        changed
    }

    /// Only client HTTP delivery may release the initial operation deadline after
    /// acquiring the first slice. Already admitted pages retain their original deadlines
    /// and retry budgets; only newly admitted, distinct pages get child scopes.
    pub(crate) fn enable_progress(&mut self, timeout: Duration) {
        if matches!(self.budget, PageBudget::Client { .. }) {
            self.budget = PageBudget::Progressing { timeout };
            if let Some(window) = self.window.as_mut() {
                window.set_budget(self.budget);
            }
        }
    }
    fn operation_scope(&self) -> RequestScope {
        let mut scope = self.scope.clone();
        if let PageBudget::Progressing { timeout } = self.budget {
            scope.deadline.0 = scope.clock().now() + timeout;
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

    /// End the read: the window releases every hold it owns, pending holds are
    /// released by detached tasks once applied, and all other work is dropped.
    fn terminate(&mut self) {
        if let Some(window) = self.window.as_mut() {
            let mut effects = std::mem::take(&mut self.effects);
            window.terminate(&mut effects);
            let scope = self.scope.clone();
            self.apply(&mut effects, &scope);
            self.effects = effects;
        }
        for (_, operation) in self.io.drain(..) {
            let Io::Hold(number, holding) = operation else {
                continue;
            };
            // A pending hold may still be applied; its completion fence says so.
            let Ok(permit) = self.drivers.reserve() else {
                continue;
            };
            let drivers = self.drivers.clone();
            let directory = self.directory.clone();
            let version = self.metadata.version.clone();
            let request = self.scope.request;
            permit.submit_detached(Box::pin(async move {
                if let Ok(epoch) = holding.await {
                    directory.release_read_pages(&drivers, &version, [(number, epoch)], request);
                }
                Ok::<(), Error>(())
            }));
        }
        self.terminated = true;
        self.pages.clear();
        self.inputs.clear();
        self.offered.clear();
        self.filled.clear();
        self.retained.clear();
        self.lease = None;
        self.credits = None;
    }

    async fn deliver(&mut self) -> Result<Option<DirectItem>> {
        let scope = self.operation_scope();
        let cancellation = scope.cancellation.subscribe()?;
        let next = poll_fn(|cx| -> Poll<Result<win::Next>> {
            cancellation.register(cx.waker());
            self.poll_prefetch(cx);
            if let Some(error) = self.error.take() {
                return Poll::Ready(Err(error));
            }
            let mut effects = std::mem::take(&mut self.effects);
            let next = self.window().next(&mut effects);
            self.apply(&mut effects, &scope);
            self.effects = effects;
            match next {
                win::Next::Wait => Poll::Pending,
                next => Poll::Ready(Ok(next)),
            }
        })
        .await?;
        let (number, handle) = match next {
            win::Next::End => {
                self.terminated = true;
                return Ok(None);
            }
            win::Next::Fail(error) => {
                let cursor = self.window().cursor();
                return Err(match error {
                    read::Error::Internal => {
                        self.failures.remove(&cursor).unwrap_or(Error::Internal)
                    }
                    error => error.into(),
                });
            }
            win::Next::Page { page, handle, .. } => (PageNumber(page), handle),
            win::Next::Wait => return Err(Error::StaleFlight),
        };
        if !self.failures.is_empty() {
            self.failures.retain(|page, _| *page > number.0);
        }
        scope.check()?;
        match self.pages.remove(&handle) {
            Some(Held::Sealed(sealed)) => {
                self.validate_sealed(&sealed, number)?;
                let slice = self.range.slice_at(number)?.ok_or(Error::CorruptRecord)?;
                self.issue(number)?;
                Ok(Some(DirectItem::Sealed(sealed, slice)))
            }
            Some(Held::Page(result)) => {
                self.validate(&result, number)?;
                let slice = self.range.slice_at(number)?.ok_or(Error::CorruptRecord)?;
                self.issue(number)?;
                self.retained.insert(number, result.plaintext.clone());
                self.delivery
                    .attach(result.plaintext, slice)
                    .map(|reader| Some(DirectItem::Plaintext(reader)))
            }
            None => Err(Error::StaleFlight),
        }
    }

    /// Account one page as handed to the client. The window released its
    /// spill hold.
    fn issue(&mut self, number: PageNumber) -> Result<()> {
        self.credits
            .as_mut()
            .ok_or(Error::InvalidRequest)?
            .issued(number)
            .map_err(window_error)
    }

    /// Pages admitted into the window: fetched, being fetched or selected.
    pub fn buffered_pages(&self) -> usize {
        self.window.as_ref().map_or(0, |window| {
            window
                .slots()
                .filter(|slot| {
                    matches!(
                        slot,
                        win::Slot::Fetching(_)
                            | win::Slot::Selecting(_)
                            | win::Slot::Ready { .. }
                            | win::Slot::Failed(_)
                    )
                })
                .count()
        })
    }
}

/// A scripted window slot for tests, with the operation it waits on.
#[cfg(test)]
#[allow(clippy::large_enum_variant)]
enum TestSlot {
    Probing(Operation<'static, Option<PageResult>>),
    Missing {
        select: bool,
    },
    Fetching(Operation<'static, PageResult>),
    FetchingSealed(Operation<'static, Fetched>),
    Sealed(Box<SealedPage>),
    /// Covered by a selection that never completes.
    Selecting,
    Ready(Result<PageResult>),
}

#[cfg(test)]
impl RangeStream {
    /// Place a page in the window with its credit, as `pump` would.
    fn inject(&mut self, slot: TestSlot) {
        let index = self.window().slots().count();
        let number = PageNumber(self.window().cursor() + index as u64);
        assert!(reserve(&self.range, &mut self.credits, number));
        self.place(index, slot);
    }

    /// Replace (or append) window slot `index` with a scripted one.
    fn place(&mut self, index: usize, slot: TestSlot) {
        let number = self.window().cursor() + index as u64;
        let id = self.ids.allocate();
        let slot = match slot {
            TestSlot::Probing(probe) => {
                self.io.push((id, Io::Probe(probe)));
                win::Slot::Probing(id)
            }
            TestSlot::Missing { select } => win::Slot::Missing { select },
            TestSlot::Fetching(fetch) => {
                self.io.push((id, Io::Fetch(fetch)));
                win::Slot::Fetching(id)
            }
            TestSlot::FetchingSealed(fetch) => {
                self.io.push((id, Io::FetchSealed(fetch)));
                win::Slot::Fetching(id)
            }
            TestSlot::Sealed(sealed) => {
                self.pages.insert(id, Held::Sealed(sealed));
                win::Slot::Ready {
                    handle: id,
                    sealed: true,
                }
            }
            TestSlot::Selecting => win::Slot::Selecting(id),
            TestSlot::Ready(Ok(page)) => {
                self.pages.insert(id, Held::Page(page));
                win::Slot::Ready {
                    handle: id,
                    sealed: false,
                }
            }
            TestSlot::Ready(Err(error)) => win::Slot::Failed(self.core_error(number, error)),
        };
        self.window().place(index, slot);
    }

    /// Script one selection over window `pages`, waiting on `operation`.
    fn inject_selection(&mut self, pages: &[u64], operation: Operation<'static, PageResult>) {
        let id = self.ids.allocate();
        let cursor = self.window().cursor();
        for number in pages {
            self.window()
                .place((number - cursor) as usize, win::Slot::Selecting(id));
        }
        self.io.push((id, Io::Select(operation)));
    }

    fn slot(&self, index: usize) -> win::Slot {
        *self.window.as_ref().unwrap().slots().nth(index).unwrap()
    }

    fn slot_count(&self) -> usize {
        self.window
            .as_ref()
            .map_or(0, |window| window.slots().count())
    }

    fn selections(&self) -> usize {
        self.window.as_ref().map_or(0, win::ReadWindow::selections)
    }

    fn direct_ahead(&mut self) -> usize {
        self.window().direct_ahead()
    }

    /// Poll the window's operations once and feed what finished, without
    /// planning new work.
    fn poll_slots(&mut self, cx: &mut Context<'_>) -> bool {
        let scope = self.operation_scope();
        self.poll_io(cx, &scope)
    }

    /// The selections a pump would start now, as their page sets. Their
    /// operations are not started; the planned pages stay selecting.
    fn plan_selections(&mut self) -> Vec<BTreeSet<u64>> {
        let waker = futures::task::noop_waker();
        self.exchange(&Context::from_waker(&waker));
        let now = self.scope.clock().now();
        let facts = self.facts.clone();
        let mut effects = Vec::new();
        self.window();
        let window = self.window.as_mut().unwrap();
        window.pump(now, &facts, |_| false, &mut self.ids, &mut effects);
        effects
            .into_iter()
            .filter_map(|effect| match effect {
                win::Effect::Select { pages, .. } => Some(pages),
                _ => None,
            })
            .collect()
    }

    /// The selectable holes a selection limited to `limit` pages would
    /// demand, excluding pages selected `elsewhere`.
    fn selection_demand(
        &mut self,
        elsewhere: &BTreeSet<u64>,
        limit: usize,
    ) -> Option<(Vec<std::ops::Range<u64>>, BTreeSet<u64>)> {
        let window = self.window();
        let kinds = window.slots().map(win::Slot::kind).collect::<Vec<_>>();
        read::selection_demand(
            &kinds,
            window.cursor(),
            |number| elsewhere.contains(&number),
            limit,
            racer_core::subscriptions::MAX_DEMAND_INTERVALS,
        )
    }
}

/// How a slot shows in the scheduler: wanted, busy, both or neither.
fn interest_class(slot: &win::Slot) -> u8 {
    match slot {
        win::Slot::Probing(_) | win::Slot::Missing { .. } => 1,
        win::Slot::Fetching(_) => 2,
        win::Slot::Selecting(_) => 3,
        win::Slot::Ready { .. } | win::Slot::Failed(_) => 0,
    }
}

/// Reserve the client credit of one window page, if it fits. Window pages plus
/// delivered, unreleased pages never exceed the client's credits, so fetching
/// ahead pins no more plaintext than the client may hold.
fn reserve(
    range: &ResolvedRange,
    credits: &mut Option<Window<PageNumber>>,
    number: PageNumber,
) -> bool {
    let Some(length) = range
        .slice_at(number)
        .ok()
        .flatten()
        .map(|slice| u64::from(slice.length))
    else {
        return false;
    };
    credits.as_mut().is_some_and(|credits| {
        credits.can_reserve(length) && credits.reserve(number, length).is_ok()
    })
}

impl Drop for RangeStream {
    fn drop(&mut self) {
        let terminated = self.terminated;
        if !terminated || !self.io.is_empty() {
            self.terminate();
        }
        // ReaderLease and runtime completion owners retain already submitted I/O.
        // This cancels acquisition only; it cannot revoke another reader's lease.
        if !terminated {
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
    use racer_wire::control::CacheId;
    use racer_wire::control::MembershipVersion;

    struct Fixture {
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        pipes: Rc<flow_control::PipePool<AdmissionPolicy>>,
        streams: RangeStreams,
    }

    impl Fixture {
        fn new(env: &racer_io::Env, limits: crate::config::Limits, capacity: usize) -> Self {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
            let reactor = Rc::new(Reactor::new(admission.clone(), env));
            let pipes = Rc::new(new_pipe_pool(admission.clone(), reactor.sim_os()));
            let directory = std::rc::Rc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    capacity,
                    racer_io::WorkerCtx::host(),
                )
                .unwrap(),
            );
            let streams = RangeStreams::new(
                directory,
                Rc::new(Delivery::new(reactor, Duration::from_secs(30))),
                capacity,
                std::rc::Rc::new(crate::drivers::DriverQueue::new(1024)),
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
    pub(crate) fn page_result(
        admission: &flow_control::Quotas<AdmissionPolicy>,
        metadata: &ObjectMetadata,
        number: u64,
    ) -> PageResult {
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
                    storage: crate::memory::fixture_plaintext(
                        admission,
                        cache,
                        &vec![number as u8; length],
                    ),
                }),
            },
            ciphertext: CiphertextPage {
                pinned: true,
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    pins: Default::default(),
                    envelope: PageEnvelope {
                        page_size: racer_wire::object::PageSize::DEFAULT,
                        cipher: crate::model::PageCipher::Xaes256Gcm,
                        page,
                        key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                        nonce: Nonce([2; 24]),
                        plaintext_length: length as u32,
                        ciphertext_length: length as u32 + 16,
                    },
                    storage: crate::memory::fixture_ciphertext_storage(
                        admission,
                        cache,
                        &vec![0; length + 16],
                    ),
                }),
            },
        }
    }

    #[test]
    fn sealed_reads_fill_their_whole_window_directly() {
        let env = racer_io::Env::host();
        for (window, sealed) in [(1, 1), (2, 1), (8, 7)] {
            let f = Fixture::new(
                &env,
                crate::test_support::cluster::config(false).limits,
                window,
            );
            let mut metadata = metadata();
            metadata.length = 16 * PAGE_BYTES;
            let range = ByteRange::From(0).resolve(metadata.length).unwrap();
            let scope =
                RequestScope::host(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
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
    fn passthrough_reads_fill_their_whole_window_directly() {
        let env = racer_io::Env::host();
        for (window, passthrough) in [(1, 1), (2, 1), (8, 7)] {
            let f = Fixture::new(
                &env,
                crate::test_support::cluster::config(false).limits,
                window,
            );
            let mut metadata = metadata();
            metadata.length = 16 * PAGE_BYTES;
            let range = ByteRange::From(0).resolve(metadata.length).unwrap();
            let scope =
                RequestScope::host(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = f.open(&metadata, range, scope);
            assert_eq!(stream.direct_ahead(), DIRECT_AHEAD);
            stream.set_passthrough();
            assert_eq!(stream.direct_ahead(), passthrough, "window {window}");
        }
    }

    #[test]
    fn sealed_pages_are_delivered_in_order_only_to_direct_readers() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
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
            RequestScope::host(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap()
        };
        let mut stream = f.open(&metadata, range, scope());
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        let first = crate::read::sealed::tests::sealed(&page_result(admission, &metadata, 0));
        stream.inject(TestSlot::Sealed(Box::new(first)));
        stream.inject(TestSlot::Ready(Ok(page_result(admission, &metadata, 1))));
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
        stream.inject(TestSlot::Sealed(Box::new(other)));
        assert!(futures::executor::block_on(stream.next_direct()).is_err());

        // A Get stream never yields sealed pages.
        let mut stream = f.open(&metadata, range, scope());
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        let first = crate::read::sealed::tests::sealed(&page_result(admission, &metadata, 0));
        stream.inject(TestSlot::Sealed(Box::new(first)));
        assert_eq!(
            futures::executor::block_on(stream.next_slice()).err(),
            Some(Error::Internal)
        );
    }

    #[test]
    fn retained_boundary_slices_keep_full_plaintext_charged_until_exact_release() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let admission = &f.admission;
        let mut metadata = metadata();
        metadata.length = 2 * PAGE_BYTES;
        let range = ByteRange::Closed {
            first: PAGE_BYTES - 1,
            last: PAGE_BYTES,
        }
        .resolve(metadata.length)
        .unwrap();
        let scope =
            RequestScope::host(RequestId([9; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap();
        let mut stream = f.open(&metadata, range, scope);
        stream.configure_subscription(4, PAGE_BYTES).unwrap();
        for number in 0..2 {
            stream.inject(TestSlot::Ready(Ok(page_result(
                admission, &metadata, number,
            ))));
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
        // Unreleased leases remain charged.
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
        assert_eq!(
            admission.used(ResourceClass::Plaintext),
            PAGE_BYTES as usize
        );
        futures::executor::block_on(stream.cancel()).unwrap();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert!(stream.retained.is_empty());
    }

    #[test]
    fn optional_remote_observation_never_blocks_ready_delivery_or_duplicates_on_repoll() {
        let env = racer_io::Env::host();
        for repoll in [false, true] {
            let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
            let queue = f.streams.drivers.clone();
            let directory = f.streams.directory();
            assert_eq!(directory.test_observation_mailbox(), 0);
            let mut metadata = metadata();
            metadata.length = 3;
            let scope = RequestScope::host(
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
            stream.inject(TestSlot::Selecting);
            stream.exchange(&cx);
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
                assert!(matches!(stream.slot(0), win::Slot::Ready { .. }));
            }
            // No endpoint is ever polled. A continuous next_slice future must
            // still deliver immediately with the accepted hint unanswered.
            let mut next = stream.next_slice();
            let reader = match next.as_mut().poll(&mut cx) {
                Poll::Ready(Ok(Some(reader))) => reader,
                other => panic!(
                    "optional owner receipt blocked ready page (repoll {repoll}): {:?}",
                    other.map(|result| result.map(|reader| reader.is_some()))
                ),
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
        let env = racer_io::Env::host();
        {
            for mode in ["release", "cancel", "drop"] {
                let mut limits = crate::test_support::cluster::config(false).limits;
                limits.pipes = std::num::NonZeroUsize::new(1).unwrap();
                let f = Fixture::new(&env, limits, 2);
                let admission = &f.admission;
                let pipes = &f.pipes;
                let metadata = metadata();
                let scope = RequestScope::host(
                    RequestId([8; 16]),
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
                let range = ByteRange::From(PAGE_BYTES - 1)
                    .resolve(metadata.length)
                    .unwrap();
                let mut stream = f.open(&metadata, range, scope.clone());
                stream.configure_subscription(2, PAGE_BYTES).unwrap();
                for number in 0..2 {
                    stream.inject(TestSlot::Ready(Ok(page_result(
                        admission, &metadata, number,
                    ))));
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
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            }
        }
    }
    #[test]
    fn credit_starved_stream_never_holds_pipe_and_cancel_detaches_demand() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 1);
        let admission = &f.admission;
        let pipes = &f.pipes;
        let directory = &f.streams.directory;
        let metadata = metadata();
        let range = ByteRange::From(0).resolve(metadata.length).unwrap();
        let scope =
            RequestScope::host(RequestId([7; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap();
        let mut stream = f.open(&metadata, range, scope.clone());
        stream.configure_subscription(1, PAGE_BYTES).unwrap();
        stream.inject(TestSlot::Ready(Ok(page_result(admission, &metadata, 0))));
        // Model a delivered page still owned by a slow caller.
        let first = futures::executor::block_on(stream.next_slice())
            .unwrap()
            .unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert_eq!(stream.slot_count(), 0, "no credit admits no window page");
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
        let budget = PageBudget::Client { deadline };
        // Model a healthy remote page that spends a four-link route and transfers
        // acquisition credits to its destination. Neither spend can accumulate
        // into a range-length limit, and unused credits cannot grow later pages.
        for _ in 0..100 {
            let mut page = budget.next_page(racer_io::Clock::host().now()).unwrap();
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
        }
        let mut page = budget.next_page(racer_io::Clock::host().now()).unwrap();
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
        let expired = PageBudget::Client {
            deadline: Instant::now(),
        };
        assert!(matches!(
            expired.next_page(racer_io::Clock::host().now()),
            Err(racer_core::read::Error::DeadlineExceeded)
        ));
    }
    #[test]
    fn pending_client_window_never_restarts_failed_pages_and_honors_original_deadline() {
        use std::cell::Cell;
        use std::time::Duration;
        let env = racer_io::Env::host();
        for expire in [false, true] {
            let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
            let mut metadata = metadata();
            metadata.length = 100 * PAGE_BYTES;
            let scope =
                RequestScope::host(RequestId([1; 16]), Instant::now() + Duration::from_secs(60))
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
            for _ in 0..2 {
                let mut budget = stream
                    .budget
                    .next_page(racer_io::Clock::host().now())
                    .unwrap();
                let attempts = attempts.clone();
                let gate = gate.clone();
                stream.inject(TestSlot::Fetching(Box::pin(async move {
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
                    budget
                        .begin_attempt(Instant::now(), budget.deadline())
                        .map(|_| unreachable!("same page received fresh retry credits"))
                        .map_err(Error::from)
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
                assert_eq!((stream.window().cursor(), stream.slot_count()), (0, 2));
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
        budget: PageBudget,
    ) -> RangeStream {
        let scope =
            RequestScope::host(RequestId([5; 16]), Instant::now() + Duration::from_secs(60))
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
    /// Sealed (GPU) fills retry `Overloaded` and `Unavailable` like plaintext fills instead of
    /// ending the read on the first transient failure.
    #[test]
    fn transient_sealed_fill_failures_are_refetched_until_retries_run_out() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(&f, &metadata(), PageBudget::Client { deadline });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let failing =
            |error| -> TestSlot { TestSlot::FetchingSealed(Box::pin(async move { Err(error) })) };
        stream.inject(failing(Error::Overloaded));
        assert!(stream.poll_slots(&mut cx));
        assert_eq!(stream.slot(0), win::Slot::Missing { select: false });
        for _ in 1..PAGE_RETRIES {
            stream.place(0, failing(Error::Unavailable));
            stream.poll_slots(&mut cx);
            assert_eq!(stream.slot(0), win::Slot::Missing { select: false });
        }
        stream.place(0, failing(Error::Overloaded));
        stream.poll_slots(&mut cx);
        assert_eq!(stream.slot(0), win::Slot::Failed(read::Error::Overloaded));
        // A non-transient failure is not retried.
        let mut stream = stream_with_budget(&f, &metadata(), PageBudget::Client { deadline });
        stream.inject(failing(Error::OriginForbidden));
        stream.poll_slots(&mut cx);
        assert_eq!(
            stream.slot(0),
            win::Slot::Failed(read::Error::OriginForbidden)
        );
    }
    #[test]
    fn transient_fill_failures_are_refetched_until_retries_run_out() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(&f, &metadata(), PageBudget::Client { deadline });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let failing =
            |error| -> TestSlot { TestSlot::Fetching(Box::pin(async move { Err(error) })) };
        stream.inject(failing(Error::Overloaded));
        stream.poll_slots(&mut cx);
        assert_eq!(stream.slot(0), win::Slot::Missing { select: false });
        for _ in 1..PAGE_RETRIES {
            stream.place(0, failing(Error::Unavailable));
            stream.poll_slots(&mut cx);
            assert_eq!(stream.slot(0), win::Slot::Missing { select: false });
        }
        // The page's retries are spent: the failure now reaches the reader.
        stream.place(0, failing(Error::Overloaded));
        stream.poll_slots(&mut cx);
        assert_eq!(stream.slot(0), win::Slot::Failed(read::Error::Overloaded));
    }

    #[test]
    fn non_transient_fill_failures_are_not_refetched() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(&f, &metadata(), PageBudget::Client { deadline });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        stream.inject(TestSlot::Fetching(Box::pin(async move {
            Err(Error::CorruptRecord)
        })));
        stream.poll_slots(&mut cx);
        assert_eq!(
            stream.slot(0),
            win::Slot::Failed(read::Error::CorruptRecord)
        );
    }

    #[test]
    fn abandoning_window_drops_waiters() {
        let env = racer_io::Env::host();
        struct Dropped(Rc<std::cell::Cell<bool>>);
        impl Drop for Dropped {
            fn drop(&mut self) {
                self.0.set(true);
            }
        }
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut metadata = metadata();
        metadata.length = 3;
        let mut stream = stream_with_budget(&f, &metadata, PageBudget::Client { deadline });
        let dropped = Rc::new(std::cell::Cell::new(false));
        let guard = Dropped(dropped.clone());
        stream.inject(TestSlot::Fetching(Box::pin(async move {
            let _guard = guard;
            std::future::pending::<()>().await;
            Err(Error::Cancelled)
        })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        futures::executor::block_on(stream.cancel()).unwrap();
        assert!(dropped.get());
    }
    #[test]
    fn later_failure_still_fills_earlier_pages_and_surfaces_in_order() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(&f, &metadata(), PageBudget::Client { deadline });
        // The head probe misses only after the later page has already failed.
        let gate = Rc::new(std::cell::Cell::new(false));
        let opened = gated(gate.clone());
        stream.inject(TestSlot::Probing(Box::pin(async move {
            opened.await;
            Ok(None)
        })));
        stream.inject(TestSlot::Fetching(Box::pin(async move {
            Err(Error::VersionUnavailable)
        })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            stream.slot(1),
            win::Slot::Failed(read::Error::VersionUnavailable)
        );
        gate.set(true);
        stream.poll_prefetch(&mut cx);
        // The missed head gets a direct fill despite the later failure (this
        // fixture has no worker, so starting it may fail at once), and nothing
        // is admitted at or past the failed page.
        assert!(
            matches!(
                stream.slot(0),
                win::Slot::Fetching(_)
                    | win::Slot::Failed(_)
                    | win::Slot::Missing { select: false }
            ),
            "the head must not be left without work"
        );
        assert_eq!(
            stream.slot(1),
            win::Slot::Failed(read::Error::VersionUnavailable)
        );
        assert_eq!(stream.selections(), 0);
        // Once the head arrives it is delivered, then the failure ends the stream.
        stream.place(
            0,
            TestSlot::Ready(Ok(page_result(&f.admission, &metadata(), 0))),
        );
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
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let metadata = metadata();
        let deadline = Instant::now() + Duration::from_secs(60);
        let mut stream = stream_with_budget(&f, &metadata, PageBudget::Client { deadline });
        let gate = Rc::new(std::cell::Cell::new(false));
        let opened = gated(gate.clone());
        let head = page_result(&f.admission, &metadata, 0);
        stream.inject(TestSlot::Fetching(Box::pin(async move {
            opened.await;
            Ok(head)
        })));
        let hit = page_result(&f.admission, &metadata, 1);
        stream.inject(TestSlot::Probing(Box::pin(async move { Ok(Some(hit)) })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        // The memory hit is ready ahead of the head and started no fill.
        assert!(matches!(stream.slot(1), win::Slot::Ready { .. }));
        assert_eq!(stream.selections(), 0);
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
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let metadata = metadata();
        let scope =
            RequestScope::host(RequestId([6; 16]), Instant::now() + Duration::from_secs(60))
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
        stream.inject(TestSlot::Fetching(Box::pin(async move {
            opened.await;
            Ok(head)
        })));
        stream.inject(TestSlot::Fetching(Box::pin(async move { Ok(later) })));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(stream.next_slice().as_mut().poll(&mut cx).is_pending());
        assert!(matches!(stream.slot(1), win::Slot::Ready { .. }));
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
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 4);
        let metadata = metadata();
        let mut streams = (0..2)
            .map(|_| {
                let mut stream = f.open(
                    &metadata,
                    ByteRange::From(0).resolve(metadata.length).unwrap(),
                    RequestScope::host(
                        RequestId([4; 16]),
                        Instant::now() + Duration::from_secs(60),
                    )
                    .unwrap(),
                );
                stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
                stream.inject(TestSlot::Ready(Ok(page_result(&f.admission, &metadata, 0))));
                stream.inject(TestSlot::Selecting);
                stream
            })
            .collect::<Vec<_>>();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for stream in &mut streams {
            stream.poll_prefetch(&mut cx);
            assert!(matches!(stream.slot(1), win::Slot::Selecting(_)));
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
            assert!(matches!(stream.slot(1), win::Slot::Ready { .. }));
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
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
        let metadata = metadata();
        for error in [
            Error::Unavailable,
            Error::Overloaded,
            Error::Cancelled,
            Error::DeadlineExceeded,
        ] {
            let scope =
                RequestScope::host(RequestId([3; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap();
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(metadata.length).unwrap(),
                scope.clone(),
            );
            stream.configure_subscription(2, 2 * PAGE_BYTES).unwrap();
            stream.inject(TestSlot::Ready(Ok(page_result(&f.admission, &metadata, 0))));
            stream.inject(TestSlot::Missing { select: true });
            stream.inject_selection(&[1], Box::pin(async move { Err(error) }));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            // A selection's own failure, even of its child deadline, is not
            // the stream's.
            assert!(stream.poll_slots(&mut cx));
            assert_eq!(stream.selections(), 0);
            assert_eq!(stream.slot(1), win::Slot::Missing { select: false });
            // The page is no longer offered for selection, only for a direct fill.
            assert!(
                stream
                    .selection_demand(&BTreeSet::new(), usize::MAX)
                    .is_none()
            );
            // Canceling the stream itself is fatal.
            scope.cancel().unwrap();
            stream.poll_prefetch(&mut cx);
            assert_eq!(stream.error, Some(Error::Cancelled));
        }
    }
    #[test]
    fn spill_stays_off_by_default() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 4);
        let mut metadata = metadata();
        metadata.length = 4 * PAGE_BYTES;
        let mut stream = f.open(
            &metadata,
            ByteRange::From(0).resolve(metadata.length).unwrap(),
            RequestScope::host(RequestId([2; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap(),
        );
        stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        assert_eq!(stream.spill, 0);
    }
    #[test]
    fn passthrough_stream_issues_no_selection_and_never_spills() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 4);
        f.streams
            .directory
            .configure_spill(crate::read::spill::Config {
                range_spill_pages: 4,
                ..Default::default()
            })
            .unwrap();
        let mut metadata = metadata();
        metadata.length = 4 * PAGE_BYTES;
        for passthrough in [false, true] {
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(metadata.length).unwrap(),
                RequestScope::host(RequestId([3; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap(),
            );
            if passthrough {
                stream.set_passthrough();
            }
            stream.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
            assert_eq!(stream.spill == 0, passthrough);
            for _ in 0..4 {
                stream.inject(TestSlot::Missing { select: true });
            }
            // Selections ask providers for retained copies; passthrough
            // pages are each fetched through their owner's flight instead.
            assert_eq!(
                stream.plan_selections().is_empty(),
                passthrough,
                "passthrough={passthrough}"
            );
        }
    }
    #[test]
    fn read_keeps_disjoint_selections_in_flight_up_to_spare_granted_flights() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 10);
        let mut metadata = metadata();
        metadata.length = 10 * PAGE_BYTES;
        let open = || {
            let mut stream = f.open(
                &metadata,
                ByteRange::From(0).resolve(metadata.length).unwrap(),
                RequestScope::host(RequestId([5; 16]), Instant::now() + Duration::from_secs(60))
                    .unwrap(),
            );
            stream.configure_subscription(10, 10 * PAGE_BYTES).unwrap();
            for _ in 0..10 {
                stream.inject(TestSlot::Missing { select: true });
            }
            stream
        };
        // The head and `DIRECT_AHEAD` pages are filled directly; selections
        // cover the rest. Before any provider grants more than one flight, a
        // read runs one selection over every selectable hole.
        assert_eq!(DIRECT_AHEAD, 1);
        let mut stream = open();
        assert_eq!(stream.plan_selections(), vec![BTreeSet::from_iter(2..10)]);
        drop(stream);
        // A grant of several flights on a contract of the version lets the
        // read split its holes over several concurrent selections.
        let scheduler = &f.streams.directory.subscriptions;
        let provider = racer_wire::control::NodeId("provider".into());
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
        let pages = stream.plan_selections();
        assert_eq!(pages.len(), SELECTIONS);
        assert_eq!(
            pages,
            vec![
                BTreeSet::from([2, 3]),
                BTreeSet::from([4, 5]),
                BTreeSet::from([6, 7]),
                BTreeSet::from([8, 9]),
            ]
        );
        assert!((2..10).all(|index| matches!(stream.slot(index), win::Slot::Selecting(_))));
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
        assert_eq!(stream.plan_selections(), vec![BTreeSet::from_iter(2..10)]);
        drop(held);
    }
    #[test]
    fn selection_demand_covers_only_selectable_holes_not_selected_elsewhere() {
        let env = racer_io::Env::host();
        let f = Fixture::new(&env, crate::test_support::cluster::config(false).limits, 8);
        let mut metadata = metadata();
        metadata.length = 8 * PAGE_BYTES;
        let mut stream = f.open(
            &metadata,
            ByteRange::From(0).resolve(metadata.length).unwrap(),
            RequestScope::host(RequestId([2; 16]), Instant::now() + Duration::from_secs(60))
                .unwrap(),
        );
        stream.configure_subscription(8, 8 * PAGE_BYTES).unwrap();
        stream.inject(TestSlot::Fetching(Box::pin(std::future::pending())));
        stream.inject(TestSlot::Ready(Ok(page_result(&f.admission, &metadata, 1))));
        stream.inject(TestSlot::Missing { select: true });
        stream.inject(TestSlot::Missing { select: false });
        for _ in 4..8 {
            stream.inject(TestSlot::Missing { select: true });
        }
        let (intervals, pages) = stream
            .selection_demand(&BTreeSet::from([5]), usize::MAX)
            .unwrap();
        assert_eq!(intervals, [2..3, 4..5, 6..8]);
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
            let clock = racer_io::SimClock::new(55);
            let environment = clock.ctx(0);
            let env = racer_io::Env::new(racer_io::Driver::Uring, environment.clone());
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let reactor = Rc::new(Reactor::new(admission.clone(), &env));
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
                racer_wire::control::canonical_socket_paths("framing").unwrap();
            let worker = crate::test_support::ReadWorker::new(
                &env,
                racer_wire::control::CacheDefinition {
                    id: metadata.version.object.cache.clone(),
                    name: "framing".into(),
                    client_socket,
                    origin_socket,
                    priority: racer_wire::control::DEFAULT_PRIORITY,
                    mode: racer_wire::control::CacheMode::Cached,
                    origin: None,
                },
                metadata.clone(),
                admission.clone(),
                reactor.clone(),
                delivery.clone(),
                4,
            );
            let scope = RequestScope::on(
                &env.ctx.clock,
                RequestId([7; 16]),
                env.ctx.now()
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
                // Pages admitted before the headers keep the original deadline,
                // and with four credits that is every page of this range. Their
                // fetches take real time, so simulated time only moves once all of
                // them have completed: the origin served every page and the worker
                // replied to every command. The poll above has then made each
                // window slot ready. Only delivery remains, which must outlive the
                // original deadline.
                if !crossed.load(std::sync::atomic::Ordering::Acquire)
                    && headers_sent.load(std::sync::atomic::Ordering::Acquire)
                    && worker
                        .origin
                        .completed(racer_wire::object::test_util::RequestKind::PinnedGet)
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
            if progressing {
                assert!(
                    env.ctx.now() > scope.deadline.0,
                    "stream must cross the scaled old absolute deadline"
                );
            }
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn progressing_page_children_keep_deadlines_and_retry_limits_across_long_streams() {
        let clock = racer_io::SimClock::new(81);
        let sim_env = clock.ctx(0);
        let sim_ctx = sim_env.clone();
        let timeout = Duration::from_millis(30);
        let old_deadline = sim_ctx.clock.now() + timeout;
        let budget = PageBudget::Progressing { timeout };
        for _ in 0..12 {
            let mut page = budget.next_page(sim_ctx.clock.clone().now()).unwrap();
            let deadline = page.deadline();
            assert_eq!(deadline, sim_ctx.clock.now() + timeout);
            for _ in 0..8 {
                page.begin_attempt(sim_ctx.clock.now(), deadline).unwrap();
            }
            assert_eq!(
                page.begin_attempt(sim_ctx.clock.now(), deadline),
                Err(racer_core::read::Error::Unavailable)
            );
            clock.advance(Duration::from_millis(20));
            assert_eq!(page.deadline(), deadline);
        }
        assert!(sim_ctx.clock.now() > old_deadline);
        let mut stalled = budget.next_page(sim_ctx.clock.clone().now()).unwrap();
        clock.advance(timeout);
        assert_eq!(
            stalled.begin_attempt(sim_ctx.clock.now(), stalled.deadline()),
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
        use racer_core::subscriptions::Demand as WireDemand;
        use racer_wire::control::CacheId;
        use racer_wire::control::MembershipVersion;
        use racer_wire::control::NodeId;
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
            let scheduler = Scheduler::new(4, racer_io::WorkerCtx::host());
            let deadline = racer_io::Clock::host().now() + std::time::Duration::from_secs(30);
            let (first, one) = issue(&scheduler, 0, "provider", Some(4), deadline);
            let (one, _) = one.unwrap();
            // Until the provider grants more, a second concurrent selection
            // from the same provider waits for the first; another provider is
            // independent.
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
            let scheduler = Scheduler::new(4, racer_io::WorkerCtx::host());
            let deadline = racer_io::Clock::host().now() + std::time::Duration::from_secs(30);
            let provider = NodeId("provider".into());
            let (first, result) = issue(&scheduler, 0, "provider", Some(3), deadline);
            result.unwrap();
            assert_eq!(scheduler.spare(&version()), 0);
            // A grant above the request, or from another provider, is clamped
            // or ignored.
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
            let scheduler = Scheduler::new(4, racer_io::WorkerCtx::host());
            let deadline = racer_io::Clock::host().now() + std::time::Duration::from_millis(20);
            let (first, result) = issue(&scheduler, 0, "provider", Some(2), deadline);
            let (old, _) = result.unwrap();
            scheduler.granted(&first, &NodeId("provider".into()), 2);
            std::thread::sleep(std::time::Duration::from_millis(30));
            let later = racer_io::Clock::host().now() + std::time::Duration::from_secs(30);
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
            let scheduler = Scheduler::new(2, racer_io::WorkerCtx::host());
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
            let scheduler = Scheduler::new(4, racer_io::WorkerCtx::host());
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
            let scheduler = Scheduler::new(1, racer_io::WorkerCtx::host());
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
            let scheduler = Scheduler::new(2, racer_io::WorkerCtx::host());
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
                RequestScope::host(
                    crate::model::RequestId([1; 16]),
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap(),
            )
        }
        #[test]
        fn partial_final_page_uses_exact_bytes_and_completion_does_not_require_release() {
            let env = racer_io::Env::host();
            let f =
                super::Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
            let mut metadata = super::metadata();
            metadata.length = PAGE_BYTES + 5;
            let range = crate::model::ByteRange::From(PAGE_BYTES - 3)
                .resolve(metadata.length)
                .unwrap();
            let mut reader = stream(&f, &metadata, range);
            reader.configure_subscription(2, PAGE_BYTES).unwrap();
            for number in 0..2 {
                reader.inject(TestSlot::Ready(Ok(super::page_result(
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
            let env = racer_io::Env::host();
            let f =
                super::Fixture::new(&env, crate::test_support::cluster::config(false).limits, 2);
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
                reader.inject(TestSlot::Ready(Ok(super::page_result(
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
        fn small_page_credits_are_bounded_by_the_range_page_size() {
            let env = racer_io::Env::host();
            let f =
                super::Fixture::new(&env, crate::test_support::cluster::config(false).limits, 1);
            let size = racer_wire::object::PageSize::new(64 << 10).unwrap();
            let metadata = super::metadata();
            let range = crate::model::ByteRange::From(0)
                .resolve_paged(1, size)
                .unwrap();
            // One small page of credit is enough; less is not.
            let mut reader = stream(&f, &metadata, range);
            assert_eq!(
                reader.configure_subscription(1, size.get() - 1),
                Err(Error::InvalidRequest)
            );
            reader.configure_subscription(1, size.get()).unwrap();
            drop(reader);
            // Credits sized for the largest page stay accepted.
            let mut reader = stream(&f, &metadata, range);
            reader.configure_subscription(4, 4 * PAGE_BYTES).unwrap();
        }
        #[test]
        fn invalid_credit_contracts_never_admit_demand() {
            let env = racer_io::Env::host();
            let f =
                super::Fixture::new(&env, crate::test_support::cluster::config(false).limits, 1);
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
