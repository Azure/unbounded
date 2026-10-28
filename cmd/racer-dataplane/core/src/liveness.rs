//! Peer silence: a neighbor that accepts requests and never answers them
//! (Known issues 41 and 43).
//!
//! A peer server can hang without failing its connections: requests are
//! accepted and never answered or forwarded. Transport circuits never see a
//! failure, so without more evidence every node keeps sending to it, and
//! each request costs its sender a whole attempt. Owners above a hung rank 0
//! learn it page by page ([`crate::liveness::Rank0Watch`]), each on its own
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
//!
//! Busy is not hung (Known issues 53 and 54). Any answer from a peer quiets
//! it for [`Silence::quiet`], as an answered ping does, so a busy peer that
//! answers some exchanges and leaves others (that joined its running work)
//! unanswered is pinged at most once per sender per `quiet`. A peer that
//! itself sheds an exchange (a signed `Overloaded`, such as a hot page's
//! owner) is busy: [`Silence::shed`] marks it so readers visit it after the
//! page's other owners for `quiet`, without a ping.

use std::collections::{HashMap, VecDeque};
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

/// Unanswered probes of a page's rank 0 before the prober pings rank 0:
/// one. An owner above rank 0 withholds origin after it
/// ([`crate::liveness`]); a reader would otherwise visit a hung rank 0
/// again on its next try (waiting for a second timeout failed ten times as
/// many reads with 50 of 5,000 members hung in the simulator).
pub const RANK0_TIMEOUTS: u32 = 1;

/// A reader's unanswered probe of an owner above rank 0 never asks a ping:
/// such an owner usually waited on rank 0. It only demotes the owner in this
/// node's visit order ([`Silence::unanswered`]).
pub const NO_PING: u32 = u32::MAX;

#[derive(Clone, Debug)]
struct SilenceEntry<P> {
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
    /// The peer itself shed one of this node's requests (a signed
    /// `Overloaded`): it is busy, not hung, and readers visit it after the
    /// page's other owners until then (Known issue 54).
    busy_until: Option<Instant>,
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
    entries: HashMap<N, SilenceEntry<P>>,
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
    /// `hang_after`, the time [`crate::liveness::Rank0Watch`] gives a
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

    /// `peer` itself shed an exchange of this node at `now` (an
    /// `Overloaded` it signed, such as a hot page's owner shedding a
    /// reader): it answered, so it is alive ([`Self::answered`]), and busy.
    /// Readers visit it after the page's other owners for [`Self::quiet`]
    /// ([`Self::unanswered`]), unless it answers something else first (Known
    /// issue 54): a read retried after a shed otherwise repeated the same
    /// walk (`Spread` is deterministic per requester and page) and was shed
    /// again. No ping: the shed is the answer.
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

    /// `peer` answered an exchange at `now` (a response it signed or
    /// relayed): it is not hung. An outstanding ping's outcome is then
    /// ignored, and the peer is not pinged for [`Self::quiet`] after the
    /// answer, as after an answered ping (Known issue 53): a busy peer
    /// answers this node between the probes that join its running work and
    /// time out, and each answer used to forget the peer, so the next such
    /// timeout pinged it again. A peer that hangs right after answering is
    /// found at most `quiet` later, the bound an answered ping already has.
    /// Only a peer already tracked is kept (no entry per answer).
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

    /// Whether `peer` left an exchange of this node unanswered and has
    /// answered nothing since (or is silent), within [`SILENCE_TTL`], or
    /// shed this node within [`Self::quiet`] ([`Self::shed`]): readers visit
    /// it after the page's other owners (Known issues 41 and 54).
    pub fn unanswered(&self, peer: &N, now: Instant) -> bool {
        self.entries.get(peer).is_some_and(|e| {
            ((e.silent || e.timeouts > 0) && now.saturating_duration_since(e.last) <= SILENCE_TTL)
                || e.busy_until.is_some_and(|until| now < until)
        })
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

fn holds<P>(e: &SilenceEntry<P>, now: Instant) -> bool {
    e.silent && now.saturating_duration_since(e.last) <= SILENCE_TTL
}

#[cfg(test)]
#[allow(clippy::disallowed_methods)] // Tests build instants to feed the API.
mod silence_tests {
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
        // An untracked peer's answer adds no entry.
        s.answered(&9, t);
        assert!(s.is_empty());
        assert!(s.timed_out(1, (), t, t + 2 * S, 1));
        s.start_ping(&1, t + 2 * S);
        s.answered(&1, t + 3 * S);
        // The peer answered meanwhile: a lost ping proves nothing.
        assert!(!s.ping_failed(&1, t + 4 * S));
        assert!(!s.silent(&1, t + 4 * S));
        assert!(!s.unanswered(&1, t + 4 * S));
        // Past the quiet time after the answer, a timeout asks again.
        assert!(s.timed_out(1, (), t + 6 * S, t + 8 * S, 1));
        s.start_ping(&1, t + 8 * S);
        assert!(s.ping_failed(&1, t + 10 * S));
        // An answer clears a silent peer.
        s.answered(&1, t + 10 * S);
        assert!(!s.silent(&1, t + 10 * S));
        assert!(s.silent_peers(t + 10 * S).next().is_none());
        assert!(s.due_pings(t + 30 * S, 16).is_empty());
    }

    #[test]
    fn a_peer_that_answered_recently_is_not_pinged_for_the_quiet_time() {
        // Known issue 53: a busy peer answers some exchanges and leaves
        // others (that joined its running work) unanswered; each answer
        // used to forget it, so every later timeout pinged it again.
        let t = Instant::now();
        let mut s = silence();
        assert!(s.timed_out(1, (), t, t + 2 * S, RANK0_TIMEOUTS));
        s.start_ping(&1, t + 2 * S);
        s.ping_answered(&1, t + 3 * S);
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
        assert!(s.ping_failed(&1, t + 14 * S));
        assert!(s.silent(&1, t + 14 * S));
        // An answer never shortens a quiet time already set.
        s.ping_answered(&1, t + 20 * S);
        s.answered(&1, t + 16 * S);
        assert!(!s.timed_out(1, (), t + 22 * S, t + 24 * S, RANK0_TIMEOUTS));
        assert!(s.timed_out(1, (), t + 23 * S, t + 25 * S, RANK0_TIMEOUTS));
    }

    #[test]
    fn a_peer_that_shed_this_node_is_busy_for_the_quiet_time() {
        // Known issue 54: an owner that signed `Overloaded` for this node is
        // alive and busy. Readers visit it after the other owners until the
        // quiet time passes or it answers something else; it is never
        // pinged or silent for it.
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
        s.ping_answered(&1, t + 22 * S);
        assert!(s.unanswered(&1, t + 22 * S));
        // A shed clears a silent verdict (the peer answered) and voids an
        // outstanding ping.
        assert!(s.ping_failed(&2, t + 23 * S));
        assert!(s.silent(&2, t + 23 * S));
        s.shed(2, (), t + 24 * S);
        assert!(!s.silent(&2, t + 24 * S));
        assert!(s.unanswered(&2, t + 24 * S));
        assert!(!s.ping_failed(&2, t + 25 * S));
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
        s.ping_answered(&1, t + 7 * S);
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
        s.ping_answered(&2, t + 3 * S);
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

// Rank 0 watch: what an owner above rank 0 last learned about rank 0's
// acquisition of a page, so that it does not wait forever on a rank 0 that
// hangs (Known issue 39).
//
// An owner above rank 0 asks rank 0 for the page before origin. When rank 0
// accepts the request but does not answer within the attempt, rank 0 is most
// likely still fetching (its origin request queued behind others), and the
// owner leaves the fetch to it ([`crate::read::walk::Outcome::TimedOut`],
// Known issue 31). Without memory, a rank 0 (or a relay on the route) that
// accepts requests and never answers, without failing the connection,
// blocks the page for as long as it stays a member: every owner walk ends
// without origin authority.
//
// The owner therefore notes a page when its probe of rank 0 timed out
// ([`Rank0Watch::timed_out`]). For [`Rank0Watch::hang_after`] (half the
// node's client request timeout) after that, or until the request's own deadline
// if later, the page is [`Rank0State::Fetching`] and the timeout rule
// holds. After it the page is [`Rank0State::Overdue`]: a further timeout
// gives authority like a transport failure, and the page stays overdue
// while timeouts keep coming. Any answer signed by rank 0
// ([`Rank0Watch::answered`]) forgets the page.
//
// `Fetching` does not withhold origin after other failures of rank 0. The
// simulator measured that variant (a route failure, `Overloaded` or shed
// from a rank 0 found fetching withheld origin like a timeout): in 5k
// `zipf` at 16 MiB pages it removed 71 to 80 of about 110 overlapping
// origin requests but failed 29 to 55 more reads (seeds 1 and 2). Nearly
// all of those `Overloaded` answers are signed by a relay on the route
// whose relay quota is full, not by rank 0, so they say nothing about rank
// 0, and an owner that does not fetch leaves its readers unserved (Known
// issue 40).
//
// Why a fixed allowance and not the request's deadline: rank 0's
// acquisition outlives the request that started it while other waiters
// join it (flight handover), and an owner's request often carries a short
// deadline (a reader's attempt share). In the simulator an owner's first
// deadline was about 3 s after the timeout while rank 0's origin request
// stayed queued for longer, and taking authority then overlapped it. Why
// half the request timeout: a read that met the first timeout still has
// time to retry once rank 0 is presumed hung. In the simulator (10 s read
// timeout) 5 s added no overlapping origin request in 5k `zipf` at 16 MiB
// pages (3 s added 44), and with 50 of 5,000 members hung it failed 136
// reads where 10 s failed 188 and no watch 371.
//
// A hung peer server hangs for every page, so a timeout on a page that is
// already overdue also marks rank 0 itself silent: its other pages are
// overdue at once, until it answers any probe from this owner or the mark
// expires. Without it each page rank 0 owns would fail its readers for
// another `hang_after`.
//
// Rank 0's own failures (Known issue 52). When rank 0 itself answers an
// owner's probe `Unavailable` ([`crate::read::walk::Outcome::Failed`]),
// its acquisition of the page failed, usually at origin, and the owner
// takes origin authority and asks the same origin right away; the reader's
// next retry then sends rank 0 to origin again, so during an origin error
// burst each reader retry cost two origin requests. The cause is not
// visible in the answer (rank 0 also answers `Unavailable` when it fails
// for reasons of its own, and then the owner's fetch is what serves the
// page). So the owner learns it from its own fetch: when its own origin
// request for the page fails after it took authority
// ([`Rank0Watch::failed`]), origin is failing for the page, and for
// [`Rank0Watch::hang_after`] after that the page is
// [`Rank0State::Failed`]: a further `Failed` answer from rank 0 leaves the
// retry to rank 0 (the walk withholds authority, as for a timeout). Past
// it the page is `Unknown` again, so the owner fetches once more after
// rank 0's next failure, and a failure of that fetch starts the next
// interval. While origin keeps failing a page, the owner adds at most one
// origin request per `hang_after` (plus the first) to rank 0's, which
// follow the reader retries; a failure local to rank 0 is never withheld
// unless the owner's own fetch failed too. In the simulator (5k
// `full-failover` with every origin request failing for 30 s twice) this
// cut origin requests 17,114 to 15,179.
//
// Costs: one entry per page an owner deferred to rank 0, bounded by
// [`WATCH_CAPACITY`] and expiring [`WATCH_TTL`] after the last timeout.
// No message is added and no budget grows: the rule only changes whether a
// walk that already ended ends with origin authority. A hung rank 0 blocks
// a page for at most `hang_after` after the first timeout; a rank 0
// that is down or partitioned fails at the transport level, which gives
// authority as before.

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
    /// is marked silent: rank 0 is presumed hung.
    Overdue,
    /// This owner's own origin request for the page failed less than
    /// [`Rank0Watch::hang_after`] ago ([`Rank0Watch::failed`]): leave the
    /// retry after a failure of rank 0 to rank 0.
    Failed,
}

struct Rank0Entry<N> {
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
    entries: HashMap<K, Rank0Entry<N>>,
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

    /// This owner's own origin request for `key`, made with authority
    /// while `rank0` was the page's rank 0, failed (origin error or
    /// deadline; Known issue 52). The first failure starts an interval of
    /// [`Rank0Watch::hang_after`] during which the page is
    /// [`Rank0State::Failed`]; further failures inside it do not extend it.
    /// A failure after it starts the next interval.
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

    /// `rank0` itself answered this owner's probe for `key` (a copy, a
    /// miss, or a failure of its own): it is not hung, forget the page and
    /// the silent mark.
    pub fn answered(&mut self, key: &K, rank0: &N) {
        self.entries.remove(key);
        self.silent.remove(rank0);
    }

    /// `rank0` itself answered this owner's probe for `key` that its own
    /// acquisition failed ([`crate::read::walk::Outcome::Failed`]): it is
    /// not hung (forget a timeout and the silent mark), but a recent origin
    /// failure of this owner's stays (Known issue 52).
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
