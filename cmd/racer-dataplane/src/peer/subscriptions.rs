//! Node-wide bounded provider selection. Authentication and acquisition stay in PeerServer.
//!
//! All shared mutations are synchronous. No guard escapes into Fill or a future;
//! wakers are invoked only after releasing the mutex. Live handles, including
//! completed responses, retain capacity until consumed or dropped.
use crate::{
    error::{Error, Result},
    memory::{page::CiphertextCopy, pool::CiphertextPage},
    model::{
        MAX_FIELD_BYTES,
        identity::{MembershipVersion, NodeId, ObjectVersion, PageId, PageNumber},
        metadata::ObjectMetadata,
    },
};
use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    task::{Context, Poll, Waker},
};

pub const MAX_DEMAND_INTERVALS: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PageInterval {
    pub start: u64,
    pub end: u64,
}

/// Canonical intervals, never an expanded list of pages. Adjacent intervals must
/// be merged by the sender, so one logical demand has one encoding.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Demand(Vec<PageInterval>);

impl Demand {
    pub fn new(intervals: Vec<PageInterval>) -> Result<Self> {
        if intervals.len() > MAX_DEMAND_INTERVALS
            || intervals.iter().any(|i| i.start >= i.end)
            || intervals.windows(2).any(|w| w[0].end >= w[1].start)
        {
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
}

#[derive(Clone, Copy, Debug)]
pub struct SubscriptionLimits {
    pub max_entries: usize,
    /// Node-wide total, not a per-subscription allocation.
    pub max_completed_intervals: usize,
    pub max_inflight: usize,
    /// Includes unconsumed completed/error responses, not just active waiters.
    pub max_pending: usize,
}

impl Default for SubscriptionLimits {
    fn default() -> Self {
        Self {
            max_entries: 1024,
            max_completed_intervals: 4096,
            max_inflight: 64,
            max_pending: 256,
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
    next: u64,
    entries: HashMap<EntryKey, Entry>,
    flights: HashMap<u64, FlightKey>,
    pending: HashMap<u64, Pending>,
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
    subscription: Subscription,
    deadline: u64,
    completed: Vec<PageInterval>,
    pending: Option<u64>,
}

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
struct FlightKey {
    membership: MembershipVersion,
    page: PageId,
}

struct Pending {
    entry: EntryKey,
    flight: u64,
    deadline: u64,
    result: Option<Result<Completion>>,
    waker: Option<Waker>,
    promoted: bool,
}

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
        self.schedule_eligible(subscription, membership, receiver, deadline, now_ms, |_| {
            true
        })
    }

    /// Eligibility is evaluated only at compact sweep endpoints, never by expanding
    /// an object's pages. The signed demand remains intact in the retained contract.
    pub fn schedule_eligible(
        self: &Arc<Self>,
        subscription: Subscription,
        membership: MembershipVersion,
        receiver: NodeId,
        deadline: u64,
        now_ms: u64,
        eligible: impl Fn(u64) -> bool,
    ) -> Result<Selection> {
        if membership.0 == 0
            || receiver.0.is_empty()
            || receiver.0.len() > MAX_FIELD_BYTES
            || subscription.version.object.cache.0.is_empty()
            || subscription.version.object.cache.0.len() > MAX_FIELD_BYTES
        {
            return Err(Error::InvalidRequest);
        }
        self.with_state(Some(now_ms), |state| {
            if deadline <= state.now {
                return Err(Error::DeadlineExceeded);
            }
            let key = EntryKey {
                membership,
                receiver: receiver.clone(),
                id: subscription.id,
            };
            if let Some(entry) = state.entries.get_mut(&key) {
                if subscription.sequence <= entry.subscription.sequence {
                    return Err(Error::Replay);
                }
                if entry.subscription.version != subscription.version
                    || deadline > entry.deadline
                    || subscription.page_budget > entry.subscription.page_budget
                    || subscription.byte_budget > entry.subscription.byte_budget
                {
                    return Err(Error::InvalidRequest);
                }
                if entry.pending.is_some() {
                    return Err(Error::Overloaded);
                }
                entry.subscription = subscription;
                entry.deadline = deadline;
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
                state.entries.insert(
                    key.clone(),
                    Entry {
                        subscription,
                        deadline,
                        completed: Vec::new(),
                        pending: None,
                    },
                );
            }
            let entry = &state.entries[&key];
            if entry.subscription.page_budget == 0 || entry.subscription.byte_budget == 0 {
                return Err(Error::Unavailable);
            }
            if state.pending.len() >= self.limits.max_pending {
                return Err(Error::Overloaded);
            }
            let number = state.select(&key, &eligible).ok_or(Error::Unavailable)?;
            let page = PageId {
                version: entry.subscription.version.clone(),
                number: PageNumber(number),
            };
            let flight_key = FlightKey {
                membership,
                page: page.clone(),
            };
            let existing = state.flights.iter().find_map(|(&token, candidate)| {
                // Do not attach new authority to an abandoned/expired leader.
                (candidate == &flight_key
                    && state
                        .pending
                        .values()
                        .any(|p| p.flight == token && p.result.is_none() && p.deadline > state.now))
                .then_some(token)
            });
            if existing.is_none() && state.flights.len() >= self.limits.max_inflight {
                return Err(Error::Overloaded);
            }
            // Checked tokens prevent stale handles from ever addressing new work.
            let token = state.next.checked_add(1).ok_or(Error::Overloaded)?;
            state.next = token;
            let flight = existing.unwrap_or(token);
            if existing.is_none() {
                state.flights.insert(flight, flight_key);
            }
            let entry = state.entries.get_mut(&key).expect("admitted entry");
            entry.subscription.page_budget -= 1;
            entry.pending = Some(token);
            state.pending.insert(
                token,
                Pending {
                    entry: key,
                    flight,
                    deadline,
                    result: None,
                    waker: None,
                    promoted: false,
                },
            );
            let waiter = Waiter {
                scheduler: self.clone(),
                token: Some(token),
            };
            Ok(if existing.is_some() {
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
            })
        })
    }
}

impl State {
    fn expire(&mut self, now: u64) {
        self.now = self.now.max(now);
        for pending in self.pending.values_mut() {
            if pending.deadline <= self.now {
                pending.result = Some(Err(Error::DeadlineExceeded));
                if let Some(waker) = pending.waker.take() {
                    self.wake.push(waker);
                }
            }
        }
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

    fn select(&self, target: &EntryKey, eligible: &impl Fn(u64) -> bool) -> Option<u64> {
        let version = &self.entries[target].subscription.version;
        // Sweep endpoints, not pages. Count distinct receivers, not subscription
        // IDs, so one receiver cannot boost its priority by issuing duplicate IDs.
        let mut events = Vec::new();
        for (key, entry) in &self.entries {
            if key.membership != target.membership
                || &entry.subscription.version != version
                || entry.deadline <= self.now
                || entry.subscription.byte_budget == 0
                || (entry.subscription.page_budget == 0 && entry.pending.is_none())
            {
                continue;
            }
            for interval in outstanding(&entry.subscription.demand, &entry.completed) {
                events.push((interval.start, &key.receiver, key == target, true));
                events.push((interval.end, &key.receiver, key == target, false));
            }
        }
        events.sort_unstable_by_key(|e| e.0);
        let mut receivers: HashMap<&NodeId, usize> = HashMap::new();
        let mut active = false;
        let mut best = None;
        let mut score = 0;
        let mut cursor = 0;
        while cursor < events.len() {
            let point = events[cursor].0;
            while cursor < events.len() && events[cursor].0 == point {
                let (_, receiver, target, start) = events[cursor];
                if start {
                    *receivers.entry(receiver).or_default() += 1;
                } else if let Some(count) = receivers.get_mut(receiver) {
                    *count -= 1;
                    if *count == 0 {
                        receivers.remove(receiver);
                    }
                }
                if target {
                    active = start;
                }
                cursor += 1;
            }
            if active && receivers.len() > score && eligible(point) {
                best = Some(point);
                score = receivers.len();
            }
        }
        best
    }

    fn remove_pending(&mut self, token: u64) -> Option<Pending> {
        let pending = self.pending.remove(&token)?;
        if let Some(entry) = self.entries.get_mut(&pending.entry)
            && entry.pending == Some(token)
        {
            entry.pending = None;
        }
        Some(pending)
    }

    fn finish(&mut self, flight: u64, result: Result<CiphertextCopy>, limit: usize) {
        let Some(key) = self.flights.remove(&flight) else {
            return;
        };
        // Bounded by max_pending. No per-flight list accumulates cancelled IDs.
        let mut tokens: Vec<_> = self
            .pending
            .iter()
            .filter_map(|(&token, p)| (p.flight == flight && p.result.is_none()).then_some(token))
            .collect();
        tokens.sort_unstable();
        // An origin rejection says nothing about another credential supplier.
        // Elect exactly one retained follower to retry with its own verified
        // request and original route credits. Never clone or refund authority.
        if matches!(
            result,
            Err(Error::OriginRejected | Error::OriginForbidden | Error::Unauthorized)
        ) {
            let next = tokens.iter().copied().find(|token| *token != flight);
            if let Some(next) = next {
                self.flights.insert(next, key.clone());
                for token in tokens.iter().copied().filter(|token| *token != flight) {
                    let pending = self.pending.get_mut(&token).expect("retained follower");
                    pending.flight = next;
                    pending.promoted = token == next;
                    if let Some(waker) = pending.waker.take() {
                        self.wake.push(waker);
                    }
                }
                tokens.retain(|token| *token == flight);
            }
        }
        for token in tokens {
            let pending = self.pending.get_mut(&token).expect("pending token");
            let completion = (|| {
                let copy = result.as_ref().map_err(|e| *e)?;
                let entry = self
                    .entries
                    .get_mut(&pending.entry)
                    .ok_or(Error::DeadlineExceeded)?;
                if entry.pending != Some(token) {
                    return Err(Error::StaleFlight);
                }
                let bytes = copy.ciphertext.bytes().len() as u64;
                let remaining = entry
                    .subscription
                    .byte_budget
                    .checked_sub(bytes)
                    .ok_or(Error::Unavailable)?;
                let completed = include_page(&entry.completed, key.page.number.0);
                let count = self.completed_intervals - entry.completed.len() + completed.len();
                if count > limit {
                    return Err(Error::Overloaded);
                }
                self.completed_intervals = count;
                entry.completed = completed;
                entry.subscription.byte_budget = remaining;
                Ok(Completion {
                    metadata: copy.metadata.clone(),
                    ciphertext: copy.ciphertext.clone(),
                    grant: TransferGrant {
                        subscription_id: pending.entry.id,
                        sequence: entry.subscription.sequence,
                        page: key.page.clone(),
                        membership: pending.entry.membership,
                        receiver: pending.entry.receiver.clone(),
                        deadline: entry.deadline,
                        remaining_page_budget: entry.subscription.page_budget,
                        remaining_byte_budget: remaining,
                    },
                })
            })();
            pending.result = Some(completion);
            if let Some(waker) = pending.waker.take() {
                self.wake.push(waker);
            }
        }
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
            let pending = state.pending.get_mut(&token)?;
            if !pending.promoted || pending.result.is_some() {
                return None;
            }
            pending.promoted = false;
            Some(Work {
                scheduler: self.scheduler.clone(),
                token: Some(pending.flight),
                page: state.flights.get(&pending.flight)?.page.clone(),
            })
        })
    }
    pub fn try_result(&mut self, now_ms: u64) -> Result<Option<Completion>> {
        match self.poll_inner(None, now_ms) {
            Poll::Ready(result) => result.map(Some),
            Poll::Pending => Ok(None),
        }
    }

    pub fn poll_result(&mut self, cx: &mut Context<'_>, now_ms: u64) -> Poll<Result<Completion>> {
        self.poll_inner(Some(cx.waker()), now_ms)
    }

    fn poll_inner(&mut self, waker: Option<&Waker>, now_ms: u64) -> Poll<Result<Completion>> {
        let Some(token) = self.token else {
            return Poll::Ready(Err(Error::StaleFlight));
        };
        let result = self.scheduler.with_state(Some(now_ms), |state| {
            let Some(pending) = state.pending.get_mut(&token) else {
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
            if let Some(waker) = waker
                && !pending
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

fn contains(intervals: &[PageInterval], page: u64) -> bool {
    let index = intervals.partition_point(|i| i.end <= page);
    intervals.get(index).is_some_and(|i| i.start <= page)
}

fn outstanding(demand: &Demand, completed: &[PageInterval]) -> Vec<PageInterval> {
    let mut result = Vec::new();
    let mut index = 0;
    for interval in demand.intervals() {
        let mut start = interval.start;
        while index < completed.len() && completed[index].end <= start {
            index += 1;
        }
        let mut cursor = index;
        while cursor < completed.len() && completed[cursor].start < interval.end {
            let done = completed[cursor];
            if start < done.start {
                result.push(PageInterval {
                    start,
                    end: done.start,
                });
            }
            start = start.max(done.end);
            cursor += 1;
        }
        if start < interval.end {
            result.push(PageInterval {
                start,
                end: interval.end,
            });
        }
    }
    result
}

fn include_page(intervals: &[PageInterval], page: u64) -> Vec<PageInterval> {
    // Selected pages always lie in a half-open interval, so page < u64::MAX.
    let mut result = intervals.to_vec();
    result.push(PageInterval {
        start: page,
        end: page + 1,
    });
    result.sort_unstable_by_key(|i| i.start);
    let mut merged: Vec<PageInterval> = Vec::with_capacity(result.len());
    for interval in result {
        if let Some(last) = merged.last_mut()
            && last.end >= interval.start
        {
            last.end = last.end.max(interval.end);
            continue;
        }
        merged.push(interval);
    }
    merged
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::{CacheId, CacheKey, ObjectId, StrongEtag};

    fn subscription(id: u8, start: u64, end: u64) -> Subscription {
        Subscription {
            id: [id; 16],
            sequence: 0,
            page_budget: 10,
            byte_budget: 1024,
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
        assert!(scheduler.state.lock().unwrap().flights.is_empty());
    }
    #[test]
    fn budgets_sequences_deadlines_membership_and_capacity_are_finite() {
        let scheduler = Arc::new(
            Subscriptions::new(SubscriptionLimits {
                max_entries: 1,
                max_pending: 1,
                max_inflight: 1,
                max_completed_intervals: 1,
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
            scheduler.state.lock().unwrap().flights.len(),
            1,
            "expiry cannot release unfenced acquisition owner"
        );
        drop(waiter);
        drop(work);
        assert!(scheduler.state.lock().unwrap().flights.is_empty());
        assert!(matches!(
            scheduler.schedule(sub, MembershipVersion(0), NodeId("a".into()), 2000, 1001),
            Err(Error::InvalidRequest)
        ));
    }
    #[test]
    fn completed_intervals_merge_with_bounded_state() {
        let mut completed = Vec::new();
        for page in [4, 2, 3, 1, 0] {
            completed = include_page(&completed, page);
        }
        assert_eq!(completed, vec![PageInterval { start: 0, end: 5 }]);
        assert_eq!(
            outstanding(&subscription(1, 0, 10).demand, &completed),
            vec![PageInterval { start: 5, end: 10 }]
        );
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
        assert!(scheduler.state.lock().unwrap().flights.is_empty());
    }
    #[test]
    fn successful_fanout_shares_allocation_and_charges_each_receiver_once() {
        use crate::{
            memory::pool::BufferPool,
            model::{
                envelope::{KeyId, Nonce, PageEnvelope},
                limits::ResourceClass,
                metadata::ExpiresAt,
            },
            runtime::admission::Admission,
        };
        use std::{rc::Rc, time::UNIX_EPOCH};
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
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
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
                    page: page.clone(),
                    key_id: KeyId([1; 16]),
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
                    expires_at: ExpiresAt(UNIX_EPOCH),
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
            assert!(state.flights.is_empty());
            assert!(state.pending.is_empty());
        }
    }
}
