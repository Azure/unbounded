//! Orphan notices: an owner above rank 0 that fetched a page from origin
//! tells rank 0 later that it holds the page (Known issue 37).
//!
//! An owner above rank 0 reaches origin only after its probe of rank 0 ended
//! without a copy. When that probe reached rank 0, rank 0 noted the owner as a
//! successor hint ([`crate::read::hints`]) and asks it before its own next
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
//! had missed a copy-only probe ([`SuccessorHints::note_offer`]); its next walk then copies
//! the page from the holder instead of fetching it. An offer that does not get
//! an answer signed by its destination (transport failure, a relay's answer,
//! shed) is tried again after the next delay, at most [`NOTICE_TRIES`] times
//! over about 155 s (longer than the simulator's 90 s partitions).
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

struct NoticeEntry<N> {
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
    entries: HashMap<K, NoticeEntry<N>>,
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

    /// Whether a notice for `key` is pending (not yet due, deferred, or
    /// waiting for its next try).
    pub fn pending(&self, key: &K) -> bool {
        self.entries.contains_key(key)
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
    /// deferred to the block's end without spending a try. An offer to a
    /// group whose block just ended holds the group for [`PROBE_HOLD`]
    /// (one probe per cut); report every offer with
    /// [`Self::reached`] or [`Self::cut`] (or [`Self::failed`] for a failure
    /// that says nothing about the route).
    pub fn pop_open(&mut self, now: Instant, group_of: impl Fn(&N) -> G) -> Option<Due<K, N>> {
        loop {
            let due = self.pop_due(now)?;
            let group = group_of(&due.target);
            match self.blocked(&group, now) {
                Some(until) => self.defer(due, until),
                None => {
                    // After a block ends, this offer probes the group:
                    // hold the rest until it is reported. A group never
                    // blocked (or released since) is not held.
                    if self.blocked.contains_key(&group) {
                        self.block(group, now + PROBE_HOLD, now);
                    }
                    return Some(due);
                }
            }
        }
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

    /// A popped offer was not sent (the notice was dropped), or was
    /// delivered (the route works): release the group's probe hold.
    pub fn release(&mut self, group: &G) {
        self.blocked.remove(group);
    }

    /// `due`'s offer to a target in `group` failed with cut evidence (a
    /// transport failure, or an answer a relay signed): schedule the next
    /// offer as [`Self::failed`] does and block the group until it is due
    /// (or for the next retry delay, when the notice is done).
    fn cut(&mut self, due: Due<K, N>, group: G, now: Instant) -> Option<Instant> {
        let delay = NOTICE_DELAYS[(due.tries + 1).min(NOTICE_TRIES - 1)];
        let next = self.failed(due, now);
        self.block(group, next.unwrap_or(now + delay), now);
        next
    }

    /// A failure that says nothing about the route (the target itself
    /// refused): release the group's probe hold and schedule the next offer
    /// as [`Self::failed`] does.
    fn refused(&mut self, due: Due<K, N>, group: &G, now: Instant) -> Option<Instant> {
        self.blocked.remove(group);
        self.failed(due, now)
    }

    /// `due`'s offer to a target in `group` ended with `end`. Returns when
    /// the next offer for it is due (`None`: delivered, dropped, or out of
    /// tries).
    pub fn offer_result(
        &mut self,
        due: Due<K, N>,
        end: OfferEnd,
        group: G,
        now: Instant,
    ) -> Option<Instant> {
        match end {
            OfferEnd::Dropped | OfferEnd::Delivered => {
                self.release(&group);
                None
            }
            OfferEnd::Refused => self.refused(due, &group, now),
            OfferEnd::Cut => self.cut(due, group, now),
        }
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
            NoticeEntry {
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
            .insert(key.clone(), NoticeEntry { target, tries, due });
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

    fn live(entries: &HashMap<K, NoticeEntry<N>>, level: usize, item: &(K, Instant)) -> bool {
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

/// How a copy offer ended (Known issue 50).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum OfferEnd {
    /// Not sent: this node is no longer an owner after rank 0.
    Dropped,
    /// The target answered `Miss`.
    Delivered,
    /// The target itself answered something else (shed, behind).
    Refused,
    /// A transport failure or timeout, or a relay's answer: the route is cut.
    Cut,
}

impl OfferEnd {
    /// Classify an offer's answer: `Some(miss)` when the target itself
    /// signed it (`miss`: it answered `Miss`), `None` for a transport
    /// failure, a timeout, or a relay's answer.
    pub fn of(answer: Option<bool>) -> Self {
        match answer {
            Some(true) => Self::Delivered,
            Some(false) => Self::Refused,
            None => Self::Cut,
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
pub fn notice_indexes(rank: Option<usize>, evidence: &[crate::read::ProbeOutcome]) -> Vec<u8> {
    use crate::read::ProbeOutcome;
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

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod notice_tests {
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
        assert_eq!(now, start + Duration::from_secs(5 + 15 + 45 + 90));
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
        use crate::read::ProbeOutcome::*;
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
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        assert!(!h.note_offer(1u64, 7u32, true, false, now));
        assert!(!h.note_offer(1, 7, false, true, now));
        assert!(!h.note_offer(1, 7, true, true, now));
        assert!(h.is_empty());
        assert!(h.note_offer(1, 7, false, false, now));
        assert_eq!(h.len(), 1);
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
        n.note(2, 11, now + Duration::from_secs(3));
        n.note(3, 12, now + Duration::from_secs(4));
        n.note(4, 20, now + Duration::from_secs(5));
        let first = n.pop_open(at(now, 5), site).expect("due");
        assert_eq!(first.key, 1);
        // No cut seen yet: offers to the group are not held.
        assert_eq!(n.blocked(&1, at(now, 5)), None);
        // Cut at 7 s: next offer of 1 at 22 s; group 1 blocked until then.
        assert_eq!(n.cut(first, 1, at(now, 7)), Some(at(now, 22)));
        assert_eq!(n.blocked(&1, at(now, 7)), Some(at(now, 22)));
        // 2 and 3 come due and are deferred; another group is unaffected.
        assert_eq!(n.pop_open(at(now, 10), site).map(|d| d.key), Some(4));
        assert_eq!(n.pop_open(at(now, 21), site), None);
        assert_eq!(n.len(), 3);
        assert_eq!(n.next_due(), Some(at(now, 22)));
        // At 22 s one probe (no try spent by the deferred ones).
        let probe = n.pop_open(at(now, 22), site).expect("probe");
        assert_eq!(n.pop_open(at(now, 22), site), None);
        // Delivered: the group opens; the deferred ones, held behind the
        // probe, go with their tries once the probe hold ends.
        n.release(&1);
        assert_eq!(n.next_due(), Some(at(now, 22) + PROBE_HOLD));
        let later = at(now, 22) + PROBE_HOLD;
        let mut rest: Vec<(u64, usize)> = std::iter::from_fn(|| {
            n.pop_open(later, site).map(|d| {
                n.release(&1);
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
    fn offer_results() {
        assert_eq!(OfferEnd::of(Some(true)), OfferEnd::Delivered);
        assert_eq!(OfferEnd::of(Some(false)), OfferEnd::Refused);
        assert_eq!(OfferEnd::of(None), OfferEnd::Cut);
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(8);
        n.note(1, 10, now);
        let due = n.pop_open(at(now, 5), |_| 1).expect("due");
        // A cut blocks the group until the next offer.
        let next = n.offer_result(due, OfferEnd::Cut, 1, at(now, 5));
        assert!(next.is_some());
        assert_eq!(n.blocked(&1, at(now, 5)), next);
        let due = n.pop_open(next.expect("next"), |_| 1).expect("due");
        // Delivered and dropped offers release the group, nothing is due.
        assert_eq!(
            n.offer_result(due, OfferEnd::Delivered, 1, at(now, 30)),
            None
        );
        assert_eq!(n.blocked(&1, at(now, 30)), None);
        n.note(2, 10, at(now, 30));
        let due = n.pop_open(at(now, 35), |_| 1).expect("due");
        assert_eq!(n.offer_result(due, OfferEnd::Dropped, 1, at(now, 35)), None);
        n.note(3, 10, at(now, 40));
        let due = n.pop_open(at(now, 45), |_| 1).expect("due");
        // A refusal spends a try without blocking the group.
        assert!(
            n.offer_result(due, OfferEnd::Refused, 1, at(now, 45))
                .is_some()
        );
        assert_eq!(n.blocked(&1, at(now, 45)), None);
    }

    #[test]
    fn a_refusal_releases_the_group_and_spends_a_try() {
        let now = Instant::now();
        let mut n: OrphanNotices<u64, u32, u8> = OrphanNotices::new(8);
        n.note(1, 10, now);
        n.note(2, 11, now);
        let due = n.pop_open(at(now, 5), |_| 1).expect("due");
        n.cut(due, 1, at(now, 5));
        // After the block, one probe; a refusal releases the group.
        let due = n.pop_open(at(now, 20), |_| 1).expect("probe");
        assert!(n.blocked(&1, at(now, 20)).is_some());
        let other = 3 - due.key;
        let tries = due.tries;
        assert_eq!(
            n.refused(due, &1, at(now, 20)),
            Some(at(now, 20) + NOTICE_DELAYS[tries + 1])
        );
        assert_eq!(n.blocked(&1, at(now, 20)), None);
        assert_eq!(n.pop_open(at(now, 20), |_| 1).map(|d| d.key), Some(other));
        assert_eq!(n.blocked(&1, at(now, 20)), None);
        // An unreported probe holds the group for `PROBE_HOLD` only.
        n.note(3, 10, at(now, 20));
        n.note(4, 10, at(now, 20));
        let due = n.pop_open(at(now, 25), |_| 1).expect("due");
        n.cut(due, 1, at(now, 25));
        let _probe = n.pop_open(at(now, 40), |_| 1).expect("probe");
        assert_eq!(n.pop_open(at(now, 40), |_| 1), None);
        assert!(n.blocked(&1, at(now, 40)).is_some());
        assert_eq!(n.blocked(&1, at(now, 40) + PROBE_HOLD), None);
        assert!(n.pop_open(at(now, 40) + PROBE_HOLD, |_| 1).is_some());
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

// Successor hints: an owner remembering which other owner fetched a page it
// answered without a copy, so readers ask that owner instead of making it
// fetch the page again (Known issue 21).
//
// An owner above rank 0 reaches origin only after its probe of rank 0 ended
// without a copy: rank 0 missed a copy-only probe (the owner could not fund
// a delegation) or rank 0's own acquisition failed. The owner then fetches
// the page itself, an orphan copy rank 0 does not know about, and rank 0
// fetches it again on its next cold miss. Rank 0 cannot ask the orphan's
// holder itself: the requests that reach it carry one or two credits and a
// few links, too few to route to another owner (the simulator measured
// hint probes from rank 0 missing nine times in ten, mostly on route search).
//
// The reader that reaches rank 0 does have the budget: it visits every owner
// anyway. So an owner that answers another owner's page probe without a copy
// notes the requester ([`SuccessorHints::note`]; a copy-only miss is noted
// without ranking, and [`successor_deferral`] checks ownership), and while the hint is fresh
// it answers a non-owner's page `Acquire` with `Miss` instead of acquiring
// ([`successor_deferral`]). The reader goes on to the next owner in its visit
// order, which includes the holder. Owner requesters are never deferred: an
// owner asks only its predecessors, and an owner below the holder keeps one
// link per later predecessor, too few to reach it (deferring them made rank 2
// fetch more pages itself in the simulator).
//
// Readers do not cover every refetch: owners ranked after this node also
// delegate to it (their `Acquire` carries one origin credit and a few
// links), and a reader's deferral budget runs out. So an owner's own walk
// also asks the hinted owners copy-only before origin
// ([`successor_targets`], [`crate::read::walk::WalkConfig::successors`]),
// each probe funded by a fixed grant rather than the requester's budget
// (the walk's credits never grow; the grant's leftovers are dropped). A hint
// funds one such probe ([`SuccessorHints::probe`]), so the extra exchanges
// are bounded by the hints noted (round 4, orphan2).
//
// Only owners ranked after this node are deferred to or probed: an owner
// fetches after asking its predecessors, so a predecessor that asked this
// node (rank 0's successor probe answered `Miss`) has no copy either, and
// deferring to it bounced readers between two owners without one.
//
// Costs: a hint is noted only on a page probe this node already answered
// without a copy, and needs no message of its own. Each hint defers at most
// [`MAX_DEFERRALS`] requests and expires after [`HINT_TTL`], so a holder
// that lost its copy costs each deferred reader one extra owner probe, and
// at most [`MAX_DEFERRALS`] of them, before this owner fetches again. Origin
// authority is unchanged: a deferral never fetches, and the reader never
// gains authority (non-owners never reach origin).

/// Nodes remembered per page (the latest ones win).
pub const HINTS_PER_PAGE: usize = 2;

/// How long a hint stays usable. Owned pages stay resident, so the holder
/// usually still has the copy minutes later; the simulator's orphan refetches
/// came up to several minutes after the orphan fetch.
pub const HINT_TTL: Duration = Duration::from_secs(600);

/// How long a hint stays usable for this node's own successor probe
/// ([`SuccessorHints::probe`]), at least the deferral TTL. Longer than
/// [`HINT_TTL`]: a stale probe costs one granted copy-only miss, while a
/// stale deferral costs every deferred reader an extra owner visit. Rank 0
/// often refetches an orphan more than 10 minutes after it learned of it
/// (5k `full-failover`: most remaining noted refetches after orphan notices,
/// Known issue 37).
pub const PROBE_TTL: Duration = Duration::from_secs(3600);

/// Deferrals per hint before this owner fetches the page again.
pub const MAX_DEFERRALS: u32 = 8;

/// How old a hint must be before [`SuccessorHints::probe`] returns it. A
/// younger hint names an owner whose own fetch is most likely still running
/// (a concurrent cold miss), and a copy-only probe would only miss.
pub const PROBE_AFTER: Duration = Duration::from_secs(2);

/// Default bound on remembered pages per node.
pub const HINT_CAPACITY: usize = 4096;

/// How long a requester deferred to an offer's sender is remembered
/// ([`SuccessorHints::peek_for`]): a requester that comes back for the page
/// within this window lost the deferral (the sender failed it, for example
/// because it crashed), so this node acquires instead of deferring it again.
/// Twice the default request timeout (30 s): it covers a read's retries,
/// while a new read of the page minutes later is deferred as usual (Known
/// issue 58).
pub const RETURN_WITHIN: Duration = Duration::from_secs(60);

struct HintEntry<N> {
    nodes: Vec<N>,
    at: Instant,
    /// Deferrals made on this entry ([`SuccessorHints::charge`]).
    uses: u32,
    /// Deferrals left before [`SuccessorHints::peek`] stops returning it.
    max: u32,
    /// Already returned by [`SuccessorHints::probe`].
    probed: bool,
    /// The newest node was noted from an offer: it holds the copy, so
    /// [`PROBE_AFTER`] does not apply (Known issue 51).
    offered: bool,
    /// The nodes in `nodes` noted by an offer ([`SuccessorHints::note_offer`])
    /// and not since by [`SuccessorHints::note`].
    senders: Vec<N>,
    /// Requesters deferred to an offer's sender on this entry and when,
    /// newest last, at most [`MAX_DEFERRALS`] ([`SuccessorHints::charge`]).
    /// Kept when the hint is renewed: the requester's return says the
    /// deferral failed it, whoever noted the hint since.
    deferred: Vec<(N, Instant)>,
}

/// Bounded, expiring map from a page to the owners that recently asked this
/// node for it and got no copy.
pub struct SuccessorHints<K, N> {
    capacity: usize,
    ttl: Duration,
    entries: HashMap<K, HintEntry<N>>,
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
            entry.senders.retain(|n| *n != node);
            entry.nodes.push(node);
            if entry.nodes.len() > HINTS_PER_PAGE {
                entry.nodes.remove(0);
            }
            let nodes = &entry.nodes;
            entry.senders.retain(|n| nodes.contains(n));
            entry.at = now;
            entry.uses = 0;
            entry.max = u32::MAX;
            entry.probed = false;
            entry.offered = false;
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
            HintEntry {
                nodes: vec![node],
                at: now,
                uses: 0,
                max: u32::MAX,
                probed: false,
                offered: false,
                senders: Vec::new(),
                deferred: Vec::new(),
            },
        );
        self.order.push_back((key, now));
        self.compact();
    }

    /// `node` offered its copy of `key` (Known issue 37,
    /// [`crate::read::hints`]). Like [`Self::note`] for this node's own
    /// successor probe, but a new hint defers no readers, and an existing
    /// one keeps its deferral count: the holder may be far away or gone by
    /// the time a reader would be sent to it, and this node asks it itself on
    /// its next acquisition (DST `dst_generated_traffic_churn_oracle` seed 1
    /// returned a 503 when offers deferred readers).
    ///
    /// Notes only when this node `holds` no copy and is not `acquiring` one
    /// (then its own copy serves readers, and the offer is redundant).
    /// Returns whether it noted. Either way the offer is answered `Miss`.
    pub fn note_offer(
        &mut self,
        key: K,
        node: N,
        holds: bool,
        acquiring: bool,
        now: Instant,
    ) -> bool {
        if holds || acquiring {
            return false;
        }
        let kept = self.entries.get(&key).map(|e| (e.uses, e.max));
        self.note(key.clone(), node.clone(), now);
        if let Some(entry) = self.entries.get_mut(&key) {
            (entry.uses, entry.max) = kept.unwrap_or((0, 0));
            entry.offered = true;
            entry.senders.push(node);
        }
        true
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

    /// The hints for `key` not older than [`PROBE_TTL`] (or the deferral
    /// TTL, if longer), oldest first, for this node's own walk to ask before
    /// origin ([`successor_targets`]). Each noted hint is returned once, so
    /// one hint funds at most one granted probe per node it names; a renewed
    /// note returns it again. A hint younger than [`PROBE_AFTER`] is held
    /// back unless an offer noted it: an offer's sender holds the copy, and
    /// waiting let rank 0 refetch pages offered a second earlier (Known
    /// issue 51).
    pub fn probe(&mut self, key: &K, now: Instant) -> Vec<N> {
        let ttl = self.ttl.max(PROBE_TTL);
        match self.entries.get_mut(key) {
            Some(e)
                if !e.probed
                    && now.saturating_duration_since(e.at) <= ttl
                    && (e.offered || now.saturating_duration_since(e.at) >= PROBE_AFTER) =>
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

    /// [`Self::peek`] for a deferral of `requester`: nothing if this node
    /// deferred `requester` for `key` to an offer's sender within
    /// [`RETURN_WITHIN`] (Known issue 58). A deferral answers `Miss` and the
    /// requester asks the hinted owner next; a requester that comes back did
    /// not get the page there. An offer's sender (Known issue 51) may have
    /// crashed since it offered, while this node, reached because rank 0
    /// failed the reader, is the one owner left that would fetch; deferring
    /// the returning requester again failed every retry of its read.
    ///
    /// Deferrals to an owner noted from its own miss are not remembered: it
    /// asked this node moments before and is fetching, and a requester that
    /// comes back was most likely shed by it while busy. Acquiring for such
    /// requesters failed more reads in 5k `zipf` (14 to 18 and 16 to 22 on
    /// seeds 2 and 3): the returning reader reaches this owner last, with
    /// too few credits to fetch.
    pub fn peek_for(&self, key: &K, requester: &N, now: Instant) -> Vec<N> {
        let returned = self.entries.get(key).is_some_and(|e| {
            e.deferred.iter().any(|(n, at)| {
                n == requester && now.saturating_duration_since(*at) <= RETURN_WITHIN
            })
        });
        if returned {
            return Vec::new();
        }
        self.peek(key, now)
    }

    /// Count one deferral of `requester` to `to` on `key`'s hints;
    /// [`Self::peek`] stops returning them after `max` (they stay for
    /// [`Self::probe`] until they expire or are evicted). If `to` is an
    /// offer's sender, [`Self::peek_for`] returns nothing for `requester`
    /// within [`RETURN_WITHIN`].
    pub fn charge(&mut self, key: &K, requester: N, to: &N, max: u32, now: Instant) {
        if let Some(entry) = self.entries.get_mut(key) {
            entry.uses += 1;
            entry.max = max;
            if !entry.senders.contains(to) {
                return;
            }
            entry.deferred.retain(|(n, _)| *n != requester);
            entry.deferred.push((requester, now));
            if entry.deferred.len() > MAX_DEFERRALS as usize {
                entry.deferred.remove(0);
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
/// asked anyway.
///
/// A hinted owner that is also a handoff or lineage target stays a successor
/// target: [`with_successors`] moves it to the front on its own grant.
/// Excluding it (round 4, orphan2) left it an opportunistic handoff probe
/// that rank 0, holding the single credit of a delegation, never sends: in
/// 5k `full-failover` most rank 0 refetches of a holder rank 0 had noted were
/// such lineage targets (Known issue 37).
pub fn successor_targets<N: PartialEq + Clone>(hints: &[N], owners: &[N], rank: usize) -> Vec<N> {
    let mut out: Vec<N> = Vec::new();
    for n in hints.iter().rev() {
        let later = owners.iter().position(|o| o == n).is_some_and(|r| r > rank);
        if later && !out.contains(n) {
            out.push(n.clone());
        }
    }
    out
}

/// The handoff targets of an owner walk with `successors` first (at most
/// [`crate::read::walk::MAX_HANDOFF`] of them), at most
/// [`crate::read::walk::MAX_TARGETS`] in all, and how many of them are
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
        if out.len() < crate::read::walk::MAX_TARGETS && !out.contains(n) {
            out.push(n.clone());
        }
    }
    (out, count)
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod successor_tests {
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

    /// Known issue 37: a hint too old for deferrals still funds this
    /// node's successor probe, up to [`PROBE_TTL`].
    #[test]
    fn probe_outlives_the_deferral_ttl() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        h.note(1u64, 7u32, now);
        h.note(2u64, 8u32, now);
        let late = now + HINT_TTL + Duration::from_secs(1);
        assert!(h.peek(&1, late).is_empty());
        assert_eq!(h.probe(&1, late), [7]);
        assert!(
            h.probe(&2, now + PROBE_TTL + Duration::from_secs(1))
                .is_empty()
        );
    }

    /// Known issue 37: an offer arms this node's probe but defers no
    /// readers, and leaves an existing hint's deferral count alone.
    #[test]
    fn offers_arm_probes_without_deferrals() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        h.note_offer(1u64, 7u32, false, false, now);
        assert!(h.peek(&1, now).is_empty());
        assert_eq!(h.probe(&1, now + PROBE_AFTER), [7]);
        h.note(2u64, 8u32, now);
        h.charge(&2, 100, &0, 2, now);
        h.note_offer(2, 9, false, false, now);
        assert_eq!(h.peek(&2, now), [8, 9]);
        h.charge(&2, 100, &0, 2, now);
        assert!(h.peek(&2, now).is_empty());
        assert_eq!(h.probe(&2, now + PROBE_AFTER), [8, 9]);
    }

    /// Known issue 51: an offered hint is probed at once; a miss-noted one
    /// waits `PROBE_AFTER`, and a later miss note restores the wait.
    #[test]
    fn offered_hints_skip_the_probe_delay() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        h.note(1u64, 7u32, now);
        assert!(h.probe(&1, now).is_empty());
        h.note_offer(2u64, 8u32, false, false, now);
        assert_eq!(h.probe(&2, now), [8]);
        assert!(h.probe(&2, now).is_empty());
        h.note_offer(3u64, 9u32, false, false, now);
        h.note(3, 10, now);
        assert!(h.probe(&3, now).is_empty());
        assert_eq!(h.probe(&3, now + PROBE_AFTER), [9, 10]);
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
        h.charge(&1, 100, &0, 2, now);
        assert_eq!(h.peek(&1, now), [7]);
        h.note(1, 7, now);
        h.charge(&1, 100, &0, 2, now);
        assert_eq!(h.peek(&1, now), [7]);
        h.charge(&1, 100, &0, 2, now);
        assert!(h.peek(&1, now).is_empty());
        // The hint stays for this node's own successor probe.
        assert_eq!(h.len(), 1);
        assert_eq!(h.probe(&1, now + PROBE_AFTER), [7]);
        // Charging an absent key is a no-op.
        h.charge(&2, 100, &0, 1, now);
        assert_eq!(h.len(), 1);
    }

    /// Known issue 58: a requester this node deferred to an offer's sender
    /// that comes back within [`RETURN_WITHIN`] is not deferred again (the
    /// sender failed it); other requesters still are, renewals keep the
    /// memory, and it lapses after the window. Deferrals to an owner noted
    /// from its own miss are not remembered.
    #[test]
    fn returning_requesters_are_not_deferred_again() {
        let now = Instant::now();
        let mut h = SuccessorHints::new(8, HINT_TTL);
        h.note(1u64, 7u32, now);
        h.charge(&1, 100, &7, MAX_DEFERRALS, now);
        assert_eq!(h.peek_for(&1, &100, now), [7]);
        h.note_offer(1, 9, false, false, now);
        assert_eq!(h.peek_for(&1, &100, now), [7, 9]);
        h.charge(&1, 100, &9, MAX_DEFERRALS, now);
        assert!(h.peek_for(&1, &100, now).is_empty());
        assert!(h.peek_for(&1, &100, now + RETURN_WITHIN).is_empty());
        assert_eq!(h.peek_for(&1, &101, now), [7, 9]);
        // Renewed by a note: the returning requester still gets no
        // deferral, and the renewed node is no longer a sender.
        h.note(1, 9, now);
        assert!(h.peek_for(&1, &100, now).is_empty());
        h.charge(&1, 101, &9, MAX_DEFERRALS, now);
        assert_eq!(h.peek_for(&1, &101, now), [7, 9]);
        // After the window it is an ordinary requester again.
        let later = now + RETURN_WITHIN + Duration::from_secs(1);
        assert_eq!(h.peek_for(&1, &100, later), [7, 9]);
        // Bounded: the oldest deferred requester is forgotten first.
        h.note_offer(1, 8, false, false, now);
        for r in 0..=MAX_DEFERRALS {
            h.note(1, 9, now);
            h.charge(&1, 200 + r, &8, MAX_DEFERRALS, now);
        }
        assert_eq!(h.peek_for(&1, &200, now), [8, 9]);
        assert!(h.peek_for(&1, &(200 + MAX_DEFERRALS), now).is_empty());
        // A sender pushed out of the hint is no longer one.
        h.note(1, 10, now);
        h.note(1, 11, now);
        h.note_offer(1, 12, false, false, now);
        h.charge(&1, 300, &11, MAX_DEFERRALS, now);
        assert_eq!(h.peek_for(&1, &300, now), [11, 12]);
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
        // Hints older than the probe TTL are never probed.
        h.note(2, 9, now);
        assert!(
            h.probe(&2, now + PROBE_TTL + Duration::from_secs(1))
                .is_empty()
        );
        assert!(h.names(&2, &9));
        assert!(!h.names(&2, &7));
        assert!(!h.names(&3, &9));
    }

    #[test]
    fn successor_targets_are_later_owners_newest_first() {
        let owners = [1u32, 2, 3];
        // Rank 0 asks both later owners, newest hint first.
        assert_eq!(successor_targets(&[2, 3], &owners, 0), [3, 2]);
        // Rank 1 asks only rank 2; a predecessor or non-owner never.
        assert_eq!(successor_targets(&[1, 3, 9], &owners, 1), [3]);
        assert!(successor_targets(&[1, 2], &owners, 2).is_empty());
        assert!(successor_targets(&[], &owners, 0).is_empty());
    }

    /// Known issue 37: a hinted owner that is also a handoff (lineage)
    /// target is asked first on its own grant, not left as an unfunded
    /// handoff probe, and is not asked twice.
    #[test]
    fn hinted_handoff_target_moves_to_the_granted_front() {
        let owners = [1u32, 2, 3];
        let targets = successor_targets(&[2], &owners, 0);
        assert_eq!(targets, [2]);
        let (all, n) = with_successors(targets, &[7, 2]);
        assert_eq!((all, n), (vec![2, 7], 1));
    }

    #[test]
    fn successors_come_first_within_the_handoff_cap() {
        let (all, n) = with_successors(vec![3, 2], &[7, 8]);
        assert_eq!((all, n), (vec![3, 2, 7, 8], 2));
        let (all, n) = with_successors(Vec::new(), &[7, 8]);
        assert_eq!((all, n), (vec![7, 8], 0));
        let (all, n) = with_successors(vec![1, 2, 3, 4], &[7]);
        assert_eq!((all, n), (vec![1, 2, 3, 7], 3));
        // Lineage targets are no longer cut at three (Known issue 46), but
        // the list stays within `MAX_TARGETS`.
        let lineage: Vec<u32> = (10..30).collect();
        let (all, n) = with_successors(vec![1, 2], &lineage);
        assert_eq!(n, 2);
        assert_eq!(all.len(), crate::read::walk::MAX_TARGETS);
        assert_eq!(all[2..], lineage[..crate::read::walk::MAX_TARGETS - 2]);
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
