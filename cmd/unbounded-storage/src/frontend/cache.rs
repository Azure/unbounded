// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-shard object-name to origin-metadata cache with TTL expiry.
//!
//! The S3 frontend resolves origin metadata (`HEAD origin/bucket/key`)
//! once per object-name and caches the resolved metadata (size, ETag,
//! ...) here behind a TTL so the HEAD is not repeated on every request
//! - the regional-cache role.
//!
//! Deliberately shard-local: `!Send + !Sync` is fine because each
//! shard owns its own instance, so no locking is needed. Time is
//! injected rather than read from a global clock, so TTL expiry is
//! deterministically testable: every mutating/reading call takes an
//! explicit monotonic `now` tick. The frontend feeds it a real
//! monotonic clock at runtime; tests feed it a controlled counter.

use std::collections::HashMap;

/// A monotonic time tick in arbitrary units (the frontend uses
/// milliseconds since an epoch; tests use a plain counter). Only
/// ordering and subtraction matter, so the unit is the caller's
/// choice as long as it is consistent for one cache instance.
pub type Tick = u64;

struct Entry<V> {
    value: V,
    /// Tick at which this entry stops being valid. A lookup at
    /// `now >= expires_at` treats the entry as absent.
    expires_at: Tick,
}

/// Shard-local TTL cache mapping an owned key (an object name) to a
/// value (the resolved origin metadata, or any metadata the frontend
/// caches). Generic over key and value so
/// the pure logic is testable in isolation from the origin types.
///
/// Expiry is lazy: an expired entry stays in the map until the next
/// access for that key, or until [`Self::purge_expired`] sweeps it.
/// That keeps the hot path (one `get` per request) branch-light.
pub struct TtlCache<K, V> {
    map: HashMap<K, Entry<V>>,
    ttl: Tick,
}

impl<K, V> TtlCache<K, V>
where
    K: std::hash::Hash + Eq,
{
    /// Build a cache whose entries live `ttl` ticks from insertion.
    pub fn new(ttl: Tick) -> Self {
        Self {
            map: HashMap::new(),
            ttl,
        }
    }

    /// Insert (or replace) `key -> value`, valid until `now + ttl`.
    /// Returns the previous value if one was present and not yet
    /// expired at `now`; an expired prior value is reported as absent.
    pub fn insert(&mut self, now: Tick, key: K, value: V) -> Option<V> {
        let expires_at = now.saturating_add(self.ttl);
        let prev = self.map.insert(key, Entry { value, expires_at });
        prev.filter(|e| now < e.expires_at).map(|e| e.value)
    }

    /// Fetch the value for `key` if it is present and not expired at
    /// `now`. Expired entries are evicted as a side effect so the map
    /// does not grow unboundedly under a churning key set.
    pub fn get(&mut self, now: Tick, key: &K) -> Option<&V> {
        // Evict first if stale, then borrow. Splitting the borrow this
        // way keeps the borrow checker happy without cloning the key.
        let stale = match self.map.get(key) {
            Some(e) => now >= e.expires_at,
            None => return None,
        };
        if stale {
            self.map.remove(key);
            return None;
        }
        self.map.get(key).map(|e| &e.value)
    }

    /// Whether `key` is present and live at `now`. Does not evict.
    pub fn contains(&self, now: Tick, key: &K) -> bool {
        self.map.get(key).is_some_and(|e| now < e.expires_at)
    }

    /// Drop `key` if present, returning its value when it was still
    /// live at `now`.
    pub fn remove(&mut self, now: Tick, key: &K) -> Option<V> {
        self.map
            .remove(key)
            .filter(|e| now < e.expires_at)
            .map(|e| e.value)
    }

    /// Sweep every expired entry. Returns how many were removed.
    /// Optional housekeeping the shard loop can call when idle.
    pub fn purge_expired(&mut self, now: Tick) -> usize {
        let before = self.map.len();
        self.map.retain(|_, e| now < e.expires_at);
        before - self.map.len()
    }

    /// Number of entries currently stored, *including* lazily-expired
    /// ones not yet swept. For live count, call after
    /// [`Self::purge_expired`].
    pub fn len(&self) -> usize {
        self.map.len()
    }

    pub fn is_empty(&self) -> bool {
        self.map.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn insert_then_get_within_ttl() {
        let mut c: TtlCache<String, u32> = TtlCache::new(10);
        assert_eq!(c.insert(0, "k".into(), 42), None);
        // Live at any tick strictly before expiry (0 + 10 = 10).
        assert_eq!(c.get(0, &"k".into()), Some(&42));
        assert_eq!(c.get(9, &"k".into()), Some(&42));
        assert!(c.contains(9, &"k".into()));
    }

    #[test]
    fn get_returns_none_when_expired() {
        let mut c: TtlCache<String, u32> = TtlCache::new(10);
        c.insert(0, "k".into(), 42);
        // At exactly now + ttl the entry is expired (half-open window).
        assert_eq!(c.get(10, &"k".into()), None);
        assert_eq!(c.get(100, &"k".into()), None);
        // The expired entry was evicted by the lookup.
        assert!(c.is_empty());
    }

    #[test]
    fn reinsert_refreshes_ttl_and_reports_live_prior() {
        let mut c: TtlCache<String, u32> = TtlCache::new(10);
        c.insert(0, "k".into(), 1);
        // Reinsert at tick 5 within the live window: prior value
        // reported, and the entry now expires at 5 + 10 = 15.
        assert_eq!(c.insert(5, "k".into(), 2), Some(1));
        assert_eq!(c.get(14, &"k".into()), Some(&2));
        assert_eq!(c.get(15, &"k".into()), None);
    }

    #[test]
    fn reinsert_over_expired_reports_no_prior() {
        let mut c: TtlCache<String, u32> = TtlCache::new(10);
        c.insert(0, "k".into(), 1);
        // Reinsert after the prior entry already expired: prior value
        // is treated as absent.
        assert_eq!(c.insert(20, "k".into(), 2), None);
        assert_eq!(c.get(25, &"k".into()), Some(&2));
    }

    #[test]
    fn remove_respects_liveness() {
        let mut c: TtlCache<String, u32> = TtlCache::new(10);
        c.insert(0, "k".into(), 1);
        // Removing a live entry returns its value.
        assert_eq!(c.remove(5, &"k".into()), Some(1));
        assert!(c.is_empty());

        // Removing an expired entry returns None (but still clears it).
        c.insert(0, "k2".into(), 9);
        assert_eq!(c.remove(50, &"k2".into()), None);
        assert!(c.is_empty());
    }

    #[test]
    fn purge_sweeps_only_expired() {
        let mut c: TtlCache<String, u32> = TtlCache::new(10);
        c.insert(0, "old".into(), 1);
        c.insert(8, "new".into(), 2);
        // At tick 12: "old" (expires 10) is stale, "new" (expires 18)
        // is live.
        assert_eq!(c.purge_expired(12), 1);
        assert_eq!(c.len(), 1);
        assert_eq!(c.get(12, &"new".into()), Some(&2));
    }

    #[test]
    fn zero_ttl_never_caches() {
        let mut c: TtlCache<String, u32> = TtlCache::new(0);
        c.insert(5, "k".into(), 1);
        // Expires at 5 + 0 = 5, so it is already stale at the same tick.
        assert_eq!(c.get(5, &"k".into()), None);
    }
}
