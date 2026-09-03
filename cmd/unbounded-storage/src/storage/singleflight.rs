// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Per-key singleflight gate.
//!
//! Concurrent fills for the same [`PageKey`] collapse onto one in-flight
//! operation. The leader holds a [`LeaseGuard`]; followers return immediately
//! without performing duplicate I/O. Dropping the guard releases the key on
//! both success and failure paths.

use std::collections::HashSet;
use std::sync::Mutex;

use crate::storage::types::PageKey;

pub struct Singleflight {
    shards: Vec<Mutex<HashSet<PageKey>>>,
}

impl Singleflight {
    pub fn new(shards: usize) -> Self {
        let shards = shards.max(1);
        Self {
            shards: (0..shards).map(|_| Mutex::new(HashSet::new())).collect(),
        }
    }

    fn shard(&self, key: &PageKey) -> &Mutex<HashSet<PageKey>> {
        const SHARD_DOMAIN: u32 = 0;
        let h = key.mix(SHARD_DOMAIN);
        &self.shards[(h as usize) % self.shards.len()]
    }

    pub fn try_acquire(&self, key: PageKey) -> Option<LeaseGuard<'_>> {
        if !self.shard(&key).lock().unwrap().insert(key) {
            return None;
        }

        Some(LeaseGuard { owner: self, key })
    }

    fn release(&self, key: &PageKey) {
        let removed = self.shard(key).lock().unwrap().remove(key);
        debug_assert!(removed, "singleflight lease released more than once");
    }

    pub fn in_flight(&self) -> usize {
        self.shards.iter().map(|s| s.lock().unwrap().len()).sum()
    }
}

pub struct LeaseGuard<'a> {
    owner: &'a Singleflight,
    key: PageKey,
}

impl Drop for LeaseGuard<'_> {
    fn drop(&mut self) {
        self.owner.release(&self.key);
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use proptest::collection::vec;
    use proptest::prelude::*;

    use super::*;

    fn key(i: u32) -> PageKey {
        PageKey::new([0u8; 32], i)
    }

    #[test]
    fn follower_is_rejected_until_guard_drops() {
        let sf = Singleflight::new(4);
        let leader = sf.try_acquire(key(1)).expect("first caller is leader");

        assert!(sf.try_acquire(key(1)).is_none());
        assert_eq!(sf.in_flight(), 1);

        drop(leader);
        assert!(sf.try_acquire(key(1)).is_some());
    }

    #[test]
    fn different_keys_do_not_collide() {
        let sf = Singleflight::new(1);
        let first = sf.try_acquire(key(1)).expect("first key is available");
        let second = sf.try_acquire(key(2)).expect("second key is available");

        assert_eq!(sf.in_flight(), 2);
        drop((first, second));
        assert_eq!(sf.in_flight(), 0);
    }

    #[test]
    fn failure_scope_releases_key() {
        let sf = Singleflight::new(4);
        let result: Result<(), ()> = (|| {
            let _guard = sf.try_acquire(key(7)).expect("first caller is leader");
            Err(())
        })();

        assert!(result.is_err());
        assert!(sf.try_acquire(key(7)).is_some());
    }

    proptest! {
        #![proptest_config(ProptestConfig {
            cases: 128,
            ..ProptestConfig::default()
        })]

        #[test]
        fn randomized_acquire_release_interleavings(
            ops in vec((0usize..8, 0u32..4, any::<bool>()), 1..200),
        ) {
            let sf = Singleflight::new(4);
            let mut guards: Vec<Option<LeaseGuard<'_>>> =
                (0..8).map(|_| None).collect();
            let mut active: HashMap<PageKey, usize> = HashMap::new();

            for (actor, key_idx, acquire) in ops {
                if acquire {
                    if guards[actor].is_some() {
                        continue;
                    }

                    let page_key = key(key_idx);
                    match sf.try_acquire(page_key) {
                        Some(guard) => {
                            prop_assert!(active.insert(page_key, actor).is_none());
                            guards[actor] = Some(guard);
                        }
                        None => prop_assert!(active.contains_key(&page_key)),
                    }
                } else if let Some(guard) = guards[actor].take() {
                    active.remove(&guard.key);
                    drop(guard);
                }

                prop_assert_eq!(sf.in_flight(), active.len());
            }

            for guard in guards.into_iter().flatten() {
                active.remove(&guard.key);
                drop(guard);
            }
            prop_assert!(active.is_empty());
            prop_assert_eq!(sf.in_flight(), 0);
        }
    }
}
