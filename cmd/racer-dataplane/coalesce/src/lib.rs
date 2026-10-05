//! Worker-local keyed cohorts with bounded registration and leader re-election.
//!
//! Callers own execution, cancellation, admission policy, and result semantics.
//! A cloned registration retains the same waiter and leadership until its final
//! handle drops. Complete a cohort only after the real operation has finished.

pub mod flight;

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, hash_map::RandomState};
use std::hash::{BuildHasher, Hash};
use std::rc::Rc;
use std::task::{Poll, Waker};

/// Local bounds. Zero waiters rejects all joins; zero attempts immediately
/// broadcasts the caller-supplied exhausted value without electing a leader.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    /// Maximum distinct registrations sharing one cohort.
    pub waiters_per_cohort: usize,

    /// Maximum leadership elections before broadcasting exhaustion.
    pub attempts_per_cohort: usize,
}

/// Admission would exceed a key, waiter, or identifier bound.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CapacityError;

/// A registration may start an attempt or observe its cohort's result.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Event<V> {
    /// This registration owns the next attempt.
    Lead,
    /// The operation finished or the attempt budget was exhausted.
    Complete(V),
}

/// Election state shared by distinct registrations for the same key.
struct Cohort<V, S> {
    next: u64,

    leader: Option<u64>,

    attempts: usize,

    waiters: HashMap<u64, Option<Waker>, S>,

    result: Option<V>,
}

impl<V, S: Default> Default for Cohort<V, S> {
    /// Start with no waiters, leadership, attempts, or published result.
    fn default() -> Self {
        Self {
            next: 0,
            leader: None,
            attempts: 0,
            waiters: HashMap::default(),
            result: None,
        }
    }
}

/// Completed cohorts close admission immediately but continue charging live
/// registrations. `S::default()` is called for each map, including new cohorts.
pub struct Table<K, V, S = RandomState> {
    active: RefCell<HashMap<K, CohortOwner<V, S>, S>>,

    registrations: Cell<usize>,

    limits: Limits,

    exhausted: V,
}

/// Local cohort ownership shared between the admission index and registrations.
type CohortOwner<V, S> = Rc<RefCell<Cohort<V, S>>>;

impl<K, V, S: Default> Table<K, V, S> {
    /// Create an empty worker-local table and its attempt-exhaustion result.
    pub fn new(limits: Limits, exhausted: V) -> Self {
        Self {
            active: RefCell::new(HashMap::default()),
            registrations: Cell::new(0),
            limits,
            exhausted,
        }
    }
}

impl<K, V, S> Table<K, V, S> {
    /// Count distinct registrations, including readers of completed cohorts.
    pub fn registration_count(&self) -> usize {
        self.registrations.get()
    }

    /// Count cohorts still accepting new registrations.
    pub fn active_count(&self) -> usize {
        self.active.borrow().len()
    }
}

impl<K: Clone + Eq + Hash, V, S: BuildHasher + Default> Table<K, V, S> {
    /// Capacity bounds active keys and, multiplied by the waiter limit, all live
    /// registrations (including readers of completed cohorts). Clones count once.
    pub fn join(
        self: &Rc<Self>,
        key: K,
        capacity: usize,
    ) -> Result<Registration<K, V, S>, CapacityError> {
        if self.registrations.get() >= capacity.saturating_mul(self.limits.waiters_per_cohort) {
            return Err(CapacityError);
        }
        let mut active = self.active.borrow_mut();
        let cohort = if let Some(cohort) = active.get(&key) {
            cohort.clone()
        } else {
            if active.len() >= capacity {
                return Err(CapacityError);
            }
            let cohort = Rc::new(RefCell::new(Cohort::default()));
            active.insert(key.clone(), cohort.clone());
            cohort
        };
        let id = {
            let mut state = cohort.borrow_mut();
            if state.waiters.len() >= self.limits.waiters_per_cohort {
                return Err(CapacityError);
            }
            let id = state.next;
            state.next = state.next.checked_add(1).ok_or(CapacityError)?;
            state.waiters.insert(id, None);
            id
        };
        self.registrations.set(self.registrations.get() + 1);
        Ok(Registration {
            owner: Rc::new(RegistrationOwner {
                table: self.clone(),
                key,
                cohort,
                id,
            }),
        })
    }
}

/// A worker-local handle to one waiter. Clones retain its leadership and charge;
/// only dropping the last handle detaches it. Cloning never clones the key.
/// Handles cannot move or be shared across workers:
/// ```compile_fail
/// fn require_send<T: Send>() {}
/// require_send::<coalesce::Registration<u32, u32>>();
/// ```
/// ```compile_fail
/// fn require_sync<T: Sync>() {}
/// require_sync::<coalesce::Registration<u32, u32>>();
/// ```
pub struct Registration<K: Eq + Hash, V, S: BuildHasher = RandomState> {
    owner: Rc<RegistrationOwner<K, V, S>>,
}

/// Owns exactly one registration charge and releases it once on final Rc drop.
struct RegistrationOwner<K: Eq + Hash, V, S: BuildHasher> {
    table: Rc<Table<K, V, S>>,

    key: K,

    cohort: CohortOwner<V, S>,

    id: u64,
}

impl<K: Eq + Hash, V, S: BuildHasher> Clone for Registration<K, V, S> {
    /// Retain the same waiter owner without duplicating its key or charge.
    fn clone(&self) -> Self {
        Self {
            owner: self.owner.clone(),
        }
    }
}

impl<K: Eq + Hash, V, S: BuildHasher> Registration<K, V, S> {
    /// Whether this is the last handle for this registration, not the cohort.
    pub fn is_only_handle(&self) -> bool {
        Rc::strong_count(&self.owner) == 1
    }

    /// Release leadership and notify the cohort after an attempt completes.
    /// Retry policy and whether this caller should detach belong to the caller.
    pub fn retry(&self) {
        let wakers = {
            let mut state = self.owner.cohort.borrow_mut();
            if state.leader == Some(self.owner.id) {
                state.leader = None;
            }
            take_wakers(&mut state)
        };
        for waker in wakers {
            waker.wake();
        }
    }

    /// Broadcast a caller-owned value and close admission before waking readers.
    /// The operation owner must call this only after completion, not cancellation.
    pub fn finish(&self, result: V) {
        let wakers = {
            let mut state = self.owner.cohort.borrow_mut();
            state.result = Some(result);
            state.leader = None;
            take_wakers(&mut state)
        };
        self.owner.remove_active();
        for waker in wakers {
            waker.wake();
        }
    }
}

impl<K: Eq + Hash, V: Clone, S: BuildHasher> Registration<K, V, S> {
    /// Register the latest wake target and elect at most once per attempt.
    pub fn event(&self, waker: &Waker) -> Poll<Event<V>> {
        let mut state = self.owner.cohort.borrow_mut();
        if let Some(result) = &state.result {
            return Poll::Ready(Event::Complete(result.clone()));
        }
        state.waiters.insert(self.owner.id, Some(waker.clone()));
        if state.leader.is_none() {
            if state.attempts >= self.owner.table.limits.attempts_per_cohort {
                drop(state);
                let result = self.owner.table.exhausted.clone();
                self.finish(result.clone());
                return Poll::Ready(Event::Complete(result));
            }
            state.attempts += 1;
            state.leader = Some(self.owner.id);
            return Poll::Ready(Event::Lead);
        }
        Poll::Pending
    }
}

impl<K: Eq + Hash, V, S: BuildHasher> RegistrationOwner<K, V, S> {
    /// Remove only this cohort, never a replacement admitted under the same key.
    fn remove_active(&self) {
        let mut active = self.table.active.borrow_mut();
        if active
            .get(&self.key)
            .is_some_and(|entry| Rc::ptr_eq(entry, &self.cohort))
        {
            active.remove(&self.key);
        }
    }
}

impl<K: Eq + Hash, V, S: BuildHasher> Drop for RegistrationOwner<K, V, S> {
    /// Detach once, release the charge, then wake followers outside all borrows.
    fn drop(&mut self) {
        let (empty, wakers) = {
            let mut state = self.cohort.borrow_mut();
            state.waiters.remove(&self.id);
            let wakers = if state.leader == Some(self.id) {
                state.leader = None;
                take_wakers(&mut state)
            } else {
                Vec::new()
            };
            (state.waiters.is_empty(), wakers)
        };
        self.table
            .registrations
            .set(self.table.registrations.get() - 1);
        if empty {
            self.remove_active();
        }
        for waker in wakers {
            waker.wake();
        }
    }
}

/// Drain wake targets so callbacks can run after releasing the cohort borrow.
fn take_wakers<V, S>(state: &mut Cohort<V, S>) -> Vec<Waker> {
    state
        .waiters
        .values_mut()
        .filter_map(Option::take)
        .collect()
}

/// Shared-result cohorts whose callers own execution rather than electing leaders.
/// Dropping receivers cannot remove real work. Dropping a completion sender
/// closes receivers but keeps its entry until completion is confirmed.
pub mod shared {
    use futures::FutureExt;
    use futures::channel::oneshot;
    use futures::future::{LocalBoxFuture, Shared};
    use std::cell::RefCell;
    use std::collections::BTreeMap;
    use std::rc::Rc;

    /// Cloneable, worker-local result receiver independent of execution ownership.
    pub type Receiver<V> = Shared<LocalBoxFuture<'static, V>>;

    /// Ordered index retaining work until its completion owner explicitly finishes.
    pub struct Table<K, V: Clone> {
        entries: RefCell<BTreeMap<K, Receiver<V>>>,
    }

    impl<K, V: Clone> Default for Table<K, V> {
        /// Create an empty operation index.
        fn default() -> Self {
            Self {
                entries: RefCell::new(BTreeMap::new()),
            }
        }
    }

    impl<K, V: Clone> Table<K, V> {
        /// Count operations, including those whose completion sender was lost.
        pub fn len(&self) -> usize {
            self.entries.borrow().len()
        }

        /// Whether no operation remains indexed.
        pub fn is_empty(&self) -> bool {
            self.entries.borrow().is_empty()
        }
    }

    impl<K: Ord, V: Clone> Table<K, V> {
        /// Join an existing result without taking ownership of its execution.
        pub fn get(&self, key: &K) -> Option<Receiver<V>> {
            self.entries.borrow().get(key).cloned()
        }
    }

    impl<K: Ord + Clone, V: Clone + 'static> Table<K, V> {
        /// Called after miss-only admission, without yielding between get and start.
        /// The caller must not start a replacement while an entry is present.
        pub fn start(self: &Rc<Self>, key: K, closed: V) -> (Receiver<V>, Completion<K, V>) {
            let (send, receive) = oneshot::channel();
            let receive = async move { receive.await.unwrap_or(closed) }
                .boxed_local()
                .shared();
            self.entries
                .borrow_mut()
                .insert(key.clone(), receive.clone());
            (
                receive,
                Completion {
                    table: self.clone(),
                    key,
                    send,
                },
            )
        }
    }

    /// Sole completion authority. Dropping it reports closure, not real completion,
    /// so its table entry deliberately remains occupied.
    pub struct Completion<K, V: Clone> {
        table: Rc<Table<K, V>>,

        key: K,

        send: oneshot::Sender<V>,
    }

    impl<K: Ord, V: Clone> Completion<K, V> {
        /// Real completion closes admission before notifying any old readers.
        pub fn finish(self, value: V) {
            self.table.entries.borrow_mut().remove(&self.key);
            let _ = self.send.send(value);
        }
    }

    /// Shared-result ownership and replacement contracts.
    #[cfg(test)]
    mod tests {
        use super::*;
        use futures::executor::block_on;

        /// Results remain readable after completion admits a replacement.
        #[test]
        fn success_miss_failure_broadcast_and_replacement() {
            for value in [Ok(Some(7)), Ok(None), Err("io")] {
                let table = Rc::new(Table::default());
                let (first, completion) = table.start(1, Err("closed"));
                let second = table.get(&1).unwrap();
                assert_eq!(table.len(), 1);
                completion.finish(value);
                assert!(table.is_empty());
                let (next, completion) = table.start(1, Err("closed"));
                assert_eq!(block_on(first), value);
                assert_eq!(block_on(second), value);
                assert_eq!(table.len(), 1);
                completion.finish(Ok(Some(8)));
                assert_eq!(block_on(next), Ok(Some(8)));
            }
        }

        /// Neither receiver cancellation nor sender loss pretends work completed.
        #[test]
        fn all_readers_drop_keeps_completion_owner_and_lost_sender_stays_closed() {
            let table = Rc::new(Table::default());
            let (receive, completion) = table.start(1, Err::<u32, _>("closed"));
            drop(receive);
            assert_eq!(table.len(), 1);
            let late = table.get(&1).unwrap();
            completion.finish(Ok(9));
            assert_eq!(block_on(late), Ok(9));
            assert!(table.is_empty());
            let (receive, completion) = table.start(1, Err("closed"));
            drop(completion);
            assert_eq!(block_on(receive), Err("closed"));
            assert_eq!(block_on(table.get(&1).unwrap()), Err("closed"));
            assert_eq!(table.len(), 1);
        }
    }
}

/// Election, capacity, and final-handle cleanup contracts.
#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::Wake;

    /// Counts delivered notifications without running an executor.
    #[derive(Default)]
    struct Counter(AtomicUsize);
    impl Wake for Counter {
        /// Record one notification.
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    /// Small keyed table used by the cohort scenarios.
    type TestTable = Table<&'static str, Result<u32, &'static str>>;

    /// Construct a table with independently configurable waiter and attempt bounds.
    fn table(waiters: usize, attempts: usize) -> Rc<TestTable> {
        Rc::new(Table::new(
            Limits {
                waiters_per_cohort: waiters,
                attempts_per_cohort: attempts,
            },
            Err("exhausted"),
        ))
    }

    /// Completion reaches both parked and unpolled readers without deleting replacements.
    #[test]
    fn broadcasts_success_and_failure_before_or_after_poll() {
        for result in [Ok(42), Err("failed")] {
            for poll_first in [false, true] {
                let table = table(4, 2);
                let leader = table.join("key", 1).unwrap();
                let follower = table.join("key", 1).unwrap();
                let count = Arc::new(Counter::default());
                let waker = Waker::from(count.clone());
                assert_eq!(leader.event(Waker::noop()), Poll::Ready(Event::Lead));
                if poll_first {
                    assert_eq!(follower.event(&waker), Poll::Pending);
                }
                leader.finish(result);
                assert_eq!(count.0.load(Ordering::Relaxed), usize::from(poll_first));
                assert_eq!(follower.event(&waker), Poll::Ready(Event::Complete(result)));
                assert_eq!(table.active_count(), 0);
                let next = table.join("key", 1).unwrap();
                assert_eq!(next.event(Waker::noop()), Poll::Ready(Event::Lead));
                drop(leader);
                drop(follower);
                assert_eq!(table.active_count(), 1, "old cohort cannot remove new key");
            }
        }
    }

    /// Detached execution retains one waiter until its final handle is dropped.
    #[test]
    fn last_handle_drop_releases_leadership_and_registration() {
        let table = table(2, 3);
        let request = table.join("key", 1).unwrap();
        let follower = table.join("key", 1).unwrap();
        assert_eq!(request.event(Waker::noop()), Poll::Ready(Event::Lead));
        let driver = request.clone();
        assert!(!driver.is_only_handle());
        drop(request);
        assert!(driver.is_only_handle());
        assert_eq!(table.registration_count(), 2);
        let old = Arc::new(Counter::default());
        let latest = Arc::new(Counter::default());
        assert!(follower.event(&Waker::from(old.clone())).is_pending());
        assert!(follower.event(&Waker::from(latest.clone())).is_pending());
        drop(driver);
        assert_eq!(old.0.load(Ordering::Relaxed), 0);
        assert_eq!(latest.0.load(Ordering::Relaxed), 1);
        assert_eq!(table.registration_count(), 1);
        assert_eq!(follower.event(Waker::noop()), Poll::Ready(Event::Lead));
        drop(follower);
        assert_eq!(table.registration_count(), 0);
        assert_eq!(table.active_count(), 0);
    }

    /// Retry and owner loss consume the same bounded election budget.
    #[test]
    fn retry_and_drop_share_bounded_attempts_and_broadcast_exhaustion() {
        let table = table(3, 2);
        let first = table.join("key", 1).unwrap();
        let second = table.join("key", 1).unwrap();
        let observer = table.join("key", 1).unwrap();
        assert_eq!(first.event(Waker::noop()), Poll::Ready(Event::Lead));
        assert!(first.event(Waker::noop()).is_pending());
        first.retry();
        assert_eq!(second.event(Waker::noop()), Poll::Ready(Event::Lead));
        drop(second);
        let exhausted = Poll::Ready(Event::Complete(Err("exhausted")));
        assert_eq!(observer.event(Waker::noop()), exhausted);
        assert_eq!(first.event(Waker::noop()), exhausted);
        assert_eq!(table.active_count(), 0);
    }

    /// Completed readers still consume global capacity until they detach.
    #[test]
    fn capacity_bounds_keys_waiters_and_completed_readers() {
        let table = table(2, 1);
        assert!(matches!(table.join("a", 0), Err(CapacityError)));
        let a = table.join("a", 2).unwrap();
        let b = table.join("b", 2).unwrap();
        assert!(matches!(table.join("c", 2), Err(CapacityError)));
        let a2 = table.join("a", 2).unwrap();
        assert!(matches!(table.join("a", 2), Err(CapacityError)));
        let b2 = table.join("b", 2).unwrap();
        a.finish(Ok(1));
        b.finish(Ok(2));
        assert_eq!(table.active_count(), 0);
        assert!(matches!(table.join("a", 2), Err(CapacityError)));
        drop(a2);
        let next = table.join("a", 2).unwrap();
        drop((a, b, b2, next));
        assert_eq!(table.registration_count(), 0);
        assert_eq!(table.active_count(), 0);
    }

    /// Zero bounds and identifier exhaustion reject work without leaking charges.
    #[test]
    fn zero_limits_and_id_exhaustion_never_wrap_or_leak_registrations() {
        assert!(matches!(table(0, 1).join("key", 1), Err(CapacityError)));
        let zero = table(1, 0);
        let waiter = zero.join("key", 1).unwrap();
        assert_eq!(
            waiter.event(Waker::noop()),
            Poll::Ready(Event::Complete(Err("exhausted")))
        );
        let table = table(2, 1);
        let first = table.join("key", usize::MAX).unwrap();
        first.owner.cohort.borrow_mut().next = u64::MAX;
        assert!(matches!(table.join("key", usize::MAX), Err(CapacityError)));
        assert_eq!(table.registration_count(), 1);
        drop(first);
        assert_eq!(table.active_count(), 0);
    }
}
