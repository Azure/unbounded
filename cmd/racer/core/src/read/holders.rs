//! Holder table: nodes that likely keep a durable copy of a page in their own
//! origin adapter.
//!
//! A cache with persistence Adapter keeps pages in Racer's memory only; the
//! durable copies live in the origin adapters (for example a containerd
//! content store) of the nodes that read the object. When an owner evicts such
//! a page, the nodes it served are the cheapest place to get it back. The
//! owner records each peer requester it serves ([`Holders::record`]), and a
//! later fill asks up to [`HOLDER_ATTEMPTS`] of them ([`Holders::candidates`])
//! for a local-only copy before its ranked walk and the remote origin. A holder
//! that answers without a copy, or fails, is dropped ([`Holders::forget`]).
//!
//! Keys identify a page of one exact version (the caller's page identity
//! includes the version and its etag), so a new version never routes to
//! holders of an old one: it simply has no entry until it is served.
//!
//! Bounds: [`HOLDERS_PER_PAGE`] nodes per page (newest first, deduplicated),
//! [`HOLDER_TTL`] per recorded node, and [`HOLDER_CAPACITY`] pages, evicting
//! the oldest inserted page first. Time is injected, never read from a clock.

use std::collections::{HashMap, VecDeque};
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Holders remembered per page. A newer record displaces the oldest.
pub const HOLDERS_PER_PAGE: usize = 4;

/// How long one recorded holder stays a candidate. Adapters keep content for
/// the life of the images that use it, which is usually much longer; a stale
/// holder only costs one quick miss.
pub const HOLDER_TTL: Duration = Duration::from_secs(24 * 60 * 60);

/// Default bound on pages with recorded holders per node.
pub const HOLDER_CAPACITY: usize = 65_536;

/// Holders one fill asks before it moves on to the ranked walk.
pub const HOLDER_ATTEMPTS: usize = 2;

/// Deadline of one holder probe. A holder answers from memory or a local
/// read of its adapter, so a slower answer is treated like a dead holder.
pub const HOLDER_TIMEOUT: Duration = Duration::from_secs(2);

struct Entry<N> {
    /// Newest first.
    nodes: Vec<(N, Instant)>,
    /// Insertion sequence of this entry, matched against the FIFO queue so
    /// a removed and re-inserted key is not evicted by its stale position.
    seq: u64,
}

/// Bounded, deterministic map from page to nodes that hold it.
pub struct Holders<K, N> {
    capacity: usize,
    ttl: Duration,
    entries: HashMap<K, Entry<N>>,
    order: VecDeque<(K, u64)>,
    seq: u64,
}

impl<K: Eq + Hash + Clone, N: Eq + Clone> Holders<K, N> {
    pub fn new(capacity: usize, ttl: Duration) -> Self {
        Self {
            capacity: capacity.max(1),
            ttl,
            entries: HashMap::new(),
            order: VecDeque::new(),
            seq: 0,
        }
    }

    /// Pages with at least one recorded holder (expired ones included until
    /// they are touched or evicted).
    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// Note that `node` holds `page` as of `now`. A node already recorded
    /// moves to the front with the new time; past [`HOLDERS_PER_PAGE`] the
    /// oldest record is dropped. A new page past the capacity evicts the
    /// oldest inserted page.
    pub fn record(&mut self, page: &K, node: N, now: Instant) {
        if let Some(entry) = self.entries.get_mut(page) {
            entry.nodes.retain(|(held, _)| held != &node);
            entry.nodes.insert(0, (node, now));
            entry.nodes.truncate(HOLDERS_PER_PAGE);
            return;
        }
        while self.entries.len() >= self.capacity {
            let Some((key, seq)) = self.order.pop_front() else {
                break;
            };
            if self.entries.get(&key).is_some_and(|entry| entry.seq == seq) {
                self.entries.remove(&key);
            }
        }
        self.seq += 1;
        self.entries.insert(
            page.clone(),
            Entry {
                nodes: vec![(node, now)],
                seq: self.seq,
            },
        );
        self.order.push_back((page.clone(), self.seq));
        // Removed keys leave stale queue positions; compact when they
        // dominate so the queue stays proportional to the table.
        if self.order.len() > self.capacity.saturating_mul(2) {
            let entries = &self.entries;
            self.order
                .retain(|(key, seq)| entries.get(key).is_some_and(|entry| entry.seq == *seq));
        }
    }

    /// Unexpired holders of `page` other than `local`, newest first, at most
    /// [`HOLDER_ATTEMPTS`]. Expired records are dropped.
    pub fn candidates(&mut self, page: &K, local: &N, now: Instant) -> Vec<N> {
        let ttl = self.ttl;
        let Some(entry) = self.entries.get_mut(page) else {
            return Vec::new();
        };
        entry
            .nodes
            .retain(|(_, at)| now.checked_duration_since(*at).is_none_or(|age| age < ttl));
        let found = entry
            .nodes
            .iter()
            .filter(|(node, _)| node != local)
            .take(HOLDER_ATTEMPTS)
            .map(|(node, _)| node.clone())
            .collect();
        if entry.nodes.is_empty() {
            self.entries.remove(page);
        }
        found
    }

    /// Drop `node` as a holder of `page`, after it answered without a copy or
    /// failed.
    pub fn forget(&mut self, page: &K, node: &N) {
        if let Some(entry) = self.entries.get_mut(page) {
            entry.nodes.retain(|(held, _)| held != node);
            if entry.nodes.is_empty() {
                self.entries.remove(page);
            }
        }
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

    type Key = (&'static str, &'static str, u64);

    fn table(capacity: usize) -> (Holders<Key, u32>, Instant) {
        (Holders::new(capacity, HOLDER_TTL), Instant::now())
    }

    const PAGE: Key = ("cache/key", "etag-1", 0);

    #[test]
    fn records_newest_first_and_caps_per_page() {
        let (mut h, now) = table(8);
        for node in 1..=6 {
            h.record(&PAGE, node, now);
        }
        assert_eq!(h.candidates(&PAGE, &0, now), vec![6, 5]);
        // Only the newest HOLDERS_PER_PAGE survive: forgetting the top two
        // exposes 4 and 3, and then nothing.
        h.forget(&PAGE, &6);
        h.forget(&PAGE, &5);
        assert_eq!(h.candidates(&PAGE, &0, now), vec![4, 3]);
        h.forget(&PAGE, &4);
        h.forget(&PAGE, &3);
        assert!(h.candidates(&PAGE, &0, now).is_empty());
        assert!(h.is_empty());
    }

    #[test]
    fn deduplicates_and_refreshes() {
        let (mut h, now) = table(8);
        h.record(&PAGE, 1, now);
        h.record(&PAGE, 2, now);
        h.record(&PAGE, 1, now + Duration::from_secs(1));
        assert_eq!(h.candidates(&PAGE, &0, now), vec![1, 2]);
        h.forget(&PAGE, &1);
        assert_eq!(h.candidates(&PAGE, &0, now), vec![2]);
    }

    #[test]
    fn excludes_the_local_node() {
        let (mut h, now) = table(8);
        h.record(&PAGE, 1, now);
        h.record(&PAGE, 7, now);
        h.record(&PAGE, 2, now);
        assert_eq!(h.candidates(&PAGE, &7, now), vec![2, 1]);
    }

    #[test]
    fn expires_after_the_ttl() {
        let (mut h, now) = table(8);
        h.record(&PAGE, 1, now);
        h.record(&PAGE, 2, now + Duration::from_secs(10));
        let late = now + HOLDER_TTL;
        assert_eq!(h.candidates(&PAGE, &0, late), vec![2]);
        assert!(
            h.candidates(&PAGE, &0, late + Duration::from_secs(10))
                .is_empty()
        );
        assert!(h.is_empty());
        // A record in the injected future is not expired.
        h.record(&PAGE, 3, late);
        assert_eq!(h.candidates(&PAGE, &0, now), vec![3]);
    }

    #[test]
    fn capacity_evicts_the_oldest_page() {
        let (mut h, now) = table(2);
        let a = ("c/k", "e", 0);
        let b = ("c/k", "e", 1);
        let c = ("c/k", "e", 2);
        h.record(&a, 1, now);
        h.record(&b, 1, now);
        // Recording on an existing page does not refresh its position.
        h.record(&a, 2, now);
        h.record(&c, 1, now);
        assert_eq!(h.len(), 2);
        assert!(h.candidates(&a, &0, now).is_empty());
        assert_eq!(h.candidates(&b, &0, now), vec![1]);
        assert_eq!(h.candidates(&c, &0, now), vec![1]);
    }

    #[test]
    fn reinserted_pages_are_not_evicted_by_stale_positions() {
        let (mut h, now) = table(2);
        let a = ("c/k", "e", 0);
        let b = ("c/k", "e", 1);
        let c = ("c/k", "e", 2);
        h.record(&a, 1, now);
        h.forget(&a, &1);
        h.record(&b, 1, now);
        h.record(&a, 1, now);
        // b is now the oldest live entry, not a.
        h.record(&c, 1, now);
        assert!(h.candidates(&b, &0, now).is_empty());
        assert_eq!(h.candidates(&a, &0, now), vec![1]);
        assert_eq!(h.candidates(&c, &0, now), vec![1]);
    }

    #[test]
    fn queue_stays_bounded_under_churn() {
        let (mut h, now) = table(4);
        for round in 0..1000u64 {
            let page = ("c/k", "e", round % 3);
            h.record(&page, 1, now);
            h.forget(&page, &1);
        }
        assert!(h.order.len() <= 8 + 1);
        assert!(h.is_empty());
    }

    #[test]
    fn versions_are_isolated() {
        let (mut h, now) = table(8);
        let old = ("cache/key", "etag-1", 0);
        let new = ("cache/key", "etag-2", 0);
        h.record(&old, 1, now);
        assert!(h.candidates(&new, &0, now).is_empty());
        h.record(&new, 2, now);
        h.forget(&new, &1);
        assert_eq!(h.candidates(&old, &0, now), vec![1]);
        assert_eq!(h.candidates(&new, &0, now), vec![2]);
    }

    #[test]
    fn forgetting_unknown_entries_is_a_no_op() {
        let (mut h, now) = table(8);
        h.forget(&PAGE, &1);
        h.record(&PAGE, 1, now);
        h.forget(&PAGE, &9);
        assert_eq!(h.candidates(&PAGE, &0, now), vec![1]);
    }
}
