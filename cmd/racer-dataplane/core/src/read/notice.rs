//! Orphan notices: an owner above rank 0 that fetched a page from origin
//! tells rank 0 later that it holds the page (Known issue 37).
//!
//! An owner above rank 0 reaches origin only after its probe of rank 0 ended
//! without a copy. When that probe reached rank 0, rank 0 noted the owner as a
//! successor hint ([`crate::read::successor`]) and asks it before its own next
//! origin fetch. When it did not (the route failed, a partition cut it, a
//! relay or rank 0 shed it, a relay answered `Unavailable`), rank 0 never
//! learned of the copy and fetches the page again on its next cold miss,
//! usually tens of seconds to minutes later.
//!
//! So the holder remembers the page ([`OrphanNotices::note`]) and, after
//! [`NOTICE_DELAYS`]`[0]`, sends rank 0 a copy offer: a page request in
//! offer mode that carries no origin credentials and no attempts, and is
//! answered `Miss` without a body. A receiver that neither holds the page nor
//! is acquiring it notes the requester as a successor hint, exactly as if it
//! had missed a copy-only probe ([`offer_noted`]); its next walk then copies
//! the page from the holder instead of fetching it. An offer that does not get
//! an answer signed by its destination (transport failure, a relay's answer,
//! shed) is tried again after the next delay, at most [`NOTICE_TRIES`] times
//! over about [`NOTICE_SPAN`] (longer than the simulator's 90 s partitions).
//!
//! Bounds: one notice per origin fetch by an owner above rank 0; each sends
//! at most [`NOTICE_TRIES`] offers, each funded by its own fixed route grant
//! ([`NOTICE_LINKS`] links, no attempts), never by a reader's budget. At most
//! [`NOTICE_CAPACITY`] notices are pending per node (the oldest give way). An
//! offer cannot make the receiver fetch or send a body: it only notes a hint,
//! which an authenticated owner could already set with a copy-only probe. A
//! holder that no longer holds the page, or is no longer an owner above
//! rank 0, drops the notice instead of sending it ([`notice_target`]).

use std::collections::{HashMap, VecDeque};
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Delay before each offer, measured from the origin fetch (first) or the
/// previous failed offer.
pub const NOTICE_DELAYS: [Duration; 4] = [
    Duration::from_secs(5),
    Duration::from_secs(15),
    Duration::from_secs(45),
    Duration::from_secs(90),
];

/// Offers per notice.
pub const NOTICE_TRIES: usize = NOTICE_DELAYS.len();

/// Time from the fetch to the last offer.
pub const NOTICE_SPAN: Duration = Duration::from_secs(5 + 15 + 45 + 90);

/// Default bound on pending notices per node.
pub const NOTICE_CAPACITY: usize = 1024;

/// Route links granted to one offer (as many as a successor probe,
/// [`crate::read::walk::SUCCESSOR_GRANT_LINKS`]). Offers carry no attempts.
pub const NOTICE_LINKS: u8 = crate::read::walk::SUCCESSOR_GRANT_LINKS;

/// A due offer: send it to `target`, then report the outcome with
/// [`OrphanNotices::delivered`] or [`OrphanNotices::failed`].
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Due<K, N> {
    pub key: K,
    pub target: N,
    /// Offers already sent for this notice.
    pub tries: usize,
}

struct Entry<N> {
    target: N,
    tries: usize,
    due: Instant,
}

/// Pending orphan notices by page, bounded and ordered by due time.
///
/// Every notice with the same number of tries waits the same delay, so each
/// try level is a FIFO queue in due order; [`Self::pop_due`] looks at the
/// queue heads only. Queue items whose entry changed are skipped.
pub struct OrphanNotices<K, N> {
    capacity: usize,
    entries: HashMap<K, Entry<N>>,
    queues: [VecDeque<(K, Instant)>; NOTICE_TRIES],
}

impl<K: Eq + Hash + Clone, N: Clone> OrphanNotices<K, N> {
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity: capacity.max(1),
            entries: HashMap::new(),
            queues: Default::default(),
        }
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// This node fetched `key` from origin as an owner above rank 0, and
    /// `target` is the page's rank 0. Returns when the first offer is due.
    /// A renewed note starts over.
    pub fn note(&mut self, key: K, target: N, now: Instant) -> Instant {
        if !self.entries.contains_key(&key) {
            while self.entries.len() >= self.capacity && self.evict() {}
        }
        self.schedule(key, target, 0, now)
    }

    /// The earliest due time, if any notice is pending.
    pub fn next_due(&mut self) -> Option<Instant> {
        self.skip_stale();
        self.queues
            .iter()
            .filter_map(|q| q.front().map(|(_, at)| *at))
            .min()
    }

    /// Remove and return the earliest notice due at `now`.
    pub fn pop_due(&mut self, now: Instant) -> Option<Due<K, N>> {
        self.skip_stale();
        let level = (0..NOTICE_TRIES)
            .filter_map(|l| self.queues[l].front().map(|(_, at)| (*at, l)))
            .filter(|(at, _)| *at <= now)
            .min_by_key(|(at, _)| *at)?
            .1;
        let (key, _) = self.queues[level].pop_front().expect("front");
        let entry = self.entries.remove(&key).expect("live");
        Some(Due {
            key,
            target: entry.target,
            tries: entry.tries,
        })
    }

    /// `due`'s offer got no answer signed by its target: schedule the next
    /// offer, unless it was the last. Returns when it is due.
    pub fn failed(&mut self, due: Due<K, N>, now: Instant) -> Option<Instant> {
        let tries = due.tries + 1;
        if tries >= NOTICE_TRIES || self.entries.contains_key(&due.key) {
            // Out of tries, or noted again meanwhile (that note wins).
            return None;
        }
        while self.entries.len() >= self.capacity && self.evict() {}
        Some(self.schedule(due.key, due.target, tries, now))
    }

    /// Drop the notice for `key` (the page left this node).
    pub fn forget(&mut self, key: &K) {
        self.entries.remove(key);
    }

    fn schedule(&mut self, key: K, target: N, tries: usize, now: Instant) -> Instant {
        let due = now + NOTICE_DELAYS[tries];
        self.entries
            .insert(key.clone(), Entry { target, tries, due });
        self.queues[tries].push_back((key, due));
        self.compact();
        due
    }

    /// Drop the notice closest to giving up (most tries, oldest).
    fn evict(&mut self) -> bool {
        self.skip_stale();
        for level in (0..NOTICE_TRIES).rev() {
            if let Some((key, _)) = self.queues[level].pop_front() {
                self.entries.remove(&key);
                return true;
            }
        }
        false
    }

    fn live(entries: &HashMap<K, Entry<N>>, level: usize, item: &(K, Instant)) -> bool {
        entries
            .get(&item.0)
            .is_some_and(|e| e.tries == level && e.due == item.1)
    }

    /// Drop queue heads whose entry changed or left.
    fn skip_stale(&mut self) {
        for level in 0..NOTICE_TRIES {
            while let Some(item) = self.queues[level].front() {
                if Self::live(&self.entries, level, item) {
                    break;
                }
                self.queues[level].pop_front();
            }
        }
    }

    /// Bound the queues: drop items that no longer match.
    fn compact(&mut self) {
        let total: usize = self.queues.iter().map(VecDeque::len).sum();
        if total <= 2 * self.capacity {
            return;
        }
        let entries = &self.entries;
        for (level, queue) in self.queues.iter_mut().enumerate() {
            queue.retain(|item| Self::live(entries, level, item));
        }
    }
}

/// Whether an origin fetch by this node leaves an orphan copy to notice:
/// the node is a current owner ranked after rank 0.
pub fn should_notice(rank: Option<usize>) -> bool {
    rank.is_some_and(|r| r > 0)
}

/// Where a due notice goes under the current membership: the page's rank 0,
/// when `local` still holds the page and is still an owner ranked after it.
/// `None` drops the notice.
pub fn notice_target<N: PartialEq + Clone>(local: &N, owners: &[N], holds: bool) -> Option<N> {
    let rank = owners.iter().position(|o| o == local)?;
    (holds && rank > 0).then(|| owners[0].clone())
}

/// Whether a node receiving an offer notes the requester as a successor
/// hint: only when it has no copy and is not fetching one (then its own copy
/// serves readers, and the offer is redundant). Either way it answers `Miss`.
pub fn offer_noted(holds: bool, acquiring: bool) -> bool {
    !holds && !acquiring
}

#[cfg(test)]
mod tests {
    use super::*;

    fn at(now: Instant, secs: u64) -> Instant {
        now + Duration::from_secs(secs)
    }

    #[test]
    fn first_offer_waits_the_first_delay() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(8);
        assert_eq!(n.note(1u64, 0u32, now), at(now, 5));
        assert_eq!(n.next_due(), Some(at(now, 5)));
        assert_eq!(n.pop_due(at(now, 4)), None);
        let due = n.pop_due(at(now, 5)).expect("due");
        assert_eq!(
            due,
            Due {
                key: 1,
                target: 0,
                tries: 0
            }
        );
        assert!(n.is_empty());
        assert_eq!(n.next_due(), None);
    }

    #[test]
    fn failed_offers_back_off_then_give_up() {
        let start = Instant::now();
        let mut n = OrphanNotices::new(8);
        let mut now = n.note(1u64, 0u32, start);
        let mut sent = 0;
        while let Some(due) = n.pop_due(now) {
            sent += 1;
            match n.failed(due, now) {
                Some(next) => now = next,
                None => break,
            }
        }
        assert_eq!(sent, NOTICE_TRIES);
        assert_eq!(now, start + NOTICE_SPAN);
        assert!(n.is_empty());
    }

    #[test]
    fn delivered_offer_is_not_retried() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(8);
        n.note(1u64, 0u32, now);
        let _delivered = n.pop_due(at(now, 5)).expect("due");
        assert_eq!(n.pop_due(at(now, 1000)), None);
    }

    #[test]
    fn earliest_due_comes_first_across_try_levels() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(8);
        n.note(1u64, 0u32, now);
        let due = n.pop_due(at(now, 5)).expect("due");
        // Retry due at 5 + 15 = 20 s; a new note at 10 s is due at 15 s.
        assert_eq!(n.failed(due, at(now, 5)), Some(at(now, 20)));
        n.note(2, 0, at(now, 10));
        assert_eq!(n.next_due(), Some(at(now, 15)));
        assert_eq!(n.pop_due(at(now, 30)).map(|d| d.key), Some(2));
        assert_eq!(
            n.pop_due(at(now, 30)).map(|d| (d.key, d.tries)),
            Some((1, 1))
        );
    }

    #[test]
    fn renewed_note_starts_over_and_wins_over_a_retry() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(8);
        n.note(1u64, 0u32, now);
        let due = n.pop_due(at(now, 5)).expect("due");
        n.note(1, 3, at(now, 6));
        assert_eq!(n.failed(due, at(now, 6)), None);
        assert_eq!(n.len(), 1);
        let due = n.pop_due(at(now, 11)).expect("due");
        assert_eq!((due.target, due.tries), (3, 0));
        // A renewed note before the offer replaces the pending one.
        n.note(2u64, 0, now);
        n.note(2, 0, at(now, 3));
        assert_eq!(n.pop_due(at(now, 5)), None);
        assert!(n.pop_due(at(now, 8)).is_some());
    }

    #[test]
    fn capacity_bounds_pending_notices() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(3);
        for k in 0..10u64 {
            n.note(k, 0u32, now + Duration::from_millis(k));
            assert!(n.len() <= 3);
        }
        let keys: Vec<u64> =
            std::iter::from_fn(|| n.pop_due(at(now, 100)).map(|d| d.key)).collect();
        assert_eq!(keys, vec![7, 8, 9]);
        // Many renewals keep the queues bounded.
        for i in 0..1000u64 {
            n.note(i % 3, 0, now + Duration::from_millis(i));
        }
        assert!(n.queues.iter().map(VecDeque::len).sum::<usize>() <= 2 * 3 + 1);
    }

    #[test]
    fn eviction_prefers_notices_closest_to_giving_up() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(2);
        n.note(1u64, 0u32, now);
        let due = n.pop_due(at(now, 5)).expect("due");
        n.note(2, 0, at(now, 5));
        n.failed(due, at(now, 5)).expect("retry");
        n.note(3, 0, at(now, 6));
        let keys: Vec<u64> =
            std::iter::from_fn(|| n.pop_due(at(now, 100)).map(|d| d.key)).collect();
        assert_eq!(keys, vec![2, 3]);
    }

    #[test]
    fn forgotten_notice_is_never_due() {
        let now = Instant::now();
        let mut n = OrphanNotices::new(8);
        n.note(1u64, 0u32, now);
        n.forget(&1);
        assert_eq!(n.next_due(), None);
        assert_eq!(n.pop_due(at(now, 100)), None);
    }

    #[test]
    fn only_owners_after_rank_zero_notice() {
        assert!(!should_notice(None));
        assert!(!should_notice(Some(0)));
        assert!(should_notice(Some(1)));
        assert!(should_notice(Some(2)));
    }

    #[test]
    fn target_is_current_rank_zero_while_held() {
        let owners = [5u32, 7, 9];
        assert_eq!(notice_target(&7, &owners, true), Some(5));
        assert_eq!(notice_target(&9, &owners, true), Some(5));
        assert_eq!(notice_target(&7, &owners, false), None);
        assert_eq!(notice_target(&5, &owners, true), None);
        assert_eq!(notice_target(&8, &owners, true), None);
    }

    #[test]
    fn offers_are_noted_only_without_a_copy_or_a_fetch() {
        assert!(offer_noted(false, false));
        assert!(!offer_noted(true, false));
        assert!(!offer_noted(false, true));
        assert!(!offer_noted(true, true));
    }
}
