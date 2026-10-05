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

pub mod state;

/// Run a synchronous owner transaction, then notify outside its mutable borrow.
pub fn update<T, R>(owner: &RefCell<T>, f: impl FnOnce(&mut T, &mut Vec<Waker>) -> R) -> R {
    let mut wakes = Vec::new();
    let result = f(&mut owner.borrow_mut(), &mut wakes);
    for waker in wakes {
        waker.wake();
    }
    result
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Exhausted;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Stale;

/// Monotonic IDs are never reused, even after an entry is removed.
#[derive(Default)]
pub struct Counter(u64);
impl Counter {
    pub fn next_id(&mut self) -> Result<u64, Exhausted> {
        self.0 = self.0.checked_add(1).ok_or(Exhausted)?;
        Ok(self.0)
    }
}

/// Key equality belongs to the adapter; this identity fences owner, incarnation,
/// and acquisition generation independently of any application key schema.
#[derive(Clone)]
pub struct Identity {
    pub owner: Rc<()>,
    pub incarnation: u64,
    pub generation: u64,
}
impl Identity {
    pub fn same_registration(&self, current: &Self) -> bool {
        Rc::ptr_eq(&self.owner, &current.owner) && self.incarnation == current.incarnation
    }

    pub fn validate(&self, current: &Self) -> Result<(), Stale> {
        if !self.same_registration(current) || self.generation != current.generation {
            return Err(Stale);
        }
        Ok(())
    }

    /// Called only after the adapter confirms eligibility and completion fences.
    pub fn advance(&mut self, limit: u64) -> Result<(), Exhausted> {
        if self.generation >= limit {
            return Err(Exhausted);
        }
        self.generation = self.generation.checked_add(1).ok_or(Exhausted)?;
        Ok(())
    }
}

/// Stable round-robin selection without a scan, including wrap after removal.
#[derive(Default)]
pub struct Cursor(u64);
impl Cursor {
    pub fn next<'a, V>(&mut self, entries: &'a BTreeMap<u64, V>) -> Option<(u64, &'a V)> {
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

/// Application lifecycle hooks. Refresh must be bounded and only enqueue wakes.
/// Quiescence must include both detached waiters and actual retained operations,
/// never merely cancellation requested by a caller or expiration of a deadline.
pub trait Entry {
    fn incarnation(&self) -> u64;
    fn refresh(&mut self, wakes: &mut Vec<Waker>);
    fn quiescent(&self) -> bool;
}

/// Owned membership with a synchronized, bounded sweep index.
/// Direct map mutation is intentionally unavailable:
/// ```compile_fail
/// let mut table = coalesce::flight::Table::<u32, ()>::default();
/// table.entries.clear();
/// ```
pub struct Table<K, E, S = RandomState> {
    entries: HashMap<K, Indexed<E>, S>,
    sweep: BTreeMap<u64, K>,
    next_sweep_id: u64,
    cursor: Cursor,
    incarnation: Counter,
    pub next_waiter: Counter,
    pub next_operation: Counter,
    pub stopping: bool,
    pub drain_waker: Option<Waker>,
}

// Membership identity is owned by the table, never by mutable application state.
struct Indexed<E> {
    sweep_id: u64,
    entry: E,
}

impl<K, E, S: Default> Default for Table<K, E, S> {
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
        }
    }
}
impl<K: Clone + Eq + Hash, E: Entry, S: BuildHasher> Table<K, E, S> {
    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub fn get(&self, key: &K) -> Option<&E> {
        self.entries.get(key).map(|indexed| &indexed.entry)
    }

    /// Mutate application state without affecting table-owned sweep membership.
    /// The adapter remains responsible for its own incarnation-fence semantics.
    pub fn get_mut(&mut self, key: &K) -> Option<&mut E> {
        self.entries.get_mut(key).map(|indexed| &mut indexed.entry)
    }

    pub fn contains_key(&self, key: &K) -> bool {
        self.entries.contains_key(key)
    }

    pub fn values(&self) -> impl Iterator<Item = &E> {
        self.entries.values().map(|indexed| &indexed.entry)
    }

    pub fn identity(&mut self, owner: Rc<()>) -> Result<Identity, Exhausted> {
        Ok(Identity {
            owner,
            incarnation: self.incarnation.next_id()?,
            generation: 0,
        })
    }

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

    /// Used after explicit detach or completion as well as by background sweeps.
    pub fn remove_quiescent(&mut self, key: &K) -> bool {
        if !self.get(key).is_some_and(Entry::quiescent) {
            return false;
        }
        let entry = self.entries.remove(key).expect("quiescent entry");
        self.sweep.remove(&entry.sweep_id);
        true
    }

    pub fn notify_drain(&mut self, wakes: &mut Vec<Waker>) {
        if let Some(waker) = self.drain_waker.take() {
            wakes.push(waker);
        }
    }

    /// Stop admission synchronously; the adapter chooses the per-entry error and
    /// settlement hooks, and remains responsible for driving actual completions.
    pub fn stop(&mut self, wakes: &mut Vec<Waker>, mut stop: impl FnMut(&mut E, &mut Vec<Waker>)) {
        self.stopping = true;
        for entry in self.entries.values_mut() {
            stop(&mut entry.entry, wakes);
        }
    }

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
    slots: HashMap<u64, Option<R>, S>,
}
impl<R, S: Default> Default for Operations<R, S> {
    fn default() -> Self {
        Self {
            slots: HashMap::default(),
        }
    }
}
impl<R, S: BuildHasher> Operations<R, S> {
    pub fn len(&self) -> usize {
        self.slots.len()
    }

    pub fn is_empty(&self) -> bool {
        self.slots.is_empty()
    }

    pub fn insert(&mut self, id: u64, resources: R) {
        self.slots.insert(id, Some(resources));
    }

    pub fn take(&mut self, id: u64) -> Result<R, Stale> {
        self.slots.get_mut(&id).and_then(Option::take).ok_or(Stale)
    }

    pub fn complete(&mut self, id: u64) -> Result<(), Stale> {
        if !matches!(self.slots.get(&id), Some(None)) {
            return Err(Stale);
        }
        self.slots.remove(&id);
        Ok(())
    }
}

#[cfg(test)]
mod tests;
