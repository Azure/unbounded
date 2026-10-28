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
pub use racer_core::subscriptions::MAX_SUBSCRIPTION_FLIGHTS;

pub use flow_control::subscriptions::Interval as PageInterval;
use flow_control::subscriptions::contains;

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

pub use racer_core::subscriptions::SubscriptionLimits;

/// The sans-IO scheduler this module wraps with a mutex, handles and wakers.
type Core = racer_core::subscriptions::Scheduler<
    MembershipVersion,
    NodeId,
    ObjectVersion,
    (ObjectMetadata, CiphertextPage),
    Error,
>;

type EntryKey = racer_core::subscriptions::EntryKey<MembershipVersion, NodeId>;

type Delivery = racer_core::subscriptions::Delivery<
    (ObjectMetadata, CiphertextPage),
    MembershipVersion,
    NodeId,
    ObjectVersion,
>;

impl racer_core::subscriptions::Fault for Error {
    fn of(kind: racer_core::subscriptions::Kind) -> Self {
        use racer_core::subscriptions::Kind;
        match kind {
            Kind::DeadlineExceeded => Self::DeadlineExceeded,
            Kind::Cancelled => Self::Cancelled,
            Kind::Overloaded => Self::Overloaded,
            Kind::Unavailable => Self::Unavailable,
            Kind::StaleFlight => Self::StaleFlight,
            Kind::Replay => Self::Replay,
            Kind::InvalidRequest => Self::InvalidRequest,
        }
    }

    fn promotes(&self) -> bool {
        matches!(
            self,
            Self::OriginRejected | Self::OriginForbidden | Self::Unauthorized
        )
    }
}

pub struct Subscriptions {
    limits: SubscriptionLimits,

    state: Mutex<State>,
}

struct State {
    core: Core,

    /// Registered notifications of parked response slots, by token.
    wakers: HashMap<u64, Waker>,
}

#[derive(Clone)]
pub struct Completion {
    pub metadata: ObjectMetadata,

    pub ciphertext: CiphertextPage,

    pub grant: TransferGrant,
}

impl From<Delivery> for Completion {
    fn from(delivery: Delivery) -> Self {
        let (metadata, ciphertext) = delivery.payload;
        let grant = delivery.grant;
        Self {
            metadata,
            ciphertext,
            grant: TransferGrant {
                subscription_id: grant.subscription_id,
                sequence: grant.sequence,
                page: PageId {
                    version: grant.version,
                    number: PageNumber(grant.page),
                },
                membership: grant.membership,
                receiver: grant.receiver,
                deadline: grant.deadline,
                remaining_page_budget: grant.remaining_page_budget,
                remaining_byte_budget: grant.remaining_byte_budget,
                flights: grant.flights,
            },
        }
    }
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
        let core = Core::new(limits).ok_or(Error::InvalidConfiguration)?;
        Ok(Self {
            limits,
            state: Mutex::new(State {
                core,
                wakers: HashMap::new(),
            }),
        })
    }

    /// Run one core transition, then invoke the wakers it readied after
    /// releasing the mutex.
    fn with_state<T>(&self, f: impl FnOnce(&mut Core) -> T) -> T {
        self.with_wakers(|state| f(&mut state.core))
    }

    /// As `with_state`, with access to the registered wakers.
    fn with_wakers<T>(&self, f: impl FnOnce(&mut State) -> T) -> T {
        let (result, wake) = {
            let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
            let result = f(&mut state);
            let State { core, wakers } = &mut *state;
            let wake: Vec<Waker> = core
                .take_wakes()
                .into_iter()
                .filter_map(|token| wakers.remove(&token))
                .collect();
            (result, wake)
        };
        for waker in wake {
            waker.wake();
        }
        result
    }

    #[cfg(test)]
    pub fn prune(&self, now_ms: u64) {
        self.with_state(|core| core.advance(now_ms));
    }

    #[cfg(test)]
    fn inspect<T>(&self, f: impl FnOnce(&Core) -> T) -> T {
        f(&self.state.lock().unwrap().core)
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

    /// Validate the authenticated fields, then admit the request on its
    /// contract (`racer_core::subscriptions::Scheduler::admit`).
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
        let contract = racer_core::subscriptions::Contract {
            id: subscription.id,
            version: subscription.version,
            demand: subscription.demand.0,
            sequence: subscription.sequence,
            page_budget: subscription.page_budget,
            byte_budget: subscription.byte_budget,
            flights: subscription.flights,
        };
        self.with_state(|core| core.admit(contract, membership, receiver, deadline, now_ms))
    }

    /// Drop an admitted request that will not select. Its sequence stays in
    /// the replay window.
    fn release(&self, key: &(EntryKey, u64)) {
        self.with_state(|core| core.release(key));
    }

    /// Select one page for an admitted request. An unknown eligibility endpoint
    /// is returned without reserving a flight. The caller computes it outside
    /// the lock, then repeats this transaction.
    fn select(
        self: &Arc<Self>,
        key: &(EntryKey, u64),
        now_ms: u64,
        eligible: impl Fn(u64) -> Option<bool>,
    ) -> Result<std::result::Result<Selection, u64>> {
        let selected = self.with_state(|core| core.select(key, now_ms, eligible))?;
        let selected = match selected {
            Ok(selected) => selected,
            Err(page) => return Ok(Err(page)),
        };
        let waiter = Waiter {
            scheduler: self.clone(),
            token: Some(selected.waiter),
        };
        Ok(Ok(match selected.work {
            None => Selection::Follower(waiter),
            Some(flight) => Selection::Leader {
                work: Work {
                    scheduler: self.clone(),
                    token: Some(flight),
                    page: PageId {
                        version: selected.version,
                        number: PageNumber(selected.page),
                    },
                },
                waiter,
            },
        }))
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
        let result = valid.map(|()| {
            let bytes = copy.ciphertext.bytes().len() as u64;
            ((copy.metadata.clone(), copy.ciphertext.clone()), bytes)
        });
        let token = self.token.take().expect("owned work");
        self.scheduler
            .with_state(|core| core.complete(token, result, Some(now_ms)));
        valid
    }

    pub fn fail(mut self, error: Error) {
        let token = self.token.take().expect("owned work");
        self.scheduler
            .with_state(|core| core.complete(token, Err(error), None));
    }
}

impl Drop for Work {
    fn drop(&mut self) {
        if let Some(token) = self.token.take() {
            self.scheduler.with_state(|core| core.cancel_work(token));
        }
    }
}

impl Waiter {
    /// Called before polling the response. Only credential-supplier failure can
    /// transfer acquisition ownership, and only to one still-live waiter.
    pub fn take_work(&mut self, now_ms: u64) -> Option<Work> {
        let token = self.token?;
        let (flight, version, page) = self
            .scheduler
            .with_state(|core| core.take_work(token, now_ms))?;
        Some(Work {
            scheduler: self.scheduler.clone(),
            token: Some(flight),
            page: PageId {
                version,
                number: PageNumber(page),
            },
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
        use racer_core::subscriptions::Polled;
        let waker = cx.waker();
        let Some(token) = self.token else {
            return Poll::Ready(Err(Error::StaleFlight));
        };
        // Register under the same lock as the poll, so no wake is lost.
        let result = self
            .scheduler
            .with_wakers(|state| match state.core.poll(token, now_ms) {
                Polled::Ready(result) => {
                    state.wakers.remove(&token);
                    Some(result)
                }
                Polled::Pending => {
                    let slot = state.wakers.entry(token).or_insert_with(|| waker.clone());
                    if !slot.will_wake(waker) {
                        *slot = waker.clone();
                    }
                    None
                }
            });
        let Some(result) = result else {
            return Poll::Pending;
        };
        self.token = None;
        Poll::Ready(result.map(Completion::from))
    }
}

impl Drop for Waiter {
    fn drop(&mut self) {
        if let Some(token) = self.token.take() {
            self.scheduler.with_wakers(|state| {
                state.wakers.remove(&token);
                state.core.cancel_waiter(token);
            });
        }
    }
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
        assert_eq!(scheduler.inspect(|c| c.next_prune()), 101);
        scheduler.prune(60_049);
        scheduler.prune(60_050);
        assert_eq!(scheduler.inspect(|c| c.entry_count()), 1);
        scheduler.prune(60_148);
        assert_eq!(scheduler.inspect(|c| c.entry_count()), 1);
        scheduler.prune(60_149);
        assert_eq!(scheduler.inspect(|c| c.entry_count()), 0);
        scheduler.prune(0);
        assert_eq!(scheduler.inspect(|c| c.now()), 60_149);
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
            scheduler.inspect(|c| c.entry(&key.0).unwrap().page_budget),
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
            scheduler.inspect(|c| c.entry(&key.0).unwrap().page_budget),
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
            scheduler.inspect(|c| c.entry(&key.0).unwrap().page_budget),
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
        assert_eq!(scheduler.inspect(|c| c.flight_count()), 0);
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
            scheduler.inspect(|c| c.flight_count()),
            1,
            "expiry cannot release unfenced acquisition owner"
        );
        drop(waiter);
        drop(work);
        assert_eq!(scheduler.inspect(|c| c.flight_count()), 0);
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
        assert_eq!(scheduler.inspect(|c| c.entry_count()), 0);
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
        assert!(scheduler.inspect(|c| c.entries().all(|e| e.page_budget == 9)));
        last.fail(Error::Unavailable);
        assert!(matches!(third.try_result(2), Err(Error::Unavailable)));
        assert_eq!(scheduler.inspect(|c| c.flight_count()), 0);
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
            assert_eq!(scheduler.inspect(|c| c.flight_count()), 0);
            assert_eq!(scheduler.inspect(|c| c.pending_count()), 0);
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

    /// Mirror of the requester check in `forwarding::response_matches`.
    fn requester_accepts(requested: Option<u16>, granted: Option<u16>) -> bool {
        granted.is_some() == requested.is_some() && granted <= requested
    }

    #[test]
    fn late_selection_grant_is_capped_at_its_own_request() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let (_newest, _newest_waiter) = leader(schedule(&scheduler, multi(1, 5, 3), "a"));
        let late = multi(1, 4, 1);
        let (work, mut waiter) = leader(schedule(&scheduler, late.clone(), "a"));
        let _admission = complete(work);
        let grant = waiter.try_result(2).unwrap().unwrap().grant;
        assert_eq!(grant.sequence, 4);
        assert_eq!(grant.flights, Some(1));
        assert!(requester_accepts(late.flights, grant.flights));
        let key = EntryKey {
            membership: MembershipVersion(1),
            receiver: NodeId("a".into()),
            id: [1; 16],
        };
        // A late older request does not lower the newest request's allowance.
        assert_eq!(scheduler.inspect(|c| c.entry(&key).unwrap().flights), 3);
    }

    #[test]
    fn late_single_flight_request_gets_no_flight_grant() {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let (_newest, _newest_waiter) = leader(schedule(&scheduler, multi(1, 5, 3), "a"));
        let mut late = multi(1, 4, 1);
        late.flights = None;
        let (work, mut waiter) = leader(schedule(&scheduler, late.clone(), "a"));
        let _admission = complete(work);
        let grant = waiter.try_result(2).unwrap().unwrap().grant;
        assert_eq!(grant.sequence, 4);
        assert_eq!(grant.flights, None);
        assert!(requester_accepts(late.flights, grant.flights));
        assert!(!requester_accepts(Some(1), Some(3)));
        assert!(!requester_accepts(None, Some(1)));
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
            let entry = scheduler.inspect(|c| c.entry(&key).unwrap());
            assert_eq!(entry.page_budget, 7);
            assert_eq!(entry.pending, 3);
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
            scheduler.inspect(|c| c.entry(&key).unwrap().byte_budget),
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
            scheduler.inspect(|c| c.entry(&key).unwrap().page_budget),
            // Sequence 4's ceiling of 6 excludes the refused sequence 3, which
            // was never admitted and keeps its own page until the window
            // moves past it.
            6
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
        assert_eq!(scheduler.inspect(|c| c.entry(&key).unwrap().page_budget), 8);
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

    /// Requester-shaped ceilings: each request pre-debits one page and
    /// `PAGE_BYTES + 16` bytes for every earlier issued selection.
    fn ceiling(sequence: u64, pages: u32, reserves: u64) -> Subscription {
        let mut sub = multi(1, sequence, 2);
        sub.page_budget = pages;
        sub.byte_budget = reserves * (crate::model::PAGE_BYTES + 16);
        sub
    }

    fn contract(scheduler: &Subscriptions) -> (u32, u64) {
        let entry = scheduler.inspect(|c| c.entries().next().unwrap());
        (entry.page_budget, entry.byte_budget)
    }

    #[test]
    fn multi_flight_shrinking_ceilings_do_not_charge_outstanding_flights_twice() {
        const RESERVE: u64 = crate::model::PAGE_BYTES + 16;
        // Both newer ceilings arrive before the older selection completes.
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let (first_work, mut first) = leader(schedule(&scheduler, ceiling(0, 2, 2), "a"));
        let (second_work, mut second) = leader(schedule(&scheduler, ceiling(1, 1, 1), "a"));
        assert_eq!(contract(&scheduler), (0, 2 * RESERVE));
        let _a = complete(first_work);
        let _b = complete(second_work);
        let a = first.try_result(2).unwrap().unwrap();
        let b = second.try_result(2).unwrap().unwrap();
        assert_eq!(a.grant.remaining_byte_budget, 2 * RESERVE - 19);
        assert_eq!(b.grant.remaining_byte_budget, RESERVE - 19);
        // The first completion is charged its full reservation, never refunded.
        assert_eq!(contract(&scheduler), (0, RESERVE - 19));
        drop((a, b));
        // A request reusing an exhausted ceiling cannot obtain another page.
        assert!(matches!(
            schedule(&scheduler, ceiling(2, 1, 1), "a"),
            Err(Error::Unavailable)
        ));

        // An older request admitted but not yet selected keeps its page.
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let admit = |sub| scheduler.admit(sub, MembershipVersion(1), NodeId("a".into()), 1000, 1);
        let zero = admit(ceiling(0, 2, 2)).unwrap();
        let one = admit(ceiling(1, 1, 1)).unwrap();
        assert_eq!(contract(&scheduler), (2, 2 * RESERVE));
        let one = leader(
            scheduler
                .select(&one, 1, |_| Some(true))
                .map(|s| s.unwrap()),
        );
        let zero = leader(
            scheduler
                .select(&zero, 1, |_| Some(true))
                .map(|s| s.unwrap()),
        );
        assert_eq!(contract(&scheduler).0, 0);
        let (one_work, mut one_waiter) = one;
        let (zero_work, mut zero_waiter) = zero;
        let _a = complete(one_work);
        let _b = complete(zero_work);
        assert!(one_waiter.try_result(2).unwrap().is_some());
        assert!(zero_waiter.try_result(2).unwrap().is_some());
        assert_eq!(contract(&scheduler), (0, RESERVE - 19));
    }

    #[test]
    fn multi_flight_unused_widening_is_forfeited() {
        const RESERVE: u64 = crate::model::PAGE_BYTES + 16;
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        // A failed outstanding flight forfeits the bytes set aside for it.
        let (work, waiter) = leader(schedule(&scheduler, ceiling(0, 3, 3), "a"));
        let admit = |sub| scheduler.admit(sub, MembershipVersion(1), NodeId("a".into()), 1000, 1);
        let one = admit(ceiling(1, 2, 2)).unwrap();
        assert_eq!(contract(&scheduler), (2, 3 * RESERVE));
        drop((work, waiter));
        assert_eq!(contract(&scheduler), (2, 2 * RESERVE));
        // An admitted request that never selects forfeits its page and bytes,
        // so the contract falls to the newest ceiling.
        let two = admit(ceiling(2, 1, 1)).unwrap();
        assert_eq!(contract(&scheduler), (2, 2 * RESERVE));
        scheduler.release(&one);
        assert_eq!(contract(&scheduler), (1, RESERVE));
        drop(two);
    }

    /// Three-flight contract whose sequence 0 already completed and was
    /// consumed, leaving `(pages, bytes)` on the contract.
    fn late_contract(pages: u32, bytes: u64) -> Arc<Subscriptions> {
        let scheduler = Arc::new(Subscriptions::new(Default::default()).unwrap());
        let mut zero = multi(1, 0, 3);
        zero.page_budget = pages;
        zero.byte_budget = bytes;
        let (work, mut waiter) = leader(schedule(&scheduler, zero, "a"));
        let _admission = complete(work);
        drop(waiter.try_result(2).unwrap().unwrap());
        scheduler
    }

    fn late(sequence: u64, pages: u32, bytes: u64) -> Subscription {
        let mut sub = multi(1, sequence, 3);
        sub.page_budget = pages;
        sub.byte_budget = bytes;
        sub
    }

    fn finish(selection: Result<Selection>) -> Completion {
        let (work, mut waiter) = leader(selection);
        let _admission = complete(work);
        waiter.try_result(2).unwrap().unwrap()
    }

    #[test]
    fn multi_flight_late_sequence_is_funded_from_its_own_allowance() {
        const RESERVE: u64 = crate::model::PAGE_BYTES + 16;
        // Pages: sequence 2's ceiling excluded sequence 1's page.
        let scheduler = late_contract(3, 3 * RESERVE);
        assert_eq!(contract(&scheduler), (2, 3 * RESERVE - 19));
        let two = finish(schedule(&scheduler, late(2, 1, RESERVE), "a"));
        assert_eq!(contract(&scheduler).0, 1);
        let one = finish(schedule(&scheduler, late(1, 2, 2 * RESERVE), "a"));
        assert_eq!((one.grant.sequence, two.grant.sequence), (1, 2));
        // The late completion is charged its full reservation.
        assert_eq!(contract(&scheduler), (0, RESERVE - 19));
        assert!(matches!(
            schedule(&scheduler, late(1, 2, 2 * RESERVE), "a"),
            Err(Error::Replay)
        ));
        assert_eq!(contract(&scheduler), (0, RESERVE - 19));

        // Bytes: sequence 2's ceiling leaves exactly one page of bytes.
        let scheduler = late_contract(10, 3 * RESERVE);
        finish(schedule(&scheduler, late(2, 8, 19), "a"));
        assert_eq!(contract(&scheduler), (8, RESERVE));
        finish(schedule(&scheduler, late(1, 9, RESERVE + 19), "a"));
        assert_eq!(contract(&scheduler), (7, 0));
    }

    #[test]
    fn multi_flight_late_allowance_is_funded_once_and_forfeited() {
        const RESERVE: u64 = crate::model::PAGE_BYTES + 16;
        // A late request that never selects forfeits its allowance.
        let scheduler = late_contract(3, 3 * RESERVE);
        let admit = |sub| scheduler.admit(sub, MembershipVersion(1), NodeId("a".into()), 1000, 1);
        let two = admit(late(2, 1, RESERVE)).unwrap();
        assert_eq!(contract(&scheduler), (2, 2 * RESERVE));
        let one = admit(late(1, 2, 2 * RESERVE)).unwrap();
        scheduler.release(&one);
        assert_eq!(contract(&scheduler), (1, RESERVE));
        assert!(matches!(admit(late(1, 2, 2 * RESERVE)), Err(Error::Replay)));
        assert_eq!(contract(&scheduler), (1, RESERVE));
        drop(two);

        // A newer ceiling that moves the window past an unseen sequence
        // forfeits its allowance; the stale sequence is a replay.
        let scheduler = late_contract(5, 5 * RESERVE);
        let admit = |sub| scheduler.admit(sub, MembershipVersion(1), NodeId("a".into()), 1000, 1);
        let two = admit(late(2, 2, 2 * RESERVE)).unwrap();
        assert_eq!(contract(&scheduler).0, 3);
        scheduler.release(&two);
        // Sequence 4 excludes 1, 2 and 3; only the unseen 3 is in its window.
        let four = admit(late(4, 0, 0)).unwrap();
        assert_eq!(contract(&scheduler), (1, RESERVE));
        assert!(matches!(admit(late(1, 3, 3 * RESERVE)), Err(Error::Replay)));
        assert!(matches!(admit(late(2, 2, 2 * RESERVE)), Err(Error::Replay)));
        let three = admit(late(3, 1, RESERVE)).unwrap();
        assert_eq!(contract(&scheduler), (1, RESERVE));
        drop((three, four));
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
