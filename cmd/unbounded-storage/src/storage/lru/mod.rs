// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! SIEVE eviction policy.
//!
//! Per-disk cache pages are tracked in a single FIFO queue. Each
//! page's "referenced" bit lives on the shared
//! [`RefcountTable`](crate::storage::refcount::RefcountTable),
//! colocated with its pin count. On every access the engine
//! sets the bit; the SIEVE hand sweeps from the tail and:
//!
//! - if the page is pinned, skip it without touching the bit
//!   (it's in flight; future eviction passes will revisit);
//! - if the bit is set, clear it and rotate the page back to
//!   the head;
//! - otherwise evict the page.
//!
//! Compared to a textbook clock, SIEVE only rotates entries
//! that the hand visits, which keeps the queue's tail
//! biased toward genuinely cold pages.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use crate::storage::refcount::RefcountTable;
use crate::storage::types::{Lba, PageKey};

/// Metadata needed to evict one resident data run without a
/// separate reverse-LBA index.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Resident {
    pub lba: Lba,
    pub key: PageKey,
    pub byte_len: u32,
}

pub struct SieveLru {
    capacity: u64,
    queue: Mutex<VecDeque<Resident>>,
    refcount: Arc<RefcountTable>,
}

impl SieveLru {
    pub fn new(capacity: u64, refcount: Arc<RefcountTable>) -> Self {
        Self {
            capacity,
            queue: Mutex::new(VecDeque::new()),
            refcount,
        }
    }

    pub fn capacity(&self) -> u64 {
        self.capacity
    }

    pub fn len(&self) -> usize {
        self.queue.lock().unwrap().len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Returns true once the populated fraction crosses the
    /// configured high watermark. Callers (the eviction worker)
    /// use this to decide when to run a sweep.
    pub fn watermark_exceeded(&self, fraction: f64) -> bool {
        let len = self.len() as f64;
        let cap = self.capacity.max(1) as f64;
        len / cap >= fraction
    }

    pub fn admit(&self, resident: Resident) {
        self.queue.lock().unwrap().push_front(resident);
    }

    pub fn touch(&self, lba: Lba) {
        let _ = self.refcount.mark_referenced(lba.0);
    }

    /// Sweep the hand looking for up to `target` evictable
    /// pages. Pinned pages are skipped (left in place); pages
    /// whose referenced bit is set get demoted (bit cleared,
    /// rotated to the head); cold pages are returned as
    /// victims. The sweep gives up after seeing twice the
    /// queue's length to avoid an unbounded loop when every
    /// resident page is pinned.
    pub fn sweep(&self, target: usize) -> Vec<Resident> {
        let mut victims = Vec::with_capacity(target.min(64));
        let mut q = self.queue.lock().unwrap();
        let mut budget = q.len() * 2;
        while victims.len() < target && budget > 0 {
            budget -= 1;
            let Some(resident) = q.pop_back() else {
                break;
            };
            if self.refcount.is_pinned(resident.lba.0).unwrap_or(false) {
                q.push_front(resident);
                continue;
            }
            if self.refcount.is_referenced(resident.lba.0).unwrap_or(false) {
                let _ = self.refcount.clear_referenced(resident.lba.0);
                q.push_front(resident);
                continue;
            }
            victims.push(resident);
        }
        victims
    }

    /// Remove `lba` from the queue if present. Used when an
    /// external invalidation removes a page (e.g., the engine
    /// observed bitrot).
    pub fn forget(&self, lba: Lba) {
        let mut q = self.queue.lock().unwrap();
        if let Some(pos) = q.iter().position(|resident| resident.lba == lba) {
            q.remove(pos);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn resident(i: u64) -> Resident {
        Resident {
            lba: Lba(i),
            key: PageKey::new([i as u8; 32], i as u32),
            byte_len: 4096,
        }
    }

    fn lru(capacity: u64) -> SieveLru {
        let rc = Arc::new(RefcountTable::new(capacity));
        SieveLru::new(capacity, rc)
    }

    #[test]
    fn admit_then_sweep_returns_cold_pages_in_lru_order() {
        let l = lru(16);
        for i in 2..6 {
            l.admit(resident(i));
        }
        let v = l.sweep(2);
        assert_eq!(v, vec![resident(2), resident(3)]);
        assert_eq!(l.len(), 2);
    }

    #[test]
    fn touched_pages_survive_first_sweep() {
        let l = lru(16);
        for i in 2..6 {
            l.admit(resident(i));
        }
        l.touch(Lba(2));
        // Hand reaches Lba(2) first; bit set => clear+rotate.
        let v = l.sweep(1);
        assert_eq!(v, vec![resident(3)]);
        // Sweep again: now Lba(2) has bit cleared and gets evicted next.
        let v = l.sweep(1);
        assert_eq!(v, vec![resident(4)]);
    }

    #[test]
    fn pinned_pages_are_skipped() {
        let l = lru(16);
        let rc = l.refcount.clone();
        for i in 2..6 {
            l.admit(resident(i));
        }
        let _g = rc.pin(2).unwrap();
        let v = l.sweep(3);
        assert_eq!(v, vec![resident(3), resident(4), resident(5)]);
        // Lba(2) is still in the queue (skipped, re-enqueued).
        assert_eq!(l.len(), 1);
    }

    #[test]
    fn watermark() {
        let l = lru(10);
        assert!(!l.watermark_exceeded(0.9));
        for i in 2..11 {
            l.admit(resident(i));
        }
        assert!(l.watermark_exceeded(0.9));
    }

    #[test]
    fn forget_removes_lba() {
        let l = lru(8);
        l.admit(resident(2));
        l.admit(resident(3));
        l.forget(Lba(2));
        assert_eq!(l.len(), 1);
        assert_eq!(l.sweep(2), vec![resident(3)]);
    }
}
