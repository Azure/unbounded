//! Worker-owned singleflight keyed exactly by page identity, with bounded waiters.
//!
//! Lifecycle contract:
//! - join/wait elects one acquisition caller; other callers independently wait.
//! - publish shares a complete, validated PageResult, never a request context.
//! - origin rejection fails only its supplying caller; another live acquisition
//!   waiter can be elected with its own context and unreset budget. Copy waiters
//!   cannot be elected. Other terminal failures wake all current waiters.
//! - cancellation/drop detaches one caller. Abandoned leadership enters Draining;
//!   only actual I/O/crypto completion fences permit RetryPending or removal.
//! - removal requires no waiters AND no retained operations. A replacement entry
//!   gets a new incarnation; every acquisition/retry gets a new generation.
//!
//! The worker table owns operations, reservations, and completion fences,
//! independently of futures/tokens. Dropping a token must schedule detach/abandon
//! on that worker, never free live resources. Register/recheck wakeups before
//! parking. Bound entries, waiters, retry attempts, and retained completions.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::AcquiredPage;
use crate::memory::PageResult;
use crate::memory::UnverifiedPage;
use crate::model::PageId;
use crate::runtime::RequestScope;
use crate::security::OriginContext;
use flow_control::coalesce;
use std::cell::RefCell;
use std::future::poll_fn;
use std::rc::Rc;
use std::task::Context;
use std::task::Poll;
use std::task::Waker;
use std::time::Instant;

pub struct Flights {
    pub(super) admission: Rc<flow_control::Quotas<AdmissionPolicy>>,

    availability: Rc<crate::control::Availability>,

    owner: Rc<()>,

    limits: FlightLimits,

    pub(super) table: RefCell<Table>,

    waiting: uring_runtime::offload::AdmissionQueue<PageId>,

    admission_deadlines: Rc<uring_runtime::environment::Registry>,

    capacity_wake: CapacityWake,
}

/// The current FIFO head alone owns this notification.
type CapacityWake = Rc<RefCell<Option<(u64, Waker)>>>;

/// Retire only this ticket's wake before its queue guard can wake a successor.
struct AdmissionTicket<'a> {
    ticket: Option<uring_runtime::offload::CapacityWaiter<'a, PageId>>,

    wake: CapacityWake,

    flights: &'a Flights,
}

impl Drop for AdmissionTicket<'_> {
    fn drop(&mut self) {
        let retired = {
            let mut wake = self.wake.borrow_mut();
            if wake
                .as_ref()
                .is_some_and(|(id, _)| *id == self.ticket.as_ref().unwrap().sequence())
            {
                wake.take()
            } else {
                None
            }
        };
        drop(retired);
        drop(self.ticket.take());
        self.flights
            .update(|table, wakes| table.notify_drain(wakes));
    }
}

/// Return real Flight credit before notifying the local admission head.
pub(super) struct FlightCharge {
    charge: Option<flow_control::Charge<AdmissionPolicy>>,

    wake: CapacityWake,
}

impl Drop for FlightCharge {
    fn drop(&mut self) {
        drop(self.charge.take());
        let wake = self.wake.borrow_mut().take();
        if let Some((_, wake)) = wake {
            wake.wake();
        }
    }
}

/// Local caps in addition to shared admission quotas. All dimensions must be nonzero.
#[derive(Clone, Copy, Debug)]
pub struct FlightLimits {
    pub entries: usize,
    pub waiters_per_flight: usize,
    pub operations_per_flight: usize,
    pub generations_per_flight: u64,
}
impl Default for FlightLimits {
    fn default() -> Self {
        Self {
            entries: 1024,
            waiters_per_flight: 256,
            operations_per_flight: 64,
            generations_per_flight: 1024,
        }
    }
}

#[cfg(not(test))]
type FlightHashState = std::collections::hash_map::RandomState;
#[cfg(test)]
type FlightHashState = crate::runtime::HashState;
pub(super) type Table = coalesce::flight::Table<PageId, Entry, FlightHashState>;
pub(super) struct Entry {
    created: Instant,

    diagnostic: Option<DriverDiagnostic>,

    fence: Fence,
    state: FlightCore,
    operations: coalesce::flight::Operations<Retained, FlightHashState>,
    _reservation: FlightCharge,
}

/// Driver-owned call boundary, not a backend execution state.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum DriverStage {
    /// Driver handoff has occurred, but no observed call has started.
    Starting,
    /// No instrumented call is active; synchronous admission or bookkeeping may run.
    BetweenCalls,
    /// Store read call, including allocation and reclamation inside that call.
    DiskReadCall,
    /// Serial decrypt call, including admission and completion delivery.
    DecryptCall,
    /// Serial checksum call, including admission and completion delivery.
    ChecksumCall,
    /// Composite candidate work, including concurrent hedges and nested validation.
    CandidateCompositeCall,
    /// Plaintext reservation and bounded reclamation.
    PlaintextReclaim,
    /// Origin call, not evidence that network I/O has been submitted.
    OriginCall,
    /// Serial encrypt call, including admission and completion delivery.
    EncryptCall,
    /// Publication, including metadata-owner calls.
    PublishCall,
    /// Driver observed an outcome; resource destruction and fences can remain.
    WorkReturned,
}

/// One generation's scalar observation, allocated once at driver handoff.
#[derive(Clone)]
pub(super) struct DriverDiagnostic(Rc<std::cell::Cell<DriverObservation>>);

/// Facts observed by this driver only, never inferred from lifecycle state.
#[derive(Clone, Copy)]
struct DriverObservation {
    stage: DriverStage,

    since: Instant,

    cancel_seen: Option<Instant>,

    abandoned: bool,
}

/// A serial call resets its label even on error or future destruction.
pub(super) struct DriverCall<'a> {
    diagnostic: &'a DriverDiagnostic,

    stage: DriverStage,
}

impl Drop for DriverCall<'_> {
    fn drop(&mut self) {
        if self.diagnostic.0.get().stage == self.stage {
            self.diagnostic.set(DriverStage::BetweenCalls);
        }
    }
}

/// Driver destruction is abandonment unless an outcome was observed.
pub(super) struct DriverLifetime(Option<DriverDiagnostic>);

impl Drop for DriverLifetime {
    fn drop(&mut self) {
        if let Some(diagnostic) = &self.0 {
            let mut state = diagnostic.0.get();
            if state.stage != DriverStage::WorkReturned {
                state.abandoned = true;
                diagnostic.0.set(state);
            }
        }
    }
}

impl DriverDiagnostic {
    /// Record only an actual call transition, never a poll or inferred backend phase.
    pub(super) fn set(&self, stage: DriverStage) {
        let mut state = self.0.get();
        state.stage = stage;
        state.since = uring_runtime::environment::now();
        self.0.set(state);
    }

    /// Scope a serial call without allocations, table borrows, or resource actions.
    pub(super) fn call(&self, stage: DriverStage) -> DriverCall<'_> {
        self.set(stage);
        DriverCall {
            diagnostic: self,
            stage,
        }
    }

    /// Latch the first observed request to cancel this driver's owned scope.
    pub(super) fn cancellation_seen(&self) {
        let mut state = self.0.get();
        state
            .cancel_seen
            .get_or_insert_with(uring_runtime::environment::now);
        self.0.set(state);
    }

    /// Own only diagnostic state until detached driver destruction.
    pub(super) fn lifetime(diagnostic: Option<Self>) -> DriverLifetime {
        DriverLifetime(diagnostic)
    }
}
type FlightCore =
    coalesce::flight::state::State<PageResult, UnverifiedPage, AcquiredPage, WaiterRecord>;
impl std::ops::Deref for Entry {
    type Target = FlightCore;
    fn deref(&self) -> &Self::Target {
        &self.state
    }
}
impl std::ops::DerefMut for Entry {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.state
    }
}
pub(super) struct WaiterRecord {
    scope: RequestScope,
    pub(super) budget_deadline: Instant,
    _reservation: flow_control::Charge<AdmissionPolicy>,
}
impl coalesce::flight::state::WaiterPolicy for WaiterRecord {
    type Error = Error;
    fn check(&self) -> Option<Error> {
        self.scope.check().err().or_else(|| {
            (uring_runtime::environment::now() >= self.budget_deadline)
                .then_some(Error::DeadlineExceeded)
        })
    }
    fn deadline(&self) -> Instant {
        self.scope.deadline.0.min(self.budget_deadline)
    }
}
struct Retained {
    _resources: telemetry::Lease,
    _reservation: flow_control::Charge<AdmissionPolicy>,
}
pub(super) type Outcome = coalesce::flight::state::Outcome<AcquiredPage, Error>;
pub(super) type Phase = coalesce::flight::state::Phase<PageResult, AcquiredPage, Error>;
pub(super) trait PhaseState {
    fn state(&self) -> FlightState;
}
impl PhaseState for Phase {
    fn state(&self) -> FlightState {
        match self {
            Self::Acquiring => FlightState::Acquiring,
            Self::RetryPending => FlightState::RetryPending,
            Self::Draining(_) => FlightState::Draining,
            Self::Complete(_) => FlightState::Complete,
            Self::Failed(_) => FlightState::Failed,
        }
    }
}

/// Transfer this token to the actual completion owner before submitting work.
/// `complete` is called only after completion (or confirmed non-submission).
/// Drop deliberately does NOT release resources or clear the generation fence.
/// Losing a token retains its slot until the table itself is destroyed; shutdown
/// must continue owning the table until `drain` succeeds.
pub struct FlightOperation {
    flights: Rc<Flights>,
    pub(super) fence: Fence,
    pub(super) id: u64,
}
impl FlightOperation {
    pub fn complete(self) -> Result<()> {
        self.flights.complete_operation(&self.fence, self.id)
    }

    /// Allows a completion owner to observe revoked leadership and request actual
    /// transport cancellation. Cancellation is not itself a completion fence.
    pub fn cancellation_requested(&self) -> bool {
        let table = self.flights.table.borrow();
        table.is_stopping()
            || table.get(&self.fence.page).is_none_or(|entry| {
                self.fence.validate(&entry.fence).is_err()
                    || !matches!(entry.phase, Phase::Acquiring)
                    || entry
                        .leader
                        .and_then(|id| entry.waiters.get(&id))
                        .is_none_or(|waiter| waiter.scope.check().is_err())
            })
    }
}

/// Original acquisition budget, owned by one metadata/bootstrap operation or
/// page acquisition. Client range progress admits distinct pages with separate
/// bounded allowances under the same deadline. Retry never reconstructs an
/// acquisition budget from defaults. Fanout must partition credits;
/// route attempts must charge actual forwarded links here as well as on the wire.
/// No Clone: a replacement caller uses its own remaining credits, not fresh ones.
/// ```compile_fail
/// use racer_dataplane::read::flight::AcquisitionBudget;
/// fn duplicate(budget: AcquisitionBudget) { let _other = budget.clone(); }
/// ```
pub struct AcquisitionBudget {
    deadline: Instant,
    attempts: u32,
    links: u8,
    charged_links: u16,
    failed_route: bool,
}
impl AcquisitionBudget {
    pub fn new(deadline: Instant, attempts: u32, links: u8) -> Self {
        Self {
            deadline,
            attempts,
            links,
            charged_links: 0,
            failed_route: false,
        }
    }

    /// Pure budget validation/debit. The driver also checks original cancellation
    /// and candidate authority before I/O. Return the tighter original deadline.
    pub fn begin_attempt(&mut self, now: Instant, caller_deadline: Instant) -> Result<Instant> {
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        self.attempts = self.attempts.checked_sub(1).ok_or(Error::Unavailable)?;
        Ok(deadline)
    }

    pub fn charge_links(&mut self, links: u8) -> Result<()> {
        self.links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.charged_links = self.charged_links.saturating_add(u16::from(links));
        Ok(())
    }

    /// Reconcile unused links only after a verified completed route. Refunds
    /// cannot exceed prior debits; failed exchanges must retain their full debit.
    pub fn refund_links(&mut self, links: u8) -> Result<()> {
        let charged = self
            .charged_links
            .checked_sub(u16::from(links))
            .ok_or(Error::InvalidRequest)?;
        let available = self.links.checked_add(links).ok_or(Error::InvalidRequest)?;
        self.charged_links = charged;
        self.links = available;
        Ok(())
    }

    pub fn deadline(&self) -> Instant {
        self.deadline
    }
    pub fn remaining_attempts(&self) -> u32 {
        self.attempts
    }
    pub fn remaining_links(&self) -> u8 {
        self.links
    }

    /// Both normal and failure ceilings draw from credits allocated by ingress.
    /// Observing failure changes the per-attempt ceiling, never adds credits.
    pub fn route_links(&self) -> u8 {
        self.links.min(if self.failed_route { 8 } else { 4 })
    }
    pub fn note_route_failure(&mut self) {
        self.failed_route = true;
    }

    /// Consume an owned child's unused credits exactly once. Do not reconstruct a
    /// default budget or refund uncertain work after a lost remote response.
    pub fn reunite(&mut self, child: Self) -> Result<()> {
        let attempts = self
            .attempts
            .checked_add(child.attempts)
            .ok_or(Error::InvalidRequest)?;
        let links = self
            .links
            .checked_add(child.links)
            .ok_or(Error::InvalidRequest)?;
        self.deadline = self.deadline.min(child.deadline);
        self.attempts = attempts;
        self.links = links;
        self.failed_route |= child.failed_route;
        Ok(())
    }

    /// Move credits into an independently owned worker/range child. Failure is
    /// atomic; neither fanout nor a worker transfer can mint original credits.
    pub fn partition(&mut self, attempts: u32, links: u8) -> Result<Self> {
        let remaining_attempts = self
            .attempts
            .checked_sub(attempts)
            .ok_or(Error::Unavailable)?;
        let remaining_links = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.attempts = remaining_attempts;
        self.links = remaining_links;
        let mut child = Self::new(self.deadline, attempts, links);
        child.failed_route = self.failed_route;
        Ok(child)
    }

    pub fn transfer(&mut self) -> Self {
        let attempts = std::mem::take(&mut self.attempts);
        let links = std::mem::take(&mut self.links);
        Self {
            deadline: self.deadline,
            attempts,
            links,
            charged_links: std::mem::take(&mut self.charged_links),
            failed_route: self.failed_route,
        }
    }

    /// Atomically debit a peer route's attempt and all forwarded links before I/O.
    pub fn begin_peer_attempt(
        &mut self,
        now: Instant,
        caller_deadline: Instant,
        links: u8,
    ) -> Result<Instant> {
        if links == 0 {
            return Err(Error::InvalidRequest);
        }
        let deadline = self.deadline.min(caller_deadline);
        if now >= deadline {
            return Err(Error::DeadlineExceeded);
        }
        if self.attempts == 0 {
            return Err(Error::Unavailable);
        }
        let remaining = self
            .links
            .checked_sub(links)
            .ok_or(Error::HopBudgetExhausted)?;
        self.attempts -= 1;
        self.links = remaining;
        self.charged_links = self.charged_links.saturating_add(u16::from(links));
        Ok(deadline)
    }
}

/// Includes table identity to reject tokens from another worker/table, plus entry
/// incarnation and acquisition generation to fence remove/rejoin and retry races.
/// Counters must never wrap; drain/restart the owner before exhaustion.
#[derive(Clone)]
pub(super) struct Fence {
    pub(super) page: PageId,
    pub(super) identity: coalesce::flight::Identity,
}
impl std::ops::Deref for Fence {
    type Target = coalesce::flight::Identity;
    fn deref(&self) -> &Self::Target {
        &self.identity
    }
}
impl std::ops::DerefMut for Fence {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.identity
    }
}
impl Fence {
    pub(super) fn validate(&self, current: &Self) -> Result<()> {
        if self.page != current.page {
            return Err(Error::StaleFlight);
        }
        self.identity
            .validate(&current.identity)
            .map_err(|_| Error::StaleFlight)
    }
}

/// Unique publication capability. Resources live in the worker table, not here.
/// Membership/authority are acquisition state, never part of the flight key.
/// ```compile_fail
/// use racer_dataplane::read::flight::FlightLeader;
/// fn duplicate(leader: FlightLeader) { let _other = leader.clone(); }
/// ```
pub struct FlightLeader {
    pub(super) flights: Rc<Flights>,
    pub(super) fence: Fence,
    pub(super) caller: u64,
    pub(super) active: bool,
}

/// Per-request registration; cannot be stored in a completed flight/cache entry.
/// The non-cloneable OriginContext stays with its original request. Only a short
/// borrow is exposed to the elected driver; owned per-attempt sealing/I/O messages
/// retain their own admitted allocations until actual completion. Detach drops
/// these borrows, not another caller's context or outstanding transport owners.
/// ```compile_fail
/// use racer_dataplane::read::flight::AcquisitionWaiter;
/// fn retain(waiter: AcquisitionWaiter<'_>) -> AcquisitionWaiter<'static> { waiter }
/// ```
pub struct AcquisitionWaiter<'a> {
    pub(super) registration: Registration,
    context: &'a OriginContext,
    scope: &'a RequestScope,
    pub(super) membership: std::sync::Arc<crate::topology::Membership>,
    budget: &'a mut AcquisitionBudget,
}

pub(super) struct Registration {
    flights: Rc<Flights>,
    cancellation: uring_runtime::environment::CancellationRegistration,
    // Waiters survive acquisition generations. Match owner/page/incarnation/id
    // for registration, then refresh this generation only on a new election.
    fence: Fence,
    pub(super) id: u64,
    attached: bool,
}

/// No credentials, membership, budget, or API to obtain a leader capability.
/// ```compile_fail
/// use racer_dataplane::read::flight::{CopyWaiter, FlightLeader};
/// fn acquire(mut waiter: CopyWaiter<'_>, leader: &FlightLeader) {
///     let _context = waiter.acquisition(leader);
/// }
/// ```
pub struct CopyWaiter<'a> {
    registration: Registration,
    scope: &'a RequestScope,
}

pub enum JoinedFlight<'a> {
    Waiter(AcquisitionWaiter<'a>),
    Complete(PageResult),
    Ciphertext(UnverifiedPage),
}
pub enum JoinedCopy<'a> {
    Miss,
    Waiter(CopyWaiter<'a>),
    Complete(PageResult),
    Ciphertext(UnverifiedPage),
}
pub enum AcquisitionEvent {
    /// Initial election or retry. Only this caller can lend the matching context.
    Lead(FlightLeader),
    Complete(PageResult),
    Ciphertext(UnverifiedPage),
    Failed(Error),
}

/// Borrowed only after checking the leader, registration, page/object association,
/// cancellation, and generation. It does not grant origin permission: Fill must
/// resolve candidates under this membership and obtain a new OriginAuthority.
pub struct AcquisitionContext<'a> {
    pub origin: &'a OriginContext,
    pub scope: &'a RequestScope,
    pub(crate) cancellation: &'a uring_runtime::environment::CancellationRegistration,
    pub membership: &'a std::sync::Arc<crate::topology::Membership>,
    pub budget: &'a mut AcquisitionBudget,
}

pub enum AcquisitionFailure {
    /// A validated credential-specific origin rejection, not generic peer auth
    /// failure, a copy miss, or proof that this version is unavailable.
    OriginRejected,
    /// Credential-specific origin 403; retry with independent credentials while
    /// preserving the supplying caller's HTTP status.
    OriginForbidden,
    /// Terminal only for this cohort; never negative-cache a page/version here.
    Terminal(Error),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FlightState {
    Acquiring,
    RetryPending,
    Draining,
    Complete,
    Failed,
    Removed,
}

impl AcquisitionWaiter<'_> {
    /// Independent deadline/cancellation. Lead is emitted once per election;
    /// retry elects only a still-live caller with remaining original credits.
    /// A rejected caller receives OriginRejected/OriginForbidden and is ineligible
    /// for this cohort. Peer Unauthorized failures are terminal instead.
    pub fn wait(&mut self) -> Operation<'_, AcquisitionEvent> {
        Box::pin(poll_fn(move |cx| {
            self.registration.cancellation.register(cx.waker());
            if let Err(error) = self.scope.check() {
                self.registration.cancel(error);
                return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
            }
            let flights = self.registration.flights.clone();
            let mut waker = Some(cx.waker().clone());
            let mut retired = None;
            let result = flights.update(|table, wakes| {
                let entry = match registered(table, &self.registration) {
                    Ok(entry) => entry,
                    Err(error) => return Poll::Ready(Err(error)),
                };
                refresh(entry, wakes);
                let waiter = entry.state.waiters.get_mut(&self.registration.id).unwrap();
                if let Some(error) = waiter.error {
                    return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
                }
                if let Phase::Complete(result) = &entry.state.phase {
                    return Poll::Ready(Ok(AcquisitionEvent::Complete(result.clone())));
                }
                if !waiter.complete
                    && let Some(copy) = &entry.state.partial
                {
                    return Poll::Ready(Ok(AcquisitionEvent::Ciphertext(copy.clone())));
                }
                if let Phase::Failed(error) = entry.state.phase {
                    return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
                }
                if matches!(entry.state.phase, Phase::RetryPending) && !waiter.issued {
                    if uring_runtime::environment::now() >= self.budget.deadline
                        || (self.budget.attempts == 0 && entry.state.partial.is_none())
                    {
                        let error = if self.budget.attempts == 0 {
                            Error::Unavailable
                        } else {
                            Error::DeadlineExceeded
                        };
                        waiter.error = Some(error);
                        refresh(entry, wakes);
                        return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
                    }
                    if entry
                        .state
                        .elect(
                            self.registration.id,
                            &mut entry.fence.identity,
                            flights.limits.generations_per_flight,
                        )
                        .is_err()
                    {
                        entry
                            .state
                            .begin_completion(Outcome::Failed(Error::Unavailable));
                        settle(entry, wakes);
                        return Poll::Ready(Ok(AcquisitionEvent::Failed(Error::Unavailable)));
                    }
                    self.registration.fence.generation = entry.fence.generation;
                    entry.diagnostic = None;
                    return Poll::Ready(Ok(AcquisitionEvent::Lead(FlightLeader {
                        flights: flights.clone(),
                        fence: entry.fence.clone(),
                        caller: self.registration.id,
                        active: true,
                    })));
                }
                retired =
                    coalesce::flight::state::store_waker(&mut waiter.waker, waker.take().unwrap());
                Poll::Pending
            });
            drop(retired);
            result
        }))
    }

    pub fn acquisition<'a>(&'a mut self, leader: &FlightLeader) -> Result<AcquisitionContext<'a>> {
        self.scope.check()?;
        validate_context(&leader.fence.page, self.context)?;
        if self.registration.id != leader.caller {
            return Err(Error::StaleFlight);
        }
        self.registration.fence.validate(&leader.fence)?;
        self.registration.flights.update(|table, wakes| {
            let entry = registered(table, &self.registration)?;
            refresh(entry, wakes);
            validate_leader(entry, leader)
        })?;
        Ok(AcquisitionContext {
            origin: self.context,
            scope: self.scope,
            cancellation: &self.registration.cancellation,
            membership: &self.membership,
            budget: self.budget,
        })
    }

    /// Detach only this registration. If supplying an active acquisition, cancel
    /// that attempt and drain it before electing another caller. Drop has the same
    /// required effect. A completed entry retains no OriginContext borrows.
    pub fn detach(mut self) -> Result<FlightState> {
        self.registration.detach()
    }
}
impl CopyWaiter<'_> {
    /// Observe existing work only. Failure/no eligible acquisition callers ends
    /// the wait; it never creates a retry. Results include original ciphertext.
    pub fn wait(&mut self) -> Operation<'_, AcquiredPage> {
        Box::pin(poll_fn(move |cx| {
            self.registration.cancellation.register(cx.waker());
            if let Err(error) = self.scope.check() {
                self.registration.cancel(error);
                return Poll::Ready(Err(error));
            }
            let mut waker = Some(cx.waker().clone());
            let mut retired = None;
            let result = self.registration.flights.update(|table, wakes| {
                let entry = match registered(table, &self.registration) {
                    Ok(entry) => entry,
                    Err(error) => return Poll::Ready(Err(error)),
                };
                refresh(entry, wakes);
                let waiter = entry.state.waiters.get_mut(&self.registration.id).unwrap();
                if let Some(error) = waiter.error {
                    return Poll::Ready(Err(error));
                }
                if let Phase::Failed(error) = entry.state.phase {
                    return Poll::Ready(Err(error));
                }
                if let Phase::Complete(result) = &entry.state.phase {
                    return Poll::Ready(Ok(result.clone().into()));
                }
                if let Some(copy) = &entry.state.partial {
                    return Poll::Ready(Ok(AcquiredPage::Ciphertext(copy.clone())));
                }
                retired =
                    coalesce::flight::state::store_waker(&mut waiter.waker, waker.take().unwrap());
                Poll::Pending
            });
            drop(retired);
            result
        }))
    }
}

impl Flights {
    /// Attach a fresh generation without refreshing state or touching wake ownership.
    pub(super) fn driver_diagnostic(&self, leader: &FlightLeader) -> Option<DriverDiagnostic> {
        let diagnostic = DriverDiagnostic(Rc::new(std::cell::Cell::new(DriverObservation {
            stage: DriverStage::Starting,
            since: uring_runtime::environment::now(),
            cancel_seen: None,
            abandoned: false,
        })));
        let mut table = self.table.borrow_mut();
        let entry = table.get_mut(&leader.fence.page)?;
        validate_leader(entry, leader).ok()?;
        entry.diagnostic = Some(diagnostic.clone());
        Some(diagnostic)
    }

    pub fn new(
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        availability: Rc<crate::control::Availability>,
    ) -> Self {
        let limits = FlightLimits {
            entries: admission.policy().limits().flights.get(),
            waiters_per_flight: admission.policy().limits().waiters_per_flight.get(),
            operations_per_flight: admission.policy().limits().queue_entries.get(),
            generations_per_flight: 1024,
        };
        Self {
            admission: admission.clone(),
            availability,
            owner: Rc::new(()),
            limits,
            table: RefCell::new(Table::default()),
            waiting: uring_runtime::offload::AdmissionQueue::new(
                admission.policy().limits().queue_entries,
            ),
            admission_deadlines: Rc::default(),
            capacity_wake: Rc::default(),
        }
    }

    pub fn with_limits(
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        availability: Rc<crate::control::Availability>,
        limits: FlightLimits,
    ) -> Result<Self> {
        if limits.entries == 0
            || limits.waiters_per_flight == 0
            || limits.operations_per_flight == 0
            || limits.generations_per_flight == 0
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            admission: admission.clone(),
            availability,
            owner: Rc::new(()),
            limits,
            table: RefCell::new(Table::default()),
            waiting: uring_runtime::offload::AdmissionQueue::new(
                admission.policy().limits().queue_entries,
            ),
            admission_deadlines: Rc::default(),
            capacity_wake: Rc::default(),
        })
    }

    // Only new registrations consult current admission. Existing waiters and
    // completion owners retain their result, even after its key is retired.
    fn admit_join(&self, page: &PageId, entry: Option<&Entry>) -> Result<()> {
        let availability = &self.availability;
        if !availability.cache(&page.version.object.cache) {
            return Err(Error::Unavailable);
        }
        let result = entry.and_then(|entry| {
            if let Phase::Complete(result) = &entry.phase {
                return Some(result.copy());
            }
            entry
                .partial
                .as_ref()
                .map(|p| p.copy.clone())
                .or_else(|| match &entry.phase {
                    Phase::Draining(Outcome::Published(result)) => Some(result.copy()),
                    _ => None,
                })
        });
        if result.is_some_and(|result| {
            !availability.page(
                &page.version.object.cache,
                result.ciphertext.envelope().key_id,
            )
        }) {
            return Err(Error::MissingKey);
        }
        Ok(())
    }

    fn update<T>(&self, f: impl FnOnce(&mut Table, &mut Vec<Waker>) -> T) -> T {
        coalesce::flight::update(&self.table, f)
    }

    /// Validate context.object against page.version.object before registration.
    /// Admit a bounded independent waiter. The elected Fill reserves its page
    /// memory before acquisition; accepted work uses retain_operation to retain
    /// completion capacity. Context/header differences do not split page identity.
    pub fn join<'a>(
        self: &Rc<Self>,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Result<JoinedFlight<'a>> {
        self.join_for(page, membership, context, scope, budget, true)
    }

    pub(crate) fn join_for<'a>(
        self: &Rc<Self>,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        plaintext: bool,
    ) -> Result<JoinedFlight<'a>> {
        validate_context(&page, context)?;
        scope.check()?;
        if self.table.borrow().is_stopping() {
            return Err(Error::Cancelled);
        }
        if !self.table.borrow().contains_key(&page) {
            if uring_runtime::environment::now() >= budget.deadline {
                return Err(Error::DeadlineExceeded);
            }
            if budget.attempts == 0 {
                return Err(Error::Unavailable);
            }
            if !self.waiting.is_empty() {
                self.wait_event(
                    scope,
                    &page,
                    false,
                    crate::telemetry::WaitGate::Queued,
                    None,
                    Error::Overloaded,
                );
                return Err(Error::Overloaded);
            }
        }
        self.join_prepared(
            page, membership, context, scope, budget, plaintext, None, None,
        )
    }

    /// Join with credits already owned by a bounded admission ticket.
    #[allow(clippy::too_many_arguments)]
    fn join_prepared<'a>(
        self: &Rc<Self>,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        plaintext: bool,
        mut prepared_waiter: Option<flow_control::Charge<AdmissionPolicy>>,
        mut prepared_flight: Option<FlightCharge>,
    ) -> Result<JoinedFlight<'a>> {
        validate_context(&page, context)?;
        scope.check()?;
        let mut rejected = None;
        let mut gate = crate::telemetry::JoinGate::Unclassified;
        let mut occupancy = (None, None);
        let mut quota_detail = None;
        let page_number = page.number.0;
        let result = self.update(|table, wakes| {
            if table.is_stopping() {
                return Err(Error::Cancelled);
            }
            self.admit_join(&page, table.get(&page))?;
            if let Some(entry) = table.get_mut(&page) {
                refresh(entry, wakes);
                if let Phase::Complete(result) = &entry.phase {
                    return Ok(JoinedFlight::Complete(result.clone()));
                }
                if !plaintext && let Some(copy) = &entry.partial {
                    return Ok(JoinedFlight::Ciphertext(copy.clone()));
                }
                if let Phase::Failed(error) = entry.phase {
                    gate = crate::telemetry::JoinGate::ExistingFailed;
                    return Err(error);
                }
                if entry.waiters.len() >= self.limits.waiters_per_flight {
                    gate = crate::telemetry::JoinGate::WaiterCap;
                    occupancy = (
                        Some(entry.waiters.len()),
                        Some(self.limits.waiters_per_flight),
                    );
                    return Err(Error::Overloaded);
                }
            }
            if uring_runtime::environment::now() >= budget.deadline {
                return Err(Error::DeadlineExceeded);
            }
            if budget.attempts == 0 && table.get(&page).is_none_or(|e| e.partial.is_none()) {
                return Err(Error::Unavailable);
            }
            let cancellation = scope.cancellation.subscribe()?;
            let (reservation, detail) =
                crate::admission::capture_rejection(&self.admission, || {
                    if let Some(reservation) = prepared_waiter.take() {
                        return Ok(reservation);
                    }
                    self.admission
                        .reserve(Some(&page.version.object.cache), ResourceClass::Waiter, 1)
                        .map_err(Error::from)
                });
            let reservation = reservation.inspect_err(|_| {
                gate = crate::telemetry::JoinGate::WaiterQuota;
                quota_detail = detail;
            })?;
            let id = table.next_waiter_id().map_err(|_| {
                gate = crate::telemetry::JoinGate::WaiterId;
                Error::Overloaded
            })?;
            if !table.contains_key(&page) {
                if table.len() >= self.limits.entries {
                    gate = crate::telemetry::JoinGate::EntryCap;
                    occupancy = (Some(table.len()), Some(self.limits.entries));
                    return Err(Error::Overloaded);
                }
                let identity = table.identity(self.owner.clone()).map_err(|_| {
                    gate = crate::telemetry::JoinGate::Identity;
                    Error::Overloaded
                })?;
                let (flight, detail) = crate::admission::capture_rejection(&self.admission, || {
                    if let Some(flight) = prepared_flight.take() {
                        return Ok(flight);
                    }
                    self.admission
                        .reserve(Some(&page.version.object.cache), ResourceClass::Flight, 1)
                        .map(|charge| self.flight_charge(charge))
                        .map_err(Error::from)
                });
                let flight = flight.inspect_err(|_| {
                    gate = crate::telemetry::JoinGate::FlightQuota;
                    quota_detail = detail;
                })?;
                if let Err(owned) = table.insert(
                    page.clone(),
                    Entry {
                        created: uring_runtime::environment::now(),
                        diagnostic: None,
                        fence: Fence {
                            page: page.clone(),
                            identity,
                        },
                        state: FlightCore::default(),
                        operations: coalesce::flight::Operations::default(),
                        _reservation: flight,
                    },
                ) {
                    rejected = Some(owned);
                    gate = crate::telemetry::JoinGate::Insert;
                    return Err(Error::Overloaded);
                }
            }
            let entry = table.get_mut(&page).unwrap();
            entry.state.register(
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    budget_deadline: budget.deadline,
                    _reservation: reservation,
                },
                true,
                plaintext,
            );
            Ok(JoinedFlight::Waiter(AcquisitionWaiter {
                registration: Registration {
                    flights: self.clone(),
                    cancellation,
                    fence: entry.fence.clone(),
                    id,
                    attached: true,
                },
                context,
                scope,
                membership,
                budget,
            }))
        });
        drop(rejected);
        if result.is_ok() {
            self.waiting.wake_if(
                self.admission.policy().limits().queue_entries.get(),
                |waiting| waiting == &page,
            );
        }
        if matches!(result, Err(Error::Overloaded)) {
            self.admission.policy().observer().gate_rejection(
                scope,
                Some(page_number),
                crate::telemetry::GateFacts::Join {
                    gate,
                    copy: false,
                    used: occupancy.0,
                    limit: occupancy.1,
                    detail: quota_detail,
                },
            );
        }
        result
    }

    /// Wait only for new-entry capacity, never for another resource class.
    pub(crate) async fn join_wait<'a>(
        self: &Rc<Self>,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Result<JoinedFlight<'a>> {
        validate_context(&page, context)?;
        scope.check()?;
        if self.table.borrow().contains_key(&page) {
            return self.join(page, membership, context, scope, budget);
        }
        if self.table.borrow().is_stopping() {
            return Err(Error::Cancelled);
        }
        self.admit_join(&page, None)?;
        if uring_runtime::environment::now() >= budget.deadline {
            return Err(Error::DeadlineExceeded);
        }
        if budget.attempts == 0 {
            return Err(Error::Unavailable);
        }
        if self.waiting.is_empty()
            && self.table.borrow().len() < self.limits.entries
            && self
                .admission
                .reclamation(&page.version.object.cache, ResourceClass::Flight, 1)
                .is_none()
        {
            return self.join(page, membership, context, scope, budget);
        }
        let cutoff = uring_runtime::environment::now() + std::time::Duration::from_millis(100);
        let deadline = cutoff.min(scope.deadline.0).min(budget.deadline);
        let (reservation, detail) = crate::admission::capture_rejection(&self.admission, || {
            self.admission
                .reserve(Some(&page.version.object.cache), ResourceClass::Waiter, 1)
                .map_err(Error::from)
        });
        if matches!(reservation, Err(Error::Overloaded)) {
            self.admission.policy().observer().gate_rejection(
                scope,
                Some(page.number.0),
                crate::telemetry::GateFacts::Join {
                    gate: crate::telemetry::JoinGate::WaiterQuota,
                    copy: false,
                    used: None,
                    limit: None,
                    detail,
                },
            );
        }
        let reservation = reservation?;
        let cancellation = scope.cancellation.subscribe()?;
        let expiry = self.admission_deadlines.register(deadline)?;
        let mut ticket: Option<AdmissionTicket<'_>> = None;
        let mut reason = crate::telemetry::WaitGate::AdmissionFailed;
        let mut cause = None;
        let mut expiry_snapshot = None;
        let flight = poll_fn(|cx| {
            cancellation.register(cx.waker());
            let _ = expiry.poll_expired(cx);
            if let Err(error) = scope.check() {
                reason = if error == Error::Cancelled {
                    crate::telemetry::WaitGate::Cancelled
                } else {
                    crate::telemetry::WaitGate::ScopeDeadline
                };
                return Poll::Ready(Err(error));
            }
            let now = uring_runtime::environment::now();
            if now >= budget.deadline {
                reason = crate::telemetry::WaitGate::BudgetDeadline;
                return Poll::Ready(Err(Error::DeadlineExceeded));
            }
            if now >= cutoff {
                reason = crate::telemetry::WaitGate::AllowanceExpired;
                expiry_snapshot = Some(self.expiry_snapshot(now, cutoff, ticket.as_ref()));
                return Poll::Ready(Err(Error::Overloaded));
            }
            if self.table.borrow().is_stopping() {
                reason = crate::telemetry::WaitGate::Cancelled;
                return Poll::Ready(Err(Error::Cancelled));
            }
            self.admit_join(&page, self.table.borrow().get(&page))?;
            if self.table.borrow().contains_key(&page) {
                return Poll::Ready(Ok(None));
            }
            if budget.attempts == 0 {
                return Poll::Ready(Err(Error::Unavailable));
            }
            if ticket.is_none() {
                let entered = match self.waiting.enter(page.clone(), cx) {
                    Ok(ticket) => ticket,
                    Err(error) => {
                        reason = if error == uring_runtime::Error::Overloaded {
                            crate::telemetry::WaitGate::QueueFull
                        } else {
                            crate::telemetry::WaitGate::QueueAdmissionRejected
                        };
                        return Poll::Ready(Err(error.into()));
                    }
                };
                ticket = Some(AdmissionTicket {
                    ticket: Some(entered),
                    wake: self.capacity_wake.clone(),
                    flights: self,
                });
            }
            let ticket = ticket.as_ref().unwrap().ticket.as_ref().unwrap();
            ticket.poll(cx, |cx| {
                let wake = cx.waker().clone();
                let old = self
                    .capacity_wake
                    .borrow_mut()
                    .replace((ticket.sequence(), wake));
                drop(old);
                if self.table.borrow().len() >= self.limits.entries {
                    cause = Some(crate::telemetry::JoinGate::EntryCap);
                    return Poll::Pending;
                }
                if self
                    .admission
                    .reclamation(&page.version.object.cache, ResourceClass::Flight, 1)
                    .is_some()
                {
                    cause = Some(crate::telemetry::JoinGate::FlightQuota);
                    return Poll::Pending;
                }
                let (result, detail) = crate::admission::capture_rejection(&self.admission, || {
                    self.admission
                        .reserve(Some(&page.version.object.cache), ResourceClass::Flight, 1)
                        .map_err(Error::from)
                });
                match result {
                    Ok(charge) => Poll::Ready(Ok(Some(self.flight_charge(charge)))),
                    Err(Error::Overloaded)
                        if matches!(
                            detail,
                            Some(crate::telemetry::Detail::Resource {
                                class: ResourceClass::Flight,
                                ..
                            })
                        ) =>
                    {
                        cause = Some(crate::telemetry::JoinGate::FlightQuota);
                        Poll::Pending
                    }
                    Err(error) => Poll::Ready(Err(error)),
                }
            })
        })
        .await;
        if let Some(snapshot) = expiry_snapshot {
            self.admission
                .policy()
                .observer()
                .flight_expiry(scope, page.number.0, snapshot);
        }
        let flight = flight
            .inspect_err(|error| self.wait_event(scope, &page, false, reason, cause, *error))?;
        drop((expiry, cancellation));
        let result = self.join_prepared(
            page,
            membership,
            context,
            scope,
            budget,
            true,
            Some(reservation),
            flight,
        );
        drop(ticket);
        result
    }

    /// Attach a worker-local release notification to a real Flight charge.
    fn flight_charge(&self, charge: flow_control::Charge<AdmissionPolicy>) -> FlightCharge {
        FlightCharge {
            charge: Some(charge),
            wake: self.capacity_wake.clone(),
        }
    }

    /// Freeze local facts before ticket cleanup; quota counters are separate samples.
    fn expiry_snapshot(
        &self,
        now: Instant,
        cutoff: Instant,
        ticket: Option<&AdmissionTicket<'_>>,
    ) -> crate::telemetry::FlightExpiry {
        let age =
            |at| u64::try_from(now.saturating_duration_since(at).as_micros()).unwrap_or(u64::MAX);
        let queue = self.waiting.snapshot(now);
        let ticket = ticket.and_then(|ticket| ticket.ticket.as_ref());
        let table = self.table.borrow();
        let entries = table.sample::<6>().map(|entry| {
            entry.map(|entry| crate::telemetry::FlightExpiryEntry {
                age_us: age(entry.created),
                lifecycle: entry.phase.state(),
                stage: entry
                    .diagnostic
                    .as_ref()
                    .map(|diagnostic| diagnostic.0.get().stage),
                stage_age_us: entry
                    .diagnostic
                    .as_ref()
                    .map(|diagnostic| age(diagnostic.0.get().since)),
                cancel_request_seen_age_us: entry
                    .diagnostic
                    .as_ref()
                    .and_then(|diagnostic| diagnostic.0.get().cancel_seen.map(age)),
                driver_abandoned: entry
                    .diagnostic
                    .as_ref()
                    .is_some_and(|diagnostic| diagnostic.0.get().abandoned),
                waiters: entry.waiters.len(),
                operations: entry.operations.len(),
                completing: entry.operations.completing(),
            })
        });
        crate::telemetry::FlightExpiry {
            observed_unix_ms: crate::telemetry::timestamp(now),
            overshoot_us: age(cutoff),
            queue,
            caller_head: ticket.map(|ticket| Some(ticket.sequence()) == queue.head),
            caller_age_us: ticket.map(|ticket| age(ticket.entered())),
            total: table.len(),
            entry_limit: self.limits.entries,
            flight_used: self.admission.used(ResourceClass::Flight),
            flight_limit: self.admission.limit(ResourceClass::Flight),
            waiter_used: self.admission.used(ResourceClass::Waiter),
            waiter_limit: self.admission.limit(ResourceClass::Waiter),
            entries,
        }
    }

    /// Inspect the same immutable facts without fabricating a queued ticket.
    #[cfg(test)]
    pub(super) fn expiry_snapshot_without_ticket(
        &self,
        now: Instant,
    ) -> crate::telemetry::FlightExpiry {
        self.expiry_snapshot(now, now, None)
    }

    /// Inspect admission ownership in lifecycle regressions.
    #[cfg(test)]
    pub(super) fn admission_state(&self) -> (usize, usize, bool) {
        (
            self.waiting.len(),
            self.admission_deadlines.len(),
            self.capacity_wake.borrow().is_some(),
        )
    }

    /// Private disk copies fail promptly rather than taking credit ahead of queued reads.
    pub(super) fn reserve_copy_flight(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<FlightCharge> {
        scope.check()?;
        if !self.waiting.is_empty() {
            self.wait_event(
                scope,
                page,
                true,
                crate::telemetry::WaitGate::Queued,
                None,
                Error::Overloaded,
            );
            return Err(Error::Overloaded);
        }
        let (result, detail) = crate::admission::capture_rejection(&self.admission, || {
            self.admission
                .reserve(Some(&page.version.object.cache), ResourceClass::Flight, 1)
                .map_err(Error::from)
        });
        if matches!(result, Err(Error::Overloaded)) {
            self.admission.policy().observer().gate_rejection(
                scope,
                Some(page.number.0),
                crate::telemetry::GateFacts::Join {
                    gate: crate::telemetry::JoinGate::FlightQuota,
                    copy: true,
                    used: None,
                    limit: None,
                    detail,
                },
            );
        }
        result.map(|charge| self.flight_charge(charge))
    }

    /// Report one terminal admission outcome, not a fresh capacity measurement.
    fn wait_event(
        &self,
        scope: &RequestScope,
        page: &PageId,
        copy: bool,
        reason: crate::telemetry::WaitGate,
        cause: Option<crate::telemetry::JoinGate>,
        error: Error,
    ) {
        self.admission.policy().observer().gate_rejection(
            scope,
            Some(page.number.0),
            crate::telemetry::GateFacts::Wait {
                reason,
                copy,
                cause,
                error,
            },
        );
    }

    /// Never creates an entry, elects a leader, or revives failed/draining work.
    /// It may observe retries funded by independently registered acquisition callers.
    pub fn join_copy<'a>(
        self: &Rc<Self>,
        page: &PageId,
        scope: &'a RequestScope,
    ) -> Result<JoinedCopy<'a>> {
        scope.check()?;
        let mut gate = crate::telemetry::JoinGate::Unclassified;
        let mut occupancy = (None, None);
        let mut quota_detail = None;
        let result = self.update(|table, wakes| {
            if table.is_stopping() {
                return Err(Error::Cancelled);
            }
            let Some(entry) = table.get_mut(page) else {
                return Ok(JoinedCopy::Miss);
            };
            if self.admit_join(page, Some(entry)).is_err() {
                return Ok(JoinedCopy::Miss);
            }
            refresh(entry, wakes);
            if let Phase::Complete(result) = &entry.phase {
                return Ok(JoinedCopy::Complete(result.clone()));
            }
            if let Some(copy) = &entry.partial {
                return Ok(JoinedCopy::Ciphertext(copy.clone()));
            }
            if matches!(entry.phase, Phase::Failed(_) | Phase::Draining(_)) {
                return Ok(JoinedCopy::Miss);
            }
            if entry.waiters.len() >= self.limits.waiters_per_flight {
                gate = crate::telemetry::JoinGate::WaiterCap;
                occupancy = (
                    Some(entry.waiters.len()),
                    Some(self.limits.waiters_per_flight),
                );
                return Err(Error::Overloaded);
            }
            let cancellation = scope.cancellation.subscribe()?;
            let (reservation, detail) =
                crate::admission::capture_rejection(&self.admission, || {
                    self.admission
                        .reserve(Some(&page.version.object.cache), ResourceClass::Waiter, 1)
                        .map_err(Error::from)
                });
            let reservation = reservation.inspect_err(|_| {
                gate = crate::telemetry::JoinGate::WaiterQuota;
                quota_detail = detail;
            })?;
            let id = table.next_waiter_id().map_err(|_| {
                gate = crate::telemetry::JoinGate::WaiterId;
                Error::Overloaded
            })?;
            let entry = table.get_mut(page).expect("existing copy flight");
            entry.state.register(
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    budget_deadline: scope.deadline.0,
                    _reservation: reservation,
                },
                false,
                false,
            );
            Ok(JoinedCopy::Waiter(CopyWaiter {
                registration: Registration {
                    flights: self.clone(),
                    cancellation,
                    fence: entry.fence.clone(),
                    id,
                    attached: true,
                },
                scope,
            }))
        });
        if matches!(result, Err(Error::Overloaded)) {
            self.admission.policy().observer().gate_rejection(
                scope,
                Some(page.number.0),
                crate::telemetry::GateFacts::Join {
                    gate,
                    copy: true,
                    used: occupancy.0,
                    limit: occupancy.1,
                    detail: quota_detail,
                },
            );
        }
        result
    }

    /// Check live leader fence before publication, then validate_for its exact
    /// page. Wake all readers with clones of the complete credential-free bundle.
    /// This cannot update a current-version freshness pointer. Release raw caller
    /// borrows at request completion; owned I/O/crypto state still obeys its fences.
    pub fn publish(&self, leader: FlightLeader, page: PageResult) -> Result<FlightState> {
        self.publish_acquired(leader, page.into())
    }

    pub(crate) fn ciphertext_for(&self, leader: &FlightLeader) -> Result<Option<UnverifiedPage>> {
        self.update(|table, wakes| Ok(self.leader_entry(table, leader, wakes)?.partial.clone()))
    }

    pub(crate) fn discard_ciphertext(&self, leader: &FlightLeader) -> Result<()> {
        self.update(|table, wakes| {
            self.leader_entry(table, leader, wakes)?.partial = None;
            Ok(())
        })
    }

    pub(crate) fn publish_acquired(
        &self,
        mut leader: FlightLeader,
        page: AcquiredPage,
    ) -> Result<FlightState> {
        self.update(|table, wakes| {
            let entry = self.leader_entry(table, &leader, wakes)?;
            page.validate_for(&entry.fence.page)?;
            entry.state.begin_completion(Outcome::Published(page));
            leader.active = false;
            settle(entry, wakes);
            Ok(entry.phase.state())
        })
    }

    /// Accept only once the worker confirms actual completions are reaped; a
    /// caller's report alone is not a completion fence. OriginRejected
    /// marks only its caller failed and moves to RetryPending if an eligible caller
    /// remains; otherwise fail remaining copy waiters with Unavailable. Terminal
    /// errors fail the cohort. Neither result is cached as evidence about the page.
    /// Stale fences return StaleFlight without mutating the current generation.
    pub fn fail(
        &self,
        mut leader: FlightLeader,
        failure: AcquisitionFailure,
    ) -> Result<FlightState> {
        self.update(|table, wakes| {
            let entry = self.leader_entry(table, &leader, wakes)?;
            let outcome = match failure {
                AcquisitionFailure::OriginRejected => {
                    entry.state.cancel(leader.caller, Error::OriginRejected);
                    Outcome::Retry
                }
                AcquisitionFailure::OriginForbidden => {
                    entry.state.cancel(leader.caller, Error::OriginForbidden);
                    Outcome::Retry
                }
                AcquisitionFailure::Terminal(error) => Outcome::Failed(error),
            };
            entry.state.begin_completion(outcome);
            leader.active = false;
            notify(entry, wakes);
            settle(entry, wakes);
            Ok(entry.phase.state())
        })
    }

    /// I/O worker lifecycle hook, called even with no remaining request futures.
    /// Process detach/abandon notifications and already-reaped runtime completions,
    /// check their fences, wake/elect live callers, and remove quiescent entries.
    /// Never substitute a timeout or cancellation request for an actual fence.
    pub fn poll_budgeted(&self, work_budget: usize) -> Result<()> {
        self.poll_with_context(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            work_budget,
        )
    }

    /// Reactor-facing variant registers its real waker with owned drivers. The
    /// worker must also poll at request deadlines to expire parked waiters.
    pub fn poll_with_context(&self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        uring_runtime::drivers::poll(cx, work_budget);
        self.admission_deadlines
            .poll(uring_runtime::environment::now(), work_budget);
        self.sweep_budgeted(work_budget)
    }

    /// Earliest live registration deadline for the worker's timer queue. Poll
    /// flights at this instant even if no transport completion wakes the worker.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.table
            .borrow()
            .values()
            .filter_map(|entry| {
                entry
                    .deadlines
                    .first_key_value()
                    .map(|((deadline, _), _)| *deadline)
            })
            .chain(self.admission_deadlines.next_deadline())
            .min()
    }

    fn sweep_budgeted(&self, work_budget: usize) -> Result<()> {
        drop(self.update(|table, wakes| table.sweep(work_budget, wakes)));
        Ok(())
    }

    /// Stop new joins/elections, detach callers, request cancellation, and keep
    /// reaping accepted work. Expiring shutdown scope cannot free live owners.
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        // Initiate shutdown synchronously: dropping the returned future cannot
        // reopen admission or discard accepted work.
        self.update(|table, wakes| {
            table.stop(wakes, |entry, wakes| {
                entry.state.stop(Error::Cancelled, wakes);
                settle(entry, wakes);
            });
        });
        self.waiting
            .wake_if(self.admission.policy().limits().queue_entries.get(), |_| {
                true
            });
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(move |cx| {
                cancellation.register(cx.waker());
                self.poll_with_context(cx, self.limits.entries)?;
                let waker = cx.waker().clone();
                let mut table = self.table.borrow_mut();
                if table.is_empty()
                    && self.waiting.is_empty()
                    && uring_runtime::drivers::pending() == 0
                {
                    return Poll::Ready(Ok(()));
                }
                scope.check()?;
                let previous = table.register_drain(waker);
                drop(table);
                drop(previous);
                Poll::Pending
            })
            .await
        })
    }

    /// Retain actual resources before submission. The worker completing the I/O
    /// owns the token; the request future must not report cancellation as completion.
    pub fn retain_operation(
        self: &Rc<Self>,
        leader: &FlightLeader,
        resources: telemetry::Lease,
    ) -> Result<FlightOperation> {
        self.update(|table, wakes| {
            let id = table.next_operation_id().map_err(|_| Error::Overloaded)?;
            let entry = self.leader_entry(table, leader, wakes)?;
            if entry.operations.len() >= self.limits.operations_per_flight {
                return Err(Error::Overloaded);
            }
            let reservation = self.admission.reserve(
                Some(&entry.fence.page.version.object.cache),
                ResourceClass::ControlProgress,
                1,
            )?;
            entry.operations.insert(
                id,
                Retained {
                    _resources: resources,
                    _reservation: reservation,
                },
            );
            Ok(FlightOperation {
                flights: self.clone(),
                fence: entry.fence.clone(),
                id,
            })
        })
    }

    pub(super) fn complete_operation(&self, fence: &Fence, id: u64) -> Result<()> {
        // Drop caller-owned resource bundles outside the table borrow.
        let resources = self.update(|table, _wakes| {
            let entry = table.get_mut(&fence.page).ok_or(Error::StaleFlight)?;
            fence.validate(&entry.fence)?;
            entry.operations.take(id).map_err(|_| Error::StaleFlight)
        })?;
        drop(resources);
        self.update(|table, wakes| -> Result<()> {
            let entry = table.get_mut(&fence.page).ok_or(Error::StaleFlight)?;
            fence.validate(&entry.fence)?;
            // Keep the slot occupied while resource destructors run: they may
            // reenter the worker, but cannot cause an overlapping election.
            entry
                .operations
                .complete(id)
                .map_err(|_| Error::StaleFlight)?;
            refresh(entry, wakes);
            table.notify_drain(wakes);
            Ok(())
        })?;
        drop(self.update(|table, _wakes| table.remove_quiescent(&fence.page)));
        Ok(())
    }

    fn leader_entry<'a>(
        &self,
        table: &'a mut Table,
        leader: &FlightLeader,
        wakes: &mut Vec<Waker>,
    ) -> Result<&'a mut Entry> {
        if !Rc::ptr_eq(&self.owner, &leader.fence.owner) {
            return Err(Error::StaleFlight);
        }
        let entry = table
            .get_mut(&leader.fence.page)
            .ok_or(Error::StaleFlight)?;
        refresh(entry, wakes);
        validate_leader(entry, leader)?;
        Ok(entry)
    }
}

impl coalesce::flight::Entry for Entry {
    fn refresh(&mut self, wakes: &mut Vec<Waker>) {
        // One cancellation candidate per entry, independent of fan-in.
        self.state.sweep_waiter(wakes);
        refresh(self, wakes);
    }

    fn quiescent(&self) -> bool {
        self.waiters.is_empty() && self.operations.is_empty()
    }
}
fn notify(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    entry.state.notify(wakes);
}
fn settle(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    entry.state.settle(
        entry.operations.is_empty(),
        Error::Unavailable,
        split_result,
        wakes,
    );
}
fn split_result(
    page: AcquiredPage,
) -> coalesce::flight::state::Published<PageResult, UnverifiedPage> {
    match page {
        AcquiredPage::Plaintext(page) => coalesce::flight::state::Published::Complete(page),
        AcquiredPage::Ciphertext(page) => coalesce::flight::state::Published::Partial(page),
    }
}
fn revoke(entry: &mut Entry, error: Error, wakes: &mut Vec<Waker>) {
    entry.state.revoke(error, wakes);
    settle(entry, wakes);
}
fn refresh(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    entry.state.refresh(
        entry.operations.is_empty(),
        Error::Unavailable,
        Error::Cancelled,
        uring_runtime::environment::now,
        split_result,
        wakes,
    );
}
fn validate_leader(entry: &Entry, leader: &FlightLeader) -> Result<()> {
    leader.fence.validate(&entry.fence)?;
    entry
        .state
        .validate_leader(leader.caller, leader.active)
        .map_err(|_| Error::StaleFlight)
}
fn registered<'a>(table: &'a mut Table, registration: &Registration) -> Result<&'a mut Entry> {
    if table.is_stopping() {
        return Err(Error::Cancelled);
    }
    let entry = table
        .get_mut(&registration.fence.page)
        .ok_or(Error::StaleFlight)?;
    entry
        .state
        .validate_registration(
            registration.id,
            registration.attached,
            &registration.fence.identity,
            &entry.fence.identity,
        )
        .map_err(|_| Error::StaleFlight)?;
    Ok(entry)
}
impl Registration {
    fn cancel(&self, error: Error) {
        self.flights.update(|table, wakes| {
            if let Ok(entry) = registered(table, self) {
                entry.state.cancel(self.id, error);
                refresh(entry, wakes);
            }
        });
    }
    fn detach(&mut self) -> Result<FlightState> {
        let state = self.flights.update(|table, wakes| {
            let entry = registered(table, self)?;
            entry.state.detach(self.id);
            refresh(entry, wakes);
            let state = entry.phase.state();
            let removed = table.remove_quiescent(&self.fence.page);
            if removed.is_some() {
                table.notify_drain(wakes);
                Ok((FlightState::Removed, removed))
            } else {
                Ok((state, removed))
            }
        });
        self.attached = false;
        state.map(|(state, removed)| {
            drop(removed);
            state
        })
    }
}
impl Drop for Registration {
    fn drop(&mut self) {
        if self.attached {
            let _ = self.detach();
        }
    }
}
impl Drop for FlightLeader {
    fn drop(&mut self) {
        if self.active {
            self.flights.update(|table, wakes| {
                if let Some(entry) = table.get_mut(&self.fence.page)
                    && validate_leader(entry, self).is_ok()
                {
                    revoke(entry, Error::Cancelled, wakes);
                }
            });
        }
    }
}

pub(super) fn validate_context(page: &PageId, context: &OriginContext) -> Result<()> {
    if page.version.object != context.object {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

#[cfg(test)]
mod admission_wake_tests {
    use super::*;

    /// Serial completion, error, abandonment, and old cells cannot invent an outcome.
    #[test]
    fn expiry_snapshot_call_guards_and_generation_cells() {
        let clock = uring_runtime::environment::SimulationClock::new(920);
        let _environment = clock.environment(0).enter();
        let make = || {
            DriverDiagnostic(Rc::new(std::cell::Cell::new(DriverObservation {
                stage: DriverStage::Starting,
                since: uring_runtime::environment::now(),
                cancel_seen: None,
                abandoned: false,
            })))
        };
        let old = make();
        for success in [false, true] {
            let result: Result<()> = {
                let _guard = old.call(DriverStage::DecryptCall);
                if success { Ok(()) } else { Err(Error::Io) }
            };
            assert_eq!(result.is_ok(), success);
            assert_eq!(old.0.get().stage, DriverStage::BetweenCalls);
        }
        let lifetime = DriverDiagnostic::lifetime(Some(old.clone()));
        let call = old.call(DriverStage::DiskReadCall);
        old.cancellation_seen();
        let first = old.0.get().cancel_seen;
        clock.advance(std::time::Duration::from_millis(2));
        old.cancellation_seen();
        assert_eq!(old.0.get().cancel_seen, first);
        drop(call);
        drop(lifetime);
        assert!(old.0.get().abandoned);
        assert_eq!(old.0.get().stage, DriverStage::BetweenCalls);
        let replacement = make();
        old.set(DriverStage::WorkReturned);
        assert_eq!(replacement.0.get().stage, DriverStage::Starting);
        let lifetime = DriverDiagnostic::lifetime(Some(replacement.clone()));
        let call = replacement.call(DriverStage::PublishCall);
        replacement.set(DriverStage::WorkReturned);
        drop(call);
        drop(lifetime);
        assert_eq!(replacement.0.get().stage, DriverStage::WorkReturned);
        assert!(!replacement.0.get().abandoned);
    }

    /// A successor can install its notification synchronously during ticket removal.
    #[test]
    fn bounded_admission_reentrant_successor_registration_survives_head_drop() {
        thread_local! {
            static CALLBACK: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        }
        struct Reenter;
        impl std::task::Wake for Reenter {
            fn wake(self: std::sync::Arc<Self>) {
                self.wake_by_ref();
            }
            fn wake_by_ref(self: &std::sync::Arc<Self>) {
                let callback = CALLBACK.with(|slot| slot.borrow_mut().take());
                if let Some(callback) = callback {
                    callback();
                }
            }
        }
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let flights = Rc::new(Flights::new(
            admission.clone(),
            crate::test_support::availability(),
        ));
        let page = crate::memory::tests::bundle_for(
            &admission,
            crate::model::VersionMetadata {
                version: crate::model::ObjectVersion {
                    object: crate::model::ObjectId {
                        cache: racer_control_wire::CacheId(
                            crate::test_support::security::CACHE.into(),
                        ),
                        key: crate::model::CacheKey([0; 32]),
                    },
                    etag: crate::model::StrongEtag::test_value("wake"),
                },
                length: 3,
                content_type: None,
            },
        )
        .plaintext
        .page()
        .clone();
        let head = flights
            .waiting
            .enter(page.clone(), &Context::from_waker(Waker::noop()))
            .unwrap();
        let head_id = head.sequence();
        let reentrant = Waker::from(std::sync::Arc::new(Reenter));
        let tail = flights
            .waiting
            .enter(page, &Context::from_waker(&reentrant))
            .unwrap();
        let tail_id = tail.sequence();
        let count = std::sync::Arc::new(crate::test_support::WakeCounter::default());
        let next_wake = Waker::from(count.clone());
        *flights.capacity_wake.borrow_mut() = Some((head_id, Waker::noop().clone()));
        let owner = flights.clone();
        CALLBACK.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert_eq!(owner.waiting.len(), 1);
                assert!(owner.capacity_wake.borrow().is_none());
                *owner.capacity_wake.borrow_mut() = Some((tail_id, next_wake));
            }))
        });
        let charge =
            flights.flight_charge(admission.reserve(None, ResourceClass::Flight, 1).unwrap());
        drop(AdmissionTicket {
            ticket: Some(head),
            wake: flights.capacity_wake.clone(),
            flights: &flights,
        });
        assert_eq!(flights.capacity_wake.borrow().as_ref().unwrap().0, tail_id);
        drop(charge);
        assert_eq!(count.count(), 1);
        drop(AdmissionTicket {
            ticket: Some(tail),
            wake: flights.capacity_wake.clone(),
            flights: &flights,
        });
        assert_eq!(flights.admission_state(), (0, 0, false));
    }
}
