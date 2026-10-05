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
    fence: Fence,
    state: FlightCore,
    operations: coalesce::flight::Operations<Retained, FlightHashState>,
    _reservation: flow_control::Charge<AdmissionPolicy>,
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
        table.stopping
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
    cancellation: uring_runtime::deadline::CancellationRegistration,
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
    pub(crate) cancellation: &'a uring_runtime::deadline::CancellationRegistration,
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
            flights.update(|table, wakes| {
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
                if !waiter.complete {
                    if let Some(copy) = &entry.state.partial {
                        return Poll::Ready(Ok(AcquisitionEvent::Ciphertext(copy.clone())));
                    }
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
                    return Poll::Ready(Ok(AcquisitionEvent::Lead(FlightLeader {
                        flights: flights.clone(),
                        fence: entry.fence.clone(),
                        caller: self.registration.id,
                        active: true,
                    })));
                }
                coalesce::flight::state::store_waker(&mut waiter.waker, cx.waker());
                Poll::Pending
            })
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
            self.registration.flights.update(|table, wakes| {
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
                coalesce::flight::state::store_waker(&mut waiter.waker, cx.waker());
                Poll::Pending
            })
        }))
    }
}

impl Flights {
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
            admission,
            availability,
            owner: Rc::new(()),
            limits,
            table: RefCell::new(Table::default()),
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
            admission,
            availability,
            owner: Rc::new(()),
            limits,
            table: RefCell::new(Table::default()),
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
        self.update(|table, wakes| {
            if table.stopping {
                return Err(Error::Cancelled);
            }
            self.admit_join(&page, table.get(&page))?;
            if let Some(entry) = table.get_mut(&page) {
                refresh(entry, wakes);
                if let Phase::Complete(result) = &entry.phase {
                    return Ok(JoinedFlight::Complete(result.clone()));
                }
                if !plaintext {
                    if let Some(copy) = &entry.partial {
                        return Ok(JoinedFlight::Ciphertext(copy.clone()));
                    }
                }
                if let Phase::Failed(error) = entry.phase {
                    return Err(error);
                }
                if entry.waiters.len() >= self.limits.waiters_per_flight {
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
            let reservation = self.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Waiter,
                1,
            )?;
            let id = table.next_waiter.next_id().map_err(|_| Error::Overloaded)?;
            if !table.contains_key(&page) {
                if table.len() >= self.limits.entries {
                    return Err(Error::Overloaded);
                }
                let flight = self.admission.reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Flight,
                    1,
                )?;
                let identity = table
                    .identity(self.owner.clone())
                    .map_err(|_| Error::Overloaded)?;
                table.insert(
                    page.clone(),
                    Entry {
                        fence: Fence {
                            page: page.clone(),
                            identity,
                        },
                        state: FlightCore::default(),
                        operations: coalesce::flight::Operations::default(),
                        _reservation: flight,
                    },
                );
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
        })
    }

    /// Never creates an entry, elects a leader, or revives failed/draining work.
    /// It may observe retries funded by independently registered acquisition callers.
    pub fn join_copy<'a>(
        self: &Rc<Self>,
        page: &PageId,
        scope: &'a RequestScope,
    ) -> Result<JoinedCopy<'a>> {
        scope.check()?;
        self.update(|table, wakes| {
            if table.stopping {
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
                return Err(Error::Overloaded);
            }
            let cancellation = scope.cancellation.subscribe()?;
            let reservation = self.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Waiter,
                1,
            )?;
            let id = table.next_waiter.next_id().map_err(|_| Error::Overloaded)?;
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
        })
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
            .min()
    }

    fn sweep_budgeted(&self, work_budget: usize) -> Result<()> {
        self.update(|table, wakes| {
            table.sweep(work_budget, wakes);
            Ok(())
        })
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
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(move |cx| {
                cancellation.register(cx.waker());
                self.poll_with_context(cx, self.limits.entries)?;
                let mut table = self.table.borrow_mut();
                if table.is_empty() && uring_runtime::drivers::pending() == 0 {
                    return Poll::Ready(Ok(()));
                }
                scope.check()?;
                coalesce::flight::state::store_waker(&mut table.drain_waker, cx.waker());
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
            let id = table
                .next_operation
                .next_id()
                .map_err(|_| Error::Overloaded)?;
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
        self.update(|table, _wakes| {
            table.remove_quiescent(&fence.page);
            Ok(())
        })
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
    fn incarnation(&self) -> u64 {
        self.fence.incarnation
    }

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
    if table.stopping {
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
            if table.remove_quiescent(&self.fence.page) {
                table.notify_drain(wakes);
                Ok(FlightState::Removed)
            } else {
                Ok(state)
            }
        });
        self.attached = false;
        state
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
                if let Some(entry) = table.get_mut(&self.fence.page) {
                    if validate_leader(entry, self).is_ok() {
                        revoke(entry, Error::Cancelled, wakes);
                    }
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
