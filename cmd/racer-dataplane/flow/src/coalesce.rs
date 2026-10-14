//! Worker-local keyed cohorts with bounded registration and leader re-election.
//!
//! Callers own execution, cancellation, admission policy, and result semantics.
//! A cloned registration retains the same waiter and leadership until its final
//! handle drops. Complete a cohort only after the real operation has finished.
//!
//! This small cohort API has no request budget or cancellation token. Before
//! polling for leadership, the adapter must check its request's eligibility.
//! The attempt driver must retain a clone of the elected registration until real
//! work ends, even if the request cancels. Only that driver may call `retry` or
//! `finish`; followers must not publish a terminal result. This is a caller-owned
//! protocol, not a capability enforced by the registration type. Use `flight`
//! when operation ownership and generation validation need separate state.

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
    /// Canceled requests do not refund elections. A retained driver prevents
    /// re-election and exhaustion until its actual attempt ends.
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

/// Completed cohorts close admission immediately but continue charging live
/// registrations. `S::default()` is called for each map, including new cohorts.
/// ```compile_fail,E0277
/// fn require_send<T: Send>() {}
/// require_send::<flow_control::coalesce::Table<u32, u32>>();
/// ```
/// ```compile_fail,E0277
/// fn require_sync<T: Sync>() {}
/// require_sync::<flow_control::coalesce::Table<u32, u32>>();
/// ```
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
        if self.limits.waiters_per_cohort == 0 {
            return Err(CapacityError);
        }
        if self.registrations.get() >= capacity.saturating_mul(self.limits.waiters_per_cohort) {
            return Err(CapacityError);
        }
        let index_key = {
            let active = self.active.borrow();
            if active.contains_key(&key) {
                None
            } else {
                if active.len() >= capacity {
                    return Err(CapacityError);
                }
                drop(active);
                Some(key.clone())
            }
        };
        // Key cloning can admit work. Recheck all bounds and cohort membership.
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
            active.insert(
                index_key.expect("missing cohort key was cloned"),
                cohort.clone(),
            );
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
/// ```compile_fail,E0277
/// fn require_send<T: Send>() {}
/// require_send::<flow_control::coalesce::Registration<u32, u32>>();
/// ```
/// ```compile_fail,E0277
/// fn require_sync<T: Sync>() {}
/// require_sync::<flow_control::coalesce::Registration<u32, u32>>();
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
            state.take_wakers()
        };
        wake_all(wakers);
    }

    /// Broadcast a caller-owned value and close admission before waking readers.
    /// The operation owner must call this only after completion, not cancellation.
    /// This method intentionally does not check leader identity: adapters may also
    /// publish a cached or externally completed result before an election. They must
    /// ensure a follower never uses it to cancel or replace the driver's real work.
    /// The first result is final; later calls leave it unchanged.
    pub fn finish(&self, result: V) {
        let wakers = {
            let mut state = self.owner.cohort.borrow_mut();
            if state.result.is_some() {
                return;
            }
            state.result = Some(Rc::new(result));
            state.leader = None;
            state.take_wakers()
        };
        self.owner.remove_active();
        wake_all(wakers);
    }
}

impl<K: Eq + Hash, V: Clone, S: BuildHasher> Registration<K, V, S> {
    /// Register the latest wake target and elect at most once per attempt.
    pub fn event(&self, waker: &Waker) -> Poll<Event<V>> {
        let waker = waker.clone();
        let retired = {
            let mut state = self.owner.cohort.borrow_mut();
            if state.result.is_some() {
                Some(waker)
            } else {
                state.waiters.insert(self.owner.id, Some(waker)).flatten()
            }
        };
        drop(retired);

        // Clone and drop callbacks can retry, elect, or finish. Decide from the
        // current state only after those callbacks have released their borrows.
        let mut state = self.owner.cohort.borrow_mut();
        if let Some(result) = state.result.clone() {
            // Retain the value without calling application code under the borrow.
            drop(state);
            return Poll::Ready(Event::Complete((*result).clone()));
        }
        if state.leader.is_none() {
            if state.attempts >= self.owner.table.limits.attempts_per_cohort {
                drop(state);
                self.finish(self.owner.table.exhausted.clone());
                // Cloning exhaustion can publish another result first.
                let result = self.owner.cohort.borrow().result.clone().unwrap();
                return Poll::Ready(Event::Complete((*result).clone()));
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
        let removed = {
            let mut active = self.table.active.borrow_mut();
            if active
                .get(&self.key)
                .is_some_and(|entry| Rc::ptr_eq(entry, &self.cohort))
            {
                active.remove_entry(&self.key)
            } else {
                None
            }
        };
        // Both the key and cohort may own application destructors.
        drop(removed);
    }
}

impl<K: Eq + Hash, V, S: BuildHasher> Drop for RegistrationOwner<K, V, S> {
    /// Detach once, release the charge, then wake followers outside all borrows.
    fn drop(&mut self) {
        let (empty, retired, wakers) = {
            let mut state = self.cohort.borrow_mut();
            let retired = state.waiters.remove(&self.id);
            let wakers = if state.leader == Some(self.id) {
                state.leader = None;
                state.take_wakers()
            } else {
                Vec::new()
            };
            (state.waiters.is_empty(), retired, wakers)
        };
        self.table
            .registrations
            .set(self.table.registrations.get() - 1);
        if empty {
            self.remove_active();
        }
        drop(retired);
        wake_all(wakers);
    }
}

/// Election state shared by distinct registrations for the same key.
struct Cohort<V, S> {
    next: u64,

    leader: Option<u64>,

    attempts: usize,

    waiters: HashMap<u64, Option<Waker>, S>,

    result: Option<Rc<V>>,
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

impl<V, S> Cohort<V, S> {
    /// Drain wake targets so callbacks can run after releasing the cohort borrow.
    fn take_wakers(&mut self) -> Vec<Waker> {
        self.waiters.values_mut().filter_map(Option::take).collect()
    }
}

/// Deliver collected notifications only after the caller releases owner borrows.
fn wake_all(wakers: Vec<Waker>) {
    for waker in wakers {
        waker.wake();
    }
}

/// Shared-result cohorts whose callers own execution rather than electing leaders.
/// Dropping receivers cannot remove real work. Dropping a completion sender
/// closes receivers but keeps its entry until completion is confirmed.
/// Supervisors can discover these entries with [`shared::Table::lost_keys`], then
/// use [`shared::Table::evict_lost`] after confirming the real work has ended.
pub mod shared {
    use futures::FutureExt;
    use futures::channel::oneshot;
    use futures::future::{LocalBoxFuture, Shared};
    use std::cell::{Cell, RefCell};
    use std::collections::BTreeMap;
    use std::rc::Rc;

    /// Cloneable, worker-local result receiver independent of execution ownership.
    pub type Receiver<V> = Shared<LocalBoxFuture<'static, V>>;

    /// Ordered index retaining work until its completion owner explicitly finishes.
    /// ```compile_fail,E0277
    /// fn require_send<T: Send>() {}
    /// require_send::<flow_control::coalesce::shared::Table<u32, u32>>();
    /// ```
    /// ```compile_fail,E0277
    /// fn require_sync<T: Sync>() {}
    /// require_sync::<flow_control::coalesce::shared::Table<u32, u32>>();
    /// ```
    pub struct Table<K, V: Clone> {
        entries: RefCell<BTreeMap<Rc<K>, Indexed<V>>>,

        lost_completions: Cell<u64>,
    }

    /// Receiver and completion-owner liveness for one admission.
    struct Indexed<V: Clone> {
        receive: Receiver<V>,

        live: Rc<Cell<bool>>,
    }

    impl<K, V: Clone> Default for Table<K, V> {
        /// Create an empty operation index.
        fn default() -> Self {
            Self {
                entries: RefCell::new(BTreeMap::new()),
                lost_completions: Cell::new(0),
            }
        }
    }

    impl<K, V: Clone> Table<K, V> {
        /// Count all lost completion owners, including evicted entries, saturating
        /// rather than wrapping. Use [`Self::lost_keys`] to discover retained losses.
        pub fn lost_completion_count(&self) -> u64 {
            self.lost_completions.get()
        }

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
            self.entries
                .borrow()
                .get(key)
                .map(|entry| entry.receive.clone())
        }

        /// Recover a lost-owner entry only after independently confirming real work
        /// has finished. Cancellation alone is not sufficient. Live owners cannot be
        /// evicted. Old receivers keep their closed result; new starts get a new slot.
        /// A [`Self::lost_keys`] snapshot is not an eviction permit: coordinate
        /// recovery with other supervisors and confirm work for the current entry,
        /// not an earlier operation under the same key.
        pub fn evict_lost(&self, key: &K) -> bool {
            let removed = {
                let mut entries = self.entries.borrow_mut();
                if entries.get(key).is_some_and(|entry| !entry.live.get()) {
                    entries.remove_entry(key)
                } else {
                    None
                }
            };
            let evicted = removed.is_some();
            drop(removed);
            evicted
        }
    }

    impl<K: Clone, V: Clone> Table<K, V> {
        /// Snapshot retained lost-owner keys in key order, excluding live owners
        /// and finished entries. This scans the full index but removes nothing;
        /// repeated calls keep reporting a loss until [`Self::evict_lost`] removes it.
        /// Discovery does not allow new work or prove that old work has ended.
        ///
        /// Key clones run outside the table borrow and may reenter or unwind.
        /// The snapshot can become stale, including during cloning. Before eviction,
        /// independently confirm that the current work for the key has ended.
        pub fn lost_keys(&self) -> Vec<K> {
            let keys: Vec<_> = self
                .entries
                .borrow()
                .iter()
                .filter(|(_, entry)| !entry.live.get())
                .map(|(key, _)| key.clone())
                .collect();
            keys.into_iter().map(|key| (*key).clone()).collect()
        }
    }

    impl<K: Ord + Clone, V: Clone + 'static> Table<K, V> {
        /// Called after miss-only admission, without yielding between get and start.
        /// Recheck after key cloning: an occupied key joins without completion
        /// authority. Start work only when a completion owner is returned.
        pub fn start(
            self: &Rc<Self>,
            key: K,
            closed: V,
        ) -> (Receiver<V>, Option<Completion<K, V>>) {
            let index_key = Rc::new(key.clone());
            let (send, receive) = oneshot::channel();
            let receive = async move { receive.await.unwrap_or(closed) }
                .boxed_local()
                .shared();
            let live = Rc::new(Cell::new(true));
            let existing = {
                let mut entries = self.entries.borrow_mut();
                // Keep the unused key and receiver outside this borrow on a join.
                if let Some(existing) = entries.get(&index_key) {
                    Some(existing.receive.clone())
                } else {
                    entries.insert(
                        index_key,
                        Indexed {
                            receive: receive.clone(),
                            live: live.clone(),
                        },
                    );
                    None
                }
            };
            if let Some(existing) = existing {
                return (existing, None);
            }
            (
                receive,
                Some(Completion {
                    table: self.clone(),
                    key,
                    send: Some(send),
                    live,
                }),
            )
        }
    }

    /// Sole completion authority. Dropping it reports closure, not real completion,
    /// so its table entry remains occupied until explicit lost-owner recovery.
    /// ```compile_fail,E0277
    /// fn require_send<T: Send>() {}
    /// require_send::<flow_control::coalesce::shared::Completion<u32, u32>>();
    /// ```
    /// ```compile_fail,E0277
    /// fn require_sync<T: Sync>() {}
    /// require_sync::<flow_control::coalesce::shared::Completion<u32, u32>>();
    /// ```
    pub struct Completion<K, V: Clone> {
        table: Rc<Table<K, V>>,

        key: K,

        send: Option<oneshot::Sender<V>>,

        live: Rc<Cell<bool>>,
    }

    impl<K: Ord, V: Clone> Completion<K, V> {
        /// Real completion closes admission before notifying any old readers.
        pub fn finish(mut self, value: V) {
            let send = self.send.take().expect("completion sender");
            let removed = {
                let mut entries = self.table.entries.borrow_mut();
                if entries
                    .get(&self.key)
                    .is_some_and(|entry| Rc::ptr_eq(&entry.live, &self.live))
                {
                    entries.remove_entry(&self.key)
                } else {
                    None
                }
            };
            // Key and final receiver destruction can reenter the table.
            drop(removed);
            let _ = send.send(value);
        }
    }

    impl<K, V: Clone> Drop for Completion<K, V> {
        /// Record loss before closing readers; neither action implies completion.
        fn drop(&mut self) {
            if self.send.is_some() {
                self.live.set(false);
                self.table
                    .lost_completions
                    .set(self.table.lost_completions.get().saturating_add(1));
            }
        }
    }

    /// Shared-result ownership and replacement contracts.
    #[cfg(test)]
    mod tests {
        use super::*;
        use futures::executor::block_on;

        #[test]
        fn lost_keys_excludes_live_and_finished_entries_without_draining() {
            let table = Rc::new(Table::default());
            assert!(table.lost_keys().is_empty());
            let (live_reader, live) = table.start(2, Err::<u32, _>("closed"));
            drop(live_reader);
            for (key, result) in [(3, Ok(7)), (4, Err("io"))] {
                let (reader, completion) = table.start(key, Err("closed"));
                completion.unwrap().finish(result);
                assert_eq!(block_on(reader), result);
            }
            assert!(table.lost_keys().is_empty());
            for key in [5, 1] {
                let (reader, completion) = table.start(key, Err("closed"));
                drop(completion);
                drop(reader);
            }
            for _ in 0..3 {
                assert_eq!(table.lost_keys(), vec![1, 5]);
                assert_eq!(table.len(), 3);
                assert_eq!(table.lost_completion_count(), 2);
                assert!(!table.evict_lost(&2));
                assert!(!table.evict_lost(&3));
                let (joined, rejected) = table.start(1, Err("other"));
                assert!(rejected.is_none());
                assert_eq!(block_on(joined), Err("closed"));
            }
            assert!(table.evict_lost(&1));
            assert_eq!(table.lost_keys(), vec![5]);
            assert!(table.evict_lost(&5));
            assert!(table.lost_keys().is_empty());
            live.unwrap().finish(Ok(9));
            assert!(table.is_empty());
            assert_eq!(table.lost_completion_count(), 2);
        }

        #[test]
        fn discovered_losses_require_work_confirmation_before_restart() {
            let table = Rc::new(Table::default());
            for losses in 1..=3 {
                let work_running = Cell::new(true);
                let (old, completion) = table.start("page".to_owned(), Err::<u32, _>("closed"));
                assert!(completion.is_some());
                drop(completion);
                let keys = table.lost_keys();
                assert_eq!(keys, vec!["page"]);
                assert_eq!(table.lost_completion_count(), losses);
                assert!(work_running.get());
                let (joined, rejected) = table.start(keys[0].clone(), Err("other"));
                assert!(rejected.is_none());
                assert_eq!(block_on(joined), Err("closed"));
                assert_eq!(table.lost_keys(), keys);

                // The supervisor confirms completion through its own work tracker.
                work_running.set(false);
                assert!(!work_running.get());
                assert!(table.evict_lost(&keys[0]));
                assert!(!table.evict_lost(&keys[0]));
                assert!(table.lost_keys().is_empty());
                let (fresh, completion) = table.start(keys[0].clone(), Err("new closed"));
                assert!(completion.is_some());
                work_running.set(true);
                assert!(table.lost_keys().is_empty());
                assert!(!table.evict_lost(&keys[0]));
                assert_eq!(block_on(old), Err("closed"));
                work_running.set(false);
                completion.unwrap().finish(Ok(7));
                assert_eq!(block_on(fresh), Ok(7));
                assert!(table.lost_keys().is_empty());
                assert!(table.is_empty());
                assert_eq!(table.lost_completion_count(), losses);
            }
        }

        #[test]
        fn lost_keys_clone_can_reenter_or_unwind_without_losing_discovery() {
            use std::panic::{AssertUnwindSafe, catch_unwind};

            thread_local! {
                static ON_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
            }

            #[derive(Debug, Eq, PartialEq, Ord, PartialOrd)]
            struct Key(u32);

            impl Clone for Key {
                fn clone(&self) -> Self {
                    let callback = ON_CLONE.with(|slot| slot.borrow_mut().take());
                    if let Some(callback) = callback {
                        callback();
                    }
                    Self(self.0)
                }
            }

            let table = Rc::new(Table::default());
            for key in [1, 2] {
                let (reader, completion) = table.start(Key(key), 0);
                drop(completion);
                drop(reader);
            }
            ON_CLONE.with(|slot| {
                *slot.borrow_mut() = Some(Box::new(|| panic!("snapshot clone failed")));
            });
            assert!(catch_unwind(AssertUnwindSafe(|| table.lost_keys())).is_err());
            assert_eq!(table.lost_keys(), vec![Key(1), Key(2)]);
            assert_eq!(table.lost_completion_count(), 2);

            let replacement = Rc::new(RefCell::new(None));
            ON_CLONE.with(|slot| {
                let table = table.clone();
                let replacement = replacement.clone();
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert!(table.evict_lost(&Key(1)));
                    let (reader, completion) = table.start(Key(1), 0);
                    drop(reader);
                    *replacement.borrow_mut() = completion;
                    assert_eq!(table.lost_keys(), vec![Key(2)]);
                }));
            });
            assert_eq!(table.lost_keys(), vec![Key(1), Key(2)]);
            assert_eq!(table.lost_keys(), vec![Key(2)]);
            assert!(!table.evict_lost(&Key(1)));
            replacement.borrow_mut().take().unwrap().finish(7);
            assert!(table.evict_lost(&Key(2)));
            assert!(table.lost_keys().is_empty());
            assert_eq!(table.lost_completion_count(), 2);
        }

        #[test]
        fn occupied_start_drops_unused_key_and_closed_value_outside_borrow() {
            use std::cell::Cell;

            thread_local! {
                static ON_KEY_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
            }

            #[derive(Clone, Eq, PartialEq, Ord, PartialOrd)]
            struct Key(u32);

            impl Drop for Key {
                fn drop(&mut self) {
                    let callback = ON_KEY_DROP.with(|slot| slot.borrow_mut().take());
                    if let Some(callback) = callback {
                        callback();
                    }
                }
            }

            struct Closed(Option<Box<dyn FnOnce()>>);

            impl Drop for Closed {
                fn drop(&mut self) {
                    if let Some(callback) = self.0.take() {
                        callback();
                    }
                }
            }

            let table = Rc::new(Table::default());
            let (first, completion) = table.start(Key(1), Rc::new(Closed(None)));
            drop(first);
            let key_dropped = Rc::new(Cell::new(false));
            let closed_dropped = Rc::new(Cell::new(false));
            ON_KEY_DROP.with(|slot| {
                let table = table.clone();
                let observed = key_dropped.clone();
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert_eq!(table.entries.borrow_mut().len(), 1);
                    observed.set(true);
                }));
            });
            let closed = {
                let table = table.clone();
                let observed = closed_dropped.clone();
                Rc::new(Closed(Some(Box::new(move || {
                    assert_eq!(table.entries.borrow_mut().len(), 1);
                    observed.set(true);
                }))))
            };
            let (joined, rejected) = table.start(Key(1), closed);
            assert!(rejected.is_none());
            assert!(key_dropped.get());
            assert!(closed_dropped.get());
            let result = Rc::new(Closed(None));
            completion.unwrap().finish(result.clone());
            assert!(Rc::ptr_eq(&block_on(joined), &result));
            assert!(table.is_empty());
        }

        /// Final receiver destruction can reenter through its retained closed value.
        #[test]
        fn completion_drops_last_receiver_outside_table_borrow() {
            use std::cell::Cell;
            use std::sync::Arc;
            use std::task::{Context, Wake, Waker};

            thread_local! {
                static ON_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
            }

            struct ReentrantDrop;

            impl Wake for ReentrantDrop {
                fn wake(self: Arc<Self>) {
                    panic!("closed result waker must only be dropped");
                }
            }

            impl Drop for ReentrantDrop {
                fn drop(&mut self) {
                    let callback = ON_DROP.with(|slot| slot.borrow_mut().take());
                    if let Some(callback) = callback {
                        callback();
                    }
                }
            }

            let table = Rc::new(Table::default());
            let dropped = Rc::new(Cell::new(false));
            let closed = Some(Waker::from(Arc::new(ReentrantDrop)));
            let (mut receive, completion) = table.start(1, closed);
            assert!(
                receive
                    .poll_unpin(&mut Context::from_waker(Waker::noop()))
                    .is_pending()
            );
            drop(receive);
            assert_eq!(table.len(), 1);

            let nested = table.clone();
            let observed = dropped.clone();
            ON_DROP.with(|slot| {
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert!(nested.is_empty());
                    assert!(nested.get(&1).is_none());
                    let (replacement, completion) = nested.start(1, None);
                    assert_eq!(nested.len(), 1);
                    completion.unwrap().finish(None);
                    assert!(block_on(replacement).is_none());
                    observed.set(true);
                }));
            });

            assert!(!dropped.get());
            completion.unwrap().finish(None);
            assert!(dropped.get());
            assert!(table.is_empty());
            assert!(ON_DROP.with(|slot| slot.borrow().is_none()));
        }

        /// Results remain readable after completion admits a replacement.
        #[test]
        fn success_miss_failure_broadcast_and_replacement() {
            for value in [Ok(Some(7)), Ok(None), Err("io")] {
                let table = Rc::new(Table::default());
                let (first, completion) = table.start(1, Err("closed"));
                let second = table.get(&1).unwrap();
                assert_eq!(table.len(), 1);
                completion.unwrap().finish(value);
                assert!(table.is_empty());
                let (next, completion) = table.start(1, Err("closed"));
                assert_eq!(block_on(first), value);
                assert_eq!(block_on(second), value);
                assert_eq!(table.len(), 1);
                completion.unwrap().finish(Ok(Some(8)));
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
            completion.unwrap().finish(Ok(9));
            assert_eq!(block_on(late), Ok(9));
            assert!(table.is_empty());
            let (receive, completion) = table.start(1, Err("closed"));
            drop(completion);
            assert_eq!(block_on(receive), Err("closed"));
            assert_eq!(block_on(table.get(&1).unwrap()), Err("closed"));
            assert_eq!(table.len(), 1);
        }

        /// Only explicit recovery after real completion can reopen a lost key.
        #[test]
        fn lost_completion_recovery_preserves_old_readers_and_replacement() {
            let table = Rc::new(Table::default());
            assert!(!table.evict_lost(&1));
            let (old, completion) = table.start(1, Err::<u32, _>("closed"));
            assert!(!table.evict_lost(&1));
            assert_eq!(table.lost_completion_count(), 0);
            drop(completion);
            assert_eq!(table.lost_completion_count(), 1);
            let (joined, rejected) = table.start(1, Err("other"));
            assert!(rejected.is_none());
            assert_eq!(block_on(joined), Err("closed"));
            assert!(table.evict_lost(&1));
            assert!(!table.evict_lost(&1));
            let (new, completion) = table.start(1, Err("new closed"));
            assert_eq!(block_on(old), Err("closed"));
            assert_eq!(table.len(), 1);
            completion.unwrap().finish(Ok(7));
            assert_eq!(block_on(new), Ok(7));
            assert!(table.is_empty());
            assert_eq!(table.lost_completion_count(), 1);
        }
    }
}

pub mod flight {
    //! Owned flight indexing, generation fences, and bounded lifecycle sweeps.
    //!
    //! Entry hooks own result interpretation and policy. A sweep refreshes each
    //! selected entry once and removes it only when the hook reports quiescence.
    //! Clone incoming wakers before borrowing. Hooks must stage removed values with
    //! `Txn::retire`, not drop them or return them alongside queued notifications.
    //! The transaction releases staged values before waking, including on unwind.
    //! Low-level methods return owned values for inspection; stage those values before
    //! queuing wakes. Key callbacks and hooks must not reborrow the owner. Callbacks
    //! and destructors must not panic during transaction cleanup.

    use std::cell::RefCell;
    use std::collections::{BTreeMap, HashMap, hash_map::RandomState};
    use std::hash::{BuildHasher, Hash};
    use std::rc::Rc;
    use std::task::Waker;
    use std::time::Instant;

    /// Stage owned values and notifications while borrowing a worker-local owner.
    /// Values passed to `retire` drop before wakes, after the owner borrow releases.
    /// Raw state access still requires the adapter to stage every removed value.
    pub struct Txn {
        now: Instant,

        retired: Vec<Box<dyn std::any::Any>>,

        wakes: Vec<Waker>,
    }

    impl Txn {
        /// Use one caller-sampled time for every hook in this transaction.
        pub fn new(now: Instant) -> Self {
            Self {
                now,
                retired: Vec::new(),
                wakes: Vec::new(),
            }
        }

        /// Read the fixed time without invoking an application clock callback.
        pub fn now(&self) -> Instant {
            self.now
        }

        /// Transfer an owned, worker-local value for destruction before notification.
        pub fn retire<T: 'static>(&mut self, value: T) {
            self.retired.push(Box::new(value));
        }

        /// Remove a quiescent entry and stage both index keys and its application state.
        pub fn remove_quiescent<K: Eq + Hash + 'static, E: Entry + 'static, S: BuildHasher>(
            &mut self,
            table: &mut Table<K, E, S>,
            key: &K,
        ) -> bool {
            if let Some(removed) = table.remove_quiescent(key) {
                self.retire(removed);
                true
            } else {
                false
            }
        }

        /// Sweep and stage all removed entries before any queued notification runs.
        pub fn sweep<K: Clone + Eq + Hash + 'static, E: Entry + 'static, S: BuildHasher>(
            &mut self,
            table: &mut Table<K, E, S>,
            budget: usize,
        ) {
            table.sweep(budget, self);
        }

        /// Store a precloned drain target and stage the previous target for release.
        pub fn register_drain<K, E, S>(&mut self, table: &mut Table<K, E, S>, waker: Waker) {
            self.retire(table.register_drain(waker));
        }
    }

    impl std::ops::Deref for Txn {
        type Target = Vec<Waker>;

        /// Access notifications collected by low-level state transitions.
        fn deref(&self) -> &Self::Target {
            &self.wakes
        }
    }

    impl std::ops::DerefMut for Txn {
        /// Enqueue notifications without invoking callbacks under the owner borrow.
        fn deref_mut(&mut self) -> &mut Self::Target {
            &mut self.wakes
        }
    }

    impl Drop for Txn {
        /// The update scope releases its borrow before this guard runs, even on panic.
        fn drop(&mut self) {
            self.retired.clear();
            super::wake_all(std::mem::take(&mut self.wakes));
        }
    }

    /// Sample time before borrowing and release staged values before notifying.
    pub fn update<T, R>(owner: &RefCell<T>, f: impl FnOnce(&mut T, &mut Txn) -> R) -> R {
        update_at(owner, Instant::now(), f)
    }

    /// Run hooks with one supplied clock snapshot and cleanup on normal return or unwind.
    pub fn update_at<T, R>(
        owner: &RefCell<T>,
        now: Instant,
        f: impl FnOnce(&mut T, &mut Txn) -> R,
    ) -> R {
        let mut txn = Txn::new(now);
        {
            let mut state = owner.borrow_mut();
            f(&mut state, &mut txn)
        }
    }

    /// A monotonic identifier or acquisition budget has been exhausted.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Exhausted;

    /// A registration, generation, or operation no longer matches its owner.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Stale;

    /// Key equality belongs to the adapter; this identity fences owner, incarnation,
    /// and acquisition generation independently of any application key schema.
    #[derive(Clone)]
    pub struct Identity {
        /// Local table owner; pointer identity fences unrelated workers and tables.
        pub owner: Rc<()>,

        /// Monotonic identity of one entry admission.
        pub incarnation: u64,

        /// Acquisition attempt within this incarnation.
        pub generation: u64,
    }

    impl Identity {
        /// Compare entry ownership without invalidating waiters across retries.
        pub fn same_registration(&self, current: &Self) -> bool {
            Rc::ptr_eq(&self.owner, &current.owner) && self.incarnation == current.incarnation
        }

        /// Validate an acquisition capability against the current generation.
        pub fn validate(&self, current: &Self) -> Result<(), Stale> {
            if !self.same_registration(current) || self.generation != current.generation {
                return Err(Stale);
            }
            Ok(())
        }

        /// Called only after the adapter confirms eligibility and completion fences.
        fn advance(&mut self, limit: u64) -> Result<(), Exhausted> {
            if self.generation >= limit {
                return Err(Exhausted);
            }
            self.generation = self.generation.checked_add(1).ok_or(Exhausted)?;
            Ok(())
        }
    }

    /// Application lifecycle hooks. Refresh must be bounded and only enqueue wakes.
    /// Quiescence must include both detached waiters and actual retained operations,
    /// never merely cancellation requested by a caller or expiration of a deadline.
    pub trait Entry {
        /// Refresh bounded lifecycle work and enqueue notifications without waking.
        fn refresh(&mut self, txn: &mut Txn);

        /// Report that neither waiters nor retained operations need this entry.
        fn quiescent(&self) -> bool;
    }

    /// Owned membership with a synchronized, bounded sweep index.
    /// Direct map mutation is intentionally unavailable:
    /// ```compile_fail,E0616
    /// let mut table = flow_control::coalesce::flight::Table::<u32, ()>::default();
    /// table.entries.clear();
    /// ```
    /// Even an empty table belongs to its worker:
    /// ```compile_fail,E0277
    /// fn require_send<T: Send>() {}
    /// require_send::<flow_control::coalesce::flight::Table<u32, ()>>();
    /// ```
    /// ```compile_fail,E0277
    /// fn require_sync<T: Sync>() {}
    /// require_sync::<flow_control::coalesce::flight::Table<u32, ()>>();
    /// ```
    pub struct Table<K, E, S = RandomState> {
        entries: HashMap<K, Indexed<E>, S>,

        sweep: BTreeMap<u64, K>,

        next_sweep_id: u64,

        cursor: Cursor,

        incarnation: Counter,

        next_waiter: Counter,

        next_operation: Counter,

        stopping: bool,

        drain_waker: Option<Waker>,

        /// Keep even an empty table local to the worker that drives its lifecycle.
        local: std::marker::PhantomData<Rc<()>>,
    }

    /// Membership identity owned by the table, never by mutable application state.
    struct Indexed<E> {
        sweep_id: u64,

        entry: E,
    }

    /// Removed membership owns both keys so their destructors run with the entry.
    #[must_use = "stage removed membership with Txn::retire before queuing notifications"]
    pub struct Removed<K, E> {
        _map_key: K,

        _sweep_key: K,

        /// Application state retained outside its former owner's borrow.
        pub entry: E,
    }

    impl<K, E, S: Default> Default for Table<K, E, S> {
        /// Create an empty worker-local index with fresh identifier sequences.
        fn default() -> Self {
            Self {
                entries: HashMap::default(),
                sweep: BTreeMap::new(),
                next_sweep_id: 0,
                cursor: Cursor::default(),
                incarnation: Counter::default(),
                next_waiter: Counter::default(),
                next_operation: Counter::default(),
                stopping: false,
                drain_waker: None,
                local: std::marker::PhantomData,
            }
        }
    }

    impl<K, E, S> Table<K, E, S> {
        /// Count entries, including those waiting for real operation completion.
        pub fn len(&self) -> usize {
            self.entries.len()
        }

        /// Whether all owned entries have been removed.
        pub fn is_empty(&self) -> bool {
            self.entries.is_empty()
        }

        /// Inspect application state without exposing the membership index.
        pub fn values(&self) -> impl Iterator<Item = &E> {
            self.entries.values().map(|indexed| &indexed.entry)
        }

        /// Allocate a never-reused waiter identifier.
        pub fn next_waiter_id(&mut self) -> Result<u64, Exhausted> {
            self.next_waiter.next_id()
        }

        /// Allocate a never-reused operation identifier.
        pub fn next_operation_id(&mut self) -> Result<u64, Exhausted> {
            self.next_operation.next_id()
        }

        /// Whether shutdown was requested; the adapter must enforce admission closure.
        pub fn is_stopping(&self) -> bool {
            self.stopping
        }

        /// Retain the latest drain task's wake target and return the previous one.
        /// Clone before borrowing the owner. With `update`, use `Txn::register_drain`
        /// or stage the returned waker with `Txn::retire` before notifications run.
        #[must_use = "stage the previous drain waker with Txn::retire"]
        pub fn register_drain(&mut self, waker: Waker) -> Option<Waker> {
            self.drain_waker.replace(waker)
        }

        /// Enqueue a pending drain notification at most once.
        pub fn notify_drain(&mut self, wakes: &mut Vec<Waker>) {
            if let Some(waker) = self.drain_waker.take() {
                wakes.push(waker);
            }
        }

        /// Allocate an entry incarnation fenced by its local owner.
        pub fn identity(&mut self, owner: Rc<()>) -> Result<Identity, Exhausted> {
            Ok(Identity {
                owner,
                incarnation: self.incarnation.next_id()?,
                generation: 0,
            })
        }
    }

    impl<K: Eq + Hash, E, S: BuildHasher> Table<K, E, S> {
        /// Look up the current entry for a key.
        pub fn get(&self, key: &K) -> Option<&E> {
            self.entries.get(key).map(|indexed| &indexed.entry)
        }

        /// Mutate application state without affecting table-owned sweep membership.
        /// The adapter remains responsible for its own incarnation-fence semantics.
        pub fn get_mut(&mut self, key: &K) -> Option<&mut E> {
            self.entries.get_mut(key).map(|indexed| &mut indexed.entry)
        }

        /// Whether a key currently has an owned entry.
        pub fn contains_key(&self, key: &K) -> bool {
            self.entries.contains_key(key)
        }
    }

    impl<K: Clone + Eq + Hash, E, S: BuildHasher> Table<K, E, S> {
        /// Insert only into a vacant key after the adapter checks admission.
        /// Occupied keys, even quiescent ones, return the rejected key and entry.
        /// Remove quiescent entries explicitly before admitting replacements.
        /// Stage rejected values with `Txn::retire` before notifications run.
        #[must_use = "stage the rejected key and entry with Txn::retire"]
        pub fn insert(&mut self, key: K, entry: E) -> Result<(), (K, E)> {
            if self.entries.contains_key(&key) {
                return Err((key, entry));
            }
            let sweep_id = self.allocate_sweep_id();
            self.entries
                .insert(key.clone(), Indexed { sweep_id, entry });
            self.sweep.insert(sweep_id, key);
            Ok(())
        }

        /// Find a free internal sweep slot without consuming incarnation IDs.
        fn allocate_sweep_id(&mut self) -> u64 {
            // Unlike externally visible incarnation fences, these IDs can be reused
            // after removal. At most len + 1 probes find a free ID, even after wrap.
            // Vacant insertion needs no incarnation allocation or fallible counter.
            for _ in 0..=self.sweep.len() {
                self.next_sweep_id = self.next_sweep_id.wrapping_add(1);
                if !self.sweep.contains_key(&self.next_sweep_id) {
                    return self.next_sweep_id;
                }
            }
            unreachable!("a finite in-memory table cannot occupy every u64 sweep ID")
        }
    }

    impl<K: Eq + Hash, E: Entry, S: BuildHasher> Table<K, E, S> {
        /// Used after explicit detach or completion as well as by background sweeps.
        /// With `update`, use `Txn::remove_quiescent` or stage the returned entry
        /// with `Txn::retire` so destruction precedes notifications.
        #[must_use = "stage the removed entry with Txn::retire"]
        pub fn remove_quiescent(&mut self, key: &K) -> Option<Removed<K, E>> {
            if !self.get(key).is_some_and(Entry::quiescent) {
                return None;
            }
            let (map_key, entry) = self.entries.remove_entry(key).expect("quiescent entry");
            let sweep_key = self
                .sweep
                .remove(&entry.sweep_id)
                .expect("indexed sweep key");
            Some(Removed {
                _map_key: map_key,
                _sweep_key: sweep_key,
                entry: entry.entry,
            })
        }
    }

    impl<K, E, S> Table<K, E, S> {
        /// Mark shutdown and visit each entry synchronously. The adapter must reject
        /// admission, choose settlement policy, and drive actual operation completions.
        pub fn stop(&mut self, txn: &mut Txn, mut stop: impl FnMut(&mut E, &mut Txn)) {
            self.stopping = true;
            for entry in self.entries.values_mut() {
                stop(&mut entry.entry, txn);
            }
        }
    }

    impl<K: Clone + Eq + Hash + 'static, E: Entry + 'static, S: BuildHasher> Table<K, E, S> {
        /// Refresh at most the budgeted entry count and remove quiescent entries.
        /// Stage keys and entries immediately, so a later hook panic cannot drop them
        /// under the borrow. Return the number of removed entries.
        pub fn sweep(&mut self, budget: usize, txn: &mut Txn) -> usize {
            let mut removed = 0;
            for _ in 0..budget.min(self.sweep.len()) {
                let Some((_, key)) = self.cursor.next(&self.sweep) else {
                    break;
                };
                let key = key.clone();
                // Stage the temporary key before invoking application hooks.
                let key = Rc::new(key);
                txn.retire(key.clone());
                if let Some(entry) = self.get_mut(&key) {
                    entry.refresh(txn);
                }
                if let Some(entry) = self.remove_quiescent(&key) {
                    txn.retire(entry);
                    removed += 1;
                }
            }
            if self.entries.is_empty() {
                self.notify_drain(txn);
            }
            removed
        }
    }

    /// Two-phase completion slots. Taking a resource leaves its slot occupied while
    /// its destructor runs outside the table borrow. Only explicit completion clears
    /// that slot; dropping an external completion token does not touch this owner.
    /// ```compile_fail,E0277
    /// fn require_send<T: Send>() {}
    /// require_send::<flow_control::coalesce::flight::Operations<()>>();
    /// ```
    /// ```compile_fail,E0277
    /// fn require_sync<T: Sync>() {}
    /// require_sync::<flow_control::coalesce::flight::Operations<()>>();
    /// ```
    pub struct Operations<R, S = RandomState> {
        slots: HashMap<u64, OperationSlot<R>, S>,

        /// Resource destruction and completion must run on the owning worker.
        local: std::marker::PhantomData<Rc<()>>,
    }

    impl<R, S: Default> Default for Operations<R, S> {
        /// Create an empty local resource owner.
        fn default() -> Self {
            Self {
                slots: HashMap::default(),
                local: std::marker::PhantomData,
            }
        }
    }

    impl<R, S> Operations<R, S> {
        /// Count retained resources and occupied completion tombstones.
        pub fn len(&self) -> usize {
            self.slots.len()
        }

        /// Whether every resource has been taken and explicitly completed.
        pub fn is_empty(&self) -> bool {
            self.slots.is_empty()
        }
    }

    impl<R, S: BuildHasher> Operations<R, S> {
        /// Retain resources under a caller-allocated unique operation identifier.
        /// Return duplicate resources unchanged; even a completion tombstone is occupied.
        #[must_use = "retire rejected resources outside the owner borrow"]
        pub fn insert(&mut self, id: u64, resources: R) -> Result<(), R> {
            if self.slots.contains_key(&id) {
                return Err(resources);
            }
            self.slots.insert(id, OperationSlot::Retained(resources));
            Ok(())
        }

        /// Take resources for destruction outside the owner borrow, retaining a fence.
        pub fn take(&mut self, id: u64) -> Result<R, Stale> {
            let slot = self.slots.get_mut(&id).ok_or(Stale)?;
            match std::mem::replace(slot, OperationSlot::Completing) {
                OperationSlot::Retained(resources) => Ok(resources),
                OperationSlot::Completing => Err(Stale),
            }
        }

        /// Clear a taken slot after the caller has finished resource destruction.
        pub fn complete(&mut self, id: u64) -> Result<(), Stale> {
            if !matches!(self.slots.get(&id), Some(OperationSlot::Completing)) {
                return Err(Stale);
            }
            self.slots.remove(&id);
            Ok(())
        }
    }

    /// An occupied operation owns either resources or their unfinished completion fence.
    enum OperationSlot<R> {
        /// Resources must be taken and destroyed before completion is acknowledged.
        Retained(R),

        /// Resources left the owner, but actual completion has not been confirmed.
        Completing,
    }

    /// Monotonic IDs are never reused, even after an entry is removed.
    #[derive(Default)]
    struct Counter(u64);

    impl Counter {
        /// Allocate the next identifier without wrapping or reusing an old value.
        fn next_id(&mut self) -> Result<u64, Exhausted> {
            self.0 = self.0.checked_add(1).ok_or(Exhausted)?;
            Ok(self.0)
        }
    }

    /// Stable round-robin selection without a scan, including wrap after removal.
    #[derive(Default)]
    struct Cursor(u64);

    impl Cursor {
        /// Select the next live identifier, wrapping to the first when necessary.
        fn next<'a, V>(&mut self, entries: &'a BTreeMap<u64, V>) -> Option<(u64, &'a V)> {
            let (&id, value) = entries
                .range((
                    std::ops::Bound::Excluded(self.0),
                    std::ops::Bound::Unbounded,
                ))
                .next()
                .or_else(|| entries.first_key_value())?;
            self.0 = id;
            Some((id, value))
        }
    }

    /// Waiter lifecycle and result-independent flight transitions.
    pub mod state {
        use super::{Cursor, Exhausted, Identity, Stale};
        use std::collections::BTreeMap;
        use std::ops::{Deref, DerefMut};
        use std::task::Waker;
        use std::time::Instant;

        /// Request-owned policy facts, not acquisition credits or result interpretation.
        pub trait WaiterPolicy {
            /// Lightweight failure value retained with a waiter.
            type Error: Copy;

            /// Check cancellation, deadline, and any other request-owned policy.
            /// Use this snapshot for deadline decisions. A due policy may defer failure;
            /// its index remains live until a later check reports an error.
            fn check(&self, now: Instant) -> Option<Self::Error>;

            /// Return the request's fixed deadline for bounded expiry indexing.
            fn deadline(&self) -> Instant;
        }

        /// One attached caller's policy, acquisition eligibility, and notification state.
        pub struct Waiter<W: WaiterPolicy> {
            /// Caller-owned policy facts, available through dereferencing as well.
            policy: W,

            /// Whether this caller can acquire rather than only observe results.
            acquisition: bool,

            /// Whether partial publication permits another attempt for this caller.
            pub complete: bool,

            /// Whether this caller has already received its acquisition opportunity.
            pub issued: bool,

            /// Sticky policy or cancellation failure.
            pub error: Option<W::Error>,

            /// Latest wake target, taken once when notification is enqueued.
            pub waker: Option<Waker>,
        }

        impl<W: WaiterPolicy> Deref for Waiter<W> {
            type Target = W;

            /// Read the caller-owned policy facts.
            fn deref(&self) -> &W {
                &self.policy
            }
        }

        impl<W: WaiterPolicy> DerefMut for Waiter<W> {
            /// Update caller-owned policy facts without replacing waiter state.
            fn deref_mut(&mut self) -> &mut W {
                &mut self.policy
            }
        }

        impl<W: WaiterPolicy> Waiter<W> {
            /// Whether an unissued acquisition opportunity remains usable.
            fn eligible(&self) -> bool {
                self.acquisition && !self.issued && self.error.is_none()
            }
        }

        /// Attempt outcome held behind the retained-operation completion fence.
        pub enum Outcome<O, E> {
            /// A publication awaiting settlement and result interpretation.
            Published(O),

            /// A terminal error awaiting settlement.
            Failed(E),

            /// An attempt that permits another eligible caller after settlement.
            Retry,
        }

        /// Flight lifecycle independent of application-specific result semantics.
        pub enum Phase<C, O, E> {
            /// A leader may acquire and publish a result.
            Acquiring,

            /// No retained attempt blocks election of an eligible caller.
            RetryPending,

            /// An outcome exists but retained operations may still own resources.
            Draining(Outcome<O, E>),

            /// A fully settled complete result is available.
            Complete(C),

            /// A fully settled terminal error is available.
            Failed(E),
        }

        /// Adapter interpretation of a successfully settled publication.
        pub enum Published<C, P> {
            /// A result that satisfies all callers.
            Complete(C),

            /// A reusable intermediate result that may require another acquisition.
            Partial(P),
        }

        /// Values removed by shutdown. Stage with `Txn::retire` before notifications run.
        #[must_use = "stage the stopped phase and waiters with Txn::retire"]
        pub struct Stopped<C, O, W: WaiterPolicy> {
            /// Previous outcome or complete result, no longer visible to callers.
            pub phase: Phase<C, O, W::Error>,

            /// Detached policies with shutdown errors and no parked wakers.
            pub waiters: BTreeMap<u64, Waiter<W>>,
        }

        /// Worker-local waiter index and transition state driven by an owning adapter.
        /// The adapter supplies actual operation idleness; cancellation is not idleness.
        /// ```compile_fail,E0277
        /// use flow_control::coalesce::flight::state::{State, WaiterPolicy};
        /// use std::time::Instant;
        /// struct Policy;
        /// impl WaiterPolicy for Policy {
        ///     type Error = ();
        ///     fn check(&self, _: Instant) -> Option<()> { None }
        ///     fn deadline(&self) -> Instant { Instant::now() }
        /// }
        /// fn require_send<T: Send>() {}
        /// require_send::<State<(), (), (), Policy>>();
        /// ```
        /// ```compile_fail,E0277
        /// use flow_control::coalesce::flight::state::{State, WaiterPolicy};
        /// use std::time::Instant;
        /// struct Policy;
        /// impl WaiterPolicy for Policy {
        ///     type Error = ();
        ///     fn check(&self, _: Instant) -> Option<()> { None }
        ///     fn deadline(&self) -> Instant { Instant::now() }
        /// }
        /// fn require_sync<T: Sync>() {}
        /// require_sync::<State<(), (), (), Policy>>();
        /// ```
        pub struct State<C, P, O, W: WaiterPolicy> {
            /// Current acquisition or settlement phase.
            pub phase: Phase<C, O, W::Error>,

            /// Current leader's waiter identifier, retained until settlement.
            pub leader: Option<u64>,

            /// Attached callers indexed by monotonic waiter identifier.
            pub waiters: BTreeMap<u64, Waiter<W>>,

            /// Deadline index used for bounded expiration work.
            pub deadlines: BTreeMap<(Instant, u64), ()>,

            /// Settled intermediate result retained across another acquisition.
            pub partial: Option<P>,

            cursor: Cursor,

            deadline_cursor: Option<(Instant, u64)>,

            /// State transitions belong to one worker even for thread-safe policy data.
            local: std::marker::PhantomData<std::rc::Rc<()>>,
        }

        impl<C, P, O, W: WaiterPolicy> Default for State<C, P, O, W> {
            /// Start without callers or a retained attempt, ready for initial election.
            fn default() -> Self {
                Self {
                    phase: Phase::RetryPending,
                    leader: None,
                    waiters: BTreeMap::new(),
                    deadlines: BTreeMap::new(),
                    partial: None,
                    cursor: Cursor::default(),
                    deadline_cursor: None,
                    local: std::marker::PhantomData,
                }
            }
        }

        impl<C, P, O, W: WaiterPolicy> State<C, P, O, W> {
            /// Attach a caller using an identifier allocated by the owning table.
            /// Duplicate IDs return the policy without changing the original waiter or deadline.
            #[must_use = "retire rejected policies outside the owner borrow"]
            pub fn register(
                &mut self,
                id: u64,
                policy: W,
                acquisition: bool,
                complete: bool,
            ) -> Result<(), W> {
                if self.waiters.contains_key(&id) {
                    return Err(policy);
                }
                self.deadlines.insert((policy.deadline(), id), ());
                self.waiters.insert(
                    id,
                    Waiter {
                        policy,
                        acquisition,
                        complete,
                        issued: false,
                        error: None,
                        waker: None,
                    },
                );
                Ok(())
            }

            /// Remove a caller and its deadline without declaring operation completion.
            /// Stage the returned waiter with `Txn::retire`; its policy and waker
            /// may run callbacks and must drop after the borrow, before notifications.
            #[must_use = "stage the detached waiter with Txn::retire"]
            pub fn detach(&mut self, id: u64) -> Option<Waiter<W>> {
                let waiter = self.waiters.remove(&id)?;
                self.deadlines.remove(&(waiter.deadline(), id));
                Some(waiter)
            }

            /// Validate attachment and incarnation while allowing acquisition retries.
            pub fn validate_registration(
                &self,
                id: u64,
                attached: bool,
                registered: &Identity,
                current: &Identity,
            ) -> Result<(), Stale> {
                if !attached
                    || !registered.same_registration(current)
                    || !self.waiters.contains_key(&id)
                {
                    return Err(Stale);
                }
                Ok(())
            }

            /// Mark only the supplying caller; refresh decides whether to revoke a leader.
            pub fn cancel(&mut self, id: u64, error: W::Error) {
                if let Some(waiter) = self.waiters.get_mut(&id) {
                    waiter.error = Some(error);
                }
            }

            /// Caller validates its leader capability and publication before this step.
            /// Notification timing remains explicit: publication wakes only on settlement,
            /// while rejection/revocation also notifies before retained operations finish.
            /// Stage the old phase with `Txn::retire` before notifications run.
            #[must_use = "stage the retired phase with Txn::retire"]
            pub fn begin_completion(
                &mut self,
                outcome: Outcome<O, W::Error>,
            ) -> Phase<C, O, W::Error> {
                std::mem::replace(&mut self.phase, Phase::Draining(outcome))
            }

            /// Enqueue all parked caller notifications without invoking their wakers.
            pub fn notify(&mut self, wakes: &mut Vec<Waker>) {
                for waiter in self.waiters.values_mut() {
                    if let Some(waker) = waiter.waker.take() {
                        wakes.push(waker);
                    }
                }
            }

            /// Check one caller in round-robin order without scanning the waiter map.
            pub fn sweep_waiter(&mut self, now: Instant, wakes: &mut Vec<Waker>) {
                if let Some((id, _)) = self.cursor.next(&self.waiters) {
                    self.refresh_waiter(id, now, wakes);
                }
            }

            /// Validate policy/budget first in the adapter. Only an eligible waiter can
            /// receive a new generation; retained operations must have settled first.
            pub fn elect(
                &mut self,
                id: u64,
                identity: &mut Identity,
                limit: u64,
            ) -> Result<bool, Exhausted> {
                if !matches!(self.phase, Phase::RetryPending) {
                    return Ok(false);
                }
                let Some(waiter) = self.waiters.get_mut(&id).filter(|waiter| waiter.eligible())
                else {
                    return Ok(false);
                };
                identity.advance(limit)?;
                self.phase = Phase::Acquiring;
                self.leader = Some(id);
                waiter.issued = true;
                Ok(true)
            }

            /// Validate the active leader after the adapter checks its generation fence.
            pub fn validate_leader(&self, id: u64, active: bool) -> Result<(), Stale> {
                if !active || !matches!(self.phase, Phase::Acquiring) || self.leader != Some(id) {
                    return Err(Stale);
                }
                Ok(())
            }

            /// Expose an outcome only after the adapter confirms all operations completed.
            /// The split hook runs inside the transaction and must not reborrow its owner.
            /// Stage the old partial with `Txn::retire` before notifications run.
            #[must_use = "stage the retired partial with Txn::retire"]
            pub fn settle(
                &mut self,
                idle: bool,
                unavailable: W::Error,
                split: fn(O) -> Published<C, P>,
                wakes: &mut Vec<Waker>,
            ) -> Option<P> {
                if !idle || !matches!(self.phase, Phase::Draining(_)) {
                    return None;
                }
                let Phase::Draining(outcome) =
                    std::mem::replace(&mut self.phase, Phase::RetryPending)
                else {
                    unreachable!()
                };
                self.leader = None;
                let mut retired = None;
                self.phase = match outcome {
                    Outcome::Published(value) => match split(value) {
                        Published::Complete(value) => {
                            retired = self.partial.take();
                            Phase::Complete(value)
                        }
                        Published::Partial(value) => {
                            retired = self.partial.replace(value);
                            for waiter in self.waiters.values_mut() {
                                if waiter.complete {
                                    waiter.issued = false;
                                }
                            }
                            Phase::RetryPending
                        }
                    },
                    Outcome::Failed(error) => Phase::Failed(error),
                    Outcome::Retry if self.waiters.values().any(Waiter::eligible) => {
                        Phase::RetryPending
                    }
                    Outcome::Retry => Phase::Failed(unavailable),
                };
                self.notify(wakes);
                retired
            }

            /// Revoke acquisition immediately, retaining its outcome behind completion.
            /// Stage the old phase with `Txn::retire` before notifications run.
            #[must_use = "stage the retired phase with Txn::retire"]
            pub fn revoke(
                &mut self,
                error: W::Error,
                wakes: &mut Vec<Waker>,
            ) -> Phase<C, O, W::Error> {
                if let Some(waiter) = self.leader.and_then(|id| self.waiters.get_mut(&id)) {
                    waiter.error = Some(error);
                }
                let retired = self.begin_completion(Outcome::Retry);
                self.notify(wakes);
                retired
            }

            /// Check the leader and up to 64 expired callers, then settle eligible outcomes.
            /// Sample `now` before `update`; all deadline entries use that snapshot.
            /// Stage the old partial with `Txn::retire` before notifications run.
            #[must_use = "stage the retired partial with Txn::retire"]
            pub fn refresh(
                &mut self,
                idle: bool,
                unavailable: W::Error,
                canceled: W::Error,
                now: Instant,
                split: fn(O) -> Published<C, P>,
                wakes: &mut Vec<Waker>,
            ) -> Option<P> {
                if let Some(id) = self.leader {
                    self.refresh_waiter(id, now, wakes);
                }
                self.refresh_expired(now, wakes);
                if matches!(self.phase, Phase::Acquiring) {
                    let leader = self.leader.and_then(|id| self.waiters.get(&id));
                    if leader.is_none_or(|waiter| waiter.error.is_some()) {
                        let error = leader.and_then(|waiter| waiter.error).unwrap_or(canceled);
                        // Acquiring holds no caller-owned value to retire.
                        drop(self.revoke(error, wakes));
                    }
                }
                let retired = self.settle(idle, unavailable, split, wakes);
                if matches!(self.phase, Phase::RetryPending)
                    && self.partial.is_none()
                    && !self.waiters.values().any(Waiter::eligible)
                {
                    self.phase = Phase::Draining(Outcome::Failed(unavailable));
                    // This failed outcome cannot replace a partial value.
                    let unused = self.settle(idle, unavailable, split, wakes);
                    debug_assert!(unused.is_none());
                }
                retired
            }

            /// Detach and notify all callers while preserving the operation drain fence.
            /// Stage removed values with `Txn::retire` before notifications run.
            /// The partial result and leader remain retained until normal settlement/cleanup.
            /// ```
            /// # use flow_control::coalesce::flight::{self, state::{State, WaiterPolicy}};
            /// # use std::{cell::RefCell, time::Instant};
            /// # struct Policy;
            /// # impl WaiterPolicy for Policy {
            /// #     type Error = u8;
            /// #     fn check(&self, _: Instant) -> Option<u8> { None }
            /// #     fn deadline(&self) -> Instant { Instant::now() }
            /// # }
            /// # let owner = RefCell::new(State::<(), (), (), Policy>::default());
            /// flight::update(&owner, |state, txn| {
            ///     let stopped = state.stop(8, txn);
            ///     txn.retire(stopped);
            /// });
            /// ```
            pub fn stop(&mut self, error: W::Error, wakes: &mut Vec<Waker>) -> Stopped<C, O, W> {
                for waiter in self.waiters.values_mut() {
                    waiter.error = Some(error);
                }
                let phase = self.begin_completion(Outcome::Failed(error));
                self.notify(wakes);
                self.deadlines.clear();
                Stopped {
                    phase,
                    waiters: std::mem::take(&mut self.waiters),
                }
            }

            /// Check one caller and enqueue its wake if its policy has failed.
            fn refresh_waiter(&mut self, id: u64, now: Instant, wakes: &mut Vec<Waker>) {
                let Some(waiter) = self.waiters.get_mut(&id) else {
                    return;
                };
                if waiter.error.is_none() {
                    waiter.error = waiter.check(now);
                }
                if waiter.error.is_some() {
                    self.deadlines.remove(&(waiter.deadline(), id));
                    if let Some(waker) = waiter.waker.take() {
                        wakes.push(waker);
                    }
                }
            }

            /// Check at most 64 due entries once each, rotating past deferred failures.
            fn refresh_expired(&mut self, now: Instant, wakes: &mut Vec<Waker>) {
                use std::ops::Bound::{Excluded, Included, Unbounded};

                let end = (now, u64::MAX);
                let cursor = self.deadline_cursor.map(|cursor| cursor.min(end));
                let due: Vec<_> = self
                    .deadlines
                    .range((cursor.map_or(Unbounded, Excluded), Included(end)))
                    .chain(
                        cursor
                            .into_iter()
                            .flat_map(|cursor| self.deadlines.range(..=cursor)),
                    )
                    .take(64)
                    .map(|(&(deadline, id), _)| (deadline, id))
                    .collect();
                for (deadline, id) in due {
                    self.deadline_cursor = Some((deadline, id));
                    if self.waiters.contains_key(&id) {
                        self.refresh_waiter(id, now, wakes);
                    } else {
                        self.deadlines.remove(&(deadline, id));
                    }
                }
            }
        }

        /// Store an owned wake target without calling waker clone or drop callbacks.
        /// Clone the input before borrowing the owner. Stage the retired waker with
        /// `Txn::retire` before notifications run. For the same target, keep
        /// the stored waker and return the unused input instead.
        #[must_use = "stage the retired waker with Txn::retire"]
        pub fn store_waker(slot: &mut Option<Waker>, waker: Waker) -> Option<Waker> {
            if slot.as_ref().is_some_and(|old| old.will_wake(&waker)) {
                Some(waker)
            } else {
                slot.replace(waker)
            }
        }

        /// Pure transition tests for cancellation, publication, and bounded expiry.
        #[cfg(test)]
        mod tests {
            use super::*;
            use std::cell::Cell;
            use std::rc::Rc;
            use std::sync::Arc;
            use std::sync::atomic::{AtomicUsize, Ordering};
            use std::task::Wake;
            use std::time::Duration;

            /// Mutable policy failure used to drive deterministic transitions.
            struct Policy {
                due: Instant,

                error: Rc<Cell<Option<u8>>>,
            }

            impl WaiterPolicy for Policy {
                type Error = u8;

                /// Read the failure injected by the test.
                fn check(&self, _: Instant) -> Option<u8> {
                    self.error.get()
                }

                /// Return the fixed test deadline.
                fn deadline(&self) -> Instant {
                    self.due
                }
            }

            /// Numeric results keep these tests independent of application semantics.
            type Core = State<u32, u32, Published<u32, u32>, Policy>;

            /// Publications already carry their complete or partial interpretation.
            fn split(value: Published<u32, u32>) -> Published<u32, u32> {
                value
            }

            /// Attach a caller and return its externally mutable policy failure.
            fn register(
                core: &mut Core,
                id: u64,
                acquisition: bool,
                complete: bool,
            ) -> Rc<Cell<Option<u8>>> {
                let error = Rc::new(Cell::new(None));
                assert!(
                    core.register(
                        id,
                        Policy {
                            due: Instant::now() + Duration::from_secs(60),
                            error: error.clone(),
                        },
                        acquisition,
                        complete,
                    )
                    .is_ok()
                );
                error
            }

            /// Construct the initial identity for an isolated transition scenario.
            fn identity() -> Identity {
                Identity {
                    owner: Rc::new(()),
                    incarnation: 1,
                    generation: 0,
                }
            }

            /// Refresh with fixed unavailable and cancellation error values.
            fn refresh(core: &mut Core, idle: bool, wakes: &mut Vec<Waker>) {
                let _ = core.refresh(idle, 9, 8, Instant::now(), split, wakes);
            }

            /// Leader cancellation or detach cannot bypass retained-operation drainage.
            #[test]
            fn cancel_or_detach_leader_drains_before_re_election() {
                for detach in [false, true] {
                    let mut core = Core::default();
                    let error = register(&mut core, 1, true, true);
                    register(&mut core, 2, true, true);
                    register(&mut core, 3, false, false);
                    let mut identity = identity();
                    assert_eq!(core.elect(3, &mut identity, 4), Ok(false));
                    assert_eq!(core.elect(1, &mut identity, 4), Ok(true));
                    let old = identity.clone();
                    assert_eq!(core.validate_registration(1, true, &old, &identity), Ok(()));
                    assert_eq!(core.elect(2, &mut identity, 4), Ok(false));
                    if detach {
                        drop(core.detach(1));
                    } else {
                        error.set(Some(7));
                    }
                    let mut wakes = Vec::new();
                    refresh(&mut core, false, &mut wakes);
                    assert!(matches!(core.phase, Phase::Draining(Outcome::Retry)));
                    assert_eq!(core.validate_leader(1, true), Err(Stale));
                    assert_eq!(core.elect(2, &mut identity, 4), Ok(false));
                    refresh(&mut core, true, &mut wakes);
                    assert_eq!(core.elect(2, &mut identity, 4), Ok(true));
                    assert_eq!(old.validate(&identity), Err(Stale));
                    assert_eq!(core.validate_registration(2, true, &old, &identity), Ok(()));
                    assert_eq!(
                        core.validate_registration(2, false, &old, &identity),
                        Err(Stale)
                    );
                    assert_eq!(
                        core.validate_registration(4, true, &old, &identity),
                        Err(Stale)
                    );
                    assert_eq!(core.validate_leader(2, true), Ok(()));
                    assert_eq!(core.validate_leader(2, false), Err(Stale));
                }
            }

            /// Partial results renew only complete-result callers after the fence clears.
            #[test]
            fn partial_then_complete_and_terminal_failure_are_fenced() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, true, false);
                let mut identity = identity();
                assert_eq!(core.elect(1, &mut identity, 3), Ok(true));
                core.waiters.get_mut(&2).unwrap().issued = true;
                let mut wakes = Vec::new();
                drop(core.begin_completion(Outcome::Published(Published::Partial(17))));
                let _ = core.settle(false, 9, split, &mut wakes);
                assert!(core.partial.is_none());
                let _ = core.settle(true, 9, split, &mut wakes);
                assert_eq!(core.partial, Some(17));
                assert!(!core.waiters[&1].issued);
                assert!(core.waiters[&2].issued);
                assert_eq!(core.elect(1, &mut identity, 3), Ok(true));
                drop(core.begin_completion(Outcome::Published(Published::Complete(18))));
                let _ = core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Complete(18)));
                assert!(core.partial.is_none());
                assert_eq!(core.elect(1, &mut identity, 3), Ok(false));
                core.phase = Phase::Draining(Outcome::Failed(6));
                let _ = core.settle(false, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Draining(Outcome::Failed(6))));
                let _ = core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Failed(6)));
            }

            /// Observers cannot acquire and exhausted generations do not issue a caller.
            #[test]
            fn copy_only_cannot_revive_retry_and_generations_are_bounded() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, false, false);
                let mut identity = identity();
                assert_eq!(core.elect(1, &mut identity, 1), Ok(true));
                drop(core.revoke(7, &mut Vec::new()));
                let _ = core.settle(true, 9, split, &mut Vec::new());
                assert!(matches!(core.phase, Phase::Failed(9)));
                core.phase = Phase::RetryPending;
                register(&mut core, 3, true, true);
                assert_eq!(core.elect(3, &mut identity, 1), Err(Exhausted));
                assert!(!core.waiters[&3].issued);
                assert_eq!(identity.generation, 1);
            }

            /// Counts wake delivery without changing transition state.
            #[derive(Default)]
            struct Count(AtomicUsize);

            impl Wake for Count {
                /// Record one delivered notification.
                fn wake(self: Arc<Self>) {
                    self.0.fetch_add(1, Ordering::Relaxed);
                }
            }

            /// Expiry work is bounded to 64 callers and only their latest wakes fire.
            #[test]
            fn deadlines_have_exact_quantum_and_latest_wakes_are_taken_once() {
                let mut core = Core::default();
                let now = Instant::now();
                let old = Arc::new(Count::default());
                let latest = Arc::new(Count::default());
                for id in 0..65 {
                    assert!(
                        core.register(
                            id,
                            Policy {
                                due: now,
                                error: Rc::new(Cell::new(Some(3))),
                            },
                            true,
                            true,
                        )
                        .is_ok()
                    );
                    let old = Waker::from(old.clone());
                    let latest = Waker::from(latest.clone());
                    let retired = {
                        let slot = &mut core.waiters.get_mut(&id).unwrap().waker;
                        (store_waker(slot, old), store_waker(slot, latest))
                    };
                    drop(retired);
                }
                let mut wakes = Vec::new();
                let _ = core.refresh(true, 9, 8, now, split, &mut wakes);
                assert_eq!(core.deadlines.len(), 1);
                assert_eq!(wakes.len(), 64);
                assert!(core.waiters[&64].error.is_none());
                let _ = core.refresh(true, 9, 8, now, split, &mut wakes);
                assert!(core.deadlines.is_empty());
                assert_eq!(wakes.len(), 65);
                core.notify(&mut wakes);
                assert_eq!(wakes.len(), 65);
                for wake in wakes {
                    wake.wake();
                }
                assert_eq!(old.0.load(Ordering::Relaxed), 0);
                assert_eq!(latest.0.load(Ordering::Relaxed), 65);
            }

            /// Shutdown clears caller indexes but retains a pending completion outcome.
            #[test]
            fn duplicate_waiter_keeps_original_policy_and_deadline() {
                let mut core = Core::default();
                let error = register(&mut core, 1, true, true);
                let due = core.waiters[&1].deadline();
                let replacement = Policy {
                    due: due + Duration::from_secs(1),
                    error: Rc::new(Cell::new(Some(4))),
                };
                let rejected = core.register(1, replacement, false, false).err().unwrap();
                assert_eq!(rejected.error.get(), Some(4));
                assert!(Rc::ptr_eq(&core.waiters[&1].policy.error, &error));
                assert_eq!(core.waiters[&1].deadline(), due);
                assert_eq!(core.deadlines.len(), 1);
                assert!(core.deadlines.contains_key(&(due, 1)));
                assert_eq!(core.elect(1, &mut identity(), 1), Ok(true));
                drop(core.detach(1));
                assert!(core.deadlines.is_empty());
            }

            /// Due policies that defer failure remain indexed until a later check fails.
            #[test]
            fn deferred_deadline_failure_is_rechecked_with_shared_time() {
                struct Deferred {
                    due: Instant,

                    fail_at: Instant,
                }

                impl WaiterPolicy for Deferred {
                    type Error = u8;

                    fn check(&self, now: Instant) -> Option<u8> {
                        (now >= self.fail_at).then_some(7)
                    }

                    fn deadline(&self) -> Instant {
                        self.due
                    }
                }

                let now = Instant::now();
                let later = now + Duration::from_secs(1);
                let mut core = State::<(), (), (), Deferred>::default();
                assert!(
                    core.register(
                        1,
                        Deferred {
                            due: now,
                            fail_at: later
                        },
                        true,
                        true
                    )
                    .is_ok()
                );
                assert!(
                    core.register(
                        2,
                        Deferred {
                            due: now,
                            fail_at: now
                        },
                        true,
                        true
                    )
                    .is_ok()
                );
                let mut wakes = Vec::new();
                core.refresh_expired(now, &mut wakes);
                assert_eq!(core.waiters[&1].error, None);
                assert_eq!(core.waiters[&2].error, Some(7));
                assert_eq!(core.deadlines.len(), 1);
                core.refresh_expired(later, &mut wakes);
                assert_eq!(core.waiters[&1].error, Some(7));
                assert!(core.deadlines.is_empty());
            }

            /// Shutdown clears caller indexes but retains a pending completion outcome.
            #[test]
            fn detach_cleans_deadline_and_stop_preserves_pending_completion() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, true, true);
                drop(core.detach(1));
                assert_eq!(core.deadlines.len(), 1);
                let mut identity = identity();
                assert_eq!(core.elect(2, &mut identity, 3), Ok(true));
                core.cancel(2, 6);
                assert_eq!(core.waiters[&2].error, Some(6));
                let mut wakes = Vec::new();
                drop(core.stop(8, &mut wakes));
                let _ = core.settle(false, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Draining(Outcome::Failed(8))));
                assert!(core.waiters.is_empty());
                assert!(core.deadlines.is_empty());
                let _ = core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Failed(8)));
            }
        }
    }

    /// Membership, sweep fairness, and retained-resource fence contracts.
    #[cfg(test)]
    mod tests {
        use super::*;
        use std::cell::{Cell, RefCell};

        /// Count a retained resource until its destructor actually runs.
        struct Resource(Rc<Cell<usize>>);

        impl Drop for Resource {
            /// Record actual resource destruction rather than cancellation.
            fn drop(&mut self) {
                self.0.set(self.0.get() - 1);
            }
        }

        /// Minimal application entry with independently tracked waiters and resources.
        struct TestEntry {
            identity: Identity,

            waiters: usize,

            canceled: bool,

            refreshed: usize,

            operations: Operations<Resource>,
        }

        impl Entry for TestEntry {
            /// Count visits and detach callers when cancellation is observed.
            fn refresh(&mut self, _: &mut Txn) {
                self.refreshed += 1;
                if self.canceled {
                    self.waiters = 0;
                }
            }

            /// Require both caller detachment and real operation completion.
            fn quiescent(&self) -> bool {
                self.waiters == 0 && self.operations.is_empty()
            }
        }

        /// Integer keys isolate table ownership from application key semantics.
        type TestTable = Table<u32, TestEntry>;

        /// Both removal paths return application entries before running destructors.
        #[test]
        fn result_callback_removed_entry_drops_outside_transaction() {
            /// A quiescent entry with a one-shot destructor callback.
            struct CallbackEntry(Option<Box<dyn FnOnce()>>);

            impl Entry for CallbackEntry {
                /// This entry has no pending lifecycle work.
                fn refresh(&mut self, _: &mut Txn) {}

                /// Allow immediate removal.
                fn quiescent(&self) -> bool {
                    true
                }
            }

            impl Drop for CallbackEntry {
                /// Run after the caller releases its table borrow.
                fn drop(&mut self) {
                    if let Some(callback) = self.0.take() {
                        callback();
                    }
                }
            }

            for sweep in [false, true] {
                let table = Rc::new(RefCell::new(Table::<u32, CallbackEntry>::default()));
                let observed = Rc::new(Cell::new(false));
                let entry = {
                    let table = table.clone();
                    let observed = observed.clone();
                    CallbackEntry(Some(Box::new(move || {
                        update(&table, |table, _| {
                            assert!(table.is_empty());
                            assert!(table.insert(1, CallbackEntry(None)).is_ok());
                        });
                        observed.set(true);
                    })))
                };
                assert!(table.borrow_mut().insert(1, entry).is_ok());
                update(&table, |table, txn| {
                    if sweep {
                        txn.sweep(table, 1);
                    } else {
                        assert!(txn.remove_quiescent(table, &1));
                    }
                });
                assert!(observed.get());
                assert_eq!(table.borrow().len(), 1);
                update(&table, |table, txn| {
                    assert!(txn.remove_quiescent(table, &1));
                });
                assert!(table.borrow().is_empty());
            }
        }

        /// Admit an entry with one waiter and return its initial identity.
        fn insert(table: &mut TestTable, owner: &Rc<()>, key: u32) -> Identity {
            let identity = table.identity(owner.clone()).unwrap();
            assert!(
                table
                    .insert(
                        key,
                        TestEntry {
                            identity: identity.clone(),
                            waiters: 1,
                            canceled: false,
                            refreshed: 0,
                            operations: Operations::default(),
                        },
                    )
                    .is_ok()
            );
            identity
        }

        /// Cancellation cannot release resources or bypass the completion tombstone.
        #[test]
        fn canceled_entry_keeps_resources_until_two_phase_completion() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            insert(&mut table, &owner, 1);
            insert(&mut table, &owner, 2);
            let live = Rc::new(Cell::new(1));
            let entry = table.get_mut(&1).unwrap();
            assert!(entry.operations.insert(1, Resource(live.clone())).is_ok());
            entry.canceled = true;
            let mut wakes = Txn::new(Instant::now());
            table.sweep(1, &mut wakes);
            assert_eq!(table.get(&1).unwrap().waiters, 0);
            assert_eq!(table.get(&2).unwrap().waiters, 1);
            assert_eq!(live.get(), 1, "cancellation is not completion");
            let resources = table.get_mut(&1).unwrap().operations.take(1).unwrap();
            assert!(
                table.remove_quiescent(&1).is_none(),
                "occupied during resource drop"
            );
            drop(resources);
            assert_eq!(live.get(), 0);
            assert!(
                table.remove_quiescent(&1).is_none(),
                "completion must clear the tombstone"
            );
            table.get_mut(&1).unwrap().operations.complete(1).unwrap();
            assert!(table.remove_quiescent(&1).is_some());
            assert_eq!(table.len(), 1);
        }

        /// Occupied entries retain live resources until explicit completion.
        #[test]
        fn duplicate_operation_preserves_resource_and_tombstone() {
            let mut operations = Operations::<u32>::default();
            assert_eq!(operations.insert(1, 7), Ok(()));
            assert_eq!(operations.insert(1, 8), Err(8));
            assert_eq!(operations.take(1), Ok(7));
            assert_eq!(operations.insert(1, 9), Err(9));
            assert_eq!(operations.take(1), Err(Stale));
            assert_eq!(operations.complete(1), Ok(()));
            assert!(operations.is_empty());
        }

        /// Occupied entries retain live resources until explicit completion.
        #[test]
        fn occupied_live_entry_rejects_insertion() {
            occupied_entry_rejects_insertion(false, false);
        }

        /// Taking resources does not let insertion bypass the completion tombstone.
        #[test]
        fn occupied_tombstone_rejects_insertion() {
            occupied_entry_rejects_insertion(true, false);
        }

        /// Even quiescent entries must be explicitly removed before replacement.
        #[test]
        fn occupied_quiescent_entry_rejects_insertion() {
            occupied_entry_rejects_insertion(true, true);
        }

        /// Rejection preserves identity, resource ownership, and sweep membership.
        fn occupied_entry_rejects_insertion(take: bool, complete: bool) {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            let original = insert(&mut table, &owner, 1);
            let live = Rc::new(Cell::new(1));
            let entry = table.get_mut(&1).unwrap();
            entry.waiters = 0;
            assert!(entry.operations.insert(1, Resource(live.clone())).is_ok());
            if take {
                drop(entry.operations.take(1).unwrap());
            }
            if complete {
                entry.operations.complete(1).unwrap();
            }
            let sweep_id = table.entries[&1].sweep_id;
            let next_sweep_id = table.next_sweep_id;
            let rejected_live = Rc::new(Cell::new(1));
            let mut operations = Operations::default();
            assert!(
                operations
                    .insert(2, Resource(rejected_live.clone()))
                    .is_ok()
            );
            let replacement = table.identity(owner).unwrap();
            let rejected = table.insert(
                1,
                TestEntry {
                    identity: replacement,
                    waiters: 1,
                    canceled: false,
                    refreshed: 0,
                    operations,
                },
            );
            assert!(rejected.is_err());
            assert!(original.same_registration(&table.get(&1).unwrap().identity));
            assert_eq!(live.get(), usize::from(!take));
            assert_eq!(rejected_live.get(), 1);
            assert_eq!(table.len(), 1);
            assert_eq!(table.entries[&1].sweep_id, sweep_id);
            assert_eq!(table.next_sweep_id, next_sweep_id);
            assert_eq!(table.sweep.len(), 1);
            assert_eq!(table.sweep[&sweep_id], 1);
            drop(rejected);
            assert_eq!(rejected_live.get(), 0);
            let removed = table.sweep(1, &mut Txn::new(Instant::now()));
            assert_eq!(removed, usize::from(complete));
            if !complete {
                let entry = table.get_mut(&1).unwrap();
                assert_eq!(entry.refreshed, 1);
                assert_eq!(entry.operations.len(), 1);
                if !take {
                    drop(entry.operations.take(1).unwrap());
                }
                entry.operations.complete(1).unwrap();
                assert!(table.remove_quiescent(&1).is_some());
            }
            assert!(table.is_empty());
            assert!(table.sweep.is_empty());
        }

        /// Request handle that detaches its waiter without completing operations.
        struct Waiter {
            table: Rc<RefCell<TestTable>>,

            key: u32,
        }

        impl Drop for Waiter {
            /// Detach this request and remove its entry only if truly quiescent.
            fn drop(&mut self) {
                update(&self.table, |table, txn| {
                    table.get_mut(&self.key).unwrap().waiters -= 1;
                    txn.remove_quiescent(table, &self.key);
                });
            }
        }

        /// Lost request and identity handles do not own operation resource release.
        #[test]
        fn dropped_waiter_and_completion_token_do_not_release_owned_operations() {
            let owner = Rc::new(());
            let table = Rc::new(RefCell::new(TestTable::default()));
            let token = insert(&mut table.borrow_mut(), &owner, 1);
            let live = Rc::new(Cell::new(1));
            assert!(
                table
                    .borrow_mut()
                    .get_mut(&1)
                    .unwrap()
                    .operations
                    .insert(1, Resource(live.clone()))
                    .is_ok()
            );
            drop(Waiter {
                table: table.clone(),
                key: 1,
            });
            drop(token);
            update(&table, |table, wakes| table.sweep(100, wakes));
            assert_eq!(table.borrow().len(), 1);
            assert_eq!(live.get(), 1);
            let resources = table
                .borrow_mut()
                .get_mut(&1)
                .unwrap()
                .operations
                .take(1)
                .unwrap();
            drop(resources);
            table
                .borrow_mut()
                .get_mut(&1)
                .unwrap()
                .operations
                .complete(1)
                .unwrap();
            update(&table, |table, wakes| table.sweep(1, wakes));
            assert!(table.borrow().is_empty());
            assert_eq!(live.get(), 0);
        }

        /// Owner, incarnation, generation, and resource-take fences are independent.
        #[test]
        fn reelection_and_recreation_fence_stale_completions() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            insert(&mut table, &owner, 1);
            let entry = table.get_mut(&1).unwrap();
            entry.identity.advance(2).unwrap();
            let old = entry.identity.clone();
            let live = Rc::new(Cell::new(1));
            assert!(entry.operations.insert(1, Resource(live.clone())).is_ok());
            assert_eq!(
                entry.operations.complete(1),
                Err(Stale),
                "cannot skip resource release"
            );
            let resources = entry.operations.take(1).unwrap();
            assert!(matches!(entry.operations.take(1), Err(Stale)));
            drop(resources);
            entry.operations.complete(1).unwrap();
            assert_eq!(entry.operations.complete(1), Err(Stale));
            entry.identity.advance(2).unwrap();
            assert_eq!(old.validate(&entry.identity), Err(Stale));
            assert!(
                old.same_registration(&entry.identity),
                "waiter survives retry"
            );
            assert_eq!(entry.identity.advance(2), Err(Exhausted));
            assert_eq!(entry.identity.generation, 2);
            let mut wrong_owner = old.clone();
            wrong_owner.owner = Rc::new(());
            assert_eq!(wrong_owner.validate(&old), Err(Stale));
            entry.waiters = 0;
            assert!(table.remove_quiescent(&1).is_some());
            let new = insert(&mut table, &owner, 1);
            assert!(!old.same_registration(&new));
            assert_eq!(old.validate(&new), Err(Stale));
            assert_eq!(live.get(), 0);
        }

        /// Each budget unit visits one entry and final removal notifies drain once.
        #[test]
        fn sweeps_are_budgeted_fair_and_remove_index_entries() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            for key in 1..=3 {
                insert(&mut table, &owner, key);
            }
            table.sweep(0, &mut Txn::new(Instant::now()));
            assert!(table.values().all(|entry| entry.refreshed == 0));
            for key in 1..=3 {
                table.sweep(1, &mut Txn::new(Instant::now()));
                assert_eq!(table.get(&key).unwrap().refreshed, 1);
            }
            table.get_mut(&2).unwrap().canceled = true;
            table.sweep(99, &mut Txn::new(Instant::now()));
            assert!(!table.contains_key(&2));
            assert_eq!(table.sweep.len(), 2);
            assert!(table.values().all(|entry| entry.refreshed == 2));
            table.drain_waker = Some(Waker::noop().clone());
            table.stop(&mut Txn::new(Instant::now()), |entry, _| {
                entry.canceled = true;
            });
            let mut wakes = Txn::new(Instant::now());
            table.sweep(99, &mut wakes);
            assert!(table.is_empty());
            assert!(table.sweep.is_empty());
            assert_eq!(wakes.len(), 1);
            table.sweep(99, &mut wakes);
            assert_eq!(wakes.len(), 1, "drain wake is taken once");
        }

        /// Cursor wrap is safe after removal while externally visible IDs never wrap.
        #[test]
        fn cursor_wraps_after_removal_and_counters_never_wrap() {
            let mut entries = BTreeMap::from([(1, ()), (2, ()), (3, ())]);
            let mut cursor = Cursor::default();
            assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(1));
            entries.remove(&1);
            assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(2));
            assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(3));
            assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(2));
            entries.clear();
            assert!(cursor.next(&entries).is_none());
            let mut counter = Counter(u64::MAX - 1);
            assert_eq!(counter.next_id(), Ok(u64::MAX));
            assert_eq!(counter.next_id(), Err(Exhausted));
            assert_eq!(counter.next_id(), Err(Exhausted));
            let mut table = TestTable {
                incarnation: counter,
                ..TestTable::default()
            };
            assert!(matches!(table.identity(Rc::new(())), Err(Exhausted)));
            assert!(table.is_empty());
        }

        /// Replacing and removing entries updates only their table-owned sweep slots.
        #[test]
        fn replacement_and_controlled_access_keep_sweep_membership_synchronized() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            assert!(table.get(&1).is_none());
            assert!(table.get_mut(&1).is_none());
            assert!(table.remove_quiescent(&1).is_none());
            insert(&mut table, &owner, 1);
            let original = table.entries[&1].sweep_id;
            table.get_mut(&1).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&1).is_some());
            insert(&mut table, &owner, 1);
            let replacement = table.entries[&1].sweep_id;
            insert(&mut table, &owner, 2);
            assert_eq!(table.len(), 2);
            assert_eq!(table.values().count(), 2);
            assert!(!table.sweep.contains_key(&original));
            assert!(table.sweep.contains_key(&replacement));
            table.sweep(2, &mut Txn::new(Instant::now()));
            assert!(table.values().all(|entry| entry.refreshed == 1));
            table.get_mut(&1).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&1).is_some());
            assert!(!table.contains_key(&1));
            assert_eq!(table.sweep.len(), table.len());
            table.sweep(1, &mut Txn::new(Instant::now()));
            assert_eq!(table.get(&2).unwrap().refreshed, 2);
        }

        /// Mutable application identities cannot corrupt another entry's membership.
        #[test]
        fn mutable_incarnation_cannot_remove_another_entries_sweep_slot() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            insert(&mut table, &owner, 1);
            let other = insert(&mut table, &owner, 2);
            table.get_mut(&1).unwrap().identity.incarnation = other.incarnation;
            table.get_mut(&1).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&1).is_some());
            assert_eq!(table.sweep.len(), 1);
            table.sweep(1, &mut Txn::new(Instant::now()));
            assert_eq!(table.get(&2).unwrap().refreshed, 1);
            table.get_mut(&2).unwrap().waiters = 0;
            table.sweep(1, &mut Txn::new(Instant::now()));
            assert!(table.is_empty());
            assert!(table.sweep.is_empty());
        }

        /// Shutdown hooks may mutate identities without invalidating the sweep index.
        #[test]
        fn stop_identity_mutation_preserves_replacement_and_removal_membership() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            for key in 1..=3 {
                insert(&mut table, &owner, key);
            }
            table.stop(&mut Txn::new(Instant::now()), |entry, _| {
                entry.identity.incarnation = u64::MAX;
                entry.canceled = true;
            });
            table.get_mut(&2).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&2).is_some());
            insert(&mut table, &owner, 2);
            assert_eq!(table.sweep.len(), 3);
            table.sweep(3, &mut Txn::new(Instant::now()));
            assert_eq!(table.len(), 1);
            assert_eq!(table.sweep.len(), 1);
            assert_eq!(table.get(&2).unwrap().refreshed, 1);
            table.get_mut(&2).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&2).is_some());
            assert!(table.sweep.is_empty());
        }

        /// Duplicate application incarnations remain independent table memberships.
        #[test]
        fn duplicate_entry_incarnations_have_independent_bounded_sweeps() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            let identity = table.identity(owner).unwrap();
            for key in 1..=3 {
                assert!(
                    table
                        .insert(
                            key,
                            TestEntry {
                                identity: identity.clone(),
                                waiters: 1,
                                canceled: false,
                                refreshed: 0,
                                operations: Operations::default(),
                            },
                        )
                        .is_ok()
                );
            }
            assert_eq!(table.sweep.len(), 3);
            table.sweep(0, &mut Txn::new(Instant::now()));
            assert!(table.values().all(|entry| entry.refreshed == 0));
            for key in 1..=3 {
                table.sweep(1, &mut Txn::new(Instant::now()));
                assert_eq!(table.get(&key).unwrap().refreshed, 1);
                assert_eq!(
                    table.values().map(|entry| entry.refreshed).sum::<usize>(),
                    key as usize
                );
            }
            table.get_mut(&2).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&2).is_some());
            table.sweep(99, &mut Txn::new(Instant::now()));
            assert_eq!(table.sweep.len(), 2);
            assert!(table.values().all(|entry| entry.refreshed == 2));
        }

        /// Internal sweep IDs wrap safely without consuming monotonic incarnation IDs.
        #[test]
        fn sweep_ids_wrap_skip_occupied_slots_and_do_not_consume_incarnations() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            let first = insert(&mut table, &owner, 1);
            table.next_sweep_id = u64::MAX;
            let second = insert(&mut table, &owner, 2);
            assert_eq!(table.entries[&2].sweep_id, 0);
            let third = insert(&mut table, &owner, 3);
            assert_eq!(table.entries[&3].sweep_id, 2, "skip occupied ID 1");
            assert_eq!(
                (first.incarnation, second.incarnation, third.incarnation),
                (1, 2, 3)
            );
            table.sweep(3, &mut Txn::new(Instant::now()));
            assert!(table.values().all(|entry| entry.refreshed == 1));
            table.incarnation = Counter(u64::MAX);
            assert!(table.identity(owner).is_err());
            assert!(
                table
                    .insert(
                        4,
                        TestEntry {
                            identity: first,
                            waiters: 1,
                            canceled: false,
                            refreshed: 0,
                            operations: Operations::default(),
                        },
                    )
                    .is_ok()
            );
            assert_eq!(
                table.len(),
                4,
                "insert does not require an incarnation allocation"
            );
            table.stop(&mut Txn::new(Instant::now()), |entry, _| {
                entry.canceled = true
            });
            table.sweep(4, &mut Txn::new(Instant::now()));
            assert!(table.is_empty());
            assert!(table.sweep.is_empty());
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

    thread_local! {
        static ON_KEY_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        static ON_RESULT_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        static ON_RESULT_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        static ON_KEY_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    }

    /// A result whose clone and destructor can each reenter once.
    #[derive(Debug, Eq, PartialEq)]
    struct CallbackResult(Result<u32, &'static str>);

    impl Clone for CallbackResult {
        /// Release the callback slot before calling application code.
        fn clone(&self) -> Self {
            let callback = ON_RESULT_CLONE.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
            Self(self.0)
        }
    }

    impl Drop for CallbackResult {
        /// Consume the callback so nested result drops are harmless.
        fn drop(&mut self) {
            let callback = ON_RESULT_DROP.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
        }
    }

    /// Allow completed readers and a replacement to coexist.
    fn callback_table(attempts: usize) -> Rc<Table<u32, CallbackResult>> {
        Rc::new(Table::new(
            Limits {
                waiters_per_cohort: 4,
                attempts_per_cohort: attempts,
            },
            CallbackResult(Err("exhausted")),
        ))
    }

    /// Completed success and failure values allow all cohort operations during clone.
    #[test]
    fn result_callback_clone_can_event_retry_finish_and_detach() {
        for value in [Ok(7), Err("failed")] {
            let table = callback_table(1);
            let first = table.join(1, 1).unwrap();
            let follower = table.join(1, 1).unwrap();
            first.finish(CallbackResult(value));
            let nested = first.clone();
            let observed = Rc::new(Cell::new(false));
            let detached = Rc::new(RefCell::new(Some(follower)));
            ON_RESULT_CLONE.with(|slot| {
                let observed = observed.clone();
                let detached = detached.clone();
                *slot.borrow_mut() = Some(Box::new(move || {
                    nested.retry();
                    nested.finish(CallbackResult(Ok(99)));
                    assert_eq!(
                        nested.event(Waker::noop()),
                        Poll::Ready(Event::Complete(CallbackResult(value)))
                    );
                    drop(detached.borrow_mut().take());
                    observed.set(true);
                }));
            });
            assert_eq!(
                first.event(Waker::noop()),
                Poll::Ready(Event::Complete(CallbackResult(value)))
            );
            assert!(observed.get());
            assert_eq!(table.registration_count(), 1);
            assert_eq!(table.active_count(), 0);
            drop(first);
            assert_eq!(table.registration_count(), 0);
        }
    }

    /// A completion published while cloning exhaustion remains the first result.
    #[test]
    fn result_callback_exhaustion_clone_preserves_reentrant_completion() {
        for attempts in [0, 1] {
            let table = callback_table(attempts);
            let first = table.join(1, 1).unwrap();
            let follower = table.join(1, 1).unwrap();
            if attempts != 0 {
                assert_eq!(first.event(Waker::noop()), Poll::Ready(Event::Lead));
                first.retry();
            }
            let nested = first.clone();
            let observed = Rc::new(Cell::new(false));
            ON_RESULT_CLONE.with(|slot| {
                let table = table.clone();
                let observed = observed.clone();
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert_eq!(table.active_count(), 1);
                    nested.finish(CallbackResult(Ok(99)));
                    nested.retry();
                    assert_eq!(
                        nested.event(Waker::noop()),
                        Poll::Ready(Event::Complete(CallbackResult(Ok(99))))
                    );
                    observed.set(true);
                }));
            });
            let expected = Poll::Ready(Event::Complete(CallbackResult(Ok(99))));
            assert_eq!(first.event(Waker::noop()), expected);
            assert_eq!(follower.event(Waker::noop()), expected);
            assert!(observed.get());
            drop((first, follower));
            assert_eq!(table.registration_count(), 0);
        }
    }

    /// A panicking application clone does not poison the cohort or replace its result.
    #[test]
    fn result_callback_clone_panic_keeps_first_result_and_releases_borrow() {
        let table = callback_table(1);
        let first = table.join(1, 1).unwrap();
        first.finish(CallbackResult(Ok(7)));
        ON_RESULT_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(|| panic!("result clone failed")));
        });
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| first.event(Waker::noop())))
                .is_err()
        );
        first.retry();
        first.finish(CallbackResult(Ok(99)));
        assert_eq!(
            first.event(Waker::noop()),
            Poll::Ready(Event::Complete(CallbackResult(Ok(7))))
        );
        assert_eq!(table.active_count(), 0);
        drop(first);
        assert_eq!(table.registration_count(), 0);
    }

    /// Rejected values and final stored values are destroyed without internal borrows.
    #[test]
    fn result_callback_rejected_and_final_result_drop_can_reenter() {
        let table = callback_table(1);
        let first = table.join(1, 1).unwrap();
        first.finish(CallbackResult(Ok(7)));
        let observed = Rc::new(Cell::new(0));
        ON_RESULT_DROP.with(|slot| {
            let nested = first.clone();
            let observed = observed.clone();
            *slot.borrow_mut() = Some(Box::new(move || {
                nested.retry();
                nested.finish(CallbackResult(Ok(100)));
                assert_eq!(
                    nested.event(Waker::noop()),
                    Poll::Ready(Event::Complete(CallbackResult(Ok(7))))
                );
                observed.set(observed.get() + 1);
            }));
        });
        first.finish(CallbackResult(Ok(99)));
        assert_eq!(observed.get(), 1);
        ON_RESULT_DROP.with(|slot| {
            let table = table.clone();
            let observed = observed.clone();
            *slot.borrow_mut() = Some(Box::new(move || {
                assert_eq!(table.registration_count(), 0);
                assert_eq!(table.active_count(), 0);
                let next = table.join(1, 1).unwrap();
                next.finish(CallbackResult(Ok(8)));
                observed.set(observed.get() + 1);
            }));
        });
        drop(first);
        assert_eq!(observed.get(), 2);
        assert_eq!(table.registration_count(), 0);
    }

    /// A key whose next destructor can inspect or replace its table entry.
    #[derive(Clone, Debug, Eq, PartialEq, Ord, PartialOrd, Hash)]
    struct DropKey(u32);

    impl Drop for DropKey {
        /// Consume the callback before a replacement can drop another key.
        fn drop(&mut self) {
            let callback = ON_KEY_DROP.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
        }
    }

    /// Completion and final detach remove membership before destroying its key.
    #[test]
    fn result_callback_cohort_key_drop_can_admit_replacement() {
        for finish in [false, true] {
            let table = Rc::new(Table::<_, u32>::new(
                Limits {
                    waiters_per_cohort: 4,
                    attempts_per_cohort: 1,
                },
                0,
            ));
            let first = table.join(DropKey(1), 1).unwrap();
            let replacement = Rc::new(RefCell::new(None));
            ON_KEY_DROP.with(|slot| {
                let table = table.clone();
                let replacement = replacement.clone();
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert_eq!(table.active_count(), 0);
                    assert_eq!(table.registration_count(), usize::from(finish));
                    *replacement.borrow_mut() = Some(table.join(DropKey(1), 1).unwrap());
                }));
            });
            if finish {
                first.finish(7);
                assert_eq!(first.event(Waker::noop()), Poll::Ready(Event::Complete(7)));
            }
            drop(first);
            assert_eq!(table.active_count(), 1);
            let next = replacement.borrow_mut().take().unwrap();
            assert_eq!(next.event(Waker::noop()), Poll::Ready(Event::Lead));
            drop(next);
            assert_eq!(table.active_count(), 0);
            assert_eq!(table.registration_count(), 0);
        }
    }

    /// Shared completion leaves a reentrantly admitted replacement intact.
    #[test]
    fn result_callback_shared_key_drop_can_admit_replacement() {
        use futures::executor::block_on;

        let table = Rc::new(shared::Table::default());
        let (receive, completion) = table.start(DropKey(1), 0);
        let replacement = Rc::new(RefCell::new(None));
        ON_KEY_DROP.with(|slot| {
            let table = table.clone();
            let replacement = replacement.clone();
            *slot.borrow_mut() = Some(Box::new(move || {
                assert!(table.is_empty());
                *replacement.borrow_mut() = Some(table.start(DropKey(1), 0));
            }));
        });
        completion.unwrap().finish(7);
        assert_eq!(block_on(receive), 7);
        let (next, completion) = replacement.borrow_mut().take().unwrap();
        assert_eq!(table.len(), 1);
        completion.unwrap().finish(8);
        assert_eq!(block_on(next), 8);
        assert!(table.is_empty());
    }

    /// A key whose next clone can call back into its table.
    #[derive(Debug, Eq, PartialEq, Ord, PartialOrd, Hash)]
    struct CloneKey(u32);

    impl Clone for CloneKey {
        /// Run the one-shot callback without retaining the callback slot borrow.
        fn clone(&self) -> Self {
            let callback = ON_KEY_CLONE.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
            Self(self.0)
        }
    }

    /// A clone callback can admit the same key before the outer join resumes.
    #[test]
    fn key_clone_reentry_joins_current_cohort() {
        let table = Rc::new(Table::<_, u32>::new(
            Limits {
                waiters_per_cohort: 2,
                attempts_per_cohort: 1,
            },
            0,
        ));
        let admitted = Rc::new(RefCell::new(None));
        let nested = table.clone();
        let retained = admitted.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert_eq!(nested.active_count(), 0);
                *retained.borrow_mut() = Some(nested.join(CloneKey(1), 1).unwrap());
            }));
        });
        let outer = table.join(CloneKey(1), 1).unwrap();
        let inner = admitted.borrow_mut().take().unwrap();
        assert_eq!(table.active_count(), 1);
        assert_eq!(table.registration_count(), 2);
        assert_eq!(inner.event(Waker::noop()), Poll::Ready(Event::Lead));
        assert!(outer.event(Waker::noop()).is_pending());
        inner.finish(7);
        assert_eq!(outer.event(Waker::noop()), Poll::Ready(Event::Complete(7)));
        assert_eq!(table.active_count(), 0);
        drop((inner, outer));
        assert_eq!(table.registration_count(), 0);
    }

    /// Clone callbacks may fill key, waiter, or completed-reader capacity.
    #[test]
    fn key_clone_reentry_rechecks_all_bounds() {
        for (capacity, waiters, nested_key, complete) in
            [(1, 2, 2, false), (2, 1, 1, false), (1, 2, 2, true)]
        {
            let table = Rc::new(Table::<_, u32>::new(
                Limits {
                    waiters_per_cohort: waiters,
                    attempts_per_cohort: 1,
                },
                0,
            ));
            let admitted = Rc::new(RefCell::new(Vec::new()));
            let nested = table.clone();
            let retained = admitted.clone();
            ON_KEY_CLONE.with(|slot| {
                *slot.borrow_mut() = Some(Box::new(move || {
                    let count = if complete { capacity * waiters } else { 1 };
                    for _ in 0..count {
                        let registration = nested.join(CloneKey(nested_key), capacity).unwrap();
                        if complete {
                            registration.finish(7);
                        }
                        retained.borrow_mut().push(registration);
                    }
                }));
            });
            assert!(matches!(
                table.join(CloneKey(1), capacity),
                Err(CapacityError)
            ));
            assert_eq!(table.active_count(), usize::from(!complete));
            assert_eq!(table.registration_count(), admitted.borrow().len());
            admitted.borrow_mut().clear();
            assert_eq!(table.active_count(), 0);
            assert_eq!(table.registration_count(), 0);
            let next = table.join(CloneKey(1), capacity).unwrap();
            assert_eq!(table.registration_count(), 1);
            drop(next);
            assert_eq!(table.active_count(), 0);
            assert_eq!(table.registration_count(), 0);
        }
    }

    /// Shared key cloning can inspect the table and start unrelated work.
    #[test]
    fn key_clone_reentry_shared_start_preserves_both_completions() {
        use futures::executor::block_on;

        let table = Rc::new(shared::Table::default());
        let admitted = Rc::new(RefCell::new(None));
        let nested = table.clone();
        let retained = admitted.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert!(nested.is_empty());
                assert!(nested.get(&CloneKey(1)).is_none());
                *retained.borrow_mut() = Some(nested.start(CloneKey(2), 0));
            }));
        });
        let (outer, completion) = table.start(CloneKey(1), 0);
        let (inner, nested_completion) = admitted.borrow_mut().take().unwrap();
        assert_eq!(table.len(), 2);
        completion.unwrap().finish(7);
        assert_eq!(block_on(outer), 7);
        assert_eq!(table.len(), 1);
        assert!(table.get(&CloneKey(1)).is_none());
        let follower = table.get(&CloneKey(2)).unwrap();
        nested_completion.unwrap().finish(8);
        assert_eq!(block_on(inner), 8);
        assert_eq!(block_on(follower), 8);
        assert!(table.is_empty());
    }

    #[test]
    fn shared_same_key_clone_reentry_joins_nested_completion() {
        use futures::FutureExt;

        for result in [Ok(7), Err("failed")] {
            let table = Rc::new(shared::Table::default());
            let admitted = Rc::new(RefCell::new(None));
            let nested = table.clone();
            let retained = admitted.clone();
            ON_KEY_CLONE.with(|slot| {
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert!(nested.get(&CloneKey(1)).is_none());
                    *retained.borrow_mut() = Some(nested.start(CloneKey(1), Err("nested closed")));
                }));
            });
            assert!(table.get(&CloneKey(1)).is_none());
            let (outer, completion) = table.start(CloneKey(1), Err("outer closed"));
            let (inner, nested_completion) = admitted.borrow_mut().take().unwrap();
            let follower = table.get(&CloneKey(1)).unwrap();
            assert_eq!(table.len(), 1);
            assert!(completion.is_none());
            nested_completion.unwrap().finish(result);
            assert_eq!(inner.now_or_never(), Some(result));
            assert_eq!(outer.now_or_never(), Some(result));
            assert_eq!(follower.now_or_never(), Some(result));
            assert!(table.is_empty());
            let (next, next_completion) = table.start(CloneKey(1), Err("next closed"));
            drop(completion);
            assert_eq!(table.len(), 1);
            next_completion.unwrap().finish(Ok(8));
            assert_eq!(next.now_or_never(), Some(Ok(8)));
            assert!(table.is_empty());
        }
    }

    #[test]
    fn shared_same_key_clone_reentry_completed_before_start_keeps_new_owner() {
        use futures::FutureExt;

        let table = Rc::new(shared::Table::default());
        let admitted = Rc::new(RefCell::new(None));
        let nested = table.clone();
        let retained = admitted.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                let (receive, completion) = nested.start(CloneKey(1), 99);
                completion.unwrap().finish(7);
                *retained.borrow_mut() = Some(receive);
                assert!(nested.is_empty());
            }));
        });
        let (outer, completion) = table.start(CloneKey(1), 99);
        assert_eq!(table.len(), 1);
        let inner = admitted.borrow_mut().take().unwrap();
        assert_eq!(inner.now_or_never(), Some(7));
        let follower = table.get(&CloneKey(1)).unwrap();
        assert!(outer.clone().now_or_never().is_none());
        completion.unwrap().finish(8);
        assert_eq!(outer.now_or_never(), Some(8));
        assert_eq!(follower.now_or_never(), Some(8));
        assert!(table.is_empty());
    }

    #[test]
    fn shared_same_key_clone_reentry_lost_sender_stays_occupied() {
        use futures::FutureExt;

        let table = Rc::new(shared::Table::default());
        let nested = table.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                let (receive, completion) = nested.start(CloneKey(1), 7);
                drop((receive, completion));
                assert_eq!(nested.len(), 1);
            }));
        });
        let (outer, completion) = table.start(CloneKey(1), 99);
        assert!(completion.is_none());
        assert_eq!(outer.now_or_never(), Some(7));
        assert_eq!(table.len(), 1);
        let (late, completion) = table.start(CloneKey(1), 88);
        assert!(completion.is_none());
        assert_eq!(late.now_or_never(), Some(7));
        assert_eq!(table.len(), 1);
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

    /// The first result survives later finishes and remains separate from replacements.
    #[test]
    fn first_completion_wins_for_early_and_late_readers() {
        for result in [Ok(42), Err("failed")] {
            let table = table(5, 2);
            let leader = table.join("key", 1).unwrap();
            let early = table.join("key", 1).unwrap();
            let late = table.join("key", 1).unwrap();
            let survivor = leader.clone();
            let count = Arc::new(Counter::default());
            let waker = Waker::from(count.clone());
            assert_eq!(leader.event(Waker::noop()), Poll::Ready(Event::Lead));
            assert_eq!(early.event(&waker), Poll::Pending);
            leader.finish(result);
            drop(leader);
            assert_eq!(count.0.load(Ordering::Relaxed), 1);
            assert_eq!(early.event(&waker), Poll::Ready(Event::Complete(result)));
            assert_eq!(table.active_count(), 0);

            let next = table.join("key", 1).unwrap();
            assert_eq!(next.event(Waker::noop()), Poll::Ready(Event::Lead));
            survivor.finish(Ok(99));
            early.finish(Err("late failure"));
            assert_eq!(count.0.load(Ordering::Relaxed), 1);
            assert_eq!(early.event(&waker), Poll::Ready(Event::Complete(result)));
            assert_eq!(
                late.event(Waker::noop()),
                Poll::Ready(Event::Complete(result))
            );
            assert_eq!(
                survivor.event(Waker::noop()),
                Poll::Ready(Event::Complete(result))
            );
            assert_eq!(table.active_count(), 1);
            let next_follower = table.join("key", 1).unwrap();
            assert_eq!(next_follower.event(Waker::noop()), Poll::Pending);
            next.finish(Ok(7));
            assert_eq!(
                next_follower.event(Waker::noop()),
                Poll::Ready(Event::Complete(Ok(7)))
            );
            assert_eq!(
                late.event(Waker::noop()),
                Poll::Ready(Event::Complete(result))
            );
            drop((survivor, early, late, next, next_follower));
            assert_eq!(table.registration_count(), 0);
            assert_eq!(table.active_count(), 0);
        }
    }

    /// Attempt exhaustion is also a terminal result, even for unpolled followers.
    #[test]
    fn first_completion_from_exhaustion_cannot_be_overwritten() {
        let table = table(2, 0);
        let first = table.join("key", 1).unwrap();
        let late = table.join("key", 1).unwrap();
        let exhausted = Poll::Ready(Event::Complete(Err("exhausted")));
        assert_eq!(first.event(Waker::noop()), exhausted);
        first.finish(Ok(1));
        late.finish(Err("late failure"));
        assert_eq!(first.event(Waker::noop()), exhausted);
        assert_eq!(late.event(Waker::noop()), exhausted);
        assert_eq!(table.active_count(), 0);
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
    fn canceled_request_keeps_driver_and_final_attempt_until_real_completion() {
        for success in [false, true] {
            let table = table(3, 1);
            let request = table.join("key", 1).unwrap();
            let follower = table.join("key", 1).unwrap();
            assert_eq!(request.event(Waker::noop()), Poll::Ready(Event::Lead));
            let driver = request.clone();
            drop(request);
            // Cancellation drops the request, not the clone held by actual work.
            assert!(follower.event(Waker::noop()).is_pending());
            assert_eq!(table.active_count(), 1);
            assert_eq!(table.registration_count(), 2);
            if success {
                driver.finish(Ok(7));
            } else {
                // Only after work ends can its driver release election ownership.
                driver.retry();
            }
            let expected = if success { Ok(7) } else { Err("exhausted") };
            assert_eq!(
                follower.event(Waker::noop()),
                Poll::Ready(Event::Complete(expected))
            );
            drop((driver, follower));
            assert_eq!(table.registration_count(), 0);
            assert_eq!(table.active_count(), 0);
        }
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
        let no_waiters = table(0, 1);
        assert!(matches!(no_waiters.join("key", 1), Err(CapacityError)));
        assert_eq!(no_waiters.active_count(), 0);
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
