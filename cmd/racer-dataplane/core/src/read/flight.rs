//! Sans-IO page singleflight rules.
//!
//! The flight transitions run in `flow_control::coalesce::flight::state::State`.
//! The step functions here hold the business decisions taken on top of it (join
//! gates, the wait/elect decision, publication, and failure settlement), and
//! are generic over the wake target `T`: production `Flights`
//! (src/read/flight.rs) calls them with `Waker`s inside its table borrow, and
//! the simulator drives them through [`PageFlights`] with plain [`Token`]s.

use crate::read::{Error, FailureKind, Result};
use flow_control::coalesce::flight::Identity;
use flow_control::coalesce::flight::state::{Outcome, Phase, Published, State, WaiterPolicy};
use racer_object_wire::model::PageId;
use std::cell::Cell;
use std::collections::HashMap;
use std::hash::BuildHasher;
use std::rc::Rc;
use std::time::Instant;

/// Opaque wake target chosen by the driver (for example, a simulated task id).
pub type Token = u64;

/// Table bounds. All dimensions must be nonzero. [`PageFlights`] does not
/// bound retained operations; production caps them at `operations_per_flight`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
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

/// Reject a join when the new waiter's budget cannot act. A caller with no
/// attempts may still join a flight that holds a reusable partial result.
pub fn join_budget_gate(
    now: Instant,
    deadline: Instant,
    attempts: u32,
    partial: bool,
) -> Result<()> {
    if now >= deadline {
        return Err(Error::DeadlineExceeded);
    }
    if attempts == 0 && !partial {
        return Err(Error::Unavailable);
    }
    Ok(())
}

/// Table entry cap for a new flight.
pub fn entry_cap_gate(entries: usize, limits: &FlightLimits) -> Result<()> {
    if entries >= limits.entries {
        Err(Error::Overloaded)
    } else {
        Ok(())
    }
}

/// A settled result a caller can use without waiting.
#[derive(Debug, Eq, PartialEq)]
pub enum Settled<C, P> {
    /// The page is complete.
    Complete(C),
    /// The retained partial (ciphertext) result.
    Partial(P),
}

/// Why a join on an existing flight was refused.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum JoinReject<E = Error> {
    /// The flight settled to this failure.
    Failed(E),
    /// The flight holds `waiters_per_flight` waiters.
    WaiterCap,
}

impl<E: From<Error>> JoinReject<E> {
    pub fn error(self) -> E {
        match self {
            JoinReject::Failed(error) => error,
            JoinReject::WaiterCap => Error::Overloaded.into(),
        }
    }
}

impl From<JoinReject> for Error {
    fn from(reject: JoinReject) -> Self {
        reject.error()
    }
}

/// Gates of an acquisition join on an existing, refreshed flight: a complete
/// result, then a partial result for a ciphertext caller, then a settled
/// failure, then the waiter cap. `None` means register a waiter (after
/// [`join_budget_gate`] and admission).
pub fn join_existing<
    C: Clone,
    P: Clone,
    O,
    W: WaiterPolicy<Error = E>,
    T,
    E: Copy + PartialEq + From<Error>,
>(
    state: &State<C, P, O, W, T>,
    plaintext: bool,
    limits: &FlightLimits,
) -> std::result::Result<Option<Settled<C, P>>, JoinReject<E>> {
    if let Phase::Complete(result) = &state.phase {
        return Ok(Some(Settled::Complete(result.clone())));
    }
    if !plaintext && let Some(partial) = &state.partial {
        return Ok(Some(Settled::Partial(partial.clone())));
    }
    if let Phase::Failed(error) = state.phase {
        return Err(JoinReject::Failed(error));
    }
    if state.waiters.len() >= limits.waiters_per_flight {
        return Err(JoinReject::WaiterCap);
    }
    Ok(None)
}

/// Result of the copy-only join gates.
#[derive(Debug, Eq, PartialEq)]
pub enum CopyJoin<C, P> {
    /// No acquisition to wait for: it failed or is draining.
    Miss,
    Settled(Settled<C, P>),
    /// Register a copy waiter: it never leads or adopts.
    Register,
}

/// Gates of a copy-only join on an existing, refreshed flight: a settled
/// result first, then a failed or draining phase (a miss), then the waiter
/// cap (`Overloaded`). A copy join never creates a flight.
pub fn join_copy_existing<
    C: Clone,
    P: Clone,
    O,
    W: WaiterPolicy<Error = E>,
    T,
    E: Copy + PartialEq + From<Error>,
>(
    state: &State<C, P, O, W, T>,
    limits: &FlightLimits,
) -> std::result::Result<CopyJoin<C, P>, E> {
    if let Phase::Complete(result) = &state.phase {
        return Ok(CopyJoin::Settled(Settled::Complete(result.clone())));
    }
    if let Some(partial) = &state.partial {
        return Ok(CopyJoin::Settled(Settled::Partial(partial.clone())));
    }
    if matches!(state.phase, Phase::Failed(_) | Phase::Draining(_)) {
        return Ok(CopyJoin::Miss);
    }
    if state.waiters.len() >= limits.waiters_per_flight {
        return Err(Error::Overloaded.into());
    }
    Ok(CopyJoin::Register)
}

/// What a polled waiter should do next.
#[derive(Debug, Eq, PartialEq)]
pub enum Wait<C, P, E = Error> {
    /// Elected to acquire with its own budget.
    Lead,
    Ready(Settled<C, P>),
    Failed(E),
    /// Parked; the wake target was stored.
    Pending,
}

/// Expire waiters, revoke or hand over a lost leader, and settle drained
/// outcomes. `idle` is whether no retained operation still runs.
pub fn refresh<C, P, O, W: WaiterPolicy<Error = E>, T, E: Copy + PartialEq + From<Error>>(
    state: &mut State<C, P, O, W, T>,
    idle: bool,
    now: impl Fn() -> Instant,
    split: fn(O) -> Published<C, P>,
    wakes: &mut Vec<T>,
) {
    state.refresh(
        idle,
        Error::Unavailable.into(),
        Error::Cancelled.into(),
        now,
        split,
        wakes,
    );
}

/// Expose a drained outcome once no retained operation runs.
pub fn settle<C, P, O, W: WaiterPolicy<Error = E>, T, E: Copy + PartialEq + From<Error>>(
    state: &mut State<C, P, O, W, T>,
    idle: bool,
    split: fn(O) -> Published<C, P>,
    wakes: &mut Vec<T>,
) {
    state.settle(idle, Error::Unavailable.into(), split, wakes);
}

/// Poll acquisition waiter `id` of a refreshed flight. The caller already
/// checked its request scope. Order: the waiter's recorded error, a complete
/// result, a partial result for a ciphertext waiter, a settled failure; then
/// on a pending retry an unissued waiter re-checks its own budget
/// (`budget_deadline`, `attempts`) and is elected under `identity`. An
/// exhausted generation fails the cohort `Unavailable`. Otherwise `park`
/// stores the wake target.
#[allow(clippy::too_many_arguments)] // The waiter's budget plus the flight's step inputs.
pub fn wait<
    C: Clone,
    P: Clone,
    O,
    W: WaiterPolicy<Error = E>,
    T,
    E: Copy + PartialEq + From<Error>,
>(
    state: &mut State<C, P, O, W, T>,
    id: u64,
    identity: &mut Identity,
    generations: u64,
    budget_deadline: Instant,
    attempts: u32,
    idle: bool,
    now: impl Fn() -> Instant,
    split: fn(O) -> Published<C, P>,
    wakes: &mut Vec<T>,
    park: impl FnOnce(&mut Option<T>),
) -> Wait<C, P, E> {
    let waiter = state.waiters.get_mut(&id).expect("registered waiter");
    if let Some(error) = waiter.error {
        return Wait::Failed(error);
    }
    if let Phase::Complete(result) = &state.phase {
        return Wait::Ready(Settled::Complete(result.clone()));
    }
    if !waiter.complete
        && let Some(partial) = &state.partial
    {
        return Wait::Ready(Settled::Partial(partial.clone()));
    }
    if let Phase::Failed(error) = state.phase {
        return Wait::Failed(error);
    }
    if matches!(state.phase, Phase::RetryPending) && !waiter.issued {
        if now() >= budget_deadline || (attempts == 0 && state.partial.is_none()) {
            let error = E::from(if attempts == 0 {
                Error::Unavailable
            } else {
                Error::DeadlineExceeded
            });
            waiter.error = Some(error);
            refresh(state, idle, now, split, wakes);
            return Wait::Failed(error);
        }
        if state.elect(id, identity, generations).is_err() {
            state.begin_completion(Outcome::Failed(Error::Unavailable.into()));
            settle(state, idle, split, wakes);
            return Wait::Failed(Error::Unavailable.into());
        }
        return Wait::Lead;
    }
    park(&mut waiter.waker);
    Wait::Pending
}

/// Poll copy waiter `id` of a refreshed flight: its recorded error, a settled
/// failure, a complete result, then the partial result. `None` after `park`
/// stores the wake target.
pub fn copy_wait<
    C: Clone,
    P: Clone,
    O,
    W: WaiterPolicy<Error = E>,
    T,
    E: Copy + PartialEq + From<Error>,
>(
    state: &mut State<C, P, O, W, T>,
    id: u64,
    park: impl FnOnce(&mut Option<T>),
) -> Option<std::result::Result<Settled<C, P>, E>> {
    let waiter = state.waiters.get_mut(&id).expect("registered waiter");
    if let Some(error) = waiter.error {
        return Some(Err(error));
    }
    if let Phase::Failed(error) = state.phase {
        return Some(Err(error));
    }
    if let Phase::Complete(result) = &state.phase {
        return Some(Ok(Settled::Complete(result.clone())));
    }
    if let Some(partial) = &state.partial {
        return Some(Ok(Settled::Partial(partial.clone())));
    }
    park(&mut waiter.waker);
    None
}

/// The validated leader published `page`. Waiters wake on settlement only.
pub fn publish<C, P, O, W: WaiterPolicy<Error = E>, T, E: Copy + PartialEq + From<Error>>(
    state: &mut State<C, P, O, W, T>,
    page: O,
    idle: bool,
    split: fn(O) -> Published<C, P>,
    wakes: &mut Vec<T>,
) {
    state.begin_completion(Outcome::Published(page));
    settle(state, idle, split, wakes);
}

/// The validated leader `caller` failed. A credential-specific origin
/// rejection fails only `caller` and retries for the next eligible waiter.
/// After a handover the elected caller's own deadline or cancellation no
/// longer describes the cohort, so it also retries for the adopter. Other
/// errors fail the cohort. Waiters are notified at once, before retained
/// operations drain.
pub fn fail<C, P, O, W: WaiterPolicy<Error = E>, T, E: Copy + PartialEq + From<Error>>(
    state: &mut State<C, P, O, W, T>,
    caller: u64,
    failure: FailureKind<E>,
    idle: bool,
    split: fn(O) -> Published<C, P>,
    wakes: &mut Vec<T>,
) {
    let outcome = match failure {
        FailureKind::OriginRejected => {
            state.cancel(caller, Error::OriginRejected.into());
            Outcome::Retry
        }
        FailureKind::OriginForbidden => {
            state.cancel(caller, Error::OriginForbidden.into());
            Outcome::Retry
        }
        FailureKind::Terminal(error)
            if state.handed_over()
                && (error == Error::DeadlineExceeded.into()
                    || error == Error::Cancelled.into()) =>
        {
            Outcome::Retry
        }
        FailureKind::Terminal(error) => Outcome::Failed(error),
    };
    state.begin_completion(outcome);
    state.notify(wakes);
    settle(state, idle, split, wakes);
}

/// Hard ceiling of a fill that may be handed over (Known issue 30): the
/// leader's deadline plus `HANDOVER_SPANS` times its remaining span. The fill
/// normally ends earlier, when no live waiter remains to adopt it; the
/// ceiling only bounds the owned scope's timers. Without handover the
/// ceiling is the leader's deadline.
pub fn handover_ceiling(now: Instant, leader_deadline: Instant, handover: bool) -> Instant {
    if !handover {
        return leader_deadline;
    }
    let span = leader_deadline.saturating_duration_since(now);
    leader_deadline + span * HANDOVER_SPANS
}

/// Read lifetimes a handed-over fill may run past its elected leader's.
pub const HANDOVER_SPANS: u32 = 4;

/// Resource a [`PageFlights`] join reserves, in production order.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Reserve {
    Waiter,
    Flight,
}

/// Simulated waiter: its deadline is both the request scope's and the
/// budget's, read against the table's current time. It owns its admission
/// charge `R` like production `WaiterRecord`.
struct Waiter<R> {
    deadline: Instant,
    now: Rc<Cell<Instant>>,
    _charge: R,
}

impl<R> WaiterPolicy for Waiter<R> {
    type Error = Error;
    fn check(&self) -> Option<Error> {
        (self.now.get() >= self.deadline).then_some(Error::DeadlineExceeded)
    }
    fn deadline(&self) -> Instant {
        self.deadline
    }
}

struct Flight<C, P, O, R> {
    state: State<C, P, O, Waiter<R>, Token>,
    identity: Identity,
    /// Retained leader operations still running (production
    /// `Entry::operations`).
    operations: usize,
    _charge: R,
}

impl<C, P, O, R> Flight<C, P, O, R> {
    fn idle(&self) -> bool {
        self.operations == 0
    }
}

/// Result of a [`PageFlights`] join.
#[derive(Debug, Eq, PartialEq)]
pub enum Joined<C, P> {
    /// No acquisition to wait for (copy joins only).
    Miss,
    Settled(Settled<C, P>),
    /// Registered as waiter `id`.
    Waiter(u64),
}

/// Single-owner page flight table for sans-IO drivers (the simulator). It
/// mirrors production `Flights` table glue (src/read/flight.rs) around the
/// shared step functions above, with an explicit clock and [`Token`] wakes.
/// `R` is the admission charge each waiter and flight owns; dropping it
/// releases the charge.
pub struct PageFlights<C, P, O, R, S> {
    flights: HashMap<PageId, Flight<C, P, O, R>, S>,
    limits: FlightLimits,
    split: fn(O) -> Published<C, P>,
    now: Rc<Cell<Instant>>,
    owner: Rc<()>,
    next_id: u64,
}

impl<C: Clone, P: Clone, O, R, S: BuildHasher> PageFlights<C, P, O, R, S> {
    pub fn new(
        limits: FlightLimits,
        split: fn(O) -> Published<C, P>,
        now: Instant,
        hasher: S,
    ) -> Self {
        Self {
            flights: HashMap::with_hasher(hasher),
            limits,
            split,
            now: Rc::new(Cell::new(now)),
            owner: Rc::new(()),
            next_id: 0,
        }
    }

    /// Number of live flights.
    pub fn len(&self) -> usize {
        self.flights.len()
    }

    pub fn is_empty(&self) -> bool {
        self.flights.is_empty()
    }

    /// Waiters attached to `page`.
    pub fn waiters(&self, page: &PageId) -> usize {
        self.flights.get(page).map_or(0, |f| f.state.waiters.len())
    }

    fn refreshed(
        &mut self,
        page: &PageId,
        wakes: &mut Vec<Token>,
    ) -> Option<&mut Flight<C, P, O, R>> {
        let now = self.now.get();
        let flight = self.flights.get_mut(page)?;
        let idle = flight.idle();
        refresh(&mut flight.state, idle, || now, self.split, wakes);
        Some(flight)
    }

    fn waiter(&mut self, deadline: Instant, charge: R) -> (u64, Waiter<R>) {
        let id = self.next_id;
        self.next_id += 1;
        (
            id,
            Waiter {
                deadline,
                now: self.now.clone(),
                _charge: charge,
            },
        )
    }

    /// Acquisition join (production `Flights::join_prepared`): the existing
    /// flight's gates, the budget gate, the waiter reservation, then for a new
    /// flight the entry cap and the flight reservation.
    #[allow(clippy::too_many_arguments)] // Mirrors production join inputs.
    pub fn join(
        &mut self,
        page: &PageId,
        now: Instant,
        deadline: Instant,
        attempts: u32,
        plaintext: bool,
        mut reserve: impl FnMut(Reserve) -> Result<R>,
        wakes: &mut Vec<Token>,
    ) -> Result<Joined<C, P>> {
        self.now.set(now);
        let limits = self.limits;
        let mut partial = false;
        if let Some(flight) = self.refreshed(page, wakes) {
            if let Some(settled) = join_existing(&flight.state, plaintext, &limits)? {
                return Ok(Joined::Settled(settled));
            }
            partial = flight.state.partial.is_some();
        }
        join_budget_gate(now, deadline, attempts, partial)?;
        let charge = reserve(Reserve::Waiter)?;
        let (id, waiter) = self.waiter(deadline, charge);
        if !self.flights.contains_key(page) {
            entry_cap_gate(self.flights.len(), &self.limits)?;
            let charge = reserve(Reserve::Flight)?;
            self.flights.insert(
                page.clone(),
                Flight {
                    state: State::default(),
                    identity: Identity {
                        owner: self.owner.clone(),
                        incarnation: 0,
                        generation: 0,
                    },
                    operations: 0,
                    _charge: charge,
                },
            );
        }
        let flight = self.flights.get_mut(page).expect("inserted");
        flight.state.register(id, waiter, true, plaintext);
        Ok(Joined::Waiter(id))
    }

    /// Copy-only join (production `Flights::join_copy`): never creates a
    /// flight, leads, or adopts.
    pub fn join_copy(
        &mut self,
        page: &PageId,
        now: Instant,
        deadline: Instant,
        reserve: impl FnOnce(Reserve) -> Result<R>,
        wakes: &mut Vec<Token>,
    ) -> Result<Joined<C, P>> {
        self.now.set(now);
        let limits = self.limits;
        let Some(flight) = self.refreshed(page, wakes) else {
            return Ok(Joined::Miss);
        };
        match join_copy_existing(&flight.state, &limits)? {
            CopyJoin::Miss => return Ok(Joined::Miss),
            CopyJoin::Settled(settled) => return Ok(Joined::Settled(settled)),
            CopyJoin::Register => {}
        }
        let charge = reserve(Reserve::Waiter)?;
        let (id, waiter) = self.waiter(deadline, charge);
        let flight = self.flights.get_mut(page).expect("present");
        flight.state.register(id, waiter, false, false);
        Ok(Joined::Waiter(id))
    }

    /// Poll waiter `id` at `now` with its remaining `attempts`; parks `token`
    /// when nothing is ready. Copy waiters follow production
    /// `CopyWaiter::wait`, others `AcquisitionWaiter::wait`. Both first check
    /// the request scope: a canceled waiter fails `Cancelled`, and one past
    /// its deadline is canceled with `DeadlineExceeded`.
    pub fn poll(
        &mut self,
        page: &PageId,
        id: u64,
        now: Instant,
        attempts: u32,
        token: Token,
        wakes: &mut Vec<Token>,
    ) -> Result<Wait<C, P>> {
        self.now.set(now);
        let (limit, split) = (self.limits.generations_per_flight, self.split);
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        let waiter = flight.state.waiters.get(&id).ok_or(Error::Internal)?;
        let (deadline, copy) = (waiter.deadline, !waiter.acquisition());
        if waiter.error == Some(Error::Cancelled) {
            return Ok(Wait::Failed(Error::Cancelled));
        }
        if now >= deadline {
            flight.state.cancel(id, Error::DeadlineExceeded);
            let idle = flight.idle();
            refresh(&mut flight.state, idle, || now, split, wakes);
            return Ok(Wait::Failed(Error::DeadlineExceeded));
        }
        let idle = flight.idle();
        refresh(&mut flight.state, idle, || now, split, wakes);
        let park = |slot: &mut Option<Token>| *slot = Some(token);
        if copy {
            return Ok(match copy_wait(&mut flight.state, id, park) {
                None => Wait::Pending,
                Some(Ok(settled)) => Wait::Ready(settled),
                Some(Err(error)) => Wait::Failed(error),
            });
        }
        Ok(wait(
            &mut flight.state,
            id,
            &mut flight.identity,
            limit,
            deadline,
            attempts,
            idle,
            || now,
            split,
            wakes,
            park,
        ))
    }

    /// Leader `id` finished (production `Flights::publish_acquired` or
    /// `Flights::fail`). `Internal` for a stale leader (production
    /// `StaleFlight`).
    pub fn finish(
        &mut self,
        page: &PageId,
        id: u64,
        result: std::result::Result<O, FailureKind>,
        wakes: &mut Vec<Token>,
    ) -> Result<()> {
        let split = self.split;
        let flight = self.leader(page, id, wakes)?;
        let idle = flight.idle();
        match result {
            Ok(page) => publish(&mut flight.state, page, idle, split, wakes),
            Err(failure) => fail(&mut flight.state, id, failure, idle, split, wakes),
        }
        Ok(())
    }

    /// The refreshed flight `id` currently leads (production
    /// `Flights::leader_entry`).
    fn leader(
        &mut self,
        page: &PageId,
        id: u64,
        wakes: &mut Vec<Token>,
    ) -> Result<&mut Flight<C, P, O, R>> {
        let flight = self.refreshed(page, wakes).ok_or(Error::Internal)?;
        flight
            .state
            .validate_leader(id, true)
            .map_err(|_| Error::Internal)?;
        Ok(flight)
    }

    /// Let leader `id`'s running acquisition pass to another live waiter when
    /// `id` expires or is canceled (production `Flights::allow_handover`).
    pub fn allow_handover(&mut self, page: &PageId, id: u64, wakes: &mut Vec<Token>) -> Result<()> {
        self.leader(page, id, wakes)?.state.allow_handover();
        Ok(())
    }

    /// Deadline of the waiter `page`'s running acquisition currently serves,
    /// or `None` once it was revoked or settled.
    pub fn leader_deadline(&self, page: &PageId) -> Option<Instant> {
        self.flights
            .get(page)
            .and_then(|f| f.state.leader_deadline())
    }

    /// Whether `page` has an acquisition running (production
    /// `Flights::acquiring`).
    pub fn acquiring(&self, page: &PageId) -> bool {
        self.flights
            .get(page)
            .is_some_and(|f| matches!(f.state.phase, Phase::Acquiring))
    }

    /// Whether `page`'s running acquisition passed to a waiter other than
    /// the elected one.
    pub fn handed_over(&self, page: &PageId) -> bool {
        self.flights
            .get(page)
            .is_some_and(|f| f.state.handed_over())
    }

    /// Whether `id` is the active leader of `page`'s current acquisition.
    pub fn is_leader(&self, page: &PageId, id: u64) -> bool {
        self.flights
            .get(page)
            .is_some_and(|f| f.state.validate_leader(id, true).is_ok())
    }

    /// Retain one operation for leader `id` before it starts (production
    /// `Flights::retain_operation`). Until [`PageFlights::complete_operation`]
    /// runs, a revoked acquisition stays draining and the entry is kept.
    pub fn retain_operation(
        &mut self,
        page: &PageId,
        id: u64,
        wakes: &mut Vec<Token>,
    ) -> Result<()> {
        self.leader(page, id, wakes)?.operations += 1;
        Ok(())
    }

    /// The retained operation finished (production
    /// `Flights::complete_operation`): settle, notify, and remove a quiescent
    /// entry. Call it before `finish`, as production does.
    pub fn complete_operation(&mut self, page: &PageId, wakes: &mut Vec<Token>) -> Result<()> {
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        flight.operations = flight.operations.checked_sub(1).ok_or(Error::Internal)?;
        let flight = self.refreshed(page, wakes).expect("present");
        flight.state.notify(wakes);
        self.remove_quiescent(page);
        Ok(())
    }

    /// Production `Entry::quiescent`: no waiters and no retained operations.
    fn remove_quiescent(&mut self, page: &PageId) {
        if self
            .flights
            .get(page)
            .is_some_and(|f| f.state.waiters.is_empty() && f.idle())
        {
            self.flights.remove(page);
        }
    }

    /// Leader `id`'s driver was dropped without an outcome (production
    /// `Drop for FlightLeader`): revoke at once, never hand over.
    pub fn abandon(&mut self, page: &PageId, id: u64, wakes: &mut Vec<Token>) {
        let split = self.split;
        if let Some(flight) = self.flights.get_mut(page)
            && flight.state.validate_leader(id, true).is_ok()
        {
            flight.state.revoke(Error::Cancelled, wakes);
            let idle = flight.idle();
            settle(&mut flight.state, idle, split, wakes);
        }
    }

    /// Cancel waiter `id` (production `Registration::cancel`).
    pub fn cancel(&mut self, page: &PageId, id: u64, wakes: &mut Vec<Token>) {
        if let Some(flight) = self.flights.get_mut(page) {
            flight.state.cancel(id, Error::Cancelled);
            self.refreshed(page, wakes);
        }
    }

    /// Detach waiter `id`, dropping its charge and removing a quiescent
    /// flight (production `Registration::detach`).
    pub fn detach(&mut self, page: &PageId, id: u64, wakes: &mut Vec<Token>) {
        let Some(flight) = self.flights.get_mut(page) else {
            return;
        };
        flight.state.detach(id);
        self.refreshed(page, wakes);
        self.remove_quiescent(page);
    }

    /// Expire waiters whose deadline passed at `now` and settle outcomes.
    pub fn refresh(&mut self, page: &PageId, now: Instant, wakes: &mut Vec<Token>) {
        self.now.set(now);
        self.refreshed(page, wakes);
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests;
