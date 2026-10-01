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
use crate::runtime::collections::HashMap;
use crate::{
    error::{Error, Operation, Result},
    memory::page::{AcquiredPage, PageResult, UnverifiedPage},
    model::{OriginContext, PageId, ResourceClass},
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
    },
    topology::membership::MembershipLease,
};
use std::{
    any::Any,
    cell::RefCell,
    collections::BTreeMap,
    future::poll_fn,
    rc::Rc,
    task::{Context, Poll, Waker},
    time::Instant,
};

pub struct Flights {
    admission: Rc<Admission>,
    availability: Rc<crate::control::state::Availability>,
    owner: Rc<()>,
    limits: FlightLimits,
    table: RefCell<Table>,
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

#[derive(Default)]
struct Table {
    entries: HashMap<PageId, Entry>,
    sweep: BTreeMap<u64, PageId>,
    sweep_cursor: u64,
    next_incarnation: u64,
    next_waiter: u64,
    next_operation: u64,
    stopping: bool,
    drain_waker: Option<Waker>,
}
struct Entry {
    fence: Fence,
    phase: Phase,
    leader: Option<u64>,
    waiters: BTreeMap<u64, WaiterRecord>,
    deadlines: BTreeMap<(Instant, u64), ()>,
    waiter_cursor: u64,
    operations: HashMap<u64, Option<Retained>>,
    ciphertext: Option<UnverifiedPage>,
    _reservation: Reservation,
}
struct WaiterRecord {
    scope: RequestScope,
    acquisition: bool,
    plaintext: bool,
    budget_deadline: Instant,
    issued: bool,
    error: Option<Error>,
    waker: Option<Waker>,
    _reservation: Reservation,
}
struct Retained {
    _resources: Box<dyn Any>,
    _reservation: Reservation,
}
enum Outcome {
    Published(AcquiredPage),
    Failed(Error),
    Retry,
}
enum Phase {
    Acquiring,
    RetryPending,
    Draining(Outcome),
    Complete(PageResult),
    Failed(Error),
}
impl Phase {
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
    fence: Fence,
    id: u64,
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
            || table.entries.get(&self.fence.page).is_none_or(|entry| {
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
struct Fence {
    owner: Rc<()>,
    page: PageId,
    incarnation: u64,
    generation: u64,
}
impl Fence {
    fn validate(&self, current: &Self) -> Result<()> {
        if !Rc::ptr_eq(&self.owner, &current.owner)
            || self.page != current.page
            || self.incarnation != current.incarnation
            || self.generation != current.generation
        {
            return Err(Error::StaleFlight);
        }
        Ok(())
    }
}

/// Unique publication capability. Resources live in the worker table, not here.
/// Membership/authority are acquisition state, never part of the flight key.
/// ```compile_fail
/// use racer_dataplane::read::flight::FlightLeader;
/// fn duplicate(leader: FlightLeader) { let _other = leader.clone(); }
/// ```
pub struct FlightLeader {
    flights: Rc<Flights>,
    fence: Fence,
    caller: u64,
    active: bool,
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
    registration: Registration,
    context: &'a OriginContext,
    scope: &'a RequestScope,
    membership: MembershipLease,
    budget: &'a mut AcquisitionBudget,
}

struct Registration {
    flights: Rc<Flights>,
    cancellation: crate::runtime::deadline::CancellationRegistration,
    // Waiters survive acquisition generations. Match owner/page/incarnation/id
    // for registration, then refresh this generation only on a new election.
    fence: Fence,
    id: u64,
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
    pub(crate) cancellation: &'a crate::runtime::deadline::CancellationRegistration,
    pub membership: &'a MembershipLease,
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
                let waiter = entry.waiters.get_mut(&self.registration.id).unwrap();
                if let Some(error) = waiter.error {
                    return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
                }
                if let Phase::Complete(result) = &entry.phase {
                    return Poll::Ready(Ok(AcquisitionEvent::Complete(result.clone())));
                }
                if !waiter.plaintext {
                    if let Some(copy) = &entry.ciphertext {
                        return Poll::Ready(Ok(AcquisitionEvent::Ciphertext(copy.clone())));
                    }
                }
                if let Phase::Failed(error) = entry.phase {
                    return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
                }
                if matches!(entry.phase, Phase::RetryPending) && !waiter.issued {
                    if crate::runtime::environment::now() >= self.budget.deadline
                        || (self.budget.attempts == 0 && entry.ciphertext.is_none())
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
                    if entry.fence.generation >= flights.limits.generations_per_flight {
                        entry.phase = Phase::Draining(Outcome::Failed(Error::Unavailable));
                        settle(entry, wakes);
                        return Poll::Ready(Ok(AcquisitionEvent::Failed(Error::Unavailable)));
                    }
                    entry.fence.generation += 1;
                    entry.phase = Phase::Acquiring;
                    entry.leader = Some(self.registration.id);
                    waiter.issued = true;
                    self.registration.fence.generation = entry.fence.generation;
                    return Poll::Ready(Ok(AcquisitionEvent::Lead(FlightLeader {
                        flights: flights.clone(),
                        fence: entry.fence.clone(),
                        caller: self.registration.id,
                        active: true,
                    })));
                }
                store_waker(&mut waiter.waker, cx);
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
                let waiter = entry.waiters.get_mut(&self.registration.id).unwrap();
                if let Some(error) = waiter.error {
                    return Poll::Ready(Err(error));
                }
                if let Phase::Failed(error) = entry.phase {
                    return Poll::Ready(Err(error));
                }
                if let Phase::Complete(result) = &entry.phase {
                    return Poll::Ready(Ok(result.clone().into()));
                }
                if let Some(copy) = &entry.ciphertext {
                    return Poll::Ready(Ok(AcquiredPage::Ciphertext(copy.clone())));
                }
                store_waker(&mut waiter.waker, cx);
                Poll::Pending
            })
        }))
    }
}

impl Flights {
    pub fn new(
        admission: Rc<Admission>,
        availability: Rc<crate::control::state::Availability>,
    ) -> Self {
        let limits = FlightLimits {
            entries: admission.limits().flights.get(),
            waiters_per_flight: admission.limits().waiters_per_flight.get(),
            operations_per_flight: admission.limits().queue_entries.get(),
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
        admission: Rc<Admission>,
        availability: Rc<crate::control::state::Availability>,
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
                .ciphertext
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
        let mut wakes = Vec::new();
        let result = f(&mut self.table.borrow_mut(), &mut wakes);
        for waker in wakes {
            waker.wake();
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
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Result<JoinedFlight<'a>> {
        self.join_for(page, membership, context, scope, budget, true)
    }

    pub(crate) fn join_for<'a>(
        self: &Rc<Self>,
        page: PageId,
        membership: MembershipLease,
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
            self.admit_join(&page, table.entries.get(&page))?;
            if let Some(entry) = table.entries.get_mut(&page) {
                refresh(entry, wakes);
                if let Phase::Complete(result) = &entry.phase {
                    return Ok(JoinedFlight::Complete(result.clone()));
                }
                if !plaintext {
                    if let Some(copy) = &entry.ciphertext {
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
            if crate::runtime::environment::now() >= budget.deadline {
                return Err(Error::DeadlineExceeded);
            }
            if budget.attempts == 0
                && table
                    .entries
                    .get(&page)
                    .is_none_or(|e| e.ciphertext.is_none())
            {
                return Err(Error::Unavailable);
            }
            let cancellation = scope.cancellation.subscribe()?;
            let reservation = self.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Waiter,
                1,
            )?;
            let id = next(&mut table.next_waiter)?;
            if !table.entries.contains_key(&page) {
                if table.entries.len() >= self.limits.entries {
                    return Err(Error::Overloaded);
                }
                let flight = self.admission.reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Flight,
                    1,
                )?;
                let incarnation = next(&mut table.next_incarnation)?;
                table.entries.insert(
                    page.clone(),
                    Entry {
                        fence: Fence {
                            owner: self.owner.clone(),
                            page: page.clone(),
                            incarnation,
                            generation: 0,
                        },
                        phase: Phase::RetryPending,
                        leader: None,
                        waiters: BTreeMap::new(),
                        deadlines: BTreeMap::new(),
                        waiter_cursor: 0,
                        operations: HashMap::default(),
                        ciphertext: None,
                        _reservation: flight,
                    },
                );
                table.sweep.insert(incarnation, page.clone());
            }
            let entry = table.entries.get_mut(&page).unwrap();
            entry
                .deadlines
                .insert((scope.deadline.0.min(budget.deadline), id), ());
            entry.waiters.insert(
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    acquisition: true,
                    plaintext,
                    budget_deadline: budget.deadline,
                    issued: false,
                    error: None,
                    waker: None,
                    _reservation: reservation,
                },
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
            let Some(entry) = table.entries.get_mut(page) else {
                return Ok(JoinedCopy::Miss);
            };
            if self.admit_join(page, Some(entry)).is_err() {
                return Ok(JoinedCopy::Miss);
            }
            refresh(entry, wakes);
            if let Phase::Complete(result) = &entry.phase {
                return Ok(JoinedCopy::Complete(result.clone()));
            }
            if let Some(copy) = &entry.ciphertext {
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
            let id = next(&mut table.next_waiter)?;
            entry.deadlines.insert((scope.deadline.0, id), ());
            entry.waiters.insert(
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    acquisition: false,
                    plaintext: false,
                    budget_deadline: scope.deadline.0,
                    issued: false,
                    error: None,
                    waker: None,
                    _reservation: reservation,
                },
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
        self.update(|table, wakes| Ok(self.leader_entry(table, leader, wakes)?.ciphertext.clone()))
    }

    pub(crate) fn discard_ciphertext(&self, leader: &FlightLeader) -> Result<()> {
        self.update(|table, wakes| {
            self.leader_entry(table, leader, wakes)?.ciphertext = None;
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
            entry.phase = Phase::Draining(Outcome::Published(page));
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
            entry.phase = Phase::Draining(match failure {
                AcquisitionFailure::OriginRejected => {
                    entry.waiters.get_mut(&leader.caller).unwrap().error =
                        Some(Error::OriginRejected);
                    Outcome::Retry
                }
                AcquisitionFailure::OriginForbidden => {
                    entry.waiters.get_mut(&leader.caller).unwrap().error =
                        Some(Error::OriginForbidden);
                    Outcome::Retry
                }
                AcquisitionFailure::Terminal(error) => Outcome::Failed(error),
            });
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
        super::drivers::poll(cx, work_budget);
        self.sweep_budgeted(work_budget)
    }

    /// Earliest live registration deadline for the worker's timer queue. Poll
    /// flights at this instant even if no transport completion wakes the worker.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.table
            .borrow()
            .entries
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
            for _ in 0..work_budget.min(table.sweep.len()) {
                let Some((&id, page)) = table
                    .sweep
                    .range((
                        std::ops::Bound::Excluded(table.sweep_cursor),
                        std::ops::Bound::Unbounded,
                    ))
                    .next()
                    .or_else(|| table.sweep.first_key_value())
                else {
                    break;
                };
                let page = page.clone();
                table.sweep_cursor = id;
                if let Some(entry) = table.entries.get_mut(&page) {
                    // One cancellation candidate per entry, independent of fan-in.
                    let waiter = entry
                        .waiters
                        .range((
                            std::ops::Bound::Excluded(entry.waiter_cursor),
                            std::ops::Bound::Unbounded,
                        ))
                        .next()
                        .or_else(|| entry.waiters.first_key_value())
                        .map(|(id, _)| *id);
                    if let Some(id) = waiter {
                        entry.waiter_cursor = id;
                        refresh_waiter(entry, id, wakes);
                    }
                    refresh(entry, wakes);
                    if entry.waiters.is_empty() && entry.operations.is_empty() {
                        table.entries.remove(&page);
                        table.sweep.remove(&id);
                    }
                }
            }
            if table.entries.is_empty() {
                if let Some(waker) = table.drain_waker.take() {
                    wakes.push(waker);
                }
            }
            Ok(())
        })
    }

    /// Stop new joins/elections, detach callers, request cancellation, and keep
    /// reaping accepted work. Expiring shutdown scope cannot free live owners.
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        // Initiate shutdown synchronously: dropping the returned future cannot
        // reopen admission or discard accepted work.
        self.update(|table, wakes| {
            table.stopping = true;
            for entry in table.entries.values_mut() {
                for waiter in entry.waiters.values_mut() {
                    waiter.error = Some(Error::Cancelled);
                }
                entry.phase = Phase::Draining(Outcome::Failed(Error::Cancelled));
                notify(entry, wakes);
                entry.waiters.clear();
                entry.deadlines.clear();
                settle(entry, wakes);
            }
        });
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            poll_fn(move |cx| {
                cancellation.register(cx.waker());
                self.poll_with_context(cx, self.limits.entries)?;
                let mut table = self.table.borrow_mut();
                if table.entries.is_empty() && super::drivers::pending() == 0 {
                    return Poll::Ready(Ok(()));
                }
                scope.check()?;
                store_waker(&mut table.drain_waker, cx);
                Poll::Pending
            })
            .await
        })
    }

    /// Retain actual resources before submission. The worker completing the I/O
    /// owns the token; the request future must not report cancellation as completion.
    pub fn retain_operation<T: 'static>(
        self: &Rc<Self>,
        leader: &FlightLeader,
        resources: T,
    ) -> Result<FlightOperation> {
        self.update(|table, wakes| {
            let id = next(&mut table.next_operation)?;
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
                Some(Retained {
                    _resources: Box::new(resources),
                    _reservation: reservation,
                }),
            );
            Ok(FlightOperation {
                flights: self.clone(),
                fence: entry.fence.clone(),
                id,
            })
        })
    }

    fn complete_operation(&self, fence: &Fence, id: u64) -> Result<()> {
        // Drop caller-owned resource bundles outside the table borrow.
        let resources = self.update(|table, _wakes| {
            let entry = table
                .entries
                .get_mut(&fence.page)
                .ok_or(Error::StaleFlight)?;
            fence.validate(&entry.fence)?;
            entry
                .operations
                .get_mut(&id)
                .and_then(Option::take)
                .ok_or(Error::StaleFlight)
        })?;
        drop(resources);
        self.update(|table, wakes| {
            let entry = table
                .entries
                .get_mut(&fence.page)
                .ok_or(Error::StaleFlight)?;
            fence.validate(&entry.fence)?;
            // Keep the slot occupied while resource destructors run: they may
            // reenter the worker, but cannot cause an overlapping election.
            entry.operations.remove(&id).ok_or(Error::StaleFlight)?;
            refresh(entry, wakes);
            if let Some(waker) = table.drain_waker.take() {
                wakes.push(waker);
            }
            Ok(())
        })?;
        self.update(|table, _wakes| {
            if table
                .entries
                .get(&fence.page)
                .is_some_and(|entry| entry.waiters.is_empty() && entry.operations.is_empty())
            {
                if let Some(entry) = table.entries.remove(&fence.page) {
                    table.sweep.remove(&entry.fence.incarnation);
                }
            }
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
            .entries
            .get_mut(&leader.fence.page)
            .ok_or(Error::StaleFlight)?;
        refresh(entry, wakes);
        validate_leader(entry, leader)?;
        Ok(entry)
    }
}

fn next(counter: &mut u64) -> Result<u64> {
    *counter = counter.checked_add(1).ok_or(Error::Overloaded)?;
    Ok(*counter)
}
fn store_waker(slot: &mut Option<Waker>, cx: &Context<'_>) {
    if slot.as_ref().is_none_or(|w| !w.will_wake(cx.waker())) {
        *slot = Some(cx.waker().clone());
    }
}
fn notify(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    for waiter in entry.waiters.values_mut() {
        if let Some(waker) = waiter.waker.take() {
            wakes.push(waker);
        }
    }
}
fn eligible(waiter: &WaiterRecord) -> bool {
    waiter.acquisition && !waiter.issued && waiter.error.is_none()
}
fn settle(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    if !entry.operations.is_empty() || !matches!(entry.phase, Phase::Draining(_)) {
        return;
    }
    let Phase::Draining(outcome) = std::mem::replace(&mut entry.phase, Phase::RetryPending) else {
        unreachable!();
    };
    entry.leader = None;
    entry.phase = match outcome {
        Outcome::Published(AcquiredPage::Plaintext(page)) => {
            entry.ciphertext = None;
            Phase::Complete(page)
        }
        Outcome::Published(AcquiredPage::Ciphertext(page)) => {
            entry.ciphertext = Some(page);
            for waiter in entry.waiters.values_mut() {
                if waiter.plaintext {
                    waiter.issued = false;
                }
            }
            Phase::RetryPending
        }
        Outcome::Failed(error) => Phase::Failed(error),
        Outcome::Retry if entry.waiters.values().any(eligible) => Phase::RetryPending,
        Outcome::Retry => Phase::Failed(Error::Unavailable),
    };
    notify(entry, wakes);
}
fn revoke(entry: &mut Entry, error: Error, wakes: &mut Vec<Waker>) {
    if let Some(waiter) = entry.leader.and_then(|id| entry.waiters.get_mut(&id)) {
        waiter.error = Some(error);
    }
    entry.phase = Phase::Draining(Outcome::Retry);
    notify(entry, wakes);
    settle(entry, wakes);
}
fn refresh(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    if let Some(id) = entry.leader {
        refresh_waiter(entry, id, wakes);
    }
    // Deadline work is bounded independently of the number of waiters. More
    // expired entries retain their index and are serviced on the next turn.
    for _ in 0..64 {
        let Some((&(deadline, id), _)) = entry.deadlines.first_key_value() else {
            break;
        };
        if deadline > crate::runtime::environment::now() {
            break;
        }
        entry.deadlines.remove(&(deadline, id));
        refresh_waiter(entry, id, wakes);
    }
    if matches!(entry.phase, Phase::Acquiring)
        && entry
            .leader
            .and_then(|id| entry.waiters.get(&id))
            .is_none_or(|w| w.error.is_some())
    {
        let error = entry
            .leader
            .and_then(|id| entry.waiters.get(&id))
            .and_then(|w| w.error)
            .unwrap_or(Error::Cancelled);
        revoke(entry, error, wakes);
    }
    settle(entry, wakes);
    if matches!(entry.phase, Phase::RetryPending)
        && entry.ciphertext.is_none()
        && !entry.waiters.values().any(eligible)
    {
        entry.phase = Phase::Draining(Outcome::Failed(Error::Unavailable));
        settle(entry, wakes);
    }
}
fn refresh_waiter(entry: &mut Entry, id: u64, wakes: &mut Vec<Waker>) {
    let Some(waiter) = entry.waiters.get_mut(&id) else {
        return;
    };
    if waiter.error.is_none() {
        waiter.error = waiter.scope.check().err().or_else(|| {
            (crate::runtime::environment::now() >= waiter.budget_deadline)
                .then_some(Error::DeadlineExceeded)
        });
    }
    if waiter.error.is_some() {
        entry
            .deadlines
            .remove(&(waiter.scope.deadline.0.min(waiter.budget_deadline), id));
        if let Some(waker) = waiter.waker.take() {
            wakes.push(waker);
        }
    }
}
fn validate_leader(entry: &Entry, leader: &FlightLeader) -> Result<()> {
    leader.fence.validate(&entry.fence)?;
    if !leader.active
        || !matches!(entry.phase, Phase::Acquiring)
        || entry.leader != Some(leader.caller)
    {
        return Err(Error::StaleFlight);
    }
    Ok(())
}
fn registered<'a>(table: &'a mut Table, registration: &Registration) -> Result<&'a mut Entry> {
    if table.stopping {
        return Err(Error::Cancelled);
    }
    let entry = table
        .entries
        .get_mut(&registration.fence.page)
        .ok_or(Error::StaleFlight)?;
    if !registration.attached
        || !Rc::ptr_eq(&registration.fence.owner, &entry.fence.owner)
        || registration.fence.incarnation != entry.fence.incarnation
        || !entry.waiters.contains_key(&registration.id)
    {
        return Err(Error::StaleFlight);
    }
    Ok(entry)
}
impl Registration {
    fn cancel(&self, error: Error) {
        self.flights.update(|table, wakes| {
            if let Ok(entry) = registered(table, self) {
                entry.waiters.get_mut(&self.id).unwrap().error = Some(error);
                refresh(entry, wakes);
            }
        });
    }
    fn detach(&mut self) -> Result<FlightState> {
        let state = self.flights.update(|table, wakes| {
            let entry = registered(table, self)?;
            if let Some(waiter) = entry.waiters.remove(&self.id) {
                entry
                    .deadlines
                    .remove(&(waiter.scope.deadline.0.min(waiter.budget_deadline), self.id));
            }
            refresh(entry, wakes);
            if entry.waiters.is_empty() && entry.operations.is_empty() {
                table.entries.remove(&self.fence.page);
                table.sweep.remove(&self.fence.incarnation);
                if let Some(waker) = table.drain_waker.take() {
                    wakes.push(waker);
                }
                Ok(FlightState::Removed)
            } else {
                Ok(entry.phase.state())
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
                if let Some(entry) = table.entries.get_mut(&self.fence.page) {
                    if validate_leader(entry, self).is_ok() {
                        revoke(entry, Error::Cancelled, wakes);
                    }
                }
            });
        }
    }
}

fn validate_context(page: &PageId, context: &OriginContext) -> Result<()> {
    if page.version.object != context.object {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}

#[cfg(test)]
mod tests;
