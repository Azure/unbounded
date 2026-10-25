//! Successor hints: an owner remembering which other owner fetched a page it
//! answered without a copy, so readers ask that owner instead of making it
//! fetch the page again (Known issue 21).
//!
//! An owner above rank 0 reaches origin only after its probe of rank 0 ended
//! without a copy: rank 0 missed a copy-only probe (the owner could not fund
//! a delegation) or rank 0's own acquisition failed. The owner then fetches
//! the page itself, an orphan copy rank 0 does not know about, and rank 0
//! fetches it again on its next cold miss. Rank 0 cannot ask the orphan's
//! holder itself: the requests that reach it carry one or two credits and a
//! few links, too few to route to another owner (the simulator measured
//! hint probes from rank 0 missing nine times in ten, mostly on route search).
//!
//! The reader that reaches rank 0 does have the budget: it visits every owner
//! anyway. So an owner that answers another owner's page probe without a copy
//! notes the requester ([`SuccessorHints::note`]; a copy-only miss is noted
//! without ranking, and [`successor_deferral`] checks ownership), and while the hint is fresh
//! it answers a non-owner's page `Acquire` with `Miss` instead of acquiring
//! ([`successor_deferral`]). The reader goes on to the next owner in its visit
//! order, which includes the holder. Owner requesters are never deferred: an
//! owner asks only its predecessors, and an owner below the holder keeps one
//! link per later predecessor, too few to reach it (deferring them made rank 2
//! fetch more pages itself in the simulator).
//!
//! Costs: a hint is noted only on a page probe this node already answered
//! without a copy, and needs no message of its own. Each hint defers at most
//! [`MAX_DEFERRALS`] requests and expires after [`HINT_TTL`], so a holder
//! that lost its copy costs each deferred reader one extra owner probe, and
//! at most [`MAX_DEFERRALS`] of them, before this owner fetches again. Origin
//! authority is unchanged: a deferral never fetches, and the reader never
//! gains authority (non-owners never reach origin).

use std::collections::{HashMap, VecDeque};
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Nodes remembered per page (the latest ones win).
pub const HINTS_PER_PAGE: usize = 2;

/// How long a hint stays usable. Owned pages stay resident, so the holder
/// usually still has the copy minutes later; the simulator's orphan refetches
/// came up to several minutes after the orphan fetch.
pub const HINT_TTL: Duration = Duration::from_secs(600);

/// Deferrals per hint before this owner fetches the page again.
pub const MAX_DEFERRALS: u32 = 8;

/// Default bound on remembered pages per node.
pub const HINT_CAPACITY: usize = 4096;

struct Entry<N> {
    nodes: Vec<N>,
    at: Instant,
    /// Deferrals made on this entry ([`SuccessorHints::charge`]).
    uses: u32,
}

/// Bounded, expiring map from a page to the owners that recently asked this
/// node for it and got no copy.
pub struct SuccessorHints<K, N> {
    capacity: usize,
    ttl: Duration,
    entries: HashMap<K, Entry<N>>,
    /// Insertion order for eviction; stale stamps are skipped.
    order: VecDeque<(K, Instant)>,
}

impl<K: Eq + Hash + Clone, N: Clone + PartialEq> SuccessorHints<K, N> {
    pub fn new(capacity: usize, ttl: Duration) -> Self {
        Self {
            capacity: capacity.max(1),
            ttl,
            entries: HashMap::new(),
            order: VecDeque::new(),
        }
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// `node` asked for `key` and got no copy from this node. A renewed hint
    /// starts a fresh deferral count.
    pub fn note(&mut self, key: K, node: N, now: Instant) {
        if let Some(entry) = self.entries.get_mut(&key) {
            entry.nodes.retain(|n| *n != node);
            entry.nodes.push(node);
            if entry.nodes.len() > HINTS_PER_PAGE {
                entry.nodes.remove(0);
            }
            entry.at = now;
            entry.uses = 0;
            self.order.push_back((key, now));
            self.compact();
            return;
        }
        while self.entries.len() >= self.capacity {
            let Some((old, at)) = self.order.pop_front() else {
                break;
            };
            if self.entries.get(&old).is_some_and(|e| e.at == at) {
                self.entries.remove(&old);
            }
        }
        self.entries.insert(
            key.clone(),
            Entry {
                nodes: vec![node],
                at: now,
                uses: 0,
            },
        );
        self.order.push_back((key, now));
        self.compact();
    }

    /// The unexpired hints for `key`, oldest first.
    pub fn peek(&self, key: &K, now: Instant) -> Vec<N> {
        self.entries
            .get(key)
            .filter(|e| now.saturating_duration_since(e.at) <= self.ttl)
            .map(|e| e.nodes.clone())
            .unwrap_or_default()
    }

    /// Count one deferral on `key`'s hints; they are dropped after `max`.
    pub fn charge(&mut self, key: &K, max: u32) {
        if let Some(entry) = self.entries.get_mut(key) {
            entry.uses += 1;
            if entry.uses >= max {
                self.entries.remove(key);
            }
        }
    }

    /// Drop the hints for `key`.
    pub fn forget(&mut self, key: &K) {
        self.entries.remove(key);
    }

    /// Bound the eviction queue: drop stamps that no longer match.
    fn compact(&mut self) {
        if self.order.len() <= 2 * self.capacity {
            return;
        }
        let entries = &self.entries;
        self.order
            .retain(|(k, at)| entries.get(k).is_some_and(|e| e.at == *at));
    }
}

/// Whether to note `requester` after answering its page probe without a
/// copy: both this node and the requester own the page.
pub fn should_note<N: PartialEq>(local: &N, requester: &N, owners: &[N]) -> bool {
    local != requester && owners.contains(local) && owners.contains(requester)
}

/// The hint `local`, holding no copy, defers a page `Acquire` to, if any:
/// the newest hinted node that is still one of the page's `owners`, when
/// `local` is an owner and the requester is not (see the module docs for why
/// owners are not deferred). The caller answers `Miss` and charges the hint.
///
/// Hints may be noted without ranking (a copy-only miss is answered before
/// anything ranks the page), so this is where ownership is checked.
pub fn successor_deferral<N: PartialEq>(
    hints: Vec<N>,
    local: &N,
    requester: &N,
    owners: &[N],
) -> Option<N> {
    if !owners.contains(local) || owners.contains(requester) {
        return None;
    }
    hints
        .into_iter()
        .rev()
        .find(|n| n != requester && owners.contains(n))
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

    #[test]
    fn peek_returns_noted_nodes_until_expiry() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, Duration::from_secs(1));
        assert!(h.peek(&1u64, now).is_empty());
        h.note(1u64, 7u32, now);
        assert_eq!(h.peek(&1, now), [7]);
        assert_eq!(h.peek(&1, now + Duration::from_secs(1)), [7]);
        assert!(h.peek(&1, now + Duration::from_secs(2)).is_empty());
    }

    #[test]
    fn keeps_latest_nodes_per_page_without_duplicates() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        h.note(1u64, 7u32, now);
        h.note(1, 8, now);
        h.note(1, 7, now);
        h.note(1, 9, now);
        assert_eq!(h.peek(&1, now), [7, 9]);
        assert_eq!(h.len(), 1);
    }

    #[test]
    fn charge_drops_hints_after_max_deferrals_and_note_resets() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        h.note(1u64, 7u32, now);
        h.charge(&1, 2);
        assert_eq!(h.peek(&1, now), [7]);
        h.note(1, 7, now);
        h.charge(&1, 2);
        assert_eq!(h.peek(&1, now), [7]);
        h.charge(&1, 2);
        assert!(h.peek(&1, now).is_empty());
        // Charging an absent key is a no-op.
        h.charge(&2, 1);
        assert!(h.is_empty());
    }

    #[test]
    fn capacity_evicts_the_oldest_page() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(2, HINT_TTL);
        h.note(1u64, 1u32, now);
        h.note(2, 2, now + Duration::from_millis(1));
        h.note(3, 3, now + Duration::from_millis(2));
        assert_eq!(h.len(), 2);
        assert!(h.peek(&1, now).is_empty());
        assert_eq!(h.peek(&3, now), [3]);
        h.forget(&3);
        assert_eq!(h.len(), 1);
    }

    #[test]
    fn renoting_keeps_the_queue_bounded() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(2, HINT_TTL);
        for i in 0..100u32 {
            h.note(1u64, i, now + Duration::from_millis(u64::from(i)));
        }
        assert!(h.order.len() <= 4);
        h.note(2, 0, now);
        h.note(3, 0, now);
        // Page 1 went in first, so inserting page 3 evicts it.
        assert_eq!(h.len(), 2);
        assert!(h.peek(&1, now).is_empty());
    }

    #[test]
    fn notes_only_between_owners() {
        let owners = [1u32, 2, 3];
        assert!(should_note(&1, &2, &owners));
        assert!(!should_note(&1, &1, &owners));
        assert!(!should_note(&1, &9, &owners));
        assert!(!should_note(&9, &1, &owners));
    }

    #[test]
    fn defers_only_non_owners_to_current_owners() {
        let owners = [1u32, 2, 3];
        // A reader goes to the newest hinted owner.
        assert_eq!(successor_deferral(vec![2, 3], &1, &9, &owners), Some(3));
        // A hint that is no longer an owner (or a reader) does not qualify.
        assert_eq!(successor_deferral(vec![2, 7], &1, &9, &owners), Some(2));
        assert_eq!(successor_deferral(vec![7], &1, &9, &owners), None);
        // Owners are never deferred.
        assert_eq!(successor_deferral(vec![2], &1, &3, &owners), None);
        assert_eq!(successor_deferral(vec![3], &1, &2, &owners), None);
        assert_eq!(successor_deferral(Vec::<u32>::new(), &1, &9, &owners), None);
        // A node that no longer owns the page never defers.
        assert_eq!(successor_deferral(vec![2], &8, &9, &owners), None);
    }
}
