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
//! Readers do not cover every refetch: owners ranked after this node also
//! delegate to it (their `Acquire` carries one origin credit and a few
//! links), and a reader's deferral budget runs out. So an owner's own walk
//! also asks the hinted owners copy-only before origin
//! ([`successor_targets`], [`crate::read::walk::WalkConfig::successors`]),
//! each probe funded by a fixed grant rather than the requester's budget
//! (the walk's credits never grow; the grant's leftovers are dropped). A hint
//! funds one such probe ([`SuccessorHints::probe`]), so the extra exchanges
//! are bounded by the hints noted (round 4, orphan2).
//!
//! Only owners ranked after this node are deferred to or probed: an owner
//! fetches after asking its predecessors, so a predecessor that asked this
//! node (rank 0's successor probe answered `Miss`) has no copy either, and
//! deferring to it bounced readers between two owners without one.
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

/// How old a hint must be before [`SuccessorHints::probe`] returns it. A
/// younger hint names an owner whose own fetch is most likely still running
/// (a concurrent cold miss), and a copy-only probe would only miss.
pub const PROBE_AFTER: Duration = Duration::from_secs(2);

/// Default bound on remembered pages per node.
pub const HINT_CAPACITY: usize = 4096;

struct Entry<N> {
    nodes: Vec<N>,
    at: Instant,
    /// Deferrals made on this entry ([`SuccessorHints::charge`]).
    uses: u32,
    /// Deferrals left before [`SuccessorHints::peek`] stops returning it.
    max: u32,
    /// Already returned by [`SuccessorHints::probe`].
    probed: bool,
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
            entry.max = u32::MAX;
            entry.probed = false;
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
                max: u32::MAX,
                probed: false,
            },
        );
        self.order.push_back((key, now));
        self.compact();
    }

    /// The unexpired hints for `key` still usable for deferral, oldest
    /// first.
    pub fn peek(&self, key: &K, now: Instant) -> Vec<N> {
        self.entries
            .get(key)
            .filter(|e| now.saturating_duration_since(e.at) <= self.ttl && e.uses < e.max)
            .map(|e| e.nodes.clone())
            .unwrap_or_default()
    }

    /// The unexpired hints for `key`, oldest first, for this node's own
    /// walk to ask before origin ([`successor_targets`]). Each noted hint is
    /// returned once, so one hint funds at most one granted probe per node
    /// it names; a renewed note returns it again.
    pub fn probe(&mut self, key: &K, now: Instant) -> Vec<N> {
        match self.entries.get_mut(key) {
            Some(e)
                if !e.probed
                    && now.saturating_duration_since(e.at) <= self.ttl
                    && now.saturating_duration_since(e.at) >= PROBE_AFTER =>
            {
                e.probed = true;
                e.nodes.clone()
            }
            _ => Vec::new(),
        }
    }

    /// Whether a hint for `key` names `node`, expired, charged or probed
    /// alike (simulator classification).
    pub fn names(&self, key: &K, node: &N) -> bool {
        self.entries
            .get(key)
            .is_some_and(|e| e.nodes.contains(node))
    }

    /// Count one deferral on `key`'s hints; [`Self::peek`] stops returning
    /// them after `max` (they stay for [`Self::probe`] until they expire or
    /// are evicted).
    pub fn charge(&mut self, key: &K, max: u32) {
        if let Some(entry) = self.entries.get_mut(key) {
            entry.uses += 1;
            entry.max = max;
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
/// Callers also check [`defer_allowed`] first.
///
/// Hints may be noted without ranking (a copy-only miss is answered before
/// anything ranks the page), so this is where ownership is checked.
pub fn successor_deferral<N: PartialEq>(
    hints: Vec<N>,
    local: &N,
    requester: &N,
    owners: &[N],
) -> Option<N> {
    let me = owners.iter().position(|o| o == local)?;
    if owners.contains(requester) {
        return None;
    }
    // Only an owner ranked after this node fetches after asking it: a
    // predecessor that asked (for example rank 0's successor probe,
    // [`successor_targets`]) was missing the page too, and deferring to it
    // would bounce readers between two owners without a copy.
    hints
        .into_iter()
        .rev()
        .find(|n| n != requester && owners.iter().position(|o| o == n).is_some_and(|r| r > me))
}

/// Whether a deferral may be considered at all. While this node's own
/// acquisition of the page runs, the requester joins it instead: a hint only
/// says that another owner asked for the page and got no copy, not that it
/// got one, and under origin overload the hinted owner's fetch often fails
/// or has not finished. Deferring then sent readers away from a fill they
/// could have joined (5k `zipf` at 16 MiB pages: 78% of deferrals, failed
/// reads 54 to 1,003; Known issue 21, wave 3 merge).
pub fn defer_allowed(acquiring: bool) -> bool {
    !acquiring
}

/// Hinted owners an owner walk at `rank` asks copy-only before origin
/// ([`crate::read::walk::WalkConfig::successors`]), newest first.
///
/// A hint names an owner that asked this node for the page and got no copy,
/// after which it usually fetched the page itself (an orphan copy). Asking it
/// before origin turns this owner's refetch into a peer copy (Known issue
/// 21). Only current owners ranked after this node qualify: predecessors are
/// asked anyway. Previous-membership targets already in `handoff` are not
/// repeated.
pub fn successor_targets<N: PartialEq + Clone>(
    hints: &[N],
    owners: &[N],
    rank: usize,
    handoff: &[N],
) -> Vec<N> {
    let mut out: Vec<N> = Vec::new();
    for n in hints.iter().rev() {
        let later = owners.iter().position(|o| o == n).is_some_and(|r| r > rank);
        if later && !handoff.contains(n) && !out.contains(n) {
            out.push(n.clone());
        }
    }
    out
}

/// The handoff targets of an owner walk with `successors` first, at most
/// [`crate::read::walk::MAX_HANDOFF`] in all, and how many of them are
/// successor targets ([`crate::read::walk::WalkConfig::successors`]).
/// Successors come first: they are funded by their own grant, while
/// previous-membership targets stop when the walk's credits run short.
pub fn with_successors<N: PartialEq + Clone>(successors: Vec<N>, handoff: &[N]) -> (Vec<N>, usize) {
    let mut out: Vec<N> = Vec::new();
    for n in successors {
        if out.len() < crate::read::walk::MAX_HANDOFF && !out.contains(&n) {
            out.push(n);
        }
    }
    let count = out.len();
    for n in handoff {
        if out.len() < crate::read::walk::MAX_HANDOFF && !out.contains(n) {
            out.push(n.clone());
        }
    }
    (out, count)
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
    fn charge_stops_deferrals_after_max_and_note_resets() {
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
        // The hint stays for this node's own successor probe.
        assert_eq!(h.len(), 1);
        assert_eq!(h.probe(&1, now + PROBE_AFTER), [7]);
        // Charging an absent key is a no-op.
        h.charge(&2, 1);
        assert_eq!(h.len(), 1);
    }

    #[test]
    fn probe_returns_each_note_once() {
        let now = Instant::now();
        let later = now + PROBE_AFTER;
        let mut h = SuccessorHints::new(8, PROBE_AFTER + Duration::from_secs(1));
        assert!(h.probe(&1u64, later).is_empty());
        h.note(1u64, 7u32, now);
        h.note(1, 8, now);
        // Too young: the noted owner is most likely still fetching.
        assert!(h.probe(&1, now).is_empty());
        assert_eq!(h.probe(&1, later), [7, 8]);
        assert!(h.probe(&1, later).is_empty());
        // Deferrals still see it.
        assert_eq!(h.peek(&1, later), [7, 8]);
        // A renewed note re-arms it.
        h.note(1, 8, now);
        assert_eq!(h.probe(&1, later), [7, 8]);
        // Expired hints are never probed.
        h.note(2, 9, now);
        assert!(h.probe(&2, later + Duration::from_secs(2)).is_empty());
        assert!(h.names(&2, &9));
        assert!(!h.names(&2, &7));
        assert!(!h.names(&3, &9));
    }

    #[test]
    fn successor_targets_are_later_owners_newest_first() {
        let owners = [1u32, 2, 3];
        // Rank 0 asks both later owners, newest hint first.
        assert_eq!(successor_targets(&[2, 3], &owners, 0, &[]), [3, 2]);
        // Rank 1 asks only rank 2; a predecessor or non-owner never.
        assert_eq!(successor_targets(&[1, 3, 9], &owners, 1, &[]), [3]);
        assert!(successor_targets(&[1, 2], &owners, 2, &[]).is_empty());
        // A previous-membership target already asked is not repeated.
        assert!(successor_targets(&[2], &owners, 0, &[2]).is_empty());
        assert!(successor_targets(&[], &owners, 0, &[]).is_empty());
    }

    #[test]
    fn successors_come_first_within_the_handoff_cap() {
        let (all, n) = with_successors(vec![3, 2], &[7, 8]);
        assert_eq!((all, n), (vec![3, 2, 7], 2));
        let (all, n) = with_successors(Vec::new(), &[7, 8]);
        assert_eq!((all, n), (vec![7, 8], 0));
        let (all, n) = with_successors(vec![1, 2, 3, 4], &[7]);
        assert_eq!((all, n), (vec![1, 2, 3], 3));
        let (all, n) = with_successors(vec![2], &[2, 7]);
        assert_eq!((all, n), (vec![2, 7], 1));
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

    #[test]
    fn defers_only_to_owners_ranked_after_this_node() {
        let owners = [1u32, 2, 3];
        // Rank 1 noted rank 0 (rank 0's successor probe missed): rank 0
        // has no copy either, so no deferral.
        assert_eq!(successor_deferral(vec![1], &2, &9, &owners), None);
        assert_eq!(successor_deferral(vec![1, 3], &2, &9, &owners), Some(3));
        assert_eq!(successor_deferral(vec![3, 1], &2, &9, &owners), Some(3));
        // Rank 2 has no later owner.
        assert_eq!(successor_deferral(vec![1, 2], &3, &9, &owners), None);
    }

    #[test]
    fn no_deferral_while_this_node_acquires() {
        assert!(defer_allowed(false));
        assert!(!defer_allowed(true));
    }
}
