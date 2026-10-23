//! Seat server admission, lifted from `server.rs`.
//!
//! A seat admits at most [`limits::MAX_WRITES`] concurrent responses with a
//! body and [`limits::MAX_BOOTSTRAPS`] concurrent enrollments or bearer
//! authorizations; beyond that it answers 429 (`Overloaded`). Each Node may
//! hold one snapshot and one keyring long poll per seat. A cursor ahead of
//! what the seat serves answers 503 (`Unavailable`). A snapshot reply is a
//! delta when the client names the exact base, else the full image.

use crate::limits;
use racer_control_wire::ProtocolFailure;
use std::collections::HashSet;

/// A counting gate without queuing, the pure core of `exec::Slots`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Gate {
    pub used: usize,
    pub capacity: usize,
}

impl Gate {
    pub const fn new(capacity: usize) -> Self {
        Self { used: 0, capacity }
    }

    /// Take a slot. Fails when full.
    pub fn try_take(&mut self) -> bool {
        match take(self.used, self.capacity) {
            Some(n) => {
                self.used = n;
                true
            }
            None => false,
        }
    }

    pub fn put(&mut self) {
        self.used = self.used.saturating_sub(1);
    }
}

/// `used + 1` when below `capacity`.
pub fn take(used: usize, capacity: usize) -> Option<usize> {
    (used < capacity).then_some(used + 1)
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

/// Whether `current` is newer than the client's cursor.
pub fn newer(after: Option<u64>, current: u64) -> bool {
    after.is_none_or(|a| current > a)
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

/// Admission state of one seat server, without sockets. The simulator uses
/// it to model 429s and long poll occupancy; production keeps the same
/// counters in `server::Server` (`Slots` and poll sets).
pub struct SeatAdmission {
    pub writes: Gate,
    pub bootstraps: Gate,
    polls: HashSet<u32>,
    keyring_polls: HashSet<u32>,
}

impl Default for SeatAdmission {
    fn default() -> Self {
        Self::new(limits::MAX_WRITES, limits::MAX_BOOTSTRAPS)
    }
}

impl SeatAdmission {
    pub fn new(writes: usize, bootstraps: usize) -> Self {
        Self {
            writes: Gate::new(writes),
            bootstraps: Gate::new(bootstraps),
            polls: HashSet::new(),
            keyring_polls: HashSet::new(),
        }
    }

    /// Start a snapshot long poll for `node` against `current` (`None` when
    /// the seat is not ready). On `Ok`, the poll is held until
    /// [`SeatAdmission::end_poll`].
    pub fn snapshot(
        &mut self,
        node: u32,
        after: Option<u64>,
        current: Option<u64>,
    ) -> Result<Poll, ProtocolFailure> {
        Self::poll(&mut self.polls, node, after, current)
    }

    /// Start a keyring long poll, like [`SeatAdmission::snapshot`].
    pub fn keyring(
        &mut self,
        node: u32,
        after: Option<u64>,
        generation: Option<u64>,
    ) -> Result<Poll, ProtocolFailure> {
        Self::poll(&mut self.keyring_polls, node, after, generation)
    }

    fn poll(
        set: &mut HashSet<u32>,
        node: u32,
        after: Option<u64>,
        current: Option<u64>,
    ) -> Result<Poll, ProtocolFailure> {
        if !set.insert(node) {
            return Err(ProtocolFailure::Overloaded);
        }
        let result = match current {
            None => Err(ProtocolFailure::Unavailable),
            Some(c) => check_ahead(after, c).map(|()| {
                if newer(after, c) {
                    Poll::Reply
                } else {
                    Poll::Wait
                }
            }),
        };
        if result.is_err() {
            set.remove(&node);
        }
        result
    }

    /// The poll of `node` finished (reply written or 204).
    pub fn end_poll(&mut self, node: u32, keyring: bool) {
        if keyring {
            self.keyring_polls.remove(&node);
        } else {
            self.polls.remove(&node);
        }
    }

    /// Concurrent snapshot and keyring polls held.
    pub fn polls(&self) -> (usize, usize) {
        (self.polls.len(), self.keyring_polls.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gates_bound_and_release() {
        let mut g = Gate::new(2);
        assert!(g.try_take() && g.try_take());
        assert!(!g.try_take());
        g.put();
        assert!(g.try_take());
        assert_eq!(take(63, 64), Some(64));
        assert_eq!(take(64, 64), None);
        let a = SeatAdmission::default();
        assert_eq!(a.writes.capacity, 64);
        assert_eq!(a.bootstraps.capacity, 32);
    }

    #[test]
    fn cursors() {
        assert_eq!(check_ahead(None, 5), Ok(()));
        assert_eq!(check_ahead(Some(5), 5), Ok(()));
        assert_eq!(check_ahead(Some(6), 5), Err(ProtocolFailure::Unavailable));
        assert!(newer(None, 1) && newer(Some(1), 2) && !newer(Some(2), 2));
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
        let mut a = SeatAdmission::default();
        assert_eq!(a.snapshot(1, Some(5), Some(5)), Ok(Poll::Wait));
        assert_eq!(
            a.snapshot(1, Some(5), Some(5)),
            Err(ProtocolFailure::Overloaded)
        );
        // Keyring polls are separate.
        assert_eq!(a.keyring(1, None, Some(2)), Ok(Poll::Reply));
        assert_eq!(a.polls(), (1, 1));
        a.end_poll(1, false);
        assert_eq!(a.snapshot(1, Some(4), Some(5)), Ok(Poll::Reply));
        // Errors do not hold the poll.
        assert_eq!(
            a.snapshot(2, Some(9), Some(5)),
            Err(ProtocolFailure::Unavailable)
        );
        assert_eq!(a.snapshot(3, None, None), Err(ProtocolFailure::Unavailable));
        assert_eq!(a.polls(), (1, 1));
    }
}
