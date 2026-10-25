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
//! Every predecessor (Known issue 51). A holder at rank 2 also offers the
//! copy to rank 1: when rank 1 later cannot reach rank 0 it fetches the page
//! itself, and it asks only later owners it has a hint for (its successor
//! targets). Without the offer it had none whenever the holder's own walk
//! did not reach it, and fetched again although the holder was its own
//! successor (5k `full-failover`, seeds 1 to 3: 9 of 43 orphan repeats, all
//! minutes after the holder's fetch). So a fetch notes one notice per owner
//! ranked before the holder that did not answer its walk ([`notice_indexes`]),
//! keyed by the page and that rank, each scheduled and bounded like the rank
//! 0 notice.
//!
//! Bounds: one notice per origin fetch by an owner above rank 0 and owner
//! ranked before it (at most two); each sends
//! at most [`NOTICE_TRIES`] offers, each funded by its own fixed route grant
//! ([`NOTICE_LINKS`] links, no attempts), never by a reader's budget. At most
//! [`NOTICE_CAPACITY`] notices are pending per node (the oldest give way). An
//! offer cannot make the receiver fetch or send a body: it only notes a hint,
//! which an authenticated owner could already set with a copy-only probe. A
//! holder that no longer holds the page, or is no longer an owner above
//! rank 0, drops the notice instead of sending it ([`notice_target`]).
//!
//! Cut groups (Known issue 50). Offers exist because a partition or a
//! route failure cut the holder off from rank 0, and while the cut lasts
//! every offer across it fails in transport and counts as a link failure
//! (in the simulator, 5k `full-failover` seed 1: each of the 283 failed
//! offers out of 728 was a partition link failure). So each target belongs
//! to a group (its site): an offer that fails with cut evidence (a
//! transport failure, or an answer signed by a relay rather than the
//! target) blocks its group ([`OrphanNotices::cut`]) until that notice's
//! next offer is due. Offers that come due for a blocked group are deferred
//! to the end of the block without spending a try. When a block ends, one
//! offer goes out as the group's probe and the group stays held for
//! [`PROBE_HOLD`] or until that offer is reported; a delivered offer
//! ([`OrphanNotices::reached`]) releases the group for new offers (those
//! deferred behind the probe go when its hold ends), another cut blocks it
//! again. The node's other exchanges feed the same table
//! ([`OrphanNotices::observe`]): a read probe that fails in transport toward
//! a group holds its offers, and one answered by a member of the group
//! releases them, so offers wait out a cut the node's reads already see.
//! While a cut lasts, a holder sends at most one offer per
//! group per retry interval instead of one per notice. Tries are spent only
//! by offers actually sent, so the bounds above hold. Blocked groups are
//! bounded by the notice capacity. Members without a site share one group,
//! which then holds all of this node's offers during a cut.

use std::cmp::Reverse;
use std::collections::{BinaryHeap, HashMap, VecDeque};
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

/// How long a group stays held after its probe offer was sent, unless the
/// offer is reported sooner (longer than any offer's attempt timeout).
pub const PROBE_HOLD: Duration = Duration::from_secs(10);

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
pub struct OrphanNotices<K, N, G = ()> {
    capacity: usize,
    entries: HashMap<K, Entry<N>>,
    queues: [VecDeque<(K, Instant)>; NOTICE_TRIES],
    /// Notices deferred by a blocked group, by their new due time (an
    /// insertion counter keeps the order stable without `K: Ord`).
    deferred: BinaryHeap<Reverse<(Instant, u64)>>,
    deferred_keys: HashMap<u64, K>,
    seq: u64,
    /// Blocked groups and until when.
    blocked: HashMap<G, Instant>,
}

impl<K: Eq + Hash + Clone, N: Clone, G: Eq + Hash + Clone> OrphanNotices<K, N, G> {
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity: capacity.max(1),
            entries: HashMap::new(),
            queues: Default::default(),
            deferred: BinaryHeap::new(),
            deferred_keys: HashMap::new(),
            seq: 0,
            blocked: HashMap::new(),
        }
    }

    /// Whether some notice waits for a blocked group to open: callers that
    /// schedule a timer per noted or failed offer also need one for
    /// [`Self::next_due`] then.
    pub fn has_deferred(&mut self) -> bool {
        self.deferred_head().is_some()
    }

    /// Until when `group` is blocked at `now`, if it is.
    pub fn blocked(&self, group: &G, now: Instant) -> Option<Instant> {
        self.blocked
            .get(group)
            .copied()
            .filter(|until| *until > now)
    }

    /// Remove and return the earliest notice due at `now` whose target's
    /// group (`group_of`) is not blocked. Due notices of a blocked group are
    /// deferred to the block's end without spending a try. The returned
    /// offer holds its group for [`PROBE_HOLD`]: report it with
    /// [`Self::reached`] or [`Self::cut`] (or [`Self::failed`] for a failure
    /// that says nothing about the route).
    pub fn pop_open(&mut self, now: Instant, group_of: impl Fn(&N) -> G) -> Option<Due<K, N>> {
        loop {
            let due = self.pop_due(now)?;
            let group = group_of(&due.target);
            match self.blocked(&group, now) {
                Some(until) => self.defer(due, until),
                None => {
                    self.block(group, now + PROBE_HOLD, now);
                    return Some(due);
                }
            }
        }
    }

    /// An offer to a target in `group` was delivered: the route works,
    /// release the group.
    pub fn reached(&mut self, group: &G) {
        self.blocked.remove(group);
    }

    /// Another exchange of this node with a member of `group` ended: a
    /// route failure (`cut`: a transport failure or a relay's answer)
    /// holds the group's offers for [`PROBE_HOLD`] (never shortening a
    /// longer block); an answer signed by that member releases it. Only
    /// while notices are pending, so the table stays empty otherwise.
    pub fn observe(&mut self, group: G, cut: bool, now: Instant) {
        if self.entries.is_empty() {
            return;
        }
        if cut {
            let until = now + PROBE_HOLD;
            if self.blocked(&group, now).is_none_or(|u| u < until) {
                self.block(group, until, now);
            }
        } else {
            self.blocked.remove(&group);
        }
    }

    /// A popped offer was not sent (the notice was dropped): release the
    /// group's probe hold.
    pub fn release(&mut self, group: &G) {
        self.blocked.remove(group);
    }

    /// `due`'s offer to a target in `group` failed with cut evidence (a
    /// transport failure, or an answer a relay signed): schedule the next
    /// offer as [`Self::failed`] does and block the group until it is due
    /// (or for the next retry delay, when the notice is done).
    pub fn cut(&mut self, due: Due<K, N>, group: G, now: Instant) -> Option<Instant> {
        let delay = NOTICE_DELAYS[(due.tries + 1).min(NOTICE_TRIES - 1)];
        let next = self.failed(due, now);
        self.block(group, next.unwrap_or(now + delay), now);
        next
    }

    /// A failure that says nothing about the route (the target itself
    /// refused): release the group's probe hold and schedule the next offer
    /// as [`Self::failed`] does.
    pub fn refused(&mut self, due: Due<K, N>, group: &G, now: Instant) -> Option<Instant> {
        self.blocked.remove(group);
        self.failed(due, now)
    }

    fn block(&mut self, group: G, until: Instant, now: Instant) {
        if !self.blocked.contains_key(&group) && self.blocked.len() >= self.capacity {
            self.blocked.retain(|_, u| *u > now);
            if self.blocked.len() >= self.capacity {
                return;
            }
        }
        self.blocked.insert(group, until);
    }

    /// Hold `due` until `until` without spending a try.
    fn defer(&mut self, due: Due<K, N>, until: Instant) {
        if self.entries.contains_key(&due.key) {
            // Noted again meanwhile: that note wins.
            return;
        }
        while self.entries.len() >= self.capacity && self.evict() {}
        self.seq += 1;
        self.entries.insert(
            due.key.clone(),
            Entry {
                target: due.target,
                tries: due.tries,
                due: until,
            },
        );
        self.deferred.push(Reverse((until, self.seq)));
        self.deferred_keys.insert(self.seq, due.key);
        self.compact();
    }

    /// The live deferred head: (due, seq).
    fn deferred_head(&mut self) -> Option<(Instant, u64)> {
        while let Some(Reverse((at, seq))) = self.deferred.peek().copied() {
            let live = self
                .deferred_keys
                .get(&seq)
                .and_then(|k| self.entries.get(k))
                .is_some_and(|e| e.due == at);
            if live {
                return Some((at, seq));
            }
            self.deferred.pop();
            self.deferred_keys.remove(&seq);
        }
        None
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
        let deferred = self.deferred_head().map(|(at, _)| at);
        self.queues
            .iter()
            .filter_map(|q| q.front().map(|(_, at)| *at))
            .chain(deferred)
            .min()
    }

    /// Remove and return the earliest notice due at `now`.
    pub fn pop_due(&mut self, now: Instant) -> Option<Due<K, N>> {
        self.skip_stale();
        let queued = (0..NOTICE_TRIES)
            .filter_map(|l| self.queues[l].front().map(|(_, at)| (*at, l)))
            .filter(|(at, _)| *at <= now)
            .min_by_key(|(at, _)| *at);
        let deferred = self.deferred_head().filter(|(at, _)| *at <= now);
        let key = match (queued, deferred) {
            (Some((queued, level)), deferred) if deferred.is_none_or(|(at, _)| queued <= at) => {
                self.queues[level].pop_front().expect("front").0
            }
            (_, Some((_, seq))) => {
                self.deferred.pop();
                self.deferred_keys.remove(&seq).expect("live")
            }
            _ => return None,
        };
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

    /// Drop the notice closest to giving up (most tries, oldest), then a
    /// deferred one.
    fn evict(&mut self) -> bool {
        self.skip_stale();
        for level in (0..NOTICE_TRIES).rev() {
            if let Some((key, _)) = self.queues[level].pop_front() {
                self.entries.remove(&key);
                return true;
            }
        }
        if let Some((_, seq)) = self.deferred_head() {
            self.deferred.pop();
            let key = self.deferred_keys.remove(&seq).expect("live");
            self.entries.remove(&key);
            return true;
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
        let entries = &self.entries;
        if self.deferred.len() > 2 * self.capacity {
            let keys = &mut self.deferred_keys;
            self.deferred.retain(|Reverse((at, seq))| {
                let live = keys
                    .get(seq)
                    .and_then(|k| entries.get(k))
                    .is_some_and(|e| e.due == *at);
                if !live {
                    keys.remove(seq);
                }
                live
            });
        }
        let total: usize = self.queues.iter().map(VecDeque::len).sum();
        if total <= 2 * self.capacity {
            return;
        }
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

/// The owner ranks an origin fetch at `rank` notices (Known issue 51): rank
/// 0, and each later owner ranked before this one that did not answer this
/// owner's walk without a copy (`evidence[i]`, the walk's predecessor
/// evidence in rank order, is not `CopyMiss` or `VersionUnavailable`). An
/// owner that did answer noted this one as a successor hint then, so an
/// offer would only repeat that. Rank 0 is always noticed (its offer also
/// re-arms a hint its successor probe may have spent before this fetch
/// ended, Known issue 37). Empty unless [`should_notice`].
pub fn notice_indexes(
    rank: Option<usize>,
    evidence: &[crate::read::candidates::ProbeOutcome],
) -> Vec<u8> {
    use crate::read::candidates::ProbeOutcome;
    (0..rank.map_or(0, |r| r.min(2)))
        .filter(|i| {
            *i == 0
                || !matches!(
                    evidence.get(*i),
                    Some(ProbeOutcome::CopyMiss | ProbeOutcome::VersionUnavailable)
                )
        })
        .map(|i| i as u8)
        .collect()
}

/// Where a due notice for owner rank `index` goes under the current
/// membership: the page's owner at that rank, when `local` still holds the
/// page and is still an owner ranked after it. `None` drops the notice.
pub fn notice_target<N: PartialEq + Clone>(
    local: &N,
    owners: &[N],
    holds: bool,
    index: u8,
) -> Option<N> {
    let rank = owners.iter().position(|o| o == local)?;
    let index = usize::from(index);
    (holds && index < rank).then(|| owners[index].clone())
}

/// Whether a node receiving an offer notes the requester as a successor
/// hint: only when it has no copy and is not fetching one (then its own copy
/// serves readers, and the offer is redundant). Either way it answers `Miss`.
pub fn offer_noted(holds: bool, acquiring: bool) -> bool {
    !holds && !acquiring
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

    fn at(now: Instant, secs: u64) -> Instant {
        now + Duration::from_secs(secs)
    }

    #[test]
    fn first_offer_waits_the_first_delay() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(8);
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
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(8);
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
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(8);
        n.note(1u64, 0u32, now);
        let _delivered = n.pop_due(at(now, 5)).expect("due");
        assert_eq!(n.pop_due(at(now, 1000)), None);
    }

    #[test]
    fn earliest_due_comes_first_across_try_levels() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(8);
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
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(8);
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
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(3);
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
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(2);
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
        let mut n: OrphanNotices<u64, u32> = OrphanNotices::new(8);
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
        assert_eq!(notice_target(&7, &owners, true, 0), Some(5));
        assert_eq!(notice_target(&9, &owners, true, 0), Some(5));
        assert_eq!(notice_target(&7, &owners, false, 0), None);
        assert_eq!(notice_target(&5, &owners, true, 0), None);
        assert_eq!(notice_target(&8, &owners, true, 0), None);
        // Known issue 51: rank 2 also offers to rank 1; rank 1 does not
        // offer to itself or to a later owner.
        assert_eq!(notice_target(&9, &owners, true, 1), Some(7));
        assert_eq!(notice_target(&7, &owners, true, 1), None);
        assert_eq!(notice_target(&9, &owners, true, 2), None);
    }

    #[test]
    fn a_fetch_notices_every_owner_before_it_that_did_not_answer() {
        use crate::read::candidates::ProbeOutcome::*;
        assert!(notice_indexes(None, &[]).is_empty());
        assert!(notice_indexes(Some(0), &[]).is_empty());
        assert_eq!(notice_indexes(Some(1), &[CopyMiss]), [0]);
        assert_eq!(notice_indexes(Some(2), &[CopyMiss, Unreachable]), [0, 1]);
        assert_eq!(notice_indexes(Some(2), &[Unreachable, Overloaded]), [0, 1]);
        assert_eq!(notice_indexes(Some(2), &[Unreachable, CopyMiss]), [0]);
        assert_eq!(
            notice_indexes(Some(2), &[Unreachable, VersionUnavailable]),
            [0]
        );
        // Missing evidence counts as not answered.
        assert_eq!(notice_indexes(Some(2), &[Unreachable]), [0, 1]);
        assert_eq!(notice_indexes(Some(5), &[]), [0, 1]);
    }

    #[test]
    fn offers_are_noted_only_without_a_copy_or_a_fetch() {
        assert!(offer_noted(false, false));
        assert!(!offer_noted(true, false));
        assert!(!offer_noted(false, true));
        assert!(!offer_noted(true, true));
    }

    /// Known issue 50: a cut blocks the target's group until the failed
    /// notice's next offer; other notices of that group come due meanwhile
    /// and are deferred without spending a try; after the block one probe
    /// offer goes out, holding the group until it is reported.
    #[test]
    fn a_cut_holds_the_group_and_sends_one_probe_after_it() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(16);
        let site = |t: &u32| (*t / 10) as u8;
        n.note(1, 10, now);
        n.note(2, 11, now + Duration::from_secs(1));
        n.note(3, 12, now + Duration::from_secs(2));
        n.note(4, 20, now + Duration::from_secs(3));
        let first = n.pop_open(at(now, 5), site).expect("due");
        assert_eq!(first.key, 1);
        // Held while the probe is out: 2 is due but deferred, untouched.
        assert_eq!(n.pop_open(at(now, 6), site), None);
        // Cut at 7 s: next offer of 1 at 22 s; group 1 blocked until then.
        assert_eq!(n.cut(first, 1, at(now, 7)), Some(at(now, 22)));
        assert_eq!(n.blocked(&1, at(now, 7)), Some(at(now, 22)));
        // 3 comes due and is deferred; another group is unaffected.
        assert_eq!(n.pop_open(at(now, 8), site).map(|d| d.key), Some(4));
        assert_eq!(n.pop_open(at(now, 21), site), None);
        assert_eq!(n.len(), 3);
        assert_eq!(n.next_due(), Some(at(now, 22)));
        // At 22 s one probe (no try spent by the deferred ones).
        let probe = n.pop_open(at(now, 22), site).expect("probe");
        assert_eq!(n.pop_open(at(now, 22), site), None);
        // Delivered: the group opens; the deferred ones, held behind the
        // probe, go with their tries once the probe hold ends.
        n.reached(&1);
        assert_eq!(n.next_due(), Some(at(now, 22) + PROBE_HOLD));
        let later = at(now, 22) + PROBE_HOLD;
        let mut rest: Vec<(u64, usize)> = std::iter::from_fn(|| {
            n.pop_open(later, site).map(|d| {
                n.reached(&1);
                (d.key, d.tries)
            })
        })
        .collect();
        rest.sort();
        let mut all = vec![(probe.key, probe.tries)];
        all.extend(rest);
        all.sort();
        assert_eq!(all, vec![(1, 1), (2, 0), (3, 0)]);
        assert!(n.is_empty());
    }

    #[test]
    fn a_refusal_releases_the_group_and_spends_a_try() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(8);
        n.note(1, 10, now);
        n.note(2, 11, now);
        let due = n.pop_open(at(now, 5), |_| 1).expect("due");
        assert_eq!(n.refused(due, &1, at(now, 5)), Some(at(now, 20)));
        assert_eq!(n.blocked(&1, at(now, 5)), None);
        assert_eq!(n.pop_open(at(now, 5), |_| 1).map(|d| d.key), Some(2));
        // An unreported probe holds the group for `PROBE_HOLD` only.
        assert!(n.blocked(&1, at(now, 5)).is_some());
        assert_eq!(
            n.blocked(&1, now + Duration::from_secs(5) + PROBE_HOLD),
            None
        );
    }

    #[test]
    fn a_cut_on_the_last_try_still_blocks_and_drops_the_notice() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(8);
        n.note(1, 10, now);
        let mut t = at(now, 5);
        for _ in 0..NOTICE_TRIES - 1 {
            let due = n.pop_open(t, |_| 1).expect("due");
            t = n.cut(due, 1, t).expect("next");
        }
        let due = n.pop_open(t, |_| 1).expect("last");
        assert_eq!(n.cut(due, 1, t), None);
        assert!(n.is_empty());
        assert_eq!(n.blocked(&1, t), Some(t + NOTICE_DELAYS[NOTICE_TRIES - 1]));
    }

    #[test]
    fn deferred_notices_respect_capacity_renewal_and_forget() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(4);
        for k in 0..4u64 {
            n.note(k, 10, now);
        }
        let due = n.pop_open(at(now, 5), |_| 1).expect("due");
        n.cut(due, 1, at(now, 5));
        assert_eq!(n.pop_open(at(now, 5), |_| 1), None);
        assert_eq!(n.len(), 4);
        // A renewed note wins over the deferral; a forgotten one is gone.
        n.note(1, 10, at(now, 6));
        n.forget(&2);
        for k in 10..20u64 {
            n.note(k, 30, at(now, 6));
            assert!(n.len() <= 4);
        }
        for _ in 0..1000 {
            n.note(10, 30, at(now, 6));
        }
        assert!(n.deferred.len() <= 2 * 4 + 1);
        // Groups are bounded by the capacity too.
        for g in 0..100u8 {
            n.block(g, at(now, 100), at(now, 6));
        }
        assert!(n.blocked.len() <= 4);
    }

    /// Reads feed the cut groups: a transport failure toward a group holds
    /// its offers for `PROBE_HOLD` (never shortening a longer block), an
    /// answer signed by a member releases them; nothing is kept while no
    /// notice is pending.
    #[test]
    fn reads_hold_and_release_offers_to_a_group() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(8);
        n.observe(1, true, now);
        assert_eq!(n.blocked(&1, now), None);
        n.note(1, 10, now);
        n.observe(1, true, at(now, 4));
        assert_eq!(n.blocked(&1, at(now, 4)), Some(at(now, 4) + PROBE_HOLD));
        assert_eq!(n.pop_open(at(now, 5), |_| 1), None);
        assert_eq!(n.len(), 1);
        n.observe(1, false, at(now, 6));
        // Deferred to the hold's end, then sent with no try spent.
        let due = n.pop_open(at(now, 4) + PROBE_HOLD, |_| 1).expect("due");
        assert_eq!(due.tries, 0);
        // A read failure does not shorten a cut's longer block.
        let next = n.cut(due, 1, at(now, 15)).expect("next");
        n.observe(1, true, at(now, 16));
        assert_eq!(n.blocked(&1, at(now, 16)), Some(next));
    }
}
