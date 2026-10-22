//! Published ClusterCache priorities as relative fair-share weights.
//!
//! One table is shared by a worker's admission policy (memory flow control) and
//! its retention policy (disk, pending writes). Admission policies are cloned
//! into transferable handles, so the table uses a lock rather than `RefCell`.
//! Reads are short and happen once per scoring pass or admission decision.
use racer_control_wire::{CacheDefinition, CacheId, DEFAULT_PRIORITY};
use std::collections::HashMap;
use std::sync::{Arc, RwLock};

#[derive(Default, Debug)]
struct Table {
    weights: HashMap<CacheId, u64>,
    total: u64,
}

/// Cache id to weight. An empty table means no catalog was published yet; then
/// every cache is treated as the sole owner of capacity (priority-agnostic).
#[derive(Clone, Default, Debug)]
pub struct Weights(Arc<RwLock<Table>>);

impl Weights {
    /// Replace the table with the published catalog. Returns whether any
    /// weight was added, removed, or changed.
    pub fn update(&self, caches: &[CacheDefinition]) -> bool {
        let next: HashMap<CacheId, u64> = caches
            .iter()
            .map(|cache| (cache.id.clone(), u64::from(cache.priority)))
            .collect();
        let mut table = self.0.write().unwrap_or_else(|e| e.into_inner());
        if table.weights == next {
            return false;
        }
        table.total = next.values().sum();
        table.weights = next;
        true
    }

    /// Published cache ids, in no particular order.
    pub fn caches(&self) -> Vec<CacheId> {
        let table = self.0.read().unwrap_or_else(|e| e.into_inner());
        table.weights.keys().cloned().collect()
    }

    /// Weight of one cache. Unknown caches use `DEFAULT_PRIORITY`.
    pub fn weight(&self, cache: &CacheId) -> u64 {
        let table = self.0.read().unwrap_or_else(|e| e.into_inner());
        table
            .weights
            .get(cache)
            .copied()
            .unwrap_or(u64::from(DEFAULT_PRIORITY))
    }

    /// Fair share of `capacity` for each cache in `caches`, in input order.
    /// `share = capacity * w / W`; all-zero `W` splits equally. Caches absent
    /// from the published table count with `DEFAULT_PRIORITY`. With no
    /// published catalog every share is the full capacity.
    pub fn shares<'a>(
        &self,
        capacity: u64,
        caches: impl IntoIterator<Item = &'a CacheId>,
    ) -> Vec<u64> {
        let table = self.0.read().unwrap_or_else(|e| e.into_inner());
        let caches: Vec<_> = caches.into_iter().collect();
        if table.weights.is_empty() {
            return vec![capacity; caches.len()];
        }
        let unknown = caches
            .iter()
            .filter(|cache| !table.weights.contains_key(**cache))
            .count() as u64;
        let total = table.total + unknown * u64::from(DEFAULT_PRIORITY);
        let members = table.weights.len() as u64 + unknown;
        caches
            .iter()
            .map(|cache| {
                if total == 0 {
                    return capacity / members.max(1);
                }
                let weight = table
                    .weights
                    .get(*cache)
                    .copied()
                    .unwrap_or(u64::from(DEFAULT_PRIORITY));
                (u128::from(capacity) * u128::from(weight) / u128::from(total)) as u64
            })
            .collect()
    }

    /// Fair share of `capacity` for one cache. See `shares`.
    pub fn share(&self, capacity: u64, cache: &CacheId) -> u64 {
        self.shares(capacity, [cache])[0]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn cache(name: &str, priority: u16) -> CacheDefinition {
        CacheDefinition {
            id: CacheId(name.into()),
            name: name.into(),
            client_socket: "/client".into(),
            origin_socket: "/origin".into(),
            priority,
        }
    }

    #[test]
    fn shares_follow_weights_and_fall_back_to_equal_or_full() {
        let weights = Weights::default();
        let a = CacheId("a".into());
        let b = CacheId("b".into());
        let c = CacheId("c".into());
        assert_eq!(weights.shares(1000, [&a, &b]), vec![1000, 1000]);
        assert_eq!(weights.weight(&a), u64::from(DEFAULT_PRIORITY));
        assert!(weights.update(&[cache("a", 900), cache("b", 100)]));
        assert!(!weights.update(&[cache("a", 900), cache("b", 100)]));
        assert_eq!(weights.shares(1000, [&a, &b]), vec![900, 100]);
        assert_eq!(weights.weight(&b), 100);
        // Unknown caches count with the default priority.
        assert_eq!(weights.share(1500, &c), 500);
        assert!(weights.update(&[cache("a", 0), cache("b", 0)]));
        assert_eq!(weights.shares(1000, [&a, &b]), vec![500, 500]);
        assert!(weights.update(&[cache("a", 0), cache("b", 1000)]));
        assert_eq!(weights.shares(1000, [&a, &b]), vec![0, 1000]);
        assert!(weights.update(&[cache("a", 7)]));
        assert_eq!(weights.share(u64::MAX, &a), u64::MAX);
        assert!(weights.update(&[]));
        assert_eq!(weights.share(10, &a), 10);
    }
}
