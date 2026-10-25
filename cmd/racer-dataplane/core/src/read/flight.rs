//! Sans-IO page singleflight table.
//!
//! [`PageFlights`] coalesces concurrent acquisitions of one page: the first
//! eligible caller leads, others wait, and settlement publishes a complete or
//! partial result or a failure to every waiter. Transitions run in
//! `flow_control::coalesce::flight::state::State`, the same engine production
//! `Flights` uses, with plain [`Token`] wakes instead of `Waker`s. The join
//! gates below are shared with production `Flights::join_prepared`.

use super::error::{Error, Result};
use flow_control::coalesce::flight::state::{Outcome, Phase, Published, State, WaiterPolicy};
use racer_object_wire::model::PageId;
use std::cell::Cell;
use std::collections::HashMap;
use std::collections::hash_map::RandomState;
use std::hash::BuildHasher;
use std::rc::Rc;
use std::time::Instant;

/// Opaque wake target chosen by the driver (for example, a simulated task id).
pub type Token = u64;

/// Table bounds; defaults match production `FlightLimits`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct FlightLimits {
    pub entries: usize,
    pub waiters_per_flight: usize,
    pub generations_per_flight: u64,
}

impl Default for FlightLimits {
    fn default() -> Self {
        Self {
            entries: 1024,
            waiters_per_flight: 256,
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

/// Per-flight waiter cap.
pub fn waiter_cap_gate(waiters: usize, limits: &FlightLimits) -> Result<()> {
    if waiters >= limits.waiters_per_flight {
        Err(Error::Overloaded)
    } else {
        Ok(())
    }
}

/// Table entry cap for a new flight.
pub fn entry_cap_gate(entries: usize, limits: &FlightLimits) -> Result<()> {
    if entries >= limits.entries {
        Err(Error::Overloaded)
    } else {
        Ok(())
    }
}

/// Resource admission for waiters and new flights (production: quotas).
pub trait FlightAdmission {
    /// Admit one waiter.
    fn waiter(&mut self, page: &PageId) -> Result<()>;
    /// Admit one new flight entry.
    fn flight(&mut self, page: &PageId) -> Result<()>;
    /// Release one waiter admitted earlier.
    fn release_waiter(&mut self, page: &PageId);
    /// Release one flight admitted earlier.
    fn release_flight(&mut self, page: &PageId);
}

/// Admission that never rejects.
#[derive(Clone, Copy, Debug, Default)]
pub struct Unlimited;

impl FlightAdmission for Unlimited {
    fn waiter(&mut self, _: &PageId) -> Result<()> {
        Ok(())
    }
    fn flight(&mut self, _: &PageId) -> Result<()> {
        Ok(())
    }
    fn release_waiter(&mut self, _: &PageId) {}
    fn release_flight(&mut self, _: &PageId) {}
}

/// Waiter policy reading the table's current time.
struct Waiter {
    deadline: Instant,
    now: Rc<Cell<Instant>>,
}

impl WaiterPolicy for Waiter {
    type Error = Error;
    fn check(&self) -> Option<Error> {
        (self.now.get() >= self.deadline).then_some(Error::DeadlineExceeded)
    }
    fn deadline(&self) -> Instant {
        self.deadline
    }
}

struct Flight<C, P, O> {
    state: State<C, P, O, Waiter, Token>,
    generation: u64,
    /// Retained leader operations still running (production
    /// `Entry::operations`). A revoked acquisition settles, and a new leader
    /// can be elected, only once this reaches zero, and the entry is never
    /// removed while it is nonzero.
    operations: usize,
}

impl<C, P, O> Flight<C, P, O> {
    fn idle(&self) -> bool {
        self.operations == 0
    }
}

/// Result of a join.
#[derive(Debug, Eq, PartialEq)]
pub enum Joined<C, P> {
    /// The page is already complete.
    Complete(C),
    /// A ciphertext-only caller may use the retained partial result.
    Partial(P),
    /// Registered as waiter `id`.
    Waiter(u64),
}

/// What a waiter should do next.
#[derive(Debug, Eq, PartialEq)]
pub enum Event<C> {
    /// Elected to acquire.
    Lead,
    Complete(C),
    Failed(Error),
    /// Parked; the table wakes the token later.
    Pending,
}

/// Sans-IO singleflight table keyed by page.
pub struct PageFlights<C, P, O, S = RandomState> {
    flights: HashMap<PageId, Flight<C, P, O>, S>,
    limits: FlightLimits,
    split: fn(O) -> Published<C, P>,
    now: Rc<Cell<Instant>>,
    next_id: u64,
}

impl<C: Clone, P: Clone, O> PageFlights<C, P, O> {
    pub fn new(limits: FlightLimits, split: fn(O) -> Published<C, P>, now: Instant) -> Self {
        Self::with_hasher(limits, split, now, RandomState::new())
    }
}

impl<C: Clone, P: Clone, O, S: BuildHasher> PageFlights<C, P, O, S> {
    /// Like [`PageFlights::new`] with an explicit (for example seeded) hasher.
    pub fn with_hasher(
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

    /// Join `page` following production gate order: settled results first,
    /// then the waiter cap, budget, waiter admission, and for a new flight the
    /// entry cap and flight admission.
    #[allow(clippy::too_many_arguments)] // Mirrors production join inputs.
    pub fn join(
        &mut self,
        page: &PageId,
        now: Instant,
        deadline: Instant,
        attempts: u32,
        plaintext: bool,
        admission: &mut impl FlightAdmission,
        wakes: &mut Vec<Token>,
    ) -> Result<Joined<C, P>> {
        self.now.set(now);
        if let Some(flight) = self.flights.get_mut(page) {
            refresh(flight, self.split, self.now.get(), wakes);
            if let Phase::Complete(result) = &flight.state.phase {
                return Ok(Joined::Complete(result.clone()));
            }
            if !plaintext && let Some(partial) = &flight.state.partial {
                return Ok(Joined::Partial(partial.clone()));
            }
            if let Phase::Failed(error) = flight.state.phase {
                return Err(error);
            }
            waiter_cap_gate(flight.state.waiters.len(), &self.limits)?;
        }
        let partial = self
            .flights
            .get(page)
            .is_some_and(|f| f.state.partial.is_some());
        join_budget_gate(now, deadline, attempts, partial)?;
        admission.waiter(page)?;
        let id = self.next_id;
        self.next_id += 1;
        if !self.flights.contains_key(page) {
            if let Err(error) = entry_cap_gate(self.flights.len(), &self.limits)
                .and_then(|()| admission.flight(page))
            {
                admission.release_waiter(page);
                return Err(error);
            }
            self.flights.insert(
                page.clone(),
                Flight {
                    state: State::default(),
                    generation: 0,
                    operations: 0,
                },
            );
        }
        let flight = self.flights.get_mut(page).expect("inserted");
        flight.state.register(
            id,
            Waiter {
                deadline,
                now: self.now.clone(),
            },
            true,
            plaintext,
        );
        Ok(Joined::Waiter(id))
    }

    /// Poll waiter `id` at `now`; parks `token` when nothing is ready.
    pub fn poll(
        &mut self,
        page: &PageId,
        id: u64,
        now: Instant,
        token: Token,
        wakes: &mut Vec<Token>,
    ) -> Result<Event<C>> {
        self.now.set(now);
        let limit = self.limits.generations_per_flight;
        let split = self.split;
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        refresh(flight, split, now, wakes);
        let waiter = flight.state.waiters.get(&id).ok_or(Error::Internal)?;
        if let Some(error) = waiter.error {
            // Production `AcquisitionWaiter::wait` checks the caller's scope
            // (cancellation, then deadline) before a recorded error such as
            // `OriginRejected`.
            let error = if error != Error::Cancelled && now >= waiter.deadline() {
                Error::DeadlineExceeded
            } else {
                error
            };
            return Ok(Event::Failed(error));
        }
        match &flight.state.phase {
            Phase::Complete(result) => return Ok(Event::Complete(result.clone())),
            Phase::Failed(error) => return Ok(Event::Failed(*error)),
            _ => {}
        }
        let mut identity = flow_identity(flight.generation);
        if flight
            .state
            .elect(id, &mut identity, limit)
            .map_err(|_| Error::Overloaded)?
        {
            flight.generation = identity.generation;
            return Ok(Event::Lead);
        }
        if let Some(waiter) = flight.state.waiters.get_mut(&id) {
            waiter.waker = Some(token);
        }
        Ok(Event::Pending)
    }

    /// Leader `id` finished with `outcome`; settle and wake waiters.
    pub fn finish(
        &mut self,
        page: &PageId,
        id: u64,
        outcome: Outcome<O, Error>,
        wakes: &mut Vec<Token>,
    ) -> Result<()> {
        let split = self.split;
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        flight
            .state
            .validate_leader(id, true)
            .map_err(|_| Error::Internal)?;
        let outcome = handover_outcome(flight.state.handed_over(), outcome);
        flight.state.begin_completion(outcome);
        let idle = flight.idle();
        flight.state.settle(idle, Error::Unavailable, split, wakes);
        Ok(())
    }

    /// Let leader `id`'s running acquisition pass to another live waiter when
    /// `id` expires or is canceled (production `Flights::allow_handover`, for
    /// fills whose caller does not fence on the operation). See
    /// [`handover_outcome`] and [`handover_ceiling`].
    pub fn allow_handover(&mut self, page: &PageId, id: u64) -> Result<()> {
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        flight
            .state
            .validate_leader(id, true)
            .map_err(|_| Error::Internal)?;
        flight.state.allow_handover();
        Ok(())
    }

    /// Deadline of the waiter `page`'s running acquisition currently serves,
    /// or `None` once it was revoked or settled.
    pub fn leader_deadline(&self, page: &PageId) -> Option<Instant> {
        self.flights
            .get(page)
            .and_then(|f| f.state.leader_deadline())
    }

    /// Whether `page` has an acquisition running (a leader is fetching it),
    /// so a new waiter would join it rather than start another.
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
    /// False once the leader was revoked (canceled or expired), in which case
    /// [`PageFlights::finish`] and [`PageFlights::reject`] return `Internal`
    /// like production's `StaleFlight` and the driver drops the outcome.
    pub fn is_leader(&self, page: &PageId, id: u64) -> bool {
        self.flights
            .get(page)
            .is_some_and(|f| f.state.validate_leader(id, true).is_ok())
    }

    /// Retain one operation for leader `id`'s acquisition before it starts
    /// (production `Flights::retain_operation`). Until
    /// [`PageFlights::complete_operation`] runs, a revoked acquisition stays
    /// draining: no other waiter is elected and the entry is kept, so a
    /// revoked leader's still-running fetch cannot overlap a new leader's.
    pub fn retain_operation(&mut self, page: &PageId, id: u64) -> Result<()> {
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        flight
            .state
            .validate_leader(id, true)
            .map_err(|_| Error::Internal)?;
        flight.operations += 1;
        Ok(())
    }

    /// The operation retained by [`PageFlights::retain_operation`] finished
    /// (production `FlightOperation::complete`). Settles a drained outcome,
    /// wakes waiters, and removes a quiescent entry. Call it before
    /// `finish`/`reject`, as production completes the operation before
    /// publishing.
    pub fn complete_operation(
        &mut self,
        page: &PageId,
        admission: &mut impl FlightAdmission,
        wakes: &mut Vec<Token>,
    ) -> Result<()> {
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        flight.operations = flight.operations.checked_sub(1).ok_or(Error::Internal)?;
        refresh(flight, self.split, self.now.get(), wakes);
        flight.state.notify(wakes);
        self.remove_quiescent(page, admission);
        Ok(())
    }

    fn remove_quiescent(&mut self, page: &PageId, admission: &mut impl FlightAdmission) {
        let Some(flight) = self.flights.get(page) else {
            return;
        };
        let quiescent = flight.state.waiters.is_empty()
            && flight.idle()
            && !matches!(flight.state.phase, Phase::Acquiring | Phase::Draining(_));
        if quiescent {
            self.flights.remove(page);
            admission.release_flight(page);
        }
    }

    /// Leader `id`'s own origin attempt was rejected for its credentials
    /// (`error` is `OriginRejected` or `OriginForbidden`). Matches production
    /// `Flights::fail` with `AcquisitionFailure::OriginRejected`/`OriginForbidden`:
    /// the leader fails with `error` and is never served another caller's
    /// result for this cohort, and the next eligible waiter is elected.
    pub fn reject(
        &mut self,
        page: &PageId,
        id: u64,
        error: Error,
        wakes: &mut Vec<Token>,
    ) -> Result<()> {
        if !matches!(error, Error::OriginRejected | Error::OriginForbidden) {
            return Err(Error::Internal);
        }
        let split = self.split;
        let flight = self.flights.get_mut(page).ok_or(Error::Internal)?;
        flight
            .state
            .validate_leader(id, true)
            .map_err(|_| Error::Internal)?;
        flight.state.cancel(id, error);
        flight.state.begin_completion(Outcome::Retry);
        flight.state.notify(wakes);
        let idle = flight.idle();
        flight.state.settle(idle, Error::Unavailable, split, wakes);
        Ok(())
    }

    /// Leader `id`'s acquisition was abandoned without an outcome: no driver
    /// remains to continue it, so it is revoked at once and never handed over
    /// (production `Drop for FlightLeader`).
    pub fn abandon(&mut self, page: &PageId, id: u64, wakes: &mut Vec<Token>) {
        let split = self.split;
        if let Some(flight) = self.flights.get_mut(page)
            && flight.state.validate_leader(id, true).is_ok()
        {
            flight.state.revoke(Error::Cancelled, wakes);
            let idle = flight.idle();
            flight.state.settle(idle, Error::Unavailable, split, wakes);
        }
    }

    /// Cancel waiter `id`; a canceled leader is revoked on the next refresh.
    pub fn cancel(&mut self, page: &PageId, id: u64, wakes: &mut Vec<Token>) {
        if let Some(flight) = self.flights.get_mut(page) {
            flight.state.cancel(id, Error::Cancelled);
            refresh(flight, self.split, self.now.get(), wakes);
        }
    }

    /// Detach waiter `id`, releasing its admission and removing a quiescent flight.
    pub fn detach(
        &mut self,
        page: &PageId,
        id: u64,
        admission: &mut impl FlightAdmission,
        wakes: &mut Vec<Token>,
    ) {
        let Some(flight) = self.flights.get_mut(page) else {
            return;
        };
        if flight.state.waiters.contains_key(&id) {
            flight.state.detach(id);
            admission.release_waiter(page);
        }
        refresh(flight, self.split, self.now.get(), wakes);
        self.remove_quiescent(page, admission);
    }

    /// Expire waiters whose deadline passed at `now` and settle outcomes.
    pub fn refresh(&mut self, page: &PageId, now: Instant, wakes: &mut Vec<Token>) {
        self.now.set(now);
        if let Some(flight) = self.flights.get_mut(page) {
            refresh(flight, self.split, self.now.get(), wakes);
        }
    }
}

/// Outcome of a handed-over acquisition. The elected caller's own deadline
/// or cancellation no longer describes the cohort: an operation that ended
/// with it retries for the adopting waiter instead of failing everyone.
pub fn handover_outcome<O>(handed_over: bool, outcome: Outcome<O, Error>) -> Outcome<O, Error> {
    match outcome {
        Outcome::Failed(Error::DeadlineExceeded | Error::Cancelled) if handed_over => {
            Outcome::Retry
        }
        outcome => outcome,
    }
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

fn refresh<C, P, O>(
    flight: &mut Flight<C, P, O>,
    split: fn(O) -> Published<C, P>,
    now: Instant,
    wakes: &mut Vec<Token>,
) {
    let idle = flight.idle();
    flight.state.refresh(
        idle,
        Error::Unavailable,
        Error::Cancelled,
        || now,
        split,
        wakes,
    );
}

/// Identity values used only for generation accounting; the table itself is
/// the fence in this single-owner model.
fn flow_identity(generation: u64) -> flow_control::coalesce::flight::Identity {
    flow_control::coalesce::flight::Identity {
        owner: Rc::new(()),
        incarnation: 0,
        generation,
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests;
