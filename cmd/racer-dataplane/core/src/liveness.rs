//! Peer silence: a neighbor that accepts requests and never answers them.

use std::collections::{HashMap, VecDeque};
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Default bound on tracked peers per sender (neighbors and direct peers).
pub const SILENCE_CAPACITY: usize = 256;

/// Silence pings a worker sends at once; more due pings wait for a later turn.
pub const MAX_PINGS_IN_FLIGHT: usize = 16;

/// Orphan copy offers a worker sends at once.
pub const MAX_OFFERS_IN_FLIGHT: usize = 4;

/// How long an entry outlives its last event.
pub(crate) const SILENCE_TTL: Duration = Duration::from_secs(60);

/// Unanswered exchanges over the last link before its sender pings.
pub const LINK_TIMEOUTS: u32 = 2;

/// Unanswered probes of a page's rank 0 before the prober pings it. Two
/// failed ten times as many reads with 50 of 5,000 members hung in the sim.
pub const RANK0_TIMEOUTS: u32 = 1;

/// Never ping: an owner above rank 0 usually waited on rank 0, so its timeout
/// only demotes it ([`Silence::unanswered`]).
pub const NO_PING: u32 = u32::MAX;

#[derive(Clone, Debug)]
struct SilenceEntry<P> {
    /// What the last unanswered exchange asked for; a ping carries it.
    subject: P,
    want: bool,
    timeouts: u32,
    pinging: Option<Instant>,
    silent: bool,
    /// Next ping for a silent peer, or end of quiet for an answered one.
    next: Instant,
    busy_until: Option<Instant>,
    last: Instant,
    /// Insertion order, to evict deterministically among equal stamps.
    seq: u64,
}

/// Classify a ping: `Ok((by_peer, unavailable))` for a signed answer, `Err(timed_out)` for none.
pub fn ping_verdict(answer: Result<(bool, bool), bool>) -> Option<bool> {
    match answer {
        Ok((true, _)) => Some(true),
        Ok((false, true)) | Err(true) => Some(false),
        Ok((false, false)) | Err(false) => None,
    }
}

/// Bounded, expiring map from a destination to its silence. `P` is what a ping carries.
pub struct Silence<N, P = ()> {
    capacity: usize,
    hang_after: Duration,
    ping_timeout: Duration,
    entries: HashMap<N, SilenceEntry<P>>,
    seq: u64,
}

impl<N: Eq + Hash + Clone, P: Clone> Silence<N, P> {
    /// `hang_after` is half the client request timeout.
    pub fn new(capacity: usize, hang_after: Duration, ping_timeout: Duration) -> Self {
        Self {
            capacity: capacity.max(1),
            hang_after,
            ping_timeout,
            entries: HashMap::new(),
            seq: 0,
        }
    }

    /// Shorter exchanges (hedge losers, canceled reads) are not evidence.
    pub fn min_wait(&self) -> Duration {
        self.hang_after / 10
    }

    /// How often a silent peer is pinged again.
    pub fn hold(&self) -> Duration {
        self.hang_after.saturating_mul(2)
    }

    pub fn ping_timeout(&self) -> Duration {
        self.ping_timeout
    }

    /// How long a peer that answered is not pinged again (matches the time
    /// [`Rank0Watch`] gives a rank 0 that may still be fetching).
    pub fn quiet(&self) -> Duration {
        self.hang_after
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// An exchange sent to `peer` at `sent` ended unanswered at `now`.
    /// Returns whether a ping is wanted now (after `needed` such exchanges).
    pub fn timed_out(
        &mut self,
        peer: N,
        subject: P,
        sent: Instant,
        now: Instant,
        needed: u32,
    ) -> bool {
        if now.saturating_duration_since(sent) < self.min_wait() {
            return false;
        }
        let stale = self.ping_timeout.saturating_mul(2);
        if let Some(entry) = self.entries.get_mut(&peer) {
            entry.last = entry.last.max(now);
            entry.subject = subject;
            entry.timeouts = entry.timeouts.saturating_add(1);
            let pinging = entry
                .pinging
                .is_some_and(|at| now.saturating_duration_since(at) < stale);
            let want = !entry.silent && !pinging && now >= entry.next && entry.timeouts >= needed;
            entry.want |= want;
            return want;
        }
        self.make_room(now);
        self.entries.insert(
            peer,
            SilenceEntry {
                subject,
                want: needed <= 1,
                timeouts: 1,
                pinging: None,
                silent: false,
                next: now,
                busy_until: None,
                last: now,
                seq: self.seq,
            },
        );
        self.seq += 1;
        needed <= 1
    }

    /// `peer` signed an `Overloaded`: alive but busy, so readers visit it last for [`Self::quiet`].
    /// Otherwise a retried read repeats the same deterministic walk and is shed again.
    pub fn shed(&mut self, peer: N, subject: P, now: Instant) {
        let quiet = self.quiet();
        if !self.entries.contains_key(&peer) {
            self.make_room(now);
            self.entries.insert(
                peer.clone(),
                SilenceEntry {
                    subject,
                    want: false,
                    timeouts: 0,
                    pinging: None,
                    silent: false,
                    next: now,
                    busy_until: None,
                    last: now,
                    seq: self.seq,
                },
            );
            self.seq += 1;
        }
        self.answered(&peer, now);
        if let Some(entry) = self.entries.get_mut(&peer) {
            entry.busy_until = Some(now + quiet);
        }
    }

    pub fn start_ping(&mut self, peer: &N, now: Instant) {
        if let Some(entry) = self.entries.get_mut(peer) {
            entry.want = false;
            entry.pinging = Some(now);
        }
    }

    /// Up to `max` peers to ping now, oldest first, marked as pinging.
    pub fn due_pings(&mut self, now: Instant, max: usize) -> Vec<(N, P)> {
        let stale = self.ping_timeout.saturating_mul(2);
        let mut due: Vec<(u64, N)> = Vec::new();
        for (peer, entry) in &self.entries {
            if entry
                .pinging
                .is_some_and(|at| now.saturating_duration_since(at) < stale)
            {
                continue;
            }
            if entry.want || (entry.silent && now >= entry.next) {
                due.push((entry.seq, peer.clone()));
            }
        }
        due.sort_unstable_by_key(|(seq, _)| *seq);
        due.truncate(max);
        due.into_iter()
            .filter_map(|(_, peer)| {
                let entry = self.entries.get_mut(&peer)?;
                entry.want = false;
                entry.pinging = Some(now);
                Some((peer, entry.subject.clone()))
            })
            .collect()
    }

    /// Record a [`ping_verdict`]. Returns whether `peer` became silent.
    pub fn ping_result(&mut self, peer: &N, answered: Option<bool>, now: Instant) -> bool {
        let (hold, quiet) = (self.hold(), self.quiet());
        let (Some(entry), Some(answered)) = (self.entries.get_mut(peer), answered) else {
            return false;
        };
        if entry.pinging.take().is_none() && !answered {
            return false;
        }
        let became = !answered && !entry.silent;
        if answered {
            entry.timeouts = 0;
        }
        entry.silent = !answered;
        entry.want = false;
        entry.next = now + if answered { quiet } else { hold };
        entry.last = entry.last.max(now);
        became
    }

    /// `peer` answered: clear its evidence and quiet it. Untracked peers get no entry.
    pub fn answered(&mut self, peer: &N, now: Instant) {
        let quiet = self.quiet();
        if let Some(entry) = self.entries.get_mut(peer) {
            entry.want = false;
            entry.timeouts = 0;
            entry.pinging = None;
            entry.silent = false;
            entry.next = entry.next.max(now + quiet);
            entry.busy_until = None;
            entry.last = entry.last.max(now);
        }
    }

    /// Whether readers should visit `peer` after a page's other owners.
    pub fn unanswered(&self, peer: &N, now: Instant) -> bool {
        self.entries.get(peer).is_some_and(|e| {
            ((e.silent || e.timeouts > 0) && now.saturating_duration_since(e.last) <= SILENCE_TTL)
                || e.busy_until.is_some_and(|until| now < until)
        })
    }

    pub fn silent(&self, peer: &N, now: Instant) -> bool {
        self.entries.get(peer).is_some_and(|e| holds(e, now))
    }

    pub fn silent_peers(&self, now: Instant) -> impl Iterator<Item = &N> {
        self.entries
            .iter()
            .filter(move |(_, e)| holds(e, now))
            .map(|(n, _)| n)
    }

    pub fn retain(&mut self, mut keep: impl FnMut(&N) -> bool) {
        self.entries.retain(|n, _| keep(n));
    }

    fn make_room(&mut self, now: Instant) {
        if self.entries.len() < self.capacity {
            return;
        }
        self.entries
            .retain(|_, e| now.saturating_duration_since(e.last) <= SILENCE_TTL);
        if self.entries.len() >= self.capacity
            && let Some(oldest) = self
                .entries
                .iter()
                .min_by_key(|(_, e)| (e.last, e.seq))
                .map(|(n, _)| n.clone())
        {
            self.entries.remove(&oldest);
        }
    }
}

fn holds<P>(e: &SilenceEntry<P>, now: Instant) -> bool {
    e.silent && now.saturating_duration_since(e.last) <= SILENCE_TTL
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod silence_tests {
    use super::*;

    #[test]
    fn ping_verdicts() {
        for (answer, verdict) in [
            (Ok((true, true)), Some(true)),
            (Ok((true, false)), Some(true)),
            (Ok((false, true)), Some(false)),
            (Err(true), Some(false)),
            (Ok((false, false)), None),
            (Err(false), None),
        ] {
            assert_eq!(ping_verdict(answer), verdict, "{answer:?}");
        }
    }

    const S: Duration = Duration::from_secs(1);

    fn silence() -> Silence<u8> {
        Silence::new(4, 5 * S, 2 * S)
    }

    #[test]
    fn a_timeout_asks_a_ping_and_only_an_unanswered_ping_makes_a_peer_silent() {
        let t = Instant::now();
        let mut s = silence();
        assert_eq!(s.min_wait(), S / 2);
        assert_eq!(s.hold(), 10 * S);
        assert_eq!(s.ping_timeout(), 2 * S);
        // A timeout is a question, not a verdict.
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        assert!(!s.silent(&1, t + 2 * S));
        assert_eq!(s.due_pings(t + 2 * S, 16), vec![(1, ())]);
        // One ping at a time per peer, whatever else times out meanwhile.
        assert!(!s.timed_out(1, (), t + S, t + 3 * S, 1));
        assert!(s.due_pings(t + 3 * S, 16).is_empty());
        // The ping went unanswered: silent until it answers anything.
        assert!(s.ping_result(&1, Some(false), t + 4 * S));
        assert!(s.silent(&1, t + 4 * S));
        assert!(s.silent_peers(t + 4 * S).eq([&1]));
        assert!(!s.silent(&2, t + 4 * S));
        // A second report of the same ping changes nothing.
        assert!(!s.ping_result(&1, Some(false), t + 5 * S));
        assert!(s.silent(&1, t + 30 * S));
    }

    #[test]
    fn a_silent_peer_is_pinged_every_hold_and_cleared_by_an_answer() {
        let t = Instant::now();
        let mut s = silence();
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        s.start_ping(&1, t + 2 * S);
        assert!(s.ping_result(&1, Some(false), t + 4 * S));
        // While silent, late timeouts ask nothing.
        assert!(!s.timed_out(1, (), t + 3 * S, t + 5 * S, 1));
        assert!(s.due_pings(t + 13 * S, 16).is_empty());
        assert_eq!(s.due_pings(t + 14 * S, 16), vec![(1, ())]);
        // Still hung: silent, renewed for another hold.
        assert!(!s.ping_result(&1, Some(false), t + 16 * S));
        assert!(s.silent(&1, t + 16 * S + SILENCE_TTL));
        assert_eq!(s.due_pings(t + 26 * S, 16), vec![(1, ())]);
        // Recovered: the ping is answered.
        s.ping_result(&1, Some(true), t + 26 * S);
        assert!(!s.silent(&1, t + 26 * S));
        // Not pinged again for `quiet`, whatever times out.
        assert_eq!(s.quiet(), 5 * S);
        assert!(!s.timed_out(1, (), t + 26 * S, t + 30 * S, 1));
        assert!(s.due_pings(t + 30 * S, 16).is_empty());
        assert!(s.timed_out(1, (), t + 26 * S, t + 31 * S, 1));
        // A silent verdict expires SILENCE_TTL after its last event.
        s.start_ping(&1, t + 31 * S);
        assert!(s.ping_result(&1, Some(false), t + 33 * S));
        assert!(!s.silent(&1, t + 34 * S + SILENCE_TTL));
    }

    #[test]
    fn any_answer_clears_and_voids_an_outstanding_ping() {
        let t = Instant::now();
        let mut s = silence();
        // An untracked peer's answer adds no entry.
        s.answered(&9, t);
        assert!(s.is_empty());
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        s.start_ping(&1, t + 2 * S);
        s.answered(&1, t + 3 * S);
        // The peer answered meanwhile: a lost ping proves nothing.
        assert!(!s.ping_result(&1, Some(false), t + 4 * S));
        assert!(!s.silent(&1, t + 4 * S));
        assert!(!s.unanswered(&1, t + 4 * S));
        // Past the quiet time after the answer, a timeout asks again.
        assert!(s.timed_out(1, (), t + 6 * S, t + 8 * S, 1));
        s.start_ping(&1, t + 8 * S);
        assert!(s.ping_result(&1, Some(false), t + 10 * S));
        // An answer clears a silent peer.
        s.answered(&1, t + 10 * S);
        assert!(!s.silent(&1, t + 10 * S));
        assert!(s.silent_peers(t + 10 * S).next().is_none());
        assert!(s.due_pings(t + 30 * S, 16).is_empty());
    }

    #[test]
    fn a_peer_that_answered_recently_is_not_pinged_for_the_quiet_time() {
        // A busy peer answers some exchanges and leaves others (that joined
        // its running work) unanswered; an answer must not forget it.
        let t = Instant::now();
        let mut s = silence();
        assert!(s.timed_out(1, (), t, t + 2 * S, RANK0_TIMEOUTS));
        s.start_ping(&1, t + 2 * S);
        s.ping_result(&1, Some(true), t + 3 * S);
        // It answers an exchange at 7 s: quiet until 12 s.
        s.answered(&1, t + 7 * S);
        assert!(!s.timed_out(1, (), t + 8 * S, t + 10 * S, RANK0_TIMEOUTS));
        assert!(s.due_pings(t + 11 * S, 16).is_empty());
        // Still demoted by the unanswered exchange meanwhile.
        assert!(s.unanswered(&1, t + 11 * S));
        // A peer that hangs after its last answer is pinged once the quiet
        // time has passed, and found silent.
        assert!(s.timed_out(1, (), t + 10 * S, t + 12 * S, RANK0_TIMEOUTS));
        s.start_ping(&1, t + 12 * S);
        assert!(s.ping_result(&1, Some(false), t + 14 * S));
        assert!(s.silent(&1, t + 14 * S));
        // An answer never shortens a quiet time already set.
        s.ping_result(&1, Some(true), t + 20 * S);
        s.answered(&1, t + 16 * S);
        assert!(!s.timed_out(1, (), t + 22 * S, t + 24 * S, RANK0_TIMEOUTS));
        assert!(s.timed_out(1, (), t + 23 * S, t + 25 * S, RANK0_TIMEOUTS));
    }

    #[test]
    fn a_peer_that_shed_this_node_is_busy_for_the_quiet_time() {
        // An owner that signed `Overloaded` for this node is alive and busy.
        let t = Instant::now();
        let mut s = silence();
        assert!(!s.unanswered(&1, t));
        s.shed(1, (), t);
        assert!(s.unanswered(&1, t + S));
        assert!(!s.silent(&1, t + S));
        assert!(s.due_pings(t + S, 16).is_empty());
        assert!(s.unanswered(&1, t + 5 * S - S / 2));
        assert!(!s.unanswered(&1, t + 5 * S));
        // Another answer ends it at once.
        s.shed(1, (), t + 10 * S);
        s.answered(&1, t + 11 * S);
        assert!(!s.unanswered(&1, t + 11 * S));
        // A ping answer does not: it is answered at once, even when busy.
        s.shed(1, (), t + 20 * S);
        assert!(s.timed_out(2, (), t + 18 * S, t + 21 * S, RANK0_TIMEOUTS));
        s.start_ping(&2, t + 21 * S);
        s.ping_result(&1, Some(true), t + 22 * S);
        assert!(s.unanswered(&1, t + 22 * S));
        // A shed clears a silent verdict (the peer answered) and voids an outstanding ping.
        assert!(s.ping_result(&2, Some(false), t + 23 * S));
        assert!(s.silent(&2, t + 23 * S));
        s.shed(2, (), t + 24 * S);
        assert!(!s.silent(&2, t + 24 * S));
        assert!(s.unanswered(&2, t + 24 * S));
        assert!(!s.ping_result(&2, Some(false), t + 25 * S));
        // Bounded like any entry.
        for i in 10..20u8 {
            s.shed(i, (), t + 30 * S);
            assert!(s.len() <= 4);
        }
    }

    #[test]
    fn a_link_pings_after_two_timeouts_without_an_answer() {
        let t = Instant::now();
        let mut s = silence();
        assert!(!s.timed_out(1, (), t, t + S, LINK_TIMEOUTS));
        assert!(s.due_pings(t + S, 16).is_empty());
        // Any answer in between starts the count again (and quiets it).
        s.answered(&1, t + S);
        assert!(!s.timed_out(1, (), t + 5 * S, t + 6 * S, LINK_TIMEOUTS));
        assert!(s.timed_out(1, (), t + 5 * S, t + 7 * S, LINK_TIMEOUTS));
        assert_eq!(s.due_pings(t + 7 * S, 16), vec![(1, ())]);
        // An answered ping also starts it again (after the quiet time).
        s.ping_result(&1, Some(true), t + 7 * S);
        assert!(!s.timed_out(1, (), t + 12 * S, t + 13 * S, LINK_TIMEOUTS));
        assert!(s.timed_out(1, (), t + 12 * S, t + 14 * S, LINK_TIMEOUTS));
        assert_eq!(RANK0_TIMEOUTS, 1);
    }

    #[test]
    fn a_reader_timeout_asks_no_ping_but_marks_the_peer_unanswered() {
        let t = Instant::now();
        let mut s = silence();
        assert!(!s.unanswered(&1, t));
        assert!(!s.timed_out(1, (), t, t + 2 * S, NO_PING));
        assert!(!s.timed_out(1, (), t, t + 3 * S, NO_PING));
        assert!(s.due_pings(t + 3 * S, 16).is_empty());
        assert!(s.unanswered(&1, t + 3 * S));
        // A reader's timeout does not cancel a ping a link asked for.
        assert!(s.timed_out(3, (), t, t + 2 * S, 1));
        assert!(!s.timed_out(3, (), t, t + 2 * S, NO_PING));
        assert_eq!(s.due_pings(t + 2 * S, 16), vec![(3, ())]);
        assert!(!s.silent(&1, t + 3 * S));
        // Any answer clears it, and so does time.
        assert!(!s.unanswered(&1, t + 4 * S + SILENCE_TTL));
        s.answered(&1, t + 4 * S);
        assert!(!s.unanswered(&1, t + 4 * S));
        // An answered ping too.
        assert!(s.timed_out(2, (), t, t + 2 * S, 1));
        s.start_ping(&2, t + 2 * S);
        assert!(s.unanswered(&2, t + 2 * S));
        s.ping_result(&2, Some(true), t + 3 * S);
        assert!(!s.unanswered(&2, t + 3 * S));
    }

    #[test]
    fn short_exchanges_are_not_evidence_and_lost_pings_are_replaced() {
        let t = Instant::now();
        let mut s = silence();
        for i in 0..10 {
            assert!(!s.timed_out(2, (), t + i * S, t + i * S + S / 4, 1));
        }
        assert!(s.is_empty());
        // A ping whose outcome never came back is replaced after twice its time bound.
        assert!(s.timed_out(3, (), t, t + S, 1));
        s.start_ping(&3, t + S);
        assert!(!s.timed_out(3, (), t, t + 4 * S, 1));
        assert!(s.timed_out(3, (), t, t + 5 * S, 1));
    }

    #[test]
    fn capacity_evicts_the_oldest_and_retain_drops_former_neighbors() {
        let t = Instant::now();
        let mut s = silence();
        for i in 0..10u8 {
            s.timed_out(i, (), t, t + S + u32::from(i) * S, 1);
            assert!(s.len() <= 4);
        }
        assert_eq!(s.len(), 4);
        assert!(s.entries.contains_key(&9));
        assert!(!s.entries.contains_key(&0));
        assert_eq!(s.due_pings(t + 20 * S, 3), vec![(6, ()), (7, ()), (8, ())]);
        assert_eq!(s.due_pings(t + 20 * S, 3), vec![(9, ())]);
        s.retain(|n| *n % 2 == 0);
        assert!(s.entries.keys().all(|n| n % 2 == 0));
    }
}

// Rank 0 watch: what an owner above rank 0 last learned about rank 0's acquisition of a page.

/// Default bound on watched pages per node.
pub const WATCH_CAPACITY: usize = 4096;

/// Half the dataplane's default client request timeout.
pub const HANG_AFTER: Duration = Duration::from_secs(15);

/// How long an entry outlives the last timeout that renewed it.
pub const WATCH_TTL: Duration = Duration::from_secs(60);

/// What this owner knows about rank 0's acquisition of a page.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Rank0State {
    #[default]
    Unknown,
    /// A probe timed out recently: rank 0 is presumed to be fetching.
    Fetching,
    /// Rank 0 is presumed hung.
    Overdue,
    /// This owner's own origin fetch failed recently: leave retries to rank 0.
    Failed,
}

struct Rank0Entry<N> {
    node: N,
    until: Instant,
    last: Instant,
    failed: bool,
}

/// Bounded, expiring map from a page to rank 0's presumed state.
pub struct Rank0Watch<K, N> {
    capacity: usize,
    ttl: Duration,
    hang_after: Duration,
    entries: HashMap<K, Rank0Entry<N>>,
    /// Ranks 0 silent past a deadline, with last confirmation.
    silent: HashMap<N, Instant>,
    /// Insertion order for eviction; stale stamps are skipped.
    order: VecDeque<(K, Instant)>,
}

impl<K: Eq + Hash + Clone, N: Eq + Hash + Clone> Rank0Watch<K, N> {
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
                    } else if silent {
                        Rank0State::Overdue
                    } else {
                        Rank0State::Unknown
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

    /// A probe for `key` timed out. Repeated timeouts cannot postpone [`Rank0State::Overdue`].
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
                Rank0Entry {
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

    /// This owner's own origin fetch for `key` failed. Failures inside the
    /// interval do not extend it.
    pub fn failed(&mut self, key: K, rank0: N, now: Instant) {
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
                Rank0Entry {
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
    fn insert(&mut self, key: K, entry: Rank0Entry<N>) {
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

    pub fn answered(&mut self, key: &K, rank0: &N) {
        self.entries.remove(key);
        self.silent.remove(rank0);
    }

    /// Rank 0 answered `Unavailable`: forget a timeout but keep this
    /// owner's own recent origin failure.
    pub fn answered_failure(&mut self, key: &K, rank0: &N) {
        if self.entries.get(key).is_some_and(|e| !e.failed) {
            self.entries.remove(key);
        }
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
mod rank0_tests {
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
    fn own_origin_failures_hold_the_page_for_hang_after() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, WATCH_TTL, 10 * S);
        w.failed(1, A, now);
        assert_eq!(w.state(&1, &A, now), Rank0State::Failed);
        assert_eq!(w.state(&1, &8, now), Rank0State::Unknown);
        assert_eq!(w.state(&2, &A, now), Rank0State::Unknown);
        // Further failures inside the interval do not extend it.
        w.failed(1, A, now + 9 * S);
        assert_eq!(w.state(&1, &A, now + 9 * S), Rank0State::Failed);
        assert_eq!(w.state(&1, &A, now + 10 * S), Rank0State::Unknown);
        // A failure after it starts the next interval.
        w.failed(1, A, now + 11 * S);
        assert_eq!(w.state(&1, &A, now + 11 * S), Rank0State::Failed);
        assert_eq!(w.state(&1, &A, now + 21 * S), Rank0State::Unknown);
        // Rank 0's failure answers keep it; any other answer forgets it.
        w.answered_failure(&1, &A);
        assert_eq!(w.state(&1, &A, now + 12 * S), Rank0State::Failed);
        w.answered(&1, &A);
        assert_eq!(w.state(&1, &A, now + 12 * S), Rank0State::Unknown);
        w.failed(3, A, now);
        assert_eq!(
            w.state(&3, &A, now + 11 * S + WATCH_TTL),
            Rank0State::Unknown
        );
    }

    #[test]
    fn failures_and_timeouts_replace_each_other() {
        let now = Instant::now();
        let mut w = Rank0Watch::new(8, WATCH_TTL, 5 * S);
        w.timed_out(1, A, now, now + S);
        w.failed(1, A, now + S);
        assert_eq!(w.state(&1, &A, now + 2 * S), Rank0State::Failed);
        // A timeout after a failure starts a fresh timeout entry.
        w.timed_out(1, A, now + 3 * S, now + 4 * S);
        assert_eq!(w.state(&1, &A, now + 3 * S), Rank0State::Fetching);
        // Rank 0 answering a failure forgets a timeout entry and silence.
        w.answered_failure(&1, &A);
        assert_eq!(w.state(&1, &A, now + 3 * S), Rank0State::Unknown);
        let mut w = Rank0Watch::new(8, WATCH_TTL, Duration::ZERO);
        w.timed_out(1, A, now, now + S);
        w.timed_out(1, A, now + 2 * S, now + 3 * S);
        assert_eq!(w.state(&2, &A, now + 2 * S), Rank0State::Overdue);
        w.answered_failure(&9, &A);
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
