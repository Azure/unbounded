//! Sans-IO page singleflight rules.
//!
//! Flight transitions run in `crate::flow::coalesce::flight::state::State`; these step functions
//! add the business decisions on top and are generic over the wake target `T` (production `Waker`s,
//! simulator [`Token`]s).

use crate::flow::coalesce::flight::Identity;
use crate::flow::coalesce::flight::state::{Outcome, Phase, Published, State, WaiterPolicy};
use crate::read::{Error, FailureKind, Result};
use std::time::Instant;

mod table;
pub use table::{Fence, FlightEntry, FlightTable, Joined, Operation, Removed, Ticket, Waiter};

/// Names one instantiation of the page [`FlightTable`].
///
/// [`crate::read::worker::WorkerCore`] and the production `Flights` both name their page table
/// through [`PageFlightTable`], so the two drivers share one table type family and one set of step
/// functions; only the payload types differ.
pub trait PageFlightTypes: 'static {
    type Page;
    /// Completed value published to joiners.
    type Complete;
    /// Reusable partial value.
    type Partial;
    /// Leader outcome split by the table's publish function.
    type Outcome;
    /// Per-waiter result payload of the default [`Waiter`].
    type Result;
    type Hash;
    type Wake;
    type Waiter: WaiterPolicy;
    /// Per-entry driver extension.
    type Ext;
    /// Retained operation payload.
    type Retained;
}

/// The page flight table for `F`.
pub type PageFlightTable<F> = FlightTable<
    <F as PageFlightTypes>::Page,
    <F as PageFlightTypes>::Complete,
    <F as PageFlightTypes>::Partial,
    <F as PageFlightTypes>::Outcome,
    <F as PageFlightTypes>::Result,
    <F as PageFlightTypes>::Hash,
    <F as PageFlightTypes>::Wake,
    <F as PageFlightTypes>::Waiter,
    <F as PageFlightTypes>::Ext,
    <F as PageFlightTypes>::Retained,
>;

/// Opaque wake target chosen by the driver (for example, a simulated task id).
pub type Token = u64;

/// A [`std::task::Waker`] as a table wake target. Two targets are equal when waking one would wake
/// the other ([`std::task::Waker::will_wake`]), which is what the entry-capacity queue needs.
#[derive(Clone, Debug)]
pub struct WakeTarget(pub std::task::Waker);

impl WakeTarget {
    pub fn wake(self) {
        self.0.wake();
    }
}

impl PartialEq for WakeTarget {
    fn eq(&self, other: &Self) -> bool {
        self.0.will_wake(&other.0)
    }
}

/// Table bounds. All dimensions must be nonzero.
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
    /// The flight holds `waiters_per_flight` waiters ([`Error::FlightFull`]: it is fetching).
    WaiterCap,
}

impl<E: From<Error>> JoinReject<E> {
    pub fn error(self) -> E {
        match self {
            JoinReject::Failed(error) => error,
            JoinReject::WaiterCap => Error::FlightFull.into(),
        }
    }
}

impl From<JoinReject> for Error {
    fn from(reject: JoinReject) -> Self {
        reject.error()
    }
}

/// Gates of an acquisition join on an existing, refreshed flight: a complete result, then a partial
/// result for a ciphertext caller, then a settled failure, then the waiter cap. `None` means
/// register a waiter (after [`join_budget_gate`] and admission).
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

/// Gates of a copy-only join on an existing, refreshed flight: a settled result first, then a
/// failed or draining phase (a miss), then the waiter cap (`FlightFull`). A copy join never
/// creates a flight.
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
        return Err(Error::FlightFull.into());
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

/// Poll acquisition waiter `id` of a refreshed flight. The caller already checked its request
/// scope. Order: the waiter's recorded error, a complete result, a partial result for a ciphertext
/// waiter, a settled failure; then on a pending retry an unissued waiter re-checks its own budget
/// and is elected under `identity`. An exhausted generation fails the cohort `Unavailable`.
/// Otherwise `park` stores the wake target.
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

/// Poll copy waiter `id` of a refreshed flight: its recorded error, a settled failure, a complete
/// result, then the partial result. `None` after `park` stores the wake target.
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

/// The validated leader `caller` failed. A credential-specific origin rejection fails only `caller`
/// and retries for the next eligible waiter; after a handover it also retries for the adopter.
/// Other errors fail the cohort. Waiters are notified at once, before retained operations drain.
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

/// Hard ceiling of a fill that may be handed over: the leader's deadline plus `HANDOVER_SPANS`
/// times its remaining span. It only bounds the owned scope's timers; without handover it is the
/// leader's deadline.
pub fn handover_ceiling(now: Instant, leader_deadline: Instant, handover: bool) -> Instant {
    if !handover {
        return leader_deadline;
    }
    let span = leader_deadline.saturating_duration_since(now);
    leader_deadline + span * HANDOVER_SPANS
}

/// Read lifetimes a handed-over fill may run past its elected leader's.
pub const HANDOVER_SPANS: u32 = 4;

/// Resource a [`FlightTable`] join reserves, in production order.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Reserve {
    Waiter,
    Flight,
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests;
