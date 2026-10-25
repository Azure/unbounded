//! Rank 0 watch: what an owner above rank 0 last learned about rank 0's
//! acquisition of a page, so that it does not wait forever on a rank 0 that
//! hangs (Known issue 39).
//!
//! An owner above rank 0 asks rank 0 for the page before origin. When rank 0
//! accepts the request but does not answer within the attempt, rank 0 is most
//! likely still fetching (its origin request queued behind others), and the
//! owner leaves the fetch to it ([`crate::read::walk::Outcome::TimedOut`],
//! Known issue 31). Without memory, a rank 0 (or a relay on the route) that
//! accepts requests and never answers, without failing the connection,
//! blocks the page for as long as it stays a member: every owner walk ends
//! without origin authority.
//!
//! The owner therefore notes a page when its probe of rank 0 timed out
//! ([`Rank0Watch::timed_out`]). For [`Rank0Watch::hang_after`] (half the
//! node's client request timeout) after that, or until the request's own deadline
//! if later, the page is [`Rank0State::Fetching`] and the timeout rule
//! holds. After it the page is [`Rank0State::Overdue`]: a further timeout
//! gives authority like a transport failure, and the page stays overdue
//! while timeouts keep coming. Any answer signed by rank 0
//! ([`Rank0Watch::answered`]) forgets the page.
//!
//! `Fetching` does not withhold origin after other failures of rank 0. The
//! simulator measured that variant (a route failure, `Overloaded` or shed
//! from a rank 0 found fetching withheld origin like a timeout): in 5k
//! `zipf` at 16 MiB pages it removed 71 to 80 of about 110 overlapping
//! origin requests but failed 29 to 55 more reads (seeds 1 and 2). Nearly
//! all of those `Overloaded` answers are signed by a relay on the route
//! whose relay quota is full, not by rank 0, so they say nothing about rank
//! 0, and an owner that does not fetch leaves its readers unserved (Known
//! issue 40).
//!
//! Why a fixed allowance and not the request's deadline: rank 0's
//! acquisition outlives the request that started it while other waiters
//! join it (flight handover), and an owner's request often carries a short
//! deadline (a reader's attempt share). In the simulator an owner's first
//! deadline was about 3 s after the timeout while rank 0's origin request
//! stayed queued for longer, and taking authority then overlapped it. Why
//! half the request timeout: a read that met the first timeout still has
//! time to retry once rank 0 is presumed hung. In the simulator (10 s read
//! timeout) 5 s added no overlapping origin request in 5k `zipf` at 16 MiB
//! pages (3 s added 44), and with 50 of 5,000 members hung it failed 136
//! reads where 10 s failed 188 and no watch 371.
//!
//! A hung peer server hangs for every page, so a timeout on a page that is
//! already overdue also marks rank 0 itself silent: its other pages are
//! overdue at once, until it answers any probe from this owner or the mark
//! expires. Without it each page rank 0 owns would fail its readers for
//! another `hang_after`.
//!
//! Rank 0's own failures (Known issue 52). When rank 0 itself answers an
//! owner's probe `Unavailable`, its acquisition of the page failed, in
//! practice at origin; the owner notes it ([`Rank0Watch::failed`]) and,
//! for [`Rank0Watch::hang_after`] after the first such failure, the page is
//! [`Rank0State::Failed`] and the owner does not fetch the page itself
//! after a further failure of rank 0 (the walk withholds authority on
//! [`crate::read::walk::Outcome::Failed`] unless the page is `Overdue`).
//! Past it the page is `Overdue`: this owner fetches once, and the next
//! failure starts a new interval. While origin keeps failing, the owner
//! thus adds at most one origin request per page per `hang_after` to
//! rank 0's (one per reader retry) instead of one per reader retry, and a
//! failure local to rank 0 (its route to origin) delays the page by at
//! most `hang_after`. In the simulator, 5k `full-failover` (180 s origin
//! 5xx burst), owners above rank 0 doubled the requests on every page that
//! kept failing.
//!
//! Costs: one entry per page an owner deferred to rank 0, bounded by
//! [`WATCH_CAPACITY`] and expiring [`WATCH_TTL`] after the last timeout.
//! No message is added and no budget grows: the rule only changes whether a
//! walk that already ended ends with origin authority. A hung rank 0 blocks
//! a page for at most `hang_after` after the first timeout; a rank 0
//! that is down or partitioned fails at the transport level, which gives
//! authority as before.

use std::collections::{HashMap, VecDeque};
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Default bound on watched pages per node.
pub const WATCH_CAPACITY: usize = 4096;

/// Default [`Rank0Watch::hang_after`]: half the dataplane's default client
/// request timeout.
pub const HANG_AFTER: Duration = Duration::from_secs(15);

/// How long an entry outlives the last timeout that renewed it.
pub const WATCH_TTL: Duration = Duration::from_secs(60);

/// What this owner knows about rank 0's acquisition of a page.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Rank0State {
    /// Nothing recent: rules as without the watch.
    #[default]
    Unknown,
    /// A probe of rank 0 timed out less than [`Rank0Watch::hang_after`]
    /// ago (or before its request's deadline, if later) and rank 0 has not
    /// answered since: rank 0 is presumed to be fetching.
    Fetching,
    /// Rank 0 has not answered this owner since a timeout that long ago, or
    /// is marked silent: rank 0 is presumed hung. Also: rank 0 has kept
    /// failing the page for that long ([`Rank0Watch::failed`]).
    Overdue,
    /// Rank 0 itself answered that its acquisition of the page failed less
    /// than [`Rank0Watch::hang_after`] ago: leave the next attempt to it.
    Failed,
}

struct Entry<N> {
    /// Rank 0 when the page was noted.
    node: N,
    /// Deadline of the first unanswered request: rank 0 must have answered
    /// or failed it by then.
    until: Instant,
    /// Last timeout or failure seen (expiry and eviction stamp).
    last: Instant,
    /// Noted by [`Rank0Watch::failed`], not by a timeout.
    failed: bool,
}

/// Bounded, expiring map from a page to rank 0's presumed state.
pub struct Rank0Watch<K, N> {
    capacity: usize,
    ttl: Duration,
    hang_after: Duration,
    entries: HashMap<K, Entry<N>>,
    /// Ranks 0 that stayed silent past a request deadline, with when that
    /// was last confirmed (at most `capacity`).
    silent: HashMap<N, Instant>,
    /// Insertion order for eviction; stale stamps are skipped.
    order: VecDeque<(K, Instant)>,
}

impl<K: Eq + Hash + Clone, N: Eq + Hash + Clone> Rank0Watch<K, N> {
    /// `hang_after` is how long rank 0 may stay silent on a page after the
    /// first timeout before it is presumed hung (half the node's client
    /// request timeout); `ttl` should exceed it.
    pub fn new(capacity: usize, ttl: Duration, hang_after: Duration) -> Self {
        Self {
            capacity: capacity.max(1),
            ttl: ttl.max(hang_after),
            hang_after,
            entries: HashMap::new(),
            silent: HashMap::new(),
            order: VecDeque::new(),
        }
    }

    pub fn hang_after(&self) -> Duration {
        self.hang_after
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// The presumed state of `rank0`'s acquisition of `key` at `now`.
    pub fn state(&self, key: &K, rank0: &N, now: Instant) -> Rank0State {
        let silent = self
            .silent
            .get(rank0)
            .is_some_and(|at| now.saturating_duration_since(*at) <= self.ttl);
        match self.entries.get(key) {
            Some(e) if e.node == *rank0 && now.saturating_duration_since(e.last) <= self.ttl => {
                if e.failed {
                    if now < e.until {
                        Rank0State::Failed
                    } else {
                        Rank0State::Overdue
                    }
                } else if now < e.until && !silent {
                    Rank0State::Fetching
                } else {
                    Rank0State::Overdue
                }
            }
            _ if silent => Rank0State::Overdue,
            _ => Rank0State::Unknown,
        }
    }

    /// This owner's probe of rank 0 for `key` was sent and not answered
    /// within its attempt; the request carried `deadline`. The first
    /// timeout's bound stands while the entry lives, so repeated timeouts
    /// cannot postpone [`Rank0State::Overdue`].
    /// A timeout on a page that is already overdue marks `rank0` silent.
    pub fn timed_out(&mut self, key: K, rank0: N, now: Instant, deadline: Instant) {
        let live = self.entries.get(&key).is_some_and(|e| {
            e.node == rank0 && !e.failed && now.saturating_duration_since(e.last) <= self.ttl
        });
        if live {
            let entry = self.entries.get_mut(&key).expect("live");
            entry.last = now;
            if now >= entry.until {
                if self.silent.len() >= self.capacity && !self.silent.contains_key(&rank0) {
                    let ttl = self.ttl;
                    self.silent
                        .retain(|_, at| now.saturating_duration_since(*at) <= ttl);
                }
                if self.silent.len() < self.capacity || self.silent.contains_key(&rank0) {
                    self.silent.insert(rank0, now);
                }
            }
        } else {
            self.insert(
                key.clone(),
                Entry {
                    node: rank0,
                    until: deadline.max(now + self.hang_after),
                    last: now,
                    failed: false,
                },
            );
        }
        self.order.push_back((key, now));
        self.compact();
    }

    /// `rank0` itself answered this owner's probe for `key` that its own
    /// acquisition failed (Known issue 52). It is not hung (the silent mark
    /// is cleared). The first failure starts an interval of
    /// [`Rank0Watch::hang_after`] during which the page is
    /// [`Rank0State::Failed`]; further failures inside it do not extend it.
    /// A failure after it (the owner was `Overdue` and fetched itself)
    /// starts the next interval.
    pub fn failed(&mut self, key: K, rank0: N, now: Instant) {
        self.silent.remove(&rank0);
        let live = self.entries.get(&key).is_some_and(|e| {
            e.node == rank0
                && e.failed
                && now < e.until
                && now.saturating_duration_since(e.last) <= self.ttl
        });
        if live {
            let entry = self.entries.get_mut(&key).expect("live");
            entry.last = now;
        } else {
            self.insert(
                key.clone(),
                Entry {
                    node: rank0,
                    until: now + self.hang_after,
                    last: now,
                    failed: true,
                },
            );
        }
        self.order.push_back((key, now));
        self.compact();
    }

    /// Replace `key`'s entry, evicting the oldest pages beyond capacity.
    fn insert(&mut self, key: K, entry: Entry<N>) {
        self.entries.remove(&key);
        while self.entries.len() >= self.capacity {
            let Some((old, at)) = self.order.pop_front() else {
                break;
            };
            if self.entries.get(&old).is_some_and(|e| e.last == at) {
                self.entries.remove(&old);
            }
        }
        self.entries.insert(key, entry);
    }

    /// `rank0` itself answered this owner's probe for `key` (a copy, a
    /// miss, or a failure of its own): it is not hung, forget the page and
    /// the silent mark.
    pub fn answered(&mut self, key: &K, rank0: &N) {
        self.entries.remove(key);
        self.silent.remove(rank0);
    }

    /// Bound the eviction queue: drop stamps that no longer match.
    fn compact(&mut self) {
        if self.order.len() <= 2 * self.capacity {
            return;
        }
        let entries = &self.entries;
        self.order
            .retain(|(k, at)| entries.get(k).is_some_and(|e| e.last == *at));
    }
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

    const S: Duration = Duration::from_secs(1);
    const A: u8 = 7;

    #[test]
    fn a_timeout_means_fetching_until_the_request_deadline_then_overdue() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, WATCH_TTL, Duration::ZERO);
        assert_eq!(w.state(&1, &A, now), Rank0State::Unknown);
        w.timed_out(1, A, now, now + 10 * S);
        assert_eq!(w.state(&1, &A, now), Rank0State::Fetching);
        assert_eq!(w.state(&1, &A, now + 9 * S), Rank0State::Fetching);
        // A different rank 0 (membership changed) knows nothing yet.
        assert_eq!(w.state(&1, &8, now), Rank0State::Unknown);
        // A later timeout does not postpone the first deadline.
        w.timed_out(1, A, now + 9 * S, now + 19 * S);
        assert_eq!(w.state(&1, &A, now + 10 * S), Rank0State::Overdue);
        assert_eq!(w.state(&2, &A, now + 10 * S), Rank0State::Unknown);
    }

    #[test]
    fn rank_zero_gets_at_least_a_request_timeout() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, S, 10 * S);
        assert_eq!(w.hang_after(), 10 * S);
        // A short request deadline does not shorten the wait.
        w.timed_out(1, A, now, now + 3 * S);
        assert_eq!(w.state(&1, &A, now + 9 * S), Rank0State::Fetching);
        assert_eq!(w.state(&1, &A, now + 10 * S), Rank0State::Overdue);
        // A longer one extends it; the TTL never undercuts the wait.
        w.timed_out(2, A, now, now + 20 * S);
        assert_eq!(w.state(&2, &A, now + 19 * S), Rank0State::Unknown);
        w.timed_out(3, A, now, now + 20 * S);
        w.timed_out(3, A, now + 10 * S, now + 20 * S);
        assert_eq!(w.state(&3, &A, now + 19 * S), Rank0State::Fetching);
        assert_eq!(w.state(&3, &A, now + 20 * S), Rank0State::Overdue);
    }

    #[test]
    fn an_answer_forgets_and_an_old_entry_expires() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, 5 * S, Duration::ZERO);
        w.timed_out(1, A, now, now + S);
        w.answered(&1, &A);
        assert_eq!(w.state(&1, &A, now), Rank0State::Unknown);
        w.timed_out(2, A, now, now + S);
        assert_eq!(w.state(&2, &A, now + 5 * S), Rank0State::Overdue);
        assert_eq!(w.state(&2, &A, now + 6 * S), Rank0State::Unknown);
        // Renewed while overdue: stays overdue.
        w.timed_out(2, A, now + 4 * S, now + 14 * S);
        assert_eq!(w.state(&2, &A, now + 9 * S), Rank0State::Overdue);
        // Expired, then noted again: a fresh deadline.
        w.timed_out(2, A, now + 20 * S, now + 30 * S);
        assert_eq!(w.state(&2, &A, now + 21 * S), Rank0State::Fetching);
    }

    #[test]
    fn a_rank_zero_silent_past_a_deadline_is_overdue_for_every_page() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, WATCH_TTL, Duration::ZERO);
        w.timed_out(1, A, now, now + S);
        // Overdue but not yet confirmed: other pages are unaffected.
        assert_eq!(w.state(&2, &A, now + 2 * S), Rank0State::Unknown);
        // Another timeout past the deadline marks rank 0 silent.
        w.timed_out(1, A, now + 2 * S, now + 12 * S);
        assert_eq!(w.state(&2, &A, now + 2 * S), Rank0State::Overdue);
        assert_eq!(w.state(&2, &8, now + 2 * S), Rank0State::Unknown);
        // A page noted fetching is overdue too while rank 0 is silent.
        w.timed_out(3, A, now + 3 * S, now + 13 * S);
        assert_eq!(w.state(&3, &A, now + 3 * S), Rank0State::Overdue);
        // The mark expires.
        assert_eq!(
            w.state(&2, &A, now + 3 * S + WATCH_TTL),
            Rank0State::Unknown
        );
        // Any answer from rank 0 clears it.
        w.answered(&9, &A);
        assert_eq!(w.state(&2, &A, now + 3 * S), Rank0State::Unknown);
        assert_eq!(w.state(&3, &A, now + 3 * S), Rank0State::Fetching);
    }

    #[test]
    fn capacity_evicts_the_oldest_page_and_bounds_silent_nodes() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(2, WATCH_TTL, Duration::ZERO);
        w.timed_out(1, A, now, now + S);
        w.timed_out(2, A, now + S, now + S);
        w.timed_out(1, A, now + 2 * S, now + S);
        w.timed_out(3, A, now + 3 * S, now + S);
        assert_eq!(w.len(), 2);
        assert!(!w.entries.contains_key(&2));
        for i in 0..100u8 {
            w.timed_out(u64::from(i), i, now, now);
            w.timed_out(u64::from(i), i, now, now);
        }
        assert_eq!(w.len(), 2);
        assert!(w.order.len() <= 4);
        assert!(w.silent.len() <= 2);
    }

    #[test]
    fn rank_zero_failures_hold_the_page_for_hang_after_then_once_overdue() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, WATCH_TTL, 10 * S);
        w.failed(1, A, now);
        assert_eq!(w.state(&1, &A, now), Rank0State::Failed);
        assert_eq!(w.state(&1, &8, now), Rank0State::Unknown);
        assert_eq!(w.state(&2, &A, now), Rank0State::Unknown);
        // Further failures inside the interval do not extend it.
        w.failed(1, A, now + 9 * S);
        assert_eq!(w.state(&1, &A, now + 9 * S), Rank0State::Failed);
        assert_eq!(w.state(&1, &A, now + 10 * S), Rank0State::Overdue);
        // The overdue walk's failure starts the next interval.
        w.failed(1, A, now + 11 * S);
        assert_eq!(w.state(&1, &A, now + 11 * S), Rank0State::Failed);
        assert_eq!(w.state(&1, &A, now + 21 * S), Rank0State::Overdue);
        // An answer forgets it; an old entry expires.
        w.answered(&1, &A);
        assert_eq!(w.state(&1, &A, now + 12 * S), Rank0State::Unknown);
        w.failed(3, A, now);
        assert_eq!(
            w.state(&3, &A, now + 11 * S + WATCH_TTL),
            Rank0State::Unknown
        );
    }

    #[test]
    fn a_failure_answer_clears_silence_and_replaces_a_timeout_entry() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, WATCH_TTL, Duration::ZERO);
        w.timed_out(1, A, now, now + S);
        w.timed_out(1, A, now + 2 * S, now + 3 * S);
        assert_eq!(w.state(&2, &A, now + 2 * S), Rank0State::Overdue);
        let mut w2 = Rank0Watch::new(8, WATCH_TTL, 5 * S);
        w2.timed_out(1, A, now, now + S);
        w2.failed(1, A, now + S);
        assert_eq!(w2.state(&1, &A, now + 2 * S), Rank0State::Failed);
        // A timeout after a failure starts a fresh timeout entry.
        w2.timed_out(1, A, now + 3 * S, now + 4 * S);
        assert_eq!(w2.state(&1, &A, now + 3 * S), Rank0State::Fetching);
        // Rank 0 answering a failure is not silent.
        w.failed(1, A, now + 2 * S);
        assert_eq!(w.state(&2, &A, now + 2 * S), Rank0State::Unknown);
    }

    #[test]
    fn failures_respect_capacity() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(2, WATCH_TTL, S);
        for i in 0..100u64 {
            w.failed(i, A, now);
        }
        assert_eq!(w.len(), 2);
        assert!(w.order.len() <= 4);
    }
}
