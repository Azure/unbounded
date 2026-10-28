//! Seat lottery, lifted from `seat::lottery`.
//!
//! A Pod without a seat waits a jittered delay inside the claim window, picks
//! a vacant seat, reads that one Lease, and claims it only when
//! [`election::seat_to_claim`] agrees. With a live primary, the window and
//! vacancies come from the primary Lease annotations; without one, the Pod
//! contends for the initial seats with an exponentially growing window.

use crate::{
    authority::election::{self, LeaseObservation, Observer, SeatView},
    entropy::Entropy,
    lease,
    shared::PrimaryView,
};
use std::{
    collections::BTreeMap,
    time::{Duration, Instant},
};

/// Longest cold start claim window, in seconds.
pub const MAX_COLD_WINDOW: u64 = 600;

/// What the lottery sees before one round.
#[derive(Clone, Copy, Debug)]
pub struct Inputs<'a> {
    pub holds_seat: bool,
    /// The cluster ID (state Secret UID) is known.
    pub cluster_known: bool,
    pub view: &'a PrimaryView,
    /// When the primary Lease resourceVersion last changed, by local clock.
    pub primary_changed_at: Option<Instant>,
    pub node: &'a str,
    pub pod_uid: &'a str,
    /// Seat count before any primary wrote one, see `seat::initial_seats`.
    pub initial_seats: usize,
}

/// Decision for one round.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Plan {
    /// Holding a seat or no cluster yet. Sleep and look again.
    Idle(Duration),
    /// No vacancy for this Node. Sleep and look again.
    NoVacancy(Duration),
    /// Sleep `delay`, then, if still seatless, read seat `index` and call
    /// [`Lottery::begin`] and [`Lottery::observe`].
    Attempt { delay: Duration, index: usize },
}

/// Lottery state of one Pod.
#[derive(Default)]
pub struct Lottery {
    attempt: u32,
    observer: Observer,
    seen: BTreeMap<usize, LeaseObservation>,
    desired: usize,
}

impl Lottery {
    /// Attempts since the last win or vacancy-free round.
    pub fn attempt(&self) -> u32 {
        self.attempt
    }

    /// Plan one round.
    pub fn plan(&mut self, i: &Inputs<'_>, now: Instant, rng: &mut dyn Entropy) -> Plan {
        if i.holds_seat || !i.cluster_known {
            return Plan::Idle(Duration::from_secs(2));
        }
        let live = i.view.live(i.primary_changed_at, now);
        let (candidates, window) = if live {
            if i.view.node_seated(i.node) {
                (Vec::new(), 0)
            } else {
                let v: Vec<usize> = (0..i.view.desired)
                    .filter(|s| i.view.seat_nodes.get(*s).is_none_or(String::is_empty))
                    .collect();
                (v, i.view.claim_window.clamp(5, 3600))
            }
        } else {
            (
                (0..i.initial_seats).collect(),
                (5u64 << self.attempt.min(7)).min(MAX_COLD_WINDOW),
            )
        };
        if candidates.is_empty() {
            self.attempt = 0;
            return Plan::NoVacancy(
                Duration::from_secs(10) + jitter(i.pod_uid, rng, 1, Duration::from_secs(10)),
            );
        }
        self.desired = if live {
            i.view.desired
        } else {
            i.initial_seats
        };
        let delay = jitter(i.pod_uid, rng, self.attempt, Duration::from_secs(window));
        let pick = jitter(i.pod_uid, rng, 3, Duration::from_millis(1 << 20)).as_millis() as usize;
        Plan::Attempt {
            delay,
            index: candidates[pick % candidates.len()],
        }
    }

    /// The attempt goes ahead: the seat Lease is about to be read.
    pub fn begin(&mut self) {
        self.attempt = self.attempt.saturating_add(1);
    }

    /// Record the Lease read for `index` and decide whether to claim it.
    pub fn observe(
        &mut self,
        index: usize,
        obs: &LeaseObservation,
        now: Instant,
        node: &str,
        identity: &str,
    ) -> bool {
        self.observer.observe(obs, now);
        self.seen.insert(index, obs.clone());
        let views = claim_views(&self.seen, &self.observer, index, now);
        election::seat_to_claim(&views, self.desired, node, identity) == Some(index)
    }

    /// The claim PUT succeeded.
    pub fn won(&mut self) {
        self.attempt = 0;
    }
}

/// Seat views for a claim of `candidate`. Only `candidate` was just read: a
/// stale cached vacancy must not shadow it, but live observations of other
/// seats still guard Node uniqueness.
pub fn claim_views(
    seen: &BTreeMap<usize, LeaseObservation>,
    observer: &Observer,
    candidate: usize,
    now: Instant,
) -> Vec<SeatView> {
    seen.iter()
        .filter_map(|(index, observation)| {
            let expired = observer.expired(observation, now);
            if *index != candidate && expired {
                return None;
            }
            Some(SeatView {
                index: *index,
                holder: lease::holder_key(observation),
                holder_node: observation
                    .annotations
                    .get(crate::annotations::HOLDER_NODE)
                    .cloned(),
                expired,
            })
        })
        .collect()
}

/// Pseudo-random delay below `max`, spread by Pod UID, `stream`, and `rng`.
///
/// `stream` separates independent delays drawn for the same Pod; it is not a
/// cryptographic input.
pub fn jitter(pod_uid: &str, rng: &mut dyn Entropy, stream: u32, max: Duration) -> Duration {
    let r = rng.u32_or_zero();
    election::claim_delay(pod_uid, stream ^ r, max)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn jitter_is_bounded_and_seeded() {
        use crate::entropy::{Failing, Seeded};
        let max = Duration::from_secs(5);
        let a = jitter("u", &mut Seeded::new(1), 0, max);
        assert!(a < max);
        assert_eq!(a, jitter("u", &mut Seeded::new(1), 0, max));
        assert_eq!(
            jitter("u", &mut Failing, 3, max),
            election::claim_delay("u", 3, max)
        );
    }
    use crate::{
        annotations,
        authority::election::LeaseSpec,
        entropy::{Failing, Seeded},
        limits,
    };

    fn view(desired: usize, nodes: &[&str]) -> PrimaryView {
        PrimaryView {
            holder: Some("p/1".into()),
            desired,
            seat_nodes: nodes.iter().map(|s| (*s).to_owned()).collect(),
            claim_window: 30,
            endpoint: None,
        }
    }

    fn inputs<'a>(v: &'a PrimaryView, at: Option<Instant>) -> Inputs<'a> {
        Inputs {
            holds_seat: false,
            cluster_known: true,
            view: v,
            primary_changed_at: at,
            node: "me",
            pod_uid: "uid",
            initial_seats: 3,
        }
    }

    fn vacant(name: &str) -> LeaseObservation {
        LeaseObservation {
            name: name.into(),
            resource_version: "1".into(),
            ..Default::default()
        }
    }

    #[test]
    fn idle_when_seated_or_no_cluster() {
        let v = view(3, &[]);
        let now = Instant::now();
        let mut l = Lottery::default();
        let mut i = inputs(&v, Some(now));
        i.holds_seat = true;
        assert_eq!(
            l.plan(&i, now, &mut Failing),
            Plan::Idle(Duration::from_secs(2))
        );
        i.holds_seat = false;
        i.cluster_known = false;
        assert!(matches!(l.plan(&i, now, &mut Failing), Plan::Idle(_)));
    }

    #[test]
    fn live_primary_vacancies_and_window() {
        let now = Instant::now();
        let v = view(3, &["a", "", "b"]);
        let mut l = Lottery::default();
        for seed in 0..32 {
            match l.plan(&inputs(&v, Some(now)), now, &mut Seeded::new(seed)) {
                Plan::Attempt { delay, index } => {
                    assert_eq!(index, 1);
                    assert!(delay < Duration::from_secs(30));
                }
                p => panic!("{p:?}"),
            }
        }
        // Seated Node: nothing to do, attempt resets.
        let seated = view(3, &["me"]);
        l.begin();
        assert!(matches!(
            l.plan(&inputs(&seated, Some(now)), now, &mut Failing),
            Plan::NoVacancy(d) if d >= Duration::from_secs(10)
        ));
        assert_eq!(l.attempt(), 0);
        // Full: no vacancy.
        let full = view(2, &["a", "b"]);
        assert!(matches!(
            l.plan(&inputs(&full, Some(now)), now, &mut Failing),
            Plan::NoVacancy(_)
        ));
    }

    #[test]
    fn cold_window_grows_and_caps() {
        let now = Instant::now();
        let v = PrimaryView::default();
        let mut l = Lottery::default();
        let mut max_delay = Duration::ZERO;
        for _ in 0..20 {
            if let Plan::Attempt { delay, index } =
                l.plan(&inputs(&v, None), now, &mut Seeded::new(7))
            {
                assert!(index < 3);
                assert!(delay < Duration::from_secs(MAX_COLD_WINDOW));
                max_delay = max_delay.max(delay);
            }
            l.begin();
        }
        assert_eq!(l.attempt(), 20);
        // Expired primary view (changed long ago) is also cold.
        let old = view(5, &[]);
        let later = now + limits::LEASE_DURATION;
        let mut l2 = Lottery::default();
        match l2.plan(&inputs(&old, Some(now)), later, &mut Failing) {
            Plan::Attempt { delay, index } => {
                assert!(delay < Duration::from_secs(5));
                assert!(index < 3);
            }
            p => panic!("{p:?}"),
        }
    }

    #[test]
    fn claim_checks_the_read_lease() {
        let now = Instant::now();
        let v = view(3, &[]);
        let mut l = Lottery::default();
        let Plan::Attempt { index, .. } = l.plan(&inputs(&v, Some(now)), now, &mut Failing) else {
            panic!()
        };
        l.begin();
        let name = annotations::seat_lease(index);
        assert!(l.observe(index, &vacant(&name), now, "me", "me/1"));
        l.won();
        assert_eq!(l.attempt(), 0);
        // A live holder blocks the claim.
        let mut held = vacant(&name);
        held.resource_version = "2".into();
        held.spec = LeaseSpec {
            holder: Some("other".into()),
            duration_secs: 30,
            ..Default::default()
        };
        let mut l = Lottery::default();
        let _ = l.plan(&inputs(&v, Some(now)), now, &mut Failing);
        assert!(!l.observe(index, &held, now, "me", "me/1"));
        // Once it expires by the local clock, the claim proceeds.
        assert!(l.observe(index, &held, now + limits::LEASE_DURATION, "me", "me/1"));
    }
}
