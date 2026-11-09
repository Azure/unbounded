//! The node's link setup state machine (`designs/racer-rdma-sessions.md`,
//! "Link setup over HTTP").
//!
//! One [`SetupTable`] per node, shared by every worker, holds each key's
//! setup state. It is pure: no owner calls, no clock reads, no I/O. Callers
//! drive it with the time and the owner's answers, and act on what each
//! transition returns.
//!
//! Initiator, per key:
//!
//! ```text
//!   Idle --begin--> Opening(token) --opened--> Exchanging(epoch) --finish--> Idle
//!                        |                          |
//!                        +--cancel--> Idle          +--accepted--> Idle
//!                        +--accepted--> Idle
//! ```
//!
//! `accepted` is the responder side winning: an authenticated offer from the
//! peer replaced whatever this node was setting up, so the initiator's later
//! transitions (`opened`, `finish`) find another state and change nothing.
//! Tokens and epochs make every transition apply only to the attempt that
//! started it.
//!
//! Responder: [`SetupTable::admit`] rate limits offers per key and applies
//! the tie-break against this node's own attempt. The responder then
//! accepts through the owner, which registers the link's expected identity
//! before the HTTP reply goes out, and calls [`SetupTable::accepted`].

use std::collections::HashMap;
use std::time::Duration;
use std::time::Instant;

use rdma_verbs::Epoch;
use rdma_verbs::LinkKey;

/// One key's setup on this node, if any.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SetupState {
    /// This node asked the owner to open a link; `token` names the attempt.
    Opening(u64),

    /// The owner opened `epoch`; the signed HTTP exchange is running.
    Exchanging(Epoch),
}

/// Result of checking an incoming setup before it reaches the owner.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SetupAdmission {
    /// Accept the offer.
    Accept,

    /// Our own concurrent offer wins: answer `409 Conflict`.
    Conflict,

    /// Rate limited or overloaded: answer `429 Too Many Requests`.
    Busy,
}

/// What [`SetupTable::opened`] found.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Opened {
    /// The attempt now owns the epoch.
    Exchanging,

    /// An accepted offer replaced the attempt while the owner opened it; the
    /// epoch is no longer tracked here.
    Superseded,
}

/// Fewest admitted entries that trigger a sweep of expired ones.
const MIN_PRUNE: usize = 64;

/// Every key's setup state, plus the responder rate limit.
#[derive(Debug, Default)]
pub(crate) struct SetupTable {
    states: HashMap<LinkKey, SetupState>,

    /// Last admitted incoming offer per key.
    admitted: HashMap<LinkKey, Instant>,

    /// Size of `admitted` that triggers the next sweep of expired entries:
    /// sweeping on every admission would scan the whole map each time.
    prune_at: usize,

    next_token: u64,
}

impl SetupTable {
    /// `key`'s state; `None` is idle.
    #[cfg(test)]
    pub(crate) fn state(&self, key: &LinkKey) -> Option<SetupState> {
        self.states.get(key).copied()
    }

    /// True when no setup from this node is running for `key`.
    pub(crate) fn idle(&self, key: &LinkKey) -> bool {
        !self.states.contains_key(key)
    }

    /// Initiator: start an attempt. `None` if one is running.
    pub(crate) fn begin(&mut self, key: LinkKey) -> Option<u64> {
        if self.states.contains_key(&key) {
            return None;
        }
        let token = self.next_token;
        self.next_token = self.next_token.wrapping_add(1);
        self.states.insert(key, SetupState::Opening(token));
        Some(token)
    }

    /// Initiator: the owner opened `epoch` for attempt `token`.
    pub(crate) fn opened(&mut self, key: LinkKey, token: u64, epoch: Epoch) -> Opened {
        match self.states.get_mut(&key) {
            Some(state) if *state == SetupState::Opening(token) => {
                *state = SetupState::Exchanging(epoch);
                Opened::Exchanging
            }
            _ => Opened::Superseded,
        }
    }

    /// Initiator: attempt `token` ended before the owner answered.
    pub(crate) fn cancel(&mut self, key: LinkKey, token: u64) {
        self.remove_if(key, SetupState::Opening(token));
    }

    /// Initiator: the attempt on `epoch` connected or gave up.
    pub(crate) fn finish(&mut self, key: LinkKey, epoch: Epoch) {
        self.remove_if(key, SetupState::Exchanging(epoch));
    }

    /// Responder: check an authenticated offer for `key` at `now`. `we_win`
    /// is the tie-break: true when this node's own concurrent offer wins.
    /// Admitting records `now` for the rate limit of one offer per key per
    /// `interval`.
    pub(crate) fn admit(
        &mut self,
        key: LinkKey,
        we_win: bool,
        now: Instant,
        interval: Duration,
    ) -> SetupAdmission {
        if we_win && self.states.contains_key(&key) {
            return SetupAdmission::Conflict;
        }
        if self
            .admitted
            .get(&key)
            .is_some_and(|last| now < *last + interval)
        {
            return SetupAdmission::Busy;
        }
        if self.admitted.len() >= self.prune_at {
            self.admitted.retain(|_, last| now < *last + interval);
            self.prune_at = (self.admitted.len() * 2).max(MIN_PRUNE);
        }
        self.admitted.insert(key, now);
        SetupAdmission::Accept
    }

    /// Responder: the owner accepted the peer's offer for `key`, replacing
    /// any attempt of ours, which lost the tie-break.
    pub(crate) fn accepted(&mut self, key: LinkKey) {
        self.states.remove(&key);
    }

    /// Setups running.
    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.states.len()
    }

    fn remove_if(&mut self, key: LinkKey, expected: SetupState) {
        if self.states.get(&key) == Some(&expected) {
            self.states.remove(&key);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: LinkKey = LinkKey {
        peer: [1; 32],
        rail: 7,
    };

    const OTHER: LinkKey = LinkKey {
        peer: [2; 32],
        rail: 7,
    };

    const SECOND: Duration = Duration::from_secs(1);

    #[test]
    /// The initiator path: one attempt per key, opened, then finished.
    fn initiator_runs_one_attempt_per_key() {
        let mut table = SetupTable::default();
        let token = table.begin(KEY).unwrap();
        assert_eq!(table.begin(KEY), None, "one attempt per key");
        assert!(table.begin(OTHER).is_some(), "keys are independent");
        assert!(!table.idle(&KEY));
        assert_eq!(table.opened(KEY, token, Epoch(4)), Opened::Exchanging);
        assert_eq!(table.state(&KEY), Some(SetupState::Exchanging(Epoch(4))));
        table.finish(KEY, Epoch(3));
        assert!(!table.idle(&KEY), "another epoch leaves it");
        table.finish(KEY, Epoch(4));
        assert!(table.idle(&KEY));
        assert_eq!(table.len(), 1);
    }

    #[test]
    /// Cancel clears only its own attempt, and only while opening.
    fn cancel_is_scoped_to_its_token() {
        let mut table = SetupTable::default();
        let first = table.begin(KEY).unwrap();
        table.cancel(KEY, first + 1);
        assert_eq!(table.state(&KEY), Some(SetupState::Opening(first)));
        table.cancel(KEY, first);
        assert!(table.idle(&KEY));
        let second = table.begin(KEY).unwrap();
        assert_ne!(first, second, "tokens are fresh");
        // A stale cancel after the attempt opened changes nothing.
        table.opened(KEY, second, Epoch(1));
        table.cancel(KEY, second);
        assert_eq!(table.state(&KEY), Some(SetupState::Exchanging(Epoch(1))));
    }

    #[test]
    /// An accepted offer supersedes our attempt at any stage; the attempt's
    /// later transitions do not resurrect or clear a newer one.
    fn accepted_offer_supersedes_our_attempt() {
        let mut table = SetupTable::default();
        let token = table.begin(KEY).unwrap();
        table.accepted(KEY);
        assert_eq!(table.opened(KEY, token, Epoch(2)), Opened::Superseded);
        assert!(table.idle(&KEY));
        let newer = table.begin(KEY).unwrap();
        table.finish(KEY, Epoch(2));
        table.cancel(KEY, token);
        assert_eq!(table.state(&KEY), Some(SetupState::Opening(newer)));
        table.opened(KEY, newer, Epoch(5));
        table.accepted(KEY);
        assert!(table.idle(&KEY));
    }

    #[test]
    /// The tie-break refuses an offer only while our own attempt runs and
    /// wins; the rate limit allows one admitted offer per key per interval.
    fn admit_applies_tie_break_then_rate_limit() {
        let mut table = SetupTable::default();
        let now = Instant::now();
        table.begin(KEY).unwrap();
        assert_eq!(
            table.admit(KEY, true, now, SECOND),
            SetupAdmission::Conflict
        );
        assert_eq!(table.admit(KEY, false, now, SECOND), SetupAdmission::Accept);
        assert_eq!(table.admit(KEY, false, now, SECOND), SetupAdmission::Busy);
        assert_eq!(
            table.admit(OTHER, true, now, SECOND),
            SetupAdmission::Accept,
            "no attempt of ours for this key"
        );
        assert_eq!(
            table.admit(KEY, false, now + SECOND, SECOND),
            SetupAdmission::Accept,
            "the interval passed"
        );
    }

    #[test]
    /// Expired admissions are swept once the map grows, so it stays bounded
    /// by recent offers without a scan on every admission.
    fn admitted_entries_are_swept_as_the_map_grows() {
        let mut table = SetupTable::default();
        let now = Instant::now();
        let key = |i: usize| LinkKey {
            peer: [1; 32],
            rail: i as u16,
        };
        for i in 0..MIN_PRUNE - 1 {
            assert_eq!(
                table.admit(key(i), false, now, SECOND),
                SetupAdmission::Accept
            );
        }
        // Below the threshold nothing is swept, even when expired.
        let later = now + 2 * SECOND;
        assert_eq!(
            table.admit(key(1000), false, later, SECOND),
            SetupAdmission::Accept
        );
        assert_eq!(table.admitted.len(), MIN_PRUNE);
        // At the threshold the expired ones go; the fresh ones stay.
        assert_eq!(
            table.admit(key(1001), false, later, SECOND),
            SetupAdmission::Accept
        );
        assert_eq!(table.admitted.len(), 2);
        assert_eq!(
            table.admit(key(1000), false, later, SECOND),
            SetupAdmission::Busy
        );
        // Thousands of keys over time stay bounded.
        for i in 0..10_000 {
            let at = later + SECOND * (i as u32 / 100);
            table.admit(key(2000 + i), false, at, SECOND);
        }
        assert!(table.admitted.len() <= 2 * MIN_PRUNE + 200);
    }
}
