//! Worker-local keyed cohorts with bounded registration and leader re-election.
//!
//! Callers own execution, cancellation, admission policy, and result semantics.
//! A cloned registration retains the same waiter and leadership until its final
//! handle drops. Complete a cohort only after the real operation has finished.

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
        if self.limits.waiters_per_cohort == 0 {
            return Err(CapacityError);
        }
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
/// require_send::<flow_control::coalesce::Registration<u32, u32>>();
/// ```
/// ```compile_fail
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
    pub fn finish(&self, result: V) {
        let wakers = {
            let mut state = self.owner.cohort.borrow_mut();
            state.result = Some(result);
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
                state.take_wakers()
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
        wake_all(wakers);
    }
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

pub mod flight {
    //! Owned flight indexing, generation fences, and bounded lifecycle sweeps.
    //!
    //! Entry hooks own result interpretation and policy. A sweep refreshes each
    //! selected entry once and removes it only when the hook reports quiescence.
    //! Callers collect wakes while borrowed and dispatch them after releasing locks.

    use std::cell::RefCell;
    use std::collections::{BTreeMap, HashMap, hash_map::RandomState};
    use std::hash::{BuildHasher, Hash};
    use std::rc::Rc;
    use std::task::Waker;

    /// Run a synchronous owner transaction, then notify outside its mutable borrow.
    pub fn update<T, R>(owner: &RefCell<T>, f: impl FnOnce(&mut T, &mut Vec<Waker>) -> R) -> R {
        let mut wakes = Vec::new();
        let result = {
            let mut state = owner.borrow_mut();
            f(&mut state, &mut wakes)
        };
        super::wake_all(wakes);
        result
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
        fn refresh(&mut self, wakes: &mut Vec<Waker>);

        /// Report that neither waiters nor retained operations need this entry.
        fn quiescent(&self) -> bool;
    }

    /// Owned membership with a synchronized, bounded sweep index.
    /// Direct map mutation is intentionally unavailable:
    /// ```compile_fail
    /// let mut table = flow_control::coalesce::flight::Table::<u32, ()>::default();
    /// table.entries.clear();
    /// ```
    /// Even an empty table belongs to its worker:
    /// ```compile_fail
    /// fn require_send<T: Send>() {}
    /// require_send::<flow_control::coalesce::flight::Table<u32, ()>>();
    /// ```
    /// ```compile_fail
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

        /// Retain the latest drain task's wake target.
        pub fn register_drain(&mut self, waker: &Waker) {
            state::store_waker(&mut self.drain_waker, waker);
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
        /// The adapter must have checked admission before inserting a new identity.
        pub fn insert(&mut self, key: K, entry: E) {
            let sweep_id = self.allocate_sweep_id();
            if let Some(previous) = self
                .entries
                .insert(key.clone(), Indexed { sweep_id, entry })
            {
                self.sweep.remove(&previous.sweep_id);
            }
            self.sweep.insert(sweep_id, key);
        }

        /// Find a free internal sweep slot without consuming incarnation IDs.
        fn allocate_sweep_id(&mut self) -> u64 {
            // Unlike externally visible incarnation fences, these IDs can be reused
            // after removal. At most len + 1 probes find a free ID, even after wrap.
            // This keeps insert infallible without coupling it to identity().
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
        pub fn remove_quiescent(&mut self, key: &K) -> bool {
            if !self.get(key).is_some_and(Entry::quiescent) {
                return false;
            }
            let entry = self.entries.remove(key).expect("quiescent entry");
            self.sweep.remove(&entry.sweep_id);
            true
        }
    }

    impl<K, E, S> Table<K, E, S> {
        /// Mark shutdown and visit each entry synchronously. The adapter must reject
        /// admission, choose settlement policy, and drive actual operation completions.
        pub fn stop(
            &mut self,
            wakes: &mut Vec<Waker>,
            mut stop: impl FnMut(&mut E, &mut Vec<Waker>),
        ) {
            self.stopping = true;
            for entry in self.entries.values_mut() {
                stop(&mut entry.entry, wakes);
            }
        }
    }

    impl<K: Clone + Eq + Hash, E: Entry, S: BuildHasher> Table<K, E, S> {
        /// Refresh at most the budgeted entry count and remove quiescent entries.
        pub fn sweep(&mut self, budget: usize, wakes: &mut Vec<Waker>) {
            for _ in 0..budget.min(self.sweep.len()) {
                let Some((_, key)) = self.cursor.next(&self.sweep) else {
                    break;
                };
                let key = key.clone();
                if let Some(entry) = self.get_mut(&key) {
                    entry.refresh(wakes);
                }
                self.remove_quiescent(&key);
            }
            if self.entries.is_empty() {
                self.notify_drain(wakes);
            }
        }
    }

    /// Two-phase completion slots. Taking a resource leaves its slot occupied while
    /// its destructor runs outside the table borrow. Only explicit completion clears
    /// that slot; dropping an external completion token does not touch this owner.
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
        pub fn insert(&mut self, id: u64, resources: R) {
            self.slots.insert(id, OperationSlot::Retained(resources));
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
            fn check(&self) -> Option<Self::Error>;

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

        /// Worker-local waiter index and transition state driven by an owning adapter.
        /// The adapter supplies actual operation idleness; cancellation is not idleness.
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
                    local: std::marker::PhantomData,
                }
            }
        }

        impl<C, P, O, W: WaiterPolicy> State<C, P, O, W> {
            /// Attach a caller using an identifier allocated by the owning table.
            pub fn register(&mut self, id: u64, policy: W, acquisition: bool, complete: bool) {
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
            }

            /// Remove a caller and its deadline without declaring operation completion.
            pub fn detach(&mut self, id: u64) {
                if let Some(waiter) = self.waiters.remove(&id) {
                    self.deadlines.remove(&(waiter.deadline(), id));
                }
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
            pub fn begin_completion(&mut self, outcome: Outcome<O, W::Error>) {
                self.phase = Phase::Draining(outcome);
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
            pub fn sweep_waiter(&mut self, wakes: &mut Vec<Waker>) {
                if let Some((id, _)) = self.cursor.next(&self.waiters) {
                    self.refresh_waiter(id, wakes);
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
            pub fn settle(
                &mut self,
                idle: bool,
                unavailable: W::Error,
                split: fn(O) -> Published<C, P>,
                wakes: &mut Vec<Waker>,
            ) {
                if !idle || !matches!(self.phase, Phase::Draining(_)) {
                    return;
                }
                let Phase::Draining(outcome) =
                    std::mem::replace(&mut self.phase, Phase::RetryPending)
                else {
                    unreachable!()
                };
                self.leader = None;
                self.phase = match outcome {
                    Outcome::Published(value) => match split(value) {
                        Published::Complete(value) => {
                            self.partial = None;
                            Phase::Complete(value)
                        }
                        Published::Partial(value) => {
                            self.partial = Some(value);
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
            }

            /// Revoke acquisition immediately, retaining its outcome behind completion.
            pub fn revoke(&mut self, error: W::Error, wakes: &mut Vec<Waker>) {
                if let Some(waiter) = self.leader.and_then(|id| self.waiters.get_mut(&id)) {
                    waiter.error = Some(error);
                }
                self.phase = Phase::Draining(Outcome::Retry);
                self.notify(wakes);
            }

            /// Check the leader and up to 64 expired callers, then settle eligible outcomes.
            pub fn refresh(
                &mut self,
                idle: bool,
                unavailable: W::Error,
                canceled: W::Error,
                now: impl Fn() -> Instant,
                split: fn(O) -> Published<C, P>,
                wakes: &mut Vec<Waker>,
            ) {
                if let Some(id) = self.leader {
                    self.refresh_waiter(id, wakes);
                }
                self.refresh_expired(&now, wakes);
                if matches!(self.phase, Phase::Acquiring) {
                    let leader = self.leader.and_then(|id| self.waiters.get(&id));
                    if leader.is_none_or(|waiter| waiter.error.is_some()) {
                        let error = leader.and_then(|waiter| waiter.error).unwrap_or(canceled);
                        self.revoke(error, wakes);
                    }
                }
                self.settle(idle, unavailable, split, wakes);
                if matches!(self.phase, Phase::RetryPending)
                    && self.partial.is_none()
                    && !self.waiters.values().any(Waiter::eligible)
                {
                    self.phase = Phase::Draining(Outcome::Failed(unavailable));
                    self.settle(idle, unavailable, split, wakes);
                }
            }

            /// Detach and notify all callers while preserving the operation drain fence.
            pub fn stop(&mut self, error: W::Error, wakes: &mut Vec<Waker>) {
                for waiter in self.waiters.values_mut() {
                    waiter.error = Some(error);
                }
                self.phase = Phase::Draining(Outcome::Failed(error));
                self.notify(wakes);
                self.waiters.clear();
                self.deadlines.clear();
            }

            /// Check one caller and enqueue its wake if its policy has failed.
            fn refresh_waiter(&mut self, id: u64, wakes: &mut Vec<Waker>) {
                let Some(waiter) = self.waiters.get_mut(&id) else {
                    return;
                };
                if waiter.error.is_none() {
                    waiter.error = waiter.check();
                }
                if waiter.error.is_some() {
                    self.deadlines.remove(&(waiter.deadline(), id));
                    if let Some(waker) = waiter.waker.take() {
                        wakes.push(waker);
                    }
                }
            }

            /// Consume at most 64 due deadline entries, independently of leader checks.
            fn refresh_expired(&mut self, now: &impl Fn() -> Instant, wakes: &mut Vec<Waker>) {
                for _ in 0..64 {
                    let Some((&(deadline, id), _)) = self.deadlines.first_key_value() else {
                        break;
                    };
                    if deadline > now() {
                        break;
                    }
                    self.deadlines.remove(&(deadline, id));
                    self.refresh_waiter(id, wakes);
                }
            }
        }

        /// Replace a wake target only when it would notify a different task.
        pub fn store_waker(slot: &mut Option<Waker>, waker: &Waker) {
            if slot.as_ref().is_none_or(|old| !old.will_wake(waker)) {
                *slot = Some(waker.clone());
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
                fn check(&self) -> Option<u8> {
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
                core.register(
                    id,
                    Policy {
                        due: Instant::now() + Duration::from_secs(60),
                        error: error.clone(),
                    },
                    acquisition,
                    complete,
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
                core.refresh(idle, 9, 8, Instant::now, split, wakes);
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
                        core.detach(1);
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
                core.begin_completion(Outcome::Published(Published::Partial(17)));
                core.settle(false, 9, split, &mut wakes);
                assert!(core.partial.is_none());
                core.settle(true, 9, split, &mut wakes);
                assert_eq!(core.partial, Some(17));
                assert!(!core.waiters[&1].issued);
                assert!(core.waiters[&2].issued);
                assert_eq!(core.elect(1, &mut identity, 3), Ok(true));
                core.begin_completion(Outcome::Published(Published::Complete(18)));
                core.settle(true, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Complete(18)));
                assert!(core.partial.is_none());
                assert_eq!(core.elect(1, &mut identity, 3), Ok(false));
                core.phase = Phase::Draining(Outcome::Failed(6));
                core.settle(false, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Draining(Outcome::Failed(6))));
                core.settle(true, 9, split, &mut wakes);
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
                core.revoke(7, &mut Vec::new());
                core.settle(true, 9, split, &mut Vec::new());
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
                    core.register(
                        id,
                        Policy {
                            due: now,
                            error: Rc::new(Cell::new(Some(3))),
                        },
                        true,
                        true,
                    );
                    let slot = &mut core.waiters.get_mut(&id).unwrap().waker;
                    store_waker(slot, &Waker::from(old.clone()));
                    store_waker(slot, &Waker::from(latest.clone()));
                }
                let mut wakes = Vec::new();
                core.refresh(true, 9, 8, || now, split, &mut wakes);
                assert_eq!(core.deadlines.len(), 1);
                assert_eq!(wakes.len(), 64);
                assert!(core.waiters[&64].error.is_none());
                core.refresh(true, 9, 8, || now, split, &mut wakes);
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
            fn detach_cleans_deadline_and_stop_preserves_pending_completion() {
                let mut core = Core::default();
                register(&mut core, 1, true, true);
                register(&mut core, 2, true, true);
                core.detach(1);
                assert_eq!(core.deadlines.len(), 1);
                let mut identity = identity();
                assert_eq!(core.elect(2, &mut identity, 3), Ok(true));
                core.cancel(2, 6);
                assert_eq!(core.waiters[&2].error, Some(6));
                let mut wakes = Vec::new();
                core.stop(8, &mut wakes);
                core.settle(false, 9, split, &mut wakes);
                assert!(matches!(core.phase, Phase::Draining(Outcome::Failed(8))));
                assert!(core.waiters.is_empty());
                assert!(core.deadlines.is_empty());
                core.settle(true, 9, split, &mut wakes);
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
            fn refresh(&mut self, _: &mut Vec<Waker>) {
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

        /// Admit an entry with one waiter and return its initial identity.
        fn insert(table: &mut TestTable, owner: &Rc<()>, key: u32) -> Identity {
            let identity = table.identity(owner.clone()).unwrap();
            table.insert(
                key,
                TestEntry {
                    identity: identity.clone(),
                    waiters: 1,
                    canceled: false,
                    refreshed: 0,
                    operations: Operations::default(),
                },
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
            entry.operations.insert(1, Resource(live.clone()));
            entry.canceled = true;
            let mut wakes = Vec::new();
            table.sweep(1, &mut wakes);
            assert_eq!(table.get(&1).unwrap().waiters, 0);
            assert_eq!(table.get(&2).unwrap().waiters, 1);
            assert_eq!(live.get(), 1, "cancellation is not completion");
            let resources = table.get_mut(&1).unwrap().operations.take(1).unwrap();
            assert!(!table.remove_quiescent(&1), "occupied during resource drop");
            drop(resources);
            assert_eq!(live.get(), 0);
            assert!(
                !table.remove_quiescent(&1),
                "completion must clear the tombstone"
            );
            table.get_mut(&1).unwrap().operations.complete(1).unwrap();
            assert!(table.remove_quiescent(&1));
            assert_eq!(table.len(), 1);
        }

        /// Request handle that detaches its waiter without completing operations.
        struct Waiter {
            table: Rc<RefCell<TestTable>>,

            key: u32,
        }

        impl Drop for Waiter {
            /// Detach this request and remove its entry only if truly quiescent.
            fn drop(&mut self) {
                let mut table = self.table.borrow_mut();
                table.get_mut(&self.key).unwrap().waiters -= 1;
                table.remove_quiescent(&self.key);
            }
        }

        /// Lost request and identity handles do not own operation resource release.
        #[test]
        fn dropped_waiter_and_completion_token_do_not_release_owned_operations() {
            let owner = Rc::new(());
            let table = Rc::new(RefCell::new(TestTable::default()));
            let token = insert(&mut table.borrow_mut(), &owner, 1);
            let live = Rc::new(Cell::new(1));
            table
                .borrow_mut()
                .get_mut(&1)
                .unwrap()
                .operations
                .insert(1, Resource(live.clone()));
            drop(Waiter {
                table: table.clone(),
                key: 1,
            });
            drop(token);
            table.borrow_mut().sweep(100, &mut Vec::new());
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
            table.borrow_mut().sweep(1, &mut Vec::new());
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
            entry.operations.insert(1, Resource(live.clone()));
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
            assert!(table.remove_quiescent(&1));
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
            table.sweep(0, &mut Vec::new());
            assert!(table.values().all(|entry| entry.refreshed == 0));
            for key in 1..=3 {
                table.sweep(1, &mut Vec::new());
                assert_eq!(table.get(&key).unwrap().refreshed, 1);
            }
            table.get_mut(&2).unwrap().canceled = true;
            table.sweep(99, &mut Vec::new());
            assert!(!table.contains_key(&2));
            assert_eq!(table.sweep.len(), 2);
            assert!(table.values().all(|entry| entry.refreshed == 2));
            table.drain_waker = Some(Waker::noop().clone());
            table.stop(&mut Vec::new(), |entry, _| {
                entry.canceled = true;
            });
            let mut wakes = Vec::new();
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
            assert!(!table.remove_quiescent(&1));
            insert(&mut table, &owner, 1);
            let original = table.entries[&1].sweep_id;
            insert(&mut table, &owner, 1);
            let replacement = table.entries[&1].sweep_id;
            insert(&mut table, &owner, 2);
            assert_eq!(table.len(), 2);
            assert_eq!(table.values().count(), 2);
            assert!(!table.sweep.contains_key(&original));
            assert!(table.sweep.contains_key(&replacement));
            table.sweep(2, &mut Vec::new());
            assert!(table.values().all(|entry| entry.refreshed == 1));
            table.get_mut(&1).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&1));
            assert!(!table.contains_key(&1));
            assert_eq!(table.sweep.len(), table.len());
            table.sweep(1, &mut Vec::new());
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
            assert!(table.remove_quiescent(&1));
            assert_eq!(table.sweep.len(), 1);
            table.sweep(1, &mut Vec::new());
            assert_eq!(table.get(&2).unwrap().refreshed, 1);
            table.get_mut(&2).unwrap().waiters = 0;
            table.sweep(1, &mut Vec::new());
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
            table.stop(&mut Vec::new(), |entry, _| {
                entry.identity.incarnation = u64::MAX;
                entry.canceled = true;
            });
            insert(&mut table, &owner, 2);
            assert_eq!(table.sweep.len(), 3);
            table.sweep(3, &mut Vec::new());
            assert_eq!(table.len(), 1);
            assert_eq!(table.sweep.len(), 1);
            assert_eq!(table.get(&2).unwrap().refreshed, 1);
            table.get_mut(&2).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&2));
            assert!(table.sweep.is_empty());
        }

        /// Duplicate application incarnations remain independent table memberships.
        #[test]
        fn duplicate_entry_incarnations_have_independent_bounded_sweeps() {
            let owner = Rc::new(());
            let mut table = TestTable::default();
            let identity = table.identity(owner).unwrap();
            for key in 1..=3 {
                table.insert(
                    key,
                    TestEntry {
                        identity: identity.clone(),
                        waiters: 1,
                        canceled: false,
                        refreshed: 0,
                        operations: Operations::default(),
                    },
                );
            }
            assert_eq!(table.sweep.len(), 3);
            table.sweep(0, &mut Vec::new());
            assert!(table.values().all(|entry| entry.refreshed == 0));
            for key in 1..=3 {
                table.sweep(1, &mut Vec::new());
                assert_eq!(table.get(&key).unwrap().refreshed, 1);
                assert_eq!(
                    table.values().map(|entry| entry.refreshed).sum::<usize>(),
                    key as usize
                );
            }
            table.get_mut(&2).unwrap().waiters = 0;
            assert!(table.remove_quiescent(&2));
            table.sweep(99, &mut Vec::new());
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
            table.sweep(3, &mut Vec::new());
            assert!(table.values().all(|entry| entry.refreshed == 1));
            table.incarnation = Counter(u64::MAX);
            assert!(table.identity(owner).is_err());
            table.insert(
                4,
                TestEntry {
                    identity: first,
                    waiters: 1,
                    canceled: false,
                    refreshed: 0,
                    operations: Operations::default(),
                },
            );
            assert_eq!(
                table.len(),
                4,
                "insert does not require an incarnation allocation"
            );
            table.stop(&mut Vec::new(), |entry, _| entry.canceled = true);
            table.sweep(4, &mut Vec::new());
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
