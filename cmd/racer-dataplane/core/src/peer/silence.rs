//! Peer silence: a neighbor that accepts requests and never answers them
//! (Known issues 41 and 43).
//!
//! A peer server can hang without failing its connections: requests are
//! accepted and never answered or forwarded. Transport circuits never see a
//! failure, so without more evidence every node keeps sending to it, and
//! each request costs its sender a whole attempt. Owners above a hung rank 0
//! learn it page by page ([`crate::read::rank0::Rank0Watch`]), each on its own
//! (Known issue 43), and readers never learn it (Known issue 41).
//!
//! A timeout alone cannot tell a hung peer from a busy one: a request that
//! joined a running acquisition, or queued behind origin, also ends
//! unanswered within the attempt. Taking every timeout as evidence (the
//! first version of this rule, two timeouts and half a request timeout
//! without an answer) marked thousands of busy owners in the simulator's
//! 5k `zipf` at 16 MiB pages and quintupled overlapping origin requests. So a
//! timeout only raises a question, and a ping answers it: a page request in
//! mode `ping`, which the receiver answers `Miss` at once without looking at
//! its cache or flights, so it never waits on work. A peer that answers the
//! ping (anything, even `Overloaded`) is alive; one that leaves the ping
//! unanswered within its own time bound is silent.
//!
//! The node on the other end of the last link to a destination is the one
//! that asks: every request for the destination reaches it over one of the
//! destination's links, sent either by the requester itself or by the relay
//! before it, so that sender can ping it over one link without blaming a
//! relay. It notes, per next hop that is also the destination, the exchanges
//! that ended unanswered (the attempt's time bound or the upstream's
//! cancellation, after at least [`Silence::min_wait`]), sends at most one
//! ping at a time per peer ([`Silence::timed_out`] says when), and forgets
//! the peer on any answer from it.
//!
//! A node that holds a peer silent fails requests for that destination at
//! once (`Unavailable`, signed when it is a relay) instead of sending them,
//! and routes around the peer. Because every request for the destination
//! crosses one of its links, the knowledge is shared: once the neighbors on
//! the common routes have it, every requester's probe fails fast, which a
//! reader treats as an unreachable owner (it moves on at once) and an owner
//! above rank 0 as a transport failure (origin authority) without its own
//! wait. A hung relay is routed around by the nodes that hold it silent.
//!
//! A silent peer stays silent until it answers anything; it is pinged again
//! every [`Silence::hold`], so one that recovers is used again within `hold`
//! plus a round trip, and one that stays hung costs one ping per `hold`. A
//! peer that answered a ping is not pinged again for a ping timeout. Entries
//! expire [`SILENCE_TTL`] after the last event and are bounded by `capacity`
//! (the sender's neighbors and direct peers); the oldest entry is evicted.
//! No reader's budget pays for a ping, and the rule only fails, earlier, a
//! request that would have timed out.

use std::collections::HashMap;
use std::hash::Hash;
use std::time::{Duration, Instant};

/// Default bound on tracked peers per sender (neighbors and direct peers).
pub const SILENCE_CAPACITY: usize = 256;

/// How long an entry outlives its last event.
pub const SILENCE_TTL: Duration = Duration::from_secs(60);

/// Unanswered exchanges over the last link before its sender pings: two, so
/// that a busy neighbor, which answers something between two timeouts, is
/// rarely pinged.
pub const LINK_TIMEOUTS: u32 = 2;

/// Unanswered probes of rank 0 before an owner above it pings rank 0: one,
/// since the owner's walk withholds origin after it ([`crate::read::rank0`]).
pub const RANK0_TIMEOUTS: u32 = 1;

#[derive(Clone, Debug)]
struct Entry<P> {
    /// What the last unanswered exchange asked for (a ping asks about it:
    /// production pings carry a page the destination ignores).
    subject: P,
    /// A timeout raised the question: ping at the next chance.
    want: bool,
    /// Exchanges left unanswered since the peer last answered.
    timeouts: u32,
    /// A ping is outstanding since then.
    pinging: Option<Instant>,
    /// The peer left a ping unanswered and has answered nothing since.
    silent: bool,
    /// When a silent peer is pinged again, or until when a peer that
    /// answered a ping is not pinged again.
    next: Instant,
    /// Last event (expiry and eviction stamp).
    last: Instant,
    /// Insertion order, to evict deterministically among equal stamps.
    seq: u64,
}

/// Bounded, expiring map from a destination to its silence. `P` is what a
/// ping for it carries.
pub struct Silence<N, P = ()> {
    capacity: usize,
    hang_after: Duration,
    ping_timeout: Duration,
    entries: HashMap<N, Entry<P>>,
    seq: u64,
}

impl<N: Eq + Hash + Clone, P: Clone> Silence<N, P> {
    /// `hang_after` is half the client request timeout (it scales
    /// [`Self::min_wait`] and [`Self::hold`]); `ping_timeout` bounds one
    /// ping (the peer attempt timeout).
    pub fn new(capacity: usize, hang_after: Duration, ping_timeout: Duration) -> Self {
        Self {
            capacity: capacity.max(1),
            hang_after,
            ping_timeout,
            entries: HashMap::new(),
            seq: 0,
        }
    }

    /// Exchanges that ended sooner than this after their send are not
    /// evidence (a hedge loser or a canceled read): a tenth of `hang_after`.
    pub fn min_wait(&self) -> Duration {
        self.hang_after / 10
    }

    /// How often a silent peer is pinged again: twice `hang_after`.
    pub fn hold(&self) -> Duration {
        self.hang_after.saturating_mul(2)
    }

    /// How long a ping may stay unanswered.
    pub fn ping_timeout(&self) -> Duration {
        self.ping_timeout
    }

    /// How long a peer that answered a ping is not pinged again:
    /// `hang_after`, the time [`crate::read::rank0::Rank0Watch`] gives a
    /// rank 0 that may still be fetching. A peer that hangs right after
    /// answering is found at most this much later; a busy peer costs at most
    /// one ping per sender per `quiet`.
    pub fn quiet(&self) -> Duration {
        self.hang_after
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// An exchange sent to `peer` (the destination, over the last link) at
    /// `sent` ended at `now` without an answer: the attempt's time bound or
    /// the upstream's cancellation. Once `needed` such exchanges went
    /// unanswered since the peer last answered, a ping is wanted. Returns
    /// whether a ping is wanted now;
    /// [`Self::due_pings`] or [`Self::start_ping`] sends it, and the caller
    /// then reports [`Self::ping_answered`] or [`Self::ping_failed`].
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
            entry.want = !entry.silent && !pinging && now >= entry.next && entry.timeouts >= needed;
            return entry.want;
        }
        self.make_room(now);
        self.entries.insert(
            peer,
            Entry {
                subject,
                want: needed <= 1,
                timeouts: 1,
                pinging: None,
                silent: false,
                next: now,
                last: now,
                seq: self.seq,
            },
        );
        self.seq += 1;
        needed <= 1
    }

    /// Send the ping a timeout asked for now (see [`Self::timed_out`]).
    pub fn start_ping(&mut self, peer: &N, now: Instant) {
        if let Some(entry) = self.entries.get_mut(peer) {
            entry.want = false;
            entry.pinging = Some(now);
        }
    }

    /// Peers to ping now: those a timeout asked about, and silent peers
    /// whose [`Self::hold`] has passed (to learn that they recovered), at
    /// most `max`, oldest first. Each is marked as pinging; at most one ping
    /// per peer is outstanding. Peers past `max` stay due.
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

    /// `peer` left the ping sent for it unanswered within
    /// [`Self::ping_timeout`]: it is silent until it answers anything, and
    /// pinged again every [`Self::hold`]. Returns whether it became silent.
    pub fn ping_failed(&mut self, peer: &N, now: Instant) -> bool {
        let hold = self.hold();
        let Some(entry) = self.entries.get_mut(peer) else {
            return false;
        };
        if entry.pinging.take().is_none() {
            return false;
        }
        let became = !entry.silent;
        entry.silent = true;
        entry.want = false;
        entry.next = now + hold;
        entry.last = entry.last.max(now);
        became
    }

    /// `peer` answered a ping: it is alive, and is not pinged again for
    /// [`Self::quiet`] whatever times out meanwhile (it is busy, not hung).
    pub fn ping_answered(&mut self, peer: &N, now: Instant) {
        let quiet = self.quiet();
        if let Some(entry) = self.entries.get_mut(peer) {
            entry.want = false;
            entry.timeouts = 0;
            entry.pinging = None;
            entry.silent = false;
            entry.next = now + quiet;
            entry.last = entry.last.max(now);
        }
    }

    /// `peer` answered an exchange (a response it signed or relayed): it is
    /// not hung. An outstanding ping's outcome is then ignored.
    pub fn answered(&mut self, peer: &N) {
        self.entries.remove(peer);
    }

    /// Whether `peer` is silent at `now`: requests for it fail at once.
    pub fn silent(&self, peer: &N, now: Instant) -> bool {
        self.entries.get(peer).is_some_and(|e| holds(e, now))
    }

    /// Peers silent at `now` (routing avoids their links).
    pub fn silent_peers(&self, now: Instant) -> impl Iterator<Item = &N> {
        self.entries
            .iter()
            .filter(move |(_, e)| holds(e, now))
            .map(|(n, _)| n)
    }

    /// Keep only peers `keep` accepts (the current neighbors and direct
    /// peers).
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

fn holds<P>(e: &Entry<P>, now: Instant) -> bool {
    e.silent && now.saturating_duration_since(e.last) <= SILENCE_TTL
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod tests {
    use super::*;

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
        assert!(s.ping_failed(&1, t + 4 * S));
        assert!(s.silent(&1, t + 4 * S));
        assert!(s.silent_peers(t + 4 * S).eq([&1]));
        assert!(!s.silent(&2, t + 4 * S));
        // A second report of the same ping changes nothing.
        assert!(!s.ping_failed(&1, t + 5 * S));
        assert!(s.silent(&1, t + 30 * S));
    }

    #[test]
    fn a_silent_peer_is_pinged_every_hold_and_cleared_by_an_answer() {
        let t = Instant::now();
        let mut s = silence();
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        s.start_ping(&1, t + 2 * S);
        assert!(s.ping_failed(&1, t + 4 * S));
        // While silent, late timeouts ask nothing.
        assert!(!s.timed_out(1, (), t + 3 * S, t + 5 * S, 1));
        assert!(s.due_pings(t + 13 * S, 16).is_empty());
        assert_eq!(s.due_pings(t + 14 * S, 16), vec![(1, ())]);
        // Still hung: silent, renewed for another hold.
        assert!(!s.ping_failed(&1, t + 16 * S));
        assert!(s.silent(&1, t + 16 * S + SILENCE_TTL));
        assert_eq!(s.due_pings(t + 26 * S, 16), vec![(1, ())]);
        // Recovered: the ping is answered.
        s.ping_answered(&1, t + 26 * S);
        assert!(!s.silent(&1, t + 26 * S));
        // Not pinged again for `quiet`, whatever times out.
        assert_eq!(s.quiet(), 5 * S);
        assert!(!s.timed_out(1, (), t + 26 * S, t + 30 * S, 1));
        assert!(s.due_pings(t + 30 * S, 16).is_empty());
        assert!(s.timed_out(1, (), t + 26 * S, t + 31 * S, 1));
        // A silent verdict expires SILENCE_TTL after its last event.
        s.start_ping(&1, t + 31 * S);
        assert!(s.ping_failed(&1, t + 33 * S));
        assert!(!s.silent(&1, t + 34 * S + SILENCE_TTL));
    }

    #[test]
    fn any_answer_clears_and_voids_an_outstanding_ping() {
        let t = Instant::now();
        let mut s = silence();
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        s.answered(&1);
        assert!(s.is_empty());
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        s.start_ping(&1, t + 2 * S);
        s.answered(&1);
        // The peer answered meanwhile: a lost ping proves nothing.
        assert!(!s.ping_failed(&1, t + 4 * S));
        assert!(!s.silent(&1, t + 4 * S));
        assert!(s.timed_out(1, (), t + 3 * S, t + 5 * S, 1));
        s.start_ping(&1, t + 5 * S);
        assert!(s.ping_failed(&1, t + 7 * S));
        s.answered(&1);
        assert!(!s.silent(&1, t + 7 * S));
        assert!(s.is_empty());
    }

    #[test]
    fn a_link_pings_after_two_timeouts_without_an_answer() {
        let t = Instant::now();
        let mut s = silence();
        assert!(!s.timed_out(1, (), t, t + S, LINK_TIMEOUTS));
        assert!(s.due_pings(t + S, 16).is_empty());
        // Any answer in between starts the count again.
        s.answered(&1);
        assert!(!s.timed_out(1, (), t, t + 2 * S, LINK_TIMEOUTS));
        assert!(s.timed_out(1, (), t, t + 3 * S, LINK_TIMEOUTS));
        assert_eq!(s.due_pings(t + 3 * S, 16), vec![(1, ())]);
        // An answered ping also starts it again (after the quiet time).
        s.ping_answered(&1, t + 3 * S);
        assert!(!s.timed_out(1, (), t + 8 * S, t + 9 * S, LINK_TIMEOUTS));
        assert!(s.timed_out(1, (), t + 8 * S, t + 10 * S, LINK_TIMEOUTS));
        assert_eq!(RANK0_TIMEOUTS, 1);
    }

    #[test]
    fn short_exchanges_are_not_evidence_and_lost_pings_are_replaced() {
        let t = Instant::now();
        let mut s = silence();
        for i in 0..10 {
            assert!(!s.timed_out(2, (), t + i * S, t + i * S + S / 4, 1));
        }
        assert!(s.is_empty());
        // A ping whose outcome never came back is replaced after twice its
        // time bound.
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
