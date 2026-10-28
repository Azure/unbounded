//! Seat renewal and release, lifted from `seat::seat_task`.

use crate::{authority::election, limits, shared::PrimaryView};
use std::time::{Duration, Instant};

/// Retry delay after a failed renew.
pub const RETRY: Duration = Duration::from_secs(2);

/// Earliest early PUT after the last one when only readiness changed.
pub const READY_MIN_INTERVAL: Duration = Duration::from_secs(1);

/// What the renew loop sees when it wakes.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Inputs {
    pub stopping: bool,
    /// Another live seat on this Node with a lower index.
    pub duplicate: bool,
    /// The serving identity is due, see `seat::serving_rotation_due`.
    pub rotation: bool,
    /// The seat is at or beyond the desired count and this Pod is not primary.
    pub shrink: bool,
    /// The READY annotation would change.
    pub ready_changed: bool,
}

impl Inputs {
    /// Fill `duplicate` and `shrink` from the primary view.
    pub fn from_view(
        view: &PrimaryView,
        index: usize,
        node: &str,
        is_primary: bool,
        stopping: bool,
        rotation: bool,
        ready_changed: bool,
    ) -> Self {
        let duplicate = view
            .seat_nodes
            .iter()
            .enumerate()
            .any(|(i, n)| i < index && n == node);
        let shrink =
            view.holder.is_some() && election::must_release(index, view.desired) && !is_primary;
        Self {
            stopping,
            duplicate,
            rotation,
            shrink,
            ready_changed,
        }
    }
}

/// Decision on wake.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Action {
    /// Release the Lease and stop.
    Release,
    /// Nothing due. Wait until [`SeatRenew::next`] or the next change.
    Wait,
    /// PUT a renewal now.
    Renew,
}

/// After a renew conflict and a re-read of the Lease.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Conflict {
    /// Still our term: retry immediately with the fresh object.
    Retry,
    /// Another term: the seat is lost.
    Lose,
    /// The re-read failed: retry after [`RETRY`].
    Later,
}

/// Renew schedule of one seat.
#[derive(Clone, Copy, Debug)]
pub struct SeatRenew {
    pub next: Instant,
    pub last_put: Instant,
}

impl SeatRenew {
    /// Schedule after a claim at `now`.
    pub fn new(now: Instant) -> Self {
        Self {
            next: now + limits::LEASE_RENEW,
            last_put: now,
        }
    }

    /// Decide on wake at `now`.
    pub fn decide(&mut self, now: Instant, i: Inputs) -> Action {
        if i.stopping || i.duplicate || i.shrink || i.rotation {
            return Action::Release;
        }
        if now < self.next {
            // Annotations refresh only at renew time, except readiness, which
            // the primary needs to publish us. That is written early, at
            // most once a second so failures cannot spin.
            if i.ready_changed {
                self.next = self.next.min(self.last_put + READY_MIN_INTERVAL);
            }
            if now < self.next {
                return Action::Wait;
            }
        }
        Action::Renew
    }

    /// A renew PUT starts at `started`.
    pub fn put_started(&mut self, started: Instant) {
        self.last_put = started;
    }

    /// The PUT that started at `started` succeeded.
    pub fn put_ok(&mut self, started: Instant) {
        self.next = started + limits::LEASE_RENEW;
    }

    /// The PUT failed or timed out, other than a conflict.
    pub fn put_failed(&mut self, now: Instant) {
        self.next = now + RETRY;
    }

    /// The PUT conflicted. `same_term` is the re-read result: `Some(true)`
    /// when the Lease is still our term, `Some(false)` for another term,
    /// `None` when the read failed.
    pub fn conflict(&mut self, same_term: Option<bool>, now: Instant) -> Conflict {
        match same_term {
            Some(true) => {
                self.next = now;
                Conflict::Retry
            }
            Some(false) => Conflict::Lose,
            None => {
                self.next = now + RETRY;
                Conflict::Later
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn release_reasons_win() {
        let t = Instant::now();
        for i in [
            Inputs {
                stopping: true,
                ..Default::default()
            },
            Inputs {
                duplicate: true,
                ..Default::default()
            },
            Inputs {
                rotation: true,
                ..Default::default()
            },
            Inputs {
                shrink: true,
                ..Default::default()
            },
        ] {
            assert_eq!(SeatRenew::new(t).decide(t, i), Action::Release);
        }
    }

    #[test]
    fn schedule_and_ready_early_write() {
        let t = Instant::now();
        let mut r = SeatRenew::new(t);
        assert_eq!(r.decide(t, Inputs::default()), Action::Wait);
        let ready = Inputs {
            ready_changed: true,
            ..Default::default()
        };
        // Within a second of the last PUT: wait, but pull next in.
        assert_eq!(
            r.decide(t + Duration::from_millis(500), ready),
            Action::Wait
        );
        assert_eq!(r.next, t + READY_MIN_INTERVAL);
        assert_eq!(r.decide(t + READY_MIN_INTERVAL, ready), Action::Renew);
        r.put_started(t + READY_MIN_INTERVAL);
        r.put_ok(t + READY_MIN_INTERVAL);
        assert_eq!(r.next, t + READY_MIN_INTERVAL + limits::LEASE_RENEW);
        assert_eq!(r.decide(r.next, Inputs::default()), Action::Renew);
    }

    #[test]
    fn failures_and_conflicts() {
        let t = Instant::now();
        let mut r = SeatRenew::new(t);
        r.put_failed(t);
        assert_eq!(r.next, t + RETRY);
        assert_eq!(r.conflict(Some(true), t), Conflict::Retry);
        assert_eq!(r.next, t);
        assert_eq!(r.conflict(None, t), Conflict::Later);
        assert_eq!(r.next, t + RETRY);
        assert_eq!(r.conflict(Some(false), t), Conflict::Lose);
    }

    #[test]
    fn view_derived_reasons() {
        let v = PrimaryView {
            holder: Some("p".into()),
            desired: 2,
            seat_nodes: vec!["n1".into(), "n2".into(), "n1".into()],
            ..Default::default()
        };
        let i = Inputs::from_view(&v, 2, "n1", false, false, false, false);
        assert!(i.duplicate && i.shrink);
        let i = Inputs::from_view(&v, 2, "n1", true, false, false, false);
        assert!(!i.shrink, "the primary keeps its seat");
        let i = Inputs::from_view(&v, 1, "n2", false, false, false, false);
        assert!(!i.duplicate && !i.shrink);
        let none = PrimaryView {
            holder: None,
            ..v.clone()
        };
        assert!(!Inputs::from_view(&none, 5, "x", false, false, false, false).shrink);
    }
}
