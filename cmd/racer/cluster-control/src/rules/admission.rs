//! Seat server admission rules (`server.rs`).
//!
//! A seat admits at most [`crate::limits::MAX_WRITES`] concurrent responses with a
//! body and [`crate::limits::MAX_BOOTSTRAPS`] concurrent enrollments or bearer
//! authorizations; beyond that it answers 429 (`Overloaded`). Each Node may
//! hold one snapshot and one keyring long poll per seat. A cursor ahead of
//! what the seat serves answers 503 (`Unavailable`). A snapshot reply is a
//! delta when the client names the exact base, else the full image.

use racer_control_wire::ProtocolFailure;
use std::collections::{HashSet, VecDeque};

/// Outcome of [`WriteQueue::admit`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Admit {
    /// A slot was free and nobody was queued: the caller holds it now.
    Take,
    /// The caller was queued; a later [`WriteQueue::release`] hands it a slot.
    Wait,
    /// Slots and queue are both full: real overload, answer 429.
    Full,
}

/// A counting gate with a bounded FIFO of waiters, the pure core of
/// `exec::Slots`. A released slot passes straight to the oldest live waiter,
/// so the count does not drop and a newcomer cannot jump the queue.
///
/// Waiters that give up (deadline, disconnect) are not removed eagerly:
/// `live` filters them at [`WriteQueue::release`] and
/// [`WriteQueue::admit`] trims dead ones from the front. Dead entries still
/// count toward `max_waiting` until trimmed, so the bound on memory holds
/// even when every waiter gives up.
#[derive(Clone, Debug)]
pub struct WriteQueue<T> {
    /// Slots held.
    pub used: usize,
    pub capacity: usize,
    pub max_waiting: usize,
    waiting: VecDeque<T>,
}

impl<T> WriteQueue<T> {
    pub fn new(capacity: usize, max_waiting: usize) -> Self {
        Self {
            used: 0,
            capacity,
            max_waiting,
            waiting: VecDeque::new(),
        }
    }

    fn trim(&mut self, live: &mut impl FnMut(&T) -> bool) {
        while self.waiting.front().is_some_and(|w| !live(w)) {
            self.waiting.pop_front();
        }
    }

    /// Take a slot without queuing. Fails when full or when live waiters
    /// are queued (FIFO fairness).
    pub fn try_take(&mut self, mut live: impl FnMut(&T) -> bool) -> bool {
        self.trim(&mut live);
        let free = self.waiting.is_empty() && self.used < self.capacity;
        self.used += usize::from(free);
        free
    }

    /// Take a slot now, or queue `waiter` when the queue has room.
    pub fn admit(&mut self, waiter: T, live: impl FnMut(&T) -> bool) -> Admit {
        if self.try_take(live) {
            return Admit::Take;
        }
        if self.waiting.len() >= self.max_waiting {
            return Admit::Full;
        }
        self.waiting.push_back(waiter);
        Admit::Wait
    }

    /// Release a held slot. Returns the waiter that now holds it, if any;
    /// otherwise the gate count drops.
    pub fn release(&mut self, mut live: impl FnMut(&T) -> bool) -> Option<T> {
        while let Some(w) = self.waiting.pop_front() {
            if live(&w) {
                return Some(w);
            }
        }
        self.used = self.used.saturating_sub(1);
        None
    }

    /// Queued entries, including dead ones not yet trimmed.
    pub fn waiting(&self) -> usize {
        self.waiting.len()
    }

    /// Remove every queued waiter, oldest first (connections dropped).
    pub fn take_waiting(&mut self) -> Vec<T> {
        self.waiting.drain(..).collect()
    }
}

/// A cursor ahead of what this seat serves means this seat lags another
/// seat the client already read from. Like the Go controller, answer 503 so
/// the client retries or fails over instead of treating its cursor as bad.
pub fn check_ahead(after: Option<u64>, current: u64) -> Result<(), ProtocolFailure> {
    if after.is_some_and(|a| a > current) {
        return Err(ProtocolFailure::Unavailable);
    }
    Ok(())
}

/// Body a snapshot reply carries.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Body {
    Delta,
    Full,
}

/// Delta for the exact base, else the full image. Mirrors
/// `publication::Image::for_base`; `delta` is `(base sequence, base hash)`.
pub fn body_for(delta: Option<(u64, &str)>, after: Option<u64>, base_hash: Option<&str>) -> Body {
    match (delta, after, base_hash) {
        (Some((seq, hash)), Some(a), Some(h)) if seq == a && hash == h => Body::Delta,
        _ => Body::Full,
    }
}

/// Outcome of the first, non-blocking stage of a long poll.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Poll {
    /// Newer data is available: reply now (needs a write slot).
    Reply,
    /// Wait for a change or the poll deadline, then 204.
    Wait,
}

/// Start a long poll for `node` against `current` (`None` when the seat
/// has nothing to serve). One poll per node: a second answers 429. On `Ok`
/// the poll stays in `set` until the caller removes it; on `Err` it is not
/// held.
pub fn start_poll<K: Eq + std::hash::Hash + Clone>(
    set: &mut HashSet<K>,
    node: &K,
    after: Option<u64>,
    current: Option<u64>,
) -> Result<Poll, ProtocolFailure> {
    if !set.insert(node.clone()) {
        return Err(ProtocolFailure::Overloaded);
    }
    let result = match current {
        None => Err(ProtocolFailure::Unavailable),
        Some(c) => check_ahead(after, c).map(|()| {
            if after.is_none_or(|a| c > a) {
                Poll::Reply
            } else {
                Poll::Wait
            }
        }),
    };
    if result.is_err() {
        set.remove(node);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unqueued_gate_bounds_and_releases() {
        let all = |_: &u32| true;
        let mut g = WriteQueue::new(2, 0);
        assert!(g.try_take(all) && g.try_take(all));
        assert!(!g.try_take(all));
        assert_eq!(g.release(all), None);
        assert!(g.try_take(all));
        assert_eq!(g.used, 2);
    }

    #[test]
    fn write_queue_hands_slots_over_in_order() {
        let all = |_: &u32| true;
        let mut q = WriteQueue::new(2, 3);
        assert_eq!(q.admit(1, all), Admit::Take);
        assert_eq!(q.admit(2, all), Admit::Take);
        assert_eq!(q.admit(3, all), Admit::Wait);
        assert_eq!(q.admit(4, all), Admit::Wait);
        assert_eq!(q.admit(5, all), Admit::Wait);
        // Bounded: the fourth waiter is real overload.
        assert_eq!(q.admit(6, all), Admit::Full);
        // No queue jumping while live waiters exist.
        assert!(!q.try_take(all));
        // Handoff keeps the gate full and goes oldest first.
        assert_eq!(q.release(all), Some(3));
        assert_eq!(q.used, 2);
        // Dead waiters are skipped.
        assert_eq!(q.release(|w| *w != 4), Some(5));
        assert_eq!(q.waiting(), 0);
        assert_eq!(q.release(all), None);
        assert_eq!(q.used, 1);
        assert_eq!(q.admit(7, all), Admit::Take);
        assert_eq!(q.admit(8, all), Admit::Wait);
        assert_eq!(q.take_waiting(), vec![8]);
        assert_eq!(q.waiting(), 0);
        assert_eq!(q.release(all), None);
        assert!(q.try_take(all));
        assert!(!q.try_take(all));
    }

    #[test]
    fn write_queue_trims_dead_waiters() {
        let mut q = WriteQueue::new(1, 2);
        assert_eq!(q.admit(1, |_| true), Admit::Take);
        assert_eq!(q.admit(2, |_| true), Admit::Wait);
        assert_eq!(q.admit(3, |_| true), Admit::Wait);
        assert_eq!(q.admit(4, |_| true), Admit::Full);
        // Waiter 2 gave up: admit trims it and makes room.
        assert_eq!(q.admit(4, |w| *w != 2), Admit::Wait);
        // All gave up: release frees the gate, try_take then succeeds.
        assert_eq!(q.release(|_| false), None);
        assert_eq!(q.used, 0);
        assert!(q.try_take(|_| false));
        // A zero-length queue never waits.
        let mut z = WriteQueue::new(0, 0);
        assert_eq!(z.admit(1, |_| true), Admit::Full);
    }

    #[test]
    fn cursors() {
        assert_eq!(check_ahead(None, 5), Ok(()));
        assert_eq!(check_ahead(Some(5), 5), Ok(()));
        assert_eq!(check_ahead(Some(6), 5), Err(ProtocolFailure::Unavailable));
    }

    #[test]
    fn delta_only_for_exact_base() {
        let d = Some((4, "h4"));
        assert_eq!(body_for(d, Some(4), Some("h4")), Body::Delta);
        assert_eq!(body_for(d, Some(4), Some("x")), Body::Full);
        assert_eq!(body_for(d, Some(3), Some("h4")), Body::Full);
        assert_eq!(body_for(d, None, Some("h4")), Body::Full);
        assert_eq!(body_for(None, Some(4), Some("h4")), Body::Full);
    }

    #[test]
    fn one_poll_per_node() {
        let (mut polls, mut keyring) = (HashSet::new(), HashSet::new());
        assert_eq!(start_poll(&mut polls, &1, Some(5), Some(5)), Ok(Poll::Wait));
        assert_eq!(
            start_poll(&mut polls, &1, Some(5), Some(5)),
            Err(ProtocolFailure::Overloaded)
        );
        // Keyring polls are a separate set.
        assert_eq!(start_poll(&mut keyring, &1, None, Some(2)), Ok(Poll::Reply));
        polls.remove(&1);
        assert_eq!(
            start_poll(&mut polls, &1, Some(4), Some(5)),
            Ok(Poll::Reply)
        );
        // Errors do not hold the poll.
        assert_eq!(
            start_poll(&mut polls, &2, Some(9), Some(5)),
            Err(ProtocolFailure::Unavailable)
        );
        assert_eq!(
            start_poll(&mut polls, &3, None, None),
            Err(ProtocolFailure::Unavailable)
        );
        assert_eq!((polls.len(), keyring.len()), (1, 1));
    }
}
