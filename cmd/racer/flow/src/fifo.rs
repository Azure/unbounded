//! Deadline-aware FIFO state for externally locked, completion-owned admission.

use std::{collections::BTreeMap, task::Waker, time::Instant};

/// Queue bookkeeping, independent of charges, request scopes, and error policy.
/// The owner holds one lock across admission, registration, and release. Returned
/// wakers must be invoked after that lock is released.
pub struct Fifo<S> {
    next: u64,

    active: usize,

    queue: BTreeMap<u64, Entry<S>>,

    cursor: Option<u64>,
}

/// A queued scope and its nonrenewable local deadline.
struct Entry<S> {
    waker: Option<Waker>,

    scope: S,

    deadline: Instant,
}

impl<S> Default for Fifo<S> {
    /// Start without live admissions or consumed identifiers.
    fn default() -> Self {
        Self {
            next: 0,
            active: 0,
            queue: BTreeMap::new(),
            cursor: None,
        }
    }
}

impl<S> Fifo<S> {
    /// Return the number of live completion owners.
    pub fn active(&self) -> usize {
        self.active
    }

    /// Return the number of retained queue slots.
    pub fn queued(&self) -> usize {
        self.queue.len()
    }

    /// Reserve an identifier without mutating state, before fallible charging.
    pub fn next(&self) -> Option<u64> {
        self.next.checked_add(1).map(|_| self.next)
    }

    /// Commit the previously checked identifier under the same owner lock.
    pub fn enqueue(&mut self, id: u64, scope: S, deadline: Instant) {
        assert_eq!(id, self.next);
        self.next = id.checked_add(1).expect("checked queue identifier");
        self.queue.insert(
            id,
            Entry {
                waker: None,
                scope,
                deadline,
            },
        );
    }

    /// Whether this ticket is the FIFO head and a completion slot is available.
    pub fn can_admit(&self, id: u64, limit: usize) -> bool {
        self.active < limit
            && self
                .queue
                .first_key_value()
                .is_some_and(|(first, _)| *first == id)
    }

    /// Transfer a queue slot into a completion-owned active slot.
    pub fn admit(&mut self, id: u64) -> Option<Waker> {
        assert!(self.queue.contains_key(&id));
        self.active += 1;
        self.remove(id)
    }

    /// Register while holding the same lock used to release capacity.
    pub fn register(&mut self, id: u64, waker: &Waker) -> bool {
        let Some(entry) = self.queue.get_mut(&id) else {
            return false;
        };
        entry.waker = Some(waker.clone());
        true
    }

    /// Remove a queued ticket without releasing any completion owner's slot.
    pub fn remove(&mut self, id: u64) -> Option<Waker> {
        self.queue.remove(&id);
        if self.queue.is_empty() {
            self.queue = BTreeMap::new();
        }
        self.wake()
    }

    /// Release exactly one active completion owner and notify the FIFO head.
    pub fn release(&mut self) -> Option<Waker> {
        self.active -= 1;
        self.wake()
    }

    /// Inspect bounded round-robin entries without changing their ownership.
    pub fn poll_deadlines(
        &mut self,
        budget: usize,
        mut expired: impl FnMut(&S, Instant) -> bool,
    ) -> Vec<Waker> {
        let start = self
            .cursor
            .map_or(std::ops::Bound::Unbounded, std::ops::Bound::Excluded);
        let mut last = None;
        let mut wakes = Vec::new();
        for (id, entry) in self
            .queue
            .range((start, std::ops::Bound::Unbounded))
            .take(budget)
        {
            last = Some(*id);
            if expired(&entry.scope, entry.deadline)
                && let Some(waker) = &entry.waker
            {
                wakes.push(waker.clone());
            }
        }
        self.cursor = last;
        wakes
    }

    /// Clone the current head notification without invoking it under the lock.
    fn wake(&self) -> Option<Waker> {
        self.queue
            .first_key_value()
            .and_then(|(_, entry)| entry.waker.clone())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Middle cancellation cannot enable barging or release active ownership.
    #[test]
    fn fifo_cancellation_and_completion_ownership() {
        let mut queue = Fifo::default();
        let now = Instant::now();
        for id in 0..3 {
            assert_eq!(queue.next(), Some(id));
            queue.enqueue(id, (), now);
        }
        assert!(!queue.can_admit(2, 1));
        queue.remove(1);
        assert!(queue.can_admit(0, 1));
        queue.admit(0);
        assert_eq!((queue.active(), queue.queued()), (1, 1));
        queue.remove(0);
        assert!(!queue.can_admit(2, 1));
        queue.release();
        assert!(queue.can_admit(2, 1));
        queue.admit(2);
        queue.release();
        assert_eq!((queue.active(), queue.queued()), (0, 0));
    }

    /// Alarm scans are bounded, wrap, and consult caller cancellation policy.
    #[test]
    fn deadline_scan_wrap_and_identifier_exhaustion() {
        let mut queue = Fifo::default();
        let now = Instant::now();
        for id in 0..3 {
            queue.enqueue(id, id, now);
            queue.register(id, futures::task::noop_waker_ref());
        }
        let mut visited = Vec::new();
        for _ in 0..4 {
            let wakes = queue.poll_deadlines(1, |scope, deadline| {
                visited.push(*scope);
                deadline <= now
            });
            assert!(wakes.len() <= 1);
        }
        assert_eq!(visited, vec![0, 1, 2]);
        assert_eq!(queue.poll_deadlines(1, |_, _| true).len(), 1);
        assert!(
            queue
                .poll_deadlines(0, |_, _| panic!("zero scan"))
                .is_empty()
        );
        queue.next = u64::MAX;
        assert_eq!(queue.next(), None);
    }
}
