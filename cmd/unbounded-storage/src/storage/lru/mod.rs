// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! SIEVE eviction policy.
//!
//! Per-disk cache pages are tracked in FIFO queues bucketed by cache
//! priority. Each page's "referenced" bit lives on the shared
//! [`RefcountTable`](crate::storage::refcount::RefcountTable),
//! colocated with its pin count. On every access the engine
//! sets the bit; the SIEVE hand sweeps from lower-priority buckets
//! first and from each bucket's tail:
//!
//! - if the page is pinned, skip it without touching the bit
//!   (it's in flight; future eviction passes will revisit);
//! - if the bit is set, clear it and rotate the page back to
//!   the head;
//! - otherwise evict the page.
//!
//! Compared to a textbook clock, SIEVE only rotates entries
//! that the hand visits, which keeps each bucket's tail
//! biased toward genuinely cold pages. Priority does not add an LBA
//! index on the write/allocation path; it is recorded only with the
//! resident LRU entry and consulted during eviction.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use crate::storage::refcount::RefcountTable;
use crate::storage::types::Lba;

pub struct SieveLru {
    capacity: u64,
    inner: Mutex<Inner>,
    refcount: Arc<RefcountTable>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct EvictionCandidate {
    pub lba: Lba,
    pub priority: i32,
}

#[derive(Default)]
struct Inner {
    buckets: BTreeMap<i32, VecDeque<Lba>>,
    len: usize,
}

impl SieveLru {
    pub fn new(capacity: u64, refcount: Arc<RefcountTable>) -> Self {
        Self {
            capacity,
            inner: Mutex::new(Inner::default()),
            refcount,
        }
    }

    pub fn capacity(&self) -> u64 {
        self.capacity
    }

    pub fn len(&self) -> usize {
        self.inner.lock().unwrap().len
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    #[cfg(test)]
    pub(crate) fn entries_for_test(&self) -> Vec<(Lba, i32)> {
        let inner = self.inner.lock().unwrap();
        inner
            .buckets
            .iter()
            .flat_map(|(&priority, bucket)| bucket.iter().map(move |&lba| (lba, priority)))
            .collect()
    }

    /// Returns true once the populated fraction crosses the
    /// configured high watermark. Callers (the eviction worker)
    /// use this to decide when to run a sweep.
    pub fn watermark_exceeded(&self, fraction: f64) -> bool {
        let len = self.len() as f64;
        let cap = self.capacity.max(1) as f64;
        len / cap >= fraction
    }

    pub fn admit(&self, lba: Lba, priority: i32) {
        let mut inner = self.inner.lock().unwrap();
        inner.buckets.entry(priority).or_default().push_front(lba);
        inner.len += 1;
    }

    pub fn touch(&self, lba: Lba) {
        let _ = self.refcount.mark_referenced(lba.0);
    }

    pub fn touch_with_priority(&self, lba: Lba, priority: i32) {
        self.reclassify(lba, priority);
        self.touch(lba);
    }

    /// Move `lba` to the current access priority if it is resident.
    pub fn reclassify(&self, lba: Lba, priority: i32) {
        let mut inner = self.inner.lock().unwrap();
        let Some((current, pos)) = inner.buckets.iter().find_map(|(&priority, q)| {
            q.iter()
                .position(|&candidate| candidate == lba)
                .map(|pos| (priority, pos))
        }) else {
            return;
        };
        if current == priority {
            return;
        }
        if let Some(q) = inner.buckets.get_mut(&current) {
            q.remove(pos);
            if inner.buckets.get(&current).is_some_and(VecDeque::is_empty) {
                inner.buckets.remove(&current);
            }
            inner.buckets.entry(priority).or_default().push_front(lba);
        }
    }

    /// Sweep the hand looking for up to `target` evictable
    /// pages. Lower-priority buckets are searched before higher-
    /// priority buckets. Pinned pages are skipped (left in place);
    /// pages whose referenced bit is set get demoted (bit cleared,
    /// rotated to the head); cold pages are returned as victims.
    /// The sweep gives up after seeing twice the resident length to
    /// avoid an unbounded loop when every resident page is pinned.
    pub fn sweep(&self, target: usize) -> Vec<EvictionCandidate> {
        let mut victims = Vec::with_capacity(target.min(64));
        let mut inner = self.inner.lock().unwrap();
        let mut budget = inner.len * 2;
        while victims.len() < target && budget > 0 && inner.len > 0 {
            let priorities: Vec<i32> = inner.buckets.keys().copied().collect();
            let mut made_progress = false;
            for priority in priorities {
                if victims.len() >= target || budget == 0 {
                    break;
                }
                let bucket_len = inner.buckets.get(&priority).map_or(0, VecDeque::len);
                for _ in 0..bucket_len {
                    if victims.len() >= target || budget == 0 {
                        break;
                    }
                    let Some(lba) = inner
                        .buckets
                        .get_mut(&priority)
                        .and_then(VecDeque::pop_back)
                    else {
                        break;
                    };
                    budget -= 1;
                    made_progress = true;
                    if self.refcount.is_pinned(lba.0).unwrap_or(false) {
                        inner.buckets.entry(priority).or_default().push_front(lba);
                    } else if self.refcount.is_referenced(lba.0).unwrap_or(false) {
                        let _ = self.refcount.clear_referenced(lba.0);
                        inner.buckets.entry(priority).or_default().push_front(lba);
                    } else {
                        inner.len -= 1;
                        victims.push(EvictionCandidate { lba, priority });
                    }
                    if inner.buckets.get(&priority).is_some_and(VecDeque::is_empty) {
                        inner.buckets.remove(&priority);
                    }
                }
            }
            if !made_progress {
                break;
            }
        }
        victims
    }

    /// Remove `lba` from the queue if present. Used when an
    /// external invalidation removes a page (e.g., the engine
    /// observed bitrot).
    pub fn forget(&self, lba: Lba) {
        let mut inner = self.inner.lock().unwrap();
        let priorities: Vec<i32> = inner.buckets.keys().copied().collect();
        for priority in priorities {
            let removed = if let Some(q) = inner.buckets.get_mut(&priority) {
                if let Some(pos) = q.iter().position(|&x| x == lba) {
                    q.remove(pos);
                    true
                } else {
                    false
                }
            } else {
                false
            };
            if removed {
                inner.len -= 1;
                if inner.buckets.get(&priority).is_some_and(VecDeque::is_empty) {
                    inner.buckets.remove(&priority);
                }
                break;
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn lru(capacity: u64) -> SieveLru {
        let rc = Arc::new(RefcountTable::new(capacity));
        SieveLru::new(capacity, rc)
    }

    #[test]
    fn admit_then_sweep_returns_cold_pages_in_lru_order() {
        let l = lru(16);
        for i in 2..6 {
            l.admit(Lba(i), 0);
        }
        let v = l.sweep(2);
        assert_eq!(lbas(&v), vec![Lba(2), Lba(3)]);
        assert_eq!(l.len(), 2);
    }

    #[test]
    fn touched_pages_survive_first_sweep() {
        let l = lru(16);
        for i in 2..6 {
            l.admit(Lba(i), 0);
        }
        l.touch(Lba(2));
        // Hand reaches Lba(2) first; bit set => clear+rotate.
        let v = l.sweep(1);
        assert_eq!(lbas(&v), vec![Lba(3)]);
        // Sweep again: now Lba(2) has bit cleared and gets evicted next.
        let v = l.sweep(1);
        assert_eq!(lbas(&v), vec![Lba(4)]);
    }

    #[test]
    fn pinned_pages_are_skipped() {
        let l = lru(16);
        let rc = l.refcount.clone();
        for i in 2..6 {
            l.admit(Lba(i), 0);
        }
        let _g = rc.pin(2).unwrap();
        let v = l.sweep(3);
        assert_eq!(lbas(&v), vec![Lba(3), Lba(4), Lba(5)]);
        // Lba(2) is still in the queue (skipped, re-enqueued).
        assert_eq!(l.len(), 1);
    }

    #[test]
    fn watermark() {
        let l = lru(10);
        assert!(!l.watermark_exceeded(0.9));
        for i in 2..11 {
            l.admit(Lba(i), 0);
        }
        assert!(l.watermark_exceeded(0.9));
    }

    #[test]
    fn forget_removes_lba() {
        let l = lru(8);
        l.admit(Lba(2), 0);
        l.admit(Lba(3), 0);
        l.forget(Lba(2));
        assert_eq!(l.len(), 1);
        assert_eq!(lbas(&l.sweep(2)), vec![Lba(3)]);
    }

    #[test]
    fn higher_priority_pages_are_evicted_last() {
        let l = lru(16);
        l.admit(Lba(2), 10);
        l.admit(Lba(3), -1);
        l.admit(Lba(4), 0);
        l.admit(Lba(5), -1);

        let v = l.sweep(3);
        assert_eq!(lbas(&v), vec![Lba(3), Lba(5), Lba(4)]);
        assert_eq!(
            v.iter().map(|c| c.priority).collect::<Vec<_>>(),
            vec![-1, -1, 0]
        );
        assert_eq!(l.len(), 1);
        assert_eq!(lbas(&l.sweep(1)), vec![Lba(2)]);
    }

    #[test]
    fn sweep_falls_through_when_lower_priority_pages_are_not_cold() {
        let l = lru(16);
        let rc = l.refcount.clone();
        l.admit(Lba(2), -10);
        l.admit(Lba(3), 5);
        let _g = rc.pin(2).unwrap();

        let v = l.sweep(1);
        assert_eq!(lbas(&v), vec![Lba(3)]);
        assert_eq!(l.len(), 1);
    }

    #[test]
    fn higher_priority_touch_reclassifies_resident_page() {
        let l = lru(16);
        l.admit(Lba(2), -10);
        l.admit(Lba(3), 0);

        l.touch_with_priority(Lba(2), 10);
        let v = l.sweep(2);

        assert_eq!(lbas(&v), vec![Lba(3), Lba(2)]);
        assert_eq!(
            v.iter()
                .map(|candidate| candidate.priority)
                .collect::<Vec<_>>(),
            vec![0, 10]
        );
    }

    #[test]
    fn lower_priority_touch_reclassifies_resident_page() {
        let l = lru(16);
        l.admit(Lba(2), 10);
        l.admit(Lba(3), 0);

        l.touch_with_priority(Lba(2), -10);
        let v = l.sweep(2);

        assert_eq!(lbas(&v), vec![Lba(3), Lba(2)]);
        assert_eq!(
            v.iter()
                .map(|candidate| candidate.priority)
                .collect::<Vec<_>>(),
            vec![0, -10]
        );
    }

    fn lbas(victims: &[EvictionCandidate]) -> Vec<Lba> {
        victims.iter().map(|candidate| candidate.lba).collect()
    }
}
