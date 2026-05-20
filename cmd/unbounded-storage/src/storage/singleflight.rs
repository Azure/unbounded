// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-key singleflight gate.
//!
//! Concurrent fills for the same [`PageKey`] collapse onto a
//! single in-flight operation. The leader holds a
//! [`LeaseGuard`] and is responsible for either publishing a
//! result or letting the guard drop (which wakes followers with
//! a `None` so they fall back to their normal "treat as miss"
//! path).
//!
//! Internally the table is sharded by key hash so contention
//! between unrelated keys is bounded. Sharding is independent
//! of the singleflight semantics for any one key - all callers
//! for the same key always land on the same shard.

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use crate::storage::btree::LeafEntry;
use crate::storage::types::PageKey;

/// Outcome of a fill: `Some(entry)` if the leader published a
/// successful result, `None` if the leader dropped without
/// publishing (treated as a miss by callers).
pub type FillResult = Option<LeafEntry>;

/// Result of [`Singleflight::acquire`]: either you're the leader
/// and must do the work, or you're a follower waiting for
/// someone else's result.
pub enum Acquire {
    Leader(LeaseGuard),
    Follower(LeaseWait),
}

struct Lease {
    result: Mutex<LeaseInner>,
}

struct LeaseInner {
    result: Option<FillResult>,
    waiters: Vec<Waker>,
    // Once true the lease is "settled" - either published or
    // abandoned - and the table entry should be cleared.
    settled: bool,
}

pub struct Singleflight {
    shards: Vec<Mutex<HashMap<PageKey, Arc<Lease>>>>,
}

impl Singleflight {
    pub fn new(shards: usize) -> Self {
        let shards = shards.max(1);
        Self {
            shards: (0..shards).map(|_| Mutex::new(HashMap::new())).collect(),
        }
    }

    fn shard(&self, key: &PageKey) -> &Mutex<HashMap<PageKey, Arc<Lease>>> {
        let h = key.mix(0);
        &self.shards[(h as usize) % self.shards.len()]
    }

    pub fn acquire(self: &Arc<Self>, key: PageKey) -> Acquire {
        let shard = self.shard(&key);
        let mut map = shard.lock().unwrap();
        if let Some(existing) = map.get(&key).cloned() {
            return Acquire::Follower(LeaseWait {
                lease: existing,
            });
        }
        let lease = Arc::new(Lease {
            result: Mutex::new(LeaseInner {
                result: None,
                waiters: Vec::new(),
                settled: false,
            }),
        });
        map.insert(key, lease.clone());
        Acquire::Leader(LeaseGuard {
            owner: self.clone(),
            key,
            lease,
            armed: true,
        })
    }

    fn remove(&self, key: &PageKey) {
        let shard = self.shard(key);
        let mut map = shard.lock().unwrap();
        map.remove(key);
    }

    pub fn in_flight(&self) -> usize {
        self.shards.iter().map(|s| s.lock().unwrap().len()).sum()
    }
}

/// Held by the leader. Call [`publish`] to hand a result to
/// followers; dropping without publishing signals "give up" and
/// followers receive `None`.
pub struct LeaseGuard {
    owner: Arc<Singleflight>,
    key: PageKey,
    lease: Arc<Lease>,
    armed: bool,
}

impl LeaseGuard {
    pub fn publish(mut self, value: LeafEntry) {
        self.settle(Some(value));
    }

    pub fn abandon(mut self) {
        self.settle(None);
    }

    fn settle(&mut self, value: FillResult) {
        let wakers = {
            let mut inner = self.lease.result.lock().unwrap();
            inner.result = Some(value);
            inner.settled = true;
            std::mem::take(&mut inner.waiters)
        };
        self.owner.remove(&self.key);
        self.armed = false;
        for w in wakers {
            w.wake();
        }
    }
}

impl Drop for LeaseGuard {
    fn drop(&mut self) {
        if self.armed {
            self.settle(None);
        }
    }
}

/// Future returned to followers. Resolves once the leader
/// settles the lease.
pub struct LeaseWait {
    lease: Arc<Lease>,
}

impl Future for LeaseWait {
    type Output = FillResult;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut inner = self.lease.result.lock().unwrap();
        if inner.settled {
            return Poll::Ready(inner.result.clone().unwrap_or(None));
        }
        // Stash our waker if we don't already have one.
        if !inner.waiters.iter().any(|w| w.will_wake(cx.waker())) {
            inner.waiters.push(cx.waker().clone());
        }
        Poll::Pending
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::types::{Checksum, Lba};
    use std::future::Future;
    use std::pin::pin;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable =
            RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        unsafe { Waker::from_raw(raw()) }
    }

    fn key(i: u32) -> PageKey {
        PageKey::new([0u8; 32], i)
    }

    fn entry(lba: u64) -> LeafEntry {
        LeafEntry {
            lba: Lba(lba),
            data_checksum: Checksum(0),
            byte_len: 0,
        }
    }

    #[test]
    fn leader_followers_get_published_value() {
        let sf = Arc::new(Singleflight::new(4));
        let leader = match sf.acquire(key(1)) {
            Acquire::Leader(g) => g,
            Acquire::Follower(_) => panic!("expected leader"),
        };
        let follower = match sf.acquire(key(1)) {
            Acquire::Follower(f) => f,
            Acquire::Leader(_) => panic!("expected follower"),
        };
        leader.publish(entry(42));
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(follower);
        match f.as_mut().poll(&mut cx) {
            Poll::Ready(Some(e)) => assert_eq!(e.lba.0, 42),
            other => panic!("got {other:?}"),
        }
        assert_eq!(sf.in_flight(), 0);
    }

    #[test]
    fn leader_drop_signals_none() {
        let sf = Arc::new(Singleflight::new(4));
        let leader = match sf.acquire(key(7)) {
            Acquire::Leader(g) => g,
            _ => unreachable!(),
        };
        let follower = match sf.acquire(key(7)) {
            Acquire::Follower(f) => f,
            _ => unreachable!(),
        };
        drop(leader);
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(follower);
        match f.as_mut().poll(&mut cx) {
            Poll::Ready(None) => {}
            other => panic!("got {other:?}"),
        }
    }

    #[test]
    fn different_keys_dont_collide() {
        let sf = Arc::new(Singleflight::new(4));
        let a = sf.acquire(key(1));
        let b = sf.acquire(key(2));
        assert!(matches!(a, Acquire::Leader(_)));
        assert!(matches!(b, Acquire::Leader(_)));
        assert_eq!(sf.in_flight(), 2);
    }

    #[test]
    fn second_acquire_after_settle_is_leader_again() {
        let sf = Arc::new(Singleflight::new(4));
        let l1 = match sf.acquire(key(3)) {
            Acquire::Leader(g) => g,
            _ => unreachable!(),
        };
        l1.publish(entry(1));
        // After publish, the table entry is gone and a new
        // acquire becomes the leader.
        let l2 = sf.acquire(key(3));
        assert!(matches!(l2, Acquire::Leader(_)));
    }
}
