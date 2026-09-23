// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::{Fill, Key, WorkerPool};
use std::{
    collections::BTreeMap,
    io,
    task::{Poll, Waker},
    time::Instant,
};

struct Waiter {
    deadline: Instant,
    waker: Waker,
}

pub(super) struct Free {
    pub(super) slots: Vec<usize>,
    // Rank-indexed FIFO queues: inspect only eligible queue heads on release.
    waiting: BTreeMap<usize, BTreeMap<u64, Waiter>>,
    granted: BTreeMap<u64, usize>,
    next: u64,
}
impl Free {
    #[cfg(test)]
    pub(super) fn is_idle(&self) -> bool {
        self.waiting.is_empty() && self.granted.is_empty()
    }
    #[cfg(test)]
    pub(super) fn grants(&self) -> impl Iterator<Item = &usize> {
        self.granted.values()
    }
    pub(super) fn new(count: usize) -> Self {
        Self {
            slots: (0..count).rev().collect(),
            waiting: BTreeMap::new(),
            granted: BTreeMap::new(),
            next: 0,
        }
    }
    fn remove(&mut self, rank: usize, id: u64) -> Option<Waiter> {
        let queue = self.waiting.get_mut(&rank)?;
        let waiter = queue.remove(&id);
        if queue.is_empty() {
            self.waiting.remove(&rank);
        }
        waiter
    }
    pub(super) fn dispatch(&mut self) -> Vec<Waker> {
        let mut wakes = Vec::new();
        if self.waiting.is_empty() {
            return wakes;
        }
        let now = crate::environment::now();
        while let Some((rank, id)) = self
            .waiting
            .range(..self.slots.len())
            .map(|(&rank, queue)| (rank, *queue.first_key_value().unwrap().0))
            .min_by_key(|&(_, id)| id)
        {
            let waiter = self.remove(rank, id).unwrap();
            if now < waiter.deadline {
                self.granted.insert(id, self.slots.pop().unwrap());
            }
            wakes.push(waiter.waker);
        }
        wakes
    }
}

pub(super) fn wake(wakes: Vec<Waker>) {
    for waker in wakes {
        // User callbacks run outside locks and cannot interrupt slot recycling.
        if let Err(payload) =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| waker.wake()))
        {
            if let Err(payload) =
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| drop(payload)))
            {
                std::mem::forget(payload);
            }
        }
    }
}

/// Worker-local, deadline-bounded allocation. Poll after a wake or at `deadline()`;
/// the caller's reactor must arrange the deadline wake. Drop cancels the wait and
/// returns any unclaimed grant. No waiter allocation occurs on the fast path.
#[must_use]
pub struct Acquisition {
    pool: WorkerPool,
    value: Option<Key>,
    reserve: usize,
    deadline: Instant,
    id: Option<u64>,
    done: bool,
}
impl Acquisition {
    pub fn deadline(&self) -> Instant {
        self.deadline
    }
    /// Pending registers the current waker atomically with the capacity check.
    /// A successful acquisition is exclusive even before the worker claims it.
    pub fn poll(&mut self, waker: &Waker) -> Poll<io::Result<Fill>> {
        assert!(!self.done, "completed buffer acquisition");
        let waker = waker.clone();
        let mut free = self.pool.node.free.lock().unwrap();
        // Serialize expiry with dispatch: a release can retire an expired waiter
        // while this worker is waiting for the lock.
        if crate::environment::now() >= self.deadline {
            drop(free);
            self.cancel();
            self.done = true;
            return Poll::Ready(Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "buffer acquisition deadline",
            )));
        }
        let index = if let Some(id) = self.id {
            if let Some(index) = free.granted.remove(&id) {
                self.id = None;
                index
            } else {
                let waiter = free
                    .waiting
                    .get_mut(&self.reserve)
                    .unwrap()
                    .get_mut(&id)
                    .unwrap();
                if !waiter.waker.will_wake(&waker) {
                    let old = std::mem::replace(&mut waiter.waker, waker);
                    drop(free);
                    drop(old);
                }
                return Poll::Pending;
            }
        } else if free.slots.len() > self.reserve {
            free.slots.pop().unwrap()
        } else {
            let id = free.next;
            free.next = id.checked_add(1).expect("buffer waiter ID overflow");
            free.waiting.entry(self.reserve).or_default().insert(
                id,
                Waiter {
                    deadline: self.deadline,
                    waker,
                },
            );
            self.id = Some(id);
            return Poll::Pending;
        };
        drop(free);
        self.done = true;
        Poll::Ready(Ok(self.pool.fill(index, self.value)))
    }
    fn cancel(&mut self) {
        let Some(id) = self.id.take() else { return };
        let (waiter, wakes) = {
            let mut free = self.pool.node.free.lock().unwrap();
            let waiter = free.remove(self.reserve, id);
            if let Some(index) = free.granted.remove(&id) {
                free.slots.push(index);
            }
            (waiter, free.dispatch())
        };
        wake(wakes);
        drop(waiter);
    }
}
impl Drop for Acquisition {
    fn drop(&mut self) {
        self.cancel();
    }
}
impl WorkerPool {
    pub fn wait_private_fill(&self, deadline: Instant) -> Acquisition {
        self.acquire(None, 0, deadline)
    }
    pub fn wait_stage(&self, key: Key, deadline: Instant) -> Acquisition {
        self.acquire(Some(key), 0, deadline)
    }
    pub(crate) fn wait_stage_reserved(
        &self,
        key: Key,
        reserve: usize,
        deadline: Instant,
    ) -> Acquisition {
        self.acquire(Some(key), reserve, deadline)
    }
    fn acquire(&self, value: Option<Key>, reserve: usize, deadline: Instant) -> Acquisition {
        Acquisition {
            pool: self.clone(),
            value,
            reserve,
            deadline,
            id: None,
            done: false,
        }
    }
}
