//! Seat rules: the lottery (`seat::lottery`), renewal and release
//! (`seat::seat_task`), and following committed state (`seat::follow_once`,
//! `seat::install_record`).
//!
//! A Pod without a seat waits a jittered delay inside the claim window, picks
//! a vacant seat, reads that one Lease, and claims it only when
//! [`election::seat_to_claim`] agrees. With a live primary, the window and
//! vacancies come from the primary Lease annotations; without one, the Pod
//! contends for the initial seats with an exponentially growing window.

use crate::{
    authority::{
        election::{self, LeaseObservation, Observer, SeatView},
        publication::Record,
        state::{self, StateError, StateRecord},
    },
    entropy::Entropy,
    lease, limits,
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
pub struct LotteryInputs<'a> {
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
    pub fn plan(&mut self, i: &LotteryInputs<'_>, now: Instant, rng: &mut dyn Entropy) -> Plan {
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

/// Retry delay after a failed renew.
pub const RETRY: Duration = Duration::from_secs(2);

/// Earliest early PUT after the last one when only readiness changed.
pub const READY_MIN_INTERVAL: Duration = Duration::from_secs(1);

/// What the renew loop sees when it wakes.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct RenewInputs {
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

impl RenewInputs {
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
    pub fn decide(&mut self, now: Instant, i: RenewInputs) -> Action {
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

/// Least time between state Secret reads.
pub const MIN_READ_INTERVAL: Duration = Duration::from_secs(1);

/// What a seat read from the state Secret.
#[derive(Clone, Copy, Debug)]
pub struct SecretRead<'a> {
    /// Cluster ID this process serves.
    pub cluster: &'a str,
    /// Secret UID.
    pub uid: &'a str,
    /// The initialized annotation is set.
    pub initialized: bool,
    /// Initialized markers and `data.cluster` equal the UID.
    pub markers_match: bool,
    /// `data.state`: absent, valid base64 decoded, or undecodable.
    pub data: Option<Result<&'a [u8], ()>>,
    /// This process already accepted a record (high water is set).
    pub accepted_before: bool,
}

/// Decision for one read.
#[derive(Debug, Eq, PartialEq)]
pub enum Read {
    /// State is missing, invalid, or wiped: stop serving.
    FailClosed,
    /// Not initialized yet: nothing to install.
    Uninitialized,
    /// Install this record, see [`admit`].
    Install(Box<StateRecord>),
}

/// Classify one state Secret read.
pub fn read(r: &SecretRead<'_>) -> Read {
    if r.uid != r.cluster {
        return Read::FailClosed;
    }
    if r.initialized && !r.markers_match {
        return Read::FailClosed;
    }
    let data = match r.data {
        Some(Ok(d)) => Some(d),
        Some(Err(())) => return Read::FailClosed,
        None => None,
    };
    match state::decode(r.uid, r.initialized, data) {
        Ok(Some(record)) => Read::Install(Box::new(record)),
        Ok(None) if r.accepted_before => Read::FailClosed,
        Ok(None) => Read::Uninitialized,
        Err(_) => Read::FailClosed,
    }
}

/// Whether a state read is older news rather than a rollback: another
/// installer (the primary's own commit, `primary.rs` `reconcile`) raised the
/// high water mark while the read was in flight, and the read is older
/// than it (the same sequence is installed as usual, so a seat still
/// learns it serves that sequence). The read may have been answered before that commit; it is
/// dropped instead of failing closed. A genuine rollback is still refused by
/// the next read, which starts after the installation (Known issue 56).
pub fn superseded(
    installed_during_read: bool,
    high_water: Option<&StateRecord>,
    record: &StateRecord,
) -> bool {
    installed_during_read && high_water.is_some_and(|h| record.sequence < h.sequence)
}

/// Whether `record` may replace the high water mark. A rollback is never
/// accepted and makes the caller fail closed.
pub fn admit(high_water: Option<&StateRecord>, record: &StateRecord) -> Result<(), StateError> {
    match high_water {
        Some(old) => state::check_monotonic(old, record).map_err(|_| StateError::Rollback),
        None => Ok(()),
    }
}

/// Whether a seat must copy the image for `record` from the primary.
pub fn need_image(record: &StateRecord, image: Option<&Record>, is_primary: bool) -> bool {
    !is_primary
        && record.sequence != 0
        && image
            .is_none_or(|i| i.sequence != record.sequence || i.content_hash != record.content_hash)
}

/// Committed state as a seat holds it, without payloads. The simulator uses
/// it directly; production keeps the same fields in `shared::Serving`.
#[derive(Clone, Debug, Default)]
pub struct Follower {
    pub high_water: Option<StateRecord>,
    pub record: Option<StateRecord>,
    pub confirmed: Option<Instant>,
    pub image: Option<Record>,
    /// Records installed so far (see [`superseded`]).
    pub installs: u64,
}

impl Follower {
    /// Install a record confirmed at `confirmed`.
    pub fn install(&mut self, record: StateRecord, confirmed: Instant) -> Result<(), StateError> {
        if let Err(e) = admit(self.high_water.as_ref(), &record) {
            self.fail_closed();
            return Err(e);
        }
        self.high_water = Some(record.clone());
        self.record = Some(record);
        self.confirmed = Some(confirmed);
        self.installs += 1;
        Ok(())
    }

    /// Stop serving. The high water mark is kept.
    pub fn fail_closed(&mut self) {
        self.record = None;
        self.confirmed = None;
        self.image = None;
    }

    /// Install a verified image for the current record. Returns false when
    /// it does not match.
    pub fn install_image(&mut self, image: Record) -> bool {
        let ok = self
            .record
            .as_ref()
            .is_some_and(|r| r.sequence == image.sequence && r.content_hash == image.content_hash);
        if ok {
            self.image = Some(image);
        }
        ok
    }

    /// Serving: fresh committed state and a matching image.
    pub fn ready(&self, now: Instant) -> bool {
        let fresh = self
            .confirmed
            .is_some_and(|c| now.duration_since(c) < crate::limits::STATE_FRESHNESS);
        fresh
            && self.record.as_ref().is_some_and(|r| {
                !need_image(r, self.image.as_ref(), false)
                    && self
                        .image
                        .as_ref()
                        .is_some_and(|i| i.sequence == r.sequence)
            })
    }
}

#[cfg(test)]
mod lottery_tests {
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

    fn inputs<'a>(v: &'a PrimaryView, at: Option<Instant>) -> LotteryInputs<'a> {
        LotteryInputs {
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

#[cfg(test)]
mod renew_tests {
    use super::*;

    #[test]
    fn release_reasons_win() {
        let t = Instant::now();
        for i in [
            RenewInputs {
                stopping: true,
                ..Default::default()
            },
            RenewInputs {
                duplicate: true,
                ..Default::default()
            },
            RenewInputs {
                rotation: true,
                ..Default::default()
            },
            RenewInputs {
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
        assert_eq!(r.decide(t, RenewInputs::default()), Action::Wait);
        let ready = RenewInputs {
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
        assert_eq!(r.decide(r.next, RenewInputs::default()), Action::Renew);
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
        let i = RenewInputs::from_view(&v, 2, "n1", false, false, false, false);
        assert!(i.duplicate && i.shrink);
        let i = RenewInputs::from_view(&v, 2, "n1", true, false, false, false);
        assert!(!i.shrink, "the primary keeps its seat");
        let i = RenewInputs::from_view(&v, 1, "n2", false, false, false, false);
        assert!(!i.duplicate && !i.shrink);
        let none = PrimaryView {
            holder: None,
            ..v.clone()
        };
        assert!(!RenewInputs::from_view(&none, 5, "x", false, false, false, false).shrink);
    }
}

#[cfg(test)]
mod follower_tests {
    use super::*;

    const C: &str = "c1c1c1c1-0000-4000-8000-000000000001";

    fn record(sequence: u64) -> StateRecord {
        let hash = |c: char| {
            if sequence == 0 {
                String::new()
            } else {
                c.to_string().repeat(64)
            }
        };
        StateRecord {
            version: state::VERSION,
            cluster: C.into(),
            term: state::Term {
                lease_uid: "l".into(),
                transitions: 1,
                holder: "p/00".into(),
            },
            writer_pod_uid: "u".into(),
            sequence,
            membership_version: sequence,
            content_hash: hash('a'),
            membership_hash: hash('b'),
            ..Default::default()
        }
    }

    fn secret<'a>(data: Option<Result<&'a [u8], ()>>, initialized: bool) -> SecretRead<'a> {
        SecretRead {
            cluster: C,
            uid: C,
            initialized,
            markers_match: true,
            data,
            accepted_before: false,
        }
    }

    #[test]
    fn superseded_reads_are_dropped_only_after_an_install_during_the_read() {
        let high = record(5);
        // Older news while another installer raised the mark.
        assert!(superseded(true, Some(&high), &record(4)));
        // The same record: installed as usual.
        assert!(!superseded(true, Some(&high), &record(5)));
        // Newer: installed as usual.
        assert!(!superseded(true, Some(&high), &record(6)));
        // Nothing installed during the read: a rollback is a rollback.
        assert!(!superseded(false, Some(&high), &record(4)));
        assert!(!superseded(true, None, &record(4)));
        let mut f = Follower::default();
        let now = Instant::now();
        f.install(record(4), now).unwrap();
        f.install(record(5), now).unwrap();
        assert_eq!(f.installs, 2);
        assert!(f.install(record(4), now).is_err());
        assert_eq!(f.installs, 2, "a refused record is not an install");
    }

    #[test]
    fn read_classification() {
        let bytes = state::encode(&record(3)).unwrap();
        assert_eq!(
            read(&secret(Some(Ok(&bytes)), true)),
            Read::Install(Box::new(record(3)))
        );
        assert_eq!(read(&secret(None, false)), Read::Uninitialized);
        let mut wiped = secret(None, false);
        wiped.accepted_before = true;
        assert_eq!(read(&wiped), Read::FailClosed);
        assert_eq!(read(&secret(Some(Err(())), true)), Read::FailClosed);
        let mut other = secret(Some(Ok(&bytes)), true);
        other.uid = "x";
        assert_eq!(read(&other), Read::FailClosed);
        let mut markers = secret(Some(Ok(&bytes)), true);
        markers.markers_match = false;
        assert_eq!(read(&markers), Read::FailClosed);
        assert_eq!(read(&secret(Some(Ok(b"{")), true)), Read::FailClosed);
    }

    #[test]
    fn install_refuses_rollback_and_keeps_high_water() {
        let t = Instant::now();
        let mut f = Follower::default();
        f.install(record(5), t).unwrap();
        assert!(f.install(record(4), t).is_err());
        assert!(f.record.is_none());
        assert_eq!(f.high_water.as_ref().unwrap().sequence, 5);
        f.install(record(6), t).unwrap();
    }

    #[test]
    fn image_rules() {
        let t = Instant::now();
        let r = record(2);
        let img = Record {
            sequence: 2,
            membership_version: 2,
            content_hash: r.content_hash.clone(),
            membership_hash: r.membership_hash.clone(),
        };
        assert!(need_image(&r, None, false));
        assert!(!need_image(&r, None, true));
        assert!(!need_image(&record(0), None, false));
        assert!(!need_image(&r, Some(&img), false));
        let mut f = Follower::default();
        assert!(!f.install_image(img.clone()));
        f.install(r, t).unwrap();
        assert!(!f.ready(t));
        assert!(f.install_image(img));
        assert!(f.ready(t));
        assert!(!f.ready(t + crate::limits::STATE_FRESHNESS));
    }
}
