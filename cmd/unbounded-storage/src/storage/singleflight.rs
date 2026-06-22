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
        // Domain tag for the singleflight shard selector. Not a
        // secret; see PageKey::mix.
        const SHARD_DOMAIN: u32 = 0;
        let h = key.mix(SHARD_DOMAIN);
        &self.shards[(h as usize) % self.shards.len()]
    }

    pub fn acquire(self: &Arc<Self>, key: PageKey) -> Acquire {
        let shard = self.shard(&key);
        let mut map = shard.lock().unwrap();
        if let Some(existing) = map.get(&key).cloned() {
            return Acquire::Follower(LeaseWait { lease: existing });
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
    use crate::runtime::noop_waker;
    use crate::storage::types::{Checksum, Lba};
    use proptest::collection::vec;
    use proptest::prelude::*;
    use std::collections::HashMap;
    use std::future::Future;
    use std::pin::pin;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::Wake;

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

    #[derive(Clone, Debug, PartialEq, Eq)]
    enum ExpectedFill {
        Published(u64),
        Abandoned,
    }

    enum Role {
        Leader {
            key: PageKey,
            epoch: u64,
            guard: LeaseGuard,
        },
        Follower {
            key: PageKey,
            epoch: u64,
            wait: Pin<Box<LeaseWait>>,
            expected: Option<ExpectedFill>,
            polled: bool,
            wakes: Arc<AtomicUsize>,
        },
    }

    struct CountWaker {
        count: Arc<AtomicUsize>,
    }

    impl Wake for CountWaker {
        fn wake(self: Arc<Self>) {
            self.wake_by_ref();
        }

        fn wake_by_ref(self: &Arc<Self>) {
            self.count.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn assert_fill_matches(got: FillResult, expected: &ExpectedFill) {
        match (got, expected) {
            (Some(e), ExpectedFill::Published(lba)) => assert_eq!(e.lba.0, *lba),
            (None, ExpectedFill::Abandoned) => {}
            (got, expected) => panic!("fill result {got:?} did not match {expected:?}"),
        }
    }

    fn follower_wake_counts(
        roles: &[Option<Role>],
        key: PageKey,
        epoch: u64,
    ) -> Vec<(usize, usize)> {
        roles
            .iter()
            .enumerate()
            .filter_map(|(idx, role)| match role {
                Some(Role::Follower {
                    key: follower_key,
                    epoch: follower_epoch,
                    polled: true,
                    wakes,
                    ..
                }) if *follower_key == key && *follower_epoch == epoch => {
                    Some((idx, wakes.load(Ordering::SeqCst)))
                }
                _ => None,
            })
            .collect()
    }

    fn mark_followers_settled(
        roles: &mut [Option<Role>],
        key: PageKey,
        epoch: u64,
        expected: ExpectedFill,
        wake_counts: &[(usize, usize)],
    ) {
        for (idx, before) in wake_counts {
            let Some(Role::Follower { wakes, .. }) = roles[*idx].as_ref() else {
                continue;
            };
            assert!(
                wakes.load(Ordering::SeqCst) > *before,
                "settling leader for key {:?} did not wake polled follower {}",
                key,
                idx,
            );
        }

        for role in roles.iter_mut().flatten() {
            if let Role::Follower {
                key: follower_key,
                epoch: follower_epoch,
                expected: follower_expected,
                ..
            } = role
            {
                if *follower_key == key && *follower_epoch == epoch {
                    assert!(
                        follower_expected.replace(expected.clone()).is_none(),
                        "follower for key {:?} settled twice",
                        key,
                    );
                }
            }
        }
    }

    fn poll_follower(role: &mut Option<Role>) {
        let Some(Role::Follower {
            wait,
            expected,
            polled,
            wakes,
            ..
        }) = role
        else {
            return;
        };

        let waker: Waker = Arc::new(CountWaker {
            count: wakes.clone(),
        })
        .into();
        let mut cx = Context::from_waker(&waker);
        match wait.as_mut().poll(&mut cx) {
            Poll::Pending => {
                assert!(
                    expected.is_none(),
                    "settled follower returned Pending instead of Ready",
                );
                *polled = true;
            }
            Poll::Ready(got) => {
                let expected = expected
                    .as_ref()
                    .expect("follower resolved before its leader settled");
                assert_fill_matches(got, expected);
                *role = None;
            }
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

    proptest! {
        #![proptest_config(ProptestConfig {
            cases: 128,
            ..ProptestConfig::default()
        })]

        #[test]
        fn randomized_leader_follower_interleavings(
            ops in vec((0usize..8, 0u32..4, 0u8..5), 1..200),
        ) {
            let sf = Arc::new(Singleflight::new(4));
            let mut roles: Vec<Option<Role>> = (0..8).map(|_| None).collect();
            let mut active: HashMap<PageKey, u64> = HashMap::new();
            let mut next_epoch: HashMap<PageKey, u64> = HashMap::new();

            for (actor, key_idx, op) in ops {
                let page_key = key(key_idx);
                match op {
                    0 => {
                        if roles[actor].is_some() {
                            continue;
                        }
                        match sf.acquire(page_key) {
                            Acquire::Leader(guard) => {
                                let epoch = next_epoch.entry(page_key).or_insert(0);
                                let leader_epoch = *epoch;
                                *epoch += 1;
                                assert!(
                                    active.insert(page_key, leader_epoch).is_none(),
                                    "created a second leader for key {:?}",
                                    page_key,
                                );
                                roles[actor] = Some(Role::Leader {
                                    key: page_key,
                                    epoch: leader_epoch,
                                    guard,
                                });
                            }
                            Acquire::Follower(wait) => {
                                let follower_epoch = *active.get(&page_key).unwrap_or_else(|| {
                                    panic!(
                                        "created a follower for key {:?} without an active leader",
                                        page_key,
                                    )
                                });
                                roles[actor] = Some(Role::Follower {
                                    key: page_key,
                                    epoch: follower_epoch,
                                    wait: Box::pin(wait),
                                    expected: None,
                                    polled: false,
                                    wakes: Arc::new(AtomicUsize::new(0)),
                                });
                            }
                        }
                    }
                    1 => poll_follower(&mut roles[actor]),
                    2 | 3 | 4 => {
                        let Some(role) = roles[actor].take() else {
                            continue;
                        };
                        match role {
                            Role::Leader {
                                key: leader_key,
                                epoch,
                                guard,
                            } => {
                                let expected = if op == 2 {
                                    ExpectedFill::Published(actor as u64 + 1000)
                                } else {
                                    ExpectedFill::Abandoned
                                };
                                let wake_counts = follower_wake_counts(&roles, leader_key, epoch);
                                match expected {
                                    ExpectedFill::Published(lba) => guard.publish(entry(lba)),
                                    ExpectedFill::Abandoned => {
                                        if op == 3 {
                                            guard.abandon();
                                        } else {
                                            drop(guard);
                                        }
                                    }
                                }
                                active.remove(&leader_key);
                                mark_followers_settled(
                                    &mut roles,
                                    leader_key,
                                    epoch,
                                    expected,
                                    &wake_counts,
                                );
                            }
                            Role::Follower { .. } => {
                                // Dropping a follower before settlement must not affect the leader
                                // or any other follower. The remaining end-of-test checks catch leaks.
                            }
                        }
                    }
                    _ => unreachable!(),
                }

                prop_assert_eq!(sf.in_flight(), active.len());
            }

            for actor in 0..roles.len() {
                let Some(Role::Leader {
                    key: leader_key,
                    epoch,
                    guard,
                }) = roles[actor].take() else {
                    continue;
                };
                let wake_counts = follower_wake_counts(&roles, leader_key, epoch);
                drop(guard);
                active.remove(&leader_key);
                mark_followers_settled(
                    &mut roles,
                    leader_key,
                    epoch,
                    ExpectedFill::Abandoned,
                    &wake_counts,
                );
            }

            for role in &mut roles {
                poll_follower(role);
                prop_assert!(role.is_none(), "follower did not resolve after all leaders settled");
            }
            prop_assert!(active.is_empty());
            prop_assert_eq!(sf.in_flight(), 0);
        }
    }
}
