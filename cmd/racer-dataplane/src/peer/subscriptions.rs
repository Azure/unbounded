//! Node-wide bounded provider selection. Authentication and acquisition stay in PeerServer.
//!
//! All shared mutations are synchronous. No guard escapes into Fill or a future;
//! wakers are invoked only after releasing the mutex. Live handles, including
//! completed responses, retain capacity until consumed or dropped.

use crate::error::Error;
use crate::error::Result;
use crate::memory::CiphertextCopy;
use crate::memory::CiphertextPage;
use crate::model::MAX_FIELD_BYTES;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::model::PageId;
use crate::model::PageNumber;
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use std::collections::HashMap;
use std::sync::Arc;
use std::sync::Mutex;
use std::task::Context;
use std::task::Poll;
use std::task::Waker;

pub const MAX_DEMAND_INTERVALS: usize = 64;

/// Largest number of selections one subscription may request in flight. The
/// replay window is a 64-bit map, so no contract may grant more than this.
pub const MAX_SUBSCRIPTION_FLIGHTS: u16 = 64;

pub use flow_control::subscriptions::Interval as PageInterval;
use flow_control::subscriptions::{Fanout, contains, include as include_page};

/// Canonical intervals, never an expanded list of pages. Adjacent intervals must
/// be merged by the sender, so one logical demand has one encoding.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Demand(Vec<PageInterval>);

impl Demand {
    pub fn new(intervals: Vec<PageInterval>) -> Result<Self> {
        if !flow_control::subscriptions::canonical(&intervals, MAX_DEMAND_INTERVALS) {
            return Err(Error::InvalidRequest);
        }
        Ok(Self(intervals))
    }

    pub fn intervals(&self) -> &[PageInterval] {
        &self.0
    }

    pub fn contains(&self, page: u64) -> bool {
        contains(&self.0, page)
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    #[cfg(test)]
    pub fn page_count(&self) -> u64 {
        // Disjoint intervals in [0, u64::MAX) cannot overflow this sum.
        self.0.iter().map(|i| i.end - i.start).sum()
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Subscription {
    pub id: [u8; 16],

    pub version: ObjectVersion,

    pub demand: Demand,

    pub sequence: u64,

    pub page_budget: u32,

    pub byte_budget: u64,

    /// Selections the receiver asks to keep in flight on this contract.
    /// `None` is the original single-flight contract and is not encoded on the
    /// wire, so older providers still accept it. `Some(n)` requires
    /// `1..=MAX_SUBSCRIPTION_FLIGHTS`; older providers refuse the request.
    pub flights: Option<u16>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TransferGrant {
    pub subscription_id: [u8; 16],

    pub sequence: u64,

    pub page: PageId,

    pub membership: MembershipVersion,

    pub receiver: NodeId,

    /// Absolute Unix epoch milliseconds, not a renewable relative timeout.
    pub deadline: u64,

    pub remaining_page_budget: u32,

    pub remaining_byte_budget: u64,

    /// Flights the provider currently grants on this contract. Present exactly
    /// when the selecting request carried `Subscription::flights`.
    pub flights: Option<u16>,
}

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
    /// `max_pending` and `MAX_SUBSCRIPTION_FLIGHTS`.
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

pub struct Subscriptions {
    limits: SubscriptionLimits,

    state: Mutex<State>,
}

#[derive(Default)]
struct State {
    now: u64,
    next_prune: u64,

    entries: HashMap<EntryKey, Entry>,

    fanout: Fanout<FlightKey, EntryKey, Completion, Error>,

    completed_intervals: usize,

    wake: Vec<Waker>,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct EntryKey {
    membership: MembershipVersion,

    receiver: NodeId,

    id: [u8; 16],
}

struct Entry {
    /// Latest admitted contract: the highest sequence, its demand, and the
    /// contract-wide budgets, which every selection debits.
    subscription: Subscription,

    deadline: u64,

    completed: Vec<PageInterval>,

    /// Granted flights. Never increases for the life of the entry.
    flights: usize,

    /// Replay window. Bit `i` records that `subscription.sequence - i` was
    /// admitted; only offsets below `flights` may still be admitted.
    seen: u64,

    /// Admitted requests that have not selected yet. A newer admission
    /// supersedes the oldest one when the flight slots are full.
    admitted: Vec<Reservation>,

    /// Selected flights whose response has not been consumed or dropped.
    pending: Vec<Flight>,
}

/// One admitted request. Its grant is reported relative to the request's own
/// demand, budgets and deadline, so every response matches its request.
#[derive(Clone)]
struct Reservation {
    sequence: u64,

    demand: Demand,

    page_budget: u32,

    byte_budget: u64,

    deadline: u64,

    flights: Option<u16>,
}

struct Flight {
    token: u64,

    page: u64,

    reservation: Reservation,
}

impl Entry {
    fn reservation(subscription: &Subscription, deadline: u64) -> Reservation {
        Reservation {
            sequence: subscription.sequence,
            demand: subscription.demand.clone(),
            page_budget: subscription.page_budget,
            byte_budget: subscription.byte_budget,
            deadline,
            flights: subscription.flights,
        }
    }

    /// Take a flight slot for `reservation`, superseding the oldest admitted
    /// request if every slot is taken. A request older than every admitted
    /// one would be superseded at once, so it is a replay.
    fn reserve(&mut self, reservation: Reservation) -> Result<()> {
        if self.pending.len() >= self.flights {
            return Err(Error::Overloaded);
        }
        if self.admitted.len() + self.pending.len() >= self.flights {
            let (index, oldest) = self
                .admitted
                .iter()
                .enumerate()
                .min_by_key(|(_, r)| r.sequence)
                .expect("a free flight slot is admitted");
            if oldest.sequence > reservation.sequence {
                return Err(Error::Replay);
            }
            self.admitted.swap_remove(index);
        }
        self.admitted.push(reservation);
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct FlightKey {
    membership: MembershipVersion,

    page: PageId,
}

type Pending = flow_control::subscriptions::Pending<EntryKey, Completion, Error>;

#[derive(Clone)]
pub struct Completion {
    pub metadata: ObjectMetadata,

    pub ciphertext: CiphertextPage,

    pub grant: TransferGrant,
}

pub enum Selection {
    Leader { work: Work, waiter: Waiter },
    Follower(Waiter),
}

/// Unique ownership of acquisition. Dropping it fails the flight, with no refund.
pub struct Work {
    scheduler: Arc<Subscriptions>,

    token: Option<u64>,

    page: PageId,
}

/// One bounded response slot. Poll with the caller's deadline/cancellation scope;
/// this scheduler deliberately owns neither an executor nor a timer service.
pub struct Waiter {
    scheduler: Arc<Subscriptions>,

    token: Option<u64>,
}

impl Subscriptions {
    pub fn new(limits: SubscriptionLimits) -> Result<Self> {
        if limits.max_entries == 0
            || limits.max_completed_intervals == 0
            || limits.max_inflight == 0
            || limits.max_pending == 0
            || limits.max_flights_per_subscription == 0
            || limits.max_flights_per_subscription > limits.max_inflight
            || limits.max_flights_per_subscription > limits.max_pending
            || limits.max_flights_per_subscription > usize::from(MAX_SUBSCRIPTION_FLIGHTS)
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            limits,
            state: Mutex::new(State::default()),
        })
    }

    fn with_state<T>(&self, now: Option<u64>, f: impl FnOnce(&mut State) -> T) -> T {
        let (result, wake) = {
            let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
            if let Some(now) = now {
                state.expire(now);
            }
            let result = f(&mut state);
            (result, std::mem::take(&mut state.wake))
        };
        for waker in wake {
            waker.wake();
        }
        result
    }

    #[cfg(test)]
    pub fn prune(&self, now_ms: u64) {
        self.with_state(Some(now_ms), |_| ());
    }

    /// Caller supplies authenticated receiver, membership, and signed deadline.
    /// Rejected updates never alter the old contract. An accepted sequence is
    /// retained even when selection fails, preventing capacity-based replay.
    pub fn schedule(
        self: &Arc<Self>,
        subscription: Subscription,
        membership: MembershipVersion,
        receiver: NodeId,
        deadline: u64,
        now_ms: u64,
    ) -> Result<Selection> {
        let key = self.admit(subscription, membership, receiver, deadline, now_ms)?;
        let _release = Release {
            scheduler: self,
            key: &key,
        };
        self.select(&key, now_ms, |_| Some(true))?
            .map_err(|_| Error::Internal)
    }

    /// Retain the accepted sequence before yielding, but spend no credits until
    /// selection commits. Every retry sweeps current demand and checks the exact
    /// sequence, capacity, expiry and pending owner under the scheduler mutex.
    pub(crate) async fn schedule_scoped(
        self: &Arc<Self>,
        subscription: Subscription,
        membership: std::sync::Arc<crate::topology::Membership>,
        receiver: NodeId,
        local: &NodeId,
        placement: &crate::topology::Placement,
        scope: &crate::runtime::RequestScope,
    ) -> Result<Selection> {
        use crate::peer::protocol::encode_deadline;
        use crate::peer::protocol::millis;
        let now = || millis(uring_runtime::environment::wall_now());
        scope.check()?;
        let object = subscription.version.object.clone();
        let key = self.admit(
            subscription,
            membership.version,
            receiver,
            encode_deadline(scope.deadline)?,
            now()?,
        )?;
        // Free the flight slot on failure or cancellation. The sequence stays
        // in the replay window, so the request still cannot be replayed.
        let _release = Release {
            scheduler: self,
            key: &key,
        };
        let mut eligible = HashMap::new();
        // Bound retained endpoint decisions even if concurrent demand continually
        // changes. This is the maximum size of one compact scheduler sweep.
        let maximum = self
            .limits
            .max_entries
            .saturating_mul(MAX_DEMAND_INTERVALS)
            .saturating_add(self.limits.max_completed_intervals)
            .saturating_mul(2);
        loop {
            scope.check()?;
            match self.select(&key, now()?, |page| eligible.get(&page).copied())? {
                Ok(selection) => return Ok(selection),
                Err(page) => {
                    if eligible.len() >= maximum {
                        return Err(Error::Overloaded);
                    }
                    let rank = placement
                        .rank_scoped(membership.clone(), &object, PageNumber(page), Some(scope))
                        .await?;
                    eligible.insert(page, rank.ordered.first() == Some(local));
                    // A ready/hot rank must not chain arbitrary endpoint work in
                    // one poll. Also separate the final cold quantum from the
                    // next rank, preserving the per-poll member hash bound.
                    uring_runtime::drivers::yield_now().await;
                }
            }
        }
    }

    /// Admit one request on its contract. Sequences are strictly increasing per
    /// contract, but with `flights > 1` requests may arrive out of order: a
    /// sequence below the highest admitted one is accepted once if it is
    /// within the granted flight window. Only the highest sequence replaces
    /// demand, budgets and deadline; an older request selects from its own
    /// demand under the current contract budgets.
    fn admit(
        &self,
        subscription: Subscription,
        membership: MembershipVersion,
        receiver: NodeId,
        deadline: u64,
        now_ms: u64,
    ) -> Result<(EntryKey, u64)> {
        if membership.0 == 0
            || receiver.0.is_empty()
            || receiver.0.len() > MAX_FIELD_BYTES
            || subscription.version.object.cache.0.is_empty()
            || subscription.version.object.cache.0.len() > MAX_FIELD_BYTES
            || subscription
                .flights
                .is_some_and(|f| f == 0 || f > MAX_SUBSCRIPTION_FLIGHTS)
        {
            return Err(Error::InvalidRequest);
        }
        let granted = usize::from(subscription.flights.unwrap_or(1))
            .min(self.limits.max_flights_per_subscription);
        self.with_state(Some(now_ms), |state| {
            if deadline <= state.now {
                return Err(Error::DeadlineExceeded);
            }
            let key = EntryKey {
                membership,
                receiver: receiver.clone(),
                id: subscription.id,
            };
            let sequence = subscription.sequence;
            if let Some(entry) = state.entries.get_mut(&key) {
                let latest = entry.subscription.sequence;
                if sequence <= latest {
                    // Within the window and never admitted before.
                    let offset = latest - sequence;
                    if offset >= entry.flights as u64 || entry.seen & (1 << offset) != 0 {
                        return Err(Error::Replay);
                    }
                    if entry.subscription.version != subscription.version {
                        return Err(Error::InvalidRequest);
                    }
                    entry.reserve(Entry::reservation(&subscription, deadline))?;
                    entry.seen |= 1 << offset;
                    return Ok((key, sequence));
                }
                // The original single-flight contract refuses any attempt to
                // raise its deadline or budgets. A multi-flight contract may see
                // the newest request after other selections already debited it,
                // so it clamps instead; neither can ever increase.
                let strict = entry.flights == 1;
                if entry.subscription.version != subscription.version
                    || (strict
                        && (deadline > entry.deadline
                            || subscription.page_budget > entry.subscription.page_budget
                            || subscription.byte_budget > entry.subscription.byte_budget))
                {
                    return Err(Error::InvalidRequest);
                }
                let flights = entry.flights.min(granted);
                if entry.pending.len() >= flights {
                    return Err(Error::Overloaded);
                }
                entry.flights = flights;
                entry.reserve(Entry::reservation(&subscription, deadline))?;
                let shift = sequence - latest;
                entry.seen = if shift >= 64 { 0 } else { entry.seen << shift } | 1;
                entry.deadline = entry.deadline.min(deadline);
                let page_budget = entry.subscription.page_budget.min(subscription.page_budget);
                let byte_budget = entry.subscription.byte_budget.min(subscription.byte_budget);
                entry.subscription = subscription;
                entry.subscription.page_budget = page_budget;
                entry.subscription.byte_budget = byte_budget;
            } else {
                // A node aggregates all local readers under one logical contract.
                // Changing IDs cannot obtain duplicate transfers or extra votes.
                if state.entries.iter().any(|(other, entry)| {
                    other.membership == membership
                        && other.receiver == receiver
                        && entry.subscription.version == subscription.version
                        && entry.deadline > state.now
                }) {
                    return Err(Error::Overloaded);
                }
                if state.entries.len() >= self.limits.max_entries {
                    return Err(Error::Overloaded);
                }
                let reservation = Entry::reservation(&subscription, deadline);
                state.entries.insert(
                    key.clone(),
                    Entry {
                        subscription,
                        deadline,
                        completed: Vec::new(),
                        flights: granted,
                        seen: 1,
                        admitted: vec![reservation],
                        pending: Vec::new(),
                    },
                );
            }
            Ok((key, sequence))
        })
    }

    /// Drop an admitted request that will not select. Its sequence stays in
    /// the replay window.
    fn release(&self, (key, sequence): &(EntryKey, u64)) {
        self.with_state(None, |state| {
            if let Some(entry) = state.entries.get_mut(key) {
                entry.admitted.retain(|r| r.sequence != *sequence);
            }
        });
    }

    /// Select one page for an admitted request. An unknown eligibility endpoint
    /// is returned without reserving a flight. The caller computes it outside
    /// the lock, then repeats this transaction.
    fn select(
        self: &Arc<Self>,
        (key, sequence): &(EntryKey, u64),
        now_ms: u64,
        eligible: impl Fn(u64) -> Option<bool>,
    ) -> Result<std::result::Result<Selection, u64>> {
        self.with_state(Some(now_ms), |state| {
            let entry = state.entries.get(key).ok_or(Error::DeadlineExceeded)?;
            if entry.deadline <= state.now {
                return Err(Error::DeadlineExceeded);
            }
            if entry.pending.len() >= entry.flights {
                return Err(Error::Overloaded);
            }
            let reservation = entry
                .admitted
                .iter()
                .find(|r| r.sequence == *sequence)
                .ok_or(Error::Replay)?
                .clone();
            if entry.subscription.page_budget == 0
                || entry.subscription.byte_budget == 0
                || reservation.page_budget == 0
                || reservation.byte_budget == 0
            {
                return Err(Error::Unavailable);
            }
            if state.fanout.pending().len() >= self.limits.max_pending {
                return Err(Error::Overloaded);
            }
            let number = match state.select(key, &reservation.demand, &eligible) {
                Ok(number) => number.ok_or(Error::Unavailable)?,
                Err(page) => return Ok(Err(page)),
            };
            let deadline = entry.deadline.min(reservation.deadline);
            let page = PageId {
                version: entry.subscription.version.clone(),
                number: PageNumber(number),
            };
            let flight_key = FlightKey {
                membership: key.membership,
                page: page.clone(),
            };
            let (token, flight, leader) = state
                .fanout
                .attach(
                    flight_key,
                    key.clone(),
                    deadline,
                    state.now,
                    self.limits.max_pending,
                    self.limits.max_inflight,
                )
                .ok_or(Error::Overloaded)?;
            let entry = state.entries.get_mut(key).expect("admitted entry");
            entry.subscription.page_budget -= 1;
            entry.admitted.retain(|r| r.sequence != *sequence);
            entry.pending.push(Flight {
                token,
                page: number,
                reservation,
            });
            let waiter = Waiter {
                scheduler: self.clone(),
                token: Some(token),
            };
            Ok(Ok(if !leader {
                Selection::Follower(waiter)
            } else {
                Selection::Leader {
                    work: Work {
                        scheduler: self.clone(),
                        token: Some(flight),
                        page,
                    },
                    waiter,
                }
            }))
        })
    }
}

/// Frees an admitted request's flight slot unless it selected.
struct Release<'a> {
    scheduler: &'a Subscriptions,

    key: &'a (EntryKey, u64),
}

impl Drop for Release<'_> {
    fn drop(&mut self) {
        self.scheduler.release(self.key);
    }
}

impl State {
    fn expire(&mut self, now: u64) {
        self.now = self.now.max(now);
        self.fanout
            .expire(self.now, || Error::DeadlineExceeded, &mut self.wake);
        if self.now < self.next_prune {
            return;
        }
        self.next_prune = self.now.saturating_add(100);
        self.entries.retain(|_, entry| {
            // Retain the spent contract for the signature freshness window after
            // expiry. Replaying it on a fresh socket cannot renew its deadline.
            if entry.deadline.saturating_add(60_000) > self.now {
                true
            } else {
                self.completed_intervals -= entry.completed.len();
                false
            }
        });
        // Never release acquisition capacity while the owner can still run Fill.
    }

    /// The target votes with the selecting request's own demand, excluding
    /// pages it already has in flight. Other receivers vote with their latest
    /// demand.
    fn select(
        &self,
        target: &EntryKey,
        demand: &Demand,
        eligible: &impl Fn(u64) -> Option<bool>,
    ) -> std::result::Result<Option<u64>, u64> {
        let version = &self.entries[target].subscription.version;
        // Sweep endpoints, not pages. Count distinct receivers, not subscription
        // IDs, so one receiver cannot boost its priority by issuing duplicate IDs.
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
                    outstanding(&entry.subscription.demand, &entry.completed),
                ));
            }
            let mut done = entry.completed.clone();
            for flight in &entry.pending {
                done = include_page(&done, flight.page);
            }
            Some((&key.receiver, true, outstanding(demand, &done)))
        });
        flow_control::subscriptions::select(demands, eligible)
    }

    fn remove_pending(&mut self, token: u64) -> Option<Pending> {
        let pending = self.fanout.remove(token)?;
        if let Some(entry) = self.entries.get_mut(&pending.owner) {
            entry.pending.retain(|flight| flight.token != token);
        }
        Some(pending)
    }

    fn finish(&mut self, flight: u64, result: Result<CiphertextCopy>, limit: usize) {
        // An origin rejection says nothing about another credential supplier.
        // Elect exactly one retained follower to retry with its own verified
        // request and original route credits. Never clone or refund authority.
        let promote = matches!(
            result,
            Err(Error::OriginRejected | Error::OriginForbidden | Error::Unauthorized)
        );
        self.fanout.finish(
            flight,
            promote,
            |token, owner, key| {
                let copy = result.as_ref().map_err(|e| *e)?;
                let entry = self.entries.get_mut(owner).ok_or(Error::DeadlineExceeded)?;
                let reservation = &entry
                    .pending
                    .iter()
                    .find(|flight| flight.token == token)
                    .ok_or(Error::StaleFlight)?
                    .reservation;
                let bytes = copy.ciphertext.bytes().len() as u64;
                let contract = entry
                    .subscription
                    .byte_budget
                    .checked_sub(bytes)
                    .ok_or(Error::Unavailable)?;
                let remaining = reservation
                    .byte_budget
                    .checked_sub(bytes)
                    .ok_or(Error::Unavailable)?;
                // Report the grant relative to the selecting request, so each
                // response matches its own signed request. The selection
                // already debited one page from a nonzero request budget.
                let grant = TransferGrant {
                    subscription_id: owner.id,
                    sequence: reservation.sequence,
                    page: key.page.clone(),
                    membership: owner.membership,
                    receiver: owner.receiver.clone(),
                    deadline: entry.deadline.min(reservation.deadline),
                    remaining_page_budget: reservation.page_budget - 1,
                    remaining_byte_budget: remaining,
                    flights: reservation
                        .flights
                        .map(|_| entry.flights.try_into().expect("bounded flights")),
                };
                let completed = include_page(&entry.completed, key.page.number.0);
                let count = self.completed_intervals - entry.completed.len() + completed.len();
                if count > limit {
                    return Err(Error::Overloaded);
                }
                self.completed_intervals = count;
                entry.completed = completed;
                entry.subscription.byte_budget = contract;
                Ok(Completion {
                    metadata: copy.metadata.clone(),
                    ciphertext: copy.ciphertext.clone(),
                    grant,
                })
            },
            &mut self.wake,
        );
    }
}

impl Work {
    pub fn page(&self) -> &PageId {
        &self.page
    }

    /// Structural validation only: Fill remains responsible for authentication.
    /// Per-subscriber budget/capacity failures are delivered through its waiter.
    pub fn complete(mut self, copy: CiphertextCopy, now_ms: u64) -> Result<()> {
        let valid = if copy.ciphertext.envelope().page != self.page {
            Err(Error::CorruptRecord)
        } else {
            copy.validate_metadata()
        };
        let result = valid.map(|()| copy);
        let token = self.token.take().expect("owned work");
        self.scheduler.with_state(Some(now_ms), |state| {
            state.finish(token, result, self.scheduler.limits.max_completed_intervals);
        });
        valid
    }

    pub fn fail(mut self, error: Error) {
        let token = self.token.take().expect("owned work");
        self.scheduler.with_state(None, |state| {
            state.finish(
                token,
                Err(error),
                self.scheduler.limits.max_completed_intervals,
            );
        });
    }
}

impl Drop for Work {
    fn drop(&mut self) {
        if let Some(token) = self.token.take() {
            self.scheduler.with_state(None, |state| {
                state.finish(
                    token,
                    Err(Error::Cancelled),
                    self.scheduler.limits.max_completed_intervals,
                );
            });
        }
    }
}

impl Waiter {
    /// Called before polling the response. Only credential-supplier failure can
    /// transfer acquisition ownership, and only to one still-live waiter.
    pub fn take_work(&mut self, now_ms: u64) -> Option<Work> {
        let token = self.token?;
        self.scheduler.with_state(Some(now_ms), |state| {
            let pending = state.fanout.pending_mut(token)?;
            if !pending.promoted || pending.result.is_some() {
                return None;
            }
            pending.promoted = false;
            let flight = pending.flight;
            Some(Work {
                scheduler: self.scheduler.clone(),
                token: Some(flight),
                page: state.fanout.flights().get(&flight)?.page.clone(),
            })
        })
    }
    #[cfg(test)]
    pub fn try_result(&mut self, now_ms: u64) -> Result<Option<Completion>> {
        match self.poll_result(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            now_ms,
        ) {
            Poll::Ready(result) => result.map(Some),
            Poll::Pending => Ok(None),
        }
    }

    pub fn poll_result(&mut self, cx: &mut Context<'_>, now_ms: u64) -> Poll<Result<Completion>> {
        let waker = cx.waker();
        let Some(token) = self.token else {
            return Poll::Ready(Err(Error::StaleFlight));
        };
        let result = self.scheduler.with_state(Some(now_ms), |state| {
            let Some(pending) = state.fanout.pending_mut(token) else {
                return Poll::Ready(Err(Error::StaleFlight));
            };
            if pending.result.is_some() {
                let pending = state.remove_pending(token).expect("pending slot");
                if pending.promoted {
                    // Expiry may win before the elected waiter takes its Work.
                    // There is no acquisition owner left to fence this flight.
                    state.finish(
                        pending.flight,
                        Err(Error::Cancelled),
                        self.scheduler.limits.max_completed_intervals,
                    );
                }
                return Poll::Ready(pending.result.expect("ready"));
            }
            if !pending
                .waker
                .as_ref()
                .is_some_and(|old| old.will_wake(waker))
            {
                pending.waker = Some(waker.clone());
            }
            Poll::Pending
        });
        if result.is_ready() {
            self.token = None;
        }
        result
    }
}

impl Drop for Waiter {
    fn drop(&mut self) {
        if let Some(token) = self.token.take() {
            self.scheduler.with_state(None, |state| {
                if let Some(pending) = state.remove_pending(token)
                    && pending.promoted
                {
                    state.finish(
                        pending.flight,
                        Err(Error::Cancelled),
                        self.scheduler.limits.max_completed_intervals,
                    );
                }
            });
        }
    }
}

fn outstanding(demand: &Demand, completed: &[PageInterval]) -> Vec<PageInterval> {
    flow_control::subscriptions::outstanding(demand.intervals(), completed)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::StrongEtag;
    use racer_control_wire::CacheId;
    use std::rc::Rc;

    fn subscription(id: u8, start: u64, end: u64) -> Subscription {
        Subscription {
            id: [id; 16],
            sequence: 0,
            page_budget: 10,
            byte_budget: 1024,
            flights: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cccccccc-1111-4111-8111-111111111111".into()),
                    key: CacheKey([1; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            demand: Demand::new(vec![PageInterval { start, end }]).unwrap(),
        }
    }
    fn schedule(s: &Arc<Subscriptions>, sub: Subscription, receiver: &str) -> Result<Selection> {
        s.schedule(sub, MembershipVersion(1), NodeId(receiver.into()), 1000, 1)
    }
    #[test]
    fn prune_gate_keeps_exact_deadlines_and_bounds_retired_contract_delay() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let key = scheduler
            .admit(
                subscription(1, 0, 1),
                MembershipVersion(1),
                NodeId("a".into()),
                50,
                1,
            )
            .unwrap();
        assert!(matches!(
            scheduler.select(&key, 50, |_| Some(true)),
            Err(Error::DeadlineExceeded)
        ));
        assert_eq!(scheduler.state.lock().unwrap().next_prune, 101);
        scheduler.prune(60_049);
        scheduler.prune(60_050);
        assert_eq!(scheduler.state.lock().unwrap().entries.len(), 1);
        scheduler.prune(60_148);
        assert_eq!(scheduler.state.lock().unwrap().entries.len(), 1);
        scheduler.prune(60_149);
        assert!(scheduler.state.lock().unwrap().entries.is_empty());
        scheduler.prune(0);
        assert_eq!(scheduler.state.lock().unwrap().now, 60_149);
    }
    #[test]
    fn compact_demand_rejects_noncanonical_and_never_expands_pages() {
        for intervals in [
            vec![PageInterval { start: 1, end: 1 }],
            vec![
                PageInterval { start: 1, end: 3 },
                PageInterval { start: 3, end: 4 },
            ],
            vec![
                PageInterval { start: 2, end: 4 },
                PageInterval { start: 1, end: 2 },
            ],
            vec![PageInterval { start: 1, end: 2 }; MAX_DEMAND_INTERVALS + 1],
        ] {
            assert!(Demand::new(intervals).is_err());
        }
        let demand = Demand::new(vec![PageInterval {
            start: 0,
            end: u64::MAX,
        }])
        .unwrap();
        assert_eq!(demand.page_count(), u64::MAX);
        assert!(demand.contains(u64::MAX - 1));
        assert!(!demand.contains(u64::MAX));
    }
    #[test]
    fn deferred_selection_rechecks_fairness_sequence_expiry_capacity_and_credits() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let key = scheduler
            .admit(
                subscription(1, 0, 20),
                MembershipVersion(1),
                NodeId("a".into()),
                1000,
                1,
            )
            .unwrap();
        assert!(matches!(scheduler.select(&key, 1, |_| None), Ok(Err(0))));
        // Demand arriving while rank is suspended changes the winner. A cached
        // answer for page zero must not commit the obsolete candidate.
        let Selection::Leader { work, waiter } =
            schedule(&scheduler, subscription(2, 8, 9), "b").unwrap()
        else {
            panic!()
        };
        assert!(matches!(
            scheduler.select(&key, 1, |p| (p == 0).then_some(true)),
            Ok(Err(8))
        ));
        let Ok(Selection::Follower(follower)) = scheduler.select(&key, 1, |_| Some(true)).unwrap()
        else {
            panic!("must join current hot page")
        };
        assert!(matches!(
            scheduler.select(&key, 1, |_| Some(true)),
            Err(Error::Overloaded)
        ));
        assert_eq!(
            scheduler.state.lock().unwrap().entries[&key.0]
                .subscription
                .page_budget,
            9
        );
        drop((work, waiter, follower));
        // Accepted updates invalidate suspended older work, without another debit.
        let mut update = subscription(1, 0, 20);
        update.sequence = 1;
        update.page_budget = 9;
        let latest = scheduler
            .admit(update, MembershipVersion(1), NodeId("a".into()), 1000, 1)
            .unwrap();
        assert!(matches!(
            scheduler.select(&key, 1, |_| Some(true)),
            Err(Error::Replay)
        ));
        assert!(matches!(
            scheduler.select(&latest, 1000, |_| Some(true)),
            Err(Error::DeadlineExceeded)
        ));
        assert_eq!(
            scheduler.state.lock().unwrap().entries[&key.0]
                .subscription
                .page_budget,
            9
        );

        let scheduler = Arc::new(
            Subscriptions::new(SubscriptionLimits {
                max_pending: 1,
                max_flights_per_subscription: 1,
                ..Default::default()
            })
            .unwrap(),
        );
        let key = scheduler
            .admit(
                subscription(1, 0, 20),
                MembershipVersion(1),
                NodeId("a".into()),
                1000,
                1,
            )
            .unwrap();
        assert!(matches!(scheduler.select(&key, 1, |_| None), Ok(Err(0))));
        let held = schedule(&scheduler, subscription(2, 8, 9), "b").unwrap();
        assert!(matches!(
            scheduler.select(&key, 1, |_| Some(true)),
            Err(Error::Overloaded)
        ));
        assert_eq!(
            scheduler.state.lock().unwrap().entries[&key.0]
                .subscription
                .page_budget,
            10
        );
        drop(held);
    }
    #[test]
    fn provider_chooses_from_whole_demand_and_joins_one_completed_flight() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let Selection::Leader {
            work,
            waiter: mut first,
        } = schedule(&scheduler, subscription(1, 8, 9), "a").unwrap()
        else {
            panic!()
        };
        let Selection::Follower(mut second) =
            schedule(&scheduler, subscription(2, 0, u64::MAX), "b").unwrap()
        else {
            panic!("overlap must select page 8, not requester prefix")
        };
        assert_eq!(work.page().number.0, 8);
        work.fail(Error::Unavailable);
        assert!(matches!(first.try_result(2), Err(Error::Unavailable)));
        assert!(matches!(second.try_result(2), Err(Error::Unavailable)));
        assert!(scheduler.state.lock().unwrap().fanout.flights().is_empty());
    }
    #[test]
    fn budgets_sequences_deadlines_membership_and_capacity_are_finite() {
        let scheduler = Arc::new(
            Subscriptions::new(SubscriptionLimits {
                max_entries: 1,
                max_pending: 1,
                max_inflight: 1,
                max_completed_intervals: 1,
                max_flights_per_subscription: 1,
            })
            .unwrap(),
        );
        let sub = subscription(1, 0, 2);
        let Selection::Leader { work, waiter } = schedule(&scheduler, sub.clone(), "a").unwrap()
        else {
            panic!()
        };
        assert!(matches!(
            schedule(&scheduler, sub.clone(), "a"),
            Err(Error::Replay)
        ));
        assert!(matches!(
            schedule(&scheduler, subscription(2, 0, 2), "b"),
            Err(Error::Overloaded)
        ));
        drop(waiter);
        drop(work);
        let mut update = sub.clone();
        update.sequence = 1;
        assert!(
            matches!(
                schedule(&scheduler, update.clone(), "a"),
                Err(Error::InvalidRequest)
            ),
            "consumed page credit cannot refill"
        );
        update.page_budget -= 1;
        assert!(
            scheduler
                .schedule(
                    update.clone(),
                    MembershipVersion(1),
                    NodeId("a".into()),
                    1001,
                    1
                )
                .is_err()
        );
        let Selection::Leader { work, waiter } = schedule(&scheduler, update, "a").unwrap() else {
            panic!()
        };
        scheduler.prune(1000);
        assert_eq!(
            scheduler.state.lock().unwrap().fanout.flights().len(),
            1,
            "expiry cannot release unfenced acquisition owner"
        );
        drop(waiter);
        drop(work);
        assert!(scheduler.state.lock().unwrap().fanout.flights().is_empty());
        assert!(matches!(
            scheduler.schedule(sub, MembershipVersion(0), NodeId("a".into()), 2000, 1001),
            Err(Error::InvalidRequest)
        ));
    }
    #[test]
    fn duplicate_receiver_ids_cannot_amplify_transfers_and_expired_contract_cannot_renew() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let Selection::Leader { work, waiter } =
            schedule(&scheduler, subscription(1, 0, 2), "a").unwrap()
        else {
            panic!()
        };
        assert!(matches!(
            schedule(&scheduler, subscription(2, 0, 2), "a"),
            Err(Error::Overloaded)
        ));
        drop(work);
        drop(waiter);
        let mut update = subscription(1, 0, 2);
        update.sequence = 1;
        update.page_budget -= 1;
        assert!(matches!(
            scheduler.schedule(update, MembershipVersion(1), NodeId("a".into()), 2000, 1001),
            Err(Error::InvalidRequest)
        ));
        scheduler.prune(61_000);
        assert!(scheduler.state.lock().unwrap().entries.is_empty());
    }
    #[test]
    fn credential_failure_elects_one_other_supplier_without_refunding_credits() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let Selection::Leader {
            work,
            waiter: mut first,
        } = schedule(&scheduler, subscription(1, 0, 2), "a").unwrap()
        else {
            panic!()
        };
        let Selection::Follower(mut second) =
            schedule(&scheduler, subscription(2, 0, 2), "b").unwrap()
        else {
            panic!()
        };
        let Selection::Follower(mut third) =
            schedule(&scheduler, subscription(3, 0, 2), "c").unwrap()
        else {
            panic!()
        };
        work.fail(Error::OriginForbidden);
        assert!(matches!(first.try_result(2), Err(Error::OriginForbidden)));
        assert!(second.try_result(2).unwrap().is_none());
        let next = second.take_work(2).expect("next credential supplier");
        assert!(second.take_work(2).is_none());
        assert!(third.take_work(2).is_none());
        assert_eq!(next.page().number.0, 0);
        next.fail(Error::OriginRejected);
        assert!(matches!(second.try_result(2), Err(Error::OriginRejected)));
        let last = third.take_work(2).unwrap();
        assert!(
            scheduler
                .state
                .lock()
                .unwrap()
                .entries
                .values()
                .all(|e| e.subscription.page_budget == 9)
        );
        last.fail(Error::Unavailable);
        assert!(matches!(third.try_result(2), Err(Error::Unavailable)));
        assert!(scheduler.state.lock().unwrap().fanout.flights().is_empty());
    }
    #[test]
    fn successful_fanout_shares_allocation_and_charges_each_receiver_once() {
        use crate::admission::AdmissionPolicy;
        use crate::admission::ResourceClass;
        use crate::memory::BufferPool;
        use crate::model::ExpiresAt;
        use crate::model::Nonce;
        use crate::model::PageEnvelope;
        use std::rc::Rc;
        use std::time::UNIX_EPOCH;
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let Selection::Leader {
            work,
            waiter: mut first,
        } = schedule(&scheduler, subscription(1, 0, 2), "a").unwrap()
        else {
            panic!()
        };
        let Selection::Follower(mut second) =
            schedule(&scheduler, subscription(2, 0, u64::MAX), "b").unwrap()
        else {
            panic!()
        };
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(admission.clone());
        let page = work.page().clone();
        let ciphertext = pool
            .ciphertext(
                admission
                    .reserve(
                        Some(&page.version.object.cache),
                        ResourceClass::Ciphertext,
                        19,
                    )
                    .unwrap(),
                PageEnvelope {
                    cipher: crate::model::PageCipher::Xaes256Gcm,
                    page: page.clone(),
                    key_id: racer_control_wire::KeyId([1; 16]),
                    nonce: Nonce([2; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                vec![3; 19],
            )
            .unwrap();
        work.complete(
            CiphertextCopy {
                metadata: ObjectMetadata {
                    version: page.version,
                    length: 3,
                    content_type: None,
                    expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
                },
                ciphertext,
            },
            2,
        )
        .unwrap();
        let a = first.try_result(2).unwrap().unwrap();
        let b = second.try_result(2).unwrap().unwrap();
        assert_eq!(a.ciphertext.bytes().as_ptr(), b.ciphertext.bytes().as_ptr());
        assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
        assert_eq!(a.grant.remaining_page_budget, 9);
        assert_eq!(b.grant.remaining_byte_budget, 1005);
        assert_ne!(a.grant.receiver, b.grant.receiver);
        assert!(matches!(first.try_result(2), Err(Error::StaleFlight)));
        drop((a, b));
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn expired_or_dropped_promoted_waiter_releases_unowned_flight() {
        for consume in [false, true] {
            let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
            let Selection::Leader {
                work,
                waiter: first,
            } = schedule(&scheduler, subscription(1, 0, 2), "a").unwrap()
            else {
                panic!()
            };
            let Selection::Follower(mut second) =
                schedule(&scheduler, subscription(2, 0, 2), "b").unwrap()
            else {
                panic!()
            };
            work.fail(Error::OriginRejected);
            drop(first);
            if consume {
                assert!(second.take_work(1000).is_none());
                assert!(matches!(
                    second.try_result(1000),
                    Err(Error::DeadlineExceeded)
                ));
            }
            drop(second);
            let state = scheduler.state.lock().unwrap();
            assert!(state.fanout.flights().is_empty());
            assert!(state.fanout.pending().is_empty());
        }
    }

    fn multi(id: u8, sequence: u64, flights: u16) -> Subscription {
        let mut sub = subscription(id, 0, 20);
        sub.sequence = sequence;
        sub.page_budget -= sequence as u32;
        sub.flights = Some(flights);
        sub
    }

    /// Complete `work` with a 3-byte page, as a fill would.
    fn complete(work: Work) -> Rc<flow_control::Quotas<crate::admission::AdmissionPolicy>> {
        use crate::admission::AdmissionPolicy;
        use crate::admission::ResourceClass;
        use crate::memory::BufferPool;
        use crate::model::ExpiresAt;
        use crate::model::Nonce;
        use crate::model::PageEnvelope;
        use std::time::UNIX_EPOCH;
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(admission.clone());
        let page = work.page().clone();
        let reservation = admission
            .reserve(
                Some(&page.version.object.cache),
                ResourceClass::Ciphertext,
                19,
            )
            .unwrap();
        let envelope = PageEnvelope {
            cipher: crate::model::PageCipher::Xaes256Gcm,
            page: page.clone(),
            key_id: racer_control_wire::KeyId([1; 16]),
            nonce: Nonce([2; 24]),
            plaintext_length: 3,
            ciphertext_length: 19,
        };
        let ciphertext = pool.ciphertext(reservation, envelope, vec![3; 19]).unwrap();
        let metadata = ObjectMetadata {
            // The completed page is the object's short last page.
            length: page.number.0 * crate::model::PAGE_BYTES + 3,
            version: page.version,
            content_type: None,
            expires_at: ExpiresAt::from_system_time(UNIX_EPOCH).unwrap(),
        };
        work.complete(
            CiphertextCopy {
                metadata,
                ciphertext,
            },
            2,
        )
        .unwrap();
        admission
    }

    fn leader(selection: Result<Selection>) -> (Work, Waiter) {
        match selection.unwrap() {
            Selection::Leader { work, waiter } => (work, waiter),
            Selection::Follower(_) => panic!("expected a leader"),
        }
    }

    #[test]
    fn multi_flight_pends_up_to_grant_and_debits_each_page() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let mut held = Vec::new();
        for sequence in 0..3 {
            held.push(leader(schedule(&scheduler, multi(1, sequence, 3), "a")));
        }
        let mut pages: Vec<_> = held.iter().map(|(w, _)| w.page().number.0).collect();
        pages.sort_unstable();
        assert_eq!(pages, [0, 1, 2], "pending pages are never selected twice");
        assert!(matches!(
            schedule(&scheduler, multi(1, 3, 3), "a"),
            Err(Error::Overloaded)
        ));
        let key = EntryKey {
            membership: MembershipVersion(1),
            receiver: NodeId("a".into()),
            id: [1; 16],
        };
        {
            let state = scheduler.state.lock().unwrap();
            assert_eq!(state.entries[&key].subscription.page_budget, 7);
            assert_eq!(state.entries[&key].pending.len(), 3);
        }
        // Each grant is relative to its own request: budget minus one page.
        let (work, mut waiter) = held.remove(1);
        let _admission = complete(work);
        let completion = waiter.try_result(2).unwrap().unwrap();
        assert_eq!(completion.grant.sequence, 1);
        assert_eq!(completion.grant.remaining_page_budget, 8);
        assert_eq!(completion.grant.remaining_byte_budget, 1024 - 19);
        assert_eq!(completion.grant.flights, Some(3));
        assert_eq!(
            scheduler.state.lock().unwrap().entries[&key]
                .subscription
                .byte_budget,
            1024 - 19
        );
        // A consumed response frees one slot for the next sequence.
        let (next, _) = leader(schedule(&scheduler, multi(1, 4, 3), "a"));
        assert_eq!(
            next.page().number.0,
            3,
            "completed and pending pages skipped"
        );
        assert_eq!(
            scheduler.state.lock().unwrap().entries[&key]
                .subscription
                .page_budget,
            5
        );
        drop((held, next));
    }

    #[test]
    fn multi_flight_grant_is_capped_and_absent_field_is_single_flight() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        for flights in [0, MAX_SUBSCRIPTION_FLIGHTS + 1] {
            let mut sub = multi(1, 0, 1);
            sub.flights = Some(flights);
            assert!(matches!(
                schedule(&scheduler, sub, "a"),
                Err(Error::InvalidRequest)
            ));
        }
        let mut held = Vec::new();
        for sequence in 0..8 {
            held.push(schedule(&scheduler, multi(1, sequence, 20), "a").unwrap());
        }
        assert!(matches!(
            schedule(&scheduler, multi(1, 8, 20), "a"),
            Err(Error::Overloaded)
        ));
        // Without the field, a second request waits for the first response.
        let mut single = subscription(2, 15, 20);
        let (work, mut waiter) = leader(schedule(&scheduler, single.clone(), "b"));
        single.sequence = 1;
        single.page_budget = 9;
        assert!(matches!(
            schedule(&scheduler, single.clone(), "b"),
            Err(Error::Overloaded)
        ));
        let _admission = complete(work);
        assert_eq!(waiter.try_result(2).unwrap().unwrap().grant.flights, None);
        single.sequence = 2;
        single.byte_budget -= 19;
        let next = schedule(&scheduler, single, "b");
        assert!(next.is_ok(), "{:?}", next.err());
        drop(held);
    }

    #[test]
    fn multi_flight_accepts_reordered_sequences_once_within_window() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let admit = |sub| scheduler.admit(sub, MembershipVersion(1), NodeId("a".into()), 1000, 1);
        let five = admit(multi(1, 5, 3)).unwrap();
        // An older request in the window is admitted once, using its own demand.
        let mut four = multi(1, 4, 3);
        four.demand = Demand::new(vec![PageInterval { start: 7, end: 8 }]).unwrap();
        let four = admit(four).unwrap();
        assert!(matches!(admit(multi(1, 4, 3)), Err(Error::Replay)));
        assert!(matches!(admit(multi(1, 5, 3)), Err(Error::Replay)));
        assert!(matches!(admit(multi(1, 2, 3)), Err(Error::Replay)));
        let Ok(Selection::Leader { work, waiter }) =
            scheduler.select(&four, 1, |_| Some(true)).unwrap()
        else {
            panic!()
        };
        assert_eq!(work.page().number.0, 7);
        assert!(matches!(
            scheduler.select(&four, 1, |_| Some(true)),
            Err(Error::Replay)
        ));
        // A third request fills the last slot; a fourth supersedes the oldest
        // unselected one, which can no longer select.
        let six = admit(multi(1, 6, 3)).unwrap();
        let seven = admit(multi(1, 7, 3)).unwrap();
        assert!(matches!(
            scheduler.select(&five, 1, |_| Some(true)),
            Err(Error::Replay)
        ));
        let six = scheduler.select(&six, 1, |_| Some(true)).unwrap().ok();
        let seven = scheduler.select(&seven, 1, |_| Some(true)).unwrap().ok();
        assert!(six.is_some() && seven.is_some());
        assert!(matches!(admit(multi(1, 8, 3)), Err(Error::Overloaded)));
        drop((work, waiter, six, seven));
    }

    #[test]
    fn multi_flight_clamps_newer_budgets_and_fans_out_across_receivers() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let (work, mut first) = leader(schedule(&scheduler, multi(1, 0, 2), "a"));
        // The newest request was computed before the first debit: clamp, do
        // not refuse, and never raise the contract budget.
        let mut update = multi(1, 1, 2);
        update.page_budget = 10;
        let (other, _waiter) = leader(schedule(&scheduler, update, "a"));
        let key = EntryKey {
            membership: MembershipVersion(1),
            receiver: NodeId("a".into()),
            id: [1; 16],
        };
        assert_eq!(
            scheduler.state.lock().unwrap().entries[&key]
                .subscription
                .page_budget,
            8
        );
        let Selection::Follower(mut follower) =
            schedule(&scheduler, subscription(2, 0, 1), "b").unwrap()
        else {
            panic!("another receiver joins the in-flight page")
        };
        assert_eq!(work.page().number.0, 0);
        let _admission = complete(work);
        let a = first.try_result(2).unwrap().unwrap();
        let b = follower.try_result(2).unwrap().unwrap();
        assert_eq!(a.ciphertext.bytes().as_ptr(), b.ciphertext.bytes().as_ptr());
        assert_eq!(a.grant.flights, Some(2));
        assert_eq!(b.grant.flights, None);
        drop(other);
    }

    #[test]
    fn flight_limit_must_fit_capacity() {
        for limits in [
            SubscriptionLimits {
                max_flights_per_subscription: 0,
                ..Default::default()
            },
            SubscriptionLimits {
                max_inflight: 4,
                max_flights_per_subscription: 5,
                ..Default::default()
            },
            SubscriptionLimits {
                max_pending: 4,
                max_flights_per_subscription: 5,
                ..Default::default()
            },
            SubscriptionLimits {
                max_inflight: 1000,
                max_pending: 1000,
                max_flights_per_subscription: usize::from(MAX_SUBSCRIPTION_FLIGHTS) + 1,
                ..Default::default()
            },
        ] {
            assert!(matches!(
                Subscriptions::new(limits),
                Err(Error::InvalidConfiguration)
            ));
        }
    }
}
