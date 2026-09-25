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
use crate::{
    error::{Error, Operation, Result},
    memory::page::PageResult,
    model::{context::OriginContext, identity::PageId, limits::ResourceClass},
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
    },
    topology::membership::MembershipLease,
};
use std::{
    any::Any,
    cell::RefCell,
    collections::{HashMap, VecDeque},
    future::poll_fn,
    rc::Rc,
    task::{Context, Poll, Waker},
    time::Instant,
};

pub struct Flights {
    admission: Rc<Admission>,
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
    sweep: VecDeque<PageId>,
    next_incarnation: u64,
    next_waiter: u64,
    next_operation: u64,
    stopping: bool,
    drain_waker: Option<Waker>,
}
struct Entry {
    fence: Fence,
    state: FlightState,
    leader: Option<u64>,
    waiters: HashMap<u64, WaiterRecord>,
    operations: HashMap<u64, Option<Retained>>,
    outcome: Option<Outcome>,
    result: Option<PageResult>,
    error: Option<Error>,
    drain_waker: Option<Waker>,
    _reservation: Reservation,
}
struct WaiterRecord {
    scope: RequestScope,
    acquisition: bool,
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
    Published(PageResult),
    Failed(Error),
    Retry,
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
                    || entry.state != FlightState::Acquiring
                    || entry
                        .leader
                        .and_then(|id| entry.waiters.get(&id))
                        .is_none_or(|waiter| waiter.scope.check().is_err())
            })
    }
}

/// Original logical-call budget, lent exclusively to one page acquisition at a
/// time. Retry never reconstructs it from defaults. Fanout must partition credits;
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
    charged_links: u8,
}
impl AcquisitionBudget {
    pub fn new(deadline: Instant, attempts: u32, links: u8) -> Self {
        Self {
            deadline,
            attempts,
            links,
            charged_links: 0,
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
        self.charged_links += links;
        Ok(())
    }

    /// Reconcile unused links only after a verified completed route. Refunds
    /// cannot exceed prior debits; failed exchanges must retain their full debit.
    pub fn refund_links(&mut self, links: u8) -> Result<()> {
        let charged = self
            .charged_links
            .checked_sub(links)
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
        Ok(Self::new(self.deadline, attempts, links))
    }

    pub fn transfer(&mut self) -> Self {
        let attempts = std::mem::take(&mut self.attempts);
        let links = std::mem::take(&mut self.links);
        Self {
            deadline: self.deadline,
            attempts,
            links,
            charged_links: std::mem::take(&mut self.charged_links),
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
        self.charged_links += links;
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
}
pub enum JoinedCopy<'a> {
    Miss,
    Waiter(CopyWaiter<'a>),
    Complete(PageResult),
}
pub enum AcquisitionEvent {
    /// Initial election or retry. Only this caller can lend the matching context.
    Lead(FlightLeader),
    Complete(PageResult),
    Failed(Error),
}

/// Borrowed only after checking the leader, registration, page/object association,
/// cancellation, and generation. It does not grant origin permission: Fill must
/// resolve candidates under this membership and obtain a new OriginAuthority.
pub struct AcquisitionContext<'a> {
    pub origin: &'a OriginContext,
    pub scope: &'a RequestScope,
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

/// Non-cloneable observation of retained canceled/abandoned work. Dropping this
/// ticket or its drain future leaves the worker responsible for reaping it.
pub struct DrainTicket {
    flights: Rc<Flights>,
    fence: Fence,
}

impl AcquisitionWaiter<'_> {
    /// Independent deadline/cancellation. Lead is emitted once per election;
    /// retry elects only a still-live caller with remaining original credits.
    /// A rejected caller receives OriginRejected/OriginForbidden and is ineligible
    /// for this cohort. Peer Unauthorized failures are terminal instead.
    pub fn wait(&mut self) -> Operation<'_, AcquisitionEvent> {
        Box::pin(poll_fn(move |cx| {
            if let Err(error) = self.scope.cancellation.register(cx.waker()) {
                self.registration.cancel(error);
                return Poll::Ready(Err(error));
            }
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
                if let Some(result) = &entry.result {
                    return Poll::Ready(Ok(AcquisitionEvent::Complete(result.clone())));
                }
                if let Some(error) = entry.error {
                    return Poll::Ready(Ok(AcquisitionEvent::Failed(error)));
                }
                if entry.state == FlightState::RetryPending && !waiter.issued {
                    if Instant::now() >= self.budget.deadline || self.budget.attempts == 0 {
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
                        entry.outcome = Some(Outcome::Failed(Error::Unavailable));
                        settle(entry, wakes);
                        return Poll::Ready(Ok(AcquisitionEvent::Failed(Error::Unavailable)));
                    }
                    entry.fence.generation += 1;
                    entry.state = FlightState::Acquiring;
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
    pub fn wait(&mut self) -> Operation<'_, PageResult> {
        Box::pin(poll_fn(move |cx| {
            if let Err(error) = self.scope.cancellation.register(cx.waker()) {
                self.registration.cancel(error);
                return Poll::Ready(Err(error));
            }
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
                if let Some(error) = waiter.error.or(entry.error) {
                    return Poll::Ready(Err(error));
                }
                if let Some(result) = &entry.result {
                    return Poll::Ready(Ok(result.clone()));
                }
                store_waker(&mut waiter.waker, cx);
                Poll::Pending
            })
        }))
    }
    pub fn detach(mut self) -> Result<FlightState> {
        self.registration.detach()
    }
}

impl Flights {
    pub fn new(admission: Rc<Admission>) -> Self {
        let limits = FlightLimits {
            entries: admission.limits().flights.get(),
            waiters_per_flight: admission.limits().waiters_per_flight.get(),
            operations_per_flight: admission.limits().queue_entries.get(),
            generations_per_flight: 1024,
        };
        Self {
            admission,
            owner: Rc::new(()),
            limits,
            table: RefCell::new(Table::default()),
        }
    }

    pub fn with_limits(admission: Rc<Admission>, limits: FlightLimits) -> Result<Self> {
        if limits.entries == 0
            || limits.waiters_per_flight == 0
            || limits.operations_per_flight == 0
            || limits.generations_per_flight == 0
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            admission,
            owner: Rc::new(()),
            limits,
            table: RefCell::new(Table::default()),
        })
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
        validate_context(&page, context)?;
        scope.check()?;
        self.update(|table, wakes| {
            if table.stopping {
                return Err(Error::Cancelled);
            }
            if let Some(entry) = table.entries.get_mut(&page) {
                refresh(entry, wakes);
                if let Some(result) = &entry.result {
                    return Ok(JoinedFlight::Complete(result.clone()));
                }
                if let Some(error) = entry.error {
                    return Err(error);
                }
                if entry.waiters.len() >= self.limits.waiters_per_flight {
                    return Err(Error::Overloaded);
                }
            }
            if Instant::now() >= budget.deadline {
                return Err(Error::DeadlineExceeded);
            }
            if budget.attempts == 0 {
                return Err(Error::Unavailable);
            }
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
                        state: FlightState::RetryPending,
                        leader: None,
                        waiters: HashMap::new(),
                        operations: HashMap::new(),
                        outcome: None,
                        result: None,
                        error: None,
                        drain_waker: None,
                        _reservation: flight,
                    },
                );
                table.sweep.push_back(page.clone());
            }
            let entry = table.entries.get_mut(&page).unwrap();
            entry.waiters.insert(
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    acquisition: true,
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
            refresh(entry, wakes);
            if let Some(result) = &entry.result {
                return Ok(JoinedCopy::Complete(result.clone()));
            }
            if matches!(entry.state, FlightState::Failed | FlightState::Draining) {
                return Ok(JoinedCopy::Miss);
            }
            if entry.waiters.len() >= self.limits.waiters_per_flight {
                return Err(Error::Overloaded);
            }
            let reservation = self.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Waiter,
                1,
            )?;
            let id = next(&mut table.next_waiter)?;
            entry.waiters.insert(
                id,
                WaiterRecord {
                    scope: scope.clone(),
                    acquisition: false,
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
    pub fn publish(&self, mut leader: FlightLeader, page: PageResult) -> Result<FlightState> {
        self.update(|table, wakes| {
            let entry = self.leader_entry(table, &leader, wakes)?;
            page.validate_for(&entry.fence.page)?;
            entry.outcome = Some(Outcome::Published(page));
            entry.state = FlightState::Draining;
            leader.active = false;
            settle(entry, wakes);
            Ok(entry.state)
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
            entry.outcome = Some(match failure {
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
            entry.state = FlightState::Draining;
            leader.active = false;
            notify(entry, wakes);
            settle(entry, wakes);
            Ok(entry.state)
        })
    }

    /// Acquiring -> Draining. Revoke publication, request cancellation, and retain
    /// all accepted operations/reservations until their actual completions. Token
    /// drop, driver-future drop, and supplying-caller cancellation use this path.
    /// The abandoned supplier becomes ineligible (Cancelled); independent callers
    /// remain registered and can be elected only after the old generation drains.
    pub fn abandon(&self, mut leader: FlightLeader) -> Result<DrainTicket> {
        self.update(|table, wakes| {
            let entry = self.leader_entry(table, &leader, wakes)?;
            revoke(entry, Error::Cancelled, wakes);
            leader.active = false;
            Ok(DrainTicket {
                flights: leader.flights.clone(),
                fence: leader.fence.clone(),
            })
        })
    }

    /// Draining -> RetryPending (eligible live acquisition caller), Failed (only
    /// copy waiters remain), or Removed (last waiter gone). Retain the page slot
    /// while draining: a new join cannot start overlapping acquisition. The worker
    /// drives completion accounting even if nobody polls this observation future.
    pub fn finish_draining(&self, ticket: DrainTicket) -> Operation<'_, FlightState> {
        Box::pin(poll_fn(move |cx| {
            self.update(|table, wakes| {
                if !Rc::ptr_eq(&self.owner, &ticket.flights.owner) {
                    return Poll::Ready(Err(Error::StaleFlight));
                }
                let Some(entry) = table.entries.get_mut(&ticket.fence.page) else {
                    return Poll::Ready(Ok(FlightState::Removed));
                };
                if let Err(error) = ticket.fence.validate(&entry.fence) {
                    return Poll::Ready(Err(error));
                }
                refresh(entry, wakes);
                if entry.state != FlightState::Draining {
                    return Poll::Ready(Ok(entry.state));
                }
                store_waker(&mut entry.drain_waker, cx);
                Poll::Pending
            })
        }))
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
            .flat_map(|entry| entry.waiters.values())
            .filter(|waiter| waiter.error.is_none())
            .map(|waiter| waiter.scope.deadline.0.min(waiter.budget_deadline))
            .min()
    }

    fn sweep_budgeted(&self, work_budget: usize) -> Result<()> {
        self.update(|table, wakes| {
            for _ in 0..work_budget.min(table.sweep.len()) {
                let page = table.sweep.pop_front().unwrap();
                if let Some(entry) = table.entries.get_mut(&page) {
                    refresh(entry, wakes);
                    if entry.waiters.is_empty() && entry.operations.is_empty() {
                        if let Some(waker) = entry.drain_waker.take() {
                            wakes.push(waker);
                        }
                        table.entries.remove(&page);
                    } else {
                        table.sweep.push_back(page);
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
                entry.outcome = Some(Outcome::Failed(Error::Cancelled));
                entry.state = FlightState::Draining;
                notify(entry, wakes);
                entry.waiters.clear();
                settle(entry, wakes);
            }
        });
        Box::pin(poll_fn(move |cx| {
            scope.cancellation.register(cx.waker())?;
            self.poll_with_context(cx, self.limits.entries)?;
            let mut table = self.table.borrow_mut();
            if table.entries.is_empty() {
                return Poll::Ready(Ok(()));
            }
            scope.check()?;
            store_waker(&mut table.drain_waker, cx);
            Poll::Pending
        }))
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
        self.sweep_budgeted(self.limits.entries)
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
    if let Some(waker) = entry.drain_waker.take() {
        wakes.push(waker);
    }
}
fn eligible(waiter: &WaiterRecord) -> bool {
    waiter.acquisition && !waiter.issued && waiter.error.is_none()
}
fn settle(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    if !entry.operations.is_empty() {
        return;
    }
    if let Some(outcome) = entry.outcome.take() {
        entry.leader = None;
        match outcome {
            Outcome::Published(result) => {
                entry.result = Some(result);
                entry.state = FlightState::Complete;
            }
            Outcome::Failed(error) => {
                entry.error = Some(error);
                entry.state = FlightState::Failed;
            }
            Outcome::Retry => {
                if entry.waiters.values().any(eligible) {
                    entry.state = FlightState::RetryPending;
                } else {
                    entry.error = Some(Error::Unavailable);
                    entry.state = FlightState::Failed;
                }
            }
        }
        notify(entry, wakes);
    }
}
fn revoke(entry: &mut Entry, error: Error, wakes: &mut Vec<Waker>) {
    if let Some(waiter) = entry.leader.and_then(|id| entry.waiters.get_mut(&id)) {
        waiter.error = Some(error);
    }
    entry.state = FlightState::Draining;
    entry.outcome = Some(Outcome::Retry);
    notify(entry, wakes);
    settle(entry, wakes);
}
fn refresh(entry: &mut Entry, wakes: &mut Vec<Waker>) {
    for waiter in entry.waiters.values_mut() {
        if waiter.error.is_none() {
            waiter.error = waiter.scope.check().err().or_else(|| {
                (Instant::now() >= waiter.budget_deadline).then_some(Error::DeadlineExceeded)
            });
            if waiter.error.is_some() {
                if let Some(waker) = waiter.waker.take() {
                    wakes.push(waker);
                }
            }
        }
    }
    if entry.state == FlightState::Acquiring
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
    if entry.state == FlightState::RetryPending && !entry.waiters.values().any(eligible) {
        entry.outcome = Some(Outcome::Failed(Error::Unavailable));
        settle(entry, wakes);
    }
}
fn validate_leader(entry: &Entry, leader: &FlightLeader) -> Result<()> {
    leader.fence.validate(&entry.fence)?;
    if !leader.active
        || entry.state != FlightState::Acquiring
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
            entry.waiters.remove(&self.id);
            refresh(entry, wakes);
            if entry.waiters.is_empty() && entry.operations.is_empty() {
                if let Some(waker) = entry.drain_waker.take() {
                    wakes.push(waker);
                }
                table.entries.remove(&self.fence.page);
                table.sweep.retain(|page| page != &self.fence.page);
                if let Some(waker) = table.drain_waker.take() {
                    wakes.push(waker);
                }
                Ok(FlightState::Removed)
            } else {
                Ok(entry.state)
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
mod tests {
    use super::*;
    use crate::model::identity::{
        CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag,
    };
    use std::time::Duration;

    fn flights(limits: FlightLimits) -> Rc<Flights> {
        Rc::new(
            Flights::with_limits(
                Rc::new(Admission::new(
                    crate::test_support::cluster::config(false).limits,
                )),
                limits,
            )
            .unwrap(),
        )
    }
    fn scope() -> RequestScope {
        RequestScope::new(
            crate::model::identity::RequestId([0; 16]),
            Instant::now() + Duration::from_secs(60),
        )
        .unwrap()
    }
    fn origin() -> OriginContext {
        OriginContext {
            object: fence().page.version.object,
            metadata: None,
            authorization: None,
        }
    }
    fn budget() -> AcquisitionBudget {
        AcquisitionBudget::new(Instant::now() + Duration::from_secs(60), 3, 8)
    }
    fn join<'a>(
        flights: &Rc<Flights>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> AcquisitionWaiter<'a> {
        let membership = std::sync::Arc::new(
            crate::topology::membership::Membership::validate(
                crate::model::identity::MembershipVersion(1),
                vec![],
            )
            .unwrap(),
        );
        match flights
            .join(fence().page, membership, context, scope, budget)
            .unwrap()
        {
            JoinedFlight::Waiter(waiter) => waiter,
            JoinedFlight::Complete(_) => panic!("unexpected completed entry"),
        }
    }
    fn poll<T>(mut future: Operation<'_, T>) -> Poll<Result<T>> {
        future
            .as_mut()
            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
    }
    fn lead(waiter: &mut AcquisitionWaiter<'_>) -> FlightLeader {
        match poll(waiter.wait()) {
            Poll::Ready(Ok(AcquisitionEvent::Lead(leader))) => leader,
            _ => panic!("expected election"),
        }
    }
    fn failed(waiter: &mut AcquisitionWaiter<'_>, expected: Error) {
        assert!(
            matches!(poll(waiter.wait()), Poll::Ready(Ok(AcquisitionEvent::Failed(error))) if error == expected)
        );
    }

    fn page_result(flights: &Flights) -> PageResult {
        result(flights, fence().page)
    }

    #[test]
    fn complete_publication_waits_for_operations_and_shares_exact_bundle() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let mut copy = match flights.join_copy(&fence().page, &scope).unwrap() {
            JoinedCopy::Waiter(waiter) => waiter,
            _ => panic!("expected waiter"),
        };
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let result = page_result(&flights);
        assert_eq!(
            flights.publish(leader, result.clone()),
            Ok(FlightState::Draining)
        );
        assert!(poll(b.wait()).is_pending());
        assert!(poll(copy.wait()).is_pending());
        operation.complete().unwrap();
        for waiter in [&mut a, &mut b] {
            match poll(waiter.wait()) {
                Poll::Ready(Ok(AcquisitionEvent::Complete(shared))) => {
                    assert!(std::ptr::eq(
                        shared.plaintext.bytes().as_ptr(),
                        result.plaintext.bytes().as_ptr()
                    ));
                    assert_eq!(shared.ciphertext.bytes(), result.ciphertext.bytes());
                }
                _ => panic!("expected complete bundle"),
            }
        }
        assert!(matches!(poll(copy.wait()), Poll::Ready(Ok(_))));
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Ok(JoinedCopy::Complete(_))
        ));
        assert!(matches!(
            flights.join(
                fence().page,
                a.membership.clone(),
                &context,
                &scope,
                &mut budget()
            ),
            Ok(JoinedFlight::Complete(_))
        ));
        drop(copy);
        drop(a);
        drop(b);
        assert!(flights.table.borrow().entries.is_empty());
    }

    #[test]
    fn malformed_publication_abandons_without_sharing_or_reusing_generation() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let leader = lead(&mut a);
        let generation = leader.fence.generation;
        let mut result = page_result(&flights);
        result.metadata.length = 4;
        assert_eq!(flights.publish(leader, result), Err(Error::CorruptRecord));
        failed(&mut a, Error::Cancelled);
        let retry = lead(&mut b);
        assert!(retry.fence.generation > generation);
    }

    #[test]
    fn forbidden_is_caller_only_and_peer_auth_is_terminal() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let leader = lead(&mut a);
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::OriginForbidden),
            Ok(FlightState::RetryPending)
        );
        failed(&mut a, Error::OriginForbidden);
        let retry = lead(&mut b);
        assert_eq!(
            flights.fail(retry, AcquisitionFailure::Terminal(Error::Unauthorized)),
            Ok(FlightState::Failed)
        );
        failed(&mut b, Error::Unauthorized);
    }

    #[test]
    fn worker_poll_drives_owned_completion_after_waiter_disappears() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut a_budget = budget();
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let (send, receive) = futures::channel::oneshot::channel::<()>();
        let worker = flights.clone();
        super::super::drivers::spawn(Box::pin(async move {
            receive.await.map_err(|_| Error::Io)?;
            operation.complete()?;
            assert_eq!(
                worker.fail(leader, AcquisitionFailure::Terminal(Error::Io)),
                Err(Error::StaleFlight)
            );
            Ok(())
        }))
        .unwrap();
        drop(a);
        flights.poll_budgeted(1).unwrap();
        assert_eq!(flights.table.borrow().entries.len(), 1);
        send.send(()).unwrap();
        flights.poll_budgeted(1).unwrap();
        assert!(flights.table.borrow().entries.is_empty());
        assert_eq!(super::super::drivers::pending(), 0);
    }

    #[test]
    fn deadline_sweep_wakes_waiter_without_io_and_does_not_expire_other_callers() {
        use std::sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        };
        struct Count(AtomicUsize);
        impl std::task::Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let flights = flights(FlightLimits::default());
        let (context, scope_a, scope_b) = (origin(), scope(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope_a, &mut a_budget);
        let mut b = join(&flights, &context, &scope_b, &mut b_budget);
        let leader = lead(&mut a);
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        assert!(
            b.wait()
                .as_mut()
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        assert!(flights.next_deadline().is_some());
        // Advance just this registration's effective deadline without sleeping.
        flights
            .table
            .borrow_mut()
            .entries
            .get_mut(&fence().page)
            .unwrap()
            .waiters
            .get_mut(&b.registration.id)
            .unwrap()
            .budget_deadline = Instant::now();
        flights.poll_budgeted(1).unwrap();
        assert!(count.0.load(Ordering::Relaxed) > 0);
        failed(&mut b, Error::DeadlineExceeded);
        assert!(a.acquisition(&leader).is_ok());
    }

    #[test]
    fn link_refund_is_bounded_and_transfers_do_not_duplicate_refund_rights() {
        let mut original = budget();
        assert_eq!(original.refund_links(1), Err(Error::InvalidRequest));
        original.charge_links(6).unwrap();
        let mut child = original.partition(1, 1).unwrap();
        assert_eq!(child.refund_links(1), Err(Error::InvalidRequest));
        let mut moved = original.transfer();
        assert_eq!(original.refund_links(1), Err(Error::InvalidRequest));
        moved.refund_links(2).unwrap();
        assert_eq!(moved.remaining_links(), 3);
        assert_eq!(moved.refund_links(5), Err(Error::InvalidRequest));
        assert_eq!(moved.remaining_links(), 3);
    }

    #[test]
    fn partition_transfer_and_peer_debits_conserve_original_credits() {
        let mut original = budget();
        let deadline = original.deadline();
        assert!(matches!(
            original.partition(2, 9),
            Err(Error::HopBudgetExhausted)
        ));
        assert_eq!(original.remaining_attempts(), 3);
        let mut child = original.partition(2, 5).unwrap();
        assert_eq!(
            (original.remaining_attempts(), original.remaining_links()),
            (1, 3)
        );
        assert_eq!(
            child.begin_peer_attempt(Instant::now(), deadline, 6),
            Err(Error::HopBudgetExhausted)
        );
        assert_eq!(child.remaining_attempts(), 2);
        assert_eq!(
            child.begin_peer_attempt(Instant::now(), deadline, 4),
            Ok(deadline)
        );
        let transferred = child.transfer();
        assert_eq!(
            (child.remaining_attempts(), child.remaining_links()),
            (0, 0)
        );
        assert_eq!(
            (
                transferred.remaining_attempts(),
                transferred.remaining_links()
            ),
            (1, 1)
        );
        assert_eq!(transferred.deadline(), deadline);
    }

    #[test]
    fn rejected_credentials_retry_only_after_completion_with_independent_budget() {
        let flights = flights(FlightLimits::default());
        let (context, scope_a, scope_b) = (origin(), scope(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope_a, &mut a_budget);
        let mut b = join(&flights, &context, &scope_b, &mut b_budget);
        let leader = lead(&mut a);
        let old_fence = leader.fence.clone();
        a.acquisition(&leader)
            .unwrap()
            .budget
            .begin_peer_attempt(Instant::now(), scope_a.deadline.0, 3)
            .unwrap();
        assert!(poll(b.wait()).is_pending());
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let id = operation.id;
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::OriginRejected),
            Ok(FlightState::Draining)
        );
        failed(&mut a, Error::OriginRejected);
        assert!(poll(b.wait()).is_pending());
        assert!(operation.cancellation_requested());
        operation.complete().unwrap();
        let retry = lead(&mut b);
        assert!(retry.fence.generation > old_fence.generation);
        assert_eq!(
            b.acquisition(&retry).unwrap().budget.remaining_attempts(),
            3
        );
        assert_eq!(
            flights.complete_operation(&old_fence, id),
            Err(Error::StaleFlight)
        );
        let stale = FlightLeader {
            flights: flights.clone(),
            fence: old_fence,
            caller: a.registration.id,
            active: true,
        };
        assert_eq!(
            flights.fail(stale, AcquisitionFailure::Terminal(Error::Io)),
            Err(Error::StaleFlight)
        );
        assert!(b.acquisition(&retry).is_ok());
        flights
            .fail(retry, AcquisitionFailure::Terminal(Error::Io))
            .unwrap();
        failed(&mut b, Error::Io);
        drop(a);
        assert_eq!(a_budget.remaining_attempts(), 2);
        assert_eq!(a_budget.remaining_links(), 5);
    }

    #[test]
    fn drop_leader_retains_resources_until_actual_completion_without_ticket() {
        struct Resource(Rc<std::cell::Cell<bool>>);
        impl Drop for Resource {
            fn drop(&mut self) {
                self.0.set(true);
            }
        }
        let flights = flights(FlightLimits::default());
        let (context, scope_a, scope_b) = (origin(), scope(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope_a, &mut a_budget);
        let mut b = join(&flights, &context, &scope_b, &mut b_budget);
        let leader = lead(&mut a);
        let released = Rc::new(std::cell::Cell::new(false));
        let operation = flights
            .retain_operation(&leader, Resource(released.clone()))
            .unwrap();
        drop(leader);
        drop(a);
        flights.poll_budgeted(1).unwrap();
        assert!(!released.get());
        assert_eq!(
            flights.table.borrow().entries[&fence().page].state,
            FlightState::Draining
        );
        assert!(poll(b.wait()).is_pending());
        operation.complete().unwrap();
        assert!(released.get());
        drop(lead(&mut b));
    }

    #[test]
    fn cancellation_reaps_independently_and_copy_never_elects() {
        let flights = flights(FlightLimits::default());
        let (context, scope_a, copy_scope) = (origin(), scope(), scope());
        let mut a_budget = budget();
        assert!(matches!(
            flights.join_copy(&fence().page, &copy_scope),
            Ok(JoinedCopy::Miss)
        ));
        let mut a = join(&flights, &context, &scope_a, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let mut copy = match flights.join_copy(&fence().page, &copy_scope).unwrap() {
            JoinedCopy::Waiter(waiter) => waiter,
            _ => panic!("expected copy waiter"),
        };
        assert!(poll(copy.wait()).is_pending());
        scope_a.cancel().unwrap();
        flights.poll_budgeted(1).unwrap();
        assert!(operation.cancellation_requested());
        failed(&mut a, Error::Cancelled);
        assert!(poll(copy.wait()).is_pending());
        operation.complete().unwrap();
        assert!(matches!(
            poll(copy.wait()),
            Poll::Ready(Err(Error::Unavailable))
        ));
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::OriginRejected),
            Err(Error::StaleFlight)
        );
    }

    #[test]
    fn table_and_incarnation_fences_prevent_replacement_corruption() {
        let first = flights(FlightLimits::default());
        let other = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&first, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let old = leader.fence.clone();
        let caller = leader.caller;
        assert_eq!(
            other.fail(leader, AcquisitionFailure::OriginRejected),
            Err(Error::StaleFlight)
        );
        a.detach().unwrap();
        let mut b = join(&first, &context, &scope, &mut b_budget);
        let leader = lead(&mut b);
        assert_ne!(old.incarnation, leader.fence.incarnation);
        let stale = FlightLeader {
            flights: first.clone(),
            fence: old,
            caller,
            active: true,
        };
        assert_eq!(
            first.fail(stale, AcquisitionFailure::Terminal(Error::Io)),
            Err(Error::StaleFlight)
        );
        assert!(b.acquisition(&leader).is_ok());
    }

    #[test]
    fn entry_waiter_operation_and_generation_caps_are_enforced() {
        let flights = flights(FlightLimits {
            entries: 1,
            waiters_per_flight: 2,
            operations_per_flight: 1,
            generations_per_flight: 1,
        });
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget, mut extra_budget) = (budget(), budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Err(Error::Overloaded)
        ));
        let mut another_page = fence().page;
        another_page.number = PageNumber(1);
        assert!(matches!(
            flights.join(
                another_page,
                a.membership.clone(),
                &context,
                &scope,
                &mut extra_budget
            ),
            Err(Error::Overloaded)
        ));
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        assert!(matches!(
            flights.retain_operation(&leader, ()),
            Err(Error::Overloaded)
        ));
        flights
            .fail(leader, AcquisitionFailure::OriginRejected)
            .unwrap();
        operation.complete().unwrap();
        failed(&mut b, Error::Unavailable);
        drop(a);
        drop(b);
        assert!(flights.table.borrow().entries.is_empty());
        assert_eq!(flights.admission.used(ResourceClass::Flight), 0);
        assert_eq!(flights.admission.used(ResourceClass::Waiter), 0);
        assert_eq!(flights.admission.used(ResourceClass::ControlProgress), 0);
    }

    #[test]
    fn abandoned_drain_observer_and_expired_shutdown_cannot_release_work() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut a_budget = budget();
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let ticket = flights.abandon(leader).unwrap();
        assert!(poll(flights.finish_draining(ticket)).is_pending());
        let expired =
            RequestScope::new(crate::model::identity::RequestId([1; 16]), Instant::now()).unwrap();
        assert!(matches!(
            poll(flights.drain(&expired)),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        assert_eq!(flights.table.borrow().entries.len(), 1);
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Err(Error::Cancelled)
        ));
        operation.complete().unwrap();
        assert!(matches!(poll(flights.drain(&scope)), Poll::Ready(Ok(()))));
    }

    #[test]
    fn dropped_completion_token_does_not_claim_completion() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut a_budget = budget();
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let (fence, id) = (operation.fence.clone(), operation.id);
        drop(operation);
        drop(leader);
        drop(a);
        flights.poll_budgeted(100).unwrap();
        assert_eq!(flights.table.borrow().entries.len(), 1);
        // Simulate the worker reaping the actual completion, not request drop.
        flights.complete_operation(&fence, id).unwrap();
        assert!(flights.table.borrow().entries.is_empty());
    }

    fn result(flights: &Flights, page: PageId) -> PageResult {
        use crate::{
            memory::pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
            model::{
                envelope::{KeyId, Nonce, PageEnvelope},
                metadata::{ExpiresAt, ObjectMetadata},
            },
        };
        use std::sync::Arc;
        PageResult {
            metadata: ObjectMetadata {
                version: page.version.clone(),
                length: 3,
                expires_at: ExpiresAt(std::time::UNIX_EPOCH),
            },
            plaintext: VerifiedPage {
                inner: Arc::new(VerifiedBytes {
                    page: page.clone(),
                    bytes: vec![1, 2, 3],
                    reservation: flights
                        .admission
                        .reserve(None, ResourceClass::Plaintext, 3)
                        .unwrap(),
                }),
            },
            ciphertext: CiphertextPage {
                inner: Arc::new(CiphertextBytes {
                    envelope: PageEnvelope {
                        page,
                        key_id: KeyId([0; 16]),
                        nonce: Nonce([0; 24]),
                        plaintext_length: 3,
                        ciphertext_length: 19,
                    },
                    bytes: vec![0; 19],
                    reservation: flights
                        .admission
                        .reserve(None, ResourceClass::Ciphertext, 19)
                        .unwrap(),
                }),
            },
        }
    }

    #[test]
    fn validated_publication_waits_for_fences_and_shares_original_bundle() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let mut copy = match flights.join_copy(&fence().page, &scope).unwrap() {
            JoinedCopy::Waiter(waiter) => waiter,
            _ => panic!("expected copy"),
        };
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let page = result(&flights, fence().page);
        assert_eq!(
            flights.publish(leader, page.clone()),
            Ok(FlightState::Draining)
        );
        assert!(poll(b.wait()).is_pending());
        assert!(poll(copy.wait()).is_pending());
        operation.complete().unwrap();
        let shared = match poll(b.wait()) {
            Poll::Ready(Ok(AcquisitionEvent::Complete(page))) => page,
            _ => panic!("expected complete"),
        };
        assert!(std::sync::Arc::ptr_eq(
            &shared.plaintext.inner,
            &page.plaintext.inner
        ));
        assert!(std::sync::Arc::ptr_eq(
            &shared.ciphertext.inner,
            &page.ciphertext.inner
        ));
        assert!(matches!(poll(copy.wait()), Poll::Ready(Ok(_))));
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Ok(JoinedCopy::Complete(_))
        ));
        drop(a);
        drop(b);
        drop(copy);
        assert!(flights.table.borrow().entries.is_empty());
        // Readers own their bundles independently of the flight entry.
        assert_eq!(shared.plaintext.bytes(), &[1, 2, 3]);
    }

    #[test]
    fn invalid_publication_abandons_supplier_and_forbidden_retries_independently() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget, mut c_budget) = (budget(), budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let mut c = join(&flights, &context, &scope, &mut c_budget);
        let leader = lead(&mut a);
        let mut bad = result(&flights, fence().page);
        bad.metadata.length = 4;
        assert_eq!(flights.publish(leader, bad), Err(Error::CorruptRecord));
        failed(&mut a, Error::Cancelled);
        let retry = lead(&mut b);
        assert_eq!(
            flights.fail(retry, AcquisitionFailure::OriginForbidden),
            Ok(FlightState::RetryPending)
        );
        failed(&mut b, Error::OriginForbidden);
        let retry = lead(&mut c);
        flights
            .fail(retry, AcquisitionFailure::Terminal(Error::Unauthorized))
            .unwrap();
        failed(&mut c, Error::Unauthorized);
    }

    #[test]
    fn driver_queue_progresses_after_all_request_waiters_are_dropped() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut a_budget = budget();
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let (complete, fence) = futures::channel::oneshot::channel::<()>();
        super::super::drivers::spawn(Box::pin(async move {
            fence.await.map_err(|_| Error::Io)?;
            operation.complete()?;
            drop(leader);
            Ok(())
        }))
        .unwrap();
        flights.poll_budgeted(4).unwrap();
        drop(a);
        assert_eq!(flights.table.borrow().entries.len(), 1);
        complete.send(()).unwrap();
        flights.poll_budgeted(4).unwrap();
        assert!(flights.table.borrow().entries.is_empty());
        assert_eq!(super::super::drivers::pending(), 0);
    }

    #[test]
    fn deadline_expiration_is_independent_and_wakes_parked_callers() {
        use std::sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        };
        struct Count(AtomicUsize);
        impl std::task::Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let leader = lead(&mut a);
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        assert!(
            b.wait()
                .as_mut()
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        flights
            .table
            .borrow_mut()
            .entries
            .get_mut(&fence().page)
            .unwrap()
            .waiters
            .get_mut(&b.registration.id)
            .unwrap()
            .budget_deadline = Instant::now();
        flights.poll_budgeted(1).unwrap();
        assert!(count.0.load(Ordering::Relaxed) > 0);
        failed(&mut b, Error::DeadlineExceeded);
        assert!(a.acquisition(&leader).is_ok());
    }

    #[test]
    fn route_refunds_cannot_duplicate_partitioned_or_transferred_credits() {
        let mut original = budget();
        original.charge_links(6).unwrap();
        let mut child = original.partition(1, 2).unwrap();
        assert_eq!(child.refund_links(1), Err(Error::InvalidRequest));
        let mut moved = original.transfer();
        assert_eq!(original.refund_links(1), Err(Error::InvalidRequest));
        assert_eq!(moved.refund_links(7), Err(Error::InvalidRequest));
        moved.refund_links(6).unwrap();
        assert_eq!(moved.refund_links(1), Err(Error::InvalidRequest));
        assert_eq!(moved.remaining_links() + child.remaining_links(), 8);
    }

    fn shared_result(flights: &Flights) -> PageResult {
        result(flights, fence().page)
    }

    #[test]
    fn publication_waits_for_completion_and_shares_whole_page() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut budget = budget();
        let mut a = join(&flights, &context, &scope, &mut budget);
        let leader = lead(&mut a);
        let mut copy = match flights.join_copy(&fence().page, &scope).unwrap() {
            JoinedCopy::Waiter(waiter) => waiter,
            _ => panic!("expected copy"),
        };
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let page = shared_result(&flights);
        assert_eq!(
            flights.publish(leader, page.clone()),
            Ok(FlightState::Draining)
        );
        assert!(poll(copy.wait()).is_pending());
        operation.complete().unwrap();
        let shared = match poll(copy.wait()) {
            Poll::Ready(Ok(result)) => result,
            _ => panic!("expected result"),
        };
        assert!(std::sync::Arc::ptr_eq(
            &shared.plaintext.inner,
            &page.plaintext.inner
        ));
        assert!(std::sync::Arc::ptr_eq(
            &shared.ciphertext.inner,
            &page.ciphertext.inner
        ));
        assert!(matches!(
            poll(a.wait()),
            Poll::Ready(Ok(AcquisitionEvent::Complete(_)))
        ));
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Ok(JoinedCopy::Complete(_))
        ));
        drop(copy);
        drop(a);
        assert!(matches!(
            flights.join_copy(&fence().page, &scope),
            Ok(JoinedCopy::Miss)
        ));
    }

    #[test]
    fn forbidden_retries_but_peer_unauthorized_is_terminal() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let (mut a_budget, mut b_budget) = (budget(), budget());
        let mut a = join(&flights, &context, &scope, &mut a_budget);
        let mut b = join(&flights, &context, &scope, &mut b_budget);
        let leader = lead(&mut a);
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::OriginForbidden),
            Ok(FlightState::RetryPending)
        );
        failed(&mut a, Error::OriginForbidden);
        let leader = lead(&mut b);
        assert_eq!(
            flights.fail(leader, AcquisitionFailure::Terminal(Error::Unauthorized)),
            Ok(FlightState::Failed)
        );
        failed(&mut b, Error::Unauthorized);
        failed(&mut a, Error::OriginForbidden);
    }

    #[test]
    fn worker_poll_drives_parent_owned_queue_after_caller_drop() {
        let flights = flights(FlightLimits::default());
        let (context, scope) = (origin(), scope());
        let mut budget = budget();
        let mut a = join(&flights, &context, &scope, &mut budget);
        let leader = lead(&mut a);
        let operation = flights.retain_operation(&leader, ()).unwrap();
        let (send, recv) = futures::channel::oneshot::channel::<()>();
        super::super::drivers::spawn(Box::pin(async move {
            recv.await.map_err(|_| Error::Io)?;
            operation.complete()?;
            drop(leader);
            Ok(())
        }))
        .unwrap();
        drop(a);
        flights.poll_budgeted(1).unwrap();
        assert_eq!(flights.table.borrow().entries.len(), 1);
        send.send(()).unwrap();
        assert!(matches!(poll(flights.drain(&scope)), Poll::Ready(Ok(()))));
        assert_eq!(super::super::drivers::pending(), 0);
    }

    #[test]
    fn link_refunds_and_transfers_cannot_duplicate_debits() {
        let mut budget = budget();
        assert_eq!(budget.refund_links(1), Err(Error::InvalidRequest));
        budget.charge_links(5).unwrap();
        let mut child = budget.partition(1, 2).unwrap();
        assert_eq!(child.refund_links(1), Err(Error::InvalidRequest));
        let mut moved = budget.transfer();
        assert_eq!(budget.refund_links(1), Err(Error::InvalidRequest));
        moved.refund_links(3).unwrap();
        assert_eq!(moved.refund_links(3), Err(Error::InvalidRequest));
        assert_eq!(moved.remaining_links() + child.remaining_links(), 6);
    }

    fn fence() -> Fence {
        Fence {
            owner: Rc::new(()),
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId("cache".into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            incarnation: 1,
            generation: 1,
        }
    }

    #[test]
    fn fences_reject_other_tables_pages_recreated_entries_and_late_retries() {
        let current = fence();
        assert_eq!(current.validate(&current.clone()), Ok(()));
        let mut stale = current.clone();
        stale.owner = Rc::new(());
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.page.number = PageNumber(1);
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.incarnation += 1;
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
        let mut stale = current.clone();
        stale.generation += 1;
        assert_eq!(stale.validate(&current), Err(Error::StaleFlight));
    }

    #[test]
    fn request_context_must_match_both_cache_and_key_before_join() {
        let page = fence().page;
        let mut context = OriginContext {
            object: page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        assert_eq!(validate_context(&page, &context), Ok(()));
        context.object.key = CacheKey([1; 32]);
        assert_eq!(
            validate_context(&page, &context),
            Err(Error::InvalidRequest)
        );
        context.object = page.version.object.clone();
        context.object.cache = CacheId("other".into());
        assert_eq!(
            validate_context(&page, &context),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn retry_debits_original_attempts_and_links_without_extending_deadline() {
        let now = Instant::now();
        let original = now + Duration::from_secs(5);
        let later = original + Duration::from_secs(10);
        let mut budget = AcquisitionBudget::new(original, 2, 4);
        assert_eq!(budget.begin_attempt(now, later), Ok(original));
        assert_eq!(budget.charge_links(3), Ok(()));
        assert_eq!(budget.charge_links(2), Err(Error::HopBudgetExhausted));
        assert_eq!(budget.charge_links(1), Ok(()));
        assert_eq!(budget.begin_attempt(now, later), Ok(original));
        assert_eq!(budget.begin_attempt(now, later), Err(Error::Unavailable));
        assert_eq!(budget.charge_links(1), Err(Error::HopBudgetExhausted));
    }

    #[test]
    fn remaining_callers_keep_independent_budgets_and_inclusive_deadlines() {
        let now = Instant::now();
        let deadline = now + Duration::from_secs(5);
        let mut first = AcquisitionBudget::new(deadline, 1, 0);
        let mut remaining = AcquisitionBudget::new(deadline, 1, 4);
        assert_eq!(first.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(first.begin_attempt(now, deadline), Err(Error::Unavailable));
        assert_eq!(
            remaining.begin_attempt(now, now),
            Err(Error::DeadlineExceeded)
        );
        // Expiry checks and failed charges do not consume unrelated credits.
        assert_eq!(remaining.begin_attempt(now, deadline), Ok(deadline));
        assert_eq!(remaining.charge_links(4), Ok(()));
        assert_eq!(
            first.begin_attempt(deadline, deadline),
            Err(Error::DeadlineExceeded)
        );
    }

    // Compile the ownership/data path rather than asserting Unimplemented errors.
    async fn acquisition_api(
        flights: Rc<Flights>,
        page: PageId,
        membership: MembershipLease,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        result: PageResult,
    ) -> Result<PageResult> {
        match flights.join(page.clone(), membership, context, scope, budget)? {
            JoinedFlight::Complete(result) => Ok(result),
            JoinedFlight::Waiter(mut caller) => match caller.wait().await? {
                AcquisitionEvent::Complete(result) => Ok(result),
                AcquisitionEvent::Failed(error) => Err(error),
                AcquisitionEvent::Lead(leader) => {
                    let acquisition = caller.acquisition(&leader)?;
                    let _: &OriginContext = acquisition.origin;
                    let _: &MembershipLease = acquisition.membership;
                    acquisition
                        .budget
                        .begin_attempt(Instant::now(), acquisition.scope.deadline.0)?;
                    result.validate_for(&page)?;
                    flights.publish(leader, result.clone())?;
                    caller.detach()?;
                    Ok(result)
                }
            },
        }
    }

    async fn failure_and_copy_api(
        flights: Rc<Flights>,
        leader: FlightLeader,
        abandoned: FlightLeader,
        page: PageId,
        scope: &RequestScope,
    ) -> Result<Option<PageResult>> {
        flights.fail(leader, AcquisitionFailure::OriginRejected)?;
        let ticket = flights.abandon(abandoned)?;
        flights.finish_draining(ticket).await?;
        match flights.join_copy(&page, scope)? {
            JoinedCopy::Miss => Ok(None),
            JoinedCopy::Complete(result) => Ok(Some(result)),
            JoinedCopy::Waiter(mut waiter) => {
                let result = waiter.wait().await?;
                waiter.detach()?;
                Ok(Some(result))
            }
        }
    }
}
