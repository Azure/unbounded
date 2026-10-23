//! Token-based subscription scheduler without locks, wakers, or clocks.
//!
//! [`Scheduler`] is the node-wide bounded provider selection that the
//! dataplane wraps in `src/peer/subscriptions.rs` (a mutex, `Arc` handles and
//! `Waker`s). Every transition is an explicit method call. Methods that take
//! `now_ms` first expire deadlines at that time; the others do not read time.
//! A transition may make parked response slots ready; [`Scheduler::take_wakes`]
//! returns their tokens, and the caller notifies them after the transition.
//!
//! The handle `Drop` logic of the dataplane maps to explicit transitions:
//! dropping unfinished work is [`Scheduler::cancel_work`], dropping a waiter
//! is [`Scheduler::cancel_waiter`], and dropping an admitted request that did
//! not select is [`Scheduler::release`].

use flow_control::subscriptions::{Fanout, Interval, include as include_page};
use std::collections::HashMap;
use std::hash::Hash;

/// Largest number of selections one subscription may request in flight. The
/// replay window is a 64-bit map, so no contract may grant more than this.
pub const MAX_SUBSCRIPTION_FLIGHTS: u16 = 64;

/// Bytes the requester debits per selection when it issues a request.
const RESERVE: u64 = racer_object_wire::PAGE_BYTES + 16;

/// Retained spent contracts outlive their deadline by the signature freshness
/// window, so replaying one cannot renew it.
const RETAIN_MS: u64 = 60_000;

/// Contracts are pruned at most this often.
const PRUNE_INTERVAL_MS: u64 = 100;

#[derive(Clone, Copy, Debug)]
pub struct SubscriptionLimits {
    pub max_entries: usize,

    /// Node-wide total, not a per-subscription allocation.
    pub max_completed_intervals: usize,

    pub max_inflight: usize,

    /// Includes unconsumed completed/error responses, not just active waiters.
    pub max_pending: usize,

    /// Most selections one subscription may have pending at once. A request
    /// asking for more is granted this many. Bounded by `max_inflight`,
    /// `max_pending` and [`MAX_SUBSCRIPTION_FLIGHTS`].
    pub max_flights_per_subscription: usize,
}

impl Default for SubscriptionLimits {
    fn default() -> Self {
        Self {
            max_entries: 1024,
            max_completed_intervals: 4096,
            max_inflight: 64,
            max_pending: 256,
            max_flights_per_subscription: 8,
        }
    }
}

impl SubscriptionLimits {
    /// Every limit is nonzero and the per-subscription flights fit the others.
    pub fn valid(&self) -> bool {
        self.max_entries != 0
            && self.max_completed_intervals != 0
            && self.max_inflight != 0
            && self.max_pending != 0
            && self.max_flights_per_subscription != 0
            && self.max_flights_per_subscription <= self.max_inflight
            && self.max_flights_per_subscription <= self.max_pending
            && self.max_flights_per_subscription <= usize::from(MAX_SUBSCRIPTION_FLIGHTS)
    }
}

/// Failures the scheduler itself produces.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    DeadlineExceeded,
    Cancelled,
    Overloaded,
    Unavailable,
    StaleFlight,
    Replay,
    InvalidRequest,
}

/// Error type the scheduler stores in response slots. Work failures supplied
/// by the caller pass through unchanged.
pub trait Fault: Clone {
    fn of(kind: Kind) -> Self;

    /// An origin rejection of the supplier's credentials: elect exactly one
    /// other retained follower to retry with its own credentials.
    fn promotes(&self) -> bool;
}

/// One request on a contract. `demand` must be canonical.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Contract<V> {
    pub id: [u8; 16],

    pub version: V,

    pub demand: Vec<Interval>,

    pub sequence: u64,

    pub page_budget: u32,

    pub byte_budget: u64,

    pub flights: Option<u16>,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct EntryKey<M, R> {
    pub membership: M,

    pub receiver: R,

    pub id: [u8; 16],
}

/// An admitted request: its contract and sequence.
pub type Admitted<M, R> = (EntryKey<M, R>, u64);

/// Grant reported relative to the selecting request.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Grant<M, R, V> {
    pub subscription_id: [u8; 16],

    pub sequence: u64,

    pub version: V,

    pub page: u64,

    pub membership: M,

    pub receiver: R,

    pub deadline: u64,

    pub remaining_page_budget: u32,

    pub remaining_byte_budget: u64,

    pub flights: Option<u16>,
}

/// A completed response: the shared payload and this receiver's grant.
#[derive(Clone, Debug)]
pub struct Delivery<P, M, R, V> {
    pub payload: P,

    pub grant: Grant<M, R, V>,
}

/// A committed selection. `work` is the acquisition owner token when this
/// request leads the flight; followers only wait.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Selected<V> {
    pub waiter: u64,

    pub work: Option<u64>,

    pub version: V,

    pub page: u64,
}

/// Result of polling a response slot.
#[derive(Debug)]
pub enum Polled<T> {
    Ready(T),
    /// The slot is parked; its token is returned by `take_wakes` once ready.
    Pending,
}

#[derive(Clone)]
struct Reservation {
    sequence: u64,
    demand: Vec<Interval>,
    page_budget: u32,
    byte_budget: u64,
    deadline: u64,
    flights: Option<u16>,
    covered_page: bool,
    covered_bytes: u64,
}

struct Flight {
    token: u64,
    page: u64,
    reservation: Reservation,
    settled: bool,
}

struct Entry<V> {
    subscription: Contract<V>,
    deadline: u64,
    completed: Vec<Interval>,
    flights: usize,
    seen: u64,
    admitted: Vec<Reservation>,
    pending: Vec<Flight>,
    pledges: Vec<(u64, bool, u64)>,
}

/// Read-only view of a contract, for tests and the simulator.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct EntryView {
    pub page_budget: u32,
    pub byte_budget: u64,
    pub flights: usize,
    pub pending: usize,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct FlightKey<M, V> {
    membership: M,
    version: V,
    page: u64,
}

type Slots<M, R, V, P, E> = Fanout<FlightKey<M, V>, EntryKey<M, R>, Delivery<P, M, R, V>, E, u64>;

type Slot<M, R, V, P, E> =
    flow_control::subscriptions::Pending<EntryKey<M, R>, Delivery<P, M, R, V>, E, u64>;

/// Sans-IO subscription scheduler. See the module documentation.
pub struct Scheduler<M, R, V, P, E> {
    limits: SubscriptionLimits,
    now: u64,
    next_prune: u64,
    entries: HashMap<EntryKey<M, R>, Entry<V>>,
    fanout: Slots<M, R, V, P, E>,
    completed_intervals: usize,
    wake: Vec<u64>,
}

impl<V> Entry<V> {
    fn reservation(subscription: &Contract<V>, deadline: u64) -> Reservation {
        Reservation {
            sequence: subscription.sequence,
            demand: subscription.demand.clone(),
            page_budget: subscription.page_budget,
            byte_budget: subscription.byte_budget,
            deadline,
            flights: subscription.flights,
            covered_page: false,
            covered_bytes: 0,
        }
    }

    /// Forfeit what newer ceilings granted on account of a request that will
    /// not consume it.
    fn forfeit(&mut self, page: bool, bytes: u64) {
        if page {
            self.subscription.page_budget = self.subscription.page_budget.saturating_sub(1);
        }
        self.subscription.byte_budget = self.subscription.byte_budget.saturating_sub(bytes);
    }

    /// Apply a newer request's ceilings. The requester debits a full page and
    /// `PAGE_BYTES + 16` bytes per selection when issuing it, so the newer
    /// ceilings already exclude every earlier outstanding request. Widen them
    /// by at most those reservations, never above the current budgets, and
    /// record the widening on the outstanding requests. Each must charge or
    /// forfeit its share, so the contract minus every recorded widening never
    /// exceeds the newest ceiling.
    fn apply_ceiling(&mut self, sequence: u64, page_budget: u32, byte_budget: u64) {
        let older = |r: &&mut Reservation| r.sequence != sequence;
        // Lower sequences in the window that have not arrived. Must run after
        // the replay window has been shifted to `sequence`.
        let unseen: Vec<u64> = (1..self.flights.min(64) as u64)
            .filter(|&offset| offset <= sequence && self.seen & (1 << offset) == 0)
            .map(|offset| sequence - offset)
            .collect();
        let pages = self.admitted.iter_mut().filter(older).count() + unseen.len();
        let flights = pages + self.pending.iter().filter(|f| !f.settled).count();
        self.subscription.page_budget = self
            .subscription
            .page_budget
            .min(page_budget.saturating_add(u32::try_from(pages).unwrap_or(u32::MAX)));
        self.subscription.byte_budget = self
            .subscription
            .byte_budget
            .min(byte_budget.saturating_add(RESERVE.saturating_mul(flights as u64)));
        let mut pages = self.subscription.page_budget.saturating_sub(page_budget);
        let mut bytes = self.subscription.byte_budget.saturating_sub(byte_budget);
        let mut cover = |covered: &mut u64| {
            *covered = bytes.min(RESERVE);
            bytes -= *covered;
        };
        for r in self.admitted.iter_mut().filter(older) {
            r.covered_page = pages > 0;
            pages = pages.saturating_sub(1);
            cover(&mut r.covered_bytes);
        }
        for flight in self.pending.iter_mut().filter(|f| !f.settled) {
            cover(&mut flight.reservation.covered_bytes);
        }
        self.pledges = unseen
            .into_iter()
            .map(|s| {
                let page = pages > 0;
                pages = pages.saturating_sub(1);
                let mut covered = 0;
                cover(&mut covered);
                (s, page, covered)
            })
            .collect();
    }

    /// Hand a late request the allowance pledged for its sequence, if any.
    fn take_pledge(&mut self, reservation: &mut Reservation) {
        if let Some(index) = self
            .pledges
            .iter()
            .position(|(s, ..)| *s == reservation.sequence)
        {
            let (_, page, bytes) = self.pledges.swap_remove(index);
            reservation.covered_page = page;
            reservation.covered_bytes = bytes;
        }
    }

    /// Take a flight slot for `reservation`, superseding the oldest admitted
    /// request if every slot is taken. A request older than every admitted
    /// one would be superseded at once, so it is a replay.
    fn reserve(&mut self, reservation: Reservation) -> Result<(), Kind> {
        if self.pending.len() >= self.flights {
            return Err(Kind::Overloaded);
        }
        if self.admitted.len() + self.pending.len() >= self.flights {
            let (index, oldest) = self
                .admitted
                .iter()
                .enumerate()
                .min_by_key(|(_, r)| r.sequence)
                .expect("a free flight slot is admitted");
            if oldest.sequence > reservation.sequence {
                return Err(Kind::Replay);
            }
            let oldest = self.admitted.swap_remove(index);
            self.forfeit(oldest.covered_page, oldest.covered_bytes);
        }
        self.admitted.push(reservation);
        Ok(())
    }
}

impl<M, R, V, P, E> Scheduler<M, R, V, P, E>
where
    M: Copy + Eq + Hash,
    R: Clone + Eq + Hash,
    V: Clone + Eq + Hash,
    P: Clone,
    E: Fault,
{
    /// `None` when the limits are invalid (see [`SubscriptionLimits::valid`]).
    pub fn new(limits: SubscriptionLimits) -> Option<Self> {
        limits.valid().then(|| Self {
            limits,
            now: 0,
            next_prune: 0,
            entries: HashMap::new(),
            fanout: Fanout::default(),
            completed_intervals: 0,
            wake: Vec::new(),
        })
    }

    pub fn limits(&self) -> &SubscriptionLimits {
        &self.limits
    }

    /// Tokens of parked response slots made ready since the last call.
    pub fn take_wakes(&mut self) -> Vec<u64> {
        std::mem::take(&mut self.wake)
    }

    /// Advance time: expire response slots and prune spent contracts. Time
    /// never moves backward.
    pub fn advance(&mut self, now_ms: u64) {
        self.now = self.now.max(now_ms);
        self.fanout
            .expire(self.now, || E::of(Kind::DeadlineExceeded), &mut self.wake);
        if self.now < self.next_prune {
            return;
        }
        self.next_prune = self.now.saturating_add(PRUNE_INTERVAL_MS);
        let now = self.now;
        let completed = &mut self.completed_intervals;
        self.entries.retain(|_, entry| {
            if entry.deadline.saturating_add(RETAIN_MS) > now {
                true
            } else {
                *completed -= entry.completed.len();
                false
            }
        });
        // Never release acquisition capacity while the owner can still run Fill.
    }

    /// Admit one request on its contract. The caller has validated the fields
    /// (nonzero membership, bounded receiver and cache names, flights in
    /// `1..=MAX_SUBSCRIPTION_FLIGHTS`). Sequences are strictly increasing per
    /// contract, but with `flights > 1` requests may arrive out of order: a
    /// sequence below the highest admitted one is accepted once if it is
    /// within the granted flight window. Only the highest sequence replaces
    /// demand, budgets and deadline.
    pub fn admit(
        &mut self,
        subscription: Contract<V>,
        membership: M,
        receiver: R,
        deadline: u64,
        now_ms: u64,
    ) -> Result<Admitted<M, R>, E> {
        self.advance(now_ms);
        self.admit_at(subscription, membership, receiver, deadline)
            .map_err(E::of)
    }

    fn admit_at(
        &mut self,
        subscription: Contract<V>,
        membership: M,
        receiver: R,
        deadline: u64,
    ) -> Result<Admitted<M, R>, Kind> {
        let granted = usize::from(subscription.flights.unwrap_or(1))
            .min(self.limits.max_flights_per_subscription);
        if deadline <= self.now {
            return Err(Kind::DeadlineExceeded);
        }
        let key = EntryKey {
            membership,
            receiver: receiver.clone(),
            id: subscription.id,
        };
        let sequence = subscription.sequence;
        if let Some(entry) = self.entries.get_mut(&key) {
            let latest = entry.subscription.sequence;
            if sequence <= latest {
                // Within the window and never admitted before.
                let offset = latest - sequence;
                if offset >= entry.flights as u64 || entry.seen & (1 << offset) != 0 {
                    return Err(Kind::Replay);
                }
                if entry.subscription.version != subscription.version {
                    return Err(Kind::InvalidRequest);
                }
                // The allowance follows the newest request only. The late
                // request's own grant is still capped at what it asked for.
                entry.reserve(Entry::reservation(&subscription, deadline))?;
                // Fund the late request from the allowance a newer ceiling
                // excluded for it. The replay bit below makes this once.
                let mut reservation = entry.admitted.pop().expect("just reserved");
                entry.take_pledge(&mut reservation);
                entry.admitted.push(reservation);
                entry.seen |= 1 << offset;
                return Ok((key, sequence));
            }
            // The single-flight contract refuses any attempt to raise its
            // deadline or budgets. A multi-flight contract clamps instead;
            // neither can ever increase.
            let strict = entry.flights == 1;
            if entry.subscription.version != subscription.version
                || (strict
                    && (deadline > entry.deadline
                        || subscription.page_budget > entry.subscription.page_budget
                        || subscription.byte_budget > entry.subscription.byte_budget))
            {
                return Err(Kind::InvalidRequest);
            }
            let flights = entry.flights.min(granted);
            if entry.pending.len() >= flights {
                return Err(Kind::Overloaded);
            }
            entry.flights = flights;
            entry.reserve(Entry::reservation(&subscription, deadline))?;
            let shift = sequence - latest;
            entry.seen = if shift >= 64 { 0 } else { entry.seen << shift } | 1;
            entry.deadline = entry.deadline.min(deadline);
            entry.apply_ceiling(sequence, subscription.page_budget, subscription.byte_budget);
            let (page_budget, byte_budget) = (
                entry.subscription.page_budget,
                entry.subscription.byte_budget,
            );
            entry.subscription = subscription;
            entry.subscription.page_budget = page_budget;
            entry.subscription.byte_budget = byte_budget;
        } else {
            // A node aggregates all local readers under one logical contract.
            // Changing IDs cannot obtain duplicate transfers or extra votes.
            if self.entries.iter().any(|(other, entry)| {
                other.membership == membership
                    && other.receiver == receiver
                    && entry.subscription.version == subscription.version
                    && entry.deadline > self.now
            }) {
                return Err(Kind::Overloaded);
            }
            if self.entries.len() >= self.limits.max_entries {
                return Err(Kind::Overloaded);
            }
            let reservation = Entry::reservation(&subscription, deadline);
            self.entries.insert(
                key.clone(),
                Entry {
                    subscription,
                    deadline,
                    completed: Vec::new(),
                    flights: granted,
                    seen: 1,
                    admitted: vec![reservation],
                    pending: Vec::new(),
                    pledges: Vec::new(),
                },
            );
        }
        Ok((key, sequence))
    }

    /// Drop an admitted request that will not select. Its sequence stays in
    /// the replay window. A no-op after it selected.
    pub fn release(&mut self, (key, sequence): &Admitted<M, R>) {
        if let Some(entry) = self.entries.get_mut(key)
            && let Some(index) = entry.admitted.iter().position(|r| r.sequence == *sequence)
        {
            let r = entry.admitted.swap_remove(index);
            entry.forfeit(r.covered_page, r.covered_bytes);
        }
    }

    /// Select one page for an admitted request. `eligible(page)` says whether
    /// this node should supply `page`; an unknown endpoint (`None`) is returned
    /// as `Ok(Err(page))` without reserving a flight, so the caller can rank it
    /// and repeat.
    pub fn select(
        &mut self,
        admitted: &Admitted<M, R>,
        now_ms: u64,
        eligible: impl Fn(u64) -> Option<bool>,
    ) -> Result<Result<Selected<V>, u64>, E> {
        self.advance(now_ms);
        self.select_at(admitted, eligible).map_err(E::of)
    }

    fn select_at(
        &mut self,
        (key, sequence): &Admitted<M, R>,
        eligible: impl Fn(u64) -> Option<bool>,
    ) -> Result<Result<Selected<V>, u64>, Kind> {
        let entry = self.entries.get(key).ok_or(Kind::DeadlineExceeded)?;
        if entry.deadline <= self.now {
            return Err(Kind::DeadlineExceeded);
        }
        if entry.pending.len() >= entry.flights {
            return Err(Kind::Overloaded);
        }
        let reservation = entry
            .admitted
            .iter()
            .find(|r| r.sequence == *sequence)
            .ok_or(Kind::Replay)?
            .clone();
        if entry.subscription.page_budget == 0
            || entry.subscription.byte_budget == 0
            || reservation.page_budget == 0
            || reservation.byte_budget == 0
        {
            return Err(Kind::Unavailable);
        }
        if self.fanout.pending().len() >= self.limits.max_pending {
            return Err(Kind::Overloaded);
        }
        let number = match self.vote(key, &reservation.demand, &eligible) {
            Ok(number) => number.ok_or(Kind::Unavailable)?,
            Err(page) => return Ok(Err(page)),
        };
        let deadline = entry.deadline.min(reservation.deadline);
        let version = entry.subscription.version.clone();
        let flight_key = FlightKey {
            membership: key.membership,
            version: version.clone(),
            page: number,
        };
        let (token, flight, leader) = self
            .fanout
            .attach(
                flight_key,
                key.clone(),
                deadline,
                self.now,
                self.limits.max_pending,
                self.limits.max_inflight,
            )
            .ok_or(Kind::Overloaded)?;
        let entry = self.entries.get_mut(key).expect("admitted entry");
        entry.subscription.page_budget -= 1;
        entry.admitted.retain(|r| r.sequence != *sequence);
        // The page debit above consumes any page a newer ceiling granted.
        let mut reservation = reservation;
        reservation.covered_page = false;
        entry.pending.push(Flight {
            token,
            page: number,
            reservation,
            settled: false,
        });
        Ok(Ok(Selected {
            waiter: token,
            work: leader.then_some(flight),
            version,
            page: number,
        }))
    }

    /// The target votes with the selecting request's own demand, excluding
    /// pages it already has in flight. Other receivers vote with their latest
    /// demand.
    fn vote(
        &self,
        target: &EntryKey<M, R>,
        demand: &[Interval],
        eligible: &impl Fn(u64) -> Option<bool>,
    ) -> Result<Option<u64>, u64> {
        let version = &self.entries[target].subscription.version;
        // Sweep endpoints, not pages. Count distinct receivers, not subscription
        // IDs, so one receiver cannot boost its priority with duplicate IDs.
        let demands = self.entries.iter().filter_map(|(key, entry)| {
            if key.membership != target.membership
                || &entry.subscription.version != version
                || entry.deadline <= self.now
                || entry.subscription.byte_budget == 0
                || (entry.subscription.page_budget == 0 && entry.pending.is_empty())
            {
                return None;
            }
            if key != target {
                return Some((
                    &key.receiver,
                    false,
                    flow_control::subscriptions::outstanding(
                        &entry.subscription.demand,
                        &entry.completed,
                    ),
                ));
            }
            let mut done = entry.completed.clone();
            for flight in &entry.pending {
                done = include_page(&done, flight.page);
            }
            Some((
                &key.receiver,
                true,
                flow_control::subscriptions::outstanding(demand, &done),
            ))
        });
        flow_control::subscriptions::select(demands, eligible)
    }

    fn remove_pending(&mut self, token: u64) -> Option<Slot<M, R, V, P, E>> {
        let pending = self.fanout.remove(token)?;
        if let Some(entry) = self.entries.get_mut(&pending.owner)
            && let Some(index) = entry.pending.iter().position(|f| f.token == token)
        {
            let flight = entry.pending.swap_remove(index);
            if !flight.settled {
                entry.forfeit(false, flight.reservation.covered_bytes);
            }
        }
        Some(pending)
    }

    /// Finish acquisition `work` with the payload and its byte length, or a
    /// failure. Every waiter on the flight receives its own grant or error. A
    /// credential failure ([`Fault::promotes`]) elects one retained follower
    /// instead. `now_ms` expires deadlines first when present.
    pub fn complete(&mut self, work: u64, result: Result<(P, u64), E>, now_ms: Option<u64>) {
        if let Some(now) = now_ms {
            self.advance(now);
        }
        self.finish(work, result);
    }

    /// Abandon acquisition `work` (the dataplane drops `Work` unfinished).
    pub fn cancel_work(&mut self, work: u64) {
        self.finish(work, Err(E::of(Kind::Cancelled)));
    }

    fn finish(&mut self, flight: u64, result: Result<(P, u64), E>) {
        // An origin rejection says nothing about another credential supplier.
        // Elect exactly one retained follower to retry with its own verified
        // request and original route credits. Never clone or refund authority.
        let promote = result.as_ref().is_err_and(E::promotes);
        let limit = self.limits.max_completed_intervals;
        let entries = &mut self.entries;
        let completed_intervals = &mut self.completed_intervals;
        self.fanout.finish(
            flight,
            promote,
            |token, owner, key| {
                let (payload, bytes) = result.as_ref().map_err(Clone::clone)?;
                let entry = entries
                    .get_mut(owner)
                    .ok_or(E::of(Kind::DeadlineExceeded))?;
                let reservation = &entry
                    .pending
                    .iter()
                    .find(|flight| flight.token == token)
                    .ok_or(E::of(Kind::StaleFlight))?
                    .reservation;
                // Charge at least what newer ceilings set aside for this flight.
                let contract = entry
                    .subscription
                    .byte_budget
                    .checked_sub((*bytes).max(reservation.covered_bytes))
                    .ok_or(E::of(Kind::Unavailable))?;
                let remaining = reservation
                    .byte_budget
                    .checked_sub(*bytes)
                    .ok_or(E::of(Kind::Unavailable))?;
                // Report the grant relative to the selecting request. The
                // selection already debited one page from a nonzero budget.
                let grant = Grant {
                    subscription_id: owner.id,
                    sequence: reservation.sequence,
                    version: key.version.clone(),
                    page: key.page,
                    membership: owner.membership,
                    receiver: owner.receiver.clone(),
                    deadline: entry.deadline.min(reservation.deadline),
                    remaining_page_budget: reservation.page_budget - 1,
                    remaining_byte_budget: remaining,
                    // Never grant more than this request asked for.
                    flights: reservation.flights.map(|requested| {
                        u16::try_from(entry.flights)
                            .expect("bounded flights")
                            .min(requested)
                    }),
                };
                let completed = include_page(&entry.completed, key.page);
                let count = *completed_intervals - entry.completed.len() + completed.len();
                if count > limit {
                    return Err(E::of(Kind::Overloaded));
                }
                *completed_intervals = count;
                entry.completed = completed;
                entry.subscription.byte_budget = contract;
                let flight = entry
                    .pending
                    .iter_mut()
                    .find(|flight| flight.token == token)
                    .expect("pending flight");
                flight.settled = true;
                Ok(Delivery {
                    payload: payload.clone(),
                    grant,
                })
            },
            &mut self.wake,
        );
    }

    /// Take acquisition ownership if credential failure elected this waiter.
    /// Returns the work token and its page.
    pub fn take_work(&mut self, waiter: u64, now_ms: u64) -> Option<(u64, V, u64)> {
        self.advance(now_ms);
        let pending = self.fanout.pending_mut(waiter)?;
        if !pending.promoted || pending.result.is_some() {
            return None;
        }
        pending.promoted = false;
        let flight = pending.flight;
        let key = self.fanout.flights().get(&flight)?;
        Some((flight, key.version.clone(), key.page))
    }

    /// Poll a response slot. A ready result consumes the slot; the token is
    /// then stale. A pending slot is parked until its token is woken.
    pub fn poll(&mut self, waiter: u64, now_ms: u64) -> Polled<Result<Delivery<P, M, R, V>, E>> {
        self.advance(now_ms);
        let Some(pending) = self.fanout.pending_mut(waiter) else {
            return Polled::Ready(Err(E::of(Kind::StaleFlight)));
        };
        if pending.result.is_none() {
            pending.waker = Some(waiter);
            return Polled::Pending;
        }
        let pending = self.remove_pending(waiter).expect("pending slot");
        if pending.promoted {
            // Expiry may win before the elected waiter takes its work. There
            // is no acquisition owner left to fence this flight.
            self.cancel_work(pending.flight);
        }
        Polled::Ready(pending.result.expect("ready"))
    }

    /// Abandon a response slot (the dataplane drops `Waiter`). An elected but
    /// untaken acquisition is cancelled with it.
    pub fn cancel_waiter(&mut self, waiter: u64) {
        if let Some(pending) = self.remove_pending(waiter)
            && pending.promoted
        {
            self.cancel_work(pending.flight);
        }
    }

    /// Current scheduler time (the largest `now_ms` seen).
    pub fn now(&self) -> u64 {
        self.now
    }

    /// Earliest time the next prune runs.
    pub fn next_prune(&self) -> u64 {
        self.next_prune
    }

    pub fn entry_count(&self) -> usize {
        self.entries.len()
    }

    /// Active acquisition owners, including abandoned or expired work.
    pub fn flight_count(&self) -> usize {
        self.fanout.flights().len()
    }

    /// Retained response slots, including unconsumed results.
    pub fn pending_count(&self) -> usize {
        self.fanout.pending().len()
    }

    pub fn entry(&self, key: &EntryKey<M, R>) -> Option<EntryView> {
        self.entries.get(key).map(view)
    }

    pub fn entries(&self) -> impl Iterator<Item = EntryView> + '_ {
        self.entries.values().map(view)
    }
}

fn view<V>(entry: &Entry<V>) -> EntryView {
    EntryView {
        page_budget: entry.subscription.page_budget,
        byte_budget: entry.subscription.byte_budget,
        flights: entry.flights,
        pending: entry.pending.len(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum Err {
        Kind(Kind),
        Rejected,
        Io,
    }

    impl Fault for Err {
        fn of(kind: Kind) -> Self {
            Self::Kind(kind)
        }

        fn promotes(&self) -> bool {
            *self == Self::Rejected
        }
    }

    type S = Scheduler<u64, u8, u8, &'static str, Err>;

    fn contract(id: u8, sequence: u64, end: u64, flights: Option<u16>) -> Contract<u8> {
        Contract {
            id: [id; 16],
            version: 1,
            demand: vec![Interval { start: 0, end }],
            sequence,
            page_budget: 10,
            byte_budget: 1 << 20,
            flights,
        }
    }

    type Got = Result<Delivery<&'static str, u64, u8, u8>, Err>;

    fn ready(polled: Polled<Got>) -> Got {
        match polled {
            Polled::Ready(result) => result,
            Polled::Pending => panic!("pending"),
        }
    }

    #[test]
    fn invalid_limits_are_rejected() {
        let limits = SubscriptionLimits {
            max_flights_per_subscription: 65,
            max_inflight: 100,
            max_pending: 100,
            ..Default::default()
        };
        assert!(S::new(limits).is_none());
        assert!(
            S::new(SubscriptionLimits {
                max_entries: 0,
                ..Default::default()
            })
            .is_none()
        );
        assert!(S::new(Default::default()).is_some());
    }

    #[test]
    fn leader_and_follower_share_one_flight_and_wake_by_token() {
        let mut s = S::new(Default::default()).unwrap();
        let a = s.admit(contract(1, 1, 1, None), 1, 1, 1000, 1).unwrap();
        let b = s.admit(contract(2, 1, 1, None), 1, 2, 1000, 1).unwrap();
        let lead = s.select(&a, 1, |_| Some(true)).unwrap().unwrap();
        let follow = s.select(&b, 1, |_| Some(true)).unwrap().unwrap();
        let work = lead.work.expect("leader");
        assert_eq!(follow.work, None);
        assert_eq!((follow.version, follow.page), (1, 0));
        assert_eq!(s.flight_count(), 1);
        assert!(matches!(s.poll(follow.waiter, 2), Polled::Pending));
        assert!(s.take_wakes().is_empty());
        s.complete(work, Ok(("page", 40)), Some(3));
        assert_eq!(s.take_wakes(), vec![follow.waiter]);
        let got = ready(s.poll(follow.waiter, 3)).unwrap();
        assert_eq!(got.payload, "page");
        assert_eq!(got.grant.receiver, 2);
        assert_eq!(got.grant.remaining_page_budget, 9);
        assert_eq!(got.grant.remaining_byte_budget, (1 << 20) - 40);
        assert_eq!(ready(s.poll(lead.waiter, 3)).unwrap().grant.receiver, 1);
        assert_eq!(
            ready(s.poll(lead.waiter, 3)).unwrap_err(),
            Err::Kind(Kind::StaleFlight)
        );
        assert_eq!(s.pending_count(), 0);
        assert_eq!(s.flight_count(), 0);
    }

    #[test]
    fn credential_failure_promotes_one_follower_and_cancel_fences_it() {
        let mut s = S::new(Default::default()).unwrap();
        let a = s.admit(contract(1, 1, 1, None), 1, 1, 1000, 1).unwrap();
        let b = s.admit(contract(2, 1, 1, None), 1, 2, 1000, 1).unwrap();
        let lead = s.select(&a, 1, |_| Some(true)).unwrap().unwrap();
        let follow = s.select(&b, 1, |_| Some(true)).unwrap().unwrap();
        assert!(matches!(s.poll(follow.waiter, 1), Polled::Pending));
        s.complete(lead.work.unwrap(), Err(Err::Rejected), None);
        assert_eq!(s.take_wakes(), vec![follow.waiter]);
        assert_eq!(ready(s.poll(lead.waiter, 1)).unwrap_err(), Err::Rejected);
        let (work, version, page) = s.take_work(follow.waiter, 1).expect("elected");
        assert_eq!((version, page), (1, 0));
        assert!(s.take_work(follow.waiter, 1).is_none(), "taken once");
        s.cancel_work(work);
        assert_eq!(
            ready(s.poll(follow.waiter, 1)).unwrap_err(),
            Err::Kind(Kind::Cancelled)
        );
        assert_eq!(s.flight_count(), 0);
    }

    #[test]
    fn dropped_promoted_waiter_cancels_the_flight() {
        let mut s = S::new(Default::default()).unwrap();
        let a = s.admit(contract(1, 1, 1, None), 1, 1, 1000, 1).unwrap();
        let b = s.admit(contract(2, 1, 1, None), 1, 2, 1000, 1).unwrap();
        let lead = s.select(&a, 1, |_| Some(true)).unwrap().unwrap();
        let follow = s.select(&b, 1, |_| Some(true)).unwrap().unwrap();
        s.complete(lead.work.unwrap(), Err(Err::Rejected), None);
        s.cancel_waiter(follow.waiter);
        assert_eq!(s.flight_count(), 0);
        s.cancel_waiter(lead.waiter);
        assert_eq!(s.pending_count(), 0);
    }

    #[test]
    fn expiry_wakes_parked_waiters_but_keeps_the_owner() {
        let mut s = S::new(Default::default()).unwrap();
        let a = s.admit(contract(1, 1, 1, None), 1, 1, 100, 1).unwrap();
        let lead = s.select(&a, 1, |_| Some(true)).unwrap().unwrap();
        assert!(matches!(s.poll(lead.waiter, 50), Polled::Pending));
        s.advance(100);
        assert_eq!(s.take_wakes(), vec![lead.waiter]);
        assert_eq!(
            ready(s.poll(lead.waiter, 100)).unwrap_err(),
            Err::Kind(Kind::DeadlineExceeded)
        );
        assert_eq!(s.flight_count(), 1, "unfenced owner keeps capacity");
        s.complete(lead.work.unwrap(), Err(Err::Io), None);
        assert_eq!(s.flight_count(), 0);
        s.advance(99);
        assert_eq!(s.now(), 100, "time never moves backward");
        s.advance(60_100);
        assert_eq!(s.entry_count(), 0);
    }

    #[test]
    fn replay_unknown_endpoint_and_release() {
        let mut s = S::new(Default::default()).unwrap();
        let a = s.admit(contract(1, 1, 2, None), 1, 1, 1000, 1).unwrap();
        assert_eq!(
            s.admit(contract(1, 1, 2, None), 1, 1, 1000, 1).unwrap_err(),
            Err::Kind(Kind::Replay)
        );
        assert_eq!(s.select(&a, 1, |_| None).unwrap(), Err(0));
        s.release(&a);
        assert_eq!(
            s.select(&a, 1, |_| Some(true)).unwrap_err(),
            Err::Kind(Kind::Replay)
        );
        assert_eq!(
            s.admit(contract(9, 1, 2, None), 1, 1, 1, 1).unwrap_err(),
            Err::Kind(Kind::DeadlineExceeded)
        );
    }

    #[test]
    fn multi_flight_late_sequence_is_admitted_once() {
        let mut s = S::new(Default::default()).unwrap();
        s.admit(contract(1, 3, 8, Some(3)), 1, 1, 1000, 1).unwrap();
        let late = s.admit(contract(1, 2, 8, Some(3)), 1, 1, 1000, 1).unwrap();
        assert_eq!(
            s.admit(contract(1, 2, 8, Some(3)), 1, 1, 1000, 1)
                .unwrap_err(),
            Err::Kind(Kind::Replay)
        );
        let selected = s.select(&late, 1, |_| Some(true)).unwrap().unwrap();
        let key = EntryKey {
            membership: 1,
            receiver: 1,
            id: [1; 16],
        };
        let entry = s.entry(&key).unwrap();
        assert_eq!((entry.flights, entry.pending), (3, 1));
        s.complete(selected.work.unwrap(), Ok(("p", 10)), None);
        let grant = ready(s.poll(selected.waiter, 1)).unwrap().grant;
        assert_eq!((grant.sequence, grant.flights), (2, Some(3)));
    }
}
