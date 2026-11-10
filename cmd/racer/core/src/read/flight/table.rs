//! [`FlightTable`]: the one keyed singleflight table.
//!
//! It drives `crate::flow::coalesce::flight::state::State` through the step functions in
//! [`super`], so join gates, election, handover, settlement, and retry are shared by every
//! instance: production page flights, metadata cohorts, the simulator, and the DST model. It adds
//! what a whole table owns:
//!
//! - entries keyed by any `K` (pages, object metadata, local copies), each with a fresh
//!   incarnation and a caller-defined extension `X`;
//! - an explicit owner [`Fence`] chosen by the caller, carried by every [`Ticket`] and
//!   [`Operation`] so capabilities from another table, worker, or incarnation are rejected by value
//!   rather than by pointer identity;
//! - retained operations with payloads `Q`, capped at [`FlightLimits::operations_per_flight`] and
//!   completed by handle in two phases (take the payload, then clear its fence);
//! - quiescent removal (no waiters and no retained operations), a FIFO of callers waiting for
//!   entry capacity, woken one per removed entry, and stop/drain/sweep for shutdown.
//!
//! Payloads are generic: `C` is a complete result, `P` a reusable partial result, `O` what a leader
//! publishes (split by `split`), `R` the admission charge each entry owns, and `W` the waiter
//! policy (by default [`Waiter`], whose deadline is read against the table's clock and which owns
//! an `R` charge). Errors are `W::Error`; table-level stale capabilities are [`Error::Internal`].

// Entry types carry every table parameter; aliases would only rename them.
#![allow(clippy::type_complexity)]

use super::{
    CopyJoin, FlightLimits, JoinReject, Reserve, Settled, Token, Wait, copy_wait, fail,
    join_budget_gate, join_copy_existing, join_existing, publish, refresh, settle, wait,
};
use crate::flow::coalesce::flight::Identity;
use crate::flow::coalesce::flight::state::{Phase, Published, State, WaiterPolicy};
use crate::read::{Error, FailureKind, Result};
use std::cell::Cell;
use std::collections::hash_map::RandomState;
use std::collections::{BTreeMap, HashMap, VecDeque};
use std::hash::{BuildHasher, Hash};
use std::rc::Rc;
use std::time::Instant;

/// Explicit table owner identity (for example, the worker index). Tickets and operations from a
/// table with another fence are stale.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Fence(pub u64);

/// Capability of one elected leader: the table fence, entry incarnation, acquisition generation,
/// and waiter id. It is stale once any of them changes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Ticket {
    pub fence: Fence,
    pub incarnation: u64,
    pub generation: u64,
    pub waiter: u64,
}

/// A retained leader operation. Completing it is the only way to release it; it is fenced to the
/// entry incarnation that retained it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Operation {
    pub(crate) fence: Fence,
    pub(crate) incarnation: u64,
    pub(crate) id: u64,
}

/// Result of a [`FlightTable`] join.
#[derive(Debug, Eq, PartialEq)]
pub enum Joined<C, P> {
    /// No acquisition to wait for (copy joins only).
    Miss,
    Settled(Settled<C, P>),
    /// Registered as waiter `id`.
    Waiter(u64),
}

/// Default table waiter: its deadline is both the request scope's and the budget's, read against
/// the table's current time. It owns its admission charge `R`.
pub struct Waiter<R> {
    pub(super) deadline: Instant,
    pub(super) now: Rc<Cell<Instant>>,
    pub(super) _charge: R,
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

/// A retained operation slot: its payload, or the fence left while the payload is destroyed.
enum Slot<Q> {
    Retained(Q),
    Completing,
}

/// One keyed flight. Removed entries are returned to the caller so payloads whose destructors run
/// code (wakers, admission charges) can be dropped after releasing a borrow of the table.
pub struct FlightEntry<C, P, O, R, T, W: WaiterPolicy, X, Q> {
    state: State<C, P, O, W, T>,
    identity: Identity,
    incarnation: u64,
    /// Retained leader operations still running.
    operations: BTreeMap<u64, Slot<Q>>,
    /// Operations whose payload was taken but whose fence is still held.
    completing: usize,
    sweep_id: u64,
    ext: X,
    _charge: R,
}

impl<C, P, O, R, T, W: WaiterPolicy, X, Q> FlightEntry<C, P, O, R, T, W, X, Q> {
    fn idle(&self) -> bool {
        self.operations.is_empty()
    }

    fn quiescent(&self) -> bool {
        self.state.waiters.is_empty() && self.idle()
    }

    pub fn state(&self) -> &State<C, P, O, W, T> {
        &self.state
    }

    pub fn ext(&self) -> &X {
        &self.ext
    }

    pub fn ext_mut(&mut self) -> &mut X {
        &mut self.ext
    }

    pub fn incarnation(&self) -> u64 {
        self.incarnation
    }

    /// Raw state, for tests that forge waiter deadlines. Bypasses every table invariant.
    #[doc(hidden)]
    pub fn state_mut(&mut self) -> &mut State<C, P, O, W, T> {
        &mut self.state
    }

    /// The reusable partial result, which a leader may discard.
    pub fn partial_mut(&mut self) -> &mut Option<P> {
        &mut self.state.partial
    }

    /// Retained operations, including those completing.
    pub fn operations(&self) -> usize {
        self.operations.len()
    }

    /// Operations whose payload was taken but not yet completed.
    pub fn completing(&self) -> usize {
        self.completing
    }
}

/// Single-owner keyed flight table with an explicit clock and wake targets `T`.
pub struct FlightTable<
    K,
    C,
    P,
    O,
    R,
    S = RandomState,
    T = Token,
    W: WaiterPolicy = Waiter<R>,
    X = (),
    Q = (),
> {
    entries: HashMap<K, FlightEntry<C, P, O, R, T, W, X, Q>, S>,
    /// Entries in insertion order for bounded sweeps and samples.
    sweep: BTreeMap<u64, K>,
    next_sweep: u64,
    cursor: u64,
    limits: FlightLimits,
    split: fn(O) -> Published<C, P>,
    now: Rc<Cell<Instant>>,
    fence: Fence,
    /// `State::elect` still compares this by pointer; it never leaves the table.
    owner: Rc<()>,
    next_id: u64,
    next_incarnation: u64,
    next_operation: u64,
    capacity: VecDeque<T>,
    /// Wake targets replaced or detached since the last [`Self::take_retired`], when retained.
    retired: Option<Vec<T>>,
    stopping: bool,
    drain: Option<T>,
}

/// A removed entry, to drop outside any borrow of the table.
pub type Removed<C, P, O, R, T, W, X, Q> = FlightEntry<C, P, O, R, T, W, X, Q>;

impl<
    K: Eq + Hash + Clone,
    C: Clone,
    P: Clone,
    O,
    R,
    S: BuildHasher,
    T: Clone + PartialEq,
    W: WaiterPolicy<Error = E>,
    X,
    Q,
    E: Copy + PartialEq + From<Error>,
> FlightTable<K, C, P, O, R, S, T, W, X, Q>
{
    pub fn new(
        limits: FlightLimits,
        split: fn(O) -> Published<C, P>,
        now: Instant,
        hasher: S,
    ) -> Self {
        Self::with_fence(Fence(0), limits, split, now, hasher)
    }

    pub fn with_fence(
        fence: Fence,
        limits: FlightLimits,
        split: fn(O) -> Published<C, P>,
        now: Instant,
        hasher: S,
    ) -> Self {
        Self {
            entries: HashMap::with_hasher(hasher),
            sweep: BTreeMap::new(),
            next_sweep: 0,
            cursor: 0,
            limits,
            split,
            now: Rc::new(Cell::new(now)),
            fence,
            owner: Rc::new(()),
            next_id: 0,
            next_incarnation: 0,
            next_operation: 0,
            capacity: VecDeque::new(),
            retired: None,
            stopping: false,
            drain: None,
        }
    }

    /// Hand replaced and detached wake targets to [`Self::take_retired`] instead of dropping them
    /// inside the table, so a driver whose targets run code on drop (a `Waker`) can drop them after
    /// releasing its borrow of the table.
    pub fn retain_retired(mut self) -> Self {
        self.retired = Some(Vec::new());
        self
    }

    /// Wake targets retired since the last call; empty unless [`Self::retain_retired`].
    pub fn take_retired(&mut self) -> Vec<T> {
        self.retired
            .as_mut()
            .map(std::mem::take)
            .unwrap_or_default()
    }

    pub fn fence(&self) -> Fence {
        self.fence
    }

    pub fn limits(&self) -> &FlightLimits {
        &self.limits
    }

    /// Advance the table clock used by refreshes and the default waiter policy.
    pub fn set_now(&mut self, now: Instant) {
        self.now.set(now);
    }

    /// Number of live entries.
    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub fn contains(&self, key: &K) -> bool {
        self.entries.contains_key(key)
    }

    pub fn entry(&self, key: &K) -> Option<&FlightEntry<C, P, O, R, T, W, X, Q>> {
        self.entries.get(key)
    }

    pub fn entry_mut(&mut self, key: &K) -> Option<&mut FlightEntry<C, P, O, R, T, W, X, Q>> {
        self.entries.get_mut(key)
    }

    pub fn entries(&self) -> impl Iterator<Item = &FlightEntry<C, P, O, R, T, W, X, Q>> {
        self.entries.values()
    }

    /// At most `N` entries in insertion order, without moving the sweep cursor.
    pub fn sample<const N: usize>(&self) -> [Option<&FlightEntry<C, P, O, R, T, W, X, Q>>; N] {
        let mut keys = self.sweep.values();
        std::array::from_fn(|_| keys.next().and_then(|key| self.entries.get(key)))
    }

    /// Earliest waiter deadline across all entries.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.entries
            .values()
            .filter_map(|e| e.state.deadlines.first_key_value().map(|((at, _), _)| *at))
            .min()
    }

    /// Waiters attached to `key`.
    pub fn waiters(&self, key: &K) -> usize {
        self.entries.get(key).map_or(0, |e| e.state.waiters.len())
    }

    /// Whether waiter `id` is attached to `key`.
    pub fn attached(&self, key: &K, id: u64) -> bool {
        self.entries
            .get(key)
            .is_some_and(|e| e.state.waiters.contains_key(&id))
    }

    /// Whether waiter `id` is attached to incarnation `incarnation` of `key`.
    pub fn registered(&self, key: &K, incarnation: u64, id: u64) -> bool {
        self.entries
            .get(key)
            .is_some_and(|e| e.incarnation == incarnation && e.state.waiters.contains_key(&id))
    }

    /// Whether `key` settled to a complete result or a failure.
    pub fn settled(&self, key: &K) -> bool {
        self.entries
            .get(key)
            .is_some_and(|e| matches!(e.state.phase, Phase::Complete(_) | Phase::Failed(_)))
    }

    /// Retained operations of `key`.
    pub fn operations(&self, key: &K) -> usize {
        self.entries.get(key).map_or(0, |e| e.operations.len())
    }

    fn refreshed(
        &mut self,
        key: &K,
        wakes: &mut Vec<T>,
    ) -> Option<&mut FlightEntry<C, P, O, R, T, W, X, Q>> {
        let now = self.now.get();
        let entry = self.entries.get_mut(key)?;
        let idle = entry.idle();
        refresh(&mut entry.state, idle, || now, self.split, wakes);
        Some(entry)
    }

    /// Refresh `key` at the current table time and return it.
    pub fn refresh_entry(
        &mut self,
        key: &K,
        wakes: &mut Vec<T>,
    ) -> Option<&mut FlightEntry<C, P, O, R, T, W, X, Q>> {
        self.refreshed(key, wakes)
    }

    /// Whether a new entry may be created now. Otherwise `token` queues FIFO and is woken when an
    /// entry is removed; call again on wake. Earlier queued callers keep priority.
    pub fn wait_entry(&mut self, token: T) -> bool {
        let front = self.capacity.front().is_none_or(|t| *t == token);
        if front && self.entries.len() < self.limits.entries {
            if !self.capacity.is_empty() {
                self.capacity.pop_front();
            }
            return true;
        }
        if !self.capacity.contains(&token) {
            self.capacity.push_back(token);
        }
        false
    }

    /// Withdraw `token` from the entry-capacity queue, passing a pending wake to the next caller.
    pub fn cancel_entry_wait(&mut self, token: &T, wakes: &mut Vec<T>) {
        let Some(at) = self.capacity.iter().position(|t| t == token) else {
            return;
        };
        self.capacity.remove(at);
        if at == 0
            && self.entries.len() < self.limits.entries
            && let Some(next) = self.capacity.front()
        {
            wakes.push(next.clone());
        }
    }

    /// Gates of an existing refreshed entry: a settled result to share, or a rejection (waiter cap
    /// or settled failure). `Ok(None)` when absent or joinable.
    pub fn join_existing(
        &mut self,
        key: &K,
        plaintext: bool,
        wakes: &mut Vec<T>,
    ) -> std::result::Result<Option<Settled<C, P>>, JoinReject<E>> {
        let limits = self.limits;
        match self.refreshed(key, wakes) {
            Some(entry) => join_existing(&entry.state, plaintext, &limits),
            None => Ok(None),
        }
    }

    /// Copy-join gates of an existing refreshed entry.
    pub fn join_copy_existing(
        &mut self,
        key: &K,
        wakes: &mut Vec<T>,
    ) -> std::result::Result<CopyJoin<C, P>, E> {
        let limits = self.limits;
        match self.refreshed(key, wakes) {
            Some(entry) => join_copy_existing(&entry.state, &limits),
            None => Ok(CopyJoin::Miss),
        }
    }

    /// `Overloaded` when no new entry fits under [`FlightLimits::entries`].
    pub fn entry_capacity(&self) -> Result<()> {
        super::entry_cap_gate(self.entries.len(), &self.limits)
    }

    /// Create the entry for `key` with extension `ext` and entry charge `charge`. The caller has
    /// checked [`Self::entry_capacity`]; returns the new incarnation, or `None` if `key` exists.
    pub fn insert(&mut self, key: K, ext: X, charge: R) -> Option<u64> {
        if self.entries.contains_key(&key) {
            return None;
        }
        let incarnation = self.next_incarnation;
        self.next_incarnation += 1;
        let sweep_id = self.next_sweep;
        self.next_sweep += 1;
        self.sweep.insert(sweep_id, key.clone());
        self.entries.insert(
            key,
            FlightEntry {
                state: State::default(),
                identity: Identity {
                    owner: self.owner.clone(),
                    incarnation,
                    generation: 0,
                },
                incarnation,
                operations: BTreeMap::new(),
                completing: 0,
                sweep_id,
                ext,
                _charge: charge,
            },
        );
        Some(incarnation)
    }

    /// Attach waiter policy `waiter` to the existing entry `key`. `acquisition` waiters may lead;
    /// `complete` waiters need a complete result. Returns the waiter id.
    pub fn register(
        &mut self,
        key: &K,
        waiter: W,
        acquisition: bool,
        complete: bool,
    ) -> Option<u64> {
        if !self.entries.contains_key(key) {
            return None;
        }
        let id = self.next_waiter_id();
        self.register_as(key, id, waiter, acquisition, complete);
        Some(id)
    }

    /// Allocate a never-reused waiter id.
    pub fn next_waiter_id(&mut self) -> u64 {
        let id = self.next_id;
        self.next_id += 1;
        id
    }

    /// [`Self::register`] with an id from [`Self::next_waiter_id`]. `false` when `key` is absent.
    pub fn register_as(
        &mut self,
        key: &K,
        id: u64,
        waiter: W,
        acquisition: bool,
        complete: bool,
    ) -> bool {
        let Some(entry) = self.entries.get_mut(key) else {
            return false;
        };
        entry.state.register(id, waiter, acquisition, complete);
        true
    }

    /// Poll acquisition waiter `id` of the refreshed entry `key` with its budget. `Lead` elects
    /// it under a new generation; `park` stores the wake target when nothing is ready.
    /// `Internal` when `id` is not attached.
    pub fn wait(
        &mut self,
        key: &K,
        id: u64,
        budget_deadline: Instant,
        attempts: u32,
        wakes: &mut Vec<T>,
        park: impl FnOnce(&mut Option<T>),
    ) -> Result<Wait<C, P, E>> {
        let (limit, split, now) = (
            self.limits.generations_per_flight,
            self.split,
            self.now.get(),
        );
        let entry = self.refreshed(key, wakes).ok_or(Error::Internal)?;
        if !entry.state.waiters.contains_key(&id) {
            return Err(Error::Internal);
        }
        let idle = entry.idle();
        Ok(wait(
            &mut entry.state,
            id,
            &mut entry.identity,
            limit,
            budget_deadline,
            attempts,
            idle,
            || now,
            split,
            wakes,
            park,
        ))
    }

    /// Store waiter `id`'s wake target without refreshing. `false` when `id` is not attached.
    pub fn park(&mut self, key: &K, id: u64, park: impl FnOnce(&mut Option<T>)) -> bool {
        let Some(waiter) = self
            .entries
            .get_mut(key)
            .and_then(|e| e.state.waiters.get_mut(&id))
        else {
            return false;
        };
        park(&mut waiter.waker);
        true
    }

    /// Poll copy waiter `id` of the refreshed entry `key`. `None` after `park`.
    pub fn copy_wait(
        &mut self,
        key: &K,
        id: u64,
        wakes: &mut Vec<T>,
        park: impl FnOnce(&mut Option<T>),
    ) -> Result<Option<std::result::Result<Settled<C, P>, E>>> {
        let entry = self.refreshed(key, wakes).ok_or(Error::Internal)?;
        if !entry.state.waiters.contains_key(&id) {
            return Err(Error::Internal);
        }
        Ok(copy_wait(&mut entry.state, id, park))
    }

    /// Leader `id` finished with a publication or failure. `Internal` for a stale leader.
    pub fn finish(
        &mut self,
        key: &K,
        id: u64,
        result: std::result::Result<O, FailureKind<E>>,
        wakes: &mut Vec<T>,
    ) -> Result<()> {
        let split = self.split;
        let entry = self.leader(key, id, wakes)?;
        let idle = entry.idle();
        match result {
            Ok(value) => publish(&mut entry.state, value, idle, split, wakes),
            Err(failure) => fail(&mut entry.state, id, failure, idle, split, wakes),
        }
        Ok(())
    }

    /// The refreshed entry `id` currently leads.
    fn leader(
        &mut self,
        key: &K,
        id: u64,
        wakes: &mut Vec<T>,
    ) -> Result<&mut FlightEntry<C, P, O, R, T, W, X, Q>> {
        let entry = self.refreshed(key, wakes).ok_or(Error::Internal)?;
        entry
            .state
            .validate_leader(id, true)
            .map_err(|_| Error::Internal)?;
        Ok(entry)
    }

    /// The refreshed entry `ticket` currently leads. `Internal` when stale.
    pub fn leader_entry(
        &mut self,
        key: &K,
        ticket: &Ticket,
        wakes: &mut Vec<T>,
    ) -> Result<&mut FlightEntry<C, P, O, R, T, W, X, Q>> {
        if ticket.fence != self.fence {
            return Err(Error::Internal);
        }
        let entry = self.leader(key, ticket.waiter, wakes)?;
        if entry.incarnation != ticket.incarnation || entry.identity.generation != ticket.generation
        {
            return Err(Error::Internal);
        }
        Ok(entry)
    }

    /// The capability of `key`'s active leader `id`, if it is one.
    pub fn ticket(&self, key: &K, id: u64) -> Option<Ticket> {
        let entry = self.entries.get(key)?;
        entry.state.validate_leader(id, true).ok()?;
        Some(Ticket {
            fence: self.fence,
            incarnation: entry.incarnation,
            generation: entry.identity.generation,
            waiter: id,
        })
    }

    /// `Internal` unless `ticket` still names `key`'s active leader in this table.
    pub fn validate(&self, key: &K, ticket: &Ticket) -> Result<()> {
        if self.ticket(key, ticket.waiter) == Some(*ticket) {
            Ok(())
        } else {
            Err(Error::Internal)
        }
    }

    /// Let leader `id`'s running acquisition pass to another live waiter when `id` expires or is
    /// canceled.
    pub fn allow_handover(&mut self, key: &K, id: u64, wakes: &mut Vec<T>) -> Result<()> {
        self.leader(key, id, wakes)?.state.allow_handover();
        Ok(())
    }

    /// Deadline of the waiter `key`'s running acquisition currently serves, or `None` once it was
    /// revoked or settled.
    pub fn leader_deadline(&self, key: &K) -> Option<Instant> {
        self.entries
            .get(key)
            .and_then(|e| e.state.leader_deadline())
    }

    /// Whether `key` has an acquisition running.
    pub fn acquiring(&self, key: &K) -> bool {
        self.entries
            .get(key)
            .is_some_and(|e| matches!(e.state.phase, Phase::Acquiring))
    }

    /// Whether `key`'s running acquisition passed to a waiter other than the elected one.
    pub fn handed_over(&self, key: &K) -> bool {
        self.entries.get(key).is_some_and(|e| e.state.handed_over())
    }

    /// Whether `id` is the active leader of `key`'s current acquisition.
    pub fn is_leader(&self, key: &K, id: u64) -> bool {
        self.entries
            .get(key)
            .is_some_and(|e| e.state.validate_leader(id, true).is_ok())
    }

    /// Retain one operation with payload `payload` for leader `id` before it starts. Until it is
    /// completed, a revoked acquisition stays draining and the entry is kept. `Overloaded` at
    /// `operations_per_flight`.
    pub fn retain_operation_with(
        &mut self,
        key: &K,
        id: u64,
        payload: impl FnOnce() -> std::result::Result<Q, E>,
        wakes: &mut Vec<T>,
    ) -> std::result::Result<Operation, E> {
        let (fence, cap) = (self.fence, self.limits.operations_per_flight);
        let op = self.next_operation;
        let entry = self.leader(key, id, wakes)?;
        if entry.operations.len() >= cap {
            return Err(Error::Overloaded.into());
        }
        entry.operations.insert(op, Slot::Retained(payload()?));
        let incarnation = entry.incarnation;
        self.next_operation += 1;
        Ok(Operation {
            fence,
            incarnation,
            id: op,
        })
    }

    /// Whether `operation` is still retained by the current incarnation of `key`.
    pub fn owns(&self, key: &K, operation: &Operation) -> bool {
        operation.fence == self.fence
            && self.entries.get(key).is_some_and(|e| {
                e.incarnation == operation.incarnation && e.operations.contains_key(&operation.id)
            })
    }

    /// First completion phase: take `operation`'s payload for destruction outside the table
    /// borrow. Its fence stays until [`Self::finish_operation`]. `Internal` when stale.
    pub fn take_operation(&mut self, key: &K, operation: &Operation) -> Result<Q> {
        if !self.owns(key, operation) {
            return Err(Error::Internal);
        }
        let entry = self.entries.get_mut(key).expect("owned");
        let slot = entry.operations.get_mut(&operation.id).expect("owned");
        match std::mem::replace(slot, Slot::Completing) {
            Slot::Retained(payload) => {
                entry.completing += 1;
                Ok(payload)
            }
            Slot::Completing => Err(Error::Internal),
        }
    }

    /// Second completion phase: clear the fence, settle, notify, and remove a quiescent entry,
    /// which is returned for dropping outside the table borrow.
    pub fn finish_operation(
        &mut self,
        key: &K,
        operation: Operation,
        wakes: &mut Vec<T>,
    ) -> Result<Option<Removed<C, P, O, R, T, W, X, Q>>> {
        if !self.owns(key, &operation) {
            return Err(Error::Internal);
        }
        let entry = self.entries.get_mut(key).expect("owned");
        if !matches!(entry.operations.get(&operation.id), Some(Slot::Completing)) {
            return Err(Error::Internal);
        }
        entry.operations.remove(&operation.id);
        entry.completing -= 1;
        let entry = self.refreshed(key, wakes).expect("present");
        entry.state.notify(wakes);
        self.notify_drain(wakes);
        Ok(self.remove_quiescent(key, wakes))
    }

    /// The retained operation finished: settle, notify, and remove a quiescent entry. Call it
    /// before `finish`. `Internal` for an operation of another table or entry incarnation.
    pub fn complete_operation(
        &mut self,
        key: &K,
        operation: Operation,
        wakes: &mut Vec<T>,
    ) -> Result<()> {
        drop(self.take_operation(key, &operation)?);
        drop(self.finish_operation(key, operation, wakes)?);
        Ok(())
    }

    /// Remove an entry with no waiters and no retained operations, waking one capacity waiter and
    /// a drain waiter once empty.
    pub fn remove_quiescent(
        &mut self,
        key: &K,
        wakes: &mut Vec<T>,
    ) -> Option<Removed<C, P, O, R, T, W, X, Q>> {
        if !self.entries.get(key).is_some_and(|e| e.quiescent()) {
            return None;
        }
        let entry = self.entries.remove(key).expect("quiescent");
        self.sweep.remove(&entry.sweep_id);
        if let Some(next) = self.capacity.front() {
            wakes.push(next.clone());
        }
        self.notify_drain(wakes);
        Some(entry)
    }

    /// Leader `id`'s driver was dropped without an outcome: revoke at once, never hand over.
    pub fn abandon(&mut self, key: &K, id: u64, wakes: &mut Vec<T>) {
        let split = self.split;
        if let Some(entry) = self.entries.get_mut(key)
            && entry.state.validate_leader(id, true).is_ok()
        {
            entry.state.revoke(Error::Canceled.into(), wakes);
            let idle = entry.idle();
            settle(&mut entry.state, idle, split, wakes);
        }
    }

    /// Cancel waiter `id`.
    pub fn cancel(&mut self, key: &K, id: u64, wakes: &mut Vec<T>) {
        self.cancel_with(key, id, Error::Canceled.into(), wakes);
    }

    /// Fail waiter `id` with `error` (its request scope ended).
    pub fn cancel_with(&mut self, key: &K, id: u64, error: E, wakes: &mut Vec<T>) {
        if let Some(entry) = self.entries.get_mut(key) {
            entry.state.cancel(id, error);
            self.refreshed(key, wakes);
        }
    }

    /// Detach waiter `id`, dropping its policy and removing a quiescent entry, which is returned.
    pub fn detach(
        &mut self,
        key: &K,
        id: u64,
        wakes: &mut Vec<T>,
    ) -> Option<Removed<C, P, O, R, T, W, X, Q>> {
        let entry = self.entries.get_mut(key)?;
        if let (Some(retired), Some(waiter)) = (&mut self.retired, entry.state.waiters.get_mut(&id))
        {
            retired.extend(waiter.waker.take());
        }
        entry.state.detach(id);
        self.refreshed(key, wakes);
        self.remove_quiescent(key, wakes)
    }

    /// Expire waiters whose deadline passed at `now` and settle outcomes.
    pub fn refresh(&mut self, key: &K, now: Instant, wakes: &mut Vec<T>) {
        self.now.set(now);
        self.refreshed(key, wakes);
    }

    /// Whether [`Self::stop`] ran. Callers reject new joins once stopping.
    pub fn is_stopping(&self) -> bool {
        self.stopping
    }

    /// Stop every flight with `error`: waiters fail, nothing is elected again, and outcomes
    /// settle once retained operations finish.
    pub fn stop(&mut self, wakes: &mut Vec<T>) {
        self.stopping = true;
        let split = self.split;
        for entry in self.entries.values_mut() {
            entry.state.stop(Error::Canceled.into(), wakes);
            let idle = entry.idle();
            settle(&mut entry.state, idle, split, wakes);
        }
    }

    /// Park the drain task's wake target, returning the one it replaces.
    pub fn register_drain(&mut self, target: T) -> Option<T> {
        self.drain.replace(target)
    }

    /// Wake the drain task once the table is empty.
    pub fn notify_drain(&mut self, wakes: &mut Vec<T>) {
        if let Some(target) = self.drain.take() {
            wakes.push(target);
        }
    }

    /// Refresh at most `budget` entries round-robin (one waiter sweep each) and remove quiescent
    /// ones, returned for dropping outside the table borrow.
    pub fn sweep(
        &mut self,
        budget: usize,
        wakes: &mut Vec<T>,
    ) -> Vec<Removed<C, P, O, R, T, W, X, Q>> {
        let mut removed = Vec::new();
        for _ in 0..budget.min(self.sweep.len()) {
            let next = self
                .sweep
                .range(self.cursor..)
                .next()
                .or_else(|| self.sweep.iter().next())
                .map(|(id, key)| (*id, key.clone()));
            let Some((id, key)) = next else {
                break;
            };
            self.cursor = id.wrapping_add(1);
            if let Some(entry) = self.entries.get_mut(&key) {
                entry.state.sweep_waiter(wakes);
            }
            self.refreshed(&key, wakes);
            removed.extend(self.remove_quiescent(&key, wakes));
        }
        if self.entries.is_empty() {
            self.notify_drain(wakes);
        }
        removed
    }
}

/// The default-waiter API: joins reserve their own charges and the table's clock decides
/// deadlines.
impl<K: Eq + Hash + Clone, C: Clone, P: Clone, O, R, S: BuildHasher, T: Clone + PartialEq, Q>
    FlightTable<K, C, P, O, R, S, T, Waiter<R>, (), Q>
{
    fn waiter(&self, deadline: Instant, charge: R) -> Waiter<R> {
        Waiter {
            deadline,
            now: self.now.clone(),
            _charge: charge,
        }
    }

    /// Acquisition join: the existing entry's gates, the budget gate, the waiter reservation, then
    /// for a new entry the entry cap and the entry reservation.
    #[allow(clippy::too_many_arguments)] // Mirrors production join inputs.
    pub fn join(
        &mut self,
        key: &K,
        now: Instant,
        deadline: Instant,
        attempts: u32,
        plaintext: bool,
        mut reserve: impl FnMut(Reserve) -> Result<R>,
        wakes: &mut Vec<T>,
    ) -> Result<Joined<C, P>> {
        self.now.set(now);
        if let Some(settled) = self.join_existing(key, plaintext, wakes)? {
            return Ok(Joined::Settled(settled));
        }
        let partial = self
            .entries
            .get(key)
            .is_some_and(|e| e.state.partial.is_some());
        join_budget_gate(now, deadline, attempts, partial)?;
        let charge = reserve(Reserve::Waiter)?;
        let waiter = self.waiter(deadline, charge);
        let id = self.next_waiter_id();
        if !self.entries.contains_key(key) {
            self.entry_capacity()?;
            let charge = reserve(Reserve::Flight)?;
            self.insert(key.clone(), (), charge);
        }
        self.register_as(key, id, waiter, true, plaintext);
        Ok(Joined::Waiter(id))
    }

    /// Copy-only join: never creates an entry, leads, or adopts.
    pub fn join_copy(
        &mut self,
        key: &K,
        now: Instant,
        deadline: Instant,
        reserve: impl FnOnce(Reserve) -> Result<R>,
        wakes: &mut Vec<T>,
    ) -> Result<Joined<C, P>> {
        self.now.set(now);
        match self.join_copy_existing(key, wakes)? {
            CopyJoin::Miss => return Ok(Joined::Miss),
            CopyJoin::Settled(settled) => return Ok(Joined::Settled(settled)),
            CopyJoin::Register => {}
        }
        let charge = reserve(Reserve::Waiter)?;
        let waiter = self.waiter(deadline, charge);
        let id = self.register(key, waiter, false, false).expect("present");
        Ok(Joined::Waiter(id))
    }

    /// Poll waiter `id` at `now` with its remaining `attempts`; parks `token` when nothing is
    /// ready. A canceled waiter fails `Canceled`; one past its deadline fails `DeadlineExceeded`.
    pub fn poll(
        &mut self,
        key: &K,
        id: u64,
        now: Instant,
        attempts: u32,
        token: T,
        wakes: &mut Vec<T>,
    ) -> Result<Wait<C, P>> {
        self.now.set(now);
        let split = self.split;
        let entry = self.entries.get_mut(key).ok_or(Error::Internal)?;
        let waiter = entry.state.waiters.get(&id).ok_or(Error::Internal)?;
        let (deadline, copy) = (waiter.deadline, !waiter.acquisition());
        if waiter.error == Some(Error::Canceled) {
            return Ok(Wait::Failed(Error::Canceled));
        }
        if now >= deadline {
            entry.state.cancel(id, Error::DeadlineExceeded);
            let idle = entry.idle();
            refresh(&mut entry.state, idle, || now, split, wakes);
            return Ok(Wait::Failed(Error::DeadlineExceeded));
        }
        let mut retired = self.retired.take();
        let park = |slot: &mut Option<T>| {
            if let (Some(old), Some(retired)) = (slot.replace(token), &mut retired) {
                retired.push(old);
            }
        };
        let result = if copy {
            self.copy_wait(key, id, wakes, park).map(|r| match r {
                None => Wait::Pending,
                Some(Ok(settled)) => Wait::Ready(settled),
                Some(Err(error)) => Wait::Failed(error),
            })
        } else {
            self.wait(key, id, deadline, attempts, wakes, park)
        };
        self.retired = retired;
        result
    }

    /// Retain one operation for leader `id` before it starts. Until [`Self::complete_operation`]
    /// runs, a revoked acquisition stays draining and the entry is kept. `Overloaded` at
    /// `operations_per_flight`.
    pub fn retain_operation(&mut self, key: &K, id: u64, wakes: &mut Vec<T>) -> Result<Operation>
    where
        Q: Default,
    {
        self.retain_operation_with(key, id, || Ok(Q::default()), wakes)
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;
    use std::time::Duration;

    type Table<K, V> = FlightTable<K, V, V, V, (), RandomState, Token>;

    fn complete<V>(v: V) -> Published<V, V> {
        Published::Complete(v)
    }

    struct Fx<K, V> {
        t: Table<K, V>,
        at: Instant,
        wakes: Vec<Token>,
    }

    fn fx<K: Eq + Hash + Clone, V: Clone>(limits: FlightLimits) -> Fx<K, V> {
        let at = Instant::now();
        Fx {
            t: FlightTable::with_fence(Fence(7), limits, complete, at, RandomState::new()),
            at,
            wakes: Vec::new(),
        }
    }

    impl<K: Eq + Hash + Clone, V: Clone> Fx<K, V> {
        fn join(&mut self, k: &K) -> Result<Joined<V, V>> {
            let deadline = self.at + Duration::from_secs(10);
            self.t
                .join(k, self.at, deadline, 2, true, |_| Ok(()), &mut self.wakes)
        }
        fn waiter(&mut self, k: &K) -> u64 {
            match self.join(k) {
                Ok(Joined::Waiter(id)) => id,
                _ => panic!("expected waiter"),
            }
        }
        fn poll(&mut self, k: &K, id: u64) -> Result<Wait<V, V>> {
            self.t.poll(k, id, self.at, 2, id, &mut self.wakes)
        }
    }

    fn limits() -> FlightLimits {
        FlightLimits {
            entries: 2,
            waiters_per_flight: 8,
            operations_per_flight: 2,
            generations_per_flight: 4,
        }
    }

    #[test]
    fn single_leader_per_key_and_no_lost_waiters() {
        let mut f = fx::<u32, u32>(limits());
        let ids: Vec<u64> = (0..5).map(|_| f.waiter(&1)).collect();
        let other = f.waiter(&2);
        let mut leaders = Vec::new();
        for &id in &ids {
            if f.poll(&1, id) == Ok(Wait::Lead) {
                leaders.push(id);
            }
        }
        assert_eq!(leaders, vec![ids[0]]);
        // A distinct key elects independently.
        assert_eq!(f.poll(&2, other), Ok(Wait::Lead));
        f.wakes.clear();
        f.t.finish(&1, ids[0], Ok(9), &mut f.wakes).unwrap();
        // Every parked waiter is woken exactly once and sees the result.
        let mut woken = f.wakes.clone();
        woken.sort_unstable();
        assert_eq!(woken, ids[1..].to_vec());
        for &id in &ids {
            assert_eq!(f.poll(&1, id), Ok(Wait::Ready(Settled::Complete(9))));
        }
        for &id in &ids {
            f.t.detach(&1, id, &mut f.wakes);
        }
        assert!(!f.t.contains(&1));
        assert!(f.t.contains(&2));
    }

    #[test]
    fn handover_keeps_one_running_acquisition() {
        let mut f = fx::<u32, u32>(limits());
        let (a, b) = (f.waiter(&1), f.waiter(&1));
        assert_eq!(f.poll(&1, a), Ok(Wait::Lead));
        f.t.allow_handover(&1, a, &mut f.wakes).unwrap();
        let op = f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
        f.t.cancel(&1, a, &mut f.wakes);
        // The running acquisition passed to `b`, which never runs a second one.
        assert!(f.t.handed_over(&1));
        assert!(f.t.acquiring(&1));
        assert_eq!(f.poll(&1, b), Ok(Wait::Pending));
        f.t.complete_operation(&1, op, &mut f.wakes).unwrap();
        assert!(f.t.is_leader(&1, b));
        f.t.finish(&1, b, Ok(3), &mut f.wakes).unwrap();
        assert_eq!(f.poll(&1, b), Ok(Wait::Ready(Settled::Complete(3))));
    }

    #[test]
    fn quiescent_removal_waits_for_operations() {
        let mut f = fx::<u32, u32>(limits());
        let a = f.waiter(&1);
        assert_eq!(f.poll(&1, a), Ok(Wait::Lead));
        let op = f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
        f.t.detach(&1, a, &mut f.wakes);
        // No waiters, but the operation still owns resources.
        assert!(f.t.contains(&1));
        assert_eq!(f.t.operations(&1), 1);
        f.t.complete_operation(&1, op, &mut f.wakes).unwrap();
        assert!(!f.t.contains(&1));
        // Completing twice is stale.
        assert_eq!(
            f.t.complete_operation(&1, op, &mut f.wakes),
            Err(Error::Internal)
        );
    }

    #[test]
    fn retained_operations_are_capped() {
        let mut f = fx::<u32, u32>(limits());
        let a = f.waiter(&1);
        assert_eq!(f.poll(&1, a), Ok(Wait::Lead));
        let first = f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
        f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
        assert_eq!(
            f.t.retain_operation(&1, a, &mut f.wakes),
            Err(Error::Overloaded)
        );
        f.t.complete_operation(&1, first, &mut f.wakes).unwrap();
        f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
    }

    #[test]
    fn owner_fence_rejects_foreign_and_stale_capabilities() {
        let mut f = fx::<u32, u32>(limits());
        let mut g = fx::<u32, u32>(limits());
        g.t = FlightTable::with_fence(Fence(8), limits(), complete, g.at, RandomState::new());
        let a = f.waiter(&1);
        let b = g.waiter(&1);
        assert_eq!((a, b), (0, 0));
        assert_eq!(f.poll(&1, a), Ok(Wait::Lead));
        assert_eq!(g.poll(&1, b), Ok(Wait::Lead));
        let ticket = f.t.ticket(&1, a).unwrap();
        assert_eq!(ticket.fence, Fence(7));
        f.t.validate(&1, &ticket).unwrap();
        // Same key, waiter, incarnation, and generation in another table: rejected by fence.
        assert_eq!(g.t.validate(&1, &ticket), Err(Error::Internal));
        let op = g.t.retain_operation(&1, b, &mut g.wakes).unwrap();
        assert_eq!(
            f.t.complete_operation(&1, op, &mut f.wakes),
            Err(Error::Internal)
        );
        // A recreated entry has a new incarnation: the old ticket and operation are stale.
        let op = f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
        f.t.finish(&1, a, Ok(1), &mut f.wakes).unwrap();
        f.t.detach(&1, a, &mut f.wakes);
        let stale = Operation { id: 99, ..op };
        f.t.complete_operation(&1, op, &mut f.wakes).unwrap();
        assert!(!f.t.contains(&1));
        let c = f.waiter(&1);
        assert_eq!(f.poll(&1, c), Ok(Wait::Lead));
        assert_eq!(f.t.validate(&1, &ticket), Err(Error::Internal));
        assert_ne!(f.t.ticket(&1, c).unwrap().incarnation, ticket.incarnation);
        assert_eq!(
            f.t.complete_operation(&1, stale, &mut f.wakes),
            Err(Error::Internal)
        );
    }

    #[test]
    fn entry_capacity_waiters_are_fifo() {
        let mut f = fx::<u32, u32>(limits());
        let (a, b) = (f.waiter(&1), f.waiter(&2));
        assert_eq!(f.join(&3), Err(Error::Overloaded));
        assert!(!f.t.wait_entry(100));
        assert!(!f.t.wait_entry(101));
        // A later caller cannot jump the queue even when polled again.
        assert!(!f.t.wait_entry(101));
        f.wakes.clear();
        f.t.detach(&1, a, &mut f.wakes);
        assert_eq!(f.wakes, vec![100]);
        assert!(!f.t.wait_entry(101));
        assert!(f.t.wait_entry(100));
        f.waiter(&3);
        // The withdrawn front passes nothing on while the table is full.
        f.wakes.clear();
        f.t.cancel_entry_wait(&101, &mut f.wakes);
        assert!(f.wakes.is_empty());
        f.t.detach(&2, b, &mut f.wakes);
        assert!(f.wakes.is_empty());
        assert!(f.t.wait_entry(102));
    }

    #[test]
    fn canceled_front_passes_capacity_on() {
        let mut f = fx::<u32, u32>(limits());
        let (a, _) = (f.waiter(&1), f.waiter(&2));
        assert!(!f.t.wait_entry(100));
        assert!(!f.t.wait_entry(101));
        f.t.detach(&1, a, &mut f.wakes);
        f.wakes.clear();
        f.t.cancel_entry_wait(&100, &mut f.wakes);
        assert_eq!(f.wakes, vec![101]);
        assert!(f.t.wait_entry(101));
    }

    /// Metadata-cohort-shaped use (see `read::cohort` for the real one): keyed by object, the first caller leads, every waiter
    /// observes the one published metadata, and a failed leader fails the cohort.
    #[test]
    fn metadata_cohort_instance() {
        #[derive(Clone, Debug, Eq, PartialEq)]
        struct Meta {
            length: u64,
        }
        let mut f = fx::<String, Meta>(limits());
        let key = "object".to_owned();
        let ids: Vec<u64> = (0..3).map(|_| f.waiter(&key)).collect();
        assert_eq!(f.poll(&key, ids[0]), Ok(Wait::Lead));
        assert_eq!(f.poll(&key, ids[1]), Ok(Wait::Pending));
        let meta = Meta { length: 42 };
        f.t.finish(&key, ids[0], Ok(meta.clone()), &mut f.wakes)
            .unwrap();
        for &id in &ids {
            assert_eq!(
                f.poll(&key, id),
                Ok(Wait::Ready(Settled::Complete(meta.clone())))
            );
        }
        // A later caller joins the settled result without waiting.
        assert_eq!(f.join(&key), Ok(Joined::Settled(Settled::Complete(meta))));
        let other = "other".to_owned();
        let (a, b) = (f.waiter(&other), f.waiter(&other));
        assert_eq!(f.poll(&other, a), Ok(Wait::Lead));
        f.t.finish(
            &other,
            a,
            Err(FailureKind::Terminal(Error::NotFound)),
            &mut f.wakes,
        )
        .unwrap();
        assert_eq!(f.poll(&other, b), Ok(Wait::Failed(Error::NotFound)));
    }

    /// Local-copy sharing (`coalesce::shared::Table`): copy joins never lead and miss when no
    /// acquisition runs; they share the result the completion owner publishes.
    #[test]
    fn shared_copy_instance() {
        let mut f = fx::<u64, Rc<[u8]>>(limits());
        let deadline = f.at + Duration::from_secs(1);
        let copy =
            |f: &mut Fx<u64, Rc<[u8]>>| f.t.join_copy(&5, f.at, deadline, |_| Ok(()), &mut f.wakes);
        assert_eq!(copy(&mut f), Ok(Joined::Miss));
        let owner = f.waiter(&5);
        assert_eq!(f.poll(&5, owner), Ok(Wait::Lead));
        let Ok(Joined::Waiter(reader)) = copy(&mut f) else {
            panic!("copy waiter");
        };
        assert_eq!(f.poll(&5, reader), Ok(Wait::Pending));
        let bytes: Rc<[u8]> = Rc::from(&b"page"[..]);
        f.wakes.clear();
        f.t.finish(&5, owner, Ok(bytes.clone()), &mut f.wakes)
            .unwrap();
        assert_eq!(f.wakes, vec![reader]);
        let Ok(Wait::Ready(Settled::Complete(shared))) = f.poll(&5, reader) else {
            panic!("shared result");
        };
        assert!(Rc::ptr_eq(&shared, &bytes));
    }

    #[test]
    fn two_phase_completion_keeps_fence_and_returns_removed_entry() {
        let at = Instant::now();
        let mut t: FlightTable<
            u32,
            u32,
            u32,
            u32,
            (),
            RandomState,
            Token,
            Waiter<()>,
            &str,
            String,
        > = FlightTable::with_fence(Fence(1), limits(), complete, at, RandomState::new());
        let mut wakes = Vec::new();
        assert_eq!(t.insert(1, "ext", ()), Some(0));
        assert_eq!(t.insert(1, "dup", ()), None);
        let w = Waiter {
            deadline: at + Duration::from_secs(1),
            now: Rc::new(Cell::new(at)),
            _charge: (),
        };
        let a = t.register(&1, w, true, true).unwrap();
        assert_eq!(
            t.wait(&1, a, at + Duration::from_secs(1), 1, &mut wakes, |_| {}),
            Ok(Wait::Lead)
        );
        let op = t
            .retain_operation_with(&1, a, || Ok("payload".to_owned()), &mut wakes)
            .unwrap();
        assert!(t.detach(&1, a, &mut wakes).is_none());
        assert_eq!(t.take_operation(&1, &op).as_deref(), Ok("payload"));
        // The fence holds while the payload is destroyed.
        assert_eq!(t.entry(&1).unwrap().completing(), 1);
        assert_eq!(t.take_operation(&1, &op), Err(Error::Internal));
        assert!(t.contains(&1));
        let removed = t.finish_operation(&1, op, &mut wakes).unwrap().unwrap();
        assert_eq!(*removed.ext(), "ext");
        assert!(t.is_empty());
    }

    #[test]
    fn stop_fails_waiters_and_sweep_drains() {
        let mut f = fx::<u32, u32>(limits());
        let (a, b) = (f.waiter(&1), f.waiter(&2));
        assert_eq!(f.poll(&1, a), Ok(Wait::Lead));
        let op = f.t.retain_operation(&1, a, &mut f.wakes).unwrap();
        assert_eq!(f.t.sample::<3>().iter().filter(|e| e.is_some()).count(), 2);
        assert_eq!(f.t.next_deadline(), Some(f.at + Duration::from_secs(10)));
        f.t.stop(&mut f.wakes);
        assert!(f.t.is_stopping());
        // Stopping detaches every waiter; the sweep removes the idle entry only.
        assert!(!f.t.attached(&1, a) && !f.t.attached(&2, b));
        assert_eq!(f.t.register_drain(99), None);
        assert_eq!(f.t.sweep(8, &mut f.wakes).len(), 1);
        assert_eq!(f.t.len(), 1);
        // Each removal wakes the drain task, which parks again while entries remain.
        assert!(f.wakes.contains(&99));
        f.wakes.clear();
        f.t.register_drain(99);
        f.t.complete_operation(&1, op, &mut f.wakes).unwrap();
        assert!(f.t.is_empty());
        assert!(f.wakes.contains(&99));
    }
}
