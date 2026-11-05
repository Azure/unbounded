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
use flow_control::coalesce::flight::state::{
    Outcome as StateOutcome, Phase as StatePhase, Published,
};
use racer_core::read::flight;
use racer_core::read::flight::{Ticket, WakeTarget};
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

    limits: FlightLimits,

    pub(super) table: RefCell<Table>,

    waiting: racer_io::xthread::offload::AdmissionQueue<PageId>,

    admission_deadlines: Rc<crate::rt::environment::Registry>,

    capacity_wake: CapacityWake,

    /// Flight ages, budget gates, and driver diagnostics.
    clock: racer_io::Clock,
}

/// The current FIFO head alone owns this notification.
type CapacityWake = Rc<RefCell<Option<(u64, Waker)>>>;

/// Retire only this ticket's wake before its queue guard can wake a successor.
struct AdmissionTicket<'a> {
    ticket: Option<racer_io::xthread::offload::CapacityWaiter<'a, PageId>>,

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

/// Local caps in addition to shared admission quotas.
pub use flight::FlightLimits;

#[cfg(not(test))]
type FlightHashState = std::collections::hash_map::RandomState;
#[cfg(test)]
type FlightHashState = crate::runtime::HashState;
/// Production page flight payloads; the same [`flight::PageFlightTable`] family the core
/// `WorkerCore` owns, instantiated with real pages, wakers, and admission charges.
pub(super) struct ProductionFlights;

impl flight::PageFlightTypes for ProductionFlights {
    type Page = PageId;
    type Complete = PageResult;
    type Partial = UnverifiedPage;
    type Outcome = AcquiredPage;
    type Result = ();
    type Hash = FlightHashState;
    type Wake = WakeTarget;
    type Waiter = WaiterRecord;
    type Ext = EntryExt;
    type Retained = Retained;
}

pub(super) type Table = flight::PageFlightTable<ProductionFlights>;
type Entry = flight::FlightEntry<
    PageResult,
    UnverifiedPage,
    AcquiredPage,
    (),
    WakeTarget,
    WaiterRecord,
    EntryExt,
    Retained,
>;

/// What production keeps per page flight beyond the shared table state.
pub(super) struct EntryExt {
    created: Instant,

    diagnostic: Option<DriverDiagnostic>,

    /// Driver polling a handover-enabled operation, woken when a waiter detaches or is canceled so
    /// it re-evaluates whether any live waiter still needs the work.
    driver_waker: Option<Waker>,

    _reservation: FlightCharge,
}

/// Every `Flights` gets its own table fence, so capabilities never cross tables.
fn next_fence() -> flight::Fence {
    static NEXT: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    flight::Fence(NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed))
}

/// Table-level stale capabilities are `StaleFlight` here.
fn stale(error: racer_core::read::Error) -> Error {
    if error == racer_core::read::Error::Internal {
        Error::StaleFlight
    } else {
        error.into()
    }
}

/// Park `waker` in `slot`, returning a replaced target to drop outside the table borrow.
fn store_waker(slot: &mut Option<WakeTarget>, waker: &Waker) -> Option<WakeTarget> {
    if slot
        .as_ref()
        .is_some_and(|current| current.0.will_wake(waker))
    {
        return None;
    }
    slot.replace(WakeTarget(waker.clone()))
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
    /// Serial tag check call, including admission and completion delivery.
    TagCheckCall,
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
pub(super) struct DriverDiagnostic(Rc<std::cell::Cell<DriverObservation>>, racer_io::Clock);

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
        state.since = self.1.now();
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
        state.cancel_seen.get_or_insert_with(|| self.1.now());
        self.0.set(state);
    }

    /// Own only diagnostic state until detached driver destruction.
    pub(super) fn lifetime(diagnostic: Option<Self>) -> DriverLifetime {
        DriverLifetime(diagnostic)
    }
}
pub(super) struct WaiterRecord {
    scope: RequestScope,
    pub(super) budget_deadline: Instant,
    _reservation: flow_control::Charge<AdmissionPolicy>,
}
impl flow_control::coalesce::flight::state::WaiterPolicy for WaiterRecord {
    type Error = Error;
    fn check(&self) -> Option<Error> {
        self.scope.check().err().or_else(|| {
            (self.scope.clock().now() >= self.budget_deadline).then_some(Error::DeadlineExceeded)
        })
    }
    fn deadline(&self) -> Instant {
        self.scope.deadline.0.min(self.budget_deadline)
    }
}
pub(super) struct Retained {
    _resources: crate::telemetry::primitives::Lease,
    _reservation: flow_control::Charge<AdmissionPolicy>,
}
pub(super) type Outcome = StateOutcome<AcquiredPage, Error>;
pub(super) type Phase = StatePhase<PageResult, AcquiredPage, Error>;
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
    pub(super) page: PageId,
    pub(super) operation: flight::Operation,
    /// Wake at the deadline of the waiter the operation currently serves, so
    /// a handed-over operation observes the end of its last adopter.
    alarm: Option<(Instant, crate::rt::environment::Registration)>,
}
impl FlightOperation {
    pub fn complete(self) -> Result<()> {
        self.flights.complete_operation(&self.page, self.operation)
    }

    /// Allows a completion owner to observe revoked leadership and request actual
    /// transport cancellation. Cancellation is not itself a completion fence.
    /// Refreshes the entry first, so an expired leader of a handover-enabled
    /// acquisition passes to another live waiter rather than ending the work.
    pub fn cancellation_requested(&self) -> bool {
        self.leader_deadline(None).is_none()
    }

    /// Deadline of the waiter this operation currently serves, or `None` once
    /// the acquisition was revoked, settled, or stopped.
    fn leader_deadline(&self, waker: Option<&Waker>) -> Option<Instant> {
        self.flights.update(|table, wakes| {
            if table.is_stopping() || !table.owns(&self.page, &self.operation) {
                return None;
            }
            let entry = table.refresh_entry(&self.page, wakes)?;
            if let Some(waker) = waker {
                match &mut entry.ext_mut().driver_waker {
                    Some(current) if current.will_wake(waker) => {}
                    slot => *slot = Some(waker.clone()),
                }
            }
            let state = entry.state();
            let waiter = state.leader.and_then(|id| state.waiters.get(&id))?;
            if !matches!(state.phase, Phase::Acquiring) || waiter.scope.check().is_err() {
                return None;
            }
            state.leader_deadline()
        })
    }

    /// [`FlightOperation::cancellation_requested`], registering `cx` to be woken at the current
    /// leader's deadline. The worker expires the alarm through [`Flights::poll_with_context`].
    pub(super) fn poll_cancellation(&mut self, cx: &mut Context<'_>) -> bool {
        let Some(deadline) = self.leader_deadline(Some(cx.waker())) else {
            self.alarm = None;
            return true;
        };
        if self.alarm.as_ref().is_none_or(|(at, _)| *at != deadline) {
            match self.flights.admission_deadlines.register(deadline) {
                Ok(registration) => self.alarm = Some((deadline, registration)),
                Err(_) => return true,
            }
        }
        if let Some((_, alarm)) = &self.alarm
            && alarm.poll_expired(cx).is_ready()
        {
            // Expired between the refresh and now; the next poll re-evaluates.
            self.alarm = None;
            cx.waker().wake_by_ref();
        }
        false
    }
}

/// Original acquisition budget; the rule lives in racer-core.
/// ```compile_fail
/// use racer::read::flight::AcquisitionBudget;
/// fn duplicate(budget: AcquisitionBudget) { let _other = budget.clone(); }
/// ```
pub use racer_core::read::AcquisitionBudget;

/// Unique publication capability. Resources live in the worker table, not here.
/// Membership/authority are acquisition state, never part of the flight key.
/// ```compile_fail
/// use racer::read::flight::FlightLeader;
/// fn duplicate(leader: FlightLeader) { let _other = leader.clone(); }
/// ```
pub struct FlightLeader {
    pub(super) flights: Rc<Flights>,
    pub(super) page: PageId,
    /// Table fence, entry incarnation, acquisition generation, and elected waiter.
    pub(super) ticket: Ticket,
    pub(super) active: bool,
}

/// Per-request registration; cannot be stored in a completed flight/cache entry.
/// The non-cloneable OriginContext stays with its original request. Only a short
/// borrow is exposed to the elected driver; owned per-attempt sealing/I/O messages
/// retain their own admitted allocations until actual completion. Detach drops
/// these borrows, not another caller's context or outstanding transport owners.
/// ```compile_fail
/// use racer::read::flight::AcquisitionWaiter;
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
    cancellation: racer_io::CancelWait,
    // Waiters survive acquisition generations: match page, incarnation, and id.
    page: PageId,
    incarnation: u64,
    pub(super) id: u64,
    attached: bool,
}

/// No credentials, membership, budget, or API to obtain a leader capability.
/// ```compile_fail
/// use racer::read::flight::{CopyWaiter, FlightLeader};
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
    pub(crate) cancellation: &'a racer_io::CancelWait,
    pub membership: &'a std::sync::Arc<crate::topology::Membership>,
    pub budget: &'a mut AcquisitionBudget,
}

pub type AcquisitionFailure = racer_core::read::FailureKind<Error>;

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
    /// A canceled warm follower must also fence the supplier it joined.
    pub(super) async fn wait_operations(&mut self) -> Result<()> {
        poll_fn(|cx| {
            let flights = self.registration.flights.clone();
            let mut retired = None;
            let registration = &self.registration;
            let result = flights.update(|table, _wakes| {
                match registered(table, registration) {
                    Ok(()) => {}
                    Err(Error::StaleFlight) => return Poll::Ready(Ok(())),
                    Err(error) => return Poll::Ready(Err(error)),
                }
                if table.operations(&registration.page) == 0 {
                    return Poll::Ready(Ok(()));
                }
                table.park(&registration.page, registration.id, |slot| {
                    retired = store_waker(slot, cx.waker());
                });
                Poll::Pending
            });
            drop(retired);
            result
        })
        .await
    }
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
            let mut retired = None;
            let (page, id) = (&self.registration.page, self.registration.id);
            let budget = &self.budget;
            let result = flights.update(|table, wakes| {
                if let Err(error) = registered(table, &self.registration) {
                    return Poll::Ready(Err(error));
                }
                let event = match table.wait(
                    page,
                    id,
                    budget.deadline(),
                    budget.remaining_attempts(),
                    wakes,
                    |slot| retired = store_waker(slot, cx.waker()),
                ) {
                    Ok(event) => event,
                    Err(error) => return Poll::Ready(Err(stale(error))),
                };
                Poll::Ready(Ok(match event {
                    flight::Wait::Pending => return Poll::Pending,
                    flight::Wait::Ready(flight::Settled::Complete(result)) => {
                        AcquisitionEvent::Complete(result)
                    }
                    flight::Wait::Ready(flight::Settled::Partial(copy)) => {
                        AcquisitionEvent::Ciphertext(copy)
                    }
                    flight::Wait::Failed(error) => AcquisitionEvent::Failed(error),
                    flight::Wait::Lead => {
                        let ticket = table.ticket(page, id).expect("elected leader");
                        if let Some(entry) = table.entry_mut(page) {
                            entry.ext_mut().diagnostic = None;
                        }
                        AcquisitionEvent::Lead(FlightLeader {
                            flights: flights.clone(),
                            page: page.clone(),
                            ticket,
                            active: true,
                        })
                    }
                }))
            });
            drop(retired);
            result
        }))
    }

    pub fn acquisition<'a>(&'a mut self, leader: &FlightLeader) -> Result<AcquisitionContext<'a>> {
        self.scope.check()?;
        validate_context(&leader.page, self.context)?;
        if self.registration.id != leader.ticket.waiter
            || self.registration.page != leader.page
            || self.registration.incarnation != leader.ticket.incarnation
        {
            return Err(Error::StaleFlight);
        }
        self.registration.flights.update(|table, wakes| {
            registered(table, &self.registration)?;
            table
                .leader_entry(&leader.page, &leader.ticket, wakes)
                .map(drop)
                .map_err(stale)
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
            let mut retired = None;
            let registration = &self.registration;
            let result = registration.flights.update(|table, wakes| {
                if let Err(error) = registered(table, registration) {
                    return Poll::Ready(Err(error));
                }
                let parked =
                    match table.copy_wait(&registration.page, registration.id, wakes, |slot| {
                        retired = store_waker(slot, cx.waker());
                    }) {
                        Ok(parked) => parked,
                        Err(error) => return Poll::Ready(Err(stale(error))),
                    };
                match parked {
                    None => Poll::Pending,
                    Some(result) => Poll::Ready(result.map(|settled| match settled {
                        flight::Settled::Complete(result) => result.into(),
                        flight::Settled::Partial(copy) => AcquiredPage::Ciphertext(copy),
                    })),
                }
            });
            drop(retired);
            result
        }))
    }
}

impl Flights {
    /// The flights' clock.
    pub(crate) fn clock(&self) -> &racer_io::Clock {
        &self.clock
    }

    /// Attach a fresh generation without refreshing state or touching wake ownership.
    pub(super) fn driver_diagnostic(&self, leader: &FlightLeader) -> Option<DriverDiagnostic> {
        let diagnostic = DriverDiagnostic(
            Rc::new(std::cell::Cell::new(DriverObservation {
                stage: DriverStage::Starting,
                since: self.clock.now(),
                cancel_seen: None,
                abandoned: false,
            })),
            self.clock.clone(),
        );
        let mut table = self.table.borrow_mut();
        table.validate(&leader.page, &leader.ticket).ok()?;
        table.entry_mut(&leader.page)?.ext_mut().diagnostic = Some(diagnostic.clone());
        Some(diagnostic)
    }

    pub fn new(
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        availability: Rc<crate::control::Availability>,
        clock: racer_io::Clock,
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
            limits,
            table: RefCell::new(Table::with_fence(
                next_fence(),
                limits,
                split_result,
                clock.now(),
                FlightHashState::default(),
            )),
            waiting: racer_io::xthread::offload::AdmissionQueue::new(
                admission.policy().limits().queue_entries,
            ),
            admission_deadlines: Rc::default(),
            capacity_wake: Rc::default(),
            clock,
        }
    }

    pub fn with_limits(
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
        availability: Rc<crate::control::Availability>,
        limits: FlightLimits,
        clock: racer_io::Clock,
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
            limits,
            table: RefCell::new(Table::with_fence(
                next_fence(),
                limits,
                split_result,
                clock.now(),
                FlightHashState::default(),
            )),
            waiting: racer_io::xthread::offload::AdmissionQueue::new(
                admission.policy().limits().queue_entries,
            ),
            admission_deadlines: Rc::default(),
            capacity_wake: Rc::default(),
            clock,
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
            let state = entry.state();
            if let Phase::Complete(result) = &state.phase {
                return Some(result.copy());
            }
            state
                .partial
                .as_ref()
                .map(|p| p.copy.clone())
                .or_else(|| match &state.phase {
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

    /// Admit a new follower of shared local plaintext work with the same current
    /// cache and source-key checks as flight registration. Callers admitted
    /// before a key retirement keep their completion rights.
    pub(super) fn admit_local(
        &self,
        page: &PageId,
        key: Option<racer_wire::control::KeyId>,
    ) -> Result<()> {
        let cache = &page.version.object.cache;
        if !self.availability.cache(cache) {
            return Err(Error::Unavailable);
        }
        if key.is_some_and(|key| !self.availability.page(cache, key)) {
            return Err(Error::MissingKey);
        }
        Ok(())
    }

    /// Run one table transaction at the current time, then wake outside the borrow.
    fn update<T>(&self, f: impl FnOnce(&mut Table, &mut Vec<WakeTarget>) -> T) -> T {
        let mut wakes = Vec::new();
        let result = {
            let mut table = self.table.borrow_mut();
            table.set_now(self.clock.now());
            f(&mut table, &mut wakes)
        };
        for wake in wakes {
            wake.wake();
        }
        result
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
        if !self.table.borrow().contains(&page) {
            if self.clock.now() >= budget.deadline() {
                return Err(Error::DeadlineExceeded);
            }
            if budget.remaining_attempts() == 0 {
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
        let mut gate = crate::telemetry::JoinGate::Unclassified;
        let mut occupancy = (None, None);
        let mut quota_detail = None;
        let page_number = page.number.0;
        let result = self.update(|table, wakes| {
            if table.is_stopping() {
                return Err(Error::Cancelled);
            }
            self.admit_join(&page, table.entry(&page))?;
            match table.join_existing(&page, plaintext, wakes) {
                Ok(None) => {}
                Ok(Some(flight::Settled::Complete(result))) => {
                    return Ok(JoinedFlight::Complete(result));
                }
                Ok(Some(flight::Settled::Partial(copy))) => {
                    return Ok(JoinedFlight::Ciphertext(copy));
                }
                Err(reject) => {
                    if reject == flight::JoinReject::WaiterCap {
                        gate = crate::telemetry::JoinGate::WaiterCap;
                        occupancy = (
                            Some(table.waiters(&page)),
                            Some(self.limits.waiters_per_flight),
                        );
                    } else {
                        gate = crate::telemetry::JoinGate::ExistingFailed;
                    }
                    return Err(reject.error());
                }
            }
            flight::join_budget_gate(
                self.clock.now(),
                budget.deadline(),
                budget.remaining_attempts(),
                table
                    .entry(&page)
                    .is_some_and(|e| e.state().partial.is_some()),
            )?;
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
            let id = table.next_waiter_id();
            if !table.contains(&page) {
                if table.entry_capacity().is_err() {
                    gate = crate::telemetry::JoinGate::EntryCap;
                    occupancy = (Some(table.len()), Some(self.limits.entries));
                    return Err(Error::Overloaded);
                }
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
                let ext = EntryExt {
                    created: self.clock.now(),
                    diagnostic: None,
                    driver_waker: None,
                    _reservation: flight,
                };
                table.insert(page.clone(), ext, ());
            }
            table.register_as(
                &page,
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    budget_deadline: budget.deadline(),
                    _reservation: reservation,
                },
                true,
                plaintext,
            );
            let incarnation = table.entry(&page).expect("registered").incarnation();
            Ok(JoinedFlight::Waiter(AcquisitionWaiter {
                registration: Registration {
                    flights: self.clone(),
                    cancellation,
                    page: page.clone(),
                    incarnation,
                    id,
                    attached: true,
                },
                context,
                scope,
                membership,
                budget,
            }))
        });
        if result.is_ok() {
            self.waiting.wake_if(
                self.admission.policy().limits().queue_entries.get(),
                |waiting| waiting == &page,
            );
        }
        if matches!(result, Err(Error::Overloaded | Error::FlightFull)) {
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
        if self.table.borrow().contains(&page) {
            return self.join(page, membership, context, scope, budget);
        }
        if self.table.borrow().is_stopping() {
            return Err(Error::Cancelled);
        }
        self.admit_join(&page, None)?;
        if self.clock.now() >= budget.deadline() {
            return Err(Error::DeadlineExceeded);
        }
        if budget.remaining_attempts() == 0 {
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
        let cutoff = self.clock.now() + std::time::Duration::from_millis(100);
        let deadline = cutoff.min(scope.deadline.0).min(budget.deadline());
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
            let now = self.clock.now();
            if now >= budget.deadline() {
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
            self.admit_join(&page, self.table.borrow().entry(&page))?;
            if self.table.borrow().contains(&page) {
                return Poll::Ready(Ok(None));
            }
            if budget.remaining_attempts() == 0 {
                return Poll::Ready(Err(Error::Unavailable));
            }
            if ticket.is_none() {
                let entered = match self.waiting.enter(page.clone(), self.clock.now(), cx) {
                    Ok(ticket) => ticket,
                    Err(error) => {
                        reason = if error == racer_io::Error::Overloaded {
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
                age_us: age(entry.ext().created),
                lifecycle: entry.state().phase.state(),
                stage: entry
                    .ext()
                    .diagnostic
                    .as_ref()
                    .map(|diagnostic| diagnostic.0.get().stage),
                stage_age_us: entry
                    .ext()
                    .diagnostic
                    .as_ref()
                    .map(|diagnostic| age(diagnostic.0.get().since)),
                cancel_request_seen_age_us: entry
                    .ext()
                    .diagnostic
                    .as_ref()
                    .and_then(|diagnostic| diagnostic.0.get().cancel_seen.map(age)),
                driver_abandoned: entry
                    .ext()
                    .diagnostic
                    .as_ref()
                    .is_some_and(|diagnostic| diagnostic.0.get().abandoned),
                waiters: entry.state().waiters.len(),
                operations: entry.operations(),
                completing: entry.completing(),
            })
        });
        crate::telemetry::FlightExpiry {
            observed_unix_ms: crate::telemetry::timestamp(&self.clock, now),
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
            let Some(entry) = table.entry(page) else {
                return Ok(JoinedCopy::Miss);
            };
            if self.admit_join(page, Some(entry)).is_err() {
                return Ok(JoinedCopy::Miss);
            }
            match table.join_copy_existing(page, wakes) {
                Ok(flight::CopyJoin::Register) => {}
                Ok(flight::CopyJoin::Miss) => return Ok(JoinedCopy::Miss),
                Ok(flight::CopyJoin::Settled(flight::Settled::Complete(result))) => {
                    return Ok(JoinedCopy::Complete(result));
                }
                Ok(flight::CopyJoin::Settled(flight::Settled::Partial(copy))) => {
                    return Ok(JoinedCopy::Ciphertext(copy));
                }
                Err(error) => {
                    gate = crate::telemetry::JoinGate::WaiterCap;
                    occupancy = (
                        Some(table.waiters(page)),
                        Some(self.limits.waiters_per_flight),
                    );
                    return Err(error);
                }
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
            let id = table.next_waiter_id();
            table.register_as(
                page,
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    budget_deadline: scope.deadline.0,
                    _reservation: reservation,
                },
                false,
                false,
            );
            let incarnation = table
                .entry(page)
                .expect("existing copy flight")
                .incarnation();
            Ok(JoinedCopy::Waiter(CopyWaiter {
                registration: Registration {
                    flights: self.clone(),
                    cancellation,
                    page: page.clone(),
                    incarnation,
                    id,
                    attached: true,
                },
                scope,
            }))
        });
        if matches!(result, Err(Error::Overloaded | Error::FlightFull)) {
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

    /// Whether `page` has an acquisition running on this worker, so a new
    /// waiter would join it (successor hints, `racer_core::read::hints`).
    pub(crate) fn acquiring(&self, page: &PageId) -> bool {
        self.table.borrow().acquiring(page)
    }

    /// Let `leader`'s running operation continue for another live acquisition waiter when `leader`
    /// expires or is canceled. Only for drivers whose caller does not fence on the operation.
    pub(crate) fn allow_handover(&self, leader: &FlightLeader) -> Result<()> {
        self.update(|table, wakes| {
            leader_entry(table, leader, wakes)?;
            table
                .allow_handover(&leader.page, leader.ticket.waiter, wakes)
                .map_err(stale)
        })
    }

    pub(crate) fn ciphertext_for(&self, leader: &FlightLeader) -> Result<Option<UnverifiedPage>> {
        self.update(|table, wakes| Ok(leader_entry(table, leader, wakes)?.state().partial.clone()))
    }

    pub(crate) fn discard_ciphertext(&self, leader: &FlightLeader) -> Result<()> {
        self.update(|table, wakes| {
            *leader_entry(table, leader, wakes)?.partial_mut() = None;
            Ok(())
        })
    }

    pub(crate) fn publish_acquired(
        &self,
        mut leader: FlightLeader,
        page: AcquiredPage,
    ) -> Result<FlightState> {
        self.update(|table, wakes| {
            leader_entry(table, &leader, wakes)?;
            page.validate_for(&leader.page)?;
            leader.active = false;
            settle_leader(table, &leader, Ok(page), wakes)
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
            leader_entry(table, &leader, wakes)?;
            leader.active = false;
            settle_leader(table, &leader, Err(failure), wakes)
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
        crate::rt::drivers::poll(cx, work_budget);
        self.admission_deadlines.poll(self.clock.now(), work_budget);
        self.sweep_budgeted(work_budget)
    }

    /// Earliest live registration deadline for the worker's timer queue. Poll
    /// flights at this instant even if no transport completion wakes the worker.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.table
            .borrow()
            .next_deadline()
            .into_iter()
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
        self.update(|table, wakes| table.stop(wakes));
        self.waiting
            .wake_if(self.admission.policy().limits().queue_entries.get(), |_| {
                true
            });
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(move |cx| {
                cancellation.register(cx.waker());
                self.poll_with_context(cx, self.limits.entries)?;
                let waker = WakeTarget(cx.waker().clone());
                let mut table = self.table.borrow_mut();
                if table.is_empty() && self.waiting.is_empty() && crate::rt::drivers::pending() == 0
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
        resources: crate::telemetry::primitives::Lease,
    ) -> Result<FlightOperation> {
        self.update(|table, wakes| {
            leader_entry(table, leader, wakes)?;
            let operation = table.retain_operation_with(
                &leader.page,
                leader.ticket.waiter,
                || {
                    let reservation = self.admission.reserve(
                        Some(&leader.page.version.object.cache),
                        ResourceClass::ControlProgress,
                        1,
                    )?;
                    Ok(Retained {
                        _resources: resources,
                        _reservation: reservation,
                    })
                },
                wakes,
            )?;
            Ok(FlightOperation {
                flights: self.clone(),
                page: leader.page.clone(),
                operation,
                alarm: None,
            })
        })
    }

    /// Retain the operation a fill driver is about to submit, first enabling
    /// handover when requested. Handover is enabled before the operation is
    /// retained so that no fallible step runs while an unsubmitted
    /// `FlightOperation` is held: dropping one keeps its table slot and
    /// admission charge (see `FlightOperation`), which would leave the flight
    /// fenced and block `drain`. Enabling handover without a running
    /// operation is inert, so a failed retain leaves nothing behind.
    pub(crate) fn retain_driver_operation(
        self: &Rc<Self>,
        leader: &FlightLeader,
        resources: crate::telemetry::primitives::Lease,
        handover: bool,
    ) -> Result<FlightOperation> {
        if handover {
            self.allow_handover(leader)?;
        }
        self.retain_operation(leader, resources)
    }

    pub(super) fn complete_operation(
        &self,
        page: &PageId,
        operation: flight::Operation,
    ) -> Result<()> {
        // Drop caller-owned resource bundles outside the table borrow. The slot stays occupied
        // while their destructors run: they may reenter the worker, but cannot cause an
        // overlapping election.
        let resources = self.update(|table, _wakes| table.take_operation(page, &operation));
        drop(resources.map_err(stale)?);
        let removed = self.update(|table, wakes| table.finish_operation(page, operation, wakes));
        drop(removed.map_err(stale)?);
        Ok(())
    }
}

/// The refreshed entry `leader` still leads, or `StaleFlight`.
fn leader_entry<'a>(
    table: &'a mut Table,
    leader: &FlightLeader,
    wakes: &mut Vec<WakeTarget>,
) -> Result<&'a mut Entry> {
    table
        .leader_entry(&leader.page, &leader.ticket, wakes)
        .map_err(stale)
}

/// Publish or fail the validated `leader`'s acquisition and report the resulting lifecycle.
fn settle_leader(
    table: &mut Table,
    leader: &FlightLeader,
    result: std::result::Result<AcquiredPage, AcquisitionFailure>,
    wakes: &mut Vec<WakeTarget>,
) -> Result<FlightState> {
    table
        .finish(&leader.page, leader.ticket.waiter, result, wakes)
        .map_err(stale)?;
    Ok(table
        .entry(&leader.page)
        .map_or(FlightState::Removed, |entry| entry.state().phase.state()))
}

fn split_result(page: AcquiredPage) -> Published<PageResult, UnverifiedPage> {
    match page {
        AcquiredPage::Plaintext(page) => Published::Complete(page),
        AcquiredPage::Ciphertext(page) => Published::Partial(page),
    }
}

/// A detached or canceled waiter may have been the last one the running
/// driver serves; let it re-check instead of waiting for its alarm or I/O.
fn wake_driver(entry: &mut Entry, wakes: &mut Vec<WakeTarget>) {
    if entry.operations() > 0
        && let Some(waker) = entry.ext_mut().driver_waker.take()
    {
        wakes.push(WakeTarget(waker));
    }
}

fn registered(table: &Table, registration: &Registration) -> Result<()> {
    if table.is_stopping() {
        return Err(Error::Cancelled);
    }
    if !registration.attached
        || !table.registered(
            &registration.page,
            registration.incarnation,
            registration.id,
        )
    {
        return Err(Error::StaleFlight);
    }
    Ok(())
}

impl Registration {
    fn cancel(&self, error: Error) {
        self.flights.update(|table, wakes| {
            if registered(table, self).is_ok() {
                table.cancel_with(&self.page, self.id, error, wakes);
                if let Some(entry) = table.entry_mut(&self.page) {
                    wake_driver(entry, wakes);
                }
            }
        });
    }

    fn detach(&mut self) -> Result<FlightState> {
        let state = self.flights.update(|table, wakes| {
            registered(table, self)?;
            let removed = table.detach(&self.page, self.id, wakes);
            if removed.is_some() {
                return Ok((FlightState::Removed, removed));
            }
            let entry = table.entry_mut(&self.page).expect("kept entry");
            wake_driver(entry, wakes);
            Ok((entry.state().phase.state(), None))
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
                if table.validate(&self.page, &self.ticket).is_ok() {
                    table.abandon(&self.page, self.ticket.waiter, wakes);
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
        let clock = crate::rt::environment::SimulationClock::new(920);
        let sim_env = clock.environment(0);
        let _environment = sim_env.enter();
        let sim_ctx = sim_env.worker_ctx();
        let make = || {
            DriverDiagnostic(
                Rc::new(std::cell::Cell::new(DriverObservation {
                    stage: DriverStage::Starting,
                    since: sim_ctx.clock.now(),
                    cancel_seen: None,
                    abandoned: false,
                })),
                sim_ctx.clock.clone(),
            )
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
            racer_io::Clock::host(),
        ));
        let page = crate::memory::tests::bundle_for(
            &admission,
            crate::model::VersionMetadata {
                version: crate::model::ObjectVersion {
                    object: crate::model::ObjectId {
                        cache: racer_wire::control::CacheId(
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
            .enter(
                page.clone(),
                flights.clock().now(),
                &Context::from_waker(Waker::noop()),
            )
            .unwrap();
        let head_id = head.sequence();
        let reentrant = Waker::from(std::sync::Arc::new(Reenter));
        let tail = flights
            .waiting
            .enter(
                page,
                flights.clock().now(),
                &Context::from_waker(&reentrant),
            )
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
