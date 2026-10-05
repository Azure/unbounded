//! Worker-local keyed cohorts with bounded registration and leader re-election.
//!
//! Callers own execution, cancellation, admission policy, and result semantics.
//! A cloned registration retains the same waiter and leadership until its final
//! handle drops. Complete a cohort only after the real operation has finished.

pub mod flight;
pub mod shared;

use std::cell::{Cell, RefCell};
use std::collections::{HashMap, hash_map::RandomState};
use std::hash::{BuildHasher, Hash};
use std::rc::Rc;
use std::task::{Poll, Waker};

/// Local bounds. Zero waiters rejects all joins; zero attempts immediately
/// broadcasts the caller-supplied exhausted value without electing a leader.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub waiters_per_cohort: usize,
    pub attempts_per_cohort: usize,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CapacityError;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Event<V> {
    Lead,
    Complete(V),
}

struct Cohort<V, S> {
    next: u64,
    leader: Option<u64>,
    attempts: usize,
    waiters: HashMap<u64, Option<Waker>, S>,
    result: Option<V>,
}

impl<V, S: Default> Default for Cohort<V, S> {
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
    active: RefCell<HashMap<K, Rc<RefCell<Cohort<V, S>>>, S>>,
    registrations: Cell<usize>,
    limits: Limits,
    exhausted: V,
}

impl<K, V, S: Default> Table<K, V, S> {
    pub fn new(limits: Limits, exhausted: V) -> Self {
        Self {
            active: RefCell::new(HashMap::default()),
            registrations: Cell::new(0),
            limits,
            exhausted,
        }
    }

    pub fn registration_count(&self) -> usize {
        self.registrations.get()
    }

    pub fn active_count(&self) -> usize {
        self.active.borrow().len()
    }
}

impl<K: Clone + Eq + Hash, V: Clone, S: BuildHasher + Default> Table<K, V, S> {
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
            table: self.clone(),
            key,
            cohort,
            id,
            lifetime: Rc::new(()),
        })
    }
}

pub struct Registration<K: Eq + Hash, V, S: BuildHasher = RandomState> {
    table: Rc<Table<K, V, S>>,
    key: K,
    cohort: Rc<RefCell<Cohort<V, S>>>,
    id: u64,
    lifetime: Rc<()>,
}

impl<K: Clone + Eq + Hash, V, S: BuildHasher> Clone for Registration<K, V, S> {
    fn clone(&self) -> Self {
        Self {
            table: self.table.clone(),
            key: self.key.clone(),
            cohort: self.cohort.clone(),
            id: self.id,
            lifetime: self.lifetime.clone(),
        }
    }
}

impl<K: Eq + Hash, V, S: BuildHasher> Registration<K, V, S> {
    /// Whether this is the last handle for this registration, not the cohort.
    pub fn is_only_handle(&self) -> bool {
        Rc::strong_count(&self.lifetime) == 1
    }

    /// Release leadership and notify the cohort after an attempt completes.
    /// Retry policy and whether this caller should detach belong to the caller.
    pub fn retry(&self) {
        let wakers = {
            let mut state = self.cohort.borrow_mut();
            if state.leader == Some(self.id) {
                state.leader = None;
            }
            take_wakers(&mut state)
        };
        for waker in wakers {
            waker.wake();
        }
    }

    fn remove_active(&self) {
        let mut active = self.table.active.borrow_mut();
        if active
            .get(&self.key)
            .is_some_and(|entry| Rc::ptr_eq(entry, &self.cohort))
        {
            active.remove(&self.key);
        }
    }

    /// Broadcast a caller-owned value and close admission before waking readers.
    /// The operation owner must call this only after completion, not cancellation.
    pub fn finish(&self, result: V) {
        let wakers = {
            let mut state = self.cohort.borrow_mut();
            state.result = Some(result);
            state.leader = None;
            take_wakers(&mut state)
        };
        self.remove_active();
        for waker in wakers {
            waker.wake();
        }
    }
}

impl<K: Eq + Hash, V: Clone, S: BuildHasher> Registration<K, V, S> {
    /// Register the latest wake target and elect at most once per attempt.
    pub fn event(&self, waker: &Waker) -> Poll<Event<V>> {
        let mut state = self.cohort.borrow_mut();
        if let Some(result) = &state.result {
            return Poll::Ready(Event::Complete(result.clone()));
        }
        state.waiters.insert(self.id, Some(waker.clone()));
        if state.leader.is_none() {
            if state.attempts >= self.table.limits.attempts_per_cohort {
                drop(state);
                let result = self.table.exhausted.clone();
                self.finish(result.clone());
                return Poll::Ready(Event::Complete(result));
            }
            state.attempts += 1;
            state.leader = Some(self.id);
            return Poll::Ready(Event::Lead);
        }
        Poll::Pending
    }
}

impl<K: Eq + Hash, V, S: BuildHasher> Drop for Registration<K, V, S> {
    fn drop(&mut self) {
        if !self.is_only_handle() {
            return;
        }
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

fn take_wakers<V, S>(state: &mut Cohort<V, S>) -> Vec<Waker> {
    state
        .waiters
        .values_mut()
        .filter_map(Option::take)
        .collect()
}

#[cfg(test)]
mod tests;
